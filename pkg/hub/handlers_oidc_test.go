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

//go:build !no_sqlite

package hub

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testOIDCIssuerURL = "https://scion.example.com"

// testOIDCServer creates a Server with an OIDCKeyManager configured for testing.
// The key manager is set after server creation, so routes are NOT registered
// via the mux. Use this for direct handler tests.
func testOIDCServer(t *testing.T) *Server {
	t.Helper()
	srv, _ := testServer(t)

	// Generate a test RSA key pair.
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	kid := computeKeyID(&privKey.PublicKey)
	signingKey := &OIDCSigningKey{
		KeyID:      kid,
		PrivateKey: privKey,
		PublicKey:  &privKey.PublicKey,
		Active:     true,
	}

	// Create the RS256 signer (needed by identity token endpoint).
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: privKey},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", kid),
	)
	require.NoError(t, err)

	mgr := &OIDCKeyManager{
		activeKey: signingKey,
		allKeys:   []*OIDCSigningKey{signingKey},
		signer:    signer,
		issuerURL: testOIDCIssuerURL,
	}

	srv.oidcKeyManager = mgr
	srv.oidcIssuerURL = testOIDCIssuerURL
	return srv
}

// testOIDCServerWithRoutes creates a Server with OIDC enabled via config so
// that routes are registered during New(). Use for mux-routing tests.
func testOIDCServerWithRoutes(t *testing.T) *Server {
	t.Helper()
	s, err := newTestStore(":memory:")
	if err != nil {
		if strings.Contains(err.Error(), "sqlite driver not registered") {
			t.Skip("Skipping test because sqlite driver is not registered (build with -tags sqlite to enable)")
		}
		t.Fatalf("failed to create test store: %v", err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate test store: %v", err)
	}

	cfg := DefaultServerConfig()
	cfg.DevAuthToken = testDevToken
	cfg.OIDCConfig = config.OIDCProviderConfig{
		Enabled:   true,
		IssuerURL: testOIDCIssuerURL,
	}

	srv, err := New(cfg, s)
	if err != nil {
		t.Fatalf("New() with OIDC failed: %v", err)
	}
	srv.SetHubID("test-hub-id")
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	return srv
}

func TestHandleOIDCDiscovery(t *testing.T) {
	tests := []struct {
		name           string
		method         string
		oidcEnabled    bool
		wantStatus     int
		checkBody      bool
		checkCacheCtrl string
	}{
		{
			name:           "GET returns valid discovery document",
			method:         http.MethodGet,
			oidcEnabled:    true,
			wantStatus:     http.StatusOK,
			checkBody:      true,
			checkCacheCtrl: "public, max-age=3600",
		},
		{
			name:        "POST returns 405",
			method:      http.MethodPost,
			oidcEnabled: true,
			wantStatus:  http.StatusMethodNotAllowed,
		},
		{
			name:        "PUT returns 405",
			method:      http.MethodPut,
			oidcEnabled: true,
			wantStatus:  http.StatusMethodNotAllowed,
		},
		{
			name:        "DELETE returns 405",
			method:      http.MethodDelete,
			oidcEnabled: true,
			wantStatus:  http.StatusMethodNotAllowed,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := testOIDCServer(t)

			req := httptest.NewRequest(tc.method, "/.well-known/openid-configuration", nil)
			w := httptest.NewRecorder()

			srv.handleOIDCDiscovery(w, req)

			assert.Equal(t, tc.wantStatus, w.Code)

			if tc.checkBody {
				assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
				assert.Equal(t, tc.checkCacheCtrl, w.Header().Get("Cache-Control"))

				var doc oidcDiscoveryDocument
				err := json.Unmarshal(w.Body.Bytes(), &doc)
				require.NoError(t, err, "response should be valid JSON")

				assert.Equal(t, testOIDCIssuerURL, doc.Issuer)
				assert.Equal(t, testOIDCIssuerURL+"/.well-known/jwks.json", doc.JWKSURI)
				assert.Equal(t, []string{"id_token"}, doc.ResponseTypesSupported)
				assert.Equal(t, []string{"public"}, doc.SubjectTypesSupported)
				assert.Equal(t, []string{"RS256"}, doc.IDTokenSigningAlgValuesSupported)
				assert.Equal(t, []string{"openid"}, doc.ScopesSupported)

				// Verify all required claims are present.
				expectedClaims := []string{
					"iss", "sub", "aud", "iat", "exp", "nbf", "jti",
					"project_id", "agent_name", "ancestry", "root_user",
				}
				assert.Equal(t, expectedClaims, doc.ClaimsSupported)
			}
		})
	}
}

func TestHandleJWKS(t *testing.T) {
	tests := []struct {
		name           string
		method         string
		wantStatus     int
		checkBody      bool
		checkCacheCtrl string
	}{
		{
			name:           "GET returns valid JWKS",
			method:         http.MethodGet,
			wantStatus:     http.StatusOK,
			checkBody:      true,
			checkCacheCtrl: "public, max-age=300",
		},
		{
			name:       "POST returns 405",
			method:     http.MethodPost,
			wantStatus: http.StatusMethodNotAllowed,
		},
		{
			name:       "PUT returns 405",
			method:     http.MethodPut,
			wantStatus: http.StatusMethodNotAllowed,
		},
		{
			name:       "DELETE returns 405",
			method:     http.MethodDelete,
			wantStatus: http.StatusMethodNotAllowed,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := testOIDCServer(t)

			req := httptest.NewRequest(tc.method, "/.well-known/jwks.json", nil)
			w := httptest.NewRecorder()

			srv.handleJWKS(w, req)

			assert.Equal(t, tc.wantStatus, w.Code)

			if tc.checkBody {
				assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
				assert.Equal(t, tc.checkCacheCtrl, w.Header().Get("Cache-Control"))

				// Parse the JWKS response.
				var jwks struct {
					Keys []map[string]interface{} `json:"keys"`
				}
				err := json.Unmarshal(w.Body.Bytes(), &jwks)
				require.NoError(t, err, "response should be valid JSON")

				require.NotEmpty(t, jwks.Keys, "JWKS should contain at least one key")

				key := jwks.Keys[0]

				// Verify required JWK fields are present.
				assert.Equal(t, "RSA", key["kty"], "key type should be RSA")
				assert.NotEmpty(t, key["kid"], "kid should be present")
				assert.Equal(t, "sig", key["use"], "use should be sig")
				assert.Equal(t, "RS256", key["alg"], "alg should be RS256")
				assert.NotEmpty(t, key["n"], "RSA modulus (n) should be present")
				assert.NotEmpty(t, key["e"], "RSA exponent (e) should be present")

				// Verify no private key material is exposed.
				privateFields := []string{"d", "p", "q", "dp", "dq", "qi"}
				for _, f := range privateFields {
					assert.Nil(t, key[f], "private key field %q must not be present in JWKS", f)
				}
			}
		})
	}
}

func TestOIDCEndpoints_Unauthenticated(t *testing.T) {
	srv := testOIDCServerWithRoutes(t)

	// These requests do NOT include any auth headers.
	tests := []struct {
		name string
		path string
	}{
		{name: "discovery endpoint", path: "/.well-known/openid-configuration"},
		{name: "JWKS endpoint", path: "/.well-known/jwks.json"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Verify isUnauthenticatedEndpoint returns true.
			assert.True(t, isUnauthenticatedEndpoint(tc.path),
				"%s should be unauthenticated", tc.path)

			// Also verify the handler responds to unauthenticated requests.
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			w := httptest.NewRecorder()

			// Route through the server mux to exercise registration.
			srv.mux.ServeHTTP(w, req)

			assert.Equal(t, http.StatusOK, w.Code,
				"%s should return 200 without auth", tc.path)
		})
	}
}

func TestOIDCEndpoints_DisabledWhenKeyManagerNil(t *testing.T) {
	// Create a server WITHOUT an OIDCKeyManager — routes should not be registered.
	srv, _ := testServer(t)
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	tests := []struct {
		name string
		path string
	}{
		{name: "discovery endpoint disabled", path: "/.well-known/openid-configuration"},
		{name: "JWKS endpoint disabled", path: "/.well-known/jwks.json"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			w := httptest.NewRecorder()

			srv.mux.ServeHTTP(w, req)

			// When OIDC is disabled, these paths are not registered and the mux
			// returns 404 (or the catch-all handler's status).
			assert.NotEqual(t, http.StatusOK, w.Code,
				"%s should not return 200 when OIDC is disabled", tc.path)
		})
	}
}

// ---------------------------------------------------------------------------
// Identity Token Endpoint Tests
// ---------------------------------------------------------------------------

// testOIDCIdentityServer creates a Server with OIDC fully configured for
// identity token tests: key manager, rate limiter, token lifetime, and a
// test agent + project in the store.
func testOIDCIdentityServer(t *testing.T) (*Server, store.Store, *AgentTokenClaims) {
	t.Helper()
	srv := testOIDCServer(t)

	// Set up token lifetime and rate limiter.
	srv.oidcTokenLifetime = 15 * time.Minute
	srv.oidcTokenRateLimiter = NewGCPTokenRateLimiter(0.5, 30)

	// Get the underlying store.
	s := srv.store

	// Create a project via the HTTP handler to get proper OwnerID etc.
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects", map[string]string{
		"name": "test-oidc-project",
	})
	require.Equal(t, http.StatusCreated, rec.Code, "create project: %s", rec.Body.String())
	var project store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&project))

	// Create an agent in the store.
	agentID := uuid.New().String()
	agent := &store.Agent{
		ID:        agentID,
		Slug:      "test-agent",
		Name:      "Test Agent",
		ProjectID: project.ID,
	}
	err := s.CreateAgent(context.Background(), agent)
	require.NoError(t, err)

	// Build agent token claims with identity token scope.
	rootUserID := uuid.New().String()
	parentAgentID := uuid.New().String()
	claims := &AgentTokenClaims{
		Claims: jwt.Claims{
			Subject: agentID,
		},
		ProjectID: project.ID,
		Scopes:    []AgentTokenScope{ScopeAgentStatusUpdate, ScopeIdentityToken},
		Ancestry:  []string{rootUserID, parentAgentID},
	}

	return srv, s, claims
}

// doIdentityTokenRequest is a helper that makes a request to the identity token endpoint.
func doIdentityTokenRequest(t *testing.T, srv *Server, claims *AgentTokenClaims, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		require.NoError(t, err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/identity-token", bytes.NewReader(bodyBytes))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	// Inject agent claims into the context.
	if claims != nil {
		ctx := context.WithValue(req.Context(), agentContextKey{}, claims)
		req = req.WithContext(ctx)
	}

	w := httptest.NewRecorder()
	srv.handleAgentIdentityToken(w, req)
	return w
}

func TestHandleAgentIdentityToken(t *testing.T) {
	tests := []struct {
		name          string
		claims        func(base *AgentTokenClaims) *AgentTokenClaims
		body          interface{}
		wantStatus    int
		wantErrCode   string
		checkToken    bool
		checkAudience string
	}{
		{
			name: "valid request returns RS256-signed JWT with correct claims",
			claims: func(base *AgentTokenClaims) *AgentTokenClaims {
				return base
			},
			body:          identityTokenRequest{Audience: "https://api.example.com"},
			wantStatus:    http.StatusOK,
			checkToken:    true,
			checkAudience: "https://api.example.com",
		},
		{
			name: "missing scope returns 403",
			claims: func(base *AgentTokenClaims) *AgentTokenClaims {
				c := *base
				c.Scopes = []AgentTokenScope{ScopeAgentStatusUpdate} // no ScopeIdentityToken
				return &c
			},
			body:        identityTokenRequest{Audience: "https://api.example.com"},
			wantStatus:  http.StatusForbidden,
			wantErrCode: ErrCodeForbidden,
		},
		{
			name: "missing audience returns 400",
			claims: func(base *AgentTokenClaims) *AgentTokenClaims {
				return base
			},
			body:        identityTokenRequest{Audience: ""},
			wantStatus:  http.StatusBadRequest,
			wantErrCode: ErrCodeInvalidRequest,
		},
		{
			name: "empty body returns 400",
			claims: func(base *AgentTokenClaims) *AgentTokenClaims {
				return base
			},
			body:        map[string]string{},
			wantStatus:  http.StatusBadRequest,
			wantErrCode: ErrCodeInvalidRequest,
		},
		{
			name:        "no agent context returns 401",
			claims:      func(_ *AgentTokenClaims) *AgentTokenClaims { return nil },
			body:        identityTokenRequest{Audience: "https://api.example.com"},
			wantStatus:  http.StatusUnauthorized,
			wantErrCode: ErrCodeUnauthorized,
		},
		{
			name: "agent not found in store returns 404",
			claims: func(base *AgentTokenClaims) *AgentTokenClaims {
				c := *base
				c.Subject = "nonexistent-agent-uuid"
				return &c
			},
			body:       identityTokenRequest{Audience: "https://api.example.com"},
			wantStatus: http.StatusNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, baseClaims := testOIDCIdentityServer(t)

			claims := tc.claims(baseClaims)
			w := doIdentityTokenRequest(t, srv, claims, tc.body)

			assert.Equal(t, tc.wantStatus, w.Code, "body: %s", w.Body.String())

			if tc.wantErrCode != "" {
				var errResp ErrorResponse
				err := json.Unmarshal(w.Body.Bytes(), &errResp)
				require.NoError(t, err)
				assert.Equal(t, tc.wantErrCode, errResp.Error.Code)
			}

			if tc.checkToken {
				var resp identityTokenResponse
				err := json.Unmarshal(w.Body.Bytes(), &resp)
				require.NoError(t, err, "response should be valid JSON")
				assert.NotEmpty(t, resp.Token, "token should not be empty")
				assert.False(t, resp.ExpiresAt.IsZero(), "expires_at should be set")

				// Parse the token and verify claims.
				parsedToken, err := jwt.ParseSigned(resp.Token, []jose.SignatureAlgorithm{jose.RS256})
				require.NoError(t, err, "token should be parseable as a signed JWT")

				// Verify using the public key from the key manager.
				jwks := srv.oidcKeyManager.JWKS()
				require.NotEmpty(t, jwks.Keys, "JWKS should contain at least one key")

				var tokenClaims OIDCIdentityTokenClaims
				err = parsedToken.Claims(jwks.Keys[0].Key, &tokenClaims)
				require.NoError(t, err, "token should be verifiable with JWKS public key")

				// Verify iss
				assert.Equal(t, testOIDCIssuerURL, tokenClaims.Issuer, "iss should match issuer URL")

				// Verify aud
				assert.Equal(t, jwt.Audience{tc.checkAudience}, tokenClaims.Audience, "aud should match requested audience")

				// Verify sub
				assert.Equal(t, claims.Subject, tokenClaims.Subject, "sub should be the agent UUID")

				// Verify custom claims
				assert.Equal(t, claims.ProjectID, tokenClaims.ProjectID, "project_id should match")
				assert.Equal(t, "test-agent", tokenClaims.AgentName, "agent_name should be the agent slug")
				assert.Equal(t, claims.Ancestry, tokenClaims.Ancestry, "ancestry should match")
				assert.Equal(t, claims.Ancestry[0], tokenClaims.RootUser, "root_user should be ancestry[0]")

				// Verify exp = now + token_lifetime (within 5 second tolerance)
				expectedExpiry := time.Now().Add(15 * time.Minute)
				tokenExpiry := tokenClaims.Expiry.Time()
				assert.WithinDuration(t, expectedExpiry, tokenExpiry, 5*time.Second,
					"exp should be approximately now + 15m")

				// Verify jti is non-empty
				assert.NotEmpty(t, tokenClaims.ID, "jti should be set")

				// Verify kid header matches JWKS active key
				headers := parsedToken.Headers
				require.NotEmpty(t, headers)
				assert.Equal(t, jwks.Keys[0].KeyID, headers[0].KeyID,
					"kid header should match JWKS active key")
			}
		})
	}
}

func TestHandleAgentIdentityToken_TokenVerifiableWithJWKS(t *testing.T) {
	srv, _, claims := testOIDCIdentityServer(t)

	w := doIdentityTokenRequest(t, srv, claims, identityTokenRequest{
		Audience: "https://verify.example.com",
	})
	require.Equal(t, http.StatusOK, w.Code)

	var resp identityTokenResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	// Get JWKS and verify the token is valid.
	jwks := srv.oidcKeyManager.JWKS()
	require.NotEmpty(t, jwks.Keys)

	parsedToken, err := jwt.ParseSigned(resp.Token, []jose.SignatureAlgorithm{jose.RS256})
	require.NoError(t, err)

	var tokenClaims OIDCIdentityTokenClaims
	err = parsedToken.Claims(jwks.Keys[0].Key, &tokenClaims)
	require.NoError(t, err, "token MUST be verifiable with the JWKS public key")

	// Validate standard claims.
	expected := jwt.Expected{
		Issuer:      testOIDCIssuerURL,
		AnyAudience: jwt.Audience{"https://verify.example.com"},
		Time:        time.Now(),
	}
	err = tokenClaims.Validate(expected)
	assert.NoError(t, err, "standard claims should validate successfully")
}

func TestHandleAgentIdentityToken_EmptyAncestry(t *testing.T) {
	srv, _, baseClaims := testOIDCIdentityServer(t)

	// Test with empty ancestry — root_user should be empty.
	claims := *baseClaims
	claims.Ancestry = nil

	w := doIdentityTokenRequest(t, srv, &claims, identityTokenRequest{
		Audience: "https://api.example.com",
	})
	require.Equal(t, http.StatusOK, w.Code)

	var resp identityTokenResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	jwks := srv.oidcKeyManager.JWKS()
	parsedToken, err := jwt.ParseSigned(resp.Token, []jose.SignatureAlgorithm{jose.RS256})
	require.NoError(t, err)

	var tokenClaims OIDCIdentityTokenClaims
	err = parsedToken.Claims(jwks.Keys[0].Key, &tokenClaims)
	require.NoError(t, err)

	assert.Empty(t, tokenClaims.RootUser, "root_user should be empty when ancestry is nil")
}

func TestHandleAgentIdentityToken_RateLimiting(t *testing.T) {
	srv, _, claims := testOIDCIdentityServer(t)

	// Configure a tight rate limiter: burst of 2 only.
	srv.oidcTokenRateLimiter = NewGCPTokenRateLimiter(0.01, 2) // very low rate, burst 2

	body := identityTokenRequest{Audience: "https://api.example.com"}

	// First 2 requests should succeed (burst).
	for i := 0; i < 2; i++ {
		w := doIdentityTokenRequest(t, srv, claims, body)
		assert.Equal(t, http.StatusOK, w.Code, "request %d should succeed within burst", i+1)
	}

	// 3rd request should be rate limited.
	w := doIdentityTokenRequest(t, srv, claims, body)
	assert.Equal(t, http.StatusTooManyRequests, w.Code, "request exceeding burst should be rate limited")
}

func TestHandleAgentIdentityToken_ForbiddenMessage(t *testing.T) {
	srv, _, baseClaims := testOIDCIdentityServer(t)

	claims := *baseClaims
	claims.Scopes = []AgentTokenScope{ScopeAgentStatusUpdate} // no identity token scope

	w := doIdentityTokenRequest(t, srv, &claims, identityTokenRequest{
		Audience: "https://api.example.com",
	})
	assert.Equal(t, http.StatusForbidden, w.Code)

	var errResp ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &errResp))
	assert.Contains(t, errResp.Error.Message, "agent not authorized to request identity tokens")
}
