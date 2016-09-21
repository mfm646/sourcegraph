package accesscontrol

import (
	"fmt"
	"strconv"
	"strings"

	"context"

	gogithub "github.com/sourcegraph/go-github/github"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"sourcegraph.com/sourcegraph/sourcegraph/api/sourcegraph"
	"sourcegraph.com/sourcegraph/sourcegraph/pkg/auth"
	"sourcegraph.com/sourcegraph/sourcegraph/pkg/store"
	"sourcegraph.com/sourcegraph/sourcegraph/services/ext/github"
)

// VerifyUserHasReadAccess checks if the user in the current context
// is authorized to make write requests to this server.
//
// This method always returns nil when the user has write access,
// and returns a non-nil error when access cannot be granted.
// If the cmdline flag auth.restrict-write-access is set, this method
// will check if the authenticated user has admin privileges.
func VerifyUserHasReadAccess(ctx context.Context, method string, repo interface{}) error {
	return VerifyActorHasReadAccess(ctx, auth.ActorFromContext(ctx), method, repo)
}

// VerifyUserHasWriteAccess checks if the user in the current context
// is authorized to make write requests to this server.
//
// This method always returns nil when the user has write access,
// and returns a non-nil error when access cannot be granted.
// If the cmdline flag auth.restrict-write-access is set, this method
// will check if the authenticated user has admin privileges.
func VerifyUserHasWriteAccess(ctx context.Context, method string, repo interface{}) error {
	return VerifyActorHasWriteAccess(ctx, auth.ActorFromContext(ctx), method, repo)
}

// VerifyUserHasWriteAccess checks if the user in the current context
// is authorized to make admin requests to this server.
func VerifyUserHasAdminAccess(ctx context.Context, method string) error {
	return VerifyActorHasAdminAccess(ctx, auth.ActorFromContext(ctx), method)
}

// VerifyUserSelfOrAdmin checks if the user in the current context has
// the given uid, or if the actor has admin access on the server.
// This check should be used in cases where a request should succeed only
// if the request is for the user's own information, or if the ctx actor is an admin.
func VerifyUserSelfOrAdmin(ctx context.Context, method string, uid string) error {
	if uid != "" && auth.ActorFromContext(ctx).UID == uid {
		return nil
	}

	return VerifyUserHasAdminAccess(ctx, method)
}

// VerifyClientSelfOrAdmin checks if the client in the current context has
// the given id, or if the actor has admin access on the server.
// This check should be used in cases where a request should succeed only
// if the request is for the client's own information, or if the ctx actor is an admin.
func VerifyClientSelfOrAdmin(ctx context.Context, method string, clientID string) error {
	return VerifyUserHasAdminAccess(ctx, method)
}

// VerifyActorHasGitHubRepoAccess delegate permissions check to GitHub
// for GitHub mirrored repos. The repo string MUST be of the form
// "github.com/USER/REPO"; it MUST begin with "github.com/" if it is a
// GitHub repository whose access permissions should be delegated to
// GitHub.
//
// NOTE: Only (*localstore.repos).Get method should call this
// func. All other callers should use
// Verify{User,Actor}Has{Read,Write}Access funcs. This func is
// specially designed to avoid infinite loops with
// (*localstore.repos).Get.
//
// TODO: move to a security model that is more robust, readable, has
// better separation when dealing with multiple configurations, actor
// types, resource types and actions.
func VerifyActorHasGitHubRepoAccess(ctx context.Context, actor *auth.Actor, method string, repo int32, repoURI string) error {
	if skip(ctx) {
		return nil
	}

	if repo == 0 || repoURI == "" {
		panic("both repo and repoURI must be set")
	}

	if strings.HasPrefix(strings.ToLower(repoURI), "github.com/") {
		if !VerifyScopeHasAccess(ctx, actor.Scope, method, repo) {
			_, err := github.ReposFromContext(ctx).Get(ctx, repoURI)
			if _, ok := err.(*gogithub.RateLimitError); ok {
				return grpc.Errorf(codes.ResourceExhausted, "GitHub API rate limit exceeded, try again later")
			} else if err != nil {
				// TODO: We don't support git clients anymore, get rid of this.
				// We don't know if the error is unauthenticated or unauthorized, so return unauthenticated
				// so that git clients will try again, providing authentication information.
				// If we return codes.PermissionDenied here, then git clients won't even try to supply authentication info.
				return grpc.Errorf(codes.Unauthenticated, "operation (%s) denied: not authenticated/authorized by GitHub API (repo %q)", method, repoURI)
			}
		}
	}
	return nil
}

func getRepo(ctx context.Context, repoIDOrURI interface{}) (repoID int32, repoURI string, err error) {
	repoURI, _ = repoIDOrURI.(string)
	repoID, _ = repoIDOrURI.(int32)

	var repoObj *sourcegraph.Repo
	if repoID != 0 && repoURI == "" {
		repoObj, err = store.ReposFromContext(ctx).Get(ctx, repoID)
	} else if repoURI != "" && repoID == 0 {
		repoObj, err = store.ReposFromContext(ctx).GetByURI(ctx, repoURI)
	}
	if err != nil {
		return
	}
	if repoObj != nil {
		repoID = repoObj.ID
		repoURI = repoObj.URI
	}
	return
}

// VerifyActorHasReadAccess checks if the given actor is authorized to make
// read requests to this server.
//
// Note that this function allows the caller to retrieve any user's
// access levels.  This is meant for trusted server code living
// outside the scope of gRPC requests to verify user permissions. For
// all other cases, VerifyUserHasWriteAccess or
// VerifyUserHasAdminAccess should be used to authorize a user for
// gRPC operations.
func VerifyActorHasReadAccess(ctx context.Context, actor *auth.Actor, method string, repo interface{}) error {
	if skip(ctx) {
		return nil
	}

	if repo != nil {
		repoID, repoURI, err := getRepo(ctx, repo)
		if err != nil {
			return err
		}
		if err := VerifyActorHasGitHubRepoAccess(ctx, actor, method, repoID, repoURI); err != nil {
			return err
		}
	}
	return nil
}

// VerifyUserHasReadAccessAll verifies checks if the current actor
// can access these repositories. This method implements a more
// efficient way of verifying permissions on a set of repositories.
// (Calling VerifyHasRepoAccess on each individual repository in a
// long list of repositories incurs too many GitHub API requests.)
// Unlike other authentication checking functions in this package,
// this function assumes that the list of repositories passed in has a
// correct `Private` field. This method does not incur a GitHub API
// call for public repositories.
//
// Unlike other access functions, this function does not return an
// error when there is a permission-denied error for one of the
// repositories. Instead, the first return value is the list of
// repositories to which access is allowed. If permission was denied
// for any repository, this list will be shorter than the repos
// argument. If there is any error in determining the list of allowed
// repositories, the second return value will be non-nil error.
func VerifyUserHasReadAccessAll(ctx context.Context, method string, repos []*sourcegraph.Repo) (allowed []*sourcegraph.Repo, err error) {
	if skip(ctx) {
		return repos, nil
	}

	var privateRepos []*sourcegraph.Repo
	for _, repo := range repos {
		if repo.Private { // only check access if repository is marked "Private"
			privateRepos = append(privateRepos, repo)
		} else {
			allowed = append(allowed, repo)
		}
	}

	// If private repositories exist, list all accessible GitHub
	// repositories and cross-check against that list.
	if len(privateRepos) > 0 {
		ghrepoURIs := make(map[string]struct{})
		ghrepos, err := github.ListAllGitHubRepos(ctx, &gogithub.RepositoryListOptions{Type: "private"})
		if err != nil {
			return nil, fmt.Errorf("could not list all accessible GitHub repositories: %s", err)
		}
		for _, ghrepo := range ghrepos {
			ghrepoURIs[ghrepo.URI] = struct{}{}
		}

		for _, repo := range privateRepos {
			if _, isGitHubAccessible := ghrepoURIs[repo.URI]; isGitHubAccessible {
				allowed = append(allowed, repo)
			}
		}
	}
	return allowed, nil
}

// VerifyActorHasWriteAccess checks if the given actor is authorized to make
// write requests to this server.
//
// Note that this function allows the caller to retrieve any user's
// access levels.  This is meant for trusted server code living
// outside the scope of gRPC requests to verify user permissions. For
// all other cases, VerifyUserHasWriteAccess should be used to
// authorize a user for gRPC operations.
func VerifyActorHasWriteAccess(ctx context.Context, actor *auth.Actor, method string, repo interface{}) error {
	if skip(ctx) {
		return nil
	}

	repoID, repoURI, err := getRepo(ctx, repo)
	if err != nil {
		return err
	}

	// TODO: redesign the permissions model to avoid short-circuited "return nil"s.
	// (because it makes modifying authorization logic more error-prone.)

	if !actor.IsAuthenticated() {
		if VerifyScopeHasAccess(ctx, actor.Scope, method, repoID) {
			return nil
		}
		return grpc.Errorf(codes.Unauthenticated, "write operation (%s) denied: not authenticated", method)
	}

	if !inAuthenticatedWriteWhitelist(method) {
		return grpc.Errorf(codes.PermissionDenied, "write operation (%s) denied: user does not have write access", method)
	}

	if repoID != 0 && repoURI != "" {
		if err := VerifyActorHasGitHubRepoAccess(ctx, actor, method, repoID, repoURI); err != nil {
			return err
		}
	}

	return nil
}

// VerifyActorHasAdminAccess checks if the given actor is authorized to make
// admin requests to this server.
//
// Note that this function allows the caller to retrieve any user's
// access levels.  This is meant for trusted server code living
// outside the scope of gRPC requests to verify user permissions. For
// all other cases, VerifyUserHasAdminAccess should be used to
// authorize a user for gRPC operations.
func VerifyActorHasAdminAccess(ctx context.Context, actor *auth.Actor, method string) error {
	if skip(ctx) {
		return nil
	}

	if !actor.IsAuthenticated() {
		if VerifyScopeHasAccess(ctx, actor.Scope, method, 0) {
			return nil
		}
		return grpc.Errorf(codes.Unauthenticated, "admin operation (%s) denied: not authenticated", method)
	}

	return grpc.Errorf(codes.PermissionDenied, "admin operation (%s) denied: not authorized", method)
}

// Check if the actor is authorized with an access token
// having a valid scope. This token is set in package cli on server
// startup, and is only available to client commands spawned
// in the server process.
//
// !!!!!!!!!!!!!!!!!!!! DANGER(security) !!!!!!!!!!!!!!!!!!!!!!
// This does not check that the token is properly signed, since
// that is done in server/internal/oauth2util/grpc_middleware.go
// when parsing the request metadata and adding the actor to the
// context. To avoid additional latency from expensive public key
// operations, that check is not repeated here, but be careful
// about refactoring that check.
func VerifyScopeHasAccess(ctx context.Context, scopes map[string]bool, method string, repo int32) bool {
	if skip(ctx) {
		return true
	}

	if scopes == nil {
		return false
	}
	for scope := range scopes {
		switch {
		case strings.HasPrefix(scope, "internal:"):
			// internal server commands have default write access.
			return true

		case scope == "worker:build":
			return true

		case strings.HasPrefix(scope, "repo:"):
			scopeRepo, err := strconv.Atoi(strings.TrimPrefix(scope, "repo:"))
			if err != nil {
				return false
			}
			if repo != 0 && int32(scopeRepo) == repo {
				return true
			}
		}
	}
	return false
}

// inAuthenticatedWriteWhitelist reports if we always allow write access
// for method to any authenticated user.
func inAuthenticatedWriteWhitelist(method string) bool {
	switch method {
	case "MirrorRepos.cloneRepo":
		// This is used for read-only users to be able to trigger mirror clones
		// of public repositories, effectively "enabling" that repository.
		return true
	default:
		return false
	}
}

// Allow skipping access checks when testing other packages.
type contextKey int

const insecureSkip contextKey = 0

// WithInsecureSkip skips all access checks performed using ctx or one
// of its descendants. It is INSECURE and should only be used during
// testing.
func WithInsecureSkip(ctx context.Context, skip bool) context.Context {
	return context.WithValue(ctx, insecureSkip, skip)
}

func skip(ctx context.Context) bool {
	v, _ := ctx.Value(insecureSkip).(bool)
	return v
}
