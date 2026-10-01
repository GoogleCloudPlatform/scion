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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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

// mmrEnableRequestLogging installs a request logger on srv so
// RequestLogMiddleware populates *logging.RequestMeta (and therefore a real,
// non-empty RequestID) on every request's context — which is what
// logging.RequestIDFromContext / createAuditRecord read into
// MutationAuditRecord.CorrelationID. testServer does not set a request
// logger by default (review r1 F4: without one, CorrelationID is always ""
// in tests, making cross-row correlation assertions vacuous). Discards
// output; only the side effect of installing RequestMeta is wanted here.
func mmrEnableRequestLogging(t *testing.T, srv *Server) {
	t.Helper()
	srv.SetRequestLogger(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	t.Cleanup(func() { srv.SetRequestLogger(nil) })
}

// ---------------------------------------------------------------------------
// Core atomic set/remove-all behavior
// ---------------------------------------------------------------------------

func TestSetMemberRoles_PutAddsBuiltInAndCustomAtomically(t *testing.T) {
	f := setupMMRFixture(t)
	mmrEnableRequestLogging(t, f.srv) // F4: so CorrelationID is a real, non-empty, shared request ID.
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
	require.NotEmpty(t, addRows[0].CorrelationID, "F4: a real request logger must produce a non-empty CorrelationID")
	assert.Equal(t, addRows[0].CorrelationID, addRows[1].CorrelationID, "both rows share one CorrelationID")
	assert.Contains(t, addRows[0].AfterSummary+addRows[1].AfterSummary, `"roleKind":"builtin"`)
	assert.Contains(t, addRows[0].AfterSummary+addRows[1].AfterSummary, `"roleKind":"custom"`)

	// F4: the custom-grant row records the CanDelegate result and the
	// authority (Via) it was granted through (design.md §3.4, addendum §4
	// item 5) — not just roleKind.
	var customRow *store.MutationAuditRecord
	for _, r := range addRows {
		if strings.Contains(r.AfterSummary, `"roleKind":"custom"`) {
			customRow = r
		}
	}
	require.NotNil(t, customRow)
	assert.Equal(t, "allowed", customRow.CanDelegateResult)
	assert.Contains(t, customRow.AfterSummary, `"authority":"project_owner"`)
}

// TestSetMemberRoles_Audit_RemoveRecordsRoleKindAndAuthority is F4 (review
// r1): a project_member_remove row for a custom binding must carry
// roleKind:"custom" and the authority (Via) under which the actor removed
// it, not just the bare role name.
func TestSetMemberRoles_Audit_RemoveRecordsRoleKindAndAuthority(t *testing.T) {
	f := setupMMRFixture(t)
	mmrEnableRequestLogging(t, f.srv)

	require.Equal(t, http.StatusOK,
		putMemberRoles(t, f.srv, f.owner, f.projectID, "user", f.member.ID,
			[]string{f.memberRD.ID, f.withinCeiling.ID}, nil).Code)
	beforeAudit := len(mmrAuditRows(t, f.store, f.projectID))

	rec := putMemberRoles(t, f.srv, f.owner, f.projectID, "user", f.member.ID,
		[]string{f.memberRD.ID}, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rows := mmrAuditRows(t, f.store, f.projectID)
	require.Greater(t, len(rows), beforeAudit)
	var removeRow *store.MutationAuditRecord
	for _, r := range rows {
		if r.MutationType == "project_member_remove" && strings.Contains(r.BeforeSummary, `"roleKind":"custom"`) {
			removeRow = r
		}
	}
	require.NotNil(t, removeRow, "expected a project_member_remove row for the removed custom binding")
	assert.NotEmpty(t, removeRow.CorrelationID)
	assert.Contains(t, removeRow.BeforeSummary, `"authority":"project_owner"`)
}

// TestSetMemberRoles_Audit_DeleteAllRecordsRoleKind is F4 (review r1):
// DELETE-all writes one project_member_remove row per binding, each with the
// correct roleKind (builtin vs custom), sharing one CorrelationID.
func TestSetMemberRoles_Audit_DeleteAllRecordsRoleKind(t *testing.T) {
	f := setupMMRFixture(t)
	mmrEnableRequestLogging(t, f.srv)

	require.Equal(t, http.StatusOK,
		putMemberRoles(t, f.srv, f.owner, f.projectID, "user", f.member.ID,
			[]string{f.adminRD.ID, f.withinCeiling.ID}, nil).Code)
	beforeAudit := len(mmrAuditRows(t, f.store, f.projectID))

	rec := deleteMemberRoles(t, f.srv, f.owner, f.projectID, "user", f.member.ID)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	rows := mmrAuditRows(t, f.store, f.projectID)
	var removeRows []*store.MutationAuditRecord
	for _, r := range rows[beforeAudit:] {
		if r.MutationType == "project_member_remove" {
			removeRows = append(removeRows, r)
		}
	}
	require.Len(t, removeRows, 2, "one row per removed binding (builtin admin + custom)")

	var sawBuiltin, sawCustom bool
	for _, r := range removeRows {
		assert.NotEmpty(t, r.CorrelationID)
		assert.Equal(t, removeRows[0].CorrelationID, r.CorrelationID, "all DELETE-all rows share one CorrelationID")
		switch {
		case strings.Contains(r.BeforeSummary, `"roleKind":"builtin"`):
			sawBuiltin = true
		case strings.Contains(r.BeforeSummary, `"roleKind":"custom"`):
			sawCustom = true
			assert.Contains(t, r.BeforeSummary, `"authority":"project_owner"`)
		}
	}
	assert.True(t, sawBuiltin, "expected a roleKind:builtin remove row")
	assert.True(t, sawCustom, "expected a roleKind:custom remove row")
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
			// N5 (review r1): the role_change row carries roleKind (and
			// principalType) on both sides, matching every other mutation
			// type's contract, even though a built-in swap is always
			// roleKind:"builtin" by construction.
			assert.Contains(t, r.BeforeSummary, `"roleKind":"builtin"`)
			assert.Contains(t, r.AfterSummary, `"roleKind":"builtin"`)
			assert.Contains(t, r.BeforeSummary, `"principalType":"user"`)
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

// TestSetMemberRoles_Escalation_AdminWithHubRoleBindingStillRefused is F2
// (review r1): customRoleAuthorityFromStore's hub role_binding.* fallback
// applies only when the actor has NO project role of their own — a
// project-admin who ALSO holds the hub role_binding.* override is refused
// exactly like a project-admin without it (test (iv) above), because the
// admin's own project-admin role means the fallback never triggers.
func TestSetMemberRoles_Escalation_AdminWithHubRoleBindingStillRefused(t *testing.T) {
	f := setupMMRFixture(t)
	mmrSeedHubAdmin(t, f.store, f.admin.ID)

	rec := putMemberRoles(t, f.srv, f.admin, f.projectID, "user", f.member.ID,
		[]string{f.memberRD.ID, f.withinCeiling.ID}, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ErrCodeRoleAssignmentForbidden, "holding hub role_binding.* must not grant custom-role authority to an actor who already has a project role")
	assert.Len(t, mmrBindingsFor(t, f.store, "user", f.member.ID, f.projectID), 1, "no bindings beyond the pre-existing member binding were created")
}

// ---------------------------------------------------------------------------
// Escalation (ii) / F1: no custom role containing role_binding.* can be
// granted through this endpoint by ANY actor — not just an owner. This is a
// structural refusal (checkNoRoleBindingPermissionInCreatedCustomRoles),
// independent of CanDelegate and of customRoleAuthorityFromStore, so it
// fires even for the hub role_binding.* override actor, who otherwise has
// full custom-role grant authority and whom CanDelegate cannot refuse (the
// override's own ceiling already includes role_binding.create/delete).
// ---------------------------------------------------------------------------

func TestSetMemberRoles_Escalation_RoleBindingPermissionRefusedForOwner(t *testing.T) {
	f := setupMMRFixture(t)
	rec := putMemberRoles(t, f.srv, f.owner, f.projectID, "user", f.member.ID,
		[]string{f.memberRD.ID, f.roleBindingCustom.ID}, nil)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ErrCodeRoleAssignmentForbidden, "structural guard (F1), not a CanDelegate ceiling check")
	assert.Contains(t, rec.Body.String(), f.roleBindingCustom.ID, "details.roleDefinitionId names the offending role")
	assert.Len(t, mmrBindingsFor(t, f.store, "user", f.member.ID, f.projectID), 1, "only the pre-existing member binding remains")
}

// TestSetMemberRoles_Escalation_RoleBindingPermissionRefusedForHubOverride is
// the hub-override half of escalation test (ii) (F1, review r1): a hub-admin
// actor who holds no project role of their own reaches
// customRoleAuthorityFromStore's hub role_binding.* override for ordinary
// custom grants (TestSetMemberRoles_HubOverride_WithinCeilingAllowed proves
// that), but this structural guard refuses the role_binding.*-bearing custom
// role anyway, with zero writes.
func TestSetMemberRoles_Escalation_RoleBindingPermissionRefusedForHubOverride(t *testing.T) {
	f := setupMMRFixture(t)
	ctx := context.Background()
	hubAdminID := tid(t.Name() + "-hubadmin")
	require.NoError(t, f.store.CreateUser(ctx, &store.User{
		ID: hubAdminID, Email: hubAdminID + "@test.com", DisplayName: "Hub Admin", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, f.store, hubAdminID)
	mmrSeedHubAdmin(t, f.store, hubAdminID)

	beforeAudit := len(mmrAuditRows(t, f.store, f.projectID))
	beforeBindings := mmrBindingsFor(t, f.store, "user", f.member.ID, f.projectID)

	svcCtx := mmrServiceCtx(hubAdminID, hubAdminID+"@test.com")
	_, decision := f.srv.membershipService.SetMemberRoles(svcCtx, SetMemberRolesRequest{
		ProjectID: f.projectID, PrincipalType: "user", PrincipalID: f.member.ID,
		Actor:          mmrServiceIdentity(hubAdminID, hubAdminID+"@test.com"),
		DesiredRoleIDs: []string{f.memberRD.ID, f.roleBindingCustom.ID},
	})
	require.NotNil(t, decision, "the hub role_binding.* override must not bypass the structural guard")
	assert.Equal(t, ErrCodeRoleAssignmentForbidden, decision.DenialCode, "%+v", decision)
	assert.Equal(t, f.roleBindingCustom.ID, decision.Details["roleDefinitionId"])

	assert.Len(t, mmrAuditRows(t, f.store, f.projectID), beforeAudit, "zero writes")
	assert.Len(t, mmrBindingsFor(t, f.store, "user", f.member.ID, f.projectID), len(beforeBindings), "nothing added or removed")
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
//
// L1 (review r1): this is a forward guard. Today, custom authority is ALSO
// system-scope-only (customRoleAuthorityFromStore / F2), so this test cannot
// yet distinguish "the built-in matrix bypass is system-scope-only" from "no
// bypass exists at all" — it will start doing real work once a later
// authority model (design-d3-addendum.md §3 option (b)) gives custom roles a
// project-scope path. It covers both halves of A2 ("set or remove"): setting
// a built-in role, and removing one via DELETE-all.
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
	assert.Contains(t, rec.Body.String(), "actor has no project role", "proves the request reached SetMemberRoles' no-project-role branch (pre-tx), not the HTTP gate")

	// L1: the A2 guard also covers REMOVING a built-in role, not just setting
	// one. f.member holds only the built-in project-member binding, so
	// DELETE-all here is a built-in-role removal.
	rec = deleteMemberRoles(t, f.srv, custodianUser, f.projectID, "user", f.member.ID)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ErrCodeRoleAssignmentForbidden)
	assert.Contains(t, rec.Body.String(), "actor has no project role")
	assert.Len(t, mmrBindingsFor(t, f.store, "user", f.member.ID, f.projectID), 1, "the member binding must survive the denied DELETE-all")
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

// TestSetMemberRoles_Eligibility_CustomForAgentRejected_HubOverride is L2
// (review r1): D1 (new custom roles blocked for agent principals) applies to
// EVERY actor, including the hub role_binding.* override — not just the
// owner the sibling test above exercises. principalEligibleForRole runs
// before any actor-authority check (design-d3-addendum.md D1 / §4 item 6),
// so the hub-override actor gets the same 400 principal_ineligible, never a
// 403.
func TestSetMemberRoles_Eligibility_CustomForAgentRejected_HubOverride(t *testing.T) {
	f := setupMMRFixture(t)
	ctx := context.Background()
	hubAdminID := tid(t.Name() + "-hubadmin")
	require.NoError(t, f.store.CreateUser(ctx, &store.User{
		ID: hubAdminID, Email: hubAdminID + "@test.com", DisplayName: "Hub Admin", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, f.store, hubAdminID)
	mmrSeedHubAdmin(t, f.store, hubAdminID)

	agentID := tid(t.Name() + "-agent")
	svcCtx := mmrServiceCtx(hubAdminID, hubAdminID+"@test.com")
	_, decision := f.srv.membershipService.SetMemberRoles(svcCtx, SetMemberRolesRequest{
		ProjectID: f.projectID, PrincipalType: "agent", PrincipalID: agentID,
		Actor:          mmrServiceIdentity(hubAdminID, hubAdminID+"@test.com"),
		DesiredRoleIDs: []string{f.memberRD.ID, f.withinCeiling.ID},
	})
	require.NotNil(t, decision)
	assert.Equal(t, ErrCodePrincipalIneligible, decision.DenialCode, "%+v", decision)
	assert.Equal(t, http.StatusBadRequest, decision.HTTPStatus)
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

// TestSetMemberRoles_CredentialGate_RejectsAgentToken is L3 (review r1):
// design.md §12 P1 / acceptance 7 list "UAT or agent token -> 403
// credential_insufficient" for this endpoint, but only the UAT half was
// tested. A real agent JWT authenticates to an AgentIdentity, which is not a
// UserIdentity; the handler now checks that before calling s.authorize (no
// permission in the registry maps project.manage to any agent scope, so an
// agent could never pass that check anyway) and returns the same
// credential_insufficient code a UAT gets, not a generic authorization
// denial.
func TestSetMemberRoles_CredentialGate_RejectsAgentToken(t *testing.T) {
	f := setupMMRFixture(t)
	ctx := context.Background()
	agentID := tid(t.Name() + "-agent")
	require.NoError(t, f.store.CreateAgent(ctx, &store.Agent{
		ID: agentID, Slug: agentID, Name: "mmr-agent", ProjectID: f.projectID,
		Phase: "running", CreatedBy: f.owner.ID, OwnerID: f.owner.ID, Ancestry: []string{f.owner.ID},
	}))
	agentToken, err := f.srv.GenerateAgentToken(agentID, f.projectID, []string{f.owner.ID}, AgentRoleFull, nil)
	require.NoError(t, err)

	bodyBytes, err := json.Marshal(map[string]interface{}{"roleDefinitionIds": []string{f.memberRD.ID}})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPut, mmrPrincipalPath(f.projectID, "user", f.member.ID), bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+agentToken)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ErrCodeMembershipCredentialInsufficient, "an agent token must be refused with the same code as a UAT")
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
// gated /members HTTP endpoints. In production, /api/v1/admin/role-bindings
// has no project.manage gate, so it is one way to reach the hub override —
// but not the only way: an actor with no built-in project role who passes
// this endpoint's own project.manage gate via a custom role carrying
// project.manage, and who also holds system role_binding.*, reaches the hub
// override over this endpoint too (review r2 R2-5).
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
// R2-2 (review r2): TOCTOU on the actor's authority SOURCE between phases
// ---------------------------------------------------------------------------

// mmrAuthoritySwapStore wraps store.Store so a test can simulate a
// concurrent request landing between SetMemberRoles' pre-transaction phase
// (Phase P: reads, CanDelegate) and its locked re-read (Phase T, inside
// WithTx). It is swapped in for ProjectMembershipService.store for the
// duration of one SetMemberRoles call; its WithTx override runs swap()
// exactly once, immediately before delegating to the real WithTx — which is
// exactly the seam between Phase P (already complete by the time
// SetMemberRoles calls svc.store.WithTx) and Phase T (whose first statement
// is the lock). swap mutates the underlying store directly, outside of any
// transaction, modelling an already-committed concurrent write.
type mmrAuthoritySwapStore struct {
	store.Store
	swap    func()
	didSwap bool
}

func (s *mmrAuthoritySwapStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	if !s.didSwap {
		s.didSwap = true
		s.swap()
	}
	return s.Store.WithTx(ctx, fn)
}

// TestSetMemberRoles_Escalation_TOCTOU_AuthoritySourceChangeRefused is R2-2
// (review r2). The actor is a direct owner who ALSO holds hub
// role_binding.* (hub-admin). Phase P's CanDelegate passes f.withinCeiling
// against the OWNER ceiling (f.withinCeiling is deliberately "within the
// owner's ceiling", per its fixture comment). Before the lock lands, a
// concurrent request (mmrAuthoritySwapStore) removes the actor's own owner
// binding. Without the R2-2 guard, Phase T's reevaluateActorTx would now
// report hubOverride=true and customRoleAuthorityFromStore(tx) would report
// Via:"hub_role_binding" — both pass governance on their own — and the
// grant would commit even though CanDelegate was never evaluated against a
// hub-admin-only ceiling, which TestSetMemberRoles_Ported_
// HubAdminCustomProjectRole_UnchangedCeilingBehavior shows refuses this
// exact role. The R2-2 guard detects the actorRole/hubOverride/customAuth-Via
// mismatch between phases and refuses with 409 membership_changed instead
// of committing.
func TestSetMemberRoles_Escalation_TOCTOU_AuthoritySourceChangeRefused(t *testing.T) {
	f := setupMMRFixture(t)
	ctx := context.Background()
	mmrSeedHubAdmin(t, f.store, f.owner.ID)

	realStore := f.srv.membershipService.store
	sw := &mmrAuthoritySwapStore{Store: realStore}
	sw.swap = func() {
		for _, b := range mmrBindingsFor(t, realStore, "user", f.owner.ID, f.projectID) {
			require.NoError(t, realStore.DeleteRoleBinding(ctx, b.ID))
		}
	}
	f.srv.membershipService.store = sw
	defer func() { f.srv.membershipService.store = realStore }()

	svcCtx := mmrServiceCtx(f.owner.ID, f.owner.Email)
	_, decision := f.srv.membershipService.SetMemberRoles(svcCtx, SetMemberRolesRequest{
		ProjectID: f.projectID, PrincipalType: "user", PrincipalID: f.member.ID,
		Actor:          mmrServiceIdentity(f.owner.ID, f.owner.Email),
		DesiredRoleIDs: []string{f.memberRD.ID, f.withinCeiling.ID},
	})

	require.NotNil(t, decision, "the grant must not silently commit under a changed authority source")
	assert.Equal(t, ErrCodeMembershipChanged, decision.DenialCode, "%+v", decision)
	assert.Equal(t, http.StatusConflict, decision.HTTPStatus)

	for _, b := range mmrBindingsFor(t, realStore, "user", f.member.ID, f.projectID) {
		assert.NotEqual(t, f.withinCeiling.ID, b.RoleDefinitionID, "the custom grant must not have committed")
	}
	assert.Empty(t, mmrAuditRows(t, realStore, f.projectID), "a refused request must write no audit rows")
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

// TestSetMemberRoles_Addressing_PercentEncodedIDNotDoubleDecoded is L6
// (review r1): r.URL.Path is already percent-decoded once by net/http
// before handleProjectRoutes ever sees it, so a second url.PathUnescape in
// the principals/ routing branch double-decoded the ID. A principal ID that
// itself contains a literal "%" (sent double-percent-encoded on the wire, as
// a real client must) was silently corrupted into a different string.
func TestSetMemberRoles_Addressing_PercentEncodedIDNotDoubleDecoded(t *testing.T) {
	f := setupMMRFixture(t)
	targetID := tid(t.Name() + "-target")
	// An email containing a literal "%40" substring (not meant to represent
	// "@" — the user's actual "@" is later in the string). A correct client
	// escapes the literal "%" as "%25" on the wire; double-decoding would
	// turn this "%40" into "@", producing a different (and non-existent)
	// email with two "@" signs.
	email := "a%40b-" + tid(t.Name())[:8] + "@test.com"
	require.NoError(t, f.store.CreateUser(context.Background(), &store.User{
		ID: targetID, Email: email, DisplayName: "Target", Role: "member", Status: "active",
	}))

	wireSegment := strings.ReplaceAll(email, "%", "%25")
	path := fmt.Sprintf("/api/v1/projects/%s/members/principals/user/%s", f.projectID, wireSegment)
	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPut, path,
		map[string]interface{}{"roleDefinitionIds": []string{f.memberRD.ID}})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Len(t, mmrBindingsFor(t, f.store, "user", targetID, f.projectID), 1, "the literal percent-containing email must resolve correctly, not a double-decoded variant")
}

// TestSetMemberRoles_Addressing_SlashInPrincipalIDRejected is L6 (review
// r1): SplitN(principalPath, "/", 2) lets "principals/user/a/b" through as a
// two-segment ID "a/b" unless the routing layer explicitly rejects an ID
// containing "/".
func TestSetMemberRoles_Addressing_SlashInPrincipalIDRejected(t *testing.T) {
	f := setupMMRFixture(t)
	path := fmt.Sprintf("/api/v1/projects/%s/members/principals/user/a/b", f.projectID)
	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPut, path,
		map[string]interface{}{"roleDefinitionIds": []string{f.memberRD.ID}})
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
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

// mmrConcurrentPut issues one PUT …/members/principals/{type}/{id} and
// returns its status code and any setup error, instead of calling require
// internally (unlike putMemberRoles/doRequestAsUser). N4 (review r1):
// require's t.FailNow is unsafe when called from a goroutine other than the
// one running the test function, so concurrency tests must collect results
// and assert on them back on the test goroutine after wg.Wait().
func mmrConcurrentPut(srv *Server, actor *store.User, projectID, principalType, principalID string, roleIDs []string) (int, error) {
	token, _, _, err := srv.userTokenService.GenerateTokenPair(
		actor.ID, actor.Email, actor.DisplayName, actor.Role, ClientTypeWeb,
	)
	if err != nil {
		return 0, fmt.Errorf("generate token: %w", err)
	}
	body, err := json.Marshal(map[string]interface{}{"roleDefinitionIds": roleIDs})
	if err != nil {
		return 0, fmt.Errorf("marshal body: %w", err)
	}
	req := httptest.NewRequest(http.MethodPut, mmrPrincipalPath(projectID, principalType, principalID), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec.Code, nil
}

func TestSetMemberRoles_Concurrency_ConflictingPUTs(t *testing.T) {
	f := setupMMRFixture(t)

	var wg sync.WaitGroup
	codes := make([]int, 2)
	errs := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		codes[0], errs[0] = mmrConcurrentPut(f.srv, f.owner, f.projectID, "user", f.member.ID, []string{f.adminRD.ID})
	}()
	go func() {
		defer wg.Done()
		codes[1], errs[1] = mmrConcurrentPut(f.srv, f.owner, f.projectID, "user", f.member.ID, []string{f.withinCeiling.ID, f.memberRD.ID})
	}()
	wg.Wait()

	for i, err := range errs {
		require.NoError(t, err, "request %d setup", i)
	}
	for _, code := range codes {
		assert.True(t, code == http.StatusOK || code == http.StatusConflict,
			"expected 200 (serialized winner) or 409 membership_changed, got %d", code)
	}
	// R2-8 (review r2): §12 P1 requires that one of the two conflicting
	// requests succeeds, not merely that neither returns an unexpected code —
	// the lock must serialize them, not reject both.
	assert.Contains(t, codes, http.StatusOK, "exactly one of the two conflicting PUTs must succeed (§12 P1), got codes %v", codes)

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
