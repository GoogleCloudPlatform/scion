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
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, p string, data []byte, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func TestDiskIndex_ServesFreshSibling(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().Truncate(time.Second)
	writeFile(t, filepath.Join(dir, "assets", "main.js"), mainJS, now)
	writeFile(t, filepath.Join(dir, "assets", "main.js.br"), fakeBrotli, now)
	d := NewDiskIndex(dir)

	rec, found := serve(t, d, http.MethodGet, "/assets/main.js", map[string]string{"Accept-Encoding": "br"})
	if !found || rec.Code != http.StatusOK {
		t.Fatalf("found=%v code=%d", found, rec.Code)
	}
	if rec.Header().Get("Content-Encoding") != "br" || !bytes.Equal(rec.Body.Bytes(), fakeBrotli) {
		t.Errorf("Content-Encoding %q, body %q", rec.Header().Get("Content-Encoding"), rec.Body.Bytes())
	}
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/javascript") {
		t.Errorf("Content-Type = %q", rec.Header().Get("Content-Type"))
	}
	etag := rec.Header().Get("ETag")
	if !strings.HasPrefix(etag, `W/"`) || !strings.HasSuffix(etag, `-br"`) {
		t.Errorf("ETag = %q, want weak with -br", etag)
	}
	if rec.Header().Get("Vary") != "Accept-Encoding" || rec.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("Vary %q Cache-Control %q", rec.Header().Get("Vary"), rec.Header().Get("Cache-Control"))
	}
	rec, _ = serve(t, d, http.MethodGet, "/assets/main.js", map[string]string{"Accept-Encoding": "br", "If-None-Match": etag})
	if rec.Code != http.StatusNotModified {
		t.Errorf("If-None-Match: code %d, want 304", rec.Code)
	}

	for _, p := range []string{"/assets/main.js.br", "/assets/", "/assets/missing.js", "/assets/../assets/main.js"} {
		if _, found := serve(t, d, http.MethodGet, p, nil); found {
			t.Errorf("%s served", p)
		}
	}
}

func TestDiskIndex_StaleSiblingIgnored(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	writeFile(t, filepath.Join(dir, "assets", "main.js.br"), fakeBrotli, old)
	writeFile(t, filepath.Join(dir, "assets", "main.js.gz"), gz(t, []byte("old build")), old)
	// The raw file was rebuilt after its siblings were written.
	writeFile(t, filepath.Join(dir, "assets", "main.js"), mainJS, old.Add(time.Minute))
	d := NewDiskIndex(dir)

	rec, found := serve(t, d, http.MethodGet, "/assets/main.js", map[string]string{"Accept-Encoding": "br, gzip"})
	if !found || rec.Code != http.StatusOK {
		t.Fatalf("found=%v code=%d", found, rec.Code)
	}
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q, want identity (siblings are stale)", got)
	}
	if !bytes.Equal(rec.Body.Bytes(), mainJS) {
		t.Error("body is not the rebuilt raw file")
	}
	if got := rec.Header().Get("Vary"); got != "" {
		t.Errorf("Vary = %q, want none when no sibling is usable", got)
	}

	// Once the sibling is regenerated it is used again.
	writeFile(t, filepath.Join(dir, "assets", "main.js.br"), fakeBrotli, old.Add(2*time.Minute))
	rec, _ = serve(t, d, http.MethodGet, "/assets/main.js", map[string]string{"Accept-Encoding": "br, gzip"})
	if got := rec.Header().Get("Content-Encoding"); got != "br" {
		t.Errorf("after regenerating: Content-Encoding = %q, want br", got)
	}
}

func TestDiskIndex_SymlinkEscapeRejected(t *testing.T) {
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "secret.js"), []byte("secret"), time.Now())
	writeFile(t, filepath.Join(outside, "secret.js.br"), []byte("secret-br"), time.Now())
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "assets", "main.js"), mainJS, time.Now())
	// A symlinked file and a symlinked directory pointing outside the root.
	if err := os.Symlink(filepath.Join(outside, "secret.js"), filepath.Join(dir, "assets", "leak.js")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "assets", "out")); err != nil {
		t.Fatal(err)
	}
	// A symlink that stays inside the root keeps working.
	if err := os.Symlink("main.js", filepath.Join(dir, "assets", "alias.js")); err != nil {
		t.Fatal(err)
	}
	d := NewDiskIndex(dir)
	for _, p := range []string{"/assets/leak.js", "/assets/out/secret.js"} {
		rec, found := serve(t, d, http.MethodGet, p, map[string]string{"Accept-Encoding": "br"})
		if found || strings.Contains(rec.Body.String(), "secret") {
			t.Errorf("%s: served outside the root (found=%v body=%q)", p, found, rec.Body.String())
		}
	}
	if rec, found := serve(t, d, http.MethodGet, "/assets/alias.js", nil); !found || !bytes.Equal(rec.Body.Bytes(), mainJS) {
		t.Errorf("in-root symlink: found=%v", found)
	}

	// The fallback file system refuses the same paths, and siblings.
	fsys := DirFileSystem(dir)
	for _, name := range []string{"/assets/leak.js", "/assets/out/secret.js", "/assets/out", "/../" + filepath.Base(outside) + "/secret.js", "/assets/main.js.br", "/assets/main.js.gz"} {
		if f, err := fsys.Open(name); err == nil {
			_ = f.Close()
			t.Errorf("DirFileSystem opened %s", name)
		} else if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("DirFileSystem %s: err %v, want fs.ErrNotExist", name, err)
		}
	}
	for _, name := range []string{"/", "/assets", "/assets/", "/assets/main.js", "/assets/alias.js"} {
		f, err := fsys.Open(name)
		if err != nil {
			t.Errorf("DirFileSystem %s: %v", name, err)
			continue
		}
		_ = f.Close()
	}
	// Through a real file server: 404, not the secret, and listings still work.
	srv := http.FileServer(fsys)
	for _, p := range []string{"/assets/leak.js", "/assets/out/secret.js", "/assets/main.js.br"} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "secret") {
			t.Errorf("file server %s: code %d", p, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets/", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "main.js") {
		t.Errorf("listing: code %d body %q", rec.Code, rec.Body.String())
	}
}

func TestDiskIndex_EncodedTraversalRejected(t *testing.T) {
	parent := t.TempDir()
	writeFile(t, filepath.Join(parent, "secret.js"), []byte("secret"), time.Now())
	dir := filepath.Join(parent, "client")
	writeFile(t, filepath.Join(dir, "assets", "main.js"), mainJS, time.Now())
	d := NewDiskIndex(dir)
	for _, target := range []string{"/assets/%2e%2e/%2e%2e/secret.js", "/assets/..%2f..%2fsecret.js", "/%2e%2e/secret.js", "/assets/..%5c..%5csecret.js"} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		u, err := url.Parse(target)
		if err != nil {
			t.Fatal(err)
		}
		req.URL = u
		rec := httptest.NewRecorder()
		if d.Serve(rec, req, noCache) || strings.Contains(rec.Body.String(), "secret") {
			t.Errorf("%s (decoded %q) served", target, u.Path)
		}
	}
}

func TestServe_MethodsOtherThanGetHead(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "assets", "main.js"), mainJS, time.Now())
	writeFile(t, filepath.Join(dir, "assets", "main.js.br"), fakeBrotli, time.Now())
	for name, s := range map[string]interface {
		Serve(http.ResponseWriter, *http.Request, CachePolicy) bool
	}{"index": newTestIndex(t), "disk": NewDiskIndex(dir)} {
		for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions, http.MethodPatch} {
			if rec, found := serve(t, s, m, "/assets/main.js", map[string]string{"Accept-Encoding": "br"}); found || len(rec.Header()) != 0 {
				t.Errorf("%s %s: served (found=%v)", name, m, found)
			}
		}
		if _, found := serve(t, s, http.MethodHead, "/assets/main.js", nil); !found {
			t.Errorf("%s HEAD: not served", name)
		}
	}
}
