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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLocalStorage_Compose(t *testing.T) {
	ctx := context.Background()
	ls, err := NewLocal(Config{
		Provider:  ProviderLocal,
		Bucket:    "test-bucket",
		LocalPath: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}

	parts := []string{"uploads-staging/h1/s1/p00000-t1", "uploads-staging/h1/s1/p00001-t2", "uploads-staging/h1/s1/p00002-t3"}
	chunks := [][]byte{[]byte("alpha-"), []byte("beta-"), []byte("gamma!")}
	var want bytes.Buffer
	for i, p := range parts {
		if _, err := ls.Upload(ctx, p, bytes.NewReader(chunks[i]), UploadOptions{Idempotent: true}); err != nil {
			t.Fatalf("Upload %s: %v", p, err)
		}
		want.Write(chunks[i])
	}

	dst := "blobs/ab/cd/composed.bin"
	obj, err := ls.Compose(ctx, dst, parts, UploadOptions{ContentType: "application/octet-stream"})
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	wantSum := sha256.Sum256(want.Bytes())
	wantHex := hex.EncodeToString(wantSum[:])
	if obj.Name != dst {
		t.Errorf("Name = %q, want %q", obj.Name, dst)
	}
	if obj.Size != int64(want.Len()) {
		t.Errorf("Size = %d, want %d", obj.Size, want.Len())
	}
	if obj.ETag != wantHex {
		t.Errorf("ETag = %q, want %q", obj.ETag, wantHex)
	}
	if obj.ContentType != "application/octet-stream" {
		t.Errorf("ContentType = %q, want application/octet-stream", obj.ContentType)
	}

	rc, _, err := ls.Download(ctx, dst)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, want.Bytes()) {
		t.Errorf("composed bytes = %q, want %q", got, want.Bytes())
	}

	// Overwriting an existing destination replaces its bytes cleanly.
	obj2, err := ls.Compose(ctx, dst, []string{parts[2]}, UploadOptions{ContentType: "text/plain"})
	if err != nil {
		t.Fatalf("Compose overwrite: %v", err)
	}
	if obj2.Size != int64(len(chunks[2])) {
		t.Errorf("overwrite Size = %d, want %d", obj2.Size, len(chunks[2]))
	}
	rc2, _, err := ls.Download(ctx, dst)
	if err != nil {
		t.Fatalf("Download overwrite: %v", err)
	}
	got2, _ := io.ReadAll(rc2)
	_ = rc2.Close()
	if !bytes.Equal(got2, chunks[2]) {
		t.Errorf("overwrite bytes = %q, want %q", got2, chunks[2])
	}

	// Missing source returns ErrNotFound.
	if _, err := ls.Compose(ctx, "blobs/missing", []string{"does/not/exist"}, UploadOptions{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing src err = %v, want ErrNotFound", err)
	}

	// Empty dst or srcs returns ErrInvalidPath.
	if _, err := ls.Compose(ctx, "", parts, UploadOptions{}); !errors.Is(err, ErrInvalidPath) {
		t.Errorf("empty dst err = %v, want ErrInvalidPath", err)
	}
	if _, err := ls.Compose(ctx, dst, nil, UploadOptions{}); !errors.Is(err, ErrInvalidPath) {
		t.Errorf("empty srcs err = %v, want ErrInvalidPath", err)
	}
}

// fakeGCSCompose is an in-memory GCS JSON API fake that strictly enforces the
// 32-source limit on Compose calls and tracks every Compose and Delete call.
type fakeGCSCompose struct {
	mu           sync.Mutex
	objects      map[string]fakeComposeGCSObject
	composeCalls int
	maxBatchSeen int
	deleted      []string
	failOnCall   int // if > 0, fail the Nth compose call with 500
}

type fakeComposeGCSObject struct {
	data        []byte
	contentType string
	generation  int64
}

func newFakeGCSCompose() *fakeGCSCompose {
	return &fakeGCSCompose{
		objects: make(map[string]fakeComposeGCSObject),
	}
}

func (f *fakeGCSCompose) put(name string, data []byte, contentType string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	gen := f.objects[name].generation + 1
	f.objects[name] = fakeComposeGCSObject{
		data:        append([]byte(nil), data...),
		contentType: contentType,
		generation:  gen,
	}
}

func (f *fakeGCSCompose) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Compose: POST /storage/v1/b/{bucket}/o/{dst}/compose
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/compose") {
			rawPath := r.URL.RawPath
			if rawPath == "" {
				rawPath = r.URL.Path
			}
			const prefix = "/storage/v1/b/b/o/"
			trimmed := strings.TrimPrefix(rawPath, prefix)
			trimmed = strings.TrimSuffix(trimmed, "/compose")
			dst, err := url.PathUnescape(trimmed)
			if err != nil {
				http.Error(w, "bad path", http.StatusBadRequest)
				return
			}

			var req struct {
				SourceObjects []struct {
					Name string `json:"name"`
				} `json:"sourceObjects"`
				Destination struct {
					ContentType string            `json:"contentType"`
					Metadata    map[string]string `json:"metadata"`
				} `json:"destination"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "bad json", http.StatusBadRequest)
				return
			}
			if len(req.SourceObjects) == 0 || len(req.SourceObjects) > gcsMaxComposeSources {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = fmt.Fprintf(w, `{"error":{"code":400,"message":"too many source objects: %d (max %d)"}}`,
					len(req.SourceObjects), gcsMaxComposeSources)
				return
			}

			f.mu.Lock()
			f.composeCalls++
			callNum := f.composeCalls
			if len(req.SourceObjects) > f.maxBatchSeen {
				f.maxBatchSeen = len(req.SourceObjects)
			}
			if f.failOnCall > 0 && callNum == f.failOnCall {
				f.mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":{"code":500,"message":"injected compose failure"}}`))
				return
			}

			var combined bytes.Buffer
			for _, src := range req.SourceObjects {
				obj, ok := f.objects[src.Name]
				if !ok {
					f.mu.Unlock()
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusNotFound)
					_, _ = fmt.Fprintf(w, `{"error":{"code":404,"message":"source %q not found"}}`, src.Name)
					return
				}
				combined.Write(obj.data)
			}
			gen := f.objects[dst].generation + 1
			f.objects[dst] = fakeComposeGCSObject{
				data:        combined.Bytes(),
				contentType: req.Destination.ContentType,
				generation:  gen,
			}
			f.mu.Unlock()

			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"bucket":      "b",
				"name":        dst,
				"size":        strconv.Itoa(combined.Len()),
				"contentType": req.Destination.ContentType,
				"generation":  strconv.FormatInt(gen, 10),
			})
			return
		}

		// Delete: DELETE /storage/v1/b/{bucket}/o/{obj}
		if r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/storage/v1/b/b/o/") {
			rawPath := r.URL.RawPath
			if rawPath == "" {
				rawPath = r.URL.Path
			}
			name, err := url.PathUnescape(strings.TrimPrefix(rawPath, "/storage/v1/b/b/o/"))
			if err != nil {
				http.Error(w, "bad path", http.StatusBadRequest)
				return
			}
			f.mu.Lock()
			if _, ok := f.objects[name]; !ok {
				f.mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":{"code":404,"message":"not found"}}`))
				return
			}
			delete(f.objects, name)
			f.deleted = append(f.deleted, name)
			f.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}

		http.Error(w, "unhandled route: "+r.Method+" "+r.URL.Path, http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestGCSStorage_ComposeSingleAndSmallBatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	fake := newFakeGCSCompose()
	fake.put("uploads-staging/h1/s1/p00000-a", []byte("hello "), "application/octet-stream")
	fake.put("uploads-staging/h1/s1/p00001-b", []byte("world"), "application/octet-stream")
	s := newFakeGCS(t, fake.serve(t))

	obj, err := s.Compose(ctx, "blobs/final", []string{
		"uploads-staging/h1/s1/p00000-a",
		"uploads-staging/h1/s1/p00001-b",
	}, UploadOptions{ContentType: "text/plain"})
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if obj.Size != 11 {
		t.Errorf("Size = %d, want 11", obj.Size)
	}
	if obj.ContentType != "text/plain" {
		t.Errorf("ContentType = %q, want text/plain", obj.ContentType)
	}
	if got := string(fake.objects["blobs/final"].data); got != "hello world" {
		t.Errorf("composed bytes = %q, want %q", got, "hello world")
	}
	if fake.composeCalls != 1 {
		t.Errorf("composeCalls = %d, want 1", fake.composeCalls)
	}
	if len(fake.deleted) != 0 {
		t.Errorf("deleted = %v, want none for <=32 sources", fake.deleted)
	}
}

func TestGCSStorage_ComposeTreeOver32Parts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 70 parts => level 0: 32 + 32 + 6 (3 intermediates), level 1: 3 -> dst (4 compose calls total).
	const numParts = 70
	fake := newFakeGCSCompose()
	srcs := make([]string, numParts)
	var want bytes.Buffer
	for i := 0; i < numParts; i++ {
		name := fmt.Sprintf("uploads-staging/h1/s1/p%05d-tok%d", i, i)
		chunk := []byte(fmt.Sprintf("[%03d]", i))
		fake.put(name, chunk, "application/octet-stream")
		srcs[i] = name
		want.Write(chunk)
	}

	s := newFakeGCS(t, fake.serve(t))
	obj, err := s.Compose(ctx, "blobs/hub/digest", srcs, UploadOptions{ContentType: "application/pdf"})
	if err != nil {
		t.Fatalf("Compose(70 parts): %v", err)
	}

	if obj.Size != int64(want.Len()) {
		t.Errorf("Size = %d, want %d", obj.Size, want.Len())
	}
	if obj.ContentType != "application/pdf" {
		t.Errorf("ContentType = %q, want application/pdf", obj.ContentType)
	}
	if fake.maxBatchSeen > gcsMaxComposeSources {
		t.Errorf("maxBatchSeen = %d, exceeds GCS limit %d", fake.maxBatchSeen, gcsMaxComposeSources)
	}
	if fake.composeCalls != 4 {
		t.Errorf("composeCalls = %d, want 4 (3 intermediates + 1 final)", fake.composeCalls)
	}
	if !bytes.Equal(fake.objects["blobs/hub/digest"].data, want.Bytes()) {
		t.Errorf("composed payload mismatch (len got=%d want=%d)", len(fake.objects["blobs/hub/digest"].data), want.Len())
	}

	// All 3 intermediate objects must have been deleted; original parts and dst remain.
	if len(fake.deleted) != 3 {
		t.Errorf("deleted intermediates = %d (%v), want 3", len(fake.deleted), fake.deleted)
	}
	for name := range fake.objects {
		if strings.Contains(name, ".compose-") {
			t.Errorf("leaked intermediate object: %s", name)
		}
	}
}

func TestGCSStorage_ComposeTreeDeletesIntermediatesOnError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// 65 parts => batch 0 (32), batch 1 (32), batch 2 (1, carried forward).
	// Fail on compose call 2; intermediate from call 1 must still be deleted.
	fake := newFakeGCSCompose()
	fake.failOnCall = 2
	srcs := make([]string, 65)
	for i := range srcs {
		name := fmt.Sprintf("uploads-staging/h1/s1/p%05d-tok", i)
		fake.put(name, []byte("x"), "application/octet-stream")
		srcs[i] = name
	}
	s := newFakeGCS(t, fake.serve(t))
	if _, err := s.Compose(ctx, "blobs/fail", srcs, UploadOptions{}); err == nil {
		t.Fatal("expected error from injected compose failure, got nil")
	}
	for name := range fake.objects {
		if strings.Contains(name, ".compose-") {
			t.Errorf("leaked intermediate object after failed compose: %s", name)
		}
	}
}

func TestGCSStorage_ComposeMissingSourceAndInvalidArgs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	fake := newFakeGCSCompose()
	s := newFakeGCS(t, fake.serve(t))

	if _, err := s.Compose(ctx, "blobs/dst", []string{"missing/part"}, UploadOptions{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing src err = %v, want ErrNotFound", err)
	}
	if _, err := s.Compose(ctx, "", []string{"a"}, UploadOptions{}); !errors.Is(err, ErrInvalidPath) {
		t.Errorf("empty dst err = %v, want ErrInvalidPath", err)
	}
	if _, err := s.Compose(ctx, "blobs/dst", nil, UploadOptions{}); !errors.Is(err, ErrInvalidPath) {
		t.Errorf("empty srcs err = %v, want ErrInvalidPath", err)
	}
}
