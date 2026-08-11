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
	"archive/zip"
	"bytes"
	"encoding/json"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// --- Teams Manifest handler tests ---

func TestHandleTeamsManifestDownload_AuthGate_Unauthenticated(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/integrations/teams/manifest", nil)
	rr := httptest.NewRecorder()
	srv.handleTeamsManifestDownload(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for unauthenticated request, got %d", rr.Code)
	}
}

func TestHandleTeamsManifestDownload_AuthGate_NonAdmin(t *testing.T) {
	srv := &Server{}
	member := NewAuthenticatedUser("u1", "member@example.com", "Member", "member", "cli")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/integrations/teams/manifest", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), member))
	rr := httptest.NewRecorder()
	srv.handleTeamsManifestDownload(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for non-admin, got %d", rr.Code)
	}
}

func TestHandleTeamsManifestDownload_MethodNotAllowed(t *testing.T) {
	srv := &Server{}
	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req := httptest.NewRequest(method, "/api/v1/admin/integrations/teams/manifest", nil)
		req = req.WithContext(contextWithIdentity(req.Context(), admin))
		rr := httptest.NewRecorder()
		srv.handleTeamsManifestDownload(rr, req)

		if rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: expected 405, got %d", method, rr.Code)
		}
	}
}

func TestHandleTeamsManifestDownload_MissingConfig(t *testing.T) {
	// No plugin manager → loadTeamsConfig returns error.
	srv := &Server{}
	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/integrations/teams/manifest", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleTeamsManifestDownload(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when config unavailable, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleTeamsManifestDownload_MissingAppID(t *testing.T) {
	// Plugin manager returns a config file with no app_id.
	tmpDir := t.TempDir()
	cfgFile := filepath.Join(tmpDir, "teams.yaml")
	if err := os.WriteFile(cfgFile, []byte("tenant_id: \"some-tenant\"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	mgr := newMockIntegrationManager()
	mgr.plugins["teams"] = map[string]string{"config_file": cfgFile}

	srv := &Server{}
	srv.pluginManager = mgr

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/integrations/teams/manifest", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleTeamsManifestDownload(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when app_id missing, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleTeamsManifestDownload_HappyPath(t *testing.T) {
	testAppID := "test-app-id-12345"

	tmpDir := t.TempDir()
	cfgFile := filepath.Join(tmpDir, "teams.yaml")
	cfgContent := "app_id: \"" + testAppID + "\"\ntenant_id: \"test-tenant\"\n"
	if err := os.WriteFile(cfgFile, []byte(cfgContent), 0644); err != nil {
		t.Fatal(err)
	}

	mgr := newMockIntegrationManager()
	mgr.plugins["teams"] = map[string]string{"config_file": cfgFile}

	srv := &Server{}
	srv.pluginManager = mgr

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/integrations/teams/manifest", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleTeamsManifestDownload(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// Verify Content-Type header.
	if ct := rr.Header().Get("Content-Type"); ct != "application/zip" {
		t.Errorf("expected Content-Type application/zip, got %q", ct)
	}

	// Verify Content-Disposition header.
	if cd := rr.Header().Get("Content-Disposition"); cd != `attachment; filename="teams-app.zip"` {
		t.Errorf("expected Content-Disposition with teams-app.zip, got %q", cd)
	}

	// Open as zip and verify contents.
	zipReader, err := zip.NewReader(bytes.NewReader(rr.Body.Bytes()), int64(rr.Body.Len()))
	if err != nil {
		t.Fatalf("failed to open response as zip: %v", err)
	}

	fileNames := make(map[string]bool)
	for _, f := range zipReader.File {
		fileNames[f.Name] = true
	}

	// Verify all three files are present.
	for _, want := range []string{"manifest.json", "color.png", "outline.png"} {
		if !fileNames[want] {
			t.Errorf("zip missing file %q; got %v", want, fileNames)
		}
	}

	// Verify manifest.json has the correct app_id.
	for _, f := range zipReader.File {
		if f.Name != "manifest.json" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("failed to open manifest.json from zip: %v", err)
		}
		defer func() { _ = rc.Close() }()

		var manifest teamsManifest
		if err := json.NewDecoder(rc).Decode(&manifest); err != nil {
			t.Fatalf("failed to decode manifest.json: %v", err)
		}
		if manifest.ID != testAppID {
			t.Errorf("manifest ID: got %q, want %q", manifest.ID, testAppID)
		}
		if manifest.Bots[0].BotID != testAppID {
			t.Errorf("manifest bot ID: got %q, want %q", manifest.Bots[0].BotID, testAppID)
		}
		if manifest.ManifestVersion != "1.16" {
			t.Errorf("manifest version: got %q, want %q", manifest.ManifestVersion, "1.16")
		}
	}

	// Verify color.png is a valid PNG.
	for _, f := range zipReader.File {
		if f.Name != "color.png" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("failed to open color.png from zip: %v", err)
		}
		defer func() { _ = rc.Close() }()

		img, err := png.Decode(rc)
		if err != nil {
			t.Fatalf("color.png is not a valid PNG: %v", err)
		}
		bounds := img.Bounds()
		if bounds.Dx() != 192 || bounds.Dy() != 192 {
			t.Errorf("color.png: expected 192x192, got %dx%d", bounds.Dx(), bounds.Dy())
		}
	}

	// Verify outline.png is a valid PNG.
	for _, f := range zipReader.File {
		if f.Name != "outline.png" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("failed to open outline.png from zip: %v", err)
		}
		defer func() { _ = rc.Close() }()

		img, err := png.Decode(rc)
		if err != nil {
			t.Fatalf("outline.png is not a valid PNG: %v", err)
		}
		bounds := img.Bounds()
		if bounds.Dx() != 32 || bounds.Dy() != 32 {
			t.Errorf("outline.png: expected 32x32, got %dx%d", bounds.Dx(), bounds.Dy())
		}
	}
}

// --- Unit tests for helpers ---

func TestBuildTeamsManifest(t *testing.T) {
	appID := "test-uuid-1234"
	m := buildTeamsManifest(appID)

	if m.ID != appID {
		t.Errorf("expected ID %q, got %q", appID, m.ID)
	}
	if m.ManifestVersion != "1.16" {
		t.Errorf("expected manifestVersion 1.16, got %q", m.ManifestVersion)
	}
	if len(m.Bots) != 1 {
		t.Fatalf("expected 1 bot, got %d", len(m.Bots))
	}
	if m.Bots[0].BotID != appID {
		t.Errorf("expected bot ID %q, got %q", appID, m.Bots[0].BotID)
	}
	if m.Icons.Color != "color.png" {
		t.Errorf("expected color icon color.png, got %q", m.Icons.Color)
	}
	if m.Icons.Outline != "outline.png" {
		t.Errorf("expected outline icon outline.png, got %q", m.Icons.Outline)
	}
}

func TestGeneratePlaceholderPNG(t *testing.T) {
	data, err := generatePlaceholderPNG(64, color.RGBA{R: 128, G: 128, B: 128, A: 255})
	if err != nil {
		t.Fatalf("generatePlaceholderPNG returned error: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("generatePlaceholderPNG returned empty data")
	}

	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("output is not valid PNG: %v", err)
	}
	bounds := img.Bounds()
	if bounds.Dx() != 64 || bounds.Dy() != 64 {
		t.Errorf("expected 64x64, got %dx%d", bounds.Dx(), bounds.Dy())
	}
}
