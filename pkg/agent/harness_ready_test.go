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
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

func probeOutput(sessionCreated, now int64, initRun, ready string) string {
	session := ""
	if sessionCreated > 0 {
		session = fmt.Sprint(sessionCreated)
	}
	return fmt.Sprintf("session=%s\nnow=%d\ninitrun=%s\nready=%s\n", session, now, initRun, ready)
}

func TestParseReadinessProbe(t *testing.T) {
	const fallback = 8 * time.Second
	hookRun := strings.TrimSuffix(state.FormatInitRun("aa11", state.ReadyModeHook), "\n")
	settleRun := strings.TrimSuffix(state.FormatInitRun("bb22", state.ReadyModeSettle), "\n")
	marker := func(n string) string { return strings.TrimSuffix(state.FormatReadyMarker(n), "\n") }

	tests := []struct {
		name      string
		out       string
		wantReady bool
		wantMode  string
		wantWhy   string
	}{
		{"hook ready", probeOutput(1000, 1001, hookRun, marker("aa11")), true, state.ReadyModeHook, ""},
		{"hook marker from previous run after restart", probeOutput(1000, 2000, hookRun, marker("cc33")), false, state.ReadyModeHook, "ready marker from another run"},
		{"hook marker missing, no settle fallback", probeOutput(1000, 2000, hookRun, ""), false, state.ReadyModeHook, "ready marker missing"},
		{"hook no session", probeOutput(0, 2000, hookRun, marker("aa11")), false, state.ReadyModeHook, "tmux session not found"},
		{"settle young", probeOutput(1000, 1007, settleRun, ""), false, state.ReadyModeSettle, "session younger than 8s"},
		{"settle old enough", probeOutput(1000, 1008, settleRun, ""), true, state.ReadyModeSettle, ""},
		{"settle ignores marker", probeOutput(1000, 1001, settleRun, marker("bb22")), false, state.ReadyModeSettle, "session younger than 8s"},
		{"old image young", probeOutput(1000, 1003, "", ""), false, readyModeNoInitRun, "session younger than 8s"},
		{"old image old", probeOutput(1000, 1100, "", ""), true, readyModeNoInitRun, ""},
		{"garbled init-run treated as old image", probeOutput(1000, 1100, "nonce=zz mode=hook", ""), true, readyModeNoInitRun, ""},
		{"no session at all", probeOutput(0, 1100, "", ""), false, readyModeNoInitRun, "tmux session not found"},
		{"empty output", "", false, readyModeNoInitRun, "tmux session not found"},
		{"missing clock", "session=1000\ninitrun=\nready=\n", false, readyModeNoInitRun, "container time unavailable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseReadinessProbe(tt.out, fallback)
			if got.Ready != tt.wantReady || got.Mode != tt.wantMode || got.Reason != tt.wantWhy {
				t.Fatalf("got %+v, want ready=%v mode=%q reason=%q", got, tt.wantReady, tt.wantMode, tt.wantWhy)
			}
		})
	}
}

func TestReadinessProbeArgvIsOneExec(t *testing.T) {
	argv := readinessProbeArgv()
	if len(argv) != 3 || argv[0] != "sh" || argv[1] != "-c" {
		t.Fatalf("argv = %q", argv)
	}
	for _, want := range []string{"has-session -t scion", "#{session_created}", state.HarnessInitRunFile, state.HarnessReadyFile, "head -c 256"} {
		if !strings.Contains(argv[2], want) {
			t.Errorf("probe script missing %q", want)
		}
	}
}

// fakeReadyRuntime is a MockRuntime whose readiness probe output comes from
// probe and whose other execs are recorded in order.
type fakeReadyRuntime struct {
	*runtime.MockRuntime
	mu     sync.Mutex
	probes int
	cmds   []string // "probe" for a readiness probe, else the joined argv
}

func newFakeReadyRuntime(probe func(n int) (string, error)) *fakeReadyRuntime {
	f := &fakeReadyRuntime{}
	f.MockRuntime = &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{{
				ContainerID:     "agent-1",
				Name:            "test-agent",
				ContainerStatus: "Up 2 minutes",
				Labels:          map[string]string{"scion.name": "test-agent"},
			}}, nil
		},
		ExecFunc: func(ctx context.Context, id string, cmd []string) (string, error) {
			f.mu.Lock()
			if len(cmd) > 0 && cmd[0] == "sh" {
				f.probes++
				n := f.probes
				f.cmds = append(f.cmds, "probe")
				f.mu.Unlock()
				return probe(n)
			}
			f.cmds = append(f.cmds, strings.Join(cmd, " "))
			f.mu.Unlock()
			return "", nil
		},
		ExecWithStdinFunc: func(ctx context.Context, id string, cmd []string, stdin io.Reader) (string, error) {
			_, _ = io.ReadAll(stdin)
			f.mu.Lock()
			f.cmds = append(f.cmds, strings.Join(cmd, " "))
			f.mu.Unlock()
			return "", nil
		},
	}
	return f
}

func (f *fakeReadyRuntime) snapshot() (int, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.probes, append([]string(nil), f.cmds...)
}

var fastReadyTimings = harnessReadyTimings{
	PollInterval:   10 * time.Millisecond,
	Deadline:       2 * time.Second,
	Settle:         50 * time.Millisecond,
	SettleFallback: 8 * time.Second,
}

const testNonce = "aa11"

func hookProbe(ready bool) string {
	marker := ""
	if ready {
		marker = strings.TrimSuffix(state.FormatReadyMarker(testNonce), "\n")
	}
	return probeOutput(1000, 1001, strings.TrimSuffix(state.FormatInitRun(testNonce, state.ReadyModeHook), "\n"), marker)
}

func TestWaitHarnessReady_ReadyAtFirstProbeDoesNotWait(t *testing.T) {
	rt := newFakeReadyRuntime(func(int) (string, error) { return hookProbe(true), nil })
	mgr := &AgentManager{Runtime: rt, readyTimings: fastReadyTimings}

	start := time.Now()
	mgr.waitHarnessReady(context.Background(), "test-agent", "")
	if el := time.Since(start); el >= fastReadyTimings.Settle {
		t.Fatalf("ready-at-once wait took %v; no settle sleep expected", el)
	}
	if probes, _ := rt.snapshot(); probes != 1 {
		t.Fatalf("probes = %d, want 1", probes)
	}
}

func TestWaitHarnessReady_BecomesReadyThenSettles(t *testing.T) {
	rt := newFakeReadyRuntime(func(n int) (string, error) { return hookProbe(n >= 4), nil })
	mgr := &AgentManager{Runtime: rt, readyTimings: fastReadyTimings}

	start := time.Now()
	mgr.waitHarnessReady(context.Background(), "test-agent", "")
	el := time.Since(start)
	probes, cmds := rt.snapshot()
	if probes != 4 {
		t.Fatalf("probes = %d, want 4", probes)
	}
	if len(cmds) != probes {
		t.Fatalf("each probe must be exactly one exec; cmds = %q", cmds)
	}
	if minWait := 3*fastReadyTimings.PollInterval + fastReadyTimings.Settle; el < minWait {
		t.Fatalf("waited %v, want at least %v (polls + settle)", el, minWait)
	}
}

func TestWaitHarnessReady_DeadlineReturns(t *testing.T) {
	rt := newFakeReadyRuntime(func(int) (string, error) { return hookProbe(false), nil })
	timings := fastReadyTimings
	timings.Deadline = 200 * time.Millisecond
	mgr := &AgentManager{Runtime: rt, readyTimings: timings}

	start := time.Now()
	mgr.waitHarnessReady(context.Background(), "test-agent", "")
	el := time.Since(start)
	if el < timings.Deadline || el > timings.Deadline+time.Second {
		t.Fatalf("waited %v, want about %v", el, timings.Deadline)
	}
	if probes, _ := rt.snapshot(); probes < 5 {
		t.Fatalf("probes = %d, want polling until the deadline", probes)
	}
}

func TestWaitHarnessReady_ProbeErrorsKeepPolling(t *testing.T) {
	rt := newFakeReadyRuntime(func(n int) (string, error) {
		if n < 3 {
			return "", errors.New("exec failed")
		}
		return hookProbe(true), nil
	})
	mgr := &AgentManager{Runtime: rt, readyTimings: fastReadyTimings}
	mgr.waitHarnessReady(context.Background(), "test-agent", "")
	if probes, _ := rt.snapshot(); probes != 3 {
		t.Fatalf("probes = %d, want 3", probes)
	}
}

func TestWaitHarnessReady_CancelReturnsPromptly(t *testing.T) {
	rt := newFakeReadyRuntime(func(int) (string, error) { return hookProbe(false), nil })
	timings := fastReadyTimings
	timings.Deadline = time.Minute
	mgr := &AgentManager{Runtime: rt, readyTimings: timings}

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	mgr.waitHarnessReady(ctx, "test-agent", "")
	if el := time.Since(start); el > time.Second {
		t.Fatalf("cancelled wait took %v", el)
	}
}

func TestWaitHarnessReady_NoTargetReturnsAtOnce(t *testing.T) {
	rt := newFakeReadyRuntime(func(int) (string, error) { return hookProbe(false), nil })
	rt.ListFunc = func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) { return nil, nil }
	mgr := &AgentManager{Runtime: rt, readyTimings: fastReadyTimings}
	mgr.waitHarnessReady(context.Background(), "test-agent", "")
	if probes, _ := rt.snapshot(); probes != 0 {
		t.Fatalf("probes = %d, want 0", probes)
	}
}

// newReadyTestManager builds a manager wired like NewManager, with a short
// debounce and fast readiness timings.
func newReadyTestManager(rt runtime.Runtime) *AgentManager {
	mgr := &AgentManager{Runtime: rt, readyTimings: fastReadyTimings}
	mgr.msgBuffer = NewMessageBuffer(20*time.Millisecond, func(agentID, projectID, message string, interrupt bool) error {
		return mgr.deliverImmediate(context.Background(), agentID, projectID, message, interrupt)
	})
	mgr.msgBuffer.waitReady = mgr.waitHarnessReady
	return mgr
}

func TestNewManagerWiresReadinessWait(t *testing.T) {
	mgr := NewManager(&runtime.MockRuntime{}).(*AgentManager)
	defer mgr.Close()
	if mgr.msgBuffer.waitReady == nil {
		t.Fatal("NewManager must wire the readiness wait into the message buffer")
	}
}

// TestBufferedFlush_PastesOnlyAfterReady checks that a buffered message
// returns to the sender at once and is pasted only after the harness is
// ready, with Enter x3 unchanged.
func TestBufferedFlush_PastesOnlyAfterReady(t *testing.T) {
	var ready atomic.Bool
	rt := newFakeReadyRuntime(func(int) (string, error) { return hookProbe(ready.Load()), nil })
	mgr := newReadyTestManager(rt)
	defer mgr.Close()

	start := time.Now()
	if err := mgr.Message(context.Background(), "test-agent", "", "hello", false); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el > 500*time.Millisecond {
		t.Fatalf("Message blocked for %v", el)
	}

	time.Sleep(200 * time.Millisecond)
	if _, cmds := rt.snapshot(); containsPrefix(cmds, "tmux load-buffer") {
		t.Fatalf("pasted before ready: %q", cmds)
	}
	ready.Store(true)

	waitFor(t, 2*time.Second, func() bool {
		_, cmds := rt.snapshot()
		return countPrefix(cmds, "tmux send-keys -t scion:0 Enter") == 3
	})
	_, cmds := rt.snapshot()
	var tail []string
	for _, c := range cmds {
		if c != "probe" {
			tail = append(tail, c)
		}
	}
	if len(tail) != 5 || !strings.HasPrefix(tail[0], "tmux load-buffer") || !strings.HasPrefix(tail[1], "tmux paste-buffer") {
		t.Fatalf("unexpected delivery sequence %q", tail)
	}
	if cmds[len(cmds)-len(tail)-1] != "probe" {
		t.Fatalf("delivery must follow the readiness probe: %q", cmds)
	}
}

// TestBufferedFlush_WaitOncePerFlushNotPerRetry checks a failed delivery
// attempt is retried without probing again.
func TestBufferedFlush_WaitOncePerFlushNotPerRetry(t *testing.T) {
	rt := newFakeReadyRuntime(func(n int) (string, error) { return hookProbe(n >= 2), nil })
	var loads atomic.Int32
	rt.ExecWithStdinFunc = func(ctx context.Context, id string, cmd []string, stdin io.Reader) (string, error) {
		if loads.Add(1) == 1 {
			return "", errors.New("transient")
		}
		return "", nil
	}
	mgr := newReadyTestManager(rt)
	mgr.msgBuffer.flushRetryBackoff = 10 * time.Millisecond
	defer mgr.Close()

	if err := mgr.Message(context.Background(), "test-agent", "", "hello", false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, func() bool { return loads.Load() == 2 })
	waitFor(t, 2*time.Second, func() bool {
		_, cmds := rt.snapshot()
		return countPrefix(cmds, "tmux send-keys -t scion:0 Enter") == 3
	})
	if probes, _ := rt.snapshot(); probes != 2 {
		t.Fatalf("probes = %d, want 2 (one wait, not one per retry)", probes)
	}
}

// TestInterruptSkipsReadinessWait checks interrupts stay synchronous and
// never probe, even when the harness never becomes ready.
func TestInterruptSkipsReadinessWait(t *testing.T) {
	rt := newFakeReadyRuntime(func(int) (string, error) { return hookProbe(false), nil })
	mgr := newReadyTestManager(rt)
	defer mgr.Close()

	start := time.Now()
	if err := mgr.Message(context.Background(), "test-agent", "", "stop", true); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("interrupt took %v", el)
	}
	probes, cmds := rt.snapshot()
	if probes != 0 {
		t.Fatalf("interrupt probed readiness %d times", probes)
	}
	if !containsPrefix(cmds, "tmux paste-buffer") {
		t.Fatalf("interrupt not delivered: %q", cmds)
	}
}

// TestManagerClose_SkipsReadinessWait checks shutdown pastes pending
// messages without waiting for readiness.
func TestManagerClose_SkipsReadinessWait(t *testing.T) {
	rt := newFakeReadyRuntime(func(int) (string, error) { return hookProbe(false), nil })
	mgr := &AgentManager{Runtime: rt, readyTimings: harnessReadyTimings{Deadline: time.Minute}}
	mgr.msgBuffer = NewMessageBuffer(time.Hour, func(agentID, projectID, message string, interrupt bool) error {
		return mgr.deliverImmediate(context.Background(), agentID, projectID, message, interrupt)
	})
	mgr.msgBuffer.waitReady = mgr.waitHarnessReady

	if err := mgr.Message(context.Background(), "test-agent", "", "bye", false); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	mgr.Close()
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("Close took %v", el)
	}
	probes, cmds := rt.snapshot()
	if probes != 0 {
		t.Fatalf("Close probed readiness %d times", probes)
	}
	if !containsPrefix(cmds, "tmux paste-buffer") {
		t.Fatalf("pending message not pasted on Close: %q", cmds)
	}
}

func containsPrefix(cmds []string, prefix string) bool { return countPrefix(cmds, prefix) > 0 }

func countPrefix(cmds []string, prefix string) int {
	n := 0
	for _, c := range cmds {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v", timeout)
}
