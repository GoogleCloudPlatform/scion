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
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestHubAdminReadUpdateRoutePermissions checks the admin-mode and
// allow-list routes against their own read and update permissions. The
// route guard checks the read permission, so a role holding only the read
// permission can read and is refused every write, and a role holding only
// the update permission is refused the read.
func TestHubAdminReadUpdateRoutePermissions(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	seedRoleDefinitions(ctx, s)

	bindCustomRole := func(name string, perms []string) UserIdentity {
		t.Helper()
		rd, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
			Name:        name,
			Description: "test role " + name,
			ScopeType:   store.RoleScopeSystem,
			Permissions: perms,
		})
		if err != nil {
			t.Fatalf("create role %s: %v", name, err)
		}
		id := tid(name)
		email := name + "@test.com"
		if err := s.CreateUser(ctx, &store.User{
			ID: id, Email: email, DisplayName: name, Role: "member", Status: "active",
		}); err != nil {
			t.Fatalf("create user %s: %v", name, err)
		}
		if _, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: rd.ID,
			PrincipalType:    "user",
			PrincipalID:      id,
			ScopeType:        store.RoleScopeSystem,
		}); err != nil {
			t.Fatalf("bind role %s: %v", name, err)
		}
		return NewAuthenticatedUser(id, email, name, "member", "api")
	}

	readOnly := bindCustomRole("hub-rw-read-only", []string{"hub.admin_mode.read", "hub.allow_list.read"})
	updateOnly := bindCustomRole("hub-rw-update-only", []string{"hub.admin_mode.update", "hub.allow_list.update"})
	readUpdate := bindCustomRole("hub-rw-read-update", []string{
		"hub.admin_mode.read", "hub.admin_mode.update", "hub.allow_list.read", "hub.allow_list.update",
	})

	maintenance := srv.routeGuard(routeMetadataTable["/api/v1/admin/maintenance"], srv.handleAdminMaintenance)
	allowList := srv.routeGuard(routeMetadataTable["/api/v1/admin/allow-list"], srv.handleAdminAllowList)
	allowListByEmail := srv.routeGuard(routeMetadataTable["/api/v1/admin/allow-list/"], srv.handleAdminAllowListByEmail)

	const allowed, refused = true, false
	cases := []struct {
		name     string
		handler  http.HandlerFunc
		method   string
		path     string
		body     string
		identity UserIdentity
		want     bool
	}{
		{"maintenance_GET_read_only", maintenance, http.MethodGet, "/api/v1/admin/maintenance", "", readOnly, allowed},
		{"maintenance_PUT_read_only", maintenance, http.MethodPut, "/api/v1/admin/maintenance", `{"message":"m"}`, readOnly, refused},
		{"maintenance_GET_update_only", maintenance, http.MethodGet, "/api/v1/admin/maintenance", "", updateOnly, refused},
		{"maintenance_PUT_read_update", maintenance, http.MethodPut, "/api/v1/admin/maintenance", `{"message":"m"}`, readUpdate, allowed},

		{"allow_list_GET_read_only", allowList, http.MethodGet, "/api/v1/admin/allow-list", "", readOnly, allowed},
		{"allow_list_POST_read_only", allowList, http.MethodPost, "/api/v1/admin/allow-list", `{"email":"a@example.com"}`, readOnly, refused},
		{"allow_list_GET_update_only", allowList, http.MethodGet, "/api/v1/admin/allow-list", "", updateOnly, refused},
		{"allow_list_POST_read_update", allowList, http.MethodPost, "/api/v1/admin/allow-list", `{"email":"b@example.com"}`, readUpdate, allowed},

		{"allow_list_domains_GET_read_only", allowListByEmail, http.MethodGet, "/api/v1/admin/allow-list/domains", "", readOnly, allowed},
		{"allow_list_domains_GET_update_only", allowListByEmail, http.MethodGet, "/api/v1/admin/allow-list/domains", "", updateOnly, refused},
		{"allow_list_import_POST_read_only", allowListByEmail, http.MethodPost, "/api/v1/admin/allow-list/import", `{"emails":[{"email":"c@example.com"}]}`, readOnly, refused},
		{"allow_list_import_POST_read_update", allowListByEmail, http.MethodPost, "/api/v1/admin/allow-list/import", `{"emails":[{"email":"d@example.com"}]}`, readUpdate, allowed},
		{"allow_list_DELETE_read_only", allowListByEmail, http.MethodDelete, "/api/v1/admin/allow-list/e@example.com", "", readOnly, refused},
		{"allow_list_DELETE_read_update", allowListByEmail, http.MethodDelete, "/api/v1/admin/allow-list/e@example.com", "", readUpdate, allowed},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var req *http.Request
			if tc.body != "" {
				req = httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
				req.Header.Set("Content-Type", "application/json")
			} else {
				req = httptest.NewRequest(tc.method, tc.path, nil)
			}
			req = req.WithContext(contextWithIdentity(ctx, tc.identity))
			rr := httptest.NewRecorder()
			tc.handler(rr, req)

			// An allowed request may still fail later in the handler (for
			// example 404 for an unknown email); it must not be refused.
			gotRefused := rr.Code == http.StatusForbidden || rr.Code == http.StatusUnauthorized
			if tc.want == allowed && gotRefused {
				t.Fatalf("%s %s: got %d, want the request allowed; body: %s", tc.method, tc.path, rr.Code, rr.Body.String())
			}
			if tc.want == refused && rr.Code != http.StatusForbidden {
				t.Fatalf("%s %s: got %d, want 403; body: %s", tc.method, tc.path, rr.Code, rr.Body.String())
			}
		})
	}
}
