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

package oidckeys

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/apierr"
	"github.com/go-jose/go-jose/v4/jwt"
)

// OIDCDiscoveryDocument represents the OpenID Connect Provider Metadata
// returned by the /.well-known/openid-configuration endpoint.
type OIDCDiscoveryDocument struct {
	Issuer                           string   `json:"issuer"`
	JWKSURI                          string   `json:"jwks_uri"`
	ResponseTypesSupported           []string `json:"response_types_supported"`
	SubjectTypesSupported            []string `json:"subject_types_supported"`
	IDTokenSigningAlgValuesSupported []string `json:"id_token_signing_alg_values_supported"`
	ClaimsSupported                  []string `json:"claims_supported"`
	ScopesSupported                  []string `json:"scopes_supported"`
}

// HandleOIDCDiscovery serves the OIDC Provider Metadata at
// GET /.well-known/openid-configuration.
func (h *OIDCHandlers) HandleOIDCDiscovery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		apierr.MethodNotAllowed(w, http.MethodGet)
		return
	}

	issuer := h.d.IssuerURL()

	doc := OIDCDiscoveryDocument{
		Issuer:                           issuer,
		JWKSURI:                          issuer + "/.well-known/jwks.json",
		ResponseTypesSupported:           []string{"id_token"},
		SubjectTypesSupported:            []string{"public"},
		IDTokenSigningAlgValuesSupported: []string{"RS256"},
		ClaimsSupported: []string{
			"iss", "sub", "aud", "iat", "exp", "nbf", "jti",
			"project_id", "agent_name", "ancestry", "root_user",
			"email", "email_verified", "name", "scion_token_kind",
		},
		ScopesSupported: []string{"openid"},
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	if err := json.NewEncoder(w).Encode(doc); err != nil {
		http.Error(w, "failed to encode discovery document", http.StatusInternalServerError)
	}
}

// HandleJWKS serves the JSON Web Key Set at GET /.well-known/jwks.json.
func (h *OIDCHandlers) HandleJWKS(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		apierr.MethodNotAllowed(w, http.MethodGet)
		return
	}

	jwks := h.d.KeyManager().JWKS()

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	if err := json.NewEncoder(w).Encode(jwks); err != nil {
		http.Error(w, "failed to encode JWKS", http.StatusInternalServerError)
	}
}

// ---------------------------------------------------------------------------
// Identity Token Endpoint — POST /api/v1/agent/identity-token
// ---------------------------------------------------------------------------

// IdentityTokenRequest is the request body for POST /api/v1/agent/identity-token.
type IdentityTokenRequest struct {
	Audience string `json:"audience"`
}

// IdentityTokenResponse is the response body for POST /api/v1/agent/identity-token.
type IdentityTokenResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Token kinds carried in the scion_token_kind claim of tokens signed with
// the hub's OIDC key, so a receiving hub tells a user access credential for
// a peer hub from an agent identity token by an explicit claim.
const (
	TokenKindUser  = "user"
	TokenKindAgent = "agent"
)

// OIDCIdentityTokenClaims defines the claims in a Scion OIDC identity token.
type OIDCIdentityTokenClaims struct {
	jwt.Claims

	ProjectID string   `json:"project_id"`
	AgentName string   `json:"agent_name"`
	Ancestry  []string `json:"ancestry"`
	RootUser  string   `json:"root_user"`
	// TokenKind is always "agent" on an agent identity token, so a
	// receiving hub tells it from a user access credential for a peer hub
	// ("user") by an explicit claim.
	TokenKind string `json:"scion_token_kind"`
}

// HandleAgentIdentityToken handles POST /api/v1/agent/identity-token.
// It mints an RS256-signed OIDC identity token for the calling agent.
func (h *OIDCHandlers) HandleAgentIdentityToken(w http.ResponseWriter, r *http.Request) {
	// 1. Extract agent identity from context.
	agent, ok := h.d.CallerAgent(r.Context())
	if !ok {
		apierr.WriteError(w, http.StatusUnauthorized, apierr.ErrCodeUnauthorized, "agent authentication required", nil)
		return
	}

	// 2. Rate limit (per-agent).
	if rl := h.d.TokenRateLimiter(); rl != nil && !rl.Allow(agent.Subject) {
		apierr.WriteError(w, http.StatusTooManyRequests, apierr.ErrCodeRateLimited, "rate limit exceeded for identity token requests", nil)
		return
	}

	// 3. Parse and validate request body.
	var req IdentityTokenRequest
	if err := apierr.ReadJSON(r, &req); err != nil {
		apierr.WriteError(w, http.StatusBadRequest, apierr.ErrCodeInvalidRequest, "invalid request body: "+err.Error(), nil)
		return
	}
	if req.Audience == "" {
		apierr.WriteError(w, http.StatusBadRequest, apierr.ErrCodeInvalidRequest, "audience is required", nil)
		return
	}

	// 4. Check scope: agent must have agent:identity:token scope.
	if !agent.CanRequestIdentityToken {
		apierr.WriteError(w, http.StatusForbidden, apierr.ErrCodeForbidden, "agent not authorized to request identity tokens", nil)
		return
	}

	// 5. Look up agent record from store.
	agentRecord, err := h.d.Agents().GetAgent(r.Context(), agent.Subject)
	if err != nil {
		apierr.NotFound(w, "Agent")
		return
	}

	// 6. Build claims.
	now := time.Now()
	expiresAt := now.Add(h.d.TokenLifetime())

	jti := make([]byte, 16)
	if _, err := rand.Read(jti); err != nil {
		apierr.WriteError(w, http.StatusInternalServerError, apierr.ErrCodeRuntimeError, "failed to generate token ID", nil)
		return
	}

	var rootUser string
	if len(agent.Ancestry) > 0 {
		rootUser = agent.Ancestry[0]
	}

	agentName := agentRecord.Slug
	if agentName == "" {
		agentName = agentRecord.Name
	}

	claims := OIDCIdentityTokenClaims{
		Claims: jwt.Claims{
			Issuer:    h.d.IssuerURL(),
			Subject:   agent.Subject,
			Audience:  jwt.Audience{req.Audience},
			IssuedAt:  jwt.NewNumericDate(now),
			Expiry:    jwt.NewNumericDate(expiresAt),
			NotBefore: jwt.NewNumericDate(now),
			ID:        base64.RawURLEncoding.EncodeToString(jti),
		},
		ProjectID: agent.ProjectID,
		AgentName: agentName,
		Ancestry:  agent.Ancestry,
		RootUser:  rootUser,
		TokenKind: TokenKindAgent,
	}

	// 7. Sign with RS256.
	signer := h.d.KeyManager().Signer()
	token, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		slog.Error("Failed to sign OIDC identity token",
			"agent_id", agent.Subject,
			"error", err,
		)
		apierr.WriteError(w, http.StatusInternalServerError, apierr.ErrCodeRuntimeError, "failed to sign identity token", nil)
		return
	}

	// 8. Audit log.
	slog.Info("OIDC identity token issued",
		"agent_id", agent.Subject,
		"project_id", agent.ProjectID,
		"audience", req.Audience,
		"agent_name", agentName,
		"expires_at", expiresAt.UTC().Format(time.RFC3339),
	)

	// 9. Return 200 with token and expiry.
	apierr.WriteJSON(w, http.StatusOK, IdentityTokenResponse{
		Token:     token,
		ExpiresAt: expiresAt,
	})
}
