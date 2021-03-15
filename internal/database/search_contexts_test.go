package database

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/sourcegraph/sourcegraph/internal/database/dbtesting"
	"github.com/sourcegraph/sourcegraph/internal/search"
	"github.com/sourcegraph/sourcegraph/internal/types"
)

func createSearchContexts(ctx context.Context, store *SearchContextsStore, searchContexts []*types.SearchContext) ([]*types.SearchContext, error) {
	emptyRepositoryRevisions := []*search.RepositoryRevisions{}
	createdSearchContexts := make([]*types.SearchContext, len(searchContexts))
	for idx, searchContext := range searchContexts {
		createdSearchContext, err := store.CreateSearchContextWithRepositoryRevisions(ctx, searchContext, emptyRepositoryRevisions)
		if err != nil {
			return nil, err
		}
		createdSearchContexts[idx] = createdSearchContext
	}
	return createdSearchContexts, nil
}

func TestSearchContexts_Get(t *testing.T) {
	db := dbtesting.GetDB(t)
	ctx := context.Background()
	u := Users(db)
	o := Orgs(db)
	sc := SearchContexts(db)

	user, err := u.Create(ctx, NewUser{Username: "u", Password: "p"})
	if err != nil {
		t.Errorf("Expected no error, got %s", err)
	}
	displayName := "My Org"
	org, err := o.Create(ctx, "myorg", &displayName)
	if err != nil {
		t.Errorf("Expected no error, got %s", err)
	}

	createdSearchContexts, err := createSearchContexts(ctx, sc, []*types.SearchContext{
		{Name: "instance", Description: "instance level", Public: true},
		{Name: "user", Description: "user level", Public: true, NamespaceUserID: user.ID},
		{Name: "org", Description: "org level", Public: true, NamespaceOrgID: org.ID},
	})
	if err != nil {
		t.Errorf("Expected no error, got %s", err)
	}

	tests := []struct {
		name    string
		opts    GetSearchContextOptions
		want    *types.SearchContext
		wantErr string
	}{
		{name: "get instance-level search context", opts: GetSearchContextOptions{}, want: createdSearchContexts[0]},
		{name: "get user search context", opts: GetSearchContextOptions{NamespaceUserID: user.ID}, want: createdSearchContexts[1]},
		{name: "get org search context", opts: GetSearchContextOptions{NamespaceOrgID: org.ID}, want: createdSearchContexts[2]},
		{name: "get user and org context", opts: GetSearchContextOptions{NamespaceUserID: 1, NamespaceOrgID: 2}, wantErr: "options NamespaceUserID and NamespaceOrgID are mutually exclusive"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			searchContext, err := sc.GetSearchContext(ctx, tt.opts)
			if err != nil && !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("got error %v, want it to contain %q", err, tt.wantErr)
			}
			if !reflect.DeepEqual(tt.want, searchContext) {
				t.Errorf("wanted %v search contexts, got %v", tt.want, searchContext)
			}
		})
	}
}

func TestSearchContexts_List(t *testing.T) {
	db := dbtesting.GetDB(t)
	ctx := context.Background()
	u := Users(db)
	sc := SearchContexts(db)

	user, err := u.Create(ctx, NewUser{Username: "u", Password: "p"})
	if err != nil {
		t.Errorf("Expected no error, got %s", err)
	}

	createdSearchContexts, err := createSearchContexts(ctx, sc, []*types.SearchContext{
		{Name: "instance", Description: "instance level", Public: true},
		{Name: "user", Description: "user level", Public: true, NamespaceUserID: user.ID},
	})
	if err != nil {
		t.Errorf("Expected no error, got %s", err)
	}

	wantInstanceLevelSearchContexts := createdSearchContexts[:1]
	gotInstanceLevelSearchContexts, err := sc.ListInstanceLevelSearchContexts(ctx)
	if err != nil {
		t.Errorf("Expected no error, got %s", err)
	}
	if !reflect.DeepEqual(wantInstanceLevelSearchContexts, gotInstanceLevelSearchContexts) {
		t.Errorf("wanted %v search contexts, got %v", wantInstanceLevelSearchContexts, wantInstanceLevelSearchContexts)
	}

	wantUserSearchContexts := createdSearchContexts[1:]
	gotUserSearchContexts, err := sc.ListSearchContextsByUserID(ctx, user.ID)
	if err != nil {
		t.Errorf("Expected no error, got %s", err)
	}
	if !reflect.DeepEqual(wantUserSearchContexts, gotUserSearchContexts) {
		t.Errorf("wanted %v search contexts, got %v", wantUserSearchContexts, gotUserSearchContexts)
	}
}

func TestSearchContexts_CreateAndSetRepositoryRevisions(t *testing.T) {
	db := dbtesting.GetDB(t)
	ctx := context.Background()
	sc := SearchContexts(db)
	r := Repos(db)

	err := r.Create(ctx, &types.Repo{Name: "testA", URI: "https://example.com/a"}, &types.Repo{Name: "testB", URI: "https://example.com/b"})
	if err != nil {
		t.Errorf("Expected no error, got %s", err)
	}
	repoA, err := r.GetByName(ctx, "testA")
	if err != nil {
		t.Errorf("Expected no error, got %s", err)
	}
	repoB, err := r.GetByName(ctx, "testB")
	if err != nil {
		t.Errorf("Expected no error, got %s", err)
	}

	repoAName := &types.RepoName{ID: repoA.ID, Name: repoA.Name}
	repoBName := &types.RepoName{ID: repoB.ID, Name: repoB.Name}

	// Create a search context with initial repository revisions
	initialRepositoryRevisions := []*search.RepositoryRevisions{
		{Repo: repoAName, Revs: []search.RevisionSpecifier{{RevSpec: "branch-1"}, {RevSpec: "branch-6"}}},
		{Repo: repoBName, Revs: []search.RevisionSpecifier{{RevSpec: "branch-2"}}},
	}
	searchContext, err := sc.CreateSearchContextWithRepositoryRevisions(
		ctx,
		&types.SearchContext{Name: "sc", Description: "sc", Public: true},
		initialRepositoryRevisions,
	)
	if err != nil {
		t.Errorf("Expected no error, got %s", err)
	}
	gotRepositoryRevisions, err := sc.GetSearchContextRepositoryRevisions(ctx, searchContext.ID)
	if err != nil {
		t.Errorf("Expected no error, got %s", err)
	}
	if !reflect.DeepEqual(initialRepositoryRevisions, gotRepositoryRevisions) {
		t.Errorf("wanted %v repository revisions, got %v", initialRepositoryRevisions, gotRepositoryRevisions)
	}

	// Modify the repository revisions for the search context
	modifiedRepositoryRevisions := []*search.RepositoryRevisions{
		{Repo: repoAName, Revs: []search.RevisionSpecifier{{RevSpec: "branch-1"}, {RevSpec: "branch-3"}}},
		{Repo: repoBName, Revs: []search.RevisionSpecifier{{RevSpec: "branch-0"}, {RevSpec: "branch-2"}, {RevSpec: "branch-4"}}},
	}
	err = sc.SetSearchContextRepositoryRevisions(ctx, searchContext.ID, modifiedRepositoryRevisions)
	if err != nil {
		t.Errorf("Expected no error, got %s", err)
	}
	gotRepositoryRevisions, err = sc.GetSearchContextRepositoryRevisions(ctx, searchContext.ID)
	if err != nil {
		t.Errorf("Expected no error, got %s", err)
	}
	if !reflect.DeepEqual(modifiedRepositoryRevisions, gotRepositoryRevisions) {
		t.Errorf("wanted %v repository revisions, got %v", modifiedRepositoryRevisions, gotRepositoryRevisions)
	}
}
