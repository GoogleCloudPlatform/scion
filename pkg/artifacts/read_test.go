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
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// --- read ---

func TestGetArtifactMetadata(t *testing.T) {
	f := newFixture(t, false)
	pub := f.publish(agentA, "notes.txt", []byte("hello"), "")
	rec := f.do(&agentB, http.MethodGet, "/api/v1/artifacts/"+pub.Artifact.ID, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var got ArtifactResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Artifact.ID != pub.Artifact.ID || got.Version == nil || len(got.Version.Files) != 1 || got.Version.Files[0].Path != "notes.txt" {
		t.Errorf("metadata = %+v", got)
	}
	if rec.Header().Get("Cache-Control") != "private, no-store" {
		t.Errorf("Cache-Control = %q", rec.Header().Get("Cache-Control"))
	}
}

// TestReadAuthorization covers who may read: the owner, members of the home
// scope via the synthetic grant, principal grants; and that everyone else
// gets the same 404 as for an artifact that does not exist.
func TestReadAuthorization(t *testing.T) {
	f := newFixture(t, false)
	pub := f.publish(agentA, "doc.md", []byte("# doc"), "")
	id := pub.Artifact.ID
	paths := []string{
		"/api/v1/artifacts/" + id,
		"/api/v1/artifacts/" + id + "/files/doc.md",
		"/api/v1/artifacts/" + id + "/versions/1/files/doc.md",
	}
	missing := "/api/v1/artifacts/00000000-0000-4000-8000-000000000000"
	notFound := f.do(&agentA, http.MethodGet, missing, nil, nil)
	if notFound.Code != http.StatusNotFound {
		t.Fatalf("missing artifact: %d", notFound.Code)
	}

	for _, tc := range []struct {
		name string
		p    *principal
		ok   bool
	}{
		{"owner", &agentA, true},
		{"same project agent", &agentB, true},
		{"project member user", &userU, true},
		{"other project agent", &agentX, false},
		{"outsider user", &outside, false},
		{"unauthenticated", nil, false},
	} {
		for _, p := range paths {
			rec := f.do(tc.p, http.MethodGet, p, nil, nil)
			if tc.ok && rec.Code != http.StatusOK {
				t.Errorf("%s %s: %d, want 200", tc.name, p, rec.Code)
			}
			if !tc.ok {
				// Same status and body as a missing artifact: absent and unreadable
				// artifacts answer identically.
				if rec.Code != http.StatusNotFound || rec.Body.String() != notFound.Body.String() {
					t.Errorf("%s %s: %d %q, want the missing-artifact 404", tc.name, p, rec.Code, rec.Body.String())
				}
			}
		}
	}

	// A principal grant lets the other-project agent in.
	if _, err := f.db.Exec(`INSERT INTO artifact_grant (id, artifact_id, subject_kind, subject_ref, permission, created_at)
		VALUES ('g2', ?, 'principal', ?, 'read', ?)`, id, PrincipalRef(agentX.kind, agentX.ref), time.Now().UTC().Format(sqliteTimeLayout)); err != nil {
		t.Fatal(err)
	}
	if rec := f.do(&agentX, http.MethodGet, paths[1], nil, nil); rec.Code != http.StatusOK {
		t.Errorf("principal grant: %d, want 200", rec.Code)
	}
	// An expired grant does not.
	if _, err := f.db.Exec(`UPDATE artifact_grant SET expires_at = ? WHERE id = 'g2'`, time.Now().Add(-time.Minute).UTC().Format(sqliteTimeLayout)); err != nil {
		t.Fatal(err)
	}
	if rec := f.do(&agentX, http.MethodGet, paths[1], nil, nil); rec.Code != http.StatusNotFound {
		t.Errorf("expired principal grant: %d, want 404", rec.Code)
	}
}

func TestReadAfterHomeScopeLost(t *testing.T) {
	// When the host stops authorizing the home scope (project deleted,
	// membership removed), members lose access and the owner keeps it (D16).
	f := newFixture(t, false)
	pub := f.publish(agentA, "doc.md", []byte("# doc"), "")
	f.host.mu.Lock()
	f.host.perms = map[string]map[string]map[string]bool{}
	f.host.mu.Unlock()
	if rec := f.do(&agentB, http.MethodGet, "/api/v1/artifacts/"+pub.Artifact.ID, nil, nil); rec.Code != http.StatusNotFound {
		t.Errorf("member after scope loss: %d, want 404", rec.Code)
	}
	if rec := f.do(&agentA, http.MethodGet, "/api/v1/artifacts/"+pub.Artifact.ID, nil, nil); rec.Code != http.StatusOK {
		t.Errorf("owner after scope loss: %d, want 200", rec.Code)
	}
}

func TestReadExpiredOrDeletedArtifact(t *testing.T) {
	f := newFixture(t, false)
	for _, col := range []string{"expires_at", "deleted_at"} {
		pub := f.publish(agentA, "doc.md", []byte("# doc "+col), "")
		if _, err := f.db.Exec("UPDATE artifact SET "+col+" = ? WHERE id = ?", time.Now().Add(-time.Second).UTC().Format(sqliteTimeLayout), pub.Artifact.ID); err != nil {
			t.Fatal(err)
		}
		if rec := f.do(&agentA, http.MethodGet, "/api/v1/artifacts/"+pub.Artifact.ID, nil, nil); rec.Code != http.StatusNotFound {
			t.Errorf("%s set: owner read %d, want 404", col, rec.Code)
		}
	}
}

func TestGetFileStreamsLocal(t *testing.T) {
	f := newFixture(t, false)
	body := []byte("# Title\n\n<script>alert(1)</script>\n")
	pub := f.publish(agentA, "design.md", body, "")
	for _, p := range []string{
		"/api/v1/artifacts/" + pub.Artifact.ID + "/files/design.md",
		"/api/v1/artifacts/" + pub.Artifact.ID + "/versions/1/files/design.md",
		"/api/v1/artifacts/" + pub.Artifact.ID + "/files/design.md?stream=1", // no-op on local
	} {
		rec := f.do(&agentB, http.MethodGet, p, nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", p, rec.Code, rec.Body.String())
		}
		if !bytes.Equal(rec.Body.Bytes(), body) {
			t.Errorf("%s: body differs", p)
		}
		h := rec.Header()
		for k, want := range map[string]string{
			"Content-Type":            "text/markdown; charset=utf-8",
			"X-Content-Type-Options":  "nosniff",
			"Content-Disposition":     `inline; filename=design.md`,
			"Content-Security-Policy": fileCSP,
			"Content-Length":          strconv.Itoa(len(body)),
			"ETag":                    `"sha256:` + sha(body) + `"`,
		} {
			if got := h.Get(k); got != want {
				t.Errorf("%s: %s = %q, want %q", p, k, got, want)
			}
		}
	}
	// Conditional GET.
	rec := f.do(&agentB, http.MethodGet, "/api/v1/artifacts/"+pub.Artifact.ID+"/files/design.md", nil,
		map[string]string{"If-None-Match": `"sha256:` + sha(body) + `"`})
	if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
		t.Errorf("If-None-Match: %d, %d bytes", rec.Code, rec.Body.Len())
	}
	// HEAD has headers and no body.
	rec = f.do(&agentB, http.MethodHead, "/api/v1/artifacts/"+pub.Artifact.ID+"/files/design.md", nil, nil)
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 || rec.Header().Get("Content-Length") != strconv.Itoa(len(body)) {
		t.Errorf("HEAD: %d, %d bytes, length %q", rec.Code, rec.Body.Len(), rec.Header().Get("Content-Length"))
	}
}

func TestGetFileUnsafeTypesAreAttachments(t *testing.T) {
	f := newFixture(t, false)
	for name, wantType := range map[string]string{
		"page.html": "text/html; charset=utf-8",
		"pic.svg":   "image/svg+xml",
		"blob.bin":  "application/octet-stream",
	} {
		pub := f.publish(agentA, name, []byte("<html><script>x</script></html>"), "")
		rec := f.do(&agentA, http.MethodGet, "/api/v1/artifacts/"+pub.Artifact.ID+"/files/"+name, nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d", name, rec.Code)
		}
		if got := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(got, "attachment") {
			t.Errorf("%s: Content-Disposition %q, want attachment", name, got)
		}
		if name != "blob.bin" && rec.Header().Get("Content-Type") != wantType {
			t.Errorf("%s: Content-Type %q, want %q", name, rec.Header().Get("Content-Type"), wantType)
		}
		if rec.Header().Get("Content-Security-Policy") != fileCSP {
			t.Errorf("%s: missing sandbox CSP", name)
		}
	}
}

func TestGetFileRedirectsOnObjectStore(t *testing.T) {
	f := newFixture(t, true)
	body := []byte("png-ish")
	pub := f.publish(agentA, "shot.png", body, "")
	base := "/api/v1/artifacts/" + pub.Artifact.ID + "/files/shot.png"

	rec := f.do(&agentB, http.MethodGet, base, nil, nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("status %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "https://objects.example/"+BlobPath("hub-1", sha(body))+"?sig=x" {
		t.Errorf("Location = %q", loc)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("redirect carried %d body bytes", rec.Body.Len())
	}
	rs := f.blobs.(*redirectingStorage)
	if rs.last.Method != http.MethodGet || rs.last.Expires != signedURLTTL ||
		rs.last.ResponseContentType != "image/png" || rs.last.ResponseContentDisposition != "inline; filename=shot.png" {
		t.Errorf("signed URL options = %+v", rs.last)
	}
	for k, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"Content-Disposition":    "inline; filename=shot.png",
		"Cache-Control":          "private, no-store",
	} {
		if got := rec.Header().Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}

	// ?stream=1 makes the hub serve the bytes with the same headers.
	rec = f.do(&agentB, http.MethodGet, base+"?stream=1", nil, nil)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), body) {
		t.Fatalf("stream: %d %q", rec.Code, rec.Body.String())
	}
	for k, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"Content-Disposition":    "inline; filename=shot.png",
		"Content-Type":           "image/png",
	} {
		if got := rec.Header().Get(k); got != want {
			t.Errorf("stream %s = %q, want %q", k, got, want)
		}
	}
	// Both modes share authorization: an outsider gets 404 either way.
	for _, q := range []string{"", "?stream=1"} {
		if rec := f.do(&agentX, http.MethodGet, base+q, nil, nil); rec.Code != http.StatusNotFound {
			t.Errorf("outsider %q: %d, want 404", q, rec.Code)
		}
	}
}

func TestGetFilePaths(t *testing.T) {
	f := newFixture(t, false)
	pub := f.publish(agentA, "a b.txt", []byte("spaced"), "")
	id := pub.Artifact.ID
	if rec := f.do(&agentA, http.MethodGet, "/api/v1/artifacts/"+id+"/files/a%20b.txt", nil, nil); rec.Code != http.StatusOK {
		t.Errorf("escaped name: %d", rec.Code)
	}
	for _, p := range []string{
		"/files/../a%20b.txt",
		"/files/./a%20b.txt",
		"/files/x%2F..%2Fa%20b.txt",
		"/files/%2e%2e/a%20b.txt",
		"/files/a%5Cb.txt",
		"/files/",
		"/files",
		"/versions/0/files/a%20b.txt",
		"/versions/01/files/a%20b.txt",
		"/versions/-1/files/a%20b.txt",
		"/versions/2/files/a%20b.txt",
		"/versions/x/files/a%20b.txt",
		"/files/missing.txt",
		"/other",
		"/",
	} {
		if rec := f.do(&agentA, http.MethodGet, "/api/v1/artifacts/"+id+p, nil, nil); rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", p, rec.Code)
		}
	}
	for _, p := range []string{"/api/v1/artifacts/shared/tok", "/api/v1/artifactsX", "/api/v1/artifacts/" + id + "%2Ffiles",
		"/api/v1/artifacts/%00", "/api/v1/artifacts/%FF", "/api/v1/artifacts/not-a-uuid",
		"/api/v1/artifacts/" + strings.ToUpper(id), "/api/v1/artifacts/%00/files/a%20b.txt"} {
		if rec := f.do(&agentA, http.MethodGet, p, nil, nil); rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", p, rec.Code)
		}
	}
	for _, p := range []string{"", "/files/a%20b.txt", "/versions/1/files/a%20b.txt"} {
		rec := f.do(&agentA, http.MethodDelete, "/api/v1/artifacts/"+id+p, nil, nil)
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, HEAD" {
			t.Errorf("DELETE %s: %d Allow=%q", p, rec.Code, rec.Header().Get("Allow"))
		}
	}
}

// TestReadCredentialCheckComesFirst: Host.Permits is asked before the
// owner, home-scope and grant paths, and a refusal there is the same 404
// even for the owner or a principal-grant holder.
func TestReadCredentialCheckComesFirst(t *testing.T) {
	// Local storage streams; the object-store fixture redirects by default
	// and streams with ?stream=1. The credential check guards all three.
	for _, tc := range []struct {
		name     string
		redirect bool
		query    string
	}{
		{"local", false, ""},
		{"object store, redirect", true, ""},
		{"object store, stream", true, "?stream=1"},
	} {
		t.Run(tc.name, func(t *testing.T) { checkCredentialFirst(t, tc.redirect, tc.query) })
	}
}

func checkCredentialFirst(t *testing.T, redirect bool, query string) {
	f := newFixture(t, redirect)
	pub := f.publish(agentA, "doc.md", []byte("# doc"), "")
	id := pub.Artifact.ID
	if _, err := f.db.Exec(`INSERT INTO artifact_grant (id, artifact_id, subject_kind, subject_ref, permission, created_at)
		VALUES ('gx', ?, 'principal', ?, 'read', ?)`, id, PrincipalRef(agentX.kind, agentX.ref), time.Now().UTC().Format(sqliteTimeLayout)); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/artifacts/" + id + "/files/doc.md" + query
	okCode := http.StatusOK
	if redirect && query == "" {
		okCode = http.StatusFound
	}
	for _, p := range []principal{agentA, agentB, agentX} {
		if rec := f.do(&p, http.MethodGet, path, nil, nil); rec.Code != okCode {
			t.Fatalf("%s before deny: %d, want %d", p.ref, rec.Code, okCode)
		}
		f.host.deny(p, "project-1", PermissionRead)
	}
	notFound := f.do(&agentA, http.MethodGet, "/api/v1/artifacts/00000000-0000-4000-8000-000000000000", nil, nil).Body.String()
	for name, p := range map[string]principal{"owner": agentA, "home-scope member": agentB, "principal grant": agentX} {
		for _, route := range []string{"/api/v1/artifacts/" + id, path, "/api/v1/artifacts/" + id + "/versions/1/files/doc.md" + query} {
			rec := f.do(&p, http.MethodGet, route, nil, nil)
			if rec.Code != http.StatusNotFound || rec.Body.String() != notFound {
				t.Errorf("%s %s: %d, want the missing-artifact 404", name, route, rec.Code)
			}
		}
	}
	// The credential check is the first host question for a read.
	f.host.mu.Lock()
	f.host.calls = nil
	f.host.mu.Unlock()
	f.do(&agentB, http.MethodGet, path, nil, nil)
	f.host.mu.Lock()
	defer f.host.mu.Unlock()
	if len(f.host.calls) == 0 || f.host.calls[0] != "permits project-1 "+PermissionRead {
		t.Errorf("host calls %v, want Permits first", f.host.calls)
	}
}

func TestGetFileWithoutContentIsNotFound(t *testing.T) {
	f := newFixture(t, false)
	pub := f.publish(agentA, "doc.md", []byte("# doc"), "")
	if _, err := f.db.Exec(`INSERT INTO artifact_file (version_id, path, size, media_type, origin, source_url, fetch_status, fetch_error)
		SELECT id, '_remote/x', 0, 'application/octet-stream', 'remote', 'https://example.com/x.png', 'failed', 'status 404'
		FROM artifact_version WHERE artifact_id = ?`, pub.Artifact.ID); err != nil {
		t.Fatal(err)
	}
	if rec := f.do(&agentA, http.MethodGet, "/api/v1/artifacts/"+pub.Artifact.ID+"/files/_remote/x", nil, nil); rec.Code != http.StatusNotFound {
		t.Errorf("file without content: %d, want 404", rec.Code)
	}
}
