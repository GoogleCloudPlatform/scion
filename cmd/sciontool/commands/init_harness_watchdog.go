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
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/dialects"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/handlers"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/telemetry"
)

// harness_error watchdogs in sciontool init (ptone/scion#7858 design §4.2
// (2) and (3)). The hook path (a turn-end hook carrying an error) is the
// primary source of harness_error; these cover harnesses whose TUI swallows
// a model API error without a failing completion hook (OTel API-error
// watchdog) and harnesses stuck before their first turn (first-turn
// watchdog).
const (
	// apiErrorWatchdogTick is how often the API-error watchdog checks. It
	// is short next to apiErrorGrace so a report lands within about 40 s
	// of the failure when the turn end was seen (design criterion 10).
	apiErrorWatchdogTick = 10 * time.Second
	// apiErrorGrace is the quiet period after a non-retryable API failure
	// once the turn has visibly ended (a status write after the failure
	// left the agent working): no success, no turn event and no status
	// change for this long. It lets a completion hook land first (the hook
	// path wins when present).
	apiErrorGrace = 30 * time.Second
	// apiErrorThinkingGrace is the quiet period while no turn end has been
	// seen since the failure. A harness may be retrying the request and
	// streaming the retry, or streaming the main model while a side call
	// failed, and no outcome arrives until that request completes. It
	// stays under the 5-minute stall threshold, so a turn that died
	// silently (codex 0.169 stuck thinking) still gets HARNESS_ERROR
	// before STALLED.
	apiErrorThinkingGrace = 120 * time.Second
	// firstTurnTimeout is how long after readiness an agent started with a
	// task may go without a turn event. It sits under the 5-minute
	// stall threshold, so HARNESS_ERROR comes before STALLED.
	firstTurnTimeout = 3 * time.Minute
	// harnessWatchdogReportTimeout bounds one Hub report.
	harnessWatchdogReportTimeout = 10 * time.Second

	// firstTurnRetractedMessage is the Hub message sent with working when
	// the first-turn watchdog retracts its report.
	firstTurnRetractedMessage = "Ready"

	// firstTurnRetractionTick is how often the first-turn watchdog checks,
	// after reporting, whether the harness has started the task after all.
	firstTurnRetractionTick = 10 * time.Second

	// firstTurnReasonSource is the first-turn watchdog's reason source.
	firstTurnReasonSource = "harness did not start the initial task"
)

// envInitialTask is set to "1" in the agent's environment when the agent was
// started with a task (pkg/agent: initialTaskEnv). Only then does the
// first-turn watchdog run: a resumed or task-less agent waits for input and
// sends no turn event, rightly.
const envInitialTask = "SCION_INITIAL_TASK"

// isNonRetryableAPIStatus reports whether status is a model API failure no
// retry fixes: a bad request, failed authentication, a denied permission
// or an unknown model or endpoint. 408, 409, 429 and 5xx are retried by
// the harnesses and do not count.
func isNonRetryableAPIStatus(status int) bool {
	switch status {
	case 400, 401, 403, 404:
		return true
	}
	return false
}

// harnessWatchdogConfig drives runHarnessErrorWatchdogs. Every clock,
// timer, file read and side effect is injected, so tests run without
// waiting, without /tmp and without a Hub.
type harnessWatchdogConfig struct {
	// LaunchedAt is when the harness was launched. Turn events and
	// pending errors recorded before it belong to an earlier run.
	LaunchedAt time.Time

	// Now is the clock; nil uses time.Now.
	Now func() time.Time
	// NewTicker returns a tick channel and its stop function; nil uses
	// time.NewTicker.
	NewTicker func(d time.Duration) (<-chan time.Time, func())
	// After returns a channel that fires once after d; nil uses time.After.
	After func(d time.Duration) <-chan time.Time

	// Outcomes holds the model API outcomes of this run. Nil turns the
	// API-error watchdog off (no telemetry pipeline).
	Outcomes *apiOutcomeTracker
	// ConfiguredModel is the agent's configured model (SCION_MODEL), used
	// to tell the primary model's outcomes from side and sub-agent calls
	// (see apiOutcomeTracker.Candidate).
	ConfiguredModel string
	// LiveWork reports whether the agent has live work (a running tool
	// process or background job). Nil means no live work. sciontool init
	// wires it to the heartbeat's liveness samples (liveWorkRecorder.Live,
	// ptone/scion#7855).
	LiveWork func() bool
	// LastTurnEventAt returns when the hook process last recorded a
	// turn event (handlers.IsTurnEvent).
	LastTurnEventAt func() (time.Time, bool)
	// LastStatusChangeAt returns when agent-info.json was last written.
	LastStatusChangeAt func() (time.Time, bool)
	// LocalActivity returns the activity in agent-info.json.
	LocalActivity func() state.Activity
	// SetHarnessError records harness_error locally when allow accepts the
	// current activity (handlers.StatusHandler.SetHarnessErrorIf).
	SetHarnessError func(reason string, allow func(state.Activity) bool) (bool, error)
	// Report sends harness_error with reason to the Hub. Nil without a Hub.
	Report func(ctx context.Context, reason string) error

	// FirstTurn enables the first-turn watchdog: the agent was started
	// with a task and its harness declares turn hooks.
	FirstTurn bool
	// PendingError returns the last errored model-end or tool-end.
	PendingError func() (handlers.PendingHarnessError, bool)
	// PaneTail returns the last lines of the harness pane, raw.
	PaneTail func(ctx context.Context) string
	// RestoreFromHarnessError replaces harness_error with `to` only while
	// its reason is still `reason` (handlers.StatusHandler
	// .RestoreFromHarnessErrorIf).
	RestoreFromHarnessError func(reason string, to state.Activity) (bool, error)
	// ReportActivity sends running/<activity> with message to the Hub. Nil
	// without a Hub.
	ReportActivity func(ctx context.Context, activity state.Activity, message string) error
}

func (c *harnessWatchdogConfig) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// runHarnessErrorWatchdogs runs both watchdogs until ctx is cancelled. It
// runs after the readiness watcher in the same goroutine, so readyAt is
// when readiness was reached; a cancelled readiness watch starts nothing.
func runHarnessErrorWatchdogs(ctx context.Context, cfg harnessWatchdogConfig, readiness readyWatchOutcome, readyAt time.Time) {
	if readiness == readyOutcomeCancelled || ctx.Err() != nil {
		return
	}
	var wg sync.WaitGroup
	if cfg.Outcomes != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runAPIErrorWatchdog(ctx, cfg)
		}()
	}
	if cfg.FirstTurn {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runFirstTurnWatchdog(ctx, cfg, readyAt)
		}()
	}
	wg.Wait()
}

// runAPIErrorWatchdog checks every apiErrorWatchdogTick whether the latest
// non-retryable model API failure has gone unanswered (see
// apiErrorReason) and, if so, reports harness_error once for it.
func runAPIErrorWatchdog(ctx context.Context, cfg harnessWatchdogConfig) {
	ticks, stop := newWatchdogTicker(cfg, apiErrorWatchdogTick)
	defer stop()
	var reported time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
		}
		reason, failureAt, ok := apiErrorReason(cfg, cfg.now(), reported)
		if !ok {
			continue
		}
		if reportHarnessError(ctx, cfg, reason, isAPIErrorReplaceable) {
			reported = failureAt
			log.Info("harness_error: %s", reason)
		}
	}
}

// apiErrorReason decides whether the API-error watchdog reports. It does
// when all of these hold (design §4.2 (2)):
//   - the primary model's latest outcome in this run is a failure with a
//     non-retryable status, not yet reported. A failed call to another
//     model (a sub-agent, a small/fast side call) is ignored; see
//     apiOutcomeTracker.Candidate;
//   - nothing happened for the grace period since: no success (it would be
//     the latest outcome), no turn event and no status change. A hook
//     activity change after the failure restarts the quiet period; it
//     never cancels the report. gemini-cli's AfterAgent and Claude's Stop
//     rewrite working right after a failed turn, carrying no error, and the
//     report must still come. A retry shows up as a newer outcome
//     (success or another failure), not as a status write;
//   - the grace period is apiErrorGrace (30 s) once a turn end has been
//     seen after the failure (a status write after it, and the agent is
//     working), and apiErrorThinkingGrace (120 s) otherwise: while the
//     turn has not visibly ended, a retry or the main model may still be
//     streaming, and no outcome arrives until that request completes;
//   - the activity is thinking or working;
//   - there is no live work.
//
// It returns the reason and the failure's time.
func apiErrorReason(cfg harnessWatchdogConfig, now, reported time.Time) (string, time.Time, bool) {
	latest, ok := cfg.Outcomes.Candidate(cfg.ConfiguredModel, cfg.LaunchedAt)
	if !ok || latest.OK || !isNonRetryableAPIStatus(latest.Status) {
		return "", time.Time{}, false
	}
	if latest.At.Before(cfg.LaunchedAt) || latest.At.Equal(reported) {
		return "", time.Time{}, false
	}
	quietSince := latest.At
	if cfg.LastTurnEventAt != nil {
		if at, ok := cfg.LastTurnEventAt(); ok && at.After(quietSince) {
			quietSince = at
		}
	}
	statusChangedAfterFailure := false
	if cfg.LastStatusChangeAt != nil {
		if at, ok := cfg.LastStatusChangeAt(); ok && at.After(latest.At) {
			statusChangedAfterFailure = true
			if at.After(quietSince) {
				quietSince = at
			}
		}
	}
	if cfg.LocalActivity == nil {
		return "", time.Time{}, false
	}
	activity := cfg.LocalActivity()
	if !isAPIErrorReplaceable(activity) {
		return "", time.Time{}, false
	}
	grace := apiErrorThinkingGrace
	if statusChangedAfterFailure && activity == state.ActivityWorking {
		grace = apiErrorGrace
	}
	if now.Sub(quietSince) < grace {
		return "", time.Time{}, false
	}
	if cfg.LiveWork != nil && cfg.LiveWork() {
		return "", time.Time{}, false
	}
	source := fmt.Sprintf("model API %d", latest.Status)
	return handlers.FormatHarnessErrorReason(source, latest.Message), latest.At, true
}

// isAPIErrorReplaceable reports whether the API-error watchdog may replace
// activity: thinking or working, the activities a harness that swallowed
// the error sits in.
func isAPIErrorReplaceable(activity state.Activity) bool {
	return activity == state.ActivityThinking || activity == state.ActivityWorking
}

// runFirstTurnWatchdog waits firstTurnTimeout after readyAt and reports
// harness_error when no turn event has been recorded in this run.
func runFirstTurnWatchdog(ctx context.Context, cfg harnessWatchdogConfig, readyAt time.Time) {
	wait := readyAt.Add(firstTurnTimeout).Sub(cfg.now())
	after := cfg.After
	if after == nil {
		after = time.After
	}
	if wait > 0 {
		select {
		case <-ctx.Done():
			return
		case <-after(wait):
		}
	}
	// Taken before firstTurnReason reads the turn marks, so a turn event
	// that lands while the report is being made is after reportedAt and
	// retracts it.
	reportedAt := cfg.now()
	reason, ok := firstTurnReason(ctx, cfg)
	if !ok {
		return
	}
	if !reportHarnessError(ctx, cfg, reason, isFirstTurnReplaceable) {
		return
	}
	log.Info("harness_error: %s", reason)
	watchFirstTurnRetraction(ctx, cfg, reason, reportedAt)
}

// watchFirstTurnRetraction retracts the first-turn watchdog's own report
// once the harness shows it did start: a turn event recorded after the
// report. A harness with no turn-start event for the initial prompt (pi,
// OpenCode) can show progress only through a tool call, a finished model
// step or a finished turn, and a long first step can outlast
// firstTurnTimeout. A tool call clears harness_error through the hook path
// already, but pi's error-free agent-end does not (agent-end does not clear
// harness_error in general: Claude's Stop and gemini-cli's AfterAgent fire
// without an error right after a failed turn).
//
// The replace is guarded: only harness_error with this watchdog's own
// reason becomes working, so a harness_error the hook path set is never
// cleared here. The watch ends at the first turn event after the report,
// when the activity is no longer harness_error, or when ctx is cancelled
// (init shutdown).
func watchFirstTurnRetraction(ctx context.Context, cfg harnessWatchdogConfig, reason string, reportedAt time.Time) {
	if cfg.LastTurnEventAt == nil || cfg.RestoreFromHarnessError == nil {
		return
	}
	ticks, stop := newWatchdogTicker(cfg, firstTurnRetractionTick)
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
		}
		if cfg.LocalActivity != nil && cfg.LocalActivity() != state.ActivityHarnessError {
			return
		}
		at, ok := cfg.LastTurnEventAt()
		if !ok || !at.After(reportedAt) {
			continue
		}
		restored, err := cfg.RestoreFromHarnessError(reason, state.ActivityWorking)
		if err != nil {
			log.Error("harness_error: retracting the first-turn report locally: %v", err)
			return
		}
		if !restored {
			return
		}
		log.Info("harness_error: retracted the first-turn report; the harness started the task")
		if cfg.ReportActivity != nil {
			reportCtx, cancel := context.WithTimeout(ctx, harnessWatchdogReportTimeout)
			// "Ready", as the hook path's end-of-turn working sends: the
			// Hub replaces the stale harness_error reason in its message.
			if err := cfg.ReportActivity(reportCtx, state.ActivityWorking, firstTurnRetractedMessage); err != nil {
				log.Error("harness_error: reporting the retraction to Hub: %v", err)
			}
			cancel()
		}
		return
	}
}

// firstTurnReason returns the first-turn watchdog's reason, and false when
// the harness started the initial task. It counts as not started when no
// turn event (handlers.IsTurnEvent) was recorded in this run, or when the last word of the
// turn so far is a model error: a pending model-end error newer than the
// last turn event. Real harnesses submit the prompt (a turn-start
// event) before the model call that fails (muse-code: PreLLMCall, then
// PostLLMCall with the error, then nothing), so "no turn-start event" alone
// would only catch failures before the prompt. A pending tool-end error
// does not count that way: a failed tool is normal work, and the model may
// still be reasoning about it.
//
// The reason carries the pending error from this run when there is one,
// and the harness pane's tail otherwise (Q-B2 = a).
func firstTurnReason(ctx context.Context, cfg harnessWatchdogConfig) (string, bool) {
	var pending handlers.PendingHarnessError
	hasPending := false
	if cfg.PendingError != nil {
		if p, ok := cfg.PendingError(); ok && !p.At.Before(cfg.LaunchedAt) {
			pending, hasPending = p, true
		}
	}
	if cfg.LastTurnEventAt != nil {
		if at, ok := cfg.LastTurnEventAt(); ok && !at.Before(cfg.LaunchedAt) {
			endedOnModelError := hasPending && pending.Event == hooks.EventModelEnd && pending.At.After(at)
			if !endedOnModelError {
				return "", false
			}
		}
	}
	if hasPending && pending.Reason != "" {
		return handlers.FormatHarnessErrorReason(firstTurnReasonSource, pending.Reason), true
	}
	detail := ""
	if cfg.PaneTail != nil {
		detail = cfg.PaneTail(ctx)
	}
	// A pane tail's newest lines are at its end: keep the end when cutting.
	return handlers.FormatHarnessTailReason(firstTurnReasonSource, detail), true
}

// isFirstTurnReplaceable reports whether the first-turn watchdog may
// replace activity: any recorded non-sticky activity. A sticky one
// (waiting_for_input, blocked, completed, a hook-set harness_error, ...)
// already says more than the watchdog can.
func isFirstTurnReplaceable(activity state.Activity) bool {
	return activity != "" && !activity.IsSticky()
}

// reportHarnessError records harness_error locally when allow accepts the
// current activity and, if it did, reports it to the Hub. It reports
// whether the local record was written.
func reportHarnessError(ctx context.Context, cfg harnessWatchdogConfig, reason string, allow func(state.Activity) bool) bool {
	if cfg.SetHarnessError == nil {
		return false
	}
	wrote, err := cfg.SetHarnessError(reason, allow)
	if err != nil {
		log.Error("harness_error: updating local status: %v", err)
		return false
	}
	if !wrote {
		return false
	}
	if cfg.Report != nil {
		reportCtx, cancel := context.WithTimeout(ctx, harnessWatchdogReportTimeout)
		defer cancel()
		if err := cfg.Report(reportCtx, reason); err != nil {
			log.Error("harness_error: reporting to Hub: %v", err)
		}
	}
	return true
}

func newWatchdogTicker(cfg harnessWatchdogConfig, d time.Duration) (<-chan time.Time, func()) {
	if cfg.NewTicker != nil {
		return cfg.NewTicker(d)
	}
	t := time.NewTicker(d)
	return t.C, t.Stop
}

// fileModTime returns path's mtime, without following a symlink at path.
func fileModTime(path string) (time.Time, bool) {
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return time.Time{}, false
	}
	return fi.ModTime(), true
}

// harnessWatchdogDeps are the RunInit values the watchdogs' production
// configuration is built from.
type harnessWatchdogDeps struct {
	LaunchedAt         time.Time
	AgentHome          string
	StatusHandler      *handlers.StatusHandler
	HubClient          *hub.Client
	Pipeline           *telemetry.Pipeline
	TargetUID          int
	TargetGID          int
	Getenv             func(string) string
	CapturePane        paneCaptureFunc
	TurnEventsPath     string
	PendingErrorPath   string
	SetOutcomeObserver func(telemetry.APIOutcomeObserver)
	// LiveWork reports whether the container has live work
	// (liveWorkRecorder.Live). Nil means none.
	LiveWork func() bool
}

// newHarnessWatchdogConfig builds the watchdogs' production configuration
// and, when telemetry runs, registers the API outcome observer with the
// pipeline.
func newHarnessWatchdogConfig(d harnessWatchdogDeps) harnessWatchdogConfig {
	getenv := d.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	turnPath, pendingPath := d.TurnEventsPath, d.PendingErrorPath
	cfg := harnessWatchdogConfig{
		LaunchedAt: d.LaunchedAt,
		LastTurnEventAt: func() (time.Time, bool) {
			return handlers.ReadLastTurnEventAt(turnPath)
		},
		PendingError: func() (handlers.PendingHarnessError, bool) {
			return handlers.ReadPendingHarnessError(pendingPath)
		},
		FirstTurn:       getenv(envInitialTask) == "1" && dialects.HasTurnHooks(getenv("SCION_HARNESS"), d.AgentHome),
		ConfiguredModel: getenv("SCION_MODEL"),
		LiveWork:        d.LiveWork,
	}
	if d.StatusHandler != nil {
		statusPath := d.StatusHandler.StatusPath
		cfg.LocalActivity = d.StatusHandler.CurrentActivity
		cfg.SetHarnessError = d.StatusHandler.SetHarnessErrorIf
		cfg.RestoreFromHarnessError = d.StatusHandler.RestoreFromHarnessErrorIf
		cfg.LastStatusChangeAt = func() (time.Time, bool) { return fileModTime(statusPath) }
	}
	if d.CapturePane != nil {
		uid, gid, capture := d.TargetUID, d.TargetGID, d.CapturePane
		cfg.PaneTail = func(ctx context.Context) string { return harnessPaneTail(ctx, capture, uid, gid) }
	}
	if d.HubClient != nil && d.HubClient.IsConfigured() {
		client := d.HubClient
		cfg.Report = func(ctx context.Context, reason string) error {
			return client.UpdateStatus(ctx, handlers.HarnessErrorStatusUpdate(reason))
		}
		cfg.ReportActivity = func(ctx context.Context, activity state.Activity, message string) error {
			as := state.AgentState{Phase: state.PhaseRunning, Activity: activity}
			return client.UpdateStatus(ctx, hub.StatusUpdate{
				Phase:    state.PhaseRunning,
				Activity: activity,
				Status:   as.DisplayStatus(),
				Message:  message,
			})
		}
	}
	setObserver := d.SetOutcomeObserver
	if setObserver == nil && d.Pipeline != nil {
		setObserver = d.Pipeline.SetAPIOutcomeObserver
	}
	if setObserver != nil {
		cfg.Outcomes = &apiOutcomeTracker{}
		setObserver(cfg.Outcomes.Observe)
	}
	return cfg
}
