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

// Package hub provides a client for sciontool to communicate with the Scion Hub.
// It uses the SCION_AUTH_TOKEN environment variable for authentication.
package hub

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	state "github.com/GoogleCloudPlatform/scion/pkg/agent/state"
)

// ErrTokenRefreshUnauthorized indicates the hub rejected the token refresh
// request because the presented token is no longer accepted (HTTP 401/403).
// This typically happens after a hub signing-key rotation invalidates all
// previously-issued agent JWTs. It is terminal for the current token: retrying
// with the same token can never succeed, so recovery requires a fresh token
// injected out-of-band (e.g. via the broker reset-auth path / SIGUSR2).
var ErrTokenRefreshUnauthorized = errors.New("token refresh unauthorized")

const (
	// TokenFile is the canonical token file name. The SCION_AUTH_TOKEN env var
	// bootstraps the initial value into the container; sciontool init writes it
	// here immediately and all consumers read from this file. Token refreshes
	// update the same file.
	TokenFile = "scion-token"
)

const (
	// EnvHubEndpoint is the preferred environment variable for the Hub endpoint.
	EnvHubEndpoint = "SCION_HUB_ENDPOINT"
	// EnvHubURL is the legacy environment variable for the Hub URL.
	EnvHubURL = "SCION_HUB_URL"
	// EnvHubToken is the environment variable for Hub authentication.
	// Generic agent-to-hub auth token (JWT or dev token).
	EnvHubToken = "SCION_AUTH_TOKEN"
	// EnvAgentID is the environment variable for the agent ID.
	EnvAgentID = "SCION_AGENT_ID"
	// EnvAgentMode is the environment variable for the agent mode.
	EnvAgentMode = "SCION_AGENT_MODE"

	// AgentModeHosted indicates the agent is running in hosted mode.
	AgentModeHosted = "hosted"
)

// Mode represents the operating mode of the sciontool within a container.
type Mode int

const (
	// ModeLocal indicates no hub is configured (SCION_HUB_ENDPOINT not set).
	ModeLocal Mode = iota
	// ModeHubConnected indicates a hub is configured but the agent is not in hosted mode.
	ModeHubConnected
	// ModeHosted indicates a hub is configured and SCION_AGENT_MODE=hosted.
	ModeHosted
)

// String returns a human-readable label for the mode.
func (m Mode) String() string {
	switch m {
	case ModeHubConnected:
		return "hub-connected"
	case ModeHosted:
		return "hosted"
	default:
		return "local"
	}
}

// OperatingMode returns the current operating mode based on environment variables.
// It consolidates the mode detection logic from IsConfigured() and IsHostedMode().
func OperatingMode() Mode {
	hubURL := os.Getenv(EnvHubEndpoint)
	if hubURL == "" {
		hubURL = os.Getenv(EnvHubURL)
	}
	if hubURL == "" {
		return ModeLocal
	}
	if os.Getenv(EnvAgentMode) == AgentModeHosted {
		return ModeHosted
	}
	return ModeHubConnected
}

const (

	// DefaultTimeout is the default HTTP request timeout.
	DefaultTimeout = 30 * time.Second

	// DefaultMaxRetries is the default number of retry attempts for transient failures.
	DefaultMaxRetries = 3
	// DefaultRetryBaseDelay is the base delay for exponential backoff.
	DefaultRetryBaseDelay = 500 * time.Millisecond
	// DefaultRetryMaxDelay is the maximum delay between retries.
	DefaultRetryMaxDelay = 5 * time.Second
)

// StatusUpdate represents a status update request.
// Fields:
// - Phase: Infrastructure lifecycle phase (canonical).
// - Activity: What the agent is doing (canonical).
// - ToolName: Tool name when activity is executing.
// - Status: Backward-compatible flat status string (computed via DisplayStatus).
// - Message: Optional message associated with the status.
// - TaskSummary: Current task description.
// - Heartbeat: If true, only updates last_seen without changing status.
type StatusUpdate struct {
	Phase       state.Phase       `json:"phase,omitempty"`
	Activity    state.Activity    `json:"activity,omitempty"`
	ToolName    string            `json:"toolName,omitempty"`
	Status      string            `json:"status,omitempty"`
	Message     string            `json:"message,omitempty"`
	TaskSummary string            `json:"taskSummary,omitempty"`
	Heartbeat   bool              `json:"heartbeat,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`

	// Limits tracking
	CurrentTurns      *int   `json:"currentTurns,omitempty"`
	CurrentModelCalls *int   `json:"currentModelCalls,omitempty"`
	StartedAt         string `json:"startedAt,omitempty"`

	// Exit tracking
	ExitCode *int `json:"exitCode,omitempty"`
}

// Client is a Hub API client for sciontool.
type Client struct {
	hubURL         string
	token          string
	tokenMu        sync.RWMutex
	agentID        string
	client         *http.Client
	maxRetries     int
	retryBaseDelay time.Duration
	retryMaxDelay  time.Duration
	oidcSource     oidcTokenSource // transport-layer OIDC token source (nil = disabled)
}

// NewClient creates a new Hub client from environment variables.
// Reads SCION_HUB_ENDPOINT first, falling back to SCION_HUB_URL for legacy compat.
// The token is read from the canonical token file (~/.scion/scion-token), falling
// back to the SCION_AUTH_TOKEN env var for bootstrap (before init has run).
// Returns nil if the required environment variables are not set.
//
// Defense-in-depth: when running under `go test`, refuses to create a client
// that would talk to a non-localhost hub. Tests that need a hub client must
// scrub SCION_* env vars and point at an httptest server (see scrubHubEnv
// in hub_test.go). Without this guard, a test that forgets env sandboxing
// leaks real status updates to the hub under the container's agent identity.
func NewClient() *Client {
	hubURL := os.Getenv(EnvHubEndpoint)
	if hubURL == "" {
		hubURL = os.Getenv(EnvHubURL)
	}
	agentID := os.Getenv(EnvAgentID)

	if testing.Testing() && !hubTestSandboxed && hubURL != "" && !isLocalhostURL(hubURL) {
		return nil
	}

	// Prefer the canonical token file; fall back to env var for bootstrap.
	token := ReadTokenFile()
	if token == "" {
		token = os.Getenv(EnvHubToken)
	}

	if hubURL == "" || token == "" || agentID == "" {
		return nil
	}

	c := &Client{
		hubURL:         hubURL,
		token:          token,
		agentID:        agentID,
		maxRetries:     DefaultMaxRetries,
		retryBaseDelay: DefaultRetryBaseDelay,
		retryMaxDelay:  DefaultRetryMaxDelay,
		client: &http.Client{
			Timeout: DefaultTimeout,
		},
	}
	c.configureOIDCTransport()
	return c
}

// NewClientWithConfig creates a new Hub client with explicit configuration.
func NewClientWithConfig(hubURL, token, agentID string) *Client {
	return &Client{
		hubURL:         hubURL,
		token:          token,
		agentID:        agentID,
		maxRetries:     DefaultMaxRetries,
		retryBaseDelay: DefaultRetryBaseDelay,
		retryMaxDelay:  DefaultRetryMaxDelay,
		client: &http.Client{
			Timeout: DefaultTimeout,
		},
	}
}

// IsConfigured returns true if the client is properly configured.
// Requires hubURL, token, and agentID to all be set.
func (c *Client) IsConfigured() bool {
	if c == nil {
		return false
	}
	c.tokenMu.RLock()
	token := c.token
	c.tokenMu.RUnlock()
	return c.hubURL != "" && token != "" && c.agentID != ""
}

// IsHostedMode returns true if the agent is running in hosted mode.
func IsHostedMode() bool {
	return os.Getenv(EnvAgentMode) == AgentModeHosted
}

// GetAgentID returns the agent ID from environment.
func GetAgentID() string {
	return os.Getenv(EnvAgentID)
}

// UpdateStatus sends a status update to the Hub with automatic retry on transient failures.
func (c *Client) UpdateStatus(ctx context.Context, status StatusUpdate) error {
	if !c.IsConfigured() {
		return fmt.Errorf("hub client not configured")
	}

	endpoint := fmt.Sprintf("%s/api/v1/agents/%s/status", strings.TrimSuffix(c.hubURL, "/"), c.agentID)

	body, err := json.Marshal(status)
	if err != nil {
		return fmt.Errorf("failed to marshal status: %w", err)
	}

	// Read token under lock to avoid data race with concurrent RefreshToken calls.
	c.tokenMu.RLock()
	currentToken := c.token
	c.tokenMu.RUnlock()

	var lastErr error
	attempts := c.maxRetries + 1
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			// Calculate exponential backoff delay
			delay := c.calculateBackoff(attempt)
			select {
			case <-ctx.Done():
				return fmt.Errorf("context cancelled during retry: %w", ctx.Err())
			case <-time.After(delay):
				// Continue with retry
			}
		}

		// Create a fresh request for each attempt (body reader needs to be recreated)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("failed to create request: %w", err)
		}

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Scion-Agent-Token", currentToken)

		resp, err := c.client.Do(req)
		if err != nil {
			// Check if context was cancelled - don't retry
			if ctx.Err() != nil {
				return fmt.Errorf("request failed (context cancelled): %w", ctx.Err())
			}
			// Network error - retry
			lastErr = fmt.Errorf("failed to send request: %w", err)
			continue
		}

		// Read response body
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		// Success
		if resp.StatusCode < 400 {
			return nil
		}

		// 4xx errors are client errors - don't retry
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return fmt.Errorf("hub returned error %d: %s", resp.StatusCode, string(respBody))
		}

		// 5xx errors are server errors - retry
		lastErr = fmt.Errorf("hub returned error %d: %s", resp.StatusCode, string(respBody))
	}

	return fmt.Errorf("request failed after %d attempts: %w", attempts, lastErr)
}

// calculateBackoff returns the delay for a retry attempt using exponential backoff.
func (c *Client) calculateBackoff(attempt int) time.Duration {
	// Exponential backoff: baseDelay * 2^(attempt-1)
	delay := c.retryBaseDelay
	for i := 1; i < attempt; i++ {
		delay *= 2
		if delay > c.retryMaxDelay {
			delay = c.retryMaxDelay
			break
		}
	}
	return delay
}

// Heartbeat sends a heartbeat to the Hub.
// Note: Heartbeat only updates last_seen timestamp, it does not change the agent's status.
// This allows the actual status (working, busy, etc.) to be preserved between heartbeats.
func (c *Client) Heartbeat(ctx context.Context) error {
	return c.UpdateStatus(ctx, StatusUpdate{
		Heartbeat: true,
	})
}

// ReportState sends a structured phase/activity update to the Hub.
// The backward-compatible Status field is computed automatically via DisplayStatus().
func (c *Client) ReportState(ctx context.Context, phase state.Phase, activity state.Activity, message string) error {
	s := state.AgentState{Phase: phase, Activity: activity}
	return c.UpdateStatus(ctx, StatusUpdate{
		Phase:    phase,
		Activity: activity,
		Status:   s.DisplayStatus(),
		Message:  message,
	})
}

// RefreshTokenEntry represents a single token in the generalized refresh response.
// Mirrors the hub's RefreshTokenEntry type.
type RefreshTokenEntry struct {
	Layer     string `json:"layer"`              // "app" | "transport"
	Type      string `json:"type"`               // "scion_access" | "scion_refresh" | "google_oidc"
	Value     string `json:"value"`              // the token value
	ExpiresIn int    `json:"expiresIn"`          // seconds until expiry
	Audience  string `json:"audience,omitempty"` // only for transport tokens
}

// RefreshTokenResponse is the response from the token refresh endpoint.
// Includes both legacy single-token fields (backward compat) and the
// generalized tokens[] array.
type RefreshTokenResponse struct {
	Token     string              `json:"token"`
	ExpiresAt string              `json:"expires_at"`
	Tokens    []RefreshTokenEntry `json:"tokens,omitempty"`
}

// RefreshToken calls the Hub to refresh the agent's authentication token.
// On success, the client's token is updated in-place and persisted to the
// refreshed token file so that child processes (hooks, status commands) can
// pick up the new token.
func (c *Client) RefreshToken(ctx context.Context) (string, time.Time, error) {
	if !c.IsConfigured() {
		return "", time.Time{}, fmt.Errorf("hub client not configured")
	}

	endpoint := fmt.Sprintf("%s/api/v1/agents/%s/token/refresh",
		strings.TrimSuffix(c.hubURL, "/"), c.agentID)

	// Read current token under lock
	c.tokenMu.RLock()
	currentToken := c.token
	c.tokenMu.RUnlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("X-Scion-Agent-Token", currentToken)

	resp, err := c.client.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("token refresh request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		// 401/403 mean the presented token is rejected (e.g. after a hub
		// signing-key rotation). Tag these so the refresh loop can distinguish a
		// terminal auth failure from a transient (network/5xx) one. The literal
		// "token refresh failed with status %d" wording is preserved for the
		// non-auth path because existing log-based tooling matches on it.
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return "", time.Time{}, fmt.Errorf("%w: token refresh failed with status %d: %s",
				ErrTokenRefreshUnauthorized, resp.StatusCode, string(respBody))
		}
		return "", time.Time{}, fmt.Errorf("token refresh failed with status %d: %s",
			resp.StatusCode, string(respBody))
	}

	var result RefreshTokenResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", time.Time{}, fmt.Errorf("failed to parse refresh response: %w", err)
	}

	expiresAt, err := time.Parse(time.RFC3339, result.ExpiresAt)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("failed to parse expiry time: %w", err)
	}

	// Update the client's token under write lock
	c.tokenMu.Lock()
	c.token = result.Token
	c.tokenMu.Unlock()

	// Persist the new token to a file so child processes can read it.
	// Errors are non-fatal — the in-memory token is already updated.
	if err := WriteTokenFile(result.Token); err != nil {
		// Log will be handled by caller; we don't import log here
		_ = err
	}

	// Process the generalized tokens[] array if present.
	// Apply each entry to the appropriate subsystem by layer/type.
	if len(result.Tokens) > 0 {
		c.applyRefreshTokens(result.Tokens)
	}

	return result.Token, expiresAt, nil
}

// applyRefreshTokens processes the tokens[] array from a refresh response,
// applying each entry to the appropriate subsystem.
func (c *Client) applyRefreshTokens(tokens []RefreshTokenEntry) {
	for _, entry := range tokens {
		switch {
		case entry.Layer == "transport" && entry.Type == "google_oidc":
			// Update the OIDC transport's token source
			if c.oidcSource != nil {
				entryExpiry := time.Now().Add(time.Duration(entry.ExpiresIn) * time.Second)
				c.oidcSource.setToken(entry.Value, entryExpiry)
			}
			// app/scion_access is already handled via the legacy token field above
		}
	}
}

// adjustRefreshForTransportTokens checks if the OIDC source has a shorter
// expiry than the proposed refresh time and returns the earlier of the two.
// Transport tokens (~1h) use a 5-minute refresh margin vs the app token's
// 2-hour margin.
func (c *Client) adjustRefreshForTransportTokens(proposed time.Time) time.Time {
	if c.oidcSource == nil {
		return proposed
	}

	// Read the transport token expiry from the source
	switch src := c.oidcSource.(type) {
	case *injectedTokenSource:
		src.mu.RLock()
		expiry := src.expiresAt
		src.mu.RUnlock()
		if !expiry.IsZero() {
			transportRefresh := expiry.Add(-oidcRefreshMargin)
			if transportRefresh.Before(proposed) {
				return transportRefresh
			}
		}
	case *metadataTokenSource:
		// Metadata source self-refreshes; no need to drive refresh from here.
	}
	return proposed
}

// TokenRefreshConfig configures the token refresh loop.
type TokenRefreshConfig struct {
	// RefreshAt is the time at which the token should be refreshed.
	RefreshAt time.Time
	// Timeout is the context timeout for each refresh request.
	Timeout time.Duration
	// ChownUID and ChownGID set ownership on the token file after writing.
	// When ChownUID > 0, the file is chowned so non-root users (e.g. the
	// scion container user) can read it. Zero values skip chown.
	ChownUID int
	ChownGID int
	// OnRefreshed is called when the token is successfully refreshed.
	OnRefreshed func(newExpiry time.Time)
	// OnError is called when a refresh attempt fails.
	OnError func(error)
	// OnAuthLost is called when auth is terminally lost (token expired, cannot refresh).
	OnAuthLost func()
	// RetryBaseDelay overrides the initial backoff between failed refresh
	// attempts. Zero uses tokenRefreshRetryBaseDelay.
	RetryBaseDelay time.Duration
	// RetryMaxDelay overrides the cap on backoff between failed refresh attempts.
	// Zero uses tokenRefreshRetryMaxDelay.
	RetryMaxDelay time.Duration
}

// DefaultTokenRefreshTimeout is the default timeout for token refresh requests.
const DefaultTokenRefreshTimeout = 30 * time.Second

const (
	// tokenRefreshRetryBaseDelay is the initial delay before retrying a failed
	// token refresh.
	tokenRefreshRetryBaseDelay = 30 * time.Second
	// tokenRefreshRetryMaxDelay caps the backoff between failed refresh attempts.
	// A persistently failing refresh (e.g. after a hub signing-key rotation that
	// invalidates the current token) must not hot-loop, but should still retry
	// often enough to recover promptly once the hub is healthy again or an
	// out-of-band reset-auth injects a fresh token.
	tokenRefreshRetryMaxDelay = 5 * time.Minute
)

// tokenRefreshBackoff returns the delay before the next refresh retry after the
// given number of consecutive failures, using exponential backoff (starting at
// base, doubling each attempt) capped at max.
func tokenRefreshBackoff(consecutiveFailures int, base, max time.Duration) time.Duration {
	if consecutiveFailures < 1 {
		consecutiveFailures = 1
	}
	delay := base
	for i := 1; i < consecutiveFailures; i++ {
		delay *= 2
		if delay >= max {
			return max
		}
	}
	if delay > max {
		delay = max
	}
	return delay
}

// StartTokenRefresh starts a background goroutine that refreshes the agent token
// before it expires. After a successful refresh, the next refresh is scheduled
// based on the new token's expiry (2 hours before expiry for a 10-hour token).
//
// On failure the loop retries with exponential backoff (capped at
// tokenRefreshRetryMaxDelay) instead of exiting, so the agent recovers
// automatically once the hub is healthy again or a fresh token is injected
// out-of-band (e.g. via reset-auth). When the current token has actually expired
// and refresh still fails, OnAuthLost is invoked once for observability; the loop
// keeps retrying so recovery remains possible. The loop only exits when ctx is
// cancelled. Returns a channel that is closed when the loop exits.
func (c *Client) StartTokenRefresh(ctx context.Context, config *TokenRefreshConfig) <-chan struct{} {
	done := make(chan struct{})

	timeout := DefaultTokenRefreshTimeout
	if config != nil && config.Timeout > 0 {
		timeout = config.Timeout
	}

	retryBase := tokenRefreshRetryBaseDelay
	if config != nil && config.RetryBaseDelay > 0 {
		retryBase = config.RetryBaseDelay
	}
	retryMax := tokenRefreshRetryMaxDelay
	if config != nil && config.RetryMaxDelay > 0 {
		retryMax = config.RetryMaxDelay
	}
	if retryMax < retryBase {
		retryMax = retryBase
	}

	go func() {
		defer close(done)

		// tokenExpiry tracks the actual expiry of the token currently held by the
		// client. refreshAt (the scheduled wake time) is rewritten on every retry,
		// so it cannot be used to decide when auth is terminally lost — we must
		// compare against the real expiry instead. Seed it from the current token,
		// falling back to the configured refresh time plus the standard 2h
		// pre-expiry margin when the token is not a parseable JWT.
		tokenExpiry := config.RefreshAt.Add(2 * time.Hour)
		if exp, parseErr := ParseTokenExpiry(c.GetToken()); parseErr == nil {
			tokenExpiry = exp
		}

		refreshAt := config.RefreshAt
		consecutiveFailures := 0
		authLostNotified := false

		for {
			delay := time.Until(refreshAt)
			if delay < 0 {
				delay = 0
			}
			timer := time.NewTimer(delay)

			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}

			refreshCtx, cancel := context.WithTimeout(ctx, timeout)
			_, newExpiry, err := c.RefreshToken(refreshCtx)
			cancel()

			if err != nil {
				if config != nil && config.OnError != nil {
					config.OnError(err)
				}

				// Once the current token has actually expired and refresh still
				// fails, auth is lost. Surface it once (for observability and to
				// trigger out-of-band recovery such as reset-auth) — but keep
				// retrying with capped backoff rather than exiting, so the agent
				// self-heals if the hub recovers (e.g. its signing key is restored)
				// or a fresh token is injected. The previous implementation reset
				// the expiry estimate on every retry, so OnAuthLost never fired and
				// the loop hot-looped every 30s indefinitely.
				if !authLostNotified && !time.Now().Before(tokenExpiry) {
					authLostNotified = true
					if config != nil && config.OnAuthLost != nil {
						config.OnAuthLost()
					}
				}

				consecutiveFailures++
				refreshAt = time.Now().Add(tokenRefreshBackoff(consecutiveFailures, retryBase, retryMax))
				continue
			}

			// Successful refresh: reset failure tracking and clear any prior
			// auth-lost state so a later loss is reported again.
			consecutiveFailures = 0
			authLostNotified = false
			tokenExpiry = newExpiry

			// Fix ownership after atomic rewrite (init runs as root).
			if config.ChownUID > 0 {
				if chownErr := os.Chown(TokenFilePath(), config.ChownUID, config.ChownGID); chownErr != nil {
					if config.OnError != nil {
						config.OnError(fmt.Errorf("failed to chown token file: %w", chownErr))
					}
				}
			}

			if config != nil && config.OnRefreshed != nil {
				config.OnRefreshed(newExpiry)
			}

			// Schedule next refresh: 2 hours before new expiry for the app token.
			refreshAt = newExpiry.Add(-2 * time.Hour)

			// If transport tokens are present, use the shortest-lived entry
			// to drive refresh timing (transport tokens ~1h need a tighter margin).
			refreshAt = c.adjustRefreshForTransportTokens(refreshAt)

			if refreshAt.Before(time.Now()) {
				// Token duration is very short; refresh in 1 minute
				refreshAt = time.Now().Add(1 * time.Minute)
			}
		}
	}()

	return done
}

// GetToken returns the client's current auth token.
func (c *Client) GetToken() string {
	if c == nil {
		return ""
	}
	c.tokenMu.RLock()
	defer c.tokenMu.RUnlock()
	return c.token
}

// SetToken updates the client's in-memory auth token. This is used during
// auth reset to inject a freshly-issued token without restarting the client.
func (c *Client) SetToken(token string) {
	c.tokenMu.Lock()
	c.token = token
	c.tokenMu.Unlock()
}

// Environment variable and file path constants for GitHub App token refresh.
const (
	// EnvGitHubAppEnabled indicates whether GitHub App token refresh is active.
	EnvGitHubAppEnabled = "SCION_GITHUB_APP_ENABLED"
	// EnvGitHubTokenExpiry is the ISO 8601 expiry time of the initial GitHub token.
	EnvGitHubTokenExpiry = "SCION_GITHUB_TOKEN_EXPIRY"
	// EnvGitHubTokenPath is the path to the refreshable GitHub token file.
	EnvGitHubTokenPath = "SCION_GITHUB_TOKEN_PATH"
	// DefaultGitHubTokenPath is the default path for the GitHub token file.
	DefaultGitHubTokenPath = "/tmp/.github-token"
	// EnvUserGitHubToken is set to "true" when the user has explicitly
	// provided their own GITHUB_TOKEN alongside a GitHub App installation.
	// When set, the gh CLI wrapper skips token injection so the user's
	// token takes precedence.
	EnvUserGitHubToken = "SCION_USER_GITHUB_TOKEN"
)

// GitHubTokenRefreshResponse is the response from the GitHub token refresh endpoint.
type GitHubTokenRefreshResponse struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
}

// RefreshGitHubToken calls the Hub to mint a fresh GitHub App installation token.
// Returns the new token, its expiry time, and any error.
func (c *Client) RefreshGitHubToken(ctx context.Context) (string, time.Time, error) {
	if !c.IsConfigured() {
		return "", time.Time{}, fmt.Errorf("hub client not configured")
	}

	endpoint := fmt.Sprintf("%s/api/v1/agents/%s/refresh-token",
		strings.TrimSuffix(c.hubURL, "/"), c.agentID)

	// Read current Hub auth token under lock
	c.tokenMu.RLock()
	currentToken := c.token
	c.tokenMu.RUnlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("X-Scion-Agent-Token", currentToken)

	resp, err := c.client.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("GitHub token refresh request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return "", time.Time{}, fmt.Errorf("GitHub token refresh failed with status %d: %s",
			resp.StatusCode, string(respBody))
	}

	var result GitHubTokenRefreshResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", time.Time{}, fmt.Errorf("failed to parse GitHub token refresh response: %w", err)
	}

	expiresAt, err := time.Parse(time.RFC3339, result.ExpiresAt)
	if err != nil {
		// Try ISO 8601 format without timezone name
		expiresAt, err = time.Parse("2006-01-02T15:04:05Z", result.ExpiresAt)
		if err != nil {
			return "", time.Time{}, fmt.Errorf("failed to parse GitHub token expiry: %w", err)
		}
	}

	return result.Token, expiresAt, nil
}

// GitHubTokenRefreshConfig configures the GitHub token refresh loop.
type GitHubTokenRefreshConfig struct {
	// RefreshAt is the time at which the first refresh should occur.
	RefreshAt time.Time
	// TokenPath is the file path to write the refreshed token to.
	TokenPath string
	// ChownUID and ChownGID set ownership on the token file after writing.
	// When ChownUID > 0, the file is chowned so non-root users (e.g. the
	// scion container user) can read it. Zero values skip chown.
	ChownUID int
	ChownGID int
	// Timeout is the context timeout for each refresh request.
	Timeout time.Duration
	// OnRefreshed is called when the token is successfully refreshed.
	OnRefreshed func(newToken string, newExpiry time.Time)
	// OnError is called when a refresh attempt fails.
	OnError func(error)
}

// DefaultGitHubTokenRefreshTimeout is the default timeout for GitHub token refresh requests.
const DefaultGitHubTokenRefreshTimeout = 30 * time.Second

// StartGitHubTokenRefresh starts a background goroutine that proactively refreshes
// the GitHub App installation token before it expires. The fresh token is written
// to the token file at config.TokenPath so non-git consumers (gh CLI, custom scripts)
// always have a valid token. The GITHUB_TOKEN env var is also updated in-process.
// Returns a channel that is closed when the loop exits.
func (c *Client) StartGitHubTokenRefresh(ctx context.Context, config *GitHubTokenRefreshConfig) <-chan struct{} {
	done := make(chan struct{})

	timeout := DefaultGitHubTokenRefreshTimeout
	if config != nil && config.Timeout > 0 {
		timeout = config.Timeout
	}

	go func() {
		defer close(done)

		refreshAt := config.RefreshAt
		for {
			now := time.Now()
			delay := refreshAt.Sub(now)
			if delay <= 0 {
				delay = 0
			}

			timer := time.NewTimer(delay)

			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}

			refreshCtx, cancel := context.WithTimeout(ctx, timeout)
			newToken, newExpiry, err := c.RefreshGitHubToken(refreshCtx)
			cancel()

			if err != nil {
				if config != nil && config.OnError != nil {
					config.OnError(err)
				}
				// Retry in 30 seconds
				refreshAt = time.Now().Add(30 * time.Second)
				continue
			}

			// Write the fresh token and expiry to the token file
			if config.TokenPath != "" {
				if writeErr := WriteGitHubTokenFile(config.TokenPath, newToken); writeErr != nil {
					if config.OnError != nil {
						config.OnError(fmt.Errorf("failed to write GitHub token file: %w", writeErr))
					}
				} else {
					// Write the companion expiry file so the credential helper
					// (a separate process) can detect stale tokens.
					if expiryErr := WriteGitHubTokenExpiry(config.TokenPath, newExpiry); expiryErr != nil {
						if config.OnError != nil {
							config.OnError(fmt.Errorf("failed to write GitHub token expiry file: %w", expiryErr))
						}
					}
					if config.ChownUID > 0 {
						if chownErr := os.Chown(config.TokenPath, config.ChownUID, config.ChownGID); chownErr != nil {
							if config.OnError != nil {
								config.OnError(fmt.Errorf("failed to chown GitHub token file: %w", chownErr))
							}
						}
						expiryPath := GitHubTokenExpiryPath(config.TokenPath)
						if chownErr := os.Chown(expiryPath, config.ChownUID, config.ChownGID); chownErr != nil {
							if config.OnError != nil {
								config.OnError(fmt.Errorf("failed to chown GitHub token expiry file: %w", chownErr))
							}
						}
					}
				}
			}

			// Update GITHUB_TOKEN env var in-process
			os.Setenv("GITHUB_TOKEN", newToken)

			if config != nil && config.OnRefreshed != nil {
				config.OnRefreshed(newToken, newExpiry)
			}

			// Schedule next refresh: 10 minutes before expiry (tokens last 1 hour)
			refreshAt = newExpiry.Add(-10 * time.Minute)
			if refreshAt.Before(time.Now()) {
				// Token duration is very short; refresh in 1 minute
				refreshAt = time.Now().Add(1 * time.Minute)
			}
		}
	}()

	return done
}

// WriteGitHubTokenFile writes a GitHub token to the specified path atomically.
func WriteGitHubTokenFile(path, token string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("failed to create token file directory: %w", err)
	}

	// Write to temp file then rename for atomicity
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(token), 0600); err != nil {
		return fmt.Errorf("failed to write GitHub token file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("failed to rename GitHub token file: %w", err)
	}
	return nil
}

// ReadGitHubTokenFile reads a GitHub token from the specified path.
// Returns empty string if the file doesn't exist or can't be read.
func ReadGitHubTokenFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// GitHubTokenExpiryPath returns the companion expiry file path for a token file.
func GitHubTokenExpiryPath(tokenPath string) string {
	return tokenPath + ".expiry"
}

// WriteGitHubTokenExpiry writes the token expiry time to a companion file
// alongside the token file. This allows the credential helper (a separate
// process) to check whether the cached token is still valid.
func WriteGitHubTokenExpiry(tokenPath string, expiry time.Time) error {
	expiryPath := GitHubTokenExpiryPath(tokenPath)
	return os.WriteFile(expiryPath, []byte(expiry.Format(time.RFC3339)), 0600)
}

// ReadGitHubTokenExpiry reads the token expiry time from the companion expiry
// file. Returns zero time and an error if the file doesn't exist or can't be
// parsed.
func ReadGitHubTokenExpiry(tokenPath string) (time.Time, error) {
	expiryPath := GitHubTokenExpiryPath(tokenPath)
	data, err := os.ReadFile(expiryPath)
	if err != nil {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339, strings.TrimSpace(string(data)))
}

// IsGitHubTokenExpired checks whether the token at the given path has expired
// by reading the companion expiry file. Returns true if the token is expired
// or if the expiry cannot be determined (missing/corrupt expiry file).
func IsGitHubTokenExpired(tokenPath string) bool {
	expiry, err := ReadGitHubTokenExpiry(tokenPath)
	if err != nil {
		// Can't determine expiry — treat as expired to be safe
		return true
	}
	return time.Now().After(expiry)
}

// GitHubTokenPath returns the configured GitHub token file path from env,
// falling back to the default path.
func GitHubTokenPath() string {
	if p := os.Getenv(EnvGitHubTokenPath); p != "" {
		return p
	}
	return DefaultGitHubTokenPath
}

// IsGitHubAppEnabled returns true if GitHub App token refresh is active.
func IsGitHubAppEnabled() bool {
	return os.Getenv(EnvGitHubAppEnabled) == "true"
}

// ParseTokenExpiry extracts the expiry time from a JWT token without
// validating the signature. This is safe for scheduling purposes since
// the Hub will validate the token on each request.
func ParseTokenExpiry(tokenString string) (time.Time, error) {
	parts := strings.Split(tokenString, ".")
	if len(parts) != 3 {
		return time.Time{}, fmt.Errorf("invalid JWT format: expected 3 parts, got %d", len(parts))
	}

	// Decode the payload (second part)
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, fmt.Errorf("failed to decode JWT payload: %w", err)
	}

	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return time.Time{}, fmt.Errorf("failed to parse JWT claims: %w", err)
	}

	if claims.Exp == 0 {
		return time.Time{}, fmt.Errorf("token has no expiry claim")
	}

	return time.Unix(claims.Exp, 0), nil
}

// isLocalhostURL returns true if the URL points to localhost or 127.0.0.1,
// indicating a test server rather than a real hub endpoint.
func isLocalhostURL(rawURL string) bool {
	lower := strings.ToLower(rawURL)
	for _, prefix := range []string{
		"http://localhost", "https://localhost",
		"http://127.0.0.1", "https://127.0.0.1",
		"http://[::1]", "https://[::1]",
	} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

// HeartbeatConfig configures the heartbeat loop.
type HeartbeatConfig struct {
	// Interval is the time between heartbeats. Default: 30 seconds.
	Interval time.Duration
	// Timeout is the context timeout for each heartbeat request. Default: 10 seconds.
	Timeout time.Duration
	// OnError is called when a heartbeat fails (after retries). Optional.
	OnError func(error)
	// OnSuccess is called when a heartbeat succeeds. Optional.
	OnSuccess func()
}

// DefaultHeartbeatInterval is the default interval between heartbeats.
const DefaultHeartbeatInterval = 30 * time.Second

// DefaultHeartbeatTimeout is the default timeout for heartbeat requests.
const DefaultHeartbeatTimeout = 10 * time.Second

// tokenHomeResolver returns the home directory to use for the token file.
// Override in tests via SetTokenHome to use a temp directory.
var tokenHomeResolver = resolveTokenHome

var (
	resolvedTokenHome    string
	resolveTokenHomeOnce sync.Once
)

// resolveTokenHome returns the home directory to use for the token file.
// Inside agent containers, sciontool init runs as root (HOME=/root) while
// child processes run as the scion user (HOME=/home/scion). Both must
// resolve to the same token file path — the scion user's home.
// The result is cached because user.Lookup is expensive and the home
// directory does not change at runtime.
func resolveTokenHome() string {
	resolveTokenHomeOnce.Do(func() {
		if u, err := user.Lookup("scion"); err == nil && u.HomeDir != "" {
			resolvedTokenHome = u.HomeDir
			return
		}
		resolvedTokenHome = os.Getenv("HOME")
		if resolvedTokenHome == "" {
			resolvedTokenHome = "/home/scion"
		}
	})
	return resolvedTokenHome
}

// hubTestSandboxed reports whether the calling test has explicitly declared
// that it has sandboxed the hub environment (e.g. by calling scrubHubEnv and
// setting test values). NewClient refuses to connect to a non-localhost hub
// under `go test` unless this flag is set, preventing accidental leakage of
// status updates to a real hub when tests run inside an agent container.
var hubTestSandboxed bool

// SetHubTestSandboxed marks the current test as having properly sandboxed the
// hub environment. Call this in tests that deliberately set non-localhost hub
// URLs (e.g. for verifying URL preference logic). Returns a cleanup function.
func SetHubTestSandboxed() func() {
	orig := hubTestSandboxed
	hubTestSandboxed = true
	return func() { hubTestSandboxed = orig }
}

// tokenHomeOverridden reports whether SetTokenHome has installed a test
// override. WriteTokenFile refuses to write under `go test` unless this is set,
// so a test that forgets SetTokenHome can never clobber a live
// ~/.scion/scion-token (as happened when the suite was run inside an agent
// container, where resolveTokenHome finds the real scion user).
var tokenHomeOverridden bool

// SetTokenHome overrides the token home directory for testing.
// Returns a cleanup function that restores the original resolver.
func SetTokenHome(dir string) func() {
	orig := tokenHomeResolver
	origOverridden := tokenHomeOverridden
	tokenHomeResolver = func() string { return dir }
	tokenHomeOverridden = true
	return func() {
		tokenHomeResolver = orig
		tokenHomeOverridden = origOverridden
	}
}

// TokenFilePath returns the path to the canonical token file.
// In agent containers it always resolves to the scion user's home
// directory so that root (sciontool init) and scion (child processes)
// agree on the same path.
func TokenFilePath() string {
	return filepath.Join(tokenHomeResolver(), ".scion", TokenFile)
}

// WriteTokenFile writes the agent token to the canonical token file.
// Called by sciontool init to seed the initial value and by the refresh
// loop to persist updated tokens. Written atomically via temp file + rename.
func WriteTokenFile(token string) error {
	// Guardrail: under `go test`, refuse to write the real token file unless a
	// test has explicitly isolated it via SetTokenHome. resolveTokenHome
	// resolves to the live scion user's home inside agent containers, so a test
	// that forgets to isolate would silently overwrite a running agent's token
	// (seen in the wild: a refresh test persisted the literal "refreshed-token",
	// 401-ing the agent). Fail loudly instead of corrupting live state.
	if testing.Testing() && !tokenHomeOverridden {
		panic("scion/hub: WriteTokenFile called during a test without SetTokenHome(); " +
			"call SetTokenHome(t.TempDir()) so tests never overwrite the real ~/.scion/scion-token")
	}

	path := TokenFilePath()
	dir := filepath.Dir(path)

	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("failed to create token file directory: %w", err)
	}

	// Write to temp file then rename for atomicity
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(token), 0600); err != nil {
		return fmt.Errorf("failed to write token file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("failed to rename token file: %w", err)
	}
	return nil
}

// ReadTokenFile reads the agent token from the canonical token file.
// Returns empty string if the file doesn't exist or can't be read.
func ReadTokenFile() string {
	data, err := os.ReadFile(TokenFilePath())
	if err != nil {
		return ""
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return ""
	}
	return token
}

// OutboundMessage is the payload for sending an agent-to-human outbound message.
type OutboundMessage struct {
	Recipient   string            `json:"recipient,omitempty"`
	RecipientID string            `json:"recipient_id,omitempty"`
	Msg         string            `json:"msg"`
	Type        string            `json:"type,omitempty"`
	Urgent      bool              `json:"urgent,omitempty"`
	Visibility  string            `json:"visibility,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// SendOutboundMessage sends an outbound message from the agent to a human inbox.
// Posts to POST /api/v1/agents/{agentID}/outbound-message using the agent token.
// No retries — this is a best-effort fire-and-forget call.
func (c *Client) SendOutboundMessage(ctx context.Context, msg OutboundMessage) error {
	if !c.IsConfigured() {
		return fmt.Errorf("hub client not configured")
	}

	endpoint := fmt.Sprintf("%s/api/v1/agents/%s/outbound-message",
		strings.TrimSuffix(c.hubURL, "/"), c.agentID)

	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal outbound message: %w", err)
	}

	c.tokenMu.RLock()
	currentToken := c.token
	c.tokenMu.RUnlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Scion-Agent-Token", currentToken)

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send outbound message: %w", err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("hub returned error %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// StartHeartbeat starts a background goroutine that periodically sends heartbeats to the Hub.
// The heartbeat loop runs until the context is cancelled.
// Returns a channel that will be closed when the heartbeat loop exits.
func (c *Client) StartHeartbeat(ctx context.Context, config *HeartbeatConfig) <-chan struct{} {
	done := make(chan struct{})

	// Apply defaults
	interval := DefaultHeartbeatInterval
	timeout := DefaultHeartbeatTimeout
	var onError func(error)
	var onSuccess func()

	if config != nil {
		if config.Interval > 0 {
			interval = config.Interval
		}
		if config.Timeout > 0 {
			timeout = config.Timeout
		}
		onError = config.OnError
		onSuccess = config.OnSuccess
	}

	go func() {
		defer close(done)

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				heartbeatCtx, cancel := context.WithTimeout(ctx, timeout)
				if err := c.Heartbeat(heartbeatCtx); err != nil {
					if onError != nil {
						onError(err)
					}
				} else if onSuccess != nil {
					onSuccess()
				}
				cancel()
			}
		}
	}()

	return done
}
