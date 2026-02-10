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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgentTokenService_GenerateAndValidate(t *testing.T) {
	service, err := NewAgentTokenService(AgentTokenConfig{
		SigningKey:    make([]byte, 32),
		TokenDuration: time.Hour,
	})
	require.NoError(t, err)

	// Generate a token
	token, err := service.GenerateAgentToken("agent-123", "grove-456", []AgentTokenScope{ScopeAgentStatusUpdate})
	require.NoError(t, err)
	assert.NotEmpty(t, token)

	// Validate the token
	claims, err := service.ValidateAgentToken(token)
	require.NoError(t, err)
	assert.Equal(t, "agent-123", claims.Subject)
	assert.Equal(t, "grove-456", claims.GroveID)
	assert.Contains(t, claims.Scopes, ScopeAgentStatusUpdate)
	assert.Equal(t, AgentTokenIssuer, claims.Issuer)
}

func TestAgentTokenService_DefaultScopes(t *testing.T) {
	service, err := NewAgentTokenService(AgentTokenConfig{
		SigningKey: make([]byte, 32),
	})
	require.NoError(t, err)

	// Generate a token with no scopes specified
	token, err := service.GenerateAgentToken("agent-123", "grove-456", nil)
	require.NoError(t, err)

	// Validate the token has default scope
	claims, err := service.ValidateAgentToken(token)
	require.NoError(t, err)
	assert.Contains(t, claims.Scopes, ScopeAgentStatusUpdate)
}

func TestAgentTokenService_ExpiredToken(t *testing.T) {
	service, err := NewAgentTokenService(AgentTokenConfig{
		SigningKey:    make([]byte, 32),
		TokenDuration: -time.Hour, // Already expired
	})
	require.NoError(t, err)

	// Generate an expired token
	token, err := service.GenerateAgentToken("agent-123", "grove-456", nil)
	require.NoError(t, err)

	// Validation should fail
	_, err = service.ValidateAgentToken(token)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "token validation failed")
}

func TestAgentTokenService_InvalidSignature(t *testing.T) {
	service1, err := NewAgentTokenService(AgentTokenConfig{
		SigningKey: []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32},
	})
	require.NoError(t, err)

	service2, err := NewAgentTokenService(AgentTokenConfig{
		SigningKey: []byte{32, 31, 30, 29, 28, 27, 26, 25, 24, 23, 22, 21, 20, 19, 18, 17, 16, 15, 14, 13, 12, 11, 10, 9, 8, 7, 6, 5, 4, 3, 2, 1},
	})
	require.NoError(t, err)

	// Generate with service1
	token, err := service1.GenerateAgentToken("agent-123", "grove-456", nil)
	require.NoError(t, err)

	// Validate with service2 should fail
	_, err = service2.ValidateAgentToken(token)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to verify token")
}

func TestAgentTokenClaims_HasScope(t *testing.T) {
	claims := &AgentTokenClaims{
		Scopes: []AgentTokenScope{ScopeAgentStatusUpdate, ScopeAgentLogAppend},
	}

	assert.True(t, claims.HasScope(ScopeAgentStatusUpdate))
	assert.True(t, claims.HasScope(ScopeAgentLogAppend))
	assert.False(t, claims.HasScope(ScopeGroveSecretRead))
}

func TestAgentTokenService_RandomKeyGeneration(t *testing.T) {
	// When no signing key is provided, a random one should be generated
	service, err := NewAgentTokenService(AgentTokenConfig{})
	require.NoError(t, err)
	assert.NotNil(t, service)

	// Generate and validate should work
	token, err := service.GenerateAgentToken("agent-123", "grove-456", nil)
	require.NoError(t, err)

	claims, err := service.ValidateAgentToken(token)
	require.NoError(t, err)
	assert.Equal(t, "agent-123", claims.Subject)
}

func TestAgentAuthMiddleware(t *testing.T) {
	service, err := NewAgentTokenService(AgentTokenConfig{
		SigningKey: make([]byte, 32),
	})
	require.NoError(t, err)

	// Generate a valid token
	token, err := service.GenerateAgentToken("agent-123", "grove-456", []AgentTokenScope{ScopeAgentStatusUpdate})
	require.NoError(t, err)

	// Create a test handler that checks for agent context
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims := GetAgentFromContext(r.Context())
		if claims != nil {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(claims.Subject))
		} else {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte("no agent"))
		}
	})

	// Wrap with middleware
	wrapped := service.AgentAuthMiddleware(handler)

	t.Run("valid token in X-Scion-Agent-Token", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Header.Set("X-Scion-Agent-Token", token)
		rr := httptest.NewRecorder()

		wrapped.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)
		assert.Equal(t, "agent-123", rr.Body.String())
	})

	t.Run("valid token in Authorization header", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()

		wrapped.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)
		assert.Equal(t, "agent-123", rr.Body.String())
	})

	t.Run("invalid token", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Header.Set("X-Scion-Agent-Token", "invalid-token")
		rr := httptest.NewRecorder()

		wrapped.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusUnauthorized, rr.Code)
	})

	t.Run("no token passes through", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		rr := httptest.NewRecorder()

		wrapped.ServeHTTP(rr, req)

		// Should pass through (no agent context)
		assert.Equal(t, http.StatusNotFound, rr.Code)
		assert.Equal(t, "no agent", rr.Body.String())
	})
}

func TestRequireAgentScope(t *testing.T) {
	service, err := NewAgentTokenService(AgentTokenConfig{
		SigningKey: make([]byte, 32),
	})
	require.NoError(t, err)

	// Generate a token with only status update scope
	token, err := service.GenerateAgentToken("agent-123", "grove-456", []AgentTokenScope{ScopeAgentStatusUpdate})
	require.NoError(t, err)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	t.Run("has required scope", func(t *testing.T) {
		wrapped := service.AgentAuthMiddleware(RequireAgentScope(ScopeAgentStatusUpdate)(handler))

		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Header.Set("X-Scion-Agent-Token", token)
		rr := httptest.NewRecorder()

		wrapped.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)
	})

	t.Run("missing required scope", func(t *testing.T) {
		wrapped := service.AgentAuthMiddleware(RequireAgentScope(ScopeGroveSecretRead)(handler))

		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Header.Set("X-Scion-Agent-Token", token)
		rr := httptest.NewRecorder()

		wrapped.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusForbidden, rr.Code)
	})

	t.Run("no agent context", func(t *testing.T) {
		wrapped := RequireAgentScope(ScopeAgentStatusUpdate)(handler)

		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		rr := httptest.NewRecorder()

		wrapped.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusUnauthorized, rr.Code)
	})
}

func TestGetAgentFromContext(t *testing.T) {
	t.Run("no agent in context", func(t *testing.T) {
		ctx := context.Background()
		claims := GetAgentFromContext(ctx)
		assert.Nil(t, claims)
	})

	t.Run("agent in context", func(t *testing.T) {
		claims := &AgentTokenClaims{
			GroveID: "grove-123",
		}
		ctx := context.WithValue(context.Background(), agentContextKey{}, claims)
		retrieved := GetAgentFromContext(ctx)
		assert.Equal(t, claims, retrieved)
	})
}
