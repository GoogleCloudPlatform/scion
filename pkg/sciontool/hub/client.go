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
	"fmt"
	"io"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"time"

	state "github.com/GoogleCloudPlatform/scion/pkg/agent/state"
)

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
}

// NewClient creates a new Hub client from environment variables.
// Reads SCION_HUB_ENDPOINT first, falling back to SCION_HUB_URL for legacy compat.
// The token is read from the canonical token file (~/.scion/scion-token), falling
// back to the SCION_AUTH_TOKEN env var for bootstrap (before init has run).
// Returns nil if the required environment variables are not set.
func NewClient() *Client {
	hubURL := os.Getenv(EnvHubEndpoint)
	if hubURL == "" {
		hubURL = os.Getenv(EnvHubURL)
	}
	agentID := os.Getenv(EnvAgentID)

	// Prefer the canonical token file; fall back to env var for bootstrap.
	token := ReadTokenFile()
	if token == "" {
		token = os.Getenv(EnvHubToken)
	}

	if hubURL == "" || token == "" || agentID == "" {
		return nil
	}

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

// RefreshTokenResponse is the response from the token refresh endpoint.
type RefreshTokenResponse struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
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

	return result.Token, expiresAt, nil
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
}

// DefaultTokenRefreshTimeout is the default timeout for token refresh requests.
const DefaultTokenRefreshTimeout = 30 * time.Second

// StartTokenRefresh starts a background goroutine that refreshes the agent token
// before it expires. After a successful refresh, the next refresh is scheduled
// based on the new token's expiry (2 hours before expiry for a 10-hour token).
// Returns a channel that will be closed when the refresh loop exits.
func (c *Client) StartTokenRefresh(ctx context.Context, config *TokenRefreshConfig) <-chan struct{} {
	done := make(chan struct{})

	timeout := DefaultTokenRefreshTimeout
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
				// Refresh time has already passed; try immediately
				delay = 0
			}

			var timer *time.Timer
			if delay > 0 {
				timer = time.NewTimer(delay)
			} else {
				timer = time.NewTimer(0) // fire immediately
			}

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

				// If the token has already expired, auth is terminally lost
				if time.Now().After(refreshAt.Add(2 * time.Hour)) {
					if config != nil && config.OnAuthLost != nil {
						config.OnAuthLost()
					}
					return
				}

				// Retry in 30 seconds
				refreshAt = time.Now().Add(30 * time.Second)
				continue
			}

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

			// Schedule next refresh: 2 hours before new expiry
			refreshAt = newExpiry.Add(-2 * time.Hour)
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

// resolveTokenHome returns the home directory to use for the token file.
// Inside agent containers, sciontool init runs as root (HOME=/root) while
// child processes run as the scion user (HOME=/home/scion). Both must
// resolve to the same token file path — the scion user's home.
func resolveTokenHome() string {
	// Prefer the scion user's home when it exists (inside containers).
	if u, err := user.Lookup("scion"); err == nil && u.HomeDir != "" {
		return u.HomeDir
	}
	home := os.Getenv("HOME")
	if home == "" {
		home = "/home/scion"
	}
	return home
}

// SetTokenHome overrides the token home directory for testing.
// Returns a cleanup function that restores the original resolver.
func SetTokenHome(dir string) func() {
	orig := tokenHomeResolver
	tokenHomeResolver = func() string { return dir }
	return func() { tokenHomeResolver = orig }
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
	Recipient   string `json:"recipient,omitempty"`
	RecipientID string `json:"recipient_id,omitempty"`
	Msg         string `json:"msg"`
	Type        string `json:"type,omitempty"`
	Urgent      bool   `json:"urgent,omitempty"`
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
