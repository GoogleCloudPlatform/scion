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

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Unit tests: derivePermissionID (production enforcement — base behavior)
// =============================================================================

// TestDerivePermissionID_BaseUnchanged verifies that the production
// derivePermissionID function retains its original behavior: exact
// (Resource, Action) match or string-concatenation fallback.
// This is intentionally NOT enhanced with canonicalization — the
// production enforcement path must remain exactly as-is.
func TestDerivePermissionID_BaseUnchanged(t *testing.T) {
	tests := []struct {
		name     string
		resource string
		action   Action
		wantID   string
	}{
		// Primary lookup: exact (Resource, Action) match.
		{"canonical user.read", "user", ActionRead, "user.read"},
		{"canonical user.list", "user", ActionList, "user.list"},
		{"canonical agent.create", "agent", ActionCreate, "agent.create"},
		{"canonical project.read", "project", ActionRead, "project.read"},

		// Fallback: non-canonical combinations produce concatenated IDs.
		// This is the expected base behavior — production code never hits
		// these paths because route middleware supplies correct inputs.
		{"hub+user.read fallback", "hub", "user.read", "hub.user.read"},
		{"hub.user+read fallback", "hub.user", ActionRead, "hub.user.read"},
		{"unknown+unknown fallback", "widget", "frobnicate", "widget.frobnicate"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := derivePermissionID(tt.resource, tt.action)
			assert.Equal(t, tt.wantID, got)
		})
	}
}

// TestDerivePermissionID_AllRegistryPermissions verifies that every canonical
// permission in the registry can be derived from its own (Resource, Action).
func TestDerivePermissionID_AllRegistryPermissions(t *testing.T) {
	// Some Resource+Action pairs appear multiple times (e.g. hub+read maps to
	// multiple hub.X.read permissions). Track which ones are ambiguous.
	seen := make(map[string]string) // "resource.action" → first ID
	for _, p := range permissions.Registry {
		key := p.Resource + "." + p.Action
		if first, ok := seen[key]; ok {
			// Ambiguous: skip — derivePermissionID returns the first match.
			t.Logf("skipping ambiguous (%s, %s): first=%s, also=%s", p.Resource, p.Action, first, p.ID)
			continue
		}
		seen[key] = p.ID

		t.Run(p.ID, func(t *testing.T) {
			got := derivePermissionID(p.Resource, Action(p.Action))
			assert.Equal(t, p.ID, got,
				"derivePermissionID(%q, %q) should return canonical ID %q", p.Resource, p.Action, p.ID)
		})
	}
}

// =============================================================================
// Unit tests: canonicalizeExplainPermission (explain-only helper)
// =============================================================================

// TestCanonicalizeExplainPermission verifies the explain-specific
// canonicalization helper that normalizes non-canonical resource.type + action
// pairs to canonical permission IDs. This helper is ONLY called from the
// explain handler — production enforcement uses derivePermissionID unmodified.
func TestCanonicalizeExplainPermission(t *testing.T) {
	tests := []struct {
		name     string
		resource string
		action   string
		wantID   string
	}{
		// Primary lookup: exact (Resource, Action) match.
		{"canonical user.read", "user", "read", "user.read"},
		{"canonical user.list", "user", "list", "user.list"},
		{"canonical agent.create", "agent", "create", "agent.create"},
		{"canonical project.read", "project", "read", "project.read"},

		// Canonicalization: action is already a canonical permission ID.
		{"hub + user.read → user.read", "hub", "user.read", "user.read"},
		{"hub + user.list → user.list", "hub", "user.list", "user.list"},
		{"hub + agent.create → agent.create", "hub", "agent.create", "agent.create"},

		// Canonicalization: constructed ID matches a canonical ID directly.
		{"hub + settings.read → hub.settings.read", "hub", "settings.read", "hub.settings.read"},
		{"hub + config.read → hub.config.read", "hub", "config.read", "hub.config.read"},

		// Prefix-strip canonicalization: dotted resource type.
		{"hub.user + read → user.read", "hub.user", "read", "user.read"},
		{"hub.agent + create → agent.create", "hub.agent", "create", "agent.create"},
		{"hub.project + read → project.read", "hub.project", "read", "project.read"},

		// Fallback: truly unknown combinations still fall through.
		{"unknown + unknown", "widget", "frobnicate", "widget.frobnicate"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := canonicalizeExplainPermission(tt.resource, tt.action)
			assert.Equal(t, tt.wantID, got)
		})
	}
}

// TestIsKnownPermission verifies the registry lookup helper.
func TestIsKnownPermission(t *testing.T) {
	assert.True(t, isKnownPermission("user.read"), "user.read is canonical")
	assert.True(t, isKnownPermission("agent.create"), "agent.create is canonical")
	assert.True(t, isKnownPermission("hub.settings.read"), "hub.settings.read is canonical")
	assert.False(t, isKnownPermission("hub.user.read"), "hub.user.read is NOT canonical")
	assert.False(t, isKnownPermission("widget.frobnicate"), "widget.frobnicate is NOT canonical")
	assert.False(t, isKnownPermission(""), "empty string is NOT canonical")
}

// =============================================================================
// Integration tests: explain API with non-canonical inputs
// =============================================================================

// TestExplainAPI_PermissionCanonicalization verifies that the explain API
// returns correct results for various non-canonical resource.type + action
// input combinations. This is the primary regression test for the
// hub.user.read mismatch bug.
func TestExplainAPI_PermissionCanonicalization(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	memberID := tid("explain-canon-user")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID:          memberID,
		Email:       "canon@test.com",
		DisplayName: "Canon Test",
		Role:        "member",
		Status:      "active",
	}))
	ensureHubMembership(ctx, s, memberID)
	identity := NewAuthenticatedUser(memberID, "canon@test.com", "Canon Test", "member", "api")

	tests := []struct {
		name         string
		resourceType string
		action       string
		wantAllowed  bool
		wantPermID   string // Expected canonical permission in provenance.
	}{
		// Canonical inputs — hub-member has user.read.
		{"canonical user+read", "user", "read", true, "user.read"},
		{"canonical user+list", "user", "list", true, "user.list"},

		// Non-canonical: action is a full permission ID.
		{"hub+user.read canonicalizes", "hub", "user.read", true, "user.read"},
		{"hub+user.list canonicalizes", "hub", "user.list", true, "user.list"},
		{"hub+group.read canonicalizes", "hub", "group.read", true, "group.read"},

		// Non-canonical: dotted resource type.
		{"hub.user+read canonicalizes", "hub.user", "read", true, "user.read"},
		{"hub.group+read canonicalizes", "hub.group", "read", true, "group.read"},

		// Hub-member does NOT have admin permissions → denied.
		{"user+delete denied", "user", "delete", false, "user.delete"},
		{"policy+create denied", "policy", "create", false, "policy.create"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := map[string]interface{}{
				"resource": map[string]interface{}{
					"type": tt.resourceType,
					"id":   "test-resource",
				},
				"action": tt.action,
			}
			bodyBytes, _ := json.Marshal(body)
			req := newRequestWithIdentity(t, http.MethodPost, "/api/v1/authz/explain", bodyBytes, identity)
			rec := httptest.NewRecorder()
			srv.handleAuthzExplain(rec, req)

			require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

			var resp explainResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

			assert.Equal(t, tt.wantAllowed, resp.Allowed,
				"allowed mismatch for resource.type=%q action=%q", tt.resourceType, tt.action)

			if resp.Provenance != nil {
				assert.Equal(t, tt.wantPermID, resp.Provenance.Permission,
					"permission ID mismatch for resource.type=%q action=%q", tt.resourceType, tt.action)
			}
		})
	}
}

// TestExplainAPI_ExplicitPermissionField verifies that the explain API
// accepts a "permission" field in the request body and passes it through
// to the authorization decision, bypassing resource.type + action derivation.
func TestExplainAPI_ExplicitPermissionField(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	memberID := tid("explain-explicit-perm")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID:          memberID,
		Email:       "explicit-perm@test.com",
		DisplayName: "Explicit Perm",
		Role:        "member",
		Status:      "active",
	}))
	ensureHubMembership(ctx, s, memberID)
	identity := NewAuthenticatedUser(memberID, "explicit-perm@test.com", "Explicit Perm", "member", "api")

	t.Run("explicit permission bypasses derivation", func(t *testing.T) {
		body := map[string]interface{}{
			"resource": map[string]interface{}{
				"type": "hub", // Non-canonical resource type...
				"id":   "test",
			},
			"action":     "user.read", // ...with non-canonical action...
			"permission": "user.read", // ...but explicit canonical permission.
		}
		bodyBytes, _ := json.Marshal(body)
		req := newRequestWithIdentity(t, http.MethodPost, "/api/v1/authz/explain", bodyBytes, identity)
		rec := httptest.NewRecorder()
		srv.handleAuthzExplain(rec, req)

		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

		var resp explainResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

		assert.True(t, resp.Allowed,
			"explicit permission=user.read should be allowed for hub-member")
		if resp.Provenance != nil {
			assert.Equal(t, "user.read", resp.Provenance.Permission,
				"provenance should reflect the explicit permission ID")
		}
	})

	t.Run("explicit permission for denied access", func(t *testing.T) {
		body := map[string]interface{}{
			"resource": map[string]interface{}{
				"type": "user",
				"id":   "test",
			},
			"action":     "delete",
			"permission": "user.delete",
		}
		bodyBytes, _ := json.Marshal(body)
		req := newRequestWithIdentity(t, http.MethodPost, "/api/v1/authz/explain", bodyBytes, identity)
		rec := httptest.NewRecorder()
		srv.handleAuthzExplain(rec, req)

		require.Equal(t, http.StatusOK, rec.Code)

		var resp explainResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

		assert.False(t, resp.Allowed,
			"hub-member should not have user.delete")
	})
}

// =============================================================================
// Adversarial tests: unknown / mismatched permission field
// =============================================================================

// TestExplainAPI_UnknownExplicitPermission_Returns400 verifies that the
// explain API returns HTTP 400 when the client provides an explicit
// "permission" field that is not a canonical permission ID in the registry.
func TestExplainAPI_UnknownExplicitPermission_Returns400(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	memberID := tid("explain-unknown-perm")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID:          memberID,
		Email:       "unknown-perm@test.com",
		DisplayName: "Unknown Perm",
		Role:        "member",
		Status:      "active",
	}))
	ensureHubMembership(ctx, s, memberID)
	identity := NewAuthenticatedUser(memberID, "unknown-perm@test.com", "Unknown Perm", "member", "api")

	tests := []struct {
		name       string
		permission string
	}{
		{"completely fabricated", "widget.frobnicate"},
		{"non-canonical hub.user.read", "hub.user.read"},
		{"empty-looking dotted", "a.b.c.d"},
		{"partial match prefix", "user.readx"},
		{"sql injection attempt", "'; DROP TABLE users; --"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := map[string]interface{}{
				"resource": map[string]interface{}{
					"type": "user",
					"id":   "test",
				},
				"action":     "read",
				"permission": tt.permission,
			}
			bodyBytes, _ := json.Marshal(body)
			req := newRequestWithIdentity(t, http.MethodPost, "/api/v1/authz/explain", bodyBytes, identity)
			rec := httptest.NewRecorder()
			srv.handleAuthzExplain(rec, req)

			assert.Equal(t, http.StatusBadRequest, rec.Code,
				"unknown permission %q must be rejected with 400, got %d: %s",
				tt.permission, rec.Code, rec.Body.String())

			// Verify JSON error response.
			var errResp map[string]interface{}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp),
				"error response should be valid JSON")
			assert.Contains(t, errResp, "error",
				"error response should contain 'error' field")

			// Error message should mention the rejected permission.
			if msg, ok := errResp["error"].(map[string]interface{}); ok {
				if message, ok := msg["message"].(string); ok {
					assert.Contains(t, message, tt.permission,
						"error message should mention the unknown permission")
				}
			}
		})
	}
}

// TestExplainAPI_MismatchedResourceActionPermission verifies explain
// behavior when the explicit "permission" field does not match the
// resource.type + action semantically. The server accepts this — the
// explicit permission takes precedence, and the resource/action are
// used only for resource context (scope, ID), not permission derivation.
func TestExplainAPI_MismatchedResourceActionPermission(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	memberID := tid("explain-mismatch-perm")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID:          memberID,
		Email:       "mismatch-perm@test.com",
		DisplayName: "Mismatch Perm",
		Role:        "member",
		Status:      "active",
	}))
	ensureHubMembership(ctx, s, memberID)
	identity := NewAuthenticatedUser(memberID, "mismatch-perm@test.com", "Mismatch Perm", "member", "api")

	t.Run("resource=agent action=create but permission=user.read", func(t *testing.T) {
		// Semantically mismatched: resource says "agent" + "create" but
		// permission is "user.read". The explicit permission wins — the
		// result reflects whether the principal has user.read, not agent.create.
		body := map[string]interface{}{
			"resource": map[string]interface{}{
				"type": "agent",
				"id":   "test-agent",
			},
			"action":     "create",
			"permission": "user.read", // Canonical, but doesn't match resource+action.
		}
		bodyBytes, _ := json.Marshal(body)
		req := newRequestWithIdentity(t, http.MethodPost, "/api/v1/authz/explain", bodyBytes, identity)
		rec := httptest.NewRecorder()
		srv.handleAuthzExplain(rec, req)

		// Should succeed (200) — explicit permission is valid.
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

		var resp explainResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

		// Hub-member has user.read → allowed, even though resource says agent.
		assert.True(t, resp.Allowed,
			"explicit permission=user.read should be evaluated regardless of resource type")
		if resp.Provenance != nil {
			assert.Equal(t, "user.read", resp.Provenance.Permission,
				"provenance should reflect the explicit permission, not resource+action")
		}
	})

	t.Run("permission contradicts resource — denied case", func(t *testing.T) {
		// Hub-member has user.read but NOT user.delete.
		body := map[string]interface{}{
			"resource": map[string]interface{}{
				"type": "user",
				"id":   "test-user",
			},
			"action":     "read",        // Would be allowed via canonicalization...
			"permission": "user.delete", // ...but explicit permission overrides → denied.
		}
		bodyBytes, _ := json.Marshal(body)
		req := newRequestWithIdentity(t, http.MethodPost, "/api/v1/authz/explain", bodyBytes, identity)
		rec := httptest.NewRecorder()
		srv.handleAuthzExplain(rec, req)

		require.Equal(t, http.StatusOK, rec.Code)

		var resp explainResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

		assert.False(t, resp.Allowed,
			"explicit permission=user.delete should override resource+action canonicalization")
	})
}

// TestExplainAPI_NonCanonicalWithoutExplicitPermission_Truthful verifies
// that when the explain canonicalization cannot find a canonical match,
// the non-canonical constructed ID produces a truthful "denied" result
// (rather than silently matching an unrelated permission).
func TestExplainAPI_NonCanonicalWithoutExplicitPermission_Truthful(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	memberID := tid("explain-noncanon-truthful")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID:          memberID,
		Email:       "noncanon@test.com",
		DisplayName: "NonCanon Test",
		Role:        "member",
		Status:      "active",
	}))
	ensureHubMembership(ctx, s, memberID)
	identity := NewAuthenticatedUser(memberID, "noncanon@test.com", "NonCanon Test", "member", "api")

	// Completely unknown resource.type + action that cannot canonicalize.
	body := map[string]interface{}{
		"resource": map[string]interface{}{
			"type": "widget",
			"id":   "test",
		},
		"action": "frobnicate",
	}
	bodyBytes, _ := json.Marshal(body)
	req := newRequestWithIdentity(t, http.MethodPost, "/api/v1/authz/explain", bodyBytes, identity)
	rec := httptest.NewRecorder()
	srv.handleAuthzExplain(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var resp explainResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	// Should be denied — widget.frobnicate is not a real permission.
	assert.False(t, resp.Allowed,
		"completely unknown resource+action should be truthfully denied")
}

// =============================================================================
// Integration test: group-derived bindings
// =============================================================================

// TestExplainAPI_GroupDerivedBinding verifies that explain correctly reports
// allowed=true for permissions granted via group membership (the hub-members
// group → hub-member role path).
func TestExplainAPI_GroupDerivedBinding(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	memberID := tid("explain-group-derived")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID:          memberID,
		Email:       "group-derived@test.com",
		DisplayName: "Group Derived",
		Role:        "member",
		Status:      "active",
	}))
	ensureHubMembership(ctx, s, memberID)
	identity := NewAuthenticatedUser(memberID, "group-derived@test.com", "Group Derived", "member", "api")

	// Check a permission that hub-member gets via group membership.
	body := map[string]interface{}{
		"resource": map[string]interface{}{
			"type": "user",
			"id":   "test-user",
		},
		"action": "read",
	}
	bodyBytes, _ := json.Marshal(body)
	req := newRequestWithIdentity(t, http.MethodPost, "/api/v1/authz/explain", bodyBytes, identity)
	rec := httptest.NewRecorder()
	srv.handleAuthzExplain(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var resp explainResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	assert.True(t, resp.Allowed,
		"hub-member with group-derived binding should be allowed user.read")
	require.NotNil(t, resp.Provenance, "provenance should be populated")
	assert.Equal(t, "user.read", resp.Provenance.Permission)

	// Provenance should include grant details showing the group derivation.
	assert.NotEmpty(t, resp.Provenance.Grants,
		"provenance should include at least one active grant")

	// Verify the grant references hub-member role.
	foundHubMember := false
	for _, g := range resp.Provenance.Grants {
		if g.RoleName == "hub-member" {
			foundHubMember = true
			break
		}
	}
	assert.True(t, foundHubMember,
		"provenance should include a grant from the hub-member role")
}

// =============================================================================
// Integration test: project-scoped explain
// =============================================================================

// TestExplainAPI_ProjectScopedPermission verifies that explain correctly
// evaluates project-scoped permissions with canonical resource type + action.
func TestExplainAPI_ProjectScopedPermission(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	memberID := tid("explain-project-scope")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID:          memberID,
		Email:       "project-scope@test.com",
		DisplayName: "Project Scope",
		Role:        "member",
		Status:      "active",
	}))
	ensureHubMembership(ctx, s, memberID)

	project := &store.Project{
		ID:        tid("explain-project-scope-proj"),
		Name:      "Scope Test",
		Slug:      "scope-test",
		CreatedBy: DevUserID,
		OwnerID:   DevUserID,
	}
	require.NoError(t, s.CreateProject(ctx, project))

	// Give the member a project-member role binding.
	rd, err := s.GetRoleDefinitionByName(ctx, "project-member", string(store.RoleScopeProject))
	require.NoError(t, err, "project-member role should exist")
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      memberID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          project.ID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	identity := NewAuthenticatedUser(memberID, "project-scope@test.com", "Project Scope", "member", "api")

	t.Run("project-scoped agent.read allowed", func(t *testing.T) {
		body := map[string]interface{}{
			"resource": map[string]interface{}{
				"type":      "agent",
				"id":        tid("test-agent"),
				"projectId": project.ID,
			},
			"action": "read",
		}
		bodyBytes, _ := json.Marshal(body)
		req := newRequestWithIdentity(t, http.MethodPost, "/api/v1/authz/explain", bodyBytes, identity)
		rec := httptest.NewRecorder()
		srv.handleAuthzExplain(rec, req)

		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

		var resp explainResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.True(t, resp.Allowed, "project-member should be allowed agent.read in their project")
	})

	t.Run("project-scoped agent.read denied for wrong project", func(t *testing.T) {
		body := map[string]interface{}{
			"resource": map[string]interface{}{
				"type":      "agent",
				"id":        tid("test-agent"),
				"projectId": tid("other-project"),
			},
			"action": "read",
		}
		bodyBytes, _ := json.Marshal(body)
		req := newRequestWithIdentity(t, http.MethodPost, "/api/v1/authz/explain", bodyBytes, identity)
		rec := httptest.NewRecorder()
		srv.handleAuthzExplain(rec, req)

		require.Equal(t, http.StatusOK, rec.Code)

		var resp explainResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.False(t, resp.Allowed, "project-member should be denied agent.read in a different project")
	})
}

// =============================================================================
// Integration test: redaction with canonicalized permissions
// =============================================================================

// TestExplainAPI_RedactionWithCanonicalization verifies that cross-principal
// explain with non-canonical inputs still redacts properly.
func TestExplainAPI_RedactionWithCanonicalization(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	targetID := tid("explain-redact-canon")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID:          targetID,
		Email:       "redact-canon@test.com",
		DisplayName: "Secret Name",
		Role:        "member",
		Status:      "active",
	}))
	ensureHubMembership(ctx, s, targetID)

	// Admin (dev user) explains for a hub-member using non-canonical input.
	body := map[string]interface{}{
		"resource": map[string]interface{}{
			"type": "hub",
			"id":   "test",
		},
		"action":        "user.read", // Non-canonical.
		"principalId":   targetID,
		"principalKind": "user",
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/authz/explain", body)
	require.Equal(t, http.StatusOK, rec.Code, "status: %s", rec.Body.String())

	var resp explainResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	// Should still be allowed (canonicalization fixes the permission).
	assert.True(t, resp.Allowed,
		"hub-member should be allowed user.read even with non-canonical input")

	require.NotNil(t, resp.Provenance, "cross-principal explain must include provenance")

	// Redaction should still work: principal IDs should be "[redacted]".
	for _, g := range resp.Provenance.Grants {
		assert.Equal(t, "[redacted]", g.PrincipalID,
			"cross-principal grant principal ID must be redacted even with canonicalization")
	}

	// Display name should not leak.
	assert.NotContains(t, rec.Body.String(), "Secret Name",
		"cross-principal explain must not leak display names")
}

// =============================================================================
// Regression test: the original hub.user.read mismatch (bug report scenario)
// =============================================================================

// TestExplainAPI_HubUserReadRegression is the canonical regression test for the
// authz explain permission-name mismatch bug. A hub-member user with canonical
// user.read permission should get allowed=true from the explain API regardless
// of whether the caller uses:
//   - resource.type="user", action="read"      (canonical)
//   - resource.type="hub",  action="user.read"  (non-canonical)
//   - resource.type="hub.user", action="read"   (non-canonical)
//
// Before the fix, the latter two produced "hub.user.read" via derivePermissionID
// fallback concatenation, which didn't match any granted permission.
func TestExplainAPI_HubUserReadRegression(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	memberID := tid("explain-regression-member")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID:          memberID,
		Email:       "regression@test.com",
		DisplayName: "Regression Test",
		Role:        "member",
		Status:      "active",
	}))
	ensureHubMembership(ctx, s, memberID)
	identity := NewAuthenticatedUser(memberID, "regression@test.com", "Regression Test", "member", "api")

	variants := []struct {
		name         string
		resourceType string
		action       string
	}{
		{"canonical", "user", "read"},
		{"non-canonical hub+user.read", "hub", "user.read"},
		{"non-canonical hub.user+read", "hub.user", "read"},
	}

	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			body := map[string]interface{}{
				"resource": map[string]interface{}{
					"type": v.resourceType,
					"id":   "any-user",
				},
				"action": v.action,
			}
			bodyBytes, _ := json.Marshal(body)
			req := newRequestWithIdentity(t, http.MethodPost, "/api/v1/authz/explain", bodyBytes, identity)
			rec := httptest.NewRecorder()
			srv.handleAuthzExplain(rec, req)

			require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

			var resp explainResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

			// All variants must resolve to user.read and be allowed.
			assert.True(t, resp.Allowed,
				"hub-member must be allowed user.read via %s (resource.type=%q action=%q)",
				v.name, v.resourceType, v.action)

			require.NotNil(t, resp.Provenance,
				"provenance must be populated for %s", v.name)
			assert.Equal(t, "user.read", resp.Provenance.Permission,
				"permission must be canonical user.read for %s (resource.type=%q action=%q)",
				v.name, v.resourceType, v.action)
		})
	}
}

// =============================================================================
// Integration test: effective_permissions mode with canonicalization
// =============================================================================

// TestExplainAPI_EffectivePermissionsCanonical verifies that the
// effective_permissions mode returns canonical permission IDs and that
// the hub-member user.read permission appears in the effective set.
func TestExplainAPI_EffectivePermissionsCanonical(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	memberID := tid("explain-effperm-canon")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID:          memberID,
		Email:       "effperm-canon@test.com",
		DisplayName: "EffPerm Canon",
		Role:        "member",
		Status:      "active",
	}))
	ensureHubMembership(ctx, s, memberID)
	identity := NewAuthenticatedUser(memberID, "effperm-canon@test.com", "EffPerm Canon", "member", "api")

	body := map[string]interface{}{
		"resource": map[string]interface{}{
			"type": "user",
			"id":   "any-user",
		},
		"action": "read",
		"mode":   "effective_permissions",
	}
	bodyBytes, _ := json.Marshal(body)
	req := newRequestWithIdentity(t, http.MethodPost, "/api/v1/authz/explain", bodyBytes, identity)
	rec := httptest.NewRecorder()
	srv.handleAuthzExplain(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp explainResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	// Should have effective permissions.
	require.NotEmpty(t, resp.EffectivePermissions,
		"hub-member should have effective permissions")

	// user.read should be in the effective set.
	foundUserRead := false
	for _, pp := range resp.EffectivePermissions {
		if pp.PermissionID == "user.read" {
			foundUserRead = true
			assert.True(t, pp.Granted,
				"user.read should be granted in effective permissions")
			break
		}
	}
	assert.True(t, foundUserRead,
		"user.read must appear in hub-member's effective permissions")

	// No permission should have the non-canonical "hub.user.read" ID.
	for _, pp := range resp.EffectivePermissions {
		assert.NotEqual(t, "hub.user.read", pp.PermissionID,
			"effective permissions must not contain non-canonical hub.user.read")
	}
}
