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
	"testing"
)

// TestRouteGuardHubAdminPermissionWithoutAuthzServiceIsRefused: a hub-admin
// route that declares a Permission, served by a Server with no
// authorization service, is refused with 500 for every caller and never
// reaches the handler. A route without a Permission still uses requireAdmin.
func TestRouteGuardHubAdminPermissionWithoutAuthzServiceIsRefused(t *testing.T) {
	sessionOnly := guardedSessionOnly()
	permissionOnly := guardedSessionOnly()
	permissionOnly.SessionOnly = ""
	noPermission := RouteMetadata{
		Pattern: "GET /api/v1/test/legacy", RouteID: "test.legacy",
		Classification: RouteHubAdmin,
	}

	admin := NewAuthenticatedUser("admin-1", "admin@example.com", "Admin", "admin", "web")
	session := CredentialContext{Kind: CredentialKindInteractive, ID: "sess-1"}
	token := CredentialContext{Kind: CredentialKindUAT, ID: "uat-1"}

	tests := []struct {
		name       string
		meta       RouteMetadata
		identity   Identity
		credential *CredentialContext
		wantStatus int
		wantServed bool
	}{
		{name: "session-only route, admin session", meta: sessionOnly, identity: admin, credential: &session, wantStatus: http.StatusInternalServerError},
		{name: "session-only route, admin token", meta: sessionOnly, identity: admin, credential: &token, wantStatus: http.StatusInternalServerError},
		{name: "session-only route, admin without credential context", meta: sessionOnly, identity: admin, wantStatus: http.StatusInternalServerError},
		{name: "session-only route, no identity", meta: sessionOnly, wantStatus: http.StatusInternalServerError},
		{name: "permission route, admin session", meta: permissionOnly, identity: admin, credential: &session, wantStatus: http.StatusInternalServerError},
		{name: "permission route, admin token", meta: permissionOnly, identity: admin, credential: &token, wantStatus: http.StatusInternalServerError},
		{name: "route without permission, admin session", meta: noPermission, identity: admin, credential: &session, wantStatus: http.StatusOK, wantServed: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := &Server{}
			served := false
			h := srv.routeGuard(tt.meta, func(w http.ResponseWriter, _ *http.Request) {
				served = true
				w.WriteHeader(http.StatusOK)
			})

			ctx := context.Background()
			if tt.identity != nil {
				ctx = contextWithIdentity(ctx, tt.identity)
			}
			if tt.credential != nil {
				ctx = contextWithCredentialContext(ctx, *tt.credential)
			}
			req := httptest.NewRequest(http.MethodGet, "/api/v1/test/op", nil).WithContext(ctx)
			rr := httptest.NewRecorder()
			h(rr, req)

			if rr.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d (body: %s)", rr.Code, tt.wantStatus, rr.Body.String())
			}
			if served != tt.wantServed {
				t.Errorf("handler served = %v, want %v", served, tt.wantServed)
			}
		})
	}
}
