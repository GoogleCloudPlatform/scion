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

package telemetry

import (
	"os"
	"sync"
	"time"
)

// ToolCallStats tracks per-tool invocation counts.
type ToolCallStats struct {
	Calls   int `json:"calls"`
	Success int `json:"success"`
	Error   int `json:"error"`
}

// SessionSummary is the aggregated result of a completed session, ready to be
// sent to the Hub as a MetricsPayload.
type SessionSummary struct {
	SessionID       string
	AgentID         string
	ProjectID       string
	StartedAt       time.Time
	EndedAt         time.Time
	Status          string
	Model           string
	TurnCount       int
	APICallCount    int
	TokensInput     int64
	TokensOutput    int64
	TokensCached    int64
	TokensReasoning int64
	ToolCalls       map[string]ToolCallStats
}

// Aggregator accumulates session-level metrics from hook events. It is
// thread-safe: hook events may arrive concurrently from different goroutines.
type Aggregator struct {
	mu sync.Mutex

	sessionID       string
	agentID         string
	projectID       string
	startedAt       time.Time
	model           string
	turnCount       int
	apiCallCount    int
	tokensInput     int64
	tokensOutput    int64
	tokensCached    int64
	tokensReasoning int64
	toolCalls       map[string]*ToolCallStats
}

// NewAggregator creates a new Aggregator pre-populated with agent and project
// IDs from the environment.
func NewAggregator() *Aggregator {
	agentID := os.Getenv("SCION_AGENT_ID")
	projectID := os.Getenv("SCION_GROVE_ID")
	if projectID == "" {
		projectID = os.Getenv("SCION_PROJECT_ID")
	}
	model := os.Getenv("SCION_MODEL")

	return &Aggregator{
		agentID:   agentID,
		projectID: projectID,
		model:     model,
		toolCalls: make(map[string]*ToolCallStats),
	}
}

// StartSession initialises the aggregator for a new session. It resets all
// counters so the same aggregator can be reused across sessions.
func (a *Aggregator) StartSession(sessionID string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.sessionID = sessionID
	a.startedAt = time.Now()
	a.turnCount = 0
	a.apiCallCount = 0
	a.tokensInput = 0
	a.tokensOutput = 0
	a.tokensCached = 0
	a.tokensReasoning = 0
	a.toolCalls = make(map[string]*ToolCallStats)
}

// RecordToolEnd records a completed tool call.
func (a *Aggregator) RecordToolEnd(toolName string, errMsg string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	stats, ok := a.toolCalls[toolName]
	if !ok {
		stats = &ToolCallStats{}
		a.toolCalls[toolName] = stats
	}
	stats.Calls++
	if errMsg != "" {
		stats.Error++
	} else {
		stats.Success++
	}
}

// RecordModelEnd records a completed model/API call and accumulates token
// counts reported on the event.
func (a *Aggregator) RecordModelEnd(inputTokens, outputTokens, cachedTokens, reasoningTokens int64) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.apiCallCount++
	a.tokensInput += inputTokens
	a.tokensOutput += outputTokens
	a.tokensCached += cachedTokens
	a.tokensReasoning += reasoningTokens
}

// RecordTurn records an agent turn (agent-end event).
func (a *Aggregator) RecordTurn() {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.turnCount++
}

// Finalize produces a SessionSummary snapshot and accepts the cumulative token
// counts from the session-end event. If the session-end event provides token
// totals they override the running accumulation (they represent the harness's
// authoritative totals).
func (a *Aggregator) Finalize(inputTokens, outputTokens, cachedTokens, reasoningTokens int64, errMsg string) SessionSummary {
	a.mu.Lock()
	defer a.mu.Unlock()

	status := "completed"
	if errMsg != "" {
		status = "error"
	}

	// If session-end carries cumulative totals, prefer them. We use a group
	// gate: if any session-end token total is non-zero, all values are
	// treated as authoritative (so legitimate zeros override accumulated
	// values).
	if inputTokens > 0 || outputTokens > 0 || cachedTokens > 0 || reasoningTokens > 0 {
		a.tokensInput = inputTokens
		a.tokensOutput = outputTokens
		a.tokensCached = cachedTokens
		a.tokensReasoning = reasoningTokens
	}

	toolCalls := make(map[string]ToolCallStats, len(a.toolCalls))
	for name, stats := range a.toolCalls {
		toolCalls[name] = *stats
	}

	return SessionSummary{
		SessionID:       a.sessionID,
		AgentID:         a.agentID,
		ProjectID:       a.projectID,
		StartedAt:       a.startedAt,
		EndedAt:         time.Now(),
		Status:          status,
		Model:           a.model,
		TurnCount:       a.turnCount,
		APICallCount:    a.apiCallCount,
		TokensInput:     a.tokensInput,
		TokensOutput:    a.tokensOutput,
		TokensCached:    a.tokensCached,
		TokensReasoning: a.tokensReasoning,
		ToolCalls:       toolCalls,
	}
}
