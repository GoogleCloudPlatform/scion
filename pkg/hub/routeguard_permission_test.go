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
