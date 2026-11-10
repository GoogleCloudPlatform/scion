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

package hubtestutil

import (
	"context"
	"go/ast"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// BearerCeiling builds the frozen permission ceiling for selectors and fails
// the test if any selector does not resolve.
func BearerCeiling(t *testing.T, selectors ...string) permissions.FrozenPermissionCeiling {
	t.Helper()
	ceiling, ok := permissions.BuildCeilingFromSelectors(selectors)
	require.True(t, ok, "selectors must resolve: %v", selectors)
	return ceiling
}

// SystemRoleUserWithPermissions creates a user with a custom system-scoped
// role definition carrying exactly permissionIDs.
func SystemRoleUserWithPermissions(t *testing.T, s store.Store, userID string, permissionIDs []string) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: userID, Email: userID + "@test.com", DisplayName: "u", Role: "member", Status: store.UserStatusActive}))
	rd, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        "apa-custom-" + userID,
		ScopeType:   store.RoleScopeSystem,
		Permissions: permissionIDs,
	})
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
}

// CreateTestUserWithRole creates a user and assigns them a system-scoped role binding.
// For super-admin bindings, CreatedBy is set to the system reconciler sentinel
// because the D10 store-level guard rejects non-reconciler callers.
func CreateTestUserWithRole(t *testing.T, s store.Store, userID, email, role, roleName string) {
	t.Helper()
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userID, Email: email, DisplayName: email, Role: role, Status: "active",
	}))

	rd, err := s.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeSystem)
	require.NoError(t, err, "role definition %q not found", roleName)

	createdBy := "test"
	if roleName == store.SystemRoleSuperAdmin {
		createdBy = store.SystemReconcileCreatedBy
	}

	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
		ScopeID:          "",
		CreatedBy:        createdBy,
	})
	if err != nil && err != store.ErrAlreadyExists {
		t.Fatalf("failed to create role binding: %v", err)
	}
}

// CreateTestUserWithProjectRole creates a user and assigns them a project-scoped role binding.
func CreateTestUserWithProjectRole(t *testing.T, s store.Store, userID, email, projectID, roleName string) {
	t.Helper()
	ctx := context.Background()

	// Create user if not exists
	if _, err := s.GetUser(ctx, userID); err != nil {
		require.NoError(t, s.CreateUser(ctx, &store.User{
			ID: userID, Email: email, DisplayName: email, Role: "member", Status: "active",
		}))
	}

	rd, err := s.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeProject)
	require.NoError(t, err, "role definition %q not found", roleName)

	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	if err != nil && err != store.ErrAlreadyExists {
		t.Fatalf("failed to create project role binding: %v", err)
	}
}

// CreateDelegateTestProject creates a project in the store.
func CreateDelegateTestProject(t *testing.T, s store.Store, projectID, slug, createdBy string) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID:        projectID,
		Name:      slug,
		Slug:      slug,
		CreatedBy: createdBy,
	}))
}

// BoundedCeiling returns a version-1 bounded effect ceiling over the given
// permission IDs (an empty list when there are none).
func BoundedCeiling(ids ...string) store.EffectCeiling {
	if ids == nil {
		ids = []string{}
	}
	return store.EffectCeiling{Kind: store.EffectCeilingBounded, Version: permissions.CeilingVersionV1, PermissionIDs: ids}
}

// AllRegistryIDs returns the ID of every registry permission, in registry
// order.
func AllRegistryIDs() []string {
	ids := make([]string, 0, len(permissions.Registry))
	for _, p := range permissions.Registry {
		ids = append(ids, p.ID)
	}
	return ids
}

// SoftDeleteStoredAgent marks the stored agent id as deleted at the current
// time.
func SoftDeleteStoredAgent(t *testing.T, s store.Store, id string) {
	t.Helper()
	a, err := s.GetAgent(context.Background(), id)
	require.NoError(t, err)
	a.DeletedAt = time.Now()
	require.NoError(t, s.UpdateAgent(context.Background(), a))
}

// AddProjectMemberWithRole is a small helper that adds the given user to the
// project's members group with the requested role.
func AddProjectMemberWithRole(t *testing.T, s store.Store, project *store.Project, userID, role string) {
	t.Helper()
	ctx := context.Background()
	membersGroup, err := s.GetGroupBySlug(ctx, "project:"+project.Slug+":members")
	require.NoError(t, err)
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    membersGroup.ID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   userID,
		Role:       role,
	}))

	// Phase 1F: also create the corresponding project role binding, since
	// isProjectOwnerOrAdmin now uses role bindings as the sole source of truth.
	groupRoleMap := map[string]string{
		store.GroupMemberRoleOwner:  store.ProjectRoleOwner,
		store.GroupMemberRoleAdmin:  store.ProjectRoleAdmin,
		store.GroupMemberRoleMember: store.ProjectRoleMember,
	}
	if roleName, ok := groupRoleMap[role]; ok {
		rd, err := s.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeProject)
		require.NoError(t, err, "project role definition %q not found", roleName)
		_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: rd.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      userID,
			ScopeType:        store.RoleScopeProject,
			ScopeID:          project.ID,
			CreatedBy:        "test",
		})
		if err != nil && err != store.ErrAlreadyExists {
			t.Fatalf("failed to create project role binding: %v", err)
		}
	}
}

// RecordedProv returns version-1 provenance whose source is the edge's
// delegator, as every recording write produces.
func RecordedProv(kind string, id string, cred store.SourceCredentialKind) store.AuthorityProvenance {
	return store.AuthorityProvenance{
		ProvenanceVersion:    store.ProvenanceVersionV1,
		SourcePrincipalKind:  kind,
		SourcePrincipalID:    id,
		SourceCredentialKind: cred,
	}
}

// DeclReceiverName returns "Recv.Name" or "Name" for decl.
func DeclReceiverName(decl *ast.FuncDecl) string {
	if decl.Recv == nil || len(decl.Recv.List) == 0 {
		return decl.Name.Name
	}
	typ := decl.Recv.List[0].Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	if id, ok := typ.(*ast.Ident); ok {
		return id.Name + "." + decl.Name.Name
	}
	return decl.Name.Name
}

// SameTypeRegistryPermissions returns every registry permission whose
// Resource is resourceType, in registry order.
func SameTypeRegistryPermissions(resourceType string) []permissions.Permission {
	var out []permissions.Permission
	for _, p := range permissions.Registry {
		if p.Resource == resourceType {
			out = append(out, p)
		}
	}
	return out
}

// GrantProjectAccessOnly binds userID in projectID to a project-scoped role
// with no permissions. The binding is project membership (active project
// access, which owner and ancestor relationships on project targets
// require; ptone/scion#2141) without granting any permission through the
// role, so the relationship stays the only grant source under test.
func GrantProjectAccessOnly(t *testing.T, s store.Store, userID, projectID string) {
	t.Helper()
	ctx := context.Background()
	name := "relchar-access-only"
	rd, err := s.GetRoleDefinitionByName(ctx, name, store.RoleScopeProject)
	if err != nil {
		require.ErrorIs(t, err, store.ErrNotFound)
		rd, err = s.CreateRoleDefinition(ctx, &store.RoleDefinition{
			ID: api.NewUUID(), Name: name, Description: "project access, no permissions",
			ScopeType: store.RoleScopeProject, Permissions: []string{},
		})
		require.NoError(t, err)
	}
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: userID,
		ScopeType: store.RoleScopeProject, ScopeID: projectID, CreatedBy: "test",
	})
	require.NoError(t, err)
}

// SetUserStatus sets the stored user's status.
func SetUserStatus(t *testing.T, s store.Store, id, status string) {
	t.Helper()
	u, err := s.GetUser(context.Background(), id)
	require.NoError(t, err)
	u.Status = status
	require.NoError(t, s.UpdateUser(context.Background(), u))
}

// CreateTestRoleDefinition creates a custom role definition for tests.
func CreateTestRoleDefinition(t *testing.T, s store.Store, name, scopeType string, permissions []string) *store.RoleDefinition {
	t.Helper()
	ctx := context.Background()
	rd, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        name,
		ScopeType:   scopeType,
		Permissions: permissions,
	})
	require.NoError(t, err)
	return rd
}
