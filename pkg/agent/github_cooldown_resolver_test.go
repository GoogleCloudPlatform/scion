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

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// serveTestCommit registers the commits/main handler for owner/repo.
func serveTestCommit(mux *http.ServeMux, owner, repo string, calls *atomic.Int64) {
	mux.HandleFunc("/repos/"+owner+"/"+repo+"/commits/main", func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(testCommitSHA))
	})
}

// serveTestSkill registers contents and raw handlers for
// owner/repo/skills/name on mux, counting every request in calls. The
// commit handler is registered separately, once per repo.
func serveTestSkill(mux *http.ServeMux, owner, repo, name string, calls *atomic.Int64) {
	mux.HandleFunc("/repos/"+owner+"/"+repo+"/contents/skills/"+name, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode([]githubContentEntry{
			{Name: "SKILL.md", Path: "skills/" + name + "/SKILL.md", Type: "file", Size: 5},
		})
	})
	mux.HandleFunc("/raw/"+owner+"/"+repo+"/"+testCommitSHA+"/skills/"+name+"/SKILL.md", func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte("hello"))
	})
}

func newCooldownTestResolver(t *testing.T, mux *http.ServeMux, clock *fakeClock) *GitHubSkillResolver {
	t.Helper()
	server, srvMux := newTestGitHubServer(t)
	srvMux.Handle("/", mux)
	cache, err := NewGitHubResolutionCache(t.TempDir(), time.Hour)
	if err != nil {
		t.Fatalf("NewGitHubResolutionCache: %v", err)
	}
	r := newTestGitHubResolver(server)
	r.resolutionCache = cache
	r.cooldown = NewGitHubCooldown(clock.Now)
	return r
}

// TestGitHubSkillResolver_CooldownServesStaleWithoutRefresh: while the
// credential is in a cooldown, a stale branch-ref entry is served, no
// background refresh is started and no request reaches GitHub.
func TestGitHubSkillResolver_CooldownServesStaleWithoutRefresh(t *testing.T) {
	var calls atomic.Int64
	mux := http.NewServeMux()
	serveTestCommit(mux, "owner", "repo", &calls)
	serveTestSkill(mux, "owner", "repo", "s", &calls)
	clock := newFakeClock()
	r := newCooldownTestResolver(t, mux, clock)

	const uri = "gh://owner/repo/s@main"
	ghRef, err := ParseGitHubSkillURI(uri)
	if err != nil {
		t.Fatal(err)
	}
	cacheKey := resolutionCacheKey(ghRef, r.token)
	now := time.Now()
	r.resolutionCache.mu.Lock()
	r.resolutionCache.entries[cacheKey] = &resolutionCacheEntry{
		Skill:       ResolvedSkill{Name: "s", URI: uri, Version: "stale"},
		CachedAt:    now.Add(-2 * time.Hour),
		ExpiresAt:   now.Add(-time.Minute),
		IsBranchRef: true,
	}
	r.resolutionCache.mu.Unlock()

	r.cooldown.record(GitHubCooldownIdentity(r.token), clock.Now().Add(time.Minute))

	var served, started atomic.Int64
	hook := func(_ string, refreshStarted bool) {
		served.Add(1)
		if refreshStarted {
			started.Add(1)
		}
	}
	staleServeHook.Store(&hook)
	t.Cleanup(func() { staleServeHook.Store(nil) })

	res, err := r.Resolve(context.Background(), []api.SkillReference{{URI: uri}}, ResolveOpts{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(res.Errors) != 0 || len(res.Resolved) != 1 || res.Resolved[0].Version != "stale" {
		t.Fatalf("expected the stale entry, got %+v", res)
	}
	if served.Load() != 1 {
		t.Fatalf("expected one stale serve, got %d", served.Load())
	}
	if started.Load() != 0 {
		t.Error("no background refresh may start during a cooldown")
	}
	if calls.Load() != 0 {
		t.Errorf("expected no GitHub requests, got %d", calls.Load())
	}
}

// TestGitHubSkillResolver_CooldownMissFailsFast: with no cached entry, a ref
// whose credential is in a cooldown fails at once with a rate_limited error
// naming the ref, and nothing is sent.
func TestGitHubSkillResolver_CooldownMissFailsFast(t *testing.T) {
	var calls atomic.Int64
	mux := http.NewServeMux()
	serveTestCommit(mux, "owner", "repo", &calls)
	serveTestSkill(mux, "owner", "repo", "s", &calls)
	clock := newFakeClock()
	r := newCooldownTestResolver(t, mux, clock)
	r.cooldown.record(GitHubCooldownIdentity(r.token), clock.Now().Add(time.Minute))

	const uri = "gh://owner/repo/s@main"
	res, err := r.Resolve(context.Background(), []api.SkillReference{{URI: uri}}, ResolveOpts{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(res.Errors) != 1 || res.Errors[0].Code != GitHubRateLimitedCode {
		t.Fatalf("expected one rate_limited error, got %+v", res.Errors)
	}
	if !strings.Contains(res.Errors[0].Message, uri) {
		t.Errorf("error must name the ref, got %q", res.Errors[0].Message)
	}
	if strings.Contains(res.Errors[0].Message, r.token) || strings.Contains(res.Errors[0].Message, credentialFingerprint(r.token)) {
		t.Error("error must not carry credential-derived material")
	}
	if calls.Load() != 0 {
		t.Errorf("expected no GitHub requests, got %d", calls.Load())
	}

	// After T, the same ref is fetched normally and the cooldown is gone.
	clock.Advance(time.Minute)
	res, err = r.Resolve(context.Background(), []api.SkillReference{{URI: uri}}, ResolveOpts{})
	if err != nil || len(res.Errors) != 0 || len(res.Resolved) != 1 {
		t.Fatalf("expected success after the cooldown, got err=%v res=%+v", err, res)
	}
	if _, active := r.cooldown.Active(GitHubCooldownIdentity(r.token)); active {
		t.Error("cooldown must be over after T")
	}
}

// TestGitHubSkillResolver_ProvisionBatchWithRateLimitedRef models the
// provision-time batch: 19 refs resolved sequentially, one of which (a
// private repo under its own credential) gets a secondary rate limit with a
// 30s Retry-After. The batch must finish far inside a 30s create deadline:
// the limited ref fails fast with a typed error after one request, and the
// other refs resolve normally.
func TestGitHubSkillResolver_ProvisionBatchWithRateLimitedRef(t *testing.T) {
	var calls, limitedCalls atomic.Int64
	mux := http.NewServeMux()
	var refs []api.SkillReference
	serveTestCommit(mux, "public-org", "skills", &calls)
	for i := 0; i < 18; i++ {
		name := fmt.Sprintf("s%02d", i)
		serveTestSkill(mux, "public-org", "skills", name, &calls)
		refs = append(refs, api.SkillReference{URI: "gh://public-org/skills/" + name + "@main"})
	}
	mux.HandleFunc("/repos/private-org/private/commits/main", func(w http.ResponseWriter, _ *http.Request) {
		limitedCalls.Add(1)
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"You have exceeded a secondary rate limit."}`))
	})
	const limitedURI = "gh://private-org/private/p@main"
	// The limited ref sits in the middle of the batch.
	refs = append(refs[:9], append([]api.SkillReference{{URI: limitedURI}}, refs[9:]...)...)

	clock := newFakeClock()
	r := newCooldownTestResolver(t, mux, clock)
	r.provisionCredentials = map[string]string{"GH_PRIVATE_ORG": "private-org-credential"}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	res, err := r.Resolve(ctx, refs, ResolveOpts{ProjectID: "p"})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("batch took %v; a rate-limited ref must not stall the batch", elapsed)
	}
	if len(res.Resolved) != 18 {
		t.Errorf("expected the 18 other refs to resolve, got %d (errors %+v)", len(res.Resolved), res.Errors)
	}
	if len(res.Errors) != 1 || res.Errors[0].URI != limitedURI || res.Errors[0].Code != GitHubRateLimitedCode {
		t.Fatalf("expected one rate_limited error for %s, got %+v", limitedURI, res.Errors)
	}
	if !strings.Contains(res.Errors[0].Message, limitedURI) {
		t.Errorf("error must name the ref, got %q", res.Errors[0].Message)
	}
	if limitedCalls.Load() != 1 {
		t.Errorf("expected exactly one request for the limited ref, got %d", limitedCalls.Load())
	}

	// A second create inside the cooldown sends nothing for the limited ref
	// and serves the others from cache.
	before := calls.Load()
	res, err = r.Resolve(ctx, refs, ResolveOpts{ProjectID: "p"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(res.Resolved) != 18 || len(res.Errors) != 1 || res.Errors[0].Code != GitHubRateLimitedCode {
		t.Fatalf("second batch: unexpected result %+v", res)
	}
	if limitedCalls.Load() != 1 {
		t.Errorf("no request may be sent for the limited ref during the cooldown; got %d total", limitedCalls.Load())
	}
	if calls.Load() != before {
		t.Errorf("other refs must be served from cache; %d new requests", calls.Load()-before)
	}
}

// TestGitHubSkillResolver_SharedIdentityRateLimitFailsRestFast: when the
// rate-limited ref shares its credential with the rest of the batch, the
// refs after it that have no cached entry fail fast without requests.
func TestGitHubSkillResolver_SharedIdentityRateLimitFailsRestFast(t *testing.T) {
	var calls, limitedCalls atomic.Int64
	mux := http.NewServeMux()
	serveTestCommit(mux, "org", "skills", &calls)
	serveTestSkill(mux, "org", "skills", "a", &calls)
	serveTestSkill(mux, "org", "skills", "c", &calls)
	mux.HandleFunc("/repos/org/limited/commits/main", func(w http.ResponseWriter, _ *http.Request) {
		limitedCalls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	})
	clock := newFakeClock()
	r := newCooldownTestResolver(t, mux, clock)

	res, err := r.Resolve(context.Background(), []api.SkillReference{
		{URI: "gh://org/skills/a@main"},
		{URI: "gh://org/limited/b@main"},
		{URI: "gh://org/skills/c@main"},
	}, ResolveOpts{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(res.Resolved) != 1 || res.Resolved[0].URI != "gh://org/skills/a@main" {
		t.Errorf("expected only the first ref to resolve, got %+v", res.Resolved)
	}
	if len(res.Errors) != 2 {
		t.Fatalf("expected 2 errors, got %+v", res.Errors)
	}
	for _, e := range res.Errors {
		if e.Code != GitHubRateLimitedCode {
			t.Errorf("%s: code %q, want %q", e.URI, e.Code, GitHubRateLimitedCode)
		}
	}
	if limitedCalls.Load() != 1 {
		t.Errorf("expected one request for the limited ref, got %d", limitedCalls.Load())
	}
	// Only ref a's three requests went out: c was held back.
	if calls.Load() != 3 {
		t.Errorf("expected 3 requests for ref a only, got %d", calls.Load())
	}
}
