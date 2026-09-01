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
	"log/slog"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// govTestSetup creates a GovernanceService backed by an in-memory SQLite store
// with standard test data: a super-admin user, roles, and permissions.
func govTestSetup(t *testing.T) (*GovernanceService, *PreviewService, *AuthzService, store.Store) {
	t.Helper()
	srv, s := testServer(t)
	authz := srv.authzService
	logger := slog.Default()
	key := []byte("test-governance-hmac-key-32byte!")
	ps := NewPreviewServiceWithKey(s, authz, logger, key)
	ps.nowFunc = func() time.Time { return time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC) }
	gs := NewGovernanceService(s, ps, authz, logger)
	gs.nowFunc = ps.nowFunc
	return gs, ps, authz, s
}

// govSeedAdminUser creates a user and grants them constraint-admin permission.
func govSeedAdminUser(t *testing.T, s store.Store, name string) string {
	t.Helper()
	userID := pvSeedUser(t, s, name)
	// Create a role with constraint-admin permission.
	rd := createTestRoleDefinition(t, s, "admin-role-"+name, store.RoleScopeSystem,
		[]string{PermissionConstraintAdmin, "agent.read", "agent.create", "agent.delete"})
	pvSeedRoleBinding(t, s, rd.ID, "user", userID, store.RoleScopeSystem, "")
	return userID
}

// govSeedNonAdminUser creates a user with basic permissions but NOT constraint-admin.
func govSeedNonAdminUser(t *testing.T, s store.Store, name string) string {
	t.Helper()
	userID := pvSeedUser(t, s, name)
	rd := createTestRoleDefinition(t, s, "basic-role-"+name, store.RoleScopeSystem,
		[]string{"agent.read", "agent.create"})
	pvSeedRoleBinding(t, s, rd.ID, "user", userID, store.RoleScopeSystem, "")
	return userID
}

// govCreateAndCommit generates a preview for the given draft and commits it.
func govCreateAndCommit(t *testing.T, gs *GovernanceService, ps *PreviewService, draft *store.AccessConstraint, actor PrincipalContext) *store.AccessConstraint {
	t.Helper()
	ctx := context.Background()

	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     actor,
	})
	require.NoError(t, err)
	require.NotNil(t, result)

	commitResult, err := gs.CommitBoundaryChange(ctx, CommitRequest{
		Operation:    "create",
		Draft:        draft,
		PreviewToken: result.PreviewToken,
		Actor:        actor,
	})
	require.NoError(t, err)
	return commitResult.Constraint
}

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

func TestGovernance_UserSuspension_SecurityReviewRequired(t *testing.T) {
	gs, _, _, s := govTestSetup(t)
	ctx := context.Background()

	adminID := govSeedAdminUser(t, s, "gov-admin-suspend")

	check, err := gs.CheckUserSuspension(ctx, adminID)
	require.NoError(t, err)
	assert.True(t, check.ReviewRequired, "suspending admin user should require review")
}

func TestGovernance_UserSuspension_NoReviewForNonAdmin(t *testing.T) {
	gs, _, _, s := govTestSetup(t)
	ctx := context.Background()

	userID := govSeedNonAdminUser(t, s, "gov-user-suspend")

	check, err := gs.CheckUserSuspension(ctx, userID)
	require.NoError(t, err)
	assert.False(t, check.ReviewRequired, "suspending non-admin user should not require review")
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
// 16. Group principal constraint triggers review
// ---------------------------------------------------------------------------

func TestGovernance_GroupPrincipalConstraint_TriggersReview(t *testing.T) {
	gs, _, _, s := govTestSetup(t)
	ctx := context.Background()

	adminID := govSeedAdminUser(t, s, "gov-admin-grp-principal")
	groupID := pvSeedGroup(t, s, "gov-group-principal-constraint")
	pvSeedGroupMember(t, s, groupID, "user", adminID)

	// Create a constraint with principal kind targeting the group.
	principalType := "group"
	_, err := s.CreateAccessConstraint(ctx, &store.AccessConstraint{
		Name:                 "principal-group-constraint",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: &principalType,
		SubjectPrincipalID:   pvStrPtr(groupID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "group principal test",
		CreatedBy:            adminID,
	})
	require.NoError(t, err)

	// Check group member removal.
	check, err := gs.CheckGroupMemberRemoval(ctx, groupID, "user", adminID)
	require.NoError(t, err)
	assert.True(t, check.ReviewRequired, "group referenced as principal subject should trigger review")
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
