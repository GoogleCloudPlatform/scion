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
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	cache, err := newTestResolutionCache(t.TempDir(), time.Hour)
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
// refs started after it that have no cached entry fail fast without
// requests. Refs are resolved one at a time here so that ref c starts only
// after ref b's rate limit; the parallel case is covered by
// TestGitHubSkillResolver_SharedIdentityRateLimitHoldsBackParallelRefs.
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
	r.maxConcurrent = 1

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

// TestGitHubSkillResolver_SharedIdentityRateLimitHoldsBackParallelRefs:
// with refs resolved in parallel, a rate limit on one ref still holds back
// the rest of the batch that shares its credential. A ref already in flight
// when the cooldown starts sends no further request, and refs started
// after it send none at all.
func TestGitHubSkillResolver_SharedIdentityRateLimitHoldsBackParallelRefs(t *testing.T) {
	var calls, limitedCalls atomic.Int64
	mux := http.NewServeMux()
	clock := newFakeClock()
	r := newCooldownTestResolver(t, mux, clock)
	r.maxConcurrent = 2
	identity := GitHubCooldownIdentity(r.token)

	// Ref b's rate limit is answered only once ref c's commit lookup has
	// arrived, and c's lookup is answered only once that rate limit has
	// started the cooldown, so c is in flight across that moment.
	cArrived := make(chan struct{})
	var cArrivedOnce sync.Once
	mux.HandleFunc("/repos/org/skills/commits/main", func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		cArrivedOnce.Do(func() { close(cArrived) })
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if _, active := r.cooldownTracker().Active(identity); active {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		_, _ = w.Write([]byte(testCommitSHA))
	})
	for _, name := range []string{"c", "d", "e"} {
		serveTestSkill(mux, "org", "skills", name, &calls)
	}
	mux.HandleFunc("/repos/org/limited/commits/main", func(w http.ResponseWriter, _ *http.Request) {
		limitedCalls.Add(1)
		select {
		case <-cArrived:
		case <-time.After(5 * time.Second):
		}
		w.WriteHeader(http.StatusTooManyRequests)
	})

	res, err := r.Resolve(context.Background(), []api.SkillReference{
		{URI: "gh://org/limited/b@main"},
		{URI: "gh://org/skills/c@main"},
		{URI: "gh://org/skills/d@main"},
		{URI: "gh://org/skills/e@main"},
	}, ResolveOpts{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(res.Resolved) != 0 {
		t.Errorf("expected no ref to resolve, got %+v", res.Resolved)
	}
	if len(res.Errors) != 4 {
		t.Fatalf("expected 4 errors, got %+v", res.Errors)
	}
	for _, e := range res.Errors {
		if e.Code != GitHubRateLimitedCode {
			t.Errorf("%s: code %q, want %q", e.URI, e.Code, GitHubRateLimitedCode)
		}
	}
	if limitedCalls.Load() != 1 {
		t.Errorf("expected one request for the limited ref, got %d", limitedCalls.Load())
	}
	// Only ref c's commit lookup went out.
	if calls.Load() != 1 {
		t.Errorf("expected 1 request (ref c's commit lookup), got %d", calls.Load())
	}
}

// TestGitHubSkillResolver_CooldownWritesNothingToDisk: during a cooldown,
// neither a stale entry served from a reloaded cache file nor a
// rate_limited error is stored, so the cache file is left exactly as it
// was. Three refs share the cooldown credential: one with an acceptable
// stale entry (served), one whose stale entry has no file content (not
// usable without the install credential, so it is resolved again and fails
// fast), and one with no entry (fails fast).
func TestGitHubSkillResolver_CooldownWritesNothingToDisk(t *testing.T) {
	var calls atomic.Int64
	mux := http.NewServeMux()
	serveTestCommit(mux, "owner", "repo", &calls)
	for _, name := range []string{"stale", "nocontent", "miss"} {
		serveTestSkill(mux, "owner", "repo", name, &calls)
	}
	clock := newFakeClock()
	r := newCooldownTestResolver(t, mux, clock)

	keyFor := func(uri string) string {
		ref, err := ParseGitHubSkillURI(uri)
		if err != nil {
			t.Fatal(err)
		}
		return resolutionCacheKey(ref, r.token)
	}
	const (
		staleURI     = "gh://owner/repo/stale@main"
		noContentURI = "gh://owner/repo/nocontent@main"
		missURI      = "gh://owner/repo/miss@main"
	)
	now := time.Now()
	dir := t.TempDir()
	original := writeCacheFile(t, dir, map[string]*resolutionCacheEntry{
		keyFor(staleURI): {
			Skill:    ResolvedSkill{Name: "stale", URI: staleURI, Version: "stale"},
			CachedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour), IsBranchRef: true,
		},
		keyFor(noContentURI): {
			Skill: ResolvedSkill{Name: "nocontent", URI: noContentURI, Version: "stale",
				Files: []ResolvedFile{{Path: "SKILL.md"}}},
			CachedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour), IsBranchRef: true,
		},
	})
	cache, err := newTestResolutionCache(dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	r.resolutionCache = cache
	r.cooldown.record(GitHubCooldownIdentity(r.token), clock.Now().Add(time.Minute))

	res, err := r.Resolve(context.Background(), []api.SkillReference{
		{URI: staleURI}, {URI: noContentURI}, {URI: missURI},
	}, ResolveOpts{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(res.Resolved) != 1 || res.Resolved[0].Version != "stale" {
		t.Fatalf("expected only the stale entry to resolve, got %+v", res.Resolved)
	}
	if len(res.Errors) != 2 {
		t.Fatalf("expected two errors, got %+v", res.Errors)
	}
	for _, e := range res.Errors {
		if e.Code != GitHubRateLimitedCode {
			t.Errorf("expected %s, got %+v", GitHubRateLimitedCode, e)
		}
	}
	if calls.Load() != 0 {
		t.Errorf("expected no GitHub requests, got %d", calls.Load())
	}

	cache.Flush()
	if n := cache.saveCount.Load(); n != 0 {
		t.Errorf("expected no cache file rewrite during a cooldown, got %d", n)
	}
	got, err := os.ReadFile(filepath.Join(dir, resolutionCacheFileName))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Error("cache file changed during a cooldown")
	}
	if _, ok := cache.Get(keyFor(missURI)); ok {
		t.Error("a rate_limited error must not be stored as a resolution")
	}
}

// TestGitHubResolveError_UnwrapReachesRateLimitError: a rate-limit error
// wrapped in a githubResolveError is still found by errors.As and errors.Is,
// withRateLimitRef still names the ref, and Resolve's classification would
// report it as rate_limited.
func TestGitHubResolveError_UnwrapReachesRateLimitError(t *testing.T) {
	rl := &GitHubRateLimitError{RetryAt: time.Now().Add(time.Minute), Sent: true}
	wrapped := fmt.Errorf("failed to resolve ref: %w",
		&githubResolveError{code: SkillErrCodeUpstreamUnavailable, msg: "outer", err: rl})

	var got *GitHubRateLimitError
	if !errors.As(wrapped, &got) || got != rl {
		t.Fatalf("errors.As must reach the rate-limit error, got %v", got)
	}
	if !errors.Is(wrapped, rl) {
		t.Error("errors.Is must reach the rate-limit error")
	}
	named := withRateLimitRef(wrapped, "gh://o/r/s@main")
	if !strings.Contains(named.Error(), "gh://o/r/s@main") {
		t.Errorf("expected the ref in %q", named.Error())
	}
}

// TestGitHubSkillResolver_CooldownDuringBackoffFailsWithoutSleeping: a 503
// is normally retried after a backoff. If a cooldown for the same identity
// starts in the meantime (here, while the 503 is served), the retry is not
// sent and the call fails at once with a rate-limit error instead of
// sleeping the backoff first.
func TestGitHubSkillResolver_CooldownDuringBackoffFailsWithoutSleeping(t *testing.T) {
	clock := newFakeClock()
	var r *GitHubSkillResolver
	var commitCalls atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo/commits/main", func(w http.ResponseWriter, _ *http.Request) {
		commitCalls.Add(1)
		r.cooldown.record(GitHubCooldownIdentity(r.token), clock.Now().Add(time.Minute))
		w.Header().Set("Retry-After", "10")
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	r = newCooldownTestResolver(t, mux, clock)

	start := time.Now()
	res, err := r.Resolve(context.Background(), []api.SkillReference{{URI: "gh://owner/repo/s@main"}}, ResolveOpts{})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(res.Errors) != 1 || res.Errors[0].Code != GitHubRateLimitedCode || res.Errors[0].RetryAfter != "60" {
		t.Fatalf("expected one rate_limited error with RetryAfter 60, got %+v", res.Errors)
	}
	if n := commitCalls.Load(); n != 1 {
		t.Errorf("expected 1 request, got %d", n)
	}
	if elapsed >= 5*time.Second {
		t.Errorf("expected no backoff sleep, took %s", elapsed)
	}
}

// TestGitHubSkillResolver_CooldownRetryAfterRoundsUpWithFloor pins how
// cooldownRetryAfter renders the time left on a cooldown: a fraction of a
// second rounds up, and the result is never below 1, even when RetryAt has
// already passed by the tracker's clock.
func TestGitHubSkillResolver_CooldownRetryAfterRoundsUpWithFloor(t *testing.T) {
	clock := newFakeClock()
	r := &GitHubSkillResolver{cooldown: NewGitHubCooldown(clock.Now)}
	now := clock.Now()
	for _, tc := range []struct {
		left time.Duration
		want string
	}{
		{30*time.Second + 200*time.Millisecond, "31"},
		{100 * time.Millisecond, "1"},
		{-time.Second, "1"},
	} {
		if got := r.cooldownRetryAfter(&GitHubRateLimitError{RetryAt: now.Add(tc.left)}); got != tc.want {
			t.Errorf("RetryAt = now + (%v): got %q, want %q", tc.left, got, tc.want)
		}
	}
}
