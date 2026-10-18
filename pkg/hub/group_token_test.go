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
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// groupTokenFixture is a project with a narrow custom project role (project
// read and agent read), and a caller who:
//   - holds that role in the project;
//   - holds every group permission through a system-scoped custom role, so
//     the group selectors are mintable on a hub boundary and pass the live
//     authority check;
//   - owns every group the fixture creates, which clears the group role
//     hierarchy check of member changes.
type groupTokenFixture struct {
	srv       *Server
	s         store.Store
	projectID string
	roleID    string
	callerID  string
	caller    *store.User
}

// groupTokenRoleSelectors are the selectors of the fixture role's
// permissions.
var groupTokenRoleSelectors = []string{"project:read", "agent:read"}

// groupTokenGroupSelectors are the selectors of every group permission.
var groupTokenGroupSelectors = []string{
	"group:create", "group:read", "group:list", "group:update", "group:delete", "group:addMember", "group:removeMember",
}

func newGroupTokenFixture(t *testing.T, prefix string) *groupTokenFixture {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()
	f := &groupTokenFixture{srv: srv, s: s, projectID: tid(prefix + "-project"), callerID: tid(prefix + "-caller")}
	createRS1Project(t, s, f.projectID, tid(prefix+"-owner"))

	role, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name: prefix + "-narrow", Description: "project read and agent read", ScopeType: store.RoleScopeProject,
		Permissions: []string{"project.read", "agent.read"},
	})
	require.NoError(t, err)
	f.roleID = role.ID

	f.caller = &store.User{ID: f.callerID, Email: f.callerID + "@test.com", DisplayName: "caller", Role: "member", Status: "active"}
	require.NoError(t, s.CreateUser(ctx, f.caller))
	ensureHubMembership(ctx, s, f.callerID)
	f.bindUser(t, f.callerID, f.projectID)
	groupRole, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name: prefix + "-groups", Description: "every group permission", ScopeType: store.RoleScopeSystem,
		Permissions: []string{"group.create", "group.read", "group.list", "group.update", "group.delete", "group.addMember", "group.removeMember"},
	})
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: groupRole.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: f.callerID,
		ScopeType: store.RoleScopeSystem, CreatedBy: "test",
	})
	require.NoError(t, err)
	return f
}

// bindUser binds the fixture role to a user in a project.
func (f *groupTokenFixture) bindUser(t *testing.T, userID, projectID string) {
	t.Helper()
	_, err := f.s.CreateRoleBinding(context.Background(), &store.RoleBinding{
		RoleDefinitionID: f.roleID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: userID,
		ScopeType: store.RoleScopeProject, ScopeID: projectID, CreatedBy: "test",
	})
	require.NoError(t, err)
}

// group creates an explicit group (no project) owned by the caller.
func (f *groupTokenFixture) group(t *testing.T, name string) *store.Group {
	t.Helper()
	g := &store.Group{ID: tid(name), Slug: name, Name: name, GroupType: store.GroupTypeExplicit, OwnerID: f.callerID}
	require.NoError(t, f.s.CreateGroup(context.Background(), g))
	return g
}

// bindGroup binds a role to a group at a scope.
func (f *groupTokenFixture) bindGroup(t *testing.T, g *store.Group, roleID, scopeType, scopeID string) string {
	t.Helper()
	b, err := f.s.CreateRoleBinding(context.Background(), &store.RoleBinding{
		RoleDefinitionID: roleID, PrincipalType: store.RoleBindingPrincipalGroup, PrincipalID: g.ID,
		ScopeType: scopeType, ScopeID: scopeID, CreatedBy: "test",
	})
	require.NoError(t, err)
	return b.ID
}

// nest makes child a member of parent.
func (f *groupTokenFixture) nest(t *testing.T, parent, child *store.Group) {
	t.Helper()
	require.NoError(t, f.s.AddGroupMember(context.Background(), &store.GroupMember{
		GroupID: parent.ID, MemberType: store.GroupMemberTypeGroup, MemberID: child.ID, Role: store.GroupMemberRoleMember,
	}))
}

func (f *groupTokenFixture) user(t *testing.T, name string) string {
	t.Helper()
	id := tid(name)
	require.NoError(t, f.s.CreateUser(context.Background(), &store.User{ID: id, Email: id + "@test.com", DisplayName: name, Role: "member", Status: "active"}))
	return id
}

// mint mints a real token for the caller.
func (f *groupTokenFixture) mint(t *testing.T, boundary TokenBoundary, scopes ...string) string {
	t.Helper()
	key, _, err := f.srv.uatService.CreateTokenWithParams(rs4MintContext(f.callerID), CreateTokenParams{
		UserID: f.callerID, Name: "gt-" + tid("tok"), Boundary: boundary, Scopes: scopes,
	})
	require.NoError(t, err, "mint %s token with %v", boundary.Kind, scopes)
	return key
}

// session sends a request with an interactive session of the caller.
func (f *groupTokenFixture) session(t *testing.T, method, path string, body interface{}) int {
	t.Helper()
	rec := doRequestAsUser(t, f.srv, f.caller, method, path, body)
	return rec.Code
}

func (f *groupTokenFixture) isMember(t *testing.T, g *store.Group, memberType, memberID string) bool {
	t.Helper()
	_, err := f.s.GetGroupMembership(context.Background(), g.ID, memberType, memberID)
	if err == nil {
		return true
	}
	require.ErrorIs(t, err, store.ErrNotFound)
	return false
}

func withSelectors(sets ...[]string) []string {
	var out []string
	for _, s := range sets {
		out = append(out, s...)
	}
	return out
}

// noBody is the request body of a route that takes none.
func noBody(string) interface{} { return nil }

func addUserBody(userID string) map[string]interface{} {
	return map[string]interface{}{"memberType": store.GroupMemberTypeUser, "memberId": userID, "role": store.GroupMemberRoleMember}
}

// TestGroupMemberAddToken_ProjectBindingsOnClosureRequireCallerAuthority
// pins that a user access token adds a member to a group whose closure
// carries a project-scoped role binding only when the token itself holds,
// in that project, every permission the binding carries: the holder's live
// role bindings intersected with the token's selectors. The binding may be
// on the group or on any group that contains it. A session of a caller with
// no authority in the project adds the member.
func TestGroupMemberAddToken_ProjectBindingsOnClosureRequireCallerAuthority(t *testing.T) {
	f := newGroupTokenFixture(t, "gpb")
	ctx := context.Background()

	bound := f.group(t, "gpb-bound")
	f.bindGroup(t, bound, f.roleID, store.RoleScopeProject, f.projectID)
	parent := f.group(t, "gpb-parent")
	f.bindGroup(t, parent, f.roleID, store.RoleScopeProject, f.projectID)
	nested := f.group(t, "gpb-nested")
	f.nest(t, parent, nested)

	without := f.mint(t, hubBoundary(), "group:addMember")
	with := f.mint(t, hubBoundary(), withSelectors([]string{"group:addMember"}, groupTokenRoleSelectors)...)

	for _, g := range []*store.Group{bound, nested} {
		target := f.user(t, g.Slug+"-target")
		rec := doRequestWithUAT(t, f.srv, without, http.MethodPost, "/api/v1/groups/"+g.ID+"/members", addUserBody(target))
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s: a token without the closure's project selectors: %s", g.Slug, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "Cannot grant authority you do not hold")
		assert.False(t, f.isMember(t, g, store.GroupMemberTypeUser, target), "%s: the refused add adds no member", g.Slug)
		assert.Empty(t, grantAudits(t, f.s, "group_member_add", g.ID), "%s: the refused add writes no audit record", g.Slug)

		rec = doRequestWithUAT(t, f.srv, with, http.MethodPost, "/api/v1/groups/"+g.ID+"/members", addUserBody(target))
		require.Equal(t, http.StatusCreated, rec.Code, "%s: a token holding the closure's project authority: %s", g.Slug, rec.Body.String())
		assert.True(t, f.isMember(t, g, store.GroupMemberTypeUser, target))
		audits := grantAudits(t, f.s, "group_member_add", g.ID)
		require.Len(t, audits, 1, "%s: one audit record, written with the membership", g.Slug)
		assert.Equal(t, "allow", audits[0].CanDelegateResult)
		assert.Equal(t, f.callerID, audits[0].ActorPrincipalID)
	}

	// The holder's live authority bounds the token: with the binding that
	// gives the caller the project role removed, the same selectors do not
	// admit the add.
	bindings, err := f.s.ListRoleBindingsForPrincipals(ctx, []store.PrincipalRef{{Type: store.RoleBindingPrincipalUser, ID: f.callerID}}, []string{store.RoleScopeProject}, []string{f.projectID})
	require.NoError(t, err)
	require.Len(t, bindings, 1)
	require.NoError(t, f.s.DeleteRoleBinding(ctx, bindings[0].ID))
	target := f.user(t, "gpb-after-loss")
	rec := doRequestWithUAT(t, f.srv, with, http.MethodPost, "/api/v1/groups/"+bound.ID+"/members", addUserBody(target))
	assert.Equal(t, http.StatusForbidden, rec.Code, "a token whose holder lacks the project role: %s", rec.Body.String())
	assert.False(t, f.isMember(t, bound, store.GroupMemberTypeUser, target))

	// A session is governed by group roles for project-scoped closure
	// bindings: the same caller, without any project authority, adds
	// the member.
	assert.Equal(t, http.StatusCreated, f.session(t, http.MethodPost, "/api/v1/groups/"+bound.ID+"/members", addUserBody(target)))
	assert.True(t, f.isMember(t, bound, store.GroupMemberTypeUser, target))
}

// TestGroupMemberAddToken_SystemBindingsOnClosureDenied pins that a user
// access token never adds a member to a group whose closure carries a
// system-scoped role binding, even when the holder is a super-admin and the
// token carries the selectors of every permission the binding carries,
// including through an ancestor group and when creating a child group. A
// session of the same super-admin adds the member.
func TestGroupMemberAddToken_SystemBindingsOnClosureDenied(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	adminID := tid("gsb-admin")
	createTestUserWithRole(t, s, adminID, adminID+"@test.com", "admin", store.SystemRoleSuperAdmin)
	ensureHubMembership(ctx, s, adminID)
	admin, err := s.GetUser(ctx, adminID)
	require.NoError(t, err)

	sysRole, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name: "gsb-system", Description: "group read", ScopeType: store.RoleScopeSystem, Permissions: []string{"group.read"},
	})
	require.NoError(t, err)
	newGroup := func(name string) *store.Group {
		g := &store.Group{ID: tid(name), Slug: name, Name: name, GroupType: store.GroupTypeExplicit, OwnerID: adminID}
		require.NoError(t, s.CreateGroup(ctx, g))
		return g
	}
	bound := newGroup("gsb-bound")
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{RoleDefinitionID: sysRole.ID, PrincipalType: store.RoleBindingPrincipalGroup, PrincipalID: bound.ID, ScopeType: store.RoleScopeSystem, CreatedBy: "test"})
	require.NoError(t, err)
	nested := newGroup("gsb-nested")
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{GroupID: bound.ID, MemberType: store.GroupMemberTypeGroup, MemberID: nested.ID, Role: store.GroupMemberRoleMember}))

	key, _, err := srv.uatService.CreateTokenWithParams(rs4MintContext(adminID), CreateTokenParams{
		UserID: adminID, Name: "gsb", Boundary: hubBoundary(), Scopes: []string{"group:addMember", "group:read", "group:create"},
	})
	require.NoError(t, err)

	target := tid("gsb-target")
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: target, Email: target + "@test.com", DisplayName: "t", Role: "member", Status: "active"}))
	for _, g := range []*store.Group{bound, nested} {
		rec := doRequestWithUAT(t, srv, key, http.MethodPost, "/api/v1/groups/"+g.ID+"/members", addUserBody(target))
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", g.Slug, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "system-scoped group authority", g.Slug)
		_, err := s.GetGroupMembership(ctx, g.ID, store.GroupMemberTypeUser, target)
		assert.ErrorIs(t, err, store.ErrNotFound, "%s: the refused add adds no member", g.Slug)
	}

	rec := doRequestWithUAT(t, srv, key, http.MethodPost, "/api/v1/groups", map[string]interface{}{"name": "gsb child", "slug": "gsb-child", "parentId": bound.ID})
	assert.Equal(t, http.StatusForbidden, rec.Code, "a child group under a system-bound parent: %s", rec.Body.String())
	_, err = s.GetGroupBySlug(ctx, "gsb-child")
	assert.ErrorIs(t, err, store.ErrNotFound, "the refused create writes no group")

	recSession := doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/groups/"+bound.ID+"/members", addUserBody(target))
	assert.Equal(t, http.StatusCreated, recSession.Code, "a super-admin session adds the member: %s", recSession.Body.String())
}

// TestProjectAgentsGroup_UserMembersRefused pins that the members of a
// project_agents group are the project's agents: adding a user, or an
// agent, to one is refused with 400 for a session and for a user access
// token alike, and nothing is written.
func TestProjectAgentsGroup_UserMembersRefused(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	projectID := tid("gpa2-project")
	createRS1Project(t, s, projectID, tid("gpa2-owner"))
	adminID := tid("gpa2-admin")
	createTestUserWithRole(t, s, adminID, adminID+"@test.com", "admin", store.SystemRoleSuperAdmin)
	ensureHubMembership(ctx, s, adminID)
	admin, err := s.GetUser(ctx, adminID)
	require.NoError(t, err)

	g := &store.Group{ID: tid("gpa2-agents"), Slug: "gpa2-agents", Name: "agents", ProjectID: projectID, GroupType: store.GroupTypeProjectAgents, OwnerID: adminID}
	require.NoError(t, s.CreateGroup(ctx, g))
	agentID := tid("gpa2-agent")
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{ID: agentID, Slug: "gpa2-agent", Name: "gpa2 agent", ProjectID: projectID}))
	userID := tid("gpa2-user")
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: userID, Email: userID + "@test.com", DisplayName: "u", Role: "member", Status: "active"}))

	key, _, err := srv.uatService.CreateTokenWithParams(rs4MintContext(adminID), CreateTokenParams{
		UserID: adminID, Name: "gpa2", Boundary: hubBoundary(), Scopes: []string{"group:addMember"},
	})
	require.NoError(t, err)

	bodies := map[string]map[string]interface{}{
		"user":  addUserBody(userID),
		"agent": {"memberType": store.GroupMemberTypeAgent, "memberId": agentID, "role": store.GroupMemberRoleMember},
	}
	path := "/api/v1/groups/" + g.ID + "/members"
	for name, body := range bodies {
		session := doRequestAsUser(t, srv, admin, http.MethodPost, path, body)
		assert.Equal(t, http.StatusBadRequest, session.Code, "session %s member: %s", name, session.Body.String())
		token := doRequestWithUAT(t, srv, key, http.MethodPost, path, body)
		assert.Equal(t, http.StatusBadRequest, token.Code, "token %s member: %s", name, token.Body.String())
		assert.Empty(t, grantAudits(t, s, "group_member_add", g.ID), "%s: no audit record is written", name)
	}
	_, err = s.GetGroupMembership(ctx, g.ID, store.GroupMemberTypeUser, userID)
	assert.ErrorIs(t, err, store.ErrNotFound, "no user member is written")
}

// TestExplicitGroupToken_ResolvesHubTarget pins that a group with no
// project resolves to the hub target for a user access token, and that the
// group list shows a token exactly the rows its per-row group.read decision
// admits: a hub token with group:read sees the explicit groups its holder
// may read; a token without the selector sees none of them; a project
// boundary cannot carry group:read. A session of the same holder lists the
// explicit groups as well.
func TestExplicitGroupToken_ResolvesHubTarget(t *testing.T) {
	f := newGroupTokenFixture(t, "geh")
	ctx := context.Background()

	explicit := f.group(t, "geh-explicit")
	other := &store.Group{ID: tid("geh-other"), Slug: "geh-other", Name: "geh-other", GroupType: store.GroupTypeExplicit}
	require.NoError(t, f.s.CreateGroup(ctx, other))
	projectGroup := &store.Group{ID: tid("geh-project"), Slug: "geh-project", Name: "geh-project", ProjectID: f.projectID}
	require.NoError(t, f.s.CreateGroup(ctx, projectGroup))

	r := groupResource(explicit)
	assert.Equal(t, "system", r.ParentType)
	assert.Empty(t, r.ParentID)
	assert.True(t, isHubScopedResource(r))
	assert.Equal(t, TargetScopeHub, ResolveTargetScope(r, TargetScopeEvidence{}).Kind)
	pr := groupResource(projectGroup)
	assert.Equal(t, TargetScope{Kind: TargetScopeProject, ProjectID: f.projectID}, ResolveTargetScope(pr, TargetScopeEvidence{}))

	listIDs := func(rec int, body []byte) map[string]bool {
		require.Equal(t, http.StatusOK, rec, string(body))
		var resp ListGroupsResponse
		require.NoError(t, json.Unmarshal(body, &resp))
		out := map[string]bool{}
		for _, g := range resp.Groups {
			out[g.ID] = true
		}
		return out
	}

	readKey := f.mint(t, hubBoundary(), "group:read")
	rec := doRequestWithUAT(t, f.srv, readKey, http.MethodGet, "/api/v1/groups?limit=100", nil)
	seen := listIDs(rec.Code, rec.Body.Bytes())
	assert.True(t, seen[explicit.ID], "a hub token with group:read lists an explicit group its holder may read")
	assert.True(t, seen[other.ID], "the holder's system group.read covers every explicit group")
	readToken, err := f.srv.uatService.ValidateToken(ctx, readKey)
	require.NoError(t, err)
	for id := range seen {
		g, err := f.s.GetGroup(ctx, id)
		require.NoError(t, err)
		d := f.srv.authzService.CheckAccess(ctx, readToken, groupResource(g), ActionRead)
		assert.True(t, d.Allowed, "every listed row is one the per-row decision admits: %s (%s)", g.Slug, d.Reason)
	}
	d := f.srv.authzService.CheckAccess(ctx, readToken, groupResource(explicit), ActionRead)
	assert.True(t, d.Allowed, d.Reason)

	unrelated := f.mint(t, hubBoundary(), "project:read")
	rec = doRequestWithUAT(t, f.srv, unrelated, http.MethodGet, "/api/v1/groups?limit=100", nil)
	seen = listIDs(rec.Code, rec.Body.Bytes())
	assert.False(t, seen[explicit.ID], "a token without group:read lists no explicit group")
	assert.False(t, seen[other.ID])

	_, _, err = f.srv.uatService.CreateTokenWithParams(rs4MintContext(f.callerID), CreateTokenParams{
		UserID: f.callerID, Name: "geh-project", Boundary: projectBoundary(f.projectID), Scopes: []string{"group:read"},
	})
	assert.Error(t, err, "group:read is not mintable on a project boundary")
	legacy := uatpInsertLegacyToken(t, f.s, f.callerID, f.projectID, []string{"group:read"})
	rec = doRequestWithUAT(t, f.srv, legacy, http.MethodGet, "/api/v1/groups?limit=100", nil)
	seen = listIDs(rec.Code, rec.Body.Bytes())
	assert.False(t, seen[explicit.ID], "a project-boundary row carrying group:read lists no explicit group")

	session := doRequestAsUser(t, f.srv, f.caller, http.MethodGet, "/api/v1/groups?limit=100", nil)
	seen = listIDs(session.Code, session.Body.Bytes())
	assert.True(t, seen[explicit.ID], "a session lists the explicit group")
	assert.True(t, seen[other.ID], "a session lists every explicit group its holder may read")
}

// TestGroupChangeToken_SelectorAndBoundaryPerRoute pins, for update,
// delete, member add and member removal on an explicit group, that a user
// access token is admitted with the route's group selector on a hub
// boundary, refused without it, and refused on a project boundary, and
// that a session makes the same change. Member add runs as the group's
// owner on a group with no role binding, so the group role hierarchy and
// the delegation check both pass.
func TestGroupChangeToken_SelectorAndBoundaryPerRoute(t *testing.T) {
	type route struct {
		name     string
		selector string
		method   string
		path     func(g *store.Group, member string) string
		body     func(member string) interface{}
		// existing is whether the route acts on a user who is already a
		// member of the group.
		existing bool
		done     func(t *testing.T, f *groupTokenFixture, g *store.Group, member string) bool
	}
	routes := []route{
		{"update", "group:update", http.MethodPatch,
			func(g *store.Group, _ string) string { return "/api/v1/groups/" + g.ID },
			func(string) interface{} { return map[string]interface{}{"description": "changed"} }, true,
			func(t *testing.T, f *groupTokenFixture, g *store.Group, _ string) bool {
				got, err := f.s.GetGroup(context.Background(), g.ID)
				require.NoError(t, err)
				return got.Description == "changed"
			}},
		{"delete", "group:delete", http.MethodDelete,
			func(g *store.Group, _ string) string { return "/api/v1/groups/" + g.ID }, noBody, true,
			func(t *testing.T, f *groupTokenFixture, g *store.Group, _ string) bool {
				_, err := f.s.GetGroup(context.Background(), g.ID)
				return err != nil
			}},
		{"add member", "group:addMember", http.MethodPost,
			func(g *store.Group, _ string) string { return "/api/v1/groups/" + g.ID + "/members" },
			func(m string) interface{} { return addUserBody(m) }, false,
			func(t *testing.T, f *groupTokenFixture, g *store.Group, m string) bool {
				return f.isMember(t, g, store.GroupMemberTypeUser, m)
			}},
		{"remove member", "group:removeMember", http.MethodDelete,
			func(g *store.Group, m string) string { return "/api/v1/groups/" + g.ID + "/members/user/" + m }, noBody, true,
			func(t *testing.T, f *groupTokenFixture, g *store.Group, m string) bool {
				return !f.isMember(t, g, store.GroupMemberTypeUser, m)
			}},
	}
	for _, rt := range routes {
		t.Run(rt.name, func(t *testing.T) {
			f := newGroupTokenFixture(t, "gsr")
			setup := func(name string) (*store.Group, string) {
				g := f.group(t, name)
				m := f.user(t, name+"-member")
				if rt.existing {
					require.NoError(t, f.s.AddGroupMember(context.Background(), &store.GroupMember{GroupID: g.ID, MemberType: store.GroupMemberTypeUser, MemberID: m, Role: store.GroupMemberRoleMember}))
				}
				return g, m
			}
			var others []string
			for _, sel := range groupTokenGroupSelectors {
				if sel != rt.selector {
					others = append(others, sel)
				}
			}

			g, m := setup("gsr-refused")
			rec := doRequestWithUAT(t, f.srv, f.mint(t, hubBoundary(), others...), rt.method, rt.path(g, m), rt.body(m))
			assert.Equal(t, http.StatusForbidden, rec.Code, "without %s: %s", rt.selector, rec.Body.String())
			assert.False(t, rt.done(t, f, g, m), "the refused change does not happen")

			_, _, err := f.srv.uatService.CreateTokenWithParams(rs4MintContext(f.callerID), CreateTokenParams{
				UserID: f.callerID, Name: "gsr-project", Boundary: projectBoundary(f.projectID), Scopes: []string{rt.selector},
			})
			assert.Error(t, err, "%s is not mintable on a project boundary", rt.selector)
			legacy := uatpInsertLegacyToken(t, f.s, f.callerID, f.projectID, []string{rt.selector})
			rec = doRequestWithUAT(t, f.srv, legacy, rt.method, rt.path(g, m), rt.body(m))
			assert.Equal(t, http.StatusForbidden, rec.Code, "a project-boundary row carrying %s: %s", rt.selector, rec.Body.String())
			assert.False(t, rt.done(t, f, g, m), "the refused change does not happen")

			rec = doRequestWithUAT(t, f.srv, f.mint(t, hubBoundary(), rt.selector), rt.method, rt.path(g, m), rt.body(m))
			assert.Less(t, rec.Code, 300, "with %s on a hub boundary: %d %s", rt.selector, rec.Code, rec.Body.String())
			assert.True(t, rt.done(t, f, g, m), "the admitted change happens")

			g2, m2 := setup("gsr-session")
			assert.Less(t, f.session(t, rt.method, rt.path(g2, m2), rt.body(m2)), 300, "a session makes the change")
			assert.True(t, rt.done(t, f, g2, m2))
		})
	}
}

// TestGroupCreateToken_RequiresCreateSelectorOnHubBoundary pins that group
// create targets the hub's group collection: a user access token creates a
// group with group:create on a hub boundary, is refused without it, and is
// refused on a project boundary; a session creates a group too.
func TestGroupCreateToken_RequiresCreateSelectorOnHubBoundary(t *testing.T) {
	f := newGroupTokenFixture(t, "gcr")
	ctx := context.Background()
	create := func(key, slug string) int {
		rec := doRequestWithUAT(t, f.srv, key, http.MethodPost, "/api/v1/groups", map[string]interface{}{"name": slug, "slug": slug})
		return rec.Code
	}
	exists := func(slug string) bool {
		_, err := f.s.GetGroupBySlug(ctx, slug)
		return err == nil
	}

	var others []string
	for _, sel := range groupTokenGroupSelectors {
		if sel != "group:create" {
			others = append(others, sel)
		}
	}
	assert.Equal(t, http.StatusForbidden, create(f.mint(t, hubBoundary(), others...), "gcr-without"))
	assert.False(t, exists("gcr-without"))

	legacy := uatpInsertLegacyToken(t, f.s, f.callerID, f.projectID, []string{"group:create"})
	assert.Equal(t, http.StatusForbidden, create(legacy, "gcr-project"))
	assert.False(t, exists("gcr-project"))

	assert.Equal(t, http.StatusCreated, create(f.mint(t, hubBoundary(), "group:create"), "gcr-with"))
	require.True(t, exists("gcr-with"))
	created, err := f.s.GetGroupBySlug(ctx, "gcr-with")
	require.NoError(t, err)
	assert.Equal(t, f.callerID, created.OwnerID, "the token holder owns the created group")
	assert.Empty(t, created.ProjectID)

	assert.Equal(t, http.StatusCreated, f.session(t, http.MethodPost, "/api/v1/groups", map[string]interface{}{"name": "gcr-session", "slug": "gcr-session"}))
	assert.True(t, exists("gcr-session"))
}

// TestGroupMemberAddToken_HeldToGroupRoleHierarchy pins that a user access
// token never takes the platform-admin exemptions of the group routes: a
// super-admin's hub token with every group selector is held to the group
// role hierarchy (only the group's owner or an owner or admin member adds
// members), and gets the per-row group list, not the unfiltered admin
// view. A session of the same super-admin adds the member.
func TestGroupMemberAddToken_HeldToGroupRoleHierarchy(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	adminID := tid("ghr-admin")
	createTestUserWithRole(t, s, adminID, adminID+"@test.com", "admin", store.SystemRoleSuperAdmin)
	ensureHubMembership(ctx, s, adminID)
	admin, err := s.GetUser(ctx, adminID)
	require.NoError(t, err)

	g := &store.Group{ID: tid("ghr-group"), Slug: "ghr-group", Name: "ghr", GroupType: store.GroupTypeExplicit}
	require.NoError(t, s.CreateGroup(ctx, g))
	target := tid("ghr-target")
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: target, Email: target + "@test.com", DisplayName: "t", Role: "member", Status: "active"}))

	key, _, err := srv.uatService.CreateTokenWithParams(rs4MintContext(adminID), CreateTokenParams{
		UserID: adminID, Name: "ghr", Boundary: hubBoundary(), Scopes: groupTokenGroupSelectors,
	})
	require.NoError(t, err)

	rec := doRequestWithUAT(t, srv, key, http.MethodPost, "/api/v1/groups/"+g.ID+"/members", addUserBody(target))
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "Only group owners or admins can add members")
	_, err = s.GetGroupMembership(ctx, g.ID, store.GroupMemberTypeUser, target)
	assert.ErrorIs(t, err, store.ErrNotFound)

	// The list: a token carrying group:list but not group:read gets no
	// rows, because each row needs group.read; the session's admin view
	// lists the group.
	listKey, _, err := srv.uatService.CreateTokenWithParams(rs4MintContext(adminID), CreateTokenParams{
		UserID: adminID, Name: "ghr-list", Boundary: hubBoundary(), Scopes: []string{"group:list"},
	})
	require.NoError(t, err)
	rec = doRequestWithUAT(t, srv, listKey, http.MethodGet, "/api/v1/groups?limit=100", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp ListGroupsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Empty(t, resp.Groups, "a token gets the per-row list, so group:list alone shows no row")

	session := doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/groups?limit=100", nil)
	require.Equal(t, http.StatusOK, session.Code, session.Body.String())
	resp = ListGroupsResponse{}
	require.NoError(t, json.Unmarshal(session.Body.Bytes(), &resp))
	found := false
	for _, row := range resp.Groups {
		found = found || row.ID == g.ID
	}
	assert.True(t, found, "the super-admin session lists the group")

	recSession := doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/groups/"+g.ID+"/members", addUserBody(target))
	assert.Equal(t, http.StatusCreated, recSession.Code, recSession.Body.String())
}

// TestGroupMembershipToken_CannotCarryAuthorityBeyondCaller pins that a
// user access token cannot place a principal into a group whose closure
// carries authority beyond the token's own: not a user, not a group (whose
// whole subtree would gain the closure's authority), not through an
// ancestor group's binding, and not by creating a child group under a bound
// parent. Each refusal writes no membership, no group and no audit record.
func TestGroupMembershipToken_CannotCarryAuthorityBeyondCaller(t *testing.T) {
	f := newGroupTokenFixture(t, "gcb")
	ctx := context.Background()

	// A wider project role in the same project: the caller holds only the
	// narrow fixture role there.
	wide, err := f.s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name: "gcb-wide", Description: "project read, agent read and agent create", ScopeType: store.RoleScopeProject,
		Permissions: []string{"project.read", "agent.read", "agent.create"},
	})
	require.NoError(t, err)
	top := f.group(t, "gcb-top")
	f.bindGroup(t, top, wide.ID, store.RoleScopeProject, f.projectID)
	inner := f.group(t, "gcb-inner")
	f.nest(t, top, inner)
	// A second project where the caller has no project read.
	otherProject := tid("gcb-other-project")
	createRS1Project(t, f.s, otherProject, tid("gcb-other-owner"))
	elsewhere := f.group(t, "gcb-elsewhere")
	f.bindGroup(t, elsewhere, f.roleID, store.RoleScopeProject, otherProject)
	// The caller holds agent.create only in the second project, so the
	// agent:create selector is mintable on a hub boundary while the caller
	// still lacks agent.create in the first project and project read in
	// the second.
	createOnly, err := f.s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name: "gcb-create", Description: "agent create", ScopeType: store.RoleScopeProject,
		Permissions: []string{"agent.create"},
	})
	require.NoError(t, err)
	_, err = f.s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: createOnly.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: f.callerID,
		ScopeType: store.RoleScopeProject, ScopeID: otherProject, CreatedBy: "test",
	})
	require.NoError(t, err)

	subtree := f.group(t, "gcb-subtree")
	subUser := f.user(t, "gcb-subtree-user")
	require.NoError(t, f.s.AddGroupMember(ctx, &store.GroupMember{GroupID: subtree.ID, MemberType: store.GroupMemberTypeUser, MemberID: subUser, Role: store.GroupMemberRoleMember}))

	key := f.mint(t, hubBoundary(), withSelectors(groupTokenGroupSelectors, groupTokenRoleSelectors, []string{"agent:create"})...)

	for _, g := range []*store.Group{top, inner, elsewhere} {
		target := f.user(t, g.Slug+"-user")
		rec := doRequestWithUAT(t, f.srv, key, http.MethodPost, "/api/v1/groups/"+g.ID+"/members", addUserBody(target))
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s user: %s", g.Slug, rec.Body.String())
		assert.False(t, f.isMember(t, g, store.GroupMemberTypeUser, target))

		rec = doRequestWithUAT(t, f.srv, key, http.MethodPost, "/api/v1/groups/"+g.ID+"/members",
			map[string]interface{}{"memberType": store.GroupMemberTypeGroup, "memberId": subtree.ID, "role": store.GroupMemberRoleMember})
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s group: %s", g.Slug, rec.Body.String())
		assert.False(t, f.isMember(t, g, store.GroupMemberTypeGroup, subtree.ID))
		assert.Empty(t, grantAudits(t, f.s, "group_member_add", g.ID), "%s: no audit record", g.Slug)

		slug := g.Slug + "-child"
		rec = doRequestWithUAT(t, f.srv, key, http.MethodPost, "/api/v1/groups", map[string]interface{}{"name": slug, "slug": slug, "parentId": g.ID})
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s child create: %s", g.Slug, rec.Body.String())
		_, err := f.s.GetGroupBySlug(ctx, slug)
		assert.ErrorIs(t, err, store.ErrNotFound, "%s: the refused create writes no group", g.Slug)
	}
	effective, err := f.s.GetEffectiveGroups(ctx, subUser)
	require.NoError(t, err)
	for _, g := range []*store.Group{top, inner, elsewhere} {
		assert.NotContains(t, effective, g.ID, "the subtree user gains no bound group")
	}
}

// groupClosureChangingStore adds a role binding on one group, inside the
// membership transaction, before the handler's transaction body runs, so
// the closure the transaction sees differs from the one the delegation
// check read.
type groupClosureChangingStore struct {
	store.Store
	groupID, roleID, projectID string
}

func (s *groupClosureChangingStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	return s.Store.WithTx(ctx, func(tx store.Store) error {
		if _, err := tx.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: s.roleID, PrincipalType: store.RoleBindingPrincipalGroup, PrincipalID: s.groupID,
			ScopeType: store.RoleScopeProject, ScopeID: s.projectID, CreatedBy: "test",
		}); err != nil {
			return err
		}
		return fn(tx)
	})
}

// TestGroupMembershipToken_ClosureChangeBeforeWriteRefused pins that a
// token's membership is written only under the closure its delegation
// check evaluated: when the group's role bindings change between the check
// and the membership write, the transaction refuses with 409 and nothing is
// written. A session's add is not re-checked.
func TestGroupMembershipToken_ClosureChangeBeforeWriteRefused(t *testing.T) {
	f := newGroupTokenFixture(t, "gtc")
	ctx := context.Background()
	g := f.group(t, "gtc-group")
	target := f.user(t, "gtc-target")
	key := f.mint(t, hubBoundary(), withSelectors([]string{"group:addMember"}, groupTokenRoleSelectors)...)

	orig := f.srv.store
	f.srv.store = &groupClosureChangingStore{Store: orig, groupID: g.ID, roleID: f.roleID, projectID: f.projectID}
	rec := doRequestWithUAT(t, f.srv, key, http.MethodPost, "/api/v1/groups/"+g.ID+"/members", addUserBody(target))
	f.srv.store = orig

	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.False(t, f.isMember(t, g, store.GroupMemberTypeUser, target), "nothing is written")
	assert.Empty(t, grantAudits(t, f.s, "group_member_add", g.ID))
	bindings, err := f.s.ListRoleBindingsForPrincipals(ctx, []store.PrincipalRef{{Type: store.RoleBindingPrincipalGroup, ID: g.ID}}, nil, nil)
	require.NoError(t, err)
	assert.Empty(t, bindings, "the transaction rolled back as a whole")

	f.srv.store = &groupClosureChangingStore{Store: orig, groupID: g.ID, roleID: f.roleID, projectID: f.projectID}
	code := f.session(t, http.MethodPost, "/api/v1/groups/"+g.ID+"/members", addUserBody(target))
	f.srv.store = orig
	assert.Equal(t, http.StatusCreated, code, "a session's add is governed by group roles and not re-checked")
}

// groupOwnerTokenRefusalBody is the response body of the one refusal a
// user access token gets when it may not set a group's owner.
func groupOwnerTokenRefusalBody() string {
	rec := httptest.NewRecorder()
	writeForbidden(rec, groupOwnerTokenRefusal)
	return rec.Body.String()
}

func (f *groupTokenFixture) ownerOf(t *testing.T, g *store.Group) string {
	t.Helper()
	got, err := f.s.GetGroup(context.Background(), g.ID)
	require.NoError(t, err)
	return got.OwnerID
}

// TestGroupOwnerChangeToken_RequiresDelegationForNewOwner pins that a user
// access token that changes a group's owner needs the holder's live update
// authority on the group and, for the new owner, the delegation check of a
// member add against the group's closure; group:addMember and the group
// role hierarchy do not apply. Every refusal is the same 403 with the
// same body: for a token without the closure's authority, for an unknown
// group, for a token without group:update, and for a token whose holder
// lost the update authority after the token was issued. Other fields are
// updated as for any update. A session changes the owner.
func TestGroupOwnerChangeToken_RequiresDelegationForNewOwner(t *testing.T) {
	f := newGroupTokenFixture(t, "goc")
	ctx := context.Background()
	refusal := groupOwnerTokenRefusalBody()
	patch := func(key string, g string, body map[string]interface{}) (int, string) {
		rec := doRequestWithUAT(t, f.srv, key, http.MethodPatch, "/api/v1/groups/"+g, body)
		return rec.Code, rec.Body.String()
	}
	setOwner := func(id string) map[string]interface{} { return map[string]interface{}{"ownerId": id} }

	bound := f.group(t, "goc-bound")
	f.bindGroup(t, bound, f.roleID, store.RoleScopeProject, f.projectID)
	newOwner := f.user(t, "goc-new-owner")

	without := f.mint(t, hubBoundary(), "group:update")
	code, body := patch(without, bound.ID, setOwner(newOwner))
	assert.Equal(t, http.StatusForbidden, code, body)
	assert.Equal(t, refusal, body, "a token without the closure's project authority gets the fixed refusal")
	assert.Equal(t, f.callerID, f.ownerOf(t, bound), "the refused change writes no owner")

	code, body = patch(without, bound.ID, map[string]interface{}{"description": "goc-described"})
	assert.Equal(t, http.StatusOK, code, "the same token updates other fields: %s", body)

	code, body = patch(without, tid("goc-unknown"), setOwner(newOwner))
	assert.Equal(t, http.StatusForbidden, code, body)
	assert.Equal(t, refusal, body, "an unknown group gets the same refusal")

	// The token holds the closure's authority, so only the group:update
	// check refuses it.
	readOnly := f.mint(t, hubBoundary(), withSelectors([]string{"group:read"}, groupTokenRoleSelectors)...)
	code, body = patch(readOnly, bound.ID, setOwner(newOwner))
	assert.Equal(t, http.StatusForbidden, code, body)
	assert.Equal(t, refusal, body, "a token holding the closure's authority without group:update gets the same refusal")
	assert.Equal(t, f.callerID, f.ownerOf(t, bound))

	with := f.mint(t, hubBoundary(), withSelectors([]string{"group:update"}, groupTokenRoleSelectors)...)
	code, body = patch(with, bound.ID, setOwner(newOwner))
	require.Equal(t, http.StatusOK, code, "a token holding the closure's project authority: %s", body)
	assert.Equal(t, newOwner, f.ownerOf(t, bound))

	sessionOwner := f.user(t, "goc-session-owner")
	assert.Equal(t, http.StatusOK, f.session(t, http.MethodPatch, "/api/v1/groups/"+bound.ID, setOwner(sessionOwner)),
		"a session changes the owner of a group with a project-scoped binding")
	assert.Equal(t, sessionOwner, f.ownerOf(t, bound))

	// Current authority decides: a token whose holder does not hold
	// group.update gets the same refusal. The groups are owned by another
	// user, so the update authority comes only from the holder's role.
	elsewhereOwner := f.user(t, "goc-other")
	before := f.group(t, "goc-before")
	after := f.group(t, "goc-after")
	for _, g := range []*store.Group{before, after} {
		g.OwnerID = elsewhereOwner
		require.NoError(t, f.s.UpdateGroup(ctx, g))
	}
	demoted := f.mint(t, hubBoundary(), "group:update")
	code, body = patch(demoted, before.ID, setOwner(newOwner))
	require.Equal(t, http.StatusOK, code, "the holder's role admits the change: %s", body)
	groupRole, err := f.s.GetRoleDefinitionByName(ctx, "goc-groups", store.RoleScopeSystem)
	require.NoError(t, err)
	bindings, err := f.s.ListRoleBindingsForPrincipals(ctx, []store.PrincipalRef{{Type: store.RoleBindingPrincipalUser, ID: f.callerID}}, []string{store.RoleScopeSystem}, nil)
	require.NoError(t, err)
	removed := 0
	for _, b := range bindings {
		if b.RoleDefinitionID == groupRole.ID {
			require.NoError(t, f.s.DeleteRoleBinding(ctx, b.ID))
			removed++
		}
	}
	require.Equal(t, 1, removed, "the holder's group role binding is removed")
	code, body = patch(demoted, after.ID, setOwner(newOwner))
	assert.Equal(t, http.StatusForbidden, code, body)
	assert.Equal(t, refusal, body, "a token whose holder lost group.update gets the same refusal")
	assert.Equal(t, elsewhereOwner, f.ownerOf(t, after))
}

// TestGroupOwnerChangeToken_ClosureChangeBeforeWriteRefused pins that a
// token's owner change is written only under the closure its delegation
// check evaluated: when the group's role bindings change between the check
// and the write, the request is refused with 409 and nothing is written.
func TestGroupOwnerChangeToken_ClosureChangeBeforeWriteRefused(t *testing.T) {
	f := newGroupTokenFixture(t, "got")
	ctx := context.Background()
	g := f.group(t, "got-group")
	newOwner := f.user(t, "got-new-owner")
	key := f.mint(t, hubBoundary(), withSelectors([]string{"group:update"}, groupTokenRoleSelectors)...)

	orig := f.srv.store
	f.srv.store = &groupClosureChangingStore{Store: orig, groupID: g.ID, roleID: f.roleID, projectID: f.projectID}
	rec := doRequestWithUAT(t, f.srv, key, http.MethodPatch, "/api/v1/groups/"+g.ID, map[string]interface{}{"ownerId": newOwner})
	f.srv.store = orig

	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Equal(t, f.callerID, f.ownerOf(t, g), "nothing is written")
	bindings, err := f.s.ListRoleBindingsForPrincipals(ctx, []store.PrincipalRef{{Type: store.RoleBindingPrincipalGroup, ID: g.ID}}, nil, nil)
	require.NoError(t, err)
	assert.Empty(t, bindings, "the transaction rolled back as a whole")
}

// TestGroupCreateToken_ChildWrittenUnderCheckedParentClosure pins that a
// user access token creates a group under a parent whose closure it may
// delegate (201, the child under the parent, one membership audit record),
// and that the child is written only under the parent closure the
// delegation check evaluated: when the parent's role bindings change
// between the check and the write, the request is refused with 409 and no
// group, child edge, binding or audit record is written.
func TestGroupCreateToken_ChildWrittenUnderCheckedParentClosure(t *testing.T) {
	f := newGroupTokenFixture(t, "gcc")
	ctx := context.Background()
	key := f.mint(t, hubBoundary(), withSelectors(groupTokenGroupSelectors, groupTokenRoleSelectors)...)
	create := func(slug, parentID string) *httptest.ResponseRecorder {
		return doRequestWithUAT(t, f.srv, key, http.MethodPost, "/api/v1/groups", map[string]interface{}{"name": slug, "slug": slug, "parentId": parentID})
	}
	children := func(parentID string) []store.Group {
		list, err := f.s.ListGroups(ctx, store.GroupFilter{ParentID: parentID}, store.ListOptions{})
		require.NoError(t, err)
		return list.Items
	}

	plain := f.group(t, "gcc-plain")
	rec := create("gcc-plain-child", plain.ID)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var child store.Group
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&child))
	assert.Equal(t, plain.ID, child.ParentID)
	got := children(plain.ID)
	require.Len(t, got, 1)
	assert.Equal(t, child.ID, got[0].ID)
	audits := grantAudits(t, f.s, "group_member_add", plain.ID)
	require.Len(t, audits, 1)
	assert.Equal(t, "allow", audits[0].CanDelegateResult)
	assert.Equal(t, f.callerID, audits[0].ActorPrincipalID)

	parent := f.group(t, "gcc-parent")
	orig := f.srv.store
	f.srv.store = &groupClosureChangingStore{Store: orig, groupID: parent.ID, roleID: f.roleID, projectID: f.projectID}
	rec = create("gcc-changed-child", parent.ID)
	f.srv.store = orig
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	_, err := f.s.GetGroupBySlug(ctx, "gcc-changed-child")
	assert.ErrorIs(t, err, store.ErrNotFound, "no group is written")
	assert.Empty(t, children(parent.ID), "no child edge is written")
	assert.Empty(t, grantAudits(t, f.s, "group_member_add", parent.ID))
	bindings, err := f.s.ListRoleBindingsForPrincipals(ctx, []store.PrincipalRef{{Type: store.RoleBindingPrincipalGroup, ID: parent.ID}}, nil, nil)
	require.NoError(t, err)
	assert.Empty(t, bindings, "the transaction rolled back as a whole")
}

// unresolvedRoleStore reports one role definition as not resolving, either
// as store.ErrNotFound or as a nil definition.
type unresolvedRoleStore struct {
	store.Store
	roleID string
	asNil  bool
}

func (s *unresolvedRoleStore) GetRoleDefinition(ctx context.Context, id string) (*store.RoleDefinition, error) {
	if id == s.roleID {
		if s.asNil {
			return nil, nil
		}
		return nil, store.ErrNotFound
	}
	return s.Store.GetRoleDefinition(ctx, id)
}

// unresolvedRoleTxStore runs each transaction on an unresolvedRoleStore, so
// only reads inside the transaction see the role as not resolving.
type unresolvedRoleTxStore struct {
	store.Store
	roleID string
	asNil  bool
}

func (s *unresolvedRoleTxStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	return s.Store.WithTx(ctx, func(tx store.Store) error {
		return fn(&unresolvedRoleStore{Store: tx, roleID: s.roleID, asNil: s.asNil})
	})
}

// TestGroupMemberAddToken_UnresolvedClosureRoleIsInternalError pins that a
// closure role binding whose role definition does not resolve when the
// membership transaction re-reads the closure refuses the token's add with
// 500, not 404, and writes nothing.
func TestGroupMemberAddToken_UnresolvedClosureRoleIsInternalError(t *testing.T) {
	for _, asNil := range []bool{false, true} {
		f := newGroupTokenFixture(t, "gur")
		g := f.group(t, "gur-group")
		bindingID := f.bindGroup(t, g, f.roleID, store.RoleScopeProject, f.projectID)
		target := f.user(t, "gur-target")
		key := f.mint(t, hubBoundary(), withSelectors([]string{"group:addMember"}, groupTokenRoleSelectors)...)

		orig := f.srv.store
		f.srv.store = &unresolvedRoleTxStore{Store: orig, roleID: f.roleID, asNil: asNil}
		rec := doRequestWithUAT(t, f.srv, key, http.MethodPost, "/api/v1/groups/"+g.ID+"/members", addUserBody(target))
		f.srv.store = orig

		assert.Equal(t, http.StatusInternalServerError, rec.Code, "nil definition %v: %s", asNil, rec.Body.String())
		assert.NotContains(t, rec.Body.String(), f.roleID, "the response names no role definition")
		assert.NotContains(t, rec.Body.String(), bindingID, "the response names no role binding")
		assert.False(t, f.isMember(t, g, store.GroupMemberTypeUser, target), "nothing is written")
		assert.Empty(t, grantAudits(t, f.s, "group_member_add", g.ID))
	}
}

// TestCanDelegate_GroupMembershipToken_UnresolvedClosureRoleDenied pins
// that the delegation check of a user access token refuses a group
// membership when a project-scoped binding on the group's closure names a
// role definition that does not resolve, and admits the same membership
// when it resolves.
func TestCanDelegate_GroupMembershipToken_UnresolvedClosureRoleDenied(t *testing.T) {
	f := newGroupTokenFixture(t, "gud")
	ctx := context.Background()
	closureRole, err := f.s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name: "gud-closure", Description: "project read", ScopeType: store.RoleScopeProject,
		Permissions: []string{"project.read"},
	})
	require.NoError(t, err)
	g := f.group(t, "gud-group")
	f.bindGroup(t, g, closureRole.ID, store.RoleScopeProject, f.projectID)

	holder := NewAuthenticatedUser(f.callerID, f.caller.Email, f.caller.DisplayName, "member", "api")
	token := NewScopedUserIdentityWithBoundaryAndDecoration(holder, hubBoundary(), groupTokenRoleSelectors, tid("gud-cred"), bearerCeiling(t, groupTokenRoleSelectors...), nil)
	grant := GrantDescriptor{Type: GrantTypeGroupMembership, GroupID: g.ID, GranteeType: store.GroupMemberTypeUser, GranteeID: f.user(t, "gud-target")}

	authz := f.srv.authzService
	decision := authz.CanDelegate(ctx, token, grant)
	require.True(t, decision.Allowed, "the resolving role is delegated: %s", decision.Reason)

	orig := authz.store
	defer func() { authz.store = orig }()
	for _, asNil := range []bool{false, true} {
		authz.store = &unresolvedRoleStore{Store: orig, roleID: closureRole.ID, asNil: asNil}
		decision = authz.CanDelegate(ctx, token, grant)
		assert.False(t, decision.Allowed, "nil definition %v", asNil)
		assert.Equal(t, "cannot resolve role definition for group binding", decision.Reason)
	}
}
