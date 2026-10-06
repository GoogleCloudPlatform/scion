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
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// appIconContentTypes lists every app icon path with the Content-Type it
// must be served with.
var appIconContentTypes = map[string]string{
	"/favicon.ico":           "image/x-icon",
	"/favicon.svg":           "image/svg+xml",
	"/apple-touch-icon.png":  "image/png",
	"/icon-192.png":          "image/png",
	"/icon-512.png":          "image/png",
	"/icon-maskable-512.png": "image/png",
	"/manifest.webmanifest":  "application/manifest+json",
}

// newAppIconWebServer returns a web server (no dev auth, so no session)
// serving a temp assets dir holding every app icon path plus an unrelated
// root-level file.
func newAppIconWebServer(t *testing.T, maintenance bool) *WebServer {
	t.Helper()
	dir := t.TempDir()
	for p := range appIconContentTypes {
		require.NoError(t, os.WriteFile(filepath.Join(dir, strings.TrimPrefix(p, "/")), []byte("data"), 0o644))
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "robots.txt"), []byte("data"), 0o644))
	ws := newTestWebServer(t, WebServerConfig{AssetsDir: dir})
	if maintenance {
		ws.SetMaintenanceState(NewMaintenanceState(true, ""))
	}
	return ws
}

func TestAppIcons_ServedWithoutSession(t *testing.T) {
	for _, maintenance := range []bool{false, true} {
		t.Run(fmt.Sprintf("maintenance=%v", maintenance), func(t *testing.T) {
			ws := newAppIconWebServer(t, maintenance)
			handler := ws.Handler()
			for p, wantCT := range appIconContentTypes {
				req := httptest.NewRequest(http.MethodGet, p, nil)
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)

				assert.Equal(t, http.StatusOK, rec.Code, "GET %s", p)
				assert.Equal(t, wantCT, strings.Split(rec.Header().Get("Content-Type"), ";")[0],
					"Content-Type of %s", p)
			}
		})
	}
}

func TestAppIcons_AdminModeStillBlocksOtherPaths(t *testing.T) {
	// robots.txt is a real root-level file that skips session auth: the
	// admin-mode allowlist must stay exact rather than letting every
	// root-level static file through.
	ws := newAppIconWebServer(t, true)
	req := httptest.NewRequest(http.MethodGet, "/robots.txt", nil)
	rec := httptest.NewRecorder()
	ws.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, "GET /robots.txt in admin mode")

	// Without a session the full chain redirects other paths to /login
	// before admin mode runs, so check the middleware directly.
	mw := newTestWebServerWithMaintenance(true, "")
	mw.mux.HandleFunc("/", passthrough)
	handler := mw.adminModeWebMiddleware(mw.mux)
	for _, p := range []string{"/robots.txt", "/dashboard", "/manifest.webmanifest/x", "/icon-1024.png"} {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code, "GET %s in admin mode", p)
	}
}

// TestAppIcons_AllowlistMatchesPublicFiles keeps appIconPaths, the files
// in web/public/ and the manifest icon list in sync.
func TestAppIcons_AllowlistMatchesPublicFiles(t *testing.T) {
	require.Len(t, appIconPaths, len(appIconContentTypes))
	for p := range appIconContentTypes {
		assert.True(t, isAppIconPath(p), "%s missing from appIconPaths", p)
		_, err := os.Stat(filepath.Join("../../web/public", strings.TrimPrefix(p, "/")))
		assert.NoError(t, err, "web/public%s", p)
	}

	raw, err := os.ReadFile("../../web/public/manifest.webmanifest")
	require.NoError(t, err)
	var manifest struct {
		Display string `json:"display"`
		Icons   []struct {
			Src     string `json:"src"`
			Purpose string `json:"purpose"`
		} `json:"icons"`
	}
	require.NoError(t, json.Unmarshal(raw, &manifest))
	assert.Equal(t, "standalone", manifest.Display)
	var maskable bool
	for _, icon := range manifest.Icons {
		assert.True(t, isAppIconPath(icon.Src), "manifest icon %s not in appIconPaths", icon.Src)
		maskable = maskable || icon.Purpose == "maskable"
	}
	assert.True(t, maskable, "manifest needs a maskable icon")
}

// appIconHrefRE matches href values in the app-icons head block.
var appIconHrefRE = regexp.MustCompile(`href="([^"]+)"`)

// appIconsBlock returns the tags between the app-icons markers, with the
// start comment (whose wording differs per side) removed and whitespace
// collapsed.
func appIconsBlock(t *testing.T, html string) string {
	t.Helper()
	block := extractBetween(t, html, "<!-- app-icons:start", "<!-- app-icons:end -->")
	_, block, ok := strings.Cut(block, "-->")
	require.True(t, ok, "app-icons start comment not closed")
	return strings.Join(strings.Fields(block), " ")
}

// TestSPAShellAppIconTags keeps the icon, manifest and theme-color tags in
// the production shell identical to web/index.html, and every linked file
// on the admin-mode allowlist.
func TestSPAShellAppIconTags(t *testing.T) {
	indexHTML, err := os.ReadFile("../../web/index.html")
	require.NoError(t, err)

	shellBlock := appIconsBlock(t, spaShellTemplate)
	assert.Equal(t, shellBlock, appIconsBlock(t, string(indexHTML)),
		"app icon tags must match between pkg/hub/web.go and web/index.html")

	for _, tag := range []string{`rel="icon"`, `rel="apple-touch-icon"`, `rel="manifest"`, `name="theme-color"`} {
		assert.Contains(t, shellBlock, tag)
	}
	for _, m := range appIconHrefRE.FindAllStringSubmatch(shellBlock, -1) {
		assert.True(t, isAppIconPath(m[1]), "%s not in appIconPaths", m[1])
	}

	assert.Contains(t, renderSPAShell(t), `<link rel="manifest" href="/manifest.webmanifest" />`)
}
