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

// Package githubapp implements GitHub App authentication for the Scion Hub.
// It handles JWT generation from private keys and installation access token minting.
package githubapp

import (
	"context"
	"crypto/hmac"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// Config holds the GitHub App configuration.
type Config struct {
	AppID           int64  `json:"app_id" yaml:"app_id" koanf:"app_id"`
	PrivateKeyPath  string `json:"private_key_path,omitempty" yaml:"private_key_path,omitempty" koanf:"private_key_path"`
	PrivateKey      string `json:"private_key,omitempty" yaml:"private_key,omitempty" koanf:"private_key"`
	WebhookSecret   string `json:"webhook_secret,omitempty" yaml:"webhook_secret,omitempty" koanf:"webhook_secret"`
	APIBaseURL      string `json:"api_base_url,omitempty" yaml:"api_base_url,omitempty" koanf:"api_base_url"`
	WebhooksEnabled bool   `json:"webhooks_enabled,omitempty" yaml:"webhooks_enabled,omitempty" koanf:"webhooks_enabled"`
}

// IsConfigured returns true if the GitHub App config has the minimum required fields.
func (c *Config) IsConfigured() bool {
	return c.AppID != 0 && (c.PrivateKeyPath != "" || c.PrivateKey != "")
}

// TokenPermissions specifies the permissions to request when minting an installation token.
type TokenPermissions struct {
	Contents     string `json:"contents,omitempty"`
	PullRequests string `json:"pull_requests,omitempty"`
	Issues       string `json:"issues,omitempty"`
	Metadata     string `json:"metadata,omitempty"`
	Checks       string `json:"checks,omitempty"`
	Actions      string `json:"actions,omitempty"`
}

// DefaultTokenPermissions returns the default permissions for installation tokens.
func DefaultTokenPermissions() TokenPermissions {
	return TokenPermissions{
		Contents:     "write",
		PullRequests: "write",
		Issues:       "write",
		Metadata:     "read",
	}
}

// InstallationToken represents a minted GitHub App installation access token.
type InstallationToken struct {
	Token       string            `json:"token"`
	ExpiresAt   time.Time         `json:"expires_at"`
	Permissions map[string]string `json:"permissions"`
}

// TokenMintError classifies GitHub API errors during token minting.
type TokenMintError struct {
	ErrorCode  string // One of the error codes below
	StatusCode int    // HTTP status from GitHub API
	Message    string // Human-readable error message
	Err        error  // Underlying error
}

func (e *TokenMintError) Error() string {
	return fmt.Sprintf("github app token mint error (%s): %s", e.ErrorCode, e.Message)
}

func (e *TokenMintError) Unwrap() error {
	return e.Err
}

// Error codes for token minting failures.
const (
	ErrCodeInstallationRevoked   = "installation_revoked"
	ErrCodeInstallationSuspended = "installation_suspended"
	ErrCodeRepoNotAccessible     = "repo_not_accessible"
	ErrCodePermissionDenied      = "permission_denied"
	ErrCodeTokenMintFailed       = "token_mint_failed"
	ErrCodePrivateKeyInvalid     = "private_key_invalid"
	ErrCodeAppNotFound           = "app_not_found"
)

// RateLimitInfo holds GitHub API rate limit information from response headers.
type RateLimitInfo struct {
	Limit     int       `json:"limit"`
	Remaining int       `json:"remaining"`
	Reset     time.Time `json:"reset"`
	Used      int       `json:"used"`
}

// parseRateLimitHeaders extracts rate limit info from a GitHub API response.
func parseRateLimitHeaders(resp *http.Response) *RateLimitInfo {
	info := &RateLimitInfo{}
	if v := resp.Header.Get("X-RateLimit-Limit"); v != "" {
		info.Limit, _ = strconv.Atoi(v)
	}
	if v := resp.Header.Get("X-RateLimit-Remaining"); v != "" {
		info.Remaining, _ = strconv.Atoi(v)
	}
	if v := resp.Header.Get("X-RateLimit-Used"); v != "" {
		info.Used, _ = strconv.Atoi(v)
	}
	if v := resp.Header.Get("X-RateLimit-Reset"); v != "" {
		if epoch, err := strconv.ParseInt(v, 10, 64); err == nil {
			info.Reset = time.Unix(epoch, 0)
		}
	}
	return info
}

// Client handles GitHub App authentication operations.
type Client struct {
	appID      int64
	privateKey *rsa.PrivateKey
	apiBaseURL string
	httpClient *http.Client

	mu            sync.Mutex
	lastRateLimit *RateLimitInfo
}

// GetRateLimit returns the most recently observed rate limit info.
func (c *Client) GetRateLimit() *RateLimitInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lastRateLimit == nil {
		return nil
	}
	rl := *c.lastRateLimit
	return &rl
}

// trackRateLimit parses rate limit headers from a response, stores the info,
// and logs a warning if the remaining quota is below 20%.
func (c *Client) trackRateLimit(resp *http.Response) {
	rl := parseRateLimitHeaders(resp)
	c.mu.Lock()
	c.lastRateLimit = rl
	c.mu.Unlock()

	if rl.Limit > 0 && rl.Remaining < rl.Limit/5 {
		slog.Warn("GitHub API rate limit running low",
			"remaining", rl.Remaining,
			"limit", rl.Limit,
			"reset", rl.Reset.Format(time.RFC3339))
	}
}

// KeyFingerprint returns a short SHA-256 fingerprint of the RSA public key
// for diagnostic logging (first 16 hex chars). This helps identify which key
// is being used without exposing the key itself.
func KeyFingerprint(key *rsa.PrivateKey) string {
	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return "unknown"
	}
	hash := sha256.Sum256(pubDER)
	return hex.EncodeToString(hash[:8])
}

// NewClient creates a new GitHub App client from the given config.
// It parses the private key (from inline PEM or file path) and validates it.
func NewClient(cfg Config, keyData []byte) (*Client, error) {
	if cfg.AppID == 0 {
		return nil, fmt.Errorf("github app: app_id is required")
	}

	privateKey, err := ParsePrivateKey(keyData)
	if err != nil {
		return nil, &TokenMintError{
			ErrorCode: ErrCodePrivateKeyInvalid,
			Message:   fmt.Sprintf("failed to parse private key: %v", err),
			Err:       err,
		}
	}

	slog.Debug("GitHub App client created",
		"app_id", cfg.AppID,
		"key_fingerprint", KeyFingerprint(privateKey),
		"key_bits", privateKey.N.BitLen(),
	)

	apiBaseURL := cfg.APIBaseURL
	if apiBaseURL == "" {
		apiBaseURL = "https://api.github.com"
	}
	apiBaseURL = strings.TrimRight(apiBaseURL, "/")

	return &Client{
		appID:      cfg.AppID,
		privateKey: privateKey,
		apiBaseURL: apiBaseURL,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// ParsePrivateKey parses a PEM-encoded RSA private key (PKCS1 or PKCS8).
func ParsePrivateKey(pemData []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemData)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found in private key data")
	}

	// Try PKCS1 first (RSA PRIVATE KEY)
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}

	// Try PKCS8 (PRIVATE KEY)
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse private key (tried PKCS1 and PKCS8): %w", err)
	}

	rsaKey, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("private key is not RSA (got %T)", parsed)
	}

	return rsaKey, nil
}

// GenerateJWT generates a signed JWT for GitHub App authentication.
// The JWT is used to authenticate as the app itself (not an installation).
// It has a maximum lifetime of 10 minutes per GitHub's requirements.
func (c *Client) GenerateJWT() (string, error) {
	now := time.Now()

	sig, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: c.privateKey},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		return "", &TokenMintError{
			ErrorCode: ErrCodePrivateKeyInvalid,
			Message:   fmt.Sprintf("failed to create JWT signer: %v", err),
			Err:       err,
		}
	}

	claims := jwt.Claims{
		Issuer:   fmt.Sprintf("%d", c.appID),
		IssuedAt: jwt.NewNumericDate(now.Add(-60 * time.Second)), // 60s clock drift allowance
		Expiry:   jwt.NewNumericDate(now.Add(10 * time.Minute)),
	}

	token, err := jwt.Signed(sig).Claims(claims).Serialize()
	if err != nil {
		return "", &TokenMintError{
			ErrorCode: ErrCodePrivateKeyInvalid,
			Message:   fmt.Sprintf("failed to sign JWT: %v", err),
			Err:       err,
		}
	}

	return token, nil
}

// accessTokenRequest is the request body for creating an installation access token.
type accessTokenRequest struct {
	Repositories []string          `json:"repositories,omitempty"`
	Permissions  map[string]string `json:"permissions,omitempty"`
}

// accessTokenResponse is the response from GitHub when creating an installation access token.
type accessTokenResponse struct {
	Token       string            `json:"token"`
	ExpiresAt   string            `json:"expires_at"`
	Permissions map[string]string `json:"permissions"`
}

// githubErrorResponse is the error response from the GitHub API.
type githubErrorResponse struct {
	Message          string `json:"message"`
	DocumentationURL string `json:"documentation_url"`
}

// MintInstallationToken creates a new installation access token scoped to the
// given repositories and permissions.
func (c *Client) MintInstallationToken(ctx context.Context, installationID int64, repos []string, perms TokenPermissions) (*InstallationToken, error) {
	jwtToken, err := c.GenerateJWT()
	if err != nil {
		return nil, err
	}

	// Build permissions map (only include non-empty values)
	permMap := make(map[string]string)
	if perms.Contents != "" {
		permMap["contents"] = perms.Contents
	}
	if perms.PullRequests != "" {
		permMap["pull_requests"] = perms.PullRequests
	}
	if perms.Issues != "" {
		permMap["issues"] = perms.Issues
	}
	if perms.Metadata != "" {
		permMap["metadata"] = perms.Metadata
	}
	if perms.Checks != "" {
		permMap["checks"] = perms.Checks
	}
	if perms.Actions != "" {
		permMap["actions"] = perms.Actions
	}

	reqBody := accessTokenRequest{
		Repositories: repos,
		Permissions:  permMap,
	}

	bodyJSON, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal token request: %w", err)
	}

	url := fmt.Sprintf("%s/app/installations/%d/access_tokens", c.apiBaseURL, installationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(bodyJSON)))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+jwtToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, &TokenMintError{
			ErrorCode: ErrCodeTokenMintFailed,
			Message:   fmt.Sprintf("HTTP request failed: %v", err),
			Err:       err,
		}
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &TokenMintError{
			ErrorCode: ErrCodeTokenMintFailed,
			Message:   "failed to read response body",
			Err:       err,
		}
	}

	c.trackRateLimit(resp)

	if resp.StatusCode != http.StatusCreated {
		return nil, classifyGitHubError(resp.StatusCode, body)
	}

	var tokenResp accessTokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, &TokenMintError{
			ErrorCode: ErrCodeTokenMintFailed,
			Message:   "failed to parse token response",
			Err:       err,
		}
	}

	expiresAt, err := time.Parse(time.RFC3339, tokenResp.ExpiresAt)
	if err != nil {
		// Fallback: assume 1 hour from now
		expiresAt = time.Now().Add(1 * time.Hour)
	}

	return &InstallationToken{
		Token:       tokenResp.Token,
		ExpiresAt:   expiresAt,
		Permissions: tokenResp.Permissions,
	}, nil
}

// classifyGitHubError maps a GitHub API error response to a TokenMintError with
// an appropriate error code.
func classifyGitHubError(statusCode int, body []byte) *TokenMintError {
	var ghErr githubErrorResponse
	_ = json.Unmarshal(body, &ghErr) // best effort

	msg := ghErr.Message
	if msg == "" {
		msg = fmt.Sprintf("GitHub API returned status %d", statusCode)
	}

	switch statusCode {
	case http.StatusUnauthorized:
		// 401: JWT is invalid (bad app ID, expired key, etc.)
		if strings.Contains(strings.ToLower(msg), "app not found") ||
			strings.Contains(strings.ToLower(msg), "integration not found") {
			return &TokenMintError{
				ErrorCode:  ErrCodeAppNotFound,
				StatusCode: statusCode,
				Message:    fmt.Sprintf("GitHub App not found — verify the App ID in Admin > Server Config > GitHub App. GitHub says: %s", msg),
			}
		}
		return &TokenMintError{
			ErrorCode:  ErrCodePrivateKeyInvalid,
			StatusCode: statusCode,
			Message:    fmt.Sprintf("GitHub rejected the JWT — the private key likely doesn't match the App. Regenerate the key in GitHub App settings and re-upload it in Admin > Server Config. GitHub says: %s", msg),
		}

	case http.StatusForbidden:
		// 403: Installation suspended or permission issue
		if strings.Contains(strings.ToLower(msg), "suspended") {
			return &TokenMintError{
				ErrorCode:  ErrCodeInstallationSuspended,
				StatusCode: statusCode,
				Message:    fmt.Sprintf("Installation is suspended: %s", msg),
			}
		}
		return &TokenMintError{
			ErrorCode:  ErrCodePermissionDenied,
			StatusCode: statusCode,
			Message:    fmt.Sprintf("Permission denied: %s", msg),
		}

	case http.StatusNotFound:
		// 404: Installation doesn't exist (revoked/deleted)
		return &TokenMintError{
			ErrorCode:  ErrCodeInstallationRevoked,
			StatusCode: statusCode,
			Message:    fmt.Sprintf("Installation not found (may have been revoked): %s", msg),
		}

	case http.StatusUnprocessableEntity:
		// 422: Typically repo not accessible or invalid permissions
		if strings.Contains(strings.ToLower(msg), "repository") {
			return &TokenMintError{
				ErrorCode:  ErrCodeRepoNotAccessible,
				StatusCode: statusCode,
				Message:    fmt.Sprintf("Repository not accessible: %s", msg),
			}
		}
		return &TokenMintError{
			ErrorCode:  ErrCodePermissionDenied,
			StatusCode: statusCode,
			Message:    fmt.Sprintf("Invalid request: %s", msg),
		}

	default:
		return &TokenMintError{
			ErrorCode:  ErrCodeTokenMintFailed,
			StatusCode: statusCode,
			Message:    fmt.Sprintf("GitHub API error (status %d): %s", statusCode, msg),
		}
	}
}

// Installation represents a GitHub App installation as returned by the GitHub API.
type Installation struct {
	ID      int64 `json:"id"`
	Account struct {
		Login string `json:"login"`
		Type  string `json:"type"` // "Organization" or "User"
	} `json:"account"`
	AppID               int64   `json:"app_id"`
	TargetType          string  `json:"target_type"`
	RepositorySelection string  `json:"repository_selection"` // "all" or "selected"
	SuspendedAt         *string `json:"suspended_at"`
}

// InstallationRepository represents a repository accessible to a GitHub App installation.
type InstallationRepository struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	FullName string `json:"full_name"` // "owner/repo"
	Private  bool   `json:"private"`
}

// GetInstallation retrieves a specific installation by ID.
func (c *Client) GetInstallation(ctx context.Context, installationID int64) (*Installation, error) {
	jwtToken, err := c.GenerateJWT()
	if err != nil {
		return nil, err
	}

	url := fmt.Sprintf("%s/app/installations/%d", c.apiBaseURL, installationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+jwtToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	c.trackRateLimit(resp)

	if resp.StatusCode != http.StatusOK {
		return nil, classifyGitHubError(resp.StatusCode, body)
	}

	var installation Installation
	if err := json.Unmarshal(body, &installation); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	return &installation, nil
}

// ListInstallations lists all installations for the authenticated app.
func (c *Client) ListInstallations(ctx context.Context) ([]Installation, error) {
	jwtToken, err := c.GenerateJWT()
	if err != nil {
		return nil, err
	}

	var allInstallations []Installation
	page := 1

	for {
		url := fmt.Sprintf("%s/app/installations?per_page=100&page=%d", c.apiBaseURL, page)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}

		req.Header.Set("Authorization", "Bearer "+jwtToken)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("HTTP request failed: %w", err)
		}

		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("failed to read response: %w", err)
		}

		c.trackRateLimit(resp)

		if resp.StatusCode != http.StatusOK {
			return nil, classifyGitHubError(resp.StatusCode, body)
		}

		var installations []Installation
		if err := json.Unmarshal(body, &installations); err != nil {
			return nil, fmt.Errorf("failed to parse response: %w", err)
		}

		allInstallations = append(allInstallations, installations...)

		if len(installations) < 100 {
			break
		}
		page++
	}

	return allInstallations, nil
}

// ListInstallationRepos lists repositories accessible to an installation.
// Uses an installation access token (not JWT).
func (c *Client) ListInstallationRepos(ctx context.Context, installationID int64) ([]InstallationRepository, error) {
	// First mint a token for this installation with minimal permissions
	token, err := c.MintInstallationToken(ctx, installationID, nil, TokenPermissions{Metadata: "read"})
	if err != nil {
		return nil, fmt.Errorf("failed to mint token for repo listing: %w", err)
	}

	var allRepos []InstallationRepository
	page := 1

	for {
		url := fmt.Sprintf("%s/installation/repositories?per_page=100&page=%d", c.apiBaseURL, page)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}

		req.Header.Set("Authorization", "Bearer "+token.Token)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("HTTP request failed: %w", err)
		}

		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("failed to read response: %w", err)
		}

		c.trackRateLimit(resp)

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("failed to list repos (status %d): %s", resp.StatusCode, string(body))
		}

		var result struct {
			Repositories []InstallationRepository `json:"repositories"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			return nil, fmt.Errorf("failed to parse response: %w", err)
		}

		allRepos = append(allRepos, result.Repositories...)

		if len(result.Repositories) < 100 {
			break
		}
		page++
	}

	return allRepos, nil
}

// VerifyWebhookSignature validates a GitHub webhook payload signature.
// GitHub sends the signature in the X-Hub-Signature-256 header as "sha256=<hex>".
func VerifyWebhookSignature(payload []byte, signature string, secret string) bool {
	if secret == "" || signature == "" {
		return false
	}

	const prefix = "sha256="
	if !strings.HasPrefix(signature, prefix) {
		return false
	}
	sigHex := signature[len(prefix):]

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	expectedMAC := mac.Sum(nil)
	expectedHex := fmt.Sprintf("%x", expectedMAC)

	return hmac.Equal([]byte(sigHex), []byte(expectedHex))
}

// GetApp retrieves the authenticated GitHub App's information.
// Used to verify the app configuration and check registered permissions.
func (c *Client) GetApp(ctx context.Context) (map[string]interface{}, error) {
	jwtToken, err := c.GenerateJWT()
	if err != nil {
		return nil, err
	}

	url := fmt.Sprintf("%s/app", c.apiBaseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+jwtToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	c.trackRateLimit(resp)

	if resp.StatusCode != http.StatusOK {
		return nil, classifyGitHubError(resp.StatusCode, body)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	return result, nil
}
