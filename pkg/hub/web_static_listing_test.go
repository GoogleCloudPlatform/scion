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
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The listing tests (ptone/scion#5346) check that the hub lists no
// directory and serves no path with a segment starting with a dot, in
// embedded and disk mode, while it still reads the Vite manifest itself.

const listingMarker = "listing-marker"

// listingAssets is a client build with a Shoelace icon directory, a dot
// file under /assets/ and a Vite manifest listing one fingerprinted chunk.
func listingAssets() fstest.MapFS {
	return fstest.MapFS{
		"assets/main.js":                    {Data: []byte("// entry")},
		"assets/view-QwErTyUi.js":           {Data: []byte("// chunk")},
		"assets/.hidden":                    {Data: []byte(listingMarker)},
		"shoelace/assets/icons/check.svg":   {Data: []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`)},
		"shoelace/assets/icons/index.html":  {Data: []byte(listingMarker)},
		"favicon.svg":                       {Data: []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`)},
		".vite/manifest.json":               {Data: []byte(`{"src/view.ts": {"file": "assets/view-QwErTyUi.js"}, "src/main.ts": {"file": "assets/main.js", "isEntry": true}, "x": {"file": "` + listingMarker + `"}}`)},
		".vite/other/index.html":            {Data: []byte(listingMarker)},
		"assets/sub/chunk-AbCd1234.js":      {Data: []byte("// nested chunk")},
		"assets/sub/deeper/chunk-EfGh56.js": {Data: []byte("// deeper chunk")},
	}
}

func writeListingDisk(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, f := range listingAssets() {
		p := filepath.Join(dir, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, f.Data, 0o644))
	}
	return dir
}

// listingServers returns, per asset mode, a web server without auth and
// one with dev auth (the SPA catch-all path), both over listingAssets.
func listingServers(t *testing.T) map[string][2]*WebServer {
	t.Helper()
	embedded := func(ws *WebServer) *WebServer {
		ws.assets = listingAssets()
		ws.assetsDisk = ""
		ws.hasAssets = ws.detectWebAssets()
		ws.fingerprintedAssets = fingerprintedAssetsOf(ws)
		require.True(t, ws.hasAssets)
		return ws
	}
	disk := newTestWebServer(t, WebServerConfig{AssetsDir: writeListingDisk(t)})
	require.True(t, disk.hasAssets)
	diskAuth := newDevAuthWebServer(t, func(cfg *WebServerConfig) { cfg.AssetsDir = writeListingDisk(t) })
	require.True(t, diskAuth.hasAssets)
	return map[string][2]*WebServer{
		"embedded": {embedded(newTestWebServer(t, WebServerConfig{})), embedded(newDevAuthWebServer(t))},
		"disk":     {disk, diskAuth},
	}
}

func getListing(t *testing.T, h http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("Accept-Encoding", "br, gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestStaticAsset_DirectoryListing404(t *testing.T) {
	for mode, servers := range listingServers(t) {
		t.Run(mode, func(t *testing.T) {
			for _, ws := range servers {
				for _, target := range []string{
					"/assets/",
					"/shoelace/assets/icons/",
					"/shoelace/",
					"/shoelace/assets/",
					"/assets/sub/",
					"/assets/sub/deeper/",
				} {
					rec := getListing(t, ws.Handler(), target)
					assert.Equal(t, http.StatusNotFound, rec.Code, target)
					body := rec.Body.String()
					assert.NotContains(t, body, "<a href", "%s: a listing", target)
					assert.NotContains(t, body, listingMarker, "%s: an index.html", target)
					assert.NotContains(t, body, "check.svg", target)
					assert.NotContains(t, body, "main.js", target)
					assert.Empty(t, rec.Header().Get("ETag"), target)
				}
				// Without the trailing slash, a directory is a 404 too,
				// not a redirect to the listing.
				for _, target := range []string{"/assets/sub", "/shoelace/assets/icons"} {
					rec := getListing(t, ws.Handler(), target)
					assert.Equal(t, http.StatusNotFound, rec.Code, target)
					assert.NotContains(t, rec.Body.String(), listingMarker, target)
				}
				// A directory's index.html is never served: the file
				// server redirects it to the directory, which is a 404.
				rec := getListing(t, ws.Handler(), "/shoelace/assets/icons/index.html")
				assert.NotEqual(t, http.StatusOK, rec.Code)
				assert.NotContains(t, rec.Body.String(), listingMarker)
				if loc := rec.Header().Get("Location"); loc != "" {
					// "./" is /shoelace/assets/icons/, checked above.
					assert.Equal(t, "./", loc)
				}
				// Files inside those directories are still served.
				for _, target := range []string{"/assets/main.js", "/assets/sub/chunk-AbCd1234.js", "/shoelace/assets/icons/check.svg"} {
					rec := getListing(t, ws.Handler(), target)
					assert.Equal(t, http.StatusOK, rec.Code, target)
				}
			}
		})
	}

	t.Run("disk symlink out of the root is not served", func(t *testing.T) {
		outside := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(outside, "outside.js"), []byte(listingMarker), 0o644))
		dir := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "assets"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "assets", "main.js"), validatorMainJS, 0o644))
		if err := os.Symlink(filepath.Join(outside, "outside.js"), filepath.Join(dir, "assets", "linked.js")); err != nil {
			t.Skipf("symlinks unsupported: %v", err)
		}
		require.NoError(t, os.Symlink(outside, filepath.Join(dir, "assets", "out")))
		disk := newTestWebServer(t, WebServerConfig{AssetsDir: dir})
		for _, p := range []string{"/assets/linked.js", "/assets/out/outside.js", "/assets/out/", "/assets/out"} {
			rec := getListing(t, disk.Handler(), p)
			assert.Equal(t, http.StatusNotFound, rec.Code, p)
			assert.NotContains(t, rec.Body.String(), listingMarker, p)
		}
	})
}

func TestStaticFile_DotPath404(t *testing.T) {
	for mode, servers := range listingServers(t) {
		t.Run(mode, func(t *testing.T) {
			ws := servers[0]
			// Root paths go through the SPA catch-all, which needs a
			// session (a dev sign-in); /assets/ and /shoelace/ are public.
			signedIn := devSignedIn(t, servers[1])
			check := func(h http.Handler, targets []string) {
				for _, target := range targets {
					rec := getListing(t, h, target)
					assert.Equal(t, http.StatusNotFound, rec.Code, target)
					assert.NotContains(t, rec.Body.String(), listingMarker, target)
					assert.NotContains(t, rec.Body.String(), "isEntry", target)
					assert.Empty(t, rec.Header().Get("Content-Encoding"), target)
				}
			}
			check(signedIn, []string{
				"/.vite/manifest.json",
				"/.vite/",
				"/.vite",
				"/.vite/other/index.html",
				"/.vite/missing.json",
				"/.env",
				"/%2evite/manifest.json",
			})
			for _, h := range []http.Handler{ws.Handler(), signedIn} {
				check(h, []string{
					"/assets/.hidden",
					"/assets/.missing",
					"/shoelace/assets/.icons/check.svg",
					"/assets/%2ehidden",
				})
			}
			// A deeper dot segment can be part of a client-side route: it
			// still gets the shell, and never a file.
			for _, target := range []string{"/projects/x/artifacts/.env", "/projects/x/files/.vite/manifest.json"} {
				rec := getListing(t, signedIn, target)
				assert.Equal(t, http.StatusOK, rec.Code, target)
				assert.Contains(t, rec.Header().Get("Content-Type"), "text/html", target)
				assert.Contains(t, rec.Body.String(), "scion-app", target)
				assert.NotContains(t, rec.Body.String(), listingMarker, target)
			}
			// Root files without a dot segment are still served.
			rec := getListing(t, signedIn, "/favicon.svg")
			assert.Equal(t, http.StatusOK, rec.Code)
			assert.Contains(t, rec.Header().Get("Content-Type"), "image/svg+xml")
		})
	}

	// The first-segment dot rule of the SPA catch-all leaves /.well-known/
	// alone: with the hub mounted, its own mux entry still routes it to
	// the hub.
	t.Run("well-known still routed to the hub", func(t *testing.T) {
		const marker = "well-known-marker"
		for mode, servers := range listingServers(t) {
			for _, ws := range servers {
				ws.MountHubAPI(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"marker": "` + marker + `", "path": "` + r.URL.Path + `"}`))
				}), func(context.Context) error { return nil })
				rec := getListing(t, ws.Handler(), "/.well-known/openid-configuration")
				assert.Equal(t, http.StatusOK, rec.Code, mode)
				assert.Contains(t, rec.Body.String(), marker, mode)
				assert.Contains(t, rec.Body.String(), "/.well-known/openid-configuration", mode)
				// Other first-segment dot paths are still a 404 (the
				// catch-all needs a session, so only signed in with dev auth).
				if ws.config.DevAuthToken != "" {
					rec = getListing(t, devSignedIn(t, ws), "/.vite/manifest.json")
					assert.Equal(t, http.StatusNotFound, rec.Code, mode)
				}
			}
		}
	})
}

func TestFingerprintedAssets_StillLoadedFromManifest(t *testing.T) {
	for mode, servers := range listingServers(t) {
		t.Run(mode, func(t *testing.T) {
			for i, ws := range servers {
				// The hub reads the manifest from the asset source even
				// though the manifest is no longer served.
				fp := fingerprintedAssetsOf(ws)
				assert.Equal(t, map[string]bool{"/assets/view-QwErTyUi.js": true}, fp)
				assert.True(t, ws.fingerprintedAssets["/assets/view-QwErTyUi.js"])

				rec := getListing(t, ws.Handler(), "/assets/view-QwErTyUi.js")
				require.Equal(t, http.StatusOK, rec.Code)
				assert.Equal(t, ws.staticCacheControl("/assets/view-QwErTyUi.js"), rec.Header().Get("Cache-Control"))
				assert.NotEqual(t, "no-cache", rec.Header().Get("Cache-Control"), "a manifest-listed chunk gets the long lifetime")

				if i == 1 { // the SPA catch-all needs a session (a dev sign-in)
					rec = getListing(t, devSignedIn(t, ws), "/.vite/manifest.json")
					assert.Equal(t, http.StatusNotFound, rec.Code)
				}
			}
		})
	}
}

// TestHasDotSegment pins the segment rule of hasDotSegment, including
// relative and empty-segment forms, and that it does not allocate.
func TestHasDotSegment(t *testing.T) {
	for p, want := range map[string]bool{
		"":                     false,
		"/":                    false,
		"/assets/main.js":      false,
		"/assets/a.b/c.js":     false,
		"/assets/x.":           false,
		"/a..b":                false,
		"/.vite/manifest.json": true,
		"/assets/.hidden":      true,
		"/assets/sub/.d/x.js":  true,
		"/assets//.x":          true,
		"/.":                   true,
		"/..":                  true,
		".vite":                true,
		"assets/.x":            true,
	} {
		assert.Equal(t, want, hasDotSegment(p), p)
	}
	allocs := testing.AllocsPerRun(100, func() { _ = hasDotSegment("/shoelace/assets/icons/check-circle.svg") })
	assert.Zero(t, allocs)
}
