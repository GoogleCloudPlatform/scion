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

package metadata

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

func TestShouldAttemptMetadataInterception(t *testing.T) {
	tests := []struct {
		name        string
		uid         int
		networkMode string
		want        bool
	}{
		{name: "root", uid: 0, networkMode: "", want: true},
		{name: "non-root", uid: 1000, networkMode: "", want: false},
		{name: "root-host-network", uid: 0, networkMode: "host", want: false},
		{name: "non-root-host-network", uid: 1000, networkMode: "host", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldAttemptMetadataInterception(tt.uid, tt.networkMode); got != tt.want {
				t.Fatalf("shouldAttemptMetadataInterception(%d, %q) = %v, want %v", tt.uid, tt.networkMode, got, tt.want)
			}
		})
	}
}

func TestMetadataServer_HealthCheck(t *testing.T) {
	port := freePort(t)
	srv := New(Config{
		Mode:      "block",
		Port:      port,
		ProjectID: "test-project",
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()
	time.Sleep(50 * time.Millisecond)

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if string(body) != "OK" {
		t.Fatalf("expected OK, got %q", string(body))
	}

	if resp.Header.Get("Metadata-Flavor") != "Google" {
		t.Fatal("expected Metadata-Flavor: Google header")
	}
}

func TestMetadataServer_RequiresMetadataFlavorHeader(t *testing.T) {
	port := freePort(t)
	srv := New(Config{
		Mode:      "block",
		Port:      port,
		ProjectID: "test-project",
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()
	time.Sleep(50 * time.Millisecond)

	// Request without Metadata-Flavor header should get 403
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/computeMetadata/v1/project/project-id", port))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 without Metadata-Flavor header, got %d", resp.StatusCode)
	}
}

func metadataGet(t *testing.T, port int, path string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d%s", port, path), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp, string(body)
}

func TestMetadataServer_ProjectID(t *testing.T) {
	port := freePort(t)
	srv := New(Config{
		Mode:      "block",
		Port:      port,
		ProjectID: "my-test-project",
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()
	time.Sleep(50 * time.Millisecond)

	resp, body := metadataGet(t, port, "/computeMetadata/v1/project/project-id")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if body != "my-test-project" {
		t.Fatalf("expected my-test-project, got %q", body)
	}
}

func TestMetadataServer_NumericProjectID(t *testing.T) {
	port := freePort(t)
	srv := New(Config{
		Mode:      "block",
		Port:      port,
		ProjectID: "my-test-project",
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()
	time.Sleep(50 * time.Millisecond)

	resp, body := metadataGet(t, port, "/computeMetadata/v1/project/numeric-project-id")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if body != "0" {
		t.Fatalf("expected numeric-project-id to be \"0\", got %q", body)
	}
}

func TestMetadataServer_BlockMode(t *testing.T) {
	port := freePort(t)
	srv := New(Config{
		Mode:      "block",
		Port:      port,
		ProjectID: "test-project",
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()
	time.Sleep(50 * time.Millisecond)

	// Token endpoint should return 403
	resp, _ := metadataGet(t, port, "/computeMetadata/v1/instance/service-accounts/default/token")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for token in block mode, got %d", resp.StatusCode)
	}

	// Email endpoint should return 403
	resp, _ = metadataGet(t, port, "/computeMetadata/v1/instance/service-accounts/default/email")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for email in block mode, got %d", resp.StatusCode)
	}

	// Service account listing should return 403
	resp, _ = metadataGet(t, port, "/computeMetadata/v1/instance/service-accounts/")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for SA listing in block mode, got %d", resp.StatusCode)
	}

	// Project ID should still work in block mode
	resp, body := metadataGet(t, port, "/computeMetadata/v1/project/project-id")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for project-id in block mode, got %d", resp.StatusCode)
	}
	if body != "test-project" {
		t.Fatalf("expected test-project, got %q", body)
	}
}

func TestMetadataServer_AssignMode_SAEndpoints(t *testing.T) {
	// Create a mock Hub that returns tokens
	hubServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent/gcp-token":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"access_token": "ya29.test-token",
				"expires_in":   3599,
				"token_type":   "Bearer",
			})
		case "/api/v1/agent/gcp-identity-token":
			_ = json.NewEncoder(w).Encode(map[string]string{
				"token": "eyJhbGciOiJSUzI1NiIs.test-id-token",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer hubServer.Close()

	port := freePort(t)
	srv := New(Config{
		Mode:      "assign",
		Port:      port,
		SAEmail:   "agent-worker@project.iam.gserviceaccount.com",
		ProjectID: "my-project",
		HubURL:    hubServer.URL,
		AuthToken: "test-auth-token",
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()
	time.Sleep(50 * time.Millisecond)

	// Email endpoint
	resp, body := metadataGet(t, port, "/computeMetadata/v1/instance/service-accounts/default/email")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for email, got %d", resp.StatusCode)
	}
	if body != "agent-worker@project.iam.gserviceaccount.com" {
		t.Fatalf("unexpected email: %q", body)
	}

	// Scopes endpoint
	resp, body = metadataGet(t, port, "/computeMetadata/v1/instance/service-accounts/default/scopes")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for scopes, got %d", resp.StatusCode)
	}
	if body != "https://www.googleapis.com/auth/cloud-platform" {
		t.Fatalf("unexpected scopes: %q", body)
	}

	// Token endpoint (goes to mock Hub)
	resp, body = metadataGet(t, port, "/computeMetadata/v1/instance/service-accounts/default/token")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for token, got %d: %s", resp.StatusCode, body)
	}

	var tokenResp map[string]interface{}
	if err := json.Unmarshal([]byte(body), &tokenResp); err != nil {
		t.Fatalf("failed to parse token response: %v", err)
	}
	if tokenResp["access_token"] != "ya29.test-token" {
		t.Fatalf("unexpected access_token: %v", tokenResp["access_token"])
	}
	if tokenResp["token_type"] != "Bearer" {
		t.Fatalf("unexpected token_type: %v", tokenResp["token_type"])
	}

	// Service account listing
	resp, body = metadataGet(t, port, "/computeMetadata/v1/instance/service-accounts/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for SA listing, got %d", resp.StatusCode)
	}
	if body != "default/\nagent-worker@project.iam.gserviceaccount.com/\n" {
		t.Fatalf("unexpected SA listing: %q", body)
	}

	// Identity token endpoint
	resp, body = metadataGet(t, port, "/computeMetadata/v1/instance/service-accounts/default/identity?audience=https://example.com")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for identity token, got %d: %s", resp.StatusCode, body)
	}
	if body != "eyJhbGciOiJSUzI1NiIs.test-id-token" {
		t.Fatalf("unexpected identity token: %q", body)
	}

	// Token endpoint with email instead of default
	resp, _ = metadataGet(t, port, "/computeMetadata/v1/instance/service-accounts/agent-worker@project.iam.gserviceaccount.com/token")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for token via email, got %d", resp.StatusCode)
	}

	// Unknown SA should 404
	resp, _ = metadataGet(t, port, "/computeMetadata/v1/instance/service-accounts/unknown@project.iam.gserviceaccount.com/token")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown SA, got %d", resp.StatusCode)
	}
}

func TestMetadataServer_AssignMode_TokenCaching(t *testing.T) {
	requestCount := 0
	hubServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token": fmt.Sprintf("ya29.token-%d", requestCount),
			"expires_in":   3599,
			"token_type":   "Bearer",
		})
	}))
	defer hubServer.Close()

	port := freePort(t)
	srv := New(Config{
		Mode:      "assign",
		Port:      port,
		SAEmail:   "test@project.iam.gserviceaccount.com",
		ProjectID: "test-project",
		HubURL:    hubServer.URL,
		AuthToken: "test-token",
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()
	time.Sleep(50 * time.Millisecond)

	// First request should hit the Hub
	_, body1 := metadataGet(t, port, "/computeMetadata/v1/instance/service-accounts/default/token")
	// Second request should be cached
	_, body2 := metadataGet(t, port, "/computeMetadata/v1/instance/service-accounts/default/token")

	var resp1, resp2 map[string]interface{}
	_ = json.Unmarshal([]byte(body1), &resp1)
	_ = json.Unmarshal([]byte(body2), &resp2)

	// Both should have the same token (cached)
	if resp1["access_token"] != resp2["access_token"] {
		t.Fatalf("expected cached token, got different tokens: %v vs %v", resp1["access_token"], resp2["access_token"])
	}

	// Only one Hub request should have been made
	if requestCount != 1 {
		t.Fatalf("expected 1 Hub request (caching), got %d", requestCount)
	}
}

func TestMetadataServer_AssignMode_RecursiveServiceAccount(t *testing.T) {
	hubServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token": "ya29.test-token",
			"expires_in":   3599,
			"token_type":   "Bearer",
		})
	}))
	defer hubServer.Close()

	port := freePort(t)
	srv := New(Config{
		Mode:      "assign",
		Port:      port,
		SAEmail:   "agent-worker@project.iam.gserviceaccount.com",
		ProjectID: "my-project",
		HubURL:    hubServer.URL,
		AuthToken: "test-auth-token",
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()
	time.Sleep(50 * time.Millisecond)

	// Recursive on a specific service account (the main bug scenario)
	resp, body := metadataGet(t, port, "/computeMetadata/v1/instance/service-accounts/default/?recursive=true")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if ct != "application/json" {
		t.Fatalf("expected application/json content-type, got %q", ct)
	}

	var info map[string]interface{}
	if err := json.Unmarshal([]byte(body), &info); err != nil {
		t.Fatalf("failed to parse recursive response: %v\nbody: %s", err, body)
	}
	if info["email"] != "agent-worker@project.iam.gserviceaccount.com" {
		t.Fatalf("unexpected email: %v", info["email"])
	}
	scopes, ok := info["scopes"].([]interface{})
	if !ok || len(scopes) == 0 {
		t.Fatalf("expected scopes array, got %v", info["scopes"])
	}
	if scopes[0] != "https://www.googleapis.com/auth/cloud-platform" {
		t.Fatalf("unexpected scope: %v", scopes[0])
	}
	aliases, ok := info["aliases"].([]interface{})
	if !ok || len(aliases) == 0 || aliases[0] != "default" {
		t.Fatalf("expected aliases [\"default\"], got %v", info["aliases"])
	}

	// Recursive on service-accounts listing
	resp, body = metadataGet(t, port, "/computeMetadata/v1/instance/service-accounts/?recursive=true")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	ct = resp.Header.Get("Content-Type")
	if ct != "application/json" {
		t.Fatalf("expected application/json for SA listing, got %q", ct)
	}

	var saList map[string]interface{}
	if err := json.Unmarshal([]byte(body), &saList); err != nil {
		t.Fatalf("failed to parse recursive SA listing: %v\nbody: %s", err, body)
	}
	if _, ok := saList["default"]; !ok {
		t.Fatal("expected 'default' key in recursive SA listing")
	}
	if _, ok := saList["agent-worker@project.iam.gserviceaccount.com"]; !ok {
		t.Fatal("expected email key in recursive SA listing")
	}

	// Non-recursive should still return text listing
	resp, body = metadataGet(t, port, "/computeMetadata/v1/instance/service-accounts/default/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if body != "email\nscopes\ntoken\nidentity\n" {
		t.Fatalf("expected text listing without recursive, got %q", body)
	}
}

func TestMetadataServer_BlockMode_RecursiveForbidden(t *testing.T) {
	port := freePort(t)
	srv := New(Config{
		Mode:      "block",
		Port:      port,
		ProjectID: "test-project",
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()
	time.Sleep(50 * time.Millisecond)

	// Recursive on service account in block mode should still be 403
	resp, _ := metadataGet(t, port, "/computeMetadata/v1/instance/service-accounts/default/?recursive=true")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for recursive in block mode, got %d", resp.StatusCode)
	}

	// Recursive on SA listing in block mode should still be 403
	resp, _ = metadataGet(t, port, "/computeMetadata/v1/instance/service-accounts/?recursive=true")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for recursive SA listing in block mode, got %d", resp.StatusCode)
	}
}

func TestMetadataServer_IdentityToken_RequiresAudience(t *testing.T) {
	port := freePort(t)
	srv := New(Config{
		Mode:      "assign",
		Port:      port,
		SAEmail:   "test@project.iam.gserviceaccount.com",
		ProjectID: "test-project",
		HubURL:    "http://localhost:9999",
		AuthToken: "test-token",
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()
	time.Sleep(50 * time.Millisecond)

	// Identity token without audience should fail
	resp, _ := metadataGet(t, port, "/computeMetadata/v1/instance/service-accounts/default/identity")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 without audience, got %d", resp.StatusCode)
	}
}

func TestMetadataServer_AssignMode_SingleflightToken(t *testing.T) {
	var requestCount int64
	hubServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&requestCount, 1)
		time.Sleep(200 * time.Millisecond) // simulate slow Hub
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token": "ya29.singleflight-token",
			"expires_in":   3599,
			"token_type":   "Bearer",
		})
	}))
	defer hubServer.Close()

	port := freePort(t)
	srv := New(Config{
		Mode:      "assign",
		Port:      port,
		SAEmail:   "test@project.iam.gserviceaccount.com",
		ProjectID: "test-project",
		HubURL:    hubServer.URL,
		AuthToken: "test-token",
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()
	time.Sleep(50 * time.Millisecond)

	// Launch 10 concurrent token requests
	const concurrency = 10
	var wg sync.WaitGroup
	results := make([]int, concurrency)
	for i := range concurrency {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			resp, _ := metadataGet(t, port, "/computeMetadata/v1/instance/service-accounts/default/token")
			results[idx] = resp.StatusCode
		}(i)
	}
	wg.Wait()

	for i, code := range results {
		if code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d", i, code)
		}
	}

	// Singleflight should collapse all concurrent requests into 1 Hub call
	count := atomic.LoadInt64(&requestCount)
	if count != 1 {
		t.Fatalf("expected 1 Hub request (singleflight), got %d", count)
	}
}

func TestMetadataServer_ProbeHealth(t *testing.T) {
	port := freePort(t)
	srv := New(Config{
		Mode:      "block",
		Port:      port,
		ProjectID: "test-project",
	})

	// Before start, probe should fail
	if srv.probeHealth() {
		t.Fatal("expected probeHealth to fail before server start")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()
	time.Sleep(50 * time.Millisecond)

	// After start, probe should succeed
	if !srv.probeHealth() {
		t.Fatal("expected probeHealth to succeed after server start")
	}
}

func TestMetadataServer_RestartHTTP(t *testing.T) {
	port := freePort(t)
	srv := New(Config{
		Mode:      "block",
		Port:      port,
		ProjectID: "test-project",
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()
	time.Sleep(50 * time.Millisecond)

	if !srv.probeHealth() {
		t.Fatal("expected healthy before shutdown")
	}

	// Forcibly close the HTTP server to simulate a crash
	_ = srv.srv.Close()
	time.Sleep(50 * time.Millisecond)

	if srv.probeHealth() {
		t.Fatal("expected probe to fail after server close")
	}

	// Restart should bring it back
	if err := srv.restartHTTP(ctx); err != nil {
		t.Fatalf("restartHTTP failed: %v", err)
	}

	if !srv.probeHealth() {
		t.Fatal("expected healthy after restart")
	}

	// Verify it actually serves requests
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
	if err != nil {
		t.Fatalf("GET after restart: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 after restart, got %d", resp.StatusCode)
	}
}

func TestMetadataServer_RestartLimit(t *testing.T) {
	port := freePort(t)
	srv := New(Config{
		Mode:      "block",
		Port:      port,
		ProjectID: "test-project",
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()
	time.Sleep(50 * time.Millisecond)

	// Exhaust restart attempts
	for i := 0; i < maxRestarts; i++ {
		_ = srv.srv.Close()
		time.Sleep(50 * time.Millisecond)
		if err := srv.restartHTTP(ctx); err != nil {
			t.Fatalf("restart %d should succeed: %v", i+1, err)
		}
	}

	// Next restart should fail (limit reached)
	_ = srv.srv.Close()
	time.Sleep(50 * time.Millisecond)
	err := srv.restartHTTP(ctx)
	if err == nil {
		t.Fatal("expected error after exceeding restart limit")
	}

	if !srv.isAbandoned() {
		t.Fatal("expected server to be marked abandoned")
	}
}

func TestMetadataServer_ShutdownEndpoint(t *testing.T) {
	port := freePort(t)
	srv := New(Config{
		Mode:      "block",
		Port:      port,
		ProjectID: "test-project",
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()
	time.Sleep(50 * time.Millisecond)

	// Verify server is running
	if !srv.probeHealth() {
		t.Fatal("expected server to be healthy")
	}

	// GET should be rejected
	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/_scion/shutdown", port), nil)
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for GET, got %d", resp.StatusCode)
	}

	// POST without Metadata-Flavor header should be rejected
	req, _ = http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/_scion/shutdown", port), nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 without Metadata-Flavor, got %d", resp.StatusCode)
	}

	// POST with Metadata-Flavor but no shutdown token should be rejected
	req, _ = http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/_scion/shutdown", port), nil)
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 without shutdown token, got %d", resp.StatusCode)
	}

	token, err := os.ReadFile(shutdownTokenPath(port))
	if err != nil {
		t.Fatal(err)
	}

	// POST with Metadata-Flavor and shutdown token should succeed and shut down
	req, _ = http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/_scion/shutdown", port), nil)
	req.Header.Set("Metadata-Flavor", "Google")
	req.Header.Set("X-Scion-Shutdown-Token", strings.TrimSpace(string(token)))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if string(body) != "shutting down" {
		t.Fatalf("expected 'shutting down', got %q", string(body))
	}

	// Wait for shutdown to complete
	time.Sleep(200 * time.Millisecond)

	// Server should no longer be reachable
	if srv.probeHealth() {
		t.Fatal("expected server to be unreachable after shutdown")
	}
}

func TestMetadataServer_StartReclaimsPort(t *testing.T) {
	port := freePort(t)

	// Start a first metadata server on the port
	srv1 := New(Config{
		Mode:      "block",
		Port:      port,
		ProjectID: "old-project",
	})
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()

	if err := srv1.Start(ctx1); err != nil {
		t.Fatal(err)
	}
	defer srv1.Stop()
	time.Sleep(50 * time.Millisecond)

	if !srv1.probeHealth() {
		t.Fatal("first server not healthy")
	}

	// Start a second server on the same port — should reclaim it
	srv2 := New(Config{
		Mode:      "block",
		Port:      port,
		ProjectID: "new-project",
	})
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()

	if err := srv2.Start(ctx2); err != nil {
		t.Fatalf("second Start() should succeed by reclaiming port: %v", err)
	}
	defer srv2.Stop()
	time.Sleep(50 * time.Millisecond)

	// The new server should be serving with the new config
	resp, body := metadataGet(t, port, "/computeMetadata/v1/project/project-id")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if body != "new-project" {
		t.Fatalf("expected new-project from replacement server, got %q", body)
	}
}

func TestMetadataServer_StartReclaimsPortViaShutdownEndpoint(t *testing.T) {
	port := freePort(t)

	srv1 := New(Config{
		Mode:      "block",
		Port:      port,
		ProjectID: "old-project",
	})
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()

	if err := srv1.Start(ctx1); err != nil {
		t.Fatal(err)
	}
	defer srv1.Stop()
	time.Sleep(50 * time.Millisecond)

	activeServerMu.Lock()
	activeServer = nil
	activeServerMu.Unlock()

	srv2 := New(Config{
		Mode:      "block",
		Port:      port,
		ProjectID: "new-project",
	})
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()

	if err := srv2.Start(ctx2); err != nil {
		t.Fatalf("second Start() should reclaim port via shutdown endpoint: %v", err)
	}
	defer srv2.Stop()
	time.Sleep(50 * time.Millisecond)

	resp, body := metadataGet(t, port, "/computeMetadata/v1/project/project-id")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if body != "new-project" {
		t.Fatalf("expected new-project from replacement server, got %q", body)
	}
}
