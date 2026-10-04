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
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// resolveCounting calls ResolveWithFetch for key with a fetch that counts
// its calls and returns fetchErr (or a skill when fetchErr is nil).
func resolveCounting(t *testing.T, cache *GitHubResolutionCache, key string, calls *atomic.Int32, fetchErr error) error {
	t.Helper()
	_, err := cache.ResolveWithFetch(context.Background(), key, "flight|"+key, "cred", "ref", true, nil,
		func(context.Context) (ResolvedSkill, error) {
			calls.Add(1)
			if fetchErr != nil {
				return ResolvedSkill{}, fetchErr
			}
			return ResolvedSkill{Name: "s"}, nil
		})
	return err
}

// TestGitHubResolutionCache_NotFoundIsRemembered checks that a not_found
// from a fetch is returned again for the same cacheKey without fetching,
// and that another cacheKey (another ref, or the same ref with another
// credential) still fetches.
func TestGitHubResolutionCache_NotFoundIsRemembered(t *testing.T) {
	cache, err := newTestResolutionCache(t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	notFound := &githubResolveError{code: SkillErrCodeNotFound, msg: "skill not found"}
	var calls atomic.Int32

	for i := 0; i < 3; i++ {
		err := resolveCounting(t, cache, "gh://o/r/s@main#cred-a", &calls, notFound)
		var rerr *githubResolveError
		if !errors.As(err, &rerr) || rerr.code != SkillErrCodeNotFound {
			t.Fatalf("attempt %d: err = %v, want a not_found", i, err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("fetch ran %d times for one cacheKey, want 1", got)
	}

	_ = resolveCounting(t, cache, "gh://o/r/s@main#cred-b", &calls, notFound)
	if got := calls.Load(); got != 2 {
		t.Fatalf("fetch ran %d times after a second cacheKey, want 2", got)
	}
}

// TestGitHubResolutionCache_RetryableFailuresAreNotRemembered checks that
// only not_found is remembered: retryable causes, timeouts, rate limits and
// unclassified errors fetch again on the next call.
func TestGitHubResolutionCache_RetryableFailuresAreNotRemembered(t *testing.T) {
	cases := map[string]error{
		"upstream_unavailable": &githubResolveError{code: SkillErrCodeUpstreamUnavailable, msg: "503"},
		"unreachable":          &githubResolveError{code: SkillErrCodeUnreachable, msg: "no route"},
		"timeout":              &githubResolveError{code: SkillErrCodeTimeout, msg: "deadline"},
		"rate_limited":         &githubResolveError{code: SkillErrCodeRateLimited, msg: "limited"},
		"unclassified":         errors.New("status 422"),
	}
	for name, fetchErr := range cases {
		t.Run(name, func(t *testing.T) {
			cache, err := newTestResolutionCache(t.TempDir(), time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			_ = resolveCounting(t, cache, "gh://o/r/s@main", &calls, fetchErr)
			_ = resolveCounting(t, cache, "gh://o/r/s@main", &calls, fetchErr)
			if got := calls.Load(); got != 2 {
				t.Fatalf("fetch ran %d times, want 2 (failure must not be remembered)", got)
			}
		})
	}
}

// TestGitHubResolutionCache_RememberedFailureExpires checks that a
// remembered not_found stops applying once failureCacheTTL has passed.
func TestGitHubResolutionCache_RememberedFailureExpires(t *testing.T) {
	cache, err := newTestResolutionCache(t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	const key = "gh://o/r/s@main"
	notFound := &githubResolveError{code: SkillErrCodeNotFound, msg: "skill not found"}
	var calls atomic.Int32
	_ = resolveCounting(t, cache, key, &calls, notFound)

	// Move the remembered failure's expiry into the past.
	cache.failures.expire(key)

	if err := resolveCounting(t, cache, key, &calls, nil); err != nil {
		t.Fatalf("resolve after expiry: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("fetch ran %d times, want 2 once the failure expired", got)
	}
}

// TestGitHubResolutionCache_SuccessClearsRememberedFailure checks that a
// successful resolution stored for cacheKey replaces a remembered failure.
func TestGitHubResolutionCache_SuccessClearsRememberedFailure(t *testing.T) {
	cache, err := newTestResolutionCache(t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	const key = "gh://o/r/s@main"
	cache.recordFailure(key, &githubResolveError{code: SkillErrCodeNotFound, msg: "skill not found"})
	if cache.recentFailure(key) == nil {
		t.Fatal("failure not remembered")
	}
	cache.putEntry(key, ResolvedSkill{Name: "s"}, true)
	if err := cache.recentFailure(key); err != nil {
		t.Fatalf("failure still remembered after a successful resolution: %v", err)
	}
}

// TestGitHubResolutionCache_RecordFailureSkipsExpired checks that an
// expired failure is not served after another failure is recorded, and that
// the new failure is.
func TestGitHubResolutionCache_RecordFailureSkipsExpired(t *testing.T) {
	cache, err := newTestResolutionCache(t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cache.recordFailure("old", errors.New("old"))
	cache.failures.expire("old")
	cache.recordFailure("new", &githubResolveError{code: SkillErrCodeNotFound, msg: "nf"})
	if err := cache.recentFailure("old"); err != nil {
		t.Errorf("expired failure served: %v", err)
	}
	if cache.recentFailure("new") == nil {
		t.Error("new failure not recorded")
	}
}

// TestGitHubSkillResolver_MissingRefNotRefetched checks the resolver end to
// end: a ref GitHub reports as not found is looked up once, and a second
// Resolve shortly after reports the same not_found without another request.
func TestGitHubSkillResolver_MissingRefNotRefetched(t *testing.T) {
	server, mux := newTestGitHubServer(t)
	var hits atomic.Int32
	mux.HandleFunc("/repos/acme/tools/commits/main", func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "not found", http.StatusNotFound)
	})

	cache, err := newTestResolutionCache(t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	r := newTestGitHubResolver(server)
	r.resolutionCache = cache

	for i := 0; i < 2; i++ {
		res, err := r.Resolve(context.Background(), []api.SkillReference{{URI: "gh://acme/tools/missing@main"}}, ResolveOpts{})
		if err != nil {
			t.Fatalf("Resolve %d: %v", i, err)
		}
		if len(res.Errors) != 1 || res.Errors[0].Code != SkillErrCodeNotFound {
			t.Fatalf("Resolve %d: errors = %+v, want one not_found", i, res.Errors)
		}
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("GitHub was asked %d times, want 1", got)
	}
}

// TestGitHubResolutionCache_RecordFailureSetsTTL checks that a recorded
// failure expires failureCacheTTL after it was recorded.
func TestGitHubResolutionCache_RecordFailureSetsTTL(t *testing.T) {
	cache, err := newTestResolutionCache(t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	cache.recordFailure("k", &githubResolveError{code: SkillErrCodeNotFound, msg: "nf"})
	after := time.Now()

	cache.failures.mu.Lock()
	f, ok := cache.failures.entries["k"]
	cache.failures.mu.Unlock()
	if !ok {
		t.Fatal("failure not recorded")
	}
	if f.expiresAt.Before(before.Add(failureCacheTTL)) || f.expiresAt.After(after.Add(failureCacheTTL)) {
		t.Fatalf("expiresAt = %v, want within [%v, %v]", f.expiresAt,
			before.Add(failureCacheTTL), after.Add(failureCacheTTL))
	}
}

// TestGitHubResolutionCache_RememberedFailuresAreCapped checks that at
// most FailureMemoMaxEntries unexpired failures are held: a failure for a
// new key is not remembered once the limit is reached, a key already held
// is still updated, and expired entries make room again.
func TestGitHubResolutionCache_RememberedFailuresAreCapped(t *testing.T) {
	cache, err := newTestResolutionCache(t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	nf := &githubResolveError{code: SkillErrCodeNotFound, msg: "nf"}
	for i := 0; i < FailureMemoMaxEntries; i++ {
		cache.recordFailure(fmt.Sprintf("k%d", i), nf)
	}
	cache.recordFailure("extra", nf)
	if cache.recentFailure("extra") != nil {
		t.Error("failure for a new key remembered past the limit")
	}
	cache.recordFailure("k0", nf)
	if cache.recentFailure("k0") == nil {
		t.Error("failure for a key already held was dropped at the limit")
	}
	if n := cache.failures.Len(); n != FailureMemoMaxEntries {
		t.Errorf("remembered %d failures, want %d", n, FailureMemoMaxEntries)
	}
	// Expire one entry; the next record drops it and has room.
	cache.failures.expire("k1")

	cache.recordFailure("extra", nf)
	if cache.recentFailure("extra") == nil {
		t.Error("failure not remembered after an expired entry made room")
	}
}
