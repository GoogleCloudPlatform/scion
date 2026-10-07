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

package hubclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestArtifactPublish(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/artifacts" {
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("name") != "design notes.md" || q.Get("title") != "Design" || q.Get("scope") != "p1" {
			t.Errorf("query %v", q)
		}
		if r.Header.Get("X-Scion-Agent-Token") != "tok" {
			t.Errorf("agent token not sent")
		}
		if r.Header.Get("X-Content-SHA256") != "abc" || r.Header.Get("Content-Type") != "text/markdown" {
			t.Errorf("headers %v", r.Header)
		}
		if r.ContentLength != 5 {
			t.Errorf("Content-Length %d", r.ContentLength)
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != "hello" {
			t.Errorf("body %q", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError) // must not be retried
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "internal", "message": "boom"}})
	}))
	defer srv.Close()

	c, err := New(srv.URL, WithAgentToken("tok"), WithRetry(3, 0))
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Artifacts().Publish(context.Background(), &PublishArtifactRequest{
		Name: "design notes.md", Title: "Design", Scope: "p1", Content: strings.NewReader("hello"),
		Size: 5, SHA256: "abc", ContentType: "text/markdown",
	})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("err = %v, want the hub error", err)
	}
	if calls.Load() != 1 {
		t.Errorf("publish sent %d times, want exactly once", calls.Load())
	}
}

func TestArtifactGetAndOpenFileStreamed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/api/v1/artifacts/id-1":
			_ = json.NewEncoder(w).Encode(ArtifactResponse{Artifact: Artifact{ID: "id-1", Title: "T"},
				Version: &ArtifactVersion{Seq: 1, EntryPath: "a b.md", Files: []ArtifactFile{{Path: "a b.md"}}}})
		case "/api/v1/artifacts/id-1/files/a%20b.md":
			_, _ = io.WriteString(w, "current")
		case "/api/v1/artifacts/id-1/versions/2/files/dir/a%20b.md":
			_, _ = io.WriteString(w, "v2")
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"code":"not_found","message":"not found"}}`)
		}
	}))
	defer srv.Close()
	c, _ := New(srv.URL)
	ctx := context.Background()

	got, err := c.Artifacts().Get(ctx, "id-1")
	if err != nil || got.Artifact.Title != "T" || got.Version.EntryPath != "a b.md" {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	for _, tc := range []struct {
		seq  int
		path string
		want string
	}{{0, "a b.md", "current"}, {2, "dir/a b.md", "v2"}} {
		rc, err := c.Artifacts().OpenFile(ctx, "id-1", tc.seq, tc.path)
		if err != nil {
			t.Fatalf("OpenFile(%d, %q): %v", tc.seq, tc.path, err)
		}
		b, _ := io.ReadAll(rc)
		_ = rc.Close()
		if string(b) != tc.want {
			t.Errorf("OpenFile(%d, %q) = %q, want %q", tc.seq, tc.path, b, tc.want)
		}
	}
	if _, err := c.Artifacts().OpenFile(ctx, "id-2", 0, "x"); err == nil {
		t.Error("OpenFile of a missing artifact succeeded")
	}
}

// TestArtifactOpenFileRedirectDropsCredentials checks that the signed
// object-store URL is fetched without the hub token.
func TestArtifactOpenFileRedirectDropsCredentials(t *testing.T) {
	object := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, h := range []string{"X-Scion-Agent-Token", "Authorization", "Cookie"} {
			if r.Header.Get(h) != "" {
				t.Errorf("object store received %s", h)
			}
		}
		if r.URL.Query().Get("sig") != "s" {
			t.Errorf("signed query lost: %s", r.URL.RawQuery)
		}
		_, _ = io.WriteString(w, "object bytes")
	}))
	defer object.Close()
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Scion-Agent-Token") != "tok" {
			t.Errorf("hub did not receive the agent token")
		}
		http.Redirect(w, r, object.URL+"/blob?sig=s", http.StatusFound)
	}))
	defer hub.Close()

	c, _ := New(hub.URL, WithAgentToken("tok"))
	rc, err := c.Artifacts().OpenFile(context.Background(), "id-1", 0, "a.png")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	b, _ := io.ReadAll(rc)
	if string(b) != "object bytes" {
		t.Errorf("body %q", b)
	}
}

// TestArtifactLongCallsIgnoreClientTimeout: publishing, uploading,
// finalizing and reading file bytes are bounded by the caller's context,
// not by the client's whole-exchange timeout, which still applies to
// metadata calls.
func TestArtifactLongCallsIgnoreClientTimeout(t *testing.T) {
	const slow = 300 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(slow)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPut:
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/finalize"), r.URL.Query().Has("name"):
			_, _ = io.WriteString(w, `{"artifact":{"id":"a"},"version":{"seq":1}}`)
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"artifact":{"id":"a"},"version":{"seq":1},"upload":{"required":[]}}`)
		case strings.Contains(r.URL.Path, "/files/"):
			_, _ = io.WriteString(w, "bytes")
		default:
			_, _ = io.WriteString(w, `{"artifact":{"id":"a"}}`)
		}
	}))
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, WithTimeout(slow/3))
	if err != nil {
		t.Fatal(err)
	}
	svc := c.Artifacts()
	ctx := context.Background()
	if _, err := svc.Publish(ctx, &PublishArtifactRequest{Name: "a.md", Content: strings.NewReader("x"), Size: 1}); err != nil {
		t.Errorf("Publish: %v", err)
	}
	if _, err := svc.CreateVersion(ctx, "", &CreateVersionRequest{Entry: "a.md"}); err != nil {
		t.Errorf("CreateVersion: %v", err)
	}
	if err := svc.UploadFile(ctx, "a", 1, "a.md", strings.NewReader("x"), 1, ""); err != nil {
		t.Errorf("UploadFile: %v", err)
	}
	if _, err := svc.FinalizeVersion(ctx, "a", 1); err != nil {
		t.Errorf("FinalizeVersion: %v", err)
	}
	rc, err := svc.OpenFile(ctx, "a", 1, "a.md")
	if err != nil {
		t.Errorf("OpenFile: %v", err)
	} else {
		_ = rc.Close()
	}
	if _, err := svc.Get(ctx, "a"); err == nil {
		t.Errorf("Get: a metadata call must keep the client timeout")
	}
	// The caller's context still bounds the long calls.
	short, cancel := context.WithTimeout(ctx, slow/3)
	defer cancel()
	if _, err := svc.FinalizeVersion(short, "a", 1); err == nil {
		t.Errorf("FinalizeVersion ignored the context deadline")
	}
}
