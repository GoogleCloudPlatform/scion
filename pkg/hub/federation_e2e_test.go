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
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// newFedAuthPointer creates an atomic.Pointer pre-loaded with the given authenticator.
func newFedAuthPointer(auth *FederationAuthenticator) *atomic.Pointer[FederationAuthenticator] {
	p := &atomic.Pointer[FederationAuthenticator]{}
	if auth != nil {
		p.Store(auth)
	}
	return p
}

// e2eResponse is the JSON response returned by the E2E test endpoint handler
// to verify identity details from the full authentication flow.
type e2eResponse struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	IssuerURL string `json:"issuer_url"`
	AgentID   string `json:"agent_id"`
	AgentName string `json:"agent_name"`
	ProjectID string `json:"project_id"`
}

// setupE2EServer sets up the full Hub B middleware stack:
//   - FederationAuthenticator trusting Hub A
//   - UnifiedAuthMiddleware with the authenticator
//   - RequireFederationAccess(requiredScope) on a test endpoint
//   - A handler that returns identity details as JSON
//
// It returns the test server, the Hub A private key and kid for signing tokens,
// the Hub A issuer URL, and the Hub B audience.
func setupE2EServer(t *testing.T, requiredScope AgentTokenScope) (
	server *httptest.Server,
	hubAKey *rsa.PrivateKey,
	hubAIssuer string,
	hubBAudience string,
	kid string,
) {
	t.Helper()

	// --- Hub A's OIDC infrastructure ---
	kid = "hub-a-e2e-key"
	hubAIssuer = "https://hub-a.example.com"
	hubBAudience = "https://hub-b.example.com"

	hubAKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate RSA key: %v", err)
	}

	jwks := jose.JSONWebKeySet{
		Keys: []jose.JSONWebKey{
			{
				Key:       &hubAKey.PublicKey,
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

	jwksSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jwksData)
	}))
	t.Cleanup(jwksSrv.Close)

	// --- Hub B's middleware stack ---
	fedCfg := config.FederationConfig{
		Enabled: true,
		TrustedIssuers: []config.TrustedIssuerConfig{
			{
				IssuerURL:        hubAIssuer,
				JWKSURL:          jwksSrv.URL,
				ExpectedAudience: hubBAudience,
			},
		},
	}
	authenticator, err := NewFederationAuthenticator(fedCfg, hubBAudience,
		&http.Client{Timeout: 5 * time.Second}, "dev", slog.Default())
	if err != nil {
		t.Fatalf("NewFederationAuthenticator failed: %v", err)
	}

	authCfg := AuthConfig{
		Mode:           "production",
		FederationAuth: newFedAuthPointer(authenticator),
		Debug:          true,
		Logger:         slog.Default(),
	}

	// Build the handler chain: auth middleware -> access control -> handler
	identityHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity := GetAgentIdentityFromContext(r.Context())
		if identity == nil {
			http.Error(w, "no agent identity", http.StatusInternalServerError)
			return
		}

		fed, ok := identity.(*FederatedAgentIdentity)
		if !ok {
			http.Error(w, "not a federated identity", http.StatusInternalServerError)
			return
		}

		resp := e2eResponse{
			ID:        fed.ID(),
			Type:      fed.Type(),
			IssuerURL: fed.IssuerURL(),
			AgentID:   fed.RemoteAgentID(),
			AgentName: fed.AgentName(),
			ProjectID: fed.ProjectID(),
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)
	})

	accessMiddleware := RequireFederationAccess(requiredScope)
	authMiddleware := UnifiedAuthMiddleware(authCfg)
	handler := authMiddleware(accessMiddleware(identityHandler))

	server = httptest.NewServer(handler)
	t.Cleanup(server.Close)

	return server, hubAKey, hubAIssuer, hubBAudience, kid
}

// TestFederationE2E_FullSuccessPath tests the complete federation flow:
// Hub A issues a token -> Hub B validates it via middleware -> access control passes -> 200.
func TestFederationE2E_FullSuccessPath(t *testing.T) {
	server, hubAKey, hubAIssuer, hubBAudience, kid := setupE2EServer(t, ScopeAgentStatusUpdate)

	claims := validFederationClaims(hubAIssuer, hubBAudience)
	claims.Subject = "e2e-agent-1"
	claims.AgentName = "e2e-worker"
	claims.ProjectID = "e2e-project"
	// Phase 1G fix 1: ancestry[0] must agree with root_user.
	claims.RootUser = "user:e2e-admin"
	claims.Ancestry = []string{"user:e2e-admin"}
	token := signFederationToken(t, hubAKey, kid, claims)

	req, err := http.NewRequest(http.MethodGet, server.URL+"/api/v1/test", nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	req.Header.Set(FederationTokenHeader, token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	var body e2eResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if body.AgentID != "e2e-agent-1" {
		t.Errorf("expected agent_id 'e2e-agent-1', got %q", body.AgentID)
	}
	if body.Type != "federated_agent" {
		t.Errorf("expected type 'federated_agent', got %q", body.Type)
	}
	if body.IssuerURL != hubAIssuer {
		t.Errorf("expected issuer_url %q, got %q", hubAIssuer, body.IssuerURL)
	}
	if body.AgentName != "e2e-worker" {
		t.Errorf("expected agent_name 'e2e-worker', got %q", body.AgentName)
	}
	// ProjectID should be empty — federated agents have no local project binding
	if body.ProjectID != "" {
		t.Errorf("expected empty project_id, got %q", body.ProjectID)
	}
}

// TestFederationE2E_ScopeDenied tests that a valid token without the required
// scope is rejected with 403.
func TestFederationE2E_ScopeDenied(t *testing.T) {
	// Server requires ScopeProjectSecretRead, but default scopes only include
	// ScopeAgentStatusUpdate and ScopeAgentLogAppend.
	server, hubAKey, hubAIssuer, hubBAudience, kid := setupE2EServer(t, ScopeProjectSecretRead)

	claims := validFederationClaims(hubAIssuer, hubBAudience)
	claims.Subject = "e2e-agent-2"
	token := signFederationToken(t, hubAKey, kid, claims)

	req, err := http.NewRequest(http.MethodGet, server.URL+"/api/v1/test", nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	req.Header.Set(FederationTokenHeader, token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected status 403, got %d", resp.StatusCode)
	}
}

// TestFederationE2E_UntrustedIssuer tests that a token from an issuer not in
// Hub B's trusted list is rejected with 401.
func TestFederationE2E_UntrustedIssuer(t *testing.T) {
	server, _, _, hubBAudience, _ := setupE2EServer(t, ScopeAgentStatusUpdate)

	// Generate a separate key for the untrusted issuer
	untrustedKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate untrusted RSA key: %v", err)
	}

	untrustedIssuer := "https://evil-hub.example.com"
	claims := validFederationClaims(untrustedIssuer, hubBAudience)
	claims.Subject = "evil-agent"
	token := signFederationToken(t, untrustedKey, "evil-key", claims)

	req, err := http.NewRequest(http.MethodGet, server.URL+"/api/v1/test", nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	req.Header.Set(FederationTokenHeader, token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected status 401, got %d", resp.StatusCode)
	}
}

// TestFederationE2E_ExpiredToken tests that an expired token is rejected with 401.
func TestFederationE2E_ExpiredToken(t *testing.T) {
	server, hubAKey, hubAIssuer, hubBAudience, kid := setupE2EServer(t, ScopeAgentStatusUpdate)

	claims := validFederationClaims(hubAIssuer, hubBAudience)
	claims.Subject = "expired-agent"
	claims.Expiry = jwt.NewNumericDate(time.Now().Add(-10 * time.Minute))
	claims.IssuedAt = jwt.NewNumericDate(time.Now().Add(-15 * time.Minute))
	claims.NotBefore = jwt.NewNumericDate(time.Now().Add(-15 * time.Minute))
	token := signFederationToken(t, hubAKey, kid, claims)

	req, err := http.NewRequest(http.MethodGet, server.URL+"/api/v1/test", nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	req.Header.Set(FederationTokenHeader, token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected status 401, got %d", resp.StatusCode)
	}
}

// TestFederationE2E_NoFederationHeader tests that a request without the
// federation header is rejected with 401 (no agent identity for access control).
func TestFederationE2E_NoFederationHeader(t *testing.T) {
	server, _, _, _, _ := setupE2EServer(t, ScopeAgentStatusUpdate)

	req, err := http.NewRequest(http.MethodGet, server.URL+"/api/v1/test", nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	// No federation header set

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected status 401, got %d", resp.StatusCode)
	}
}

// --- Non-Hub Issuer E2E Tests ---

// setupNonHubE2EServer creates a test server with federation auth middleware for non-hub issuers.
// Unlike setupE2EServer, this does NOT use RequireFederationAccess (which requires AgentIdentity),
// since non-hub identities (service accounts, users) do not implement AgentIdentity.
// The handler extracts identity via GetIdentityFromContext and returns details as JSON.
func setupNonHubE2EServer(t *testing.T, fedCfg config.FederationConfig, audience string) *httptest.Server {
	t.Helper()

	authenticator, err := NewFederationAuthenticator(fedCfg, audience,
		&http.Client{Timeout: 5 * time.Second}, "dev", slog.Default())
	if err != nil {
		t.Fatalf("NewFederationAuthenticator failed: %v", err)
	}

	authCfg := AuthConfig{
		Mode:           "production",
		FederationAuth: newFedAuthPointer(authenticator),
		Debug:          true,
		Logger:         slog.Default(),
	}

	identityHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity := GetIdentityFromContext(r.Context())
		if identity == nil {
			http.Error(w, "no identity in context", http.StatusInternalServerError)
			return
		}

		resp := map[string]interface{}{
			"id":   identity.ID(),
			"type": identity.Type(),
		}

		if fed, ok := identity.(FederatedIdentity); ok {
			resp["issuer_url"] = fed.IssuerURL()
		}

		if sid, ok := identity.(*FederatedServiceIdentity); ok {
			resp["email"] = sid.Email()
			resp["subject"] = sid.Subject()
			resp["scopes_count"] = len(sid.Scopes())
		}

		if uid, ok := identity.(*FederatedUserIdentity); ok {
			resp["email"] = uid.Email()
			resp["display_name"] = uid.DisplayName()
			resp["role"] = uid.Role()
			resp["subject"] = uid.Subject()
			resp["scopes_count"] = len(uid.Scopes())
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)
	})

	authMiddleware := UnifiedAuthMiddleware(authCfg)
	handler := authMiddleware(identityHandler)

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

// TestFederationE2E_GCPServiceAccount tests the full E2E flow for a GCP service account issuer.
func TestFederationE2E_GCPServiceAccount(t *testing.T) {
	kid := "sa-e2e-key"
	saIssuer := "https://accounts.google.com"
	audience := "https://hub-b.example.com"

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
	jwksSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jwksData)
	}))
	t.Cleanup(jwksSrv.Close)

	fedCfg := config.FederationConfig{
		Enabled: true,
		TrustedIssuers: []config.TrustedIssuerConfig{
			{
				IssuerURL:        saIssuer,
				JWKSURL:          jwksSrv.URL,
				ExpectedAudience: audience,
				IssuerType:       "service_account",
				AllowedEmails:    []string{"my-sa@my-project.iam.gserviceaccount.com"},
			},
		},
	}

	server := setupNonHubE2EServer(t, fedCfg, audience)

	now := time.Now()
	claims := map[string]interface{}{
		"iss":            saIssuer,
		"sub":            "sa-subject-123",
		"aud":            audience,
		"iat":            now.Add(-1 * time.Minute).Unix(),
		"exp":            now.Add(5 * time.Minute).Unix(),
		"nbf":            now.Add(-1 * time.Minute).Unix(),
		"email":          "my-sa@my-project.iam.gserviceaccount.com",
		"email_verified": true,
	}
	token := signGenericToken(t, privKey, kid, claims)

	req, err := http.NewRequest(http.MethodGet, server.URL+"/api/v1/test", nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	req.Header.Set(FederationTokenHeader, token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	var body map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// Verify identity fields
	if body["type"] != "federated_service" {
		t.Errorf("expected type 'federated_service', got %v", body["type"])
	}
	expectedID := saIssuer + ":sa-subject-123"
	if body["id"] != expectedID {
		t.Errorf("expected id %q, got %v", expectedID, body["id"])
	}
	if body["email"] != "my-sa@my-project.iam.gserviceaccount.com" {
		t.Errorf("expected email 'my-sa@my-project.iam.gserviceaccount.com', got %v", body["email"])
	}
	if body["subject"] != "sa-subject-123" {
		t.Errorf("expected subject 'sa-subject-123', got %v", body["subject"])
	}
	if body["issuer_url"] != saIssuer {
		t.Errorf("expected issuer_url %q, got %v", saIssuer, body["issuer_url"])
	}
	// SA gets zero-trust empty scopes
	if body["scopes_count"] != float64(0) {
		t.Errorf("expected scopes_count 0 (zero-trust), got %v", body["scopes_count"])
	}
}

// TestFederationE2E_FirebaseUser tests the full E2E flow for a Firebase user issuer.
func TestFederationE2E_FirebaseUser(t *testing.T) {
	kid := "user-e2e-key"
	userIssuer := "https://securetoken.google.com/my-firebase-project"
	audience := "my-firebase-project"

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
	jwksSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jwksData)
	}))
	t.Cleanup(jwksSrv.Close)

	fedCfg := config.FederationConfig{
		Enabled: true,
		TrustedIssuers: []config.TrustedIssuerConfig{
			{
				IssuerURL:        userIssuer,
				JWKSURL:          jwksSrv.URL,
				ExpectedAudience: audience,
				IssuerType:       "user",
				DefaultRole:      "editor",
				AllowedEmails:    []string{"*@example.com"},
			},
		},
	}

	server := setupNonHubE2EServer(t, fedCfg, audience)

	now := time.Now()
	claims := map[string]interface{}{
		"iss":            userIssuer,
		"sub":            "firebase-uid-abc123",
		"aud":            audience,
		"iat":            now.Add(-1 * time.Minute).Unix(),
		"exp":            now.Add(5 * time.Minute).Unix(),
		"nbf":            now.Add(-1 * time.Minute).Unix(),
		"email":          "user@example.com",
		"email_verified": true,
		"name":           "Test User",
	}
	token := signGenericToken(t, privKey, kid, claims)

	req, err := http.NewRequest(http.MethodGet, server.URL+"/api/v1/test", nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	req.Header.Set(FederationTokenHeader, token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	var body map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// Verify identity fields
	if body["type"] != "federated_user" {
		t.Errorf("expected type 'federated_user', got %v", body["type"])
	}
	expectedID := userIssuer + ":firebase-uid-abc123"
	if body["id"] != expectedID {
		t.Errorf("expected id %q, got %v", expectedID, body["id"])
	}
	if body["email"] != "user@example.com" {
		t.Errorf("expected email 'user@example.com', got %v", body["email"])
	}
	if body["display_name"] != "Test User" {
		t.Errorf("expected display_name 'Test User', got %v", body["display_name"])
	}
	if body["role"] != "editor" {
		t.Errorf("expected role 'editor', got %v", body["role"])
	}
	if body["issuer_url"] != userIssuer {
		t.Errorf("expected issuer_url %q, got %v", userIssuer, body["issuer_url"])
	}
	// User gets zero-trust empty scopes
	if body["scopes_count"] != float64(0) {
		t.Errorf("expected scopes_count 0 (zero-trust), got %v", body["scopes_count"])
	}

	// Also verify the identity can be retrieved via GetUserIdentityFromContext
	// and GetFederatedIdentityFromContext. We do this by creating a direct
	// authenticator-level test inline since E2E handlers can't easily return
	// context interface assertions over HTTP.
	authenticator, err := NewFederationAuthenticator(fedCfg, audience,
		&http.Client{Timeout: 5 * time.Second}, "dev", slog.Default())
	if err != nil {
		t.Fatalf("NewFederationAuthenticator failed: %v", err)
	}
	identity, err := authenticator.Authenticate(token)
	if err != nil {
		t.Fatalf("Authenticate failed: %v", err)
	}
	// FederatedUserIdentity implements UserIdentity
	if _, ok := identity.(UserIdentity); !ok {
		t.Errorf("expected FederatedUserIdentity to implement UserIdentity")
	}
}

// TestFederationE2E_SAEmailRejected tests that a SA with email not in allowed_emails is rejected.
func TestFederationE2E_SAEmailRejected(t *testing.T) {
	kid := "sa-reject-key"
	saIssuer := "https://accounts.google.com"
	audience := "https://hub-b.example.com"

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
	jwksSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jwksData)
	}))
	t.Cleanup(jwksSrv.Close)

	fedCfg := config.FederationConfig{
		Enabled: true,
		TrustedIssuers: []config.TrustedIssuerConfig{
			{
				IssuerURL:        saIssuer,
				JWKSURL:          jwksSrv.URL,
				ExpectedAudience: audience,
				IssuerType:       "service_account",
				AllowedEmails:    []string{"allowed-sa@my-project.iam.gserviceaccount.com"},
			},
		},
	}

	server := setupNonHubE2EServer(t, fedCfg, audience)

	now := time.Now()
	claims := map[string]interface{}{
		"iss":            saIssuer,
		"sub":            "wrong-sa-subject",
		"aud":            audience,
		"iat":            now.Add(-1 * time.Minute).Unix(),
		"exp":            now.Add(5 * time.Minute).Unix(),
		"nbf":            now.Add(-1 * time.Minute).Unix(),
		"email":          "wrong-sa@other-project.iam.gserviceaccount.com",
		"email_verified": true,
	}
	token := signGenericToken(t, privKey, kid, claims)

	req, err := http.NewRequest(http.MethodGet, server.URL+"/api/v1/test", nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	req.Header.Set(FederationTokenHeader, token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected status 401 for disallowed email, got %d", resp.StatusCode)
	}
}

// TestFederationE2E_OIDCDiscovery tests the full E2E flow where OIDC discovery resolves the JWKS URL.
func TestFederationE2E_OIDCDiscovery(t *testing.T) {
	kid := "discovery-e2e-key"
	audience := "https://hub-b.example.com"

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

	// JWKS server at a separate URL
	jwksSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jwksData)
	}))
	t.Cleanup(jwksSrv.Close)

	// Issuer server with OIDC discovery endpoint that points to the JWKS server
	issuerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/openid-configuration" {
			w.Header().Set("Content-Type", "application/json")
			discoveryDoc := `{"issuer": "` + "placeholder" + `", "jwks_uri": "` + jwksSrv.URL + `"}`
			_, _ = w.Write([]byte(discoveryDoc))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(issuerSrv.Close)

	// Configure with NO jwks_url — OIDC discovery should resolve it
	fedCfg := config.FederationConfig{
		Enabled: true,
		TrustedIssuers: []config.TrustedIssuerConfig{
			{
				IssuerURL:        issuerSrv.URL,
				JWKSURL:          "", // empty — discovery should resolve
				ExpectedAudience: audience,
				IssuerType:       "service_account",
				AllowedEmails:    []string{"*@my-project.iam.gserviceaccount.com"},
			},
		},
	}

	server := setupNonHubE2EServer(t, fedCfg, audience)

	now := time.Now()
	claims := map[string]interface{}{
		"iss":            issuerSrv.URL,
		"sub":            "discovery-sa-subject",
		"aud":            audience,
		"iat":            now.Add(-1 * time.Minute).Unix(),
		"exp":            now.Add(5 * time.Minute).Unix(),
		"nbf":            now.Add(-1 * time.Minute).Unix(),
		"email":          "discovered-sa@my-project.iam.gserviceaccount.com",
		"email_verified": true,
	}
	token := signGenericToken(t, privKey, kid, claims)

	req, err := http.NewRequest(http.MethodGet, server.URL+"/api/v1/test", nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	req.Header.Set(FederationTokenHeader, token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200 (OIDC discovery should resolve JWKS URL), got %d", resp.StatusCode)
	}

	var body map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if body["type"] != "federated_service" {
		t.Errorf("expected type 'federated_service', got %v", body["type"])
	}
	if body["email"] != "discovered-sa@my-project.iam.gserviceaccount.com" {
		t.Errorf("expected email 'discovered-sa@my-project.iam.gserviceaccount.com', got %v", body["email"])
	}
}
