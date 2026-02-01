package hub

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/ptone/scion-agent/pkg/store"
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
	Provider     string `json:"provider"`     // "google", "github", etc.
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

// APIKeyCreateRequest is the request body for creating an API key.
type APIKeyCreateRequest struct {
	Name      string     `json:"name"`
	Scopes    []string   `json:"scopes,omitempty"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

// APIKeyCreateResponse is the response for creating an API key.
type APIKeyCreateResponse struct {
	Key    string          `json:"key"` // Full key, only shown once
	APIKey *APIKeyResponse `json:"apiKey"`
}

// APIKeyResponse is the API key info (without the actual key).
type APIKeyResponse struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Prefix    string     `json:"prefix"` // First 8 chars for identification
	Scopes    []string   `json:"scopes,omitempty"`
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
	if req.Provider == "" || req.Email == "" {
		ValidationError(w, "missing required fields", map[string]interface{}{
			"required": []string{"provider", "email"},
		})
		return
	}

	// TODO: In production, validate the provider token with the OAuth provider
	// For now, we trust the provided information (suitable for dev mode)

	// Find or create user
	ctx := r.Context()
	user, err := s.store.GetUserByEmail(ctx, req.Email)
	if err != nil {
		// Create new user
		user = &store.User{
			ID:          generateID(),
			Email:       req.Email,
			DisplayName: req.Name,
			AvatarURL:   req.Avatar,
			Role:        "member",
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
		if req.Avatar != "" && user.AvatarURL == "" {
			user.AvatarURL = req.Avatar
		}
		if req.Name != "" && user.DisplayName == "" {
			user.DisplayName = req.Name
		}
		_ = s.store.UpdateUser(ctx, user)
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

	// Default provider to google for now if not specified in request
	provider := req.Provider
	if provider == "" {
		provider = "google"
		if strings.Contains(req.RedirectURI, "github") {
			provider = "github"
		}
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
		writeError(w, http.StatusBadRequest, "oauth_error",
			"failed to exchange authorization code: "+err.Error(), nil)
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
			Role:        "member",
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
		_ = s.store.UpdateUser(ctx, user)
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

// handleAPIKeys routes API key requests.
func (s *Server) handleAPIKeys(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleListAPIKeys(w, r)
	case http.MethodPost:
		s.handleCreateAPIKey(w, r)
	default:
		MethodNotAllowed(w)
	}
}

// handleAPIKeyByID routes API key requests by ID.
func (s *Server) handleAPIKeyByID(w http.ResponseWriter, r *http.Request) {
	id := extractID(r, "/api/v1/auth/api-keys")
	if id == "" {
		s.handleAPIKeys(w, r)
		return
	}

	switch r.Method {
	case http.MethodDelete:
		s.handleDeleteAPIKey(w, r, id)
	default:
		MethodNotAllowed(w)
	}
}

// handleListAPIKeys handles GET /api/v1/auth/api-keys.
func (s *Server) handleListAPIKeys(w http.ResponseWriter, r *http.Request) {
	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		Unauthorized(w)
		return
	}

	if s.apiKeyService == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"items": []interface{}{}})
		return
	}

	keys, err := s.apiKeyService.ListAPIKeys(r.Context(), user.ID())
	if err != nil {
		InternalError(w)
		return
	}

	// Convert to response format
	items := make([]APIKeyResponse, 0, len(keys))
	for _, k := range keys {
		items = append(items, APIKeyResponse{
			ID:        k.ID,
			Name:      k.Name,
			Prefix:    k.Prefix,
			Scopes:    k.Scopes,
			ExpiresAt: k.ExpiresAt,
			LastUsed:  k.LastUsed,
			Created:   k.Created,
		})
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"items": items})
}

// handleCreateAPIKey handles POST /api/v1/auth/api-keys.
func (s *Server) handleCreateAPIKey(w http.ResponseWriter, r *http.Request) {
	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		Unauthorized(w)
		return
	}

	if s.apiKeyService == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented",
			"API key management not enabled", nil)
		return
	}

	var req APIKeyCreateRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "invalid request body")
		return
	}

	if req.Name == "" {
		ValidationError(w, "name is required", nil)
		return
	}

	key, apiKey, err := s.apiKeyService.CreateAPIKey(r.Context(), user.ID(), req.Name, req.Scopes, req.ExpiresAt)
	if err != nil {
		InternalError(w)
		return
	}

	writeJSON(w, http.StatusCreated, APIKeyCreateResponse{
		Key: key,
		APIKey: &APIKeyResponse{
			ID:        apiKey.ID,
			Name:      apiKey.Name,
			Prefix:    apiKey.Prefix,
			Scopes:    apiKey.Scopes,
			ExpiresAt: apiKey.ExpiresAt,
			LastUsed:  apiKey.LastUsed,
			Created:   apiKey.Created,
		},
	})
}

// handleDeleteAPIKey handles DELETE /api/v1/auth/api-keys/{id}.
func (s *Server) handleDeleteAPIKey(w http.ResponseWriter, r *http.Request, id string) {
	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		Unauthorized(w)
		return
	}

	if s.apiKeyService == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented",
			"API key management not enabled", nil)
		return
	}

	if err := s.apiKeyService.DeleteAPIKey(r.Context(), user.ID(), id); err != nil {
		NotFound(w, "API key")
		return
	}

	w.WriteHeader(http.StatusNoContent)
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
			log.Printf("[Hub] CLI auth authorize request for provider %q failed: OAuth service is nil", provider)
			log.Printf("[Hub] Check environment variables SCION_SERVER_OAUTH_CLI_*_CLIENTID/CLIENTSECRET")
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
			log.Printf("[Hub] CLI auth token exchange for provider %q failed: OAuth service is nil", provider)
		}
		writeError(w, http.StatusNotImplemented, "not_implemented",
			"OAuth is not configured on this server", nil)
		return
	}

	// Exchange code for user info using CLI OAuth client
	ctx := r.Context()
	userInfo, err := s.oauthService.ExchangeCodeForClient(ctx, OAuthClientTypeCLI, provider, req.Code, req.CallbackURL)
	if err != nil {
		writeError(w, http.StatusBadRequest, "oauth_error",
			"failed to exchange authorization code: "+err.Error(), nil)
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
			Role:        "member",
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
		_ = s.store.UpdateUser(ctx, user)
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

// generateID generates a new UUID.
func generateID() string {
	return uuid.New().String()
}
