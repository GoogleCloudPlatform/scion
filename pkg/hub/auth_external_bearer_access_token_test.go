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
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"testing"
)

// ---------------------------------------------------------------------------
// Phase 2 — external-bearer Google OAuth2 access tokens, the caching
// decorator wired into the middleware, and the per-IP rate limiter (design
// §4.2(iii), §4.4; §6 rows C1-C6 and the access-token half of U2).
// ---------------------------------------------------------------------------

// validAccessTokenEndpoints returns tokeninfo/userinfo handlers describing a
// single, otherwise-valid Google access token.
func validAccessTokenEndpoints(azp, sub, email string, emailVerified bool) (http.HandlerFunc, http.HandlerFunc) {
	tokenInfo := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"azp": azp, "aud": azp, "sub": sub,
			"email": email, "email_verified": emailVerified,
			"expires_in": 3600,
		})
	}
	userInfo := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"sub": sub, "email": email, "email_verified": emailVerified, "name": "Test User",
		})
	}
	return tokenInfo, userInfo
}

// ---------------------------------------------------------------------------
// C1 — valid user access token, azp = expected -> 200; wrong azp -> 401
// (exact body).
// ---------------------------------------------------------------------------

func TestExternalBearer_AccessToken_ValidAzp_Authenticates(t *testing.T) {
	tokenInfo, userInfo := validAccessTokenEndpoints(externalBearerTestAudience, "google-sub-access-1", "user@gmail.com", true)
	endpoints := newTestEndpoints(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("JWKS must not be called for an access token") }),
		tokenInfo, userInfo,
	)
	defer endpoints.close()

	userStore := newFakeUserStore()
	extStore := newMemExtIDStore()
	resolver := NewGoogleIdentityResolver(userStore, extStore, alwaysAuthorized, nil, slog.Default())
	cfg := newExternalBearerConfig(t, newTestValidator(endpoints), resolver)

	w, result := doExternalBearerRequest(cfg, "opaque-access-token-1")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: body=%s", w.Code, w.Body.String())
	}
	if result.authType != AuthTypeExternalBearer {
		t.Errorf("auth type = %q, want %q", result.authType, AuthTypeExternalBearer)
	}
	if result.identity == nil || result.identity.Email() != "user@gmail.com" {
		t.Fatalf("expected authenticated user@gmail.com, got %+v", result.identity)
	}
}

func TestExternalBearer_AccessToken_WrongAzp_Unauthorized(t *testing.T) {
	tokenInfo, userInfo := validAccessTokenEndpoints("some-other-client-id.apps.googleusercontent.com", "google-sub-access-1", "user@gmail.com", true)
	endpoints := newTestEndpoints(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), tokenInfo, userInfo)
	defer endpoints.close()

	userStore := newFakeUserStore()
	extStore := newMemExtIDStore()
	resolver := NewGoogleIdentityResolver(userStore, extStore, alwaysAuthorized, nil, slog.Default())
	cfg := newExternalBearerConfig(t, newTestValidator(endpoints), resolver)

	w, result := doExternalBearerRequest(cfg, "opaque-access-token-2")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: body=%s", w.Code, w.Body.String())
	}
	if result.reached {
		t.Fatal("handler must not be reached for a wrong-azp access token")
	}
	wantBody := wantErrorBody(t, ErrCodeUnauthorized, "invalid external bearer token")
	if !bytes.Equal(w.Body.Bytes(), wantBody) {
		t.Errorf("body = %s, want %s", w.Body.Bytes(), wantBody)
	}
}

// ---------------------------------------------------------------------------
// U2 (access-token half) — email_verified=false -> 401.
// ---------------------------------------------------------------------------

func TestExternalBearer_AccessToken_UnverifiedEmail_Unauthorized(t *testing.T) {
	tokenInfo, userInfo := validAccessTokenEndpoints(externalBearerTestAudience, "google-sub-access-1", "user@gmail.com", false)
	endpoints := newTestEndpoints(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), tokenInfo, userInfo)
	defer endpoints.close()

	userStore := newFakeUserStore()
	extStore := newMemExtIDStore()
	resolver := NewGoogleIdentityResolver(userStore, extStore, alwaysAuthorized, nil, slog.Default())
	cfg := newExternalBearerConfig(t, newTestValidator(endpoints), resolver)

	w, result := doExternalBearerRequest(cfg, "opaque-access-token-3")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: body=%s", w.Code, w.Body.String())
	}
	if result.reached {
		t.Fatal("handler must not be reached for an unverified email")
	}
	wantBody := wantErrorBody(t, ErrCodeUnauthorized, "invalid external bearer token")
	if !bytes.Equal(w.Body.Bytes(), wantBody) {
		t.Errorf("body = %s, want %s", w.Body.Bytes(), wantBody)
	}
}

// ---------------------------------------------------------------------------
// A service-account identity must never be admitted via an access token,
// after this phase or any later one (Phase 3 owns the SA *ID-token* branch;
// this keeps the existing SA rejection covering both credential kinds).
// ---------------------------------------------------------------------------

func TestExternalBearer_AccessToken_ServiceAccount_Rejected(t *testing.T) {
	tokenInfo, userInfo := validAccessTokenEndpoints(externalBearerTestAudience, "sa-sub-1", "sa@proj.iam.gserviceaccount.com", true)
	endpoints := newTestEndpoints(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), tokenInfo, userInfo)
	defer endpoints.close()

	userStore := newFakeUserStore()
	extStore := newMemExtIDStore()
	resolver := NewGoogleIdentityResolver(userStore, extStore, alwaysAuthorized, nil, slog.Default())
	cfg := newExternalBearerConfig(t, newTestValidator(endpoints), resolver)

	w, result := doExternalBearerRequest(cfg, "opaque-sa-access-token")
	if result.reached {
		t.Fatal("handler must not be reached: a service-account identity must never be admitted via an access token")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: body=%s", w.Code, w.Body.String())
	}
	wantBody := wantErrorBody(t, ErrCodeUnauthorized, "invalid external bearer token")
	if !bytes.Equal(w.Body.Bytes(), wantBody) {
		t.Errorf("body = %s, want %s", w.Body.Bytes(), wantBody)
	}
	if len(userStore.users) != 0 {
		t.Errorf("expected no users created, got %d", len(userStore.users))
	}
}

// ---------------------------------------------------------------------------
// Phase 2 classifier change, end to end: with Google trust configured, an
// opaque token is now a candidate access token (unlike Phase 1, where it was
// unconditionally not-applicable). TestExternalBearer_ConfiguredTrustInvariant_Golden's
// case (d) proves the complementary "no trust" half of I1.
// ---------------------------------------------------------------------------

// TestExternalBearer_TrustConfiguredOpaqueToken_AttemptsAccessTokenValidation
// uses the REAL validator against a tokeninfo-400 stub, not a fake configured
// to return an error the real validator would never produce for this input.
// Review r1 finding 2: an earlier version of this test used
// fakeGoogleValidator{accessTokenErr: ErrGoogleInvalidCredential}, which
// happened to assert the post-fix status/body/code but could not have caught
// the pre-fix bug (real tokeninfo 400 -> ErrGoogleUpstreamError -> 503) at
// all, since the fake never went near that classification logic.
func TestExternalBearer_TrustConfiguredOpaqueToken_AttemptsAccessTokenValidation(t *testing.T) {
	var tokenInfoCalls atomic.Int64
	tokenInfoHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenInfoCalls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"error": "invalid_token"})
	})
	endpoints := newTestEndpoints(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("JWKS must not be called for an access token") }),
		tokenInfoHandler,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("userinfo must not be called when tokeninfo fails")
		}),
	)
	defer endpoints.close()

	userStore := newFakeUserStore()
	extStore := newMemExtIDStore()
	resolver := NewGoogleIdentityResolver(userStore, extStore, alwaysAuthorized, nil, slog.Default())
	cfg := newExternalBearerConfig(t, newTestValidator(endpoints), resolver)

	w, result := doExternalBearerRequest(cfg, "some-opaque-token")
	if result.reached {
		t.Fatal("handler must not be reached: Google rejects every access token here")
	}
	// The real validator's classification is what's under test: a tokeninfo
	// 400 must be 401 "invalid external bearer token", not 503
	// "upstream_unavailable" (review r1 finding 2).
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: body=%s", w.Code, w.Body.String())
	}
	wantBody := wantErrorBody(t, ErrCodeUnauthorized, "invalid external bearer token")
	if !bytes.Equal(w.Body.Bytes(), wantBody) {
		t.Errorf("body = %s, want %s", w.Body.Bytes(), wantBody)
	}
	if got := tokenInfoCalls.Load(); got != 1 {
		t.Errorf("tokeninfo called %d time(s), want 1 (Phase 2: trust configured + opaque token is now a candidate access token)", got)
	}
}

// TestExternalBearer_AccessToken_TokenInfo400_NegativelyCached is the second
// half of review r1 finding 2's ask: an invalid/expired/revoked access token
// (tokeninfo 400) must be negatively cached, so repeated presentations of the
// same garbage token within negTTL cost one upstream call, not one per
// request.
func TestExternalBearer_AccessToken_TokenInfo400_NegativelyCached(t *testing.T) {
	var tokenInfoCalls atomic.Int64
	tokenInfoHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenInfoCalls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"error": "invalid_token"})
	})
	endpoints := newTestEndpoints(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
		tokenInfoHandler,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("userinfo must not be called when tokeninfo fails")
		}),
	)
	defer endpoints.close()

	userStore := newFakeUserStore()
	extStore := newMemExtIDStore()
	resolver := NewGoogleIdentityResolver(userStore, extStore, alwaysAuthorized, nil, slog.Default())
	cached := NewCachingGoogleCredentialValidator(newTestValidator(endpoints))
	cfg := newExternalBearerConfig(t, cached, resolver)

	const token = "opaque-invalid-token"
	for i := 0; i < 2; i++ {
		w, result := doExternalBearerRequest(cfg, token)
		if result.reached {
			t.Fatalf("request %d: handler must not be reached", i)
		}
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("request %d: status = %d, want 401: body=%s", i, w.Code, w.Body.String())
		}
		wantBody := wantErrorBody(t, ErrCodeUnauthorized, "invalid external bearer token")
		if !bytes.Equal(w.Body.Bytes(), wantBody) {
			t.Errorf("request %d: body = %s, want %s", i, w.Body.Bytes(), wantBody)
		}
	}
	if got := tokenInfoCalls.Load(); got != 1 {
		t.Errorf("tokeninfo called %d time(s), want 1 (an invalid credential must be negatively cached)", got)
	}
}

// ---------------------------------------------------------------------------
// C2 — N requests with the same access token within the cache TTL cost
// exactly one tokeninfo call and one userinfo call, proven through the real
// middleware with the production caching decorator in front of the real
// validator (not just the decorator's own unit tests in
// google_credential_cache_test.go).
// ---------------------------------------------------------------------------

func TestExternalBearer_AccessToken_RepeatedRequests_OneUpstreamRoundTrip(t *testing.T) {
	var tokenInfoCalls, userInfoCalls atomic.Int64
	tokenInfoHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenInfoCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"azp": externalBearerTestAudience, "aud": externalBearerTestAudience,
			"sub": "google-sub-access-1", "email": "user@gmail.com", "email_verified": true,
			"expires_in": 3600,
		})
	})
	userInfoHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userInfoCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"sub": "google-sub-access-1", "email": "user@gmail.com", "email_verified": true, "name": "Test User",
		})
	})
	endpoints := newTestEndpoints(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), tokenInfoHandler, userInfoHandler)
	defer endpoints.close()

	userStore := newFakeUserStore()
	extStore := newMemExtIDStore()
	resolver := NewGoogleIdentityResolver(userStore, extStore, alwaysAuthorized, nil, slog.Default())
	cached := NewCachingGoogleCredentialValidator(newTestValidator(endpoints))
	cfg := newExternalBearerConfig(t, cached, resolver)

	const token = "opaque-access-token-cache-me"
	for i := 0; i < 5; i++ {
		w, _ := doExternalBearerRequest(cfg, token)
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200: body=%s", i, w.Code, w.Body.String())
		}
	}
	if got := tokenInfoCalls.Load(); got != 1 {
		t.Errorf("tokeninfo endpoint called %d time(s), want 1", got)
	}
	if got := userInfoCalls.Load(); got != 1 {
		t.Errorf("userinfo endpoint called %d time(s), want 1", got)
	}
}

// ---------------------------------------------------------------------------
// C4 — an upstream 5xx maps to 503 and is never negatively cached: the next
// request must retry upstream, not replay the failure.
// ---------------------------------------------------------------------------

func TestExternalBearer_AccessToken_UpstreamFault_ServiceUnavailableNotCached(t *testing.T) {
	var calls atomic.Int64
	tokenInfoHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	})
	endpoints := newTestEndpoints(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
		tokenInfoHandler,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("userinfo must not be called when tokeninfo fails")
		}),
	)
	defer endpoints.close()

	userStore := newFakeUserStore()
	extStore := newMemExtIDStore()
	resolver := NewGoogleIdentityResolver(userStore, extStore, alwaysAuthorized, nil, slog.Default())
	cached := NewCachingGoogleCredentialValidator(newTestValidator(endpoints))
	cfg := newExternalBearerConfig(t, cached, resolver)

	const token = "opaque-access-token-fault"
	for i := 0; i < 2; i++ {
		w, result := doExternalBearerRequest(cfg, token)
		if result.reached {
			t.Fatalf("request %d: handler must not be reached", i)
		}
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("request %d: status = %d, want 503: body=%s", i, w.Code, w.Body.String())
		}
		var errResp ErrorResponse
		if err := json.Unmarshal(w.Body.Bytes(), &errResp); err != nil {
			t.Fatalf("request %d: decode error body: %v", i, err)
		}
		if errResp.Error.Code != "upstream_unavailable" {
			t.Errorf("request %d: error code = %q, want %q", i, errResp.Error.Code, "upstream_unavailable")
		}
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("tokeninfo endpoint called %d time(s), want 2 (an upstream fault must never be negatively cached)", got)
	}
}

// ---------------------------------------------------------------------------
// C5 — random opaque tokens from one IP beyond the burst get 429 +
// Retry-After; cache hits are not rate-limited.
// ---------------------------------------------------------------------------

func TestExternalBearer_AccessToken_RateLimitedBeyondBurst(t *testing.T) {
	limiter := newExternalBearerRateLimiter(nil)
	limiter.buckets.burst = 3 // small burst, default 5rps refill (negligible within this test's runtime)

	// Every request below uses a distinct token, so every one of them is a
	// cache miss and must consult the rate limiter (design §4.4).
	counting := &countingGoogleValidator{fakeGoogleValidator: fakeGoogleValidator{accessTokenErr: ErrGoogleInvalidCredential}}
	userStore := newFakeUserStore()
	extStore := newMemExtIDStore()
	resolver := NewGoogleIdentityResolver(userStore, extStore, alwaysAuthorized, nil, slog.Default())
	cfg := newExternalBearerConfig(t, counting, resolver)
	cfg.ExternalBearerLimiter = limiter

	for i := 0; i < 3; i++ {
		w, _ := doExternalBearerRequest(cfg, fmt.Sprintf("opaque-token-%d", i))
		if w.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d: rate limited within the burst", i)
		}
	}

	w, result := doExternalBearerRequest(cfg, "opaque-token-beyond-burst")
	if result.reached {
		t.Fatal("handler must not be reached when rate limited")
	}
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: body=%s", w.Code, w.Body.String())
	}
	// Pinned to the exact expected value (review r1 optional finding 9), not
	// just non-empty: with burst 3 exhausted and the default 5 rps refill,
	// ceil(1/5) = 1 second is the only correct value. A units error (e.g.
	// milliseconds, or a hardcoded 0) would pass a mere non-empty check.
	if got := w.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want %q", got, "1")
	}
	wantBody := wantErrorBody(t, ErrCodeRateLimited, "rate limit exceeded")
	if !bytes.Equal(w.Body.Bytes(), wantBody) {
		t.Errorf("body = %s, want %s", w.Body.Bytes(), wantBody)
	}
}

func TestExternalBearer_AccessToken_CacheHitsNotRateLimited(t *testing.T) {
	limiter := newExternalBearerRateLimiter(nil)
	limiter.buckets.burst = 1 // the tightest possible budget: only the first (miss) request fits

	tokenInfo, userInfo := validAccessTokenEndpoints(externalBearerTestAudience, "google-sub-access-1", "user@gmail.com", true)
	endpoints := newTestEndpoints(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), tokenInfo, userInfo)
	defer endpoints.close()

	userStore := newFakeUserStore()
	extStore := newMemExtIDStore()
	resolver := NewGoogleIdentityResolver(userStore, extStore, alwaysAuthorized, nil, slog.Default())
	cached := NewCachingGoogleCredentialValidator(newTestValidator(endpoints))
	cfg := newExternalBearerConfig(t, cached, resolver)
	cfg.ExternalBearerLimiter = limiter

	const token = "opaque-access-token-repeat"
	for i := 0; i < 10; i++ {
		w, _ := doExternalBearerRequest(cfg, token)
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200 (cache hits must not be rate-limited): body=%s", i, w.Code, w.Body.String())
		}
	}
}
