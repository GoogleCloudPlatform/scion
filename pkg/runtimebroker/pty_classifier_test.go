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
	"fmt"
	"io"
	"os/exec"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
)

// String makes a lookupResult test failure readable (e.g. "lookupAbsent"
// instead of a bare "2") without adding a Stringer to the production type.
// Test-only: fmt's %v honors this automatically wherever a lookupResult is
// printed in this package's tests.
func (r lookupResult) String() string {
	switch r {
	case lookupUnknown:
		return "lookupUnknown"
	case lookupResolves:
		return "lookupResolves"
	case lookupAbsent:
		return "lookupAbsent"
	default:
		return fmt.Sprintf("lookupResult(%d)", int(r))
	}
}

// fakeAttachProber drives classifyAttachEnd's unit tests without any real
// runtime exec, so the full decision matrix can be tested directly.
type fakeAttachProber struct {
	probe   probeResult
	lookup  lookupResult
	running runningResult
}

func (f fakeAttachProber) probeHasSession(ctx context.Context) probeResult      { return f.probe }
func (f fakeAttachProber) lookupStillResolves(ctx context.Context) lookupResult { return f.lookup }
func (f fakeAttachProber) containerRunningState(ctx context.Context) runningResult {
	return f.running
}

func TestClassifyAttachEnd(t *testing.T) {
	tests := []struct {
		name       string
		startErr   error
		cleanExit  bool
		prober     fakeAttachProber
		wantCode   int
		wantReason string
	}{
		{
			name:       "start failed, container removed",
			startErr:   errors.New("waitForTmuxSession timed out"),
			prober:     fakeAttachProber{lookup: lookupAbsent},
			wantCode:   wsprotocol.ClosePTYSessionGone,
			wantReason: wsprotocol.CloseReasonContainerRemoved,
		},
		{
			name:       "start failed, lookup unavailable retries (never terminal)",
			startErr:   errors.New("waitForTmuxSession timed out"),
			prober:     fakeAttachProber{lookup: lookupUnknown},
			wantCode:   wsprotocol.ClosePTYUpstreamUnavailable,
			wantReason: wsprotocol.CloseReasonLookupUnavailable,
		},
		{
			// ptone/scion#2088: a container that is definitively not running
			// (Exited/stopped) must end the attach with a terminal 4410, not
			// the retry class 4503 — the session cannot come back until the
			// agent is started again.
			name:       "start failed, container resolves but is stopped -> terminal 4410",
			startErr:   errors.New("waitForTmuxSession timed out"),
			prober:     fakeAttachProber{lookup: lookupResolves, running: runningNo},
			wantCode:   wsprotocol.ClosePTYSessionGone,
			wantReason: wsprotocol.CloseReasonAgentStopped,
		},
		{
			// The container resolves and is confirmed running (e.g. still
			// starting up, tmux not launched yet): retry.
			name:       "start failed, container resolves and running -> session not ready",
			startErr:   errors.New("waitForTmuxSession timed out"),
			prober:     fakeAttachProber{lookup: lookupResolves, running: runningYes},
			wantCode:   wsprotocol.ClosePTYUpstreamUnavailable,
			wantReason: wsprotocol.CloseReasonSessionNotReady,
		},
		{
			// The container resolves, but whether it's running or stopped
			// couldn't be determined (the running-state probe itself failed
			// or the runtime reported no usable phase). An unknown running
			// state must never be downgraded to the terminal 4410 - retry.
			name:       "start failed, container resolves but running state unknown -> retries",
			startErr:   errors.New("waitForTmuxSession timed out"),
			prober:     fakeAttachProber{lookup: lookupResolves, running: runningUnknown},
			wantCode:   wsprotocol.ClosePTYUpstreamUnavailable,
			wantReason: wsprotocol.CloseReasonSessionNotReady,
		},
		{
			name:       "clean detach, session alive",
			cleanExit:  true,
			prober:     fakeAttachProber{probe: probeAlive},
			wantCode:   wsprotocol.ClosePTYNormal,
			wantReason: "",
		},
		{
			// A killed exec with the session still alive must retry (4503),
			// not report a terminal outcome.
			name:       "killed exec, session alive -> transport drop retries",
			cleanExit:  false,
			prober:     fakeAttachProber{probe: probeAlive},
			wantCode:   wsprotocol.ClosePTYUpstreamUnavailable,
			wantReason: wsprotocol.CloseReasonRuntimeStreamDropped,
		},
		{
			// A killed tmux session must give 4410 (terminal), not a retry.
			name:       "session gone, container removed",
			cleanExit:  true,
			prober:     fakeAttachProber{probe: probeAbsent, lookup: lookupAbsent},
			wantCode:   wsprotocol.ClosePTYSessionGone,
			wantReason: wsprotocol.CloseReasonContainerRemoved,
		},
		{
			name:       "session gone, container still present",
			cleanExit:  true,
			prober:     fakeAttachProber{probe: probeAbsent, lookup: lookupResolves},
			wantCode:   wsprotocol.ClosePTYSessionGone,
			wantReason: wsprotocol.CloseReasonSessionEnded,
		},
		{
			// A list-unavailable error must give 4503, never 4404 or 4410.
			name:       "session gone, lookup unavailable -> retries, never terminal",
			cleanExit:  true,
			prober:     fakeAttachProber{probe: probeAbsent, lookup: lookupUnknown},
			wantCode:   wsprotocol.ClosePTYUpstreamUnavailable,
			wantReason: wsprotocol.CloseReasonLookupUnavailable,
		},
		{
			name:       "probe itself failed or timed out -> retry",
			cleanExit:  true,
			prober:     fakeAttachProber{probe: probeUnknown},
			wantCode:   wsprotocol.ClosePTYInternalError,
			wantReason: wsprotocol.CloseReasonProbeFailed,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, reason := classifyAttachEnd(context.Background(), tc.startErr, tc.cleanExit, tc.prober)
			if code != tc.wantCode || reason != tc.wantReason {
				t.Errorf("classifyAttachEnd() = (%d, %q), want (%d, %q)", code, reason, tc.wantCode, tc.wantReason)
			}
		})
	}
}

// TestAttachEndProber_ContainerRunningState drives the real
// attachEndProber.containerRunningState (not fakeAttachProber, which
// TestClassifyAttachEnd uses) through a fake AgentLookup. TestClassifyAttachEnd
// alone cannot catch a broken production wiring between LookupAgent's Phase
// and the classifier's decision, since it never calls attachEndProber or
// AgentLookup at all.
func TestAttachEndProber_ContainerRunningState(t *testing.T) {
	tests := []struct {
		name   string
		lookup AgentLookup
		want   runningResult
	}{
		{
			name:   "stopped container -> runningNo",
			lookup: &fixedAgentLookup{result: &AgentLookupResult{ContainerID: "c1", Phase: "stopped"}},
			want:   runningNo,
		},
		{
			name:   "running container -> runningYes",
			lookup: &fixedAgentLookup{result: &AgentLookupResult{ContainerID: "c1", Phase: "running"}},
			want:   runningYes,
		},
		{
			name:   "lookup error -> runningUnknown, never guessed stopped",
			lookup: &fixedAgentLookup{err: errors.New("agent list unavailable")},
			want:   runningUnknown,
		},
		{
			name:   "nil lookup (no AgentLookup configured) -> runningUnknown",
			lookup: nil,
			want:   runningUnknown,
		},
		{
			// A well-behaved AgentLookup never returns (nil, nil), but a
			// test double or a future implementation might. This must never
			// dereference the nil result, and must never guess running or
			// stopped from it.
			name:   "nil result with nil error -> runningUnknown, never dereferenced",
			lookup: &fixedAgentLookup{result: nil, err: nil},
			want:   runningUnknown,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := &attachEndProber{lookup: tc.lookup, slug: "agent1", projectID: ""}
			if got := p.containerRunningState(context.Background()); got != tc.want {
				t.Errorf("containerRunningState() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAttachEndProber_LookupStillResolves drives attachEndProber.lookupStillResolves
// through a fake AgentLookup, covering the same tri-state decisions
// TestClassifyAttachEnd exercises through fakeAttachProber, but against the
// real prober method. It also covers the (nil, nil) edge case a well-behaved
// AgentLookup should never produce but a test double or future
// implementation might: that must map to lookupUnknown (retry), never
// lookupResolves and never lookupAbsent.
func TestAttachEndProber_LookupStillResolves(t *testing.T) {
	tests := []struct {
		name   string
		lookup AgentLookup
		want   lookupResult
	}{
		{
			name:   "resolves",
			lookup: &fixedAgentLookup{result: &AgentLookupResult{ContainerID: "c1"}},
			want:   lookupResolves,
		},
		{
			name:   "nil result with nil error -> unknown, never treated as resolves",
			lookup: &fixedAgentLookup{result: nil, err: nil},
			want:   lookupUnknown,
		},
		{
			name:   "list unavailable -> unknown, never absent",
			lookup: &fixedAgentLookup{err: fmt.Errorf("%w: docker ps failed", ErrAgentListUnavailable)},
			want:   lookupUnknown,
		},
		{
			name:   "genuine not-found -> absent",
			lookup: &fixedAgentLookup{err: errors.New("agent 'ghost' not found")},
			want:   lookupAbsent,
		},
		{
			name:   "nil lookup (no AgentLookup configured) -> unknown",
			lookup: nil,
			want:   lookupUnknown,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := &attachEndProber{lookup: tc.lookup, slug: "agent1", projectID: ""}
			if got := p.lookupStillResolves(context.Background()); got != tc.want {
				t.Errorf("lookupStillResolves() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAttachEndProber_LookupStillResolves_RealServerListErrors is a
// prober-level regression test wired to a real *Server, not a fake
// AgentLookup: it proves lookupStillResolves inherits LookupAgent's
// list-unavailable hardening, so a transient auxiliary-runtime or
// project-fallback List failure gives lookupUnknown (retry), never
// lookupAbsent — which classifyAttachEnd would otherwise turn into a false
// terminal 4410 container_removed for a container that was never actually
// gone, just briefly unreachable. TestLookupAgent_AuxiliaryListErrorSurfacesUnavailable
// and TestLookupAgent_PrimaryManagerFallbackListErrorSurfacesUnavailable
// (server_lookup_test.go) cover the same fakes against LookupAgent directly;
// this test exercises the prober's own call site, the seam that actually
// matters to the classifier.
func TestAttachEndProber_LookupStillResolves_RealServerListErrors(t *testing.T) {
	t.Run("auxiliary runtime List error", func(t *testing.T) {
		defaultMgr := &filteringMockManager{}
		defaultMgr.agents = []api.AgentInfo{}
		auxMgr := &mockManager{listErr: errors.New("docker ps: connection refused")}
		rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
		auxRt := &runtime.MockRuntime{NameFunc: func() string { return "kubernetes" }}
		srv := New(DefaultServerConfig(), defaultMgr, rt)
		srv.auxiliaryRuntimesMu.Lock()
		srv.auxiliaryRuntimes["kubernetes"] = auxiliaryRuntime{Runtime: auxRt, Manager: auxMgr}
		srv.auxiliaryRuntimesMu.Unlock()

		p := &attachEndProber{lookup: srv, slug: "ghost", projectID: ""}
		if got := p.lookupStillResolves(context.Background()); got != lookupUnknown {
			t.Errorf("lookupStillResolves() = %v, want lookupUnknown", got)
		}
	})

	t.Run("primary manager fallback List error", func(t *testing.T) {
		mgr := &scopedThenFailManager{failErr: errors.New("list: connection reset")}
		rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
		srv := New(DefaultServerConfig(), mgr, rt)

		p := &attachEndProber{lookup: srv, slug: "ghost", projectID: "some-project"}
		if got := p.lookupStillResolves(context.Background()); got != lookupUnknown {
			t.Errorf("lookupStillResolves() = %v, want lookupUnknown", got)
		}
	})
}

// TestIsCleanExit exercises the docker/podman/cloudrun-sandbox exit-status
// signal that feeds classifyAttachEnd's cleanExit argument.
func TestIsCleanExit(t *testing.T) {
	if isCleanExit(nil) {
		t.Error("nil ProcessState (never reaped) must not be treated as clean")
	}

	cleanCmd := exec.Command("true")
	if err := cleanCmd.Run(); err != nil {
		t.Fatalf("unexpected error running `true`: %v", err)
	}
	if !isCleanExit(cleanCmd.ProcessState) {
		t.Error("exit 0 must be treated as clean")
	}

	dirtyCmd := exec.Command("false")
	_ = dirtyCmd.Run() // expected non-nil error; ProcessState is still populated
	if isCleanExit(dirtyCmd.ProcessState) {
		t.Error("non-zero exit must not be treated as clean")
	}
}

// TestCleanExitFromCmd covers the wrapper both Run() implementations' final
// defers use to populate cleanExit: it must not panic on a nil *exec.Cmd
// (treating that the same as never having reaped a process), and must
// otherwise match isCleanExit exactly.
func TestCleanExitFromCmd(t *testing.T) {
	if cleanExitFromCmd(nil) {
		t.Error("nil *exec.Cmd must not be treated as clean")
	}

	cleanCmd := exec.Command("true")
	if err := cleanCmd.Run(); err != nil {
		t.Fatalf("unexpected error running `true`: %v", err)
	}
	if !cleanExitFromCmd(cleanCmd) {
		t.Error("a reaped, exit-0 cmd must be treated as clean")
	}

	dirtyCmd := exec.Command("false")
	_ = dirtyCmd.Run()
	if cleanExitFromCmd(dirtyCmd) {
		t.Error("a reaped, non-zero-exit cmd must not be treated as clean")
	}

	unreapedCmd := exec.Command("true") // never Run/Start: ProcessState is nil
	if cleanExitFromCmd(unreapedCmd) {
		t.Error("a non-nil cmd with a nil ProcessState must not be treated as clean")
	}
}

// TestClassifyProbeErr covers the tri-state mapping tmuxHasSession relies on.
func TestClassifyProbeErr(t *testing.T) {
	ctx := context.Background()

	if got := classifyProbeErr(ctx, nil); got != probeAlive {
		t.Errorf("nil error -> %v, want probeAlive", got)
	}

	dirtyCmd := exec.Command("false")
	err := dirtyCmd.Run()
	if got := classifyProbeErr(ctx, err); got != probeAbsent {
		t.Errorf("ExitError -> %v, want probeAbsent", got)
	}

	notFoundErr := &exec.Error{Name: "definitely-not-a-real-binary", Err: exec.ErrNotFound}
	if got := classifyProbeErr(ctx, notFoundErr); got != probeUnknown {
		t.Errorf("exec.Error (couldn't even start) -> %v, want probeUnknown", got)
	}

	cancelledCtx, cancel := context.WithCancel(ctx)
	cancel()
	if got := classifyProbeErr(cancelledCtx, errors.New("deadline")); got != probeUnknown {
		t.Errorf("cancelled probe context -> %v, want probeUnknown", got)
	}
}

// TestRunningResultFromPhase covers the mapping classifyAttachEnd's startErr
// branch relies on to tell a definitively-stopped container (ptone/scion#2088)
// from one that's merely still starting up or reported no usable phase.
// "error" is included alongside "stopped" per the same rule
// pkg/agent/list.go already applies (a crashed container is not running
// either).
func TestRunningResultFromPhase(t *testing.T) {
	cases := []struct {
		phase string
		want  runningResult
	}{
		{phase: "running", want: runningYes},
		{phase: "stopped", want: runningNo},
		{phase: "error", want: runningNo},
		{phase: "created", want: runningUnknown},
		{phase: "provisioning", want: runningUnknown},
		// Deliberately unknown, not runningNo: these are transitional
		// phases, not confirmed-stopped ones. Listed explicitly (rather
		// than relying on the "unrecognized phase" default below) so a
		// future change that adds one of these to the runningNo case is
		// caught as an intentional, reviewed decision rather than silently
		// passing this test.
		{phase: "starting", want: runningUnknown},
		{phase: "cloning", want: runningUnknown},
		{phase: "stopping", want: runningUnknown},
		{phase: "suspended", want: runningUnknown},
		{phase: "", want: runningUnknown},
		{phase: "some-future-phase-we-dont-know-about", want: runningUnknown},
	}
	for _, tc := range cases {
		if got := runningResultFromPhase(tc.phase); got != tc.want {
			t.Errorf("runningResultFromPhase(%q) = %v, want %v", tc.phase, got, tc.want)
		}
	}
}

// TestAwaitK8sExecEnd tests awaitK8sExecEnd's own ordering logic — the fix
// for the k8s (and LocalPTYSession) executor-error-vs-I/O-EOF race: the
// executor's own result and an I/O pump's end signal must never be collapsed
// onto one channel, or whichever arrives first — usually the I/O pump's EOF,
// since the caller closes the stdout pipe right before it sends the
// executor's result — silently decides "clean exit" regardless of what the
// executor actually reported. These orderings drive the helper directly with
// plain channels, so they need no real k8s cluster or tmux session.
//
// This covers the helper in isolation, built from hand-made channels rather
// than the real call sites. The call sites themselves — StreamPTYHandler's
// and LocalPTYSession's bridgeK8sExec, which is where execErrCh and errCh are
// actually created and wired to the executor and I/O goroutines — are
// covered separately in pty_k8s_bridge_test.go (TestBridgeK8sExec,
// TestLocalPTYSessionBridgeK8sExec) using a fake remotecommand.Executor.
func TestAwaitK8sExecEnd(t *testing.T) {
	const grace = 100 * time.Millisecond

	t.Run("io EOF first, then exec reports clean -> clean", func(t *testing.T) {
		execErrCh := make(chan error, 1)
		ioErrCh := make(chan error, 1)
		ioErrCh <- io.EOF
		go func() {
			time.Sleep(grace / 4)
			execErrCh <- nil
		}()

		cancelled := false
		err, clean := awaitK8sExecEnd(execErrCh, ioErrCh, func() { cancelled = true }, grace)

		if !errors.Is(err, io.EOF) {
			t.Errorf("err = %v, want io.EOF (the first signal)", err)
		}
		if !clean {
			t.Error("clean = false, want true: the executor reported a clean exit")
		}
		if !cancelled {
			t.Error("cancel was not called")
		}
	})

	t.Run("io EOF first, then exec reports an error -> not clean", func(t *testing.T) {
		execErrCh := make(chan error, 1)
		ioErrCh := make(chan error, 1)
		ioErrCh <- io.EOF
		transportErr := errors.New("transport dropped")
		go func() {
			time.Sleep(grace / 4)
			execErrCh <- transportErr
		}()

		_, clean := awaitK8sExecEnd(execErrCh, ioErrCh, func() {}, grace)

		if clean {
			t.Error("clean = true, want false: the executor reported a transport error, not a clean exit")
		}
	})

	t.Run("exec reports clean first -> clean", func(t *testing.T) {
		execErrCh := make(chan error, 1)
		ioErrCh := make(chan error, 1)
		execErrCh <- nil

		err, clean := awaitK8sExecEnd(execErrCh, ioErrCh, func() {}, grace)

		if err != nil {
			t.Errorf("err = %v, want nil", err)
		}
		if !clean {
			t.Error("clean = false, want true")
		}
	})

	t.Run("exec never reports -> not clean after the grace period", func(t *testing.T) {
		execErrCh := make(chan error, 1)
		ioErrCh := make(chan error, 1)
		ioErrCh <- errors.New("io pump ended")

		start := time.Now()
		_, clean := awaitK8sExecEnd(execErrCh, ioErrCh, func() {}, grace)
		elapsed := time.Since(start)

		if clean {
			t.Error("clean = true, want false: the executor never reported a result")
		}
		if elapsed < grace {
			t.Errorf("returned after %v, want at least the grace period %v", elapsed, grace)
		}
	})
}
