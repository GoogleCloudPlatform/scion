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
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestGrantLadderTable (K1, ptone/scion#5145): the grant levels form a
// ladder, read ⊂ review ⊂ write ⊂ admin. A user whose only access to an
// agent-owned artifact is a principal grant at one level reads it at every
// level, may publish reviews from review up, ordinary versions from write
// up, and manage it only with admin. The list includes the artifact at
// every level (pushdown parity with the read decision).
func TestGrantLadderTable(t *testing.T) {
	for _, tc := range []struct {
		level                   string
		review, publish, manage bool
	}{
		{GrantRead, false, false, false},
		{GrantReview, true, false, false},
		{GrantWrite, false, true, false},
		{GrantAdmin, false, true, true},
	} {
		t.Run(tc.level, func(t *testing.T) {
			f, id := newReviewGrantFixture(t)
			// otherProj reads only project-2, so the grant is its one read path.
			insertGrant(f, id, SubjectPrincipal, PrincipalRef(otherProj.kind, otherProj.ref), tc.level, nil)
			rec := f.do(&otherProj, http.MethodGet, "/api/v1/artifacts/"+id, nil, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET with a %s grant only: %d %s", tc.level, rec.Code, rec.Body.String())
			}
			resp := decodeInto[ArtifactResponse](t, rec)
			if resp.CanReview != tc.review || resp.CanPublish != tc.publish || resp.CanManage != tc.manage {
				t.Errorf("%s: canReview=%v canPublish=%v canManage=%v, want %v %v %v",
					tc.level, resp.CanReview, resp.CanPublish, resp.CanManage, tc.review, tc.publish, tc.manage)
			}
			list := f.do(&otherProj, http.MethodGet, "/api/v1/artifacts?mine=1", nil, nil)
			if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), id) {
				t.Errorf("%s: list leaves the artifact out: %d %s", tc.level, list.Code, list.Body.String())
			}
		})
	}
}

// listed reports whether the artifact list at target, as p, includes id.
func listed(t *testing.T, f *fixture, p principal, target, id string) bool {
	t.Helper()
	rec := f.do(&p, http.MethodGet, target, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", target, rec.Code, rec.Body.String())
	}
	return strings.Contains(rec.Body.String(), id)
}

// TestListPushdownParityByLevel: the list's SQL pushdowns agree with the
// read decision at every grant level. For a user whose only access is a
// principal grant, the artifact is in the list both with mine=1 and with
// mine=1&scope=<its home project> (the home-scope pushdown) exactly when GET
// reads it, and absent from both with no grant.
func TestListPushdownParityByLevel(t *testing.T) {
	for _, level := range []string{"", GrantRead, GrantReview, GrantWrite, GrantAdmin} {
		name := level
		if name == "" {
			name = "no grant"
		}
		t.Run(name, func(t *testing.T) {
			f, id := newReviewGrantFixture(t)
			// otherProj reads only project-2, so a grant is its one read path.
			if level != "" {
				insertGrant(f, id, SubjectPrincipal, PrincipalRef(otherProj.kind, otherProj.ref), level, nil)
			}
			reads := f.do(&otherProj, http.MethodGet, "/api/v1/artifacts/"+id, nil, nil).Code == http.StatusOK
			if want := level != ""; reads != want {
				t.Fatalf("GET reads = %v, want %v", reads, want)
			}
			for _, target := range []string{"/api/v1/artifacts?mine=1", "/api/v1/artifacts?mine=1&scope=project-1"} {
				if got := listed(t, f, otherProj, target, id); got != reads {
					t.Errorf("%s lists the artifact = %v, but GET reads = %v", target, got, reads)
				}
			}
		})
	}
}

// TestAutomaticReviewGrantUnderLadder: under the ladder a review grant
// reads, so the automatic review grant (ptone/scion#7134) is held to its
// home-role gate at the routes. Without a role that reads in the home
// project its holder gets the 404 of a missing artifact (body included)
// and no list entry; with the role it reads; once a person changes its
// level it is an ordinary grant and reads after the role ends.
func TestAutomaticReviewGrantUnderLadder(t *testing.T) {
	f, _ := newSponsorReviewFixture(t)
	id := f.publish(agentA, "a.md", []byte("# a"), "").Artifact.ID
	checkSponsorGrant(t, f, id)
	leave := func() {
		f.host.mu.Lock()
		delete(f.host.perms[srSponsorID], "project-1")
		f.host.mu.Unlock()
	}
	get := func() (int, string) {
		rec := f.do(&srSponsor, http.MethodGet, "/api/v1/artifacts/"+id, nil, nil)
		return rec.Code, response(rec)
	}

	if code, _ := get(); code != http.StatusOK {
		t.Fatalf("with a home role: GET %d, want 200", code)
	}

	leave()
	missing := f.do(&srSponsor, http.MethodGet, "/api/v1/artifacts/"+uuid.NewString(), nil, nil)
	if code, body := get(); code != http.StatusNotFound || missing.Code != http.StatusNotFound || body != response(missing) {
		t.Errorf("no home role: GET %d %s; a missing artifact answers %d %s", code, body, missing.Code, response(missing))
	}
	if listed(t, f, srSponsor, "/api/v1/artifacts?mine=1", id) {
		t.Error("no home role: the artifact is listed with mine=1")
	}

	// Restore the home role: a precondition for the level-change steps
	// below. The 200 here comes from the role, so it is not grant coverage.
	f.host.allow(srSponsor, "project-1", PermissionRead)
	if code, _ := get(); code != http.StatusOK {
		t.Errorf("role added back: GET %d, want 200", code)
	}

	// A steward changes the level (to write, then back to review): the
	// grant is then an ordinary review grant, which reads on its own.
	f.host.allow(srSteward, "project-1", PermissionRead)
	f.host.allowSteward(srSteward, agentA.ref, "project-1", StewardClauseProjectOwner)
	for _, level := range []string{GrantWrite, GrantReview} {
		if rec := jsonDo(f, srSteward, http.MethodPost, grantsPath(id), reviewBody(SubjectPrincipal, srSponsorID, level)); rec.Code != http.StatusOK {
			t.Fatalf("level change to %s: %d %s", level, rec.Code, rec.Body.String())
		}
	}
	if g := f.grantOf(id, srSponsorID); g == nil || g.Permission != GrantReview || g.CreatedByRef == SponsorReviewCreator {
		t.Fatalf("after the level changes: %+v, want an ordinary review grant", g)
	}
	leave()
	if code, _ := get(); code != http.StatusOK {
		t.Errorf("ordinary review grant after the role ends: GET %d, want 200", code)
	}
	if !listed(t, f, srSponsor, "/api/v1/artifacts?mine=1", id) {
		t.Error("ordinary review grant after the role ends: not listed with mine=1")
	}
}
