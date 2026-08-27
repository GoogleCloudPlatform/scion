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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestIntegrationsHooksPermissionConversion verifies the PR-A5 permission-based
// conversion for integrations and lifecycle hooks routes:
//  1. Super-admin is allowed for all converted endpoints.
//  2. Hub-admin with correct permission is allowed.
//  3. Hub-admin with read-only permission can GET but not PUT/DELETE (dual-method routes).
//  4. Regular member is denied for all.
func TestIntegrationsHooksPermissionConversion(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	seedRoleDefinitions(ctx, s)

	adminUser := &store.User{
		ID: tid("admin-ih"), Email: "admin-ih@test.com", DisplayName: "Admin",
		Role: "admin", Status: "active",
	}
	memberUser := &store.User{
		ID: tid("member-ih"), Email: "member-ih@test.com", DisplayName: "Member",
		Role: "member", Status: "active",
	}
	hubAdminUser := &store.User{
		ID: tid("hub-admin-ih"), Email: "hub-admin-ih@test.com", DisplayName: "Hub Admin",
		Role: "member", Status: "active",
	}
	for _, u := range []*store.User{adminUser, memberUser, hubAdminUser} {
		if err := s.CreateUser(ctx, u); err != nil {
			t.Fatalf("create user %s: %v", u.Email, err)
		}
	}

	// Grant hub-admin role binding.
	hubAdminRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	if err != nil {
		t.Fatalf("get hub-admin role definition: %v", err)
	}
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: hubAdminRD.ID,
		PrincipalType:    "user",
		PrincipalID:      hubAdminUser.ID,
		ScopeType:        store.RoleScopeSystem,
	})
	if err != nil {
		t.Fatalf("create hub-admin role binding: %v", err)
	}

	okHandler := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}

	adminIdent := NewAuthenticatedUser(tid("admin-ih"), "admin-ih@test.com", "Admin", "admin", "api")
	memberIdent := NewAuthenticatedUser(tid("member-ih"), "member-ih@test.com", "Member", "member", "api")
	hubAdminIdent := NewAuthenticatedUser(tid("hub-admin-ih"), "hub-admin-ih@test.com", "Hub Admin", "member", "api")

	// -----------------------------------------------------------------------
	// Route guard tests: these test the routeGuard metadata entries directly.
	// -----------------------------------------------------------------------

	routeGuardTests := []struct {
		name       string
		pattern    string
		method     string
		identity   Identity
		wantStatus int
	}{
		// /api/v1/admin/integrations — hub.integrations.read
		{"integrations_GET_super_admin", "/api/v1/admin/integrations", http.MethodGet, adminIdent, http.StatusOK},
		{"integrations_GET_hub_admin", "/api/v1/admin/integrations", http.MethodGet, hubAdminIdent, http.StatusOK},
		{"integrations_GET_member_denied", "/api/v1/admin/integrations", http.MethodGet, memberIdent, http.StatusForbidden},

		// /api/v1/admin/integrations/teams/manifest — hub.teams_manifest.read
		{"teams_manifest_GET_super_admin", "/api/v1/admin/integrations/teams/manifest", http.MethodGet, adminIdent, http.StatusOK},
		{"teams_manifest_GET_hub_admin", "/api/v1/admin/integrations/teams/manifest", http.MethodGet, hubAdminIdent, http.StatusOK},
		{"teams_manifest_GET_member_denied", "/api/v1/admin/integrations/teams/manifest", http.MethodGet, memberIdent, http.StatusForbidden},

		// /api/v1/admin/integrations/ — hub.integrations.read (route guard)
		{"integration_byname_GET_super_admin", "/api/v1/admin/integrations/", http.MethodGet, adminIdent, http.StatusOK},
		{"integration_byname_GET_hub_admin", "/api/v1/admin/integrations/", http.MethodGet, hubAdminIdent, http.StatusOK},
		{"integration_byname_GET_member_denied", "/api/v1/admin/integrations/", http.MethodGet, memberIdent, http.StatusForbidden},

		// /api/v1/admin/lifecycle-hooks — hub.lifecycle_hooks.read
		{"lifecycle_hooks_GET_super_admin", "/api/v1/admin/lifecycle-hooks", http.MethodGet, adminIdent, http.StatusOK},
		{"lifecycle_hooks_GET_hub_admin", "/api/v1/admin/lifecycle-hooks", http.MethodGet, hubAdminIdent, http.StatusOK},
		{"lifecycle_hooks_GET_member_denied", "/api/v1/admin/lifecycle-hooks", http.MethodGet, memberIdent, http.StatusForbidden},

		// /api/v1/admin/lifecycle-hooks/ — hub.lifecycle_hooks.read
		{"lifecycle_hook_byid_GET_super_admin", "/api/v1/admin/lifecycle-hooks/", http.MethodGet, adminIdent, http.StatusOK},
		{"lifecycle_hook_byid_GET_hub_admin", "/api/v1/admin/lifecycle-hooks/", http.MethodGet, hubAdminIdent, http.StatusOK},
		{"lifecycle_hook_byid_GET_member_denied", "/api/v1/admin/lifecycle-hooks/", http.MethodGet, memberIdent, http.StatusForbidden},

		// Unauthenticated
		{"integrations_GET_unauth", "/api/v1/admin/integrations", http.MethodGet, nil, http.StatusUnauthorized},
		{"lifecycle_hooks_GET_unauth", "/api/v1/admin/lifecycle-hooks", http.MethodGet, nil, http.StatusUnauthorized},
	}

	for _, tc := range routeGuardTests {
		t.Run("routeGuard/"+tc.name, func(t *testing.T) {
			meta, ok := routeMetadataTable[tc.pattern]
			if !ok {
				t.Fatalf("no route metadata for pattern %q", tc.pattern)
			}
			handler := srv.routeGuard(meta, okHandler)

			req := httptest.NewRequest(tc.method, tc.pattern, nil)
			reqCtx := ctx
			if tc.identity != nil {
				reqCtx = contextWithIdentity(ctx, tc.identity)
			}
			req = req.WithContext(reqCtx)
			rr := httptest.NewRecorder()
			handler(rr, req)

			if rr.Code != tc.wantStatus {
				t.Errorf("got %d, want %d; body: %s", rr.Code, tc.wantStatus, rr.Body.String())
			}
		})
	}

	// -----------------------------------------------------------------------
	// Inline Decide tests: for dual-method routes, test that the handler-level
	// inline Decide blocks write operations for read-only users.
	// -----------------------------------------------------------------------

	// Create a custom role with only read permissions (no update).
	readOnlyPerms := []string{
		"hub.integrations.read",
		"hub.lifecycle_hooks.read",
		"hub.teams_manifest.read",
	}
	readOnlyRD, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        "integ-read-only",
		Description: "Read-only access to integrations/hooks",
		ScopeType:   store.RoleScopeSystem,
		Permissions: readOnlyPerms,
	})
	if err != nil {
		t.Fatalf("create read-only role def: %v", err)
	}

	readOnlyUser := &store.User{
		ID: tid("readonly-ih"), Email: "readonly-ih@test.com", DisplayName: "ReadOnly",
		Role: "member", Status: "active",
	}
	if err := s.CreateUser(ctx, readOnlyUser); err != nil {
		t.Fatalf("create read-only user: %v", err)
	}
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: readOnlyRD.ID,
		PrincipalType:    "user",
		PrincipalID:      readOnlyUser.ID,
		ScopeType:        store.RoleScopeSystem,
	})
	if err != nil {
		t.Fatalf("create read-only role binding: %v", err)
	}

	readOnlyIdent := NewAuthenticatedUser(tid("readonly-ih"), "readonly-ih@test.com", "ReadOnly", "member", "api")

	// Test inline Decide in handleAdminIntegrationByName: read-only user can
	// GET through the route guard but is blocked for PUT/POST/DELETE by inline Decide.
	t.Run("inline/integration_byname_PUT_readonly_denied", func(t *testing.T) {
		meta := routeMetadataTable["/api/v1/admin/integrations/"]
		// Wrap the actual handler — it uses inline Decide for writes.
		handler := srv.routeGuard(meta, srv.handleAdminIntegrationByName)

		req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/integrations/test-plugin/config", nil)
		req = req.WithContext(contextWithIdentity(ctx, readOnlyIdent))
		rr := httptest.NewRecorder()
		handler(rr, req)

		if rr.Code != http.StatusForbidden {
			t.Errorf("read-only user PUT integration: got %d, want 403; body: %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("inline/integration_byname_POST_readonly_denied", func(t *testing.T) {
		meta := routeMetadataTable["/api/v1/admin/integrations/"]
		handler := srv.routeGuard(meta, srv.handleAdminIntegrationByName)

		req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/integrations/test-plugin/update", nil)
		req = req.WithContext(contextWithIdentity(ctx, readOnlyIdent))
		rr := httptest.NewRecorder()
		handler(rr, req)

		if rr.Code != http.StatusForbidden {
			t.Errorf("read-only user POST integration: got %d, want 403; body: %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("inline/integration_byname_DELETE_readonly_denied", func(t *testing.T) {
		meta := routeMetadataTable["/api/v1/admin/integrations/"]
		handler := srv.routeGuard(meta, srv.handleAdminIntegrationByName)

		req := httptest.NewRequest(http.MethodDelete, "/api/v1/admin/integrations/test-plugin", nil)
		req = req.WithContext(contextWithIdentity(ctx, readOnlyIdent))
		rr := httptest.NewRecorder()
		handler(rr, req)

		if rr.Code != http.StatusForbidden {
			t.Errorf("read-only user DELETE integration: got %d, want 403; body: %s", rr.Code, rr.Body.String())
		}
	})

	// Verify super-admin can PUT through the inline Decide.
	t.Run("inline/integration_byname_PUT_super_admin_allowed", func(t *testing.T) {
		meta := routeMetadataTable["/api/v1/admin/integrations/"]
		handler := srv.routeGuard(meta, srv.handleAdminIntegrationByName)

		// PUT to a non-existent plugin — we expect it to pass the authz check
		// and reach the handler logic (likely returning 404 for the missing plugin).
		req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/integrations/nonexistent/config", nil)
		req = req.WithContext(contextWithIdentity(ctx, adminIdent))
		rr := httptest.NewRecorder()
		handler(rr, req)

		// The handler should NOT return 403 — it passed the permission check.
		if rr.Code == http.StatusForbidden {
			t.Errorf("super-admin PUT integration: got 403 unexpectedly; body: %s", rr.Body.String())
		}
	})

	// Test inline Decide in handleAdminLifecycleHooks: read-only user blocked for POST.
	t.Run("inline/lifecycle_hooks_POST_readonly_denied", func(t *testing.T) {
		meta := routeMetadataTable["/api/v1/admin/lifecycle-hooks"]
		handler := srv.routeGuard(meta, srv.handleAdminLifecycleHooks)

		req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/lifecycle-hooks", nil)
		req = req.WithContext(contextWithIdentity(ctx, readOnlyIdent))
		rr := httptest.NewRecorder()
		handler(rr, req)

		if rr.Code != http.StatusForbidden {
			t.Errorf("read-only user POST lifecycle hook: got %d, want 403; body: %s", rr.Code, rr.Body.String())
		}
	})

	// Test inline Decide in handleAdminLifecycleHookByID: read-only user blocked for PUT/DELETE.
	t.Run("inline/lifecycle_hook_byid_PUT_readonly_denied", func(t *testing.T) {
		meta := routeMetadataTable["/api/v1/admin/lifecycle-hooks/"]
		handler := srv.routeGuard(meta, srv.handleAdminLifecycleHookByID)

		req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/lifecycle-hooks/some-id", nil)
		req = req.WithContext(contextWithIdentity(ctx, readOnlyIdent))
		rr := httptest.NewRecorder()
		handler(rr, req)

		if rr.Code != http.StatusForbidden {
			t.Errorf("read-only user PUT lifecycle hook: got %d, want 403; body: %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("inline/lifecycle_hook_byid_DELETE_readonly_denied", func(t *testing.T) {
		meta := routeMetadataTable["/api/v1/admin/lifecycle-hooks/"]
		handler := srv.routeGuard(meta, srv.handleAdminLifecycleHookByID)

		req := httptest.NewRequest(http.MethodDelete, "/api/v1/admin/lifecycle-hooks/some-id", nil)
		req = req.WithContext(contextWithIdentity(ctx, readOnlyIdent))
		rr := httptest.NewRecorder()
		handler(rr, req)

		if rr.Code != http.StatusForbidden {
			t.Errorf("read-only user DELETE lifecycle hook: got %d, want 403; body: %s", rr.Code, rr.Body.String())
		}
	})

	// Verify member (no permissions at all) is denied for write operations.
	t.Run("inline/integration_byname_PUT_member_denied", func(t *testing.T) {
		meta := routeMetadataTable["/api/v1/admin/integrations/"]
		handler := srv.routeGuard(meta, srv.handleAdminIntegrationByName)

		req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/integrations/test-plugin/config", nil)
		req = req.WithContext(contextWithIdentity(ctx, memberIdent))
		rr := httptest.NewRecorder()
		handler(rr, req)

		if rr.Code != http.StatusForbidden {
			t.Errorf("member PUT integration: got %d, want 403; body: %s", rr.Code, rr.Body.String())
		}
	})

	// Verify hub-admin (has update permissions) can PUT through inline Decide.
	t.Run("inline/integration_byname_PUT_hub_admin_allowed", func(t *testing.T) {
		meta := routeMetadataTable["/api/v1/admin/integrations/"]
		handler := srv.routeGuard(meta, srv.handleAdminIntegrationByName)

		req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/integrations/nonexistent/config", nil)
		req = req.WithContext(contextWithIdentity(ctx, hubAdminIdent))
		rr := httptest.NewRecorder()
		handler(rr, req)

		// Should pass authz (not 403). The handler will likely return 404
		// or 405 for the nonexistent plugin — that's fine.
		if rr.Code == http.StatusForbidden {
			t.Errorf("hub-admin PUT integration: got 403 unexpectedly; body: %s", rr.Body.String())
		}
	})

	t.Run("inline/lifecycle_hooks_POST_hub_admin_allowed", func(t *testing.T) {
		meta := routeMetadataTable["/api/v1/admin/lifecycle-hooks"]
		handler := srv.routeGuard(meta, srv.handleAdminLifecycleHooks)

		req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/lifecycle-hooks", nil)
		req = req.WithContext(contextWithIdentity(ctx, hubAdminIdent))
		rr := httptest.NewRecorder()
		handler(rr, req)

		// Should pass authz. Will likely return 400 for invalid body — not 403.
		if rr.Code == http.StatusForbidden {
			t.Errorf("hub-admin POST lifecycle hook: got 403 unexpectedly; body: %s", rr.Body.String())
		}
	})
}
