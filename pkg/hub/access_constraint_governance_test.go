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
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// 1. Real interleavings: state change detected stale
// ---------------------------------------------------------------------------

func TestGovernance_StalePreviewDetected(t *testing.T) {
	gs, ps, _, s := govTestSetup(t)
	ctx := context.Background()

	adminID := govSeedAdminUser(t, s, "gov-admin-stale")
	userID := pvSeedUser(t, s, "gov-user-stale")

	// Give user permissions.
	rd := createTestRoleDefinition(t, s, "test-role-stale", store.RoleScopeSystem,
		[]string{"agent.read", "agent.create"})
	pvSeedRoleBinding(t, s, rd.ID, "user", userID, store.RoleScopeSystem, "")

	// Generate preview.
	draft := &store.AccessConstraint{
		Name:                 "stale-test",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(userID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "test stale detection",
		CreatedBy:            adminID,
	}

	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     pvTestActor(adminID),
	})
	require.NoError(t, err)

	// Interleave: create another constraint that changes state fingerprint.
	_, err = s.CreateAccessConstraint(ctx, &store.AccessConstraint{
		Name:               "interleaved-constraint",
		SubjectKind:        store.ConstraintSubjectAllPrincipals,
		ScopeType:          store.RoleScopeSystem,
		MaximumPermissions: []string{"agent.read", "agent.create"},
		Purpose:            "interleave",
		CreatedBy:          adminID,
	})
	require.NoError(t, err)

	// Commit should detect stale state.
	_, err = gs.CommitBoundaryChange(ctx, CommitRequest{
		Operation:    "create",
		Draft:        draft,
		PreviewToken: result.PreviewToken,
		Actor:        pvTestActor(adminID),
	})
	require.Error(t, err)
	var tve *TokenValidationError
	if errors.As(err, &tve) {
		assert.Equal(t, ErrCodePreviewStateMismatch, tve.Code)
	} else {
		var ge *GovernanceError
		if errors.As(err, &ge) {
			assert.Equal(t, ErrCodePreviewStateMismatch, ge.Code)
		}
	}
}

// ---------------------------------------------------------------------------
// 2. Two last-admin removals: at most one commits
//
// NOTE: This test verifies fingerprint-based serialization via sequential
// execution, not true goroutine-level parallelism. Both previews are
// generated against clean state; the first commit succeeds and changes the
// state fingerprint; the second commit detects the stale fingerprint and
// fails. This proves that the fingerprint mechanism serializes sequential
// interleaving, but does not prove serialization under true goroutine-level
// concurrency (where both goroutines could pass the fingerprint check before
// either commits).
// ---------------------------------------------------------------------------

func TestGovernance_TwoLastAdminRemovals_AtMostOneCommits(t *testing.T) {
	gs, ps, _, s := govTestSetup(t)
	ctx := context.Background()

	// Create two admin users.
	admin1 := govSeedAdminUser(t, s, "gov-admin-race-1")
	admin2 := govSeedAdminUser(t, s, "gov-admin-race-2")

	// Create a constraint that restricts admin1 (removing their ability to
	// admin constraints) — allowed because admin2 survives.
	draft1 := &store.AccessConstraint{
		Name:                 "lockout-race-1",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(admin1),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "lockout race 1",
		CreatedBy:            admin2,
	}

	// Generate preview for constraint on admin1.
	result1, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft1,
		Actor:     pvTestActor(admin2),
	})
	require.NoError(t, err)

	// Create a constraint that restricts admin2 — also allowed at preview time
	// because admin1 still survives.
	draft2 := &store.AccessConstraint{
		Name:                 "lockout-race-2",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(admin2),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "lockout race 2",
		CreatedBy:            admin1,
	}

	result2, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft2,
		Actor:     pvTestActor(admin1),
	})
	require.NoError(t, err)

	// Commit the first one — should succeed.
	_, err = gs.CommitBoundaryChange(ctx, CommitRequest{
		Operation:    "create",
		Draft:        draft1,
		PreviewToken: result1.PreviewToken,
		Actor:        pvTestActor(admin2),
	})
	require.NoError(t, err)

	// Commit the second one — should fail because the first commit changed
	// the state fingerprint (or the lockout check detects only one admin left).
	_, err = gs.CommitBoundaryChange(ctx, CommitRequest{
		Operation:    "create",
		Draft:        draft2,
		PreviewToken: result2.PreviewToken,
		Actor:        pvTestActor(admin1),
	})
	require.Error(t, err, "second commit should fail — state changed or lockout would occur")
}

// ---------------------------------------------------------------------------
// 3. Scheduled lockout: future lockout prevented
// ---------------------------------------------------------------------------

func TestGovernance_ScheduledLockout_Prevented(t *testing.T) {
	gs, ps, _, s := govTestSetup(t)
	ctx := context.Background()

	adminID := govSeedAdminUser(t, s, "gov-admin-sched")

	// Create a constraint with a future activation that would eventually
	// lock out the admin.
	futureTime := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	draft := &store.AccessConstraint{
		Name:               "scheduled-lockout",
		SubjectKind:        store.ConstraintSubjectAllPrincipals,
		ScopeType:          store.RoleScopeSystem,
		MaximumPermissions: []string{"agent.read"}, // Does NOT include access_constraint.admin
		NotBefore:          &futureTime,
		Purpose:            "scheduled lockout test",
		CreatedBy:          adminID,
	}

	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     pvTestActor(adminID),
	})
	require.NoError(t, err)

	// The preview should flag the lockout.
	if result.Lockout.Safe != nil {
		assert.False(t, *result.Lockout.Safe, "lockout should be unsafe for future-activating constraint")
	}

	// Commit should be blocked by the lockout invariant.
	if result.CommitBlocked != nil {
		assert.Equal(t, ErrCodeConstraintAdminLockout, result.CommitBlocked.Code)
	}

	// Even if we try to force commit, governance should reject.
	_, err = gs.CommitBoundaryChange(ctx, CommitRequest{
		Operation:    "create",
		Draft:        draft,
		PreviewToken: result.PreviewToken,
		Actor:        pvTestActor(adminID),
	})
	// Should fail — either token validation rejects (incomplete preview) or lockout check.
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// 4. Nested group delete: constraint-bearing group deletion triggers review
// ---------------------------------------------------------------------------

func TestGovernance_NestedGroupDelete_TriggersReview(t *testing.T) {
	gs, _, _, s := govTestSetup(t)
	ctx := context.Background()

	adminID := govSeedAdminUser(t, s, "gov-admin-grp-del")

	// Create a group.
	groupID := pvSeedGroup(t, s, "gov-group-constraint")
	pvSeedGroupMember(t, s, groupID, "user", adminID)

	// Create a constraint targeting this group.
	_, err := s.CreateAccessConstraint(ctx, &store.AccessConstraint{
		Name:               "group-constraint",
		SubjectKind:        store.ConstraintSubjectGroupClosure,
		SubjectGroupID:     pvStrPtr(groupID),
		ScopeType:          store.RoleScopeSystem,
		MaximumPermissions: []string{"agent.read"},
		Purpose:            "group constraint test",
		CreatedBy:          adminID,
	})
	require.NoError(t, err)

	// Check group deletion.
	check, err := gs.CheckGroupDeletion(ctx, groupID)
	require.NoError(t, err)
	assert.True(t, check.ReviewRequired, "deleting constraint-bearing group should require review")
	assert.NotEmpty(t, check.AffectedBoundaryIDs)
}

// ---------------------------------------------------------------------------
// 5. Actor self-removal: actor removing own admin caught by lockout
// ---------------------------------------------------------------------------

func TestGovernance_ActorSelfRemoval_Lockout(t *testing.T) {
	gs, ps, _, s := govTestSetup(t)
	ctx := context.Background()

	// Single admin user.
	adminID := govSeedAdminUser(t, s, "gov-admin-self-remove")

	// Try to create a constraint that applies to ALL principals and restricts
	// them to agent.read only — this would lock out every admin (including
	// the dev user from testServer and our admin user).
	draft := &store.AccessConstraint{
		Name:               "self-lockout-all",
		SubjectKind:        store.ConstraintSubjectAllPrincipals,
		ScopeType:          store.RoleScopeSystem,
		MaximumPermissions: []string{"agent.read"}, // Does NOT include access_constraint.admin
		Purpose:            "self-lockout test targeting all principals",
		CreatedBy:          adminID,
	}

	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     pvTestActor(adminID),
	})
	require.NoError(t, err)

	// The preview should identify lockout since ALL principals would lose
	// access_constraint.admin.
	if result.Lockout.Safe != nil {
		assert.False(t, *result.Lockout.Safe, "all-principals self-removal should be unsafe")
	}

	// The commit should be blocked by the lockout invariant (in preview or governance).
	if result.CommitBlocked != nil {
		assert.Equal(t, ErrCodeConstraintAdminLockout, result.CommitBlocked.Code)
	}

	_, err = gs.CommitBoundaryChange(ctx, CommitRequest{
		Operation:    "create",
		Draft:        draft,
		PreviewToken: result.PreviewToken,
		Actor:        pvTestActor(adminID),
	})
	require.Error(t, err, "self-lockout should be rejected")
}

// ---------------------------------------------------------------------------
// 6. Update moving coverage: changing subject/scope triggers re-evaluation
// ---------------------------------------------------------------------------

func TestGovernance_UpdateMovingCoverage(t *testing.T) {
	gs, ps, _, s := govTestSetup(t)
	ctx := context.Background()

	admin1 := govSeedAdminUser(t, s, "gov-admin-upd-1")
	admin2 := govSeedAdminUser(t, s, "gov-admin-upd-2")

	user1 := pvSeedUser(t, s, "gov-user-upd-1")
	rd := createTestRoleDefinition(t, s, "test-role-upd", store.RoleScopeSystem,
		[]string{"agent.read", "agent.create"})
	pvSeedRoleBinding(t, s, rd.ID, "user", user1, store.RoleScopeSystem, "")

	// Create a constraint on user1.
	draft := &store.AccessConstraint{
		Name:                 "movable-constraint",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(user1),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read", PermissionConstraintAdmin},
		Purpose:              "movable test",
		CreatedBy:            admin1,
	}
	created := govCreateAndCommit(t, gs, ps, draft, pvTestActor(admin1))

	// Now update to target admin2 instead (moving coverage).
	updateDraft := &store.AccessConstraint{
		ID:                   created.ID,
		Name:                 "movable-constraint",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(admin2),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read", PermissionConstraintAdmin},
		Purpose:              "moved to admin2",
		CreatedBy:            admin1,
	}

	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation:    "update",
		Draft:        updateDraft,
		ConstraintID: created.ID,
		BaseRevision: created.Revision,
		Actor:        pvTestActor(admin1),
	})
	require.NoError(t, err)
	assert.NotEmpty(t, result.Classification, "update should have a classification")
}

// ---------------------------------------------------------------------------
// 7. Zero-admin degraded database: zero admins is conflict, not pass
// ---------------------------------------------------------------------------

func TestGovernance_ZeroAdminDegraded_IsConflict(t *testing.T) {
	_, ps, _, s := govTestSetup(t)
	ctx := context.Background()

	// The testServer creates a dev user with super-admin, which includes
	// access_constraint.admin. To test the "zero admins" scenario, we target
	// all_principals with a constraint that excludes admin. Since the dev
	// user's super-admin binding exists, this effectively demonstrates that
	// a constraint excluding admin from all principals is detected as unsafe.
	userID := pvSeedUser(t, s, "gov-user-no-admin")

	draft := &store.AccessConstraint{
		Name:               "zero-admin-test",
		SubjectKind:        store.ConstraintSubjectAllPrincipals,
		ScopeType:          store.RoleScopeSystem,
		MaximumPermissions: []string{"agent.read"}, // Excludes access_constraint.admin
		Purpose:            "zero admin test - applies to all principals",
		CreatedBy:          userID,
	}

	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     pvTestActor(userID),
	})
	require.NoError(t, err)

	// All-principals constraint excluding admin should be unsafe:
	// every admin user would lose access_constraint.admin.
	require.NotNil(t, result.Lockout.Safe, "lockout Safe should not be nil")
	assert.False(t, *result.Lockout.Safe,
		"all-principals constraint excluding admin should be unsafe")
	require.NotNil(t, result.Lockout.RemainingActiveDirectAdmins)
	assert.Equal(t, 0, *result.Lockout.RemainingActiveDirectAdmins,
		"zero admins should survive a constraint that excludes admin from all")
}

// ---------------------------------------------------------------------------
// 8. Store failures: any store error rolls back entire transaction
// ---------------------------------------------------------------------------

// governanceErrorStore wraps a real store and injects errors.
type governanceErrorStore struct {
	store.Store
	createConstraintErr error
	updateConstraintErr error
	deleteConstraintErr error
}

func (s *governanceErrorStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	return s.Store.WithTx(ctx, func(tx store.Store) error {
		return fn(&governanceErrorStore{
			Store:               tx,
			createConstraintErr: s.createConstraintErr,
			updateConstraintErr: s.updateConstraintErr,
			deleteConstraintErr: s.deleteConstraintErr,
		})
	})
}

func (s *governanceErrorStore) CreateAccessConstraint(ctx context.Context, c *store.AccessConstraint) (*store.AccessConstraint, error) {
	if s.createConstraintErr != nil {
		return nil, s.createConstraintErr
	}
	return s.Store.CreateAccessConstraint(ctx, c)
}

func (s *governanceErrorStore) UpdateAccessConstraint(ctx context.Context, c *store.AccessConstraint, expectedRevision int64) (*store.AccessConstraint, error) {
	if s.updateConstraintErr != nil {
		return nil, s.updateConstraintErr
	}
	return s.Store.UpdateAccessConstraint(ctx, c, expectedRevision)
}

func (s *governanceErrorStore) DeleteAccessConstraint(ctx context.Context, id string) error {
	if s.deleteConstraintErr != nil {
		return s.deleteConstraintErr
	}
	return s.Store.DeleteAccessConstraint(ctx, id)
}

func TestGovernance_StoreFailure_RollsBack(t *testing.T) {
	_, ps, authz, realStore := govTestSetup(t)
	ctx := context.Background()

	adminID := govSeedAdminUser(t, realStore, "gov-admin-store-fail")
	userID := pvSeedUser(t, realStore, "gov-user-store-fail")

	rd := createTestRoleDefinition(t, realStore, "test-role-store-fail", store.RoleScopeSystem,
		[]string{"agent.read", "agent.create"})
	pvSeedRoleBinding(t, realStore, rd.ID, "user", userID, store.RoleScopeSystem, "")

	// Wrap with error-injecting store for the governance service.
	errStore := &governanceErrorStore{
		Store:               realStore,
		createConstraintErr: errors.New("simulated store failure"),
	}
	gs := NewGovernanceService(errStore, ps, authz, slog.Default())
	gs.nowFunc = ps.nowFunc

	draft := &store.AccessConstraint{
		Name:                 "store-fail-test",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(userID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read", PermissionConstraintAdmin},
		Purpose:              "store failure test",
		CreatedBy:            adminID,
	}

	// Generate preview on real store.
	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     pvTestActor(adminID),
	})
	require.NoError(t, err)

	// Commit with injected error.
	_, err = gs.CommitBoundaryChange(ctx, CommitRequest{
		Operation:    "create",
		Draft:        draft,
		PreviewToken: result.PreviewToken,
		Actor:        pvTestActor(adminID),
	})
	require.Error(t, err, "store failure should cause commit failure")
	assert.Contains(t, err.Error(), "simulated store failure")

	// Verify no constraint was created.
	constraints, err := realStore.ListAccessConstraints(ctx, 100, 0)
	require.NoError(t, err)
	for _, c := range constraints {
		assert.NotEqual(t, "store-fail-test", c.Name,
			"constraint should not exist after store failure")
	}
}

// ---------------------------------------------------------------------------
// 9. Project vs system admin containment
// ---------------------------------------------------------------------------

func TestGovernance_ProjectScopeContainment(t *testing.T) {
	gs, ps, _, s := govTestSetup(t)
	ctx := context.Background()

	adminID := govSeedAdminUser(t, s, "gov-admin-proj-scope")
	userID := pvSeedUser(t, s, "gov-user-proj-scope")
	projectID := pvSeedProject(t, s, "gov-project-scope")

	rd := createTestRoleDefinition(t, s, "test-role-proj", store.RoleScopeProject,
		[]string{"agent.read"})
	pvSeedRoleBinding(t, s, rd.ID, "user", userID, store.RoleScopeProject, projectID)

	// Create a project-scoped constraint.
	draft := &store.AccessConstraint{
		Name:                 "project-scope-test",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(userID),
		ScopeType:            store.RoleScopeProject,
		ScopeID:              projectID,
		MaximumPermissions:   []string{"agent.read", PermissionConstraintAdmin},
		Purpose:              "project scope test",
		CreatedBy:            adminID,
	}

	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     pvTestActor(adminID),
	})
	require.NoError(t, err)
	require.NotNil(t, result)

	// Verify the preview is scoped correctly.
	assert.Equal(t, "create", result.Operation)

	// Verify governance resolves scope from draft.
	scopeType, scopeID := gs.resolveScope(ctx, CommitRequest{Draft: draft})
	assert.Equal(t, store.RoleScopeProject, scopeType)
	assert.Equal(t, projectID, scopeID)
}

// ---------------------------------------------------------------------------
// 10. Concurrent operations: two simultaneous boundary creates
//
// NOTE: This test verifies fingerprint-based serialization via sequential
// execution, not true goroutine-level parallelism. Both previews are
// generated against clean state; the first commit succeeds; the second
// commit's preview token has a stale state fingerprint and is rejected.
// This proves that sequential interleaving is caught by the fingerprint
// mechanism, but does not prove serialization under true concurrent
// goroutine execution.
// ---------------------------------------------------------------------------

func TestGovernance_ConcurrentCreates_AtMostOneSucceeds(t *testing.T) {
	gs, ps, _, s := govTestSetup(t)
	ctx := context.Background()

	admin1 := govSeedAdminUser(t, s, "gov-admin-conc-1")
	admin2 := govSeedAdminUser(t, s, "gov-admin-conc-2")

	// Create drafts that each target a different admin. These constraints
	// individually are safe (the dev super-admin still survives), but the
	// preview token's state fingerprint should change after the first commit.
	draft1 := &store.AccessConstraint{
		Name:                 "concurrent-1",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(admin1),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "concurrent test 1",
		CreatedBy:            admin2,
	}
	draft2 := &store.AccessConstraint{
		Name:                 "concurrent-2",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(admin2),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "concurrent test 2",
		CreatedBy:            admin1,
	}

	// Generate previews sequentially (both against clean state).
	result1, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft1,
		Actor:     pvTestActor(admin2),
	})
	require.NoError(t, err)

	result2, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft2,
		Actor:     pvTestActor(admin1),
	})
	require.NoError(t, err)

	// Commit the first one — should succeed.
	_, err = gs.CommitBoundaryChange(ctx, CommitRequest{
		Operation:    "create",
		Draft:        draft1,
		PreviewToken: result1.PreviewToken,
		Actor:        pvTestActor(admin2),
	})
	require.NoError(t, err, "first concurrent commit should succeed")

	// The second commit should fail because the state fingerprint changed
	// (the first commit added a constraint, which changes the fingerprint).
	_, err = gs.CommitBoundaryChange(ctx, CommitRequest{
		Operation:    "create",
		Draft:        draft2,
		PreviewToken: result2.PreviewToken,
		Actor:        pvTestActor(admin1),
	})
	require.Error(t, err, "second commit should fail because state changed")
}

// ---------------------------------------------------------------------------
// 11. Adjacent domain gates
// ---------------------------------------------------------------------------

func TestGovernance_GroupMemberRemoval_SecurityReviewRequired(t *testing.T) {
	gs, _, _, s := govTestSetup(t)
	ctx := context.Background()

	adminID := govSeedAdminUser(t, s, "gov-admin-grp-member")
	groupID := pvSeedGroup(t, s, "gov-group-member-rm")
	pvSeedGroupMember(t, s, groupID, "user", adminID)

	// Create a constraint referencing this group.
	_, err := s.CreateAccessConstraint(ctx, &store.AccessConstraint{
		Name:               "group-member-constraint",
		SubjectKind:        store.ConstraintSubjectGroupClosure,
		SubjectGroupID:     pvStrPtr(groupID),
		ScopeType:          store.RoleScopeSystem,
		MaximumPermissions: []string{"agent.read"},
		Purpose:            "group member test",
		CreatedBy:          adminID,
	})
	require.NoError(t, err)

	// Check group member removal.
	check, err := gs.CheckGroupMemberRemoval(ctx, groupID, "user", adminID)
	require.NoError(t, err)
	assert.True(t, check.ReviewRequired, "removing member from constraint-bearing group should require review")
	assert.NotEmpty(t, check.AffectedBoundaryIDs)
}

func TestGovernance_GroupMemberRemoval_NoReviewForNonConstraintGroup(t *testing.T) {
	gs, _, _, s := govTestSetup(t)
	ctx := context.Background()

	userID := pvSeedUser(t, s, "gov-user-no-constraint")
	groupID := pvSeedGroup(t, s, "gov-group-no-constraint")
	pvSeedGroupMember(t, s, groupID, "user", userID)

	// No constraint references this group.
	check, err := gs.CheckGroupMemberRemoval(ctx, groupID, "user", userID)
	require.NoError(t, err)
	assert.False(t, check.ReviewRequired, "non-constraint group should not require review")
}

func TestGovernance_RoleBindingChange_SecurityReviewRequired(t *testing.T) {
	gs, _, _, s := govTestSetup(t)
	ctx := context.Background()

	// Create a role with constraint-admin.
	rd := createTestRoleDefinition(t, s, "admin-role-binding-check", store.RoleScopeSystem,
		[]string{PermissionConstraintAdmin})

	// Check if modifying a binding with this role triggers review.
	check, err := gs.CheckRoleBindingChange(ctx, "binding-id", rd.ID, true)
	require.NoError(t, err)
	assert.True(t, check.ReviewRequired, "removing admin role binding should require review")
}

func TestGovernance_RoleBindingChange_NoReviewForNonAdminRole(t *testing.T) {
	gs, _, _, s := govTestSetup(t)
	ctx := context.Background()

	// Create a role without constraint-admin.
	rd := createTestRoleDefinition(t, s, "basic-role-binding-check", store.RoleScopeSystem,
		[]string{"agent.read"})

	check, err := gs.CheckRoleBindingChange(ctx, "binding-id", rd.ID, true)
	require.NoError(t, err)
	assert.False(t, check.ReviewRequired, "non-admin role binding should not require review")
}

// ---------------------------------------------------------------------------
// 11b. Constraint-admin lockout on user suspension and deletion
// ---------------------------------------------------------------------------

// lockoutProjectAdmins seeds a project and n users, each holding a
// project-scoped role with access_constraint.admin. Project scope keeps
// the dev user's system-scoped super-admin binding out of the count.
func lockoutProjectAdmins(t *testing.T, s store.Store, name string, n int) (string, []string, []*store.RoleBinding) {
	t.Helper()
	ctx := context.Background()
	projectID := pvSeedProject(t, s, name+"-project")
	rd := createTestRoleDefinition(t, s, name+"-admin-role", store.RoleScopeProject,
		[]string{PermissionConstraintAdmin, "agent.read"})
	var users []string
	var bindings []*store.RoleBinding
	for i := 0; i < n; i++ {
		userID := pvSeedUser(t, s, fmt.Sprintf("%s-admin-%d", name, i))
		b, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: rd.ID,
			PrincipalType:    "user",
			PrincipalID:      userID,
			ScopeType:        store.RoleScopeProject,
			ScopeID:          projectID,
			CreatedBy:        "test",
		})
		require.NoError(t, err)
		users = append(users, userID)
		bindings = append(bindings, b)
	}
	return projectID, users, bindings
}

func requireUserStatus(t *testing.T, s store.Store, userID, want string) {
	t.Helper()
	u, err := s.GetUser(context.Background(), userID)
	require.NoError(t, err)
	assert.Equal(t, want, u.Status)
}

func TestConstraintAdminLockout_SuspendLastAdminRefused(t *testing.T) {
	srv, s := testServer(t)
	_, users, _ := lockoutProjectAdmins(t, s, "lockout-suspend-last", 1)

	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/users/"+users[0],
		map[string]string{"status": "suspended"})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Equal(t, ErrCodeConstraintAdminLockout, decodeErrorCode(t, rec.Body.Bytes()))
	assert.Contains(t, rec.Body.String(), "suspending this user")
	requireUserStatus(t, s, users[0], store.UserStatusActive)
}

func TestConstraintAdminLockout_SuspendWithOtherAdminAllowed(t *testing.T) {
	srv, s := testServer(t)
	_, users, _ := lockoutProjectAdmins(t, s, "lockout-suspend-ok", 2)

	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/users/"+users[0],
		map[string]string{"status": "suspended"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	requireUserStatus(t, s, users[0], store.UserStatusSuspended)

	// The remaining admin is now the last active one.
	rec = doRequest(t, srv, http.MethodPatch, "/api/v1/users/"+users[1],
		map[string]string{"status": "suspended"})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Equal(t, ErrCodeConstraintAdminLockout, decodeErrorCode(t, rec.Body.Bytes()))
	requireUserStatus(t, s, users[1], store.UserStatusActive)
}

func TestConstraintAdminLockout_SuspendNonAdminAllowed(t *testing.T) {
	srv, s := testServer(t)
	lockoutProjectAdmins(t, s, "lockout-suspend-nonadmin", 1)
	userID := govSeedNonAdminUser(t, s, "lockout-suspend-nonadmin-user")

	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/users/"+userID,
		map[string]string{"status": "suspended"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	requireUserStatus(t, s, userID, store.UserStatusSuspended)
}

func TestConstraintAdminLockout_DeleteLastAdminRefused(t *testing.T) {
	srv, s := testServer(t)
	_, users, _ := lockoutProjectAdmins(t, s, "lockout-delete-last", 1)

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/users/"+users[0], nil)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Equal(t, ErrCodeConstraintAdminLockout, decodeErrorCode(t, rec.Body.Bytes()))
	assert.Contains(t, rec.Body.String(), "deleting this user")
	requireUserStatus(t, s, users[0], store.UserStatusActive)
}

func TestConstraintAdminLockout_DeleteWithOtherAdminAllowed(t *testing.T) {
	srv, s := testServer(t)
	_, users, _ := lockoutProjectAdmins(t, s, "lockout-delete-ok", 2)

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/users/"+users[0], nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	_, err := s.GetUser(context.Background(), users[0])
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestConstraintAdminLockout_DeleteWhenOtherAdminSuspendedRefused(t *testing.T) {
	srv, s := testServer(t)
	_, users, _ := lockoutProjectAdmins(t, s, "lockout-delete-susp", 2)
	u, err := s.GetUser(context.Background(), users[1])
	require.NoError(t, err)
	u.Status = store.UserStatusSuspended
	require.NoError(t, s.UpdateUser(context.Background(), u))

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/users/"+users[0], nil)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Equal(t, ErrCodeConstraintAdminLockout, decodeErrorCode(t, rec.Body.Bytes()))
	requireUserStatus(t, s, users[0], store.UserStatusActive)
}

// Suspending one admin and then downgrading the other is refused at the
// second step: a suspended admin does not count as remaining.
func TestConstraintAdminLockout_SuspendThenDowngradeRefused(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	projectID, users, bindings := lockoutProjectAdmins(t, s, "lockout-two-step", 2)
	ps := NewPreviewServiceWithKey(s, srv.authzService, slog.Default(), []byte("test-governance-hmac-key-32byte!"))
	gs := NewGovernanceService(s, ps, srv.authzService, slog.Default())

	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/users/"+users[0],
		map[string]string{"status": "suspended"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	basicRD := createTestRoleDefinition(t, s, "lockout-two-step-basic", store.RoleScopeProject,
		[]string{"agent.read"})
	_, err := gs.ReplaceRoleBinding(ctx, bindings[1].ID, &store.RoleBinding{
		RoleDefinitionID: basicRD.ID,
		PrincipalType:    "user",
		PrincipalID:      users[1],
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	}, "")
	var ge *GovernanceError
	require.ErrorAs(t, err, &ge)
	assert.Equal(t, ErrCodeConstraintAdminLockout, ge.Code)
	_, err = s.GetRoleBinding(ctx, bindings[1].ID)
	assert.NoError(t, err, "the admin binding is kept")
}

// The shared check counts admins holding access_constraint.admin through
// a group, and ignores users who are not admins.
func TestConstraintAdminLockout_UserRemovalCheck(t *testing.T) {
	_, _, _, s := govTestSetup(t)
	ctx := context.Background()
	now := time.Now()
	projectID := pvSeedProject(t, s, "lockout-group-project")
	rd := createTestRoleDefinition(t, s, "lockout-group-admin-role", store.RoleScopeProject,
		[]string{PermissionConstraintAdmin})
	groupID := pvSeedGroup(t, s, "lockout-group")
	pvSeedRoleBinding(t, s, rd.ID, "group", groupID, store.RoleScopeProject, projectID)
	member := pvSeedUser(t, s, "lockout-group-member")
	pvSeedGroupMember(t, s, groupID, store.GroupMemberTypeUser, member)

	err := checkUserRemovalAdminLockout(ctx, s, now, member, userRemovalSuspend)
	var ge *GovernanceError
	require.ErrorAs(t, err, &ge, "the only group-derived admin cannot be removed")
	assert.Equal(t, ErrCodeConstraintAdminLockout, ge.Code)

	other := pvSeedUser(t, s, "lockout-group-member-2")
	pvSeedGroupMember(t, s, groupID, store.GroupMemberTypeUser, other)
	assert.NoError(t, checkUserRemovalAdminLockout(ctx, s, now, member, userRemovalSuspend))

	nonAdmin := govSeedNonAdminUser(t, s, "lockout-group-nonadmin")
	assert.NoError(t, checkUserRemovalAdminLockout(ctx, s, now, nonAdmin, userRemovalSuspend))
}

// ---------------------------------------------------------------------------
// 12. RoleBinding replacement
// ---------------------------------------------------------------------------

func TestGovernance_ReplaceRoleBinding_Atomic(t *testing.T) {
	gs, _, _, s := govTestSetup(t)
	ctx := context.Background()

	adminID := govSeedAdminUser(t, s, "gov-admin-replace-rb")

	// Create an admin role binding to downgrade.
	rd := createTestRoleDefinition(t, s, "admin-role-replace", store.RoleScopeSystem,
		[]string{PermissionConstraintAdmin, "agent.read"})
	oldBinding, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    "user",
		PrincipalID:      adminID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Create a basic role for the replacement.
	basicRD := createTestRoleDefinition(t, s, "basic-role-replace", store.RoleScopeSystem,
		[]string{"agent.read"})

	newBinding := &store.RoleBinding{
		RoleDefinitionID: basicRD.ID,
		PrincipalType:    "user",
		PrincipalID:      adminID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	}

	// Should succeed since admin has another admin binding from govSeedAdminUser.
	result, err := gs.ReplaceRoleBinding(ctx, oldBinding.ID, newBinding, "")
	require.NoError(t, err)
	require.NotNil(t, result)

	// Old binding should be gone.
	_, err = s.GetRoleBinding(ctx, oldBinding.ID)
	assert.Error(t, err, "old binding should be deleted")

	// New binding should exist.
	_, err = s.GetRoleBinding(ctx, result.ID)
	assert.NoError(t, err, "new binding should exist")
}

func TestGovernance_ReplaceRoleBinding_LastAdminBlocked(t *testing.T) {
	gs, _, _, s := govTestSetup(t)
	ctx := context.Background()

	// The testServer seeds a dev user with super-admin (which includes
	// access_constraint.admin). The lockout check sees multiple admins,
	// so we verify the check works by checking the function directly.
	// Instead, we test the mechanics: when the only admin binding for a
	// user is downgraded and that user has no other admin bindings, the
	// enforceRoleBindingLockout function is called and counts surviving admins.

	// Create a user with exactly ONE admin binding.
	userID := pvSeedUser(t, s, "gov-user-last-admin-rb")
	adminRD := createTestRoleDefinition(t, s, "single-admin-role-rb", store.RoleScopeSystem,
		[]string{PermissionConstraintAdmin})
	binding, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: adminRD.ID,
		PrincipalType:    "user",
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Try to replace with non-admin role. The dev user still has admin,
	// so the overall lockout check passes (surviving > 0). This is correct
	// behavior: there IS still an admin (the dev user), so the replace
	// is allowed.
	basicRD := createTestRoleDefinition(t, s, "basic-role-last-admin-rb", store.RoleScopeSystem,
		[]string{"agent.read"})
	newBinding := &store.RoleBinding{
		RoleDefinitionID: basicRD.ID,
		PrincipalType:    "user",
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	}

	result, err := gs.ReplaceRoleBinding(ctx, binding.ID, newBinding, "")
	// Should succeed because the dev user (super-admin) still has admin.
	require.NoError(t, err, "replace should succeed when another admin exists (dev user)")
	require.NotNil(t, result)

	// Old binding should be gone.
	_, err = s.GetRoleBinding(ctx, binding.ID)
	assert.Error(t, err, "old binding should be deleted")
}

// ---------------------------------------------------------------------------
// 12b. ReplaceRoleBinding lockout: actually blocked when last admin is downgraded
// ---------------------------------------------------------------------------

func TestGovernance_ReplaceRoleBinding_LastAdminActuallyBlocked(t *testing.T) {
	gs, _, _, s := govTestSetup(t)
	ctx := context.Background()

	// Use project scope so the dev user's system-scoped super-admin binding
	// is NOT visible to resolveAdminUsers. This isolates the lockout check
	// to project-scoped admins only.
	projectID := pvSeedProject(t, s, "gov-project-lockout-rb")

	userID := pvSeedUser(t, s, "gov-user-lockout-rb")
	adminRD := createTestRoleDefinition(t, s, "proj-admin-role-lockout-rb", store.RoleScopeProject,
		[]string{PermissionConstraintAdmin, "agent.read"})
	binding, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: adminRD.ID,
		PrincipalType:    "user",
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Create a non-admin role for the replacement.
	basicRD := createTestRoleDefinition(t, s, "proj-basic-role-lockout-rb", store.RoleScopeProject,
		[]string{"agent.read"})
	newBinding := &store.RoleBinding{
		RoleDefinitionID: basicRD.ID,
		PrincipalType:    "user",
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	}

	// Replace should fail because this user is the only admin at project scope.
	_, err = gs.ReplaceRoleBinding(ctx, binding.ID, newBinding, "")
	require.Error(t, err, "replace should fail when it would leave zero admins at project scope")

	var ge *GovernanceError
	require.ErrorAs(t, err, &ge)
	assert.Equal(t, ErrCodeConstraintAdminLockout, ge.Code)
}

// ---------------------------------------------------------------------------
// 13. Permission lost between preview and commit
// ---------------------------------------------------------------------------

func TestGovernance_MutationPermissionLost(t *testing.T) {
	gs, ps, _, s := govTestSetup(t)
	ctx := context.Background()

	// Create admin and give them a revocable admin binding.
	userID := pvSeedUser(t, s, "gov-admin-perm-lost")
	adminRD := createTestRoleDefinition(t, s, "revocable-admin-role", store.RoleScopeSystem,
		[]string{PermissionConstraintAdmin, "agent.read"})
	adminBinding, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: adminRD.ID,
		PrincipalType:    "user",
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Also create a second admin so lockout isn't the issue.
	govSeedAdminUser(t, s, "gov-admin-other-perm-lost")

	targetUser := pvSeedUser(t, s, "gov-user-target-perm-lost")
	rd := createTestRoleDefinition(t, s, "test-role-perm-lost", store.RoleScopeSystem,
		[]string{"agent.read"})
	pvSeedRoleBinding(t, s, rd.ID, "user", targetUser, store.RoleScopeSystem, "")

	draft := &store.AccessConstraint{
		Name:                 "perm-lost-test",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(targetUser),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read", PermissionConstraintAdmin},
		Purpose:              "permission lost test",
		CreatedBy:            userID,
	}

	// Generate preview while user still has admin.
	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     pvTestActor(userID),
	})
	require.NoError(t, err)

	// Revoke admin permission between preview and commit.
	err = s.DeleteRoleBinding(ctx, adminBinding.ID)
	require.NoError(t, err)

	// Commit should detect permission loss.
	_, err = gs.CommitBoundaryChange(ctx, CommitRequest{
		Operation:    "create",
		Draft:        draft,
		PreviewToken: result.PreviewToken,
		Actor:        pvTestActor(userID),
	})
	require.Error(t, err, "commit should fail when actor lost admin permission")
	// The error may be from re-authorization or stale state detection.
}

// ---------------------------------------------------------------------------
// 14. Relaxation authority enforcement
// ---------------------------------------------------------------------------

func TestGovernance_RelaxationAuthority_InsufficientAuthority(t *testing.T) {
	gs, ps, _, s := govTestSetup(t)
	ctx := context.Background()

	adminID := govSeedAdminUser(t, s, "gov-admin-relax-auth")
	userID := pvSeedUser(t, s, "gov-user-relax-target")

	rd := createTestRoleDefinition(t, s, "test-role-relax", store.RoleScopeSystem,
		[]string{"agent.read", "agent.create"})
	pvSeedRoleBinding(t, s, rd.ID, "user", userID, store.RoleScopeSystem, "")

	// Create a tight constraint.
	tightDraft := &store.AccessConstraint{
		Name:                 "tight-constraint",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(userID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read", PermissionConstraintAdmin},
		Purpose:              "tight constraint",
		CreatedBy:            adminID,
	}
	created := govCreateAndCommit(t, gs, ps, tightDraft, pvTestActor(adminID))

	// Now try to update to add agent.create (relaxation).
	relaxDraft := &store.AccessConstraint{
		ID:                   created.ID,
		Name:                 "tight-constraint",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(userID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read", "agent.create", PermissionConstraintAdmin},
		Purpose:              "relaxed constraint",
		CreatedBy:            adminID,
	}

	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation:    "update",
		Draft:        relaxDraft,
		ConstraintID: created.ID,
		BaseRevision: created.Revision,
		Actor:        pvTestActor(adminID),
	})
	require.NoError(t, err)

	// The classification should reflect relaxation.
	assert.Contains(t, []string{ClassificationRelax, ClassificationMixed}, result.Classification)
}

// ---------------------------------------------------------------------------
// 14b. Relaxation authority blocks delete when actor lacks authority
// ---------------------------------------------------------------------------

func TestGovernance_DeleteRelaxationAuthority_InsufficientAuthority(t *testing.T) {
	gs, ps, _, s := govTestSetup(t)
	ctx := context.Background()

	// Create an admin user whose effective permissions do NOT include all the
	// permissions in the boundary's max set (specifically agent.delete).
	adminID := pvSeedUser(t, s, "gov-admin-relax-del")
	adminRD := createTestRoleDefinition(t, s, "partial-admin-role-del", store.RoleScopeSystem,
		[]string{PermissionConstraintAdmin, "agent.read", "agent.create"})
	pvSeedRoleBinding(t, s, adminRD.ID, "user", adminID, store.RoleScopeSystem, "")

	// Also ensure another admin exists so lockout is not the issue.
	govSeedAdminUser(t, s, "gov-admin-relax-del-other")

	userID := pvSeedUser(t, s, "gov-user-relax-del-target")
	basicRD := createTestRoleDefinition(t, s, "basic-role-relax-del", store.RoleScopeSystem,
		[]string{"agent.read", "agent.create", "agent.delete"})
	pvSeedRoleBinding(t, s, basicRD.ID, "user", userID, store.RoleScopeSystem, "")

	// Create a constraint with max permissions that include agent.delete
	// (a permission the admin does NOT hold).
	draft := &store.AccessConstraint{
		Name:                 "relax-del-constraint",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(userID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read", "agent.delete", PermissionConstraintAdmin},
		Purpose:              "relaxation delete authority test",
		CreatedBy:            adminID,
	}
	created := govCreateAndCommit(t, gs, ps, draft, pvTestActor(adminID))

	// Now try to delete the constraint. Delete is ClassificationRelax, so
	// checkRelaxationAuthority runs. The actor lacks "agent.delete" which is
	// in the boundary's max set, so delete should be blocked.
	deletePreview, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation:    "delete",
		ConstraintID: created.ID,
		BaseRevision: created.Revision,
		Actor:        pvTestActor(adminID),
	})
	require.NoError(t, err)

	_, err = gs.CommitBoundaryChange(ctx, CommitRequest{
		Operation:    "delete",
		ConstraintID: created.ID,
		BaseRevision: created.Revision,
		PreviewToken: deletePreview.PreviewToken,
		Actor:        pvTestActor(adminID),
	})
	require.Error(t, err, "delete should fail when actor lacks authority over boundary's max permissions")

	var ge *GovernanceError
	require.ErrorAs(t, err, &ge)
	assert.Equal(t, ErrCodeInsufficientRelaxationAuthority, ge.Code)
}

// ---------------------------------------------------------------------------
// 14c. Scheduled lockout blocks RoleBinding change
// ---------------------------------------------------------------------------

func TestGovernance_ReplaceRoleBinding_ScheduledLockout(t *testing.T) {
	gs, _, _, s := govTestSetup(t)
	ctx := context.Background()

	// Use project scope so the dev user's system-scoped super-admin binding
	// is NOT visible to resolveAdminUsers.
	projectID := pvSeedProject(t, s, "gov-project-sched-lockout-rb")

	// Create two admin users at project scope.
	user1 := pvSeedUser(t, s, "gov-user-sched-rb-1")
	user2 := pvSeedUser(t, s, "gov-user-sched-rb-2")
	adminRD := createTestRoleDefinition(t, s, "proj-admin-role-sched-rb", store.RoleScopeProject,
		[]string{PermissionConstraintAdmin, "agent.read"})
	binding1, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: adminRD.ID,
		PrincipalType:    "user",
		PrincipalID:      user1,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: adminRD.ID,
		PrincipalType:    "user",
		PrincipalID:      user2,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Create a scheduled constraint that targets user2 (the other admin),
	// excluding access_constraint.admin. When this activates, user2 will
	// lose admin. If we also remove user1's admin binding, zero admins survive.
	futureTime := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	_, err = s.CreateAccessConstraint(ctx, &store.AccessConstraint{
		Name:                 "scheduled-rb-lockout",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(user2),
		ScopeType:            store.RoleScopeProject,
		ScopeID:              projectID,
		MaximumPermissions:   []string{"agent.read"}, // Excludes access_constraint.admin
		NotBefore:            &futureTime,
		Purpose:              "scheduled lockout for RoleBinding test",
		CreatedBy:            "test",
	})
	require.NoError(t, err)

	// Try to replace user1's admin binding with a non-admin role.
	basicRD := createTestRoleDefinition(t, s, "proj-basic-role-sched-rb", store.RoleScopeProject,
		[]string{"agent.read"})
	newBinding := &store.RoleBinding{
		RoleDefinitionID: basicRD.ID,
		PrincipalType:    "user",
		PrincipalID:      user1,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	}

	// Without the scheduled-state check this would pass (2 admins currently,
	// user2 survives). With scheduled-state check, user2 is blocked by the
	// future constraint, leaving zero admins → lockout error.
	_, err = gs.ReplaceRoleBinding(ctx, binding1.ID, newBinding, "")
	require.Error(t, err, "replace should fail: scheduled constraint would block remaining admin")

	var ge *GovernanceError
	require.ErrorAs(t, err, &ge)
	assert.Equal(t, ErrCodeConstraintAdminLockout, ge.Code)
}

// ---------------------------------------------------------------------------
// 15. Error code constants are correctly defined
// ---------------------------------------------------------------------------

func TestGovernance_ErrorCodeConstants(t *testing.T) {
	// Verify all B5 error codes exist and use lower_snake_case.
	codes := map[string]string{
		"ErrCodeConstraintAdminLockout":          ErrCodeConstraintAdminLockout,
		"ErrCodeStaleAuthorizationPreview":       ErrCodeStaleAuthorizationPreview,
		"ErrCodeInsufficientRelaxationAuthority": ErrCodeInsufficientRelaxationAuthority,
		"ErrCodeMutationPermissionLost":          ErrCodeMutationPermissionLost,
		"ErrCodeSecurityReviewRequired":          ErrCodeSecurityReviewRequired,
	}

	for name, code := range codes {
		assert.NotEmpty(t, code, "%s should not be empty", name)
		// Verify lower_snake_case convention.
		for _, c := range code {
			if c >= 'A' && c <= 'Z' {
				t.Errorf("%s = %q contains uppercase letter, violating lower_snake_case convention", name, code)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// 16. Group closure constraint triggers review
// ---------------------------------------------------------------------------

func TestGovernance_GroupClosureConstraint_TriggersReview(t *testing.T) {
	gs, _, _, s := govTestSetup(t)
	ctx := context.Background()

	adminID := govSeedAdminUser(t, s, "gov-admin-grp-closure-trigger")
	groupID := pvSeedGroup(t, s, "gov-group-closure-constraint-trigger")
	pvSeedGroupMember(t, s, groupID, "user", adminID)

	// Create a constraint with group_closure kind targeting the group.
	// (exact-group principal subjects are no longer accepted — groups are
	// collection resources with no identity.)
	_, err := s.CreateAccessConstraint(ctx, &store.AccessConstraint{
		Name:               "closure-group-constraint",
		SubjectKind:        store.ConstraintSubjectGroupClosure,
		SubjectGroupID:     pvStrPtr(groupID),
		ScopeType:          store.RoleScopeSystem,
		MaximumPermissions: []string{"agent.read"},
		Purpose:            "group closure review test",
		CreatedBy:          adminID,
	})
	require.NoError(t, err)

	// Check group member removal.
	check, err := gs.CheckGroupMemberRemoval(ctx, groupID, "user", adminID)
	require.NoError(t, err)
	assert.True(t, check.ReviewRequired, "group referenced as closure subject should trigger review")
}

// ---------------------------------------------------------------------------
// 17. Successful create/update/delete flow
// ---------------------------------------------------------------------------

func TestGovernance_FullCreateUpdateDeleteFlow(t *testing.T) {
	gs, ps, _, s := govTestSetup(t)
	ctx := context.Background()

	adminID := govSeedAdminUser(t, s, "gov-admin-full-flow")
	userID := pvSeedUser(t, s, "gov-user-full-flow")

	rd := createTestRoleDefinition(t, s, "test-role-full-flow", store.RoleScopeSystem,
		[]string{"agent.read", "agent.create", "agent.delete"})
	pvSeedRoleBinding(t, s, rd.ID, "user", userID, store.RoleScopeSystem, "")

	// CREATE
	createDraft := &store.AccessConstraint{
		Name:                 "full-flow-constraint",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(userID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read", "agent.create", PermissionConstraintAdmin},
		Purpose:              "full flow test",
		CreatedBy:            adminID,
	}

	createPreview, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     createDraft,
		Actor:     pvTestActor(adminID),
	})
	require.NoError(t, err)

	createResult, err := gs.CommitBoundaryChange(ctx, CommitRequest{
		Operation:    "create",
		Draft:        createDraft,
		PreviewToken: createPreview.PreviewToken,
		Actor:        pvTestActor(adminID),
	})
	require.NoError(t, err)
	require.NotNil(t, createResult.Constraint)
	assert.Equal(t, "create", createResult.Operation)
	assert.Equal(t, "full-flow-constraint", createResult.Constraint.Name)

	// UPDATE
	updateDraft := &store.AccessConstraint{
		ID:                   createResult.Constraint.ID,
		Name:                 "full-flow-constraint-updated",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(userID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read", PermissionConstraintAdmin},
		Purpose:              "full flow updated",
		CreatedBy:            adminID,
	}

	updatePreview, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation:    "update",
		Draft:        updateDraft,
		ConstraintID: createResult.Constraint.ID,
		BaseRevision: createResult.Constraint.Revision,
		Actor:        pvTestActor(adminID),
	})
	require.NoError(t, err)

	updateResult, err := gs.CommitBoundaryChange(ctx, CommitRequest{
		Operation:    "update",
		Draft:        updateDraft,
		ConstraintID: createResult.Constraint.ID,
		BaseRevision: createResult.Constraint.Revision,
		PreviewToken: updatePreview.PreviewToken,
		Actor:        pvTestActor(adminID),
	})
	require.NoError(t, err)
	require.NotNil(t, updateResult.Constraint)
	assert.Equal(t, "update", updateResult.Operation)
	assert.Equal(t, "full-flow-constraint-updated", updateResult.Constraint.Name)

	// DELETE
	deletePreview, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation:    "delete",
		ConstraintID: updateResult.Constraint.ID,
		BaseRevision: updateResult.Constraint.Revision,
		Actor:        pvTestActor(adminID),
	})
	require.NoError(t, err)

	deleteResult, err := gs.CommitBoundaryChange(ctx, CommitRequest{
		Operation:    "delete",
		ConstraintID: updateResult.Constraint.ID,
		BaseRevision: updateResult.Constraint.Revision,
		PreviewToken: deletePreview.PreviewToken,
		Actor:        pvTestActor(adminID),
	})
	require.NoError(t, err)
	assert.Equal(t, "delete", deleteResult.Operation)

	// Verify constraint is gone.
	_, err = s.GetAccessConstraint(ctx, updateResult.Constraint.ID)
	assert.Error(t, err, "constraint should be deleted")
}

// ---------------------------------------------------------------------------
// 18. GovernanceError is properly typed
// ---------------------------------------------------------------------------

func TestGovernanceError_Interface(t *testing.T) {
	err := &GovernanceError{
		Code:    ErrCodeConstraintAdminLockout,
		Message: "test lockout message",
		Details: map[string]interface{}{"key": "value"},
	}

	assert.Equal(t, "test lockout message", err.Error())
	assert.Equal(t, ErrCodeConstraintAdminLockout, err.Code)
	assert.NotNil(t, err.Details)

	// Verify it satisfies the error interface.
	var e error = err
	assert.NotNil(t, e)
}

// ---------------------------------------------------------------------------
// 11c. Constraint-admin lockout on role-binding delete paths
// ---------------------------------------------------------------------------

func requireBindingExists(t *testing.T, s store.Store, bindingID string, want bool) {
	t.Helper()
	_, err := s.GetRoleBinding(context.Background(), bindingID)
	if want {
		assert.NoError(t, err, "binding %s should be kept", bindingID)
	} else {
		assert.ErrorIs(t, err, store.ErrNotFound, "binding %s should be deleted", bindingID)
	}
}

func requireLockoutRefusal(t *testing.T, code int, body []byte) {
	t.Helper()
	require.Equal(t, http.StatusConflict, code, string(body))
	assert.Equal(t, ErrCodeConstraintAdminLockout, decodeErrorCode(t, body))
}

// lockoutSystemBinding creates a system-scoped binding of the named
// built-in role for a new active user.
func lockoutSystemBinding(t *testing.T, s store.Store, roleName, name string) *store.RoleBinding {
	t.Helper()
	ctx := context.Background()
	rd, err := s.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeSystem)
	require.NoError(t, err)
	b, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      pvSeedUser(t, s, name),
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        store.SystemReconcileCreatedBy,
	})
	require.NoError(t, err)
	return b
}

// lockoutSuspendDevUser keeps the dev user's own super-admin binding out of
// the system-scope admin count. The request still runs as the dev user.
// The fixture isolates the lockout guards: the authorization decision
// reads the dev user as active, so the request reaches the guards, while
// the guards' transaction reads the stored suspended row. A suspended actor
// is refused before the guards;
// TestConstraintAdminLockout_RoleBindingDeleteSuspendedActorRefused pins
// that.
func lockoutSuspendDevUser(t *testing.T, srv *Server, s store.Store) {
	t.Helper()
	ctx := context.Background()
	dev := getDevUser(t, srv, s)
	srv.ensureSuperAdminBinding(ctx, dev.ID)
	dev.Status = store.UserStatusSuspended
	require.NoError(t, s.UpdateUser(ctx, dev))
	authzSeesUserActive(t, srv, dev.ID)
}

// TestConstraintAdminLockout_RoleBindingDeleteSuspendedActorRefused follows
// TestConstraintAdminLockout_RoleBindingDeleteSystem without a stub: the
// suspended dev user's own stored row is what authorization reads. The
// account-status check refuses the request with 403 user_suspended in both
// of that test's states: with two hub-admin bindings (where it gets 204)
// and, after a is removed directly in the store, with b as the last one
// (where it gets 409 constraint_admin_lockout). The bindings the requests name are kept.
func TestConstraintAdminLockout_RoleBindingDeleteSuspendedActorRefused(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	dev := getDevUser(t, srv, s)
	srv.ensureSuperAdminBinding(ctx, dev.ID)
	dev.Status = store.UserStatusSuspended
	require.NoError(t, s.UpdateUser(ctx, dev))
	a := lockoutSystemBinding(t, s, store.SystemRoleHubAdmin, "lockout-rb-susp-a")
	b := lockoutSystemBinding(t, s, store.SystemRoleHubAdmin, "lockout-rb-susp-b")

	// The would-be-204 state: two hub-admin bindings.
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/admin/role-bindings/"+a.ID, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, "user_suspended", decodeErrorCode(t, rec.Body.Bytes()))
	requireBindingExists(t, s, a.ID, true)

	// The would-be-409 state: b is the last hub-admin binding.
	require.NoError(t, s.DeleteRoleBinding(ctx, a.ID))
	rec = doRequest(t, srv, http.MethodDelete, "/api/v1/admin/role-bindings/"+b.ID, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, "user_suspended", decodeErrorCode(t, rec.Body.Bytes()))
	requireBindingExists(t, s, b.ID, true)
}

// DELETE /admin/role-bindings/{id}, system scope, non-super-admin role.
func TestConstraintAdminLockout_RoleBindingDeleteSystem(t *testing.T) {
	srv, s := testServer(t)
	lockoutSuspendDevUser(t, srv, s)
	a := lockoutSystemBinding(t, s, store.SystemRoleHubAdmin, "lockout-rb-sys-a")
	b := lockoutSystemBinding(t, s, store.SystemRoleHubAdmin, "lockout-rb-sys-b")

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/admin/role-bindings/"+a.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	requireBindingExists(t, s, a.ID, false)

	rec = doRequest(t, srv, http.MethodDelete, "/api/v1/admin/role-bindings/"+b.ID, nil)
	requireLockoutRefusal(t, rec.Code, rec.Body.Bytes())
	assert.Contains(t, rec.Body.String(), "removing this role binding")
	requireBindingExists(t, s, b.ID, true)
}

// DELETE /admin/role-bindings/{id}, system super-admin binding. The
// last-super-admin guard passes; the remaining super-admin is restricted
// by a scheduled constraint, so the lockout check refuses.
func TestConstraintAdminLockout_RoleBindingDeleteSuperAdmin(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	lockoutSuspendDevUser(t, srv, s)
	a := lockoutSystemBinding(t, s, store.SystemRoleSuperAdmin, "lockout-rb-sa-a")
	b := lockoutSystemBinding(t, s, store.SystemRoleSuperAdmin, "lockout-rb-sa-b")
	future := time.Now().Add(24 * time.Hour)
	_, err := s.CreateAccessConstraint(ctx, &store.AccessConstraint{
		Name:                 "lockout-rb-sa-scheduled",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(b.PrincipalID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read"},
		NotBefore:            &future,
		Purpose:              "scheduled restriction",
		CreatedBy:            "test",
	})
	require.NoError(t, err)

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/admin/role-bindings/"+a.ID, nil)
	requireLockoutRefusal(t, rec.Code, rec.Body.Bytes())
	assert.Contains(t, rec.Body.String(), "when scheduled constraints activate")
	requireBindingExists(t, s, a.ID, true)

	// With an unrestricted super-admin remaining, the delete succeeds.
	lockoutSystemBinding(t, s, store.SystemRoleSuperAdmin, "lockout-rb-sa-c")
	rec = doRequest(t, srv, http.MethodDelete, "/api/v1/admin/role-bindings/"+a.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	requireBindingExists(t, s, a.ID, false)
}

// DELETE /admin/role-bindings/{id}, project scope (membership service).
func TestConstraintAdminLockout_RoleBindingDeleteProject(t *testing.T) {
	srv, s := testServer(t)
	_, _, bindings := lockoutProjectAdmins(t, s, "lockout-rb-proj", 2)

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/admin/role-bindings/"+bindings[0].ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	requireBindingExists(t, s, bindings[0].ID, false)

	rec = doRequest(t, srv, http.MethodDelete, "/api/v1/admin/role-bindings/"+bindings[1].ID, nil)
	requireLockoutRefusal(t, rec.Code, rec.Body.Bytes())
	assert.Contains(t, rec.Body.String(), "removing this role binding")
	requireBindingExists(t, s, bindings[1].ID, true)
}

// DELETE /projects/{id}/members/{bindingID}.
func TestConstraintAdminLockout_ProjectMemberDelete(t *testing.T) {
	srv, s := testServer(t)
	projectID, _, bindings := lockoutProjectAdmins(t, s, "lockout-member-del", 2)
	path := "/api/v1/projects/" + projectID + "/members/"

	rec := doRequest(t, srv, http.MethodDelete, path+bindings[0].ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	requireBindingExists(t, s, bindings[0].ID, false)

	rec = doRequest(t, srv, http.MethodDelete, path+bindings[1].ID, nil)
	requireLockoutRefusal(t, rec.Code, rec.Body.Bytes())
	requireBindingExists(t, s, bindings[1].ID, true)
}

// DELETE /projects/{id}/members/principals/user/{userID}.
func TestConstraintAdminLockout_ProjectMemberPrincipalDelete(t *testing.T) {
	srv, s := testServer(t)
	projectID, users, bindings := lockoutProjectAdmins(t, s, "lockout-principal-del", 2)
	path := "/api/v1/projects/" + projectID + "/members/principals/user/"

	rec := doRequest(t, srv, http.MethodDelete, path+users[0], nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	requireBindingExists(t, s, bindings[0].ID, false)

	rec = doRequest(t, srv, http.MethodDelete, path+users[1], nil)
	requireLockoutRefusal(t, rec.Code, rec.Body.Bytes())
	assert.Contains(t, rec.Body.String(), "removing this role binding")
	requireBindingExists(t, s, bindings[1].ID, true)
}

// PUT /projects/{id}/members/principals/user/{userID} with a role set that
// no longer carries access_constraint.admin.
func TestConstraintAdminLockout_ProjectMemberPrincipalPut(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	projectID, users, bindings := lockoutProjectAdmins(t, s, "lockout-principal-put", 2)
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	path := "/api/v1/projects/" + projectID + "/members/principals/user/"
	body := setMemberRolesRequestBody{RoleDefinitionIDs: []string{memberRD.ID}}

	rec := doRequest(t, srv, http.MethodPut, path+users[0], body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	requireBindingExists(t, s, bindings[0].ID, false)

	rec = doRequest(t, srv, http.MethodPut, path+users[1], body)
	requireLockoutRefusal(t, rec.Code, rec.Body.Bytes())
	requireBindingExists(t, s, bindings[1].ID, true)
}

// lockoutProjectBinding creates a project-scoped binding and returns it.
func lockoutProjectBinding(t *testing.T, s store.Store, roleDefID, principalType, principalID, projectID string) *store.RoleBinding {
	t.Helper()
	b, err := s.CreateRoleBinding(context.Background(), &store.RoleBinding{
		RoleDefinitionID: roleDefID,
		PrincipalType:    principalType,
		PrincipalID:      principalID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
	return b
}

// Removing a group binding that carries access_constraint.admin counts the
// group's members as losing that authority.
func TestConstraintAdminLockout_GroupBindingDelete(t *testing.T) {
	_, _, _, s := govTestSetup(t)
	ctx := context.Background()
	now := time.Now()
	projectID := pvSeedProject(t, s, "lockout-grp-rb-project")
	rd := createTestRoleDefinition(t, s, "lockout-grp-rb-admin-role", store.RoleScopeProject,
		[]string{PermissionConstraintAdmin})
	groupID := pvSeedGroup(t, s, "lockout-grp-rb")
	groupBinding := lockoutProjectBinding(t, s, rd.ID, "group", groupID, projectID)
	member := pvSeedUser(t, s, "lockout-grp-rb-member")
	pvSeedGroupMember(t, s, groupID, store.GroupMemberTypeUser, member)

	err := checkRoleBindingRemovalAdminLockout(ctx, s, now, groupBinding)
	var ge *GovernanceError
	require.ErrorAs(t, err, &ge, "the only admin group binding cannot be removed")
	assert.Equal(t, ErrCodeConstraintAdminLockout, ge.Code)

	other := pvSeedUser(t, s, "lockout-grp-rb-other")
	pvSeedRoleBinding(t, s, rd.ID, "user", other, store.RoleScopeProject, projectID)
	assert.NoError(t, checkRoleBindingRemovalAdminLockout(ctx, s, now, groupBinding))
}

// Only admin bindings at the removed binding's scope count as keeping the
// user an admin there.
func TestConstraintAdminLockout_OtherScopeAdminNotCounted(t *testing.T) {
	_, _, _, s := govTestSetup(t)
	ctx := context.Background()
	now := time.Now()
	projectID := pvSeedProject(t, s, "lockout-scope-p1")
	otherProjectID := pvSeedProject(t, s, "lockout-scope-p2")
	rd := createTestRoleDefinition(t, s, "lockout-scope-admin-role", store.RoleScopeProject,
		[]string{PermissionConstraintAdmin})
	user := pvSeedUser(t, s, "lockout-scope-user")
	binding := lockoutProjectBinding(t, s, rd.ID, "user", user, projectID)
	pvSeedRoleBinding(t, s, rd.ID, "user", user, store.RoleScopeProject, otherProjectID)

	err := checkRoleBindingRemovalAdminLockout(ctx, s, now, binding)
	var ge *GovernanceError
	require.ErrorAs(t, err, &ge, "an admin binding at another scope does not count")
	assert.Equal(t, ErrCodeConstraintAdminLockout, ge.Code)

	other := pvSeedUser(t, s, "lockout-scope-other")
	pvSeedRoleBinding(t, s, rd.ID, "user", other, store.RoleScopeProject, projectID)
	assert.NoError(t, checkRoleBindingRemovalAdminLockout(ctx, s, now, binding))
}

// Only admin bindings active at the check time count as keeping the user
// an admin at the removed binding's scope. A binding that is not yet
// valid or has expired keeps no admin.
func TestConstraintAdminLockout_InactiveSameScopeAdminNotCounted(t *testing.T) {
	now := time.Now()
	future := now.Add(24 * time.Hour)
	past := now.Add(-24 * time.Hour)
	tests := []struct {
		name      string
		notBefore *time.Time
		expiresAt *time.Time
		refused   bool
	}{
		{name: "scheduled", notBefore: &future, refused: true},
		{name: "expired", expiresAt: &past, refused: true},
		{name: "active", notBefore: &past, expiresAt: &future, refused: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, s := govTestSetup(t)
			ctx := context.Background()
			prefix := "lockout-inactive-" + tc.name
			projectID := pvSeedProject(t, s, prefix+"-project")
			rd := createTestRoleDefinition(t, s, prefix+"-admin-role",
				store.RoleScopeProject, []string{PermissionConstraintAdmin})
			otherRD := createTestRoleDefinition(t, s, prefix+"-other-role",
				store.RoleScopeProject, []string{PermissionConstraintAdmin})
			user := pvSeedUser(t, s, prefix+"-user")
			binding := lockoutProjectBinding(t, s, rd.ID, "user", user, projectID)
			_, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
				RoleDefinitionID: otherRD.ID,
				PrincipalType:    "user",
				PrincipalID:      user,
				ScopeType:        store.RoleScopeProject,
				ScopeID:          projectID,
				NotBefore:        tc.notBefore,
				ExpiresAt:        tc.expiresAt,
				CreatedBy:        "test",
			})
			require.NoError(t, err)

			err = checkRoleBindingRemovalAdminLockout(ctx, s, now, binding)
			if !tc.refused {
				assert.NoError(t, err)
				return
			}
			var ge *GovernanceError
			require.ErrorAs(t, err, &ge)
			assert.Equal(t, ErrCodeConstraintAdminLockout, ge.Code)
		})
	}
}

// Only a binding active at the check time makes another user a remaining
// admin, through a user binding or a group binding at the scope. The
// check time is whole seconds so stored times compare exactly; the
// window bounds follow isBindingActive (NotBefore is inclusive and
// ExpiresAt is exclusive, so a binding that expires at the check time
// is no longer active).
func TestConstraintAdminLockout_InactiveOtherAdminNotCounted(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	at := func(d time.Duration) *time.Time {
		v := now.Add(d)
		return &v
	}
	windows := []struct {
		name      string
		notBefore *time.Time
		expiresAt *time.Time
		refused   bool
	}{
		{name: "no-window"},
		{name: "scheduled", notBefore: at(time.Hour), refused: true},
		{name: "starts-in-1s", notBefore: at(time.Second), refused: true},
		{name: "starts-now", notBefore: at(0)},
		{name: "expires-in-1s", expiresAt: at(time.Second)},
		{name: "expires-now", expiresAt: at(0), refused: true},
		{name: "expired-1s-ago", expiresAt: at(-time.Second), refused: true},
		{name: "expired", expiresAt: at(-time.Hour), refused: true},
	}
	for _, route := range []string{"user", "group"} {
		for _, w := range windows {
			t.Run(route+"/"+w.name, func(t *testing.T) {
				_, _, _, s := govTestSetup(t)
				ctx := context.Background()
				prefix := "lockout-other-" + route + "-" + w.name
				projectID := pvSeedProject(t, s, prefix+"-project")
				rd := createTestRoleDefinition(t, s, prefix+"-admin-role",
					store.RoleScopeProject, []string{PermissionConstraintAdmin})
				removed := lockoutProjectBinding(t, s, rd.ID, "user",
					pvSeedUser(t, s, prefix+"-removed"), projectID)
				other := pvSeedUser(t, s, prefix+"-other")
				principalType, principalID := "user", other
				if route == "group" {
					principalType = "group"
					principalID = pvSeedGroup(t, s, prefix+"-group")
					pvSeedGroupMember(t, s, principalID, store.GroupMemberTypeUser, other)
				}
				_, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
					RoleDefinitionID: rd.ID,
					PrincipalType:    principalType,
					PrincipalID:      principalID,
					ScopeType:        store.RoleScopeProject,
					ScopeID:          projectID,
					NotBefore:        w.notBefore,
					ExpiresAt:        w.expiresAt,
					CreatedBy:        "test",
				})
				require.NoError(t, err)

				err = checkRoleBindingRemovalAdminLockout(ctx, s, now, removed)
				if !w.refused {
					assert.NoError(t, err)
					return
				}
				var ge *GovernanceError
				require.ErrorAs(t, err, &ge)
				assert.Equal(t, ErrCodeConstraintAdminLockout, ge.Code)
			})
		}
	}
}

// lockoutReadErrStore fails the named reads. roleDefNotFoundID makes
// GetRoleDefinition report ErrNotFound for that role only. When
// scopeErrFromCall is above zero, ListRoleBindingsForScope fails from
// that call on (counted in scopeCalls); otherwise scopeErr fails every
// call.
type lockoutReadErrStore struct {
	store.Store
	roleDefErr         error
	roleDefNotFoundID  string
	scopeErr           error
	scopeErrFromCall   int
	scopeCalls         *int
	groupMembersErr    error
	effectiveGroupsErr error
	userErr            error
}

func (s lockoutReadErrStore) GetRoleDefinition(ctx context.Context, id string) (*store.RoleDefinition, error) {
	if s.roleDefErr != nil {
		return nil, s.roleDefErr
	}
	if id != "" && id == s.roleDefNotFoundID {
		return nil, fmt.Errorf("role %s: %w", id, store.ErrNotFound)
	}
	return s.Store.GetRoleDefinition(ctx, id)
}

func (s lockoutReadErrStore) ListRoleBindingsForScope(ctx context.Context, scopeType, scopeID string) ([]*store.RoleBinding, error) {
	if s.scopeErr != nil {
		if s.scopeErrFromCall <= 0 {
			return nil, s.scopeErr
		}
		*s.scopeCalls++
		if *s.scopeCalls >= s.scopeErrFromCall {
			return nil, s.scopeErr
		}
	}
	return s.Store.ListRoleBindingsForScope(ctx, scopeType, scopeID)
}

func (s lockoutReadErrStore) GetGroupMembers(ctx context.Context, groupID string) ([]store.GroupMember, error) {
	if s.groupMembersErr != nil {
		return nil, s.groupMembersErr
	}
	return s.Store.GetGroupMembers(ctx, groupID)
}

func (s lockoutReadErrStore) GetEffectiveGroups(ctx context.Context, userID string) ([]string, error) {
	if s.effectiveGroupsErr != nil {
		return nil, s.effectiveGroupsErr
	}
	return s.Store.GetEffectiveGroups(ctx, userID)
}

func (s lockoutReadErrStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	if s.userErr != nil {
		return nil, s.userErr
	}
	return s.Store.GetUser(ctx, id)
}

// A failed role definition or scope binding read refuses the change.
func TestConstraintAdminLockout_ReadFailureRefuses(t *testing.T) {
	readErr := errors.New("read failed")
	for _, tc := range []struct {
		name string
		st   func(store.Store) store.Store
	}{
		{name: "role-definition", st: func(s store.Store) store.Store {
			return lockoutReadErrStore{Store: s, roleDefErr: readErr}
		}},
		{name: "scope-bindings", st: func(s store.Store) store.Store {
			return lockoutReadErrStore{Store: s, scopeErr: readErr}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, s := govTestSetup(t)
			ctx := context.Background()
			projectID, users, _ := lockoutProjectAdmins(t, s, "lockout-readerr-"+tc.name, 2)
			rm := adminRemoval{userID: users[0], userAction: userRemovalSuspend}
			require.NoError(t, checkConstraintAdminLockout(ctx, s, time.Now(),
				store.RoleScopeProject, projectID, rm), "control: reads succeed")

			err := checkConstraintAdminLockout(ctx, tc.st(s), time.Now(),
				store.RoleScopeProject, projectID, rm)
			var ge *GovernanceError
			require.ErrorAs(t, err, &ge)
			assert.Equal(t, ErrCodeConstraintAdminLockout, ge.Code)
		})
	}
}

// Removing a user whose constraint-admin bindings at the scope are all
// outside their activation window still has the scope judged over the
// active admins: with none left, or none once scheduled constraints
// activate, the removal is refused. A user with no constraint-admin
// binding at the scope runs no check.
func TestConstraintAdminLockout_UserRemovalInactiveAdmin(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	future := now.Add(time.Hour)
	past := now.Add(-time.Hour)
	tests := []struct {
		name      string
		route     string // how the removed user holds the admin binding
		notBefore *time.Time
		expiresAt *time.Time
		other     string // "", "active" or "blocked"
		refused   bool
	}{
		{name: "expired-only-admin", route: "user", expiresAt: &past, refused: true},
		{name: "scheduled-only-admin", route: "user", notBefore: &future, refused: true},
		{name: "group-expired-only-admin", route: "group", expiresAt: &past, refused: true},
		{name: "scheduled-other-blocked", route: "user", notBefore: &future, other: "blocked", refused: true},
		{name: "expired-other-active", route: "user", expiresAt: &past, other: "active"},
		{name: "no-admin-binding", route: "none"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, s := govTestSetup(t)
			ctx := context.Background()
			prefix := "lockout-urm-" + tc.name
			projectID := pvSeedProject(t, s, prefix+"-project")
			rd := createTestRoleDefinition(t, s, prefix+"-admin-role",
				store.RoleScopeProject, []string{PermissionConstraintAdmin})
			user := pvSeedUser(t, s, prefix+"-user")
			principalType, principalID := "user", user
			if tc.route == "group" {
				principalType = "group"
				principalID = pvSeedGroup(t, s, prefix+"-group")
				pvSeedGroupMember(t, s, principalID, store.GroupMemberTypeUser, user)
			}
			if tc.route != "none" {
				_, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
					RoleDefinitionID: rd.ID,
					PrincipalType:    principalType,
					PrincipalID:      principalID,
					ScopeType:        store.RoleScopeProject,
					ScopeID:          projectID,
					NotBefore:        tc.notBefore,
					ExpiresAt:        tc.expiresAt,
					CreatedBy:        "test",
				})
				require.NoError(t, err)
			}
			if tc.other != "" {
				other := pvSeedUser(t, s, prefix+"-other")
				pvSeedRoleBinding(t, s, rd.ID, "user", other, store.RoleScopeProject, projectID)
				if tc.other == "blocked" {
					_, err := s.CreateAccessConstraint(ctx, &store.AccessConstraint{
						Name:                 prefix + "-scheduled",
						SubjectKind:          store.ConstraintSubjectPrincipal,
						SubjectPrincipalType: pvStrPtr("user"),
						SubjectPrincipalID:   pvStrPtr(other),
						ScopeType:            store.RoleScopeProject,
						ScopeID:              projectID,
						MaximumPermissions:   []string{"agent.read"},
						NotBefore:            &future,
						Purpose:              "scheduled restriction",
						CreatedBy:            "test",
					})
					require.NoError(t, err)
				}
			}

			err := checkConstraintAdminLockout(ctx, s, now, store.RoleScopeProject, projectID,
				adminRemoval{userID: user, userAction: userRemovalSuspend})
			if !tc.refused {
				assert.NoError(t, err)
				return
			}
			var ge *GovernanceError
			require.ErrorAs(t, err, &ge)
			assert.Equal(t, ErrCodeConstraintAdminLockout, ge.Code)
		})
	}
}

// A role definition read error while listing the scopes a removed user
// administers refuses the removal.
func TestConstraintAdminLockout_UserRemovalRoleReadFailureRefuses(t *testing.T) {
	_, _, _, s := govTestSetup(t)
	ctx := context.Background()
	_, users, _ := lockoutProjectAdmins(t, s, "lockout-urm-readerr", 2)
	require.NoError(t, checkUserRemovalAdminLockout(ctx, s, time.Now(), users[0], userRemovalSuspend),
		"control: reads succeed")

	st := lockoutReadErrStore{Store: s, roleDefErr: errors.New("read failed")}
	assert.Error(t, checkUserRemovalAdminLockout(ctx, st, time.Now(), users[0], userRemovalSuspend))
}

// A failed read for a user who holds an admin binding at the scope only
// outside its activation window fails the admin resolution.
func TestConstraintAdminLockout_InactiveBindingReadFailureRefuses(t *testing.T) {
	readErr := errors.New("read failed")
	for _, tc := range []struct {
		name  string
		route string
		fault func(*lockoutReadErrStore)
	}{
		{name: "group-members", route: "group", fault: func(s *lockoutReadErrStore) { s.groupMembersErr = readErr }},
		{name: "user", route: "user", fault: func(s *lockoutReadErrStore) { s.userErr = readErr }},
		{name: "effective-groups", route: "user", fault: func(s *lockoutReadErrStore) { s.effectiveGroupsErr = readErr }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, s := govTestSetup(t)
			ctx := context.Background()
			now := time.Now()
			past := now.Add(-time.Hour)
			prefix := "lockout-inactive-readerr-" + tc.name
			projectID := pvSeedProject(t, s, prefix+"-project")
			rd := createTestRoleDefinition(t, s, prefix+"-admin-role",
				store.RoleScopeProject, []string{PermissionConstraintAdmin})
			user := pvSeedUser(t, s, prefix+"-user")
			principalType, principalID := "user", user
			if tc.route == "group" {
				principalType = "group"
				principalID = pvSeedGroup(t, s, prefix+"-group")
				pvSeedGroupMember(t, s, principalID, store.GroupMemberTypeUser, user)
			}
			_, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
				RoleDefinitionID: rd.ID,
				PrincipalType:    principalType,
				PrincipalID:      principalID,
				ScopeType:        store.RoleScopeProject,
				ScopeID:          projectID,
				ExpiresAt:        &past,
				CreatedBy:        "test",
			})
			require.NoError(t, err)

			admins, inactiveOnly, err := resolveConstraintAdmins(ctx, s, now, store.RoleScopeProject, projectID)
			require.NoError(t, err, "control: reads succeed")
			assert.Empty(t, admins)
			assert.True(t, inactiveOnly[user], "control: the user holds only an inactive binding")

			st := lockoutReadErrStore{Store: s}
			tc.fault(&st)
			_, _, err = resolveConstraintAdmins(ctx, st, now, store.RoleScopeProject, projectID)
			assert.ErrorIs(t, err, readErr)
		})
	}
}

// A binding whose role definition does not exist grants nothing: the
// resolution and the user removal skip it rather than refuse.
func TestConstraintAdminLockout_MissingRoleDefinitionSkipped(t *testing.T) {
	_, _, _, s := govTestSetup(t)
	ctx := context.Background()
	now := time.Now()
	projectID := pvSeedProject(t, s, "lockout-missing-role-project")
	rdA := createTestRoleDefinition(t, s, "lockout-missing-role-a",
		store.RoleScopeProject, []string{PermissionConstraintAdmin})
	rdB := createTestRoleDefinition(t, s, "lockout-missing-role-b",
		store.RoleScopeProject, []string{PermissionConstraintAdmin})
	userA := pvSeedUser(t, s, "lockout-missing-role-user-a")
	userB := pvSeedUser(t, s, "lockout-missing-role-user-b")
	lockoutProjectBinding(t, s, rdA.ID, "user", userA, projectID)
	lockoutProjectBinding(t, s, rdB.ID, "user", userB, projectID)
	st := lockoutReadErrStore{Store: s, roleDefNotFoundID: rdB.ID}

	admins, _, err := resolveConstraintAdmins(ctx, st, now, store.RoleScopeProject, projectID)
	require.NoError(t, err)
	require.Len(t, admins, 1)
	assert.Equal(t, userA, admins[0].userID)

	// userB administers no scope once its role is missing, so removing
	// userB runs no check; removing userA, the only remaining admin, is
	// refused.
	assert.NoError(t, checkUserRemovalAdminLockout(ctx, st, now, userB, userRemovalSuspend))
	var ge *GovernanceError
	require.ErrorAs(t, checkUserRemovalAdminLockout(ctx, st, now, userA, userRemovalSuspend), &ge)
	assert.Equal(t, ErrCodeConstraintAdminLockout, ge.Code)
}

// A failed read of the removed binding's scope bindings, after the admins
// resolve, counts every covered admin as losing authority.
func TestConstraintAdminLockout_SurvivorScopeReadFailureRefuses(t *testing.T) {
	_, _, _, s := govTestSetup(t)
	ctx := context.Background()
	now := time.Now()
	projectID := pvSeedProject(t, s, "lockout-survivor-readerr-project")
	rd := createTestRoleDefinition(t, s, "lockout-survivor-readerr-role",
		store.RoleScopeProject, []string{PermissionConstraintAdmin})
	otherRD := createTestRoleDefinition(t, s, "lockout-survivor-readerr-other",
		store.RoleScopeProject, []string{PermissionConstraintAdmin})
	user := pvSeedUser(t, s, "lockout-survivor-readerr-user")
	binding := lockoutProjectBinding(t, s, rd.ID, "user", user, projectID)
	lockoutProjectBinding(t, s, otherRD.ID, "user", user, projectID)
	require.NoError(t, checkRoleBindingRemovalAdminLockout(ctx, s, now, binding),
		"control: the other binding keeps the user an admin")

	calls := 0
	st := lockoutReadErrStore{Store: s, scopeErr: errors.New("read failed"),
		scopeErrFromCall: 2, scopeCalls: &calls}
	err := checkRoleBindingRemovalAdminLockout(ctx, st, now, binding)
	var ge *GovernanceError
	require.ErrorAs(t, err, &ge)
	assert.Equal(t, ErrCodeConstraintAdminLockout, ge.Code)
	assert.Equal(t, 2, calls, "the admin resolution read succeeded and the second read failed")
}

// The lockout preview counts only active bindings toward the remaining
// admins, and a role definition read error leaves it undetermined.
func TestConstraintAdminLockout_PreviewCountsActiveBindings(t *testing.T) {
	ps, authz, s := previewTestSetup(t)
	ctx := context.Background()
	now := ps.nowFunc()
	past := now.Add(-time.Hour)
	projectID := pvSeedProject(t, s, "lockout-preview-project")
	rd := createTestRoleDefinition(t, s, "lockout-preview-admin-role",
		store.RoleScopeProject, []string{PermissionConstraintAdmin})
	inactive := pvSeedUser(t, s, "lockout-preview-inactive")
	_, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    "user",
		PrincipalID:      inactive,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		ExpiresAt:        &past,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
	draft := &store.AccessConstraint{
		Name:                 "lockout-preview-draft",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(pvSeedUser(t, s, "lockout-preview-subject")),
		ScopeType:            store.RoleScopeProject,
		ScopeID:              projectID,
		MaximumPermissions:   []string{"agent.read"},
	}
	assess := func(p *PreviewService) LockoutAssessment {
		return p.assessLockout(ctx, PreviewRequest{Operation: "create", Draft: draft,
			Actor: pvTestActor(inactive)}, nil, storeToHubAccessConstraint(draft), now)
	}

	l := assess(ps)
	require.NotNil(t, l.Safe)
	assert.False(t, *l.Safe, "an admin with only an expired binding does not count")
	require.NotNil(t, l.RemainingActiveDirectAdmins)
	assert.Equal(t, 0, *l.RemainingActiveDirectAdmins)

	pvSeedRoleBinding(t, s, rd.ID, "user", pvSeedUser(t, s, "lockout-preview-active"),
		store.RoleScopeProject, projectID)
	l = assess(ps)
	require.NotNil(t, l.Safe)
	assert.True(t, *l.Safe, "control: an active admin counts")
	require.NotNil(t, l.RemainingActiveDirectAdmins)
	assert.Equal(t, 1, *l.RemainingActiveDirectAdmins)

	eps := NewPreviewServiceWithKey(lockoutReadErrStore{Store: s, roleDefErr: errors.New("read failed")},
		authz, slog.Default(), []byte("test-preview-hmac-key-32-bytes!!"))
	l = assess(eps)
	assert.Nil(t, l.Safe, "a role definition read error leaves the assessment undetermined")
	assert.NotEmpty(t, l.UndeterminedReason)
}

// DELETE /projects/{id}/members/principals/group/{groupID}.
func TestConstraintAdminLockout_ProjectMemberGroupPrincipalDelete(t *testing.T) {
	srv, s := testServer(t)
	projectID := pvSeedProject(t, s, "lockout-grp-principal-project")
	rd := createTestRoleDefinition(t, s, "lockout-grp-principal-admin-role", store.RoleScopeProject,
		[]string{PermissionConstraintAdmin, "agent.read"})
	groupID := pvSeedGroup(t, s, "lockout-grp-principal")
	pvSeedGroupMember(t, s, groupID, store.GroupMemberTypeUser, pvSeedUser(t, s, "lockout-grp-principal-member"))
	groupBinding := lockoutProjectBinding(t, s, rd.ID, "group", groupID, projectID)
	path := "/api/v1/projects/" + projectID + "/members/principals/group/" + groupID

	rec := doRequest(t, srv, http.MethodDelete, path, nil)
	requireLockoutRefusal(t, rec.Code, rec.Body.Bytes())
	requireBindingExists(t, s, groupBinding.ID, true)

	lockoutProjectBinding(t, s, rd.ID, "user", pvSeedUser(t, s, "lockout-grp-principal-other"), projectID)
	rec = doRequest(t, srv, http.MethodDelete, path, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	requireBindingExists(t, s, groupBinding.ID, false)
}

// PATCH /projects/{id}/members/{bindingID} from a custom role carrying
// access_constraint.admin to project-member.
func TestConstraintAdminLockout_ProjectMemberRoleChange(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	projectID, _, bindings := lockoutProjectAdmins(t, s, "lockout-role-change", 2)
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	path := "/api/v1/projects/" + projectID + "/members/"
	body := updateProjectMemberRequest{RoleDefinitionID: memberRD.ID}

	rec := doRequest(t, srv, http.MethodPatch, path+bindings[0].ID, body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	requireBindingExists(t, s, bindings[0].ID, false)

	rec = doRequest(t, srv, http.MethodPatch, path+bindings[1].ID, body)
	requireLockoutRefusal(t, rec.Code, rec.Body.Bytes())
	requireBindingExists(t, s, bindings[1].ID, true)
}

// A user who stays a constraint admin through a group binding at the
// project can drop their own constraint-admin role.
func TestConstraintAdminLockout_ProjectMemberPrincipalPutGroupKeepsAdmin(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	projectID := pvSeedProject(t, s, "lockout-put-grp-project")
	rd := createTestRoleDefinition(t, s, "lockout-put-grp-admin-role", store.RoleScopeProject,
		[]string{PermissionConstraintAdmin, "agent.read"})
	user := pvSeedUser(t, s, "lockout-put-grp-user")
	userBinding := lockoutProjectBinding(t, s, rd.ID, "user", user, projectID)
	groupID := pvSeedGroup(t, s, "lockout-put-grp")
	pvSeedGroupMember(t, s, groupID, store.GroupMemberTypeUser, user)
	lockoutProjectBinding(t, s, rd.ID, "group", groupID, projectID)
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	rec := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+projectID+"/members/principals/user/"+user,
		setMemberRolesRequestBody{RoleDefinitionIDs: []string{memberRD.ID}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	requireBindingExists(t, s, userBinding.ID, false)
}

// requireLockoutErr asserts err is a constraint-admin lockout refusal.
func requireLockoutErr(t *testing.T, err error, msgAndArgs ...interface{}) {
	t.Helper()
	var ge *GovernanceError
	require.ErrorAs(t, err, &ge, msgAndArgs...)
	assert.Equal(t, ErrCodeConstraintAdminLockout, ge.Code)
}

// ---------------------------------------------------------------------------
// Replace path: one transaction, admin-guard lock
// ---------------------------------------------------------------------------

// replaceTxStore counts admin-guard locks taken on a transaction handle,
// counts the lockout reads made on the transaction handle (txReads) and
// on the outer handle (outerReads), and fails DeleteRoleBinding with
// deleteErr, inside or outside a transaction.
type replaceTxStore struct {
	store.Store
	inTx       bool
	txLocks    *int
	txReads    *int
	outerReads *int
	deleteErr  error
}

func (s replaceTxStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	return s.Store.WithTx(ctx, func(tx store.Store) error {
		inner := s
		inner.Store, inner.inTx = tx, true
		return fn(inner)
	})
}

// read counts one lockout read on the handle it is made through.
func (s replaceTxStore) read() {
	if s.inTx && s.txReads != nil {
		*s.txReads++
	}
	if !s.inTx && s.outerReads != nil {
		*s.outerReads++
	}
}

func (s replaceTxStore) GetRoleBinding(ctx context.Context, id string) (*store.RoleBinding, error) {
	s.read()
	return s.Store.GetRoleBinding(ctx, id)
}

func (s replaceTxStore) GetRoleDefinition(ctx context.Context, id string) (*store.RoleDefinition, error) {
	s.read()
	return s.Store.GetRoleDefinition(ctx, id)
}

func (s replaceTxStore) ListRoleBindingsForScope(ctx context.Context, scopeType, scopeID string) ([]*store.RoleBinding, error) {
	s.read()
	return s.Store.ListRoleBindingsForScope(ctx, scopeType, scopeID)
}

func (s replaceTxStore) GetGroupMembers(ctx context.Context, groupID string) ([]store.GroupMember, error) {
	s.read()
	return s.Store.GetGroupMembers(ctx, groupID)
}

func (s replaceTxStore) GetEffectiveGroups(ctx context.Context, userID string) ([]string, error) {
	s.read()
	return s.Store.GetEffectiveGroups(ctx, userID)
}

func (s replaceTxStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	s.read()
	return s.Store.GetUser(ctx, id)
}

func (s replaceTxStore) ListConstraintsForScope(ctx context.Context, scopeType, scopeID string) ([]*store.AccessConstraint, error) {
	s.read()
	return s.Store.ListConstraintsForScope(ctx, scopeType, scopeID)
}

func (s replaceTxStore) GetRoleDefinitionByName(ctx context.Context, name, scopeType string) (*store.RoleDefinition, error) {
	s.read()
	return s.Store.GetRoleDefinitionByName(ctx, name, scopeType)
}

func (s replaceTxStore) LockRoleDefinitionForAdminGuard(ctx context.Context, id string) error {
	if s.inTx {
		*s.txLocks++
	}
	return s.Store.LockRoleDefinitionForAdminGuard(ctx, id)
}

func (s replaceTxStore) DeleteRoleBinding(ctx context.Context, id string) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	return s.Store.DeleteRoleBinding(ctx, id)
}

// replaceGovernance returns a GovernanceService over st.
func replaceGovernance(t *testing.T, st store.Store, authz *AuthzService) *GovernanceService {
	t.Helper()
	ps := NewPreviewServiceWithKey(st, authz, slog.Default(), []byte("test-governance-hmac-key-32byte!"))
	return NewGovernanceService(st, ps, authz, slog.Default())
}

// replaceDowngrade returns a non-admin binding for b's principal at b's
// scope.
func replaceDowngrade(t *testing.T, s store.Store, name string, b *store.RoleBinding) *store.RoleBinding {
	t.Helper()
	basic := createTestRoleDefinition(t, s, name+"-basic", store.RoleScopeProject, []string{"agent.read"})
	return &store.RoleBinding{
		RoleDefinitionID: basic.ID,
		PrincipalType:    b.PrincipalType,
		PrincipalID:      b.PrincipalID,
		ScopeType:        b.ScopeType,
		ScopeID:          b.ScopeID,
		CreatedBy:        "test",
	}
}

// A downgrade from an admin role takes the admin-guard lock on the
// transaction handle the replacement writes through, and the lockout
// check reads through that handle only.
func TestConstraintAdminLockout_ReplaceTakesGuardLockInTx(t *testing.T) {
	_, _, authz, s := govTestSetup(t)
	ctx := context.Background()
	projectID, _, bindings := lockoutProjectAdmins(t, s, "replace-lock", 2)
	group := pvSeedGroup(t, s, "replace-lock-group")
	pvSeedGroupMember(t, s, group, store.GroupMemberTypeUser,
		pvSeedUser(t, s, "replace-lock-group-member"))
	lockoutProjectBinding(t, s, bindings[0].RoleDefinitionID, "group", group, projectID)
	newBinding := replaceDowngrade(t, s, "replace-lock", bindings[0])
	locks, txReads, outerReads := 0, 0, 0
	gs := replaceGovernance(t, replaceTxStore{Store: s, txLocks: &locks,
		txReads: &txReads, outerReads: &outerReads}, authz)

	created, err := gs.ReplaceRoleBinding(ctx, bindings[0].ID, newBinding, "")
	require.NoError(t, err)
	assert.Positive(t, locks, "the lockout check runs under the admin-guard lock in the transaction")
	assert.Positive(t, txReads, "the lockout reads go through the transaction handle")
	assert.Zero(t, outerReads, "no lockout read goes through the outer handle")
	requireBindingExists(t, s, bindings[0].ID, false)
	requireBindingExists(t, s, created.ID, true)
}

// When the old binding cannot be deleted, the replacement binding is not
// kept: the create and the delete commit together or not at all.
func TestConstraintAdminLockout_ReplaceDeleteFailureRollsBack(t *testing.T) {
	_, _, authz, s := govTestSetup(t)
	ctx := context.Background()
	projectID, _, bindings := lockoutProjectAdmins(t, s, "replace-rollback", 2)
	locks := 0
	gs := replaceGovernance(t, replaceTxStore{Store: s, txLocks: &locks,
		deleteErr: errors.New("delete failed")}, authz)
	newBinding := replaceDowngrade(t, s, "replace-rollback", bindings[0])

	_, err := gs.ReplaceRoleBinding(ctx, bindings[0].ID, newBinding, "")
	require.ErrorContains(t, err, "failed to delete old binding")
	requireBindingExists(t, s, bindings[0].ID, true)
	scope, err := s.ListRoleBindingsForScope(ctx, store.RoleScopeProject, projectID)
	require.NoError(t, err)
	for _, b := range scope {
		assert.NotEqual(t, newBinding.RoleDefinitionID, b.RoleDefinitionID,
			"the replacement binding is not kept")
	}
}

// requireReplaceOutcome checks a ReplaceRoleBinding result. An allowed
// replace deletes old and keeps the created binding. A refused replace
// fails with ErrCodeConstraintAdminLockout, keeps old, and writes no
// binding for newBinding's role and principal at newBinding's scope.
func requireReplaceOutcome(t *testing.T, s store.Store, err error, created, old, newBinding *store.RoleBinding, allowed bool) {
	t.Helper()
	if allowed {
		require.NoError(t, err)
		requireBindingExists(t, s, old.ID, false)
		requireBindingExists(t, s, created.ID, true)
		return
	}
	var ge *GovernanceError
	require.ErrorAs(t, err, &ge)
	assert.Equal(t, ErrCodeConstraintAdminLockout, ge.Code)
	requireBindingExists(t, s, old.ID, true)
	scope, lerr := s.ListRoleBindingsForScope(context.Background(), newBinding.ScopeType, newBinding.ScopeID)
	require.NoError(t, lerr)
	for _, b := range scope {
		if b.ID == old.ID {
			continue
		}
		assert.False(t, b.RoleDefinitionID == newBinding.RoleDefinitionID &&
			b.PrincipalType == newBinding.PrincipalType &&
			b.PrincipalID == newBinding.PrincipalID,
			"the replacement binding is not kept")
	}
}

// replaceMoved returns a binding with b's role at the given principal and
// project scope.
func replaceMoved(b *store.RoleBinding, principalType, principalID, projectID string) *store.RoleBinding {
	return &store.RoleBinding{
		RoleDefinitionID: b.RoleDefinitionID,
		PrincipalType:    principalType,
		PrincipalID:      principalID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	}
}

// A replace that hands the admin role to another principal at the same
// scope is judged like a binding delete at that scope: with no other
// active admin it is refused, and the new binding does not count as a
// remaining admin. Control: another active admin at the scope.
func TestConstraintAdminLockout_ReplaceMovesAdminToOtherPrincipal(t *testing.T) {
	for _, others := range []int{0, 1} {
		t.Run(fmt.Sprintf("other-admins-%d", others), func(t *testing.T) {
			gs, _, _, s := govTestSetup(t)
			ctx := context.Background()
			name := fmt.Sprintf("replace-move-principal-%d", others)
			projectID, _, bindings := lockoutProjectAdmins(t, s, name, 1+others)
			target := pvSeedUser(t, s, name+"-target")
			newBinding := replaceMoved(bindings[0], "user", target, projectID)

			created, err := gs.ReplaceRoleBinding(ctx, bindings[0].ID, newBinding, "")
			requireReplaceOutcome(t, s, err, created, bindings[0], newBinding, others > 0)
		})
	}
}

// A replace that moves the admin role to another scope is judged at the
// original scope: with no other active admin there it is refused.
// Control: another active admin at the original scope.
func TestConstraintAdminLockout_ReplaceMovesAdminToOtherScope(t *testing.T) {
	for _, others := range []int{0, 1} {
		t.Run(fmt.Sprintf("other-admins-%d", others), func(t *testing.T) {
			gs, _, _, s := govTestSetup(t)
			ctx := context.Background()
			name := fmt.Sprintf("replace-move-scope-%d", others)
			_, users, bindings := lockoutProjectAdmins(t, s, name, 1+others)
			otherProjectID := pvSeedProject(t, s, name+"-other")
			newBinding := replaceMoved(bindings[0], "user", users[0], otherProjectID)

			created, err := gs.ReplaceRoleBinding(ctx, bindings[0].ID, newBinding, "")
			requireReplaceOutcome(t, s, err, created, bindings[0], newBinding, others > 0)
		})
	}
}

// A replace that keeps an admin role for the same principal at the same
// scope, over a window that covers the old one, removes no admin, so the
// sole admin may make it.
func TestConstraintAdminLockout_ReplaceSamePrincipalAdminRoleAllowed(t *testing.T) {
	gs, _, _, s := govTestSetup(t)
	ctx := context.Background()
	projectID, users, bindings := lockoutProjectAdmins(t, s, "replace-same-admin", 1)
	otherAdmin := createTestRoleDefinition(t, s, "replace-same-admin-other-role",
		store.RoleScopeProject, []string{PermissionConstraintAdmin, "agent.list"})
	newBinding := replaceMoved(bindings[0], "user", users[0], projectID)
	newBinding.RoleDefinitionID = otherAdmin.ID

	created, err := gs.ReplaceRoleBinding(ctx, bindings[0].ID, newBinding, "")
	requireReplaceOutcome(t, s, err, created, bindings[0], newBinding, true)
}

// replaceReadErrStore is a lockoutReadErrStore whose WithTx hands the
// callback a transaction handle with the same read errors.
type replaceReadErrStore struct {
	lockoutReadErrStore
}

func (s replaceReadErrStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	return s.Store.WithTx(ctx, func(tx store.Store) error {
		inner := s
		inner.Store = tx
		return fn(inner)
	})
}

// A replace that moves an admin binding refuses when the scope bindings
// cannot be read in the transaction. Control: the same replace with the
// reads succeeding is allowed, as another active admin remains.
func TestConstraintAdminLockout_ReplaceMoveReadFailureRefuses(t *testing.T) {
	for _, move := range []string{"principal", "scope"} {
		for _, readErr := range []error{nil, errors.New("read failed")} {
			t.Run(fmt.Sprintf("%s-err-%t", move, readErr != nil), func(t *testing.T) {
				_, _, authz, s := govTestSetup(t)
				ctx := context.Background()
				name := fmt.Sprintf("replace-move-readerr-%s-%t", move, readErr != nil)
				projectID, users, bindings := lockoutProjectAdmins(t, s, name, 2)
				newBinding := replaceMoved(bindings[0], "user", pvSeedUser(t, s, name+"-target"), projectID)
				if move == "scope" {
					newBinding = replaceMoved(bindings[0], "user", users[0], pvSeedProject(t, s, name+"-other"))
				}
				gs := replaceGovernance(t, replaceReadErrStore{lockoutReadErrStore{Store: s, scopeErr: readErr}}, authz)

				created, err := gs.ReplaceRoleBinding(ctx, bindings[0].ID, newBinding, "")
				requireReplaceOutcome(t, s, err, created, bindings[0], newBinding, readErr == nil)
			})
		}
	}
}

// A replace that keeps an admin role for the same principal at the same
// scope but narrows the validity window removes admin authority for the
// time the new window leaves out, so the sole admin's replace is refused.
// Control: another active admin at the scope. A widened or equal window
// removes nothing and is allowed for the sole admin.
func TestConstraintAdminLockout_ReplaceNarrowedWindow(t *testing.T) {
	const later, sooner, earlier = 24 * time.Hour, time.Hour, -time.Hour
	// Window bounds are offsets from the pinned gs.nowFunc(); zero leaves
	// the bound unset.
	for _, tc := range []struct {
		name          string
		oldNB, oldExp time.Duration
		newNB, newExp time.Duration
		others        int
		allowed       bool
	}{
		{name: "expiry-added", newExp: sooner},
		{name: "expiry-added-other-admin", newExp: sooner, others: 1, allowed: true},
		{name: "expiry-earlier", oldExp: later, newExp: sooner},
		{name: "not-before-later", newNB: sooner},
		{name: "not-before-later-other-admin", newNB: sooner, others: 1, allowed: true},
		{name: "expiry-removed", oldExp: later, allowed: true},
		{name: "expiry-later", oldExp: sooner, newExp: later, allowed: true},
		{name: "window-equal", oldNB: earlier, oldExp: later, newNB: earlier, newExp: later, allowed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gs, _, _, s := govTestSetup(t)
			ctx := context.Background()
			name := "replace-window-" + tc.name
			now := gs.nowFunc()
			at := func(d time.Duration) *time.Time {
				if d == 0 {
					return nil
				}
				v := now.Add(d)
				return &v
			}
			projectID, _, _ := lockoutProjectAdmins(t, s, name, tc.others)
			user := pvSeedUser(t, s, name+"-user")
			old, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
				RoleDefinitionID: createTestRoleDefinition(t, s, name+"-role",
					store.RoleScopeProject, []string{PermissionConstraintAdmin}).ID,
				PrincipalType: "user",
				PrincipalID:   user,
				ScopeType:     store.RoleScopeProject,
				ScopeID:       projectID,
				NotBefore:     at(tc.oldNB),
				ExpiresAt:     at(tc.oldExp),
				CreatedBy:     "test",
			})
			require.NoError(t, err)
			// A second admin role: the store keeps one binding per role,
			// principal and scope, and the replace creates before it
			// deletes.
			newBinding := replaceMoved(old, "user", user, projectID)
			newBinding.RoleDefinitionID = createTestRoleDefinition(t, s, name+"-new-role",
				store.RoleScopeProject, []string{PermissionConstraintAdmin}).ID
			newBinding.NotBefore, newBinding.ExpiresAt = at(tc.newNB), at(tc.newExp)

			created, err := gs.ReplaceRoleBinding(ctx, old.ID, newBinding, "")
			requireReplaceOutcome(t, s, err, created, old, newBinding, tc.allowed)
		})
	}
}

// ---------------------------------------------------------------------------
// Nested groups: effective (transitive) membership
// ---------------------------------------------------------------------------

// nestedAdminFixture is a project with an admin role bound to group outer,
// group inner nested in outer when nested is true, and user member in
// inner.
type nestedAdminFixture struct {
	projectID, roleID, outer, inner, member string
}

func newNestedAdminFixture(t *testing.T, s store.Store, name string, nested bool) nestedAdminFixture {
	t.Helper()
	f := nestedAdminFixture{projectID: pvSeedProject(t, s, name+"-project")}
	f.roleID = createTestRoleDefinition(t, s, name+"-admin-role", store.RoleScopeProject,
		[]string{PermissionConstraintAdmin}).ID
	f.outer = pvSeedGroup(t, s, name+"-outer")
	f.inner = pvSeedGroup(t, s, name+"-inner")
	if nested {
		pvSeedGroupMember(t, s, f.outer, store.GroupMemberTypeGroup, f.inner)
	}
	lockoutProjectBinding(t, s, f.roleID, "group", f.outer, f.projectID)
	f.member = pvSeedUser(t, s, name+"-member")
	pvSeedGroupMember(t, s, f.inner, store.GroupMemberTypeUser, f.member)
	return f
}

// A user in a group nested in an admin-bound group is a remaining admin
// when another admin's binding is removed. Control: with the group not
// nested, the user is no admin and the removal is refused.
func TestConstraintAdminLockout_NestedGroupSurvivorCounted(t *testing.T) {
	for _, nested := range []bool{false, true} {
		name := "nested-survivor-flat"
		if nested {
			name = "nested-survivor"
		}
		t.Run(name, func(t *testing.T) {
			_, _, _, s := govTestSetup(t)
			f := newNestedAdminFixture(t, s, name, nested)
			other := pvSeedUser(t, s, name+"-other")
			b := lockoutProjectBinding(t, s, f.roleID, "user", other, f.projectID)

			err := checkRoleBindingRemovalAdminLockout(context.Background(), s, time.Now(), b)
			if nested {
				assert.NoError(t, err, "the nested member keeps an admin")
				return
			}
			requireLockoutErr(t, err, "no admin remains without the nesting")
		})
	}
}

// Suspending a user who is the only admin, through a nested group, is
// judged and refused.
func TestConstraintAdminLockout_NestedGroupAdminRemovalJudged(t *testing.T) {
	_, _, _, s := govTestSetup(t)
	f := newNestedAdminFixture(t, s, "nested-removal", true)
	err := checkConstraintAdminLockout(context.Background(), s, time.Now(),
		store.RoleScopeProject, f.projectID,
		adminRemoval{userID: f.member, userAction: userRemovalSuspend})
	requireLockoutErr(t, err)
}

// nestedGroupErrStore fails GetGroupMembers for one group.
type nestedGroupErrStore struct {
	store.Store
	groupID string
}

func (s nestedGroupErrStore) GetGroupMembers(ctx context.Context, groupID string) ([]store.GroupMember, error) {
	if groupID == s.groupID {
		return nil, errors.New("read failed")
	}
	return s.Store.GetGroupMembers(ctx, groupID)
}

// A failed member read of a nested group fails the admin resolution and
// refuses the change: a binding removal, and the suspension of the user
// who is an admin through that group.
func TestConstraintAdminLockout_NestedGroupReadFailureRefuses(t *testing.T) {
	_, _, _, s := govTestSetup(t)
	ctx := context.Background()
	f := newNestedAdminFixture(t, s, "nested-readerr", true)
	other := pvSeedUser(t, s, "nested-readerr-other")
	b := lockoutProjectBinding(t, s, f.roleID, "user", other, f.projectID)
	suspend := adminRemoval{userID: f.member, userAction: userRemovalSuspend}
	require.NoError(t, checkRoleBindingRemovalAdminLockout(ctx, s, time.Now(), b),
		"control: reads succeed")
	require.NoError(t, checkConstraintAdminLockout(ctx, s, time.Now(),
		store.RoleScopeProject, f.projectID, suspend),
		"control: the other admin remains")

	st := nestedGroupErrStore{Store: s, groupID: f.inner}
	err := checkRoleBindingRemovalAdminLockout(ctx, st, time.Now(), b)
	requireLockoutErr(t, err)
	assert.ErrorContains(t, err, "failed to resolve admin users")

	err = checkConstraintAdminLockout(ctx, st, time.Now(),
		store.RoleScopeProject, f.projectID, suspend)
	requireLockoutErr(t, err)
	assert.ErrorContains(t, err, "failed to resolve admin users")
}

// Two groups nested in each other end the resolution, and a user in
// them counts once.
func TestConstraintAdminLockout_NestedGroupCycleTerminates(t *testing.T) {
	_, _, _, s := govTestSetup(t)
	projectID := pvSeedProject(t, s, "nested-cycle-project")
	rd := createTestRoleDefinition(t, s, "nested-cycle-admin-role", store.RoleScopeProject,
		[]string{PermissionConstraintAdmin})
	a := pvSeedGroup(t, s, "nested-cycle-a")
	b := pvSeedGroup(t, s, "nested-cycle-b")
	pvSeedGroupMember(t, s, b, store.GroupMemberTypeGroup, a)
	pvSeedGroupMember(t, s, a, store.GroupMemberTypeGroup, b)
	user := pvSeedUser(t, s, "nested-cycle-user")
	pvSeedGroupMember(t, s, b, store.GroupMemberTypeUser, user)
	lockoutProjectBinding(t, s, rd.ID, "group", a, projectID)

	type result struct {
		admins []adminUserInfo
		err    error
	}
	done := make(chan result, 1)
	go func() {
		admins, err := resolveConstraintAdminUsers(context.Background(), s, time.Now(),
			store.RoleScopeProject, projectID)
		done <- result{admins, err}
	}()
	select {
	case r := <-done:
		require.NoError(t, r.err)
		require.Len(t, r.admins, 1)
		assert.Equal(t, user, r.admins[0].userID)
	case <-time.After(30 * time.Second):
		t.Fatal("resolution did not finish on nested groups that form a cycle")
	}
}

// The admin resolution and isConstraintAdmin agree on who holds
// access_constraint.admin through two levels of nesting, including test
// identities: at system scope the clamp removes a test fixture's grant
// and the resolution does not count it; at project scope both count it.
func TestConstraintAdminLockout_NestedGroupMatchesIsConstraintAdmin(t *testing.T) {
	gs, _, _, s := govTestSetup(t)
	ctx := context.Background()
	role := tiCustomSystemRole(t, s, "nested-agree-role", PermissionConstraintAdmin)
	outer := tiGroupWithSystemRole(t, s, "nested-agree-outer", role, DevUserID)
	mid := pvSeedGroup(t, s, "nested-agree-mid")
	inner := pvSeedGroup(t, s, "nested-agree-inner")
	pvSeedGroupMember(t, s, outer.ID, store.GroupMemberTypeGroup, mid)
	pvSeedGroupMember(t, s, mid, store.GroupMemberTypeGroup, inner)
	deep := pvSeedUser(t, s, "nested-agree-deep")
	pvSeedGroupMember(t, s, inner, store.GroupMemberTypeUser, deep)
	middle := pvSeedUser(t, s, "nested-agree-middle")
	pvSeedGroupMember(t, s, mid, store.GroupMemberTypeUser, middle)
	fixture := tiStoreFixture(t, s, generateID(), time.Now().Add(time.Hour))
	pvSeedGroupMember(t, s, inner, store.GroupMemberTypeUser, fixture.ID)
	outsider := pvSeedUser(t, s, "nested-agree-outsider")

	admins, err := resolveConstraintAdminUsers(ctx, s, time.Now(), store.RoleScopeSystem, "")
	require.NoError(t, err)
	resolved := map[string]bool{}
	for _, a := range admins {
		resolved[a.userID] = true
	}
	var fromResolver, fromAllow []string
	for _, id := range []string{deep, middle, fixture.ID, outsider} {
		if resolved[id] {
			fromResolver = append(fromResolver, id)
		}
		ok, err := gs.isConstraintAdmin(ctx, id)
		require.NoError(t, err)
		if ok {
			fromAllow = append(fromAllow, id)
		}
	}
	want := []string{deep, middle}
	sort.Strings(want)
	sort.Strings(fromResolver)
	sort.Strings(fromAllow)
	assert.Equal(t, want, fromResolver)
	assert.Equal(t, want, fromAllow)

	// Project scope: a test fixture in a nested group counts on both.
	projectID := pvSeedProject(t, s, "nested-agree-project")
	projectRole := createTestRoleDefinition(t, s, "nested-agree-project-role",
		store.RoleScopeProject, []string{PermissionConstraintAdmin})
	pOuter := pvSeedGroup(t, s, "nested-agree-p-outer")
	pMid := pvSeedGroup(t, s, "nested-agree-p-mid")
	pInner := pvSeedGroup(t, s, "nested-agree-p-inner")
	pvSeedGroupMember(t, s, pOuter, store.GroupMemberTypeGroup, pMid)
	pvSeedGroupMember(t, s, pMid, store.GroupMemberTypeGroup, pInner)
	lockoutProjectBinding(t, s, projectRole.ID, "group", pOuter, projectID)
	pHuman := pvSeedUser(t, s, "nested-agree-p-human")
	pvSeedGroupMember(t, s, pInner, store.GroupMemberTypeUser, pHuman)
	pFixture := tiStoreFixture(t, s, generateID(), time.Now().Add(time.Hour))
	pvSeedGroupMember(t, s, pInner, store.GroupMemberTypeUser, pFixture.ID)
	pOutsider := pvSeedUser(t, s, "nested-agree-p-outsider")

	admins, err = resolveConstraintAdminUsers(ctx, s, time.Now(), store.RoleScopeProject, projectID)
	require.NoError(t, err)
	resolved = map[string]bool{}
	for _, a := range admins {
		resolved[a.userID] = true
	}
	fromResolver, fromAllow = nil, nil
	for _, id := range []string{pHuman, pFixture.ID, pOutsider} {
		if resolved[id] {
			fromResolver = append(fromResolver, id)
		}
		ok, err := gs.isConstraintAdmin(ctx, id)
		require.NoError(t, err)
		if ok {
			fromAllow = append(fromAllow, id)
		}
	}
	want = []string{pHuman, pFixture.ID}
	sort.Strings(want)
	sort.Strings(fromResolver)
	sort.Strings(fromAllow)
	assert.Equal(t, want, fromResolver)
	assert.Equal(t, want, fromAllow)
}

// A test identity that holds admin only through a nested group is not a
// remaining admin, so removing the last human admin's binding is refused:
// a test fixture at system scope, and an admin-kind identity at project
// scope. Control: a human in the nested group keeps an admin.
func TestConstraintAdminLockout_NestedGroupTestIdentityNotCounted(t *testing.T) {
	t.Run("fixture-system", func(t *testing.T) {
		srv, s := testServer(t)
		ctx := context.Background()
		lockoutSuspendDevUser(t, srv, s)
		role := tiCustomSystemRole(t, s, "nested-ti-sys-role", PermissionConstraintAdmin)
		rd, err := s.GetRoleDefinitionByName(ctx, role, store.RoleScopeSystem)
		require.NoError(t, err)
		outer := tiGroupWithSystemRole(t, s, "nested-ti-sys-outer", role, DevUserID)
		inner := pvSeedGroup(t, s, "nested-ti-sys-inner")
		pvSeedGroupMember(t, s, outer.ID, store.GroupMemberTypeGroup, inner)
		fixture := tiStoreFixture(t, s, generateID(), time.Now().Add(time.Hour))
		pvSeedGroupMember(t, s, inner, store.GroupMemberTypeUser, fixture.ID)
		human, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: rd.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      pvSeedUser(t, s, "nested-ti-sys-human"),
			ScopeType:        store.RoleScopeSystem,
			CreatedBy:        "test",
		})
		require.NoError(t, err)

		requireLockoutErr(t, checkRoleBindingRemovalAdminLockout(ctx, s, time.Now(), human))

		pvSeedGroupMember(t, s, inner, store.GroupMemberTypeUser,
			pvSeedUser(t, s, "nested-ti-sys-other"))
		assert.NoError(t, checkRoleBindingRemovalAdminLockout(ctx, s, time.Now(), human),
			"control: a human in the nested group keeps an admin")
	})

	t.Run("admin-kind-project", func(t *testing.T) {
		_, _, _, s := govTestSetup(t)
		ctx := context.Background()
		hubAdmin, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
		require.NoError(t, err)
		expires := time.Now().Add(time.Hour)
		issuer, issuance, purpose := DevUserID, generateID(), "nested lockout test"
		identity := &store.User{
			ID:          generateID(),
			Email:       "nested-ti-admin@" + store.TestHubAdminEmailDomain,
			DisplayName: "nested-ti-admin",
			Role:        store.TestAdminRole(store.UserKindTestHubAdmin),
			Status:      store.UserStatusActive,
			Kind:        store.UserKindTestHubAdmin,
			ExpiresAt:   &expires,
			IssuedBy:    &issuer,
			Purpose:     &purpose,
			IssuanceID:  &issuance,
			Created:     time.Now(),
		}
		_, err = s.CreateTestAdminUser(ctx, identity, hubAdmin.ID)
		require.NoError(t, err)

		f := newNestedAdminFixture(t, s, "nested-ti-admin", true)
		pvSeedGroupMember(t, s, f.inner, store.GroupMemberTypeUser, identity.ID)
		require.NoError(t, s.RemoveGroupMember(ctx, f.inner, store.GroupMemberTypeUser, f.member))
		human := lockoutProjectBinding(t, s, f.roleID, "user",
			pvSeedUser(t, s, "nested-ti-admin-human"), f.projectID)

		requireLockoutErr(t, checkRoleBindingRemovalAdminLockout(ctx, s, time.Now(), human))

		pvSeedGroupMember(t, s, f.inner, store.GroupMemberTypeUser, f.member)
		assert.NoError(t, checkRoleBindingRemovalAdminLockout(ctx, s, time.Now(), human),
			"control: a human in the nested group keeps an admin")
	})
}

// ---------------------------------------------------------------------------
// Scheduled constraints: binding validity at the activation time
// ---------------------------------------------------------------------------

// scheduledFixture seeds a project with admin binding removed for one
// user and an admin binding for another that expires at survivorExpiry,
// plus a constraint without access_constraint.admin for a third,
// non-admin user that takes effect at notBefore (nil: in effect now).
func scheduledFixture(t *testing.T, s store.Store, name string, survivorExpiry time.Time, notBefore *time.Time) (string, *store.RoleBinding) {
	t.Helper()
	ctx := context.Background()
	projectID := pvSeedProject(t, s, name+"-project")
	rd := createTestRoleDefinition(t, s, name+"-admin-role", store.RoleScopeProject,
		[]string{PermissionConstraintAdmin, "agent.read"})
	removed := lockoutProjectBinding(t, s, rd.ID, "user", pvSeedUser(t, s, name+"-removed"), projectID)
	_, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    "user",
		PrincipalID:      pvSeedUser(t, s, name+"-survivor"),
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		ExpiresAt:        &survivorExpiry,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
	_, err = s.CreateAccessConstraint(ctx, &store.AccessConstraint{
		Name:                 name + "-constraint",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(pvSeedUser(t, s, name+"-subject")),
		ScopeType:            store.RoleScopeProject,
		ScopeID:              projectID,
		MaximumPermissions:   []string{"agent.read"},
		NotBefore:            notBefore,
		Purpose:              "scheduled validity test",
		CreatedBy:            "test",
	})
	require.NoError(t, err)
	return projectID, removed
}

// When a scheduled constraint takes effect, a remaining admin counts only
// through a binding still valid at that activation time.
func TestConstraintAdminLockout_ScheduledSurvivorValidAtActivation(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	activation := now.Add(48 * time.Hour)
	for _, tc := range []struct {
		name      string
		expiry    time.Time
		notBefore *time.Time
		refused   bool
	}{
		{name: "expires-before-activation", expiry: now.Add(24 * time.Hour), notBefore: &activation, refused: true},
		{name: "valid-at-activation", expiry: now.Add(72 * time.Hour), notBefore: &activation},
		{name: "constraint-in-effect", expiry: now.Add(24 * time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, s := govTestSetup(t)
			_, removed := scheduledFixture(t, s, "sched-valid-"+tc.name, tc.expiry, tc.notBefore)
			err := checkRoleBindingRemovalAdminLockout(context.Background(), s, now, removed)
			if !tc.refused {
				assert.NoError(t, err)
				return
			}
			requireLockoutErr(t, err)
			assert.Contains(t, err.Error(), "when scheduled constraints activate")
		})
	}
}

// A failed scope binding read for the activation-time check refuses a
// user removal.
func TestConstraintAdminLockout_ScheduledScopeReadFailureRefuses(t *testing.T) {
	_, _, _, s := govTestSetup(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	activation := now.Add(48 * time.Hour)
	projectID, removed := scheduledFixture(t, s, "sched-readerr", now.Add(72*time.Hour), &activation)
	rm := adminRemoval{userID: removed.PrincipalID, userAction: userRemovalSuspend}
	require.NoError(t, checkConstraintAdminLockout(ctx, s, now, store.RoleScopeProject, projectID, rm),
		"control: reads succeed")

	calls := 0
	st := lockoutReadErrStore{Store: s, scopeErr: errors.New("read failed"),
		scopeErrFromCall: 2, scopeCalls: &calls}
	requireLockoutErr(t, checkConstraintAdminLockout(ctx, st, now, store.RoleScopeProject, projectID, rm))
	assert.Equal(t, 2, calls, "the admin resolution read succeeded and the second read failed")
}

// Every user the admin resolution reaches from a bound group, walking
// group members down, has that group among the user's effective groups
// (GetEffectiveGroups, which walks parent groups up). The graph nests
// three groups in a cycle and nests a project_agents group (agent
// members only) in the middle one; the admin role is bound to the outer
// group and to the project_agents group.
func TestConstraintAdminLockout_GroupWalksAgree(t *testing.T) {
	_, _, _, s := govTestSetup(t)
	ctx := context.Background()
	projectID := pvSeedProject(t, s, "walk-agree-project")
	rd := createTestRoleDefinition(t, s, "walk-agree-admin-role", store.RoleScopeProject,
		[]string{PermissionConstraintAdmin})
	outer := pvSeedGroup(t, s, "walk-agree-outer")
	mid := pvSeedGroup(t, s, "walk-agree-mid")
	inner := pvSeedGroup(t, s, "walk-agree-inner")
	pvSeedGroupMember(t, s, outer, store.GroupMemberTypeGroup, mid)
	pvSeedGroupMember(t, s, mid, store.GroupMemberTypeGroup, inner)
	pvSeedGroupMember(t, s, inner, store.GroupMemberTypeGroup, outer)
	agents := tid("walk-agree-agents")
	require.NoError(t, s.CreateGroup(ctx, &store.Group{
		ID:        agents,
		Name:      "walk-agree-agents",
		Slug:      "project:walk-agree:agents",
		GroupType: store.GroupTypeProjectAgents,
		ProjectID: projectID,
	}))
	pvSeedGroupMember(t, s, mid, store.GroupMemberTypeGroup, agents)
	var users []string
	for i, g := range []string{outer, mid, inner} {
		u := pvSeedUser(t, s, fmt.Sprintf("walk-agree-user-%d", i))
		pvSeedGroupMember(t, s, g, store.GroupMemberTypeUser, u)
		users = append(users, u)
	}
	lockoutProjectBinding(t, s, rd.ID, "group", outer, projectID)
	lockoutProjectBinding(t, s, rd.ID, "group", agents, projectID)

	admins, err := resolveConstraintAdminUsers(ctx, s, time.Now(), store.RoleScopeProject, projectID)
	require.NoError(t, err)
	var reached []string
	for _, a := range admins {
		reached = append(reached, a.userID)
		groups, err := s.GetEffectiveGroups(ctx, a.userID)
		require.NoError(t, err)
		assert.Contains(t, groups, outer,
			"user %s reached from the bound group has it among its effective groups", a.userID)
	}
	assert.ElementsMatch(t, users, reached, "the walk reaches every user in the cycle")
}
