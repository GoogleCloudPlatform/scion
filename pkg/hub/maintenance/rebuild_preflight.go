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

package maintenance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/rebuildpreflight"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

const (
	defaultPreflightProbeTimeout = 10 * time.Second
	defaultPreflightRunTimeout   = 120 * time.Second
	defaultPreflightWaitDelay    = 5 * time.Second
)

// ColocatedBrokerFunc resolves the hub's legacy co-located broker ID for
// rebuild-server preflight. ready == false means registration is still
// pending (classified as not_ready). regErr != nil records a failed
// registration so the executor logs the distinction from a hub with no
// co-located broker (Q-G).
type ColocatedBrokerFunc func(ctx context.Context) (brokerID string, ready bool, regErr error)

// SetForceGrant implements ForceGrantee.
func (e *RebuildServerExecutor) SetForceGrant(grant *rebuildpreflight.ForceGrant) {
	e.grant = grant
	if grant != nil {
		e.state.RerunOf = grant.RerunOf
	}
}

// SetMaintenanceRunID records the maintenance run's ID for audit and log records.
func (e *RebuildServerExecutor) SetMaintenanceRunID(runID string) {
	e.runID = runID
}

// SetCheckpoint implements Checkpointer.
func (e *RebuildServerExecutor) SetCheckpoint(fn func(result map[string]any) error) {
	e.checkpoint = fn
}

// RunResult implements ResultReporter, returning a fresh copy of the run's
// structured result map (never setting "error").
func (e *RebuildServerExecutor) RunResult() map[string]any {
	return rebuildpreflight.RunResultMap(e.state)
}

// execPreflightCmd executes the newly built scion binary for either the
// --help support probe or the real `server preflight` run. Kept as a single
// function so the host-git door guard tracks its dynamic command start in
// one allowlist entry.
func execPreflightCmd(ctx context.Context, bin string, args []string, env []string, dir string, waitDelay time.Duration, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.WaitDelay = waitDelay
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

// runPreflightStage executes the built binary's preflight check (design 1.2-1.6, 3.4-3.6)
// and enforces the decision, checkpoints, and audit emission before install.
func (e *RebuildServerExecutor) runPreflightStage(ctx context.Context, logger io.Writer, bin string) error {
	log := logging.Subsystem("hub.maintenance.rebuild-server")
	_, _ = fmt.Fprintf(logger, "\n==> Preflight checks\n")

	// 1. Resolve co-located broker readiness (M1, M2, Q-G, CN7).
	var (
		brokerID string
		ready    bool
		regErr   error
	)
	if e.colocatedBroker != nil {
		brokerID, ready, regErr = e.colocatedBroker(ctx)
	}
	if regErr != nil {
		_, _ = fmt.Fprintf(logger, "Note: co-located broker registration failed (%v); running preflight without a co-located broker.\n", regErr)
		log.Warn("Co-located broker registration failed; running preflight with empty broker ID", "error", regErr)
	}

	// 2. Probe and run preflight when ready.
	var (
		probeRes rebuildpreflight.ProbeResult
		runRes   rebuildpreflight.RunResult
		dsn      string
	)
	if ready {
		probeRes = e.runPreflightProbe(ctx, bin)
		if probeRes.Attempted && probeRes.StartErr == nil && !probeRes.TimedOut && probeRes.ExitCode == 0 && strings.Contains(probeRes.Stdout, "--colocated-broker-id") {
			var err error
			dsn, err = e.resolvePreflightDSN()
			if err != nil {
				return fmt.Errorf("resolve preflight database DSN: %w", err)
			}
			runRes = e.runPreflightBinary(ctx, logger, bin, brokerID, dsn)
		}
	}

	// 3. Classify outcome (1.4, 1.5).
	outcome := rebuildpreflight.Classify(ready, probeRes, runRes)

	// Log redacted stdout preview (max 200 bytes) on unsupported / invalid_report (CN4).
	if outcome.CouldNotRun != nil && (outcome.CouldNotRun.Kind == rebuildpreflight.KindUnsupported || outcome.CouldNotRun.Kind == rebuildpreflight.KindInvalidReport) && len(runRes.Stdout) > 0 {
		preview := config.RedactDSNIn(string(runRes.Stdout), e.databaseDriver, dsn)
		if len(preview) > 200 {
			preview = preview[:200]
		}
		_, _ = fmt.Fprintf(logger, "preflight stdout preview (redacted): %s\n", strings.TrimSpace(preview))
	}

	// Record preflight / couldNotRun on executor state.
	e.state.Preflight = outcome.Report
	e.state.CouldNotRun = outcome.CouldNotRun

	// Log findings in writePreflightText format (1.6).
	if outcome.Report != nil {
		_, _ = io.WriteString(logger, rebuildpreflight.FormatPreflightText(*outcome.Report))
	} else if outcome.CouldNotRun != nil {
		f := outcome.CouldNotRun.Finding()
		_, _ = fmt.Fprintf(logger, "[BLOCK] %s: %s\n", f.Code, f.Message)
	}

	// 4. Decide proceed / stop (1.6, 3.4, Q-D, C1).
	dec := rebuildpreflight.Decide(outcome, e.grant)
	if !dec.Proceed {
		if e.grant != nil {
			e.state.ForceRejected = "findings changed"
		}
		return errors.New(dec.Reason)
	}

	// 5. Q-D: forced run found no blocking findings -> proceed unforced with forceNotNeeded.
	if dec.NotNeeded {
		forced := false
		e.state.Forced = &forced
		e.state.ForceNotNeeded = true
		_, _ = fmt.Fprintln(logger, "force not needed: no blocking findings")
		if e.checkpoint != nil {
			if err := e.checkpoint(e.RunResult()); err != nil {
				return fmt.Errorf("checkpoint preflight result: %w", err)
			}
		}
		return nil
	}

	// 6. Forced override path (3.5, 3.6, CN-e, C5, S4-S6).
	if dec.Forced {
		if e.checkpoint == nil {
			return errors.New("internal: a forced run has no checkpoint")
		}
		now := e.now().UTC()
		ov := rebuildpreflight.NewOverride(e.grant, outcome, now)
		forced := true
		applied := false
		e.state.Forced = &forced
		e.state.OverrideApplied = &applied
		e.state.Override = ov

		// Step 1: log PREFLIGHT OVERRIDDEN line before Write 1.
		summary := ""
		var blocking int
		var cnrKind, cnrClass string
		if outcome.CouldNotRun != nil {
			cnrKind = outcome.CouldNotRun.Kind
			cnrClass = outcome.CouldNotRun.Class
			summary = fmt.Sprintf("could not run (%s: %s)", cnrKind, cnrClass)
		} else if outcome.Report != nil {
			blocking = outcome.Report.Blocking
			summary = fmt.Sprintf("%d blocking finding(s)", blocking)
		}
		_, _ = fmt.Fprintf(logger, "PREFLIGHT OVERRIDDEN by force by %s (%s) at %s for commit %s: %s\n",
			e.grant.ActorEmail, e.grant.ActorID, ov.At, e.grant.PinnedCommit, summary)

		// Steps 2-4: Write 1 (forced row, overrideApplied: false), Write 2 (failed row forcedBy),
		// Write 1b (forced row, overrideApplied: true).
		if err := e.checkpoint(e.RunResult()); err != nil {
			// C5: leave overrideApplied: false and override intact on failure.
			return err
		}
		applied = true
		e.state.OverrideApplied = &applied

		// Step 5: Hub Warn (no finding text).
		log.Warn("maintenance rebuild-server preflight overridden by force",
			"run_id", e.runID,
			"rerun_of", e.grant.RerunOf,
			"actor_id", e.grant.ActorID,
			"actor_email", e.grant.ActorEmail,
			"blocking", blocking,
			"could_not_run_kind", cnrKind,
			"could_not_run_class", cnrClass,
			"commit", e.grant.PinnedCommit,
		)

		// Step 6: Audit event (S4-S6, fail-open).
		ev := &rebuildpreflight.MaintenanceOverrideEvent{
			EventType:        rebuildpreflight.MaintenanceOverrideEventType,
			ActorID:          e.grant.ActorID,
			ActorEmail:       e.grant.ActorEmail,
			Timestamp:        now,
			RunID:            e.runID,
			RerunOf:          e.grant.RerunOf,
			Operation:        rebuildpreflight.RebuildServerKey,
			Commit:           e.grant.PinnedCommit,
			Branch:           e.grant.PinnedBranch,
			Blocking:         blocking,
			CouldNotRunKind:  cnrKind,
			CouldNotRunClass: cnrClass,
		}
		rebuildpreflight.EmitOverride(ctx, e.audit, ev, func(msg string, args ...any) {
			log.Error(msg, args...)
		})
		return nil
	}

	// 7. Unforced clean path: checkpoint with forced: false (CN-e: nil checkpoint is a no-op unforced).
	forced := false
	e.state.Forced = &forced
	if e.checkpoint != nil {
		if err := e.checkpoint(e.RunResult()); err != nil {
			return fmt.Errorf("checkpoint preflight result: %w", err)
		}
	}
	return nil
}

func (e *RebuildServerExecutor) runPreflightProbe(ctx context.Context, bin string) rebuildpreflight.ProbeResult {
	timeout := e.preflightProbeTimeout
	if timeout <= 0 {
		timeout = defaultPreflightProbeTimeout
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var stdout, stderr bytes.Buffer
	err := execPreflightCmd(probeCtx, bin, []string{"server", "preflight", "--help"}, os.Environ(), os.TempDir(), e.preflightWaitDelay, &stdout, &stderr)
	res := rebuildpreflight.ProbeResult{
		Attempted: true,
		Stdout:    stdout.String(),
	}
	if err != nil {
		if errors.Is(probeCtx.Err(), context.DeadlineExceeded) {
			res.TimedOut = true
		} else {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				res.ExitCode = exitErr.ExitCode()
			} else {
				res.StartErr = err
			}
		}
	}
	return res
}

func (e *RebuildServerExecutor) resolvePreflightDSN() (string, error) {
	if strings.EqualFold(e.databaseDriver, "postgres") || strings.EqualFold(e.databaseDriver, "postgresql") {
		return e.databaseDSN, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		cwd = ""
	}
	return rebuildpreflight.AbsSQLiteDSN(e.databaseDSN, cwd)
}

func (e *RebuildServerExecutor) runPreflightBinary(ctx context.Context, logger io.Writer, bin, brokerID, dsn string) rebuildpreflight.RunResult {
	log := logging.Subsystem("hub.maintenance.rebuild-server")
	timeout := e.preflightRunTimeout
	if timeout <= 0 {
		timeout = defaultPreflightRunTimeout
	}
	waitDelay := e.preflightWaitDelay
	if waitDelay <= 0 {
		waitDelay = defaultPreflightWaitDelay
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	argv, env := rebuildpreflight.PreflightCommand(bin, brokerID, e.configPath, os.Environ(), dsn)
	// Debug log carries argv only, never env (4.1).
	log.Debug("Executing preflight binary", "argv", fmt.Sprint(argv), "dir", os.TempDir())

	stdoutCap := rebuildpreflight.NewCappedWriter(rebuildpreflight.MaxStdoutBytes)
	stderrCap := rebuildpreflight.NewCapture()

	err := execPreflightCmd(runCtx, argv[0], argv[1:], env, os.TempDir(), waitDelay, stdoutCap, stderrCap)

	// R3: capture -> drop partial lines -> redact -> truncate BEFORE writing to run log.
	redactedStderr := stderrCap.Redacted(e.databaseDriver, dsn)
	if redactedStderr != "" {
		_, _ = io.WriteString(logger, redactedStderr)
		if !strings.HasSuffix(redactedStderr, "\n") {
			_, _ = io.WriteString(logger, "\n")
		}
	}

	res := rebuildpreflight.RunResult{
		Stdout:         stdoutCap.Bytes(),
		StdoutOversize: stdoutCap.Oversize(),
		RedactedStderr: redactedStderr,
	}
	if err != nil {
		if errors.Is(err, exec.ErrWaitDelay) {
			// CN-b: WaitDelay does not change classification; note it in the log.
			res.WaitDelay = true
			_, _ = fmt.Fprintln(logger, "Note: preflight subprocess held output pipes open past WaitDelay.")
		}
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			res.TimedOut = true
		} else {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				code := exitErr.ExitCode()
				if code < 0 {
					res.Signal = strings.TrimPrefix(exitErr.Error(), "signal: ")
				} else {
					res.ExitCode = code
				}
			} else if !errors.Is(err, exec.ErrWaitDelay) {
				res.StartErr = err
			}
		}
	}
	return res
}

func (e *RebuildServerExecutor) now() time.Time {
	if e.nowFn != nil {
		return e.nowFn()
	}
	return time.Now()
}
