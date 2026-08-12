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

package hub

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Test 1: Valid discovery — mock returns {"jwks_uri": "..."}, function returns the URL
func TestDiscoverJWKSURL_ValidDiscovery(t *testing.T) {
	expectedJWKSURL := "https://example.com/keys"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"issuer": "https://example.com", "jwks_uri": "` + expectedJWKSURL + `"}`))
	}))
	t.Cleanup(srv.Close)

	client := &http.Client{Timeout: 5 * time.Second}
	jwksURL, err := discoverJWKSURL(srv.URL, client, false)
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}
	if jwksURL != expectedJWKSURL {
		t.Errorf("expected jwks_uri %q, got %q", expectedJWKSURL, jwksURL)
	}
}

// Test 2: Missing jwks_uri — mock returns {} or {"issuer": "..."}, function returns error
func TestDiscoverJWKSURL_MissingJWKSURI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"issuer": "https://example.com"}`))
	}))
	t.Cleanup(srv.Close)

	client := &http.Client{Timeout: 5 * time.Second}
	_, err := discoverJWKSURL(srv.URL, client, false)
	if err == nil {
		t.Fatal("expected error for missing jwks_uri, got nil")
	}
	if !strings.Contains(err.Error(), "no jwks_uri") {
		t.Errorf("expected 'no jwks_uri' in error, got: %v", err)
	}
}

// Test 3: Non-200 status — mock returns 404, function returns error
func TestDiscoverJWKSURL_Non200Status(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	client := &http.Client{Timeout: 5 * time.Second}
	_, err := discoverJWKSURL(srv.URL, client, false)
	if err == nil {
		t.Fatal("expected error for non-200 status, got nil")
	}
	if !strings.Contains(err.Error(), "returned status 404") {
		t.Errorf("expected 'returned status 404' in error, got: %v", err)
	}
}

// Test 4: Invalid JSON — mock returns garbage, function returns error
func TestDiscoverJWKSURL_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`this is not valid json`))
	}))
	t.Cleanup(srv.Close)

	client := &http.Client{Timeout: 5 * time.Second}
	_, err := discoverJWKSURL(srv.URL, client, false)
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
	if !strings.Contains(err.Error(), "failed to parse response") {
		t.Errorf("expected 'failed to parse response' in error, got: %v", err)
	}
}

// Test 5: Network error — use an unreachable URL, function returns error
func TestDiscoverJWKSURL_NetworkError(t *testing.T) {
	client := &http.Client{Timeout: 1 * time.Second}
	_, err := discoverJWKSURL("http://127.0.0.1:1", client, false)
	if err == nil {
		t.Fatal("expected error for network error, got nil")
	}
	if !strings.Contains(err.Error(), "failed to fetch") {
		t.Errorf("expected 'failed to fetch' in error, got: %v", err)
	}
}

// Test 6: Trailing slash normalization — issuerURL with trailing slash still works
func TestDiscoverJWKSURL_TrailingSlashNormalization(t *testing.T) {
	expectedJWKSURL := "https://example.com/keys"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The path should be /.well-known/openid-configuration (not //.well-known/...)
		if r.URL.Path != "/.well-known/openid-configuration" {
			t.Errorf("unexpected path %q (double slash?)", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jwks_uri": "` + expectedJWKSURL + `"}`))
	}))
	t.Cleanup(srv.Close)

	client := &http.Client{Timeout: 5 * time.Second}
	// Issuer URL with trailing slash
	jwksURL, err := discoverJWKSURL(srv.URL+"/", client, false)
	if err != nil {
		t.Fatalf("expected success with trailing slash, got error: %v", err)
	}
	if jwksURL != expectedJWKSURL {
		t.Errorf("expected jwks_uri %q, got %q", expectedJWKSURL, jwksURL)
	}
}

// Test 7: requireHTTPS=true rejects HTTP jwks_uri
func TestDiscoverJWKSURL_RequireHTTPS_RejectsHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jwks_uri": "http://example.com/keys"}`))
	}))
	t.Cleanup(srv.Close)

	client := &http.Client{Timeout: 5 * time.Second}
	_, err := discoverJWKSURL(srv.URL, client, true)
	if err == nil {
		t.Fatal("expected error for HTTP jwks_uri with requireHTTPS=true, got nil")
	}
	if !strings.Contains(err.Error(), "must use HTTPS") {
		t.Errorf("expected 'must use HTTPS' in error, got: %v", err)
	}
}

// Test 8: requireHTTPS=true accepts HTTPS jwks_uri
func TestDiscoverJWKSURL_RequireHTTPS_AcceptsHTTPS(t *testing.T) {
	expectedJWKSURL := "https://example.com/keys"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jwks_uri": "` + expectedJWKSURL + `"}`))
	}))
	t.Cleanup(srv.Close)

	client := &http.Client{Timeout: 5 * time.Second}
	jwksURL, err := discoverJWKSURL(srv.URL, client, true)
	if err != nil {
		t.Fatalf("expected success for HTTPS jwks_uri with requireHTTPS=true, got error: %v", err)
	}
	if jwksURL != expectedJWKSURL {
		t.Errorf("expected jwks_uri %q, got %q", expectedJWKSURL, jwksURL)
	}
}

// --- Tests for discoverOIDCEndpoints ---

func TestDiscoverOIDCEndpoints_ValidDiscovery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"issuer": "https://example.com",
			"authorization_endpoint": "https://example.com/auth",
			"token_endpoint": "https://example.com/token",
			"userinfo_endpoint": "https://example.com/userinfo",
			"jwks_uri": "https://example.com/keys"
		}`))
	}))
	t.Cleanup(srv.Close)

	client := &http.Client{Timeout: 5 * time.Second}
	doc, err := discoverOIDCEndpoints(srv.URL, client)
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}
	if doc.Issuer != "https://example.com" {
		t.Errorf("expected issuer %q, got %q", "https://example.com", doc.Issuer)
	}
	if doc.AuthorizationEndpoint != "https://example.com/auth" {
		t.Errorf("expected authorization_endpoint %q, got %q", "https://example.com/auth", doc.AuthorizationEndpoint)
	}
	if doc.TokenEndpoint != "https://example.com/token" {
		t.Errorf("expected token_endpoint %q, got %q", "https://example.com/token", doc.TokenEndpoint)
	}
	if doc.UserinfoEndpoint != "https://example.com/userinfo" {
		t.Errorf("expected userinfo_endpoint %q, got %q", "https://example.com/userinfo", doc.UserinfoEndpoint)
	}
	if doc.JWKSURI != "https://example.com/keys" {
		t.Errorf("expected jwks_uri %q, got %q", "https://example.com/keys", doc.JWKSURI)
	}
}

func TestDiscoverOIDCEndpoints_MissingAuthorizationEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"issuer": "https://example.com",
			"token_endpoint": "https://example.com/token",
			"userinfo_endpoint": "https://example.com/userinfo"
		}`))
	}))
	t.Cleanup(srv.Close)

	client := &http.Client{Timeout: 5 * time.Second}
	_, err := discoverOIDCEndpoints(srv.URL, client)
	if err == nil {
		t.Fatal("expected error for missing authorization_endpoint, got nil")
	}
	if !strings.Contains(err.Error(), "no authorization_endpoint") {
		t.Errorf("expected 'no authorization_endpoint' in error, got: %v", err)
	}
}

func TestDiscoverOIDCEndpoints_MissingTokenEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"issuer": "https://example.com",
			"authorization_endpoint": "https://example.com/auth",
			"userinfo_endpoint": "https://example.com/userinfo"
		}`))
	}))
	t.Cleanup(srv.Close)

	client := &http.Client{Timeout: 5 * time.Second}
	_, err := discoverOIDCEndpoints(srv.URL, client)
	if err == nil {
		t.Fatal("expected error for missing token_endpoint, got nil")
	}
	if !strings.Contains(err.Error(), "no token_endpoint") {
		t.Errorf("expected 'no token_endpoint' in error, got: %v", err)
	}
}

func TestDiscoverOIDCEndpoints_MissingUserinfoEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"issuer": "https://example.com",
			"authorization_endpoint": "https://example.com/auth",
			"token_endpoint": "https://example.com/token"
		}`))
	}))
	t.Cleanup(srv.Close)

	client := &http.Client{Timeout: 5 * time.Second}
	_, err := discoverOIDCEndpoints(srv.URL, client)
	if err == nil {
		t.Fatal("expected error for missing userinfo_endpoint, got nil")
	}
	if !strings.Contains(err.Error(), "no userinfo_endpoint") {
		t.Errorf("expected 'no userinfo_endpoint' in error, got: %v", err)
	}
}

func TestDiscoverOIDCEndpoints_Non200Status(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	client := &http.Client{Timeout: 5 * time.Second}
	_, err := discoverOIDCEndpoints(srv.URL, client)
	if err == nil {
		t.Fatal("expected error for non-200 status, got nil")
	}
	if !strings.Contains(err.Error(), "returned status 404") {
		t.Errorf("expected 'returned status 404' in error, got: %v", err)
	}
}

func TestDiscoverOIDCEndpoints_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`not valid json`))
	}))
	t.Cleanup(srv.Close)

	client := &http.Client{Timeout: 5 * time.Second}
	_, err := discoverOIDCEndpoints(srv.URL, client)
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
	if !strings.Contains(err.Error(), "failed to parse response") {
		t.Errorf("expected 'failed to parse response' in error, got: %v", err)
	}
}

func TestDiscoverOIDCEndpoints_TrailingSlashNormalization(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			t.Errorf("unexpected path %q (double slash?)", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"issuer": "https://example.com",
			"authorization_endpoint": "https://example.com/auth",
			"token_endpoint": "https://example.com/token",
			"userinfo_endpoint": "https://example.com/userinfo"
		}`))
	}))
	t.Cleanup(srv.Close)

	client := &http.Client{Timeout: 5 * time.Second}
	doc, err := discoverOIDCEndpoints(srv.URL+"/", client)
	if err != nil {
		t.Fatalf("expected success with trailing slash, got error: %v", err)
	}
	if doc.AuthorizationEndpoint != "https://example.com/auth" {
		t.Errorf("expected authorization_endpoint %q, got %q", "https://example.com/auth", doc.AuthorizationEndpoint)
	}
}

func TestDiscoverOIDCEndpoints_NetworkError(t *testing.T) {
	client := &http.Client{Timeout: 1 * time.Second}
	_, err := discoverOIDCEndpoints("http://127.0.0.1:1", client)
	if err == nil {
		t.Fatal("expected error for network error, got nil")
	}
	if !strings.Contains(err.Error(), "failed to fetch") {
		t.Errorf("expected 'failed to fetch' in error, got: %v", err)
	}
}

// --- Tests for HTTPS endpoint scheme validation ---

func TestDiscoverOIDCEndpoints_RejectsHTTPEndpoints(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"issuer": "https://example.com",
			"authorization_endpoint": "http://evil.com/auth",
			"token_endpoint": "https://example.com/token",
			"userinfo_endpoint": "https://example.com/userinfo"
		}`))
	}))
	t.Cleanup(srv.Close)

	client := &http.Client{Timeout: 5 * time.Second}
	_, err := discoverOIDCEndpoints(srv.URL, client)
	if err == nil {
		t.Fatal("expected error for HTTP authorization_endpoint, got nil")
	}
	if !strings.Contains(err.Error(), "must use HTTPS") {
		t.Errorf("expected 'must use HTTPS' in error, got: %v", err)
	}
}

func TestDiscoverOIDCEndpoints_AllowsHTTPLocalhostEndpoints(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"issuer": "http://localhost",
			"authorization_endpoint": "http://localhost:8080/auth",
			"token_endpoint": "http://127.0.0.1:8080/token",
			"userinfo_endpoint": "http://localhost:8080/userinfo"
		}`))
	}))
	t.Cleanup(srv.Close)

	client := &http.Client{Timeout: 5 * time.Second}
	doc, err := discoverOIDCEndpoints(srv.URL, client)
	if err != nil {
		t.Fatalf("expected success for localhost HTTP endpoints, got error: %v", err)
	}
	if doc.AuthorizationEndpoint != "http://localhost:8080/auth" {
		t.Errorf("unexpected authorization_endpoint: %s", doc.AuthorizationEndpoint)
	}
}

func TestValidateOIDCEndpointScheme(t *testing.T) {
	tests := []struct {
		name      string
		endpoint  string
		wantError bool
	}{
		{"https is valid", "https://idp.example.com/auth", false},
		{"http is rejected", "http://idp.example.com/auth", true},
		{"http localhost is allowed", "http://localhost:8080/auth", false},
		{"http 127.0.0.1 is allowed", "http://127.0.0.1:9090/token", false},
		{"ftp is rejected", "ftp://example.com/auth", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateOIDCEndpointScheme(tc.endpoint, "test_endpoint")
			if tc.wantError && err == nil {
				t.Errorf("expected error for %q, got nil", tc.endpoint)
			}
			if !tc.wantError && err != nil {
				t.Errorf("expected no error for %q, got: %v", tc.endpoint, err)
			}
		})
	}
}
