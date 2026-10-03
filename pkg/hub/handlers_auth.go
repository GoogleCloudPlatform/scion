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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// AuthLoginRequest is the request body for /api/v1/auth/login.
type AuthLoginRequest struct {
	Provider      string `json:"provider"`      // "google", "github", etc.
	ProviderToken string `json:"providerToken"` // OAuth access token from provider
	Email         string `json:"email"`         // From OAuth payload
	Name          string `json:"name"`          // Display name
	Avatar        string `json:"avatar"`        // Avatar URL
}

// AuthLoginResponse is the response for /api/v1/auth/login.
type AuthLoginResponse struct {
	User         *UserResponse `json:"user"`
	AccessToken  string        `json:"accessToken"`
	RefreshToken string        `json:"refreshToken"`
	ExpiresIn    int64         `json:"expiresIn"` // Seconds until access token expires
}

// UserResponse is the user info returned in auth responses.
type UserResponse struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"displayName"`
	Role        string `json:"role"`
	AvatarURL   string `json:"avatarUrl,omitempty"`

	// Preferences is populated only by handleAuthMe, from a live store read.
	Preferences *store.UserPreferences `json:"preferences,omitempty"`
}

// AuthTokenRequest is the request body for /api/v1/auth/token.
type AuthTokenRequest struct {
	Provider     string `json:"provider"` // "google", "github", etc.
	Code         string `json:"code"`
	RedirectURI  string `json:"redirectUri"`
	GrantType    string `json:"grantType"`    // "authorization_code"
	CodeVerifier string `json:"codeVerifier"` // PKCE
	ClientType   string `json:"clientType"`   // "web", "cli" - determines token lifetime
}

// AuthTokenResponse is the response for /api/v1/auth/token.
type AuthTokenResponse struct {
	AccessToken  string        `json:"accessToken"`
	RefreshToken string        `json:"refreshToken"`
	ExpiresIn    int64         `json:"expiresIn"`
	TokenType    string        `json:"tokenType"` // "Bearer"
	User         *UserResponse `json:"user"`
}

// AuthRefreshRequest is the request body for /api/v1/auth/refresh.
type AuthRefreshRequest struct {
	RefreshToken string `json:"refreshToken"`
}

// AuthRefreshResponse is the response for /api/v1/auth/refresh.
type AuthRefreshResponse struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresIn    int64  `json:"expiresIn"`
}

// AuthValidateRequest is the request body for /api/v1/auth/validate.
type AuthValidateRequest struct {
	Token string `json:"token"`
}

// AuthValidateResponse is the response for /api/v1/auth/validate.
type AuthValidateResponse struct {
	Valid      bool          `json:"valid"`
	User       *UserResponse `json:"user,omitempty"`
	ExpiresAt  *time.Time    `json:"expiresAt,omitempty"`
	TokenType  string        `json:"tokenType,omitempty"`
	ClientType string        `json:"clientType,omitempty"`
}

// AuthLogoutRequest is the request body for /api/v1/auth/logout.
type AuthLogoutRequest struct {
	RefreshToken string `json:"refreshToken,omitempty"` // Optional: revoke specific token
}

// AuthLogoutResponse is the response for /api/v1/auth/logout.
type AuthLogoutResponse struct {
	Success bool `json:"success"`
}

// CLIAuthAuthorizeRequest is the request body for /api/v1/auth/cli/authorize.
type CLIAuthAuthorizeRequest struct {
	CallbackURL string `json:"callbackUrl"`
	State       string `json:"state"`
	Provider    string `json:"provider,omitempty"` // "google" (default) or "github"
}

// CLIAuthProvidersResponse is the response for GET /api/v1/auth/providers.
type CLIAuthProvidersResponse struct {
	ClientType string   `json:"clientType"`
	Providers  []string `json:"providers"`
}

// CLIAuthAuthorizeResponse is the response for /api/v1/auth/cli/authorize.
type CLIAuthAuthorizeResponse struct {
	URL string `json:"url"`
}

// CLIAuthTokenRequest is the request body for /api/v1/auth/cli/token.
type CLIAuthTokenRequest struct {
	Code        string `json:"code"`
	CallbackURL string `json:"callbackUrl"`
	Provider    string `json:"provider,omitempty"` // "google" (default) or "github"
}

// CLIAuthTokenResponse is the response for /api/v1/auth/cli/token.
type CLIAuthTokenResponse struct {
	AccessToken  string        `json:"accessToken"`
	RefreshToken string        `json:"refreshToken,omitempty"`
	ExpiresIn    int64         `json:"expiresIn"` // seconds
	User         *UserResponse `json:"user,omitempty"`
}

// TokenCreateRequest is the request body for creating a user access token.
//
// The token boundary is named by exactly one of two forms:
//   - Boundary, the explicit form: {"kind":"project","projectId":...} or
//     {"kind":"hub"}.
//   - ProjectID, the project-boundary shorthand.
//
// A request that names neither is rejected; it is never read as a hub
// boundary. A request that uses both forms must name the same project
// boundary in each. A hub boundary with any project ID is rejected.
type TokenCreateRequest struct {
	Name      string                `json:"name"`
	ProjectID string                `json:"projectId"`
	Boundary  *TokenBoundaryRequest `json:"boundary,omitempty"`
	Scopes    []string              `json:"scopes"`
	ExpiresAt *time.Time            `json:"expiresAt,omitempty"`

	// E.1 descriptive credential metadata: optional, bounded, immutable
	// after issuance (there is no update endpoint).
	Purpose string            `json:"purpose,omitempty"`
	Labels  map[string]string `json:"labels,omitempty"`
}

// TokenBoundaryRequest is the explicit boundary form of a token create
// request.
type TokenBoundaryRequest struct {
	Kind      string `json:"kind"`
	ProjectID string `json:"projectId,omitempty"`
}

// TokenBoundaryResponse is the boundary a token was issued under.
type TokenBoundaryResponse struct {
	Kind      string `json:"kind"`
	ProjectID string `json:"projectId,omitempty"`
}

// TokenCreateResponse is the response for creating a user access token.
type TokenCreateResponse struct {
	Token       string         `json:"token"` // Full token, only shown once
	AccessToken *TokenResponse `json:"accessToken"`
}

// TokenResponse is the access token info (without the actual token value).
//
// Boundary is the authoritative boundary. ProjectID is set for project
// tokens and omitted for hub tokens; clients read Boundary.Kind to tell
// the two apart.
type TokenResponse struct {
	ID        string                `json:"id"`
	Name      string                `json:"name"`
	Prefix    string                `json:"prefix"`
	Boundary  TokenBoundaryResponse `json:"boundary"`
	ProjectID string                `json:"projectId,omitempty"`
	Scopes    []string              `json:"scopes"`
	Revoked   bool                  `json:"revoked"`
	ExpiresAt *time.Time            `json:"expiresAt,omitempty"`
	LastUsed  *time.Time            `json:"lastUsed,omitempty"`
	Created   time.Time             `json:"created"`

	// E.1 descriptive credential metadata: empty for tokens created before
	// E.1 or without metadata supplied at issuance.
	Purpose string            `json:"purpose,omitempty"`
	Labels  map[string]string `json:"labels,omitempty"`
}

// ExternalUserInfo carries the provider-verified identity fields needed to
// provision a user. It is a subset of OAuthUserInfo, decoupled from the
// OAuth layer so that provisionUser can serve both OAuth and proxy callers.
type ExternalUserInfo struct {
	Email       string
	DisplayName string
	AvatarURL   string
}

// ErrAccessDenied is returned by provisionUser when the user's email is not
// authorized to log in (domain restriction, invite-only, etc.).
var ErrAccessDenied = errors.New("access denied")

// handleAuthLogin handles POST /api/v1/auth/login.
// This endpoint exchanges an OAuth provider token for Hub-issued tokens.
func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	var req AuthLoginRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "invalid request body")
		return
	}

	// Validate required fields
	if req.Provider == "" || req.ProviderToken == "" {
		ValidationError(w, "missing required fields", map[string]interface{}{
			"required": []string{"provider", "providerToken"},
		})
		return
	}

	provider := strings.ToLower(strings.TrimSpace(req.Provider))
	if !hubclient.IsKnownOAuthProvider(provider) {
		writeError(w, http.StatusBadRequest, "invalid_provider",
			"unsupported OAuth provider", nil)
		return
	}

	if s.oauthService == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented",
			"OAuth is not configured on this server", nil)
		return
	}

	// Validate provider token with upstream provider and derive identity from
	// verified provider response (never trust request-supplied email/profile).
	userInfo, err := s.getDeviceFlowUserInfo(r.Context(), provider, req.ProviderToken)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid_provider_token",
			"failed to validate provider token", nil)
		return
	}

	// Provision user (authorize + find-or-create + hub membership)
	ctx := r.Context()
	user, err := s.provisionUser(ctx, &ExternalUserInfo{
		Email:       userInfo.Email,
		DisplayName: userInfo.DisplayName,
		AvatarURL:   userInfo.AvatarURL,
	})
	if err != nil {
		if errors.Is(err, ErrAccessDenied) {
			writeError(w, http.StatusForbidden, "unauthorized_domain",
				"your email domain is not authorized", nil)
			return
		}
		if errors.Is(err, ErrUserSuspended) {
			writeError(w, http.StatusForbidden, "user_suspended",
				"your account has been suspended", nil)
			return
		}
		InternalError(w)
		return
	}

	// Generate tokens
	if s.userTokenService == nil {
		InternalError(w)
		return
	}

	accessToken, refreshToken, expiresIn, err := s.userTokenService.GenerateTokenPair(
		user.ID, user.Email, user.DisplayName, user.Role, ClientTypeWeb,
	)
	if err != nil {
		InternalError(w)
		return
	}

	writeJSON(w, http.StatusOK, AuthLoginResponse{
		User: &UserResponse{
			ID:          user.ID,
			Email:       user.Email,
			DisplayName: user.DisplayName,
			Role:        user.Role,
			AvatarURL:   user.AvatarURL,
		},
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresIn:    expiresIn,
	})
}

// handleAuthToken handles POST /api/v1/auth/token.
// This endpoint exchanges an OAuth authorization code for tokens.
func (s *Server) handleAuthToken(w http.ResponseWriter, r *http.Request) {
	var req AuthTokenRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "invalid request body")
		return
	}

	// Validate required fields
	if req.Code == "" || req.RedirectURI == "" || req.GrantType == "" {
		ValidationError(w, "missing required fields", map[string]interface{}{
			"required": []string{"code", "redirectUri", "grantType"},
		})
		return
	}

	if req.GrantType != "authorization_code" {
		BadRequest(w, "unsupported grant type")
		return
	}

	// Default provider if not specified in request
	provider := req.Provider
	if provider == "" {
		if strings.Contains(req.RedirectURI, "github") {
			provider = "github"
		} else if s.oauthService != nil {
			provider = s.oauthService.DefaultProviderForClient(OAuthClientTypeWeb)
		} else {
			provider = "google"
		}
		slog.Debug("OAuth provider inferred", "provider", provider)
	}

	// Validate provider is a known value
	if !hubclient.IsKnownOAuthProvider(provider) {
		writeError(w, http.StatusBadRequest, "invalid_provider",
			"unsupported OAuth provider", nil)
		return
	}

	// Map client type string to internal type
	clientType := ClientTypeCLI
	oauthClientType := OAuthClientTypeCLI
	if strings.ToLower(req.ClientType) == "web" {
		clientType = ClientTypeWeb
		oauthClientType = OAuthClientTypeWeb
	}

	// Check if OAuth service is configured
	if s.oauthService == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented",
			"OAuth is not configured on this server", nil)
		return
	}

	// Exchange code for user info
	ctx := r.Context()
	userInfo, err := s.oauthService.ExchangeCodeForClient(ctx, oauthClientType, provider, req.Code, req.RedirectURI)
	if err != nil {
		slog.Error("OAuth code exchange failed", "provider", provider, "error", err)
		writeError(w, http.StatusBadRequest, "oauth_error",
			"failed to exchange authorization code", nil)
		return
	}

	// Provision user (authorize + find-or-create + hub membership)
	user, err := s.provisionUser(ctx, &ExternalUserInfo{
		Email:       userInfo.Email,
		DisplayName: userInfo.DisplayName,
		AvatarURL:   userInfo.AvatarURL,
	})
	if err != nil {
		if errors.Is(err, ErrAccessDenied) {
			writeError(w, http.StatusForbidden, "unauthorized_domain",
				"your email domain is not authorized", nil)
			return
		}
		if errors.Is(err, ErrUserSuspended) {
			writeError(w, http.StatusForbidden, "user_suspended",
				"your account has been suspended", nil)
			return
		}
		InternalError(w)
		return
	}

	// Generate tokens
	if s.userTokenService == nil {
		InternalError(w)
		return
	}

	accessToken, refreshToken, expiresIn, err := s.userTokenService.GenerateTokenPair(
		user.ID, user.Email, user.DisplayName, user.Role, clientType,
	)
	if err != nil {
		InternalError(w)
		return
	}

	writeJSON(w, http.StatusOK, AuthTokenResponse{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresIn:    expiresIn,
		TokenType:    "Bearer",
		User: &UserResponse{
			ID:          user.ID,
			Email:       user.Email,
			DisplayName: user.DisplayName,
			Role:        user.Role,
			AvatarURL:   user.AvatarURL,
		},
	})
}

// handleAuthRefresh handles POST /api/v1/auth/refresh.
func (s *Server) handleAuthRefresh(w http.ResponseWriter, r *http.Request) {
	var req AuthRefreshRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "invalid request body")
		return
	}

	if req.RefreshToken == "" {
		BadRequest(w, "refresh token required")
		return
	}

	if s.userTokenService == nil {
		InternalError(w)
		return
	}

	claims, err := s.userTokenService.ValidateRefreshToken(req.RefreshToken)
	if err != nil {
		writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
			"invalid refresh token", nil)
		return
	}

	// See isReservedPlatformIdentity: every path that provisions a user or
	// mints/re-mints a hub token checks this, including an already-issued
	// refresh token for the configured service account.
	if isReservedPlatformIdentity(claims.Email, s.platformAuthSA) {
		writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
			"invalid refresh token", nil)
		return
	}

	// Re-evaluate admin status on token refresh. The stored role is the source
	// of truth for UI-granted promotions; admin_emails can only add to it.
	//
	// storedRole deliberately starts empty rather than at claims.Role: if the
	// store cannot be read we fall back to config-only evaluation instead of
	// trusting the token's own role claim, which would let a stale admin claim
	// renew itself indefinitely through the rotating refresh chain.
	role := claims.Role
	storedRole := ""
	var user *store.User
	if s.store != nil {
		u, err := s.store.GetUserByEmail(r.Context(), claims.Email)
		switch {
		case err == nil:
			user = u
			storedRole = u.Role
		case errors.Is(err, store.ErrNotFound):
			// The user no longer exists — refuse to mint a new token pair.
			slog.Warn("Refresh rejected: user no longer exists", "email", claims.Email)
			writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
				"invalid refresh token", nil)
			return
		default:
			slog.Warn("Refresh: user lookup failed, falling back to config-only role",
				"email", claims.Email, "error", err)
		}
	}
	if user != nil && user.Status == store.UserStatusSuspended {
		slog.Warn("Refresh rejected: user is suspended", "email", claims.Email, "user_id", user.ID)
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"user account is suspended", nil)
		return
	}
	refreshUserID := ""
	if user != nil {
		refreshUserID = user.ID
	}
	if newRole := s.getUserRole(r.Context(), claims.Email, storedRole, refreshUserID); role != newRole {
		slog.Info("User role changed on token refresh", "email", claims.Email, "old_role", role, "new_role", newRole)
		role = newRole
		// Persist the role change, then bring the super-admin binding and
		// the hub role grants in line with it (mirrors provisionUser). Grant
		// changes are applied only after UpdateUser succeeds so they never
		// diverge from the stored role; grant failures are logged and the
		// refresh continues.
		if user != nil && user.Role != role {
			oldRole := user.Role
			user.Role = role
			if err := s.store.UpdateUser(r.Context(), user); err != nil {
				slog.Error("Failed to persist role change on token refresh",
					"email", claims.Email, "error", err)
			} else {
				if oldRole == store.UserRoleAdmin {
					s.deleteSuperAdminBinding(r.Context(), user.ID)
				} else if role == store.UserRoleAdmin {
					s.ensureSuperAdminBinding(r.Context(), user.ID)
				}
				if err := syncHubRoleGrants(r.Context(), s.store, user.ID, role, store.SystemReconcileCreatedBy); err != nil {
					slog.Warn("failed to sync hub role grants on token refresh",
						"email", claims.Email, "user_id", user.ID, "role", role, "error", err)
				}
			}
		}
	}

	// Prefer the freshly-read user's ID: on delete-and-recreate of the same
	// email, the token's ID refers to the dead record.
	userID := claims.UserID
	if user != nil {
		userID = user.ID
	}

	accessToken, refreshToken, expiresIn, err := s.userTokenService.GenerateTokenPair(
		userID, claims.Email, claims.DisplayName, role, claims.ClientType,
	)
	if err != nil {
		InternalError(w)
		return
	}

	writeJSON(w, http.StatusOK, AuthRefreshResponse{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresIn:    expiresIn,
	})
}

// handleAuthValidate handles POST /api/v1/auth/validate.
func (s *Server) handleAuthValidate(w http.ResponseWriter, r *http.Request) {
	var req AuthValidateRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "invalid request body")
		return
	}

	if req.Token == "" {
		BadRequest(w, "token required")
		return
	}

	if s.userTokenService == nil {
		writeJSON(w, http.StatusOK, AuthValidateResponse{Valid: false})
		return
	}

	claims, err := s.userTokenService.ValidateUserToken(req.Token)
	if err != nil {
		writeJSON(w, http.StatusOK, AuthValidateResponse{Valid: false})
		return
	}

	// See isReservedPlatformIdentity: a token for the reserved identity must
	// not be reported valid, matching the same token's rejection at the
	// UnifiedAuthMiddleware choke point.
	if isReservedPlatformIdentity(claims.Email, s.platformAuthSA) {
		writeJSON(w, http.StatusOK, AuthValidateResponse{Valid: false})
		return
	}

	var expiresAt *time.Time
	if claims.Expiry != nil {
		t := claims.Expiry.Time()
		expiresAt = &t
	}

	writeJSON(w, http.StatusOK, AuthValidateResponse{
		Valid: true,
		User: &UserResponse{
			ID:          claims.UserID,
			Email:       claims.Email,
			DisplayName: claims.DisplayName,
			Role:        claims.Role,
		},
		ExpiresAt:  expiresAt,
		TokenType:  string(claims.TokenType),
		ClientType: string(claims.ClientType),
	})
}

// handleAuthLogout handles POST /api/v1/auth/logout.
// In proxy mode, this is a no-op (the proxy owns the session).
func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	// In proxy mode, the hub does not own the session.
	if s.config.AuthMode == "proxy" {
		writeJSON(w, http.StatusOK, AuthLogoutResponse{Success: true})
		return
	}

	var req AuthLogoutRequest
	_ = json.NewDecoder(r.Body).Decode(&req) // Empty body is fine for logout.

	// TODO: In production, add the refresh token to a blacklist
	// For now, just acknowledge the logout

	writeJSON(w, http.StatusOK, AuthLogoutResponse{Success: true})
}

// handleAuthMe handles GET /api/v1/auth/me.
func (s *Server) handleAuthMe(w http.ResponseWriter, r *http.Request) {
	// Check for agent identity first — agent tokens don't implement UserIdentity
	// but should still be recognized as authenticated callers.
	if agentIdent := GetAgentIdentityFromContext(r.Context()); agentIdent != nil {
		writeJSON(w, http.StatusOK, UserResponse{
			ID:          agentIdent.ID(),
			DisplayName: "agent:" + agentIdent.ID(),
			Role:        "agent",
		})
		return
	}

	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		Unauthorized(w)
		return
	}

	writeJSON(w, http.StatusOK, UserResponse{
		ID:          user.ID(),
		Email:       user.Email(),
		DisplayName: user.DisplayName(),
		Role:        user.Role(),
		Preferences: loadUserPreferences(r.Context(), s.store, user.ID()),
	})
}

// AdminStatusResponse is the response for GET /api/v1/auth/admin-status.
type AdminStatusResponse struct {
	IsAdmin      bool     `json:"isAdmin"`
	IsSuperAdmin bool     `json:"isSuperAdmin"`
	Permissions  []string `json:"permissions"`
}

// handleAuthAdminStatus handles GET /api/v1/auth/admin-status.
// Returns whether the current user has hub-admin or super-admin capabilities,
// along with their effective system-scoped permission IDs.
// This is used by the frontend to decide whether to show admin navigation,
// which admin sections are visible, and to allow access to admin routes.
func (s *Server) handleAuthAdminStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}

	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		Unauthorized(w)
		return
	}

	isSuperAdmin := IsUnscopedLocalPlatformAdmin(user)

	var perms []string
	if isSuperAdmin {
		// Super-admin has all permissions — populate from registry so the
		// frontend shows all UI elements without a separate code path.
		perms = allPermissionIDs()
	} else if s.authzService != nil {
		// Resolve effective permissions from all system-scoped role bindings
		// for this user. This handles both the built-in hub-admin role and
		// any custom roles with partial permissions.
		effective, err := s.authzService.getEffectivePermissions(
			r.Context(),
			store.RoleBindingPrincipalUser, user.ID(),
			store.RoleScopeSystem, "",
		)
		if err != nil {
			slog.Error("failed to resolve effective permissions for admin-status",
				"user_id", user.ID(), "error", err)
			InternalError(w)
			return
		}
		perms = effective
	}

	// Ensure JSON serializes as [] rather than null when there are no permissions.
	if perms == nil {
		perms = []string{}
	} else {
		sort.Strings(perms)
	}

	writeJSON(w, http.StatusOK, AdminStatusResponse{
		IsAdmin:      isSuperAdmin || len(perms) > 0,
		IsSuperAdmin: isSuperAdmin,
		Permissions:  perms,
	})
}

// requireSessionCredential enforces the A1 credential caveat: only interactive
// session or dev credentials may perform token-management operations.
//
// The guard uses the CredentialContext recorded by authentication middleware
// (not identity type), so broker-on-behalf-of, federation, UAT, and agent JWT
// credentials are all rejected even when they present a valid UserIdentity.
// An empty or unknown credential kind is also rejected (fail closed).
func requireSessionCredential(ctx context.Context) error {
	credential := GetCredentialContextFromContext(ctx)
	switch credential.Kind {
	case CredentialKindInteractive, CredentialKindDev:
		return nil
	default:
		// Fail closed: empty, unknown, UAT, agent_jwt, federation, broker.
		return ErrUATCredentialDenied
	}
}

// denyTokenManagement logs and writes the standard 403 for a token-management
// request from a non-session credential — exactly the case where a UAT (or
// other non-interactive credential) attempting to manage access tokens would
// show up (plan §3.4, item 4).
func denyTokenManagement(w http.ResponseWriter, r *http.Request, identity Identity, err error) {
	logAuthzDenial(r, identity, Resource{Type: "user_access_token"}, ActionManage, err.Error())
	writeError(w, http.StatusForbidden, ErrCodeForbidden, err.Error(), nil)
}

// handleTokens routes user access token requests.
func (s *Server) handleTokens(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleListTokens(w, r)
	case http.MethodPost:
		s.handleCreateToken(w, r)
	default:
		MethodNotAllowed(w, http.MethodGet, http.MethodPost)
	}
}

// handleTokenByID routes user access token requests by ID.
func (s *Server) handleTokenByID(w http.ResponseWriter, r *http.Request) {
	// Check for /revoke suffix
	path := r.URL.Path
	base := "/api/v1/auth/tokens/"

	remaining := strings.TrimPrefix(path, base)
	if remaining == "" {
		s.handleTokens(w, r)
		return
	}

	// Check for {id}/revoke pattern
	if parts := strings.SplitN(remaining, "/", 2); len(parts) == 2 && parts[1] == "revoke" {
		if r.Method == http.MethodPost {
			s.handleRevokeToken(w, r, parts[0])
		} else {
			MethodNotAllowed(w, http.MethodPost)
		}
		return
	}

	id := remaining

	switch r.Method {
	case http.MethodGet:
		s.handleGetToken(w, r, id)
	case http.MethodDelete:
		s.handleDeleteToken(w, r, id)
	default:
		MethodNotAllowed(w, http.MethodGet, http.MethodDelete)
	}
}

// handleListTokens handles GET /api/v1/auth/tokens.
func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) {
	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		Unauthorized(w)
		return
	}

	// B4/A1: Credential caveat — only session/dev credentials may manage tokens.
	if err := requireSessionCredential(r.Context()); err != nil {
		denyTokenManagement(w, r, user, err)
		return
	}

	tokens, err := s.uatService.ListTokens(r.Context(), user.ID())
	if err != nil {
		InternalError(w)
		return
	}

	items := make([]TokenResponse, 0, len(tokens))
	for _, t := range tokens {
		items = append(items, tokenToResponse(t))
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"items": items})
}

// handleCreateToken handles POST /api/v1/auth/tokens.
func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		Unauthorized(w)
		return
	}

	// B4/A1: Credential caveat — only session/dev credentials may manage tokens.
	if err := requireSessionCredential(r.Context()); err != nil {
		denyTokenManagement(w, r, user, err)
		return
	}

	var req TokenCreateRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "invalid request body")
		return
	}

	boundary, err := tokenCreateBoundary(req)
	if err != nil {
		writeTokenBoundaryError(w, err)
		return
	}

	key, token, err := s.uatService.CreateTokenWithParams(r.Context(), CreateTokenParams{
		UserID: user.ID(), Name: req.Name, Boundary: boundary, ProjectID: req.ProjectID, Scopes: req.Scopes, ExpiresAt: req.ExpiresAt,
		Metadata: TokenMetadata{Purpose: req.Purpose, Labels: req.Labels},
	})
	if err != nil {
		var metadataErr *ErrInvalidUATMetadata
		switch {
		case errors.Is(err, ErrUATLimitExceeded):
			writeError(w, http.StatusConflict, "limit_exceeded", err.Error(), nil)
		case errors.Is(err, ErrInvalidUATScope):
			ValidationError(w, err.Error(), nil)
		case errors.Is(err, ErrUATExpiryTooLong):
			ValidationError(w, err.Error(), nil)
		case errors.Is(err, ErrUATExpiryPast):
			ValidationError(w, err.Error(), nil)
		case errors.Is(err, ErrUATNameRequired):
			ValidationError(w, err.Error(), nil)
		case errors.Is(err, ErrUATBoundaryRequired), errors.Is(err, ErrUATBoundaryInvalid):
			writeTokenBoundaryError(w, err)
		case errors.Is(err, ErrUATScopeEmpty):
			ValidationError(w, err.Error(), nil)
		case errors.Is(err, ErrUATScopeViolation):
			var details map[string]interface{}
			var violation *UATScopeViolationError
			if errors.As(err, &violation) {
				details = map[string]interface{}{
					"selector": violation.Selector,
					"reason":   string(violation.Reason),
				}
			}
			writeError(w, http.StatusForbidden, "scope_violation", err.Error(), details)
		case errors.Is(err, ErrUATProjectForbidden):
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "forbidden", nil)
		case errors.As(err, &metadataErr):
			// E.1: bounded metadata validation failure. The error message
			// names the field/rule only; it never echoes the offending
			// value (see ErrInvalidUATMetadata).
			ValidationError(w, err.Error(), nil)
		default:
			InternalError(w)
		}
		return
	}

	writeJSON(w, http.StatusCreated, TokenCreateResponse{
		Token:       key,
		AccessToken: tokenResponsePtr(token),
	})
}

// handleGetToken handles GET /api/v1/auth/tokens/{id}.
func (s *Server) handleGetToken(w http.ResponseWriter, r *http.Request, id string) {
	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		Unauthorized(w)
		return
	}

	// B4/A1: Credential caveat — only session/dev credentials may manage tokens.
	if err := requireSessionCredential(r.Context()); err != nil {
		denyTokenManagement(w, r, user, err)
		return
	}

	token, err := s.uatService.GetToken(r.Context(), user.ID(), id)
	if err != nil {
		NotFound(w, "access token")
		return
	}

	resp := tokenToResponse(*token)
	writeJSON(w, http.StatusOK, resp)
}

// handleRevokeToken handles POST /api/v1/auth/tokens/{id}/revoke.
func (s *Server) handleRevokeToken(w http.ResponseWriter, r *http.Request, id string) {
	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		Unauthorized(w)
		return
	}

	// B4/A1: Credential caveat — only session/dev credentials may manage tokens.
	if err := requireSessionCredential(r.Context()); err != nil {
		denyTokenManagement(w, r, user, err)
		return
	}

	if err := s.uatService.RevokeToken(r.Context(), user.ID(), id); err != nil {
		NotFound(w, "access token")
		return
	}

	// Audit is now atomic inside the service (B3/G4).
	w.WriteHeader(http.StatusNoContent)
}

// handleDeleteToken handles DELETE /api/v1/auth/tokens/{id}.
func (s *Server) handleDeleteToken(w http.ResponseWriter, r *http.Request, id string) {
	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		Unauthorized(w)
		return
	}

	// B4/A1: Credential caveat — only session/dev credentials may manage tokens.
	if err := requireSessionCredential(r.Context()); err != nil {
		denyTokenManagement(w, r, user, err)
		return
	}

	if err := s.uatService.DeleteToken(r.Context(), user.ID(), id); err != nil {
		NotFound(w, "access token")
		return
	}

	// Audit is now atomic inside the service (B3/G4).
	w.WriteHeader(http.StatusNoContent)
}

func tokenToResponse(t store.UserAccessToken) TokenResponse {
	resp := TokenResponse{
		ID:        t.ID,
		Name:      t.Name,
		Prefix:    t.Prefix,
		Boundary:  TokenBoundaryResponse{Kind: t.BoundaryKind, ProjectID: t.ProjectID},
		ProjectID: t.ProjectID,
		Scopes:    t.Scopes,
		Revoked:   t.Revoked,
		ExpiresAt: t.ExpiresAt,
		LastUsed:  t.LastUsed,
		Created:   t.Created,
	}
	// E.1 descriptive credential metadata.
	if t.Purpose != nil {
		resp.Purpose = *t.Purpose
	}
	if len(t.Labels) > 0 {
		resp.Labels = t.Labels
	}
	return resp
}

// tokenCreateBoundary returns the explicit boundary named by req, or the
// zero boundary when req uses only the project ID shorthand. A boundary
// object without a kind is rejected. CreateTokenWithParams applies the
// remaining rules (a request naming no boundary, a blank project ID in
// either form, disagreeing forms, invalid boundaries).
func tokenCreateBoundary(req TokenCreateRequest) (TokenBoundary, error) {
	if req.Boundary == nil {
		return TokenBoundary{}, nil
	}
	if req.Boundary.Kind == "" {
		return TokenBoundary{}, ErrUATBoundaryInvalid
	}
	return TokenBoundary{Kind: BoundaryKind(req.Boundary.Kind), ProjectID: req.Boundary.ProjectID}, nil
}

// writeTokenBoundaryError writes the 400 response for a token request
// whose boundary is missing or invalid.
func writeTokenBoundaryError(w http.ResponseWriter, err error) {
	reason := "boundary_invalid"
	if errors.Is(err, ErrUATBoundaryRequired) {
		reason = "boundary_required"
	}
	ValidationError(w, err.Error(), map[string]interface{}{"field": "boundary", "reason": reason})
}

func tokenResponsePtr(t *store.UserAccessToken) *TokenResponse {
	resp := tokenToResponse(*t)
	return &resp
}

// handleCLIAuthAuthorize handles POST /api/v1/auth/cli/authorize.
// This endpoint generates an OAuth authorization URL for CLI login.
func (s *Server) handleCLIAuthAuthorize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}

	var req CLIAuthAuthorizeRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "invalid request body")
		return
	}

	// Validate required fields
	if req.CallbackURL == "" || req.State == "" {
		ValidationError(w, "missing required fields", map[string]interface{}{
			"required": []string{"callbackUrl", "state"},
		})
		return
	}

	// Default provider if not specified
	provider := req.Provider
	if provider == "" {
		if s.oauthService != nil {
			provider = s.oauthService.DefaultProviderForClient(OAuthClientTypeCLI)
		} else {
			provider = "google"
		}
	}

	// Check if OAuth service is configured
	if s.oauthService == nil {
		if s.config.Debug {
			slog.Debug("CLI auth authorize request failed: OAuth service is nil", "provider", provider)
			slog.Debug("Check environment variables SCION_SERVER_OAUTH_CLI_*_CLIENTID/CLIENTSECRET")
		}
		writeError(w, http.StatusNotImplemented, "not_implemented",
			"OAuth is not configured on this server", nil)
		return
	}

	// Check if the requested provider is configured for CLI
	if !s.oauthService.IsProviderConfiguredForClient(OAuthClientTypeCLI, provider) {
		writeError(w, http.StatusBadRequest, ErrCodeValidationError,
			"OAuth provider not configured for CLI: "+provider, nil)
		return
	}

	// Generate authorization URL using CLI OAuth client
	authURL, err := s.oauthService.GetAuthorizationURLForClient(OAuthClientTypeCLI, provider, req.CallbackURL, req.State)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "oauth_error",
			"failed to generate authorization URL: "+err.Error(), nil)
		return
	}

	writeJSON(w, http.StatusOK, CLIAuthAuthorizeResponse{
		URL: authURL,
	})
}

// handleCLIAuthProviders handles GET /api/v1/auth/providers.
// This endpoint returns configured OAuth providers for a given client type.
func (s *Server) handleCLIAuthProviders(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}

	clientTypeParam := strings.TrimSpace(r.URL.Query().Get("clientType"))
	if clientTypeParam == "" {
		ValidationError(w, "missing required query parameter: clientType", map[string]interface{}{
			"required": []string{"clientType"},
		})
		return
	}

	var clientType OAuthClientType
	switch clientTypeParam {
	case string(OAuthClientTypeWeb):
		clientType = OAuthClientTypeWeb
	case string(OAuthClientTypeCLI):
		clientType = OAuthClientTypeCLI
	case string(OAuthClientTypeDevice):
		clientType = OAuthClientTypeDevice
	default:
		ValidationError(w, "invalid clientType", map[string]interface{}{
			"allowed": []string{string(OAuthClientTypeWeb), string(OAuthClientTypeCLI), string(OAuthClientTypeDevice)},
		})
		return
	}

	resp := CLIAuthProvidersResponse{
		ClientType: clientTypeParam,
		Providers:  []string{},
	}
	// In proxy mode, no OAuth providers are available.
	if s.config.AuthMode != "proxy" && s.oauthService != nil {
		resp.Providers = s.oauthService.ConfiguredProvidersForClient(clientType)
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleCLIAuthToken handles POST /api/v1/auth/cli/token.
// This endpoint exchanges an OAuth authorization code for Hub tokens.
func (s *Server) handleCLIAuthToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}

	var req CLIAuthTokenRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "invalid request body")
		return
	}

	// Validate required fields
	if req.Code == "" || req.CallbackURL == "" {
		ValidationError(w, "missing required fields", map[string]interface{}{
			"required": []string{"code", "callbackUrl"},
		})
		return
	}

	// Default provider if not specified
	provider := req.Provider
	if provider == "" {
		if s.oauthService != nil {
			provider = s.oauthService.DefaultProviderForClient(OAuthClientTypeCLI)
		} else {
			provider = "google"
		}
	}

	// Check if OAuth service is configured
	if s.oauthService == nil {
		if s.config.Debug {
			slog.Debug("CLI auth token exchange failed: OAuth service is nil", "provider", provider)
		}
		writeError(w, http.StatusNotImplemented, "not_implemented",
			"OAuth is not configured on this server", nil)
		return
	}

	// Exchange code for user info using CLI OAuth client
	ctx := r.Context()
	userInfo, err := s.oauthService.ExchangeCodeForClient(ctx, OAuthClientTypeCLI, provider, req.Code, req.CallbackURL)
	if err != nil {
		slog.Error("CLI OAuth code exchange failed", "provider", provider, "error", err)
		writeError(w, http.StatusBadRequest, "oauth_error",
			"failed to exchange authorization code", nil)
		return
	}

	// Provision user (authorize + find-or-create + hub membership)
	user, err := s.provisionUser(ctx, &ExternalUserInfo{
		Email:       userInfo.Email,
		DisplayName: userInfo.DisplayName,
		AvatarURL:   userInfo.AvatarURL,
	})
	if err != nil {
		if errors.Is(err, ErrAccessDenied) {
			writeError(w, http.StatusForbidden, "unauthorized_domain",
				"your email domain is not authorized", nil)
			return
		}
		if errors.Is(err, ErrUserSuspended) {
			writeError(w, http.StatusForbidden, "user_suspended",
				"your account has been suspended", nil)
			return
		}
		InternalError(w)
		return
	}

	// Generate Hub tokens (CLI type for longer duration)
	if s.userTokenService == nil {
		InternalError(w)
		return
	}

	accessToken, refreshToken, expiresIn, err := s.userTokenService.GenerateTokenPair(
		user.ID, user.Email, user.DisplayName, user.Role, ClientTypeCLI,
	)
	if err != nil {
		InternalError(w)
		return
	}

	writeJSON(w, http.StatusOK, CLIAuthTokenResponse{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresIn:    expiresIn,
		User: &UserResponse{
			ID:          user.ID,
			Email:       user.Email,
			DisplayName: user.DisplayName,
			Role:        user.Role,
			AvatarURL:   user.AvatarURL,
		},
	})
}

// CLIDeviceAuthorizeRequest is the request body for /api/v1/auth/cli/device.
type CLIDeviceAuthorizeRequest struct {
	Provider string `json:"provider,omitempty"`
}

// CLIDeviceAuthorizeResponse is the response for /api/v1/auth/cli/device.
type CLIDeviceAuthorizeResponse struct {
	DeviceCode              string `json:"deviceCode"`
	UserCode                string `json:"userCode"`
	VerificationURL         string `json:"verificationUrl"`
	VerificationURLComplete string `json:"verificationUrlComplete,omitempty"`
	ExpiresIn               int    `json:"expiresIn"`
	Interval                int    `json:"interval"`
}

// CLIDeviceTokenRequest is the request body for /api/v1/auth/cli/device/token.
type CLIDeviceTokenRequest struct {
	DeviceCode string `json:"deviceCode"`
	Provider   string `json:"provider,omitempty"`
}

// CLIDeviceTokenResponse is the response for /api/v1/auth/cli/device/token.
type CLIDeviceTokenResponse struct {
	// Pending/error states:
	Status   string `json:"status,omitempty"`
	Interval int    `json:"interval,omitempty"`
	// Success (same shape as CLIAuthTokenResponse):
	AccessToken  string        `json:"accessToken,omitempty"`
	RefreshToken string        `json:"refreshToken,omitempty"`
	ExpiresIn    int64         `json:"expiresIn,omitempty"`
	User         *UserResponse `json:"user,omitempty"`
}

// handleCLIDeviceAuthorize handles POST /api/v1/auth/cli/device.
// This endpoint initiates the device authorization flow.
func (s *Server) handleCLIDeviceAuthorize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}

	var req CLIDeviceAuthorizeRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "invalid request body")
		return
	}

	provider := req.Provider
	if provider == "" {
		if s.oauthService != nil {
			provider = s.oauthService.DefaultProviderForClient(OAuthClientTypeDevice)
		} else {
			provider = "google"
		}
	}

	if s.oauthService == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented",
			"OAuth is not configured on this server", nil)
		return
	}

	if !s.oauthService.IsProviderConfiguredForClient(OAuthClientTypeDevice, provider) {
		writeError(w, http.StatusBadRequest, ErrCodeValidationError,
			"OAuth provider not configured for device flow: "+provider, nil)
		return
	}

	codeResp, err := s.oauthService.RequestDeviceCode(r.Context(), OAuthClientTypeDevice, provider)
	if err != nil {
		slog.Error("Device code request failed", "provider", provider, "error", err)
		writeError(w, http.StatusInternalServerError, "oauth_error",
			"failed to request device code", nil)
		return
	}

	writeJSON(w, http.StatusOK, CLIDeviceAuthorizeResponse{
		DeviceCode:              codeResp.DeviceCode,
		UserCode:                codeResp.UserCode,
		VerificationURL:         codeResp.VerificationURI,
		VerificationURLComplete: codeResp.VerificationURIComplete,
		ExpiresIn:               codeResp.ExpiresIn,
		Interval:                codeResp.Interval,
	})
}

// handleCLIDeviceToken handles POST /api/v1/auth/cli/device/token.
// This endpoint polls for the device authorization result.
func (s *Server) handleCLIDeviceToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}

	var req CLIDeviceTokenRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "invalid request body")
		return
	}

	if req.DeviceCode == "" {
		ValidationError(w, "missing required field: deviceCode", nil)
		return
	}

	provider := req.Provider
	if provider == "" {
		if s.oauthService != nil {
			provider = s.oauthService.DefaultProviderForClient(OAuthClientTypeDevice)
		} else {
			provider = "google"
		}
	}

	if s.oauthService == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented",
			"OAuth is not configured on this server", nil)
		return
	}

	ctx := r.Context()
	tokenResp, err := s.oauthService.PollDeviceToken(ctx, OAuthClientTypeDevice, provider, req.DeviceCode)
	if err != nil {
		// Check if it's a device auth error (pending, slow_down, expired)
		if authErr, ok := err.(*DeviceAuthError); ok {
			switch authErr.Code {
			case "authorization_pending":
				writeJSON(w, http.StatusAccepted, CLIDeviceTokenResponse{
					Status: "authorization_pending",
				})
				return
			case "expired_token":
				writeJSON(w, http.StatusGone, CLIDeviceTokenResponse{
					Status: "expired_token",
				})
				return
			case "slow_down":
				writeJSON(w, http.StatusTooManyRequests, CLIDeviceTokenResponse{
					Status:   "slow_down",
					Interval: authErr.Interval,
				})
				return
			}
		}
		slog.Error("Device token poll failed", "provider", provider, "error", err)
		writeError(w, http.StatusBadRequest, "oauth_error",
			"failed to poll device token", nil)
		return
	}

	// Success — get user info from provider and complete login
	userInfo, err := s.getDeviceFlowUserInfo(ctx, provider, tokenResp.AccessToken)
	if err != nil {
		slog.Error("Failed to get user info from device flow token", "provider", provider, "error", err)
		writeError(w, http.StatusInternalServerError, "oauth_error",
			"failed to get user info", nil)
		return
	}

	s.completeOAuthLogin(w, r, userInfo)
}

// getDeviceFlowUserInfo retrieves user info from the provider using an access token.
func (s *Server) getDeviceFlowUserInfo(ctx context.Context, provider, accessToken string) (*OAuthUserInfo, error) {
	switch provider {
	case "google":
		return s.oauthService.getGoogleUserInfo(ctx, accessToken)
	case "github":
		return s.oauthService.getGitHubUserInfo(ctx, accessToken)
	default:
		return nil, fmt.Errorf("unsupported provider: %s", provider)
	}
}

// completeOAuthLogin is the shared logic for completing an OAuth login
// after user info has been obtained from the provider.
func (s *Server) completeOAuthLogin(w http.ResponseWriter, r *http.Request, userInfo *OAuthUserInfo) {
	ctx := r.Context()

	// Provision user (authorize + find-or-create + hub membership)
	user, err := s.provisionUser(ctx, &ExternalUserInfo{
		Email:       userInfo.Email,
		DisplayName: userInfo.DisplayName,
		AvatarURL:   userInfo.AvatarURL,
	})
	if err != nil {
		if errors.Is(err, ErrAccessDenied) {
			writeError(w, http.StatusForbidden, "unauthorized_domain",
				"your email domain is not authorized", nil)
			return
		}
		if errors.Is(err, ErrUserSuspended) {
			writeError(w, http.StatusForbidden, "user_suspended",
				"your account has been suspended", nil)
			return
		}
		InternalError(w)
		return
	}

	// Generate Hub tokens (CLI type for longer duration)
	if s.userTokenService == nil {
		InternalError(w)
		return
	}

	accessToken, refreshToken, expiresIn, err := s.userTokenService.GenerateTokenPair(
		user.ID, user.Email, user.DisplayName, user.Role, ClientTypeCLI,
	)
	if err != nil {
		InternalError(w)
		return
	}

	writeJSON(w, http.StatusOK, CLIAuthTokenResponse{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresIn:    expiresIn,
		User: &UserResponse{
			ID:          user.ID,
			Email:       user.Email,
			DisplayName: user.DisplayName,
			Role:        user.Role,
			AvatarURL:   user.AvatarURL,
		},
	})
}

// ErrUserSuspended is returned by provisionUser when the user's account is suspended.
var ErrUserSuspended = errors.New("user account is suspended")

// provisionUser authorizes the external user, then finds or creates the
// corresponding store.User. It returns ErrAccessDenied when the email is not
// authorized, or ErrUserSuspended when the user is suspended.
// On success it also ensures hub-members group membership.
func (s *Server) provisionUser(ctx context.Context, info *ExternalUserInfo) (*store.User, error) {
	// The hub does not create or authenticate user accounts for its
	// configured transport service account. Checked before authorization and
	// before find-or-create so this covers both a brand-new identity and an
	// already-existing user row for that email.
	if isReservedPlatformIdentity(info.Email, s.platformAuthSA) {
		return nil, ErrAccessDenied
	}

	// Authorization check
	if !s.isUserAuthorized(ctx, info.Email) {
		reason := "not_on_allow_list"
		if s.UserAccessMode() != "invite_only" {
			reason = "domain_not_authorized"
		}
		LogInviteAuditFailure(ctx, s.auditLogger, InviteAuditLoginDenied, info.Email, reason)
		return nil, ErrAccessDenied
	}

	// Find or create user
	user, err := s.store.GetUserByEmail(ctx, info.Email)
	if err != nil {
		// Create new user
		user = &store.User{
			ID:          generateID(),
			Email:       info.Email,
			DisplayName: info.DisplayName,
			AvatarURL:   info.AvatarURL,
			Role:        s.getUserRole(ctx, info.Email, "", ""),
			Status:      "active",
			Created:     time.Now(),
			LastLogin:   time.Now(),
		}
		if err := s.store.CreateUser(ctx, user); err != nil {
			return nil, fmt.Errorf("create user: %w", err)
		}
		// Cold-start fix: when a new user is provisioned as admin,
		// immediately create the system-scoped super-admin RoleBinding.
		// ReconcileSuperAdminBindings already ran at startup against an
		// empty store and won't run again until the next restart.
		if user.Role == "admin" {
			s.ensureSuperAdminBinding(ctx, user.ID)
		}
		// Make the hub-members group and hub-viewer binding match the role
		// now, so the user's first request already has the right hub
		// permissions. Best-effort: a grant write failure degrades the
		// session but must not fail the login; the startup backfill repairs
		// it.
		if err := syncHubRoleGrants(ctx, s.store, user.ID, user.Role, store.SystemReconcileCreatedBy); err != nil {
			slog.Warn("failed to sync hub role grants on login", "email", info.Email, "user_id", user.ID, "role", user.Role, "error", err)
		}
		return user, nil
	}

	// Existing record: apply the same live sign-in policy and account-state
	// handling every sign-in path shares (see applyLiveSignInPolicy in
	// sign_in_policy.go). The sign-in policy was already checked above for
	// both branches of this function, so preAuthorized=true here — the
	// helper still enforces suspension unconditionally.
	user, err = applyLiveSignInPolicy(ctx, s.signInPolicyDeps(), user, info.DisplayName, info.AvatarURL, true, signInPolicyPersistOpts{AlwaysPersist: true})
	if err != nil {
		return nil, err
	}
	return user, nil
}

// signInPolicyDeps returns the applyLiveSignInPolicy callbacks backed by
// this Server's config and store. Used by provisionUser for the
// existing-record case; kept faithful to provisionUser's pre-refactor
// inline logic field for field.
func (s *Server) signInPolicyDeps() signInPolicyDeps {
	return signInPolicyDeps{
		authorize: s.isUserAuthorized,
		activationRole: func(ctx context.Context, email, currentRole, userID string) string {
			// The stored role on an invited row is a placeholder (invites
			// carry no role), so evaluate as a brand-new user: admin if in
			// admin_emails, otherwise the current default_user_role. The one
			// carve-out is a pending invite that was already promoted
			// through the admin UI; keep that admin.
			activation := ""
			uiPromoted := currentRole == store.UserRoleAdmin && hasUIPromotedBinding(ctx, s.store, userID)
			if uiPromoted {
				activation = currentRole
			}
			return determineUserRole(email, s.AdminEmails(), activation, s.demotionSafe.Load(), uiPromoted, s.DefaultUserRole())
		},
		roleFor: func(ctx context.Context, email, currentRole, userID string) string {
			// Re-evaluate admin status on every sign-in.
			// D11-fix2: when demotion actually happens (admin → non-admin),
			// also delete the super-admin binding so IsSystemAdmin
			// immediately agrees with IsUnscopedLocalPlatformAdmin. The
			// empty-list guard is inherited: determineUserRole refuses to
			// demote when AdminEmails is nil/empty.
			return s.getUserRole(ctx, email, currentRole, userID)
		},
		UpdateUser:              s.store.UpdateUser,
		ensureSuperAdminBinding: s.ensureSuperAdminBinding,
		deleteSuperAdminBinding: s.deleteSuperAdminBinding,
		syncGrants: func(ctx context.Context, userID, role string) error {
			return syncHubRoleGrants(ctx, s.store, userID, role, store.SystemReconcileCreatedBy)
		},
		auditActivated: func(ctx context.Context, email, userID string) {
			LogInviteAudit(ctx, s.auditLogger, InviteAuditUserActivated, email, "", userID, email, nil)
		},
		// auditDenied mirrors provisionUser's own denial-reason logic above.
		// provisionUser calls the helper with preAuthorized=true, so this
		// only fires from a caller (the resolver) that passes
		// preAuthorized=false and fails the authorize check — provisionUser
		// itself already audited its denial before ever reaching the helper,
		// so there is no double-audit for that path.
		//
		// Volume note: the external-bearer path validates a cached credential
		// on every request, so a token that keeps failing the sign-in policy
		// writes one denial record per request for as long as the caller
		// keeps presenting it. That is accepted deliberately: a policy
		// denial is exactly the kind of event the audit trail exists to
		// capture, an unauthorized caller retrying is expected to stop or be
		// blocked at a layer above this one, and de-duplicating here would
		// mean dropping legitimate repeat-denial records.
		auditDenied: func(ctx context.Context, email string) {
			reason := "not_on_allow_list"
			if s.UserAccessMode() != "invite_only" {
				reason = "domain_not_authorized"
			}
			LogInviteAuditFailure(ctx, s.auditLogger, InviteAuditLoginDenied, email, reason)
		},
	}
}

// generateID generates a new UUID.
func generateID() string {
	return uuid.New().String()
}

// isUserAuthorized checks whether a user is permitted to log in based on
// admin_emails, authorized_domains, and user_access_mode (allow list).
func (s *Server) isUserAuthorized(ctx context.Context, email string) bool {
	return checkUserAuthorized(ctx, email, s.AuthorizedDomains(), s.AdminEmails(), s.UserAccessMode(), s.store)
}

// checkUserAuthorized is a package-level authorization check used by both
// Server and WebServer to enforce admin bypass, domain, and access mode rules.
func checkUserAuthorized(ctx context.Context, email string, authorizedDomains, adminEmails []string, accessMode string, st store.Store) bool {
	emailLower := strings.ToLower(email)

	// Admin emails always bypass all checks
	for _, admin := range adminEmails {
		if strings.ToLower(admin) == emailLower {
			return true
		}
	}

	// Domain check (applies when authorized_domains is configured)
	if len(authorizedDomains) > 0 {
		if !isEmailInDomains(emailLower, authorizedDomains) {
			return false
		}
	}

	// Access mode check
	switch accessMode {
	case "invite_only":
		if st == nil {
			slog.Error("user authorization check failed: store is nil", "email", emailLower)
			return false
		}
		found, err := st.IsUserInvitedOrActive(ctx, emailLower)
		if err != nil {
			slog.Error("user authorization check failed", "email", emailLower, "error", err)
			return false
		}
		return found
	case "domain_restricted":
		if len(authorizedDomains) == 0 {
			slog.Warn("user_access_mode is domain_restricted but no authorized_domains configured; all users will be blocked",
				"email", emailLower)
		}
		return len(authorizedDomains) > 0
	default: // "open" or empty
		return true
	}
}

// isEmailInDomains checks if the email's domain matches any authorized domain.
func isEmailInDomains(emailLower string, authorizedDomains []string) bool {
	atIndex := strings.LastIndex(emailLower, "@")
	if atIndex == -1 {
		return false
	}
	domain := emailLower[atIndex+1:]

	for _, authorized := range authorizedDomains {
		authorizedLower := strings.ToLower(authorized)
		if authorizedLower == domain {
			return true
		}
		if strings.HasPrefix(authorizedLower, "*.") {
			suffix := authorizedLower[1:]
			if strings.HasSuffix(domain, suffix) {
				return true
			}
		}
	}
	return false
}

// determineUserRole returns the role for a user based on their email and the
// role they currently hold.
//
// The adminEmails config list is the sole authority for the "admin" role:
//   - Present in adminEmails → always "admin" (promotion).
//   - Absent from adminEmails AND currentRole is "admin" → demoted to
//     defaultRole (D11: removal from AdminEmails is no longer a no-op).
//   - Absent from adminEmails AND currentRole is anything else → preserved
//     verbatim (including "viewer" and any role added in future).
//   - No stored role (new user) → defaultRole (from config; defaults to "member").
//
// This function intentionally does NOT demote non-admin roles: a "viewer" set
// through the admin UI stays "viewer". Only the "admin" role is owned by
// config; all other roles are owned by the database.
//
// demotionSafe gates whether demotion is permitted: it is set to true by the
// startup reconciler only when the intended admin set is non-empty. When false
// (reconciler refused demotions, never ran, or errored), login-time demotion
// is blocked to prevent one-at-a-time admin loss through the interactive path.
//
// currentRole is the user's role as stored in the database; pass "" for a user
// that does not exist yet.
//
// defaultRole is the configured default role for new users (from
// auth.default_user_role). Only "member" and "viewer" are accepted; any other
// value (including "admin") falls back to "member".
func determineUserRole(email string, adminEmails []string, currentRole string, demotionSafe bool, isUIPromoted bool, defaultRole string) string {
	emailLower := strings.ToLower(email)
	for _, adminEmail := range adminEmails {
		if strings.ToLower(adminEmail) == emailLower {
			return "admin"
		}
	}
	// D11: if the user currently holds "admin" but is no longer in adminEmails,
	// demote to defaultRole. The admin role is owned by config, not by the
	// store. Using defaultRole ensures that when the org configures "viewer"
	// as default, a demoted admin does not land at a higher privilege than
	// new users would receive.
	//
	// UI-promoted guard: admins promoted via the admin API (AdminAPICreatedBy
	// binding) are immune to login-time demotion — their authority comes from
	// an explicit admin action, not from the AdminEmails config.
	//
	// Empty-list safety: when adminEmails is nil or empty, do NOT demote.
	// An empty list is almost always a config load failure, not an instruction
	// to remove every administrator. The reconciliation function has its own
	// empty-list guard, but login-time evaluation must be equally safe.
	//
	// Reconciler guard: when demotionSafe is false (reconciler refused batch
	// demotions because the intended admin set was empty, or reconciler never
	// ran), refuse login-time demotion too — the same condition that prevents
	// mass demotion at startup must prevent one-at-a-time demotion at login.
	if currentRole == "admin" && len(adminEmails) > 0 && demotionSafe && !isUIPromoted {
		return normalizedDefaultRole(defaultRole)
	}
	if currentRole != "" {
		return currentRole
	}
	// New user: use the configured default role. Only "member" and "viewer"
	// are accepted; anything else (including "admin") falls back to "member"
	// to prevent config-driven admin escalation.
	return normalizedDefaultRole(defaultRole)
}

// normalizedDefaultRole maps the configured default_user_role to the role a
// user actually gets when it applies (new user, invite activation, or admin
// demotion at login or at startup reconciliation): "viewer" stays "viewer",
// anything else (including "" and "admin") is "member". It is the single
// place this rule lives, so login-time and startup demotion cannot disagree.
func normalizedDefaultRole(defaultRole string) string {
	if defaultRole == store.UserRoleViewer {
		return store.UserRoleViewer
	}
	return store.UserRoleMember
}

// (s *Server) getUserRole is a convenience method to determine role using server config.
// It consults the process-level demotionSafe flag set by the startup reconciler:
// when the reconciler refused demotions (zero intended admins, empty config, or
// reconciler never ran), demotionSafe is false and login-time demotion is blocked
// to prevent one-at-a-time admin loss through the interactive path.
//
// userID may be empty for new users (no bindings to check). When non-empty and
// the current role is "admin", the store is queried for an AdminAPICreatedBy
// super-admin binding to protect UI-promoted admins from demotion.
func (s *Server) getUserRole(ctx context.Context, email, currentRole, userID string) string {
	uiPromoted := false
	if currentRole == "admin" && userID != "" {
		uiPromoted = hasUIPromotedBinding(ctx, s.store, userID)
	}
	return determineUserRole(email, s.AdminEmails(), currentRole, s.demotionSafe.Load(), uiPromoted, s.DefaultUserRole())
}

// hasUIPromotedBinding reports whether the user has a system-scoped super-admin
// role binding created via the admin API (AdminAPICreatedBy). Such users were
// promoted by an admin through the UI and must not be demoted by config changes.
func hasUIPromotedBinding(ctx context.Context, st store.Store, userID string) bool {
	if st == nil {
		return false
	}
	rd, err := st.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	if err != nil {
		return false
	}
	bindings, err := st.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
	if err != nil {
		return false
	}
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeSystem && b.RoleDefinitionID == rd.ID && b.CreatedBy == store.AdminAPICreatedBy {
			return true
		}
	}
	return false
}

// handleInviteRedeem handles POST /api/v1/auth/invite/redeem.
func (s *Server) handleInviteRedeem(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}

	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "authentication required", nil)
		return
	}

	var req struct {
		Code string `json:"code"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "invalid request body", nil)
		return
	}

	if req.Code == "" {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "code is required", nil)
		return
	}

	if s.inviteService == nil {
		InternalError(w)
		return
	}

	// Check authorized domains before allowing redemption
	authorizedDomains := s.AuthorizedDomains()
	if len(authorizedDomains) > 0 {
		if !isEmailInDomains(strings.ToLower(user.Email()), authorizedDomains) {
			writeError(w, http.StatusForbidden, "unauthorized_domain",
				"your email domain is not authorized to join this hub", nil)
			return
		}
	}

	invite, err := s.inviteService.RedeemCode(r.Context(), req.Code, user.Email(), user.ID())
	if err != nil {
		switch {
		case errors.Is(err, ErrInviteInvalidFormat):
			writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "invalid invite code format", nil)
		case errors.Is(err, ErrInviteNotFound),
			errors.Is(err, ErrInviteExpired),
			errors.Is(err, ErrInviteRevoked),
			errors.Is(err, ErrInviteExhausted):
			writeError(w, http.StatusNotFound, ErrCodeNotFound, "invite code not found or no longer valid", nil)
		default:
			slog.Error("invite redemption failed", "error", err)
			InternalError(w)
		}
		return
	}

	slog.Info("invite code redeemed",
		"invite_id", invite.ID,
		"email", user.Email(),
	)
	LogInviteAudit(r.Context(), s.auditLogger, InviteAuditInviteRedeemed, user.Email(), invite.ID, user.ID(), user.Email(), nil)
	s.events.PublishInviteChanged(r.Context(), "redeemed", invite.ID, invite.CodePrefix)
	s.events.PublishAllowListChanged(r.Context(), "added", user.Email())

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"message": "You have been added to the hub.",
		"user": map[string]string{
			"id":    user.ID(),
			"email": user.Email(),
		},
	})
}

// TokenBoundaryDTO is the wire shape for a UAT boundary: {"kind":"project",
// "projectId":"..."} for a project boundary, or {"kind":"hub"} (projectId
// omitted) for a hub boundary. This is the shared shape referenced from both
// the scopes-eligibility response below and the token create/response
// boundary field; keep both call sites on this one type rather than two
// independently drifting json structs.
type TokenBoundaryDTO struct {
	Kind      string `json:"kind"`
	ProjectID string `json:"projectId,omitempty"`
}

func tokenBoundaryToDTO(b TokenBoundary) TokenBoundaryDTO {
	dto := TokenBoundaryDTO{Kind: string(b.Kind)}
	if b.Kind == BoundaryKindProject {
		dto.ProjectID = b.ProjectID
	}
	return dto
}

// ScopeEligibility answers, for one selector and one requested boundary,
// only "may the authenticated user select this restriction" -- never a
// capability and never a target list. Eligible is computed fresh from the
// principal's current authority; it is never widened by any credential the
// caller happens to present.
type ScopeEligibility struct {
	Boundary TokenBoundaryDTO `json:"boundary"`
	Eligible bool             `json:"eligible"`
	// Reason is a MintDenialReason code, present only when !Eligible,
	// including "project_access_required": that code appears here
	// whenever at least one OTHER selector in the same request was
	// admitted, since the caller already knows the project exists in that
	// case. It appears ONLY as the request's uniform 403 -- never here --
	// when EVERY evaluated selector was denied for project access; see
	// handleAuthScopes.
	Reason string `json:"reason,omitempty"`
	Note   string `json:"note,omitempty"`
	// IneligibleMembers is set only on an alias entry (AuthScopeAlias): the
	// subset of ExpandsTo that is not eligible. An alias is Eligible only
	// when every member is.
	IneligibleMembers []string `json:"ineligibleMembers,omitempty"`
}

// AuthScopeEntry is a single UAT scope in the scopes endpoint response.
type AuthScopeEntry struct {
	ID          string `json:"id"`
	Resource    string `json:"resource"`
	Action      string `json:"action"`
	Description string `json:"description"`

	// PermissionID is the canonical registry permission ID (e.g.
	// "agent.attach") this selector resolves to via
	// permissions.ResolveSelector.
	PermissionID string `json:"permissionId,omitempty"`
	// AllowedBoundaries mirrors permissions.PermissionAllowedBoundaries for
	// this selector: which TokenBoundary kinds may select it at mint time.
	AllowedBoundaries []string `json:"allowedBoundaries,omitempty"`
	// EligibilityKind is "flat_role" (the project role's own flat
	// permission subset, the default for a selector absent from
	// permissions.MintEligibilityRegistry) or "relationship" (also
	// mintable via a declared owner/ancestor relationship candidacy, with
	// no target required to exist yet).
	EligibilityKind string `json:"eligibilityKind,omitempty"`
	// Relationships names the candidate relationship types (e.g. "owner",
	// "ancestor") when EligibilityKind is "relationship".
	Relationships []string `json:"relationships,omitempty"`
	// RequiresExistingTarget is always false today: every published
	// selector, including relationship-eligible ones, may be selected
	// before any matching target exists.
	RequiresExistingTarget bool `json:"requiresExistingTarget,omitempty"`
	// Eligibility is present only when the request named a boundary
	// (?projectId= or ?boundary=).
	Eligibility *ScopeEligibility `json:"eligibility,omitempty"`
}

// AuthScopeAlias is a convenience alias that expands to multiple scopes.
type AuthScopeAlias struct {
	ID          string   `json:"id"`
	Description string   `json:"description"`
	ExpandsTo   []string `json:"expands_to"`

	// Eligibility is present only when the request named a boundary. An
	// alias is eligible only when every member selector is.
	Eligibility *ScopeEligibility `json:"eligibility,omitempty"`
}

// AuthScopesResponse is the response for GET /api/v1/auth/scopes.
type AuthScopesResponse struct {
	Scopes  []AuthScopeEntry `json:"scopes"`
	Aliases []AuthScopeAlias `json:"aliases"`
}

// scopeEligibilityNote returns optional human-readable framing for a
// relationship-kind entry, eligible or not: eligibility here answers only
// "may you select this restriction," and a relationship-eligible selector
// is re-checked against the actual target on every later request.
func scopeEligibilityNote(kind permissions.MintEligibilityKind) string {
	if kind == permissions.MintEligibilityRelationship {
		return "checked on each target: your own agents and their descendants"
	}
	return ""
}

// dedupStrings returns ss with duplicates removed, order preserved.
func dedupStrings(ss []string) []string {
	if len(ss) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(ss))
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// boundaryKindsToStrings converts registry boundary kinds to their wire
// strings for AuthScopeEntry.AllowedBoundaries.
func boundaryKindsToStrings(kinds []permissions.BoundaryKind) []string {
	if len(kinds) == 0 {
		return nil
	}
	out := make([]string, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, string(k))
	}
	return out
}

// parseAuthScopesBoundary interprets the ?boundary=/?projectId= query
// parameters for GET /api/v1/auth/scopes. A nil *TokenBoundary with a nil
// error means "no boundary requested" (today's unchanged catalog-only
// response). This validation is intentionally independent of the hub-
// boundary feature gate below it: when that gate is later removed, the
// projectId-conflict and missing-projectId 400s must still hold.
func parseAuthScopesBoundary(w http.ResponseWriter, r *http.Request) (boundary *TokenBoundary, handled bool) {
	query := r.URL.Query()
	boundaryParam := query.Get("boundary")
	projectID := query.Get("projectId")

	switch boundaryParam {
	case "":
		if projectID == "" {
			return nil, false
		}
		return &TokenBoundary{Kind: BoundaryKindProject, ProjectID: projectID}, false
	case "project":
		if projectID == "" {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "projectId is required for boundary=project", nil)
			return nil, true
		}
		return &TokenBoundary{Kind: BoundaryKindProject, ProjectID: projectID}, false
	case "hub":
		// A hub boundary never accepts a projectId, regardless of whether
		// hub eligibility itself is enabled yet.
		if projectID != "" {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "boundary=hub does not accept projectId", nil)
			return nil, true
		}
		// Hub-boundary eligibility is not enabled yet; enabling it is this
		// single switch. The two validation cases above must keep
		// returning 400 unchanged when it is.
		writeError(w, http.StatusBadRequest, "unsupported_boundary", "hub boundary eligibility is not available yet", nil)
		return nil, true
	default:
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "unknown boundary", nil)
		return nil, true
	}
}

// handleAuthScopes handles GET /api/v1/auth/scopes.
// Returns all valid UAT scopes from the permissions registry. With
// ?projectId= (or the equivalent ?boundary=project&projectId=), each entry
// additionally reports whether the authenticated user may currently select
// it as a project-boundary restriction (CanMintSelector), computed fresh for
// the principal and never widened by whatever credential made this request.
//
// Project-access aggregation: the whole response collapses to the same
// oracle-resistant 403 as token mint ONLY when the batch has at least one
// MintDenialProjectAccessRequired result AND no result is admitted
// (OK=true) -- that combination is indistinguishable from "project does
// not exist" (no membership and no exact-permission system authority for
// anything relevant), so existence stays unobservable in that all-denied
// case. A selector denied for an unrelated, structural reason
// (boundary_not_allowed, unknown_selector -- e.g. a hub-only selector
// requested under a project boundary, true for every principal) neither
// triggers nor blocks the collapse; it is orthogonal to project access and
// always shown per-entry either way. Otherwise the response answers
// per-entry, and an entry denied for project access reports
// eligible=false, reason="project_access_required" verbatim: a caller
// admitted for at least one selector already knows the project exists, so
// seeing which of their own selectors also lack authority is not a new
// oracle. This keeps the listing consistent with mint: a selector the list
// shows eligible must mint, and one it shows ineligible must not.
func (s *Server) handleAuthScopes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}

	boundary, handled := parseAuthScopesBoundary(w, r)
	if handled {
		return
	}

	options := permissions.UATScopeOptions(false)

	var eligByScope map[string]SelectorEligibility
	if boundary != nil {
		// Eligibility is a per-user computation (CanMintSelector requires a
		// local user principal); the parameterless catalog below has no
		// such requirement and stays available to any authenticated
		// identity, exactly as before this endpoint gained eligibility.
		user := GetUserIdentityFromContext(r.Context())
		if user == nil {
			Unauthorized(w)
			return
		}
		selectors := make([]string, 0, len(options))
		for _, opt := range options {
			selectors = append(selectors, opt.UATScope)
		}
		principal := principalContextForIdentity(user)
		results, err := s.authzService.CanMintSelector(r.Context(), principal, *boundary, selectors)
		if err != nil {
			slog.Warn("auth scopes: CanMintSelector failed",
				"user_id", user.ID(), "project_id", boundary.ProjectID, "error", err)
			// Fail closed, oracle-resistant: identical to mint's forbidden
			// response for an inaccessible or nonexistent project.
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "forbidden", nil)
			return
		}
		// Aggregate across the whole batch. A selector denied for a reason
		// OTHER than project access (boundary_not_allowed, unknown_selector)
		// is a static, principal-independent fact -- e.g. a hub-only
		// selector requested under a project boundary always fails that
		// way, for every principal -- and must not count toward, or
		// against, "every selector lacks project access": it is simply
		// irrelevant to that question. Only project_access_required
		// results and OK=true results inform the aggregate.
		var sawProjectAccessDenial, sawAdmitted bool
		for _, res := range results {
			if res.OK {
				sawAdmitted = true
			}
			if res.Reason == MintDenialProjectAccessRequired {
				sawProjectAccessDenial = true
			}
		}
		if sawProjectAccessDenial && !sawAdmitted {
			// Every selector that reached the project-admission gate was
			// denied for lack of project access, and nothing else was
			// admitted: identical to a nonexistent or wholly inaccessible
			// project. Collapse to mint's uniform 403 rather than confirm
			// "you have zero authority here" per entry.
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "forbidden", nil)
			return
		}
		eligByScope = make(map[string]SelectorEligibility, len(results))
		for _, res := range results {
			eligByScope[res.Selector] = res
		}
	}

	scopes := make([]AuthScopeEntry, 0, len(options))
	for _, opt := range options {
		entry := AuthScopeEntry{
			ID:           opt.UATScope,
			Resource:     opt.Resource,
			Action:       opt.Action,
			Description:  opt.Description,
			PermissionID: opt.ID,
		}
		if mapping, ok := permissions.ResolveSelector(opt.UATScope); ok {
			entry.AllowedBoundaries = boundaryKindsToStrings(mapping.AllowedBoundaries)
		}
		entry.EligibilityKind = string(permissions.MintEligibilityFlatRole)
		if descriptor, ok := permissions.MintEligibilityRegistry[opt.ID]; ok {
			entry.RequiresExistingTarget = descriptor.RequiresExistingTarget
			var relationships []string
			for _, src := range descriptor.Sources {
				if src.Kind == permissions.MintEligibilityRelationship {
					entry.EligibilityKind = string(permissions.MintEligibilityRelationship)
					relationships = append(relationships, src.RelationshipTypes...)
				}
			}
			entry.Relationships = dedupStrings(relationships)
		}
		if boundary != nil {
			if res, ok := eligByScope[opt.UATScope]; ok {
				elig := &ScopeEligibility{
					Boundary: tokenBoundaryToDTO(*boundary),
					Eligible: res.OK,
				}
				if !res.OK {
					elig.Reason = string(res.Reason)
				}
				elig.Note = scopeEligibilityNote(permissions.MintEligibilityKind(entry.EligibilityKind))
				entry.Eligibility = elig
			}
		}
		scopes = append(scopes, entry)
	}

	// Build aliases dynamically from the manage alias registry.
	aliasKeys := make([]string, 0, len(permissions.UATManageAliases))
	for alias := range permissions.UATManageAliases {
		aliasKeys = append(aliasKeys, alias)
	}
	sort.Strings(aliasKeys)
	aliases := make([]AuthScopeAlias, 0, len(aliasKeys))
	for _, alias := range aliasKeys {
		resource := permissions.UATManageAliases[alias]
		expandsTo := permissions.UATManageScopesFor(resource)
		aliasEntry := AuthScopeAlias{
			ID:          alias,
			Description: fmt.Sprintf("All %s management operations", resource),
			ExpandsTo:   expandsTo,
		}
		if boundary != nil {
			elig := &ScopeEligibility{
				Boundary: tokenBoundaryToDTO(*boundary),
				Eligible: true,
			}
			var ineligible []string
			for _, member := range expandsTo {
				res, ok := eligByScope[member]
				if !ok || !res.OK {
					ineligible = append(ineligible, member)
				}
			}
			if len(ineligible) > 0 {
				elig.Eligible = false
				elig.IneligibleMembers = ineligible
			}
			aliasEntry.Eligibility = elig
		}
		aliases = append(aliases, aliasEntry)
	}

	writeJSON(w, http.StatusOK, AuthScopesResponse{
		Scopes:  scopes,
		Aliases: aliases,
	})
}

// deleteSuperAdminBinding removes the system-scoped super-admin role binding for
// a user, if one exists. Called at login time when demotion actually happened
// (D11-fix2, Finding 3) so that IsSystemAdmin immediately agrees with
// IsUnscopedLocalPlatformAdmin. Best-effort: errors are logged but do not fail
// the login — the next startup reconciliation will clean up.
func (s *Server) deleteSuperAdminBinding(ctx context.Context, userID string) {
	deleteSuperAdminRoleBinding(ctx, s.store, userID)
}

// ensureSuperAdminBinding idempotently creates a system-scoped super-admin
// RoleBinding for the given user. This closes the cold-start gap where
// provisionUser assigns Role="admin" but ReconcileSuperAdminBindings has
// already run against an empty user store and will not run again until the
// next restart. Without this, AuthzService.IsSystemAdmin returns false for
// the first admin until the hub is restarted.
func (s *Server) ensureSuperAdminBinding(ctx context.Context, userID string) {
	ensureSuperAdminRoleBinding(ctx, s.store, userID)
}

// deleteSuperAdminRoleBinding removes the system-scoped super-admin role binding
// for a user, if one exists. This is a package-level function so it can be
// called from both Server (API auth) and WebServer (browser auth) login paths.
// Best-effort: errors are logged but do not fail the login — the next startup
// reconciliation will clean up.
func deleteSuperAdminRoleBinding(ctx context.Context, st store.Store, userID string) {
	rd, err := st.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	if err != nil {
		slog.Warn("deleteSuperAdminBinding: super-admin role definition not found", "user_id", userID, "error", err)
		return
	}

	bindings, err := st.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
	if err != nil {
		slog.Warn("deleteSuperAdminBinding: failed to list bindings", "user_id", userID, "error", err)
		return
	}

	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeSystem && b.RoleDefinitionID == rd.ID {
			if err := st.DeleteRoleBinding(ctx, b.ID); err != nil {
				slog.Warn("deleteSuperAdminBinding: failed to delete binding",
					"user_id", userID, "binding_id", b.ID, "error", err)
			} else {
				slog.Info("deleted super-admin binding at login-time demotion",
					"user_id", userID, "binding_id", b.ID)
			}
		}
	}
}

// ensureSuperAdminRoleBinding idempotently creates a system-scoped super-admin
// RoleBinding for the given user. This is a package-level function so it can be
// called from both Server (API auth) and WebServer (browser auth) login paths.
// It closes the cold-start gap where user provisioning assigns Role="admin" but
// ReconcileSuperAdminBindings has already run against an empty user store and
// will not run again until the next restart.
func ensureSuperAdminRoleBinding(ctx context.Context, st store.Store, userID string) {
	rd, err := st.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	if err != nil {
		slog.Warn("ensureSuperAdminBinding: super-admin role definition not found — "+
			"binding will be created on next restart by ReconcileSuperAdminBindings",
			"user_id", userID, "error", err)
		return
	}
	if rd == nil {
		slog.Error("ensureSuperAdminBinding: GetRoleDefinitionByName returned nil without error — "+
			"binding will be created on next restart by ReconcileSuperAdminBindings",
			"user_id", userID)
		return
	}

	_, err = st.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
		ScopeID:          "",
		CreatedBy:        store.SystemReconcileCreatedBy,
	})
	if err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		slog.Error("ensureSuperAdminBinding: failed to create super-admin binding",
			"user_id", userID, "error", err)
	} else if err == nil {
		slog.Info("created super-admin binding at login-time provisioning",
			"user_id", userID)
	}
}
