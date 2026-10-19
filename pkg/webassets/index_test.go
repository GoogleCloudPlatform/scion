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

package webassets

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

// mainJS is large enough that its fake variants differ from it.
var mainJS = []byte(strings.Repeat("console.log('scion');\n", 100))

func gz(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zw.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// fakeBrotli stands in for a brotli body: the server never decodes a
// variant, it only sends its bytes, so the tests compare bytes.
var fakeBrotli = []byte("BROTLI-BYTES-of-main.js")

func testFS(t *testing.T) fstest.MapFS {
	t.Helper()
	return fstest.MapFS{
		"assets/main.js":               {Data: mainJS},
		"assets/main.js.br":            {Data: fakeBrotli},
		"assets/main.js.gz":            {Data: gz(t, mainJS)},
		"assets/plain.js":              {Data: []byte("plain")},
		"assets/style.css":             {Data: []byte("body{}")},
		"assets/style.css.gz":          {Data: gz(t, []byte("body{}"))},
		"assets/sub/deep.js":           {Data: []byte("deep")},
		"shoelace/assets/icons/x.svg":  {Data: []byte("<svg/>")},
		"favicon.svg":                  {Data: []byte("<svg/>")},
		"favicon.svg.br":               {Data: []byte("svg-br")},
		"noext":                        {Data: []byte("<!DOCTYPE html><html></html>")},
		".vite/manifest.json":          {Data: []byte("{}")},
		"other/file.js":                {Data: []byte("out of scope")},
		"assets/orphan.js.gz":          {Data: gz(t, []byte("orphan"))},
		"assets/dir.js/placeholder.js": {Data: []byte("x")},
		"assets/index.html":            {Data: []byte("<html></html>")},
	}
}

func newTestIndex(t *testing.T) *Index {
	t.Helper()
	ix, err := NewIndex(testFS(t))
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	return ix
}

func noCache(string) string { return "no-cache" }

func serve(t *testing.T, s interface {
	Serve(http.ResponseWriter, *http.Request, CachePolicy) bool
}, method, target string, header map[string]string) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	found := s.Serve(rec, req, noCache)
	return rec, found
}

func TestServe_Encodings(t *testing.T) {
	ix := newTestIndex(t)
	cases := []struct {
		accept, wantEnc string
		wantBody        []byte
	}{
		{"br, gzip", "br", fakeBrotli},
		{"gzip", "gzip", nil}, // decoded below
		{"", "", mainJS},
	}
	for _, tc := range cases {
		rec, found := serve(t, ix, http.MethodGet, "/assets/main.js", map[string]string{"Accept-Encoding": tc.accept})
		if !found || rec.Code != http.StatusOK {
			t.Fatalf("%q: found=%v code=%d", tc.accept, found, rec.Code)
		}
		if got := rec.Header().Get("Content-Encoding"); got != tc.wantEnc {
			t.Errorf("%q: Content-Encoding = %q, want %q", tc.accept, got, tc.wantEnc)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("%q: Cache-Control = %q", tc.accept, got)
		}
		body := rec.Body.Bytes()
		if tc.wantEnc == "gzip" {
			zr, err := gzip.NewReader(bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			if body, err = io.ReadAll(zr); err != nil {
				t.Fatal(err)
			}
			tc.wantBody = mainJS
		}
		if !bytes.Equal(body, tc.wantBody) {
			t.Errorf("%q: body mismatch", tc.accept)
		}
	}
}

func TestServe_ETag304(t *testing.T) {
	ix := newTestIndex(t)
	first, _ := serve(t, ix, http.MethodGet, "/assets/main.js", map[string]string{"Accept-Encoding": "br"})
	brTag := first.Header().Get("ETag")
	if !strings.HasPrefix(brTag, `"`) || !strings.HasSuffix(brTag, `-br"`) {
		t.Fatalf("br ETag = %q, want a strong tag ending in -br", brTag)
	}
	idRec, _ := serve(t, ix, http.MethodGet, "/assets/main.js", nil)
	idTag := idRec.Header().Get("ETag")
	gzRec, _ := serve(t, ix, http.MethodGet, "/assets/main.js", map[string]string{"Accept-Encoding": "gzip"})
	gzTag := gzRec.Header().Get("ETag")
	if idTag == brTag || idTag == gzTag || brTag == gzTag || !strings.HasSuffix(gzTag, `-gz"`) {
		t.Fatalf("variant ETags must differ: identity %q br %q gzip %q", idTag, brTag, gzTag)
	}
	if strings.TrimSuffix(brTag, `-br"`) != strings.TrimSuffix(idTag, `"`) {
		t.Errorf("br ETag %q is not the identity ETag %q plus -br", brTag, idTag)
	}

	// Matching validator: 304 with an empty body.
	rec, _ := serve(t, ix, http.MethodGet, "/assets/main.js", map[string]string{"Accept-Encoding": "br", "If-None-Match": brTag})
	if rec.Code != http.StatusNotModified {
		t.Fatalf("matching If-None-Match: code %d, want 304", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("304 has a body of %d bytes", rec.Body.Len())
	}
	if rec.Header().Get("ETag") != brTag {
		t.Errorf("304 ETag = %q", rec.Header().Get("ETag"))
	}

	// A different variant's validator does not match: 200 with the body.
	rec, _ = serve(t, ix, http.MethodGet, "/assets/main.js", map[string]string{"Accept-Encoding": "gzip", "If-None-Match": brTag})
	if rec.Code != http.StatusOK || rec.Body.Len() == 0 {
		t.Errorf("other variant's ETag: code %d body %d, want 200 with body", rec.Code, rec.Body.Len())
	}

	// The ETag is stable across index builds of the same bytes.
	again, _ := serve(t, newTestIndex(t), http.MethodGet, "/assets/main.js", nil)
	if again.Header().Get("ETag") != idTag {
		t.Errorf("ETag not stable: %q vs %q", again.Header().Get("ETag"), idTag)
	}
}

func TestServe_VaryAlways(t *testing.T) {
	ix := newTestIndex(t)
	for _, accept := range []string{"br", "gzip", "", "identity", "*;q=0"} {
		rec, _ := serve(t, ix, http.MethodGet, "/assets/main.js", map[string]string{"Accept-Encoding": accept})
		if got := rec.Header().Values("Vary"); len(got) != 1 || got[0] != "Accept-Encoding" {
			t.Errorf("Accept-Encoding %q: Vary = %q, want exactly Accept-Encoding", accept, got)
		}
	}
	// 304s keep Vary too.
	first, _ := serve(t, ix, http.MethodGet, "/assets/main.js", nil)
	rec, _ := serve(t, ix, http.MethodGet, "/assets/main.js", map[string]string{"If-None-Match": first.Header().Get("ETag")})
	if rec.Code != http.StatusNotModified || rec.Header().Get("Vary") != "Accept-Encoding" {
		t.Errorf("304: code %d Vary %q", rec.Code, rec.Header().Get("Vary"))
	}
	// A file without variants has no Vary.
	rec, _ = serve(t, ix, http.MethodGet, "/assets/plain.js", map[string]string{"Accept-Encoding": "br"})
	if got := rec.Header().Get("Vary"); got != "" {
		t.Errorf("no-variant file: Vary = %q, want none", got)
	}
}

func TestServe_ContentTypeFromRawName(t *testing.T) {
	ix := newTestIndex(t)
	for _, tc := range []struct{ path, accept, wantPrefix string }{
		{"/assets/main.js", "br", "text/javascript"},
		{"/assets/main.js", "gzip", "text/javascript"},
		{"/assets/main.js", "", "text/javascript"},
		{"/assets/style.css", "gzip", "text/css"},
		{"/favicon.svg", "br", "image/svg+xml"},
		{"/noext", "", "text/html"}, // sniffed from the raw bytes
	} {
		rec, found := serve(t, ix, http.MethodGet, tc.path, map[string]string{"Accept-Encoding": tc.accept})
		if !found {
			t.Fatalf("%s not found", tc.path)
		}
		if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, tc.wantPrefix) {
			t.Errorf("%s (%q): Content-Type = %q, want %s...", tc.path, tc.accept, got, tc.wantPrefix)
		}
	}
	// A Content-Type the caller set first wins.
	req := httptest.NewRequest(http.MethodGet, "/assets/main.js", nil)
	req.Header.Set("Accept-Encoding", "br")
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "application/x-custom")
	ix.Serve(rec, req, noCache)
	if got := rec.Header().Get("Content-Type"); got != "application/x-custom" {
		t.Errorf("caller Content-Type overridden: %q", got)
	}
}

func TestServe_RangeOnVariant(t *testing.T) {
	ix := newTestIndex(t)
	rec, _ := serve(t, ix, http.MethodGet, "/assets/main.js", map[string]string{"Accept-Encoding": "br", "Range": "bytes=0-5"})
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("code %d, want 206", rec.Code)
	}
	if got := rec.Body.Bytes(); !bytes.Equal(got, fakeBrotli[:6]) {
		t.Errorf("range body %q, want %q (bytes of the br variant)", got, fakeBrotli[:6])
	}
	want := fmt.Sprintf("bytes 0-5/%d", len(fakeBrotli))
	if got := rec.Header().Get("Content-Range"); got != want {
		t.Errorf("Content-Range = %q, want %q", got, want)
	}
	if rec.Header().Get("Content-Encoding") != "br" {
		t.Errorf("Content-Encoding = %q", rec.Header().Get("Content-Encoding"))
	}
	// HEAD: headers only. (http.ServeContent leaves Content-Length out
	// when Content-Encoding is set; net/http then sizes or chunks the body.)
	rec, _ = serve(t, ix, http.MethodHead, "/assets/main.js", map[string]string{"Accept-Encoding": "br"})
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 || rec.Header().Get("Content-Encoding") != "br" {
		t.Errorf("HEAD: code %d body %d Content-Encoding %q", rec.Code, rec.Body.Len(), rec.Header().Get("Content-Encoding"))
	}
}

func TestServe_SiblingNamesNotServable(t *testing.T) {
	ix := newTestIndex(t)
	for _, p := range []string{"/assets/main.js.br", "/assets/main.js.gz", "/assets/MAIN.JS.BR", "/favicon.svg.br", "/assets/orphan.js.gz", "/assets/orphan.js"} {
		rec, found := serve(t, ix, http.MethodGet, p, map[string]string{"Accept-Encoding": "br, gzip"})
		if found {
			t.Errorf("%s served (code %d)", p, rec.Code)
		}
		if rec.Body.Len() != 0 || len(rec.Header()) != 0 {
			t.Errorf("%s: a miss must write nothing", p)
		}
	}
}

func TestServe_NotIndexedFallsBack(t *testing.T) {
	ix := newTestIndex(t)
	for _, p := range []string{"/assets/", "/assets", "/shoelace/assets/icons/", "/", "/.vite/manifest.json", "/other/file.js", "/assets/missing.js", "/assets/dir.js", "/assets/index.html"} {
		if _, found := serve(t, ix, http.MethodGet, p, nil); found {
			t.Errorf("%s: found, want fallback", p)
		}
	}
	for _, p := range []string{"/assets/sub/deep.js", "/shoelace/assets/icons/x.svg", "/favicon.svg", "/assets/dir.js/placeholder.js"} {
		if _, found := serve(t, ix, http.MethodGet, p, nil); !found {
			t.Errorf("%s: not found, want served", p)
		}
	}
	var nilIx *Index
	if _, found := serve(t, nilIx, http.MethodGet, "/assets/main.js", nil); found {
		t.Error("nil index served")
	}
}

func TestNewIndex_NonCleanPathRejected(t *testing.T) {
	ix := newTestIndex(t)
	for _, p := range []string{
		"/assets/../assets/main.js",
		"/assets/../.vite/manifest.json",
		"/../assets/main.js",
		"/assets/./main.js",
		"//assets/main.js",
		"/assets//main.js",
		`/assets\main.js`,
		`/assets/..\..\main.js`,
		"/assets/main.js/",
		"/assets/main.js\x00",
		"assets/main.js",
		"",
	} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.URL.Path = p
		rec := httptest.NewRecorder()
		if ix.Serve(rec, req, noCache) {
			t.Errorf("%q served (code %d)", p, rec.Code)
		}
	}
	// Percent-encoded dot segments decode to a non-clean path and are refused.
	for _, target := range []string{"/assets/%2e%2e/.vite/manifest.json", "/assets/%2E%2E%2F.vite%2Fmanifest.json", "/%2e%2e/assets/main.js", "/assets/main.js%00", "/assets/main.js%2ebr"} {
		u, err := url.Parse(target)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.URL = u
		rec := httptest.NewRecorder()
		if ix.Serve(rec, req, noCache) {
			t.Errorf("%s (decoded %q) served", target, u.Path)
		}
	}
	if _, ok := ix.entries[".vite/manifest.json"]; ok {
		t.Error(".vite/ is indexed")
	}
	for name := range ix.entries {
		if strings.Contains(name, "..") || strings.HasPrefix(name, "/") || IsVariantName(name) {
			t.Errorf("bad index key %q", name)
		}
	}
}

func TestNewIndex_NilFS(t *testing.T) {
	ix, err := NewIndex(nil)
	if err == nil || ix != nil {
		t.Fatalf("NewIndex(nil) = %v, %v; want nil index and an error", ix, err)
	}
}

// TestNewIndex_HashingTime10MB bounds the startup cost of hashing (design
// risk 3): indexing 10 MB of assets must take under 200 ms.
func TestNewIndex_HashingTime10MB(t *testing.T) {
	fsys := fstest.MapFS{}
	const files, size = 100, (10 << 20) / 100 // 10 MiB in about 100 KiB chunks, like a client build
	for i := 0; i < files; i++ {
		b := make([]byte, size)
		if _, err := rand.Read(b); err != nil {
			t.Fatal(err)
		}
		fsys[fmt.Sprintf("assets/chunk-%03d.js", i)] = &fstest.MapFile{Data: b}
		fsys[fmt.Sprintf("assets/chunk-%03d.js.br", i)] = &fstest.MapFile{Data: b[:size/4]}
	}
	start := time.Now()
	ix, err := NewIndex(fsys)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if ix.Len() != files {
		t.Fatalf("indexed %d files, want %d", ix.Len(), files)
	}
	t.Logf("indexed %d bytes in %v", files*size, elapsed)
	// The race detector slows hashing several times over; the bound holds
	// for normal builds only (the nightly race job runs this package).
	if !raceEnabled && elapsed >= 200*time.Millisecond {
		t.Errorf("indexing 10 MB took %v, want < 200ms", elapsed)
	}
}
