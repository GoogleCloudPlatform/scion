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
	"testing"
)

// Regression tests for the broker-header short-circuit in UnifiedAuthMiddleware
// (issue #591, design §8.1).
//
// The pre-fix middleware short-circuited on the mere *presence* of
// X-Scion-Broker-ID: it set an auth-type label, never an identity, and called
// next.ServeHTTP. BrokerAuthMiddleware — the only thing that validates the HMAC
// signature — is installed only when brokerAuthService is non-nil
// (Server.applyMiddleware), so with no broker auth service a bare
//
//	curl -H "X-Scion-Broker-ID: anything"
//
// reached the handlers fully unauthenticated. Fail closed instead.

// brokerHeaderTestChain builds the relevant slice of the real middleware chain:
// UnifiedAuthMiddleware followed by BrokerAuthMiddleware, installed under the
// same condition applyMiddleware uses (svc != nil). It reports whether the
// terminal handler ran and what identity, if any, it saw.
func brokerHeaderTestChain(svc *BrokerAuthService, reached *bool, gotIdentity *Identity) http.Handler {
	var h http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*reached = true
		*gotIdentity = GetIdentityFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	// Mirrors Server.applyMiddleware: broker auth middleware is only installed
	// when the service exists.
	if svc != nil {
		h = BrokerAuthMiddleware(svc)(h)
	}
	return UnifiedAuthMiddleware(AuthConfig{
		Mode:          "production",
		BrokerAuthSvc: svc,
	})(h)
}

// TestUnifiedAuthMiddleware_BrokerHeaderWithoutBrokerAuth asserts the fix: when
// no broker auth service is configured, a request carrying X-Scion-Broker-ID and
// no valid signature is rejected, not passed through.
func TestUnifiedAuthMiddleware_BrokerHeaderWithoutBrokerAuth(t *testing.T) {
	tests := []struct {
		name string
		svc  *BrokerAuthService
	}{
		{
			// The zero-valued ServerConfig case: BrokerAuthConfig.Enabled is
			// false, so NewServer never constructs the service and
			// BrokerAuthMiddleware is never installed.
			name: "no broker auth service",
			svc:  nil,
		},
		{
			// The service exists but is disabled, so BrokerAuthMiddleware
			// no-ops on every request. Same hole, different shape.
			name: "broker auth service disabled",
			svc: func() *BrokerAuthService {
				cfg := DefaultBrokerAuthConfig()
				cfg.Enabled = false
				return NewBrokerAuthService(cfg, nil)
			}(),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var reached bool
			var gotIdentity Identity
			handler := brokerHeaderTestChain(tc.svc, &reached, &gotIdentity)

			req := httptest.NewRequest(http.MethodPost,
				"/api/v1/runtime-brokers/test-host/heartbeat", nil)
			req.Header.Set(HeaderBrokerID, "anything")
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if reached {
				t.Error("request with X-Scion-Broker-ID and no broker auth reached the handler; " +
					"it must be rejected (#591)")
			}
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("expected status %d, got %d (body: %s)",
					http.StatusUnauthorized, rec.Code, rec.Body.String())
			}
		})
	}
}
