//go:build unix

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

package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Sandbox diagnostics: restored sandboxes are watched for an exit, a
// failed exec is logged at Warn (ptone/scion#1309), and a failed run
// names the requested image (ptone/scion#1291).

const (
	restoredWatchLog = "cloudrun-sandbox: watching restored sandbox for exit"
	execFailedLog    = "cloudrun-sandbox: exec in sandbox failed"
)

// diagBin writes a mock sandbox binary: `wait` appends the sandbox name to
// waits, then runs waitScript; `exec` runs execScript; anything else exits 0.
func diagBin(t *testing.T, waits, waitScript, execScript string) string {
	t.Helper()
	return writeMockBin(t, fmt.Sprintf(`case "$1" in
  wait) echo "$2" >> %q; %s ;;
  exec) %s ;;
esac
exit 0`, waits, waitScript, execScript))
}

// watchRegistered reports the process-wide watcher registration of
// (path, name).
func watchRegistered(path, name string) (sandboxWatchReg, bool) {
	sandboxWatchersMu.Lock()
	defer sandboxWatchersMu.Unlock()
	reg, ok := sandboxWatchers[sandboxWatchKey{path: path, name: name}]
	return reg, ok
}

// stopWatchersOnCleanup cancels the exit watchers of each runtime when the
// test ends and waits up to 10 s for their registrations on statePath to
// be released, so no watcher outlives the test.
func stopWatchersOnCleanup(t *testing.T, statePath string, rts ...*CloudRunSandboxRuntime) {
	t.Helper()
	t.Cleanup(func() {
		for _, rt := range rts {
			rt.watchMu.Lock()
			for name, cancel := range rt.watchCancels {
				cancel()
				delete(rt.watchCancels, name)
			}
			rt.watchMu.Unlock()
		}
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			held := false
			sandboxWatchersMu.Lock()
			for k := range sandboxWatchers {
				held = held || k.path == statePath
			}
			sandboxWatchersMu.Unlock()
			if !held {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Errorf("watchers on %s still registered 10 s after cancel", statePath)
	})
}

// waitLines returns the sandbox names `wait` was invoked for.
func waitLines(t *testing.T, waits string) []string {
	t.Helper()
	data, err := os.ReadFile(waits)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	return strings.Fields(string(data))
}

// waitForWaitLines waits up to 10 s for at least n `wait` invocations.
func waitForWaitLines(t *testing.T, waits string, n int) []string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if got := waitLines(t, waits); len(got) >= n {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("want %d `wait` invocations within 10 s, got %q", n, waitLines(t, waits))
	return nil
}

// countRecords returns how many records have level and msg.
func countRecords(h *recordingHandler, level slog.Level, msg string) int {
	n := 0
	for _, r := range h.records() {
		if r.level == level && r.msg == msg {
			n++
		}
	}
	return n
}

// Fault check T1: no watcher for restored sandboxes. A running entry
// whose reconcile probe passes is watched; when its sandbox exits with 3,
// the entry is marked stopped with code 3 and the abnormal exit is logged.
func TestCloudRunSandboxDiag_RestoredSandboxWatched(t *testing.T) {
	h := newRecordingHandler(slog.LevelDebug)
	setRuntimeLogHandler(t, h)

	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	waits := filepath.Join(dir, "waits")
	bin := diagBin(t, waits, "exit 3", "exit 0")
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(home, 0755); err != nil {
		t.Fatal(err)
	}
	newSandboxStateStore(statePath).add(&sandboxStateEntry{SandboxName: "sb-live", AgentID: "dev", AgentHome: home})

	rt := newReconciledSandboxRuntime(bin, statePath, t.TempDir())
	stopWatchersOnCleanup(t, statePath, rt)

	deadline := time.Now().Add(10 * time.Second)
	var e *sandboxStateEntry
	for time.Now().Before(deadline) {
		if e = readStateForTest(t, statePath)["sb-live"]; e != nil && e.Stopped {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if e == nil || !e.Stopped || e.ExitCode == nil || *e.ExitCode != 3 {
		t.Fatalf("restored entry = %+v, want stopped with exit code 3", e)
	}
	deadline = time.Now().Add(10 * time.Second)
	for countRecords(h, slog.LevelError, "sandbox exited abnormally") == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if countRecords(h, slog.LevelError, "sandbox exited abnormally") != 1 {
		t.Errorf("want one \"sandbox exited abnormally\" error for the restored sandbox, records: %+v", h.records())
	}
	if countRecords(h, slog.LevelInfo, restoredWatchLog) != 1 {
		t.Errorf("want one %q line, records: %+v", restoredWatchLog, h.records())
	}
}

// Fault check T2: every snapshot entry is watched. Only the running entry
// whose probe passed is watched: not one whose probe failed, a Starting
// entry past its horizon whose probe passed, or a stopped entry.
func TestCloudRunSandboxDiag_OnlyLiveRunningEntriesWatched(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	waits := filepath.Join(dir, "waits")
	bin := diagBin(t, waits, "exec sleep 60", `[ "$2" = sb-dead ] && exit 1; exit 0`)

	past := time.Now().Add(-time.Hour)
	ss := newSandboxStateStore(statePath)
	ss.add(&sandboxStateEntry{SandboxName: "sb-live", AgentID: "a"})
	ss.add(&sandboxStateEntry{SandboxName: "sb-dead", AgentID: "b"})
	ss.add(&sandboxStateEntry{SandboxName: "sb-starting", AgentID: "c", Starting: true, StartingAt: &past, StartingUntil: &past})
	ss.add(&sandboxStateEntry{SandboxName: "sb-stopped", AgentID: "d", Stopped: true})

	rt := newReconciledSandboxRuntime(bin, statePath, t.TempDir())
	stopWatchersOnCleanup(t, statePath, rt)

	for _, name := range []string{"sb-dead", "sb-starting", "sb-stopped"} {
		if _, ok := watchRegistered(statePath, name); ok {
			t.Errorf("%s is watched, want unwatched", name)
		}
	}
	if _, ok := watchRegistered(statePath, "sb-live"); !ok {
		t.Errorf("sb-live is not watched, want watched")
	}
	waitForWaitLines(t, waits, 1)
	time.Sleep(200 * time.Millisecond)
	if got := waitLines(t, waits); len(got) != 1 || got[0] != "sb-live" {
		t.Errorf("`wait` invoked for %q, want only sb-live", got)
	}
}

// Fault check T3: no process-wide check. Two runtimes built on the same
// state file (the factory builds one per lookup) start one watcher for a
// restored sandbox, not two.
func TestCloudRunSandboxDiag_RestoredWatchDeduplicated(t *testing.T) {
	h := newRecordingHandler(slog.LevelInfo)
	setRuntimeLogHandler(t, h)

	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	waits := filepath.Join(dir, "waits")
	bin := diagBin(t, waits, "exec sleep 60", "exit 0")
	newSandboxStateStore(statePath).add(&sandboxStateEntry{SandboxName: "sb-live", AgentID: "dev"})

	first := newReconciledSandboxRuntime(bin, statePath, t.TempDir())
	second := newReconciledSandboxRuntime(bin, statePath, t.TempDir())
	stopWatchersOnCleanup(t, statePath, first, second)

	waitForWaitLines(t, waits, 1)
	time.Sleep(200 * time.Millisecond)
	if got := waitLines(t, waits); len(got) != 1 {
		t.Errorf("`wait` invoked %d times (%q), want 1", len(got), got)
	}
	second.watchMu.Lock()
	n := len(second.watchCancels)
	second.watchMu.Unlock()
	if n != 0 {
		t.Errorf("second runtime started %d watchers, want 0", n)
	}
	if c := countRecords(h, slog.LevelInfo, restoredWatchLog); c != 1 {
		t.Errorf("%q logged %d times, want 1", restoredWatchLog, c)
	}
}

// Fault check T4: Run's watcher is not registered process-wide. After a
// Run, a runtime built on the same state file starts no second watcher
// for the sandbox Run watches.
func TestCloudRunSandboxDiag_RunWatcherCountsForRestore(t *testing.T) {
	rt, env := newStartingRuntime(t, startScripts{})
	if _, err := rt.Run(context.Background(), startingCfg(t, "run-a")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	reg, ok := watchRegistered(env.state, "dev")
	if !ok {
		t.Fatal("Run's watcher is not registered")
	}
	if want := nonceOf(fileEntry(t, env.state)); reg.nonce != want {
		t.Errorf("registered nonce = %v, want the running entry's %v", reg.nonce, want)
	}

	later := newReconciledSandboxRuntime(rt.bin, env.state, t.TempDir())
	stopWatchersOnCleanup(t, env.state, rt, later)
	later.watchMu.Lock()
	n := len(later.watchCancels)
	later.watchMu.Unlock()
	if n != 0 {
		t.Errorf("a later runtime started %d watchers for the sandbox Run watches, want 0", n)
	}
}

// resetExecFailureThrottle clears the failed-exec log throttle before and
// after the test.
func resetExecFailureThrottle(t *testing.T) {
	t.Helper()
	reset := func() {
		sandboxExecFailuresMu.Lock()
		sandboxExecFailures = make(map[string]*sandboxExecFailureState)
		sandboxExecFailuresMu.Unlock()
	}
	reset()
	t.Cleanup(reset)
}

// execRuntime returns a runtime whose `exec` runs script, with the
// throttle reset and a recording log handler.
func execRuntime(t *testing.T, script string) (*CloudRunSandboxRuntime, *recordingHandler) {
	t.Helper()
	resetExecFailureThrottle(t)
	h := newRecordingHandler(slog.LevelDebug)
	setRuntimeLogHandler(t, h)
	bin := writeMockBin(t, fmt.Sprintf(`[ "$1" = exec ] && { %s; }
exit 0`, script))
	return NewCloudRunSandboxRuntimeForTest(bin, filepath.Join(t.TempDir(), "state.json"), t.TempDir()), h
}

// execWarns returns the failed-exec Warn records.
func execWarns(h *recordingHandler) []recordedLog {
	var out []recordedLog
	for _, r := range h.records() {
		if r.level == slog.LevelWarn && r.msg == execFailedLog {
			out = append(out, r)
		}
	}
	return out
}

// Fault check T5: a failed exec is not logged. A failed Exec and a failed
// ExecWithStdin each log one Warn with the sandbox name, the exit code and
// the output.
func TestCloudRunSandboxDiag_FailedExecWarn(t *testing.T) {
	for _, withStdin := range []bool{false, true} {
		t.Run(fmt.Sprintf("stdin=%v", withStdin), func(t *testing.T) {
			rt, h := execRuntime(t, "echo boom; exit 7")
			var err error
			if withStdin {
				_, err = rt.ExecWithStdin(context.Background(), "sb-a", []string{"/bin/true"}, strings.NewReader("x"))
			} else {
				_, err = rt.Exec(context.Background(), "sb-a", []string{"/bin/true"})
			}
			if err == nil {
				t.Fatal("exec succeeded, want the CLI's failure")
			}
			w := execWarns(h)
			if len(w) != 1 {
				t.Fatalf("want one %q warning, got %d: %+v", execFailedLog, len(w), h.records())
			}
			if w[0].attrs["sandbox"] != "sb-a" || w[0].attrs["exitCode"] != "7" || w[0].attrs["output"] != "boom" {
				t.Errorf("warning attrs = %v, want sandbox=sb-a exitCode=7 output=boom", w[0].attrs)
			}
			if _, ok := w[0].attrs["suppressed"]; ok {
				t.Errorf("first warning carries suppressed=%s, want no key", w[0].attrs["suppressed"])
			}
		})
	}
}

// Fault check T6: output logged under WithSensitiveExec. The Warn has the
// sandbox name and exit code but no output key.
func TestCloudRunSandboxDiag_FailedExecSensitiveNoOutput(t *testing.T) {
	rt, h := execRuntime(t, "echo boom; exit 7")
	if _, err := rt.Exec(WithSensitiveExec(context.Background()), "sb-a", []string{"/bin/true"}); err == nil {
		t.Fatal("exec succeeded, want the CLI's failure")
	}
	w := execWarns(h)
	if len(w) != 1 {
		t.Fatalf("want one %q warning, got %d", execFailedLog, len(w))
	}
	if _, ok := w[0].attrs["output"]; ok {
		t.Errorf("sensitive exec warning has an output key: %v", w[0].attrs)
	}
	if w[0].attrs["sandbox"] != "sb-a" || w[0].attrs["exitCode"] != "7" {
		t.Errorf("warning attrs = %v, want sandbox=sb-a exitCode=7", w[0].attrs)
	}
	for _, r := range h.records() {
		for _, v := range r.attrs {
			if strings.Contains(v, "boom") {
				t.Errorf("sensitive exec output in a log record: %+v", r)
			}
		}
	}
}

// Fault check T7: the whole output is logged. 10 KB of output gives an
// excerpt of at most sandboxExecFailureExcerpt bytes plus the marker.
func TestCloudRunSandboxDiag_FailedExecExcerptBounded(t *testing.T) {
	rt, h := execRuntime(t, `head -c 10240 /dev/zero | tr '\0' x; exit 1`)
	if _, err := rt.Exec(context.Background(), "sb-a", []string{"/bin/true"}); err == nil {
		t.Fatal("exec succeeded, want the CLI's failure")
	}
	w := execWarns(h)
	if len(w) != 1 {
		t.Fatalf("want one %q warning, got %d", execFailedLog, len(w))
	}
	out := w[0].attrs["output"]
	const marker = "...(truncated)"
	if len(out) != sandboxExecFailureExcerpt+len(marker) || !strings.HasSuffix(out, marker) {
		t.Errorf("output excerpt is %d bytes, want %d ending in %q", len(out), sandboxExecFailureExcerpt+len(marker), marker)
	}
}

// Fault check T8: no throttle. Three failures inside the interval give
// one Warn; the first failure after it gives a Warn with suppressed=2.
// Another sandbox is not held back.
func TestCloudRunSandboxDiag_FailedExecThrottled(t *testing.T) {
	rt, h := execRuntime(t, "exit 1")
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	setNow(t, t0)
	for i := 0; i < 3; i++ {
		_, _ = rt.Exec(context.Background(), "sb-a", []string{"/bin/true"})
	}
	if w := execWarns(h); len(w) != 1 {
		t.Fatalf("3 failures inside the interval gave %d warnings, want 1", len(w))
	}
	_, _ = rt.Exec(context.Background(), "sb-b", []string{"/bin/true"})
	if w := execWarns(h); len(w) != 2 || w[1].attrs["sandbox"] != "sb-b" {
		t.Fatalf("a failure in another sandbox was held back: %+v", w)
	}
	setNow(t, t0.Add(sandboxExecFailureLogInterval+time.Second))
	_, _ = rt.Exec(context.Background(), "sb-a", []string{"/bin/true"})
	w := execWarns(h)
	if len(w) != 3 {
		t.Fatalf("failure after the interval gave %d warnings in total, want 3", len(w))
	}
	if w[2].attrs["sandbox"] != "sb-a" || w[2].attrs["suppressed"] != "2" {
		t.Errorf("warning after the interval = %v, want sandbox=sb-a suppressed=2", w[2].attrs)
	}
}

// Fault check T9: every exec logs. A successful exec logs no Warn.
func TestCloudRunSandboxDiag_SuccessfulExecNoWarn(t *testing.T) {
	rt, h := execRuntime(t, "echo fine; exit 0")
	if out, err := rt.Exec(context.Background(), "sb-a", []string{"/bin/true"}); err != nil || out != "fine" {
		t.Fatalf("Exec = %q, %v; want \"fine\", nil", out, err)
	}
	if w := execWarns(h); len(w) != 0 {
		t.Errorf("successful exec logged %d warnings, want 0", len(w))
	}
}

// Fault check T10: the requested image is left out of the run error. A
// launcher failure that names a mirror gives an error that names the
// sandbox, the requested image (or "(none)"), the launcher's own image,
// and the launcher output.
func TestCloudRunSandboxDiag_RunFailureNamesRequestedImage(t *testing.T) {
	for _, tc := range []struct{ image, want string }{
		{"example.com/agent:v1", "requested image example.com/agent:v1;"},
		{"", "requested image (none);"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			rt, _ := newStartingRuntime(t, startScripts{run: `echo "pull failed: mirror.gcr.io/library/omni:latest" >&2; exit 1`})
			cfg := startingCfg(t, "run-a")
			cfg.Image = tc.image
			_, err := rt.Run(context.Background(), cfg)
			if err == nil {
				t.Fatal("Run succeeded, want the run failure")
			}
			for _, frag := range []string{
				"cloudrun-sandbox: run of sandbox dev failed: ",
				tc.want,
				"this tier runs the sandbox launcher's own image (rootfs /) and does not pull the requested image",
				"(output: pull failed: mirror.gcr.io/library/omni:latest",
			} {
				if !strings.Contains(err.Error(), frag) {
					t.Errorf("run error %q does not contain %q", err, frag)
				}
			}
		})
	}
}

// Fault check R1a: a restored claim replaces a registration with another
// nonce. A Run's newer registration for the name stays in place: a
// restored watch of the name starts no watcher and replaces nothing, even
// when its nonce matches the state file.
func TestCloudRunSandboxDiag_RestoredWatchKeepsAnyRegistration(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	waits := filepath.Join(dir, "waits")
	bin := diagBin(t, waits, "exec sleep 60", "exit 0")
	ss := newSandboxStateStore(statePath)
	ss.add(&sandboxStateEntry{SandboxName: "sb-x", AgentID: "dev"})
	_, diskNonce, err := ss.diskEntry("sb-x")
	if err != nil {
		t.Fatal(err)
	}

	newer := sandboxNonce{value: "newer-run", present: true}
	runCtx, runCancel := context.WithCancel(context.Background())
	runToken, ok := claimSandboxWatch(statePath, "sb-x", newer, runCancel, false)
	if !ok {
		t.Fatal("Run's claim refused")
	}
	rt := NewCloudRunSandboxRuntimeForTest(bin, statePath, t.TempDir())
	stopWatchersOnCleanup(t, statePath, rt)
	// Registered after stopWatchersOnCleanup, so it runs before it.
	t.Cleanup(func() { cancelSandboxWatch(statePath, "sb-x") })
	rt.watchRestored([]sandboxWatchTarget{{name: "sb-x", agentID: "dev", nonce: diskNonce}})

	reg, ok := watchRegistered(statePath, "sb-x")
	if !ok || reg.nonce != newer || reg.token != runToken {
		t.Errorf("registration after the restored watch = %+v (present %v), want Run's (nonce %v, token %d)", reg, ok, newer, runToken)
	}
	if runCtx.Err() != nil {
		t.Errorf("Run's watcher was cancelled by a restored watch")
	}
	rt.watchMu.Lock()
	n := len(rt.watchCancels)
	rt.watchMu.Unlock()
	if n != 0 {
		t.Errorf("restored watch started %d watchers, want 0", n)
	}
	time.Sleep(200 * time.Millisecond)
	if got := waitLines(t, waits); len(got) != 0 {
		t.Errorf("`wait` invoked for %q, want none", got)
	}
}

// Fault check R1b: the restored watch trusts the reconcile snapshot. A
// target whose entry changed on disk since the snapshot (another nonce,
// removed) or whose name lock is busy starts no watcher.
func TestCloudRunSandboxDiag_RestoredWatchRechecksEntry(t *testing.T) {
	setDuration(t, &sandboxRestoreLockWait, 50*time.Millisecond)
	for _, tc := range []string{"nonce changed", "entry removed", "name busy"} {
		t.Run(tc, func(t *testing.T) {
			dir := t.TempDir()
			statePath := filepath.Join(dir, "state.json")
			waits := filepath.Join(dir, "waits")
			bin := diagBin(t, waits, "exec sleep 60", "exit 0")
			name := "sb-recheck-" + strings.ReplaceAll(tc, " ", "-")
			ss := newSandboxStateStore(statePath)
			ss.add(&sandboxStateEntry{SandboxName: name, AgentID: "dev"})
			_, nonce, err := ss.diskEntry(name)
			if err != nil {
				t.Fatal(err)
			}
			switch tc {
			case "nonce changed":
				nonce = sandboxNonce{value: "stale-snapshot", present: true}
			case "entry removed":
				ss.remove(name)
			case "name busy":
				unlock, err := lockSandboxNameCtx(context.Background(), name)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(unlock)
			}

			rt := NewCloudRunSandboxRuntimeForTest(bin, statePath, t.TempDir())
			stopWatchersOnCleanup(t, statePath, rt)
			rt.watchRestored([]sandboxWatchTarget{{name: name, agentID: "dev", nonce: nonce}})

			if reg, ok := watchRegistered(statePath, name); ok {
				t.Errorf("restored watch registered %+v, want none", reg)
			}
			time.Sleep(200 * time.Millisecond)
			if got := waitLines(t, waits); len(got) != 0 {
				t.Errorf("`wait` invoked for %q, want none", got)
			}
		})
	}
}

// Fault check R1c: cancelWatcher stops only its own instance's watcher.
// A restored watcher started by one runtime instance is stopped by
// cancelWatcher (as Delete calls it) on another instance for the same
// state file: its registration is removed and its `sandbox wait` process
// is ended.
func TestCloudRunSandboxDiag_CancelOnOtherInstanceStopsRestoredWatch(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	waits := filepath.Join(dir, "waits")
	pidFile := filepath.Join(dir, "wait.pid")
	bin := diagBin(t, waits, fmt.Sprintf(`echo $$ > %q; exec sleep 60`, pidFile), "exit 0")
	newSandboxStateStore(statePath).add(&sandboxStateEntry{SandboxName: "sb-live", AgentID: "dev"})

	transient := newReconciledSandboxRuntime(bin, statePath, t.TempDir())
	broker := NewCloudRunSandboxRuntimeForTest(bin, statePath, t.TempDir())
	stopWatchersOnCleanup(t, statePath, transient, broker)
	if _, ok := watchRegistered(statePath, "sb-live"); !ok {
		t.Fatal("restored sandbox not watched")
	}
	waitForWaitLines(t, waits, 1)
	waitForFile(t, pidFile)
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		t.Fatalf("wait pid %q: %v", data, err)
	}

	broker.cancelWatcher("sb-live")

	alive := func() bool { return syscall.Kill(pid, 0) == nil }
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_, registered := watchRegistered(statePath, "sb-live")
		if !registered && !alive() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, registered := watchRegistered(statePath, "sb-live")
	t.Errorf("10 s after cancelWatcher on another instance: registered=%v, wait process alive=%v; want neither", registered, alive())
}

// Fault check R1d: a Run's claim leaves the replaced watcher running. A
// claim that replaces a registration (a Run's new entry) cancels the
// watcher it replaces, and the new one stays live.
func TestCloudRunSandboxDiag_RunClaimCancelsReplacedWatcher(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	t.Cleanup(func() { cancelSandboxWatch(path, "sb-x") })
	oldCtx, oldCancel := context.WithCancel(context.Background())
	if _, ok := claimSandboxWatch(path, "sb-x", sandboxNonce{value: "n1", present: true}, oldCancel, false); !ok {
		t.Fatal("first claim refused")
	}
	newCtx, newCancel := context.WithCancel(context.Background())
	if _, ok := claimSandboxWatch(path, "sb-x", sandboxNonce{value: "n2", present: true}, newCancel, false); !ok {
		t.Fatal("replacing claim refused")
	}
	if oldCtx.Err() == nil {
		t.Errorf("replaced watcher not cancelled")
	}
	if newCtx.Err() != nil {
		t.Errorf("new watcher cancelled")
	}
	if reg, ok := watchRegistered(path, "sb-x"); !ok || reg.nonce.value != "n2" {
		t.Errorf("registration = %+v (present %v), want n2", reg, ok)
	}
}
