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
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// --- Test helpers ---

// setupFederationTestServer generates an RSA key pair, starts a JWKS test server,
// and returns the private key, server, and kid for signing tokens.
func setupFederationTestServer(t *testing.T) (*rsa.PrivateKey, *httptest.Server, string) {
	t.Helper()
	kid := "test-fed-key-1"
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate RSA key: %v", err)
	}

	jwks := jose.JSONWebKeySet{
		Keys: []jose.JSONWebKey{
			{
				Key:       &privKey.PublicKey,
				KeyID:     kid,
				Algorithm: string(jose.RS256),
				Use:       "sig",
			},
		},
	}
	jwksData, err := json.Marshal(jwks)
	if err != nil {
		t.Fatalf("failed to marshal JWKS: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jwksData)
	}))
	t.Cleanup(srv.Close)

	return privKey, srv, kid
}

// signFederationToken creates a signed RS256 JWT for federation testing.
func signFederationToken(t *testing.T, key *rsa.PrivateKey, kid string, claims federationClaims) string {
	t.Helper()
	signerKey := jose.SigningKey{Algorithm: jose.RS256, Key: key}
	opts := (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", kid)
	signer, err := jose.NewSigner(signerKey, opts)
	if err != nil {
		t.Fatalf("failed to create signer: %v", err)
	}
	raw, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		t.Fatalf("failed to sign JWT: %v", err)
	}
	return raw
}

// validFederationClaims returns a baseline valid claims set for testing.
func validFederationClaims(issuer, audience string) federationClaims {
	now := time.Now()
	return federationClaims{
		Claims: jwt.Claims{
			Issuer:    issuer,
			Subject:   "agent-123",
			Audience:  jwt.Audience{audience},
			IssuedAt:  jwt.NewNumericDate(now.Add(-1 * time.Minute)),
			Expiry:    jwt.NewNumericDate(now.Add(5 * time.Minute)),
			NotBefore: jwt.NewNumericDate(now.Add(-1 * time.Minute)),
		},
		ProjectID: "project-alpha",
		AgentName: "worker-1",
		Ancestry:  []string{"user:alice", "agent:root"},
		RootUser:  "user:alice",
	}
}

// newTestAuthenticator creates a FederationAuthenticator with a single trusted issuer
// pointing at the given JWKS test server.
func newTestAuthenticator(t *testing.T, issuerURL, jwksURL, expectedAudience string) *FederationAuthenticator {
	t.Helper()
	cfg := config.FederationConfig{
		Enabled: true,
		TrustedIssuers: []config.TrustedIssuerConfig{
			{
				IssuerURL:        issuerURL,
				JWKSURL:          jwksURL,
				ExpectedAudience: expectedAudience,
			},
		},
	}
	auth, err := NewFederationAuthenticator(cfg, expectedAudience, &http.Client{Timeout: 5 * time.Second}, "dev", slog.Default())
	if err != nil {
		t.Fatalf("NewFederationAuthenticator failed: %v", err)
	}
	return auth
}

// signGenericToken creates a signed RS256 JWT with arbitrary claims for non-hub token testing.
func signGenericToken(t *testing.T, key *rsa.PrivateKey, kid string, claims interface{}) string {
	t.Helper()
	signerKey := jose.SigningKey{Algorithm: jose.RS256, Key: key}
	opts := (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", kid)
	signer, err := jose.NewSigner(signerKey, opts)
	if err != nil {
		t.Fatalf("failed to create signer: %v", err)
	}
	raw, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		t.Fatalf("failed to sign JWT: %v", err)
	}
	return raw
}

// newTestAuthenticatorWithConfig creates a FederationAuthenticator from a full config.
func newTestAuthenticatorWithConfig(t *testing.T, cfg config.FederationConfig, expectedAudience string) *FederationAuthenticator {
	t.Helper()
	auth, err := NewFederationAuthenticator(cfg, expectedAudience, &http.Client{Timeout: 5 * time.Second}, "dev", slog.Default())
	if err != nil {
		t.Fatalf("NewFederationAuthenticator failed: %v", err)
	}
	return auth
}

// --- Test cases ---

// Test 1: Valid RS256 token from trusted issuer -> success
func TestFederationAuth_ValidToken(t *testing.T) {
	privKey, jwksSrv, kid := setupFederationTestServer(t)
	issuer := "https://hub-a.example.com"
	audience := "https://hub-b.example.com"

	auth := newTestAuthenticator(t, issuer, jwksSrv.URL, audience)

	claims := validFederationClaims(issuer, audience)
	token := signFederationToken(t, privKey, kid, claims)

	identity, err := auth.Authenticate(token)
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}
	if identity == nil {
		t.Fatal("expected identity, got nil")
	}
	agentID, ok := identity.(*FederatedAgentIdentity)
	if !ok {
		t.Fatalf("expected *FederatedAgentIdentity, got %T", identity)
	}
	if agentID.RemoteAgentID() != "agent-123" {
		t.Errorf("expected agent ID 'agent-123', got %q", agentID.RemoteAgentID())
	}
	if identity.IssuerURL() != issuer {
		t.Errorf("expected issuer %q, got %q", issuer, identity.IssuerURL())
	}
	if agentID.RemoteProjectID() != "project-alpha" {
		t.Errorf("expected project 'project-alpha', got %q", agentID.RemoteProjectID())
	}
	if agentID.AgentName() != "worker-1" {
		t.Errorf("expected agent name 'worker-1', got %q", agentID.AgentName())
	}
	if agentID.OriginUserID() != "user:alice" {
		t.Errorf("expected root user 'user:alice', got %q", agentID.OriginUserID())
	}
	if identity.Type() != "federated_agent" {
		t.Errorf("expected type 'federated_agent', got %q", identity.Type())
	}
}

// Test 2: Token from untrusted issuer -> reject
func TestFederationAuth_UntrustedIssuer(t *testing.T) {
	privKey, jwksSrv, kid := setupFederationTestServer(t)
	trustedIssuer := "https://hub-a.example.com"
	audience := "https://hub-b.example.com"

	auth := newTestAuthenticator(t, trustedIssuer, jwksSrv.URL, audience)

	// Sign token with a different issuer
	claims := validFederationClaims("https://evil.example.com", audience)
	token := signFederationToken(t, privKey, kid, claims)

	_, err := auth.Authenticate(token)
	if err == nil {
		t.Fatal("expected error for untrusted issuer, got nil")
	}
	if !strings.Contains(err.Error(), "untrusted issuer") {
		t.Errorf("expected 'untrusted issuer' in error, got: %v", err)
	}
}

// Test 3: Expired token -> reject
func TestFederationAuth_ExpiredToken(t *testing.T) {
	privKey, jwksSrv, kid := setupFederationTestServer(t)
	issuer := "https://hub-a.example.com"
	audience := "https://hub-b.example.com"

	auth := newTestAuthenticator(t, issuer, jwksSrv.URL, audience)

	claims := validFederationClaims(issuer, audience)
	// Set expiry well in the past (beyond clock skew)
	claims.Expiry = jwt.NewNumericDate(time.Now().Add(-5 * time.Minute))
	claims.IssuedAt = jwt.NewNumericDate(time.Now().Add(-10 * time.Minute))
	claims.NotBefore = jwt.NewNumericDate(time.Now().Add(-10 * time.Minute))
	token := signFederationToken(t, privKey, kid, claims)

	_, err := auth.Authenticate(token)
	if err == nil {
		t.Fatal("expected error for expired token, got nil")
	}
	if !strings.Contains(err.Error(), "claims validation failed") {
		t.Errorf("expected 'claims validation failed' in error, got: %v", err)
	}
}

// Test 4: Token with wrong audience -> reject
func TestFederationAuth_WrongAudience(t *testing.T) {
	privKey, jwksSrv, kid := setupFederationTestServer(t)
	issuer := "https://hub-a.example.com"
	audience := "https://hub-b.example.com"

	auth := newTestAuthenticator(t, issuer, jwksSrv.URL, audience)

	claims := validFederationClaims(issuer, "https://wrong-audience.example.com")
	token := signFederationToken(t, privKey, kid, claims)

	_, err := auth.Authenticate(token)
	if err == nil {
		t.Fatal("expected error for wrong audience, got nil")
	}
	if !strings.Contains(err.Error(), "claims validation failed") {
		t.Errorf("expected 'claims validation failed' in error, got: %v", err)
	}
}

// Test 5: Token with project not in allowed_projects -> reject
func TestFederationAuth_ProjectNotAllowed(t *testing.T) {
	privKey, jwksSrv, kid := setupFederationTestServer(t)
	issuer := "https://hub-a.example.com"
	audience := "https://hub-b.example.com"

	cfg := config.FederationConfig{
		Enabled: true,
		TrustedIssuers: []config.TrustedIssuerConfig{
			{
				IssuerURL:        issuer,
				JWKSURL:          jwksSrv.URL,
				ExpectedAudience: audience,
				AllowedProjects:  []string{"project-beta", "project-gamma"},
			},
		},
	}
	auth, err := NewFederationAuthenticator(cfg, audience, &http.Client{Timeout: 5 * time.Second}, "dev", slog.Default())
	if err != nil {
		t.Fatalf("NewFederationAuthenticator failed: %v", err)
	}

	claims := validFederationClaims(issuer, audience)
	claims.ProjectID = "project-alpha" // not in allowed list
	token := signFederationToken(t, privKey, kid, claims)

	_, err = auth.Authenticate(token)
	if err == nil {
		t.Fatal("expected error for disallowed project, got nil")
	}
	if !strings.Contains(err.Error(), "not in allowed_projects") {
		t.Errorf("expected 'not in allowed_projects' in error, got: %v", err)
	}
}

// Test 6: Token with root_user not in allowed_root_users -> reject
func TestFederationAuth_RootUserNotAllowed(t *testing.T) {
	privKey, jwksSrv, kid := setupFederationTestServer(t)
	issuer := "https://hub-a.example.com"
	audience := "https://hub-b.example.com"

	cfg := config.FederationConfig{
		Enabled: true,
		TrustedIssuers: []config.TrustedIssuerConfig{
			{
				IssuerURL:        issuer,
				JWKSURL:          jwksSrv.URL,
				ExpectedAudience: audience,
				AllowedRootUsers: []string{"user:bob"},
			},
		},
	}
	auth, err := NewFederationAuthenticator(cfg, audience, &http.Client{Timeout: 5 * time.Second}, "dev", slog.Default())
	if err != nil {
		t.Fatalf("NewFederationAuthenticator failed: %v", err)
	}

	claims := validFederationClaims(issuer, audience)
	claims.RootUser = "user:alice" // not in allowed list
	token := signFederationToken(t, privKey, kid, claims)

	_, err = auth.Authenticate(token)
	if err == nil {
		t.Fatal("expected error for disallowed root_user, got nil")
	}
	if !strings.Contains(err.Error(), "not in allowed_root_users") {
		t.Errorf("expected 'not in allowed_root_users' in error, got: %v", err)
	}
}

// Test 7: Token with alg: HS256 -> reject (algorithm pinning)
func TestFederationAuth_HS256Rejected(t *testing.T) {
	_, jwksSrv, _ := setupFederationTestServer(t)
	issuer := "https://hub-a.example.com"
	audience := "https://hub-b.example.com"

	auth := newTestAuthenticator(t, issuer, jwksSrv.URL, audience)

	// Create an HS256-signed token
	symmetricKey := []byte("super-secret-key-for-testing-1234")
	signerKey := jose.SigningKey{Algorithm: jose.HS256, Key: symmetricKey}
	opts := (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "hmac-key")
	signer, err := jose.NewSigner(signerKey, opts)
	if err != nil {
		t.Fatalf("failed to create HS256 signer: %v", err)
	}

	claims := validFederationClaims(issuer, audience)
	token, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		t.Fatalf("failed to sign HS256 JWT: %v", err)
	}

	_, err = auth.Authenticate(token)
	if err == nil {
		t.Fatal("expected error for HS256 token, got nil")
	}
	if !strings.Contains(err.Error(), "failed to parse JWT") {
		t.Errorf("expected 'failed to parse JWT' in error, got: %v", err)
	}
}

// Test 8: Unknown kid triggers JWKS refresh -> success after refresh
func TestFederationAuth_UnknownKidTriggersRefresh(t *testing.T) {
	kid1 := "key-1"
	kid2 := "key-2"

	privKey1, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate RSA key 1: %v", err)
	}
	privKey2, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate RSA key 2: %v", err)
	}

	// Start with only key 1 in JWKS
	var currentJWKS atomic.Value
	jwks1 := jose.JSONWebKeySet{
		Keys: []jose.JSONWebKey{
			{Key: &privKey1.PublicKey, KeyID: kid1, Algorithm: string(jose.RS256), Use: "sig"},
		},
	}
	jwks1Data, _ := json.Marshal(jwks1)
	currentJWKS.Store(jwks1Data)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(currentJWKS.Load().([]byte))
	}))
	t.Cleanup(srv.Close)

	issuer := "https://hub-a.example.com"
	audience := "https://hub-b.example.com"
	auth := newTestAuthenticator(t, issuer, srv.URL, audience)

	// First, authenticate with key 1 to populate cache
	claims1 := validFederationClaims(issuer, audience)
	token1 := signFederationToken(t, privKey1, kid1, claims1)
	_, err = auth.Authenticate(token1)
	if err != nil {
		t.Fatalf("first auth failed: %v", err)
	}

	// Now add key 2 to the JWKS
	jwks2 := jose.JSONWebKeySet{
		Keys: []jose.JSONWebKey{
			{Key: &privKey1.PublicKey, KeyID: kid1, Algorithm: string(jose.RS256), Use: "sig"},
			{Key: &privKey2.PublicKey, KeyID: kid2, Algorithm: string(jose.RS256), Use: "sig"},
		},
	}
	jwks2Data, _ := json.Marshal(jwks2)
	currentJWKS.Store(jwks2Data)

	// Reset debounce to allow a fresh fetch
	entry := auth.issuers[issuer]
	entry.cache.mu.Lock()
	entry.cache.lastAttempted = time.Time{}
	entry.cache.mu.Unlock()

	// Authenticate with key 2 — should trigger refresh and succeed
	claims2 := validFederationClaims(issuer, audience)
	claims2.Subject = "agent-456"
	token2 := signFederationToken(t, privKey2, kid2, claims2)
	identity, err := auth.Authenticate(token2)
	if err != nil {
		t.Fatalf("second auth (new kid) failed: %v", err)
	}
	agentID, ok := identity.(*FederatedAgentIdentity)
	if !ok {
		t.Fatalf("expected *FederatedAgentIdentity, got %T", identity)
	}
	if agentID.RemoteAgentID() != "agent-456" {
		t.Errorf("expected agent ID 'agent-456', got %q", agentID.RemoteAgentID())
	}
}

// Test 9: JWKS endpoint down, cached keys -> serve from cache
func TestFederationAuth_JWKSDownCachedKeys(t *testing.T) {
	kid := "cached-key"
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate RSA key: %v", err)
	}

	jwks := jose.JSONWebKeySet{
		Keys: []jose.JSONWebKey{
			{Key: &privKey.PublicKey, KeyID: kid, Algorithm: string(jose.RS256), Use: "sig"},
		},
	}
	jwksData, _ := json.Marshal(jwks)

	var serverUp atomic.Bool
	serverUp.Store(true)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !serverUp.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jwksData)
	}))
	t.Cleanup(srv.Close)

	issuer := "https://hub-a.example.com"
	audience := "https://hub-b.example.com"
	auth := newTestAuthenticator(t, issuer, srv.URL, audience)

	// First auth to populate cache
	claims := validFederationClaims(issuer, audience)
	token := signFederationToken(t, privKey, kid, claims)
	_, err = auth.Authenticate(token)
	if err != nil {
		t.Fatalf("initial auth failed: %v", err)
	}

	// Take JWKS server down
	serverUp.Store(false)

	// A new token with the same kid should still succeed from cache
	// (proactive background refresh will fail silently)
	claims2 := validFederationClaims(issuer, audience)
	claims2.Subject = "agent-789"
	token2 := signFederationToken(t, privKey, kid, claims2)
	identity, err := auth.Authenticate(token2)
	if err != nil {
		t.Fatalf("auth with cached key during outage failed: %v", err)
	}
	agentID, ok := identity.(*FederatedAgentIdentity)
	if !ok {
		t.Fatalf("expected *FederatedAgentIdentity, got %T", identity)
	}
	if agentID.RemoteAgentID() != "agent-789" {
		t.Errorf("expected agent ID 'agent-789', got %q", agentID.RemoteAgentID())
	}
}

// Test 9b: JWKS endpoint down with no cached keys -> reject with clear error
func TestFederationAuth_JWKSDownNoCachedKeys(t *testing.T) {
	kid := "no-cache-key"
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate RSA key: %v", err)
	}

	// JWKS server that always returns HTTP 500
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	issuer := "https://hub-a.example.com"
	audience := "https://hub-b.example.com"
	auth := newTestAuthenticator(t, issuer, srv.URL, audience)

	claims := validFederationClaims(issuer, audience)
	token := signFederationToken(t, privKey, kid, claims)

	_, err = auth.Authenticate(token)
	if err == nil {
		t.Fatal("expected error when JWKS is down with no cached keys, got nil")
	}
	if !strings.Contains(err.Error(), "JWKS key lookup failed") {
		t.Errorf("expected 'JWKS key lookup failed' in error, got: %v", err)
	}
}

// Test 10: Empty sub claim -> reject
func TestFederationAuth_EmptySubject(t *testing.T) {
	privKey, jwksSrv, kid := setupFederationTestServer(t)
	issuer := "https://hub-a.example.com"
	audience := "https://hub-b.example.com"

	auth := newTestAuthenticator(t, issuer, jwksSrv.URL, audience)

	claims := validFederationClaims(issuer, audience)
	claims.Subject = "" // empty sub
	token := signFederationToken(t, privKey, kid, claims)

	_, err := auth.Authenticate(token)
	if err == nil {
		t.Fatal("expected error for empty sub claim, got nil")
	}
	if !strings.Contains(err.Error(), "empty sub claim") {
		t.Errorf("expected 'empty sub claim' in error, got: %v", err)
	}
}

// Test 11: Valid token with default scopes (no per-issuer config) -> gets DefaultFederationScopes
func TestFederationAuth_DefaultScopes(t *testing.T) {
	privKey, jwksSrv, kid := setupFederationTestServer(t)
	issuer := "https://hub-a.example.com"
	audience := "https://hub-b.example.com"

	auth := newTestAuthenticator(t, issuer, jwksSrv.URL, audience)

	claims := validFederationClaims(issuer, audience)
	token := signFederationToken(t, privKey, kid, claims)

	identity, err := auth.Authenticate(token)
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}

	agentID, ok := identity.(*FederatedAgentIdentity)
	if !ok {
		t.Fatalf("expected *FederatedAgentIdentity, got %T", identity)
	}
	scopes := agentID.Scopes()
	if len(scopes) != len(DefaultFederationScopes) {
		t.Fatalf("expected %d default scopes, got %d", len(DefaultFederationScopes), len(scopes))
	}
	for i, s := range DefaultFederationScopes {
		if scopes[i] != s {
			t.Errorf("scope[%d]: expected %q, got %q", i, s, scopes[i])
		}
	}
}

// Test 12: Valid token with per-issuer scopes -> gets configured scopes
func TestFederationAuth_PerIssuerScopes(t *testing.T) {
	privKey, jwksSrv, kid := setupFederationTestServer(t)
	issuer := "https://hub-a.example.com"
	audience := "https://hub-b.example.com"

	cfg := config.FederationConfig{
		Enabled: true,
		TrustedIssuers: []config.TrustedIssuerConfig{
			{
				IssuerURL:        issuer,
				JWKSURL:          jwksSrv.URL,
				ExpectedAudience: audience,
				DefaultScopes:    []string{"agent:status:update", "agent:log:append", "project:secret:read"},
			},
		},
	}
	auth, err := NewFederationAuthenticator(cfg, audience, &http.Client{Timeout: 5 * time.Second}, "dev", slog.Default())
	if err != nil {
		t.Fatalf("NewFederationAuthenticator failed: %v", err)
	}

	claims := validFederationClaims(issuer, audience)
	token := signFederationToken(t, privKey, kid, claims)

	identity, err := auth.Authenticate(token)
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}

	agentID, ok := identity.(*FederatedAgentIdentity)
	if !ok {
		t.Fatalf("expected *FederatedAgentIdentity, got %T", identity)
	}
	scopes := agentID.Scopes()
	if len(scopes) != 3 {
		t.Fatalf("expected 3 scopes, got %d: %v", len(scopes), scopes)
	}
	if !agentID.HasScope(ScopeProjectSecretRead) {
		t.Error("expected ScopeProjectSecretRead to be granted")
	}
}

// Test 13: Multiple issuers -> each validates independently
func TestFederationAuth_MultipleIssuers(t *testing.T) {
	issuerA := "https://hub-a.example.com"
	issuerB := "https://hub-b.example.com"
	audience := "https://hub-c.example.com"

	kidA := "key-a"
	kidB := "key-b"

	privKeyA, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate RSA key A: %v", err)
	}
	privKeyB, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate RSA key B: %v", err)
	}

	jwksA := jose.JSONWebKeySet{
		Keys: []jose.JSONWebKey{
			{Key: &privKeyA.PublicKey, KeyID: kidA, Algorithm: string(jose.RS256), Use: "sig"},
		},
	}
	jwksAData, _ := json.Marshal(jwksA)
	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jwksAData)
	}))
	t.Cleanup(srvA.Close)

	jwksB := jose.JSONWebKeySet{
		Keys: []jose.JSONWebKey{
			{Key: &privKeyB.PublicKey, KeyID: kidB, Algorithm: string(jose.RS256), Use: "sig"},
		},
	}
	jwksBData, _ := json.Marshal(jwksB)
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jwksBData)
	}))
	t.Cleanup(srvB.Close)

	cfg := config.FederationConfig{
		Enabled: true,
		TrustedIssuers: []config.TrustedIssuerConfig{
			{
				IssuerURL:        issuerA,
				JWKSURL:          srvA.URL,
				ExpectedAudience: audience,
			},
			{
				IssuerURL:        issuerB,
				JWKSURL:          srvB.URL,
				ExpectedAudience: audience,
			},
		},
	}
	auth, err := NewFederationAuthenticator(cfg, audience, &http.Client{Timeout: 5 * time.Second}, "dev", slog.Default())
	if err != nil {
		t.Fatalf("NewFederationAuthenticator failed: %v", err)
	}

	// Token from issuer A
	claimsA := validFederationClaims(issuerA, audience)
	claimsA.Subject = "agent-from-a"
	tokenA := signFederationToken(t, privKeyA, kidA, claimsA)
	identityA, err := auth.Authenticate(tokenA)
	if err != nil {
		t.Fatalf("auth for issuer A failed: %v", err)
	}
	if identityA.IssuerURL() != issuerA {
		t.Errorf("expected issuer %q, got %q", issuerA, identityA.IssuerURL())
	}

	// Token from issuer B
	claimsB := validFederationClaims(issuerB, audience)
	claimsB.Subject = "agent-from-b"
	tokenB := signFederationToken(t, privKeyB, kidB, claimsB)
	identityB, err := auth.Authenticate(tokenB)
	if err != nil {
		t.Fatalf("auth for issuer B failed: %v", err)
	}
	if identityB.IssuerURL() != issuerB {
		t.Errorf("expected issuer %q, got %q", issuerB, identityB.IssuerURL())
	}

	// Cross-signing: token signed by A's key but claiming issuer B -> should fail
	claimsCross := validFederationClaims(issuerB, audience)
	tokenCross := signFederationToken(t, privKeyA, kidA, claimsCross)
	_, err = auth.Authenticate(tokenCross)
	if err == nil {
		t.Fatal("expected error for cross-signed token, got nil")
	}
}

// Test 14: NewFederationAuthenticator with HTTP issuer in hosted mode -> error
func TestFederationAuth_HTTPIssuerHostedMode(t *testing.T) {
	cfg := config.FederationConfig{
		Enabled: true,
		TrustedIssuers: []config.TrustedIssuerConfig{
			{
				IssuerURL:        "http://insecure-hub.example.com",
				ExpectedAudience: "https://hub-b.example.com",
			},
		},
	}

	// "hosted" mode (not workstation or dev) should reject HTTP
	_, err := NewFederationAuthenticator(cfg, "https://hub-b.example.com",
		&http.Client{Timeout: 5 * time.Second}, "hosted", slog.Default())
	if err == nil {
		t.Fatal("expected error for HTTP issuer in hosted mode, got nil")
	}
	if !strings.Contains(err.Error(), "not allowed") {
		t.Errorf("expected 'not allowed' in error, got: %v", err)
	}

	// "workstation" mode should allow HTTP
	_, err = NewFederationAuthenticator(cfg, "https://hub-b.example.com",
		&http.Client{Timeout: 5 * time.Second}, "workstation", slog.Default())
	if err != nil {
		t.Errorf("expected HTTP issuer allowed in workstation mode, got error: %v", err)
	}

	// "dev" mode should allow HTTP
	_, err = NewFederationAuthenticator(cfg, "https://hub-b.example.com",
		&http.Client{Timeout: 5 * time.Second}, "dev", slog.Default())
	if err != nil {
		t.Errorf("expected HTTP issuer allowed in dev mode, got error: %v", err)
	}
}

// --- Additional edge case tests ---

// Test: JWKS URL is derived from issuer URL when not explicitly set
func TestFederationAuth_DerivedJWKSURL(t *testing.T) {
	kid := "derived-key"
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate RSA key: %v", err)
	}

	jwks := jose.JSONWebKeySet{
		Keys: []jose.JSONWebKey{
			{Key: &privKey.PublicKey, KeyID: kid, Algorithm: string(jose.RS256), Use: "sig"},
		},
	}
	jwksData, _ := json.Marshal(jwks)

	// Start server that serves JWKS only at the derived path /.well-known/jwks.json
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/jwks.json" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jwksData)
	}))
	t.Cleanup(srv.Close)

	audience := "https://hub-b.example.com"
	cfg := config.FederationConfig{
		Enabled: true,
		TrustedIssuers: []config.TrustedIssuerConfig{
			{
				IssuerURL:        srv.URL, // JWKSURL left empty — derivation adds /.well-known/jwks.json
				ExpectedAudience: audience,
			},
		},
	}
	auth, err := NewFederationAuthenticator(cfg, audience, &http.Client{Timeout: 5 * time.Second}, "dev", slog.Default())
	if err != nil {
		t.Fatalf("NewFederationAuthenticator failed: %v", err)
	}

	claims := validFederationClaims(srv.URL, audience)
	token := signFederationToken(t, privKey, kid, claims)

	identity, err := auth.Authenticate(token)
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}
	if identity == nil {
		t.Fatal("expected identity, got nil")
	}
}

// Test: NewFederationAuthenticator fails when audience cannot be resolved
func TestFederationAuth_NoAudienceError(t *testing.T) {
	cfg := config.FederationConfig{
		Enabled: true,
		TrustedIssuers: []config.TrustedIssuerConfig{
			{
				IssuerURL:        "https://hub-a.example.com",
				ExpectedAudience: "", // empty
			},
		},
	}

	// Both config audience and oidcIssuerURL are empty
	_, err := NewFederationAuthenticator(cfg, "", &http.Client{}, "dev", slog.Default())
	if err == nil {
		t.Fatal("expected error when audience cannot be resolved, got nil")
	}
	if !strings.Contains(err.Error(), "expected_audience is empty") {
		t.Errorf("expected 'expected_audience is empty' in error, got: %v", err)
	}
}

// Test: Token with project in allowed_projects -> success
func TestFederationAuth_ProjectAllowed(t *testing.T) {
	privKey, jwksSrv, kid := setupFederationTestServer(t)
	issuer := "https://hub-a.example.com"
	audience := "https://hub-b.example.com"

	cfg := config.FederationConfig{
		Enabled: true,
		TrustedIssuers: []config.TrustedIssuerConfig{
			{
				IssuerURL:        issuer,
				JWKSURL:          jwksSrv.URL,
				ExpectedAudience: audience,
				AllowedProjects:  []string{"project-alpha", "project-beta"},
			},
		},
	}
	auth, err := NewFederationAuthenticator(cfg, audience, &http.Client{Timeout: 5 * time.Second}, "dev", slog.Default())
	if err != nil {
		t.Fatalf("NewFederationAuthenticator failed: %v", err)
	}

	claims := validFederationClaims(issuer, audience)
	claims.ProjectID = "project-alpha" // in allowed list
	token := signFederationToken(t, privKey, kid, claims)

	identity, err := auth.Authenticate(token)
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}
	agentID, ok := identity.(*FederatedAgentIdentity)
	if !ok {
		t.Fatalf("expected *FederatedAgentIdentity, got %T", identity)
	}
	if agentID.RemoteProjectID() != "project-alpha" {
		t.Errorf("expected project 'project-alpha', got %q", agentID.RemoteProjectID())
	}
}

// --- Service Account tests ---

// SA Test 1: Valid SA token (iss=accounts.google.com, has email+sub) -> FederatedServiceIdentity
func TestFederationAuth_ServiceAccount_ValidToken(t *testing.T) {
	privKey, jwksSrv, kid := setupFederationTestServer(t)
	issuer := "https://accounts.google.com"
	audience := "https://hub-b.example.com"

	cfg := config.FederationConfig{
		Enabled: true,
		TrustedIssuers: []config.TrustedIssuerConfig{
			{
				IssuerURL:        issuer,
				JWKSURL:          jwksSrv.URL,
				ExpectedAudience: audience,
				IssuerType:       "service_account",
			},
		},
	}
	auth := newTestAuthenticatorWithConfig(t, cfg, audience)

	now := time.Now()
	claims := map[string]interface{}{
		"iss":            issuer,
		"sub":            "123456789",
		"aud":            audience,
		"iat":            now.Add(-1 * time.Minute).Unix(),
		"exp":            now.Add(5 * time.Minute).Unix(),
		"nbf":            now.Add(-1 * time.Minute).Unix(),
		"email":          "my-sa@my-project.iam.gserviceaccount.com",
		"email_verified": true,
	}
	token := signGenericToken(t, privKey, kid, claims)

	identity, err := auth.Authenticate(token)
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}

	sid, ok := identity.(*FederatedServiceIdentity)
	if !ok {
		t.Fatalf("expected *FederatedServiceIdentity, got %T", identity)
	}
	if sid.Email() != "my-sa@my-project.iam.gserviceaccount.com" {
		t.Errorf("expected email 'my-sa@my-project.iam.gserviceaccount.com', got %q", sid.Email())
	}
	if sid.Subject() != "123456789" {
		t.Errorf("expected subject '123456789', got %q", sid.Subject())
	}
	if sid.IssuerURL() != issuer {
		t.Errorf("expected issuer %q, got %q", issuer, sid.IssuerURL())
	}
	if sid.Type() != "federated_service" {
		t.Errorf("expected type 'federated_service', got %q", sid.Type())
	}
}

// SA Test 2: SA token missing email -> reject
func TestFederationAuth_ServiceAccount_MissingEmail(t *testing.T) {
	privKey, jwksSrv, kid := setupFederationTestServer(t)
	issuer := "https://accounts.google.com"
	audience := "https://hub-b.example.com"

	cfg := config.FederationConfig{
		Enabled: true,
		TrustedIssuers: []config.TrustedIssuerConfig{
			{
				IssuerURL:        issuer,
				JWKSURL:          jwksSrv.URL,
				ExpectedAudience: audience,
				IssuerType:       "service_account",
			},
		},
	}
	auth := newTestAuthenticatorWithConfig(t, cfg, audience)

	now := time.Now()
	claims := map[string]interface{}{
		"iss": issuer,
		"sub": "123456789",
		"aud": audience,
		"iat": now.Add(-1 * time.Minute).Unix(),
		"exp": now.Add(5 * time.Minute).Unix(),
		"nbf": now.Add(-1 * time.Minute).Unix(),
		// no email claim
	}
	token := signGenericToken(t, privKey, kid, claims)

	_, err := auth.Authenticate(token)
	if err == nil {
		t.Fatal("expected error for missing email, got nil")
	}
	if !strings.Contains(err.Error(), "missing email claim") {
		t.Errorf("expected 'missing email claim' in error, got: %v", err)
	}
}

// SA Test 3: SA token with email not in allowed_emails -> reject
func TestFederationAuth_ServiceAccount_EmailNotAllowed(t *testing.T) {
	privKey, jwksSrv, kid := setupFederationTestServer(t)
	issuer := "https://accounts.google.com"
	audience := "https://hub-b.example.com"

	cfg := config.FederationConfig{
		Enabled: true,
		TrustedIssuers: []config.TrustedIssuerConfig{
			{
				IssuerURL:        issuer,
				JWKSURL:          jwksSrv.URL,
				ExpectedAudience: audience,
				IssuerType:       "service_account",
				AllowedEmails:    []string{"allowed-sa@my-project.iam.gserviceaccount.com"},
			},
		},
	}
	auth := newTestAuthenticatorWithConfig(t, cfg, audience)

	now := time.Now()
	claims := map[string]interface{}{
		"iss":   issuer,
		"sub":   "123456789",
		"aud":   audience,
		"iat":   now.Add(-1 * time.Minute).Unix(),
		"exp":   now.Add(5 * time.Minute).Unix(),
		"nbf":   now.Add(-1 * time.Minute).Unix(),
		"email": "disallowed-sa@other-project.iam.gserviceaccount.com",
	}
	token := signGenericToken(t, privKey, kid, claims)

	_, err := auth.Authenticate(token)
	if err == nil {
		t.Fatal("expected error for disallowed email, got nil")
	}
	if !strings.Contains(err.Error(), "not in allowed_emails") {
		t.Errorf("expected 'not in allowed_emails' in error, got: %v", err)
	}
}

// SA Test 4: SA token with email matching wildcard pattern -> success
func TestFederationAuth_ServiceAccount_WildcardEmailMatch(t *testing.T) {
	privKey, jwksSrv, kid := setupFederationTestServer(t)
	issuer := "https://accounts.google.com"
	audience := "https://hub-b.example.com"

	cfg := config.FederationConfig{
		Enabled: true,
		TrustedIssuers: []config.TrustedIssuerConfig{
			{
				IssuerURL:        issuer,
				JWKSURL:          jwksSrv.URL,
				ExpectedAudience: audience,
				IssuerType:       "service_account",
				AllowedEmails:    []string{"*@my-project.iam.gserviceaccount.com"},
			},
		},
	}
	auth := newTestAuthenticatorWithConfig(t, cfg, audience)

	now := time.Now()
	claims := map[string]interface{}{
		"iss":   issuer,
		"sub":   "123456789",
		"aud":   audience,
		"iat":   now.Add(-1 * time.Minute).Unix(),
		"exp":   now.Add(5 * time.Minute).Unix(),
		"nbf":   now.Add(-1 * time.Minute).Unix(),
		"email": "deploy-bot@my-project.iam.gserviceaccount.com",
	}
	token := signGenericToken(t, privKey, kid, claims)

	identity, err := auth.Authenticate(token)
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}

	sid, ok := identity.(*FederatedServiceIdentity)
	if !ok {
		t.Fatalf("expected *FederatedServiceIdentity, got %T", identity)
	}
	if sid.Email() != "deploy-bot@my-project.iam.gserviceaccount.com" {
		t.Errorf("expected email 'deploy-bot@my-project.iam.gserviceaccount.com', got %q", sid.Email())
	}
}

// SA Test 5: SA token default scopes are empty (zero-trust)
func TestFederationAuth_ServiceAccount_DefaultScopesEmpty(t *testing.T) {
	privKey, jwksSrv, kid := setupFederationTestServer(t)
	issuer := "https://accounts.google.com"
	audience := "https://hub-b.example.com"

	cfg := config.FederationConfig{
		Enabled: true,
		TrustedIssuers: []config.TrustedIssuerConfig{
			{
				IssuerURL:        issuer,
				JWKSURL:          jwksSrv.URL,
				ExpectedAudience: audience,
				IssuerType:       "service_account",
			},
		},
	}
	auth := newTestAuthenticatorWithConfig(t, cfg, audience)

	now := time.Now()
	claims := map[string]interface{}{
		"iss":   issuer,
		"sub":   "123456789",
		"aud":   audience,
		"iat":   now.Add(-1 * time.Minute).Unix(),
		"exp":   now.Add(5 * time.Minute).Unix(),
		"nbf":   now.Add(-1 * time.Minute).Unix(),
		"email": "my-sa@my-project.iam.gserviceaccount.com",
	}
	token := signGenericToken(t, privKey, kid, claims)

	identity, err := auth.Authenticate(token)
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}

	sid, ok := identity.(*FederatedServiceIdentity)
	if !ok {
		t.Fatalf("expected *FederatedServiceIdentity, got %T", identity)
	}
	scopes := sid.Scopes()
	if len(scopes) != 0 {
		t.Errorf("expected empty scopes for SA (zero-trust), got %v", scopes)
	}
}

// --- User tests ---

// User Test 6: Valid user token (Firebase-shaped, has email+sub+name) -> FederatedUserIdentity
func TestFederationAuth_User_ValidToken(t *testing.T) {
	privKey, jwksSrv, kid := setupFederationTestServer(t)
	issuer := "https://securetoken.google.com/my-firebase-project"
	audience := "my-firebase-project"

	cfg := config.FederationConfig{
		Enabled: true,
		TrustedIssuers: []config.TrustedIssuerConfig{
			{
				IssuerURL:        issuer,
				JWKSURL:          jwksSrv.URL,
				ExpectedAudience: audience,
				IssuerType:       "user",
			},
		},
	}
	auth := newTestAuthenticatorWithConfig(t, cfg, audience)

	now := time.Now()
	claims := map[string]interface{}{
		"iss":            issuer,
		"sub":            "abcdef123456",
		"aud":            audience,
		"iat":            now.Add(-1 * time.Minute).Unix(),
		"exp":            now.Add(5 * time.Minute).Unix(),
		"nbf":            now.Add(-1 * time.Minute).Unix(),
		"email":          "user@example.com",
		"email_verified": true,
		"name":           "Test User",
	}
	token := signGenericToken(t, privKey, kid, claims)

	identity, err := auth.Authenticate(token)
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}

	uid, ok := identity.(*FederatedUserIdentity)
	if !ok {
		t.Fatalf("expected *FederatedUserIdentity, got %T", identity)
	}
	if uid.Email() != "user@example.com" {
		t.Errorf("expected email 'user@example.com', got %q", uid.Email())
	}
	if uid.DisplayName() != "Test User" {
		t.Errorf("expected display name 'Test User', got %q", uid.DisplayName())
	}
	if uid.Subject() != "abcdef123456" {
		t.Errorf("expected subject 'abcdef123456', got %q", uid.Subject())
	}
	if uid.IssuerURL() != issuer {
		t.Errorf("expected issuer %q, got %q", issuer, uid.IssuerURL())
	}
	if uid.Type() != "federated_user" {
		t.Errorf("expected type 'federated_user', got %q", uid.Type())
	}
}

// User Test 7: User token with default role -> role is "viewer"
func TestFederationAuth_User_DefaultRole(t *testing.T) {
	privKey, jwksSrv, kid := setupFederationTestServer(t)
	issuer := "https://securetoken.google.com/my-firebase-project"
	audience := "my-firebase-project"

	cfg := config.FederationConfig{
		Enabled: true,
		TrustedIssuers: []config.TrustedIssuerConfig{
			{
				IssuerURL:        issuer,
				JWKSURL:          jwksSrv.URL,
				ExpectedAudience: audience,
				IssuerType:       "user",
				// DefaultRole not set — should default to "viewer"
			},
		},
	}
	auth := newTestAuthenticatorWithConfig(t, cfg, audience)

	now := time.Now()
	claims := map[string]interface{}{
		"iss":   issuer,
		"sub":   "abcdef123456",
		"aud":   audience,
		"iat":   now.Add(-1 * time.Minute).Unix(),
		"exp":   now.Add(5 * time.Minute).Unix(),
		"nbf":   now.Add(-1 * time.Minute).Unix(),
		"email": "user@example.com",
	}
	token := signGenericToken(t, privKey, kid, claims)

	identity, err := auth.Authenticate(token)
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}

	uid, ok := identity.(*FederatedUserIdentity)
	if !ok {
		t.Fatalf("expected *FederatedUserIdentity, got %T", identity)
	}
	if uid.Role() != "viewer" {
		t.Errorf("expected default role 'viewer', got %q", uid.Role())
	}
}

// User Test 8: User token with configured default_role -> role matches config
func TestFederationAuth_User_ConfiguredRole(t *testing.T) {
	privKey, jwksSrv, kid := setupFederationTestServer(t)
	issuer := "https://securetoken.google.com/my-firebase-project"
	audience := "my-firebase-project"

	cfg := config.FederationConfig{
		Enabled: true,
		TrustedIssuers: []config.TrustedIssuerConfig{
			{
				IssuerURL:        issuer,
				JWKSURL:          jwksSrv.URL,
				ExpectedAudience: audience,
				IssuerType:       "user",
				DefaultRole:      "member",
			},
		},
	}
	auth := newTestAuthenticatorWithConfig(t, cfg, audience)

	now := time.Now()
	claims := map[string]interface{}{
		"iss":   issuer,
		"sub":   "abcdef123456",
		"aud":   audience,
		"iat":   now.Add(-1 * time.Minute).Unix(),
		"exp":   now.Add(5 * time.Minute).Unix(),
		"nbf":   now.Add(-1 * time.Minute).Unix(),
		"email": "user@example.com",
	}
	token := signGenericToken(t, privKey, kid, claims)

	identity, err := auth.Authenticate(token)
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}

	uid, ok := identity.(*FederatedUserIdentity)
	if !ok {
		t.Fatalf("expected *FederatedUserIdentity, got %T", identity)
	}
	if uid.Role() != "member" {
		t.Errorf("expected configured role 'member', got %q", uid.Role())
	}
}

func TestFederationAuth_UserAdminDefaultRoleRejected(t *testing.T) {
	_, jwksSrv, _ := setupFederationTestServer(t)
	issuer := "https://securetoken.google.com/my-firebase-project"
	audience := "my-firebase-project"

	cfg := config.FederationConfig{
		Enabled: true,
		TrustedIssuers: []config.TrustedIssuerConfig{{
			IssuerURL:        issuer,
			JWKSURL:          jwksSrv.URL,
			ExpectedAudience: audience,
			IssuerType:       "user",
			DefaultRole:      "admin",
		}},
	}

	_, err := NewFederationAuthenticator(cfg, audience, &http.Client{Timeout: 5 * time.Second}, "dev", slog.Default())
	if err == nil {
		t.Fatal("expected federated user default_role admin to be rejected")
	}
	if !strings.Contains(err.Error(), "default_role \"admin\" is not allowed") {
		t.Errorf("unexpected error: %v", err)
	}
}

// User Test 9: User token with email not in allowed_emails -> reject
func TestFederationAuth_User_EmailNotAllowed(t *testing.T) {
	privKey, jwksSrv, kid := setupFederationTestServer(t)
	issuer := "https://securetoken.google.com/my-firebase-project"
	audience := "my-firebase-project"

	cfg := config.FederationConfig{
		Enabled: true,
		TrustedIssuers: []config.TrustedIssuerConfig{
			{
				IssuerURL:        issuer,
				JWKSURL:          jwksSrv.URL,
				ExpectedAudience: audience,
				IssuerType:       "user",
				AllowedEmails:    []string{"allowed@example.com"},
			},
		},
	}
	auth := newTestAuthenticatorWithConfig(t, cfg, audience)

	now := time.Now()
	claims := map[string]interface{}{
		"iss":   issuer,
		"sub":   "abcdef123456",
		"aud":   audience,
		"iat":   now.Add(-1 * time.Minute).Unix(),
		"exp":   now.Add(5 * time.Minute).Unix(),
		"nbf":   now.Add(-1 * time.Minute).Unix(),
		"email": "disallowed@example.com",
	}
	token := signGenericToken(t, privKey, kid, claims)

	_, err := auth.Authenticate(token)
	if err == nil {
		t.Fatal("expected error for disallowed email, got nil")
	}
	if !strings.Contains(err.Error(), "not in allowed_emails") {
		t.Errorf("expected 'not in allowed_emails' in error, got: %v", err)
	}
}

// User Test 10: User token default scopes are empty (zero-trust)
func TestFederationAuth_User_DefaultScopesEmpty(t *testing.T) {
	privKey, jwksSrv, kid := setupFederationTestServer(t)
	issuer := "https://securetoken.google.com/my-firebase-project"
	audience := "my-firebase-project"

	cfg := config.FederationConfig{
		Enabled: true,
		TrustedIssuers: []config.TrustedIssuerConfig{
			{
				IssuerURL:        issuer,
				JWKSURL:          jwksSrv.URL,
				ExpectedAudience: audience,
				IssuerType:       "user",
			},
		},
	}
	auth := newTestAuthenticatorWithConfig(t, cfg, audience)

	now := time.Now()
	claims := map[string]interface{}{
		"iss":   issuer,
		"sub":   "abcdef123456",
		"aud":   audience,
		"iat":   now.Add(-1 * time.Minute).Unix(),
		"exp":   now.Add(5 * time.Minute).Unix(),
		"nbf":   now.Add(-1 * time.Minute).Unix(),
		"email": "user@example.com",
	}
	token := signGenericToken(t, privKey, kid, claims)

	identity, err := auth.Authenticate(token)
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}

	uid, ok := identity.(*FederatedUserIdentity)
	if !ok {
		t.Fatalf("expected *FederatedUserIdentity, got %T", identity)
	}
	scopes := uid.Scopes()
	if len(scopes) != 0 {
		t.Errorf("expected empty scopes for user (zero-trust), got %v", scopes)
	}
}

// --- Trailing slash normalization tests ---

// Test: Issuer URL trailing slash normalization — config has no slash, token has slash
func TestFederationAuth_IssuerTrailingSlashNormalization(t *testing.T) {
	privKey, jwksSrv, kid := setupFederationTestServer(t)
	// Config issuer has no trailing slash
	configIssuer := "https://hub-a.example.com"
	// Token issuer has trailing slash
	tokenIssuer := "https://hub-a.example.com/"
	audience := "https://hub-b.example.com"

	auth := newTestAuthenticator(t, configIssuer, jwksSrv.URL, audience)

	claims := validFederationClaims(tokenIssuer, audience)
	token := signFederationToken(t, privKey, kid, claims)

	identity, err := auth.Authenticate(token)
	if err != nil {
		t.Fatalf("expected success despite trailing slash difference, got error: %v", err)
	}
	if identity == nil {
		t.Fatal("expected identity, got nil")
	}
	agentID, ok := identity.(*FederatedAgentIdentity)
	if !ok {
		t.Fatalf("expected *FederatedAgentIdentity, got %T", identity)
	}
	if agentID.RemoteAgentID() != "agent-123" {
		t.Errorf("expected agent ID 'agent-123', got %q", agentID.RemoteAgentID())
	}
}

// Test: Issuer URL trailing slash normalization — config has slash, token has no slash
func TestFederationAuth_IssuerTrailingSlashNormalization_Reverse(t *testing.T) {
	privKey, jwksSrv, kid := setupFederationTestServer(t)
	// Config issuer has trailing slash
	configIssuer := "https://hub-a.example.com/"
	// Token issuer has no trailing slash
	tokenIssuer := "https://hub-a.example.com"
	audience := "https://hub-b.example.com"

	auth := newTestAuthenticator(t, configIssuer, jwksSrv.URL, audience)

	claims := validFederationClaims(tokenIssuer, audience)
	token := signFederationToken(t, privKey, kid, claims)

	identity, err := auth.Authenticate(token)
	if err != nil {
		t.Fatalf("expected success despite trailing slash difference, got error: %v", err)
	}
	if identity == nil {
		t.Fatal("expected identity, got nil")
	}
}

// --- Email matching tests ---

// Email Test 11: Exact match works
func TestMatchesAllowedEmails_ExactMatch(t *testing.T) {
	patterns := []string{"user@example.com", "admin@corp.com"}
	if !matchesAllowedEmails(patterns, "user@example.com") {
		t.Error("expected exact match for user@example.com")
	}
	if !matchesAllowedEmails(patterns, "admin@corp.com") {
		t.Error("expected exact match for admin@corp.com")
	}
	if matchesAllowedEmails(patterns, "other@example.com") {
		t.Error("expected no match for other@example.com")
	}
}

// Email Test 12: Wildcard *@example.com matches user@example.com
func TestMatchesAllowedEmails_WildcardMatch(t *testing.T) {
	patterns := []string{"*@example.com"}
	if !matchesAllowedEmails(patterns, "user@example.com") {
		t.Error("expected wildcard match for user@example.com")
	}
	if !matchesAllowedEmails(patterns, "admin@example.com") {
		t.Error("expected wildcard match for admin@example.com")
	}
}

// Email Test 13: Wildcard *@example.com does NOT match user@other.com
func TestMatchesAllowedEmails_WildcardNoMatch(t *testing.T) {
	patterns := []string{"*@example.com"}
	if matchesAllowedEmails(patterns, "user@other.com") {
		t.Error("expected no match for user@other.com with pattern *@example.com")
	}
	if matchesAllowedEmails(patterns, "user@example.com.evil.com") {
		t.Error("expected no match for user@example.com.evil.com with pattern *@example.com")
	}
}

// Email Test 14: Empty allowed_emails list -> all accepted
func TestMatchesAllowedEmails_EmptyListAcceptsAll(t *testing.T) {
	// When AllowedEmails is empty, the caller (Authenticate) skips the check entirely.
	// But matchesAllowedEmails with empty list should return false (defense-in-depth).
	patterns := []string{}
	if matchesAllowedEmails(patterns, "anyone@example.com") {
		t.Error("expected empty pattern list to not match anything (caller should skip)")
	}
}

// Email Test 15: Case-insensitive exact match
func TestMatchesAllowedEmails_CaseInsensitiveExact(t *testing.T) {
	patterns := []string{"User@Example.COM"}
	if !matchesAllowedEmails(patterns, "user@example.com") {
		t.Error("expected case-insensitive match for user@example.com against User@Example.COM")
	}
}

// Email Test 16: Case-insensitive wildcard match
func TestMatchesAllowedEmails_CaseInsensitiveWildcard(t *testing.T) {
	patterns := []string{"*@Example.COM"}
	if !matchesAllowedEmails(patterns, "user@example.com") {
		t.Error("expected case-insensitive wildcard match for user@example.com against *@Example.COM")
	}
}

// --- Hub backward compatibility ---

// Hub Test 15: Existing hub issuer with no issuer_type field -> works as before (defaults to "hub")
func TestFederationAuth_HubBackwardCompat_NoIssuerType(t *testing.T) {
	privKey, jwksSrv, kid := setupFederationTestServer(t)
	issuer := "https://hub-a.example.com"
	audience := "https://hub-b.example.com"

	// No IssuerType set — should default to hub behavior
	auth := newTestAuthenticator(t, issuer, jwksSrv.URL, audience)

	claims := validFederationClaims(issuer, audience)
	token := signFederationToken(t, privKey, kid, claims)

	identity, err := auth.Authenticate(token)
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}

	agentID, ok := identity.(*FederatedAgentIdentity)
	if !ok {
		t.Fatalf("expected *FederatedAgentIdentity (hub default), got %T", identity)
	}
	if agentID.RemoteAgentID() != "agent-123" {
		t.Errorf("expected agent ID 'agent-123', got %q", agentID.RemoteAgentID())
	}
	if agentID.RemoteProjectID() != "project-alpha" {
		t.Errorf("expected project 'project-alpha', got %q", agentID.RemoteProjectID())
	}
	if agentID.AgentName() != "worker-1" {
		t.Errorf("expected agent name 'worker-1', got %q", agentID.AgentName())
	}
	// Verify default scopes are applied (hub defaults)
	scopes := agentID.Scopes()
	if len(scopes) != len(DefaultFederationScopes) {
		t.Errorf("expected %d default scopes, got %d", len(DefaultFederationScopes), len(scopes))
	}
}

// --- OIDC Discovery integration tests ---

// Discovery Test 1: OIDC discovery resolves JWKS URL for non-hub issuer
func TestFederationAuth_OIDCDiscovery_ResolvesJWKSURL(t *testing.T) {
	kid := "discovery-key"
	audience := "https://hub-b.example.com"

	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate RSA key: %v", err)
	}

	// JWKS server
	jwks := jose.JSONWebKeySet{
		Keys: []jose.JSONWebKey{
			{Key: &privKey.PublicKey, KeyID: kid, Algorithm: string(jose.RS256), Use: "sig"},
		},
	}
	jwksData, _ := json.Marshal(jwks)
	jwksSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jwksData)
	}))
	t.Cleanup(jwksSrv.Close)

	// Issuer server with OIDC discovery that points to the JWKS server
	issuerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/openid-configuration" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jwks_uri": "` + jwksSrv.URL + `"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(issuerSrv.Close)

	cfg := config.FederationConfig{
		Enabled: true,
		TrustedIssuers: []config.TrustedIssuerConfig{
			{
				IssuerURL:        issuerSrv.URL,
				JWKSURL:          "", // empty — discovery should resolve
				ExpectedAudience: audience,
				IssuerType:       "service_account",
			},
		},
	}

	// NewFederationAuthenticator should succeed (discovery resolves JWKS URL)
	auth, err := NewFederationAuthenticator(cfg, audience,
		&http.Client{Timeout: 5 * time.Second}, "dev", slog.Default())
	if err != nil {
		t.Fatalf("NewFederationAuthenticator should succeed with discovery, got error: %v", err)
	}

	// Authenticate a valid token to prove the discovered URL was used
	now := time.Now()
	claims := map[string]interface{}{
		"iss":   issuerSrv.URL,
		"sub":   "discovery-test-subject",
		"aud":   audience,
		"iat":   now.Add(-1 * time.Minute).Unix(),
		"exp":   now.Add(5 * time.Minute).Unix(),
		"nbf":   now.Add(-1 * time.Minute).Unix(),
		"email": "test-sa@project.iam.gserviceaccount.com",
	}
	token := signGenericToken(t, privKey, kid, claims)

	identity, err := auth.Authenticate(token)
	if err != nil {
		t.Fatalf("expected authentication success, got error: %v", err)
	}

	sid, ok := identity.(*FederatedServiceIdentity)
	if !ok {
		t.Fatalf("expected *FederatedServiceIdentity, got %T", identity)
	}
	if sid.Email() != "test-sa@project.iam.gserviceaccount.com" {
		t.Errorf("expected email 'test-sa@project.iam.gserviceaccount.com', got %q", sid.Email())
	}
}

// Discovery Test 2: OIDC discovery failure when no jwks_url configured and no discovery endpoint
func TestFederationAuth_OIDCDiscovery_FailureNoEndpoint(t *testing.T) {
	audience := "https://hub-b.example.com"

	// Issuer server with NO discovery endpoint
	issuerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(issuerSrv.Close)

	cfg := config.FederationConfig{
		Enabled: true,
		TrustedIssuers: []config.TrustedIssuerConfig{
			{
				IssuerURL:        issuerSrv.URL,
				JWKSURL:          "", // empty — discovery will fail
				ExpectedAudience: audience,
				IssuerType:       "service_account",
			},
		},
	}

	// NewFederationAuthenticator should fail — no JWKS URL and discovery fails
	_, err := NewFederationAuthenticator(cfg, audience,
		&http.Client{Timeout: 5 * time.Second}, "dev", slog.Default())
	if err == nil {
		t.Fatal("expected error when JWKS URL not configured and discovery fails, got nil")
	}
	if !strings.Contains(err.Error(), "OIDC discovery failed") {
		t.Errorf("expected 'OIDC discovery failed' in error, got: %v", err)
	}
}

// ============================================================================
// Phase 1G Federation Fix Tests
// ============================================================================

// Federation Fix 1: ancestry[0] disagrees with root_user → rejection
func TestExtractHubClaims_AncestryDisagreesWithRootUser(t *testing.T) {
	claims := &federationClaims{
		Claims: jwt.Claims{
			Subject: "agent-1",
			Issuer:  "https://remote.example.com",
		},
		ProjectID: "proj-1",
		AgentName: "test-agent",
		RootUser:  "user:bob",
		Ancestry:  []string{"user:alice"}, // disagrees with root_user
	}

	_, err := extractHubClaims(claims, config.TrustedIssuerConfig{}, nil)
	if err == nil {
		t.Fatal("expected error when ancestry[0] disagrees with root_user, got nil")
	}
	if !strings.Contains(err.Error(), "disagrees with root_user") {
		t.Errorf("expected error mentioning 'disagrees with root_user', got: %v", err)
	}
}

// Federation Fix 2: per-element ancestry validation against allowed_root_users
func TestFederationAuth_AncestryPerElementRejection(t *testing.T) {
	privKey, jwksSrv, kid := setupFederationTestServer(t)
	issuer := jwksSrv.URL // Use the test server URL as issuer
	audience := "https://hub.example.com"

	cfg := config.FederationConfig{
		Enabled: true,
		TrustedIssuers: []config.TrustedIssuerConfig{
			{
				IssuerURL:        issuer,
				JWKSURL:          jwksSrv.URL,
				ExpectedAudience: audience,
				IssuerType:       "hub",
				AllowedRootUsers: []string{"user:bob"}, // bob is allowed, eve is NOT
			},
		},
	}
	auth := newTestAuthenticatorWithConfig(t, cfg, audience)

	now := time.Now()
	claims := map[string]interface{}{
		"iss":        issuer,
		"sub":        "agent-fed-2",
		"aud":        audience,
		"iat":        now.Add(-1 * time.Minute).Unix(),
		"exp":        now.Add(5 * time.Minute).Unix(),
		"nbf":        now.Add(-1 * time.Minute).Unix(),
		"project_id": "proj-1",
		"agent_name": "test-agent",
		"root_user":  "user:bob",                       // in allowed list
		"ancestry":   []string{"user:bob", "user:eve"}, // user:eve NOT in allowed list
	}
	token := signGenericToken(t, privKey, kid, claims)

	_, err := auth.Authenticate(token)
	if err == nil {
		t.Fatal("expected rejection when ancestry element not in allowed_root_users, got nil")
	}
	if !strings.Contains(err.Error(), "ancestry element") {
		t.Errorf("expected error mentioning 'ancestry element', got: %v", err)
	}
}
