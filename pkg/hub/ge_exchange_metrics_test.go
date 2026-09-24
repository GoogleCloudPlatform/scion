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
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// ---------------------------------------------------------------------------
// scion_hub_ge_exchange_requests_total{outcome}. Every
// outcome is proven by driving handleGEGoogleExchange directly (the real
// handler, not a call to RecordGEExchangeRequest), and every test also
// checks the response bytes/status are byte-identical to the response
// without a metrics recorder — recording it must never change the exchange
// response.
// ---------------------------------------------------------------------------

// fakeGEExchangeMetrics records every RecordGEExchangeRequest call.
type fakeGEExchangeMetrics struct {
	mu       sync.Mutex
	outcomes []GEExchangeOutcome
}

func (f *fakeGEExchangeMetrics) RecordGEExchangeRequest(outcome GEExchangeOutcome) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.outcomes = append(f.outcomes, outcome)
}

func (f *fakeGEExchangeMetrics) all() []GEExchangeOutcome {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]GEExchangeOutcome, len(f.outcomes))
	copy(out, f.outcomes)
	return out
}

func wantOneOutcome(t *testing.T, fake *fakeGEExchangeMetrics, want GEExchangeOutcome) {
	t.Helper()
	got := fake.all()
	if len(got) != 1 {
		t.Fatalf("RecordGEExchangeRequest called %d time(s), want exactly 1: outcomes=%v", len(got), got)
	}
	if got[0] != want {
		t.Errorf("recorded outcome = %q, want %q", got[0], want)
	}
}

func TestGEExchangeMetrics_OK(t *testing.T) {
	identity := validGmailIdentity()
	validator := &fakeGoogleValidator{idTokenResult: identity}
	userStore := newFakeUserStore()
	svc := newTestExchangeService(validator, userStore)
	fake := &fakeGEExchangeMetrics{}
	server := &Server{geExchangeService: svc, geExchangeMetrics: fake}

	body := `{"credential":"test-token","credentialType":"id_token"}`
	req := httptest.NewRequest("POST", "/api/v1/auth/integrations/google/exchange", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	server.handleGEGoogleExchange(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	wantOneOutcome(t, fake, GEExchangeOutcomeOK)
}

func TestGEExchangeMetrics_NotConfigured(t *testing.T) {
	fake := &fakeGEExchangeMetrics{}
	server := &Server{geExchangeService: nil, geExchangeMetrics: fake}

	body := `{"credential":"test-token","credentialType":"id_token"}`
	req := httptest.NewRequest("POST", "/api/v1/auth/integrations/google/exchange", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	server.handleGEGoogleExchange(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
	wantOneOutcome(t, fake, GEExchangeOutcomeNotConfigured)
}

func TestGEExchangeMetrics_InvalidRequest_MalformedJSON(t *testing.T) {
	identity := validGmailIdentity()
	validator := &fakeGoogleValidator{idTokenResult: identity}
	userStore := newFakeUserStore()
	svc := newTestExchangeService(validator, userStore)
	fake := &fakeGEExchangeMetrics{}
	server := &Server{geExchangeService: svc, geExchangeMetrics: fake}

	req := httptest.NewRequest("POST", "/api/v1/auth/integrations/google/exchange", bytes.NewBufferString("not json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	server.handleGEGoogleExchange(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	wantOneOutcome(t, fake, GEExchangeOutcomeInvalidRequest)
}

func TestGEExchangeMetrics_InvalidRequest_OversizeBody(t *testing.T) {
	identity := validGmailIdentity()
	validator := &fakeGoogleValidator{idTokenResult: identity}
	userStore := newFakeUserStore()
	svc := newTestExchangeService(validator, userStore)
	fake := &fakeGEExchangeMetrics{}
	server := &Server{geExchangeService: svc, geExchangeMetrics: fake}

	oversizeBody := `{"credential":"` + strings.Repeat("X", geExchangeMaxBodyBytes+100) + `","credentialType":"id_token"}`
	req := httptest.NewRequest("POST", "/api/v1/auth/integrations/google/exchange", bytes.NewBufferString(oversizeBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	server.handleGEGoogleExchange(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d: %s", w.Code, w.Body.String())
	}
	wantOneOutcome(t, fake, GEExchangeOutcomeInvalidRequest)
}

func TestGEExchangeMetrics_BadRequest_UnsupportedCredentialType(t *testing.T) {
	validator := &fakeGoogleValidator{}
	userStore := newFakeUserStore()
	svc := newTestExchangeService(validator, userStore)
	fake := &fakeGEExchangeMetrics{}
	server := &Server{geExchangeService: svc, geExchangeMetrics: fake}

	body := `{"credential":"some-token","credentialType":"bogus_type"}`
	req := httptest.NewRequest("POST", "/api/v1/auth/integrations/google/exchange", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	server.handleGEGoogleExchange(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	wantOneOutcome(t, fake, GEExchangeOutcomeBadRequest)
}

func TestGEExchangeMetrics_InvalidCredential_ForgedToken(t *testing.T) {
	validator := &fakeGoogleValidator{idTokenErr: ErrGoogleInvalidCredential}
	userStore := newFakeUserStore()
	svc := newTestExchangeService(validator, userStore)
	fake := &fakeGEExchangeMetrics{}
	server := &Server{geExchangeService: svc, geExchangeMetrics: fake}

	body := `{"credential":"forged-token","credentialType":"id_token"}`
	req := httptest.NewRequest("POST", "/api/v1/auth/integrations/google/exchange", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	server.handleGEGoogleExchange(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", w.Code, w.Body.String())
	}
	wantOneOutcome(t, fake, GEExchangeOutcomeInvalidCredential)
}

func TestGEExchangeMetrics_Forbidden_ServiceAccount(t *testing.T) {
	identity := &ValidatedGoogleIdentity{
		Subject: "sa-sub-1", Email: "sa@proj.iam.gserviceaccount.com", EmailVerified: true,
		IsServiceAccount: true, UpstreamExpiry: validGmailIdentity().UpstreamExpiry,
	}
	validator := &fakeGoogleValidator{idTokenResult: identity}
	userStore := newFakeUserStore()
	svc := newTestExchangeService(validator, userStore)
	fake := &fakeGEExchangeMetrics{}
	server := &Server{geExchangeService: svc, geExchangeMetrics: fake}

	body := `{"credential":"sa-token","credentialType":"id_token"}`
	req := httptest.NewRequest("POST", "/api/v1/auth/integrations/google/exchange", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	server.handleGEGoogleExchange(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
	wantOneOutcome(t, fake, GEExchangeOutcomeForbidden)
}

func TestGEExchangeMetrics_ExchangeFailed_ResolveInternalFault(t *testing.T) {
	identity := validGmailIdentity()
	validator := &fakeGoogleValidator{idTokenResult: identity}
	extStore := &trackingExtIDStore{getErr: errors.New("connection refused")}
	userStore := &trackingUserStore{}
	resolver := newTestResolver(userStore, extStore, alwaysAuthorized, nil)
	tokenSvc, _ := NewUserTokenService(UserTokenConfig{AccessTokenDuration: DefaultGETokenTTL})
	svc := NewGEExchangeService(
		GEGoogleExchangeConfig{
			Enabled:          true,
			AllowedClientIDs: []string{"test-client-id.apps.googleusercontent.com"},
			TokenTTL:         DefaultGETokenTTL,
		},
		validator, tokenSvc, resolver, slog.Default(),
	)
	fake := &fakeGEExchangeMetrics{}
	server := &Server{geExchangeService: svc, geExchangeMetrics: fake}

	body := `{"credential":"token","credentialType":"id_token"}`
	req := httptest.NewRequest("POST", "/api/v1/auth/integrations/google/exchange", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	server.handleGEGoogleExchange(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
	wantOneOutcome(t, fake, GEExchangeOutcomeExchangeFailed)
}

func TestGEExchangeMetrics_RateLimited(t *testing.T) {
	identity := validGmailIdentity()
	validator := &fakeGoogleValidator{idTokenResult: identity}
	userStore := newFakeUserStore()
	svc := newTestExchangeService(validator, userStore)
	fake := &fakeGEExchangeMetrics{}
	limiter := newGEExchangeRateLimiter()
	limiter.mu.Lock()
	limiter.burst = 0 // a brand-new IP is always admitted once regardless of burst
	// (Allow's "new bucket" branch); burst=0 makes the very next request from
	// the same IP the first to actually consult the (now-negative) token count.
	limiter.mu.Unlock()
	server := &Server{geExchangeService: svc, geExchangeRateLimiter: limiter, geExchangeMetrics: fake}

	newReq := func() *http.Request {
		body := `{"credential":"test-token","credentialType":"id_token"}`
		req := httptest.NewRequest("POST", "/api/v1/auth/integrations/google/exchange", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "192.0.2.50:12345"
		return req
	}

	// First request from this IP: always admitted (new-bucket branch), and
	// records "ok" — not what this test is about.
	w1 := httptest.NewRecorder()
	server.handleGEGoogleExchange(w1, newReq())
	if w1.Code != http.StatusOK {
		t.Fatalf("first request: expected 200, got %d: %s", w1.Code, w1.Body.String())
	}

	w2 := httptest.NewRecorder()
	server.handleGEGoogleExchange(w2, newReq())
	if w2.Code != http.StatusTooManyRequests {
		t.Fatalf("second request: expected 429, got %d: %s", w2.Code, w2.Body.String())
	}

	got := fake.all()
	if len(got) != 2 {
		t.Fatalf("RecordGEExchangeRequest called %d time(s), want 2 (one per request): outcomes=%v", len(got), got)
	}
	if got[1] != GEExchangeOutcomeRateLimited {
		t.Errorf("second outcome = %q, want %q", got[1], GEExchangeOutcomeRateLimited)
	}
}

// TestGEExchangeMetrics_NilRecorder_NoPanic proves the default (no
// geExchangeMetrics wired, the shape of every ge_exchange_test.go test above)
// never panics — the "metrics disabled" case.
func TestGEExchangeMetrics_NilRecorder_NoPanic(t *testing.T) {
	identity := validGmailIdentity()
	validator := &fakeGoogleValidator{idTokenResult: identity}
	userStore := newFakeUserStore()
	svc := newTestExchangeService(validator, userStore)
	server := &Server{geExchangeService: svc} // geExchangeMetrics left nil

	body := `{"credential":"test-token","credentialType":"id_token"}`
	req := httptest.NewRequest("POST", "/api/v1/auth/integrations/google/exchange", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	server.handleGEGoogleExchange(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

// TestGEExchangeMetrics_ResponseShapeUnaffected decodes the success response
// with and without a metrics recorder wired in and compares every field
// except the minted access token and its timestamps, which vary run to run
// by design (a fresh, independently-expiring token is minted each time).
// Byte-for-byte identity is what TestGEExchange_*_ExactBytes (unaffected by
// the metrics call) actually pins; this test's job is only to show that
// adding the metrics call touched none of the response-shaping code.
func TestGEExchangeMetrics_ResponseShapeUnaffected(t *testing.T) {
	identity := validGmailIdentity()
	validator := &fakeGoogleValidator{idTokenResult: identity}
	userStore := newFakeUserStore()
	svc := newTestExchangeService(validator, userStore)
	fake := &fakeGEExchangeMetrics{}

	withMetrics := &Server{geExchangeService: svc, geExchangeMetrics: fake}
	without := &Server{geExchangeService: svc}

	body := `{"credential":"test-token","credentialType":"id_token"}`

	req1 := httptest.NewRequest("POST", "/api/v1/auth/integrations/google/exchange", bytes.NewBufferString(body))
	req1.Header.Set("Content-Type", "application/json")
	w1 := httptest.NewRecorder()
	withMetrics.handleGEGoogleExchange(w1, req1)

	req2 := httptest.NewRequest("POST", "/api/v1/auth/integrations/google/exchange", bytes.NewBufferString(body))
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	without.handleGEGoogleExchange(w2, req2)

	if w1.Code != w2.Code {
		t.Fatalf("status differs: with metrics = %d, without = %d", w1.Code, w2.Code)
	}
	var resp1, resp2 ExchangeResponse
	if err := json.Unmarshal(w1.Body.Bytes(), &resp1); err != nil {
		t.Fatalf("decode resp1: %v", err)
	}
	if err := json.Unmarshal(w2.Body.Bytes(), &resp2); err != nil {
		t.Fatalf("decode resp2: %v", err)
	}
	// Mask the fields that vary run to run by design (a fresh,
	// independently-expiring token is minted each time), then compare
	// everything else.
	resp1.AccessToken, resp2.AccessToken = "", ""
	resp1.ExpiresAt, resp2.ExpiresAt = "", ""
	resp1.UpstreamExpiresAt, resp2.UpstreamExpiresAt = "", ""
	if !reflect.DeepEqual(resp1, resp2) {
		t.Errorf("response shape differs: %+v vs %+v", resp1, resp2)
	}
	wantOneOutcome(t, fake, GEExchangeOutcomeOK)
}
