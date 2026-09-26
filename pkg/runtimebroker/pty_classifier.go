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

// attachProber answers the two questions classifyAttachEnd needs once a PTY
// attach loop has ended: is the tmux session still there, and does the agent
// still resolve to a container? Both probes are bounded and best-effort — a
// fake implementation drives the unit tests in pty_classifier_test.go; the
// real implementation is attachEndProber below.
type attachProber interface {
	probeHasSession(ctx context.Context) probeResult
	lookupStillResolves(ctx context.Context) lookupResult
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
			return wsprotocol.ClosePTYUpstreamUnavailable, wsprotocol.CloseReasonSessionNotReady
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
	_, err := p.lookup.LookupAgent(ctx, p.slug, p.projectID)
	switch {
	case err == nil:
		return lookupResolves
	case errors.Is(err, ErrAgentListUnavailable):
		return lookupUnknown
	default:
		return lookupAbsent
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
