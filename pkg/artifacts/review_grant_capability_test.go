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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// canGrantReview on GET of an artifact and of a version (design D24,
// ptone/scion#4014; ptone/scion#4593).

// reviewCapabilityCase is one caller on an artifact of
// newReviewGrantFixture (owned by agentA, homed in project-1).
type reviewCapabilityCase struct {
	name   string
	caller principal
	// setup changes the fixture; it returns the artifact to use, or "" for
	// the fixture's agent-owned one.
	setup func(f *fixture, id string) string
	// manage and review are the expected canManage and canGrantReview.
	manage, review bool
}

func reviewCapabilityCases() []reviewCapabilityCase {
	userOwned := func(f *fixture, _ string) string {
		id := f.publish(userU, "mine.md", []byte("# mine"), "scope=project-1").Artifact.ID
		return id
	}
	return []reviewCapabilityCase{
		{name: "delegating user", caller: delegator, review: true},
		{name: "home-project admin", caller: projAdmin, review: true},
		{name: "owning agent", caller: agentA},
		{name: "owning agent the authority would answer yes for", caller: agentA,
			setup: func(f *fixture, _ string) string { f.host.allowReview(agentA, agentA.ref, "project-1"); return "" }},
		{name: "project member", caller: member},
		{name: "user from another project with a read grant", caller: otherProj,
			setup: func(f *fixture, id string) string { f.grantPrincipal(id, otherProj); return "" }},
		{name: "revoked delegation", caller: delegator,
			setup: func(f *fixture, _ string) string { f.host.revokeReview(delegator, agentA.ref, "project-1"); return "" }},
		{name: "credential lacks manage", caller: delegator,
			setup: func(f *fixture, _ string) string { f.host.deny(delegator, "project-1", PermissionManage); return "" }},
		{name: "admin grantee with review authority", caller: delegator, manage: true,
			setup: func(f *fixture, id string) string {
				insertGrant(f, id, SubjectPrincipal, PrincipalRef(delegator.kind, delegator.ref), GrantAdmin, nil)
				return ""
			}},
		{name: "admin grantee", caller: member, manage: true,
			setup: func(f *fixture, id string) string {
				insertGrant(f, id, SubjectPrincipal, PrincipalRef(member.kind, member.ref), GrantAdmin, nil)
				return ""
			}},
		{name: "owner of a user-owned artifact", caller: userU, manage: true, setup: userOwned},
		{name: "authority on a user-owned artifact", caller: member,
			setup: func(f *fixture, id string) string {
				id = userOwned(f, id)
				f.host.allowReview(member, userU.ref, "project-1")
				return id
			}},
	}
}

func (tc reviewCapabilityCase) prepare(t *testing.T) (*fixture, string) {
	t.Helper()
	f, id := newReviewGrantFixture(t)
	if tc.setup != nil {
		if other := tc.setup(f, id); other != "" {
			id = other
		}
	}
	return f, id
}

// TestCanGrantReviewCapability: GET of the artifact and of a version
// states canGrantReview true only for the delegating user and a
// home-project admin who cannot manage the artifact, and omits it from
// everyone else's JSON.
func TestCanGrantReviewCapability(t *testing.T) {
	for _, tc := range reviewCapabilityCases() {
		t.Run(tc.name, func(t *testing.T) {
			f, id := tc.prepare(t)
			for _, target := range []string{"/api/v1/artifacts/" + id, "/api/v1/artifacts/" + id + "/versions/1"} {
				rec := f.do(&tc.caller, http.MethodGet, target, nil, nil)
				if rec.Code != http.StatusOK {
					t.Fatalf("GET %s: %d %s", target, rec.Code, rec.Body.String())
				}
				resp := decodeInto[ArtifactResponse](t, rec)
				if resp.CanManage != tc.manage || resp.CanGrantReview != tc.review {
					t.Errorf("GET %s: canManage=%v canGrantReview=%v, want %v %v",
						target, resp.CanManage, resp.CanGrantReview, tc.manage, tc.review)
				}
				if !tc.review && strings.Contains(rec.Body.String(), `"canGrantReview"`) {
					t.Errorf("GET %s: canGrantReview present when false: %s", target, rec.Body.String())
				}
			}
		})
	}
}

// TestCanGrantReviewNeedsHostSupport: a host without ReviewGrantAuthority
// states canGrantReview for nobody, as it allows no review grant.
func TestCanGrantReviewNeedsHostSupport(t *testing.T) {
	f, id := newReviewGrantFixture(t)
	svc := NewService(plainHost{f.host})
	svc.SetStore(f.store)
	svc.SetBlobStorage(f.blobs, "hub-1")
	rec := httptest.NewRecorder()
	svc.ServeHTTP(rec, withPrincipal(httptest.NewRequest(http.MethodGet, "/api/v1/artifacts/"+id, nil), delegator))
	if rec.Code != http.StatusOK || decodeInto[ArtifactResponse](t, rec).CanGrantReview {
		t.Errorf("host without the extension: %d %s", rec.Code, rec.Body.String())
	}
}

// TestCanGrantReviewMatchesGrantRoute: for every case, the capabilities on
// GET are exactly what POST /{id}/grants decides for a user review grant
// from the same caller: it succeeds when canManage or canGrantReview is
// true and is refused with 403 otherwise, and the two are never both true.
// It fails if the field's check and the route's check diverge.
func TestCanGrantReviewMatchesGrantRoute(t *testing.T) {
	for _, tc := range reviewCapabilityCases() {
		t.Run(tc.name, func(t *testing.T) {
			f, id := tc.prepare(t)
			get := f.do(&tc.caller, http.MethodGet, "/api/v1/artifacts/"+id, nil, nil)
			if get.Code != http.StatusOK {
				t.Fatalf("GET: %d", get.Code)
			}
			resp := decodeInto[ArtifactResponse](t, get)
			if resp.CanManage && resp.CanGrantReview {
				t.Errorf("canManage and canGrantReview both true")
			}
			post := f.postGrantBody(tc.caller, id, reviewBody(SubjectPrincipal, reviewerID, GrantReview))
			granted := post.Code == http.StatusCreated || post.Code == http.StatusOK
			if !granted && post.Code != http.StatusForbidden {
				t.Fatalf("POST /grants: %d %s", post.Code, post.Body.String())
			}
			if granted != (resp.CanManage || resp.CanGrantReview) {
				t.Errorf("canManage=%v canGrantReview=%v but POST /grants answered %d %s",
					resp.CanManage, resp.CanGrantReview, post.Code, post.Body.String())
			}
		})
	}
}

// TestCanGrantReviewGrantReadFailureIsLoud: when the grants cannot be read
// while stating the caller's rights, GET answers 500 for a caller with
// review authority, as for canManage and canPublish, never a 200 that
// hides the capability.
func TestCanGrantReviewGrantReadFailureIsLoud(t *testing.T) {
	f, id := newReviewGrantFixture(t)
	f.svc.SetStore(failGrantsStore{f.store})
	for _, target := range []string{"/api/v1/artifacts/" + id, "/api/v1/artifacts/" + id + "/versions/1"} {
		rec := f.do(&delegator, http.MethodGet, target, nil, nil)
		if rec.Code != http.StatusInternalServerError || errCode(t, rec) != "internal" {
			t.Errorf("GET %s with grants unreadable: %d %s, want 500 internal", target, rec.Code, rec.Body.String())
		}
	}
}

// TestCanGrantReviewUnreadableIsMissing: a caller that cannot read the
// artifact gets the same 404 as for a missing one, even with review
// authority, and the authority is never asked.
func TestCanGrantReviewUnreadableIsMissing(t *testing.T) {
	f, id := newReviewGrantFixture(t)
	f.host.allowReview(otherProj, agentA.ref, "project-1")
	missing := f.do(&otherProj, http.MethodGet, "/api/v1/artifacts/00000000-0000-4000-8000-000000000001", nil, nil)
	for _, target := range []string{"/api/v1/artifacts/" + id, "/api/v1/artifacts/" + id + "/versions/1"} {
		f.host.calls = nil
		got := f.do(&otherProj, http.MethodGet, target, nil, nil)
		if got.Code != http.StatusNotFound || got.Body.String() != missing.Body.String() {
			t.Errorf("unreadable GET %s: %d %q differs from missing %d %q",
				target, got.Code, got.Body.String(), missing.Code, missing.Body.String())
		}
		for _, c := range f.host.calls {
			if strings.HasPrefix(c, "review ") {
				t.Errorf("GET %s: review authority asked for a non-reader", target)
			}
		}
	}
}

// TestReviewGrantRouteChecksBodyBeforeAuthority: sharing the decision with
// canGrantReview keeps the grant route's order: a malformed body is
// refused before the host's review authority is asked.
func TestReviewGrantRouteChecksBodyBeforeAuthority(t *testing.T) {
	f, id := newReviewGrantFixture(t)
	f.host.calls = nil
	if rec := f.postGrantBody(delegator, id, "junk"); rec.Code != http.StatusForbidden {
		t.Fatalf("junk body: %d", rec.Code)
	}
	for _, c := range f.host.calls {
		if strings.HasPrefix(c, "review ") {
			t.Errorf("review authority asked before the body was checked")
		}
	}
}
