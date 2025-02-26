// Package searcher provides a client for our just in time text searching
// service "searcher".
package searcher

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/opentracing-contrib/go-stdlib/nethttp"

	"github.com/sourcegraph/sourcegraph/cmd/searcher/protocol"
	"github.com/sourcegraph/sourcegraph/internal/api"
	"github.com/sourcegraph/sourcegraph/internal/endpoint"
	"github.com/sourcegraph/sourcegraph/internal/errcode"
	"github.com/sourcegraph/sourcegraph/internal/httpcli"
	"github.com/sourcegraph/sourcegraph/internal/search"
	"github.com/sourcegraph/sourcegraph/internal/trace"
	"github.com/sourcegraph/sourcegraph/internal/trace/ot"
)

var (
	searchDoer, _ = httpcli.NewInternalClientFactory("search").Doer()
	MockSearch    func(ctx context.Context, repo api.RepoName, commit api.CommitID, p *search.TextPatternInfo, fetchTimeout time.Duration) (matches []*protocol.FileMatch, limitHit bool, err error)
)

// Search searches repo@commit with p.
func Search(
	ctx context.Context,
	searcherURLs *endpoint.Map,
	repo api.RepoName,
	branch string,
	commit api.CommitID,
	indexed bool,
	p *search.TextPatternInfo,
	fetchTimeout time.Duration,
	indexerEndpoints []string,
	onMatches func([]*protocol.FileMatch),
) (matches []*protocol.FileMatch, limitHit bool, err error) {
	if MockSearch != nil {
		return MockSearch(ctx, repo, commit, p, fetchTimeout)
	}

	tr, ctx := trace.New(ctx, "searcher.client", fmt.Sprintf("%s@%s", repo, commit))
	defer func() {
		tr.SetError(err)
		tr.Finish()
	}()

	q := url.Values{
		"Repo":            []string{string(repo)},
		"Commit":          []string{string(commit)},
		"Branch":          []string{branch},
		"Pattern":         []string{p.Pattern},
		"ExcludePattern":  []string{p.ExcludePattern},
		"IncludePatterns": p.IncludePatterns,
		"FetchTimeout":    []string{fetchTimeout.String()},
		"Languages":       p.Languages,
		"CombyRule":       []string{p.CombyRule},

		"PathPatternsAreRegExps": []string{"true"},
		"IndexerEndpoints":       indexerEndpoints,
		"Select":                 []string{p.Select.Root()},
	}
	if deadline, ok := ctx.Deadline(); ok {
		t, err := deadline.MarshalText()
		if err != nil {
			return nil, false, err
		}
		q.Set("Deadline", string(t))
	}
	q.Set("Limit", strconv.FormatInt(int64(p.FileMatchLimit), 10))
	if p.IsRegExp {
		q.Set("IsRegExp", "true")
	}
	if p.IsStructuralPat {
		q.Set("IsStructuralPat", "true")
	}
	if indexed {
		q.Set("Indexed", "true")
	}
	if p.IsWordMatch {
		q.Set("IsWordMatch", "true")
	}
	if p.IsCaseSensitive {
		q.Set("IsCaseSensitive", "true")
	}
	if p.PathPatternsAreCaseSensitive {
		q.Set("PathPatternsAreCaseSensitive", "true")
	}
	if p.IsNegated {
		q.Set("IsNegated", "true")
	}
	if onMatches != nil {
		q.Set("Stream", "true")
	}
	// TEMP BACKCOMPAT: always set even if false so that searcher can distinguish new frontends that send
	// these fields from old frontends that do not (and provide a default in the latter case).
	q.Set("PatternMatchesContent", strconv.FormatBool(p.PatternMatchesContent))
	q.Set("PatternMatchesPath", strconv.FormatBool(p.PatternMatchesPath))
	rawQuery := q.Encode()

	// Searcher caches the file contents for repo@commit since it is
	// relatively expensive to fetch from gitserver. So we use consistent
	// hashing to increase cache hits.
	consistentHashKey := string(repo) + "@" + string(commit)
	tr.LazyPrintf("%s", consistentHashKey)

	var (
		// When we retry do not use a host we already tried.
		excludedSearchURLs = map[string]bool{}
		attempt            = 0
		maxAttempts        = 2
	)
	for {
		attempt++

		searcherURL, err := searcherURLs.Get(consistentHashKey, excludedSearchURLs)
		if err != nil {
			return nil, false, err
		}

		// Fallback to a bad host if nothing is left
		if searcherURL == "" {
			tr.LazyPrintf("failed to find endpoint, trying again without excludes")
			searcherURL, err = searcherURLs.Get(consistentHashKey, nil)
			if err != nil {
				return nil, false, err
			}
		}

		url := searcherURL + "?" + rawQuery
		tr.LazyPrintf("attempt %d: %s", attempt, url)
		if onMatches != nil {
			limitHit, err = textSearchURLStream(ctx, url, onMatches)
			if err == nil || errcode.IsTimeout(err) {
				return nil, limitHit, err
			}
		} else {
			matches, limitHit, err = textSearchURL(ctx, url)
			if err == nil || errcode.IsTimeout(err) {
				return matches, limitHit, err
			}
		}

		// If we are canceled, return that error.
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}

		// If not temporary or our last attempt then don't try again.
		if !errcode.IsTemporary(err) || attempt == maxAttempts {
			return nil, false, err
		}

		tr.LazyPrintf("transient error %s", err.Error())
		// Retry search on another searcher instance (if possible)
		excludedSearchURLs[searcherURL] = true
	}
}

func textSearchURLStream(ctx context.Context, url string, cb func([]*protocol.FileMatch)) (bool, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return false, err
	}
	req = req.WithContext(ctx)

	req, ht := nethttp.TraceRequest(ot.GetTracer(ctx), req,
		nethttp.OperationName("Searcher Client"),
		nethttp.ClientTrace(false))
	defer ht.Finish()

	// Do not lose the context returned by TraceRequest
	ctx = req.Context()

	resp, err := searchDoer.Do(req)
	if err != nil {
		// If we failed due to cancellation or timeout (with no partial results in the response
		// body), return just that.
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return false, errors.Wrap(err, "streaming searcher request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return false, err
		}
		return false, errors.WithStack(&searcherError{StatusCode: resp.StatusCode, Message: string(body)})
	}

	var ed EventDone
	dec := StreamDecoder{
		OnMatches: cb,
		OnDone: func(e EventDone) {
			ed = e
		},
		OnUnknown: func(event []byte, _ []byte) {
			err = errors.Errorf("unknown event %q", event)
		},
	}
	if err := dec.ReadAll(resp.Body); err != nil {
		return false, err
	}
	if ed.Error != "" {
		return false, errors.New(ed.Error)
	}
	if ed.DeadlineHit {
		err = context.DeadlineExceeded
	}
	return ed.LimitHit, err
}

func textSearchURL(ctx context.Context, url string) ([]*protocol.FileMatch, bool, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, false, err
	}
	req = req.WithContext(ctx)

	req, ht := nethttp.TraceRequest(ot.GetTracer(ctx), req,
		nethttp.OperationName("Searcher Client"),
		nethttp.ClientTrace(false))
	defer ht.Finish()

	// Do not lose the context returned by TraceRequest
	ctx = req.Context()

	resp, err := searchDoer.Do(req)
	if err != nil {
		// If we failed due to cancellation or timeout (with no partial results in the response
		// body), return just that.
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return nil, false, errors.Wrap(err, "searcher request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, false, err
		}
		return nil, false, errors.WithStack(&searcherError{StatusCode: resp.StatusCode, Message: string(body)})
	}

	r := struct {
		Matches     []*protocol.FileMatch
		LimitHit    bool
		DeadlineHit bool
	}{}
	err = json.NewDecoder(resp.Body).Decode(&r)
	if err != nil {
		return nil, false, errors.Wrap(err, "searcher response invalid")
	}
	if r.DeadlineHit {
		err = context.DeadlineExceeded
	}
	return r.Matches, r.LimitHit, err
}

type searcherError struct {
	StatusCode int
	Message    string
}

func (e *searcherError) BadRequest() bool {
	return e.StatusCode == http.StatusBadRequest
}

func (e *searcherError) Temporary() bool {
	return e.StatusCode == http.StatusServiceUnavailable
}

func (e *searcherError) Error() string {
	return e.Message
}
