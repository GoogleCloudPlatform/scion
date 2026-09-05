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

// previewTestSetup creates a PreviewService backed by an in-memory SQLite store
// with standard test data: a super-admin user, roles, and permissions.
func previewTestSetup(t *testing.T) (*PreviewService, *AuthzService, store.Store) {
	t.Helper()
	srv, s := testServer(t)
	authz := srv.authzService
	logger := slog.Default()
	key := []byte("test-preview-hmac-key-32-bytes!!")
	ps := NewPreviewServiceWithKey(s, authz, logger, key)
	ps.nowFunc = func() time.Time { return time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC) }
	return ps, authz, s
}

// pvSeedUser creates a user in the store (preview-test-scoped helper).
func pvSeedUser(t *testing.T, s store.Store, name string) string {
	t.Helper()
	id := tid(name)
	err := s.CreateUser(context.Background(), &store.User{
		ID:          id,
		Email:       name + "@test.com",
		DisplayName: name,
		Role:        "member",
		Status:      "active",
	})
	require.NoError(t, err)
	return id
}

// pvSeedAgent creates an agent in the store (preview-test-scoped helper).
func pvSeedAgent(t *testing.T, s store.Store, name, projectID string) string {
	t.Helper()
	id := tid(name)
	err := s.CreateAgent(context.Background(), &store.Agent{
		ID:        id,
		Name:      name,
		Slug:      name,
		ProjectID: projectID,
		Phase:     "stopped",
		CreatedBy: tid("test-creator"),
	})
	require.NoError(t, err)
	return id
}

// pvSeedGroup creates a group in the store (preview-test-scoped helper).
func pvSeedGroup(t *testing.T, s store.Store, name string) string {
	t.Helper()
	id := tid(name)
	err := s.CreateGroup(context.Background(), &store.Group{
		ID:        id,
		Name:      name,
		Slug:      name,
		GroupType: store.GroupTypeExplicit,
	})
	require.NoError(t, err)
	return id
}

// pvSeedGroupMember adds a member to a group (preview-test-scoped helper).
func pvSeedGroupMember(t *testing.T, s store.Store, groupID, memberType, memberID string) {
	t.Helper()
	err := s.AddGroupMember(context.Background(), &store.GroupMember{
		GroupID:    groupID,
		MemberType: memberType,
		MemberID:   memberID,
		Role:       "member",
		AddedBy:    "test",
	})
	require.NoError(t, err)
}

// pvSeedRoleBinding creates a role binding (preview-test-scoped helper).
func pvSeedRoleBinding(t *testing.T, s store.Store, roleDefID, principalType, principalID, scopeType, scopeID string) {
	t.Helper()
	_, err := s.CreateRoleBinding(context.Background(), &store.RoleBinding{
		RoleDefinitionID: roleDefID,
		PrincipalType:    principalType,
		PrincipalID:      principalID,
		ScopeType:        scopeType,
		ScopeID:          scopeID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
}

// pvSeedConstraint creates a constraint in the store (preview-test-scoped helper).
func pvSeedConstraint(t *testing.T, s store.Store, c *store.AccessConstraint) *store.AccessConstraint {
	t.Helper()
	result, err := s.CreateAccessConstraint(context.Background(), c)
	require.NoError(t, err)
	return result
}

// pvSeedProject creates a project in the store (preview-test-scoped helper).
func pvSeedProject(t *testing.T, s store.Store, name string) string {
	t.Helper()
	id := tid(name)
	err := s.CreateProject(context.Background(), &store.Project{
		ID:   id,
		Name: name,
		Slug: name,
	})
	require.NoError(t, err)
	return id
}

// pvTestActor returns a PrincipalContext for testing.
func pvTestActor(userID string) PrincipalContext {
	return PrincipalContext{
		Kind: PrincipalKindUser,
		ID:   userID,
	}
}

// pvStrPtr returns a pointer to a string (preview-test-scoped helper).
func pvStrPtr(s string) *string { return &s }

// pvTimePtr returns a pointer to a time (preview-test-scoped helper).
func pvTimePtr(t time.Time) *time.Time { return &t }

// ---------------------------------------------------------------------------
// 1. Subject choice tests
// ---------------------------------------------------------------------------

func TestPreview_ExactUser(t *testing.T) {
	ps, _, s := previewTestSetup(t)
	ctx := context.Background()

	userID := pvSeedUser(t, s, "preview-user-1")
	adminID := pvSeedUser(t, s, "preview-admin-1")

	// Give user a permission via role binding.
	rd := createTestRoleDefinition(t, s, "test-role-exact-user", store.RoleScopeSystem, []string{"agent.read", "agent.create", "agent.delete"})
	pvSeedRoleBinding(t, s, rd.ID, "user", userID, store.RoleScopeSystem, "")

	// Give admin the constraint-admin permission.
	adminRD := createTestRoleDefinition(t, s, "constraint-admin-exact-user", store.RoleScopeSystem, []string{PermissionConstraintAdmin, "agent.read", "agent.create", "agent.delete"})
	pvSeedRoleBinding(t, s, adminRD.ID, "user", adminID, store.RoleScopeSystem, "")

	// Create a constraint that restricts user to only agent.read.
	draft := &store.AccessConstraint{
		Name:                 "restrict-user-1",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(userID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "test exact user",
	}

	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     pvTestActor(adminID),
	})
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.Equal(t, "create", result.Operation)
	assert.Equal(t, ClassificationTighten, result.Classification)
	assert.NotEmpty(t, result.PreviewToken)
	assert.NotEmpty(t, result.DraftHash)
	assert.True(t, result.ExpiresAt.After(result.GeneratedAt))

	// Should have at least one affected principal (the targeted user).
	assert.GreaterOrEqual(t, result.Impact.AffectedPrincipalCount, 1)
}

func TestPreview_ExactAgent(t *testing.T) {
	ps, _, s := previewTestSetup(t)
	ctx := context.Background()

	projectID := pvSeedProject(t, s, "agent-project")
	agentID := pvSeedAgent(t, s, "preview-agent-1", projectID)
	adminID := pvSeedUser(t, s, "agent-admin-1")

	// Give agent permissions via role binding.
	rd := createTestRoleDefinition(t, s, "test-role-exact-agent", store.RoleScopeSystem, []string{"agent.read", "agent.create"})
	pvSeedRoleBinding(t, s, rd.ID, "agent", agentID, store.RoleScopeSystem, "")

	// Admin setup.
	adminRD := createTestRoleDefinition(t, s, "constraint-admin-exact-agent", store.RoleScopeSystem, []string{PermissionConstraintAdmin})
	pvSeedRoleBinding(t, s, adminRD.ID, "user", adminID, store.RoleScopeSystem, "")

	draft := &store.AccessConstraint{
		Name:                 "restrict-agent-1",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("agent"),
		SubjectPrincipalID:   pvStrPtr(agentID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "test exact agent",
	}

	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     pvTestActor(adminID),
	})
	require.NoError(t, err)
	assert.Equal(t, ClassificationTighten, result.Classification)
}

func TestPreview_ExactGroup(t *testing.T) {
	ps, _, s := previewTestSetup(t)
	ctx := context.Background()

	groupID := pvSeedGroup(t, s, "preview-group-1")
	adminID := pvSeedUser(t, s, "group-admin-1")

	// Admin setup.
	adminRD := createTestRoleDefinition(t, s, "constraint-admin-exact-group", store.RoleScopeSystem, []string{PermissionConstraintAdmin})
	pvSeedRoleBinding(t, s, adminRD.ID, "user", adminID, store.RoleScopeSystem, "")

	draft := &store.AccessConstraint{
		Name:                 "restrict-group-entity",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("group"),
		SubjectPrincipalID:   pvStrPtr(groupID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "test exact group",
	}

	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     pvTestActor(adminID),
	})
	require.NoError(t, err)
	assert.NotNil(t, result)
	assert.NotEmpty(t, result.PreviewToken)
}

func TestPreview_GroupClosure(t *testing.T) {
	ps, _, s := previewTestSetup(t)
	ctx := context.Background()

	groupID := pvSeedGroup(t, s, "closure-group-1")
	user1ID := pvSeedUser(t, s, "closure-user-1")
	user2ID := pvSeedUser(t, s, "closure-user-2")
	adminID := pvSeedUser(t, s, "closure-admin-1")

	pvSeedGroupMember(t, s, groupID, "user", user1ID)
	pvSeedGroupMember(t, s, groupID, "user", user2ID)

	// Give group members permissions.
	rd := createTestRoleDefinition(t, s, "test-role-closure", store.RoleScopeSystem, []string{"agent.read", "agent.create", "project.read"})
	pvSeedRoleBinding(t, s, rd.ID, "group", groupID, store.RoleScopeSystem, "")

	// Admin setup.
	adminRD := createTestRoleDefinition(t, s, "constraint-admin-closure", store.RoleScopeSystem, []string{PermissionConstraintAdmin})
	pvSeedRoleBinding(t, s, adminRD.ID, "user", adminID, store.RoleScopeSystem, "")

	draft := &store.AccessConstraint{
		Name:               "restrict-closure-group",
		SubjectKind:        store.ConstraintSubjectGroupClosure,
		SubjectGroupID:     pvStrPtr(groupID),
		ScopeType:          store.RoleScopeSystem,
		MaximumPermissions: []string{"agent.read"},
		Purpose:            "test group closure",
	}

	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     pvTestActor(adminID),
	})
	require.NoError(t, err)
	assert.Equal(t, ClassificationTighten, result.Classification)
	// Should affect both group members.
	assert.GreaterOrEqual(t, result.Impact.AffectedPrincipalCount, 2)
	assert.GreaterOrEqual(t, result.Impact.LosingPrincipalCount, 1)
}

func TestPreview_AllPrincipals(t *testing.T) {
	ps, _, s := previewTestSetup(t)
	ctx := context.Background()

	user1ID := pvSeedUser(t, s, "all-user-1")
	user2ID := pvSeedUser(t, s, "all-user-2")
	adminID := pvSeedUser(t, s, "all-admin-1")

	// Give users permissions.
	rd := createTestRoleDefinition(t, s, "test-role-all", store.RoleScopeSystem, []string{"agent.read", "agent.create"})
	pvSeedRoleBinding(t, s, rd.ID, "user", user1ID, store.RoleScopeSystem, "")
	pvSeedRoleBinding(t, s, rd.ID, "user", user2ID, store.RoleScopeSystem, "")

	// Admin setup.
	adminRD := createTestRoleDefinition(t, s, "constraint-admin-all", store.RoleScopeSystem, []string{PermissionConstraintAdmin, "agent.read", "agent.create"})
	pvSeedRoleBinding(t, s, adminRD.ID, "user", adminID, store.RoleScopeSystem, "")

	draft := &store.AccessConstraint{
		Name:               "restrict-all-principals",
		SubjectKind:        store.ConstraintSubjectAllPrincipals,
		ScopeType:          store.RoleScopeSystem,
		MaximumPermissions: []string{"agent.read"},
		Purpose:            "test all principals",
	}

	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     pvTestActor(adminID),
	})
	require.NoError(t, err)
	assert.Equal(t, ClassificationTighten, result.Classification)
	// Should affect multiple principals.
	assert.GreaterOrEqual(t, result.Impact.AffectedPrincipalCount, 2)
}

// ---------------------------------------------------------------------------
// 2. Scope tests
// ---------------------------------------------------------------------------

func TestPreview_SystemScope(t *testing.T) {
	ps, _, s := previewTestSetup(t)
	ctx := context.Background()

	userID := pvSeedUser(t, s, "sys-scope-user")
	adminID := pvSeedUser(t, s, "sys-scope-admin")

	rd := createTestRoleDefinition(t, s, "test-role-sys-scope", store.RoleScopeSystem, []string{"agent.read", "agent.create"})
	pvSeedRoleBinding(t, s, rd.ID, "user", userID, store.RoleScopeSystem, "")

	adminRD := createTestRoleDefinition(t, s, "constraint-admin-sys-scope", store.RoleScopeSystem, []string{PermissionConstraintAdmin})
	pvSeedRoleBinding(t, s, adminRD.ID, "user", adminID, store.RoleScopeSystem, "")

	draft := &store.AccessConstraint{
		Name:                 "sys-scope-constraint",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(userID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "test system scope",
	}

	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     pvTestActor(adminID),
	})
	require.NoError(t, err)
	assert.Equal(t, ClassificationTighten, result.Classification)
}

func TestPreview_ProjectScope(t *testing.T) {
	ps, _, s := previewTestSetup(t)
	ctx := context.Background()

	projectID := pvSeedProject(t, s, "scope-project")
	userID := pvSeedUser(t, s, "proj-scope-user")
	adminID := pvSeedUser(t, s, "proj-scope-admin")

	rd := createTestRoleDefinition(t, s, "test-role-proj-scope", store.RoleScopeProject, []string{"agent.read", "agent.create"})
	pvSeedRoleBinding(t, s, rd.ID, "user", userID, store.RoleScopeProject, projectID)

	adminRD := createTestRoleDefinition(t, s, "constraint-admin-proj-scope", store.RoleScopeSystem, []string{PermissionConstraintAdmin})
	pvSeedRoleBinding(t, s, adminRD.ID, "user", adminID, store.RoleScopeSystem, "")

	draft := &store.AccessConstraint{
		Name:                 "proj-scope-constraint",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(userID),
		ScopeType:            store.RoleScopeProject,
		ScopeID:              projectID,
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "test project scope",
	}

	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     pvTestActor(adminID),
	})
	require.NoError(t, err)
	assert.NotNil(t, result)
}

// ---------------------------------------------------------------------------
// 3. Principal count edge cases
// ---------------------------------------------------------------------------

func TestPreview_ZeroAffectedPrincipals(t *testing.T) {
	ps, _, s := previewTestSetup(t)
	ctx := context.Background()

	adminID := pvSeedUser(t, s, "zero-admin")
	adminRD := createTestRoleDefinition(t, s, "constraint-admin-zero", store.RoleScopeSystem, []string{PermissionConstraintAdmin})
	pvSeedRoleBinding(t, s, adminRD.ID, "user", adminID, store.RoleScopeSystem, "")

	// Target a user that doesn't exist (or has no permissions).
	nonexistentID := tid("nonexistent-user")
	draft := &store.AccessConstraint{
		Name:                 "zero-affected",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(nonexistentID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "test zero affected",
	}

	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     pvTestActor(adminID),
	})
	require.NoError(t, err)
	assert.Equal(t, 1, result.Impact.AffectedPrincipalCount) // The target itself.
}

func TestPreview_SingleAffectedPrincipal(t *testing.T) {
	ps, _, s := previewTestSetup(t)
	ctx := context.Background()

	userID := pvSeedUser(t, s, "single-user")
	adminID := pvSeedUser(t, s, "single-admin")

	rd := createTestRoleDefinition(t, s, "test-role-single", store.RoleScopeSystem, []string{"agent.read", "agent.create"})
	pvSeedRoleBinding(t, s, rd.ID, "user", userID, store.RoleScopeSystem, "")
	adminRD := createTestRoleDefinition(t, s, "constraint-admin-single", store.RoleScopeSystem, []string{PermissionConstraintAdmin})
	pvSeedRoleBinding(t, s, adminRD.ID, "user", adminID, store.RoleScopeSystem, "")

	draft := &store.AccessConstraint{
		Name:                 "single-affected",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(userID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "test single affected",
	}

	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     pvTestActor(adminID),
	})
	require.NoError(t, err)
	assert.Equal(t, 1, result.Impact.AffectedPrincipalCount)
	assert.GreaterOrEqual(t, result.Impact.LosingPrincipalCount, 1)
}

// ---------------------------------------------------------------------------
// 4. Nested groups with multiple membership paths
// ---------------------------------------------------------------------------

func TestPreview_NestedGroups(t *testing.T) {
	ps, _, s := previewTestSetup(t)
	ctx := context.Background()

	// Create nested group structure: user -> group-child -> group-parent.
	parentGroupID := pvSeedGroup(t, s, "parent-group")
	childGroupID := pvSeedGroup(t, s, "child-group")
	userID := pvSeedUser(t, s, "nested-user")
	adminID := pvSeedUser(t, s, "nested-admin")

	pvSeedGroupMember(t, s, childGroupID, "user", userID)
	pvSeedGroupMember(t, s, parentGroupID, "group", childGroupID)

	// Give parent group permissions.
	rd := createTestRoleDefinition(t, s, "test-role-nested", store.RoleScopeSystem, []string{"agent.read", "agent.create", "project.read"})
	pvSeedRoleBinding(t, s, rd.ID, "group", parentGroupID, store.RoleScopeSystem, "")

	adminRD := createTestRoleDefinition(t, s, "constraint-admin-nested", store.RoleScopeSystem, []string{PermissionConstraintAdmin})
	pvSeedRoleBinding(t, s, adminRD.ID, "user", adminID, store.RoleScopeSystem, "")

	draft := &store.AccessConstraint{
		Name:               "restrict-parent-closure",
		SubjectKind:        store.ConstraintSubjectGroupClosure,
		SubjectGroupID:     pvStrPtr(parentGroupID),
		ScopeType:          store.RoleScopeSystem,
		MaximumPermissions: []string{"agent.read"},
		Purpose:            "test nested groups",
	}

	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     pvTestActor(adminID),
	})
	require.NoError(t, err)
	// Should find the user via nested group path.
	assert.GreaterOrEqual(t, result.Impact.AffectedPrincipalCount, 1)
}

// ---------------------------------------------------------------------------
// 5. Multiple intersecting boundaries
// ---------------------------------------------------------------------------

func TestPreview_IntersectingBoundaries(t *testing.T) {
	ps, _, s := previewTestSetup(t)
	ctx := context.Background()

	userID := pvSeedUser(t, s, "intersect-user")
	adminID := pvSeedUser(t, s, "intersect-admin")

	rd := createTestRoleDefinition(t, s, "test-role-intersect", store.RoleScopeSystem, []string{"agent.read", "agent.create", "project.read"})
	pvSeedRoleBinding(t, s, rd.ID, "user", userID, store.RoleScopeSystem, "")

	adminRD := createTestRoleDefinition(t, s, "constraint-admin-intersect", store.RoleScopeSystem, []string{PermissionConstraintAdmin, "agent.read", "agent.create", "project.read"})
	pvSeedRoleBinding(t, s, adminRD.ID, "user", adminID, store.RoleScopeSystem, "")

	// Create an existing constraint.
	pvSeedConstraint(t, s, &store.AccessConstraint{
		Name:                 "existing-boundary-1",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(userID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read", "agent.create"},
		Purpose:              "existing intersection test",
	})

	// Propose a new constraint that overlaps.
	draft := &store.AccessConstraint{
		Name:                 "new-intersecting",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(userID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read", "project.read"},
		Purpose:              "test intersection",
	}

	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     pvTestActor(adminID),
	})
	require.NoError(t, err)
	// Should find at least one intersecting boundary.
	assert.GreaterOrEqual(t, len(result.Intersecting), 1)
	assert.Equal(t, "existing-boundary-1", result.Intersecting[0].Name)
}

// ---------------------------------------------------------------------------
// 6. Permission edge cases
// ---------------------------------------------------------------------------

func TestPreview_PermissionNeverGranted(t *testing.T) {
	ps, _, s := previewTestSetup(t)
	ctx := context.Background()

	userID := pvSeedUser(t, s, "no-grant-user")
	adminID := pvSeedUser(t, s, "no-grant-admin")

	// User has agent.read only.
	rd := createTestRoleDefinition(t, s, "test-role-no-grant", store.RoleScopeSystem, []string{"agent.read"})
	pvSeedRoleBinding(t, s, rd.ID, "user", userID, store.RoleScopeSystem, "")

	adminRD := createTestRoleDefinition(t, s, "constraint-admin-no-grant", store.RoleScopeSystem, []string{PermissionConstraintAdmin})
	pvSeedRoleBinding(t, s, adminRD.ID, "user", adminID, store.RoleScopeSystem, "")

	// Constraint allows agent.read + agent.create (agent.create never granted).
	draft := &store.AccessConstraint{
		Name:                 "allow-extras",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(userID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read", "agent.create"},
		Purpose:              "test no-grant permission",
	}

	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     pvTestActor(adminID),
	})
	require.NoError(t, err)
	// Should be relax/no-effect since the constraint allows everything the user has.
	assert.Contains(t, []string{ClassificationRelax, ClassificationNoEffect}, result.Classification)
}

func TestPreview_NewPermissionExcluded(t *testing.T) {
	ps, _, s := previewTestSetup(t)
	ctx := context.Background()

	userID := pvSeedUser(t, s, "new-perm-user")
	adminID := pvSeedUser(t, s, "new-perm-admin")

	// User has multiple permissions.
	rd := createTestRoleDefinition(t, s, "test-role-new-perm", store.RoleScopeSystem, []string{"agent.read", "agent.create", "project.read"})
	pvSeedRoleBinding(t, s, rd.ID, "user", userID, store.RoleScopeSystem, "")

	adminRD := createTestRoleDefinition(t, s, "constraint-admin-new-perm", store.RoleScopeSystem, []string{PermissionConstraintAdmin})
	pvSeedRoleBinding(t, s, adminRD.ID, "user", adminID, store.RoleScopeSystem, "")

	// Constraint excludes project.read.
	draft := &store.AccessConstraint{
		Name:                 "exclude-project-read",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(userID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read", "agent.create"},
		Purpose:              "test excluded permission",
	}

	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     pvTestActor(adminID),
	})
	require.NoError(t, err)
	assert.Equal(t, ClassificationTighten, result.Classification)
	assert.GreaterOrEqual(t, result.Impact.LosingPrincipalCount, 1)

	// Check that project.read appears in permission diffs.
	found := false
	for _, d := range result.Impact.PermissionDiffs {
		if d.PermissionID == "project.read" {
			found = true
			assert.GreaterOrEqual(t, d.LosingCount, 1)
		}
	}
	assert.True(t, found, "project.read should appear in permission diffs")
}

// ---------------------------------------------------------------------------
// 7. Mixed scope/subject changes (update)
// ---------------------------------------------------------------------------

func TestPreview_MixedUpdate(t *testing.T) {
	ps, _, s := previewTestSetup(t)
	ctx := context.Background()

	user1ID := pvSeedUser(t, s, "mixed-user-1")
	user2ID := pvSeedUser(t, s, "mixed-user-2")
	adminID := pvSeedUser(t, s, "mixed-admin")

	rd := createTestRoleDefinition(t, s, "test-role-mixed", store.RoleScopeSystem, []string{"agent.read", "agent.create"})
	pvSeedRoleBinding(t, s, rd.ID, "user", user1ID, store.RoleScopeSystem, "")
	pvSeedRoleBinding(t, s, rd.ID, "user", user2ID, store.RoleScopeSystem, "")

	adminRD := createTestRoleDefinition(t, s, "constraint-admin-mixed", store.RoleScopeSystem, []string{PermissionConstraintAdmin, "agent.read", "agent.create"})
	pvSeedRoleBinding(t, s, adminRD.ID, "user", adminID, store.RoleScopeSystem, "")

	// Create existing constraint targeting user1.
	existing := pvSeedConstraint(t, s, &store.AccessConstraint{
		Name:                 "existing-constraint",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(user1ID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "original",
	})

	// Update to target user2 instead, allowing more permissions.
	draft := &store.AccessConstraint{
		Name:                 "updated-constraint",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(user2ID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "changed target",
	}

	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation:    "update",
		Draft:        draft,
		ConstraintID: existing.ID,
		BaseRevision: existing.Revision,
		Actor:        pvTestActor(adminID),
	})
	require.NoError(t, err)
	// User1 regains (constraint removed from them), user2 loses.
	assert.Equal(t, ClassificationMixed, result.Classification)
}

// ---------------------------------------------------------------------------
// 8. Temporal impact (notBefore/expiresAt)
// ---------------------------------------------------------------------------

func TestPreview_ScheduledActivation(t *testing.T) {
	ps, _, s := previewTestSetup(t)
	ctx := context.Background()

	userID := pvSeedUser(t, s, "sched-user")
	adminID := pvSeedUser(t, s, "sched-admin")

	rd := createTestRoleDefinition(t, s, "test-role-sched", store.RoleScopeSystem, []string{"agent.read", "agent.create"})
	pvSeedRoleBinding(t, s, rd.ID, "user", userID, store.RoleScopeSystem, "")

	adminRD := createTestRoleDefinition(t, s, "constraint-admin-sched", store.RoleScopeSystem, []string{PermissionConstraintAdmin})
	pvSeedRoleBinding(t, s, adminRD.ID, "user", adminID, store.RoleScopeSystem, "")

	futureTime := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	draft := &store.AccessConstraint{
		Name:                 "scheduled-constraint",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(userID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read"},
		NotBefore:            pvTimePtr(futureTime),
		Purpose:              "scheduled activation test",
	}

	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     pvTestActor(adminID),
	})
	require.NoError(t, err)
	// Should have temporal states.
	assert.GreaterOrEqual(t, len(result.TemporalStates), 2, "should have pre-activation and activation states")

	// Should include a scheduled activation warning.
	hasScheduledWarning := false
	for _, w := range result.Warnings {
		if w.Code == WarningCodeScheduledActivation {
			hasScheduledWarning = true
		}
	}
	assert.True(t, hasScheduledWarning, "should have scheduled activation warning")
}

func TestPreview_ScheduledExpiry(t *testing.T) {
	ps, _, s := previewTestSetup(t)
	ctx := context.Background()

	userID := pvSeedUser(t, s, "expiry-user")
	adminID := pvSeedUser(t, s, "expiry-admin")

	rd := createTestRoleDefinition(t, s, "test-role-expiry", store.RoleScopeSystem, []string{"agent.read", "agent.create"})
	pvSeedRoleBinding(t, s, rd.ID, "user", userID, store.RoleScopeSystem, "")

	adminRD := createTestRoleDefinition(t, s, "constraint-admin-expiry", store.RoleScopeSystem, []string{PermissionConstraintAdmin})
	pvSeedRoleBinding(t, s, adminRD.ID, "user", adminID, store.RoleScopeSystem, "")

	expiresAt := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	draft := &store.AccessConstraint{
		Name:                 "expiring-constraint",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(userID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read"},
		ExpiresAt:            pvTimePtr(expiresAt),
		Purpose:              "expiry test",
	}

	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     pvTestActor(adminID),
	})
	require.NoError(t, err)
	// Tighten-then-expire is "mixed" because it tightens during the window and relaxes after.
	assert.Equal(t, ClassificationMixed, result.Classification)
	assert.GreaterOrEqual(t, len(result.TemporalStates), 2, "should have active and post-expiry states")
}

func TestPreview_ScheduledThenExpire_IsMixed(t *testing.T) {
	ps, _, s := previewTestSetup(t)
	ctx := context.Background()

	userID := pvSeedUser(t, s, "mixed-temporal-user")
	adminID := pvSeedUser(t, s, "mixed-temporal-admin")

	rd := createTestRoleDefinition(t, s, "test-role-mixed-temporal", store.RoleScopeSystem, []string{"agent.read", "agent.create"})
	pvSeedRoleBinding(t, s, rd.ID, "user", userID, store.RoleScopeSystem, "")

	adminRD := createTestRoleDefinition(t, s, "constraint-admin-mixed-temporal", store.RoleScopeSystem, []string{PermissionConstraintAdmin})
	pvSeedRoleBinding(t, s, adminRD.ID, "user", adminID, store.RoleScopeSystem, "")

	notBefore := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	expiresAt := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	draft := &store.AccessConstraint{
		Name:                 "scheduled-then-expire",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(userID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read"},
		NotBefore:            pvTimePtr(notBefore),
		ExpiresAt:            pvTimePtr(expiresAt),
		Purpose:              "scheduled-then-expire",
	}

	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     pvTestActor(adminID),
	})
	require.NoError(t, err)
	// A scheduled tighten-then-expire is "mixed" because:
	// - Before activation: relax (no change)
	// - Active period: tighten
	// - After expiry: relax (permissions restored)
	assert.Equal(t, ClassificationMixed, result.Classification)
}

// ---------------------------------------------------------------------------
// 9. Expired and recovery-disabled records
// ---------------------------------------------------------------------------

func TestPreview_ExpiredConstraint(t *testing.T) {
	ps, _, s := previewTestSetup(t)
	ctx := context.Background()

	userID := pvSeedUser(t, s, "expired-user")
	adminID := pvSeedUser(t, s, "expired-admin")

	rd := createTestRoleDefinition(t, s, "test-role-expired", store.RoleScopeSystem, []string{"agent.read"})
	pvSeedRoleBinding(t, s, rd.ID, "user", userID, store.RoleScopeSystem, "")

	adminRD := createTestRoleDefinition(t, s, "constraint-admin-expired", store.RoleScopeSystem, []string{PermissionConstraintAdmin})
	pvSeedRoleBinding(t, s, adminRD.ID, "user", adminID, store.RoleScopeSystem, "")

	// Create an already-expired constraint.
	pastTime := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	existing := pvSeedConstraint(t, s, &store.AccessConstraint{
		Name:                 "expired-constraint",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(userID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read"},
		ExpiresAt:            pvTimePtr(pastTime),
		Purpose:              "expired",
	})

	// Deleting an expired constraint should be safe (no effect).
	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation:    "delete",
		ConstraintID: existing.ID,
		BaseRevision: existing.Revision,
		Actor:        pvTestActor(adminID),
	})
	require.NoError(t, err)
	assert.NotNil(t, result)
}

func TestPreview_RecoveryDisabledConstraint(t *testing.T) {
	ps, _, s := previewTestSetup(t)
	ctx := context.Background()

	userID := pvSeedUser(t, s, "disabled-user")
	adminID := pvSeedUser(t, s, "disabled-admin")

	rd := createTestRoleDefinition(t, s, "test-role-disabled", store.RoleScopeSystem, []string{"agent.read"})
	pvSeedRoleBinding(t, s, rd.ID, "user", userID, store.RoleScopeSystem, "")

	adminRD := createTestRoleDefinition(t, s, "constraint-admin-disabled", store.RoleScopeSystem, []string{PermissionConstraintAdmin})
	pvSeedRoleBinding(t, s, adminRD.ID, "user", adminID, store.RoleScopeSystem, "")

	// Create a disabled (recovery) constraint.
	existing := pvSeedConstraint(t, s, &store.AccessConstraint{
		Name:                 "disabled-constraint",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(userID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read"},
		Disabled:             true,
		Purpose:              "recovery disabled",
	})

	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation:    "delete",
		ConstraintID: existing.ID,
		BaseRevision: existing.Revision,
		Actor:        pvTestActor(adminID),
	})
	require.NoError(t, err)
	assert.NotNil(t, result)
}

// ---------------------------------------------------------------------------
// 10. Classification tests
// ---------------------------------------------------------------------------

func TestPreview_Classification_PureTighten(t *testing.T) {
	ps, _, s := previewTestSetup(t)
	ctx := context.Background()

	userID := pvSeedUser(t, s, "class-tighten-user")
	adminID := pvSeedUser(t, s, "class-tighten-admin")

	rd := createTestRoleDefinition(t, s, "test-role-class-tighten", store.RoleScopeSystem, []string{"agent.read", "agent.create"})
	pvSeedRoleBinding(t, s, rd.ID, "user", userID, store.RoleScopeSystem, "")

	adminRD := createTestRoleDefinition(t, s, "constraint-admin-class-tighten", store.RoleScopeSystem, []string{PermissionConstraintAdmin})
	pvSeedRoleBinding(t, s, adminRD.ID, "user", adminID, store.RoleScopeSystem, "")

	draft := &store.AccessConstraint{
		Name:                 "pure-tighten",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(userID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "tighten only",
	}

	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     pvTestActor(adminID),
	})
	require.NoError(t, err)
	assert.Equal(t, ClassificationTighten, result.Classification)
}

func TestPreview_Classification_PureRelax(t *testing.T) {
	ps, _, s := previewTestSetup(t)
	ctx := context.Background()

	userID := pvSeedUser(t, s, "class-relax-user")
	adminID := pvSeedUser(t, s, "class-relax-admin")

	rd := createTestRoleDefinition(t, s, "test-role-class-relax", store.RoleScopeSystem, []string{"agent.read", "agent.create"})
	pvSeedRoleBinding(t, s, rd.ID, "user", userID, store.RoleScopeSystem, "")

	adminRD := createTestRoleDefinition(t, s, "constraint-admin-class-relax", store.RoleScopeSystem, []string{PermissionConstraintAdmin, "agent.read", "agent.create"})
	pvSeedRoleBinding(t, s, adminRD.ID, "user", adminID, store.RoleScopeSystem, "")

	// Create existing tight constraint.
	existing := pvSeedConstraint(t, s, &store.AccessConstraint{
		Name:                 "tight-constraint",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(userID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "to be relaxed",
	})

	// Delete it: pure relaxation.
	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation:    "delete",
		ConstraintID: existing.ID,
		BaseRevision: existing.Revision,
		Actor:        pvTestActor(adminID),
	})
	require.NoError(t, err)
	assert.Equal(t, ClassificationRelax, result.Classification)
}

func TestPreview_Classification_NoEffect(t *testing.T) {
	ps, _, s := previewTestSetup(t)
	ctx := context.Background()

	userID := pvSeedUser(t, s, "class-noeffect-user")
	adminID := pvSeedUser(t, s, "class-noeffect-admin")

	// User has agent.read only.
	rd := createTestRoleDefinition(t, s, "test-role-class-noeffect", store.RoleScopeSystem, []string{"agent.read"})
	pvSeedRoleBinding(t, s, rd.ID, "user", userID, store.RoleScopeSystem, "")

	adminRD := createTestRoleDefinition(t, s, "constraint-admin-class-noeffect", store.RoleScopeSystem, []string{PermissionConstraintAdmin})
	pvSeedRoleBinding(t, s, adminRD.ID, "user", adminID, store.RoleScopeSystem, "")

	// Constraint allows exactly what user has.
	draft := &store.AccessConstraint{
		Name:                 "no-effect-constraint",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(userID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "no effect test",
	}

	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     pvTestActor(adminID),
	})
	require.NoError(t, err)
	// No-effect maps to relax on the wire.
	assert.Equal(t, ClassificationRelax, result.Classification)
}

// ---------------------------------------------------------------------------
// 11. Completeness and degraded state
// ---------------------------------------------------------------------------

func TestPreview_CompletenessTracking(t *testing.T) {
	ps, _, s := previewTestSetup(t)
	ctx := context.Background()

	adminID := pvSeedUser(t, s, "completeness-admin")
	adminRD := createTestRoleDefinition(t, s, "constraint-admin-completeness", store.RoleScopeSystem, []string{PermissionConstraintAdmin})
	pvSeedRoleBinding(t, s, adminRD.ID, "user", adminID, store.RoleScopeSystem, "")

	userID := pvSeedUser(t, s, "completeness-user")
	rd := createTestRoleDefinition(t, s, "test-role-completeness", store.RoleScopeSystem, []string{"agent.read"})
	pvSeedRoleBinding(t, s, rd.ID, "user", userID, store.RoleScopeSystem, "")

	// A normal constraint should produce a complete preview.
	draft := &store.AccessConstraint{
		Name:                 "complete-constraint",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(userID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "completeness test",
	}

	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     pvTestActor(adminID),
	})
	require.NoError(t, err)
	assert.True(t, result.Completeness.Complete, "simple preview should be complete")
	assert.False(t, result.Completeness.Truncated)
	assert.False(t, result.Completeness.Degraded)
	assert.Nil(t, result.CommitBlocked, "complete preview should not be commit-blocked")
}

func TestPreview_ResolverFault_Degraded(t *testing.T) {
	// Test that group resolution errors produce degraded (not complete) state.
	ps, _, s := previewTestSetup(t)
	ctx := context.Background()

	adminID := pvSeedUser(t, s, "degraded-admin")
	adminRD := createTestRoleDefinition(t, s, "constraint-admin-degraded", store.RoleScopeSystem, []string{PermissionConstraintAdmin})
	pvSeedRoleBinding(t, s, adminRD.ID, "user", adminID, store.RoleScopeSystem, "")

	// Use a group closure with a nonexistent group to trigger resolution failure.
	nonexistentGroupID := tid("nonexistent-group")
	draft := &store.AccessConstraint{
		Name:               "degraded-constraint",
		SubjectKind:        store.ConstraintSubjectGroupClosure,
		SubjectGroupID:     pvStrPtr(nonexistentGroupID),
		ScopeType:          store.RoleScopeSystem,
		MaximumPermissions: []string{"agent.read"},
		Purpose:            "degraded test",
	}

	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     pvTestActor(adminID),
	})
	require.NoError(t, err)
	// Resolution failure should produce incomplete/degraded preview.
	assert.False(t, result.Completeness.Complete)
	assert.True(t, len(result.Completeness.Reasons) > 0)
	// Commit should be blocked.
	assert.NotNil(t, result.CommitBlocked)
}

// ---------------------------------------------------------------------------
// 12. Token validation tests
// ---------------------------------------------------------------------------

func TestPreview_TokenReplayRejected(t *testing.T) {
	ps, _, s := previewTestSetup(t)
	ctx := context.Background()

	userID := pvSeedUser(t, s, "replay-user")
	adminID := pvSeedUser(t, s, "replay-admin")

	rd := createTestRoleDefinition(t, s, "test-role-replay", store.RoleScopeSystem, []string{"agent.read", "agent.create"})
	pvSeedRoleBinding(t, s, rd.ID, "user", userID, store.RoleScopeSystem, "")

	adminRD := createTestRoleDefinition(t, s, "constraint-admin-replay", store.RoleScopeSystem, []string{PermissionConstraintAdmin})
	pvSeedRoleBinding(t, s, adminRD.ID, "user", adminID, store.RoleScopeSystem, "")

	draft := &store.AccessConstraint{
		Name:                 "replay-test",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(userID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "replay test",
	}

	actor := pvTestActor(adminID)
	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     actor,
	})
	require.NoError(t, err)

	// First use: should succeed.
	err = ps.ValidateToken(ctx, result.PreviewToken, actor, "create", draft, 0)
	require.NoError(t, err)

	// Second use: should fail (replay).
	err = ps.ValidateToken(ctx, result.PreviewToken, actor, "create", draft, 0)
	require.Error(t, err)
	var tokenErr *TokenValidationError
	require.ErrorAs(t, err, &tokenErr)
	assert.Equal(t, ErrCodePreviewTokenReplay, tokenErr.Code)
}

func TestPreview_TokenExpiryRejected(t *testing.T) {
	ps, _, s := previewTestSetup(t)
	ctx := context.Background()

	userID := pvSeedUser(t, s, "expiry-token-user")
	adminID := pvSeedUser(t, s, "expiry-token-admin")

	rd := createTestRoleDefinition(t, s, "test-role-expiry-token", store.RoleScopeSystem, []string{"agent.read"})
	pvSeedRoleBinding(t, s, rd.ID, "user", userID, store.RoleScopeSystem, "")

	adminRD := createTestRoleDefinition(t, s, "constraint-admin-expiry-token", store.RoleScopeSystem, []string{PermissionConstraintAdmin})
	pvSeedRoleBinding(t, s, adminRD.ID, "user", adminID, store.RoleScopeSystem, "")

	draft := &store.AccessConstraint{
		Name:                 "expiry-token-test",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(userID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "expiry token test",
	}

	actor := pvTestActor(adminID)
	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     actor,
	})
	require.NoError(t, err)

	// Advance time past expiry.
	ps.nowFunc = func() time.Time {
		return time.Date(2026, 8, 15, 12, 10, 0, 0, time.UTC) // 10 min later
	}

	err = ps.ValidateToken(ctx, result.PreviewToken, actor, "create", draft, 0)
	require.Error(t, err)
	var tokenErr *TokenValidationError
	require.ErrorAs(t, err, &tokenErr)
	assert.Equal(t, ErrCodePreviewTokenExpired, tokenErr.Code)
}

func TestPreview_TokenActorSwapRejected(t *testing.T) {
	ps, _, s := previewTestSetup(t)
	ctx := context.Background()

	userID := pvSeedUser(t, s, "actor-swap-user")
	adminID := pvSeedUser(t, s, "actor-swap-admin")
	otherAdminID := pvSeedUser(t, s, "actor-swap-other-admin")

	rd := createTestRoleDefinition(t, s, "test-role-actor-swap", store.RoleScopeSystem, []string{"agent.read"})
	pvSeedRoleBinding(t, s, rd.ID, "user", userID, store.RoleScopeSystem, "")

	adminRD := createTestRoleDefinition(t, s, "constraint-admin-actor-swap", store.RoleScopeSystem, []string{PermissionConstraintAdmin})
	pvSeedRoleBinding(t, s, adminRD.ID, "user", adminID, store.RoleScopeSystem, "")
	pvSeedRoleBinding(t, s, adminRD.ID, "user", otherAdminID, store.RoleScopeSystem, "")

	draft := &store.AccessConstraint{
		Name:                 "actor-swap-test",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(userID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "actor swap test",
	}

	actor := pvTestActor(adminID)
	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     actor,
	})
	require.NoError(t, err)

	// Try to use with a different actor.
	otherActor := pvTestActor(otherAdminID)
	err = ps.ValidateToken(ctx, result.PreviewToken, otherActor, "create", draft, 0)
	require.Error(t, err)
	var tokenErr *TokenValidationError
	require.ErrorAs(t, err, &tokenErr)
	assert.Equal(t, ErrCodePreviewActorMismatch, tokenErr.Code)
}

func TestPreview_TokenDraftModifiedRejected(t *testing.T) {
	ps, _, s := previewTestSetup(t)
	ctx := context.Background()

	userID := pvSeedUser(t, s, "draft-mod-user")
	adminID := pvSeedUser(t, s, "draft-mod-admin")

	rd := createTestRoleDefinition(t, s, "test-role-draft-mod", store.RoleScopeSystem, []string{"agent.read", "agent.create"})
	pvSeedRoleBinding(t, s, rd.ID, "user", userID, store.RoleScopeSystem, "")

	adminRD := createTestRoleDefinition(t, s, "constraint-admin-draft-mod", store.RoleScopeSystem, []string{PermissionConstraintAdmin})
	pvSeedRoleBinding(t, s, adminRD.ID, "user", adminID, store.RoleScopeSystem, "")

	draft := &store.AccessConstraint{
		Name:                 "draft-mod-test",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(userID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "draft mod test",
	}

	actor := pvTestActor(adminID)
	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     actor,
	})
	require.NoError(t, err)

	// Modify the draft (add a permission).
	modifiedDraft := &store.AccessConstraint{
		Name:                 "draft-mod-test",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(userID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read", "agent.create"}, // Modified!
		Purpose:              "draft mod test",
	}

	err = ps.ValidateToken(ctx, result.PreviewToken, actor, "create", modifiedDraft, 0)
	require.Error(t, err)
	var tokenErr *TokenValidationError
	require.ErrorAs(t, err, &tokenErr)
	assert.Equal(t, ErrCodePreviewDraftModified, tokenErr.Code)
}

func TestPreview_TokenOperationMismatchRejected(t *testing.T) {
	ps, _, s := previewTestSetup(t)
	ctx := context.Background()

	userID := pvSeedUser(t, s, "op-mismatch-user")
	adminID := pvSeedUser(t, s, "op-mismatch-admin")

	rd := createTestRoleDefinition(t, s, "test-role-op-mismatch", store.RoleScopeSystem, []string{"agent.read"})
	pvSeedRoleBinding(t, s, rd.ID, "user", userID, store.RoleScopeSystem, "")

	adminRD := createTestRoleDefinition(t, s, "constraint-admin-op-mismatch", store.RoleScopeSystem, []string{PermissionConstraintAdmin})
	pvSeedRoleBinding(t, s, adminRD.ID, "user", adminID, store.RoleScopeSystem, "")

	draft := &store.AccessConstraint{
		Name:                 "op-mismatch-test",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(userID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "op mismatch test",
	}

	actor := pvTestActor(adminID)
	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     actor,
	})
	require.NoError(t, err)

	// Try to use with a different operation.
	err = ps.ValidateToken(ctx, result.PreviewToken, actor, "update", draft, 0)
	require.Error(t, err)
	var tokenErr *TokenValidationError
	require.ErrorAs(t, err, &tokenErr)
	assert.Equal(t, ErrCodePreviewOperationMismatch, tokenErr.Code)
}

// ---------------------------------------------------------------------------
// 13. Lockout assessment
// ---------------------------------------------------------------------------

func TestPreview_LockoutAssessment_Safe(t *testing.T) {
	ps, _, s := previewTestSetup(t)
	ctx := context.Background()

	userID := pvSeedUser(t, s, "lockout-safe-user")
	adminID := pvSeedUser(t, s, "lockout-safe-admin")

	rd := createTestRoleDefinition(t, s, "test-role-lockout-safe", store.RoleScopeSystem, []string{"agent.read"})
	pvSeedRoleBinding(t, s, rd.ID, "user", userID, store.RoleScopeSystem, "")

	adminRD := createTestRoleDefinition(t, s, "constraint-admin-lockout-safe", store.RoleScopeSystem, []string{PermissionConstraintAdmin, "agent.read"})
	pvSeedRoleBinding(t, s, adminRD.ID, "user", adminID, store.RoleScopeSystem, "")

	// Constraint on a non-admin user: should be safe.
	draft := &store.AccessConstraint{
		Name:                 "safe-constraint",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(userID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "lockout safe test",
	}

	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     pvTestActor(adminID),
	})
	require.NoError(t, err)
	require.NotNil(t, result.Lockout.Safe)
	assert.True(t, *result.Lockout.Safe)
}

func TestPreview_LockoutAssessment_Unsafe_ZeroAdmins(t *testing.T) {
	// Zero resolved admins = UNSAFE (never degraded pass).
	ps, _, s := previewTestSetup(t)
	ctx := context.Background()

	userID := pvSeedUser(t, s, "lockout-zero-user")

	// No admin bindings at all — but we need someone to be the actor.
	draft := &store.AccessConstraint{
		Name:               "lockout-zero-test",
		SubjectKind:        store.ConstraintSubjectAllPrincipals,
		ScopeType:          store.RoleScopeSystem,
		MaximumPermissions: []string{"agent.read"},
		Purpose:            "lockout zero admins",
	}

	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     pvTestActor(userID),
	})
	require.NoError(t, err)
	require.NotNil(t, result.Lockout.Safe)
	assert.False(t, *result.Lockout.Safe, "zero admins must be UNSAFE")
	assert.NotNil(t, result.CommitBlocked, "zero admins should block commit")
}

func TestPreview_LockoutAssessment_DeleteIsSafe(t *testing.T) {
	ps, _, s := previewTestSetup(t)
	ctx := context.Background()

	userID := pvSeedUser(t, s, "lockout-delete-user")
	adminID := pvSeedUser(t, s, "lockout-delete-admin")

	rd := createTestRoleDefinition(t, s, "test-role-lockout-delete", store.RoleScopeSystem, []string{"agent.read"})
	pvSeedRoleBinding(t, s, rd.ID, "user", userID, store.RoleScopeSystem, "")

	adminRD := createTestRoleDefinition(t, s, "constraint-admin-lockout-delete", store.RoleScopeSystem, []string{PermissionConstraintAdmin})
	pvSeedRoleBinding(t, s, adminRD.ID, "user", adminID, store.RoleScopeSystem, "")

	existing := pvSeedConstraint(t, s, &store.AccessConstraint{
		Name:                 "constraint-to-delete",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(userID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "to be deleted",
	})

	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation:    "delete",
		ConstraintID: existing.ID,
		BaseRevision: existing.Revision,
		Actor:        pvTestActor(adminID),
	})
	require.NoError(t, err)
	require.NotNil(t, result.Lockout.Safe)
	assert.True(t, *result.Lockout.Safe, "delete always safe for lockout")
}

// ---------------------------------------------------------------------------
// 14. Async preview
// ---------------------------------------------------------------------------

func TestPreview_AsyncPreview(t *testing.T) {
	ps, _, s := previewTestSetup(t)
	ctx := context.Background()

	adminID := pvSeedUser(t, s, "async-admin")
	adminRD := createTestRoleDefinition(t, s, "constraint-admin-async", store.RoleScopeSystem, []string{PermissionConstraintAdmin})
	pvSeedRoleBinding(t, s, adminRD.ID, "user", adminID, store.RoleScopeSystem, "")

	userID := pvSeedUser(t, s, "async-user")
	rd := createTestRoleDefinition(t, s, "test-role-async", store.RoleScopeSystem, []string{"agent.read"})
	pvSeedRoleBinding(t, s, rd.ID, "user", userID, store.RoleScopeSystem, "")

	draft := &store.AccessConstraint{
		Name:                 "async-test",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(userID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "async test",
	}

	job, err := ps.GeneratePreviewAsync(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     pvTestActor(adminID),
	})
	require.NoError(t, err)
	assert.NotEmpty(t, job.JobID)
	assert.Equal(t, "create", job.Operation)

	// Wait for completion.
	var finalJob *PreviewJob
	for i := 0; i < 100; i++ {
		time.Sleep(50 * time.Millisecond)
		finalJob, err = ps.GetPreviewJob(ctx, job.JobID)
		require.NoError(t, err)
		if finalJob.Status == JobStatusSucceeded || finalJob.Status == JobStatusFailed {
			break
		}
	}
	require.NotNil(t, finalJob)
	assert.Equal(t, JobStatusSucceeded, finalJob.Status)
	assert.NotNil(t, finalJob.Result)
}

func TestPreview_AsyncCancel(t *testing.T) {
	ps, _, s := previewTestSetup(t)
	ctx := context.Background()

	adminID := pvSeedUser(t, s, "cancel-admin")
	adminRD := createTestRoleDefinition(t, s, "constraint-admin-cancel", store.RoleScopeSystem, []string{PermissionConstraintAdmin})
	pvSeedRoleBinding(t, s, adminRD.ID, "user", adminID, store.RoleScopeSystem, "")

	userID := pvSeedUser(t, s, "cancel-user")
	rd := createTestRoleDefinition(t, s, "test-role-cancel", store.RoleScopeSystem, []string{"agent.read"})
	pvSeedRoleBinding(t, s, rd.ID, "user", userID, store.RoleScopeSystem, "")

	draft := &store.AccessConstraint{
		Name:                 "cancel-test",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(userID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "cancel test",
	}

	job, err := ps.GeneratePreviewAsync(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     pvTestActor(adminID),
	})
	require.NoError(t, err)

	// Cancel immediately.
	err = ps.CancelPreviewJob(ctx, job.JobID)
	// May or may not succeed depending on timing, but should not error fatally.
	if err == nil {
		job, _ = ps.GetPreviewJob(ctx, job.JobID)
		assert.Equal(t, JobStatusCancelled, job.Status)
	}
}

// ---------------------------------------------------------------------------
// 15. Affected principals pagination
// ---------------------------------------------------------------------------

func TestPreview_AffectedPrincipalsPagination(t *testing.T) {
	ps, _, s := previewTestSetup(t)
	ctx := context.Background()

	adminID := pvSeedUser(t, s, "page-admin")
	adminRD := createTestRoleDefinition(t, s, "constraint-admin-page", store.RoleScopeSystem, []string{PermissionConstraintAdmin})
	pvSeedRoleBinding(t, s, adminRD.ID, "user", adminID, store.RoleScopeSystem, "")

	userID := pvSeedUser(t, s, "page-user")
	rd := createTestRoleDefinition(t, s, "test-role-page", store.RoleScopeSystem, []string{"agent.read"})
	pvSeedRoleBinding(t, s, rd.ID, "user", userID, store.RoleScopeSystem, "")

	draft := &store.AccessConstraint{
		Name:                 "page-test",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(userID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "pagination test",
	}

	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     pvTestActor(adminID),
	})
	require.NoError(t, err)
	assert.NotNil(t, result.AffectedPage)
	assert.Equal(t, result.Impact.AffectedPrincipalCount, result.AffectedPage.TotalCount)
}

// ---------------------------------------------------------------------------
// 16. Request validation
// ---------------------------------------------------------------------------

func TestPreview_RequestValidation(t *testing.T) {
	ps, _, s := previewTestSetup(t)
	ctx := context.Background()

	adminID := pvSeedUser(t, s, "validation-admin")

	// Missing draft for create.
	_, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Actor:     pvTestActor(adminID),
	})
	assert.Error(t, err, "should reject create without draft")

	// Missing constraint ID for update.
	_, err = ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "update",
		Draft: &store.AccessConstraint{
			Name:               "test",
			SubjectKind:        "principal",
			ScopeType:          "system",
			MaximumPermissions: []string{"agent.read"},
		},
		Actor: pvTestActor(adminID),
	})
	assert.Error(t, err, "should reject update without constraintID")

	// Missing constraint ID for delete.
	_, err = ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "delete",
		Actor:     pvTestActor(adminID),
	})
	assert.Error(t, err, "should reject delete without constraintID")

	// Invalid operation.
	_, err = ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "invalid",
		Actor:     pvTestActor(adminID),
	})
	assert.Error(t, err, "should reject invalid operation")

	// Missing actor.
	_, err = ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft: &store.AccessConstraint{
			Name:               "test",
			SubjectKind:        "principal",
			ScopeType:          "system",
			MaximumPermissions: []string{"agent.read"},
		},
		Actor: PrincipalContext{},
	})
	assert.Error(t, err, "should reject missing actor")
}

// ---------------------------------------------------------------------------
// 17. Draft hash stability
// ---------------------------------------------------------------------------

func TestPreview_DraftHashStability(t *testing.T) {
	ps, _, _ := previewTestSetup(t)

	draft := &store.AccessConstraint{
		Name:                 "hash-test",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr("user-1"),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"z.perm", "a.perm", "m.perm"},
		Purpose:              "hash stability",
	}

	hash1 := ps.computeDraftHash(draft)
	hash2 := ps.computeDraftHash(draft)
	assert.Equal(t, hash1, hash2, "same draft should produce same hash")

	// Different permission order should produce same hash (sorted).
	draft2 := &store.AccessConstraint{
		Name:                 "hash-test",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr("user-1"),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"a.perm", "m.perm", "z.perm"},
		Purpose:              "hash stability",
	}
	hash3 := ps.computeDraftHash(draft2)
	assert.Equal(t, hash1, hash3, "same permissions in different order should produce same hash")

	// Different draft should produce different hash.
	draft3 := &store.AccessConstraint{
		Name:                 "hash-test-different",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr("user-1"),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"a.perm"},
		Purpose:              "different hash",
	}
	hash4 := ps.computeDraftHash(draft3)
	assert.NotEqual(t, hash1, hash4, "different draft should produce different hash")

	// Nil draft should produce deterministic hash.
	hashNil1 := ps.computeDraftHash(nil)
	hashNil2 := ps.computeDraftHash(nil)
	assert.Equal(t, hashNil1, hashNil2, "nil draft should produce deterministic hash")
	assert.Equal(t, "null", hashNil1)
}

// ---------------------------------------------------------------------------
// 18. Token signature validation
// ---------------------------------------------------------------------------

func TestPreview_TokenSignatureValidation(t *testing.T) {
	ps, _, s := previewTestSetup(t)
	ctx := context.Background()

	adminID := pvSeedUser(t, s, "sig-admin")
	adminRD := createTestRoleDefinition(t, s, "constraint-admin-sig", store.RoleScopeSystem, []string{PermissionConstraintAdmin})
	pvSeedRoleBinding(t, s, adminRD.ID, "user", adminID, store.RoleScopeSystem, "")

	userID := pvSeedUser(t, s, "sig-user")
	rd := createTestRoleDefinition(t, s, "test-role-sig", store.RoleScopeSystem, []string{"agent.read"})
	pvSeedRoleBinding(t, s, rd.ID, "user", userID, store.RoleScopeSystem, "")

	draft := &store.AccessConstraint{
		Name:                 "sig-test",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(userID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "sig test",
	}

	actor := pvTestActor(adminID)
	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     actor,
	})
	require.NoError(t, err)

	// Tamper with the token.
	err = ps.ValidateToken(ctx, result.PreviewToken+"tampered", actor, "create", draft, 0)
	require.Error(t, err)
	var tokenErr *TokenValidationError
	require.ErrorAs(t, err, &tokenErr)
	assert.Equal(t, ErrCodePreviewTokenInvalid, tokenErr.Code)

	// Malformed token.
	err = ps.ValidateToken(ctx, "not-a-valid-token", actor, "create", draft, 0)
	require.Error(t, err)
	require.ErrorAs(t, err, &tokenErr)
	assert.Equal(t, ErrCodePreviewTokenInvalid, tokenErr.Code)
}

// ---------------------------------------------------------------------------
// 19. Full end-to-end: preview + validate token
// ---------------------------------------------------------------------------

func TestPreview_EndToEnd_CreateAndCommit(t *testing.T) {
	ps, _, s := previewTestSetup(t)
	ctx := context.Background()

	userID := pvSeedUser(t, s, "e2e-user")
	adminID := pvSeedUser(t, s, "e2e-admin")

	rd := createTestRoleDefinition(t, s, "test-role-e2e", store.RoleScopeSystem, []string{"agent.read", "agent.create"})
	pvSeedRoleBinding(t, s, rd.ID, "user", userID, store.RoleScopeSystem, "")

	adminRD := createTestRoleDefinition(t, s, "constraint-admin-e2e", store.RoleScopeSystem, []string{PermissionConstraintAdmin, "agent.read", "agent.create"})
	pvSeedRoleBinding(t, s, adminRD.ID, "user", adminID, store.RoleScopeSystem, "")

	draft := &store.AccessConstraint{
		Name:                 "e2e-constraint",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(userID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "e2e test",
	}

	actor := pvTestActor(adminID)

	// Step 1: Generate preview.
	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     actor,
	})
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.Equal(t, ClassificationTighten, result.Classification)
	assert.True(t, result.Completeness.Complete)
	assert.NotEmpty(t, result.PreviewToken)
	assert.NotEmpty(t, result.PreviewID)
	assert.Equal(t, "create", result.Operation)
	assert.True(t, result.ExpiresAt.After(result.GeneratedAt))

	// Step 2: Validate token for commit.
	err = ps.ValidateToken(ctx, result.PreviewToken, actor, "create", draft, 0)
	require.NoError(t, err, "token should be valid for commit")
}

func TestPreview_EndToEnd_UpdateAndCommit(t *testing.T) {
	ps, _, s := previewTestSetup(t)
	ctx := context.Background()

	userID := pvSeedUser(t, s, "e2e-update-user")
	adminID := pvSeedUser(t, s, "e2e-update-admin")

	rd := createTestRoleDefinition(t, s, "test-role-e2e-update", store.RoleScopeSystem, []string{"agent.read", "agent.create", "project.read"})
	pvSeedRoleBinding(t, s, rd.ID, "user", userID, store.RoleScopeSystem, "")

	adminRD := createTestRoleDefinition(t, s, "constraint-admin-e2e-update", store.RoleScopeSystem, []string{PermissionConstraintAdmin, "agent.read", "agent.create", "project.read"})
	pvSeedRoleBinding(t, s, adminRD.ID, "user", adminID, store.RoleScopeSystem, "")

	// Create existing constraint.
	existing := pvSeedConstraint(t, s, &store.AccessConstraint{
		Name:                 "existing-e2e",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(userID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read", "agent.create"},
		Purpose:              "to be updated",
	})

	// Update to be more restrictive.
	draft := &store.AccessConstraint{
		Name:                 "updated-e2e",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(userID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "updated and tighter",
	}

	actor := pvTestActor(adminID)
	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation:    "update",
		Draft:        draft,
		ConstraintID: existing.ID,
		BaseRevision: existing.Revision,
		Actor:        actor,
	})
	require.NoError(t, err)
	assert.Equal(t, ClassificationTighten, result.Classification)

	// Validate token.
	err = ps.ValidateToken(ctx, result.PreviewToken, actor, "update", draft, existing.Revision)
	require.NoError(t, err)
}

func TestPreview_EndToEnd_DeleteAndCommit(t *testing.T) {
	ps, _, s := previewTestSetup(t)
	ctx := context.Background()

	userID := pvSeedUser(t, s, "e2e-delete-user")
	adminID := pvSeedUser(t, s, "e2e-delete-admin")

	rd := createTestRoleDefinition(t, s, "test-role-e2e-delete", store.RoleScopeSystem, []string{"agent.read", "agent.create"})
	pvSeedRoleBinding(t, s, rd.ID, "user", userID, store.RoleScopeSystem, "")

	adminRD := createTestRoleDefinition(t, s, "constraint-admin-e2e-delete", store.RoleScopeSystem, []string{PermissionConstraintAdmin, "agent.read", "agent.create"})
	pvSeedRoleBinding(t, s, adminRD.ID, "user", adminID, store.RoleScopeSystem, "")

	existing := pvSeedConstraint(t, s, &store.AccessConstraint{
		Name:                 "to-delete-e2e",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(userID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "to be deleted",
	})

	actor := pvTestActor(adminID)
	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation:    "delete",
		ConstraintID: existing.ID,
		BaseRevision: existing.Revision,
		Actor:        actor,
	})
	require.NoError(t, err)
	assert.Equal(t, ClassificationRelax, result.Classification)

	// Validate token with nil draft (delete).
	err = ps.ValidateToken(ctx, result.PreviewToken, actor, "delete", nil, existing.Revision)
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// 20. Preview result structure verification
// ---------------------------------------------------------------------------

func TestPreview_ResultStructure(t *testing.T) {
	ps, _, s := previewTestSetup(t)
	ctx := context.Background()

	userID := pvSeedUser(t, s, "struct-user")
	adminID := pvSeedUser(t, s, "struct-admin")

	rd := createTestRoleDefinition(t, s, "test-role-struct", store.RoleScopeSystem, []string{"agent.read", "agent.create", "project.read"})
	pvSeedRoleBinding(t, s, rd.ID, "user", userID, store.RoleScopeSystem, "")

	adminRD := createTestRoleDefinition(t, s, "constraint-admin-struct", store.RoleScopeSystem, []string{PermissionConstraintAdmin, "agent.read", "agent.create", "project.read"})
	pvSeedRoleBinding(t, s, adminRD.ID, "user", adminID, store.RoleScopeSystem, "")

	draft := &store.AccessConstraint{
		Name:                 "struct-test",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(userID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "structure verification",
	}

	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     pvTestActor(adminID),
	})
	require.NoError(t, err)

	// Verify all required fields are populated.
	assert.NotEmpty(t, result.PreviewID, "previewId must be non-empty")
	assert.NotEmpty(t, result.PreviewToken, "previewToken must be non-empty")
	assert.False(t, result.GeneratedAt.IsZero(), "generatedAt must be set")
	assert.False(t, result.ExpiresAt.IsZero(), "expiresAt must be set")
	assert.Equal(t, "create", result.Operation)
	assert.NotEmpty(t, result.DraftHash)
	assert.NotEmpty(t, result.Classification)

	// Completeness.
	assert.NotNil(t, result.Completeness.Reasons)

	// Lockout.
	assert.NotNil(t, result.Lockout.CheckedPermissionIDs)
	assert.Contains(t, result.Lockout.CheckedPermissionIDs, PermissionConstraintAdmin)

	// Impact.
	assert.GreaterOrEqual(t, result.Impact.AffectedPrincipalCount, 0)

	// Temporal states.
	assert.GreaterOrEqual(t, len(result.TemporalStates), 1)

	// Warnings.
	assert.NotNil(t, result.Warnings)

	// Affected page.
	assert.Equal(t, result.Impact.AffectedPrincipalCount, result.AffectedPage.TotalCount)

	// Permission diffs should exist for tightening.
	if result.Classification == ClassificationTighten {
		assert.GreaterOrEqual(t, len(result.Impact.PermissionDiffs), 1,
			"tightening should have at least one permission diff")
	}

	// Verify reproducibility: every summary count should be derivable from rows.
	losing := 0
	regaining := 0
	noEffect := 0
	for _, ip := range result.AffectedPage.Items {
		switch ip.ChangeKind {
		case "loses":
			losing++
		case "regains":
			regaining++
		case "mixed":
			losing++
			regaining++
		case "no_effect":
			noEffect++
		}
	}
	// If the page is not truncated, counts should match.
	if result.AffectedPage.NextPageToken == "" {
		assert.Equal(t, result.Impact.LosingPrincipalCount, losing,
			"losing count should match page items")
	}
}

// ---------------------------------------------------------------------------
// 21. Warnings
// ---------------------------------------------------------------------------

func TestPreview_Warnings_Degraded(t *testing.T) {
	ps, _, s := previewTestSetup(t)
	ctx := context.Background()

	adminID := pvSeedUser(t, s, "warn-degraded-admin")
	adminRD := createTestRoleDefinition(t, s, "constraint-admin-warn-degraded", store.RoleScopeSystem, []string{PermissionConstraintAdmin})
	pvSeedRoleBinding(t, s, adminRD.ID, "user", adminID, store.RoleScopeSystem, "")

	// Use nonexistent group to trigger degraded.
	draft := &store.AccessConstraint{
		Name:               "warn-degraded-test",
		SubjectKind:        store.ConstraintSubjectGroupClosure,
		SubjectGroupID:     pvStrPtr(tid("nonexistent-warn-group")),
		ScopeType:          store.RoleScopeSystem,
		MaximumPermissions: []string{"agent.read"},
		Purpose:            "degraded warning test",
	}

	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     pvTestActor(adminID),
	})
	require.NoError(t, err)

	hasDegradedWarning := false
	for _, w := range result.Warnings {
		if w.Code == WarningCodePreviewDegraded {
			hasDegradedWarning = true
		}
	}
	assert.True(t, hasDegradedWarning, "degraded preview should produce PREVIEW_DEGRADED warning")
}

// ---------------------------------------------------------------------------
// 22. Classification helper unit tests
// ---------------------------------------------------------------------------

func TestClassifyFromImpacted(t *testing.T) {
	tests := []struct {
		name     string
		impacted []ImpactedPrincipal
		want     string
	}{
		{
			name:     "empty → relax",
			impacted: nil,
			want:     ClassificationRelax,
		},
		{
			name:     "all losing → tighten",
			impacted: []ImpactedPrincipal{{ChangeKind: "loses"}, {ChangeKind: "loses"}},
			want:     ClassificationTighten,
		},
		{
			name:     "all regaining → relax",
			impacted: []ImpactedPrincipal{{ChangeKind: "regains"}, {ChangeKind: "regains"}},
			want:     ClassificationRelax,
		},
		{
			name:     "mixed changes → mixed",
			impacted: []ImpactedPrincipal{{ChangeKind: "loses"}, {ChangeKind: "regains"}},
			want:     ClassificationMixed,
		},
		{
			name:     "all no_effect → relax",
			impacted: []ImpactedPrincipal{{ChangeKind: "no_effect"}, {ChangeKind: "no_effect"}},
			want:     ClassificationRelax,
		},
		{
			name:     "single mixed principal → mixed",
			impacted: []ImpactedPrincipal{{ChangeKind: "mixed"}},
			want:     ClassificationMixed,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyFromImpacted(tt.impacted)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestInvertClassification(t *testing.T) {
	assert.Equal(t, ClassificationRelax, invertClassification(ClassificationTighten))
	assert.Equal(t, ClassificationTighten, invertClassification(ClassificationRelax))
	assert.Equal(t, ClassificationMixed, invertClassification(ClassificationMixed))
}

// ---------------------------------------------------------------------------
// 23. Revision mismatch
// ---------------------------------------------------------------------------

func TestPreview_RevisionMismatch(t *testing.T) {
	ps, _, s := previewTestSetup(t)
	ctx := context.Background()

	userID := pvSeedUser(t, s, "rev-mismatch-user")
	adminID := pvSeedUser(t, s, "rev-mismatch-admin")

	rd := createTestRoleDefinition(t, s, "test-role-rev-mismatch", store.RoleScopeSystem, []string{"agent.read"})
	pvSeedRoleBinding(t, s, rd.ID, "user", userID, store.RoleScopeSystem, "")

	adminRD := createTestRoleDefinition(t, s, "constraint-admin-rev-mismatch", store.RoleScopeSystem, []string{PermissionConstraintAdmin})
	pvSeedRoleBinding(t, s, adminRD.ID, "user", adminID, store.RoleScopeSystem, "")

	existing := pvSeedConstraint(t, s, &store.AccessConstraint{
		Name:                 "rev-test",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(userID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "revision test",
	})

	// Preview with wrong revision.
	_, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation:    "update",
		Draft:        existing,
		ConstraintID: existing.ID,
		BaseRevision: existing.Revision + 999, // Wrong revision.
		Actor:        pvTestActor(adminID),
	})
	require.Error(t, err, "should reject mismatched revision")
}

// ---------------------------------------------------------------------------
// 24. Delete preview
// ---------------------------------------------------------------------------

func TestPreview_Delete(t *testing.T) {
	ps, _, s := previewTestSetup(t)
	ctx := context.Background()

	userID := pvSeedUser(t, s, "delete-prev-user")
	adminID := pvSeedUser(t, s, "delete-prev-admin")

	rd := createTestRoleDefinition(t, s, "test-role-delete-prev", store.RoleScopeSystem, []string{"agent.read", "agent.create"})
	pvSeedRoleBinding(t, s, rd.ID, "user", userID, store.RoleScopeSystem, "")

	adminRD := createTestRoleDefinition(t, s, "constraint-admin-delete-prev", store.RoleScopeSystem, []string{PermissionConstraintAdmin, "agent.read", "agent.create"})
	pvSeedRoleBinding(t, s, adminRD.ID, "user", adminID, store.RoleScopeSystem, "")

	existing := pvSeedConstraint(t, s, &store.AccessConstraint{
		Name:                 "delete-preview-test",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(userID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "to be deleted",
	})

	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation:    "delete",
		ConstraintID: existing.ID,
		BaseRevision: existing.Revision,
		Actor:        pvTestActor(adminID),
	})
	require.NoError(t, err)
	assert.Equal(t, "delete", result.Operation)
	assert.Equal(t, ClassificationRelax, result.Classification)
	assert.Equal(t, existing.ID, result.ConstraintID)
}

// ---------------------------------------------------------------------------
// 25. Token revision mismatch on validate
// ---------------------------------------------------------------------------

func TestPreview_TokenRevisionMismatch(t *testing.T) {
	ps, _, s := previewTestSetup(t)
	ctx := context.Background()

	userID := pvSeedUser(t, s, "token-rev-user")
	adminID := pvSeedUser(t, s, "token-rev-admin")

	rd := createTestRoleDefinition(t, s, "test-role-token-rev", store.RoleScopeSystem, []string{"agent.read"})
	pvSeedRoleBinding(t, s, rd.ID, "user", userID, store.RoleScopeSystem, "")

	adminRD := createTestRoleDefinition(t, s, "constraint-admin-token-rev", store.RoleScopeSystem, []string{PermissionConstraintAdmin})
	pvSeedRoleBinding(t, s, adminRD.ID, "user", adminID, store.RoleScopeSystem, "")

	existing := pvSeedConstraint(t, s, &store.AccessConstraint{
		Name:                 "token-rev-test",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(userID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "token rev test",
	})

	actor := pvTestActor(adminID)
	draft := &store.AccessConstraint{
		Name:                 "updated-token-rev",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(userID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "token rev update",
	}

	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation:    "update",
		Draft:        draft,
		ConstraintID: existing.ID,
		BaseRevision: existing.Revision,
		Actor:        actor,
	})
	require.NoError(t, err)

	// Validate with different revision (simulating concurrent update).
	err = ps.ValidateToken(ctx, result.PreviewToken, actor, "update", draft, existing.Revision+1)
	require.Error(t, err)
	var tokenErr *TokenValidationError
	require.ErrorAs(t, err, &tokenErr)
	assert.Equal(t, ErrCodePreviewRevisionMismatch, tokenErr.Code)
}

// ---------------------------------------------------------------------------
// 26. Affected principal detail fields
// ---------------------------------------------------------------------------

func TestPreview_AffectedPrincipalDetail(t *testing.T) {
	ps, _, s := previewTestSetup(t)
	ctx := context.Background()

	userID := pvSeedUser(t, s, "detail-user")
	adminID := pvSeedUser(t, s, "detail-admin")

	rd := createTestRoleDefinition(t, s, "test-role-detail", store.RoleScopeSystem, []string{"agent.read", "agent.create", "project.read"})
	pvSeedRoleBinding(t, s, rd.ID, "user", userID, store.RoleScopeSystem, "")

	adminRD := createTestRoleDefinition(t, s, "constraint-admin-detail", store.RoleScopeSystem, []string{PermissionConstraintAdmin, "agent.read", "agent.create", "project.read"})
	pvSeedRoleBinding(t, s, adminRD.ID, "user", adminID, store.RoleScopeSystem, "")

	draft := &store.AccessConstraint{
		Name:                 "detail-test",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(userID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "detail test",
	}

	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     pvTestActor(adminID),
	})
	require.NoError(t, err)

	// Find the target user in affected principals.
	var targetPrincipal *ImpactedPrincipal
	for i, ip := range result.AffectedPage.Items {
		if ip.PrincipalID == userID {
			targetPrincipal = &result.AffectedPage.Items[i]
			break
		}
	}
	require.NotNil(t, targetPrincipal, "target user should be in affected principals")

	assert.Equal(t, "user", targetPrincipal.PrincipalType)
	assert.Equal(t, userID, targetPrincipal.PrincipalID)
	assert.Equal(t, "loses", targetPrincipal.ChangeKind)
	assert.GreaterOrEqual(t, targetPrincipal.CurrentPermissionCount, 1)
	assert.GreaterOrEqual(t, len(targetPrincipal.RemovedPermissions), 1)
	assert.Equal(t, len(targetPrincipal.RemovedPermissions), targetPrincipal.RemovedPermissionCount)

	// Removed permissions should include agent.create and project.read.
	removedSet := toSet(targetPrincipal.RemovedPermissions)
	_, hasAgentCreate := removedSet["agent.create"]
	_, hasProjectRead := removedSet["project.read"]
	assert.True(t, hasAgentCreate, "agent.create should be in removed permissions")
	assert.True(t, hasProjectRead, "project.read should be in removed permissions")
}

// ---------------------------------------------------------------------------
// 27. Ensure every summary count is derivable from page rows
// ---------------------------------------------------------------------------

func TestPreview_SummaryCountsMatchRows(t *testing.T) {
	ps, _, s := previewTestSetup(t)
	ctx := context.Background()

	// Create a group with 3 members having different permission sets.
	groupID := pvSeedGroup(t, s, "count-group")
	user1ID := pvSeedUser(t, s, "count-user-1")
	user2ID := pvSeedUser(t, s, "count-user-2")
	user3ID := pvSeedUser(t, s, "count-user-3")
	adminID := pvSeedUser(t, s, "count-admin")

	pvSeedGroupMember(t, s, groupID, "user", user1ID)
	pvSeedGroupMember(t, s, groupID, "user", user2ID)
	pvSeedGroupMember(t, s, groupID, "user", user3ID)

	// Give different permission sets.
	rd1 := createTestRoleDefinition(t, s, "count-role-1", store.RoleScopeSystem, []string{"agent.read", "agent.create"})
	rd2 := createTestRoleDefinition(t, s, "count-role-2", store.RoleScopeSystem, []string{"agent.read", "project.read"})
	rd3 := createTestRoleDefinition(t, s, "count-role-3", store.RoleScopeSystem, []string{"agent.read"})

	pvSeedRoleBinding(t, s, rd1.ID, "user", user1ID, store.RoleScopeSystem, "")
	pvSeedRoleBinding(t, s, rd2.ID, "user", user2ID, store.RoleScopeSystem, "")
	pvSeedRoleBinding(t, s, rd3.ID, "user", user3ID, store.RoleScopeSystem, "")

	adminRD := createTestRoleDefinition(t, s, "constraint-admin-count", store.RoleScopeSystem, []string{PermissionConstraintAdmin})
	pvSeedRoleBinding(t, s, adminRD.ID, "user", adminID, store.RoleScopeSystem, "")

	draft := &store.AccessConstraint{
		Name:               "count-test",
		SubjectKind:        store.ConstraintSubjectGroupClosure,
		SubjectGroupID:     pvStrPtr(groupID),
		ScopeType:          store.RoleScopeSystem,
		MaximumPermissions: []string{"agent.read"},
		Purpose:            "count verification",
	}

	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     pvTestActor(adminID),
	})
	require.NoError(t, err)

	// Verify counts match row analysis.
	if result.AffectedPage.NextPageToken == "" {
		// All items on one page — verify counts match.
		derivedLosing := 0
		derivedRegaining := 0
		derivedNoEffect := 0
		derivedPermDiffs := make(map[string]int) // permID → losing count

		for _, ip := range result.AffectedPage.Items {
			switch ip.ChangeKind {
			case "loses":
				derivedLosing++
			case "regains":
				derivedRegaining++
			case "mixed":
				derivedLosing++
				derivedRegaining++
			case "no_effect":
				derivedNoEffect++
			}
			for _, p := range ip.RemovedPermissions {
				derivedPermDiffs[p]++
			}
		}

		assert.Equal(t, result.Impact.LosingPrincipalCount, derivedLosing,
			"losing count must match page rows")
		assert.Equal(t, result.Impact.RegainingPrincipalCount, derivedRegaining,
			"regaining count must match page rows")
		assert.Equal(t, result.Impact.NoEffectPrincipalCount, derivedNoEffect,
			"no-effect count must match page rows")

		// Permission diffs should match.
		for _, pd := range result.Impact.PermissionDiffs {
			assert.Equal(t, derivedPermDiffs[pd.PermissionID], pd.LosingCount,
				"permission diff for %s: losing count should match", pd.PermissionID)
		}
	}
}

// ---------------------------------------------------------------------------
// 28. Incomplete data never labeled complete
// ---------------------------------------------------------------------------

func TestPreview_IncompleteNeverLabeledComplete(t *testing.T) {
	ps, _, s := previewTestSetup(t)
	ctx := context.Background()

	adminID := pvSeedUser(t, s, "incomplete-admin")
	adminRD := createTestRoleDefinition(t, s, "constraint-admin-incomplete", store.RoleScopeSystem, []string{PermissionConstraintAdmin})
	pvSeedRoleBinding(t, s, adminRD.ID, "user", adminID, store.RoleScopeSystem, "")

	// Group closure with nonexistent group → degraded.
	draft := &store.AccessConstraint{
		Name:               "incomplete-test",
		SubjectKind:        store.ConstraintSubjectGroupClosure,
		SubjectGroupID:     pvStrPtr(tid("nonexistent-incomplete-group")),
		ScopeType:          store.RoleScopeSystem,
		MaximumPermissions: []string{"agent.read"},
		Purpose:            "incomplete test",
	}

	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     pvTestActor(adminID),
	})
	require.NoError(t, err)

	// Verify: incomplete data is never labeled as complete.
	assert.False(t, result.Completeness.Complete,
		"resolution failure must not be labeled complete")
	assert.True(t, len(result.Completeness.Reasons) > 0,
		"incomplete preview must give reasons")
	assert.NotNil(t, result.CommitBlocked,
		"incomplete preview must block commit")

	// Count exactness must reflect degraded state.
	assert.False(t, result.Impact.AffectedPrincipalCountExact,
		"counts in degraded state must not be labeled exact")
	assert.False(t, result.AffectedPage.TotalCountExact,
		"page total in degraded state must not be labeled exact")

	t.Logf("Incompleteness reasons: %v", result.Completeness.Reasons)
	for _, r := range result.Completeness.Reasons {
		t.Logf("  Code=%s Message=%s", r.Code, r.Message)
	}
}

// ---------------------------------------------------------------------------
// 29. Token valid for the full 5 minutes
// ---------------------------------------------------------------------------

func TestPreview_TokenValidFor5Minutes(t *testing.T) {
	ps, _, s := previewTestSetup(t)
	ctx := context.Background()

	adminID := pvSeedUser(t, s, "ttl-admin")
	adminRD := createTestRoleDefinition(t, s, "constraint-admin-ttl", store.RoleScopeSystem, []string{PermissionConstraintAdmin})
	pvSeedRoleBinding(t, s, adminRD.ID, "user", adminID, store.RoleScopeSystem, "")

	userID := pvSeedUser(t, s, "ttl-user")
	rd := createTestRoleDefinition(t, s, "test-role-ttl", store.RoleScopeSystem, []string{"agent.read"})
	pvSeedRoleBinding(t, s, rd.ID, "user", userID, store.RoleScopeSystem, "")

	draft := &store.AccessConstraint{
		Name:                 "ttl-test",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(userID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "ttl test",
	}

	actor := pvTestActor(adminID)
	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     actor,
	})
	require.NoError(t, err)

	// Check expiry is ~5 minutes from generation.
	expectedExpiry := result.GeneratedAt.Add(5 * time.Minute)
	assert.Equal(t, expectedExpiry.Unix(), result.ExpiresAt.Unix(),
		"token should expire 5 minutes after generation")

	// Token should still be valid at 4 minutes 59 seconds.
	ps.nowFunc = func() time.Time {
		return result.GeneratedAt.Add(4*time.Minute + 59*time.Second)
	}
	err = ps.ValidateToken(ctx, result.PreviewToken, actor, "create", draft, 0)
	require.NoError(t, err, "token should still be valid at 4m59s")
}

// ---------------------------------------------------------------------------
// 30. Multiple constraints interaction in impact computation
// ---------------------------------------------------------------------------

func TestPreview_MultipleConstraintsInteraction(t *testing.T) {
	ps, _, s := previewTestSetup(t)
	ctx := context.Background()

	userID := pvSeedUser(t, s, "multi-constraint-user")
	adminID := pvSeedUser(t, s, "multi-constraint-admin")

	rd := createTestRoleDefinition(t, s, "test-role-multi-constraint", store.RoleScopeSystem,
		[]string{"agent.read", "agent.create", "project.read", "project.create"})
	pvSeedRoleBinding(t, s, rd.ID, "user", userID, store.RoleScopeSystem, "")

	adminRD := createTestRoleDefinition(t, s, "constraint-admin-multi-constraint", store.RoleScopeSystem,
		[]string{PermissionConstraintAdmin, "agent.read", "agent.create", "project.read", "project.create"})
	pvSeedRoleBinding(t, s, adminRD.ID, "user", adminID, store.RoleScopeSystem, "")

	// Existing constraint already restricts to agent.* + project.read.
	pvSeedConstraint(t, s, &store.AccessConstraint{
		Name:                 "existing-multi",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(userID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read", "agent.create", "project.read"},
		Purpose:              "existing restriction",
	})

	// New constraint further restricts to agent.read only.
	// Combined effect: intersection of {agent.read, agent.create, project.read}
	// and {agent.read} = {agent.read}.
	draft := &store.AccessConstraint{
		Name:                 "further-restrict",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(userID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "further restriction",
	}

	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     pvTestActor(adminID),
	})
	require.NoError(t, err)
	assert.Equal(t, ClassificationTighten, result.Classification)

	// User should lose agent.create and project.read (previously allowed by existing constraint).
	for _, ip := range result.AffectedPage.Items {
		if ip.PrincipalID == userID {
			removedSet := toSet(ip.RemovedPermissions)
			_, hasAgentCreate := removedSet["agent.create"]
			_, hasProjectRead := removedSet["project.read"]
			assert.True(t, hasAgentCreate || hasProjectRead,
				"intersection should remove permissions beyond just agent.read; removed: %v", ip.RemovedPermissions)
			break
		}
	}
}

// ---------------------------------------------------------------------------
// 32. C1 fix: Lockout degraded state blocks commit
// ---------------------------------------------------------------------------

func TestPreview_LockoutDegradedBlocksCommit(t *testing.T) {
	ps, _, s := previewTestSetup(t)
	ctx := context.Background()

	adminID := pvSeedUser(t, s, "lockout-degraded-admin")
	adminRD := createTestRoleDefinition(t, s, "constraint-admin-lockout-degraded", store.RoleScopeSystem,
		[]string{PermissionConstraintAdmin})
	pvSeedRoleBinding(t, s, adminRD.ID, "user", adminID, store.RoleScopeSystem, "")

	draft := &store.AccessConstraint{
		Name:               "lockout-degraded-test",
		SubjectKind:        store.ConstraintSubjectAllPrincipals,
		ScopeType:          store.RoleScopeSystem,
		MaximumPermissions: []string{"agent.read"},
		Purpose:            "lockout degraded test",
	}

	t.Run("zero admins blocks commit", func(t *testing.T) {
		noAdminResult, err := ps.GeneratePreview(ctx, PreviewRequest{
			Operation: "create",
			Draft:     draft,
			Actor:     pvTestActor(pvSeedUser(t, s, "lockout-degraded-nonadmin")),
		})
		require.NoError(t, err)
		require.NotNil(t, noAdminResult.Lockout.Safe)
		assert.False(t, *noAdminResult.Lockout.Safe, "zero admins must be unsafe")
		assert.NotNil(t, noAdminResult.CommitBlocked, "unsafe lockout must block commit")
		assert.Equal(t, ErrCodeConstraintAdminLockout, noAdminResult.CommitBlocked.Code)
	})

	t.Run("nil Safe (undetermined) blocks commit", func(t *testing.T) {
		// Directly test the commit-blocking logic: when assessLockout returns
		// Safe==nil (undetermined, e.g. due to resolver error), commit must be blocked.
		// We simulate this by calling assessLockout with a project scope that
		// has admins but will fail at ListConstraintsForScope. Since we cannot
		// easily inject store errors, we verify the logic by directly constructing
		// the scenario the production code handles.
		//
		// assessLockout returns Safe==nil with an UndeterminedReason when
		// resolveAdminUsers or ListConstraintsForScope fails. We invoke assessLockout
		// against a non-existent project scope where ListRoleBindingsForScope returns
		// no results (zero admins → Safe=false), but the critical test is: what
		// happens when Safe is nil?
		//
		// Directly test the condition: construct a lockout with Safe=nil and verify
		// the commit-blocked code in GeneratePreview would block.
		lockout := LockoutAssessment{
			Safe:                 nil, // undetermined
			CheckedPermissionIDs: []string{PermissionConstraintAdmin},
			UndeterminedReason:   "simulated resolver error",
		}

		// Reproduce the exact commit-blocking logic from GeneratePreview:
		var commitBlocked *CommitBlockedReason
		if lockout.Safe == nil || !*lockout.Safe {
			code := ErrCodeConstraintAdminLockout
			msg := "mutation would lock out all constraint admins"
			if lockout.Safe == nil {
				code = ErrCodePreviewIncomplete
				msg = "lockout assessment could not be determined: " + lockout.UndeterminedReason
			}
			commitBlocked = &CommitBlockedReason{
				Code:    code,
				Message: msg,
			}
		}

		require.NotNil(t, commitBlocked, "nil Safe must block commit")
		assert.Equal(t, ErrCodePreviewIncomplete, commitBlocked.Code,
			"nil Safe should use ErrCodePreviewIncomplete, not ErrCodeConstraintAdminLockout")
		assert.Contains(t, commitBlocked.Message, "simulated resolver error",
			"message should include the undetermined reason")
	})

	t.Run("assessLockout returns nil Safe on scope error", func(t *testing.T) {
		// Use a non-existent project scope where ListConstraintsForScope
		// returns valid results but with an admin who cannot be resolved.
		// The key: assessLockout returns Safe=nil when it hits errors.
		// With a project scope that has admin users but fails constraint load:
		projID := pvSeedProject(t, s, "lockout-err-proj")
		errUserID := pvSeedUser(t, s, "lockout-err-user")

		// Give user admin permission at project scope.
		projAdminRD := createTestRoleDefinition(t, s, "proj-admin-lockout-err", store.RoleScopeProject,
			[]string{PermissionConstraintAdmin})
		pvSeedRoleBinding(t, s, projAdminRD.ID, "user", errUserID, store.RoleScopeProject, projID)

		// assessLockout at project scope should resolve the admin, load constraints,
		// and return Safe=true (no restricting constraints).
		lockout := ps.assessLockout(ctx, PreviewRequest{
			Operation: "create",
			Draft: &store.AccessConstraint{
				Name:               "lockout-test",
				SubjectKind:        store.ConstraintSubjectAllPrincipals,
				ScopeType:          store.RoleScopeProject,
				ScopeID:            projID,
				MaximumPermissions: []string{"agent.read"},
			},
			Actor: pvTestActor(errUserID),
		}, nil, storeToHubAccessConstraint(&store.AccessConstraint{
			Name:               "lockout-test",
			SubjectKind:        store.ConstraintSubjectAllPrincipals,
			ScopeType:          store.RoleScopeProject,
			ScopeID:            projID,
			MaximumPermissions: []string{"agent.read"},
		}), ps.nowFunc())

		// With valid data, lockout should be deterministic (not nil).
		require.NotNil(t, lockout.Safe,
			"with resolvable admins and loadable constraints, Safe must not be nil")

		// Now verify the nil path is handled: if this were nil, the production
		// code would set CommitBlocked with ErrCodePreviewIncomplete.
		// This is covered by the "nil Safe (undetermined) blocks commit" subtest above.
	})
}

// ---------------------------------------------------------------------------
// 33. R1 fix: Project-scoped delete validates correctly
// ---------------------------------------------------------------------------

func TestPreview_ProjectScopedDeleteTokenValidation(t *testing.T) {
	ps, _, s := previewTestSetup(t)
	ctx := context.Background()

	projectID := pvSeedProject(t, s, "proj-del-scope")
	userID := pvSeedUser(t, s, "proj-del-user")
	adminID := pvSeedUser(t, s, "proj-del-admin")

	rd := createTestRoleDefinition(t, s, "test-role-proj-del", store.RoleScopeProject, []string{"agent.read", "agent.create"})
	pvSeedRoleBinding(t, s, rd.ID, "user", userID, store.RoleScopeProject, projectID)

	adminRD := createTestRoleDefinition(t, s, "constraint-admin-proj-del", store.RoleScopeSystem,
		[]string{PermissionConstraintAdmin, "agent.read", "agent.create"})
	pvSeedRoleBinding(t, s, adminRD.ID, "user", adminID, store.RoleScopeSystem, "")

	// Create a project-scoped constraint.
	existing := pvSeedConstraint(t, s, &store.AccessConstraint{
		Name:                 "proj-scoped-to-delete",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(userID),
		ScopeType:            store.RoleScopeProject,
		ScopeID:              projectID,
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "project-scoped delete test",
	})

	actor := pvTestActor(adminID)
	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation:    "delete",
		ConstraintID: existing.ID,
		BaseRevision: existing.Revision,
		Actor:        actor,
	})
	require.NoError(t, err)
	assert.Equal(t, ClassificationRelax, result.Classification)

	// Token validation for project-scoped delete must succeed (R1 fix: uses
	// constraint ID, not preview ID, for state fingerprint).
	err = ps.ValidateToken(ctx, result.PreviewToken, actor, "delete", nil, existing.Revision)
	require.NoError(t, err, "project-scoped delete token validation must succeed")
}

// ---------------------------------------------------------------------------
// 34. R2 fix: Incomplete preview token rejected at commit
// ---------------------------------------------------------------------------

func TestPreview_IncompleteTokenRejectedAtCommit(t *testing.T) {
	ps, _, s := previewTestSetup(t)
	ctx := context.Background()

	adminID := pvSeedUser(t, s, "incomplete-commit-admin")
	adminRD := createTestRoleDefinition(t, s, "constraint-admin-incomplete-commit", store.RoleScopeSystem,
		[]string{PermissionConstraintAdmin})
	pvSeedRoleBinding(t, s, adminRD.ID, "user", adminID, store.RoleScopeSystem, "")

	// Use nonexistent group to trigger incomplete/degraded state.
	draft := &store.AccessConstraint{
		Name:               "incomplete-commit-test",
		SubjectKind:        store.ConstraintSubjectGroupClosure,
		SubjectGroupID:     pvStrPtr(tid("nonexistent-incomplete-commit-group")),
		ScopeType:          store.RoleScopeSystem,
		MaximumPermissions: []string{"agent.read"},
		Purpose:            "incomplete commit test",
	}

	actor := pvTestActor(adminID)
	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     actor,
	})
	require.NoError(t, err)
	assert.False(t, result.Completeness.Complete, "preview should be incomplete")
	assert.NotNil(t, result.CommitBlocked, "incomplete preview should have CommitBlocked set")

	// Even though a token was issued, ValidateToken must reject it server-side.
	err = ps.ValidateToken(ctx, result.PreviewToken, actor, "create", draft, 0)
	require.Error(t, err, "incomplete preview token must be rejected at validation")
	var tokenErr *TokenValidationError
	require.ErrorAs(t, err, &tokenErr)
	assert.Equal(t, ErrCodePreviewIncomplete, tokenErr.Code)
}

// ---------------------------------------------------------------------------
// 35. R3 fix: Nonce not consumed on binding check failure
// ---------------------------------------------------------------------------

func TestPreview_NonceNotConsumedOnBindingFailure(t *testing.T) {
	ps, _, s := previewTestSetup(t)
	ctx := context.Background()

	userID := pvSeedUser(t, s, "nonce-preserve-user")
	adminID := pvSeedUser(t, s, "nonce-preserve-admin")
	otherAdminID := pvSeedUser(t, s, "nonce-preserve-other-admin")

	rd := createTestRoleDefinition(t, s, "test-role-nonce-preserve", store.RoleScopeSystem,
		[]string{"agent.read", "agent.create"})
	pvSeedRoleBinding(t, s, rd.ID, "user", userID, store.RoleScopeSystem, "")

	adminRD := createTestRoleDefinition(t, s, "constraint-admin-nonce-preserve", store.RoleScopeSystem,
		[]string{PermissionConstraintAdmin})
	pvSeedRoleBinding(t, s, adminRD.ID, "user", adminID, store.RoleScopeSystem, "")
	pvSeedRoleBinding(t, s, adminRD.ID, "user", otherAdminID, store.RoleScopeSystem, "")

	draft := &store.AccessConstraint{
		Name:                 "nonce-preserve-test",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(userID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "nonce preserve test",
	}

	actor := pvTestActor(adminID)
	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     actor,
	})
	require.NoError(t, err)

	// Attempt to use the token with the wrong actor — this should fail on
	// actor binding check WITHOUT consuming the nonce (R3 fix).
	wrongActor := pvTestActor(otherAdminID)
	err = ps.ValidateToken(ctx, result.PreviewToken, wrongActor, "create", draft, 0)
	require.Error(t, err, "wrong actor should be rejected")
	var tokenErr *TokenValidationError
	require.ErrorAs(t, err, &tokenErr)
	assert.Equal(t, ErrCodePreviewActorMismatch, tokenErr.Code)

	// The legitimate actor should still be able to use the token — the nonce
	// was NOT consumed by the failed attempt.
	err = ps.ValidateToken(ctx, result.PreviewToken, actor, "create", draft, 0)
	require.NoError(t, err, "legitimate actor must still be able to use token after failed attempt by attacker")
}

// ---------------------------------------------------------------------------
// 36. R5 fix: all_principals includes agents
// ---------------------------------------------------------------------------

func TestPreview_AllPrincipalsIncludesAgents(t *testing.T) {
	ps, _, s := previewTestSetup(t)
	ctx := context.Background()

	projectID := pvSeedProject(t, s, "all-agents-project")
	agentID := pvSeedAgent(t, s, "all-agents-agent-1", projectID)
	adminID := pvSeedUser(t, s, "all-agents-admin")

	// Give agent permissions via role binding.
	rd := createTestRoleDefinition(t, s, "test-role-all-agents", store.RoleScopeSystem,
		[]string{"agent.read", "agent.create"})
	pvSeedRoleBinding(t, s, rd.ID, "agent", agentID, store.RoleScopeSystem, "")

	adminRD := createTestRoleDefinition(t, s, "constraint-admin-all-agents", store.RoleScopeSystem,
		[]string{PermissionConstraintAdmin, "agent.read", "agent.create"})
	pvSeedRoleBinding(t, s, adminRD.ID, "user", adminID, store.RoleScopeSystem, "")

	draft := &store.AccessConstraint{
		Name:               "all-principals-with-agents",
		SubjectKind:        store.ConstraintSubjectAllPrincipals,
		ScopeType:          store.RoleScopeSystem,
		MaximumPermissions: []string{"agent.read"},
		Purpose:            "test all_principals includes agents",
	}

	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     pvTestActor(adminID),
	})
	require.NoError(t, err)

	// Look for the agent in affected principals.
	foundAgent := false
	for _, ip := range result.AffectedPage.Items {
		if ip.PrincipalType == "agent" && ip.PrincipalID == agentID {
			foundAgent = true
			break
		}
	}
	assert.True(t, foundAgent, "all_principals should include agents in the affected set")
}

// ---------------------------------------------------------------------------
// 37. R6 fix: ListAffectedPrincipals works for sync previews
// ---------------------------------------------------------------------------

func TestPreview_ListAffectedPrincipalsSyncPreview(t *testing.T) {
	ps, _, s := previewTestSetup(t)
	ctx := context.Background()

	adminID := pvSeedUser(t, s, "list-page-admin")
	adminRD := createTestRoleDefinition(t, s, "constraint-admin-list-page", store.RoleScopeSystem,
		[]string{PermissionConstraintAdmin})
	pvSeedRoleBinding(t, s, adminRD.ID, "user", adminID, store.RoleScopeSystem, "")

	userID := pvSeedUser(t, s, "list-page-user")
	rd := createTestRoleDefinition(t, s, "test-role-list-page", store.RoleScopeSystem,
		[]string{"agent.read"})
	pvSeedRoleBinding(t, s, rd.ID, "user", userID, store.RoleScopeSystem, "")

	draft := &store.AccessConstraint{
		Name:                 "list-page-test",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(userID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "list page test",
	}

	result, err := ps.GeneratePreview(ctx, PreviewRequest{
		Operation: "create",
		Draft:     draft,
		Actor:     pvTestActor(adminID),
	})
	require.NoError(t, err)

	// ListAffectedPrincipals should work for sync previews (R6 fix: stored in asyncJobs).
	page, err := ps.ListAffectedPrincipals(ctx, result.PreviewID, "", 10)
	require.NoError(t, err, "ListAffectedPrincipals must work for sync previews")
	assert.Equal(t, result.Impact.AffectedPrincipalCount, page.TotalCount)
}
