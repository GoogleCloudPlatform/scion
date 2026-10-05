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

// TestResolveGitHubSkill_FollowerGetsItsOwnURI is the permanent regression
// test for a flight follower receiving a response built from the leader's
// raw URI text. computeCacheKey normalises owner/repo case and ref (an
// omitted ref and "@HEAD" compute the same key), so two callers can share one
// flight while having asked with different raw spellings. The shared value
// behind that flight is a cache entry, not a response — each caller must
// build its own response from its own parsed ghRef after the flight
// resolves, or a caller gets back a URI it never requested and the broker's
// router drops the result as "not requested" (and spends a redundant
// resolution on its own fallback resolver).
//
// Covers both spelling differences named in the design: a bare ref vs an
// explicit "@HEAD", and owner letter case.
func TestResolveGitHubSkill_FollowerGetsItsOwnURI(t *testing.T) {
	const (
		owner     = "acme"
		repo      = "uri-repo"
		skillPath = "skills/widget"
		leaderURI = "gh://" + owner + "/" + repo + "/widget" // bare ref, lowercase owner
	)
	followerURIs := []string{
		"gh://" + owner + "/" + repo + "/widget@HEAD", // bare vs @HEAD
		"gh://Acme/" + repo + "/widget",               // owner letter case
	}

	srv, _, _, _, project := setupSkillAuthzTest(t)
	srv.ghResolutionStore = NewGitHubResolutionStore(enttest.NewClient(t))

	entered := make(chan struct{})
	var enterOnce sync.Once
	proceed := make(chan struct{})
	var proceedOnce sync.Once
	closeProceed := func() { proceedOnce.Do(func() { close(proceed) }) }

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+owner+"/"+repo+"/commits/HEAD", func(w http.ResponseWriter, r *http.Request) {
		enterOnce.Do(func() { close(entered) })
		select {
		case <-proceed:
			_, _ = w.Write([]byte("9999999999999999999999999999999999999999"))
		case <-r.Context().Done():
		}
	})
	mux.HandleFunc("/repos/"+owner+"/"+repo+"/contents/"+skillPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"name":"SKILL.md","path":"` + skillPath + `/SKILL.md","sha":"x","size":1,"type":"file"}]`))
	})
	gh := httptest.NewServer(mux)
	t.Cleanup(gh.Close)
	t.Cleanup(closeProceed)
	srv.config.GitHubAppConfig.APIBaseURL = gh.URL
	srv.config.GitHubAppConfig.RawBaseURL = gh.URL

	ghRef, err := agent.ParseGitHubSkillURI(leaderURI)
	require.NoError(t, err)
	if ghRef.Ref == "" {
		ghRef.Ref = "HEAD" // same default resolveGitHubSkill applies internally
	}
	cacheKey := computeCacheKey(ghRef.Owner, ghRef.Repo, ghRef.SkillPath, ghRef.Ref, "public")

	var joinCount int32
	allJoined := make(chan struct{})
	hook := func(key string) {
		if key != cacheKey {
			return
		}
		if atomic.AddInt32(&joinCount, 1) == int32(1+len(followerURIs)) {
			close(allJoined)
		}
	}
	ghFlightJoinHook.Store(&hook)
	t.Cleanup(func() { ghFlightJoinHook.Store(nil) })

	doneLeader := make(chan struct{})
	go func() {
		_, _ = srv.resolveGitHubSkill(context.Background(), leaderURI, project.ID, nil)
		close(doneLeader)
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("leader never started")
	}

	type followerResult struct {
		resp *ResolvedSkillResponse
		err  error
	}
	results := make([]followerResult, len(followerURIs))
	var wg sync.WaitGroup
	for i, fURI := range followerURIs {
		i, fURI := i, fURI
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := srv.resolveGitHubSkill(context.Background(), fURI, project.ID, nil)
			results[i] = followerResult{resp, err}
		}()
	}

	select {
	case <-allJoined:
	case <-time.After(5 * time.Second):
		t.Fatal("not every follower reached the leader's flight")
	}

	closeProceed()

	wgDone := make(chan struct{})
	go func() { wg.Wait(); close(wgDone) }()
	select {
	case <-wgDone:
	case <-time.After(5 * time.Second):
		t.Fatal("followers did not complete after the flight was released")
	}
	<-doneLeader

	for i, fURI := range followerURIs {
		require.NoError(t, results[i].err, "follower %d (%s)", i, fURI)
		require.NotNil(t, results[i].resp, "follower %d (%s)", i, fURI)
		assert.Equal(t, fURI, results[i].resp.URI,
			"follower %d must get back the URI it asked for, not the leader's raw text", i)
	}
}

// TestResolveGitHubSkill_CancelledRequestStartsNoNewFlights is the permanent
// regression test for the refSHAMemo data race: a per-request memo shared
// across every gh:// URI in one handleSkillsResolve call must never be
// touched by two flights at once. The race arose when a request's context
// ended while its first URI's flight was still running (detached), and
// handleSkillsResolve's loop moved on to the next URI on the same,
// now-cancelled context and memo, starting a *second* detached flight that
// wrote the same plain map concurrently with the first — in production, an
// unsynchronised concurrent map write is a fatal error recover() cannot
// catch, so this could crash the hub outright.
//
// Fixed two ways, both exercised here: resolveGitHubSkill now refuses to
// start a new flight once its own ctx is already done (so URI b below starts
// no flight and reaches GitHub zero times), and refSHAMemo (*ghSHAMemo) is
// mutex-guarded regardless, so it stays safe even at a future call site that
// lands on it without that guard. Run with -race.
func TestResolveGitHubSkill_CancelledRequestStartsNoNewFlights(t *testing.T) {
	const (
		owner = "acme"
		repo  = "memo-repo"
		sha   = "abababababababababababababababababababab"
	)

	srv, _, _, _, _ := setupSkillAuthzTest(t)
	srv.ghResolutionStore = NewGitHubResolutionStore(enttest.NewClient(t))

	var commitCalls atomic.Int64
	entered := make(chan struct{})
	var enterOnce sync.Once
	proceed := make(chan struct{})
	var proceedOnce sync.Once
	closeProceed := func() { proceedOnce.Do(func() { close(proceed) }) }

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+owner+"/"+repo+"/commits/main", func(w http.ResponseWriter, r *http.Request) {
		commitCalls.Add(1)
		enterOnce.Do(func() { close(entered) })
		select {
		case <-proceed:
			_, _ = w.Write([]byte(sha))
		case <-r.Context().Done():
		}
	})
	for _, p := range []string{"skills/a", "skills/b"} {
		p := p
		mux.HandleFunc("/repos/"+owner+"/"+repo+"/contents/"+p, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"name":"SKILL.md","path":"` + p + `/SKILL.md","sha":"x","size":1,"type":"file"}]`))
		})
	}
	gh := httptest.NewServer(mux)
	t.Cleanup(gh.Close)
	// Registered after gh's own Close, so this runs first (cleanups run in
	// reverse order): see the identical comment on the cancelled-leader test
	// above for why that order matters.
	t.Cleanup(closeProceed)
	srv.config.GitHubAppConfig.APIBaseURL = gh.URL
	srv.config.GitHubAppConfig.RawBaseURL = gh.URL

	memo := newGHSHAMemo() // one request's memo, as in handleSkillsResolve
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// touched records every cacheKey injectGHFlightJoin fires for. That call
	// happens synchronously in the caller's own goroutine, before DoChan is
	// even invoked (see resolveGitHubSkill) — so by the time a
	// resolveGitHubSkill call returns, any flight it started has already been
	// recorded here, with no timing window to race: unlike asserting on
	// commitCalls (whose increment happens inside a different goroutine's
	// HTTP handler, at some later point), this needs no join or wait at all.
	var touchedMu sync.Mutex
	touched := make(map[string]bool)
	hook := func(key string) {
		touchedMu.Lock()
		touched[key] = true
		touchedMu.Unlock()
	}
	ghFlightJoinHook.Store(&hook)
	t.Cleanup(func() { ghFlightJoinHook.Store(nil) })

	doneA := make(chan error, 1)
	go func() {
		_, err := srv.resolveGitHubSkill(ctx, "gh://"+owner+"/"+repo+"/a@main", "", memo)
		doneA <- err
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("URI a's fetch never started")
	}

	cancel()

	select {
	case err := <-doneA:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled for URI a, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the cancelled request for URI a did not return promptly")
	}

	// The handler loop (handleSkillsResolve) continues to the next URI on the
	// same, now-cancelled ctx and the same memo — this must start no new
	// flight at all, and so must never reach GitHub.
	_, errB := srv.resolveGitHubSkill(ctx, "gh://"+owner+"/"+repo+"/b@main", "", memo)
	if !errors.Is(errB, context.Canceled) {
		t.Fatalf("expected a request for URI b on an already-cancelled context to return context.Canceled without starting a flight, got %v", errB)
	}

	// errB returning context.Canceled only proves this caller did not wait
	// for a flight's result — the leader's own ctx is checked by a select
	// that races the flight's resultCh, after injectGHFlightJoin/DoChan have
	// already (synchronously, in this same call) launched it. touched is the
	// actual proof that no flight was started for URI b at all.
	ghRefB, err := agent.ParseGitHubSkillURI("gh://" + owner + "/" + repo + "/b@main")
	require.NoError(t, err)
	cacheKeyB := computeCacheKey(ghRefB.Owner, ghRefB.Repo, ghRefB.SkillPath, ghRefB.Ref, "public")
	touchedMu.Lock()
	gotB := touched[cacheKeyB]
	touchedMu.Unlock()
	if gotB {
		t.Fatal("URI b must never start a flight once its request context is already done")
	}

	closeProceed()
	// Join URI a's (still-running, detached) flight so it fully completes
	// before the httptest server's cleanup tries to close it.
	ghRefA, err := agent.ParseGitHubSkillURI("gh://" + owner + "/" + repo + "/a@main")
	require.NoError(t, err)
	cacheKeyA := computeCacheKey(ghRefA.Owner, ghRefA.Repo, ghRefA.SkillPath, ghRefA.Ref, "public")
	_, _, _ = srv.ghResolveFlight.Do(cacheKeyA, func() (interface{}, error) { return nil, nil })

	assert.Equal(t, int64(1), commitCalls.Load(),
		"URI b must never reach GitHub once its request context is already done")
}

// TestGHSHAMemo_ConcurrentAccessIsRaceFree drives a single *ghSHAMemo from
// many goroutines directly, independently of resolveGitHubSkill's ctx guard
// (see TestResolveGitHubSkill_CancelledRequestStartsNoNewFlights above): that
// guard is what currently keeps a second flight from ever starting on a dead
// request, but nothing before this test exercised the memo's own mutex on
// its own. Several goroutines share each of a handful of keys, so get and
// set on the same key happen concurrently; without the mutex this is a
// textbook concurrent map access. Run with -race.
func TestGHSHAMemo_ConcurrentAccessIsRaceFree(t *testing.T) {
	memo := newGHSHAMemo()
	keys := []string{
		"acme/repo-a@main:public",
		"acme/repo-b@main:public",
		"acme/repo-c@main:public",
		"acme/repo-d@main:public",
	}
	const goroutinesPerKey = 5

	var wg sync.WaitGroup
	for _, key := range keys {
		key := key
		for i := 0; i < goroutinesPerKey; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				memo.set(key, "sha-value")
				_, _ = memo.get(key)
			}()
		}
	}
	wg.Wait()
}

// TestResolveGitHubSkill_CancelledLeaderDoesNotFailWaiters is the acceptance
// test for the hub-side single-flight leader needing its own detached,
// bounded context: the synchronous flight's leader has its own request
// context cancelled mid-flight. The leader itself must get context.Canceled
// promptly, but the flight must keep running — the other waiter must still
// succeed, and the cache write must still happen (not be skipped because the
// leader walked away).
//
// It uses ghFlightJoinHook to know, deterministically and without sleeping or
// polling, that the waiter has actually reached the point of joining the
// leader's still-in-flight call before the leader is cancelled — otherwise a
// race (the leader's flight already failing and being removed before the
// waiter calls DoChan) could let the waiter start a fresh flight of its own
// and still pass, without the test ever having exercised the "does not fail
// the flight" property it claims to. The commits-endpoint call count is
// asserted to be exactly 1 for the same reason: under the mutation this test
// targets (the flight running on the leader's own ctx instead of a detached
// one), the waiter either fails too or ends up making its own second commit
// call.
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

	var commitCalls atomic.Int64
	entered := make(chan struct{})
	var enterOnce sync.Once
	proceed := make(chan struct{})
	var proceedOnce sync.Once
	closeProceed := func() { proceedOnce.Do(func() { close(proceed) }) }

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+owner+"/"+repo+"/commits/main", func(w http.ResponseWriter, r *http.Request) {
		commitCalls.Add(1)
		enterOnce.Do(func() { close(entered) })
		select {
		case <-proceed:
			_, _ = w.Write([]byte(commitSHA))
		case <-r.Context().Done():
		}
	})
	mux.HandleFunc("/repos/"+owner+"/"+repo+"/contents/"+skillPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"name":"SKILL.md","path":"` + skillPath + `/SKILL.md","sha":"x","size":1,"type":"file"}]`))
	})
	gh := httptest.NewServer(mux)
	t.Cleanup(gh.Close)
	// Registered after gh's own Close, so this runs first (cleanups run in
	// reverse order): releasing any blocked handler before the server tries
	// to close keeps a regression (the waiter starting its own flight on a
	// context that never cancels) from turning Close() into a hang instead
	// of a clean test failure.
	t.Cleanup(closeProceed)
	srv.config.GitHubAppConfig.APIBaseURL = gh.URL
	srv.config.GitHubAppConfig.RawBaseURL = gh.URL

	ghRef, err := agent.ParseGitHubSkillURI(uri)
	require.NoError(t, err)
	cacheKey := computeCacheKey(ghRef.Owner, ghRef.Repo, ghRef.SkillPath, ghRef.Ref, "public")

	var joinCount int32
	waiterJoined := make(chan struct{})
	hook := func(key string) {
		if key != cacheKey {
			return
		}
		if atomic.AddInt32(&joinCount, 1) == 2 {
			close(waiterJoined)
		}
	}
	ghFlightJoinHook.Store(&hook)
	t.Cleanup(func() { ghFlightJoinHook.Store(nil) })

	ctxLeader, cancelLeader := context.WithCancel(context.Background())
	defer cancelLeader()
	ctxWaiter := context.Background()

	var respLeader, respWaiter *ResolvedSkillResponse
	var errLeader, errWaiter error
	doneLeader := make(chan struct{})
	go func() {
		respLeader, errLeader = srv.resolveGitHubSkill(ctxLeader, uri, project.ID, nil)
		close(doneLeader)
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("leader's fetch never started")
	}

	doneWaiter := make(chan struct{})
	go func() {
		respWaiter, errWaiter = srv.resolveGitHubSkill(ctxWaiter, uri, project.ID, nil)
		close(doneWaiter)
	}()

	select {
	case <-waiterJoined:
	case <-time.After(5 * time.Second):
		t.Fatal("waiter never reached the flight join point")
	}

	cancelLeader()

	select {
	case <-doneLeader:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled leader did not return promptly")
	}
	if !errors.Is(errLeader, context.Canceled) {
		t.Fatalf("expected the cancelled leader to get context.Canceled, got %v", errLeader)
	}
	if respLeader != nil {
		t.Errorf("expected a nil response for the cancelled leader, got %+v", respLeader)
	}

	select {
	case <-doneWaiter:
		t.Fatal("the waiter returned before the flight was released — it should still be blocked on proceed")
	default:
	}

	closeProceed() // let the still-running flight finish for the waiter

	select {
	case <-doneWaiter:
	case <-time.After(5 * time.Second):
		t.Fatal("waiter did not complete after the flight was released")
	}

	require.NoError(t, errWaiter)
	require.NotNil(t, respWaiter)
	assert.Equal(t, safeShortSHA(commitSHA), respWaiter.ResolvedVersion)
	assert.Equal(t, int64(1), commitCalls.Load(),
		"the waiter must join the leader's flight, not make its own commit call")

	_, hit, err := srv.ghResolutionStore.Get(context.Background(), cacheKey)
	require.NoError(t, err)
	assert.True(t, hit, "the flight's cache write must not be skipped because the leader was cancelled")
}

// TestResolveGitHubSkill_ShortDeadlineLeaderDoesNotFailWaiter is the
// acceptance test for bounding the hub's flight by the fixed ceiling only: a
// leader with a short deadline must not fail a waiter that has none, once
// that deadline passes. The commits handler honors the request's own
// context — the flight's detached one, not any one caller's — and only
// stops early if *that* is cancelled, which a correct bound never does from
// the leader's deadline alone.
func TestResolveGitHubSkill_ShortDeadlineLeaderDoesNotFailWaiter(t *testing.T) {
	const (
		owner     = "acme"
		repo      = "short-deadline-repo"
		skillPath = "skills/widget"
		uri       = "gh://" + owner + "/" + repo + "/widget@main"
		commitSHA = "8888888888888888888888888888888888888888"
	)

	srv, _, _, _, project := setupSkillAuthzTest(t)
	srv.ghResolutionStore = NewGitHubResolutionStore(enttest.NewClient(t))

	entered := make(chan struct{})
	var enterOnce sync.Once
	proceed := make(chan struct{})
	var proceedOnce sync.Once
	closeProceed := func() { proceedOnce.Do(func() { close(proceed) }) }
	// reqCtx captures the request context the handler actually runs under —
	// the flight's own, shared by leader and waiter alike — the first (and
	// only; the flight coalesces both) time the handler runs, so it can be
	// inspected deterministically below instead of racing a timer against
	// whether a wrongly-applied leader deadline fires.
	var reqCtx context.Context
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+owner+"/"+repo+"/commits/main", func(w http.ResponseWriter, r *http.Request) {
		enterOnce.Do(func() {
			reqCtx = r.Context()
			close(entered)
		})
		select {
		case <-proceed:
			_, _ = w.Write([]byte(commitSHA))
		case <-r.Context().Done():
		}
	})
	mux.HandleFunc("/repos/"+owner+"/"+repo+"/contents/"+skillPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"name":"SKILL.md","path":"` + skillPath + `/SKILL.md","sha":"x","size":1,"type":"file"}]`))
	})
	gh := httptest.NewServer(mux)
	t.Cleanup(gh.Close)
	t.Cleanup(closeProceed)
	srv.config.GitHubAppConfig.APIBaseURL = gh.URL
	srv.config.GitHubAppConfig.RawBaseURL = gh.URL

	ghRef, err := agent.ParseGitHubSkillURI(uri)
	require.NoError(t, err)
	cacheKey := computeCacheKey(ghRef.Owner, ghRef.Repo, ghRef.SkillPath, ghRef.Ref, "public")

	var joinCount int32
	waiterJoined := make(chan struct{})
	hook := func(key string) {
		if key != cacheKey {
			return
		}
		if atomic.AddInt32(&joinCount, 1) == 2 {
			close(waiterJoined)
		}
	}
	ghFlightJoinHook.Store(&hook)
	t.Cleanup(func() { ghFlightJoinHook.Store(nil) })

	leaderCtx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	go func() { _, _ = srv.resolveGitHubSkill(leaderCtx, uri, project.ID, nil) }()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("leader's fetch never started")
	}

	var werr error
	done := make(chan struct{})
	go func() {
		_, werr = srv.resolveGitHubSkill(context.Background(), uri, project.ID, nil)
		close(done)
	}()

	select {
	case <-waiterJoined:
	case <-time.After(5 * time.Second):
		t.Fatal("waiter never joined")
	}

	<-leaderCtx.Done() // let the leader's own deadline pass

	// Deterministic check, no fixed wait: the request context the handler is
	// actually running under must still be alive the moment the leader's own
	// deadline has passed — a flight wrongly tied to that deadline would have
	// already cancelled it by now (net/http propagates request-context
	// cancellation to the server's r.Context() by closing the underlying
	// connection, not through any Deadline() visible server-side, so this
	// checks liveness directly rather than comparing deadlines as the
	// broker-side version of this test does).
	if err := reqCtx.Err(); err != nil {
		t.Fatalf("request context ended when the leader's own deadline passed: %v", err)
	}

	closeProceed()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("waiter hung")
	}

	if werr != nil {
		t.Fatalf("hub waiter with no deadline failed after the leader's deadline passed: %v", werr)
	}
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

// TestResolveGitHubSkill_SHARefNeverServedStale is the acceptance test for
// excluding commit-SHA refs from stale-serve on the hub: a SHA ref is
// immutable once resolved, so an expired row must always trigger a
// synchronous re-resolution, never the stale-serve path that branch refs
// use — even when the row is well within agent.MaxResolutionStaleAge.
func TestResolveGitHubSkill_SHARefNeverServedStale(t *testing.T) {
	const (
		owner     = "acme"
		repo      = "sha-repo"
		skillPath = "skills/widget"
		sha       = "5555555555555555555555555555555555555555"
		uri       = "gh://" + owner + "/" + repo + "/widget@" + sha
	)

	srv, _, _, _, project := setupSkillAuthzTest(t)
	srv.ghResolutionStore = NewGitHubResolutionStore(enttest.NewClient(t))

	var contentsCalls atomic.Int64
	mux := http.NewServeMux()
	// No commits handler is registered: a full-SHA ref is already resolved
	// and must never trigger a commits/{ref} call (see isFullCommitSHA).
	mux.HandleFunc("/repos/"+owner+"/"+repo+"/contents/"+skillPath, func(w http.ResponseWriter, _ *http.Request) {
		contentsCalls.Add(1)
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
	require.NoError(t, srv.ghResolutionStore.Put(ctx, cacheKey, GitHubCacheEntry{
		CommitSHA:   sha,
		FileEntries: []GitHubFileEntry{{Path: "SKILL.md", URL: "http://example.invalid/SKILL.md", Hash: "old", Size: 1}},
		BundleHash:  "sha256:old",
		TokenScope:  "public",
		// Expired, but well within MaxResolutionStaleAge — if this were a
		// branch ref, it would be served stale. A SHA ref must not be.
		ExpiresAt:   time.Now().Add(-time.Minute),
		OriginalURI: uri,
	}))

	resp, err := srv.resolveGitHubSkill(ctx, uri, project.ID, nil)
	require.NoError(t, err)
	assert.Equal(t, safeShortSHA(sha), resp.ResolvedVersion)
	assert.NotEqual(t, "sha256:old", resp.ContentHash,
		"must not serve the stale cached entry — the content hash should reflect a fresh resolution")
	assert.Equal(t, int64(1), contentsCalls.Load(),
		"an expired SHA-ref entry must trigger exactly one synchronous re-resolution, never a stale-serve")
}

// TestResolveGitHubSkill_RefreshFailureBackoffSkipsRetry is the acceptance
// test for the refresh-failure backoff on the hub: once a background refresh
// has failed recently for a cache key, a later stale hit for that key must
// not start another one — it must keep serving the stale value, with zero
// additional GitHub calls, until the backoff window passes. The failure is
// primed directly via recordGHRefreshFailure, exactly as
// refreshGitHubSkillInBackground would have left it after a real failure.
//
// The backoff decision is made synchronously inside resolveGitHubSkill,
// before it returns, but a wrongly launched refresh runs in its own
// goroutine; ghFlightJoinHook fires as refreshGitHubSkillInBackground's first
// statement, before any GitHub call, so the bound below only has to cover
// that goroutine getting scheduled, not completing any work.
func TestResolveGitHubSkill_RefreshFailureBackoffSkipsRetry(t *testing.T) {
	const (
		owner     = "acme"
		repo      = "backoff-repo"
		skillPath = "skills/widget"
		uri       = "gh://" + owner + "/" + repo + "/widget@main"
		staleSHA  = "6666666666666666666666666666666666666666"
	)

	srv, _, _, _, project := setupSkillAuthzTest(t)
	srv.ghResolutionStore = NewGitHubResolutionStore(enttest.NewClient(t))

	var calls atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+owner+"/"+repo+"/commits/main", func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	})
	mux.HandleFunc("/repos/"+owner+"/"+repo+"/contents/"+skillPath, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
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
		ExpiresAt:   time.Now().Add(-time.Minute),
		OriginalURI: uri,
	}))

	srv.recordGHRefreshFailure(cacheKey)

	flightStarted := make(chan struct{})
	var startedOnce sync.Once
	hook := func(key string) {
		if key == cacheKey {
			startedOnce.Do(func() { close(flightStarted) })
		}
	}
	ghFlightJoinHook.Store(&hook)
	t.Cleanup(func() { ghFlightJoinHook.Store(nil) })

	resp, err := srv.resolveGitHubSkill(ctx, uri, project.ID, nil)
	require.NoError(t, err)
	assert.Equal(t, safeShortSHA(staleSHA), resp.ResolvedVersion,
		"must keep serving the stale value while backed off")

	select {
	case <-flightStarted:
		t.Fatal("a background refresh was started while within the refresh-failure backoff window")
	case <-time.After(200 * time.Millisecond):
	}

	assert.Equal(t, int64(0), calls.Load(),
		"must not retry a refresh while within the refresh-failure backoff window")
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

// TestSkillsResolve_GHCacheEntryExpiryIsJittered reads back the ExpiresAt of
// entries the resolve handler stored and checks the TTL was jittered on
// write: every entry expires within the jitter band around the nominal TTL,
// and at least one is visibly off the nominal TTL. Entries written together
// then do not all expire at the same instant.
func TestSkillsResolve_GHCacheEntryExpiryIsJittered(t *testing.T) {
	const (
		owner     = "acme"
		repo      = "private-repo"
		commitSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	)

	srv, _, alice, _, project := setupSkillAuthzTest(t)
	client := enttest.NewClient(t)
	srv.ghResolutionStore = NewGitHubResolutionStore(client)

	gh := newFakeGitHub(t, owner, repo, "skills/any", commitSHA)
	srv.config.GitHubAppConfig.APIBaseURL = gh.URL
	srv.config.GitHubAppConfig.RawBaseURL = gh.URL

	// The refs name a branch, so entries use the branch TTL.
	ttl := agent.DefaultResolutionCacheTTL
	band := time.Duration(float64(ttl) * 0.10)
	// The store may round times; allow a little slack on the band edges.
	const slack = time.Second

	type window struct{ before, after time.Time }
	windows := map[string]window{}
	for _, name := range []string{"one", "two", "three", "four", "five"} {
		uri := "gh://" + owner + "/" + repo + "/" + name + "@main"
		before := time.Now()
		rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/skills/resolve",
			ResolveSkillsRequest{Skills: []ResolveSkillRef{{URI: uri}}, ProjectID: project.ID})
		after := time.Now()
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
		var resp ResolveSkillsResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		require.Empty(t, resp.Errors)
		windows[uri] = window{before, after}
	}

	rows, err := client.GitHubResolutionCache.Query().All(context.Background())
	require.NoError(t, err)
	require.Len(t, rows, len(windows))

	jittered := 0
	for _, row := range rows {
		w, ok := windows[row.OriginalURI]
		require.True(t, ok, "unexpected cache row for %s", row.OriginalURI)
		lo := w.before.Add(ttl - band - slack)
		hi := w.after.Add(ttl + band + slack)
		assert.False(t, row.ExpiresAt.Before(lo) || row.ExpiresAt.After(hi),
			"%s: expires_at %v outside the jitter band [%v, %v]", row.OriginalURI, row.ExpiresAt, lo, hi)
		// Outside the window an unjittered TTL would land in.
		if row.ExpiresAt.Before(w.before.Add(ttl-slack)) || row.ExpiresAt.After(w.after.Add(ttl+slack)) {
			jittered++
		}
	}
	assert.Positive(t, jittered,
		"no stored entry is off the nominal TTL; the TTL does not look jittered on write")
}
