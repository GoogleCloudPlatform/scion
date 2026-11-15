/*
Copyright 2026 The Scion Authors.
*/

package handlers

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// turnMarkReplay is the result of replaying one fixture record through the
// dialect, the StatusHandler and the TurnMarkHandler, in hook order.
type turnMarkReplay struct {
	event   *hooks.Event
	at      time.Time // the clock when the record was handled
	turnAt  time.Time // turn mark after the record; zero when none
	pending PendingHarnessError
}

// replayTurnMarks replays a captured fixture (see harness_error_fixtures_test.go)
// through harness's runtime dialect, the StatusHandler and the
// TurnMarkHandler. Every file is in t.TempDir; the clock advances one second
// per record so the marks can be ordered.
func replayTurnMarks(t *testing.T, harness, fixture string) []turnMarkReplay {
	t.Helper()
	d := mappingDialect(t, harness)
	status := newTempStatusHandler(t)
	writeAgentInfoJSON(t, status, `{"phase":"running","activity":"working","detail":{"message":"Harness started"}}`)
	dir := t.TempDir()
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	marks := &TurnMarkHandler{
		TurnEventsPath:   filepath.Join(dir, "scion-turn-events"),
		PendingErrorPath: filepath.Join(dir, "scion-last-harness-error.json"),
		Now:              func() time.Time { return now },
	}
	var out []turnMarkReplay
	for _, rec := range loadErrorFixture(t, harness, fixture) {
		event, err := d.Parse(rec)
		require.NoError(t, err)
		if event.Name == hooks.EventSessionEnd {
			continue
		}
		now = now.Add(time.Second)
		require.NoError(t, status.Handle(event))
		require.NoError(t, marks.Handle(event))
		r := turnMarkReplay{event: event, at: now}
		r.turnAt, _ = ReadLastTurnEventAt(marks.TurnEventsPath)
		r.pending, _ = ReadPendingHarnessError(marks.PendingErrorPath)
		out = append(out, r)
	}
	return out
}

// lastWith returns the last replay step whose event is name, failing when
// there is none.
func lastWith(t *testing.T, steps []turnMarkReplay, name string) turnMarkReplay {
	t.Helper()
	for i := len(steps) - 1; i >= 0; i-- {
		if steps[i].event.Name == name {
			return steps[i]
		}
	}
	t.Fatalf("no %s event in the replay", name)
	return turnMarkReplay{}
}

// muse-code 1.4.20: PostLLMCall with the 400 maps to an errored model-end
// (B2a), which writes the pending error, and no turn event follows it, so
// the first-turn watchdog sees a turn that ended on a model error.
func TestTurnMarksFixture_MuseModelEndErrorWritesPending(t *testing.T) {
	steps := replayTurnMarks(t, "muse-code", "api-error-400-1.4.20.jsonl")
	end := lastWith(t, steps, hooks.EventModelEnd)
	require.NotEmpty(t, end.event.Data.Error)
	assert.False(t, IsTurnEvent(end.event))
	assert.Equal(t, hooks.EventModelEnd, end.pending.Event)
	assert.True(t, end.pending.At.Equal(end.at))
	assert.Equal(t, "model API: API error 400: Permission 'aiplatform.endpoints.predict' denied on resource (or it may not exist). (invalid_request_error)",
		end.pending.Reason)

	final := steps[len(steps)-1]
	assert.True(t, final.pending.At.After(final.turnAt),
		"the turn must end on the model error: pending %v, last turn mark %v", final.pending.At, final.turnAt)
}

// copilot 1.0.112: errorOccurred (errorContext model_call) maps to an
// uncounted, errored model-end (B2a). It writes the pending error and is
// not a turn event.
func TestTurnMarksFixture_CopilotErrorOccurredWritesPending(t *testing.T) {
	steps := replayTurnMarks(t, "copilot", "api-error-400-1.0.112.jsonl")
	end := lastWith(t, steps, hooks.EventModelEnd)
	require.True(t, end.event.Data.Uncounted)
	assert.False(t, IsTurnEvent(end.event))
	assert.Equal(t, hooks.EventModelEnd, end.pending.Event)
	assert.Contains(t, end.pending.Reason, "model API: ")
	assert.Contains(t, end.pending.Reason, "Permission 'aiplatform.endpoints.predict' denied")
	prompt := lastWith(t, steps, hooks.EventPromptSubmit)
	assert.True(t, end.turnAt.Equal(prompt.at), "errorOccurred moved the turn mark")
	assert.True(t, end.pending.At.After(end.turnAt))
}

// copilot retried a 503 to success: the errorOccurred records a pending
// error, and the error-free agentStop that follows is a turn event, so the
// turn did not end on the model error.
func TestTurnMarksFixture_CopilotRetriedErrorThenStop(t *testing.T) {
	steps := replayTurnMarks(t, "copilot", "transient-503-then-200-1.0.112.jsonl")
	end := lastWith(t, steps, hooks.EventModelEnd)
	assert.False(t, IsTurnEvent(end.event))
	assert.Contains(t, end.pending.Reason, "currently unavailable")
	stop := lastWith(t, steps, hooks.EventAgentEnd)
	assert.True(t, IsTurnEvent(stop.event))
	assert.True(t, stop.turnAt.After(stop.pending.At))
}

// grok-build 1.0.77: the agent_error Notification is discarded by its
// dialect (EventIgnored) and the StopFailure is agent-error (B1's hook
// path). Neither is a turn event nor a pending error; only the prompts move
// the turn mark.
func TestTurnMarksFixture_GrokIgnoredNotificationIsNotProgress(t *testing.T) {
	steps := replayTurnMarks(t, "grok-build", "api-error-400-1.0.77.jsonl")
	sawIgnored, sawAgentError := false, false
	var lastPrompt time.Time
	for _, s := range steps {
		switch s.event.Name {
		case hooks.EventIgnored:
			sawIgnored = true
		case hooks.EventAgentError:
			sawAgentError = true
		case hooks.EventPromptSubmit:
			lastPrompt = s.at
		}
		if s.event.Name == hooks.EventIgnored || s.event.Name == hooks.EventAgentError {
			assert.False(t, IsTurnEvent(s.event), s.event.Name)
			assert.True(t, s.turnAt.Equal(lastPrompt), "%s moved the turn mark", s.event.Name)
		}
		assert.True(t, s.pending.At.IsZero(), "%s wrote a pending error", s.event.Name)
	}
	assert.True(t, sawIgnored, "the agent_error Notification was not ignored")
	assert.True(t, sawAgentError, "the StopFailure did not map to agent-error")
}

func TestIsTurnEvent_UncountedAndIgnored(t *testing.T) {
	assert.False(t, IsTurnEvent(&hooks.Event{Name: hooks.EventModelEnd, Data: hooks.EventData{Uncounted: true}}),
		"an uncounted model-end is not progress even without error text")
	assert.True(t, IsTurnEvent(&hooks.Event{Name: hooks.EventModelEnd}))
	assert.False(t, IsTurnEvent(&hooks.Event{Name: hooks.EventIgnored}))
	assert.False(t, IsTurnEvent(&hooks.Event{Name: hooks.EventAgentError}))
}
