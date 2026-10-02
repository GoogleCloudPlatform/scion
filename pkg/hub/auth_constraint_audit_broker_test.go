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

type constraintAuditBrokerMiddlewareVariant struct {
	name      string
	auditable bool
	wrap      func(*BrokerAuthService, AuditLogger, http.Handler) http.Handler
}

func constraintAuditBrokerMiddlewareVariants() []constraintAuditBrokerMiddlewareVariant {
	return []constraintAuditBrokerMiddlewareVariant{
		{
			name: "ordinary",
			wrap: func(svc *BrokerAuthService, _ AuditLogger, next http.Handler) http.Handler {
				return BrokerAuthMiddleware(svc)(next)
			},
		},
		{
			name:      "auditable",
			auditable: true,
			wrap: func(svc *BrokerAuthService, logger AuditLogger, next http.Handler) http.Handler {
				return AuditableBrokerAuthMiddleware(svc, logger)(next)
			},
		},
	}
}

func constraintAuditBrokerFullChain(
	svc *BrokerAuthService,
	logger AuditLogger,
	variant constraintAuditBrokerMiddlewareVariant,
	next http.Handler,
) http.Handler {
	broker := variant.wrap(svc, logger, next)
	return UnifiedAuthMiddleware(AuthConfig{Mode: "production", BrokerAuthSvc: svc})(broker)
}

func TestConstraintAuditBrokerAuth_ForgedSignatureIsIndistinguishable(t *testing.T) {
	for _, variant := range constraintAuditBrokerMiddlewareVariants() {
		t.Run(variant.name, func(t *testing.T) {
			srv, s := testServer(t)
			_, signReq := setupSignedBrokerRequest(t, srv.brokerAuthService, s)
			logger := &mockAuditLogger{}
			called := false
			handler := constraintAuditBrokerFullChain(srv.brokerAuthService, logger, variant,
				http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))

			req := signReq(http.MethodGet, constraintAuditAuthTestPath, nil)
			req.Header.Set(HeaderSignature, strings.Repeat("0", 64))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			assertConstraintAuditNotFound(t, rec)
			if called {
				t.Fatal("forged broker signature invoked the downstream handler")
			}
			if variant.auditable {
				if len(logger.brokerEvents) != 1 || logger.brokerEvents[0].EventType != BrokerAuthEventAuthFailure || logger.brokerEvents[0].Success {
					t.Fatalf("auditable forged-signature events = %#v, want one auth failure", logger.brokerEvents)
				}
			}
		})
	}
}

func TestConstraintAuditBrokerAuth_RejectedOnBehalfOfIsIndistinguishable(t *testing.T) {
	for _, variant := range constraintAuditBrokerMiddlewareVariants() {
		for _, rejection := range []struct {
			name  string
			value func(t *testing.T, s store.Store) string
		}{
			{
				name: "unsupported scheme",
				value: func(*testing.T, store.Store) string {
					return "service:internal@example.com"
				},
			},
			{
				name: "unknown user",
				value: func(*testing.T, store.Store) string {
					return "user:missing-constraint-audit@example.com"
				},
			},
			{
				name: "suspended user",
				value: func(t *testing.T, s store.Store) string {
					user := &store.User{
						ID: tid("constraint-audit-suspended"), Email: "constraint-audit-suspended@example.com",
						DisplayName: "Suspended", Role: "member", Status: store.UserStatusSuspended,
					}
					if err := s.CreateUser(context.Background(), user); err != nil {
						t.Fatalf("CreateUser() error = %v", err)
					}
					return "user:" + user.Email
				},
			},
		} {
			t.Run(variant.name+"/"+rejection.name, func(t *testing.T) {
				srv, s := testServer(t)
				_, signReq := setupSignedBrokerRequest(t, srv.brokerAuthService, s)
				logger := &mockAuditLogger{}
				called := false
				handler := constraintAuditBrokerFullChain(srv.brokerAuthService, logger, variant,
					http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
				req := signReq(http.MethodGet, constraintAuditAuthTestPath, map[string]string{
					HeaderOnBehalfOf: rejection.value(t, s),
				})
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)

				assertConstraintAuditNotFound(t, rec)
				if called {
					t.Fatal("rejected on-behalf-of identity invoked the downstream handler")
				}
				if strings.Contains(rec.Body.String(), "scheme") || strings.Contains(rec.Body.String(), "missing-constraint-audit") || strings.Contains(rec.Body.String(), "suspended") {
					t.Errorf("normalized response leaked OBO rejection detail: %s", rec.Body.String())
				}
				if variant.auditable && len(logger.brokerEvents) != 0 {
					t.Errorf("OBO rejection changed existing audit behavior: events = %#v", logger.brokerEvents)
				}
			})
		}
	}
}

func TestConstraintAuditBrokerAuth_SuccessPreservesContextWriterAndDownstreamResponse(t *testing.T) {
	for _, variant := range constraintAuditBrokerMiddlewareVariants() {
		for _, downstreamStatus := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
			t.Run(variant.name+"/"+http.StatusText(downstreamStatus), func(t *testing.T) {
				srv, s := testServer(t)
				brokerID, signReq := setupSignedBrokerRequest(t, srv.brokerAuthService, s)
				user := &store.User{
					ID: tid("constraint-audit-obo-user"), Email: "constraint-audit-obo-user@example.com",
					DisplayName: "Constraint Audit User", Role: "member", Status: store.UserStatusActive,
				}
				if err := s.CreateUser(context.Background(), user); err != nil {
					t.Fatalf("CreateUser() error = %v", err)
				}

				logger := &mockAuditLogger{}
				called := false
				var rec *httptest.ResponseRecorder
				next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					called = true
					if w != rec {
						t.Errorf("downstream writer = %T %p, want original recorder %p", w, w, rec)
					}
					identity := GetIdentityFromContext(r.Context())
					if identity == nil || identity.ID() != user.ID || identity.Type() != "user" {
						t.Errorf("effective identity = %#v, want user %q", identity, user.ID)
					}
					broker := GetBrokerIdentityFromContext(r.Context())
					credential := GetCredentialContextFromContext(r.Context())
					obo, ok := BrokerOnBehalfOfFromContext(r.Context())
					if broker == nil || broker.ID() != brokerID || credential.Kind != CredentialKindBroker || credential.ID != brokerID || !ok || obo.BrokerID != brokerID {
						t.Errorf("broker context = broker:%#v credential:%#v obo:%#v/%v", broker, credential, obo, ok)
					}
					writeError(w, downstreamStatus, "downstream_status", "downstream response", nil)
				})
				handler := constraintAuditBrokerFullChain(srv.brokerAuthService, logger, variant, next)
				req := signReq(http.MethodGet, constraintAuditAuthTestPath, map[string]string{
					HeaderOnBehalfOf: "user:" + user.Email,
				})
				rec = httptest.NewRecorder()
				handler.ServeHTTP(rec, req)

				if !called {
					t.Fatal("valid broker/OBO authentication did not invoke downstream handler")
				}
				if rec.Code != downstreamStatus || !strings.Contains(rec.Body.String(), "downstream response") {
					t.Errorf("downstream response = %d %s, want unchanged %d", rec.Code, rec.Body.String(), downstreamStatus)
				}
				if variant.auditable {
					if len(logger.brokerEvents) != 1 || logger.brokerEvents[0].EventType != BrokerAuthEventAuthSuccess || !logger.brokerEvents[0].Success {
						t.Fatalf("auditable success events = %#v, want one auth success", logger.brokerEvents)
					}
				}
			})
		}
	}
}

func TestConstraintAuditBrokerAuth_NeighborRoutesRetainBrokerFailure(t *testing.T) {
	for _, variant := range constraintAuditBrokerMiddlewareVariants() {
		for _, request := range []struct {
			name   string
			method string
			path   string
		}{
			{name: "non-GET", method: http.MethodPost, path: constraintAuditAuthTestPath},
			{name: "constraint detail", method: http.MethodGet, path: "/api/v1/admin/access-constraints/constraint-2405"},
		} {
			t.Run(variant.name+"/"+request.name, func(t *testing.T) {
				srv, s := testServer(t)
				_, signReq := setupSignedBrokerRequest(t, srv.brokerAuthService, s)
				called := false
				handler := constraintAuditBrokerFullChain(srv.brokerAuthService, &mockAuditLogger{}, variant,
					http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
				req := signReq(request.method, request.path, nil)
				req.Header.Set(HeaderSignature, strings.Repeat("0", 64))
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)

				if rec.Code != http.StatusUnauthorized {
					t.Errorf("status = %d, want existing broker 401; body: %s", rec.Code, rec.Body.String())
				}
				if called {
					t.Fatal("forged broker signature invoked downstream handler")
				}
			})
		}
	}
}
