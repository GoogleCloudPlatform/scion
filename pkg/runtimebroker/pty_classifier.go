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

package runtimebroker

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	k8sexecutil "k8s.io/client-go/util/exec"
)

// probeResult is the tri-state outcome of a single post-exit
// "tmux has-session" probe.
type probeResult int

const (
	// probeUnknown means the probe itself failed or timed out (the runtime
	// was unreachable, or the check could not be run at all). It must never
	// be treated the same as probeAbsent: we don't know whether the session
	// survived.
	probeUnknown probeResult = iota
	// probeAlive means tmux reported the session exists.
	probeAlive
	// probeAbsent means tmux ran and reported no such session.
	probeAbsent
)

// lookupResult is the tri-state outcome of re-resolving an agent to a
// container after a PTY attach ends.
type lookupResult int

const (
	// lookupUnknown means the runtime's list call itself failed
	// (ErrAgentListUnavailable), so whether the container still exists is
	// unknown. It must never be treated the same as lookupAbsent.
	lookupUnknown lookupResult = iota
	// lookupResolves means the agent still resolves to a container.
	lookupResolves
	// lookupAbsent means the list call succeeded and found no such agent.
	lookupAbsent
)

// runningResult is the tri-state outcome of checking whether an agent's
// container is definitively running right now. It is consulted only when
// the tmux exec never started (startErr != nil) and the agent still
// resolves to a container: that combination is ambiguous by itself (the
// container could be mid-startup, or it could already be Exited), and only
// this check tells the two apart.
type runningResult int

const (
	// runningUnknown means the runtime couldn't say (the lookup itself
	// failed, or the runtime reported no phase/status at all). It must
	// never be treated the same as runningStopped: an unresolvable running
	// state always retries.
	runningUnknown runningResult = iota
	// runningYes means the runtime reports the container is up.
	runningYes
	// runningNo means the runtime reports the container is definitively not
	// running (e.g. Exited/stopped). The tmux session inside it cannot come
	// back on its own; only restarting the agent will help.
	runningNo
)

// attachProber answers the questions classifyAttachEnd needs once a PTY
// attach loop has ended: is the tmux session still there, does the agent
// still resolve to a container, and (when the exec never even started) is
// that container definitively running or stopped? All three probes are
// bounded and best-effort — a fake implementation drives the unit tests in
// pty_classifier_test.go; the real implementation is attachEndProber below.
type attachProber interface {
	probeHasSession(ctx context.Context) probeResult
	lookupStillResolves(ctx context.Context) lookupResult
	// containerRunningState reports whether the agent's container is
	// definitively running or stopped right now. Only called when
	// lookupStillResolves has already returned lookupResolves, so
	// implementations may assume the agent resolves to a container.
	containerRunningState(ctx context.Context) runningResult
}

// classifyAttachEnd decides the WebSocket/control-channel close code and
// machine-readable reason to report once a broker-side PTY attach loop has
// ended. It never downgrades an unknown to a terminal outcome: a probe or
// lookup failure always retries rather than reporting 4404/4410, so a
// transient failure to check on the session is never mistaken for the
// session actually being gone.
//
// startErr is non-nil when the attach never got as far as running the tmux
// exec (e.g. waitForTmuxSession timed out before the session was ready).
// cleanExit is true only when the exec process that ran `tmux
// attach-session` exited zero (docker/podman/cloudrun-sandbox), or the k8s
// executor reported no error (see isCleanExit). Callers must not invoke this
// function when the Hub already initiated the close (handler.closed): in
// that case the peer is already gone, so no probe should run and nothing
// should be sent.
func classifyAttachEnd(ctx context.Context, startErr error, cleanExit bool, prober attachProber) (code int, reason string) {
	if startErr != nil {
		switch prober.lookupStillResolves(ctx) {
		case lookupAbsent:
			return wsprotocol.ClosePTYSessionGone, wsprotocol.CloseReasonContainerRemoved
		case lookupUnknown:
			return wsprotocol.ClosePTYUpstreamUnavailable, wsprotocol.CloseReasonLookupUnavailable
		default: // lookupResolves
			// The container still resolves, but the tmux exec never started
			// (e.g. waitForTmuxSession timed out). That alone is ambiguous:
			// the agent could be starting up (retry) or its container could
			// already be Exited, in which case tmux will never appear no
			// matter how long we wait. Check which one it definitively is
			// before deciding — an unknown running state must still retry,
			// same as an unknown lookup does.
			switch prober.containerRunningState(ctx) {
			case runningNo:
				return wsprotocol.ClosePTYSessionGone, wsprotocol.CloseReasonAgentStopped
			default: // runningYes, runningUnknown
				return wsprotocol.ClosePTYUpstreamUnavailable, wsprotocol.CloseReasonSessionNotReady
			}
		}
	}

	switch prober.probeHasSession(ctx) {
	case probeAlive:
		if cleanExit {
			// The tmux client detached (or was detached by another client);
			// the session is untouched. Do not retry.
			return wsprotocol.ClosePTYNormal, ""
		}
		// The session survived, but the exec transport that carried this
		// client's attach did not end cleanly: a killed process, a
		// docker/podman exec transport drop, or (once the k8s race is
		// fixed) a k8s apiserver stream error. Retry.
		return wsprotocol.ClosePTYUpstreamUnavailable, wsprotocol.CloseReasonRuntimeStreamDropped
	case probeAbsent:
		switch prober.lookupStillResolves(ctx) {
		case lookupAbsent:
			return wsprotocol.ClosePTYSessionGone, wsprotocol.CloseReasonContainerRemoved
		case lookupUnknown:
			// We can't tell whether the container is still there. A gone
			// tmux session plus an unknown container state must still
			// retry, never 4410 — the container may well be fine and the
			// runtime listing may recover on the next attempt.
			return wsprotocol.ClosePTYUpstreamUnavailable, wsprotocol.CloseReasonLookupUnavailable
		default: // lookupResolves
			return wsprotocol.ClosePTYSessionGone, wsprotocol.CloseReasonSessionEnded
		}
	default: // probeUnknown
		return wsprotocol.ClosePTYInternalError, wsprotocol.CloseReasonProbeFailed
	}
}

// isCleanExit reports whether the runtime exec process that ran `tmux
// attach-session` exited zero. state is nil when the process was never
// started or never reaped (treated conservatively as not clean, since we
// have no evidence the exit was orderly). This is the docker/podman/
// cloudrun-sandbox signal; the k8s path uses the executor's own error
// instead (see awaitK8sExecEnd).
func isCleanExit(state *os.ProcessState) bool {
	return state != nil && state.Success()
}

// cleanExitFromCmd reports whether cmd represents a clean exit, treating a
// nil cmd the same as a nil ProcessState: not clean. A nil cmd should not
// happen — both Run() implementations only reach the defer that calls this
// after the exec has already been started (cmd assigned) — but the field
// itself does not encode that guarantee, so this stays safe against a
// future change to that ordering instead of a nil-pointer panic.
func cleanExitFromCmd(cmd *exec.Cmd) bool {
	if cmd == nil {
		return false
	}
	return isCleanExit(cmd.ProcessState)
}

// awaitK8sExecEnd waits for a k8s PTY bridge (StreamPTYHandler.runK8sExec or
// LocalPTYSession.runK8sExec) to end, and resolves both "when to stop" and
// "was the tmux client's exit clean" from two independent signals:
//
//   - execErrCh carries only the SPDY executor's own result (nil on a clean
//     exit, non-nil on a transport/apiserver drop). It must never share a
//     channel with an I/O goroutine's own error: the caller closes the
//     stdout pipe right before it sends the executor's result, and the
//     stdout reader's resulting EOF can race the executor's real error into
//     a shared channel — usually winning, which would silently turn a
//     transport drop into a false "clean exit".
//   - ioErrCh carries the "something else ended the session" signal from the
//     I/O pump goroutines (stdout read failure, control-channel/WebSocket
//     closed, context cancelled).
//
// Whichever of the two ends the session first determines the returned err
// (matching the pre-fix behavior for logging/return purposes). clean is
// always resolved from execErrCh: if it wasn't the first signal, this waits
// up to grace for the executor to report its own result before giving up
// and treating the end as not-clean — never guessing "clean" from an I/O
// signal alone.
//
// cancel is invoked immediately once the first signal arrives (before the
// optional grace wait), matching both callers' original ordering: it
// unblocks the executor and any related pumps so grace has a chance to be
// short in practice.
func awaitK8sExecEnd(execErrCh, ioErrCh <-chan error, cancel func(), grace time.Duration) (err error, clean bool) {
	var haveExecResult bool
	select {
	case execErr := <-execErrCh:
		err = execErr
		clean = execErr == nil
		haveExecResult = true
	case ioErr := <-ioErrCh:
		err = ioErr
	}
	cancel()

	if !haveExecResult {
		select {
		case execErr := <-execErrCh:
			clean = execErr == nil
		case <-time.After(grace):
			// The executor did not report its own result promptly after
			// cancellation; treat this as not-clean rather than guess.
			clean = false
		}
	}
	return err, clean
}

// attachProbeTimeout bounds each post-exit probe exec (tmux has-session, or
// the agent lookup) so a broker with a wedged runtime cannot hang the whole
// classification step.
const attachProbeTimeout = 2 * time.Second

// attachEndProber is the production attachProber. It reuses the exact
// per-runtime exec paths that already start and prepare a PTY attach
// (tmuxHasSession, AgentLookup.LookupAgent) so the probe adds no new
// runtime integration.
type attachEndProber struct {
	lookup    AgentLookup // may be nil (e.g. no agent lookup configured)
	slug      string
	projectID string

	runtimeCmd  string
	containerID string
	namespace   string
	execUser    string

	k8sConfig    *rest.Config
	k8sClientset kubernetes.Interface
}

func (p *attachEndProber) probeHasSession(ctx context.Context) probeResult {
	ctx, cancel := context.WithTimeout(ctx, attachProbeTimeout)
	defer cancel()
	return tmuxHasSession(ctx, p.runtimeCmd, p.containerID, p.namespace, p.execUser, p.k8sConfig, p.k8sClientset)
}

func (p *attachEndProber) lookupStillResolves(ctx context.Context) lookupResult {
	if p.lookup == nil {
		return lookupUnknown
	}
	ctx, cancel := context.WithTimeout(ctx, attachProbeTimeout)
	defer cancel()
	// LookupAgent, not LookupContainerID: both now classify list failures
	// identically, returning ErrAgentListUnavailable from every
	// auxiliary-runtime and project-fallback List failure (see
	// TestLookupAgent_*ListErrorSurfacesUnavailable and
	// TestLookupContainerID_*ListErrorSurfacesUnavailable in
	// server_lookup_test.go). LookupAgent is used here because it performs
	// the same resolution the attach path itself relies on. The result
	// (including its Phase re-resolution) is discarded either way; only the
	// error's shape matters.
	result, err := p.lookup.LookupAgent(ctx, p.slug, p.projectID)
	switch {
	case err == nil && result != nil:
		return lookupResolves
	case err == nil:
		// A well-behaved AgentLookup never returns (nil, nil), but a test
		// double or a future implementation might. Don't read that as
		// "resolves" — treat it the same as a list-unavailable failure.
		return lookupUnknown
	case errors.Is(err, ErrAgentListUnavailable):
		return lookupUnknown
	default:
		return lookupAbsent
	}
}

// containerRunningState re-resolves the agent and reports whether its
// container is definitively running or stopped, from AgentLookupResult's
// Phase — which LookupAgent populates from the container runtime's own
// unmerged listing (rawRuntimePhase in server.go), not from agent.Manager's
// agent-info.json-overlaid view. That distinction matters here specifically:
// the overlay can keep reporting a stale "stopped"/"error" phase for a
// container that has since restarted or is still starting up, which would
// otherwise turn a transient state into a false terminal 4410. It issues its
// own lookup rather than reusing lookupStillResolves's: the two are called
// from different branches of classifyAttachEnd (this one only when the exec
// never started), and keeping them independent means a failure here can
// never be mistaken for lookupStillResolves's own tri-state.
func (p *attachEndProber) containerRunningState(ctx context.Context) runningResult {
	if p.lookup == nil {
		return runningUnknown
	}
	ctx, cancel := context.WithTimeout(ctx, attachProbeTimeout)
	defer cancel()
	result, err := p.lookup.LookupAgent(ctx, p.slug, p.projectID)
	if err != nil || result == nil {
		// Includes ErrAgentListUnavailable and a genuine not-found: either
		// way we can't confirm the container is stopped, so don't guess. A
		// nil result with a nil error is not a contract a well-behaved
		// AgentLookup should produce, but a test double or a future
		// implementation might — don't dereference it, and don't guess
		// "running" or "stopped" from it either.
		return runningUnknown
	}
	return runningResultFromPhase(result.Phase)
}

// runningResultFromPhase maps a raw, unmerged runtime.List-derived lifecycle
// phase (see api.AgentInfo.Phase and runtime.phaseFromContainerStatus; NOT
// agent.Manager's agent-info.json-overlaid phase — see rawRuntimePhase in
// server.go) to a runningResult, using the same stopped/error-means-stopped
// rule
// pkg/agent/list.go already applies when reconciling an agent's phase with
// its container status. "created" and "provisioning" mean the container
// exists but hasn't reached "running" yet — exactly the state
// waitForTmuxSession is normally waiting out — so those must retry rather
// than terminate. An empty or unrecognized phase (an older runtime that
// doesn't populate it) is unknown, not stopped.
func runningResultFromPhase(phase string) runningResult {
	switch state.Phase(phase) {
	case state.PhaseRunning:
		return runningYes
	case state.PhaseStopped, state.PhaseError:
		return runningNo
	default:
		return runningUnknown
	}
}

// tmuxHasSession runs a single non-blocking "tmux has-session" check against
// the container's tmux server, using the same per-runtime exec paths as
// waitForTmuxSession (factored out so both the readiness poll and the
// post-exit probe share one implementation).
func tmuxHasSession(ctx context.Context, runtimeCmd, containerID, namespace, execUser string, k8sConfig *rest.Config, k8sClientset kubernetes.Interface) probeResult {
	execUser = sanitizeExecUser(execUser)
	isK8s := runtimeCmd == "kubernetes" || runtimeCmd == "k8s"
	isCloudRunSandbox := runtimeCmd == "cloudrun-sandbox"

	if ctx.Err() != nil {
		return probeUnknown
	}

	var checkErr error
	switch {
	case isCloudRunSandbox:
		cmd := exec.CommandContext(ctx, cloudRunSandboxBin, "exec", containerID, "--",
			"/usr/bin/tmux", "has-session", "-t", "scion")
		checkErr = cmd.Run()
	case isK8s && k8sConfig != nil && k8sClientset != nil:
		// The tmux session runs as the scion user (via sciontool init
		// privilege drop), so check as that user — root can't see scion's
		// tmux socket.
		checkErr = k8sExecCheck(ctx, k8sConfig, k8sClientset, namespace, containerID, runtime.ExecAsUserCmd(execUser, "tmux has-session -t scion"))
	default:
		cmd := exec.CommandContext(ctx, runtimeCmd, "exec", "--user", execUser, containerID, "tmux", "has-session", "-t", "scion")
		checkErr = cmd.Run()
	}

	return classifyProbeErr(ctx, checkErr)
}

// classifyProbeErr turns a has-session exec result into a probeResult. A nil
// error means the session exists. A process that ran and exited non-zero
// (the common case: tmux itself reporting "can't find session") means the
// session is absent — this covers both a plain os/exec.ExitError (docker,
// podman, cloudrun-sandbox exec) and the k8s client-go equivalent
// (k8s.io/client-go/util/exec.ExitError, returned by remotecommand). Any
// other failure — the probe's own context deadline, or the exec never
// starting at all (runtime unreachable) — is unknown, so the caller retries
// instead of guessing the session is gone.
func classifyProbeErr(ctx context.Context, err error) probeResult {
	if err == nil {
		return probeAlive
	}
	if ctx.Err() != nil {
		return probeUnknown
	}
	var stdExitErr *exec.ExitError
	if errors.As(err, &stdExitErr) {
		return probeAbsent
	}
	var k8sExitErr k8sexecutil.ExitError
	if errors.As(err, &k8sExitErr) {
		return probeAbsent
	}
	return probeUnknown
}
