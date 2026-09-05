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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedRolesTestUser creates a user in the store if it does not already exist.
// The store requires user principals to exist before role bindings can reference them.
func seedRolesTestUser(t *testing.T, s store.Store, id, email string) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.GetUser(ctx, id); err == nil {
		return // already exists
	}
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: id, Email: email, DisplayName: email, Role: "member", Status: "active",
	}))
}

// seedRolesTestAgent creates an agent in the store with a minimal project,
// so role bindings referencing agent principals pass existence validation.
func seedRolesTestAgent(t *testing.T, s store.Store, agentID, projectID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.GetAgent(ctx, agentID); err == nil {
		return // already exists
	}
	// Ensure project exists for the agent.
	if _, err := s.GetProject(ctx, projectID); err != nil {
		_ = s.CreateProject(ctx, &store.Project{
			ID: projectID, Name: "roles-test-project", Slug: "roles-test-project",
		})
	}
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: agentID, Slug: agentID, Name: "roles-test-agent", ProjectID: projectID,
	}))
}

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
	srv, s := testServer(t)

	userID := tid("rb-some-user")
	seedRolesTestUser(t, s, userID, "rb-some-user@test.local")

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
		PrincipalID:      userID,
		ScopeType:        "system",
	})

	assert.NotEmpty(t, binding.ID)
	assert.Equal(t, role.ID, binding.RoleDefinitionID)
	assert.Equal(t, "user", binding.PrincipalType)
	assert.Equal(t, userID, binding.PrincipalID)
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
	srv, s := testServer(t)

	listUserID := tid("rb-list-user")
	seedRolesTestUser(t, s, listUserID, "rb-list-user@test.local")

	// Create a binding so the list is non-empty.
	role := createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name:        "list-bindings-role",
		ScopeType:   "system",
		Permissions: []string{"agent.read"},
	})
	createBindingViaAPI(t, srv, createRoleBindingRequest{
		RoleDefinitionID: role.ID,
		PrincipalType:    "user",
		PrincipalID:      listUserID,
		ScopeType:        "system",
	})

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/role-bindings", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp listRoleBindingsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.GreaterOrEqual(t, resp.TotalCount, 1)
}

func TestRolesAPI_DeleteRoleBinding(t *testing.T) {
	srv, s := testServer(t)

	delUserID := tid("rb-del-user")
	seedRolesTestUser(t, s, delUserID, "rb-del-user@test.local")

	// Create a custom role and binding.
	role := createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name:        "del-binding-role",
		ScopeType:   "system",
		Permissions: []string{"agent.read"},
	})
	binding := createBindingViaAPI(t, srv, createRoleBindingRequest{
		RoleDefinitionID: role.ID,
		PrincipalType:    "user",
		PrincipalID:      delUserID,
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
	srv, s := testServer(t)

	targetUserID := tid("rb-target-user")
	seedRolesTestUser(t, s, targetUserID, "rb-target-user@test.local")

	// Create a custom role and bind it to a specific user.
	role := createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name:        "user-binding-role",
		ScopeType:   "system",
		Permissions: []string{"agent.read"},
	})
	createBindingViaAPI(t, srv, createRoleBindingRequest{
		RoleDefinitionID: role.ID,
		PrincipalType:    "user",
		PrincipalID:      targetUserID,
		ScopeType:        "system",
	})

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/role-bindings/user/"+targetUserID, nil)
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
// Tests: Role Binding Sorting
// ---------------------------------------------------------------------------

// setupSortBindings creates three bindings with distinct principals, roles, and
// scopes to exercise every sort axis.  It returns them keyed by a short label.
func setupSortBindings(t *testing.T, srv *Server) map[string]*store.RoleBinding {
	t.Helper()

	roleA := createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name:        "sort-alpha-role",
		ScopeType:   "system",
		Permissions: []string{"agent.read"},
	})
	roleB := createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name:        "sort-beta-role",
		ScopeType:   "system",
		Permissions: []string{"project.read"},
	})
	roleC := createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name:        "sort-gamma-role",
		ScopeType:   "project",
		Permissions: []string{"agent.read"},
	})

	b1 := createBindingViaAPI(t, srv, createRoleBindingRequest{
		RoleDefinitionID: roleA.ID,
		PrincipalType:    "user",
		PrincipalID:      "alice",
		ScopeType:        "system",
	})
	b2 := createBindingViaAPI(t, srv, createRoleBindingRequest{
		RoleDefinitionID: roleB.ID,
		PrincipalType:    "user",
		PrincipalID:      "bob",
		ScopeType:        "system",
	})
	b3 := createBindingViaAPI(t, srv, createRoleBindingRequest{
		RoleDefinitionID: roleC.ID,
		PrincipalType:    "user",
		PrincipalID:      "charlie",
		ScopeType:        "project",
		ScopeID:          "proj-1",
	})

	return map[string]*store.RoleBinding{
		"alice":   b1,
		"bob":     b2,
		"charlie": b3,
	}
}

func TestRolesAPI_ListRoleBindings_SortByPrincipalAsc(t *testing.T) {
	srv, _ := testServer(t)
	bindings := setupSortBindings(t, srv)

	rec := doRequest(t, srv, http.MethodGet,
		"/api/v1/admin/role-bindings?sort_by=principal&sort_order=asc&limit=100", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp listRoleBindingsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.GreaterOrEqual(t, len(resp.Items), 3)

	// Find the indices of our bindings in the result set.
	idx := make(map[string]int)
	for i, item := range resp.Items {
		for label, b := range bindings {
			if item.ID == b.ID {
				idx[label] = i
			}
		}
	}

	assert.Less(t, idx["alice"], idx["bob"], "alice < bob in principal asc")
	assert.Less(t, idx["bob"], idx["charlie"], "bob < charlie in principal asc")
}

func TestRolesAPI_ListRoleBindings_SortByPrincipalDesc(t *testing.T) {
	srv, _ := testServer(t)
	bindings := setupSortBindings(t, srv)

	rec := doRequest(t, srv, http.MethodGet,
		"/api/v1/admin/role-bindings?sort_by=principal&sort_order=desc&limit=100", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp listRoleBindingsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	idx := make(map[string]int)
	for i, item := range resp.Items {
		for label, b := range bindings {
			if item.ID == b.ID {
				idx[label] = i
			}
		}
	}

	assert.Less(t, idx["charlie"], idx["bob"], "charlie before bob in principal desc")
	assert.Less(t, idx["bob"], idx["alice"], "bob before alice in principal desc")
}

func TestRolesAPI_ListRoleBindings_SortByCreatedAsc(t *testing.T) {
	srv, _ := testServer(t)
	bindings := setupSortBindings(t, srv)

	rec := doRequest(t, srv, http.MethodGet,
		"/api/v1/admin/role-bindings?sort_by=created&sort_order=asc&limit=100", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp listRoleBindingsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	idx := make(map[string]int)
	for i, item := range resp.Items {
		for label, b := range bindings {
			if item.ID == b.ID {
				idx[label] = i
			}
		}
	}

	// Bindings are created in order: alice, bob, charlie.
	assert.Less(t, idx["alice"], idx["bob"], "alice before bob in created asc")
	assert.Less(t, idx["bob"], idx["charlie"], "bob before charlie in created asc")
}

func TestRolesAPI_ListRoleBindings_SortByCreatedDesc(t *testing.T) {
	srv, _ := testServer(t)
	bindings := setupSortBindings(t, srv)

	rec := doRequest(t, srv, http.MethodGet,
		"/api/v1/admin/role-bindings?sort_by=created&sort_order=desc&limit=100", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp listRoleBindingsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	idx := make(map[string]int)
	for i, item := range resp.Items {
		for label, b := range bindings {
			if item.ID == b.ID {
				idx[label] = i
			}
		}
	}

	assert.Less(t, idx["charlie"], idx["bob"], "charlie before bob in created desc")
	assert.Less(t, idx["bob"], idx["alice"], "bob before alice in created desc")
}

func TestRolesAPI_ListRoleBindings_SortByRole(t *testing.T) {
	srv, _ := testServer(t)
	bindings := setupSortBindings(t, srv)

	rec := doRequest(t, srv, http.MethodGet,
		"/api/v1/admin/role-bindings?sort_by=role&sort_order=asc&limit=100", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp listRoleBindingsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	// Collect the role-definition IDs for our bindings in result order.
	idx := make(map[string]int)
	for i, item := range resp.Items {
		for label, b := range bindings {
			if item.ID == b.ID {
				idx[label] = i
			}
		}
	}
	require.Len(t, idx, 3, "all three bindings must appear")

	// With asc ordering on role_definition_id (UUIDs), we just need consistency:
	// the order is deterministic and the same three bindings always appear in the
	// same relative order.
	ids := []string{
		bindings["alice"].RoleDefinitionID,
		bindings["bob"].RoleDefinitionID,
		bindings["charlie"].RoleDefinitionID,
	}
	// Assert that the result order matches the sorted order of role def IDs.
	for i := 0; i < len(ids); i++ {
		for j := i + 1; j < len(ids); j++ {
			labelI := [3]string{"alice", "bob", "charlie"}[i]
			labelJ := [3]string{"alice", "bob", "charlie"}[j]
			if ids[i] < ids[j] {
				assert.Less(t, idx[labelI], idx[labelJ],
					"%s (role %s) should come before %s (role %s) in asc",
					labelI, ids[i], labelJ, ids[j])
			} else if ids[i] > ids[j] {
				assert.Greater(t, idx[labelI], idx[labelJ],
					"%s (role %s) should come after %s (role %s) in asc",
					labelI, ids[i], labelJ, ids[j])
			}
		}
	}
}

func TestRolesAPI_ListRoleBindings_SecondaryScopeOrdering(t *testing.T) {
	srv, _ := testServer(t)

	// Create two bindings with the same principal but different scopes.
	role := createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name:        "scope-order-role",
		ScopeType:   "project",
		Permissions: []string{"agent.read"},
	})

	bProj := createBindingViaAPI(t, srv, createRoleBindingRequest{
		RoleDefinitionID: role.ID,
		PrincipalType:    "user",
		PrincipalID:      "scope-user",
		ScopeType:        "project",
		ScopeID:          "proj-scope-1",
	})

	roleSys := createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name:        "scope-order-role-sys",
		ScopeType:   "system",
		Permissions: []string{"agent.read"},
	})
	bSys := createBindingViaAPI(t, srv, createRoleBindingRequest{
		RoleDefinitionID: roleSys.ID,
		PrincipalType:    "user",
		PrincipalID:      "scope-user",
		ScopeType:        "system",
	})

	// Sort by principal asc — both have "scope-user", so secondary sort by
	// scope_type (asc) kicks in: "project" < "system".
	rec := doRequest(t, srv, http.MethodGet,
		"/api/v1/admin/role-bindings?sort_by=principal&sort_order=asc&limit=100", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp listRoleBindingsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	idxProj, idxSys := -1, -1
	for i, item := range resp.Items {
		if item.ID == bProj.ID {
			idxProj = i
		}
		if item.ID == bSys.ID {
			idxSys = i
		}
	}
	require.NotEqual(t, -1, idxProj, "project binding must be in results")
	require.NotEqual(t, -1, idxSys, "system binding must be in results")
	assert.Less(t, idxProj, idxSys,
		"project scope should sort before system scope (secondary sort)")
}

func TestRolesAPI_ListRoleBindings_SortBeforePagination(t *testing.T) {
	srv, _ := testServer(t)

	// Create enough bindings to span two pages (page size = 1).
	role := createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name:        "page-sort-role",
		ScopeType:   "system",
		Permissions: []string{"agent.read"},
	})

	createBindingViaAPI(t, srv, createRoleBindingRequest{
		RoleDefinitionID: role.ID,
		PrincipalType:    "user",
		PrincipalID:      "page-user-b",
		ScopeType:        "system",
	})
	createBindingViaAPI(t, srv, createRoleBindingRequest{
		RoleDefinitionID: role.ID,
		PrincipalType:    "user",
		PrincipalID:      "page-user-a",
		ScopeType:        "system",
	})

	// Page 1 (limit=1, offset=0) sorted by principal asc.
	rec1 := doRequest(t, srv, http.MethodGet,
		"/api/v1/admin/role-bindings?sort_by=principal&sort_order=asc&limit=1&offset=0", nil)
	require.Equal(t, http.StatusOK, rec1.Code)
	var page1 listRoleBindingsResponse
	require.NoError(t, json.NewDecoder(rec1.Body).Decode(&page1))

	// Page 2 (limit=1, offset=1) sorted by principal asc.
	rec2 := doRequest(t, srv, http.MethodGet,
		"/api/v1/admin/role-bindings?sort_by=principal&sort_order=asc&limit=1&offset=1", nil)
	require.Equal(t, http.StatusOK, rec2.Code)
	var page2 listRoleBindingsResponse
	require.NoError(t, json.NewDecoder(rec2.Body).Decode(&page2))

	// Sorting happens before pagination, so the first page should contain
	// a binding whose principal_id is ≤ that of the second page.
	require.Len(t, page1.Items, 1)
	require.Len(t, page2.Items, 1)
	assert.LessOrEqual(t, page1.Items[0].PrincipalID, page2.Items[0].PrincipalID,
		"page 1 item should have a ≤ principalId compared to page 2 (sort before paginate)")
}

func TestRolesAPI_ListRoleBindings_InvalidSortBy(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodGet,
		"/api/v1/admin/role-bindings?sort_by=invalid", nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestRolesAPI_ListRoleBindings_InvalidSortOrder(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodGet,
		"/api/v1/admin/role-bindings?sort_order=sideways", nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestRolesAPI_ListRoleBindings_DefaultSortIsCreatedDesc(t *testing.T) {
	srv, _ := testServer(t)
	bindings := setupSortBindings(t, srv)

	// No sort params: should default to created desc.
	rec := doRequest(t, srv, http.MethodGet,
		"/api/v1/admin/role-bindings?limit=100", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp listRoleBindingsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	idx := make(map[string]int)
	for i, item := range resp.Items {
		for label, b := range bindings {
			if item.ID == b.ID {
				idx[label] = i
			}
		}
	}

	// Default is created desc: charlie (last created) should be first.
	assert.Less(t, idx["charlie"], idx["bob"], "charlie before bob in default sort")
	assert.Less(t, idx["bob"], idx["alice"], "bob before alice in default sort")
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
	srv, s := testServer(t)

	agentID := tid("rb-some-agent")
	projectID := tid("rb-agent-project")
	seedRolesTestAgent(t, s, agentID, projectID)

	role := createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name:        "agent-binding-role",
		ScopeType:   "project",
		Permissions: []string{"agent.read"},
	})

	binding := createBindingViaAPI(t, srv, createRoleBindingRequest{
		RoleDefinitionID: role.ID,
		PrincipalType:    "agent",
		PrincipalID:      agentID,
		ScopeType:        "project",
		ScopeID:          projectID,
	})

	assert.Equal(t, "agent", binding.PrincipalType)
	assert.Equal(t, agentID, binding.PrincipalID)
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

func TestRolesAPI_CreateRoleBinding_GroupPrincipal_BySlug(t *testing.T) {
	srv, s := testServer(t)
	ctx := t.Context()

	// Create a group with a known slug.
	require.NoError(t, s.CreateGroup(ctx, &store.Group{
		ID: tid("slug-resolve-group"), Slug: "my-team-slug", Name: "Slug Resolve Group",
	}))

	role := createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name:        "slug-resolve-role",
		ScopeType:   "system",
		Permissions: []string{"agent.read"},
	})

	// Use the slug as principalId — should resolve to the UUID.
	binding := createBindingViaAPI(t, srv, createRoleBindingRequest{
		RoleDefinitionID: role.ID,
		PrincipalType:    "group",
		PrincipalID:      "my-team-slug",
		ScopeType:        "system",
	})

	assert.Equal(t, "group", binding.PrincipalType)
	assert.Equal(t, tid("slug-resolve-group"), binding.PrincipalID, "principalId should be resolved to UUID")
}

func TestRolesAPI_CreateRoleBinding_GroupPrincipal_NeitherUUIDNorSlug(t *testing.T) {
	srv, _ := testServer(t)

	role := createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name:        "neither-uuid-nor-slug-role",
		ScopeType:   "system",
		Permissions: []string{"agent.read"},
	})

	// A non-UUID string that also doesn't match any slug.
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/role-bindings", createRoleBindingRequest{
		RoleDefinitionID: role.ID,
		PrincipalType:    "group",
		PrincipalID:      "nonexistent-slug",
		ScopeType:        "system",
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "group not found")
}

// ---------------------------------------------------------------------------
// Tests: Project Slug Resolution in Role Bindings
// ---------------------------------------------------------------------------

func TestRolesAPI_CreateRoleBinding_ProjectScope_UUIDUnchanged(t *testing.T) {
	srv, s := testServer(t)
	ctx := t.Context()

	// Create project with a known ID and slug.
	projectID := tid("proj-uuid-check")
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: projectID, Name: "UUID Check Project", Slug: "uuid-check-proj",
	}))

	role := createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name:        "proj-uuid-role",
		ScopeType:   "project",
		Permissions: []string{"agent.read"},
	})

	seedRolesTestUser(t, s, tid("proj-uuid-user"), "proj-uuid-user@test.com")

	// Use UUID as scopeId — should be accepted unchanged.
	binding := createBindingViaAPI(t, srv, createRoleBindingRequest{
		RoleDefinitionID: role.ID,
		PrincipalType:    "user",
		PrincipalID:      tid("proj-uuid-user"),
		ScopeType:        "project",
		ScopeID:          projectID,
	})

	assert.Equal(t, projectID, binding.ScopeID, "UUID scopeId should be stored unchanged")
}

func TestRolesAPI_CreateRoleBinding_ProjectScope_SlugResolvesToUUID(t *testing.T) {
	srv, s := testServer(t)
	ctx := t.Context()

	// Create project with a known ID and slug.
	projectID := tid("proj-slug-resolve")
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: projectID, Name: "Slug Resolve Project", Slug: "my-slug-project",
	}))

	role := createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name:        "proj-slug-role",
		ScopeType:   "project",
		Permissions: []string{"agent.read"},
	})

	seedRolesTestUser(t, s, tid("proj-slug-user"), "proj-slug-user@test.com")

	// Use slug as scopeId — should resolve to UUID.
	binding := createBindingViaAPI(t, srv, createRoleBindingRequest{
		RoleDefinitionID: role.ID,
		PrincipalType:    "user",
		PrincipalID:      tid("proj-slug-user"),
		ScopeType:        "project",
		ScopeID:          "my-slug-project",
	})

	assert.Equal(t, projectID, binding.ScopeID, "slug scopeId should be resolved to project UUID")
}

func TestRolesAPI_CreateRoleBinding_ProjectScope_MixedCaseSlug(t *testing.T) {
	srv, s := testServer(t)
	ctx := t.Context()

	// Create project with lowercase slug.
	projectID := tid("proj-mixcase")
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: projectID, Name: "Mixed Case Project", Slug: "my-mixed-project",
	}))

	role := createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name:        "proj-mixcase-role",
		ScopeType:   "project",
		Permissions: []string{"agent.read"},
	})

	seedRolesTestUser(t, s, tid("proj-mixcase-user"), "proj-mixcase@test.com")

	// Use mixed-case slug — should resolve case-insensitively to UUID.
	binding := createBindingViaAPI(t, srv, createRoleBindingRequest{
		RoleDefinitionID: role.ID,
		PrincipalType:    "user",
		PrincipalID:      tid("proj-mixcase-user"),
		ScopeType:        "project",
		ScopeID:          "My-Mixed-PROJECT",
	})

	assert.Equal(t, projectID, binding.ScopeID,
		"mixed-case slug should resolve case-insensitively to canonical UUID")
}

func TestRolesAPI_CreateRoleBinding_ProjectScope_UnknownSlug(t *testing.T) {
	srv, _ := testServer(t)

	role := createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name:        "proj-unknown-slug-role",
		ScopeType:   "project",
		Permissions: []string{"agent.read"},
	})

	// An unknown slug should return 400.
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/role-bindings", createRoleBindingRequest{
		RoleDefinitionID: role.ID,
		PrincipalType:    "user",
		PrincipalID:      "some-user",
		ScopeType:        "project",
		ScopeID:          "nonexistent-project-slug",
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "project not found with slug")
}

func TestRolesAPI_CreateRoleBinding_ProjectScope_AuthzSeesResolvedUUID(t *testing.T) {
	srv, s := testServer(t)
	ctx := t.Context()

	// Create project.
	projectID := tid("proj-authz-check")
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: projectID, Name: "Authz Check Project", Slug: "authz-check-proj",
	}))

	role := createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name:        "proj-authz-role",
		ScopeType:   "project",
		Permissions: []string{"agent.read"},
	})

	seedRolesTestUser(t, s, tid("proj-authz-user"), "proj-authz-user@test.com")

	// Submit binding with slug; verify stored ScopeID is the UUID.
	binding := createBindingViaAPI(t, srv, createRoleBindingRequest{
		RoleDefinitionID: role.ID,
		PrincipalType:    "user",
		PrincipalID:      tid("proj-authz-user"),
		ScopeType:        "project",
		ScopeID:          "authz-check-proj",
	})

	assert.Equal(t, projectID, binding.ScopeID, "stored ScopeID must be UUID, not slug")

	// Verify from the store directly that UUID was persisted.
	stored, err := s.GetRoleBinding(ctx, binding.ID)
	require.NoError(t, err)
	assert.Equal(t, projectID, stored.ScopeID, "store record must contain resolved UUID")
}

func TestRolesAPI_CreateRoleBinding_ProjectScope_NoPartialCreateOnLookupFailure(t *testing.T) {
	srv, s := testServer(t)
	ctx := t.Context()

	role := createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name:        "proj-no-partial-role",
		ScopeType:   "project",
		Permissions: []string{"agent.read"},
	})

	seedRolesTestUser(t, s, tid("proj-nopartial-user"), "proj-nopartial@test.com")

	// Attempt with nonexistent slug.
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/role-bindings", createRoleBindingRequest{
		RoleDefinitionID: role.ID,
		PrincipalType:    "user",
		PrincipalID:      tid("proj-nopartial-user"),
		ScopeType:        "project",
		ScopeID:          "ghost-project-slug",
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	// Verify no binding was created.
	bindings, err := s.ListRoleBindingsForPrincipal(ctx, "user", tid("proj-nopartial-user"))
	require.NoError(t, err)
	assert.Empty(t, bindings, "no binding should be created when project slug lookup fails")
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

	// Ensure the user exists in the store (CreateRoleBinding validates principal existence).
	seedRolesTestUser(t, st, nonAdminUserID, nonAdminUserEmail)

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

	otherUserID := tid("rb-other-user-held")
	seedRolesTestUser(t, st, otherUserID, "rb-other-user-held@test.local")

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
		PrincipalID:      otherUserID,
		ScopeType:        "system",
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
}

func TestRolesAPI_NonAdmin_CreateBinding_WithUnheldPermissions(t *testing.T) {
	srv, st := testServer(t)

	otherUserID := tid("rb-other-user-unheld")
	seedRolesTestUser(t, st, otherUserID, "rb-other-user-unheld@test.local")

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
		PrincipalID:      otherUserID,
		ScopeType:        "system",
	})
	assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
}

// ---------------------------------------------------------------------------
// Tests: Lifecycle fields (notBefore / expiresAt) — C9
// ---------------------------------------------------------------------------

func TestRolesAPI_CreateRoleBinding_WithLifecycleFields(t *testing.T) {
	srv, s := testServer(t)

	userID := tid("rb-lifecycle-user")
	seedRolesTestUser(t, s, userID, "rb-lifecycle-user@test.local")

	role := createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name:        "lifecycle-test-role",
		ScopeType:   "system",
		Permissions: []string{"agent.read"},
	})

	notBefore := time.Now().Add(1 * time.Hour).Truncate(time.Second).UTC()
	expiresAt := time.Now().Add(24 * time.Hour).Truncate(time.Second).UTC()

	// Create binding with lifecycle fields.
	binding := createBindingViaAPI(t, srv, createRoleBindingRequest{
		RoleDefinitionID: role.ID,
		PrincipalType:    "user",
		PrincipalID:      userID,
		ScopeType:        "system",
		NotBefore:        &notBefore,
		ExpiresAt:        &expiresAt,
	})

	assert.NotEmpty(t, binding.ID)
	require.NotNil(t, binding.NotBefore, "notBefore should be returned")
	require.NotNil(t, binding.ExpiresAt, "expiresAt should be returned")
	assert.True(t, binding.NotBefore.Equal(notBefore), "notBefore round-trip: want %v got %v", notBefore, *binding.NotBefore)
	assert.True(t, binding.ExpiresAt.Equal(expiresAt), "expiresAt round-trip: want %v got %v", expiresAt, *binding.ExpiresAt)
}

func TestRolesAPI_CreateRoleBinding_ExpiresAtInPast(t *testing.T) {
	srv, _ := testServer(t)

	role := createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name:        "past-expiry-role",
		ScopeType:   "system",
		Permissions: []string{"agent.read"},
	})

	past := time.Now().Add(-1 * time.Hour).UTC()
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/role-bindings", createRoleBindingRequest{
		RoleDefinitionID: role.ID,
		PrincipalType:    "user",
		PrincipalID:      DevUserID,
		ScopeType:        "system",
		ExpiresAt:        &past,
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "expiresAt must be in the future")
}

func TestRolesAPI_CreateRoleBinding_ExpiresAtBeforeNotBefore(t *testing.T) {
	srv, _ := testServer(t)

	role := createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name:        "bad-window-role",
		ScopeType:   "system",
		Permissions: []string{"agent.read"},
	})

	notBefore := time.Now().Add(24 * time.Hour).UTC()
	expiresAt := time.Now().Add(1 * time.Hour).UTC()
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/role-bindings", createRoleBindingRequest{
		RoleDefinitionID: role.ID,
		PrincipalType:    "user",
		PrincipalID:      DevUserID,
		ScopeType:        "system",
		NotBefore:        &notBefore,
		ExpiresAt:        &expiresAt,
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "expiresAt must be after notBefore")
}

func TestRolesAPI_CreateRoleBinding_WithOnlyNotBefore(t *testing.T) {
	srv, s := testServer(t)

	userID := tid("rb-notbefore-only-user")
	seedRolesTestUser(t, s, userID, "rb-notbefore-only-user@test.local")

	role := createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name:        "notbefore-only-role",
		ScopeType:   "system",
		Permissions: []string{"agent.read"},
	})

	notBefore := time.Now().Add(1 * time.Hour).Truncate(time.Second).UTC()
	binding := createBindingViaAPI(t, srv, createRoleBindingRequest{
		RoleDefinitionID: role.ID,
		PrincipalType:    "user",
		PrincipalID:      userID,
		ScopeType:        "system",
		NotBefore:        &notBefore,
	})

	require.NotNil(t, binding.NotBefore, "notBefore should be persisted")
	assert.Nil(t, binding.ExpiresAt, "expiresAt should be nil when not set")
}

func TestRolesAPI_CreateRoleBinding_WithOnlyExpiresAt(t *testing.T) {
	srv, s := testServer(t)

	userID := tid("rb-expiresat-only-user")
	seedRolesTestUser(t, s, userID, "rb-expiresat-only-user@test.local")

	role := createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name:        "expiresat-only-role",
		ScopeType:   "system",
		Permissions: []string{"agent.read"},
	})

	expiresAt := time.Now().Add(24 * time.Hour).Truncate(time.Second).UTC()
	binding := createBindingViaAPI(t, srv, createRoleBindingRequest{
		RoleDefinitionID: role.ID,
		PrincipalType:    "user",
		PrincipalID:      userID,
		ScopeType:        "system",
		ExpiresAt:        &expiresAt,
	})

	assert.Nil(t, binding.NotBefore, "notBefore should be nil when not set")
	require.NotNil(t, binding.ExpiresAt, "expiresAt should be persisted")
}

// ---------------------------------------------------------------------------
// Tests: Role Export / Import
// ---------------------------------------------------------------------------

func TestRolesAPI_ExportRoles_Empty(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/roles/export", nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp roleExportResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	assert.Equal(t, "1", resp.Version)
	assert.NotEmpty(t, resp.ExportedAt)
	assert.Empty(t, resp.Roles, "no custom roles should exist initially")
}

func TestRolesAPI_ExportRoles_ExcludesSystemRoles(t *testing.T) {
	srv, _ := testServer(t)

	// Create a custom role first
	createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name:        "export-test-custom",
		Description: "A custom role for export testing",
		ScopeType:   "system",
		Permissions: []string{"agent.read"},
	})

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/roles/export", nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp roleExportResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	assert.Equal(t, "1", resp.Version)
	require.Len(t, resp.Roles, 1, "only custom roles should be exported")
	assert.Equal(t, "export-test-custom", resp.Roles[0].Name)
	assert.Equal(t, "A custom role for export testing", resp.Roles[0].Description)
	assert.Equal(t, "system", resp.Roles[0].ScopeType)
	assert.Equal(t, []string{"agent.read"}, resp.Roles[0].Permissions)
}

func TestRolesAPI_ExportRoles_MethodNotAllowed(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/roles/export", nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestRolesAPI_ImportRoles_Basic(t *testing.T) {
	srv, _ := testServer(t)

	body := roleImportRequest{
		Version: "1",
		Roles: []exportedRole{
			{
				Name:        "imported-role-1",
				Description: "First imported role",
				ScopeType:   "system",
				Permissions: []string{"agent.read", "agent.list"},
			},
			{
				Name:        "imported-role-2",
				Description: "Second imported role",
				ScopeType:   "project",
				Permissions: []string{"agent.read"},
			},
		},
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/roles/import", body)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp roleImportResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	assert.Equal(t, 2, resp.Created)
	assert.Equal(t, 0, resp.Skipped)
	assert.Equal(t, 0, resp.Errors)
	require.Len(t, resp.Items, 2)
	assert.Equal(t, "created", resp.Items[0].Status)
	assert.NotEmpty(t, resp.Items[0].ID)
	assert.Equal(t, "created", resp.Items[1].Status)
	assert.NotEmpty(t, resp.Items[1].ID)
}

func TestRolesAPI_ImportRoles_SkipDuplicate(t *testing.T) {
	srv, _ := testServer(t)

	// Create a role first
	createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name:        "existing-role",
		ScopeType:   "system",
		Permissions: []string{"agent.read"},
	})

	// Try to import with a duplicate name
	body := roleImportRequest{
		Version: "1",
		Roles: []exportedRole{
			{
				Name:        "existing-role",
				ScopeType:   "system",
				Permissions: []string{"agent.read"},
			},
			{
				Name:        "new-role",
				ScopeType:   "system",
				Permissions: []string{"agent.read"},
			},
		},
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/roles/import", body)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp roleImportResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	assert.Equal(t, 1, resp.Created)
	assert.Equal(t, 1, resp.Skipped)
	assert.Equal(t, 0, resp.Errors)
	assert.Equal(t, "skipped", resp.Items[0].Status)
	assert.Contains(t, resp.Items[0].Reason, "already exists")
	assert.Equal(t, "created", resp.Items[1].Status)
}

func TestRolesAPI_ImportRoles_RejectSystemRoleName(t *testing.T) {
	srv, _ := testServer(t)

	body := roleImportRequest{
		Version: "1",
		Roles: []exportedRole{
			{
				Name:        "super-admin",
				ScopeType:   "system",
				Permissions: []string{"agent.read"},
			},
		},
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/roles/import", body)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp roleImportResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	assert.Equal(t, 0, resp.Created)
	assert.Equal(t, 1, resp.Errors)
	assert.Equal(t, "error", resp.Items[0].Status)
	assert.Contains(t, resp.Items[0].Reason, "system role")
}

func TestRolesAPI_ImportRoles_InvalidPermission(t *testing.T) {
	srv, _ := testServer(t)

	body := roleImportRequest{
		Version: "1",
		Roles: []exportedRole{
			{
				Name:        "bad-perms-role",
				ScopeType:   "system",
				Permissions: []string{"nonexistent.permission"},
			},
		},
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/roles/import", body)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp roleImportResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	assert.Equal(t, 0, resp.Created)
	assert.Equal(t, 1, resp.Errors)
	assert.Equal(t, "error", resp.Items[0].Status)
	assert.Contains(t, resp.Items[0].Reason, "invalid permission")
}

func TestRolesAPI_ImportRoles_InvalidVersion(t *testing.T) {
	srv, _ := testServer(t)

	body := roleImportRequest{
		Version: "99",
		Roles: []exportedRole{
			{
				Name:        "some-role",
				ScopeType:   "system",
				Permissions: []string{"agent.read"},
			},
		},
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/roles/import", body)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestRolesAPI_ImportRoles_EmptyRoles(t *testing.T) {
	srv, _ := testServer(t)

	body := roleImportRequest{
		Version: "1",
		Roles:   []exportedRole{},
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/roles/import", body)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestRolesAPI_ImportRoles_InvalidScopeType(t *testing.T) {
	srv, _ := testServer(t)

	body := roleImportRequest{
		Version: "1",
		Roles: []exportedRole{
			{
				Name:        "bad-scope-role",
				ScopeType:   "invalid",
				Permissions: []string{"agent.read"},
			},
		},
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/roles/import", body)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp roleImportResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	assert.Equal(t, 1, resp.Errors)
	assert.Equal(t, "error", resp.Items[0].Status)
	assert.Contains(t, resp.Items[0].Reason, "scopeType")
}

func TestRolesAPI_ImportRoles_EmptyName(t *testing.T) {
	srv, _ := testServer(t)

	body := roleImportRequest{
		Version: "1",
		Roles: []exportedRole{
			{
				Name:        "",
				ScopeType:   "system",
				Permissions: []string{"agent.read"},
			},
		},
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/roles/import", body)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp roleImportResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	assert.Equal(t, 1, resp.Errors)
	assert.Equal(t, "error", resp.Items[0].Status)
	assert.Contains(t, resp.Items[0].Reason, "name is required")
}

func TestRolesAPI_ImportRoles_MethodNotAllowed(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/roles/import", nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestRolesAPI_ExportImportRoundTrip(t *testing.T) {
	srv, _ := testServer(t)

	// Create some custom roles
	createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name:        "roundtrip-alpha",
		Description: "Alpha role",
		ScopeType:   "system",
		Permissions: []string{"agent.read", "agent.list"},
	})
	createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name:        "roundtrip-beta",
		Description: "Beta role",
		ScopeType:   "project",
		Permissions: []string{"agent.read"},
	})

	// Export
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/roles/export", nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var exported roleExportResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&exported))
	require.Len(t, exported.Roles, 2)

	// Import into a fresh server — simulate by using the exported payload
	// on the same server. The duplicate names will be skipped, but the
	// format is verified as correct.
	importBody := roleImportRequest{
		Version: exported.Version,
		Roles:   exported.Roles,
	}
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/admin/roles/import", importBody)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var imported roleImportResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&imported))

	// Both should be skipped since they already exist
	assert.Equal(t, 0, imported.Created)
	assert.Equal(t, 2, imported.Skipped)
	assert.Equal(t, 0, imported.Errors)
}

func TestRolesAPI_ImportRoles_AuthRequired(t *testing.T) {
	srv, _, _, member := setupScopedAdminTest(t)

	// A regular member should be denied at the route guard (role.read check)
	body := roleImportRequest{
		Version: "1",
		Roles: []exportedRole{
			{
				Name:        "member-import-attempt",
				ScopeType:   "system",
				Permissions: []string{"agent.read"},
			},
		},
	}

	rec := doRequestAsUser(t, srv, member, http.MethodPost, "/api/v1/admin/roles/import", body)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"regular member should not be able to import roles; body: %s", rec.Body.String())
}

// ---------------------------------------------------------------------------
// Tests: Duplicate Role Definition
// ---------------------------------------------------------------------------

func TestRolesAPI_DuplicateCustomRole(t *testing.T) {
	srv, _ := testServer(t)

	// Create a custom role to duplicate.
	source := createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name:        "dup-source-custom",
		Description: "Source custom role",
		ScopeType:   "project",
		Permissions: []string{"agent.read", "project.read"},
	})

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/roles/"+source.ID+"/duplicate", duplicateRoleDefinitionRequest{
		Name: "dup-target-custom",
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var dup store.RoleDefinition
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&dup))
	assert.NotEmpty(t, dup.ID)
	assert.NotEqual(t, source.ID, dup.ID, "duplicate should have a new ID")
	assert.Equal(t, "dup-target-custom", dup.Name)
	assert.Equal(t, source.Description, dup.Description)
	assert.Equal(t, source.ScopeType, dup.ScopeType)
	assert.Equal(t, source.Permissions, dup.Permissions)
	assert.False(t, dup.System, "duplicated role must be a custom role")
}

func TestRolesAPI_DuplicateSystemRole(t *testing.T) {
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

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/roles/"+systemRole.ID+"/duplicate", duplicateRoleDefinitionRequest{
		Name: "my-custom-" + systemRole.Name,
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var dup store.RoleDefinition
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&dup))
	assert.Equal(t, "my-custom-"+systemRole.Name, dup.Name)
	assert.Equal(t, systemRole.ScopeType, dup.ScopeType)
	assert.Equal(t, systemRole.Permissions, dup.Permissions)
	assert.False(t, dup.System, "duplicated system role must become a custom role")
}

func TestRolesAPI_DuplicateRole_MissingName(t *testing.T) {
	srv, _ := testServer(t)

	source := createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name:        "dup-missing-name-src",
		ScopeType:   "system",
		Permissions: []string{"agent.read"},
	})

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/roles/"+source.ID+"/duplicate", duplicateRoleDefinitionRequest{
		Name: "",
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "name is required")
}

func TestRolesAPI_DuplicateRole_NameConflict(t *testing.T) {
	srv, _ := testServer(t)

	source := createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name:        "dup-conflict-src",
		ScopeType:   "system",
		Permissions: []string{"agent.read"},
	})

	// First duplicate should succeed.
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/roles/"+source.ID+"/duplicate", duplicateRoleDefinitionRequest{
		Name: "dup-conflict-target",
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	// Second duplicate with the same name should conflict.
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/admin/roles/"+source.ID+"/duplicate", duplicateRoleDefinitionRequest{
		Name: "dup-conflict-target",
	})
	assert.Equal(t, http.StatusConflict, rec.Code)
}

func TestRolesAPI_DuplicateRole_SourceNotFound(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/roles/00000000-0000-0000-0000-000000000000/duplicate", duplicateRoleDefinitionRequest{
		Name: "orphan-dup",
	})
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestRolesAPI_DuplicateRole_MethodNotAllowed(t *testing.T) {
	srv, _ := testServer(t)

	source := createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name:        "dup-method-src",
		ScopeType:   "system",
		Permissions: []string{"agent.read"},
	})

	// GET on /duplicate should be rejected.
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/roles/"+source.ID+"/duplicate", nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestRolesAPI_NonAdmin_DuplicateRole_WithUnheldPermissions(t *testing.T) {
	srv, st := testServer(t)

	// Give non-admin user role.create and agent.read, but NOT user.suspend.
	user := setupNonAdminUser(t, st, []string{"role.create", "role.read", "agent.read"})

	// Create a role as admin that includes user.suspend.
	source := createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name:        "dup-escalation-src",
		Description: "Has unheld permissions",
		ScopeType:   "system",
		Permissions: []string{"agent.read", "user.suspend"},
	})

	// Non-admin tries to duplicate -> should get 403 (CanDelegate denies).
	rec := doRequestAsIdentity(t, srv, user, http.MethodPost, "/api/v1/admin/roles/"+source.ID+"/duplicate", duplicateRoleDefinitionRequest{
		Name: "dup-escalation-target",
	})
	assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
}

func TestRolesAPI_NonAdmin_DuplicateRole_WithHeldPermissions(t *testing.T) {
	srv, st := testServer(t)

	// Give non-admin user role.create and agent.read.
	user := setupNonAdminUser(t, st, []string{"role.create", "role.read", "agent.read"})

	// Create a role as admin with only agent.read.
	source := createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name:        "dup-held-src",
		Description: "Has only held permissions",
		ScopeType:   "system",
		Permissions: []string{"agent.read"},
	})

	// Non-admin duplicates -> should succeed.
	rec := doRequestAsIdentity(t, srv, user, http.MethodPost, "/api/v1/admin/roles/"+source.ID+"/duplicate", duplicateRoleDefinitionRequest{
		Name: "dup-held-target",
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var dup store.RoleDefinition
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&dup))
	assert.Equal(t, "dup-held-target", dup.Name)
	assert.False(t, dup.System)
}

// ---------------------------------------------------------------------------
// Tests: Agent principal scope constraints
// ---------------------------------------------------------------------------

func TestRolesAPI_CreateRoleBinding_AgentSystemScope_Rejected(t *testing.T) {
	srv, s := testServer(t)

	agentID := tid("rb-agent-sys")
	projectID := tid("rb-agent-sys-proj")
	seedRolesTestAgent(t, s, agentID, projectID)

	// Create a system-scoped custom role.
	role := createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name:        "agent-sys-scope-test",
		ScopeType:   "system",
		Permissions: []string{"agent.read"},
	})

	// Agent + system scope should be rejected.
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/role-bindings", createRoleBindingRequest{
		RoleDefinitionID: role.ID,
		PrincipalType:    "agent",
		PrincipalID:      agentID,
		ScopeType:        "system",
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "project-bound")
}

func TestRolesAPI_CreateRoleBinding_AgentProjectScope_Allowed(t *testing.T) {
	srv, s := testServer(t)

	agentID := tid("rb-agent-proj")
	projectID := tid("rb-agent-proj-proj")
	seedRolesTestAgent(t, s, agentID, projectID)

	// Create a project-scoped custom role.
	role := createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name:        "agent-proj-scope-test",
		ScopeType:   "project",
		Permissions: []string{"agent.read"},
	})

	// Agent + project scope should succeed.
	binding := createBindingViaAPI(t, srv, createRoleBindingRequest{
		RoleDefinitionID: role.ID,
		PrincipalType:    "agent",
		PrincipalID:      agentID,
		ScopeType:        "project",
		ScopeID:          projectID,
	})
	assert.Equal(t, "agent", binding.PrincipalType)
	assert.Equal(t, "project", binding.ScopeType)
}

// ---------------------------------------------------------------------------
// R6: Generic DELETE /api/v1/admin/role-bindings/{id} super-admin guards
// ---------------------------------------------------------------------------

// createSuperAdminBindingDirect creates a super-admin binding via the store
// directly (bypassing the API's D10 guard), and returns the binding.
func createSuperAdminBindingDirect(t *testing.T, s store.Store, userID string) *store.RoleBinding {
	t.Helper()
	ctx := context.Background()
	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	b, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
		ScopeID:          "",
		CreatedBy:        store.SystemReconcileCreatedBy,
	})
	require.NoError(t, err)
	return b
}

// TestGenericDeleteBinding_SuperAdmin_SelfLockout verifies that a super-admin
// cannot delete their own super-admin binding via the generic DELETE endpoint.
func TestGenericDeleteBinding_SuperAdmin_SelfLockout(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	devUser := getDevUser(t, srv, s)
	srv.ensureSuperAdminBinding(ctx, devUser.ID)

	// Find dev user's super-admin binding.
	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, devUser.ID)
	require.NoError(t, err)
	var saBinding *store.RoleBinding
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeSystem && b.RoleDefinitionID == rd.ID {
			saBinding = b
			break
		}
	}
	require.NotNil(t, saBinding, "dev user should have super-admin binding")

	// Try to delete own super-admin binding: should be rejected as self-lockout.
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/admin/role-bindings/"+saBinding.ID, nil)
	assert.Equal(t, http.StatusConflict, rec.Code,
		"deleting own super-admin binding should be rejected as self-lockout")
	assert.Contains(t, rec.Body.String(), "self_lockout")
}

// TestGenericDeleteBinding_SuperAdmin_LastAdmin verifies that deleting the
// last active super-admin binding (of another user) is blocked when no other
// active admins survive.
func TestGenericDeleteBinding_SuperAdmin_LastAdmin(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	devUser := getDevUser(t, srv, s)
	srv.ensureSuperAdminBinding(ctx, devUser.ID)

	// Create a second admin.
	targetID := tid("ga-sole-target")
	seedRolesTestUser(t, s, targetID, "gasole@test.local")
	targetBinding := createSuperAdminBindingDirect(t, s, targetID)

	// Suspend the dev user so they don't count as an active admin.
	// (checkLastSuperAdminTx only counts active users.)
	devUser.Status = "suspended"
	require.NoError(t, s.UpdateUser(ctx, devUser))

	// Now try to delete target's binding. Target is the only active admin.
	// Dev can still make the request (auth middleware uses identity, not user status
	// for existing sessions), but the guard should see zero surviving active admins.
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/admin/role-bindings/"+targetBinding.ID, nil)
	assert.Equal(t, http.StatusConflict, rec.Code,
		"deleting last active admin's binding should be blocked")
	assert.Contains(t, rec.Body.String(), "last_admin")

	// Re-activate dev for cleanup.
	devUser.Status = "active"
	_ = s.UpdateUser(ctx, devUser)
}

// TestGenericDeleteBinding_SuperAdmin_AllowedWithSurvivor verifies that
// deleting a super-admin binding succeeds when another active admin exists.
func TestGenericDeleteBinding_SuperAdmin_AllowedWithSurvivor(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	devUser := getDevUser(t, srv, s)
	srv.ensureSuperAdminBinding(ctx, devUser.ID)

	// Create a target with a super-admin binding.
	targetID := tid("ga-del-ok")
	seedRolesTestUser(t, s, targetID, "gadelok@test.local")
	targetBinding := createSuperAdminBindingDirect(t, s, targetID)

	// Dev is still active admin, so deleting target's binding is allowed.
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/admin/role-bindings/"+targetBinding.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code,
		"deleting super-admin binding should succeed when another admin survives")

	// Verify the binding is actually gone.
	rd, _ := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	bindings, _ := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, targetID)
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeSystem && b.RoleDefinitionID == rd.ID {
			t.Fatal("super-admin binding should have been deleted")
		}
	}
}

// TestGenericDeleteBinding_SuperAdmin_AuditRecorded verifies that deleting a
// super-admin binding via the generic DELETE endpoint produces an audit record.
func TestGenericDeleteBinding_SuperAdmin_AuditRecorded(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	devUser := getDevUser(t, srv, s)
	srv.ensureSuperAdminBinding(ctx, devUser.ID)

	targetID := tid("ga-audit-target")
	seedRolesTestUser(t, s, targetID, "gaaudit@test.local")
	targetBinding := createSuperAdminBindingDirect(t, s, targetID)

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/admin/role-bindings/"+targetBinding.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// Verify audit record exists.
	audits, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{
		TargetType: "role_binding",
		TargetID:   targetBinding.ID,
		Limit:      10,
	})
	require.NoError(t, err)
	found := false
	for _, a := range audits {
		if a.MutationType == "role_binding_delete" {
			found = true
			assert.Contains(t, a.BeforeSummary, "super-admin")
			assert.Contains(t, a.AfterSummary, "generic_delete_endpoint")
		}
	}
	assert.True(t, found, "deletion of super-admin binding via generic endpoint should produce audit record")
}

// TestGenericDeleteBinding_NonSuperAdmin_StillWorks verifies that deleting a
// non-super-admin system-scoped binding still works without the super-admin guards.
func TestGenericDeleteBinding_NonSuperAdmin_StillWorks(t *testing.T) {
	srv, s := testServer(t)

	userID := tid("ga-nonsuperadmin")
	seedRolesTestUser(t, s, userID, "ganonsuperadmin@test.local")

	role := createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name:        "ga-test-role",
		ScopeType:   "system",
		Permissions: []string{"agent.read"},
	})
	binding := createBindingViaAPI(t, srv, createRoleBindingRequest{
		RoleDefinitionID: role.ID,
		PrincipalType:    "user",
		PrincipalID:      userID,
		ScopeType:        "system",
	})

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/admin/role-bindings/"+binding.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code,
		"non-super-admin binding deletion should work normally")
}

// ---------------------------------------------------------------------------
// R6: Credential boundary on generic DELETE super-admin bindings
// ---------------------------------------------------------------------------

// doDeleteBindingWithCredentialKind creates a request to the generic DELETE
// role-bindings endpoint with a specific credential kind injected directly
// into the context, bypassing the auth middleware.
func doDeleteBindingWithCredentialKind(
	t *testing.T, srv *Server, user *store.User,
	credKind CredentialKind, bindingID string,
) *httptest.ResponseRecorder {
	t.Helper()

	path := "/api/v1/admin/role-bindings/" + bindingID
	req := httptest.NewRequest(http.MethodDelete, path, nil)

	ctx := req.Context()
	identity := NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, "web")
	ctx = contextWithIdentity(ctx, identity)
	ctx = contextWithCredentialContext(ctx, CredentialContext{Kind: credKind})
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	srv.handleAdminRoleBindingByID(rec, req)
	return rec
}

// TestGenericDeleteBinding_SuperAdmin_BrokerDenied verifies that broker
// credentials cannot delete super-admin bindings via the generic endpoint.
func TestGenericDeleteBinding_SuperAdmin_BrokerDenied(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	devUser := getDevUser(t, srv, s)
	srv.ensureSuperAdminBinding(ctx, devUser.ID)

	targetID := tid("ga-cred-broker")
	seedRolesTestUser(t, s, targetID, "gacredbroker@test.local")
	targetBinding := createSuperAdminBindingDirect(t, s, targetID)

	rec := doDeleteBindingWithCredentialKind(t, srv, devUser, CredentialKindBroker, targetBinding.ID)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"broker credential should be denied for super-admin binding deletion")
}

// TestGenericDeleteBinding_SuperAdmin_AgentJWTDenied verifies that agent JWT
// credentials cannot delete super-admin bindings.
func TestGenericDeleteBinding_SuperAdmin_AgentJWTDenied(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	devUser := getDevUser(t, srv, s)
	srv.ensureSuperAdminBinding(ctx, devUser.ID)

	targetID := tid("ga-cred-agent")
	seedRolesTestUser(t, s, targetID, "gacredagent@test.local")
	targetBinding := createSuperAdminBindingDirect(t, s, targetID)

	rec := doDeleteBindingWithCredentialKind(t, srv, devUser, CredentialKindAgentJWT, targetBinding.ID)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"agent JWT credential should be denied for super-admin binding deletion")
}

// TestGenericDeleteBinding_SuperAdmin_UATDenied verifies that UAT credentials
// cannot delete super-admin bindings.
func TestGenericDeleteBinding_SuperAdmin_UATDenied(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	devUser := getDevUser(t, srv, s)
	srv.ensureSuperAdminBinding(ctx, devUser.ID)

	targetID := tid("ga-cred-uat")
	seedRolesTestUser(t, s, targetID, "gacreduat@test.local")
	targetBinding := createSuperAdminBindingDirect(t, s, targetID)

	rec := doDeleteBindingWithCredentialKind(t, srv, devUser, CredentialKindUAT, targetBinding.ID)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"UAT credential should be denied for super-admin binding deletion")
}

// TestGenericDeleteBinding_SuperAdmin_DevAllowed verifies that dev credentials
// CAN delete super-admin bindings (positive control).
func TestGenericDeleteBinding_SuperAdmin_DevAllowed(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	devUser := getDevUser(t, srv, s)
	srv.ensureSuperAdminBinding(ctx, devUser.ID)

	targetID := tid("ga-cred-dev-ok")
	seedRolesTestUser(t, s, targetID, "gacreddevok@test.local")
	targetBinding := createSuperAdminBindingDirect(t, s, targetID)

	rec := doDeleteBindingWithCredentialKind(t, srv, devUser, CredentialKindDev, targetBinding.ID)
	assert.Equal(t, http.StatusNoContent, rec.Code,
		"dev credential should be allowed for super-admin binding deletion")
}

// TestGenericDeleteBinding_SuperAdmin_InteractiveAllowed verifies that
// interactive session credentials CAN delete super-admin bindings.
func TestGenericDeleteBinding_SuperAdmin_InteractiveAllowed(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	devUser := getDevUser(t, srv, s)
	srv.ensureSuperAdminBinding(ctx, devUser.ID)

	targetID := tid("ga-cred-int-ok")
	seedRolesTestUser(t, s, targetID, "gacredintok@test.local")
	targetBinding := createSuperAdminBindingDirect(t, s, targetID)

	rec := doDeleteBindingWithCredentialKind(t, srv, devUser, CredentialKindInteractive, targetBinding.ID)
	assert.Equal(t, http.StatusNoContent, rec.Code,
		"interactive credential should be allowed for super-admin binding deletion")

	// Verify the binding is actually gone.
	rd, _ := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	bindings, _ := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, targetID)
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeSystem && b.RoleDefinitionID == rd.ID {
			t.Fatal("super-admin binding should have been deleted by interactive credential")
		}
	}
}

// TestGenericDeleteBinding_SuperAdmin_FederationDenied verifies that
// federation credentials cannot delete super-admin bindings via the generic
// endpoint.
func TestGenericDeleteBinding_SuperAdmin_FederationDenied(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	devUser := getDevUser(t, srv, s)
	srv.ensureSuperAdminBinding(ctx, devUser.ID)

	targetID := tid("ga-cred-fed")
	seedRolesTestUser(t, s, targetID, "gacredfed@test.local")
	targetBinding := createSuperAdminBindingDirect(t, s, targetID)

	rec := doDeleteBindingWithCredentialKind(t, srv, devUser, CredentialKindFederation, targetBinding.ID)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"federation credential should be denied for super-admin binding deletion")

	// Verify binding was NOT mutated.
	rd, _ := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	bindings, _ := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, targetID)
	found := false
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeSystem && b.RoleDefinitionID == rd.ID {
			found = true
		}
	}
	assert.True(t, found, "super-admin binding must survive denied federation credential")
}

// TestGenericDeleteBinding_SuperAdmin_MissingCredentialDenied verifies that a
// request with no CredentialContext set (zero-value Kind) is denied. This
// covers legacy callers or test contexts that set only an identity.
func TestGenericDeleteBinding_SuperAdmin_MissingCredentialDenied(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	devUser := getDevUser(t, srv, s)
	srv.ensureSuperAdminBinding(ctx, devUser.ID)

	targetID := tid("ga-cred-none")
	seedRolesTestUser(t, s, targetID, "gacrednone@test.local")
	targetBinding := createSuperAdminBindingDirect(t, s, targetID)

	// Build a request with identity but NO credential context.
	path := "/api/v1/admin/role-bindings/" + targetBinding.ID
	req := httptest.NewRequest(http.MethodDelete, path, nil)
	reqCtx := req.Context()
	identity := NewAuthenticatedUser(devUser.ID, devUser.Email, devUser.DisplayName, devUser.Role, "web")
	reqCtx = contextWithIdentity(reqCtx, identity)
	// Deliberately omit contextWithCredentialContext.
	req = req.WithContext(reqCtx)

	rec := httptest.NewRecorder()
	srv.handleAdminRoleBindingByID(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"missing credential context should be denied for super-admin binding deletion")

	// Verify binding was NOT mutated.
	rd, _ := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	bindings, _ := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, targetID)
	found := false
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeSystem && b.RoleDefinitionID == rd.ID {
			found = true
		}
	}
	assert.True(t, found, "super-admin binding must survive missing credential context")
}

// ---------------------------------------------------------------------------
// Tests: Role Binding by-ID structured denial (method-aware gate)
//
// These tests verify that the by-ID route (/api/v1/admin/role-bindings/{id})
// returns structured 403 responses with resource_type and denied_action when
// a hub-member without role_binding permissions attempts operations, rather
// than a plain 403 from the route guard.
// ---------------------------------------------------------------------------

func TestRoleBindingByID_MemberDelete_StructuredDenial(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a hub-member user with no role_binding permissions.
	member := &store.User{
		ID:          tid("rb-byid-member-del"),
		Email:       "rb-byid-member-del@test.local",
		DisplayName: "Member Del",
		Role:        "member",
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, member))

	// The binding ID doesn't need to exist — authorization runs before lookup.
	rec := doRequestAsUser(t, srv, member, http.MethodDelete,
		"/api/v1/admin/role-bindings/00000000-0000-0000-0000-000000000001", nil)

	assert.Equal(t, http.StatusForbidden, rec.Code)
	resp := parseErrorResponse(t, rec.Body.Bytes())
	assertStructuredDenial(t, resp, "role_binding", "delete")
}

func TestRoleBindingByID_MemberGetUserBindings_StructuredDenial(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a hub-member user with no role_binding permissions.
	member := &store.User{
		ID:          tid("rb-byid-member-get"),
		Email:       "rb-byid-member-get@test.local",
		DisplayName: "Member Get",
		Role:        "member",
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, member))

	// Create a target user with a binding — verify no data leaks on denial.
	targetUserID := tid("rb-byid-target-user")
	seedRolesTestUser(t, s, targetUserID, "rb-byid-target@test.local")

	// Create a binding for the target user (via super-admin).
	role := createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name:        "byid-denial-role",
		ScopeType:   "system",
		Permissions: []string{"agent.read"},
	})
	createBindingViaAPI(t, srv, createRoleBindingRequest{
		RoleDefinitionID: role.ID,
		PrincipalType:    "user",
		PrincipalID:      targetUserID,
		ScopeType:        "system",
	})

	// Hub-member requests the target's bindings — should get 403, not data.
	rec := doRequestAsUser(t, srv, member, http.MethodGet,
		"/api/v1/admin/role-bindings/user/"+targetUserID, nil)

	assert.Equal(t, http.StatusForbidden, rec.Code)
	resp := parseErrorResponse(t, rec.Body.Bytes())
	assertStructuredDenial(t, resp, "role_binding", "read")

	// Ensure no binding data leaks in the response body.
	assert.NotContains(t, rec.Body.String(), targetUserID,
		"response must not leak target user ID")
	assert.NotContains(t, rec.Body.String(), role.ID,
		"response must not leak role definition ID")
}

func TestRoleBindingByID_Unauthenticated_GET_Returns401(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequestNoAuth(t, srv, http.MethodGet,
		"/api/v1/admin/role-bindings/user/some-user-id", nil)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestRoleBindingByID_Unauthenticated_DELETE_Returns401(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequestNoAuth(t, srv, http.MethodDelete,
		"/api/v1/admin/role-bindings/00000000-0000-0000-0000-000000000001", nil)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestRoleBindingByID_AuthorizedDelete_Succeeds(t *testing.T) {
	srv, s := testServer(t)

	delUserID := tid("rb-byid-auth-del-user")
	seedRolesTestUser(t, s, delUserID, "rb-byid-auth-del@test.local")

	// Create a custom role and binding.
	role := createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name:        "byid-auth-del-role",
		ScopeType:   "system",
		Permissions: []string{"agent.read"},
	})
	binding := createBindingViaAPI(t, srv, createRoleBindingRequest{
		RoleDefinitionID: role.ID,
		PrincipalType:    "user",
		PrincipalID:      delUserID,
		ScopeType:        "system",
	})

	// Super-admin (dev token) can delete.
	rec := doRequest(t, srv, http.MethodDelete,
		"/api/v1/admin/role-bindings/"+binding.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestRoleBindingByID_AuthorizedGetUserBindings_Succeeds(t *testing.T) {
	srv, s := testServer(t)

	targetUserID := tid("rb-byid-auth-get-user")
	seedRolesTestUser(t, s, targetUserID, "rb-byid-auth-get@test.local")

	// Create a binding for the target user.
	role := createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name:        "byid-auth-get-role",
		ScopeType:   "system",
		Permissions: []string{"agent.read"},
	})
	createBindingViaAPI(t, srv, createRoleBindingRequest{
		RoleDefinitionID: role.ID,
		PrincipalType:    "user",
		PrincipalID:      targetUserID,
		ScopeType:        "system",
	})

	// Super-admin (dev token) can read.
	rec := doRequest(t, srv, http.MethodGet,
		"/api/v1/admin/role-bindings/user/"+targetUserID, nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp listRoleBindingsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.GreaterOrEqual(t, resp.TotalCount, 1)
}
