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
// that Decide-based permission checks work.
func newPolicyTestServer(t *testing.T) *Server {
	t.Helper()
	srv, s := testServer(t)
	seedRoleDefinitions(context.Background(), s)
	return srv
}

// TestPolicyEndpoints_RequireAdmin verifies that the policy API is removed in
// CO1: all callers (admin, non-admin, unauthenticated) receive 410 Gone.
func TestPolicyEndpoints_RequireAdmin(t *testing.T) {
	srv := newPolicyTestServer(t)
	admin := NewAuthenticatedUser(tid("pol-admin"), "admin@test.com", "Admin", "admin", "cli")
	member := NewAuthenticatedUser(tid("pol-member"), "user@test.com", "User", "member", "cli")

	policyBody := `{"name":"test-pol","scopeType":"hub","actions":["read"],"effect":"allow"}`

	// CO1: handlePolicies returns 410 Gone for every caller/method.
	for _, tc := range []struct {
		name string
		user *AuthenticatedUser // nil = unauthenticated
	}{
		{"non-admin member", member},
		{"unauthenticated", nil},
		{"admin", admin},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/policies", strings.NewReader(policyBody))
			req.Header.Set("Content-Type", "application/json")
			if tc.user != nil {
				req = req.WithContext(contextWithIdentity(req.Context(), tc.user))
			}
			rr := httptest.NewRecorder()
			srv.handlePolicies(rr, req)
			if rr.Code != http.StatusGone {
				t.Errorf("expected 410, got %d: %s", rr.Code, rr.Body.String())
			}
		})
	}
}

// TestPolicyRouteEndpoints_RequireAdmin verifies that policy routes dispatched
// through handlePolicyRoutes return 410 Gone in CO1 for all callers.
func TestPolicyRouteEndpoints_RequireAdmin(t *testing.T) {
	srv := newPolicyTestServer(t)
	member := NewAuthenticatedUser(tid("pol-route-member"), "user@test.com", "User", "member", "cli")

	// CO1: handlePolicyRoutes returns 410 Gone for every method/caller.
	for _, tc := range []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"PATCH policy", http.MethodPatch, "/api/v1/policies/nonexistent-id", `{"name":"updated"}`},
		{"DELETE policy", http.MethodDelete, "/api/v1/policies/nonexistent-id", ""},
		{"DELETE binding", http.MethodDelete, "/api/v1/policies/nonexistent-id/bindings/user/user-1", ""},
	} {
		t.Run(tc.name+" non-admin", func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			req = req.WithContext(contextWithIdentity(req.Context(), member))
			rr := httptest.NewRecorder()
			srv.handlePolicyRoutes(rr, req)
			if rr.Code != http.StatusGone {
				t.Errorf("non-admin: expected 410, got %d: %s", rr.Code, rr.Body.String())
			}
		})

		t.Run(tc.name+" unauthenticated", func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			srv.handlePolicyRoutes(rr, req)
			if rr.Code != http.StatusGone {
				t.Errorf("unauthenticated: expected 410, got %d: %s", rr.Code, rr.Body.String())
			}
		})
	}
}

// TestPolicyEndpoints_AdminPassesThrough verifies that even admin callers
// receive 410 Gone in CO1 — the policy API is fully removed.
func TestPolicyEndpoints_AdminPassesThrough(t *testing.T) {
	srv := newPolicyTestServer(t)
	admin := NewAuthenticatedUser(tid("pol-admin-pt"), "admin@test.com", "Admin", "admin", "cli")

	// POST (create) as admin -> 410
	body := `{"name":"admin-test-pol","scopeType":"hub","actions":["read"],"effect":"allow"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/policies", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handlePolicies(rr, req)
	if rr.Code != http.StatusGone {
		t.Fatalf("admin create: expected 410, got %d: %s", rr.Code, rr.Body.String())
	}

	// GET (list) as admin -> 410
	req = httptest.NewRequest(http.MethodGet, "/api/v1/policies", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr = httptest.NewRecorder()
	srv.handlePolicies(rr, req)
	if rr.Code != http.StatusGone {
		t.Fatalf("admin list: expected 410, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestPolicyRouteGuard_AuthenticatedGetsGone verifies that policy routes
// return 410 Gone for any authenticated user after CO1 cutover. OBS-5
// removed policy.read/policy.list from all roles and the route classification
// was relaxed to RouteAuthenticated.
func TestPolicyRouteGuard_AuthenticatedGetsGone(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

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

	// Hub-admin should now reach the 410 Gone handler (route is RouteAuthenticated).
	rec := doRequestAsUser(t, srv, hubAdminUser, http.MethodGet, "/api/v1/policies", nil)
	if rec.Code != http.StatusGone {
		t.Errorf("hub-admin accessing policy route: expected 410 Gone, got %d", rec.Code)
	}

	// Super-admin also gets 410 Gone (the handler always returns Gone).
	createTestUserWithRole(t, s, tid("sa-pol"), "sa-pol@test.com", "admin", store.SystemRoleSuperAdmin)
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/policies", nil)
	if rec.Code != http.StatusGone {
		t.Errorf("super-admin accessing policy route: expected 410 Gone, got %d", rec.Code)
	}
}
