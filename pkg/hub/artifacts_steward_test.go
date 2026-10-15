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
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Stewards of agent-owned artifacts (design D25, ptone/scion#5144):
// artifactHost.StewardClause and the routes it opens.

func (f reviewGrantFixture) steward(host *artifactHost, identity Identity, agentID, home string) string {
	ctx := context.Background()
	if identity != nil {
		ctx = contextWithIdentity(ctx, identity)
	}
	return host.StewardClause(ctx, agentID, home, "", false)
}

// TestArtifactHostStewardClause: an owner of the home project (and a
// super-admin) is a project_owner steward; the user at the root of the
// owning agent's live chain is a delegator steward while it reads in the
// home project through a role; nobody else is: not a project admin (who
// holds project.manage but not artifact.steward), a member, a hub-admin,
// a user of another project, the agent, or a caller with no identity.
func TestArtifactHostStewardClause(t *testing.T) {
	f := newReviewGrantFixture(t)
	createRS3SuperAdmin(t, f.s, tid("st-super"))
	createRS3HubAdmin(t, f.s, tid("st-hub"))
	host := newArtifactHost(f.srv)
	agentToken := artifactTestAgent(f.agent.ID, f.home, ScopesForRole(AgentRoleFull)...)

	cases := []struct {
		name     string
		identity Identity
		agentID  string
		want     string
	}{
		{"home-project owner", f.user(f.owner), f.agent.ID, artifacts.StewardClauseProjectOwner},
		{"home-project owner, agent with no edge", f.user(f.owner), f.noEdge.ID, artifacts.StewardClauseProjectOwner},
		{"super-admin", f.user(tid("st-super")), f.agent.ID, artifacts.StewardClauseProjectOwner},
		{"delegating user", f.user(f.delegator), f.agent.ID, artifacts.StewardClauseDelegator},
		{"delegating user, agent-spawned agent", f.user(f.delegator), f.child.ID, artifacts.StewardClauseDelegator},
		{"another user's delegate", f.user(f.member), f.sibling.ID, artifacts.StewardClauseDelegator},

		{"home-project admin", f.user(f.admin), f.agent.ID, ""},
		{"plain project member", f.user(f.member), f.agent.ID, ""},
		{"hub-admin", f.user(tid("st-hub")), f.agent.ID, ""},
		{"user from another project", f.user(f.outside), f.agent.ID, ""},
		{"admin of another project", f.user(f.otherAdmin), f.agent.ID, ""},
		{"creator of an agent with no edge", f.user(f.delegator), f.noEdge.ID, ""},
		{"delegating user of a sibling chain", f.user(f.member), f.child.ID, ""},
		{"unknown agent", f.user(f.delegator), tid("st-missing"), ""},
		{"owning agent", agentToken, f.agent.ID, ""},
		{"no identity", nil, f.agent.ID, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, f.steward(host, tc.identity, tc.agentID, f.home))
		})
	}

	// After a move the clauses follow the current home: the delegating user
	// has no role in other, so it is not a steward there (unlike the review
	// clause, which only asks for the agent's project); other's admin is
	// not an owner either; the old home's owner has no role in other.
	t.Run("moved home", func(t *testing.T) {
		assert.Equal(t, "", f.steward(host, f.user(f.delegator), f.agent.ID, f.other), "delegating user without a role in the new home")
		assert.Equal(t, "", f.steward(host, f.user(f.owner), f.agent.ID, f.other), "old home's owner")
		assert.Equal(t, "", f.steward(host, f.user(f.otherAdmin), f.agent.ID, f.other), "new home's admin")
		assert.Equal(t, artifacts.StewardClauseProjectOwner, f.steward(host, f.user(tid("st-super")), f.agent.ID, f.other))
	})
	t.Run("empty arguments", func(t *testing.T) {
		assert.Equal(t, "", f.steward(host, f.user(f.owner), "", f.home))
		assert.Equal(t, "", f.steward(host, f.user(f.owner), f.agent.ID, ""))
	})
}

// TestArtifactHostStewardClause_ScopedToken: a scoped user access token
// keeps its limits: the project-owner clause needs artifact:steward in the
// ceiling (a token minted before the permission existed fails closed), the
// delegator clause needs artifact:read, and a token bounded to another
// project stewards nothing in this one.
func TestArtifactHostStewardClause_ScopedToken(t *testing.T) {
	f := newReviewGrantFixture(t)
	host := newArtifactHost(f.srv)
	owner := NewAuthenticatedUser(f.owner, "rg-owner@test.com", "owner", "member", "api")
	delegator := NewAuthenticatedUser(f.delegator, "rg-delegator@test.com", "delegator", "member", "api")

	assert.Equal(t, "", f.steward(host, artifactTestUAT(t, owner, f.home, "artifact:read", "artifact:manage"), f.agent.ID, f.home),
		"owner token without artifact:steward")
	assert.Equal(t, artifacts.StewardClauseProjectOwner,
		f.steward(host, artifactTestUAT(t, owner, f.home, "artifact:read", "artifact:manage", "artifact:steward"), f.agent.ID, f.home),
		"owner token with artifact:steward")
	assert.Equal(t, "", f.steward(host, artifactTestUAT(t, owner, f.other, "artifact:read", "artifact:manage", "artifact:steward"), f.agent.ID, f.home),
		"owner token bounded to another project")
	assert.Equal(t, artifacts.StewardClauseDelegator,
		f.steward(host, artifactTestUAT(t, delegator, f.home, "artifact:read", "artifact:manage"), f.agent.ID, f.home),
		"delegating user's token")
	assert.Equal(t, "", f.steward(host, artifactTestUAT(t, delegator, f.home, "artifact:manage"), f.agent.ID, f.home),
		"delegating user's token without artifact:read")
	assert.Equal(t, "", f.steward(host, artifactTestUAT(t, delegator, f.other, "artifact:read", "artifact:manage"), f.agent.ID, f.home),
		"delegating user's token bounded to another project")
}

// TestArtifactHostStewardClause_Live: the clauses are decided at request
// time: revoking the delegation, deactivating the delegating user, its
// leaving the home project, deleting the agent or deleting the home
// project each take the authority away; owners keep theirs where the
// project still exists.
func TestArtifactHostStewardClause_Live(t *testing.T) {
	t.Run("delegation revoked", func(t *testing.T) {
		f := newReviewGrantFixture(t)
		host := newArtifactHost(f.srv)
		require.Equal(t, artifacts.StewardClauseDelegator, f.steward(host, f.user(f.delegator), f.agent.ID, f.home))
		revokeDelegateEdges(t, f.s, f.agent.ID)
		assert.Equal(t, "", f.steward(host, f.user(f.delegator), f.agent.ID, f.home))
		assert.Equal(t, "", f.steward(host, f.user(f.delegator), f.child.ID, f.home), "agent below a revoked edge")
		assert.Equal(t, artifacts.StewardClauseProjectOwner, f.steward(host, f.user(f.owner), f.agent.ID, f.home), "owner unaffected")
	})
	t.Run("delegating user inactive", func(t *testing.T) {
		f := newReviewGrantFixture(t)
		host := newArtifactHost(f.srv)
		setUserStatus(t, f.s, f.delegator, "suspended")
		assert.Equal(t, "", f.steward(host, f.user(f.delegator), f.agent.ID, f.home))
	})
	t.Run("delegating user left the project", func(t *testing.T) {
		f := newReviewGrantFixture(t)
		host := newArtifactHost(f.srv)
		bindings, err := f.s.ListRoleBindingsForPrincipal(context.Background(), store.RoleBindingPrincipalUser, f.delegator)
		require.NoError(t, err)
		for _, b := range bindings {
			if b.ScopeType == store.RoleScopeProject && b.ScopeID == f.home {
				require.NoError(t, f.s.DeleteRoleBinding(context.Background(), b.ID))
			}
		}
		assert.Equal(t, "", f.steward(host, f.user(f.delegator), f.agent.ID, f.home))
	})
	t.Run("agent deleted", func(t *testing.T) {
		f := newReviewGrantFixture(t)
		host := newArtifactHost(f.srv)
		require.NoError(t, f.s.DeleteAgent(context.Background(), f.agent.ID))
		assert.Equal(t, "", f.steward(host, f.user(f.delegator), f.agent.ID, f.home), "no sponsor clause yet")
		assert.Equal(t, artifacts.StewardClauseProjectOwner, f.steward(host, f.user(f.owner), f.agent.ID, f.home))
	})
	t.Run("home project deleted", func(t *testing.T) {
		f := newReviewGrantFixture(t)
		createRS3SuperAdmin(t, f.s, tid("st-live-super"))
		host := newArtifactHost(f.srv)
		require.NoError(t, f.s.DeleteProject(context.Background(), f.home))
		assert.Equal(t, "", f.steward(host, f.user(tid("st-live-super")), f.agent.ID, f.home), "super-admin")
		assert.Equal(t, "", f.steward(host, f.user(f.owner), f.agent.ID, f.home), "former owner")
		assert.Equal(t, "", f.steward(host, f.user(f.delegator), f.agent.ID, f.home), "delegating user")
	})
	t.Run("no authz service", func(t *testing.T) {
		f := newReviewGrantFixture(t)
		assert.Equal(t, "", f.steward(newArtifactHost(&Server{}), f.user(f.owner), f.agent.ID, f.home))
		assert.Equal(t, "", f.steward(&artifactHost{}, f.user(f.owner), f.agent.ID, f.home))
	})
}

// TestBuiltInRolesArtifactSteward: artifact.steward is carried by the
// project-owner role and super-admin, and by no other built-in role.
func TestBuiltInRolesArtifactSteward(t *testing.T) {
	var holders []string
	for _, r := range BuiltInRoles() {
		if slices.Contains(r.Permissions, artifacts.PermissionSteward) {
			holders = append(holders, r.Name)
		}
	}
	slices.Sort(holders)
	want := []string{store.ProjectRoleOwner, store.SystemRoleSuperAdmin}
	slices.Sort(want)
	assert.Equal(t, want, holders)
}

// TestArtifactStewardOnRoutes: through the real hub, on an artifact owned
// by an agent, its delegating user and a home-project owner each give a
// user outside the project Can view (who can then open it) and create a
// share link that works signed out; they cannot give Can manage or move
// it (403 steward_action_not_allowed), and their grants to other projects
// follow the cross-project switch. A home-project admin who is not an
// owner, a member, and a delegating user with no role in the home (only a
// grant on the artifact) cannot share; the admin can still delete. GET
// states canShare and shareAuthority. Each share is recorded with the
// steward clause.
func TestArtifactStewardOnRoutes(t *testing.T) {
	type sharer struct {
		name      string
		user      func(f reviewGrantFixture) string
		authority string
		clause    string
	}
	sharers := []sharer{
		{"delegating user", func(f reviewGrantFixture) string { return f.delegator }, artifacts.AuthorityStewardDelegator, artifacts.StewardClauseDelegator},
		{"home-project owner", func(f reviewGrantFixture) string { return f.owner }, artifacts.AuthorityStewardProjectOwner, artifacts.StewardClauseProjectOwner},
	}
	for _, sh := range sharers {
		t.Run(sh.name, func(t *testing.T) {
			f := newReviewGrantFixture(t)
			st, blobs := enableArtifactsForTest(t, f.srv)
			id := reviewGrantArtifact(t, f, st, blobs)
			me := f.user(sh.user(f))
			path := "/api/v1/artifacts/" + id
			grants := path + "/grants"

			rec := identityArtifactRequest(t, f.srv, me, http.MethodGet, path, nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var resp artifacts.ArtifactResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			assert.True(t, resp.CanShare)
			assert.False(t, resp.CanManage)
			assert.Equal(t, sh.clause, resp.ShareAuthority)

			// Share with a user outside the project, at Can view.
			outside := f.user(f.outside)
			require.Equal(t, http.StatusNotFound, identityArtifactRequest(t, f.srv, outside, http.MethodGet, path, nil).Code)
			rec = identityArtifactRequest(t, f.srv, me, http.MethodPost, grants,
				[]byte(`{"subjectKind":"principal","subjectRef":"user:`+f.outside+`","permission":"read"}`))
			require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
			assert.Equal(t, http.StatusOK, identityArtifactRequest(t, f.srv, outside, http.MethodGet, path, nil).Code)

			// A share link that works signed out.
			rec = identityArtifactRequest(t, f.srv, me, http.MethodPost, path+"/links", []byte(`{"ttlHours": 2}`))
			require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
			var link artifacts.CreateLinkResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &link))
			shared := anonymous(f.srv, http.MethodGet, link.URL)
			require.Equal(t, http.StatusSeeOther, shared.Code, shared.Body.String())

			// The bounded actions.
			for _, req := range []struct{ method, target, body string }{
				{http.MethodPost, grants, `{"subjectKind":"principal","subjectRef":"user:` + f.outside + `","permission":"admin"}`},
				{http.MethodPost, grants, `{"subjectKind":"scope","subjectRef":"` + f.other + `","permission":"admin"}`},
				{http.MethodPatch, path, `{"scopeRef":"` + f.other + `"}`},
			} {
				rec = identityArtifactRequest(t, f.srv, me, req.method, req.target, []byte(req.body))
				assert.Equal(t, http.StatusForbidden, rec.Code, "%s %s: %s", req.method, req.body, rec.Body.String())
				assert.Contains(t, rec.Body.String(), artifacts.CodeStewardActionNotAllowed, "%s %s", req.method, req.body)
			}

			// Cross-project grants follow the switch.
			scopeBody := []byte(`{"subjectKind":"scope","subjectRef":"` + f.other + `","permission":"write"}`)
			rec = identityArtifactRequest(t, f.srv, me, http.MethodPost, grants, scopeBody)
			assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), "cross_project_sharing_disabled")
			f.srv.SetOperationalSettings(artifactsOpsWith(t, "", `{"cross_project_messaging_enabled": true}`))
			rec = identityArtifactRequest(t, f.srv, me, http.MethodPost, grants, scopeBody)
			assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

			// The events name the steward clause and never the token.
			evs, err := st.ListEvents(context.Background(), id)
			require.NoError(t, err)
			var created []string
			for _, e := range evs {
				if e.Action == artifacts.EventGrantCreated || e.Action == artifacts.EventLinkCreated {
					assert.Equal(t, me.ID(), e.ActorRef, "%s actor", e.Action)
					assert.Equal(t, sh.authority, e.Authority, "%s authority", e.Action)
					created = append(created, e.Action)
				}
			}
			assert.Equal(t, []string{artifacts.EventGrantCreated, artifacts.EventLinkCreated, artifacts.EventGrantCreated}, created)
			raw, err := json.Marshal(evs)
			require.NoError(t, err)
			token := strings.TrimPrefix(link.URL, artifacts.RouteShared)
			assert.NotContains(t, string(raw), token)
			assert.NotContains(t, string(raw), artifacts.LinkTokenHash(token))
		})
	}

	t.Run("not stewards", func(t *testing.T) {
		f := newReviewGrantFixture(t)
		st, blobs := enableArtifactsForTest(t, f.srv)
		id := reviewGrantArtifact(t, f, st, blobs)
		path := "/api/v1/artifacts/" + id
		share := []byte(`{"subjectKind":"principal","subjectRef":"user:` + f.outside + `","permission":"read"}`)

		// A delegating user with no role left in the home project, given
		// a read grant on the artifact: the grant lets it read, but it is
		// not a steward (a grant never counts as a role).
		bindings, err := f.s.ListRoleBindingsForPrincipal(context.Background(), store.RoleBindingPrincipalUser, f.delegator)
		require.NoError(t, err)
		for _, b := range bindings {
			if b.ScopeType == store.RoleScopeProject && b.ScopeID == f.home {
				require.NoError(t, f.s.DeleteRoleBinding(context.Background(), b.ID))
			}
		}
		_, err = st.PutGrant(context.Background(), &artifacts.Grant{ID: tid("st-routes-read"), ArtifactID: id,
			SubjectKind: artifacts.SubjectPrincipal, SubjectRef: artifacts.PrincipalRef(artifacts.PrincipalKindUser, f.delegator),
			Permission: artifacts.GrantRead, CreatedAt: time.Now()}, artifacts.MaxGrantsPerArtifact, false,
			&artifacts.Actor{Kind: artifacts.PrincipalKindUser, Ref: "test-setup", Authority: artifacts.AuthorityOwner})
		require.NoError(t, err)

		for name, uid := range map[string]string{"home-project admin": f.admin, "member": f.member, "delegating user with only a grant": f.delegator} {
			u := f.user(uid)
			rec := identityArtifactRequest(t, f.srv, u, http.MethodGet, path, nil)
			require.Equal(t, http.StatusOK, rec.Code, "%s GET: %s", name, rec.Body.String())
			var resp artifacts.ArtifactResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			assert.False(t, resp.CanShare, "%s canShare", name)
			assert.Empty(t, resp.ShareAuthority, "%s shareAuthority", name)
			rec = identityArtifactRequest(t, f.srv, u, http.MethodPost, path+"/grants", share)
			assert.Equal(t, http.StatusForbidden, rec.Code, "%s share: %s", name, rec.Body.String())
			rec = identityArtifactRequest(t, f.srv, u, http.MethodPost, path+"/links", nil)
			assert.Equal(t, http.StatusForbidden, rec.Code, "%s link: %s", name, rec.Body.String())
		}
		// The admin still deletes, as custodian.
		rec := identityArtifactRequest(t, f.srv, f.user(f.admin), http.MethodDelete, path, nil)
		require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
		evs, err := st.ListEvents(context.Background(), id)
		require.NoError(t, err)
		require.NotEmpty(t, evs)
		last := evs[len(evs)-1]
		assert.Equal(t, artifacts.EventDeleted, last.Action)
		assert.Equal(t, artifacts.AuthorityCustodianProjectAdmin, last.Authority)
	})

	t.Run("steward deletes", func(t *testing.T) {
		f := newReviewGrantFixture(t)
		st, blobs := enableArtifactsForTest(t, f.srv)
		id := reviewGrantArtifact(t, f, st, blobs)
		rec := identityArtifactRequest(t, f.srv, f.user(f.delegator), http.MethodDelete, "/api/v1/artifacts/"+id, nil)
		require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
		evs, err := st.ListEvents(context.Background(), id)
		require.NoError(t, err)
		require.NotEmpty(t, evs)
		assert.Equal(t, artifacts.AuthorityStewardDelegator, evs[len(evs)-1].Authority)
	})

	t.Run("revoked delegation", func(t *testing.T) {
		f := newReviewGrantFixture(t)
		st, blobs := enableArtifactsForTest(t, f.srv)
		id := reviewGrantArtifact(t, f, st, blobs)
		revokeDelegateEdges(t, f.s, f.agent.ID)
		share := []byte(`{"subjectKind":"principal","subjectRef":"user:` + f.outside + `","permission":"read"}`)
		rec := identityArtifactRequest(t, f.srv, f.user(f.delegator), http.MethodPost, "/api/v1/artifacts/"+id+"/grants", share)
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		rec = identityArtifactRequest(t, f.srv, f.user(f.owner), http.MethodPost, "/api/v1/artifacts/"+id+"/grants", share)
		assert.Equal(t, http.StatusCreated, rec.Code, "owner still shares: %s", rec.Body.String())
	})

	t.Run("user-owned artifact", func(t *testing.T) {
		f := newReviewGrantFixture(t)
		st, _ := enableArtifactsForTest(t, f.srv)
		id := seedOwnedArtifact(t, st, artifacts.PrincipalKindUser, f.member, f.home)
		share := []byte(`{"subjectKind":"principal","subjectRef":"user:` + f.outside + `","permission":"read"}`)
		rec := identityArtifactRequest(t, f.srv, f.user(f.owner), http.MethodPost, "/api/v1/artifacts/"+id+"/grants", share)
		assert.Equal(t, http.StatusForbidden, rec.Code, "project owner on a user-owned artifact: %s", rec.Body.String())
	})

	t.Run("unreadable is missing", func(t *testing.T) {
		f := newReviewGrantFixture(t)
		st, blobs := enableArtifactsForTest(t, f.srv)
		id := reviewGrantArtifact(t, f, st, blobs)
		u := f.user(f.otherAdmin)
		for _, req := range []struct{ method, suffix, body string }{
			{http.MethodPost, "/grants", `{"subjectKind":"principal","subjectRef":"user:x","permission":"read"}`},
			{http.MethodPost, "/links", ""},
			{http.MethodPatch, "", `{"expiresAt":null}`},
			{http.MethodDelete, "", ""},
		} {
			missing := identityArtifactRequest(t, f.srv, u, req.method, "/api/v1/artifacts/"+tid("st-missing")+req.suffix, []byte(req.body))
			got := identityArtifactRequest(t, f.srv, u, req.method, "/api/v1/artifacts/"+id+req.suffix, []byte(req.body))
			assert.Equal(t, http.StatusNotFound, got.Code, "%s %s", req.method, req.suffix)
			assert.Equal(t, missing.Body.String(), got.Body.String(), "%s %s", req.method, req.suffix)
		}
	})
}

// TestArtifactStewardDecisionLog: a steward's refused admin grant is
// logged with the reason steward_action_not_allowed and no token or title.
func TestArtifactStewardDecisionLog(t *testing.T) {
	f := newReviewGrantFixture(t)
	st, blobs := enableArtifactsForTest(t, f.srv)
	id := reviewGrantArtifact(t, f, st, blobs)
	var logged strings.Builder
	l := slog.New(slog.NewJSONHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
	f.srv.SetArtifactLogger(l)
	rec := identityArtifactRequest(t, f.srv, f.user(f.delegator), http.MethodPost, "/api/v1/artifacts/"+id+"/grants",
		[]byte(`{"subjectKind":"principal","subjectRef":"user:`+f.outside+`","permission":"admin"}`))
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, logged.String(), `"reason":"steward_action_not_allowed"`)
	assert.NotContains(t, logged.String(), "draft")
}
