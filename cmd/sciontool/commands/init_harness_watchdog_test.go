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
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/handlers"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/telemetry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Isolation: every test here uses a temp agent-info.json, temp marker
// files, an injected clock and tick channel, and either a recording Report
// or an in-process httptest Hub. Nothing touches /tmp/scion-*, tmux or a
// live Hub.

var watchdogLaunch = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

// watchdogFixture is a harnessWatchdogConfig over a real StatusHandler on
// a temp agent-info.json, a fake clock and recorded Hub reports.
type watchdogFixture struct {
	t       *testing.T
	status  *handlers.StatusHandler
	turnAt  time.Time
	now     time.Time
	live    bool
	mu      sync.Mutex
	reports []string
	cfg     harnessWatchdogConfig
}

func newWatchdogFixture(t *testing.T, activity string) *watchdogFixture {
	t.Helper()
	f := &watchdogFixture{t: t, now: watchdogLaunch}
	f.status = &handlers.StatusHandler{StatusPath: filepath.Join(t.TempDir(), "agent-info.json")}
	f.writeActivity(activity)
	f.cfg = harnessWatchdogConfig{
		LaunchedAt: watchdogLaunch,
		Now:        func() time.Time { return f.now },
		Outcomes:   &apiOutcomeTracker{},
		LiveWork:   func() bool { return f.live },
		LastTurnEventAt: func() (time.Time, bool) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.turnAt, !f.turnAt.IsZero()
		},
		LocalActivity:   f.status.CurrentActivity,
		SetHarnessError: f.status.SetHarnessErrorIf,
		Report: func(_ context.Context, reason string) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.reports = append(f.reports, reason)
			return nil
		},
	}
	return f
}

func (f *watchdogFixture) writeActivity(activity string) {
	f.t.Helper()
	body := `{"phase":"running","activity":"` + activity + `","detail":{"message":"Ready"}}`
	require.NoError(f.t, os.WriteFile(f.status.StatusPath, []byte(body), 0o644))
}

func (f *watchdogFixture) outcome(after time.Duration, ok bool, status int, message string) {
	f.cfg.Outcomes.Observe(telemetry.APIOutcome{At: watchdogLaunch.Add(after), OK: ok, Status: status, Message: message})
}

func (f *watchdogFixture) modelOutcome(after time.Duration, model string, ok bool, status int, message string) {
	f.cfg.Outcomes.Observe(telemetry.APIOutcome{At: watchdogLaunch.Add(after), Model: model, OK: ok, Status: status, Message: message})
}

// turnEnd records a status write at launch+at that left the agent working:
// the turn end a completion hook (Stop, AfterAgent) writes.
func (f *watchdogFixture) turnEnd(at time.Duration) {
	f.writeActivity("working")
	changed := watchdogLaunch.Add(at)
	f.cfg.LastStatusChangeAt = func() (time.Time, bool) { return changed, true }
}

// check runs one API-error watchdog decision at launch+at and, when it
// fires, the local write and Hub report, as one tick does.
func (f *watchdogFixture) check(at time.Duration, reported *time.Time) bool {
	f.now = watchdogLaunch.Add(at)
	reason, failureAt, ok := apiErrorReason(f.cfg, f.now, *reported)
	if !ok {
		return false
	}
	if reportHarnessError(context.Background(), f.cfg, reason, isAPIErrorReplaceable) {
		*reported = failureAt
		return true
	}
	return false
}

func (f *watchdogFixture) info() map[string]interface{} {
	f.t.Helper()
	data, err := os.ReadFile(f.status.StatusPath)
	require.NoError(f.t, err)
	var info map[string]interface{}
	require.NoError(f.t, json.Unmarshal(data, &info))
	return info
}

func (f *watchdogFixture) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reports...)
}

func infoMessage(info map[string]interface{}) string {
	detail, _ := info["detail"].(map[string]interface{})
	msg, _ := detail["message"].(string)
	return msg
}

func TestAPIErrorWatchdog_NonRetryableFailureAfterGrace(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			f := newWatchdogFixture(t, "thinking")
			f.outcome(10*time.Second, false, status, "Permission 'aiplatform.endpoints.predict' denied")
			f.turnEnd(12 * time.Second)
			var reported time.Time

			assert.False(t, f.check(41*time.Second, &reported), "fired inside the grace period")
			assert.True(t, f.check(42*time.Second, &reported), "did not fire 30 s after the turn end")

			info := f.info()
			want := "model API " + itoa(status) + ": Permission 'aiplatform.endpoints.predict' denied"
			assert.Equal(t, "harness_error", info["activity"])
			assert.Equal(t, want, infoMessage(info))
			assert.Equal(t, []string{want}, f.recorded())

			// Reported once: later ticks for the same failure do nothing.
			assert.False(t, f.check(100*time.Second, &reported))
			assert.Len(t, f.recorded(), 1)
		})
	}
}

// Without a turn end after the failure the grace period is
// apiErrorThinkingGrace: a retry or the main model may still be streaming.
func TestAPIErrorWatchdog_NoTurnEndUsesLongGrace(t *testing.T) {
	for _, activity := range []string{"thinking", "working"} {
		t.Run(activity, func(t *testing.T) {
			f := newWatchdogFixture(t, activity)
			f.cfg.LastStatusChangeAt = func() (time.Time, bool) { return watchdogLaunch.Add(5 * time.Second), true }
			f.outcome(10*time.Second, false, 400, "bad request")
			var reported time.Time
			assert.False(t, f.check(129*time.Second, &reported))
			assert.True(t, f.check(130*time.Second, &reported))
		})
	}
	t.Run("a status write that leaves the agent thinking is not a turn end", func(t *testing.T) {
		f := newWatchdogFixture(t, "thinking")
		f.outcome(10*time.Second, false, 400, "bad request")
		f.cfg.LastStatusChangeAt = func() (time.Time, bool) { return watchdogLaunch.Add(12 * time.Second), true }
		var reported time.Time
		assert.False(t, f.check(60*time.Second, &reported))
		assert.True(t, f.check(132*time.Second, &reported))
	})
}

// Review R1: a failed call to another model while the main model streams
// a long response, and a failed request whose retry streams longer than
// 30 s, are not the agent's turn failing.
func TestAPIErrorWatchdog_HealthyTurnFalsePositives(t *testing.T) {
	t.Run("side-model 404 during a long main-model stream", func(t *testing.T) {
		f := newWatchdogFixture(t, "thinking")
		f.modelOutcome(10*time.Second, "claude-sonnet", true, 200, "")
		f.turnAt = watchdogLaunch.Add(15 * time.Second)
		f.modelOutcome(20*time.Second, "claude-haiku", false, 404, "model not found")
		var reported time.Time
		for at := 30 * time.Second; at <= 4*time.Minute; at += 10 * time.Second {
			require.False(t, f.check(at, &reported), "reported at +%s", at)
		}
		f.turnEnd(4 * time.Minute) // the main model finishes; Stop writes working
		assert.False(t, f.check(5*time.Minute, &reported))
		assert.Equal(t, "working", f.info()["activity"])
		assert.Empty(t, f.recorded())
	})
	t.Run("side-model 404 before anything succeeded, configured model known", func(t *testing.T) {
		f := newWatchdogFixture(t, "working")
		f.cfg.ConfiguredModel = "sonnet"
		f.modelOutcome(5*time.Second, "claude-sonnet-5", false, 503, "overloaded")
		f.modelOutcome(6*time.Second, "claude-haiku-4", false, 404, "model not found")
		var reported time.Time
		assert.False(t, f.check(4*time.Minute, &reported))
	})
	t.Run("sibling model 404 while the configured model streams", func(t *testing.T) {
		f := newWatchdogFixture(t, "thinking")
		f.cfg.ConfiguredModel = "gemini-2.5-flash"
		f.modelOutcome(5*time.Second, "gemini-2.5-flash", true, 200, "")
		f.modelOutcome(10*time.Second, "gemini-2.5-flash-lite", false, 404, "model not found")
		var reported time.Time
		assert.False(t, f.check(4*time.Minute, &reported))
		assert.Empty(t, f.recorded())
	})
	t.Run("400 then a 90 s streaming retry, then success", func(t *testing.T) {
		f := newWatchdogFixture(t, "thinking")
		f.modelOutcome(10*time.Second, "claude-sonnet", false, 400, "prompt is too long")
		var reported time.Time
		for at := 20 * time.Second; at < 100*time.Second; at += 10 * time.Second {
			require.False(t, f.check(at, &reported), "reported at +%s", at)
		}
		f.modelOutcome(100*time.Second, "claude-sonnet", true, 200, "")
		for at := 100 * time.Second; at <= 5*time.Minute; at += 10 * time.Second {
			require.False(t, f.check(at, &reported), "reported at +%s", at)
		}
		assert.Empty(t, f.recorded())
	})
	t.Run("the primary model failing is still reported despite a side success", func(t *testing.T) {
		f := newWatchdogFixture(t, "thinking")
		f.modelOutcome(5*time.Second, "claude-sonnet", true, 200, "")
		f.modelOutcome(6*time.Second, "claude-sonnet", true, 200, "")
		f.modelOutcome(20*time.Second, "claude-sonnet", false, 403, "denied")
		f.modelOutcome(21*time.Second, "claude-haiku", true, 200, "")
		f.turnEnd(22 * time.Second)
		var reported time.Time
		assert.True(t, f.check(52*time.Second, &reported))
		assert.Equal(t, []string{"model API 403: denied"}, f.recorded())
	})
	t.Run("the primary model failing before anything succeeded is reported", func(t *testing.T) {
		f := newWatchdogFixture(t, "thinking")
		f.cfg.ConfiguredModel = "gemini-pro-latest" // an alias no label matches
		f.modelOutcome(5*time.Second, "gemini-2.5-pro", false, 403, "denied")
		var reported time.Time
		assert.True(t, f.check(125*time.Second, &reported))
	})
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// Criterion 13: a retried transient error does not set harness_error.
func TestAPIErrorWatchdog_RetriedTransientErrorDoesNotFire(t *testing.T) {
	t.Run("503 then 200 in the same turn", func(t *testing.T) {
		f := newWatchdogFixture(t, "thinking")
		f.outcome(5*time.Second, false, 503, "overloaded")
		f.outcome(9*time.Second, true, 200, "")
		var reported time.Time
		for at := 30 * time.Second; at <= 5*time.Minute; at += 30 * time.Second {
			assert.False(t, f.check(at, &reported))
		}
		assert.Equal(t, "thinking", f.info()["activity"])
		assert.Empty(t, f.recorded())
	})
	for _, status := range []int{408, 409, 429, 500, 503, 0} {
		t.Run("retryable "+itoa(status)+" alone", func(t *testing.T) {
			f := newWatchdogFixture(t, "working")
			f.outcome(5*time.Second, false, status, "x")
			var reported time.Time
			assert.False(t, f.check(5*time.Minute, &reported))
		})
	}
	t.Run("400 then success within the grace period", func(t *testing.T) {
		f := newWatchdogFixture(t, "working")
		f.outcome(5*time.Second, false, 400, "bad request")
		f.outcome(20*time.Second, true, 200, "")
		var reported time.Time
		assert.False(t, f.check(5*time.Minute, &reported))
		assert.Empty(t, f.recorded())
	})
	t.Run("a success reported late but timed earlier does not hide the failure", func(t *testing.T) {
		f := newWatchdogFixture(t, "working")
		f.outcome(20*time.Second, false, 400, "bad request")
		f.outcome(5*time.Second, true, 200, "")
		var reported time.Time
		assert.True(t, f.check(3*time.Minute, &reported))
	})
}

// Harness sequences from B2a's real-CLI captures, where the OTel watchdog
// is the only detection.
func TestAPIErrorWatchdog_CapturedHarnessSequences(t *testing.T) {
	// codex 0.169.0: SessionStart, UserPromptSubmit (thinking), a failed
	// codex.api_request 400, then no Stop and no notify.
	// No turn end is ever seen, so the long grace applies: about 120 s.
	t.Run("codex: stuck thinking, no completion hook", func(t *testing.T) {
		f := newWatchdogFixture(t, "thinking")
		f.turnAt = watchdogLaunch.Add(10 * time.Second)
		f.cfg.LastStatusChangeAt = func() (time.Time, bool) { return watchdogLaunch.Add(10 * time.Second), true }
		f.outcome(11*time.Second, false, 400, "Permission denied")
		var reported time.Time
		assert.False(t, f.check(130*time.Second, &reported))
		assert.True(t, f.check(131*time.Second, &reported))
		assert.Equal(t, "harness_error", f.info()["activity"])
		assert.Equal(t, []string{"model API 400: Permission denied"}, f.recorded())
	})
	// gemini-cli 0.68.0: BeforeAgent, BeforeModel, gemini_cli.api_error
	// 400, then AfterAgent with no error field writes working. That status
	// write restarts the quiet period; it does not cancel the report.
	t.Run("gemini-cli: AfterAgent rewrites working after the error", func(t *testing.T) {
		f := newWatchdogFixture(t, "thinking")
		f.turnAt = watchdogLaunch.Add(5 * time.Second)
		f.outcome(6*time.Second, false, 400, "Permission denied")
		var reported time.Time
		f.writeActivity("working") // AfterAgent: agent-end without an error
		afterAgent := watchdogLaunch.Add(8 * time.Second)
		f.cfg.LastStatusChangeAt = func() (time.Time, bool) { return afterAgent, true }
		assert.False(t, f.check(37*time.Second, &reported), "inside the grace period after AfterAgent")
		assert.True(t, f.check(38*time.Second, &reported))
		assert.Equal(t, "harness_error", f.info()["activity"])
	})
}

func TestAPIErrorWatchdog_QuietPeriodRestartsOnHookActivity(t *testing.T) {
	t.Run("turn event", func(t *testing.T) {
		f := newWatchdogFixture(t, "working")
		f.outcome(10*time.Second, false, 400, "bad request")
		f.turnEnd(12 * time.Second)
		f.turnAt = watchdogLaunch.Add(25 * time.Second)
		var reported time.Time
		assert.False(t, f.check(50*time.Second, &reported))
		assert.True(t, f.check(55*time.Second, &reported))
	})
	t.Run("status change", func(t *testing.T) {
		f := newWatchdogFixture(t, "working")
		f.outcome(10*time.Second, false, 400, "bad request")
		changed := watchdogLaunch.Add(30 * time.Second)
		f.cfg.LastStatusChangeAt = func() (time.Time, bool) { return changed, true }
		var reported time.Time
		assert.False(t, f.check(59*time.Second, &reported))
		assert.True(t, f.check(60*time.Second, &reported))
	})
}

func TestAPIErrorWatchdog_Guards(t *testing.T) {
	for _, activity := range []string{"executing", "waiting_for_input", "blocked", "idle", "completed", "initializing"} {
		t.Run("activity "+activity, func(t *testing.T) {
			f := newWatchdogFixture(t, activity)
			f.outcome(time.Second, false, 401, "bad key")
			var reported time.Time
			// Past both grace periods, so only the activity guard can
			// hold the report back.
			assert.False(t, f.check(3*time.Minute, &reported))
			assert.Equal(t, activity, f.info()["activity"])
		})
	}
	t.Run("live work", func(t *testing.T) {
		f := newWatchdogFixture(t, "working")
		f.live = true
		f.outcome(time.Second, false, 401, "bad key")
		var reported time.Time
		assert.False(t, f.check(3*time.Minute, &reported))
		f.live = false
		assert.True(t, f.check(190*time.Second, &reported))
	})
	t.Run("nil live work means none", func(t *testing.T) {
		f := newWatchdogFixture(t, "working")
		f.cfg.LiveWork = nil
		f.outcome(time.Second, false, 401, "bad key")
		var reported time.Time
		assert.True(t, f.check(3*time.Minute, &reported))
	})
	t.Run("failure from an earlier run", func(t *testing.T) {
		f := newWatchdogFixture(t, "working")
		f.outcome(-time.Second, false, 401, "bad key")
		var reported time.Time
		assert.False(t, f.check(time.Minute, &reported))
	})
	t.Run("a new failure after a cleared error fires again", func(t *testing.T) {
		f := newWatchdogFixture(t, "working")
		f.outcome(time.Second, false, 401, "first")
		var reported time.Time
		require.True(t, f.check(3*time.Minute, &reported))
		f.writeActivity("working") // a new turn cleared harness_error
		f.outcome(4*time.Minute, false, 403, "second")
		assert.True(t, f.check(7*time.Minute, &reported))
		assert.Len(t, f.recorded(), 2)
	})
}

// The hook path wins: a harness_error a turn-end hook already set is
// neither replaced nor reported again.
func TestAPIErrorWatchdog_HookPathHarnessErrorWins(t *testing.T) {
	f := newWatchdogFixture(t, "working")
	hookStatus := &handlers.StatusHandler{StatusPath: f.status.StatusPath}
	f.outcome(time.Second, false, 400, "bad request")
	require.NoError(t, hookStatus.Handle(turnEndHookErrorEvent("APIError (status 400)")))
	require.Equal(t, "harness_error", f.info()["activity"])

	var reported time.Time
	assert.False(t, f.check(time.Minute, &reported))
	assert.Equal(t, "model API: APIError (status 400)", infoMessage(f.info()))
	assert.Empty(t, f.recorded(), "the watchdog reported a harness_error the hook path already set")

	// Even if the activity check raced the hook's write, the locked
	// conditional write refuses to replace it.
	wrote, err := f.status.SetHarnessErrorIf("model API 400: bad request", func(state.Activity) bool { return true })
	require.NoError(t, err)
	assert.False(t, wrote)
}

func turnEndHookErrorEvent(errText string) *hooks.Event {
	return &hooks.Event{Name: hooks.EventAgentEnd, Dialect: "opencode", Data: hooks.EventData{Error: errText}}
}

func TestAPIErrorWatchdog_LoopReportsOnTick(t *testing.T) {
	f := newWatchdogFixture(t, "working")
	ticks := make(chan time.Time)
	f.cfg.NewTicker = func(d time.Duration) (<-chan time.Time, func()) {
		assert.Equal(t, apiErrorWatchdogTick, d)
		return ticks, func() {}
	}
	f.outcome(time.Second, false, 404, "model not found")
	f.now = watchdogLaunch.Add(3 * time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runAPIErrorWatchdog(ctx, f.cfg)
	}()
	ticks <- time.Time{}
	ticks <- time.Time{} // the second tick sees the first one's report
	cancel()
	<-done
	assert.Equal(t, []string{"model API 404: model not found"}, f.recorded())
}

func TestFirstTurnWatchdog(t *testing.T) {
	readyAt := watchdogLaunch.Add(10 * time.Second)
	run := func(f *watchdogFixture) {
		f.t.Helper()
		var waited time.Duration
		f.cfg.After = func(d time.Duration) <-chan time.Time {
			waited = d
			ch := make(chan time.Time, 1)
			ch <- time.Time{}
			return ch
		}
		f.now = readyAt
		runFirstTurnWatchdog(context.Background(), f.cfg, readyAt)
		assert.Equal(f.t, firstTurnTimeout, waited)
	}

	t.Run("no turn event: pane tail reason", func(t *testing.T) {
		f := newWatchdogFixture(t, "working")
		f.cfg.PaneTail = func(context.Context) string {
			return "Please log in | Authorization: Bearer sk-abcdefghijklmnopqrstuvwxyz0123"
		}
		run(f)
		info := f.info()
		assert.Equal(t, "harness_error", info["activity"])
		msg := infoMessage(info)
		assert.True(t, strings.HasPrefix(msg, "harness did not start the initial task: Please log in"), msg)
		assert.NotContains(t, msg, "sk-abcdefghijklmnopqrstuvwxyz0123")
		assert.Equal(t, []string{msg}, f.recorded())
	})
	t.Run("a long pane tail keeps its newest end", func(t *testing.T) {
		f := newWatchdogFixture(t, "working")
		f.cfg.PaneTail = func(context.Context) string {
			return strings.Repeat("old output ", 60) + "| Error: please run /login"
		}
		run(f)
		msg := infoMessage(f.info())
		assert.True(t, strings.HasPrefix(msg, "harness did not start the initial task: "), msg)
		assert.True(t, strings.HasSuffix(msg, "Error: please run /login"), msg)
		assert.LessOrEqual(t, len([]rune(msg)), 300)
	})
	t.Run("pending error wins over the pane tail", func(t *testing.T) {
		f := newWatchdogFixture(t, "working")
		f.cfg.PendingError = func() (handlers.PendingHarnessError, bool) {
			return handlers.PendingHarnessError{At: watchdogLaunch.Add(time.Second), Reason: "model API: 401 invalid x-api-key"}, true
		}
		f.cfg.PaneTail = func(context.Context) string { t.Error("pane captured despite a pending error"); return "" }
		run(f)
		assert.Equal(t, "harness did not start the initial task: model API: 401 invalid x-api-key", infoMessage(f.info()))
	})
	t.Run("a pending error from an earlier run is ignored", func(t *testing.T) {
		f := newWatchdogFixture(t, "working")
		f.cfg.PendingError = func() (handlers.PendingHarnessError, bool) {
			return handlers.PendingHarnessError{At: watchdogLaunch.Add(-time.Hour), Reason: "model API: stale"}, true
		}
		f.cfg.PaneTail = func(context.Context) string { return "> " }
		run(f)
		assert.Equal(t, "harness did not start the initial task: >", infoMessage(f.info()))
	})
	t.Run("turn event in this run: no report", func(t *testing.T) {
		f := newWatchdogFixture(t, "working")
		f.turnAt = watchdogLaunch.Add(time.Minute)
		run(f)
		assert.Equal(t, "working", f.info()["activity"])
		assert.Empty(t, f.recorded())
	})
	// muse-code 1.4.20 (B2a capture): UserPromptSubmit, PreLLMCall, then
	// PostLLMCall carrying "API error 400: ..." and no Stop. The prompt was
	// submitted, but the turn's last word is the model error.
	t.Run("turn ended on a model error: pending reason", func(t *testing.T) {
		f := newWatchdogFixture(t, "working")
		f.turnAt = watchdogLaunch.Add(20 * time.Second)
		f.cfg.PendingError = func() (handlers.PendingHarnessError, bool) {
			return handlers.PendingHarnessError{At: watchdogLaunch.Add(21 * time.Second), Event: hooks.EventModelEnd,
				Reason: "model API: API error 400: Permission 'aiplatform.endpoints.predict' denied"}, true
		}
		run(f)
		assert.Equal(t, "harness did not start the initial task: model API: API error 400: Permission 'aiplatform.endpoints.predict' denied",
			infoMessage(f.info()))
		assert.Len(t, f.recorded(), 1)
	})
	t.Run("a model error followed by a new turn event: no report", func(t *testing.T) {
		f := newWatchdogFixture(t, "working")
		f.turnAt = watchdogLaunch.Add(22 * time.Second)
		f.cfg.PendingError = func() (handlers.PendingHarnessError, bool) {
			return handlers.PendingHarnessError{At: watchdogLaunch.Add(21 * time.Second), Event: hooks.EventModelEnd, Reason: "model API: 503"}, true
		}
		run(f)
		assert.Equal(t, "working", f.info()["activity"])
		assert.Empty(t, f.recorded())
	})
	t.Run("a failed tool after the last turn event: no report", func(t *testing.T) {
		f := newWatchdogFixture(t, "working")
		f.turnAt = watchdogLaunch.Add(20 * time.Second)
		f.cfg.PendingError = func() (handlers.PendingHarnessError, bool) {
			return handlers.PendingHarnessError{At: watchdogLaunch.Add(21 * time.Second), Event: hooks.EventToolEnd, Reason: "tool Bash: exit 1"}, true
		}
		run(f)
		assert.Equal(t, "working", f.info()["activity"])
		assert.Empty(t, f.recorded())
	})
	t.Run("turn event from an earlier run still reports", func(t *testing.T) {
		f := newWatchdogFixture(t, "working")
		f.turnAt = watchdogLaunch.Add(-time.Minute)
		run(f)
		assert.Equal(t, "harness_error", f.info()["activity"])
	})
	for _, activity := range []string{"waiting_for_input", "blocked", "idle", "completed", "harness_error"} {
		t.Run("keeps sticky "+activity, func(t *testing.T) {
			f := newWatchdogFixture(t, activity)
			run(f)
			assert.Equal(t, activity, f.info()["activity"])
			assert.Empty(t, f.recorded())
		})
	}
	t.Run("cancelled before the timeout", func(t *testing.T) {
		f := newWatchdogFixture(t, "working")
		f.cfg.After = func(time.Duration) <-chan time.Time { return make(chan time.Time) }
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		runFirstTurnWatchdog(ctx, f.cfg, readyAt)
		assert.Equal(t, "working", f.info()["activity"])
	})
}

// Review R1 (final): the first-turn watchdog retracts its own report when a
// turn event arrives after it, and never a harness_error with another
// reason.
func TestFirstTurnWatchdog_Retraction(t *testing.T) {
	type rig struct {
		f       *watchdogFixture
		ticks   chan time.Time
		done    chan struct{}
		cancel  context.CancelFunc
		working []state.Activity
		tick    func()
	}
	start := func(t *testing.T) *rig {
		t.Helper()
		r := &rig{f: newWatchdogFixture(t, "working"), ticks: make(chan time.Time), done: make(chan struct{})}
		// A tick send never blocks once the watch has ended: the watch can
		// see a state change and return before it takes the next tick.
		r.tick = func() {
			select {
			case r.ticks <- time.Time{}:
			case <-r.done:
			}
		}
		f := r.f
		f.now = watchdogLaunch.Add(4 * time.Minute)
		f.cfg.PaneTail = func(context.Context) string { return "thinking..." }
		f.cfg.After = func(time.Duration) <-chan time.Time {
			ch := make(chan time.Time, 1)
			ch <- time.Time{}
			return ch
		}
		f.cfg.NewTicker = func(d time.Duration) (<-chan time.Time, func()) {
			assert.Equal(t, firstTurnRetractionTick, d)
			return r.ticks, func() {}
		}
		f.cfg.RestoreFromHarnessError = f.status.RestoreFromHarnessErrorIf
		f.cfg.ReportActivity = func(_ context.Context, a state.Activity, msg string) error {
			assert.Equal(t, "Ready", msg)
			f.mu.Lock()
			defer f.mu.Unlock()
			r.working = append(r.working, a)
			return nil
		}
		ctx, cancel := context.WithCancel(context.Background())
		r.cancel = cancel
		go func() {
			defer close(r.done)
			runFirstTurnWatchdog(ctx, f.cfg, watchdogLaunch)
		}()
		r.tick() // the first tick: the report has been written
		require.Equal(t, "harness_error", f.info()["activity"])
		return r
	}
	wait := func(t *testing.T, r *rig) {
		t.Helper()
		select {
		case <-r.done:
		case <-time.After(5 * time.Second):
			t.Fatal("the retraction watch did not end")
		}
	}

	t.Run("a turn event after the report restores working", func(t *testing.T) {
		r := start(t)
		defer r.cancel()
		r.f.mu.Lock()
		r.f.turnAt = r.f.now.Add(time.Second)
		r.f.mu.Unlock()
		r.tick()
		wait(t, r)
		info := r.f.info()
		assert.Equal(t, "working", info["activity"])
		assert.Empty(t, infoMessage(info))
		assert.Equal(t, []state.Activity{state.ActivityWorking}, r.working)
	})
	t.Run("a turn event before the report does not", func(t *testing.T) {
		r := start(t)
		r.f.mu.Lock()
		r.f.turnAt = r.f.now.Add(-time.Second)
		r.f.mu.Unlock()
		r.tick()
		r.cancel()
		wait(t, r)
		assert.Equal(t, "harness_error", r.f.info()["activity"])
		assert.Empty(t, r.working)
	})
	t.Run("a hook-path harness_error is never cleared", func(t *testing.T) {
		r := start(t)
		defer r.cancel()
		hook := &handlers.StatusHandler{StatusPath: r.f.status.StatusPath}
		require.NoError(t, hook.Handle(turnEndHookErrorEvent("APIError (status 400)")))
		r.f.mu.Lock()
		r.f.turnAt = r.f.now.Add(time.Second)
		r.f.mu.Unlock()
		r.tick()
		wait(t, r)
		info := r.f.info()
		assert.Equal(t, "harness_error", info["activity"])
		assert.Equal(t, "model API: APIError (status 400)", infoMessage(info))
		assert.Empty(t, r.working)
	})
	t.Run("the watch ends once the hook path cleared the report", func(t *testing.T) {
		r := start(t)
		defer r.cancel()
		r.f.writeActivity("executing") // a tool-start cleared harness_error
		r.tick()
		wait(t, r)
		assert.Empty(t, r.working)
	})
	t.Run("the watch stops at shutdown", func(t *testing.T) {
		r := start(t)
		r.cancel()
		wait(t, r)
	})
}

func TestRunHarnessErrorWatchdogs_CancelledReadinessStartsNothing(t *testing.T) {
	f := newWatchdogFixture(t, "working")
	f.cfg.FirstTurn = true
	f.cfg.NewTicker = func(time.Duration) (<-chan time.Time, func()) { t.Error("API watchdog started"); return nil, func() {} }
	f.cfg.After = func(time.Duration) <-chan time.Time { t.Error("first-turn watchdog started"); return nil }
	runHarnessErrorWatchdogs(context.Background(), f.cfg, readyOutcomeCancelled, watchdogLaunch)
}

func TestRunHarnessErrorWatchdogs_StopsOnCancel(t *testing.T) {
	f := newWatchdogFixture(t, "working")
	f.cfg.FirstTurn = true
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runHarnessErrorWatchdogs(ctx, f.cfg, readyOutcomeSettled, watchdogLaunch)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("watchdogs did not stop on cancel")
	}
}

// newHarnessWatchdogConfig wires the production pieces: the outcome
// observer, the marker files, the first-turn gate and the Hub report (to
// an in-process httptest Hub).
func TestNewHarnessWatchdogConfig(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	agentHome := filepath.Join(dir, "home")
	require.NoError(t, os.MkdirAll(agentHome, 0o755))
	status := &handlers.StatusHandler{StatusPath: filepath.Join(agentHome, "agent-info.json")}
	require.NoError(t, os.WriteFile(status.StatusPath, []byte(`{"phase":"running","activity":"working"}`), 0o644))
	turnPath := filepath.Join(dir, "turn-events")
	pendingPath := filepath.Join(dir, "pending.json")

	var observer telemetry.APIOutcomeObserver
	env := map[string]string{"SCION_INITIAL_TASK": "1", "SCION_HARNESS": "claude", "SCION_MODEL": "claude-sonnet-5"}
	cfg := newHarnessWatchdogConfig(harnessWatchdogDeps{
		LaunchedAt:         watchdogLaunch,
		AgentHome:          agentHome,
		StatusHandler:      status,
		HubClient:          hub.NewClientWithConfig(srv.URL, "test-token", "test-agent"),
		Getenv:             func(k string) string { return env[k] },
		CapturePane:        func(context.Context, int, int) ([]byte, error) { return []byte("line1\n\nline2\n"), nil },
		TurnEventsPath:     turnPath,
		PendingErrorPath:   pendingPath,
		SetOutcomeObserver: func(o telemetry.APIOutcomeObserver) { observer = o },
	})

	require.NotNil(t, observer)
	require.NotNil(t, cfg.Outcomes)
	observer(telemetry.APIOutcome{At: watchdogLaunch, Status: 403})
	assert.Equal(t, "claude-sonnet-5", cfg.ConfiguredModel)
	latest, ok := cfg.Outcomes.Candidate(cfg.ConfiguredModel, watchdogLaunch)
	assert.True(t, ok)
	assert.Equal(t, 403, latest.Status)

	assert.True(t, cfg.FirstTurn)
	assert.Equal(t, "line1 | line2", cfg.PaneTail(context.Background()))
	assert.Equal(t, state.ActivityWorking, cfg.LocalActivity())
	_, ok = cfg.LastStatusChangeAt()
	assert.True(t, ok)

	marks := &handlers.TurnMarkHandler{TurnEventsPath: turnPath, PendingErrorPath: pendingPath}
	require.NoError(t, marks.Handle(&hooks.Event{Name: hooks.EventModelEnd, Data: hooks.EventData{Error: "401"}}))
	p, ok := cfg.PendingError()
	require.True(t, ok)
	assert.Equal(t, "model API: 401", p.Reason)
	_, ok = cfg.LastTurnEventAt()
	assert.False(t, ok)
	require.NoError(t, marks.Handle(&hooks.Event{Name: hooks.EventToolStart}))
	_, ok = cfg.LastTurnEventAt()
	assert.True(t, ok)

	require.True(t, reportHarnessError(context.Background(), cfg, "model API 403: denied", isAPIErrorReplaceable))
	// The retraction's working report carries "Ready", so the Hub replaces
	// the harness_error reason in its message.
	require.NotNil(t, cfg.ReportActivity)
	require.NoError(t, cfg.ReportActivity(context.Background(), state.ActivityWorking, firstTurnRetractedMessage))
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, bodies, 2)
	assert.Equal(t, "harness_error", bodies[0]["activity"])
	assert.Equal(t, "model API 403: denied", bodies[0]["message"])
	assert.Equal(t, "working", bodies[1]["activity"])
	assert.Equal(t, "Ready", bodies[1]["message"])

	t.Run("first-turn gate", func(t *testing.T) {
		for _, tc := range []struct {
			name, task, harness string
			want                bool
		}{
			{"no task", "", "claude", false},
			{"hermes has no hooks", "1", "hermes", false},
			{"task with built-in dialect", "1", "gemini-cli", true},
		} {
			env := map[string]string{"SCION_INITIAL_TASK": tc.task, "SCION_HARNESS": tc.harness}
			cfg := newHarnessWatchdogConfig(harnessWatchdogDeps{AgentHome: agentHome, Getenv: func(k string) string { return env[k] }})
			assert.Equal(t, tc.want, cfg.FirstTurn, tc.name)
			assert.Nil(t, cfg.Outcomes, "no pipeline: API-error watchdog off")
			assert.Nil(t, cfg.Report, "no Hub: no report")
		}
	})
}

func TestReportHarnessError_LocalFailureSkipsHub(t *testing.T) {
	reported := false
	cfg := harnessWatchdogConfig{
		SetHarnessError: func(string, func(state.Activity) bool) (bool, error) { return false, errors.New("disk full") },
		Report:          func(context.Context, string) error { reported = true; return nil },
	}
	assert.False(t, reportHarnessError(context.Background(), cfg, "r", isAPIErrorReplaceable))
	assert.False(t, reported)
}

func TestHarnessPaneTail(t *testing.T) {
	out := "a\n\n  b  \nc\nd\ne\nf\n\n"
	got := harnessPaneTail(context.Background(), func(context.Context, int, int) ([]byte, error) { return []byte(out), nil }, 1000, 1000)
	assert.Equal(t, "b | c | d | e | f", got)
	assert.Empty(t, harnessPaneTail(context.Background(), func(context.Context, int, int) ([]byte, error) { return nil, errors.New("no server") }, 0, 0))
	assert.Empty(t, harnessPaneTail(context.Background(), nil, 0, 0))
}

func TestNewHarnessWatchdogConfig_ShellSkipsPaneTail(t *testing.T) {
	capture := func(context.Context, int, int) ([]byte, error) {
		return []byte("gh auth token\nghp_secret\n"), nil
	}
	idleEnv := state.FormatReadySpecEnv(state.DefaultReadySpec(state.ReadyStrategyIdle))
	shellCfg := newHarnessWatchdogConfig(harnessWatchdogDeps{
		Getenv: func(k string) string {
			return map[string]string{
				state.HarnessReadyEnv: idleEnv,
				"SCION_HARNESS":       "claude",
			}[k]
		},
		CapturePane: capture,
	})
	assert.Nil(t, shellCfg.PaneTail, "idle-ready execution container must not wire PaneTail capture even when SCION_HARNESS=claude")

	deliveryDisabledCfg := newHarnessWatchdogConfig(harnessWatchdogDeps{
		Getenv: func(k string) string {
			return map[string]string{
				state.HarnessDeliveryDisabledEnv: "1",
				"SCION_HARNESS":                  "claude",
			}[k]
		},
		CapturePane: capture,
	})
	assert.Nil(t, deliveryDisabledCfg.PaneTail, "delivery-disabled container must not wire PaneTail capture")

	legacyShellCfg := newHarnessWatchdogConfig(harnessWatchdogDeps{
		Getenv:      func(k string) string { return map[string]string{"SCION_HARNESS": "shell"}[k] },
		CapturePane: capture,
	})
	assert.Nil(t, legacyShellCfg.PaneTail, "SCION_HARNESS=shell fallback must not wire PaneTail capture")

	llmCfg := newHarnessWatchdogConfig(harnessWatchdogDeps{
		Getenv:      func(k string) string { return map[string]string{"SCION_HARNESS": "claude"}[k] },
		CapturePane: capture,
	})
	require.NotNil(t, llmCfg.PaneTail, "non-shell harness must wire PaneTail capture")
	assert.Equal(t, "gh auth token | ghp_secret", llmCfg.PaneTail(context.Background()))
}
