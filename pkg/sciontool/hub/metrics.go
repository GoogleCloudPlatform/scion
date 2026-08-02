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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/telemetry"
)

// SessionMetrics holds session-level information in a MetricsPayload.
type SessionMetrics struct {
	ID        string `json:"id"`
	StartedAt string `json:"started_at"`
	EndedAt   string `json:"ended_at,omitempty"`
	Status    string `json:"status,omitempty"`
	TurnCount int    `json:"turn_count,omitempty"`
	Model     string `json:"model,omitempty"`
}

// TokenMetrics holds token usage counts in a MetricsPayload.
type TokenMetrics struct {
	Input     int64 `json:"input"`
	Output    int64 `json:"output"`
	Cached    int64 `json:"cached,omitempty"`
	Reasoning int64 `json:"reasoning,omitempty"`
}

// ToolStats holds per-tool invocation counts in a MetricsPayload.
type ToolStats struct {
	Calls   int `json:"calls"`
	Success int `json:"success"`
	Error   int `json:"error"`
}

// MetricsPayload is the JSON payload sent from sciontool to the Hub on
// session-end. It matches the Hub Reporting Protocol defined in the design doc
// (Section 4.2).
type MetricsPayload struct {
	Type      string               `json:"type"`
	AgentID   string               `json:"agent_id"`
	Timestamp string               `json:"timestamp"`
	Session   SessionMetrics       `json:"session"`
	Tokens    TokenMetrics         `json:"tokens"`
	Tools     map[string]ToolStats `json:"tools,omitempty"`
}

// ReportMetrics sends a metrics payload to the Hub. It follows the same
// pattern as UpdateStatus: POST with agent token auth, retry on 5xx, no retry
// on 4xx.
func (c *Client) ReportMetrics(ctx context.Context, payload MetricsPayload) error {
	if !c.IsConfigured() {
		return fmt.Errorf("hub client not configured")
	}

	endpoint := fmt.Sprintf("%s/api/v1/agents/%s/metrics",
		strings.TrimSuffix(c.hubURL, "/"), c.agentID)

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal metrics payload: %w", err)
	}

	c.tokenMu.RLock()
	currentToken := c.token
	c.tokenMu.RUnlock()

	var lastErr error
	attempts := c.maxRetries + 1
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			delay := c.calculateBackoff(attempt)
			select {
			case <-ctx.Done():
				return fmt.Errorf("context cancelled during retry: %w", ctx.Err())
			case <-time.After(delay):
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("failed to create request: %w", err)
		}

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Scion-Agent-Token", currentToken)

		resp, err := c.client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("request failed (context cancelled): %w", ctx.Err())
			}
			lastErr = fmt.Errorf("failed to send request: %w", err)
			continue
		}

		respBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if resp.StatusCode < 400 {
			return nil
		}

		// 4xx — client error, don't retry
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return fmt.Errorf("hub returned error %d: %s", resp.StatusCode, string(respBody))
		}

		// 5xx — server error, retry
		lastErr = fmt.Errorf("hub returned error %d: %s", resp.StatusCode, string(respBody))
	}

	return fmt.Errorf("request failed after %d attempts: %w", attempts, lastErr)
}

// SummaryToMetricsPayload converts a telemetry.SessionSummary (produced by the
// aggregator on session-end) into a MetricsPayload suitable for ReportMetrics.
func SummaryToMetricsPayload(s telemetry.SessionSummary) MetricsPayload {
	tools := make(map[string]ToolStats, len(s.ToolCalls))
	for name, tc := range s.ToolCalls {
		tools[name] = ToolStats{
			Calls:   tc.Calls,
			Success: tc.Success,
			Error:   tc.Error,
		}
	}

	return MetricsPayload{
		Type:      "agent_metrics",
		AgentID:   s.AgentID,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Session: SessionMetrics{
			ID:        s.SessionID,
			StartedAt: s.StartedAt.UTC().Format(time.RFC3339),
			EndedAt:   s.EndedAt.UTC().Format(time.RFC3339),
			Status:    s.Status,
			TurnCount: s.TurnCount,
			Model:     s.Model,
		},
		Tokens: TokenMetrics{
			Input:     s.TokensInput,
			Output:    s.TokensOutput,
			Cached:    s.TokensCached,
			Reasoning: s.TokensReasoning,
		},
		Tools: tools,
	}
}
