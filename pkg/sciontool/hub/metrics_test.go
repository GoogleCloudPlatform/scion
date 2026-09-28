// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClient_ReportMetrics(t *testing.T) {
	attempts := 0
	var received MetricsPayload
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/agents/agent-123/metrics", r.URL.Path)
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		assert.Equal(t, "test-token", r.Header.Get("X-Scion-Agent-Token"))
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if attempts == 1 {
			http.Error(w, "retry", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client := NewClientWithConfig(server.URL, "test-token", "agent-123")
	client.retryBaseDelay = time.Millisecond
	payload := MetricsPayload{
		Type:    "agent_metrics",
		AgentID: "agent-123",
		Session: SessionMetrics{ID: "session-1"},
		Tokens:  TokenMetrics{Input: 12, Output: 7},
	}

	require.NoError(t, client.ReportMetrics(context.Background(), payload))
	assert.Equal(t, 2, attempts)
	assert.Equal(t, payload, received)
}

func TestClient_ReportMetricsDoesNotRetryClientErrors(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		http.Error(w, "bad metrics", http.StatusBadRequest)
	}))
	defer server.Close()

	client := NewClientWithConfig(server.URL, "test-token", "agent-123")
	client.retryBaseDelay = time.Millisecond

	err := client.ReportMetrics(context.Background(), MetricsPayload{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "hub returned error 400: bad metrics")
	assert.Equal(t, 1, attempts)
}
