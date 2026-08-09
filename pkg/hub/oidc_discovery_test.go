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
