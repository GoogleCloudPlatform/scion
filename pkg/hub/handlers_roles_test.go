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
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// createRoleViaAPI creates a role definition through the handler and returns it.
func createRoleViaAPI(t *testing.T, srv *Server, req createRoleDefinitionRequest) *store.RoleDefinition {
	t.Helper()
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/roles", req)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	var def store.RoleDefinition
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&def))
	return &def
}

// createBindingViaAPI creates a role binding through the handler and returns it.
func createBindingViaAPI(t *testing.T, srv *Server, req createRoleBindingRequest) *store.RoleBinding {
	t.Helper()
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/role-bindings", req)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	var rb store.RoleBinding
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&rb))
	return &rb
}

// ---------------------------------------------------------------------------
// Tests: Role Definition CRUD
// ---------------------------------------------------------------------------

func TestRolesAPI_CreateRoleDefinition(t *testing.T) {
	srv, _ := testServer(t)

	def := createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name:        "custom-viewer",
		Description: "Custom viewer role",
		ScopeType:   "system",
		Permissions: []string{"agent.read", "project.read"},
	})

	assert.NotEmpty(t, def.ID)
	assert.Equal(t, "custom-viewer", def.Name)
	assert.Equal(t, "system", def.ScopeType)
	assert.Equal(t, []string{"agent.read", "project.read"}, def.Permissions)
	assert.False(t, def.System)
}

func TestRolesAPI_CreateRoleDefinition_MissingName(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/roles", createRoleDefinitionRequest{
		ScopeType:   "system",
		Permissions: []string{"agent.read"},
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestRolesAPI_CreateRoleDefinition_InvalidScopeType(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/roles", createRoleDefinitionRequest{
		Name:        "bad-scope",
		ScopeType:   "invalid",
		Permissions: []string{"agent.read"},
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestRolesAPI_CreateRoleDefinition_InvalidPermissions(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/roles", createRoleDefinitionRequest{
		Name:        "bad-perms",
		ScopeType:   "system",
		Permissions: []string{"nonexistent.permission"},
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestRolesAPI_CreateRoleDefinition_ProjectScope(t *testing.T) {
	srv, _ := testServer(t)

	def := createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name:        "project-viewer",
		Description: "Project viewer role",
		ScopeType:   "project",
		Permissions: []string{"agent.read", "agent.list"},
	})

	assert.Equal(t, "project", def.ScopeType)
	assert.False(t, def.System)
}

func TestRolesAPI_GetRoleDefinition(t *testing.T) {
	srv, _ := testServer(t)

	created := createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name:        "get-test-role",
		Description: "Get test",
		ScopeType:   "system",
		Permissions: []string{"agent.read"},
	})

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/roles/"+created.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var def store.RoleDefinition
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&def))
	assert.Equal(t, created.ID, def.ID)
	assert.Equal(t, "get-test-role", def.Name)
}

func TestRolesAPI_GetRoleDefinition_NotFound(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/roles/00000000-0000-0000-0000-000000000000", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestRolesAPI_ListRoleDefinitions(t *testing.T) {
	srv, _ := testServer(t)

	// System roles are seeded, so the list should already be non-empty.
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/roles", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp listRoleDefinitionsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Greater(t, resp.TotalCount, 0)
}

func TestRolesAPI_UpdateRoleDefinition(t *testing.T) {
	srv, _ := testServer(t)

	created := createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name:        "update-test-role",
		Description: "Before update",
		ScopeType:   "system",
		Permissions: []string{"agent.read"},
	})

	rec := doRequest(t, srv, http.MethodPut, "/api/v1/admin/roles/"+created.ID, updateRoleDefinitionRequest{
		Name:        "updated-role",
		Description: "After update",
		Permissions: []string{"agent.read", "project.read"},
	})
	require.Equal(t, http.StatusOK, rec.Code)

	var updated store.RoleDefinition
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&updated))
	assert.Equal(t, "updated-role", updated.Name)
	assert.Equal(t, "After update", updated.Description)
	assert.Equal(t, []string{"agent.read", "project.read"}, updated.Permissions)
}

func TestRolesAPI_UpdateRoleDefinition_SystemRole(t *testing.T) {
	srv, st := testServer(t)

	// Find a system role.
	defs, err := st.ListRoleDefinitions(t.Context())
	require.NoError(t, err)

	var systemRole *store.RoleDefinition
	for _, d := range defs {
		if d.System {
			systemRole = d
			break
		}
	}
	require.NotNil(t, systemRole, "expected at least one system role")

	rec := doRequest(t, srv, http.MethodPut, "/api/v1/admin/roles/"+systemRole.ID, updateRoleDefinitionRequest{
		Name:        "hacked-name",
		Description: "hacked",
		Permissions: []string{"agent.read"},
	})
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestRolesAPI_DeleteRoleDefinition(t *testing.T) {
	srv, _ := testServer(t)

	created := createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name:        "delete-test-role",
		Description: "To be deleted",
		ScopeType:   "system",
		Permissions: []string{"agent.read"},
	})

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/admin/roles/"+created.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// Verify it's gone.
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/admin/roles/"+created.ID, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestRolesAPI_DeleteRoleDefinition_SystemRole(t *testing.T) {
	srv, st := testServer(t)

	defs, err := st.ListRoleDefinitions(t.Context())
	require.NoError(t, err)

	var systemRole *store.RoleDefinition
	for _, d := range defs {
		if d.System {
			systemRole = d
			break
		}
	}
	require.NotNil(t, systemRole)

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/admin/roles/"+systemRole.ID, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestRolesAPI_DeleteRoleDefinition_WithBindings(t *testing.T) {
	srv, _ := testServer(t)

	// Create a custom role.
	role := createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name:        "bound-role",
		Description: "Has bindings",
		ScopeType:   "system",
		Permissions: []string{"agent.read"},
	})

	// Create a binding to it.
	createBindingViaAPI(t, srv, createRoleBindingRequest{
		RoleDefinitionID: role.ID,
		PrincipalType:    "user",
		PrincipalID:      DevUserID,
		ScopeType:        "system",
	})

	// Try to delete — should fail because of active bindings.
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/admin/roles/"+role.ID, nil)
	assert.Equal(t, http.StatusConflict, rec.Code)
}

func TestRolesAPI_MethodNotAllowed_Roles(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/admin/roles", nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

// ---------------------------------------------------------------------------
// Tests: Role Binding CRUD
// ---------------------------------------------------------------------------

func TestRolesAPI_CreateRoleBinding(t *testing.T) {
	srv, _ := testServer(t)

	// Create a custom role first.
	role := createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name:        "binding-test-role",
		Description: "For binding tests",
		ScopeType:   "system",
		Permissions: []string{"agent.read"},
	})

	binding := createBindingViaAPI(t, srv, createRoleBindingRequest{
		RoleDefinitionID: role.ID,
		PrincipalType:    "user",
		PrincipalID:      "some-user-id",
		ScopeType:        "system",
	})

	assert.NotEmpty(t, binding.ID)
	assert.Equal(t, role.ID, binding.RoleDefinitionID)
	assert.Equal(t, "user", binding.PrincipalType)
	assert.Equal(t, "some-user-id", binding.PrincipalID)
	assert.Equal(t, "system", binding.ScopeType)
}

func TestRolesAPI_CreateRoleBinding_MissingFields(t *testing.T) {
	srv, _ := testServer(t)

	tests := []struct {
		name string
		req  createRoleBindingRequest
	}{
		{"missing roleDefinitionId", createRoleBindingRequest{PrincipalType: "user", PrincipalID: "u1", ScopeType: "system"}},
		{"missing principalType", createRoleBindingRequest{RoleDefinitionID: "some-id", PrincipalID: "u1", ScopeType: "system"}},
		{"missing principalId", createRoleBindingRequest{RoleDefinitionID: "some-id", PrincipalType: "user", ScopeType: "system"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/role-bindings", tt.req)
			assert.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
		})
	}
}

func TestRolesAPI_CreateRoleBinding_InvalidPrincipalType(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/role-bindings", createRoleBindingRequest{
		RoleDefinitionID: "some-id",
		PrincipalType:    "organization",
		PrincipalID:      "o1",
		ScopeType:        "system",
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestRolesAPI_CreateRoleBinding_ProjectScopeMissingScopeID(t *testing.T) {
	srv, _ := testServer(t)

	role := createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name:        "project-scope-no-id-role",
		ScopeType:   "project",
		Permissions: []string{"agent.read"},
	})

	// project-scoped binding with empty scope_id → 400
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/role-bindings", createRoleBindingRequest{
		RoleDefinitionID: role.ID,
		PrincipalType:    "user",
		PrincipalID:      "u1",
		ScopeType:        "project",
		ScopeID:          "",
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "scope_id is required")
}

func TestRolesAPI_CreateRoleBinding_InvalidScopeType(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/role-bindings", createRoleBindingRequest{
		RoleDefinitionID: "some-id",
		PrincipalType:    "user",
		PrincipalID:      "u1",
		ScopeType:        "invalid",
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestRolesAPI_CreateRoleBinding_SuperAdmin_Blocked(t *testing.T) {
	srv, st := testServer(t)

	// Find the super-admin role definition.
	rd, err := st.GetRoleDefinitionByName(t.Context(), store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)

	// Try to create a super-admin binding — should be blocked by the D10 store guard.
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/role-bindings", createRoleBindingRequest{
		RoleDefinitionID: rd.ID,
		PrincipalType:    "user",
		PrincipalID:      "some-user",
		ScopeType:        "system",
	})
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestRolesAPI_ListRoleBindings(t *testing.T) {
	srv, _ := testServer(t)

	// Create a binding so the list is non-empty.
	role := createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name:        "list-bindings-role",
		ScopeType:   "system",
		Permissions: []string{"agent.read"},
	})
	createBindingViaAPI(t, srv, createRoleBindingRequest{
		RoleDefinitionID: role.ID,
		PrincipalType:    "user",
		PrincipalID:      "list-user",
		ScopeType:        "system",
	})

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/role-bindings", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp listRoleBindingsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.GreaterOrEqual(t, resp.TotalCount, 1)
}

func TestRolesAPI_DeleteRoleBinding(t *testing.T) {
	srv, _ := testServer(t)

	// Create a custom role and binding.
	role := createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name:        "del-binding-role",
		ScopeType:   "system",
		Permissions: []string{"agent.read"},
	})
	binding := createBindingViaAPI(t, srv, createRoleBindingRequest{
		RoleDefinitionID: role.ID,
		PrincipalType:    "user",
		PrincipalID:      "del-user",
		ScopeType:        "system",
	})

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/admin/role-bindings/"+binding.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestRolesAPI_DeleteRoleBinding_NotFound(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/admin/role-bindings/00000000-0000-0000-0000-000000000000", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestRolesAPI_ListBindingsForUser(t *testing.T) {
	srv, _ := testServer(t)

	// Create a custom role and bind it to a specific user.
	role := createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name:        "user-binding-role",
		ScopeType:   "system",
		Permissions: []string{"agent.read"},
	})
	createBindingViaAPI(t, srv, createRoleBindingRequest{
		RoleDefinitionID: role.ID,
		PrincipalType:    "user",
		PrincipalID:      "target-user-123",
		ScopeType:        "system",
	})

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/role-bindings/user/target-user-123", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp listRoleBindingsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.GreaterOrEqual(t, resp.TotalCount, 1)
}

func TestRolesAPI_ListBindingsForUser_Empty(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/role-bindings/user/nonexistent-user", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp listRoleBindingsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, 0, resp.TotalCount)
}

func TestRolesAPI_MethodNotAllowed_Bindings(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/admin/role-bindings", nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

// ---------------------------------------------------------------------------
// Tests: Permissions Registry Endpoint
// ---------------------------------------------------------------------------

func TestRolesAPI_ListPermissions(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/permissions", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp listPermissionsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, len(permissions.Registry), resp.TotalCount)
	assert.Greater(t, resp.TotalCount, 0)

	// Verify role permissions are in the registry.
	found := make(map[string]bool)
	for _, p := range resp.Items {
		found[p.ID] = true
	}
	assert.True(t, found["role.read"], "role.read should be in registry")
	assert.True(t, found["role.create"], "role.create should be in registry")
	assert.True(t, found["role_binding.read"], "role_binding.read should be in registry")
	assert.True(t, found["role_binding.create"], "role_binding.create should be in registry")
}

func TestRolesAPI_ListPermissions_MethodNotAllowed(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/permissions", nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

// ---------------------------------------------------------------------------
// Tests: Unauthenticated access
// ---------------------------------------------------------------------------

func TestRolesAPI_Unauthenticated_Roles(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequestNoAuth(t, srv, http.MethodGet, "/api/v1/admin/roles", nil)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestRolesAPI_Unauthenticated_Bindings(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequestNoAuth(t, srv, http.MethodGet, "/api/v1/admin/role-bindings", nil)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestRolesAPI_Unauthenticated_Permissions(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequestNoAuth(t, srv, http.MethodGet, "/api/v1/admin/permissions", nil)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

// ---------------------------------------------------------------------------
// Tests: Validate Permission IDs
// ---------------------------------------------------------------------------

func TestValidatePermissionIDs_Valid(t *testing.T) {
	err := validatePermissionIDs([]string{"agent.read", "project.read"})
	assert.NoError(t, err)
}

func TestValidatePermissionIDs_Invalid(t *testing.T) {
	err := validatePermissionIDs([]string{"agent.read", "fake.perm"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "fake.perm")
}

func TestValidatePermissionIDs_Empty(t *testing.T) {
	err := validatePermissionIDs(nil)
	assert.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Tests: Role Binding with agent principal type
// ---------------------------------------------------------------------------

func TestRolesAPI_CreateRoleBinding_AgentPrincipal(t *testing.T) {
	srv, _ := testServer(t)

	role := createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name:        "agent-binding-role",
		ScopeType:   "project",
		Permissions: []string{"agent.read"},
	})

	binding := createBindingViaAPI(t, srv, createRoleBindingRequest{
		RoleDefinitionID: role.ID,
		PrincipalType:    "agent",
		PrincipalID:      "some-agent-id",
		ScopeType:        "project",
		ScopeID:          "some-project-id",
	})

	assert.Equal(t, "agent", binding.PrincipalType)
	assert.Equal(t, "some-agent-id", binding.PrincipalID)
	assert.Equal(t, "project", binding.ScopeType)
}

func TestRolesAPI_CreateRoleBinding_GroupPrincipal(t *testing.T) {
	srv, s := testServer(t)
	ctx := t.Context()

	// Create a group so the group existence check passes
	require.NoError(t, s.CreateGroup(ctx, &store.Group{
		ID: tid("rb-test-group"), Slug: "rb-test-group", Name: "RB Test Group",
	}))

	role := createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name:        "group-binding-role",
		ScopeType:   "system",
		Permissions: []string{"agent.read"},
	})

	binding := createBindingViaAPI(t, srv, createRoleBindingRequest{
		RoleDefinitionID: role.ID,
		PrincipalType:    "group",
		PrincipalID:      tid("rb-test-group"),
		ScopeType:        "system",
	})

	assert.Equal(t, "group", binding.PrincipalType)
	assert.Equal(t, tid("rb-test-group"), binding.PrincipalID)
	assert.Equal(t, "system", binding.ScopeType)
}

func TestRolesAPI_CreateRoleBinding_GroupPrincipal_NotFound(t *testing.T) {
	srv, _ := testServer(t)

	role := createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name:        "group-notfound-role",
		ScopeType:   "system",
		Permissions: []string{"agent.read"},
	})

	// Try to create a binding for a non-existent group
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/role-bindings", createRoleBindingRequest{
		RoleDefinitionID: role.ID,
		PrincipalType:    "group",
		PrincipalID:      "00000000-0000-0000-0000-000000000099",
		ScopeType:        "system",
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "group not found")
}

// ---------------------------------------------------------------------------
// Tests: Update role definition with invalid permissions
// ---------------------------------------------------------------------------

func TestRolesAPI_UpdateRoleDefinition_InvalidPermissions(t *testing.T) {
	srv, _ := testServer(t)

	created := createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name:        "update-invalid-perms",
		ScopeType:   "system",
		Permissions: []string{"agent.read"},
	})

	rec := doRequest(t, srv, http.MethodPut, "/api/v1/admin/roles/"+created.ID, updateRoleDefinitionRequest{
		Name:        "updated-name",
		Permissions: []string{"nonexistent.permission"},
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// ---------------------------------------------------------------------------
// Helpers: Non-admin identity
// ---------------------------------------------------------------------------

const (
	nonAdminUserID    = "11111111-1111-1111-1111-111111111111"
	nonAdminUserEmail = "scoped-admin@test.local"
)

// doRequestAsIdentity performs an HTTP request with a custom identity injected
// into the context. This bypasses the dev-auth super-admin fast-path by using
// an AuthenticatedUser whose Role() != "admin", so that CanDelegate and
// permission-based route guards are actually exercised.
func doRequestAsIdentity(t *testing.T, srv *Server, identity Identity, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("failed to marshal body: %v", err)
		}
	}

	req := httptest.NewRequest(method, path, bytes.NewReader(bodyBytes))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// Inject the custom identity directly into the context so the route
	// guard and handler see a non-admin user instead of the DevUser.
	ctx := contextWithIdentity(req.Context(), identity)
	req = req.WithContext(ctx)

	// Serve using the mux directly (bypassing auth middleware) since the
	// identity is already in the context. The route guards and handlers
	// call GetIdentityFromContext, which will find our injected identity.
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	return rec
}

// setupNonAdminUser creates a non-admin user identity and grants it specific
// permissions via a custom role binding. Returns the identity.
//
// The user gets a role binding granting the specified permissions at system scope.
func setupNonAdminUser(t *testing.T, st store.Store, perms []string) *AuthenticatedUser {
	t.Helper()
	ctx := t.Context()

	user := NewAuthenticatedUser(nonAdminUserID, nonAdminUserEmail, "Scoped Admin", "member", "api")

	// Create a custom role definition with the requested permissions.
	rd, err := st.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        "test-scoped-admin-" + t.Name(),
		Description: "Test scoped admin role",
		ScopeType:   store.RoleScopeSystem,
		Permissions: perms,
		System:      false,
	})
	require.NoError(t, err)

	// Bind it to our test user.
	_, err = st.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      nonAdminUserID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test-setup",
	})
	require.NoError(t, err)

	return user
}

// ---------------------------------------------------------------------------
// Tests: Non-admin CanDelegate enforcement
// ---------------------------------------------------------------------------

func TestRolesAPI_NonAdmin_CreateRole_WithHeldPermissions(t *testing.T) {
	srv, st := testServer(t)

	// Give non-admin user role.create, role.read, and agent.read permissions.
	user := setupNonAdminUser(t, st, []string{"role.create", "role.read", "agent.read"})

	// Create a role containing only permissions the user holds → should succeed.
	rec := doRequestAsIdentity(t, srv, user, http.MethodPost, "/api/v1/admin/roles", createRoleDefinitionRequest{
		Name:        "non-admin-created-role",
		Description: "Created by non-admin",
		ScopeType:   "system",
		Permissions: []string{"agent.read"},
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var def store.RoleDefinition
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&def))
	assert.Equal(t, "non-admin-created-role", def.Name)
	assert.Equal(t, []string{"agent.read"}, def.Permissions)
}

func TestRolesAPI_NonAdmin_CreateRole_WithUnheldPermissions(t *testing.T) {
	srv, st := testServer(t)

	// Give non-admin user role.create and agent.read, but NOT user.suspend.
	user := setupNonAdminUser(t, st, []string{"role.create", "role.read", "agent.read"})

	// Try to create a role containing user.suspend → should get 403 (CanDelegate denies).
	rec := doRequestAsIdentity(t, srv, user, http.MethodPost, "/api/v1/admin/roles", createRoleDefinitionRequest{
		Name:        "escalated-role",
		Description: "Attempting privilege escalation",
		ScopeType:   "system",
		Permissions: []string{"agent.read", "user.suspend"},
	})
	assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
}

func TestRolesAPI_NonAdmin_UpdateRole_WithUnheldPermissions(t *testing.T) {
	srv, st := testServer(t)

	// Give non-admin user role.create, role.update, role.read, and agent.read.
	user := setupNonAdminUser(t, st, []string{"role.create", "role.update", "role.read", "agent.read"})

	// First, create a role as admin (DevUser) with only agent.read.
	created := createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name:        "update-escalation-test",
		Description: "Role to be updated by non-admin",
		ScopeType:   "system",
		Permissions: []string{"agent.read"},
	})

	// Non-admin tries to update the role to add user.suspend → should get 403.
	rec := doRequestAsIdentity(t, srv, user, http.MethodPut, "/api/v1/admin/roles/"+created.ID, updateRoleDefinitionRequest{
		Name:        "update-escalation-test",
		Description: "Escalated",
		Permissions: []string{"agent.read", "user.suspend"},
	})
	assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
}

func TestRolesAPI_NonAdmin_CreateBinding_WithHeldPermissions(t *testing.T) {
	srv, st := testServer(t)

	// Give non-admin user role_binding.create, role_binding.read, and agent.read.
	user := setupNonAdminUser(t, st, []string{"role_binding.create", "role_binding.read", "agent.read"})

	// Create a role as admin with only agent.read (a permission our user holds).
	role := createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name:        "binding-held-test",
		Description: "Contains only held permissions",
		ScopeType:   "system",
		Permissions: []string{"agent.read"},
	})

	// Non-admin creates binding for this role → should succeed.
	rec := doRequestAsIdentity(t, srv, user, http.MethodPost, "/api/v1/admin/role-bindings", createRoleBindingRequest{
		RoleDefinitionID: role.ID,
		PrincipalType:    "user",
		PrincipalID:      "some-other-user",
		ScopeType:        "system",
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
}

func TestRolesAPI_NonAdmin_CreateBinding_WithUnheldPermissions(t *testing.T) {
	srv, st := testServer(t)

	// Give non-admin user role_binding.create, role_binding.read, and agent.read
	// but NOT user.suspend.
	user := setupNonAdminUser(t, st, []string{"role_binding.create", "role_binding.read", "agent.read"})

	// Create a role as admin that includes user.suspend (a permission our user does NOT hold).
	role := createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name:        "binding-unheld-test",
		Description: "Contains unheld permissions",
		ScopeType:   "system",
		Permissions: []string{"agent.read", "user.suspend"},
	})

	// Non-admin tries to create binding for this role → should get 403.
	rec := doRequestAsIdentity(t, srv, user, http.MethodPost, "/api/v1/admin/role-bindings", createRoleBindingRequest{
		RoleDefinitionID: role.ID,
		PrincipalType:    "user",
		PrincipalID:      "some-other-user",
		ScopeType:        "system",
	})
	assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
}
