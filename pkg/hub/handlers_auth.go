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
	"strings"
	"time"

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
type TokenCreateRequest struct {
	Name      string     `json:"name"`
	GroveID   string     `json:"groveId"`
	Scopes    []string   `json:"scopes"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

// TokenCreateResponse is the response for creating a user access token.
type TokenCreateResponse struct {
	Token       string         `json:"token"` // Full token, only shown once
	AccessToken *TokenResponse `json:"accessToken"`
}

// TokenResponse is the access token info (without the actual token value).
type TokenResponse struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Prefix    string     `json:"prefix"`
	GroveID   string     `json:"groveId"`
	Scopes    []string   `json:"scopes"`
	Revoked   bool       `json:"revoked"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
	LastUsed  *time.Time `json:"lastUsed,omitempty"`
	Created   time.Time  `json:"created"`
}

// handleAuth routes auth-related requests.
func (s *Server) handleAuth(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case path == "/api/v1/auth/login" && r.Method == http.MethodPost:
		s.handleAuthLogin(w, r)
	case path == "/api/v1/auth/token" && r.Method == http.MethodPost:
		s.handleAuthToken(w, r)
	case path == "/api/v1/auth/refresh" && r.Method == http.MethodPost:
		s.handleAuthRefresh(w, r)
	case path == "/api/v1/auth/validate" && r.Method == http.MethodPost:
		s.handleAuthValidate(w, r)
	case path == "/api/v1/auth/logout" && r.Method == http.MethodPost:
		s.handleAuthLogout(w, r)
	case path == "/api/v1/auth/me" && r.Method == http.MethodGet:
		s.handleAuthMe(w, r)
	default:
		MethodNotAllowed(w)
	}
}

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
	if provider != "google" && provider != "github" {
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

	// Check if user's email domain is authorized.
	if !isEmailAuthorized(userInfo.Email, s.config.AuthorizedDomains, s.config.AdminEmails) {
		writeError(w, http.StatusForbidden, "unauthorized_domain",
			"your email domain is not authorized", nil)
		return
	}

	// Find or create user
	ctx := r.Context()
	user, err := s.store.GetUserByEmail(ctx, userInfo.Email)
	if err != nil {
		// Create new user
		user = &store.User{
			ID:          generateID(),
			Email:       userInfo.Email,
			DisplayName: userInfo.DisplayName,
			AvatarURL:   userInfo.AvatarURL,
			Role:        s.getUserRole(userInfo.Email),
			Status:      "active",
			Created:     time.Now(),
			LastLogin:   time.Now(),
		}
		if err := s.store.CreateUser(ctx, user); err != nil {
			InternalError(w)
			return
		}
	} else {
		// Update last login
		user.LastLogin = time.Now()
		if userInfo.AvatarURL != "" && user.AvatarURL == "" {
			user.AvatarURL = userInfo.AvatarURL
		}
		if userInfo.DisplayName != "" && user.DisplayName == "" {
			user.DisplayName = userInfo.DisplayName
		}
		// Check if user should be promoted to admin (in case admin list changed)
		if user.Role != "admin" && s.getUserRole(userInfo.Email) == "admin" {
			user.Role = "admin"
		}
		_ = s.store.UpdateUser(ctx, user)
	}

	// Ensure user is a member of the hub-members group
	ensureHubMembership(ctx, s.store, user.ID)

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

	// Default provider to google for now if not specified in request
	provider := req.Provider
	if provider == "" {
		provider = "google"
		if strings.Contains(req.RedirectURI, "github") {
			provider = "github"
		}
		slog.Debug("OAuth provider inferred from redirect URI", "provider", provider)
	}

	// Validate provider is a known value
	if provider != "google" && provider != "github" {
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

	// Find or create user
	user, err := s.store.GetUserByEmail(ctx, userInfo.Email)
	if err != nil {
		// Create new user
		user = &store.User{
			ID:          generateID(),
			Email:       userInfo.Email,
			DisplayName: userInfo.DisplayName,
			AvatarURL:   userInfo.AvatarURL,
			Role:        s.getUserRole(userInfo.Email),
			Status:      "active",
			Created:     time.Now(),
			LastLogin:   time.Now(),
		}
		if err := s.store.CreateUser(ctx, user); err != nil {
			InternalError(w)
			return
		}
	} else {
		// Update last login
		user.LastLogin = time.Now()
		if userInfo.AvatarURL != "" && user.AvatarURL == "" {
			user.AvatarURL = userInfo.AvatarURL
		}
		if userInfo.DisplayName != "" && user.DisplayName == "" {
			user.DisplayName = userInfo.DisplayName
		}
		// Check if user should be promoted to admin (in case admin list changed)
		if user.Role != "admin" && s.getUserRole(userInfo.Email) == "admin" {
			user.Role = "admin"
		}
		_ = s.store.UpdateUser(ctx, user)
	}

	// Ensure user is a member of the hub-members group
	ensureHubMembership(ctx, s.store, user.ID)

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

	accessToken, refreshToken, expiresIn, err := s.userTokenService.RefreshTokens(req.RefreshToken)
	if err != nil {
		writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
			"invalid refresh token", nil)
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
func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	var req AuthLogoutRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		// Empty body is fine for logout
	}

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
	})
}

// handleTokens routes user access token requests.
func (s *Server) handleTokens(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleListTokens(w, r)
	case http.MethodPost:
		s.handleCreateToken(w, r)
	default:
		MethodNotAllowed(w)
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
			MethodNotAllowed(w)
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
		MethodNotAllowed(w)
	}
}

// handleListTokens handles GET /api/v1/auth/tokens.
func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) {
	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		Unauthorized(w)
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

	// Prevent UAT-creates-UAT: reject if authenticated with a UAT
	if _, ok := user.(*ScopedUserIdentity); ok {
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"access tokens cannot create other access tokens", nil)
		return
	}

	var req TokenCreateRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "invalid request body")
		return
	}

	key, token, err := s.uatService.CreateToken(r.Context(), user.ID(), req.Name, req.GroveID, req.Scopes, req.ExpiresAt)
	if err != nil {
		switch {
		case errors.Is(err, ErrUATLimitExceeded):
			writeError(w, http.StatusConflict, "limit_exceeded", err.Error(), nil)
		case errors.Is(err, ErrInvalidUATScope):
			ValidationError(w, err.Error(), nil)
		case errors.Is(err, ErrUATExpiryTooLong):
			ValidationError(w, err.Error(), nil)
		case strings.Contains(err.Error(), "required"):
			ValidationError(w, err.Error(), nil)
		case strings.Contains(err.Error(), "grove not found"):
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

	if err := s.uatService.RevokeToken(r.Context(), user.ID(), id); err != nil {
		NotFound(w, "access token")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleDeleteToken handles DELETE /api/v1/auth/tokens/{id}.
func (s *Server) handleDeleteToken(w http.ResponseWriter, r *http.Request, id string) {
	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		Unauthorized(w)
		return
	}

	if err := s.uatService.DeleteToken(r.Context(), user.ID(), id); err != nil {
		NotFound(w, "access token")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func tokenToResponse(t store.UserAccessToken) TokenResponse {
	return TokenResponse{
		ID:        t.ID,
		Name:      t.Name,
		Prefix:    t.Prefix,
		GroveID:   t.GroveID,
		Scopes:    t.Scopes,
		Revoked:   t.Revoked,
		ExpiresAt: t.ExpiresAt,
		LastUsed:  t.LastUsed,
		Created:   t.Created,
	}
}

func tokenResponsePtr(t *store.UserAccessToken) *TokenResponse {
	resp := tokenToResponse(*t)
	return &resp
}

// handleCLIAuthAuthorize handles POST /api/v1/auth/cli/authorize.
// This endpoint generates an OAuth authorization URL for CLI login.
func (s *Server) handleCLIAuthAuthorize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
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

	// Default to Google if no provider specified
	provider := req.Provider
	if provider == "" {
		provider = "google"
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
		MethodNotAllowed(w)
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
	if s.oauthService != nil {
		resp.Providers = s.oauthService.ConfiguredProvidersForClient(clientType)
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleCLIAuthToken handles POST /api/v1/auth/cli/token.
// This endpoint exchanges an OAuth authorization code for Hub tokens.
func (s *Server) handleCLIAuthToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
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

	// Default to Google if no provider specified
	provider := req.Provider
	if provider == "" {
		provider = "google"
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

	// Check if user's email domain is authorized
	if !isEmailAuthorized(userInfo.Email, s.config.AuthorizedDomains, s.config.AdminEmails) {
		writeError(w, http.StatusForbidden, "unauthorized_domain",
			"your email domain is not authorized", nil)
		return
	}

	// Find or create user
	user, err := s.store.GetUserByEmail(ctx, userInfo.Email)
	if err != nil {
		// Create new user
		user = &store.User{
			ID:          generateID(),
			Email:       userInfo.Email,
			DisplayName: userInfo.DisplayName,
			AvatarURL:   userInfo.AvatarURL,
			Role:        s.getUserRole(userInfo.Email),
			Status:      "active",
			Created:     time.Now(),
			LastLogin:   time.Now(),
		}
		if err := s.store.CreateUser(ctx, user); err != nil {
			InternalError(w)
			return
		}
	} else {
		// Update last login and profile info
		user.LastLogin = time.Now()
		if userInfo.AvatarURL != "" && user.AvatarURL == "" {
			user.AvatarURL = userInfo.AvatarURL
		}
		if userInfo.DisplayName != "" && user.DisplayName == "" {
			user.DisplayName = userInfo.DisplayName
		}
		// Check if user should be promoted to admin (in case admin list changed)
		if user.Role != "admin" && s.getUserRole(userInfo.Email) == "admin" {
			user.Role = "admin"
		}
		_ = s.store.UpdateUser(ctx, user)
	}

	// Ensure user is a member of the hub-members group
	ensureHubMembership(ctx, s.store, user.ID)

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
		MethodNotAllowed(w)
		return
	}

	var req CLIDeviceAuthorizeRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "invalid request body")
		return
	}

	provider := req.Provider
	if provider == "" {
		provider = "google"
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
		MethodNotAllowed(w)
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
		provider = "google"
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

	// Check if user's email domain is authorized
	if !isEmailAuthorized(userInfo.Email, s.config.AuthorizedDomains, s.config.AdminEmails) {
		writeError(w, http.StatusForbidden, "unauthorized_domain",
			"your email domain is not authorized", nil)
		return
	}

	// Find or create user
	user, err := s.store.GetUserByEmail(ctx, userInfo.Email)
	if err != nil {
		// Create new user
		user = &store.User{
			ID:          generateID(),
			Email:       userInfo.Email,
			DisplayName: userInfo.DisplayName,
			AvatarURL:   userInfo.AvatarURL,
			Role:        s.getUserRole(userInfo.Email),
			Status:      "active",
			Created:     time.Now(),
			LastLogin:   time.Now(),
		}
		if err := s.store.CreateUser(ctx, user); err != nil {
			InternalError(w)
			return
		}
	} else {
		// Update last login and profile info
		user.LastLogin = time.Now()
		if userInfo.AvatarURL != "" && user.AvatarURL == "" {
			user.AvatarURL = userInfo.AvatarURL
		}
		if userInfo.DisplayName != "" && user.DisplayName == "" {
			user.DisplayName = userInfo.DisplayName
		}
		if user.Role != "admin" && s.getUserRole(userInfo.Email) == "admin" {
			user.Role = "admin"
		}
		_ = s.store.UpdateUser(ctx, user)
	}

	// Ensure user is a member of the hub-members group
	ensureHubMembership(ctx, s.store, user.ID)

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

// generateID generates a new UUID.
func generateID() string {
	return uuid.New().String()
}

// isEmailAuthorized checks if an email address is from an authorized domain.
// If authorizedDomains is empty, all emails are allowed.
// Bootstrap admin emails (from AdminEmails config) bypass the domain check.
func isEmailAuthorized(email string, authorizedDomains []string, adminEmails []string) bool {
	// If no domains are configured, allow all
	if len(authorizedDomains) == 0 {
		return true
	}

	// Bootstrap admin emails bypass domain restrictions
	emailLower := strings.ToLower(email)
	for _, admin := range adminEmails {
		if strings.ToLower(admin) == emailLower {
			return true
		}
	}

	// Extract domain from email
	atIndex := strings.LastIndex(email, "@")
	if atIndex == -1 {
		return false
	}

	domain := strings.ToLower(email[atIndex+1:])

	// Check if domain is in the authorized list
	for _, authorized := range authorizedDomains {
		authorizedLower := strings.ToLower(authorized)
		if authorizedLower == domain {
			return true
		}
		// Support wildcard subdomains: "*.example.com" matches "foo.example.com",
		// "bar.baz.example.com", etc.
		if strings.HasPrefix(authorizedLower, "*.") {
			suffix := authorizedLower[1:] // e.g. ".example.com"
			if strings.HasSuffix(domain, suffix) {
				return true
			}
		}
	}

	return false
}

// determineUserRole returns the role for a user based on their email.
// Returns "admin" if the email is in the adminEmails list, otherwise "member".
func determineUserRole(email string, adminEmails []string) string {
	emailLower := strings.ToLower(email)
	for _, adminEmail := range adminEmails {
		if strings.ToLower(adminEmail) == emailLower {
			return "admin"
		}
	}
	return "member"
}

// (s *Server) getUserRole is a convenience method to determine role using server config.
func (s *Server) getUserRole(email string) string {
	return determineUserRole(email, s.config.AdminEmails)
}
