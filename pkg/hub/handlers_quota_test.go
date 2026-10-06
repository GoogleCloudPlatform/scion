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

// TestQuotaAPI_UsageMe_UserScopedLimitReflectsReservation proves a
// user-scoped limit still appears in /usage/me with its correct current
// count once the user holds a reservation against it — the counterpart to
// TestQuotaAPI_UsageMe_ExcludesBrokerScopedLimit below (ptone/scion#2313:
// only the broker-scoped row is dropped, user-scoped rows are unchanged).
// It reserves at the (system, empty scope_id) shape getMyUsage queries,
// not the scope shape any production limit is actually reserved at.
func TestQuotaAPI_UsageMe_UserScopedLimitReflectsReservation(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	seedRoleDefinitions(ctx, s)

	limit, err := s.CreateLimitDefinition(ctx, &store.LimitDefinition{
		Name:         "me_test_limit_reserved",
		ResourceType: "agent",
		Unit:         "count",
		Description:  "test",
		DefaultValue: 5,
	})
	require.NoError(t, err)

	memberU := &store.User{
		ID:          tid("quota-member-3"),
		Email:       "quota-member-3@example.com",
		DisplayName: "Quota Member 3",
		Role:        "member",
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, memberU))

	_, err = srv.quotaService.Reserve(ctx, limit.Name, memberU.ID, store.QuotaScopeSystem, "", tid("resource-me-3"))
	require.NoError(t, err)

	rec := doRequestAsUser(t, srv, memberU, http.MethodGet, "/api/v1/usage/me", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp myUsageResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	var found bool
	for _, entry := range resp.Items {
		if entry.LimitDefinition.ID == limit.ID {
			found = true
			assert.Equal(t, int64(1), entry.Current)
			assert.Equal(t, int64(5), entry.Max)
			break
		}
	}
	assert.True(t, found, "expected to find limit %s in usage/me response", limit.ID)
}

// TestQuotaAPI_UsageMe_ExcludesBrokerScopedLimit proves the fix for
// ptone/scion#2313: max_agents_per_broker reservations live at
// store.QuotaScopeBroker keyed by broker ID (broker_quota.go), never at the
// store.QuotaScopeSystem/userID pair getMyUsage queries, so the row always
// showed "0 used" regardless of real broker usage — and it isn't a per-user
// quota to begin with. The row must be omitted from /usage/me entirely, even
// when the broker actually holds active reservations.
func TestQuotaAPI_UsageMe_ExcludesBrokerScopedLimit(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	seedRoleDefinitions(ctx, s)

	broker := &store.RuntimeBroker{
		ID:     tid("broker-usage-me"),
		Name:   "Usage Me Broker",
		Slug:   "usage-me-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	limit := maxAgentsPerBrokerLimit(t, s)

	// Simulate an active broker reservation, the way broker_quota.go does
	// when an agent is dispatched to this broker.
	_, err := srv.quotaService.Reserve(ctx, store.LimitMaxAgentsPerBroker, broker.ID, store.QuotaScopeBroker, broker.ID, tid("agent-usage-me"))
	require.NoError(t, err)
	require.Equal(t, int64(1), brokerReservationCount(t, s, broker.ID), "sanity check: reservation should be active")

	memberU := &store.User{
		ID:          tid("quota-member-4"),
		Email:       "quota-member-4@example.com",
		DisplayName: "Quota Member 4",
		Role:        "member",
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, memberU))

	rec := doRequestAsUser(t, srv, memberU, http.MethodGet, "/api/v1/usage/me", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp myUsageResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	for _, entry := range resp.Items {
		assert.NotEqual(t, limit.ID, entry.LimitDefinition.ID,
			"max_agents_per_broker must not appear in /usage/me, even with active broker reservations")
	}
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

// ---------------------------------------------------------------------------
// Tests: Fix R1 — Route guard permission: by-ID paths use quota.read
// ---------------------------------------------------------------------------

func TestQuotaAPI_RouteMetadata_ByIDPathsUseReadPermission(t *testing.T) {
	// Verify the route guard for by-ID paths uses quota.read (not quota.update),
	// so that users with only read permission can GET individual resources.
	limitByID := routeMetadataTable["/api/v1/admin/limits/"]
	assert.Equal(t, "quota.read", limitByID.Permission,
		"limit by-ID route guard should require quota.read")
	assert.Equal(t, "read", limitByID.Action)

	entByID := routeMetadataTable["/api/v1/admin/entitlements/"]
	assert.Equal(t, "quota.read", entByID.Permission,
		"entitlement by-ID route guard should require quota.read")
	assert.Equal(t, "read", entByID.Action)
}

// ---------------------------------------------------------------------------
// Tests: Fix R3 — updateLimitDefinition rejects blank name
// ---------------------------------------------------------------------------

func TestQuotaAPI_UpdateLimitDefinition_BlankName(t *testing.T) {
	srv, _ := testServer(t)

	created := createLimitViaAPI(t, srv, createLimitDefinitionRequest{
		Name: "update_blank_name", ResourceType: "agent", Unit: "count", DefaultValue: 5,
	})

	rec := doRequest(t, srv, http.MethodPut, "/api/v1/admin/limits/"+created.ID, updateLimitDefinitionRequest{
		Name:         "",
		ResourceType: "agent",
		Unit:         "count",
		DefaultValue: 5,
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// ---------------------------------------------------------------------------
// Tests: Fix M1 — System-seeded limits cannot be updated
// ---------------------------------------------------------------------------

func TestQuotaAPI_UpdateLimitDefinition_SystemSeeded(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a system-seeded limit directly in the store.
	systemDef, err := s.CreateLimitDefinition(ctx, &store.LimitDefinition{
		Name:         "system_update_test",
		ResourceType: "agent",
		Unit:         "count",
		DefaultValue: 100,
		System:       true,
	})
	require.NoError(t, err)

	rec := doRequest(t, srv, http.MethodPut, "/api/v1/admin/limits/"+systemDef.ID, updateLimitDefinitionRequest{
		Name:         "renamed_system_limit",
		ResourceType: "agent",
		Unit:         "count",
		DefaultValue: 200,
	})
	assert.Equal(t, http.StatusForbidden, rec.Code)

	// Verify it was not modified.
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/admin/limits/"+systemDef.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var def store.LimitDefinition
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&def))
	assert.Equal(t, "system_update_test", def.Name)
	assert.Equal(t, int64(100), def.DefaultValue)
}

// ---------------------------------------------------------------------------
// Tests: ptone/scion#2061 P1a / ptone/scion#2063 — system limit definitions
// allow changing default_value and description, but not name/resource_type/
// unit.
// ---------------------------------------------------------------------------

// TestQuotaAPI_UpdateLimitDefinition_SystemSeeded_DefaultValueAllowed verifies
// that a PUT changing only default_value (and description) on a system limit
// succeeds and persists. This is the supported admin path for the hub-wide
// max_agents_per_broker value (design.md §4.3, P1-D3).
func TestQuotaAPI_UpdateLimitDefinition_SystemSeeded_DefaultValueAllowed(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	systemDef, err := s.CreateLimitDefinition(ctx, &store.LimitDefinition{
		Name:         "system_default_value_test",
		ResourceType: "agent",
		Unit:         "count",
		Description:  "original description",
		DefaultValue: 100,
		System:       true,
	})
	require.NoError(t, err)

	rec := doRequest(t, srv, http.MethodPut, "/api/v1/admin/limits/"+systemDef.ID, updateLimitDefinitionRequest{
		Name:         systemDef.Name,
		ResourceType: systemDef.ResourceType,
		Unit:         systemDef.Unit,
		Description:  "updated description",
		DefaultValue: 16,
	})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var updated store.LimitDefinition
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&updated))
	assert.Equal(t, int64(16), updated.DefaultValue)
	assert.Equal(t, "updated description", updated.Description)
	assert.Equal(t, systemDef.Name, updated.Name)

	// Verify it persisted.
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/admin/limits/"+systemDef.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var def store.LimitDefinition
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&def))
	assert.Equal(t, int64(16), def.DefaultValue)
	assert.Equal(t, "updated description", def.Description)
}

// TestQuotaAPI_UpdateLimitDefinition_SystemSeeded_NameChangeForbidden verifies
// that a PUT changing name on a system limit is rejected with 403 and the
// documented message, and leaves the row untouched.
func TestQuotaAPI_UpdateLimitDefinition_SystemSeeded_NameChangeForbidden(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	systemDef, err := s.CreateLimitDefinition(ctx, &store.LimitDefinition{
		Name:         "system_name_change_test",
		ResourceType: "agent",
		Unit:         "count",
		DefaultValue: 100,
		System:       true,
	})
	require.NoError(t, err)

	rec := doRequest(t, srv, http.MethodPut, "/api/v1/admin/limits/"+systemDef.ID, updateLimitDefinitionRequest{
		Name:         "renamed",
		ResourceType: systemDef.ResourceType,
		Unit:         systemDef.Unit,
		DefaultValue: 16,
	})
	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "only default_value and description can be changed")

	rec = doRequest(t, srv, http.MethodGet, "/api/v1/admin/limits/"+systemDef.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var def store.LimitDefinition
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&def))
	assert.Equal(t, "system_name_change_test", def.Name)
	assert.Equal(t, int64(100), def.DefaultValue)
}

// TestQuotaAPI_UpdateLimitDefinition_SystemSeeded_ResourceTypeChangeForbidden
// verifies the same 403 for a resource_type change.
func TestQuotaAPI_UpdateLimitDefinition_SystemSeeded_ResourceTypeChangeForbidden(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	systemDef, err := s.CreateLimitDefinition(ctx, &store.LimitDefinition{
		Name:         "system_rt_change_test",
		ResourceType: "agent",
		Unit:         "count",
		DefaultValue: 100,
		System:       true,
	})
	require.NoError(t, err)

	rec := doRequest(t, srv, http.MethodPut, "/api/v1/admin/limits/"+systemDef.ID, updateLimitDefinitionRequest{
		Name:         systemDef.Name,
		ResourceType: "project",
		Unit:         systemDef.Unit,
		DefaultValue: 16,
	})
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// TestQuotaAPI_UpdateLimitDefinition_SystemSeeded_UnitChangeForbidden verifies
// the same 403 for a unit change.
func TestQuotaAPI_UpdateLimitDefinition_SystemSeeded_UnitChangeForbidden(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	systemDef, err := s.CreateLimitDefinition(ctx, &store.LimitDefinition{
		Name:         "system_unit_change_test",
		ResourceType: "agent",
		Unit:         "count",
		DefaultValue: 100,
		System:       true,
	})
	require.NoError(t, err)

	rec := doRequest(t, srv, http.MethodPut, "/api/v1/admin/limits/"+systemDef.ID, updateLimitDefinitionRequest{
		Name:         systemDef.Name,
		ResourceType: systemDef.ResourceType,
		Unit:         "instances",
		DefaultValue: 16,
	})
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// TestQuotaAPI_UpdateLimitDefinition_SystemSeeded_PaddedFieldsNormalized
// verifies that trimming happens before the system-seeded comparison
// (ptone/scion#2343): a PUT that merely pads resource_type/unit with
// whitespace is normalised and succeeds, while a PUT that still trims to a
// genuinely different protected field value is rejected.
func TestQuotaAPI_UpdateLimitDefinition_SystemSeeded_PaddedFieldsNormalized(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	systemDef, err := s.CreateLimitDefinition(ctx, &store.LimitDefinition{
		Name:         "system_padded_test",
		ResourceType: "agent",
		Unit:         "count",
		DefaultValue: 100,
		System:       true,
	})
	require.NoError(t, err)

	// Padding that normalises to the existing values: should succeed.
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/admin/limits/"+systemDef.ID, updateLimitDefinitionRequest{
		Name:         systemDef.Name,
		ResourceType: "  agent  ",
		Unit:         " count ",
		DefaultValue: 200,
	})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var updated store.LimitDefinition
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&updated))
	assert.Equal(t, "agent", updated.ResourceType)
	assert.Equal(t, "count", updated.Unit)
	assert.Equal(t, int64(200), updated.DefaultValue)

	// Padding that still trims to a genuinely different value: still rejected.
	rec = doRequest(t, srv, http.MethodPut, "/api/v1/admin/limits/"+systemDef.ID, updateLimitDefinitionRequest{
		Name:         systemDef.Name,
		ResourceType: "  project  ",
		Unit:         "count",
		DefaultValue: 300,
	})
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// TestQuotaAPI_UpdateLimitDefinition_SystemSeeded_Forbidden_NonAdmin verifies
// that a caller without quota.update cannot PUT a system limit definition
// (or any limit definition).
func TestQuotaAPI_UpdateLimitDefinition_SystemSeeded_Forbidden_NonAdmin(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	seedRoleDefinitions(ctx, s)

	systemDef, err := s.CreateLimitDefinition(ctx, &store.LimitDefinition{
		Name:         "system_nonadmin_test",
		ResourceType: "agent",
		Unit:         "count",
		DefaultValue: 100,
		System:       true,
	})
	require.NoError(t, err)

	memberU := &store.User{
		ID: tid("quota-nonadmin-put"), Email: "qa-nonadmin-put@example.com",
		DisplayName: "Member", Role: "member", Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, memberU))
	handler := srv.guarded("/api/v1/admin/limits/", srv.handleAdminLimitByID)

	body, err := json.Marshal(updateLimitDefinitionRequest{
		Name:         systemDef.Name,
		ResourceType: systemDef.ResourceType,
		Unit:         systemDef.Unit,
		DefaultValue: 16,
	})
	require.NoError(t, err)

	member := NewAuthenticatedUser(tid("quota-nonadmin-put"), "qa-nonadmin-put@example.com", "Member", "member", "cli")
	req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/limits/"+systemDef.ID, bytes.NewReader(body))
	req = req.WithContext(contextWithIdentity(ctx, member))
	rec := httptest.NewRecorder()
	handler(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)

	// Verify it was not modified.
	getRec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/limits/"+systemDef.ID, nil)
	require.Equal(t, http.StatusOK, getRec.Code)
	var def store.LimitDefinition
	require.NoError(t, json.NewDecoder(getRec.Body).Decode(&def))
	assert.Equal(t, int64(100), def.DefaultValue)
}

// ---------------------------------------------------------------------------
// Tests: Fix M2 — Negative value validation
// ---------------------------------------------------------------------------

func TestQuotaAPI_CreateLimitDefinition_NegativeDefaultValue(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/limits", createLimitDefinitionRequest{
		Name:         "neg_default_limit",
		ResourceType: "agent",
		Unit:         "count",
		DefaultValue: -5,
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestQuotaAPI_UpdateLimitDefinition_NegativeDefaultValue(t *testing.T) {
	srv, _ := testServer(t)

	created := createLimitViaAPI(t, srv, createLimitDefinitionRequest{
		Name: "neg_update_limit", ResourceType: "agent", Unit: "count", DefaultValue: 5,
	})

	rec := doRequest(t, srv, http.MethodPut, "/api/v1/admin/limits/"+created.ID, updateLimitDefinitionRequest{
		Name:         "neg_update_limit",
		ResourceType: "agent",
		Unit:         "count",
		DefaultValue: -10,
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestQuotaAPI_CreateEntitlement_NegativeValue(t *testing.T) {
	srv, _ := testServer(t)

	limit := createLimitViaAPI(t, srv, createLimitDefinitionRequest{
		Name: "neg_ent_create_limit", ResourceType: "agent", Unit: "count", DefaultValue: 5,
	})

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/limits/"+limit.ID+"/entitlements", createEntitlementBindingRequest{
		SubjectType: "user",
		SubjectID:   "user-1",
		ScopeType:   "system",
		Value:       -1,
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestQuotaAPI_UpdateEntitlement_NegativeValue(t *testing.T) {
	srv, _ := testServer(t)

	limit := createLimitViaAPI(t, srv, createLimitDefinitionRequest{
		Name: "neg_ent_update_limit", ResourceType: "agent", Unit: "count", DefaultValue: 5,
	})
	binding := createEntitlementViaAPI(t, srv, limit.ID, createEntitlementBindingRequest{
		SubjectType: "user",
		SubjectID:   "user-1",
		ScopeType:   "system",
		Value:       10,
	})

	rec := doRequest(t, srv, http.MethodPut, "/api/v1/admin/entitlements/"+binding.ID, updateEntitlementBindingRequest{
		SubjectType: "user",
		SubjectID:   "user-1",
		ScopeType:   "system",
		Value:       -5,
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// Zero values are valid (0 means unlimited).
func TestQuotaAPI_CreateLimitDefinition_ZeroDefaultValue(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/limits", createLimitDefinitionRequest{
		Name:         "zero_default_limit",
		ResourceType: "agent",
		Unit:         "count",
		DefaultValue: 0,
	})
	assert.Equal(t, http.StatusCreated, rec.Code)
}

func TestQuotaAPI_CreateEntitlement_ZeroValue(t *testing.T) {
	srv, _ := testServer(t)

	limit := createLimitViaAPI(t, srv, createLimitDefinitionRequest{
		Name: "zero_ent_limit", ResourceType: "agent", Unit: "count", DefaultValue: 5,
	})

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/limits/"+limit.ID+"/entitlements", createEntitlementBindingRequest{
		SubjectType: "user",
		SubjectID:   "user-1",
		ScopeType:   "system",
		Value:       0,
	})
	assert.Equal(t, http.StatusCreated, rec.Code)
}

// ---------------------------------------------------------------------------
// Tests: ptone/scion#2061 P2-D4 / ptone/scion#2063 item 2 — broker-scoped
// max_agents_per_broker bindings are rejected; the broker settings API
// replaces them.
// ---------------------------------------------------------------------------

// maxAgentsPerBrokerLimit fetches the max_agents_per_broker limit
// definition, which testServer's startup seeding already creates (it is a
// system-seeded limit — creating a second one via the API would conflict).
func maxAgentsPerBrokerLimit(t *testing.T, s store.Store) *store.LimitDefinition {
	t.Helper()
	def, err := s.GetLimitDefinitionByName(context.Background(), store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)
	return def
}

// TestQuotaAPI_CreateEntitlement_MaxAgentsPerBrokerBrokerScoped_Rejected
// proves a new broker-scoped binding on max_agents_per_broker is rejected
// with 400 pointing at the settings API, rather than silently creating a
// binding the entitlement engine no longer honours for this limit/scope.
func TestQuotaAPI_CreateEntitlement_MaxAgentsPerBrokerBrokerScoped_Rejected(t *testing.T) {
	srv, s := testServer(t)
	limit := maxAgentsPerBrokerLimit(t, s)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/limits/"+limit.ID+"/entitlements", createEntitlementBindingRequest{
		SubjectType: store.EntitlementSubjectUser,
		SubjectID:   "user-1",
		ScopeType:   store.QuotaScopeBroker,
		ScopeID:     "broker-1",
		Value:       5,
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "PUT /api/v1/runtime-brokers/{id}/settings")
}

// TestQuotaAPI_CreateEntitlement_MaxAgentsPerBrokerSystemScoped_StillWorks
// proves the hub-wide, system-scoped override for max_agents_per_broker
// (P1-D6 / ptone/scion#2063 item 1's companion path) is unaffected by the
// broker-scope rejection.
func TestQuotaAPI_CreateEntitlement_MaxAgentsPerBrokerSystemScoped_StillWorks(t *testing.T) {
	srv, s := testServer(t)
	limit := maxAgentsPerBrokerLimit(t, s)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/limits/"+limit.ID+"/entitlements", createEntitlementBindingRequest{
		SubjectType: store.EntitlementSubjectSystemDefault,
		SubjectID:   "system",
		ScopeType:   store.QuotaScopeSystem,
		Value:       50,
	})
	assert.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
}

// TestQuotaAPI_CreateEntitlement_BrokerScopedOtherLimit_Unaffected proves the
// rejection is specific to max_agents_per_broker: a broker-scoped binding on
// a different limit is unaffected.
func TestQuotaAPI_CreateEntitlement_BrokerScopedOtherLimit_Unaffected(t *testing.T) {
	srv, _ := testServer(t)

	limit := createLimitViaAPI(t, srv, createLimitDefinitionRequest{
		Name: "some_other_broker_limit", ResourceType: "agent", Unit: "count", DefaultValue: 5,
	})

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/limits/"+limit.ID+"/entitlements", createEntitlementBindingRequest{
		SubjectType: store.EntitlementSubjectUser,
		SubjectID:   "user-1",
		ScopeType:   store.QuotaScopeBroker,
		ScopeID:     "broker-1",
		Value:       5,
	})
	assert.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
}

// TestQuotaAPI_UpdateEntitlement_MaxAgentsPerBrokerBrokerScoped_Rejected
// proves the update path cannot be used to reshape an existing binding into
// the same rejected broker-scoped max_agents_per_broker shape — closing the
// gap createEntitlement alone would leave open.
func TestQuotaAPI_UpdateEntitlement_MaxAgentsPerBrokerBrokerScoped_Rejected(t *testing.T) {
	srv, s := testServer(t)
	limit := maxAgentsPerBrokerLimit(t, s)
	binding := createEntitlementViaAPI(t, srv, limit.ID, createEntitlementBindingRequest{
		SubjectType: store.EntitlementSubjectSystemDefault,
		SubjectID:   "system",
		ScopeType:   store.QuotaScopeSystem,
		Value:       50,
	})

	rec := doRequest(t, srv, http.MethodPut, "/api/v1/admin/entitlements/"+binding.ID, updateEntitlementBindingRequest{
		SubjectType: store.EntitlementSubjectUser,
		SubjectID:   "user-1",
		ScopeType:   store.QuotaScopeBroker,
		ScopeID:     "broker-1",
		Value:       5,
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "PUT /api/v1/runtime-brokers/{id}/settings")
}

// TestQuotaAPI_UpdateEntitlement_MaxAgentsPerBrokerSystemScoped_StillWorks
// proves an ordinary update to a system-scoped max_agents_per_broker binding
// still works.
func TestQuotaAPI_UpdateEntitlement_MaxAgentsPerBrokerSystemScoped_StillWorks(t *testing.T) {
	srv, s := testServer(t)
	limit := maxAgentsPerBrokerLimit(t, s)
	binding := createEntitlementViaAPI(t, srv, limit.ID, createEntitlementBindingRequest{
		SubjectType: store.EntitlementSubjectSystemDefault,
		SubjectID:   "system",
		ScopeType:   store.QuotaScopeSystem,
		Value:       50,
	})

	rec := doRequest(t, srv, http.MethodPut, "/api/v1/admin/entitlements/"+binding.ID, updateEntitlementBindingRequest{
		SubjectType: store.EntitlementSubjectSystemDefault,
		SubjectID:   "system",
		ScopeType:   store.QuotaScopeSystem,
		Value:       75,
	})
	assert.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
}

// ---------------------------------------------------------------------------
// Tests: Fix B3 — nil quotaService returns empty usage (HIGH)
// ---------------------------------------------------------------------------

func TestQuotaAPI_UsageMe_NilQuotaService(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	seedRoleDefinitions(ctx, s)

	memberU := &store.User{
		ID:          tid("quota-nil-qs"),
		Email:       "quota-nil-qs@example.com",
		DisplayName: "Nil QS User",
		Role:        "member",
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, memberU))

	// Nil out the quotaService to simulate a store that doesn't support quotas.
	srv.quotaService = nil

	rec := doRequestAsUser(t, srv, memberU, http.MethodGet, "/api/v1/usage/me", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp myUsageResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Empty(t, resp.Items, "nil quotaService should return empty usage items")
}

// ---------------------------------------------------------------------------
// Tests: Fix B3 — trailing slash on by-ID routes (MEDIUM-2)
// ---------------------------------------------------------------------------

func TestQuotaAPI_GetLimitDefinition_TrailingSlash(t *testing.T) {
	srv, _ := testServer(t)

	created := createLimitViaAPI(t, srv, createLimitDefinitionRequest{
		Name: "trailing_slash_limit", ResourceType: "agent", Unit: "count", DefaultValue: 5,
	})

	// Request with trailing slash — should still find the resource.
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/limits/"+created.ID+"/", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var def store.LimitDefinition
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&def))
	assert.Equal(t, created.ID, def.ID)
}

// ---------------------------------------------------------------------------
// Tests: Fix B3 — whitespace-only names rejected (MEDIUM-3 / MEDIUM-4)
// ---------------------------------------------------------------------------

func TestQuotaAPI_CreateLimitDefinition_WhitespaceOnlyName(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/limits", createLimitDefinitionRequest{
		Name:         "   ",
		ResourceType: "agent",
		Unit:         "count",
		DefaultValue: 5,
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestQuotaAPI_UpdateLimitDefinition_WhitespaceOnlyName(t *testing.T) {
	srv, _ := testServer(t)

	created := createLimitViaAPI(t, srv, createLimitDefinitionRequest{
		Name: "ws_update_limit", ResourceType: "agent", Unit: "count", DefaultValue: 5,
	})

	rec := doRequest(t, srv, http.MethodPut, "/api/v1/admin/limits/"+created.ID, updateLimitDefinitionRequest{
		Name:         "   ",
		ResourceType: "agent",
		Unit:         "count",
		DefaultValue: 5,
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// ---------------------------------------------------------------------------
// Tests: upstream review (GoogleCloudPlatform/scion#2114, gemini-code-assist),
// round 5 (broker-settings-rev-p1a-5). gemini flagged that PUT on a
// non-system limit definition with an empty or whitespace-only resource_type
// silently corrupted the row. Round 5 found the matching create-side hole
// (resource_type) and a regression the round-4 fix introduced (requiring
// unit on update, when create never required it, made existing empty-unit
// rows permanently uneditable). Final rule: resource_type is trimmed and
// required on both create and update for non-system rows; unit is trimmed
// but never required, on either path.
// ---------------------------------------------------------------------------

func TestQuotaAPI_CreateLimitDefinition_EmptyResourceType(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/limits", createLimitDefinitionRequest{
		Name:         "empty_rt_create_limit",
		ResourceType: "",
		Unit:         "count",
		DefaultValue: 5,
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestQuotaAPI_CreateLimitDefinition_WhitespaceResourceType(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/limits", createLimitDefinitionRequest{
		Name:         "ws_rt_create_limit",
		ResourceType: "   ",
		Unit:         "count",
		DefaultValue: 5,
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestQuotaAPI_UpdateLimitDefinition_EmptyResourceType(t *testing.T) {
	srv, _ := testServer(t)

	created := createLimitViaAPI(t, srv, createLimitDefinitionRequest{
		Name: "empty_rt_update_limit", ResourceType: "agent", Unit: "count", DefaultValue: 5,
	})

	rec := doRequest(t, srv, http.MethodPut, "/api/v1/admin/limits/"+created.ID, updateLimitDefinitionRequest{
		Name:         created.Name,
		ResourceType: "   ",
		Unit:         created.Unit,
		DefaultValue: 5,
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	getRec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/limits/"+created.ID, nil)
	require.Equal(t, http.StatusOK, getRec.Code)
	var def store.LimitDefinition
	require.NoError(t, json.NewDecoder(getRec.Body).Decode(&def))
	assert.Equal(t, "agent", def.ResourceType, "rejected update must not corrupt the stored resource type")
}

// TestQuotaAPI_UpdateLimitDefinition_EmptyUnitFromCreateStillEditable is the
// round-5 F1 regression test: createLimitDefinition has never required unit
// (the admin UI treats it as optional), so a row created with an empty unit
// must remain editable. A PUT that only changes default_value on such a row
// must succeed, not fail with "unit is required".
func TestQuotaAPI_UpdateLimitDefinition_EmptyUnitFromCreateStillEditable(t *testing.T) {
	srv, _ := testServer(t)

	created := createLimitViaAPI(t, srv, createLimitDefinitionRequest{
		Name: "empty_unit_from_create_limit", ResourceType: "agent", Unit: "", DefaultValue: 5,
	})
	require.Equal(t, "", created.Unit)

	rec := doRequest(t, srv, http.MethodPut, "/api/v1/admin/limits/"+created.ID, updateLimitDefinitionRequest{
		Name:         created.Name,
		ResourceType: created.ResourceType,
		Unit:         created.Unit,
		DefaultValue: 7,
	})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var updated store.LimitDefinition
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&updated))
	assert.Equal(t, int64(7), updated.DefaultValue)
	assert.Equal(t, "", updated.Unit)
}

// TestQuotaAPI_UpdateLimitDefinition_TrimsResourceTypeAndUnit is the round-5
// F4 test: a non-system PUT with padding whitespace around resource_type and
// unit must persist the trimmed values, not the raw ones.
func TestQuotaAPI_UpdateLimitDefinition_TrimsResourceTypeAndUnit(t *testing.T) {
	srv, _ := testServer(t)

	created := createLimitViaAPI(t, srv, createLimitDefinitionRequest{
		Name: "trim_update_limit", ResourceType: "agent", Unit: "count", DefaultValue: 5,
	})

	rec := doRequest(t, srv, http.MethodPut, "/api/v1/admin/limits/"+created.ID, updateLimitDefinitionRequest{
		Name:         created.Name,
		ResourceType: "  project  ",
		Unit:         "  members  ",
		DefaultValue: 5,
	})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var updated store.LimitDefinition
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&updated))
	assert.Equal(t, "project", updated.ResourceType)
	assert.Equal(t, "members", updated.Unit)

	getRec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/limits/"+created.ID, nil)
	require.Equal(t, http.StatusOK, getRec.Code)
	var def store.LimitDefinition
	require.NoError(t, json.NewDecoder(getRec.Body).Decode(&def))
	assert.Equal(t, "project", def.ResourceType)
	assert.Equal(t, "members", def.Unit)
}

// TestQuotaAPI_CreateLimitDefinition_TrimsUnit is a regression test for
// ptone/scion#2307: the create path did not trim whitespace from unit, while
// the update path (see TestQuotaAPI_UpdateLimitDefinition_TrimsResourceTypeAndUnit)
// already did.
func TestQuotaAPI_CreateLimitDefinition_TrimsUnit(t *testing.T) {
	srv, _ := testServer(t)

	def := createLimitViaAPI(t, srv, createLimitDefinitionRequest{
		Name:         "trim_create_limit",
		ResourceType: "agent",
		Unit:         "  count  ",
		DefaultValue: 5,
	})
	assert.Equal(t, "count", def.Unit)

	getRec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/limits/"+def.ID, nil)
	require.Equal(t, http.StatusOK, getRec.Code)
	var stored store.LimitDefinition
	require.NoError(t, json.NewDecoder(getRec.Body).Decode(&stored))
	assert.Equal(t, "count", stored.Unit)
}

// TestQuotaAPI_UpdateLimitDefinition_SystemSeeded_EmptyResourceTypeAndUnitForbidden
// is the round-5 F3 fix: a system row's resource_type/unit are identity
// fields, not editable content, so sending an empty or whitespace-only value
// for either must take the existing identity-mismatch path (403), not the
// non-system empty-value validation (400). The new non-system-only checks
// never run for a system row.
func TestQuotaAPI_UpdateLimitDefinition_SystemSeeded_EmptyResourceTypeAndUnitForbidden(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	systemDef, err := s.CreateLimitDefinition(ctx, &store.LimitDefinition{
		Name:         "system_empty_identity_test",
		ResourceType: "agent",
		Unit:         "count",
		DefaultValue: 100,
		System:       true,
	})
	require.NoError(t, err)

	rec := doRequest(t, srv, http.MethodPut, "/api/v1/admin/limits/"+systemDef.ID, updateLimitDefinitionRequest{
		Name:         systemDef.Name,
		ResourceType: "   ",
		Unit:         "",
		DefaultValue: 42,
	})
	assert.Equal(t, http.StatusForbidden, rec.Code, "empty resource_type/unit on a system row is an identity change, not a validation error")

	getRec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/limits/"+systemDef.ID, nil)
	require.Equal(t, http.StatusOK, getRec.Code)
	var def store.LimitDefinition
	require.NoError(t, json.NewDecoder(getRec.Body).Decode(&def))
	assert.Equal(t, "agent", def.ResourceType)
	assert.Equal(t, "count", def.Unit)
	assert.Equal(t, int64(100), def.DefaultValue, "the rejected PUT must not have changed default_value either")
}

// ---------------------------------------------------------------------------
// Tests: Fix B3 — empty SubjectType/SubjectID rejected (MEDIUM-5 / MEDIUM-6)
// ---------------------------------------------------------------------------

func TestQuotaAPI_CreateEntitlement_EmptySubjectType(t *testing.T) {
	srv, _ := testServer(t)

	limit := createLimitViaAPI(t, srv, createLimitDefinitionRequest{
		Name: "ent_empty_st_create", ResourceType: "agent", Unit: "count", DefaultValue: 5,
	})

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/limits/"+limit.ID+"/entitlements", createEntitlementBindingRequest{
		SubjectType: "",
		SubjectID:   "user-1",
		ScopeType:   "system",
		Value:       10,
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestQuotaAPI_CreateEntitlement_EmptySubjectID(t *testing.T) {
	srv, _ := testServer(t)

	limit := createLimitViaAPI(t, srv, createLimitDefinitionRequest{
		Name: "ent_empty_sid_create", ResourceType: "agent", Unit: "count", DefaultValue: 5,
	})

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/limits/"+limit.ID+"/entitlements", createEntitlementBindingRequest{
		SubjectType: "user",
		SubjectID:   "",
		ScopeType:   "system",
		Value:       10,
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestQuotaAPI_UpdateEntitlement_EmptySubjectType(t *testing.T) {
	srv, _ := testServer(t)

	limit := createLimitViaAPI(t, srv, createLimitDefinitionRequest{
		Name: "ent_empty_st_update", ResourceType: "agent", Unit: "count", DefaultValue: 5,
	})
	binding := createEntitlementViaAPI(t, srv, limit.ID, createEntitlementBindingRequest{
		SubjectType: "user",
		SubjectID:   "user-1",
		ScopeType:   "system",
		Value:       10,
	})

	rec := doRequest(t, srv, http.MethodPut, "/api/v1/admin/entitlements/"+binding.ID, updateEntitlementBindingRequest{
		SubjectType: "",
		SubjectID:   "user-1",
		ScopeType:   "system",
		Value:       20,
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestQuotaAPI_UpdateEntitlement_EmptySubjectID(t *testing.T) {
	srv, _ := testServer(t)

	limit := createLimitViaAPI(t, srv, createLimitDefinitionRequest{
		Name: "ent_empty_sid_update", ResourceType: "agent", Unit: "count", DefaultValue: 5,
	})
	binding := createEntitlementViaAPI(t, srv, limit.ID, createEntitlementBindingRequest{
		SubjectType: "user",
		SubjectID:   "user-1",
		ScopeType:   "system",
		Value:       10,
	})

	rec := doRequest(t, srv, http.MethodPut, "/api/v1/admin/entitlements/"+binding.ID, updateEntitlementBindingRequest{
		SubjectType: "user",
		SubjectID:   "",
		ScopeType:   "system",
		Value:       20,
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}
