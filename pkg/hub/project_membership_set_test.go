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
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// SetMemberRoles hub tests (ptone/scion#2529 P1, design.md §12 P1).
//
// Ports miller79/scion PR #127's handlers_roles_owner_custom_test.go fixture
// and all 9 scenarios, re-targeted from POST /admin/role-bindings to
// PUT/DELETE …/members/principals/{type}/{id} (design.md §7). The D1 ruling
// (2026-10-01) blocks custom roles for agent principals, so the ported
// "agent" scenario now expects 400 principal_ineligible instead of 403.
//
// Co-authored-by: Anthony Lofton <6901313+miller79@users.noreply.github.com>
// =============================================================================

type mmrFixture struct {
	srv            *Server
	store          store.Store
	owner          *store.User
	admin          *store.User
	member         *store.User
	projectID      string
	projectSlug    string
	otherProjectID string

	ownerRD, adminRD, memberRD *store.RoleDefinition
	withinCeiling              *store.RoleDefinition // member perms + agent.message: within the owner's ceiling
	beyondCeiling              *store.RoleDefinition // carries agent.attach, which owners do not hold
	roleBindingCustom          *store.RoleDefinition // carries role_binding.create, which owners do not hold
}

func setupMMRFixture(t *testing.T) *mmrFixture {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()

	ownerID := tid(t.Name() + "-owner")
	projectID := tid(t.Name() + "-project")
	createRS1Project(t, s, projectID, ownerID)
	owner, err := s.GetUser(ctx, ownerID)
	require.NoError(t, err)

	_, otherProjectID := func() (string, string) {
		oid := tid(t.Name() + "-other-owner")
		pid := tid(t.Name() + "-other-project")
		createRS1Project(t, s, pid, oid)
		return oid, pid
	}()

	adminID := tid(t.Name() + "-admin")
	createRS1UserWithRole(t, s, adminID, adminID+"@test.com", projectID, store.ProjectRoleAdmin)
	admin, err := s.GetUser(ctx, adminID)
	require.NoError(t, err)

	memberID := tid(t.Name() + "-member")
	createRS1UserWithRole(t, s, memberID, memberID+"@test.com", projectID, store.ProjectRoleMember)
	member, err := s.GetUser(ctx, memberID)
	require.NoError(t, err)

	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	adminRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)
	ownerRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)

	within, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        "mmr-within-" + tid(t.Name())[:8],
		ScopeType:   store.RoleScopeProject,
		Permissions: append(append([]string{}, memberRD.Permissions...), "agent.message"),
	})
	require.NoError(t, err)
	beyond, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        "mmr-beyond-" + tid(t.Name())[:8],
		ScopeType:   store.RoleScopeProject,
		Permissions: []string{"project.read", "agent.list", "agent.read", "agent.attach"},
	})
	require.NoError(t, err)
	rbCustom, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        "mmr-rolebinding-" + tid(t.Name())[:8],
		ScopeType:   store.RoleScopeProject,
		Permissions: []string{"project.read", "role_binding.create", "role_binding.delete"},
	})
	require.NoError(t, err)

	return &mmrFixture{
		srv: srv, store: s,
		owner: owner, admin: admin, member: member,
		projectID: projectID, projectSlug: fmt.Sprintf("rs1-test-%s", projectID[:8]),
		otherProjectID:    otherProjectID,
		ownerRD:           ownerRD,
		adminRD:           adminRD,
		memberRD:          memberRD,
		withinCeiling:     within,
		beyondCeiling:     beyond,
		roleBindingCustom: rbCustom,
	}
}

// ---------------------------------------------------------------------------
// Request helpers
// ---------------------------------------------------------------------------

func mmrPrincipalPath(projectID, principalType, principalID string) string {
	return fmt.Sprintf("/api/v1/projects/%s/members/principals/%s/%s", projectID, principalType, url.PathEscape(principalID))
}

func putMemberRoles(t *testing.T, srv *Server, actor *store.User, projectID, principalType, principalID string, roleIDs []string, expected *[]string) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]interface{}{"roleDefinitionIds": roleIDs}
	if expected != nil {
		body["expectedRoleDefinitionIds"] = *expected
	}
	return doRequestAsUser(t, srv, actor, http.MethodPut, mmrPrincipalPath(projectID, principalType, principalID), body)
}

func deleteMemberRoles(t *testing.T, srv *Server, actor *store.User, projectID, principalType, principalID string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequestAsUser(t, srv, actor, http.MethodDelete, mmrPrincipalPath(projectID, principalType, principalID), nil)
}

// mmrBindingsFor returns the principal's active project-scope bindings.
func mmrBindingsFor(t *testing.T, s store.Store, principalType, principalID, projectID string) []*store.RoleBinding {
	t.Helper()
	bindings, err := s.ListRoleBindingsForPrincipal(context.Background(), principalType, principalID)
	require.NoError(t, err)
	var out []*store.RoleBinding
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeProject && b.ScopeID == projectID {
			out = append(out, b)
		}
	}
	return out
}

func mmrAuditRows(t *testing.T, s store.Store, projectID string) []*store.MutationAuditRecord {
	t.Helper()
	rows, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{
		TargetType: "project_membership",
		TargetID:   projectID,
		Limit:      1000,
	})
	require.NoError(t, err)
	return rows
}

// ---------------------------------------------------------------------------
// Core atomic set/remove-all behavior
// ---------------------------------------------------------------------------

func TestSetMemberRoles_PutAddsBuiltInAndCustomAtomically(t *testing.T) {
	f := setupMMRFixture(t)
	target := tid(t.Name() + "-target")
	require.NoError(t, f.store.CreateUser(context.Background(), &store.User{
		ID: target, Email: target + "@test.com", DisplayName: "Target", Role: "member", Status: "active",
	}))

	rec := putMemberRoles(t, f.srv, f.owner, f.projectID, "user", target,
		[]string{f.adminRD.ID, f.withinCeiling.ID}, nil)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	bindings := mmrBindingsFor(t, f.store, "user", target, f.projectID)
	require.Len(t, bindings, 2)

	rows := mmrAuditRows(t, f.store, f.projectID)
	var addRows []*store.MutationAuditRecord
	for _, r := range rows {
		if r.MutationType == "project_member_add" {
			addRows = append(addRows, r)
		}
	}
	require.Len(t, addRows, 2)
	assert.Equal(t, addRows[0].CorrelationID, addRows[1].CorrelationID, "both rows share one CorrelationID")
	assert.Contains(t, addRows[0].AfterSummary+addRows[1].AfterSummary, `"roleKind":"builtin"`)
	assert.Contains(t, addRows[0].AfterSummary+addRows[1].AfterSummary, `"roleKind":"custom"`)
}

func TestSetMemberRoles_PutChangesBuiltInKeepsCustom(t *testing.T) {
	f := setupMMRFixture(t)
	target := tid(t.Name() + "-target")
	require.NoError(t, f.store.CreateUser(context.Background(), &store.User{
		ID: target, Email: target + "@test.com", DisplayName: "Target", Role: "member", Status: "active",
	}))
	rec := putMemberRoles(t, f.srv, f.owner, f.projectID, "user", target,
		[]string{f.memberRD.ID, f.withinCeiling.ID}, nil)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	before := mmrBindingsFor(t, f.store, "user", target, f.projectID)
	var customBindingID string
	for _, b := range before {
		if b.RoleDefinitionID == f.withinCeiling.ID {
			customBindingID = b.ID
		}
	}
	require.NotEmpty(t, customBindingID)

	rec = putMemberRoles(t, f.srv, f.owner, f.projectID, "user", target,
		[]string{f.adminRD.ID, f.withinCeiling.ID}, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	after := mmrBindingsFor(t, f.store, "user", target, f.projectID)
	require.Len(t, after, 2)
	found := false
	for _, b := range after {
		if b.RoleDefinitionID == f.withinCeiling.ID {
			assert.Equal(t, customBindingID, b.ID, "custom binding ID is unchanged across a built-in role change")
			found = true
		}
	}
	assert.True(t, found)

	rows := mmrAuditRows(t, f.store, f.projectID)
	var changeRows int
	for _, r := range rows {
		if r.MutationType == "project_member_role_change" {
			changeRows++
		}
	}
	assert.Equal(t, 1, changeRows)
}

// TestSetMemberRoles_Escalation_BeyondCeilingLeavesOtherBindingsUnapplied is
// both design.md §12 P1's "atomicity" test and escalation test (i): an owner
// PUT that creates a custom role beyond the owner's own ceiling must leave
// EVERY binding in the request untouched, not just the offending one.
func TestSetMemberRoles_Escalation_BeyondCeilingLeavesOtherBindingsUnapplied(t *testing.T) {
	f := setupMMRFixture(t)
	beforeAudit := len(mmrAuditRows(t, f.store, f.projectID))
	beforeBindings := mmrBindingsFor(t, f.store, "user", f.member.ID, f.projectID)

	rec := putMemberRoles(t, f.srv, f.owner, f.projectID, "user", f.member.ID,
		[]string{f.adminRD.ID, f.beyondCeiling.ID}, nil)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ErrCodeTargetRoleProtected)
	assert.Contains(t, rec.Body.String(), f.beyondCeiling.ID, "details.roleDefinitionId names the offending role")

	afterBindings := mmrBindingsFor(t, f.store, "user", f.member.ID, f.projectID)
	require.Len(t, afterBindings, len(beforeBindings))
	assert.Equal(t, beforeBindings[0].RoleDefinitionID, afterBindings[0].RoleDefinitionID, "the member binding was not touched")
	assert.Equal(t, beforeBindings[0].ID, afterBindings[0].ID)
	assert.Len(t, mmrAuditRows(t, f.store, f.projectID), beforeAudit, "no audit rows for a denied PUT")
}

func TestSetMemberRoles_AtomicityInTransaction_LastOwnerRollsBack(t *testing.T) {
	f := setupMMRFixture(t)
	before := mmrBindingsFor(t, f.store, "user", f.owner.ID, f.projectID)
	require.Len(t, before, 1)

	rec := putMemberRoles(t, f.srv, f.owner, f.projectID, "user", f.owner.ID,
		[]string{f.memberRD.ID}, nil)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ErrCodeLastOwner)

	after := mmrBindingsFor(t, f.store, "user", f.owner.ID, f.projectID)
	require.Len(t, after, 1, "the delete-then-create inside the transaction was rolled back")
	assert.Equal(t, before[0].ID, after[0].ID)
	assert.Equal(t, f.ownerRD.ID, after[0].RoleDefinitionID, "the owner binding, not a member binding, survives")
}

// ---------------------------------------------------------------------------
// Admin tier
// ---------------------------------------------------------------------------

func TestSetMemberRoles_AdminTier_CannotAddCustom(t *testing.T) {
	f := setupMMRFixture(t)
	rec := putMemberRoles(t, f.srv, f.admin, f.projectID, "user", f.member.ID,
		[]string{f.memberRD.ID, f.withinCeiling.ID}, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ErrCodeRoleAssignmentForbidden)
}

func TestSetMemberRoles_AdminTier_CannotRemoveCustom(t *testing.T) {
	f := setupMMRFixture(t)
	// f.member already holds project-member from the fixture, so adding
	// withinCeiling here is a built-in-change-plus-create (200), not a
	// fresh create (201).
	require.Equal(t, http.StatusOK,
		putMemberRoles(t, f.srv, f.owner, f.projectID, "user", f.member.ID, []string{f.memberRD.ID, f.withinCeiling.ID}, nil).Code)

	rec := putMemberRoles(t, f.srv, f.admin, f.projectID, "user", f.member.ID,
		[]string{f.memberRD.ID}, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

func TestSetMemberRoles_AdminTier_CanChangeMemberToNoneKeepingCustom(t *testing.T) {
	f := setupMMRFixture(t)
	// f.member already holds project-member, so this is 200, not 201.
	require.Equal(t, http.StatusOK,
		putMemberRoles(t, f.srv, f.owner, f.projectID, "user", f.member.ID, []string{f.memberRD.ID, f.withinCeiling.ID}, nil).Code)

	rec := putMemberRoles(t, f.srv, f.admin, f.projectID, "user", f.member.ID,
		[]string{f.withinCeiling.ID}, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	after := mmrBindingsFor(t, f.store, "user", f.member.ID, f.projectID)
	require.Len(t, after, 1)
	assert.Equal(t, f.withinCeiling.ID, after[0].RoleDefinitionID)
}

func TestSetMemberRoles_AdminTier_CannotDeleteAllWhenHoldsCustom(t *testing.T) {
	f := setupMMRFixture(t)
	// f.member already holds project-member, so this is 200, not 201.
	require.Equal(t, http.StatusOK,
		putMemberRoles(t, f.srv, f.owner, f.projectID, "user", f.member.ID, []string{f.memberRD.ID, f.withinCeiling.ID}, nil).Code)

	rec := deleteMemberRoles(t, f.srv, f.admin, f.projectID, "user", f.member.ID)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Len(t, mmrBindingsFor(t, f.store, "user", f.member.ID, f.projectID), 2, "nothing removed")
}

func TestSetMemberRoles_AdminTier_CannotSetOwnerOrAdmin(t *testing.T) {
	f := setupMMRFixture(t)
	rec := putMemberRoles(t, f.srv, f.admin, f.projectID, "user", f.member.ID, []string{f.adminRD.ID}, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ErrCodeTargetRoleProtected)

	rec = putMemberRoles(t, f.srv, f.admin, f.projectID, "user", f.member.ID, []string{f.ownerRD.ID}, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

// TestSetMemberRoles_Escalation_AdminCannotGrantAnyCustomRoleEvenWithinCeiling
// is escalation test (iv): governance for custom roles is owner-only (or hub
// override); an admin is refused even for a role within their own ceiling.
func TestSetMemberRoles_Escalation_AdminCannotGrantAnyCustomRoleEvenWithinCeiling(t *testing.T) {
	f := setupMMRFixture(t)
	withinAdminCeiling, err := f.store.CreateRoleDefinition(context.Background(), &store.RoleDefinition{
		Name:        "mmr-admin-ceiling-" + tid(t.Name())[:8],
		ScopeType:   store.RoleScopeProject,
		Permissions: []string{"project.read"},
	})
	require.NoError(t, err)

	rec := putMemberRoles(t, f.srv, f.admin, f.projectID, "user", f.member.ID,
		[]string{f.memberRD.ID, withinAdminCeiling.ID}, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ErrCodeRoleAssignmentForbidden)
	assert.Contains(t, rec.Body.String(), "requiredPermission")
	// f.member already holds exactly one binding (project-member, from the
	// fixture); the denied PUT must not add the custom role to it.
	assert.Len(t, mmrBindingsFor(t, f.store, "user", f.member.ID, f.projectID), 1, "no bindings beyond the pre-existing member binding were created")
}

// ---------------------------------------------------------------------------
// Escalation (ii): owners cannot grant themselves role_binding authority
// through a custom role.
// ---------------------------------------------------------------------------

func TestSetMemberRoles_Escalation_OwnerCannotGrantRoleBindingPermission(t *testing.T) {
	f := setupMMRFixture(t)
	rec := putMemberRoles(t, f.srv, f.owner, f.projectID, "user", f.member.ID,
		[]string{f.memberRD.ID, f.roleBindingCustom.ID}, nil)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ErrCodeTargetRoleProtected, "CanDelegate refuses role_binding.* even for an owner")
}

// ---------------------------------------------------------------------------
// Escalation (iii): CanDelegate runs per created binding, not per request.
// ---------------------------------------------------------------------------

func TestSetMemberRoles_Escalation_CanDelegatePerBindingNotPerRequest(t *testing.T) {
	f := setupMMRFixture(t)
	target := tid(t.Name() + "-target")
	require.NoError(t, f.store.CreateUser(context.Background(), &store.User{
		ID: target, Email: target + "@test.com", DisplayName: "Target", Role: "member", Status: "active",
	}))

	rec := putMemberRoles(t, f.srv, f.owner, f.projectID, "user", target,
		[]string{f.memberRD.ID, f.withinCeiling.ID, f.beyondCeiling.ID}, nil)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), f.beyondCeiling.ID, "the denial names the beyond-ceiling role, not the within-ceiling one")
	assert.NotContains(t, rec.Body.String(), f.withinCeiling.ID)
	assert.Empty(t, mmrBindingsFor(t, f.store, "user", target, f.projectID), "nothing committed: within-ceiling customA was never persisted despite passing its own CanDelegate check")
}

// ---------------------------------------------------------------------------
// Escalation (v) / acceptance A2: a custom-only holder cannot bypass the
// built-in governance matrix, even if their custom role carries
// role_binding.create.
// ---------------------------------------------------------------------------

func TestSetMemberRoles_Escalation_CustomOnlyHolderCannotChangeBuiltIn(t *testing.T) {
	f := setupMMRFixture(t)
	ctx := context.Background()

	custodian := tid(t.Name() + "-custodian")
	require.NoError(t, f.store.CreateUser(ctx, &store.User{
		ID: custodian, Email: custodian + "@test.com", DisplayName: "Custodian", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, f.store, custodian)
	// The custodian's custom role carries project.manage (so the HTTP
	// project.manage gate is passed, reaching the service) plus
	// role_binding.create at PROJECT scope (the permission this guard test
	// is about). customRoleAuthorityFromStore and the built-in hub override
	// only ever look at SYSTEM-scope role_binding.*, so this project-scope
	// grant must not help the custodian change a built-in role.
	custodianRole, err := f.store.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        "mmr-custodian-" + tid(t.Name())[:8],
		ScopeType:   store.RoleScopeProject,
		Permissions: []string{"project.read", "project.manage", "role_binding.create"},
	})
	require.NoError(t, err)
	_, err = f.store.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: custodianRole.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      custodian,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          f.projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	custodianUser, err := f.store.GetUser(ctx, custodian)
	require.NoError(t, err)

	// Attempt a built-in change: the custodian tries to make the member an
	// admin. projectEffectiveRole for the custodian is "" (custom roles do
	// not count), and their project-scope role_binding.create does not
	// satisfy the SYSTEM-scope-only built-in hub override.
	rec := putMemberRoles(t, f.srv, custodianUser, f.projectID, "user", f.member.ID,
		[]string{f.adminRD.ID}, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ErrCodeRoleAssignmentForbidden)
}

// ---------------------------------------------------------------------------
// Last owner
// ---------------------------------------------------------------------------

func TestSetMemberRoles_LastOwner_WithTwoOwnersDemoteSucceeds(t *testing.T) {
	f := setupMMRFixture(t)
	coOwnerID := tid(t.Name() + "-co-owner")
	createRS1UserWithRole(t, f.store, coOwnerID, coOwnerID+"@test.com", f.projectID, store.ProjectRoleOwner)

	rec := putMemberRoles(t, f.srv, f.owner, f.projectID, "user", f.owner.ID, []string{f.memberRD.ID}, nil)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestSetMemberRoles_LastOwner_DeleteAllOfSoleOwner(t *testing.T) {
	f := setupMMRFixture(t)
	rec := deleteMemberRoles(t, f.srv, f.owner, f.projectID, "user", f.owner.ID)
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ErrCodeLastOwner)
}

func TestSetMemberRoles_LastOwner_RemovingExpiredOwnerAllowed(t *testing.T) {
	f := setupMMRFixture(t)
	ctx := context.Background()
	past, err := time.Parse(time.RFC3339, "2020-01-01T00:00:00Z")
	require.NoError(t, err)
	expiredOwnerID := tid(t.Name() + "-expired-owner")
	require.NoError(t, f.store.CreateUser(ctx, &store.User{
		ID: expiredOwnerID, Email: expiredOwnerID + "@test.com", DisplayName: "Expired Owner", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, f.store, expiredOwnerID)
	_, err = f.store.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: f.ownerRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      expiredOwnerID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          f.projectID,
		ExpiresAt:        &past,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// f.owner is still an active owner, so removing the already-expired
	// owner binding must be allowed.
	rec := deleteMemberRoles(t, f.srv, f.owner, f.projectID, "user", expiredOwnerID)
	assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
}

// ---------------------------------------------------------------------------
// Eligibility
// ---------------------------------------------------------------------------

func TestSetMemberRoles_Eligibility_OwnerForGroupRejected(t *testing.T) {
	f := setupMMRFixture(t)
	ctx := context.Background()
	groupID := tid(t.Name() + "-group")
	require.NoError(t, f.store.CreateGroup(ctx, &store.Group{ID: groupID, Name: "g", Slug: "mmr-elig-group"}))

	rec := putMemberRoles(t, f.srv, f.owner, f.projectID, "group", groupID, []string{f.ownerRD.ID}, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ErrCodePrincipalIneligible)
}

func TestSetMemberRoles_Eligibility_AdminForAgentRejected(t *testing.T) {
	f := setupMMRFixture(t)
	agentID := tid(t.Name() + "-agent")
	rec := putMemberRoles(t, f.srv, f.owner, f.projectID, "agent", agentID, []string{f.adminRD.ID}, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ErrCodePrincipalIneligible)
}

// TestSetMemberRoles_Eligibility_CustomForAgentRejected is the ported and
// re-targeted miller79/scion PR #127 "agent" scenario, adjusted per the
// 2026-10-01 D1 ruling: a custom-role CREATE for an agent principal is 400
// principal_ineligible (not 403), for every actor including hub override.
func TestSetMemberRoles_Eligibility_CustomForAgentRejected(t *testing.T) {
	f := setupMMRFixture(t)
	agentID := tid(t.Name() + "-agent")
	rec := putMemberRoles(t, f.srv, f.owner, f.projectID, "agent", agentID,
		[]string{f.memberRD.ID, f.withinCeiling.ID}, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ErrCodePrincipalIneligible)
}

// TestSetMemberRoles_Eligibility_KeepingCustomOnAgentAllowed is design-d3-
// addendum.md acceptance A3's second half: a PUT that merely KEEPS a custom
// role an agent already holds (seeded directly, bypassing eligibility) is
// accepted — only creating a new custom binding on an agent is blocked.
func TestSetMemberRoles_Eligibility_KeepingCustomOnAgentAllowed(t *testing.T) {
	f := setupMMRFixture(t)
	ctx := context.Background()
	agentID := tid(t.Name() + "-agent")
	require.NoError(t, f.store.CreateAgent(ctx, &store.Agent{
		ID: agentID, Slug: agentID, Name: "mmr-agent", ProjectID: f.projectID,
		Phase: "running", CreatedBy: f.owner.ID, OwnerID: f.owner.ID, Ancestry: []string{f.owner.ID},
	}))
	_, err := f.store.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: f.withinCeiling.ID,
		PrincipalType:    store.RoleBindingPrincipalAgent,
		PrincipalID:      agentID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          f.projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
	_, err = f.store.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: f.memberRD.ID,
		PrincipalType:    store.RoleBindingPrincipalAgent,
		PrincipalID:      agentID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          f.projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Keep both; nothing new is created.
	rec := putMemberRoles(t, f.srv, f.owner, f.projectID, "agent", agentID,
		[]string{f.memberRD.ID, f.withinCeiling.ID}, nil)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	bindings := mmrBindingsFor(t, f.store, "agent", agentID, f.projectID)
	assert.Len(t, bindings, 2)
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

func TestSetMemberRoles_Validation_EmptySet(t *testing.T) {
	f := setupMMRFixture(t)
	rec := putMemberRoles(t, f.srv, f.owner, f.projectID, "user", f.member.ID, []string{}, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ErrCodeEmptyRoleSet)
}

func TestSetMemberRoles_Validation_TwoBuiltIns(t *testing.T) {
	f := setupMMRFixture(t)
	rec := putMemberRoles(t, f.srv, f.owner, f.projectID, "user", f.member.ID, []string{f.adminRD.ID, f.memberRD.ID}, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ErrCodeInvalidRoleSet)
}

func TestSetMemberRoles_Validation_UnknownRoleID(t *testing.T) {
	f := setupMMRFixture(t)
	rec := putMemberRoles(t, f.srv, f.owner, f.projectID, "user", f.member.ID, []string{"not-a-real-role-id"}, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ErrCodeInvalidRoleSet)
}

// TestSetMemberRoles_Ported_SystemScopedRoleRejected is the ported and
// re-targeted "system scope role" scenario from miller79/scion PR #127.
func TestSetMemberRoles_Ported_SystemScopedRoleRejected(t *testing.T) {
	f := setupMMRFixture(t)
	hubMember, err := f.store.GetRoleDefinitionByName(context.Background(), store.SystemRoleHubMember, store.RoleScopeSystem)
	require.NoError(t, err)

	rec := putMemberRoles(t, f.srv, f.owner, f.projectID, "user", f.member.ID, []string{hubMember.ID}, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ErrCodeInvalidRoleSet)
}

// ---------------------------------------------------------------------------
// Credential gate
// ---------------------------------------------------------------------------

func TestSetMemberRoles_CredentialGate_RejectsUAT(t *testing.T) {
	f := setupMMRFixture(t)
	uatKey := mintScopedUAT(t, f.srv, f.owner.ID, f.projectID, []string{"project:manage"})

	body := map[string]interface{}{"roleDefinitionIds": []string{f.memberRD.ID}}
	rec := doRequestWithUAT(t, f.srv, uatKey, http.MethodPut, mmrPrincipalPath(f.projectID, "user", f.member.ID), body)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ErrCodeMembershipCredentialInsufficient)
}

// ---------------------------------------------------------------------------
// Idempotency
// ---------------------------------------------------------------------------

func TestSetMemberRoles_Idempotent_SameSetTwice(t *testing.T) {
	f := setupMMRFixture(t)
	roles := []string{f.memberRD.ID, f.withinCeiling.ID}
	rec := putMemberRoles(t, f.srv, f.owner, f.projectID, "user", f.member.ID, roles, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	beforeAudit := len(mmrAuditRows(t, f.store, f.projectID))

	rec = putMemberRoles(t, f.srv, f.owner, f.projectID, "user", f.member.ID, roles, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"changed":false`)
	assert.Len(t, mmrAuditRows(t, f.store, f.projectID), beforeAudit, "no new audit rows for a no-op re-PUT")
}

// ---------------------------------------------------------------------------
// Preconditions
// ---------------------------------------------------------------------------

func TestSetMemberRoles_Precondition_Mismatch(t *testing.T) {
	f := setupMMRFixture(t)
	wrongExpected := []string{f.adminRD.ID}
	rec := putMemberRoles(t, f.srv, f.owner, f.projectID, "user", f.member.ID, []string{f.withinCeiling.ID}, &wrongExpected)
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ErrCodeMembershipChanged)
	assert.Contains(t, rec.Body.String(), "currentRoleDefinitionIds")
}

func TestSetMemberRoles_Precondition_ExpectEmptyAgainstExistingMember(t *testing.T) {
	f := setupMMRFixture(t)
	empty := []string{}
	rec := putMemberRoles(t, f.srv, f.owner, f.projectID, "user", f.member.ID, []string{f.adminRD.ID}, &empty)
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ErrCodeMembershipChanged)
}

// ---------------------------------------------------------------------------
// Hub override
// ---------------------------------------------------------------------------

func mmrSeedHubAdmin(t *testing.T, s store.Store, userID string) {
	t.Helper()
	ctx := context.Background()
	hubAdmin, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: hubAdmin.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
}

// mmrServiceCtx builds a context carrying an interactive identity for a
// DIRECT ProjectMembershipService.SetMemberRoles call. The hub-override
// tests below call the service directly rather than through PUT, because the
// HTTP entry gate on this endpoint is project.manage (design.md §3.1) and
// hub-admin does NOT hold project.manage (seed.go hubAdminPermissionIDs) —
// exactly like the existing AddMember/RemoveMember hub-override logic, whose
// own tests (rs5_global_admin_governance_test.go, rs5_r2_hardening_test.go)
// also call the service directly rather than through the project.manage-
// gated /members HTTP endpoints. The hub override is reached in production
// through /api/v1/admin/role-bindings, which has no project.manage gate.
func mmrServiceCtx(userID, email string) context.Context {
	identity := NewAuthenticatedUser(userID, email, "Test User", "member", string(ClientTypeAPI))
	ctx := contextWithIdentity(context.Background(), identity)
	return contextWithCredentialContext(ctx, CredentialContext{Kind: CredentialKindInteractive, ID: "test-session"})
}

func mmrServiceIdentity(userID, email string) UserIdentity {
	return NewAuthenticatedUser(userID, email, "Test User", "member", string(ClientTypeAPI))
}

func TestSetMemberRoles_HubOverride_WithinCeilingAllowed(t *testing.T) {
	f := setupMMRFixture(t)
	ctx := context.Background()
	hubAdminID := tid(t.Name() + "-hubadmin")
	require.NoError(t, f.store.CreateUser(ctx, &store.User{
		ID: hubAdminID, Email: hubAdminID + "@test.com", DisplayName: "Hub Admin", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, f.store, hubAdminID)
	mmrSeedHubAdmin(t, f.store, hubAdminID)

	// hub-admin's ceiling is its OWN (system-scope) permission set, which
	// holds project.read but none of the project-member permissions
	// (agent.create, agent.list, ...) that f.withinCeiling carries. A role
	// within a hub admin's own ceiling must ask for no more than that.
	withinHubAdminCeiling, err := f.store.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        "mmr-within-hubadmin-" + tid(t.Name())[:8],
		ScopeType:   store.RoleScopeProject,
		Permissions: []string{"project.read"},
	})
	require.NoError(t, err)

	svcCtx := mmrServiceCtx(hubAdminID, hubAdminID+"@test.com")
	_, decision := f.srv.membershipService.SetMemberRoles(svcCtx, SetMemberRolesRequest{
		ProjectID: f.projectID, PrincipalType: "user", PrincipalID: f.member.ID,
		Actor:          mmrServiceIdentity(hubAdminID, hubAdminID+"@test.com"),
		DesiredRoleIDs: []string{f.memberRD.ID, withinHubAdminCeiling.ID},
	})
	require.Nil(t, decision, "hub override should allow a within-(hub-admin)-ceiling custom grant: %+v", decision)
}

// TestSetMemberRoles_Ported_HubAdminCustomProjectRole_UnchangedCeilingBehavior
// is the ported and re-targeted "hub-admin ceiling unchanged" scenario: a hub
// admin with no project role of their own on f.otherProjectID still passes
// the entry gate (hub override) but is refused by CanDelegate because
// hub-admin is a system role carrying no project permissions.
func TestSetMemberRoles_Ported_HubAdminCustomProjectRole_UnchangedCeilingBehavior(t *testing.T) {
	f := setupMMRFixture(t)
	ctx := context.Background()
	hubAdminID := tid(t.Name() + "-hubadmin")
	require.NoError(t, f.store.CreateUser(ctx, &store.User{
		ID: hubAdminID, Email: hubAdminID + "@test.com", DisplayName: "Hub Admin", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, f.store, hubAdminID)
	mmrSeedHubAdmin(t, f.store, hubAdminID)

	target := tid(t.Name() + "-target")
	require.NoError(t, f.store.CreateUser(ctx, &store.User{
		ID: target, Email: target + "@test.com", DisplayName: "Target", Role: "member", Status: "active",
	}))

	svcCtx := mmrServiceCtx(hubAdminID, hubAdminID+"@test.com")
	_, decision := f.srv.membershipService.SetMemberRoles(svcCtx, SetMemberRolesRequest{
		ProjectID: f.otherProjectID, PrincipalType: "user", PrincipalID: target,
		Actor:          mmrServiceIdentity(hubAdminID, hubAdminID+"@test.com"),
		DesiredRoleIDs: []string{f.withinCeiling.ID},
	})
	require.NotNil(t, decision)
	assert.Equal(t, ErrCodeTargetRoleProtected, decision.DenialCode, "a hub admin should reach the delegation ceiling, not the entry gate: %+v", decision)
}

func TestSetMemberRoles_HubOverride_DemotionOwnerToMemberAllowed(t *testing.T) {
	f := setupMMRFixture(t)
	ctx := context.Background()
	hubAdminID := tid(t.Name() + "-hubadmin")
	require.NoError(t, f.store.CreateUser(ctx, &store.User{
		ID: hubAdminID, Email: hubAdminID + "@test.com", DisplayName: "Hub Admin", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, f.store, hubAdminID)
	mmrSeedHubAdmin(t, f.store, hubAdminID)

	// Two owners so demoting one does not trip the last-owner guard.
	coOwnerID := tid(t.Name() + "-co-owner")
	createRS1UserWithRole(t, f.store, coOwnerID, coOwnerID+"@test.com", f.projectID, store.ProjectRoleOwner)

	svcCtx := mmrServiceCtx(hubAdminID, hubAdminID+"@test.com")
	_, decision := f.srv.membershipService.SetMemberRoles(svcCtx, SetMemberRolesRequest{
		ProjectID: f.projectID, PrincipalType: "user", PrincipalID: f.owner.ID,
		Actor:          mmrServiceIdentity(hubAdminID, hubAdminID+"@test.com"),
		DesiredRoleIDs: []string{f.memberRD.ID},
	})
	assert.Nil(t, decision, "no CanDelegate check on a decrease: %+v", decision)
}

// ---------------------------------------------------------------------------
// Principal addressing
// ---------------------------------------------------------------------------

func TestSetMemberRoles_Addressing_ByEmailAndSlug(t *testing.T) {
	f := setupMMRFixture(t)
	ctx := context.Background()
	target := tid(t.Name() + "-target")
	require.NoError(t, f.store.CreateUser(ctx, &store.User{
		ID: target, Email: "mmr-addr-target@test.com", DisplayName: "Target", Role: "member", Status: "active",
	}))

	rec := putMemberRoles(t, f.srv, f.owner, f.projectID, "user", "mmr-addr-target@test.com", []string{f.memberRD.ID}, nil)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Len(t, mmrBindingsFor(t, f.store, "user", target, f.projectID), 1)

	groupID := tid(t.Name() + "-group")
	require.NoError(t, f.store.CreateGroup(ctx, &store.Group{ID: groupID, Name: "g", Slug: "mmr-addr-group"}))
	rec = putMemberRoles(t, f.srv, f.owner, f.projectID, "group", "mmr-addr-group", []string{f.adminRD.ID}, nil)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Len(t, mmrBindingsFor(t, f.store, "group", groupID, f.projectID), 1)
}

func TestSetMemberRoles_Addressing_DeleteOnPrincipalWithNoBindings404(t *testing.T) {
	f := setupMMRFixture(t)
	nobody := tid(t.Name() + "-nobody")
	require.NoError(t, f.store.CreateUser(context.Background(), &store.User{
		ID: nobody, Email: nobody + "@test.com", DisplayName: "Nobody", Role: "member", Status: "active",
	}))
	rec := deleteMemberRoles(t, f.srv, f.owner, f.projectID, "user", nobody)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
}

// ---------------------------------------------------------------------------
// Concurrency
// ---------------------------------------------------------------------------

func TestSetMemberRoles_Concurrency_ConflictingPUTs(t *testing.T) {
	f := setupMMRFixture(t)

	var wg sync.WaitGroup
	codes := make([]int, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		rec := putMemberRoles(t, f.srv, f.owner, f.projectID, "user", f.member.ID, []string{f.adminRD.ID}, nil)
		codes[0] = rec.Code
	}()
	go func() {
		defer wg.Done()
		rec := putMemberRoles(t, f.srv, f.owner, f.projectID, "user", f.member.ID, []string{f.withinCeiling.ID, f.memberRD.ID}, nil)
		codes[1] = rec.Code
	}()
	wg.Wait()

	for _, code := range codes {
		assert.True(t, code == http.StatusOK || code == http.StatusConflict,
			"expected 200 (serialized winner) or 409 membership_changed, got %d", code)
	}

	// Whatever the final state, the D4 invariant (at most one built-in
	// binding per principal per project) must hold.
	bindings := mmrBindingsFor(t, f.store, "user", f.member.ID, f.projectID)
	builtInCount := 0
	for _, b := range bindings {
		def, derr := f.store.GetRoleDefinition(context.Background(), b.RoleDefinitionID)
		require.NoError(t, derr)
		if store.IsBuiltInProjectMembershipRole(def.Name) {
			builtInCount++
		}
	}
	assert.LessOrEqual(t, builtInCount, 1, "D4: at most one built-in binding per principal per project")
}

// ---------------------------------------------------------------------------
// Ported miller79/scion PR #127 scenarios (re-targeted to the PUT endpoint)
// ---------------------------------------------------------------------------

func TestSetMemberRoles_Ported_OwnerAssignsCustomProjectRole_WithinCeiling(t *testing.T) {
	f := setupMMRFixture(t)
	rec := putMemberRoles(t, f.srv, f.owner, f.projectID, "user", f.member.ID, []string{f.memberRD.ID, f.withinCeiling.ID}, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	bindings := mmrBindingsFor(t, f.store, "user", f.member.ID, f.projectID)
	found := false
	for _, b := range bindings {
		if b.RoleDefinitionID == f.withinCeiling.ID {
			found = true
		}
	}
	assert.True(t, found, "custom binding should exist on the owner's project")
}

func TestSetMemberRoles_Ported_OwnerCannotAssignCustomProjectRole_BeyondCeiling(t *testing.T) {
	f := setupMMRFixture(t)
	rec := putMemberRoles(t, f.srv, f.owner, f.projectID, "user", f.member.ID, []string{f.memberRD.ID, f.beyondCeiling.ID}, nil)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ErrCodeTargetRoleProtected)
}

func TestSetMemberRoles_Ported_OwnerCannotAssignCustomProjectRole_OnAnotherProject(t *testing.T) {
	f := setupMMRFixture(t)
	target := tid(t.Name() + "-target")
	require.NoError(t, f.store.CreateUser(context.Background(), &store.User{
		ID: target, Email: target + "@test.com", DisplayName: "Target", Role: "member", Status: "active",
	}))
	rec := putMemberRoles(t, f.srv, f.owner, f.otherProjectID, "user", target, []string{f.memberRD.ID, f.withinCeiling.ID}, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

func TestSetMemberRoles_Ported_MemberCannotAssignCustomProjectRole(t *testing.T) {
	f := setupMMRFixture(t)
	rec := putMemberRoles(t, f.srv, f.member, f.projectID, "user", f.member.ID, []string{f.memberRD.ID, f.withinCeiling.ID}, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

func TestSetMemberRoles_Ported_OwnerRemovesAssignedCustomProjectRole(t *testing.T) {
	f := setupMMRFixture(t)
	rec := putMemberRoles(t, f.srv, f.owner, f.projectID, "user", f.member.ID, []string{f.memberRD.ID, f.withinCeiling.ID}, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = putMemberRoles(t, f.srv, f.owner, f.projectID, "user", f.member.ID, []string{f.memberRD.ID}, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	bindings := mmrBindingsFor(t, f.store, "user", f.member.ID, f.projectID)
	for _, b := range bindings {
		assert.NotEqual(t, f.withinCeiling.ID, b.RoleDefinitionID, "custom binding should be gone after removal")
	}
}

// ---------------------------------------------------------------------------
// Regression: rs1_*/rs2_*/rs3_*/d002_*/pm1_* stay green, unmodified. Run with:
//
//	go test ./pkg/hub/ -run 'RS|D002|PM1|ProjectMember'
// ---------------------------------------------------------------------------
