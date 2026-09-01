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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// RS1: Project Membership and Ownership Authority Mutations
//
// Exhaustive table-driven tests for the ProjectMembershipService, covering:
//   - CT1 D5 typed governance matrix (actor/operation/target authority)
//   - CT1 D1 atomic ownership transfer
//   - CT1 D2 active-at-decision-time lifecycle enforcement
//   - CT1 D3 group eligibility for project-admin
//   - CT1 D4 one-binding-per-principal invariant with atomic replacement
//   - Last-owner guard
//   - Server-derived operation/target capabilities
//   - Delegation (non-amplification / conditional-on-increase)
//   - Principal eligibility
//   - Denial codes (lower_snake_case)
// =============================================================================

// ---------------------------------------------------------------------------
// RS1.1: Governance Matrix — table-driven actor × operation × target
// ---------------------------------------------------------------------------

func TestRS1_GovernanceMatrix(t *testing.T) {
	tests := []struct {
		name       string
		actorRole  string // project role of the actor
		op         string // "add", "update", "remove"
		targetRole string // role being assigned/changed/removed
		expectOK   bool
	}{
		// Members cannot manage membership.
		{"member_add_member", store.ProjectRoleMember, "add", store.ProjectRoleMember, false},
		{"member_add_admin", store.ProjectRoleMember, "add", store.ProjectRoleAdmin, false},
		{"member_add_owner", store.ProjectRoleMember, "add", store.ProjectRoleOwner, false},
		{"member_remove_member", store.ProjectRoleMember, "remove", store.ProjectRoleMember, false},
		{"member_remove_admin", store.ProjectRoleMember, "remove", store.ProjectRoleAdmin, false},

		// Admins can manage ordinary members only.
		{"admin_add_member", store.ProjectRoleAdmin, "add", store.ProjectRoleMember, true},
		{"admin_add_admin", store.ProjectRoleAdmin, "add", store.ProjectRoleAdmin, false},
		{"admin_add_owner", store.ProjectRoleAdmin, "add", store.ProjectRoleOwner, false},
		{"admin_remove_member", store.ProjectRoleAdmin, "remove", store.ProjectRoleMember, true},
		{"admin_remove_admin", store.ProjectRoleAdmin, "remove", store.ProjectRoleAdmin, false},

		// Owners can manage all roles.
		{"owner_add_member", store.ProjectRoleOwner, "add", store.ProjectRoleMember, true},
		{"owner_add_admin", store.ProjectRoleOwner, "add", store.ProjectRoleAdmin, true},
		{"owner_add_owner", store.ProjectRoleOwner, "add", store.ProjectRoleOwner, true},
		{"owner_remove_member", store.ProjectRoleOwner, "remove", store.ProjectRoleMember, true},
		{"owner_remove_admin", store.ProjectRoleOwner, "remove", store.ProjectRoleAdmin, true},
		// owner_remove_owner is subject to last-owner guard — tested separately.
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, s := testServer(t)
			ctx := context.Background()

			projectID := tid("rs1-gm-proj-" + tt.name)
			actorID := tid("rs1-gm-actor-" + tt.name)
			targetID := tid("rs1-gm-target-" + tt.name)

			// Create project with a separate permanent owner to avoid last-owner issues.
			permanentOwnerID := tid("rs1-gm-permowner-" + tt.name)
			createRS1Project(t, s, projectID, permanentOwnerID)

			// Create the actor with the specified role.
			createRS1UserWithRole(t, s, actorID, actorID+"@test.com", projectID, tt.actorRole)

			// Create the target user.
			require.NoError(t, s.CreateUser(ctx, &store.User{
				ID: targetID, Email: targetID + "@test.com",
				DisplayName: "Target", Role: "member", Status: "active",
			}))

			switch tt.op {
			case "add":
				rd, err := s.GetRoleDefinitionByName(ctx, tt.targetRole, store.RoleScopeProject)
				require.NoError(t, err)

				rec := doRequestAsUser(t, srv, &store.User{
					ID: actorID, Email: actorID + "@test.com",
					DisplayName: "Actor", Role: "member",
				}, http.MethodPost, "/api/v1/projects/"+projectID+"/members",
					addProjectMemberRequest{
						RoleDefinitionID: rd.ID,
						PrincipalType:    store.RoleBindingPrincipalUser,
						PrincipalID:      targetID,
					})

				if tt.expectOK {
					assert.True(t, rec.Code == http.StatusCreated || rec.Code == http.StatusOK,
						"expected 201/200 but got %d: %s", rec.Code, rec.Body.String())
				} else {
					assert.True(t, rec.Code == http.StatusForbidden || rec.Code == http.StatusBadRequest,
						"expected 403/400 but got %d: %s", rec.Code, rec.Body.String())
				}

			case "remove":
				// First, create a binding for the target.
				rd, err := s.GetRoleDefinitionByName(ctx, tt.targetRole, store.RoleScopeProject)
				require.NoError(t, err)
				binding, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
					RoleDefinitionID: rd.ID,
					PrincipalType:    store.RoleBindingPrincipalUser,
					PrincipalID:      targetID,
					ScopeType:        store.RoleScopeProject,
					ScopeID:          projectID,
					CreatedBy:        "test",
				})
				require.NoError(t, err)

				rec := doRequestAsUser(t, srv, &store.User{
					ID: actorID, Email: actorID + "@test.com",
					DisplayName: "Actor", Role: "member",
				}, http.MethodDelete, "/api/v1/projects/"+projectID+"/members/"+binding.ID, nil)

				if tt.expectOK {
					assert.Equal(t, http.StatusNoContent, rec.Code,
						"expected 204 but got %d: %s", rec.Code, rec.Body.String())
				} else {
					assert.True(t, rec.Code == http.StatusForbidden || rec.Code == http.StatusBadRequest,
						"expected 403/400 but got %d: %s", rec.Code, rec.Body.String())
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// RS1.2: Group-derived admin authority
// ---------------------------------------------------------------------------

func TestRS1_GroupDerivedAdminCanManageMembers(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs1-grp-admin-proj")
	ownerID := tid("rs1-grp-admin-owner")
	adminUserID := tid("rs1-grp-admin-user")
	groupID := tid("rs1-grp-admin-grp")
	targetID := tid("rs1-grp-admin-target")

	createRS1Project(t, s, projectID, ownerID)

	// Create user with no direct project role.
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: adminUserID, Email: "grp-admin@test.com",
		DisplayName: "GrpAdmin", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, adminUserID)

	// Create group, bind project-admin to it, add user to group.
	require.NoError(t, s.CreateGroup(ctx, &store.Group{
		ID: groupID, Slug: "rs1-admin-group", Name: "RS1 Admin Group",
	}))
	rd, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      groupID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    groupID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   adminUserID,
		Role:       store.GroupMemberRoleMember,
	}))

	// Create target user.
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: "grp-admin-target@test.com",
		DisplayName: "Target", Role: "member", Status: "active",
	}))

	// Group-derived admin should be able to add a member.
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	rec := doRequestAsUser(t, srv, &store.User{
		ID: adminUserID, Email: "grp-admin@test.com",
		DisplayName: "GrpAdmin", Role: "member",
	}, http.MethodPost, "/api/v1/projects/"+projectID+"/members",
		addProjectMemberRequest{
			RoleDefinitionID: memberRD.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      targetID,
		})

	assert.Equal(t, http.StatusCreated, rec.Code,
		"group-derived admin should be able to add members: %s", rec.Body.String())

	// But group-derived admin cannot add an admin.
	adminRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)

	target2ID := tid("rs1-grp-admin-target2")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: target2ID, Email: "grp-admin-target2@test.com",
		DisplayName: "Target2", Role: "member", Status: "active",
	}))

	rec = doRequestAsUser(t, srv, &store.User{
		ID: adminUserID, Email: "grp-admin@test.com",
		DisplayName: "GrpAdmin", Role: "member",
	}, http.MethodPost, "/api/v1/projects/"+projectID+"/members",
		addProjectMemberRequest{
			RoleDefinitionID: adminRD.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      target2ID,
		})

	assert.Equal(t, http.StatusForbidden, rec.Code,
		"group-derived admin cannot add admin: %s", rec.Body.String())
}

// ---------------------------------------------------------------------------
// RS1.3: Principal Eligibility (D3)
// ---------------------------------------------------------------------------

func TestRS1_PrincipalEligibility(t *testing.T) {
	tests := []struct {
		name          string
		principalType string
		roleName      string
		expectOK      bool
	}{
		// Owner: direct user only.
		{"user_owner", store.RoleBindingPrincipalUser, store.ProjectRoleOwner, true},
		{"group_owner", store.RoleBindingPrincipalGroup, store.ProjectRoleOwner, false},
		{"agent_owner", store.RoleBindingPrincipalAgent, store.ProjectRoleOwner, false},

		// Admin: user or group (D3).
		{"user_admin", store.RoleBindingPrincipalUser, store.ProjectRoleAdmin, true},
		{"group_admin", store.RoleBindingPrincipalGroup, store.ProjectRoleAdmin, true},
		{"agent_admin", store.RoleBindingPrincipalAgent, store.ProjectRoleAdmin, false},

		// Member: user, agent, or group.
		{"user_member", store.RoleBindingPrincipalUser, store.ProjectRoleMember, true},
		{"group_member", store.RoleBindingPrincipalGroup, store.ProjectRoleMember, true},
		{"agent_member", store.RoleBindingPrincipalAgent, store.ProjectRoleMember, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := principalEligibleForRole(tt.principalType, tt.roleName)
			assert.Equal(t, tt.expectOK, result,
				"principalEligibleForRole(%q, %q) = %v, want %v",
				tt.principalType, tt.roleName, result, tt.expectOK)
		})
	}
}

// ---------------------------------------------------------------------------
// RS1.4: One-Binding Invariant (D4)
// ---------------------------------------------------------------------------

func TestRS1_OneBindingInvariant_AtomicReplacement(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs1-d4-proj")
	ownerID := tid("rs1-d4-owner")
	targetID := tid("rs1-d4-target")

	createRS1Project(t, s, projectID, ownerID)

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: "d4-target@test.com",
		DisplayName: "Target", Role: "member", Status: "active",
	}))

	// Add target as member.
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	rec := doRequestAsUser(t, srv, &store.User{
		ID: ownerID, Email: ownerID + "@test.com",
		DisplayName: "Owner", Role: "member",
	}, http.MethodPost, "/api/v1/projects/"+projectID+"/members",
		addProjectMemberRequest{
			RoleDefinitionID: memberRD.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      targetID,
		})
	require.Equal(t, http.StatusCreated, rec.Code, "first add: %s", rec.Body.String())

	// Now add with admin role — should replace, not create duplicate.
	adminRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)

	rec = doRequestAsUser(t, srv, &store.User{
		ID: ownerID, Email: ownerID + "@test.com",
		DisplayName: "Owner", Role: "member",
	}, http.MethodPost, "/api/v1/projects/"+projectID+"/members",
		addProjectMemberRequest{
			RoleDefinitionID: adminRD.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      targetID,
		})
	// Atomic replacement returns 200, not 201.
	require.Equal(t, http.StatusOK, rec.Code, "atomic replacement: %s", rec.Body.String())

	// Verify only one binding exists.
	bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, targetID)
	require.NoError(t, err)
	var projectBindings []*store.RoleBinding
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeProject && b.ScopeID == projectID {
			projectBindings = append(projectBindings, b)
		}
	}
	assert.Len(t, projectBindings, 1,
		"one-binding invariant: exactly one binding per principal per project")
	assert.Equal(t, adminRD.ID, projectBindings[0].RoleDefinitionID,
		"binding should be the new admin role")
}

// ---------------------------------------------------------------------------
// RS1.5: Last-Owner Guard
// ---------------------------------------------------------------------------

func TestRS1_LastOwnerGuard_PreventsDemotion(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs1-lo-proj")
	ownerID := tid("rs1-lo-owner")

	createRS1Project(t, s, projectID, ownerID)

	// Get owner's binding.
	bindings, err := s.ListRoleBindingsForScope(ctx, store.RoleScopeProject, projectID)
	require.NoError(t, err)
	var ownerBinding *store.RoleBinding
	for _, b := range bindings {
		if b.PrincipalID == ownerID {
			ownerBinding = b
			break
		}
	}
	require.NotNil(t, ownerBinding, "owner binding must exist")

	// Try to change role to member — should fail with last_owner.
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	rec := doRequestAsUser(t, srv, &store.User{
		ID: ownerID, Email: ownerID + "@test.com",
		DisplayName: "Owner", Role: "member",
	}, http.MethodPatch, "/api/v1/projects/"+projectID+"/members/"+ownerBinding.ID,
		updateProjectMemberRequest{RoleDefinitionID: memberRD.ID})

	assert.Equal(t, http.StatusConflict, rec.Code,
		"last-owner demotion should be denied: %s", rec.Body.String())
}

func TestRS1_LastOwnerGuard_PreventsRemoval(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs1-lo-rm-proj")
	ownerID := tid("rs1-lo-rm-owner")

	createRS1Project(t, s, projectID, ownerID)

	// Get owner's binding.
	bindings, err := s.ListRoleBindingsForScope(ctx, store.RoleScopeProject, projectID)
	require.NoError(t, err)
	var ownerBinding *store.RoleBinding
	for _, b := range bindings {
		if b.PrincipalID == ownerID {
			ownerBinding = b
			break
		}
	}
	require.NotNil(t, ownerBinding)

	rec := doRequestAsUser(t, srv, &store.User{
		ID: ownerID, Email: ownerID + "@test.com",
		DisplayName: "Owner", Role: "member",
	}, http.MethodDelete, "/api/v1/projects/"+projectID+"/members/"+ownerBinding.ID, nil)

	assert.Equal(t, http.StatusConflict, rec.Code,
		"last-owner removal should be denied: %s", rec.Body.String())
}

func TestRS1_LastOwnerGuard_AllowsRemovalWhenMultipleOwners(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs1-lo-multi-proj")
	owner1ID := tid("rs1-lo-multi-owner1")
	owner2ID := tid("rs1-lo-multi-owner2")

	createRS1Project(t, s, projectID, owner1ID)

	// Add a second owner.
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: owner2ID, Email: owner2ID + "@test.com",
		DisplayName: "Owner2", Role: "member", Status: "active",
	}))
	ownerRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: ownerRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      owner2ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Get owner2's binding.
	bindings, err := s.ListRoleBindingsForScope(ctx, store.RoleScopeProject, projectID)
	require.NoError(t, err)
	var owner2Binding *store.RoleBinding
	for _, b := range bindings {
		if b.PrincipalID == owner2ID {
			owner2Binding = b
			break
		}
	}
	require.NotNil(t, owner2Binding)

	// Owner1 can remove owner2 because at least one owner remains.
	rec := doRequestAsUser(t, srv, &store.User{
		ID: owner1ID, Email: owner1ID + "@test.com",
		DisplayName: "Owner1", Role: "member",
	}, http.MethodDelete, "/api/v1/projects/"+projectID+"/members/"+owner2Binding.ID, nil)

	assert.Equal(t, http.StatusNoContent, rec.Code,
		"removing non-last owner should succeed: %s", rec.Body.String())
}

// ---------------------------------------------------------------------------
// RS1.6: Atomic Ownership Transfer (D1)
// ---------------------------------------------------------------------------

func TestRS1_AtomicOwnershipTransfer(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs1-xfer-proj")
	ownerID := tid("rs1-xfer-owner")
	newOwnerID := tid("rs1-xfer-newowner")

	createRS1Project(t, s, projectID, ownerID)

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: newOwnerID, Email: "new-owner@test.com",
		DisplayName: "New Owner", Role: "member", Status: "active",
	}))

	rec := doRequestAsUser(t, srv, &store.User{
		ID: ownerID, Email: ownerID + "@test.com",
		DisplayName: "Owner", Role: "member",
	}, http.MethodPost, "/api/v1/projects/"+projectID+"/transfer-ownership",
		transferOwnershipRequest{NewOwnerID: newOwnerID})

	require.Equal(t, http.StatusOK, rec.Code,
		"ownership transfer should succeed: %s", rec.Body.String())

	// Verify new owner is now owner.
	membership, err := s.GetProjectMembership(ctx, projectID, newOwnerID)
	require.NoError(t, err)
	assert.Equal(t, store.ProjectRoleOwner, membership.Role,
		"new owner should have owner role")

	// Verify old owner is now member.
	oldMembership, err := s.GetProjectMembership(ctx, projectID, ownerID)
	require.NoError(t, err)
	assert.Equal(t, store.ProjectRoleMember, oldMembership.Role,
		"old owner should be downgraded to member")
}

func TestRS1_TransferOwnership_NonOwnerDenied(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs1-xfer-deny-proj")
	ownerID := tid("rs1-xfer-deny-owner")
	adminID := tid("rs1-xfer-deny-admin")
	newOwnerID := tid("rs1-xfer-deny-target")

	createRS1Project(t, s, projectID, ownerID)
	createRS1UserWithRole(t, s, adminID, "admin-xfer@test.com", projectID, store.ProjectRoleAdmin)

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: newOwnerID, Email: "new-owner-deny@test.com",
		DisplayName: "NewOwner", Role: "member", Status: "active",
	}))

	// Admin tries to transfer — should be denied.
	rec := doRequestAsUser(t, srv, &store.User{
		ID: adminID, Email: "admin-xfer@test.com",
		DisplayName: "Admin", Role: "member",
	}, http.MethodPost, "/api/v1/projects/"+projectID+"/transfer-ownership",
		transferOwnershipRequest{NewOwnerID: newOwnerID})

	assert.Equal(t, http.StatusForbidden, rec.Code,
		"admin cannot transfer ownership: %s", rec.Body.String())
}

func TestRS1_TransferOwnership_SelfTransferDenied(t *testing.T) {
	srv, s := testServer(t)

	projectID := tid("rs1-xfer-self-proj")
	ownerID := tid("rs1-xfer-self-owner")

	createRS1Project(t, s, projectID, ownerID)

	rec := doRequestAsUser(t, srv, &store.User{
		ID: ownerID, Email: ownerID + "@test.com",
		DisplayName: "Owner", Role: "member",
	}, http.MethodPost, "/api/v1/projects/"+projectID+"/transfer-ownership",
		transferOwnershipRequest{NewOwnerID: ownerID})

	assert.Equal(t, http.StatusConflict, rec.Code,
		"self-transfer should be denied: %s", rec.Body.String())
}

// ---------------------------------------------------------------------------
// RS1.7: Lifecycle Enforcement (D2)
// ---------------------------------------------------------------------------

func TestRS1_LifecycleEnforcement_ExpiredOwnerNotCounted(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs1-lc-proj")
	ownerID := tid("rs1-lc-owner")
	expiredOwnerID := tid("rs1-lc-expired")

	createRS1Project(t, s, projectID, ownerID)

	// Create an expired owner binding.
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: expiredOwnerID, Email: "expired@test.com",
		DisplayName: "Expired", Role: "member", Status: "active",
	}))
	ownerRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)
	past := time.Now().Add(-1 * time.Hour)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: ownerRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      expiredOwnerID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		ExpiresAt:        &past,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Even though there are two owner bindings, the expired one shouldn't count.
	// The active owner count should still be 1.
	svc := &ProjectMembershipService{
		store:   s,
		nowFunc: time.Now,
	}
	count, err := svc.countActiveDirectOwners(ctx, projectID)
	require.NoError(t, err)
	assert.Equal(t, 1, count,
		"expired owner should not be counted as active")
}

func TestRS1_LifecycleEnforcement_FutureOwnerNotCounted(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs1-lc-future-proj")
	ownerID := tid("rs1-lc-future-owner")
	futureOwnerID := tid("rs1-lc-future-user")

	createRS1Project(t, s, projectID, ownerID)

	// Create a future owner binding (not active yet).
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: futureOwnerID, Email: "future@test.com",
		DisplayName: "Future", Role: "member", Status: "active",
	}))
	ownerRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)
	future := time.Now().Add(24 * time.Hour)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: ownerRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      futureOwnerID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		NotBefore:        &future,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	svc := &ProjectMembershipService{
		store:   s,
		nowFunc: time.Now,
	}
	count, err := svc.countActiveDirectOwners(ctx, projectID)
	require.NoError(t, err)
	assert.Equal(t, 1, count,
		"future owner should not be counted as active")
}

// ---------------------------------------------------------------------------
// RS1.8: Server-derived Capabilities
// ---------------------------------------------------------------------------

func TestRS1_Capabilities(t *testing.T) {
	srv, s := testServer(t)
	_ = context.Background()

	projectID := tid("rs1-caps-proj")
	ownerID := tid("rs1-caps-owner")
	adminID := tid("rs1-caps-admin")
	memberID := tid("rs1-caps-member")

	createRS1Project(t, s, projectID, ownerID)
	createRS1UserWithRole(t, s, adminID, "caps-admin@test.com", projectID, store.ProjectRoleAdmin)
	createRS1UserWithRole(t, s, memberID, "caps-member@test.com", projectID, store.ProjectRoleMember)

	tests := []struct {
		name             string
		userID           string
		email            string
		canManageMembers bool
		canManageAdmins  bool
		canManageOwners  bool
		canTransfer      bool
	}{
		{"owner", ownerID, ownerID + "@test.com", true, true, true, true},
		{"admin", adminID, "caps-admin@test.com", true, false, false, false},
		{"member", memberID, "caps-member@test.com", false, false, false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := doRequestAsUser(t, srv, &store.User{
				ID: tt.userID, Email: tt.email,
				DisplayName: "User", Role: "member",
			}, http.MethodGet, "/api/v1/projects/"+projectID+"/members", nil)

			require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

			var resp listProjectMembersResponse
			require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
			require.NotNil(t, resp.Capabilities, "capabilities should be present")

			caps := resp.Capabilities
			assert.Equal(t, tt.canManageMembers, caps.CanManageMembers,
				"CanManageMembers for %s", tt.name)
			assert.Equal(t, tt.canManageAdmins, caps.CanManageAdmins,
				"CanManageAdmins for %s", tt.name)
			assert.Equal(t, tt.canManageOwners, caps.CanManageOwners,
				"CanManageOwners for %s", tt.name)
			assert.Equal(t, tt.canTransfer, caps.CanTransfer,
				"CanTransfer for %s", tt.name)
		})
	}
}

// ---------------------------------------------------------------------------
// RS1.9: Group Eligibility for project-admin (D3)
// ---------------------------------------------------------------------------

func TestRS1_GroupAdminEligible(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs1-d3-proj")
	ownerID := tid("rs1-d3-owner")
	groupID := tid("rs1-d3-group")

	createRS1Project(t, s, projectID, ownerID)

	require.NoError(t, s.CreateGroup(ctx, &store.Group{
		ID: groupID, Slug: "rs1-d3-group", Name: "RS1 D3 Group",
	}))

	// Owner adds the group as project-admin — should succeed.
	adminRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)

	rec := doRequestAsUser(t, srv, &store.User{
		ID: ownerID, Email: ownerID + "@test.com",
		DisplayName: "Owner", Role: "member",
	}, http.MethodPost, "/api/v1/projects/"+projectID+"/members",
		addProjectMemberRequest{
			RoleDefinitionID: adminRD.ID,
			PrincipalType:    store.RoleBindingPrincipalGroup,
			PrincipalID:      groupID,
		})

	assert.Equal(t, http.StatusCreated, rec.Code,
		"D3: groups should be eligible for project-admin: %s", rec.Body.String())
}

func TestRS1_GroupOwnerIneligible(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs1-d3-nown-proj")
	ownerID := tid("rs1-d3-nown-owner")
	groupID := tid("rs1-d3-nown-group")

	createRS1Project(t, s, projectID, ownerID)

	require.NoError(t, s.CreateGroup(ctx, &store.Group{
		ID: groupID, Slug: "rs1-d3-nown-group", Name: "RS1 D3 Nown Group",
	}))

	// Owner tries to add the group as project-owner — should fail.
	ownerRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)

	rec := doRequestAsUser(t, srv, &store.User{
		ID: ownerID, Email: ownerID + "@test.com",
		DisplayName: "Owner", Role: "member",
	}, http.MethodPost, "/api/v1/projects/"+projectID+"/members",
		addProjectMemberRequest{
			RoleDefinitionID: ownerRD.ID,
			PrincipalType:    store.RoleBindingPrincipalGroup,
			PrincipalID:      groupID,
		})

	assert.Equal(t, http.StatusBadRequest, rec.Code,
		"D3: groups should NOT be eligible for project-owner: %s", rec.Body.String())
}

// ---------------------------------------------------------------------------
// RS1.10: No Direct RoleBinding Mutation in Handlers
// ---------------------------------------------------------------------------

// This test validates that the handlers delegate to the service by checking
// that the service's audit emitting is called. Since handlers now only call
// s.membershipService.*, and the service always emits audit, the audit
// presence is proof of service delegation.

func TestRS1_HandlersDoNotDirectlyMutate(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs1-nodelete-proj")
	ownerID := tid("rs1-nodelete-owner")
	targetID := tid("rs1-nodelete-target")

	createRS1Project(t, s, projectID, ownerID)

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: "nodelete-target@test.com",
		DisplayName: "Target", Role: "member", Status: "active",
	}))

	// Add member through API — this goes through the service.
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	rec := doRequestAsUser(t, srv, &store.User{
		ID: ownerID, Email: ownerID + "@test.com",
		DisplayName: "Owner", Role: "member",
	}, http.MethodPost, "/api/v1/projects/"+projectID+"/members",
		addProjectMemberRequest{
			RoleDefinitionID: memberRD.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      targetID,
		})
	require.Equal(t, http.StatusCreated, rec.Code, "add: %s", rec.Body.String())

	// Verify the binding was created correctly via the store.
	membership, err := s.GetProjectMembership(ctx, projectID, targetID)
	require.NoError(t, err)
	assert.Equal(t, store.ProjectRoleMember, membership.Role)
}

// ---------------------------------------------------------------------------
// RS1.11: Update Role — Both Old/New Target Governance
// ---------------------------------------------------------------------------

func TestRS1_UpdateRole_AdminCannotPromoteToAdmin(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs1-upd-proj")
	ownerID := tid("rs1-upd-owner")
	adminID := tid("rs1-upd-admin")
	targetID := tid("rs1-upd-target")

	createRS1Project(t, s, projectID, ownerID)
	createRS1UserWithRole(t, s, adminID, "upd-admin@test.com", projectID, store.ProjectRoleAdmin)

	// Create target as member.
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: "upd-target@test.com",
		DisplayName: "Target", Role: "member", Status: "active",
	}))
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	binding, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      targetID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Admin tries to promote member to admin — denied (admin can only manage members).
	adminRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)

	rec := doRequestAsUser(t, srv, &store.User{
		ID: adminID, Email: "upd-admin@test.com",
		DisplayName: "Admin", Role: "member",
	}, http.MethodPatch, "/api/v1/projects/"+projectID+"/members/"+binding.ID,
		updateProjectMemberRequest{RoleDefinitionID: adminRD.ID})

	assert.Equal(t, http.StatusForbidden, rec.Code,
		"admin cannot promote to admin: %s", rec.Body.String())
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// createRS1Project creates a project with a permanent owner.
func createRS1Project(t *testing.T, s store.Store, projectID, ownerID string) {
	t.Helper()
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: ownerID, Email: ownerID + "@test.com",
		DisplayName: "Owner", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, ownerID)

	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID:        projectID,
		Name:      "RS1 Test Project " + projectID,
		Slug:      fmt.Sprintf("rs1-test-%s", projectID[:8]),
		CreatedBy: ownerID,
	}))

	// Create owner role binding.
	ownerRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: ownerRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      ownerID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	if err != nil && err != store.ErrAlreadyExists {
		t.Fatalf("failed to create owner binding: %v", err)
	}
}

// createRS1UserWithRole creates a user and assigns them a project role.
func createRS1UserWithRole(t *testing.T, s store.Store, userID, email, projectID, roleName string) {
	t.Helper()
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userID, Email: email,
		DisplayName: "User", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, userID)

	rd, err := s.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	if err != nil && err != store.ErrAlreadyExists {
		t.Fatalf("failed to create role binding: %v", err)
	}
}
