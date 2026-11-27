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

//go:build !no_sqlite

package artifacts

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/storage"
)

func TestUploadDeadlineScaling(t *testing.T) {
	cases := []struct {
		name string
		size int64
		want time.Duration
	}{
		{name: "negative", size: -10, want: uploadDeadlineFloor},
		{name: "zero", size: 0, want: uploadDeadlineFloor},
		{name: "small_1KiB", size: 1 << 10, want: uploadDeadlineFloor},
		{name: "32MiB", size: 32 << 20, want: uploadDeadlineFloor + 32*time.Second},
		{name: "1GiB", size: 1 << 30, want: uploadDeadlineFloor + 1024*time.Second},
		{name: "capped_10GiB", size: 10 << 30, want: uploadDeadlineMax},
		{name: "overflow_MaxInt64", size: math.MaxInt64, want: uploadDeadlineMax},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := UploadDeadline(tc.size); got != tc.want {
				t.Errorf("UploadDeadline(%d) = %v, want %v", tc.size, got, tc.want)
			}
		})
	}
}

func TestExpectedUploadBytes(t *testing.T) {
	const maxBytes int64 = 1 << 30
	cases := []struct {
		name          string
		contentLength int64
		maxBytes      int64
		want          int64
	}{
		{name: "known", contentLength: 64 << 20, maxBytes: maxBytes, want: 64 << 20},
		{name: "zero", contentLength: 0, maxBytes: maxBytes, want: 0},
		{name: "unknown_chunked", contentLength: -1, maxBytes: maxBytes, want: maxBytes},
		{name: "clamped_to_max", contentLength: 2 << 30, maxBytes: maxBytes, want: maxBytes},
		{name: "no_max_cap", contentLength: 64 << 20, maxBytes: 0, want: 64 << 20},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExpectedUploadBytes(tc.contentLength, tc.maxBytes); got != tc.want {
				t.Errorf("ExpectedUploadBytes(%d, %d) = %d, want %d", tc.contentLength, tc.maxBytes, got, tc.want)
			}
		})
	}
}

// rwDeadlineRecorder records SetReadDeadline and SetWriteDeadline calls made
// through http.ResponseController.
type rwDeadlineRecorder struct {
	*httptest.ResponseRecorder
	readDeadlines  []time.Time
	writeDeadlines []time.Time
}

func (r *rwDeadlineRecorder) SetReadDeadline(t time.Time) error {
	r.readDeadlines = append(r.readDeadlines, t)
	return nil
}

func (r *rwDeadlineRecorder) SetWriteDeadline(t time.Time) error {
	r.writeDeadlines = append(r.writeDeadlines, t)
	return nil
}

func TestExtendUploadDeadlines_RespectsServerTimeouts(t *testing.T) {
	t0 := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	prevNow := uploadDeadlineNow
	uploadDeadlineNow = func() time.Time { return t0 }
	t.Cleanup(func() { uploadDeadlineNow = prevNow })

	const size = 32 << 20
	wantRead := UploadDeadline(size)
	wantWrite := 2*wantRead + publishDeadlineMargin

	t.Run("default_server_timeouts_extended", func(t *testing.T) {
		rec := &rwDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
		ctx := context.WithValue(context.Background(), http.ServerContextKey, &http.Server{
			ReadTimeout:  30 * time.Second,
			WriteTimeout: 60 * time.Second,
		})
		ExtendUploadDeadlines(ctx, rec, size)
		if len(rec.readDeadlines) != 1 || !rec.readDeadlines[0].Equal(t0.Add(wantRead)) {
			t.Errorf("readDeadlines = %v, want [%v]", rec.readDeadlines, t0.Add(wantRead))
		}
		if len(rec.writeDeadlines) != 1 || !rec.writeDeadlines[0].Equal(t0.Add(wantWrite)) {
			t.Errorf("writeDeadlines = %v, want [%v]", rec.writeDeadlines, t0.Add(wantWrite))
		}
	})

	t.Run("unbounded_server_timeouts_not_shortened", func(t *testing.T) {
		rec := &rwDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
		ctx := context.WithValue(context.Background(), http.ServerContextKey, &http.Server{
			ReadTimeout:  0,
			WriteTimeout: 0,
		})
		ExtendUploadDeadlines(ctx, rec, size)
		if len(rec.readDeadlines) != 0 || len(rec.writeDeadlines) != 0 {
			t.Errorf("expected no deadline calls for unbounded server, got read=%v write=%v",
				rec.readDeadlines, rec.writeDeadlines)
		}
	})

	t.Run("longer_server_timeouts_not_shortened", func(t *testing.T) {
		rec := &rwDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
		ctx := context.WithValue(context.Background(), http.ServerContextKey, &http.Server{
			ReadTimeout:  2 * time.Hour,
			WriteTimeout: 2 * time.Hour,
		})
		ExtendUploadDeadlines(ctx, rec, size)
		if len(rec.readDeadlines) != 0 || len(rec.writeDeadlines) != 0 {
			t.Errorf("expected no deadline calls when server timeouts exceed budget, got read=%v write=%v",
				rec.readDeadlines, rec.writeDeadlines)
		}
	})

	t.Run("unsupported_response_writer_ignored", func(t *testing.T) {
		rec := httptest.NewRecorder()
		ExtendUploadDeadlines(context.Background(), rec, size)
	})
}

// slowUploadStorage wraps a storage.Storage and pauses inside Upload so tests
// can verify write-deadline extension past a short http.Server.WriteTimeout.
type slowUploadStorage struct {
	storage.Storage
	delay time.Duration
}

func (s *slowUploadStorage) Upload(ctx context.Context, objectPath string, reader io.Reader, opts storage.UploadOptions) (*storage.Object, error) {
	timer := time.NewTimer(s.delay)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return s.Storage.Upload(ctx, objectPath, reader, opts)
}

// slowReader drips data in two chunks separated by delay so reading the body
// outlasts a short http.Server.ReadTimeout.
type slowReader struct {
	data  []byte
	pos   int
	delay time.Duration
	slept bool
}

func (r *slowReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	if r.pos == 0 {
		// Emit 1 byte first so the server receives headers + start of body.
		p[0] = r.data[0]
		r.pos = 1
		return 1, nil
	}
	if !r.slept {
		r.slept = true
		time.Sleep(r.delay)
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

func TestPublishAndPutFile_ExtendDeadlinesThroughRealServer(t *testing.T) {
	const (
		serverTimeout = 200 * time.Millisecond
		phaseDelay    = 300 * time.Millisecond
	)

	f := newFixture(t, false)
	f.svc.SetBlobStorage(&slowUploadStorage{Storage: f.local, delay: phaseDelay}, "hub-1")

	hs := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.svc.ServeHTTP(w, withPrincipal(r, agentA))
	}))
	hs.Config.ReadTimeout = serverTimeout
	hs.Config.WriteTimeout = serverTimeout
	hs.Start()
	t.Cleanup(hs.Close)

	payload := []byte("slow-upload-payload-bytes")

	t.Run("single_file_publish_known_length", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodPost, hs.URL+"/api/v1/artifacts?name=slow.bin", &slowReader{data: payload, delay: phaseDelay})
		if err != nil {
			t.Fatal(err)
		}
		req.ContentLength = int64(len(payload))
		req.Header.Set("Content-Type", "application/octet-stream")

		start := time.Now()
		resp, err := hs.Client().Do(req)
		if err != nil {
			t.Fatalf("publish failed (deadline not extended): %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusCreated {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d, want 201: %s", resp.StatusCode, body)
		}
		if elapsed := time.Since(start); elapsed < 2*phaseDelay {
			t.Errorf("elapsed = %v, want >= %v (both read and write phases must exceed server timeout)", elapsed, 2*phaseDelay)
		}
	})

	t.Run("single_file_publish_chunked_unknown_length", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodPost, hs.URL+"/api/v1/artifacts?name=chunked.bin", &slowReader{data: payload, delay: phaseDelay})
		if err != nil {
			t.Fatal(err)
		}
		req.ContentLength = -1
		req.Header.Set("Content-Type", "application/octet-stream")

		start := time.Now()
		resp, err := hs.Client().Do(req)
		if err != nil {
			t.Fatalf("chunked publish failed (deadline not extended): %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusCreated {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d, want 201: %s", resp.StatusCode, body)
		}
		if elapsed := time.Since(start); elapsed < 2*phaseDelay {
			t.Errorf("elapsed = %v, want >= %v", elapsed, 2*phaseDelay)
		}
	})

	t.Run("put_file_version_upload", func(t *testing.T) {
		// Create a pending version first.
		manifest := CreateVersionRequest{
			Title: "slow version",
			Entry: "blob.bin",
			Files: []ManifestFile{{Path: "blob.bin", Size: int64(len(payload)), SHA256: sha(payload)}},
		}
		raw, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		createResp, err := hs.Client().Post(hs.URL+"/api/v1/artifacts", "application/json", bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = createResp.Body.Close() }()
		if createResp.StatusCode != http.StatusCreated {
			b, _ := io.ReadAll(createResp.Body)
			t.Fatalf("create pending status = %d: %s", createResp.StatusCode, b)
		}
		var pending PendingVersionResponse
		if err := json.NewDecoder(createResp.Body).Decode(&pending); err != nil {
			t.Fatal(err)
		}

		putURL := hs.URL + "/api/v1/artifacts/" + pending.Artifact.ID + "/versions/1/files/blob.bin"
		req, err := http.NewRequest(http.MethodPut, putURL, &slowReader{data: payload, delay: phaseDelay})
		if err != nil {
			t.Fatal(err)
		}
		req.ContentLength = int64(len(payload))
		req.Header.Set("Content-Type", "application/octet-stream")

		start := time.Now()
		resp, err := hs.Client().Do(req)
		if err != nil {
			t.Fatalf("put file failed (deadline not extended): %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusNoContent {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d, want 204: %s", resp.StatusCode, body)
		}
		if elapsed := time.Since(start); elapsed < 2*phaseDelay {
			t.Errorf("elapsed = %v, want >= %v", elapsed, 2*phaseDelay)
		}
	})
}
