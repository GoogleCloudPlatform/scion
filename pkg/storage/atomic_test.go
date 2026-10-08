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

	gax "github.com/googleapis/gax-go/v2"
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
	old, oldDeadline := gcsIdempotentBackoff, gcsIdempotentRetryDeadline
	gcsIdempotentBackoff = gax.Backoff{Initial: time.Millisecond, Max: 2 * time.Millisecond, Multiplier: 1}
	gcsIdempotentRetryDeadline = 300 * time.Millisecond
	t.Cleanup(func() { gcsIdempotentBackoff, gcsIdempotentRetryDeadline = old, oldDeadline })
	s, err := newGCS(context.Background(), Config{Bucket: "b"},
		option.WithEndpoint(srv.URL+"/storage/v1/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestGCSIdempotentUploadRetries: an idempotent upload is retried through
// rate limiting, a bounded number of times, and its last error is
// returned.
func TestGCSIdempotentUploadRetries(t *testing.T) {
	ctx := context.Background()
	srv, uploads := fakeGCS(t, 2)
	s := newFakeGCS(t, srv)
	if _, err := s.Upload(ctx, "obj", strings.NewReader("abc"), UploadOptions{Idempotent: true}); err != nil {
		t.Fatalf("idempotent upload through two 429s: %v", err)
	}
	if n := atomic.LoadInt32(uploads); n != 3 {
		t.Errorf("%d upload requests, want 3", n)
	}

	srv, uploads = fakeGCS(t, -1)
	s = newFakeGCS(t, srv)
	start := time.Now()
	if _, err := s.Upload(ctx, "obj", strings.NewReader("abc"), UploadOptions{Idempotent: true}); err == nil {
		t.Errorf("an always-limited idempotent upload succeeded")
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("the bounded retry took %v", took)
	}
	if n := atomic.LoadInt32(uploads); n < 2 {
		t.Errorf("always limited: %d requests, want retries", n)
	}
}
