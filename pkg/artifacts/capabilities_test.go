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
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// capabilityCase is one caller and the grants and credential it has on
// userU's artifact (newLinkFixture: homed in project-1, with the home
// project's read grant).
type capabilityCase struct {
	name   string
	caller principal
	// setup adds grants or credential limits.
	setup func(f *fixture, id string)
	// publish is the expected canPublish.
	publish bool
}

func insertGrant(f *fixture, id, kind, ref, perm string, expires *time.Time) {
	f.t.Helper()
	var exp any
	if expires != nil {
		exp = expires.UTC().Format(sqliteTimeLayout)
	}
	f.exec(f.t, `INSERT INTO artifact_grant (id, artifact_id, subject_kind, subject_ref, permission, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, "g-"+kind+"-"+ref, id, kind, ref, perm, exp, linkNow())
}

func capabilityCases() []capabilityCase {
	principalGrant := func(p principal, perm string) func(*fixture, string) {
		return func(f *fixture, id string) {
			insertGrant(f, id, SubjectPrincipal, PrincipalRef(p.kind, p.ref), perm, nil)
		}
	}
	past := time.Now().Add(-time.Hour)
	return []capabilityCase{
		{name: "owner", caller: userU, publish: true},
		{name: "owner whose credential does not permit publishing", caller: userU, publish: false,
			setup: func(f *fixture, _ string) { f.host.deny(userU, "project-1", PermissionCreate) }},
		{name: "home-project reader", caller: agentB, publish: false},
		{name: "home-project write grant", caller: agentB, publish: true,
			setup: func(f *fixture, id string) {
				f.exec(f.t, `UPDATE artifact_grant SET permission = 'write' WHERE artifact_id = ? AND subject_kind = 'scope' AND subject_ref = 'project-1'`, id)
			}},
		{name: "expired principal write grant", caller: agentB, publish: false,
			setup: func(f *fixture, id string) {
				insertGrant(f, id, SubjectPrincipal, PrincipalRef(agentB.kind, agentB.ref), GrantWrite, &past)
			}},
		{name: "read grantee", caller: outside, publish: false, setup: principalGrant(outside, GrantRead)},
		{name: "write grantee", caller: outside, publish: true, setup: principalGrant(outside, GrantWrite)},
		{name: "admin grantee", caller: outside, publish: true, setup: principalGrant(outside, GrantAdmin)},
		{name: "write grantee whose credential does not permit publishing", caller: outside, publish: false,
			setup: func(f *fixture, id string) {
				principalGrant(outside, GrantWrite)(f, id)
				f.host.deny(outside, "project-1", PermissionCreate)
			}},
		{name: "agent of a project with a write scope grant", caller: agentX, publish: true,
			setup: func(f *fixture, id string) { insertGrant(f, id, SubjectScope, "project-2", GrantWrite, nil) }},
		{name: "agent of a project with a read scope grant", caller: agentX, publish: false,
			setup: func(f *fixture, id string) { insertGrant(f, id, SubjectScope, "project-2", GrantRead, nil) }},
	}
}

// TestCanPublishCapability: GET of the artifact and of a version states
// canPublish true for the owner and write or admin grantees, false for
// readers without publish rights, and omits it from their JSON.
func TestCanPublishCapability(t *testing.T) {
	for _, tc := range capabilityCases() {
		t.Run(tc.name, func(t *testing.T) {
			f, id := newLinkFixture(t)
			if tc.setup != nil {
				tc.setup(f, id)
			}
			for _, target := range []string{"/api/v1/artifacts/" + id, "/api/v1/artifacts/" + id + "/versions/1"} {
				rec := f.do(&tc.caller, http.MethodGet, target, nil, nil)
				if rec.Code != http.StatusOK {
					t.Fatalf("GET %s: %d %s", target, rec.Code, rec.Body.String())
				}
				if got := decodeInto[ArtifactResponse](t, rec).CanPublish; got != tc.publish {
					t.Errorf("GET %s canPublish = %v, want %v", target, got, tc.publish)
				}
				if !tc.publish && strings.Contains(rec.Body.String(), `"canPublish"`) {
					t.Errorf("GET %s: canPublish present when false: %s", target, rec.Body.String())
				}
			}
		})
	}
}

// TestCanPublishMatchesWriteRoute: for every case, canPublish is exactly
// the decision POST /{id}/versions makes for the same caller (201 when
// true, 403 when false). It fails if the field's check and the route's
// check diverge.
func TestCanPublishMatchesWriteRoute(t *testing.T) {
	files := bundle{"doc.md": []byte("# v2")}
	for _, tc := range capabilityCases() {
		t.Run(tc.name, func(t *testing.T) {
			f, id := newLinkFixture(t)
			if tc.setup != nil {
				tc.setup(f, id)
			}
			get := f.do(&tc.caller, http.MethodGet, "/api/v1/artifacts/"+id, nil, nil)
			if get.Code != http.StatusOK {
				t.Fatalf("GET: %d", get.Code)
			}
			field := decodeInto[ArtifactResponse](t, get).CanPublish
			post := f.postJSON(&tc.caller, "/api/v1/artifacts/"+id+"/versions", files.manifest("doc.md"))
			want := http.StatusForbidden
			if field {
				want = http.StatusCreated
			}
			if post.Code != want {
				t.Errorf("canPublish = %v but POST /versions answered %d %s", field, post.Code, post.Body.String())
			}
		})
	}
}

// TestCanPublishGrantReadFailureIsLoud: when stating canPublish needs the
// grants and they cannot be read, GET answers 500 like canManage does,
// never a 200 with canPublish false.
func TestCanPublishGrantReadFailureIsLoud(t *testing.T) {
	f, id := newLinkFixture(t)
	f.svc.SetStore(failGrantsStore{f.store})
	// agentB reads through its home project and is not a user (so
	// canAdminister needs no grants); only the publish decision reads them.
	for _, target := range []string{"/api/v1/artifacts/" + id, "/api/v1/artifacts/" + id + "/versions/1"} {
		rec := f.do(&agentB, http.MethodGet, target, nil, nil)
		if rec.Code != http.StatusInternalServerError || errCode(t, rec) != "internal" {
			t.Errorf("GET %s with grants unreadable: %d %s, want 500 internal", target, rec.Code, rec.Body.String())
		}
	}
	// The owner's decision needs no grants.
	if rec := f.do(&userU, http.MethodGet, "/api/v1/artifacts/"+id, nil, nil); rec.Code != http.StatusOK || !decodeInto[ArtifactResponse](t, rec).CanPublish {
		t.Errorf("owner GET with grants unreadable: %d %s", rec.Code, rec.Body.String())
	}
}

// TestCanPublishUnreadableIsMissing: a caller that cannot read the
// artifact gets the same 404 as for a missing one; the capability adds
// nothing for it.
func TestCanPublishUnreadableIsMissing(t *testing.T) {
	f, id := newLinkFixture(t)
	missing := f.do(&outside, http.MethodGet, "/api/v1/artifacts/00000000-0000-4000-8000-000000000001", nil, nil)
	got := f.do(&outside, http.MethodGet, "/api/v1/artifacts/"+id, nil, nil)
	if got.Code != http.StatusNotFound || response(got) != response(missing) || got.Body.String() != missing.Body.String() {
		t.Errorf("unreadable GET %d %q differs from missing %d %q", got.Code, got.Body.String(), missing.Code, missing.Body.String())
	}
}

// TestWriteRoutesGrantReadFailureIsLoud: a write route whose decision
// needs the grants answers 500 when they cannot be read, never the 403 a
// working read might not give. agentB reads through its home project, so
// only the write decision reads the grants.
func TestWriteRoutesGrantReadFailureIsLoud(t *testing.T) {
	files := bundle{"doc.md": []byte("# v2")}
	f, id := newLinkFixture(t)
	versions := "/api/v1/artifacts/" + id + "/versions"
	// With the grants readable, agentB may read but not write.
	if rec := f.postJSON(&agentB, versions, files.manifest("doc.md")); rec.Code != http.StatusForbidden {
		t.Fatalf("POST /versions with grants readable: %d, want 403", rec.Code)
	}
	f.svc.SetStore(failGrantsStore{f.store})
	for _, tc := range []struct {
		name string
		do   func() *httptest.ResponseRecorder
	}{
		{"POST /versions", func() *httptest.ResponseRecorder {
			return f.postJSON(&agentB, versions, files.manifest("doc.md"))
		}},
		{"PUT file", func() *httptest.ResponseRecorder {
			return f.do(&agentB, http.MethodPut, versions+"/2/files/doc.md", files["doc.md"], nil)
		}},
		{"finalize", func() *httptest.ResponseRecorder {
			return f.do(&agentB, http.MethodPost, versions+"/2/finalize", nil, nil)
		}},
	} {
		if rec := tc.do(); rec.Code != http.StatusInternalServerError || errCode(t, rec) != "internal" {
			t.Errorf("%s with grants unreadable: %d %s, want 500 internal", tc.name, rec.Code, rec.Body.String())
		}
	}
	// The owner's decision needs no grants.
	if rec := f.postJSON(&userU, versions, files.manifest("doc.md")); rec.Code != http.StatusCreated {
		t.Errorf("owner POST /versions with grants unreadable: %d %s", rec.Code, rec.Body.String())
	}
}

// TestAppendVersionGrantReadFailureIsLoud: appendVersion's own write check
// (it runs again after writableArtifact, and alone on a keyed append)
// answers 500 on a failed grant read, with and without a known Permits
// answer.
func TestAppendVersionGrantReadFailureIsLoud(t *testing.T) {
	files := bundle{"doc.md": []byte("# v2")}
	f, id := newLinkFixture(t)
	a, err := f.store.GetArtifact(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	f.svc.SetStore(failGrantsStore{f.store})
	b, _ := f.svc.backend()
	permitted := true
	for _, p := range []*bool{nil, &permitted} {
		req := files.manifest("doc.md")
		r := withPrincipal(httptest.NewRequest(http.MethodPost, "/api/v1/artifacts/"+id+"/versions", nil), agentB)
		rec := httptest.NewRecorder()
		f.svc.appendVersion(rec, r, b, a, &req, p)
		if rec.Code != http.StatusInternalServerError || errCode(t, rec) != "internal" {
			t.Errorf("appendVersion (permitted known: %v) with grants unreadable: %d %s, want 500 internal", p != nil, rec.Code, rec.Body.String())
		}
	}
}
