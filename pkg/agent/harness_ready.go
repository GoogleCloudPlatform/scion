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

package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
)

// Default harness-readiness wait timings. See waitHarnessReady.
const (
	defaultReadyPollInterval  = 500 * time.Millisecond
	defaultReadyDeadline      = 90 * time.Second
	defaultReadySettle        = 1500 * time.Millisecond
	defaultReadySettleSession = 8 * time.Second
	readyProbeExecTimeout     = 5 * time.Second
)

// harnessReadyTimings holds the readiness wait timings. A zero field uses
// its default.
type harnessReadyTimings struct {
	// PollInterval is the delay between probes.
	PollInterval time.Duration
	// Deadline bounds the whole wait, measured from the start of the flush.
	Deadline time.Duration
	// Settle is slept once readiness first appears during the wait, giving
	// the harness's input a moment to mount after its hook fired.
	Settle time.Duration
	// SettleFallback is the minimum tmux session age that counts as ready
	// in settle mode, or when the image writes no init-run file.
	SettleFallback time.Duration
}

func (t harnessReadyTimings) withDefaults() harnessReadyTimings {
	if t.PollInterval <= 0 {
		t.PollInterval = defaultReadyPollInterval
	}
	if t.Deadline <= 0 {
		t.Deadline = defaultReadyDeadline
	}
	if t.Settle <= 0 {
		t.Settle = defaultReadySettle
	}
	if t.SettleFallback <= 0 {
		t.SettleFallback = defaultReadySettleSession
	}
	return t
}

// readyModeNoInitRun labels a probe of an image that writes no init-run file
// (an older sciontool). It is handled like settle mode.
const readyModeNoInitRun = "no-init-run"

// readinessProbeArgv is the single command run per probe. It reports whether
// tmux session "scion" exists and when it was created, the container's
// current time (so the session age uses one clock), and the first line of
// each readiness file, size-capped. It always exits 0; missing pieces show
// up as empty values.
func readinessProbeArgv() []string {
	script := `if tmux has-session -t scion 2>/dev/null; then ` +
		`echo "session=$(tmux display-message -p -t scion: '#{session_created}' 2>/dev/null)"; ` +
		`else echo "session="; fi; ` +
		`echo "now=$(date +%s)"; ` +
		`echo "initrun=$(head -c 256 ` + state.HarnessInitRunFile + ` 2>/dev/null | head -n 1)"; ` +
		`echo "ready=$(head -c 256 ` + state.HarnessReadyFile + ` 2>/dev/null | head -n 1)"`
	return []string{"sh", "-c", script}
}

// readinessProbe is the parsed result of one probe.
type readinessProbe struct {
	// Ready reports whether the harness can take a pasted message.
	Ready bool
	// Mode is state.ReadyModeHook, state.ReadyModeSettle or
	// readyModeNoInitRun.
	Mode string
	// Nonce is the init-run nonce, empty without a valid init-run file.
	Nonce string
	// Reason says why the probe is not ready; empty when Ready.
	Reason string
}

// parseReadinessProbe interprets readinessProbeArgv's output.
//
// Hook mode is ready when the tmux session exists and the ready marker holds
// the init-run nonce; a marker from an earlier run (another nonce) does not
// count, and there is no session-age fallback. Settle mode, or no valid
// init-run file (an older image), is ready when the session exists and is
// at least settleFallback old.
func parseReadinessProbe(out string, settleFallback time.Duration) readinessProbe {
	fields := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if key, val, ok := strings.Cut(strings.TrimRight(line, "\r"), "="); ok {
			if _, seen := fields[key]; !seen {
				fields[key] = val
			}
		}
	}

	res := readinessProbe{Mode: readyModeNoInitRun}
	if nonce, mode, ok := state.ParseInitRun(fields["initrun"]); ok {
		res.Mode, res.Nonce = mode, nonce
	}

	created, err := strconv.ParseInt(strings.TrimSpace(fields["session"]), 10, 64)
	if err != nil || created <= 0 {
		res.Reason = "tmux session not found"
		return res
	}

	if res.Mode == state.ReadyModeHook {
		marker, ok := state.ParseReadyMarker(fields["ready"])
		switch {
		case !ok:
			res.Reason = "ready marker missing"
		case marker != res.Nonce:
			res.Reason = "ready marker from another run"
		default:
			res.Ready = true
		}
		return res
	}

	now, err := strconv.ParseInt(strings.TrimSpace(fields["now"]), 10, 64)
	if err != nil {
		res.Reason = "container time unavailable"
		return res
	}
	if age := time.Duration(now-created) * time.Second; age < settleFallback {
		res.Reason = fmt.Sprintf("session younger than %s", settleFallback)
		return res
	}
	res.Ready = true
	return res
}

// waitHarnessReady waits until the agent's harness is ready to read a pasted
// message, polling with one exec per probe (see parseReadinessProbe for what
// "ready" means per mode). It is MessageBuffer.waitReady for buffered
// flushes.
//
//   - It runs in the asynchronous buffered flush, after the runtime has
//     already answered the hub, so senders never block on it.
//   - Interrupts are delivered synchronously by Message and never wait.
//   - A message can therefore sit in memory for up to the deadline (90s by
//     default) before it is pasted; that window is covered by the hub
//     re-send tracked in ptone/scion#4487.
//   - A blocking wait option for senders is out of scope (ptone/scion#4485).
//
// It waits once per flush, never per retry, and does not hold the injection
// lock. If readiness first appears during the wait, it sleeps the settle
// period before returning. At the deadline it logs a warning and returns,
// and the message is pasted as before; that is not a delivery failure. It
// returns at once when ctx is cancelled (MessageBuffer.Close), so shutdown
// never waits. Any error resolving the target returns at once too: delivery
// then reports it as before.
func (m *AgentManager) waitHarnessReady(ctx context.Context, agentID, projectID string) {
	timings := m.readyTimings.withDefaults()
	start := time.Now()
	deadline := start.Add(timings.Deadline)

	target, err := m.findMessageTarget(ctx, agentID, projectID)
	if err != nil {
		return
	}

	probe := func() readinessProbe {
		probeTimeout := time.Until(deadline)
		if probeTimeout > readyProbeExecTimeout {
			probeTimeout = readyProbeExecTimeout
		}
		if probeTimeout <= 0 {
			probeTimeout = time.Millisecond
		}
		pctx, cancel := context.WithTimeout(ctx, probeTimeout)
		defer cancel()
		out, err := m.Runtime.Exec(pctx, target.ContainerID, readinessProbeArgv())
		if err != nil {
			return readinessProbe{Mode: readyModeNoInitRun, Reason: "probe failed: " + err.Error()}
		}
		return parseReadinessProbe(out, timings.SettleFallback)
	}

	for first := true; ; first = false {
		res := probe()
		if res.Ready {
			if first {
				return
			}
			if !sleepCtx(ctx, timings.Settle) {
				return
			}
			slog.Info("message delivery waited for harness readiness",
				"agent", agentID,
				"mode", res.Mode,
				"waited_ms", time.Since(start).Milliseconds(),
			)
			return
		}
		if ctx.Err() != nil {
			return
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			slog.Warn("harness not ready by deadline; delivering message anyway",
				"agent", agentID,
				"mode", res.Mode,
				"waited_ms", time.Since(start).Milliseconds(),
				"reason", res.Reason,
			)
			return
		}
		if !sleepCtx(ctx, min(timings.PollInterval, remaining)) {
			return
		}
	}
}

// sleepCtx sleeps for d, returning false early if ctx is cancelled.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
