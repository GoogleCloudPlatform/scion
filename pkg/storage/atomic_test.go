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

package storage

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/api/option"
)

// failingReader yields some bytes, then an error.
type failingReader struct {
	data []byte
	sent bool
}

func (r *failingReader) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		return copy(p, r.data), nil
	}
	return 0, errors.New("connection reset")
}

func newTestLocal(t *testing.T) (*LocalStorage, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := NewLocal(Config{Provider: ProviderLocal, Bucket: "b", LocalPath: dir})
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func readAll(t *testing.T, s *LocalStorage, p string) string {
	t.Helper()
	rc, _, err := s.Download(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestLocalUploadFailedRewriteKeepsOldBytes: a re-upload that fails part
// way leaves the existing object exactly as it was, and no temporary file.
func TestLocalUploadFailedRewriteKeepsOldBytes(t *testing.T) {
	s, dir := newTestLocal(t)
	ctx := context.Background()
	old := strings.Repeat("old content ", 200)
	if _, err := s.Upload(ctx, "a/b/obj", strings.NewReader(old), UploadOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Upload(ctx, "a/b/obj", &failingReader{data: []byte("partial")}, UploadOptions{}); err == nil {
		t.Fatal("a failing upload succeeded")
	}
	if got := readAll(t, s, "a/b/obj"); got != old {
		t.Fatalf("object after a failed rewrite has %d bytes, want the old %d", len(got), len(old))
	}
	_ = dir
	entries, _ := os.ReadDir(filepath.Dir(s.fullPath("a/b/obj")))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), UploadTempPrefix) {
			t.Errorf("temporary file left behind: %s", e.Name())
		}
	}
	// Copy goes through the same path.
	if _, err := s.Copy(ctx, "a/b/obj", "a/c/copy"); err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, s, "a/c/copy"); got != old {
		t.Errorf("copy has %d bytes", len(got))
	}
}

// TestLocalUploadReaderSeesWholeObject: while a rewrite is in progress, a
// download sees the whole old object; afterwards the whole new one.
func TestLocalUploadReaderSeesWholeObject(t *testing.T) {
	s, _ := newTestLocal(t)
	ctx := context.Background()
	old := strings.Repeat("A", 4096)
	fresh := strings.Repeat("B", 4096)
	if _, err := s.Upload(ctx, "obj", strings.NewReader(old), UploadOptions{}); err != nil {
		t.Fatal(err)
	}
	pr, pw := io.Pipe()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := s.Upload(ctx, "obj", pr, UploadOptions{}); err != nil {
			t.Errorf("rewrite: %v", err)
		}
	}()
	if _, err := pw.Write([]byte(fresh[:1000])); err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, s, "obj"); got != old {
		t.Errorf("download during a rewrite: %d bytes of mixed content", len(got))
	}
	if _, err := pw.Write([]byte(fresh[1000:])); err != nil {
		t.Fatal(err)
	}
	_ = pw.Close()
	wg.Wait()
	if got := readAll(t, s, "obj"); got != fresh {
		t.Errorf("after the rewrite: %d bytes", len(got))
	}
}

// TestLocalRemoveStaleTemps: temporary upload files are never listed;
// stale ones (a crash) are removed, fresh ones (an upload in progress) and
// regular files are kept.
func TestLocalRemoveStaleTemps(t *testing.T) {
	s, dir := newTestLocal(t)
	ctx := context.Background()
	if _, err := s.Upload(ctx, "p/x", strings.NewReader("x"), UploadOptions{}); err != nil {
		t.Fatal(err)
	}
	_ = dir
	stale := filepath.Join(filepath.Dir(s.fullPath("p/x")), UploadTempPrefix+"x-1")
	fresh := filepath.Join(filepath.Dir(s.fullPath("p/x")), UploadTempPrefix+"x-2")
	for _, f := range []string{stale, fresh} {
		if err := os.WriteFile(f, []byte("partial"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	res, err := s.List(ctx, ListOptions{Prefix: "p/"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Objects) != 1 || res.Objects[0].Name != "p/x" {
		t.Errorf("List = %+v, want only p/x", res.Objects)
	}
	n, err := s.RemoveStaleTemps(ctx, "p/", time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("RemoveStaleTemps: %d %v", n, err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale temporary file kept")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("fresh temporary file removed: %v", err)
	}
	if got := readAll(t, s, "p/x"); got != "x" {
		t.Errorf("regular file changed")
	}
}

// fakeGCS answers uploads with 429 for the first failFirst requests (all
// of them when failFirst < 0), then accepts them, and serves object
// metadata.
func fakeGCS(t *testing.T, failFirst int) (*httptest.Server, *int32) {
	t.Helper()
	var uploads int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/upload/") {
			n := atomic.AddInt32(&uploads, 1)
			_, _ = io.Copy(io.Discard, r.Body)
			if failFirst < 0 || int(n) <= failFirst {
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":{"code":429,"message":"rate limited"}}`))
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"bucket":"b","name":"obj","generation":"7","size":"3","contentType":"text/plain"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &uploads
}

func newFakeGCS(t *testing.T, srv *httptest.Server) *GCSStorage {
	t.Helper()
	s, err := newGCS(context.Background(), Config{Bucket: "b"},
		option.WithEndpoint(srv.URL+"/storage/v1/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestGCSIdempotentUploadIsOneAttempt: an idempotent upload makes one
// request per call, however the store answers (its caller bounds the
// retries), and returns the store's error.
func TestGCSIdempotentUploadIsOneAttempt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	srv, uploads := fakeGCS(t, -1)
	s := newFakeGCS(t, srv)
	if _, err := s.Upload(ctx, "obj", strings.NewReader("abc"), UploadOptions{Idempotent: true}); err == nil {
		t.Fatal("a rate-limited idempotent upload succeeded")
	}
	if n := atomic.LoadInt32(uploads); n != 1 {
		t.Errorf("%d requests for one idempotent upload, want 1", n)
	}
	srv, uploads = fakeGCS(t, 0)
	s = newFakeGCS(t, srv)
	if _, err := s.Upload(ctx, "obj", strings.NewReader("abc"), UploadOptions{Idempotent: true}); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if n := atomic.LoadInt32(uploads); n != 1 {
		t.Errorf("%d requests, want 1", n)
	}
}

// TestGCSUploadCopyErrorAborts: when reading the source fails part way,
// the upload is aborted rather than finalized with partial content.
func TestGCSUploadCopyErrorAborts(t *testing.T) {
	var completed int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/upload/") {
			if _, err := io.ReadAll(r.Body); err == nil {
				atomic.AddInt32(&completed, 1)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"bucket":"b","name":"obj","generation":"7","size":"3"}`))
	}))
	t.Cleanup(srv.Close)
	s := newFakeGCS(t, srv)
	big := strings.Repeat("x", 64<<10)
	r := io.MultiReader(strings.NewReader(big), &failingReader{sent: true})
	if _, err := s.Upload(context.Background(), "obj", r, UploadOptions{Idempotent: true}); err == nil {
		t.Fatal("upload with a failing source succeeded")
	}
	time.Sleep(100 * time.Millisecond)
	if n := atomic.LoadInt32(&completed); n != 0 {
		t.Errorf("%d uploads were finalized with partial content", n)
	}
}

// TestLocalUploadMode: an uploaded file gets the mode os.Create gives
// (0666 less the umask), not a temporary file's 0600.
func TestLocalUploadMode(t *testing.T) {
	s, _ := newTestLocal(t)
	if _, err := s.Upload(context.Background(), "m/obj", strings.NewReader("x"), UploadOptions{}); err != nil {
		t.Fatal(err)
	}
	ref := filepath.Join(filepath.Dir(s.fullPath("m/obj")), "reference")
	f, err := os.Create(ref)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	got, _ := os.Stat(s.fullPath("m/obj"))
	want, _ := os.Stat(ref)
	if got.Mode().Perm() != want.Mode().Perm() {
		t.Errorf("mode %v, want %v", got.Mode().Perm(), want.Mode().Perm())
	}
}

// TestLocalUploadSyncsDirectories: an upload syncs the directory it
// renames into, and the parents of directories it creates.
func TestLocalUploadSyncsDirectories(t *testing.T) {
	s, _ := newTestLocal(t)
	var synced []string
	syncDirHook = func(d string) { synced = append(synced, d) }
	t.Cleanup(func() { syncDirHook = nil })
	if _, err := s.Upload(context.Background(), "x/y/z/obj", strings.NewReader("x"), UploadOptions{}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(s.fullPath("x/y/z/obj"))
	want := map[string]bool{dir: false, filepath.Dir(dir): false, filepath.Dir(filepath.Dir(dir)): false}
	for _, d := range synced {
		if _, ok := want[d]; ok {
			want[d] = true
		}
	}
	for d, ok := range want {
		if !ok {
			t.Errorf("directory %s not synced (synced %v)", d, synced)
		}
	}
	if synced[len(synced)-1] != dir {
		t.Errorf("the last sync is not the rename's directory: %v", synced)
	}
}
