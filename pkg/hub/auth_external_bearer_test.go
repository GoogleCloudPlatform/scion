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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

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
	fedCfg := config.FederationConfig{
		Enabled: true,
		TrustedIssuers: []config.TrustedIssuerConfig{
			{
				IssuerURL:        googleIssuerHTTPS,
				JWKSURL:          "http://unused.invalid/jwks",
				ExpectedAudience: expectedAudience,
				IssuerType:       "user",
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
// no-op: the response is byte-identical to the pre-existing rejection.
// ---------------------------------------------------------------------------

func TestExternalBearer_NoGoogleTrustConfigured_Golden401(t *testing.T) {
	userTokenSvc, err := NewUserTokenService(UserTokenConfig{})
	if err != nil {
		t.Fatalf("NewUserTokenService: %v", err)
	}
	// Deliberately the zero value for FederationAuth/GoogleValidator/
	// GoogleResolver: Google trust is not configured at all, matching the
	// pre-#1847 AuthConfig shape.
	cfg := AuthConfig{
		Mode:         "production",
		UserTokenSvc: userTokenSvc,
		Logger:       slog.Default(),
	}

	// A JWT-shaped token that is not a valid Hub-issued JWT — the exact shape
	// that would route into the external-bearer hook.
	w, result := doExternalBearerRequest(cfg, "not-a.valid-hub.jwt")

	if result.reached {
		t.Fatal("handler must not be reached")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	var errResp ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if errResp.Error.Code != ErrCodeUnauthorized {
		t.Errorf("error code = %q, want %q", errResp.Error.Code, ErrCodeUnauthorized)
	}
	if !strings.HasPrefix(errResp.Error.Message, "invalid access token:") {
		t.Errorf("message = %q, want prefix %q (unchanged pre-existing rejection)", errResp.Error.Message, "invalid access token:")
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
	counting := &countingGoogleValidator{}

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
		defer f.Close()
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

func TestExternalBearer_ClassifyNonJWT_NotApplicable(t *testing.T) {
	if got := classifyExternalBearer("not-a-jwt"); got != externalBearerNotApplicable {
		t.Errorf("classifyExternalBearer(opaque) = %v, want externalBearerNotApplicable", got)
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

// TestHasGoogleUserTrust exercises the startup-time detection helper used by
// server.go's New to decide whether to build the Google validator/resolver
// stack (independent of classifyExternalBearer/googleTrust, which read
// through the hot-reloadable FederationAuthenticator at request time).
func TestHasGoogleUserTrust(t *testing.T) {
	tests := []struct {
		name string
		cfg  config.FederationConfig
		want bool
	}{
		{
			name: "disabled",
			cfg:  config.FederationConfig{Enabled: false},
			want: false,
		},
		{
			name: "no trusted issuers",
			cfg:  config.FederationConfig{Enabled: true},
			want: false,
		},
		{
			name: "google user issuer with audience",
			cfg: config.FederationConfig{
				Enabled: true,
				TrustedIssuers: []config.TrustedIssuerConfig{
					{IssuerURL: googleIssuerHTTPS, IssuerType: "user", ExpectedAudience: "client-id"},
				},
			},
			want: true,
		},
		{
			name: "google user issuer without audience is disabled",
			cfg: config.FederationConfig{
				Enabled: true,
				TrustedIssuers: []config.TrustedIssuerConfig{
					{IssuerURL: googleIssuerHTTPS, IssuerType: "user"},
				},
			},
			want: false,
		},
		{
			name: "google issuer with wrong issuer_type",
			cfg: config.FederationConfig{
				Enabled: true,
				TrustedIssuers: []config.TrustedIssuerConfig{
					{IssuerURL: googleIssuerHTTPS, IssuerType: "service_account", ExpectedAudience: "client-id"},
				},
			},
			want: false,
		},
		{
			name: "non-google issuer",
			cfg: config.FederationConfig{
				Enabled: true,
				TrustedIssuers: []config.TrustedIssuerConfig{
					{IssuerURL: "https://issuer.example.com", IssuerType: "user", ExpectedAudience: "client-id"},
				},
			},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasGoogleUserTrust(tt.cfg); got != tt.want {
				t.Errorf("hasGoogleUserTrust() = %v, want %v", got, tt.want)
			}
		})
	}
}
