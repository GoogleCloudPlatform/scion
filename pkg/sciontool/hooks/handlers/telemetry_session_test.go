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

package handlers

import (
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/telemetry"
)

func sessionEvent(name, sessionID string) *hooks.Event {
	return &hooks.Event{Name: name, Data: hooks.EventData{SessionID: sessionID}}
}

// When the session-start hook is missed, the session ID carried by later
// events must still reach the session summary sent to the Hub.
func TestTelemetryHandler_SessionSummaryWithoutSessionStart(t *testing.T) {
	h := NewTelemetryHandler(nil, nil, nil)

	var got []telemetry.SessionSummary
	h.OnSessionEnd = func(s telemetry.SessionSummary) { got = append(got, s) }

	model := sessionEvent(hooks.EventModelEnd, "sess-1")
	model.Data.InputTokens = 40
	model.Data.OutputTokens = 4
	tool := sessionEvent(hooks.EventToolEnd, "sess-1")
	tool.Data.ToolName = "Bash"

	for _, ev := range []*hooks.Event{
		model,
		tool,
		sessionEvent(hooks.EventAgentEnd, "sess-1"),
		sessionEvent(hooks.EventSessionEnd, "sess-1"),
	} {
		if err := h.Handle(ev); err != nil {
			t.Fatalf("Handle(%s): %v", ev.Name, err)
		}
	}

	if len(got) != 1 {
		t.Fatalf("OnSessionEnd called %d times, want 1", len(got))
	}
	s := got[0]
	if s.SessionID != "sess-1" {
		t.Errorf("SessionID = %q, want sess-1", s.SessionID)
	}
	if s.StartedAt.IsZero() {
		t.Error("StartedAt is zero, want time of first event")
	}
	if s.TokensInput != 40 || s.APICallCount != 1 || s.TurnCount != 1 {
		t.Errorf("in=%d api=%d turns=%d, want 40/1/1", s.TokensInput, s.APICallCount, s.TurnCount)
	}
	if s.ToolCalls["Bash"].Calls != 1 {
		t.Errorf("Bash calls = %d, want 1", s.ToolCalls["Bash"].Calls)
	}
}

// A session-start that arrives after other events of the same session must
// not drop or double-count what was already recorded.
func TestTelemetryHandler_LateSessionStart(t *testing.T) {
	h := NewTelemetryHandler(nil, nil, nil)

	var got []telemetry.SessionSummary
	h.OnSessionEnd = func(s telemetry.SessionSummary) { got = append(got, s) }

	m1 := sessionEvent(hooks.EventModelEnd, "sess-2")
	m1.Data.InputTokens = 10
	m2 := sessionEvent(hooks.EventModelEnd, "sess-2")
	m2.Data.InputTokens = 5

	for _, ev := range []*hooks.Event{
		m1,
		sessionEvent(hooks.EventSessionStart, "sess-2"),
		m2,
		sessionEvent(hooks.EventSessionEnd, "sess-2"),
	} {
		if err := h.Handle(ev); err != nil {
			t.Fatalf("Handle(%s): %v", ev.Name, err)
		}
	}

	if len(got) != 1 {
		t.Fatalf("OnSessionEnd called %d times, want 1", len(got))
	}
	if got[0].SessionID != "sess-2" || got[0].TokensInput != 15 || got[0].APICallCount != 2 {
		t.Errorf("got id=%q in=%d api=%d, want sess-2/15/2", got[0].SessionID, got[0].TokensInput, got[0].APICallCount)
	}
}

// The normal flow, with session-start first, is unchanged.
func TestTelemetryHandler_SessionSummaryWithSessionStart(t *testing.T) {
	h := NewTelemetryHandler(nil, nil, nil)

	var got []telemetry.SessionSummary
	h.OnSessionEnd = func(s telemetry.SessionSummary) { got = append(got, s) }

	m := sessionEvent(hooks.EventModelEnd, "sess-3")
	m.Data.InputTokens = 7
	for _, ev := range []*hooks.Event{
		sessionEvent(hooks.EventSessionStart, "sess-3"),
		m,
		sessionEvent(hooks.EventSessionEnd, "sess-3"),
	} {
		if err := h.Handle(ev); err != nil {
			t.Fatalf("Handle(%s): %v", ev.Name, err)
		}
	}

	if len(got) != 1 || got[0].SessionID != "sess-3" || got[0].TokensInput != 7 || got[0].StartedAt.IsZero() {
		t.Fatalf("unexpected summaries: %+v", got)
	}
}

// Lifecycle events before session-start do not open a session, so the
// start time still comes from the session-start event.
func TestTelemetryHandler_LifecycleEventsDoNotOpenSession(t *testing.T) {
	h := NewTelemetryHandler(nil, nil, nil)

	var got []telemetry.SessionSummary
	h.OnSessionEnd = func(s telemetry.SessionSummary) { got = append(got, s) }

	for _, ev := range []*hooks.Event{
		sessionEvent(hooks.EventPreStart, ""),
		sessionEvent(hooks.EventNotification, "sess-4"),
	} {
		if err := h.Handle(ev); err != nil {
			t.Fatalf("Handle(%s): %v", ev.Name, err)
		}
	}
	before := time.Now()
	for _, ev := range []*hooks.Event{
		sessionEvent(hooks.EventSessionStart, "sess-4"),
		sessionEvent(hooks.EventSessionEnd, "sess-4"),
	} {
		if err := h.Handle(ev); err != nil {
			t.Fatalf("Handle(%s): %v", ev.Name, err)
		}
	}

	if len(got) != 1 || got[0].SessionID != "sess-4" {
		t.Fatalf("unexpected summaries: %+v", got)
	}
	if got[0].StartedAt.Before(before) {
		t.Errorf("StartedAt %v precedes session-start (%v)", got[0].StartedAt, before)
	}
}

// Only session-end carries the session ID: observing session-end must
// put that ID on the summary.
func TestTelemetryHandler_SessionIDOnlyOnSessionEnd(t *testing.T) {
	h := NewTelemetryHandler(nil, nil, nil)

	var got []telemetry.SessionSummary
	h.OnSessionEnd = func(s telemetry.SessionSummary) { got = append(got, s) }

	model := sessionEvent(hooks.EventModelEnd, "")
	model.Data.InputTokens = 9
	for _, ev := range []*hooks.Event{
		model,
		sessionEvent(hooks.EventToolEnd, ""),
		sessionEvent(hooks.EventAgentEnd, ""),
		sessionEvent(hooks.EventSessionEnd, "sess-5"),
	} {
		if err := h.Handle(ev); err != nil {
			t.Fatalf("Handle(%s): %v", ev.Name, err)
		}
	}

	if len(got) != 1 {
		t.Fatalf("OnSessionEnd called %d times, want 1", len(got))
	}
	if got[0].SessionID != "sess-5" || got[0].TokensInput != 9 || got[0].APICallCount != 1 {
		t.Errorf("got id=%q in=%d api=%d, want sess-5/9/1", got[0].SessionID, got[0].TokensInput, got[0].APICallCount)
	}
}

// Claude repeats session-start with the same ID on /compact and on
// resume. Counts from before the repeat must reach the summary.
func TestTelemetryHandler_RepeatedSessionStartSameIDKeepsCounts(t *testing.T) {
	h := NewTelemetryHandler(nil, nil, nil)

	var got []telemetry.SessionSummary
	h.OnSessionEnd = func(s telemetry.SessionSummary) { got = append(got, s) }

	m1 := sessionEvent(hooks.EventModelEnd, "sess-6")
	m1.Data.InputTokens = 20
	m2 := sessionEvent(hooks.EventModelEnd, "sess-6")
	m2.Data.InputTokens = 3
	before := time.Now()
	for _, ev := range []*hooks.Event{
		sessionEvent(hooks.EventSessionStart, "sess-6"),
		m1,
		sessionEvent(hooks.EventAgentEnd, "sess-6"),
		sessionEvent(hooks.EventSessionStart, "sess-6"),
		m2,
		sessionEvent(hooks.EventSessionEnd, "sess-6"),
	} {
		if err := h.Handle(ev); err != nil {
			t.Fatalf("Handle(%s): %v", ev.Name, err)
		}
	}

	if len(got) != 1 {
		t.Fatalf("OnSessionEnd called %d times, want 1", len(got))
	}
	s := got[0]
	if s.SessionID != "sess-6" || s.TokensInput != 23 || s.APICallCount != 2 || s.TurnCount != 1 {
		t.Errorf("got id=%q in=%d api=%d turns=%d, want sess-6/23/2/1", s.SessionID, s.TokensInput, s.APICallCount, s.TurnCount)
	}
	if s.StartedAt.Before(before) || s.StartedAt.After(before.Add(time.Second)) {
		t.Errorf("StartedAt %v, want the first session-start (~%v)", s.StartedAt, before)
	}
}
