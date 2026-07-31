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
	"os"
	"path/filepath"
	"strings"
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

	cache, err := NewGitHubResolutionCache(t.TempDir(), 5*time.Minute)
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
	cache, err := NewGitHubResolutionCache(t.TempDir(), 5*time.Minute)
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
	cache, err := NewGitHubResolutionCache(t.TempDir(), 5*time.Minute)
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
