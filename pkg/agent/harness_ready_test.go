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
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
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
	return probeOutputMtime(sessionCreated, now, initRun, ready, 0)
}

// probeOutputMtime is probeOutput with the ready marker's mtime (0 = none).
func probeOutputMtime(sessionCreated, now int64, initRun, ready string, readyMtime int64) string {
	session := ""
	if sessionCreated > 0 {
		session = fmt.Sprint(sessionCreated)
	}
	mtime := ""
	if readyMtime > 0 {
		mtime = fmt.Sprint(readyMtime)
	}
	return fmt.Sprintf("session=%s\nnow=%d\ninitrun=%s\nready=%s\nreadymtime=%s\n", session, now, initRun, ready, mtime)
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
			got := parseReadinessProbe(tt.out, fallback, 1500*time.Millisecond)
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
	for _, want := range []string{"has-session -t scion", "#{session_created}", state.HarnessInitRunFile, state.HarnessReadyFile, "head -c 256", "stat -c %Y " + state.HarnessReadyFile} {
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

// hookProbe is a hook-mode probe output; a ready marker, when present, was
// written long before the probe (not fresh).
func hookProbe(ready bool) string {
	return hookProbeMtime(ready, 900)
}

// hookProbeMtime is hookProbe with the marker's mtime; the probe's clock is
// 1001.
func hookProbeMtime(ready bool, mtime int64) string {
	marker := ""
	if ready {
		marker = strings.TrimSuffix(state.FormatReadyMarker(testNonce), "\n")
	}
	return probeOutputMtime(1000, 1001, strings.TrimSuffix(state.FormatInitRun(testNonce, state.ReadyModeHook), "\n"), marker, mtime)
}

func TestParseReadinessProbe_MarkerFreshness(t *testing.T) {
	const settle = 1500 * time.Millisecond
	for _, tt := range []struct {
		mtime int64
		fresh bool
	}{
		{1001, true}, // written this second
		{1000, true}, // 1s ago, under 1.5s
		{999, false}, // 2s ago
		{0, false},   // no mtime reported (no stat): not fresh
	} {
		got := parseReadinessProbe(hookProbeMtime(true, tt.mtime), 8*time.Second, settle)
		if !got.Ready || got.MarkerFresh != tt.fresh {
			t.Errorf("mtime %d: got %+v, want ready and fresh=%v", tt.mtime, got, tt.fresh)
		}
	}
	// Settle mode never reports a fresh marker.
	settleOut := probeOutputMtime(1000, 1100, strings.TrimSuffix(state.FormatInitRun(testNonce, state.ReadyModeSettle), "\n"), "", 1100)
	if got := parseReadinessProbe(settleOut, 8*time.Second, settle); !got.Ready || got.MarkerFresh {
		t.Errorf("settle mode: got %+v", got)
	}
}

func TestWaitHarnessReady_FreshMarkerSettlesOnFirstProbe(t *testing.T) {
	rt := newFakeReadyRuntime(func(int) (string, error) { return hookProbeMtime(true, 1001), nil })
	mgr := &AgentManager{Runtime: rt, readyTimings: fastReadyTimings}

	start := time.Now()
	mgr.waitHarnessReady(context.Background(), "test-agent", "")
	if el := time.Since(start); el < fastReadyTimings.Settle {
		t.Fatalf("fresh marker: waited %v, want at least the settle time %v", el, fastReadyTimings.Settle)
	}
	if probes, _ := rt.snapshot(); probes != 1 {
		t.Fatalf("probes = %d, want 1", probes)
	}
}

func TestWaitHarnessReady_ContainerNotFoundStopsAtOnce(t *testing.T) {
	rt := newFakeReadyRuntime(func(int) (string, error) {
		return "", fmt.Errorf("agent 'x' %w", runtime.ErrContainerNotFound)
	})
	timings := fastReadyTimings
	timings.Deadline = time.Minute
	mgr := &AgentManager{Runtime: rt, readyTimings: timings}

	start := time.Now()
	mgr.waitHarnessReady(context.Background(), "test-agent", "")
	if el := time.Since(start); el > time.Second {
		t.Fatalf("wait took %v after the container was reported missing", el)
	}
	if probes, _ := rt.snapshot(); probes != 1 {
		t.Fatalf("probes = %d, want 1", probes)
	}
}

func TestWaitHarnessReady_TargetGoneFromLookupStopsAtOnce(t *testing.T) {
	var lists atomic.Int32
	rt := newFakeReadyRuntime(func(int) (string, error) { return "", errors.New("exec: pod unreachable") })
	base := rt.ListFunc
	rt.ListFunc = func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
		if lists.Add(1) == 1 {
			return base(ctx, filter) // initial target resolution
		}
		return nil, nil // the pod is gone
	}
	timings := fastReadyTimings
	timings.Deadline = time.Minute
	mgr := &AgentManager{Runtime: rt, readyTimings: timings}

	start := time.Now()
	mgr.waitHarnessReady(context.Background(), "test-agent", "")
	if el := time.Since(start); el > time.Second {
		t.Fatalf("wait took %v after the lookup showed no container", el)
	}
	if probes, _ := rt.snapshot(); probes != 1 {
		t.Fatalf("probes = %d, want 1", probes)
	}
}

func TestWaitHarnessReady_StoppedContainerStopsAtOnce(t *testing.T) {
	var lists atomic.Int32
	rt := newFakeReadyRuntime(func(int) (string, error) { return "", errors.New("exec failed") })
	base := rt.ListFunc
	rt.ListFunc = func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
		agents, err := base(ctx, filter)
		if lists.Add(1) > 1 {
			agents[0].Phase = string(state.PhaseStopped)
		}
		return agents, err
	}
	timings := fastReadyTimings
	timings.Deadline = time.Minute
	mgr := &AgentManager{Runtime: rt, readyTimings: timings}

	start := time.Now()
	mgr.waitHarnessReady(context.Background(), "test-agent", "")
	if el := time.Since(start); el > time.Second {
		t.Fatalf("wait took %v for a stopped container", el)
	}
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

// warnCounter is a slog handler counting records at Warn and above.
type warnCounter struct{ n atomic.Int32 }

func (w *warnCounter) Enabled(context.Context, slog.Level) bool { return true }
func (w *warnCounter) Handle(_ context.Context, r slog.Record) error {
	if r.Level >= slog.LevelWarn {
		w.n.Add(1)
	}
	return nil
}
func (w *warnCounter) WithAttrs([]slog.Attr) slog.Handler { return w }
func (w *warnCounter) WithGroup(string) slog.Handler      { return w }

func countWarns(t *testing.T) *warnCounter {
	t.Helper()
	w := &warnCounter{}
	old := slog.Default()
	slog.SetDefault(slog.New(w))
	t.Cleanup(func() { slog.SetDefault(old) })
	return w
}

func hookProbeNonce(nonce string, ready bool) string {
	marker := ""
	if ready {
		marker = strings.TrimSuffix(state.FormatReadyMarker(nonce), "\n")
	}
	return probeOutputMtime(1000, 1001, strings.TrimSuffix(state.FormatInitRun(nonce, state.ReadyModeHook), "\n"), marker, 900)
}

// TestWaitHarnessReady_DeadlineRememberedPerNonce checks that once a wait
// for an init run reaches the deadline, a later flush for the same run
// probes once and does not wait, with one Warn per run; a new run (new
// nonce) waits again.
func TestWaitHarnessReady_DeadlineRememberedPerNonce(t *testing.T) {
	warns := countWarns(t)
	var nonce atomic.Value
	nonce.Store("aa11")
	rt := newFakeReadyRuntime(func(int) (string, error) { return hookProbeNonce(nonce.Load().(string), false), nil })
	timings := fastReadyTimings
	timings.Deadline = 200 * time.Millisecond
	mgr := newReadyTestManager(rt)
	mgr.readyTimings = timings

	start := time.Now()
	mgr.waitHarnessReady(context.Background(), "test-agent", "")
	if el := time.Since(start); el < timings.Deadline {
		t.Fatalf("first wait %v, want the full deadline", el)
	}
	if got := warns.n.Load(); got != 1 {
		t.Fatalf("warns after first wait = %d, want 1", got)
	}

	before, _ := rt.snapshot()
	start = time.Now()
	mgr.waitHarnessReady(context.Background(), "test-agent", "")
	if el := time.Since(start); el > 100*time.Millisecond {
		t.Fatalf("second wait for the same run took %v; want no wait", el)
	}
	if after, _ := rt.snapshot(); after-before != 1 {
		t.Fatalf("second wait probed %d times, want 1", after-before)
	}
	if got := warns.n.Load(); got != 1 {
		t.Fatalf("warns after second wait = %d, want still 1", got)
	}

	nonce.Store("bb22") // the harness restarted: a new init run
	start = time.Now()
	mgr.waitHarnessReady(context.Background(), "test-agent", "")
	if el := time.Since(start); el < timings.Deadline {
		t.Fatalf("wait for a new run took %v, want the full deadline", el)
	}
	if got := warns.n.Load(); got != 2 {
		t.Fatalf("warns after new run = %d, want 2", got)
	}
	if n := mgr.readyMemos.size(); n != 1 {
		t.Fatalf("memo entries = %d, want 1 (old nonce evicted)", n)
	}
	mgr.Close()
	if n := mgr.readyMemos.size(); n != 0 {
		t.Fatalf("memo entries after Close = %d, want 0", n)
	}
}

// TestWaitHarnessReady_SeenReadyRunSkipsSettle checks a run already seen
// ready does not settle again when readiness reappears after a failed probe.
func TestWaitHarnessReady_SeenReadyRunSkipsSettle(t *testing.T) {
	var calls atomic.Int32
	rt := newFakeReadyRuntime(func(int) (string, error) {
		if calls.Add(1) == 2 {
			return "", errors.New("transient exec failure")
		}
		return hookProbeNonce("aa11", true), nil
	})
	timings := fastReadyTimings
	timings.Settle = 300 * time.Millisecond
	mgr := &AgentManager{Runtime: rt, readyTimings: timings}

	mgr.waitHarnessReady(context.Background(), "test-agent", "") // seen ready
	start := time.Now()
	mgr.waitHarnessReady(context.Background(), "test-agent", "") // error, then ready
	if el := time.Since(start); el >= timings.Settle {
		t.Fatalf("wait took %v; a run already seen ready must not settle again", el)
	}
	if probes, _ := rt.snapshot(); probes != 3 {
		t.Fatalf("probes = %d, want 3", probes)
	}
}

func TestReadyMemoStore_EvictionAndBound(t *testing.T) {
	var s readyMemoStore
	s.markReady("c1", "n1")
	if !s.get("c1", "n1").ready {
		t.Fatal("expected ready memo")
	}
	if s.get("c2", "n1").ready {
		t.Fatal("a nonce recorded for another container must not match")
	}
	if !s.markTimedOut("c1", "n2") {
		t.Fatal("first timeout for n2 should report first")
	}
	if s.markTimedOut("c1", "n2") {
		t.Fatal("second timeout for n2 should not report first")
	}
	if s.get("c1", "n1").ready || s.size() != 1 {
		t.Fatalf("a new nonce for c1 must evict n1; size=%d", s.size())
	}
	for i := 0; i < maxReadyMemos+10; i++ {
		s.markReady(fmt.Sprintf("c-%d", i), fmt.Sprintf("n-%d", i))
	}
	if n := s.size(); n > maxReadyMemos {
		t.Fatalf("size = %d, want at most %d", n, maxReadyMemos)
	}
	s.clear()
	if s.size() != 0 {
		t.Fatal("clear left entries")
	}
}

// TestReadinessProbeScript_RefusesNonRegularFiles runs the production probe
// script (tmux replaced by false, paths pointed at a temp dir) and checks a
// symlinked or non-regular readiness file is reported as missing.
func TestReadinessProbeScript_RefusesNonRegularFiles(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	dir := t.TempDir()
	initRun := filepath.Join(dir, "init-run")
	ready := filepath.Join(dir, "ready")
	run := func() string {
		script := readinessProbeArgv()[2]
		script = strings.ReplaceAll(script, "tmux ", "false ")
		script = strings.ReplaceAll(script, state.HarnessInitRunFile, initRun)
		script = strings.ReplaceAll(script, state.HarnessReadyFile, ready)
		out, err := exec.Command("sh", "-c", script).CombinedOutput()
		if err != nil {
			t.Fatalf("probe script failed: %v (%s)", err, out)
		}
		return string(out)
	}
	fieldsOf := func(out string) map[string]string {
		f := map[string]string{}
		for _, line := range strings.Split(out, "\n") {
			if k, v, ok := strings.Cut(line, "="); ok {
				f[k] = v
			}
		}
		return f
	}

	// Regular files are read.
	initContent := strings.TrimSuffix(state.FormatInitRun("aa11", state.ReadyModeHook), "\n")
	markerContent := strings.TrimSuffix(state.FormatReadyMarker("aa11"), "\n")
	if err := os.WriteFile(initRun, []byte(initContent+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ready, []byte(markerContent+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := fieldsOf(run())
	if f["initrun"] != initContent || f["ready"] != markerContent || f["session"] != "" {
		t.Fatalf("regular files: %q", f)
	}
	// BSD stat (darwin) has no -c, so the mtime is only checked elsewhere.
	if goruntime.GOOS != "darwin" && f["readymtime"] == "" {
		t.Fatalf("regular marker: no mtime: %q", f)
	}

	// Symlinks to valid files are refused.
	elsewhere := t.TempDir()
	for _, p := range []string{initRun, ready} {
		target := filepath.Join(elsewhere, filepath.Base(p))
		data, _ := os.ReadFile(p)
		if err := os.WriteFile(target, data, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
	}
	out := run()
	f = fieldsOf(out)
	if f["initrun"] != "" || f["ready"] != "" || f["readymtime"] != "" {
		t.Fatalf("symlinks must read as missing: %q", f)
	}
	if got := parseReadinessProbe(out, 8*time.Second, 1500*time.Millisecond); got.Ready || got.Mode != readyModeNoInitRun {
		t.Fatalf("parsed symlinked probe = %+v", got)
	}

	// A directory in place of the marker is refused too.
	_ = os.Remove(ready)
	if err := os.Mkdir(ready, 0o755); err != nil {
		t.Fatal(err)
	}
	if f = fieldsOf(run()); f["ready"] != "" || f["readymtime"] != "" {
		t.Fatalf("directory marker must read as missing: %q", f)
	}
}

// TestWaitHarnessReady_TimedOutRunSkipsWaitAfterFailedFirstProbe checks a
// run that already reached the deadline is not waited for again when this
// wait's first probe fails transiently and a later probe shows the same
// nonce, still not ready.
func TestWaitHarnessReady_TimedOutRunSkipsWaitAfterFailedFirstProbe(t *testing.T) {
	var failNext atomic.Bool
	rt := newFakeReadyRuntime(func(int) (string, error) {
		if failNext.CompareAndSwap(true, false) {
			return "", errors.New("transient exec failure")
		}
		return hookProbeNonce("aa11", false), nil
	})
	timings := fastReadyTimings
	timings.Deadline = 200 * time.Millisecond
	mgr := &AgentManager{Runtime: rt, readyTimings: timings}

	mgr.waitHarnessReady(context.Background(), "test-agent", "") // reaches the deadline

	failNext.Store(true)
	before, _ := rt.snapshot()
	start := time.Now()
	mgr.waitHarnessReady(context.Background(), "test-agent", "")
	if el := time.Since(start); el > 100*time.Millisecond {
		t.Fatalf("wait took %v; a run known to have timed out must not wait again", el)
	}
	if after, _ := rt.snapshot(); after-before != 2 {
		t.Fatalf("probes = %d, want 2 (failed, then same nonce)", after-before)
	}
}
