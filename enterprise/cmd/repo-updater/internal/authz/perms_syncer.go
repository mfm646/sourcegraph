package authz

import (
	"container/heap"
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/RoaringBitmap/roaring"
	"github.com/cockroachdb/errors"
	"github.com/inconshreveable/log15"
	otlog "github.com/opentracing/opentracing-go/log"

	"github.com/sourcegraph/sourcegraph/cmd/frontend/globals"
	edb "github.com/sourcegraph/sourcegraph/enterprise/internal/database"
	"github.com/sourcegraph/sourcegraph/enterprise/internal/licensing"
	"github.com/sourcegraph/sourcegraph/internal/api"
	"github.com/sourcegraph/sourcegraph/internal/authz"
	"github.com/sourcegraph/sourcegraph/internal/conf"
	"github.com/sourcegraph/sourcegraph/internal/database"
	"github.com/sourcegraph/sourcegraph/internal/errcode"
	"github.com/sourcegraph/sourcegraph/internal/extsvc"
	"github.com/sourcegraph/sourcegraph/internal/extsvc/github"
	"github.com/sourcegraph/sourcegraph/internal/ratelimit"
	"github.com/sourcegraph/sourcegraph/internal/repos"
	"github.com/sourcegraph/sourcegraph/internal/trace"
	"github.com/sourcegraph/sourcegraph/internal/types"
)

// PermsSyncer is a permissions syncing manager that is in charge of keeping
// permissions up-to-date for users and repositories.
//
// It is meant to be running in the background.
type PermsSyncer struct {
	// The priority queue to maintain the permissions syncing requests.
	queue *requestQueue
	// The database interface for any repos and external services operations.
	reposStore *repos.Store
	// The database interface for any permissions operations.
	permsStore *edb.PermsStore
	// The mockable function to return the current time.
	clock func() time.Time
	// The rate limit registry for code hosts.
	rateLimiterRegistry *ratelimit.Registry
	// The time duration of how often to re-compute schedule for users and repositories.
	scheduleInterval time.Duration
}

// NewPermsSyncer returns a new permissions syncing manager.
func NewPermsSyncer(
	reposStore *repos.Store,
	permsStore *edb.PermsStore,
	clock func() time.Time,
	rateLimiterRegistry *ratelimit.Registry,
) *PermsSyncer {
	return &PermsSyncer{
		queue:               newRequestQueue(),
		reposStore:          reposStore,
		permsStore:          permsStore,
		clock:               clock,
		rateLimiterRegistry: rateLimiterRegistry,
		scheduleInterval:    time.Minute,
	}
}

// ScheduleUsers schedules new permissions syncing requests for given users.
// By design, all schedules triggered by user actions are in high priority.
//
// This method implements the repoupdater.Server.PermsSyncer in the OSS namespace.
func (s *PermsSyncer) ScheduleUsers(ctx context.Context, userIDs ...int32) {
	if len(userIDs) == 0 {
		return
	} else if s.isDisabled() {
		log15.Warn("PermsSyncer.ScheduleUsers.disabled", "userIDs", userIDs)
		return
	}

	users := make([]scheduledUser, len(userIDs))
	for i := range userIDs {
		users[i] = scheduledUser{
			priority: priorityHigh,
			userID:   userIDs[i],
			// NOTE: Have nextSyncAt with zero value (i.e. not set) gives it higher priority,
			// as the request is most likely triggered by a user action from OSS namespace.
		}
	}

	s.scheduleUsers(ctx, users...)
}

func (s *PermsSyncer) scheduleUsers(ctx context.Context, users ...scheduledUser) {
	for _, u := range users {
		select {
		case <-ctx.Done():
			log15.Debug("PermsSyncer.scheduleUsers.canceled")
			return
		default:
		}

		updated := s.queue.enqueue(&requestMeta{
			Priority:   u.priority,
			Type:       requestTypeUser,
			ID:         u.userID,
			NextSyncAt: u.nextSyncAt,
			NoPerms:    u.noPerms,
		})
		log15.Debug("PermsSyncer.queue.enqueued", "userID", u.userID, "updated", updated)
	}
}

// ScheduleRepos schedules new permissions syncing requests for given repositories.
// By design, all schedules triggered by user actions are in high priority.
//
// This method implements the repoupdater.Server.PermsSyncer in the OSS namespace.
func (s *PermsSyncer) ScheduleRepos(ctx context.Context, repoIDs ...api.RepoID) {
	if len(repoIDs) == 0 {
		return
	} else if s.isDisabled() {
		log15.Warn("PermsSyncer.ScheduleRepos.disabled", "repoIDs", repoIDs)
		return
	}

	repos := make([]scheduledRepo, len(repoIDs))
	for i := range repoIDs {
		repos[i] = scheduledRepo{
			priority: priorityHigh,
			repoID:   repoIDs[i],
			// NOTE: Have nextSyncAt with zero value (i.e. not set) gives it higher priority,
			// as the request is most likely triggered by a user action from OSS namespace.
		}
	}

	s.scheduleRepos(ctx, repos...)
}

func (s *PermsSyncer) scheduleRepos(ctx context.Context, repos ...scheduledRepo) {
	for _, r := range repos {
		select {
		case <-ctx.Done():
			log15.Debug("PermsSyncer.scheduleRepos.canceled")
			return
		default:
		}

		updated := s.queue.enqueue(&requestMeta{
			Priority:   r.priority,
			Type:       requestTypeRepo,
			ID:         int32(r.repoID),
			NextSyncAt: r.nextSyncAt,
			NoPerms:    r.noPerms,
		})
		log15.Debug("PermsSyncer.queue.enqueued", "repoID", r.repoID, "updated", updated)
	}
}

// providersByServiceID returns a list of authz.Provider configured in the external services.
// Keys are ServiceID, e.g. "https://github.com/".
func (s *PermsSyncer) providersByServiceID() map[string]authz.Provider {
	_, ps := authz.GetProviders()
	providers := make(map[string]authz.Provider, len(ps))
	for _, p := range ps {
		providers[p.ServiceID()] = p
	}
	return providers
}

// providersByURNs returns a list of authz.Provider configured in the external services.
// Keys are URN, e.g. "extsvc:github:1".
func (s *PermsSyncer) providersByURNs() map[string]authz.Provider {
	_, ps := authz.GetProviders()
	providers := make(map[string]authz.Provider, len(ps))
	for _, p := range ps {
		providers[p.URN()] = p
	}
	return providers
}

// listPrivateRepoNamesByExact slices over the `repoSpecs` at pace of 10000
// elements at a time to workaround Postgres' limit of 65535 bind parameters
// using exact name matching. This method only includes private repository names
// and does not do deduplication on the returned list.
func (s *PermsSyncer) listPrivateRepoNamesByExact(ctx context.Context, repoSpecs []api.ExternalRepoSpec) ([]types.RepoName, error) {
	if len(repoSpecs) == 0 {
		return []types.RepoName{}, nil
	}

	remaining := repoSpecs
	nextCut := 10000
	if len(remaining) < nextCut {
		nextCut = len(remaining)
	}

	repoNames := make([]types.RepoName, 0, len(repoSpecs))
	for nextCut > 0 {
		rs, err := s.reposStore.RepoStore.ListRepoNames(ctx,
			database.ReposListOptions{
				ExternalRepos: remaining[:nextCut],
				OnlyPrivate:   true,
			},
		)
		if err != nil {
			return nil, err
		}

		repoNames = append(repoNames, rs...)

		remaining = remaining[nextCut:]
		if len(remaining) < nextCut {
			nextCut = len(remaining)
		}
	}
	return repoNames, nil
}

// syncUserPerms processes permissions syncing request in user-centric way. When `noPerms` is true,
// the method will use partial results to update permissions tables even when error occurs.
func (s *PermsSyncer) syncUserPerms(ctx context.Context, userID int32, noPerms bool) (err error) {
	ctx, save := s.observe(ctx, "PermsSyncer.syncUserPerms", "")
	defer save(requestTypeUser, userID, &err)

	// NOTE: If a <repo_id, user_id> pair is present in the external_service_repos
	//  table, the user has proven that they have read access to the repository.
	repoIDs, err := s.reposStore.ListExternalServicePrivateRepoIDsByUserID(ctx, userID)
	if err != nil {
		return errors.Wrap(err, "list external service repo IDs by user ID")
	}

	user, err := database.UsersWith(s.reposStore).GetByID(ctx, userID)
	if err != nil {
		return errors.Wrap(err, "get user")
	}

	accts, err := s.permsStore.ListExternalAccounts(ctx, user.ID)
	if err != nil {
		return errors.Wrap(err, "list external accounts")
	}

	serviceToAccounts := make(map[string]*extsvc.Account)
	for _, acct := range accts {
		serviceToAccounts[acct.ServiceType+":"+acct.ServiceID] = acct
	}

	userEmails, err := database.UserEmailsWith(s.reposStore).ListByUser(ctx,
		database.UserEmailsListOptions{
			UserID:       user.ID,
			OnlyVerified: true,
		},
	)
	if err != nil {
		return errors.Wrap(err, "list user verified emails")
	}

	emails := make([]string, len(userEmails))
	for i := range userEmails {
		emails[i] = userEmails[i].Email
	}

	byServiceID := s.providersByServiceID()
	accounts := database.ExternalAccountsWith(s.reposStore)

	// Check if the user has an external account for every authz provider respectively,
	// and try to fetch the account when not.
	for _, provider := range byServiceID {
		_, ok := serviceToAccounts[provider.ServiceType()+":"+provider.ServiceID()]
		if ok {
			continue
		}

		acct, err := provider.FetchAccount(ctx, user, accts, emails)
		if err != nil {
			log15.Error("Could not fetch account from authz provider",
				"userID", user.ID,
				"authzProvider", provider.ServiceID(),
				"error", err)
			continue
		}

		// Not an operation failure but the authz provider is unable to determine
		// the external account for the current user.
		if acct == nil {
			continue
		}

		err = accounts.AssociateUserAndSave(ctx, user.ID, acct.AccountSpec, acct.AccountData)
		if err != nil {
			log15.Error("Could not associate external account to user",
				"userID", user.ID,
				"authzProvider", provider.ServiceID(),
				"error", err)
			continue
		}

		accts = append(accts, acct)
	}

	// Fetch all the users external services
	externalServices := database.ExternalServicesWith(s.reposStore)
	svcs, err := externalServices.List(ctx, database.ExternalServicesListOptions{
		NamespaceUserID: userID,
		Kinds:           []string{extsvc.KindGitHub, extsvc.KindGitLab},
	})
	if err != nil {
		return errors.Wrap(err, "fetching external services")
	}

	byURN := s.providersByURNs()

	var accountsOrServices []interface{}
	for i := range accts {
		accountsOrServices = append(accountsOrServices, accts[i])
	}
	for i := range svcs {
		accountsOrServices = append(accountsOrServices, svcs[i])
	}

	var repoSpecs, includeContainsSpecs, excludeContainsSpecs []api.ExternalRepoSpec

	for _, accountOrService := range accountsOrServices {
		var extIDs *authz.ExternalUserPermissions
		var provider authz.Provider

		switch v := accountOrService.(type) {
		case *extsvc.Account:
			provider = byServiceID[v.ServiceID]
			if provider == nil {
				// We have no authz provider configured for this external account or service
				continue
			}

			if err := s.waitForRateLimit(ctx, provider.ServiceID(), 1); err != nil {
				return errors.Wrap(err, "wait for rate limiter")
			}
			extIDs, err = provider.FetchUserPerms(ctx, v)

			if err != nil {
				// The "401 Unauthorized" is returned by code hosts when the token is no longer valid
				unauthorized := errcode.IsUnauthorized(err)

				forbidden := errcode.IsForbidden(err)

				// Detect GitHub account suspension error
				accountSuspended := errcode.IsAccountSuspended(err)

				if unauthorized || accountSuspended || forbidden {
					err = accounts.TouchExpired(ctx, v.ID)
					if err != nil {
						return errors.Wrapf(err, "set expired for external account %d", v.ID)
					}
					log15.Debug("PermsSyncer.syncUserPerms.setExternalAccountExpired",
						"userID", user.ID, "id", v.ID,
						"unauthorized", unauthorized, "accountSuspended", accountSuspended, "forbidden", forbidden)

					// We still want to continue processing other external accounts
					continue
				}

				// Process partial results if this is an initial fetch.
				if !noPerms {
					return errors.Wrap(err, "fetch user permissions")
				}
				log15.Warn("PermsSyncer.syncUserPerms.proceedWithPartialResults", "userID", user.ID, "error", err)
			} else {
				err = accounts.TouchLastValid(ctx, v.ID)
				if err != nil {
					return errors.Wrapf(err, "set last valid for external account %d", v.ID)
				}
			}

		case *types.ExternalService:
			provider = byURN[v.URN()]
			if provider == nil {
				// We have no authz provider configured for this external service or service
				continue
			}
			token, err := extsvc.ExtractToken(v.Config, v.Kind)
			if err != nil {
				log15.Warn("Extracting token from external service config", "error", err, "id", v.ID)
				continue
			}
			if token == "" {
				log15.Warn("Empty token for external service", "id", v.ID)
				continue
			}

			if err := s.waitForRateLimit(ctx, provider.ServiceID(), 1); err != nil {
				return errors.Wrap(err, "wait for rate limiter")
			}

			extIDs, err = provider.FetchUserPermsByToken(ctx, token)
			if err != nil {
				log15.Warn("Fetching user permissions by token", "error", err)
				continue
			}

		default:
			log15.Error("Expected account or external service", "got", fmt.Sprintf("%T", accountOrService))
			continue
		}

		if extIDs == nil {
			continue
		}

		if len(extIDs.Exacts) > 0 {
			for _, exact := range extIDs.Exacts {
				repoSpecs = append(repoSpecs,
					api.ExternalRepoSpec{
						ID:          string(exact),
						ServiceType: provider.ServiceType(),
						ServiceID:   provider.ServiceID(),
					},
				)
			}
		}
		if len(extIDs.IncludeContains) > 0 {
			for _, includePrefix := range extIDs.IncludeContains {
				includeContainsSpecs = append(includeContainsSpecs,
					api.ExternalRepoSpec{
						ID:          string(includePrefix),
						ServiceType: provider.ServiceType(),
						ServiceID:   provider.ServiceID(),
					},
				)
			}
		}
		if len(extIDs.ExcludeContains) > 0 {
			for _, excludePrefix := range extIDs.ExcludeContains {
				excludeContainsSpecs = append(excludeContainsSpecs,
					api.ExternalRepoSpec{
						ID:          string(excludePrefix),
						ServiceType: provider.ServiceType(),
						ServiceID:   provider.ServiceID(),
					},
				)
			}
		}
	}

	// Get corresponding internal database IDs
	repoNames, err := s.listPrivateRepoNamesByExact(ctx, repoSpecs)
	if err != nil {
		return errors.Wrap(err, "list external repositories by exact matching")
	}

	// Exclusions are relative to inclusions, so if there is no inclusion, exclusion
	// are meaningless and no need to trigger a DB query.
	if len(includeContainsSpecs) > 0 {
		rs, err := s.reposStore.RepoStore.ListRepoNames(ctx,
			database.ReposListOptions{
				ExternalRepoIncludeContains: includeContainsSpecs,
				ExternalRepoExcludeContains: excludeContainsSpecs,
				OnlyPrivate:                 true,
			},
		)
		if err != nil {
			return errors.Wrap(err, "list external repositories by contains matching")
		}
		repoNames = append(repoNames, rs...)
	}

	// Save permissions to database
	p := &authz.UserPermissions{
		UserID: user.ID,
		Perm:   authz.Read, // Note: We currently only support read for repository permissions.
		Type:   authz.PermRepos,
		IDs:    roaring.NewBitmap(),
	}
	for i := range repoNames {
		p.IDs.Add(uint32(repoNames[i].ID))
	}
	for i := range repoIDs {
		p.IDs.Add(uint32(repoIDs[i]))
	}

	err = s.permsStore.SetUserPermissions(ctx, p)
	if err != nil {
		return errors.Wrap(err, "set user permissions")
	}

	log15.Debug("PermsSyncer.syncUserPerms.synced", "userID", user.ID)
	return nil
}

// syncRepoPerms processes permissions syncing request in repository-centric way.
// When `noPerms` is true, the method will use partial results to update permissions
// tables even when error occurs.
func (s *PermsSyncer) syncRepoPerms(ctx context.Context, repoID api.RepoID, noPerms bool) (err error) {
	ctx, save := s.observe(ctx, "PermsSyncer.syncRepoPerms", "")
	defer save(requestTypeRepo, int32(repoID), &err)

	rs, err := s.reposStore.RepoStore.List(ctx, database.ReposListOptions{
		IDs: []api.RepoID{repoID},
	})
	if err != nil {
		return errors.Wrap(err, "list repositories")
	} else if len(rs) == 0 {
		return nil
	}
	repo := rs[0]

	var userIDs []int32
	var provider authz.Provider

	// Only check authz provider for private repositories because we only need to
	// fetch permissions for private repositories.
	if repo.Private {
		// NOTE: If a <repo_id, user_id> pair is present in the external_service_repos
		//  table, the user has proven that they have read access to the repository.
		userIDs, err = s.reposStore.ListExternalServiceUserIDsByRepoID(ctx, repoID)
		if err != nil {
			return errors.Wrap(err, "list external service user IDs by repo ID")
		}

		// Loop over repository's sources and see if matching any authz provider's URN.
		providers := s.providersByURNs()
		for urn := range repo.Sources {
			p, ok := providers[urn]
			if ok {
				provider = p
				break
			}
		}
	}

	// For non-private repositories, we rely on the fact that the `provider` is
	// always nil here because we don't restrict access to non-private repositories.
	if provider == nil {
		log15.Debug("PermsSyncer.syncRepoPerms.noProvider",
			"repoID", repo.ID,
			"private", repo.Private,
		)

		// We have no authz provider configured for the repository.
		// However, we need to upsert the dummy record in order to
		// prevent scheduler keep scheduling this repository.
		return errors.Wrap(s.permsStore.TouchRepoPermissions(ctx, int32(repoID)), "touch repository permissions")
	}

	if err := s.waitForRateLimit(ctx, provider.ServiceID(), 1); err != nil {
		return errors.Wrap(err, "wait for rate limiter")
	}

	extAccountIDs, err := provider.FetchRepoPerms(ctx, &extsvc.Repository{
		URI:              repo.URI,
		ExternalRepoSpec: repo.ExternalRepo,
	})

	// Detect 404 error (i.e. not authorized to call given APIs) that often happens with GitHub.com
	// when the owner of the token only has READ access. However, we don't want to fail
	// so the scheduler won't keep trying to fetch permissions of this same repository, so we
	// return a nil error and log a warning message.
	var e *github.APIError
	if errors.As(err, &e) && e.Code == http.StatusNotFound {
		log15.Warn("PermsSyncer.syncRepoPerms.ignoreUnauthorizedAPIError", "repoID", repo.ID, "err", err, "suggestion", "GitHub access token user may only have read access to the repository, but needs write for permissions")
		return errors.Wrap(s.permsStore.TouchRepoPermissions(ctx, int32(repoID)), "touch repository permissions")
	}

	if err != nil {
		// Process partial results if this is an initial fetch.
		if !noPerms {
			return errors.Wrap(err, "fetch repository permissions")
		}
		log15.Warn("PermsSyncer.syncRepoPerms.proceedWithPartialResults", "repoID", repo.ID, "err", err)
	}

	pendingAccountIDsSet := make(map[string]struct{})
	var accountIDToUserID map[string]int32 // Account ID -> User ID
	if len(extAccountIDs) > 0 {
		accountIDs := make([]string, len(extAccountIDs))
		for i := range extAccountIDs {
			accountIDs[i] = string(extAccountIDs[i])
		}

		// Get corresponding internal database IDs
		accountIDToUserID, err = s.permsStore.GetUserIDsByExternalAccounts(ctx, &extsvc.Accounts{
			ServiceType: provider.ServiceType(),
			ServiceID:   provider.ServiceID(),
			AccountIDs:  accountIDs,
		})
		if err != nil {
			return errors.Wrap(err, "get user IDs by external accounts")
		}

		// Set up the set of all account IDs that need to be bound to permissions
		pendingAccountIDsSet = make(map[string]struct{}, len(accountIDs))
		for i := range accountIDs {
			pendingAccountIDsSet[accountIDs[i]] = struct{}{}
		}
	}

	// Save permissions to database
	p := &authz.RepoPermissions{
		RepoID:  int32(repoID),
		Perm:    authz.Read, // Note: We currently only support read for repository permissions.
		UserIDs: roaring.NewBitmap(),
	}

	for aid, uid := range accountIDToUserID {
		// Add existing user to permissions
		p.UserIDs.Add(uint32(uid))

		// Remove existing user from the set of pending users
		delete(pendingAccountIDsSet, aid)
	}
	for i := range userIDs {
		p.UserIDs.Add(uint32(userIDs[i]))
	}

	pendingAccountIDs := make([]string, 0, len(pendingAccountIDsSet))
	for aid := range pendingAccountIDsSet {
		pendingAccountIDs = append(pendingAccountIDs, aid)
	}

	txs, err := s.permsStore.Transact(ctx)
	if err != nil {
		return errors.Wrap(err, "start transaction")
	}
	defer func() { err = txs.Done(err) }()

	accounts := &extsvc.Accounts{
		ServiceType: provider.ServiceType(),
		ServiceID:   provider.ServiceID(),
		AccountIDs:  pendingAccountIDs,
	}

	if err = txs.SetRepoPermissions(ctx, p); err != nil {
		return errors.Wrap(err, "set repository permissions")
	} else if err = txs.SetRepoPendingPermissions(ctx, accounts, p); err != nil {
		return errors.Wrap(err, "set repository pending permissions")
	}

	log15.Debug("PermsSyncer.syncRepoPerms.synced", "repoID", repo.ID, "name", repo.Name, "count", len(extAccountIDs))
	return nil
}

// waitForRateLimit blocks until rate limit permits n events to happen. It returns
// an error if n exceeds the limiter's burst size, the context is canceled, or the
// expected wait time exceeds the context's deadline. The burst limit is ignored if
// the rate limit is Inf.
func (s *PermsSyncer) waitForRateLimit(ctx context.Context, serviceID string, n int) error {
	if s.rateLimiterRegistry == nil {
		return nil
	}

	rl := s.rateLimiterRegistry.Get(serviceID)
	if err := rl.WaitN(ctx, n); err != nil {
		return err
	}
	return nil
}

// syncPerms processes the permissions syncing request and remove the request from
// the queue once it is done (independent of success or failure).
func (s *PermsSyncer) syncPerms(ctx context.Context, request *syncRequest) error {
	defer s.queue.remove(request.Type, request.ID, true)

	var err error
	switch request.Type {
	case requestTypeUser:
		err = s.syncUserPerms(ctx, request.ID, request.NoPerms)
	case requestTypeRepo:
		err = s.syncRepoPerms(ctx, api.RepoID(request.ID), request.NoPerms)
	default:
		err = errors.Errorf("unexpected request type: %v", request.Type)
	}

	return err
}

func (s *PermsSyncer) runSync(ctx context.Context) {
	log15.Debug("PermsSyncer.runSync.started")
	defer log15.Info("PermsSyncer.runSync.stopped")

	// To unblock the "select" on the next loop iteration if no enqueue happened in between.
	notifyDequeued := make(chan struct{}, 1)
	for {
		select {
		case <-notifyDequeued:
		case <-s.queue.notifyEnqueue:
		case <-ctx.Done():
			return
		}

		request := s.queue.acquireNext()
		if request == nil {
			// No waiting request is in the queue
			continue
		}

		// Check if it's the time to sync the request
		if wait := request.NextSyncAt.Sub(s.clock()); wait > 0 {
			s.queue.release(request.Type, request.ID)
			time.AfterFunc(wait, func() {
				notify(s.queue.notifyEnqueue)
			})

			log15.Debug("PermsSyncer.Run.waitForNextSync", "duration", wait)
			continue
		}

		notify(notifyDequeued)

		err := s.syncPerms(ctx, request)
		if err != nil {
			log15.Error("Failed to sync permissions", "type", request.Type, "id", request.ID, "err", err)
			continue
		}
	}
}

// scheduleUsersWithNoPerms returns computed schedules for users who have no permissions
// found in database.
func (s *PermsSyncer) scheduleUsersWithNoPerms(ctx context.Context) ([]scheduledUser, error) {
	ids, err := s.permsStore.UserIDsWithNoPerms(ctx)
	if err != nil {
		return nil, err
	}
	metricsNoPerms.WithLabelValues("user").Set(float64(len(ids)))

	users := make([]scheduledUser, len(ids))
	for i, id := range ids {
		users[i] = scheduledUser{
			priority: priorityLow,
			userID:   id,
			// NOTE: Have nextSyncAt with zero value (i.e. not set) gives it higher priority.
			noPerms: true,
		}
	}
	return users, nil
}

// scheduleReposWithNoPerms returns computed schedules for private repositories that
// have no permissions found in database.
func (s *PermsSyncer) scheduleReposWithNoPerms(ctx context.Context) ([]scheduledRepo, error) {
	ids, err := s.permsStore.RepoIDsWithNoPerms(ctx)
	if err != nil {
		return nil, err
	}
	metricsNoPerms.WithLabelValues("repo").Set(float64(len(ids)))

	repos := make([]scheduledRepo, len(ids))
	for i, id := range ids {
		repos[i] = scheduledRepo{
			priority: priorityLow,
			repoID:   id,
			// NOTE: Have nextSyncAt with zero value (i.e. not set) gives it higher priority.
			noPerms: true,
		}
	}
	return repos, nil
}

// scheduleUsersWithOldestPerms returns computed schedules for users who have oldest
// permissions in database and capped results by the limit.
func (s *PermsSyncer) scheduleUsersWithOldestPerms(ctx context.Context, limit int) ([]scheduledUser, error) {
	results, err := s.permsStore.UserIDsWithOldestPerms(ctx, limit)
	if err != nil {
		return nil, err
	}

	users := make([]scheduledUser, 0, len(results))
	for id, t := range results {
		users = append(users, scheduledUser{
			priority:   priorityLow,
			userID:     id,
			nextSyncAt: t,
		})
	}
	return users, nil
}

// scheduleReposWithOldestPerms returns computed schedules for private repositories that
// have oldest permissions in database.
func (s *PermsSyncer) scheduleReposWithOldestPerms(ctx context.Context, limit int) ([]scheduledRepo, error) {
	results, err := s.permsStore.ReposIDsWithOldestPerms(ctx, limit)
	if err != nil {
		return nil, err
	}

	repos := make([]scheduledRepo, 0, len(results))
	for id, t := range results {
		repos = append(repos, scheduledRepo{
			priority:   priorityLow,
			repoID:     id,
			nextSyncAt: t,
		})
	}
	return repos, nil
}

// schedule contains information for scheduling users and repositories.
type schedule struct {
	Users []scheduledUser
	Repos []scheduledRepo
}

// scheduledUser contains information for scheduling a user.
type scheduledUser struct {
	priority   priority
	userID     int32
	nextSyncAt time.Time

	// Whether the user has no permissions when scheduled. Currently used to
	// accept partial results from authz provider in case of error.
	noPerms bool
}

// scheduledRepo contains for scheduling a repository.
type scheduledRepo struct {
	priority   priority
	repoID     api.RepoID
	nextSyncAt time.Time

	// Whether the repository has no permissions when scheduled. Currently used
	// to accept partial results from authz provider in case of error.
	noPerms bool
}

// schedule computes schedule four lists in the following order:
//   1. Users with no permissions, because they can't do anything meaningful (e.g. not able to search).
//   2. Private repositories with no permissions, because those can't be viewed by anyone except site admins.
//   3. Rolling updating user permissions over time from oldest ones.
//   4. Rolling updating repository permissions over time from oldest ones.
func (s *PermsSyncer) schedule(ctx context.Context) (*schedule, error) {
	schedule := new(schedule)

	users, err := s.scheduleUsersWithNoPerms(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "schedule users with no permissions")
	}
	schedule.Users = append(schedule.Users, users...)

	repos, err := s.scheduleReposWithNoPerms(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "schedule repositories with no permissions")
	}
	schedule.Repos = append(schedule.Repos, repos...)

	// TODO(jchen): Predict a limit taking account into:
	//   1. Based on total repos and users that make sense to finish syncing before
	//      next schedule call, so we don't waste database bandwidth.
	//   2. How we're doing in terms of rate limiting.
	// Formula (in worse case scenario, at the pace of 1 req/s):
	//   initial limit  = <predicted from the previous step>
	//	 consumed by users = <initial limit> / (<total repos> / <page size>)
	//   consumed by repos = (<initial limit> - <consumed by users>) / (<total users> / <page size>)
	// Hard coded both to 10 for now.
	const limit = 10

	// TODO(jchen): Use better heuristics for setting NextSyncAt, the initial version
	// just uses the value of LastUpdatedAt get from the perms tables.

	users, err = s.scheduleUsersWithOldestPerms(ctx, limit)
	if err != nil {
		return nil, errors.Wrap(err, "load users with oldest permissions")
	}
	schedule.Users = append(schedule.Users, users...)

	repos, err = s.scheduleReposWithOldestPerms(ctx, limit)
	if err != nil {
		return nil, errors.Wrap(err, "scan repositories with oldest permissions")
	}
	schedule.Repos = append(schedule.Repos, repos...)

	return schedule, nil
}

// isDisabled returns true if the background permissions syncing is not enabled.
// It is not enabled if:
//   - Permissions user mapping is enabled
//   - No authz provider is configured
//   - Not purchased with the current license
//   - `disableAutoCodeHostSyncs` site setting is set to true
func (s *PermsSyncer) isDisabled() bool {
	return globals.PermissionsUserMapping().Enabled ||
		len(s.providersByServiceID()) == 0 ||
		(licensing.EnforceTiers && licensing.Check(licensing.FeatureACLs) != nil) ||
		conf.Get().DisableAutoCodeHostSyncs
}

func (s *PermsSyncer) runSchedule(ctx context.Context) {
	log15.Debug("PermsSyncer.runSchedule.started")
	defer log15.Info("PermsSyncer.runSchedule.stopped")

	ticker := time.NewTicker(s.scheduleInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return
		}

		if s.isDisabled() {
			continue
		}

		schedule, err := s.schedule(ctx)
		if err != nil {
			log15.Error("Failed to compute schedule", "err", err)
			continue
		}

		s.scheduleUsers(ctx, schedule.Users...)
		s.scheduleRepos(ctx, schedule.Repos...)
	}
}

// DebugDump returns the state of the permissions syncer for debugging.
func (s *PermsSyncer) DebugDump() interface{} {
	type requestInfo struct {
		Meta     *requestMeta
		Acquired bool
	}
	data := struct {
		Name  string
		Size  int
		Queue []*requestInfo
	}{
		Name: "permissions",
	}

	queue := requestQueue{
		heap: make([]*syncRequest, len(s.queue.heap)),
	}

	s.queue.mu.RLock()
	defer s.queue.mu.RUnlock()

	for i, request := range s.queue.heap {
		// Copy the syncRequest as a value so that poping off the heap here won't
		// update the index value of the real heap, and we don't do a racy read on
		// the repo pointer which may change concurrently in the real heap.
		requestCopy := *request
		queue.heap[i] = &requestCopy
	}

	for len(queue.heap) > 0 {
		// Copy values of the syncRequest so that the requestMeta pointer
		// won't change concurrently after we release the lock.
		request := heap.Pop(&queue).(*syncRequest)
		data.Queue = append(data.Queue, &requestInfo{
			Meta: &requestMeta{
				Priority:   request.Priority,
				Type:       request.Type,
				ID:         request.ID,
				NextSyncAt: request.NextSyncAt,
			},
			Acquired: request.acquired,
		})
	}
	data.Size = len(data.Queue)

	return &data
}

func (s *PermsSyncer) observe(ctx context.Context, family, title string) (context.Context, func(requestType, int32, *error)) {
	began := s.clock()
	tr, ctx := trace.New(ctx, family, title)

	return ctx, func(typ requestType, id int32, err *error) {
		defer tr.Finish()
		tr.LogFields(otlog.Int32("id", id))

		var typLabel string
		switch typ {
		case requestTypeRepo:
			typLabel = "repo"
		case requestTypeUser:
			typLabel = "user"
		default:
			tr.SetError(errors.Errorf("unexpected request type: %v", typ))
			return
		}

		success := err == nil || *err == nil
		metricsSyncDuration.WithLabelValues(typLabel, strconv.FormatBool(success)).Observe(time.Since(began).Seconds())

		if !success {
			tr.SetError(*err)
			metricsSyncErrors.WithLabelValues(typLabel).Add(1)
		}
	}
}

// collectMetrics periodically collecting metrics values from both database and memory objects.
func (s *PermsSyncer) collectMetrics(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return
		}

		m, err := s.permsStore.Metrics(ctx, 3*24*time.Hour)
		if err != nil {
			log15.Error("Failed to get metrics from database", "err", err)
			continue
		}

		metricsStalePerms.WithLabelValues("user").Set(float64(m.UsersWithStalePerms))
		metricsPermsGap.WithLabelValues("user").Set(m.UsersPermsGapSeconds)
		metricsStalePerms.WithLabelValues("repo").Set(float64(m.ReposWithStalePerms))
		metricsPermsGap.WithLabelValues("repo").Set(m.ReposPermsGapSeconds)

		s.queue.mu.RLock()
		metricsQueueSize.Set(float64(s.queue.Len()))
		s.queue.mu.RUnlock()
	}
}

// Run kicks off the permissions syncing process, this method is blocking and
// should be called as a goroutine.
func (s *PermsSyncer) Run(ctx context.Context) {
	go s.runSync(ctx)
	go s.runSchedule(ctx)
	go s.collectMetrics(ctx)

	<-ctx.Done()
}
