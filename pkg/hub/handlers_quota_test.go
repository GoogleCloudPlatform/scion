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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// createLimitViaAPI creates a limit definition through the handler and returns it.
func createLimitViaAPI(t *testing.T, srv *Server, req createLimitDefinitionRequest) *store.LimitDefinition {
	t.Helper()
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/limits", req)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	var def store.LimitDefinition
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&def))
	return &def
}

// createEntitlementViaAPI creates an entitlement binding through the handler.
func createEntitlementViaAPI(t *testing.T, srv *Server, limitID string, req createEntitlementBindingRequest) *store.EntitlementBinding {
	t.Helper()
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/limits/"+limitID+"/entitlements", req)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	var binding store.EntitlementBinding
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&binding))
	return &binding
}

// ---------------------------------------------------------------------------
// Tests: Limit Definition CRUD
// ---------------------------------------------------------------------------

func TestQuotaAPI_CreateLimitDefinition(t *testing.T) {
	srv, _ := testServer(t)

	def := createLimitViaAPI(t, srv, createLimitDefinitionRequest{
		Name:         "test_limit",
		ResourceType: "agent",
		Unit:         "count",
		Description:  "Test limit",
		DefaultValue: 10,
	})

	assert.NotEmpty(t, def.ID)
	assert.Equal(t, "test_limit", def.Name)
	assert.Equal(t, "agent", def.ResourceType)
	assert.Equal(t, "count", def.Unit)
	assert.Equal(t, int64(10), def.DefaultValue)
	assert.False(t, def.System)
}

func TestQuotaAPI_CreateLimitDefinition_MissingName(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/limits", createLimitDefinitionRequest{
		ResourceType: "agent",
		Unit:         "count",
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestQuotaAPI_CreateLimitDefinition_DuplicateName(t *testing.T) {
	srv, _ := testServer(t)

	createLimitViaAPI(t, srv, createLimitDefinitionRequest{
		Name:         "duplicate_limit",
		ResourceType: "agent",
		Unit:         "count",
		DefaultValue: 5,
	})

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/limits", createLimitDefinitionRequest{
		Name:         "duplicate_limit",
		ResourceType: "agent",
		Unit:         "count",
		DefaultValue: 10,
	})
	assert.Equal(t, http.StatusConflict, rec.Code)
}

func TestQuotaAPI_GetLimitDefinition(t *testing.T) {
	srv, _ := testServer(t)

	created := createLimitViaAPI(t, srv, createLimitDefinitionRequest{
		Name:         "get_limit",
		ResourceType: "project",
		Unit:         "count",
		Description:  "Get test",
		DefaultValue: 5,
	})

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/limits/"+created.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var def store.LimitDefinition
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&def))
	assert.Equal(t, created.ID, def.ID)
	assert.Equal(t, "get_limit", def.Name)
}

func TestQuotaAPI_GetLimitDefinition_NotFound(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/limits/"+tid("nonexistent"), nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestQuotaAPI_ListLimitDefinitions(t *testing.T) {
	srv, _ := testServer(t)

	createLimitViaAPI(t, srv, createLimitDefinitionRequest{
		Name: "list_limit_1", ResourceType: "agent", Unit: "count", DefaultValue: 5,
	})
	createLimitViaAPI(t, srv, createLimitDefinitionRequest{
		Name: "list_limit_2", ResourceType: "project", Unit: "count", DefaultValue: 10,
	})

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/limits", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp listLimitDefinitionsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.GreaterOrEqual(t, resp.TotalCount, 2)
}

func TestQuotaAPI_UpdateLimitDefinition(t *testing.T) {
	srv, _ := testServer(t)

	created := createLimitViaAPI(t, srv, createLimitDefinitionRequest{
		Name: "update_limit", ResourceType: "agent", Unit: "count", DefaultValue: 5,
	})

	rec := doRequest(t, srv, http.MethodPut, "/api/v1/admin/limits/"+created.ID, updateLimitDefinitionRequest{
		Name:         "updated_limit",
		ResourceType: "agent",
		Unit:         "count",
		Description:  "Updated description",
		DefaultValue: 20,
	})
	require.Equal(t, http.StatusOK, rec.Code)

	var updated store.LimitDefinition
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&updated))
	assert.Equal(t, "updated_limit", updated.Name)
	assert.Equal(t, int64(20), updated.DefaultValue)
	assert.Equal(t, "Updated description", updated.Description)
}

func TestQuotaAPI_UpdateLimitDefinition_NotFound(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodPut, "/api/v1/admin/limits/"+tid("nonexistent"), updateLimitDefinitionRequest{
		Name: "nope", ResourceType: "agent", Unit: "count",
	})
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestQuotaAPI_DeleteLimitDefinition(t *testing.T) {
	srv, _ := testServer(t)

	created := createLimitViaAPI(t, srv, createLimitDefinitionRequest{
		Name: "delete_limit", ResourceType: "agent", Unit: "count", DefaultValue: 5,
	})

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/admin/limits/"+created.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// Verify it's gone.
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/admin/limits/"+created.ID, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestQuotaAPI_DeleteLimitDefinition_SystemSeeded(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a system-seeded limit directly in the store.
	systemDef, err := s.CreateLimitDefinition(ctx, &store.LimitDefinition{
		Name:         "system_limit",
		ResourceType: "agent",
		Unit:         "count",
		DefaultValue: 100,
		System:       true,
	})
	require.NoError(t, err)

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/admin/limits/"+systemDef.ID, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code)

	// Verify it still exists.
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/admin/limits/"+systemDef.ID, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestQuotaAPI_DeleteLimitDefinition_NotFound(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/admin/limits/"+tid("nonexistent"), nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// ---------------------------------------------------------------------------
// Tests: Entitlement Binding CRUD
// ---------------------------------------------------------------------------

func TestQuotaAPI_CreateEntitlement(t *testing.T) {
	srv, _ := testServer(t)

	limit := createLimitViaAPI(t, srv, createLimitDefinitionRequest{
		Name: "ent_create_limit", ResourceType: "agent", Unit: "count", DefaultValue: 5,
	})

	binding := createEntitlementViaAPI(t, srv, limit.ID, createEntitlementBindingRequest{
		SubjectType: store.EntitlementSubjectUser,
		SubjectID:   "user-1",
		ScopeType:   store.QuotaScopeSystem,
		ScopeID:     "",
		Value:       10,
	})

	assert.NotEmpty(t, binding.ID)
	assert.Equal(t, limit.ID, binding.LimitDefinitionID)
	assert.Equal(t, store.EntitlementSubjectUser, binding.SubjectType)
	assert.Equal(t, "user-1", binding.SubjectID)
	assert.Equal(t, int64(10), binding.Value)
}

func TestQuotaAPI_CreateEntitlement_LimitNotFound(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/limits/"+tid("nonexistent")+"/entitlements", createEntitlementBindingRequest{
		SubjectType: store.EntitlementSubjectUser,
		SubjectID:   "user-1",
		ScopeType:   store.QuotaScopeSystem,
		Value:       10,
	})
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestQuotaAPI_GetEntitlement(t *testing.T) {
	srv, _ := testServer(t)

	limit := createLimitViaAPI(t, srv, createLimitDefinitionRequest{
		Name: "ent_get_limit", ResourceType: "agent", Unit: "count", DefaultValue: 5,
	})
	binding := createEntitlementViaAPI(t, srv, limit.ID, createEntitlementBindingRequest{
		SubjectType: store.EntitlementSubjectUser,
		SubjectID:   "user-1",
		ScopeType:   store.QuotaScopeSystem,
		Value:       10,
	})

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/entitlements/"+binding.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var got store.EntitlementBinding
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	assert.Equal(t, binding.ID, got.ID)
}

func TestQuotaAPI_GetEntitlement_NotFound(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/entitlements/"+tid("nonexistent"), nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestQuotaAPI_ListEntitlements(t *testing.T) {
	srv, _ := testServer(t)

	limit := createLimitViaAPI(t, srv, createLimitDefinitionRequest{
		Name: "ent_list_limit", ResourceType: "agent", Unit: "count", DefaultValue: 5,
	})
	createEntitlementViaAPI(t, srv, limit.ID, createEntitlementBindingRequest{
		SubjectType: store.EntitlementSubjectUser,
		SubjectID:   "user-1",
		ScopeType:   store.QuotaScopeSystem,
		Value:       10,
	})
	createEntitlementViaAPI(t, srv, limit.ID, createEntitlementBindingRequest{
		SubjectType: store.EntitlementSubjectGroup,
		SubjectID:   "group-1",
		ScopeType:   store.QuotaScopeSystem,
		Value:       20,
	})

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/limits/"+limit.ID+"/entitlements", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp listEntitlementBindingsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, 2, resp.TotalCount)
}

func TestQuotaAPI_UpdateEntitlement(t *testing.T) {
	srv, _ := testServer(t)

	limit := createLimitViaAPI(t, srv, createLimitDefinitionRequest{
		Name: "ent_update_limit", ResourceType: "agent", Unit: "count", DefaultValue: 5,
	})
	binding := createEntitlementViaAPI(t, srv, limit.ID, createEntitlementBindingRequest{
		SubjectType: store.EntitlementSubjectUser,
		SubjectID:   "user-1",
		ScopeType:   store.QuotaScopeSystem,
		Value:       10,
	})

	rec := doRequest(t, srv, http.MethodPut, "/api/v1/admin/entitlements/"+binding.ID, updateEntitlementBindingRequest{
		SubjectType: store.EntitlementSubjectUser,
		SubjectID:   "user-1",
		ScopeType:   store.QuotaScopeSystem,
		Value:       50,
	})
	require.Equal(t, http.StatusOK, rec.Code)

	var updated store.EntitlementBinding
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&updated))
	assert.Equal(t, int64(50), updated.Value)
}

func TestQuotaAPI_DeleteEntitlement(t *testing.T) {
	srv, _ := testServer(t)

	limit := createLimitViaAPI(t, srv, createLimitDefinitionRequest{
		Name: "ent_delete_limit", ResourceType: "agent", Unit: "count", DefaultValue: 5,
	})
	binding := createEntitlementViaAPI(t, srv, limit.ID, createEntitlementBindingRequest{
		SubjectType: store.EntitlementSubjectUser,
		SubjectID:   "user-1",
		ScopeType:   store.QuotaScopeSystem,
		Value:       10,
	})

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/admin/entitlements/"+binding.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// Verify it's gone.
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/admin/entitlements/"+binding.ID, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// ---------------------------------------------------------------------------
// Tests: Usage Queries
// ---------------------------------------------------------------------------

func TestQuotaAPI_GetUsageSummary(t *testing.T) {
	srv, _ := testServer(t)

	createLimitViaAPI(t, srv, createLimitDefinitionRequest{
		Name: "usage_summary_limit", ResourceType: "agent", Unit: "count", DefaultValue: 5,
	})

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/usage", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp usageSummaryResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.NotEmpty(t, resp.Items)
}

func TestQuotaAPI_GetUsageByLimit(t *testing.T) {
	srv, _ := testServer(t)

	limit := createLimitViaAPI(t, srv, createLimitDefinitionRequest{
		Name: "usage_by_limit", ResourceType: "agent", Unit: "count", DefaultValue: 5,
	})

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/usage/"+limit.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp usageByLimitResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, limit.ID, resp.LimitDefinition.ID)
	assert.Equal(t, 0, resp.TotalActive)
}

func TestQuotaAPI_GetUsageByLimit_NotFound(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/usage/"+tid("nonexistent"), nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// ---------------------------------------------------------------------------
// Tests: /usage/me (self-service, no admin required)
// ---------------------------------------------------------------------------

func TestQuotaAPI_UsageMe_Authenticated(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	seedRoleDefinitions(ctx, s)

	// Create a non-admin user.
	memberU := &store.User{
		ID:          tid("quota-member"),
		Email:       "quota-member@example.com",
		DisplayName: "Quota Member",
		Role:        "member",
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, memberU))

	rec := doRequestAsUser(t, srv, memberU, http.MethodGet, "/api/v1/usage/me", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp myUsageResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	// Should return items (may be empty if no limits are defined).
	assert.NotNil(t, resp.Items)
}

func TestQuotaAPI_UsageMe_Unauthenticated(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequestNoAuth(t, srv, http.MethodGet, "/api/v1/usage/me", nil)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestQuotaAPI_UsageMe_WithLimits(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	seedRoleDefinitions(ctx, s)

	// Create a limit and a user.
	limit, err := s.CreateLimitDefinition(ctx, &store.LimitDefinition{
		Name:         "me_test_limit",
		ResourceType: "agent",
		Unit:         "count",
		Description:  "test",
		DefaultValue: 5,
	})
	require.NoError(t, err)

	memberU := &store.User{
		ID:          tid("quota-member-2"),
		Email:       "quota-member-2@example.com",
		DisplayName: "Quota Member 2",
		Role:        "member",
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, memberU))

	rec := doRequestAsUser(t, srv, memberU, http.MethodGet, "/api/v1/usage/me", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp myUsageResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.NotEmpty(t, resp.Items)

	// Find our limit in the response.
	var found bool
	for _, entry := range resp.Items {
		if entry.LimitDefinition.ID == limit.ID {
			found = true
			assert.Equal(t, int64(0), entry.Current) // no reservations yet
			assert.Equal(t, int64(5), entry.Max)     // default value
			break
		}
	}
	assert.True(t, found, "expected to find limit %s in usage/me response", limit.ID)
}

// ---------------------------------------------------------------------------
// Tests: Admin permission enforcement
// ---------------------------------------------------------------------------

func TestQuotaAPI_AdminLimits_Forbidden_NonAdmin(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	seedRoleDefinitions(ctx, s)
	memberU := &store.User{
		ID: tid("quota-nonadmin1"), Email: "qa-nonadmin1@example.com",
		DisplayName: "Member", Role: "member", Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, memberU))
	handler := srv.guarded("/api/v1/admin/limits", srv.handleAdminLimits)

	member := NewAuthenticatedUser(tid("quota-nonadmin1"), "qa-nonadmin1@example.com", "Member", "member", "cli")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/limits", nil)
	req = req.WithContext(contextWithIdentity(ctx, member))
	rec := httptest.NewRecorder()
	handler(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestQuotaAPI_AdminLimits_Forbidden_Unauthenticated(t *testing.T) {
	srv, _ := testServer(t)
	ctx := context.Background()
	handler := srv.guarded("/api/v1/admin/limits", srv.handleAdminLimits)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/limits", nil)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	handler(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestQuotaAPI_AdminEntitlements_Forbidden_NonAdmin(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	seedRoleDefinitions(ctx, s)
	memberU := &store.User{
		ID: tid("quota-nonadmin2"), Email: "qa-nonadmin2@example.com",
		DisplayName: "Member", Role: "member", Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, memberU))
	handler := srv.guarded("/api/v1/admin/entitlements/", srv.handleAdminEntitlementByID)

	member := NewAuthenticatedUser(tid("quota-nonadmin2"), "qa-nonadmin2@example.com", "Member", "member", "cli")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/entitlements/some-id", nil)
	req = req.WithContext(contextWithIdentity(ctx, member))
	rec := httptest.NewRecorder()
	handler(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestQuotaAPI_AdminUsage_Forbidden_NonAdmin(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	seedRoleDefinitions(ctx, s)
	memberU := &store.User{
		ID: tid("quota-nonadmin3"), Email: "qa-nonadmin3@example.com",
		DisplayName: "Member", Role: "member", Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, memberU))
	handler := srv.guarded("/api/v1/admin/usage", srv.handleAdminUsage)

	member := NewAuthenticatedUser(tid("quota-nonadmin3"), "qa-nonadmin3@example.com", "Member", "member", "cli")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/usage", nil)
	req = req.WithContext(contextWithIdentity(ctx, member))
	rec := httptest.NewRecorder()
	handler(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// ---------------------------------------------------------------------------
// Tests: Hub-admin with quota permissions can access admin endpoints
// ---------------------------------------------------------------------------

func TestQuotaAPI_HubAdmin_CanAccessLimits(t *testing.T) {
	srv, _ := testServer(t)
	// The dev auth token creates a super-admin by default, which should have access.
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/limits", nil)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestQuotaAPI_HubAdmin_CanAccessUsage(t *testing.T) {
	srv, _ := testServer(t)
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/usage", nil)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// ---------------------------------------------------------------------------
// Tests: Method not allowed
// ---------------------------------------------------------------------------

func TestQuotaAPI_Limits_MethodNotAllowed(t *testing.T) {
	srv, _ := testServer(t)
	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/admin/limits", nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestQuotaAPI_Usage_MethodNotAllowed(t *testing.T) {
	srv, _ := testServer(t)
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/usage", nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestQuotaAPI_UsageMe_MethodNotAllowed(t *testing.T) {
	srv, _ := testServer(t)
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/usage/me", nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}
