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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
	"github.com/go-jose/go-jose/v4/jwt"
)

func newRateLimitedTestRequest(method, path, body, agentID string) (*http.Request, *logging.RequestMeta) {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	meta := &logging.RequestMeta{}
	ctx := logging.ContextWithRequestMeta(req.Context(), meta)
	claims := &AgentTokenClaims{
		Claims: jwt.Claims{Subject: agentID},
	}
	ctx = context.WithValue(ctx, agentContextKey{}, claims)
	return req.WithContext(ctx), meta
}

func captureSlogJSON(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestHandleAgentGCPIdentityToken_RateLimitSetsRetryAfterAndLogsAttribution(t *testing.T) {
	logBuf := captureSlogJSON(t)

	rl := NewGCPTokenRateLimiter(1, 1) // 1/s, burst 1
	agentID := tid("agent-rl-id")
	// Exhaust the agent's burst bucket.
	if !rl.Allow(agentID) {
		t.Fatal("setup: expected initial Allow to succeed")
	}

	srv := &Server{
		gcpTokenRateLimiter: rl,
		gcpTokenMetrics:     NewGCPTokenMetrics(),
	}

	audience := "https://target.example.com/api"
	req, meta := newRateLimitedTestRequest(
		http.MethodPost,
		"/api/v1/agent/gcp-identity-token",
		`{"audience":"`+audience+`"}`,
		agentID,
	)
	rec := httptest.NewRecorder()

	srv.handleAgentGCPIdentityToken(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (body: %s)", rec.Code, rec.Body.String())
	}

	ra := rec.Header().Get("Retry-After")
	secs, err := strconv.Atoi(ra)
	if err != nil || secs < 1 {
		t.Fatalf("Retry-After = %q, want integer >= 1", ra)
	}

	if meta.AgentID != agentID {
		t.Errorf("request meta AgentID = %q, want %q", meta.AgentID, agentID)
	}

	var logEntry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(logBuf.Bytes()), &logEntry); err != nil {
		t.Fatalf("failed to parse warning log JSON (%q): %v", logBuf.String(), err)
	}
	if logEntry["agent_id"] != agentID {
		t.Errorf("log agent_id = %v, want %q", logEntry["agent_id"], agentID)
	}
	if logEntry["audience"] != audience {
		t.Errorf("log audience = %v, want %q", logEntry["audience"], audience)
	}
	if logEntry["level"] != "WARN" {
		t.Errorf("log level = %v, want WARN", logEntry["level"])
	}
}

func TestHandleAgentGCPIdentityToken_RateLimitBoundedBodyRead(t *testing.T) {
	logBuf := captureSlogJSON(t)

	rl := NewGCPTokenRateLimiter(1, 1)
	agentID := tid("agent-rl-big")
	_ = rl.Allow(agentID)

	srv := &Server{
		gcpTokenRateLimiter: rl,
	}

	// Oversized body (> 4 KiB) must not cause an error or unbounded decode.
	oversized := `{"audience":"` + strings.Repeat("a", 16384) + `"}`
	req, _ := newRateLimitedTestRequest(
		http.MethodPost,
		"/api/v1/agent/gcp-identity-token",
		oversized,
		agentID,
	)
	rec := httptest.NewRecorder()

	srv.handleAgentGCPIdentityToken(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("expected Retry-After header on 429")
	}

	var logEntry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(logBuf.Bytes()), &logEntry); err != nil {
		t.Fatalf("failed to parse warning log JSON (%q): %v", logBuf.String(), err)
	}
	if logEntry["agent_id"] != agentID {
		t.Errorf("log agent_id = %v, want %q", logEntry["agent_id"], agentID)
	}
	// Truncated JSON fails unmarshal cleanly and logs empty audience.
	if logEntry["audience"] != "" {
		t.Errorf("log audience = %v, want empty string for truncated oversized JSON", logEntry["audience"])
	}
}

func TestHandleAgentGCPToken_RateLimitSetsRetryAfterAndLogsAgentID(t *testing.T) {
	logBuf := captureSlogJSON(t)

	rl := NewGCPTokenRateLimiter(1, 1)
	agentID := tid("agent-rl-access")
	_ = rl.Allow(agentID)

	srv := &Server{
		gcpTokenRateLimiter: rl,
		gcpTokenMetrics:     NewGCPTokenMetrics(),
	}

	req, meta := newRateLimitedTestRequest(
		http.MethodPost,
		"/api/v1/agent/gcp-token",
		`{"scopes":["https://www.googleapis.com/auth/cloud-platform"]}`,
		agentID,
	)
	rec := httptest.NewRecorder()

	srv.handleAgentGCPToken(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if secs, err := strconv.Atoi(rec.Header().Get("Retry-After")); err != nil || secs < 1 {
		t.Fatalf("Retry-After = %q, want integer >= 1", rec.Header().Get("Retry-After"))
	}
	if meta.AgentID != agentID {
		t.Errorf("request meta AgentID = %q, want %q", meta.AgentID, agentID)
	}

	var logEntry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(logBuf.Bytes()), &logEntry); err != nil {
		t.Fatalf("failed to parse warning log JSON (%q): %v", logBuf.String(), err)
	}
	if logEntry["agent_id"] != agentID {
		t.Errorf("log agent_id = %v, want %q", logEntry["agent_id"], agentID)
	}
}
