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
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewClient_FromEnvironment(t *testing.T) {
	// Save and restore env vars
	origEndpoint := os.Getenv(EnvHubEndpoint)
	origURL := os.Getenv(EnvHubURL)
	origToken := os.Getenv(EnvHubToken)
	origAgentID := os.Getenv(EnvAgentID)
	defer func() {
		os.Setenv(EnvHubEndpoint, origEndpoint)
		os.Setenv(EnvHubURL, origURL)
		os.Setenv(EnvHubToken, origToken)
		os.Setenv(EnvAgentID, origAgentID)
	}()

	t.Run("missing env vars returns nil", func(t *testing.T) {
		os.Unsetenv(EnvHubEndpoint)
		os.Unsetenv(EnvHubURL)
		os.Unsetenv(EnvHubToken)
		os.Unsetenv(EnvAgentID)

		client := NewClient()
		assert.Nil(t, client)
	})

	t.Run("missing token returns nil", func(t *testing.T) {
		os.Unsetenv(EnvHubEndpoint)
		os.Setenv(EnvHubURL, "http://hub.example.com")
		os.Unsetenv(EnvHubToken)
		os.Unsetenv(EnvAgentID)

		client := NewClient()
		assert.Nil(t, client)
	})

	t.Run("with all env vars returns client", func(t *testing.T) {
		os.Unsetenv(EnvHubEndpoint)
		os.Setenv(EnvHubURL, "http://hub.example.com")
		os.Setenv(EnvHubToken, "test-token")
		os.Setenv(EnvAgentID, "agent-123")

		client := NewClient()
		require.NotNil(t, client)
		assert.True(t, client.IsConfigured())
	})

	t.Run("prefers SCION_HUB_ENDPOINT over SCION_HUB_URL", func(t *testing.T) {
		os.Setenv(EnvHubEndpoint, "http://endpoint.example.com")
		os.Setenv(EnvHubURL, "http://url.example.com")
		os.Setenv(EnvHubToken, "test-token")
		os.Setenv(EnvAgentID, "agent-123")

		client := NewClient()
		require.NotNil(t, client)
		assert.Equal(t, "http://endpoint.example.com", client.hubURL)
	})

	t.Run("falls back to SCION_HUB_URL when SCION_HUB_ENDPOINT not set", func(t *testing.T) {
		os.Unsetenv(EnvHubEndpoint)
		os.Setenv(EnvHubURL, "http://url.example.com")
		os.Setenv(EnvHubToken, "test-token")
		os.Setenv(EnvAgentID, "agent-123")

		client := NewClient()
		require.NotNil(t, client)
		assert.Equal(t, "http://url.example.com", client.hubURL)
	})
}

func TestNewClientWithConfig(t *testing.T) {
	client := NewClientWithConfig("http://hub.example.com", "test-token", "agent-123")

	require.NotNil(t, client)
	assert.True(t, client.IsConfigured())
}

func TestClient_UpdateStatus(t *testing.T) {
	// Create a test server
	var receivedStatus StatusUpdate
	var receivedToken string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check request
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/agents/agent-123/status", r.URL.Path)
		receivedToken = r.Header.Get("X-Scion-Agent-Token")

		// Parse body
		err := json.NewDecoder(r.Body).Decode(&receivedStatus)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := NewClientWithConfig(server.URL, "test-token", "agent-123")

	err := client.UpdateStatus(context.Background(), StatusUpdate{
		Status:  StatusRunning,
		Message: "test message",
	})

	require.NoError(t, err)
	assert.Equal(t, "test-token", receivedToken)
	assert.Equal(t, StatusRunning, receivedStatus.Status)
	assert.Equal(t, "test message", receivedStatus.Message)
}

func TestClient_UpdateStatus_Errors(t *testing.T) {
	t.Run("not configured", func(t *testing.T) {
		client := &Client{}
		err := client.UpdateStatus(context.Background(), StatusUpdate{})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "not configured")
	})

	t.Run("no agent ID", func(t *testing.T) {
		client := NewClientWithConfig("http://hub.example.com", "test-token", "")
		err := client.UpdateStatus(context.Background(), StatusUpdate{})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "agent ID not set")
	})

	t.Run("server error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("internal error"))
		}))
		defer server.Close()

		client := NewClientWithConfig(server.URL, "test-token", "agent-123")
		err := client.UpdateStatus(context.Background(), StatusUpdate{})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "500")
	})
}

func TestClient_ConvenienceMethods(t *testing.T) {
	var lastStatus StatusUpdate

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&lastStatus)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := NewClientWithConfig(server.URL, "test-token", "agent-123")
	ctx := context.Background()

	t.Run("Heartbeat", func(t *testing.T) {
		err := client.Heartbeat(ctx)
		require.NoError(t, err)
		// Heartbeat should not set a status (empty string), only the heartbeat flag
		// This prevents heartbeats from overwriting the actual agent status
		assert.Equal(t, AgentStatus(""), lastStatus.Status)
		assert.True(t, lastStatus.Heartbeat)
	})

	t.Run("ReportRunning", func(t *testing.T) {
		err := client.ReportRunning(ctx, "running now")
		require.NoError(t, err)
		assert.Equal(t, StatusRunning, lastStatus.Status)
		assert.Equal(t, "running now", lastStatus.Message)
	})

	t.Run("ReportCloning", func(t *testing.T) {
		metadata := map[string]string{
			"repository": "github.com/org/repo",
			"branch":     "main",
		}
		err := client.ReportCloning(ctx, "Cloning repository", metadata)
		require.NoError(t, err)
		assert.Equal(t, StatusCloning, lastStatus.Status)
		assert.Equal(t, "Cloning repository", lastStatus.Message)
		assert.Equal(t, "github.com/org/repo", lastStatus.Metadata["repository"])
		assert.Equal(t, "main", lastStatus.Metadata["branch"])
	})

	t.Run("ReportBusy", func(t *testing.T) {
		err := client.ReportBusy(ctx, "processing")
		require.NoError(t, err)
		// ReportBusy sends to sessionStatus (not lifecycle status)
		assert.Equal(t, StatusBusy, lastStatus.SessionStatus)
		assert.Equal(t, "processing", lastStatus.Message)
	})

	t.Run("ReportIdle", func(t *testing.T) {
		err := client.ReportIdle(ctx, "waiting")
		require.NoError(t, err)
		// ReportIdle sends to sessionStatus (not lifecycle status)
		assert.Equal(t, StatusIdle, lastStatus.SessionStatus)
		assert.Equal(t, "waiting", lastStatus.Message)
	})

	t.Run("ReportError", func(t *testing.T) {
		err := client.ReportError(ctx, "something went wrong")
		require.NoError(t, err)
		assert.Equal(t, StatusError, lastStatus.Status)
	})

	t.Run("ReportShuttingDown", func(t *testing.T) {
		err := client.ReportShuttingDown(ctx, "shutting down")
		require.NoError(t, err)
		assert.Equal(t, StatusShuttingDown, lastStatus.Status)
	})

	t.Run("ReportStopped", func(t *testing.T) {
		err := client.ReportStopped(ctx, "agent stopped")
		require.NoError(t, err)
		assert.Equal(t, StatusStopped, lastStatus.Status)
		assert.Equal(t, "agent stopped", lastStatus.Message)
	})

	t.Run("ReportTaskCompleted", func(t *testing.T) {
		err := client.ReportTaskCompleted(ctx, "implemented feature")
		require.NoError(t, err)
		// ReportTaskCompleted sends to sessionStatus (not lifecycle status)
		assert.Equal(t, StatusIdle, lastStatus.SessionStatus)
		assert.Equal(t, "implemented feature", lastStatus.TaskSummary)
	})
}

func TestIsHostedMode(t *testing.T) {
	origMode := os.Getenv(EnvAgentMode)
	defer os.Setenv(EnvAgentMode, origMode)

	t.Run("not hosted mode", func(t *testing.T) {
		os.Unsetenv(EnvAgentMode)
		assert.False(t, IsHostedMode())

		os.Setenv(EnvAgentMode, "solo")
		assert.False(t, IsHostedMode())
	})

	t.Run("hosted mode", func(t *testing.T) {
		os.Setenv(EnvAgentMode, "hosted")
		assert.True(t, IsHostedMode())
	})
}

func TestGetAgentID(t *testing.T) {
	origID := os.Getenv(EnvAgentID)
	defer os.Setenv(EnvAgentID, origID)

	os.Setenv(EnvAgentID, "test-agent-id")
	assert.Equal(t, "test-agent-id", GetAgentID())

	os.Unsetenv(EnvAgentID)
	assert.Equal(t, "", GetAgentID())
}

func TestClient_RetryLogic(t *testing.T) {
	t.Run("retries on 5xx errors", func(t *testing.T) {
		attempts := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attempts++
			if attempts < 3 {
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte("server error"))
				return
			}
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		client := NewClientWithConfig(server.URL, "test-token", "agent-123")
		// Use shorter delays for testing
		client.retryBaseDelay = 10 * time.Millisecond
		client.retryMaxDelay = 50 * time.Millisecond

		err := client.UpdateStatus(context.Background(), StatusUpdate{Status: StatusRunning})
		require.NoError(t, err)
		assert.Equal(t, 3, attempts, "should have retried until success")
	})

	t.Run("does not retry on 4xx errors", func(t *testing.T) {
		attempts := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attempts++
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte("bad request"))
		}))
		defer server.Close()

		client := NewClientWithConfig(server.URL, "test-token", "agent-123")
		client.retryBaseDelay = 10 * time.Millisecond

		err := client.UpdateStatus(context.Background(), StatusUpdate{Status: StatusRunning})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "400")
		assert.Equal(t, 1, attempts, "should not retry on 4xx errors")
	})

	t.Run("gives up after max retries", func(t *testing.T) {
		attempts := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attempts++
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("server error"))
		}))
		defer server.Close()

		client := NewClientWithConfig(server.URL, "test-token", "agent-123")
		client.maxRetries = 2
		client.retryBaseDelay = 10 * time.Millisecond

		err := client.UpdateStatus(context.Background(), StatusUpdate{Status: StatusRunning})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "3 attempts")
		assert.Equal(t, 3, attempts, "should have attempted 1 + 2 retries")
	})

	t.Run("respects context cancellation during retry", func(t *testing.T) {
		attempts := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attempts++
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer server.Close()

		client := NewClientWithConfig(server.URL, "test-token", "agent-123")
		client.maxRetries = 5
		client.retryBaseDelay = 100 * time.Millisecond

		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()

		err := client.UpdateStatus(ctx, StatusUpdate{Status: StatusRunning})
		require.Error(t, err)
		assert.True(t, attempts < 5, "should have stopped early due to context timeout")
	})
}

func TestClient_CalculateBackoff(t *testing.T) {
	client := &Client{
		retryBaseDelay: 100 * time.Millisecond,
		retryMaxDelay:  5 * time.Second,
	}

	// attempt 1: base delay
	assert.Equal(t, 100*time.Millisecond, client.calculateBackoff(1))
	// attempt 2: base * 2
	assert.Equal(t, 200*time.Millisecond, client.calculateBackoff(2))
	// attempt 3: base * 4
	assert.Equal(t, 400*time.Millisecond, client.calculateBackoff(3))
	// attempt 4: base * 8
	assert.Equal(t, 800*time.Millisecond, client.calculateBackoff(4))
}

func TestClient_CalculateBackoff_MaxDelay(t *testing.T) {
	client := &Client{
		retryBaseDelay: 1 * time.Second,
		retryMaxDelay:  3 * time.Second,
	}

	// attempt 1: 1s
	assert.Equal(t, 1*time.Second, client.calculateBackoff(1))
	// attempt 2: 2s
	assert.Equal(t, 2*time.Second, client.calculateBackoff(2))
	// attempt 3: would be 4s, but capped at max
	assert.Equal(t, 3*time.Second, client.calculateBackoff(3))
	// attempt 4: still capped at max
	assert.Equal(t, 3*time.Second, client.calculateBackoff(4))
}

func TestClient_StartHeartbeat(t *testing.T) {
	t.Run("sends heartbeats at interval", func(t *testing.T) {
		heartbeatCount := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			heartbeatCount++
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		client := NewClientWithConfig(server.URL, "test-token", "agent-123")

		ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		defer cancel()

		done := client.StartHeartbeat(ctx, &HeartbeatConfig{
			Interval: 50 * time.Millisecond,
			Timeout:  100 * time.Millisecond,
		})

		<-done // Wait for heartbeat loop to finish
		// With 250ms timeout and 50ms interval, we expect ~4-5 heartbeats
		assert.GreaterOrEqual(t, heartbeatCount, 3, "should have sent multiple heartbeats")
	})

	t.Run("calls OnError callback on failure", func(t *testing.T) {
		errorCount := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer server.Close()

		client := NewClientWithConfig(server.URL, "test-token", "agent-123")
		// Reduce retries for faster test
		client.maxRetries = 0
		client.retryBaseDelay = 5 * time.Millisecond

		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()

		done := client.StartHeartbeat(ctx, &HeartbeatConfig{
			Interval: 50 * time.Millisecond,
			Timeout:  100 * time.Millisecond,
			OnError: func(err error) {
				errorCount++
			},
		})

		<-done
		assert.GreaterOrEqual(t, errorCount, 1, "should have called OnError")
	})

	t.Run("calls OnSuccess callback on success", func(t *testing.T) {
		successCount := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		client := NewClientWithConfig(server.URL, "test-token", "agent-123")

		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()

		done := client.StartHeartbeat(ctx, &HeartbeatConfig{
			Interval: 50 * time.Millisecond,
			Timeout:  100 * time.Millisecond,
			OnSuccess: func() {
				successCount++
			},
		})

		<-done
		assert.GreaterOrEqual(t, successCount, 1, "should have called OnSuccess")
	})

	t.Run("stops when context is cancelled", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		client := NewClientWithConfig(server.URL, "test-token", "agent-123")

		ctx, cancel := context.WithCancel(context.Background())
		done := client.StartHeartbeat(ctx, &HeartbeatConfig{
			Interval: 1 * time.Second, // Long interval
			Timeout:  100 * time.Millisecond,
		})

		// Cancel immediately
		cancel()

		// Should exit quickly
		select {
		case <-done:
			// Good - loop exited
		case <-time.After(100 * time.Millisecond):
			t.Fatal("heartbeat loop did not exit after context cancellation")
		}
	})
}

func TestClient_StartHeartbeat_DefaultConfig(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := NewClientWithConfig(server.URL, "test-token", "agent-123")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	// Should work with nil config (uses defaults)
	done := client.StartHeartbeat(ctx, nil)
	<-done
}
