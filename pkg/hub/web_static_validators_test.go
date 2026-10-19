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
	ws.fingerprintedAssets = ws.loadFingerprintedAssets()
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
			assert.NotContains(t, rec.Body.String(), "outside-marker", target)
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

// TestStaticAsset_DirectoryFallbackUnchanged pins that A1 keeps the
// http.FileServer fallback for everything the index does not serve: the
// directory listing still works (A4 changes it later).
func TestStaticAsset_DirectoryFallbackUnchanged(t *testing.T) {
	ws := newValidatorWebServer(t)
	resp := doStatic(t, ws, http.MethodGet, "/assets/", nil)
	body := readBody(t, resp)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, string(body), "main.js")
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/html")
	assert.Empty(t, resp.Header.Get("ETag"), "listings are not indexed")

	t.Run("disk", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "assets"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "assets", "main.js"), validatorMainJS, 0o644))
		disk := newTestWebServer(t, WebServerConfig{AssetsDir: dir})
		resp := doStatic(t, disk, http.MethodGet, "/assets/", nil)
		body := readBody(t, resp)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Contains(t, string(body), "main.js")
	})

	t.Run("disk symlink out of the root is not served", func(t *testing.T) {
		outside := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(outside, "outside.js"), []byte("outside-marker"), 0o644))
		dir := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "assets"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "assets", "main.js"), validatorMainJS, 0o644))
		if err := os.Symlink(filepath.Join(outside, "outside.js"), filepath.Join(dir, "assets", "linked.js")); err != nil {
			t.Skipf("symlinks unsupported: %v", err)
		}
		require.NoError(t, os.Symlink(outside, filepath.Join(dir, "assets", "out")))
		disk := newTestWebServer(t, WebServerConfig{AssetsDir: dir})
		for _, p := range []string{"/assets/linked.js", "/assets/out/outside.js", "/assets/out/"} {
			resp := doStatic(t, disk, http.MethodGet, p, nil)
			body := readBody(t, resp)
			assert.Equal(t, http.StatusNotFound, resp.StatusCode, p)
			assert.NotContains(t, string(body), "outside-marker", p)
		}
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
