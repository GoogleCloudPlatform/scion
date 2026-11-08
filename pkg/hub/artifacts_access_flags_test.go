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

package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestArtifactAccessFlagsAgreeWithRoutes: the Manage access dialog
// (ptone/scion#7107) decides each section from the capability flags of
// GET /api/v1/artifacts/{id}, and calls only existing routes. For every
// caller, each route the dialog calls accepts exactly when the flag the
// dialog reads says so:
//   - canShare: list grants, list links, create a link, revoke a link,
//     remove a grant, change the expiry (the hub also accepts link
//     creation from a steward; the web offers it to managers only until
//     phase 2);
//   - canManage or canGrantReview: a review grant to a user who can
//     already read the artifact;
//   - the owning agent is refused on every one of them.
func TestArtifactAccessFlagsAgreeWithRoutes(t *testing.T) {
	type caller struct {
		name     string
		identity func(f reviewGrantFixture) Identity
		// adminGrant gives the caller (a user) an admin grant first.
		adminGrant bool
		agent      bool
		// The flags the hub states for the caller.
		manage, share, review bool
	}
	user := func(pick func(f reviewGrantFixture) string) func(f reviewGrantFixture) Identity {
		return func(f reviewGrantFixture) Identity { return f.user(pick(f)) }
	}
	cases := []caller{
		{name: "delegating user", identity: user(func(f reviewGrantFixture) string { return f.delegator }), share: true, review: true},
		{name: "home-project owner", identity: user(func(f reviewGrantFixture) string { return f.owner }), share: true, review: true},
		{name: "home-project admin", identity: user(func(f reviewGrantFixture) string { return f.admin }), review: true},
		{name: "home-project member", identity: user(func(f reviewGrantFixture) string { return f.member })},
		{name: "admin grantee", identity: user(func(f reviewGrantFixture) string { return f.member }), adminGrant: true,
			manage: true, share: true},
		{name: "owning agent", agent: true, identity: func(f reviewGrantFixture) Identity {
			return artifactTestAgent(f.agent.ID, f.home, ScopeProjectArtifactRead, ScopeProjectArtifactWrite)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newReviewGrantFixture(t)
			st, blobs := enableArtifactsForTest(t, f.srv)
			id := reviewGrantArtifact(t, f, st, blobs)
			path := "/api/v1/artifacts/" + id
			identity := tc.identity(f)
			setup := &artifacts.Actor{Kind: artifacts.PrincipalKindUser, Ref: "test-setup", Authority: artifacts.AuthorityOwner}
			now := time.Now()
			if tc.adminGrant {
				_, err := st.PutGrant(ctx, &artifacts.Grant{ID: tid("af-admin"), ArtifactID: id,
					SubjectKind: artifacts.SubjectPrincipal, SubjectRef: artifacts.PrincipalRef(artifacts.PrincipalKindUser, identity.ID()),
					Permission: artifacts.GrantAdmin, CreatedAt: now}, artifacts.MaxGrantsPerArtifact, true, setup)
				require.NoError(t, err)
			}
			// A grant the dialog may remove, and a link it may revoke.
			removable := tid("af-removable")
			_, err := st.PutGrant(ctx, &artifacts.Grant{ID: removable, ArtifactID: id,
				SubjectKind: artifacts.SubjectPrincipal, SubjectRef: artifacts.PrincipalRef(artifacts.PrincipalKindUser, f.outside),
				Permission: artifacts.GrantRead, CreatedAt: now}, artifacts.MaxGrantsPerArtifact, false, setup)
			require.NoError(t, err)
			exp := now.Add(24 * time.Hour)
			seeded := &artifacts.Grant{ID: tid("af-link"), ArtifactID: id, SubjectKind: artifacts.SubjectLink,
				SubjectRef: artifacts.LinkTokenHash("af-seeded-token"), Permission: artifacts.GrantRead, ExpiresAt: &exp,
				CreatedByRef: "user:test-setup", CreatedAt: now}
			require.NoError(t, st.CreateLink(ctx, seeded, artifacts.MaxLinksPerArtifact, now, setup))

			rec := identityArtifactRequest(t, f.srv, identity, http.MethodGet, path, nil)
			require.Equal(t, http.StatusOK, rec.Code, "GET: %s", rec.Body.String())
			var resp artifacts.ArtifactResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			require.Equal(t, tc.manage, resp.CanManage, "canManage")
			require.Equal(t, tc.share, resp.CanShare, "canShare")
			require.Equal(t, tc.review, resp.CanGrantReview, "canGrantReview")

			// check asserts a route accepts (one of ok) exactly when want,
			// and otherwise answers 403.
			check := func(label, method, target string, body []byte, want bool, ok ...int) {
				t.Helper()
				got := identityArtifactRequest(t, f.srv, identity, method, target, body)
				accepted := false
				for _, c := range ok {
					accepted = accepted || got.Code == c
				}
				if want {
					assert.True(t, accepted, "%s: flags say yes, route answered %d %s", label, got.Code, got.Body.String())
					return
				}
				assert.Equal(t, http.StatusForbidden, got.Code, "%s: flags say no, route answered %s", label, got.Body.String())
			}

			check("GET /grants", http.MethodGet, path+"/grants", nil, resp.CanShare, http.StatusOK)
			check("GET /links", http.MethodGet, path+"/links", nil, resp.CanShare, http.StatusOK)
			check("POST /links", http.MethodPost, path+"/links", []byte(`{"ttlHours": 2}`), resp.CanShare, http.StatusCreated)
			check("DELETE /links/{id}", http.MethodDelete, path+"/links/"+seeded.ID, nil, resp.CanShare, http.StatusNoContent)
			check("DELETE /grants/{id}", http.MethodDelete, path+"/grants/"+removable, nil, resp.CanShare, http.StatusNoContent)
			expiry := now.Add(72 * time.Hour).UTC().Format(time.RFC3339)
			check("PATCH expiresAt", http.MethodPatch, path, []byte(`{"expiresAt":"`+expiry+`"}`), resp.CanShare, http.StatusOK)

			// A review grant to a home-project member, who can already read
			// the artifact.
			createDCUser(t, f.s, tid("af-reviewer"), "af-reviewer@test.com", f.home, store.ProjectRoleMember)
			review := []byte(`{"subjectKind":"principal","subjectRef":"user:` + tid("af-reviewer") + `","permission":"review"}`)
			check("POST /grants review", http.MethodPost, path+"/grants", review,
				resp.CanManage || resp.CanGrantReview, http.StatusCreated, http.StatusOK)

			if tc.agent {
				assert.False(t, resp.CanManage || resp.CanShare || resp.CanGrantReview, "an agent holds no access flag")
			}
		})
	}
}
