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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newPolicyTestServer(t *testing.T) *Server {
	t.Helper()
	s, err := newTestStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	srv := &Server{store: s}
	return srv
}

// TestPolicyEndpoints_RequireAdmin verifies that all policy endpoints require
// admin authentication. Non-admin authenticated users must receive 403 and
// unauthenticated callers must receive 401.
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
		{
			name:    "GET /api/v1/policies (listPolicies)",
			method:  http.MethodGet,
			path:    "/api/v1/policies",
			handler: srv.handlePolicies,
			// Admin gets 200
			wantAdmin:  http.StatusOK,
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
// through handlePolicyRoutes require admin for get/update/delete operations.
func TestPolicyRouteEndpoints_RequireAdmin(t *testing.T) {
	srv := newPolicyTestServer(t)
	member := NewAuthenticatedUser("user-1", "user@test.com", "User", "member", "cli")

	// All of these go through handlePolicyRoutes which dispatches to individual
	// handlers that each check requireAdmin.
	type testCase struct {
		name   string
		method string
		path   string
		body   string
	}

	tests := []testCase{
		{
			name:   "GET /api/v1/policies/{id} (getPolicy)",
			method: http.MethodGet,
			path:   "/api/v1/policies/nonexistent-id",
		},
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
			name:   "GET /api/v1/policies/{id}/bindings (listPolicyBindings)",
			method: http.MethodGet,
			path:   "/api/v1/policies/nonexistent-id/bindings",
		},
		{
			name:   "POST /api/v1/policies/{id}/bindings (addPolicyBinding)",
			method: http.MethodPost,
			path:   "/api/v1/policies/nonexistent-id/bindings",
			body:   `{"principalType":"user","principalId":"user-1"}`,
		},
		{
			name:   "DELETE /api/v1/policies/{id}/bindings/user/user-1 (removePolicyBinding)",
			method: http.MethodDelete,
			path:   "/api/v1/policies/nonexistent-id/bindings/user/user-1",
		},
	}

	for _, tc := range tests {
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
