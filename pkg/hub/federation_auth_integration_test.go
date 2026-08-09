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
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// setupFederationIntegration generates an RSA key pair and JWKS test server,
// creates a FederationAuthenticator, and returns everything needed for
// middleware integration tests.
func setupFederationIntegration(t *testing.T) (
	privKey *rsa.PrivateKey,
	auth *FederationAuthenticator,
	issuer string,
	audience string,
	kid string,
) {
	t.Helper()

	kid = "integration-test-key"
	audience = "https://hub-local.example.com"

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

	jwksSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jwksData)
	}))
	t.Cleanup(jwksSrv.Close)

	issuer = "https://hub-remote.example.com"

	cfg := config.FederationConfig{
		Enabled: true,
		TrustedIssuers: []config.TrustedIssuerConfig{
			{
				IssuerURL:        issuer,
				JWKSURL:          jwksSrv.URL,
				ExpectedAudience: audience,
			},
		},
	}
	auth, err = NewFederationAuthenticator(cfg, audience,
		&http.Client{Timeout: 5 * time.Second}, "dev", slog.Default())
	if err != nil {
		t.Fatalf("NewFederationAuthenticator failed: %v", err)
	}

	return privKey, auth, issuer, audience, kid
}

// TestFederationMiddleware_ValidToken verifies that a valid federation token
// in X-Scion-Federation-Token results in 200 with correct identity on the context.
func TestFederationMiddleware_ValidToken(t *testing.T) {
	privKey, auth, issuer, audience, kid := setupFederationIntegration(t)

	cfg := AuthConfig{
		Mode: "production",
		FederationAuth: func() *atomic.Pointer[FederationAuthenticator] {
			p := &atomic.Pointer[FederationAuthenticator]{}
			p.Store(auth)
			return p
		}(),
		Debug:  true,
		Logger: slog.Default(),
	}

	middleware := UnifiedAuthMiddleware(cfg)

	claims := validFederationClaims(issuer, audience)
	claims.Subject = "agent-integration-1"
	claims.ProjectID = "project-test"
	claims.AgentName = "test-worker"
	claims.RootUser = "user:integrator"
	claims.Ancestry = []string{"user:integrator", "agent:root"}
	token := signFederationToken(t, privKey, kid, claims)

	var gotIdentity Identity
	var gotAgentIdentity AgentIdentity
	var gotUserIdentity UserIdentity
	var gotAgent *AgentTokenClaims
	var gotAuthType string

	handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotIdentity = GetIdentityFromContext(r.Context())
		gotAgentIdentity = GetAgentIdentityFromContext(r.Context())
		gotUserIdentity = GetUserIdentityFromContext(r.Context())
		gotAgent = GetAgentFromContext(r.Context())
		if at, ok := r.Context().Value(logging.AuthTypeKey{}).(string); ok {
			gotAuthType = at
		}
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	req.Header.Set(FederationTokenHeader, token)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify identity is a *FederatedAgentIdentity
	if gotIdentity == nil {
		t.Fatal("expected identity in context, got nil")
	}
	fedIdentity, ok := gotIdentity.(*FederatedAgentIdentity)
	if !ok {
		t.Fatalf("expected *FederatedAgentIdentity, got %T", gotIdentity)
	}

	// Verify FederatedAgentIdentity fields
	if fedIdentity.IssuerURL() != issuer {
		t.Errorf("expected issuer %q, got %q", issuer, fedIdentity.IssuerURL())
	}
	if fedIdentity.RemoteAgentID() != "agent-integration-1" {
		t.Errorf("expected agent ID 'agent-integration-1', got %q", fedIdentity.RemoteAgentID())
	}
	if fedIdentity.RemoteProjectID() != "project-test" {
		t.Errorf("expected project 'project-test', got %q", fedIdentity.RemoteProjectID())
	}
	if fedIdentity.AgentName() != "test-worker" {
		t.Errorf("expected agent name 'test-worker', got %q", fedIdentity.AgentName())
	}
	if fedIdentity.OriginUserID() != "user:integrator" {
		t.Errorf("expected root user 'user:integrator', got %q", fedIdentity.OriginUserID())
	}
	if fedIdentity.Type() != "federated_agent" {
		t.Errorf("expected type 'federated_agent', got %q", fedIdentity.Type())
	}

	// Verify GetAgentIdentityFromContext returns the FederatedAgentIdentity
	if gotAgentIdentity == nil {
		t.Fatal("expected agent identity from GetAgentIdentityFromContext, got nil")
	}

	// Verify GetUserIdentityFromContext returns nil (not a user)
	if gotUserIdentity != nil {
		t.Errorf("expected nil from GetUserIdentityFromContext, got %v", gotUserIdentity)
	}

	// Verify GetAgentFromContext returns nil (not a local agent token)
	if gotAgent != nil {
		t.Errorf("expected nil from GetAgentFromContext, got %v", gotAgent)
	}

	// Verify auth type is "federation"
	if gotAuthType != AuthTypeFederation {
		t.Errorf("expected auth type %q, got %q", AuthTypeFederation, gotAuthType)
	}
}

// TestFederationMiddleware_InvalidToken verifies that an invalid federation token
// results in 401 "invalid federation token".
func TestFederationMiddleware_InvalidToken(t *testing.T) {
	_, auth, _, _, _ := setupFederationIntegration(t)

	cfg := AuthConfig{
		Mode: "production",
		FederationAuth: func() *atomic.Pointer[FederationAuthenticator] {
			p := &atomic.Pointer[FederationAuthenticator]{}
			p.Store(auth)
			return p
		}(),
		Debug:  true,
		Logger: slog.Default(),
	}

	middleware := UnifiedAuthMiddleware(cfg)

	handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called for invalid token")
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	req.Header.Set(FederationTokenHeader, "invalid.token.here")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected status 401, got %d", rec.Code)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "invalid federation token") {
		t.Errorf("expected 'invalid federation token' in body, got: %s", body)
	}
}

// TestFederationMiddleware_NotConfigured verifies that a federation header
// present but federation not configured (nil authenticator) results in 401
// "federation authentication is not configured".
func TestFederationMiddleware_NotConfigured(t *testing.T) {
	cfg := AuthConfig{
		Mode:           "production",
		FederationAuth: nil, // federation not enabled
		Debug:          true,
		Logger:         slog.Default(),
	}

	middleware := UnifiedAuthMiddleware(cfg)

	handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called when federation is not configured")
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	req.Header.Set(FederationTokenHeader, "some.federation.token")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected status 401, got %d", rec.Code)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "federation authentication is not configured") {
		t.Errorf("expected 'federation authentication is not configured' in body, got: %s", body)
	}
}

// TestFederationMiddleware_NoHeader verifies that when no federation header is
// present, the request falls through to subsequent auth steps (not rejected by
// the federation check). With no other auth configured, it should get a
// "missing authorization header" response — proving federation didn't intercept.
func TestFederationMiddleware_NoHeader(t *testing.T) {
	_, auth, _, _, _ := setupFederationIntegration(t)

	cfg := AuthConfig{
		Mode: "production",
		FederationAuth: func() *atomic.Pointer[FederationAuthenticator] {
			p := &atomic.Pointer[FederationAuthenticator]{}
			p.Store(auth)
			return p
		}(),
		Debug:  true,
		Logger: slog.Default(),
	}

	middleware := UnifiedAuthMiddleware(cfg)

	handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called without any auth")
		w.WriteHeader(http.StatusOK)
	}))

	// No federation header, no agent header, no bearer token
	req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected status 401, got %d", rec.Code)
	}

	body := rec.Body.String()
	// Should fall through to "missing authorization header" — not a federation error
	if !strings.Contains(body, "missing authorization header") {
		t.Errorf("expected 'missing authorization header' in body (fall-through), got: %s", body)
	}
}

// TestFederationMiddleware_AgentTokenTakesPriority verifies that when both
// X-Scion-Agent-Token (valid) and X-Scion-Federation-Token are present, the
// agent token wins because step 1 runs before step 1.5.
func TestFederationMiddleware_AgentTokenTakesPriority(t *testing.T) {
	privKey, auth, issuer, audience, kid := setupFederationIntegration(t)

	// Create an agent token service for the local agent token
	agentTokenSvc, err := NewAgentTokenService(AgentTokenConfig{})
	if err != nil {
		t.Fatalf("failed to create agent token service: %v", err)
	}

	agentToken, err := agentTokenSvc.GenerateAgentToken(
		"local-agent-1", "project-local", []AgentTokenScope{ScopeAgentStatusUpdate}, nil)
	if err != nil {
		t.Fatalf("failed to generate agent token: %v", err)
	}

	cfg := AuthConfig{
		Mode:          "production",
		AgentTokenSvc: agentTokenSvc,
		FederationAuth: func() *atomic.Pointer[FederationAuthenticator] {
			p := &atomic.Pointer[FederationAuthenticator]{}
			p.Store(auth)
			return p
		}(),
		Debug:  true,
		Logger: slog.Default(),
	}

	middleware := UnifiedAuthMiddleware(cfg)

	// Create a valid federation token too
	claims := validFederationClaims(issuer, audience)
	claims.Subject = "federated-agent-1"
	fedToken := signFederationToken(t, privKey, kid, claims)

	var gotIdentity Identity
	var gotAuthType string

	handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotIdentity = GetIdentityFromContext(r.Context())
		if at, ok := r.Context().Value(logging.AuthTypeKey{}).(string); ok {
			gotAuthType = at
		}
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	req.Header.Set("X-Scion-Agent-Token", agentToken)
	req.Header.Set(FederationTokenHeader, fedToken)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	if gotIdentity == nil {
		t.Fatal("expected identity in context, got nil")
	}

	// The identity should be the local agent, not the federated agent
	if gotIdentity.Type() != "agent" {
		t.Errorf("expected identity type 'agent' (local), got %q", gotIdentity.Type())
	}
	if gotIdentity.ID() != "local-agent-1" {
		t.Errorf("expected agent ID 'local-agent-1', got %q", gotIdentity.ID())
	}

	// Auth type should be "agent", not "federation"
	if gotAuthType != AuthTypeAgent {
		t.Errorf("expected auth type %q, got %q", AuthTypeAgent, gotAuthType)
	}
}

// TestFederationMiddleware_ExpiredToken verifies that an expired but otherwise
// valid federation token is rejected at the middleware level with 401.
func TestFederationMiddleware_ExpiredToken(t *testing.T) {
	privKey, auth, issuer, audience, kid := setupFederationIntegration(t)

	cfg := AuthConfig{
		Mode: "production",
		FederationAuth: func() *atomic.Pointer[FederationAuthenticator] {
			p := &atomic.Pointer[FederationAuthenticator]{}
			p.Store(auth)
			return p
		}(),
		Debug:  true,
		Logger: slog.Default(),
	}

	middleware := UnifiedAuthMiddleware(cfg)

	claims := validFederationClaims(issuer, audience)
	claims.Expiry = jwt.NewNumericDate(time.Now().Add(-10 * time.Minute))
	claims.IssuedAt = jwt.NewNumericDate(time.Now().Add(-15 * time.Minute))
	claims.NotBefore = jwt.NewNumericDate(time.Now().Add(-15 * time.Minute))
	token := signFederationToken(t, privKey, kid, claims)

	handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called for expired token")
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	req.Header.Set(FederationTokenHeader, token)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected status 401, got %d", rec.Code)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "invalid federation token") {
		t.Errorf("expected 'invalid federation token' in body, got: %s", body)
	}
}
