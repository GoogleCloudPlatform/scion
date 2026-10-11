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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// b7TestServer creates a test server with B3-B6 boundary services wired.
// The dev user is automatically granted constraint admin + read permissions.
func b7TestServer(t *testing.T) (*Server, store.Store) {
	t.Helper()
	srv, s := testServer(t)

	// The dev user is already seeded by testServer → New() → seedDevUser() with
	// a super-admin role binding. Super-admin includes all permissions from the
	// registry, but access_constraint.admin is intentionally excluded from the
	// hub-admin built-in role (it requires explicit binding). For tests that need
	// admin-level access, add a dedicated role binding.
	rd := createTestRoleDefinition(t, s, "test-boundary-admin", store.RoleScopeSystem,
		[]string{PermissionConstraintAdmin, PermissionConstraintRead,
			"agent.read", "agent.create", "agent.delete", "project.read"})
	pvSeedRoleBinding(t, s, rd.ID, "user", DevUserID, store.RoleScopeSystem, "")

	// Ensure B3-B6 services are initialized.
	if srv.previewService == nil {
		srv.initBoundaryServices()
	}

	return srv, s
}

// b7SeedConstraint creates a constraint directly in the store for testing reads.
// Targets a specific seeded user to avoid restricting the dev admin.
func b7SeedConstraint(t *testing.T, s store.Store, name string) *store.AccessConstraint {
	t.Helper()

	// Seed a target user for the constraint subject.
	targetUserID := pvSeedUser(t, s, "constraint-target-"+name)

	principalType := "user"
	c := &store.AccessConstraint{
		Name:                 name,
		Purpose:              "Test constraint",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: &principalType,
		SubjectPrincipalID:   &targetUserID,
		ScopeType:            store.RoleScopeSystem,
		ScopeID:              "",
		MaximumPermissions:   []string{"agent.read", "agent.create"},
		CreatedBy:            DevUserID,
		UpdatedBy:            DevUserID,
	}
	created, err := s.CreateAccessConstraint(t.Context(), c)
	require.NoError(t, err)
	return created
}

// doRequestHeaders performs an HTTP request with custom headers against the test server.
func doRequestHeaders(t *testing.T, srv *Server, method, path string, body interface{}, headers map[string]string) *httptest.ResponseRecorder {
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
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// ---------------------------------------------------------------------------
// 1. List endpoint with cursor/filter/sort
// ---------------------------------------------------------------------------

func TestB7_ListAccessConstraints(t *testing.T) {
	srv, s := b7TestServer(t)

	// Seed several constraints.
	b7SeedConstraint(t, s, "alpha-boundary")
	b7SeedConstraint(t, s, "beta-boundary")
	b7SeedConstraint(t, s, "gamma-boundary")

	resp := doRequest(t, srv, http.MethodGet, "/api/v1/admin/access-constraints", nil)
	assert.Equal(t, http.StatusOK, resp.Code, "body: %s", resp.Body.String())

	var result accessBoundaryListResponse
	err := json.Unmarshal(resp.Body.Bytes(), &result)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, result.TotalCount, 3)
	assert.GreaterOrEqual(t, len(result.Items), 3)

	// Check resolved data is present (WP0 shape).
	for _, item := range result.Items {
		assert.NotEmpty(t, item.ID, "ID should be set")
		assert.NotEmpty(t, item.Name, "Name should be set")
		assert.NotEmpty(t, item.Subject.Kind, "Subject.Kind should be set")
		assert.NotEmpty(t, item.Status, "Status should be set")
		assert.NotEmpty(t, item.Revision, "Revision should be opaque string")
		assert.NotNil(t, item.Risk, "Risk should be present (may be empty)")
		assert.NotEmpty(t, item.Health.State, "Health.State should be set")
		assert.NotNil(t, item.CreatedBy, "CreatedBy should be PrincipalRef")
	}

	// R4.7: Collection-level _capabilities should be present.
	assert.NotNil(t, result.Capabilities, "list should include collection-level _capabilities")
	assert.NotNil(t, result.Capabilities.Actions, "collection _capabilities should have actions")
}

func TestB7_ListAccessConstraints_WithFilter(t *testing.T) {
	srv, s := b7TestServer(t)
	b7SeedConstraint(t, s, "filtered-boundary")

	// Filter by subject kind.
	resp := doRequest(t, srv, http.MethodGet,
		"/api/v1/admin/access-constraints?subjectKind=principal", nil)
	assert.Equal(t, http.StatusOK, resp.Code)

	var result accessBoundaryListResponse
	err := json.Unmarshal(resp.Body.Bytes(), &result)
	require.NoError(t, err)
	for _, item := range result.Items {
		assert.Equal(t, "principal", item.Subject.Kind)
	}
}

func TestB7_ListAccessConstraints_Pagination(t *testing.T) {
	srv, s := b7TestServer(t)

	// Seed enough constraints.
	for i := 0; i < 5; i++ {
		b7SeedConstraint(t, s, fmt.Sprintf("page-boundary-%d", i))
	}

	// First page.
	resp := doRequest(t, srv, http.MethodGet,
		"/api/v1/admin/access-constraints?pageSize=2", nil)
	assert.Equal(t, http.StatusOK, resp.Code)

	var result accessBoundaryListResponse
	err := json.Unmarshal(resp.Body.Bytes(), &result)
	require.NoError(t, err)
	assert.Equal(t, 2, len(result.Items))
	assert.NotEmpty(t, result.NextPageToken, "should have next page token")
}

// ---------------------------------------------------------------------------
// 2. Detail endpoint
// ---------------------------------------------------------------------------

func TestB7_GetAccessConstraint(t *testing.T) {
	srv, s := b7TestServer(t)
	created := b7SeedConstraint(t, s, "detail-boundary")

	resp := doRequest(t, srv, http.MethodGet,
		"/api/v1/admin/access-constraints/"+created.ID, nil)
	assert.Equal(t, http.StatusOK, resp.Code, "body: %s", resp.Body.String())

	var detail accessBoundaryDetail
	err := json.Unmarshal(resp.Body.Bytes(), &detail)
	require.NoError(t, err)
	assert.Equal(t, created.ID, detail.ID)
	assert.Equal(t, "detail-boundary", detail.Name)
	assert.NotEmpty(t, detail.Status)
	assert.NotNil(t, detail.Provenance)
	assert.Contains(t, detail.Provenance.AuditURL, created.ID)

	// R4.1: Revision should be opaque string.
	assert.Equal(t, strconv.FormatInt(created.Revision, 10), detail.Revision)

	// R4.2: Health should use WP0 shape.
	assert.NotEmpty(t, detail.Health.State)
	assert.NotNil(t, detail.Health.UnresolvedReferences)

	// R4.3: CreatedBy/UpdatedBy should be PrincipalRef.
	assert.NotNil(t, detail.CreatedBy)
	assert.NotEmpty(t, detail.CreatedBy.Type)
	assert.NotEmpty(t, detail.CreatedBy.ID)

	// R4.4: Risk should be present (empty array is ok).
	assert.NotNil(t, detail.Risk)

	// R4.6: AppliesWhen should be present.
	// (no time bounds set, so both nil)

	// ETag header should be set (opaque string).
	etag := resp.Header().Get("ETag")
	assert.NotEmpty(t, etag, "ETag header should be set")
	assert.Contains(t, etag, strconv.FormatInt(created.Revision, 10))
}

func TestB7_GetAccessConstraint_NotFound(t *testing.T) {
	srv, _ := b7TestServer(t)

	resp := doRequest(t, srv, http.MethodGet,
		"/api/v1/admin/access-constraints/nonexistent", nil)
	assert.Equal(t, http.StatusNotFound, resp.Code)
}

// ---------------------------------------------------------------------------
// 3. Affected-principals subresource
// ---------------------------------------------------------------------------

func TestB7_GetAffectedPrincipals(t *testing.T) {
	srv, s := b7TestServer(t)
	created := b7SeedConstraint(t, s, "affected-boundary")

	resp := doRequest(t, srv, http.MethodGet,
		"/api/v1/admin/access-constraints/"+created.ID+"/affected-principals", nil)
	assert.Equal(t, http.StatusOK, resp.Code, "body: %s", resp.Body.String())

	var result affectedPrincipalsResponse
	err := json.Unmarshal(resp.Body.Bytes(), &result)
	require.NoError(t, err)
	assert.NotNil(t, result.Items)
}

func TestB7_GetAffectedPrincipals_NotFound(t *testing.T) {
	srv, _ := b7TestServer(t)

	resp := doRequest(t, srv, http.MethodGet,
		"/api/v1/admin/access-constraints/nonexistent/affected-principals", nil)
	assert.Equal(t, http.StatusNotFound, resp.Code)
}

// ---------------------------------------------------------------------------
// 4. Audit subresource
// ---------------------------------------------------------------------------

func TestB7_GetConstraintAudit(t *testing.T) {
	srv, s := b7TestServer(t)
	created := b7SeedConstraint(t, s, "audit-boundary")

	resp := doRequest(t, srv, http.MethodGet,
		"/api/v1/admin/access-constraints/"+created.ID+"/audit", nil)
	assert.Equal(t, http.StatusOK, resp.Code, "body: %s", resp.Body.String())

	var result auditListResponse
	err := json.Unmarshal(resp.Body.Bytes(), &result)
	require.NoError(t, err)
	// No audit entries for a constraint created directly in the store.
	assert.Equal(t, 0, result.TotalCount)
	assert.NotNil(t, result.Items)
}

func TestB7_GetConstraintAudit_NotFound(t *testing.T) {
	srv, _ := b7TestServer(t)

	resp := doRequest(t, srv, http.MethodGet,
		"/api/v1/admin/access-constraints/nonexistent/audit", nil)
	assert.Equal(t, http.StatusNotFound, resp.Code)
}

// ---------------------------------------------------------------------------
// 5. Preview endpoint
// ---------------------------------------------------------------------------

func TestB7_CreatePreview(t *testing.T) {
	srv, s := b7TestServer(t)
	targetUserID := pvSeedUser(t, s, "preview-target")

	body := previewCreateRequest{
		Operation: "create",
		Draft: &previewDraftRequest{
			Name:    "preview-test",
			Purpose: "Test preview",
			Subject: subjectSelectorRequest{
				Kind:          "principal",
				PrincipalType: "user",
				PrincipalID:   targetUserID,
			},
			Scope: constraintScopeRequest{
				Type: "system",
			},
			MaximumPermissions: []string{"agent.read"},
		},
	}

	resp := doRequest(t, srv, http.MethodPost,
		"/api/v1/admin/access-constraint-previews", body)
	assert.Equal(t, http.StatusOK, resp.Code, "body: %s", resp.Body.String())

	var result PreviewResult
	err := json.Unmarshal(resp.Body.Bytes(), &result)
	require.NoError(t, err)
	assert.NotEmpty(t, result.PreviewToken, "should have preview token")
	assert.NotEmpty(t, result.PreviewID, "should have preview ID")
	assert.Equal(t, "create", result.Operation)
	assert.NotEmpty(t, result.Classification)
}

func TestB7_CreatePreview_InvalidOperation(t *testing.T) {
	srv, _ := b7TestServer(t)

	body := previewCreateRequest{
		Operation: "invalid",
	}

	resp := doRequest(t, srv, http.MethodPost,
		"/api/v1/admin/access-constraint-previews", body)
	assert.NotEqual(t, http.StatusOK, resp.Code)
}

// ---------------------------------------------------------------------------
// 6. Preview-bound mutations
// ---------------------------------------------------------------------------

// TestB7_CreateRequiresPreviewToken verifies the preview-bound contract: no
// raw CRUD bypass.
func TestB7_CreateRequiresPreviewToken(t *testing.T) {
	srv, s := b7TestServer(t)
	targetUserID := pvSeedUser(t, s, "no-token-target")

	body := accessConstraintCreateRequest{
		Name:    "no-token",
		Purpose: "Test constraint",
		Subject: subjectSelectorRequest{
			Kind:          "principal",
			PrincipalType: "user",
			PrincipalID:   targetUserID,
		},
		Scope: constraintScopeRequest{
			Type: "system",
		},
		MaximumPermissions: []string{"agent.read"},
		// PreviewToken omitted.
	}

	resp := doRequest(t, srv, http.MethodPost,
		"/api/v1/admin/access-constraints", body)
	assert.Equal(t, http.StatusBadRequest, resp.Code)

	var errResp ErrorResponse
	err := json.Unmarshal(resp.Body.Bytes(), &errResp)
	require.NoError(t, err)
	assert.Contains(t, errResp.Error.Message, "previewToken is required")
}

// TestB7_UpdateRequiresIfMatch verifies that PUT requires an If-Match header.
func TestB7_UpdateRequiresIfMatch(t *testing.T) {
	srv, s := b7TestServer(t)
	created := b7SeedConstraint(t, s, "update-no-ifmatch")

	body := accessConstraintUpdateRequest{
		Name:    "updated-name",
		Purpose: "Updated purpose",
		Subject: subjectSelectorRequest{
			Kind: "all_principals",
		},
		Scope: constraintScopeRequest{
			Type: "system",
		},
		MaximumPermissions: []string{"agent.read"},
		PreviewToken:       "some-token",
	}

	resp := doRequest(t, srv, http.MethodPut,
		"/api/v1/admin/access-constraints/"+created.ID, body)
	// Should fail because no If-Match header (doRequest doesn't set it).
	assert.Equal(t, http.StatusPreconditionRequired, resp.Code, "body: %s", resp.Body.String())
}

// TestB7_UpdateRequiresPreviewToken verifies update needs preview token.
func TestB7_UpdateRequiresPreviewToken(t *testing.T) {
	srv, s := b7TestServer(t)
	created := b7SeedConstraint(t, s, "update-no-token")

	body := accessConstraintUpdateRequest{
		Name:    "updated-name",
		Purpose: "Updated purpose",
		Subject: subjectSelectorRequest{
			Kind: "all_principals",
		},
		Scope: constraintScopeRequest{
			Type: "system",
		},
		MaximumPermissions: []string{"agent.read"},
		// PreviewToken omitted.
	}

	resp := doRequestHeaders(t, srv, http.MethodPut,
		"/api/v1/admin/access-constraints/"+created.ID, body,
		map[string]string{"If-Match": fmt.Sprintf(`"%d"`, created.Revision)})
	assert.Equal(t, http.StatusBadRequest, resp.Code, "body: %s", resp.Body.String())
}

// TestB7_DeleteRequiresPreviewToken verifies delete needs preview token.
func TestB7_DeleteRequiresPreviewToken(t *testing.T) {
	srv, s := b7TestServer(t)
	created := b7SeedConstraint(t, s, "delete-no-token")

	resp := doRequest(t, srv, http.MethodDelete,
		"/api/v1/admin/access-constraints/"+created.ID, nil)
	assert.Equal(t, http.StatusBadRequest, resp.Code, "body: %s", resp.Body.String())
}

// TestB7_RecoveryDisabledImmutable verifies that mutation on disabled constraints
// returns recovery_disabled_immutable.
func TestB7_RecoveryDisabledImmutable(t *testing.T) {
	srv, s := b7TestServer(t)
	created := b7SeedConstraint(t, s, "disabled-boundary")

	// Disable the constraint.
	err := s.DisableAccessConstraint(t.Context(), created.ID)
	require.NoError(t, err)

	// Try to update.
	body := accessConstraintUpdateRequest{
		Name:    "updated-name",
		Purpose: "Updated purpose",
		Subject: subjectSelectorRequest{
			Kind: "all_principals",
		},
		Scope: constraintScopeRequest{
			Type: "system",
		},
		MaximumPermissions: []string{"agent.read"},
		PreviewToken:       "some-token",
	}

	resp := doRequestHeaders(t, srv, http.MethodPut,
		"/api/v1/admin/access-constraints/"+created.ID, body,
		map[string]string{"If-Match": fmt.Sprintf(`"%d"`, created.Revision)})
	assert.Equal(t, http.StatusConflict, resp.Code, "body: %s", resp.Body.String())

	var errResp ErrorResponse
	err = json.Unmarshal(resp.Body.Bytes(), &errResp)
	require.NoError(t, err)
	assert.Equal(t, ErrCodeRecoveryDisabledImmutable, errResp.Error.Code)
}

// TestB7_RecoveryDisabledDeleteImmutable verifies that delete on disabled
// constraint returns recovery_disabled_immutable.
func TestB7_RecoveryDisabledDeleteImmutable(t *testing.T) {
	srv, s := b7TestServer(t)
	created := b7SeedConstraint(t, s, "disabled-delete-boundary")

	err := s.DisableAccessConstraint(t.Context(), created.ID)
	require.NoError(t, err)

	// R3: Try to delete with a preview token via X-Preview-Token header
	// (query param fallback was removed — tokens must not appear in URLs).
	resp := doRequestHeaders(t, srv, http.MethodDelete,
		"/api/v1/admin/access-constraints/"+created.ID, nil,
		map[string]string{"X-Preview-Token": "some-token"})
	assert.Equal(t, http.StatusConflict, resp.Code, "body: %s", resp.Body.String())

	var errResp ErrorResponse
	err = json.Unmarshal(resp.Body.Bytes(), &errResp)
	require.NoError(t, err)
	assert.Equal(t, ErrCodeRecoveryDisabledImmutable, errResp.Error.Code)
}

// ---------------------------------------------------------------------------
// 7. Error codes
// ---------------------------------------------------------------------------

func TestB7_ErrorCodes_Defined(t *testing.T) {
	// Verify all 12 required error codes are defined.
	codes := map[string]string{
		"constraint_admin_lockout":                     ErrCodeConstraintAdminLockout,
		"stale_authorization_preview":                  ErrCodeStaleAuthorizationPreview,
		"preview_incomplete":                           ErrCodePreviewIncomplete,
		"resolution_failed":                            ErrCodeResolutionFailed,
		"subject_not_found":                            ErrCodeSubjectNotFound,
		"scope_not_found":                              ErrCodeScopeNotFound,
		"scope_mismatch":                               ErrCodeScopeMismatch,
		"permission_registry_changed":                  ErrCodePermissionRegistryChanged,
		"insufficient_constraint_relaxation_authority": ErrCodeInsufficientRelaxationAuthority,
		"mutation_permission_lost":                     ErrCodeMutationPermissionLost,
		"revision_conflict":                            ErrCodeRevisionConflict,
		"recovery_disabled_immutable":                  ErrCodeRecoveryDisabledImmutable,
	}

	for expected, actual := range codes {
		assert.Equal(t, expected, actual,
			"error code constant should match expected value")
	}
}

func TestB7_ErrorResponseFormat(t *testing.T) {
	srv, _ := b7TestServer(t)

	// Trigger a known error.
	resp := doRequest(t, srv, http.MethodGet,
		"/api/v1/admin/access-constraints/nonexistent", nil)
	assert.Equal(t, http.StatusNotFound, resp.Code)

	var errResp ErrorResponse
	err := json.Unmarshal(resp.Body.Bytes(), &errResp)
	require.NoError(t, err)
	assert.NotEmpty(t, errResp.Error.Code)
	assert.NotEmpty(t, errResp.Error.Message)
}

// ---------------------------------------------------------------------------
// 8. Route metadata and permissions
// ---------------------------------------------------------------------------

func TestB7_RouteMetadata_ReadPermission(t *testing.T) {
	// Verify the route metadata uses access_constraint.read for list/detail.
	meta, ok := routeMetadataTable["/api/v1/admin/access-constraints"]
	require.True(t, ok, "list route must be in metadata table")
	assert.Equal(t, "access_constraint.read", meta.Permission)

	meta, ok = routeMetadataTable["/api/v1/admin/access-constraints/"]
	require.True(t, ok, "detail route must be in metadata table")
	assert.Equal(t, "access_constraint.read", meta.Permission)
}

func TestB7_RouteMetadata_PreviewPermission(t *testing.T) {
	meta, ok := routeMetadataTable["/api/v1/admin/access-constraint-previews"]
	require.True(t, ok, "preview route must be in metadata table")
	assert.Equal(t, "access_constraint.admin", meta.Permission)
}

// ---------------------------------------------------------------------------
// 9. Capabilities
// ---------------------------------------------------------------------------

func TestB7_DetailIncludesCapabilities(t *testing.T) {
	srv, s := b7TestServer(t)
	created := b7SeedConstraint(t, s, "caps-boundary")

	resp := doRequest(t, srv, http.MethodGet,
		"/api/v1/admin/access-constraints/"+created.ID, nil)
	assert.Equal(t, http.StatusOK, resp.Code, "body: %s", resp.Body.String())

	// Parse and check that _capabilities is present with actions array (R1).
	var raw map[string]json.RawMessage
	err := json.Unmarshal(resp.Body.Bytes(), &raw)
	require.NoError(t, err)
	capsJSON, hasCaps := raw["_capabilities"]
	assert.True(t, hasCaps, "detail response should include _capabilities")

	// Verify it uses the WP0 actions array shape, not boolean fields.
	var caps wpCapabilities
	err = json.Unmarshal(capsJSON, &caps)
	require.NoError(t, err)
	assert.NotNil(t, caps.Actions, "_capabilities should have actions array")
	assert.Contains(t, caps.Actions, "read", "actions should include 'read'")
}

func TestB7_ListIncludesCapabilities(t *testing.T) {
	srv, s := b7TestServer(t)
	b7SeedConstraint(t, s, "list-caps-boundary")

	resp := doRequest(t, srv, http.MethodGet,
		"/api/v1/admin/access-constraints", nil)
	assert.Equal(t, http.StatusOK, resp.Code)

	var result accessBoundaryListResponse
	err := json.Unmarshal(resp.Body.Bytes(), &result)
	require.NoError(t, err)
	for _, item := range result.Items {
		assert.NotNil(t, item.Capabilities, "each list item should include _capabilities")
		assert.NotNil(t, item.Capabilities.Actions, "item _capabilities should have actions array")
	}

	// R4.7: Collection-level _capabilities.
	assert.NotNil(t, result.Capabilities, "list should have collection-level _capabilities")
}

// ---------------------------------------------------------------------------
// 10. Status computation
// ---------------------------------------------------------------------------

func TestB7_StatusComputation(t *testing.T) {
	now := time.Now()
	past := now.Add(-1 * time.Hour)
	future := now.Add(1 * time.Hour)

	tests := []struct {
		name     string
		sc       *store.AccessConstraint
		expected string
	}{
		{
			name:     "active - no time bounds",
			sc:       &store.AccessConstraint{},
			expected: "active",
		},
		{
			name:     "scheduled - future notBefore",
			sc:       &store.AccessConstraint{NotBefore: &future},
			expected: "scheduled",
		},
		{
			name:     "expired",
			sc:       &store.AccessConstraint{ExpiresAt: &past},
			expected: "expired",
		},
		{
			name:     "recovery_disabled",
			sc:       &store.AccessConstraint{Disabled: true},
			expected: "recovery_disabled",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hc := storeToHubAccessConstraint(tt.sc)
			status := computeConstraintStatus(tt.sc, hc)
			assert.Equal(t, tt.expected, status)
		})
	}
}

// ---------------------------------------------------------------------------
// 11. Health computation
// ---------------------------------------------------------------------------

func TestB7_HealthComputation(t *testing.T) {
	t.Run("healthy", func(t *testing.T) {
		hc := &AccessConstraint{Degraded: false}
		health := computeWPHealth(hc)
		assert.Equal(t, "healthy", health.State)
		assert.Empty(t, health.UnresolvedReferences)
	})

	t.Run("degraded", func(t *testing.T) {
		hc := &AccessConstraint{Degraded: true}
		health := computeWPHealth(hc)
		assert.Equal(t, "degraded", health.State)
		assert.NotEmpty(t, health.UnresolvedReferences)
	})

	t.Run("nil", func(t *testing.T) {
		health := computeWPHealth(nil)
		assert.Equal(t, "degraded", health.State)
		assert.NotEmpty(t, health.UnresolvedReferences)
	})
}

// ---------------------------------------------------------------------------
// 12. If-Match header parsing
// ---------------------------------------------------------------------------

func TestB7_ParseIfMatchRevision(t *testing.T) {
	tests := []struct {
		input    string
		expected int64
		wantErr  bool
	}{
		{`"1"`, 1, false},
		{`"42"`, 42, false},
		{"5", 5, false},
		{`"0"`, 0, true}, // Must be positive.
		{`"*"`, 0, true},
		{"", 0, true},
		{"abc", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			rev, err := parseIfMatchRevision(tt.input)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.expected, rev)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 13. Governance error mapping
// ---------------------------------------------------------------------------

func TestB7_GovernanceErrorStatus(t *testing.T) {
	tests := []struct {
		code     string
		expected int
	}{
		{ErrCodeConstraintAdminLockout, http.StatusConflict},
		{ErrCodeStaleAuthorizationPreview, http.StatusConflict},
		{ErrCodePreviewIncomplete, http.StatusConflict},
		{ErrCodeInsufficientRelaxationAuthority, http.StatusForbidden},
		{ErrCodeMutationPermissionLost, http.StatusForbidden},
		{ErrCodeRevisionConflict, http.StatusConflict},
		{ErrCodeRecoveryDisabledImmutable, http.StatusConflict},
		{ErrCodeInvalidRequest, http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.code, func(t *testing.T) {
			assert.Equal(t, tt.expected, governanceErrorStatus(tt.code))
		})
	}
}

func TestB7_TokenErrorStatus(t *testing.T) {
	tests := []struct {
		code     string
		expected int
	}{
		{ErrCodePreviewTokenExpired, http.StatusConflict},
		{ErrCodePreviewTokenReplay, http.StatusConflict},
		{ErrCodePreviewActorMismatch, http.StatusForbidden},
		{ErrCodePreviewOperationMismatch, http.StatusBadRequest},
		{ErrCodePreviewDraftModified, http.StatusConflict},
		{ErrCodePreviewRevisionMismatch, http.StatusConflict},
		{ErrCodePreviewStateMismatch, http.StatusConflict},
		{ErrCodePreviewIncomplete, http.StatusConflict},
		{ErrCodePreviewTokenInvalid, http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.code, func(t *testing.T) {
			assert.Equal(t, tt.expected, tokenErrorStatus(tt.code))
		})
	}
}

// ---------------------------------------------------------------------------
// 14. Full preview + create + verify flow
// ---------------------------------------------------------------------------

func TestB7_PreviewAndCreate_FullFlow(t *testing.T) {
	srv, s := b7TestServer(t)

	// Seed a target user so we can create a principal-scoped constraint
	// that doesn't lock out the admin.
	targetUserID := pvSeedUser(t, s, "flow-target")

	// Step 1: Generate preview.
	previewBody := previewCreateRequest{
		Operation: "create",
		Draft: &previewDraftRequest{
			Name:    "full-flow-boundary",
			Purpose: "Test full flow",
			Subject: subjectSelectorRequest{
				Kind:          "principal",
				PrincipalType: "user",
				PrincipalID:   targetUserID,
			},
			Scope: constraintScopeRequest{
				Type: "system",
			},
			MaximumPermissions: []string{"agent.read", "agent.create"},
		},
	}

	previewResp := doRequest(t, srv, http.MethodPost,
		"/api/v1/admin/access-constraint-previews", previewBody)
	assert.Equal(t, http.StatusOK, previewResp.Code, "preview body: %s", previewResp.Body.String())

	var preview PreviewResult
	err := json.Unmarshal(previewResp.Body.Bytes(), &preview)
	require.NoError(t, err)
	require.NotEmpty(t, preview.PreviewToken)

	// Step 2: Create using the preview token.
	createBody := accessConstraintCreateRequest{
		Name:    "full-flow-boundary",
		Purpose: "Test full flow",
		Subject: subjectSelectorRequest{
			Kind:          "principal",
			PrincipalType: "user",
			PrincipalID:   targetUserID,
		},
		Scope: constraintScopeRequest{
			Type: "system",
		},
		MaximumPermissions: []string{"agent.read", "agent.create"},
		PreviewToken:       preview.PreviewToken,
	}

	createResp := doRequest(t, srv, http.MethodPost,
		"/api/v1/admin/access-constraints", createBody)
	assert.Equal(t, http.StatusCreated, createResp.Code, "create body: %s", createResp.Body.String())

	var created mutationResponse
	err = json.Unmarshal(createResp.Body.Bytes(), &created)
	require.NoError(t, err)
	assert.NotEmpty(t, created.ID)
	assert.Equal(t, "full-flow-boundary", created.Name)
	assert.NotEmpty(t, created.AuditID, "mutation must return an audit ID")

	// Step 3: Verify the constraint is retrievable.
	getResp := doRequest(t, srv, http.MethodGet,
		"/api/v1/admin/access-constraints/"+created.ID, nil)
	assert.Equal(t, http.StatusOK, getResp.Code)
}

// TestB7_PreviewTokenReplay verifies that a preview token cannot be reused.
func TestB7_PreviewTokenReplay(t *testing.T) {
	srv, s := b7TestServer(t)
	targetUserID := pvSeedUser(t, s, "replay-target")

	// Generate preview.
	previewBody := previewCreateRequest{
		Operation: "create",
		Draft: &previewDraftRequest{
			Name:    "replay-boundary",
			Purpose: "Test replay",
			Subject: subjectSelectorRequest{
				Kind:          "principal",
				PrincipalType: "user",
				PrincipalID:   targetUserID,
			},
			Scope: constraintScopeRequest{
				Type: "system",
			},
			MaximumPermissions: []string{"agent.read"},
		},
	}

	previewResp := doRequest(t, srv, http.MethodPost,
		"/api/v1/admin/access-constraint-previews", previewBody)
	require.Equal(t, http.StatusOK, previewResp.Code)

	var preview PreviewResult
	err := json.Unmarshal(previewResp.Body.Bytes(), &preview)
	require.NoError(t, err)

	// First create succeeds.
	createBody := accessConstraintCreateRequest{
		Name:    "replay-boundary",
		Purpose: "Test replay",
		Subject: subjectSelectorRequest{
			Kind:          "principal",
			PrincipalType: "user",
			PrincipalID:   targetUserID,
		},
		Scope: constraintScopeRequest{
			Type: "system",
		},
		MaximumPermissions: []string{"agent.read"},
		PreviewToken:       preview.PreviewToken,
	}

	firstResp := doRequest(t, srv, http.MethodPost,
		"/api/v1/admin/access-constraints", createBody)
	assert.Equal(t, http.StatusCreated, firstResp.Code, "first create should succeed: %s", firstResp.Body.String())

	// Second attempt with same token should fail.
	createBody.Name = "replay-boundary-2"        // Different name to avoid duplicate name error.
	createBody.Purpose = "Test replay attempt 2" // Keep purpose non-empty.
	secondResp := doRequest(t, srv, http.MethodPost,
		"/api/v1/admin/access-constraints", createBody)
	assert.NotEqual(t, http.StatusCreated, secondResp.Code, "replay should be rejected: %s", secondResp.Body.String())
}

// ---------------------------------------------------------------------------
// 15. Unauthenticated access
// ---------------------------------------------------------------------------

func TestB7_UnauthenticatedAccess(t *testing.T) {
	srv, _ := b7TestServer(t)

	// List should require auth.
	resp := doRequestNoAuth(t, srv, http.MethodGet,
		"/api/v1/admin/access-constraints", nil)
	assert.Equal(t, http.StatusUnauthorized, resp.Code)

	// Preview should require auth.
	resp = doRequestNoAuth(t, srv, http.MethodPost,
		"/api/v1/admin/access-constraint-previews", nil)
	assert.Equal(t, http.StatusUnauthorized, resp.Code)
}

// ---------------------------------------------------------------------------
// 16. Strict JSON: unknown fields rejected (R2)
// ---------------------------------------------------------------------------

func TestB7_StrictJSON_RejectsUnknownFields(t *testing.T) {
	srv, s := b7TestServer(t)
	_ = pvSeedUser(t, s, "strict-json-target")

	// Build a valid preview-create request with an extra unknown field.
	// Use raw JSON so we can inject a field not in previewCreateRequest.
	rawBody := `{
		"operation": "create",
		"draft": {
			"name": "strict-json-test",
			"purpose": "Test strict JSON",
			"subject": {"kind": "all_principals"},
			"scope": {"type": "system"},
			"maximumPermissions": ["agent.read"]
		},
		"bogus": true
	}`

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/access-constraint-previews",
		bytes.NewBufferString(rawBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testDevToken)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code,
		"request with unknown field should be rejected; body: %s", rec.Body.String())

	// Verify the error message mentions the unknown field.
	var errResp ErrorResponse
	err := json.Unmarshal(rec.Body.Bytes(), &errResp)
	require.NoError(t, err)
	assert.Contains(t, errResp.Error.Message, "bogus",
		"error message should mention the unknown field name")
}

// ---------------------------------------------------------------------------
// 17. Method not allowed
// ---------------------------------------------------------------------------

func TestB7_MethodNotAllowed(t *testing.T) {
	srv, _ := b7TestServer(t)

	// PATCH is not allowed on constraints (use PUT instead).
	resp := doRequest(t, srv, http.MethodPatch,
		"/api/v1/admin/access-constraints/some-id", nil)
	assert.Equal(t, http.StatusMethodNotAllowed, resp.Code)
}
