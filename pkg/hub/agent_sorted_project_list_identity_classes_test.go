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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file proves that the decision-count and caps-deep-equal tests
// exercise more than one identity class. ComputeCapabilitiesForActions is a
// separate copy of ComputeCapabilitiesBatch's loop (intentionally not a
// shared call; see the PINNED comment in capabilities.go), so its
// IsScopedUserIdentity/DecideFromContext branch is only proven correct if a
// scoped UAT actually exercises it end to end -- owner/member/agent-JWT
// alone never take that branch.
//
// Each test mints the identity class via the real token/role-binding path
// (never a hand-built Identity value), hits both the legacy and sorted
// endpoints with it, and asserts: (1) the sorted page's merged
// _capabilities deep-equal the legacy path's ComputeCapabilitiesBatch-
// derived _capabilities, action order included (the technique
// TestListProjectAgentsSorted_CapsDeepEqualLegacy already uses for the
// owner case); and (2) the exact decision count the 5+8n
// (all-readable, complete) formula predicts.

// TestListProjectAgentsSorted_CapsDeepEqual_ScopedUAT exercises the one
// identity class whose capability evaluation takes a genuinely different
// code path: a scoped UAT goes through computeCapabilitiesWithContext ->
// DecideFromContext, not CheckAccess.
func TestListProjectAgentsSorted_CapsDeepEqual_ScopedUAT(t *testing.T) {
	f := sortedListSetup(t)
	const n = 3
	for i := 0; i < n; i++ {
		f.createAgent(t, fmt.Sprintf("uat-%d", i), string(state.PhaseStopped), nil)
	}

	// agent:manage expands to create/delete/lifecycle/list/message/read
	// (permissions.UATManageAliases), which covers both the project's
	// agent.list gate and agent.read -- a UAT scoped to only "agent:read"
	// would never pass the gate at all.
	uatKey := mintScopedUAT(t, f.srv, f.owner.ID, f.project.ID, []string{"agent:manage"})

	legacyRec := doRequestWithUAT(t, f.srv, uatKey, http.MethodGet, f.listPath(""), nil)
	require.Equal(t, http.StatusOK, legacyRec.Code, legacyRec.Body.String())
	legacy := mustDecodeListAgentsResponse(t, legacyRec.Body)
	require.Len(t, legacy.Agents, n)
	legacyCaps := map[string][]string{}
	for _, a := range legacy.Agents {
		legacyCaps[a.ID] = a.Cap.Actions
	}

	emitter := &recordingDecisionAuditEmitter{}
	f.srv.authzService.SetDecisionAuditEmitter(emitter)

	sortedRec := doRequestWithUAT(t, f.srv, uatKey, http.MethodGet, f.listPath(fmt.Sprintf("sort=updated&fit=%d&limit=%d", n, n)), nil)
	require.Equal(t, http.StatusOK, sortedRec.Code, sortedRec.Body.String())
	sorted := mustDecodeListAgentsResponse(t, sortedRec.Body)
	require.Len(t, sorted.Agents, n)

	for _, a := range sorted.Agents {
		assert.Equal(t, legacyCaps[a.ID], a.Cap.Actions,
			"scoped UAT: sorted mode's merged capabilities must deep-equal the legacy ComputeCapabilitiesBatch output for agent %s, including action order", a.ID)
	}

	// 5 (gate + 4 scope caps) + n (read pass, via DecideFromContext)
	// + 7n (remaining-actions pass, via DecideFromContext) = 5 + 8n.
	// DecideFromContext costs exactly one Decide/audit record per
	// (resource, action), the same as CheckAccess (authz.go: "All
	// authorization decisions route through the AK1 kernel -- there are no
	// privileged early-allow bypasses"), so the formula is identity-class
	// independent.
	assert.Len(t, emitter.records, 5+8*n, "the 5+8n formula must hold for a scoped UAT too, not just a plain member")
}

// TestListProjectAgentsSorted_CapsDeepEqual_SuperAdmin exercises a
// super-admin who is NOT a project member: super-admin's role definition
// holds every permission, so the kernel's normal role-binding search
// (CheckAccess, same as a plain member) grants everything with no project
// membership needed and no decision/audit skipped.
func TestListProjectAgentsSorted_CapsDeepEqual_SuperAdmin(t *testing.T) {
	f := sortedListSetup(t)
	const n = 3
	for i := 0; i < n; i++ {
		f.createAgent(t, fmt.Sprintf("sa-%d", i), string(state.PhaseStopped), nil)
	}

	superAdminID := tid("sl-identity-super-admin")
	createTestUserWithRole(t, f.store, superAdminID, "sl-identity-superadmin@test.com", store.UserRoleAdmin, store.SystemRoleSuperAdmin)
	superAdmin, err := f.store.GetUser(context.Background(), superAdminID)
	require.NoError(t, err)

	legacyRec := doRequestAsUser(t, f.srv, superAdmin, http.MethodGet, f.listPath(""), nil)
	require.Equal(t, http.StatusOK, legacyRec.Code, legacyRec.Body.String())
	legacy := mustDecodeListAgentsResponse(t, legacyRec.Body)
	require.Len(t, legacy.Agents, n, "super-admin must see every agent despite not being a project member")
	legacyCaps := map[string][]string{}
	for _, a := range legacy.Agents {
		legacyCaps[a.ID] = a.Cap.Actions
	}

	emitter := &recordingDecisionAuditEmitter{}
	f.srv.authzService.SetDecisionAuditEmitter(emitter)

	sortedRec := doRequestAsUser(t, f.srv, superAdmin, http.MethodGet, f.listPath(fmt.Sprintf("sort=updated&fit=%d&limit=%d", n, n)), nil)
	require.Equal(t, http.StatusOK, sortedRec.Code, sortedRec.Body.String())
	sorted := mustDecodeListAgentsResponse(t, sortedRec.Body)
	require.Len(t, sorted.Agents, n)

	for _, a := range sorted.Agents {
		assert.Equal(t, legacyCaps[a.ID], a.Cap.Actions,
			"super-admin: sorted mode's merged capabilities must deep-equal the legacy ComputeCapabilitiesBatch output for agent %s, including action order", a.ID)
	}

	assert.Len(t, emitter.records, 5+8*n, "the 5+8n formula must hold for a non-member super-admin too")
}

// TestListProjectAgentsSorted_HubAdminNonMember_DeniedAtGate documents the
// hub-admin identity class's actual behavior: hub-admin's curated
// permission set (hubAdminPermissionIDs, seed.go) does not include
// agent.list/agent.read -- those are project-scoped permissions, granted by
// project membership roles, not the system-scope hub-admin role. A
// non-member hub-admin is therefore correctly denied at the project's
// agent.list gate, at exactly one decision, the same fail-closed shape as
// any other non-member (TestListProjectAgentsRequiresAuthorization's
// "non-member user is denied" case). This is the hub-admin identity class's
// actual evidence for the decision-count gate: proving it does not
// special-case "admin-sounding" roles that lack the specific permission.
func TestListProjectAgentsSorted_HubAdminNonMember_DeniedAtGate(t *testing.T) {
	f := sortedListSetup(t)
	f.createAgent(t, "ha-1", string(state.PhaseStopped), nil)

	hubAdminID := tid("sl-identity-hub-admin")
	createTestUserWithRole(t, f.store, hubAdminID, "sl-identity-hubadmin@test.com", store.UserRoleMember, store.SystemRoleHubAdmin)
	hubAdmin, err := f.store.GetUser(context.Background(), hubAdminID)
	require.NoError(t, err)

	emitter := &recordingDecisionAuditEmitter{}
	f.srv.authzService.SetDecisionAuditEmitter(emitter)

	rec := doRequestAsUser(t, f.srv, hubAdmin, http.MethodGet, f.listPath("sort=updated"), nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Len(t, emitter.records, 1, "exactly the agent.list gate decision; hub-admin's curated permissions do not include agent.list")
}

// TestListProjectAgentsSorted_CapsDeepEqual_HubAdminProjectMember covers the
// hub-admin role in the one shape where it actually reaches the per-item
// capability loop: combined with ordinary project membership (hub-admin is
// a system-scope addition, not a project-scope substitute). This proves the
// extra system-scope binding does not perturb the normal member capability
// resolution or the decision-count formula.
func TestListProjectAgentsSorted_CapsDeepEqual_HubAdminProjectMember(t *testing.T) {
	f := sortedListSetup(t)
	const n = 3
	for i := 0; i < n; i++ {
		f.createAgent(t, fmt.Sprintf("hapm-%d", i), string(state.PhaseStopped), nil)
	}

	hubAdminID := tid("sl-identity-hub-admin-member")
	createTestUserWithRole(t, f.store, hubAdminID, "sl-identity-hubadmin-member@test.com", store.UserRoleMember, store.SystemRoleHubAdmin)
	msgAuthzAddProjectMember(t, f.store, hubAdminID, f.project.ID, f.project.Slug, store.GroupMemberRoleMember)
	hubAdmin, err := f.store.GetUser(context.Background(), hubAdminID)
	require.NoError(t, err)

	legacyRec := doRequestAsUser(t, f.srv, hubAdmin, http.MethodGet, f.listPath(""), nil)
	require.Equal(t, http.StatusOK, legacyRec.Code, legacyRec.Body.String())
	legacy := mustDecodeListAgentsResponse(t, legacyRec.Body)
	require.Len(t, legacy.Agents, n)
	legacyCaps := map[string][]string{}
	for _, a := range legacy.Agents {
		legacyCaps[a.ID] = a.Cap.Actions
	}

	emitter := &recordingDecisionAuditEmitter{}
	f.srv.authzService.SetDecisionAuditEmitter(emitter)

	sortedRec := doRequestAsUser(t, f.srv, hubAdmin, http.MethodGet, f.listPath(fmt.Sprintf("sort=updated&fit=%d&limit=%d", n, n)), nil)
	require.Equal(t, http.StatusOK, sortedRec.Code, sortedRec.Body.String())
	sorted := mustDecodeListAgentsResponse(t, sortedRec.Body)
	require.Len(t, sorted.Agents, n)

	for _, a := range sorted.Agents {
		assert.Equal(t, legacyCaps[a.ID], a.Cap.Actions,
			"hub-admin+member: sorted mode's merged capabilities must deep-equal the legacy ComputeCapabilitiesBatch output for agent %s, including action order", a.ID)
	}

	assert.Len(t, emitter.records, 5+8*n, "the 5+8n formula must hold for a hub-admin project member too")
}
