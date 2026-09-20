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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

const testGEClientID = "1234567890-abc.apps.googleusercontent.com"

// fakeTokenInfo is one canned answer from the fake Google tokeninfo server.
type fakeTokenInfo struct {
	status int
	body   map[string]string
}

// newFakeTokenInfoServer serves canned tokeninfo responses keyed by the
// access_token query parameter. Unknown tokens get Google's 400.
func newFakeTokenInfoServer(t *testing.T, answers map[string]fakeTokenInfo) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ans, ok := answers[r.URL.Query().Get("access_token")]
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_token"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(ans.status)
		_ = json.NewEncoder(w).Encode(ans.body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newGoogleIssuerAuth builds a FederationAuthenticator with a single
// accounts.google.com user issuer. The JWKS URL points at an empty key set:
// these tests exercise the opaque access-token path, which never touches JWKS.
func newGoogleIssuerAuth(t *testing.T, issuer config.TrustedIssuerConfig) *atomic.Pointer[FederationAuthenticator] {
	t.Helper()
	jwksSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"keys":[]}`))
	}))
	t.Cleanup(jwksSrv.Close)

	if issuer.IssuerURL == "" {
		issuer.IssuerURL = googleIssuerURL
	}
	if issuer.JWKSURL == "" {
		issuer.JWKSURL = jwksSrv.URL
	}
	auth, err := NewFederationAuthenticator(config.FederationConfig{
		Enabled:        true,
		TrustedIssuers: []config.TrustedIssuerConfig{issuer},
	}, "https://hub.example.com", &http.Client{Timeout: 5 * time.Second}, "dev", slog.Default())
	if err != nil {
		t.Fatalf("NewFederationAuthenticator: %v", err)
	}
	p := &atomic.Pointer[FederationAuthenticator]{}
	p.Store(auth)
	return p
}

// runExternalBearer sends one request with the given bearer token through
// UnifiedAuthMiddleware and reports the status plus the identity/auth type
// the downstream handler observed.
func runExternalBearer(t *testing.T, cfg AuthConfig, token string) (status int, ident UserIdentity, authType string) {
	t.Helper()
	handler := UnifiedAuthMiddleware(cfg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ident = GetUserIdentityFromContext(r.Context())
		authType, _ = r.Context().Value(logging.AuthTypeKey{}).(string)
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec.Code, ident, authType
}

// provisionerRecorder is a stand-in for the Hub's sign-in policy that records
// which emails reached it and returns a canned identity.
type provisionerRecorder struct {
	calls []string
	deny  error
}

func (p *provisionerRecorder) provision(_ context.Context, info *ProxyUserInfo) (UserIdentity, error) {
	p.calls = append(p.calls, info.Email)
	if p.deny != nil {
		return nil, p.deny
	}
	return NewAuthenticatedUser("uid-"+info.Email, info.Email, info.DisplayName, "member", string(ClientTypeWeb)), nil
}

func TestExternalBearer_GoogleAccessToken_Accepted(t *testing.T) {
	const token = googleAccessTokenPrefix + "valid-token"
	tokeninfo := newFakeTokenInfoServer(t, map[string]fakeTokenInfo{
		token: {status: 200, body: map[string]string{
			"aud":            testGEClientID,
			"azp":            testGEClientID,
			"email":          "Outside.Dev@example.com",
			"email_verified": "true",
			"expires_in":     "3000",
		}},
	})
	rec := &provisionerRecorder{}
	cfg := AuthConfig{
		Mode: "production",
		FederationAuth: newGoogleIssuerAuth(t, config.TrustedIssuerConfig{
			IssuerType:       "user",
			ExpectedAudience: testGEClientID,
			AllowedEmails:    []string{"*@example.com"},
		}),
		ExternalUserProvisioner: rec.provision,
		GoogleTokenInfoURL:      tokeninfo.URL,
		Logger:                  slog.Default(),
	}

	status, ident, authType := runExternalBearer(t, cfg, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if ident == nil || ident.Email() != "outside.dev@example.com" {
		t.Fatalf("identity = %v, want provisioned outside.dev@example.com", ident)
	}
	if ident.Role() != "member" {
		t.Errorf("role = %q, want member (policy-assigned, never admin by default)", ident.Role())
	}
	if authType != AuthTypeExternalBearer {
		t.Errorf("auth type = %q, want %q", authType, AuthTypeExternalBearer)
	}
	if len(rec.calls) != 1 || rec.calls[0] != "outside.dev@example.com" {
		t.Errorf("provisioner calls = %v, want exactly one lowercased email", rec.calls)
	}
}

func TestExternalBearer_GoogleAccessToken_RejectsWrongAudience(t *testing.T) {
	// A token minted for a different OAuth client (e.g. gcloud) must never
	// authenticate its owner, even though Google vouches for the email.
	const token = googleAccessTokenPrefix + "other-app-token"
	tokeninfo := newFakeTokenInfoServer(t, map[string]fakeTokenInfo{
		token: {status: 200, body: map[string]string{
			"aud":            "32555940559.apps.googleusercontent.com",
			"azp":            "32555940559.apps.googleusercontent.com",
			"email":          "dev@example.com",
			"email_verified": "true",
			"expires_in":     "3000",
		}},
	})
	rec := &provisionerRecorder{}
	cfg := AuthConfig{
		Mode: "production",
		FederationAuth: newGoogleIssuerAuth(t, config.TrustedIssuerConfig{
			IssuerType:       "user",
			ExpectedAudience: testGEClientID,
		}),
		ExternalUserProvisioner: rec.provision,
		GoogleTokenInfoURL:      tokeninfo.URL,
		Logger:                  slog.Default(),
	}

	status, _, _ := runExternalBearer(t, cfg, token)
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for audience mismatch", status)
	}
	if len(rec.calls) != 0 {
		t.Errorf("provisioner must not run for a rejected token; calls = %v", rec.calls)
	}
}

func TestExternalBearer_GoogleAccessToken_RejectsEmailNotAllowed(t *testing.T) {
	const token = googleAccessTokenPrefix + "stranger-token"
	tokeninfo := newFakeTokenInfoServer(t, map[string]fakeTokenInfo{
		token: {status: 200, body: map[string]string{
			"aud":            testGEClientID,
			"email":          "stranger@elsewhere.org",
			"email_verified": "true",
			"expires_in":     "3000",
		}},
	})
	rec := &provisionerRecorder{}
	cfg := AuthConfig{
		Mode: "production",
		FederationAuth: newGoogleIssuerAuth(t, config.TrustedIssuerConfig{
			IssuerType:       "user",
			ExpectedAudience: testGEClientID,
			AllowedEmails:    []string{"*@example.com", "named.tester@gmail.com"},
		}),
		ExternalUserProvisioner: rec.provision,
		GoogleTokenInfoURL:      tokeninfo.URL,
		Logger:                  slog.Default(),
	}

	status, _, _ := runExternalBearer(t, cfg, token)
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for email outside allowed_emails", status)
	}
	if len(rec.calls) != 0 {
		t.Errorf("provisioner must not run for a disallowed email; calls = %v", rec.calls)
	}
}

func TestExternalBearer_GoogleAccessToken_RejectsUnverifiedEmail(t *testing.T) {
	const token = googleAccessTokenPrefix + "unverified-token"
	tokeninfo := newFakeTokenInfoServer(t, map[string]fakeTokenInfo{
		token: {status: 200, body: map[string]string{
			"aud":            testGEClientID,
			"email":          "dev@example.com",
			"email_verified": "false",
			"expires_in":     "3000",
		}},
	})
	cfg := AuthConfig{
		Mode: "production",
		FederationAuth: newGoogleIssuerAuth(t, config.TrustedIssuerConfig{
			IssuerType:       "user",
			ExpectedAudience: testGEClientID,
		}),
		ExternalUserProvisioner: (&provisionerRecorder{}).provision,
		GoogleTokenInfoURL:      tokeninfo.URL,
		Logger:                  slog.Default(),
	}

	if status, _, _ := runExternalBearer(t, cfg, token); status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for unverified email", status)
	}
}

func TestExternalBearer_GoogleAccessToken_RejectedByGoogle(t *testing.T) {
	// Expired/revoked tokens get HTTP 400 from tokeninfo.
	tokeninfo := newFakeTokenInfoServer(t, nil)
	cfg := AuthConfig{
		Mode: "production",
		FederationAuth: newGoogleIssuerAuth(t, config.TrustedIssuerConfig{
			IssuerType:       "user",
			ExpectedAudience: testGEClientID,
		}),
		ExternalUserProvisioner: (&provisionerRecorder{}).provision,
		GoogleTokenInfoURL:      tokeninfo.URL,
		Logger:                  slog.Default(),
	}

	if status, _, _ := runExternalBearer(t, cfg, googleAccessTokenPrefix+"expired"); status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 when Google rejects the token", status)
	}
}

func TestExternalBearer_GoogleAccessToken_RequiresExpectedAudience(t *testing.T) {
	// Without expected_audience there is nothing to bind the token to, so
	// opaque access tokens are refused outright (fail closed).
	const token = googleAccessTokenPrefix + "no-aud-config"
	tokeninfo := newFakeTokenInfoServer(t, map[string]fakeTokenInfo{
		token: {status: 200, body: map[string]string{
			"aud": testGEClientID, "email": "dev@example.com", "email_verified": "true", "expires_in": "3000",
		}},
	})
	cfg := AuthConfig{
		Mode:                    "production",
		FederationAuth:          newGoogleIssuerAuth(t, config.TrustedIssuerConfig{IssuerType: "user"}),
		ExternalUserProvisioner: (&provisionerRecorder{}).provision,
		GoogleTokenInfoURL:      tokeninfo.URL,
		Logger:                  slog.Default(),
	}

	if status, _, _ := runExternalBearer(t, cfg, token); status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 when issuer has no expected_audience", status)
	}
}

func TestExternalBearer_NoGoogleIssuerConfigured_NotApplicable(t *testing.T) {
	// Federation is configured, but not for accounts.google.com: the token
	// is "not ours" and the base rejection message must be preserved, with
	// no tokeninfo call at all.
	var tokeninfoCalls atomic.Int32
	tokeninfo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokeninfoCalls.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"aud":"` + testGEClientID + `","email":"dev@example.com","email_verified":"true"}`))
	}))
	t.Cleanup(tokeninfo.Close)

	cfg := AuthConfig{
		Mode: "production",
		FederationAuth: newGoogleIssuerAuth(t, config.TrustedIssuerConfig{
			IssuerURL:        "https://hub-remote.example.com",
			IssuerType:       "hub",
			ExpectedAudience: "https://hub.example.com",
		}),
		ExternalUserProvisioner: (&provisionerRecorder{}).provision,
		GoogleTokenInfoURL:      tokeninfo.URL,
		Logger:                  slog.Default(),
	}

	status, _, _ := runExternalBearer(t, cfg, googleAccessTokenPrefix+"some-token")
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
	if tokeninfoCalls.Load() != 0 {
		t.Errorf("tokeninfo was called %d times; must not be consulted without a Google issuer", tokeninfoCalls.Load())
	}
}

func TestExternalBearer_NoFederation_UnchangedRejection(t *testing.T) {
	cfg := AuthConfig{Mode: "production", Logger: slog.Default()}
	handler := UnifiedAuthMiddleware(cfg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	req.Header.Set("Authorization", "Bearer "+googleAccessTokenPrefix+"anything")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err == nil && body.Error.Message != "" &&
		body.Error.Message != "unrecognized token format" {
		t.Errorf("message = %q, want base %q", body.Error.Message, "unrecognized token format")
	}
}

func TestExternalBearer_PolicyDenied_Returns403(t *testing.T) {
	const token = googleAccessTokenPrefix + "not-invited"
	tokeninfo := newFakeTokenInfoServer(t, map[string]fakeTokenInfo{
		token: {status: 200, body: map[string]string{
			"aud": testGEClientID, "email": "dev@example.com", "email_verified": "true", "expires_in": "3000",
		}},
	})
	for _, tc := range []struct {
		name string
		deny error
	}{
		{"access denied by sign-in policy", ErrAccessDenied},
		{"suspended", fmt.Errorf("wrapped: %w", ErrUserSuspended)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := AuthConfig{
				Mode: "production",
				FederationAuth: newGoogleIssuerAuth(t, config.TrustedIssuerConfig{
					IssuerType:       "user",
					ExpectedAudience: testGEClientID,
				}),
				ExternalUserProvisioner: (&provisionerRecorder{deny: tc.deny}).provision,
				GoogleTokenInfoURL:      tokeninfo.URL,
				Logger:                  slog.Default(),
			}
			if status, _, _ := runExternalBearer(t, cfg, token); status != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", status)
			}
		})
	}
}

func TestExternalBearer_TokenInfoCached(t *testing.T) {
	const token = googleAccessTokenPrefix + "cached-token"
	var calls atomic.Int32
	tokeninfo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"aud":"` + testGEClientID + `","email":"dev@example.com","email_verified":"true","expires_in":"3000"}`))
	}))
	t.Cleanup(tokeninfo.Close)

	cfg := AuthConfig{
		Mode: "production",
		FederationAuth: newGoogleIssuerAuth(t, config.TrustedIssuerConfig{
			IssuerType:       "user",
			ExpectedAudience: testGEClientID,
		}),
		ExternalUserProvisioner: (&provisionerRecorder{}).provision,
		GoogleTokenInfoURL:      tokeninfo.URL,
		Logger:                  slog.Default(),
	}
	for i := 0; i < 3; i++ {
		if status, _, _ := runExternalBearer(t, cfg, token); status != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i, status)
		}
	}
	if calls.Load() != 1 {
		t.Errorf("tokeninfo calls = %d, want 1 (subsequent requests served from cache)", calls.Load())
	}
}

// fakeUserStore is an in-memory store.UserStore that only answers
// GetUserByEmail; every other method panics via the nil embedded interface.
type fakeUserStore struct {
	store.UserStore
	users map[string]*store.User
}

func (f *fakeUserStore) GetUserByEmail(_ context.Context, email string) (*store.User, error) {
	if u, ok := f.users[email]; ok {
		return u, nil
	}
	return nil, store.ErrNotFound
}

func TestAuthenticateExternalBearer_NoProvisioner_RequiresExistingActiveUser(t *testing.T) {
	// Without a provisioner (e.g. minimal test servers) the path falls back
	// to "user must already exist and be active" — it never creates users.
	answers := map[string]fakeTokenInfo{}
	for _, email := range []string{"ghost@example.com", "active@example.com", "frozen@example.com"} {
		answers[googleAccessTokenPrefix+"np-"+email] = fakeTokenInfo{status: 200, body: map[string]string{
			"aud": testGEClientID, "email": email, "email_verified": "true", "expires_in": "3000",
		}}
	}
	tokeninfo := newFakeTokenInfoServer(t, answers)
	cfg := AuthConfig{
		FederationAuth: newGoogleIssuerAuth(t, config.TrustedIssuerConfig{
			IssuerType:       "user",
			ExpectedAudience: testGEClientID,
		}),
		GoogleTokenInfoURL: tokeninfo.URL,
		UserStore: &fakeUserStore{users: map[string]*store.User{
			"active@example.com": {ID: "u-active", Email: "active@example.com", Role: "member", Status: store.UserStatusActive},
			"frozen@example.com": {ID: "u-frozen", Email: "frozen@example.com", Role: "member", Status: store.UserStatusSuspended},
		}},
	}

	if _, err := authenticateExternalBearer(context.Background(), googleAccessTokenPrefix+"np-ghost@example.com", cfg); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("unknown user: err = %v, want ErrAccessDenied", err)
	}
	if _, err := authenticateExternalBearer(context.Background(), googleAccessTokenPrefix+"np-frozen@example.com", cfg); !errors.Is(err, ErrUserSuspended) {
		t.Fatalf("suspended user: err = %v, want ErrUserSuspended", err)
	}
	ident, err := authenticateExternalBearer(context.Background(), googleAccessTokenPrefix+"np-active@example.com", cfg)
	if err != nil {
		t.Fatalf("active user: unexpected error %v", err)
	}
	if ident.ID() != "u-active" || ident.Role() != "member" {
		t.Errorf("active user: identity = %s/%s, want u-active/member", ident.ID(), ident.Role())
	}
}
