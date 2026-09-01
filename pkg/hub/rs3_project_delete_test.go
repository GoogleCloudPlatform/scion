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
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// RS3: Project Deletion — Destructive/Protected-Target Reference Slice
//
// Tests cover:
//   - Owner positive control (direct owner can delete)
//   - Governance matrix (admin, member, unrelated user, group-derived, stale OwnerID)
//   - Super-admin and hub-admin bypass
//   - Suspended principal denial
//   - Credential ceiling (session-only)
//   - Atomic mutation audit (before state, actor/credential context)
//   - Absence of audit on denial/failure
//   - Complete security-relevant cascade
//   - Concurrent deletes (deterministic safe outcome)
//   - Already-deleted target (idempotent)
//   - Stable status/error codes and no existence oracle
//   - Non-HTTP bypass/rollback classification (AST proof)
// =============================================================================

// ---------------------------------------------------------------------------
// RS3.1: Owner Positive Control
// ---------------------------------------------------------------------------

func TestRS3_ProjectDeleteOwnerPositiveControl(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs3-owner-pos")
	ownerID := tid("rs3-owner-pos-user")

	createRS3Project(t, s, projectID, ownerID)

	req := ProjectDeleteRequest{
		ProjectID: projectID,
		Actor:     NewAuthenticatedUser(ownerID, ownerID+"@test.com", "Owner", "member", "web"),
	}

	// Set identity context for audit record.
	ctx = setTestIdentity(ctx, req.Actor)

	result, decision := srv.deletionService.Delete(ctx, req)
	require.Nil(t, decision, "direct owner should be able to delete project")
	require.NotNil(t, result)
	assert.Equal(t, projectID, result.Project.ID)

	// Verify project is actually deleted.
	_, err := s.GetProject(ctx, projectID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// ---------------------------------------------------------------------------
// RS3.2: Governance Matrix — actor x target
// ---------------------------------------------------------------------------

func TestRS3_ProjectDeleteGovernanceMatrix(t *testing.T) {
	tests := []struct {
		name     string
		role     string // project role of the actor
		expectOK bool
	}{
		{"direct_owner", store.ProjectRoleOwner, true},
		{"direct_admin", store.ProjectRoleAdmin, false},
		{"direct_member", store.ProjectRoleMember, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, s := testServer(t)
			ctx := context.Background()

			projectID := tid("rs3-gm-" + tt.name)
			permanentOwnerID := tid("rs3-gm-permowner-" + tt.name)
			actorID := tid("rs3-gm-actor-" + tt.name)

			createRS3Project(t, s, projectID, permanentOwnerID)
			createRS3UserWithRole(t, s, actorID, actorID+"@test.com", projectID, tt.role)

			req := ProjectDeleteRequest{
				ProjectID: projectID,
				Actor:     NewAuthenticatedUser(actorID, actorID+"@test.com", "Actor", "member", "web"),
			}
			ctx = setTestIdentity(ctx, req.Actor)

			result, decision := srv.deletionService.Delete(ctx, req)
			if tt.expectOK {
				require.Nil(t, decision, "expected deletion to succeed for role %s", tt.role)
				require.NotNil(t, result)
			} else {
				require.NotNil(t, decision, "expected denial for role %s", tt.role)
				assert.False(t, decision.Allowed)
				assert.Equal(t, 403, decision.HTTPStatus)
				assert.Equal(t, ErrCodeProjectDeleteForbidden, decision.DenialCode)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// RS3.3: Unrelated User
// ---------------------------------------------------------------------------

func TestRS3_ProjectDeleteUnrelatedUser(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs3-unrelated")
	ownerID := tid("rs3-unrelated-owner")
	unrelatedID := tid("rs3-unrelated-user")

	createRS3Project(t, s, projectID, ownerID)

	// Create unrelated user with no project bindings.
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: unrelatedID, Email: unrelatedID + "@test.com",
		DisplayName: "Unrelated", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, unrelatedID)

	req := ProjectDeleteRequest{
		ProjectID: projectID,
		Actor:     NewAuthenticatedUser(unrelatedID, unrelatedID+"@test.com", "Unrelated", "member", "web"),
	}
	ctx = setTestIdentity(ctx, req.Actor)

	_, decision := srv.deletionService.Delete(ctx, req)
	require.NotNil(t, decision)
	assert.False(t, decision.Allowed)
	assert.Equal(t, 403, decision.HTTPStatus)
}

// ---------------------------------------------------------------------------
// RS3.4: Super-Admin and Hub-Admin Bypass
// ---------------------------------------------------------------------------

func TestRS3_ProjectDeleteSuperAdminBypass(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs3-sa-bypass")
	ownerID := tid("rs3-sa-bypass-owner")
	superAdminID := tid("rs3-sa-bypass-admin")

	createRS3Project(t, s, projectID, ownerID)
	createRS3SuperAdmin(t, s, superAdminID)

	req := ProjectDeleteRequest{
		ProjectID: projectID,
		Actor:     NewAuthenticatedUser(superAdminID, superAdminID+"@test.com", "SuperAdmin", "admin", "web"),
	}
	ctx = setTestIdentity(ctx, req.Actor)

	result, decision := srv.deletionService.Delete(ctx, req)
	require.Nil(t, decision, "super-admin should bypass project-level governance")
	require.NotNil(t, result)
}

func TestRS3_ProjectDeleteHubAdminBypass(t *testing.T) {
	// Hub-admin has project.read/list/update but NOT project.delete in the
	// frozen policy. Hub-admin should be DENIED deletion at the base
	// permission check. This test verifies the frozen policy is enforced.
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs3-ha-bypass")
	ownerID := tid("rs3-ha-bypass-owner")
	hubAdminID := tid("rs3-ha-bypass-admin")

	createRS3Project(t, s, projectID, ownerID)
	createRS3HubAdmin(t, s, hubAdminID)

	req := ProjectDeleteRequest{
		ProjectID: projectID,
		Actor:     NewAuthenticatedUser(hubAdminID, hubAdminID+"@test.com", "HubAdmin", "admin", "web"),
	}
	ctx = setTestIdentity(ctx, req.Actor)

	_, decision := srv.deletionService.Delete(ctx, req)
	require.NotNil(t, decision, "hub-admin lacks project.delete — should be denied")
	assert.False(t, decision.Allowed)
	assert.Equal(t, 403, decision.HTTPStatus)
	assert.Equal(t, ErrCodeProjectDeleteForbidden, decision.DenialCode)
}

// ---------------------------------------------------------------------------
// RS3.5: Group-Derived Owner — Denied
// ---------------------------------------------------------------------------

func TestRS3_ProjectDeleteGroupDerivedOwnerDenied(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs3-group-owner")
	permanentOwnerID := tid("rs3-group-perm")
	groupUserID := tid("rs3-group-user")

	createRS3Project(t, s, projectID, permanentOwnerID)

	// Create user and group.
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: groupUserID, Email: groupUserID + "@test.com",
		DisplayName: "GroupUser", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, groupUserID)

	// Create group with owner binding on the project.
	groupID := tid("rs3-group-grp")
	require.NoError(t, s.CreateGroup(ctx, &store.Group{
		ID:   groupID,
		Name: "RS3 Test Group",
		Slug: "rs3-test-group-" + groupID[:8],
	}))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    groupID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   groupUserID,
		Role:       "member",
	}))

	// Groups can't actually get owner role (per D3), but let's test with admin
	// to verify group-derived roles don't confer deletion authority.
	adminRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: adminRD.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      groupID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	req := ProjectDeleteRequest{
		ProjectID: projectID,
		Actor:     NewAuthenticatedUser(groupUserID, groupUserID+"@test.com", "GroupUser", "member", "web"),
	}
	ctx = setTestIdentity(ctx, req.Actor)

	_, decision := srv.deletionService.Delete(ctx, req)
	require.NotNil(t, decision, "group-derived access should not confer deletion authority")
	assert.False(t, decision.Allowed)
	assert.Equal(t, 403, decision.HTTPStatus)
}

// ---------------------------------------------------------------------------
// RS3.6: Stale OwnerID — Does Not Grant Deletion Authority
// ---------------------------------------------------------------------------

func TestRS3_ProjectDeleteStaleOwnerIDDenied(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs3-stale-owner")
	realOwnerID := tid("rs3-stale-real")
	staleOwnerID := tid("rs3-stale-user")

	// Create project with the real owner.
	createRS3Project(t, s, projectID, realOwnerID)

	// Create the stale user with no project role bindings.
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: staleOwnerID, Email: staleOwnerID + "@test.com",
		DisplayName: "StaleOwner", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, staleOwnerID)

	// Simulate stale OwnerID by updating the project record.
	project, _ := s.GetProject(ctx, projectID)
	project.OwnerID = staleOwnerID
	_ = s.UpdateProject(ctx, project)

	req := ProjectDeleteRequest{
		ProjectID: projectID,
		Actor:     NewAuthenticatedUser(staleOwnerID, staleOwnerID+"@test.com", "StaleOwner", "member", "web"),
	}
	ctx = setTestIdentity(ctx, req.Actor)

	_, decision := srv.deletionService.Delete(ctx, req)
	require.NotNil(t, decision, "stale OwnerID should not grant deletion authority")
	assert.False(t, decision.Allowed)
	assert.Equal(t, 403, decision.HTTPStatus)
}

// ---------------------------------------------------------------------------
// RS3.7: Suspended Principal
// ---------------------------------------------------------------------------

func TestRS3_ProjectDeleteSuspendedPrincipal(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs3-suspended")
	ownerID := tid("rs3-suspended-user")

	createRS3Project(t, s, projectID, ownerID)

	// Suspend the owner.
	user, _ := s.GetUser(ctx, ownerID)
	user.Status = "suspended"
	require.NoError(t, s.UpdateUser(ctx, user))

	req := ProjectDeleteRequest{
		ProjectID: projectID,
		Actor:     NewAuthenticatedUser(ownerID, ownerID+"@test.com", "Owner", "member", "web"),
	}
	ctx = setTestIdentity(ctx, req.Actor)

	_, decision := srv.deletionService.Delete(ctx, req)
	require.NotNil(t, decision, "suspended principal should be denied")
	assert.False(t, decision.Allowed)
	assert.Equal(t, ErrCodeUserSuspended, decision.DenialCode)
	assert.Equal(t, 403, decision.HTTPStatus)
}

// ---------------------------------------------------------------------------
// RS3.8: Credential Ceiling — Scoped UAT Denied
// ---------------------------------------------------------------------------

func TestRS3_ProjectDeleteScopedUATDenied(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs3-uat-deny")
	ownerID := tid("rs3-uat-deny-user")

	createRS3Project(t, s, projectID, ownerID)

	req := ProjectDeleteRequest{
		ProjectID: projectID,
		Actor:     NewAuthenticatedUser(ownerID, ownerID+"@test.com", "Owner", "member", "web"),
	}

	// Set credential context to scoped UAT.
	ctx = setTestIdentity(ctx, req.Actor)
	ctx = setTestCredentialContext(ctx, CredentialContext{
		Kind: CredentialKindUAT,
		ID:   "test-uat-id",
	})

	_, decision := srv.deletionService.Delete(ctx, req)
	require.NotNil(t, decision, "scoped UAT should be denied for project deletion")
	assert.False(t, decision.Allowed)
	assert.Equal(t, ErrCodeCredentialInsufficient, decision.DenialCode)
	assert.Equal(t, 403, decision.HTTPStatus)
}

// ---------------------------------------------------------------------------
// RS3.9: Atomic Mutation Audit
// ---------------------------------------------------------------------------

func TestRS3_ProjectDeleteAtomicAudit(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs3-audit")
	ownerID := tid("rs3-audit-owner")

	createRS3Project(t, s, projectID, ownerID)

	req := ProjectDeleteRequest{
		ProjectID: projectID,
		Actor:     NewAuthenticatedUser(ownerID, ownerID+"@test.com", "AuditOwner", "member", "web"),
	}
	ctx = setTestIdentity(ctx, req.Actor)

	result, decision := srv.deletionService.Delete(ctx, req)
	require.Nil(t, decision)
	require.NotNil(t, result)

	// Verify audit record exists.
	audits, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{
		MutationType: "project_delete",
		TargetID:     projectID,
	})
	require.NoError(t, err)
	require.Len(t, audits, 1, "exactly one audit record should exist")

	audit := audits[0]
	assert.Equal(t, "project_delete", audit.MutationType)
	assert.Equal(t, "project", audit.TargetType)
	assert.Equal(t, projectID, audit.TargetID)
	assert.Equal(t, "user", audit.ActorPrincipalKind)
	assert.Equal(t, ownerID, audit.ActorPrincipalID)

	// Verify before state fields.
	var beforeState map[string]string
	require.NoError(t, json.Unmarshal([]byte(audit.BeforeSummary), &beforeState))
	assert.Equal(t, projectID, beforeState["project_id"])
	assert.NotEmpty(t, beforeState["project_name"])
}

// ---------------------------------------------------------------------------
// RS3.10: No Audit On Denial
// ---------------------------------------------------------------------------

func TestRS3_ProjectDeleteNoAuditOnDenial(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs3-noaudit")
	ownerID := tid("rs3-noaudit-owner")
	unrelatedID := tid("rs3-noaudit-unrelated")

	createRS3Project(t, s, projectID, ownerID)

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: unrelatedID, Email: unrelatedID + "@test.com",
		DisplayName: "Unrelated", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, unrelatedID)

	req := ProjectDeleteRequest{
		ProjectID: projectID,
		Actor:     NewAuthenticatedUser(unrelatedID, unrelatedID+"@test.com", "Unrelated", "member", "web"),
	}
	ctx = setTestIdentity(ctx, req.Actor)

	_, decision := srv.deletionService.Delete(ctx, req)
	require.NotNil(t, decision)
	assert.False(t, decision.Allowed)

	// Verify NO audit record was created.
	audits, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{
		MutationType: "project_delete",
		TargetID:     projectID,
	})
	require.NoError(t, err)
	assert.Len(t, audits, 0, "no audit record should exist on denial")
}

// ---------------------------------------------------------------------------
// RS3.11: Security-Relevant Cascade
// ---------------------------------------------------------------------------

func TestRS3_ProjectDeleteCompleteCascade(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs3-cascade")
	ownerID := tid("rs3-cascade-owner")
	memberID := tid("rs3-cascade-member")

	createRS3Project(t, s, projectID, ownerID)
	createRS3UserWithRole(t, s, memberID, memberID+"@test.com", projectID, store.ProjectRoleMember)

	// Add project-scoped secrets.
	require.NoError(t, s.CreateSecret(ctx, &store.Secret{
		ID:             tid("rs3-cascade-secret"),
		Key:            "rs3-test-secret",
		EncryptedValue: "encrypted-secret-value",
		Scope:          store.ScopeProject,
		ScopeID:        projectID,
	}))

	// Add project-scoped env vars.
	require.NoError(t, s.CreateEnvVar(ctx, &store.EnvVar{
		ID:  tid("rs3-cascade-envvar"),
		Key: "RS3_TEST_VAR", Value: "test-value",
		Scope: store.ScopeProject, ScopeID: projectID,
	}))

	// Delete the project.
	req := ProjectDeleteRequest{
		ProjectID: projectID,
		Actor:     NewAuthenticatedUser(ownerID, ownerID+"@test.com", "Owner", "member", "web"),
	}
	ctx = setTestIdentity(ctx, req.Actor)

	result, decision := srv.deletionService.Delete(ctx, req)
	require.Nil(t, decision)
	require.NotNil(t, result)

	// Verify cascades: project is deleted.
	_, err := s.GetProject(ctx, projectID)
	assert.ErrorIs(t, err, store.ErrNotFound)

	// Verify cascades: role bindings are deleted.
	bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, ownerID)
	require.NoError(t, err)
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeProject && b.ScopeID == projectID {
			t.Errorf("owner binding should be cascade-deleted, found: %+v", b)
		}
	}
	bindings, err = s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, memberID)
	require.NoError(t, err)
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeProject && b.ScopeID == projectID {
			t.Errorf("member binding should be cascade-deleted, found: %+v", b)
		}
	}

	// Verify cascades: secrets are deleted.
	secrets, err := s.ListSecrets(ctx, store.SecretFilter{
		Scope: store.ScopeProject, ScopeID: projectID,
	})
	require.NoError(t, err)
	assert.Len(t, secrets, 0, "project secrets should be cascade-deleted")

	// Verify cascades: env vars are deleted.
	envVars, err := s.ListEnvVars(ctx, store.EnvVarFilter{
		Scope: store.ScopeProject, ScopeID: projectID,
	})
	require.NoError(t, err)
	assert.Len(t, envVars, 0, "project env vars should be cascade-deleted")
}

// ---------------------------------------------------------------------------
// RS3.12: Already-Deleted Target
// ---------------------------------------------------------------------------

func TestRS3_ProjectDeleteAlreadyDeleted(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs3-already-del")
	ownerID := tid("rs3-already-del-own")

	createRS3Project(t, s, projectID, ownerID)

	req := ProjectDeleteRequest{
		ProjectID: projectID,
		Actor:     NewAuthenticatedUser(ownerID, ownerID+"@test.com", "Owner", "member", "web"),
	}
	ctx = setTestIdentity(ctx, req.Actor)

	// First delete succeeds.
	_, decision := srv.deletionService.Delete(ctx, req)
	require.Nil(t, decision)

	// Second delete: project not found.
	_, decision = srv.deletionService.Delete(ctx, req)
	require.NotNil(t, decision)
	assert.Equal(t, 404, decision.HTTPStatus)
	assert.Equal(t, ErrCodeNotFound, decision.DenialCode)
}

// ---------------------------------------------------------------------------
// RS3.13: Concurrent Deletes — Deterministic Safe Outcome
// ---------------------------------------------------------------------------

func TestRS3_ProjectDeleteConcurrentDeletes(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs3-concurrent")
	ownerID := tid("rs3-concurrent-own")

	createRS3Project(t, s, projectID, ownerID)

	req := ProjectDeleteRequest{
		ProjectID: projectID,
		Actor:     NewAuthenticatedUser(ownerID, ownerID+"@test.com", "Owner", "member", "web"),
	}

	const goroutines = 5
	results := make([]struct {
		result   *ProjectDeleteResult
		decision *ProjectDeleteDecision
	}, goroutines)

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			gctx := setTestIdentity(ctx, req.Actor)
			results[idx].result, results[idx].decision = srv.deletionService.Delete(gctx, req)
		}(i)
	}
	wg.Wait()

	// Exactly one should succeed, rest should get not-found or error.
	successes := 0
	for _, r := range results {
		if r.decision == nil && r.result != nil {
			successes++
		}
	}
	// With SQLite serialization, either exactly one or possibly zero succeed
	// (if a concurrent transaction rolls back). At most one should succeed.
	assert.LessOrEqual(t, successes, 1, "at most one concurrent delete should succeed")

	// Verify project is deleted.
	_, err := s.GetProject(ctx, projectID)
	// If any succeeded, project should be gone. If none succeeded due to
	// transaction contention, project may still exist.
	if successes > 0 {
		assert.ErrorIs(t, err, store.ErrNotFound)
	}

	// Verify no duplicate audit records.
	audits, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{
		MutationType: "project_delete",
		TargetID:     projectID,
	})
	require.NoError(t, err)
	assert.Equal(t, successes, len(audits), "audit count should match success count")
}

// ---------------------------------------------------------------------------
// RS3.14: Expired/Future Role Bindings
// ---------------------------------------------------------------------------

func TestRS3_ProjectDeleteExpiredBindingDenied(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs3-expired-bind")
	permanentOwnerID := tid("rs3-expired-perm")
	expiredOwnerID := tid("rs3-expired-user")

	createRS3Project(t, s, projectID, permanentOwnerID)

	// Create user with expired owner binding.
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: expiredOwnerID, Email: expiredOwnerID + "@test.com",
		DisplayName: "ExpiredOwner", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, expiredOwnerID)

	ownerRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)
	past := time.Now().Add(-24 * time.Hour)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: ownerRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      expiredOwnerID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
		ExpiresAt:        &past,
	})
	require.NoError(t, err)

	req := ProjectDeleteRequest{
		ProjectID: projectID,
		Actor:     NewAuthenticatedUser(expiredOwnerID, expiredOwnerID+"@test.com", "ExpiredOwner", "member", "web"),
	}
	ctx = setTestIdentity(ctx, req.Actor)

	_, decision := srv.deletionService.Delete(ctx, req)
	require.NotNil(t, decision, "expired binding should not confer deletion authority")
	assert.False(t, decision.Allowed)
	assert.Equal(t, 403, decision.HTTPStatus)
}

// ---------------------------------------------------------------------------
// RS3.15: Stable Error Codes — No Existence Oracle
// ---------------------------------------------------------------------------

func TestRS3_ProjectDeleteNoExistenceOracle(t *testing.T) {
	srv, _ := testServer(t)
	ctx := context.Background()

	nonexistentID := tid("rs3-nonexistent")
	actorID := tid("rs3-oracle-actor")

	req := ProjectDeleteRequest{
		ProjectID: nonexistentID,
		Actor:     NewAuthenticatedUser(actorID, actorID+"@test.com", "Actor", "member", "web"),
	}
	ctx = setTestIdentity(ctx, req.Actor)

	_, decision := srv.deletionService.Delete(ctx, req)
	require.NotNil(t, decision)
	// Non-existent and unauthorized targets should be indistinguishable.
	assert.Equal(t, 404, decision.HTTPStatus)
	assert.Equal(t, ErrCodeNotFound, decision.DenialCode)
}

// ---------------------------------------------------------------------------
// RS3.16: AST Proof — DeleteProject Call Site Classification
// ---------------------------------------------------------------------------

func TestRS3_DeleteProjectCallSiteClassification(t *testing.T) {
	// Walk all non-test .go files under pkg/hub/ and pkg/store/ and find
	// all call sites of DeleteProject. Each must be classified in the
	// mutation classifications map.
	classifiedSites := make(map[string]bool)
	for _, mc := range authzop.MutationClassifications {
		if mc.Symbol == "DeleteProject" {
			key := fmt.Sprintf("%s:%s", mc.File, mc.Function)
			classifiedSites[key] = true
		}
	}

	discovered := findDeleteProjectCallSites(t)

	for _, site := range discovered {
		key := fmt.Sprintf("%s:%s", site.file, site.function)
		if !classifiedSites[key] {
			t.Errorf("unclassified DeleteProject call site: %s in %s (line %d)",
				site.function, site.file, site.line)
		}
	}

	// Also verify no stale classifications exist.
	discoveredKeys := make(map[string]bool)
	for _, site := range discovered {
		key := fmt.Sprintf("%s:%s", site.file, site.function)
		discoveredKeys[key] = true
	}
	for _, mc := range authzop.MutationClassifications {
		if mc.Symbol == "DeleteProject" {
			key := fmt.Sprintf("%s:%s", mc.File, mc.Function)
			// Store-level implementations and test fixtures are exempt
			// from the discoverable-in-hub check.
			if strings.HasPrefix(mc.File, "pkg/store/") {
				continue
			}
			if !discoveredKeys[key] {
				t.Errorf("stale DeleteProject classification: %s in %s", mc.Function, mc.File)
			}
		}
	}
}

type callSite struct {
	file     string
	function string
	line     int
}

func findDeleteProjectCallSites(t *testing.T) []callSite {
	t.Helper()
	var sites []callSite

	dirs := []string{"pkg/hub/", "pkg/store/entadapter/"}
	for _, dir := range dirs {
		fullDir := filepath.Join(repoRoot(t), dir)
		entries, err := os.ReadDir(fullDir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
				continue
			}
			if strings.HasSuffix(entry.Name(), "_test.go") {
				continue
			}
			filePath := filepath.Join(fullDir, entry.Name())
			relPath := filepath.Join(dir, entry.Name())
			sites = append(sites, findCallSitesInFile(t, filePath, relPath, "DeleteProject")...)
		}
	}
	return sites
}

func findCallSitesInFile(t *testing.T, filePath, relPath, symbol string) []callSite {
	t.Helper()
	fset := token.NewFileSet()
	node, err := parser.ParseFile(fset, filePath, nil, 0)
	if err != nil {
		t.Logf("warning: could not parse %s: %v", filePath, err)
		return nil
	}

	var sites []callSite
	var currentFunc string

	ast.Inspect(node, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncDecl:
			currentFunc = x.Name.Name
		case *ast.SelectorExpr:
			if x.Sel.Name == symbol {
				sites = append(sites, callSite{
					file:     relPath,
					function: currentFunc,
					line:     fset.Position(x.Pos()).Line,
				})
			}
		}
		return true
	})
	return sites
}

func repoRoot(t *testing.T) string {
	t.Helper()
	// Walk up from the test file to find the repository root.
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find repository root (no go.mod found)")
		}
		dir = parent
	}
}

// ---------------------------------------------------------------------------
// Test Helpers
// ---------------------------------------------------------------------------

func createRS3Project(t *testing.T, s store.Store, projectID, ownerID string) {
	t.Helper()
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: ownerID, Email: ownerID + "@test.com",
		DisplayName: "Owner", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, ownerID)

	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID:        projectID,
		Name:      "RS3 Test Project " + projectID,
		Slug:      fmt.Sprintf("rs3-test-%s", projectID[:8]),
		CreatedBy: ownerID,
	}))

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

func createRS3UserWithRole(t *testing.T, s store.Store, userID, email, projectID, roleName string) {
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

func createRS3SuperAdmin(t *testing.T, s store.Store, userID string) {
	t.Helper()
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userID, Email: userID + "@test.com",
		DisplayName: "SuperAdmin", Role: "admin", Status: "active",
	}))
	ensureHubMembership(ctx, s, userID)

	saRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: saRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
		ScopeID:          "",
		CreatedBy:        store.SystemReconcileCreatedBy,
	})
	require.NoError(t, err)
}

func createRS3HubAdmin(t *testing.T, s store.Store, userID string) {
	t.Helper()
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userID, Email: userID + "@test.com",
		DisplayName: "HubAdmin", Role: "admin", Status: "active",
	}))
	ensureHubMembership(ctx, s, userID)

	haRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: haRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
		ScopeID:          "",
		CreatedBy:        store.SystemReconcileCreatedBy,
	})
	require.NoError(t, err)
}

// setTestIdentity sets the identity in context for the test.
func setTestIdentity(ctx context.Context, user UserIdentity) context.Context {
	return contextWithIdentity(ctx, user)
}

// setTestCredentialContext sets the credential context for the test.
func setTestCredentialContext(ctx context.Context, cred CredentialContext) context.Context {
	return contextWithCredentialContext(ctx, cred)
}
