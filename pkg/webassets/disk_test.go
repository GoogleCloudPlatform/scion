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
	"net/http"
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
