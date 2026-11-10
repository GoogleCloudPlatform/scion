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
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The cache policy tests (ptone/scion#5348) check the Cache-Control of each
// class of static asset on every representation the hub sends: identity,
// the br and gzip variants, the 304 of a conditional request, and HEAD; in
// embedded and disk mode.

// cachePolicyAssets is a client build in which every file has br and gzip
// siblings. The manifest lists view-QwErTyUi.js (not a hex name) and
// hexy-abcdef12.js (a hex name, so the manifest decides, not the name);
// chunk-abc12345.js is a hex name the manifest does not list.
func cachePolicyAssets() fstest.MapFS {
	files := fstest.MapFS{
		".vite/manifest.json": {Data: []byte(`{
  "src/main.ts": {"file": "assets/main.js", "isEntry": true},
  "src/view.ts": {"file": "assets/view-QwErTyUi.js", "css": ["assets/view-QwErTyUi.css"]},
  "src/hexy.ts": {"file": "assets/hexy-abcdef12.js"}
}`)},
	}
	for _, name := range []string{
		"assets/main.js",
		"assets/view-QwErTyUi.js",
		"assets/view-QwErTyUi.css",
		"assets/hexy-abcdef12.js",
		"assets/chunk-abc12345.js",
		"assets/settings-QwErTyUi.js",
		"shoelace/assets/icons/check.svg",
		"favicon.svg",
	} {
		files[name] = &fstest.MapFile{Data: []byte("raw:" + name)}
		files[name+".br"] = &fstest.MapFile{Data: []byte("br:" + name)}
		files[name+".gz"] = &fstest.MapFile{Data: []byte("gz:" + name)}
	}
	return files
}

// writeCachePolicyDisk writes cachePolicyAssets to a directory with one
// shared mtime, so every sibling counts as current (DiskIndex ignores a
// sibling older than its source).
func writeCachePolicyDisk(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mtime := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for name, f := range cachePolicyAssets() {
		p := filepath.Join(dir, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, f.Data, 0o644))
		require.NoError(t, os.Chtimes(p, mtime, mtime))
	}
	return dir
}

// cachePolicyServers returns, per asset mode, a web server without auth
// (for /assets/ and /shoelace/, which are public) and one with dev auth
// (for root files, which go through the SPA catch-all).
func cachePolicyServers(t *testing.T) map[string][2]*WebServer {
	t.Helper()
	embedded := func(ws *WebServer) *WebServer {
		ws.assets = cachePolicyAssets()
		ws.assetsDisk = ""
		ws.hasAssets = ws.detectWebAssets()
		ws.fingerprintedAssets = fingerprintedAssetsOf(ws)
		require.True(t, ws.hasAssets)
		return ws
	}
	disk := newTestWebServer(t, WebServerConfig{AssetsDir: writeCachePolicyDisk(t)})
	require.True(t, disk.hasAssets)
	diskAuth := newDevAuthWebServer(t, func(cfg *WebServerConfig) { cfg.AssetsDir = writeCachePolicyDisk(t) })
	require.True(t, diskAuth.hasAssets)
	return map[string][2]*WebServer{
		"embedded": {embedded(newTestWebServer(t, WebServerConfig{})), embedded(newDevAuthWebServer(t))},
		"disk":     {disk, diskAuth},
	}
}

// assertCacheControlEverywhere requests target as identity, br and gzip,
// then conditionally with each representation's ETag (a 304) and as HEAD,
// and asserts every response carries want.
func assertCacheControlEverywhere(t *testing.T, h http.Handler, target, want string) {
	t.Helper()
	do := func(method, acceptEncoding, ifNoneMatch string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, nil)
		if acceptEncoding != "" {
			req.Header.Set("Accept-Encoding", acceptEncoding)
		}
		if ifNoneMatch != "" {
			req.Header.Set("If-None-Match", ifNoneMatch)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	for _, tc := range []struct{ accept, encoding string }{
		{"", ""},
		{"br", "br"},
		{"gzip", "gzip"},
	} {
		label := target + " Accept-Encoding=" + tc.accept
		rec := do(http.MethodGet, tc.accept, "")
		require.Equal(t, http.StatusOK, rec.Code, label)
		assert.Equal(t, tc.encoding, rec.Header().Get("Content-Encoding"), label)
		assert.Equal(t, want, rec.Header().Get("Cache-Control"), label)
		etag := rec.Header().Get("ETag")
		require.NotEmpty(t, etag, label)

		rec = do(http.MethodGet, tc.accept, etag)
		assert.Equal(t, http.StatusNotModified, rec.Code, label+" (conditional)")
		assert.Empty(t, rec.Body.Bytes(), label+" (conditional)")
		assert.Equal(t, want, rec.Header().Get("Cache-Control"), label+" (conditional)")
		assert.Equal(t, etag, rec.Header().Get("ETag"), label+" (conditional)")

		rec = do(http.MethodHead, tc.accept, "")
		assert.Equal(t, http.StatusOK, rec.Code, label+" (HEAD)")
		assert.Equal(t, want, rec.Header().Get("Cache-Control"), label+" (HEAD)")
	}
}

func TestCacheControl_ManifestAssetImmutable(t *testing.T) {
	for mode, servers := range cachePolicyServers(t) {
		t.Run(mode, func(t *testing.T) {
			ws := servers[0]
			assert.Equal(t, map[string]bool{
				"/assets/view-QwErTyUi.js":  true,
				"/assets/view-QwErTyUi.css": true,
				"/assets/hexy-abcdef12.js":  true,
			}, ws.fingerprintedAssets)
			for target := range ws.fingerprintedAssets {
				assertCacheControlEverywhere(t, ws.Handler(), target, "public, max-age=31536000, immutable")
			}
		})
	}
}

func TestCacheControl_HexHeuristicOneDay(t *testing.T) {
	for mode, servers := range cachePolicyServers(t) {
		t.Run(mode, func(t *testing.T) {
			ws := servers[0]
			// A hex name the manifest does not list only looks
			// content-addressed: one day, not immutable.
			assertCacheControlEverywhere(t, ws.Handler(), "/assets/chunk-abc12345.js", "public, max-age=86400")
		})
	}

	t.Run("no manifest", func(t *testing.T) {
		files := cachePolicyAssets()
		delete(files, ".vite/manifest.json")
		ws := newTestWebServer(t, WebServerConfig{})
		ws.assets = files
		ws.assetsDisk = ""
		ws.hasAssets = ws.detectWebAssets()
		ws.fingerprintedAssets = fingerprintedAssetsOf(ws)
		require.True(t, ws.hasAssets)
		require.Empty(t, ws.fingerprintedAssets)
		// Without a manifest nothing is immutable; hex names keep one day.
		assertCacheControlEverywhere(t, ws.Handler(), "/assets/chunk-abc12345.js", "public, max-age=86400")
		assertCacheControlEverywhere(t, ws.Handler(), "/assets/hexy-abcdef12.js", "public, max-age=86400")
		assertCacheControlEverywhere(t, ws.Handler(), "/assets/view-QwErTyUi.js", "no-cache")
	})
}

func TestCacheControl_EntryNoCache(t *testing.T) {
	for mode, servers := range cachePolicyServers(t) {
		t.Run(mode, func(t *testing.T) {
			ws, wsAuth := servers[0], servers[1]
			for _, target := range []string{
				"/assets/main.js",                  // the unhashed entry
				"/assets/settings-QwErTyUi.js",     // looks fingerprinted, not in the manifest
				"/shoelace/assets/icons/check.svg", // icon
			} {
				assertCacheControlEverywhere(t, ws.Handler(), target, "no-cache")
			}
			// Root files go through the SPA catch-all (dev auth).
			assertCacheControlEverywhere(t, devSignedIn(t, wsAuth), "/favicon.svg", "no-cache")
		})
	}
}
