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
	"io"
	"os/exec"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
)

// fakeAttachProber drives classifyAttachEnd's unit tests without any real
// runtime exec, so the full decision matrix can be tested directly.
type fakeAttachProber struct {
	probe  probeResult
	lookup lookupResult
}

func (f fakeAttachProber) probeHasSession(ctx context.Context) probeResult      { return f.probe }
func (f fakeAttachProber) lookupStillResolves(ctx context.Context) lookupResult { return f.lookup }

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
			name:       "start failed, container still resolves -> session not ready",
			startErr:   errors.New("waitForTmuxSession timed out"),
			prober:     fakeAttachProber{lookup: lookupResolves},
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
