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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

const testCommitSHA = "abc123def456abc123def456abc123def456abcd"

func newTestGitHubServer(t *testing.T) (*httptest.Server, *http.ServeMux) {
	t.Helper()
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, mux
}

func newTestGitHubResolver(server *httptest.Server) *GitHubSkillResolver {
	return &GitHubSkillResolver{
		httpClient: server.Client(),
		token:      "test-token",
		apiBase:    server.URL,
		rawBase:    server.URL + "/raw",
	}
}

func TestGitHubSkillResolver_HappyPath(t *testing.T) {
	skillContent := "# My Skill\nDoes things."
	readmeContent := "# README"

	server, mux := newTestGitHubServer(t)

	mux.HandleFunc("/repos/owner/repo/commits/main", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/vnd.github.v3.sha" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(testCommitSHA))
	})

	mux.HandleFunc("/repos/owner/repo/contents/skills/my-skill", func(w http.ResponseWriter, r *http.Request) {
		ref := r.URL.Query().Get("ref")
		if ref != testCommitSHA {
			t.Errorf("expected ref=%s, got %s", testCommitSHA, ref)
		}
		_ = json.NewEncoder(w).Encode([]githubContentEntry{
			{Name: "SKILL.md", Path: "skills/my-skill/SKILL.md", Type: "file", Size: len(skillContent)},
			{Name: "README.md", Path: "skills/my-skill/README.md", Type: "file", Size: len(readmeContent)},
		})
	})

	mux.HandleFunc("/raw/owner/repo/"+testCommitSHA+"/skills/my-skill/SKILL.md", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(skillContent))
	})
	mux.HandleFunc("/raw/owner/repo/"+testCommitSHA+"/skills/my-skill/README.md", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(readmeContent))
	})

	resolver := newTestGitHubResolver(server)

	result, err := resolver.Resolve(context.Background(), []api.SkillReference{
		{URI: "gh://owner/repo/my-skill@main"},
	}, ResolveOpts{})

	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if len(result.Errors) != 0 {
		t.Fatalf("unexpected errors: %v", result.Errors)
	}
	if len(result.Resolved) != 1 {
		t.Fatalf("expected 1 resolved skill, got %d", len(result.Resolved))
	}

	skill := result.Resolved[0]
	if skill.Name != "my-skill" {
		t.Errorf("expected name my-skill, got %s", skill.Name)
	}
	if skill.Version != testCommitSHA[:12] {
		t.Errorf("expected version %s, got %s", testCommitSHA[:12], skill.Version)
	}
	if len(skill.Files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(skill.Files))
	}

	expectedHash := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(skillContent)))
	if skill.Files[0].Hash != expectedHash {
		t.Errorf("expected hash %s, got %s", expectedHash, skill.Files[0].Hash)
	}
	if skill.Files[0].Path != "SKILL.md" {
		t.Errorf("expected relative path SKILL.md, got %s", skill.Files[0].Path)
	}
	expectedURL := server.URL + "/raw/owner/repo/" + testCommitSHA + "/skills/my-skill/SKILL.md"
	if skill.Files[0].URL != expectedURL {
		t.Errorf("expected URL %s, got %s", expectedURL, skill.Files[0].URL)
	}

	bundleHash := transfer.ComputeContentHash([]transfer.FileInfo{
		{Path: "SKILL.md", Hash: fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(skillContent)))},
		{Path: "README.md", Hash: fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(readmeContent)))},
	})
	if skill.Hash != bundleHash {
		t.Errorf("expected bundle hash %s, got %s", bundleHash, skill.Hash)
	}
}

func TestGitHubSkillResolver_AuthHeader(t *testing.T) {
	server, mux := newTestGitHubServer(t)

	var gotAuth string
	mux.HandleFunc("/repos/owner/repo/commits/main", func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(testCommitSHA))
	})
	mux.HandleFunc("/repos/owner/repo/contents/skills/my-skill", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]githubContentEntry{
			{Name: "SKILL.md", Path: "skills/my-skill/SKILL.md", Type: "file", Size: 5},
		})
	})
	mux.HandleFunc("/raw/owner/repo/"+testCommitSHA+"/skills/my-skill/SKILL.md", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("hello"))
	})

	resolver := newTestGitHubResolver(server)
	resolver.token = "my-secret-token"

	_, err := resolver.Resolve(context.Background(), []api.SkillReference{
		{URI: "gh://owner/repo/my-skill@main"},
	}, ResolveOpts{})

	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if gotAuth != "Bearer my-secret-token" {
		t.Errorf("expected Authorization header 'Bearer my-secret-token', got %q", gotAuth)
	}
}

func TestGitHubSkillResolver_NotFound_Repo(t *testing.T) {
	server, mux := newTestGitHubServer(t)

	mux.HandleFunc("/repos/owner/nonexistent/commits/main", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	resolver := newTestGitHubResolver(server)

	result, err := resolver.Resolve(context.Background(), []api.SkillReference{
		{URI: "gh://owner/nonexistent/my-skill@main"},
	}, ResolveOpts{})

	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("expected 1 error, got %d", len(result.Errors))
	}
	if result.Errors[0].Code != SkillErrCodeNotFound {
		t.Errorf("expected code %s, got %s", SkillErrCodeNotFound, result.Errors[0].Code)
	}
	if !strings.Contains(result.Errors[0].Message, "not found") {
		t.Errorf("expected error to contain 'not found', got %s", result.Errors[0].Message)
	}
}

func TestGitHubSkillResolver_NotFound_SkillDir(t *testing.T) {
	server, mux := newTestGitHubServer(t)

	mux.HandleFunc("/repos/owner/repo/commits/main", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(testCommitSHA))
	})
	mux.HandleFunc("/repos/owner/repo/contents/skills/missing-skill", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	resolver := newTestGitHubResolver(server)

	result, err := resolver.Resolve(context.Background(), []api.SkillReference{
		{URI: "gh://owner/repo/missing-skill@main"},
	}, ResolveOpts{})

	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("expected 1 error, got %d", len(result.Errors))
	}
	if !strings.Contains(result.Errors[0].Message, "missing-skill") {
		t.Errorf("expected error to mention skill name, got %s", result.Errors[0].Message)
	}
	if result.Errors[0].Code != SkillErrCodeNotFound {
		t.Errorf("expected code %s, got %s", SkillErrCodeNotFound, result.Errors[0].Code)
	}
}

func TestGitHubSkillResolver_RateLimit(t *testing.T) {
	server, mux := newTestGitHubServer(t)

	attempts := 0
	mux.HandleFunc("/repos/owner/repo/commits/main", func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", "1700000000")
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusForbidden)
	})

	resolver := newTestGitHubResolver(server)

	result, err := resolver.Resolve(context.Background(), []api.SkillReference{
		{URI: "gh://owner/repo/my-skill@main"},
	}, ResolveOpts{})

	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("expected 1 error, got %d", len(result.Errors))
	}
	if result.Errors[0].Code != GitHubRateLimitedCode {
		t.Errorf("expected code %q, got %q", GitHubRateLimitedCode, result.Errors[0].Code)
	}
	if !strings.Contains(result.Errors[0].Message, "rate limit") {
		t.Errorf("expected error to mention rate limit, got %s", result.Errors[0].Message)
	}
	if !strings.Contains(result.Errors[0].Message, "gh://owner/repo/my-skill@main") {
		t.Errorf("expected error to name the ref, got %s", result.Errors[0].Message)
	}
	// A rate-limit response starts a cooldown instead of being retried.
	if attempts != 1 {
		t.Errorf("expected exactly 1 attempt, got %d", attempts)
	}
}

func TestGitHubSkillResolver_NoRetryOn429(t *testing.T) {
	server, mux := newTestGitHubServer(t)

	attempts := 0
	mux.HandleFunc("/repos/owner/repo/commits/main", func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	})

	resolver := newTestGitHubResolver(server)

	result, err := resolver.Resolve(context.Background(), []api.SkillReference{
		{URI: "gh://owner/repo/my-skill@main"},
	}, ResolveOpts{})

	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if len(result.Errors) != 1 || result.Errors[0].Code != GitHubRateLimitedCode {
		t.Fatalf("expected one rate_limited error, got %+v", result.Errors)
	}
	if attempts != 1 {
		t.Errorf("expected exactly 1 attempt, got %d", attempts)
	}
}

func TestGitHubSkillResolver_RetryOn5xx(t *testing.T) {
	server, mux := newTestGitHubServer(t)

	attempts := 0
	mux.HandleFunc("/repos/owner/repo/commits/main", func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		if attempts == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(testCommitSHA))
	})
	mux.HandleFunc("/repos/owner/repo/contents/skills/my-skill", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]githubContentEntry{
			{Name: "SKILL.md", Path: "skills/my-skill/SKILL.md", Type: "file", Size: 5},
		})
	})
	mux.HandleFunc("/raw/owner/repo/"+testCommitSHA+"/skills/my-skill/SKILL.md", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("hello"))
	})

	resolver := newTestGitHubResolver(server)

	result, err := resolver.Resolve(context.Background(), []api.SkillReference{
		{URI: "gh://owner/repo/my-skill@main"},
	}, ResolveOpts{})

	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if len(result.Errors) != 0 {
		t.Fatalf("unexpected errors: %v", result.Errors)
	}
	if attempts < 2 {
		t.Errorf("expected at least 2 attempts, got %d", attempts)
	}
}

// TestGitHubSkillResolver_StalledServer_FailsWithinBudget proves that a
// connection that never responds no longer consumes the whole create-deadline
// budget before failing (#2546): each attempt is bounded by
// githubRequestTimeout independently of the per-request client timeout, and
// doWithRetry fails fast once the next backoff would not fit the remaining
// ctx budget, instead of sleeping into an opaque context cancellation.
func TestGitHubSkillResolver_StalledServer_FailsWithinBudget(t *testing.T) {
	server, mux := newTestGitHubServer(t)

	var attempts int32
	mux.HandleFunc("/repos/owner/repo/commits/main", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		<-r.Context().Done() // never respond; only unblocks when the client gives up
	})

	resolver := newTestGitHubResolver(server)
	resolver.requestTimeout = 100 * time.Millisecond

	const budget = time.Second
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	start := time.Now()
	result, err := resolver.Resolve(ctx, []api.SkillReference{
		{URI: "gh://owner/repo/my-skill@main"},
	}, ResolveOpts{})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("expected 1 error, got %d", len(result.Errors))
	}
	if elapsed >= budget {
		t.Errorf("expected failure well before the %s budget elapsed, took %s", budget, elapsed)
	}
	if !strings.Contains(result.Errors[0].Message, "gh://owner/repo/my-skill@main") {
		t.Errorf("expected error to name the skill ref, got %s", result.Errors[0].Message)
	}
	if result.Errors[0].Code != SkillErrCodeTimeout {
		t.Errorf("expected code %s, got %s", SkillErrCodeTimeout, result.Errors[0].Code)
	}
	if atomic.LoadInt32(&attempts) < 1 {
		t.Error("expected at least one attempt to reach the server")
	}
	t.Logf("stalled-server resolution failed after %s (budget %s), attempts=%d, message=%s",
		elapsed, budget, atomic.LoadInt32(&attempts), result.Errors[0].Message)
}

// TestGitHubSkillResolver_RateLimit429_LargeRetryAfter_FailsWithinBudget
// proves that a 429 carrying a Retry-After larger than the remaining ctx
// budget fails immediately, naming the ref, instead of sleeping through (and
// past) the create deadline (#2546). The rate-limit cooldown is what ends the
// call: a rate-limit response is never retried. The doWithRetry fail-fast
// paths are pinned with 5xx responses by the BudgetFit and RetryAfterAboveCap
// tests.
func TestGitHubSkillResolver_RateLimit429_LargeRetryAfter_FailsWithinBudget(t *testing.T) {
	server, mux := newTestGitHubServer(t)

	var attempts int32
	mux.HandleFunc("/repos/owner/repo/commits/main", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.Header().Set("Retry-After", "120") // far larger than the budget below
		w.WriteHeader(http.StatusTooManyRequests)
	})

	resolver := newTestGitHubResolver(server)

	const budget = 5 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	start := time.Now()
	result, err := resolver.Resolve(ctx, []api.SkillReference{
		{URI: "gh://owner/repo/my-skill@main"},
	}, ResolveOpts{})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("expected 1 error, got %d", len(result.Errors))
	}
	if elapsed >= budget {
		t.Errorf("expected failure well before the %s budget elapsed, took %s", budget, elapsed)
	}
	if !strings.Contains(result.Errors[0].Message, "gh://owner/repo/my-skill@main") {
		t.Errorf("expected error to name the skill ref, got %s", result.Errors[0].Message)
	}
	if result.Errors[0].Code != SkillErrCodeRateLimited {
		t.Errorf("expected code %s, got %s", SkillErrCodeRateLimited, result.Errors[0].Code)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("expected exactly 1 attempt before failing fast on the oversized Retry-After, got %d", got)
	}
}

// TestGitHubSkillResolver_RateLimit403_LargeRetryAfter_FailsWithinBudget is
// the 403-rate-limit-exhaustion counterpart of the 429 test above.
func TestGitHubSkillResolver_RateLimit403_LargeRetryAfter_FailsWithinBudget(t *testing.T) {
	server, mux := newTestGitHubServer(t)

	var attempts int32
	mux.HandleFunc("/repos/owner/repo/commits/main", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("Retry-After", "180") // far larger than the budget below
		w.WriteHeader(http.StatusForbidden)
	})

	resolver := newTestGitHubResolver(server)

	const budget = 5 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	start := time.Now()
	result, err := resolver.Resolve(ctx, []api.SkillReference{
		{URI: "gh://owner/repo/my-skill@main"},
	}, ResolveOpts{})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("expected 1 error, got %d", len(result.Errors))
	}
	if elapsed >= budget {
		t.Errorf("expected failure well before the %s budget elapsed, took %s", budget, elapsed)
	}
	if !strings.Contains(result.Errors[0].Message, "gh://owner/repo/my-skill@main") {
		t.Errorf("expected error to name the skill ref, got %s", result.Errors[0].Message)
	}
	if result.Errors[0].Code != SkillErrCodeRateLimited {
		t.Errorf("expected code %s, got %s", SkillErrCodeRateLimited, result.Errors[0].Code)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("expected exactly 1 attempt before failing fast on the oversized Retry-After, got %d", got)
	}
}

// TestGitHubSkillResolver_RateLimit429_LargeRetryAfter_NoDeadlineCtx proves
// the exact production scenario from #2546: the broker's create ctx carries
// no deadline at all (context.Background(), not context.WithTimeout), yet a
// 429 with a Retry-After far larger than githubMaxBackoff still fails fast
// and names the ref. The rate-limit cooldown ends the call at the first
// response, without retrying.
func TestGitHubSkillResolver_RateLimit429_LargeRetryAfter_NoDeadlineCtx(t *testing.T) {
	server, mux := newTestGitHubServer(t)

	var attempts int32
	mux.HandleFunc("/repos/owner/repo/commits/main", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.Header().Set("Retry-After", "120") // far larger than githubMaxBackoff
		w.WriteHeader(http.StatusTooManyRequests)
	})

	resolver := newTestGitHubResolver(server)

	start := time.Now()
	result, err := resolver.Resolve(context.Background(), []api.SkillReference{
		{URI: "gh://owner/repo/my-skill@main"},
	}, ResolveOpts{})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("expected 1 error, got %d", len(result.Errors))
	}
	// Well under the CLI's ~30s timeout and under githubResolveBudget.
	if elapsed >= 5*time.Second {
		t.Errorf("expected failure within a few seconds, took %s", elapsed)
	}
	if !strings.Contains(result.Errors[0].Message, "gh://owner/repo/my-skill@main") {
		t.Errorf("expected error to name the skill ref, got %s", result.Errors[0].Message)
	}
	if result.Errors[0].Code != SkillErrCodeRateLimited {
		t.Errorf("expected code %s, got %s", SkillErrCodeRateLimited, result.Errors[0].Code)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("expected exactly 1 attempt before failing fast, got %d", got)
	}
}

// TestGitHubSkillResolver_StalledServer_NoDeadlineCtx is the stalled-server
// counterpart of the test above: with no caller deadline at all, Resolve must
// still impose its own budget (githubResolveBudget) so the ctx-deadline-based
// fail-fast in doWithRetry actually runs, instead of ctx.Deadline() reporting
// ok=false forever and leaving only the CLI's distant ~30s timeout to end the
// request as an opaque "context canceled" (#2546 R1).
func TestGitHubSkillResolver_StalledServer_NoDeadlineCtx(t *testing.T) {
	server, mux := newTestGitHubServer(t)

	var attempts int32
	mux.HandleFunc("/repos/owner/repo/commits/main", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		<-r.Context().Done() // never respond
	})

	resolver := newTestGitHubResolver(server)
	resolver.resolveBudget = time.Second
	resolver.requestTimeout = 100 * time.Millisecond

	start := time.Now()
	result, err := resolver.Resolve(context.Background(), []api.SkillReference{
		{URI: "gh://owner/repo/my-skill@main"},
	}, ResolveOpts{})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("expected 1 error, got %d", len(result.Errors))
	}
	if elapsed >= 2*resolver.resolveBudget {
		t.Errorf("expected failure within roughly the resolve budget (%s), took %s", resolver.resolveBudget, elapsed)
	}
	if !strings.Contains(result.Errors[0].Message, "gh://owner/repo/my-skill@main") {
		t.Errorf("expected error to name the skill ref, got %s", result.Errors[0].Message)
	}
	if result.Errors[0].Code != SkillErrCodeTimeout {
		t.Errorf("expected code %s, got %s", SkillErrCodeTimeout, result.Errors[0].Code)
	}
	if atomic.LoadInt32(&attempts) < 1 {
		t.Error("expected at least one attempt to reach the server")
	}
}

// TestGitHubSkillResolver_Upstream5xxAfterRetries_ClassifiedUpstreamUnavailable
// proves that GitHub returning 5xx on every attempt (retries exhausted) is
// classified upstream_unavailable rather than falling into the generic
// unclassified "resolve_failed" (#2546 R3). Retry-After: 0 makes every
// backoff zero, so the retries exhaust immediately.
func TestGitHubSkillResolver_Upstream5xxAfterRetries_ClassifiedUpstreamUnavailable(t *testing.T) {
	server, mux := newTestGitHubServer(t)

	var attempts int32
	mux.HandleFunc("/repos/owner/repo/commits/main", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusServiceUnavailable)
	})

	resolver := newTestGitHubResolver(server)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	result, err := resolver.Resolve(ctx, []api.SkillReference{
		{URI: "gh://owner/repo/my-skill@main"},
	}, ResolveOpts{})

	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("expected 1 error, got %d", len(result.Errors))
	}
	if result.Errors[0].Code != SkillErrCodeUpstreamUnavailable {
		t.Errorf("expected code %s, got %s", SkillErrCodeUpstreamUnavailable, result.Errors[0].Code)
	}
	if got := atomic.LoadInt32(&attempts); got != githubMaxRetries+1 {
		t.Errorf("expected %d attempts (retries exhausted), got %d", githubMaxRetries+1, got)
	}
}

// TestGitHubSkillResolver_UnknownClientError_StaysUnclassified proves that a
// non-retryable, non-404 status like 401 (bad or expired token) is left
// unclassified ("resolve_failed") rather than guessed at — it used to default
// to a client-facing 400 at the broker; after the fix the default stays on
// the 500 path (#2546 R3).
func TestGitHubSkillResolver_UnknownClientError_StaysUnclassified(t *testing.T) {
	server, mux := newTestGitHubServer(t)

	mux.HandleFunc("/repos/owner/repo/commits/main", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})

	resolver := newTestGitHubResolver(server)

	result, err := resolver.Resolve(context.Background(), []api.SkillReference{
		{URI: "gh://owner/repo/my-skill@main"},
	}, ResolveOpts{})

	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("expected 1 error, got %d", len(result.Errors))
	}
	if result.Errors[0].Code != "resolve_failed" {
		t.Errorf("expected unclassified code resolve_failed, got %s", result.Errors[0].Code)
	}
}

// TestGitHubSkillResolver_ConnectionRefused_ClassifiedUnreachable proves that
// a network-level failure — here, a connection refused because nothing is
// listening — is classified unreachable rather than the misleading generic
// "timeout" (which is reserved for a context deadline actually expiring)
// (#2546 N1).
func TestGitHubSkillResolver_ConnectionRefused_ClassifiedUnreachable(t *testing.T) {
	// Bind and immediately close a listener to get a port nothing is
	// listening on, so the connection is refused quickly and deterministically.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to allocate a port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	resolver := &GitHubSkillResolver{
		httpClient:     &http.Client{Timeout: githubAPITimeout},
		apiBase:        "http://" + addr,
		rawBase:        "http://" + addr + "/raw",
		requestTimeout: 100 * time.Millisecond,
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	result, err := resolver.Resolve(ctx, []api.SkillReference{
		{URI: "gh://owner/repo/my-skill@main"},
	}, ResolveOpts{})

	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("expected 1 error, got %d", len(result.Errors))
	}
	if result.Errors[0].Code != SkillErrCodeUnreachable {
		t.Errorf("expected code %s, got %s", SkillErrCodeUnreachable, result.Errors[0].Code)
	}

	// Resolve flattens the error to a code and message, so check the chain
	// one level down, on a real request path: the typed error must carry
	// the transport failure as its cause, reachable through Unwrap. With a
	// 1s ctx, the 1s first backoff does not fit, so this pins the budget
	// fail-fast path, not retries-exhausted (see
	// TestGitHubSkillResolver_NetworkErrorOnFinalAttempt_KeepsCause).
	ghRef, err := ParseGitHubSkillURI("gh://owner/repo/my-skill@main")
	if err != nil {
		t.Fatalf("ParseGitHubSkillURI: %v", err)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	_, err = resolver.resolveCommitSHA(ctx2, ghRef, "")
	var rerr *githubResolveError
	if !errors.As(err, &rerr) || rerr.code != SkillErrCodeUnreachable {
		t.Fatalf("expected a githubResolveError with code %s, got %T: %v", SkillErrCodeUnreachable, err, err)
	}
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		t.Errorf("expected the *url.Error cause to be reachable through Unwrap, got %v", err)
	}
	var opErr *net.OpError
	if !errors.As(err, &opErr) || opErr.Op != "dial" {
		t.Errorf("expected a dial *net.OpError in the chain, got %v", err)
	}
}

// TestGitHubSkillResolver_NetworkErrorOnFinalAttempt_KeepsCause pins the
// retries-exhausted path after doWithRetry's loop: the first attempts get a
// 503 with Retry-After: 0, so every backoff is zero, and the final attempt's
// connection is closed without a response. The typed error must be
// classified from that network error and carry it as its cause.
func TestGitHubSkillResolver_NetworkErrorOnFinalAttempt_KeepsCause(t *testing.T) {
	server, mux := newTestGitHubServer(t)

	var attempts int32
	mux.HandleFunc("/repos/owner/repo/commits/main", func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&attempts, 1) <= githubMaxRetries {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_ = conn.Close()
	})

	resolver := newTestGitHubResolver(server)
	ghRef, err := ParseGitHubSkillURI("gh://owner/repo/my-skill@main")
	if err != nil {
		t.Fatalf("ParseGitHubSkillURI: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err = resolver.resolveCommitSHA(ctx, ghRef, "")

	if got := atomic.LoadInt32(&attempts); got < githubMaxRetries+1 {
		t.Fatalf("expected at least %d attempts (retries exhausted), got %d", githubMaxRetries+1, got)
	}
	var rerr *githubResolveError
	if !errors.As(err, &rerr) || rerr.code != SkillErrCodeUnreachable {
		t.Fatalf("expected a githubResolveError with code %s, got %T: %v", SkillErrCodeUnreachable, err, err)
	}
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		t.Errorf("expected the *url.Error cause to be reachable through Unwrap, got %v", err)
	}
}

// TestGitHubSkillResolver_RawDownloadStall_FailsFast proves that a raw-file
// download whose connection stalls after accepting the request still fails
// fast (via the httpClient's ResponseHeaderTimeout), even though the ctx
// timeout bounding the overall download attempt was widened to accommodate
// large, legitimately slow transfers (#2546 O2).
func TestGitHubSkillResolver_RawDownloadStall_FailsFast(t *testing.T) {
	server, mux := newTestGitHubServer(t)

	mux.HandleFunc("/repos/owner/repo/commits/main", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(testCommitSHA))
	})
	mux.HandleFunc("/repos/owner/repo/contents/skills/my-skill", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]githubContentEntry{
			{Name: "SKILL.md", Path: "skills/my-skill/SKILL.md", Type: "file", Size: 5},
		})
	})
	mux.HandleFunc("/raw/owner/repo/"+testCommitSHA+"/skills/my-skill/SKILL.md", func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // accept the connection but never write a response
	})

	resolver := newTestGitHubResolver(server)
	// Production wiring (newGitHubHTTPClient) with shrunk timeouts: the short
	// ResponseHeaderTimeout must catch the stall, independent of the much
	// longer ctx bound for the download attempt.
	const stall = 100 * time.Millisecond
	resolver.httpClient = newGitHubHTTPClient(stall)
	resolver.requestTimeout = stall
	resolver.downloadTimeout = 10 * time.Second

	// 1s leaves no room for a retry (1s backoff), so the call ends at the
	// first stall; only ResponseHeaderTimeout can end it well before 1s.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	start := time.Now()
	result, err := resolver.Resolve(ctx, []api.SkillReference{
		{URI: "gh://owner/repo/my-skill@main"},
	}, ResolveOpts{})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("expected 1 error, got %d", len(result.Errors))
	}
	if elapsed >= 500*time.Millisecond {
		t.Errorf("expected the stall to be caught by ResponseHeaderTimeout (%s), well before the download timeout, took %s",
			stall, elapsed)
	}
	if result.Errors[0].Code != SkillErrCodeTimeout {
		t.Errorf("expected code %s, got %s", SkillErrCodeTimeout, result.Errors[0].Code)
	}
}

// TestNewGitHubHTTPClient_KeepsDefaultTransportSettings proves the resolver's
// client is built from a clone of http.DefaultTransport, so proxy support
// and the default dial/TLS settings survive alongside the stall timeout
// (#2546 RQ2).
func TestNewGitHubHTTPClient_KeepsDefaultTransportSettings(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for name, c := range map[string]*http.Client{
		"helper":      newGitHubHTTPClient(githubRequestTimeout),
		"constructor": NewGitHubSkillResolver().httpClient,
	} {
		tr, ok := c.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("%s: expected *http.Transport, got %T", name, c.Transport)
		}
		if tr.Proxy == nil {
			t.Errorf("%s: expected Proxy to be set (ProxyFromEnvironment)", name)
		}
		if tr.TLSHandshakeTimeout == 0 || tr.MaxIdleConns == 0 || !tr.ForceAttemptHTTP2 {
			t.Errorf("%s: expected DefaultTransport settings to survive, got TLSHandshakeTimeout=%s MaxIdleConns=%d ForceAttemptHTTP2=%t",
				name, tr.TLSHandshakeTimeout, tr.MaxIdleConns, tr.ForceAttemptHTTP2)
		}
		if tr.ResponseHeaderTimeout != githubRequestTimeout {
			t.Errorf("%s: expected ResponseHeaderTimeout %s, got %s", name, githubRequestTimeout, tr.ResponseHeaderTimeout)
		}
		if tr == http.DefaultTransport {
			t.Errorf("%s: expected a clone, not http.DefaultTransport itself", name)
		}
	}
}

// TestGitHubSkillResolver_StallBound_ReadsTransport proves the budget-fit
// reservation comes from the transport's ResponseHeaderTimeout, the real
// stall bound, rather than a separately configured field (#2546 N1).
func TestGitHubSkillResolver_StallBound_ReadsTransport(t *testing.T) {
	r := &GitHubSkillResolver{
		httpClient:     newGitHubHTTPClient(250 * time.Millisecond),
		requestTimeout: 7 * time.Second,
	}
	if got := r.stallBound(); got != 250*time.Millisecond {
		t.Errorf("expected stall bound from transport (250ms), got %s", got)
	}

	r.httpClient = http.DefaultClient
	if got := r.stallBound(); got != 7*time.Second {
		t.Errorf("expected fallback to requestTimeout (7s) without ResponseHeaderTimeout, got %s", got)
	}
}

// TestGitHubSkillResolver_TransientThenSuccess_FitsWithinBudget proves the
// budget-fit check does not interfere with a retry that legitimately fits:
// a single transient 5xx followed by success still succeeds inside a
// realistic create-deadline-sized budget.
func TestGitHubSkillResolver_TransientThenSuccess_FitsWithinBudget(t *testing.T) {
	server, mux := newTestGitHubServer(t)

	attempts := 0
	mux.HandleFunc("/repos/owner/repo/commits/main", func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		if attempts == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(testCommitSHA))
	})
	mux.HandleFunc("/repos/owner/repo/contents/skills/my-skill", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]githubContentEntry{
			{Name: "SKILL.md", Path: "skills/my-skill/SKILL.md", Type: "file", Size: 5},
		})
	})
	mux.HandleFunc("/raw/owner/repo/"+testCommitSHA+"/skills/my-skill/SKILL.md", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("hello"))
	})

	resolver := newTestGitHubResolver(server)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	result, err := resolver.Resolve(ctx, []api.SkillReference{
		{URI: "gh://owner/repo/my-skill@main"},
	}, ResolveOpts{})

	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if len(result.Errors) != 0 {
		t.Fatalf("unexpected errors: %v", result.Errors)
	}
	if len(result.Resolved) != 1 {
		t.Fatalf("expected 1 resolved skill, got %d", len(result.Resolved))
	}
	if attempts < 2 {
		t.Errorf("expected at least 2 attempts, got %d", attempts)
	}
}

// TestGitHubSkillResolver_BackoffLogsStatusAndRetryAfterAtWarn proves that
// the HTTP status and Retry-After are logged at WARN before a backoff, so a
// production incident shows more than the previous debug-only "context
// canceled" (#2546).
func TestGitHubSkillResolver_BackoffLogsStatusAndRetryAfterAtWarn(t *testing.T) {
	server, mux := newTestGitHubServer(t)

	attempts := 0
	mux.HandleFunc("/repos/owner/repo/commits/main", func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		if attempts == 1 {
			// A 5xx, not a rate limit: rate-limit responses start a
			// cooldown and are not retried, so they log no backoff.
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(testCommitSHA))
	})
	mux.HandleFunc("/repos/owner/repo/contents/skills/my-skill", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]githubContentEntry{
			{Name: "SKILL.md", Path: "skills/my-skill/SKILL.md", Type: "file", Size: 5},
		})
	})
	mux.HandleFunc("/raw/owner/repo/"+testCommitSHA+"/skills/my-skill/SKILL.md", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("hello"))
	})

	resolver := newTestGitHubResolver(server)

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	_, err := resolver.Resolve(context.Background(), []api.SkillReference{
		{URI: "gh://owner/repo/my-skill@main"},
	}, ResolveOpts{})
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "level=WARN") {
		t.Fatalf("expected a WARN log line, got: %s", out)
	}
	if !strings.Contains(out, "status=503") {
		t.Errorf("expected log to include the HTTP status, got: %s", out)
	}
	if !strings.Contains(out, "retry_after=0") {
		t.Errorf("expected log to include Retry-After, got: %s", out)
	}
}

func TestGitHubSkillResolver_ResolutionCacheHit(t *testing.T) {
	server, mux := newTestGitHubServer(t)
	apiCalls := 0

	mux.HandleFunc("/repos/owner/repo/commits/main", func(w http.ResponseWriter, _ *http.Request) {
		apiCalls++
		_, _ = w.Write([]byte(testCommitSHA))
	})
	mux.HandleFunc("/repos/owner/repo/contents/skills/my-skill", func(w http.ResponseWriter, _ *http.Request) {
		apiCalls++
		_ = json.NewEncoder(w).Encode([]githubContentEntry{
			{Name: "SKILL.md", Path: "skills/my-skill/SKILL.md", Type: "file", Size: 5},
		})
	})
	mux.HandleFunc("/raw/owner/repo/"+testCommitSHA+"/skills/my-skill/SKILL.md", func(w http.ResponseWriter, _ *http.Request) {
		apiCalls++
		_, _ = w.Write([]byte("hello"))
	})

	resolver := newTestGitHubResolver(server)
	cache, err := newTestResolutionCache(t.TempDir(), 5*time.Minute)
	if err != nil {
		t.Fatalf("cache creation failed: %v", err)
	}
	resolver.resolutionCache = cache

	// First call — should hit the API
	result1, err := resolver.Resolve(context.Background(), []api.SkillReference{
		{URI: "gh://owner/repo/my-skill@main"},
	}, ResolveOpts{})
	if err != nil {
		t.Fatalf("first Resolve failed: %v", err)
	}
	if len(result1.Resolved) != 1 {
		t.Fatalf("expected 1 resolved skill, got %d", len(result1.Resolved))
	}
	firstCallAPICalls := apiCalls

	// Second call — should use cache, no new API calls
	result2, err := resolver.Resolve(context.Background(), []api.SkillReference{
		{URI: "gh://owner/repo/my-skill@main"},
	}, ResolveOpts{})
	if err != nil {
		t.Fatalf("second Resolve failed: %v", err)
	}
	if len(result2.Resolved) != 1 {
		t.Fatalf("expected 1 resolved skill on second call, got %d", len(result2.Resolved))
	}
	if apiCalls != firstCallAPICalls {
		t.Errorf("expected no new API calls on cache hit, but got %d additional calls", apiCalls-firstCallAPICalls)
	}
	if result2.Resolved[0].Name != result1.Resolved[0].Name {
		t.Errorf("cached result name mismatch: %s vs %s", result2.Resolved[0].Name, result1.Resolved[0].Name)
	}
}

func TestGitHubSkillResolver_InvalidURI(t *testing.T) {
	resolver := &GitHubSkillResolver{
		httpClient: http.DefaultClient,
		apiBase:    "http://unused",
		rawBase:    "http://unused",
	}

	result, err := resolver.Resolve(context.Background(), []api.SkillReference{
		{URI: "invalid://not-github"},
	}, ResolveOpts{})

	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("expected 1 error, got %d", len(result.Errors))
	}
	if result.Errors[0].Code != "invalid_uri" {
		t.Errorf("expected code invalid_uri, got %s", result.Errors[0].Code)
	}
}

func TestGitHubSkillResolver_DefaultBranch(t *testing.T) {
	server, mux := newTestGitHubServer(t)

	var requestedPath string
	mux.HandleFunc("/repos/owner/repo/commits/HEAD", func(w http.ResponseWriter, r *http.Request) {
		requestedPath = r.URL.Path
		_, _ = w.Write([]byte(testCommitSHA))
	})
	mux.HandleFunc("/repos/owner/repo/contents/skills/my-skill", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]githubContentEntry{
			{Name: "SKILL.md", Path: "skills/my-skill/SKILL.md", Type: "file", Size: 5},
		})
	})
	mux.HandleFunc("/raw/owner/repo/"+testCommitSHA+"/skills/my-skill/SKILL.md", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("hello"))
	})

	resolver := newTestGitHubResolver(server)

	_, err := resolver.Resolve(context.Background(), []api.SkillReference{
		{URI: "gh://owner/repo/my-skill"},
	}, ResolveOpts{})

	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if !strings.HasSuffix(requestedPath, "/HEAD") {
		t.Errorf("expected HEAD ref request, got path %s", requestedPath)
	}
}

func TestGitHubSkillResolver_MixedBatch(t *testing.T) {
	server, mux := newTestGitHubServer(t)

	mux.HandleFunc("/repos/owner/repo/commits/main", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(testCommitSHA))
	})
	mux.HandleFunc("/repos/owner/repo/contents/skills/my-skill", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]githubContentEntry{
			{Name: "SKILL.md", Path: "skills/my-skill/SKILL.md", Type: "file", Size: 5},
		})
	})
	mux.HandleFunc("/raw/owner/repo/"+testCommitSHA+"/skills/my-skill/SKILL.md", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("hello"))
	})

	ghResolver := newTestGitHubResolver(server)

	hubResolved := ResolvedSkill{
		Name:    "hub-skill",
		URI:     "skill://hub-skill",
		Version: "1.0.0",
		Hash:    "sha256:fakehash",
		Files:   []ResolvedFile{{Path: "SKILL.md", URL: "https://example.com/SKILL.md", Hash: "sha256:abc", Size: 5}},
	}
	hubResolver := &stubSkillResolver{result: &ResolveResult{Resolved: []ResolvedSkill{hubResolved}}}

	router := NewRoutingSkillResolver(hubResolver)
	router.Register("gh", ghResolver)

	result, err := router.Resolve(context.Background(), []api.SkillReference{
		{URI: "gh://owner/repo/my-skill@main"},
		{URI: "skill://hub-skill"},
	}, ResolveOpts{})

	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if len(result.Errors) != 0 {
		t.Fatalf("unexpected errors: %v", result.Errors)
	}
	if len(result.Resolved) != 2 {
		t.Fatalf("expected 2 resolved skills, got %d", len(result.Resolved))
	}

	var gotGH, gotHub bool
	for _, s := range result.Resolved {
		if s.Name == "my-skill" {
			gotGH = true
		}
		if s.Name == "hub-skill" {
			gotHub = true
		}
	}
	if !gotGH {
		t.Error("missing gh:// resolved skill")
	}
	if !gotHub {
		t.Error("missing skill:// resolved skill")
	}
}

func TestIsRetryableResponse(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		headers    map[string]string
		want       bool
	}{
		// Rate limits are never retried; GitHubCooldown.Do intercepts them.
		{"429 is not retryable", 429, nil, false},
		{"403 with rate limit is not retryable", 403, map[string]string{"X-RateLimit-Remaining": "0"}, false},
		{"403 without rate limit is not retryable", 403, nil, false},
		{"500 is retryable", 500, nil, true},
		{"502 is retryable", 502, nil, true},
		{"503 is retryable", 503, nil, true},
		{"200 is not retryable", 200, nil, false},
		{"404 is not retryable", 404, nil, false},
		{"401 is not retryable", 401, nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := &http.Response{
				StatusCode: tt.statusCode,
				Header:     make(http.Header),
			}
			for k, v := range tt.headers {
				resp.Header.Set(k, v)
			}
			if got := isRetryableResponse(resp); got != tt.want {
				t.Errorf("isRetryableResponse() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRetryDelay(t *testing.T) {
	t.Run("uses Retry-After header", func(t *testing.T) {
		resp := &http.Response{
			StatusCode: 503,
			Header:     make(http.Header),
		}
		resp.Header.Set("Retry-After", "3")
		got := retryDelay(resp, 1)
		if got != 3*time.Second {
			t.Errorf("expected 3s, got %v", got)
		}
	})

	t.Run("caps Retry-After at max backoff", func(t *testing.T) {
		resp := &http.Response{
			StatusCode: 503,
			Header:     make(http.Header),
		}
		resp.Header.Set("Retry-After", "120")
		got := retryDelay(resp, 1)
		if got != githubMaxBackoff {
			t.Errorf("expected %v, got %v", githubMaxBackoff, got)
		}
	})

	t.Run("exponential backoff without headers", func(t *testing.T) {
		d1 := retryDelay(nil, 1)
		d2 := retryDelay(nil, 2)
		d3 := retryDelay(nil, 3)
		if d1 != 1*time.Second {
			t.Errorf("attempt 1: expected 1s, got %v", d1)
		}
		if d2 != 2*time.Second {
			t.Errorf("attempt 2: expected 2s, got %v", d2)
		}
		if d3 != 4*time.Second {
			t.Errorf("attempt 3: expected 4s, got %v", d3)
		}
	})
}

func TestGitHubSkillResolver_TokenForRef(t *testing.T) {
	t.Run("named secret present returns correct value", func(t *testing.T) {
		r := &GitHubSkillResolver{
			token: "default-token",
			provisionCredentials: map[string]string{
				"MY_SECRET": "secret-value",
			},
		}
		ref := &GitHubSkillRef{TokenSecretName: "MY_SECRET"}
		got, err := r.tokenForRef(ref)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "secret-value" {
			t.Errorf("expected secret-value, got %q", got)
		}
	})

	t.Run("named secret missing returns error", func(t *testing.T) {
		r := &GitHubSkillResolver{
			token:                "default-token",
			provisionCredentials: map[string]string{},
		}
		ref := &GitHubSkillRef{TokenSecretName: "MISSING_SECRET"}
		_, err := r.tokenForRef(ref)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "MISSING_SECRET") {
			t.Errorf("error should mention secret name, got: %v", err)
		}
		if !strings.Contains(err.Error(), "ProvisionCredentials") {
			t.Errorf("error should mention ProvisionCredentials, got: %v", err)
		}
	})

	t.Run("named secret with nil provisionCredentials returns error", func(t *testing.T) {
		r := &GitHubSkillResolver{
			token:                "default-token",
			provisionCredentials: nil,
		}
		ref := &GitHubSkillRef{TokenSecretName: "MY_SECRET"}
		_, err := r.tokenForRef(ref)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})

	t.Run("empty TokenSecretName returns default token", func(t *testing.T) {
		r := &GitHubSkillResolver{
			token: "default-token",
			provisionCredentials: map[string]string{
				"OTHER_SECRET": "other-value",
			},
		}
		ref := &GitHubSkillRef{TokenSecretName: ""}
		got, err := r.tokenForRef(ref)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "default-token" {
			t.Errorf("expected default-token, got %q", got)
		}
	})
}

func TestGitHubSkillResolver_PerURIToken(t *testing.T) {
	server, mux := newTestGitHubServer(t)

	var gotAuth string
	mux.HandleFunc("/repos/owner/repo/commits/HEAD", func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(testCommitSHA))
	})
	mux.HandleFunc("/repos/owner/repo/contents/skills/my-skill", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]githubContentEntry{
			{Name: "SKILL.md", Path: "skills/my-skill/SKILL.md", Type: "file", Size: 5},
		})
	})
	mux.HandleFunc("/raw/owner/repo/"+testCommitSHA+"/skills/my-skill/SKILL.md", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("hello"))
	})

	resolver := &GitHubSkillResolver{
		httpClient: server.Client(),
		token:      "default-token",
		apiBase:    server.URL,
		rawBase:    server.URL + "/raw",
		provisionCredentials: map[string]string{
			"SKILLS_TOKEN": "per-uri-token",
		},
	}

	result, err := resolver.Resolve(context.Background(), []api.SkillReference{
		{URI: "gh://owner/repo/my-skill?token=SKILLS_TOKEN"},
	}, ResolveOpts{})

	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if len(result.Errors) != 0 {
		t.Fatalf("unexpected errors: %v", result.Errors)
	}
	if gotAuth != "Bearer per-uri-token" {
		t.Errorf("expected per-uri-token to be used, got Authorization: %q", gotAuth)
	}
}

func TestGitHubSkillResolver_MissingNamedSecret(t *testing.T) {
	resolver := &GitHubSkillResolver{
		httpClient:           http.DefaultClient,
		token:                "default-token",
		apiBase:              "http://unused",
		rawBase:              "http://unused",
		provisionCredentials: map[string]string{},
	}

	result, err := resolver.Resolve(context.Background(), []api.SkillReference{
		{URI: "gh://owner/repo/my-skill?token=MISSING_SECRET"},
	}, ResolveOpts{})

	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("expected 1 error, got %d: %v", len(result.Errors), result.Errors)
	}
	if result.Errors[0].Code != "resolve_failed" {
		t.Errorf("expected code resolve_failed, got %s", result.Errors[0].Code)
	}
	if !strings.Contains(result.Errors[0].Message, "MISSING_SECRET") {
		t.Errorf("error should mention secret name, got: %s", result.Errors[0].Message)
	}
}

func TestGitHubSkillResolver_CacheHitCredentialCheck(t *testing.T) {
	server, mux := newTestGitHubServer(t)
	apiCalls := 0

	mux.HandleFunc("/repos/owner/repo/commits/HEAD", func(w http.ResponseWriter, _ *http.Request) {
		apiCalls++
		_, _ = w.Write([]byte(testCommitSHA))
	})
	mux.HandleFunc("/repos/owner/repo/contents/skills/my-skill", func(w http.ResponseWriter, _ *http.Request) {
		apiCalls++
		_ = json.NewEncoder(w).Encode([]githubContentEntry{
			{Name: "SKILL.md", Path: "skills/my-skill/SKILL.md", Type: "file", Size: 5},
		})
	})
	mux.HandleFunc("/raw/owner/repo/"+testCommitSHA+"/skills/my-skill/SKILL.md", func(w http.ResponseWriter, _ *http.Request) {
		apiCalls++
		_, _ = w.Write([]byte("hello"))
	})

	cache, err := newTestResolutionCache(t.TempDir(), 5*time.Minute)
	if err != nil {
		t.Fatalf("cache creation failed: %v", err)
	}

	t.Run("cache hit with valid credential succeeds without API call", func(t *testing.T) {
		apiCalls = 0
		resolver := &GitHubSkillResolver{
			httpClient: server.Client(),
			token:      "default-token",
			apiBase:    server.URL,
			rawBase:    server.URL + "/raw",
			provisionCredentials: map[string]string{
				"SKILLS_TOKEN": "valid-secret-value",
			},
			resolutionCache: cache,
		}

		// First call — populates cache
		result1, err := resolver.Resolve(context.Background(), []api.SkillReference{
			{URI: "gh://owner/repo/my-skill?token=SKILLS_TOKEN"},
		}, ResolveOpts{})
		if err != nil {
			t.Fatalf("first Resolve failed: %v", err)
		}
		if len(result1.Errors) != 0 {
			t.Fatalf("unexpected errors on first call: %v", result1.Errors)
		}
		callsAfterFirst := apiCalls

		// Second call with same valid credential — should hit cache, no new API calls
		result2, err := resolver.Resolve(context.Background(), []api.SkillReference{
			{URI: "gh://owner/repo/my-skill?token=SKILLS_TOKEN"},
		}, ResolveOpts{})
		if err != nil {
			t.Fatalf("second Resolve failed: %v", err)
		}
		if len(result2.Errors) != 0 {
			t.Fatalf("unexpected errors on cache hit: %v", result2.Errors)
		}
		if len(result2.Resolved) != 1 {
			t.Fatalf("expected 1 resolved skill on cache hit, got %d", len(result2.Resolved))
		}
		if apiCalls != callsAfterFirst {
			t.Errorf("expected no new API calls on cache hit, got %d additional calls", apiCalls-callsAfterFirst)
		}
	})

	t.Run("cache hit with missing credential returns error", func(t *testing.T) {
		apiCallsBefore := apiCalls

		// A resolver that has a populated cache but lacks the named secret
		resolverNoSecret := &GitHubSkillResolver{
			httpClient:           server.Client(),
			token:                "default-token",
			apiBase:              server.URL,
			rawBase:              server.URL + "/raw",
			provisionCredentials: map[string]string{}, // SKILLS_TOKEN not present
			resolutionCache:      cache,
		}

		result, err := resolverNoSecret.Resolve(context.Background(), []api.SkillReference{
			{URI: "gh://owner/repo/my-skill?token=SKILLS_TOKEN"},
		}, ResolveOpts{})
		if err != nil {
			t.Fatalf("Resolve returned unexpected Go error: %v", err)
		}
		// Should get a resolve error, not a cache hit success
		if len(result.Errors) != 1 {
			t.Fatalf("expected 1 error (credential check on cache hit), got %d errors and %d resolved", len(result.Errors), len(result.Resolved))
		}
		if result.Errors[0].Code != "resolve_failed" {
			t.Errorf("expected code resolve_failed, got %s", result.Errors[0].Code)
		}
		if !strings.Contains(result.Errors[0].Message, "SKILLS_TOKEN") {
			t.Errorf("error should mention secret name, got: %s", result.Errors[0].Message)
		}
		// No new API calls should have been made (cache hit path, rejected before fetch)
		if apiCalls != apiCallsBefore {
			t.Errorf("expected no new API calls when credential check fails on cache hit, got %d", apiCalls-apiCallsBefore)
		}
	})
}

func TestGitHubSkillResolver_CrossCredentialCacheIsolation(t *testing.T) {
	// Verify that two resolvers with different credentials for the same URI
	// do NOT share a cache entry. This prevents cross-credential information
	// disclosure where a lower-privilege token would receive cached content
	// fetched by a higher-privilege token.
	server, mux := newTestGitHubServer(t)
	apiCalls := 0

	mux.HandleFunc("/repos/owner/repo/commits/HEAD", func(w http.ResponseWriter, _ *http.Request) {
		apiCalls++
		_, _ = w.Write([]byte(testCommitSHA))
	})
	mux.HandleFunc("/repos/owner/repo/contents/skills/my-skill", func(w http.ResponseWriter, _ *http.Request) {
		apiCalls++
		_ = json.NewEncoder(w).Encode([]githubContentEntry{
			{Name: "SKILL.md", Path: "skills/my-skill/SKILL.md", Type: "file", Size: 5},
		})
	})
	mux.HandleFunc("/raw/owner/repo/"+testCommitSHA+"/skills/my-skill/SKILL.md", func(w http.ResponseWriter, _ *http.Request) {
		apiCalls++
		_, _ = w.Write([]byte("hello"))
	})

	// Use a shared cache to demonstrate isolation.
	cache, err := newTestResolutionCache(t.TempDir(), 5*time.Minute)
	if err != nil {
		t.Fatalf("cache creation failed: %v", err)
	}

	// Resolver A uses token "token-alpha".
	resolverA := &GitHubSkillResolver{
		httpClient:      server.Client(),
		token:           "token-alpha",
		apiBase:         server.URL,
		rawBase:         server.URL + "/raw",
		resolutionCache: cache,
	}

	// Resolver B uses a different token "token-beta" but requests the same URI.
	resolverB := &GitHubSkillResolver{
		httpClient:      server.Client(),
		token:           "token-beta",
		apiBase:         server.URL,
		rawBase:         server.URL + "/raw",
		resolutionCache: cache,
	}

	uri := []api.SkillReference{{URI: "gh://owner/repo/my-skill"}}

	// First call via resolver A — populates cache under alpha's key.
	resultA, err := resolverA.Resolve(context.Background(), uri, ResolveOpts{})
	if err != nil {
		t.Fatalf("resolverA Resolve failed: %v", err)
	}
	if len(resultA.Errors) != 0 {
		t.Fatalf("resolverA: unexpected errors: %v", resultA.Errors)
	}
	callsAfterA := apiCalls

	// Second call via resolver B — must NOT hit resolver A's cache entry.
	// Because tokens differ, the cache keys differ, so a fresh API call is required.
	resultB, err := resolverB.Resolve(context.Background(), uri, ResolveOpts{})
	if err != nil {
		t.Fatalf("resolverB Resolve failed: %v", err)
	}
	if len(resultB.Errors) != 0 {
		t.Fatalf("resolverB: unexpected errors: %v", resultB.Errors)
	}
	if apiCalls == callsAfterA {
		t.Errorf("expected resolver B to make new API calls (different token = different cache key), but no additional calls were made — cross-credential cache sharing detected")
	}

	// Third call via resolver B — now should hit resolver B's own cache entry.
	callsAfterB := apiCalls
	resultB2, err := resolverB.Resolve(context.Background(), uri, ResolveOpts{})
	if err != nil {
		t.Fatalf("resolverB second Resolve failed: %v", err)
	}
	if len(resultB2.Errors) != 0 {
		t.Fatalf("resolverB second call: unexpected errors: %v", resultB2.Errors)
	}
	if apiCalls != callsAfterB {
		t.Errorf("expected resolver B's second call to hit its own cache entry, got %d additional API calls", apiCalls-callsAfterB)
	}
}

// TestGitHubPrivateRepoInstall_NoDoubleDownload is a regression test for a bug
// where the install phase made a second, unauthenticated raw.githubusercontent.com
// request to fetch file content that was already downloaded (with auth) during
// resolution. Private repos return HTTP 404 to unauthenticated raw requests, so
// the install phase would fail even though resolution succeeded.
//
// The fix (Option A): ResolvedFile.Content carries the bytes from resolution so
// the install phase writes them directly and skips the network re-download.
func TestGitHubPrivateRepoInstall_NoDoubleDownload(t *testing.T) {
	skillContent := "# Private Skill\nSecret content."
	readmeContent := "# README\nAlso secret."

	// Track raw-content requests so we can assert auth behaviour.
	var rawTotal atomic.Int32
	var rawUnauthenticated atomic.Int32

	server, mux := newTestGitHubServer(t)

	// Commits endpoint — requires auth (private repo behaviour).
	mux.HandleFunc("/repos/private-org/private-repo/commits/HEAD", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(testCommitSHA))
	})

	// Contents listing — requires auth.
	mux.HandleFunc("/repos/private-org/private-repo/contents/skills/secret-skill", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode([]githubContentEntry{
			{Name: "SKILL.md", Path: "skills/secret-skill/SKILL.md", Type: "file", Size: len(skillContent)},
			{Name: "README.md", Path: "skills/secret-skill/README.md", Type: "file", Size: len(readmeContent)},
		})
	})

	// Raw content — return 404 without auth, simulating GitHub private repo behaviour.
	// Any request here without a Bearer token is exactly the unauthenticated
	// re-download that the bug caused during the install phase.
	mux.HandleFunc("/raw/private-org/private-repo/"+testCommitSHA+"/skills/secret-skill/SKILL.md",
		func(w http.ResponseWriter, r *http.Request) {
			rawTotal.Add(1)
			if r.Header.Get("Authorization") == "" {
				rawUnauthenticated.Add(1)
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte(skillContent))
		})
	mux.HandleFunc("/raw/private-org/private-repo/"+testCommitSHA+"/skills/secret-skill/README.md",
		func(w http.ResponseWriter, r *http.Request) {
			rawTotal.Add(1)
			if r.Header.Get("Authorization") == "" {
				rawUnauthenticated.Add(1)
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte(readmeContent))
		})

	resolver := newTestGitHubResolver(server)

	// Phase 1: Resolve (authenticated — should succeed and populate f.Content).
	result, err := resolver.Resolve(context.Background(), []api.SkillReference{
		{URI: "gh://private-org/private-repo/secret-skill"},
	}, ResolveOpts{})
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if len(result.Errors) != 0 {
		t.Fatalf("unexpected resolve errors: %v", result.Errors)
	}
	if len(result.Resolved) != 1 {
		t.Fatalf("expected 1 resolved skill, got %d", len(result.Resolved))
	}

	// All resolved files must carry pre-fetched content.
	skill := result.Resolved[0]
	for _, f := range skill.Files {
		if f.Content == nil {
			t.Errorf("file %q has nil Content after Resolve; install phase would make an unauthenticated re-download", f.Path)
		}
	}

	// Record how many raw requests the resolution phase made (should be 2: one per file).
	rawAfterResolve := rawTotal.Load()

	// Phase 2: Install — must use pre-fetched content, not re-download.
	agentHome := t.TempDir()
	skillsDest := filepath.Join(agentHome, ".claude", "skills")

	_, err = installResolvedSkills(context.Background(), result.Resolved, skillsDest, agentHome)
	if err != nil {
		// If the fix is absent, this fails with "download failed with status 404"
		// because the install phase hits the mock server without a token.
		t.Fatalf("installResolvedSkills failed (install phase may have made an unauthenticated re-download): %v", err)
	}

	// Verify files are present and correct on disk.
	for _, tc := range []struct {
		path string
		want string
	}{
		{"SKILL.md", skillContent},
		{"README.md", readmeContent},
	} {
		installed := filepath.Join(skillsDest, "secret-skill", tc.path)
		data, err := os.ReadFile(installed)
		if err != nil {
			t.Fatalf("failed to read installed file %s: %v", tc.path, err)
		}
		if string(data) != tc.want {
			t.Errorf("installed %s = %q, want %q", tc.path, string(data), tc.want)
		}
	}

	// The raw endpoint must not have been hit again during install.
	rawAfterInstall := rawTotal.Load()
	if rawAfterInstall != rawAfterResolve {
		t.Errorf("install phase made %d additional raw request(s); expected 0 (content should come from ResolvedFile.Content)",
			rawAfterInstall-rawAfterResolve)
	}

	// No unauthenticated requests must have occurred at all.
	if n := rawUnauthenticated.Load(); n > 0 {
		t.Errorf("install phase made %d unauthenticated raw request(s); private-repo content would 404", n)
	}
}

// TestIsFullCommitSHA verifies the full-SHA detection helper.
func TestIsFullCommitSHA(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"full 40-char lowercase hex", "abc123def456abc123def456abc123def456abcd", true},
		{"all zeros (valid SHA)", "0000000000000000000000000000000000000000", true},
		{"uppercase hex rejected", "ABC123DEF456ABC123DEF456ABC123DEF456ABCD", false},
		{"mixed case rejected", "Abc123def456abc123def456abc123def456abcd", false},
		{"39 chars too short", "abc123def456abc123def456abc123def456abc", false},
		{"41 chars too long", "abc123def456abc123def456abc123def456abcde", false},
		{"branch name rejected", "main", false},
		{"short SHA (12 char) rejected", "abc123def456", false},
		{"empty string rejected", "", false},
		{"non-hex chars rejected", "abc123def456abc123def456abc123def456zzzz", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isFullCommitSHA(tt.in); got != tt.want {
				t.Errorf("isFullCommitSHA(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// TestResolveCommitSHA_FullSHAShortCircuit verifies that resolveCommitSHA skips
// the GitHub API entirely when the ref is already a full 40-char commit SHA.
func TestResolveCommitSHA_FullSHAShortCircuit(t *testing.T) {
	server, mux := newTestGitHubServer(t)
	apiCalled := false
	mux.HandleFunc("/repos/owner/repo/commits/", func(w http.ResponseWriter, _ *http.Request) {
		apiCalled = true
		_, _ = w.Write([]byte(testCommitSHA))
	})

	resolver := newTestGitHubResolver(server)
	ghRef := &GitHubSkillRef{
		Owner:     "owner",
		Repo:      "repo",
		SkillPath: "skills/my-skill",
		SkillName: "my-skill",
		Ref:       testCommitSHA, // already a full SHA
		Raw:       "gh://owner/repo/my-skill@" + testCommitSHA,
	}

	got, err := resolver.resolveCommitSHA(context.Background(), ghRef, "test-token")
	if err != nil {
		t.Fatalf("resolveCommitSHA failed: %v", err)
	}
	if got != testCommitSHA {
		t.Errorf("expected %s, got %s", testCommitSHA, got)
	}
	if apiCalled {
		t.Error("API should not have been called for a full-SHA ref, but it was")
	}
}

// TestResolveCommitSHA_BranchStillCallsAPI verifies that branch names (non-SHA refs)
// still trigger the GitHub API call.
func TestResolveCommitSHA_BranchStillCallsAPI(t *testing.T) {
	server, mux := newTestGitHubServer(t)
	apiCalled := false
	mux.HandleFunc("/repos/owner/repo/commits/main", func(w http.ResponseWriter, _ *http.Request) {
		apiCalled = true
		_, _ = w.Write([]byte(testCommitSHA))
	})

	resolver := newTestGitHubResolver(server)
	ghRef := &GitHubSkillRef{
		Owner:     "owner",
		Repo:      "repo",
		SkillPath: "skills/my-skill",
		SkillName: "my-skill",
		Ref:       "main",
		Raw:       "gh://owner/repo/my-skill@main",
	}

	got, err := resolver.resolveCommitSHA(context.Background(), ghRef, "test-token")
	if err != nil {
		t.Fatalf("resolveCommitSHA failed: %v", err)
	}
	if got != testCommitSHA {
		t.Errorf("expected %s, got %s", testCommitSHA, got)
	}
	if !apiCalled {
		t.Error("API should have been called for a branch-name ref, but it was not")
	}
}

// TestNewGitHubSkillResolverWithCredentials_ProvisionCredentialsFallback verifies
// that when no explicit defaultToken is provided and the broker env lacks
// GITHUB_TOKEN, the resolver falls back to a GITHUB_TOKEN entry in
// provisionCredentials. This is the auth wiring fix for bare gh:// URIs.
func TestNewGitHubSkillResolverWithCredentials_ProvisionCredentialsFallback(t *testing.T) {
	// Ensure no GITHUB_TOKEN in the process env (clear it, restore after test).
	old := os.Getenv("GITHUB_TOKEN")
	if err := os.Unsetenv("GITHUB_TOKEN"); err != nil {
		t.Fatalf("failed to unset GITHUB_TOKEN: %v", err)
	}
	t.Cleanup(func() {
		if old != "" {
			_ = os.Setenv("GITHUB_TOKEN", old)
		}
	})

	creds := map[string]string{
		"GITHUB_TOKEN": "provision-secret-token",
		"OTHER_SECRET": "other-value",
	}
	r := NewGitHubSkillResolverWithCredentials("", creds, nil)
	if r.token != "provision-secret-token" {
		t.Errorf("expected token from provisionCredentials fallback, got %q", r.token)
	}
}

// TestNewGitHubSkillResolverWithCredentials_NilProvisionCredentials verifies that
// passing nil provisionCredentials is safe when no GITHUB_TOKEN fallback is needed.
// In Go, reading from a nil map returns "" (zero value), so the fallback is a no-op.
func TestNewGitHubSkillResolverWithCredentials_NilProvisionCredentials(t *testing.T) {
	old := os.Getenv("GITHUB_TOKEN")
	if err := os.Unsetenv("GITHUB_TOKEN"); err != nil {
		t.Fatalf("failed to unset GITHUB_TOKEN: %v", err)
	}
	t.Cleanup(func() {
		if old != "" {
			_ = os.Setenv("GITHUB_TOKEN", old)
		}
	})

	// Must not panic; token should remain empty when no credential is available.
	r := NewGitHubSkillResolverWithCredentials("", nil, nil)
	if r.token != "" {
		t.Errorf("expected empty token with nil provisionCredentials and no env var, got %q", r.token)
	}
}

// TestNewGitHubSkillResolverWithCredentials_ExplicitTokenWins verifies that an
// explicit defaultToken takes precedence over any provision credential.
func TestNewGitHubSkillResolverWithCredentials_ExplicitTokenWins(t *testing.T) {
	creds := map[string]string{
		"GITHUB_TOKEN": "provision-secret-token",
	}
	r := NewGitHubSkillResolverWithCredentials("explicit-token", creds, nil)
	if r.token != "explicit-token" {
		t.Errorf("expected explicit token to win, got %q", r.token)
	}
}

// TestGitHubSkillResolver_FullSHAPinnedSkill verifies end-to-end resolution
// of a skill pinned to a full commit SHA makes no resolveCommitSHA API call.
func TestGitHubSkillResolver_FullSHAPinnedSkill(t *testing.T) {
	server, mux := newTestGitHubServer(t)
	commitEndpointCalled := false

	// Register the commit endpoint — it should NOT be called.
	mux.HandleFunc("/repos/owner/repo/commits/"+testCommitSHA, func(w http.ResponseWriter, _ *http.Request) {
		commitEndpointCalled = true
		_, _ = w.Write([]byte(testCommitSHA))
	})

	// Contents and raw endpoints are needed for the full resolution flow.
	mux.HandleFunc("/repos/owner/repo/contents/skills/my-skill", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]githubContentEntry{
			{Name: "SKILL.md", Path: "skills/my-skill/SKILL.md", Type: "file", Size: 5},
		})
	})
	mux.HandleFunc("/raw/owner/repo/"+testCommitSHA+"/skills/my-skill/SKILL.md", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("hello"))
	})

	resolver := newTestGitHubResolver(server)

	result, err := resolver.Resolve(context.Background(), []api.SkillReference{
		{URI: "gh://owner/repo/my-skill@" + testCommitSHA},
	}, ResolveOpts{})
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if len(result.Errors) != 0 {
		t.Fatalf("unexpected errors: %v", result.Errors)
	}
	if len(result.Resolved) != 1 {
		t.Fatalf("expected 1 resolved skill, got %d", len(result.Resolved))
	}
	if commitEndpointCalled {
		t.Error("commit resolution API endpoint was called for a full-SHA ref; expected short-circuit")
	}
	// Version should be the first 12 chars of the pinned SHA.
	if result.Resolved[0].Version != testCommitSHA[:12] {
		t.Errorf("expected version %s, got %s", testCommitSHA[:12], result.Resolved[0].Version)
	}
}

// TestGitHubSkillResolver_SharedCacheSingleton verifies that when a shared
// (non-nil) cache is passed to NewGitHubSkillResolverWithCredentials, two
// sequential resolve calls for the same URI produce only one underlying API
// call (second is a cache hit).
func TestGitHubSkillResolver_SharedCacheSingleton(t *testing.T) {
	server, mux := newTestGitHubServer(t)
	apiCalls := 0

	mux.HandleFunc("/repos/owner/repo/commits/main", func(w http.ResponseWriter, _ *http.Request) {
		apiCalls++
		_, _ = w.Write([]byte(testCommitSHA))
	})
	mux.HandleFunc("/repos/owner/repo/contents/skills/my-skill", func(w http.ResponseWriter, _ *http.Request) {
		apiCalls++
		_ = json.NewEncoder(w).Encode([]githubContentEntry{
			{Name: "SKILL.md", Path: "skills/my-skill/SKILL.md", Type: "file", Size: 5},
		})
	})
	mux.HandleFunc("/raw/owner/repo/"+testCommitSHA+"/skills/my-skill/SKILL.md", func(w http.ResponseWriter, _ *http.Request) {
		apiCalls++
		_, _ = w.Write([]byte("hello"))
	})

	// Create a shared cache
	cache, err := newTestResolutionCache(t.TempDir(), 5*time.Minute)
	if err != nil {
		t.Fatalf("cache creation failed: %v", err)
	}

	// Create two resolvers sharing the same cache
	resolver1 := &GitHubSkillResolver{
		httpClient:      server.Client(),
		token:           "test-token",
		apiBase:         server.URL,
		rawBase:         server.URL + "/raw",
		resolutionCache: cache,
	}

	resolver2 := &GitHubSkillResolver{
		httpClient:      server.Client(),
		token:           "test-token",
		apiBase:         server.URL,
		rawBase:         server.URL + "/raw",
		resolutionCache: cache,
	}

	uri := []api.SkillReference{{URI: "gh://owner/repo/my-skill@main"}}

	// First call via resolver1 — should hit the API
	result1, err := resolver1.Resolve(context.Background(), uri, ResolveOpts{})
	if err != nil {
		t.Fatalf("resolver1 Resolve failed: %v", err)
	}
	if len(result1.Errors) != 0 {
		t.Fatalf("resolver1: unexpected errors: %v", result1.Errors)
	}
	if len(result1.Resolved) != 1 {
		t.Fatalf("expected 1 resolved skill from resolver1, got %d", len(result1.Resolved))
	}
	firstCallAPICalls := apiCalls

	// Second call via resolver2 (different resolver instance, same cache) — should use cache
	result2, err := resolver2.Resolve(context.Background(), uri, ResolveOpts{})
	if err != nil {
		t.Fatalf("resolver2 Resolve failed: %v", err)
	}
	if len(result2.Errors) != 0 {
		t.Fatalf("resolver2: unexpected errors: %v", result2.Errors)
	}
	if len(result2.Resolved) != 1 {
		t.Fatalf("expected 1 resolved skill from resolver2, got %d", len(result2.Resolved))
	}

	// Verify no new API calls were made (cache hit)
	if apiCalls != firstCallAPICalls {
		t.Errorf("expected no new API calls on cache hit from shared cache, but got %d additional calls", apiCalls-firstCallAPICalls)
	}

	if result2.Resolved[0].Name != result1.Resolved[0].Name {
		t.Errorf("cached result name mismatch: %s vs %s", result2.Resolved[0].Name, result1.Resolved[0].Name)
	}
}

// TestNormalizeGitHubName verifies the character normalization used in
// convention-based key derivation.
func TestNormalizeGitHubName(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"scion-frontiers", "SCION_FRONTIERS"},
		{"scion-repo-contrib", "SCION_REPO_CONTRIB"},
		{"GoogleCloudPlatform", "GOOGLECLOUDPLATFORM"},
		{"ptone", "PTONE"},
		{"my-org", "MY_ORG"},
		{"my.repo", "MY_REPO"},
		{"my_repo", "MY_REPO"},
		{"my.special.repo", "MY_SPECIAL_REPO"},
		{"a-b.c_d", "A_B_C_D"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got := normalizeGitHubName(tt.in)
			if got != tt.want {
				t.Errorf("normalizeGitHubName(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestDeriveGitHubTokenKey verifies the convention-based key derivation
// for repo-specific credentials (GH_OWNER__REPO).
func TestDeriveGitHubTokenKey(t *testing.T) {
	tests := []struct {
		owner, repo string
		want        string
	}{
		{"scion-frontiers", "scion-repo-contrib", "GH_SCION_FRONTIERS__SCION_REPO_CONTRIB"},
		{"GoogleCloudPlatform", "scion", "GH_GOOGLECLOUDPLATFORM__SCION"},
		{"ptone", "scion", "GH_PTONE__SCION"},
		{"my-org", "my.repo", "GH_MY_ORG__MY_REPO"},
		{"my-org", "my.special.repo", "GH_MY_ORG__MY_SPECIAL_REPO"},
	}
	for _, tt := range tests {
		name := tt.owner + "/" + tt.repo
		t.Run(name, func(t *testing.T) {
			got := deriveGitHubTokenKey(tt.owner, tt.repo)
			if got != tt.want {
				t.Errorf("deriveGitHubTokenKey(%q, %q) = %q, want %q", tt.owner, tt.repo, got, tt.want)
			}
		})
	}
}

// TestDeriveGitHubOwnerKey verifies the convention-based key derivation
// for owner-level credentials (GH_OWNER).
func TestDeriveGitHubOwnerKey(t *testing.T) {
	tests := []struct {
		owner string
		want  string
	}{
		{"scion-frontiers", "GH_SCION_FRONTIERS"},
		{"GoogleCloudPlatform", "GH_GOOGLECLOUDPLATFORM"},
		{"ptone", "GH_PTONE"},
		{"my-org", "GH_MY_ORG"},
	}
	for _, tt := range tests {
		t.Run(tt.owner, func(t *testing.T) {
			got := deriveGitHubOwnerKey(tt.owner)
			if got != tt.want {
				t.Errorf("deriveGitHubOwnerKey(%q) = %q, want %q", tt.owner, got, tt.want)
			}
		})
	}
}

// TestTokenForRef_ConventionKeys verifies the updated tokenForRef resolution
// precedence including convention-based key lookup.
func TestTokenForRef_ConventionKeys(t *testing.T) {
	t.Run("explicit token wins over convention key", func(t *testing.T) {
		r := &GitHubSkillResolver{
			token: "default-token",
			provisionCredentials: map[string]string{
				"MY_EXPLICIT":    "explicit-value",
				"GH_OWNER__REPO": "convention-repo-value",
				"GH_OWNER":       "convention-owner-value",
			},
		}
		ref := &GitHubSkillRef{
			Owner:           "owner",
			Repo:            "repo",
			TokenSecretName: "MY_EXPLICIT",
		}
		got, err := r.tokenForRef(ref)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "explicit-value" {
			t.Errorf("expected explicit-value, got %q", got)
		}
	})

	t.Run("repo-specific convention key wins over owner-level", func(t *testing.T) {
		r := &GitHubSkillResolver{
			token: "default-token",
			provisionCredentials: map[string]string{
				"GH_SCION_FRONTIERS__SCION_REPO_CONTRIB": "repo-specific-token",
				"GH_SCION_FRONTIERS":                     "owner-level-token",
			},
		}
		ref := &GitHubSkillRef{
			Owner: "scion-frontiers",
			Repo:  "scion-repo-contrib",
		}
		got, err := r.tokenForRef(ref)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "repo-specific-token" {
			t.Errorf("expected repo-specific-token, got %q", got)
		}
	})

	t.Run("owner-level key used when no repo-specific key", func(t *testing.T) {
		r := &GitHubSkillResolver{
			token: "default-token",
			provisionCredentials: map[string]string{
				"GH_SCION_FRONTIERS": "owner-level-token",
			},
		}
		ref := &GitHubSkillRef{
			Owner: "scion-frontiers",
			Repo:  "any-repo",
		}
		got, err := r.tokenForRef(ref)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "owner-level-token" {
			t.Errorf("expected owner-level-token, got %q", got)
		}
	})

	t.Run("default token used when no convention keys exist", func(t *testing.T) {
		r := &GitHubSkillResolver{
			token:                "default-token",
			provisionCredentials: map[string]string{},
		}
		ref := &GitHubSkillRef{
			Owner: "some-owner",
			Repo:  "some-repo",
		}
		got, err := r.tokenForRef(ref)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "default-token" {
			t.Errorf("expected default-token, got %q", got)
		}
	})

	t.Run("empty token returned when no credentials at all", func(t *testing.T) {
		r := &GitHubSkillResolver{
			token:                "",
			provisionCredentials: map[string]string{},
		}
		ref := &GitHubSkillRef{
			Owner: "some-owner",
			Repo:  "some-repo",
		}
		got, err := r.tokenForRef(ref)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "" {
			t.Errorf("expected empty token, got %q", got)
		}
	})

	t.Run("nil provisionCredentials falls through to default", func(t *testing.T) {
		r := &GitHubSkillResolver{
			token:                "default-token",
			provisionCredentials: nil,
		}
		ref := &GitHubSkillRef{
			Owner: "some-owner",
			Repo:  "some-repo",
		}
		got, err := r.tokenForRef(ref)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "default-token" {
			t.Errorf("expected default-token, got %q", got)
		}
	})

	t.Run("missing convention key is silent not an error", func(t *testing.T) {
		// When no convention key exists, no error should be returned —
		// the resolver silently falls through to the default token.
		r := &GitHubSkillResolver{
			token: "default-token",
			provisionCredentials: map[string]string{
				"UNRELATED_SECRET": "unrelated-value",
			},
		}
		ref := &GitHubSkillRef{
			Owner: "nonexistent-owner",
			Repo:  "nonexistent-repo",
		}
		got, err := r.tokenForRef(ref)
		if err != nil {
			t.Fatalf("missing convention key should not produce an error, got: %v", err)
		}
		if got != "default-token" {
			t.Errorf("expected default-token fallback, got %q", got)
		}
	})
}

// TestTokenForRef_ConventionKeyIntegration verifies that convention-based
// credential selection works end-to-end through the Resolve method.
func TestTokenForRef_ConventionKeyIntegration(t *testing.T) {
	server, mux := newTestGitHubServer(t)

	var gotAuth string
	mux.HandleFunc("/repos/scion-frontiers/scion-repo-contrib/commits/HEAD", func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(testCommitSHA))
	})
	mux.HandleFunc("/repos/scion-frontiers/scion-repo-contrib/contents/skills/my-skill", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]githubContentEntry{
			{Name: "SKILL.md", Path: "skills/my-skill/SKILL.md", Type: "file", Size: 5},
		})
	})
	mux.HandleFunc("/raw/scion-frontiers/scion-repo-contrib/"+testCommitSHA+"/skills/my-skill/SKILL.md", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("hello"))
	})

	resolver := &GitHubSkillResolver{
		httpClient: server.Client(),
		token:      "default-token",
		apiBase:    server.URL,
		rawBase:    server.URL + "/raw",
		provisionCredentials: map[string]string{
			"GH_SCION_FRONTIERS__SCION_REPO_CONTRIB": "convention-repo-token",
		},
	}

	result, err := resolver.Resolve(context.Background(), []api.SkillReference{
		{URI: "gh://scion-frontiers/scion-repo-contrib/my-skill"},
	}, ResolveOpts{})

	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if len(result.Errors) != 0 {
		t.Fatalf("unexpected errors: %v", result.Errors)
	}
	if gotAuth != "Bearer convention-repo-token" {
		t.Errorf("expected convention-repo-token to be used, got Authorization: %q", gotAuth)
	}
}

// TestTokenForRef_OwnerKeyIntegration verifies owner-level convention key
// resolution works end-to-end through the Resolve method.
func TestTokenForRef_OwnerKeyIntegration(t *testing.T) {
	server, mux := newTestGitHubServer(t)

	var gotAuth string
	mux.HandleFunc("/repos/scion-frontiers/other-repo/commits/HEAD", func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(testCommitSHA))
	})
	mux.HandleFunc("/repos/scion-frontiers/other-repo/contents/skills/my-skill", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]githubContentEntry{
			{Name: "SKILL.md", Path: "skills/my-skill/SKILL.md", Type: "file", Size: 5},
		})
	})
	mux.HandleFunc("/raw/scion-frontiers/other-repo/"+testCommitSHA+"/skills/my-skill/SKILL.md", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("hello"))
	})

	resolver := &GitHubSkillResolver{
		httpClient: server.Client(),
		token:      "default-token",
		apiBase:    server.URL,
		rawBase:    server.URL + "/raw",
		provisionCredentials: map[string]string{
			"GH_SCION_FRONTIERS": "owner-level-token",
		},
	}

	result, err := resolver.Resolve(context.Background(), []api.SkillReference{
		{URI: "gh://scion-frontiers/other-repo/my-skill"},
	}, ResolveOpts{})

	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if len(result.Errors) != 0 {
		t.Fatalf("unexpected errors: %v", result.Errors)
	}
	if gotAuth != "Bearer owner-level-token" {
		t.Errorf("expected owner-level-token to be used, got Authorization: %q", gotAuth)
	}
}

// TestTokenForRef_ExplicitTokenWinsOverConvention verifies that an explicit
// ?token= parameter on the URI takes precedence over convention keys.
func TestTokenForRef_ExplicitTokenWinsOverConvention(t *testing.T) {
	server, mux := newTestGitHubServer(t)

	var gotAuth string
	mux.HandleFunc("/repos/scion-frontiers/scion-repo-contrib/commits/HEAD", func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(testCommitSHA))
	})
	mux.HandleFunc("/repos/scion-frontiers/scion-repo-contrib/contents/skills/my-skill", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]githubContentEntry{
			{Name: "SKILL.md", Path: "skills/my-skill/SKILL.md", Type: "file", Size: 5},
		})
	})
	mux.HandleFunc("/raw/scion-frontiers/scion-repo-contrib/"+testCommitSHA+"/skills/my-skill/SKILL.md", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("hello"))
	})

	resolver := &GitHubSkillResolver{
		httpClient: server.Client(),
		token:      "default-token",
		apiBase:    server.URL,
		rawBase:    server.URL + "/raw",
		provisionCredentials: map[string]string{
			"MY_EXPLICIT_TOKEN":                      "explicit-token-value",
			"GH_SCION_FRONTIERS__SCION_REPO_CONTRIB": "convention-token-value",
			"GH_SCION_FRONTIERS":                     "owner-token-value",
		},
	}

	result, err := resolver.Resolve(context.Background(), []api.SkillReference{
		{URI: "gh://scion-frontiers/scion-repo-contrib/my-skill?token=MY_EXPLICIT_TOKEN"},
	}, ResolveOpts{})

	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if len(result.Errors) != 0 {
		t.Fatalf("unexpected errors: %v", result.Errors)
	}
	if gotAuth != "Bearer explicit-token-value" {
		t.Errorf("expected explicit ?token= to win over convention key, got Authorization: %q", gotAuth)
	}
}

// TestTokenForRef_CLILocalMode verifies that CLI local mode (nil
// ProvisionCredentials) continues to work — convention key lookup on a nil
// map is safe and falls through to the default token from os.Getenv.
func TestTokenForRef_CLILocalMode(t *testing.T) {
	r := &GitHubSkillResolver{
		token:                "env-github-token",
		provisionCredentials: nil, // CLI local mode: no ProvisionCredentials
	}
	ref := &GitHubSkillRef{
		Owner: "scion-frontiers",
		Repo:  "scion-repo-contrib",
	}
	got, err := r.tokenForRef(ref)
	if err != nil {
		t.Fatalf("CLI local mode should not produce an error, got: %v", err)
	}
	if got != "env-github-token" {
		t.Errorf("expected env-github-token (default), got %q", got)
	}
}

type stubSkillResolver struct {
	result *ResolveResult
}

func (s *stubSkillResolver) ResolverName() string { return "stub" }
func (s *stubSkillResolver) Resolve(_ context.Context, _ []api.SkillReference, _ ResolveOpts) (*ResolveResult, error) {
	return s.result, nil
}

// serveSkillWithRawResponder wires the commit and contents endpoints for
// gh://owner/repo/my-skill@main to succeed, and hands the raw SKILL.md
// download to raw, so tests can script the download path alone.
func serveSkillWithRawResponder(mux *http.ServeMux, raw http.HandlerFunc) {
	mux.HandleFunc("/repos/owner/repo/commits/main", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(testCommitSHA))
	})
	mux.HandleFunc("/repos/owner/repo/contents/skills/my-skill", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]githubContentEntry{
			{Name: "SKILL.md", Path: "skills/my-skill/SKILL.md", Type: "file", Size: 5},
		})
	})
	mux.HandleFunc("/raw/owner/repo/"+testCommitSHA+"/skills/my-skill/SKILL.md", raw)
}

// TestGitHubSkillResolver_RawDownloadTransient_RetriesWithinDefaultBudget
// proves a single transient 503 on a raw download is retried and succeeds
// under the default budget with a no-deadline ctx (the production shape). A
// 429 is not retried: it starts a cooldown and fails at once as rate_limited,
// with the cooldown's remaining time as RetryAfter. The budget-fit check must reserve only the stall bound for the
// next attempt: reserving the full download timeout, which equals the
// default budget, made every raw-download retry fail fast (#2546 RQ1).
func TestGitHubSkillResolver_RawDownloadTransient_RetriesWithinDefaultBudget(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      int
		retryAfter  string
		rateLimited bool
	}{
		{"503", http.StatusServiceUnavailable, "", false},
		{"429 with short Retry-After", http.StatusTooManyRequests, "1", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, mux := newTestGitHubServer(t)
			var downloads int32
			serveSkillWithRawResponder(mux, func(w http.ResponseWriter, _ *http.Request) {
				if atomic.AddInt32(&downloads, 1) == 1 {
					if tc.retryAfter != "" {
						w.Header().Set("Retry-After", tc.retryAfter)
					}
					w.WriteHeader(tc.status)
					return
				}
				_, _ = w.Write([]byte("hello"))
			})
			resolver := newTestGitHubResolver(server)

			start := time.Now()
			result, err := resolver.Resolve(context.Background(), []api.SkillReference{
				{URI: "gh://owner/repo/my-skill@main"},
			}, ResolveOpts{})
			elapsed := time.Since(start)

			if err != nil {
				t.Fatalf("Resolve failed: %v", err)
			}
			if tc.rateLimited {
				if len(result.Errors) != 1 || result.Errors[0].Code != SkillErrCodeRateLimited || result.Errors[0].RetryAfter != tc.retryAfter {
					t.Fatalf("expected one rate_limited error with RetryAfter %q, got %+v", tc.retryAfter, result.Errors)
				}
				if got := atomic.LoadInt32(&downloads); got != 1 {
					t.Errorf("expected 1 download attempt, got %d", got)
				}
				return
			}
			if len(result.Errors) != 0 {
				t.Fatalf("expected the transient download failure to be retried, got errors: %+v", result.Errors)
			}
			if got := atomic.LoadInt32(&downloads); got != 2 {
				t.Errorf("expected 2 download attempts, got %d", got)
			}
			if elapsed >= githubResolveBudget {
				t.Errorf("expected success within the %s budget, took %s", githubResolveBudget, elapsed)
			}
		})
	}
}

// TestGitHubSkillResolver_RetryAfterAboveCap_FailsFastWithoutBudgetPressure
// pins the Retry-After-above-cap fail-fast on its own: the ctx deadline is
// long enough that the budget-fit check would allow the capped 30s backoff,
// so only the cap check can end the call at the first attempt (#2546 RQ3).
// If that check is removed, the resolver sleeps the capped backoff and the
// watchdog below cancels it, failing the test in seconds rather than minutes.
// It uses a 503: a 429 would be ended by the rate-limit cooldown before the
// cap check runs.
func TestGitHubSkillResolver_RetryAfterAboveCap_FailsFastWithoutBudgetPressure(t *testing.T) {
	server, mux := newTestGitHubServer(t)

	var attempts int32
	mux.HandleFunc("/repos/owner/repo/commits/main", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.Header().Set("Retry-After", "60") // above githubMaxBackoff (30s)
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	resolver := newTestGitHubResolver(server)

	// A 2-minute deadline: 30s capped backoff + the next attempt fits.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	// Cancelling does not change ctx.Deadline(), so the budget check still
	// sees the full 2 minutes; this only bounds a regression's runtime.
	watchdog := time.AfterFunc(3*time.Second, cancel)
	defer watchdog.Stop()

	start := time.Now()
	result, err := resolver.Resolve(ctx, []api.SkillReference{
		{URI: "gh://owner/repo/my-skill@main"},
	}, ResolveOpts{})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("expected 1 error, got %d", len(result.Errors))
	}
	if elapsed >= time.Second {
		t.Errorf("expected an immediate fail-fast, took %s", elapsed)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("expected exactly 1 attempt, got %d", got)
	}
	e := result.Errors[0]
	if e.Code != SkillErrCodeUpstreamUnavailable {
		t.Errorf("expected code %s, got %s (%s)", SkillErrCodeUpstreamUnavailable, e.Code, e.Message)
	}
	if e.RetryAfter != "60" {
		t.Errorf("expected RetryAfter 60, got %q", e.RetryAfter)
	}
	if !strings.Contains(e.Message, "past the 30s backoff cap") {
		t.Errorf("expected the cap fail-fast message, got %q", e.Message)
	}
}

// TestGitHubSkillResolver_5xxBackoffPastDeadline_FailsFastWithinBudget pins
// the budget-fit fail-fast for a response (not a network error): a 503 whose
// Retry-After is under the backoff cap but past the ctx deadline fails at the
// first attempt with upstream_unavailable naming the ref. Without the check
// the resolver would sleep the backoff into the deadline.
func TestGitHubSkillResolver_5xxBackoffPastDeadline_FailsFastWithinBudget(t *testing.T) {
	server, mux := newTestGitHubServer(t)

	var attempts int32
	mux.HandleFunc("/repos/owner/repo/commits/main", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.Header().Set("Retry-After", "20") // under githubMaxBackoff, past the deadline
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	resolver := newTestGitHubResolver(server)

	const budget = 3 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	start := time.Now()
	result, err := resolver.Resolve(ctx, []api.SkillReference{
		{URI: "gh://owner/repo/my-skill@main"},
	}, ResolveOpts{})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("expected 1 error, got %d", len(result.Errors))
	}
	if elapsed >= time.Second {
		t.Errorf("expected an immediate fail-fast, took %s", elapsed)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("expected exactly 1 attempt, got %d", got)
	}
	e := result.Errors[0]
	if e.Code != SkillErrCodeUpstreamUnavailable {
		t.Errorf("expected code %s, got %s (%s)", SkillErrCodeUpstreamUnavailable, e.Code, e.Message)
	}
	if !strings.Contains(e.Message, "gh://owner/repo/my-skill@main") ||
		!strings.Contains(e.Message, "would exceed the") {
		t.Errorf("expected a budget fail-fast message naming the ref, got %q", e.Message)
	}
}

// TestGitHubSkillResolver_RawDownloadBodyTimeout_ClassifiedAsTimeout proves
// that a raw download cut off mid-body by the resolve budget is reported as
// a timeout naming the file, not as an unclassified resolve_failed (#2546 O1).
func TestGitHubSkillResolver_RawDownloadBodyTimeout_ClassifiedAsTimeout(t *testing.T) {
	server, mux := newTestGitHubServer(t)

	mux.HandleFunc("/repos/owner/repo/commits/main", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(testCommitSHA))
	})
	mux.HandleFunc("/repos/owner/repo/contents/skills/my-skill", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]githubContentEntry{
			{Name: "SKILL.md", Path: "skills/my-skill/SKILL.md", Type: "file", Size: 1024},
		})
	})
	mux.HandleFunc("/raw/owner/repo/"+testCommitSHA+"/skills/my-skill/SKILL.md", func(w http.ResponseWriter, r *http.Request) {
		// Send headers and a first byte promptly, then trickle the rest
		// slower than the budget allows.
		w.Header().Set("Content-Length", "1024")
		w.WriteHeader(http.StatusOK)
		for i := 0; i < 1024; i++ {
			if _, err := w.Write([]byte("x")); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
	})

	resolver := newTestGitHubResolver(server)
	resolver.resolveBudget = 300 * time.Millisecond

	result, err := resolver.Resolve(context.Background(), []api.SkillReference{
		{URI: "gh://owner/repo/my-skill@main"},
	}, ResolveOpts{})
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("expected 1 error, got %d", len(result.Errors))
	}
	if result.Errors[0].Code != SkillErrCodeTimeout {
		t.Errorf("expected code %s, got %s (message: %s)", SkillErrCodeTimeout, result.Errors[0].Code, result.Errors[0].Message)
	}
	if !strings.Contains(result.Errors[0].Message, "failed to read skills/my-skill/SKILL.md") {
		t.Errorf("expected error to name the file path in the read failure, got %s", result.Errors[0].Message)
	}
}

func TestGitHubSkillResolver_PreferFallback(t *testing.T) {
	cases := []struct {
		name    string
		uri     string
		creds   map[string]string
		prefers bool
	}{
		{"explicit ?token= param", "gh://owner/repo/skill?token=MY_TOKEN", nil, true},
		{"repo convention credential present", "gh://owner/repo/skill", map[string]string{"GH_OWNER__REPO": "x"}, true},
		{"owner convention credential present", "gh://owner/repo/skill", map[string]string{"GH_OWNER": "x"}, true},
		{"no credential override, default token", "gh://owner/repo/skill", nil, false},
		{"unrelated credential present routes through the primary", "gh://owner/repo/skill", map[string]string{"GH_OTHER": "x"}, false},
		{"empty-value credential routes through the primary", "gh://owner/repo/skill", map[string]string{"GH_OWNER": ""}, false},
		{"invalid URI routes through the primary", "not-a-gh-uri", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &GitHubSkillResolver{provisionCredentials: tc.creds}
			got := r.PreferFallback(api.SkillReference{URI: tc.uri})
			if got != tc.prefers {
				t.Errorf("PreferFallback(%q) = %v, want %v", tc.uri, got, tc.prefers)
			}
		})
	}
}

// failIfCalledResolver fails the test immediately if Resolve is ever called —
// used to prove a hub-unservable ref never reaches the primary resolver.
type failIfCalledResolver struct {
	t *testing.T
}

func (f *failIfCalledResolver) ResolverName() string { return "hub" }
func (f *failIfCalledResolver) Resolve(_ context.Context, refs []api.SkillReference, _ ResolveOpts) (*ResolveResult, error) {
	f.t.Fatalf("primary (hub) resolver must not be called for a hub-unservable ref; got refs: %+v", refs)
	return nil, nil
}

// TestGitHubSkillResolver_RouteFilter_CredentialedRefRoutesDirectlyToFallback
// is the end-to-end acceptance test for R: a ref that needs a credential the
// Hub cannot hold (here, a GH_OWNER convention credential) must be routed
// directly to the local GitHub resolver through RoutingSkillResolver, making
// zero calls to the primary (hub) resolver.
func TestGitHubSkillResolver_RouteFilter_CredentialedRefRoutesDirectlyToFallback(t *testing.T) {
	server, mux := newTestGitHubServer(t)

	mux.HandleFunc("/repos/owner/repo/commits/main", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(testCommitSHA))
	})
	mux.HandleFunc("/repos/owner/repo/contents/skills/my-skill", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]githubContentEntry{
			{Name: "SKILL.md", Path: "skills/my-skill/SKILL.md", Type: "file", Size: 5},
		})
	})
	mux.HandleFunc("/raw/owner/repo/"+testCommitSHA+"/skills/my-skill/SKILL.md", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("hello"))
	})

	gh := &GitHubSkillResolver{
		httpClient: server.Client(),
		apiBase:    server.URL,
		rawBase:    server.URL + "/raw",
		provisionCredentials: map[string]string{
			"GH_OWNER": "owner-level-secret",
		},
	}

	router := NewRoutingSkillResolver(&failIfCalledResolver{t: t})
	router.RegisterFallback("gh", gh)

	result, err := router.Resolve(context.Background(), []api.SkillReference{
		{URI: "gh://owner/repo/my-skill@main"},
	}, ResolveOpts{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Errors) != 0 {
		t.Fatalf("unexpected errors: %+v", result.Errors)
	}
	if len(result.Resolved) != 1 {
		t.Fatalf("expected 1 resolved skill, got %d", len(result.Resolved))
	}
}

// TestGitHubSkillResolver_CrossProjectCredentialIsolation is the permanent
// regression test for the cross-project flight-merge defect: two projects
// that both define a same-named convention credential (GH_ACME), with
// different values and different repo access, must never share a
// single-flight result. Project B must never receive content fetched with
// project A's credential.
//
// Before cache-key (flight identity) scoping was fixed to include
// ResolveOpts.ProjectID, both projects' resolvers computed the identical,
// unscoped flight identity "default" for this ref, so project B's resolve
// joined project A's in-flight fetch and was served project A's result
// (including file Content) — regardless of B's own credential having no
// access to the repo.
func TestGitHubSkillResolver_CrossProjectCredentialIsolation(t *testing.T) {
	server, mux := newTestGitHubServer(t)

	entered := make(chan struct{})
	var enterOnce sync.Once
	proceed := make(chan struct{})
	var proceedOnce sync.Once
	closeProceed := func() { proceedOnce.Do(func() { close(proceed) }) }
	// Registered after newTestGitHubServer's own t.Cleanup(server.Close), so
	// this runs first (cleanups run in reverse order): releasing any blocked
	// handler before the server tries to close keeps a regression (project B
	// incorrectly coalesced, so proceed is the only thing unblocking it) from
	// turning into a hung Close() instead of a clean test failure.
	t.Cleanup(closeProceed)

	// Only secret-A (project A's credential) is accepted; secret-B (project
	// B's) gets a 404, exactly as it would against the real GitHub API for a
	// repo it has no access to.
	authOK := func(r *http.Request) bool { return r.Header.Get("Authorization") == "Bearer secret-A" }
	mux.HandleFunc("/repos/acme/private/commits/main", func(w http.ResponseWriter, r *http.Request) {
		if !authOK(r) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		enterOnce.Do(func() { close(entered) })
		<-proceed
		_, _ = w.Write([]byte(testCommitSHA))
	})
	mux.HandleFunc("/repos/acme/private/contents/skills/s", func(w http.ResponseWriter, r *http.Request) {
		if !authOK(r) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode([]githubContentEntry{
			{Name: "SKILL.md", Path: "skills/s/SKILL.md", Type: "file", Size: 6},
		})
	})
	mux.HandleFunc("/raw/acme/private/"+testCommitSHA+"/skills/s/SKILL.md", func(w http.ResponseWriter, r *http.Request) {
		if !authOK(r) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte("SECRET"))
	})

	// A single shared cache, exactly as the broker wires it (one
	// GitHubResolutionCache singleton serving every project).
	cache, err := newTestResolutionCache(t.TempDir(), time.Minute)
	if err != nil {
		t.Fatalf("NewGitHubResolutionCache: %v", err)
	}

	mk := func(tok string) *GitHubSkillResolver {
		r := newTestGitHubResolver(server)
		r.token = ""
		r.provisionCredentials = map[string]string{"GH_ACME": tok}
		r.resolutionCache = cache
		return r
	}
	resolverA, resolverB := mk("secret-A"), mk("secret-B")
	ref := api.SkillReference{URI: "gh://acme/private/s@main"}

	var wg sync.WaitGroup
	var resultA *ResolveResult
	wg.Add(1)
	go func() {
		defer wg.Done()
		resultA, _ = resolverA.Resolve(context.Background(), []api.SkillReference{ref}, ResolveOpts{ProjectID: "project-A"})
	}()

	<-entered // project A's fetch has started and is blocked on proceed

	var resultB *ResolveResult
	doneB := make(chan struct{})
	go func() {
		resultB, _ = resolverB.Resolve(context.Background(), []api.SkillReference{ref}, ResolveOpts{ProjectID: "project-B"})
		close(doneB)
	}()

	// With project scoping, B's flight is independent of A's: its commits
	// call fails authOK immediately (404), with no need to wait on proceed at
	// all. If B were instead coalesced into A's flight (the defect), this
	// would hang until proceed is closed below — the select gives that
	// failure mode a clean, bounded failure instead of a test-binary hang.
	select {
	case <-doneB:
	case <-time.After(5 * time.Second):
		t.Fatal("project B's resolve did not return independently of project A's in-flight fetch — it may have been incorrectly coalesced")
	}

	closeProceed()
	wgDone := make(chan struct{})
	go func() { wg.Wait(); close(wgDone) }()
	select {
	case <-wgDone:
	case <-time.After(5 * time.Second):
		t.Fatal("project A's resolve did not complete after proceed was closed")
	}

	if len(resultB.Resolved) != 0 {
		t.Fatalf("project B must never receive content resolved with project A's credential, got: %+v", resultB.Resolved[0])
	}
	if len(resultB.Errors) != 1 {
		t.Fatalf("expected project B to get its own resolve error, got %d errors and %d resolved", len(resultB.Errors), len(resultB.Resolved))
	}

	if len(resultA.Resolved) != 1 {
		t.Fatalf("expected project A to resolve successfully, got errors: %+v", resultA.Errors)
	}
	if got := string(resultA.Resolved[0].Files[0].Content); got != "SECRET" {
		t.Errorf("expected project A's content %q, got %q", "SECRET", got)
	}
}

// TestGitHubSkillResolver_SameProjectDifferentUserCredentialIsolation is the
// permanent regression test for the within-project user-credential merge
// defect: two users of the *same* project, each with their own personal
// default GITHUB_TOKEN — the dispatcher falls back to the creating user's own
// profile-level GITHUB_TOKEN when the project itself has none (see
// httpdispatcher.go) — must never share a single-flight result under the
// "default" credential source. User B must never receive content resolved
// with user A's token. Project scoping alone (without also scoping by
// UserID for the default source) is not enough to prevent this: both users
// share one project, so only the user scope tells them apart.
func TestGitHubSkillResolver_SameProjectDifferentUserCredentialIsolation(t *testing.T) {
	server, mux := newTestGitHubServer(t)

	entered := make(chan struct{})
	var enterOnce sync.Once
	proceed := make(chan struct{})
	var proceedOnce sync.Once
	closeProceed := func() { proceedOnce.Do(func() { close(proceed) }) }
	t.Cleanup(closeProceed)

	// Only user A's personal token is accepted; user B's gets a 404, exactly
	// as it would against the real GitHub API for a repo B has no access to.
	authOK := func(r *http.Request) bool { return r.Header.Get("Authorization") == "Bearer pat-user-A" }
	mux.HandleFunc("/repos/acme/private/commits/main", func(w http.ResponseWriter, r *http.Request) {
		if !authOK(r) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		enterOnce.Do(func() { close(entered) })
		<-proceed
		_, _ = w.Write([]byte(testCommitSHA))
	})
	mux.HandleFunc("/repos/acme/private/contents/skills/s", func(w http.ResponseWriter, r *http.Request) {
		if !authOK(r) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode([]githubContentEntry{
			{Name: "SKILL.md", Path: "skills/s/SKILL.md", Type: "file", Size: 6},
		})
	})
	mux.HandleFunc("/raw/acme/private/"+testCommitSHA+"/skills/s/SKILL.md", func(w http.ResponseWriter, r *http.Request) {
		if !authOK(r) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte("SECRET"))
	})

	// A single shared cache, exactly as the broker wires it.
	cache, err := newTestResolutionCache(t.TempDir(), time.Minute)
	if err != nil {
		t.Fatalf("NewGitHubResolutionCache: %v", err)
	}

	mk := func(tok string) *GitHubSkillResolver {
		r := newTestGitHubResolver(server)
		r.token = tok // simulates the dispatcher's per-user fallback GITHUB_TOKEN
		r.provisionCredentials = nil
		r.resolutionCache = cache
		return r
	}
	resolverA, resolverB := mk("pat-user-A"), mk("pat-user-B")
	ref := api.SkillReference{URI: "gh://acme/private/s@main"}

	var wg sync.WaitGroup
	var resultA *ResolveResult
	wg.Add(1)
	go func() {
		defer wg.Done()
		resultA, _ = resolverA.Resolve(context.Background(), []api.SkillReference{ref}, ResolveOpts{ProjectID: "project-1", UserID: "user-A"})
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("user A's fetch never started")
	}

	var resultB *ResolveResult
	doneB := make(chan struct{})
	go func() {
		resultB, _ = resolverB.Resolve(context.Background(), []api.SkillReference{ref}, ResolveOpts{ProjectID: "project-1", UserID: "user-B"})
		close(doneB)
	}()

	// With user scoping on the default source, B's flight is independent of
	// A's: its commits call fails authOK immediately (404), with no need to
	// wait on proceed at all. If B were instead coalesced into A's flight
	// (the defect), this would hang until proceed is closed below — the
	// select gives that failure mode a clean, bounded failure instead of a
	// test-binary hang.
	select {
	case <-doneB:
	case <-time.After(5 * time.Second):
		t.Fatal("user B's resolve did not return independently of user A's in-flight fetch — it may have been incorrectly coalesced")
	}

	closeProceed()
	wgDone := make(chan struct{})
	go func() { wg.Wait(); close(wgDone) }()
	select {
	case <-wgDone:
	case <-time.After(5 * time.Second):
		t.Fatal("user A's resolve did not complete after proceed was closed")
	}

	if len(resultB.Resolved) != 0 {
		t.Fatalf("user B must never receive content resolved with user A's token, got: %+v", resultB.Resolved[0])
	}
	if len(resultB.Errors) != 1 {
		t.Fatalf("expected user B to get its own resolve error, got %d errors and %d resolved", len(resultB.Errors), len(resultB.Resolved))
	}

	if len(resultA.Resolved) != 1 {
		t.Fatalf("expected user A to resolve successfully, got errors: %+v", resultA.Errors)
	}
	if got := string(resultA.Resolved[0].Files[0].Content); got != "SECRET" {
		t.Errorf("expected user A's content %q, got %q", "SECRET", got)
	}
}

// TestGitHubSkillResolver_SameProjectSameUserDifferentTokenIsolation is the
// permanent regression test for merging two different credentials within the
// same project *and* the same user — for example two agents, each carrying
// its own GITHUB_TOKEN set directly on its own applied config or template,
// with the same project and the same creating user. Project and user scoping
// alone cannot separate these; only hashing the credential's own value can.
// A third subtest covers the same thing under a *named* source (a GH_*
// convention key), where both callers resolve the same key name to a
// different value — scoping by project, user and source name alone would
// still merge these, since none of those three differs between them.
//
// Both directions are covered as independent subtests for the default
// source: the agent without repo access must never receive content resolved
// with the other agent's token, and the agent with access must never be
// failed merely because a less-privileged agent happened to lead the flight.
//
// Both requests' commits calls block until released, regardless of outcome,
// so a merged (buggy) identity and a correctly-separated one are
// distinguishable deterministically: flightJoinHook reports how many
// *distinct* flight keys were touched before release. Under the bug both
// requests share one key (the hook never reports 2); under the fix each
// credential gets its own.
func TestGitHubSkillResolver_SameProjectSameUserDifferentTokenIsolation(t *testing.T) {
	cases := []struct {
		name                 string
		leaderTok, waiterTok string
		waiterShouldSucceed  bool
		// namedSource, when true, routes both callers through the same named
		// convention-key source (credentialSource returns one non-empty
		// value for both) rather than the default GITHUB_TOKEN cascade —
		// pinning that the credential-value fingerprint also separates two
		// different values presented under one named key, not only the
		// default source (see flightIdentity's named-source branch).
		namedSource bool
	}{
		{"leader has access, waiter does not", "tok-agent-A", "tok-agent-B", false, false},
		{"leader does not have access, waiter does", "tok-agent-B", "tok-agent-A", true, false},
		{"named source, same key name, different values, leader has access", "tok-agent-A", "tok-agent-B", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server, mux := newTestGitHubServer(t)
			authOK := func(r *http.Request) bool { return r.Header.Get("Authorization") == "Bearer tok-agent-A" }

			proceed := make(chan struct{})
			var proceedOnce sync.Once
			closeProceed := func() { proceedOnce.Do(func() { close(proceed) }) }
			t.Cleanup(closeProceed)

			// Every commits request — successful or not — stays in flight
			// until release, regardless of which credential made it: this is
			// what lets a merged identity (one shared flight) and a
			// correctly-separated one (two independent flights) be told
			// apart deterministically below, rather than relying on which
			// one happens to answer first.
			mux.HandleFunc("/repos/acme/private/commits/main", func(w http.ResponseWriter, r *http.Request) {
				<-proceed
				if !authOK(r) {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				_, _ = w.Write([]byte(testCommitSHA))
			})
			mux.HandleFunc("/repos/acme/private/contents/skills/s", func(w http.ResponseWriter, r *http.Request) {
				if !authOK(r) {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				_ = json.NewEncoder(w).Encode([]githubContentEntry{
					{Name: "SKILL.md", Path: "skills/s/SKILL.md", Type: "file", Size: 6},
				})
			})
			mux.HandleFunc("/raw/acme/private/"+testCommitSHA+"/skills/s/SKILL.md", func(w http.ResponseWriter, r *http.Request) {
				if !authOK(r) {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				_, _ = w.Write([]byte("SECRET"))
			})

			cache, err := newTestResolutionCache(t.TempDir(), time.Minute)
			if err != nil {
				t.Fatalf("NewGitHubResolutionCache: %v", err)
			}
			mk := func(tok string) *GitHubSkillResolver {
				r := newTestGitHubResolver(server)
				r.resolutionCache = cache
				if tc.namedSource {
					// No default fallback: only the named convention key
					// supplies a credential, under the same key name for
					// both callers but a different value each.
					r.token = ""
					r.provisionCredentials = map[string]string{deriveGitHubTokenKey("acme", "private"): tok}
				} else {
					r.token = tok
					r.provisionCredentials = nil
				}
				return r
			}
			leader, waiter := mk(tc.leaderTok), mk(tc.waiterTok)
			ref := api.SkillReference{URI: "gh://acme/private/s@main"}
			opts := ResolveOpts{ProjectID: "project-1", UserID: "user-1"}

			var seenMu sync.Mutex
			seenKeys := make(map[string]bool)
			bothStarted := make(chan struct{})
			var startedOnce sync.Once
			hook := func(key string) {
				seenMu.Lock()
				seenKeys[key] = true
				n := len(seenKeys)
				seenMu.Unlock()
				if n >= 2 {
					startedOnce.Do(func() { close(bothStarted) })
				}
			}
			flightJoinHook.Store(&hook)
			t.Cleanup(func() { flightJoinHook.Store(nil) })

			doneLeader := make(chan struct{})
			go func() {
				_, _ = leader.Resolve(context.Background(), []api.SkillReference{ref}, opts)
				close(doneLeader)
			}()

			var resWaiter *ResolveResult
			doneWaiter := make(chan struct{})
			go func() {
				resWaiter, _ = waiter.Resolve(context.Background(), []api.SkillReference{ref}, opts)
				close(doneWaiter)
			}()

			// Two distinct flight keys being touched proves the two
			// credentials were never coalesced into one flight — the
			// property this test exists to check. Under the merged-identity
			// bug, only one key is ever touched, so this would time out
			// instead of a content assertion failing — still a deterministic,
			// bounded failure.
			select {
			case <-bothStarted:
			case <-time.After(5 * time.Second):
				t.Fatal("did not observe two independent flights — the two credentials may have been coalesced into one")
			}

			closeProceed()

			for _, done := range []chan struct{}{doneLeader, doneWaiter} {
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("a resolve did not complete after the flight was released")
				}
			}

			if tc.waiterShouldSucceed {
				if len(resWaiter.Resolved) != 1 {
					t.Fatalf("expected the agent with access to resolve successfully, got errors: %+v", resWaiter.Errors)
				}
				if got := string(resWaiter.Resolved[0].Files[0].Content); got != "SECRET" {
					t.Errorf("expected content %q, got %q", "SECRET", got)
				}
			} else {
				if len(resWaiter.Resolved) != 0 {
					t.Fatalf("the agent without access must never receive content resolved with the other agent's credential, got: %+v", resWaiter.Resolved[0])
				}
			}
		})
	}
}

// TestGitHubSkillResolver_ScopeLayeringIsolation pins the project and user
// scope layered on top of the credential-value hash in flightIdentity: even
// when two callers present the exact same credential value, a different
// project, a different user within one project under the default source, or
// a different project under a named source must each still get independent
// flights. The value hash alone cannot tell these cases apart — the
// credential is identical in every subtest here — so only the project/user
// layering can; dropping it (see flightIdentity) would silently merge these
// cases back into one flight.
func TestGitHubSkillResolver_ScopeLayeringIsolation(t *testing.T) {
	const sharedToken = "tok-shared"
	cases := []struct {
		name                   string
		leaderOpts, waiterOpts ResolveOpts
		namedSource            bool
	}{
		{
			name:       "same token, different project, default source",
			leaderOpts: ResolveOpts{ProjectID: "project-1", UserID: "user-1"},
			waiterOpts: ResolveOpts{ProjectID: "project-2", UserID: "user-1"},
		},
		{
			name:       "same token, same project, different user, default source",
			leaderOpts: ResolveOpts{ProjectID: "project-1", UserID: "user-1"},
			waiterOpts: ResolveOpts{ProjectID: "project-1", UserID: "user-2"},
		},
		{
			name:        "same token, different project, named source",
			leaderOpts:  ResolveOpts{ProjectID: "project-1", UserID: "user-1"},
			waiterOpts:  ResolveOpts{ProjectID: "project-2", UserID: "user-1"},
			namedSource: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server, mux := newTestGitHubServer(t)
			authOK := func(r *http.Request) bool { return r.Header.Get("Authorization") == "Bearer "+sharedToken }

			proceed := make(chan struct{})
			var proceedOnce sync.Once
			closeProceed := func() { proceedOnce.Do(func() { close(proceed) }) }
			t.Cleanup(closeProceed)

			mux.HandleFunc("/repos/acme/shared/commits/main", func(w http.ResponseWriter, r *http.Request) {
				<-proceed
				if !authOK(r) {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				_, _ = w.Write([]byte(testCommitSHA))
			})
			mux.HandleFunc("/repos/acme/shared/contents/skills/s", func(w http.ResponseWriter, r *http.Request) {
				if !authOK(r) {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				_ = json.NewEncoder(w).Encode([]githubContentEntry{
					{Name: "SKILL.md", Path: "skills/s/SKILL.md", Type: "file", Size: 6},
				})
			})
			mux.HandleFunc("/raw/acme/shared/"+testCommitSHA+"/skills/s/SKILL.md", func(w http.ResponseWriter, r *http.Request) {
				if !authOK(r) {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				_, _ = w.Write([]byte("CONTENT"))
			})

			cache, err := newTestResolutionCache(t.TempDir(), time.Minute)
			if err != nil {
				t.Fatalf("NewGitHubResolutionCache: %v", err)
			}
			mk := func() *GitHubSkillResolver {
				r := newTestGitHubResolver(server)
				r.token = sharedToken
				r.resolutionCache = cache
				if tc.namedSource {
					r.provisionCredentials = map[string]string{deriveGitHubTokenKey("acme", "shared"): sharedToken}
				} else {
					r.provisionCredentials = nil
				}
				return r
			}
			leader, waiter := mk(), mk()
			ref := api.SkillReference{URI: "gh://acme/shared/s@main"}

			var seenMu sync.Mutex
			seenKeys := make(map[string]bool)
			bothStarted := make(chan struct{})
			var startedOnce sync.Once
			hook := func(key string) {
				seenMu.Lock()
				seenKeys[key] = true
				n := len(seenKeys)
				seenMu.Unlock()
				if n >= 2 {
					startedOnce.Do(func() { close(bothStarted) })
				}
			}
			flightJoinHook.Store(&hook)
			t.Cleanup(func() { flightJoinHook.Store(nil) })

			doneLeader := make(chan struct{})
			var resLeader *ResolveResult
			go func() {
				resLeader, _ = leader.Resolve(context.Background(), []api.SkillReference{ref}, tc.leaderOpts)
				close(doneLeader)
			}()

			doneWaiter := make(chan struct{})
			var resWaiter *ResolveResult
			go func() {
				resWaiter, _ = waiter.Resolve(context.Background(), []api.SkillReference{ref}, tc.waiterOpts)
				close(doneWaiter)
			}()

			// Two distinct flight keys being touched proves the project/user
			// scope told these two otherwise-identical credentials apart —
			// under a dropped-scope regression, both calls would land on the
			// same key and this would time out instead.
			select {
			case <-bothStarted:
			case <-time.After(5 * time.Second):
				t.Fatal("did not observe two independent flights — project/user scope may have been dropped, merging two different scopes under one identity")
			}

			closeProceed()

			for _, done := range []chan struct{}{doneLeader, doneWaiter} {
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("a resolve did not complete after the flight was released")
				}
			}

			for name, res := range map[string]*ResolveResult{"leader": resLeader, "waiter": resWaiter} {
				if len(res.Resolved) != 1 {
					t.Fatalf("%s: expected one resolved skill, got errors: %+v", name, res.Errors)
				}
				if got := string(res.Resolved[0].Files[0].Content); got != "CONTENT" {
					t.Errorf("%s: expected content %q, got %q", name, "CONTENT", got)
				}
			}
		})
	}
}

// TestGitHubSkillResolver_FullSHARefNeverServedStale is the resolver-level
// acceptance test for resolveOne's own branch-vs-SHA classification
// (isBranchRef := !isFullCommitSHA(effectiveRef)): a commit-SHA ref must
// never be served stale, which only holds if resolveOne actually computes
// isBranchRef correctly for it, not merely if the cache layer honors
// whatever isBranchRef it is given (see
// TestGitHubResolutionCache_ResolveWithFetch_SHARefNeverServedStale, which
// calls the cache directly and so cannot catch a wrong classification at
// this call site).
//
// The cache is constructed with a negative TTL so every entry it writes is
// already expired the instant it is written — standing in for "time has
// passed" without a sleep. A second resolve of the same SHA ref must then
// re-fetch rather than serve the first resolve's now-expired entry stale.
func TestGitHubSkillResolver_FullSHARefNeverServedStale(t *testing.T) {
	server, mux := newTestGitHubServer(t)

	var contentsCalls int32
	var content atomic.Value
	content.Store("v1")
	mux.HandleFunc("/repos/acme/private/contents/skills/s", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&contentsCalls, 1)
		_ = json.NewEncoder(w).Encode([]githubContentEntry{
			{Name: "SKILL.md", Path: "skills/s/SKILL.md", Type: "file", Size: 6},
		})
	})
	mux.HandleFunc("/raw/acme/private/"+testCommitSHA+"/skills/s/SKILL.md", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(content.Load().(string)))
	})

	cache, err := newTestResolutionCache(t.TempDir(), -time.Minute)
	if err != nil {
		t.Fatalf("NewGitHubResolutionCache: %v", err)
	}
	r := newTestGitHubResolver(server)
	r.resolutionCache = cache
	ref := api.SkillReference{URI: "gh://acme/private/s@" + testCommitSHA}

	res1, err := r.Resolve(context.Background(), []api.SkillReference{ref}, ResolveOpts{})
	if err != nil || len(res1.Resolved) != 1 {
		t.Fatalf("first resolve: unexpected result, err=%v res=%+v", err, res1)
	}
	if got := string(res1.Resolved[0].Files[0].Content); got != "v1" {
		t.Fatalf("first resolve: expected content %q, got %q", "v1", got)
	}

	content.Store("v2")
	res2, err := r.Resolve(context.Background(), []api.SkillReference{ref}, ResolveOpts{})
	if err != nil || len(res2.Resolved) != 1 {
		t.Fatalf("second resolve: unexpected result, err=%v res=%+v", err, res2)
	}
	if got := string(res2.Resolved[0].Files[0].Content); got != "v2" {
		t.Fatalf("a commit-SHA ref must never be served stale — expected the freshly re-resolved content %q, got %q (classified as a branch ref?)", "v2", got)
	}
	if got := atomic.LoadInt32(&contentsCalls); got != 2 {
		t.Fatalf("expected one GitHub call per resolve of an already-expired SHA-ref entry, got %d calls", got)
	}
}

// TestGitHubSkillResolver_CoalescedCallersKeepOwnAlias is the acceptance test
// for the per-caller As assignment in resolveOne (resolved.As = ref.As): two
// callers sharing one URI, token, project and user — and so one cache entry
// and one flight — must each get back their own As, not whichever caller led
// the flight. The leader's As is baked into the ResolvedSkill the fetch
// itself returns (see fetchOne), so resolveOne must overwrite it per caller
// after the shared fetch completes.
func TestGitHubSkillResolver_CoalescedCallersKeepOwnAlias(t *testing.T) {
	server, mux := newTestGitHubServer(t)

	proceed := make(chan struct{})
	var proceedOnce sync.Once
	closeProceed := func() { proceedOnce.Do(func() { close(proceed) }) }
	t.Cleanup(closeProceed)

	mux.HandleFunc("/repos/acme/shared/commits/main", func(w http.ResponseWriter, r *http.Request) {
		<-proceed
		_, _ = w.Write([]byte(testCommitSHA))
	})
	mux.HandleFunc("/repos/acme/shared/contents/skills/s", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]githubContentEntry{
			{Name: "SKILL.md", Path: "skills/s/SKILL.md", Type: "file", Size: 6},
		})
	})
	mux.HandleFunc("/raw/acme/shared/"+testCommitSHA+"/skills/s/SKILL.md", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("CONTENT"))
	})

	cache, err := newTestResolutionCache(t.TempDir(), time.Minute)
	if err != nil {
		t.Fatalf("NewGitHubResolutionCache: %v", err)
	}
	r := newTestGitHubResolver(server)
	r.resolutionCache = cache

	refA := api.SkillReference{URI: "gh://acme/shared/s@main", As: "alias-a", Scope: "project"}
	refB := api.SkillReference{URI: "gh://acme/shared/s@main", As: "alias-b", Scope: "user", Optional: true}

	var joinCount int32
	bothJoined := make(chan struct{})
	var joinedOnce sync.Once
	hook := func(key string) {
		if atomic.AddInt32(&joinCount, 1) == 2 {
			joinedOnce.Do(func() { close(bothJoined) })
		}
	}
	flightJoinHook.Store(&hook)
	t.Cleanup(func() { flightJoinHook.Store(nil) })

	var resA, resB *ResolveResult
	doneA := make(chan struct{})
	doneB := make(chan struct{})
	go func() {
		resA, _ = r.Resolve(context.Background(), []api.SkillReference{refA}, ResolveOpts{})
		close(doneA)
	}()
	go func() {
		resB, _ = r.Resolve(context.Background(), []api.SkillReference{refB}, ResolveOpts{})
		close(doneB)
	}()

	select {
	case <-bothJoined:
	case <-time.After(5 * time.Second):
		t.Fatal("did not observe both callers joining one flight")
	}

	closeProceed()

	for _, done := range []chan struct{}{doneA, doneB} {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("a resolve did not complete after the flight was released")
		}
	}

	if len(resA.Resolved) != 1 || resA.Resolved[0].As != "alias-a" {
		t.Fatalf("caller A: expected As %q, got result %+v (errors: %+v)", "alias-a", resA.Resolved, resA.Errors)
	}
	if len(resB.Resolved) != 1 || resB.Resolved[0].As != "alias-b" {
		t.Fatalf("caller B: expected As %q, got result %+v (errors: %+v)", "alias-b", resB.Resolved, resB.Errors)
	}
	// Scope and Optional are per caller too.
	if a := resA.Resolved[0]; a.Scope != "project" || a.Optional {
		t.Errorf("caller A: Scope=%q Optional=%v, want project/false", a.Scope, a.Optional)
	}
	if b := resB.Resolved[0]; b.Scope != "user" || !b.Optional {
		t.Errorf("caller B: Scope=%q Optional=%v, want user/true", b.Scope, b.Optional)
	}
}

// TestGitHubSkillResolver_CacheHitKeepsOwnScopeAndOptional checks that refs
// sharing one URI and credential, served from one cache entry, each keep
// their own Scope and Optional, both within one Resolve call and across
// calls.
func TestGitHubSkillResolver_CacheHitKeepsOwnScopeAndOptional(t *testing.T) {
	server, mux := newTestGitHubServer(t)
	var commitCalls atomic.Int32
	mux.HandleFunc("/repos/acme/shared/commits/main", func(w http.ResponseWriter, r *http.Request) {
		commitCalls.Add(1)
		_, _ = w.Write([]byte(testCommitSHA))
	})
	mux.HandleFunc("/repos/acme/shared/contents/skills/s", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]githubContentEntry{
			{Name: "SKILL.md", Path: "skills/s/SKILL.md", Type: "file", Size: 7},
		})
	})
	mux.HandleFunc("/raw/acme/shared/"+testCommitSHA+"/skills/s/SKILL.md", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("CONTENT"))
	})

	cache, err := newTestResolutionCache(t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	r := newTestGitHubResolver(server)
	r.resolutionCache = cache

	const uri = "gh://acme/shared/s@main"
	// Fill the cache from a ref with yet another Scope and Optional.
	if res, _ := r.Resolve(context.Background(), []api.SkillReference{{URI: uri, Scope: "global", Optional: true}}, ResolveOpts{}); len(res.Resolved) != 1 {
		t.Fatalf("first resolve: %+v", res)
	}

	refs := []api.SkillReference{
		{URI: uri, As: "p", Scope: "project"},
		{URI: uri, As: "u", Scope: "user", Optional: true},
	}
	res, err := r.Resolve(context.Background(), refs, ResolveOpts{})
	if err != nil || len(res.Resolved) != 2 {
		t.Fatalf("Resolve: err=%v result=%+v", err, res)
	}
	if n := commitCalls.Load(); n != 1 {
		t.Fatalf("GitHub commit lookups = %d, want 1 (later refs served from the cache)", n)
	}
	for i, got := range res.Resolved {
		want := refs[i]
		if got.As != want.As || got.Scope != want.Scope || got.Optional != want.Optional {
			t.Errorf("result %d: As=%q Scope=%q Optional=%v, want %q/%q/%v",
				i, got.As, got.Scope, got.Optional, want.As, want.Scope, want.Optional)
		}
	}
}

// TestCredentialFingerprint_FullWidth pins the full-width requirement on
// credentialFingerprint directly: the decided design calls for the full
// SHA-256 digest (or at least 128 bits) from one shared helper, not a
// truncated prefix, so that "two different credential values never merge"
// holds exactly rather than merely with high probability.
func TestCredentialFingerprint_FullWidth(t *testing.T) {
	got := credentialFingerprint("x")
	const wantLen = 64 // hex-encoded SHA-256: 32 bytes * 2 hex chars/byte
	if len(got) != wantLen {
		t.Fatalf("expected a %d-character full hex-encoded SHA-256 digest, got %d characters: %q", wantLen, len(got), got)
	}
}

// TestGitHubSkillResolver_CachedWaiterDeadline_ClassifiedAsTimeout proves that
// when the shared (coalesced) fetch outlives a caller's resolve budget, the
// caller's own deadline maps to SkillErrCodeTimeout rather than an
// unclassified resolve_failed, and that the message names only the
// credential-free logRef.
func TestGitHubSkillResolver_CachedWaiterDeadline_ClassifiedAsTimeout(t *testing.T) {
	server, mux := newTestGitHubServer(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	mux.HandleFunc("/repos/owner/repo/commits/main", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})

	cache, err := newTestResolutionCache(t.TempDir(), time.Minute)
	if err != nil {
		t.Fatalf("NewGitHubResolutionCache: %v", err)
	}
	resolver := newTestGitHubResolver(server)
	resolver.resolutionCache = cache
	resolver.resolveBudget = 200 * time.Millisecond

	result, err := resolver.Resolve(context.Background(), []api.SkillReference{
		{URI: "gh://owner/repo/my-skill@main"},
	}, ResolveOpts{})
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("expected 1 error, got %d", len(result.Errors))
	}
	got := result.Errors[0]
	if got.Code != SkillErrCodeTimeout {
		t.Errorf("expected code %s, got %s (message: %s)", SkillErrCodeTimeout, got.Code, got.Message)
	}
	if !strings.Contains(got.Message, "gh://owner/repo/my-skill@main (default)") {
		t.Errorf("expected message to name the logRef, got %s", got.Message)
	}
	if strings.Contains(got.Message, credentialFingerprint("test-token")) || strings.Contains(got.Message, "test-token") {
		t.Errorf("message must not carry credential-derived material, got %s", got.Message)
	}
}

// TestGitHubResolutionCache_WaiterDeadline_WrapsTypedAndContextError pins the
// waiter-path error contract of ResolveWithFetch directly (Resolve flattens
// errors, so it cannot see this): when the caller's own deadline expires on a
// blocked shared fetch, errors.As finds the typed timeout and errors.Is still
// matches context.DeadlineExceeded; plain cancellation returns exactly
// context.Canceled.
func TestGitHubResolutionCache_WaiterDeadline_WrapsTypedAndContextError(t *testing.T) {
	cache, err := newTestResolutionCache(t.TempDir(), time.Minute)
	if err != nil {
		t.Fatalf("NewGitHubResolutionCache: %v", err)
	}
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	blockingFetch := func(ctx context.Context) (ResolvedSkill, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return ResolvedSkill{}, errors.New("released")
	}

	dctx, dcancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer dcancel()
	_, err = cache.ResolveWithFetch(dctx, "key-deadline", "flight-deadline", "cred", "gh://o/r/s@main (default)", false, nil, blockingFetch)
	var typed *githubResolveError
	if !errors.As(err, &typed) || typed.code != SkillErrCodeTimeout {
		t.Errorf("expected typed error with code %s, got %v", SkillErrCodeTimeout, err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected errors.Is(err, context.DeadlineExceeded), got %v", err)
	}

	cctx, ccancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		ccancel()
	}()
	_, err = cache.ResolveWithFetch(cctx, "key-cancel", "flight-cancel", "cred", "gh://o/r/s@main (default)", false, nil, blockingFetch)
	if err != context.Canceled { //nolint:errorlint // exact identity is the contract under test
		t.Errorf("expected exactly context.Canceled, got %#v", err)
	}
}

// TestNewGitHubHTTPClient_NonTransportDefault_NoPanic proves that a
// replaced http.DefaultTransport that is not an *http.Transport no longer
// panics newGitHubHTTPClient, and that the fallback transport still carries
// the stall bound and the proxy setting and can serve a request. Not
// parallel: it swaps a package-level global.
func TestNewGitHubHTTPClient_NonTransportDefault_NoPanic(t *testing.T) {
	orig := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = orig })
	http.DefaultTransport = roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("replaced DefaultTransport must not be used")
	})

	var c *http.Client
	func() {
		defer func() {
			if p := recover(); p != nil {
				t.Fatalf("newGitHubHTTPClient panicked: %v", p)
			}
		}()
		c = newGitHubHTTPClient(250 * time.Millisecond)
	}()

	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport, got %T", c.Transport)
	}
	if tr.ResponseHeaderTimeout != 250*time.Millisecond {
		t.Errorf("expected ResponseHeaderTimeout 250ms, got %s", tr.ResponseHeaderTimeout)
	}
	if tr.Proxy == nil || tr.DialContext == nil || tr.TLSHandshakeTimeout == 0 {
		t.Errorf("expected proxy, dialer and TLS handshake timeout to be set, got Proxy=%t DialContext=%t TLSHandshakeTimeout=%s",
			tr.Proxy != nil, tr.DialContext != nil, tr.TLSHandshakeTimeout)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	resp, err := c.Get(server.URL)
	if err != nil {
		t.Fatalf("request with fallback transport failed: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("expected 204, got %d", resp.StatusCode)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestGitHubResolveError_Unwrap proves errors.Is and errors.As reach a
// githubResolveError's cause, while errors.As for *githubResolveError still
// finds the typed error first through the fmt.Errorf wrapping fetchOne and
// the request helpers add.
func TestGitHubResolveError_Unwrap(t *testing.T) {
	cause := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	err := fmt.Errorf("failed to resolve ref for gh://o/r/s: %w",
		fmt.Errorf("GitHub API request failed: %w",
			&githubResolveError{code: SkillErrCodeUnreachable, msg: cause.Error(), err: cause}))

	var rerr *githubResolveError
	if !errors.As(err, &rerr) || rerr.code != SkillErrCodeUnreachable {
		t.Fatalf("expected errors.As to find the typed error with code %s, got %v", SkillErrCodeUnreachable, rerr)
	}
	if !errors.Is(err, cause) {
		t.Error("expected errors.Is to reach the cause")
	}
	var opErr *net.OpError
	if !errors.As(err, &opErr) || opErr != cause {
		t.Error("expected errors.As to reach the *net.OpError cause")
	}
	if (&githubResolveError{code: SkillErrCodeTimeout}).Unwrap() != nil {
		t.Error("expected Unwrap to return nil without a cause")
	}
}

// TestGitHubSkillResolver_CancelledDuringFinalRetry_ReturnsCanceled proves
// that a caller cancelling while the last retry attempt is in flight gets
// context.Canceled back, not a typed unreachable (502) error classified
// from the aborted attempt.
func TestGitHubSkillResolver_CancelledDuringFinalRetry_ReturnsCanceled(t *testing.T) {
	server, mux := newTestGitHubServer(t)

	var attempts int32
	finalStarted := make(chan struct{})
	mux.HandleFunc("/repos/owner/repo/commits/main", func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&attempts, 1) <= githubMaxRetries {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		close(finalStarted)
		<-r.Context().Done()
	})

	resolver := newTestGitHubResolver(server)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-finalStarted:
			cancel()
		case <-time.After(10 * time.Second):
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/repos/owner/repo/commits/main", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := resolver.doWithRetry(ctx, req, 5*time.Second, "")
	if resp != nil {
		_ = resp.Body.Close()
		t.Fatalf("expected no response, got status %d", resp.StatusCode)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	var rerr *githubResolveError
	if errors.As(err, &rerr) {
		t.Errorf("expected no typed error on cancellation, got code %s", rerr.code)
	}
	if got := atomic.LoadInt32(&attempts); got != githubMaxRetries+1 {
		t.Errorf("expected %d attempts, got %d", githubMaxRetries+1, got)
	}
}

// TestGitHubSkillResolver_CancelledBeforeRetry_ReturnsCanceled covers the
// cancel check at the top of doWithRetry's loop. The caller cancels while
// the first attempt is in flight, and that attempt still returns a 503 with
// a Retry-After above the backoff cap. Without the check, the
// Retry-After-above-cap fail-fast would run first and return a typed
// upstream_unavailable error; with it, the caller gets context.Canceled and
// no second request is made. The transport cancels ctx and then returns the
// response, so the attempt itself is not affected by the cancel.
func TestGitHubSkillResolver_CancelledBeforeRetry_ReturnsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var attempts int32
	resolver := &GitHubSkillResolver{
		httpClient: &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			atomic.AddInt32(&attempts, 1)
			cancel()
			h := make(http.Header)
			h.Set("Retry-After", "120") // above githubMaxBackoff
			return &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Header:     h,
				Body:       http.NoBody,
				Request:    r,
			}, nil
		})},
		apiBase: "http://github.invalid",
		rawBase: "http://github.invalid/raw",
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://github.invalid/repos/owner/repo/commits/main", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := resolver.doWithRetry(ctx, req, 5*time.Second, "")
	if resp != nil {
		_ = resp.Body.Close()
		t.Fatalf("expected no response, got status %d", resp.StatusCode)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	var rerr *githubResolveError
	if errors.As(err, &rerr) {
		t.Errorf("expected no typed error on cancellation, got code %s", rerr.code)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("expected exactly 1 attempt, got %d", got)
	}
}

// TestRetryAfter_HugeValueDoesNotOverflow checks that a Retry-After far too
// large for time.Duration is capped (or saturated) rather than wrapping into
// a negative or short duration, at each place the header is converted.
func TestRetryAfter_HugeValueDoesNotOverflow(t *testing.T) {
	// 99999999999 seconds wraps to a large positive duration; 9223372037
	// wraps to a negative one and 18446744074 to under a second, so a cap
	// applied after the multiplication would not catch the last two.
	// 99999999999999999999 does not fit in int64 at all and must still read
	// as huge, not as absent.
	for _, huge := range []string{"99999999999", "9223372037", "18446744074", "99999999999999999999"} {
		t.Run(huge, func(t *testing.T) {
			newResp := func(status int) *http.Response {
				resp := &http.Response{StatusCode: status, Header: make(http.Header)}
				resp.Header.Set("Retry-After", huge)
				return resp
			}

			if got := githubCooldownFor(newResp(http.StatusTooManyRequests), time.Now()); got != GitHubCooldownMax {
				t.Errorf("githubCooldownFor = %v, want GitHubCooldownMax (%v)", got, GitHubCooldownMax)
			}
			if got := retryDelay(newResp(http.StatusServiceUnavailable), 1); got != githubMaxBackoff {
				t.Errorf("retryDelay = %v, want githubMaxBackoff (%v)", got, githubMaxBackoff)
			}
			got, ok := retryAfterDuration(newResp(http.StatusServiceUnavailable))
			if !ok || got <= githubMaxBackoff {
				t.Errorf("retryAfterDuration = %v, %v; want ok and longer than githubMaxBackoff", got, ok)
			}
		})
	}
}

func TestParseRetryAfterSeconds(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"0", 0, true},
		{"30", 30, true},
		{"99999999999999999999", math.MaxInt64, true},
		{"-99999999999999999999", 0, false},
		{"+99999999999999999999", 0, false},
		{"1.5", 0, false},
		{"Wed, 21 Oct 2015 07:28:00 GMT", 0, false},
		{"", 0, false},
	}
	for _, tc := range cases {
		got, ok := parseRetryAfterSeconds(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("parseRetryAfterSeconds(%q) = %d, %v; want %d, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestSecondsUpTo(t *testing.T) {
	cases := []struct {
		secs int64
		max  time.Duration
		want time.Duration
	}{
		{0, time.Minute, 0},
		{-5, time.Minute, 0},
		{30, time.Minute, 30 * time.Second},
		{60, time.Minute, time.Minute},
		{61, time.Minute, time.Minute},
		{math.MaxInt64, time.Minute, time.Minute},
		{99999999999, time.Duration(math.MaxInt64), time.Duration(math.MaxInt64)},
	}
	for _, tc := range cases {
		if got := secondsUpTo(tc.secs, tc.max); got != tc.want {
			t.Errorf("secondsUpTo(%d, %v) = %v, want %v", tc.secs, tc.max, got, tc.want)
		}
	}
}
