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
	"database/sql"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// D-002: Migration/custom coexistence tests
//
// These tests verify that MigrateMultiRoleBindings correctly handles the
// coexistence of built-in membership bindings and custom project-scoped role
// bindings. Custom bindings are additive and must be ignored by membership
// deduplication; orphaned/unknown role definitions must fail closed.
// =============================================================================

// ---------------------------------------------------------------------------
// D-002.1: One built-in + N custom → migration is no-op, all preserved
// ---------------------------------------------------------------------------

func TestD002_MigrationWithBuiltInPlusCustomRoles(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("d002-bic-proj")
	ownerID := tid("d002-bic-owner")
	targetID := tid("d002-bic-targ")

	createRS1Project(t, s, projectID, ownerID)

	// Create target user with a built-in membership.
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: "d002-bic@test.com",
		DisplayName: "D002 Target", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, targetID)

	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	builtInBinding, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      targetID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
	builtInSnap := snapshotBinding(t, s, builtInBinding)

	// Create two custom project-scoped role definitions and bindings.
	customRD1 := createCustomRoleDef(t, s, "d002-reviewer", []string{"project.read"})
	customRD2 := createCustomRoleDef(t, s, "d002-deployer", []string{"project.deploy"})

	customBind1 := createCustomBinding(t, s, customRD1.ID, targetID, projectID)
	customBind2 := createCustomBinding(t, s, customRD2.ID, targetID, projectID)
	customSnap1 := snapshotBinding(t, s, customBind1)
	customSnap2 := snapshotBinding(t, s, customBind2)

	// Verify we have 3 bindings for the target (1 built-in + 2 custom)
	// plus the owner's binding.
	projectBindings := listProjectBindings(t, s, targetID, projectID)
	require.Len(t, projectBindings, 3, "setup: expected 3 project bindings for target")

	// Run migration.
	results, err := srv.membershipService.MigrateMultiRoleBindings(ctx)
	require.NoError(t, err)

	// No migration result should be produced for the target — one built-in
	// plus custom bindings is not a duplicate that needs resolution.
	for _, r := range results {
		if r.PrincipalID == targetID && r.ProjectID == projectID {
			t.Errorf("migration should not produce a result for valid coexistence state, got: %+v", r)
		}
	}

	// Verify all 3 bindings are preserved field-for-field.
	assertBindingPreserved(t, s, builtInSnap)
	assertBindingPreserved(t, s, customSnap1)
	assertBindingPreserved(t, s, customSnap2)

	// Verify count is still 3.
	projectBindings = listProjectBindings(t, s, targetID, projectID)
	assert.Len(t, projectBindings, 3, "all 3 bindings must survive migration")
}

// ---------------------------------------------------------------------------
// D-002.2: Custom-only bindings (no built-in) → migration is no-op
// ---------------------------------------------------------------------------

func TestD002_MigrationWithCustomOnlyBindings(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("d002-conly-proj")
	ownerID := tid("d002-conly-own")
	targetID := tid("d002-conly-targ")

	createRS1Project(t, s, projectID, ownerID)

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: "d002-conly@test.com",
		DisplayName: "D002 CustomOnly", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, targetID)

	// Create two custom bindings, no built-in membership.
	customRD1 := createCustomRoleDef(t, s, "d002-co-role1", []string{"project.read"})
	customRD2 := createCustomRoleDef(t, s, "d002-co-role2", []string{"project.write"})

	cb1 := createCustomBinding(t, s, customRD1.ID, targetID, projectID)
	cb2 := createCustomBinding(t, s, customRD2.ID, targetID, projectID)
	snap1 := snapshotBinding(t, s, cb1)
	snap2 := snapshotBinding(t, s, cb2)

	// Run migration.
	results, err := srv.membershipService.MigrateMultiRoleBindings(ctx)
	require.NoError(t, err)

	// No result for target.
	for _, r := range results {
		if r.PrincipalID == targetID && r.ProjectID == projectID {
			t.Errorf("migration should not produce a result for custom-only bindings, got: %+v", r)
		}
	}

	// Both custom bindings preserved.
	assertBindingPreserved(t, s, snap1)
	assertBindingPreserved(t, s, snap2)
}

// ---------------------------------------------------------------------------
// D-002.3: Idempotency — run migration twice with valid coexistence
// ---------------------------------------------------------------------------

func TestD002_MigrationIdempotencyWithCoexistence(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("d002-idem-proj")
	ownerID := tid("d002-idem-own")
	targetID := tid("d002-idem-targ")

	createRS1Project(t, s, projectID, ownerID)

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: "d002-idem@test.com",
		DisplayName: "D002 Idempotent", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, targetID)

	// One built-in + two custom.
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	bi, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      targetID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	customRD := createCustomRoleDef(t, s, "d002-idem-role", []string{"project.read"})
	cb := createCustomBinding(t, s, customRD.ID, targetID, projectID)

	biSnap := snapshotBinding(t, s, bi)
	cbSnap := snapshotBinding(t, s, cb)

	// First migration.
	results1, err := srv.membershipService.MigrateMultiRoleBindings(ctx)
	require.NoError(t, err)
	for _, r := range results1 {
		if r.PrincipalID == targetID {
			t.Errorf("first migration should not touch valid coexistence, got: %+v", r)
		}
	}

	// Second migration — must also be no-op.
	results2, err := srv.membershipService.MigrateMultiRoleBindings(ctx)
	require.NoError(t, err)
	for _, r := range results2 {
		if r.PrincipalID == targetID {
			t.Errorf("second migration should not touch valid coexistence, got: %+v", r)
		}
	}

	// Bindings unchanged after two migrations.
	assertBindingPreserved(t, s, biSnap)
	assertBindingPreserved(t, s, cbSnap)
}

// ---------------------------------------------------------------------------
// D-002.4: Two built-in + custom → dedup built-ins, custom survives
// ---------------------------------------------------------------------------

func TestD002_TwoBuiltInPlusCustom_DedupsBuiltInsPreservesCustom(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("d002-2bi-proj")
	ownerID := tid("d002-2bi-owner")
	targetID := tid("d002-2bi-targ")

	createRS1Project(t, s, projectID, ownerID)

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: "d002-2bi@test.com",
		DisplayName: "D002 TwoBuiltIn", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, targetID)

	// Create two custom bindings first (before the index drop).
	customRD1 := createCustomRoleDef(t, s, "d002-2bi-rev", []string{"project.read"})
	customRD2 := createCustomRoleDef(t, s, "d002-2bi-dep", []string{"project.deploy"})
	cb1 := createCustomBinding(t, s, customRD1.ID, targetID, projectID)
	cb2 := createCustomBinding(t, s, customRD2.ID, targetID, projectID)
	customSnap1 := snapshotBinding(t, s, cb1)
	customSnap2 := snapshotBinding(t, s, cb2)

	// Insert two built-in membership bindings via raw SQL (bypasses both
	// D4 index and application-level check).
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	adminRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)

	dbProvider, ok := s.(interface{ DB() *sql.DB })
	require.True(t, ok, "store must expose DB()")
	db := dbProvider.DB()

	_, execErr := db.ExecContext(ctx, "DROP INDEX IF EXISTS idx_rolebinding_one_membership_per_principal_per_project")
	require.NoError(t, execErr)

	const insertSQL = `INSERT INTO role_bindings ` +
		`(id, role_definition_id, principal_type, principal_id, scope_type, scope_id, membership_kind, created_by, created) ` +
		`VALUES (?, ?, 'user', ?, 'project', ?, 'builtin', ?, datetime('now'))`
	_, execErr = db.ExecContext(ctx, insertSQL,
		uuid.New().String(), memberRD.ID, targetID, projectID, ownerID)
	require.NoError(t, execErr)
	_, execErr = db.ExecContext(ctx, insertSQL,
		uuid.New().String(), adminRD.ID, targetID, projectID, ownerID)
	require.NoError(t, execErr)

	// Verify setup: 4 bindings (2 built-in + 2 custom).
	projectBindings := listProjectBindings(t, s, targetID, projectID)
	require.Len(t, projectBindings, 4, "setup: expected 4 bindings for target")

	// Run migration.
	results, err := srv.membershipService.MigrateMultiRoleBindings(ctx)
	require.NoError(t, err)

	// Find migration result for target: should have kept admin (higher
	// authority) and deleted member.
	var found bool
	for _, r := range results {
		if r.PrincipalID == targetID && r.ProjectID == projectID {
			found = true
			assert.NoError(t, r.Error, "migration should succeed")
			assert.False(t, r.NonComparable, "should not be flagged as non-comparable")
			assert.Equal(t, store.ProjectRoleAdmin, r.KeptRole,
				"should keep admin (higher authority than member)")
			assert.Equal(t, 1, r.DeletedCount,
				"should delete exactly 1 duplicate built-in binding")
		}
	}
	assert.True(t, found, "migration should produce a result for the target")

	// Verify: 3 bindings remain (1 built-in admin + 2 custom).
	projectBindings = listProjectBindings(t, s, targetID, projectID)
	assert.Len(t, projectBindings, 3, "after migration: 1 admin + 2 custom")

	// Custom bindings must be preserved field-for-field.
	assertBindingPreserved(t, s, customSnap1)
	assertBindingPreserved(t, s, customSnap2)
}

// ---------------------------------------------------------------------------
// D-002.5: Orphaned role definition → fail closed
// ---------------------------------------------------------------------------

func TestD002_OrphanedRoleDefinition_FailsClosed(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("d002-orph-proj")
	ownerID := tid("d002-orph-own")
	targetID := tid("d002-orph-targ")

	createRS1Project(t, s, projectID, ownerID)

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: "d002-orph@test.com",
		DisplayName: "D002 Orphan", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, targetID)

	// Create one legitimate custom binding.
	customRD := createCustomRoleDef(t, s, "d002-orph-legit", []string{"project.read"})
	createCustomBinding(t, s, customRD.ID, targetID, projectID)

	// Insert a binding referencing a non-existent role definition via raw SQL.
	// Must temporarily disable FK checks since SQLite enforces them.
	dbProvider, ok := s.(interface{ DB() *sql.DB })
	require.True(t, ok, "store must expose DB()")
	db := dbProvider.DB()

	_, execErr := db.ExecContext(ctx, "PRAGMA foreign_keys = OFF")
	require.NoError(t, execErr)

	orphanedRoleDefID := uuid.New().String()
	const insertSQL = `INSERT INTO role_bindings ` +
		`(id, role_definition_id, principal_type, principal_id, scope_type, scope_id, created_by, created) ` +
		`VALUES (?, ?, 'user', ?, 'project', ?, 'test', datetime('now'))`
	_, execErr = db.ExecContext(ctx, insertSQL,
		uuid.New().String(), orphanedRoleDefID, targetID, projectID)
	require.NoError(t, execErr)

	_, execErr = db.ExecContext(ctx, "PRAGMA foreign_keys = ON")
	require.NoError(t, execErr)

	// Verify setup: 2 bindings for target.
	projectBindings := listProjectBindings(t, s, targetID, projectID)
	require.Len(t, projectBindings, 2, "setup: expected 2 bindings for target")

	// Run migration — should produce an error result for the orphaned binding.
	results, err := srv.membershipService.MigrateMultiRoleBindings(ctx)
	require.NoError(t, err, "MigrateMultiRoleBindings itself should not return a top-level error")

	// Find the result for target — it should have an error.
	var found bool
	for _, r := range results {
		if r.PrincipalID == targetID && r.ProjectID == projectID {
			found = true
			assert.Error(t, r.Error, "should fail closed on orphaned role definition")
			assert.Contains(t, r.Error.Error(), "orphaned",
				"error message should mention orphaned role definition")
		}
	}
	assert.True(t, found, "migration should produce a result for the orphaned binding")
}

// ---------------------------------------------------------------------------
// D-002.6: runMembershipMigration fails startup on orphaned role definition
// ---------------------------------------------------------------------------

func TestD002_RunMembershipMigration_FailsOnOrphanedRoleDef(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("d002-runorph-proj")
	ownerID := tid("d002-runorph-own")
	targetID := tid("d002-runorph-targ")

	createRS1Project(t, s, projectID, ownerID)

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: "d002-runorph@test.com",
		DisplayName: "D002 RunOrphan", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, targetID)

	// Create a legitimate custom binding.
	customRD := createCustomRoleDef(t, s, "d002-ro-legit", []string{"project.read"})
	createCustomBinding(t, s, customRD.ID, targetID, projectID)

	// Insert an orphaned binding via raw SQL (FK checks disabled temporarily).
	dbProvider, ok := s.(interface{ DB() *sql.DB })
	require.True(t, ok, "store must expose DB()")
	db := dbProvider.DB()

	_, execErr := db.ExecContext(ctx, "PRAGMA foreign_keys = OFF")
	require.NoError(t, execErr)

	orphanedRoleDefID := uuid.New().String()
	const insertSQL = `INSERT INTO role_bindings ` +
		`(id, role_definition_id, principal_type, principal_id, scope_type, scope_id, created_by, created) ` +
		`VALUES (?, ?, 'user', ?, 'project', ?, 'test', datetime('now'))`
	_, execErr = db.ExecContext(ctx, insertSQL,
		uuid.New().String(), orphanedRoleDefID, targetID, projectID)
	require.NoError(t, execErr)

	_, execErr = db.ExecContext(ctx, "PRAGMA foreign_keys = ON")
	require.NoError(t, execErr)

	// runMembershipMigration should fail closed.
	err := srv.runMembershipMigration(ctx)
	require.Error(t, err, "runMembershipMigration should fail closed on orphaned role definition")
	assert.Contains(t, err.Error(), "errors", "error should mention migration errors")
}

// ---------------------------------------------------------------------------
// D-002.7: runMembershipMigration succeeds with valid custom coexistence
// ---------------------------------------------------------------------------

func TestD002_RunMembershipMigration_SucceedsWithCustomCoexistence(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("d002-runok-proj")
	ownerID := tid("d002-runok-own")
	targetID := tid("d002-runok-targ")

	createRS1Project(t, s, projectID, ownerID)

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: "d002-runok@test.com",
		DisplayName: "D002 RunOK", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, targetID)

	// Assign a built-in membership.
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	bi, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      targetID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
	biSnap := snapshotBinding(t, s, bi)

	// Add two custom bindings.
	customRD1 := createCustomRoleDef(t, s, "d002-rok-rev", []string{"project.read"})
	customRD2 := createCustomRoleDef(t, s, "d002-rok-dep", []string{"project.deploy"})
	cb1 := createCustomBinding(t, s, customRD1.ID, targetID, projectID)
	cb2 := createCustomBinding(t, s, customRD2.ID, targetID, projectID)
	cbSnap1 := snapshotBinding(t, s, cb1)
	cbSnap2 := snapshotBinding(t, s, cb2)

	// runMembershipMigration should succeed — this is the exact state that
	// previously blocked startup before D-002.
	err = srv.runMembershipMigration(ctx)
	require.NoError(t, err, "runMembershipMigration must succeed with valid custom coexistence")

	// All bindings preserved.
	assertBindingPreserved(t, s, biSnap)
	assertBindingPreserved(t, s, cbSnap1)
	assertBindingPreserved(t, s, cbSnap2)
}

// ---------------------------------------------------------------------------
// D-002.8: Three built-in (owner + admin + member) + custom → keeps owner
// ---------------------------------------------------------------------------

func TestD002_ThreeBuiltInPlusCustom_KeepsOwner(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("d002-3bi-proj")
	ownerID := tid("d002-3bi-owner")
	targetID := tid("d002-3bi-targ")

	createRS1Project(t, s, projectID, ownerID)

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: "d002-3bi@test.com",
		DisplayName: "D002 ThreeBI", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, targetID)

	// Create a custom binding.
	customRD := createCustomRoleDef(t, s, "d002-3bi-cust", []string{"project.read"})
	cb := createCustomBinding(t, s, customRD.ID, targetID, projectID)
	cbSnap := snapshotBinding(t, s, cb)

	// Insert three built-in bindings via raw SQL.
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	adminRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)
	ownerRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)

	dbProvider, ok := s.(interface{ DB() *sql.DB })
	require.True(t, ok, "store must expose DB()")
	db := dbProvider.DB()

	_, execErr := db.ExecContext(ctx, "DROP INDEX IF EXISTS idx_rolebinding_one_membership_per_principal_per_project")
	require.NoError(t, execErr)

	const insertSQL = `INSERT INTO role_bindings ` +
		`(id, role_definition_id, principal_type, principal_id, scope_type, scope_id, membership_kind, created_by, created) ` +
		`VALUES (?, ?, 'user', ?, 'project', ?, 'builtin', ?, datetime('now'))`
	_, execErr = db.ExecContext(ctx, insertSQL,
		uuid.New().String(), memberRD.ID, targetID, projectID, ownerID)
	require.NoError(t, execErr)
	_, execErr = db.ExecContext(ctx, insertSQL,
		uuid.New().String(), adminRD.ID, targetID, projectID, ownerID)
	require.NoError(t, execErr)
	_, execErr = db.ExecContext(ctx, insertSQL,
		uuid.New().String(), ownerRD.ID, targetID, projectID, ownerID)
	require.NoError(t, execErr)

	// Run migration.
	results, err := srv.membershipService.MigrateMultiRoleBindings(ctx)
	require.NoError(t, err)

	// Should keep owner (highest authority), delete member + admin.
	var found bool
	for _, r := range results {
		if r.PrincipalID == targetID && r.ProjectID == projectID {
			found = true
			assert.NoError(t, r.Error)
			assert.Equal(t, store.ProjectRoleOwner, r.KeptRole,
				"should keep owner (highest authority)")
			assert.Equal(t, 2, r.DeletedCount,
				"should delete member + admin duplicates")
		}
	}
	assert.True(t, found, "migration should process the triple built-in duplicate")

	// Custom binding preserved.
	assertBindingPreserved(t, s, cbSnap)

	// Verify: 2 bindings remain (1 owner + 1 custom).
	projectBindings := listProjectBindings(t, s, targetID, projectID)
	assert.Len(t, projectBindings, 2, "after migration: 1 owner + 1 custom")
}

// ---------------------------------------------------------------------------
// D-002.9: Idempotency after dedup — second run is no-op
// ---------------------------------------------------------------------------

func TestD002_IdempotencyAfterDedup(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("d002-iddup-proj")
	ownerID := tid("d002-iddup-own")
	targetID := tid("d002-iddup-targ")

	createRS1Project(t, s, projectID, ownerID)

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: "d002-iddup@test.com",
		DisplayName: "D002 IdempAfterDedup", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, targetID)

	// Create custom binding.
	customRD := createCustomRoleDef(t, s, "d002-iddup-cust", []string{"project.read"})
	cb := createCustomBinding(t, s, customRD.ID, targetID, projectID)
	cbSnap := snapshotBinding(t, s, cb)

	// Insert two built-in bindings.
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	adminRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)

	dbProvider, ok := s.(interface{ DB() *sql.DB })
	require.True(t, ok)
	db := dbProvider.DB()

	_, execErr := db.ExecContext(ctx, "DROP INDEX IF EXISTS idx_rolebinding_one_membership_per_principal_per_project")
	require.NoError(t, execErr)

	const insertSQL = `INSERT INTO role_bindings ` +
		`(id, role_definition_id, principal_type, principal_id, scope_type, scope_id, membership_kind, created_by, created) ` +
		`VALUES (?, ?, 'user', ?, 'project', ?, 'builtin', ?, datetime('now'))`
	_, execErr = db.ExecContext(ctx, insertSQL,
		uuid.New().String(), memberRD.ID, targetID, projectID, ownerID)
	require.NoError(t, execErr)
	_, execErr = db.ExecContext(ctx, insertSQL,
		uuid.New().String(), adminRD.ID, targetID, projectID, ownerID)
	require.NoError(t, execErr)

	// First migration: dedup.
	results1, err := srv.membershipService.MigrateMultiRoleBindings(ctx)
	require.NoError(t, err)

	var firstFound bool
	for _, r := range results1 {
		if r.PrincipalID == targetID && r.ProjectID == projectID {
			firstFound = true
			require.NoError(t, r.Error)
			assert.Equal(t, 1, r.DeletedCount)
		}
	}
	require.True(t, firstFound, "first migration should dedup")

	// Second migration: no-op.
	results2, err := srv.membershipService.MigrateMultiRoleBindings(ctx)
	require.NoError(t, err)

	for _, r := range results2 {
		if r.PrincipalID == targetID && r.ProjectID == projectID {
			t.Errorf("second migration should be no-op, got: %+v", r)
		}
	}

	// Custom binding still preserved.
	assertBindingPreserved(t, s, cbSnap)
}
