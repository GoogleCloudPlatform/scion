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
	"strconv"
	"strings"
	"testing"
	"time"
)

func grantsPath(id string) string { return "/api/v1/artifacts/" + id + "/grants" }

func (f *fixture) putGrant(p principal, id, kind, ref, perm string) (*httptest.ResponseRecorder, GrantInfo) {
	f.t.Helper()
	rec := f.postJSON(&p, grantsPath(id), GrantRequest{SubjectKind: kind, SubjectRef: ref, Permission: perm})
	var g GrantInfo
	if rec.Code == http.StatusCreated || rec.Code == http.StatusOK {
		g = decodeInto[GrantResponse](f.t, rec).Grant
	}
	return rec, g
}

func (f *fixture) patch(p principal, id, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.do(&p, http.MethodPatch, "/api/v1/artifacts/"+id, []byte(body), map[string]string{"Content-Type": "application/json"})
}

// TestGrantsGiveAccess: a principal grant lets that principal read; a
// scope grant lets the scope's readers read; removing a grant takes the
// access away; a write grant lets the grantee append versions.
func TestGrantsGiveAccess(t *testing.T) {
	f, id := newLinkFixture(t)
	get := func(p principal) int { return f.do(&p, http.MethodGet, "/api/v1/artifacts/"+id, nil, nil).Code }
	if get(outside) != http.StatusNotFound || get(agentX) != http.StatusNotFound {
		t.Fatalf("readable before any grant")
	}
	rec, g := f.putGrant(userU, id, SubjectPrincipal, PrincipalRef(outside.kind, outside.ref), GrantRead)
	if rec.Code != http.StatusCreated || g.Home || g.CreatedBy != PrincipalRef(userU.kind, userU.ref) {
		t.Fatalf("principal grant: %d %+v", rec.Code, g)
	}
	if get(outside) != http.StatusOK {
		t.Errorf("principal grant does not give read")
	}
	f.host.crossScope = true
	rec, sg := f.putGrant(userU, id, SubjectScope, "project-2", GrantRead)
	if rec.Code != http.StatusCreated {
		t.Fatalf("scope grant: %d %s", rec.Code, rec.Body.String())
	}
	if get(agentX) != http.StatusOK {
		t.Errorf("scope grant does not give read to the project's agent")
	}
	if rec := f.do(&userU, http.MethodDelete, grantsPath(id)+"/"+sg.ID, nil, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", rec.Code)
	}
	if get(agentX) != http.StatusNotFound {
		t.Errorf("read kept after the grant was removed")
	}
	// Changing the permission is a 200 on the same grant.
	rec, g2 := f.putGrant(userU, id, SubjectPrincipal, PrincipalRef(outside.kind, outside.ref), GrantWrite)
	if rec.Code != http.StatusOK || g2.ID != g.ID || g2.Permission != GrantWrite {
		t.Fatalf("change permission: %d %+v", rec.Code, g2)
	}
	files := bundle{"doc.md": []byte("# v2")}
	pend := f.createPending(outside, "/api/v1/artifacts/"+id+"/versions", files.manifest("doc.md"))
	if pend.Version.Seq != 2 {
		t.Errorf("write grantee appended seq %d", pend.Version.Seq)
	}
}

// TestGrantsAdminGate: the grants routes and PATCH need canAdminister;
// a non-reader gets 404 whatever it sends.
func TestGrantsAdminGate(t *testing.T) {
	f, id := newLinkFixture(t)
	missing := f.do(&outside, http.MethodGet, grantsPath("00000000-0000-4000-8000-000000000001"), nil, nil)
	for _, tc := range []struct{ method, target, body string }{
		{http.MethodGet, grantsPath(id), ""},
		{http.MethodPost, grantsPath(id), "junk"},
		{http.MethodDelete, grantsPath(id) + "/00000000-0000-4000-8000-000000000002", ""},
		{http.MethodPatch, "/api/v1/artifacts/" + id, "junk"},
	} {
		rec := f.do(&outside, tc.method, tc.target, []byte(tc.body), nil)
		if response(rec) != response(missing) {
			t.Errorf("%s %s by a non-reader: %s", tc.method, tc.target, response(rec))
		}
		if rec := f.do(&agentA, tc.method, tc.target, []byte(tc.body), nil); rec.Code != http.StatusForbidden {
			t.Errorf("%s %s by a reading agent: %d, want 403", tc.method, tc.target, rec.Code)
		}
	}
}

// TestGrantValidation: subjects, permissions and the home grant's rules.
func TestGrantValidation(t *testing.T) {
	f, id := newLinkFixture(t)
	for _, tc := range []struct{ kind, ref, perm string }{
		{"link", "x", GrantRead},
		{SubjectPrincipal, "user-2", GrantRead},
		{SubjectPrincipal, "group:g", GrantRead},
		{SubjectPrincipal, "user:", GrantRead},
		{SubjectPrincipal, "user:a b", GrantRead},
		{SubjectPrincipal, "user:a:b", GrantRead},
		{SubjectPrincipal, "user:" + strings.Repeat("a", maxSubjectRefBytes+1), GrantRead},
		{SubjectScope, "", GrantRead},
		{SubjectScope, "p\x00", GrantRead},
		{SubjectPrincipal, "user:u9", "owner"},
		{SubjectScope, "project-1", GrantAdmin},
	} {
		if rec, _ := f.putGrant(userU, id, tc.kind, tc.ref, tc.perm); rec.Code != http.StatusBadRequest {
			t.Errorf("%+v: %d, want 400", tc, rec.Code)
		}
	}
	for _, body := range []string{"", "{}x", `{"subjectKind":"principal","subjectRef":"user:u","permission":"read","x":1}`} {
		if rec := f.do(&userU, http.MethodPost, grantsPath(id), []byte(body), nil); rec.Code != http.StatusBadRequest {
			t.Errorf("body %q: %d", body, rec.Code)
		}
	}
	// The home grant is listed first, may be raised to write, and cannot
	// be removed.
	rec, home := f.putGrant(userU, id, SubjectScope, "project-1", GrantWrite)
	if rec.Code != http.StatusOK || !home.Home || home.Permission != GrantWrite {
		t.Fatalf("raise home grant: %d %+v", rec.Code, home)
	}
	f.putGrant(userU, id, SubjectPrincipal, "agent:agent-x-uuid", GrantRead)
	list := decodeInto[GrantListResponse](t, f.do(&userU, http.MethodGet, grantsPath(id), nil, nil))
	if len(list.Grants) != 2 || !list.Grants[0].Home || list.Grants[1].SubjectRef != "agent:agent-x-uuid" {
		t.Fatalf("list = %+v", list.Grants)
	}
	if rec := f.do(&userU, http.MethodDelete, grantsPath(id)+"/"+home.ID, nil, nil); rec.Code != http.StatusConflict {
		t.Errorf("delete home grant: %d, want 409", rec.Code)
	}
	// Links are not grants here.
	_, link := f.mustMintLink(userU, id, "")
	if rec := f.do(&userU, http.MethodDelete, grantsPath(id)+"/"+link.Link.ID, nil, nil); rec.Code != http.StatusNotFound {
		t.Errorf("delete a link through grants: %d", rec.Code)
	}
	if strings.Contains(f.do(&userU, http.MethodGet, grantsPath(id), nil, nil).Body.String(), link.Link.ID) {
		t.Errorf("grants list shows a link")
	}
	if rec := f.do(&userU, http.MethodDelete, grantsPath(id)+"/nope", nil, nil); rec.Code != http.StatusNotFound {
		t.Errorf("delete malformed id: %d", rec.Code)
	}
}

// TestGrantCrossScopeGate: a scope grant to another project needs the
// host's cross-project switch; the home project never does; turning the
// switch off later leaves existing grants working.
func TestGrantCrossScopeGate(t *testing.T) {
	f, id := newLinkFixture(t)
	rec, _ := f.putGrant(userU, id, SubjectScope, "project-2", GrantRead)
	if rec.Code != http.StatusForbidden || errCode(t, rec) != "cross_project_sharing_disabled" {
		t.Fatalf("cross-project off: %d %s", rec.Code, rec.Body.String())
	}
	if rec, _ := f.putGrant(userU, id, SubjectScope, "project-1", GrantRead); rec.Code != http.StatusOK {
		t.Errorf("home project with the switch off: %d", rec.Code)
	}
	if rec, _ := f.putGrant(userU, id, SubjectPrincipal, "agent:agent-x-uuid", GrantRead); rec.Code != http.StatusCreated {
		t.Errorf("principal grant with the switch off: %d", rec.Code)
	}
	f.host.crossScope = true
	if rec, _ := f.putGrant(userU, id, SubjectScope, "project-2", GrantRead); rec.Code != http.StatusCreated {
		t.Fatalf("cross-project on: %d", rec.Code)
	}
	f.host.crossScope = false
	if rec := f.do(&agentX, http.MethodGet, "/api/v1/artifacts/"+id, nil, nil); rec.Code != http.StatusOK {
		t.Errorf("existing scope grant after the switch went off: %d", rec.Code)
	}
}

// TestGrantCap: at most MaxGrantsPerArtifact principal and scope grants.
func TestGrantCap(t *testing.T) {
	f, id := newLinkFixture(t)
	for i := 1; i < MaxGrantsPerArtifact; i++ { // the home grant is the first
		if rec, _ := f.putGrant(userU, id, SubjectPrincipal, "user:u"+strconv.Itoa(i), GrantRead); rec.Code != http.StatusCreated {
			t.Fatalf("grant %d: %d", i, rec.Code)
		}
	}
	rec, _ := f.putGrant(userU, id, SubjectPrincipal, "user:one-more", GrantRead)
	if rec.Code != http.StatusConflict || errCode(t, rec) != "too_many_grants" {
		t.Fatalf("over the cap: %d %s", rec.Code, rec.Body.String())
	}
	// Changing an existing grant is not adding one.
	if rec, _ := f.putGrant(userU, id, SubjectPrincipal, "user:u1", GrantWrite); rec.Code != http.StatusOK {
		t.Errorf("change at the cap: %d", rec.Code)
	}
	// Links do not count.
	f.mustMintLink(userU, id, "")
}

// TestPatchExpiry: set, report what it cuts, clear; the past is refused.
func TestPatchExpiry(t *testing.T) {
	f, id := newLinkFixture(t)
	f.mustMintLink(userU, id, `{"ttlHours": 48}`)
	f.mustMintLink(userU, id, `{"ttlHours": 1}`)
	f.putGrant(userU, id, SubjectPrincipal, "user:u9", GrantRead)
	exp := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	rec := f.patch(userU, id, `{"expiresAt": "`+exp.Format(time.RFC3339)+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("set expiry: %d %s", rec.Code, rec.Body.String())
	}
	resp := decodeInto[PatchArtifactResponse](t, rec)
	if resp.Artifact.ExpiresAt == nil || !resp.Artifact.ExpiresAt.Equal(exp) || resp.LinksCutShort != 1 || resp.GrantsRemoved != 2 {
		t.Errorf("set expiry = %+v (expiry %v)", resp, resp.Artifact.ExpiresAt)
	}
	got := decodeInto[ArtifactResponse](t, f.do(&userU, http.MethodGet, "/api/v1/artifacts/"+id, nil, nil))
	if got.Artifact.ExpiresAt == nil || !got.Artifact.ExpiresAt.Equal(exp) {
		t.Errorf("GET expiry %v", got.Artifact.ExpiresAt)
	}
	rec = f.patch(userU, id, `{"expiresAt": null}`)
	resp = decodeInto[PatchArtifactResponse](t, rec)
	if rec.Code != http.StatusOK || resp.Artifact.ExpiresAt != nil || resp.LinksCutShort != 0 || resp.GrantsRemoved != 0 {
		t.Errorf("clear expiry: %d %+v", rec.Code, resp)
	}
	for _, body := range []string{`{"expiresAt": "` + time.Now().Add(-time.Minute).UTC().Format(time.RFC3339) + `"}`,
		`{"expiresAt": "soon"}`, `{}`, `{"title": "x"}`, `{"scopeRef": ""}`} {
		if rec := f.patch(userU, id, body); rec.Code != http.StatusBadRequest {
			t.Errorf("patch %s: %d, want 400", body, rec.Code)
		}
	}
}

// TestPatchRehome: moving needs the publish gate in the new project; the
// old home keeps its grant; the new home gets one; the key must stay
// unique there.
func TestPatchRehome(t *testing.T) {
	f, id := newLinkFixture(t)
	// userU has no role in project-3 yet.
	if rec := f.patch(userU, id, `{"scopeRef": "project-3"}`); rec.Code != http.StatusOK {
		// Owner: Permits is enough without Authorize.
		t.Fatalf("owner rehome: %d %s", rec.Code, rec.Body.String())
	}
	got := decodeInto[ArtifactResponse](t, f.do(&userU, http.MethodGet, "/api/v1/artifacts/"+id, nil, nil))
	if got.Artifact.ScopeRef != "project-3" {
		t.Fatalf("scope after move %q", got.Artifact.ScopeRef)
	}
	list := decodeInto[GrantListResponse](t, f.do(&userU, http.MethodGet, grantsPath(id), nil, nil))
	refs := map[string]bool{}
	for _, g := range list.Grants {
		refs[g.SubjectRef+"/"+strconv.FormatBool(g.Home)] = true
	}
	if !refs["project-3/true"] || !refs["project-1/false"] || len(list.Grants) != 2 {
		t.Errorf("grants after move = %+v", list.Grants)
	}
	if rec := f.do(&agentB, http.MethodGet, "/api/v1/artifacts/"+id, nil, nil); rec.Code != http.StatusOK {
		t.Errorf("old home project lost read: %d", rec.Code)
	}

	// Credential that does not permit publishing there.
	f.host.deny(userU, "project-4", PermissionCreate)
	if rec := f.patch(userU, id, `{"scopeRef": "project-4"}`); rec.Code != http.StatusForbidden {
		t.Errorf("rehome without Permits: %d", rec.Code)
	}
	// An admin grantee who is not the owner needs Authorize there.
	admin := principal{PrincipalKindUser, "user-7", ""}
	f.host.allow(admin, "project-3", PermissionRead)
	f.putGrant(userU, id, SubjectPrincipal, PrincipalRef(admin.kind, admin.ref), GrantAdmin)
	if rec := f.patch(admin, id, `{"scopeRef": "project-5"}`); rec.Code != http.StatusForbidden {
		t.Errorf("admin without Authorize in the target: %d", rec.Code)
	}
	f.host.allow(admin, "project-5", PermissionCreate)
	if rec := f.patch(admin, id, `{"scopeRef": "project-5"}`); rec.Code != http.StatusOK {
		t.Errorf("admin with Authorize in the target: %d %s", rec.Code, rec.Body.String())
	}

	// Key clash in the target project.
	files := bundle{"k.md": []byte("k")}
	req := files.manifest("k.md")
	req.Key, req.Scope = "k", "project-1"
	a1 := f.publishBundle(userU, "/api/v1/artifacts", req, files).Artifact.ID
	req.Scope = "project-3"
	f.host.allow(userU, "project-3", PermissionRead, PermissionCreate)
	f.publishBundle(userU, "/api/v1/artifacts", req, files)
	if rec := f.patch(userU, a1, `{"scopeRef": "project-3"}`); rec.Code != http.StatusConflict {
		t.Errorf("key clash: %d", rec.Code)
	}
}

// TestRetentionAtCreation: the default retention sets the expiry of new
// artifacts, on both publish paths.
func TestRetentionAtCreation(t *testing.T) {
	f, _ := newLinkFixture(t)
	f.svc.SetLimits(func(context.Context) Limits { return Limits{DefaultRetention: 48 * time.Hour} })
	pub := f.publish(userU, "r.md", []byte("r"), "scope=project-1")
	if pub.Artifact.ExpiresAt == nil || time.Until(*pub.Artifact.ExpiresAt) < 47*time.Hour || time.Until(*pub.Artifact.ExpiresAt) > 48*time.Hour {
		t.Errorf("single-file publish expiry %v", pub.Artifact.ExpiresAt)
	}
	files := bundle{"a.md": []byte("a")}
	req := files.manifest("a.md")
	req.Scope = "project-1"
	pend := f.createPending(userU, "/api/v1/artifacts", req)
	if pend.Artifact.ExpiresAt == nil {
		t.Errorf("two-step publish has no expiry")
	}
	f.svc.SetLimits(nil)
	if pub := f.publish(userU, "n.md", []byte("n"), "scope=project-1"); pub.Artifact.ExpiresAt != nil {
		t.Errorf("no retention: expiry %v", pub.Artifact.ExpiresAt)
	}
}

// TestGrantDocumentedValues pins the values the reference documentation
// states.
func TestGrantDocumentedValues(t *testing.T) {
	if MaxGrantsPerArtifact != 100 || DefaultGCGrace != 168*time.Hour || MinGCGrace != 24*time.Hour {
		t.Errorf("documented grant or sweep values changed: update docs-site reference/artifacts.md")
	}
}

// TestGrantReadFailureIsLoud: when reading an artifact needs its grants
// and they cannot be read, the answer is 500, not 404.
func TestGrantReadFailureIsLoud(t *testing.T) {
	f, id := newLinkFixture(t)
	f.putGrant(userU, id, SubjectPrincipal, PrincipalRef(outside.kind, outside.ref), GrantRead)
	f.svc.SetStore(failGrantsStore{f.store})
	if rec := f.do(&outside, http.MethodGet, "/api/v1/artifacts/"+id, nil, nil); rec.Code != http.StatusInternalServerError {
		t.Errorf("grantee GET with grants unreadable: %d, want 500", rec.Code)
	}
	// The owner and home-project readers never need the grants.
	if rec := f.do(&userU, http.MethodGet, "/api/v1/artifacts/"+id, nil, nil); rec.Code != http.StatusOK {
		t.Errorf("owner GET: %d", rec.Code)
	}
}

// TestGrantCrossScopeNeedsHostSupport: a host without the cross-scope
// extension never allows sharing with another project.
func TestGrantCrossScopeNeedsHostSupport(t *testing.T) {
	f, id := newLinkFixture(t)
	f.host.crossScope = true
	svc := NewService(plainHost{f.host})
	svc.SetStore(f.store)
	svc.SetBlobStorage(f.blobs, "hub-1")
	r := withPrincipal(httptest.NewRequest(http.MethodPost, grantsPath(id),
		strings.NewReader(`{"subjectKind":"scope","subjectRef":"project-2","permission":"read"}`)), userU)
	rec := httptest.NewRecorder()
	svc.ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-project grant through a host without the extension: %d", rec.Code)
	}
}

// plainHost hides every optional extension of the host it wraps.
type plainHost struct{ Host }

// TestGrantDeleteMalformedIDSkipsStore: a malformed grant id never reaches
// the store.
func TestGrantDeleteMalformedIDSkipsStore(t *testing.T) {
	f, id := newLinkFixture(t)
	rs := &recordingStore{Store: f.store}
	f.svc.SetStore(rs)
	for _, bad := range []string{"nope", "%00", strings.Repeat("a", 36)} {
		if rec := f.do(&userU, http.MethodDelete, grantsPath(id)+"/"+bad, nil, nil); rec.Code != http.StatusNotFound {
			t.Errorf("%q: %d", bad, rec.Code)
		}
	}
	for _, c := range rs.take() {
		if c == "DeleteGrant" {
			t.Errorf("a malformed grant id reached the store")
		}
	}
}

// TestListAccessAndProjectShares: list rows say why the caller sees them,
// and a list narrowed to a project includes artifacts shared with it,
// marked, with shared=1 keeping only those.
func TestListAccessAndProjectShares(t *testing.T) {
	f, own := newLinkFixture(t)                                      // userU owns, homed in project-1
	proj := f.publish(agentA, "p.md", []byte("p"), "").Artifact.ID   // homed in project-1, userU reads via role
	other := f.publish(agentX, "x.md", []byte("x"), "").Artifact.ID  // homed in project-2
	f.insertScopeGrant(other, "project-1")                           // shared with project-1
	direct := f.publish(agentX, "d.md", []byte("d"), "").Artifact.ID // homed in project-2
	f.grantPrincipal(direct, userU)                                  // shared with userU directly
	access := map[string]string{}
	shared := map[string]bool{}
	for _, it := range f.list(&userU, listPath).Artifacts {
		access[it.ID] = it.Access
		shared[it.ID] = it.SharedWithScope
	}
	want := map[string]string{own: AccessOwned, proj: AccessProject, other: AccessShared, direct: AccessShared}
	for id, a := range want {
		if access[id] != a {
			t.Errorf("%s: access %q, want %q", id, access[id], a)
		}
		if shared[id] {
			t.Errorf("%s: sharedWithScope without scope=", id)
		}
	}
	ids := func(target string) map[string]bool {
		out := map[string]bool{}
		for _, it := range f.list(&userU, target).Artifacts {
			out[it.ID] = it.SharedWithScope
		}
		return out
	}
	got := ids(listPath + "&scope=project-1")
	if len(got) != 3 || got[own] || got[proj] || !got[other] {
		t.Errorf("scope=project-1: %v", got)
	}
	got = ids(listPath + "&scope=project-1&shared=1")
	if len(got) != 1 || !got[other] {
		t.Errorf("scope=project-1&shared=1: %v", got)
	}
	// An expired scope grant does not count.
	f.exec(t, `UPDATE artifact_grant SET expires_at = ? WHERE artifact_id = ? AND subject_kind = 'scope' AND subject_ref = 'project-1'`, linkPast(), other)
	if got := ids(listPath + "&scope=project-1&shared=1"); len(got) != 0 {
		t.Errorf("expired scope grant listed: %v", got)
	}
	for _, q := range []string{"&shared=1", "&scope=project-1&shared=maybe"} {
		if rec := f.do(&userU, http.MethodGet, listPath+q, nil, nil); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", q, rec.Code)
		}
	}
}
