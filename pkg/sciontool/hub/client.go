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
// It uses the SCION_SERVER_AUTH_DEV_TOKEN environment variable for authentication.
package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	// EnvHubEndpoint is the preferred environment variable for the Hub endpoint.
	EnvHubEndpoint = "SCION_HUB_ENDPOINT"
	// EnvHubURL is the legacy environment variable for the Hub URL.
	EnvHubURL = "SCION_HUB_URL"
	// EnvHubToken is the environment variable for Hub authentication.
	// Maps to the non-deprecated server.auth.dev_token setting.
	EnvHubToken = "SCION_SERVER_AUTH_DEV_TOKEN"
	// EnvAgentID is the environment variable for the agent ID.
	EnvAgentID = "SCION_AGENT_ID"
	// EnvAgentMode is the environment variable for the agent mode.
	EnvAgentMode = "SCION_AGENT_MODE"

	// AgentModeHosted indicates the agent is running in hosted mode.
	AgentModeHosted = "hosted"

	// DefaultTimeout is the default HTTP request timeout.
	DefaultTimeout = 30 * time.Second

	// DefaultMaxRetries is the default number of retry attempts for transient failures.
	DefaultMaxRetries = 3
	// DefaultRetryBaseDelay is the base delay for exponential backoff.
	DefaultRetryBaseDelay = 500 * time.Millisecond
	// DefaultRetryMaxDelay is the maximum delay between retries.
	DefaultRetryMaxDelay = 5 * time.Second
)

// AgentStatus represents the status of an agent.
type AgentStatus string

const (
	StatusPending      AgentStatus = "pending"
	StatusProvisioning AgentStatus = "provisioning"
	StatusStarting     AgentStatus = "starting"
	StatusRunning      AgentStatus = "running"
	StatusBusy         AgentStatus = "busy"
	StatusIdle         AgentStatus = "idle"
	StatusStopping     AgentStatus = "stopping"
	StatusStopped      AgentStatus = "stopped"
	StatusError        AgentStatus = "error"
	StatusShuttingDown AgentStatus = "shutting_down"
	StatusCloning      AgentStatus = "cloning"
)

// StatusUpdate represents a status update request.
// Fields:
// - Status: Lifecycle status (running, stopped, error). Only set for lifecycle transitions.
// - SessionStatus: Agent activity status (idle, busy). Used for agent session state.
// - Message: Optional message associated with the status.
// - TaskSummary: Current task description.
// - Heartbeat: If true, only updates last_seen without changing status.
type StatusUpdate struct {
	Status        AgentStatus       `json:"status,omitempty"`
	SessionStatus AgentStatus       `json:"sessionStatus,omitempty"`
	Message       string            `json:"message,omitempty"`
	TaskSummary   string            `json:"taskSummary,omitempty"`
	Heartbeat     bool              `json:"heartbeat,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
}

// Client is a Hub API client for sciontool.
type Client struct {
	hubURL         string
	token          string
	agentID        string
	client         *http.Client
	maxRetries     int
	retryBaseDelay time.Duration
	retryMaxDelay  time.Duration
}

// NewClient creates a new Hub client from environment variables.
// Reads SCION_HUB_ENDPOINT first, falling back to SCION_HUB_URL for legacy compat.
// Returns nil if the required environment variables are not set.
func NewClient() *Client {
	hubURL := os.Getenv(EnvHubEndpoint)
	if hubURL == "" {
		hubURL = os.Getenv(EnvHubURL)
	}
	token := os.Getenv(EnvHubToken)
	agentID := os.Getenv(EnvAgentID)

	if hubURL == "" || token == "" {
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
func (c *Client) IsConfigured() bool {
	return c != nil && c.hubURL != "" && c.token != ""
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

	if c.agentID == "" {
		return fmt.Errorf("agent ID not set")
	}

	endpoint := fmt.Sprintf("%s/api/v1/agents/%s/status", strings.TrimSuffix(c.hubURL, "/"), c.agentID)

	body, err := json.Marshal(status)
	if err != nil {
		return fmt.Errorf("failed to marshal status: %w", err)
	}

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
		req.Header.Set("X-Scion-Agent-Token", c.token)

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
// This allows the actual status (idle, busy, etc.) to be preserved between heartbeats.
func (c *Client) Heartbeat(ctx context.Context) error {
	return c.UpdateStatus(ctx, StatusUpdate{
		Heartbeat: true,
	})
}

// ReportRunning reports that the agent is running.
func (c *Client) ReportRunning(ctx context.Context, message string) error {
	return c.UpdateStatus(ctx, StatusUpdate{
		Status:  StatusRunning,
		Message: message,
	})
}

// ReportCloning reports that the agent is cloning a git repository.
func (c *Client) ReportCloning(ctx context.Context, message string, metadata map[string]string) error {
	return c.UpdateStatus(ctx, StatusUpdate{
		Status:   StatusCloning,
		Message:  message,
		Metadata: metadata,
	})
}

// ReportBusy reports that the agent is busy with a task.
// This updates the session status (not lifecycle status).
func (c *Client) ReportBusy(ctx context.Context, message string) error {
	return c.UpdateStatus(ctx, StatusUpdate{
		SessionStatus: StatusBusy,
		Message:       message,
	})
}

// ReportIdle reports that the agent is idle.
// This updates the session status (not lifecycle status).
func (c *Client) ReportIdle(ctx context.Context, message string) error {
	return c.UpdateStatus(ctx, StatusUpdate{
		SessionStatus: StatusIdle,
		Message:       message,
	})
}

// ReportError reports an error status.
func (c *Client) ReportError(ctx context.Context, message string) error {
	return c.UpdateStatus(ctx, StatusUpdate{
		Status:  StatusError,
		Message: message,
	})
}

// ReportShuttingDown reports that the agent is shutting down.
func (c *Client) ReportShuttingDown(ctx context.Context, message string) error {
	return c.UpdateStatus(ctx, StatusUpdate{
		Status:  StatusShuttingDown,
		Message: message,
	})
}

// ReportStopped reports that the agent has stopped.
func (c *Client) ReportStopped(ctx context.Context, message string) error {
	return c.UpdateStatus(ctx, StatusUpdate{
		Status:  StatusStopped,
		Message: message,
	})
}

// ReportTaskCompleted reports that a task has been completed.
// This sets the session status to idle and records the task summary.
func (c *Client) ReportTaskCompleted(ctx context.Context, taskSummary string) error {
	return c.UpdateStatus(ctx, StatusUpdate{
		SessionStatus: StatusIdle,
		TaskSummary:   taskSummary,
	})
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
