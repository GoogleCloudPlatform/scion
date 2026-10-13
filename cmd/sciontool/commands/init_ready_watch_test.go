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
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/handlers"
)

// readyWatchFixture is a temp agent-info.json plus a fake hub that records
// the reported activities, wired the way RunInit wires the watcher.
type readyWatchFixture struct {
	status *handlers.StatusHandler
	mu     sync.Mutex
	hub    []state.Activity
}

func newReadyWatchFixture(t *testing.T) *readyWatchFixture {
	t.Helper()
	dir := t.TempDir()
	initRunFilePath = filepath.Join(dir, "init-run")
	harnessReadyFilePath = filepath.Join(dir, "ready")
	t.Cleanup(func() {
		initRunFilePath = state.HarnessInitRunFile
		harnessReadyFilePath = state.HarnessReadyFile
	})
	f := &readyWatchFixture{status: &handlers.StatusHandler{StatusPath: filepath.Join(dir, "agent-info.json")}}
	// What init's post-start does locally.
	if err := f.status.Handle(&hooks.Event{Name: hooks.EventPostStart}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *readyWatchFixture) cfg(nonce, mode string, start time.Time) readyWatchConfig {
	return readyWatchConfig{
		Nonce:          nonce,
		Mode:           mode,
		Start:          start,
		PollInterval:   5 * time.Millisecond,
		Settle:         60 * time.Millisecond,
		Deadline:       150 * time.Millisecond,
		FallbackSettle: 60 * time.Millisecond,
		MarkerTime:     harnessReadyMarkerTime,
		LocalActivity:  f.status.CurrentActivity,
		ReplaceLocal:   f.status.ReplaceActivityIf,
		Report: func(_ context.Context, a state.Activity, _ string) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.hub = append(f.hub, a)
			return nil
		},
	}
}

func (f *readyWatchFixture) reports() []state.Activity {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]state.Activity(nil), f.hub...)
}

func writeMarker(t *testing.T, nonce string) {
	t.Helper()
	if err := os.WriteFile(harnessReadyFilePath, []byte(state.FormatReadyMarker(nonce)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func equalActivities(a, b []state.Activity) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// afterFirstPoll wraps cfg.MarkerTime so that write runs once, right after
// the watcher's first poll has found no marker. This places the hook event
// after the watch began without a sleep: on every poll the watcher checks
// the marker before the deadline, so any poll after the first sees it, even
// if it runs after the deadline.
func afterFirstPoll(t *testing.T, cfg *readyWatchConfig, write func()) {
	t.Helper()
	inner := cfg.MarkerTime
	polled := false
	cfg.MarkerTime = func(nonce string) (time.Time, bool) {
		at, ready := inner(nonce)
		if !polled {
			polled = true
			if ready {
				t.Errorf("marker present at the first poll; want it written after the watch started")
			}
			write()
		}
		return at, ready
	}
}

// Hook mode: the hook fires after init's report. The hook's own handlers move
// the activity off initializing, so the watcher reports nothing.
func TestWatchHarnessReady_HookMarkerAfterStart(t *testing.T) {
	f := newReadyWatchFixture(t)
	nonce := "abc123"
	cfg := f.cfg(nonce, state.ReadyModeHook, time.Now())
	afterFirstPoll(t, &cfg, func() {
		if err := f.status.Handle(&hooks.Event{Name: hooks.EventSessionStart, Data: hooks.EventData{Source: "startup"}}); err != nil {
			t.Errorf("session-start hook: %v", err)
		}
		writeMarker(t, nonce)
	})
	got := watchHarnessReady(context.Background(), cfg)
	if got != readyOutcomeHookMarker {
		t.Fatalf("outcome = %s, want %s", got, readyOutcomeHookMarker)
	}
	if r := f.reports(); len(r) != 0 {
		t.Errorf("hub reports = %v, want none (the hook reported)", r)
	}
	if a := f.status.CurrentActivity(); a != state.ActivityWorking {
		t.Errorf("local activity = %s, want working", a)
	}
}

// Hook mode: the marker appears but the local write did not happen; the
// watcher ends initializing itself.
func TestWatchHarnessReady_HookMarkerLocalStillInitializing(t *testing.T) {
	f := newReadyWatchFixture(t)
	nonce := "abc123"
	cfg := f.cfg(nonce, state.ReadyModeHook, time.Now())
	afterFirstPoll(t, &cfg, func() { writeMarker(t, nonce) })
	got := watchHarnessReady(context.Background(), cfg)
	if got != readyOutcomeHookMarker {
		t.Fatalf("outcome = %s", got)
	}
	if r := f.reports(); !equalActivities(r, []state.Activity{state.ActivityWorking}) {
		t.Errorf("hub reports = %v, want [working]", r)
	}
}

// Hook mode: the hook fired before init's initializing report returned. The
// watcher re-sends the hook's activity so init's report does not hide it.
func TestWatchHarnessReady_HookMarkerBeforeStartSyncs(t *testing.T) {
	f := newReadyWatchFixture(t)
	nonce := "abc123"
	if err := f.status.Handle(&hooks.Event{Name: hooks.EventNotification}); err != nil {
		t.Fatal(err)
	}
	writeMarker(t, nonce)
	got := watchHarnessReady(context.Background(), f.cfg(nonce, state.ReadyModeHook, time.Now()))
	if got != readyOutcomeHookMarker {
		t.Fatalf("outcome = %s", got)
	}
	if r := f.reports(); !equalActivities(r, []state.Activity{state.ActivityWaitingForInput}) {
		t.Errorf("hub reports = %v, want [waiting_for_input]", r)
	}
	if a := f.status.CurrentActivity(); a != state.ActivityWaitingForInput {
		t.Errorf("local activity = %s, want waiting_for_input (not overwritten)", a)
	}
}

// Hook mode: a stale marker (previous run's nonce) never counts; the deadline
// reports working.
func TestWatchHarnessReady_HookDeadline(t *testing.T) {
	f := newReadyWatchFixture(t)
	writeMarker(t, "0ff")
	start := time.Now()
	got := watchHarnessReady(context.Background(), f.cfg("abc123", state.ReadyModeHook, start))
	if got != readyOutcomeDeadline {
		t.Fatalf("outcome = %s, want deadline", got)
	}
	if time.Since(start) < 150*time.Millisecond {
		t.Errorf("returned before the deadline")
	}
	if r := f.reports(); !equalActivities(r, []state.Activity{state.ActivityWorking}) {
		t.Errorf("hub reports = %v, want [working]", r)
	}
	if a := f.status.CurrentActivity(); a != state.ActivityWorking {
		t.Errorf("local activity = %s, want working", a)
	}
}

// Settle mode: working at Start+Settle, not before.
func TestWatchHarnessReady_Settle(t *testing.T) {
	f := newReadyWatchFixture(t)
	start := time.Now()
	got := watchHarnessReady(context.Background(), f.cfg("abc123", state.ReadyModeSettle, start))
	if got != readyOutcomeSettled {
		t.Fatalf("outcome = %s, want settled", got)
	}
	if time.Since(start) < 60*time.Millisecond {
		t.Errorf("returned before the settle period")
	}
	if r := f.reports(); !equalActivities(r, []state.Activity{state.ActivityWorking}) {
		t.Errorf("hub reports = %v, want [working]", r)
	}
}

// No nonce (init-run file write failed) in hook mode uses the session-age
// rule with FallbackSettle, like the runtime's no-init-run fallback, not the
// hook spec's post-marker settle.
func TestWatchHarnessReady_NoNonceSettles(t *testing.T) {
	f := newReadyWatchFixture(t)
	cfg := f.cfg("", state.ReadyModeHook, time.Now())
	cfg.Settle = 0
	cfg.FallbackSettle = 120 * time.Millisecond
	start := time.Now()
	if got := watchHarnessReady(context.Background(), cfg); got != readyOutcomeSettled {
		t.Fatalf("outcome = %s, want settled", got)
	}
	if waited := time.Since(start); waited < 120*time.Millisecond {
		t.Errorf("returned after %s, before FallbackSettle", waited)
	}
}

// Settle mode: a hook already moved the activity on; nothing is reported.
func TestWatchHarnessReady_SettleDoesNotOverwriteHookActivity(t *testing.T) {
	f := newReadyWatchFixture(t)
	if err := f.status.Handle(&hooks.Event{Name: hooks.EventPromptSubmit}); err != nil {
		t.Fatal(err)
	}
	watchHarnessReady(context.Background(), f.cfg("abc123", state.ReadyModeSettle, time.Now()))
	if r := f.reports(); len(r) != 0 {
		t.Errorf("hub reports = %v, want none", r)
	}
	if a := f.status.CurrentActivity(); a != state.ActivityThinking {
		t.Errorf("local activity = %s, want thinking", a)
	}
}

// Cancellation (init shutting down) stops the watcher without reporting.
func TestWatchHarnessReady_Cancelled(t *testing.T) {
	f := newReadyWatchFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	var done atomic.Value
	go func() {
		done.Store(watchHarnessReady(ctx, f.cfg("abc123", state.ReadyModeHook, time.Now())))
	}()
	time.Sleep(15 * time.Millisecond)
	cancel()
	deadline := time.Now().Add(time.Second)
	for done.Load() == nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got, _ := done.Load().(readyWatchOutcome); got != readyOutcomeCancelled {
		t.Fatalf("outcome = %v, want cancelled", done.Load())
	}
	if r := f.reports(); len(r) != 0 {
		t.Errorf("hub reports = %v, want none", r)
	}
}

func TestHarnessReadyMarkerTime(t *testing.T) {
	newReadyWatchFixture(t)
	if _, ok := harnessReadyMarkerTime("abc"); ok {
		t.Error("missing marker matched")
	}
	writeMarker(t, "abc")
	at, ok := harnessReadyMarkerTime("abc")
	if !ok {
		t.Fatal("current marker not matched")
	}
	fi, err := os.Stat(harnessReadyFilePath)
	if err != nil {
		t.Fatal(err)
	}
	if !at.Equal(fi.ModTime()) {
		t.Errorf("marker time = %v, want mtime %v", at, fi.ModTime())
	}
	if _, ok := harnessReadyMarkerTime("def"); ok {
		t.Error("marker from another run matched")
	}
	// A symlinked marker never counts.
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(target, []byte(state.FormatReadyMarker("abc")), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(harnessReadyFilePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, harnessReadyFilePath); err != nil {
		t.Fatal(err)
	}
	if _, ok := harnessReadyMarkerTime("abc"); ok {
		t.Error("symlinked marker matched")
	}
}

// Hook mode: readiness is the marker's mtime plus Settle, not the moment the
// marker is first seen. The watcher must not report working before then.
func TestWatchHarnessReady_HookAppliesSettleAfterMarker(t *testing.T) {
	f := newReadyWatchFixture(t)
	nonce := "abc123"
	cfg := f.cfg(nonce, state.ReadyModeHook, time.Now())
	cfg.Settle = 300 * time.Millisecond
	cfg.Deadline = 10 * time.Second
	var reportedAt time.Time
	inner := cfg.Report
	cfg.Report = func(ctx context.Context, a state.Activity, m string) error {
		reportedAt = time.Now()
		return inner(ctx, a, m)
	}
	// The marker is written but the hook's local write did not happen, so
	// the watcher itself reports working.
	afterFirstPoll(t, &cfg, func() { writeMarker(t, nonce) })
	got := watchHarnessReady(context.Background(), cfg)
	if got != readyOutcomeHookMarker {
		t.Fatalf("outcome = %s, want %s", got, readyOutcomeHookMarker)
	}
	fi, err := os.Stat(harnessReadyFilePath)
	if err != nil {
		t.Fatal(err)
	}
	if want := fi.ModTime().Add(cfg.Settle); reportedAt.Before(want) {
		t.Errorf("reported working at %v, before marker mtime + settle %v", reportedAt, want)
	}
	if r := f.reports(); !equalActivities(r, []state.Activity{state.ActivityWorking}) {
		t.Errorf("hub reports = %v, want [working]", r)
	}
}

// Hook mode: a marker written longer than Settle ago is ready at once.
func TestWatchHarnessReady_HookOldMarkerReadyAtOnce(t *testing.T) {
	f := newReadyWatchFixture(t)
	nonce := "abc123"
	writeMarker(t, nonce)
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(harnessReadyFilePath, old, old); err != nil {
		t.Fatal(err)
	}
	cfg := f.cfg(nonce, state.ReadyModeHook, time.Now())
	cfg.Settle = 30 * time.Second
	cfg.Deadline = 60 * time.Second
	start := time.Now()
	if got := watchHarnessReady(context.Background(), cfg); got != readyOutcomeHookMarker {
		t.Fatalf("outcome = %s, want %s", got, readyOutcomeHookMarker)
	}
	if waited := time.Since(start); waited > 5*time.Second {
		t.Errorf("waited %s for a marker written a minute ago", waited)
	}
}

// Hook mode: the deadline comes from the spec, not state.ReadyDeadline.
func TestWatchHarnessReady_SpecDeadline(t *testing.T) {
	f := newReadyWatchFixture(t)
	cfg := f.cfg("abc123", state.ReadyModeHook, time.Now())
	cfg.Settle = 0
	cfg.Deadline = 200 * time.Millisecond
	start := time.Now()
	if got := watchHarnessReady(context.Background(), cfg); got != readyOutcomeDeadline {
		t.Fatalf("outcome = %s, want deadline", got)
	}
	waited := time.Since(start)
	if waited < 200*time.Millisecond {
		t.Errorf("returned after %s, before the spec deadline", waited)
	}
	if waited > 10*time.Second {
		t.Errorf("returned after %s; the spec deadline is 200ms", waited)
	}
	if r := f.reports(); !equalActivities(r, []state.Activity{state.ActivityWorking}) {
		t.Errorf("hub reports = %v, want [working]", r)
	}
}

// Hook mode: once the marker is seen, its settle is waited out even when
// that runs past the deadline, as the runtime does.
func TestWatchHarnessReady_MarkerSettleOutlastsDeadline(t *testing.T) {
	f := newReadyWatchFixture(t)
	nonce := "abc123"
	cfg := f.cfg(nonce, state.ReadyModeHook, time.Now())
	cfg.Settle = 400 * time.Millisecond
	cfg.Deadline = 100 * time.Millisecond
	afterFirstPoll(t, &cfg, func() { writeMarker(t, nonce) })
	if got := watchHarnessReady(context.Background(), cfg); got != readyOutcomeHookMarker {
		t.Fatalf("outcome = %s, want %s", got, readyOutcomeHookMarker)
	}
}

// Settle mode: a zero settle is used as given (ready at Start).
func TestWatchHarnessReady_ZeroSettle(t *testing.T) {
	f := newReadyWatchFixture(t)
	cfg := f.cfg("abc123", state.ReadyModeSettle, time.Now())
	cfg.Settle = 0
	cfg.PollInterval = time.Hour
	if got := watchHarnessReady(context.Background(), cfg); got != readyOutcomeSettled {
		t.Fatalf("outcome = %s, want settled", got)
	}
}
