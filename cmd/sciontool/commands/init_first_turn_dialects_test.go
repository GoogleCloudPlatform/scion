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

package commands

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/handlers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// capturedHookPayloads returns the captured bridge payloads in rel, keyed by
// hook_event_name; every payload of a name is kept, in order.
func capturedHookPayloads(t *testing.T, rel string) map[string][][]byte {
	t.Helper()
	f, err := os.Open(repoPath(t, rel))
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	out := map[string][][]byte{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		if len(line) == 0 {
			continue
		}
		var ev struct {
			Name string `json:"hook_event_name"`
		}
		require.NoError(t, json.Unmarshal(line, &ev))
		out[ev.Name] = append(out[ev.Name], line)
	}
	require.NoError(t, sc.Err())
	return out
}

// firstTurnDialectFixture runs real `sciontool hook` processing with a
// harness's staged dialect.yaml and judges the first-turn watchdog against
// the marker files it wrote. HOME, the marker files and the log are in
// t.TempDir; no Hub is configured.
func firstTurnDialectFixture(t *testing.T, dialect string) (send func([]byte), judge func() (string, bool)) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	scrubScionEnv(t)
	setTestLogPath(t, filepath.Join(home, "agent.log"))
	oldDialect, oldStdout := hookDialect, hookStdout
	oldTurn, oldPending := turnEventsFilePath, pendingHarnessErrorFilePath
	hookDialect, hookStdout = dialect, io.Discard
	turnEventsFilePath = filepath.Join(home, "scion-turn-events")
	pendingHarnessErrorFilePath = filepath.Join(home, "scion-last-harness-error.json")
	t.Cleanup(func() {
		hookDialect, hookStdout = oldDialect, oldStdout
		turnEventsFilePath, pendingHarnessErrorFilePath = oldTurn, oldPending
	})
	spec, err := os.ReadFile(repoPath(t, "harnesses/"+dialect+"/dialect.yaml"))
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".scion", "harness"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".scion", "harness", "dialect.yaml"), spec, 0o644))

	launched := time.Now().Add(-time.Second)
	turnPath, pendingPath := turnEventsFilePath, pendingHarnessErrorFilePath
	cfg := harnessWatchdogConfig{
		LaunchedAt:      launched,
		LastTurnEventAt: func() (time.Time, bool) { return handlers.ReadLastTurnEventAt(turnPath) },
		PendingError:    func() (handlers.PendingHarnessError, bool) { return handlers.ReadPendingHarnessError(pendingPath) },
		PaneTail:        func(context.Context) string { return "pane" },
	}
	send = func(payload []byte) {
		t.Helper()
		require.NoError(t, processHookData(payload))
	}
	judge = func() (string, bool) { return firstTurnReason(context.Background(), cfg) }
	return send, judge
}

// Review R2: OpenCode sends no turn-start event for the initial --prompt.
// An error-free step-finish (model-end) or session.idle (agent-end) proves
// the task started.
func TestFirstTurnWatchdog_OpenCode(t *testing.T) {
	p := capturedHookPayloads(t, "pkg/sciontool/hooks/dialects/testdata/opencode/hook-payloads-1.18.33.jsonl")

	t.Run("text-only first turn", func(t *testing.T) {
		send, judge := firstTurnDialectFixture(t, "opencode")
		send(p["session.created"][0])
		send(p["message.part.updated.step-finish"][0])
		send(p["session.idle"][0])
		_, report := judge()
		assert.False(t, report)
	})
	t.Run("first step finishes before 3 min, first tool call after", func(t *testing.T) {
		send, judge := firstTurnDialectFixture(t, "opencode")
		send(p["session.created"][0])
		send(p["message.part.updated.step-finish"][0])
		_, report := judge() // the 3-minute check
		assert.False(t, report)
		send(p["tool.execute.before"][0])
	})
	t.Run("first tool call before 3 min", func(t *testing.T) {
		send, judge := firstTurnDialectFixture(t, "opencode")
		send(p["session.created"][0])
		send(p["tool.execute.before"][0])
		_, report := judge()
		assert.False(t, report)
	})
	// Residual: a single first LLM step that runs past 3 min before any
	// tool call or step-finish sends OpenCode nothing to observe.
	t.Run("nothing after session.created reports", func(t *testing.T) {
		send, judge := firstTurnDialectFixture(t, "opencode")
		send(p["session.created"][0])
		reason, report := judge()
		assert.True(t, report)
		assert.Equal(t, "harness did not start the initial task: pane", reason)
	})
	t.Run("a failed first turn is not progress", func(t *testing.T) {
		send, judge := firstTurnDialectFixture(t, "opencode")
		send(p["session.created"][0])
		send([]byte(`{"hook_event_name":"session.idle","session_id":"s","error":"APIError (status 400)"}`))
		_, report := judge()
		assert.True(t, report, "an errored session.idle must not count as the task starting")
	})
}

// Review R2: pi's prompt-submit is human/RPC input only; the initial
// positional prompt may send none. Its agent_end, or a tool call, proves the
// task started.
func TestFirstTurnWatchdog_Pi(t *testing.T) {
	p := capturedHookPayloads(t, "pkg/sciontool/hooks/dialects/testdata/pi/bridge-payloads.jsonl")
	var okEnd, errEnd []byte
	for _, payload := range p["agent_end"] {
		var ev struct {
			Error string `json:"error"`
		}
		require.NoError(t, json.Unmarshal(payload, &ev))
		if ev.Error == "" {
			okEnd = payload
		} else {
			errEnd = payload
		}
	}
	require.NotNil(t, okEnd)
	require.NotNil(t, errEnd)

	t.Run("text-only first run", func(t *testing.T) {
		send, judge := firstTurnDialectFixture(t, "pi")
		send(p["session_start"][0])
		send(okEnd)
		_, report := judge()
		assert.False(t, report)
	})
	t.Run("first tool call", func(t *testing.T) {
		send, judge := firstTurnDialectFixture(t, "pi")
		send(p["session_start"][0])
		send(p["tool_execution_start"][0])
		_, report := judge()
		assert.False(t, report)
	})
	t.Run("a failed first run is not progress", func(t *testing.T) {
		send, judge := firstTurnDialectFixture(t, "pi")
		send(p["session_start"][0])
		send(errEnd)
		_, report := judge()
		assert.True(t, report)
	})
}

// Review R1 (final): pi's first model step can outlast firstTurnTimeout
// with nothing to observe, so the watchdog reports. The report is the
// watchdog's own and is retracted by the first turn event after it: the
// error-free agent_end of a text-only turn (which the hook path does not let
// clear harness_error), or a tool call (which the hook path clears itself).
// Real `sciontool hook` processing with pi's dialect; HOME, agent-info.json
// and the marker files are in t.TempDir; no Hub.
func TestFirstTurnWatchdog_PiLongFirstStepRetracts(t *testing.T) {
	p := capturedHookPayloads(t, "pkg/sciontool/hooks/dialects/testdata/pi/bridge-payloads.jsonl")
	var okEnd []byte
	for _, payload := range p["agent_end"] {
		var ev struct {
			Error string `json:"error"`
		}
		require.NoError(t, json.Unmarshal(payload, &ev))
		if ev.Error == "" {
			okEnd = payload
		}
	}
	require.NotNil(t, okEnd)

	for _, tc := range []struct {
		name, wantActivity string
		progress           []byte
		wantRestore        bool
	}{
		{"text-only first turn ends", "working", okEnd, true},
		{"tool call after 3 min", "executing", p["tool_execution_start"][0], false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			send, _ := firstTurnDialectFixture(t, "pi")
			home := os.Getenv("HOME")
			status := &handlers.StatusHandler{StatusPath: filepath.Join(home, "agent-info.json")}
			send(p["session_start"][0])
			require.NoError(t, os.WriteFile(status.StatusPath, []byte(`{"phase":"running","activity":"working"}`), 0o644))

			turnPath := turnEventsFilePath
			var restoredTo []state.Activity
			ticks := make(chan time.Time, 1)
			cfg := harnessWatchdogConfig{
				LaunchedAt:              time.Now().Add(-4 * time.Minute),
				LastTurnEventAt:         func() (time.Time, bool) { return handlers.ReadLastTurnEventAt(turnPath) },
				PaneTail:                func(context.Context) string { return "pi> thinking" },
				LocalActivity:           status.CurrentActivity,
				SetHarnessError:         status.SetHarnessErrorIf,
				RestoreFromHarnessError: status.RestoreFromHarnessErrorIf,
				ReportActivity: func(_ context.Context, a state.Activity, msg string) error {
					assert.Equal(t, "Ready", msg)
					restoredTo = append(restoredTo, a)
					return nil
				},
				NewTicker: func(time.Duration) (<-chan time.Time, func()) { return ticks, func() {} },
			}

			// The 3-minute check: nothing since session_start.
			reason, report := firstTurnReason(context.Background(), cfg)
			require.True(t, report)
			reportedAt := time.Now()
			require.True(t, reportHarnessError(context.Background(), cfg, reason, isFirstTurnReplaceable))
			require.Equal(t, state.ActivityHarnessError, status.CurrentActivity())

			// The first step finishes after the report.
			time.Sleep(2 * time.Millisecond) // the marker's time must follow reportedAt
			send(tc.progress)

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			ticks <- time.Time{}
			watchFirstTurnRetraction(ctx, cfg, reason, reportedAt)
			require.NoError(t, ctx.Err(), "the retraction watch did not end")

			assert.Equal(t, tc.wantActivity, string(status.CurrentActivity()))
			if tc.wantRestore {
				assert.Equal(t, []state.Activity{state.ActivityWorking}, restoredTo)
			} else {
				assert.Empty(t, restoredTo, "the hook path already cleared it")
			}
		})
	}
}

// copilot sends nothing for a successful model call. A transient 503 that
// copilot retries to success early in the first turn writes a pending
// model-end error newer than the prompt, so with no tool call before the
// 3-minute check the watchdog reports. The report is retracted at the
// turn's error-free agentStop (or a tool call). Captured copilot 1.0.112
// payloads through real `sciontool hook` processing with copilot's
// dialect; HOME, agent-info.json and the marker files are in t.TempDir; no
// Hub.
func TestFirstTurnWatchdog_CopilotRetriedErrorRetractsOnStop(t *testing.T) {
	p := capturedHookPayloads(t, "pkg/sciontool/hooks/dialects/testdata/copilot/transient-503-then-200-1.0.112.jsonl")
	send, _ := firstTurnDialectFixture(t, "copilot")
	status := &handlers.StatusHandler{StatusPath: filepath.Join(os.Getenv("HOME"), "agent-info.json")}
	launched := time.Now().Add(-time.Second)
	send(p["sessionStart"][0])
	require.NoError(t, os.WriteFile(status.StatusPath, []byte(`{"phase":"running","activity":"working"}`), 0o644))
	send(p["userPromptSubmitted"][0])
	time.Sleep(2 * time.Millisecond) // the pending error must follow the prompt's mark
	send(p["errorOccurred"][0])

	turnPath, pendingPath := turnEventsFilePath, pendingHarnessErrorFilePath
	var reported []state.Activity
	ticks := make(chan time.Time, 1)
	cfg := harnessWatchdogConfig{
		LaunchedAt:              launched,
		LastTurnEventAt:         func() (time.Time, bool) { return handlers.ReadLastTurnEventAt(turnPath) },
		PendingError:            func() (handlers.PendingHarnessError, bool) { return handlers.ReadPendingHarnessError(pendingPath) },
		LocalActivity:           status.CurrentActivity,
		SetHarnessError:         status.SetHarnessErrorIf,
		RestoreFromHarnessError: status.RestoreFromHarnessErrorIf,
		ReportActivity: func(_ context.Context, a state.Activity, msg string) error {
			assert.Equal(t, "Ready", msg)
			reported = append(reported, a)
			return nil
		},
		NewTicker: func(time.Duration) (<-chan time.Time, func()) { return ticks, func() {} },
	}

	// The 3-minute check: the turn so far ended on the 503.
	reason, report := firstTurnReason(context.Background(), cfg)
	require.True(t, report)
	assert.Contains(t, reason, "harness did not start the initial task: model API: ")
	assert.Contains(t, reason, "currently unavailable")
	reportedAt := time.Now()
	require.True(t, reportHarnessError(context.Background(), cfg, reason, isFirstTurnReplaceable))

	time.Sleep(2 * time.Millisecond)
	send(p["agentStop"][0])
	require.Equal(t, state.ActivityHarnessError, status.CurrentActivity(), "agentStop alone does not clear harness_error")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ticks <- time.Time{}
	watchFirstTurnRetraction(ctx, cfg, reason, reportedAt)
	require.NoError(t, ctx.Err(), "the retraction watch did not end")
	assert.Equal(t, state.ActivityWorking, status.CurrentActivity())
	assert.Equal(t, []state.Activity{state.ActivityWorking}, reported)
}

// copilot's fatal 400: errorOccurred and no agentStop, so the report stands.
func TestFirstTurnWatchdog_CopilotFatalErrorReports(t *testing.T) {
	p := capturedHookPayloads(t, "pkg/sciontool/hooks/dialects/testdata/copilot/api-error-400-1.0.112.jsonl")
	send, judge := firstTurnDialectFixture(t, "copilot")
	send(p["sessionStart"][0])
	send(p["userPromptSubmitted"][0])
	time.Sleep(2 * time.Millisecond)
	send(p["errorOccurred"][0])
	reason, report := judge()
	require.True(t, report)
	assert.Contains(t, reason, "Permission 'aiplatform.endpoints.predict' denied")
}
