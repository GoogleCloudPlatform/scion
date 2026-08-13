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
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// OAuthProviderConfig holds OAuth credentials for a single provider.
type OAuthProviderConfig struct {
	ClientID     string
	ClientSecret string
}

// OAuthClientConfig holds OAuth provider configurations for a specific client type.
type OAuthClientConfig struct {
	Google OAuthProviderConfig
	GitHub OAuthProviderConfig
}

// IsConfigured returns true if at least one OAuth provider is configured.
func (c *OAuthClientConfig) IsConfigured() bool {
	return c.Google.ClientID != "" || c.GitHub.ClientID != ""
}

// IsProviderConfigured returns true if the specified provider is configured.
func (c *OAuthClientConfig) IsProviderConfigured(provider string) bool {
	switch provider {
	case hubclient.OAuthProviderGoogle:
		return c.Google.ClientID != "" && c.Google.ClientSecret != ""
	case hubclient.OAuthProviderGitHub:
		return c.GitHub.ClientID != "" && c.GitHub.ClientSecret != ""
	default:
		return false
	}
}

// GetProvider returns the provider config for the specified provider.
func (c *OAuthClientConfig) GetProvider(provider string) OAuthProviderConfig {
	switch provider {
	case hubclient.OAuthProviderGoogle:
		return c.Google
	case hubclient.OAuthProviderGitHub:
		return c.GitHub
	default:
		return OAuthProviderConfig{}
	}
}

// OAuthConfig holds configuration for all OAuth providers.
// Web, CLI, and Device use separate OAuth clients due to different redirect URI requirements.
type OAuthConfig struct {
	// Web OAuth client settings (for web frontend flows).
	Web OAuthClientConfig
	// CLI OAuth client settings (for CLI localhost callback flows).
	CLI OAuthClientConfig
	// Device OAuth client settings (for device authorization grant / headless flows).
	Device OAuthClientConfig
}

// IsConfigured returns true if at least one OAuth provider is configured.
func (c *OAuthConfig) IsConfigured() bool {
	return c.Web.IsConfigured() || c.CLI.IsConfigured() || c.Device.IsConfigured()
}

// IsProviderConfigured returns true if the specified provider is configured
// for at least one client type.
func (c *OAuthConfig) IsProviderConfigured(provider string) bool {
	return c.Web.IsProviderConfigured(provider) || c.CLI.IsProviderConfigured(provider) || c.Device.IsProviderConfigured(provider)
}

// OAuthClientType represents the type of client (web or CLI).
type OAuthClientType = hubclient.OAuthClientType

const (
	// OAuthClientTypeWeb is for web browser-based OAuth flows.
	OAuthClientTypeWeb = hubclient.OAuthClientTypeWeb
	// OAuthClientTypeCLI is for CLI localhost callback OAuth flows.
	OAuthClientTypeCLI = hubclient.OAuthClientTypeCLI
	// OAuthClientTypeDevice is for device authorization grant (headless) flows.
	OAuthClientTypeDevice = hubclient.OAuthClientTypeDevice
)

func oauthProviderOrder() []string {
	return hubclient.OAuthProviderOrder()
}

// oidcDiscoveryCache caches the OIDC discovery document with a TTL.
// If the discovery fetch fails while a stale entry exists, the stale entry
// is returned — this provides resilience against transient IdP outages.
type oidcDiscoveryCache struct {
	mu        sync.RWMutex
	doc       *OIDCDiscoveryDoc
	fetchedAt time.Time
	ttl       time.Duration // default: 1 hour
}

// get returns the cached discovery document, fetching it if stale or absent.
func (c *oidcDiscoveryCache) get(issuerURL string, client *http.Client) (*OIDCDiscoveryDoc, error) {
	c.mu.RLock()
	if c.doc != nil && time.Since(c.fetchedAt) < c.ttl {
		doc := c.doc
		c.mu.RUnlock()
		return doc, nil
	}
	staleDoc := c.doc
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()

	// Double-check after acquiring write lock (another goroutine may have refreshed).
	if c.doc != nil && time.Since(c.fetchedAt) < c.ttl {
		return c.doc, nil
	}

	doc, err := discoverOIDCEndpoints(issuerURL, client)
	if err != nil {
		if staleDoc != nil {
			slog.Warn("OIDC discovery refresh failed, using stale cache", "error", err)
			return staleDoc, nil
		}
		return nil, err
	}

	c.doc = doc
	c.fetchedAt = time.Now()
	return doc, nil
}

// OAuthService handles OAuth operations for authentication.
type OAuthService struct {
	config          OAuthConfig
	oidcLoginConfig *config.OIDCLoginConfig // nil when OIDC login is disabled
	httpClient      *http.Client
	oidcCache       *oidcDiscoveryCache // lazy-initialized when oidcLoginConfig != nil
}

// NewOAuthService creates a new OAuth service.
func NewOAuthService(cfg OAuthConfig, oidcLoginCfg *config.OIDCLoginConfig) *OAuthService {
	svc := &OAuthService{
		config:          cfg,
		oidcLoginConfig: oidcLoginCfg,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
	if oidcLoginCfg != nil && oidcLoginCfg.Enabled {
		svc.oidcCache = &oidcDiscoveryCache{ttl: 1 * time.Hour}
	}
	return svc
}

// getClientConfig returns the appropriate OAuth client config for the given client type.
func (s *OAuthService) getClientConfig(clientType OAuthClientType) OAuthClientConfig {
	switch clientType {
	case OAuthClientTypeWeb:
		return s.config.Web
	case OAuthClientTypeCLI:
		return s.config.CLI
	case OAuthClientTypeDevice:
		return s.config.Device
	default:
		return OAuthClientConfig{}
	}
}

// IsProviderConfiguredForClient returns true if the specified provider is configured
// for the given client type.
func (s *OAuthService) IsProviderConfiguredForClient(clientType OAuthClientType, provider string) bool {
	if provider == hubclient.OAuthProviderOIDC {
		// V1: OIDC login is web-only
		return clientType == OAuthClientTypeWeb &&
			s.oidcLoginConfig != nil &&
			s.oidcLoginConfig.Enabled &&
			s.oidcLoginConfig.ClientID != "" &&
			s.oidcLoginConfig.IssuerURL != ""
	}
	cfg := s.getClientConfig(clientType)
	return cfg.IsProviderConfigured(provider)
}

// ConfiguredProvidersForClient returns the configured OAuth providers for the
// given client type in stable display order.
func (s *OAuthService) ConfiguredProvidersForClient(clientType OAuthClientType) []string {
	order := oauthProviderOrder()
	providers := make([]string, 0, len(order))
	for _, provider := range order {
		if s.IsProviderConfiguredForClient(clientType, provider) {
			providers = append(providers, provider)
		}
	}

	return providers
}

// DefaultProviderForClient returns the first configured OAuth provider for a
// client type, following the standard provider order.
func (s *OAuthService) DefaultProviderForClient(clientType OAuthClientType) string {
	if providers := s.ConfiguredProvidersForClient(clientType); len(providers) > 0 {
		return providers[0]
	}
	return "google"
}

// OAuthUserInfo contains user information retrieved from an OAuth provider.
type OAuthUserInfo struct {
	ID          string
	Email       string
	DisplayName string
	AvatarURL   string
	Provider    string
}

// Google OAuth endpoints
const (
	googleAuthURL  = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenURL = "https://oauth2.googleapis.com/token"
	googleUserURL  = "https://www.googleapis.com/oauth2/v2/userinfo"
)

// GitHub OAuth endpoints
const (
	githubAuthURL  = "https://github.com/login/oauth/authorize"
	githubTokenURL = "https://github.com/login/oauth/access_token"
	githubUserURL  = "https://api.github.com/user"
	githubEmailURL = "https://api.github.com/user/emails"
)

// Device authorization endpoints
const (
	googleDeviceCodeURL = "https://oauth2.googleapis.com/device/code"
	githubDeviceCodeURL = "https://github.com/login/device/code"
)

// GetAuthorizationURL generates an OAuth authorization URL for the specified provider.
// Uses the default (CLI) client configuration for backward compatibility.
func (s *OAuthService) GetAuthorizationURL(provider, callbackURL, state string) (string, error) {
	return s.GetAuthorizationURLForClient(OAuthClientTypeCLI, provider, callbackURL, state)
}

// GetAuthorizationURLForClient generates an OAuth authorization URL for the specified
// provider and client type.
func (s *OAuthService) GetAuthorizationURLForClient(clientType OAuthClientType, provider, callbackURL, state string) (string, error) {
	cfg := s.getClientConfig(clientType)

	switch provider {
	case hubclient.OAuthProviderGoogle:
		return s.getGoogleAuthURLWithConfig(cfg.Google, callbackURL, state)
	case hubclient.OAuthProviderGitHub:
		return s.getGitHubAuthURLWithConfig(cfg.GitHub, callbackURL, state)
	case hubclient.OAuthProviderOIDC:
		return s.getOIDCAuthURL(callbackURL, state)
	default:
		return "", fmt.Errorf("unsupported OAuth provider: %s", provider)
	}
}

// getGoogleAuthURL generates a Google OAuth authorization URL using the default config.
//
//nolint:unused // Kept for direct OAuth flow compatibility.
func (s *OAuthService) getGoogleAuthURL(callbackURL, state string) (string, error) {
	return s.getGoogleAuthURLWithConfig(s.config.CLI.Google, callbackURL, state)
}

// getGoogleAuthURLWithConfig generates a Google OAuth authorization URL with the given config.
func (s *OAuthService) getGoogleAuthURLWithConfig(cfg OAuthProviderConfig, callbackURL, state string) (string, error) {
	if cfg.ClientID == "" {
		return "", fmt.Errorf("google OAuth is not configured")
	}

	params := url.Values{
		"client_id":     {cfg.ClientID},
		"redirect_uri":  {callbackURL},
		"response_type": {"code"},
		"scope":         {"openid email profile"},
		"state":         {state},
		"access_type":   {"offline"},
		"prompt":        {"consent"},
	}

	return googleAuthURL + "?" + params.Encode(), nil
}

// getGitHubAuthURL generates a GitHub OAuth authorization URL using the default config.
//
//nolint:unused // Kept for direct OAuth flow compatibility.
func (s *OAuthService) getGitHubAuthURL(callbackURL, state string) (string, error) {
	return s.getGitHubAuthURLWithConfig(s.config.CLI.GitHub, callbackURL, state)
}

// getGitHubAuthURLWithConfig generates a GitHub OAuth authorization URL with the given config.
func (s *OAuthService) getGitHubAuthURLWithConfig(cfg OAuthProviderConfig, callbackURL, state string) (string, error) {
	if cfg.ClientID == "" {
		return "", fmt.Errorf("GitHub OAuth is not configured")
	}

	params := url.Values{
		"client_id":    {cfg.ClientID},
		"redirect_uri": {callbackURL},
		"scope":        {"read:user user:email"},
		"state":        {state},
	}

	return githubAuthURL + "?" + params.Encode(), nil
}

// ExchangeCode exchanges an authorization code for user information.
// Uses the default (CLI) client configuration for backward compatibility.
func (s *OAuthService) ExchangeCode(ctx context.Context, provider, code, callbackURL string) (*OAuthUserInfo, error) {
	return s.ExchangeCodeForClient(ctx, OAuthClientTypeCLI, provider, code, callbackURL)
}

// ExchangeCodeForClient exchanges an authorization code for user information
// using the specified client type's configuration.
func (s *OAuthService) ExchangeCodeForClient(ctx context.Context, clientType OAuthClientType, provider, code, callbackURL string) (*OAuthUserInfo, error) {
	cfg := s.getClientConfig(clientType)

	switch provider {
	case "google":
		return s.exchangeGoogleCodeWithConfig(ctx, cfg.Google, code, callbackURL)
	case "github":
		return s.exchangeGitHubCodeWithConfig(ctx, cfg.GitHub, code, callbackURL)
	case hubclient.OAuthProviderOIDC:
		return s.exchangeOIDCCode(ctx, code, callbackURL)
	default:
		return nil, fmt.Errorf("unsupported OAuth provider: %s", provider)
	}
}

// exchangeGoogleCode exchanges a Google authorization code for user info.
//
//nolint:unused // Kept for direct OAuth flow compatibility.
func (s *OAuthService) exchangeGoogleCode(ctx context.Context, code, callbackURL string) (*OAuthUserInfo, error) {
	return s.exchangeGoogleCodeWithConfig(ctx, s.config.CLI.Google, code, callbackURL)
}

// exchangeGoogleCodeWithConfig exchanges a Google authorization code for user info using the given config.
func (s *OAuthService) exchangeGoogleCodeWithConfig(ctx context.Context, cfg OAuthProviderConfig, code, callbackURL string) (*OAuthUserInfo, error) {
	if cfg.ClientID == "" || cfg.ClientSecret == "" {
		return nil, fmt.Errorf("google OAuth is not configured")
	}

	// Exchange code for access token
	tokenResp, err := s.exchangeCodeForToken(ctx, googleTokenURL, cfg.ClientID, cfg.ClientSecret, code, callbackURL)
	if err != nil {
		return nil, fmt.Errorf("failed to exchange Google code: %w", err)
	}

	// Get user info
	userInfo, err := s.getGoogleUserInfo(ctx, tokenResp.AccessToken)
	if err != nil {
		return nil, fmt.Errorf("failed to get Google user info: %w", err)
	}

	return userInfo, nil
}

// exchangeGitHubCode exchanges a GitHub authorization code for user info.
//
//nolint:unused // Kept for direct OAuth flow compatibility.
func (s *OAuthService) exchangeGitHubCode(ctx context.Context, code, callbackURL string) (*OAuthUserInfo, error) {
	return s.exchangeGitHubCodeWithConfig(ctx, s.config.CLI.GitHub, code, callbackURL)
}

// exchangeGitHubCodeWithConfig exchanges a GitHub authorization code for user info using the given config.
func (s *OAuthService) exchangeGitHubCodeWithConfig(ctx context.Context, cfg OAuthProviderConfig, code, callbackURL string) (*OAuthUserInfo, error) {
	if cfg.ClientID == "" || cfg.ClientSecret == "" {
		return nil, fmt.Errorf("GitHub OAuth is not configured")
	}

	// Exchange code for access token
	tokenResp, err := s.exchangeGitHubCodeForTokenWithConfig(ctx, cfg, code, callbackURL)
	if err != nil {
		return nil, fmt.Errorf("failed to exchange GitHub code: %w", err)
	}

	// Get user info
	userInfo, err := s.getGitHubUserInfo(ctx, tokenResp.AccessToken)
	if err != nil {
		return nil, fmt.Errorf("failed to get GitHub user info: %w", err)
	}

	return userInfo, nil
}

// tokenResponse represents the response from an OAuth token endpoint.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

// exchangeCodeForToken exchanges an authorization code for an access token (Google).
func (s *OAuthService) exchangeCodeForToken(ctx context.Context, tokenURL, clientID, clientSecret, code, callbackURL string) (*tokenResponse, error) {
	data := url.Values{
		"grant_type":   {"authorization_code"},
		"code":         {code},
		"redirect_uri": {callbackURL},
		"client_id":    {clientID},
	}
	// Only send client_secret when non-empty. Public OIDC clients (e.g.
	// Keycloak public clients) have no client secret, and some providers
	// reject requests that include an empty client_secret parameter.
	if clientSecret != "" {
		data.Set("client_secret", clientSecret)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", tokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("token exchange failed: %s - %s", resp.Status, string(body))
	}

	var tokenResp tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return nil, fmt.Errorf("failed to decode token response: %w", err)
	}

	return &tokenResp, nil
}

// exchangeGitHubCodeForToken exchanges a GitHub authorization code for an access token.
//
//nolint:unused // Kept for direct OAuth flow compatibility.
func (s *OAuthService) exchangeGitHubCodeForToken(ctx context.Context, code, callbackURL string) (*tokenResponse, error) {
	return s.exchangeGitHubCodeForTokenWithConfig(ctx, s.config.CLI.GitHub, code, callbackURL)
}

// exchangeGitHubCodeForTokenWithConfig exchanges a GitHub authorization code for an access token using the given config.
func (s *OAuthService) exchangeGitHubCodeForTokenWithConfig(ctx context.Context, cfg OAuthProviderConfig, code, callbackURL string) (*tokenResponse, error) {
	data := url.Values{
		"client_id":     {cfg.ClientID},
		"client_secret": {cfg.ClientSecret},
		"code":          {code},
		"redirect_uri":  {callbackURL},
	}

	req, err := http.NewRequestWithContext(ctx, "POST", githubTokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("token exchange failed: %s - %s", resp.Status, string(body))
	}

	var tokenResp tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return nil, fmt.Errorf("failed to decode token response: %w", err)
	}

	if tokenResp.AccessToken == "" {
		// GitHub sometimes returns error in the body
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("no access token in response: %s", string(body))
	}

	return &tokenResp, nil
}

// googleUserInfo represents the response from Google's userinfo endpoint.
type googleUserInfo struct {
	ID            string `json:"id"`
	Email         string `json:"email"`
	VerifiedEmail bool   `json:"verified_email"`
	Name          string `json:"name"`
	GivenName     string `json:"given_name"`
	FamilyName    string `json:"family_name"`
	Picture       string `json:"picture"`
}

// getGoogleUserInfo retrieves user information from Google.
func (s *OAuthService) getGoogleUserInfo(ctx context.Context, accessToken string) (*OAuthUserInfo, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", googleUserURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("failed to get user info: %s - %s", resp.Status, string(body))
	}

	var userInfo googleUserInfo
	if err := json.NewDecoder(resp.Body).Decode(&userInfo); err != nil {
		return nil, fmt.Errorf("failed to decode user info: %w", err)
	}

	return &OAuthUserInfo{
		ID:          userInfo.ID,
		Email:       userInfo.Email,
		DisplayName: userInfo.Name,
		AvatarURL:   userInfo.Picture,
		Provider:    "google",
	}, nil
}

// githubUser represents the response from GitHub's user endpoint.
type githubUser struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	AvatarURL string `json:"avatar_url"`
}

// githubEmail represents an email from GitHub's user/emails endpoint.
type githubEmail struct {
	Email    string `json:"email"`
	Primary  bool   `json:"primary"`
	Verified bool   `json:"verified"`
}

// getGitHubUserInfo retrieves user information from GitHub.
func (s *OAuthService) getGitHubUserInfo(ctx context.Context, accessToken string) (*OAuthUserInfo, error) {
	// Get user profile
	req, err := http.NewRequestWithContext(ctx, "GET", githubUserURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("failed to get user info: %s - %s", resp.Status, string(body))
	}

	var user githubUser
	if err := json.NewDecoder(resp.Body).Decode(&user); err != nil {
		return nil, fmt.Errorf("failed to decode user info: %w", err)
	}

	// If email is not public, fetch from emails endpoint
	email := user.Email
	if email == "" {
		email, err = s.getGitHubPrimaryEmail(ctx, accessToken)
		if err != nil {
			return nil, fmt.Errorf("failed to get user email: %w", err)
		}
	}

	displayName := user.Name
	if displayName == "" {
		displayName = user.Login
	}

	return &OAuthUserInfo{
		ID:          fmt.Sprintf("%d", user.ID),
		Email:       email,
		DisplayName: displayName,
		AvatarURL:   user.AvatarURL,
		Provider:    "github",
	}, nil
}

// getGitHubPrimaryEmail fetches the primary email from GitHub's emails endpoint.
func (s *OAuthService) getGitHubPrimaryEmail(ctx context.Context, accessToken string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", githubEmailURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("failed to get emails: %s - %s", resp.Status, string(body))
	}

	var emails []githubEmail
	if err := json.NewDecoder(resp.Body).Decode(&emails); err != nil {
		return "", fmt.Errorf("failed to decode emails: %w", err)
	}

	// Find primary verified email
	for _, e := range emails {
		if e.Primary && e.Verified {
			return e.Email, nil
		}
	}

	// Fall back to any verified email
	for _, e := range emails {
		if e.Verified {
			return e.Email, nil
		}
	}

	// Fall back to any email
	if len(emails) > 0 {
		return emails[0].Email, nil
	}

	return "", fmt.Errorf("no email found")
}

// DeviceCodeResponse holds the response from a device authorization request.
type DeviceCodeResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete,omitempty"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// googleDeviceCodeResponse handles Google's non-standard field naming.
// Google returns "verification_url" instead of the RFC 8628 "verification_uri".
type googleDeviceCodeResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURL         string `json:"verification_url"`
	VerificationURIComplete string `json:"verification_uri_complete,omitempty"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// DeviceAuthError represents a non-success state from device token polling.
type DeviceAuthError struct {
	Code     string // "authorization_pending", "slow_down", "expired_token"
	Interval int    // updated interval for slow_down
}

func (e *DeviceAuthError) Error() string {
	return fmt.Sprintf("device auth: %s", e.Code)
}

// RequestDeviceCode initiates the device authorization flow with the provider.
func (s *OAuthService) RequestDeviceCode(ctx context.Context, clientType OAuthClientType, provider string) (*DeviceCodeResponse, error) {
	cfg := s.getClientConfig(clientType)

	switch provider {
	case "google":
		return s.requestGoogleDeviceCode(ctx, cfg.Google)
	case "github":
		return s.requestGitHubDeviceCode(ctx, cfg.GitHub)
	default:
		return nil, fmt.Errorf("unsupported OAuth provider for device flow: %s", provider)
	}
}

func (s *OAuthService) requestGoogleDeviceCode(ctx context.Context, cfg OAuthProviderConfig) (*DeviceCodeResponse, error) {
	if cfg.ClientID == "" {
		return nil, fmt.Errorf("google OAuth is not configured")
	}

	data := url.Values{
		"client_id": {cfg.ClientID},
		"scope":     {"openid email profile"},
	}

	req, err := http.NewRequestWithContext(ctx, "POST", googleDeviceCodeURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("device code request failed: %s - %s", resp.Status, string(body))
	}

	// Google returns "verification_url" instead of the RFC 8628 "verification_uri"
	var gResult googleDeviceCodeResponse
	if err := json.NewDecoder(resp.Body).Decode(&gResult); err != nil {
		return nil, fmt.Errorf("failed to decode device code response: %w", err)
	}

	return &DeviceCodeResponse{
		DeviceCode:              gResult.DeviceCode,
		UserCode:                gResult.UserCode,
		VerificationURI:         gResult.VerificationURL,
		VerificationURIComplete: gResult.VerificationURIComplete,
		ExpiresIn:               gResult.ExpiresIn,
		Interval:                gResult.Interval,
	}, nil
}

func (s *OAuthService) requestGitHubDeviceCode(ctx context.Context, cfg OAuthProviderConfig) (*DeviceCodeResponse, error) {
	if cfg.ClientID == "" {
		return nil, fmt.Errorf("GitHub OAuth is not configured")
	}

	data := url.Values{
		"client_id": {cfg.ClientID},
		"scope":     {"read:user user:email"},
	}

	req, err := http.NewRequestWithContext(ctx, "POST", githubDeviceCodeURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("device code request failed: %s - %s", resp.Status, string(body))
	}

	var result DeviceCodeResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode device code response: %w", err)
	}

	return &result, nil
}

// PollDeviceToken polls the provider's token endpoint for the device flow result.
// Returns a tokenResponse on success, or a DeviceAuthError for pending/slow_down/expired states.
func (s *OAuthService) PollDeviceToken(ctx context.Context, clientType OAuthClientType, provider, deviceCode string) (*tokenResponse, error) {
	cfg := s.getClientConfig(clientType)

	switch provider {
	case "google":
		return s.pollGoogleDeviceToken(ctx, cfg.Google, deviceCode)
	case "github":
		return s.pollGitHubDeviceToken(ctx, cfg.GitHub, deviceCode)
	default:
		return nil, fmt.Errorf("unsupported OAuth provider for device flow: %s", provider)
	}
}

// deviceTokenErrorResponse represents an error response from a device token endpoint.
type deviceTokenErrorResponse struct {
	Error    string `json:"error"`
	Interval int    `json:"interval,omitempty"`
}

func (s *OAuthService) pollGoogleDeviceToken(ctx context.Context, cfg OAuthProviderConfig, deviceCode string) (*tokenResponse, error) {
	data := url.Values{
		"client_id":     {cfg.ClientID},
		"client_secret": {cfg.ClientSecret},
		"device_code":   {deviceCode},
		"grant_type":    {"urn:ietf:params:oauth:grant-type:device_code"},
	}

	req, err := http.NewRequestWithContext(ctx, "POST", googleTokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read device token response: %w", err)
	}

	if resp.StatusCode == http.StatusOK {
		var tokenResp tokenResponse
		if err := json.Unmarshal(body, &tokenResp); err != nil {
			return nil, fmt.Errorf("failed to decode token response: %w", err)
		}
		return &tokenResp, nil
	}

	// Parse error response
	var errResp deviceTokenErrorResponse
	if err := json.Unmarshal(body, &errResp); err != nil {
		return nil, fmt.Errorf("device token poll failed: %s - %s", resp.Status, string(body))
	}

	switch errResp.Error {
	case "authorization_pending", "slow_down", "expired_token":
		return nil, &DeviceAuthError{Code: errResp.Error, Interval: errResp.Interval}
	default:
		return nil, fmt.Errorf("device token poll failed: %s", errResp.Error)
	}
}

func (s *OAuthService) pollGitHubDeviceToken(ctx context.Context, cfg OAuthProviderConfig, deviceCode string) (*tokenResponse, error) {
	data := url.Values{
		"client_id":   {cfg.ClientID},
		"device_code": {deviceCode},
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
	}

	req, err := http.NewRequestWithContext(ctx, "POST", githubTokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read device token response: %w", err)
	}

	// GitHub returns 200 for all responses, using "error" field to indicate state
	var errCheck deviceTokenErrorResponse
	if err := json.Unmarshal(body, &errCheck); err == nil && errCheck.Error != "" {
		switch errCheck.Error {
		case "authorization_pending", "slow_down", "expired_token":
			return nil, &DeviceAuthError{Code: errCheck.Error, Interval: errCheck.Interval}
		default:
			return nil, fmt.Errorf("device token poll failed: %s", errCheck.Error)
		}
	}

	if resp.StatusCode == http.StatusOK {
		var tokenResp tokenResponse
		if err := json.Unmarshal(body, &tokenResp); err != nil {
			return nil, fmt.Errorf("failed to decode token response: %w", err)
		}
		if tokenResp.AccessToken == "" {
			return nil, fmt.Errorf("no access token in response: %s", string(body))
		}
		return &tokenResp, nil
	}

	return nil, fmt.Errorf("device token poll failed: %s - %s", resp.Status, string(body))
}

// --- OIDC Login Methods ---

// OIDCDisplayName returns the human-readable name for the OIDC login provider.
// Falls back to "SSO" if no display name is configured.
func (s *OAuthService) OIDCDisplayName() string {
	if s.oidcLoginConfig != nil && s.oidcLoginConfig.DisplayName != "" {
		return s.oidcLoginConfig.DisplayName
	}
	return "SSO"
}

// oidcUserInfoResponse represents the standard OIDC userinfo response.
type oidcUserInfoResponse struct {
	Sub               string `json:"sub"`
	Email             string `json:"email"`
	EmailVerified     bool   `json:"email_verified"`
	Name              string `json:"name"`
	PreferredUsername string `json:"preferred_username"`
	Picture           string `json:"picture"`
}

// oidcScopes returns the configured scopes or the default OIDC scopes.
func (s *OAuthService) oidcScopes() string {
	if s.oidcLoginConfig != nil && len(s.oidcLoginConfig.Scopes) > 0 {
		return strings.Join(s.oidcLoginConfig.Scopes, " ")
	}
	return "openid email profile"
}

// getOIDCAuthURL generates an authorization URL for the configured OIDC provider.
func (s *OAuthService) getOIDCAuthURL(callbackURL, state string) (string, error) {
	if s.oidcLoginConfig == nil || !s.oidcLoginConfig.Enabled {
		return "", fmt.Errorf("OIDC login is not configured")
	}

	doc, err := s.oidcCache.get(s.oidcLoginConfig.IssuerURL, s.httpClient)
	if err != nil {
		return "", fmt.Errorf("OIDC discovery failed: %w", err)
	}

	u, err := url.Parse(doc.AuthorizationEndpoint)
	if err != nil {
		return "", fmt.Errorf("invalid authorization endpoint URL: %w", err)
	}
	q := u.Query()
	q.Set("client_id", s.oidcLoginConfig.ClientID)
	q.Set("redirect_uri", callbackURL)
	q.Set("response_type", "code")
	q.Set("scope", s.oidcScopes())
	q.Set("state", state)
	u.RawQuery = q.Encode()

	return u.String(), nil
}

// exchangeOIDCCode exchanges an OIDC authorization code for user information.
func (s *OAuthService) exchangeOIDCCode(ctx context.Context, code, callbackURL string) (*OAuthUserInfo, error) {
	if s.oidcLoginConfig == nil || !s.oidcLoginConfig.Enabled {
		return nil, fmt.Errorf("OIDC login is not configured")
	}

	doc, err := s.oidcCache.get(s.oidcLoginConfig.IssuerURL, s.httpClient)
	if err != nil {
		return nil, fmt.Errorf("OIDC discovery failed: %w", err)
	}

	// Reuse the existing generic token exchange
	tokenResp, err := s.exchangeCodeForToken(ctx, doc.TokenEndpoint,
		s.oidcLoginConfig.ClientID, s.oidcLoginConfig.ClientSecret,
		code, callbackURL)
	if err != nil {
		return nil, fmt.Errorf("OIDC token exchange failed: %w", err)
	}

	return s.getOIDCUserInfo(ctx, tokenResp.AccessToken, doc.UserinfoEndpoint)
}

// getOIDCUserInfo fetches user information from the OIDC userinfo endpoint.
func (s *OAuthService) getOIDCUserInfo(ctx context.Context, accessToken, userinfoURL string) (*OAuthUserInfo, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", userinfoURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create userinfo request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch OIDC userinfo: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// Limit error body to 1 KB to avoid OOM on malicious responses.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		return nil, fmt.Errorf("OIDC userinfo endpoint returned %d: %s", resp.StatusCode, string(body))
	}

	var userInfo oidcUserInfoResponse
	// Limit userinfo response to 1 MB to prevent OOM from oversized payloads.
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&userInfo); err != nil {
		return nil, fmt.Errorf("failed to decode OIDC userinfo response: %w", err)
	}

	// The "sub" claim is the primary user identifier in OIDC; it must be present.
	if userInfo.Sub == "" {
		return nil, fmt.Errorf("OIDC provider did not return a 'sub' claim; the identity provider must include a subject identifier")
	}

	if userInfo.Email == "" {
		return nil, fmt.Errorf("OIDC provider did not return an email claim; ensure the 'email' scope is requested and the user has an email address configured in the identity provider")
	}

	if !userInfo.EmailVerified {
		return nil, fmt.Errorf("OIDC provider returned an unverified email address %q; "+
			"the user must verify their email in the identity provider before logging in", userInfo.Email)
	}

	// Determine display name: prefer Name, fall back to PreferredUsername, then Email.
	displayName := userInfo.Name
	if displayName == "" {
		displayName = userInfo.PreferredUsername
	}
	if displayName == "" {
		displayName = userInfo.Email
	}

	return &OAuthUserInfo{
		ID:          userInfo.Sub,
		Email:       userInfo.Email,
		DisplayName: displayName,
		AvatarURL:   userInfo.Picture,
		Provider:    hubclient.OAuthProviderOIDC,
	}, nil
}

// validateOIDCLoginConfig validates the OIDC login configuration at startup.
// It ensures all required fields are present and the IssuerURL is well-formed.
func validateOIDCLoginConfig(cfg *config.OIDCLoginConfig) error {
	if cfg.IssuerURL == "" {
		return fmt.Errorf("issuerUrl is required when OIDC login is enabled")
	}
	parsed, err := url.Parse(cfg.IssuerURL)
	if err != nil {
		return fmt.Errorf("issuerUrl is not a valid URL: %w", err)
	}
	// Require https, but allow http for localhost/127.0.0.1 (local dev).
	host := parsed.Hostname()
	if parsed.Scheme != "https" && (parsed.Scheme != "http" || (host != "localhost" && host != "127.0.0.1")) {
		return fmt.Errorf("issuerUrl must use https scheme (got %q)", parsed.Scheme)
	}
	if parsed.RawQuery != "" {
		return fmt.Errorf("issuerUrl must not contain query parameters")
	}
	if parsed.Fragment != "" {
		return fmt.Errorf("issuerUrl must not contain a fragment")
	}
	if cfg.ClientID == "" {
		return fmt.Errorf("clientId is required when OIDC login is enabled")
	}
	// clientSecret is intentionally not required: public OIDC clients (e.g.
	// Keycloak public clients) legitimately have no client secret.
	// TODO: Implement PKCE (RFC 7636) support for public OIDC clients to prevent authorization code interception attacks.
	return nil
}
