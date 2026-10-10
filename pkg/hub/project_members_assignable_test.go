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

	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// GET …/members/assignable-roles and MembershipCapabilities.canManageCustomRoles
// (ptone/scion#2529 P2).
// =============================================================================

func assignableRolesPath(projectID string) string {
	return "/api/v1/projects/" + projectID + "/members/assignable-roles"
}

func getAssignableRoles(t *testing.T, f *mmrFixture, actor *store.User) map[string]AssignableProjectRole {
	t.Helper()
	rec := doRequestAsUser(t, f.srv, actor, http.MethodGet, assignableRolesPath(f.projectID), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body listAssignableRolesResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	byID := make(map[string]AssignableProjectRole, len(body.Items))
	for _, it := range body.Items {
		byID[it.ID] = it
	}
	require.Len(t, byID, len(body.Items), "no role is listed twice")
	return byID
}

// asgManagerActor creates a user with NO built-in project role who still
// passes the endpoint's project.manage gate, through a custom project role
// carrying project.read and project.manage. With hubAdmin it also holds the
// system hub-admin role (system role_binding.*): the hub-override actor
// reachable over HTTP. Without it, the actor has neither a project role nor
// hub authority, so the PUT refuses it before governance. A plain hub admin
// is refused at the project.manage gate (see
// TestAssignableRoles_HubAdminWithNoProjectRole).
func asgManagerActor(t *testing.T, f *mmrFixture, suffix string, hubAdmin bool) (*store.User, *store.RoleDefinition) {
	t.Helper()
	ctx := context.Background()
	manager, err := f.store.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name: "asg-manager-" + tid(t.Name() + suffix)[:8], ScopeType: store.RoleScopeProject,
		Permissions: []string{"project.read", "project.manage"},
	})
	require.NoError(t, err)
	u := grpUser(t, f.store, t.Name()+"-"+suffix, "Manager "+suffix)
	grpBind(t, f.store, "user", u.ID, manager.ID, f.projectID)
	if hubAdmin {
		mmrSeedHubAdmin(t, f.store, u.ID)
	}
	return u, manager
}

// asgHubOverrideActor is asgManagerActor with the hub-admin seed.
func asgHubOverrideActor(t *testing.T, f *mmrFixture) (*store.User, *store.RoleDefinition) {
	t.Helper()
	return asgManagerActor(t, f, "huboverride", true)
}

// asgJSONNormalize round-trips v through JSON so maps built with different
// Go types compare equal when their JSON is equal.
func asgJSONNormalize(t *testing.T, v interface{}) interface{} {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	var out interface{}
	require.NoError(t, json.Unmarshal(b, &out))
	return out
}

func TestAssignableRoles_Owner(t *testing.T) {
	f := setupMMRFixture(t)
	roles := getAssignableRoles(t, f, f.owner)

	for _, rd := range []*store.RoleDefinition{f.ownerRD, f.adminRD, f.memberRD} {
		r, ok := roles[rd.ID]
		require.True(t, ok, "built-in %s listed", rd.Name)
		assert.Equal(t, roleKindBuiltIn, r.RoleKind)
		assert.True(t, r.Grantable, "owner can grant built-in %s: %+v", rd.Name, r)
		assert.Empty(t, r.Reason)
		assert.Empty(t, r.DenialCode)
	}

	within := roles[f.withinCeiling.ID]
	assert.Equal(t, roleKindCustom, within.RoleKind)
	assert.True(t, within.Grantable, "within-ceiling custom role grantable for owner: %+v", within)

	beyond := roles[f.beyondCeiling.ID]
	assert.False(t, beyond.Grantable)
	assert.Equal(t, ErrCodeTargetRoleProtected, beyond.DenialCode)
	assert.Contains(t, beyond.Reason, "agent.attach", "beyond-ceiling reason names the missing permission")
	assert.Equal(t, f.beyondCeiling.ID, beyond.Details["roleDefinitionId"])

	rb := roles[f.roleBindingCustom.ID]
	assert.False(t, rb.Grantable, "a role_binding.*-bearing custom role is never grantable")
	assert.Equal(t, ErrCodeRoleAssignmentForbidden, rb.DenialCode)
	assert.Contains(t, rb.Reason, "role_binding.")
	assert.Equal(t, f.roleBindingCustom.ID, rb.Details["roleDefinitionId"])
}

func TestAssignableRoles_Admin(t *testing.T) {
	f := setupMMRFixture(t)
	roles := getAssignableRoles(t, f, f.admin)

	assert.True(t, roles[f.memberRD.ID].Grantable, "admin can grant member: %+v", roles[f.memberRD.ID])
	for _, rd := range []*store.RoleDefinition{f.ownerRD, f.adminRD} {
		r := roles[rd.ID]
		assert.False(t, r.Grantable, "admin cannot grant %s", rd.Name)
		assert.Equal(t, ErrCodeTargetRoleProtected, r.DenialCode)
		assert.NotEmpty(t, r.Reason)
	}

	// Custom roles are refused on authority, and the refusal is keyed on
	// details.requiredPermission, not on reason text.
	for _, rd := range []*store.RoleDefinition{f.withinCeiling, f.beyondCeiling} {
		r := roles[rd.ID]
		assert.False(t, r.Grantable, "admin cannot grant custom %s", rd.Name)
		assert.Equal(t, ErrCodeRoleAssignmentForbidden, r.DenialCode)
		assert.Equal(t, PermRoleBindingCreate, r.Details["requiredPermission"])
		assert.NotEmpty(t, r.Reason)
	}
	// The structural role_binding.* refusal precedes authority, as on the PUT.
	rb := roles[f.roleBindingCustom.ID]
	assert.False(t, rb.Grantable)
	assert.Contains(t, rb.Reason, "role_binding.")
	assert.Nil(t, rb.Details["requiredPermission"])
}

func TestAssignableRoles_AdminWithHubRoleBindingStillRefusedCustom(t *testing.T) {
	f := setupMMRFixture(t)
	mmrSeedHubAdmin(t, f.store, f.admin.ID)
	roles := getAssignableRoles(t, f, f.admin)
	r := roles[f.withinCeiling.ID]
	assert.False(t, r.Grantable, "the hub fallback applies only to actors with no project role")
	assert.Equal(t, PermRoleBindingCreate, r.Details["requiredPermission"])
}

func TestAssignableRoles_MemberWithoutManageForbidden(t *testing.T) {
	f := setupMMRFixture(t)
	rec := doRequestAsUser(t, f.srv, f.member, http.MethodGet, assignableRolesPath(f.projectID), nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

func TestAssignableRoles_MethodNotAllowed(t *testing.T) {
	f := setupMMRFixture(t)
	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPost, assignableRolesPath(f.projectID), map[string]string{})
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code, rec.Body.String())
}

func TestAssignableRoles_SystemRolesNeverListedAndOrdered(t *testing.T) {
	f := setupMMRFixture(t)
	ctx := context.Background()
	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, assignableRolesPath(f.projectID), nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var body listAssignableRolesResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))

	all, err := f.store.ListRoleDefinitions(ctx)
	require.NoError(t, err)
	projectRoles := 0
	for _, rd := range all {
		if rd.ScopeType == store.RoleScopeProject {
			projectRoles++
		}
	}
	require.Len(t, body.Items, projectRoles, "every project-scoped role is listed")
	for _, it := range body.Items {
		rd, err := f.store.GetRoleDefinition(ctx, it.ID)
		require.NoError(t, err)
		assert.Equal(t, store.RoleScopeProject, rd.ScopeType, "system role %s must not be listed", rd.Name)
	}

	// Built-ins first, in tier order; then custom roles by name.
	require.GreaterOrEqual(t, len(body.Items), 3)
	assert.Equal(t, store.ProjectRoleOwner, body.Items[0].Name)
	assert.Equal(t, store.ProjectRoleAdmin, body.Items[1].Name)
	assert.Equal(t, store.ProjectRoleMember, body.Items[2].Name)
	for i := 4; i < len(body.Items); i++ {
		assert.Equal(t, roleKindCustom, body.Items[i].RoleKind)
		assert.LessOrEqual(t, body.Items[i-1].Name, body.Items[i].Name)
	}
}

func TestAssignableRoles_IsReadOnly(t *testing.T) {
	f := setupMMRFixture(t)
	ctx := context.Background()
	before, err := f.store.ListRoleBindingsForScope(ctx, store.RoleScopeProject, f.projectID)
	require.NoError(t, err)
	auditBefore := len(mmrAuditRows(t, f.store, f.projectID))

	getAssignableRoles(t, f, f.owner)
	getAssignableRoles(t, f, f.admin)

	after, err := f.store.ListRoleBindingsForScope(ctx, store.RoleScopeProject, f.projectID)
	require.NoError(t, err)
	assert.Len(t, after, len(before))
	assert.Len(t, mmrAuditRows(t, f.store, f.projectID), auditBefore)
}

// TestAssignableRoles_HubAdminWithNoProjectRole: a plain hub admin (system
// hub-admin, no project role) does not hold project.manage, so the endpoint
// refuses it at the gate, as it refuses every project.manage-gated members
// endpoint. Its authority as computed by the service (the hub override)
// grants custom roles within its own ceiling and refuses roles beyond it.
func TestAssignableRoles_HubAdminWithNoProjectRole(t *testing.T) {
	f := setupMMRFixture(t)
	ctx := context.Background()
	hubAdmin := grpUser(t, f.store, t.Name()+"-hubadmin", "Hub Admin")
	mmrSeedHubAdmin(t, f.store, hubAdmin.ID)

	rec := doRequestAsUser(t, f.srv, hubAdmin, http.MethodGet, assignableRolesPath(f.projectID), nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, "hub-admin lacks project.manage: %s", rec.Body.String())

	readOnly, err := f.store.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name: "asg-readonly-" + tid(t.Name())[:8], ScopeType: store.RoleScopeProject, Permissions: []string{"project.read"},
	})
	require.NoError(t, err)

	svcCtx := mmrServiceCtx(hubAdmin.ID, hubAdmin.Email)
	items, err := f.srv.membershipService.AssignableRoles(svcCtx, mmrServiceIdentity(hubAdmin.ID, hubAdmin.Email), f.projectID)
	require.NoError(t, err)
	byID := map[string]AssignableProjectRole{}
	for _, it := range items {
		byID[it.ID] = it
	}
	assert.True(t, byID[readOnly.ID].Grantable, "within the hub admin's own ceiling: %+v", byID[readOnly.ID])
	assert.False(t, byID[f.withinCeiling.ID].Grantable, "member permissions are beyond the hub admin's ceiling")
	assert.Equal(t, ErrCodeTargetRoleProtected, byID[f.withinCeiling.ID].DenialCode)
	assert.False(t, byID[f.memberRD.ID].Grantable, "the hub-admin ceiling does not cover built-in project roles")
	assert.Equal(t, ErrCodeTargetRoleProtected, byID[f.memberRD.ID].DenialCode)
	assert.False(t, byID[f.roleBindingCustom.ID].Grantable)
}

// TestAssignableRoles_HubOverrideOverHTTP: the hub fallback reached over
// HTTP by an actor with no built-in project role.
func TestAssignableRoles_HubOverrideOverHTTP(t *testing.T) {
	f := setupMMRFixture(t)
	actor, manager := asgHubOverrideActor(t, f)
	roles := getAssignableRoles(t, f, actor)
	assert.True(t, roles[manager.ID].Grantable, "hub override grants a custom role within the actor's ceiling: %+v", roles[manager.ID])
	assert.False(t, roles[f.roleBindingCustom.ID].Grantable)
}

func TestAssignableRoles_CredentialGateRefusesEverything(t *testing.T) {
	f := setupMMRFixture(t)
	// A context with the owner's identity but no credential kind: the PUT's
	// credential gate refuses it, so nothing is grantable.
	identity := mmrServiceIdentity(f.owner.ID, f.owner.Email)
	ctx := contextWithIdentity(context.Background(), identity)
	items, err := f.srv.membershipService.AssignableRoles(ctx, identity, f.projectID)
	require.NoError(t, err)
	require.NotEmpty(t, items)
	for _, it := range items {
		assert.False(t, it.Grantable, "%s", it.Name)
		assert.Equal(t, ErrCodeMembershipCredentialInsufficient, it.DenialCode)
	}
}

// TestAssignableRoles_TokenItemsCarrySessionOnlyDetails pins that a user
// access token passing the endpoint's project.manage gate gets every role
// listed as not grantable, each item carrying the PUT's credential refusal:
// denialCode credential_insufficient with details.reason GOV_PENDING and
// details.credential session_required.
func TestAssignableRoles_TokenItemsCarrySessionOnlyDetails(t *testing.T) {
	f := setupMMRFixture(t)
	ensureHubMembership(context.Background(), f.store, f.owner.ID)
	key, _, err := f.srv.uatService.CreateTokenWithParams(rs4MintContext(f.owner.ID), CreateTokenParams{
		UserID: f.owner.ID, Name: "asg-token", Boundary: projectBoundary(f.projectID), Scopes: []string{"project:manage"},
	})
	require.NoError(t, err)

	rec := doRequestWithUAT(t, f.srv, key, http.MethodGet, assignableRolesPath(f.projectID), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body listAssignableRolesResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.NotEmpty(t, body.Items)
	for _, it := range body.Items {
		assert.False(t, it.Grantable, "%s", it.Name)
		assert.Equal(t, ErrCodeMembershipCredentialInsufficient, it.DenialCode, "%s", it.Name)
		assert.Equal(t, string(authzop.ReasonGovernancePending), it.Details["reason"], "%s: details.reason", it.Name)
		assert.Equal(t, sessionRequiredCredential, it.Details["credential"], "%s: details.credential", it.Name)
	}
}

// -----------------------------------------------------------------------------
// Capabilities
// -----------------------------------------------------------------------------

func TestCapabilities_CanManageCustomRoles(t *testing.T) {
	f := setupMMRFixture(t)
	ctx := context.Background()
	svc := f.srv.membershipService

	assert.True(t, svc.ComputeCapabilities(ctx, f.owner.ID, f.projectID).CanManageCustomRoles, "direct owner")
	assert.False(t, svc.ComputeCapabilities(ctx, f.admin.ID, f.projectID).CanManageCustomRoles, "project admin")
	assert.False(t, svc.ComputeCapabilities(ctx, f.member.ID, f.projectID).CanManageCustomRoles, "project member")

	// A project admin who also holds hub role_binding.* is still refused:
	// the hub fallback applies only to actors with no project role.
	mmrSeedHubAdmin(t, f.store, f.admin.ID)
	assert.False(t, svc.ComputeCapabilities(ctx, f.admin.ID, f.projectID).CanManageCustomRoles, "project admin with hub role_binding.*")

	// The no-project-role hub fallback.
	hubAdmin := grpUser(t, f.store, t.Name()+"-hubadmin", "Hub Admin")
	mmrSeedHubAdmin(t, f.store, hubAdmin.ID)
	assert.True(t, svc.ComputeCapabilities(ctx, hubAdmin.ID, f.projectID).CanManageCustomRoles, "hub admin with no project role")

	// A user with neither.
	nobody := grpUser(t, f.store, t.Name()+"-nobody", "Nobody")
	assert.False(t, svc.ComputeCapabilities(ctx, nobody.ID, f.projectID).CanManageCustomRoles)
}

func TestCapabilities_CanManageCustomRolesOnFlatList(t *testing.T) {
	f := setupMMRFixture(t)
	for _, tc := range []struct {
		actor *store.User
		want  bool
	}{{f.owner, true}, {f.admin, false}} {
		rec := doRequestAsUser(t, f.srv, tc.actor, http.MethodGet, "/api/v1/projects/"+f.projectID+"/members", nil)
		require.Equal(t, http.StatusOK, rec.Code)
		var raw struct {
			Capabilities map[string]interface{} `json:"_capabilities"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
		assert.Equal(t, tc.want, raw.Capabilities["canManageCustomRoles"], "actor %s", tc.actor.ID)
	}
}

// -----------------------------------------------------------------------------
// Consistency: assignable-roles agrees with the PUT.
// -----------------------------------------------------------------------------

// TestAssignableRoles_ConsistentWithPut: for every fixture role and actor,
// a role reported grantable=true is accepted by a P1 PUT by the same actor
// creating it on a fresh user, and a role reported grantable=false is
// refused by that PUT with the reported denial code, writing nothing.
func TestAssignableRoles_ConsistentWithPut(t *testing.T) {
	f := setupMMRFixture(t)
	hubOverride, manager := asgHubOverrideActor(t, f)
	// No built-in project role and no hub authority: passes the
	// project.manage gate through a custom role only.
	customManager, customManagerRole := asgManagerActor(t, f, "custommanager", false)

	actors := []struct {
		name string
		user *store.User
	}{
		{"owner", f.owner},
		{"admin", f.admin},
		{"hub-override", hubOverride},
		{"custom-manager", customManager},
	}
	fixtureRoles := []*store.RoleDefinition{
		f.ownerRD, f.adminRD, f.memberRD,
		f.withinCeiling, f.beyondCeiling, f.roleBindingCustom, manager, customManagerRole,
	}

	sawGrantable, sawRefused := false, false
	for _, a := range actors {
		roles := getAssignableRoles(t, f, a.user)
		for _, rd := range fixtureRoles {
			t.Run(a.name+"/"+rd.Name, func(t *testing.T) {
				r, ok := roles[rd.ID]
				require.True(t, ok, "role %s listed", rd.Name)

				target := grpUser(t, f.store, t.Name()+"-target", "Target")
				rec := putMemberRoles(t, f.srv, a.user, f.projectID, "user", target.ID, []string{rd.ID}, &[]string{})
				bindings := mmrBindingsFor(t, f.store, "user", target.ID, f.projectID)

				if r.Grantable {
					sawGrantable = true
					assert.Equal(t, http.StatusCreated, rec.Code, "grantable=true must be accepted by the PUT: %s", rec.Body.String())
					require.Len(t, bindings, 1)
					assert.Equal(t, rd.ID, bindings[0].RoleDefinitionID)
					return
				}
				sawRefused = true
				assert.GreaterOrEqual(t, rec.Code, 400, "grantable=false must be refused by the PUT: %s", rec.Body.String())
				assert.Less(t, rec.Code, 500)
				var errBody ErrorResponse
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errBody))
				assert.Equal(t, r.DenialCode, errBody.Error.Code, "same denial code as the PUT (reason %q vs %q)", r.Reason, errBody.Error.Message)
				assert.Equal(t, r.Reason, errBody.Error.Message, "same reason as the PUT")
				assert.Equal(t, asgJSONNormalize(t, r.Details), asgJSONNormalize(t, errBody.Error.Details), "same details as the PUT")
				assert.Empty(t, bindings, "a refused PUT writes nothing")
			})
		}
	}
	assert.True(t, sawGrantable, "fixture exercises grantable roles")
	assert.True(t, sawRefused, "fixture exercises refused roles")
}

// TestAssignableRoles_CustomManagerWithNoProjectRole: an actor with no
// built-in project role and no hub role_binding.* authority, who passes the
// project.manage gate through a custom role, is refused every role. The
// structural role_binding.* refusal comes first, as on the PUT; every other
// role is refused because the actor has no project role.
func TestAssignableRoles_CustomManagerWithNoProjectRole(t *testing.T) {
	f := setupMMRFixture(t)
	actor, managerRole := asgManagerActor(t, f, "custommanager", false)
	roles := getAssignableRoles(t, f, actor)

	for _, rd := range []*store.RoleDefinition{f.ownerRD, f.adminRD, f.memberRD, f.withinCeiling, f.beyondCeiling, managerRole} {
		r := roles[rd.ID]
		assert.False(t, r.Grantable, "%s", rd.Name)
		assert.Equal(t, ErrCodeRoleAssignmentForbidden, r.DenialCode, "%s", rd.Name)
		assert.Equal(t, "actor has no project role", r.Reason, "%s", rd.Name)
		assert.Nil(t, r.Details, "%s", rd.Name)
	}
	rb := roles[f.roleBindingCustom.ID]
	assert.False(t, rb.Grantable)
	assert.Equal(t, ErrCodeRoleAssignmentForbidden, rb.DenialCode)
	assert.Contains(t, rb.Reason, "role_binding.", "the structural refusal precedes the no-project-role refusal")
	assert.Equal(t, f.roleBindingCustom.ID, rb.Details["roleDefinitionId"])
}

// TestAssignableRoles_AgentTokenGetsCredentialInsufficient: a non-user
// identity is refused with the members PUT's credential_insufficient code.
func TestAssignableRoles_AgentTokenGetsCredentialInsufficient(t *testing.T) {
	f := setupMMRFixture(t)
	ctx := context.Background()
	agentID := tid(t.Name() + "-agent")
	require.NoError(t, f.store.CreateAgent(ctx, &store.Agent{
		ID: agentID, Slug: agentID, Name: "asg-agent", ProjectID: f.projectID,
		Phase: "running", CreatedBy: f.owner.ID, OwnerID: f.owner.ID, Ancestry: []string{f.owner.ID},
	}))
	agentToken, err := f.srv.GenerateAgentToken(agentID, f.projectID, []string{f.owner.ID}, AgentRoleFull, nil)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, assignableRolesPath(f.projectID), nil)
	req.Header.Set("Authorization", "Bearer "+agentToken)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	var errBody ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errBody))
	assert.Equal(t, ErrCodeMembershipCredentialInsufficient, errBody.Error.Code)
}

// TestAssignableRoles_RoutingKeepsMemberAddressing: the literal
// members/assignable-roles segment does not shadow principal or binding-ID
// addressing.
func TestAssignableRoles_RoutingKeepsMemberAddressing(t *testing.T) {
	f := setupMMRFixture(t)
	ctx := context.Background()
	membersPath := "/api/v1/projects/" + f.projectID + "/members"

	// A principal literally named "assignable-roles" (a group with that
	// slug) is still addressed through members/principals/{type}/{id}.
	const literal = "assignable-roles"
	groupID := tid(t.Name() + "-group")
	require.NoError(t, f.store.CreateGroup(ctx, &store.Group{ID: groupID, Name: "literal", Slug: literal}))
	rec := putMemberRoles(t, f.srv, f.owner, f.projectID, "group", literal, []string{f.memberRD.ID}, &[]string{})
	require.Equal(t, http.StatusCreated, rec.Code, "principal handler: %s", rec.Body.String())
	bindings := mmrBindingsFor(t, f.store, "group", groupID, f.projectID)
	require.Len(t, bindings, 1)
	assert.Equal(t, f.memberRD.ID, bindings[0].RoleDefinitionID)

	// members/principals/user/assignable-roles reaches the principal
	// handler too: PUT is answered by it (no such user; never the
	// assignable-roles handler's GET-only 405), and GET gets the principal
	// handler's PUT/DELETE-only 405.
	rec = putMemberRoles(t, f.srv, f.owner, f.projectID, "user", literal, []string{f.memberRD.ID}, &[]string{})
	assert.NotEqual(t, http.StatusMethodNotAllowed, rec.Code, rec.Body.String())
	assert.GreaterOrEqual(t, rec.Code, 400, rec.Body.String())
	rec = doRequestAsUser(t, f.srv, f.owner, http.MethodGet, mmrPrincipalPath(f.projectID, "user", literal), nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	assert.Equal(t, "PUT, DELETE", rec.Header().Get("Allow"), "principal handler")

	// ID addressing still works alongside the new route.
	byID := grpUser(t, f.store, t.Name()+"-byid", "By ID")
	rec = putMemberRoles(t, f.srv, f.owner, f.projectID, "user", byID.ID, []string{f.memberRD.ID}, &[]string{})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Len(t, mmrBindingsFor(t, f.store, "user", byID.ID, f.projectID), 1)

	// Email addressing still works alongside the new route.
	byEmail := grpUser(t, f.store, t.Name()+"-byemail", "By Email")
	rec = putMemberRoles(t, f.srv, f.owner, f.projectID, "user", byEmail.Email, []string{f.memberRD.ID}, &[]string{})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Len(t, mmrBindingsFor(t, f.store, "user", byEmail.ID, f.projectID), 1)

	// PATCH/DELETE members/assignable-roles are pinned to 405 (GET only) and
	// write nothing; they no longer reach the binding-ID handler.
	before, err := f.store.ListRoleBindingsForScope(ctx, store.RoleScopeProject, f.projectID)
	require.NoError(t, err)
	for _, method := range []string{http.MethodPatch, http.MethodDelete} {
		rec = doRequestAsUser(t, f.srv, f.owner, method, membersPath+"/assignable-roles", map[string]string{"role": store.ProjectRoleMember})
		assert.Equal(t, http.StatusMethodNotAllowed, rec.Code, "%s: %s", method, rec.Body.String())
		assert.Equal(t, http.MethodGet, rec.Header().Get("Allow"), method)
	}
	after, err := f.store.ListRoleBindingsForScope(ctx, store.RoleScopeProject, f.projectID)
	require.NoError(t, err)
	assert.Len(t, after, len(before))

	// A deeper path still falls to the binding-ID handler.
	rec = doRequestAsUser(t, f.srv, f.owner, http.MethodGet, membersPath+"/assignable-roles/x", nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	assert.Equal(t, "PATCH, DELETE", rec.Header().Get("Allow"), "binding-ID handler")

	// A real binding ID is still addressable.
	rec = doRequestAsUser(t, f.srv, f.owner, http.MethodDelete, membersPath+"/"+bindings[0].ID, nil)
	assert.Less(t, rec.Code, 300, "binding-ID DELETE: %s", rec.Body.String())
	assert.Empty(t, mmrBindingsFor(t, f.store, "group", groupID, f.projectID))
}

// TestSetMemberRoles_MalformedUserPrincipalID400: a user principal addressed
// by something that is neither an email nor a well-formed user ID is a 400
// invalid_request on both PUT and DELETE. It used to reach the store and
// surface as a 500 (PUT) or a "no bindings" 404 (DELETE)
// (ptone/scion#2529, review r2 L-500). Nothing is written.
func TestSetMemberRoles_MalformedUserPrincipalID400(t *testing.T) {
	f := setupMMRFixture(t)
	const malformed = "not-a-uuid"

	rec := putMemberRoles(t, f.srv, f.owner, f.projectID, "user", malformed, []string{f.memberRD.ID}, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"code":"`+ErrCodeInvalidRequest+`"`)

	rec = deleteMemberRoles(t, f.srv, f.owner, f.projectID, "user", malformed)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"code":"`+ErrCodeInvalidRequest+`"`)

	assert.Empty(t, mmrBindingsFor(t, f.store, "user", malformed, f.projectID))

	// Well-formed addressing is unaffected: a UUID with no bindings is
	// still DELETE's 404, and an unknown email still PUT's 400.
	rec = deleteMemberRoles(t, f.srv, f.owner, f.projectID, "user", tid(t.Name()+"-nobody"))
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	rec = putMemberRoles(t, f.srv, f.owner, f.projectID, "user", "nobody-l500@test.com", []string{f.memberRD.ID}, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
}

// TestSetMemberRoles_MalformedAgentPrincipalID400: an agent principal
// addressed by anything but a well-formed agent ID is a 400 invalid_request
// on both PUT and DELETE, the same as a malformed user ID. It used to reach
// the store and surface as a 500 (PUT) or a "no bindings" 404 (DELETE)
// (ptone/scion#2529). Nothing is written.
func TestSetMemberRoles_MalformedAgentPrincipalID400(t *testing.T) {
	f := setupMMRFixture(t)
	for _, malformed := range []string{"not-a-uuid", "agent@test.com"} {
		rec := putMemberRoles(t, f.srv, f.owner, f.projectID, "agent", malformed, []string{f.memberRD.ID}, nil)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "PUT %s: %s", malformed, rec.Body.String())
		assert.Contains(t, rec.Body.String(), `"code":"`+ErrCodeInvalidRequest+`"`)

		rec = deleteMemberRoles(t, f.srv, f.owner, f.projectID, "agent", malformed)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "DELETE %s: %s", malformed, rec.Body.String())
		assert.Contains(t, rec.Body.String(), `"code":"`+ErrCodeInvalidRequest+`"`)

		assert.Empty(t, mmrBindingsFor(t, f.store, "agent", malformed, f.projectID))
	}
}
