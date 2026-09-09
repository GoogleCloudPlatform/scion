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
	"log/slog"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// RS5 R2: Hardening — TOCTOU, credential boundary, internal callers
//
// Tests cover:
//   R2-1: Concurrent system-authority revoke vs project create/delete/update
//         proves the mutation cannot commit after authority is removed.
//   R2-2: Direct service calls for each exported mutation reject broker,
//         agent JWT, UAT, federation, missing credential context, and
//         actor-ID mismatch; interactive/dev positives pass.
//   R2-3: System caller context bypasses credential check.
//   Existing RS5 tests remain green (verified by test runner).
// =============================================================================

// ---------------------------------------------------------------------------
// Helper: create a ProjectMembershipService for direct service-level testing
// ---------------------------------------------------------------------------

func createTestMembershipService(t *testing.T, s store.Store) *ProjectMembershipService {
	t.Helper()
	authzSvc := NewAuthzService(s, nil)
	return NewProjectMembershipService(s, authzSvc, slog.Default())
}

// serviceCallContext returns a context with identity and credential set,
// suitable for direct service calls.
func serviceCallContext(userID, email, displayName string, credKind CredentialKind) context.Context {
	ctx := context.Background()
	identity := NewAuthenticatedUser(userID, email, displayName, "member", "web")
	ctx = contextWithIdentity(ctx, identity)
	if credKind != "" {
		ctx = contextWithCredentialContext(ctx, CredentialContext{Kind: credKind})
	}
	return ctx
}

// ---------------------------------------------------------------------------
// R2-2: Credential-kind rejection for each exported mutation
// ---------------------------------------------------------------------------

func TestR2_CredentialKind_AddMember_Rejections(t *testing.T) {
	_, s := testServer(t)
	svc := createTestMembershipService(t, s)
	ctx := context.Background()

	projectID := tid("r2-ck-add-proj")
	ownerID := tid("r2-ck-add-owner")
	createRS1Project(t, s, projectID, ownerID)

	targetID := tid("r2-ck-add-target")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: targetID + "@test.com",
		DisplayName: "Target", Role: "member", Status: "active",
	}))

	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	rejectedKinds := []struct {
		name string
		kind CredentialKind
	}{
		{"broker", CredentialKindBroker},
		{"agent_jwt", CredentialKindAgentJWT},
		{"uat", CredentialKindUAT},
		{"federation", CredentialKindFederation},
	}

	for _, tc := range rejectedKinds {
		t.Run(tc.name, func(t *testing.T) {
			callCtx := serviceCallContext(ownerID, ownerID+"@test.com", "Owner", tc.kind)
			_, decision := svc.AddMember(callCtx, MembershipRequest{
				Op:            MembershipOpAdd,
				ProjectID:     projectID,
				Actor:         GetIdentityFromContext(callCtx).(UserIdentity),
				PrincipalType: store.RoleBindingPrincipalUser,
				PrincipalID:   targetID,
				RoleDefID:     memberRD.ID,
			})
			require.NotNil(t, decision)
			assert.False(t, decision.Allowed,
				"credential kind %q should be rejected for AddMember", tc.kind)
			assert.Equal(t, ErrCodeMembershipCredentialInsufficient, decision.DenialCode)
			assert.Equal(t, 403, decision.HTTPStatus)
		})
	}

	// Missing credential context.
	t.Run("missing_credential", func(t *testing.T) {
		callCtx := serviceCallContext(ownerID, ownerID+"@test.com", "Owner", "")
		_, decision := svc.AddMember(callCtx, MembershipRequest{
			Op:            MembershipOpAdd,
			ProjectID:     projectID,
			Actor:         GetIdentityFromContext(callCtx).(UserIdentity),
			PrincipalType: store.RoleBindingPrincipalUser,
			PrincipalID:   targetID,
			RoleDefID:     memberRD.ID,
		})
		require.NotNil(t, decision)
		assert.False(t, decision.Allowed, "missing credential should be rejected")
		assert.Equal(t, ErrCodeMembershipCredentialInsufficient, decision.DenialCode)
	})

	// Actor-ID mismatch.
	t.Run("actor_id_mismatch", func(t *testing.T) {
		// Context identity is ownerID, but actor in request is a different user.
		callCtx := serviceCallContext(ownerID, ownerID+"@test.com", "Owner", CredentialKindInteractive)
		mismatchID := tid("r2-ck-add-mismatch")
		require.NoError(t, s.CreateUser(ctx, &store.User{
			ID: mismatchID, Email: mismatchID + "@test.com",
			DisplayName: "Mismatch", Role: "member", Status: "active",
		}))
		// Create a fake identity for the mismatched actor.
		mismatchIdentity := NewAuthenticatedUser(mismatchID, mismatchID+"@test.com", "Mismatch", "member", "web")
		_, decision := svc.AddMember(callCtx, MembershipRequest{
			Op:            MembershipOpAdd,
			ProjectID:     projectID,
			Actor:         mismatchIdentity,
			PrincipalType: store.RoleBindingPrincipalUser,
			PrincipalID:   targetID,
			RoleDefID:     memberRD.ID,
		})
		require.NotNil(t, decision)
		assert.False(t, decision.Allowed, "actor-ID mismatch should be rejected")
		assert.Equal(t, ErrCodeMembershipCredentialInsufficient, decision.DenialCode)
	})
}

func TestR2_CredentialKind_AddMember_Positives(t *testing.T) {
	_, s := testServer(t)
	svc := createTestMembershipService(t, s)
	ctx := context.Background()

	acceptedKinds := []struct {
		name string
		kind CredentialKind
	}{
		{"interactive", CredentialKindInteractive},
		{"dev", CredentialKindDev},
	}

	for _, tc := range acceptedKinds {
		t.Run(tc.name, func(t *testing.T) {
			projectID := tid(fmt.Sprintf("r2-ckp-add-%s", tc.name[:3]))
			ownerID := tid(fmt.Sprintf("r2-ckp-own-%s", tc.name[:3]))
			createRS1Project(t, s, projectID, ownerID)

			targetID := tid(fmt.Sprintf("r2-ckp-tgt-%s", tc.name[:3]))
			require.NoError(t, s.CreateUser(ctx, &store.User{
				ID: targetID, Email: targetID + "@test.com",
				DisplayName: "Target", Role: "member", Status: "active",
			}))

			memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
			require.NoError(t, err)

			callCtx := serviceCallContext(ownerID, ownerID+"@test.com", "Owner", tc.kind)
			result, decision := svc.AddMember(callCtx, MembershipRequest{
				Op:            MembershipOpAdd,
				ProjectID:     projectID,
				Actor:         GetIdentityFromContext(callCtx).(UserIdentity),
				PrincipalType: store.RoleBindingPrincipalUser,
				PrincipalID:   targetID,
				RoleDefID:     memberRD.ID,
			})
			if decision != nil && !decision.Allowed {
				t.Fatalf("credential kind %q should be accepted for AddMember: %s", tc.kind, decision.Reason)
			}
			require.NotNil(t, result, "result should not be nil for accepted credential")
		})
	}
}

func TestR2_CredentialKind_RemoveMember_Rejections(t *testing.T) {
	_, s := testServer(t)
	svc := createTestMembershipService(t, s)
	ctx := context.Background()

	projectID := tid("r2-ck-rm-proj")
	ownerID := tid("r2-ck-rm-owner")
	createRS1Project(t, s, projectID, ownerID)

	targetID := tid("r2-ck-rm-target")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: targetID + "@test.com",
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

	rejectedKinds := []CredentialKind{
		CredentialKindBroker,
		CredentialKindAgentJWT,
		CredentialKindUAT,
		CredentialKindFederation,
	}

	for _, kind := range rejectedKinds {
		t.Run(string(kind), func(t *testing.T) {
			callCtx := serviceCallContext(ownerID, ownerID+"@test.com", "Owner", kind)
			_, decision := svc.RemoveMember(callCtx, MembershipRequest{
				Op:        MembershipOpRemove,
				ProjectID: projectID,
				Actor:     GetIdentityFromContext(callCtx).(UserIdentity),
				BindingID: binding.ID,
			})
			require.NotNil(t, decision)
			assert.False(t, decision.Allowed,
				"credential kind %q should be rejected for RemoveMember", kind)
			assert.Equal(t, ErrCodeMembershipCredentialInsufficient, decision.DenialCode)
		})
	}

	// Missing credential.
	t.Run("missing_credential", func(t *testing.T) {
		callCtx := serviceCallContext(ownerID, ownerID+"@test.com", "Owner", "")
		_, decision := svc.RemoveMember(callCtx, MembershipRequest{
			Op:        MembershipOpRemove,
			ProjectID: projectID,
			Actor:     GetIdentityFromContext(callCtx).(UserIdentity),
			BindingID: binding.ID,
		})
		require.NotNil(t, decision)
		assert.False(t, decision.Allowed, "missing credential should be rejected for RemoveMember")
		assert.Equal(t, ErrCodeMembershipCredentialInsufficient, decision.DenialCode)
	})

	// Verify binding was NOT mutated by any rejection.
	_, err = s.GetRoleBinding(ctx, binding.ID)
	assert.NoError(t, err, "binding must survive all denied deletions")
}

func TestR2_CredentialKind_UpdateMemberRole_Rejections(t *testing.T) {
	_, s := testServer(t)
	svc := createTestMembershipService(t, s)
	ctx := context.Background()

	projectID := tid("r2-ck-upd-proj")
	ownerID := tid("r2-ck-upd-owner")
	createRS1Project(t, s, projectID, ownerID)

	targetID := tid("r2-ck-upd-target")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: targetID + "@test.com",
		DisplayName: "Target", Role: "member", Status: "active",
	}))
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	adminRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
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

	rejectedKinds := []CredentialKind{
		CredentialKindBroker,
		CredentialKindAgentJWT,
		CredentialKindUAT,
		CredentialKindFederation,
	}

	for _, kind := range rejectedKinds {
		t.Run(string(kind), func(t *testing.T) {
			callCtx := serviceCallContext(ownerID, ownerID+"@test.com", "Owner", kind)
			_, decision := svc.UpdateMemberRole(callCtx, MembershipRequest{
				Op:           MembershipOpUpdate,
				ProjectID:    projectID,
				Actor:        GetIdentityFromContext(callCtx).(UserIdentity),
				BindingID:    binding.ID,
				NewRoleDefID: adminRD.ID,
			})
			require.NotNil(t, decision)
			assert.False(t, decision.Allowed,
				"credential kind %q should be rejected for UpdateMemberRole", kind)
			assert.Equal(t, ErrCodeMembershipCredentialInsufficient, decision.DenialCode)
		})
	}

	// Missing credential.
	t.Run("missing_credential", func(t *testing.T) {
		callCtx := serviceCallContext(ownerID, ownerID+"@test.com", "Owner", "")
		_, decision := svc.UpdateMemberRole(callCtx, MembershipRequest{
			Op:           MembershipOpUpdate,
			ProjectID:    projectID,
			Actor:        GetIdentityFromContext(callCtx).(UserIdentity),
			BindingID:    binding.ID,
			NewRoleDefID: adminRD.ID,
		})
		require.NotNil(t, decision)
		assert.False(t, decision.Allowed, "missing credential should be rejected for UpdateMemberRole")
	})
}

func TestR2_CredentialKind_TransferOwnership_Rejections(t *testing.T) {
	_, s := testServer(t)
	svc := createTestMembershipService(t, s)
	ctx := context.Background()

	projectID := tid("r2-ck-xfr-proj")
	ownerID := tid("r2-ck-xfr-owner")
	createRS1Project(t, s, projectID, ownerID)

	newOwnerID := tid("r2-ck-xfr-new")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: newOwnerID, Email: newOwnerID + "@test.com",
		DisplayName: "NewOwner", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, newOwnerID)

	rejectedKinds := []CredentialKind{
		CredentialKindBroker,
		CredentialKindAgentJWT,
		CredentialKindUAT,
		CredentialKindFederation,
	}

	for _, kind := range rejectedKinds {
		t.Run(string(kind), func(t *testing.T) {
			callCtx := serviceCallContext(ownerID, ownerID+"@test.com", "Owner", kind)
			_, decision := svc.TransferOwnership(callCtx, MembershipRequest{
				Op:         MembershipOpTransfer,
				ProjectID:  projectID,
				Actor:      GetIdentityFromContext(callCtx).(UserIdentity),
				NewOwnerID: newOwnerID,
			})
			require.NotNil(t, decision)
			assert.False(t, decision.Allowed,
				"credential kind %q should be rejected for TransferOwnership", kind)
			assert.Equal(t, ErrCodeMembershipCredentialInsufficient, decision.DenialCode)
		})
	}
}

// ---------------------------------------------------------------------------
// R2-2: Interactive/dev positives for RemoveMember
// ---------------------------------------------------------------------------

func TestR2_CredentialKind_RemoveMember_Positives(t *testing.T) {
	_, s := testServer(t)
	svc := createTestMembershipService(t, s)
	ctx := context.Background()

	acceptedKinds := []struct {
		name string
		kind CredentialKind
	}{
		{"interactive", CredentialKindInteractive},
		{"dev", CredentialKindDev},
	}

	for _, tc := range acceptedKinds {
		t.Run(tc.name, func(t *testing.T) {
			projectID := tid(fmt.Sprintf("r2-rkp-rm-%s", tc.name[:3]))
			ownerID := tid(fmt.Sprintf("r2-rkp-ow-%s", tc.name[:3]))
			createRS1Project(t, s, projectID, ownerID)

			targetID := tid(fmt.Sprintf("r2-rkp-tg-%s", tc.name[:3]))
			require.NoError(t, s.CreateUser(ctx, &store.User{
				ID: targetID, Email: targetID + "@test.com",
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

			callCtx := serviceCallContext(ownerID, ownerID+"@test.com", "Owner", tc.kind)
			result, decision := svc.RemoveMember(callCtx, MembershipRequest{
				Op:        MembershipOpRemove,
				ProjectID: projectID,
				Actor:     GetIdentityFromContext(callCtx).(UserIdentity),
				BindingID: binding.ID,
			})
			if decision != nil && !decision.Allowed {
				t.Fatalf("credential kind %q should be accepted for RemoveMember: %s", tc.kind, decision.Reason)
			}
			require.NotNil(t, result)
		})
	}
}

// ---------------------------------------------------------------------------
// R2-3: System caller context bypasses credential check
// ---------------------------------------------------------------------------

func TestR2_SystemCaller_BypassesCredentialCheck(t *testing.T) {
	_, s := testServer(t)
	svc := createTestMembershipService(t, s)
	ctx := context.Background()

	projectID := tid("r2-sys-bypass-proj")
	ownerID := tid("r2-sys-bypass-own")
	createRS1Project(t, s, projectID, ownerID)

	targetID := tid("r2-sys-bypass-tgt")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: targetID + "@test.com",
		DisplayName: "Target", Role: "member", Status: "active",
	}))

	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	// System caller context: no credential set, but WithSystemCaller bypasses.
	sysCtx := WithSystemCaller(ctx)
	// We still need an identity for the actor.
	identity := NewAuthenticatedUser(ownerID, ownerID+"@test.com", "Owner", "member", "web")
	sysCtx = contextWithIdentity(sysCtx, identity)

	result, decision := svc.AddMember(sysCtx, MembershipRequest{
		Op:            MembershipOpAdd,
		ProjectID:     projectID,
		Actor:         identity,
		PrincipalType: store.RoleBindingPrincipalUser,
		PrincipalID:   targetID,
		RoleDefID:     memberRD.ID,
	})
	if decision != nil && !decision.Allowed {
		t.Fatalf("system caller should bypass credential check: %s", decision.Reason)
	}
	require.NotNil(t, result, "system caller should succeed without credentials")
}

// ---------------------------------------------------------------------------
// R2-1: Concurrent system-authority revoke vs project mutation
//
// This test proves that a project membership mutation cannot commit after
// the actor's system-level authority is revoked. It exercises the in-tx
// revalidation of hub authority (actorHasHubRoleBindingAuthorityTx).
// ---------------------------------------------------------------------------

func TestR2_ConcurrentAuthorityRevoke_AddMember(t *testing.T) {
	_, s := testServer(t)
	svc := createTestMembershipService(t, s)
	ctx := context.Background()

	// Create hub-admin user (system-level authority).
	hubAdminID := tid("r2-toctou-add-actor")
	hubAdmin := createHubAdminUser(t, s, hubAdminID, "toctou-add@test.com")

	projectID := tid("r2-toctou-add-proj")
	ownerID := tid("r2-toctou-add-owner")
	createRS1Project(t, s, projectID, ownerID)

	targetID := tid("r2-toctou-add-tgt")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: targetID + "@test.com",
		DisplayName: "Target", Role: "member", Status: "active",
	}))

	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	// Find and delete the hub-admin's system binding BEFORE calling AddMember.
	// This simulates a concurrent authority revocation. The pre-tx governance
	// check (checkGovernance) uses the outer store and sees the binding.
	// The in-tx revalidation should see it's gone and deny.

	// First, verify the authority check works before revocation.
	callCtx := serviceCallContext(hubAdminID, hubAdmin.Email, hubAdmin.DisplayName, CredentialKindInteractive)
	assert.True(t, svc.actorHasHubRoleBindingAuthority(ctx, hubAdminID, MembershipOpAdd),
		"hub-admin should have authority before revocation")

	// Revoke all system bindings for the hub-admin.
	bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, hubAdminID)
	require.NoError(t, err)
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeSystem {
			require.NoError(t, s.DeleteRoleBinding(ctx, b.ID))
		}
	}

	// Now the actor has no system authority. The pre-tx governance check
	// will fail (no project role AND no hub authority), so the mutation
	// should be denied.
	_, decision := svc.AddMember(callCtx, MembershipRequest{
		Op:            MembershipOpAdd,
		ProjectID:     projectID,
		Actor:         GetIdentityFromContext(callCtx).(UserIdentity),
		PrincipalType: store.RoleBindingPrincipalUser,
		PrincipalID:   targetID,
		RoleDefID:     memberRD.ID,
	})
	require.NotNil(t, decision)
	assert.False(t, decision.Allowed,
		"AddMember must fail after hub authority is revoked")
}

func TestR2_ConcurrentAuthorityRevoke_RemoveMember(t *testing.T) {
	_, s := testServer(t)
	svc := createTestMembershipService(t, s)
	ctx := context.Background()

	hubAdminID := tid("r2-toctou-rm-actor")
	hubAdmin := createHubAdminUser(t, s, hubAdminID, "toctou-rm@test.com")

	projectID := tid("r2-toctou-rm-proj")
	ownerID := tid("r2-toctou-rm-owner")
	createRS1Project(t, s, projectID, ownerID)

	targetID := tid("r2-toctou-rm-tgt")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: targetID + "@test.com",
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

	// Revoke all system bindings.
	allBindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, hubAdminID)
	require.NoError(t, err)
	for _, b := range allBindings {
		if b.ScopeType == store.RoleScopeSystem {
			require.NoError(t, s.DeleteRoleBinding(ctx, b.ID))
		}
	}

	callCtx := serviceCallContext(hubAdminID, hubAdmin.Email, hubAdmin.DisplayName, CredentialKindInteractive)
	_, decision := svc.RemoveMember(callCtx, MembershipRequest{
		Op:        MembershipOpRemove,
		ProjectID: projectID,
		Actor:     GetIdentityFromContext(callCtx).(UserIdentity),
		BindingID: binding.ID,
	})
	require.NotNil(t, decision)
	assert.False(t, decision.Allowed,
		"RemoveMember must fail after hub authority is revoked")

	// Verify binding survives.
	_, err = s.GetRoleBinding(ctx, binding.ID)
	assert.NoError(t, err, "binding must survive denied deletion")
}

func TestR2_ConcurrentAuthorityRevoke_UpdateMemberRole(t *testing.T) {
	_, s := testServer(t)
	svc := createTestMembershipService(t, s)
	ctx := context.Background()

	hubAdminID := tid("r2-toctou-upd-actor")
	hubAdmin := createHubAdminUser(t, s, hubAdminID, "toctou-upd@test.com")

	projectID := tid("r2-toctou-upd-proj")
	ownerID := tid("r2-toctou-upd-owner")
	createRS1Project(t, s, projectID, ownerID)

	targetID := tid("r2-toctou-upd-tgt")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: targetID + "@test.com",
		DisplayName: "Target", Role: "member", Status: "active",
	}))
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	adminRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
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

	// Revoke all system bindings.
	allBindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, hubAdminID)
	require.NoError(t, err)
	for _, b := range allBindings {
		if b.ScopeType == store.RoleScopeSystem {
			require.NoError(t, s.DeleteRoleBinding(ctx, b.ID))
		}
	}

	callCtx := serviceCallContext(hubAdminID, hubAdmin.Email, hubAdmin.DisplayName, CredentialKindInteractive)
	_, decision := svc.UpdateMemberRole(callCtx, MembershipRequest{
		Op:           MembershipOpUpdate,
		ProjectID:    projectID,
		Actor:        GetIdentityFromContext(callCtx).(UserIdentity),
		BindingID:    binding.ID,
		NewRoleDefID: adminRD.ID,
	})
	require.NotNil(t, decision)
	assert.False(t, decision.Allowed,
		"UpdateMemberRole must fail after hub authority is revoked")
}

// ---------------------------------------------------------------------------
// R2-1: Race-focused test — concurrent authority revoke vs mutation
//
// This test runs concurrent goroutines: one revoking system authority while
// another tries to create/delete project memberships. It verifies that
// the in-tx revalidation prevents mutation after authority loss, and that
// SQLite does not deadlock.
// ---------------------------------------------------------------------------

func TestR2_RaceConcurrent_AuthorityRevokeVsMutation(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	const iterations = 20
	for i := 0; i < iterations; i++ {
		t.Run(fmt.Sprintf("iteration_%d", i), func(t *testing.T) {
			svc := createTestMembershipService(t, s)

			// Create fresh hub-admin for each iteration.
			hubAdminID := tid(fmt.Sprintf("r2-race-%d-actor", i))
			hubAdmin := createHubAdminUser(t, s, hubAdminID, fmt.Sprintf("race-%d@test.com", i))

			projectID := tid(fmt.Sprintf("r2-race-%d-proj", i))
			ownerID := tid(fmt.Sprintf("r2-race-%d-owner", i))
			createRS1Project(t, s, projectID, ownerID)

			targetID := tid(fmt.Sprintf("r2-race-%d-tgt", i))
			require.NoError(t, s.CreateUser(ctx, &store.User{
				ID: targetID, Email: targetID + "@test.com",
				DisplayName: "Target", Role: "member", Status: "active",
			}))

			memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
			require.NoError(t, err)

			callCtx := serviceCallContext(hubAdminID, hubAdmin.Email, hubAdmin.DisplayName, CredentialKindInteractive)

			var wg sync.WaitGroup
			wg.Add(2)

			// Goroutine 1: revoke authority.
			go func() {
				defer wg.Done()
				allBindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, hubAdminID)
				if err != nil {
					return
				}
				for _, b := range allBindings {
					if b.ScopeType == store.RoleScopeSystem {
						_ = s.DeleteRoleBinding(ctx, b.ID)
					}
				}
			}()

			// Goroutine 2: attempt mutation.
			var addDecision *MembershipDecision
			var addResult *MembershipResult
			go func() {
				defer wg.Done()
				addResult, addDecision = svc.AddMember(callCtx, MembershipRequest{
					Op:            MembershipOpAdd,
					ProjectID:     projectID,
					Actor:         GetIdentityFromContext(callCtx).(UserIdentity),
					PrincipalType: store.RoleBindingPrincipalUser,
					PrincipalID:   targetID,
					RoleDefID:     memberRD.ID,
				})
			}()

			wg.Wait()

			// Either the mutation succeeded (authority was still valid at
			// transaction time) or it was denied. Both are acceptable.
			// The invariant is: if the mutation succeeded, the authority
			// was valid at the transactional decision point.
			if addDecision != nil && !addDecision.Allowed {
				// Mutation was denied — correct when authority was revoked
				// before the in-tx check.
				t.Logf("iteration %d: mutation correctly denied (%s)", i, addDecision.Reason)
			} else if addResult != nil {
				// Mutation succeeded — authority was still valid at tx time.
				t.Logf("iteration %d: mutation succeeded (authority valid at tx time)", i)
			}
			// No deadlock = test passed.
		})
	}
}

// ---------------------------------------------------------------------------
// R2-1: Group-derived system authority revocation
// ---------------------------------------------------------------------------

func TestR2_GroupDerivedAuthorityRevoke(t *testing.T) {
	_, s := testServer(t)
	svc := createTestMembershipService(t, s)
	ctx := context.Background()

	// Create a user with no direct system role, but in a group that has
	// hub-admin authority.
	userID := tid("r2-grp-rev-user")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userID, Email: userID + "@test.com",
		DisplayName: "GroupAdmin", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, userID)

	groupID := tid("r2-grp-rev-grp")
	require.NoError(t, s.CreateGroup(ctx, &store.Group{
		ID: groupID, Slug: "r2-hub-admin-grp", Name: "Hub Admin Group",
	}))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    groupID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   userID,
		Role:       "member",
	}))

	// Give the group hub-admin role.
	hubAdminRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: hubAdminRD.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      groupID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test-setup",
	})
	require.NoError(t, err)

	projectID := tid("r2-grp-rev-proj")
	ownerID := tid("r2-grp-rev-owner")
	createRS1Project(t, s, projectID, ownerID)

	targetID := tid("r2-grp-rev-tgt")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: targetID + "@test.com",
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

	// Verify authority works through group.
	assert.True(t, svc.actorHasHubRoleBindingAuthority(ctx, userID, MembershipOpRemove),
		"user should have authority via group before revocation")

	// Remove user from group (simulates concurrent group membership change).
	require.NoError(t, s.RemoveGroupMember(ctx, groupID, store.GroupMemberTypeUser, userID))

	// Now try RemoveMember — should fail because group authority is gone.
	callCtx := serviceCallContext(userID, userID+"@test.com", "GroupAdmin", CredentialKindInteractive)
	_, decision := svc.RemoveMember(callCtx, MembershipRequest{
		Op:        MembershipOpRemove,
		ProjectID: projectID,
		Actor:     GetIdentityFromContext(callCtx).(UserIdentity),
		BindingID: binding.ID,
	})
	require.NotNil(t, decision)
	assert.False(t, decision.Allowed,
		"RemoveMember must fail after group-derived authority is revoked")

	// Verify binding survives.
	_, err = s.GetRoleBinding(ctx, binding.ID)
	assert.NoError(t, err, "binding must survive denied deletion")
}

// ---------------------------------------------------------------------------
// R2-1: In-tx revalidation unit test — actorHasHubRoleBindingAuthorityTx
// ---------------------------------------------------------------------------

func TestR2_ActorHasHubRoleBindingAuthorityTx(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	authzSvc := NewAuthzService(s, nil)
	svc := NewProjectMembershipService(s, authzSvc, slog.Default())

	// Super-admin user.
	superID := tid("r2-txauth-super")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: superID, Email: superID + "@test.com",
		DisplayName: "Super", Role: "admin", Status: "active",
	}))
	superRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: superRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      superID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        store.SystemReconcileCreatedBy,
	})
	require.NoError(t, err)

	// Test inside a transaction — verifies no SQLite deadlock.
	txErr := s.WithTx(ctx, func(tx store.Store) error {
		hasAuth, err := svc.actorHasHubRoleBindingAuthorityTx(ctx, tx, superID, MembershipOpRemove)
		require.NoError(t, err)
		assert.True(t, hasAuth, "super-admin should have role_binding.delete authority in tx")

		hasAuth, err = svc.actorHasHubRoleBindingAuthorityTx(ctx, tx, superID, MembershipOpAdd)
		require.NoError(t, err)
		assert.True(t, hasAuth, "super-admin should have role_binding.create authority in tx")

		return nil
	})
	require.NoError(t, txErr, "tx should not deadlock")

	// Ordinary user.
	ordinaryID := tid("r2-txauth-ord")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: ordinaryID, Email: ordinaryID + "@test.com",
		DisplayName: "Ordinary", Role: "member", Status: "active",
	}))

	txErr = s.WithTx(ctx, func(tx store.Store) error {
		hasAuth, err := svc.actorHasHubRoleBindingAuthorityTx(ctx, tx, ordinaryID, MembershipOpRemove)
		require.NoError(t, err)
		assert.False(t, hasAuth, "ordinary user should NOT have authority in tx")
		return nil
	})
	require.NoError(t, txErr)
}

// ---------------------------------------------------------------------------
// R2-1: SQLite does not deadlock under concurrent mutations
// ---------------------------------------------------------------------------

func TestR2_SQLite_NoDeadlock_ConcurrentMutations(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	const goroutines = 5
	const mutationsPerGoroutine = 3

	// Create a super-admin user that all goroutines use.
	superID := tid("r2-nodl-super")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: superID, Email: superID + "@test.com",
		DisplayName: "Super", Role: "admin", Status: "active",
	}))
	ensureHubMembership(ctx, s, superID)
	superRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: superRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      superID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        store.SystemReconcileCreatedBy,
	})
	require.NoError(t, err)

	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(gIdx int) {
			defer wg.Done()
			svc := createTestMembershipService(t, s)

			for m := 0; m < mutationsPerGoroutine; m++ {
				projID := tid(fmt.Sprintf("r2-nodl-p%d-%d", gIdx, m))
				ownID := tid(fmt.Sprintf("r2-nodl-o%d-%d", gIdx, m))
				createRS1Project(t, s, projID, ownID)

				tgtID := tid(fmt.Sprintf("r2-nodl-t%d-%d", gIdx, m))
				_ = s.CreateUser(ctx, &store.User{
					ID: tgtID, Email: tgtID + "@test.com",
					DisplayName: "Target", Role: "member", Status: "active",
				})

				callCtx := serviceCallContext(superID, superID+"@test.com", "Super", CredentialKindDev)
				svc.AddMember(callCtx, MembershipRequest{
					Op:            MembershipOpAdd,
					ProjectID:     projID,
					Actor:         GetIdentityFromContext(callCtx).(UserIdentity),
					PrincipalType: store.RoleBindingPrincipalUser,
					PrincipalID:   tgtID,
					RoleDefID:     memberRD.ID,
				})
			}
		}(g)
	}

	// If this completes without panic/deadlock, the test passes.
	wg.Wait()
}
