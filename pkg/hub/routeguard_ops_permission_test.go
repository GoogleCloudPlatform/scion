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

// TestRouteGuardOpsPermissions verifies the PR-A4 operations handler conversion.
// It tests that the route metadata permissions for operations endpoints enforce
// correct access:
//   - Super-admin is allowed for all converted endpoints
//   - Hub-admin with hub.health.read or hub.scheduler.read can access those endpoints
//   - Hub-admin WITHOUT hub.maintenance.execute is DENIED for maintenance endpoints
//   - Regular member is denied for all converted endpoints
func TestRouteGuardOpsPermissions(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Seed role definitions so hub-admin role exists with its permissions
	seedRoleDefinitions(ctx, s)

	// Create test users
	adminUser := &store.User{
		ID: tid("ops-admin"), Email: "ops-admin@test.com", DisplayName: "Super Admin",
		Role: "admin", Status: "active",
	}
	memberUser := &store.User{
		ID: tid("ops-member"), Email: "ops-member@test.com", DisplayName: "Member",
		Role: "member", Status: "active",
	}
	hubAdminUser := &store.User{
		ID: tid("ops-hubadmin"), Email: "ops-hubadmin@test.com", DisplayName: "Hub Admin",
		Role: "member", Status: "active",
	}
	for _, u := range []*store.User{adminUser, memberUser, hubAdminUser} {
		if err := s.CreateUser(ctx, u); err != nil {
			t.Fatalf("create user %s: %v", u.Email, err)
		}
	}

	// Give hub-admin user the hub-admin role binding
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

	// Helper: a handler that returns 200 to confirm the guard passed.
	okHandler := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}

	// Build identity objects
	superAdmin := NewAuthenticatedUser(tid("ops-admin"), "ops-admin@test.com", "Super Admin", "admin", "api")
	member := NewAuthenticatedUser(tid("ops-member"), "ops-member@test.com", "Member", "member", "api")
	hubAdmin := NewAuthenticatedUser(tid("ops-hubadmin"), "ops-hubadmin@test.com", "Hub Admin", "member", "api")

	// Routes converted in PR-A4, grouped by expected hub-admin access.
	// Super-admin-only routes: hub-admin should be DENIED (permission not in hub-admin role).
	superAdminOnlyRoutes := []struct {
		pattern    string
		permission string
		resource   string
		action     string
	}{
		{"/api/v1/admin/maintenance", "hub.admin_mode.update", "hub", "update"},
		{"/api/v1/admin/maintenance/operations", "hub.maintenance.execute", "hub", "execute"},
		{"/api/v1/admin/maintenance/operations/", "hub.maintenance.execute", "hub", "execute"},
		{"/api/v1/admin/maintenance/migrations/", "hub.maintenance.execute", "hub", "execute"},
		{"/api/v1/admin/maintenance/check-updates", "hub.maintenance.execute", "hub", "execute"},
		{"/api/v1/admin/maintenance/restart", "hub.maintenance.execute", "hub", "execute"},
		{"/api/v1/admin/agents/reset-auth-all", "hub.auth_reset.execute", "hub", "execute"},
		{"/api/v1/admin/diagnostics/logs", "hub.diagnostics.read", "hub", "read"},
		{"/api/v1/admin/diagnostics/logs/stream", "hub.diagnostics.read", "hub", "read"},
	}

	// Hub-admin-accessible routes: hub-admin SHOULD be allowed (permission is in hub-admin role).
	hubAdminAccessibleRoutes := []struct {
		pattern    string
		permission string
		resource   string
		action     string
	}{
		{"/api/v1/admin/scheduler", "hub.scheduler.read", "hub", "read"},
		{"/api/v1/admin/health/summary", "hub.health.read", "hub", "read"},
		{"/api/v1/metrics/", "hub.metrics.read", "hub", "read"},
		{"/api/v1/admin/metrics-dashboard", "hub.metrics.read", "hub", "read"},
	}

	// --- Super-admin is allowed for ALL converted endpoints ---
	t.Run("super_admin_allowed_all", func(t *testing.T) {
		allRoutes := append(superAdminOnlyRoutes, hubAdminAccessibleRoutes...)
		for _, route := range allRoutes {
			meta := routeMetadataTable[route.pattern]
			handler := srv.routeGuard(meta, okHandler)

			req := httptest.NewRequest(http.MethodGet, route.pattern, nil)
			req = req.WithContext(contextWithIdentity(ctx, superAdmin))
			rr := httptest.NewRecorder()
			handler(rr, req)

			if rr.Code != http.StatusOK {
				t.Errorf("super-admin denied for %s: got %d, want 200", route.pattern, rr.Code)
			}
		}
	})

	// --- Regular member is denied for ALL converted endpoints ---
	t.Run("member_denied_all", func(t *testing.T) {
		allRoutes := append(superAdminOnlyRoutes, hubAdminAccessibleRoutes...)
		for _, route := range allRoutes {
			meta := routeMetadataTable[route.pattern]
			handler := srv.routeGuard(meta, okHandler)

			req := httptest.NewRequest(http.MethodGet, route.pattern, nil)
			req = req.WithContext(contextWithIdentity(ctx, member))
			rr := httptest.NewRecorder()
			handler(rr, req)

			if rr.Code != http.StatusForbidden {
				t.Errorf("member allowed for %s: got %d, want 403", route.pattern, rr.Code)
			}
		}
	})

	// --- Hub-admin is DENIED for super-admin-only endpoints ---
	t.Run("hub_admin_denied_superadmin_only", func(t *testing.T) {
		for _, route := range superAdminOnlyRoutes {
			meta := routeMetadataTable[route.pattern]
			handler := srv.routeGuard(meta, okHandler)

			req := httptest.NewRequest(http.MethodGet, route.pattern, nil)
			req = req.WithContext(contextWithIdentity(ctx, hubAdmin))
			rr := httptest.NewRecorder()
			handler(rr, req)

			if rr.Code != http.StatusForbidden {
				t.Errorf("hub-admin allowed for super-admin-only %s (perm: %s): got %d, want 403",
					route.pattern, route.permission, rr.Code)
			}
		}
	})

	// --- Hub-admin is ALLOWED for hub-admin-accessible endpoints ---
	t.Run("hub_admin_allowed_accessible", func(t *testing.T) {
		for _, route := range hubAdminAccessibleRoutes {
			meta := routeMetadataTable[route.pattern]
			handler := srv.routeGuard(meta, okHandler)

			req := httptest.NewRequest(http.MethodGet, route.pattern, nil)
			req = req.WithContext(contextWithIdentity(ctx, hubAdmin))
			rr := httptest.NewRecorder()
			handler(rr, req)

			if rr.Code != http.StatusOK {
				t.Errorf("hub-admin denied for %s (perm: %s): got %d, want 200; body: %s",
					route.pattern, route.permission, rr.Code, rr.Body.String())
			}
		}
	})

	// --- Verify route metadata correctness ---
	t.Run("route_metadata_completeness", func(t *testing.T) {
		allRoutes := append(superAdminOnlyRoutes, hubAdminAccessibleRoutes...)
		for _, route := range allRoutes {
			meta, ok := routeMetadataTable[route.pattern]
			if !ok {
				t.Errorf("route %s missing from routeMetadataTable", route.pattern)
				continue
			}
			if meta.Permission != route.permission {
				t.Errorf("route %s: Permission = %q, want %q", route.pattern, meta.Permission, route.permission)
			}
			if meta.Resource != route.resource {
				t.Errorf("route %s: Resource = %q, want %q", route.pattern, meta.Resource, route.resource)
			}
			if meta.Action != route.action {
				t.Errorf("route %s: Action = %q, want %q", route.pattern, meta.Action, route.action)
			}
			if meta.Classification != RouteHubAdmin {
				t.Errorf("route %s: Classification = %q, want %q", route.pattern, meta.Classification, RouteHubAdmin)
			}
		}
	})
}
