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
// state.ReadySettleSession / state.ReadyDeadline timings.
type readyWatchConfig struct {
	// Nonce is the current init-run nonce; empty when the init-run file
	// could not be written, which uses the settle rule like the runtime.
	Nonce string
	// Mode is state.ReadyModeHook or state.ReadyModeSettle.
	Mode string
	// Start is when the harness (tmux) was launched.
	Start time.Time

	PollInterval time.Duration // zero uses readyWatchPollInterval
	Settle       time.Duration // zero uses state.ReadySettleSession
	Deadline     time.Duration // zero uses state.ReadyDeadline

	// MarkerReady reports whether the ready marker carries Nonce.
	MarkerReady func(nonce string) bool
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
//   - Hook mode: a readiness hook event writes the ready marker and, through
//     the hook's own status and hub handlers, already replaces initializing
//     locally and on the hub. When the marker appears the watcher only acts
//     if the local activity is still initializing (the hook's local write
//     failed). One ordering case needs a sync: if the marker already exists
//     when the watcher starts, the hook may have reported to the hub before
//     init's own initializing report landed and been overwritten by it, so
//     the watcher re-sends the current local activity.
//   - Settle mode (or no nonce): at Start+Settle the watcher reports working
//     if the local activity is still initializing.
//   - Deadline: if the hook marker has not appeared by Start+Deadline, the
//     watcher reports working (logged at Info), matching the runtime buffer,
//     which pastes buffered messages at the same deadline.
//
// The hub is only told "working" when the local swap from initializing
// succeeds, so an activity written by a hook is never overwritten.
func watchHarnessReady(ctx context.Context, cfg readyWatchConfig) readyWatchOutcome {
	poll := cfg.PollInterval
	if poll <= 0 {
		poll = readyWatchPollInterval
	}
	settle := cfg.Settle
	if settle <= 0 {
		settle = state.ReadySettleSession
	}
	deadline := cfg.Deadline
	if deadline <= 0 {
		deadline = state.ReadyDeadline
	}
	hookMode := cfg.Mode == state.ReadyModeHook && cfg.Nonce != "" && cfg.MarkerReady != nil

	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	first := true
	for {
		now := time.Now()
		switch {
		case hookMode && cfg.MarkerReady(cfg.Nonce):
			if first {
				syncLocalActivity(ctx, cfg)
			}
			markReady(ctx, cfg, "Harness ready")
			log.Debug("Harness readiness: hook marker seen")
			return readyOutcomeHookMarker
		case !hookMode && !now.Before(cfg.Start.Add(settle)):
			markReady(ctx, cfg, "Harness started")
			log.Debug("Harness readiness: settle period elapsed")
			return readyOutcomeSettled
		case hookMode && !now.Before(cfg.Start.Add(deadline)):
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
