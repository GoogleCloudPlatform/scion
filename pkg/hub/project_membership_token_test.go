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
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Project membership changes by user access token. A token is admitted on
// project:manage, on a boundary that covers the project, and then acts only
// through its holder's own project role: the governance matrix, the
// direct-owner rule, CanDelegate against the token's selectors, last-owner
// protection and the one-binding invariant apply, and the hub role-binding
// authority of a holder with no project role is never used.

// mtNarrowPerms are the permissions a narrowed role carries in these tests;
// every one maps to a selector, so a token can carry all of them.
var mtNarrowPerms = []string{"project.read", "agent.read"}

// mtNarrowSelectors are the selectors that cover mtNarrowPerms, plus
// project:manage.
var mtNarrowSelectors = []string{"project:manage", "project:read", "agent:read"}

// mtMint mints a real user access token for userID through the token
// service, over a session.
func mtMint(t *testing.T, srv *Server, userID string, boundary TokenBoundary, scopes ...string) string {
	t.Helper()
	key, _, err := srv.uatService.CreateTokenWithParams(rs4MintContext(userID), CreateTokenParams{
		UserID: userID, Name: "mt-token", Boundary: boundary, Scopes: scopes,
	})
	require.NoError(t, err, "mint %v on %s", scopes, boundary.Kind)
	return key
}

// mtErrCode returns error.code of an error response, or "".
func mtErrCode(rec *httptest.ResponseRecorder) string {
	var resp ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		return ""
	}
	return resp.Error.Code
}

// mtNarrowRole replaces the permissions of the role definition rd in the
// store with perms.
func mtNarrowRole(t *testing.T, s store.Store, rd *store.RoleDefinition, perms ...string) {
	t.Helper()
	require.NoError(t, s.UpdateSystemRoleDefinitionPermissions(context.Background(), rd.ID, perms))
}

// mtMemberUser creates a user holding role rd on projectID and returns the
// user and the binding ID.
func mtMemberUser(t *testing.T, f *mmrFixture, name string, rd *store.RoleDefinition) (*store.User, string) {
	t.Helper()
	u := grpUser(t, f.store, t.Name()+"-"+name, name)
	grpBind(t, f.store, "user", u.ID, rd.ID, f.projectID)
	return u, pinBindingID(t, f.store, u.ID, f.projectID)
}

// mtBindingExists reports whether the role binding id is still stored.
func mtBindingExists(t *testing.T, s store.Store, id string) bool {
	t.Helper()
	_, err := s.GetRoleBinding(context.Background(), id)
	return err == nil
}

// mtSuperAdmin creates a super-admin who holds no role in any project.
func mtSuperAdmin(t *testing.T, s store.Store, name string) *store.User {
	t.Helper()
	id := tid(t.Name() + "-" + name)
	createTestUserWithRole(t, s, id, id+"@test.com", "admin", store.SystemRoleSuperAdmin)
	ensureHubMembership(context.Background(), s, id)
	u, err := s.GetUser(context.Background(), id)
	require.NoError(t, err)
	return u
}

func mtRoleBindingPath(id string) string { return "/api/v1/admin/role-bindings/" + id }

// TestMembershipToken_GovernanceMatrixApplies pins that a token acts on
// membership through its holder's project role: an admin's token manages
// members but not admins, an owner's token manages admins, a token whose
// holder holds only the member role is refused, and a token whose holder has
// hub role-binding authority but no project role is refused while the
// holder's session is admitted.
func TestMembershipToken_GovernanceMatrixApplies(t *testing.T) {
	f := setupMMRFixture(t)
	project := projectBoundary(f.projectID)
	adminTok := mtMint(t, f.srv, f.admin.ID, project, "project:manage")
	ownerTok := mtMint(t, f.srv, f.owner.ID, project, "project:manage")

	// An admin's token removes a member, through the members route and
	// through the role-binding route.
	_, memberBinding := mtMemberUser(t, f, "m1", f.memberRD)
	rec := doRequestWithUAT(t, f.srv, adminTok, http.MethodDelete, pinMemberPath(f.projectID, memberBinding), nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.False(t, mtBindingExists(t, f.store, memberBinding))
	_, memberBinding2 := mtMemberUser(t, f, "m2", f.memberRD)
	rec = doRequestWithUAT(t, f.srv, adminTok, http.MethodDelete, mtRoleBindingPath(memberBinding2), nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.False(t, mtBindingExists(t, f.store, memberBinding2))

	// An admin's token cannot remove or change another admin, or remove
	// the owner.
	_, admin2Binding := mtMemberUser(t, f, "a2", f.adminRD)
	rec = doRequestWithUAT(t, f.srv, adminTok, http.MethodDelete, pinMemberPath(f.projectID, admin2Binding), nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, ErrCodeTargetRoleProtected, mtErrCode(rec), rec.Body.String())
	rec = doRequestWithUAT(t, f.srv, adminTok, http.MethodPatch, pinMemberPath(f.projectID, admin2Binding), map[string]string{"roleDefinitionId": f.memberRD.ID})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	ownerBinding := pinBindingID(t, f.store, f.owner.ID, f.projectID)
	rec = doRequestWithUAT(t, f.srv, adminTok, http.MethodDelete, mtRoleBindingPath(ownerBinding), nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, ErrCodeTargetRoleProtected, mtErrCode(rec), rec.Body.String())
	assert.True(t, mtBindingExists(t, f.store, admin2Binding))
	assert.True(t, mtBindingExists(t, f.store, ownerBinding))

	// An admin's token cannot raise a member to admin, even when its
	// selectors carry every permission of the admin role: the governance
	// matrix refuses the admin target.
	mtNarrowRole(t, f.store, f.adminRD, "project.manage", "project.read", "agent.read")
	adminCoveringTok := mtMint(t, f.srv, f.admin.ID, project, mtNarrowSelectors...)
	member3, memberBinding3 := mtMemberUser(t, f, "m3", f.memberRD)
	rec = doRequestWithUAT(t, f.srv, adminCoveringTok, http.MethodPatch, pinMemberPath(f.projectID, memberBinding3), map[string]string{"roleDefinitionId": f.adminRD.ID})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, ErrCodeTargetRoleProtected, mtErrCode(rec), rec.Body.String())
	assert.True(t, mtBindingExists(t, f.store, memberBinding3))
	assert.Equal(t, store.ProjectRoleMember, mmrRoleNameOf(t, f, member3.ID))

	// The direct owner's token demotes and removes an admin (a lowering
	// change grants nothing, so CanDelegate does not apply).
	rec = doRequestWithUAT(t, f.srv, ownerTok, http.MethodPatch, pinMemberPath(f.projectID, admin2Binding), map[string]string{"roleDefinitionId": f.memberRD.ID})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	_, admin3Binding := mtMemberUser(t, f, "a3", f.adminRD)
	rec = doRequestWithUAT(t, f.srv, ownerTok, http.MethodDelete, pinMemberPath(f.projectID, admin3Binding), nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	// The admin's token stops working once its holder is only a member.
	pinSetDirectRole(t, f.store, f.admin.ID, f.projectID, f.memberRD)
	rec = doRequestWithUAT(t, f.srv, adminTok, http.MethodDelete, pinMemberPath(f.projectID, memberBinding3), nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.True(t, mtBindingExists(t, f.store, memberBinding3))

	// A super-admin with no project role: the hub token carrying
	// project:manage passes project.manage on the project, and is refused
	// by governance because a token never uses hub role-binding
	// authority. The same holder's session is admitted through it.
	super := mtSuperAdmin(t, f.store, "super")
	superTok := mtMint(t, f.srv, super.ID, hubBoundary(), "project:manage")
	for _, path := range []string{pinMemberPath(f.projectID, memberBinding3), mtRoleBindingPath(memberBinding3)} {
		rec = doRequestWithUAT(t, f.srv, superTok, http.MethodDelete, path, nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", path, rec.Body.String())
		assert.Equal(t, ErrCodeRoleAssignmentForbidden, mtErrCode(rec), "%s: %s", path, rec.Body.String())
	}
	rec = doRequestWithUAT(t, f.srv, superTok, http.MethodPut, mmrPrincipalPath(f.projectID, "user", f.member.ID), map[string]interface{}{"roleDefinitionIds": []string{f.memberRD.ID, f.withinCeiling.ID}})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, ErrCodeRoleAssignmentForbidden, mtErrCode(rec), rec.Body.String())
	assert.True(t, mtBindingExists(t, f.store, memberBinding3))
	rec = doRequestAsUser(t, f.srv, super, http.MethodDelete, pinMemberPath(f.projectID, memberBinding3), nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	// The service reports no hub role-binding authority for a token
	// request, and keeps it for the same holder's session.
	ctx := realTokenContext(t, f.srv, superTok)
	assert.False(t, f.srv.membershipService.actorHasHubRoleBindingAuthority(ctx, super.ID, MembershipOpRemove))
	has, err := f.srv.membershipService.actorHasHubRoleBindingAuthorityTx(ctx, f.store, super.ID, MembershipOpRemove)
	require.NoError(t, err)
	assert.False(t, has)
	session := rs4MintContext(super.ID)
	assert.True(t, f.srv.membershipService.actorHasHubRoleBindingAuthority(session, super.ID, MembershipOpRemove))
}

// TestMembershipToken_CannotGrantRoleAboveCaller pins CanDelegate for a
// token: a role is granted only when the token's selectors carry every
// permission of the role. A token that lacks the selector of one
// permission of project-member cannot grant it; with every selector it
// can.
func TestMembershipToken_CannotGrantRoleAboveCaller(t *testing.T) {
	f := setupMMRFixture(t)
	project := projectBoundary(f.projectID)

	// project:manage alone covers none of the member role's permissions.
	manageOnly := mtMint(t, f.srv, f.owner.ID, project, "project:manage")
	target := grpUser(t, f.store, t.Name()+"-t1", "T1")
	rec := doRequestWithUAT(t, f.srv, manageOnly, http.MethodPost, legacyMembersPath(f.projectID),
		addProjectMemberRequest{RoleDefinitionID: f.memberRD.ID, PrincipalType: "user", PrincipalID: target.ID})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, ErrCodeTargetRoleProtected, mtErrCode(rec), rec.Body.String())
	assert.Empty(t, mmrBindingsFor(t, f.store, "user", target.ID, f.projectID))

	// Every permission of project-member maps to a selector that is allowed
	// on the project boundary. A token missing one of those selectors cannot
	// grant the role on any route, and nothing is written.
	var selectors, unmapped []string
	for _, p := range f.memberRD.Permissions {
		sel := bearerMatrixSelector(p)
		if sel == "" {
			unmapped = append(unmapped, p)
			continue
		}
		if projectBoundaryEligible(p) {
			selectors = append(selectors, sel)
		}
	}
	require.Empty(t, unmapped, "every project-member permission maps to a selector")
	sort.Strings(selectors)
	const dropped = "scheduled_event:create"
	var missingOne []string
	for _, sel := range selectors {
		if sel != dropped {
			missingOne = append(missingOne, sel)
		}
	}
	require.Len(t, missingOne, len(selectors)-1, "project-member carries %s", dropped)
	lacksOne := mtMint(t, f.srv, f.owner.ID, project, append(missingOne, "project:manage")...)
	grantRoutes := func(principalID string) []struct {
		method, path string
		body         interface{}
		created      bool
	} {
		return []struct {
			method, path string
			body         interface{}
			created      bool
		}{
			{http.MethodPost, legacyMembersPath(f.projectID), addProjectMemberRequest{RoleDefinitionID: f.memberRD.ID, PrincipalType: "user", PrincipalID: principalID}, true},
			{http.MethodPut, mmrPrincipalPath(f.projectID, "user", principalID), map[string]interface{}{"roleDefinitionIds": []string{f.memberRD.ID}}, false},
			{http.MethodPost, "/api/v1/admin/role-bindings", createRoleBindingRequest{RoleDefinitionID: f.memberRD.ID, PrincipalType: "user", PrincipalID: principalID, ScopeType: store.RoleScopeProject, ScopeID: f.projectID}, true},
		}
	}
	for _, c := range grantRoutes(target.ID) {
		rec = doRequestWithUAT(t, f.srv, lacksOne, c.method, c.path, c.body)
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s %s: %s", c.method, c.path, rec.Body.String())
		assert.Equal(t, ErrCodeTargetRoleProtected, mtErrCode(rec), "%s %s: %s", c.method, c.path, rec.Body.String())
	}
	assert.Empty(t, mmrBindingsFor(t, f.store, "user", target.ID, f.projectID))

	// The owner's token with every member selector and project:manage
	// grants project-member on each route; each grant goes to a new user.
	allSelectors := mtMint(t, f.srv, f.owner.ID, project, append(selectors, "project:manage")...)
	for i := range grantRoutes("") {
		grantee := grpUser(t, f.store, fmt.Sprintf("%s-full%d", t.Name(), i), fmt.Sprintf("Full%d", i))
		c := grantRoutes(grantee.ID)[i]
		rec = doRequestWithUAT(t, f.srv, allSelectors, c.method, c.path, c.body)
		if c.created {
			require.Equal(t, http.StatusCreated, rec.Code, "%s %s: %s", c.method, c.path, rec.Body.String())
		} else {
			require.Less(t, rec.Code, 300, "%s %s: %s", c.method, c.path, rec.Body.String())
		}
		assert.Len(t, mmrBindingsFor(t, f.store, "user", grantee.ID, f.projectID), 1, "%s %s", c.method, c.path)
	}

	// With the member role narrowed to permissions the token covers, the
	// grant goes through on every route, and project:manage alone is
	// still refused.
	mtNarrowRole(t, f.store, f.memberRD, mtNarrowPerms...)
	narrow := mtMint(t, f.srv, f.owner.ID, project, mtNarrowSelectors...)
	rec = doRequestWithUAT(t, f.srv, manageOnly, http.MethodPost, legacyMembersPath(f.projectID),
		addProjectMemberRequest{RoleDefinitionID: f.memberRD.ID, PrincipalType: "user", PrincipalID: target.ID})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	rec = doRequestWithUAT(t, f.srv, narrow, http.MethodPost, legacyMembersPath(f.projectID),
		addProjectMemberRequest{RoleDefinitionID: f.memberRD.ID, PrincipalType: "user", PrincipalID: target.ID})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Len(t, mmrBindingsFor(t, f.store, "user", target.ID, f.projectID), 1)

	viaPut := grpUser(t, f.store, t.Name()+"-t2", "T2")
	rec = doRequestWithUAT(t, f.srv, narrow, http.MethodPut, mmrPrincipalPath(f.projectID, "user", viaPut.ID), map[string]interface{}{"roleDefinitionIds": []string{f.memberRD.ID}})
	require.Less(t, rec.Code, 300, rec.Body.String())
	assert.Len(t, mmrBindingsFor(t, f.store, "user", viaPut.ID, f.projectID), 1)

	viaRB := grpUser(t, f.store, t.Name()+"-t3", "T3")
	rec = doRequestWithUAT(t, f.srv, narrow, http.MethodPost, "/api/v1/admin/role-bindings", createRoleBindingRequest{
		RoleDefinitionID: f.memberRD.ID, PrincipalType: "user", PrincipalID: viaRB.ID, ScopeType: store.RoleScopeProject, ScopeID: f.projectID,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Len(t, mmrBindingsFor(t, f.store, "user", viaRB.ID, f.projectID), 1)

	// A custom role the token covers is granted by the direct owner's
	// token; an admin's token with the same selectors has no custom-role
	// authority.
	custom, err := f.store.CreateRoleDefinition(context.Background(), &store.RoleDefinition{
		Name: "mt-narrow-" + tid(t.Name())[:8], ScopeType: store.RoleScopeProject, Permissions: mtNarrowPerms,
	})
	require.NoError(t, err)
	adminNarrow := mtMint(t, f.srv, f.admin.ID, project, mtNarrowSelectors...)
	body := map[string]interface{}{"roleDefinitionIds": []string{f.memberRD.ID, custom.ID}}
	rec = doRequestWithUAT(t, f.srv, adminNarrow, http.MethodPut, mmrPrincipalPath(f.projectID, "user", viaPut.ID), body)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	rec = doRequestWithUAT(t, f.srv, narrow, http.MethodPut, mmrPrincipalPath(f.projectID, "user", viaPut.ID), body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Len(t, mmrBindingsFor(t, f.store, "user", viaPut.ID, f.projectID), 2)
}

// TestMembershipToken_ProjectMemberGrantNeedsChatPostSelector pins that a
// token grants the built-in project-member role only when it carries the
// project:chat_post selector (ptone/scion#5518): the role holds
// project.chat_post, and a token keeps it for delegation only through that
// selector. Without it the grant is refused and nothing is written.
func TestMembershipToken_ProjectMemberGrantNeedsChatPostSelector(t *testing.T) {
	f := setupMMRFixture(t)
	project := projectBoundary(f.projectID)
	require.Contains(t, f.memberRD.Permissions, "project.chat_post", "project-member holds project.chat_post")

	var selectors, withoutChatPost []string
	for _, p := range f.memberRD.Permissions {
		sel := bearerMatrixSelector(p)
		require.NotEmpty(t, sel, "%s maps to a selector", p)
		if !projectBoundaryEligible(p) {
			continue
		}
		selectors = append(selectors, sel)
		if sel != "project:chat_post" {
			withoutChatPost = append(withoutChatPost, sel)
		}
	}
	require.Contains(t, selectors, "project:chat_post")
	require.Len(t, withoutChatPost, len(selectors)-1)

	lacksChatPost := mtMint(t, f.srv, f.owner.ID, project, append(withoutChatPost, "project:manage")...)
	refused := grpUser(t, f.store, t.Name()+"-refused", "Refused")
	rec := doRequestWithUAT(t, f.srv, lacksChatPost, http.MethodPost, legacyMembersPath(f.projectID),
		addProjectMemberRequest{RoleDefinitionID: f.memberRD.ID, PrincipalType: "user", PrincipalID: refused.ID})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, ErrCodeTargetRoleProtected, mtErrCode(rec), rec.Body.String())
	assert.Contains(t, rec.Body.String(), "project.chat_post", "the refusal names the uncovered permission")
	assert.Empty(t, mmrBindingsFor(t, f.store, "user", refused.ID, f.projectID), "nothing is written")

	withChatPost := mtMint(t, f.srv, f.owner.ID, project, append(selectors, "project:manage")...)
	granted := grpUser(t, f.store, t.Name()+"-granted", "Granted")
	rec = doRequestWithUAT(t, f.srv, withChatPost, http.MethodPost, legacyMembersPath(f.projectID),
		addProjectMemberRequest{RoleDefinitionID: f.memberRD.ID, PrincipalType: "user", PrincipalID: granted.ID})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Len(t, mmrBindingsFor(t, f.store, "user", granted.ID, f.projectID), 1)
}

// TestMembershipToken_LastOwnerProtected pins that the sole owner's token
// cannot remove or demote the last owner on any membership route.
func TestMembershipToken_LastOwnerProtected(t *testing.T) {
	f := setupMMRFixture(t)
	ownerTok := mtMint(t, f.srv, f.owner.ID, projectBoundary(f.projectID), "project:manage")
	ownerBinding := pinBindingID(t, f.store, f.owner.ID, f.projectID)

	for _, c := range []struct {
		method, path string
		body         interface{}
	}{
		{http.MethodDelete, pinMemberPath(f.projectID, ownerBinding), nil},
		{http.MethodPatch, pinMemberPath(f.projectID, ownerBinding), map[string]string{"roleDefinitionId": f.memberRD.ID}},
		{http.MethodDelete, mmrPrincipalPath(f.projectID, "user", f.owner.ID), nil},
		{http.MethodPut, mmrPrincipalPath(f.projectID, "user", f.owner.ID), map[string]interface{}{"roleDefinitionIds": []string{f.memberRD.ID}}},
		{http.MethodDelete, mtRoleBindingPath(ownerBinding), nil},
	} {
		rec := doRequestWithUAT(t, f.srv, ownerTok, c.method, c.path, c.body)
		assert.Equal(t, http.StatusConflict, rec.Code, "%s %s: %s", c.method, c.path, rec.Body.String())
		assert.Equal(t, ErrCodeLastOwner, mtErrCode(rec), "%s %s: %s", c.method, c.path, rec.Body.String())
		assert.True(t, mtBindingExists(t, f.store, ownerBinding), "%s %s", c.method, c.path)
	}
}

// TestTransferOwnershipToken_RequiresDirectOwner pins that a token
// transfers ownership only for a direct owner, and only when its selectors
// carry every permission of the owner role.
func TestTransferOwnershipToken_RequiresDirectOwner(t *testing.T) {
	f := setupMMRFixture(t)
	project := projectBoundary(f.projectID)
	body := map[string]string{"newOwnerId": f.member.ID}

	adminTok := mtMint(t, f.srv, f.admin.ID, project, mtNarrowSelectors...)
	rec := doRequestWithUAT(t, f.srv, adminTok, http.MethodPost, pinTransferPath(f.projectID), body)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, ErrCodeRoleAssignmentForbidden, mtErrCode(rec), rec.Body.String())

	manageOnly := mtMint(t, f.srv, f.owner.ID, project, "project:manage")
	rec = doRequestWithUAT(t, f.srv, manageOnly, http.MethodPost, pinTransferPath(f.projectID), body)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, ErrCodeTargetRoleProtected, mtErrCode(rec), rec.Body.String())
	assert.Equal(t, store.ProjectRoleOwner, mmrRoleNameOf(t, f, f.owner.ID))

	// With the owner role narrowed to permissions the token covers, the
	// direct owner's token transfers ownership.
	mtNarrowRole(t, f.store, f.ownerRD, "project.manage", "project.read", "agent.read")
	ownerTok := mtMint(t, f.srv, f.owner.ID, project, mtNarrowSelectors...)
	rec = doRequestWithUAT(t, f.srv, adminTok, http.MethodPost, pinTransferPath(f.projectID), body)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	rec = doRequestWithUAT(t, f.srv, ownerTok, http.MethodPost, pinTransferPath(f.projectID), body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, store.ProjectRoleOwner, mmrRoleNameOf(t, f, f.member.ID))
	assert.Equal(t, store.ProjectRoleMember, mmrRoleNameOf(t, f, f.owner.ID))
}

// mmrRoleNameOf returns the name of the single direct project role userID
// holds in the fixture project.
func mmrRoleNameOf(t *testing.T, f *mmrFixture, userID string) string {
	t.Helper()
	bindings := mmrBindingsFor(t, f.store, "user", userID, f.projectID)
	require.Len(t, bindings, 1)
	rd, err := f.store.GetRoleDefinition(context.Background(), bindings[0].RoleDefinitionID)
	require.NoError(t, err)
	return rd.Name
}

// TestAssignableRolesToken_ReflectsDelegationLimits pins that the grantable
// flag a token sees is the members PUT's decision for that token: a role is
// grantable only when the token's selectors carry all its permissions and
// the holder's project role may grant it.
func TestAssignableRolesToken_ReflectsDelegationLimits(t *testing.T) {
	f := setupMMRFixture(t)
	project := projectBoundary(f.projectID)
	custom, err := f.store.CreateRoleDefinition(context.Background(), &store.RoleDefinition{
		Name: "mt-assignable-" + tid(t.Name())[:8], ScopeType: store.RoleScopeProject, Permissions: mtNarrowPerms,
	})
	require.NoError(t, err)

	get := func(key string) map[string]AssignableProjectRole {
		t.Helper()
		rec := doRequestWithUAT(t, f.srv, key, http.MethodGet, assignableRolesPath(f.projectID), nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var body listAssignableRolesResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		out := make(map[string]AssignableProjectRole, len(body.Items))
		for _, it := range body.Items {
			out[it.ID] = it
		}
		return out
	}

	// The owner's session may grant both.
	session := getAssignableRoles(t, f, f.owner)
	assert.True(t, session[f.memberRD.ID].Grantable)
	assert.True(t, session[custom.ID].Grantable)

	// project:manage alone grants neither.
	items := get(mtMint(t, f.srv, f.owner.ID, project, "project:manage"))
	assert.False(t, items[f.memberRD.ID].Grantable)
	assert.Equal(t, ErrCodeTargetRoleProtected, items[f.memberRD.ID].DenialCode)
	assert.False(t, items[custom.ID].Grantable)
	assert.Equal(t, ErrCodeTargetRoleProtected, items[custom.ID].DenialCode)

	// Selectors covering the custom role make it grantable; the member
	// role carries permissions the token does not cover.
	items = get(mtMint(t, f.srv, f.owner.ID, project, mtNarrowSelectors...))
	assert.True(t, items[custom.ID].Grantable, "%+v", items[custom.ID])
	assert.False(t, items[f.memberRD.ID].Grantable)
	assert.Equal(t, ErrCodeTargetRoleProtected, items[f.memberRD.ID].DenialCode)

	// An admin's token with the same selectors has no custom-role
	// authority.
	items = get(mtMint(t, f.srv, f.admin.ID, project, mtNarrowSelectors...))
	assert.False(t, items[custom.ID].Grantable)
	assert.NotEmpty(t, items[custom.ID].DenialCode)

	// A super-admin's hub token: no project role, and a token never uses
	// hub role-binding authority, so nothing is grantable.
	super := mtSuperAdmin(t, f.store, "super")
	items = get(mtMint(t, f.srv, super.ID, hubBoundary(), "project:manage", "project:read", "agent:read"))
	require.NotEmpty(t, items)
	for _, it := range items {
		assert.False(t, it.Grantable, "%s", it.Name)
		assert.Equal(t, ErrCodeRoleAssignmentForbidden, it.DenialCode, "%s", it.Name)
	}
}

// TestMembershipToken_SelectorAndBoundaryPerRoute pins, on every membership
// route, that a token is refused without project:manage and on a project
// boundary for another project, and is admitted with project:manage on the
// project's own boundary and on the hub boundary, where the same changes
// succeed as for the holder's session.
func TestMembershipToken_SelectorAndBoundaryPerRoute(t *testing.T) {
	type step struct {
		name   string
		method string
		path   string
		body   interface{}
	}
	// steps builds one pass over every route, for fresh targets.
	steps := func(t *testing.T, f *mmrFixture) []step {
		t.Helper()
		add := grpUser(t, f.store, t.Name()+"-add", "Add")
		_, demote := mtMemberUser(t, f, "demote", f.adminRD)
		put := grpUser(t, f.store, t.Name()+"-put", "Put")
		rb := grpUser(t, f.store, t.Name()+"-rb", "RB")
		_, rbDel := mtMemberUser(t, f, "rbdel", f.memberRD)
		_, del := mtMemberUser(t, f, "del", f.memberRD)
		putDel, _ := mtMemberUser(t, f, "putdel", f.memberRD)
		return []step{
			{"members add", http.MethodPost, legacyMembersPath(f.projectID), addProjectMemberRequest{RoleDefinitionID: f.memberRD.ID, PrincipalType: "user", PrincipalID: add.ID}},
			{"members change", http.MethodPatch, pinMemberPath(f.projectID, demote), map[string]string{"roleDefinitionId": f.memberRD.ID}},
			{"members remove", http.MethodDelete, pinMemberPath(f.projectID, del), nil},
			{"principal set", http.MethodPut, mmrPrincipalPath(f.projectID, "user", put.ID), map[string]interface{}{"roleDefinitionIds": []string{f.memberRD.ID}}},
			{"principal remove", http.MethodDelete, mmrPrincipalPath(f.projectID, "user", putDel.ID), nil},
			{"role binding create", http.MethodPost, "/api/v1/admin/role-bindings", createRoleBindingRequest{RoleDefinitionID: f.memberRD.ID, PrincipalType: "user", PrincipalID: rb.ID, ScopeType: store.RoleScopeProject, ScopeID: f.projectID}},
			{"role binding delete", http.MethodDelete, mtRoleBindingPath(rbDel), nil},
			{"transfer ownership", http.MethodPost, pinTransferPath(f.projectID), map[string]string{"newOwnerId": f.member.ID}},
		}
	}
	setup := func(t *testing.T) *mmrFixture {
		f := setupMMRFixture(t)
		mtNarrowRole(t, f.store, f.memberRD, mtNarrowPerms...)
		mtNarrowRole(t, f.store, f.ownerRD, "project.manage", "project.read", "agent.read")
		// The owner also owns the other project, so a token bound to it
		// mints.
		grpBind(t, f.store, "user", f.owner.ID, f.ownerRD.ID, f.otherProjectID)
		return f
	}

	t.Run("refused", func(t *testing.T) {
		f := setup(t)
		noManage := mtMint(t, f.srv, f.owner.ID, projectBoundary(f.projectID), "project:read", "agent:read")
		hubNoManage := mtMint(t, f.srv, f.owner.ID, hubBoundary(), "project:read", "agent:read")
		otherProject := mtMint(t, f.srv, f.owner.ID, projectBoundary(f.otherProjectID), mtNarrowSelectors...)
		before := len(mmrAuditRows(t, f.store, f.projectID))
		for _, s := range steps(t, f) {
			for name, key := range map[string]string{"without project:manage": noManage, "hub boundary without project:manage": hubNoManage, "bound to another project": otherProject} {
				rec := doRequestWithUAT(t, f.srv, key, s.method, s.path, s.body)
				assert.Equal(t, http.StatusForbidden, rec.Code, "%s %s: %s", s.name, name, rec.Body.String())
			}
		}
		assert.Len(t, mmrAuditRows(t, f.store, f.projectID), before, "a refused token changes no membership")
		assert.Equal(t, store.ProjectRoleOwner, mmrRoleNameOf(t, f, f.owner.ID))
	})

	admitted := map[string]func(t *testing.T, f *mmrFixture) func(s step) *httptest.ResponseRecorder{
		"project token": func(t *testing.T, f *mmrFixture) func(s step) *httptest.ResponseRecorder {
			key := mtMint(t, f.srv, f.owner.ID, projectBoundary(f.projectID), mtNarrowSelectors...)
			return func(s step) *httptest.ResponseRecorder {
				return doRequestWithUAT(t, f.srv, key, s.method, s.path, s.body)
			}
		},
		"hub token": func(t *testing.T, f *mmrFixture) func(s step) *httptest.ResponseRecorder {
			key := mtMint(t, f.srv, f.owner.ID, hubBoundary(), mtNarrowSelectors...)
			return func(s step) *httptest.ResponseRecorder {
				return doRequestWithUAT(t, f.srv, key, s.method, s.path, s.body)
			}
		},
		"session": func(t *testing.T, f *mmrFixture) func(s step) *httptest.ResponseRecorder {
			return func(s step) *httptest.ResponseRecorder {
				return doRequestAsUser(t, f.srv, f.owner, s.method, s.path, s.body)
			}
		},
	}
	for name, as := range admitted {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			do := as(t, f)
			for _, s := range steps(t, f) {
				rec := do(s)
				assert.Less(t, rec.Code, 300, "%s: %s", s.name, rec.Body.String())
			}
			assert.Equal(t, store.ProjectRoleOwner, mmrRoleNameOf(t, f, f.member.ID), "ownership transferred")
		})
	}
}

// TestMembershipToken_ProjectBoundaryHolderMustStayMember pins that a
// project token stops admitting membership changes once its holder leaves
// the project.
func TestMembershipToken_ProjectBoundaryHolderMustStayMember(t *testing.T) {
	f := setupMMRFixture(t)
	adminTok := mtMint(t, f.srv, f.admin.ID, projectBoundary(f.projectID), "project:manage")
	_, binding := mtMemberUser(t, f, "m", f.memberRD)
	pinDeleteDirectBindings(t, f.store, f.admin.ID, f.projectID)
	rec := doRequestWithUAT(t, f.srv, adminTok, http.MethodDelete, pinMemberPath(f.projectID, binding), nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.True(t, mtBindingExists(t, f.store, binding))
}

// TestMembershipActor_ServiceAdmitsTokenOnlyUnderProjectManage pins the
// service-boundary rule for direct callers: a token context is admitted
// only with a token identity whose selectors carry project:manage for the
// project, and a session or dev context is admitted as an identity that is
// not a token.
func TestMembershipActor_ServiceAdmitsTokenOnlyUnderProjectManage(t *testing.T) {
	f := setupMMRFixture(t)
	svc := f.srv.membershipService
	withManage := realTokenContext(t, f.srv, mtMint(t, f.srv, f.owner.ID, projectBoundary(f.projectID), "project:manage"))
	withoutManage := realTokenContext(t, f.srv, mtMint(t, f.srv, f.owner.ID, projectBoundary(f.projectID), "project:read"))

	assert.Nil(t, svc.checkMembershipActor(withManage, f.owner.ID, f.projectID))
	d := svc.checkMembershipActor(withoutManage, f.owner.ID, f.projectID)
	require.NotNil(t, d)
	assert.Equal(t, http.StatusForbidden, d.HTTPStatus)
	d = svc.checkMembershipActor(withManage, f.owner.ID, f.otherProjectID)
	require.NotNil(t, d, "the token's boundary does not cover the other project")
	d = svc.checkMembershipActor(withManage, f.admin.ID, f.projectID)
	require.NotNil(t, d, "the token identity must be the actor")
	assert.Equal(t, ErrCodeMembershipCredentialInsufficient, d.DenialCode)

	// A token identity under a session credential kind is refused.
	scoped := GetIdentityFromContext(withManage)
	mixed := contextWithCredentialContext(contextWithIdentity(context.Background(), scoped), CredentialContext{Kind: CredentialKindInteractive})
	d = svc.checkMembershipActor(mixed, f.owner.ID, f.projectID)
	require.NotNil(t, d)
	assert.Equal(t, ErrCodeMembershipCredentialInsufficient, d.DenialCode)

	for _, kind := range []CredentialKind{CredentialKindInteractive, CredentialKindDev} {
		ctx := serviceCallContext(f.owner.ID, f.owner.Email, "Owner", kind)
		assert.Nil(t, svc.checkMembershipActor(ctx, f.owner.ID, f.projectID), "%s", kind)
	}
}

// TestRoleBindingDelete_ProjectBindingUnderProjectGovernance pins the
// session rule of DELETE /api/v1/admin/role-bindings/{id}: a project-scoped
// binding is removed under project governance (owner and admin sessions
// remove a member binding, an admin session cannot remove the owner), a
// member or non-member session needs hub role_binding.delete, and a
// hub-scoped binding or an unknown ID goes through the hub check first.
func TestRoleBindingDelete_ProjectBindingUnderProjectGovernance(t *testing.T) {
	f := setupMMRFixture(t)
	ctx := context.Background()

	_, b1 := mtMemberUser(t, f, "b1", f.memberRD)
	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodDelete, mtRoleBindingPath(b1), nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.False(t, mtBindingExists(t, f.store, b1))

	_, b2 := mtMemberUser(t, f, "b2", f.memberRD)
	rec = doRequestAsUser(t, f.srv, f.admin, http.MethodDelete, mtRoleBindingPath(b2), nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.False(t, mtBindingExists(t, f.store, b2))

	ownerBinding := pinBindingID(t, f.store, f.owner.ID, f.projectID)
	rec = doRequestAsUser(t, f.srv, f.admin, http.MethodDelete, mtRoleBindingPath(ownerBinding), nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, ErrCodeTargetRoleProtected, mtErrCode(rec), rec.Body.String())
	assert.True(t, mtBindingExists(t, f.store, ownerBinding))

	_, b3 := mtMemberUser(t, f, "b3", f.memberRD)
	outsider := grpUser(t, f.store, t.Name()+"-outsider", "Outsider")
	for name, actor := range map[string]*store.User{"member": f.member, "non-member": outsider} {
		rec = doRequestAsUser(t, f.srv, actor, http.MethodDelete, mtRoleBindingPath(b3), nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", name, rec.Body.String())
		assert.True(t, mtBindingExists(t, f.store, b3), name)
	}

	hubMember, err := f.store.GetRoleDefinitionByName(ctx, store.SystemRoleHubMember, store.RoleScopeSystem)
	require.NoError(t, err)
	sys, err := f.store.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: hubMember.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: outsider.ID,
		ScopeType: store.RoleScopeSystem, CreatedBy: "test",
	})
	require.NoError(t, err)
	rec = doRequestAsUser(t, f.srv, f.owner, http.MethodDelete, mtRoleBindingPath(sys.ID), nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.True(t, mtBindingExists(t, f.store, sys.ID))
	rec = doRequestAsUser(t, f.srv, f.owner, http.MethodDelete, mtRoleBindingPath("00000000-0000-0000-0000-000000000000"), nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, "an unknown ID is refused before lookup: %s", rec.Body.String())
	rec = doRequest(t, f.srv, http.MethodDelete, mtRoleBindingPath(sys.ID), nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
}

// TestMembershipToken_ChangeAndAuditCommitTogether pins, for membership
// changes sent with a token, that the change and its one audit record
// commit in the same transaction: an injected audit failure leaves the
// membership as it was.
func TestMembershipToken_ChangeAndAuditCommitTogether(t *testing.T) {
	for _, c := range []struct {
		name, mutation string
	}{
		{"remove", "project_member_remove"},
		{"add", "project_member_add"},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv, real, fault, heal := newGrantAuditFaultServer(t, grantAuditFaultStore{auditErrFor: c.mutation})
			// The membership service writes through its own store, so it
			// gets the same switch-gated wrapper.
			svcStore := srv.membershipService.store
			srv.membershipService.store = &grantAuditFaultStore{Store: svcStore, fault: fault, heal: heal, auditErrFor: c.mutation}
			t.Cleanup(func() { srv.membershipService.store = svcStore })
			ctx := context.Background()
			projectID, ownerID := tid(t.Name()+"-p"), tid(t.Name()+"-o")
			createRS1Project(t, real, projectID, ownerID)
			memberRD, err := real.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
			require.NoError(t, err)
			require.NoError(t, real.UpdateSystemRoleDefinitionPermissions(ctx, memberRD.ID, mtNarrowPerms))
			target := tid(t.Name() + "-t")
			require.NoError(t, real.CreateUser(ctx, &store.User{ID: target, Email: target + "@test.com", DisplayName: "T", Role: "member", Status: "active"}))
			ensureHubMembership(ctx, real, target)
			key := mtMint(t, srv, ownerID, projectBoundary(projectID), mtNarrowSelectors...)

			var method, path string
			var body interface{}
			if c.name == "remove" {
				b, err := real.CreateRoleBinding(ctx, &store.RoleBinding{RoleDefinitionID: memberRD.ID, PrincipalType: store.RoleBindingPrincipalUser,
					PrincipalID: target, ScopeType: store.RoleScopeProject, ScopeID: projectID, CreatedBy: ownerID})
				require.NoError(t, err)
				method, path = http.MethodDelete, pinMemberPath(projectID, b.ID)
			} else {
				method, path = http.MethodPost, legacyMembersPath(projectID)
				body = addProjectMemberRequest{RoleDefinitionID: memberRD.ID, PrincipalType: "user", PrincipalID: target}
			}
			bindings := func() int {
				all, err := real.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, target)
				require.NoError(t, err)
				return len(all)
			}
			before := bindings()

			fault.Arm()
			rec := doRequestWithUAT(t, srv, key, method, path, body)
			assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
			assert.Equal(t, before, bindings(), "the membership stays as it was after the audit failure")
			assert.Empty(t, grantAudits(t, real, c.mutation, projectID))

			heal.Arm()
			rec = doRequestWithUAT(t, srv, key, method, path, body)
			require.Less(t, rec.Code, 300, rec.Body.String())
			assert.NotEqual(t, before, bindings())
			audits := grantAudits(t, real, c.mutation, projectID)
			require.Len(t, audits, 1, "exactly one audit record")
			assert.Equal(t, ownerID, audits[0].ActorPrincipalID)
			assert.Contains(t, audits[0].AfterSummary+audits[0].BeforeSummary, target)
		})
	}
}
