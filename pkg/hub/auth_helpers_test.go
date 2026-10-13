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
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

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

const constraintAuditAuthTestPath = "/api/v1/admin/access-constraints/constraint-2405/audit"

func assertConstraintAuditNotFound(t *testing.T, got *httptest.ResponseRecorder) {
	t.Helper()
	want := canonicalConstraintAuditNotFound(t)
	if got.Code != want.Code {
		t.Fatalf("status = %d, want %d; body: %s", got.Code, want.Code, got.Body.String())
	}
	if !reflect.DeepEqual(got.Header(), want.Header()) {
		t.Errorf("headers = %#v, want %#v", got.Header(), want.Header())
	}
	if got.Body.String() != want.Body.String() {
		t.Errorf("body = %q, want %q", got.Body.String(), want.Body.String())
	}
}

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

// newExternalBearerConfigWithDomains is newExternalBearerConfig, but the
// Google trust entry carries AllowedDomains (and, optionally,
// AllowedGCPProjects — see newGoogleTrustFederationAuthWithDomains).
func newExternalBearerConfigWithDomains(t *testing.T, validator GoogleCredentialValidator, resolver *GoogleIdentityResolver, allowedDomains, allowedGCPProjects []string) AuthConfig {
	t.Helper()
	userTokenSvc, err := NewUserTokenService(UserTokenConfig{})
	if err != nil {
		t.Fatalf("NewUserTokenService: %v", err)
	}
	fa := newGoogleTrustFederationAuthWithDomains(t, externalBearerTestAudience, allowedDomains, allowedGCPProjects)
	return AuthConfig{
		Mode:            "production",
		UserTokenSvc:    userTokenSvc,
		FederationAuth:  federationAuthPointer(fa),
		GoogleValidator: validator,
		GoogleResolver:  resolver,
		Logger:          slog.Default(),
	}
}

// neverAuthorized always denies. Proves that a service account
// admitted via allowed_gcp_projects bypasses the Hub sign-in policy — the
// project allowlist IS the authorization decision (ResolvePolicy.PreAuthorized).
func neverAuthorized(_ context.Context, _ string) bool { return false }

// saNumericSub is a realistic Google service-account "sub"/"azp" value: a
// large numeric unique ID, matching what the metadata server and
// iamcredentials.generateIdToken actually issue. Fake sub-shaped strings can
// hide a real classification bug, so tests should use a realistic value.
const saNumericSub = "111122223333444455556"

// serviceAccountIDTokenClaims returns claims shaped like a real Google
// service-account ID token: sub == azp (both the SA's own numeric unique
// ID), aud the caller-chosen audience, email/email_verified set.
func serviceAccountIDTokenClaims(email string) map[string]interface{} {
	now := time.Now()
	return map[string]interface{}{
		"iss":            googleIssuerHTTPS,
		"sub":            saNumericSub,
		"azp":            saNumericSub,
		"aud":            externalBearerTestAudience,
		"email":          email,
		"email_verified": true,
		"exp":            now.Add(30 * time.Minute).Unix(),
		"iat":            now.Unix(),
	}
}

func newSAJWKSEndpoints(kp *gcvTestKeyPair) *testEndpoints {
	return newTestEndpoints(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(gcvJWKSJSON(kp))
		}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
	)
}

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
// "golden" property these tests need, without pinning fragile go-jose library
// error-string wording as a literal.
func wantErrorBody(t *testing.T, code, message string) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(ErrorResponse{Error: APIError{Code: code, Message: message}}); err != nil {
		t.Fatalf("encode expected body: %v", err)
	}
	return buf.Bytes()
}

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

// trackingExtIDStore is a minimal ExternalIdentityStore whose
// GetExternalIdentity always fails with a configured error, and which
// records whether CreateExternalIdentity was ever called — it must not be,
// since Resolve should fail before ever reaching the bootstrap path.
type trackingExtIDStore struct {
	getErr       error
	createCalled bool
}

// trackingUserStore is a UserStore whose GetUserByEmail records whether it
// was ever called. Embedding store.UserStore (nil) means any other method
// call panics loudly, which is exactly what we want: Resolve must not reach
// any of them either when GetExternalIdentity faults. CreateUser is
// overridden (rather than left to panic on the nil embed) so that a
// regression which disables the GetExternalIdentity-fault guard fails at
// this test's own getByEmailCalled/createUserCalled assertions instead of a
// SIGSEGV that aborts the whole test binary.
type trackingUserStore struct {
	store.UserStore
	getByEmailCalled bool
	createUserCalled bool
}

// stubUserStore implements the subset of store.UserStore needed by the JWT
// auth middleware. It returns a configurable error from GetUser.
type stubUserStore struct {
	store.UserStore
	getUser func(ctx context.Context, id string) (*store.User, error)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func canonicalConstraintAuditNotFound(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	NotFound(rec, "Access Constraint")
	return rec
}

// newGoogleTrustFederationAuthWithDomains builds a FederationAuthenticator
// whose Google issuer entry carries AllowedDomains and, optionally,
// AllowedGCPProjects (non-nil only when a test also needs the SA branch
// reachable on the same trust entry).
func newGoogleTrustFederationAuthWithDomains(t *testing.T, expectedAudience string, allowedDomains, allowedGCPProjects []string) *FederationAuthenticator {
	t.Helper()
	fedCfg := config.FederationConfig{
		Enabled: true,
		TrustedIssuers: []config.TrustedIssuerConfig{
			{
				IssuerURL:          googleIssuerHTTPS,
				JWKSURL:            "http://unused.invalid/jwks",
				ExpectedAudience:   expectedAudience,
				IssuerType:         "user",
				AllowedDomains:     allowedDomains,
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

// newGoogleTrustFederationAuthWithSA builds a FederationAuthenticator, via
// the real validated NewFederationAuthenticator, whose Google issuer entry
// additionally carries AllowedGCPProjects — the distinct field for
// service-account project admission (not AllowedProjects, which is the
// unrelated, longer-standing hub-federation Scion-project allowlist). Used
// for testing the SA branch of authenticateExternalBearer.
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

func (s *trackingUserStore) GetUserByEmail(_ context.Context, _ string) (*store.User, error) {
	s.getByEmailCalled = true
	return nil, store.ErrNotFound
}

func (s *trackingUserStore) CreateUser(_ context.Context, _ *store.User) error {
	s.createUserCalled = true
	return errors.New("trackingUserStore: CreateUser must not be reached")
}

func (s *stubUserStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	if s.getUser != nil {
		return s.getUser(ctx, id)
	}
	return nil, store.ErrNotFound
}

// newGoogleFederationAuthWithIssuerType is newGoogleTrustFederationAuth with a
// configurable issuer_type, so tests can build a Google trust entry with the
// "wrong" type (e.g. "service_account" or "hub") to prove googleTrust's
// issuer_type: user guard actually matters.
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
