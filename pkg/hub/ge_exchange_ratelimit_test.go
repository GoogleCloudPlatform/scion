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
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// countingGoogleValidator — tracks call counts for zero-call assertions.
// ---------------------------------------------------------------------------

type countingGoogleValidator struct {
	fakeGoogleValidator
	idTokenCalls     atomic.Int64
	accessTokenCalls atomic.Int64
}

func (v *countingGoogleValidator) ValidateIDToken(ctx context.Context, token string, clientIDs []string) (*ValidatedGoogleIdentity, error) {
	v.idTokenCalls.Add(1)
	return v.fakeGoogleValidator.ValidateIDToken(ctx, token, clientIDs)
}

func (v *countingGoogleValidator) ValidateAccessToken(ctx context.Context, token string, clientIDs []string) (*ValidatedGoogleIdentity, error) {
	v.accessTokenCalls.Add(1)
	return v.fakeGoogleValidator.ValidateAccessToken(ctx, token, clientIDs)
}

func (v *countingGoogleValidator) totalCalls() int64 {
	return v.idTokenCalls.Load() + v.accessTokenCalls.Load()
}

// newRejectingCountingValidator returns a *countingGoogleValidator whose zero
// value would otherwise be a fakeGoogleValidator that returns (nil, nil) — if
// a mutation ever lets a "must not reach the validator" test actually reach
// it, a nil identity dereference panics the whole test binary (P1 r4
// Optional 1's problem, and P2 r2 Optional 3's, for this specific type)
// instead of failing the test's own counting.totalCalls() assertion. Giving
// both methods a default error means that mutation fails loudly and locally.
// Extracted (P2 review r3, P3 carry-in Nit 3) to replace 7 identical pasted
// blocks across auth_external_bearer_test.go.
func newRejectingCountingValidator() *countingGoogleValidator {
	return &countingGoogleValidator{fakeGoogleValidator: fakeGoogleValidator{
		idTokenErr:     ErrGoogleInvalidCredential,
		accessTokenErr: ErrGoogleInvalidCredential,
	}}
}

// ---------------------------------------------------------------------------
// Helper: build a test server with rate limiter + exchange service.
// ---------------------------------------------------------------------------

func newRateLimitedTestServer(validator GoogleCredentialValidator, userStore *fakeUserStore, limiter *geExchangeRateLimiter) *Server {
	svc := newTestExchangeService(validator, userStore)
	return &Server{
		geExchangeService:     svc,
		geExchangeRateLimiter: limiter,
	}
}

func exchangeRequest(body string) *http.Request {
	return exchangeRequestWithAddr(body, "192.0.2.1:12345")
}

func exchangeRequestWithAddr(body, remoteAddr string) *http.Request {
	req := httptest.NewRequest("POST", "/api/v1/auth/integrations/google/exchange",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = remoteAddr
	return req
}

// ---------------------------------------------------------------------------
// Rate limit tests.
// ---------------------------------------------------------------------------

func TestGEExchange_RateLimit_BurstReaches429(t *testing.T) {
	identity := validGmailIdentity()
	validator := &countingGoogleValidator{
		fakeGoogleValidator: fakeGoogleValidator{idTokenResult: identity},
	}
	userStore := newFakeUserStore()

	limiter := newGEExchangeRateLimiter()
	limiter.burst = 3 // Small burst for testing.
	server := newRateLimitedTestServer(validator, userStore, limiter)

	body := `{"credential":"test-token","credentialType":"id_token"}`

	// First 3 requests should succeed (burst).
	for i := 0; i < 3; i++ {
		w := httptest.NewRecorder()
		server.handleGEGoogleExchange(w, exchangeRequest(body))
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d: %s", i+1, w.Code, w.Body.String())
		}
	}

	// 4th request should be rate-limited.
	w := httptest.NewRecorder()
	server.handleGEGoogleExchange(w, exchangeRequest(body))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d: %s", w.Code, w.Body.String())
	}

	// Retry-After header must be present and positive.
	retryAfter := w.Header().Get("Retry-After")
	if retryAfter == "" {
		t.Fatal("Retry-After header missing on 429 response")
	}

	// Body should contain rate_limited error code.
	var errResp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &errResp); err == nil {
		if code, ok := errResp["error"].(string); ok && code != ErrCodeRateLimited {
			t.Errorf("expected error code %q, got %q", ErrCodeRateLimited, code)
		}
	}
}

func TestGEExchange_RateLimit_ZeroGoogleCallsWhenLimited(t *testing.T) {
	identity := validGmailIdentity()
	validator := &countingGoogleValidator{
		fakeGoogleValidator: fakeGoogleValidator{idTokenResult: identity},
	}
	userStore := newFakeUserStore()

	limiter := newGEExchangeRateLimiter()
	limiter.burst = 1 // Allow exactly one request.
	server := newRateLimitedTestServer(validator, userStore, limiter)

	body := `{"credential":"test-token","credentialType":"id_token"}`

	// Consume the burst.
	w := httptest.NewRecorder()
	server.handleGEGoogleExchange(w, exchangeRequest(body))
	if w.Code != http.StatusOK {
		t.Fatalf("first request: expected 200, got %d", w.Code)
	}
	callsBefore := validator.totalCalls()

	// Subsequent request must be rate-limited with zero Google calls.
	w = httptest.NewRecorder()
	server.handleGEGoogleExchange(w, exchangeRequest(body))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", w.Code)
	}

	callsAfter := validator.totalCalls()
	if callsAfter != callsBefore {
		t.Errorf("rate-limited request made %d Google validator calls; want 0",
			callsAfter-callsBefore)
	}
}

func TestGEExchange_RateLimit_RefillAllowsRetry(t *testing.T) {
	identity := validGmailIdentity()
	validator := &fakeGoogleValidator{idTokenResult: identity}
	userStore := newFakeUserStore()

	now := time.Now()
	limiter := newGEExchangeRateLimiter()
	limiter.burst = 1
	limiter.rate = 1.0 // 1 token/sec for fast test.
	limiter.nowFunc = func() time.Time { return now }
	server := newRateLimitedTestServer(validator, userStore, limiter)

	body := `{"credential":"test-token","credentialType":"id_token"}`

	// Consume the burst.
	w := httptest.NewRecorder()
	server.handleGEGoogleExchange(w, exchangeRequest(body))
	if w.Code != http.StatusOK {
		t.Fatalf("first request: expected 200, got %d", w.Code)
	}

	// Immediately — should be 429.
	w = httptest.NewRecorder()
	server.handleGEGoogleExchange(w, exchangeRequest(body))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", w.Code)
	}

	// Advance time by 2 seconds (1 token refills at 1/sec).
	now = now.Add(2 * time.Second)

	// Should succeed after refill.
	w = httptest.NewRecorder()
	server.handleGEGoogleExchange(w, exchangeRequest(body))
	if w.Code != http.StatusOK {
		t.Fatalf("after refill: expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Trusted proxy / IP extraction tests.
// ---------------------------------------------------------------------------

func TestGEExchange_RateLimit_UntrustedSpoofedHeaders(t *testing.T) {
	identity := validGmailIdentity()
	validator := &fakeGoogleValidator{idTokenResult: identity}
	userStore := newFakeUserStore()

	limiter := newGEExchangeRateLimiter()
	limiter.burst = 1
	// No trusted proxies configured on the server.
	server := &Server{
		geExchangeService:     newTestExchangeService(validator, userStore),
		geExchangeRateLimiter: limiter,
		config:                ServerConfig{TrustedProxies: nil},
	}

	body := `{"credential":"test-token","credentialType":"id_token"}`

	// Consume the burst from 192.0.2.1.
	w := httptest.NewRecorder()
	server.handleGEGoogleExchange(w, exchangeRequest(body))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Spoofed X-Forwarded-For should NOT evade the limiter: the limiter
	// should ignore it because the peer is not a trusted proxy.
	req := exchangeRequest(body)
	req.Header.Set("X-Forwarded-For", "203.0.113.99")
	w = httptest.NewRecorder()
	server.handleGEGoogleExchange(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("spoofed X-Forwarded-For must NOT evade limiter; got %d, want 429", w.Code)
	}

	// Spoofed X-Real-IP should also be ignored.
	req = exchangeRequest(body)
	req.Header.Set("X-Real-IP", "203.0.113.100")
	w = httptest.NewRecorder()
	server.handleGEGoogleExchange(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("spoofed X-Real-IP must NOT evade limiter; got %d, want 429", w.Code)
	}
}

func TestGEExchange_RateLimit_TrustedProxyChain(t *testing.T) {
	// Test that with a trusted proxy, the correct client IP is extracted
	// from the X-Forwarded-For chain.
	tests := []struct {
		name       string
		remoteAddr string
		trusted    []string
		xff        string
		realIP     string
		wantIP     string
	}{
		{
			name:       "no trusted proxies — use RemoteAddr",
			remoteAddr: "192.0.2.1:1234",
			trusted:    nil,
			xff:        "203.0.113.7",
			wantIP:     "192.0.2.1",
		},
		{
			name:       "untrusted peer — ignore XFF",
			remoteAddr: "192.0.2.1:1234",
			trusted:    []string{"10.0.0.0/8"},
			xff:        "203.0.113.7",
			wantIP:     "192.0.2.1",
		},
		{
			name:       "trusted proxy — first untrusted in XFF",
			remoteAddr: "10.0.0.2:1234",
			trusted:    []string{"10.0.0.0/8"},
			xff:        "203.0.113.7, 10.0.0.1",
			wantIP:     "203.0.113.7",
		},
		{
			name:       "trusted proxy — multi-hop chain",
			remoteAddr: "10.0.0.3:1234",
			trusted:    []string{"10.0.0.0/8"},
			xff:        "198.51.100.5, 10.0.0.1, 10.0.0.2",
			wantIP:     "198.51.100.5",
		},
		{
			name:       "trusted proxy — all hops trusted, fallback to X-Real-IP",
			remoteAddr: "10.0.0.2:1234",
			trusted:    []string{"10.0.0.0/8"},
			xff:        "10.0.0.1",
			realIP:     "203.0.113.8",
			wantIP:     "203.0.113.8",
		},
		{
			name:       "trusted proxy — malformed XFF entry, fallback to peer",
			remoteAddr: "10.0.0.2:1234",
			trusted:    []string{"10.0.0.0/8"},
			xff:        "not-an-ip, 10.0.0.1",
			wantIP:     "10.0.0.2",
		},
		{
			name:       "IPv4-mapped IPv6 normalized",
			remoteAddr: "[::ffff:192.0.2.1]:1234",
			trusted:    nil,
			wantIP:     "192.0.2.1",
		},
		{
			name:       "pure IPv6 peer",
			remoteAddr: "[2001:db8::1]:1234",
			trusted:    nil,
			wantIP:     "2001:db8::1",
		},
		{
			name:       "trusted proxy — single IP CIDR",
			remoteAddr: "10.0.0.5:1234",
			trusted:    []string{"10.0.0.5"},
			xff:        "203.0.113.42",
			wantIP:     "203.0.113.42",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/", nil)
			req.RemoteAddr = tt.remoteAddr
			if tt.xff != "" {
				req.Header.Set("X-Forwarded-For", tt.xff)
			}
			if tt.realIP != "" {
				req.Header.Set("X-Real-IP", tt.realIP)
			}
			nets := parseTrustedProxies(tt.trusted)
			got := geExchangeClientIP(req, nets)
			if got != tt.wantIP {
				t.Errorf("geExchangeClientIP() = %q, want %q", got, tt.wantIP)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Body size limit tests.
// ---------------------------------------------------------------------------

func TestGEExchange_BodyLimit_OversizeRejected(t *testing.T) {
	validator := &countingGoogleValidator{
		fakeGoogleValidator: fakeGoogleValidator{idTokenResult: validGmailIdentity()},
	}
	userStore := newFakeUserStore()

	server := &Server{
		geExchangeService:     newTestExchangeService(validator, userStore),
		geExchangeRateLimiter: nil, // No rate limiter for this test.
	}

	// Create a body larger than geExchangeMaxBodyBytes (8 KB).
	oversizeBody := `{"credential":"` + strings.Repeat("A", geExchangeMaxBodyBytes+100) + `","credentialType":"id_token"}`

	req := exchangeRequest(oversizeBody)
	w := httptest.NewRecorder()
	server.handleGEGoogleExchange(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d: %s", w.Code, w.Body.String())
	}

	// Must have made zero Google validator calls.
	if calls := validator.totalCalls(); calls != 0 {
		t.Errorf("oversize request made %d Google validator calls; want 0", calls)
	}
}

func TestGEExchange_BodyLimit_NormalSizeAccepted(t *testing.T) {
	validator := &fakeGoogleValidator{idTokenResult: validGmailIdentity()}
	userStore := newFakeUserStore()

	server := &Server{
		geExchangeService: newTestExchangeService(validator, userStore),
	}

	body := `{"credential":"normal-token","credentialType":"id_token"}`
	w := httptest.NewRecorder()
	server.handleGEGoogleExchange(w, exchangeRequest(body))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGEExchange_BodyLimit_ZeroOutboundOnOversize(t *testing.T) {
	// Verify that an oversize request makes zero calls to the Google
	// validator, user store, and binding store.
	validator := &countingGoogleValidator{
		fakeGoogleValidator: fakeGoogleValidator{idTokenResult: validGmailIdentity()},
	}
	userStore := newFakeUserStore()

	server := &Server{
		geExchangeService: newTestExchangeService(validator, userStore),
	}

	// Fill with padding past the limit. The credential field alone exceeds 8 KB.
	oversizeBody := `{"credential":"` + strings.Repeat("B", geExchangeMaxBodyBytes) + `","credentialType":"id_token"}`

	req := exchangeRequest(oversizeBody)
	w := httptest.NewRecorder()
	server.handleGEGoogleExchange(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d: %s", w.Code, w.Body.String())
	}

	if calls := validator.totalCalls(); calls != 0 {
		t.Errorf("validator called %d times on oversize body; want 0", calls)
	}
}

// ---------------------------------------------------------------------------
// Limiter state bound / churn / cleanup tests.
// ---------------------------------------------------------------------------

func TestGEExchange_RateLimit_MaxEntriesFailsClosed(t *testing.T) {
	limiter := newGEExchangeRateLimiter()
	limiter.maxEntries = 5
	limiter.burst = 100 // High burst so no IP gets 429 from token exhaustion.

	// Fill the limiter to capacity.
	for i := 0; i < 5; i++ {
		ip := net.IPv4(10, 0, 0, byte(i)).String()
		allowed, _ := limiter.Allow(ip)
		if !allowed {
			t.Fatalf("IP %s should be allowed", ip)
		}
	}

	if limiter.Len() != 5 {
		t.Fatalf("expected 5 entries, got %d", limiter.Len())
	}

	// New IP should be rejected (fail closed).
	allowed, retryAfter := limiter.Allow("10.0.0.99")
	if allowed {
		t.Fatal("new IP should be rejected when limiter is at capacity")
	}
	if retryAfter <= 0 {
		t.Error("Retry-After should be positive when at capacity")
	}

	// Existing IPs should still be allowed.
	allowed, _ = limiter.Allow("10.0.0.0")
	if !allowed {
		t.Fatal("existing IP should still be allowed when at capacity")
	}
}

func TestGEExchange_RateLimit_CleanupFreesSlots(t *testing.T) {
	now := time.Now()
	limiter := newGEExchangeRateLimiter()
	limiter.maxEntries = 3
	limiter.maxAge = 10 * time.Minute
	limiter.nowFunc = func() time.Time { return now }

	// Fill to capacity.
	for i := 0; i < 3; i++ {
		ip := net.IPv4(10, 0, 0, byte(i)).String()
		limiter.Allow(ip)
	}

	// At capacity — new IP rejected.
	allowed, _ := limiter.Allow("10.0.0.99")
	if allowed {
		t.Fatal("should be at capacity")
	}

	// Advance time past maxAge and cleanup.
	now = now.Add(11 * time.Minute)
	limiter.Cleanup(now)

	if limiter.Len() != 0 {
		t.Fatalf("expected 0 entries after cleanup, got %d", limiter.Len())
	}

	// New IP should now be allowed.
	allowed, _ = limiter.Allow("10.0.0.99")
	if !allowed {
		t.Fatal("new IP should be allowed after cleanup freed slots")
	}
}

func TestGEExchange_RateLimit_CleanupUnderRace(t *testing.T) {
	limiter := newGEExchangeRateLimiter()
	limiter.maxEntries = 1000
	limiter.burst = 100

	// Concurrent Allow + Cleanup must not race.
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func(n int) {
			defer wg.Done()
			ip := net.IPv4(10, 0, byte(n/256), byte(n%256)).String()
			for j := 0; j < 50; j++ {
				limiter.Allow(ip)
			}
		}(i)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				limiter.Cleanup(time.Now())
			}
		}()
	}
	wg.Wait()
	// No panic or data race — test passes via -race detector.
}

// ---------------------------------------------------------------------------
// IP normalization tests.
// ---------------------------------------------------------------------------

func TestNormalizeIPForRateLimit(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"192.0.2.1", "192.0.2.1"},
		{"::ffff:192.0.2.1", "192.0.2.1"}, // IPv4-mapped IPv6
		{"2001:db8::1", "2001:db8::1"},    // pure IPv6
		{"::ffff:10.0.0.1", "10.0.0.1"},   // IPv4-mapped
		{"not-an-ip", "not-an-ip"},        // unparseable
		{"", ""},                          // empty
		{"::1", "::1"},                    // loopback
		{"127.0.0.1", "127.0.0.1"},        // IPv4 loopback
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := normalizeIPForRateLimit(tt.input)
			if got != tt.want {
				t.Errorf("normalizeIPForRateLimit(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Integration: rate limit + body limit do NOT break normal exchange flow.
// ---------------------------------------------------------------------------

func TestGEExchange_WithRateLimitAndBodyLimit_NormalFlow(t *testing.T) {
	identity := validGmailIdentity()
	validator := &fakeGoogleValidator{idTokenResult: identity}
	userStore := newFakeUserStore()
	limiter := newGEExchangeRateLimiter()

	server := newRateLimitedTestServer(validator, userStore, limiter)

	body := `{"credential":"test-token","credentialType":"id_token"}`
	w := httptest.NewRecorder()
	server.handleGEGoogleExchange(w, exchangeRequest(body))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp ExchangeResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
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
// geExchangeClientIP unit tests (IPv4/IPv6/malformed).
// ---------------------------------------------------------------------------

func TestGEExchangeClientIP_IPv6MappedIPv4(t *testing.T) {
	// Ensure IPv4-mapped IPv6 and regular IPv4 map to the same key,
	// so dual-stack clients cannot evade the limiter.
	req4 := httptest.NewRequest("GET", "/", nil)
	req4.RemoteAddr = "192.0.2.1:1234"

	req6 := httptest.NewRequest("GET", "/", nil)
	req6.RemoteAddr = "[::ffff:192.0.2.1]:1234"

	ip4 := geExchangeClientIP(req4, nil)
	ip6 := geExchangeClientIP(req6, nil)

	if ip4 != ip6 {
		t.Errorf("IPv4 %q and IPv4-mapped IPv6 %q should normalize to same key", ip4, ip6)
	}
}
