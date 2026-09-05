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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Explain API: provenance populated (non-empty trace)
// =============================================================================

// TestExplainAPI_ProvenancePopulated verifies that the explain API returns
// real provenance data, not an empty trace.
func TestExplainAPI_ProvenancePopulated(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:        tid("explain-prov-project"),
		Name:      "Explain Provenance Test",
		Slug:      "explain-prov",
		CreatedBy: DevUserID,
		OwnerID:   DevUserID,
	}
	require.NoError(t, s.CreateProject(ctx, project))

	body := map[string]interface{}{
		"resource": map[string]interface{}{
			"type":      "project",
			"id":        project.ID,
			"projectId": project.ID,
		},
		"action": "read",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/authz/explain", body)
	require.Equal(t, http.StatusOK, rec.Code, "unexpected status: %s", rec.Body.String())

	var resp explainResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	// The provenance must not be nil — this is the core fix.
	require.NotNil(t, resp.Provenance,
		"explain response must include provenance (not empty trace)")

	// For an admin user, there should be at least one active grant.
	assert.NotEmpty(t, resp.Provenance.Grants,
		"admin user should have at least one active grant in provenance")

	// The permission should be recorded.
	assert.NotEmpty(t, resp.Provenance.Permission,
		"provenance must record the checked permission")
}

// TestExplainAPI_DenyHasProvenance verifies that even deny decisions include
// provenance with deny reasons (not an empty trace).
func TestExplainAPI_DenyHasProvenance(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	memberID := tid("explain-deny-member")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: memberID, Email: "deny-member@test.com",
		DisplayName: "Deny Member", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, memberID)

	body := map[string]interface{}{
		"resource": map[string]interface{}{
			"type": "settings",
			"id":   "hub",
		},
		"action": "manage",
	}

	bodyBytes, _ := json.Marshal(body)
	req := newRequestWithIdentity(t, http.MethodPost, "/api/v1/authz/explain", bodyBytes,
		NewAuthenticatedUser(memberID, "deny-member@test.com", "Deny Member", "member", "api"))
	rec := httptest.NewRecorder()
	srv.handleAuthzExplain(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var resp explainResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	assert.False(t, resp.Allowed, "member should be denied manage on settings")
	require.NotNil(t, resp.Provenance, "deny decision must include provenance")
	assert.NotEmpty(t, resp.Provenance.DenyReasons,
		"deny provenance must include deny reasons, not empty trace")
}

// =============================================================================
// Explain API: cross-principal redaction
// =============================================================================

// TestExplainAPI_CrossPrincipalRedaction verifies that cross-principal
// explain requests redact sensitive fields while preserving causal structure.
func TestExplainAPI_CrossPrincipalRedaction(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a target user.
	targetID := tid("explain-redact-target")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: "redact-target@test.com",
		DisplayName: "Sensitive Name", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, targetID)

	// Create a project for the target.
	project := &store.Project{
		ID:        tid("explain-redact-project"),
		Name:      "Redact Test",
		Slug:      "redact-test",
		CreatedBy: DevUserID,
		OwnerID:   DevUserID,
	}
	require.NoError(t, s.CreateProject(ctx, project))

	// Super-admin (dev user) explains for another user.
	body := map[string]interface{}{
		"resource": map[string]interface{}{
			"type":      "project",
			"id":        project.ID,
			"projectId": project.ID,
		},
		"action":        "read",
		"principalId":   targetID,
		"principalKind": "user",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/authz/explain", body)
	require.Equal(t, http.StatusOK, rec.Code, "status: %s", rec.Body.String())

	var resp explainResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Provenance, "cross-principal explain must include provenance")

	// Verify redaction: principal IDs should be "[redacted]".
	for _, g := range resp.Provenance.Grants {
		assert.Equal(t, "[redacted]", g.PrincipalID,
			"cross-principal grant principal ID must be redacted")
		assert.Nil(t, g.MembershipPath,
			"cross-principal grant membership path must be redacted")
		// Stable identifiers should be preserved.
		assert.NotEmpty(t, g.BindingID, "binding ID should be preserved")
	}

	// Boundary names should be redacted.
	for _, r := range resp.Provenance.Restrictions {
		assert.Equal(t, "[redacted]", r.BoundaryName,
			"boundary name should be redacted in cross-principal explain")
		// Boundary ID should be preserved.
		assert.NotEmpty(t, r.BoundaryID,
			"boundary ID should be preserved as stable identifier")
	}

	// Membership path elements should be redacted.
	for _, mp := range resp.Provenance.MembershipPaths {
		for _, elem := range mp.Path {
			assert.Contains(t, elem, "[redacted]",
				"membership path element should be redacted")
		}
		// Kind should be preserved.
		assert.NotEmpty(t, mp.Kind, "path kind should be preserved")
	}

	// Response body should not contain the target's display name.
	respBody := rec.Body.String()
	assert.NotContains(t, respBody, "Sensitive Name",
		"cross-principal explain must not leak display names")
}

// =============================================================================
// Explain API: JSON error responses
// =============================================================================

// TestExplainAPI_JSONErrors verifies that the explain endpoint returns
// JSON error responses instead of plain text.
func TestExplainAPI_JSONErrors(t *testing.T) {
	srv, _ := testServer(t)

	tests := []struct {
		name      string
		method    string
		body      interface{}
		wantCode  int
		wantField string // Expected field in the JSON error.
	}{
		{
			name:      "wrong method",
			method:    http.MethodGet,
			body:      nil,
			wantCode:  http.StatusMethodNotAllowed,
			wantField: "error",
		},
		{
			name:      "missing resource type",
			method:    http.MethodPost,
			body:      map[string]interface{}{"action": "read", "resource": map[string]interface{}{}},
			wantCode:  http.StatusBadRequest,
			wantField: "error",
		},
		{
			name:      "missing action",
			method:    http.MethodPost,
			body:      map[string]interface{}{"resource": map[string]interface{}{"type": "project"}},
			wantCode:  http.StatusBadRequest,
			wantField: "error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := doRequest(t, srv, tt.method, "/api/v1/authz/explain", tt.body)
			assert.Equal(t, tt.wantCode, rec.Code)

			// Verify the response is JSON, not plain text.
			contentType := rec.Header().Get("Content-Type")
			assert.Contains(t, contentType, "application/json",
				"error response should be JSON, not plain text")

			var errResp map[string]interface{}
			err := json.Unmarshal(rec.Body.Bytes(), &errResp)
			assert.NoError(t, err, "error response should be valid JSON")
			assert.Contains(t, errResp, tt.wantField,
				"error response should contain %q field", tt.wantField)
		})
	}
}

// =============================================================================
// Explain API: inactive grants with reason
// =============================================================================

// TestExplainAPI_InactiveGrantsInProvenance verifies that grants that
// exist but did not contribute (expired, wrong scope) appear as inactive
// in the explain provenance.
func TestExplainAPI_InactiveGrantsInProvenance(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a user with an expired role binding.
	userID := tid("explain-inactive-user")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userID, Email: "inactive@test.com",
		DisplayName: "Inactive Test", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, userID)

	project := &store.Project{
		ID:        tid("explain-inactive-project"),
		Name:      "Inactive Grant Test",
		Slug:      "inactive-grant",
		CreatedBy: DevUserID,
		OwnerID:   DevUserID,
	}
	require.NoError(t, s.CreateProject(ctx, project))

	// Create a role with agent.read.
	rd, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        "test-viewer-inactive",
		Description: "Test viewer for inactive grant",
		ScopeType:   store.RoleScopeProject,
		Permissions: []string{"agent.read"},
	})
	require.NoError(t, err)

	// Create an expired binding.
	expired := time.Now().Add(-24 * time.Hour)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          project.ID,
		ExpiresAt:        &expired,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Explain the user's access. The dev user (admin) explains for the target.
	body := map[string]interface{}{
		"resource": map[string]interface{}{
			"type":      "agent",
			"id":        tid("some-agent"),
			"projectId": project.ID,
		},
		"action":        "read",
		"principalId":   userID,
		"principalKind": "user",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/authz/explain", body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp explainResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Provenance)

	// The expired binding should appear as an inactive grant.
	foundExpired := false
	for _, ig := range resp.Provenance.InactiveGrants {
		if ig.InactiveReason != "" {
			foundExpired = true
			break
		}
	}
	assert.True(t, foundExpired,
		"expired binding should appear as inactive grant with reason")
}

// =============================================================================
// Explain API: non-admin cannot explain for others (JSON response)
// =============================================================================

func TestExplainAPI_ForbiddenIsJSON(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	memberID := tid("explain-forbidden-member")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: memberID, Email: "forbidden@test.com",
		DisplayName: "Forbidden", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, memberID)

	body := map[string]interface{}{
		"resource": map[string]interface{}{
			"type": "project",
			"id":   tid("some-project"),
		},
		"action":      "read",
		"principalId": tid("another-user"),
	}

	bodyBytes, _ := json.Marshal(body)
	req := newRequestWithIdentity(t, http.MethodPost, "/api/v1/authz/explain", bodyBytes,
		NewAuthenticatedUser(memberID, "forbidden@test.com", "Forbidden", "member", "api"))
	rec := httptest.NewRecorder()
	srv.handleAuthzExplain(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)

	// Verify JSON response.
	contentType := rec.Header().Get("Content-Type")
	assert.Contains(t, contentType, "application/json",
		"forbidden response should be JSON")

	var errResp map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
	assert.Contains(t, errResp, "error",
		"forbidden response should contain error field")
}

// =============================================================================
// Explain API: effective permissions mode
// =============================================================================

func TestExplainAPI_EffectivePermissionsMode(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:        tid("explain-effperm-project"),
		Name:      "Effective Permissions Test",
		Slug:      "effperm-test",
		CreatedBy: DevUserID,
		OwnerID:   DevUserID,
	}
	require.NoError(t, s.CreateProject(ctx, project))

	// Dev user (admin) should have effective permissions.
	body := map[string]interface{}{
		"resource": map[string]interface{}{
			"type":      "project",
			"id":        project.ID,
			"projectId": project.ID,
		},
		"action": "read",
		"mode":   "effective_permissions",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/authz/explain", body)
	require.Equal(t, http.StatusOK, rec.Code, "status: %s", rec.Body.String())

	var resp explainResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	// Admin should have effective permissions.
	assert.NotEmpty(t, resp.EffectivePermissions,
		"admin should have effective permissions in the response")

	// Each permission should have provenance.
	for _, pp := range resp.EffectivePermissions {
		assert.NotEmpty(t, pp.PermissionID,
			"each permission provenance must have a permission ID")
	}
}

// =============================================================================
// Explain API: ComparePrincipalID authorization (C1 fix)
// =============================================================================

// TestExplainAPI_ComparePrincipalID_RequiresAuditRead verifies that a
// non-admin user receives 403 when setting comparePrincipalId in
// effective_permissions mode (C1 fix).
func TestExplainAPI_ComparePrincipalID_RequiresAuditRead(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a non-admin user.
	memberID := tid("explain-compare-member")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: memberID, Email: "compare-member@test.com",
		DisplayName: "Compare Member", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, memberID)

	// Non-admin tries to compare their permissions with another principal.
	body := map[string]interface{}{
		"resource": map[string]interface{}{
			"type": "project",
			"id":   tid("some-project"),
		},
		"action":             "read",
		"mode":               "effective_permissions",
		"comparePrincipalId": tid("another-user"),
	}

	bodyBytes, _ := json.Marshal(body)
	req := newRequestWithIdentity(t, http.MethodPost, "/api/v1/authz/explain", bodyBytes,
		NewAuthenticatedUser(memberID, "compare-member@test.com", "Compare Member", "member", "api"))
	rec := httptest.NewRecorder()
	srv.handleAuthzExplain(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code,
		"non-admin must be denied comparePrincipalId access")

	// Verify JSON error response.
	var errResp map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
	assert.Contains(t, errResp, "error",
		"forbidden response should contain error field")
}

// TestExplainAPI_ComparePrincipalID_AdminAllowed verifies that an admin
// user can use comparePrincipalId in effective_permissions mode.
func TestExplainAPI_ComparePrincipalID_AdminAllowed(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a target user for comparison.
	targetID := tid("explain-compare-target")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: "compare-target@test.com",
		DisplayName: "Compare Target", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, targetID)

	project := &store.Project{
		ID:        tid("explain-compare-project"),
		Name:      "Compare Test",
		Slug:      "compare-test",
		CreatedBy: DevUserID,
		OwnerID:   DevUserID,
	}
	require.NoError(t, s.CreateProject(ctx, project))

	// Admin (dev user) compares with another user.
	body := map[string]interface{}{
		"resource": map[string]interface{}{
			"type":      "project",
			"id":        project.ID,
			"projectId": project.ID,
		},
		"action":               "read",
		"mode":                 "effective_permissions",
		"comparePrincipalId":   targetID,
		"comparePrincipalKind": "user",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/authz/explain", body)
	require.Equal(t, http.StatusOK, rec.Code, "admin should be allowed: %s", rec.Body.String())

	var resp explainResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	// Comparison result should be present.
	require.NotNil(t, resp.CompareResult,
		"admin comparison should return CompareResult")
}
