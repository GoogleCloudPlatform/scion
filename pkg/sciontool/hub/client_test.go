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
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	state "github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// makeJWTWithExpiry builds an unsigned JWT-shaped token whose payload carries the
// given expiry. ParseTokenExpiry only base64-decodes the payload (it does not
// verify the signature), so this is enough to drive the refresh loop's
// expiry-tracking logic in tests.
func makeJWTWithExpiry(t *testing.T, exp time.Time) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payloadJSON, err := json.Marshal(struct {
		Exp int64 `json:"exp"`
	}{Exp: exp.Unix()})
	require.NoError(t, err)
	payload := base64.RawURLEncoding.EncodeToString(payloadJSON)
	return header + "." + payload + ".sig"
}

// scrubHubEnv clears all Hub-related environment variables for the
// duration of the test, preventing accidental communication with a
// real Hub when tests run inside an agent container. See issue #123.
func scrubHubEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		EnvHubEndpoint,
		EnvHubURL,
		EnvHubToken,
		EnvAgentID,
		EnvAgentMode,
	} {
		t.Setenv(key, "")
	}
	t.Cleanup(SetHubTestSandboxed())
}

func TestNewClient_FromEnvironment(t *testing.T) {
	// Clear Hub env vars to prevent leakage from the container (issue #123).
	scrubHubEnv(t)

	t.Run("missing env vars returns nil", func(t *testing.T) {
		scrubHubEnv(t)
		client := NewClient()
		assert.Nil(t, client)
	})

	t.Run("missing token returns nil", func(t *testing.T) {
		scrubHubEnv(t)
		t.Setenv(EnvHubURL, "http://hub.example.com")
		client := NewClient()
		assert.Nil(t, client)
	})

	t.Run("missing agentID returns nil", func(t *testing.T) {
		scrubHubEnv(t)
		t.Setenv(EnvHubEndpoint, "http://hub.example.com")
		t.Setenv(EnvHubToken, "test-token")
		client := NewClient()
		assert.Nil(t, client, "should not create client without agent ID (local agent scenario)")
	})

	t.Run("with all env vars returns client", func(t *testing.T) {
		scrubHubEnv(t)
		t.Setenv(EnvHubURL, "http://hub.example.com")
		t.Setenv(EnvHubToken, "test-token")
		t.Setenv(EnvAgentID, "agent-123")

		client := NewClient()
		require.NotNil(t, client)
		assert.True(t, client.IsConfigured())
	})

	t.Run("prefers SCION_HUB_ENDPOINT over SCION_HUB_URL", func(t *testing.T) {
		scrubHubEnv(t)
		t.Setenv(EnvHubEndpoint, "http://endpoint.example.com")
		t.Setenv(EnvHubURL, "http://url.example.com")
		t.Setenv(EnvHubToken, "test-token")
		t.Setenv(EnvAgentID, "agent-123")

		client := NewClient()
		require.NotNil(t, client)
		assert.Equal(t, "http://endpoint.example.com", client.hubURL)
	})

	t.Run("falls back to SCION_HUB_URL when SCION_HUB_ENDPOINT not set", func(t *testing.T) {
		scrubHubEnv(t)
		t.Setenv(EnvHubURL, "http://url.example.com")
		t.Setenv(EnvHubToken, "test-token")
		t.Setenv(EnvAgentID, "agent-123")

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

func TestClient_IsConfigured(t *testing.T) {
	t.Run("nil client", func(t *testing.T) {
		var c *Client
		assert.False(t, c.IsConfigured())
	})

	t.Run("empty client", func(t *testing.T) {
		c := &Client{}
		assert.False(t, c.IsConfigured())
	})

	t.Run("missing agentID", func(t *testing.T) {
		c := NewClientWithConfig("http://hub.example.com", "token", "")
		assert.False(t, c.IsConfigured())
	})

	t.Run("missing token", func(t *testing.T) {
		c := NewClientWithConfig("http://hub.example.com", "", "agent-123")
		assert.False(t, c.IsConfigured())
	})

	t.Run("missing hubURL", func(t *testing.T) {
		c := NewClientWithConfig("", "token", "agent-123")
		assert.False(t, c.IsConfigured())
	})

	t.Run("all fields set", func(t *testing.T) {
		c := NewClientWithConfig("http://hub.example.com", "token", "agent-123")
		assert.True(t, c.IsConfigured())
	})
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
		Phase:    state.PhaseRunning,
		Activity: state.ActivityWorking,
		Status:   "working",
		Message:  "test message",
	})

	require.NoError(t, err)
	assert.Equal(t, "test-token", receivedToken)
	assert.Equal(t, state.PhaseRunning, receivedStatus.Phase)
	assert.Equal(t, state.ActivityWorking, receivedStatus.Activity)
	assert.Equal(t, "working", receivedStatus.Status)
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
		assert.Contains(t, err.Error(), "not configured")
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

func TestClient_ReportState(t *testing.T) {
	var lastPayload map[string]interface{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&lastPayload)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := NewClientWithConfig(server.URL, "test-token", "agent-123")
	ctx := context.Background()

	t.Run("running/idle", func(t *testing.T) {
		err := client.ReportState(ctx, state.PhaseRunning, state.ActivityWorking, "ready")
		require.NoError(t, err)
		assert.Equal(t, "running", lastPayload["phase"])
		assert.Equal(t, "working", lastPayload["activity"])
		assert.Equal(t, "working", lastPayload["status"])
		assert.Equal(t, "ready", lastPayload["message"])
	})

	t.Run("stopped", func(t *testing.T) {
		err := client.ReportState(ctx, state.PhaseStopped, "", "session ended")
		require.NoError(t, err)
		assert.Equal(t, "stopped", lastPayload["phase"])
		assert.Equal(t, "stopped", lastPayload["status"])
		assert.Equal(t, "session ended", lastPayload["message"])
	})
}

func TestClient_Heartbeat(t *testing.T) {
	var lastStatus StatusUpdate

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&lastStatus)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := NewClientWithConfig(server.URL, "test-token", "agent-123")
	ctx := context.Background()

	err := client.Heartbeat(ctx)
	require.NoError(t, err)
	assert.Equal(t, state.Phase(""), lastStatus.Phase)
	assert.Equal(t, state.Activity(""), lastStatus.Activity)
	assert.True(t, lastStatus.Heartbeat)
}

func TestIsHostedMode(t *testing.T) {
	t.Run("not hosted mode", func(t *testing.T) {
		t.Setenv(EnvAgentMode, "")
		assert.False(t, IsHostedMode())

		t.Setenv(EnvAgentMode, "solo")
		assert.False(t, IsHostedMode())
	})

	t.Run("hosted mode", func(t *testing.T) {
		t.Setenv(EnvAgentMode, "hosted")
		assert.True(t, IsHostedMode())
	})
}

func TestGetAgentID(t *testing.T) {
	t.Setenv(EnvAgentID, "test-agent-id")
	assert.Equal(t, "test-agent-id", GetAgentID())

	t.Setenv(EnvAgentID, "")
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

		err := client.UpdateStatus(context.Background(), StatusUpdate{
			Activity: state.ActivityWorking,
			Status:   "working",
		})
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

		err := client.UpdateStatus(context.Background(), StatusUpdate{
			Activity: state.ActivityWorking,
			Status:   "working",
		})
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

		err := client.UpdateStatus(context.Background(), StatusUpdate{
			Activity: state.ActivityWorking,
			Status:   "working",
		})
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

		err := client.UpdateStatus(ctx, StatusUpdate{
			Activity: state.ActivityWorking,
			Status:   "working",
		})
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

func TestParseTokenExpiry(t *testing.T) {
	t.Run("valid JWT with exp claim", func(t *testing.T) {
		// Construct a simple JWT with known expiry
		// Header: {"alg":"HS256","typ":"JWT"}
		// Payload: {"sub":"agent-123","exp":1893456000} (2030-01-01T00:00:00Z)
		// Note: signature doesn't matter for expiry parsing
		token := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJhZ2VudC0xMjMiLCJleHAiOjE4OTM0NTYwMDB9.invalid-sig"

		expiry, err := ParseTokenExpiry(token)
		require.NoError(t, err)
		expected := time.Unix(1893456000, 0)
		assert.Equal(t, expected, expiry)
	})

	t.Run("invalid JWT format", func(t *testing.T) {
		_, err := ParseTokenExpiry("not-a-jwt")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "invalid JWT format")
	})

	t.Run("empty token", func(t *testing.T) {
		_, err := ParseTokenExpiry("")
		assert.Error(t, err)
	})

	t.Run("no exp claim", func(t *testing.T) {
		// Header: {"alg":"HS256","typ":"JWT"}
		// Payload: {"sub":"agent-123"} (no exp)
		token := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJhZ2VudC0xMjMifQ.invalid-sig"

		_, err := ParseTokenExpiry(token)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "no expiry claim")
	})
}

func TestClient_RefreshToken(t *testing.T) {
	t.Run("successful refresh", func(t *testing.T) {
		cleanup := SetTokenHome(t.TempDir())
		defer cleanup()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodPost, r.Method)
			assert.Equal(t, "/api/v1/agents/agent-123/token/refresh", r.URL.Path)
			assert.Equal(t, "old-token", r.Header.Get("X-Scion-Agent-Token"))

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"token":"new-token","expires_at":"2030-01-01T00:00:00Z"}`))
		}))
		defer server.Close()

		client := NewClientWithConfig(server.URL, "old-token", "agent-123")

		newToken, expiresAt, err := client.RefreshToken(context.Background())
		require.NoError(t, err)
		assert.Equal(t, "new-token", newToken)
		assert.Equal(t, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), expiresAt)

		// Client token should be updated
		assert.Equal(t, "new-token", client.GetToken())

		// Token file should be written for child processes
		fileToken := ReadTokenFile()
		assert.Equal(t, "new-token", fileToken, "refreshed token should be persisted to file")
	})

	t.Run("server rejects refresh", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte("token expired"))
		}))
		defer server.Close()

		client := NewClientWithConfig(server.URL, "expired-token", "agent-123")

		_, _, err := client.RefreshToken(context.Background())
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "401")

		// Client token should not be updated on failure
		assert.Equal(t, "expired-token", client.GetToken())
	})

	t.Run("not configured", func(t *testing.T) {
		client := &Client{}
		_, _, err := client.RefreshToken(context.Background())
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "not configured")
	})
}

func TestClient_GetToken(t *testing.T) {
	t.Run("nil client", func(t *testing.T) {
		var c *Client
		assert.Equal(t, "", c.GetToken())
	})

	t.Run("configured client", func(t *testing.T) {
		c := NewClientWithConfig("http://hub", "my-token", "agent-1")
		assert.Equal(t, "my-token", c.GetToken())
	})
}

func TestClient_StartTokenRefresh(t *testing.T) {
	t.Run("refreshes token at scheduled time", func(t *testing.T) {
		// Isolate the token file to a temp dir. Without this, RefreshToken's
		// WriteTokenFile would clobber the real ~/.scion/scion-token when the
		// suite is run inside an agent container (where the scion user exists).
		cleanup := SetTokenHome(t.TempDir())
		defer cleanup()

		refreshed := false
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			refreshed = true
			futureExpiry := time.Now().Add(10 * time.Hour).UTC().Format(time.RFC3339)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"token":"refreshed-token","expires_at":"` + futureExpiry + `"}`))
		}))
		defer server.Close()

		client := NewClientWithConfig(server.URL, "old-token", "agent-123")

		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()

		done := client.StartTokenRefresh(ctx, &TokenRefreshConfig{
			RefreshAt: time.Now(), // Refresh immediately
			Timeout:   5 * time.Second,
			OnRefreshed: func(newExpiry time.Time) {
				assert.True(t, newExpiry.After(time.Now()))
			},
		})

		<-done
		assert.True(t, refreshed, "should have refreshed the token")
		assert.Equal(t, "refreshed-token", client.GetToken())
	})

	t.Run("stops when context is cancelled", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"token":"new","expires_at":"2030-01-01T00:00:00Z"}`))
		}))
		defer server.Close()

		client := NewClientWithConfig(server.URL, "token", "agent-123")

		ctx, cancel := context.WithCancel(context.Background())
		done := client.StartTokenRefresh(ctx, &TokenRefreshConfig{
			RefreshAt: time.Now().Add(1 * time.Hour), // Far in future
		})

		cancel()

		select {
		case <-done:
			// Good
		case <-time.After(200 * time.Millisecond):
			t.Fatal("token refresh loop did not exit after context cancellation")
		}
	})

	t.Run("retries after a transient failure then recovers", func(t *testing.T) {
		cleanup := SetTokenHome(t.TempDir())
		defer cleanup()

		var calls int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// First attempt fails transiently (503); the second succeeds.
			if atomic.AddInt32(&calls, 1) == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				w.Write([]byte("temporarily down"))
				return
			}
			futureExpiry := time.Now().Add(10 * time.Hour).UTC().Format(time.RFC3339)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"token":"recovered-token","expires_at":"` + futureExpiry + `"}`))
		}))
		defer server.Close()

		client := NewClientWithConfig(server.URL, "old-token", "agent-123")

		var errCount int32
		refreshed := make(chan struct{}, 1)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		done := client.StartTokenRefresh(ctx, &TokenRefreshConfig{
			RefreshAt:      time.Now(),
			Timeout:        time.Second,
			RetryBaseDelay: 10 * time.Millisecond,
			RetryMaxDelay:  10 * time.Millisecond,
			OnError:        func(error) { atomic.AddInt32(&errCount, 1) },
			OnRefreshed: func(time.Time) {
				select {
				case refreshed <- struct{}{}:
				default:
				}
			},
		})

		select {
		case <-refreshed:
			// Recovered after the transient failure.
		case <-time.After(time.Second):
			t.Fatal("token refresh did not recover after a transient failure")
		}

		assert.Equal(t, "recovered-token", client.GetToken())
		assert.GreaterOrEqual(t, atomic.LoadInt32(&errCount), int32(1), "transient failure should invoke OnError")

		cancel()
		<-done
	})

	t.Run("auth lost fires once and keeps retrying", func(t *testing.T) {
		cleanup := SetTokenHome(t.TempDir())
		defer cleanup()

		// The current token is already expired, so the first failed refresh is a
		// terminal auth loss. The server always rejects with 401 (as it would
		// after a hub signing-key rotation).
		expiredToken := makeJWTWithExpiry(t, time.Now().Add(-time.Minute))

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte("invalid agent token: failed to verify token"))
		}))
		defer server.Close()

		client := NewClientWithConfig(server.URL, expiredToken, "agent-123")

		var errCount, authLostCount int32
		var sawUnauthorized atomic.Bool
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		done := client.StartTokenRefresh(ctx, &TokenRefreshConfig{
			RefreshAt:      time.Now(),
			Timeout:        time.Second,
			RetryBaseDelay: 10 * time.Millisecond,
			RetryMaxDelay:  10 * time.Millisecond,
			OnError: func(err error) {
				atomic.AddInt32(&errCount, 1)
				if errors.Is(err, ErrTokenRefreshUnauthorized) {
					sawUnauthorized.Store(true)
				}
			},
			OnAuthLost: func() { atomic.AddInt32(&authLostCount, 1) },
		})

		// Let several retries elapse.
		require.Eventually(t, func() bool {
			return atomic.LoadInt32(&errCount) >= 3
		}, time.Second, 5*time.Millisecond, "expected the loop to keep retrying")

		// The loop must still be running (not exited) despite the auth loss.
		select {
		case <-done:
			t.Fatal("refresh loop exited instead of continuing to retry after auth loss")
		default:
		}

		cancel()
		<-done

		assert.Equal(t, int32(1), atomic.LoadInt32(&authLostCount), "OnAuthLost should fire exactly once")
		assert.True(t, sawUnauthorized.Load(), "401 refresh error should wrap ErrTokenRefreshUnauthorized")
	})
}

func TestTokenRefreshBackoff(t *testing.T) {
	base := 30 * time.Second
	max := 5 * time.Minute

	assert.Equal(t, base, tokenRefreshBackoff(0, base, max), "non-positive failures clamps to one attempt")
	assert.Equal(t, base, tokenRefreshBackoff(1, base, max))
	assert.Equal(t, 2*base, tokenRefreshBackoff(2, base, max))
	assert.Equal(t, 4*base, tokenRefreshBackoff(3, base, max))
	assert.Equal(t, max, tokenRefreshBackoff(10, base, max), "backoff is capped at max")
}

func TestOperatingMode(t *testing.T) {
	tests := []struct {
		name         string
		endpoint     string
		hubURL       string
		agentMode    string
		expectedMode Mode
		expectedStr  string
	}{
		{
			name:         "no hub configured returns ModeLocal",
			endpoint:     "",
			hubURL:       "",
			agentMode:    "",
			expectedMode: ModeLocal,
			expectedStr:  "local",
		},
		{
			name:         "hub endpoint set without hosted mode returns ModeHubConnected",
			endpoint:     "http://hub.example.com",
			hubURL:       "",
			agentMode:    "",
			expectedMode: ModeHubConnected,
			expectedStr:  "hub-connected",
		},
		{
			name:         "hub endpoint set with hosted mode returns ModeHosted",
			endpoint:     "http://hub.example.com",
			hubURL:       "",
			agentMode:    "hosted",
			expectedMode: ModeHosted,
			expectedStr:  "hosted",
		},
		{
			name:         "legacy hub URL set without hosted mode returns ModeHubConnected",
			endpoint:     "",
			hubURL:       "http://hub.example.com",
			agentMode:    "",
			expectedMode: ModeHubConnected,
			expectedStr:  "hub-connected",
		},
		{
			name:         "legacy hub URL set with hosted mode returns ModeHosted",
			endpoint:     "",
			hubURL:       "http://hub.example.com",
			agentMode:    "hosted",
			expectedMode: ModeHosted,
			expectedStr:  "hosted",
		},
		{
			name:         "non-hosted agent mode with hub returns ModeHubConnected",
			endpoint:     "http://hub.example.com",
			hubURL:       "",
			agentMode:    "solo",
			expectedMode: ModeHubConnected,
			expectedStr:  "hub-connected",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clear Hub env, then set test values (issue #123).
			scrubHubEnv(t)
			if tt.endpoint != "" {
				t.Setenv(EnvHubEndpoint, tt.endpoint)
			}
			if tt.hubURL != "" {
				t.Setenv(EnvHubURL, tt.hubURL)
			}
			if tt.agentMode != "" {
				t.Setenv(EnvAgentMode, tt.agentMode)
			}

			mode := OperatingMode()
			assert.Equal(t, tt.expectedMode, mode)
			assert.Equal(t, tt.expectedStr, mode.String())
		})
	}
}

func TestOperatingMode_Defaults(t *testing.T) {
	// Clear all relevant env vars (issue #123).
	scrubHubEnv(t)

	mode := OperatingMode()
	assert.Equal(t, ModeLocal, mode, "should default to ModeLocal when no env vars are set")
	assert.Equal(t, "local", mode.String())
}

func TestTokenFile_WriteAndRead(t *testing.T) {
	cleanup := SetTokenHome(t.TempDir())
	defer cleanup()

	t.Run("read returns empty when no file", func(t *testing.T) {
		token := ReadTokenFile()
		assert.Equal(t, "", token)
	})

	t.Run("write and read round-trip", func(t *testing.T) {
		err := WriteTokenFile("my-refreshed-token")
		require.NoError(t, err)

		token := ReadTokenFile()
		assert.Equal(t, "my-refreshed-token", token)
	})

	t.Run("overwrite with newer token", func(t *testing.T) {
		err := WriteTokenFile("even-newer-token")
		require.NoError(t, err)

		token := ReadTokenFile()
		assert.Equal(t, "even-newer-token", token)
	})
}

func TestNewClient_UsesTokenFile(t *testing.T) {
	cleanup := SetTokenHome(t.TempDir())
	defer cleanup()

	// Clear Hub env, then set test values (issue #123).
	scrubHubEnv(t)
	t.Setenv(EnvHubEndpoint, "http://hub.example.com")
	t.Setenv(EnvHubToken, "original-env-token")
	t.Setenv(EnvAgentID, "agent-123")

	t.Run("uses env token when no file exists", func(t *testing.T) {
		client := NewClient()
		require.NotNil(t, client)
		assert.Equal(t, "original-env-token", client.GetToken())
	})

	t.Run("prefers file token over env token", func(t *testing.T) {
		err := WriteTokenFile("refreshed-file-token")
		require.NoError(t, err)

		client := NewClient()
		require.NotNil(t, client)
		assert.Equal(t, "refreshed-file-token", client.GetToken(),
			"NewClient should prefer the token file over the env var")
	})
}

func TestClient_RefreshToken_ConcurrentAccess(t *testing.T) {
	cleanup := SetTokenHome(t.TempDir())
	defer cleanup()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/agents/agent-123/token/refresh" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"token":"concurrent-new-token","expires_at":"2030-01-01T00:00:00Z"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := NewClientWithConfig(server.URL, "initial-token", "agent-123")

	// Run concurrent refresh and heartbeat to detect data races.
	// Use -race flag to validate: go test -race ./pkg/sciontool/hub/
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			client.RefreshToken(context.Background())
		}
	}()

	// Concurrent heartbeats
	for ctx.Err() == nil {
		client.Heartbeat(context.Background())
	}

	<-done
	// If we get here without a race detector failure, the mutex is working
	assert.NotEmpty(t, client.GetToken())
}

func TestClient_RefreshGitHubToken(t *testing.T) {
	t.Run("successful refresh", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodPost, r.Method)
			assert.Equal(t, "/api/v1/agents/agent-123/refresh-token", r.URL.Path)
			assert.Equal(t, "hub-token", r.Header.Get("X-Scion-Agent-Token"))

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"token":"ghs_fresh_github_token","expires_at":"2030-01-01T01:00:00Z"}`))
		}))
		defer server.Close()

		client := NewClientWithConfig(server.URL, "hub-token", "agent-123")

		newToken, expiresAt, err := client.RefreshGitHubToken(context.Background())
		require.NoError(t, err)
		assert.Equal(t, "ghs_fresh_github_token", newToken)
		assert.Equal(t, time.Date(2030, 1, 1, 1, 0, 0, 0, time.UTC), expiresAt)
	})

	t.Run("server rejects request", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":"no github app installation"}`))
		}))
		defer server.Close()

		client := NewClientWithConfig(server.URL, "hub-token", "agent-123")

		_, _, err := client.RefreshGitHubToken(context.Background())
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "403")
	})

	t.Run("not configured", func(t *testing.T) {
		client := &Client{}
		_, _, err := client.RefreshGitHubToken(context.Background())
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "not configured")
	})

	t.Run("parses ISO 8601 without timezone name", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			// This is the format returned by mintGitHubAppToken: "2006-01-02T15:04:05Z"
			w.Write([]byte(`{"token":"ghs_token","expires_at":"2030-06-15T14:30:00Z"}`))
		}))
		defer server.Close()

		client := NewClientWithConfig(server.URL, "hub-token", "agent-123")

		_, expiresAt, err := client.RefreshGitHubToken(context.Background())
		require.NoError(t, err)
		assert.Equal(t, time.Date(2030, 6, 15, 14, 30, 0, 0, time.UTC), expiresAt)
	})
}

func TestGitHubTokenFile_WriteAndRead(t *testing.T) {
	tmpDir := t.TempDir()
	tokenPath := tmpDir + "/github-token"

	t.Run("read returns empty when no file", func(t *testing.T) {
		token := ReadGitHubTokenFile(tokenPath)
		assert.Equal(t, "", token)
	})

	t.Run("write and read round-trip", func(t *testing.T) {
		err := WriteGitHubTokenFile(tokenPath, "ghs_test_token")
		require.NoError(t, err)

		token := ReadGitHubTokenFile(tokenPath)
		assert.Equal(t, "ghs_test_token", token)
	})

	t.Run("overwrites with newer token", func(t *testing.T) {
		err := WriteGitHubTokenFile(tokenPath, "ghs_newer_token")
		require.NoError(t, err)

		token := ReadGitHubTokenFile(tokenPath)
		assert.Equal(t, "ghs_newer_token", token)
	})
}

func TestIsGitHubAppEnabled(t *testing.T) {
	t.Setenv(EnvGitHubAppEnabled, "")
	assert.False(t, IsGitHubAppEnabled())

	t.Setenv(EnvGitHubAppEnabled, "false")
	assert.False(t, IsGitHubAppEnabled())

	t.Setenv(EnvGitHubAppEnabled, "true")
	assert.True(t, IsGitHubAppEnabled())
}

func TestGitHubTokenPath(t *testing.T) {
	t.Setenv(EnvGitHubTokenPath, "")
	assert.Equal(t, DefaultGitHubTokenPath, GitHubTokenPath())

	t.Setenv(EnvGitHubTokenPath, "/custom/path/token")
	assert.Equal(t, "/custom/path/token", GitHubTokenPath())
}

func TestClient_StartGitHubTokenRefresh(t *testing.T) {
	t.Run("refreshes token and writes to file", func(t *testing.T) {
		tmpDir := t.TempDir()
		tokenPath := tmpDir + "/github-token"

		refreshed := false
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			refreshed = true
			futureExpiry := time.Now().Add(1 * time.Hour).UTC().Format(time.RFC3339)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"token":"ghs_refreshed","expires_at":"` + futureExpiry + `"}`))
		}))
		defer server.Close()

		client := NewClientWithConfig(server.URL, "hub-token", "agent-123")

		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()

		var receivedToken string
		done := client.StartGitHubTokenRefresh(ctx, &GitHubTokenRefreshConfig{
			RefreshAt: time.Now(), // Refresh immediately
			TokenPath: tokenPath,
			OnRefreshed: func(newToken string, newExpiry time.Time) {
				receivedToken = newToken
			},
		})

		<-done
		assert.True(t, refreshed, "should have refreshed the GitHub token")
		assert.Equal(t, "ghs_refreshed", receivedToken)

		// Token file should have been written
		fileToken := ReadGitHubTokenFile(tokenPath)
		assert.Equal(t, "ghs_refreshed", fileToken)
	})

	t.Run("stops when context is cancelled", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"token":"ghs_new","expires_at":"2030-01-01T00:00:00Z"}`))
		}))
		defer server.Close()

		client := NewClientWithConfig(server.URL, "hub-token", "agent-123")

		ctx, cancel := context.WithCancel(context.Background())
		done := client.StartGitHubTokenRefresh(ctx, &GitHubTokenRefreshConfig{
			RefreshAt: time.Now().Add(1 * time.Hour), // Far in future
			TokenPath: "/tmp/test-token-never-written",
		})

		cancel()

		select {
		case <-done:
			// Good
		case <-time.After(200 * time.Millisecond):
			t.Fatal("GitHub token refresh loop did not exit after context cancellation")
		}
	})

	t.Run("calls error callback on failure", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("server error"))
		}))
		defer server.Close()

		client := NewClientWithConfig(server.URL, "hub-token", "agent-123")

		// Use a short context — we just want to verify the error callback fires
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()

		errorCount := 0
		done := client.StartGitHubTokenRefresh(ctx, &GitHubTokenRefreshConfig{
			RefreshAt: time.Now(), // Refresh immediately
			TokenPath: "",
			OnError: func(err error) {
				errorCount++
			},
		})

		<-done
		assert.GreaterOrEqual(t, errorCount, 1, "should have called OnError at least once")
	})
}

func TestGitHubTokenExpiry_WriteAndRead(t *testing.T) {
	tmpDir := t.TempDir()
	tokenPath := tmpDir + "/github-token"

	t.Run("read returns error when no expiry file", func(t *testing.T) {
		_, err := ReadGitHubTokenExpiry(tokenPath)
		assert.Error(t, err)
	})

	t.Run("write and read round-trip", func(t *testing.T) {
		expiry := time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC)
		err := WriteGitHubTokenExpiry(tokenPath, expiry)
		require.NoError(t, err)

		got, err := ReadGitHubTokenExpiry(tokenPath)
		require.NoError(t, err)
		assert.True(t, expiry.Equal(got), "expected %v, got %v", expiry, got)
	})

	t.Run("expiry file path is companion to token path", func(t *testing.T) {
		assert.Equal(t, "/tmp/.github-token.expiry", GitHubTokenExpiryPath("/tmp/.github-token"))
	})
}

func TestIsGitHubTokenExpired(t *testing.T) {
	tmpDir := t.TempDir()
	tokenPath := tmpDir + "/github-token"

	t.Run("returns true when no expiry file exists", func(t *testing.T) {
		assert.True(t, IsGitHubTokenExpired(tokenPath))
	})

	t.Run("returns true when token is expired", func(t *testing.T) {
		expiry := time.Now().Add(-1 * time.Hour) // expired 1 hour ago
		err := WriteGitHubTokenExpiry(tokenPath, expiry)
		require.NoError(t, err)
		assert.True(t, IsGitHubTokenExpired(tokenPath))
	})

	t.Run("returns false when token is still valid", func(t *testing.T) {
		expiry := time.Now().Add(1 * time.Hour) // valid for 1 more hour
		err := WriteGitHubTokenExpiry(tokenPath, expiry)
		require.NoError(t, err)
		assert.False(t, IsGitHubTokenExpired(tokenPath))
	})
}

func TestStartGitHubTokenRefresh_WritesExpiryFile(t *testing.T) {
	tmpDir := t.TempDir()
	tokenPath := tmpDir + "/github-token"

	futureExpiry := time.Now().Add(1 * time.Hour).UTC()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"token":"ghs_refreshed","expires_at":"` + futureExpiry.Format(time.RFC3339) + `"}`))
	}))
	defer server.Close()

	client := NewClientWithConfig(server.URL, "hub-token", "agent-123")

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	done := client.StartGitHubTokenRefresh(ctx, &GitHubTokenRefreshConfig{
		RefreshAt: time.Now(),
		TokenPath: tokenPath,
	})

	<-done

	// Verify both token and expiry files were written
	fileToken := ReadGitHubTokenFile(tokenPath)
	assert.Equal(t, "ghs_refreshed", fileToken)

	gotExpiry, err := ReadGitHubTokenExpiry(tokenPath)
	require.NoError(t, err)
	// Allow 1 second tolerance for time formatting precision
	assert.WithinDuration(t, futureExpiry, gotExpiry, 1*time.Second)

	// Token should not be considered expired
	assert.False(t, IsGitHubTokenExpired(tokenPath))
}

func TestStartGitHubTokenRefresh_CallsOnRefreshedAfterEnvUpdate(t *testing.T) {
	tmpDir := t.TempDir()
	tokenPath := tmpDir + "/github-token"

	t.Setenv("GITHUB_TOKEN", "ghs_original_stale")

	futureExpiry := time.Now().Add(1 * time.Hour).UTC()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{
			"token":      "ghs_fresh_token",
			"expires_at": futureExpiry.Format(time.RFC3339),
		})
	}))
	defer server.Close()

	client := NewClientWithConfig(server.URL, "hub-token", "agent-123")

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	var callbackToken string
	done := client.StartGitHubTokenRefresh(ctx, &GitHubTokenRefreshConfig{
		RefreshAt: time.Now(),
		TokenPath: tokenPath,
		OnRefreshed: func(newToken string, newExpiry time.Time) {
			// At callback time, GITHUB_TOKEN env var should already be updated
			callbackToken = newToken
			assert.Equal(t, "ghs_fresh_token", os.Getenv("GITHUB_TOKEN"),
				"GITHUB_TOKEN env var should be updated before OnRefreshed is called")
		},
	})

	<-done

	assert.Equal(t, "ghs_fresh_token", callbackToken,
		"OnRefreshed callback should have been called with the new token")
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
