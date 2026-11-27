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

// The exec routes run only on Linux (execSupported; ExecEnabled never
// registers them elsewhere), and these tests use POSIX paths and PATH
// lists, so they build only there. exec_other_test.go covers the other
// platforms.

//go:build linux

package control

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/liveness"
)

type blockingExecProc struct {
	release chan struct{}
	final   execFinal
}

func (p *blockingExecProc) wait() execFinal {
	<-p.release
	return p.final
}

// TestExecRoute_LiveWork_LifecycleAndCollectedMark verifies design §4.10 / AC-F5:
//   - LiveWork is non-nil (KindExec, SuppressStall=false) while a run is live;
//   - LiveWork remains non-nil after finish while its tombstone is uncollected;
//   - an exec-start replay does NOT mark the tombstone collected;
//   - the first exec-status returning finished marks the tombstone collected
//     and clears LiveWork;
//   - an uncollected tombstone also clears once execOutputTTL elapses;
//   - a start_failed run does not hold LiveWork non-nil.
func TestExecRoute_LiveWork_LifecycleAndCollectedMark(t *testing.T) {
	var mu sync.Mutex
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	getNow := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	advance := func(d time.Duration) {
		mu.Lock()
		now = now.Add(d)
		mu.Unlock()
	}

	finishedCh := make(chan string, 4)
	releaseCh := make(chan struct{})
	e := newExecRoute(ExecOptions{
		Env:      func() ([]string, error) { return []string{"PATH=/bin:/usr/bin", "HOME=/tmp"}, nil },
		LaunchID: "launch-lw",
		Now:      getNow,
	})
	e.finished = func(runID string) { finishedCh <- runID }
	e.start = func(spec execSpec) (execProcess, error) {
		return &blockingExecProc{
			release: releaseCh,
			final: execFinal{
				outcome:      agentcontrol.ExecOutcomeOK,
				exitCode:     0,
				durationMS:   50,
				outputBytes:  5,
				capture:      agentcontrol.ExecCaptureTail,
				outputStatus: agentcontrol.ExecOutputCaptured,
				output:       []byte("done\n"),
			},
		}, nil
	}

	// Initially idle.
	if got := e.LiveWork(); got != nil {
		t.Fatalf("initial LiveWork = %+v, want nil", got)
	}

	req := &agentcontrol.ExecStartRequest{
		V:              agentcontrol.ExecVersion,
		RunID:          "run-livework-1",
		Argv:           []string{"/bin/sh", "-c", "echo done"},
		TimeoutS:       60,
		NotAfterUnixMS: getNow().Add(time.Minute).UnixMilli(),
		Capture:        agentcontrol.ExecCaptureTail,
	}
	ans := e.startRun(context.Background(), req, getNow())
	if ans.status != http.StatusAccepted {
		t.Fatalf("startRun status = %d, want 202 (%+v)", ans.status, ans)
	}

	// While live: KindExec, SuppressStall=false.
	lw := e.LiveWork()
	if lw == nil || lw.Kind != liveness.KindExec || lw.SuppressStall {
		t.Fatalf("while live: LiveWork = %+v, want KindExec/SuppressStall=false", lw)
	}

	// Finish the run; tombstone is uncollected so LiveWork stays non-nil.
	close(releaseCh)
	select {
	case <-finishedCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for run to finish")
	}

	lw = e.LiveWork()
	if lw == nil || lw.Kind != liveness.KindExec || lw.SuppressStall {
		t.Fatalf("after finish (uncollected): LiveWork = %+v, want KindExec/SuppressStall=false", lw)
	}

	// exec-start replay does NOT include output and must NOT mark collected.
	replayAns := e.startRun(context.Background(), req, getNow())
	if replayAns.status != http.StatusOK || !replayAns.resp.Replayed {
		t.Fatalf("replay startRun = %+v, want 200 replayed", replayAns)
	}
	if lw = e.LiveWork(); lw == nil || lw.Kind != liveness.KindExec {
		t.Fatalf("after exec-start replay: LiveWork = %+v, want still non-nil KindExec", lw)
	}

	// First exec-status returning finished with output marks collected -> LiveWork nil.
	stAns := e.status(context.Background(), req.RunID)
	if stAns.status != http.StatusOK || stAns.resp.State != agentcontrol.ExecStateFinished {
		t.Fatalf("exec-status = %+v, want 200 finished", stAns)
	}
	if got := e.LiveWork(); got != nil {
		t.Fatalf("after exec-status collection: LiveWork = %+v, want nil", got)
	}

	// Second run: never collected via exec-status; expires after execOutputTTL.
	release2 := make(chan struct{})
	e.start = func(spec execSpec) (execProcess, error) {
		return &blockingExecProc{
			release: release2,
			final: execFinal{
				outcome:      agentcontrol.ExecOutcomeOK,
				capture:      agentcontrol.ExecCaptureTail,
				outputStatus: agentcontrol.ExecOutputCaptured,
				output:       []byte("unpolled\n"),
			},
		}, nil
	}
	req2 := &agentcontrol.ExecStartRequest{
		V:              agentcontrol.ExecVersion,
		RunID:          "run-livework-2",
		Argv:           []string{"/bin/sh", "-c", "echo unpolled"},
		TimeoutS:       60,
		NotAfterUnixMS: getNow().Add(time.Minute).UnixMilli(),
		Capture:        agentcontrol.ExecCaptureTail,
	}
	ans2 := e.startRun(context.Background(), req2, getNow())
	if ans2.status != http.StatusAccepted {
		t.Fatalf("startRun 2 status = %d, want 202", ans2.status)
	}
	close(release2)
	select {
	case <-finishedCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for run 2 to finish")
	}
	if lw = e.LiveWork(); lw == nil || lw.Kind != liveness.KindExec {
		t.Fatalf("unpolled run 2: LiveWork = %+v, want KindExec", lw)
	}
	advance(execOutputTTL + time.Second)
	if got := e.LiveWork(); got != nil {
		t.Fatalf("after execOutputTTL: LiveWork = %+v, want nil", got)
	}

	// Third run: start_failed does not hold LiveWork non-nil (hub finishes on 202).
	req3 := &agentcontrol.ExecStartRequest{
		V:              agentcontrol.ExecVersion,
		RunID:          "run-livework-3",
		Argv:           []string{"/nonexistent-binary-for-livework-test"},
		TimeoutS:       60,
		NotAfterUnixMS: getNow().Add(time.Minute).UnixMilli(),
		Capture:        agentcontrol.ExecCaptureTail,
	}
	ans3 := e.startRun(context.Background(), req3, getNow())
	if ans3.status != http.StatusAccepted || ans3.resp.Outcome != agentcontrol.ExecOutcomeStartFailed {
		t.Fatalf("start_failed run = %+v, want 202 start_failed", ans3)
	}
	if got := e.LiveWork(); got != nil {
		t.Fatalf("after start_failed: LiveWork = %+v, want nil", got)
	}
}

// TestExecEnabled_BrokerReadySignalAndFallback verifies carry-forward I2:
// ExecEnabled keys on the broker-written SCION_HARNESS_READY signal when
// present so start-env SCION_HARNESS=shell cannot spoof shell-ness on an LLM
// agent, falling back to SCION_HARNESS only when SCION_HARNESS_READY is absent.
func TestExecEnabled_BrokerReadySignalAndFallback(t *testing.T) {
	idleReady := state.FormatReadySpecEnv(state.DefaultReadySpec(state.ReadyStrategyIdle))
	hookReady := state.FormatReadySpecEnv(state.DefaultReadySpec(state.ReadyStrategyHook))

	cases := []struct {
		name string
		env  map[string]string
		want bool
	}{
		{
			name: "broker idle signal enables command_requests even without SCION_HARNESS",
			env: map[string]string{
				EnvHubExperiments:     ExperimentCommandRequests,
				state.HarnessReadyEnv: idleReady,
			},
			want: true,
		},
		{
			name: "broker hook signal blocks spoofed SCION_HARNESS=shell",
			env: map[string]string{
				EnvHubExperiments:     ExperimentCommandRequests,
				state.HarnessReadyEnv: hookReady,
				"SCION_HARNESS":       state.ShellHarnessName,
			},
			want: false,
		},
		{
			name: "malformed SCION_HARNESS_READY fails closed even with SCION_HARNESS=shell",
			env: map[string]string{
				EnvHubExperiments:     ExperimentCommandRequests,
				state.HarnessReadyEnv: "not-json",
				"SCION_HARNESS":       state.ShellHarnessName,
			},
			want: false,
		},
		{
			name: "older broker without SCION_HARNESS_READY falls back to SCION_HARNESS=shell",
			env: map[string]string{
				EnvHubExperiments: ExperimentCommandRequests,
				"SCION_HARNESS":   state.ShellHarnessName,
			},
			want: true,
		},
		{
			name: "older broker with non-shell SCION_HARNESS stays false",
			env: map[string]string{
				EnvHubExperiments: ExperimentCommandRequests,
				"SCION_HARNESS":   "claude",
			},
			want: false,
		},
		{
			name: "exec_schedules enables on LLM agent regardless of ready signal",
			env: map[string]string{
				EnvHubExperiments:     ExperimentExecSchedules,
				state.HarnessReadyEnv: hookReady,
				"SCION_HARNESS":       "claude",
			},
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ExecEnabled(func(k string) string { return tc.env[k] })
			if got != tc.want {
				t.Fatalf("ExecEnabled = %v, want %v", got, tc.want)
			}
		})
	}
}
