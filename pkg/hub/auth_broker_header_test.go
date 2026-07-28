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
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
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

// TestUnifiedAuthMiddleware_BrokerHeaderForgedSignature covers the same header
// carrying a plausible-looking but bogus signature — the shape a real attacker
// would send. It must be rejected whether or not broker auth is configured.
func TestUnifiedAuthMiddleware_BrokerHeaderForgedSignature(t *testing.T) {
	svc, s := setupTestBrokerAuthService(t)
	brokerID := createTestBrokerWithSecret(t, s, []byte("correct-secret-key-32-bytes-ok!"))

	for _, tc := range []struct {
		name string
		svc  *BrokerAuthService
	}{
		{"broker auth configured", svc},
		{"broker auth unavailable", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reached bool
			var gotIdentity Identity
			handler := brokerHeaderTestChain(tc.svc, &reached, &gotIdentity)

			req := httptest.NewRequest(http.MethodPost,
				"/api/v1/runtime-brokers/test-host/heartbeat", nil)
			req.Header.Set(HeaderBrokerID, brokerID)
			req.Header.Set(HeaderTimestamp, strconv.FormatInt(time.Now().Unix(), 10))
			req.Header.Set(HeaderNonce, "forged-nonce")
			req.Header.Set(HeaderSignature, "not-a-real-signature")
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if reached {
				t.Error("forged broker signature reached the handler")
			}
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("expected status %d, got %d (body: %s)",
					http.StatusUnauthorized, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestUnifiedAuthMiddleware_BrokerHeaderValidSignature is the guard against
// mistaking the fix for a regression in broker dispatch: when broker auth *is*
// configured, a correctly signed request still reaches the handler with a broker
// identity in context.
func TestUnifiedAuthMiddleware_BrokerHeaderValidSignature(t *testing.T) {
	svc, s := setupTestBrokerAuthService(t)
	secretKey := []byte("test-secret-key-32-bytes-long!!")
	brokerID := createTestBrokerWithSecret(t, s, secretKey)

	var reached bool
	var gotIdentity Identity
	handler := brokerHeaderTestChain(svc, &reached, &gotIdentity)

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/runtime-brokers/test-host/heartbeat", nil)
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	nonce := "valid-nonce-" + uuid.New().String()
	req.Header.Set(HeaderBrokerID, brokerID)
	req.Header.Set(HeaderTimestamp, timestamp)
	req.Header.Set(HeaderNonce, nonce)

	mac := hmac.New(sha256.New, secretKey)
	mac.Write(svc.buildCanonicalString(req, timestamp, nonce))
	req.Header.Set(HeaderSignature, base64.StdEncoding.EncodeToString(mac.Sum(nil)))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !reached {
		t.Fatalf("validly signed broker request was rejected: status %d (body: %s)",
			rec.Code, rec.Body.String())
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, rec.Code)
	}
	if gotIdentity == nil {
		t.Fatal("expected a broker identity in context, got nil")
	}
	if gotIdentity.Type() != "broker" {
		t.Errorf("expected identity type %q, got %q", "broker", gotIdentity.Type())
	}
	if gotIdentity.ID() != brokerID {
		t.Errorf("expected identity ID %q, got %q", brokerID, gotIdentity.ID())
	}
}

// createTestBrokerWithSecret registers a runtime broker with an active HMAC
// secret and returns its ID.
func createTestBrokerWithSecret(t *testing.T, s store.Store, secretKey []byte) string {
	t.Helper()
	ctx := context.Background()

	brokerID := uuid.New().String()
	broker := &store.RuntimeBroker{
		ID:      brokerID,
		Name:    "test-host",
		Slug:    "test-host",
		Status:  store.BrokerStatusOnline,
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	secret := &store.BrokerSecret{
		BrokerID:  brokerID,
		SecretKey: secretKey,
		Algorithm: store.BrokerSecretAlgorithmHMACSHA256,
		Status:    store.BrokerSecretStatusActive,
	}
	if err := s.CreateBrokerSecret(ctx, secret); err != nil {
		t.Fatalf("failed to create broker secret: %v", err)
	}
	return brokerID
}
