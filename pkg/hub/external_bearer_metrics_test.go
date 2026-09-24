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
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ---------------------------------------------------------------------------
// O1 — every outcome of design §4.7's scion_hub_external_bearer_total
// increments its labelled counter, proven by driving the real middleware
// path (doExternalBearerRequest -> UnifiedAuthMiddleware -> serveExternalBearer),
// not by calling RecordExternalBearer directly. Every test below asserts the
// exact (kind, principal, outcome) triple AND that exactly one call was
// recorded — which is also the "no other outcome series moved" check,
// since serveExternalBearer's outcome switch records at most once per
// request (auth_external_bearer.go).
// ---------------------------------------------------------------------------

// externalBearerMetricCall is one recorded scion_hub_external_bearer_total
// increment.
type externalBearerMetricCall struct {
	kind      ExternalBearerKind
	principal ExternalBearerPrincipal
	outcome   ExternalBearerOutcome
}

// fakeExternalBearerMetrics records every RecordExternalBearer call for
// assertion. Safe for concurrent use (needed for the C6-style singleflight
// path, and general defensiveness).
type fakeExternalBearerMetrics struct {
	mu    sync.Mutex
	calls []externalBearerMetricCall
}

func (f *fakeExternalBearerMetrics) RecordExternalBearer(kind ExternalBearerKind, principal ExternalBearerPrincipal, outcome ExternalBearerOutcome) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, externalBearerMetricCall{kind, principal, outcome})
}

func (f *fakeExternalBearerMetrics) allCalls() []externalBearerMetricCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]externalBearerMetricCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// attachExternalBearerMetrics wires a fresh fakeExternalBearerMetrics into
// cfg via the *atomic.Pointer indirection AuthConfig.ExternalBearerMetrics
// requires (see its doc comment): production wires this after New()
// returns, but a test can just store directly, since doExternalBearerRequest
// builds the middleware from this cfg value after the field is set.
func attachExternalBearerMetrics(cfg *AuthConfig) *fakeExternalBearerMetrics {
	fake := &fakeExternalBearerMetrics{}
	var rec ExternalBearerMetricsRecorder = fake
	var p atomic.Pointer[ExternalBearerMetricsRecorder]
	p.Store(&rec)
	cfg.ExternalBearerMetrics = &p
	return fake
}

// wantOneCall asserts that exactly one RecordExternalBearer call happened,
// with the exact triple given.
func wantOneCall(t *testing.T, fake *fakeExternalBearerMetrics, want externalBearerMetricCall) {
	t.Helper()
	calls := fake.allCalls()
	if len(calls) != 1 {
		t.Fatalf("RecordExternalBearer called %d time(s), want exactly 1: calls=%+v", len(calls), calls)
	}
	if calls[0] != want {
		t.Errorf("recorded call = %+v, want %+v", calls[0], want)
	}
}

func TestExternalBearerMetrics_OK_UserIDToken(t *testing.T) {
	kp := newGCVTestKeyPair("test-kid-1")
	endpoints := newTestEndpoints(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(gcvJWKSJSON(kp))
		}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
	)
	defer endpoints.close()

	userStore := newFakeUserStore()
	extStore := newMemExtIDStore()
	resolver := NewGoogleIdentityResolver(userStore, extStore, alwaysAuthorized, nil, slog.Default())
	cfg := newExternalBearerConfig(t, newTestValidator(endpoints), resolver)
	fake := attachExternalBearerMetrics(&cfg)

	claims := validIDTokenClaims()
	claims["aud"] = externalBearerTestAudience
	token := signIDToken(kp, claims)

	w, result := doExternalBearerRequest(cfg, token)
	if w.Code != http.StatusOK || !result.reached {
		t.Fatalf("status = %d reached=%v, want 200/true: body=%s", w.Code, result.reached, w.Body.String())
	}
	wantOneCall(t, fake, externalBearerMetricCall{ExternalBearerKindIDToken, ExternalBearerPrincipalUser, ExternalBearerOutcomeOK})
}

func TestExternalBearerMetrics_OK_UserAccessToken(t *testing.T) {
	tokenInfo, userInfo := validAccessTokenEndpoints(externalBearerTestAudience, "google-sub-metrics-1", "user@gmail.com", true)
	endpoints := newTestEndpoints(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), tokenInfo, userInfo)
	defer endpoints.close()

	userStore := newFakeUserStore()
	extStore := newMemExtIDStore()
	resolver := NewGoogleIdentityResolver(userStore, extStore, alwaysAuthorized, nil, slog.Default())
	cfg := newExternalBearerConfig(t, newTestValidator(endpoints), resolver)
	fake := attachExternalBearerMetrics(&cfg)

	w, result := doExternalBearerRequest(cfg, "opaque-access-token-metrics-1")
	if w.Code != http.StatusOK || !result.reached {
		t.Fatalf("status = %d reached=%v, want 200/true: body=%s", w.Code, result.reached, w.Body.String())
	}
	wantOneCall(t, fake, externalBearerMetricCall{ExternalBearerKindAccessToken, ExternalBearerPrincipalUser, ExternalBearerOutcomeOK})
}

func TestExternalBearerMetrics_OK_ServiceAccountIDToken(t *testing.T) {
	kp := newGCVTestKeyPair("test-kid-1")
	endpoints := newSAJWKSEndpoints(kp)
	defer endpoints.close()

	userStore := newFakeUserStore()
	extStore := newMemExtIDStore()
	resolver := NewGoogleIdentityResolver(userStore, extStore, neverAuthorized, nil, slog.Default())
	cfg := newExternalBearerConfigWithSA(t, newTestValidator(endpoints), resolver, []string{"my-a2a-project"})
	fake := attachExternalBearerMetrics(&cfg)

	claims := serviceAccountIDTokenClaims("worker@my-a2a-project.iam.gserviceaccount.com")
	token := signIDToken(kp, claims)

	w, result := doExternalBearerRequest(cfg, token)
	if w.Code != http.StatusOK || !result.reached {
		t.Fatalf("status = %d reached=%v, want 200/true: body=%s", w.Code, result.reached, w.Body.String())
	}
	wantOneCall(t, fake, externalBearerMetricCall{ExternalBearerKindIDToken, ExternalBearerPrincipalServiceAccount, ExternalBearerOutcomeOK})
}

// TestExternalBearerMetrics_NotApplicable_NoTrust proves that a rejection
// before classification (no Google trust configured) records kind=unknown,
// principal=unknown.
func TestExternalBearerMetrics_NotApplicable_NoTrust(t *testing.T) {
	kp := newGCVTestKeyPair("test-kid-1")

	userTokenSvc, err := NewUserTokenService(UserTokenConfig{})
	if err != nil {
		t.Fatalf("NewUserTokenService: %v", err)
	}
	counting := newRejectingCountingValidator()
	userStore := newFakeUserStore()
	extStore := newMemExtIDStore()
	resolver := NewGoogleIdentityResolver(userStore, extStore, alwaysAuthorized, nil, slog.Default())
	cfg := AuthConfig{
		Mode:            "production",
		UserTokenSvc:    userTokenSvc,
		Logger:          slog.Default(),
		GoogleValidator: counting,
		GoogleResolver:  resolver,
		// FederationAuth left nil: no Google trust configured at all.
	}
	fake := attachExternalBearerMetrics(&cfg)

	claims := validIDTokenClaims()
	claims["aud"] = externalBearerTestAudience
	token := signIDToken(kp, claims)

	w, result := doExternalBearerRequest(cfg, token)
	if result.reached {
		t.Fatal("handler must not be reached: no trust is configured")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: body=%s", w.Code, w.Body.String())
	}
	wantOneCall(t, fake, externalBearerMetricCall{ExternalBearerKindUnknown, ExternalBearerPrincipalUnknown, ExternalBearerOutcomeNotApplicable})
}

// TestExternalBearerMetrics_Rejected_WrongAudience covers the 401 "rejected"
// outcome from a verification failure: kind is known (classification
// succeeded), principal is not (verification never returned an identity).
func TestExternalBearerMetrics_Rejected_WrongAudience(t *testing.T) {
	kp := newGCVTestKeyPair("test-kid-1")
	endpoints := newTestEndpoints(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(gcvJWKSJSON(kp))
		}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
	)
	defer endpoints.close()

	userStore := newFakeUserStore()
	extStore := newMemExtIDStore()
	resolver := NewGoogleIdentityResolver(userStore, extStore, alwaysAuthorized, nil, slog.Default())
	cfg := newExternalBearerConfig(t, newTestValidator(endpoints), resolver)
	fake := attachExternalBearerMetrics(&cfg)

	claims := validIDTokenClaims()
	claims["aud"] = "some-other-client-id.apps.googleusercontent.com"
	token := signIDToken(kp, claims)

	w, result := doExternalBearerRequest(cfg, token)
	if result.reached {
		t.Fatal("handler must not be reached: wrong audience")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: body=%s", w.Code, w.Body.String())
	}
	wantOneCall(t, fake, externalBearerMetricCall{ExternalBearerKindIDToken, ExternalBearerPrincipalUnknown, ExternalBearerOutcomeRejected})
}

// TestExternalBearerMetrics_Rejected_ServiceAccountAccessToken covers the
// "rejected" outcome where principal IS known (service_account): the
// validator already returned an identity with IsServiceAccount before the
// access-token-shape rejection fires.
func TestExternalBearerMetrics_Rejected_ServiceAccountAccessToken(t *testing.T) {
	tokenInfo, userInfo := validAccessTokenEndpoints(externalBearerTestAudience, "sa-sub-metrics-1", "sa@proj.iam.gserviceaccount.com", true)
	endpoints := newTestEndpoints(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), tokenInfo, userInfo)
	defer endpoints.close()

	userStore := newFakeUserStore()
	extStore := newMemExtIDStore()
	resolver := NewGoogleIdentityResolver(userStore, extStore, alwaysAuthorized, nil, slog.Default())
	cfg := newExternalBearerConfig(t, newTestValidator(endpoints), resolver)
	fake := attachExternalBearerMetrics(&cfg)

	w, result := doExternalBearerRequest(cfg, "opaque-sa-access-token-metrics")
	if result.reached {
		t.Fatal("handler must not be reached: SA access tokens are always rejected")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: body=%s", w.Code, w.Body.String())
	}
	wantOneCall(t, fake, externalBearerMetricCall{ExternalBearerKindAccessToken, ExternalBearerPrincipalServiceAccount, ExternalBearerOutcomeRejected})
}

// TestExternalBearerMetrics_Rejected_ServiceAccountProjectNotAllowed covers
// the SA ID-token rejection: allowed_gcp_projects is set, but doesn't list
// this SA's project.
func TestExternalBearerMetrics_Rejected_ServiceAccountProjectNotAllowed(t *testing.T) {
	kp := newGCVTestKeyPair("test-kid-1")
	endpoints := newSAJWKSEndpoints(kp)
	defer endpoints.close()

	userStore := newFakeUserStore()
	extStore := newMemExtIDStore()
	resolver := NewGoogleIdentityResolver(userStore, extStore, alwaysAuthorized, nil, slog.Default())
	cfg := newExternalBearerConfigWithSA(t, newTestValidator(endpoints), resolver, []string{"some-other-project"})
	fake := attachExternalBearerMetrics(&cfg)

	claims := serviceAccountIDTokenClaims("worker@my-a2a-project.iam.gserviceaccount.com")
	token := signIDToken(kp, claims)

	w, result := doExternalBearerRequest(cfg, token)
	if result.reached {
		t.Fatal("handler must not be reached: project is not in allowed_gcp_projects")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: body=%s", w.Code, w.Body.String())
	}
	wantOneCall(t, fake, externalBearerMetricCall{ExternalBearerKindIDToken, ExternalBearerPrincipalServiceAccount, ExternalBearerOutcomeRejected})
}

// TestExternalBearerMetrics_Rejected_DomainNotAllowed covers the user
// allowed_domains rejection: a user (not SA) principal, already known by the
// time this check runs, is still "rejected" per the closed outcome set (no
// separate "domain_not_allowed" outcome — design §4.4 groups it with SA
// project rejection and verification failure).
func TestExternalBearerMetrics_Rejected_DomainNotAllowed(t *testing.T) {
	kp := newGCVTestKeyPair("test-kid-1")
	endpoints := newTestEndpoints(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(gcvJWKSJSON(kp))
		}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
	)
	defer endpoints.close()

	userStore := newFakeUserStore()
	extStore := newMemExtIDStore()
	resolver := NewGoogleIdentityResolver(userStore, extStore, alwaysAuthorized, nil, slog.Default())
	cfg := newExternalBearerConfigWithDomains(t, newTestValidator(endpoints), resolver, []string{"example.com"}, nil)
	fake := attachExternalBearerMetrics(&cfg)

	claims := validIDTokenClaims()
	claims["aud"] = externalBearerTestAudience
	claims["email"] = "user@not-listed.com"
	claims["hd"] = "not-listed.com"
	token := signIDToken(kp, claims)

	w, result := doExternalBearerRequest(cfg, token)
	if result.reached {
		t.Fatal("handler must not be reached: email domain is not in allowed_domains")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: body=%s", w.Code, w.Body.String())
	}
	wantOneCall(t, fake, externalBearerMetricCall{ExternalBearerKindIDToken, ExternalBearerPrincipalUser, ExternalBearerOutcomeRejected})
}

// TestExternalBearerMetrics_RateLimited covers the 429 outcome: kind is
// known (classification succeeded before the limiter is consulted),
// principal is not (verification never ran).
func TestExternalBearerMetrics_RateLimited(t *testing.T) {
	limiter := newExternalBearerRateLimiter(nil)
	limiter.buckets.burst = 1

	counting := &countingGoogleValidator{fakeGoogleValidator: fakeGoogleValidator{accessTokenErr: ErrGoogleInvalidCredential}}
	userStore := newFakeUserStore()
	extStore := newMemExtIDStore()
	resolver := NewGoogleIdentityResolver(userStore, extStore, alwaysAuthorized, nil, slog.Default())
	cfg := newExternalBearerConfig(t, counting, resolver)
	cfg.ExternalBearerLimiter = limiter
	fake := attachExternalBearerMetrics(&cfg)

	// First (distinct-token) request consumes the single burst slot.
	if w, _ := doExternalBearerRequest(cfg, "opaque-token-metrics-first"); w.Code == http.StatusTooManyRequests {
		t.Fatalf("first request unexpectedly rate limited: body=%s", w.Body.String())
	}

	w, result := doExternalBearerRequest(cfg, "opaque-token-metrics-second")
	if result.reached {
		t.Fatal("handler must not be reached when rate limited")
	}
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: body=%s", w.Code, w.Body.String())
	}

	calls := fake.allCalls()
	if len(calls) != 2 {
		t.Fatalf("RecordExternalBearer called %d time(s), want 2 (one per request): calls=%+v", len(calls), calls)
	}
	want := externalBearerMetricCall{ExternalBearerKindAccessToken, ExternalBearerPrincipalUnknown, ExternalBearerOutcomeRateLimited}
	if calls[1] != want {
		t.Errorf("second call = %+v, want %+v", calls[1], want)
	}
}

// TestExternalBearerMetrics_UpstreamError covers the 503 upstream_error
// outcome (ErrGoogleUpstreamError from the validator), distinct from
// store_error.
func TestExternalBearerMetrics_UpstreamError(t *testing.T) {
	endpoints := newTestEndpoints(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
	)
	defer endpoints.close()

	userStore := newFakeUserStore()
	extStore := newMemExtIDStore()
	resolver := NewGoogleIdentityResolver(userStore, extStore, alwaysAuthorized, nil, slog.Default())
	cfg := newExternalBearerConfig(t, newTestValidator(endpoints), resolver)
	fake := attachExternalBearerMetrics(&cfg)

	kp := newGCVTestKeyPair("test-kid-1")
	claims := validIDTokenClaims()
	claims["aud"] = externalBearerTestAudience
	token := signIDToken(kp, claims)

	w, result := doExternalBearerRequest(cfg, token)
	if result.reached {
		t.Fatal("handler must not be reached")
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: body=%s", w.Code, w.Body.String())
	}
	wantOneCall(t, fake, externalBearerMetricCall{ExternalBearerKindIDToken, ExternalBearerPrincipalUnknown, ExternalBearerOutcomeUpstreamError})
}

// TestExternalBearerMetrics_Suspended covers the 403 user_suspended outcome.
func TestExternalBearerMetrics_Suspended(t *testing.T) {
	kp := newGCVTestKeyPair("test-kid-1")
	endpoints := newTestEndpoints(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(gcvJWKSJSON(kp))
		}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
	)
	defer endpoints.close()

	userStore := newFakeUserStore()
	extStore := newMemExtIDStore()
	resolver := NewGoogleIdentityResolver(userStore, extStore, alwaysAuthorized, nil, slog.Default())
	cfg := newExternalBearerConfig(t, newTestValidator(endpoints), resolver)
	fake := attachExternalBearerMetrics(&cfg)

	claims := validIDTokenClaims()
	claims["aud"] = externalBearerTestAudience
	token := signIDToken(kp, claims)

	w1, first := doExternalBearerRequest(cfg, token)
	if w1.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200: body=%s", w1.Code, w1.Body.String())
	}

	u, err := userStore.GetUser(context.Background(), first.identity.ID())
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	u.Status = store.UserStatusSuspended
	if err := userStore.UpdateUser(context.Background(), u); err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}

	w2, second := doExternalBearerRequest(cfg, token)
	if w2.Code != http.StatusForbidden || second.reached {
		t.Fatalf("second request status = %d reached=%v, want 403/false: body=%s", w2.Code, second.reached, w2.Body.String())
	}

	calls := fake.allCalls()
	if len(calls) != 2 {
		t.Fatalf("RecordExternalBearer called %d time(s), want 2 (one per request): calls=%+v", len(calls), calls)
	}
	want := externalBearerMetricCall{ExternalBearerKindIDToken, ExternalBearerPrincipalUser, ExternalBearerOutcomeSuspended}
	if calls[1] != want {
		t.Errorf("second call = %+v, want %+v", calls[1], want)
	}
}

// TestExternalBearerMetrics_Forbidden_ResolveErrNotFound covers the 403
// forbidden outcome from a Resolve error wrapping store.ErrNotFound (a
// binding pointing at a deleted user) — distinct from store_error, since
// this fault is permanent and must not invite a retry.
func TestExternalBearerMetrics_Forbidden_ResolveErrNotFound(t *testing.T) {
	kp := newGCVTestKeyPair("test-kid-1")
	endpoints := newTestEndpoints(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(gcvJWKSJSON(kp))
		}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
	)
	defer endpoints.close()

	extStore := newMemExtIDStore()
	if err := extStore.CreateExternalIdentity(context.Background(), &ExternalIdentityBinding{
		ID:       "binding-orphan-metrics",
		Provider: "google",
		Issuer:   googleCanonicalIssuer,
		Subject:  "google-sub-test-123",
		UserID:   "missing-user",
		Email:    "user@gmail.com",
	}); err != nil {
		t.Fatalf("seed binding: %v", err)
	}
	userStore := &stubUserStore{getUser: func(_ context.Context, _ string) (*store.User, error) {
		return nil, store.ErrNotFound
	}}
	resolver := NewGoogleIdentityResolver(userStore, extStore, alwaysAuthorized, nil, slog.Default())
	cfg := newExternalBearerConfig(t, newTestValidator(endpoints), resolver)
	fake := attachExternalBearerMetrics(&cfg)

	claims := validIDTokenClaims()
	claims["aud"] = externalBearerTestAudience
	token := signIDToken(kp, claims)

	w, result := doExternalBearerRequest(cfg, token)
	if result.reached {
		t.Fatal("handler must not be reached")
	}
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: body=%s", w.Code, w.Body.String())
	}
	wantOneCall(t, fake, externalBearerMetricCall{ExternalBearerKindIDToken, ExternalBearerPrincipalUser, ExternalBearerOutcomeForbidden})
}

// TestExternalBearerMetrics_StoreError_ResolveInternalFault covers the 503
// store_error outcome from any other Resolve fault.
func TestExternalBearerMetrics_StoreError_ResolveInternalFault(t *testing.T) {
	kp := newGCVTestKeyPair("test-kid-1")
	endpoints := newTestEndpoints(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(gcvJWKSJSON(kp))
		}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
	)
	defer endpoints.close()

	extStore := newMemExtIDStore()
	if err := extStore.CreateExternalIdentity(context.Background(), &ExternalIdentityBinding{
		ID:       "binding-broken-metrics",
		Provider: "google",
		Issuer:   googleCanonicalIssuer,
		Subject:  "google-sub-test-123",
		UserID:   "some-user",
		Email:    "user@gmail.com",
	}); err != nil {
		t.Fatalf("seed binding: %v", err)
	}
	dbErr := errors.New("connection refused")
	userStore := &stubUserStore{getUser: func(_ context.Context, _ string) (*store.User, error) {
		return nil, dbErr
	}}
	resolver := NewGoogleIdentityResolver(userStore, extStore, alwaysAuthorized, nil, slog.Default())
	cfg := newExternalBearerConfig(t, newTestValidator(endpoints), resolver)
	fake := attachExternalBearerMetrics(&cfg)

	claims := validIDTokenClaims()
	claims["aud"] = externalBearerTestAudience
	token := signIDToken(kp, claims)

	w, result := doExternalBearerRequest(cfg, token)
	if result.reached {
		t.Fatal("handler must not be reached")
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: body=%s", w.Code, w.Body.String())
	}
	wantOneCall(t, fake, externalBearerMetricCall{ExternalBearerKindIDToken, ExternalBearerPrincipalUser, ExternalBearerOutcomeStoreError})
}

// TestExternalBearerMetrics_NilAuthConfigField_NoPanic proves that leaving
// AuthConfig.ExternalBearerMetrics unset (the zero value, nil) — the shape of
// every other test in this package that doesn't call
// attachExternalBearerMetrics — never panics. This is also, structurally,
// the "metrics disabled" case.
func TestExternalBearerMetrics_NilAuthConfigField_NoPanic(t *testing.T) {
	kp := newGCVTestKeyPair("test-kid-1")
	endpoints := newTestEndpoints(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(gcvJWKSJSON(kp))
		}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
	)
	defer endpoints.close()

	userStore := newFakeUserStore()
	extStore := newMemExtIDStore()
	resolver := NewGoogleIdentityResolver(userStore, extStore, alwaysAuthorized, nil, slog.Default())
	cfg := newExternalBearerConfig(t, newTestValidator(endpoints), resolver)
	// cfg.ExternalBearerMetrics is left nil (its zero value) deliberately.

	claims := validIDTokenClaims()
	claims["aud"] = externalBearerTestAudience
	token := signIDToken(kp, claims)

	w, result := doExternalBearerRequest(cfg, token)
	if w.Code != http.StatusOK || !result.reached {
		t.Fatalf("status = %d reached=%v, want 200/true: body=%s", w.Code, result.reached, w.Body.String())
	}

	// A wired but never-Store()d pointer, and one Store()d with a nil
	// interface, must also not panic.
	var p atomic.Pointer[ExternalBearerMetricsRecorder]
	cfg.ExternalBearerMetrics = &p
	if w, _ := doExternalBearerRequest(cfg, token); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (never-Store()d pointer must not panic)", w.Code)
	}
	var nilRec ExternalBearerMetricsRecorder
	p.Store(&nilRec)
	if w, _ := doExternalBearerRequest(cfg, token); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (Store()d nil interface must not panic)", w.Code)
	}
}

// TestExternalBearerMetrics_LabelTypesOnlyConstructedAsConstants is a durable
// regression guard for the closed-label-set requirement: every label value
// comes from a closed set, so no other value can ever be emitted. Every
// label value used by this design is one of the named constants declared in
// external_bearer_metrics.go; the only way
// an arbitrary, non-constant string could reach a counter is a type
// conversion like ExternalBearerOutcome(someVariable). Grepping every other
// non-test source file in the package for that syntax catches such a
// mutation directly, the same way I3's tokeninfo grep
// (TestNoTokenInfoOutsideGoogleCredentialValidator) catches its own
// regression class.
func TestExternalBearerMetrics_LabelTypesOnlyConstructedAsConstants(t *testing.T) {
	labelTypes := []string{
		"ExternalBearerKind(",
		"ExternalBearerPrincipal(",
		"ExternalBearerOutcome(",
		"GoogleValidatorCacheResult(",
		"GEExchangeOutcome(",
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	for _, f := range files {
		base := filepath.Base(f)
		if base == "external_bearer_metrics.go" || strings.HasSuffix(base, "_test.go") {
			continue // the const block itself, and this file's own literal list above
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, ty := range labelTypes {
			if strings.Contains(string(data), ty) {
				t.Errorf("%s: found a %s type conversion — label values must only ever be the named constants in external_bearer_metrics.go, never constructed from an arbitrary string", f, ty)
			}
		}
	}
}

// TestExternalBearerMetrics_LabelValidMethods cross-checks each label type's
// valid() switch against an independently hardcoded expected set (not the
// enumeration funcs valid() itself is unrelated to — a genuine cross-check,
// not a tautology): every expected constant must be both in the enumeration
// func and valid(), every enumerated value must be a known constant, and the
// two collections must be the same size. It also checks a handful of values
// outside the set are rejected. This test proves the switch and the
// enumeration agree with each other and with this independent list; it does
// not by itself prove invalid values are kept out of a recorder — that is
// TestRecord*_DropsInvalidLabel below, which is what actually enforces "no
// other value can be emitted" (recordExternalBearer, recordCache and
// recordGEExchange each check valid() before calling any recorder).
func TestExternalBearerMetrics_LabelValidMethods(t *testing.T) {
	t.Run("ExternalBearerKind", func(t *testing.T) {
		want := map[ExternalBearerKind]bool{
			ExternalBearerKindIDToken: true, ExternalBearerKindAccessToken: true, ExternalBearerKindUnknown: true,
		}
		checkValidCrossCheck(t, want, externalBearerKinds(), ExternalBearerKind.valid)
		for _, v := range []ExternalBearerKind{"", "ID_TOKEN", "oops"} {
			if v.valid() {
				t.Errorf("%q.valid() = true, want false", v)
			}
		}
	})
	t.Run("ExternalBearerPrincipal", func(t *testing.T) {
		want := map[ExternalBearerPrincipal]bool{
			ExternalBearerPrincipalUser: true, ExternalBearerPrincipalServiceAccount: true, ExternalBearerPrincipalUnknown: true,
		}
		checkValidCrossCheck(t, want, externalBearerPrincipals(), ExternalBearerPrincipal.valid)
		for _, v := range []ExternalBearerPrincipal{"", "USER", "oops"} {
			if v.valid() {
				t.Errorf("%q.valid() = true, want false", v)
			}
		}
	})
	t.Run("ExternalBearerOutcome", func(t *testing.T) {
		want := map[ExternalBearerOutcome]bool{
			ExternalBearerOutcomeOK: true, ExternalBearerOutcomeNotApplicable: true, ExternalBearerOutcomeRejected: true,
			ExternalBearerOutcomeRateLimited: true, ExternalBearerOutcomeUpstreamError: true, ExternalBearerOutcomeSuspended: true,
			ExternalBearerOutcomeForbidden: true, ExternalBearerOutcomeStoreError: true,
		}
		checkValidCrossCheck(t, want, externalBearerOutcomes(), ExternalBearerOutcome.valid)
		for _, v := range []ExternalBearerOutcome{"", "OK", "oops"} {
			if v.valid() {
				t.Errorf("%q.valid() = true, want false", v)
			}
		}
	})
	t.Run("GoogleValidatorCacheResult", func(t *testing.T) {
		want := map[GoogleValidatorCacheResult]bool{
			GoogleValidatorCacheHit: true, GoogleValidatorCacheMiss: true, GoogleValidatorCacheNegativeHit: true,
		}
		checkValidCrossCheck(t, want, googleValidatorCacheResults(), GoogleValidatorCacheResult.valid)
		for _, v := range []GoogleValidatorCacheResult{"", "HIT", "oops"} {
			if v.valid() {
				t.Errorf("%q.valid() = true, want false", v)
			}
		}
	})
	t.Run("GEExchangeOutcome", func(t *testing.T) {
		want := map[GEExchangeOutcome]bool{
			GEExchangeOutcomeOK: true, GEExchangeOutcomeRateLimited: true, GEExchangeOutcomeNotConfigured: true,
			GEExchangeOutcomeInvalidRequest: true, GEExchangeOutcomeBadRequest: true, GEExchangeOutcomeInvalidCredential: true,
			GEExchangeOutcomeForbidden: true, GEExchangeOutcomeExchangeFailed: true,
		}
		checkValidCrossCheck(t, want, geExchangeOutcomes(), GEExchangeOutcome.valid)
		for _, v := range []GEExchangeOutcome{"", "OK", "oops"} {
			if v.valid() {
				t.Errorf("%q.valid() = true, want false", v)
			}
		}
	})
}

// checkValidCrossCheck is the generic body TestExternalBearerMetrics_LabelValidMethods
// runs per label type: want is an expected set hardcoded independently in
// the test (not derived from enumFn or valid), enumFn is the type's
// enumeration func, and valid is the type's valid() method value (e.g.
// ExternalBearerKind.valid). It fails if want, enumFn's output and valid()'s
// acceptance ever disagree, in either direction.
func checkValidCrossCheck[T comparable](t *testing.T, want map[T]bool, enum []T, valid func(T) bool) {
	t.Helper()
	enumSet := make(map[T]bool, len(enum))
	for _, v := range enum {
		enumSet[v] = true
		if !valid(v) {
			t.Errorf("%v: in the enumeration but valid() = false", v)
		}
		if !want[v] {
			t.Errorf("%v: in the enumeration but not in this test's independent expected set", v)
		}
	}
	for v := range want {
		if !enumSet[v] {
			t.Errorf("%v: in this test's independent expected set but missing from the enumeration func", v)
		}
		if !valid(v) {
			t.Errorf("%v: in this test's independent expected set but valid() = false", v)
		}
	}
	if len(enumSet) != len(want) {
		t.Errorf("enumeration has %d distinct values, want %d", len(enumSet), len(want))
	}
}

// ---------------------------------------------------------------------------
// An invalid label reaches no recorder. This is what
// actually makes "no other value can be emitted" true: valid() by itself is
// just a predicate, so recordExternalBearer/recordCache/recordGEExchange
// must check it before calling any recorder, and these tests prove they do.
// ---------------------------------------------------------------------------

func TestRecordExternalBearer_DropsInvalidLabel(t *testing.T) {
	cases := []struct {
		name    string
		attempt externalBearerAttempt
		outcome ExternalBearerOutcome
	}{
		{"invalid kind", externalBearerAttempt{kind: ExternalBearerKind("oops"), principal: ExternalBearerPrincipalUser}, ExternalBearerOutcomeOK},
		{"invalid principal", externalBearerAttempt{kind: ExternalBearerKindIDToken, principal: ExternalBearerPrincipal("oops")}, ExternalBearerOutcomeOK},
		{"invalid outcome", externalBearerAttempt{kind: ExternalBearerKindIDToken, principal: ExternalBearerPrincipalUser}, ExternalBearerOutcome("oops")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeExternalBearerMetrics{}
			var rec ExternalBearerMetricsRecorder = fake
			var p atomic.Pointer[ExternalBearerMetricsRecorder]
			p.Store(&rec)
			cfg := AuthConfig{ExternalBearerMetrics: &p}

			recordExternalBearer(cfg, tc.attempt, tc.outcome)

			if got := len(fake.allCalls()); got != 0 {
				t.Errorf("RecordExternalBearer called %d time(s), want 0 (invalid label must be dropped)", got)
			}
		})
	}
}

func TestRecordCache_DropsInvalidLabel(t *testing.T) {
	base := &countingBaseValidator{
		idTokenResult: &ValidatedGoogleIdentity{Subject: "sub-1", Email: "user@gmail.com", EmailVerified: true},
	}
	fake := &fakeCacheMetrics{}
	cache := NewCachingGoogleCredentialValidator(base, WithCacheMetrics(fake)).(*cachingGoogleCredentialValidator)

	cache.recordCache(GoogleValidatorCacheResult("oops"))

	if got := len(fake.all()); got != 0 {
		t.Errorf("RecordGoogleValidatorCache called %d time(s), want 0 (invalid label must be dropped)", got)
	}
}

func TestRecordGEExchange_DropsInvalidLabel(t *testing.T) {
	fake := &fakeGEExchangeMetrics{}
	srv := &Server{geExchangeMetrics: fake}

	srv.recordGEExchange(GEExchangeOutcome("oops"))

	if got := len(fake.all()); got != 0 {
		t.Errorf("RecordGEExchangeRequest called %d time(s), want 0 (invalid label must be dropped)", got)
	}
}
