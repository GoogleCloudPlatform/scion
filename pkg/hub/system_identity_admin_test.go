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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// The mutating workstation routes gate on hub.config.update (super-admin
// bypass preserved) in addition to workstation mode and loopback origin, so a
// non-admin or non-user loopback caller cannot rebind the hub's identity or
// rewrite its settings even though it reaches the loopback hub.
func TestSystemMutatingRoutesRequireHubAdmin(t *testing.T) {
	srv := &Server{config: DefaultServerConfig(), mux: http.NewServeMux(),
		workstation: true, authzService: NewAuthzService(nil, nil)}
	srv.registerRoutes()

	routes := []struct {
		method, path string
	}{
		{http.MethodPut, "/api/v1/system/identity"},
		{http.MethodPatch, "/api/v1/system/workstation-settings"},
	}
	// A plain member and a non-user identity must both be refused by the guard
	// before the handler runs. A nil-store authz service grants neither the
	// permission nor a super-admin bypass.
	identities := map[string]Identity{
		"member user": NewAuthenticatedUser("user-1", "user@example.com", "User", "member", "api"),
		"agent":       newAgentIdentityFromStore(&store.Agent{ID: "agent-1", ProjectID: "project-1"}),
	}
	for _, rt := range routes {
		for who, id := range identities {
			t.Run(rt.path+"/"+who, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(contextWithIdentity(context.Background(), id), time.Second)
				defer cancel()
				req := httptest.NewRequest(rt.method, rt.path, nil).WithContext(ctx)
				req.RemoteAddr = "127.0.0.1:1234"
				rr := httptest.NewRecorder()
				srv.mux.ServeHTTP(rr, req)
				if rr.Code != http.StatusForbidden {
					t.Fatalf("%s %s as %s = %d, want 403: %s", rt.method, rt.path, who, rr.Code, rr.Body.String())
				}
			})
		}
		// No identity at all → 401, never through to the handler.
		t.Run(rt.path+"/unauthenticated", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			req := httptest.NewRequest(rt.method, rt.path, nil).WithContext(ctx)
			req.RemoteAddr = "127.0.0.1:1234"
			rr := httptest.NewRecorder()
			srv.mux.ServeHTTP(rr, req)
			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("%s %s unauthenticated = %d, want 401: %s", rt.method, rt.path, rr.Code, rr.Body.String())
			}
		})
	}
}

// The read-only workstation routes keep workstation-only access: no admin
// permission is required, so a non-admin loopback caller still reads status.
func TestSystemReadRoutesStayWorkstationOnly(t *testing.T) {
	srv := &Server{config: DefaultServerConfig(), mux: http.NewServeMux(),
		workstation: true, authzService: NewAuthzService(nil, nil)}
	srv.registerRoutes()

	id := NewAuthenticatedUser("user-1", "user@example.com", "User", "member", "api")
	ctx, cancel := context.WithTimeout(contextWithIdentity(context.Background(), id), time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/system/status", nil).WithContext(ctx)
	req.RemoteAddr = "127.0.0.1:1234"
	rr := httptest.NewRecorder()
	srv.mux.ServeHTTP(rr, req)
	if rr.Code == http.StatusForbidden || rr.Code == http.StatusUnauthorized {
		t.Fatalf("GET /api/v1/system/status as a member = %d, want it not gated on admin", rr.Code)
	}
}

// With workstation mode off the routes 404 as before, whatever the caller.
func TestSystemIdentityIs404OutsideWorkstation(t *testing.T) {
	srv := &Server{config: DefaultServerConfig(), mux: http.NewServeMux(),
		workstation: false, authzService: NewAuthzService(nil, nil)}
	srv.registerRoutes()

	id := NewAuthenticatedUser("admin-1", "admin@example.com", "Admin", "admin", "api")
	ctx, cancel := context.WithTimeout(contextWithIdentity(context.Background(), id), time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/system/identity", nil).WithContext(ctx)
	req.RemoteAddr = "127.0.0.1:1234"
	rr := httptest.NewRecorder()
	srv.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("PUT /api/v1/system/identity outside workstation = %d, want 404", rr.Code)
	}
}
