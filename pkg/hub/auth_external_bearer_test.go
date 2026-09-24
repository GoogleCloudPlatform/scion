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
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"log/slog"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// ---------------------------------------------------------------------------
// Test harness for the external-bearer authentication path (Phase 1: Google
// user ID tokens only). Reuses the production-validator test seam from
// google_credential_validator_test.go (real RS256 verification against a
// pinned test JWKS, via *http.Client's RoundTripper) and the fake user/
// external-identity stores from ge_exchange_test.go, so both the exchange
// endpoint and the external-bearer path are proven against the same
// resolution logic.
// ---------------------------------------------------------------------------

// newGoogleTrustFederationAuth builds a FederationAuthenticator with a single
// user-type trusted issuer for accounts.google.com. The external-bearer path
// only reads this authenticator's IssuerConfig (audience, issuer_type) — it
// never calls Authenticate() for this issuer, since signature verification
// goes through cfg.GoogleValidator instead. The JWKS URL is therefore a
// placeholder that is never fetched.
func newGoogleTrustFederationAuth(t *testing.T, expectedAudience string) *FederationAuthenticator {
	t.Helper()
	return newGoogleFederationAuthWithIssuerType(t, expectedAudience, "user")
}

// newGoogleFederationAuthWithIssuerType is newGoogleTrustFederationAuth with a
// configurable issuer_type, so tests can build a Google trust entry with the
// "wrong" type (e.g. "service_account" or "hub") to prove googleTrust's
// issuer_type: user guard actually matters (r2 finding 2).
func newGoogleFederationAuthWithIssuerType(t *testing.T, expectedAudience, issuerType string) *FederationAuthenticator {
	t.Helper()
	fedCfg := config.FederationConfig{
		Enabled: true,
		TrustedIssuers: []config.TrustedIssuerConfig{
			{
				IssuerURL:        googleIssuerHTTPS,
				JWKSURL:          "http://unused.invalid/jwks",
				ExpectedAudience: expectedAudience,
				IssuerType:       issuerType,
			},
		},
	}
	fa, err := NewFederationAuthenticator(fedCfg, "https://hub.example.com", http.DefaultClient, "hosted", slog.Default())
	if err != nil {
		t.Fatalf("NewFederationAuthenticator: %v", err)
	}
	return fa
}

// newGoogleTrustFederationAuthWithSA builds a FederationAuthenticator, via
// the real validated NewFederationAuthenticator, whose Google issuer entry
// additionally carries AllowedGCPProjects — the distinct field for
// service-account project admission (not AllowedProjects, which is the
// unrelated, longer-standing hub-federation Scion-project allowlist). Used
// for testing the SA branch of authenticateExternalBearer (design §4.1,
// §4.4, §4.5).
func newGoogleTrustFederationAuthWithSA(t *testing.T, expectedAudience string, allowedGCPProjects []string) *FederationAuthenticator {
	t.Helper()
	fedCfg := config.FederationConfig{
		Enabled: true,
		TrustedIssuers: []config.TrustedIssuerConfig{
			{
				IssuerURL:          googleIssuerHTTPS,
				JWKSURL:            "http://unused.invalid/jwks",
				ExpectedAudience:   expectedAudience,
				IssuerType:         "user",
				AllowedGCPProjects: allowedGCPProjects,
			},
		},
	}
	fa, err := NewFederationAuthenticator(fedCfg, "https://hub.example.com", http.DefaultClient, "hosted", slog.Default())
	if err != nil {
		t.Fatalf("NewFederationAuthenticator: %v", err)
	}
	return fa
}

// federationAuthPointer wraps a *FederationAuthenticator in the
// atomic.Pointer AuthConfig.FederationAuth expects.
func federationAuthPointer(fa *FederationAuthenticator) *atomic.Pointer[FederationAuthenticator] {
	var p atomic.Pointer[FederationAuthenticator]
	p.Store(fa)
	return &p
}

// probeResult captures what the terminal handler observed after the
// middleware chain ran.
type probeResult struct {
	reached  bool
	identity UserIdentity
	authType string
}

func probeHandler(result *probeResult) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		result.reached = true
		result.identity = GetUserIdentityFromContext(r.Context())
		result.authType, _ = r.Context().Value(logging.AuthTypeKey{}).(string)
		w.WriteHeader(http.StatusOK)
	})
}

// wantErrorBody reconstructs the exact bytes writeError would produce for the
// given code/message, using the same ErrorResponse/APIError JSON shape and
// encoder settings (json.NewEncoder, which appends a trailing newline). It is
// built independently of the real response, so a mutation to the response
// shape itself (e.g. adding a details map, or interpolating an error into a
// message that must stay fixed) makes the comparison fail — this is the
// "golden" property R6/R7 ask for, without pinning fragile go-jose library
// error-string wording as a literal.
func wantErrorBody(t *testing.T, code, message string) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(ErrorResponse{Error: APIError{Code: code, Message: message}}); err != nil {
		t.Fatalf("encode expected body: %v", err)
	}
	return buf.Bytes()
}

// I2 reuses the existing countingGoogleValidator (ge_exchange_ratelimit_test.go),
// which tracks call counts for zero-call assertions, to prove that a valid Hub
// credential never reaches the Google validator.

const externalBearerTestAudience = "test-client-id.apps.googleusercontent.com"

// newExternalBearerConfig assembles an AuthConfig wired for the external-
// bearer path against local test endpoints, sharing a resolver backed by the
// given fake stores (matching production wiring: one resolver instance for
// both the exchange endpoint and this path).
func newExternalBearerConfig(t *testing.T, validator GoogleCredentialValidator, resolver *GoogleIdentityResolver) AuthConfig {
	t.Helper()
	userTokenSvc, err := NewUserTokenService(UserTokenConfig{})
	if err != nil {
		t.Fatalf("NewUserTokenService: %v", err)
	}
	fa := newGoogleTrustFederationAuth(t, externalBearerTestAudience)
	return AuthConfig{
		Mode:            "production",
		UserTokenSvc:    userTokenSvc,
		FederationAuth:  federationAuthPointer(fa),
		GoogleValidator: validator,
		GoogleResolver:  resolver,
		Logger:          slog.Default(),
	}
}

// newExternalBearerConfigWithSA is newExternalBearerConfig, but the Google
// trust entry carries AllowedGCPProjects (see newGoogleTrustFederationAuthWithSA).
func newExternalBearerConfigWithSA(t *testing.T, validator GoogleCredentialValidator, resolver *GoogleIdentityResolver, allowedGCPProjects []string) AuthConfig {
	t.Helper()
	userTokenSvc, err := NewUserTokenService(UserTokenConfig{})
	if err != nil {
		t.Fatalf("NewUserTokenService: %v", err)
	}
	fa := newGoogleTrustFederationAuthWithSA(t, externalBearerTestAudience, allowedGCPProjects)
	return AuthConfig{
		Mode:            "production",
		UserTokenSvc:    userTokenSvc,
		FederationAuth:  federationAuthPointer(fa),
		GoogleValidator: validator,
		GoogleResolver:  resolver,
		Logger:          slog.Default(),
	}
}

func doExternalBearerRequest(cfg AuthConfig, token string) (*httptest.ResponseRecorder, *probeResult) {
	result := &probeResult{}
	middleware := UnifiedAuthMiddleware(cfg)
	handler := middleware(probeHandler(result))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w, result
}

// ---------------------------------------------------------------------------
// U1 — valid Google-signed user ID token authenticates; sub-bound; auth-type
// in context is external-bearer.
// ---------------------------------------------------------------------------

func TestExternalBearer_ValidIDToken_Authenticates(t *testing.T) {
	kp := newGCVTestKeyPair("test-kid-1")
	endpoints := newTestEndpoints(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(gcvJWKSJSON(kp))
		}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("tokeninfo must not be called for an ID token") }),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("userinfo must not be called for an ID token") }),
	)
	defer endpoints.close()

	userStore := newFakeUserStore()
	extStore := newMemExtIDStore()
	resolver := NewGoogleIdentityResolver(userStore, extStore, alwaysAuthorized, nil, slog.Default())
	cfg := newExternalBearerConfig(t, newTestValidator(endpoints), resolver)

	claims := validIDTokenClaims()
	claims["aud"] = externalBearerTestAudience
	token := signIDToken(kp, claims)

	w, result := doExternalBearerRequest(cfg, token)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: body=%s", w.Code, w.Body.String())
	}
	if !result.reached {
		t.Fatal("handler was not reached")
	}
	if result.authType != AuthTypeExternalBearer {
		t.Errorf("auth type = %q, want %q", result.authType, AuthTypeExternalBearer)
	}
	if result.identity == nil {
		t.Fatal("expected a resolved user identity")
	}
	if result.identity.Email() != "user@gmail.com" {
		t.Errorf("email = %q, want user@gmail.com", result.identity.Email())
	}

	// Exactly one binding must have been created.
	binding, err := extStore.GetExternalIdentity(context.Background(), "google", googleCanonicalIssuer, "google-sub-test-123")
	if err != nil {
		t.Fatalf("expected external identity binding to be created: %v", err)
	}
	if binding.UserID != result.identity.ID() {
		t.Errorf("binding user id = %q, want %q", binding.UserID, result.identity.ID())
	}
}

// ---------------------------------------------------------------------------
// U1 (second half) — same sub, changed email on a second request resolves to
// the same (sub-bound) user, not a new one.
// ---------------------------------------------------------------------------

func TestExternalBearer_SubBound_EmailChangeKeepsSameUser(t *testing.T) {
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

	claims1 := validIDTokenClaims()
	claims1["aud"] = externalBearerTestAudience
	claims1["email"] = "old@gmail.com"
	token1 := signIDToken(kp, claims1)

	_, first := doExternalBearerRequest(cfg, token1)
	if first.identity == nil {
		t.Fatal("first request: expected a resolved identity")
	}

	claims2 := validIDTokenClaims()
	claims2["aud"] = externalBearerTestAudience
	claims2["email"] = "new@gmail.com" // same sub, changed (still-authoritative) email
	token2 := signIDToken(kp, claims2)

	w2, second := doExternalBearerRequest(cfg, token2)
	if w2.Code != http.StatusOK {
		t.Fatalf("second request status = %d, want 200: body=%s", w2.Code, w2.Body.String())
	}
	if second.identity == nil {
		t.Fatal("second request: expected a resolved identity")
	}
	if second.identity.ID() != first.identity.ID() {
		t.Errorf("email change caused a different user to resolve: %s != %s", second.identity.ID(), first.identity.ID())
	}
}

// ---------------------------------------------------------------------------
// U3 — a non-authoritative email for an unbound sub is rejected (403); a
// Gmail or matching-hd email is provisioned.
// ---------------------------------------------------------------------------

func TestExternalBearer_NonAuthoritativeEmail_Forbidden(t *testing.T) {
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

	claims := validIDTokenClaims()
	claims["aud"] = externalBearerTestAudience
	claims["email"] = "user@custom-domain.com" // not Gmail, no hd claim
	delete(claims, "hd")
	token := signIDToken(kp, claims)

	w, result := doExternalBearerRequest(cfg, token)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: body=%s", w.Code, w.Body.String())
	}
	if result.reached {
		t.Fatal("handler must not be reached for a rejected credential")
	}
	wantBody := wantErrorBody(t, ErrCodeForbidden, "access denied")
	if !bytes.Equal(w.Body.Bytes(), wantBody) {
		t.Errorf("body = %s, want %s", w.Body.Bytes(), wantBody)
	}
}

func TestExternalBearer_WorkspaceEmail_Provisioned(t *testing.T) {
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

	claims := validIDTokenClaims()
	claims["aud"] = externalBearerTestAudience
	claims["email"] = "user@company.com"
	claims["hd"] = "company.com"
	token := signIDToken(kp, claims)

	w, result := doExternalBearerRequest(cfg, token)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: body=%s", w.Code, w.Body.String())
	}
	if result.identity == nil || result.identity.Email() != "user@company.com" {
		t.Fatalf("expected provisioned user@company.com, got %+v", result.identity)
	}
}

// ---------------------------------------------------------------------------
// U2 (ID-token half) — email_verified=false is rejected with 401.
// ---------------------------------------------------------------------------

func TestExternalBearer_UnverifiedEmail_Unauthorized(t *testing.T) {
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

	claims := validIDTokenClaims()
	claims["aud"] = externalBearerTestAudience
	claims["email_verified"] = false
	token := signIDToken(kp, claims)

	w, result := doExternalBearerRequest(cfg, token)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: body=%s", w.Code, w.Body.String())
	}
	if result.reached {
		t.Fatal("handler must not be reached for an unverified email")
	}
	// R7: the reason (e.g. "google email not verified") must never leak into
	// the response body — only the fixed message, logged reason aside.
	wantBody := wantErrorBody(t, ErrCodeUnauthorized, "invalid external bearer token")
	if !bytes.Equal(w.Body.Bytes(), wantBody) {
		t.Errorf("body = %s, want %s", w.Body.Bytes(), wantBody)
	}
}

// R7 (wrong aud) — a valid Google-signed ID token whose aud does not match
// the configured expected_audience must be rejected with the same reason-free
// 401 body as any other verification failure.
func TestExternalBearer_WrongAudience_Unauthorized(t *testing.T) {
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

	claims := validIDTokenClaims()
	claims["aud"] = "some-other-client-id.apps.googleusercontent.com"
	token := signIDToken(kp, claims)

	w, result := doExternalBearerRequest(cfg, token)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: body=%s", w.Code, w.Body.String())
	}
	if result.reached {
		t.Fatal("handler must not be reached for a wrong-audience token")
	}
	wantBody := wantErrorBody(t, ErrCodeUnauthorized, "invalid external bearer token")
	if !bytes.Equal(w.Body.Bytes(), wantBody) {
		t.Errorf("body = %s, want %s", w.Body.Bytes(), wantBody)
	}
}

// ---------------------------------------------------------------------------
// U4 — a suspended bound user is rejected with 403 user_suspended on the very
// next request (no cache).
// ---------------------------------------------------------------------------

func TestExternalBearer_SuspendedUser_ForbiddenOnNextRequest(t *testing.T) {
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

	claims := validIDTokenClaims()
	claims["aud"] = externalBearerTestAudience
	token := signIDToken(kp, claims)

	// First request provisions and binds the user.
	w1, first := doExternalBearerRequest(cfg, token)
	if w1.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200: body=%s", w1.Code, w1.Body.String())
	}

	// Suspend the user directly in the store — no cache exists between the
	// resolver and the store, so the next request must see this immediately.
	u, err := userStore.GetUser(context.Background(), first.identity.ID())
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	u.Status = store.UserStatusSuspended
	if err := userStore.UpdateUser(context.Background(), u); err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}

	w2, second := doExternalBearerRequest(cfg, token)
	if w2.Code != http.StatusForbidden {
		t.Fatalf("second request status = %d, want 403: body=%s", w2.Code, w2.Body.String())
	}
	if second.reached {
		t.Fatal("handler must not be reached for a suspended user")
	}
	var errResp ErrorResponse
	if err := json.Unmarshal(w2.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if errResp.Error.Code != "user_suspended" {
		t.Errorf("error code = %q, want %q", errResp.Error.Code, "user_suspended")
	}
}

// ---------------------------------------------------------------------------
// U5 — a user whose email is in admin_emails is provisioned with the admin
// role (roleFor honours admin_emails, replacing the hard-coded "member").
// ---------------------------------------------------------------------------

func TestExternalBearer_AdminEmails_ProvisionsAdminRole(t *testing.T) {
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
	roleFor := func(_ context.Context, email string) string {
		if strings.EqualFold(email, "admin@gmail.com") {
			return "admin"
		}
		return "member"
	}
	resolver := NewGoogleIdentityResolver(userStore, extStore, alwaysAuthorized, roleFor, slog.Default())
	cfg := newExternalBearerConfig(t, newTestValidator(endpoints), resolver)

	claims := validIDTokenClaims()
	claims["aud"] = externalBearerTestAudience
	claims["email"] = "admin@gmail.com"
	token := signIDToken(kp, claims)

	w, result := doExternalBearerRequest(cfg, token)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: body=%s", w.Code, w.Body.String())
	}
	if result.identity == nil || result.identity.Role() != "admin" {
		t.Fatalf("expected admin role, got %+v", result.identity)
	}
}

// ---------------------------------------------------------------------------
// I1 — with no Google trust configured, the external-bearer hook is a true
// no-op: the response is byte-identical to the pre-existing rejection. This
// used to be its own "_Golden401" test, but it over-claimed: it asserted only
// a message prefix. It's superseded by golden case (a) in
// TestExternalBearer_ConfiguredTrustInvariant_Golden below, which asserts the
// exact bytes (r2 finding 9).
//
// r2 finding 8: since O3, production always builds a non-nil GoogleValidator
// (only trust gates the path now), so I1 must also be proven in that
// "production shape" — GoogleValidator non-nil, FederationAuth pointer empty
// — not just with GoogleValidator == nil like golden case (a). Otherwise the
// nil-guard in authenticateExternalBearer could mask a bug in the googleTrust
// gate itself.
// ---------------------------------------------------------------------------

func TestExternalBearer_NoTrustProductionShape_Golden401(t *testing.T) {
	kp := newGCVTestKeyPair("test-kid-1")

	userTokenSvc, err := NewUserTokenService(UserTokenConfig{})
	if err != nil {
		t.Fatalf("NewUserTokenService: %v", err)
	}
	// A non-nil validator, as O3's unconditional construction guarantees in
	// production — but it must never be called, so a fake (rather than a real
	// validator pointed at a JWKS server) both proves and enforces that.
	// A default error (rather than a zero-value fakeGoogleValidator, which
	// returns (nil, nil)) means that if a mutation ever lets this "must not
	// reach the validator" test actually reach it, the result is a real
	// error, not a nil identity — so this test fails at its own
	// counting.totalCalls() assertion instead of a nil-pointer-dereference
	// panic that aborts the whole test binary (review r2 optional finding 3).
	counting := newRejectingCountingValidator()
	userStore := newFakeUserStore()
	extStore := newMemExtIDStore()
	resolver := NewGoogleIdentityResolver(userStore, extStore, alwaysAuthorized, nil, slog.Default())
	cfg := AuthConfig{
		Mode:         "production",
		UserTokenSvc: userTokenSvc,
		Logger:       slog.Default(),
		// Production shape (O3): validator and resolver are always built,
		// even though no trust is configured (FederationAuth left nil here,
		// matching a server with no server.federation.trusted_issuers at all).
		GoogleValidator: counting,
		GoogleResolver:  resolver,
	}

	claims := validIDTokenClaims()
	claims["aud"] = externalBearerTestAudience
	token := signIDToken(kp, claims) // a validly-signed Google token — must still be rejected

	w, result := doExternalBearerRequest(cfg, token)
	if result.reached {
		t.Fatal("handler must not be reached: no trust is configured")
	}
	// Exact bytes (r3 review optional finding 3), the same way golden cases
	// (a)/(b)/(c) pin this fallback.
	_, verifyErr := userTokenSvc.ValidateUserToken(token)
	if verifyErr == nil {
		t.Fatal("test token must be invalid as a Hub JWT")
	}
	wantBody := wantErrorBody(t, ErrCodeUnauthorized, "invalid access token: "+verifyErr.Error())
	if w.Code != http.StatusUnauthorized || !bytes.Equal(w.Body.Bytes(), wantBody) {
		t.Fatalf("status/body = %d %s, want %d %s (the original invalid-access-token rejection)", w.Code, w.Body.String(), http.StatusUnauthorized, wantBody)
	}
	if counting.totalCalls() != 0 {
		t.Errorf("Google validator was called %d time(s); want 0 (googleTrust must gate before the validator, even in production shape)", counting.totalCalls())
	}
}

// ---------------------------------------------------------------------------
// I2 — a valid Hub-issued user JWT never reaches the Google validator.
// ---------------------------------------------------------------------------

func TestExternalBearer_ValidHubJWT_NeverTouchesGoogleValidator(t *testing.T) {
	userTokenSvc, err := NewUserTokenService(UserTokenConfig{})
	if err != nil {
		t.Fatalf("NewUserTokenService: %v", err)
	}
	accessToken, _, _, err := userTokenSvc.GenerateTokenPair(
		"hub-user-1", "hub-user@example.com", "Hub User", "member", ClientTypeWeb,
	)
	if err != nil {
		t.Fatalf("GenerateTokenPair: %v", err)
	}

	userStore := newFakeUserStore()
	extStore := newMemExtIDStore()
	resolver := NewGoogleIdentityResolver(userStore, extStore, alwaysAuthorized, nil, slog.Default())
	// A default error (rather than a zero-value fakeGoogleValidator, which
	// returns (nil, nil)) means that if a mutation ever lets this "must not
	// reach the validator" test actually reach it, the result is a real
	// error, not a nil identity — so this test fails at its own
	// counting.totalCalls() assertion instead of a nil-pointer-dereference
	// panic that aborts the whole test binary (review r2 optional finding 3).
	counting := newRejectingCountingValidator()

	fa := newGoogleTrustFederationAuth(t, externalBearerTestAudience)
	cfg := AuthConfig{
		Mode:            "production",
		UserTokenSvc:    userTokenSvc,
		FederationAuth:  federationAuthPointer(fa),
		GoogleValidator: counting,
		GoogleResolver:  resolver,
		Logger:          slog.Default(),
	}

	w, result := doExternalBearerRequest(cfg, accessToken)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: body=%s", w.Code, w.Body.String())
	}
	if !result.reached || result.authType != AuthTypeJWT {
		t.Fatalf("expected the normal Hub-JWT path (auth type %q), got reached=%v authType=%q",
			AuthTypeJWT, result.reached, result.authType)
	}
	if counting.totalCalls() != 0 {
		t.Errorf("Google validator was called %d time(s) for a valid Hub JWT", counting.totalCalls())
	}
}

// ---------------------------------------------------------------------------
// I3 — grep -rn tokeninfo pkg/hub --include='*.go' | grep -v _test must only
// match google_credential_validator.go. This is a durable regression test:
// the reuse map (design §7) explicitly drops the tokeninfo-based access-token
// path from the external-bearer file, and Phase 2 must not silently
// reintroduce it outside the one file that legitimately owns it.
// ---------------------------------------------------------------------------

func TestNoTokenInfoOutsideGoogleCredentialValidator(t *testing.T) {
	tokeninfoRE := regexp.MustCompile(`(?i)tokeninfo`)
	var offenders []string

	err := filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if filepath.Base(path) == "google_credential_validator.go" {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			if tokeninfoRE.MatchString(scanner.Text()) {
				offenders = append(offenders, path)
				break
			}
		}
		return scanner.Err()
	})
	if err != nil {
		t.Fatalf("walk pkg/hub: %v", err)
	}
	if len(offenders) > 0 {
		t.Errorf("unexpected 'tokeninfo' references outside google_credential_validator.go: %v", offenders)
	}
}

// ---------------------------------------------------------------------------
// I4 — no package-level mutable state in the new files.
// (Documented here; ensured by code review: auth_external_bearer.go and
// google_identity_resolver.go declare only functions, types, and immutable
// package-level values (error sentinels, constants) — no var with mutable
// state such as a map, cache, or counter.)
// ---------------------------------------------------------------------------

// TestExternalBearer_ClassifyNonJWT_AccessToken pins the Phase 2 classifier
// change (design §4.4): a non-JWT token is now a candidate access token, by
// shape alone — classifyExternalBearer itself does not know whether Google
// trust is configured. What keeps I1 intact is call order:
// authenticateExternalBearer only reaches classifyExternalBearer after
// googleTrust has already confirmed trust is configured (see
// TestExternalBearer_ConfiguredTrustInvariant_Golden's case (d), which pins
// the no-trust behaviour end-to-end).
func TestExternalBearer_ClassifyNonJWT_AccessToken(t *testing.T) {
	if got := classifyExternalBearer("not-a-jwt"); got != externalBearerAccessToken {
		t.Errorf("classifyExternalBearer(opaque) = %v, want externalBearerAccessToken", got)
	}
}

func TestExternalBearer_ClassifyNonGoogleIssuer_NotApplicable(t *testing.T) {
	userTokenSvc, err := NewUserTokenService(UserTokenConfig{SigningKey: []byte("test-signing-key-32-bytes-long!!")})
	if err != nil {
		t.Fatalf("NewUserTokenService: %v", err)
	}
	accessToken, _, _, err := userTokenSvc.GenerateTokenPair("u1", "u1@example.com", "U1", "member", ClientTypeWeb)
	if err != nil {
		t.Fatalf("GenerateTokenPair: %v", err)
	}
	if got := classifyExternalBearer(accessToken); got != externalBearerNotApplicable {
		t.Errorf("classifyExternalBearer(hub JWT) = %v, want externalBearerNotApplicable", got)
	}
}

func TestExternalBearer_ClassifyGoogleIDToken(t *testing.T) {
	kp := newGCVTestKeyPair("test-kid-1")
	token := signIDToken(kp, validIDTokenClaims())
	if got := classifyExternalBearer(token); got != externalBearerIDToken {
		t.Errorf("classifyExternalBearer(google id token) = %v, want externalBearerIDToken", got)
	}
}

// TestExternalBearer_ClassifyBareGoogleIssuer_IDToken proves the classifier's
// other Google issuer form (r2 finding 7): Google does issue some ID tokens
// with the bare "accounts.google.com" iss (no scheme), and mutation M22
// showed dropping that clause from the classifier passes silently (the
// request just falls through to the original 401 — fail closed, but a silent
// feature loss). Pin it as a classification case and an end-to-end U1 variant.
func TestExternalBearer_ClassifyBareGoogleIssuer_IDToken(t *testing.T) {
	kp := newGCVTestKeyPair("test-kid-1")
	claims := validIDTokenClaims()
	claims["iss"] = googleIssuerBare
	token := signIDToken(kp, claims)
	if got := classifyExternalBearer(token); got != externalBearerIDToken {
		t.Errorf("classifyExternalBearer(bare-issuer google id token) = %v, want externalBearerIDToken", got)
	}
}

func TestExternalBearer_BareGoogleIssuer_Authenticates(t *testing.T) {
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

	claims := validIDTokenClaims()
	claims["aud"] = externalBearerTestAudience
	claims["iss"] = googleIssuerBare
	token := signIDToken(kp, claims)

	w, result := doExternalBearerRequest(cfg, token)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: body=%s", w.Code, w.Body.String())
	}
	if result.identity == nil || result.identity.Email() != "user@gmail.com" {
		t.Fatalf("expected authenticated user@gmail.com, got %+v", result.identity)
	}
}

// ---------------------------------------------------------------------------
// R1 — an issuer configured with an empty expected_audience must disable the
// external-bearer path for that issuer. NewFederationAuthenticator falls back
// to the Hub's own OIDC issuer URL internally (for federation-token
// Authenticate() calls), but that fallback must not leak into googleTrust's
// view of "was an audience actually configured" (design §4.1, K2).
// ---------------------------------------------------------------------------

func TestGoogleTrust_EmptyExpectedAudience_NotOK(t *testing.T) {
	fa := newGoogleTrustFederationAuth(t, "")
	cfg := AuthConfig{FederationAuth: federationAuthPointer(fa)}
	if trust, ok := googleTrust(cfg); ok {
		t.Fatalf("googleTrust must report not-ok when expected_audience was not configured, got ok=true trust=%+v "+
			"(Authenticate() falls back to the Hub's OIDC issuer URL internally, but that must not leak here)", trust)
	}
}

// ---------------------------------------------------------------------------
// r2 finding 2 — googleTrust's issuer_type: user guard has real security
// weight: an operator who trusts accounts.google.com as issuer_type:
// service_account (for GCP workload-identity federation via
// X-Scion-Federation-Token) never opted a Google *user* ID token into the
// external-bearer path. Without the guard, any Google user with a verified
// email would be silently admitted under that config.
// ---------------------------------------------------------------------------

func TestGoogleTrust_RequiresIssuerTypeUser(t *testing.T) {
	tests := []struct {
		name       string
		issuerType string
		wantOK     bool
	}{
		{name: "user", issuerType: "user", wantOK: true},
		{name: "service_account", issuerType: "service_account", wantOK: false},
		{name: "hub", issuerType: "hub", wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fa := newGoogleFederationAuthWithIssuerType(t, externalBearerTestAudience, tt.issuerType)
			cfg := AuthConfig{FederationAuth: federationAuthPointer(fa)}
			_, ok := googleTrust(cfg)
			if ok != tt.wantOK {
				t.Errorf("googleTrust() ok = %v, want %v for issuer_type %q", ok, tt.wantOK, tt.issuerType)
			}
		})
	}
}

// TestExternalBearer_ServiceAccountFederationIssuer_NotApplicable is the
// middleware-level half of finding 2: a real Google-signed user ID token,
// valid in every other respect, must fall through unchanged when Google is
// trusted only as a service_account federation issuer.
func TestExternalBearer_ServiceAccountFederationIssuer_NotApplicable(t *testing.T) {
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

	fa := newGoogleFederationAuthWithIssuerType(t, externalBearerTestAudience, "service_account")
	// A default error (rather than a zero-value fakeGoogleValidator, which
	// returns (nil, nil)) means that if a mutation ever lets this "must not
	// reach the validator" test actually reach it, the result is a real
	// error, not a nil identity — so this test fails at its own
	// counting.totalCalls() assertion instead of a nil-pointer-dereference
	// panic that aborts the whole test binary (review r2 optional finding 3).
	counting := newRejectingCountingValidator()
	userStore := newFakeUserStore()
	extStore := newMemExtIDStore()
	resolver := NewGoogleIdentityResolver(userStore, extStore, alwaysAuthorized, nil, slog.Default())
	userTokenSvc, err := NewUserTokenService(UserTokenConfig{})
	if err != nil {
		t.Fatalf("NewUserTokenService: %v", err)
	}
	cfg := AuthConfig{
		Mode:            "production",
		UserTokenSvc:    userTokenSvc,
		FederationAuth:  federationAuthPointer(fa),
		GoogleValidator: counting,
		GoogleResolver:  resolver,
		Logger:          slog.Default(),
	}

	claims := validIDTokenClaims()
	claims["aud"] = externalBearerTestAudience
	token := signIDToken(kp, claims)

	w, result := doExternalBearerRequest(cfg, token)
	if result.reached {
		t.Fatal("handler must not be reached: Google is trusted as issuer_type service_account, not user")
	}
	// Exact bytes, the same way golden cases (b)/(c) pin the tokenTypeUser
	// fallback (r3 review optional finding 3): a valid Google-signed JWT is
	// still not a valid Hub JWT, so it fails the same way any other JWT with
	// an issuer google's classifier won't route would.
	_, verifyErr := userTokenSvc.ValidateUserToken(token)
	if verifyErr == nil {
		t.Fatal("test token must be invalid as a Hub JWT")
	}
	wantBody := wantErrorBody(t, ErrCodeUnauthorized, "invalid access token: "+verifyErr.Error())
	if w.Code != http.StatusUnauthorized || !bytes.Equal(w.Body.Bytes(), wantBody) {
		t.Fatalf("status/body = %d %s, want %d %s (the original invalid-access-token rejection)", w.Code, w.Body.String(), http.StatusUnauthorized, wantBody)
	}
	if counting.totalCalls() != 0 {
		t.Errorf("Google validator was called %d time(s); want 0 (issuer_type guard must stop it before validation)", counting.totalCalls())
	}
}

func TestExternalBearer_EmptyExpectedAudience_NotApplicable(t *testing.T) {
	// expected_audience unset -> NewFederationAuthenticator resolves it to
	// "https://hub.example.com" (the oidcIssuerURL passed at construction) for
	// its own Authenticate() use, but the external-bearer path must still
	// treat this issuer as not configured at all.
	fa := newGoogleTrustFederationAuth(t, "")
	// A default error (rather than a zero-value fakeGoogleValidator, which
	// returns (nil, nil)) means that if a mutation ever lets this "must not
	// reach the validator" test actually reach it, the result is a real
	// error, not a nil identity — so this test fails at its own
	// counting.totalCalls() assertion instead of a nil-pointer-dereference
	// panic that aborts the whole test binary (review r2 optional finding 3).
	counting := newRejectingCountingValidator()
	userStore := newFakeUserStore()
	extStore := newMemExtIDStore()
	resolver := NewGoogleIdentityResolver(userStore, extStore, alwaysAuthorized, nil, slog.Default())
	userTokenSvc, err := NewUserTokenService(UserTokenConfig{})
	if err != nil {
		t.Fatalf("NewUserTokenService: %v", err)
	}
	cfg := AuthConfig{
		Mode:            "production",
		UserTokenSvc:    userTokenSvc,
		FederationAuth:  federationAuthPointer(fa),
		GoogleValidator: counting,
		GoogleResolver:  resolver,
		Logger:          slog.Default(),
	}

	// A validly-shaped Google ID token whose aud equals the fallback audience
	// the (misused) resolved config would have produced. If the R1 bug were
	// present, this token would be accepted.
	kp := newGCVTestKeyPair("test-kid-1")
	claims := validIDTokenClaims()
	claims["aud"] = "https://hub.example.com"
	token := signIDToken(kp, claims)

	w, result := doExternalBearerRequest(cfg, token)
	if result.reached {
		t.Fatal("handler must not be reached: expected_audience was not configured, so the path must be not-applicable")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (original invalid-access-token rejection)", w.Code)
	}
	if counting.totalCalls() != 0 {
		t.Errorf("Google validator was called %d time(s); want 0 (empty expected_audience must disable the path)", counting.totalCalls())
	}
	var errResp ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if !strings.HasPrefix(errResp.Error.Message, "invalid access token:") {
		t.Errorf("message = %q, want the original invalid-access-token rejection (not-applicable fallthrough)", errResp.Error.Message)
	}
}

// ---------------------------------------------------------------------------
// S4 — an SA ID token whose azp disagrees with sub must be rejected, even
// though its aud is on the allowed list. (Originally written in Phase 1/2,
// when no SA branch existed at all and any SA identity was rejected outright
// regardless of shape; Phase 3 gives SA ID tokens an admission policy, so
// this now specifically exercises the azp/sub disagreement rule, design
// §4.2(ii).)
// ---------------------------------------------------------------------------

func TestExternalBearer_ServiceAccountIDToken_AZPNotSub_Unauthorized(t *testing.T) {
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
	// The SA's project ("proj") IS listed, so the only thing that can be
	// rejecting this token is the azp/sub check — not the allowed_gcp_projects
	// gate. Isolates S4 from S2.
	cfg := newExternalBearerConfigWithSA(t, newTestValidator(endpoints), resolver, []string{"proj"})

	claims := validIDTokenClaims()
	claims["aud"] = externalBearerTestAudience
	claims["azp"] = externalBearerTestAudience // disagrees with sub ("google-sub-test-123")
	claims["email"] = "sa@proj.iam.gserviceaccount.com"
	token := signIDToken(kp, claims)

	w, result := doExternalBearerRequest(cfg, token)

	if result.reached {
		t.Fatal("handler must not be reached: an SA ID token with azp != sub must be rejected")
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
	if _, err := extStore.GetExternalIdentity(context.Background(), "google", googleCanonicalIssuer, "google-sub-test-123"); err == nil {
		t.Error("expected no external identity binding to be created for a rejected SA identity")
	}
}

// ---------------------------------------------------------------------------
// Item 6 (fix round 2, EM decision) — internal resolver errors are not
// credential rejections. A Resolve error wrapping store.ErrNotFound (the
// bound user's record is gone) maps to 403 forbidden, the same body as any
// other access denial. Any other Resolve error (a store fault, a failed
// binding create, ...) maps to 503 store_error, matching the Hub-JWT path's
// store-fault handling — not the generic 401 a validator/principal-policy
// failure gets.
// ---------------------------------------------------------------------------

func TestExternalBearer_ResolveErrNotFound_Forbidden(t *testing.T) {
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
	// A binding exists, but the user it points to is gone.
	if err := extStore.CreateExternalIdentity(context.Background(), &ExternalIdentityBinding{
		ID:       "binding-orphan",
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
	wantBody := wantErrorBody(t, ErrCodeForbidden, "access denied")
	if !bytes.Equal(w.Body.Bytes(), wantBody) {
		t.Errorf("body = %s, want %s", w.Body.Bytes(), wantBody)
	}
}

func TestExternalBearer_ResolveInternalError_ServiceUnavailable(t *testing.T) {
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
		ID:       "binding-broken",
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
	wantBody := wantErrorBody(t, "store_error", "unable to verify user status")
	if !bytes.Equal(w.Body.Bytes(), wantBody) {
		t.Errorf("body = %s, want %s", w.Body.Bytes(), wantBody)
	}
}

// ---------------------------------------------------------------------------
// F2 (fix round 3, now in scope per the issue owner: impl-design §4.3's 4th
// allowed resolver delta). A GetExternalIdentity fault that is NOT
// store.ErrNotFound must not be treated as "no binding" — that would let a
// transient store error provision a duplicate user, or (for a
// non-authoritative email) surface as 403 instead of the store fault it
// actually is. It must be wrapped and reach the item-6 503 store_error arm,
// the same as any other internal Resolve fault, having touched neither the
// email lookup nor binding creation.
// ---------------------------------------------------------------------------

// trackingExtIDStore is a minimal ExternalIdentityStore whose
// GetExternalIdentity always fails with a configured error, and which
// records whether CreateExternalIdentity was ever called — it must not be,
// since Resolve should fail before ever reaching the bootstrap path.
type trackingExtIDStore struct {
	getErr       error
	createCalled bool
}

func (s *trackingExtIDStore) GetExternalIdentity(_ context.Context, _, _, _ string) (*store.ExternalIdentityBinding, error) {
	return nil, s.getErr
}

func (s *trackingExtIDStore) CreateExternalIdentity(_ context.Context, _ *store.ExternalIdentityBinding) error {
	s.createCalled = true
	return nil
}

func (s *trackingExtIDStore) UpdateExternalIdentityEmail(_ context.Context, _, _ string) error {
	return nil
}

func (s *trackingExtIDStore) GetExternalIdentitiesByUserID(_ context.Context, _ string) ([]*store.ExternalIdentityBinding, error) {
	return nil, nil
}

// trackingUserStore is a UserStore whose GetUserByEmail records whether it
// was ever called. Embedding store.UserStore (nil) means any other method
// call panics loudly, which is exactly what we want: Resolve must not reach
// any of them either when GetExternalIdentity faults. CreateUser is
// overridden (rather than left to panic on the nil embed) so that a
// regression which disables the F2 guard fails at this test's own
// getByEmailCalled/createUserCalled assertions instead of a SIGSEGV that
// aborts the whole test binary (review r4, optional finding 1).
type trackingUserStore struct {
	store.UserStore
	getByEmailCalled bool
	createUserCalled bool
}

func (s *trackingUserStore) GetUserByEmail(_ context.Context, _ string) (*store.User, error) {
	s.getByEmailCalled = true
	return nil, store.ErrNotFound
}

func (s *trackingUserStore) CreateUser(_ context.Context, _ *store.User) error {
	s.createUserCalled = true
	return errors.New("trackingUserStore: CreateUser must not be reached")
}

func TestExternalBearer_GetExternalIdentityFault_ServiceUnavailable(t *testing.T) {
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

	extStore := &trackingExtIDStore{getErr: errors.New("connection refused")}
	userStore := &trackingUserStore{}
	resolver := NewGoogleIdentityResolver(userStore, extStore, alwaysAuthorized, nil, slog.Default())
	cfg := newExternalBearerConfig(t, newTestValidator(endpoints), resolver)

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
	wantBody := wantErrorBody(t, "store_error", "unable to verify user status")
	if !bytes.Equal(w.Body.Bytes(), wantBody) {
		t.Errorf("body = %s, want %s", w.Body.Bytes(), wantBody)
	}
	if userStore.getByEmailCalled {
		t.Error("GetUserByEmail must not be called: a GetExternalIdentity fault is not \"no binding\"")
	}
	if userStore.createUserCalled {
		t.Error("CreateUser must not be called: a GetExternalIdentity fault must fail closed before bootstrap")
	}
	if extStore.createCalled {
		t.Error("CreateExternalIdentity must not be called: a GetExternalIdentity fault must fail closed before bootstrap")
	}
}

// ---------------------------------------------------------------------------
// R6 — the §3 invariant, proven byte-for-byte, including with Google trust
// actually configured (not just absent), and at both hook sites in
// UnifiedAuthMiddleware.
// ---------------------------------------------------------------------------

func TestExternalBearer_ConfiguredTrustInvariant_Golden(t *testing.T) {
	userTokenSvc, err := NewUserTokenService(UserTokenConfig{})
	if err != nil {
		t.Fatalf("NewUserTokenService: %v", err)
	}
	otherTokenSvc, err := NewUserTokenService(UserTokenConfig{SigningKey: []byte("a-different-signing-key-32-byte!")})
	if err != nil {
		t.Fatalf("NewUserTokenService (other key): %v", err)
	}
	fa := newGoogleTrustFederationAuth(t, externalBearerTestAudience)
	userStore := newFakeUserStore()
	extStore := newMemExtIDStore()
	resolver := NewGoogleIdentityResolver(userStore, extStore, alwaysAuthorized, nil, slog.Default())
	kp := newGCVTestKeyPair("test-kid-1")

	const nonHubJWT = "not-a.valid-hub.jwt"
	wrongKeyToken, _, _, err := otherTokenSvc.GenerateTokenPair("u1", "u1@example.com", "U1", "member", ClientTypeWeb)
	if err != nil {
		t.Fatalf("GenerateTokenPair (other key): %v", err)
	}
	nonGoogleIssToken := signIDToken(kp, map[string]interface{}{
		"iss": "https://issuer.example.com",
		"sub": "sub-1",
		"aud": externalBearerTestAudience,
		"exp": time.Now().Add(10 * time.Minute).Unix(),
	})
	const opaqueToken = "opaque-nonjwt-token-xyz"

	tests := []struct {
		name       string
		withTrust  bool
		token      string
		wantStatus int
		wantBody   func(t *testing.T) []byte
	}{
		{
			name:       "a_no_trust_non_hub_jwt",
			withTrust:  false,
			token:      nonHubJWT,
			wantStatus: http.StatusUnauthorized,
			wantBody: func(t *testing.T) []byte {
				_, err := userTokenSvc.ValidateUserToken(nonHubJWT)
				if err == nil {
					t.Fatal("test token must be invalid")
				}
				return wantErrorBody(t, ErrCodeUnauthorized, "invalid access token: "+err.Error())
			},
		},
		{
			// (b) trust configured + an expired-or-wrongly-signed Hub JWT.
			name:       "b_trust_configured_wrong_signature_hub_jwt",
			withTrust:  true,
			token:      wrongKeyToken,
			wantStatus: http.StatusUnauthorized,
			wantBody: func(t *testing.T) []byte {
				_, err := userTokenSvc.ValidateUserToken(wrongKeyToken)
				if err == nil {
					t.Fatal("test token must be invalid against userTokenSvc's key")
				}
				return wantErrorBody(t, ErrCodeUnauthorized, "invalid access token: "+err.Error())
			},
		},
		{
			// (c) trust configured + a JWT with a non-Google iss.
			name:       "c_trust_configured_non_google_iss",
			withTrust:  true,
			token:      nonGoogleIssToken,
			wantStatus: http.StatusUnauthorized,
			wantBody: func(t *testing.T) []byte {
				_, err := userTokenSvc.ValidateUserToken(nonGoogleIssToken)
				if err == nil {
					t.Fatal("test token must be invalid as a Hub JWT")
				}
				return wantErrorBody(t, ErrCodeUnauthorized, "invalid access token: "+err.Error())
			},
		},
		{
			// (d) NO trust + an opaque token, exercising the
			// UnifiedAuthMiddleware default: arm (the second hook site).
			// Phase 2 changed the classifier so that, WITH trust configured,
			// an opaque token is a candidate access token (see
			// TestExternalBearer_ClassifyNonJWT_AccessToken and
			// TestExternalBearer_TrustConfiguredOpaqueToken_AttemptsAccessTokenValidation
			// below) — but the gate is googleTrust, checked in
			// authenticateExternalBearer before classification ever runs, so
			// absent trust this must still fall through byte-identical, with
			// zero validator calls (I1). No library-dependent text here, so
			// this is hardened to a true byte literal rather than
			// wantErrorBody's live reconstruction, per r2 finding 5's "cheap
			// hardening" ask.
			name:       "d_no_trust_opaque_token_default_arm",
			withTrust:  false,
			token:      opaqueToken,
			wantStatus: http.StatusUnauthorized,
			wantBody: func(t *testing.T) []byte {
				return []byte(`{"error":{"code":"unauthorized","message":"unrecognized token format"}}` + "\n")
			},
		},
		{
			// (e) trust configured + no Authorization header at all. Also no
			// library-dependent text: a true literal.
			name:       "e_trust_configured_empty_header",
			withTrust:  true,
			token:      "",
			wantStatus: http.StatusUnauthorized,
			wantBody: func(t *testing.T) []byte {
				return []byte(`{"error":{"code":"unauthorized","message":"missing authorization header"}}` + "\n")
			},
		},
		{
			// (f) trust configured + a scion_pat_-shaped token with no UATSvc
			// configured: rejected in the tokenTypeUAT branch, before either
			// hook site runs. A true literal, same reasoning.
			name:       "f_trust_configured_pat_shaped_token",
			withTrust:  true,
			token:      "scion_pat_deadbeefdeadbeef",
			wantStatus: http.StatusUnauthorized,
			wantBody: func(t *testing.T) []byte {
				return []byte(`{"error":{"code":"unauthorized","message":"user access token authentication is not enabled"}}` + "\n")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// A default error (rather than a zero-value fakeGoogleValidator,
			// which returns (nil, nil)) means that if a mutation ever lets a
			// no-trust case reach the validator, ValidateIDToken/
			// ValidateAccessToken return a real error instead of a nil
			// identity — so authenticateExternalBearer fails at
			// `id.IsServiceAccount` never even being reached, and this test
			// fails at its own counting.totalCalls() assertion below instead
			// of panicking (nil-pointer dereference) and aborting the whole
			// test binary before the remaining golden cases run (review r2
			// optional finding 3; the same class of problem P1 r4 optional
			// finding 1 fixed for trackingUserStore).
			counting := newRejectingCountingValidator()
			// GoogleValidator/GoogleResolver are always wired, matching
			// production shape (server.go's New builds them unconditionally,
			// O3): only trust varies below. Review r1 finding 3: an earlier
			// version of this test left them nil in the no-trust cases, which
			// meant authenticateExternalBearer's `cfg.GoogleValidator == nil`
			// guard returned not-applicable regardless of whether the
			// googleTrust gate itself was intact — so a mutation that skipped
			// the trust check for non-JWT tokens (`if !ok && looksLikeJWT(token)`)
			// passed the whole suite silently. With the validator always
			// non-nil, that mutation now surfaces as a nonzero
			// counting.totalCalls() below.
			cfg := AuthConfig{
				Mode:            "production",
				UserTokenSvc:    userTokenSvc,
				Logger:          slog.Default(),
				GoogleValidator: counting,
				GoogleResolver:  resolver,
			}
			if tt.withTrust {
				cfg.FederationAuth = federationAuthPointer(fa)
			}

			w, result := doExternalBearerRequest(cfg, tt.token)
			if result.reached {
				t.Fatal("handler must not be reached")
			}
			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d: body=%s", w.Code, tt.wantStatus, w.Body.String())
			}
			wantBody := tt.wantBody(t)
			if !bytes.Equal(w.Body.Bytes(), wantBody) {
				t.Errorf("body = %s, want %s (byte-identical invariant)", w.Body.Bytes(), wantBody)
			}
			if counting.totalCalls() != 0 {
				t.Errorf("Google validator was called %d time(s); want 0", counting.totalCalls())
			}
		})
	}
}

// ---------------------------------------------------------------------------
// O1 — PAT and agent tokens are structurally unreachable from either
// external-bearer hook site (they are handled by earlier branches in
// UnifiedAuthMiddleware), but pin that invariant against future reordering.
// ---------------------------------------------------------------------------

func TestExternalBearer_PATShapedToken_NeverTouchesGoogleValidator(t *testing.T) {
	// scion_pat_* tokens route to tokenTypeUAT via detectTokenType's prefix
	// check, before either hook site runs, and a PAT is never 3 dot-separated
	// segments so it cannot reach tokenTypeUser either. This test does not
	// exercise real UAT validation (UATSvc is nil here, so the request is
	// rejected before reaching any hook) — it pins that the prefix routing
	// itself never reaches the Google validator. It cannot distinguish hook
	// *ordering* (judgement 1/O1): the case is decided by detectTokenType
	// before either hook is even consulted, regardless of where the hooks
	// are placed in the switch.
	// A default error (rather than a zero-value fakeGoogleValidator, which
	// returns (nil, nil)) means that if a mutation ever lets this "must not
	// reach the validator" test actually reach it, the result is a real
	// error, not a nil identity — so this test fails at its own
	// counting.totalCalls() assertion instead of a nil-pointer-dereference
	// panic that aborts the whole test binary (review r2 optional finding 3).
	counting := newRejectingCountingValidator()
	userStore := newFakeUserStore()
	extStore := newMemExtIDStore()
	resolver := NewGoogleIdentityResolver(userStore, extStore, alwaysAuthorized, nil, slog.Default())
	fa := newGoogleTrustFederationAuth(t, externalBearerTestAudience)
	userTokenSvc, err := NewUserTokenService(UserTokenConfig{})
	if err != nil {
		t.Fatalf("NewUserTokenService: %v", err)
	}
	cfg := AuthConfig{
		Mode:            "production",
		UserTokenSvc:    userTokenSvc,
		FederationAuth:  federationAuthPointer(fa),
		GoogleValidator: counting,
		GoogleResolver:  resolver,
		Logger:          slog.Default(),
	}

	w, result := doExternalBearerRequest(cfg, "scion_pat_deadbeefdeadbeef")
	if result.reached {
		t.Fatal("handler must not be reached: UATSvc is not configured")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if counting.totalCalls() != 0 {
		t.Errorf("Google validator was called %d time(s) for a PAT-shaped token", counting.totalCalls())
	}
}

func TestExternalBearer_ValidAgentToken_NeverTouchesGoogleValidator(t *testing.T) {
	// A valid agent token is fully handled in Step 1 of UnifiedAuthMiddleware
	// (X-Scion-Agent-Token header) and returns unconditionally on success,
	// before detectTokenType or either hook site runs. This case also cannot
	// distinguish hook ordering (judgement 1/O1) — moving the hooks earlier in
	// the tokenTypeUser/default switch would not change this outcome, since
	// the agent-token branch is a separate, earlier step entirely.
	agentTokenSvc, err := NewAgentTokenService(AgentTokenConfig{})
	if err != nil {
		t.Fatalf("NewAgentTokenService: %v", err)
	}
	agentToken, err := agentTokenSvc.GenerateAgentToken("agent-1", "project-1", []AgentTokenScope{ScopeAgentStatusUpdate}, nil)
	if err != nil {
		t.Fatalf("GenerateAgentToken: %v", err)
	}

	// A default error (rather than a zero-value fakeGoogleValidator, which
	// returns (nil, nil)) means that if a mutation ever lets this "must not
	// reach the validator" test actually reach it, the result is a real
	// error, not a nil identity — so this test fails at its own
	// counting.totalCalls() assertion instead of a nil-pointer-dereference
	// panic that aborts the whole test binary (review r2 optional finding 3).
	counting := newRejectingCountingValidator()
	userStore := newFakeUserStore()
	extStore := newMemExtIDStore()
	resolver := NewGoogleIdentityResolver(userStore, extStore, alwaysAuthorized, nil, slog.Default())
	fa := newGoogleTrustFederationAuth(t, externalBearerTestAudience)
	userTokenSvc, err := NewUserTokenService(UserTokenConfig{})
	if err != nil {
		t.Fatalf("NewUserTokenService: %v", err)
	}
	cfg := AuthConfig{
		Mode:            "production",
		AgentTokenSvc:   agentTokenSvc,
		UserTokenSvc:    userTokenSvc,
		FederationAuth:  federationAuthPointer(fa),
		GoogleValidator: counting,
		GoogleResolver:  resolver,
		Logger:          slog.Default(),
	}

	result := &probeResult{}
	middleware := UnifiedAuthMiddleware(cfg)
	handler := middleware(probeHandler(result))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	req.Header.Set("X-Scion-Agent-Token", agentToken)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: body=%s", w.Code, w.Body.String())
	}
	if !result.reached || result.authType != AuthTypeAgent {
		t.Fatalf("expected agent auth type, got reached=%v authType=%q", result.reached, result.authType)
	}
	if counting.totalCalls() != 0 {
		t.Errorf("Google validator was called %d time(s) for a valid agent token", counting.totalCalls())
	}
}

// ---------------------------------------------------------------------------
// O2 — a JWKS fetch failure with no cached keys maps to 503
// upstream_unavailable, not 401 (an upstream blip must not look like an
// invalid credential and must not be negatively cached in a later phase).
// ---------------------------------------------------------------------------

func TestExternalBearer_JWKSUpstreamFailure_ServiceUnavailable(t *testing.T) {
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
	var errResp ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if errResp.Error.Code != "upstream_unavailable" {
		t.Errorf("error code = %q, want %q", errResp.Error.Code, "upstream_unavailable")
	}
}

// ---------------------------------------------------------------------------
// O3 — Google trust added later via hot reload (no restart) takes effect on
// the very next request. googleTrust reads through cfg.FederationAuth on
// every call, and the validator/resolver are never gated on trust being
// present — server.go's New now always constructs them.
// ---------------------------------------------------------------------------

func TestExternalBearer_HotReload_TrustAddedWithoutRestart(t *testing.T) {
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

	// Nothing stored yet: no trust configured at "startup", mirroring a
	// server started with server.federation.trusted_issuers absent.
	var fedPtr atomic.Pointer[FederationAuthenticator]
	userTokenSvc, err := NewUserTokenService(UserTokenConfig{})
	if err != nil {
		t.Fatalf("NewUserTokenService: %v", err)
	}
	cfg := AuthConfig{
		Mode:            "production",
		UserTokenSvc:    userTokenSvc,
		FederationAuth:  &fedPtr,
		GoogleValidator: newTestValidator(endpoints),
		GoogleResolver:  resolver,
		Logger:          slog.Default(),
	}

	claims := validIDTokenClaims()
	claims["aud"] = externalBearerTestAudience
	token := signIDToken(kp, claims)

	// Before hot reload: no trust configured, falls through unchanged.
	w1, result1 := doExternalBearerRequest(cfg, token)
	if result1.reached {
		t.Fatal("must not authenticate before trust is configured")
	}
	if w1.Code != http.StatusUnauthorized {
		t.Fatalf("before reload: status = %d, want 401", w1.Code)
	}

	// Simulate a hot reload: this is exactly what operational_settings.go's
	// reload path does (federationAuth.Store(newAuth)) after validating the
	// new config — no restart, no re-registration of middleware.
	fedPtr.Store(newGoogleTrustFederationAuth(t, externalBearerTestAudience))

	// Same cfg (same *atomic.Pointer), same request shape: must now succeed.
	w2, result2 := doExternalBearerRequest(cfg, token)
	if w2.Code != http.StatusOK {
		t.Fatalf("after reload: status = %d, want 200: body=%s", w2.Code, w2.Body.String())
	}
	if !result2.reached || result2.authType != AuthTypeExternalBearer {
		t.Fatalf("after reload: expected external-bearer auth, got reached=%v authType=%q", result2.reached, result2.authType)
	}
}

// ---------------------------------------------------------------------------
// O4 — a durable structural check for I4: the new files declare no
// package-level var other than error sentinels (errors.New(...)). Catches a
// future package-level cache/global creeping in — design §7 explicitly drops
// GoogleCloudPlatform/scion#1847's tokenInfoCache global, and Phase 2's
// google_credential_cache.go must stay clean too.
// ---------------------------------------------------------------------------

func TestNoPackageLevelMutableState(t *testing.T) {
	files := []string{
		"auth_external_bearer.go",
		"google_identity_resolver.go",
		"google_credential_cache.go",
		"external_bearer_ratelimit.go",
		"google_sa.go",
		// external_bearer_metrics.go declares only consts, type
		// definitions and interfaces (no package-level var at all), unlike
		// its OTel-backed implementation otel_external_bearer_metrics.go,
		// which — like the pre-existing otel_metrics.go and
		// otel_gcp_metrics.go — uses package-level
		// `var _ Interface = (*Impl)(nil)` compile-time assertions. Those
		// are not mutable state (never written after compilation), but
		// they are not errors.New(...) calls either, so this AST check's
		// simple heuristic would flag them; the two pre-existing
		// otel_*.go files are excluded from this list for the same
		// reason, and this design's otel_external_bearer_metrics.go
		// follows that same precedent.
		"external_bearer_metrics.go",
		// external_bearer_snapshot_metrics.go is likewise consts/types/a
		// struct with only mutex-guarded instance fields (no package-level
		// var at all), so it passes this check the same way.
		"external_bearer_snapshot_metrics.go",
	}
	fset := token.NewFileSet()
	for _, file := range files {
		t.Run(file, func(t *testing.T) {
			f, err := parser.ParseFile(fset, file, nil, parser.AllErrors)
			if err != nil {
				t.Fatalf("parse %s: %v", file, err)
			}
			for _, decl := range f.Decls {
				genDecl, ok := decl.(*ast.GenDecl)
				if !ok || genDecl.Tok != token.VAR {
					continue
				}
				for _, spec := range genDecl.Specs {
					valueSpec, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, name := range valueSpec.Names {
						if name.Name == "_" {
							continue
						}
						if i >= len(valueSpec.Values) {
							t.Errorf("%s: package-level var %q has no initializer (not an error sentinel)", file, name.Name)
							continue
						}
						call, ok := valueSpec.Values[i].(*ast.CallExpr)
						if !ok || !isErrorsNewCall(call) {
							t.Errorf("%s: package-level var %q is not an errors.New(...) sentinel — I4 requires no mutable package-level state", file, name.Name)
						}
					}
				}
			}
		})
	}
}

func isErrorsNewCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	return ok && ident.Name == "errors" && sel.Sel.Name == "New"
}
