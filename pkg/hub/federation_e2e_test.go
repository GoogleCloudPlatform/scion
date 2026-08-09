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
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

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
		Mode:                    "production",
		FederationAuthenticator: authenticator,
		Debug:                   true,
		Logger:                  slog.Default(),
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
	claims.RootUser = "user:e2e-admin"
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
	defer resp.Body.Close()

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
	defer resp.Body.Close()

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
	defer resp.Body.Close()

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
	defer resp.Body.Close()

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
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected status 401, got %d", resp.StatusCode)
	}
}
