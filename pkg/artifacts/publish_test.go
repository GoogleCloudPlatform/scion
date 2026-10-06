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
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/storage"
)

// --- publish ---

func TestPublishSingleFile(t *testing.T) {
	f := newFixture(t, false)
	body := []byte("# Design\n\nHello.\n")
	resp := f.publish(agentA, "design.md", body, "title="+url.QueryEscape("Artifact system design"))

	a := resp.Artifact
	if a.Ref != "scion://artifact/"+a.ID || a.Title != "Artifact system design" || a.CurrentSeq != 1 {
		t.Errorf("artifact = %+v", a)
	}
	// Owner refs are the stable ids the host returned, homed in the caller's project.
	if a.OwnerKind != PrincipalKindAgent || a.OwnerRef != agentA.ref || a.ScopeKind != ScopeKindProject || a.ScopeRef != "project-1" {
		t.Errorf("ownership = %+v", a)
	}
	v := resp.Version
	if v == nil || v.Seq != 1 || v.State != VersionStateReady || v.Kind != VersionKindPublish || v.EntryPath != "design.md" ||
		v.FileCount != 1 || v.TotalBytes != int64(len(body)) || v.Ref != a.Ref+"@1" {
		t.Fatalf("version = %+v", v)
	}
	if len(v.Files) != 1 || v.Files[0].SHA256 != sha(body) || v.Files[0].MediaType != "text/markdown" {
		t.Errorf("files = %+v", v.Files)
	}

	// The blob is content-addressed under the hub's artifact prefix.
	digest := sha(body)
	want := "hubs/hub-1/artifacts/blobs/sha256/" + digest[:2] + "/" + digest[2:4] + "/" + digest
	if ok, _ := f.local.Exists(context.Background(), want); !ok {
		t.Errorf("blob not at %s", want)
	}

	// The home scope holds a synthetic read grant.
	grants, err := f.store.ListGrants(context.Background(), a.ID)
	if err != nil || len(grants) != 1 {
		t.Fatalf("grants = %+v, %v", grants, err)
	}
	g := grants[0]
	if g.SubjectKind != SubjectScope || g.SubjectRef != "project-1" || g.Permission != GrantRead ||
		g.CreatedByRef != PrincipalRef(PrincipalKindAgent, agentA.ref) {
		t.Errorf("grant = %+v", g)
	}
}

func TestPublishDeduplicatesBlobs(t *testing.T) {
	f := newFixture(t, false)
	body := []byte("same bytes")
	r1 := f.publish(agentA, "a.txt", body, "")
	r2 := f.publish(agentB, "b.txt", body, "")
	if r1.Artifact.ID == r2.Artifact.ID {
		t.Fatal("two publishes produced one artifact")
	}
	res, err := f.local.List(context.Background(), storage.ListOptions{Prefix: "hubs/hub-1/artifacts/blobs/"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Objects) != 1 {
		t.Errorf("%d blobs stored for identical bytes, want 1", len(res.Objects))
	}
}

func TestPublishScope(t *testing.T) {
	f := newFixture(t, false)
	// A user has no home scope and must name one.
	rec := f.do(&userU, http.MethodPost, "/api/v1/artifacts?name=a.txt", []byte("x"), nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("user without scope: %d, want 400", rec.Code)
	}
	resp := f.publish(userU, "a.txt", []byte("x"), "scope=project-1")
	if resp.Artifact.ScopeRef != "project-1" || resp.Artifact.OwnerKind != PrincipalKindUser {
		t.Errorf("user publish = %+v", resp.Artifact)
	}
	// Publishing into a scope the host does not authorize is refused.
	rec = f.do(&agentA, http.MethodPost, "/api/v1/artifacts?name=a.txt&scope=project-2", []byte("x"), nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("foreign scope: %d, want 403", rec.Code)
	}
	// A credential that does not permit creating in the scope is refused
	// even where host policy would allow it.
	f.host.deny(agentA, "project-1", PermissionCreate)
	rec = f.do(&agentA, http.MethodPost, "/api/v1/artifacts?name=a.txt", []byte("x"), nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("credential without create: %d, want 403", rec.Code)
	}
	if !strings.Contains(strings.Join(f.host.calls, ","), "project-2 "+PermissionCreate) {
		t.Errorf("host not asked for %s: %v", PermissionCreate, f.host.calls)
	}
}

func TestPublishRejects(t *testing.T) {
	f := newFixture(t, false)
	for _, tc := range []struct {
		name   string
		p      *principal
		target string
		body   []byte
		hdr    map[string]string
		status int
	}{
		{"unauthenticated", nil, "/api/v1/artifacts?name=a.txt", []byte("x"), nil, 401},
		{"no name", &agentA, "/api/v1/artifacts", []byte("x"), nil, 400},
		{"path in name", &agentA, "/api/v1/artifacts?name=dir/a.txt", []byte("x"), nil, 400},
		{"dotdot name", &agentA, "/api/v1/artifacts?name=..", []byte("x"), nil, 400},
		{"backslash name", &agentA, "/api/v1/artifacts?name=a%5Cb", []byte("x"), nil, 400},
		{"long title", &agentA, "/api/v1/artifacts?name=a.txt&title=" + strings.Repeat("t", 513), []byte("x"), nil, 400},
		{"bad digest header", &agentA, "/api/v1/artifacts?name=a.txt", []byte("x"), map[string]string{HeaderContentSHA256: "abc"}, 400},
		{"digest mismatch", &agentA, "/api/v1/artifacts?name=a.txt", []byte("x"), map[string]string{HeaderContentSHA256: sha([]byte("y"))}, 400},
		{"GET collection", &agentA, "/api/v1/artifacts", nil, nil, 405},
	} {
		method := http.MethodPost
		if strings.HasPrefix(tc.name, "GET") {
			method = http.MethodGet
		}
		rec := f.do(tc.p, method, tc.target, tc.body, tc.hdr)
		if rec.Code != tc.status {
			t.Errorf("%s: status %d, want %d (%s)", tc.name, rec.Code, tc.status, rec.Body.String())
		}
	}
	// A matching digest header is accepted.
	rec := f.do(&agentA, http.MethodPost, "/api/v1/artifacts?name=a.txt", []byte("x"), map[string]string{HeaderContentSHA256: strings.ToUpper(sha([]byte("x")))})
	if rec.Code != http.StatusCreated {
		t.Errorf("matching digest: %d %s", rec.Code, rec.Body.String())
	}
	// 405 names what is allowed.
	rec = f.do(&agentA, http.MethodGet, "/api/v1/artifacts", nil, nil)
	if rec.Header().Get("Allow") != http.MethodPost {
		t.Errorf("Allow = %q", rec.Header().Get("Allow"))
	}
}

func TestPublishSizeLimit(t *testing.T) {
	f := newFixture(t, false)
	f.svc.SetLimits(func(context.Context) Limits { return Limits{MaxFileBytes: 10} })

	// Exactly at the limit is fine.
	f.publish(agentA, "ok.txt", bytes.Repeat([]byte("a"), 10), "")

	// Over the limit by declared length: rejected before anything is read.
	rec := f.do(&agentA, http.MethodPost, "/api/v1/artifacts?name=big.txt", bytes.Repeat([]byte("b"), 11), nil)
	if rec.Code != http.StatusRequestEntityTooLarge || errCode(t, rec) != "payload_too_large" {
		t.Errorf("declared oversize: %d %s", rec.Code, rec.Body.String())
	}
	// Over the limit with no declared length (chunked): caught while reading.
	r := withPrincipal(httptest.NewRequest(http.MethodPost, "/api/v1/artifacts?name=big.txt", strings.NewReader(strings.Repeat("c", 11))), agentA)
	r.ContentLength = -1
	rec = httptest.NewRecorder()
	f.svc.ServeHTTP(rec, r)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("chunked oversize: %d", rec.Code)
	}
	// Nothing over the limit reached blob storage.
	for _, b := range []string{strings.Repeat("b", 11), strings.Repeat("c", 11)} {
		if ok, _ := f.local.Exists(context.Background(), BlobPath("hub-1", sha([]byte(b)))); ok {
			t.Errorf("oversized body %q was stored", b[:1])
		}
	}
}

func TestPublishDefaultLimit(t *testing.T) {
	f := newFixture(t, false)
	f.svc.SetLimits(func(context.Context) Limits { return Limits{} })
	b, _ := f.svc.backend()
	if got := b.maxFileBytes(context.Background()); got != DefaultMaxFileBytes {
		t.Errorf("max file bytes = %d, want default %d", got, DefaultMaxFileBytes)
	}
}

func TestServiceUnconfigured(t *testing.T) {
	host := newFakeHost()
	host.allow(agentA, "project-1", PermissionCreate)
	svc := NewService(host)
	rec := httptest.NewRecorder()
	svc.ServeHTTP(rec, withPrincipal(httptest.NewRequest(http.MethodPost, "/api/v1/artifacts?name=a.txt", strings.NewReader("x")), agentA))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("publish without storage: %d, want 503", rec.Code)
	}
}

func TestParseRef(t *testing.T) {
	id := "5f1c2d3e-0000-4000-8000-000000000001"
	for in, want := range map[string]struct {
		id  string
		seq int
	}{
		"scion://artifact/" + id:        {id, 0},
		"scion://artifact/" + id + "@2": {id, 2},
		id:                              {id, 0},
		id + "@10":                      {id, 10},
		" " + id + " ":                  {id, 0},
		strings.ToUpper(id):             {id, 0},
	} {
		gotID, gotSeq, err := ParseRef(in)
		if err != nil || gotID != want.id || gotSeq != want.seq {
			t.Errorf("ParseRef(%q) = %q, %d, %v", in, gotID, gotSeq, err)
		}
	}
	for _, bad := range []string{"", "scion://artifact/", "scion://artifact/x", id + "@0", id + "@", id + "@-1", id + "/x", "http://x/" + id, "{" + id + "}"} {
		if _, _, err := ParseRef(bad); err == nil {
			t.Errorf("ParseRef(%q) accepted", bad)
		}
	}
	if FormatRef(id, 0) != "scion://artifact/"+id || FormatRef(id, 3) != "scion://artifact/"+id+"@3" {
		t.Error("FormatRef")
	}
}

func TestDetectMediaType(t *testing.T) {
	for _, tc := range []struct{ name, declared, head, want string }{
		{"a.md", "", "", "text/markdown"},
		{"A.PNG", "text/plain", "", "image/png"},
		{"noext", "text/x-custom; charset=utf-8", "", "text/x-custom"},
		{"noext", "application/octet-stream", "plain words", "text/plain"},
		{"noext", "", "\x89PNG\r\n\x1a\n", "image/png"},
		{"noext", "multipart/form-data; boundary=x", "\x00\x01", "application/octet-stream"},
	} {
		if got := detectMediaType(tc.name, tc.declared, []byte(tc.head)); got != tc.want {
			t.Errorf("detectMediaType(%q, %q) = %q, want %q", tc.name, tc.declared, got, tc.want)
		}
	}
}

func TestBackendProvider(t *testing.T) {
	f := newFixture(t, false)
	svc := NewService(f.host)
	calls := 0
	svc.SetBackendProvider(func() Backend {
		calls++
		return Backend{Store: f.store, Blobs: f.blobs, HubID: "hub-1"}
	})
	rec := httptest.NewRecorder()
	svc.ServeHTTP(rec, withPrincipal(httptest.NewRequest(http.MethodPost, "/api/v1/artifacts?name=p.txt", strings.NewReader("p")), agentA))
	if rec.Code != http.StatusCreated {
		t.Fatalf("publish through provider: %d %s", rec.Code, rec.Body.String())
	}
	if calls == 0 {
		t.Error("provider was not asked")
	}
	// A provider that has nothing yet means 503.
	svc.SetBackendProvider(func() Backend { return Backend{} })
	rec = httptest.NewRecorder()
	svc.ServeHTTP(rec, withPrincipal(httptest.NewRequest(http.MethodPost, "/api/v1/artifacts?name=p.txt", strings.NewReader("p")), agentA))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("empty provider: %d, want 503", rec.Code)
	}
}

// explainingHost is a fakeHost that reports a missing credential scope.
type explainingHost struct {
	*fakeHost
	scope string
	calls int
	// forbid fails the test on any call: set while exercising paths that
	// must never reach the explainer.
	forbid *testing.T
}

func (h *explainingHost) MissingScope(_ context.Context, permission string) string {
	h.calls++
	if h.forbid != nil {
		h.forbid.Errorf("MissingScope(%q) called on a path that must not consult it", permission)
	}
	if permission != PermissionCreate {
		return ""
	}
	return h.scope
}

func TestPublishMissingScope(t *testing.T) {
	f := newFixture(t, false)
	host := &explainingHost{fakeHost: f.host, scope: "project:artifact:read"}
	f.svc.host = host
	existing := f.publish(agentA, "a.txt", []byte("x"), "")

	// A caller the host does not serve, whose credential lacks a scope,
	// gets a 403 naming it rather than a 401.
	rec := f.do(nil, http.MethodPost, "/api/v1/artifacts?name=b.txt", []byte("x"), nil)
	if rec.Code != http.StatusForbidden || errCode(t, rec) != CodeMissingScope {
		t.Fatalf("unserved caller: %d %s", rec.Code, rec.Body.String())
	}
	var e errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || e.Error.Details["scope"] != "project:artifact:read" {
		t.Errorf("details = %+v (%v)", e.Error.Details, err)
	}

	// A served caller whose credential does not permit publishing gets the
	// same answer.
	host.scope = "project:artifact:write"
	f.host.deny(agentB, "project-1", PermissionCreate)
	rec = f.do(&agentB, http.MethodPost, "/api/v1/artifacts?name=b.txt", []byte("x"), nil)
	if rec.Code != http.StatusForbidden || errCode(t, rec) != CodeMissingScope ||
		!strings.Contains(rec.Body.String(), "project:artifact:write") {
		t.Errorf("served caller without the scope: %d %s", rec.Code, rec.Body.String())
	}

	// Without a missing scope the answers are unchanged.
	host.scope = ""
	if rec := f.do(nil, http.MethodPost, "/api/v1/artifacts?name=b.txt", []byte("x"), nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated: %d, want 401", rec.Code)
	}
	if rec := f.do(&agentB, http.MethodPost, "/api/v1/artifacts?name=b.txt", []byte("x"), nil); rec.Code != http.StatusForbidden || errCode(t, rec) != "forbidden" {
		t.Errorf("credential refused for another reason: %d %s", rec.Code, rec.Body.String())
	}

	// Reads never consult the explainer and keep their uniform 404: the
	// spy fails the test on any call.
	host.scope, host.calls, host.forbid = "project:artifact:read", 0, t
	id := existing.Artifact.ID
	for _, p := range []string{"/api/v1/artifacts/" + id, "/api/v1/artifacts/" + id + "/files/a.txt", "/api/v1/artifacts/" + id + "/versions/1/files/a.txt"} {
		if rec := f.do(nil, http.MethodGet, p, nil, nil); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: %d, want 404", p, rec.Code)
		}
	}
	// The same holds for a served caller whose credential does not
	// permit reading.
	f.host.deny(agentB, "project-1", PermissionRead)
	for _, p := range []string{"/api/v1/artifacts/" + id, "/api/v1/artifacts/" + id + "/files/a.txt", "/api/v1/artifacts/" + id + "/versions/1/files/a.txt"} {
		if rec := f.do(&agentB, http.MethodGet, p, nil, nil); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s as a caller without read: %d, want 404", p, rec.Code)
		}
	}
	if host.calls != 0 {
		t.Errorf("reads consulted the explainer %d times", host.calls)
	}
}
