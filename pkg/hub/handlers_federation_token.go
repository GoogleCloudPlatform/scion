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
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
)

// Token kinds carried in the scion_token_kind claim of tokens signed with
// the hub's OIDC key, so a receiving hub tells a user token from an agent
// token by an explicit claim.
const (
	tokenKindUser  = "user"
	tokenKindAgent = "agent"
)

// permissionFederationTokenMint is the self permission a user access token
// needs to obtain a federation user token.
const permissionFederationTokenMint = "federation_token.mint"

// mutationFederationTokenIssued is the mutation audit type written for each
// issued federation user token.
const mutationFederationTokenIssued = "federation_token_issued"

// federationTokenRefusalMessage is the one 403 message for every refused
// caller, whatever the reason, so the response does not say which check
// failed.
const federationTokenRefusalMessage = "not allowed to obtain a federation token"

// federationTokenAudienceRefusalMessage is the one 403 message for an
// audience this hub does not issue tokens for, including when it issues
// none.
const federationTokenAudienceRefusalMessage = "audience is not allowed"

// federationTokenRequest is the body of POST /api/v1/auth/federation-token.
type federationTokenRequest struct {
	Audience string `json:"audience"`
}

// federationTokenResponse is the response of POST /api/v1/auth/federation-token.
type federationTokenResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// FederationUserTokenClaims are the claims of a federation user token: a
// short-lived token, signed with the hub's OIDC key, that names one local
// user for one peer hub. It never carries agent claims (project_id,
// agent_name, ancestry, root_user).
type FederationUserTokenClaims struct {
	jwt.Claims

	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Name          string `json:"name,omitempty"`
	TokenKind     string `json:"scion_token_kind"`
}

// federationTokenHandler returns the handler for POST
// /api/v1/auth/federation-token, built once at route registration: the
// route answers 404 while federation.project_visitors is off.
func (s *Server) federationTokenHandler() http.HandlerFunc {
	return s.requireExperiment(experiments.FederationProjectVisitors, s.handleFederationToken)
}

// handleFederationToken handles POST /api/v1/auth/federation-token. It
// issues a federation user token for the calling user and the requested
// audience. The caller must be a local human user with status active and a
// stored email, authenticated by an interactive session or by a
// hub-boundary user access token carrying federation_token:mint; every
// other caller gets the same 403. The audience must be listed in
// server.oidc.user_token_audiences. While the OIDC provider is not enabled
// the route answers 404 for every caller.
func (s *Server) handleFederationToken(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if s.oidcKeyManager == nil {
		NotFound(w, "route")
		return
	}

	user, ok := s.federationTokenCaller(w, r)
	if !ok {
		return
	}

	var req federationTokenRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "invalid request body: "+err.Error(), nil)
		return
	}
	audience := config.NormalizeUserTokenAudience(req.Audience)
	if audience == "" {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "audience is required", nil)
		return
	}
	oidcCfg := s.config.OIDCConfig
	if !oidcCfg.AllowsUserTokenAudience(audience) {
		logAuthzDenial(r, GetIdentityFromContext(ctx), Resource{Type: permissions.ResourceFederationToken}, ActionMint, "audience not in server.oidc.user_token_audiences")
		writeError(w, http.StatusForbidden, ErrCodeForbidden, federationTokenAudienceRefusalMessage, nil)
		return
	}
	if s.oidcTokenRateLimiter != nil && !s.oidcTokenRateLimiter.Allow("user:"+user.ID) {
		writeError(w, http.StatusTooManyRequests, ErrCodeRateLimited, "rate limit exceeded for federation token requests", nil)
		return
	}

	// JWT times are whole seconds; truncating here, together with the
	// whole-seconds lifetime that ValidateUserTokens enforces, keeps iat,
	// nbf, exp, the response expires_at and the audit expires_at equal. The
	// audit record keeps the untruncated time.
	issuedAt := time.Now()
	now := issuedAt.Truncate(time.Second)
	expiresAt := now.Add(oidcCfg.EffectiveUserTokenLifetime())
	jtiBytes := make([]byte, 16)
	if _, err := rand.Read(jtiBytes); err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeRuntimeError, "failed to generate token ID", nil)
		return
	}
	jti := base64.RawURLEncoding.EncodeToString(jtiBytes)

	claims := FederationUserTokenClaims{
		Claims: jwt.Claims{
			Issuer:    s.oidcIssuerURL,
			Subject:   user.ID,
			Audience:  jwt.Audience{audience},
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			Expiry:    jwt.NewNumericDate(expiresAt),
			ID:        jti,
		},
		Email:         user.Email,
		EmailVerified: true,
		Name:          user.DisplayName,
		TokenKind:     tokenKindUser,
	}
	token, err := jwt.Signed(s.oidcKeyManager.Signer()).Claims(claims).Serialize()
	if err != nil {
		slog.ErrorContext(ctx, "Failed to sign federation user token", "user_id", user.ID, "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeRuntimeError, "failed to sign federation token", nil)
		return
	}

	// The audit record is written before the token is returned; if it
	// cannot be written, nothing is issued.
	summary, err := json.Marshal(map[string]string{
		"audience":   audience,
		"jti":        jti,
		"expires_at": expiresAt.UTC().Format(time.RFC3339),
	})
	if err != nil {
		InternalError(w)
		return
	}
	record := &store.MutationAuditRecord{
		MutationType: mutationFederationTokenIssued,
		TargetType:   "user",
		TargetID:     user.ID,
		AfterSummary: string(summary),
		Timestamp:    issuedAt,
	}
	s.buildAuditActorFromContext(ctx).ApplyActor(record)
	if err := s.store.CreateMutationAudit(ctx, record); err != nil {
		slog.ErrorContext(ctx, "federation token audit write failed; token not issued", "user_id", user.ID, "error", err)
		InternalError(w)
		return
	}

	slog.InfoContext(ctx, "Federation user token issued",
		"user_id", user.ID,
		"audience", audience,
		"jti", jti,
		"expires_at", expiresAt.UTC().Format(time.RFC3339),
	)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, federationTokenResponse{Token: token, ExpiresAt: expiresAt})
}

// federationTokenCaller returns the stored user of a caller who may obtain
// a federation user token: an interactive session, or a user access token
// whose ceiling allows federation_token.mint on a hub boundary, of a user
// whose stored kind is human, whose status is active and whose stored email
// is not empty. Every other caller (dev, federation, agent, broker,
// delegated, test fixture, a suspended or unknown user, a user with no
// stored email) gets the same 403; no identity gets 401. It writes the
// response and returns false on refusal.
func (s *Server) federationTokenCaller(w http.ResponseWriter, r *http.Request) (*store.User, bool) {
	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if isNilIdentity(identity) {
		writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "authentication required", nil)
		return nil, false
	}
	refuse := func(reason string) (*store.User, bool) {
		logAuthzDenial(r, identity, Resource{Type: permissions.ResourceFederationToken}, ActionMint, reason)
		writeError(w, http.StatusForbidden, ErrCodeForbidden, federationTokenRefusalMessage, nil)
		return nil, false
	}

	switch GetCredentialContextFromContext(ctx).Kind {
	case CredentialKindInteractive, CredentialKindUAT:
	default:
		return refuse("credential kind not admitted for federation token issuance")
	}
	switch identity.(type) {
	case *AuthenticatedUser, *ScopedUserIdentity:
	default:
		return refuse("identity type not admitted for federation token issuance")
	}
	if ok, reason := selfScopedDecision(identity, permissionFederationTokenMint, ""); !ok {
		return refuse("self-scope check: " + reason)
	}

	u, err := s.store.GetUser(ctx, identity.ID())
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return refuse("caller has no stored user")
		}
		slog.ErrorContext(ctx, "federation token: user lookup failed", "user_id", identity.ID(), "error", err)
		InternalError(w)
		return nil, false
	}
	if !u.IsHumanKind() {
		return refuse("stored user kind is not human")
	}
	if u.Status != store.UserStatusActive {
		return refuse("stored user is not active")
	}
	if strings.TrimSpace(u.Email) == "" {
		return refuse("stored user has no email")
	}
	return u, true
}
