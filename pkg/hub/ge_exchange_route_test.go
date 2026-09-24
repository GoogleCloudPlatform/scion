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

	"go.opentelemetry.io/otel/sdk/metric"
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
// The GE exchange endpoint and the external-bearer path must share
// exactly one GoogleIdentityResolver instance, so both
// mechanisms produce identical resolution/provisioning/suspension decisions
// during the exchange-to-external-bearer soak. This exercises the actual
// production wiring in server.go's New, not a test double.
//
// For the validator half, the external-bearer path uses a caching decorator
// (re-validates on every request, unlike the exchange endpoint, so it
// benefits from a cache),
// but it wraps the *same base validator instance* the exchange uses, so the
// exchange's own behaviour and latency are unaffected (server.go's New has
// the full rationale). So the assertion here is: same base validator
// instance underneath, not same top-level GoogleValidator value.
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
	// The external-bearer rate limiter must be wired
	// both onto authConfig (what authenticateExternalBearer consults) and
	// onto Server (so Start/StartBackgroundServices can run its cleanup
	// goroutine — see TestServer_ExternalBearerRateLimiter_CleanupRunsInBackground).
	// Deleting either wiring line in server.go's New must fail this test.
	if srv.authConfig.ExternalBearerLimiter == nil {
		t.Fatal("expected authConfig.ExternalBearerLimiter to be set")
	}
	if srv.externalBearerRateLimiter == nil {
		t.Fatal("expected srv.externalBearerRateLimiter to be set")
	}
	if srv.authConfig.ExternalBearerLimiter != srv.externalBearerRateLimiter {
		t.Error("authConfig.ExternalBearerLimiter is not the same instance as srv.externalBearerRateLimiter")
	}
	if srv.geExchangeService.resolver != srv.authConfig.GoogleResolver {
		t.Error("GE exchange resolver is not the same instance as the external-bearer path's resolver")
	}

	cachingValidator, ok := srv.authConfig.GoogleValidator.(*cachingGoogleCredentialValidator)
	if !ok {
		t.Fatalf("expected authConfig.GoogleValidator to be a *cachingGoogleCredentialValidator, got %T", srv.authConfig.GoogleValidator)
	}
	if cachingValidator.base != srv.geExchangeService.validator {
		t.Error("the external-bearer path's caching decorator does not wrap the same base validator instance the GE exchange uses")
	}
}

// TestGEExchange_Route_GoogleStackBuiltWithoutExchangeOrTrust proves the
// Google validator/resolver stack is built unconditionally in New, not
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
		t.Error("expected authConfig.GoogleValidator to be built unconditionally")
	}
	if srv.authConfig.GoogleResolver == nil {
		t.Error("expected authConfig.GoogleResolver to be built unconditionally")
	}
}

// TestGEExchange_Route_ProductionResolverHonoursAdminEmails: other tests
// inject a hand-written stub roleFor into
// NewGoogleIdentityResolver directly, proving only that the resolver *uses*
// roleFor — not that server.go's real closure (func(ctx, email) string {
// return srv.getUserRole(ctx, email, "", "") }) actually honours AdminEmails
// in production. This resolves against the actual resolver New() builds.
func TestGEExchange_Route_ProductionResolverHonoursAdminEmails(t *testing.T) {
	s, err := newTestStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}

	cfg := DefaultServerConfig()
	cfg.AdminEmails = []string{"admin@gmail.com"}

	srv, err := New(cfg, s)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	if srv.authConfig.GoogleResolver == nil {
		t.Fatal("expected authConfig.GoogleResolver to be set")
	}

	adminIdentity := validGmailIdentity()
	adminIdentity.Subject = "admin-sub-1"
	adminIdentity.Email = "admin@gmail.com"
	adminUser, err := srv.authConfig.GoogleResolver.Resolve(context.Background(), adminIdentity, ResolvePolicy{})
	if err != nil {
		t.Fatalf("Resolve(admin@gmail.com): %v", err)
	}
	if adminUser.Role != "admin" {
		t.Errorf("admin@gmail.com role = %q, want %q (production roleFor must honour AdminEmails)", adminUser.Role, "admin")
	}

	memberIdentity := validGmailIdentity()
	memberIdentity.Subject = "member-sub-1"
	memberIdentity.Email = "someone-else@gmail.com"
	memberUser, err := srv.authConfig.GoogleResolver.Resolve(context.Background(), memberIdentity, ResolvePolicy{})
	if err != nil {
		t.Fatalf("Resolve(someone-else@gmail.com): %v", err)
	}
	if memberUser.Role != "member" {
		t.Errorf("someone-else@gmail.com role = %q, want %q", memberUser.Role, "member")
	}
}

// ---------------------------------------------------------------------------
// The three Set*Metrics setters (server.go) actually reach the running
// request path they are meant to wire into. Every other metrics test in this
// package builds its own AuthConfig by hand and calls attachExternalBearerMetrics/
// WithCacheMetrics directly, bypassing New() -> Handler() entirely — this is
// the only test that goes through that real wiring.
//
// Handler() (server.go) is s.applyMiddleware(s.mux), and it re-reads
// s.authConfig fresh on every call — it is Start() or Handler(), not
// registerRoutes/New(), that captures authConfig by value into
// UnifiedAuthMiddleware's closure, when either calls applyMiddleware to
// build a request-serving handler. cmd/server_foreground.go calls the three
// setters before Start() in Hub-only mode; in combined mode it instead calls
// Handler() once from initWebServer to mount the Hub API into the Web
// server, so that call is the one that captures authConfig there. This test
// captures Handler() once, before calling the setters, to stand in for
// whichever of those a real deployment hits first: only with the handler
// built first do the setters have to reach an *already-captured* cfg, which
// is what actually exercises AuthConfig.ExternalBearerMetrics's
// *atomic.Pointer design and would catch SetExternalBearerMetrics ever
// replacing the box instead of storing into it.
// ---------------------------------------------------------------------------

func TestServer_MetricsSettersReachRunningHandler(t *testing.T) {
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
	// Deliberately no Federation config: the external-bearer request below
	// exercises the not_applicable outcome, which needs no Google trust and
	// no network access — this test is about whether the setter wiring
	// reaches the request path, not about a specific outcome.

	srv, err := New(cfg, s)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	srv.geExchangeService.validator = &fakeGoogleValidator{idTokenResult: validGmailIdentity()}

	handler := srv.Handler()

	extBearerFake := &fakeExternalBearerMetrics{}
	cacheFake := &fakeCacheMetrics{}
	exchangeFake := &fakeGEExchangeMetrics{}
	srv.SetExternalBearerMetrics(extBearerFake)
	srv.SetGoogleValidatorCacheMetrics(cacheFake)
	srv.SetGEExchangeMetrics(exchangeFake)

	// (a) an external-bearer request through the already-captured handler.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	req.Header.Set("Authorization", "Bearer not-a-hub-token")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if got := len(extBearerFake.allCalls()); got != 1 {
		t.Errorf("external-bearer fake saw %d call(s), want 1 (SetExternalBearerMetrics must publish into the same box the already-built handler observes)", got)
	}

	// (b) a POST to the exchange endpoint through the same handler.
	body := `{"credential":"test-token","credentialType":"id_token"}`
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/auth/integrations/google/exchange", bytes.NewBufferString(body))
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)

	if got := len(exchangeFake.all()); got != 1 {
		t.Errorf("exchange fake saw %d call(s), want 1", got)
	}

	// The cache setter's wiring is exercised directly against the caching
	// decorator, without an HTTP round trip: proving a token was actually
	// validated would need a real (or externally reachable) Google
	// credential, which is unrelated to whether SetGoogleValidatorCacheMetrics
	// reached the decorator's metrics field at all.
	cachingValidator, ok := srv.authConfig.GoogleValidator.(*cachingGoogleCredentialValidator)
	if !ok {
		t.Fatalf("expected authConfig.GoogleValidator to be a *cachingGoogleCredentialValidator, got %T", srv.authConfig.GoogleValidator)
	}
	cachingValidator.recordCache(GoogleValidatorCacheHit)
	if got := len(cacheFake.all()); got != 1 {
		t.Errorf("cache fake saw %d call(s), want 1 (SetGoogleValidatorCacheMetrics must reach the decorator's metrics field)", got)
	}
}

// ---------------------------------------------------------------------------
// New()'s default wiring (no Set*Metrics call at all)
// actually reaches GET /metrics. Every other metrics test either builds its
// own AuthConfig (bypassing New() entirely) or calls the setters, which
// replace the default — this is the only test proving the default itself
// works end to end. Deleting any of the three lines that wire it in New()
// (ExternalBearerMetrics.Store(&defaultExtBearerMetrics), the cache
// decorator's SetMetrics(srv.externalBearerSnapshot), or
// srv.geExchangeMetrics = srv.externalBearerSnapshot) leaves this counter's
// /metrics section at zero forever on a Hub with no GCP export configured —
// exactly what the exchange-deletion soak gate would read as "traffic
// stopped".
// ---------------------------------------------------------------------------

func TestServer_DefaultMetricsWiring_RecordsWithoutSetters(t *testing.T) {
	s, err := newTestStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}

	cfg := DefaultServerConfig()
	cfg.DevAuthToken = testDevToken // /metrics is authenticated, though RBAC-public
	cfg.GEGoogleExchange = GEGoogleExchangeConfig{
		Enabled:          true,
		AllowedClientIDs: []string{"test-client-id.apps.googleusercontent.com"},
		TokenTTL:         DefaultGETokenTTL,
	}
	// Deliberately no Federation config: the /api/v1/auth/me request below
	// exercises not_applicable, which needs no Google trust and no network.

	srv, err := New(cfg, s)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	srv.geExchangeService.validator = &fakeGoogleValidator{idTokenResult: validGmailIdentity()}

	handler := srv.Handler()

	// (a) an exchange POST.
	body := `{"credential":"test-token","credentialType":"id_token"}`
	req1 := httptest.NewRequest(http.MethodPost, "/api/v1/auth/integrations/google/exchange", bytes.NewBufferString(body))
	req1.Header.Set("Content-Type", "application/json")
	w1 := httptest.NewRecorder()
	handler.ServeHTTP(w1, req1)
	if w1.Code != http.StatusOK {
		t.Fatalf("exchange POST: status = %d, want 200: %s", w1.Code, w1.Body.String())
	}

	// (b) a non-Hub bearer to /api/v1/auth/me: records not_applicable.
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	req2.Header.Set("Authorization", "Bearer not-a-hub-token")
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)

	// (c) a cache record via the decorator directly — no network needed.
	cachingValidator, ok := srv.authConfig.GoogleValidator.(*cachingGoogleCredentialValidator)
	if !ok {
		t.Fatalf("expected authConfig.GoogleValidator to be a *cachingGoogleCredentialValidator, got %T", srv.authConfig.GoogleValidator)
	}
	cachingValidator.recordCache(GoogleValidatorCacheHit)

	// GET /metrics through the same handler; no setter was ever called, so
	// this is entirely New()'s default wiring.
	req3 := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req3.Header.Set("Authorization", "Bearer "+testDevToken)
	w3 := httptest.NewRecorder()
	handler.ServeHTTP(w3, req3)
	if w3.Code != http.StatusOK {
		t.Fatalf("/metrics: status = %d, want 200: %s", w3.Code, w3.Body.String())
	}

	decoded := decodeExternalBearerSection(t, w3.Body.Bytes())
	if got := decoded.ExternalBearerTotal[string(ExternalBearerOutcomeNotApplicable)]; got != 1 {
		t.Errorf("externalBearerTotal.not_applicable = %d, want 1", got)
	}
	if got := decoded.GoogleValidatorCacheTotal[string(GoogleValidatorCacheHit)]; got != 1 {
		t.Errorf("googleValidatorCacheTotal.hit = %d, want 1", got)
	}
	var exchangeTotal int64
	for _, v := range decoded.GEExchangeRequestsTotal {
		exchangeTotal += v
	}
	if exchangeTotal != 1 {
		t.Errorf("geExchangeRequestsTotal summed = %d, want 1: %+v", exchangeTotal, decoded.GEExchangeRequestsTotal)
	}
	if got := decoded.GEExchangeRequestsTotal[string(GEExchangeOutcomeOK)]; got != 1 {
		t.Errorf("geExchangeRequestsTotal.ok = %d, want 1", got)
	}
}

// decodeExternalBearerSection decodes GET /metrics's "externalBearer" JSON
// section from a response body, failing the test if it is missing.
func decodeExternalBearerSection(t *testing.T, body []byte) *ExternalBearerMetricsSnapshot {
	t.Helper()
	var decoded struct {
		ExternalBearer *ExternalBearerMetricsSnapshot `json:"externalBearer"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode /metrics: %v: body=%s", err, body)
	}
	if decoded.ExternalBearer == nil {
		t.Fatal("externalBearer section missing from /metrics")
	}
	return decoded.ExternalBearer
}

// TestServer_DefaultMetricsWiring_OTelSetterStillMovesSnapshot extends the
// test above: builds the OTel recorder exactly as
// cmd/server_foreground.go does — NewOTelExternalBearerMetrics(mp,
// srv.ExternalBearerSnapshotMetrics()) followed by all three setters — and
// proves /metrics still moves by exactly 1. If the snapshot argument were
// ever dropped (passed as nil, a legal value), the section would freeze the
// moment GCP export is configured.
func TestServer_DefaultMetricsWiring_OTelSetterStillMovesSnapshot(t *testing.T) {
	s, err := newTestStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}

	cfg := DefaultServerConfig()
	cfg.DevAuthToken = testDevToken // /metrics is authenticated, though RBAC-public
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
	srv.geExchangeService.validator = &fakeGoogleValidator{idTokenResult: validGmailIdentity()}

	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	otelRec, err := NewOTelExternalBearerMetrics(mp, srv.ExternalBearerSnapshotMetrics())
	if err != nil {
		t.Fatalf("NewOTelExternalBearerMetrics: %v", err)
	}
	srv.SetExternalBearerMetrics(otelRec)
	srv.SetGoogleValidatorCacheMetrics(otelRec)
	srv.SetGEExchangeMetrics(otelRec)

	handler := srv.Handler()

	body := `{"credential":"test-token","credentialType":"id_token"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/integrations/google/exchange", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("exchange POST: status = %d, want 200: %s", w.Code, w.Body.String())
	}

	metricsReq := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	metricsReq.Header.Set("Authorization", "Bearer "+testDevToken)
	metricsW := httptest.NewRecorder()
	handler.ServeHTTP(metricsW, metricsReq)

	decoded := decodeExternalBearerSection(t, metricsW.Body.Bytes())
	if got := decoded.GEExchangeRequestsTotal[string(GEExchangeOutcomeOK)]; got != 1 {
		t.Errorf("geExchangeRequestsTotal.ok = %d, want 1 (the OTel recorder must dual-write into the same shared snapshot passed to it)", got)
	}
}
