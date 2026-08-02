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
	"encoding/json"
	"net/http"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// metricsPayloadRequest is the JSON body POSTed by sciontool on session-end.
// It mirrors the Hub Reporting Protocol from the design doc Section 4.2.
type metricsPayloadRequest struct {
	Type      string               `json:"type"`
	AgentID   string               `json:"agent_id"`
	Timestamp string               `json:"timestamp"`
	Session   metricsSession       `json:"session"`
	Tokens    metricsTokens        `json:"tokens"`
	Tools     map[string]toolStats `json:"tools"`
	Languages []string             `json:"languages"`
}

type metricsSession struct {
	ID        string `json:"id"`
	StartedAt string `json:"started_at"`
	EndedAt   string `json:"ended_at"`
	Status    string `json:"status"`
	TurnCount int    `json:"turn_count"`
	Model     string `json:"model"`
}

type metricsTokens struct {
	Input     int64 `json:"input"`
	Output    int64 `json:"output"`
	Cached    int64 `json:"cached"`
	Reasoning int64 `json:"reasoning"`
}

type toolStats struct {
	Calls   int `json:"calls"`
	Success int `json:"success"`
	Error   int `json:"error"`
}

// handleAgentMetrics receives, validates, and persists a metrics payload from
// an agent. Only the agent itself (self-auth via X-Scion-Agent-Token) may
// report its own metrics.
func (s *Server) handleAgentMetrics(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	// Authenticate: only the agent itself may report its metrics.
	agentIdent := GetAgentIdentityFromContext(ctx)
	if agentIdent == nil {
		Unauthorized(w)
		return
	}
	if agentIdent.ID() != id {
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"Agents can only report their own metrics", nil)
		return
	}

	// Decode the payload.
	var req metricsPayloadRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	// Validate required fields.
	if req.Session.ID == "" {
		ValidationError(w, "session.id is required", nil)
		return
	}
	if req.Session.StartedAt == "" {
		ValidationError(w, "session.started_at is required", nil)
		return
	}

	// Parse timestamps.
	startedAt, err := time.Parse(time.RFC3339, req.Session.StartedAt)
	if err != nil {
		ValidationError(w, "session.started_at must be RFC3339", nil)
		return
	}
	var endedAt *time.Time
	if req.Session.EndedAt != "" {
		t, err := time.Parse(time.RFC3339, req.Session.EndedAt)
		if err != nil {
			ValidationError(w, "session.ended_at must be RFC3339", nil)
			return
		}
		if t.Before(startedAt) {
			ValidationError(w, "session.ended_at cannot be before session.started_at", nil)
			return
		}
		endedAt = &t
	}

	// Look up the agent to get the project ID.
	agent, err := s.store.GetAgent(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if agent == nil {
		writeError(w, http.StatusNotFound, ErrCodeNotFound, "Agent not found", nil)
		return
	}

	// Convert tool_calls to map[string]any for native JSON storage.
	var toolCalls map[string]any
	if len(req.Tools) > 0 {
		toolCalls = make(map[string]any, len(req.Tools))
		for k, v := range req.Tools {
			toolCalls[k] = v
		}
	}

	// Build the store model.
	metrics := &store.AgentSessionMetrics{
		ID:              uuid.New().String(),
		AgentID:         id,
		ProjectID:       agent.ProjectID,
		SessionID:       req.Session.ID,
		StartedAt:       startedAt,
		EndedAt:         endedAt,
		Status:          req.Session.Status,
		TurnCount:       req.Session.TurnCount,
		Model:           req.Session.Model,
		TokensInput:     req.Tokens.Input,
		TokensOutput:    req.Tokens.Output,
		TokensCached:    req.Tokens.Cached,
		TokensReasoning: req.Tokens.Reasoning,
		ToolCalls:       toolCalls,
		Languages:       req.Languages,
	}

	if err := s.store.CreateAgentSessionMetrics(ctx, metrics); err != nil {
		s.agentMetricsLog.Error("Failed to create session metrics",
			"agent_id", id, "session_id", req.Session.ID, "error", err)
		writeErrorFromErr(w, err, "")
		return
	}

	s.agentMetricsLog.Info("Session metrics recorded",
		"agent_id", id,
		"session_id", req.Session.ID,
		"tokens_input", req.Tokens.Input,
		"tokens_output", req.Tokens.Output,
	)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{"id": metrics.ID})
}
