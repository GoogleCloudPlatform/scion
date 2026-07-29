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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
