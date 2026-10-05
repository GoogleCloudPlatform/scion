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
	"sync"
	"testing"
)

func TestNewAggregator_ProjectIDEnvPrecedence(t *testing.T) {
	// SCION_GROVE_ID is no longer read; only SCION_PROJECT_ID is consulted.
	tests := []struct {
		name      string
		projectID string
		groveID   string
		want      string
	}{
		{name: "project id set", projectID: "proj-1", groveID: "", want: "proj-1"},
		{name: "grove id alone is ignored", projectID: "", groveID: "legacy-1", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("SCION_PROJECT_ID", tt.projectID)
			t.Setenv("SCION_GROVE_ID", tt.groveID)

			a := NewAggregator()
			if a.projectID != tt.want {
				t.Errorf("NewAggregator().projectID = %q, want %q", a.projectID, tt.want)
			}
		})
	}
}

func TestAggregator_BasicFlow(t *testing.T) {
	a := &Aggregator{
		agentID:   "agent-1",
		projectID: "project-1",
		model:     "claude-4",
		toolCalls: make(map[string]*ToolCallStats),
	}

	a.StartSession("session-abc")

	// Record some tool calls
	a.RecordToolEnd("read_file", "")
	a.RecordToolEnd("read_file", "")
	a.RecordToolEnd("write_file", "")
	a.RecordToolEnd("shell_execute", "command failed")

	// Record model calls
	a.RecordModelEnd(1000, 200, 500, 0)
	a.RecordModelEnd(2000, 300, 1000, 0)

	// Record turns
	a.RecordTurn()
	a.RecordTurn()

	summary := a.Finalize(0, 0, 0, 0, "")

	if summary.SessionID != "session-abc" {
		t.Errorf("expected session ID session-abc, got %s", summary.SessionID)
	}
	if summary.AgentID != "agent-1" {
		t.Errorf("expected agent ID agent-1, got %s", summary.AgentID)
	}
	if summary.Status != "completed" {
		t.Errorf("expected status completed, got %s", summary.Status)
	}
	if summary.Model != "claude-4" {
		t.Errorf("expected model claude-4, got %s", summary.Model)
	}
	if summary.TurnCount != 2 {
		t.Errorf("expected 2 turns, got %d", summary.TurnCount)
	}
	if summary.TokensInput != 3000 {
		t.Errorf("expected 3000 input tokens, got %d", summary.TokensInput)
	}
	if summary.TokensOutput != 500 {
		t.Errorf("expected 500 output tokens, got %d", summary.TokensOutput)
	}
	if summary.TokensCached != 1500 {
		t.Errorf("expected 1500 cached tokens, got %d", summary.TokensCached)
	}

	// Check tool call stats
	rf, ok := summary.ToolCalls["read_file"]
	if !ok {
		t.Fatal("expected read_file tool stats")
	}
	if rf.Calls != 2 || rf.Success != 2 || rf.Error != 0 {
		t.Errorf("read_file: expected calls=2 success=2 error=0, got calls=%d success=%d error=%d", rf.Calls, rf.Success, rf.Error)
	}

	se, ok := summary.ToolCalls["shell_execute"]
	if !ok {
		t.Fatal("expected shell_execute tool stats")
	}
	if se.Calls != 1 || se.Success != 0 || se.Error != 1 {
		t.Errorf("shell_execute: expected calls=1 success=0 error=1, got calls=%d success=%d error=%d", se.Calls, se.Success, se.Error)
	}
}

func TestAggregator_FinalizeWithSessionEndTokens(t *testing.T) {
	a := &Aggregator{
		agentID:   "agent-1",
		projectID: "project-1",
		model:     "gemini-2.0",
		toolCalls: make(map[string]*ToolCallStats),
	}

	a.StartSession("session-xyz")

	// Accumulate during session
	a.RecordModelEnd(1000, 200, 500, 0)

	// Finalize with authoritative session-end totals
	summary := a.Finalize(5000, 1200, 3000, 0, "")

	// Session-end totals should override
	if summary.TokensInput != 5000 {
		t.Errorf("expected 5000 input tokens (from session-end), got %d", summary.TokensInput)
	}
	if summary.TokensOutput != 1200 {
		t.Errorf("expected 1200 output tokens (from session-end), got %d", summary.TokensOutput)
	}
	if summary.TokensCached != 3000 {
		t.Errorf("expected 3000 cached tokens (from session-end), got %d", summary.TokensCached)
	}
}

func TestAggregator_FinalizeWithError(t *testing.T) {
	a := &Aggregator{
		agentID:   "agent-1",
		projectID: "project-1",
		toolCalls: make(map[string]*ToolCallStats),
	}

	a.StartSession("session-err")
	summary := a.Finalize(0, 0, 0, 0, "session crashed")

	if summary.Status != "error" {
		t.Errorf("expected status error, got %s", summary.Status)
	}
}

func TestAggregator_StartSessionResets(t *testing.T) {
	a := &Aggregator{
		agentID:   "agent-1",
		projectID: "project-1",
		toolCalls: make(map[string]*ToolCallStats),
	}

	a.StartSession("session-1")
	a.RecordToolEnd("read_file", "")
	a.RecordModelEnd(1000, 200, 500, 0)

	// Start a new session — should reset
	a.StartSession("session-2")
	summary := a.Finalize(0, 0, 0, 0, "")

	if summary.SessionID != "session-2" {
		t.Errorf("expected session-2, got %s", summary.SessionID)
	}
	if summary.TokensInput != 0 {
		t.Errorf("expected 0 input tokens after reset, got %d", summary.TokensInput)
	}
	if len(summary.ToolCalls) != 0 {
		t.Errorf("expected 0 tool calls after reset, got %d", len(summary.ToolCalls))
	}
}

func TestAggregator_ReasoningTokens(t *testing.T) {
	a := &Aggregator{
		agentID:   "agent-1",
		projectID: "project-1",
		model:     "o3",
		toolCalls: make(map[string]*ToolCallStats),
	}

	a.StartSession("session-r")

	a.RecordModelEnd(100, 50, 0, 200)
	a.RecordModelEnd(100, 50, 0, 300)

	summary := a.Finalize(0, 0, 0, 0, "")

	if summary.TokensReasoning != 500 {
		t.Errorf("expected 500 reasoning tokens, got %d", summary.TokensReasoning)
	}
	if summary.TokensInput != 200 {
		t.Errorf("expected 200 input tokens, got %d", summary.TokensInput)
	}
}

func TestAggregator_FinalizeGroupGateOverride(t *testing.T) {
	// O2: When session-end carries authoritative totals, all values (including
	// legitimate zeros) should override the running accumulation.
	a := &Aggregator{
		agentID:   "agent-1",
		projectID: "project-1",
		toolCalls: make(map[string]*ToolCallStats),
	}

	a.StartSession("session-gate")
	a.RecordModelEnd(1000, 200, 500, 100)

	// Session-end reports totals with 0 cached — the 0 should override 500.
	summary := a.Finalize(5000, 1200, 0, 0, "")

	if summary.TokensInput != 5000 {
		t.Errorf("expected 5000 input tokens, got %d", summary.TokensInput)
	}
	if summary.TokensOutput != 1200 {
		t.Errorf("expected 1200 output tokens, got %d", summary.TokensOutput)
	}
	if summary.TokensCached != 0 {
		t.Errorf("expected 0 cached tokens (legitimate override), got %d", summary.TokensCached)
	}
	if summary.TokensReasoning != 0 {
		t.Errorf("expected 0 reasoning tokens (legitimate override), got %d", summary.TokensReasoning)
	}
}

func TestAggregator_Concurrent(t *testing.T) {
	a := &Aggregator{
		agentID:   "agent-1",
		projectID: "project-1",
		model:     "claude-4",
		toolCalls: make(map[string]*ToolCallStats),
	}

	a.StartSession("session-concurrent")

	const goroutines = 50
	const opsPerGoroutine = 100

	var wg sync.WaitGroup
	wg.Add(goroutines * 2) // half tool, half model

	// Half the goroutines call RecordToolEnd
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < opsPerGoroutine; j++ {
				a.RecordToolEnd("read_file", "")
			}
		}()
	}

	// Half the goroutines call RecordModelEnd
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < opsPerGoroutine; j++ {
				a.RecordModelEnd(10, 5, 2, 1)
			}
		}()
	}

	wg.Wait()

	summary := a.Finalize(0, 0, 0, 0, "")

	expectedToolCalls := goroutines * opsPerGoroutine
	if rf, ok := summary.ToolCalls["read_file"]; !ok {
		t.Fatal("expected read_file tool stats")
	} else if rf.Calls != expectedToolCalls {
		t.Errorf("expected %d tool calls, got %d", expectedToolCalls, rf.Calls)
	} else if rf.Success != expectedToolCalls {
		t.Errorf("expected %d successful calls, got %d", expectedToolCalls, rf.Success)
	}

	expectedInput := int64(goroutines * opsPerGoroutine * 10)
	expectedOutput := int64(goroutines * opsPerGoroutine * 5)
	expectedCached := int64(goroutines * opsPerGoroutine * 2)
	expectedReasoning := int64(goroutines * opsPerGoroutine * 1)

	if summary.TokensInput != expectedInput {
		t.Errorf("expected %d input tokens, got %d", expectedInput, summary.TokensInput)
	}
	if summary.TokensOutput != expectedOutput {
		t.Errorf("expected %d output tokens, got %d", expectedOutput, summary.TokensOutput)
	}
	if summary.TokensCached != expectedCached {
		t.Errorf("expected %d cached tokens, got %d", expectedCached, summary.TokensCached)
	}
	if summary.TokensReasoning != expectedReasoning {
		t.Errorf("expected %d reasoning tokens, got %d", expectedReasoning, summary.TokensReasoning)
	}
}

func newTestAggregator() *Aggregator {
	return &Aggregator{
		agentID:   "agent-1",
		projectID: "project-1",
		toolCalls: make(map[string]*ToolCallStats),
	}
}

// A missed session-start must not lose the session: the ID carried by later
// events (including session-end) is adopted and the start time is set.
func TestAggregator_MissedSessionStart(t *testing.T) {
	a := newTestAggregator()

	a.ObserveSession("session-late")
	a.RecordToolEnd("read_file", "")
	a.ObserveSession("session-late")
	a.RecordModelEnd(100, 20, 0, 0)
	a.ObserveSession("session-late")
	summary := a.Finalize(0, 0, 0, 0, "")

	if summary.SessionID != "session-late" {
		t.Errorf("SessionID = %q, want session-late", summary.SessionID)
	}
	if summary.StartedAt.IsZero() {
		t.Error("StartedAt is zero, want the time of the first observed event")
	}
	if summary.EndedAt.Before(summary.StartedAt) {
		t.Errorf("EndedAt %v before StartedAt %v", summary.EndedAt, summary.StartedAt)
	}
	if summary.APICallCount != 1 || summary.TokensInput != 100 {
		t.Errorf("api calls=%d input=%d, want 1/100", summary.APICallCount, summary.TokensInput)
	}
	if summary.ToolCalls["read_file"].Calls != 1 {
		t.Errorf("read_file calls = %d, want 1", summary.ToolCalls["read_file"].Calls)
	}
}

// Only the session-end event carries the ID: it must still be used.
func TestAggregator_SessionIDOnlyOnSessionEnd(t *testing.T) {
	a := newTestAggregator()

	a.ObserveSession("")
	a.RecordModelEnd(10, 5, 0, 0)
	a.ObserveSession("session-end-only")
	summary := a.Finalize(0, 0, 0, 0, "")

	if summary.SessionID != "session-end-only" {
		t.Errorf("SessionID = %q, want session-end-only", summary.SessionID)
	}
	if summary.TokensInput != 10 {
		t.Errorf("TokensInput = %d, want 10", summary.TokensInput)
	}
}

// A late session-start for the lazily opened session keeps the counters
// gathered so far and does not count them twice.
func TestAggregator_LateSessionStartAfterLazyOpen(t *testing.T) {
	a := newTestAggregator()

	a.ObserveSession("s1")
	a.RecordModelEnd(100, 10, 0, 0)
	a.RecordTurn()
	started := a.startedAt

	a.StartSession("s1")
	a.ObserveSession("s1")
	a.RecordModelEnd(50, 5, 0, 0)
	summary := a.Finalize(0, 0, 0, 0, "")

	if summary.SessionID != "s1" {
		t.Errorf("SessionID = %q, want s1", summary.SessionID)
	}
	if summary.APICallCount != 2 || summary.TokensInput != 150 || summary.TokensOutput != 15 {
		t.Errorf("api=%d in=%d out=%d, want 2/150/15", summary.APICallCount, summary.TokensInput, summary.TokensOutput)
	}
	if summary.TurnCount != 1 {
		t.Errorf("TurnCount = %d, want 1", summary.TurnCount)
	}
	if !summary.StartedAt.Equal(started) {
		t.Errorf("StartedAt = %v, want lazy start %v", summary.StartedAt, started)
	}
}

// A session-start with a different ID begins a new session as before.
func TestAggregator_SessionStartWithNewIDResets(t *testing.T) {
	a := newTestAggregator()

	a.ObserveSession("old")
	a.RecordModelEnd(100, 10, 0, 0)
	a.StartSession("new")
	summary := a.Finalize(0, 0, 0, 0, "")

	if summary.SessionID != "new" {
		t.Errorf("SessionID = %q, want new", summary.SessionID)
	}
	if summary.APICallCount != 0 || summary.TokensInput != 0 {
		t.Errorf("api=%d in=%d, want counters reset", summary.APICallCount, summary.TokensInput)
	}
}

// After a session ends, events from a following session whose start was
// missed open a fresh session instead of carrying old counters over.
func TestAggregator_LazyOpenAfterFinalizeResets(t *testing.T) {
	a := newTestAggregator()

	a.StartSession("first")
	a.RecordModelEnd(100, 10, 0, 0)
	a.Finalize(0, 0, 0, 0, "")

	a.ObserveSession("second")
	a.RecordModelEnd(7, 1, 0, 0)
	a.ObserveSession("second")
	summary := a.Finalize(0, 0, 0, 0, "")

	if summary.SessionID != "second" {
		t.Errorf("SessionID = %q, want second", summary.SessionID)
	}
	if summary.APICallCount != 1 || summary.TokensInput != 7 {
		t.Errorf("api=%d in=%d, want 1/7", summary.APICallCount, summary.TokensInput)
	}
}

// Observing events inside a normally started session changes nothing.
func TestAggregator_ObserveWithinStartedSession(t *testing.T) {
	a := newTestAggregator()

	a.StartSession("s1")
	started := a.startedAt
	a.ObserveSession("s1")
	a.RecordModelEnd(1, 1, 0, 0)
	a.ObserveSession("other")
	summary := a.Finalize(0, 0, 0, 0, "")

	if summary.SessionID != "s1" || !summary.StartedAt.Equal(started) {
		t.Errorf("got id=%q started=%v, want s1/%v", summary.SessionID, summary.StartedAt, started)
	}
	if summary.APICallCount != 1 {
		t.Errorf("APICallCount = %d, want 1", summary.APICallCount)
	}
}
