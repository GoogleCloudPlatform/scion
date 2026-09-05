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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ---------------------------------------------------------------------------
// Structured denial response shape tests for User Administration endpoints.
//
// These tests verify that 403 responses from user mutation endpoints include
// machine-readable structured details (resource_type, denied_action) in the
// error envelope, enabling the frontend to render a two-line toast:
//   Line 1: human-readable reason
//   Line 2: "Permission needed: {action} on {resource_type}"
//
// They also verify that internal identifiers (user IDs, role IDs, credential
// IDs, policy internals, raw evaluator reasons) are NOT exposed.
// ---------------------------------------------------------------------------

// parseErrorResponse decodes an HTTP response body into ErrorResponse.
func parseErrorResponse(t *testing.T, body []byte) ErrorResponse {
	t.Helper()
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(body, &resp), "response body: %s", string(body))
	return resp
}

// assertStructuredDenial verifies the 403 response has the expected structured
// denial detail and redacts internal information.
func assertStructuredDenial(t *testing.T, resp ErrorResponse, wantResourceType, wantAction string) {
	t.Helper()

	assert.Equal(t, ErrCodeForbidden, resp.Error.Code, "error code")
	require.NotNil(t, resp.Error.Details, "expected structured details in 403 response")

	if wantResourceType != "" {
		got, ok := resp.Error.Details["resource_type"]
		assert.True(t, ok, "expected details.resource_type")
		assert.Equal(t, wantResourceType, got, "details.resource_type")
	}
	if wantAction != "" {
		got, ok := resp.Error.Details["denied_action"]
		assert.True(t, ok, "expected details.denied_action")
		assert.Equal(t, wantAction, got, "details.denied_action")
	}
}

// assertNoIDLeakage verifies the response body does not contain any of the
// given IDs.
func assertNoIDLeakage(t *testing.T, body string, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if id == "" {
			continue
		}
		assert.False(t, strings.Contains(body, id),
			"response body must not leak internal ID %q", id)
	}
}

// ---------------------------------------------------------------------------
// PATCH /api/v1/users/{id} — structured denial shapes
// ---------------------------------------------------------------------------

func TestDenialShape_Promote_UserPromoteDenied(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	actor := &store.User{
		ID:          tid("denial-promote-actor"),
		Email:       "denialactor@example.com",
		DisplayName: "Denial Actor",
		Role:        "member",
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, actor))

	target := &store.User{
		ID:          tid("denial-promote-target"),
		Email:       "denialtarget@example.com",
		DisplayName: "Denial Target",
		Role:        "member",
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, target))

	rec := doRequestAsUser(t, srv, actor, http.MethodPatch,
		"/api/v1/users/"+target.ID,
		map[string]string{"role": "admin"})

	assert.Equal(t, http.StatusForbidden, rec.Code)
	resp := parseErrorResponse(t, rec.Body.Bytes())
	assertStructuredDenial(t, resp, "user", "promote")

	// Must not leak target user ID or actor ID.
	assertNoIDLeakage(t, rec.Body.String(), target.ID, actor.ID)
}

func TestDenialShape_Suspend_UserSuspendDenied(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	actor := &store.User{
		ID:          tid("denial-suspend-actor"),
		Email:       "suspactor@example.com",
		DisplayName: "Suspend Actor",
		Role:        "member",
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, actor))

	target := &store.User{
		ID:          tid("denial-suspend-target"),
		Email:       "susptarget@example.com",
		DisplayName: "Suspend Target",
		Role:        "member",
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, target))

	rec := doRequestAsUser(t, srv, actor, http.MethodPatch,
		"/api/v1/users/"+target.ID,
		map[string]string{"status": "suspended"})

	assert.Equal(t, http.StatusForbidden, rec.Code)
	resp := parseErrorResponse(t, rec.Body.Bytes())
	assertStructuredDenial(t, resp, "user", "suspend")

	assertNoIDLeakage(t, rec.Body.String(), target.ID, actor.ID)
}

func TestDenialShape_Update_UserUpdateDenied(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	actor := &store.User{
		ID:          tid("denial-update-actor"),
		Email:       "updactor@example.com",
		DisplayName: "Update Actor",
		Role:        "member",
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, actor))

	target := &store.User{
		ID:          tid("denial-update-target"),
		Email:       "updtarget@example.com",
		DisplayName: "Update Target",
		Role:        "member",
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, target))

	rec := doRequestAsUser(t, srv, actor, http.MethodPatch,
		"/api/v1/users/"+target.ID,
		map[string]string{"displayName": "New Name"})

	assert.Equal(t, http.StatusForbidden, rec.Code)
	resp := parseErrorResponse(t, rec.Body.Bytes())
	assertStructuredDenial(t, resp, "user", "update")

	assertNoIDLeakage(t, rec.Body.String(), target.ID, actor.ID)
}

// TestDenialShape_MixedPatch_FirstDeniedAction verifies that a mixed PATCH
// (role + status) identifies the first denied action deterministically.
// The permission check order is: promote → suspend → update.
func TestDenialShape_MixedPatch_FirstDeniedAction(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	actor := &store.User{
		ID:          tid("denial-mixed-actor"),
		Email:       "mixedactor@example.com",
		DisplayName: "Mixed Actor",
		Role:        "member",
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, actor))

	target := &store.User{
		ID:          tid("denial-mixed-target"),
		Email:       "mixedtarget@example.com",
		DisplayName: "Mixed Target",
		Role:        "member",
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, target))

	rec := doRequestAsUser(t, srv, actor, http.MethodPatch,
		"/api/v1/users/"+target.ID,
		map[string]string{"role": "admin", "status": "suspended"})

	assert.Equal(t, http.StatusForbidden, rec.Code)
	resp := parseErrorResponse(t, rec.Body.Bytes())
	// promote is checked first, so it should be the denied action
	assertStructuredDenial(t, resp, "user", "promote")
}

// ---------------------------------------------------------------------------
// DELETE /api/v1/users/{id} — structured denial shape
// ---------------------------------------------------------------------------

func TestDenialShape_Delete_UserDeleteDenied(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	actor := &store.User{
		ID:          tid("denial-delete-actor"),
		Email:       "delactor@example.com",
		DisplayName: "Delete Actor",
		Role:        "member",
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, actor))

	target := &store.User{
		ID:          tid("denial-delete-target"),
		Email:       "deltarget@example.com",
		DisplayName: "Delete Target",
		Role:        "member",
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, target))

	rec := doRequestAsUser(t, srv, actor, http.MethodDelete,
		"/api/v1/users/"+target.ID, nil)

	assert.Equal(t, http.StatusForbidden, rec.Code)
	resp := parseErrorResponse(t, rec.Body.Bytes())
	assertStructuredDenial(t, resp, "user", "delete")

	assertNoIDLeakage(t, rec.Body.String(), target.ID, actor.ID)
}

// ---------------------------------------------------------------------------
// Role binding CanDelegate denials — structured shape
// ---------------------------------------------------------------------------

func TestDenialShape_RoleBinding_CreateCanDelegateDenied(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// A member cannot delegate role bindings.
	actor := &store.User{
		ID:          tid("denial-rb-create-actor"),
		Email:       "rbcreateactor@example.com",
		DisplayName: "RB Create Actor",
		Role:        "member",
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, actor))

	target := &store.User{
		ID:          tid("denial-rb-create-target"),
		Email:       "rbcreatetarget@example.com",
		DisplayName: "RB Create Target",
		Role:        "member",
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, target))

	// Get a role definition to try to bind.
	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubMember, store.RoleScopeSystem)
	require.NoError(t, err)

	rec := doRequestAsUser(t, srv, actor, http.MethodPost,
		"/api/v1/admin/role-bindings",
		map[string]string{
			"roleDefinitionId": rd.ID,
			"principalType":    "user",
			"principalId":      target.ID,
			"scopeType":        "system",
			"scopeId":          "",
		})

	assert.Equal(t, http.StatusForbidden, rec.Code)
	resp := parseErrorResponse(t, rec.Body.Bytes())
	assertStructuredDenial(t, resp, "role_binding", "create")

	// Must not leak role definition ID in details.
	if resp.Error.Details != nil {
		_, hasRoleDefID := resp.Error.Details["role_definition_id"]
		assert.False(t, hasRoleDefID, "details must not expose role_definition_id")
	}
}

// TestDenialShape_RoleBinding_CanDelegateDenied_ViaPromote verifies the
// structured detail shape when a member lacks delegation authority. The
// CanDelegate path is reached after user.promote, but a member without
// user.promote gets denied there first with resource_type: "user". This
// confirms the deterministic ordering and structured shape.
func TestDenialShape_RoleBinding_CanDelegateDenied_ViaPromote(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// A non-admin member with no delegation authority.
	actor := &store.User{
		ID:          tid("denial-cd-actor"),
		Email:       "cdactor@example.com",
		DisplayName: "CD Actor",
		Role:        "member",
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, actor))

	target := &store.User{
		ID:          tid("denial-cd-target"),
		Email:       "cdtarget@example.com",
		DisplayName: "CD Target",
		Role:        "member",
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, target))

	// Member tries to promote target — denied at user.promote permission,
	// which returns resource_type: "user", denied_action: "promote".
	rec := doRequestAsUser(t, srv, actor, http.MethodPatch,
		"/api/v1/users/"+target.ID,
		map[string]string{"role": "admin"})

	assert.Equal(t, http.StatusForbidden, rec.Code)
	resp := parseErrorResponse(t, rec.Body.Bytes())
	// The first denial in the promote flow is user.promote, not CanDelegate.
	assertStructuredDenial(t, resp, "user", "promote")
}

// ---------------------------------------------------------------------------
// Redaction: verify IDs and internals are not exposed
// ---------------------------------------------------------------------------

func TestDenialShape_Redaction_NoTargetIDInBody(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	actor := &store.User{
		ID:          tid("denial-redact-actor"),
		Email:       "redactactor@example.com",
		DisplayName: "Redact Actor",
		Role:        "member",
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, actor))

	sensitiveID := tid("denial-sensitive-id")
	target := &store.User{
		ID:          sensitiveID,
		Email:       "sensitive@example.com",
		DisplayName: "Sensitive User",
		Role:        "member",
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, target))

	actions := []struct {
		method string
		path   string
		body   interface{}
	}{
		{http.MethodPatch, "/api/v1/users/" + sensitiveID, map[string]string{"role": "admin"}},
		{http.MethodPatch, "/api/v1/users/" + sensitiveID, map[string]string{"status": "suspended"}},
		{http.MethodPatch, "/api/v1/users/" + sensitiveID, map[string]string{"displayName": "New"}},
		{http.MethodDelete, "/api/v1/users/" + sensitiveID, nil},
	}

	for _, a := range actions {
		t.Run(a.method+" "+a.path, func(t *testing.T) {
			rec := doRequestAsUser(t, srv, actor, a.method, a.path, a.body)
			assert.Equal(t, http.StatusForbidden, rec.Code)

			// The target user ID must not appear in the response body.
			// Note: it appears in the URL path which is expected, but the
			// response body itself must not contain it.
			bodyStr := rec.Body.String()
			assert.False(t, strings.Contains(bodyStr, sensitiveID),
				"target user ID %q must not appear in 403 response body for %s %s", sensitiveID, a.method, a.path)
		})
	}
}

// ---------------------------------------------------------------------------
// Non-permission errors remain unchanged
// ---------------------------------------------------------------------------

func TestDenialShape_SelfSuspendStillConflict(t *testing.T) {
	srv, s := testServer(t)

	devUser := getDevUser(t, srv, s)
	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/users/"+devUser.ID,
		map[string]string{"status": "suspended"})

	// Self-suspend is 409 Conflict, not 403.
	assert.Equal(t, http.StatusConflict, rec.Code)
	resp := parseErrorResponse(t, rec.Body.Bytes())
	assert.Equal(t, ErrCodeConflict, resp.Error.Code)
}

func TestDenialShape_SelfDeleteStillConflict(t *testing.T) {
	srv, s := testServer(t)

	devUser := getDevUser(t, srv, s)
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/users/"+devUser.ID, nil)

	// Self-delete is 409 Conflict, not 403.
	assert.Equal(t, http.StatusConflict, rec.Code)
	resp := parseErrorResponse(t, rec.Body.Bytes())
	assert.Equal(t, ErrCodeConflict, resp.Error.Code)
}

func TestDenialShape_LastAdminStillConflict(t *testing.T) {
	srv, s := testServer(t)

	devUser := getDevUser(t, srv, s)
	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/users/"+devUser.ID,
		map[string]string{"role": "member"})

	// Self-demotion is 409 Conflict, not 403.
	assert.Equal(t, http.StatusConflict, rec.Code)
	resp := parseErrorResponse(t, rec.Body.Bytes())
	assert.Equal(t, ErrCodeConflict, resp.Error.Code)
}

func TestDenialShape_NotFoundStill404(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/users/nonexistent",
		map[string]string{"role": "admin"})

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestDenialShape_UnauthenticatedStill401(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	target := &store.User{
		ID:          tid("denial-401-target"),
		Email:       "target401@example.com",
		DisplayName: "Target 401",
		Role:        "member",
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, target))

	// PATCH without auth
	rec := doRequestNoAuth(t, srv, http.MethodPatch, "/api/v1/users/"+target.ID,
		map[string]string{"role": "admin"})
	assert.Equal(t, http.StatusUnauthorized, rec.Code)

	// DELETE without auth
	rec = doRequestNoAuth(t, srv, http.MethodDelete, "/api/v1/users/"+target.ID, nil)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}
