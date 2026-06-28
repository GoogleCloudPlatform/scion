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
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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
		w.Write([]byte(testCommitSHA))
	})

	mux.HandleFunc("/repos/owner/repo/contents/skills/my-skill", func(w http.ResponseWriter, r *http.Request) {
		ref := r.URL.Query().Get("ref")
		if ref != testCommitSHA {
			t.Errorf("expected ref=%s, got %s", testCommitSHA, ref)
		}
		json.NewEncoder(w).Encode([]githubContentEntry{
			{Name: "SKILL.md", Path: "skills/my-skill/SKILL.md", Type: "file", Size: len(skillContent)},
			{Name: "README.md", Path: "skills/my-skill/README.md", Type: "file", Size: len(readmeContent)},
		})
	})

	mux.HandleFunc("/raw/owner/repo/"+testCommitSHA+"/skills/my-skill/SKILL.md", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(skillContent))
	})
	mux.HandleFunc("/raw/owner/repo/"+testCommitSHA+"/skills/my-skill/README.md", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(readmeContent))
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
		w.Write([]byte(testCommitSHA))
	})
	mux.HandleFunc("/repos/owner/repo/contents/skills/my-skill", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode([]githubContentEntry{
			{Name: "SKILL.md", Path: "skills/my-skill/SKILL.md", Type: "file", Size: 5},
		})
	})
	mux.HandleFunc("/raw/owner/repo/"+testCommitSHA+"/skills/my-skill/SKILL.md", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("hello"))
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
	if result.Errors[0].Code != "resolve_failed" {
		t.Errorf("expected code resolve_failed, got %s", result.Errors[0].Code)
	}
	if !strings.Contains(result.Errors[0].Message, "not found") {
		t.Errorf("expected error to contain 'not found', got %s", result.Errors[0].Message)
	}
}

func TestGitHubSkillResolver_NotFound_SkillDir(t *testing.T) {
	server, mux := newTestGitHubServer(t)

	mux.HandleFunc("/repos/owner/repo/commits/main", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(testCommitSHA))
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
	if !strings.Contains(result.Errors[0].Message, "rate limit") {
		t.Errorf("expected error to mention rate limit, got %s", result.Errors[0].Message)
	}
	if !strings.Contains(result.Errors[0].Message, "GITHUB_TOKEN") {
		t.Errorf("expected error to mention GITHUB_TOKEN, got %s", result.Errors[0].Message)
	}
	// Verify retries happened before the final rate-limit error
	if attempts > 1 {
		t.Logf("retried %d times before giving up (expected with backoff)", attempts-1)
	}
}

func TestGitHubSkillResolver_RetryOn429(t *testing.T) {
	server, mux := newTestGitHubServer(t)

	attempts := 0
	mux.HandleFunc("/repos/owner/repo/commits/main", func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts <= 2 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
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
	if len(result.Resolved) != 1 {
		t.Fatalf("expected 1 resolved skill, got %d", len(result.Resolved))
	}
	if attempts < 3 {
		t.Errorf("expected at least 3 attempts, got %d", attempts)
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
	cache, err := NewGitHubResolutionCache(t.TempDir(), 5*time.Minute)
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
		w.Write([]byte(testCommitSHA))
	})
	mux.HandleFunc("/repos/owner/repo/contents/skills/my-skill", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode([]githubContentEntry{
			{Name: "SKILL.md", Path: "skills/my-skill/SKILL.md", Type: "file", Size: 5},
		})
	})
	mux.HandleFunc("/raw/owner/repo/"+testCommitSHA+"/skills/my-skill/SKILL.md", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("hello"))
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
		w.Write([]byte(testCommitSHA))
	})
	mux.HandleFunc("/repos/owner/repo/contents/skills/my-skill", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode([]githubContentEntry{
			{Name: "SKILL.md", Path: "skills/my-skill/SKILL.md", Type: "file", Size: 5},
		})
	})
	mux.HandleFunc("/raw/owner/repo/"+testCommitSHA+"/skills/my-skill/SKILL.md", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("hello"))
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
		{"429 is retryable", 429, nil, true},
		{"403 with rate limit is retryable", 403, map[string]string{"X-RateLimit-Remaining": "0"}, true},
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
			StatusCode: 429,
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
			StatusCode: 429,
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

type stubSkillResolver struct {
	result *ResolveResult
}

func (s *stubSkillResolver) ResolverName() string { return "stub" }
func (s *stubSkillResolver) Resolve(_ context.Context, _ []api.SkillReference, _ ResolveOpts) (*ResolveResult, error) {
	return s.result, nil
}
