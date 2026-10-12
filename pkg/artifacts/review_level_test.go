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
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The review grant level (ptone/scion#4538): a review grant lets a user who
// can already read an artifact append versions of kind review, and nothing
// else. It is never a read path itself.

// rvOnly reads project-1 (the home of a reviewFixture artifact) and holds
// a review grant on it: a review-only caller.
var rvOnly = principal{PrincipalKindUser, "user-rv", ""}

// newReviewLevelFixture is a reviewFixture (v1 of plan.md and chart.png,
// owned by agentA, homed in project-1) where rvOnly reads project-1 and
// holds a review grant.
func newReviewLevelFixture(t *testing.T) *reviewFixture {
	t.Helper()
	f := newReviewFixture(t)
	f.host.allow(rvOnly, "project-1", PermissionRead)
	f.grant(rvOnly, GrantReview)
	return f
}

// setLevel changes p's principal grant on the fixture's artifact.
func (f *reviewFixture) setLevel(p principal, perm string) {
	f.t.Helper()
	if _, err := f.db.Exec("UPDATE artifact_grant SET permission = ? WHERE artifact_id = ? AND subject_ref = ?",
		perm, f.id, PrincipalRef(p.kind, p.ref)); err != nil {
		f.t.Fatal(err)
	}
}

// dropRead takes p's read in project-1 away, its only other read path.
func (f *reviewFixture) dropRead(p principal) {
	f.t.Helper()
	f.host.mu.Lock()
	defer f.host.mu.Unlock()
	delete(f.host.perms[PrincipalRef(p.kind, p.ref)], "project-1")
}

// startPublish creates a pending publish version of the fixture's
// artifact for p and uploads every file; it returns its seq.
func (f *reviewFixture) startPublish(p principal, files bundle) int {
	f.t.Helper()
	pend := f.createPending(p, "/api/v1/artifacts/"+f.id+"/versions", files.manifest("plan.md"))
	for _, path := range pend.Upload.Required {
		if rec := f.put(p, f.id, pend.Version.Seq, path, files[path]); rec.Code != http.StatusNoContent {
			f.t.Fatalf("PUT %s: %d %s", path, rec.Code, rec.Body.String())
		}
	}
	return pend.Version.Seq
}

func wantReviewOnly(t *testing.T, what string, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusForbidden || errCode(t, rec) != CodeReviewOnly {
		t.Errorf("%s: %d %s, want 403 %s", what, rec.Code, rec.Body.String(), CodeReviewOnly)
	}
}

// TestReviewLevelCanReview: a review-only caller reads the artifact, sees
// canReview (not canPublish), and starts, uploads and finalizes a review.
func TestReviewLevelCanReview(t *testing.T) {
	f := newReviewLevelFixture(t)
	rec := f.do(&rvOnly, http.MethodGet, "/api/v1/artifacts/"+f.id, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: %d %s", rec.Code, rec.Body.String())
	}
	if resp := decodeInto[ArtifactResponse](t, rec); resp.CanPublish || !resp.CanReview || resp.CanManage {
		t.Errorf("capabilities: publish=%v review=%v manage=%v, want false true false", resp.CanPublish, resp.CanReview, resp.CanManage)
	}
	seq, r := f.review(rvOnly, with(f.v1, "plan.md", reviewMarked), "plan.md")
	if r.code != http.StatusOK {
		t.Fatalf("finalize review: %d %s", r.code, r.body)
	}
	if f.current() != seq || f.state(seq) != VersionStateReady {
		t.Errorf("review seq %d: current %d state %s", seq, f.current(), f.state(seq))
	}
}

// TestReviewLevelRefusedPublishAtStart: a review-only caller cannot start
// a publish version; nothing is recorded.
func TestReviewLevelRefusedPublishAtStart(t *testing.T) {
	f := newReviewLevelFixture(t)
	for _, kind := range []string{"", VersionKindPublish} {
		req := with(f.v1, "plan.md", "# Replaced\n").manifest("plan.md")
		req.Kind = kind
		wantReviewOnly(t, "start kind "+kind, f.postJSON(&rvOnly, "/api/v1/artifacts/"+f.id+"/versions", req))
	}
	if vs, err := f.store.ListVersions(context.Background(), f.id, 0, 100); err != nil || len(vs) != 1 {
		t.Errorf("versions after refused starts: %d %v", len(vs), err)
	}
}

// TestReviewLevelRefusedPublishAtUpload: a publish version started while
// the caller held write access takes no upload once only a review grant
// is left, even after the caller started a review meanwhile.
func TestReviewLevelRefusedPublishAtUpload(t *testing.T) {
	f := newReviewLevelFixture(t)
	f.setLevel(rvOnly, GrantWrite)
	files := with(f.v1, "plan.md", "# Replaced\n")
	pend := f.createPending(rvOnly, "/api/v1/artifacts/"+f.id+"/versions", files.manifest("plan.md"))
	f.setLevel(rvOnly, GrantReview)
	f.startReview(rvOnly, with(f.v1, "plan.md", reviewMarked), "plan.md")
	wantReviewOnly(t, "upload to a publish version", f.put(rvOnly, f.id, pend.Version.Seq, "plan.md", files["plan.md"]))
	if fl, err := f.store.GetFile(context.Background(), pendingID(t, f, pend.Version.Seq), "plan.md"); err != nil || !fl.Pending {
		t.Errorf("file received after the refusal: %+v %v", fl, err)
	}
}

// TestReviewLevelRefusedPublishAtFinalize: a complete publish version
// started with write access cannot be finalized with only a review grant;
// it stays pending and unclaimed, and the current version is unchanged.
func TestReviewLevelRefusedPublishAtFinalize(t *testing.T) {
	f := newReviewLevelFixture(t)
	f.setLevel(rvOnly, GrantWrite)
	seq := f.startPublish(rvOnly, with(f.v1, "plan.md", "# Replaced\n"))
	f.setLevel(rvOnly, GrantReview)
	wantReviewOnly(t, "finalize a publish version", f.finalize(rvOnly, f.id, seq))
	if f.state(seq) != VersionStatePending || f.current() != 1 {
		t.Errorf("after the refusal: state %s current %d", f.state(seq), f.current())
	}
}

func pendingID(t *testing.T, f *reviewFixture, seq int) string {
	t.Helper()
	v, err := f.store.GetVersion(context.Background(), f.id, seq)
	if err != nil {
		t.Fatal(err)
	}
	return v.ID
}

// TestReviewLevelNoOtherPowers: a review-only caller cannot set a title
// or key, give grants, create share links, change the expiry, move or
// delete the artifact.
func TestReviewLevelNoOtherPowers(t *testing.T) {
	f := newReviewLevelFixture(t)
	f.host.crossScope = true
	f.host.allow(rvOnly, "project-2", PermissionRead, PermissionCreate)
	base := "/api/v1/artifacts/" + f.id
	review := with(f.v1, "plan.md", reviewMarked).manifest("plan.md")
	review.Kind = VersionKindReview
	titled, keyed := review, review
	titled.Title, keyed.Key = "New title", "new-key"
	for _, req := range []CreateVersionRequest{titled, keyed} {
		if rec := f.postJSON(&rvOnly, base+"/versions", req); rec.Code != http.StatusBadRequest {
			t.Errorf("title/key: %d %s", rec.Code, rec.Body.String())
		}
	}
	hdr := map[string]string{"Content-Type": "application/json"}
	for _, tc := range []struct {
		name, method, target, body string
	}{
		{"grant read", http.MethodPost, base + "/grants", reviewBody(SubjectPrincipal, "user:friend", GrantRead)},
		{"grant review", http.MethodPost, base + "/grants", reviewBody(SubjectPrincipal, "user:friend", GrantReview)},
		{"share link", http.MethodPost, base + "/links", `{}`},
		{"expiry", http.MethodPatch, base, `{"expiresAt":"` + time.Now().Add(48*time.Hour).UTC().Format(time.RFC3339) + `"}`},
		{"move", http.MethodPatch, base, `{"scopeRef":"project-2"}`},
		{"delete", http.MethodDelete, base, ``},
	} {
		before, _ := f.store.ListGrants(context.Background(), f.id)
		rec := f.do(&rvOnly, tc.method, tc.target, []byte(tc.body), hdr)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: %d %s, want 403", tc.name, rec.Code, rec.Body.String())
		}
		after, _ := f.store.ListGrants(context.Background(), f.id)
		if len(after) != len(before) {
			t.Errorf("%s: grants %d → %d", tc.name, len(before), len(after))
		}
	}
	a, err := f.store.GetArtifact(context.Background(), f.id)
	if err != nil || a.ScopeRef != "project-1" || a.ExpiresAt != nil || a.Title != "plan.md" || a.Key != "" {
		t.Errorf("artifact changed: %+v %v", a, err)
	}
}

// TestReviewLevelGrantAloneIsNotRead: a review grant with no other read
// path reads nothing: GET answers the missing-artifact 404, and the list
// leaves the artifact out.
func TestReviewLevelGrantAloneIsNotRead(t *testing.T) {
	f := newReviewLevelFixture(t)
	f.dropRead(rvOnly)
	missing := f.do(&rvOnly, http.MethodGet, "/api/v1/artifacts/00000000-0000-4000-8000-000000000001", nil, nil)
	got := f.do(&rvOnly, http.MethodGet, "/api/v1/artifacts/"+f.id, nil, nil)
	if response(got) != response(missing) {
		t.Errorf("GET with a review grant only:\n%s\nwant\n%s", response(got), response(missing))
	}
	list := f.do(&rvOnly, http.MethodGet, "/api/v1/artifacts", nil, nil)
	if list.Code != http.StatusOK || strings.Contains(list.Body.String(), f.id) {
		t.Errorf("list with a review grant only: %d %s", list.Code, list.Body.String())
	}
}

// TestReviewLevelLostReadIsMissing: once a review-only caller loses its
// other read path, every route (GET, start, upload, finalize) answers
// byte for byte the 404 of a missing artifact, never 403 or review_only,
// and nothing changes. The read check runs before the write check.
func TestReviewLevelLostReadIsMissing(t *testing.T) {
	f := newReviewLevelFixture(t)
	marked := with(f.v1, "plan.md", reviewMarked)
	// A review started while the caller could read, with a file left to upload.
	req := marked.manifest("plan.md")
	req.Kind = VersionKindReview
	pend := f.createPending(rvOnly, "/api/v1/artifacts/"+f.id+"/versions", req)
	// A complete review, ready to finalize.
	done := f.startReview(rvOnly, marked, "plan.md")
	f.dropRead(rvOnly)

	const gone = "00000000-0000-4000-8000-000000000001"
	routes := func(id string, upSeq, finSeq int) []*httptest.ResponseRecorder {
		return []*httptest.ResponseRecorder{
			f.do(&rvOnly, http.MethodGet, "/api/v1/artifacts/"+id, nil, nil),
			f.postJSON(&rvOnly, "/api/v1/artifacts/"+id+"/versions", req),
			f.put(rvOnly, id, upSeq, "plan.md", marked["plan.md"]),
			f.do(&rvOnly, http.MethodPost, fmt.Sprintf("/api/v1/artifacts/%s/versions/%d/finalize", id, finSeq),
				[]byte(fmt.Sprintf(`{"base":%d}`, 1)), nil),
		}
	}
	want := routes(gone, pend.Version.Seq, done)
	got := routes(f.id, pend.Version.Seq, done)
	for i, name := range []string{"GET", "start", "upload", "finalize"} {
		if want[i].Code != http.StatusNotFound || response(got[i]) != response(want[i]) || got[i].Body.String() != want[i].Body.String() {
			t.Errorf("%s after losing read:\n%s%s\nwant\n%s%s", name, response(got[i]), got[i].Body.String(),
				response(want[i]), want[i].Body.String())
		}
	}
	if f.current() != 1 || f.state(done) != VersionStatePending {
		t.Errorf("after the refusals: current %d, review state %s", f.current(), f.state(done))
	}
	// Getting the read path back restores review access: nothing was
	// revoked, the decision is made again on every use.
	f.host.allow(rvOnly, "project-1", PermissionRead)
	if r := f.result(f.finalizeReview(rvOnly, done, 1)); r.code != http.StatusOK {
		t.Errorf("finalize after read returns: %d %s", r.code, r.body)
	}
}

// TestReviewLevelWriteWins: a caller whose grant is raised from review to
// write is a full writer (canPublish, no canReview).
func TestReviewLevelWriteWins(t *testing.T) {
	f := newReviewLevelFixture(t)
	f.setLevel(rvOnly, GrantWrite)
	resp := decodeInto[ArtifactResponse](t, f.do(&rvOnly, http.MethodGet, "/api/v1/artifacts/"+f.id, nil, nil))
	if !resp.CanPublish || resp.CanReview {
		t.Errorf("write grantee: publish=%v review=%v", resp.CanPublish, resp.CanReview)
	}
}

// reviewLevelCase is one caller of a reviewFixture artifact.
type reviewLevelCase struct {
	name            string
	caller          principal
	setup           func(f *reviewFixture)
	publish, review bool
}

func reviewLevelCases() []reviewLevelCase {
	past := time.Now().Add(-time.Hour).UTC().Format(sqliteTimeLayout)
	return []reviewLevelCase{
		{name: "owner", caller: agentA, publish: true},
		{name: "write grantee", caller: userU, publish: true},
		{name: "review grantee who reads", caller: rvOnly, review: true},
		{name: "review grantee whose credential does not permit publishing", caller: rvOnly,
			setup: func(f *reviewFixture) { f.host.deny(rvOnly, "project-1", PermissionCreate) }},
		{name: "expired review grant", caller: rvOnly,
			setup: func(f *reviewFixture) {
				if _, err := f.db.Exec("UPDATE artifact_grant SET expires_at = ? WHERE subject_ref = ?", past, PrincipalRef(rvOnly.kind, rvOnly.ref)); err != nil {
					f.t.Fatal(err)
				}
			}},
		{name: "reader without a grant", caller: rvOnly, setup: func(f *reviewFixture) { f.setLevel(rvOnly, GrantRead) }},
		{name: "review grantee also holding write", caller: rvOnly, publish: true,
			setup: func(f *reviewFixture) { f.setLevel(rvOnly, GrantWrite) }},
	}
}

// TestCanReviewCapability: GET of the artifact and of a version states
// canReview only for a reader whose access allows review versions and no
// more, and omits it from everyone else's JSON.
func TestCanReviewCapability(t *testing.T) {
	for _, tc := range reviewLevelCases() {
		t.Run(tc.name, func(t *testing.T) {
			f := newReviewLevelFixture(t)
			if tc.setup != nil {
				tc.setup(f)
			}
			for _, target := range []string{"/api/v1/artifacts/" + f.id, "/api/v1/artifacts/" + f.id + "/versions/1"} {
				rec := f.do(&tc.caller, http.MethodGet, target, nil, nil)
				if rec.Code != http.StatusOK {
					t.Fatalf("GET %s: %d %s", target, rec.Code, rec.Body.String())
				}
				resp := decodeInto[ArtifactResponse](t, rec)
				if resp.CanPublish != tc.publish || resp.CanReview != tc.review {
					t.Errorf("GET %s: canPublish=%v canReview=%v, want %v %v", target, resp.CanPublish, resp.CanReview, tc.publish, tc.review)
				}
				if resp.CanPublish && resp.CanReview {
					t.Errorf("GET %s: canPublish and canReview both true", target)
				}
				if !tc.review && strings.Contains(rec.Body.String(), `"canReview"`) {
					t.Errorf("GET %s: canReview present when false", target)
				}
			}
		})
	}
}

// TestCanReviewMatchesReviewFinalize: for every case, an actual review
// (start, upload, finalize) by the caller succeeds exactly when GET states
// canPublish or canReview, and a publish version can be started exactly
// when it states canPublish. It fails if the capabilities and the write
// routes ever disagree.
func TestCanReviewMatchesReviewFinalize(t *testing.T) {
	for _, tc := range reviewLevelCases() {
		t.Run(tc.name, func(t *testing.T) {
			f := newReviewLevelFixture(t)
			if tc.setup != nil {
				tc.setup(f)
			}
			resp := decodeInto[ArtifactResponse](t, f.do(&tc.caller, http.MethodGet, "/api/v1/artifacts/"+f.id, nil, nil))
			req := with(f.v1, "plan.md", reviewMarked).manifest("plan.md")
			req.Kind = VersionKindReview
			start := f.postJSON(&tc.caller, "/api/v1/artifacts/"+f.id+"/versions", req)
			reviewed := false
			if start.Code == http.StatusCreated {
				pend := decodeInto[PendingVersionResponse](t, start)
				marked := with(f.v1, "plan.md", reviewMarked)
				for _, path := range pend.Upload.Required {
					if rec := f.put(tc.caller, f.id, pend.Version.Seq, path, marked[path]); rec.Code != http.StatusNoContent {
						t.Fatalf("PUT %s: %d %s", path, rec.Code, rec.Body.String())
					}
				}
				r := f.result(f.finalizeReview(tc.caller, pend.Version.Seq, 1))
				reviewed = r.code == http.StatusOK
				if !reviewed {
					t.Fatalf("review finalize: %d %s", r.code, r.body)
				}
			} else if start.Code != http.StatusForbidden {
				t.Fatalf("review start: %d %s", start.Code, start.Body.String())
			}
			if reviewed != (resp.CanPublish || resp.CanReview) {
				t.Errorf("canPublish=%v canReview=%v but the review was finalized=%v", resp.CanPublish, resp.CanReview, reviewed)
			}
			pub := f.postJSON(&tc.caller, "/api/v1/artifacts/"+f.id+"/versions", with(f.v1, "plan.md", "# New\n").manifest("plan.md"))
			if (pub.Code == http.StatusCreated) != resp.CanPublish {
				t.Errorf("canPublish=%v but starting a publish version answered %d %s", resp.CanPublish, pub.Code, pub.Body.String())
			}
		})
	}
}

// TestReviewCapabilityNeedsRead: canReview is never stated without the
// read check, whatever the write decision says.
func TestReviewCapabilityNeedsRead(t *testing.T) {
	for _, access := range []writeAccess{accessNone, accessReviewGrant, accessWriteGrant, accessOwner} {
		if p, r := reviewCapability(false, access); p || r {
			t.Errorf("unreadable, access %d: publish=%v review=%v", access, p, r)
		}
	}
	if p, r := reviewCapability(true, accessReviewGrant); p || !r {
		t.Errorf("readable review grantee: publish=%v review=%v", p, r)
	}
}

// TestCanReviewGrantReadFailureIsLoud: a failed grant read while stating
// a review grantee's rights answers 500, never a 200 without canReview.
func TestCanReviewGrantReadFailureIsLoud(t *testing.T) {
	f := newReviewLevelFixture(t)
	f.svc.SetStore(failGrantsStore{f.store})
	for _, target := range []string{"/api/v1/artifacts/" + f.id, "/api/v1/artifacts/" + f.id + "/versions/1"} {
		rec := f.do(&rvOnly, http.MethodGet, target, nil, nil)
		if rec.Code != http.StatusInternalServerError || errCode(t, rec) != "internal" {
			t.Errorf("GET %s with grants unreadable: %d %s", target, rec.Code, rec.Body.String())
		}
	}
}

// TestAdminGrantRouteReview: an administrator may give review to a user
// principal only; review for an agent, another scope or the home scope is
// refused with 400.
func TestAdminGrantRouteReview(t *testing.T) {
	f, id := newLinkFixture(t) // owned by userU, homed in project-1
	f.host.crossScope = true
	if rec, g := f.putGrant(userU, id, SubjectPrincipal, "user:friend", GrantReview); rec.Code != http.StatusCreated || g.Permission != GrantReview {
		t.Errorf("review to a user: %d %+v", rec.Code, g)
	}
	for _, tc := range []struct{ kind, ref string }{
		{SubjectPrincipal, PrincipalRef(agentB.kind, agentB.ref)},
		{SubjectScope, "project-2"},
		{SubjectScope, "project-1"},
	} {
		if rec, _ := f.putGrant(userU, id, tc.kind, tc.ref, GrantReview); rec.Code != http.StatusBadRequest {
			t.Errorf("review to %s %s: %d %s", tc.kind, tc.ref, rec.Code, rec.Body.String())
		}
	}
}

// TestReviewGrantSubjectCheckFailureIsLoud: when the subject read check
// cannot be answered, the review grant answers 500 and writes nothing.
func TestReviewGrantSubjectCheckFailureIsLoud(t *testing.T) {
	f, id := newReviewGrantFixture(t)
	f.host.subjectErr = errors.New("lookup failed")
	rec := f.postGrantBody(delegator, id, reviewBody(SubjectPrincipal, reviewerID, GrantReview))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("subject check failing: %d %s", rec.Code, rec.Body.String())
	}
	if g := f.grantOf(id, reviewerID); g != nil {
		t.Errorf("grant written: %+v", g)
	}
}

// TestReviewGrantNeedsSubjectSupport: a host without ReviewSubjectAuthority
// gives no review grant through the D24 path (fail closed), with the same
// 403 as every other refusal.
func TestReviewGrantNeedsSubjectSupport(t *testing.T) {
	f, id := newReviewGrantFixture(t)
	want := response(f.postGrantBody(member, id, "junk"))
	svc := NewService(noSubjectHost{f.host})
	svc.SetStore(f.store)
	svc.SetBlobStorage(f.blobs, "hub-1")
	r := withPrincipal(httptest.NewRequest(http.MethodPost, grantsPath(id),
		strings.NewReader(reviewBody(SubjectPrincipal, reviewerID, GrantReview))), delegator)
	rec := httptest.NewRecorder()
	svc.ServeHTTP(rec, r)
	if got := response(rec); got != want {
		t.Errorf("host without the subject check: %s", got)
	}
	if g := f.grantOf(id, reviewerID); g != nil {
		t.Errorf("grant written: %+v", g)
	}
}

// noSubjectHost is the fake host with ReviewGrantAuthority and without
// ReviewSubjectAuthority.
type noSubjectHost struct{ Host }

func (h noSubjectHost) MayGrantReview(ctx context.Context, ownerAgentID, homeScope string) bool {
	return h.Host.(ReviewGrantAuthority).MayGrantReview(ctx, ownerAgentID, homeScope)
}

// TestReviewSubjectParity: the D24 subject check and a real GET by the
// same user agree, allow and deny alike: a home member, a read grantee
// and a member of a project with a scope grant can read and may receive
// review; an outsider and a user holding only a review grant can do
// neither.
func TestReviewSubjectParity(t *testing.T) {
	f, id := newReviewGrantFixture(t)
	f.host.crossScope = true
	homeMember := principal{PrincipalKindUser, "user-home", ""}
	readGrantee := principal{PrincipalKindUser, "user-granted", ""}
	scopeMember := principal{PrincipalKindUser, "user-scope", ""}
	stranger := principal{PrincipalKindUser, "user-stranger", ""}
	reviewOnly := principal{PrincipalKindUser, "user-review-only", ""}
	f.host.allow(homeMember, "project-1", PermissionRead)
	f.grantPrincipal(id, readGrantee)
	f.host.allow(scopeMember, "project-2", PermissionRead)
	insertGrant(f, id, SubjectScope, "project-2", GrantRead, nil)
	insertGrant(f, id, SubjectPrincipal, PrincipalRef(reviewOnly.kind, reviewOnly.ref), GrantReview, nil)
	for _, tc := range []struct {
		p    principal
		want bool
	}{
		{homeMember, true}, {readGrantee, true}, {scopeMember, true}, {stranger, false}, {reviewOnly, false},
	} {
		t.Run(tc.p.ref, func(t *testing.T) {
			get := f.do(&tc.p, http.MethodGet, "/api/v1/artifacts/"+id, nil, nil)
			reads := get.Code == http.StatusOK
			if !reads && get.Code != http.StatusNotFound {
				t.Fatalf("GET: %d", get.Code)
			}
			b, _ := f.svc.backend()
			a, err := f.store.GetArtifact(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			subject, err := f.svc.reviewSubjectCanRead(context.Background(), b, a, PrincipalRef(tc.p.kind, tc.p.ref))
			if err != nil {
				t.Fatal(err)
			}
			if reads != tc.want || subject != tc.want {
				t.Errorf("GET reads=%v, subject check=%v, want both %v", reads, subject, tc.want)
			}
		})
	}
}

// TestWriteDecisionReviewNeedsRead: the write decision itself, not only
// the routes' read check before it, gives a review grantee review access
// only while it can read the artifact another way.
func TestWriteDecisionReviewNeedsRead(t *testing.T) {
	f := newReviewLevelFixture(t)
	b, _ := f.svc.backend()
	a, err := f.store.GetArtifact(context.Background(), f.id)
	if err != nil {
		t.Fatal(err)
	}
	ctx := withPrincipal(httptest.NewRequest(http.MethodGet, "/", nil), rvOnly).Context()
	if access, err := f.svc.canWriteErr(ctx, b, a); err != nil || access != accessReviewGrant {
		t.Fatalf("reader with a review grant: %v %v", access, err)
	}
	f.dropRead(rvOnly)
	if access, err := f.svc.canWriteErr(ctx, b, a); err != nil || access != accessNone {
		t.Errorf("review grant without read: %v %v, want accessNone", access, err)
	}
}
