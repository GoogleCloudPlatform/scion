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

package entadapter

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// roleTestEnv provides isolated test infrastructure for role store tests.
type roleTestEnv struct {
	client    *ent.Client
	roleStore *RoleStore

	// Pre-created entities for FK references.
	userID    string
	agentID   string
	groupID   string
	projectID string

	// Pre-created role definitions.
	systemRoleDef  *store.RoleDefinition // system-scoped, e.g. "hub-member"
	projectRoleDef *store.RoleDefinition // project-scoped, e.g. "project-member"
	superAdminDef  *store.RoleDefinition // system-scoped, "super-admin"
	projectOwnerDef *store.RoleDefinition // project-scoped, "project-owner"
}

func newRoleTestEnv(t *testing.T) *roleTestEnv {
	t.Helper()
	client := enttest.NewClient(t)
	ctx := context.Background()

	env := &roleTestEnv{
		client:    client,
		roleStore: NewRoleStore(client),
	}

	// Create a test user.
	userUID := uuid.New()
	_, err := client.User.Create().
		SetID(userUID).
		SetEmail("role-test@example.com").
		SetDisplayName("Role Test User").
		Save(ctx)
	require.NoError(t, err)
	env.userID = userUID.String()

	// Create a test project.
	projectUID := uuid.New()
	_, err = client.Project.Create().
		SetID(projectUID).
		SetName("role-test-project").
		SetSlug("role-test-project").
		Save(ctx)
	require.NoError(t, err)
	env.projectID = projectUID.String()

	// Create a test agent.
	agentUID := uuid.New()
	_, err = client.Agent.Create().
		SetID(agentUID).
		SetName("role-test-agent").
		SetSlug("role-test-agent").
		SetProjectID(projectUID).
		Save(ctx)
	require.NoError(t, err)
	env.agentID = agentUID.String()

	// Create a test group.
	groupUID := uuid.New()
	_, err = client.Group.Create().
		SetID(groupUID).
		SetName("role-test-group").
		SetSlug("role-test-group").
		Save(ctx)
	require.NoError(t, err)
	env.groupID = groupUID.String()

	// Create role definitions.
	env.systemRoleDef, err = env.roleStore.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        store.SystemRoleHubMember,
		ScopeType:   store.RoleScopeSystem,
		Permissions: []string{"hub.read"},
		System:      true,
	})
	require.NoError(t, err)

	env.projectRoleDef, err = env.roleStore.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        store.ProjectRoleMember,
		ScopeType:   store.RoleScopeProject,
		Permissions: []string{"project.read"},
		System:      true,
	})
	require.NoError(t, err)

	env.superAdminDef, err = env.roleStore.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        store.SystemRoleSuperAdmin,
		ScopeType:   store.RoleScopeSystem,
		Permissions: []string{"*"},
		System:      true,
	})
	require.NoError(t, err)

	env.projectOwnerDef, err = env.roleStore.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        store.ProjectRoleOwner,
		ScopeType:   store.RoleScopeProject,
		Permissions: []string{"project.*"},
		System:      true,
	})
	require.NoError(t, err)

	return env
}

// createGroup creates an additional group and returns its UUID string.
func (e *roleTestEnv) createGroup(t *testing.T, name string) string {
	t.Helper()
	uid := uuid.New()
	_, err := e.client.Group.Create().
		SetID(uid).
		SetName(name).
		SetSlug(name).
		Save(context.Background())
	require.NoError(t, err)
	return uid.String()
}

// createUser creates an additional user and returns its UUID string.
func (e *roleTestEnv) createUser(t *testing.T, email string) string {
	t.Helper()
	uid := uuid.New()
	_, err := e.client.User.Create().
		SetID(uid).
		SetEmail(email).
		SetDisplayName("User " + email).
		Save(context.Background())
	require.NoError(t, err)
	return uid.String()
}

// ---------------------------------------------------------------------------
// (a) All principal kinds — create bindings for user, agent, and group.
// ---------------------------------------------------------------------------

func TestCreateRoleBinding_UserPrincipal(t *testing.T) {
	env := newRoleTestEnv(t)
	ctx := context.Background()

	rb, err := env.roleStore.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: env.systemRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      env.userID,
		ScopeType:        store.RoleScopeSystem,
		ScopeID:          "",
		CreatedBy:        "test",
	})
	require.NoError(t, err)
	assert.NotEmpty(t, rb.ID)
	assert.Equal(t, store.RoleBindingPrincipalUser, rb.PrincipalType)
	assert.Equal(t, env.userID, rb.PrincipalID)
	assert.Equal(t, env.systemRoleDef.ID, rb.RoleDefinitionID)

	// Verify retrieval.
	got, err := env.roleStore.GetRoleBinding(ctx, rb.ID)
	require.NoError(t, err)
	assert.Equal(t, rb.ID, got.ID)
}

func TestCreateRoleBinding_AgentPrincipal(t *testing.T) {
	env := newRoleTestEnv(t)
	ctx := context.Background()

	rb, err := env.roleStore.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: env.systemRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalAgent,
		PrincipalID:      env.agentID,
		ScopeType:        store.RoleScopeSystem,
		ScopeID:          "",
		CreatedBy:        "test",
	})
	require.NoError(t, err)
	assert.Equal(t, store.RoleBindingPrincipalAgent, rb.PrincipalType)
	assert.Equal(t, env.agentID, rb.PrincipalID)
}

func TestCreateRoleBinding_GroupPrincipal(t *testing.T) {
	env := newRoleTestEnv(t)
	ctx := context.Background()

	rb, err := env.roleStore.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: env.systemRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      env.groupID,
		ScopeType:        store.RoleScopeSystem,
		ScopeID:          "",
		CreatedBy:        "test",
	})
	require.NoError(t, err)
	assert.Equal(t, store.RoleBindingPrincipalGroup, rb.PrincipalType)
	assert.Equal(t, env.groupID, rb.PrincipalID)
}

func TestCreateRoleBinding_ProjectScoped(t *testing.T) {
	env := newRoleTestEnv(t)
	ctx := context.Background()

	rb, err := env.roleStore.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: env.projectRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      env.userID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          env.projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
	assert.Equal(t, store.RoleScopeProject, rb.ScopeType)
	assert.Equal(t, env.projectID, rb.ScopeID)
}

// ---------------------------------------------------------------------------
// (b) Transitive query inputs — ListRoleBindingsForPrincipals.
// ---------------------------------------------------------------------------

func TestListRoleBindingsForPrincipals_PrincipalClosure(t *testing.T) {
	env := newRoleTestEnv(t)
	ctx := context.Background()

	groupA := env.createGroup(t, "group-a")
	groupB := env.createGroup(t, "group-b")

	// Create bindings for user, group-A, group-B (the "principal closure").
	_, err := env.roleStore.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: env.systemRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      env.userID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	_, err = env.roleStore.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: env.projectRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      groupA,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          env.projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	_, err = env.roleStore.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: env.projectRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      groupB,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          env.projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Create an unrelated binding that should NOT be returned.
	unrelatedUser := env.createUser(t, "unrelated@example.com")
	_, err = env.roleStore.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: env.systemRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      unrelatedUser,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Query with the full principal closure.
	principals := []store.PrincipalRef{
		{Type: store.RoleBindingPrincipalUser, ID: env.userID},
		{Type: store.RoleBindingPrincipalGroup, ID: groupA},
		{Type: store.RoleBindingPrincipalGroup, ID: groupB},
	}

	results, err := env.roleStore.ListRoleBindingsForPrincipals(ctx, principals, nil, nil)
	require.NoError(t, err)
	assert.Len(t, results, 3, "should return bindings for all 3 principals in closure")

	// Verify we did NOT get the unrelated binding.
	for _, rb := range results {
		assert.NotEqual(t, unrelatedUser, rb.PrincipalID)
	}
}

func TestListRoleBindingsForPrincipals_ScopeFilter(t *testing.T) {
	env := newRoleTestEnv(t)
	ctx := context.Background()

	// Create a system-scoped and a project-scoped binding for the same user.
	_, err := env.roleStore.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: env.systemRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      env.userID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	_, err = env.roleStore.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: env.projectRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      env.userID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          env.projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	principals := []store.PrincipalRef{
		{Type: store.RoleBindingPrincipalUser, ID: env.userID},
	}

	// Filter to system scope only.
	results, err := env.roleStore.ListRoleBindingsForPrincipals(ctx, principals, []string{store.RoleScopeSystem}, nil)
	require.NoError(t, err)
	assert.Len(t, results, 1)
	assert.Equal(t, store.RoleScopeSystem, results[0].ScopeType)

	// Filter to project scope only.
	results, err = env.roleStore.ListRoleBindingsForPrincipals(ctx, principals, []string{store.RoleScopeProject}, nil)
	require.NoError(t, err)
	assert.Len(t, results, 1)
	assert.Equal(t, store.RoleScopeProject, results[0].ScopeType)

	// No filter — returns both.
	results, err = env.roleStore.ListRoleBindingsForPrincipals(ctx, principals, nil, nil)
	require.NoError(t, err)
	assert.Len(t, results, 2)
}

func TestListRoleBindingsForPrincipals_EmptyInput(t *testing.T) {
	env := newRoleTestEnv(t)
	ctx := context.Background()

	results, err := env.roleStore.ListRoleBindingsForPrincipals(ctx, nil, nil, nil)
	require.NoError(t, err)
	assert.Empty(t, results)

	results, err = env.roleStore.ListRoleBindingsForPrincipals(ctx, []store.PrincipalRef{}, nil, nil)
	require.NoError(t, err)
	assert.Empty(t, results)
}

// ---------------------------------------------------------------------------
// (c) Invalid scope/principal combinations.
// ---------------------------------------------------------------------------

func TestCreateRoleBinding_ProjectScopedWithSystemRole_Rejected(t *testing.T) {
	env := newRoleTestEnv(t)
	ctx := context.Background()

	_, err := env.roleStore.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: env.systemRoleDef.ID, // system-scoped role
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      env.userID,
		ScopeType:        store.RoleScopeProject, // project scope — mismatch!
		ScopeID:          env.projectID,
		CreatedBy:        "test",
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, store.ErrScopeMismatch),
		"expected ErrScopeMismatch, got: %v", err)
}

func TestCreateRoleBinding_SystemScopedWithProjectRole_Rejected(t *testing.T) {
	env := newRoleTestEnv(t)
	ctx := context.Background()

	_, err := env.roleStore.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: env.projectRoleDef.ID, // project-scoped role
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      env.userID,
		ScopeType:        store.RoleScopeSystem, // system scope — mismatch!
		ScopeID:          "",
		CreatedBy:        "test",
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, store.ErrScopeMismatch),
		"expected ErrScopeMismatch, got: %v", err)
}

func TestCreateRoleBinding_NonexistentRoleDefinition_Rejected(t *testing.T) {
	env := newRoleTestEnv(t)
	ctx := context.Background()

	_, err := env.roleStore.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: uuid.New().String(), // doesn't exist
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      env.userID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestCreateRoleBinding_InvalidPrincipalType_Rejected(t *testing.T) {
	env := newRoleTestEnv(t)
	ctx := context.Background()

	_, err := env.roleStore.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: env.systemRoleDef.ID,
		PrincipalType:    "organization", // invalid
		PrincipalID:      env.userID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, store.ErrInvalidInput),
		"expected ErrInvalidInput, got: %v", err)
}

// ---------------------------------------------------------------------------
// (d) Expiration round trips — NotBefore and ExpiresAt survive storage.
// ---------------------------------------------------------------------------

func TestCreateRoleBinding_LifecycleFields_RoundTrip(t *testing.T) {
	env := newRoleTestEnv(t)
	ctx := context.Background()

	notBefore := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	expiresAt := time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC)

	rb, err := env.roleStore.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: env.systemRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      env.userID,
		ScopeType:        store.RoleScopeSystem,
		NotBefore:        &notBefore,
		ExpiresAt:        &expiresAt,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Retrieve and verify round-trip.
	got, err := env.roleStore.GetRoleBinding(ctx, rb.ID)
	require.NoError(t, err)
	require.NotNil(t, got.NotBefore, "NotBefore should be set")
	require.NotNil(t, got.ExpiresAt, "ExpiresAt should be set")
	assert.True(t, notBefore.Equal(*got.NotBefore), "NotBefore mismatch: want %v got %v", notBefore, *got.NotBefore)
	assert.True(t, expiresAt.Equal(*got.ExpiresAt), "ExpiresAt mismatch: want %v got %v", expiresAt, *got.ExpiresAt)
}

func TestCreateRoleBinding_LifecycleFields_Nil(t *testing.T) {
	env := newRoleTestEnv(t)
	ctx := context.Background()

	rb, err := env.roleStore.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: env.systemRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      env.userID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	got, err := env.roleStore.GetRoleBinding(ctx, rb.ID)
	require.NoError(t, err)
	assert.Nil(t, got.NotBefore, "NotBefore should be nil when unset")
	assert.Nil(t, got.ExpiresAt, "ExpiresAt should be nil when unset")
}

func TestCreateRoleBinding_OnlyNotBefore(t *testing.T) {
	env := newRoleTestEnv(t)
	ctx := context.Background()

	notBefore := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

	rb, err := env.roleStore.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: env.systemRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalAgent,
		PrincipalID:      env.agentID,
		ScopeType:        store.RoleScopeSystem,
		NotBefore:        &notBefore,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	got, err := env.roleStore.GetRoleBinding(ctx, rb.ID)
	require.NoError(t, err)
	require.NotNil(t, got.NotBefore)
	assert.True(t, notBefore.Equal(*got.NotBefore))
	assert.Nil(t, got.ExpiresAt)
}

func TestListRoleBindingsForPrincipals_ReturnsLifecycleFields(t *testing.T) {
	env := newRoleTestEnv(t)
	ctx := context.Background()

	notBefore := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	expiresAt := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)

	_, err := env.roleStore.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: env.systemRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      env.userID,
		ScopeType:        store.RoleScopeSystem,
		NotBefore:        &notBefore,
		ExpiresAt:        &expiresAt,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	results, err := env.roleStore.ListRoleBindingsForPrincipals(ctx, []store.PrincipalRef{
		{Type: store.RoleBindingPrincipalUser, ID: env.userID},
	}, nil, nil)
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.NotNil(t, results[0].NotBefore)
	require.NotNil(t, results[0].ExpiresAt)
	assert.True(t, notBefore.Equal(*results[0].NotBefore))
	assert.True(t, expiresAt.Equal(*results[0].ExpiresAt))
}

// ---------------------------------------------------------------------------
// (e) Duplicate races — two concurrent attempts to create the same binding.
// ---------------------------------------------------------------------------

func TestCreateRoleBinding_Duplicate_Rejected(t *testing.T) {
	env := newRoleTestEnv(t)
	ctx := context.Background()

	binding := &store.RoleBinding{
		RoleDefinitionID: env.systemRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      env.userID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	}

	// First creation succeeds.
	_, err := env.roleStore.CreateRoleBinding(ctx, binding)
	require.NoError(t, err)

	// Second creation fails.
	_, err = env.roleStore.CreateRoleBinding(ctx, binding)
	require.Error(t, err)
	assert.True(t, errors.Is(err, store.ErrAlreadyExists),
		"expected ErrAlreadyExists, got: %v", err)
}

func TestCreateRoleBinding_ConcurrentDuplicateRace(t *testing.T) {
	env := newRoleTestEnv(t)
	ctx := context.Background()

	var wg sync.WaitGroup
	results := make(chan error, 2)

	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := env.roleStore.CreateRoleBinding(ctx, &store.RoleBinding{
				RoleDefinitionID: env.systemRoleDef.ID,
				PrincipalType:    store.RoleBindingPrincipalUser,
				PrincipalID:      env.userID,
				ScopeType:        store.RoleScopeSystem,
				CreatedBy:        "test",
			})
			results <- err
		}()
	}
	wg.Wait()
	close(results)

	var successes, failures int
	for err := range results {
		if err == nil {
			successes++
		} else {
			failures++
			assert.True(t, errors.Is(err, store.ErrAlreadyExists),
				"expected ErrAlreadyExists for failing attempt, got: %v", err)
		}
	}
	assert.Equal(t, 1, successes, "exactly one creation should succeed")
	assert.Equal(t, 1, failures, "exactly one creation should fail")
}

// ---------------------------------------------------------------------------
// (f) Cascade behavior — delete group/project cleans up bindings.
// ---------------------------------------------------------------------------

func TestDeleteRoleBindingsForPrincipal_GroupCascade(t *testing.T) {
	env := newRoleTestEnv(t)
	ctx := context.Background()

	// Create bindings for the group.
	_, err := env.roleStore.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: env.systemRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      env.groupID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	_, err = env.roleStore.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: env.projectRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      env.groupID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          env.projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Also create a user binding that should survive.
	_, err = env.roleStore.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: env.systemRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      env.userID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Cascade delete group bindings.
	n, err := env.roleStore.DeleteRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalGroup, env.groupID)
	require.NoError(t, err)
	assert.Equal(t, 2, n, "should delete 2 group bindings")

	// Verify group bindings are gone.
	remaining, err := env.roleStore.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalGroup, env.groupID)
	require.NoError(t, err)
	assert.Empty(t, remaining, "no group bindings should remain")

	// Verify user binding survived.
	userBindings, err := env.roleStore.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, env.userID)
	require.NoError(t, err)
	assert.Len(t, userBindings, 1, "user binding should survive group cascade")
}

func TestDeleteRoleBindingsForScope_ProjectCascade(t *testing.T) {
	env := newRoleTestEnv(t)
	ctx := context.Background()

	// Create a project-scoped binding.
	_, err := env.roleStore.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: env.projectRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      env.userID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          env.projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	_, err = env.roleStore.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: env.projectRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      env.groupID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          env.projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Also create a system-scoped binding that should survive.
	_, err = env.roleStore.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: env.systemRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      env.userID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Cascade delete project-scoped bindings.
	n, err := env.roleStore.DeleteRoleBindingsForScope(ctx, store.RoleScopeProject, env.projectID)
	require.NoError(t, err)
	assert.Equal(t, 2, n, "should delete 2 project-scoped bindings")

	// Verify project bindings are gone.
	remaining, err := env.roleStore.ListRoleBindingsForScope(ctx, store.RoleScopeProject, env.projectID)
	require.NoError(t, err)
	assert.Empty(t, remaining, "no project-scoped bindings should remain")

	// Verify system binding survived.
	systemBindings, err := env.roleStore.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, env.userID)
	require.NoError(t, err)
	assert.Len(t, systemBindings, 1, "system binding should survive project cascade")
}

func TestDeleteRoleBindingsForPrincipal_NoOrphans(t *testing.T) {
	env := newRoleTestEnv(t)
	ctx := context.Background()

	// Create multiple bindings for the group across different roles and scopes.
	for i := 0; i < 3; i++ {
		groupID := env.createGroup(t, "cascade-group-"+uuid.New().String()[:8])
		_, err := env.roleStore.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: env.systemRoleDef.ID,
			PrincipalType:    store.RoleBindingPrincipalGroup,
			PrincipalID:      groupID,
			ScopeType:        store.RoleScopeSystem,
			CreatedBy:        "test",
		})
		require.NoError(t, err)

		// Delete and verify no orphans.
		n, err := env.roleStore.DeleteRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalGroup, groupID)
		require.NoError(t, err)
		assert.Equal(t, 1, n)

		remaining, err := env.roleStore.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalGroup, groupID)
		require.NoError(t, err)
		assert.Empty(t, remaining, "no orphan bindings for group %s", groupID)
	}
}

// ---------------------------------------------------------------------------
// (g) Super-admin/owner restrictions — direct-user-only.
// ---------------------------------------------------------------------------

func TestCreateRoleBinding_SuperAdmin_GroupPrincipal_Rejected(t *testing.T) {
	env := newRoleTestEnv(t)
	ctx := context.Background()

	_, err := env.roleStore.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: env.superAdminDef.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      env.groupID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        store.SystemReconcileCreatedBy, // reconciler caller
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, store.ErrDirectUserOnly),
		"expected ErrDirectUserOnly, got: %v", err)
}

func TestCreateRoleBinding_SuperAdmin_AgentPrincipal_Rejected(t *testing.T) {
	env := newRoleTestEnv(t)
	ctx := context.Background()

	_, err := env.roleStore.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: env.superAdminDef.ID,
		PrincipalType:    store.RoleBindingPrincipalAgent,
		PrincipalID:      env.agentID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        store.SystemReconcileCreatedBy,
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, store.ErrDirectUserOnly),
		"expected ErrDirectUserOnly, got: %v", err)
}

func TestCreateRoleBinding_SuperAdmin_DirectUser_Succeeds(t *testing.T) {
	env := newRoleTestEnv(t)
	ctx := context.Background()

	rb, err := env.roleStore.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: env.superAdminDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      env.userID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        store.SystemReconcileCreatedBy,
	})
	require.NoError(t, err)
	assert.Equal(t, store.RoleBindingPrincipalUser, rb.PrincipalType)
}

func TestCreateRoleBinding_SuperAdmin_NonReconciler_Rejected(t *testing.T) {
	env := newRoleTestEnv(t)
	ctx := context.Background()

	_, err := env.roleStore.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: env.superAdminDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      env.userID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "some-user", // not the reconciler
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, store.ErrSuperAdminBindingRestricted),
		"expected ErrSuperAdminBindingRestricted, got: %v", err)
}

func TestCreateRoleBinding_ProjectOwner_GroupPrincipal_Rejected(t *testing.T) {
	env := newRoleTestEnv(t)
	ctx := context.Background()

	_, err := env.roleStore.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: env.projectOwnerDef.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      env.groupID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          env.projectID,
		CreatedBy:        "test",
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, store.ErrDirectUserOnly),
		"expected ErrDirectUserOnly, got: %v", err)
}

func TestCreateRoleBinding_ProjectOwner_DirectUser_Succeeds(t *testing.T) {
	env := newRoleTestEnv(t)
	ctx := context.Background()

	rb, err := env.roleStore.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: env.projectOwnerDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      env.userID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          env.projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
	assert.Equal(t, store.RoleBindingPrincipalUser, rb.PrincipalType)
}

// ---------------------------------------------------------------------------
// (h) Scope compatibility validation.
// ---------------------------------------------------------------------------

func TestCreateRoleBinding_ScopeMismatch_SystemRoleProjectScope(t *testing.T) {
	env := newRoleTestEnv(t)
	ctx := context.Background()

	// hub-member is system-scoped, cannot be bound at project scope.
	_, err := env.roleStore.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: env.systemRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      env.userID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          env.projectID,
		CreatedBy:        "test",
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, store.ErrScopeMismatch))
}

func TestCreateRoleBinding_ScopeMismatch_ProjectRoleSystemScope(t *testing.T) {
	env := newRoleTestEnv(t)
	ctx := context.Background()

	// project-member is project-scoped, cannot be bound at system scope.
	_, err := env.roleStore.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: env.projectRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      env.userID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, store.ErrScopeMismatch))
}

// ---------------------------------------------------------------------------
// Additional edge cases
// ---------------------------------------------------------------------------

func TestListRoleBindingsForPrincipals_ScopeIDFilter(t *testing.T) {
	env := newRoleTestEnv(t)
	ctx := context.Background()

	// Create a second project.
	project2UID := uuid.New()
	_, err := env.client.Project.Create().
		SetID(project2UID).
		SetName("project-2").
		SetSlug("project-2").
		Save(ctx)
	require.NoError(t, err)

	// Create bindings for both projects.
	_, err = env.roleStore.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: env.projectRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      env.userID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          env.projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	_, err = env.roleStore.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: env.projectRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      env.userID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          project2UID.String(),
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	principals := []store.PrincipalRef{
		{Type: store.RoleBindingPrincipalUser, ID: env.userID},
	}

	// Filter by first project only.
	results, err := env.roleStore.ListRoleBindingsForPrincipals(ctx, principals, nil, []string{env.projectID})
	require.NoError(t, err)
	assert.Len(t, results, 1)
	assert.Equal(t, env.projectID, results[0].ScopeID)
}

func TestListRoleBindingsForPrincipals_CombinedScopeFilters(t *testing.T) {
	env := newRoleTestEnv(t)
	ctx := context.Background()

	// Create both system and project bindings.
	_, err := env.roleStore.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: env.systemRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      env.userID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	_, err = env.roleStore.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: env.projectRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      env.userID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          env.projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	principals := []store.PrincipalRef{
		{Type: store.RoleBindingPrincipalUser, ID: env.userID},
	}

	// Query both system and project scopes + filter to specific project.
	// System bindings have ScopeID="" which should NOT match the project ID filter.
	// This tests the combined semantics.
	results, err := env.roleStore.ListRoleBindingsForPrincipals(ctx, principals,
		[]string{store.RoleScopeSystem, store.RoleScopeProject},
		[]string{"", env.projectID},
	)
	require.NoError(t, err)
	assert.Len(t, results, 2, "should return both system (scopeID='') and project bindings")

	// Verify system bindings are included even when "" is NOT in scopeIDs.
	// Callers should not need to know that system bindings use scope_id="".
	results2, err := env.roleStore.ListRoleBindingsForPrincipals(ctx, principals,
		[]string{store.RoleScopeSystem, store.RoleScopeProject},
		[]string{env.projectID},
	)
	require.NoError(t, err)
	assert.Len(t, results2, 2, "system bindings should be included even without '' in scopeIDs")
}

func TestCreateRoleBinding_GroupPrincipal_ProjectScoped(t *testing.T) {
	env := newRoleTestEnv(t)
	ctx := context.Background()

	// Groups can hold project-member bindings.
	rb, err := env.roleStore.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: env.projectRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      env.groupID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          env.projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
	assert.Equal(t, store.RoleBindingPrincipalGroup, rb.PrincipalType)
	assert.Equal(t, store.RoleScopeProject, rb.ScopeType)
	assert.Equal(t, env.projectID, rb.ScopeID)
}

func TestDeleteRoleBindingsForScope_NoMatches(t *testing.T) {
	env := newRoleTestEnv(t)
	ctx := context.Background()

	n, err := env.roleStore.DeleteRoleBindingsForScope(ctx, store.RoleScopeProject, uuid.New().String())
	require.NoError(t, err)
	assert.Equal(t, 0, n, "deleting bindings for nonexistent scope should return 0")
}

func TestDeleteRoleBindingsForPrincipal_NoMatches(t *testing.T) {
	env := newRoleTestEnv(t)
	ctx := context.Background()

	n, err := env.roleStore.DeleteRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalGroup, uuid.New().String())
	require.NoError(t, err)
	assert.Equal(t, 0, n, "deleting bindings for nonexistent principal should return 0")
}

// ---------------------------------------------------------------------------
// Provenance — batched query returns enough info for caller to identify role.
// ---------------------------------------------------------------------------

func TestListRoleBindingsForPrincipals_ReturnsRoleDefinitionID(t *testing.T) {
	env := newRoleTestEnv(t)
	ctx := context.Background()

	_, err := env.roleStore.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: env.systemRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      env.userID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	results, err := env.roleStore.ListRoleBindingsForPrincipals(ctx, []store.PrincipalRef{
		{Type: store.RoleBindingPrincipalUser, ID: env.userID},
	}, nil, nil)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, env.systemRoleDef.ID, results[0].RoleDefinitionID,
		"batched query should return RoleDefinitionID for provenance")
}

// ---------------------------------------------------------------------------
// ListRoleBindingsForPrincipal preserves lifecycle fields.
// ---------------------------------------------------------------------------

func TestListRoleBindingsForPrincipal_ReturnsLifecycleFields(t *testing.T) {
	env := newRoleTestEnv(t)
	ctx := context.Background()

	notBefore := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	expiresAt := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)

	_, err := env.roleStore.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: env.systemRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      env.userID,
		ScopeType:        store.RoleScopeSystem,
		NotBefore:        &notBefore,
		ExpiresAt:        &expiresAt,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	results, err := env.roleStore.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, env.userID)
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.NotNil(t, results[0].NotBefore)
	require.NotNil(t, results[0].ExpiresAt)
}
