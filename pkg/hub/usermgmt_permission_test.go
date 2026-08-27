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

// TestUserMgmtPermissionConversion verifies that the user management routes
// (PR-A3) have correct permission-based metadata and that the route guard
// enforces access correctly for super-admin, hub-admin, and regular members.
func TestUserMgmtPermissionConversion(t *testing.T) {
	// Verify route metadata entries have the expected permissions.
	metadataTests := []struct {
		pattern    string
		permission string
		resource   string
		action     string
	}{
		{"/api/v1/admin/allow-list", "hub.allow_list.update", "hub", "update"},
		{"/api/v1/admin/allow-list/", "hub.allow_list.update", "hub", "update"},
		{"/api/v1/admin/users/invite/bulk", "user.invite", "user", "invite"},
		{"/api/v1/admin/users/invite", "user.invite", "user", "invite"},
		{"/api/v1/admin/invites", "user.invite", "user", "invite"},
		{"/api/v1/admin/invites/", "user.invite", "user", "invite"},
		{"/api/v1/admin/validate-resources", "hub.validate.execute", "hub", "execute"},
	}

	for _, tc := range metadataTests {
		t.Run("metadata_"+tc.pattern, func(t *testing.T) {
			meta, ok := routeMetadataTable[tc.pattern]
			if !ok {
				t.Fatalf("route %q not found in routeMetadataTable", tc.pattern)
			}
			if meta.Classification != RouteHubAdmin {
				t.Errorf("route %q: classification = %q, want %q", tc.pattern, meta.Classification, RouteHubAdmin)
			}
			if meta.Permission != tc.permission {
				t.Errorf("route %q: Permission = %q, want %q", tc.pattern, meta.Permission, tc.permission)
			}
			if meta.Resource != tc.resource {
				t.Errorf("route %q: Resource = %q, want %q", tc.pattern, meta.Resource, tc.resource)
			}
			if meta.Action != tc.action {
				t.Errorf("route %q: Action = %q, want %q", tc.pattern, meta.Action, tc.action)
			}
		})
	}

	// Test route guard enforcement for each converted endpoint.
	srv, s := testServer(t)
	ctx := context.Background()

	seedRoleDefinitions(ctx, s)

	adminUser := &store.User{
		ID: tid("admin-um"), Email: "admin-um@test.com", DisplayName: "Admin",
		Role: "admin", Status: "active",
	}
	memberUser := &store.User{
		ID: tid("member-um"), Email: "member-um@test.com", DisplayName: "Member",
		Role: "member", Status: "active",
	}
	hubAdminUser := &store.User{
		ID: tid("hub-admin-um"), Email: "hub-admin-um@test.com", DisplayName: "Hub Admin",
		Role: "member", Status: "active",
	}
	for _, u := range []*store.User{adminUser, memberUser, hubAdminUser} {
		if err := s.CreateUser(ctx, u); err != nil {
			t.Fatalf("create user %s: %v", u.Email, err)
		}
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

	okHandler := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}

	admin := NewAuthenticatedUser(tid("admin-um"), "admin-um@test.com", "Admin", "admin", "api")
	member := NewAuthenticatedUser(tid("member-um"), "member-um@test.com", "Member", "member", "api")
	hubAdmin := NewAuthenticatedUser(tid("hub-admin-um"), "hub-admin-um@test.com", "Hub Admin", "member", "api")

	guardTests := []struct {
		name       string
		pattern    string
		identity   UserIdentity
		wantStatus int
	}{
		// Allow-list endpoints: hub.allow_list.update
		{"allow_list_super_admin", "/api/v1/admin/allow-list", admin, http.StatusOK},
		{"allow_list_hub_admin", "/api/v1/admin/allow-list", hubAdmin, http.StatusOK},
		{"allow_list_member_denied", "/api/v1/admin/allow-list", member, http.StatusForbidden},
		{"allow_list_byemail_super_admin", "/api/v1/admin/allow-list/", admin, http.StatusOK},
		{"allow_list_byemail_hub_admin", "/api/v1/admin/allow-list/", hubAdmin, http.StatusOK},
		{"allow_list_byemail_member_denied", "/api/v1/admin/allow-list/", member, http.StatusForbidden},

		// Invite endpoints: user.invite
		{"invite_bulk_super_admin", "/api/v1/admin/users/invite/bulk", admin, http.StatusOK},
		{"invite_bulk_hub_admin", "/api/v1/admin/users/invite/bulk", hubAdmin, http.StatusOK},
		{"invite_bulk_member_denied", "/api/v1/admin/users/invite/bulk", member, http.StatusForbidden},
		{"invite_super_admin", "/api/v1/admin/users/invite", admin, http.StatusOK},
		{"invite_hub_admin", "/api/v1/admin/users/invite", hubAdmin, http.StatusOK},
		{"invite_member_denied", "/api/v1/admin/users/invite", member, http.StatusForbidden},
		{"invites_list_super_admin", "/api/v1/admin/invites", admin, http.StatusOK},
		{"invites_list_hub_admin", "/api/v1/admin/invites", hubAdmin, http.StatusOK},
		{"invites_list_member_denied", "/api/v1/admin/invites", member, http.StatusForbidden},
		{"invites_byid_super_admin", "/api/v1/admin/invites/", admin, http.StatusOK},
		{"invites_byid_hub_admin", "/api/v1/admin/invites/", hubAdmin, http.StatusOK},
		{"invites_byid_member_denied", "/api/v1/admin/invites/", member, http.StatusForbidden},

		// Validate resources: hub.validate.execute
		{"validate_super_admin", "/api/v1/admin/validate-resources", admin, http.StatusOK},
		{"validate_hub_admin", "/api/v1/admin/validate-resources", hubAdmin, http.StatusOK},
		{"validate_member_denied", "/api/v1/admin/validate-resources", member, http.StatusForbidden},
	}

	for _, tc := range guardTests {
		t.Run(tc.name, func(t *testing.T) {
			meta, ok := routeMetadataTable[tc.pattern]
			if !ok {
				t.Fatalf("route %q not found in routeMetadataTable", tc.pattern)
			}
			handler := srv.routeGuard(meta, okHandler)

			req := httptest.NewRequest(http.MethodGet, tc.pattern, nil)
			req = req.WithContext(contextWithIdentity(ctx, tc.identity))
			rr := httptest.NewRecorder()
			handler(rr, req)

			if rr.Code != tc.wantStatus {
				t.Errorf("route %s with %s: got %d, want %d; body: %s",
					tc.pattern, tc.identity.Email(), rr.Code, tc.wantStatus, rr.Body.String())
			}
		})
	}
}
