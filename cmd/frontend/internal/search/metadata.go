package search

import (
	"context"

	"github.com/cockroachdb/errors"

	"github.com/sourcegraph/sourcegraph/internal/api"
	"github.com/sourcegraph/sourcegraph/internal/database"
	"github.com/sourcegraph/sourcegraph/internal/database/dbutil"
	"github.com/sourcegraph/sourcegraph/internal/search/streaming"
	"github.com/sourcegraph/sourcegraph/internal/types"
)

func getEventRepoMetadata(ctx context.Context, db dbutil.DB, event streaming.SearchEvent) (map[api.RepoID]*types.SearchedRepo, error) {
	ids := repoIDs(event.Results)
	if len(ids) == 0 {
		// Return early if there are no repos in the event
		return nil, nil
	}

	metadataList, err := database.Repos(db).Metadata(ctx, ids...)
	if err != nil {
		return nil, errors.Wrap(err, "fetch metadata from db")
	}

	repoMetadata := make(map[api.RepoID]*types.SearchedRepo, len(ids))
	for _, repo := range metadataList {
		repoMetadata[repo.ID] = repo
	}
	return repoMetadata, nil
}
