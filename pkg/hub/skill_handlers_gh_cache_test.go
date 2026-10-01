// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build !no_sqlite

package hub

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

// fakeGitHub stands in for api.github.com for the two endpoints the gh://
// resolver uses, counting every request it serves so tests can assert that a
// cache hit reaches no further than the DB.
type fakeGitHub struct {
	*httptest.Server
	calls       atomic.Int64 // total calls (commits + contents)
	commitCalls atomic.Int64 // commits/{ref} calls only
}

// newFakeGitHub serves the commits and contents endpoints for owner/repo,
// returning commitSHA and a single-file listing under skillPath.
func newFakeGitHub(t *testing.T, owner, repo, skillPath, commitSHA string) *fakeGitHub {
	t.Helper()

	f := &fakeGitHub{}
	commitsPrefix := "/repos/" + owner + "/" + repo + "/commits/"
	contentsPrefix := "/repos/" + owner + "/" + repo + "/contents/"

	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		switch {
		case strings.HasPrefix(r.URL.Path, commitsPrefix):
			f.commitCalls.Add(1)
			// Accept: application/vnd.github.v3.sha — a bare SHA, not JSON.
			_, _ = w.Write([]byte(commitSHA))
		case strings.HasPrefix(r.URL.Path, contentsPrefix):
			assert.Equal(t, commitSHA, r.URL.Query().Get("ref"),
				"contents must be fetched at the resolved commit, not the symbolic ref")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{
				"name": "SKILL.md",
				"path": "` + skillPath + `/SKILL.md",
				"sha": "ce013625030ba8dba906f756967f9e9ca394464a",
				"size": 6,
				"type": "file",
				"download_url": "https://example.invalid/should-be-ignored"
			}]`))
		default:
			t.Errorf("unexpected GitHub API request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

// TestSkillsResolve_GHCacheHitOnSecondResolve is the end-to-end proof that the
// Hub-side gh:// cache is what makes Phase 3 worth flipping: routing agent
// gh:// resolutions through the Hub only pays off if the Hub absorbs repeat
// resolutions instead of forwarding each one to GitHub.
//
// Two identical resolve calls must produce identical responses while the second
// one reaches GitHub zero times.
func TestSkillsResolve_GHCacheHitOnSecondResolve(t *testing.T) {
	const (
		owner     = "acme"
		repo      = "private-repo"
		skillPath = "skills/secret"
		uri       = "gh://" + owner + "/" + repo + "/secret"
		commitSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	)

	srv, _, alice, _, project := setupSkillAuthzTest(t)

	// testServer leaves ghResolutionStore nil (it is only wired by
	// SetIntegrationHA in production), which would disable caching entirely and
	// make this test vacuous. Give it its own migrated SQLite client.
	srv.ghResolutionStore = NewGitHubResolutionStore(enttest.NewClient(t))

	gh := newFakeGitHub(t, owner, repo, skillPath, commitSHA)
	srv.config.GitHubAppConfig.APIBaseURL = gh.URL
	srv.config.GitHubAppConfig.RawBaseURL = gh.URL

	body := ResolveSkillsRequest{
		Skills:    []ResolveSkillRef{{URI: uri}},
		ProjectID: project.ID,
	}

	resolve := func(t *testing.T) ResolvedSkillResponse {
		t.Helper()
		rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/skills/resolve", body)
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

		var resp ResolveSkillsResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		require.Empty(t, resp.Errors, "resolution must succeed against the fake GitHub")
		require.Len(t, resp.Resolved, 1)
		return resp.Resolved[0]
	}

	// First resolve: a cache miss, so both GitHub endpoints are contacted —
	// commits/<ref> to pin the SHA, then contents at that SHA.
	first := resolve(t)
	missCalls := gh.calls.Load()
	require.Equal(t, int64(2), missCalls,
		"cache miss should call commits + contents exactly once each")

	// Second resolve: identical request, served entirely from the DB cache.
	second := resolve(t)

	assert.Equal(t, missCalls, gh.calls.Load(),
		"second resolve must be served from the DB cache and make no GitHub API calls")
	assert.Equal(t, first, second,
		"a cache hit must reproduce the miss response byte for byte")

	// Sanity-check that the cached response is actually populated, so an
	// all-empty response cannot satisfy the equality assertion above.
	assert.Equal(t, uri, second.URI)
	assert.Equal(t, "secret", second.Name)
	assert.NotEmpty(t, second.ContentHash)
	require.Len(t, second.Files, 1)
	assert.Contains(t, second.Files[0].URL, commitSHA,
		"file URL must be pinned to the resolved commit")
}

// TestSkillsResolve_GHCacheKeyedByURI confirms the cache discriminates between
// skills: a second, different gh:// URI must not be served the first one's
// cached entry.
func TestSkillsResolve_GHCacheKeyedByURI(t *testing.T) {
	const (
		owner     = "acme"
		repo      = "private-repo"
		commitSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	)

	srv, _, alice, _, project := setupSkillAuthzTest(t)
	srv.ghResolutionStore = NewGitHubResolutionStore(enttest.NewClient(t))

	// Serve any skill path under the repo.
	gh := newFakeGitHub(t, owner, repo, "skills/any", commitSHA)
	srv.config.GitHubAppConfig.APIBaseURL = gh.URL
	srv.config.GitHubAppConfig.RawBaseURL = gh.URL

	resolve := func(t *testing.T, uri string) {
		t.Helper()
		rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/skills/resolve",
			ResolveSkillsRequest{
				Skills:    []ResolveSkillRef{{URI: uri}},
				ProjectID: project.ID,
			})
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

		var resp ResolveSkillsResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		require.Empty(t, resp.Errors)
		require.Len(t, resp.Resolved, 1)
	}

	const uriA = "gh://" + owner + "/" + repo + "/first"

	resolve(t, uriA)
	afterFirst := gh.calls.Load()
	require.Equal(t, int64(2), afterFirst)

	resolve(t, "gh://"+owner+"/"+repo+"/second")
	require.Equal(t, int64(4), gh.calls.Load(),
		"a different skill path must miss the cache and hit GitHub")

	// Re-resolve the first URI. Without this the test is vacuous: two distinct
	// uncached resolves also cost 2 then 4 calls, so the assertions above hold
	// even with caching disabled. A cache hit here pins both halves of the
	// claim — caching is active, and the second URI's entry neither evicted the
	// first nor aliased onto it.
	resolve(t, uriA)
	assert.Equal(t, int64(4), gh.calls.Load(),
		"re-resolving the first URI must hit its own cache entry and make no further GitHub calls")
}

// TestSkillsResolve_GHDeclinesTokenSecretURI pins the one gh:// shape the Hub
// must refuse to resolve. `?token=NAME` names a ProvisionCredentials secret
// that exists only on the broker; the Hub has no way to read it. If the Hub
// resolved these anyway it would silently substitute the project's GitHub App
// token and return raw.githubusercontent.com URLs the broker cannot
// authenticate at install time — a confusing download failure well after the
// resolve appeared to succeed.
//
// Declining with an error is load-bearing: the broker's
// RoutingSkillResolver.retryErrorsWithFallback turns any per-URI error into a
// fallback to the local resolver, which does look up the named secret. So the
// error is the routing signal, not a dead end.
//
// The fake GitHub here would happily serve this URI, so the test genuinely
// discriminates: without the guard the request resolves successfully.
func TestSkillsResolve_GHDeclinesTokenSecretURI(t *testing.T) {
	const (
		owner     = "acme"
		repo      = "private-repo"
		skillPath = "skills/secret"
		commitSHA = "cccccccccccccccccccccccccccccccccccccccc"
	)

	srv, _, alice, _, project := setupSkillAuthzTest(t)
	srv.ghResolutionStore = NewGitHubResolutionStore(enttest.NewClient(t))

	gh := newFakeGitHub(t, owner, repo, skillPath, commitSHA)
	srv.config.GitHubAppConfig.APIBaseURL = gh.URL
	srv.config.GitHubAppConfig.RawBaseURL = gh.URL

	rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/skills/resolve", ResolveSkillsRequest{
		Skills:    []ResolveSkillRef{{URI: "gh://" + owner + "/" + repo + "/secret?token=MY_TOKEN"}},
		ProjectID: project.ID,
	})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp ResolveSkillsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	assert.Empty(t, resp.Resolved,
		"Hub must not resolve a ?token= URI: it cannot read the named broker secret")
	require.Len(t, resp.Errors, 1, "the declined URI must surface as a per-URI error")

	// The error must be a resolve failure, not an authz denial: Alice owns the
	// project, so authz passed and the Hub declined on its own terms. A
	// "forbidden" here would mean the broker's fallback is masking a real
	// permission bug.
	assert.NotEqual(t, "forbidden", resp.Errors[0].Code,
		"authz should pass for the project owner; got forbidden: %s", resp.Errors[0].Message)
	assert.Equal(t, "resolve_failed", resp.Errors[0].Code)
	assert.Contains(t, resp.Errors[0].Message, "local resolver",
		"the error should explain that the local resolver owns this URI shape")

	// The Hub must decline before contacting GitHub — otherwise it has already
	// minted and spent the project's App token on a request it cannot serve.
	assert.Zero(t, gh.calls.Load(),
		"Hub must decline the ?token= URI without calling GitHub")

	// A ?token=-free URI for the same skill still resolves, so the guard is
	// scoped to the token parameter rather than disabling gh:// caching.
	rec = doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/skills/resolve", ResolveSkillsRequest{
		Skills:    []ResolveSkillRef{{URI: "gh://" + owner + "/" + repo + "/secret"}},
		ProjectID: project.ID,
	})
	require.Equal(t, http.StatusOK, rec.Code)

	// Decode into a fresh value: omitted JSON fields leave the previous
	// response's Errors in place and would make this assertion meaningless.
	var plainResp ResolveSkillsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&plainResp))
	assert.Empty(t, plainResp.Errors, "the same skill without ?token= must still resolve")
	assert.Len(t, plainResp.Resolved, 1)
}

// TestSkillsResolve_GHRefDedup asserts that a batch request containing N URIs
// sharing the same (owner, repo, ref) performs only one ref→SHA lookup via
// commits/{ref}, not one per URI. Each URI still triggers its own contents
// lookup — dedup applies only to the SHA resolution step.
//
// This is the acceptance test for the issue-1 performance fix: 19 same-repo
// URIs should cost 1 + 19 = 20 GitHub API calls, not 19 × 2 = 38.
func TestSkillsResolve_GHRefDedup(t *testing.T) {
	const (
		owner     = "acme"
		repo      = "bundle-repo"
		commitSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)

	srv, _, alice, _, project := setupSkillAuthzTest(t)
	srv.ghResolutionStore = NewGitHubResolutionStore(enttest.NewClient(t))

	gh := newFakeGitHub(t, owner, repo, "skills/any", commitSHA)
	srv.config.GitHubAppConfig.APIBaseURL = gh.URL
	srv.config.GitHubAppConfig.RawBaseURL = gh.URL

	// Three URIs in the same (owner, repo) with the same implicit ref (HEAD)
	// but different skill paths — the common case for a skill bundle.
	skills := []ResolveSkillRef{
		{URI: "gh://" + owner + "/" + repo + "/skill-a"},
		{URI: "gh://" + owner + "/" + repo + "/skill-b"},
		{URI: "gh://" + owner + "/" + repo + "/skill-c"},
	}
	n := len(skills) // derived so assertions stay in sync if the slice grows

	rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/skills/resolve",
		ResolveSkillsRequest{Skills: skills, ProjectID: project.ID})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp ResolveSkillsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Empty(t, resp.Errors, "all %d URIs must resolve successfully", n)
	require.Len(t, resp.Resolved, n, "all %d skills must be present in the response", n)

	// The key assertion: only 1 commits/{ref} call for N URIs sharing the same
	// (owner, repo, ref). Before the fix this would be N.
	assert.Equal(t, int64(1), gh.commitCalls.Load(),
		"ref→SHA resolution must be deduplicated: %d URIs, same repo+ref → 1 commit lookup (got %d)",
		n, gh.commitCalls.Load())

	// Sanity: each URI's contents lookup must still happen independently.
	contentsCalls := gh.calls.Load() - gh.commitCalls.Load()
	assert.Equal(t, int64(n), contentsCalls,
		"each URI must still trigger its own contents lookup: expected %d, got %d", n, contentsCalls)
}

// TestResolveGitHubSkill_ConcurrentMissesCoalesce is the acceptance test for
// hub-side single-flight: N concurrent resolutions of the same ref against a
// cold cache must make exactly one commit lookup and one contents lookup, not
// N of each. Synchronization is via channels, not sleeps: the commits handler
// blocks until every caller has had a chance to start, proving the flight
// genuinely coalesced concurrent callers rather than just serializing them.
func TestResolveGitHubSkill_ConcurrentMissesCoalesce(t *testing.T) {
	const (
		owner     = "acme"
		repo      = "coalesce-repo"
		skillPath = "skills/widget"
		uri       = "gh://" + owner + "/" + repo + "/widget@main"
		commitSHA = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	)

	srv, _, _, _, project := setupSkillAuthzTest(t)
	srv.ghResolutionStore = NewGitHubResolutionStore(enttest.NewClient(t))

	var commitCalls, contentsCalls atomic.Int64
	entered := make(chan struct{})
	var enterOnce sync.Once
	proceed := make(chan struct{})

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+owner+"/"+repo+"/commits/main", func(w http.ResponseWriter, _ *http.Request) {
		commitCalls.Add(1)
		enterOnce.Do(func() { close(entered) })
		<-proceed
		_, _ = w.Write([]byte(commitSHA))
	})
	mux.HandleFunc("/repos/"+owner+"/"+repo+"/contents/"+skillPath, func(w http.ResponseWriter, _ *http.Request) {
		contentsCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"name":"SKILL.md","path":"` + skillPath + `/SKILL.md","sha":"x","size":1,"type":"file"}]`))
	})
	gh := httptest.NewServer(mux)
	t.Cleanup(gh.Close)
	srv.config.GitHubAppConfig.APIBaseURL = gh.URL
	srv.config.GitHubAppConfig.RawBaseURL = gh.URL

	const n = 6
	var wg sync.WaitGroup
	errs := make([]error, n)
	resps := make([]*ResolvedSkillResponse, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resps[i], errs[i] = srv.resolveGitHubSkill(context.Background(), uri, project.ID, nil)
		}(i)
	}

	<-entered
	close(proceed)
	wg.Wait()

	for i, err := range errs {
		require.NoError(t, err, "caller %d", i)
		require.NotNil(t, resps[i], "caller %d", i)
	}
	assert.Equal(t, int64(1), commitCalls.Load(),
		"%d concurrent resolutions of the same ref must make exactly one commit lookup", n)
	assert.Equal(t, int64(1), contentsCalls.Load(),
		"%d concurrent resolutions of the same ref must make exactly one contents lookup", n)
}

// TestResolveGitHubSkill_CancelledLeaderDoesNotFailWaiters is the acceptance
// test for the hub-side single-flight leader needing its own detached,
// bounded context: the synchronous flight's leader has its own request
// context cancelled mid-flight. The leader itself must get context.Canceled
// promptly, but the flight must keep running — the other waiter must still
// succeed, and the cache write must still happen (not be skipped because the
// leader walked away).
func TestResolveGitHubSkill_CancelledLeaderDoesNotFailWaiters(t *testing.T) {
	const (
		owner     = "acme"
		repo      = "cancel-leader-repo"
		skillPath = "skills/widget"
		uri       = "gh://" + owner + "/" + repo + "/widget@main"
		commitSHA = "6666666666666666666666666666666666666666"
	)

	srv, _, _, _, project := setupSkillAuthzTest(t)
	srv.ghResolutionStore = NewGitHubResolutionStore(enttest.NewClient(t))

	entered := make(chan struct{})
	var enterOnce sync.Once
	proceed := make(chan struct{})

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+owner+"/"+repo+"/commits/main", func(w http.ResponseWriter, _ *http.Request) {
		enterOnce.Do(func() { close(entered) })
		<-proceed
		_, _ = w.Write([]byte(commitSHA))
	})
	mux.HandleFunc("/repos/"+owner+"/"+repo+"/contents/"+skillPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"name":"SKILL.md","path":"` + skillPath + `/SKILL.md","sha":"x","size":1,"type":"file"}]`))
	})
	gh := httptest.NewServer(mux)
	t.Cleanup(gh.Close)
	srv.config.GitHubAppConfig.APIBaseURL = gh.URL
	srv.config.GitHubAppConfig.RawBaseURL = gh.URL

	ctxLeader, cancelLeader := context.WithCancel(context.Background())
	ctxWaiter := context.Background()

	var respLeader, respWaiter *ResolvedSkillResponse
	var errLeader, errWaiter error
	doneLeader := make(chan struct{})
	doneWaiter := make(chan struct{})

	go func() {
		respLeader, errLeader = srv.resolveGitHubSkill(ctxLeader, uri, project.ID, nil)
		close(doneLeader)
	}()

	<-entered // the leader's fetch has started and is blocked on proceed

	go func() {
		respWaiter, errWaiter = srv.resolveGitHubSkill(ctxWaiter, uri, project.ID, nil)
		close(doneWaiter)
	}()

	cancelLeader()
	<-doneLeader // must return promptly: the flight is still blocked on proceed below
	if !errors.Is(errLeader, context.Canceled) {
		t.Fatalf("expected the cancelled leader to get context.Canceled, got %v", errLeader)
	}
	if respLeader != nil {
		t.Errorf("expected a nil response for the cancelled leader, got %+v", respLeader)
	}

	select {
	case <-doneWaiter:
		t.Fatal("the uncancelled waiter returned before the flight was released — it should still be blocked on proceed")
	default:
	}

	close(proceed) // let the still-running flight finish for the waiter
	<-doneWaiter

	require.NoError(t, errWaiter)
	require.NotNil(t, respWaiter)
	assert.Equal(t, safeShortSHA(commitSHA), respWaiter.ResolvedVersion)

	ghRef, err := agent.ParseGitHubSkillURI(uri)
	require.NoError(t, err)
	cacheKey := computeCacheKey(ghRef.Owner, ghRef.Repo, ghRef.SkillPath, ghRef.Ref, "public")
	_, hit, err := srv.ghResolutionStore.Get(context.Background(), cacheKey)
	require.NoError(t, err)
	assert.True(t, hit, "the flight's cache write must not be skipped because the leader was cancelled")
}

// TestResolveGitHubSkill_StaleServesImmediatelyAndRefreshesInBackground is the
// acceptance test for W on the hub cache: a branch-ref entry that is
// TTL-expired but within agent.MaxResolutionStaleAge must be served
// immediately from the stale value, with a background refresh that lands
// without the caller waiting on it.
func TestResolveGitHubSkill_StaleServesImmediatelyAndRefreshesInBackground(t *testing.T) {
	const (
		owner     = "acme"
		repo      = "stale-repo"
		skillPath = "skills/widget"
		uri       = "gh://" + owner + "/" + repo + "/widget@main"
		staleSHA  = "1111111111111111111111111111111111111111"
		freshSHA  = "2222222222222222222222222222222222222222"
	)

	srv, _, _, _, project := setupSkillAuthzTest(t)
	srv.ghResolutionStore = NewGitHubResolutionStore(enttest.NewClient(t))

	refreshed := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+owner+"/"+repo+"/commits/main", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(freshSHA))
	})
	mux.HandleFunc("/repos/"+owner+"/"+repo+"/contents/"+skillPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"name":"SKILL.md","path":"` + skillPath + `/SKILL.md","sha":"x","size":1,"type":"file"}]`))
		close(refreshed) // the contents call is the last GitHub call a refresh makes
	})
	gh := httptest.NewServer(mux)
	t.Cleanup(gh.Close)
	srv.config.GitHubAppConfig.APIBaseURL = gh.URL
	srv.config.GitHubAppConfig.RawBaseURL = gh.URL

	ghRef, err := agent.ParseGitHubSkillURI(uri)
	require.NoError(t, err)
	cacheKey := computeCacheKey(ghRef.Owner, ghRef.Repo, ghRef.SkillPath, ghRef.Ref, "public")

	ctx := context.Background()
	require.NoError(t, srv.ghResolutionStore.Put(ctx, cacheKey, GitHubCacheEntry{
		CommitSHA:   staleSHA,
		FileEntries: []GitHubFileEntry{{Path: "SKILL.md", URL: "http://example.invalid/SKILL.md", Hash: "x", Size: 1}},
		BundleHash:  "sha256:stale",
		TokenScope:  "public",
		ExpiresAt:   time.Now().Add(-time.Minute), // just past the branch-ref TTL
		OriginalURI: uri,
	}))

	resp, err := srv.resolveGitHubSkill(ctx, uri, project.ID, nil)
	require.NoError(t, err)
	assert.Equal(t, safeShortSHA(staleSHA), resp.ResolvedVersion,
		"must serve the stale value immediately, without waiting on a refresh")

	<-refreshed // the refresh's GitHub calls have completed, but Put may not have landed yet

	// Deterministically wait for the refresh's Put to land by joining its
	// flight: ghResolveFlight is keyed by cacheKey on the hub (unlike the
	// broker, which separates the flight key from the cache key), so a Do
	// call for the same key either joins the still-running refresh (and so
	// blocks until its Put completes) or, if it already finished, runs this
	// no-op immediately — either way, Put has landed once this returns.
	_, _, _ = srv.ghResolveFlight.Do(cacheKey, func() (interface{}, error) { return nil, nil })

	entry, hit, err := srv.ghResolutionStore.Get(ctx, cacheKey)
	require.NoError(t, err)
	require.True(t, hit)
	assert.Equal(t, freshSHA, entry.CommitSHA, "the background refresh must have updated the cache")
	assert.Equal(t, "public", entry.TokenScope,
		"a background refresh must not overwrite TokenScope with an empty value")
}

// TestResolveGitHubSkill_PastMaxStaleAgeResolvesSynchronously is the
// acceptance test for the hard staleness bound: an entry whose last
// successful resolution is older than agent.MaxResolutionStaleAge must not be
// served stale — it must be re-resolved synchronously instead.
func TestResolveGitHubSkill_PastMaxStaleAgeResolvesSynchronously(t *testing.T) {
	const (
		owner      = "acme"
		repo       = "ancient-repo"
		skillPath  = "skills/widget"
		uri        = "gh://" + owner + "/" + repo + "/widget@main"
		ancientSHA = "3333333333333333333333333333333333333333"
		freshSHA   = "4444444444444444444444444444444444444444"
	)

	srv, _, _, _, project := setupSkillAuthzTest(t)
	srv.ghResolutionStore = NewGitHubResolutionStore(enttest.NewClient(t))

	var calls atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+owner+"/"+repo+"/commits/main", func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(freshSHA))
	})
	mux.HandleFunc("/repos/"+owner+"/"+repo+"/contents/"+skillPath, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"name":"SKILL.md","path":"` + skillPath + `/SKILL.md","sha":"x","size":1,"type":"file"}]`))
	})
	gh := httptest.NewServer(mux)
	t.Cleanup(gh.Close)
	srv.config.GitHubAppConfig.APIBaseURL = gh.URL
	srv.config.GitHubAppConfig.RawBaseURL = gh.URL

	ghRef, err := agent.ParseGitHubSkillURI(uri)
	require.NoError(t, err)
	cacheKey := computeCacheKey(ghRef.Owner, ghRef.Repo, ghRef.SkillPath, ghRef.Ref, "public")

	ctx := context.Background()
	// ExpiresAt - DefaultResolutionCacheTTL is this entry's last successful
	// resolution time; push it well past MaxResolutionStaleAge.
	lastResolvedAt := time.Now().Add(-(agent.MaxResolutionStaleAge + time.Hour))
	require.NoError(t, srv.ghResolutionStore.Put(ctx, cacheKey, GitHubCacheEntry{
		CommitSHA:   ancientSHA,
		FileEntries: []GitHubFileEntry{{Path: "SKILL.md", URL: "http://example.invalid/SKILL.md", Hash: "x", Size: 1}},
		BundleHash:  "sha256:ancient",
		TokenScope:  "public",
		ExpiresAt:   lastResolvedAt.Add(agent.DefaultResolutionCacheTTL),
		OriginalURI: uri,
	}))

	resp, err := srv.resolveGitHubSkill(ctx, uri, project.ID, nil)
	require.NoError(t, err)
	assert.Equal(t, safeShortSHA(freshSHA), resp.ResolvedVersion,
		"an entry past MaxResolutionStaleAge must not be served stale")
	assert.Equal(t, int64(2), calls.Load(), "must resolve synchronously via exactly one commit + one contents call")
}

// generateTestGitHubAppKey generates a throwaway RSA private key in PEM
// format, suitable for configuring a fake GitHub App client in tests.
func generateTestGitHubAppKey(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	return string(pemBytes)
}

// TestSkillsResolve_GHCacheHitForCredentialedBranchRef is the acceptance test
// for the hub cache-hit path on a credentialed branch ref: a project with a
// GitHub App installation (so the Hub mints a real, non-"public" token scope)
// resolves a branch ref, then resolves it again — the second resolve must hit
// the cache and make no new commit or contents calls, even though the Hub
// still mints a fresh token on every call (that reordering is out of scope
// here; this test only pins the cache-hit behavior for a credentialed scope).
func TestSkillsResolve_GHCacheHitForCredentialedBranchRef(t *testing.T) {
	const (
		owner     = "acme"
		repo      = "installed-repo"
		skillPath = "skills/widget"
		uri       = "gh://" + owner + "/" + repo + "/widget@main"
		commitSHA = "5555555555555555555555555555555555555555"
	)
	instID := int64(424242)

	srv, s, alice, _, project := setupSkillAuthzTest(t)
	srv.ghResolutionStore = NewGitHubResolutionStore(enttest.NewClient(t))

	var apiCalls, mintCalls atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/app/installations/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/access_tokens") {
			http.NotFound(w, r)
			return
		}
		mintCalls.Add(1)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"token":      "ghs_test_token",
			"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
	})
	mux.HandleFunc("/repos/"+owner+"/"+repo+"/commits/main", func(w http.ResponseWriter, _ *http.Request) {
		apiCalls.Add(1)
		_, _ = w.Write([]byte(commitSHA))
	})
	mux.HandleFunc("/repos/"+owner+"/"+repo+"/contents/"+skillPath, func(w http.ResponseWriter, _ *http.Request) {
		apiCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"name":"SKILL.md","path":"` + skillPath + `/SKILL.md","sha":"x","size":1,"type":"file"}]`))
	})
	gh := httptest.NewServer(mux)
	t.Cleanup(gh.Close)

	srv.config.GitHubAppConfig.APIBaseURL = gh.URL
	srv.config.GitHubAppConfig.RawBaseURL = gh.URL
	srv.config.GitHubAppConfig.AppID = 1
	srv.config.GitHubAppConfig.PrivateKey = generateTestGitHubAppKey(t)

	ctx := context.Background()
	require.NoError(t, s.CreateGitHubInstallation(ctx, &store.GitHubInstallation{
		InstallationID: instID,
		AccountLogin:   owner,
		AccountType:    "Organization",
		AppID:          1,
		Status:         store.GitHubInstallationStatusActive,
	}))
	project.GitHubInstallationID = &instID
	require.NoError(t, s.UpdateProject(ctx, project))

	body := ResolveSkillsRequest{Skills: []ResolveSkillRef{{URI: uri}}, ProjectID: project.ID}

	rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/skills/resolve", body)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var resp ResolveSkillsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Empty(t, resp.Errors, "body: %s", rec.Body.String())
	require.Len(t, resp.Resolved, 1)
	require.Equal(t, int64(2), apiCalls.Load(), "first resolve is a cache miss: one commit + one contents call")
	require.Equal(t, int64(1), mintCalls.Load())

	rec2 := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/skills/resolve", body)
	require.Equal(t, http.StatusOK, rec2.Code)
	var resp2 ResolveSkillsResponse
	require.NoError(t, json.NewDecoder(rec2.Body).Decode(&resp2))
	require.Empty(t, resp2.Errors)
	require.Len(t, resp2.Resolved, 1)

	assert.Equal(t, int64(2), apiCalls.Load(),
		"second resolve of a credentialed branch ref must hit the cache: no new commit or contents calls")
	assert.Equal(t, resp.Resolved[0], resp2.Resolved[0], "a cache hit must reproduce the miss response byte for byte")
}
