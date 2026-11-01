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
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// cloudrun-sandbox state robustness (ptone/scion#4819): null entries,
// watcher replacement, stale temp files.

// robustNullStateJSON is a state file with one real entry and one key
// stored as JSON null.
const robustNullStateJSON = `{
  "gone": null,
  "live": {"sandbox_name": "live", "agent_id": "live", "nonce": "n1"}
}`

// robustWriteState writes data as a state file in a new temp dir and
// returns its path.
func robustWriteState(t *testing.T, data string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// robustNoPanic runs fn and turns a panic into a test failure.
func robustNoPanic(t *testing.T, what string, fn func()) {
	t.Helper()
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("%s panicked: %v", what, p)
		}
	}()
	fn()
}

// A null entry is treated as absent when the file is read.
// Fault check F1: the nil-entry drop in readSandboxStateFile is removed.
func TestSandboxStateNullEntryDropped(t *testing.T) {
	path := robustWriteState(t, robustNullStateJSON)
	entries, err := readSandboxStateFile(path)
	if err != nil {
		t.Fatalf("readSandboxStateFile: %v", err)
	}
	if _, ok := entries["gone"]; ok {
		t.Errorf("null entry %q kept in the read map; want it treated as absent", "gone")
	}
	if e := entries["live"]; e == nil || e.Nonce != "n1" {
		t.Errorf("entry %q = %+v, want it read with nonce n1", "live", e)
	}
}

// A null entry no longer panics list or reconcile, and the real entry is
// kept by both.
// Fault check F1: the nil-entry drop in readSandboxStateFile is removed.
func TestSandboxStateNullEntryListReconcile(t *testing.T) {
	path := robustWriteState(t, robustNullStateJSON)
	bin := writeMockBin(t, "exit 0") // every probe passes
	ss := newSandboxStateStore(path)

	var listed []*sandboxStateEntry
	robustNoPanic(t, "list", func() { listed = ss.list() })
	if len(listed) != 1 || listed[0].SandboxName != "live" {
		t.Errorf("list() = %+v, want only %q", listed, "live")
	}

	robustNoPanic(t, "reconcile", func() { ss.reconcile(bin) })
	if e := ss.get("live"); e == nil {
		t.Errorf("reconcile dropped the live entry")
	}
	if e := readStateForTest(t, path)["live"]; e == nil {
		t.Errorf("state file lost the live entry after reconcile")
	}

	// The next write leaves the null key out of the file.
	if _, err := ss.casPut("other", nonceAbsent, runEntry("other", "run-o")); err != nil {
		t.Fatalf("casPut: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"gone"`) {
		t.Errorf("state file still holds the null key after a write:\n%s", data)
	}
}

// robustPids reads the pid file the mock wait writes, one pid per line.
func robustPids(t *testing.T, path string) []int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var pids []int
	for _, line := range strings.Fields(string(data)) {
		pid, err := strconv.Atoi(line)
		if err != nil {
			t.Fatalf("pid file line %q: %v", line, err)
		}
		pids = append(pids, pid)
	}
	return pids
}

// robustAlive reports whether process pid exists.
func robustAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// A second start of the same name leaves one live watcher: the first
// watcher's `sandbox wait` is killed when the second Run installs its own.
// Fault check F2: installWatcher no longer cancels the existing watcher.
func TestCloudRunSandboxSecondStartOneWatcher(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "wait.pids")
	bin := writeMockBin(t, fmt.Sprintf(`case "$1" in
  wait) echo $$ >> %q; exec sleep 60 ;;
  *)    exit 0 ;;
esac`, pidFile))
	rt := newWorkaroundTestRuntime(t, bin)
	cancelWatchersOnCleanup(t, rt)

	// waitPids waits up to 10 s for n wait processes to have started.
	waitPids := func(n int) []int {
		t.Helper()
		var pids []int
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if pids = robustPids(t, pidFile); len(pids) >= n {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if len(pids) != n {
			t.Fatalf("wait processes started = %v, want %d", pids, n)
		}
		return pids
	}

	// The first watcher's wait is running before the second start.
	if _, err := rt.Run(context.Background(), fenceRunConfig(t, "run-a")); err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	waitPids(1)
	if _, err := rt.Run(context.Background(), fenceRunConfig(t, "run-a")); err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	pids := waitPids(2)

	deadline := time.Now().Add(5 * time.Second)
	for robustAlive(pids[0]) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if robustAlive(pids[0]) {
		t.Errorf("first watcher's wait (pid %d) still running after the second start", pids[0])
	}
	if !robustAlive(pids[1]) {
		t.Errorf("second watcher's wait (pid %d) is not running", pids[1])
	}
	rt.watchMu.Lock()
	n := len(rt.watchCancels)
	rt.watchMu.Unlock()
	if n != 1 {
		t.Errorf("registered watchers = %d, want 1", n)
	}
}

// An old watcher that marks its entry stopped after a newer watcher was
// installed for the name (it was past its cancel check) removes only its
// own registration: the newer watcher's cancel stays, and is not called.
// Fault check F5: releaseWatcher removes the name without comparing tokens.
func TestCloudRunSandboxOldWatcherKeepsNewerCancel(t *testing.T) {
	bin := writeMockBin(t, `[ "$1" = wait ] && { echo 0; exit 0; }
exit 0`)
	rt := newWorkaroundTestRuntime(t, bin)
	cancelWatchersOnCleanup(t, rt)
	nonceA, err := rt.state.casPut("dev", nonceAbsent, runEntry("dev", "run-a"))
	if err != nil {
		t.Fatal(err)
	}

	_, tokenA := rt.installWatcher("dev")
	ctxB, _ := rt.installWatcher("dev")
	// The old watcher runs on a live context: it is past its cancel check.
	rt.watchSandbox(context.Background(), "dev", nonceA, tokenA)

	if e := readStateForTest(t, rt.state.path)["dev"]; e == nil || !e.Stopped {
		t.Fatalf("setup: the old watcher did not mark its entry stopped: %+v", e)
	}
	rt.watchMu.Lock()
	_, present := rt.watchCancels["dev"]
	rt.watchMu.Unlock()
	if !present {
		t.Errorf("the newer watcher's cancel was removed by the old watcher")
	}
	if ctxB.Err() != nil {
		t.Errorf("the newer watcher was cancelled: %v", ctxB.Err())
	}
}

// load removes temp files of interrupted state writes older than
// sandboxStateTempMaxAge; a fresh one (a write that may be in flight in
// another process) and files of other names are kept.
// Fault check F3: the os.Remove in removeStaleSandboxStateTemps is removed.
// Fault check F4: the age comparison in removeStaleSandboxStateTemps is removed.
func TestSandboxStateLoadRemovesStaleTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "state.json.tmp-111")
	fresh := filepath.Join(dir, "state.json.tmp-222")
	other := filepath.Join(dir, "other.json.tmp-333")
	for _, p := range []string{stale, fresh, other} {
		if err := os.WriteFile(p, []byte(`{}`), 0600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-time.Hour)
	for _, p := range []string{stale, other} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}

	newSandboxStateStore(path)

	if _, err := os.Stat(stale); err == nil {
		t.Errorf("stale temp file %s not removed at load", filepath.Base(stale))
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("fresh temp file %s removed at load: %v", filepath.Base(fresh), err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("temp file of another name %s removed at load: %v", filepath.Base(other), err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("state file removed at load: %v", err)
	}
}
