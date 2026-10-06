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
		g := &store.Group{ID: tid("grb-" + name), Slug: "grb-" + name, Name: name, ProjectID: projectID}
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
