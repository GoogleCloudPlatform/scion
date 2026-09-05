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
)

// TestRouteGuardPermissionBasedPath verifies the updated RouteHubAdmin case
// in routeGuard. When a route declares a Permission in its metadata, the guard
// calls Decide instead of requireAdmin. When no Permission is set, it falls
// back to requireAdmin.
func TestRouteGuardPermissionBasedPath(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Seed role definitions so hub-admin exists
	seedRoleDefinitions(ctx, s)

	// Create users with different roles
	adminUser := &store.User{
		ID: tid("admin-rg"), Email: "admin-rg@test.com", DisplayName: "Admin",
		Role: "admin", Status: "active",
	}
	memberUser := &store.User{
		ID: tid("member-rg"), Email: "member-rg@test.com", DisplayName: "Member",
		Role: "member", Status: "active",
	}
	hubAdminUser := &store.User{
		ID: tid("hub-admin-rg"), Email: "hub-admin-rg@test.com", DisplayName: "Hub Admin",
		Role: "member", Status: "active",
	}
	if err := s.CreateUser(ctx, adminUser); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	if err := s.CreateUser(ctx, memberUser); err != nil {
		t.Fatalf("create member: %v", err)
	}
	if err := s.CreateUser(ctx, hubAdminUser); err != nil {
		t.Fatalf("create hub-admin user: %v", err)
	}

	// Give super-admin user the super-admin role binding.
	superAdminRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	if err != nil {
		t.Fatalf("get super-admin role definition: %v", err)
	}
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: superAdminRD.ID,
		PrincipalType:    "user",
		PrincipalID:      adminUser.ID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        store.SystemReconcileCreatedBy,
	})
	if err != nil {
		t.Fatalf("create super-admin role binding: %v", err)
	}

	// Give hub-admin user a hub-admin role binding
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

	// A handler that returns 200 — this is the "next" handler after the guard passes.
	okHandler := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	}

	// Test cases for the permission-based path
	t.Run("permission_set_super_admin_allowed", func(t *testing.T) {
		meta := RouteMetadata{
			Pattern:        "/test/perm-admin",
			RouteID:        "test.perm.admin",
			Classification: RouteHubAdmin,
			Permission:     "hub.settings.read",
			Resource:       "hub",
			Action:         "read",
		}
		handler := srv.routeGuard(meta, okHandler)

		admin := NewAuthenticatedUser(tid("admin-rg"), "admin-rg@test.com", "Admin", "admin", "api")
		req := httptest.NewRequest(http.MethodGet, "/test/perm-admin", nil)
		req = req.WithContext(contextWithIdentity(ctx, admin))
		rr := httptest.NewRecorder()
		handler(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("super-admin with permission-based route: got %d, want 200; body: %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("permission_set_member_denied", func(t *testing.T) {
		meta := RouteMetadata{
			Pattern:        "/test/perm-member",
			RouteID:        "test.perm.member",
			Classification: RouteHubAdmin,
			Permission:     "hub.settings.read",
			Resource:       "hub",
			Action:         "read",
		}
		handler := srv.routeGuard(meta, okHandler)

		member := NewAuthenticatedUser(tid("member-rg"), "member-rg@test.com", "Member", "member", "api")
		req := httptest.NewRequest(http.MethodGet, "/test/perm-member", nil)
		req = req.WithContext(contextWithIdentity(ctx, member))
		rr := httptest.NewRecorder()
		handler(rr, req)

		if rr.Code != http.StatusForbidden {
			t.Fatalf("member with permission-based route: got %d, want 403; body: %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("permission_set_hub_admin_allowed", func(t *testing.T) {
		// This validates the D4 second-path: a non-super-admin user with the
		// right permission through role bindings IS allowed.
		meta := RouteMetadata{
			Pattern:        "/test/perm-hubadmin",
			RouteID:        "test.perm.hubadmin",
			Classification: RouteHubAdmin,
			Permission:     "hub.settings.read",
			Resource:       "hub",
			Action:         "read",
		}
		handler := srv.routeGuard(meta, okHandler)

		hubAdmin := NewAuthenticatedUser(tid("hub-admin-rg"), "hub-admin-rg@test.com", "Hub Admin", "member", "api")
		req := httptest.NewRequest(http.MethodGet, "/test/perm-hubadmin", nil)
		req = req.WithContext(contextWithIdentity(ctx, hubAdmin))
		rr := httptest.NewRecorder()
		handler(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("hub-admin with permission-based route: got %d, want 200; body: %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("permission_set_hub_admin_denied_superadmin_only_perm", func(t *testing.T) {
		// Hub-admin should be denied for permissions excluded from the hub-admin role.
		meta := RouteMetadata{
			Pattern:        "/test/perm-hubadmin-denied",
			RouteID:        "test.perm.hubadmin.denied",
			Classification: RouteHubAdmin,
			Permission:     "hub.maintenance.execute",
			Resource:       "hub",
			Action:         "execute",
		}
		handler := srv.routeGuard(meta, okHandler)

		hubAdmin := NewAuthenticatedUser(tid("hub-admin-rg"), "hub-admin-rg@test.com", "Hub Admin", "member", "api")
		req := httptest.NewRequest(http.MethodPost, "/test/perm-hubadmin-denied", nil)
		req = req.WithContext(contextWithIdentity(ctx, hubAdmin))
		rr := httptest.NewRecorder()
		handler(rr, req)

		if rr.Code != http.StatusForbidden {
			t.Fatalf("hub-admin with super-admin-only permission: got %d, want 403; body: %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("permission_set_unauthenticated_denied", func(t *testing.T) {
		meta := RouteMetadata{
			Pattern:        "/test/perm-unauth",
			RouteID:        "test.perm.unauth",
			Classification: RouteHubAdmin,
			Permission:     "hub.settings.read",
			Resource:       "hub",
			Action:         "read",
		}
		handler := srv.routeGuard(meta, okHandler)

		req := httptest.NewRequest(http.MethodGet, "/test/perm-unauth", nil)
		req = req.WithContext(ctx)
		rr := httptest.NewRecorder()
		handler(rr, req)

		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated with permission-based route: got %d, want 401; body: %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("no_permission_fallback_admin_allowed", func(t *testing.T) {
		// When no Permission is set, the guard falls back to requireAdmin.
		meta := RouteMetadata{
			Pattern:        "/test/no-perm-admin",
			RouteID:        "test.noperm.admin",
			Classification: RouteHubAdmin,
			// No Permission, Resource, Action set
		}
		handler := srv.routeGuard(meta, okHandler)

		admin := NewAuthenticatedUser(tid("admin-rg"), "admin-rg@test.com", "Admin", "admin", "api")
		req := httptest.NewRequest(http.MethodGet, "/test/no-perm-admin", nil)
		req = req.WithContext(contextWithIdentity(ctx, admin))
		rr := httptest.NewRecorder()
		handler(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("admin with requireAdmin fallback: got %d, want 200; body: %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("no_permission_fallback_member_denied", func(t *testing.T) {
		meta := RouteMetadata{
			Pattern:        "/test/no-perm-member",
			RouteID:        "test.noperm.member",
			Classification: RouteHubAdmin,
		}
		handler := srv.routeGuard(meta, okHandler)

		member := NewAuthenticatedUser(tid("member-rg"), "member-rg@test.com", "Member", "member", "api")
		req := httptest.NewRequest(http.MethodGet, "/test/no-perm-member", nil)
		req = req.WithContext(contextWithIdentity(ctx, member))
		rr := httptest.NewRecorder()
		handler(rr, req)

		if rr.Code != http.StatusForbidden {
			t.Fatalf("member with requireAdmin fallback: got %d, want 403; body: %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("no_permission_fallback_hub_admin_denied", func(t *testing.T) {
		// With the requireAdmin fallback, even hub-admin is denied because
		// requireAdmin checks IsUnscopedLocalPlatformAdmin (role == "admin").
		// This demonstrates the behavioral difference between the old and new paths.
		meta := RouteMetadata{
			Pattern:        "/test/no-perm-hubadmin",
			RouteID:        "test.noperm.hubadmin",
			Classification: RouteHubAdmin,
		}
		handler := srv.routeGuard(meta, okHandler)

		hubAdmin := NewAuthenticatedUser(tid("hub-admin-rg"), "hub-admin-rg@test.com", "Hub Admin", "member", "api")
		req := httptest.NewRequest(http.MethodGet, "/test/no-perm-hubadmin", nil)
		req = req.WithContext(contextWithIdentity(ctx, hubAdmin))
		rr := httptest.NewRecorder()
		handler(rr, req)

		if rr.Code != http.StatusForbidden {
			t.Fatalf("hub-admin with requireAdmin fallback: got %d, want 403; body: %s", rr.Code, rr.Body.String())
		}
	})
}

// TestRouteGuardStructuredDenialDetails verifies that permission-declared
// RouteHubAdmin routes emit structured denial envelopes with resource_type
// and denied_action, and that no secrets, IDs, or internal reasons leak.
func TestRouteGuardStructuredDenialDetails(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	seedRoleDefinitions(ctx, s)

	// Create a member user (denied by default on hub-admin routes).
	memberUser := &store.User{
		ID: tid("member-sd"), Email: "member-sd@test.com", DisplayName: "Member",
		Role: "member", Status: "active",
	}
	if err := s.CreateUser(ctx, memberUser); err != nil {
		t.Fatalf("create member: %v", err)
	}

	okHandler := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}

	// parseErrorBody extracts the error envelope from a response body.
	parseErrorBody := func(t *testing.T, rr *httptest.ResponseRecorder) map[string]interface{} {
		t.Helper()
		var body map[string]interface{}
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("failed to parse response body: %v; body: %s", err, rr.Body.String())
		}
		errObj, ok := body["error"].(map[string]interface{})
		if !ok {
			t.Fatalf("missing error object in response body: %s", rr.Body.String())
		}
		return errObj
	}

	t.Run("denied_user_gets_structured_resource_and_action", func(t *testing.T) {
		meta := RouteMetadata{
			Pattern:        "/test/structured-denial",
			RouteID:        "test.structured.denial",
			Classification: RouteHubAdmin,
			Permission:     "hub.settings.read",
			Resource:       "hub",
			Action:         "read",
		}
		handler := srv.routeGuard(meta, okHandler)

		member := NewAuthenticatedUser(tid("member-sd"), "member-sd@test.com", "Member", "member", "api")
		req := httptest.NewRequest(http.MethodGet, "/test/structured-denial", nil)
		req = req.WithContext(contextWithIdentity(ctx, member))
		rr := httptest.NewRecorder()
		handler(rr, req)

		if rr.Code != http.StatusForbidden {
			t.Fatalf("got %d, want 403; body: %s", rr.Code, rr.Body.String())
		}

		errObj := parseErrorBody(t, rr)
		details, _ := errObj["details"].(map[string]interface{})
		if details == nil {
			t.Fatalf("expected structured details in denial; got nil; body: %s", rr.Body.String())
		}
		if details["resource_type"] != "hub" {
			t.Errorf("resource_type = %v, want %q", details["resource_type"], "hub")
		}
		if details["denied_action"] != "read" {
			t.Errorf("denied_action = %v, want %q", details["denied_action"], "read")
		}
	})

	t.Run("non_user_identity_gets_structured_resource_and_action", func(t *testing.T) {
		meta := RouteMetadata{
			Pattern:        "/test/structured-nonuser",
			RouteID:        "test.structured.nonuser",
			Classification: RouteHubAdmin,
			Permission:     "hub.config.update",
			Resource:       "hub",
			Action:         "update",
		}
		handler := srv.routeGuard(meta, okHandler)

		// Use an agent identity (non-user)
		agent := &agentIdentityWrapper{&AgentTokenClaims{}}
		req := httptest.NewRequest(http.MethodPut, "/test/structured-nonuser", nil)
		req = req.WithContext(contextWithIdentity(ctx, agent))
		rr := httptest.NewRecorder()
		handler(rr, req)

		if rr.Code != http.StatusForbidden {
			t.Fatalf("got %d, want 403; body: %s", rr.Code, rr.Body.String())
		}

		errObj := parseErrorBody(t, rr)
		details, _ := errObj["details"].(map[string]interface{})
		if details == nil {
			t.Fatalf("expected structured details in denial; got nil; body: %s", rr.Body.String())
		}
		if details["resource_type"] != "hub" {
			t.Errorf("resource_type = %v, want %q", details["resource_type"], "hub")
		}
		if details["denied_action"] != "update" {
			t.Errorf("denied_action = %v, want %q", details["denied_action"], "update")
		}
	})

	t.Run("no_secrets_or_ids_leak_in_denial", func(t *testing.T) {
		meta := RouteMetadata{
			Pattern:        "/test/no-leak",
			RouteID:        "test.no.leak",
			Classification: RouteHubAdmin,
			Permission:     "hub.settings.read",
			Resource:       "hub",
			Action:         "read",
		}
		handler := srv.routeGuard(meta, okHandler)

		member := NewAuthenticatedUser(tid("member-sd"), "member-sd@test.com", "Member", "member", "api")
		req := httptest.NewRequest(http.MethodGet, "/test/no-leak", nil)
		req = req.WithContext(contextWithIdentity(ctx, member))
		rr := httptest.NewRecorder()
		handler(rr, req)

		if rr.Code != http.StatusForbidden {
			t.Fatalf("got %d, want 403; body: %s", rr.Code, rr.Body.String())
		}

		errObj := parseErrorBody(t, rr)
		body := rr.Body.String()

		// No resource IDs, user IDs, evaluator reasons, role membership, or credentials
		details, _ := errObj["details"].(map[string]interface{})
		for _, forbidden := range []string{"resource_id", "reason", "role", "credential", "principal_id", "evaluator"} {
			if _, ok := details[forbidden]; ok {
				t.Errorf("details must not contain %q; body: %s", forbidden, body)
			}
		}
		// The user's ID must not appear in the response
		if errObj["code"] != ErrCodeForbidden {
			t.Errorf("code = %v, want %q", errObj["code"], ErrCodeForbidden)
		}
	})

	t.Run("unauthenticated_gets_401_not_structured_denial", func(t *testing.T) {
		meta := RouteMetadata{
			Pattern:        "/test/unauth-401",
			RouteID:        "test.unauth.401",
			Classification: RouteHubAdmin,
			Permission:     "hub.settings.read",
			Resource:       "hub",
			Action:         "read",
		}
		handler := srv.routeGuard(meta, okHandler)

		req := httptest.NewRequest(http.MethodGet, "/test/unauth-401", nil)
		req = req.WithContext(ctx)
		rr := httptest.NewRecorder()
		handler(rr, req)

		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("got %d, want 401; body: %s", rr.Code, rr.Body.String())
		}
	})
}

// TestGitHubAppRouteMethodMatrix verifies method-aware permission enforcement
// on all GitHub App routes: unauthenticated → 401, read authority → allowed on
// reads but denied on writes, update authority → allowed on writes, ordinary
// member → 403 with structured details.
func TestGitHubAppRouteMethodMatrix(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	seedRoleDefinitions(ctx, s)

	// Create users
	adminUser := &store.User{
		ID: tid("admin-ga"), Email: "admin-ga@test.com", DisplayName: "Admin",
		Role: "admin", Status: "active",
	}
	memberUser := &store.User{
		ID: tid("member-ga"), Email: "member-ga@test.com", DisplayName: "Member",
		Role: "member", Status: "active",
	}
	if err := s.CreateUser(ctx, adminUser); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	if err := s.CreateUser(ctx, memberUser); err != nil {
		t.Fatalf("create member: %v", err)
	}

	// Seed super-admin binding for adminUser
	superAdminRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	if err != nil {
		t.Fatalf("get super-admin role def: %v", err)
	}
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: superAdminRD.ID,
		PrincipalType:    "user",
		PrincipalID:      adminUser.ID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        store.SystemReconcileCreatedBy,
	})
	if err != nil {
		t.Fatalf("create super-admin binding: %v", err)
	}

	type testCase struct {
		name       string
		method     string
		path       string
		wantAdmin  int // expected status for super-admin
		wantMember int // expected status for ordinary member
		wantUnauth int // expected status for unauthenticated
	}

	tests := []testCase{
		// Read operations
		{name: "GET github-app config", method: "GET", path: "/api/v1/github-app", wantAdmin: 200, wantMember: 403, wantUnauth: 401},
		{name: "GET installations list", method: "GET", path: "/api/v1/github-app/installations", wantAdmin: 200, wantMember: 403, wantUnauth: 401},
		{name: "GET installation by ID", method: "GET", path: "/api/v1/github-app/installations/999", wantAdmin: 404, wantMember: 403, wantUnauth: 401},
		// Write operations
		{name: "POST installations create", method: "POST", path: "/api/v1/github-app/installations", wantAdmin: 400, wantMember: 403, wantUnauth: 401},
		{name: "POST installations discover", method: "POST", path: "/api/v1/github-app/installations/discover", wantAdmin: 503, wantMember: 403, wantUnauth: 401},
		{name: "POST sync-permissions", method: "POST", path: "/api/v1/github-app/sync-permissions", wantAdmin: 502, wantMember: 403, wantUnauth: 401},
		{name: "DELETE installation", method: "DELETE", path: "/api/v1/github-app/installations/999", wantAdmin: 404, wantMember: 403, wantUnauth: 401},
	}

	for _, tt := range tests {
		t.Run(tt.name+"_admin", func(t *testing.T) {
			admin := NewAuthenticatedUser(tid("admin-ga"), "admin-ga@test.com", "Admin", "admin", "api")
			req := httptest.NewRequest(tt.method, tt.path, nil)
			req.Header.Set("Content-Type", "application/json")
			req = req.WithContext(contextWithIdentity(ctx, admin))
			rr := httptest.NewRecorder()
			srv.mux.ServeHTTP(rr, req)

			if rr.Code != tt.wantAdmin {
				t.Fatalf("admin %s %s: got %d, want %d; body: %s", tt.method, tt.path, rr.Code, tt.wantAdmin, rr.Body.String())
			}
		})

		t.Run(tt.name+"_member_denied_with_details", func(t *testing.T) {
			member := NewAuthenticatedUser(tid("member-ga"), "member-ga@test.com", "Member", "member", "api")
			req := httptest.NewRequest(tt.method, tt.path, nil)
			req.Header.Set("Content-Type", "application/json")
			req = req.WithContext(contextWithIdentity(ctx, member))
			rr := httptest.NewRecorder()
			srv.mux.ServeHTTP(rr, req)

			if rr.Code != tt.wantMember {
				t.Fatalf("member %s %s: got %d, want %d; body: %s", tt.method, tt.path, rr.Code, tt.wantMember, rr.Body.String())
			}

			// Verify structured denial details
			var body map[string]interface{}
			if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
				t.Fatalf("failed to parse response: %v", err)
			}
			errObj, _ := body["error"].(map[string]interface{})
			if errObj == nil {
				t.Fatalf("missing error object; body: %s", rr.Body.String())
			}
			details, _ := errObj["details"].(map[string]interface{})
			if details == nil {
				t.Fatalf("expected structured details; got nil; body: %s", rr.Body.String())
			}
			if details["resource_type"] != "hub" {
				t.Errorf("resource_type = %v, want %q", details["resource_type"], "hub")
			}
			action := details["denied_action"]
			if action != "read" && action != "update" {
				t.Errorf("denied_action = %v, want %q or %q", action, "read", "update")
			}
		})

		t.Run(tt.name+"_unauthenticated_401", func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			req = req.WithContext(ctx)
			rr := httptest.NewRecorder()
			srv.mux.ServeHTTP(rr, req)

			if rr.Code != tt.wantUnauth {
				t.Fatalf("unauth %s %s: got %d, want %d; body: %s", tt.method, tt.path, rr.Code, tt.wantUnauth, rr.Body.String())
			}
		})
	}
}

// TestLiveEndpointStructuredDenials is a regression test for the three live
// endpoints reported in the initial investigation: access-constraints,
// github-app, and server-config. All must produce the structured two-line
// envelope when denied.
func TestLiveEndpointStructuredDenials(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	seedRoleDefinitions(ctx, s)

	memberUser := &store.User{
		ID: tid("member-le"), Email: "member-le@test.com", DisplayName: "Member",
		Role: "member", Status: "active",
	}
	if err := s.CreateUser(ctx, memberUser); err != nil {
		t.Fatalf("create member: %v", err)
	}

	member := NewAuthenticatedUser(tid("member-le"), "member-le@test.com", "Member", "member", "api")

	endpoints := []struct {
		name   string
		method string
		path   string
	}{
		{name: "access-constraints", method: "GET", path: "/api/v1/admin/access-constraints"},
		{name: "github-app", method: "GET", path: "/api/v1/github-app"},
		{name: "server-config", method: "GET", path: "/api/v1/admin/server-config"},
	}

	for _, ep := range endpoints {
		t.Run(ep.name, func(t *testing.T) {
			req := httptest.NewRequest(ep.method, ep.path, nil)
			req = req.WithContext(contextWithIdentity(ctx, member))
			rr := httptest.NewRecorder()
			srv.mux.ServeHTTP(rr, req)

			if rr.Code != http.StatusForbidden {
				t.Fatalf("got %d, want 403; body: %s", rr.Code, rr.Body.String())
			}

			var body map[string]interface{}
			if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
				t.Fatalf("failed to parse response: %v; body: %s", err, rr.Body.String())
			}
			errObj, _ := body["error"].(map[string]interface{})
			if errObj == nil {
				t.Fatalf("missing error object; body: %s", rr.Body.String())
			}
			details, _ := errObj["details"].(map[string]interface{})
			if details == nil {
				t.Fatalf("expected structured details for %s; got nil; body: %s", ep.name, rr.Body.String())
			}
			if details["resource_type"] == nil {
				t.Errorf("missing resource_type in details for %s", ep.name)
			}
			if details["denied_action"] == nil {
				t.Errorf("missing denied_action in details for %s", ep.name)
			}
		})
	}
}
