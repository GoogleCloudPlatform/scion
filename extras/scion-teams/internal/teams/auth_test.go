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

package teams

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------- TokenProvider tests ----------

func TestTokenProvider_GetToken(t *testing.T) {
	callCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		assert.Equal(t, "POST", r.Method)
		assert.Equal(t, "application/x-www-form-urlencoded", r.Header.Get("Content-Type"))

		err := r.ParseForm()
		require.NoError(t, err)
		assert.Equal(t, "client_credentials", r.FormValue("grant_type"))
		assert.Equal(t, "test-app-id", r.FormValue("client_id"))
		assert.Equal(t, "test-secret", r.FormValue("client_secret"))
		assert.Equal(t, botFrameworkScope, r.FormValue("scope"))

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(tokenResponse{
			AccessToken: "test-token-123",
			ExpiresIn:   3600,
			TokenType:   "Bearer",
		})
	}))
	defer ts.Close()

	tp := NewTokenProvider("test-app-id", "test-secret", "test-tenant")
	tp.httpClient = ts.Client()
	tp.tokenEndpoint = ts.URL

	ctx := context.Background()

	// First call should fetch a new token.
	token, err := tp.GetToken(ctx)
	require.NoError(t, err)
	assert.Equal(t, "test-token-123", token)
	assert.Equal(t, 1, callCount)

	// Second call should use the cached token.
	token, err = tp.GetToken(ctx)
	require.NoError(t, err)
	assert.Equal(t, "test-token-123", token)
	assert.Equal(t, 1, callCount, "should use cached token")
}

func TestTokenProvider_RefreshOnExpiry(t *testing.T) {
	callCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(tokenResponse{
			AccessToken: fmt.Sprintf("token-%d", callCount),
			ExpiresIn:   3600,
			TokenType:   "Bearer",
		})
	}))
	defer ts.Close()

	tp := NewTokenProvider("app", "secret", "tenant")
	tp.httpClient = ts.Client()
	tp.tokenEndpoint = ts.URL

	ctx := context.Background()

	// Get initial token.
	token, err := tp.GetToken(ctx)
	require.NoError(t, err)
	assert.Equal(t, "token-1", token)

	// Simulate token about to expire (within refresh window).
	tp.mu.Lock()
	tp.expiresAt = time.Now().Add(2 * time.Minute) // Within 5-minute window.
	tp.mu.Unlock()

	// Should fetch a new token.
	token, err = tp.GetToken(ctx)
	require.NoError(t, err)
	assert.Equal(t, "token-2", token)
	assert.Equal(t, 2, callCount)
}

func TestTokenProvider_InvalidateToken(t *testing.T) {
	// R3: InvalidateToken clears the cache and forces a refresh.
	callCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(tokenResponse{
			AccessToken: fmt.Sprintf("token-%d", callCount),
			ExpiresIn:   3600,
		})
	}))
	defer ts.Close()

	tp := NewTokenProvider("app", "secret", "tenant")
	tp.httpClient = ts.Client()
	tp.tokenEndpoint = ts.URL

	ctx := context.Background()

	// Get initial token.
	token, err := tp.GetToken(ctx)
	require.NoError(t, err)
	assert.Equal(t, "token-1", token)
	assert.Equal(t, 1, callCount)

	// Invalidate.
	tp.InvalidateToken()

	// Next GetToken should force a refresh.
	token, err = tp.GetToken(ctx)
	require.NoError(t, err)
	assert.Equal(t, "token-2", token)
	assert.Equal(t, 2, callCount)
}

func TestTokenProvider_ErrorHandling(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte("invalid client"))
	}))
	defer ts.Close()

	tp := NewTokenProvider("bad-app", "bad-secret", "tenant")
	tp.httpClient = ts.Client()
	tp.tokenEndpoint = ts.URL

	_, err := tp.GetToken(context.Background())
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "401")
}

// ---------- TokenProvider fallback (fallbackOrError) tests ----------

func TestTokenProvider_FallbackOnRefreshFailure_CachedTokenValid(t *testing.T) {
	// Scenario: refresh fails but cached token is still valid (not yet expired).
	// Expected: returns the cached token without error.

	callCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			// First call succeeds — populates the cache.
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(tokenResponse{
				AccessToken: "cached-valid-token",
				ExpiresIn:   3600, // 1 hour
				TokenType:   "Bearer",
			})
			return
		}
		// Subsequent calls fail — simulate Azure AD outage.
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal server error"))
	}))
	defer ts.Close()

	tp := NewTokenProvider("app", "secret", "tenant")
	tp.httpClient = ts.Client()
	tp.tokenEndpoint = ts.URL

	ctx := context.Background()

	// Populate the cache with a valid token.
	token, err := tp.GetToken(ctx)
	require.NoError(t, err)
	assert.Equal(t, "cached-valid-token", token)
	assert.Equal(t, 1, callCount)

	// Move expiresAt so token is still valid but inside the proactive refresh window.
	// This forces a refresh attempt on the next GetToken call.
	tp.mu.Lock()
	tp.expiresAt = time.Now().Add(3 * time.Minute) // Within 5-min refresh window, but not expired.
	tp.mu.Unlock()

	// GetToken should attempt refresh (fails with 500), then fall back to cached token.
	token, err = tp.GetToken(ctx)
	require.NoError(t, err, "should return cached token when refresh fails but token is still valid")
	assert.Equal(t, "cached-valid-token", token)
	assert.Equal(t, 2, callCount, "should have attempted a refresh")
}

func TestTokenProvider_FallbackOnRefreshFailure_CachedTokenExpired(t *testing.T) {
	// Scenario: refresh fails and cached token has actually expired.
	// Expected: returns the HTTP error (no fallback).

	callCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			// First call succeeds — populates the cache.
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(tokenResponse{
				AccessToken: "soon-expired-token",
				ExpiresIn:   3600,
				TokenType:   "Bearer",
			})
			return
		}
		// Subsequent calls fail.
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal server error"))
	}))
	defer ts.Close()

	tp := NewTokenProvider("app", "secret", "tenant")
	tp.httpClient = ts.Client()
	tp.tokenEndpoint = ts.URL

	ctx := context.Background()

	// Populate the cache.
	token, err := tp.GetToken(ctx)
	require.NoError(t, err)
	assert.Equal(t, "soon-expired-token", token)

	// Set expiresAt to the past — token is truly expired.
	tp.mu.Lock()
	tp.expiresAt = time.Now().Add(-1 * time.Minute)
	tp.mu.Unlock()

	// GetToken should attempt refresh (fails), and since the cached token is
	// expired, it should return the error.
	_, err = tp.GetToken(ctx)
	assert.Error(t, err, "should return error when refresh fails and cached token is expired")
	assert.Contains(t, err.Error(), "500")
}

func TestTokenProvider_FallbackOnRefreshFailure_NoCachedToken(t *testing.T) {
	// Scenario: fresh TokenProvider with no cached token; token endpoint returns error.
	// Expected: returns the error directly (no fallback possible).

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("service unavailable"))
	}))
	defer ts.Close()

	tp := NewTokenProvider("app", "secret", "tenant")
	tp.httpClient = ts.Client()
	tp.tokenEndpoint = ts.URL

	_, err := tp.GetToken(context.Background())
	assert.Error(t, err, "should return error when no cached token exists and refresh fails")
	assert.Contains(t, err.Error(), "503")
}

// ---------- JWTValidator tests ----------

// testJWKS sets up a test RSA key pair and JWKS/OpenID metadata endpoints.
type testJWKS struct {
	privateKey *rsa.PrivateKey
	kid        string
	server     *httptest.Server
}

func newTestJWKS(t *testing.T) *testJWKS {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	kid := "test-key-1"

	mux := http.NewServeMux()

	// JWKS endpoint.
	mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		jwks := jwksResponse{
			Keys: []jwkKey{
				{
					Kty: "RSA",
					Use: "sig",
					Kid: kid,
					N:   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
					E:   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(jwks)
	})

	server := httptest.NewServer(mux)

	// OpenID metadata endpoint.
	mux.HandleFunc("/.well-known/openidconfiguration", func(w http.ResponseWriter, r *http.Request) {
		config := openIDConfig{
			JWKSURI: server.URL + "/keys",
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(config)
	})

	return &testJWKS{
		privateKey: key,
		kid:        kid,
		server:     server,
	}
}

func (tj *testJWKS) close() {
	tj.server.Close()
}

func (tj *testJWKS) signToken(claims jwt.MapClaims) string {
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = tj.kid
	signed, err := token.SignedString(tj.privateKey)
	if err != nil {
		panic(err)
	}
	return signed
}

func (tj *testJWKS) validClaims(appID string) jwt.MapClaims {
	return jwt.MapClaims{
		"iss": expectedIssuer,
		"aud": appID,
		"exp": time.Now().Add(1 * time.Hour).Unix(),
		"nbf": time.Now().Add(-1 * time.Minute).Unix(),
		"iat": time.Now().Unix(),
	}
}

func TestJWTValidator_ValidToken(t *testing.T) {
	tj := newTestJWKS(t)
	defer tj.close()

	appID := "test-app-id"
	v := NewJWTValidator(appID)
	v.openIDMetadataURL = tj.server.URL + "/.well-known/openidconfiguration"
	v.httpClient = tj.server.Client()

	tokenString := tj.signToken(tj.validClaims(appID))

	token, err := v.ValidateToken(context.Background(), tokenString)
	require.NoError(t, err)
	assert.True(t, token.Valid)
}

func TestJWTValidator_ExpiredToken(t *testing.T) {
	tj := newTestJWKS(t)
	defer tj.close()

	appID := "test-app-id"
	v := NewJWTValidator(appID)
	v.openIDMetadataURL = tj.server.URL + "/.well-known/openidconfiguration"
	v.httpClient = tj.server.Client()

	claims := tj.validClaims(appID)
	claims["exp"] = time.Now().Add(-1 * time.Hour).Unix()

	tokenString := tj.signToken(claims)

	_, err := v.ValidateToken(context.Background(), tokenString)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "token is expired")
}

func TestJWTValidator_NbfInFuture(t *testing.T) {
	tj := newTestJWKS(t)
	defer tj.close()

	appID := "test-app-id"
	v := NewJWTValidator(appID)
	v.openIDMetadataURL = tj.server.URL + "/.well-known/openidconfiguration"
	v.httpClient = tj.server.Client()

	claims := tj.validClaims(appID)
	claims["nbf"] = time.Now().Add(1 * time.Hour).Unix() // Not valid yet.

	tokenString := tj.signToken(claims)

	_, err := v.ValidateToken(context.Background(), tokenString)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "token is not valid yet")
}

func TestJWTValidator_WrongAudience(t *testing.T) {
	tj := newTestJWKS(t)
	defer tj.close()

	appID := "test-app-id"
	v := NewJWTValidator(appID)
	v.openIDMetadataURL = tj.server.URL + "/.well-known/openidconfiguration"
	v.httpClient = tj.server.Client()

	claims := tj.validClaims("wrong-app-id")
	tokenString := tj.signToken(claims)

	_, err := v.ValidateToken(context.Background(), tokenString)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "token validation failed")
}

func TestJWTValidator_WrongIssuer(t *testing.T) {
	tj := newTestJWKS(t)
	defer tj.close()

	appID := "test-app-id"
	v := NewJWTValidator(appID)
	v.openIDMetadataURL = tj.server.URL + "/.well-known/openidconfiguration"
	v.httpClient = tj.server.Client()

	claims := tj.validClaims(appID)
	claims["iss"] = "https://evil.example.com"
	tokenString := tj.signToken(claims)

	_, err := v.ValidateToken(context.Background(), tokenString)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "token validation failed")
}

func TestJWTValidator_TamperedSignature(t *testing.T) {
	tj := newTestJWKS(t)
	defer tj.close()

	appID := "test-app-id"
	v := NewJWTValidator(appID)
	v.openIDMetadataURL = tj.server.URL + "/.well-known/openidconfiguration"
	v.httpClient = tj.server.Client()

	tokenString := tj.signToken(tj.validClaims(appID))

	// Tamper with the signature by flipping a character.
	tampered := tokenString[:len(tokenString)-5] + "XXXXX"

	_, err := v.ValidateToken(context.Background(), tampered)
	assert.Error(t, err)
}

func TestJWTValidator_UnknownKid(t *testing.T) {
	tj := newTestJWKS(t)
	defer tj.close()

	appID := "test-app-id"
	v := NewJWTValidator(appID)
	v.openIDMetadataURL = tj.server.URL + "/.well-known/openidconfiguration"
	v.httpClient = tj.server.Client()

	// Sign with a different kid.
	claims := tj.validClaims(appID)
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = "unknown-key-id"
	tokenString, err := token.SignedString(tj.privateKey)
	require.NoError(t, err)

	_, err = v.ValidateToken(context.Background(), tokenString)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found in JWKS")
}

func TestParseRSAKey(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	jwk := jwkKey{
		Kty: "RSA",
		Kid: "test",
		N:   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		E:   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
	}

	parsed, err := parseRSAKey(jwk)
	require.NoError(t, err)
	assert.Equal(t, key.N, parsed.N)
	assert.Equal(t, key.E, parsed.E)
}
