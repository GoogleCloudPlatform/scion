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

	"github.com/google/uuid"
)

// Review grants on agent-owned artifacts (design D24, ptone/scion#4014).

var (
	delegator  = principal{PrincipalKindUser, "user-delegator", ""}
	projAdmin  = principal{PrincipalKindUser, "user-admin", ""}
	member     = principal{PrincipalKindUser, "user-member", ""}
	reviewer   = principal{PrincipalKindUser, "user-reviewer", ""}
	otherProj  = principal{PrincipalKindUser, "user-other", ""}
	reviewerID = PrincipalRef(reviewer.kind, reviewer.ref)
)

// newReviewGrantFixture publishes an artifact owned by agentA homed in
// project-1. delegator stands for agentA's delegating user and projAdmin
// for an admin of project-1 (the host's ReviewGrantAuthority answers yes
// for both); member reads project-1 but is neither; otherProj reads only
// project-2.
func newReviewGrantFixture(t *testing.T) (*fixture, string) {
	t.Helper()
	f, _ := newLinkFixture(t)
	id := f.publish(agentA, "review.md", []byte("# draft"), "scope=project-1").Artifact.ID
	for _, p := range []principal{delegator, projAdmin, member} {
		f.host.allow(p, "project-1", PermissionRead)
	}
	f.host.allow(otherProj, "project-2", PermissionRead)
	f.host.allowReview(delegator, agentA.ref, "project-1")
	f.host.allowReview(projAdmin, agentA.ref, "project-1")
	return f, id
}

func reviewBody(kind, ref, perm string) string {
	return `{"subjectKind":"` + kind + `","subjectRef":"` + ref + `","permission":"` + perm + `"}`
}

func (f *fixture) postGrantBody(p principal, id, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.do(&p, http.MethodPost, grantsPath(id), []byte(body), map[string]string{"Content-Type": "application/json"})
}

func (f *fixture) grantOf(id, subject string) *Grant {
	f.t.Helper()
	gs, err := f.store.ListGrants(context.Background(), id)
	if err != nil {
		f.t.Fatal(err)
	}
	for i := range gs {
		if gs[i].SubjectKind == SubjectPrincipal && gs[i].SubjectRef == subject {
			return &gs[i]
		}
	}
	return nil
}

// TestReviewGrantAllowed: the delegating user and a home-project admin may
// give a user a write grant on an agent-owned artifact; the grantee can
// then read it and add a version.
func TestReviewGrantAllowed(t *testing.T) {
	for _, granter := range []principal{delegator, projAdmin} {
		t.Run(granter.ref, func(t *testing.T) {
			f, id := newReviewGrantFixture(t)
			rec := f.postGrantBody(granter, id, reviewBody(SubjectPrincipal, reviewerID, GrantWrite))
			if rec.Code != http.StatusCreated {
				t.Fatalf("review grant: %d %s", rec.Code, rec.Body.String())
			}
			g := decodeInto[GrantResponse](t, rec).Grant
			if g.Permission != GrantWrite || g.SubjectRef != reviewerID || g.CreatedBy != PrincipalRef(granter.kind, granter.ref) {
				t.Errorf("grant: %+v", g)
			}
			if rec := f.do(&reviewer, http.MethodGet, "/api/v1/artifacts/"+id, nil, nil); rec.Code != http.StatusOK {
				t.Errorf("reviewer GET: %d", rec.Code)
			}
			files := bundle{"review.md": []byte("# with comments")}
			if pend := f.createPending(reviewer, "/api/v1/artifacts/"+id+"/versions", files.manifest("review.md")); pend.Version.Seq != 2 {
				t.Errorf("reviewer appended seq %d", pend.Version.Seq)
			}
			// Repeating it is a 200 on the same grant; raising a read
			// grant to write is allowed too.
			if rec := f.postGrantBody(granter, id, reviewBody(SubjectPrincipal, reviewerID, GrantWrite)); rec.Code != http.StatusOK {
				t.Errorf("repeat: %d", rec.Code)
			}
			if _, err := f.store.PutGrant(context.Background(), &Grant{ID: uuid.NewString(), ArtifactID: id,
				SubjectKind: SubjectPrincipal, SubjectRef: "user:raised", Permission: GrantRead, CreatedAt: time.Now()}, 100, false); err != nil {
				t.Fatal(err)
			}
			if rec := f.postGrantBody(granter, id, reviewBody(SubjectPrincipal, "user:raised", GrantWrite)); rec.Code != http.StatusOK {
				t.Errorf("raise read to write: %d", rec.Code)
			}
			if g := f.grantOf(id, "user:raised"); g == nil || g.Permission != GrantWrite {
				t.Errorf("raised grant: %+v", g)
			}
		})
	}
}

// TestReviewGrantDenied: every other caller or request gets exactly the
// 403 a non-administrator got before review grants existed, and nothing
// is written.
func TestReviewGrantDenied(t *testing.T) {
	ok := reviewBody(SubjectPrincipal, reviewerID, GrantWrite)
	for _, tc := range []struct {
		name   string
		caller principal
		body   string
		setup  func(f *fixture, id string)
	}{
		{"owning agent", agentA, ok, func(f *fixture, id string) { f.host.allowReview(agentA, agentA.ref, "project-1") }},
		{"another agent", agentB, ok, func(f *fixture, id string) { f.host.allowReview(agentB, agentA.ref, "project-1") }},
		{"project member", member, ok, nil},
		{"user from another project with read", otherProj, ok, func(f *fixture, id string) {
			f.grantPrincipal(id, otherProj)
		}},
		{"revoked delegation", delegator, ok, func(f *fixture, id string) { f.host.revokeReview(delegator, agentA.ref, "project-1") }},
		{"authority for another agent", member, ok, func(f *fixture, id string) { f.host.allowReview(member, agentB.ref, "project-1") }},
		{"authority in another project", member, ok, func(f *fixture, id string) { f.host.allowReview(member, agentA.ref, "project-2") }},
		{"credential lacks manage", delegator, ok, func(f *fixture, id string) { f.host.deny(delegator, "project-1", PermissionManage) }},
		{"admin grant", delegator, reviewBody(SubjectPrincipal, reviewerID, GrantAdmin), nil},
		{"read grant", delegator, reviewBody(SubjectPrincipal, reviewerID, GrantRead), nil},
		{"grant to an agent", delegator, reviewBody(SubjectPrincipal, PrincipalRef(agentB.kind, agentB.ref), GrantWrite), nil},
		{"grant to a scope", projAdmin, reviewBody(SubjectScope, "project-2", GrantWrite), func(f *fixture, id string) { f.host.crossScope = true }},
		{"scope kind naming a user", delegator, reviewBody(SubjectScope, reviewerID, GrantWrite), nil},
		{"malformed subject", delegator, reviewBody(SubjectPrincipal, "user:", GrantWrite), nil},
		{"unknown field", delegator, `{"subjectKind":"principal","subjectRef":"user:r","permission":"write","x":1}`, nil},
		{"trailing data", delegator, ok + ` {}`, nil},
		{"junk", delegator, "junk", nil},
		{"too large", delegator, `{"subjectKind":"principal","subjectRef":"user:r","permission":"write"}` + strings.Repeat(" ", maxGrantRequestBytes), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, id := newReviewGrantFixture(t)
			// The baseline: a reader that is not an administrator.
			want := response(f.postGrantBody(member, id, "junk"))
			if tc.setup != nil {
				tc.setup(f, id)
			}
			before, _ := f.store.ListGrants(context.Background(), id)
			rec := f.postGrantBody(tc.caller, id, tc.body)
			if got := response(rec); got != want {
				t.Errorf("got\n%s\nwant\n%s", got, want)
			}
			after, _ := f.store.ListGrants(context.Background(), id)
			if len(after) != len(before) {
				t.Errorf("grants %d → %d", len(before), len(after))
			}
		})
	}
}

// TestReviewGrantNonReader: a caller that cannot read the artifact gets the
// 404 of a missing artifact, even with review authority, and the authority
// is never asked.
func TestReviewGrantNonReader(t *testing.T) {
	f, id := newReviewGrantFixture(t)
	f.host.allowReview(otherProj, agentA.ref, "project-1")
	ok := reviewBody(SubjectPrincipal, reviewerID, GrantWrite)
	missing := f.postGrantBody(otherProj, "00000000-0000-4000-8000-000000000001", ok)
	f.host.calls = nil
	if rec := f.postGrantBody(otherProj, id, ok); response(rec) != response(missing) {
		t.Errorf("non-reader: %s", response(rec))
	}
	for _, c := range f.host.calls {
		if strings.HasPrefix(c, "review ") {
			t.Errorf("review authority asked for a non-reader")
		}
	}
}

// TestReviewGrantUserOwnedUnchanged: on a user-owned artifact review
// authority changes nothing: a non-owner still gets the same 403, and the
// authority is never asked.
func TestReviewGrantUserOwnedUnchanged(t *testing.T) {
	f, _ := newReviewGrantFixture(t)
	id := f.publish(userU, "mine.md", []byte("# mine"), "scope=project-1").Artifact.ID
	f.host.allowReview(member, userU.ref, "project-1")
	want := response(f.postGrantBody(member, id, "junk"))
	f.host.calls = nil
	if got := response(f.postGrantBody(member, id, reviewBody(SubjectPrincipal, reviewerID, GrantWrite))); got != want {
		t.Errorf("user-owned: %s", got)
	}
	for _, c := range f.host.calls {
		if strings.HasPrefix(c, "review ") {
			t.Errorf("review authority asked for a user-owned artifact")
		}
	}
	// The owner keeps every grant it had.
	if rec, _ := f.putGrant(userU, id, SubjectPrincipal, reviewerID, GrantAdmin); rec.Code != http.StatusCreated {
		t.Errorf("owner admin grant: %d", rec.Code)
	}
}

// TestReviewGrantNeverLowersAdmin: a user who already holds an admin grant
// keeps it; the review grant is refused with the same 403.
func TestReviewGrantNeverLowersAdmin(t *testing.T) {
	f, id := newReviewGrantFixture(t)
	if _, err := f.store.PutGrant(context.Background(), &Grant{ID: uuid.NewString(), ArtifactID: id,
		SubjectKind: SubjectPrincipal, SubjectRef: reviewerID, Permission: GrantAdmin, CreatedAt: time.Now()}, 100, false); err != nil {
		t.Fatal(err)
	}
	want := response(f.postGrantBody(member, id, "junk"))
	if got := response(f.postGrantBody(delegator, id, reviewBody(SubjectPrincipal, reviewerID, GrantWrite))); got != want {
		t.Errorf("lowering admin: %s", got)
	}
	if g := f.grantOf(id, reviewerID); g == nil || g.Permission != GrantAdmin {
		t.Errorf("admin grant changed: %+v", g)
	}
}

// TestReviewGrantNeedsHostSupport: a host without ReviewGrantAuthority
// allows no review grant.
func TestReviewGrantNeedsHostSupport(t *testing.T) {
	f, id := newReviewGrantFixture(t)
	want := response(f.postGrantBody(member, id, "junk"))
	svc := NewService(plainHost{f.host})
	svc.SetStore(f.store)
	svc.SetBlobStorage(f.blobs, "hub-1")
	r := withPrincipal(httptest.NewRequest(http.MethodPost, grantsPath(id),
		strings.NewReader(reviewBody(SubjectPrincipal, reviewerID, GrantWrite))), delegator)
	rec := httptest.NewRecorder()
	svc.ServeHTTP(rec, r)
	if got := response(rec); got != want {
		t.Errorf("host without the extension: %s", got)
	}
}

// movingReviewStore moves the artifact to another home just before
// PutReviewGrant runs, as a concurrent re-home would.
type movingReviewStore struct {
	Store
	to string
}

func (m *movingReviewStore) PutReviewGrant(ctx context.Context, g *Grant, max int, home string) (bool, error) {
	if _, err := m.UpdateArtifact(ctx, g.ArtifactID, ArtifactUpdate{
		HomeGrant: &Grant{ID: "moved-" + g.ID, ArtifactID: g.ArtifactID, SubjectKind: SubjectScope, SubjectRef: m.to,
			Permission: GrantRead, CreatedAt: time.Now()}, MaxGrants: max}); err != nil {
		return false, err
	}
	return m.Store.PutReviewGrant(ctx, g, max, home)
}

// TestReviewGrantHomeUnderLock: authority checked against the old home
// does not carry over to an artifact moved meanwhile.
func TestReviewGrantHomeUnderLock(t *testing.T) {
	f, id := newReviewGrantFixture(t)
	want := response(f.postGrantBody(member, id, "junk"))
	f.svc.SetStore(&movingReviewStore{Store: f.store, to: "project-3"})
	if got := response(f.postGrantBody(projAdmin, id, reviewBody(SubjectPrincipal, reviewerID, GrantWrite))); got != want {
		t.Errorf("after a concurrent move: %s", got)
	}
	if g := f.grantOf(id, reviewerID); g != nil {
		t.Errorf("grant written: %+v", g)
	}
}
