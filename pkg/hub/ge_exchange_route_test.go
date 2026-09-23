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
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// ---------------------------------------------------------------------------
// Full-middleware GE exchange route tests.
//
// These tests exercise the exchange endpoint through Server.Handler() — the
// production middleware chain including UnifiedAuthMiddleware — to verify that
// the endpoint is reachable WITHOUT a pre-existing Hub token.
//
// The handler-level rate limit, body limit, and credential validation remain
// as defense-in-depth and are verified here.
// ---------------------------------------------------------------------------

// countingRouteValidator tracks call counts for zero-call assertions in
// full-middleware tests.
type countingRouteValidator struct {
	fakeGoogleValidator
	idTokenCalls     atomic.Int64
	accessTokenCalls atomic.Int64
}

func (v *countingRouteValidator) ValidateIDToken(ctx context.Context, token string, clientIDs []string) (*ValidatedGoogleIdentity, error) {
	v.idTokenCalls.Add(1)
	return v.fakeGoogleValidator.ValidateIDToken(ctx, token, clientIDs)
}

func (v *countingRouteValidator) ValidateAccessToken(ctx context.Context, token string, clientIDs []string) (*ValidatedGoogleIdentity, error) {
	v.accessTokenCalls.Add(1)
	return v.fakeGoogleValidator.ValidateAccessToken(ctx, token, clientIDs)
}

func (v *countingRouteValidator) totalCalls() int64 {
	return v.idTokenCalls.Load() + v.accessTokenCalls.Load()
}

// newGERouteTestServer creates a Server with the full production middleware
// chain and a configured GE exchange service using a fake Google validator.
// The server uses an in-memory SQLite store and dev auth for protected
// endpoint testing.
func newGERouteTestServer(t *testing.T, validator GoogleCredentialValidator) *Server {
	t.Helper()

	s, err := newTestStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}

	cfg := DefaultServerConfig()
	cfg.DevAuthToken = "test-dev-token-route"
	cfg.GEGoogleExchange = GEGoogleExchangeConfig{
		Enabled:          true,
		AllowedClientIDs: []string{"test-client-id.apps.googleusercontent.com"},
		TokenTTL:         DefaultGETokenTTL,
	}

	srv, err := New(cfg, s)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	// Replace the production Google validator with the test double.
	srv.geExchangeService.validator = validator

	return srv
}

// newGERouteTestServerDisabled creates a Server with GE exchange NOT configured.
func newGERouteTestServerDisabled(t *testing.T) *Server {
	t.Helper()

	s, err := newTestStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}

	cfg := DefaultServerConfig()
	cfg.DevAuthToken = "test-dev-token-route"
	// GEGoogleExchange is zero-value (not enabled).

	srv, err := New(cfg, s)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	return srv
}

// ---------------------------------------------------------------------------
// Regression test: exchange endpoint reachable without Hub auth token.
// This is the RED test that must fail before the fix is applied.
// ---------------------------------------------------------------------------

func TestGEExchange_Route_ReachableWithoutHubToken(t *testing.T) {
	identity := validGmailIdentity()
	validator := &fakeGoogleValidator{idTokenResult: identity}
	srv := newGERouteTestServer(t, validator)

	// Send a valid exchange request WITHOUT any Authorization header.
	body := `{"credential":"test-id-token","credentialType":"id_token"}`
	req := httptest.NewRequest("POST", "/api/v1/auth/integrations/google/exchange",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.0.2.1:12345"

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	// The exchange endpoint is pre-auth: it must NOT require a Hub token.
	// If the middleware blocks it, we get 401 with "missing authorization header"
	// or "authentication required" — the outer middleware rejects before the
	// exchange handler ever runs.
	if rec.Code == http.StatusUnauthorized {
		body := rec.Body.String()
		if strings.Contains(body, "missing authorization header") ||
			strings.Contains(body, "authentication required") {
			t.Fatal("BUG: exchange endpoint blocked by outer auth middleware — " +
				"the path must be added to isUnauthenticatedEndpoint()")
		}
		t.Fatalf("unexpected 401: %s", body)
	}

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp ExchangeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.AccessToken == "" {
		t.Error("expected non-empty access token")
	}
	if resp.User == nil || resp.User.Email != "user@gmail.com" {
		t.Error("expected user in response")
	}
}

// ---------------------------------------------------------------------------
// GREEN regression cases: full production middleware.
// ---------------------------------------------------------------------------

func TestGEExchange_Route_InvalidCredentialFailsClosed(t *testing.T) {
	validator := &fakeGoogleValidator{
		idTokenErr: ErrGoogleUntrustedAudience,
	}
	srv := newGERouteTestServer(t, validator)

	body := `{"credential":"bad-token","credentialType":"id_token"}`
	req := httptest.NewRequest("POST", "/api/v1/auth/integrations/google/exchange",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.0.2.2:12345"

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for invalid credential, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify this is the exchange handler's rejection, not the outer middleware.
	var errResp struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err == nil {
		if errResp.Error.Code == "unauthorized" &&
			strings.Contains(errResp.Error.Message, "missing authorization") {
			t.Fatal("invalid credential returned outer middleware 'unauthorized' instead of exchange-level rejection")
		}
	}
}

func TestGEExchange_Route_MissingCredentialFailsClosed(t *testing.T) {
	validator := &fakeGoogleValidator{idTokenResult: validGmailIdentity()}
	srv := newGERouteTestServer(t, validator)

	// Send with empty credential.
	body := `{"credential":"","credentialType":"id_token"}`
	req := httptest.NewRequest("POST", "/api/v1/auth/integrations/google/exchange",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.0.2.3:12345"

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing credential, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestGEExchange_Route_ProtectedEndpointStillRequiresAuth(t *testing.T) {
	validator := &fakeGoogleValidator{idTokenResult: validGmailIdentity()}
	srv := newGERouteTestServer(t, validator)

	// Hit a protected endpoint WITHOUT auth — should get 401.
	req := httptest.NewRequest("GET", "/api/v1/agents", nil)
	req.RemoteAddr = "192.0.2.4:12345"

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("protected endpoint without auth should be 401, got %d: %s",
			rec.Code, rec.Body.String())
	}
}

func TestGEExchange_Route_DisabledExchangeRemainsClosedDespiteRouteExemption(t *testing.T) {
	srv := newGERouteTestServerDisabled(t)

	body := `{"credential":"test-token","credentialType":"id_token"}`
	req := httptest.NewRequest("POST", "/api/v1/auth/integrations/google/exchange",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.0.2.5:12345"

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	// When GE exchange is not configured, the handler returns 401 "not_configured".
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("disabled exchange should return 401, got %d: %s", rec.Code, rec.Body.String())
	}

	// The error response is {"error":{"code":"not_configured","message":"..."}}.
	var errResp struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("failed to decode error: %v", err)
	}
	if errResp.Error.Code != "not_configured" {
		t.Errorf("expected error code 'not_configured', got %q", errResp.Error.Code)
	}
}

func TestGEExchange_Route_RateLimitStillOperates(t *testing.T) {
	validator := &countingRouteValidator{
		fakeGoogleValidator: fakeGoogleValidator{idTokenResult: validGmailIdentity()},
	}
	srv := newGERouteTestServer(t, validator)

	// Override the rate limiter with a very small burst.
	srv.geExchangeRateLimiter.mu.Lock()
	srv.geExchangeRateLimiter.burst = 2
	srv.geExchangeRateLimiter.mu.Unlock()

	body := `{"credential":"test-token","credentialType":"id_token"}`

	// Consume the burst.
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("POST", "/api/v1/auth/integrations/google/exchange",
			bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "192.0.2.10:12345"

		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("burst request %d: expected 200, got %d: %s", i+1, rec.Code, rec.Body.String())
		}
	}

	// Next request should be rate-limited.
	callsBefore := validator.totalCalls()

	req := httptest.NewRequest("POST", "/api/v1/auth/integrations/google/exchange",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.0.2.10:12345"

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 after burst, got %d: %s", rec.Code, rec.Body.String())
	}
	if retryAfter := rec.Header().Get("Retry-After"); retryAfter == "" {
		t.Error("Retry-After header missing on 429")
	}

	// Zero Google calls on rate-limited request.
	if diff := validator.totalCalls() - callsBefore; diff != 0 {
		t.Errorf("rate-limited request made %d Google calls; want 0", diff)
	}
}

func TestGEExchange_Route_BodyLimitStillOperates(t *testing.T) {
	validator := &countingRouteValidator{
		fakeGoogleValidator: fakeGoogleValidator{idTokenResult: validGmailIdentity()},
	}
	srv := newGERouteTestServer(t, validator)

	// Send oversize body through the full middleware chain.
	oversizeBody := `{"credential":"` + strings.Repeat("X", geExchangeMaxBodyBytes+100) + `","credentialType":"id_token"}`
	req := httptest.NewRequest("POST", "/api/v1/auth/integrations/google/exchange",
		bytes.NewBufferString(oversizeBody))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.0.2.11:12345"

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 for oversize body, got %d: %s", rec.Code, rec.Body.String())
	}

	if calls := validator.totalCalls(); calls != 0 {
		t.Errorf("oversize request made %d Google calls; want 0", calls)
	}
}

// ---------------------------------------------------------------------------
// R2/R3 — the GE exchange endpoint and the external-bearer path must share
// exactly one Google validator and one GoogleIdentityResolver instance
// (design §4.4), so both mechanisms produce identical decisions during the
// exchange-to-external-bearer soak. This exercises the actual production
// wiring in server.go's New, not a test double.
// ---------------------------------------------------------------------------

func TestGEExchange_Route_SharesValidatorAndResolverWithExternalBearer(t *testing.T) {
	s, err := newTestStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}

	cfg := DefaultServerConfig()
	cfg.GEGoogleExchange = GEGoogleExchangeConfig{
		Enabled:          true,
		AllowedClientIDs: []string{"test-client-id.apps.googleusercontent.com"},
		TokenTTL:         DefaultGETokenTTL,
	}

	srv, err := New(cfg, s)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	if srv.geExchangeService == nil {
		t.Fatal("expected GE exchange service to be configured")
	}
	if srv.authConfig.GoogleValidator == nil {
		t.Fatal("expected authConfig.GoogleValidator to be set")
	}
	if srv.authConfig.GoogleResolver == nil {
		t.Fatal("expected authConfig.GoogleResolver to be set")
	}
	if srv.geExchangeService.validator != srv.authConfig.GoogleValidator {
		t.Error("GE exchange validator is not the same instance as the external-bearer path's validator (design §4.4 wiring)")
	}
	if srv.geExchangeService.resolver != srv.authConfig.GoogleResolver {
		t.Error("GE exchange resolver is not the same instance as the external-bearer path's resolver (design §4.4 wiring)")
	}
}

// TestGEExchange_Route_GoogleStackBuiltWithoutExchangeOrTrust proves O3: the
// Google validator/resolver stack is now built unconditionally in New, not
// gated on GEGoogleExchange or startup-time trust detection, so that Google
// trust added later via hot reload takes effect without a restart.
func TestGEExchange_Route_GoogleStackBuiltWithoutExchangeOrTrust(t *testing.T) {
	s, err := newTestStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}

	cfg := DefaultServerConfig()
	// Deliberately no GEGoogleExchange, no Federation config: neither Google
	// trust nor the exchange is configured.

	srv, err := New(cfg, s)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	if srv.geExchangeService != nil {
		t.Fatal("expected no GE exchange service when GEGoogleExchange is not configured")
	}
	if srv.authConfig.GoogleValidator == nil {
		t.Error("expected authConfig.GoogleValidator to be built unconditionally (O3)")
	}
	if srv.authConfig.GoogleResolver == nil {
		t.Error("expected authConfig.GoogleResolver to be built unconditionally (O3)")
	}
}
