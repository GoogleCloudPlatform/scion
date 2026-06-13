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

//go:build !no_sqlite

package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	_ "github.com/GoogleCloudPlatform/scion/pkg/store/sqlite" // register the sqlite driver for the hub test binary
)

// testWorkstationServer creates a test server with workstation mode enabled.
func testWorkstationServer(t *testing.T) (*Server, store.Store) {
	t.Helper()
	s, err := newTestStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}

	cfg := DefaultServerConfig()
	cfg.DevAuthToken = testDevToken
	cfg.Workstation = true
	srv, err := New(cfg, s)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	srv.SetHubID("test-hub-id")
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	return srv, s
}

// doWorkstationRequest performs an authenticated request from a loopback address.
func doWorkstationRequest(t *testing.T, srv *Server, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("failed to marshal body: %v", err)
		}
	}

	req := httptest.NewRequest(method, path, bytes.NewReader(bodyBytes))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	req.RemoteAddr = "127.0.0.1:1234"

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// ============================================================================
// requireWorkstation middleware
// ============================================================================

func TestRequireWorkstation_Disabled(t *testing.T) {
	srv, _ := testServer(t)
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/system/status", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 when workstation disabled, got %d", rec.Code)
	}
}

func TestRequireWorkstation_Enabled(t *testing.T) {
	srv, _ := testWorkstationServer(t)
	rec := doWorkstationRequest(t, srv, http.MethodGet, "/api/v1/system/status", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 when workstation enabled, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ============================================================================
// assertLoopback
// ============================================================================

func TestAssertLoopback(t *testing.T) {
	tests := []struct {
		remoteAddr string
		wantErr    bool
	}{
		{"127.0.0.1:1234", false},
		{"[::1]:1234", false},
		{"192.168.1.1:1234", true},
		{"10.0.0.1:5555", true},
		{"192.0.2.1:1234", true},
	}

	for _, tt := range tests {
		t.Run(tt.remoteAddr, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = tt.remoteAddr
			err := assertLoopback(req)
			if tt.wantErr && err == nil {
				t.Errorf("expected error for %s", tt.remoteAddr)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error for %s: %v", tt.remoteAddr, err)
			}
		})
	}
}

func TestAssertLoopback_RejectViaHandler(t *testing.T) {
	srv, _ := testWorkstationServer(t)

	// doRequest uses httptest default RemoteAddr (192.0.2.1:1234) — non-loopback
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/system/status", nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 for non-loopback request, got %d", rec.Code)
	}
}

// ============================================================================
// POST /system/init
// ============================================================================

func TestSystemInit_ValidHarnesses(t *testing.T) {
	srv, _ := testWorkstationServer(t)
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	restore := config.OverrideRuntimeDetection(
		func(string) (string, error) { return "/usr/bin/docker", nil },
		func(string, []string) error { return nil },
	)
	defer restore()

	rec := doWorkstationRequest(t, srv, http.MethodPost, "/api/v1/system/init", map[string]interface{}{
		"harnesses": []string{"claude"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp systemInitResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if !resp.OK || !resp.Initialized {
		t.Errorf("expected ok=true initialized=true, got %+v", resp)
	}

	settingsPath := filepath.Join(tmpHome, ".scion", "settings.yaml")
	if _, err := os.Stat(settingsPath); os.IsNotExist(err) {
		t.Error("expected settings.yaml to be created")
	}
}

func TestSystemInit_UnknownHarness(t *testing.T) {
	srv, _ := testWorkstationServer(t)
	rec := doWorkstationRequest(t, srv, http.MethodPost, "/api/v1/system/init", map[string]interface{}{
		"harnesses": []string{"bogus"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for unknown harness, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSystemInit_EmptyHarnesses(t *testing.T) {
	srv, _ := testWorkstationServer(t)
	rec := doWorkstationRequest(t, srv, http.MethodPost, "/api/v1/system/init", map[string]interface{}{
		"harnesses": []string{},
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for empty harnesses, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ============================================================================
// PUT /system/identity
// ============================================================================

func TestSystemIdentity_PUT(t *testing.T) {
	srv, _ := testWorkstationServer(t)
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	// Seed a minimal settings.yaml so UpdateSetting has a file to update
	scionDir := filepath.Join(tmpHome, ".scion")
	if err := os.MkdirAll(scionDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scionDir, "settings.yaml"), []byte("version: \"1\"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	rec := doWorkstationRequest(t, srv, http.MethodPut, "/api/v1/system/identity", map[string]interface{}{
		"displayName": "Test User",
		"email":       "test@example.com",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp systemIdentityResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if resp.DisplayName != "Test User" {
		t.Errorf("expected displayName 'Test User', got %q", resp.DisplayName)
	}
	if resp.Email != "test@example.com" {
		t.Errorf("expected email 'test@example.com', got %q", resp.Email)
	}
}

func TestSystemIdentity_MethodNotAllowed(t *testing.T) {
	srv, _ := testWorkstationServer(t)
	rec := doWorkstationRequest(t, srv, http.MethodGet, "/api/v1/system/identity", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", rec.Code)
	}
}

// ============================================================================
// POST /system/fs/validate-path
// ============================================================================

func TestFSValidatePath_ManagedOverlap(t *testing.T) {
	srv, _ := testWorkstationServer(t)
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	managedDir := filepath.Join(tmpHome, ".scion", "projects", "something")
	if err := os.MkdirAll(managedDir, 0755); err != nil {
		t.Fatal(err)
	}

	rec := doWorkstationRequest(t, srv, http.MethodPost, "/api/v1/system/fs/validate-path", map[string]interface{}{
		"path": managedDir,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp fsValidatePathResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if !resp.IsManaged {
		t.Error("expected IsManaged=true for path inside managed directory")
	}
	if resp.Error == "" {
		t.Error("expected non-empty error message for managed path")
	}
}

func TestFSValidatePath_NormalPath(t *testing.T) {
	srv, _ := testWorkstationServer(t)
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	projectDir := filepath.Join(tmpHome, "my-project")
	if err := os.MkdirAll(filepath.Join(projectDir, ".git"), 0755); err != nil {
		t.Fatal(err)
	}

	rec := doWorkstationRequest(t, srv, http.MethodPost, "/api/v1/system/fs/validate-path", map[string]interface{}{
		"path": projectDir,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp fsValidatePathResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if resp.IsManaged {
		t.Error("expected IsManaged=false for normal path")
	}
	if !resp.Exists {
		t.Error("expected Exists=true")
	}
	if !resp.IsDir {
		t.Error("expected IsDir=true")
	}
	if !resp.IsGit {
		t.Error("expected IsGit=true for dir with .git")
	}
	if resp.Error != "" {
		t.Errorf("expected no error, got %q", resp.Error)
	}
}

// ============================================================================
// GET /system/fs/list
// ============================================================================

func TestFSList_HomeDir(t *testing.T) {
	srv, _ := testWorkstationServer(t)
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	if err := os.Mkdir(filepath.Join(tmpHome, "visible-dir"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(tmpHome, ".hidden-dir"), 0755); err != nil {
		t.Fatal(err)
	}

	rec := doWorkstationRequest(t, srv, http.MethodGet, "/api/v1/system/fs/list?path="+tmpHome, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp fsListResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode error: %v", err)
	}

	found := false
	for _, e := range resp.Entries {
		if e.Name == "visible-dir" {
			found = true
		}
		if strings.HasPrefix(e.Name, ".") {
			t.Errorf("hidden entry %q should be filtered", e.Name)
		}
	}
	if !found {
		t.Error("expected 'visible-dir' in entries")
	}
}

func TestFSList_OutsideHome(t *testing.T) {
	srv, _ := testWorkstationServer(t)
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	rec := doWorkstationRequest(t, srv, http.MethodGet, "/api/v1/system/fs/list?path=/tmp", nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 for path outside home, got %d: %s", rec.Code, rec.Body.String())
	}

	// Sibling-prefix bypass: a path like /home/alice-backup must not pass
	// when home is /home/alice.
	siblingDir := tmpHome + "-backup"
	if err := os.MkdirAll(siblingDir, 0755); err != nil {
		t.Fatal(err)
	}
	rec = doWorkstationRequest(t, srv, http.MethodGet, "/api/v1/system/fs/list?path="+siblingDir, nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 for sibling-prefix path, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestFSList_DefaultsToHome(t *testing.T) {
	srv, _ := testWorkstationServer(t)
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	if err := os.Mkdir(filepath.Join(tmpHome, "subdir"), 0755); err != nil {
		t.Fatal(err)
	}

	rec := doWorkstationRequest(t, srv, http.MethodGet, "/api/v1/system/fs/list", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp fsListResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if resp.Path != tmpHome {
		t.Errorf("expected path=%q, got %q", tmpHome, resp.Path)
	}

	found := false
	for _, e := range resp.Entries {
		if e.Name == "subdir" {
			found = true
		}
	}
	if !found {
		t.Error("expected 'subdir' in entries")
	}
}
