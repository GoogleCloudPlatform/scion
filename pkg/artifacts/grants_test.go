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

// TestPatchRehome: a move needs the publish gate in the new project for
// every caller (owner included) and the cross-project switch; the old home
// loses its grant; the new home gets one; the key must stay unique there;
// the grant cap holds. The fake host's Permits answers true everywhere
// unless denied, as the hub's does for a session user, so Authorize is the
// gate that decides.
func TestPatchRehome(t *testing.T) {
	f, id := newLinkFixture(t)
	// The owner has no role in project-3: refused, switch on or off.
	f.host.crossScope = true
	if rec := f.patch(userU, id, `{"scopeRef": "project-3"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("owner without a role in the target: %d %s", rec.Code, rec.Body.String())
	}
	f.host.allow(userU, "project-3", PermissionRead, PermissionCreate)
	// With a role but the switch off: refused.
	f.host.crossScope = false
	rec := f.patch(userU, id, `{"scopeRef": "project-3"}`)
	if rec.Code != http.StatusForbidden || errCode(t, rec) != "cross_project_sharing_disabled" {
		t.Fatalf("switch off: %d %s", rec.Code, rec.Body.String())
	}
	f.host.crossScope = true
	if rec := f.patch(userU, id, `{"scopeRef": "project-3"}`); rec.Code != http.StatusOK {
		t.Fatalf("owner with a role, switch on: %d %s", rec.Code, rec.Body.String())
	}
	got := decodeInto[ArtifactResponse](t, f.do(&userU, http.MethodGet, "/api/v1/artifacts/"+id, nil, nil))
	if got.Artifact.ScopeRef != "project-3" {
		t.Fatalf("scope after move %q", got.Artifact.ScopeRef)
	}
	list := decodeInto[GrantListResponse](t, f.do(&userU, http.MethodGet, grantsPath(id), nil, nil))
	if len(list.Grants) != 1 || list.Grants[0].SubjectRef != "project-3" || !list.Grants[0].Home {
		t.Errorf("grants after move = %+v, want only the new home", list.Grants)
	}
	if rec := f.do(&agentB, http.MethodGet, "/api/v1/artifacts/"+id, nil, nil); rec.Code != http.StatusNotFound {
		t.Errorf("old home project kept read: %d", rec.Code)
	}

	// The credential must permit publishing there.
	f.host.allow(userU, "project-4", PermissionCreate)
	f.host.deny(userU, "project-4", PermissionCreate)
	if rec := f.patch(userU, id, `{"scopeRef": "project-4"}`); rec.Code != http.StatusForbidden {
		t.Errorf("rehome without Permits: %d", rec.Code)
	}
	// An admin grantee needs Authorize there too.
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

	// The grant cap holds for the new home grant.
	f.host.allow(userU, "project-5", PermissionRead, PermissionCreate, PermissionManage)
	f.host.allow(userU, "project-6", PermissionCreate)
	for i := 2; i < MaxGrantsPerArtifact; i++ { // home + admin grantee already
		if rec, _ := f.putGrant(userU, id, SubjectPrincipal, "user:c"+strconv.Itoa(i), GrantRead); rec.Code != http.StatusCreated {
			t.Fatalf("grant %d: %d", i, rec.Code)
		}
	}
	if rec, _ := f.putGrant(userU, id, SubjectPrincipal, "user:extra", GrantRead); rec.Code != http.StatusConflict {
		t.Fatalf("at the cap: %d", rec.Code)
	}
	// Moving drops the old home (one slot) and adds the new one, so a move
	// at the cap is allowed.
	if rec := f.patch(userU, id, `{"scopeRef": "project-6"}`); rec.Code != http.StatusOK {
		t.Errorf("move at the cap: %d %s", rec.Code, rec.Body.String())
	}

	// Key clash in the target project.
	files := bundle{"k.md": []byte("k")}
	req := files.manifest("k.md")
	req.Key, req.Scope = "k", "project-1"
	a1 := f.publishBundle(userU, "/api/v1/artifacts", req, files).Artifact.ID
	req.Scope = "project-3"
	f.publishBundle(userU, "/api/v1/artifacts", req, files)
	if rec := f.patch(userU, a1, `{"scopeRef": "project-3"}`); rec.Code != http.StatusConflict {
		t.Errorf("key clash: %d", rec.Code)
	}
}

// TestPatchRehomeCap: a move that would put the artifact over the grant
// cap answers 409 too_many_grants and changes nothing.
func TestPatchRehomeCap(t *testing.T) {
	f, id := newLinkFixture(t)
	f.host.crossScope = true
	f.host.allow(userU, "project-3", PermissionRead, PermissionCreate)
	f.host.allow(userU, "project-4", PermissionRead, PermissionCreate)
	// At the cap (home grant plus principal grants), a move still fits:
	// the old home's grant goes as the new one comes.
	for i := 1; i < MaxGrantsPerArtifact; i++ {
		f.putGrant(userU, id, SubjectPrincipal, "user:c"+strconv.Itoa(i), GrantRead)
	}
	if rec := f.patch(userU, id, `{"scopeRef": "project-3"}`); rec.Code != http.StatusOK {
		t.Fatalf("move at the cap: %d %s", rec.Code, rec.Body.String())
	}
	// One grant past the cap (written directly), so the next move would
	// have to add one more.
	f.exec(t, `INSERT INTO artifact_grant (id, artifact_id, subject_kind, subject_ref, permission, created_at)
		VALUES ('g-over', ?, 'principal', 'user:over', 'read', ?)`, id, linkNow())
	rec := f.patch(userU, id, `{"scopeRef": "project-4"}`)
	if rec.Code != http.StatusConflict || errCode(t, rec) != "too_many_grants" {
		t.Fatalf("move over the cap: %d %s", rec.Code, rec.Body.String())
	}
	got := decodeInto[ArtifactResponse](t, f.do(&userU, http.MethodGet, "/api/v1/artifacts/"+id, nil, nil))
	if got.Artifact.ScopeRef != "project-3" {
		t.Errorf("a refused move changed the home to %q", got.Artifact.ScopeRef)
	}
}

// TestDeleteHomeGrantAfterMove: after a move, the new home's grant is the
// protected one, decided in the store under the artifact's lock.
func TestDeleteHomeGrantAfterMove(t *testing.T) {
	f, id := newLinkFixture(t)
	f.host.crossScope = true
	f.host.allow(userU, "project-3", PermissionRead, PermissionCreate)
	if rec := f.patch(userU, id, `{"scopeRef": "project-3"}`); rec.Code != http.StatusOK {
		t.Fatalf("move: %d", rec.Code)
	}
	list := decodeInto[GrantListResponse](t, f.do(&userU, http.MethodGet, grantsPath(id), nil, nil))
	rec := f.do(&userU, http.MethodDelete, grantsPath(id)+"/"+list.Grants[0].ID, nil, nil)
	if rec.Code != http.StatusConflict || errCode(t, rec) != "home_grant" {
		t.Errorf("delete the new home grant: %d %s", rec.Code, rec.Body.String())
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

// moveFirstStore moves the artifact to a new home just before PutGrant
// runs, as a concurrent re-home would.
type moveFirstStore struct {
	Store
	to string
}

func (m *moveFirstStore) PutGrant(ctx context.Context, g *Grant, max int, cross bool) (bool, error) {
	if _, err := m.Store.UpdateArtifact(ctx, g.ArtifactID, ArtifactUpdate{
		HomeGrant: &Grant{ID: "moved-" + g.ID, ArtifactID: g.ArtifactID, SubjectKind: SubjectScope, SubjectRef: m.to,
			Permission: GrantRead, CreatedAt: time.Now()}, MaxGrants: max}); err != nil {
		return false, err
	}
	return m.Store.PutGrant(ctx, g, max, cross)
}

// TestGrantHomeRulesUnderLock: the home project's read/write-only rule
// and the cross-project switch are decided against the home as it is when
// the grant is written, not as the handler last saw it.
func TestGrantHomeRulesUnderLock(t *testing.T) {
	f, id := newLinkFixture(t)
	f.host.crossScope = true
	f.svc.SetStore(&moveFirstStore{Store: f.store, to: "project-3"})
	// project-3 was not home when the handler looked, but is when the
	// grant is written: admin is refused.
	if rec, _ := f.putGrant(userU, id, SubjectScope, "project-3", GrantAdmin); rec.Code != http.StatusBadRequest {
		t.Errorf("admin grant to the new home: %d, want 400", rec.Code)
	}
	// The old home (project-1) is now another project: with the switch
	// off, a grant to it is refused even though the handler saw it as home.
	f.svc.SetStore(&moveFirstStore{Store: f.store, to: "project-4"})
	f.host.crossScope = false
	rec, _ := f.putGrant(userU, id, SubjectScope, "project-3", GrantRead)
	if rec.Code != http.StatusForbidden || errCode(t, rec) != "cross_project_sharing_disabled" {
		t.Errorf("grant to the old home after a move with the switch off: %d %s", rec.Code, rec.Body.String())
	}
}

// TestPatchRehomeTargetAdminGrant: moving to a project that holds an
// admin grant on the artifact is refused until it is lowered.
func TestPatchRehomeTargetAdminGrant(t *testing.T) {
	f, id := newLinkFixture(t)
	f.host.crossScope = true
	f.host.allow(userU, "project-3", PermissionRead, PermissionCreate)
	if rec, _ := f.putGrant(userU, id, SubjectScope, "project-3", GrantAdmin); rec.Code != http.StatusCreated {
		t.Fatalf("admin grant to project-3: %d", rec.Code)
	}
	rec := f.patch(userU, id, `{"scopeRef": "project-3"}`)
	if rec.Code != http.StatusConflict || errCode(t, rec) != "home_admin_grant" {
		t.Fatalf("move onto an admin grant: %d %s", rec.Code, rec.Body.String())
	}
	f.putGrant(userU, id, SubjectScope, "project-3", GrantWrite)
	if rec := f.patch(userU, id, `{"scopeRef": "project-3"}`); rec.Code != http.StatusOK {
		t.Errorf("move after lowering: %d %s", rec.Code, rec.Body.String())
	}
}

// TestPatchSameScopeIsNotAMove: naming the current home in scopeRef does
// not run the move gate, so an admin without a publishing role can change
// the expiry with the same body.
func TestPatchSameScopeIsNotAMove(t *testing.T) {
	f, id := newLinkFixture(t)
	admin := principal{PrincipalKindUser, "user-7", ""}
	f.host.allow(admin, "project-1", PermissionRead) // no create
	f.putGrant(userU, id, SubjectPrincipal, PrincipalRef(admin.kind, admin.ref), GrantAdmin)
	exp := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	rec := f.patch(admin, id, `{"scopeRef": "project-1", "expiresAt": "`+exp+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("same-scope patch: %d %s", rec.Code, rec.Body.String())
	}
	if got := decodeInto[PatchArtifactResponse](t, rec); got.Artifact.ExpiresAt == nil || got.Artifact.ScopeRef != "project-1" {
		t.Errorf("same-scope patch result %+v", got)
	}
}
