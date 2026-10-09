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

	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
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

	// open is true between the start of a session (explicit or implicit)
	// and its Finalize. implicit marks a session opened by ObserveSession
	// because its session-start event was not seen.
	open     bool
	implicit bool
}

// NewAggregator creates a new Aggregator pre-populated with agent and project
// IDs from the environment.
func NewAggregator() *Aggregator {
	agentID := os.Getenv("SCION_AGENT_ID")
	projectID := projectkeys.ProjectIDFromEnv(os.Getenv)
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
//
// If a session was already opened implicitly by ObserveSession (the
// session-start event arrived late) and the IDs do not conflict, that
// session is adopted as-is so the events recorded so far are kept and not
// counted twice.
//
// A session-start with the same ID as the open session (Claude sends one
// on /compact and on resume) also keeps the counts and start time. A
// different ID, or any start after Finalize, resets.
func (a *Aggregator) StartSession(sessionID string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	sameID := sessionID != "" && a.sessionID == sessionID
	if a.open && (sameID || (a.implicit && (a.sessionID == "" || sessionID == ""))) {
		if a.sessionID == "" {
			a.sessionID = sessionID
		}
		a.implicit = false
		return
	}

	a.resetLocked(sessionID)
}

// ObserveSession records the session ID carried by any hook event other
// than session-start. If no session is open (the session-start event was
// missed) it opens one implicitly, starting now. If the open session has no
// ID yet, the observed ID is adopted. An ID that differs from the open
// session's ID is ignored.
func (a *Aggregator) ObserveSession(sessionID string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if !a.open {
		a.resetLocked(sessionID)
		a.implicit = true
		return
	}
	if a.sessionID == "" {
		a.sessionID = sessionID
	}
}

func (a *Aggregator) resetLocked(sessionID string) {
	a.open = true
	a.implicit = false
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

	endedAt := time.Now()
	startedAt := a.startedAt
	// Defensive: through the handler, ObserveSession always opens the
	// session before Finalize, so startedAt is only zero when Finalize is
	// called directly. The Hub requires started_at, so never send zero.
	if startedAt.IsZero() {
		startedAt = endedAt
	}
	a.open = false
	a.implicit = false

	toolCalls := make(map[string]ToolCallStats, len(a.toolCalls))
	for name, stats := range a.toolCalls {
		toolCalls[name] = *stats
	}

	return SessionSummary{
		SessionID:       a.sessionID,
		AgentID:         a.agentID,
		ProjectID:       a.projectID,
		StartedAt:       startedAt,
		EndedAt:         endedAt,
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

// AggregatorState is the serializable form of an Aggregator's per-session
// state. Short-lived hook processes use it to carry a session's counts from
// one hook invocation to the next. The agent ID, project ID and model are not
// part of it: they come from the environment, which every hook process for
// the agent shares.
type AggregatorState struct {
	SessionID       string                   `json:"session_id"`
	StartedAt       time.Time                `json:"started_at"`
	Open            bool                     `json:"open"`
	Implicit        bool                     `json:"implicit"`
	TurnCount       int                      `json:"turn_count"`
	APICallCount    int                      `json:"api_call_count"`
	TokensInput     int64                    `json:"tokens_input"`
	TokensOutput    int64                    `json:"tokens_output"`
	TokensCached    int64                    `json:"tokens_cached"`
	TokensReasoning int64                    `json:"tokens_reasoning"`
	ToolCalls       map[string]ToolCallStats `json:"tool_calls,omitempty"`
}

// State returns a snapshot of the aggregator's per-session state.
func (a *Aggregator) State() AggregatorState {
	a.mu.Lock()
	defer a.mu.Unlock()

	toolCalls := make(map[string]ToolCallStats, len(a.toolCalls))
	for name, stats := range a.toolCalls {
		toolCalls[name] = *stats
	}
	return AggregatorState{
		SessionID:       a.sessionID,
		StartedAt:       a.startedAt,
		Open:            a.open,
		Implicit:        a.implicit,
		TurnCount:       a.turnCount,
		APICallCount:    a.apiCallCount,
		TokensInput:     a.tokensInput,
		TokensOutput:    a.tokensOutput,
		TokensCached:    a.tokensCached,
		TokensReasoning: a.tokensReasoning,
		ToolCalls:       toolCalls,
	}
}

// RestoreState replaces the aggregator's per-session state with s, as
// previously returned by State. The counting rules applied to later events
// are unchanged.
func (a *Aggregator) RestoreState(s AggregatorState) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.sessionID = s.SessionID
	a.startedAt = s.StartedAt
	a.open = s.Open
	a.implicit = s.Implicit
	a.turnCount = s.TurnCount
	a.apiCallCount = s.APICallCount
	a.tokensInput = s.TokensInput
	a.tokensOutput = s.TokensOutput
	a.tokensCached = s.TokensCached
	a.tokensReasoning = s.TokensReasoning
	a.toolCalls = make(map[string]*ToolCallStats, len(s.ToolCalls))
	for name, stats := range s.ToolCalls {
		stats := stats
		a.toolCalls[name] = &stats
	}
}
