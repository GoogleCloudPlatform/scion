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
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGCSDownload serves object metadata on the JSON API path and object
// content on the XML read path, and records the generation each read asks
// for. Metadata always reports generation 7 and attrsSize; content is body,
// served with extra headers. A non-zero attrsStatus or readStatus answers
// that path with the status instead.
type fakeGCSDownload struct {
	attrsSize   string
	attrsStatus int
	body        string
	readHeaders map[string]string
	readStatus  int

	mu       sync.Mutex
	readGens []string
}

func (f *fakeGCSDownload) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/storage/v1/") {
			if f.attrsStatus != 0 {
				w.WriteHeader(f.attrsStatus)
				_, _ = w.Write([]byte(`{"error":{"code":404,"message":"not found"}}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"bucket":"b","name":"obj","generation":"7","size":"` + f.attrsSize + `"}`))
			return
		}
		f.mu.Lock()
		f.readGens = append(f.readGens, r.URL.Query().Get("generation"))
		f.mu.Unlock()
		if f.readStatus != 0 {
			w.WriteHeader(f.readStatus)
			return
		}
		for k, v := range f.readHeaders {
			w.Header().Set(k, v)
		}
		w.Header().Set("X-Goog-Generation", "7")
		_, _ = w.Write([]byte(f.body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeGCSDownload) gens() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.readGens...)
}

// TestGCSDownloadSizeFromReader: Download reports the size of the bytes the
// reader yields (not the metadata's stored size), and reads the generation
// the metadata describes.
func TestGCSDownloadSizeFromReader(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	f := &fakeGCSDownload{attrsSize: "99", body: "hello"}
	s := newFakeGCS(t, f.serve(t))

	rc, obj, err := s.Download(ctx, "obj")
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "hello" {
		t.Errorf("body = %q, want %q", data, "hello")
	}
	if obj.Size != int64(len(data)) {
		t.Errorf("Size = %d, want %d (the bytes read, not the stored size 99)", obj.Size, len(data))
	}
	if g := f.gens(); len(g) == 0 || g[0] != "7" {
		t.Errorf("read generations = %q, want the first read pinned to generation 7", g)
	}
}

// TestGCSDownloadSizeUnknownWhenDecompressed: when GCS decompresses a
// gzip-encoded object on read, the stored size is the compressed size, so
// Download reports the size as unknown (-1) and the bytes are unchanged.
func TestGCSDownloadSizeUnknownWhenDecompressed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	const plain = "decompressed content, longer than the stored size"
	f := &fakeGCSDownload{
		attrsSize:   "12",
		body:        plain,
		readHeaders: map[string]string{"X-Goog-Stored-Content-Encoding": "gzip"},
	}
	s := newFakeGCS(t, f.serve(t))

	rc, obj, err := s.Download(ctx, "obj")
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != plain {
		t.Errorf("body = %q, want %q", data, plain)
	}
	if obj.Size != -1 {
		t.Errorf("Size = %d, want -1 (unknown) for a decompressed read", obj.Size)
	}
}

// TestGCSDownloadNotFound: a missing object, or a pinned generation that no
// longer exists by the time it is read, maps to ErrNotFound.
func TestGCSDownloadNotFound(t *testing.T) {
	for name, f := range map[string]*fakeGCSDownload{
		"metadata missing":   {attrsStatus: http.StatusNotFound},
		"generation removed": {attrsSize: "5", readStatus: http.StatusNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			s := newFakeGCS(t, f.serve(t))
			rc, _, err := s.Download(ctx, "obj")
			if rc != nil {
				_ = rc.Close()
			}
			if !errors.Is(err, ErrNotFound) {
				t.Errorf("err = %v, want ErrNotFound", err)
			}
		})
	}
}
