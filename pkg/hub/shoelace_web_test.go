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
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestShoelaceIconServing_Disk(t *testing.T) {
	tmpDir := t.TempDir()

	// Create shoelace icon file mimicking dist/client structure
	iconsDir := filepath.Join(tmpDir, "shoelace", "assets", "icons")
	require.NoError(t, os.MkdirAll(iconsDir, 0755))
	svgContent := `<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16"><path d="M1 1h14v14H1z"/></svg>`
	require.NoError(t, os.WriteFile(filepath.Join(iconsDir, "list.svg"), []byte(svgContent), 0644))

	ws := newTestWebServer(t, WebServerConfig{
		AssetsDir: tmpDir,
	})

	req := httptest.NewRequest("GET", "/shoelace/assets/icons/list.svg", nil)
	rec := httptest.NewRecorder()
	ws.Handler().ServeHTTP(rec, req)

	resp := rec.Result()
	body, _ := io.ReadAll(resp.Body)

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, svgContent, string(body))
	assert.Contains(t, resp.Header.Get("Content-Type"), "image/svg+xml",
		"shoelace icons should be served with SVG content type")
}

func TestShoelaceIconServing_WithDevAuth(t *testing.T) {
	tmpDir := t.TempDir()

	iconsDir := filepath.Join(tmpDir, "shoelace", "assets", "icons")
	require.NoError(t, os.MkdirAll(iconsDir, 0755))
	svgContent := `<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16"><path d="M1 1h14v14H1z"/></svg>`
	require.NoError(t, os.WriteFile(filepath.Join(iconsDir, "list.svg"), []byte(svgContent), 0644))

	// Test with dev-auth enabled (simulates typical dev setup)
	ws := newDevAuthWebServer(t, func(cfg *WebServerConfig) {
		cfg.AssetsDir = tmpDir
	})

	req := httptest.NewRequest("GET", "/shoelace/assets/icons/list.svg", nil)
	rec := httptest.NewRecorder()
	ws.Handler().ServeHTTP(rec, req)

	resp := rec.Result()
	body, _ := io.ReadAll(resp.Body)

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, svgContent, string(body))
	assert.Contains(t, resp.Header.Get("Content-Type"), "image/svg+xml")
}

func TestShoelaceIconServing_NotSPAFallback(t *testing.T) {
	// Verify that /shoelace/ requests are handled by the static handler,
	// NOT the SPA catch-all. Even for missing files, the response should be
	// a 404 from the file server, not the SPA HTML shell.
	tmpDir := t.TempDir()

	// Create the shoelace directory but not the requested icon file
	iconsDir := filepath.Join(tmpDir, "shoelace", "assets", "icons")
	require.NoError(t, os.MkdirAll(iconsDir, 0755))

	ws := newDevAuthWebServer(t, func(cfg *WebServerConfig) {
		cfg.AssetsDir = tmpDir
	})

	req := httptest.NewRequest("GET", "/shoelace/assets/icons/nonexistent.svg", nil)
	rec := httptest.NewRecorder()
	ws.Handler().ServeHTTP(rec, req)

	resp := rec.Result()
	body, _ := io.ReadAll(resp.Body)

	assert.Equal(t, http.StatusNotFound, resp.StatusCode,
		"missing shoelace icon should return 404, not SPA shell")
	assert.NotContains(t, string(body), "scion-app",
		"response should not contain the SPA shell HTML")
	assert.NotContains(t, string(body), "<!DOCTYPE html",
		"response should not contain HTML")
}

func TestShoelaceRoute_IsPublic(t *testing.T) {
	// /shoelace/ routes should be accessible without authentication
	assert.True(t, isPublicRoute("/shoelace/assets/icons/list.svg"),
		"shoelace icon paths should be public")
	assert.True(t, isPublicRoute("/shoelace/"),
		"shoelace root should be public")
}

func TestShoelaceIconServing_CacheHeaders(t *testing.T) {
	tmpDir := t.TempDir()
	iconsDir := filepath.Join(tmpDir, "shoelace", "assets", "icons")
	require.NoError(t, os.MkdirAll(iconsDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(iconsDir, "list.svg"), []byte("<svg/>"), 0644))

	ws := newTestWebServer(t, WebServerConfig{AssetsDir: tmpDir})

	req := httptest.NewRequest("GET", "/shoelace/assets/icons/list.svg", nil)
	rec := httptest.NewRecorder()
	ws.Handler().ServeHTTP(rec, req)

	resp := rec.Result()
	cc := resp.Header.Get("Cache-Control")
	assert.Equal(t, "no-cache", cc,
		"non-hashed shoelace icons should get no-cache (they don't have content hashes)")
	// Verify it's not an HTML response (which would indicate SPA fallback)
	ct := resp.Header.Get("Content-Type")
	assert.True(t, strings.Contains(ct, "svg") || strings.Contains(ct, "xml"),
		"Content-Type should be SVG, got %q", ct)
}
