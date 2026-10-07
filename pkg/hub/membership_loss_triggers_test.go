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

// One test per membership-loss path (ptone/scion#3433, design section 3.2):
// each path writes a membership loss check, and processing it holds the
// user's agents when access ended. Revert proof: drop a path's enqueue and
// its test fails on the missing check or hold, while the live standing
// check still refuses the agents (asserted in each test as well, so the two
// layers are shown to be independent).

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func (f *msFixture) ownerIdentity() UserIdentity {
	return NewAuthenticatedUser(f.ownerID, f.ownerID+"@test.com", "Owner", "member", "web")
}

// requireCheck asserts a pending check exists for userID with trigger.
func requireCheck(t *testing.T, s store.Store, userID string, trigger store.MembershipLossTrigger) {
	t.Helper()
	for _, c := range pendingChecks(t, s) {
		if c.UserID == userID && c.Trigger == trigger {
			return
		}
	}
	t.Fatalf("no membership loss check for user %s with trigger %s", userID, trigger)
}

func (f *msFixture) userBinding(userID string) *store.RoleBinding {
	f.t.Helper()
	rbs, err := f.s.ListRoleBindingsForPrincipal(context.Background(), store.RoleBindingPrincipalUser, userID)
	require.NoError(f.t, err)
	for _, rb := range rbs {
		if rb.ScopeType == store.RoleScopeProject && rb.ScopeID == f.projectID {
			return rb
		}
	}
	f.t.Fatalf("no project binding for %s", userID)
	return nil
}

func (f *msFixture) requireTreeHeldAndRefused() {
	f.t.Helper()
	// A path may also process its checks in the background right after the
	// change; drain until the holds are visible (bounded).
	for i := 0; i < 50 && (!f.held(f.agentA.ID) || !f.held(f.childC.ID)); i++ {
		f.srv.drainMembershipLossChecks(context.Background())
		time.Sleep(20 * time.Millisecond)
	}
	assert.True(f.t, f.held(f.agentA.ID), "agent A held")
	assert.True(f.t, f.held(f.childC.ID), "child C held")
	require.Error(f.t, f.srv.agentStanding(context.Background(), f.childC.ID))
}

// Path 1: remove member binding.
func TestMembershipLossTrigger_RemoveMember(t *testing.T) {
	f := newMSFixture(t, "trig-remove")
	ctx := mmrServiceCtx(f.ownerID, f.ownerID+"@test.com")
	_, d := f.srv.membershipService.RemoveMember(ctx, MembershipRequest{
		Op: MembershipOpRemove, ProjectID: f.projectID, Actor: f.ownerIdentity(), BindingID: f.userBinding(f.userID).ID,
	})
	require.Nil(t, d)
	requireStandingReason(t, f.srv.agentStanding(context.Background(), f.agentA.ID), standingReasonRootNotAdmited)
	requireCheck(t, f.s, f.userID, store.MembershipLossTriggerMemberRemove)
	f.requireTreeHeldAndRefused()
}

// Path 1 (group principal): removing a group's project binding re-evaluates
// every transitive member user.
func TestMembershipLossTrigger_RemoveGroupBinding(t *testing.T) {
	f := newMSFixture(t, "trig-remove-group")
	ctx := context.Background()
	f.dropBindings(f.userID)
	groupID := uuid.NewString()
	require.NoError(t, f.s.CreateGroup(ctx, &store.Group{ID: groupID, Name: "trig-g", Slug: "trig-g-" + groupID[:8], GroupType: store.GroupTypeExplicit}))
	require.NoError(t, f.s.AddGroupMember(ctx, &store.GroupMember{GroupID: groupID, MemberType: store.GroupMemberTypeUser, MemberID: f.userID, Role: store.GroupMemberRoleMember}))
	rd, err := f.s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	rb, err := f.s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID, PrincipalType: store.RoleBindingPrincipalGroup, PrincipalID: groupID,
		ScopeType: store.RoleScopeProject, ScopeID: f.projectID, CreatedBy: "test",
	})
	require.NoError(t, err)
	require.NoError(t, f.srv.agentStanding(ctx, f.agentA.ID), "admitted through the group")

	octx := mmrServiceCtx(f.ownerID, f.ownerID+"@test.com")
	_, d := f.srv.membershipService.RemoveMember(octx, MembershipRequest{
		Op: MembershipOpRemove, ProjectID: f.projectID, Actor: f.ownerIdentity(), BindingID: rb.ID,
	})
	require.Nil(t, d)
	requireCheck(t, f.s, f.userID, store.MembershipLossTriggerMemberRemove)
	f.requireTreeHeldAndRefused()
}

// Path 2: role change (access continues: a check is written and is a no-op).
func TestMembershipLossTrigger_UpdateMemberRole(t *testing.T) {
	f := newMSFixture(t, "trig-role")
	ctx := mmrServiceCtx(f.ownerID, f.ownerID+"@test.com")
	admin, err := f.s.GetRoleDefinitionByName(context.Background(), store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)
	_, d := f.srv.membershipService.UpdateMemberRole(ctx, MembershipRequest{
		Op: MembershipOpUpdate, ProjectID: f.projectID, Actor: f.ownerIdentity(),
		BindingID: f.userBinding(f.userID).ID, NewRoleDefID: admin.ID,
	})
	require.Nil(t, d)
	requireCheck(t, f.s, f.userID, store.MembershipLossTriggerMemberRoleChange)
	f.srv.drainMembershipLossChecks(context.Background())
	assert.False(t, f.held(f.agentA.ID), "still admitted: no hold")
}

// Paths 3 and 4: PUT member roles and DELETE member principal.
func TestMembershipLossTrigger_SetMemberRolesRemoveAll(t *testing.T) {
	f := newMSFixture(t, "trig-setroles")
	ctx := mmrServiceCtx(f.ownerID, f.ownerID+"@test.com")
	_, d := f.srv.membershipService.SetMemberRoles(ctx, SetMemberRolesRequest{
		ProjectID: f.projectID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: f.userID,
		Actor: f.ownerIdentity(), RemoveAll: true,
	})
	require.Nil(t, d)
	requireStandingReason(t, f.srv.agentStanding(context.Background(), f.agentA.ID), standingReasonRootNotAdmited)
	requireCheck(t, f.s, f.userID, store.MembershipLossTriggerMemberPrincipalDelete)
	f.requireTreeHeldAndRefused()
}

func TestMembershipLossTrigger_SetMemberRolesChange(t *testing.T) {
	f := newMSFixture(t, "trig-setroles-put")
	ctx := mmrServiceCtx(f.ownerID, f.ownerID+"@test.com")
	admin, err := f.s.GetRoleDefinitionByName(context.Background(), store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)
	_, d := f.srv.membershipService.SetMemberRoles(ctx, SetMemberRolesRequest{
		ProjectID: f.projectID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: f.userID,
		Actor: f.ownerIdentity(), DesiredRoleIDs: []string{admin.ID},
	})
	require.Nil(t, d)
	requireCheck(t, f.s, f.userID, store.MembershipLossTriggerMemberRoleChange)
}

// Path 5: admin role-binding delete through the role-binding endpoint.
func TestMembershipLossTrigger_AdminBindingDelete(t *testing.T) {
	f := newMSFixture(t, "trig-adminrb")
	rec := doRequest(t, f.srv, http.MethodDelete, "/api/v1/admin/role-bindings/"+f.userBinding(f.userID).ID, nil)
	require.Less(t, rec.Code, 300, rec.Body.String())
	requireStandingReason(t, f.srv.agentStanding(context.Background(), f.agentA.ID), standingReasonRootNotAdmited)
	requireCheck(t, f.s, f.userID, store.MembershipLossTriggerAdminBindingDelete)
	f.requireTreeHeldAndRefused()
}

// Path 6: ownership transfer re-evaluates the previous owner (a no-op).
func TestMembershipLossTrigger_TransferOwnership(t *testing.T) {
	f := newMSFixture(t, "trig-transfer")
	ctx := mmrServiceCtx(f.ownerID, f.ownerID+"@test.com")
	_, d := f.srv.membershipService.TransferOwnership(ctx, MembershipRequest{
		Op: MembershipOpTransfer, ProjectID: f.projectID, Actor: f.ownerIdentity(), NewOwnerID: f.userID,
	})
	require.Nil(t, d)
	requireCheck(t, f.s, f.ownerID, store.MembershipLossTriggerOwnershipTransfer)
}

// groupFixture gives U project access only through a group G.
func groupFixture(t *testing.T, f *msFixture) string {
	t.Helper()
	ctx := context.Background()
	f.dropBindings(f.userID)
	groupID := uuid.NewString()
	require.NoError(t, f.s.CreateGroup(ctx, &store.Group{ID: groupID, Name: "trig-grp", Slug: "trig-grp-" + groupID[:8], GroupType: store.GroupTypeExplicit}))
	require.NoError(t, f.s.AddGroupMember(ctx, &store.GroupMember{GroupID: groupID, MemberType: store.GroupMemberTypeUser, MemberID: f.userID, Role: store.GroupMemberRoleMember}))
	require.NoError(t, f.s.AddGroupMember(ctx, &store.GroupMember{GroupID: groupID, MemberType: store.GroupMemberTypeUser, MemberID: f.ownerID, Role: store.GroupMemberRoleOwner}))
	rd, err := f.s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = f.s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID, PrincipalType: store.RoleBindingPrincipalGroup, PrincipalID: groupID,
		ScopeType: store.RoleScopeProject, ScopeID: f.projectID, CreatedBy: "test",
	})
	require.NoError(t, err)
	require.NoError(t, f.srv.agentStanding(ctx, f.agentA.ID))
	return groupID
}

// Path 7: removing the user from the group that gave access.
func TestMembershipLossTrigger_GroupMemberRemove(t *testing.T) {
	f := newMSFixture(t, "trig-groupmember")
	groupID := groupFixture(t, f)
	f.srv.membershipService.onMembershipLoss = nil
	rec := doRequest(t, f.srv, http.MethodDelete, "/api/v1/groups/"+groupID+"/members/user/"+f.userID, nil)
	require.Less(t, rec.Code, 300, rec.Body.String())
	requireStandingReason(t, f.srv.agentStanding(context.Background(), f.agentA.ID), standingReasonRootNotAdmited)
	f.requireTreeHeldAndRefused()
}

// Path 8: deleting the group that gave access.
func TestMembershipLossTrigger_GroupDelete(t *testing.T) {
	f := newMSFixture(t, "trig-groupdelete")
	groupID := groupFixture(t, f)
	rec := doRequest(t, f.srv, http.MethodDelete, "/api/v1/groups/"+groupID, nil)
	require.Less(t, rec.Code, 300, rec.Body.String())
	requireStandingReason(t, f.srv.agentStanding(context.Background(), f.agentA.ID), standingReasonRootNotAdmited)
	f.requireTreeHeldAndRefused()
}

// Path 16: a hub-scope change writes a check for every project.
func TestMembershipLossTrigger_HubScopeChange(t *testing.T) {
	f := newMSFixture(t, "trig-hubscope")
	ctx := context.Background()
	require.NoError(t, f.s.WithTx(ctx, func(tx store.Store) error {
		return removeHubMembershipTx(ctx, tx, f.userID)
	}))
	requireCheck(t, f.s, f.userID, store.MembershipLossTriggerSystemScopeChange)

	// Removing a super-admin binding writes one too.
	rd, err := f.s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = f.s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: f.userID,
		ScopeType: store.RoleScopeSystem, CreatedBy: store.SystemReconcileCreatedBy,
	})
	require.NoError(t, err)
	require.NoError(t, f.s.WithTx(ctx, func(tx store.Store) error {
		return f.srv.deleteSuperAdminBindingTx(ctx, tx, f.userID, rd)
	}))
	requireCheck(t, f.s, f.userID, store.MembershipLossTriggerSystemScopeChange)
}

// A check with no project re-evaluates every project the user roots agents
// in.
func TestMembershipLoss_AllProjectsCheck(t *testing.T) {
	f := newMSFixture(t, "allprojects")
	ctx := context.Background()
	f.dropBindings(f.userID)
	require.NoError(t, enqueueMembershipLossTx(ctx, f.s, f.userID, "", store.MembershipLossTriggerGroupChange, AuditActor{}))
	f.requireTreeHeldAndRefused()
}
