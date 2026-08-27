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

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestRouteGuardSettingsConversion verifies that the 4 converted settings/config
// endpoints enforce permissions via the Decide pipeline (PR-A2 conversion).
//
// The server-config and project-defaults routes use READ permission in the
// route guard (so GET is allowed with read-only access), and the handlers
// enforce UPDATE permission inline for mutating methods (PUT/PATCH/POST).
func TestRouteGuardSettingsConversion(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Seed role definitions so hub-admin exists.
	seedRoleDefinitions(ctx, s)

	// Create users.
	adminUser := &store.User{
		ID: tid("admin-set"), Email: "admin-set@test.com", DisplayName: "Admin",
		Role: "admin", Status: "active",
	}
	memberUser := &store.User{
		ID: tid("member-set"), Email: "member-set@test.com", DisplayName: "Member",
		Role: "member", Status: "active",
	}
	hubAdminUser := &store.User{
		ID: tid("hub-admin-set"), Email: "hub-admin-set@test.com", DisplayName: "Hub Admin",
		Role: "member", Status: "active",
	}
	for _, u := range []*store.User{adminUser, memberUser, hubAdminUser} {
		if err := s.CreateUser(ctx, u); err != nil {
			t.Fatalf("create user %s: %v", u.Email, err)
		}
	}

	// Give hub-admin user the hub-admin role binding.
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

	// Handler that returns 200 when the guard passes.
	okHandler := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	}

	// The 4 converted route metadata entries.
	// server-config and project-defaults now use READ permission in the guard;
	// write checks are done inline by the handlers.
	convertedRoutes := []struct {
		name string
		meta RouteMetadata
	}{
		{
			name: "server-config-schema",
			meta: RouteMetadata{
				Pattern: "/api/v1/admin/server-config/schema", RouteID: "admin.serverConfig.schema",
				Classification: RouteHubAdmin,
				Permission:     "hub.config.read", Resource: "hub", Action: "read",
			},
		},
		{
			name: "server-config-sections",
			meta: RouteMetadata{
				Pattern: "/api/v1/admin/server-config/sections/", RouteID: "admin.serverConfig.sections.byId",
				Classification: RouteHubAdmin,
				Permission:     "hub.config.update", Resource: "hub", Action: "update",
			},
		},
		{
			name: "server-config",
			meta: RouteMetadata{
				Pattern: "/api/v1/admin/server-config", RouteID: "admin.serverConfig",
				Classification: RouteHubAdmin,
				Permission:     "hub.config.read", Resource: "hub", Action: "read",
			},
		},
		{
			name: "project-defaults",
			meta: RouteMetadata{
				Pattern: "/api/v1/admin/project-defaults", RouteID: "admin.projectDefaults",
				Classification: RouteHubAdmin,
				Permission:     "hub.project_defaults.read", Resource: "hub", Action: "read",
			},
		},
	}

	for _, route := range convertedRoutes {
		route := route
		handler := srv.routeGuard(route.meta, okHandler)

		t.Run(route.name+"_super_admin_allowed", func(t *testing.T) {
			admin := NewAuthenticatedUser(tid("admin-set"), "admin-set@test.com", "Admin", "admin", "api")
			req := httptest.NewRequest(http.MethodGet, route.meta.Pattern, nil)
			req = req.WithContext(contextWithIdentity(ctx, admin))
			rr := httptest.NewRecorder()
			handler(rr, req)

			if rr.Code != http.StatusOK {
				t.Fatalf("super-admin on %s: got %d, want 200; body: %s", route.name, rr.Code, rr.Body.String())
			}
		})

		t.Run(route.name+"_hub_admin_allowed", func(t *testing.T) {
			hubAdmin := NewAuthenticatedUser(tid("hub-admin-set"), "hub-admin-set@test.com", "Hub Admin", "member", "api")
			req := httptest.NewRequest(http.MethodGet, route.meta.Pattern, nil)
			req = req.WithContext(contextWithIdentity(ctx, hubAdmin))
			rr := httptest.NewRecorder()
			handler(rr, req)

			if rr.Code != http.StatusOK {
				t.Fatalf("hub-admin on %s: got %d, want 200; body: %s", route.name, rr.Code, rr.Body.String())
			}
		})

		t.Run(route.name+"_member_denied", func(t *testing.T) {
			member := NewAuthenticatedUser(tid("member-set"), "member-set@test.com", "Member", "member", "api")
			req := httptest.NewRequest(http.MethodGet, route.meta.Pattern, nil)
			req = req.WithContext(contextWithIdentity(ctx, member))
			rr := httptest.NewRecorder()
			handler(rr, req)

			if rr.Code != http.StatusForbidden {
				t.Fatalf("member on %s: got %d, want 403; body: %s", route.name, rr.Code, rr.Body.String())
			}
		})
	}
}

// TestDualMethodRouteWritePermission verifies the inline write-permission checks
// added to handleAdminServerConfig and handleAdminProjectDefaults. The route guard
// uses READ permission, so GET is allowed for read-only users. PUT/PATCH/POST
// require the UPDATE permission enforced inside the handler.
func TestDualMethodRouteWritePermission(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Seed role definitions so hub-admin and hub-viewer exist.
	seedRoleDefinitions(ctx, s)

	// Create users.
	adminUser := &store.User{
		ID: tid("admin-dw"), Email: "admin-dw@test.com", DisplayName: "Admin",
		Role: "admin", Status: "active",
	}
	memberUser := &store.User{
		ID: tid("member-dw"), Email: "member-dw@test.com", DisplayName: "Member",
		Role: "member", Status: "active",
	}
	hubAdminUser := &store.User{
		ID: tid("hub-admin-dw"), Email: "hub-admin-dw@test.com", DisplayName: "Hub Admin",
		Role: "member", Status: "active",
	}
	// A hub-viewer only has read permissions — no update.
	hubViewerUser := &store.User{
		ID: tid("hub-viewer-dw"), Email: "hub-viewer-dw@test.com", DisplayName: "Hub Viewer",
		Role: "member", Status: "active",
	}
	for _, u := range []*store.User{adminUser, memberUser, hubAdminUser, hubViewerUser} {
		if err := s.CreateUser(ctx, u); err != nil {
			t.Fatalf("create user %s: %v", u.Email, err)
		}
	}

	// Give hub-admin user the hub-admin role binding (includes read + update).
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

	// Give hub-viewer user the hub-viewer role binding (read-only, no update).
	hubViewerRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubViewer, store.RoleScopeSystem)
	if err != nil {
		t.Fatalf("get hub-viewer role definition: %v", err)
	}
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: hubViewerRD.ID,
		PrincipalType:    "user",
		PrincipalID:      hubViewerUser.ID,
		ScopeType:        store.RoleScopeSystem,
	})
	if err != nil {
		t.Fatalf("create hub-viewer role binding: %v", err)
	}

	// Test inline write-permission checks on the two dual-method routes.
	// We wrap the real handler method with the route guard (using the READ metadata)
	// to simulate the full request flow.
	dualRoutes := []struct {
		name    string
		pattern string
		handler http.HandlerFunc
	}{
		{
			name:    "server-config",
			pattern: "/api/v1/admin/server-config",
			handler: srv.handleAdminServerConfig,
		},
		{
			name:    "project-defaults",
			pattern: "/api/v1/admin/project-defaults",
			handler: srv.handleAdminProjectDefaults,
		},
	}

	for _, route := range dualRoutes {
		route := route
		meta := routeMetadataTable[route.pattern]
		guarded := srv.routeGuard(meta, route.handler)

		// 1. Hub-viewer (read-only) can GET (route guard allows read).
		t.Run(route.name+"_hub_viewer_GET_allowed", func(t *testing.T) {
			viewer := NewAuthenticatedUser(tid("hub-viewer-dw"), "hub-viewer-dw@test.com", "Hub Viewer", "member", "api")
			req := httptest.NewRequest(http.MethodGet, route.pattern, nil)
			req = req.WithContext(contextWithIdentity(ctx, viewer))
			rr := httptest.NewRecorder()
			guarded(rr, req)

			if rr.Code == http.StatusForbidden || rr.Code == http.StatusUnauthorized {
				t.Fatalf("hub-viewer GET on %s should be allowed, got %d; body: %s", route.name, rr.Code, rr.Body.String())
			}
		})

		// 2. Hub-viewer (read-only) is DENIED PUT (inline write check blocks).
		t.Run(route.name+"_hub_viewer_PUT_denied", func(t *testing.T) {
			viewer := NewAuthenticatedUser(tid("hub-viewer-dw"), "hub-viewer-dw@test.com", "Hub Viewer", "member", "api")
			req := httptest.NewRequest(http.MethodPut, route.pattern, nil)
			req = req.WithContext(contextWithIdentity(ctx, viewer))
			rr := httptest.NewRecorder()
			guarded(rr, req)

			if rr.Code != http.StatusForbidden {
				t.Fatalf("hub-viewer PUT on %s: got %d, want 403; body: %s", route.name, rr.Code, rr.Body.String())
			}
		})

		// 3. Hub-admin (read + update) can GET.
		t.Run(route.name+"_hub_admin_GET_allowed", func(t *testing.T) {
			hubAdmin := NewAuthenticatedUser(tid("hub-admin-dw"), "hub-admin-dw@test.com", "Hub Admin", "member", "api")
			req := httptest.NewRequest(http.MethodGet, route.pattern, nil)
			req = req.WithContext(contextWithIdentity(ctx, hubAdmin))
			rr := httptest.NewRecorder()
			guarded(rr, req)

			if rr.Code == http.StatusForbidden || rr.Code == http.StatusUnauthorized {
				t.Fatalf("hub-admin GET on %s should be allowed, got %d; body: %s", route.name, rr.Code, rr.Body.String())
			}
		})

		// 4. Hub-admin (read + update) can PUT (has update permission).
		t.Run(route.name+"_hub_admin_PUT_allowed", func(t *testing.T) {
			hubAdmin := NewAuthenticatedUser(tid("hub-admin-dw"), "hub-admin-dw@test.com", "Hub Admin", "member", "api")
			req := httptest.NewRequest(http.MethodPut, route.pattern, nil)
			req = req.WithContext(contextWithIdentity(ctx, hubAdmin))
			rr := httptest.NewRecorder()
			guarded(rr, req)

			// Handler may fail for other reasons (no body, no DB), but must NOT be 403.
			if rr.Code == http.StatusForbidden {
				t.Fatalf("hub-admin PUT on %s should not be 403, got %d; body: %s", route.name, rr.Code, rr.Body.String())
			}
		})

		// 5. Member is denied even for GET (route guard denies read).
		t.Run(route.name+"_member_GET_denied", func(t *testing.T) {
			member := NewAuthenticatedUser(tid("member-dw"), "member-dw@test.com", "Member", "member", "api")
			req := httptest.NewRequest(http.MethodGet, route.pattern, nil)
			req = req.WithContext(contextWithIdentity(ctx, member))
			rr := httptest.NewRecorder()
			guarded(rr, req)

			if rr.Code != http.StatusForbidden {
				t.Fatalf("member GET on %s: got %d, want 403; body: %s", route.name, rr.Code, rr.Body.String())
			}
		})

		// 6. Super-admin GET is allowed (super-admin bypasses all).
		t.Run(route.name+"_super_admin_GET_allowed", func(t *testing.T) {
			admin := NewAuthenticatedUser(tid("admin-dw"), "admin-dw@test.com", "Admin", "admin", "api")
			req := httptest.NewRequest(http.MethodGet, route.pattern, nil)
			req = req.WithContext(contextWithIdentity(ctx, admin))
			rr := httptest.NewRecorder()
			guarded(rr, req)

			if rr.Code == http.StatusForbidden || rr.Code == http.StatusUnauthorized {
				t.Fatalf("super-admin GET on %s should be allowed, got %d; body: %s", route.name, rr.Code, rr.Body.String())
			}
		})

		// 7. Super-admin PUT is allowed (super-admin bypasses all).
		t.Run(route.name+"_super_admin_PUT_allowed", func(t *testing.T) {
			admin := NewAuthenticatedUser(tid("admin-dw"), "admin-dw@test.com", "Admin", "admin", "api")
			req := httptest.NewRequest(http.MethodPut, route.pattern, nil)
			req = req.WithContext(contextWithIdentity(ctx, admin))
			rr := httptest.NewRecorder()
			guarded(rr, req)

			// Handler may fail for other reasons (no body, no DB), but must NOT be 403.
			if rr.Code == http.StatusForbidden {
				t.Fatalf("super-admin PUT on %s should not be 403, got %d; body: %s", route.name, rr.Code, rr.Body.String())
			}
		})
	}
}

// TestM1_UATDeniedForHubLevelResources verifies that enforceUATConstraints
// denies project-scoped UATs from accessing hub-level resources (M1 fix).
func TestM1_UATDeniedForHubLevelResources(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	// Create a user for the UAT.
	uatUser := &store.User{
		ID: tid("uat-m1"), Email: "uat-m1@test.com", DisplayName: "UAT User",
		Role: "admin", Status: "active",
	}
	if err := s.CreateUser(ctx, uatUser); err != nil {
		t.Fatalf("create user: %v", err)
	}

	authz := NewAuthzService(s, nil)

	baseUser := NewAuthenticatedUser(tid("uat-m1"), "uat-m1@test.com", "UAT User", "admin", "api")

	t.Run("hub_resource_denied", func(t *testing.T) {
		// UAT with a hub scope attempting to access a hub-level resource.
		scoped := NewScopedUserIdentity(baseUser, "proj-1", []string{"hub:read"})
		resource := Resource{Type: "hub", ID: "hub-1"}
		result := authz.enforceUATConstraints(scoped, resource, "read")
		if result == nil {
			t.Fatal("expected denial for hub-level resource, got nil")
		}
		if result.Allowed {
			t.Fatal("expected denial for hub-level resource, got allowed")
		}
		if result.Reason != "token not scoped for hub-level resources" {
			t.Fatalf("unexpected reason: %s", result.Reason)
		}
	})

	t.Run("user_resource_denied", func(t *testing.T) {
		// UAT with user:invite scope attempting to access a user resource with no project parent.
		scoped := NewScopedUserIdentity(baseUser, "proj-1", []string{"user:invite"})
		resource := Resource{Type: "user", ID: "user-1"}
		result := authz.enforceUATConstraints(scoped, resource, "invite")
		if result == nil {
			t.Fatal("expected denial for user resource without project parent, got nil")
		}
		if result.Allowed {
			t.Fatal("expected denial for user resource without project parent, got allowed")
		}
		if result.Reason != "token not scoped for hub-level resources" {
			t.Fatalf("unexpected reason: %s", result.Reason)
		}
	})

	t.Run("project_child_resource_allowed", func(t *testing.T) {
		// UAT with agent:create scope for a resource that has a matching project parent.
		scoped := NewScopedUserIdentity(baseUser, "proj-1", []string{"agent:create"})
		resource := Resource{Type: "agent", ID: "agent-1", ParentType: "project", ParentID: "proj-1"}
		result := authz.enforceUATConstraints(scoped, resource, "create")
		if result != nil {
			t.Fatalf("expected nil (allowed) for project-child resource, got denial: %s", result.Reason)
		}
	})

	t.Run("project_resource_matching_allowed", func(t *testing.T) {
		// UAT accessing the project it is scoped to.
		scoped := NewScopedUserIdentity(baseUser, "proj-1", []string{"project:read"})
		resource := Resource{Type: "project", ID: "proj-1"}
		result := authz.enforceUATConstraints(scoped, resource, "read")
		if result != nil {
			t.Fatalf("expected nil (allowed) for matching project, got denial: %s", result.Reason)
		}
	})

	t.Run("project_resource_mismatch_denied", func(t *testing.T) {
		// UAT accessing a different project.
		scoped := NewScopedUserIdentity(baseUser, "proj-1", []string{"project:read"})
		resource := Resource{Type: "project", ID: "proj-2"}
		result := authz.enforceUATConstraints(scoped, resource, "read")
		if result == nil {
			t.Fatal("expected denial for mismatched project, got nil")
		}
		if result.Reason != "token not scoped for this project" {
			t.Fatalf("unexpected reason: %s", result.Reason)
		}
	})
}

// TestM2_HubPermissionsNoUATScope verifies that all hub permissions have
// empty UATScope values (M2 fix). Hub operations should not be accessible
// via project-scoped UATs.
func TestM2_HubPermissionsNoUATScope(t *testing.T) {
	for _, perm := range permissions.Registry {
		if perm.Resource != permissions.ResourceHub {
			continue
		}
		if perm.UATScope != "" {
			t.Errorf("hub permission %s has non-empty UATScope %q; hub permissions must not be accessible via project-scoped UATs",
				perm.ID, perm.UATScope)
		}
	}
}
