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

package githubapp

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// generateTestKey generates a test RSA private key in PEM format.
func generateTestKey(t *testing.T) ([]byte, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate test key: %v", err)
	}

	pemData := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})

	return pemData, key
}

func TestParsePrivateKey_PKCS1(t *testing.T) {
	pemData, original := generateTestKey(t)

	parsed, err := ParsePrivateKey(pemData)
	if err != nil {
		t.Fatalf("ParsePrivateKey failed: %v", err)
	}

	if parsed.N.Cmp(original.N) != 0 {
		t.Error("parsed key doesn't match original")
	}
}

func TestParsePrivateKey_PKCS8(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}

	pkcs8Bytes, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("failed to marshal PKCS8: %v", err)
	}

	pemData := pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: pkcs8Bytes,
	})

	parsed, err := ParsePrivateKey(pemData)
	if err != nil {
		t.Fatalf("ParsePrivateKey failed: %v", err)
	}

	if parsed.N.Cmp(key.N) != 0 {
		t.Error("parsed key doesn't match original")
	}
}

func TestParsePrivateKey_Invalid(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{"empty", []byte{}},
		{"garbage", []byte("not a pem block")},
		{"bad pem content", pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: []byte("invalid")})},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParsePrivateKey(tc.data)
			if err == nil {
				t.Error("expected error, got nil")
			}
		})
	}
}

func TestNewClient(t *testing.T) {
	pemData, _ := generateTestKey(t)

	t.Run("valid config", func(t *testing.T) {
		client, err := NewClient(Config{AppID: 12345}, pemData)
		if err != nil {
			t.Fatalf("NewClient failed: %v", err)
		}
		if client.appID != 12345 {
			t.Errorf("expected appID 12345, got %d", client.appID)
		}
		if client.apiBaseURL != "https://api.github.com" {
			t.Errorf("expected default API URL, got %s", client.apiBaseURL)
		}
	})

	t.Run("custom API base URL", func(t *testing.T) {
		client, err := NewClient(Config{
			AppID:      12345,
			APIBaseURL: "https://github.example.com/api/v3/",
		}, pemData)
		if err != nil {
			t.Fatalf("NewClient failed: %v", err)
		}
		if client.apiBaseURL != "https://github.example.com/api/v3" {
			t.Errorf("expected trimmed URL, got %s", client.apiBaseURL)
		}
	})

	t.Run("missing app ID", func(t *testing.T) {
		_, err := NewClient(Config{}, pemData)
		if err == nil {
			t.Error("expected error for missing app ID")
		}
	})

	t.Run("invalid key", func(t *testing.T) {
		_, err := NewClient(Config{AppID: 1}, []byte("bad key"))
		if err == nil {
			t.Error("expected error for bad key")
		}
		mintErr, ok := err.(*TokenMintError)
		if !ok {
			t.Fatalf("expected TokenMintError, got %T", err)
		}
		if mintErr.ErrorCode != ErrCodePrivateKeyInvalid {
			t.Errorf("expected error code %s, got %s", ErrCodePrivateKeyInvalid, mintErr.ErrorCode)
		}
	})
}

func TestGenerateJWT(t *testing.T) {
	pemData, key := generateTestKey(t)

	client, err := NewClient(Config{AppID: 42}, pemData)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	token, err := client.GenerateJWT()
	if err != nil {
		t.Fatalf("GenerateJWT failed: %v", err)
	}

	if token == "" {
		t.Fatal("expected non-empty token")
	}

	// Parse and verify the JWT
	parsed, err := jwt.ParseSigned(token, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil {
		t.Fatalf("failed to parse JWT: %v", err)
	}

	var claims jwt.Claims
	if err := parsed.Claims(&key.PublicKey, &claims); err != nil {
		t.Fatalf("failed to verify JWT claims: %v", err)
	}

	if claims.Issuer != "42" {
		t.Errorf("expected issuer '42', got '%s'", claims.Issuer)
	}

	// Check expiry is ~10 minutes from now
	now := time.Now()
	expiry := claims.Expiry.Time()
	if expiry.Before(now.Add(9*time.Minute)) || expiry.After(now.Add(11*time.Minute)) {
		t.Errorf("expected expiry ~10min from now, got %v", expiry)
	}
}

func TestMintInstallationToken(t *testing.T) {
	pemData, _ := generateTestKey(t)

	t.Run("successful mint", func(t *testing.T) {
		expiresAt := time.Now().Add(1 * time.Hour).UTC().Format(time.RFC3339)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				t.Errorf("expected POST, got %s", r.Method)
			}
			if r.URL.Path != "/app/installations/12345/access_tokens" {
				t.Errorf("unexpected path: %s", r.URL.Path)
			}

			// Verify Authorization header has Bearer token
			auth := r.Header.Get("Authorization")
			if auth == "" || len(auth) < 8 {
				t.Error("missing or short Authorization header")
			}

			// Verify request body
			var reqBody accessTokenRequest
			if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
				t.Fatalf("failed to decode request body: %v", err)
			}
			if len(reqBody.Repositories) != 1 || reqBody.Repositories[0] != "my-repo" {
				t.Errorf("expected repos [my-repo], got %v", reqBody.Repositories)
			}
			if reqBody.Permissions["contents"] != "write" {
				t.Errorf("expected contents:write, got %v", reqBody.Permissions)
			}

			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(accessTokenResponse{
				Token:     "ghs_test_token_123",
				ExpiresAt: expiresAt,
				Permissions: map[string]string{
					"contents":      "write",
					"pull_requests": "write",
					"metadata":      "read",
				},
			})
		}))
		defer server.Close()

		client, err := NewClient(Config{
			AppID:      42,
			APIBaseURL: server.URL,
		}, pemData)
		if err != nil {
			t.Fatalf("NewClient failed: %v", err)
		}

		token, err := client.MintInstallationToken(
			context.Background(),
			12345,
			[]string{"my-repo"},
			DefaultTokenPermissions(),
		)
		if err != nil {
			t.Fatalf("MintInstallationToken failed: %v", err)
		}

		if token.Token != "ghs_test_token_123" {
			t.Errorf("expected token ghs_test_token_123, got %s", token.Token)
		}
		if token.Permissions["contents"] != "write" {
			t.Errorf("expected contents:write permission")
		}
	})

	t.Run("empty permissions are refused without a request", func(t *testing.T) {
		called := false
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
			w.WriteHeader(http.StatusCreated)
		}))
		defer server.Close()

		client, _ := NewClient(Config{AppID: 42, APIBaseURL: server.URL}, pemData)
		_, err := client.MintInstallationToken(context.Background(), 12345, []string{"my-repo"}, TokenPermissions{})
		if err == nil {
			t.Fatal("expected an error for an empty permission set")
		}
		if called {
			t.Error("no access_tokens request may be sent without explicit permissions")
		}
	})

	t.Run("installation not found (404)", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(githubErrorResponse{Message: "Not Found"})
		}))
		defer server.Close()

		client, _ := NewClient(Config{AppID: 42, APIBaseURL: server.URL}, pemData)
		_, err := client.MintInstallationToken(context.Background(), 99999, nil, DefaultTokenPermissions())
		if err == nil {
			t.Fatal("expected error")
		}

		mintErr, ok := err.(*TokenMintError)
		if !ok {
			t.Fatalf("expected TokenMintError, got %T", err)
		}
		if mintErr.ErrorCode != ErrCodeInstallationRevoked {
			t.Errorf("expected %s, got %s", ErrCodeInstallationRevoked, mintErr.ErrorCode)
		}
	})

	t.Run("installation suspended (403)", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(githubErrorResponse{Message: "Installation has been suspended"})
		}))
		defer server.Close()

		client, _ := NewClient(Config{AppID: 42, APIBaseURL: server.URL}, pemData)
		_, err := client.MintInstallationToken(context.Background(), 12345, nil, DefaultTokenPermissions())
		if err == nil {
			t.Fatal("expected error")
		}

		mintErr := err.(*TokenMintError)
		if mintErr.ErrorCode != ErrCodeInstallationSuspended {
			t.Errorf("expected %s, got %s", ErrCodeInstallationSuspended, mintErr.ErrorCode)
		}
	})

	t.Run("repo not accessible (422)", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_ = json.NewEncoder(w).Encode(githubErrorResponse{Message: "Repository not found"})
		}))
		defer server.Close()

		client, _ := NewClient(Config{AppID: 42, APIBaseURL: server.URL}, pemData)
		_, err := client.MintInstallationToken(context.Background(), 12345, []string{"nonexistent"}, DefaultTokenPermissions())
		if err == nil {
			t.Fatal("expected error")
		}

		mintErr := err.(*TokenMintError)
		if mintErr.ErrorCode != ErrCodeRepoNotAccessible {
			t.Errorf("expected %s, got %s", ErrCodeRepoNotAccessible, mintErr.ErrorCode)
		}
	})

	t.Run("unauthorized (401)", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(githubErrorResponse{Message: "Bad credentials"})
		}))
		defer server.Close()

		client, _ := NewClient(Config{AppID: 42, APIBaseURL: server.URL}, pemData)
		_, err := client.MintInstallationToken(context.Background(), 12345, nil, DefaultTokenPermissions())
		if err == nil {
			t.Fatal("expected error")
		}

		mintErr := err.(*TokenMintError)
		if mintErr.ErrorCode != ErrCodePrivateKeyInvalid {
			t.Errorf("expected %s, got %s", ErrCodePrivateKeyInvalid, mintErr.ErrorCode)
		}
	})
}

func TestConfig_IsConfigured(t *testing.T) {
	tests := []struct {
		name     string
		config   Config
		expected bool
	}{
		{"empty", Config{}, false},
		{"app_id only", Config{AppID: 1}, false},
		{"app_id + key path", Config{AppID: 1, PrivateKeyPath: "/path"}, true},
		{"app_id + inline key", Config{AppID: 1, PrivateKey: "key"}, true},
		{"key path only", Config{PrivateKeyPath: "/path"}, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.config.IsConfigured(); got != tc.expected {
				t.Errorf("expected %v, got %v", tc.expected, got)
			}
		})
	}
}

func TestClassifyGitHubError(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
		expected   string
	}{
		{"404", 404, `{"message":"Not Found"}`, ErrCodeInstallationRevoked},
		{"403 suspended", 403, `{"message":"Installation has been suspended"}`, ErrCodeInstallationSuspended},
		{"403 other", 403, `{"message":"denied"}`, ErrCodePermissionDenied},
		{"401 app not found", 401, `{"message":"App not found"}`, ErrCodeAppNotFound},
		{"401 bad creds", 401, `{"message":"Bad credentials"}`, ErrCodePrivateKeyInvalid},
		{"422 repo", 422, `{"message":"Repository not found"}`, ErrCodeRepoNotAccessible},
		{"422 other", 422, `{"message":"Invalid"}`, ErrCodePermissionDenied},
		{"500", 500, `{"message":"Server Error"}`, ErrCodeTokenMintFailed},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyGitHubError(tc.statusCode, []byte(tc.body))
			if err.ErrorCode != tc.expected {
				t.Errorf("expected %s, got %s", tc.expected, err.ErrorCode)
			}
		})
	}
}

// Ensure jose import is available for JWT parsing in tests
var _ = fmt.Sprintf

func TestParseRateLimitHeaders(t *testing.T) {
	tests := []struct {
		name     string
		headers  map[string]string
		expected RateLimitInfo
	}{
		{
			name: "all headers present",
			headers: map[string]string{
				"X-RateLimit-Limit":     "5000",
				"X-RateLimit-Remaining": "4990",
				"X-RateLimit-Used":      "10",
				"X-RateLimit-Reset":     "1700000000",
			},
			expected: RateLimitInfo{
				Limit:     5000,
				Remaining: 4990,
				Used:      10,
				Reset:     time.Unix(1700000000, 0),
			},
		},
		{
			name:    "no headers",
			headers: map[string]string{},
			expected: RateLimitInfo{
				Limit:     0,
				Remaining: 0,
				Used:      0,
			},
		},
		{
			name: "partial headers",
			headers: map[string]string{
				"X-RateLimit-Limit":     "5000",
				"X-RateLimit-Remaining": "100",
			},
			expected: RateLimitInfo{
				Limit:     5000,
				Remaining: 100,
			},
		},
		{
			name: "invalid values ignored",
			headers: map[string]string{
				"X-RateLimit-Limit":     "notanumber",
				"X-RateLimit-Remaining": "200",
				"X-RateLimit-Reset":     "invalid",
			},
			expected: RateLimitInfo{
				Limit:     0,
				Remaining: 200,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{Header: http.Header{}}
			for k, v := range tc.headers {
				resp.Header.Set(k, v)
			}
			info := parseRateLimitHeaders(resp)
			if info.Limit != tc.expected.Limit {
				t.Errorf("Limit: got %d, want %d", info.Limit, tc.expected.Limit)
			}
			if info.Remaining != tc.expected.Remaining {
				t.Errorf("Remaining: got %d, want %d", info.Remaining, tc.expected.Remaining)
			}
			if info.Used != tc.expected.Used {
				t.Errorf("Used: got %d, want %d", info.Used, tc.expected.Used)
			}
			if !info.Reset.Equal(tc.expected.Reset) {
				t.Errorf("Reset: got %v, want %v", info.Reset, tc.expected.Reset)
			}
		})
	}
}

func TestClientGetRateLimit_NilWhenNoRequests(t *testing.T) {
	pemData, _ := generateTestKey(t)
	client, err := NewClient(Config{AppID: 123}, pemData)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if rl := client.GetRateLimit(); rl != nil {
		t.Errorf("expected nil rate limit before any requests, got %+v", rl)
	}
}

func TestClientGetRateLimit_ReturnsCopy(t *testing.T) {
	pemData, _ := generateTestKey(t)
	client, err := NewClient(Config{AppID: 123}, pemData)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	// Manually set rate limit to simulate a completed request
	client.mu.Lock()
	client.lastRateLimit = &RateLimitInfo{
		Limit:     5000,
		Remaining: 4500,
		Used:      500,
		Reset:     time.Unix(1700000000, 0),
	}
	client.mu.Unlock()

	rl := client.GetRateLimit()
	if rl == nil {
		t.Fatal("expected non-nil rate limit")
	}
	if rl.Limit != 5000 || rl.Remaining != 4500 || rl.Used != 500 {
		t.Errorf("unexpected rate limit values: %+v", rl)
	}

	// Verify it's a copy (modifying returned value shouldn't affect client)
	rl.Remaining = 0
	rl2 := client.GetRateLimit()
	if rl2.Remaining != 4500 {
		t.Error("GetRateLimit did not return a copy")
	}
}

func TestTrackRateLimit_UpdatesOnAPIResponse(t *testing.T) {
	// Set up a mock GitHub API that returns rate limit headers
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "4999")
		w.Header().Set("X-RateLimit-Used", "1")
		w.Header().Set("X-RateLimit-Reset", "1700000000")

		if r.URL.Path == "/app" {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id":   1,
				"name": "test-app",
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	pemData, _ := generateTestKey(t)
	client, err := NewClient(Config{
		AppID:      123,
		APIBaseURL: server.URL,
	}, pemData)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	// Before any request, rate limit should be nil
	if rl := client.GetRateLimit(); rl != nil {
		t.Fatal("expected nil rate limit before request")
	}

	// Make a request that triggers rate limit tracking
	_, err = client.GetApp(context.Background())
	if err != nil {
		t.Fatalf("GetApp: %v", err)
	}

	// Rate limit should now be populated
	rl := client.GetRateLimit()
	if rl == nil {
		t.Fatal("expected non-nil rate limit after request")
	}
	if rl.Limit != 5000 {
		t.Errorf("Limit: got %d, want 5000", rl.Limit)
	}
	if rl.Remaining != 4999 {
		t.Errorf("Remaining: got %d, want 4999", rl.Remaining)
	}
	if rl.Used != 1 {
		t.Errorf("Used: got %d, want 1", rl.Used)
	}
}

func TestParseAppIdentity(t *testing.T) {
	fetchedAt := time.Date(2026, 11, 16, 19, 30, 0, 0, time.UTC)
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(`{
		"id": 98765,
		"slug": "scion-app",
		"events": ["pull_request", "issue_comment", "issues", "pull_request"],
		"permissions": {
			"contents": "write",
			"issues": "read",
			"metadata": "read",
			"pull_requests": "write"
		}
	}`), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	id := ParseAppIdentity(raw, fetchedAt)
	if id.AppID != 98765 {
		t.Errorf("AppID = %d, want 98765", id.AppID)
	}
	if id.Slug != "scion-app" {
		t.Errorf("Slug = %q, want scion-app", id.Slug)
	}
	if id.BotLogin != "scion-app[bot]" {
		t.Errorf("BotLogin = %q, want scion-app[bot]", id.BotLogin)
	}
	wantEvents := []string{"issue_comment", "issues", "pull_request"}
	if len(id.Events) != len(wantEvents) {
		t.Fatalf("Events = %v, want %v", id.Events, wantEvents)
	}
	for i, ev := range wantEvents {
		if id.Events[i] != ev {
			t.Errorf("Events[%d] = %q, want %q", i, id.Events[i], ev)
		}
	}
	if id.Permissions["issues"] != "read" || id.Permissions["contents"] != "write" {
		t.Errorf("Permissions = %v", id.Permissions)
	}
	if !id.FetchedAt.Equal(fetchedAt) {
		t.Errorf("FetchedAt = %v, want %v", id.FetchedAt, fetchedAt)
	}

	clone := id.Clone()
	clone.Events[0] = "mutated"
	clone.Permissions["issues"] = "write"
	if id.Events[0] != "issue_comment" || id.Permissions["issues"] != "read" {
		t.Error("Clone shared backing slice or map with original")
	}
}

func TestGetApp_LargeBody(t *testing.T) {
	pemData, _ := generateTestKey(t)

	payloadMap := map[string]interface{}{
		"id":           424242,
		"slug":         "scion-enterprise-bot",
		"node_id":      "MDM6QXBwNDI0MjQy",
		"name":         "Scion Enterprise Bot",
		"description":  strings.Repeat("GitHub App integration for Scion automated agent workflows and pull request triggers. ", 24),
		"external_url": "https://scion.example.com",
		"html_url":     "https://github.com/apps/scion-enterprise-bot",
		"created_at":   "2026-01-15T10:00:00Z",
		"updated_at":   "2026-11-16T18:00:00Z",
		"owner": map[string]interface{}{
			"login":               "acme-corp",
			"id":                  1001,
			"node_id":             "MDEyOk9yZ2FuaXphdGlvbjEwMDE=",
			"avatar_url":          "https://avatars.githubusercontent.com/u/1001?v=4",
			"gravatar_id":         "",
			"url":                 "https://api.github.com/users/acme-corp",
			"html_url":            "https://github.com/acme-corp",
			"followers_url":       "https://api.github.com/users/acme-corp/followers",
			"following_url":       "https://api.github.com/users/acme-corp/following{/other_user}",
			"gists_url":           "https://api.github.com/users/acme-corp/gists{/gist_id}",
			"starred_url":         "https://api.github.com/users/acme-corp/starred{/owner}{/repo}",
			"subscriptions_url":   "https://api.github.com/users/acme-corp/subscriptions",
			"organizations_url":   "https://api.github.com/users/acme-corp/orgs",
			"repos_url":           "https://api.github.com/users/acme-corp/repos",
			"events_url":          "https://api.github.com/users/acme-corp/events{/privacy}",
			"received_events_url": "https://api.github.com/users/acme-corp/received_events",
			"type":                "Organization",
			"site_admin":          false,
		},
		"permissions": map[string]string{
			"actions":                     "read",
			"administration":              "read",
			"checks":                      "write",
			"contents":                    "write",
			"deployments":                 "read",
			"discussions":                 "write",
			"environments":                "read",
			"issues":                      "write",
			"merge_queues":                "read",
			"metadata":                    "read",
			"members":                     "read",
			"organization_administration": "read",
			"packages":                    "read",
			"pages":                       "read",
			"pull_requests":               "write",
			"repository_hooks":            "read",
			"security_events":             "read",
			"statuses":                    "write",
			"vulnerability_alerts":        "read",
			"workflows":                   "write",
		},
		"events": []string{
			"branch_protection_rule",
			"check_run",
			"check_suite",
			"commit_comment",
			"create",
			"delete",
			"deployment",
			"deployment_status",
			"discussion",
			"discussion_comment",
			"fork",
			"issue_comment",
			"issues",
			"label",
			"merge_group",
			"milestone",
			"page_build",
			"public",
			"pull_request",
			"pull_request_review",
			"pull_request_review_comment",
			"pull_request_review_thread",
			"push",
			"release",
			"repository",
			"status",
			"workflow_run",
		},
	}
	prettyBody, err := json.MarshalIndent(payloadMap, "", "  ")
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if len(prettyBody) <= upstreamBodyReadLimit {
		t.Fatalf("test fixture must exceed %d bytes, got %d", upstreamBodyReadLimit, len(prettyBody))
	}

	var returnError bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app" || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if returnError {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"Bad credentials","pad":"` + strings.Repeat("x", upstreamBodyReadLimit*2) + `"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(prettyBody)
	}))
	defer server.Close()

	client, err := NewClient(Config{
		AppID:      424242,
		APIBaseURL: server.URL,
	}, pemData)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	raw, err := client.GetApp(context.Background())
	if err != nil {
		t.Fatalf("GetApp failed on %d-byte response: %v", len(prettyBody), err)
	}

	id := ParseAppIdentity(raw, time.Date(2026, 11, 17, 4, 0, 0, 0, time.UTC))
	if id.AppID != 424242 || id.Slug != "scion-enterprise-bot" || id.BotLogin != "scion-enterprise-bot[bot]" {
		t.Fatalf("unexpected parsed identity: %+v", id)
	}
	if id.Permissions["pull_requests"] != "write" || id.Permissions["issues"] != "write" {
		t.Errorf("unexpected permissions: %v", id.Permissions)
	}

	// Non-200 responses stay capped at upstreamBodyReadLimit (4 KiB).
	returnError = true
	_, err = client.GetApp(context.Background())
	var mintErr *TokenMintError
	if !errors.As(err, &mintErr) {
		t.Fatalf("expected TokenMintError on 401, got %T: %v", err, err)
	}
	if len(mintErr.UpstreamDetail()) > upstreamBodyReadLimit {
		t.Errorf("error upstream detail exceeded %d bytes: got %d", upstreamBodyReadLimit, len(mintErr.UpstreamDetail()))
	}
}

func makeRealisticRepoObject(id int64, name string) map[string]interface{} {
	fullName := "acme-org/" + name
	return map[string]interface{}{
		"id":                id,
		"node_id":           fmt.Sprintf("R_kgDO%d", id),
		"name":              name,
		"full_name":         fullName,
		"private":           true,
		"description":       strings.Repeat("Repository metadata for "+name+". ", 40),
		"html_url":          "https://github.com/" + fullName,
		"url":               "https://api.github.com/repos/" + fullName,
		"clone_url":         "https://github.com/" + fullName + ".git",
		"ssh_url":           "git@github.com:" + fullName + ".git",
		"archive_url":       "https://api.github.com/repos/" + fullName + "/{archive_format}{/ref}",
		"assignees_url":     "https://api.github.com/repos/" + fullName + "/assignees{/user}",
		"blobs_url":         "https://api.github.com/repos/" + fullName + "/git/blobs{/sha}",
		"branches_url":      "https://api.github.com/repos/" + fullName + "/branches{/branch}",
		"collaborators_url": "https://api.github.com/repos/" + fullName + "/collaborators{/collaborator}",
		"comments_url":      "https://api.github.com/repos/" + fullName + "/comments{/number}",
		"commits_url":       "https://api.github.com/repos/" + fullName + "/commits{/sha}",
		"compare_url":       "https://api.github.com/repos/" + fullName + "/compare/{base}...{head}",
		"contents_url":      "https://api.github.com/repos/" + fullName + "/contents/{+path}",
		"deployments_url":   "https://api.github.com/repos/" + fullName + "/deployments",
		"downloads_url":     "https://api.github.com/repos/" + fullName + "/downloads",
		"events_url":        "https://api.github.com/repos/" + fullName + "/events",
		"forks_url":         "https://api.github.com/repos/" + fullName + "/forks",
		"git_commits_url":   "https://api.github.com/repos/" + fullName + "/git/commits{/sha}",
		"git_refs_url":      "https://api.github.com/repos/" + fullName + "/git/refs{/sha}",
		"git_tags_url":      "https://api.github.com/repos/" + fullName + "/git/tags{/sha}",
		"hooks_url":         "https://api.github.com/repos/" + fullName + "/hooks",
		"issue_comment_url": "https://api.github.com/repos/" + fullName + "/issues/comments{/number}",
		"issues_url":        "https://api.github.com/repos/" + fullName + "/issues{/number}",
		"keys_url":          "https://api.github.com/repos/" + fullName + "/keys{/key_id}",
		"labels_url":        "https://api.github.com/repos/" + fullName + "/labels{/name}",
		"pulls_url":         "https://api.github.com/repos/" + fullName + "/pulls{/number}",
		"releases_url":      "https://api.github.com/repos/" + fullName + "/releases{/id}",
		"statuses_url":      "https://api.github.com/repos/" + fullName + "/statuses/{sha}",
		"owner": map[string]interface{}{
			"login":               "acme-org",
			"id":                  1001,
			"avatar_url":          "https://avatars.githubusercontent.com/u/1001?v=4",
			"url":                 "https://api.github.com/users/acme-org",
			"html_url":            "https://github.com/acme-org",
			"repos_url":           "https://api.github.com/users/acme-org/repos",
			"events_url":          "https://api.github.com/users/acme-org/events{/privacy}",
			"received_events_url": "https://api.github.com/users/acme-org/received_events",
			"type":                "Organization",
		},
	}
}

func makeRealisticInstallationObject(id int64, login string) map[string]interface{} {
	return map[string]interface{}{
		"id":                   id,
		"app_id":               42,
		"target_type":          "Organization",
		"repository_selection": "selected",
		"access_tokens_url":    fmt.Sprintf("https://api.github.com/app/installations/%d/access_tokens", id),
		"repositories_url":     "https://api.github.com/installation/repositories",
		"html_url":             fmt.Sprintf("https://github.com/organizations/%s/settings/installations/%d", login, id),
		"account": map[string]interface{}{
			"login":               login,
			"id":                  2000 + id,
			"type":                "Organization",
			"avatar_url":          fmt.Sprintf("https://avatars.githubusercontent.com/u/%d?v=4", 2000+id),
			"url":                 "https://api.github.com/users/" + login,
			"html_url":            "https://github.com/" + login,
			"followers_url":       "https://api.github.com/users/" + login + "/followers",
			"following_url":       "https://api.github.com/users/" + login + "/following{/other_user}",
			"gists_url":           "https://api.github.com/users/" + login + "/gists{/gist_id}",
			"starred_url":         "https://api.github.com/users/" + login + "/starred{/owner}{/repo}",
			"subscriptions_url":   "https://api.github.com/users/" + login + "/subscriptions",
			"organizations_url":   "https://api.github.com/users/" + login + "/orgs",
			"repos_url":           "https://api.github.com/users/" + login + "/repos",
			"events_url":          "https://api.github.com/users/" + login + "/events{/privacy}",
			"received_events_url": "https://api.github.com/users/" + login + "/received_events",
		},
		"permissions": map[string]string{
			"contents":      "write",
			"issues":        "write",
			"metadata":      "read",
			"pull_requests": "write",
		},
		"events": []string{
			"check_run", "check_suite", "commit_comment", "create", "delete",
			"issue_comment", "issues", "pull_request", "pull_request_review",
			"pull_request_review_comment", "push", "repository",
		},
		"extra_padding": strings.Repeat("installation-metadata-", 200),
	}
}

func TestMintInstallationToken_LargeSuccessBody(t *testing.T) {
	pemData, _ := generateTestKey(t)
	expiresAt := time.Now().Add(1 * time.Hour).UTC().Truncate(time.Second)

	payload := map[string]interface{}{
		"token":                "ghs_large_repo_scoped_token",
		"expires_at":           expiresAt.Format(time.RFC3339),
		"permissions":          map[string]string{"contents": "write", "metadata": "read"},
		"repository_selection": "selected",
		"repositories": []interface{}{
			makeRealisticRepoObject(101, "service-alpha"),
			makeRealisticRepoObject(102, "service-beta"),
		},
	}
	body, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if len(body) <= upstreamBodyReadLimit {
		t.Fatalf("fixture must exceed %d bytes, got %d", upstreamBodyReadLimit, len(body))
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(body)
	}))
	defer server.Close()

	client, err := NewClient(Config{AppID: 42, APIBaseURL: server.URL}, pemData)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	tok, err := client.MintInstallationToken(context.Background(), 12345, []string{"service-alpha", "service-beta"}, DefaultTokenPermissions())
	if err != nil {
		t.Fatalf("MintInstallationToken failed on %d-byte response: %v", len(body), err)
	}
	if tok.Token != "ghs_large_repo_scoped_token" {
		t.Errorf("Token = %q, want ghs_large_repo_scoped_token", tok.Token)
	}
	if !tok.ExpiresAt.Equal(expiresAt) {
		t.Errorf("ExpiresAt = %v, want %v", tok.ExpiresAt, expiresAt)
	}
	if tok.Permissions["contents"] != "write" || tok.Permissions["metadata"] != "read" {
		t.Errorf("unexpected permissions: %v", tok.Permissions)
	}
}

func TestGetInstallation_LargeSuccessBody(t *testing.T) {
	pemData, _ := generateTestKey(t)
	body, err := json.MarshalIndent(makeRealisticInstallationObject(777, "acme-org"), "", "  ")
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if len(body) <= upstreamBodyReadLimit {
		t.Fatalf("fixture must exceed %d bytes, got %d", upstreamBodyReadLimit, len(body))
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	defer server.Close()

	client, err := NewClient(Config{AppID: 42, APIBaseURL: server.URL}, pemData)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	inst, err := client.GetInstallation(context.Background(), 777)
	if err != nil {
		t.Fatalf("GetInstallation failed on %d-byte response: %v", len(body), err)
	}
	if inst.ID != 777 || inst.Account.Login != "acme-org" || inst.RepositorySelection != "selected" {
		t.Errorf("unexpected installation: %+v", inst)
	}
}

func TestListInstallations_LargeSuccessBody(t *testing.T) {
	pemData, _ := generateTestKey(t)
	items := []interface{}{
		makeRealisticInstallationObject(10, "org-one"),
		makeRealisticInstallationObject(20, "org-two"),
	}
	body, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if len(body) <= upstreamBodyReadLimit {
		t.Fatalf("fixture must exceed %d bytes, got %d", upstreamBodyReadLimit, len(body))
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	defer server.Close()

	client, err := NewClient(Config{AppID: 42, APIBaseURL: server.URL}, pemData)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	got, err := client.ListInstallations(context.Background())
	if err != nil {
		t.Fatalf("ListInstallations failed on %d-byte response: %v", len(body), err)
	}
	if len(got) != 2 || got[0].Account.Login != "org-one" || got[1].Account.Login != "org-two" {
		t.Errorf("unexpected installations: %+v", got)
	}
}

func TestListInstallationRepos_LargeSuccessBody(t *testing.T) {
	pemData, _ := generateTestKey(t)
	reposBody, err := json.MarshalIndent(map[string]interface{}{
		"total_count": 2,
		"repositories": []interface{}{
			makeRealisticRepoObject(201, "repo-one"),
			makeRealisticRepoObject(202, "repo-two"),
		},
	}, "", "  ")
	if err != nil {
		t.Fatalf("marshal repos payload: %v", err)
	}
	if len(reposBody) <= upstreamBodyReadLimit {
		t.Fatalf("fixture must exceed %d bytes, got %d", upstreamBodyReadLimit, len(reposBody))
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/access_tokens") {
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(accessTokenResponse{Token: "ghs_list_repos_tok"})
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(reposBody)
	}))
	defer server.Close()

	client, err := NewClient(Config{AppID: 42, APIBaseURL: server.URL}, pemData)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	repos, err := client.ListInstallationRepos(context.Background(), 12345)
	if err != nil {
		t.Fatalf("ListInstallationRepos failed on %d-byte response: %v", len(reposBody), err)
	}
	if len(repos) != 2 || repos[0].FullName != "acme-org/repo-one" || repos[1].FullName != "acme-org/repo-two" {
		t.Errorf("unexpected repos: %+v", repos)
	}
}

func TestReadBoundedBody(t *testing.T) {
	t.Run("exact limit succeeds", func(t *testing.T) {
		input := strings.Repeat("a", 16)
		got, err := readBoundedBody(strings.NewReader(input), 16)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if string(got) != input {
			t.Errorf("got %q, want %q", string(got), input)
		}
	})

	t.Run("one byte over limit fails without truncating", func(t *testing.T) {
		input := strings.Repeat("a", 17)
		got, err := readBoundedBody(strings.NewReader(input), 16)
		if !errors.Is(err, errResponseBodyTooLarge) {
			t.Fatalf("expected errResponseBodyTooLarge, got %v", err)
		}
		if got != nil {
			t.Errorf("expected nil body on overflow, got %d bytes", len(got))
		}
	})
}

func TestSuccessBodyOverLimitRejected(t *testing.T) {
	pemData, _ := generateTestKey(t)

	// Valid JSON whose total size exceeds githubSuccessBodyReadLimit by a few bytes.
	oversized := `{"token":"ghs_should_not_parse","pad":"` + strings.Repeat("x", githubSuccessBodyReadLimit) + `"}`
	oversizedApp := `{"id":1,"pad":"` + strings.Repeat("x", appMetadataReadLimit) + `"}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/app/installations/12345/access_tokens":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(oversized))
		case "/app/installations/99999/access_tokens":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"ghs_ok"}`))
		case "/app":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(oversizedApp))
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(oversized))
		}
	}))
	defer server.Close()

	client, err := NewClient(Config{AppID: 42, APIBaseURL: server.URL}, pemData)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	t.Run("MintInstallationToken", func(t *testing.T) {
		_, err := client.MintInstallationToken(context.Background(), 12345, []string{"repo"}, DefaultTokenPermissions())
		var mintErr *TokenMintError
		if !errors.As(err, &mintErr) {
			t.Fatalf("expected TokenMintError, got %T: %v", err, err)
		}
		if mintErr.ErrorCode != ErrCodeTokenMintFailed {
			t.Errorf("ErrorCode = %q, want %q", mintErr.ErrorCode, ErrCodeTokenMintFailed)
		}
		if !errors.Is(err, errResponseBodyTooLarge) {
			t.Errorf("expected error wrapping errResponseBodyTooLarge, got %v", err)
		}
		if strings.Contains(mintErr.Message, "ghs_should_not_parse") || strings.Contains(mintErr.UpstreamDetail(), "ghs_should_not_parse") {
			t.Error("response body content leaked into error")
		}
		if mintErr.UpstreamDetail() == "" {
			t.Error("expected diagnostic UpstreamDetail for oversized response")
		}
	})

	t.Run("GetInstallation", func(t *testing.T) {
		_, err := client.GetInstallation(context.Background(), 12345)
		if !errors.Is(err, errResponseBodyTooLarge) {
			t.Fatalf("expected errResponseBodyTooLarge, got %v", err)
		}
	})

	t.Run("ListInstallations", func(t *testing.T) {
		_, err := client.ListInstallations(context.Background())
		if !errors.Is(err, errResponseBodyTooLarge) {
			t.Fatalf("expected errResponseBodyTooLarge, got %v", err)
		}
	})

	t.Run("ListInstallationRepos", func(t *testing.T) {
		_, err := client.ListInstallationRepos(context.Background(), 99999)
		if !errors.Is(err, errResponseBodyTooLarge) {
			t.Fatalf("expected errResponseBodyTooLarge, got %v", err)
		}
	})

	t.Run("GetApp", func(t *testing.T) {
		_, err := client.GetApp(context.Background())
		if !errors.Is(err, errResponseBodyTooLarge) {
			t.Fatalf("expected errResponseBodyTooLarge, got %v", err)
		}
	})
}

func TestListingCalls_ErrorBodiesRemainCapped(t *testing.T) {
	pemData, _ := generateTestKey(t)
	const tailMarker = "PAST-ERROR-CAP-MARKER"
	largeErrBody := strings.Repeat("E", upstreamBodyReadLimit) + tailMarker

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(largeErrBody))
	}))
	defer server.Close()

	client, err := NewClient(Config{AppID: 42, APIBaseURL: server.URL}, pemData)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"GetInstallation", func() error { _, err := client.GetInstallation(context.Background(), 1); return err }},
		{"ListInstallations", func() error { _, err := client.ListInstallations(context.Background()); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			var mintErr *TokenMintError
			if !errors.As(err, &mintErr) {
				t.Fatalf("expected TokenMintError, got %T: %v", err, err)
			}
			if n := len(mintErr.UpstreamDetail()); n != upstreamBodyReadLimit {
				t.Errorf("UpstreamDetail length = %d, want %d", n, upstreamBodyReadLimit)
			}
			if strings.Contains(mintErr.UpstreamDetail(), tailMarker) {
				t.Error("bytes past upstreamBodyReadLimit were read")
			}
		})
	}
}

func TestMintInstallationToken_ParseErrorPopulatesUpstreamDetail(t *testing.T) {
	pemData, _ := generateTestKey(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"ghs_truncated"`))
	}))
	defer server.Close()

	client, err := NewClient(Config{AppID: 42, APIBaseURL: server.URL}, pemData)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	_, err = client.MintInstallationToken(context.Background(), 12345, []string{"repo"}, DefaultTokenPermissions())
	var mintErr *TokenMintError
	if !errors.As(err, &mintErr) {
		t.Fatalf("expected TokenMintError, got %T: %v", err, err)
	}
	if mintErr.ErrorCode != ErrCodeTokenMintFailed {
		t.Errorf("ErrorCode = %q, want %q", mintErr.ErrorCode, ErrCodeTokenMintFailed)
	}
	if mintErr.UpstreamDetail() == "" {
		t.Error("expected parse error detail in UpstreamDetail for server logging")
	}
	if strings.Contains(mintErr.UpstreamDetail(), "ghs_truncated") || strings.Contains(mintErr.Message, "ghs_truncated") {
		t.Error("response body leaked into error")
	}
}

func TestIntersectTokenPermissions(t *testing.T) {
	defaults := DefaultTokenPermissions()
	tests := []struct {
		name      string
		requested TokenPermissions
		appPerms  map[string]string
		want      TokenPermissions
	}{
		{
			name:      "full grant preserves defaults",
			requested: defaults,
			appPerms: map[string]string{
				"contents":      "write",
				"pull_requests": "write",
				"issues":        "write",
				"metadata":      "read",
			},
			want: defaults,
		},
		{
			name:      "read-only app lowers contents and drops pull_requests and issues",
			requested: defaults,
			appPerms: map[string]string{
				"contents": "read",
				"metadata": "read",
			},
			want: TokenPermissions{
				Contents: "read",
				Metadata: "read",
			},
		},
		{
			name:      "git-only app without issues drops issues only",
			requested: defaults,
			appPerms: map[string]string{
				"contents":      "write",
				"pull_requests": "write",
				"metadata":      "read",
			},
			want: TokenPermissions{
				Contents:     "write",
				PullRequests: "write",
				Metadata:     "read",
			},
		},
		{
			name:      "cannot widen read to write or write to admin or add unrequested fields",
			requested: defaults,
			appPerms: map[string]string{
				"contents":       "admin",
				"pull_requests":  "admin",
				"issues":         "read",
				"metadata":       "write",
				"checks":         "write",
				"actions":        "write",
				"administration": "admin",
				"secrets":        "write",
			},
			want: TokenPermissions{
				Contents:     "write",
				PullRequests: "write",
				Issues:       "read",
				Metadata:     "read",
			},
		},
		{
			name: "drops unknown requested and app levels",
			requested: TokenPermissions{
				Contents: "write",
				Checks:   "admin",
				Actions:  "read",
				Metadata: "read",
			},
			appPerms: map[string]string{
				"contents": "none",
				"checks":   "admin",
				"actions":  "bogus",
				"metadata": "read",
			},
			want: TokenPermissions{
				Metadata: "read",
			},
		},
		{
			name:      "nil app permissions returns empty",
			requested: defaults,
			appPerms:  nil,
			want:      TokenPermissions{},
		},
		{
			name:      "empty app permissions returns empty",
			requested: defaults,
			appPerms:  map[string]string{},
			want:      TokenPermissions{},
		},
		{
			name:      "disjoint app permissions returns empty",
			requested: defaults,
			appPerms: map[string]string{
				"actions": "write",
				"checks":  "read",
			},
			want: TokenPermissions{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IntersectTokenPermissions(tt.requested, tt.appPerms)
			if got != tt.want {
				t.Errorf("IntersectTokenPermissions(%+v, %v) = %+v, want %+v", tt.requested, tt.appPerms, got, tt.want)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

type failingReadCloser struct {
	err error
}

func (f failingReadCloser) Read([]byte) (int, error) { return 0, f.err }
func (f failingReadCloser) Close() error             { return nil }

func TestMintInstallationToken_Non201ReadErrorSetsStatusCode(t *testing.T) {
	pemData, _ := generateTestKey(t)
	client, err := NewClient(Config{AppID: 42, APIBaseURL: "https://api.github.test"}, pemData)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	client.httpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadGateway,
			Header:     make(http.Header),
			Body:       failingReadCloser{err: errors.New("connection reset by peer")},
		}, nil
	})

	_, err = client.MintInstallationToken(context.Background(), 12345, []string{"repo"}, DefaultTokenPermissions())
	var mintErr *TokenMintError
	if !errors.As(err, &mintErr) {
		t.Fatalf("expected TokenMintError, got %T: %v", err, err)
	}
	if mintErr.ErrorCode != ErrCodeTokenMintFailed {
		t.Errorf("ErrorCode = %q, want %q", mintErr.ErrorCode, ErrCodeTokenMintFailed)
	}
	if mintErr.StatusCode != http.StatusBadGateway {
		t.Errorf("StatusCode = %d, want %d", mintErr.StatusCode, http.StatusBadGateway)
	}
	if mintErr.UpstreamDetail() == "" {
		t.Error("expected UpstreamDetail for non-201 read error")
	}
}
