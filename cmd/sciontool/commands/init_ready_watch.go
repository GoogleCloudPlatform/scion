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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

// readyWatchPollInterval is how often the readiness watcher checks the ready
// marker. It matches the runtime buffer's probe interval.
const readyWatchPollInterval = 500 * time.Millisecond

// readyWatchReportTimeout bounds one hub status report from the watcher.
const readyWatchReportTimeout = 10 * time.Second

// readyWatchConfig drives watchHarnessReady. It uses the same readiness
// decision as the runtime message buffer (pkg/agent/harness_ready.go): the
// mode and nonce from the init-run file, the hook ready marker, and the
// ready spec's settle and deadline, which init also writes to the init-run
// file for the runtime. The rule is: ready = signal time + Settle.
type readyWatchConfig struct {
	// Nonce is the current init-run nonce; empty when the init-run file
	// could not be written, which uses the session-age rule with
	// FallbackSettle, like the runtime when it finds no init-run file.
	Nonce string
	// Mode is state.ReadyModeHook or state.ReadyModeSettle.
	Mode string
	// Start is when the harness (tmux) was launched.
	Start time.Time

	PollInterval time.Duration // zero uses readyWatchPollInterval
	// Settle is the wait after the signal: after Start in settle mode,
	// after the ready marker's mtime in hook mode. Used as given; zero
	// means ready at the signal.
	Settle time.Duration
	// Deadline bounds the wait for the hook marker, from Start. Zero uses
	// state.ReadyDeadline.
	Deadline time.Duration
	// FallbackSettle is the session age that counts as ready in hook mode
	// without a nonce. Zero uses state.ReadySettleSession.
	FallbackSettle time.Duration

	// MarkerTime reports whether the ready marker carries Nonce and, if
	// so, when it was written.
	MarkerTime func(nonce string) (time.Time, bool)
	// LocalActivity returns the activity in agent-info.json.
	LocalActivity func() state.Activity
	// ReplaceLocal sets the local activity to `to` only if it is `from`.
	ReplaceLocal func(from, to state.Activity) (bool, error)
	// Report sends running/<activity> to the hub. Nil without a hub.
	Report func(ctx context.Context, activity state.Activity, message string) error
}

// readyWatchOutcome says how a watch ended; used by tests and logs.
type readyWatchOutcome string

const (
	readyOutcomeHookMarker readyWatchOutcome = "hook-marker"
	readyOutcomeSettled    readyWatchOutcome = "settled"
	readyOutcomeDeadline   readyWatchOutcome = "deadline"
	readyOutcomeCancelled  readyWatchOutcome = "cancelled"
)

// watchHarnessReady ends the initializing activity that init reports at
// start. It runs in its own goroutine, never blocks init, and returns once
// the harness is ready, the deadline passes, or ctx is cancelled (init
// cancels it before reporting shutdown, so it cannot report working after
// stopping).
//
//   - Hook mode: ready once the marker is seen and Settle has passed since
//     its mtime, or since the watcher first saw it if that is earlier. A readiness hook event writes its activity to the local
//     agent-info.json and only then the ready marker (the marker is a
//     handler registered right after the status handler in `sciontool
//     hook`), and its hub handler reports that activity. So when the watcher
//     sees the marker, the local activity is already final: a single
//     compare-and-swap from initializing (true only if the hook's local
//     write failed) cannot overwrite the hook's activity. One ordering case
//     needs a sync: if the marker already exists when the watcher starts,
//     the hook may have reported to the hub before init's own initializing
//     report landed and been overwritten by it, so the watcher re-sends the
//     current local activity once ready.
//   - Settle mode: at Start+Settle the watcher swaps initializing to
//     working and reports working if the swap happened. Hook mode without
//     a nonce does the same at Start+FallbackSettle.
//   - Deadline: if the hook marker has not appeared by Start+Deadline, the
//     watcher does the same swap (logged at Info), matching the runtime
//     buffer, which pastes buffered messages at the same deadline. Once the
//     marker has been seen, its settle is waited out even past the
//     deadline, as the runtime does.
//
// The hub is only told "working" when the local swap from initializing
// succeeds. The swap is one locked read-modify-write of agent-info.json
// (StatusHandler.ReplaceActivityIf), so a hook's status write in another
// process cannot land between the watcher's read and write.
func watchHarnessReady(ctx context.Context, cfg readyWatchConfig) readyWatchOutcome {
	poll := cfg.PollInterval
	if poll <= 0 {
		poll = readyWatchPollInterval
	}
	settle := cfg.Settle
	if settle < 0 {
		settle = 0
	}
	deadline := cfg.Deadline
	if deadline <= 0 {
		deadline = state.ReadyDeadline
	}
	hookMode := cfg.Mode == state.ReadyModeHook && cfg.Nonce != "" && cfg.MarkerTime != nil
	sessionSettle := settle
	if cfg.Mode == state.ReadyModeHook {
		// The hook spec's settle follows the marker; without a marker to
		// wait for, use the runtime's no-init-run session age.
		sessionSettle = cfg.FallbackSettle
		if sessionSettle <= 0 {
			sessionSettle = state.ReadySettleSession
		}
	}

	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	first := true
	markerAtStart := false
	markerSeen := false
	var firstSeenAt time.Time
	for {
		now := time.Now()
		var markerAt time.Time
		if hookMode {
			markerAt, markerSeen = cfg.MarkerTime(cfg.Nonce)
			if markerSeen && firstSeenAt.IsZero() {
				firstSeenAt = now
				markerAtStart = first
			}
			// The signal is the marker's mtime, but never later than when
			// the watcher first saw it: a future mtime (a clock anomaly, or
			// the agent user touching its own marker) must not hold
			// readiness past firstSeenAt + Settle.
			if markerSeen && markerAt.After(firstSeenAt) {
				markerAt = firstSeenAt
			}
		}
		switch {
		case markerSeen && !now.Before(markerAt.Add(settle)):
			if markerAtStart {
				syncLocalActivity(ctx, cfg)
			}
			markReady(ctx, cfg, "Harness ready")
			log.Debug("Harness readiness: hook marker seen and settled")
			return readyOutcomeHookMarker
		case !hookMode && !now.Before(cfg.Start.Add(sessionSettle)):
			markReady(ctx, cfg, "Harness started")
			log.Debug("Harness readiness: settle period elapsed")
			return readyOutcomeSettled
		case hookMode && !markerSeen && !now.Before(cfg.Start.Add(deadline)):
			if markReady(ctx, cfg, "Harness readiness not confirmed by deadline; assuming ready") {
				log.Info("Harness readiness: no ready hook event within %s; reporting working", deadline)
			}
			return readyOutcomeDeadline
		}
		first = false
		select {
		case <-ctx.Done():
			return readyOutcomeCancelled
		case <-ticker.C:
		}
	}
}

// markReady swaps the local activity from initializing to working and, if
// that happened, reports working to the hub. It reports whether it swapped.
func markReady(ctx context.Context, cfg readyWatchConfig, message string) bool {
	if cfg.ReplaceLocal == nil {
		return false
	}
	swapped, err := cfg.ReplaceLocal(state.ActivityInitializing, state.ActivityWorking)
	if err != nil {
		log.Error("Harness readiness: updating local status: %v", err)
		// Without a local record, still tell the hub: leaving the agent
		// initializing would only delay interrupts and wakes.
		swapped = cfg.LocalActivity != nil && cfg.LocalActivity() == state.ActivityInitializing
	}
	if !swapped {
		return false
	}
	report(ctx, cfg, state.ActivityWorking, message)
	return true
}

// syncLocalActivity re-sends the local activity to the hub when a hook has
// already moved it off initializing.
func syncLocalActivity(ctx context.Context, cfg readyWatchConfig) {
	if cfg.LocalActivity == nil {
		return
	}
	a := cfg.LocalActivity()
	if a == "" || a == state.ActivityInitializing {
		return
	}
	report(ctx, cfg, a, "Harness ready")
}

func report(ctx context.Context, cfg readyWatchConfig, activity state.Activity, message string) {
	if cfg.Report == nil || ctx.Err() != nil {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, readyWatchReportTimeout)
	defer cancel()
	if err := cfg.Report(rctx, activity, message); err != nil {
		log.Error("Harness readiness: reporting %s to Hub: %v", activity, err)
	}
}
