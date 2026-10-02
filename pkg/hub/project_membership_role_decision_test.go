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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSetMemberRoles_MemberRoleDecision_CheckSelectionAndOrder pins the
// shared per-role decision (ptone/scion#2646 item 2): only the selected
// checks run, and with every check selected the first refusal in the fixed
// order (credential, structural, actor authority, governance, CanDelegate)
// wins. The end-to-end agreement between the PUT and assignable-roles is
// pinned separately by TestAssignableRoles_ConsistentWithPut.
func TestSetMemberRoles_MemberRoleDecision_CheckSelectionAndOrder(t *testing.T) {
	f := setupMMRFixture(t)
	svc := f.srv.membershipService
	ctx := mmrServiceCtx(f.owner.ID, f.owner.Email)
	actor := mmrServiceIdentity(f.owner.ID, f.owner.Email)
	addOf := func(name string) planChange { return planChange{op: MembershipOpAdd, roleName: name} }

	credentialDenial := &MembershipDecision{Allowed: false, DenialCode: ErrCodeCredentialInsufficient, Reason: "credential", HTTPStatus: 403}
	denied := &memberActorAuthority{credentialDenial: credentialDenial, authorityDenial: noProjectRoleDecision()}

	// Every check: the credential refusal comes first, even for a
	// structurally refused role.
	d, _ := svc.memberRoleDecision(ctx, actor, f.projectID, denied, addOf(f.roleBindingCustom.Name), f.roleBindingCustom, memberRoleCheckAll)
	assert.Same(t, credentialDenial, d)

	// Structural only: reads no actor authority (nil is safe) and refuses
	// only a role_binding.*-bearing custom role.
	d, _ = svc.memberRoleDecision(ctx, actor, f.projectID, nil, planChange{}, f.roleBindingCustom, memberRoleCheckStructural)
	require.NotNil(t, d)
	assert.Equal(t, ErrCodeRoleAssignmentForbidden, d.DenialCode)
	assert.Equal(t, f.roleBindingCustom.ID, d.Details["roleDefinitionId"])
	d, _ = svc.memberRoleDecision(ctx, actor, f.projectID, nil, planChange{}, f.withinCeiling, memberRoleCheckStructural)
	assert.Nil(t, d)

	// Structural precedes actor authority.
	d, _ = svc.memberRoleDecision(ctx, actor, f.projectID, denied, addOf(f.roleBindingCustom.Name), f.roleBindingCustom, memberRoleCheckStructural|memberRoleCheckActorAuthority)
	require.NotNil(t, d)
	assert.Equal(t, f.roleBindingCustom.ID, d.Details["roleDefinitionId"])
	d, _ = svc.memberRoleDecision(ctx, actor, f.projectID, denied, addOf(f.memberRD.Name), f.memberRD, memberRoleCheckStructural|memberRoleCheckActorAuthority)
	assert.Equal(t, noProjectRoleDecision(), d)

	// The real owner authority, as both callers compute it.
	owner, err := svc.memberActorAuthorityPreTx(ctx, f.owner.ID, f.projectID, true, false, true, false)
	require.NoError(t, err)
	require.Nil(t, owner.authorityDenial)
	assert.Equal(t, store.ProjectRoleOwner, owner.role)
	assert.True(t, owner.isDirectOwner)
	assert.False(t, owner.hubOverride)

	// Governance passes for the owner; CanDelegate refuses a beyond-ceiling
	// role only when selected, and returns the allow reason otherwise.
	d, _ = svc.memberRoleDecision(ctx, actor, f.projectID, owner, addOf(f.beyondCeiling.Name), f.beyondCeiling, memberRoleCheckGovernance)
	assert.Nil(t, d, "governance alone does not run CanDelegate")
	d, _ = svc.memberRoleDecision(ctx, actor, f.projectID, owner, addOf(f.beyondCeiling.Name), f.beyondCeiling, memberRoleCheckAll)
	assert.Equal(t, canDelegateRefusal(f.beyondCeiling, d.Details["reason"].(string)), d)
	d, reason := svc.memberRoleDecision(ctx, actor, f.projectID, owner, addOf(f.withinCeiling.Name), f.withinCeiling, memberRoleCheckAll)
	assert.Nil(t, d)
	assert.NotEmpty(t, reason, "an allowed CanDelegate returns its reason for the audit row")
}
