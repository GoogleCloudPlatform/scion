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
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
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

// IntersectTokenPermissions caps each non-empty field of requested to the level
// granted in appPermissions ("read" or "write"). It never adds fields that are
// empty in requested, never widens a level above requested or above "write"
// (an App grant of "admin" satisfies "write" or "read" without widening), and
// drops fields absent from or with unrecognized levels in appPermissions.
// If appPermissions is nil or empty, it returns the zero TokenPermissions.
func IntersectTokenPermissions(requested TokenPermissions, appPermissions map[string]string) TokenPermissions {
	if len(appPermissions) == 0 {
		return TokenPermissions{}
	}
	capLevel := func(req, app string) string {
		switch req {
		case "read":
			if app == "read" || app == "write" || app == "admin" {
				return "read"
			}
			return ""
		case "write":
			switch app {
			case "write", "admin":
				return "write"
			case "read":
				return "read"
			default:
				return ""
			}
		default:
			return ""
		}
	}
	return TokenPermissions{
		Contents:     capLevel(requested.Contents, appPermissions["contents"]),
		PullRequests: capLevel(requested.PullRequests, appPermissions["pull_requests"]),
		Issues:       capLevel(requested.Issues, appPermissions["issues"]),
		Metadata:     capLevel(requested.Metadata, appPermissions["metadata"]),
		Checks:       capLevel(requested.Checks, appPermissions["checks"]),
		Actions:      capLevel(requested.Actions, appPermissions["actions"]),
	}
}

// InstallationToken represents a minted GitHub App installation access token.
type InstallationToken struct {
	Token       string            `json:"token"`
	ExpiresAt   time.Time         `json:"expires_at"`
	Permissions map[string]string `json:"permissions"`
}

// TokenMintError classifies GitHub API errors during token minting.
//
// Message is fixed text for the error code: it never carries text received
// from GitHub, so it is safe to return to API callers and to store as a
// project status. GitHub's own text (its "message" field, an error body, or
// a transport error) is kept apart and read through UpstreamDetail, for the
// hub to log once, capped and sanitised (ptone/scion#7498).
type TokenMintError struct {
	ErrorCode  string // One of the error codes below
	StatusCode int    // HTTP status from GitHub API
	Message    string // Fixed, human-readable error message (no upstream text)
	Err        error  // Underlying error

	upstream string // text received from GitHub or the transport; see UpstreamDetail
}

func (e *TokenMintError) Error() string {
	return fmt.Sprintf("github app token mint error (%s): %s", e.ErrorCode, e.Message)
}

func (e *TokenMintError) Unwrap() error {
	return e.Err
}

// UpstreamDetail returns the text GitHub or the transport sent with the
// failure, unrendered. It is for the server log only: callers render it
// (the hub's upstreamText) and never return it to a client, store it, or use
// it as a metrics label.
func (e *TokenMintError) UpstreamDetail() string {
	return e.upstream
}

// upstreamBodyReadLimit bounds how much of a GitHub API error response body
// this package reads. It equals the hub's upstreamErrorBodyReadLimit
// (pkg/hub/upstream_text.go); keep the two in step.
const upstreamBodyReadLimit = 4 << 10

// appMetadataReadLimit bounds how much of a 200 OK GET /app response body
// GetApp reads. Realistic GitHub App payloads (owner URLs, description,
// subscribed events, and permissions) routinely exceed 4 KiB.
const appMetadataReadLimit = 1 << 20

// githubSuccessBodyReadLimit bounds how much of a successful GitHub API
// response body MintInstallationToken, GetInstallation, ListInstallations,
// and ListInstallationRepos read. A 100-item repository or installation page
// can exceed 1 MiB, and this matches commentListResponseBodyLimit.
const githubSuccessBodyReadLimit = 8 << 20

// errResponseBodyTooLarge is returned when a successful GitHub API response
// body exceeds its configured read bound.
var errResponseBodyTooLarge = errors.New("response body exceeds read limit")

// readBoundedBody reads r up to limit bytes. If r yields more than limit
// bytes, it returns an error wrapping errResponseBodyTooLarge rather than
// returning a silently truncated body.
func readBoundedBody(r io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("%w (%d bytes)", errResponseBodyTooLarge, limit)
	}
	return body, nil
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
			"reset", rl.Reset.UTC().Format(time.RFC3339))
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

	// A request without permissions gets every permission the installation
	// has. Callers always name the permissions they need; refuse rather
	// than send an empty set.
	if len(permMap) == 0 {
		return nil, &TokenMintError{
			ErrorCode: ErrCodeTokenMintFailed,
			Message:   "refusing to request an installation token without explicit permissions",
		}
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
			Message:   "GitHub API request failed",
			Err:       err,
			upstream:  err.Error(),
		}
	}
	defer func() { _ = resp.Body.Close() }()

	c.trackRateLimit(resp)

	if resp.StatusCode != http.StatusCreated {
		errBody, err := io.ReadAll(io.LimitReader(resp.Body, upstreamBodyReadLimit))
		if err != nil {
			return nil, &TokenMintError{
				ErrorCode:  ErrCodeTokenMintFailed,
				StatusCode: resp.StatusCode,
				Message:    "failed to read response body",
				Err:        err,
				upstream:   err.Error(),
			}
		}
		return nil, classifyGitHubError(resp.StatusCode, errBody)
	}

	body, err := readBoundedBody(resp.Body, githubSuccessBodyReadLimit)
	if err != nil {
		msg := "failed to read response body"
		if errors.Is(err, errResponseBodyTooLarge) {
			msg = "GitHub API response body exceeded size limit"
		}
		return nil, &TokenMintError{
			ErrorCode:  ErrCodeTokenMintFailed,
			StatusCode: resp.StatusCode,
			Message:    msg,
			Err:        err,
			upstream:   err.Error(),
		}
	}

	var tokenResp accessTokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, &TokenMintError{
			ErrorCode:  ErrCodeTokenMintFailed,
			StatusCode: resp.StatusCode,
			Message:    "failed to parse token response",
			Err:        err,
			upstream:   err.Error(),
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
// an appropriate error code. GitHub's "message" field only chooses the code;
// the returned Message is fixed text for that code, and GitHub's text (the
// field, or the body when the field is absent) is kept for UpstreamDetail.
func classifyGitHubError(statusCode int, body []byte) *TokenMintError {
	var ghErr githubErrorResponse
	_ = json.Unmarshal(body, &ghErr) // best effort

	detail := ghErr.Message
	if detail == "" {
		detail = string(body)
	}
	lower := strings.ToLower(ghErr.Message)
	mintErr := func(code, message string) *TokenMintError {
		return &TokenMintError{ErrorCode: code, StatusCode: statusCode, Message: message, upstream: detail}
	}

	switch statusCode {
	case http.StatusUnauthorized:
		// 401: JWT is invalid (bad app ID, expired key, etc.)
		if strings.Contains(lower, "app not found") || strings.Contains(lower, "integration not found") {
			return mintErr(ErrCodeAppNotFound,
				"GitHub App not found — verify the App ID in Admin > Server Config > GitHub App.")
		}
		return mintErr(ErrCodePrivateKeyInvalid,
			"GitHub rejected the JWT — the private key likely doesn't match the App. Regenerate the key in GitHub App settings and re-upload it in Admin > Server Config.")

	case http.StatusForbidden:
		// 403: Installation suspended or permission issue
		if strings.Contains(lower, "suspended") {
			return mintErr(ErrCodeInstallationSuspended, "The GitHub App installation is suspended.")
		}
		return mintErr(ErrCodePermissionDenied, "GitHub denied the GitHub App permission for this request.")

	case http.StatusNotFound:
		// 404: Installation doesn't exist (revoked/deleted)
		return mintErr(ErrCodeInstallationRevoked,
			"The GitHub App installation was not found; it may have been revoked.")

	case http.StatusUnprocessableEntity:
		// 422: Typically repo not accessible or invalid permissions
		if strings.Contains(lower, "repository") {
			return mintErr(ErrCodeRepoNotAccessible,
				"The repository is not accessible to the GitHub App installation.")
		}
		return mintErr(ErrCodePermissionDenied, "GitHub rejected the token request as invalid.")

	default:
		return mintErr(ErrCodeTokenMintFailed, fmt.Sprintf("GitHub API error (status %d).", statusCode))
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

	c.trackRateLimit(resp)

	if resp.StatusCode != http.StatusOK {
		errBody, err := io.ReadAll(io.LimitReader(resp.Body, upstreamBodyReadLimit))
		if err != nil {
			return nil, fmt.Errorf("failed to read response: %w", err)
		}
		return nil, classifyGitHubError(resp.StatusCode, errBody)
	}

	body, err := readBoundedBody(resp.Body, githubSuccessBodyReadLimit)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
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

		c.trackRateLimit(resp)

		if resp.StatusCode != http.StatusOK {
			errBody, err := io.ReadAll(io.LimitReader(resp.Body, upstreamBodyReadLimit))
			_ = resp.Body.Close()
			if err != nil {
				return nil, fmt.Errorf("failed to read response: %w", err)
			}
			return nil, classifyGitHubError(resp.StatusCode, errBody)
		}

		body, err := readBoundedBody(resp.Body, githubSuccessBodyReadLimit)
		_ = resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("failed to read response: %w", err)
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

		c.trackRateLimit(resp)

		if resp.StatusCode != http.StatusOK {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, upstreamBodyReadLimit))
			_ = resp.Body.Close()
			// GitHub's body is not part of the error (ptone/scion#7498).
			return nil, fmt.Errorf("failed to list repos (status %d)", resp.StatusCode)
		}

		body, err := readBoundedBody(resp.Body, githubSuccessBodyReadLimit)
		_ = resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("failed to read response: %w", err)
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

	c.trackRateLimit(resp)

	if resp.StatusCode != http.StatusOK {
		errBody, err := io.ReadAll(io.LimitReader(resp.Body, upstreamBodyReadLimit))
		if err != nil {
			return nil, fmt.Errorf("failed to read response: %w", err)
		}
		return nil, classifyGitHubError(resp.StatusCode, errBody)
	}

	body, err := readBoundedBody(resp.Body, appMetadataReadLimit)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	return result, nil
}

// AppIdentity holds the GitHub App's metadata, subscribed webhook events, and
// permissions returned by GET /app.
type AppIdentity struct {
	AppID       int64             `json:"app_id"`
	Slug        string            `json:"slug"`
	BotLogin    string            `json:"bot_login"`
	Events      []string          `json:"events"`
	Permissions map[string]string `json:"permissions"`
	FetchedAt   time.Time         `json:"fetched_at"`
}

// Clone returns a deep copy of id so callers cannot mutate cached slices or maps.
func (id AppIdentity) Clone() AppIdentity {
	events := make([]string, len(id.Events))
	copy(events, id.Events)
	perms := make(map[string]string, len(id.Permissions))
	for k, v := range id.Permissions {
		perms[k] = v
	}
	return AppIdentity{
		AppID:       id.AppID,
		Slug:        id.Slug,
		BotLogin:    id.BotLogin,
		Events:      events,
		Permissions: perms,
		FetchedAt:   id.FetchedAt,
	}
}

// ParseAppIdentity extracts an AppIdentity from a decoded GET /app response.
func ParseAppIdentity(raw map[string]interface{}, fetchedAt time.Time) AppIdentity {
	out := AppIdentity{
		Events:      []string{},
		Permissions: map[string]string{},
		FetchedAt:   fetchedAt.UTC(),
	}
	if raw == nil {
		return out
	}

	switch v := raw["id"].(type) {
	case float64:
		out.AppID = int64(v)
	case json.Number:
		if n, err := v.Int64(); err == nil {
			out.AppID = n
		}
	case int64:
		out.AppID = v
	case int:
		out.AppID = int64(v)
	}

	if slug, ok := raw["slug"].(string); ok {
		slug = strings.TrimSpace(slug)
		out.Slug = slug
		if slug != "" {
			out.BotLogin = slug + "[bot]"
		}
	}

	seenEvents := make(map[string]struct{})
	addEvent := func(ev string) {
		ev = strings.TrimSpace(ev)
		if ev == "" {
			return
		}
		if _, dup := seenEvents[ev]; dup {
			return
		}
		seenEvents[ev] = struct{}{}
		out.Events = append(out.Events, ev)
	}
	switch evs := raw["events"].(type) {
	case []interface{}:
		for _, item := range evs {
			if s, ok := item.(string); ok {
				addEvent(s)
			}
		}
	case []string:
		for _, s := range evs {
			addEvent(s)
		}
	}
	sort.Strings(out.Events)

	switch perms := raw["permissions"].(type) {
	case map[string]interface{}:
		for k, v := range perms {
			if vs, ok := v.(string); ok {
				out.Permissions[k] = vs
			}
		}
	case map[string]string:
		for k, v := range perms {
			out.Permissions[k] = v
		}
	}

	return out
}
