//go:build !no_sqlite

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

package hub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// newPolicyTestServer creates a test server with authzService initialized so
// that Decide-based permission checks work. Callers get a super-admin user
// (role "admin") who passes through the step-1 bypass in Decide, and a
// regular member who does not.
func newPolicyTestServer(t *testing.T) *Server {
	t.Helper()
	srv, s := testServer(t)
	seedRoleDefinitions(context.Background(), s)
	return srv
}

// TestPolicyEndpoints_RequireAdmin verifies that policy endpoints enforce
// permission-based checks. Policy permissions are NOT in the hub-admin role,
// so only super-admins can access them. Non-admin authenticated users must
// receive 403 and unauthenticated callers must receive 401.
func TestPolicyEndpoints_RequireAdmin(t *testing.T) {
	srv := newPolicyTestServer(t)
	admin := NewAuthenticatedUser("admin-1", "admin@test.com", "Admin", "admin", "cli")
	member := NewAuthenticatedUser("user-1", "user@test.com", "User", "member", "cli")

	type testCase struct {
		name       string
		method     string
		path       string
		body       string
		handler    func(http.ResponseWriter, *http.Request)
		wantAdmin  int // expected status for admin (2xx or 4xx for missing body/resource)
		wantMember int // expected status for non-admin
		wantAnon   int // expected status for unauthenticated
	}

	policyBody := `{"name":"test-pol","scopeType":"hub","actions":["read"],"effect":"allow"}`

	tests := []testCase{
		{
			name:    "POST /api/v1/policies (createPolicy)",
			method:  http.MethodPost,
			path:    "/api/v1/policies",
			body:    policyBody,
			handler: srv.handlePolicies,
			// Admin gets 201 (created) — the request is valid
			wantAdmin:  http.StatusCreated,
			wantMember: http.StatusForbidden,
			wantAnon:   http.StatusUnauthorized,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Test non-admin member → 403
			var bodyReader *strings.Reader
			if tc.body != "" {
				bodyReader = strings.NewReader(tc.body)
			} else {
				bodyReader = strings.NewReader("")
			}
			req := httptest.NewRequest(tc.method, tc.path, bodyReader)
			req.Header.Set("Content-Type", "application/json")
			req = req.WithContext(contextWithIdentity(req.Context(), member))
			rr := httptest.NewRecorder()
			tc.handler(rr, req)
			if rr.Code != tc.wantMember {
				t.Errorf("non-admin: expected %d, got %d: %s", tc.wantMember, rr.Code, rr.Body.String())
			}

			// Test unauthenticated → 401
			if tc.body != "" {
				bodyReader = strings.NewReader(tc.body)
			} else {
				bodyReader = strings.NewReader("")
			}
			req = httptest.NewRequest(tc.method, tc.path, bodyReader)
			req.Header.Set("Content-Type", "application/json")
			// No identity in context
			rr = httptest.NewRecorder()
			tc.handler(rr, req)
			if rr.Code != tc.wantAnon {
				t.Errorf("unauthenticated: expected %d, got %d: %s", tc.wantAnon, rr.Code, rr.Body.String())
			}

			// Test admin → passes through (gets expected status, not 401/403)
			if tc.body != "" {
				bodyReader = strings.NewReader(tc.body)
			} else {
				bodyReader = strings.NewReader("")
			}
			req = httptest.NewRequest(tc.method, tc.path, bodyReader)
			req.Header.Set("Content-Type", "application/json")
			req = req.WithContext(contextWithIdentity(req.Context(), admin))
			rr = httptest.NewRecorder()
			tc.handler(rr, req)
			if rr.Code != tc.wantAdmin {
				t.Errorf("admin: expected %d, got %d: %s", tc.wantAdmin, rr.Code, rr.Body.String())
			}
		})
	}
}

// TestPolicyRouteEndpoints_RequireAdmin verifies that policy routes dispatched
// through handlePolicyRoutes enforce permission checks for write operations.
// Read operations (GET) are protected by the route guard, not the handler.
func TestPolicyRouteEndpoints_RequireAdmin(t *testing.T) {
	srv := newPolicyTestServer(t)
	member := NewAuthenticatedUser("user-1", "user@test.com", "User", "member", "cli")

	type testCase struct {
		name   string
		method string
		path   string
		body   string
	}

	// Write operations have inline Decide checks and should return 403
	writeTests := []testCase{
		{
			name:   "PATCH /api/v1/policies/{id} (updatePolicy)",
			method: http.MethodPatch,
			path:   "/api/v1/policies/nonexistent-id",
			body:   `{"name":"updated"}`,
		},
		{
			name:   "DELETE /api/v1/policies/{id} (deletePolicy)",
			method: http.MethodDelete,
			path:   "/api/v1/policies/nonexistent-id",
		},
		{
			name:   "DELETE /api/v1/policies/{id}/bindings/user/user-1 (removePolicyBinding)",
			method: http.MethodDelete,
			path:   "/api/v1/policies/nonexistent-id/bindings/user/user-1",
		},
	}

	for _, tc := range writeTests {
		t.Run(tc.name+" non-admin", func(t *testing.T) {
			var bodyReader *strings.Reader
			if tc.body != "" {
				bodyReader = strings.NewReader(tc.body)
			} else {
				bodyReader = strings.NewReader("")
			}
			req := httptest.NewRequest(tc.method, tc.path, bodyReader)
			req.Header.Set("Content-Type", "application/json")
			req = req.WithContext(contextWithIdentity(req.Context(), member))
			rr := httptest.NewRecorder()
			srv.handlePolicyRoutes(rr, req)
			if rr.Code != http.StatusForbidden {
				t.Errorf("non-admin: expected 403, got %d: %s", rr.Code, rr.Body.String())
			}
		})

		t.Run(tc.name+" unauthenticated", func(t *testing.T) {
			var bodyReader *strings.Reader
			if tc.body != "" {
				bodyReader = strings.NewReader(tc.body)
			} else {
				bodyReader = strings.NewReader("")
			}
			req := httptest.NewRequest(tc.method, tc.path, bodyReader)
			req.Header.Set("Content-Type", "application/json")
			// No identity in context
			rr := httptest.NewRecorder()
			srv.handlePolicyRoutes(rr, req)
			if rr.Code != http.StatusUnauthorized {
				t.Errorf("unauthenticated: expected 401, got %d: %s", rr.Code, rr.Body.String())
			}
		})
	}
}

// TestPolicyEndpoints_AdminPassesThrough verifies that admin callers can
// successfully reach the underlying handler logic (not blocked by the gate).
func TestPolicyEndpoints_AdminPassesThrough(t *testing.T) {
	srv := newPolicyTestServer(t)
	admin := NewAuthenticatedUser("admin-1", "admin@test.com", "Admin", "admin", "cli")

	// Create a policy as admin — should succeed
	body := `{"name":"admin-test-pol","scopeType":"hub","actions":["read"],"effect":"allow"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/policies", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handlePolicies(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("admin create: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}

	// List policies as admin — should succeed
	req = httptest.NewRequest(http.MethodGet, "/api/v1/policies", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr = httptest.NewRecorder()
	srv.handlePolicies(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("admin list: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestPolicyRouteGuard_SuperAdminOnlyAccess verifies that policy routes in the
// route metadata table enforce super-admin-only access. Policy permissions are
// NOT in the hub-admin role, so hub-admins should be denied.
func TestPolicyRouteGuard_SuperAdminOnlyAccess(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	seedRoleDefinitions(ctx, s)

	// Create a hub-admin user (member role with hub-admin role binding)
	hubAdminUser := &store.User{
		ID: tid("ha-pol"), Email: "ha-pol@test.com", DisplayName: "Hub Admin",
		Role: "member", Status: "active",
	}
	if err := s.CreateUser(ctx, hubAdminUser); err != nil {
		t.Fatalf("create hub-admin user: %v", err)
	}
	hubAdminRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	if err != nil {
		t.Fatalf("get hub-admin role definition: %v", err)
	}
	if _, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: hubAdminRD.ID,
		PrincipalType:    "user",
		PrincipalID:      hubAdminUser.ID,
		ScopeType:        store.RoleScopeSystem,
	}); err != nil {
		t.Fatalf("create hub-admin role binding: %v", err)
	}

	okHandler := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}

	// Test the policy route guard: hub-admin should be denied because
	// policy.read is not in the hub-admin role.
	meta := routeMetadataTable["/api/v1/policies"]
	handler := srv.routeGuard(meta, okHandler)

	hubAdmin := NewAuthenticatedUser(tid("ha-pol"), "ha-pol@test.com", "Hub Admin", "member", "api")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/policies", nil)
	req = req.WithContext(contextWithIdentity(ctx, hubAdmin))
	rr := httptest.NewRecorder()
	handler(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("hub-admin accessing policy route: expected 403, got %d", rr.Code)
	}

	// Super-admin should pass
	superAdmin := NewAuthenticatedUser("admin-1", "admin@test.com", "Admin", "admin", "api")
	req = httptest.NewRequest(http.MethodGet, "/api/v1/policies", nil)
	req = req.WithContext(contextWithIdentity(ctx, superAdmin))
	rr = httptest.NewRecorder()
	handler(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("super-admin accessing policy route: expected 200, got %d", rr.Code)
	}
}
