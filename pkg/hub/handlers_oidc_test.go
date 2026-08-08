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
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
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

	mgr := &OIDCKeyManager{
		activeKey: signingKey,
		allKeys:   []*OIDCSigningKey{signingKey},
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
