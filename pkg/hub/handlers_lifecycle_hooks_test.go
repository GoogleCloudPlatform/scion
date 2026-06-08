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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// validWebhookAction returns a minimal well-formed webhook action that passes
// validation (no execution identity required for webhook type).
func validWebhookAction() *store.LifecycleHookAction {
	return &store.LifecycleHookAction{
		Type:           store.LifecycleHookActionWebhook,
		Method:         "POST",
		URL:            "https://hooks.example.com/webhook",
		Body:           `{"agent":"${AGENT_ID}"}`,
		TimeoutSeconds: 10,
		OnError:        store.LifecycleHookOnErrorLog,
	}
}

// validCreateRequest returns a well-formed create-hook request body (webhook
// type so no execution identity is needed).
func validCreateRequest() createLifecycleHookRequest {
	return createLifecycleHookRequest{
		Name:      "register-agent",
		ScopeType: store.LifecycleHookScopeHub,
		Trigger:   store.LifecycleHookTriggerRunning,
		Action:    validWebhookAction(),
		Enabled:   true,
	}
}

// createHookViaAPI is a convenience wrapper that creates a lifecycle hook
// through the API and returns the decoded response body.
func createHookViaAPI(t *testing.T, srv *Server, req createLifecycleHookRequest) store.LifecycleHook {
	t.Helper()
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/lifecycle-hooks", req)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	var hook store.LifecycleHook
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&hook))
	return hook
}

// ---------------------------------------------------------------------------
// Tests: Create
// ---------------------------------------------------------------------------

func TestLifecycleHook_Create_HappyPath(t *testing.T) {
	srv, _ := testServer(t)
	req := validCreateRequest()

	hook := createHookViaAPI(t, srv, req)

	assert.NotEmpty(t, hook.ID)
	assert.Equal(t, "register-agent", hook.Name)
	assert.Equal(t, store.LifecycleHookScopeHub, hook.ScopeType)
	assert.Equal(t, store.LifecycleHookTriggerRunning, hook.Trigger)
	assert.True(t, hook.Enabled)
	assert.Equal(t, int64(1), hook.StateVersion)
	assert.False(t, hook.Created.IsZero())
}

func TestLifecycleHook_Create_DefaultScopeToHub(t *testing.T) {
	srv, _ := testServer(t)
	req := validCreateRequest()
	req.ScopeType = "" // omit — should default to hub

	hook := createHookViaAPI(t, srv, req)
	assert.Equal(t, store.LifecycleHookScopeHub, hook.ScopeType)
}

func TestLifecycleHook_Create_MissingName(t *testing.T) {
	srv, _ := testServer(t)
	req := validCreateRequest()
	req.Name = ""

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/lifecycle-hooks", req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestLifecycleHook_Create_ValidationError_BadTrigger(t *testing.T) {
	srv, _ := testServer(t)
	req := validCreateRequest()
	req.Trigger = "booting" // not a valid v1 trigger

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/lifecycle-hooks", req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	var body ErrorResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	assert.Equal(t, ErrCodeValidationError, body.Error.Code)
	assert.Contains(t, body.Error.Message, "trigger")
}

func TestLifecycleHook_Create_ValidationError_UntrustedVarInHeader(t *testing.T) {
	srv, _ := testServer(t)
	req := validCreateRequest()
	req.Action.Headers = map[string]string{
		"X-Agent": "${AGENT_NAME}",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/lifecycle-hooks", req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	var body ErrorResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	assert.Equal(t, ErrCodeValidationError, body.Error.Code)
	assert.Contains(t, body.Error.Message, "AGENT_NAME")
}

// ---------------------------------------------------------------------------
// Tests: Authz
// ---------------------------------------------------------------------------

func TestLifecycleHook_Create_Forbidden_NonAdmin(t *testing.T) {
	srv := &Server{}

	member := NewAuthenticatedUser("u1", "member@example.com", "Member", "member", "cli")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/lifecycle-hooks", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), member))
	rec := httptest.NewRecorder()
	srv.handleAdminLifecycleHooks(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestLifecycleHook_Create_Forbidden_Unauthenticated(t *testing.T) {
	srv := &Server{}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/lifecycle-hooks", nil)
	rec := httptest.NewRecorder()
	srv.handleAdminLifecycleHooks(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestLifecycleHook_Get_Forbidden_NonAdmin(t *testing.T) {
	srv := &Server{}

	member := NewAuthenticatedUser("u1", "member@example.com", "Member", "member", "cli")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/lifecycle-hooks/some-id", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), member))
	rec := httptest.NewRecorder()
	srv.handleAdminLifecycleHookByID(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestLifecycleHook_List_Forbidden_NonAdmin(t *testing.T) {
	srv := &Server{}

	member := NewAuthenticatedUser("u1", "member@example.com", "Member", "member", "cli")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/lifecycle-hooks", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), member))
	rec := httptest.NewRecorder()
	srv.handleAdminLifecycleHooks(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestLifecycleHook_Update_Forbidden_NonAdmin(t *testing.T) {
	srv := &Server{}

	member := NewAuthenticatedUser("u1", "member@example.com", "Member", "member", "cli")
	req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/lifecycle-hooks/some-id", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), member))
	rec := httptest.NewRecorder()
	srv.handleAdminLifecycleHookByID(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestLifecycleHook_Update_Forbidden_Unauthenticated(t *testing.T) {
	srv := &Server{}

	req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/lifecycle-hooks/some-id", nil)
	rec := httptest.NewRecorder()
	srv.handleAdminLifecycleHookByID(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestLifecycleHook_Delete_Forbidden_NonAdmin(t *testing.T) {
	srv := &Server{}

	member := NewAuthenticatedUser("u1", "member@example.com", "Member", "member", "cli")
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/admin/lifecycle-hooks/some-id", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), member))
	rec := httptest.NewRecorder()
	srv.handleAdminLifecycleHookByID(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// ---------------------------------------------------------------------------
// Tests: Get
// ---------------------------------------------------------------------------

func TestLifecycleHook_Get_HappyPath(t *testing.T) {
	srv, _ := testServer(t)
	created := createHookViaAPI(t, srv, validCreateRequest())

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/lifecycle-hooks/"+created.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var hook store.LifecycleHook
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&hook))
	assert.Equal(t, created.ID, hook.ID)
	assert.Equal(t, "register-agent", hook.Name)
}

func TestLifecycleHook_Get_NotFound(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/lifecycle-hooks/"+uuid.New().String(), nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// ---------------------------------------------------------------------------
// Tests: List
// ---------------------------------------------------------------------------

func TestLifecycleHook_List_Empty(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/lifecycle-hooks", nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp listLifecycleHooksResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Empty(t, resp.Items)
	assert.Equal(t, 0, resp.TotalCount)
}

func TestLifecycleHook_List_MultipleHooks(t *testing.T) {
	srv, _ := testServer(t)

	req1 := validCreateRequest()
	req1.Name = "hook-1"
	createHookViaAPI(t, srv, req1)

	req2 := validCreateRequest()
	req2.Name = "hook-2"
	req2.Trigger = store.LifecycleHookTriggerStopped
	createHookViaAPI(t, srv, req2)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/lifecycle-hooks", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp listLifecycleHooksResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, 2, resp.TotalCount)
	assert.Len(t, resp.Items, 2)
}

func TestLifecycleHook_List_FilterByTrigger(t *testing.T) {
	srv, _ := testServer(t)

	req1 := validCreateRequest()
	req1.Trigger = store.LifecycleHookTriggerRunning
	createHookViaAPI(t, srv, req1)

	req2 := validCreateRequest()
	req2.Name = "stopped-hook"
	req2.Trigger = store.LifecycleHookTriggerStopped
	createHookViaAPI(t, srv, req2)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/lifecycle-hooks?trigger=stopped", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp listLifecycleHooksResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, 1, resp.TotalCount)
	assert.Equal(t, "stopped-hook", resp.Items[0].Name)
}

// ---------------------------------------------------------------------------
// Tests: Update
// ---------------------------------------------------------------------------

func TestLifecycleHook_Update_HappyPath(t *testing.T) {
	srv, _ := testServer(t)
	created := createHookViaAPI(t, srv, validCreateRequest())

	updateReq := updateLifecycleHookRequest{
		Name:         "deregister-agent",
		Trigger:      store.LifecycleHookTriggerStopped,
		Action:       validWebhookAction(),
		Enabled:      false,
		StateVersion: created.StateVersion,
	}

	rec := doRequest(t, srv, http.MethodPut, "/api/v1/admin/lifecycle-hooks/"+created.ID, updateReq)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var updated store.LifecycleHook
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&updated))
	assert.Equal(t, "deregister-agent", updated.Name)
	assert.Equal(t, store.LifecycleHookTriggerStopped, updated.Trigger)
	assert.False(t, updated.Enabled)
	assert.Equal(t, created.StateVersion+1, updated.StateVersion)
}

func TestLifecycleHook_Update_VersionConflict(t *testing.T) {
	srv, _ := testServer(t)
	created := createHookViaAPI(t, srv, validCreateRequest())

	// First update succeeds.
	updateReq := updateLifecycleHookRequest{
		Name:         "updated-name",
		Trigger:      store.LifecycleHookTriggerRunning,
		Action:       validWebhookAction(),
		Enabled:      true,
		StateVersion: created.StateVersion,
	}
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/admin/lifecycle-hooks/"+created.ID, updateReq)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	// Second update with stale version should conflict.
	staleReq := updateLifecycleHookRequest{
		Name:         "stale-update",
		Trigger:      store.LifecycleHookTriggerRunning,
		Action:       validWebhookAction(),
		Enabled:      true,
		StateVersion: created.StateVersion, // stale!
	}
	rec = doRequest(t, srv, http.MethodPut, "/api/v1/admin/lifecycle-hooks/"+created.ID, staleReq)
	assert.Equal(t, http.StatusConflict, rec.Code)

	var body ErrorResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	assert.Equal(t, ErrCodeVersionConflict, body.Error.Code)
}

func TestLifecycleHook_Update_NotFound(t *testing.T) {
	srv, _ := testServer(t)

	updateReq := updateLifecycleHookRequest{
		Name:         "ghost",
		Trigger:      store.LifecycleHookTriggerRunning,
		Action:       validWebhookAction(),
		Enabled:      true,
		StateVersion: 1,
	}
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/admin/lifecycle-hooks/"+uuid.New().String(), updateReq)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestLifecycleHook_Update_ScopeImmutable(t *testing.T) {
	srv, _ := testServer(t)

	// Create a hook with hub scope and a scope ID.
	createReq := validCreateRequest()
	createReq.ScopeType = store.LifecycleHookScopeHub
	createReq.ScopeID = "original-scope-id"
	created := createHookViaAPI(t, srv, createReq)

	assert.Equal(t, store.LifecycleHookScopeHub, created.ScopeType)
	assert.Equal(t, "original-scope-id", created.ScopeID)

	// Update the hook — the updateLifecycleHookRequest intentionally omits
	// scopeType and scopeId, ensuring they cannot be changed after creation.
	updateReq := updateLifecycleHookRequest{
		Name:         "updated-name",
		Trigger:      store.LifecycleHookTriggerRunning,
		Action:       validWebhookAction(),
		Enabled:      true,
		StateVersion: created.StateVersion,
	}

	rec := doRequest(t, srv, http.MethodPut, "/api/v1/admin/lifecycle-hooks/"+created.ID, updateReq)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	// Re-fetch and verify scope fields are unchanged.
	getRec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/lifecycle-hooks/"+created.ID, nil)
	require.Equal(t, http.StatusOK, getRec.Code)

	var got store.LifecycleHook
	require.NoError(t, json.NewDecoder(getRec.Body).Decode(&got))
	assert.Equal(t, store.LifecycleHookScopeHub, got.ScopeType, "scopeType must be immutable after creation")
	assert.Equal(t, "original-scope-id", got.ScopeID, "scopeId must be immutable after creation")
}

func TestLifecycleHook_Update_ValidationError_BadTrigger(t *testing.T) {
	srv, _ := testServer(t)
	created := createHookViaAPI(t, srv, validCreateRequest())

	updateReq := updateLifecycleHookRequest{
		Name:         "bad-trigger",
		Trigger:      "invalid",
		Action:       validWebhookAction(),
		Enabled:      true,
		StateVersion: created.StateVersion,
	}
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/admin/lifecycle-hooks/"+created.ID, updateReq)
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	var body ErrorResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	assert.Equal(t, ErrCodeValidationError, body.Error.Code)
}

// ---------------------------------------------------------------------------
// Tests: Delete
// ---------------------------------------------------------------------------

func TestLifecycleHook_Delete_HappyPath(t *testing.T) {
	srv, _ := testServer(t)
	created := createHookViaAPI(t, srv, validCreateRequest())

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/admin/lifecycle-hooks/"+created.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// Confirm deletion.
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/admin/lifecycle-hooks/"+created.ID, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestLifecycleHook_Delete_NotFound(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/admin/lifecycle-hooks/"+uuid.New().String(), nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// ---------------------------------------------------------------------------
// Tests: Method not allowed
// ---------------------------------------------------------------------------

func TestLifecycleHook_MethodNotAllowed(t *testing.T) {
	srv := &Server{}

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/admin/lifecycle-hooks", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rec := httptest.NewRecorder()
	srv.handleAdminLifecycleHooks(rec, req)

	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}
