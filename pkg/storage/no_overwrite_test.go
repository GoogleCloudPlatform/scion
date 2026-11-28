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
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestGCSWithFakeRSAKey(t *testing.T, bucket string) *GCSStorage {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal PKCS8 key: %v", err)
	}
	pemKey := pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: pkcs8,
	})
	saJSON, err := json.Marshal(map[string]string{
		"type":         "service_account",
		"project_id":   "test-project",
		"client_email": "test-signer@test-project.iam.gserviceaccount.com",
		"private_key":  string(pemKey),
		"token_uri":    "https://oauth2.googleapis.com/token",
	})
	if err != nil {
		t.Fatalf("marshal fake service account json: %v", err)
	}
	gcs, err := NewGCS(context.Background(), Config{
		Provider: ProviderGCS,
		Bucket:   bucket,
		Credentials: &Credentials{
			ServiceAccountJSON: string(saJSON),
		},
	})
	if err != nil {
		t.Fatalf("NewGCS: %v", err)
	}
	t.Cleanup(func() { _ = gcs.Close() })
	return gcs
}

func TestGCSGenerateSignedURL_PutNoOverwriteHeaders(t *testing.T) {
	gcs := newTestGCSWithFakeRSAKey(t, "audit-bucket")
	zero := int64(0)

	signed, err := gcs.GenerateSignedURL(context.Background(), "hub-1/trajectories/v1/raw/g0-0000000000000000.seg.gz", SignedURLOptions{
		Method:             http.MethodPut,
		Expires:            10 * time.Minute,
		ContentType:        "application/gzip",
		ContentLengthRange: &ContentLengthRange{Min: 128, Max: 128},
		IfGenerationMatch:  &zero,
	})
	if err != nil {
		t.Fatalf("GenerateSignedURL: %v", err)
	}
	if signed.Method != http.MethodPut {
		t.Errorf("Method = %q, want PUT", signed.Method)
	}
	if got := signed.Headers["x-goog-if-generation-match"]; got != "0" {
		t.Errorf("Headers[x-goog-if-generation-match] = %q, want %q", got, "0")
	}
	if got := signed.Headers["x-goog-content-length-range"]; got != "128,128" {
		t.Errorf("Headers[x-goog-content-length-range] = %q, want %q", got, "128,128")
	}
	if got := signed.Headers["Content-Type"]; got != "application/gzip" {
		t.Errorf("Headers[Content-Type] = %q, want %q", got, "application/gzip")
	}

	u, err := url.Parse(signed.URL)
	if err != nil {
		t.Fatalf("parse signed URL: %v", err)
	}
	signedHeaders := u.Query().Get("X-Goog-SignedHeaders")
	for _, want := range []string{"content-type", "host", "x-goog-content-length-range", "x-goog-if-generation-match"} {
		if !strings.Contains(signedHeaders, want) {
			t.Errorf("X-Goog-SignedHeaders = %q, missing %q", signedHeaders, want)
		}
	}
	if u.Query().Get("X-Goog-Algorithm") != "GOOG4-RSA-SHA256" {
		t.Errorf("X-Goog-Algorithm = %q, want GOOG4-RSA-SHA256", u.Query().Get("X-Goog-Algorithm"))
	}
}

func TestGCSGenerateSignedURL_NegativeIfGenerationMatchRejected(t *testing.T) {
	gcs := newTestGCSWithFakeRSAKey(t, "audit-bucket")
	neg := int64(-1)
	_, err := gcs.GenerateSignedURL(context.Background(), "obj", SignedURLOptions{
		Method:            http.MethodPut,
		IfGenerationMatch: &neg,
	})
	if err == nil {
		t.Fatalf("expected error for negative IfGenerationMatch, got nil")
	}
}

func TestLocalStorage_UploadIfGenerationMatchZero_NoOverwrite(t *testing.T) {
	dir := t.TempDir()
	loc, err := NewLocal(Config{
		Provider:  ProviderLocal,
		Bucket:    "local",
		LocalPath: dir,
	})
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}

	ctx := context.Background()
	zero := int64(0)
	const objPath = "hub-1/trajectories/v1/raw/g0-0000000000000000.seg.gz"
	original := []byte("original-immutable-bytes")

	obj, err := loc.Upload(ctx, objPath, bytes.NewReader(original), UploadOptions{
		ContentType:       "application/gzip",
		IfGenerationMatch: &zero,
	})
	if err != nil {
		t.Fatalf("initial Upload(IfGenerationMatch=0): %v", err)
	}
	if obj.Size != int64(len(original)) {
		t.Errorf("Size = %d, want %d", obj.Size, len(original))
	}

	// Second upload to the same path must fail with ErrPreconditionFailed and
	// leave the original bytes untouched.
	_, err = loc.Upload(ctx, objPath, bytes.NewReader([]byte("mutated-bytes")), UploadOptions{
		ContentType:       "application/gzip",
		IfGenerationMatch: &zero,
	})
	if !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("second Upload err = %v, want ErrPreconditionFailed", err)
	}

	rc, _, err := loc.Download(ctx, objPath)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	defer func() { _ = rc.Close() }()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, original) {
		t.Errorf("stored bytes = %q, want %q (must never be overwritten)", got, original)
	}
}

type repeatZeroReader struct{}

func (repeatZeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// TestGCSUpload_PreconditionFailed_SmallAndOver16MiB verifies that a GCS HTTP
// 412 Precondition Failed response maps to ErrPreconditionFailed for both a
// small single-shot body and a body exceeding the 16 MiB default resumable
// chunk size (where the 412 surfaces on the io.Copy path).
func TestGCSUpload_PreconditionFailed_SmallAndOver16MiB(t *testing.T) {
	for _, tc := range []struct {
		name string
		size int64
	}{
		{name: "small body (3 bytes)", size: 3},
		{name: "large body (>16 MiB)", size: 17 << 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var (
				mu             sync.Mutex
				sawIfGenMatch0 bool
			)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("ifGenerationMatch") == "0" {
					mu.Lock()
					sawIfGenMatch0 = true
					mu.Unlock()
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusPreconditionFailed)
				_, _ = w.Write([]byte(`{"error":{"code":412,"message":"At least one of the pre-conditions you specified did not hold."}}`))
			}))
			t.Cleanup(srv.Close)

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			gcs := newFakeGCS(t, srv)
			zero := int64(0)
			_, err := gcs.Upload(ctx, "hub-1/trajectories/v1/raw/g0-0000000000000000.seg.gz",
				io.LimitReader(repeatZeroReader{}, tc.size),
				UploadOptions{
					ContentType:       "application/gzip",
					IfGenerationMatch: &zero,
				})
			if !errors.Is(err, ErrPreconditionFailed) {
				t.Fatalf("Upload(size=%d) err = %v, want ErrPreconditionFailed", tc.size, err)
			}
			mu.Lock()
			gotPrecond := sawIfGenMatch0
			mu.Unlock()
			if !gotPrecond {
				t.Errorf("fake GCS did not receive ifGenerationMatch=0 query parameter")
			}
		})
	}
}

type fakeGCSObject struct {
	name        string
	generation  int64
	size        int64
	contentType string
	etag        string
	created     time.Time
	updated     time.Time
	metadata    map[string]string
}

type fakeGCSResumableSession struct {
	name        string
	contentType string
	metadata    map[string]string
	ifGenMatch  string
	written     int64
}

// fakeStatefulGCS mimics GCS's precondition semantics across multipart uploads,
// resumable uploads (>16 MiB), and object metadata GETs. Crucially, like real
// GCS, it rejects a conditioned metadata GET (ifGenerationMatch=0 on an
// existing object, or ifGenerationMatch=G on a different generation) with 412.
type fakeStatefulGCS struct {
	mu           sync.Mutex
	objects      map[string]*fakeGCSObject
	sessions     map[string]*fakeGCSResumableSession
	nextGen      int64
	nextSession  int
	metadataGETs int
	now          time.Time
	srv          *httptest.Server
}

func newFakeStatefulGCS(t *testing.T) *fakeStatefulGCS {
	t.Helper()
	f := &fakeStatefulGCS{
		objects:  make(map[string]*fakeGCSObject),
		sessions: make(map[string]*fakeGCSResumableSession),
		now:      time.Date(2026, 11, 28, 16, 0, 0, 0, time.UTC),
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serveHTTP))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeStatefulGCS) checkPreconditionLocked(name, ifGenMatch string) bool {
	if ifGenMatch == "" {
		return true
	}
	want, err := strconv.ParseInt(ifGenMatch, 10, 64)
	if err != nil {
		return false
	}
	existing := f.objects[name]
	if want == 0 {
		return existing == nil
	}
	return existing != nil && existing.generation == want
}

func writeGCS412(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusPreconditionFailed)
	_, _ = w.Write([]byte(`{"error":{"code":412,"message":"At least one of the pre-conditions you specified did not hold."}}`))
}

func writeGCSObjectJSON(w http.ResponseWriter, obj *fakeGCSObject) {
	resp := map[string]any{
		"bucket":      "b",
		"name":        obj.name,
		"generation":  strconv.FormatInt(obj.generation, 10),
		"size":        strconv.FormatInt(obj.size, 10),
		"contentType": obj.contentType,
		"etag":        obj.etag,
		"timeCreated": obj.created.Format(time.RFC3339Nano),
		"updated":     obj.updated.Format(time.RFC3339Nano),
	}
	if len(obj.metadata) > 0 {
		resp["metadata"] = obj.metadata
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (f *fakeStatefulGCS) commitObjectLocked(name, contentType string, metadata map[string]string, size int64) *fakeGCSObject {
	f.nextGen++
	ts := f.now.Add(time.Duration(f.nextGen) * time.Second)
	metaCopy := make(map[string]string, len(metadata))
	for k, v := range metadata {
		metaCopy[k] = v
	}
	obj := &fakeGCSObject{
		name:        name,
		generation:  f.nextGen,
		size:        size,
		contentType: contentType,
		etag:        fmt.Sprintf("etag-gen-%d", f.nextGen),
		created:     ts,
		updated:     ts,
		metadata:    metaCopy,
	}
	f.objects[name] = obj
	return obj
}

func (f *fakeStatefulGCS) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.Contains(r.URL.Path, "/upload/") {
		f.handleUpload(w, r)
		return
	}
	// Object metadata GET: /storage/v1/b/<bucket>/o/<object>
	const marker = "/o/"
	idx := strings.Index(r.URL.Path, marker)
	if r.Method == http.MethodGet && idx >= 0 {
		rawName := r.URL.Path[idx+len(marker):]
		name, err := url.PathUnescape(rawName)
		if err != nil {
			name = rawName
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.metadataGETs++
		if !f.checkPreconditionLocked(name, r.URL.Query().Get("ifGenerationMatch")) {
			writeGCS412(w)
			return
		}
		obj := f.objects[name]
		if obj == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":404,"message":"No such object"}}`))
			return
		}
		writeGCSObjectJSON(w, obj)
		return
	}
	http.Error(w, "unexpected request: "+r.Method+" "+r.URL.String(), http.StatusBadRequest)
}

func (f *fakeStatefulGCS) handleUpload(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	uploadType := q.Get("uploadType")

	if uploadType == "resumable" {
		if uploadID := q.Get("upload_id"); uploadID != "" {
			// Resumable chunk PUT
			n, _ := io.Copy(io.Discard, r.Body)
			f.mu.Lock()
			defer f.mu.Unlock()
			sess := f.sessions[uploadID]
			if sess == nil {
				http.Error(w, "unknown upload_id", http.StatusNotFound)
				return
			}
			sess.written += n
			cr := r.Header.Get("Content-Range")
			// Non-final chunk has "bytes start-end/*".
			if strings.HasSuffix(cr, "/*") {
				if sess.written > 0 {
					w.Header().Set("Range", fmt.Sprintf("bytes=0-%d", sess.written-1))
				}
				// cloud.google.com/go/storage sends X-GUploader-No-308: yes and
				// expects HTTP 200 with X-Http-Status-Code-Override: 308.
				if r.Header.Get("X-GUploader-No-308") == "yes" {
					w.Header().Set("X-Http-Status-Code-Override", "308")
					w.WriteHeader(http.StatusOK)
				} else {
					w.WriteHeader(http.StatusPermanentRedirect)
				}
				return
			}
			if !f.checkPreconditionLocked(sess.name, sess.ifGenMatch) {
				writeGCS412(w)
				return
			}
			obj := f.commitObjectLocked(sess.name, sess.contentType, sess.metadata, sess.written)
			delete(f.sessions, uploadID)
			writeGCSObjectJSON(w, obj)
			return
		}

		// Resumable session initiation POST
		var meta struct {
			Name        string            `json:"name"`
			ContentType string            `json:"contentType"`
			Metadata    map[string]string `json:"metadata"`
		}
		if err := json.NewDecoder(r.Body).Decode(&meta); err != nil {
			http.Error(w, "bad resumable init json: "+err.Error(), http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		ifGenMatch := q.Get("ifGenerationMatch")
		if !f.checkPreconditionLocked(meta.Name, ifGenMatch) {
			writeGCS412(w)
			return
		}
		f.nextSession++
		id := fmt.Sprintf("s%d", f.nextSession)
		f.sessions[id] = &fakeGCSResumableSession{
			name:        meta.Name,
			contentType: meta.ContentType,
			metadata:    meta.Metadata,
			ifGenMatch:  ifGenMatch,
		}
		w.Header().Set("Location", f.srv.URL+"/upload/storage/v1/b/b/o?uploadType=resumable&upload_id="+id)
		w.WriteHeader(http.StatusOK)
		return
	}

	// Multipart upload (default for small bodies <= 16 MiB).
	var (
		name        = q.Get("name")
		contentType = r.Header.Get("Content-Type")
		metadata    map[string]string
		bodySize    int64
	)
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err == nil && strings.HasPrefix(mediaType, "multipart/") {
		mr := multipart.NewReader(r.Body, params["boundary"])
		p1, err := mr.NextPart()
		if err != nil {
			http.Error(w, "missing metadata part", http.StatusBadRequest)
			return
		}
		var meta struct {
			Name        string            `json:"name"`
			ContentType string            `json:"contentType"`
			Metadata    map[string]string `json:"metadata"`
		}
		if err := json.NewDecoder(p1).Decode(&meta); err != nil {
			http.Error(w, "bad metadata part: "+err.Error(), http.StatusBadRequest)
			return
		}
		if meta.Name != "" {
			name = meta.Name
		}
		contentType = meta.ContentType
		metadata = meta.Metadata
		p2, err := mr.NextPart()
		if err != nil {
			http.Error(w, "missing media part", http.StatusBadRequest)
			return
		}
		bodySize, _ = io.Copy(io.Discard, p2)
	} else {
		bodySize, _ = io.Copy(io.Discard, r.Body)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.checkPreconditionLocked(name, q.Get("ifGenerationMatch")) {
		writeGCS412(w)
		return
	}
	obj := f.commitObjectLocked(name, contentType, metadata, bodySize)
	writeGCSObjectJSON(w, obj)
}

// TestGCSUpload_ConditionalNewObject_SmallAndOver16MiB verifies that a
// conditional upload (IfGenerationMatch=0 / DoesNotExist) of a new object
// succeeds and returns the written object's attributes without issuing a
// conditioned metadata GET (which real GCS rejects with 412 once the object
// exists), for both a small single-shot body and a >16 MiB resumable body.
func TestGCSUpload_ConditionalNewObject_SmallAndOver16MiB(t *testing.T) {
	for _, tc := range []struct {
		name string
		size int64
	}{
		{name: "small body (11 bytes)", size: 11},
		{name: "large body (>16 MiB)", size: 17 << 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeStatefulGCS(t)
			gcs := newFakeGCS(t, fake.srv)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			const objPath = "hub-1/trajectories/v1/raw/g0-0000000000000000.seg.gz"
			zero := int64(0)
			wantMeta := map[string]string{"sha256": "abc123", "kind": "raw"}

			obj, err := gcs.Upload(ctx, objPath, io.LimitReader(repeatZeroReader{}, tc.size), UploadOptions{
				ContentType:       "application/gzip",
				Metadata:          wantMeta,
				IfGenerationMatch: &zero,
			})
			if err != nil {
				t.Fatalf("conditional Upload(size=%d) failed: %v", tc.size, err)
			}
			if obj.Name != objPath {
				t.Errorf("Name = %q, want %q", obj.Name, objPath)
			}
			if obj.Size != tc.size {
				t.Errorf("Size = %d, want %d", obj.Size, tc.size)
			}
			if obj.ContentType != "application/gzip" {
				t.Errorf("ContentType = %q, want %q", obj.ContentType, "application/gzip")
			}
			if obj.ETag != "etag-gen-1" {
				t.Errorf("ETag = %q, want %q", obj.ETag, "etag-gen-1")
			}
			wantTime := fake.now.Add(1 * time.Second)
			if !obj.Created.Equal(wantTime) {
				t.Errorf("Created = %v, want %v", obj.Created, wantTime)
			}
			if !obj.Updated.Equal(wantTime) {
				t.Errorf("Updated = %v, want %v", obj.Updated, wantTime)
			}
			if len(obj.Metadata) != len(wantMeta) || obj.Metadata["sha256"] != "abc123" || obj.Metadata["kind"] != "raw" {
				t.Errorf("Metadata = %v, want %v", obj.Metadata, wantMeta)
			}

			fake.mu.Lock()
			gets := fake.metadataGETs
			fake.mu.Unlock()
			if gets != 0 {
				t.Errorf("metadata GET count = %d, want 0 (attrs should come from writer.Attrs())", gets)
			}

			// A second conditional upload with IfGenerationMatch=0 must still fail with ErrPreconditionFailed.
			_, err = gcs.Upload(ctx, objPath, io.LimitReader(repeatZeroReader{}, 5), UploadOptions{
				ContentType:       "application/gzip",
				IfGenerationMatch: &zero,
			})
			if !errors.Is(err, ErrPreconditionFailed) {
				t.Fatalf("second conditional Upload err = %v, want ErrPreconditionFailed", err)
			}
		})
	}
}

// TestGCSUpload_ConditionalGenerationMatch verifies that a generation-match
// conditional upload (IfGenerationMatch > 0) also succeeds without failing on
// a post-write conditioned metadata GET, and returns the new generation's
// attributes.
func TestGCSUpload_ConditionalGenerationMatch(t *testing.T) {
	fake := newFakeStatefulGCS(t)
	gcs := newFakeGCS(t, fake.srv)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const objPath = "hub-1/manifest.json"
	zero := int64(0)
	first, err := gcs.Upload(ctx, objPath, strings.NewReader("v1"), UploadOptions{
		ContentType:       "application/json",
		IfGenerationMatch: &zero,
	})
	if err != nil {
		t.Fatalf("initial upload failed: %v", err)
	}
	if first.ETag != "etag-gen-1" {
		t.Fatalf("first ETag = %q, want etag-gen-1", first.ETag)
	}

	gen1 := int64(1)
	second, err := gcs.Upload(ctx, objPath, strings.NewReader("v2-updated"), UploadOptions{
		ContentType:       "application/json",
		Metadata:          map[string]string{"rev": "2"},
		IfGenerationMatch: &gen1,
	})
	if err != nil {
		t.Fatalf("generation-match upload failed: %v", err)
	}
	if second.Size != int64(len("v2-updated")) {
		t.Errorf("Size = %d, want %d", second.Size, len("v2-updated"))
	}
	if second.ETag != "etag-gen-2" {
		t.Errorf("ETag = %q, want etag-gen-2", second.ETag)
	}
	if second.Metadata["rev"] != "2" {
		t.Errorf("Metadata = %v, want rev=2", second.Metadata)
	}

	// Reusing stale generation 1 must fail with ErrPreconditionFailed.
	_, err = gcs.Upload(ctx, objPath, strings.NewReader("v3-stale"), UploadOptions{
		ContentType:       "application/json",
		IfGenerationMatch: &gen1,
	})
	if !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("stale generation-match upload err = %v, want ErrPreconditionFailed", err)
	}
}
