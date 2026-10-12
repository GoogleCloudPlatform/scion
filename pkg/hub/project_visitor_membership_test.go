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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A project visitor is never counted as a member (ptone/scion#4603). Each
// test gives a federated account only a project-visitor binding on the
// project and checks one family of membership rules, with an ordinary
// member as the positive control.

type visitorMembershipFixture struct {
	srv       *Server
	st        store.Store
	projectID string
	owner     *store.User
	member    *store.User
	visitor   *store.User
}

func newVisitorMembershipFixture(t *testing.T) *visitorMembershipFixture {
	t.Helper()
	srv, st := testServer(t)
	ctx := context.Background()

	ownerID := tid(t.Name() + "-owner")
	projectID := tid(t.Name() + "-project")
	createRS1Project(t, st, projectID, ownerID)
	owner, err := st.GetUser(ctx, ownerID)
	require.NoError(t, err)

	memberID := tid(t.Name() + "-member")
	createRS1UserWithRole(t, st, memberID, memberID+"@test.com", projectID, store.ProjectRoleMember)
	member, err := st.GetUser(ctx, memberID)
	require.NoError(t, err)

	return &visitorMembershipFixture{
		srv: srv, st: st, projectID: projectID, owner: owner, member: member,
		visitor: createProjectVisitor(t, st, "visitor@corp.example", projectID),
	}
}

// createProjectVisitor creates an active federated account for email and
// grants it the project-visitor role on projectID through the visitor
// path.
func createProjectVisitor(t *testing.T, st store.Store, email, projectID string) *store.User {
	t.Helper()
	ctx := context.Background()
	issuer := "https://hub-a.example.com"
	u := &store.User{
		DisplayName: "Visitor", Kind: store.UserKindFederated, Status: store.UserStatusActive,
		HomeIssuer: &issuer, FederatedEmail: &email,
	}
	require.NoError(t, st.CreateFederatedUser(ctx, u))
	_, err := st.CreateVisitorRoleBinding(ctx, &store.RoleBinding{
		PrincipalType: store.RoleBindingPrincipalUser,
		PrincipalID:   u.ID,
		ScopeType:     store.RoleScopeProject,
		ScopeID:       projectID,
		CreatedBy:     "test",
	})
	require.NoError(t, err)
	return u
}

func userPrincipal(id string) PrincipalContext {
	return PrincipalContext{Kind: PrincipalKindUser, ID: id}
}

func TestProjectVisitor_NotMember_ProjectMembershipEvidence(t *testing.T) {
	f := newVisitorMembershipFixture(t)
	ctx := context.Background()

	ok, _, err := f.srv.authzService.ProjectMembershipEvidence(ctx, userPrincipal(f.visitor.ID), f.projectID)
	require.NoError(t, err)
	assert.False(t, ok, "a project-visitor binding is not membership evidence")

	ok, src, err := f.srv.authzService.ProjectMembershipEvidence(ctx, userPrincipal(f.member.ID), f.projectID)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, ProjectAccessSourceMembership, src)
}

func TestProjectVisitor_NotMember_CheckEffectiveMembership(t *testing.T) {
	f := newVisitorMembershipFixture(t)
	ctx := context.Background()

	res := f.srv.CheckEffectiveMembership(ctx, f.visitor.ID, f.projectID)
	require.NoError(t, res.Err)
	assert.False(t, res.IsMember)
	assert.Empty(t, res.Role)

	res = f.srv.CheckEffectiveMembership(ctx, f.member.ID, f.projectID)
	require.NoError(t, res.Err)
	assert.True(t, res.IsMember)
	assert.Equal(t, store.ProjectRoleMember, res.Role)
}

func TestProjectVisitor_NotMember_DirectRoleChecks(t *testing.T) {
	f := newVisitorMembershipFixture(t)
	ctx := context.Background()

	roles, err := activeDirectProjectRoles(ctx, f.st, f.srv.authzService, true, f.visitor.ID, f.projectID)
	require.NoError(t, err)
	assert.Empty(t, roles, "a project-visitor binding yields no membership role")
	assert.False(t, f.srv.isActiveMember(ctx, f.visitor.ID, f.projectID))
	assert.False(t, f.srv.isProjectOwner(ctx, f.visitor.ID, f.projectID))
	assert.False(t, f.srv.authzService.isProjectOwnerOrAdmin(ctx, f.visitor.ID, f.projectID))

	assert.True(t, f.srv.isActiveMember(ctx, f.member.ID, f.projectID))
	assert.True(t, f.srv.isProjectOwner(ctx, f.owner.ID, f.projectID))
}

func TestProjectVisitor_NotMember_ChatMembership(t *testing.T) {
	f := newVisitorMembershipFixture(t)
	ctx := context.Background()

	got, err := f.srv.chatMemberProjectIDs(ctx, f.visitor.ID)
	require.NoError(t, err)
	assert.False(t, got[f.projectID])

	got, err = f.srv.chatMemberProjectIDs(ctx, f.member.ID)
	require.NoError(t, err)
	assert.True(t, got[f.projectID])
}

func TestProjectVisitor_NotMember_MemberLists(t *testing.T) {
	f := newVisitorMembershipFixture(t)
	ctx := context.Background()

	members, err := f.st.ListProjectMembers(ctx, f.projectID)
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, m := range members {
		ids[m.UserID] = true
	}
	assert.False(t, ids[f.visitor.ID], "visitor is not listed")
	assert.True(t, ids[f.member.ID])
	assert.True(t, ids[f.owner.ID])

	ok, err := f.st.IsProjectMember(ctx, f.projectID, f.visitor.ID)
	require.NoError(t, err)
	assert.False(t, ok)
	_, err = f.st.GetProjectMembership(ctx, f.projectID, f.visitor.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)

	// The Members API lists neither the visitor's binding nor counts it.
	for _, path := range []string{
		"/api/v1/projects/" + f.projectID + "/members",
		"/api/v1/projects/" + f.projectID + "/members?groupBy=principal",
	} {
		rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, path, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.NotContains(t, rec.Body.String(), f.visitor.ID, path)
		assert.Contains(t, rec.Body.String(), f.member.ID, path)
	}
	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, "/api/v1/projects/"+f.projectID+"/members", nil)
	var body listProjectMembersResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, len(body.Items), body.TotalCount)

	// The members editor does not offer the project-visitor role.
	rec = doRequestAsUser(t, f.srv, f.owner, http.MethodGet, "/api/v1/projects/"+f.projectID+"/members/assignable-roles", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var roles listAssignableRolesResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &roles))
	for _, it := range roles.Items {
		assert.NotEqual(t, store.ProjectRoleVisitor, it.Name)
	}
}

func TestProjectVisitor_NotMember_MembersInboundPolicy(t *testing.T) {
	f := crossProjectSetup(t)
	ctx := context.Background()
	enableCrossProjectMessaging(t, f.srv)
	_, err := f.store.UpdateProjectMessagingPolicy(ctx, f.projectB, store.CrossProjectInboundMembers, 1)
	require.NoError(t, err)

	// The origin user holds only a project-visitor binding on project B.
	visitor := createProjectVisitor(t, f.store, "origin@corp.example", f.projectB)
	sender := msgAuthzAgent(t, f.store, "cp-visitor-sender", f.projectA, store.MessageModeHub, []string{visitor.ID})
	target := msgAuthzAgent(t, f.store, "cp-visitor-target", f.projectB, store.MessageModeProject, []string{f.ownerB.ID})

	decision := f.srv.EvaluateAgentMessage(ctx, msgAuthzAgentIdentity(sender.ID, f.projectA, sender.Ancestry), target)
	assert.False(t, decision.Allowed)
	assert.Equal(t, MessageDenialCrossProjectNotMember, decision.Code, decision.Reason)
}

func TestProjectVisitor_NotMember_MembershipLossReconciler(t *testing.T) {
	f := newVisitorMembershipFixture(t)
	ctx := context.Background()

	admitted, err := f.srv.userAdmittedByID(ctx, f.visitor.ID, f.projectID)
	require.NoError(t, err)
	assert.False(t, admitted, "a visitor-only principal is not admitted, so its agents would be held")

	admitted, err = f.srv.userAdmittedByID(ctx, f.member.ID, f.projectID)
	require.NoError(t, err)
	assert.True(t, admitted)
}

func TestProjectVisitor_NotMember_RelevantProjectAdmission(t *testing.T) {
	f := newVisitorMembershipFixture(t)
	ctx := context.Background()

	ok, err := f.srv.authzService.hasRelevantProjectAdmission(ctx, userPrincipal(f.visitor.ID), "agent.list")
	require.NoError(t, err)
	assert.False(t, ok)

	ok, err = f.srv.authzService.hasRelevantProjectAdmission(ctx, userPrincipal(f.member.ID), "agent.list")
	require.NoError(t, err)
	assert.True(t, ok)
}

// The general role binding path and the members editor refuse the
// project-visitor role.
func TestProjectVisitor_GeneralBindingPathRefusesVisitorRole(t *testing.T) {
	f := newVisitorMembershipFixture(t)
	ctx := context.Background()
	rd, err := f.st.GetRoleDefinitionByName(ctx, store.ProjectRoleVisitor, store.RoleScopeProject)
	require.NoError(t, err)

	_, err = f.st.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      f.member.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          f.projectID,
		CreatedBy:        "test",
	})
	assert.ErrorIs(t, err, store.ErrNonMembershipRoleRefused)

	// The members editor PUT refuses it as an invalid role set.
	rec := putMemberRoles(t, f.srv, f.owner, f.projectID, "user", f.member.ID, []string{rd.ID}, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ErrCodeInvalidRoleSet)
	m, err := f.st.GetProjectMembership(ctx, f.projectID, f.member.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ProjectRoleMember, m.Role, "the member's roles are unchanged")
}

// The seeded project-visitor role holds exactly agent.message, agent.list
// and agent.read and does not confer membership; project.manage_visitors
// is on the owner and admin roles only.
func TestBuiltInRoles_ProjectVisitorAndManageVisitors(t *testing.T) {
	var visitor *BuiltInRole
	for _, role := range BuiltInRoles() {
		if role.Name == store.ProjectRoleVisitor {
			r := role
			visitor = &r
		}
		holds := false
		for _, p := range role.Permissions {
			if p == "project.manage_visitors" {
				holds = true
			}
		}
		switch role.Name {
		case store.ProjectRoleOwner, store.ProjectRoleAdmin:
			assert.True(t, holds, "%s holds project.manage_visitors", role.Name)
		case store.SystemRoleSuperAdmin:
			// super-admin holds every non-reserved permission.
		default:
			assert.False(t, holds, "%s must not hold project.manage_visitors", role.Name)
		}
		if role.Name != store.ProjectRoleVisitor {
			assert.False(t, role.NoMembership, "%s confers membership", role.Name)
		}
	}
	require.NotNil(t, visitor, "project-visitor is seeded")
	assert.Equal(t, store.RoleScopeProject, visitor.ScopeType)
	assert.True(t, visitor.NoMembership)
	assert.ElementsMatch(t, []string{"agent.message", "agent.list", "agent.read"}, visitor.Permissions)

	_, st := testServer(t)
	ctx := context.Background()
	rd, err := st.GetRoleDefinitionByName(ctx, store.ProjectRoleVisitor, store.RoleScopeProject)
	require.NoError(t, err)
	assert.True(t, rd.System)
	assert.False(t, rd.ConfersProjectMembership())
	assert.ElementsMatch(t, visitor.Permissions, rd.Permissions)
	for _, name := range []string{store.ProjectRoleOwner, store.ProjectRoleAdmin, store.ProjectRoleMember} {
		rd, err := st.GetRoleDefinitionByName(ctx, name, store.RoleScopeProject)
		require.NoError(t, err)
		assert.True(t, rd.ConfersProjectMembership(), name)
	}
}

// A custom role an operator created under the project-visitor name before
// the built-in role existed is left unchanged by reconciliation: it keeps
// conferring membership and its permissions.
func TestReconcileBuiltInRoles_LeavesCustomRoleWithVisitorName(t *testing.T) {
	_, st := testServer(t)
	ctx := context.Background()
	// The seeded project-visitor row exists, so the custom role takes a
	// different name and a built-in role of that name is reconciled.
	custom := &store.RoleDefinition{
		Name: store.ProjectRoleVisitor + "-x", ScopeType: store.RoleScopeProject,
		Permissions: []string{"project.read", "agent.list"},
	}
	_, err := st.CreateRoleDefinition(ctx, custom)
	require.NoError(t, err)

	role := BuiltInRole{
		Name: custom.Name, ScopeType: store.RoleScopeProject, Revision: 1,
		Permissions: projectVisitorPermissionIDs(), NoMembership: true,
	}
	reconcileBuiltInRole(ctx, st, role)

	got, err := st.GetRoleDefinitionByName(ctx, custom.Name, store.RoleScopeProject)
	require.NoError(t, err)
	assert.True(t, got.ConfersProjectMembership(), "a custom role keeps conferring membership")
	assert.ElementsMatch(t, []string{"project.read", "agent.list"}, got.Permissions, "its permissions are unchanged")
}
