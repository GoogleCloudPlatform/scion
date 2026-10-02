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
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

const ghCooldownTestSHA = "cccccccccccccccccccccccccccccccccccccccc"

type ghCooldownClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *ghCooldownClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *ghCooldownClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// ghCooldownFake serves the commits and contents endpoints for acme/skills,
// and answers every request for acme/limited with a 429.
type ghCooldownFake struct {
	*httptest.Server
	calls        atomic.Int64 // acme/skills requests
	limitedCalls atomic.Int64 // acme/limited requests
}

func newGHCooldownFake(t *testing.T) *ghCooldownFake {
	t.Helper()
	f := &ghCooldownFake{}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/skills/commits/", func(w http.ResponseWriter, _ *http.Request) {
		f.calls.Add(1)
		_, _ = w.Write([]byte(ghCooldownTestSHA))
	})
	mux.HandleFunc("/repos/acme/skills/contents/", func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"name":"SKILL.md","path":"` + r.URL.Path[len("/repos/acme/skills/contents/"):] +
			`/SKILL.md","sha":"x","size":1,"type":"file"}]`))
	})
	mux.HandleFunc("/repos/acme/limited/", func(w http.ResponseWriter, _ *http.Request) {
		f.limitedCalls.Add(1)
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func setupGHCooldownTest(t *testing.T) (*Server, *ghCooldownFake, *ghCooldownClock, func(t *testing.T, uris ...string) ResolveSkillsResponse, string) {
	t.Helper()
	srv, _, alice, _, project := setupSkillAuthzTest(t)
	srv.ghResolutionStore = NewGitHubResolutionStore(enttest.NewClient(t))
	clock := &ghCooldownClock{t: time.Now()}
	srv.ghCooldown = agent.NewGitHubCooldown(clock.Now)
	gh := newGHCooldownFake(t)
	srv.config.GitHubAppConfig.APIBaseURL = gh.URL
	srv.config.GitHubAppConfig.RawBaseURL = gh.URL

	resolve := func(t *testing.T, uris ...string) ResolveSkillsResponse {
		t.Helper()
		refs := make([]ResolveSkillRef, 0, len(uris))
		for _, u := range uris {
			refs = append(refs, ResolveSkillRef{URI: u})
		}
		rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/skills/resolve",
			ResolveSkillsRequest{Skills: refs, ProjectID: project.ID})
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
		var resp ResolveSkillsResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		return resp
	}
	return srv, gh, clock, resolve, project.ID
}

func putGHCooldownEntry(t *testing.T, srv *Server, uri string, expiresAt time.Time) string {
	t.Helper()
	ghRef, err := agent.ParseGitHubSkillURI(uri)
	require.NoError(t, err)
	cacheKey := computeCacheKey(ghRef.Owner, ghRef.Repo, ghRef.SkillPath, ghRef.Ref, "public")
	require.NoError(t, srv.ghResolutionStore.Put(context.Background(), cacheKey, GitHubCacheEntry{
		CommitSHA:   ghCooldownTestSHA,
		FileEntries: []GitHubFileEntry{{Path: "SKILL.md", URL: "http://example.invalid/SKILL.md", Hash: "x", Size: 1}},
		BundleHash:  "sha256:cached",
		TokenScope:  "public",
		ExpiresAt:   expiresAt,
		OriginalURI: uri,
	}))
	return cacheKey
}

// TestSkillsResolve_GHRateLimitedRefInBatch: a batch where one ref gets a
// 429. The limited ref reports rate_limited after one request; refs before
// it resolve; a cached ref after it is still served; an uncached ref after
// it (same identity) fails fast with no request. A second batch inside the
// cooldown sends nothing to GitHub for the limited identity.
func TestSkillsResolve_GHRateLimitedRefInBatch(t *testing.T) {
	srv, gh, _, resolve, _ := setupGHCooldownTest(t)

	const (
		first   = "gh://acme/skills/a@main"
		limited = "gh://acme/limited/b@main"
		cached  = "gh://acme/skills/c@main"
		later   = "gh://acme/skills/d@main"
	)
	putGHCooldownEntry(t, srv, cached, time.Now().Add(time.Hour))

	resp := resolve(t, first, limited, cached, later)
	gotResolved := map[string]bool{}
	for _, r := range resp.Resolved {
		gotResolved[r.URI] = true
	}
	assert.True(t, gotResolved[first], "a ref before the limited one must resolve")
	assert.True(t, gotResolved[cached], "a cached ref must be served during the cooldown")
	require.Len(t, resp.Errors, 2, "errors: %+v", resp.Errors)
	for _, e := range resp.Errors {
		assert.Equal(t, agent.GitHubRateLimitedCode, e.Code, "%s", e.URI)
		assert.Contains(t, e.Message, e.URI, "error must name the ref")
	}
	assert.Equal(t, int64(1), gh.limitedCalls.Load(), "the limited ref must be requested once, with no retry")
	assert.Equal(t, int64(2), gh.calls.Load(), "only the first ref's commit + contents may be requested")

	resp = resolve(t, limited, later)
	require.Len(t, resp.Errors, 2)
	assert.Equal(t, int64(1), gh.limitedCalls.Load(), "no request during the cooldown")
	assert.Equal(t, int64(2), gh.calls.Load(), "no request during the cooldown")
}

// TestResolveGitHubSkill_StaleServedWithoutRefreshDuringCooldown: a stale
// branch-ref entry is served during a cooldown, and no background refresh
// is started.
func TestResolveGitHubSkill_StaleServedWithoutRefreshDuringCooldown(t *testing.T) {
	srv, gh, _, _, projectID := setupGHCooldownTest(t)
	ctx := context.Background()
	const uri = "gh://acme/skills/s@main"
	cacheKey := putGHCooldownEntry(t, srv, uri, time.Now().Add(-time.Minute))

	// Start the cooldown for the (anonymous) identity via a 429.
	_, err := srv.resolveGitHubSkill(ctx, "gh://acme/limited/x@main", projectID, nil)
	var rl *agent.GitHubRateLimitError
	require.True(t, errors.As(err, &rl), "want a rate-limit error, got %v", err)

	var served, started atomic.Int64
	hook := func(key string, refreshStarted bool) {
		if key != cacheKey {
			return
		}
		served.Add(1)
		if refreshStarted {
			started.Add(1)
		}
	}
	ghStaleServeHook.Store(&hook)
	t.Cleanup(func() { ghStaleServeHook.Store(nil) })

	resp, err := srv.resolveGitHubSkill(ctx, uri, projectID, nil)
	require.NoError(t, err)
	assert.Equal(t, safeShortSHA(ghCooldownTestSHA), resp.ResolvedVersion, "the stale entry must be served")
	assert.Equal(t, int64(1), served.Load(), "expected one stale serve")
	assert.Equal(t, int64(0), started.Load(), "no background refresh may start during a cooldown")
	assert.Equal(t, int64(0), gh.calls.Load(), "no request during the cooldown")
}

// TestResolveGitHubSkill_MissDuringCooldownFailsFast: with no cached entry, a
// ref under an identity in a cooldown fails with a typed error and sends
// nothing; after T it resolves and the cooldown is cleared.
func TestResolveGitHubSkill_MissDuringCooldownFailsFast(t *testing.T) {
	srv, gh, clock, _, projectID := setupGHCooldownTest(t)
	ctx := context.Background()

	_, err := srv.resolveGitHubSkill(ctx, "gh://acme/limited/x@main", projectID, nil)
	var rl *agent.GitHubRateLimitError
	require.True(t, errors.As(err, &rl), "want a rate-limit error, got %v", err)
	require.Equal(t, int64(1), gh.limitedCalls.Load())

	const uri = "gh://acme/skills/s@main"
	_, err = srv.resolveGitHubSkill(ctx, uri, projectID, nil)
	require.True(t, errors.As(err, &rl), "want a rate-limit error, got %v", err)
	assert.False(t, rl.Sent)
	assert.Equal(t, uri, rl.Ref)
	assert.Equal(t, int64(0), gh.calls.Load(), "no request during the cooldown")

	clock.Advance(30 * time.Second)
	resp, err := srv.resolveGitHubSkill(ctx, uri, projectID, nil)
	require.NoError(t, err)
	assert.Equal(t, uri, resp.URI)
	assert.Equal(t, int64(2), gh.calls.Load())
	_, active := srv.ghCooldown.Active(agent.GitHubCooldownIdentityForInstallation("public"))
	assert.False(t, active, "the cooldown must be over after T")
}
