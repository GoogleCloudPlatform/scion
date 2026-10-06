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
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGroupChange_TokenRefusedWhenClosureCarriesRoleBinding pins the group
// rule for user access tokens: updating or deleting a group, and adding or
// removing a member, are refused for a token (403, session-only reason
// GOV_PENDING) when the group or a group that contains it is the principal
// of a role binding. The same token changes a group with no role binding in
// its closure, and a session changes a group that carries one.
func TestGroupChange_TokenRefusedWhenClosureCarriesRoleBinding(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("grb-project")
	ownerID := tid("grb-owner")
	createRS1Project(t, s, projectID, ownerID)
	adminID := tid("grb-admin")
	createTestUserWithRole(t, s, adminID, adminID+"@test.com", "admin", store.SystemRoleSuperAdmin)
	ensureHubMembership(ctx, s, adminID)

	newUser := func(name string) string {
		id := tid("grb-" + name)
		require.NoError(t, s.CreateUser(ctx, &store.User{ID: id, Email: id + "@test.com", DisplayName: name, Role: "member", Status: "active"}))
		return id
	}
	newGroup := func(name string) *store.Group {
		// The token holder owns each group, which clears the group role
		// hierarchy check of member changes; the rule under test runs
		// after it.
		g := &store.Group{ID: tid("grb-" + name), Slug: "grb-" + name, Name: name, ProjectID: projectID, OwnerID: adminID}
		require.NoError(t, s.CreateGroup(ctx, g))
		return g
	}
	memberRole, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	bind := func(g *store.Group) {
		_, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: memberRole.ID, PrincipalType: store.RoleBindingPrincipalGroup, PrincipalID: g.ID,
			ScopeType: store.RoleScopeProject, ScopeID: projectID, CreatedBy: "test",
		})
		require.NoError(t, err)
	}

	plain := newGroup("plain")
	bound := newGroup("bound")
	bind(bound)
	parent := newGroup("parent")
	bind(parent)
	nested := newGroup("nested")
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{GroupID: parent.ID, MemberType: store.GroupMemberTypeGroup, MemberID: nested.ID, Role: store.GroupMemberRoleMember}))

	existing := newUser("existing")
	for _, g := range []*store.Group{plain, bound, nested} {
		require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{GroupID: g.ID, MemberType: store.GroupMemberTypeUser, MemberID: existing, Role: store.GroupMemberRoleMember}))
	}
	added := newUser("added")

	key, _, err := srv.uatService.CreateTokenWithParams(rs4MintContext(adminID), CreateTokenParams{
		UserID: adminID, Name: "grb", Boundary: hubBoundary(),
		Scopes: []string{"group:update", "group:delete", "group:addMember", "group:removeMember"},
	})
	require.NoError(t, err)

	type change struct {
		name   string
		method string
		path   func(g *store.Group) string
		body   interface{}
	}
	changes := []change{
		{"add member", http.MethodPost, func(g *store.Group) string { return "/api/v1/groups/" + g.ID + "/members" },
			map[string]interface{}{"memberType": "user", "memberId": added, "role": "member"}},
		{"remove member", http.MethodDelete, func(g *store.Group) string { return "/api/v1/groups/" + g.ID + "/members/user/" + existing }, nil},
		{"update", http.MethodPatch, func(g *store.Group) string { return "/api/v1/groups/" + g.ID },
			map[string]interface{}{"description": "changed"}},
		{"delete", http.MethodDelete, func(g *store.Group) string { return "/api/v1/groups/" + g.ID }, nil},
	}

	for _, g := range []*store.Group{bound, nested} {
		for _, c := range changes {
			rec := doRequestWithUAT(t, srv, key, c.method, c.path(g), c.body)
			requireSessionOnlyRefusal(t, rec, authzop.ReasonGovernancePending, g.Name+" "+c.name)
		}
		_, err := s.GetGroup(ctx, g.ID)
		assert.NoError(t, err, "%s: the refused delete leaves the group in place", g.Name)
		_, err = s.GetGroupMembership(ctx, g.ID, store.GroupMemberTypeUser, existing)
		assert.NoError(t, err, "%s: the refused removal leaves the member in place", g.Name)
		_, err = s.GetGroupMembership(ctx, g.ID, store.GroupMemberTypeUser, added)
		assert.Error(t, err, "%s: the refused add adds no member", g.Name)
	}

	for _, c := range changes {
		rec := doRequestWithUAT(t, srv, key, c.method, c.path(plain), c.body)
		assert.Less(t, rec.Code, 300, "plain %s: a group with no role binding in its closure accepts the token: %d %s", c.name, rec.Code, rec.Body.String())
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/groups/"+bound.ID+"/members",
		map[string]interface{}{"memberType": "user", "memberId": added, "role": "member"})
	assert.Less(t, rec.Code, 300, "a session adds a member to a group that carries a role binding: %d %s", rec.Code, rec.Body.String())
}

// groupRuleFixture is a project, a super-admin who owns every group it
// creates, and a hub token of that super-admin carrying the group selectors.
type groupRuleFixture struct {
	srv       *Server
	s         store.Store
	projectID string
	adminID   string
	key       string
}

func newGroupRuleFixture(t *testing.T, prefix string) *groupRuleFixture {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()
	f := &groupRuleFixture{srv: srv, s: s, projectID: tid(prefix + "-project"), adminID: tid(prefix + "-admin")}
	createRS1Project(t, s, f.projectID, tid(prefix+"-owner"))
	createTestUserWithRole(t, s, f.adminID, f.adminID+"@test.com", "admin", store.SystemRoleSuperAdmin)
	ensureHubMembership(ctx, s, f.adminID)
	key, _, err := srv.uatService.CreateTokenWithParams(rs4MintContext(f.adminID), CreateTokenParams{
		UserID: f.adminID, Name: prefix, Boundary: hubBoundary(),
		Scopes: []string{"group:update", "group:delete", "group:addMember", "group:removeMember"},
	})
	require.NoError(t, err)
	f.key = key
	return f
}

func (f *groupRuleFixture) group(t *testing.T, name, groupType string) *store.Group {
	t.Helper()
	g := &store.Group{ID: tid(name), Slug: name, Name: name, ProjectID: f.projectID, OwnerID: f.adminID, GroupType: groupType}
	require.NoError(t, f.s.CreateGroup(context.Background(), g))
	return g
}

func (f *groupRuleFixture) bind(t *testing.T, g *store.Group) {
	t.Helper()
	ctx := context.Background()
	memberRole, err := f.s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = f.s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRole.ID, PrincipalType: store.RoleBindingPrincipalGroup, PrincipalID: g.ID,
		ScopeType: store.RoleScopeProject, ScopeID: f.projectID, CreatedBy: "test",
	})
	require.NoError(t, err)
}

func (f *groupRuleFixture) user(t *testing.T, name string) string {
	t.Helper()
	id := tid(name)
	require.NoError(t, f.s.CreateUser(context.Background(), &store.User{ID: id, Email: id + "@test.com", DisplayName: name, Role: "member", Status: "active"}))
	return id
}

func (f *groupRuleFixture) nest(t *testing.T, parent, child *store.Group) {
	t.Helper()
	require.NoError(t, f.s.AddGroupMember(context.Background(), &store.GroupMember{
		GroupID: parent.ID, MemberType: store.GroupMemberTypeGroup, MemberID: child.ID, Role: store.GroupMemberRoleMember,
	}))
}

// groupClosureFailingStore fails the group rule's closure lookups for one
// group: GetParentGroups for that group, or ListRoleBindingsForPrincipals
// for a principal set that names it. Every other call reaches the store.
type groupClosureFailingStore struct {
	store.Store
	groupID         string
	failParents     error
	failRoleBinding error
}

func (s *groupClosureFailingStore) GetParentGroups(ctx context.Context, groupID string) ([]string, error) {
	if s.failParents != nil && groupID == s.groupID {
		return nil, s.failParents
	}
	return s.Store.GetParentGroups(ctx, groupID)
}

func (s *groupClosureFailingStore) ListRoleBindingsForPrincipals(ctx context.Context, principals []store.PrincipalRef, scopeTypes []string, scopeIDs []string) ([]*store.RoleBinding, error) {
	if s.failRoleBinding != nil {
		for _, p := range principals {
			if p.Type == store.RoleBindingPrincipalGroup && p.ID == s.groupID {
				return nil, s.failRoleBinding
			}
		}
	}
	return s.Store.ListRoleBindingsForPrincipals(ctx, principals, scopeTypes, scopeIDs)
}

// TestGroupChange_ClosureLookupErrorRefusesToken pins that the group rule
// refuses a user access token with 500 when the group's closure cannot be
// resolved, and the change does not happen. The failing store serves the
// handler only; authorization reads the real store, so the token passes
// every check that runs before the group rule.
func TestGroupChange_ClosureLookupErrorRefusesToken(t *testing.T) {
	cases := []struct {
		name  string
		fails func(st *groupClosureFailingStore)
	}{
		{"ancestor lookup", func(st *groupClosureFailingStore) { st.failParents = errors.New("injected parent group failure") }},
		{"role binding lookup", func(st *groupClosureFailingStore) { st.failRoleBinding = errors.New("injected role binding failure") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newGroupRuleFixture(t, "gcf")
			g := f.group(t, "gcf-group", "")
			added := f.user(t, "gcf-added")

			orig := f.srv.store
			failing := &groupClosureFailingStore{Store: orig, groupID: g.ID}
			tc.fails(failing)
			f.srv.store = failing
			rec := doRequestWithUAT(t, f.srv, f.key, http.MethodPost, "/api/v1/groups/"+g.ID+"/members",
				map[string]interface{}{"memberType": "user", "memberId": added, "role": "member"})
			f.srv.store = orig

			assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
			_, err := f.s.GetGroupMembership(context.Background(), g.ID, store.GroupMemberTypeUser, added)
			assert.ErrorIs(t, err, store.ErrNotFound, "a refused add adds no member")
		})
	}
}

// TestGroupChange_TokenRefusedAtAnyAncestorDepth pins that the group rule
// counts a role binding on an ancestor at any nesting depth: a token adding
// a member to the innermost group of a 40-level chain whose outermost group
// carries a role binding is refused with the session-only reason.
func TestGroupChange_TokenRefusedAtAnyAncestorDepth(t *testing.T) {
	f := newGroupRuleFixture(t, "gdeep")
	const depth = 40
	top := f.group(t, "gdeep-0", "")
	f.bind(t, top)
	inner := top
	for i := 1; i < depth; i++ {
		next := f.group(t, fmt.Sprintf("gdeep-%d", i), "")
		f.nest(t, inner, next)
		inner = next
	}
	parents, err := f.s.GetParentGroups(context.Background(), inner.ID)
	require.NoError(t, err)
	assert.Len(t, parents, depth-1, "every ancestor of the innermost group is resolved")

	added := f.user(t, "gdeep-added")
	rec := doRequestWithUAT(t, f.srv, f.key, http.MethodPost, "/api/v1/groups/"+inner.ID+"/members",
		map[string]interface{}{"memberType": "user", "memberId": added, "role": "member"})
	requireSessionOnlyRefusal(t, rec, authzop.ReasonGovernancePending, "innermost group add member")
	_, err = f.s.GetGroupMembership(context.Background(), inner.ID, store.GroupMemberTypeUser, added)
	assert.ErrorIs(t, err, store.ErrNotFound, "a refused add adds no member")
}
