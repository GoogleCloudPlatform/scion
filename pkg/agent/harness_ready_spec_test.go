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
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
)

// probeOutputSpec is probeOutputMtime with the init-run file's line 2.
func probeOutputSpec(sessionCreated, now int64, initRun, initSpec, ready string, readyMtime int64) string {
	return probeOutputMtime(sessionCreated, now, initRun, ready, readyMtime) + "initspec=" + initSpec + "\n"
}

// initRunLines returns the two lines of the init-run file for nonce and
// spec, without newlines.
func initRunLines(nonce string, spec api.ReadySpec) (line1, line2 string) {
	line1, line2, _ = strings.Cut(strings.TrimSuffix(state.FormatInitRunSpec(nonce, spec), "\n"), "\n")
	return line1, line2
}

func TestParseReadinessProbe_UsesLine2Timings(t *testing.T) {
	marker := strings.TrimSuffix(state.FormatReadyMarker(testNonce), "\n")
	hook1, hook2 := initRunLines(testNonce, api.ReadySpec{Strategy: state.ReadyStrategyHook, Settle: 3 * time.Second, Timeout: 30 * time.Second})
	zero1, zero2 := initRunLines(testNonce, api.ReadySpec{Strategy: state.ReadyStrategyHook, Settle: 0, Timeout: 30 * time.Second})
	delay1, delay2 := initRunLines(testNonce, api.ReadySpec{Strategy: state.ReadyStrategyDelay, Settle: 2 * time.Second})

	tests := []struct {
		name         string
		out          string
		wantReady    bool
		wantFresh    bool
		wantReason   string
		wantSettle   time.Duration
		wantDeadline time.Duration
	}{
		// The caller's settle is 1.5s, so a marker 2s old would not be
		// fresh; line 2's 3s makes it fresh.
		{"hook settle from line 2", probeOutputSpec(1000, 1001, hook1, hook2, marker, 999), true, true, "", 3 * time.Second, 30 * time.Second},
		{"hook marker older than line 2 settle", probeOutputSpec(1000, 1001, hook1, hook2, marker, 997), true, false, "", 3 * time.Second, 30 * time.Second},
		{"hook marker missing", probeOutputSpec(1000, 1001, hook1, hook2, "", 0), false, false, "ready marker missing", 3 * time.Second, 30 * time.Second},
		{"hook zero settle never fresh", probeOutputSpec(1000, 1001, zero1, zero2, marker, 1001), true, false, "", 0, 30 * time.Second},
		// The caller's session age is 8s; line 2's 2s applies.
		{"delay settle from line 2, old enough", probeOutputSpec(1000, 1002, delay1, delay2, "", 0), true, false, "", defaultReadySettle, 0},
		{"delay settle from line 2, young", probeOutputSpec(1000, 1001, delay1, delay2, "", 0), false, false, "session younger than 2s", defaultReadySettle, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseReadinessProbe(tt.out, defaultReadySettleSession, defaultReadySettle)
			if got.Ready != tt.wantReady || got.MarkerFresh != tt.wantFresh || got.Reason != tt.wantReason ||
				got.Settle != tt.wantSettle || got.Deadline != tt.wantDeadline || got.Nonce != testNonce {
				t.Fatalf("got %+v, want ready=%v fresh=%v reason=%q settle=%s deadline=%s nonce=%s",
					got, tt.wantReady, tt.wantFresh, tt.wantReason, tt.wantSettle, tt.wantDeadline, testNonce)
			}
		})
	}
}

// TestParseReadinessProbe_NoLine2UsesConstants covers an image older than the
// ready spec: its init-run file has one line, so the probe's initspec is
// empty (a new host) or absent (the field is not printed), and the caller's
// timings, the readiness constants, apply.
func TestParseReadinessProbe_NoLine2UsesConstants(t *testing.T) {
	marker := strings.TrimSuffix(state.FormatReadyMarker(testNonce), "\n")
	hookRun := strings.TrimSuffix(state.FormatInitRun(testNonce, state.ReadyModeHook), "\n")
	settleRun := strings.TrimSuffix(state.FormatInitRun(testNonce, state.ReadyModeSettle), "\n")
	for name, withField := range map[string]bool{"empty initspec": true, "no initspec field": false} {
		out := func(created, now int64, run, ready string, mtime int64) string {
			if withField {
				return probeOutputSpec(created, now, run, "", ready, mtime)
			}
			return probeOutputMtime(created, now, run, ready, mtime)
		}
		// Hook: a marker 1s old is fresh under the 1.5s constant.
		got := parseReadinessProbe(out(1000, 1001, hookRun, marker, 1000), defaultReadySettleSession, defaultReadySettle)
		if !got.Ready || !got.MarkerFresh || got.Settle != defaultReadySettle || got.Deadline != 0 || got.Mode != state.ReadyModeHook {
			t.Errorf("%s: hook: got %+v, want ready, fresh, settle %s, no deadline", name, got, defaultReadySettle)
		}
		// Settle: 7s of session age is under the 8s constant.
		got = parseReadinessProbe(out(1000, 1007, settleRun, "", 0), defaultReadySettleSession, defaultReadySettle)
		if got.Ready || got.Reason != "session younger than 8s" || got.Deadline != 0 {
			t.Errorf("%s: settle young: got %+v", name, got)
		}
		got = parseReadinessProbe(out(1000, 1008, settleRun, "", 0), defaultReadySettleSession, defaultReadySettle)
		if !got.Ready {
			t.Errorf("%s: settle old enough: got %+v", name, got)
		}
	}
	// No init-run file at all.
	got := parseReadinessProbe(probeOutputSpec(1000, 1007, "", "strategy=delay settle_ms=1000 deadline_ms=0", "", 0), defaultReadySettleSession, defaultReadySettle)
	if got.Ready || got.Mode != readyModeNoInitRun || got.Reason != "session younger than 8s" {
		t.Errorf("line 2 without line 1 must be ignored: got %+v", got)
	}
}

// TestWaitHarnessReady_UsesLine2Deadline: line 2's deadline replaces the
// manager's. The manager's deadline here is 100ms; line 2 says 10s, so the
// wait is still running well after 100ms and returns only when cancelled.
func TestWaitHarnessReady_UsesLine2Deadline(t *testing.T) {
	line1, line2 := initRunLines(testNonce, api.ReadySpec{Strategy: state.ReadyStrategyHook, Settle: 0, Timeout: 10 * time.Second})
	rt := newFakeReadyRuntime(func(int) (string, error) {
		return probeOutputSpec(1000, 1001, line1, line2, "", 0), nil
	})
	timings := fastReadyTimings
	timings.Deadline = 100 * time.Millisecond
	mgr := &AgentManager{Runtime: rt, readyTimings: timings}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan time.Duration, 1)
	start := time.Now()
	go func() {
		mgr.waitHarnessReady(ctx, "test-agent", "")
		done <- time.Since(start)
	}()
	select {
	case waited := <-done:
		cancel()
		t.Fatalf("wait returned after %s; line 2's 10s deadline should still hold", waited)
	case <-time.After(600 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("wait did not return after cancel")
	}
}

// TestWaitHarnessReady_UsesLine2Settle: once the marker appears during the
// wait, the settle slept is line 2's (300ms), not the manager's (50ms).
func TestWaitHarnessReady_UsesLine2Settle(t *testing.T) {
	const settle = 300 * time.Millisecond
	line1, line2 := initRunLines(testNonce, api.ReadySpec{Strategy: state.ReadyStrategyHook, Settle: settle, Timeout: 30 * time.Second})
	marker := strings.TrimSuffix(state.FormatReadyMarker(testNonce), "\n")
	var readyAt time.Time
	rt := newFakeReadyRuntime(func(n int) (string, error) {
		if n < 3 {
			return probeOutputSpec(1000, 1001, line1, line2, "", 0), nil
		}
		if readyAt.IsZero() {
			readyAt = time.Now()
		}
		return probeOutputSpec(1000, 1100, line1, line2, marker, 900), nil
	})
	mgr := &AgentManager{Runtime: rt, readyTimings: fastReadyTimings}
	mgr.waitHarnessReady(context.Background(), "test-agent", "")
	if readyAt.IsZero() {
		t.Fatal("marker never probed")
	}
	if slept := time.Since(readyAt); slept < settle {
		t.Errorf("settled %s after the marker appeared, want at least line 2's %s", slept, settle)
	}
}

// legacyReadinessProbeScript is the readiness probe script of hosts that
// predate the ready spec, copied verbatim, so the format test below runs
// what an old host runs.
func legacyReadinessProbeScript() string {
	return `if tmux has-session -t scion 2>/dev/null; then ` +
		`echo "session=$(tmux display-message -p -t scion: '#{session_created}' 2>/dev/null)"; ` +
		`else echo "session="; fi; ` +
		`echo "now=$(date +%s)"; ` +
		`echo "initrun=$(` + regularFileGuard(state.HarnessInitRunFile) + ` && head -c 256 ` + state.HarnessInitRunFile + ` 2>/dev/null | head -n 1)"; ` +
		`echo "ready=$(` + regularFileGuard(state.HarnessReadyFile) + ` && head -c 256 ` + state.HarnessReadyFile + ` 2>/dev/null | head -n 1)"; ` +
		`echo "readymtime=$(` + regularFileGuard(state.HarnessReadyFile) + ` && stat -c %Y ` + state.HarnessReadyFile + ` 2>/dev/null)"`
}

// TestReadinessProbeArgv_KeepsLegacyInitRunLine pins the new probe's initrun
// command to the legacy one, so new and old hosts read line 1 the same way.
func TestReadinessProbeArgv_KeepsLegacyInitRunLine(t *testing.T) {
	legacy := legacyReadinessProbeScript()
	start := strings.Index(legacy, `echo "initrun=`)
	end := strings.Index(legacy, `echo "ready=`)
	if start < 0 || end < 0 {
		t.Fatal("legacy script changed shape")
	}
	if script := readinessProbeArgv()[2]; !strings.Contains(script, legacy[start:end]) {
		t.Errorf("probe script no longer contains the legacy initrun command %q", legacy[start:end])
	}
}

// TestOldHostProbeReadsLine1Only runs the legacy probe script, as a host
// older than the ready spec does, against a two-line init-run file in a
// private tmux server, and checks the old host still sees a valid hook-mode
// line 1 and gates on the ready marker.
func TestOldHostProbeReadsLine1Only(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping real-tmux integration test in short mode")
	}
	if goruntime.GOOS == "darwin" {
		t.Skip("the probe's stat -c is not available on darwin")
	}
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not installed; skipping real-tmux integration test")
	}
	requireTmuxVersionFloor(t, tmuxPath)

	dir, err := os.MkdirTemp("", "tmxo")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "sock")
	initRun := filepath.Join(dir, "init-run")
	ready := filepath.Join(dir, "ready")
	const nonce = "0123456789abcdef"

	runTmux := func(args ...string) {
		t.Helper()
		if out, err := exec.Command(tmuxPath, append([]string{"-S", sock}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("tmux %v: %v (%s)", args, err, out)
		}
	}
	runTmux("-f", "/dev/null", "new-session", "-d", "-s", "scion", "-x", "80", "-y", "24", "sleep 60")
	t.Cleanup(func() { _ = exec.Command(tmuxPath, "-S", sock, "kill-server").Run() })

	spec := state.DefaultReadySpec(state.ReadyStrategyHook)
	content := state.FormatInitRunSpec(nonce, spec)
	if err := os.WriteFile(initRun, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	line1, _ := initRunLines(nonce, spec)

	probe := func() string {
		t.Helper()
		script := legacyReadinessProbeScript()
		script = strings.ReplaceAll(script, "tmux ", tmuxPath+" -S "+sock+" ")
		script = strings.ReplaceAll(script, state.HarnessInitRunFile, initRun)
		script = strings.ReplaceAll(script, state.HarnessReadyFile, ready)
		out, err := exec.Command("sh", "-c", script).CombinedOutput()
		if err != nil {
			t.Fatalf("legacy probe: %v (%s)", err, out)
		}
		return string(out)
	}

	out := probe()
	fields := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(l, "="); ok {
			fields[k] = v
		}
	}
	if fields["initrun"] != line1 {
		t.Fatalf("old host read initrun %q, want line 1 %q", fields["initrun"], line1)
	}
	if _, ok := fields["initspec"]; ok {
		t.Fatalf("legacy probe printed initspec: %q", out)
	}
	if fields["session"] == "" {
		t.Fatalf("legacy probe saw no tmux session: %q", out)
	}
	// Line 1 alone is the legacy single-line format.
	if gotNonce, mode, ok := state.ParseInitRun(fields["initrun"]); !ok || gotNonce != nonce || mode != state.ReadyModeHook {
		t.Fatalf("line 1 %q: ParseInitRun = %q %q %v", fields["initrun"], gotNonce, mode, ok)
	}
	// Without the marker the old host waits; the session-age rule does not
	// apply in hook mode.
	if got := parseReadinessProbe(out, defaultReadySettleSession, defaultReadySettle); got.Ready || got.Mode != state.ReadyModeHook || got.Reason != "ready marker missing" {
		t.Fatalf("no marker: got %+v, want hook mode waiting for the marker", got)
	}
	if err := os.WriteFile(ready, []byte(state.FormatReadyMarker(nonce)), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := parseReadinessProbe(probe(), defaultReadySettleSession, defaultReadySettle); !got.Ready || got.Nonce != nonce {
		t.Fatalf("with marker: got %+v, want ready for nonce %s", got, nonce)
	}
}

// TestReadinessProbeScript_ReadsLine2 runs the production probe script (tmux
// replaced by false) against a two-line init-run file: initrun is line 1 and
// initspec line 2; a one-line file gives an empty initspec, and a symlinked
// file gives neither.
func TestReadinessProbeScript_ReadsLine2(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	dir := t.TempDir()
	initRun := filepath.Join(dir, "init-run")
	ready := filepath.Join(dir, "ready")
	run := func() map[string]string {
		script := readinessProbeArgv()[2]
		script = strings.ReplaceAll(script, "tmux ", "false ")
		script = strings.ReplaceAll(script, state.HarnessInitRunFile, initRun)
		script = strings.ReplaceAll(script, state.HarnessReadyFile, ready)
		out, err := exec.Command("sh", "-c", script).CombinedOutput()
		if err != nil {
			t.Fatalf("probe script failed: %v (%s)", err, out)
		}
		f := map[string]string{}
		for _, line := range strings.Split(string(out), "\n") {
			if k, v, ok := strings.Cut(line, "="); ok {
				f[k] = v
			}
		}
		return f
	}

	spec := api.ReadySpec{Strategy: state.ReadyStrategyHook, Settle: 2 * time.Second, Timeout: 30 * time.Second}
	line1, line2 := initRunLines("aa11", spec)
	if err := os.WriteFile(initRun, []byte(state.FormatInitRunSpec("aa11", spec)), 0o644); err != nil {
		t.Fatal(err)
	}
	if f := run(); f["initrun"] != line1 || f["initspec"] != line2 {
		t.Fatalf("two-line file: initrun=%q initspec=%q, want %q and %q", f["initrun"], f["initspec"], line1, line2)
	}

	if err := os.WriteFile(initRun, []byte(state.FormatInitRun("aa11", state.ReadyModeHook)), 0o644); err != nil {
		t.Fatal(err)
	}
	if f := run(); f["initrun"] != line1 || f["initspec"] != "" {
		t.Fatalf("one-line file: initrun=%q initspec=%q, want %q and empty", f["initrun"], f["initspec"], line1)
	}

	target := filepath.Join(t.TempDir(), "init-run")
	if err := os.WriteFile(target, []byte(state.FormatInitRunSpec("aa11", spec)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(initRun); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, initRun); err != nil {
		t.Fatal(err)
	}
	if f := run(); f["initrun"] != "" || f["initspec"] != "" {
		t.Fatalf("symlinked file must read as missing: initrun=%q initspec=%q", f["initrun"], f["initspec"])
	}
}

// TestRealTmuxTwoLineInitRunGatesOnMarkerAndSettle runs the real-tmux
// delayed-reader scenario with a two-line init-run file: delivery waits for
// the ready marker (written realTmuxMarkerDelay after the session starts)
// and then line 2's settle (2s), not the manager's (200ms).
func TestRealTmuxTwoLineInitRunGatesOnMarkerAndSettle(t *testing.T) {
	const nonce = "0123456789abcdef"
	spec := api.ReadySpec{Strategy: state.ReadyStrategyHook, Settle: 2 * time.Second, Timeout: 30 * time.Second}
	took := runRealTmuxDelayedReader(t, nonce, state.FormatInitRunSpec(nonce, spec), harnessReadyTimings{
		PollInterval:   100 * time.Millisecond,
		Deadline:       15 * time.Second,
		Settle:         200 * time.Millisecond,
		SettleFallback: 8 * time.Second,
	})
	if want := realTmuxMarkerDelay + spec.Settle; took < want {
		t.Errorf("message delivered %s after the session started; want at least the marker delay plus line 2's settle, %s", took, want)
	}
}

// TestReadySpecFromEmbeddedConfig checks the reader chain for every built-in
// harness: the embedded config's ReadySpec, written to the init-run file,
// gives the readiness mode of the legacy hard-coded table (hook for claude,
// gemini-cli and grok-build; settle with 8s for the rest, copilot included),
// with the legacy timings.
func TestReadySpecFromEmbeddedConfig(t *testing.T) {
	legacyHook := map[string]bool{"claude": true, "gemini-cli": true, "grok-build": true}
	names := harness.AllHarnessNames()
	if len(names) < 9 {
		t.Fatalf("AllHarnessNames() = %v, want the nine bundles", names)
	}
	for _, name := range append(names, "generic", "no-such-harness") {
		spec := harness.New(name).ReadySpec()
		run, ok := state.ParseInitRunSpec(state.FormatInitRunSpec("aa11", spec))
		if !ok {
			t.Fatalf("%s: init-run content does not parse", name)
		}
		want := state.DefaultReadySpec(state.ReadyStrategyDelay)
		wantMode := state.ReadyModeSettle
		if legacyHook[name] {
			want = state.DefaultReadySpec(state.ReadyStrategyHook)
			wantMode = state.ReadyModeHook
		}
		if spec != want || run.Mode != wantMode || run.Spec != want {
			t.Errorf("%s: spec %+v mode %s, want %+v mode %s", name, spec, run.Mode, want, wantMode)
		}
	}
	if want := (api.ReadySpec{Strategy: "hook", Settle: 1500 * time.Millisecond, Timeout: 90 * time.Second}); state.DefaultReadySpec(state.ReadyStrategyHook) != want {
		t.Errorf("hook defaults changed: %+v", state.DefaultReadySpec(state.ReadyStrategyHook))
	}
	if want := (api.ReadySpec{Strategy: "delay", Settle: 8 * time.Second}); state.DefaultReadySpec(state.ReadyStrategyDelay) != want {
		t.Errorf("delay defaults changed: %+v", state.DefaultReadySpec(state.ReadyStrategyDelay))
	}
}
