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

package hub

import (
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// planRoleSet unit tests (ptone/scion#2529 P1).
//
// These are pure-function tests: no store, no transaction, no HTTP. They
// cover the diff algorithm planRoleSet uses to turn (current bindings,
// desired role definitions) into Keep/Remove/Create/BuiltInChange.
// =============================================================================

func planBinding(id, roleDefID string) *store.RoleBinding {
	return &store.RoleBinding{ID: id, RoleDefinitionID: roleDefID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: "p1", ScopeType: store.RoleScopeProject, ScopeID: "proj1"}
}

func planRoleDef(id, name string) *store.RoleDefinition {
	return &store.RoleDefinition{ID: id, Name: name, ScopeType: store.RoleScopeProject}
}

func TestPlanRoleSet_Keep(t *testing.T) {
	memberDef := planRoleDef("rd-member", store.ProjectRoleMember)
	current := []*store.RoleBinding{planBinding("b1", "rd-member")}
	defs := map[string]*store.RoleDefinition{"rd-member": memberDef}

	plan := planRoleSet(current, defs, []*store.RoleDefinition{memberDef})

	require.Len(t, plan.Keep, 1)
	assert.Equal(t, "b1", plan.Keep[0].ID)
	assert.Empty(t, plan.Remove)
	assert.Empty(t, plan.Create)
	assert.Nil(t, plan.BuiltInChange)
	assert.True(t, plan.isEmpty())
}

func TestPlanRoleSet_Add(t *testing.T) {
	memberDef := planRoleDef("rd-member", store.ProjectRoleMember)
	customDef := planRoleDef("rd-customA", "custom-a")
	current := []*store.RoleBinding{planBinding("b1", "rd-member")}
	defs := map[string]*store.RoleDefinition{"rd-member": memberDef}

	plan := planRoleSet(current, defs, []*store.RoleDefinition{memberDef, customDef})

	require.Len(t, plan.Keep, 1)
	assert.Empty(t, plan.Remove)
	require.Len(t, plan.Create, 1)
	assert.Equal(t, "rd-customA", plan.Create[0].ID)
	assert.Nil(t, plan.BuiltInChange)
	assert.False(t, plan.isEmpty())
}

func TestPlanRoleSet_Remove(t *testing.T) {
	memberDef := planRoleDef("rd-member", store.ProjectRoleMember)
	customDef := planRoleDef("rd-customA", "custom-a")
	current := []*store.RoleBinding{planBinding("b1", "rd-member"), planBinding("b2", "rd-customA")}
	defs := map[string]*store.RoleDefinition{"rd-member": memberDef, "rd-customA": customDef}

	plan := planRoleSet(current, defs, []*store.RoleDefinition{memberDef})

	require.Len(t, plan.Keep, 1)
	require.Len(t, plan.Remove, 1)
	assert.Equal(t, "b2", plan.Remove[0].ID)
	assert.Empty(t, plan.Create)
	assert.Nil(t, plan.BuiltInChange)
}

func TestPlanRoleSet_BuiltInChange(t *testing.T) {
	memberDef := planRoleDef("rd-member", store.ProjectRoleMember)
	adminDef := planRoleDef("rd-admin", store.ProjectRoleAdmin)
	current := []*store.RoleBinding{planBinding("b1", "rd-member")}
	defs := map[string]*store.RoleDefinition{"rd-member": memberDef}

	plan := planRoleSet(current, defs, []*store.RoleDefinition{adminDef})

	require.Len(t, plan.Remove, 1)
	require.Len(t, plan.Create, 1)
	require.NotNil(t, plan.BuiltInChange)
	assert.Equal(t, "b1", plan.BuiltInChange.Old.ID)
	assert.Equal(t, "rd-admin", plan.BuiltInChange.New.ID)
}

func TestPlanRoleSet_BuiltInToNone(t *testing.T) {
	memberDef := planRoleDef("rd-member", store.ProjectRoleMember)
	current := []*store.RoleBinding{planBinding("b1", "rd-member")}
	defs := map[string]*store.RoleDefinition{"rd-member": memberDef}

	plan := planRoleSet(current, defs, nil)

	require.Len(t, plan.Remove, 1)
	assert.Empty(t, plan.Create)
	assert.Nil(t, plan.BuiltInChange)
}

func TestPlanRoleSet_NoneToBuiltIn(t *testing.T) {
	memberDef := planRoleDef("rd-member", store.ProjectRoleMember)

	plan := planRoleSet(nil, map[string]*store.RoleDefinition{}, []*store.RoleDefinition{memberDef})

	assert.Empty(t, plan.Remove)
	require.Len(t, plan.Create, 1)
	assert.Equal(t, "rd-member", plan.Create[0].ID)
	assert.Nil(t, plan.BuiltInChange)
}

func TestPlanRoleSet_Duplicates(t *testing.T) {
	memberDef := planRoleDef("rd-member", store.ProjectRoleMember)
	current := []*store.RoleBinding{planBinding("b1", "rd-member")}
	defs := map[string]*store.RoleDefinition{"rd-member": memberDef}

	// Desired carries the same definition twice; planRoleSet should not
	// produce duplicate Keep/Create entries.
	plan := planRoleSet(current, defs, []*store.RoleDefinition{memberDef, memberDef})

	assert.Len(t, plan.Keep, 1)
	assert.Empty(t, plan.Create)
	assert.True(t, plan.isEmpty())
}

func TestPlanRoleSet_CustomOnly(t *testing.T) {
	customA := planRoleDef("rd-customA", "custom-a")
	customB := planRoleDef("rd-customB", "custom-b")
	current := []*store.RoleBinding{planBinding("b1", "rd-customA")}
	defs := map[string]*store.RoleDefinition{"rd-customA": customA}

	plan := planRoleSet(current, defs, []*store.RoleDefinition{customA, customB})

	require.Len(t, plan.Keep, 1)
	assert.Empty(t, plan.Remove)
	require.Len(t, plan.Create, 1)
	assert.Equal(t, "rd-customB", plan.Create[0].ID)
	assert.Nil(t, plan.BuiltInChange, "no built-in role is involved")
}

func TestPlanRoleSet_Changes_BuiltInChangeReportedAsTwoUpdates(t *testing.T) {
	memberDef := planRoleDef("rd-member", store.ProjectRoleMember)
	adminDef := planRoleDef("rd-admin", store.ProjectRoleAdmin)
	current := []*store.RoleBinding{planBinding("b1", "rd-member")}
	defs := map[string]*store.RoleDefinition{"rd-member": memberDef}

	plan := planRoleSet(current, defs, []*store.RoleDefinition{adminDef})
	changes := plan.changes(defs)

	require.Len(t, changes, 2)
	assert.Equal(t, planChange{op: MembershipOpUpdate, roleName: store.ProjectRoleMember}, changes[0])
	assert.Equal(t, planChange{op: MembershipOpUpdate, roleName: store.ProjectRoleAdmin}, changes[1])
}

func TestPlanRoleSet_Changes_ExcludesKeep(t *testing.T) {
	memberDef := planRoleDef("rd-member", store.ProjectRoleMember)
	customA := planRoleDef("rd-customA", "custom-a")
	current := []*store.RoleBinding{planBinding("b1", "rd-member"), planBinding("b2", "rd-customA")}
	defs := map[string]*store.RoleDefinition{"rd-member": memberDef, "rd-customA": customA}

	plan := planRoleSet(current, defs, []*store.RoleDefinition{memberDef, customA})
	assert.Empty(t, plan.changes(defs), "an all-Keep plan has no governance-relevant changes")
}

func TestPlanRoleSet_HasCustomCreateAndRemove(t *testing.T) {
	memberDef := planRoleDef("rd-member", store.ProjectRoleMember)
	customA := planRoleDef("rd-customA", "custom-a")
	current := []*store.RoleBinding{planBinding("b1", "rd-member")}
	defs := map[string]*store.RoleDefinition{"rd-member": memberDef}

	plan := planRoleSet(current, defs, []*store.RoleDefinition{memberDef, customA})
	assert.True(t, plan.hasCustomCreate())
	assert.False(t, plan.hasCustomRemove(defs))

	current2 := []*store.RoleBinding{planBinding("b1", "rd-member"), planBinding("b2", "rd-customA")}
	defs2 := map[string]*store.RoleDefinition{"rd-member": memberDef, "rd-customA": customA}
	plan2 := planRoleSet(current2, defs2, []*store.RoleDefinition{memberDef})
	assert.False(t, plan2.hasCustomCreate())
	assert.True(t, plan2.hasCustomRemove(defs2))
}

// =============================================================================
// actorAuthorityChanged unit tests (review r3 R3-3).
//
// Pure-function table tests for the R2-2 TOCTOU guard's comparison, driving
// each of the three components (role, hubOverride, per-perm Via)
// independently so a regression that decouples them from role equality is
// caught even though no production path can reach that combination today
// (see the doc comment on actorAuthorityChanged in project_membership_set.go).
// =============================================================================

func TestActorAuthorityChanged(t *testing.T) {
	owner := customRoleAuthority{Allowed: true, Via: customRoleAuthorityViaOwner}
	hubBinding := customRoleAuthority{Allowed: true, Via: customRoleAuthorityViaHub}

	cases := []struct {
		name string
		pre  actorAuthoritySnapshot
		post actorAuthoritySnapshot
		want bool
	}{
		{
			name: "identical snapshots, no custom perms asked",
			pre:  actorAuthoritySnapshot{role: store.ProjectRoleOwner, hubOverride: false},
			post: actorAuthoritySnapshot{role: store.ProjectRoleOwner, hubOverride: false},
			want: false,
		},
		{
			name: "role changed",
			pre:  actorAuthoritySnapshot{role: store.ProjectRoleOwner, hubOverride: false},
			post: actorAuthoritySnapshot{role: "", hubOverride: true},
			want: true,
		},
		{
			// R4-2 (review r4): the "role changed" case above also changes
			// hubOverride in the same step, so the hubOverride sub-check alone
			// already makes it return true — probe P4 (removing the
			// `pre.role != post.role` comparison) left every existing case
			// green. role is the only one of the three components reachable
			// in production (see the doc comment on actorAuthorityChanged),
			// so it needs its own case that changes role ALONE, holding
			// hubOverride equal.
			name: "role changed alone, hubOverride held equal (the only component reachable in production)",
			pre:  actorAuthoritySnapshot{role: store.ProjectRoleOwner, hubOverride: false},
			post: actorAuthoritySnapshot{role: store.ProjectRoleAdmin, hubOverride: false},
			want: true,
		},
		{
			// Same idea, with equal non-empty customAuth maps on both sides,
			// so only the role comparison can be responsible for `want: true`.
			name: "role changed alone, hubOverride and customAuth held equal",
			pre: actorAuthoritySnapshot{role: store.ProjectRoleOwner, hubOverride: false,
				customAuth: map[string]customRoleAuthority{PermRoleBindingCreate: owner}},
			post: actorAuthoritySnapshot{role: store.ProjectRoleAdmin, hubOverride: false,
				customAuth: map[string]customRoleAuthority{PermRoleBindingCreate: owner}},
			want: true,
		},
		{
			name: "hubOverride changed with role held equal (unreachable in production; defence in depth)",
			pre:  actorAuthoritySnapshot{role: "", hubOverride: false},
			post: actorAuthoritySnapshot{role: "", hubOverride: true},
			want: true,
		},
		{
			name: "Via changed for an asked perm, role and hubOverride held equal (unreachable in production; defence in depth)",
			pre: actorAuthoritySnapshot{role: "", hubOverride: true,
				customAuth: map[string]customRoleAuthority{PermRoleBindingCreate: hubBinding}},
			post: actorAuthoritySnapshot{role: "", hubOverride: true,
				customAuth: map[string]customRoleAuthority{PermRoleBindingCreate: owner}},
			want: true,
		},
		{
			name: "same Via for an asked perm",
			pre: actorAuthoritySnapshot{role: store.ProjectRoleOwner, hubOverride: false,
				customAuth: map[string]customRoleAuthority{PermRoleBindingCreate: owner}},
			post: actorAuthoritySnapshot{role: store.ProjectRoleOwner, hubOverride: false,
				customAuth: map[string]customRoleAuthority{PermRoleBindingCreate: owner}},
			want: false,
		},
		{
			// Review r1 A-R2: an asked-set mismatch is treated as changed
			// (fail closed), even though plan1 == plan0 by construction
			// (current1 == current0) means this case is unreached in
			// production today.
			name: "perm asked pre but not post fails closed (plan1 == plan0 by construction; unreached in production)",
			pre: actorAuthoritySnapshot{role: store.ProjectRoleOwner, hubOverride: false,
				customAuth: map[string]customRoleAuthority{PermRoleBindingCreate: owner}},
			post: actorAuthoritySnapshot{role: store.ProjectRoleOwner, hubOverride: false,
				customAuth: map[string]customRoleAuthority{}},
			want: true,
		},
		{
			// Mirror of the above: asked post but not pre.
			name: "perm asked post but not pre fails closed (plan1 == plan0 by construction; unreached in production)",
			pre: actorAuthoritySnapshot{role: store.ProjectRoleOwner, hubOverride: false,
				customAuth: map[string]customRoleAuthority{}},
			post: actorAuthoritySnapshot{role: store.ProjectRoleOwner, hubOverride: false,
				customAuth: map[string]customRoleAuthority{PermRoleBindingCreate: owner}},
			want: true,
		},
		{
			name: "two perms asked, only the second's Via changed",
			pre: actorAuthoritySnapshot{role: store.ProjectRoleOwner, hubOverride: false,
				customAuth: map[string]customRoleAuthority{
					PermRoleBindingCreate: owner,
					PermRoleBindingDelete: owner,
				}},
			post: actorAuthoritySnapshot{role: store.ProjectRoleOwner, hubOverride: false,
				customAuth: map[string]customRoleAuthority{
					PermRoleBindingCreate: owner,
					PermRoleBindingDelete: hubBinding,
				}},
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, actorAuthorityChanged(tc.pre, tc.post))
		})
	}
}
