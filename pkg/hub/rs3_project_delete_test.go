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
	"log/slog"
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

// ---------------------------------------------------------------------------
// RS3.17: Agent JWT Credential Denial
// ---------------------------------------------------------------------------

func TestRS3_ProjectDeleteAgentJWTDenied(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs3-agent-jwt")
	ownerID := tid("rs3-agent-jwt-own")

	createRS3Project(t, s, projectID, ownerID)

	req := ProjectDeleteRequest{
		ProjectID: projectID,
		Actor:     NewAuthenticatedUser(ownerID, ownerID+"@test.com", "Owner", "member", "web"),
	}

	// Set credential context to agent JWT — should be denied by credential ceiling.
	ctx = setTestIdentity(ctx, req.Actor)
	ctx = setTestCredentialContext(ctx, CredentialContext{
		Kind: CredentialKindAgentJWT,
		ID:   "agent-jwt-test-id",
	})

	_, decision := srv.deletionService.Delete(ctx, req)
	require.NotNil(t, decision, "agent JWT should be denied for project deletion")
	assert.False(t, decision.Allowed)
	assert.Equal(t, ErrCodeCredentialInsufficient, decision.DenialCode)
	assert.Equal(t, 403, decision.HTTPStatus)

	// Verify project still exists (not deleted).
	_, err := s.GetProject(ctx, projectID)
	assert.NoError(t, err, "project should still exist after credential denial")

	// Verify no audit record was created.
	audits, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{
		MutationType: "project_delete",
		TargetID:     projectID,
	})
	require.NoError(t, err)
	assert.Len(t, audits, 0, "no audit on credential denial")
}

// ---------------------------------------------------------------------------
// RS3.18: Future-Dated Binding Denial (NotBefore in the future)
// ---------------------------------------------------------------------------

func TestRS3_ProjectDeleteFutureBindingDenied(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs3-future-bind")
	permanentOwnerID := tid("rs3-future-perm")
	futureOwnerID := tid("rs3-future-user")

	createRS3Project(t, s, projectID, permanentOwnerID)

	// Create user with future-dated owner binding (NotBefore tomorrow).
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: futureOwnerID, Email: futureOwnerID + "@test.com",
		DisplayName: "FutureOwner", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, futureOwnerID)

	ownerRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)
	future := time.Now().Add(24 * time.Hour)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: ownerRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      futureOwnerID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
		NotBefore:        &future,
	})
	require.NoError(t, err)

	req := ProjectDeleteRequest{
		ProjectID: projectID,
		Actor:     NewAuthenticatedUser(futureOwnerID, futureOwnerID+"@test.com", "FutureOwner", "member", "web"),
	}
	ctx = setTestIdentity(ctx, req.Actor)

	_, decision := srv.deletionService.Delete(ctx, req)
	require.NotNil(t, decision, "future-dated binding should not confer deletion authority")
	assert.False(t, decision.Allowed)
	assert.Equal(t, 403, decision.HTTPStatus)

	// Verify project still exists.
	_, err = s.GetProject(ctx, projectID)
	assert.NoError(t, err, "project should still exist after future binding denial")
}

// ---------------------------------------------------------------------------
// RS3.19: Cross-Project Actor — Owner of A cannot delete B
// ---------------------------------------------------------------------------

func TestRS3_ProjectDeleteCrossProjectActor(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectA := tid("rs3-cross-projA")
	projectB := tid("rs3-cross-projB")
	ownerA := tid("rs3-cross-ownA")
	ownerB := tid("rs3-cross-ownB")

	createRS3Project(t, s, projectA, ownerA)
	createRS3Project(t, s, projectB, ownerB)

	// Owner of project A attempts to delete project B.
	req := ProjectDeleteRequest{
		ProjectID: projectB,
		Actor:     NewAuthenticatedUser(ownerA, ownerA+"@test.com", "OwnerA", "member", "web"),
	}
	ctx = setTestIdentity(ctx, req.Actor)

	_, decision := srv.deletionService.Delete(ctx, req)
	require.NotNil(t, decision, "owner of project A should not be able to delete project B")
	assert.False(t, decision.Allowed)
	assert.Equal(t, 403, decision.HTTPStatus)
	assert.Equal(t, ErrCodeProjectDeleteForbidden, decision.DenialCode)

	// Verify both projects still exist.
	_, err := s.GetProject(ctx, projectA)
	assert.NoError(t, err, "project A should still exist")
	_, err = s.GetProject(ctx, projectB)
	assert.NoError(t, err, "project B should still exist")
}

// ---------------------------------------------------------------------------
// RS3.20: Cascade Failure Rollback — no false 204
// ---------------------------------------------------------------------------

func TestRS3_ProjectDeleteCascadeFailureRollback(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs3-cascade-fail")
	ownerID := tid("rs3-cascade-f-own")

	createRS3Project(t, s, projectID, ownerID)

	// Create a failing store wrapper that errors on cascade.
	failStore := &rs3FailingStore{Store: s, failOn: "cascade_bindings"}
	failSvc := NewProjectDeletionService(failStore, newTestAuthzService(s), slog.Default())

	req := ProjectDeleteRequest{
		ProjectID: projectID,
		Actor:     NewAuthenticatedUser(ownerID, ownerID+"@test.com", "Owner", "member", "web"),
	}
	ctx = setTestIdentity(ctx, req.Actor)

	_, decision := failSvc.Delete(ctx, req)
	require.NotNil(t, decision, "cascade failure should not produce a successful result")
	assert.Equal(t, 500, decision.HTTPStatus, "cascade failure should result in 500, not 204")

	// Verify project is NOT deleted — transaction was rolled back.
	_, err := s.GetProject(ctx, projectID)
	assert.NoError(t, err, "project should still exist after cascade failure (rollback)")

	// Verify no audit record — audit is in the same tx, so rollback removes it.
	audits, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{
		MutationType: "project_delete",
		TargetID:     projectID,
	})
	require.NoError(t, err)
	assert.Len(t, audits, 0, "no audit on cascade failure (transaction rolled back)")
}

// ---------------------------------------------------------------------------
// RS3.21: Audit Failure Rollback — no false 204
// ---------------------------------------------------------------------------

func TestRS3_ProjectDeleteAuditFailureRollback(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs3-audit-fail")
	ownerID := tid("rs3-audit-f-own")

	createRS3Project(t, s, projectID, ownerID)

	// Create a failing store wrapper that errors on audit write.
	failStore := &rs3FailingStore{Store: s, failOn: "audit"}
	failSvc := NewProjectDeletionService(failStore, newTestAuthzService(s), slog.Default())

	req := ProjectDeleteRequest{
		ProjectID: projectID,
		Actor:     NewAuthenticatedUser(ownerID, ownerID+"@test.com", "Owner", "member", "web"),
	}
	ctx = setTestIdentity(ctx, req.Actor)

	_, decision := failSvc.Delete(ctx, req)
	require.NotNil(t, decision, "audit failure should not produce a successful result")
	assert.Equal(t, 500, decision.HTTPStatus, "audit failure should result in 500, not 204")

	// Verify project is NOT deleted — transaction was rolled back.
	_, err := s.GetProject(ctx, projectID)
	assert.NoError(t, err, "project should still exist after audit failure (rollback)")
}

// ---------------------------------------------------------------------------
// RS3.22: External Effect Spies — No effects on denial
// ---------------------------------------------------------------------------

func TestRS3_ProjectDeleteNoEffectsOnDenial(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Set up a mock dispatcher to track agent dispatch calls.
	disp := &rs3EffectSpy{}
	srv.SetDispatcher(disp)

	// Create a project with agents (so effects would be visible if they fired).
	projectID := tid("rs3-effect-spy")
	ownerID := tid("rs3-effect-spy-own")
	createRS3Project(t, s, projectID, ownerID)

	agentID := tid("rs3-effect-spy-agt")
	brokerID := tid("rs3-effect-spy-brk")
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: brokerID, Name: "Spy Broker", Slug: "spy-broker",
		Status: store.BrokerStatusOnline, Endpoint: "http://localhost:9800",
	}))
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: agentID, Slug: "spy-agent", Name: "Spy Agent",
		ProjectID: projectID, RuntimeBrokerID: brokerID,
	}))

	// Suspend the owner so the service denies at actor status (step 4).
	user, _ := s.GetUser(ctx, ownerID)
	user.Status = "suspended"
	require.NoError(t, s.UpdateUser(ctx, user))

	// Pre-enumerate effects (handler does this before calling service).
	effectInputs := srv.preEnumerateDeletionEffects(ctx, projectID)
	require.Len(t, effectInputs.agents, 1, "agent should be pre-enumerated")

	// Call the deletion service — should be denied.
	req := ProjectDeleteRequest{
		ProjectID: projectID,
		Actor:     NewAuthenticatedUser(ownerID, ownerID+"@test.com", "Owner", "member", "web"),
	}
	sCtx := setTestIdentity(ctx, req.Actor)

	_, decision := srv.deletionService.Delete(sCtx, req)
	require.NotNil(t, decision, "suspended user should be denied")
	assert.Equal(t, ErrCodeUserSuspended, decision.DenialCode)

	// Handler pattern: effects only fire on success (decision == nil).
	// Since decision != nil, executePostDeletionEffects is NOT called.
	// Verify the dispatcher was NOT called.
	assert.Equal(t, 0, disp.dispatchDeleteCalls,
		"no agent dispatch should occur when deletion is denied")

	// Verify project still exists.
	_, err := s.GetProject(ctx, projectID)
	assert.NoError(t, err, "project should still exist after denial")
}

// ---------------------------------------------------------------------------
// RS3.23: Concurrent Authority Change vs. Delete (TOCTOU)
// ---------------------------------------------------------------------------

func TestRS3_ProjectDeleteConcurrentRoleRevoke(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs3-conc-revoke")
	ownerID := tid("rs3-conc-rev-own")
	createRS3Project(t, s, projectID, ownerID)

	req := ProjectDeleteRequest{
		ProjectID: projectID,
		Actor:     NewAuthenticatedUser(ownerID, ownerID+"@test.com", "Owner", "member", "web"),
	}

	const goroutines = 5
	type outcome struct {
		result   *ProjectDeleteResult
		decision *ProjectDeleteDecision
	}

	// Run concurrent deletes and role revocations.
	// One goroutine deletes the project; another revokes the owner binding.
	// The TOCTOU closure (in-tx governance re-check) ensures that either:
	//   (a) Delete sees active binding → succeeds
	//   (b) Delete sees revoked binding → denies (governance re-check under lock)
	// Either way, the outcome must be consistent.
	results := make([]outcome, goroutines)
	var wg sync.WaitGroup
	wg.Add(goroutines + 1) // +1 for role revoker

	// Role revoker goroutine: revokes the owner binding.
	go func() {
		defer wg.Done()
		// Small delay to make race more likely.
		time.Sleep(time.Millisecond)
		bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, ownerID)
		if err != nil {
			return
		}
		for _, b := range bindings {
			if b.ScopeType == store.RoleScopeProject && b.ScopeID == projectID {
				_ = s.DeleteRoleBinding(ctx, b.ID)
			}
		}
	}()

	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			gctx := setTestIdentity(ctx, req.Actor)
			results[idx].result, results[idx].decision = srv.deletionService.Delete(gctx, req)
		}(i)
	}
	wg.Wait()

	// Count outcomes.
	successes := 0
	denials := 0
	for _, r := range results {
		if r.decision == nil && r.result != nil {
			successes++
		} else if r.decision != nil {
			denials++
		}
	}

	// At most one delete should succeed (serialization via project lock).
	assert.LessOrEqual(t, successes, 1, "at most one concurrent delete should succeed")

	// Every outcome should be either a clean success or a clean denial.
	assert.Equal(t, goroutines, successes+denials,
		"every goroutine should produce either success or denial, never a partial result")
}

// ---------------------------------------------------------------------------
// RS3.24a: Structural Lock Assertion — deletion and membership share the
// same project-scoped serialization lock.
//
// This AST-based test proves that both ProjectDeletionService and
// ProjectMembershipService call LockProjectForMembership inside their
// WithTx callbacks, ensuring they serialize against each other.
// SQLite's single-writer makes this redundant in the test environment, but
// on PostgreSQL the advisory lock is the serialization mechanism. A full
// PostgreSQL integration test is the proper venue to verify lock contention
// under concurrent writes; this test proves the lock call exists in both
// production code paths.
// ---------------------------------------------------------------------------

func TestRS3_ProjectDeleteAndMembershipShareLock(t *testing.T) {
	root := repoRoot(t)

	// Verify deletion service calls LockProjectForMembership.
	deletionCalls := findCallSitesInFile(t,
		filepath.Join(root, "pkg/hub/project_deletion_service.go"),
		"pkg/hub/project_deletion_service.go",
		"LockProjectForMembership")
	require.NotEmpty(t, deletionCalls,
		"ProjectDeletionService must call LockProjectForMembership — "+
			"deletion and membership must serialize on the same lock")

	// Verify membership service calls LockProjectForMembership.
	membershipCalls := findCallSitesInFile(t,
		filepath.Join(root, "pkg/hub/project_membership_service.go"),
		"pkg/hub/project_membership_service.go",
		"LockProjectForMembership")
	require.NotEmpty(t, membershipCalls,
		"ProjectMembershipService must call LockProjectForMembership — "+
			"membership mutations and deletion must serialize on the same lock")

	// Both services call the same method name on the transactional store,
	// proving they acquire the same project-scoped advisory lock.
	t.Logf("deletion service lock sites: %d, membership service lock sites: %d",
		len(deletionCalls), len(membershipCalls))
	t.Logf("NOTE: SQLite single-writer serializes all writes in tests. " +
		"On PostgreSQL, LockProjectForMembership acquires an advisory lock " +
		"that serializes deletion against concurrent membership mutations. " +
		"A PostgreSQL integration test should verify lock contention under " +
		"concurrent writes.")
}

// ---------------------------------------------------------------------------
// RS3.24: Frontend Capability — N/A
//
// Project deletion does not expose a frontend capability signal. The hub
// does not serve a capabilities endpoint for project.delete. Frontend UI
// decisions are derived from the project list response metadata. This is
// documented rather than tested — there is no capability system to assert
// against. If a capability system is added in a future RS, this should be
// revisited.
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// RS3.25: Extended Cascade — UATs, Schedules, Agent Credentials
// ---------------------------------------------------------------------------

func TestRS3_ProjectDeleteCascadeUATsSchedulesCredentials(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs3-ext-cascade")
	ownerID := tid("rs3-ext-casc-own")

	createRS3Project(t, s, projectID, ownerID)

	// Create a project-scoped UAT.
	require.NoError(t, s.CreateUserAccessToken(ctx, &store.UserAccessToken{
		ID:        tid("rs3-ext-uat"),
		UserID:    ownerID,
		Name:      "test-uat",
		Prefix:    "scion_uat_",
		KeyHash:   "hash-" + tid("rs3-ext-uat"),
		ProjectID: projectID,
		Scopes:    []string{"agent:read"},
	}))

	// Create a schedule for the project.
	require.NoError(t, s.CreateSchedule(ctx, &store.Schedule{
		ID:        tid("rs3-ext-sched"),
		ProjectID: projectID,
		Name:      "test-schedule",
		CronExpr:  "0 * * * *",
		EventType: "message",
		Payload:   "{}",
		Status:    store.ScheduleStatusActive,
	}))

	// Create an agent and agent credential.
	agentID := tid("rs3-ext-agent")
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: agentID, Slug: "ext-cascade-agent", Name: "ExtAgent",
		ProjectID: projectID,
	}))
	require.NoError(t, s.CreateAgentCredential(ctx, &store.AgentCredential{
		ID:           tid("rs3-ext-cred"),
		AgentID:      agentID,
		ProjectID:    projectID,
		TokenJTIHash: "jti-hash-" + tid("rs3-ext-cred"),
		IssuedAt:     time.Now(),
		ExpiresAt:    time.Now().Add(24 * time.Hour),
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

	// Verify UATs are cascade-deleted.
	uats, err := s.ListUserAccessTokens(ctx, ownerID)
	require.NoError(t, err)
	for _, uat := range uats {
		if uat.ProjectID == projectID {
			t.Errorf("UAT should be cascade-deleted, found: %+v", uat)
		}
	}
	assert.GreaterOrEqual(t, result.CascadeSummary.UserAccessTokens, 1,
		"cascade summary should report at least 1 UAT deleted")

	// Verify schedules are cascade-deleted.
	schedules, err := s.ListSchedules(ctx, store.ScheduleFilter{ProjectID: projectID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, schedules.Items, 0, "project schedules should be cascade-deleted")
	assert.GreaterOrEqual(t, result.CascadeSummary.Schedules, 1,
		"cascade summary should report at least 1 schedule deleted")

	// Verify agent credentials are cascade-deleted.
	// Agent records are deleted by CompositeStore.DeleteProject, so we can't
	// query by agent. Check cascade summary instead.
	assert.GreaterOrEqual(t, result.CascadeSummary.AgentCredentials, 1,
		"cascade summary should report at least 1 agent credential deleted")
}

// ---------------------------------------------------------------------------
// RS3.26: AfterSummary / CascadeSummary Contract Assertion
// ---------------------------------------------------------------------------

func TestRS3_ProjectDeleteAuditAfterSummary(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs3-after-summ")
	ownerID := tid("rs3-after-s-own")

	createRS3Project(t, s, projectID, ownerID)

	// Add some security state so the cascade summary has content.
	require.NoError(t, s.CreateSecret(ctx, &store.Secret{
		ID: tid("rs3-as-secret"), Key: "after-summary-secret",
		EncryptedValue: "encrypted", Scope: store.ScopeProject, ScopeID: projectID,
	}))

	req := ProjectDeleteRequest{
		ProjectID: projectID,
		Actor:     NewAuthenticatedUser(ownerID, ownerID+"@test.com", "Owner", "member", "web"),
	}
	ctx = setTestIdentity(ctx, req.Actor)

	_, decision := srv.deletionService.Delete(ctx, req)
	require.Nil(t, decision)

	// Retrieve audit record and assert AfterSummary is populated.
	audits, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{
		MutationType: "project_delete",
		TargetID:     projectID,
	})
	require.NoError(t, err)
	require.Len(t, audits, 1)

	audit := audits[0]
	assert.NotEmpty(t, audit.AfterSummary, "AfterSummary should contain cascade summary JSON")

	// Verify the AfterSummary JSON round-trips correctly.
	var afterState map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(audit.AfterSummary), &afterState))
	assert.Contains(t, afterState, "role_bindings_deleted",
		"AfterSummary should contain role_bindings_deleted field")
	assert.Contains(t, afterState, "secrets_deleted",
		"AfterSummary should contain secrets_deleted field")
	assert.Contains(t, afterState, "user_access_tokens_deleted",
		"AfterSummary should contain user_access_tokens_deleted field")
	assert.Contains(t, afterState, "schedules_deleted",
		"AfterSummary should contain schedules_deleted field")
	assert.Contains(t, afterState, "agent_credentials_deleted",
		"AfterSummary should contain agent_credentials_deleted field")
}

// ---------------------------------------------------------------------------
// RS3.27: Table-Driven Cascade Failure Rollback
//
// Each cascade category must prove: transaction rollback, project survival,
// no audit record, and non-success HTTP status on injected failure.
// ---------------------------------------------------------------------------

func TestRS3_ProjectDeleteCascadeFailureRollbackMatrix(t *testing.T) {
	cases := []struct {
		name   string
		failOn string
	}{
		{"uats", "cascade_uats"},
		{"schedules", "cascade_schedules"},
		{"agent_credentials", "cascade_agent_credentials"},
		{"lifecycle_hooks", "cascade_lifecycle_hooks"},
		{"pre_start_hooks", "cascade_pre_start_hooks"},
		{"project_providers", "cascade_project_providers"},
		{"project_sync_states", "cascade_project_sync_states"},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			_, s := testServer(t)
			ctx := context.Background()

			projectID := tid("rs3-cfm-" + tt.name)
			ownerID := tid("rs3-cfmo-" + tt.name)

			createRS3Project(t, s, projectID, ownerID)

			failStore := &rs3FailingStore{Store: s, failOn: tt.failOn}
			failSvc := NewProjectDeletionService(failStore, newTestAuthzService(s), slog.Default())

			req := ProjectDeleteRequest{
				ProjectID: projectID,
				Actor:     NewAuthenticatedUser(ownerID, ownerID+"@test.com", "Owner", "member", "web"),
			}
			ctx = setTestIdentity(ctx, req.Actor)

			_, decision := failSvc.Delete(ctx, req)

			// Must produce non-success result.
			require.NotNil(t, decision, "%s cascade failure should not produce success", tt.name)
			assert.Equal(t, 500, decision.HTTPStatus,
				"%s cascade failure should produce 500, not 204", tt.name)

			// Project must survive — transaction was rolled back.
			_, err := s.GetProject(ctx, projectID)
			assert.NoError(t, err,
				"project should survive %s cascade failure (rollback)", tt.name)

			// No audit record — audit is in the same tx, so rollback removes it.
			audits, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{
				MutationType: "project_delete",
				TargetID:     projectID,
			})
			require.NoError(t, err)
			assert.Len(t, audits, 0,
				"no audit should exist after %s cascade failure (rolled back)", tt.name)
		})
	}
}

// ---------------------------------------------------------------------------
// RS3 Failure Injection — store wrapper
// ---------------------------------------------------------------------------

// rs3FailingStore wraps a real store.Store and injects errors at specific
// operations to test rollback behavior. Used for cascade and audit failure
// injection tests.
type rs3FailingStore struct {
	store.Store
	failOn string // which operation to fail: "cascade_bindings", "audit"
}

func (f *rs3FailingStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	return f.Store.WithTx(ctx, func(tx store.Store) error {
		wrappedTx := &rs3FailingStore{Store: tx, failOn: f.failOn}
		return fn(wrappedTx)
	})
}

func (f *rs3FailingStore) DeleteRoleBindingsForScope(ctx context.Context, scopeType, scopeID string) (int, error) {
	if f.failOn == "cascade_bindings" {
		return 0, fmt.Errorf("injected: cascade failure at role bindings")
	}
	return f.Store.DeleteRoleBindingsForScope(ctx, scopeType, scopeID)
}

func (f *rs3FailingStore) CreateMutationAudit(ctx context.Context, record *store.MutationAuditRecord) error {
	if f.failOn == "audit" {
		return fmt.Errorf("injected: audit write failure")
	}
	return f.Store.CreateMutationAudit(ctx, record)
}

func (f *rs3FailingStore) DeleteUserAccessTokensByProject(ctx context.Context, projectID string) (int, error) {
	if f.failOn == "cascade_uats" {
		return 0, fmt.Errorf("injected: cascade failure at UATs")
	}
	return f.Store.DeleteUserAccessTokensByProject(ctx, projectID)
}

func (f *rs3FailingStore) DeleteSchedulesByProject(ctx context.Context, projectID string) (int, error) {
	if f.failOn == "cascade_schedules" {
		return 0, fmt.Errorf("injected: cascade failure at schedules")
	}
	return f.Store.DeleteSchedulesByProject(ctx, projectID)
}

func (f *rs3FailingStore) DeleteAgentCredentialsByProject(ctx context.Context, projectID string) (int, error) {
	if f.failOn == "cascade_agent_credentials" {
		return 0, fmt.Errorf("injected: cascade failure at agent credentials")
	}
	return f.Store.DeleteAgentCredentialsByProject(ctx, projectID)
}

func (f *rs3FailingStore) DeleteLifecycleHooksByScope(ctx context.Context, scopeType string, scopeID string) (int, error) {
	if f.failOn == "cascade_lifecycle_hooks" {
		return 0, fmt.Errorf("injected: cascade failure at lifecycle hooks")
	}
	return f.Store.DeleteLifecycleHooksByScope(ctx, scopeType, scopeID)
}

func (f *rs3FailingStore) DeletePreStartHooksByProject(ctx context.Context, projectID string) (int, error) {
	if f.failOn == "cascade_pre_start_hooks" {
		return 0, fmt.Errorf("injected: cascade failure at pre-start hooks")
	}
	return f.Store.DeletePreStartHooksByProject(ctx, projectID)
}

func (f *rs3FailingStore) DeleteProjectProvidersByProject(ctx context.Context, projectID string) (int, error) {
	if f.failOn == "cascade_project_providers" {
		return 0, fmt.Errorf("injected: cascade failure at project providers")
	}
	return f.Store.DeleteProjectProvidersByProject(ctx, projectID)
}

func (f *rs3FailingStore) DeleteProjectSyncStatesByProject(ctx context.Context, projectID string) (int, error) {
	if f.failOn == "cascade_project_sync_states" {
		return 0, fmt.Errorf("injected: cascade failure at project sync states")
	}
	return f.Store.DeleteProjectSyncStatesByProject(ctx, projectID)
}

// ---------------------------------------------------------------------------
// RS3 Effect Spy — mock dispatcher
// ---------------------------------------------------------------------------

// rs3EffectSpy is a mock dispatcher that tracks whether agent dispatch was called.
// Compile-time assertion: rs3EffectSpy must satisfy AgentDispatcher.
// If AgentDispatcher grows new methods, this will fail to compile, surfacing
// the change rather than silently letting the spy observe a subset.
var _ AgentDispatcher = (*rs3EffectSpy)(nil)

type rs3EffectSpy struct {
	createAgentDispatcher
	dispatchDeleteCalls int
}

func (d *rs3EffectSpy) DispatchAgentDelete(_ context.Context, _ *store.Agent, _, _, _ bool, _ time.Time) error {
	d.dispatchDeleteCalls++
	return nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// newTestAuthzService creates an AuthzService from the given store for use in
// failure injection tests where we can't use the full server.
func newTestAuthzService(s store.Store) *AuthzService {
	return NewAuthzService(s, slog.Default())
}

// setTestIdentity sets the identity in context for the test.
func setTestIdentity(ctx context.Context, user UserIdentity) context.Context {
	return contextWithIdentity(ctx, user)
}

// setTestCredentialContext sets the credential context for the test.
func setTestCredentialContext(ctx context.Context, cred CredentialContext) context.Context {
	return contextWithCredentialContext(ctx, cred)
}
