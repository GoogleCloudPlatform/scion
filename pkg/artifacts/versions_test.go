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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// bundle is a set of files by path.
type bundle map[string][]byte

func (bd bundle) manifest(entry string) CreateVersionRequest {
	req := CreateVersionRequest{Entry: entry}
	for p, b := range bd {
		req.Files = append(req.Files, ManifestFile{Path: p, Size: int64(len(b)), SHA256: sha(b)})
	}
	return req
}

func (f *fixture) postJSON(p *principal, target string, v any) *httptest.ResponseRecorder {
	f.t.Helper()
	body, err := json.Marshal(v)
	if err != nil {
		f.t.Fatal(err)
	}
	return f.do(p, http.MethodPost, target, body, map[string]string{"Content-Type": "application/json"})
}

func decodeInto[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return v
}

func (f *fixture) createPending(p principal, target string, req CreateVersionRequest) PendingVersionResponse {
	f.t.Helper()
	rec := f.postJSON(&p, target, req)
	if rec.Code != http.StatusCreated {
		f.t.Fatalf("create %s: %d %s", target, rec.Code, rec.Body.String())
	}
	return decodeInto[PendingVersionResponse](f.t, rec)
}

func (f *fixture) put(p principal, id string, seq int, filePath string, body []byte) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.do(&p, http.MethodPut, fmt.Sprintf("/api/v1/artifacts/%s/versions/%d/files/%s", id, seq, filePath), body, nil)
}

func (f *fixture) finalize(p principal, id string, seq int) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.do(&p, http.MethodPost, fmt.Sprintf("/api/v1/artifacts/%s/versions/%d/finalize", id, seq), nil, nil)
}

// publishBundle runs the whole two-step publish and returns the finalized
// response.
func (f *fixture) publishBundle(p principal, target string, req CreateVersionRequest, files bundle) ArtifactResponse {
	f.t.Helper()
	pend := f.createPending(p, target, req)
	for _, path := range pend.Upload.Required {
		if rec := f.put(p, pend.Artifact.ID, pend.Version.Seq, path, files[path]); rec.Code != http.StatusNoContent {
			f.t.Fatalf("PUT %s: %d %s", path, rec.Code, rec.Body.String())
		}
	}
	rec := f.finalize(p, pend.Artifact.ID, pend.Version.Seq)
	if rec.Code != http.StatusOK {
		f.t.Fatalf("finalize: %d %s", rec.Code, rec.Body.String())
	}
	return decodeInto[ArtifactResponse](f.t, rec)
}

var site = bundle{
	"index.html":  []byte(`<html><img src="img/a.png"></html>`),
	"img/a.png":   []byte("\x89PNG\r\n\x1a\nfake"),
	"css/s.css":   []byte("body{}"),
	"notes/a.dat": []byte("plain words"),
}

func TestTwoStepPublishBundle(t *testing.T) {
	f := newFixture(t, false)
	req := site.manifest("index.html")
	req.Title, req.Note = "Site", "first"
	pend := f.createPending(agentA, "/api/v1/artifacts", req)
	if pend.Version.State != VersionStatePending || pend.Version.Seq != 1 || len(pend.Upload.Required) != len(site) {
		t.Fatalf("pending = %+v", pend)
	}
	id := pend.Artifact.ID
	if pend.Artifact.CurrentSeq != 0 {
		t.Errorf("pending artifact has current version %d", pend.Artifact.CurrentSeq)
	}
	// Nothing of a pending version is readable.
	for _, p := range []string{"/api/v1/artifacts/" + id + "/versions/1", "/api/v1/artifacts/" + id + "/versions/1/files/index.html", "/api/v1/artifacts/" + id + "/files/index.html"} {
		if rec := f.do(&agentA, http.MethodGet, p, nil, nil); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s while pending: %d, want 404", p, rec.Code)
		}
	}
	// Finalize refuses while files are missing and names them.
	if rec := f.put(agentA, id, 1, "index.html", site["index.html"]); rec.Code != http.StatusNoContent {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body.String())
	}
	rec := f.finalize(agentA, id, 1)
	if rec.Code != http.StatusConflict || errCode(t, rec) != CodeIncomplete || !strings.Contains(rec.Body.String(), "img/a.png") {
		t.Fatalf("incomplete finalize: %d %s", rec.Code, rec.Body.String())
	}
	// Re-uploading is idempotent while pending.
	if rec := f.put(agentA, id, 1, "index.html", site["index.html"]); rec.Code != http.StatusNoContent {
		t.Errorf("second PUT: %d", rec.Code)
	}
	for _, p := range []string{"img/a.png", "css/s.css", "notes/a.dat"} {
		if rec := f.put(agentA, id, 1, p, site[p]); rec.Code != http.StatusNoContent {
			t.Fatalf("PUT %s: %d %s", p, rec.Code, rec.Body.String())
		}
	}
	rec = f.finalize(agentA, id, 1)
	if rec.Code != http.StatusOK {
		t.Fatalf("finalize: %d %s", rec.Code, rec.Body.String())
	}
	done := decodeInto[ArtifactResponse](t, rec)
	if done.Artifact.CurrentSeq != 1 || done.Version.State != VersionStateReady || done.Version.EntryPath != "index.html" ||
		done.Version.Note != "first" || done.Version.FileCount != 4 || len(done.Version.Files) != 4 {
		t.Errorf("finalized = %+v / %+v", done.Artifact, done.Version)
	}
	types := map[string]string{}
	for _, fi := range done.Version.Files {
		types[fi.Path] = fi.MediaType
	}
	if types["img/a.png"] != "image/png" || types["css/s.css"] != "text/css" || types["notes/a.dat"] != "text/plain" {
		t.Errorf("media types = %v", types)
	}
	// Every file is readable by a project member, at the current and the
	// pinned version.
	for p, body := range site {
		for _, u := range []string{"/api/v1/artifacts/" + id + "/files/" + p, "/api/v1/artifacts/" + id + "/versions/1/files/" + p} {
			rec := f.do(&agentB, http.MethodGet, u, nil, nil)
			if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), body) {
				t.Errorf("GET %s: %d", u, rec.Code)
			}
		}
	}
	// After finalize the version takes no more uploads and no second
	// finalize.
	if rec := f.put(agentA, id, 1, "index.html", site["index.html"]); rec.Code != http.StatusConflict {
		t.Errorf("PUT after finalize: %d, want 409", rec.Code)
	}
	if rec := f.finalize(agentA, id, 1); rec.Code != http.StatusConflict {
		t.Errorf("second finalize: %d, want 409", rec.Code)
	}
}

func TestTwoStepKeyAppendsVersions(t *testing.T) {
	f := newFixture(t, false)
	first := bundle{"design.md": []byte("# v1"), "img/a.png": []byte("png-1")}
	req := first.manifest("design.md")
	req.Key = "artifacts/design"
	v1 := f.publishBundle(agentA, "/api/v1/artifacts", req, first)

	// Same key, changed entry, unchanged image: v2 of the same artifact,
	// and only the changed file needs uploading.
	second := bundle{"design.md": []byte("# v2"), "img/a.png": []byte("png-1")}
	req = second.manifest("design.md")
	req.Key = "artifacts/design"
	pend := f.createPending(agentA, "/api/v1/artifacts", req)
	if pend.Artifact.ID != v1.Artifact.ID || pend.Version.Seq != 2 {
		t.Fatalf("same key made %s v%d, want %s v2", pend.Artifact.ID, pend.Version.Seq, v1.Artifact.ID)
	}
	if fmt.Sprint(pend.Upload.Required) != "[design.md]" {
		t.Errorf("required = %v, want only the changed file", pend.Upload.Required)
	}
	if rec := f.put(agentA, pend.Artifact.ID, 2, "design.md", second["design.md"]); rec.Code != http.StatusNoContent {
		t.Fatalf("PUT: %d", rec.Code)
	}
	rec := f.finalize(agentA, pend.Artifact.ID, 2)
	if rec.Code != http.StatusOK || decodeInto[ArtifactResponse](t, rec).Artifact.CurrentSeq != 2 {
		t.Fatalf("finalize v2: %d %s", rec.Code, rec.Body.String())
	}
	id := v1.Artifact.ID
	// v1 stays fetchable by seq; the current version is v2.
	if rec := f.do(&agentB, http.MethodGet, "/api/v1/artifacts/"+id+"/versions/1/files/design.md", nil, nil); rec.Body.String() != "# v1" {
		t.Errorf("v1 entry = %q", rec.Body.String())
	}
	if rec := f.do(&agentB, http.MethodGet, "/api/v1/artifacts/"+id+"/files/design.md", nil, nil); rec.Body.String() != "# v2" {
		t.Errorf("current entry = %q", rec.Body.String())
	}
	// The unchanged image is one blob shared by both versions.
	pngPath := BlobPath("hub-1", sha([]byte("png-1")))
	if ok, err := f.blobs.Exists(context.Background(), pngPath); err != nil || !ok {
		t.Errorf("shared blob missing: %v", err)
	}
	vs := decodeInto[VersionListResponse](t, f.do(&agentB, http.MethodGet, "/api/v1/artifacts/"+id+"/versions", nil, nil))
	if len(vs.Versions) != 2 || vs.Versions[0].Seq != 2 || vs.Versions[1].Seq != 1 || vs.Versions[0].Files != nil {
		t.Errorf("versions = %+v", vs.Versions)
	}
	page := decodeInto[VersionListResponse](t, f.do(&agentB, http.MethodGet, "/api/v1/artifacts/"+id+"/versions?limit=1", nil, nil))
	if len(page.Versions) != 1 || page.Versions[0].Seq != 2 || page.NextBefore != 2 {
		t.Errorf("first page = %+v", page)
	}
	page = decodeInto[VersionListResponse](t, f.do(&agentB, http.MethodGet, "/api/v1/artifacts/"+id+"/versions?limit=1&before=2", nil, nil))
	if len(page.Versions) != 1 || page.Versions[0].Seq != 1 || page.NextBefore != 1 {
		t.Errorf("second page = %+v", page)
	}
	if rec := f.do(&agentB, http.MethodGet, "/api/v1/artifacts/"+id+"/versions?limit=0", nil, nil); rec.Code != http.StatusBadRequest {
		t.Errorf("limit=0: %d", rec.Code)
	}
	one := decodeInto[ArtifactResponse](t, f.do(&agentB, http.MethodGet, "/api/v1/artifacts/"+id+"/versions/1", nil, nil))
	if one.Version.Seq != 1 || len(one.Version.Files) != 2 {
		t.Errorf("version 1 = %+v", one.Version)
	}
	// The same key for another publisher is another artifact.
	req = first.manifest("design.md")
	req.Key = "artifacts/design"
	other := f.createPending(agentB, "/api/v1/artifacts", req)
	if other.Artifact.ID == id || other.Version.Seq != 1 {
		t.Errorf("another publisher's key reused the artifact: %+v", other.Artifact)
	}

	// POST /{id}/versions appends too.
	third := bundle{"design.md": []byte("# v3")}
	req = third.manifest("")
	req.Note = "three"
	pend = f.createPending(agentA, "/api/v1/artifacts/"+id+"/versions", req)
	if pend.Version.Seq != 3 || pend.Version.EntryPath != "design.md" || pend.Version.Note != "three" {
		t.Errorf("appended = %+v", pend.Version)
	}
}

func TestTwoStepUploadChecks(t *testing.T) {
	f := newFixture(t, false)
	files := bundle{"a.txt": []byte("hello")}
	pend := f.createPending(agentA, "/api/v1/artifacts", files.manifest("a.txt"))
	id := pend.Artifact.ID
	for name, tc := range map[string]struct {
		p      principal
		path   string
		body   []byte
		hdr    map[string]string
		status int
	}{
		"wrong bytes":          {agentA, "a.txt", []byte("HELLO"), nil, http.StatusBadRequest},
		"short body":           {agentA, "a.txt", []byte("hell"), nil, http.StatusBadRequest},
		"long body":            {agentA, "a.txt", []byte("hello!"), nil, http.StatusBadRequest},
		"header digest":        {agentA, "a.txt", []byte("hello"), map[string]string{HeaderContentSHA256: sha([]byte("x"))}, http.StatusBadRequest},
		"not in manifest":      {agentA, "b.txt", []byte("hello"), nil, http.StatusNotFound},
		"other member":         {agentB, "a.txt", []byte("hello"), nil, http.StatusNotFound}, // not shown before its first finalize
		"outside the project":  {agentX, "a.txt", []byte("hello"), nil, http.StatusNotFound},
		"unauthenticated":      {principal{}, "a.txt", []byte("hello"), nil, http.StatusNotFound},
		"traversal":            {agentA, "..%2Fa.txt", []byte("hello"), nil, http.StatusNotFound},
		"reserved remote path": {agentA, "_remote/" + sha([]byte("u")), []byte("hello"), nil, http.StatusNotFound},
	} {
		var p *principal
		if tc.p.ref != "" {
			p = &tc.p
		}
		rec := f.do(p, http.MethodPut, fmt.Sprintf("/api/v1/artifacts/%s/versions/1/files/%s", id, tc.path), tc.body, tc.hdr)
		if rec.Code != tc.status {
			t.Errorf("%s: %d, want %d (%s)", name, rec.Code, tc.status, rec.Body.String())
		}
	}
	if rec := f.finalize(agentB, id, 1); rec.Code != http.StatusNotFound {
		t.Errorf("finalize by another member: %d, want 404", rec.Code)
	}
	if rec := f.finalize(agentX, id, 1); rec.Code != http.StatusNotFound {
		t.Errorf("finalize from outside: %d, want 404", rec.Code)
	}
	if rec := f.finalize(agentA, id, 1); rec.Code != http.StatusConflict {
		t.Errorf("finalize after only failed uploads: %d, want 409", rec.Code)
	}
	if rec := f.finalize(agentA, id, 9); rec.Code != http.StatusNotFound {
		t.Errorf("finalize of a missing version: %d, want 404", rec.Code)
	}
	// An exact Content-Length mismatch is refused before reading.
	r := httptest.NewRequest(http.MethodPut, "/api/v1/artifacts/"+id+"/versions/1/files/a.txt", strings.NewReader("hello"))
	r.ContentLength = 4
	rec := httptest.NewRecorder()
	f.svc.ServeHTTP(rec, withPrincipal(r, agentA))
	if rec.Code != http.StatusBadRequest || errCode(t, rec) != "size_mismatch" {
		t.Errorf("Content-Length mismatch: %d %s", rec.Code, rec.Body.String())
	}
}

func TestTwoStepWriteAuthorization(t *testing.T) {
	f := newFixture(t, false)
	files := bundle{"a.txt": []byte("a")}
	pub := f.publishBundle(agentA, "/api/v1/artifacts", files.manifest("a.txt"), files)
	id := pub.Artifact.ID
	target := "/api/v1/artifacts/" + id + "/versions"
	// A project member can read but not append to another's artifact.
	if rec := f.postJSON(&agentB, target, files.manifest("a.txt")); rec.Code != http.StatusForbidden {
		t.Errorf("member append: %d, want 403", rec.Code)
	}
	// Outside the project the artifact does not exist.
	if rec := f.postJSON(&agentX, target, files.manifest("a.txt")); rec.Code != http.StatusNotFound {
		t.Errorf("outsider append: %d, want 404", rec.Code)
	}
	// A principal write grant allows appending.
	now := time.Now()
	if _, err := f.db.Exec("INSERT INTO artifact_grant (id, artifact_id, subject_kind, subject_ref, permission, created_at) VALUES (?, ?, ?, ?, ?, ?)",
		"g-write", id, SubjectPrincipal, PrincipalRef(agentB.kind, agentB.ref), GrantWrite, now.UTC().Format(sqliteTimeLayout)); err != nil {
		t.Fatal(err)
	}
	if rec := f.postJSON(&agentB, target, files.manifest("a.txt")); rec.Code != http.StatusCreated {
		t.Errorf("grantee append: %d %s", rec.Code, rec.Body.String())
	}
	// The owner whose credential does not permit publishing cannot.
	f.host.deny(agentA, "project-1", PermissionCreate)
	if rec := f.postJSON(&agentA, target, files.manifest("a.txt")); rec.Code != http.StatusForbidden {
		t.Errorf("owner without update: %d, want 403", rec.Code)
	}
	// Nor can it upload to or finalize a version it started.
	delete(f.host.denied, PrincipalRef(agentA.kind, agentA.ref)+" project-1 "+PermissionCreate)
	keyed := files.manifest("a.txt")
	keyed.Key = "k"
	pend := f.createPending(agentA, "/api/v1/artifacts", keyed)
	f.host.deny(agentA, "project-1", PermissionCreate)
	if rec := f.put(agentA, pend.Artifact.ID, 1, "a.txt", files["a.txt"]); rec.Code != http.StatusForbidden {
		t.Errorf("PUT without publish permission: %d, want 403", rec.Code)
	}
	if rec := f.finalize(agentA, pend.Artifact.ID, 1); rec.Code != http.StatusForbidden {
		t.Errorf("finalize without publish permission: %d, want 403", rec.Code)
	}
	// Creating needs artifact.create in the scope.
	if rec := f.postJSON(&agentA, "/api/v1/artifacts", CreateVersionRequest{Scope: "project-2", Entry: "a.txt",
		Files: []ManifestFile{{Path: "a.txt", Size: 1, SHA256: sha([]byte("a"))}}}); rec.Code != http.StatusForbidden {
		t.Errorf("create in a foreign scope: %d, want 403", rec.Code)
	}
	if rec := f.postJSON(nil, "/api/v1/artifacts", files.manifest("a.txt")); rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated create: %d, want 401", rec.Code)
	}
}

func TestTwoStepManifestValidation(t *testing.T) {
	f := newFixture(t, false)
	f.svc.SetLimits(func(context.Context) Limits { return Limits{MaxFileBytes: 10, MaxBundleBytes: 15, MaxFiles: 3} })
	d := sha([]byte("x"))
	file := func(p string, size int64) ManifestFile { return ManifestFile{Path: p, Size: size, SHA256: d} }
	for name, tc := range map[string]struct {
		req    CreateVersionRequest
		status int
	}{
		"no files":                          {CreateVersionRequest{Entry: "a"}, 400},
		"entry missing":                     {CreateVersionRequest{Entry: "b", Files: []ManifestFile{file("a", 1)}}, 400},
		"no entry, several":                 {CreateVersionRequest{Files: []ManifestFile{file("a", 1), file("b", 1)}}, 400},
		"duplicate path":                    {CreateVersionRequest{Entry: "a", Files: []ManifestFile{file("a", 1), file("a", 1)}}, 400},
		"file and directory":                {CreateVersionRequest{Entry: "a", Files: []ManifestFile{file("a", 1), file("a/b", 1)}}, 400},
		"case collision":                    {CreateVersionRequest{Entry: "a.md", Files: []ManifestFile{file("a.md", 1), file("A.md", 1)}}, 400},
		"normalization collision":           {CreateVersionRequest{Entry: "caf\u00e9.md", Files: []ManifestFile{file("caf\u00e9.md", 1), file("cafe\u0301.md", 1)}}, 400},
		"case file and directory":           {CreateVersionRequest{Entry: "a", Files: []ManifestFile{file("a", 1), file("A/b", 1)}}, 400},
		"C1 control":                        {CreateVersionRequest{Entry: "a\u0085b", Files: []ManifestFile{file("a\u0085b", 1)}}, 400},
		"bidi control":                      {CreateVersionRequest{Entry: "a\u202eb", Files: []ManifestFile{file("a\u202eb", 1)}}, 400},
		"zero width":                        {CreateVersionRequest{Entry: "a\u200bb", Files: []ManifestFile{file("a\u200bb", 1)}}, 400},
		"case differs in folders only":      {CreateVersionRequest{Entry: "Docs/a", Files: []ManifestFile{file("Docs/a", 1), file("docs/b", 1)}}, 201},
		"traversal":                         {CreateVersionRequest{Entry: "../a", Files: []ManifestFile{file("../a", 1)}}, 400},
		"absolute":                          {CreateVersionRequest{Entry: "/a", Files: []ManifestFile{file("/a", 1)}}, 400},
		"reserved":                          {CreateVersionRequest{Entry: "_remote/x", Files: []ManifestFile{file("_remote/x", 1)}}, 400},
		"name starting with a dot":          {CreateVersionRequest{Entry: "a", Files: []ManifestFile{file("a", 1), file(".envrc", 1)}}, 400},
		"folder starting with a dot":        {CreateVersionRequest{Entry: "a", Files: []ManifestFile{file("a", 1), file(".git/config", 1)}}, 400},
		"nested folder starting with a dot": {CreateVersionRequest{Entry: "a", Files: []ManifestFile{file("a", 1), file("foo/.git/config", 1)}}, 400},
		"dot folder with a nested file":     {CreateVersionRequest{Entry: "a", Files: []ManifestFile{file("a", 1), file(".claude/settings.json", 1)}}, 400},
		"reserved bare":                     {CreateVersionRequest{Entry: "a", Files: []ManifestFile{file("a", 1), file("_remote", 1)}}, 400},
		"bad digest":                        {CreateVersionRequest{Entry: "a", Files: []ManifestFile{{Path: "a", Size: 1, SHA256: "zz"}}}, 400},
		"negative size":                     {CreateVersionRequest{Entry: "a", Files: []ManifestFile{file("a", -1)}}, 400},
		"review kind":                       {CreateVersionRequest{Kind: VersionKindReview, Entry: "a", Files: []ManifestFile{file("a", 1)}}, 400},
		"unknown kind":                      {CreateVersionRequest{Kind: "draft", Entry: "a", Files: []ManifestFile{file("a", 1)}}, 400},
		"long note":                         {CreateVersionRequest{Note: strings.Repeat("n", maxNoteRunes+1), Entry: "a", Files: []ManifestFile{file("a", 1)}}, 400},
		"long key":                          {CreateVersionRequest{Key: strings.Repeat("k", maxKeyBytes+1), Entry: "a", Files: []ManifestFile{file("a", 1)}}, 400},
		"control in key":                    {CreateVersionRequest{Key: "a\nb", Entry: "a", Files: []ManifestFile{file("a", 1)}}, 400},
		"file too large":                    {CreateVersionRequest{Entry: "a", Files: []ManifestFile{file("a", 11)}}, 413},
		"bundle too large":                  {CreateVersionRequest{Entry: "a", Files: []ManifestFile{file("a", 8), file("b", 8)}}, 413},
		"too many files":                    {CreateVersionRequest{Entry: "a", Files: []ManifestFile{file("a", 1), file("b", 1), file("c", 1), file("d", 1)}}, 413},
		"at the limits":                     {CreateVersionRequest{Entry: "a", Files: []ManifestFile{file("a", 10), file("b", 5), file("c", 0)}}, 201},
	} {
		if rec := f.postJSON(&agentA, "/api/v1/artifacts", tc.req); rec.Code != tc.status {
			t.Errorf("%s: %d, want %d (%s)", name, rec.Code, tc.status, rec.Body.String())
		}
	}
	// Not JSON, unknown fields, trailing data, oversized body.
	for name, body := range map[string]string{
		"not json":      "name=a",
		"unknown field": `{"entry":"a","files":[],"owner":"x"}`,
		"trailing":      `{"entry":"a","files":[{"path":"a","size":1,"sha256":"` + d + `"}]} {}`,
		"oversized":     `{"note":"` + strings.Repeat("x", maxManifestBytes) + `"}`,
	} {
		rec := f.do(&agentA, http.MethodPost, "/api/v1/artifacts", []byte(body), nil)
		if rec.Code != http.StatusBadRequest && rec.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("%s: %d", name, rec.Code)
		}
	}
	// Title, key and scope belong to creation only.
	pend := f.createPending(agentA, "/api/v1/artifacts", CreateVersionRequest{Entry: "a", Files: []ManifestFile{file("a", 1)}})
	if rec := f.postJSON(&agentA, "/api/v1/artifacts/"+pend.Artifact.ID+"/versions",
		CreateVersionRequest{Title: "t", Entry: "a", Files: []ManifestFile{file("a", 1)}}); rec.Code != http.StatusBadRequest {
		t.Errorf("title on append: %d, want 400", rec.Code)
	}
}

func TestTwoStepPendingCap(t *testing.T) {
	f := newFixture(t, false)
	files := bundle{"a.txt": []byte("a")}
	pend := f.createPending(agentA, "/api/v1/artifacts", files.manifest("a.txt"))
	target := "/api/v1/artifacts/" + pend.Artifact.ID + "/versions"
	for i := 1; i < MaxPendingVersions; i++ {
		f.createPending(agentA, target, files.manifest("a.txt"))
	}
	rec := f.postJSON(&agentA, target, files.manifest("a.txt"))
	if rec.Code != http.StatusConflict || errCode(t, rec) != "too_many_pending" {
		t.Errorf("pending beyond the cap: %d %s", rec.Code, rec.Body.String())
	}
}

func TestServiceReapPending(t *testing.T) {
	f := newFixture(t, false)
	files := bundle{"a.txt": []byte("a")}
	pend := f.createPending(agentA, "/api/v1/artifacts", files.manifest("a.txt"))
	if n, err := f.svc.ReapPending(context.Background(), 10); err != nil || n != 0 {
		t.Fatalf("fresh version reaped: %d, %v", n, err)
	}
	old := time.Now().Add(-PendingVersionTTL - time.Minute).UTC().Format(sqliteTimeLayout)
	if _, err := f.db.Exec("UPDATE artifact_version SET created_at = ?", old); err != nil {
		t.Fatal(err)
	}
	if n, err := f.svc.ReapPending(context.Background(), 10); err != nil || n != 1 {
		t.Fatalf("ReapPending = %d, %v; want 1", n, err)
	}
	if rec := f.put(agentA, pend.Artifact.ID, 1, "a.txt", files["a.txt"]); rec.Code != http.StatusNotFound {
		t.Errorf("PUT to a reaped version's artifact: %d, want 404", rec.Code)
	}
	if rec := f.do(&agentA, http.MethodGet, "/api/v1/artifacts/"+pend.Artifact.ID, nil, nil); rec.Code != http.StatusNotFound {
		t.Errorf("reaped empty artifact: %d, want 404", rec.Code)
	}
}

// TestTwoStepWriteGateBindings: ownership compares kind and ref exactly, a
// grant counts only on the artifact it was written for, and a scope grant
// counts only for a caller the host authorizes to publish in the grant's
// own scope.
func TestTwoStepWriteGateBindings(t *testing.T) {
	f := newFixture(t, false)
	files := bundle{"a.txt": []byte("a")}
	owned := f.publishBundle(userU, "/api/v1/artifacts", CreateVersionRequest{Scope: "project-1", Entry: "a.txt",
		Files: []ManifestFile{{Path: "a.txt", Size: 1, SHA256: sha([]byte("a"))}}}, files)
	other := f.publishBundle(agentA, "/api/v1/artifacts", files.manifest("a.txt"), files)

	// An agent whose ref equals the owning user's id is not the owner.
	lookalike := principal{PrincipalKindAgent, userU.ref, "project-1"}
	f.host.allow(lookalike, "project-1", PermissionRead, PermissionCreate)
	if rec := f.postJSON(&lookalike, "/api/v1/artifacts/"+owned.Artifact.ID+"/versions", files.manifest("a.txt")); rec.Code != http.StatusForbidden {
		t.Errorf("agent with the owner's user id: %d, want 403", rec.Code)
	}

	insertGrant := func(id, artifactID, kind, ref, perm string) {
		t.Helper()
		if _, err := f.db.Exec("INSERT INTO artifact_grant (id, artifact_id, subject_kind, subject_ref, permission, created_at) VALUES (?, ?, ?, ?, ?, ?)",
			id, artifactID, kind, ref, perm, time.Now().UTC().Format(sqliteTimeLayout)); err != nil {
			t.Fatal(err)
		}
	}
	// A write grant on one artifact does not reach another.
	insertGrant("g-other", other.Artifact.ID, SubjectPrincipal, PrincipalRef(agentB.kind, agentB.ref), GrantWrite)
	if rec := f.postJSON(&agentB, "/api/v1/artifacts/"+owned.Artifact.ID+"/versions", files.manifest("a.txt")); rec.Code != http.StatusForbidden {
		t.Errorf("grant on another artifact: %d, want 403", rec.Code)
	}
	if rec := f.postJSON(&agentB, "/api/v1/artifacts/"+other.Artifact.ID+"/versions", files.manifest("a.txt")); rec.Code != http.StatusCreated {
		t.Errorf("grant on this artifact: %d %s", rec.Code, rec.Body.String())
	}

	// A scope write grant for project-3 counts only for callers the host
	// authorizes to publish in project-3.
	insertGrant("g-scope", owned.Artifact.ID, SubjectScope, "project-3", GrantWrite)
	member3 := principal{PrincipalKindUser, "user-3", ""}
	f.host.allow(member3, "project-1", PermissionRead)
	f.host.allow(member3, "project-2", PermissionCreate)
	if rec := f.postJSON(&member3, "/api/v1/artifacts/"+owned.Artifact.ID+"/versions", files.manifest("a.txt")); rec.Code != http.StatusForbidden {
		t.Errorf("publisher in an unrelated scope: %d, want 403", rec.Code)
	}
	f.host.allow(member3, "project-3", PermissionRead)
	if rec := f.postJSON(&member3, "/api/v1/artifacts/"+owned.Artifact.ID+"/versions", files.manifest("a.txt")); rec.Code != http.StatusForbidden {
		t.Errorf("reader (not publisher) in the grant's scope: %d, want 403", rec.Code)
	}
	f.host.allow(member3, "project-3", PermissionCreate)
	if rec := f.postJSON(&member3, "/api/v1/artifacts/"+owned.Artifact.ID+"/versions", files.manifest("a.txt")); rec.Code != http.StatusCreated {
		t.Errorf("publisher in the grant's scope: %d %s", rec.Code, rec.Body.String())
	}
	// A read grant never confers write.
	insertGrant("g-read", owned.Artifact.ID, SubjectPrincipal, PrincipalRef(agentX.kind, agentX.ref), GrantRead)
	if rec := f.postJSON(&agentX, "/api/v1/artifacts/"+owned.Artifact.ID+"/versions", files.manifest("a.txt")); rec.Code != http.StatusForbidden {
		t.Errorf("read grantee: %d, want 403", rec.Code)
	}
}

// TestPendingVersionBelongsToItsPublisher: a principal who may write the
// artifact still cannot upload to or finalize a pending version another
// principal started.
func TestPendingVersionBelongsToItsPublisher(t *testing.T) {
	f := newFixture(t, false)
	files := bundle{"a.txt": []byte("a")}
	pub := f.publishBundle(agentA, "/api/v1/artifacts", files.manifest("a.txt"), files)
	id := pub.Artifact.ID
	if _, err := f.db.Exec("INSERT INTO artifact_grant (id, artifact_id, subject_kind, subject_ref, permission, created_at) VALUES (?, ?, ?, ?, ?, ?)",
		"g-b", id, SubjectPrincipal, PrincipalRef(agentB.kind, agentB.ref), GrantWrite, time.Now().UTC().Format(sqliteTimeLayout)); err != nil {
		t.Fatal(err)
	}
	next := bundle{"a.txt": []byte("b")}
	byA := f.createPending(agentA, "/api/v1/artifacts/"+id+"/versions", next.manifest("a.txt"))
	byB := f.createPending(agentB, "/api/v1/artifacts/"+id+"/versions", next.manifest("a.txt"))
	for _, tc := range []struct {
		name string
		p    principal
		seq  int
	}{{"B on A's version", agentB, byA.Version.Seq}, {"A on B's version", agentA, byB.Version.Seq}} {
		if rec := f.put(tc.p, id, tc.seq, "a.txt", next["a.txt"]); rec.Code != http.StatusForbidden {
			t.Errorf("%s: PUT %d, want 403", tc.name, rec.Code)
		}
		if rec := f.finalize(tc.p, id, tc.seq); rec.Code != http.StatusForbidden {
			t.Errorf("%s: finalize %d, want 403", tc.name, rec.Code)
		}
	}
	if rec := f.put(agentB, id, byB.Version.Seq, "a.txt", next["a.txt"]); rec.Code != http.StatusNoContent {
		t.Errorf("B on its own version: PUT %d", rec.Code)
	}
	if rec := f.finalize(agentB, id, byB.Version.Seq); rec.Code != http.StatusOK {
		t.Errorf("B on its own version: finalize %d", rec.Code)
	}
}

// TestFinalizeAppliesCurrentLimits: limits lowered while a version is
// pending apply when it is finalized.
func TestFinalizeAppliesCurrentLimits(t *testing.T) {
	f := newFixture(t, false)
	files := bundle{"a.txt": []byte("aaaa"), "b.txt": []byte("bb")}
	for name, lim := range map[string]Limits{
		"file count": {MaxFiles: 1},
		"total size": {MaxBundleBytes: 5},
		"file size":  {MaxFileBytes: 3, MaxBundleBytes: 100},
	} {
		f.svc.SetLimits(nil)
		pend := f.createPending(agentA, "/api/v1/artifacts", files.manifest("a.txt"))
		for p, body := range files {
			if rec := f.put(agentA, pend.Artifact.ID, 1, p, body); rec.Code != http.StatusNoContent {
				t.Fatalf("PUT: %d", rec.Code)
			}
		}
		l := lim
		f.svc.SetLimits(func(context.Context) Limits { return l })
		if rec := f.finalize(agentA, pend.Artifact.ID, 1); rec.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("%s: finalize %d, want 413", name, rec.Code)
		}
	}
}

// TestPendingArtifactVisibleOnlyToItsOwner: before its first version is
// finalized, an artifact is shown only to its owner, on every route; a
// project member and a principal holding a write grant both get 404.
func TestPendingArtifactVisibleOnlyToItsOwner(t *testing.T) {
	f := newFixture(t, false)
	files := bundle{"index.html": []byte("<p>x</p>")}
	f.svc.SetViewKey(testViewKey)
	pend := f.createPending(agentA, "/api/v1/artifacts", files.manifest("index.html"))
	id := pend.Artifact.ID
	if _, err := f.db.Exec("INSERT INTO artifact_grant (id, artifact_id, subject_kind, subject_ref, permission, created_at) VALUES (?, ?, ?, ?, ?, ?)",
		"g-b", id, SubjectPrincipal, PrincipalRef(agentB.kind, agentB.ref), GrantWrite, time.Now().UTC().Format(sqliteTimeLayout)); err != nil {
		t.Fatal(err)
	}
	member := principal{PrincipalKindUser, "member-u", ""}
	f.host.allow(member, "project-1", PermissionRead, PermissionCreate)
	manifest, _ := json.Marshal(files.manifest("index.html"))
	routes := []struct {
		method, target string
		body           []byte
	}{
		{http.MethodGet, "/api/v1/artifacts/" + id, nil},
		{http.MethodGet, "/api/v1/artifacts/" + id + "/versions", nil},
		{http.MethodGet, "/api/v1/artifacts/" + id + "/versions/1", nil},
		{http.MethodGet, "/api/v1/artifacts/" + id + "/files/index.html", nil},
		{http.MethodPut, "/api/v1/artifacts/" + id + "/versions/1/files/index.html", files["index.html"]},
		{http.MethodPost, "/api/v1/artifacts/" + id + "/versions/1/finalize", nil},
		{http.MethodPost, "/api/v1/artifacts/" + id + "/versions", manifest},
		{http.MethodPost, "/api/v1/artifacts/" + id + "/versions/1/view", nil},
	}
	for _, p := range []principal{member, agentB} {
		for _, rt := range routes {
			if rec := f.do(&p, rt.method, rt.target, rt.body, nil); rec.Code != http.StatusNotFound {
				t.Errorf("%s %s %s before finalize: %d, want 404", p.ref, rt.method, rt.target, rec.Code)
			}
		}
	}
	// The list route answers 404, not an empty list.
	if rec := f.do(&member, http.MethodGet, "/api/v1/artifacts/"+id+"/versions", nil, nil); strings.Contains(rec.Body.String(), "versions") {
		t.Errorf("member list before finalize: %d %s", rec.Code, rec.Body.String())
	}
	// The owner sees and completes it.
	if rec := f.do(&agentA, http.MethodGet, "/api/v1/artifacts/"+id, nil, nil); rec.Code != http.StatusOK {
		t.Errorf("owner GET before finalize: %d", rec.Code)
	}
	if rec := f.do(&agentA, http.MethodGet, "/api/v1/artifacts/"+id+"/versions", nil, nil); rec.Code != http.StatusOK {
		t.Errorf("owner list before finalize: %d", rec.Code)
	}
	if rec := f.put(agentA, id, 1, "index.html", files["index.html"]); rec.Code != http.StatusNoContent {
		t.Fatalf("owner PUT: %d", rec.Code)
	}
	if rec := f.finalize(agentA, id, 1); rec.Code != http.StatusOK {
		t.Fatalf("owner finalize: %d", rec.Code)
	}
	for _, p := range []principal{member, agentB} {
		if rec := f.do(&p, http.MethodGet, "/api/v1/artifacts/"+id, nil, nil); rec.Code != http.StatusOK {
			t.Errorf("%s after finalize: %d, want 200", p.ref, rec.Code)
		}
	}
}

// reapedStore reports a version reaped between loading it and recording
// an upload.
type reapedStore struct{ Store }

func (reapedStore) MarkReceived(context.Context, string, string, string) error { return ErrNotFound }

func TestUploadToAVersionReapedMeanwhile(t *testing.T) {
	f := newFixture(t, false)
	files := bundle{"a.txt": []byte("a")}
	pend := f.createPending(agentA, "/api/v1/artifacts", files.manifest("a.txt"))
	f.svc.SetStore(reapedStore{f.store})
	if rec := f.put(agentA, pend.Artifact.ID, 1, "a.txt", files["a.txt"]); rec.Code != http.StatusConflict {
		t.Errorf("PUT: %d, want 409", rec.Code)
	}
}

// keyRaceStore makes the first key lookup miss while another publish
// takes the key, as a concurrent publish would.
type keyRaceStore struct {
	Store
	raced bool
	race  func()
}

func (s *keyRaceStore) GetArtifactByKey(ctx context.Context, scopeKind, scopeRef, ownerKind, ownerRef, key string) (*Artifact, error) {
	if !s.raced {
		s.raced = true
		s.race()
		return nil, ErrNotFound
	}
	return s.Store.GetArtifactByKey(ctx, scopeKind, scopeRef, ownerKind, ownerRef, key)
}

// TestCreateWithKeyRace: when another publish takes the key between the
// lookup and the create, the request appends to that artifact instead.
func TestCreateWithKeyRace(t *testing.T) {
	f := newFixture(t, false)
	files := bundle{"a.txt": []byte("a")}
	var winner string
	ks := &keyRaceStore{Store: f.store}
	ks.race = func() {
		f.svc.SetStore(f.store)
		req := files.manifest("a.txt")
		req.Key = "k"
		winner = f.createPending(agentA, "/api/v1/artifacts", req).Artifact.ID
		f.svc.SetStore(ks)
	}
	f.svc.SetStore(ks)
	req := files.manifest("a.txt")
	req.Key = "k"
	got := f.createPending(agentA, "/api/v1/artifacts", req)
	if got.Artifact.ID != winner || got.Version.Seq != 2 {
		t.Errorf("raced create = %s v%d, want %s v2", got.Artifact.ID, got.Version.Seq, winner)
	}
}

// TestKeyedAppendAsksPermitsOnce: a keyed publish that appends a version
// asks the credential check once.
func TestKeyedAppendAsksPermitsOnce(t *testing.T) {
	f := newFixture(t, false)
	files := bundle{"a.txt": []byte("a")}
	req := files.manifest("a.txt")
	req.Key = "k"
	f.createPending(agentA, "/api/v1/artifacts", req)
	f.host.calls = nil
	if got := f.createPending(agentA, "/api/v1/artifacts", req); got.Version.Seq != 2 {
		t.Fatalf("append made v%d", got.Version.Seq)
	}
	n := 0
	for _, c := range f.host.calls {
		if strings.HasPrefix(c, "permits ") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("Permits asked %d times: %v", n, f.host.calls)
	}
}

// TestFinalizeTakesOverAStaleClaim drives the takeover through the API: a
// version left claimed by a finalize that never completed answers 409
// while the claim is recent, and finalizes once the claim is stale.
func TestFinalizeTakesOverAStaleClaim(t *testing.T) {
	f := newFixture(t, false)
	files := bundle{"a.txt": []byte("a")}
	pend := f.createPending(agentA, "/api/v1/artifacts", files.manifest("a.txt"))
	id := pend.Artifact.ID
	if rec := f.put(agentA, id, 1, "a.txt", files["a.txt"]); rec.Code != http.StatusNoContent {
		t.Fatalf("PUT: %d", rec.Code)
	}
	// A finalize that claimed the version and then stopped.
	if _, err := f.store.ClaimFinalize(context.Background(), id, 1, time.Now().Add(-staleFinalizeClaim)); err != nil {
		t.Fatal(err)
	}
	if rec := f.finalize(agentA, id, 1); rec.Code != http.StatusConflict {
		t.Fatalf("finalize while the claim is recent: %d, want 409", rec.Code)
	}
	old := time.Now().Add(-staleFinalizeClaim - time.Minute).UTC().Format(sqliteTimeLayout)
	if _, err := f.db.Exec("UPDATE artifact_version SET claimed_at = ? WHERE artifact_id = ? AND seq = 1", old, id); err != nil {
		t.Fatal(err)
	}
	rec := f.finalize(agentA, id, 1)
	if rec.Code != http.StatusOK {
		t.Fatalf("finalize of a stale claim: %d %s", rec.Code, rec.Body.String())
	}
	if got := decodeInto[ArtifactResponse](t, rec); got.Artifact.CurrentSeq != 1 {
		t.Errorf("current = %d", got.Artifact.CurrentSeq)
	}
	// Uploads still need a pending version.
	pend2 := f.createPending(agentA, "/api/v1/artifacts/"+id+"/versions", files.manifest("a.txt"))
	f.put(agentA, id, pend2.Version.Seq, "a.txt", files["a.txt"])
	if _, err := f.store.ClaimFinalize(context.Background(), id, pend2.Version.Seq, time.Now().Add(-staleFinalizeClaim)); err != nil {
		t.Fatal(err)
	}
	if rec := f.put(agentA, id, pend2.Version.Seq, "a.txt", files["a.txt"]); rec.Code != http.StatusConflict {
		t.Errorf("PUT to a finalizing version: %d, want 409", rec.Code)
	}
}

// TestFinalizeWorkIsBoundedBelowTheTakeoverAge pins the ordering the
// takeover relies on: a finalize request's own work is cut off before its
// claim can be taken over.
func TestFinalizeWorkIsBoundedBelowTheTakeoverAge(t *testing.T) {
	if finalizeWorkLimit >= staleFinalizeClaim {
		t.Fatalf("finalizeWorkLimit %v must be shorter than staleFinalizeClaim %v", finalizeWorkLimit, staleFinalizeClaim)
	}
	if MaxRemoteFetchBudget > finalizeWorkLimit {
		t.Fatalf("the remote fetch budget %v must fit in finalizeWorkLimit %v", MaxRemoteFetchBudget, finalizeWorkLimit)
	}
}
