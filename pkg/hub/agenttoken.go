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
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

const (
	// AgentTokenIssuer is the issuer claim for agent tokens.
	AgentTokenIssuer = "scion-hub"
	// AgentTokenAudience is the audience claim for agent tokens.
	AgentTokenAudience = "scion-hub-api"
	// DefaultAgentTokenDuration is the default validity duration for agent tokens.
	DefaultAgentTokenDuration = 24 * time.Hour
)

// AgentTokenScope represents the authorized scopes for an agent.
type AgentTokenScope string

const (
	// ScopeAgentStatusUpdate allows the agent to update its own status.
	ScopeAgentStatusUpdate AgentTokenScope = "agent:status:update"
	// ScopeAgentLogAppend allows the agent to append logs.
	ScopeAgentLogAppend AgentTokenScope = "agent:log:append"
	// ScopeGroveSecretRead allows the agent to read grove secrets.
	ScopeGroveSecretRead AgentTokenScope = "grove:secret:read"
)

// AgentTokenClaims represents the custom claims in an agent JWT.
type AgentTokenClaims struct {
	jwt.Claims
	GroveID string            `json:"grove_id,omitempty"`
	Scopes  []AgentTokenScope `json:"scopes,omitempty"`
}

// AgentTokenConfig holds configuration for agent token generation.
type AgentTokenConfig struct {
	// SigningKey is the secret key used for HS256 signing.
	// In production, use RS256 with a proper key pair.
	SigningKey []byte
	// TokenDuration is how long tokens remain valid.
	TokenDuration time.Duration
}

// AgentTokenService handles agent token generation and validation.
type AgentTokenService struct {
	config AgentTokenConfig
	signer jose.Signer
}

// NewAgentTokenService creates a new agent token service.
// If signingKey is empty, a random key is generated (suitable for development).
func NewAgentTokenService(config AgentTokenConfig) (*AgentTokenService, error) {
	if len(config.SigningKey) == 0 {
		// Generate a random key for development/testing
		config.SigningKey = make([]byte, 32)
		if _, err := rand.Read(config.SigningKey); err != nil {
			return nil, fmt.Errorf("failed to generate signing key: %w", err)
		}
	}

	if config.TokenDuration == 0 {
		config.TokenDuration = DefaultAgentTokenDuration
	}

	// Create signer using HS256 (symmetric)
	// In production, consider RS256 (asymmetric) for better security
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.HS256, Key: config.SigningKey},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create signer: %w", err)
	}

	return &AgentTokenService{
		config: config,
		signer: signer,
	}, nil
}

// GenerateAgentToken generates a JWT for an agent with the specified scopes.
func (s *AgentTokenService) GenerateAgentToken(agentID, groveID string, scopes []AgentTokenScope) (string, error) {
	now := time.Now()

	// Default to status update scope if none provided
	if len(scopes) == 0 {
		scopes = []AgentTokenScope{ScopeAgentStatusUpdate}
	}

	claims := AgentTokenClaims{
		Claims: jwt.Claims{
			Issuer:    AgentTokenIssuer,
			Subject:   agentID,
			Audience:  jwt.Audience{AgentTokenAudience},
			IssuedAt:  jwt.NewNumericDate(now),
			Expiry:    jwt.NewNumericDate(now.Add(s.config.TokenDuration)),
			NotBefore: jwt.NewNumericDate(now),
			ID:        generateTokenID(),
		},
		GroveID: groveID,
		Scopes:  scopes,
	}

	token, err := jwt.Signed(s.signer).Claims(claims).Serialize()
	if err != nil {
		return "", fmt.Errorf("failed to sign token: %w", err)
	}

	return token, nil
}

// ValidateAgentToken validates a JWT and returns the claims if valid.
func (s *AgentTokenService) ValidateAgentToken(tokenString string) (*AgentTokenClaims, error) {
	token, err := jwt.ParseSigned(tokenString, []jose.SignatureAlgorithm{jose.HS256})
	if err != nil {
		return nil, fmt.Errorf("failed to parse token: %w", err)
	}

	var claims AgentTokenClaims
	if err := token.Claims(s.config.SigningKey, &claims); err != nil {
		return nil, fmt.Errorf("failed to verify token: %w", err)
	}

	// Validate standard claims
	expected := jwt.Expected{
		Issuer:      AgentTokenIssuer,
		AnyAudience: jwt.Audience{AgentTokenAudience},
		Time:        time.Now(),
	}

	if err := claims.Claims.Validate(expected); err != nil {
		return nil, fmt.Errorf("token validation failed: %w", err)
	}

	return &claims, nil
}

// HasScope checks if the claims include the specified scope.
func (c *AgentTokenClaims) HasScope(scope AgentTokenScope) bool {
	for _, s := range c.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// generateTokenID generates a unique token ID.
func generateTokenID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// agentContextKey is the key for storing agent claims in the request context.
type agentContextKey struct{}

// GetAgentFromContext retrieves the agent claims from the request context.
func GetAgentFromContext(ctx context.Context) *AgentTokenClaims {
	if claims, ok := ctx.Value(agentContextKey{}).(*AgentTokenClaims); ok {
		return claims
	}
	return nil
}

// AgentAuthMiddleware creates middleware that validates agent tokens.
// It looks for tokens in the Authorization header (Bearer) or X-Scion-Agent-Token header.
// If valid, it adds the agent claims to the request context.
func (s *AgentTokenService) AgentAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Try to extract token from headers
		token := extractAgentToken(r)
		if token == "" {
			// No agent token found, continue to next middleware (may have other auth)
			next.ServeHTTP(w, r)
			return
		}

		// Validate the token
		claims, err := s.ValidateAgentToken(token)
		if err != nil {
			writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
				"invalid agent token: "+err.Error(), nil)
			return
		}

		// Add claims to context
		ctx := context.WithValue(r.Context(), agentContextKey{}, claims)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// extractAgentToken extracts the agent token from the request.
// It checks both the Authorization header and X-Scion-Agent-Token header.
func extractAgentToken(r *http.Request) string {
	// Check X-Scion-Agent-Token header first (takes precedence)
	if token := r.Header.Get("X-Scion-Agent-Token"); token != "" {
		return token
	}

	// Check Authorization header for Bearer token
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		return ""
	}

	parts := strings.SplitN(authHeader, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") {
		return ""
	}

	return parts[1]
}

// RequireAgentScope returns a middleware that requires the agent to have a specific scope.
// It must be used after AgentAuthMiddleware.
func RequireAgentScope(scope AgentTokenScope) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims := GetAgentFromContext(r.Context())
			if claims == nil {
				writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
					"agent authentication required", nil)
				return
			}

			if !claims.HasScope(scope) {
				writeError(w, http.StatusForbidden, ErrCodeForbidden,
					fmt.Sprintf("missing required scope: %s", scope), nil)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// RequireAgentSelfAccess returns a middleware that ensures the agent can only access its own resources.
// It extracts the agent ID from the URL path and compares it with the token's subject.
func RequireAgentSelfAccess(pathPrefix string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims := GetAgentFromContext(r.Context())
			if claims == nil {
				writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
					"agent authentication required", nil)
				return
			}

			// Extract agent ID from path
			agentID, _ := extractAction(r, pathPrefix)
			if agentID == "" {
				writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
					"agent ID required in path", nil)
				return
			}

			// Verify the agent is accessing its own resource
			if agentID != claims.Subject {
				writeError(w, http.StatusForbidden, ErrCodeForbidden,
					"agents can only access their own resources", nil)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
