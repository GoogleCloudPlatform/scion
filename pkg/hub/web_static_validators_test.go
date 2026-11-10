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
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The static validator tests (ptone/scion#5302) serve an in-memory client
// build with precompressed siblings. Go has no brotli decoder in this
// module, so the brotli body is checked byte for byte against the stored
// .br variant (the build test, web/scripts/precompress.test.mjs, proves the
// real .br files decode to their sources); gzip is decoded for real.

var (
	validatorMainJS  = []byte(strings.Repeat("console.log('scion main');\n", 80))
	validatorMainBr  = []byte("stored-brotli-variant-of-main.js")
	validatorFavicon = []byte(`<svg xmlns="http://www.w3.org/2000/svg"><path d="M0 0h16v16H0z"/></svg>`)
	validatorFavBr   = []byte("stored-brotli-variant-of-favicon.svg")
)

func gzipBytes(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, err := zw.Write(b)
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

func validatorAssets(t *testing.T) fstest.MapFS {
	t.Helper()
	return fstest.MapFS{
		"assets/main.js":           {Data: validatorMainJS},
		"assets/main.js.br":        {Data: validatorMainBr},
		"assets/main.js.gz":        {Data: gzipBytes(t, validatorMainJS)},
		"assets/chunk-abc12345.js": {Data: []byte("// hashed chunk")},
		"assets/chunk-abc12345.js.br": {
			Data: []byte("stored-brotli-variant-of-chunk"),
		},
		"favicon.svg":         {Data: validatorFavicon},
		"favicon.svg.br":      {Data: validatorFavBr},
		"favicon.svg.gz":      {Data: gzipBytes(t, validatorFavicon)},
		".vite/manifest.json": {Data: []byte(`{"outside-marker": {"file": "assets/main.js", "isEntry": true}}`)},
	}
}

// newValidatorWebServer returns a web server over the in-memory build,
// set up the way the other static tests swap ws.assets after construction.
func newValidatorWebServer(t *testing.T) *WebServer {
	t.Helper()
	ws := newTestWebServer(t, WebServerConfig{})
	ws.assets = validatorAssets(t)
	ws.assetsDisk = ""
	ws.hasAssets = ws.detectWebAssets()
	ws.fingerprintedAssets = fingerprintedAssetsOf(ws)
	require.True(t, ws.hasAssets)
	return ws
}

func doStatic(t *testing.T, ws *WebServer, method, target string, header map[string]string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	ws.Handler().ServeHTTP(rec, req)
	return rec.Result()
}

func readBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return b
}

func TestStaticAsset_PrecompressedBrotli(t *testing.T) {
	ws := newValidatorWebServer(t)

	resp := doStatic(t, ws, http.MethodGet, "/assets/main.js", map[string]string{"Accept-Encoding": "gzip, deflate, br, zstd"})
	body := readBody(t, resp)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "br", resp.Header.Get("Content-Encoding"))
	assert.Equal(t, validatorMainBr, body, "the stored .br variant, byte for byte")
	assert.Equal(t, "no-cache", resp.Header.Get("Cache-Control"), "Cache-Control unchanged for the unhashed entry")
	assert.Equal(t, "Accept-Encoding", resp.Header.Get("Vary"))
	assert.Contains(t, resp.Header.Get("Content-Type"), "javascript", "Content-Type from main.js, not main.js.br")
	assert.NotEmpty(t, resp.Header.Get("ETag"))

	t.Run("gzip decodes byte-identical", func(t *testing.T) {
		resp := doStatic(t, ws, http.MethodGet, "/assets/main.js", map[string]string{"Accept-Encoding": "gzip"})
		body := readBody(t, resp)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, "gzip", resp.Header.Get("Content-Encoding"))
		zr, err := gzip.NewReader(bytes.NewReader(body))
		require.NoError(t, err)
		decoded, err := io.ReadAll(zr)
		require.NoError(t, err)
		assert.Equal(t, validatorMainJS, decoded)
		assert.Equal(t, "no-cache", resp.Header.Get("Cache-Control"))
	})

	t.Run("identity without Accept-Encoding", func(t *testing.T) {
		resp := doStatic(t, ws, http.MethodGet, "/assets/main.js", nil)
		body := readBody(t, resp)
		assert.Empty(t, resp.Header.Get("Content-Encoding"))
		assert.Equal(t, validatorMainJS, body)
		assert.Equal(t, "Accept-Encoding", resp.Header.Get("Vary"))
	})

	t.Run("hashed chunk keeps the long lifetime", func(t *testing.T) {
		resp := doStatic(t, ws, http.MethodGet, "/assets/chunk-abc12345.js", map[string]string{"Accept-Encoding": "br"})
		_ = readBody(t, resp)
		assert.Equal(t, "br", resp.Header.Get("Content-Encoding"))
		assert.Equal(t, "public, max-age=86400", resp.Header.Get("Cache-Control"))
	})

	t.Run("sibling names are not served directly", func(t *testing.T) {
		for _, p := range []string{"/assets/main.js.br", "/assets/main.js.gz", "/assets/MAIN.JS.BR"} {
			resp := doStatic(t, ws, http.MethodGet, p, map[string]string{"Accept-Encoding": "br, gzip"})
			body := readBody(t, resp)
			assert.Equal(t, http.StatusNotFound, resp.StatusCode, p)
			assert.NotEqual(t, validatorMainBr, body, p)
			assert.Empty(t, resp.Header.Get("Content-Encoding"), p)
		}
	})

	t.Run("methods other than GET and HEAD never get a variant", func(t *testing.T) {
		for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
			resp := doStatic(t, ws, m, "/assets/main.js", map[string]string{"Accept-Encoding": "br"})
			body := readBody(t, resp)
			assert.Empty(t, resp.Header.Get("Content-Encoding"), m)
			assert.NotEqual(t, validatorMainBr, body, m)
		}
		resp := doStatic(t, ws, http.MethodHead, "/assets/main.js", map[string]string{"Accept-Encoding": "br"})
		body := readBody(t, resp)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "br", resp.Header.Get("Content-Encoding"))
		assert.Empty(t, body)
	})

	t.Run("encoded dot segments stay inside the root", func(t *testing.T) {
		for _, target := range []string{"/assets/%2e%2e/.vite/manifest.json", "/assets/..%2f.vite%2fmanifest.json", "/shoelace/%2e%2e/%2e%2e/.vite/manifest.json"} {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			u, err := url.Parse(target)
			require.NoError(t, err)
			req.URL = u
			req.RequestURI = target
			rec := httptest.NewRecorder()
			ws.Handler().ServeHTTP(rec, req)
			assert.Equal(t, http.StatusNotFound, rec.Code, target)
			assert.NotContains(t, rec.Body.String(), "outside-marker", target)
			assert.Empty(t, rec.Header().Get("Content-Encoding"), target)
		}
	})

	t.Run("non-clean decoded paths are refused, not cleaned", func(t *testing.T) {
		// The mux cleans only the escaped path: an encoded slash or dot
		// leaves dot segments in the decoded path. Cleaning them would
		// reach the stored sibling (main.js.br) or another file, so the
		// request is a 404.
		for _, target := range []string{
			"/assets/main.js.br%2f.",
			"/assets/main.js.br%2F%2e",
			"/assets/main.js.gz%2f.",
			"/assets/x%2f..%2fmain.js.br",
			"/assets/main.js%2f.",
			"/assets/%2e/main.js",
		} {
			req := httptest.NewRequest(http.MethodGet, target, nil)
			req.Header.Set("Accept-Encoding", "br, gzip")
			rec := httptest.NewRecorder()
			ws.Handler().ServeHTTP(rec, req)
			assert.Equal(t, http.StatusNotFound, rec.Code, target)
			assert.NotEqual(t, validatorMainBr, rec.Body.Bytes(), target)
			assert.NotContains(t, rec.Body.String(), "console.log", target)
			assert.Empty(t, rec.Header().Get("Content-Encoding"), target)
		}
	})
}

func TestStaticAsset_ConditionalGet304(t *testing.T) {
	ws := newValidatorWebServer(t)
	etagFor := func(accept string) string {
		resp := doStatic(t, ws, http.MethodGet, "/assets/main.js", map[string]string{"Accept-Encoding": accept})
		_ = readBody(t, resp)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		return resp.Header.Get("ETag")
	}
	brTag, gzTag, idTag := etagFor("br"), etagFor("gzip"), etagFor("")
	for _, tag := range []string{brTag, gzTag, idTag} {
		assert.True(t, strings.HasPrefix(tag, `"`) && strings.HasSuffix(tag, `"`), "strong ETag: %q", tag)
	}
	assert.NotEqual(t, brTag, gzTag)
	assert.NotEqual(t, brTag, idTag)
	assert.NotEqual(t, gzTag, idTag)
	assert.Equal(t, brTag, etagFor("br"), "ETag is stable")

	// The exact variant's validator: 304, no body, headers kept.
	resp := doStatic(t, ws, http.MethodGet, "/assets/main.js", map[string]string{"Accept-Encoding": "br", "If-None-Match": brTag})
	body := readBody(t, resp)
	assert.Equal(t, http.StatusNotModified, resp.StatusCode)
	assert.Empty(t, body)
	assert.Equal(t, brTag, resp.Header.Get("ETag"))
	assert.Equal(t, "Accept-Encoding", resp.Header.Get("Vary"))
	assert.Equal(t, "no-cache", resp.Header.Get("Cache-Control"))

	// Another variant's validator does not match what would be served.
	for _, tc := range []struct{ accept, inm string }{
		{"gzip", brTag},
		{"br", gzTag},
		{"", brTag},
		{"br", idTag},
	} {
		resp := doStatic(t, ws, http.MethodGet, "/assets/main.js", map[string]string{"Accept-Encoding": tc.accept, "If-None-Match": tc.inm})
		body := readBody(t, resp)
		assert.Equal(t, http.StatusOK, resp.StatusCode, "Accept-Encoding %q, If-None-Match %s", tc.accept, tc.inm)
		assert.NotEmpty(t, body)
	}

	// A list containing the served variant's tag matches.
	resp = doStatic(t, ws, http.MethodGet, "/assets/main.js", map[string]string{"Accept-Encoding": "gzip", "If-None-Match": brTag + ", " + gzTag})
	_ = readBody(t, resp)
	assert.Equal(t, http.StatusNotModified, resp.StatusCode)

	t.Run("disk mode uses weak validators", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "assets"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "assets", "main.js"), validatorMainJS, 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "assets", "main.js.br"), validatorMainBr, 0o644))
		disk := newTestWebServer(t, WebServerConfig{AssetsDir: dir})
		resp := doStatic(t, disk, http.MethodGet, "/assets/main.js", map[string]string{"Accept-Encoding": "br"})
		body := readBody(t, resp)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "br", resp.Header.Get("Content-Encoding"))
		assert.Equal(t, validatorMainBr, body)
		tag := resp.Header.Get("ETag")
		assert.True(t, strings.HasPrefix(tag, `W/"`), tag)
		resp = doStatic(t, disk, http.MethodGet, "/assets/main.js", map[string]string{"Accept-Encoding": "br", "If-None-Match": tag})
		_ = readBody(t, resp)
		assert.Equal(t, http.StatusNotModified, resp.StatusCode)
		resp = doStatic(t, disk, http.MethodGet, "/assets/main.js", map[string]string{"If-None-Match": tag})
		_ = readBody(t, resp)
		assert.Equal(t, http.StatusOK, resp.StatusCode, "identity does not match the br validator")
	})
}

func TestRootStaticFile_ETag(t *testing.T) {
	ws := newValidatorWebServer(t)
	resp := doStatic(t, ws, http.MethodGet, "/favicon.svg", map[string]string{"Accept-Encoding": "br"})
	body := readBody(t, resp)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "br", resp.Header.Get("Content-Encoding"))
	assert.Equal(t, validatorFavBr, body)
	assert.Contains(t, resp.Header.Get("Content-Type"), "image/svg+xml")
	assert.Equal(t, "no-cache", resp.Header.Get("Cache-Control"))
	tag := resp.Header.Get("ETag")
	require.True(t, strings.HasPrefix(tag, `"`) && strings.HasSuffix(tag, `-br"`), tag)

	resp = doStatic(t, ws, http.MethodGet, "/favicon.svg", map[string]string{"Accept-Encoding": "br", "If-None-Match": tag})
	body = readBody(t, resp)
	assert.Equal(t, http.StatusNotModified, resp.StatusCode)
	assert.Empty(t, body)

	resp = doStatic(t, ws, http.MethodGet, "/favicon.svg", nil)
	body = readBody(t, resp)
	assert.Equal(t, validatorFavicon, body)
	assert.NotEqual(t, tag, resp.Header.Get("ETag"))

	// The sibling name is not a root file: it never returns the variant.
	resp = doStatic(t, ws, http.MethodGet, "/favicon.svg.br", map[string]string{"Accept-Encoding": "br"})
	body = readBody(t, resp)
	assert.NotEqual(t, validatorFavBr, body)
	assert.Empty(t, resp.Header.Get("Content-Encoding"))
}

// TestStaticAsset_IndexFollowsAssetSource pins that the index never serves
// a stale source: swapping ws.assets (as tests do) rebuilds it, and
// concurrent first requests agree on one index.
func TestStaticAsset_IndexFollowsAssetSource(t *testing.T) {
	ws := newValidatorWebServer(t)
	resp := doStatic(t, ws, http.MethodGet, "/assets/main.js", nil)
	assert.Equal(t, validatorMainJS, readBody(t, resp))

	ws.assets = fstest.MapFS{"assets/main.js": {Data: []byte("// swapped")}}
	var wg sync.WaitGroup
	bodies := make([][]byte, 8)
	for i := range bodies {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/assets/main.js", nil)
			req.Header.Set("Accept-Encoding", "br")
			rec := httptest.NewRecorder()
			ws.Handler().ServeHTTP(rec, req)
			bodies[i] = rec.Body.Bytes()
		}(i)
	}
	wg.Wait()
	for i, b := range bodies {
		assert.Equal(t, "// swapped", string(b), "request %d", i)
	}

	ws.assets = nil
	ws.assetsDisk = t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(ws.assetsDisk, "assets"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(ws.assetsDisk, "assets", "main.js"), []byte("// disk"), 0o644))
	resp = doStatic(t, ws, http.MethodGet, "/assets/main.js", nil)
	assert.Equal(t, "// disk", string(readBody(t, resp)))
}

// writeValidatorDisk writes the validator build into a temp dir for disk
// mode (--web-assets-dir).
func writeValidatorDisk(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	// One shared mtime: the files are written in map order, and DiskIndex
	// ignores a sibling older than its source, so a clock tick between a
	// sibling and its source must not drop the sibling.
	mtime := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for name, f := range validatorAssets(t) {
		p := filepath.Join(dir, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, f.Data, 0o644))
		require.NoError(t, os.Chtimes(p, mtime, mtime))
	}
	return dir
}

// validatorServers returns, for embedded and disk mode, a plain server and
// a dev-auth server over the validator build. Signed in (devSignedIn), the
// dev-auth server lets root-level and deep-link requests reach the SPA
// catch-all (and so tryServeStaticFile) rather than the login gate.
func validatorServers(t *testing.T) map[string][2]*WebServer {
	t.Helper()
	embedded := newValidatorWebServer(t)
	embeddedAuth := newDevAuthWebServer(t)
	embeddedAuth.assets = validatorAssets(t)
	embeddedAuth.assetsDisk = ""
	embeddedAuth.hasAssets = embeddedAuth.detectWebAssets()
	require.True(t, embeddedAuth.hasAssets)

	dir := writeValidatorDisk(t)
	disk := newTestWebServer(t, WebServerConfig{AssetsDir: dir})
	require.True(t, disk.hasAssets)
	diskAuth := newDevAuthWebServer(t, func(cfg *WebServerConfig) { cfg.AssetsDir = writeValidatorDisk(t) })
	require.True(t, diskAuth.hasAssets)
	return map[string][2]*WebServer{
		"embedded": {embedded, embeddedAuth},
		"disk":     {disk, diskAuth},
	}
}

// TestStaticAsset_SiblingNameForms covers every spelling of a sibling
// name, through the /assets/ handler (serveStaticAsset) and the root-file
// path (tryServeStaticFile), in embedded and disk mode: none returns the
// stored variant or any Content-Encoding. The /assets/ forms get a 404; the
// root forms get the SPA shell, as any missing root file does. A literal dot
// segment gets the mux's redirect, which is followed once and checked the
// same way.
func TestStaticAsset_SiblingNameForms(t *testing.T) {
	gzMain := gzipBytes(t, validatorMainJS)
	gzFav := gzipBytes(t, validatorFavicon)
	cases := []struct {
		base   string // the sibling, e.g. /assets/main.js.br
		stored []byte
		root   bool // served through tryServeStaticFile
	}{
		{"/assets/main.js.br", validatorMainBr, false},
		{"/assets/main.js.gz", gzMain, false},
		{"/favicon.svg.br", validatorFavBr, true},
		{"/favicon.svg.gz", gzFav, true},
	}
	// check asserts that a response never carries the stored variant.
	check := func(t *testing.T, target string, rec *httptest.ResponseRecorder, stored []byte, root bool) {
		t.Helper()
		body := rec.Body.Bytes()
		assert.NotEqual(t, stored, body, target)
		assert.False(t, bytes.Contains(body, stored), "%s: body holds the stored variant", target)
		assert.Empty(t, rec.Header().Get("Content-Encoding"), target)
		switch {
		case !root && rec.Code == http.StatusNotFound:
		case rec.Code == http.StatusOK && root:
			// The SPA shell. Under /assets/ a dot-dot form lands on the
			// directory, which is a 404 (no listings).
			assert.Contains(t, rec.Header().Get("Content-Type"), "text/html", "%s: a 200 must be HTML", target)
		default:
			t.Errorf("%s: unexpected status %d", target, rec.Code)
		}
	}
	isRedirect := func(code int) bool {
		return code == http.StatusMovedPermanently || code == http.StatusTemporaryRedirect || code == http.StatusPermanentRedirect
	}
	for mode, servers := range validatorServers(t) {
		t.Run(mode, func(t *testing.T) {
			ws, wsAuth := servers[0], servers[1]
			signedIn := devSignedIn(t, wsAuth)
			srv := func(root bool) http.Handler {
				if root {
					return signedIn
				}
				return ws.Handler()
			}
			for _, tc := range cases {
				for _, suffix := range []string{"", "/", "%2F", "%2f", "/.", "%2f.", "/..", "%2f..", "%2F%2E", "%2f%2e%2e"} {
					target := tc.base + suffix
					req := httptest.NewRequest(http.MethodGet, target, nil)
					req.Header.Set("Accept-Encoding", "br, gzip")
					rec := httptest.NewRecorder()
					srv(tc.root).ServeHTTP(rec, req)
					if isRedirect(rec.Code) {
						// The mux cleans a literal dot segment with a
						// redirect; follow it once and check where it lands.
						assert.Empty(t, rec.Header().Get("Content-Encoding"), target)
						loc := rec.Header().Get("Location")
						require.NotEmpty(t, loc, "%s: redirect without Location", target)
						assert.False(t, bytes.Contains(rec.Body.Bytes(), tc.stored), "%s: redirect body holds the stored variant", target)
						req = httptest.NewRequest(http.MethodGet, loc, nil)
						req.Header.Set("Accept-Encoding", "br, gzip")
						rec = httptest.NewRecorder()
						srv(tc.root).ServeHTTP(rec, req)
						target += " -> " + loc
					}
					check(t, target, rec, tc.stored, tc.root)
				}
			}
		})
	}
}

// TestSPADeepLinks_EncodedSegmentsGetShell pins that client-side routes,
// including encoded segments, still get the SPA shell in embedded and disk
// mode: the static-file check only ever falls through to the shell.
func TestSPADeepLinks_EncodedSegmentsGetShell(t *testing.T) {
	for mode, servers := range validatorServers(t) {
		t.Run(mode, func(t *testing.T) {
			signedIn := devSignedIn(t, servers[1])
			for _, target := range []string{
				"/projects/x/agents/y",
				"/projects/a%20b/agents/y",
				"/projects/x/artifacts/a%2Fb",
				"/projects/x/artifacts/a%2F%2Fb",
			} {
				req := httptest.NewRequest(http.MethodGet, target, nil)
				rec := httptest.NewRecorder()
				signedIn.ServeHTTP(rec, req)
				assert.Equal(t, http.StatusOK, rec.Code, target)
				assert.Contains(t, rec.Header().Get("Content-Type"), "text/html", target)
				assert.Contains(t, rec.Body.String(), "scion-app", target)
			}
		})
	}
}

func TestCleanStaticPath(t *testing.T) {
	for in, want := range map[string]string{
		"":                       "/",
		"/":                      "/",
		"/assets/":               "/assets/",
		"/assets/main.js":        "/assets/main.js",
		"/assets/main.js.br/.":   "/assets/main.js.br",
		"/assets/../favicon.svg": "/favicon.svg",
		"/assets//main.js":       "/assets/main.js",
		"/assets/./":             "/assets/",
		"assets/main.js":         "/assets/main.js",
	} {
		assert.Equal(t, want, cleanStaticPath(in), in)
	}
}

func TestSameFS(t *testing.T) {
	a := fstest.MapFS{"x": {Data: []byte("1")}}
	b := fstest.MapFS{"x": {Data: []byte("1")}}
	dir := t.TempDir()
	assert.True(t, sameFS(a, a))
	assert.False(t, sameFS(a, b), "equal contents, different maps")
	assert.True(t, sameFS(os.DirFS(dir), os.DirFS(dir)))
	assert.False(t, sameFS(os.DirFS(dir), a))
	assert.True(t, sameFS(nil, nil))
	assert.False(t, sameFS(a, nil))
}
