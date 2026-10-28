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

// TestArtifactCanRetireOnRoutes (ptone/scion#4535): on the hub's routes,
// with the real artifact host and authz service, GET of the artifact and
// of a version states canRetire for an admin grantee, the stewards (the
// delegating user and a home-project owner) and a home-project admin (the
// custodian clause), and for nobody else; DELETE by the same caller
// answers 204 exactly when canRetire is true, and 403 otherwise. The flag
// is advice: when the authority behind a true canRetire is withdrawn
// before the DELETE (the admin grant removed, the home-project admin made a
// plain member), the DELETE decides again with the real authorization and
// refuses with 403, and the artifact is still there.
func TestArtifactCanRetireOnRoutes(t *testing.T) {
	type caller struct {
		name     string
		identity func(f reviewGrantFixture) Identity
		// adminGrant gives the caller (a user) an admin grant first.
		adminGrant bool
		retire     bool
	}
	user := func(pick func(f reviewGrantFixture) string) func(f reviewGrantFixture) Identity {
		return func(f reviewGrantFixture) Identity { return f.user(pick(f)) }
	}
	cases := []caller{
		{name: "delegating user", identity: user(func(f reviewGrantFixture) string { return f.delegator }), retire: true},
		{name: "home-project owner", identity: user(func(f reviewGrantFixture) string { return f.owner }), retire: true},
		{name: "home-project admin", identity: user(func(f reviewGrantFixture) string { return f.admin }), retire: true},
		{name: "admin grantee", identity: user(func(f reviewGrantFixture) string { return f.member }), adminGrant: true, retire: true},
		{name: "home-project member", identity: user(func(f reviewGrantFixture) string { return f.member })},
		{name: "owning agent", identity: func(f reviewGrantFixture) Identity {
			return artifactTestAgent(f.agent.ID, f.home, ScopeProjectArtifactRead, ScopeProjectArtifactWrite)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newReviewGrantFixture(t)
			st, blobs := enableArtifactsForTest(t, f.srv)
			id := reviewGrantArtifact(t, f, st, blobs)
			identity := tc.identity(f)
			if tc.adminGrant {
				_, err := st.PutGrant(context.Background(), &artifacts.Grant{ID: tid("cr-admin-" + tc.name), ArtifactID: id,
					SubjectKind: artifacts.SubjectPrincipal, SubjectRef: artifacts.PrincipalRef(artifacts.PrincipalKindUser, identity.ID()),
					Permission: artifacts.GrantAdmin, CreatedAt: time.Now()}, artifacts.MaxGrantsPerArtifact, true,
					&artifacts.Actor{Kind: artifacts.PrincipalKindUser, Ref: "test-setup", Authority: artifacts.AuthorityOwner})
				require.NoError(t, err)
			}
			var resp artifacts.ArtifactResponse
			for _, target := range []string{"/api/v1/artifacts/" + id, "/api/v1/artifacts/" + id + "/versions/1"} {
				rec := identityArtifactRequest(t, f.srv, identity, http.MethodGet, target, nil)
				require.Equal(t, http.StatusOK, rec.Code, "GET %s: %s", target, rec.Body.String())
				resp = artifacts.ArtifactResponse{}
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
				assert.Equal(t, tc.retire, resp.CanRetire, "GET %s canRetire", target)
				if !tc.retire {
					assert.NotContains(t, rec.Body.String(), `"canRetire"`, "GET %s", target)
				}
			}
			// Parity: the delete route agrees with the flag.
			del := identityArtifactRequest(t, f.srv, identity, http.MethodDelete, "/api/v1/artifacts/"+id, nil)
			want := http.StatusForbidden
			if resp.CanRetire {
				want = http.StatusNoContent
			}
			assert.Equal(t, want, del.Code, "canRetire=%v but DELETE answered %d %s", resp.CanRetire, del.Code, del.Body.String())
		})
	}

	t.Run("stale flag: admin grant removed", func(t *testing.T) {
		f := newReviewGrantFixture(t)
		st, blobs := enableArtifactsForTest(t, f.srv)
		id := reviewGrantArtifact(t, f, st, blobs)
		ctx := context.Background()
		owner := &artifacts.Actor{Kind: artifacts.PrincipalKindUser, Ref: "test-setup", Authority: artifacts.AuthorityOwner}
		grantID := tid("cr-stale-admin")
		_, err := st.PutGrant(ctx, &artifacts.Grant{ID: grantID, ArtifactID: id,
			SubjectKind: artifacts.SubjectPrincipal, SubjectRef: artifacts.PrincipalRef(artifacts.PrincipalKindUser, f.member),
			Permission: artifacts.GrantAdmin, CreatedAt: time.Now()}, artifacts.MaxGrantsPerArtifact, true, owner)
		require.NoError(t, err)
		assertCanRetireThenStale(t, f, st, id, f.user(f.member), func() {
			require.NoError(t, st.DeleteGrant(ctx, id, grantID, owner))
		})
	})

	t.Run("stale flag: home-project admin made a member", func(t *testing.T) {
		f := newReviewGrantFixture(t)
		st, blobs := enableArtifactsForTest(t, f.srv)
		id := reviewGrantArtifact(t, f, st, blobs)
		ctx := context.Background()
		assertCanRetireThenStale(t, f, st, id, f.user(f.admin), func() {
			bindings, err := f.s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, f.admin)
			require.NoError(t, err)
			removed := 0
			for _, b := range bindings {
				if b.ScopeType == store.RoleScopeProject && b.ScopeID == f.home {
					require.NoError(t, f.s.DeleteRoleBinding(ctx, b.ID))
					removed++
				}
			}
			require.Equal(t, 1, removed, "the admin's role binding in the home project")
			// Still a member, so still a reader: the refusal is 403, not 404.
			createDCUser(t, f.s, f.admin, "rg-admin@test.com", f.home, store.ProjectRoleMember)
		})
	})
}

// assertCanRetireThenStale checks that identity sees canRetire true on id,
// runs withdraw, and that the DELETE by the same identity is then refused
// with 403 and leaves the artifact in place.
func assertCanRetireThenStale(t *testing.T, f reviewGrantFixture, st artifacts.Store, id string, identity Identity, withdraw func()) {
	t.Helper()
	rec := identityArtifactRequest(t, f.srv, identity, http.MethodGet, "/api/v1/artifacts/"+id, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp artifacts.ArtifactResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.True(t, resp.CanRetire, "canRetire before the authority is withdrawn")
	withdraw()
	del := identityArtifactRequest(t, f.srv, identity, http.MethodDelete, "/api/v1/artifacts/"+id, nil)
	assert.Equal(t, http.StatusForbidden, del.Code, "DELETE after the authority was withdrawn: %s", del.Body.String())
	_, err := st.GetArtifact(context.Background(), id)
	assert.NoError(t, err, "the refused DELETE deleted the artifact")
}
