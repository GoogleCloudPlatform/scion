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
	"log/slog"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// ---------------------------------------------------------------------------
// The user-domain constraint half of authenticateExternalBearer:
// allowed_domains restricts USER principals to
// specific verified-email domains. A service account is never subject to
// this check — its admission is governed by allowed_gcp_projects instead.
//
// These tests use newGoogleTrustFederationAuthWithDomains /
// newExternalBearerConfigWithDomains, which build a real, validated
// FederationAuthenticator with AllowedDomains set on the Google trust entry
// (mirroring newGoogleTrustFederationAuthWithSA /
// newExternalBearerConfigWithSA in auth_external_bearer_sa_test.go).
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// A user ID token or access token whose verified email domain is not in
// allowed_domains -> 401, validator called, resolver (and thus the Hub
// sign-in policy and provisioning) never reached.
// ---------------------------------------------------------------------------

func TestExternalBearer_UserIDToken_DomainNotAllowed_Unauthorized(t *testing.T) {
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
	cfg := newExternalBearerConfigWithDomains(t, newTestValidator(endpoints), resolver, []string{"example.com"}, nil)

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
	wantBody := wantErrorBody(t, ErrCodeUnauthorized, "invalid external bearer token")
	if !bytes.Equal(w.Body.Bytes(), wantBody) {
		t.Errorf("body = %s, want %s", w.Body.Bytes(), wantBody)
	}
	if len(userStore.users) != 0 {
		t.Errorf("expected no users provisioned, got %d", len(userStore.users))
	}
}

func TestExternalBearer_AccessToken_DomainNotAllowed_Unauthorized(t *testing.T) {
	tokenInfo, userInfo := validAccessTokenEndpoints(externalBearerTestAudience, "google-sub-domain-1", "user@not-listed.com", true)
	endpoints := newTestEndpoints(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), tokenInfo, userInfo)
	defer endpoints.close()

	userStore := newFakeUserStore()
	extStore := newMemExtIDStore()
	resolver := NewGoogleIdentityResolver(userStore, extStore, alwaysAuthorized, nil, slog.Default())
	cfg := newExternalBearerConfigWithDomains(t, newTestValidator(endpoints), resolver, []string{"example.com"}, nil)

	w, result := doExternalBearerRequest(cfg, "opaque-access-token-domain-1")
	if result.reached {
		t.Fatal("handler must not be reached: email domain is not in allowed_domains")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: body=%s", w.Code, w.Body.String())
	}
	wantBody := wantErrorBody(t, ErrCodeUnauthorized, "invalid external bearer token")
	if !bytes.Equal(w.Body.Bytes(), wantBody) {
		t.Errorf("body = %s, want %s", w.Body.Bytes(), wantBody)
	}
	if len(userStore.users) != 0 {
		t.Errorf("expected no users provisioned, got %d", len(userStore.users))
	}
}

// TestExternalBearer_UserIDToken_MixedCaseAllowedDomain_Authenticates proves
// that a mixed-case operator-configured allowed_domains entry still matches
// a mixed-case verified email's domain — containsFold, not an exact string
// match, and neither list nor email is rewritten at config load or
// verification time.
func TestExternalBearer_UserIDToken_MixedCaseAllowedDomain_Authenticates(t *testing.T) {
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
	cfg := newExternalBearerConfigWithDomains(t, newTestValidator(endpoints), resolver, []string{"Example.COM"}, nil)

	claims := validIDTokenClaims()
	claims["aud"] = externalBearerTestAudience
	claims["email"] = "user@EXAMPLE.com"
	claims["hd"] = "EXAMPLE.com"
	token := signIDToken(kp, claims)

	w, result := doExternalBearerRequest(cfg, token)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: body=%s", w.Code, w.Body.String())
	}
	if !result.reached {
		t.Fatal("handler must be reached: mixed-case allowed_domains must still match case-insensitively")
	}
}

// TestExternalBearer_AccessToken_ListedDomain_Authenticates is the access-token
// counterpart of the ID-token mixed-case test above: a listed domain must let
// a user ACCESS token through too, not just an ID token. Without this, a
// mutant that rejects every access token once allowed_domains is set (a
// plausible copy-paste of the ID-token-only domain check) would pass the
// whole suite, even though access tokens are the primary GE credential
// shape.
func TestExternalBearer_AccessToken_ListedDomain_Authenticates(t *testing.T) {
	const email = "user@Example.COM"
	// A Workspace hd claim is required so the resolver's authoritative-domain
	// bootstrap admits a first-time, non-Gmail email (google_identity_resolver.go);
	// otherwise this would 403 there instead of exercising the allowed_domains
	// check this test targets.
	tokenInfo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"azp": externalBearerTestAudience, "aud": externalBearerTestAudience, "sub": "google-sub-domain-listed-1",
			"email": email, "email_verified": true, "hd": "Example.COM", "expires_in": 3600,
		})
	})
	userInfo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"sub": "google-sub-domain-listed-1", "email": email, "email_verified": true, "hd": "Example.COM", "name": "Test User",
		})
	})
	endpoints := newTestEndpoints(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), tokenInfo, userInfo)
	defer endpoints.close()

	userStore := newFakeUserStore()
	extStore := newMemExtIDStore()
	resolver := NewGoogleIdentityResolver(userStore, extStore, alwaysAuthorized, nil, slog.Default())
	cfg := newExternalBearerConfigWithDomains(t, newTestValidator(endpoints), resolver, []string{"example.COM"}, nil)

	w, result := doExternalBearerRequest(cfg, "opaque-access-token-listed-domain-1")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: body=%s", w.Code, w.Body.String())
	}
	if !result.reached {
		t.Fatal("handler must be reached: a listed domain must let an access token through, not just an ID token")
	}
}

// TestExternalBearer_UserIDToken_AllowedDomainsUnset_Authenticates proves
// that an unset allowed_domains imposes no issuer-level domain constraint at
// all — any domain proceeds to the Hub sign-in policy.
func TestExternalBearer_UserIDToken_AllowedDomainsUnset_Authenticates(t *testing.T) {
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
	cfg := newExternalBearerConfigWithDomains(t, newTestValidator(endpoints), resolver, nil, nil)

	claims := validIDTokenClaims()
	claims["aud"] = externalBearerTestAudience
	claims["email"] = "user@anything-goes.com"
	claims["hd"] = "anything-goes.com"
	token := signIDToken(kp, claims)

	w, result := doExternalBearerRequest(cfg, token)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: body=%s", w.Code, w.Body.String())
	}
	if !result.reached {
		t.Fatal("handler must be reached: allowed_domains is unset, so no issuer-level constraint applies")
	}
}

// TestExternalBearer_UserIDToken_ListedDomainSubdomain_Unauthorized proves
// there is no subdomain/suffix matching: a listed "example.com" must not
// admit "sub.example.com".
func TestExternalBearer_UserIDToken_ListedDomainSubdomain_Unauthorized(t *testing.T) {
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

	claims := validIDTokenClaims()
	claims["aud"] = externalBearerTestAudience
	claims["email"] = "user@sub.example.com"
	claims["hd"] = "sub.example.com"
	token := signIDToken(kp, claims)

	w, result := doExternalBearerRequest(cfg, token)
	if result.reached {
		t.Fatal("handler must not be reached: sub.example.com must not match a listed example.com (no subdomain matching)")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: body=%s", w.Code, w.Body.String())
	}
}

// TestExternalBearer_UserIDToken_ListedDomain_SignInPolicyDenies_Forbidden
// proves the Hub sign-in policy still runs, and still governs the outcome,
// after a listed-domain identity passes the issuer-level check: a domain
// constraint is not a substitute for the sign-in policy.
func TestExternalBearer_UserIDToken_ListedDomain_SignInPolicyDenies_Forbidden(t *testing.T) {
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
	// neverAuthorized: the sign-in policy denies every identity, regardless
	// of the issuer-level domain check having already passed.
	resolver := NewGoogleIdentityResolver(userStore, extStore, neverAuthorized, nil, slog.Default())
	cfg := newExternalBearerConfigWithDomains(t, newTestValidator(endpoints), resolver, []string{"example.com"}, nil)

	claims := validIDTokenClaims()
	claims["aud"] = externalBearerTestAudience
	claims["email"] = "user@example.com"
	claims["hd"] = "example.com"
	token := signIDToken(kp, claims)

	w, result := doExternalBearerRequest(cfg, token)
	if result.reached {
		t.Fatal("handler must not be reached: the Hub sign-in policy denies this identity")
	}
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: body=%s", w.Code, w.Body.String())
	}
	wantBody := wantErrorBody(t, ErrCodeForbidden, "access denied")
	if !bytes.Equal(w.Body.Bytes(), wantBody) {
		t.Errorf("body = %s, want %s", w.Body.Bytes(), wantBody)
	}
	if len(userStore.users) != 0 {
		t.Errorf("expected no users provisioned, got %d", len(userStore.users))
	}
}

// ---------------------------------------------------------------------------
// SA unaffected — a service-account ID token whose email domain
// (*.iam.gserviceaccount.com) is not in allowed_domains, but whose project
// is listed in allowed_gcp_projects, still authenticates: allowed_domains
// never applies to a service-account principal.
// ---------------------------------------------------------------------------

func TestExternalBearer_ServiceAccountIDToken_AllowedDomainsDoesNotApply_Authenticates(t *testing.T) {
	kp := newGCVTestKeyPair("test-kid-1")
	endpoints := newSAJWKSEndpoints(kp)
	defer endpoints.close()

	userStore := newFakeUserStore()
	extStore := newMemExtIDStore()
	// neverAuthorized: if the SA were subject to allowed_domains or the Hub
	// sign-in policy, this would be rejected. Success proves the SA branch
	// neither checks allowed_domains nor consults the sign-in policy — only
	// allowed_gcp_projects governs it.
	resolver := NewGoogleIdentityResolver(userStore, extStore, neverAuthorized, nil, slog.Default())
	// allowed_domains is non-empty and deliberately does NOT contain the SA's
	// email domain (my-a2a-project.iam.gserviceaccount.com), so a mutant that
	// applied the domain check to SAs would reject this token.
	cfg := newExternalBearerConfigWithDomains(t, newTestValidator(endpoints), resolver, []string{"example.com"}, []string{"my-a2a-project"})

	claims := serviceAccountIDTokenClaims("worker@my-a2a-project.iam.gserviceaccount.com")
	token := signIDToken(kp, claims)

	w, result := doExternalBearerRequest(cfg, token)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: body=%s", w.Code, w.Body.String())
	}
	if !result.reached {
		t.Fatal("handler must be reached: allowed_domains must never gate a service-account principal")
	}
}

// ---------------------------------------------------------------------------
// domainOf edge cases (table test).
// ---------------------------------------------------------------------------

func TestDomainOf(t *testing.T) {
	tests := []struct {
		name       string
		email      string
		wantDomain string
		wantOK     bool
	}{
		{"simple", "user@example.com", "example.com", true},
		{"uppercase domain and local part lower-cased", "USER@EXAMPLE.COM", "example.com", true},
		{"mixed case domain", "user@Example.Com", "example.com", true},
		{"no at sign", "not-an-email", "", false},
		{"empty string", "", "", false},
		{"multiple at signs", "user@sub@example.com", "", false},
		{"trailing dot on domain", "user@example.com.", "", false},
		{"empty local part", "@example.com", "", false},
		{"empty domain part", "user@", "", false},
		{"only at sign", "@", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			domain, ok := domainOf(tt.email)
			if domain != tt.wantDomain || ok != tt.wantOK {
				t.Errorf("domainOf(%q) = (%q, %v), want (%q, %v)", tt.email, domain, ok, tt.wantDomain, tt.wantOK)
			}
		})
	}
}
