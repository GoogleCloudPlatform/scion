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
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// CR-0 (ptone/scion#4755): the ctx-aware sandbox name lock, file-authoritative
// reads, and nonce compare-and-set state writes. Each test names the
// mutant (the production change) it must catch.

// -----------------------------------------------------------------------
// Test-only store helpers. The non-CAS writers exist only here, so
// production code cannot call them.
// -----------------------------------------------------------------------

// add writes e under e.SandboxName against the file's current nonce.
func (s *sandboxStateStore) add(e *sandboxStateEntry) {
	_, n, err := s.diskEntry(e.SandboxName)
	if err != nil {
		panic(err)
	}
	if _, err := s.casPut(e.SandboxName, n, e); err != nil {
		panic(err)
	}
}

// remove removes name against the file's current nonce.
func (s *sandboxStateStore) remove(name string) {
	_, n, err := s.diskEntry(name)
	if err != nil {
		panic(err)
	}
	if err := s.casRemove(name, n); err != nil {
		panic(err)
	}
}

// markStopped marks name stopped against the file's current nonce; an
// absent key is a no-op.
func (s *sandboxStateStore) markStopped(name string, exitCode *int, stoppedAt *time.Time) {
	e, n, err := s.diskEntry(name)
	if err != nil {
		panic(err)
	}
	if e == nil {
		return
	}
	if _, err := s.casMarkStopped(name, n, exitCode, stoppedAt); err != nil {
		panic(err)
	}
}

// observe returns the state file's entry for name and its nonce.
func (r *CloudRunSandboxRuntime) observe(name string) sandboxObserved {
	e, n, err := r.state.diskEntry(name)
	if err != nil {
		panic(err)
	}
	return sandboxObserved{entry: e, nonce: n}
}

// siblingRuntime returns another runtime instance on rt's state file and
// binary, as the factory builds one per lookup. Its cache is loaded now.
func siblingRuntime(t *testing.T, rt *CloudRunSandboxRuntime, bin string) *CloudRunSandboxRuntime {
	t.Helper()
	if bin == "" {
		bin = rt.bin
	}
	sib := &CloudRunSandboxRuntime{
		bin:          bin,
		state:        newSandboxStateStore(rt.state.path),
		rootDir:      t.TempDir(),
		watchCancels: make(map[string]context.CancelFunc),
	}
	cancelWatchersOnCleanup(t, sib)
	return sib
}

// cancelWatchersOnCleanup cancels rt's exit watchers when the test ends.
func cancelWatchersOnCleanup(t *testing.T, rt *CloudRunSandboxRuntime) {
	t.Helper()
	t.Cleanup(func() {
		rt.watchMu.Lock()
		defer rt.watchMu.Unlock()
		for name, cancel := range rt.watchCancels {
			cancel()
			delete(rt.watchCancels, name)
		}
	})
}

// readStateForTest reads the state file at path.
func readStateForTest(t *testing.T, path string) map[string]*sandboxStateEntry {
	t.Helper()
	entries, err := readSandboxStateFile(path)
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	return entries
}

// waitForFile waits up to 10 s for path to exist.
func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

// touch creates path.
func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, nil, 0644); err != nil {
		t.Fatal(err)
	}
}

func runEntry(name, runID string) *sandboxStateEntry {
	labels := map[string]string{"scion.name": name}
	if runID != "" {
		labels[api.LabelRunID] = runID
	}
	return &sandboxStateEntry{SandboxName: name, AgentID: name, Labels: labels, CreatedAt: time.Now()}
}

// -----------------------------------------------------------------------
// Store basics
// -----------------------------------------------------------------------

// Old state files (no nonce field) load, and their entries carry the
// legacy nonce "", which nonceAbsent never matches.
func TestSandboxStateStore_LegacyFileLoadsAndAbsentIsNotEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	legacy := `{"dev": {"sandbox_name": "dev", "agent_id": "dev", "labels": {"scion.name": "dev"}, "stopped": false}}`
	if err := os.WriteFile(path, []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	ss := newSandboxStateStore(path)
	if e := ss.get("dev"); e == nil || e.AgentID != "dev" || e.Nonce != "" {
		t.Fatalf("legacy entry = %+v, want agent dev with nonce \"\"", e)
	}
	e, n, err := ss.diskEntry("dev")
	if err != nil || e == nil {
		t.Fatalf("diskEntry = %+v, %v", e, err)
	}
	if n == nonceAbsent {
		t.Fatal("a legacy entry's nonce equals nonceAbsent")
	}
	if err := ss.casRemove("dev", nonceAbsent); !errors.Is(err, errStateChanged) {
		t.Fatalf("casRemove(absent) on a legacy entry = %v, want errStateChanged", err)
	}
	if err := ss.casRemove("dev", sandboxNonce{value: "", present: true}); err != nil {
		t.Fatalf("casRemove(\"\") on a legacy entry = %v, want nil", err)
	}
	if _, ok := readStateForTest(t, path)["dev"]; ok {
		t.Error("legacy entry still in the file after casRemove")
	}
	// A missing key matches nonceAbsent only.
	if _, err := ss.casPut("dev", sandboxNonce{value: "", present: true}, runEntry("dev", "")); !errors.Is(err, errStateChanged) {
		t.Fatalf("casPut(\"\") on a missing key = %v, want errStateChanged", err)
	}
}

// Every write regenerates the nonce; a write against a stale nonce
// changes nothing.
func TestSandboxStateStore_NonceRegeneratedAndStaleWriteRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	ss := newSandboxStateStore(path)
	n1, err := ss.casPut("dev", nonceAbsent, runEntry("dev", "run-a"))
	if err != nil || !n1.present || n1.value == "" {
		t.Fatalf("casPut = %v, %v", n1, err)
	}
	marked, err := ss.casMarkStopped("dev", n1, nil, nil)
	if err != nil || marked == nil || !marked.Stopped {
		t.Fatalf("casMarkStopped = %+v, %v", marked, err)
	}
	n2 := nonceOf(marked)
	if n2 == n1 {
		t.Fatal("mark-stopped kept the nonce")
	}
	before, _ := os.ReadFile(path)
	if err := ss.casRemove("dev", n1); !errors.Is(err, errStateChanged) {
		t.Fatalf("casRemove(stale) = %v, want errStateChanged", err)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Error("a refused write changed the file")
	}
}

// -----------------------------------------------------------------------
// S0a: Delete takes the name lock.
// Mutant S0a: Delete without the name lock.
// -----------------------------------------------------------------------

func TestCloudRunSandboxDelete_WaitsForSameNameRun(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	release := filepath.Join(dir, "release")
	bin := writeMockBin(t, fmt.Sprintf(`[ "$1" = wait ] && exec sleep 60
echo "$1" >> %q
if [ "$1" = run ]; then
  while [ ! -f %q ]; do sleep 0.02; done
fi
exit 0`, log, release))
	rt := newWorkaroundTestRuntime(t, bin)
	cancelWatchersOnCleanup(t, rt)

	runDone := make(chan error, 1)
	var runReturned time.Time
	go func() {
		_, err := rt.Run(context.Background(), fenceRunConfig(t, "run-a"))
		runReturned = time.Now()
		runDone <- err
	}()
	// Wait until the mock `run` is executing inside Run.
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(sandboxCalls(t, log), "run") {
		if time.Now().After(deadline) {
			t.Fatal("mock run never started")
		}
		time.Sleep(10 * time.Millisecond)
	}

	deleteDone := make(chan error, 1)
	var deleteReturned time.Time
	go func() {
		err := rt.Delete(context.Background(), RunRef{ID: "dev"})
		deleteReturned = time.Now()
		deleteDone <- err
	}()

	select {
	case err := <-deleteDone:
		t.Errorf("Delete returned (%v) while a same-name Run was inside `sandbox run`", err)
	case <-time.After(300 * time.Millisecond):
	}
	touch(t, release)
	if err := <-runDone; err != nil {
		t.Fatalf("Run: %v", err)
	}
	select {
	case err := <-deleteDone:
		if err != nil {
			t.Fatalf("Delete: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Delete did not return after Run returned")
	}
	if deleteReturned.Before(runReturned) {
		t.Error("Delete returned before Run")
	}
	// Delete saw (and removed) the entry Run recorded.
	if _, ok := readStateForTest(t, rt.state.path)["dev"]; ok {
		t.Error("Run's entry still in the file after Delete")
	}
}

// -----------------------------------------------------------------------
// S0b: Run's held check reads the file.
// Mutant S0b: the held check reads the instance's cache only.
// -----------------------------------------------------------------------

func TestCloudRunSandboxRun_HeldCheckReadsStateFile(t *testing.T) {
	rtX, log := newFenceSandboxRuntime(t, 0)
	rtY := siblingRuntime(t, rtX, "")
	rtY.state.add(runEntry("dev", "run-b"))
	if rtX.state.get("dev") != nil {
		t.Fatal("setup: instance X's cache already has run B's entry")
	}

	_, err := rtX.Run(context.Background(), fenceRunConfig(t, "run-a"))
	if !errors.Is(err, ErrRunConflict) {
		t.Fatalf("Run A on instance X = %v, want ErrRunConflict", err)
	}
	if calls := sandboxCalls(t, log); strings.Contains(calls, "run") {
		t.Errorf("Run A launched a sandbox; calls:\n%s", calls)
	}
	if e := readStateForTest(t, rtX.state.path)["dev"]; e == nil || e.Labels[api.LabelRunID] != "run-b" {
		t.Errorf("file entry = %+v, want run B's", e)
	}
}

// -----------------------------------------------------------------------
// S0c: writes from two instances never lose each other's entries.
// Mutants S0c-put, S0c-remove, S0c-mark, S0c-reconcile, S0c-delete-plain,
// S0c-delete-workaround: that write path saves the instance's whole
// cached map instead of a compare-and-set of one key.
// -----------------------------------------------------------------------

// gonePerKeyBin returns a mock sandbox binary whose exec probe answers
// "not found" for key and succeeds for any other name; other verbs succeed.
func gonePerKeyBin(t *testing.T, key string) string {
	t.Helper()
	return writeMockBin(t, fmt.Sprintf(`[ "$1" = wait ] && exec sleep 60
if [ "$1" = exec ] && [ "$2" = %q ]; then echo "sandbox not found" >&2; exit 1; fi
exit 0`, key))
}

func TestSandboxState_TwoInstancesKeepEachOthersWrites(t *testing.T) {
	type op func(t *testing.T, rt *CloudRunSandboxRuntime, key string)
	put := func(t *testing.T, rt *CloudRunSandboxRuntime, key string) {
		if _, err := rt.state.casPut(key, nonceAbsent, runEntry(key, "run-"+key)); err != nil {
			t.Fatalf("casPut %s: %v", key, err)
		}
	}
	remove := func(t *testing.T, rt *CloudRunSandboxRuntime, key string) {
		if err := rt.state.casRemove(key, nonceOf(rt.state.get(key))); err != nil {
			t.Fatalf("casRemove %s: %v", key, err)
		}
	}
	mark := func(t *testing.T, rt *CloudRunSandboxRuntime, key string) {
		code := 3
		if _, err := rt.state.casMarkStopped(key, nonceOf(rt.state.get(key)), &code, nil); err != nil {
			t.Fatalf("casMarkStopped %s: %v", key, err)
		}
	}
	reconcile := func(t *testing.T, rt *CloudRunSandboxRuntime, key string) {
		rt.state.reconcile(gonePerKeyBin(t, key))
	}
	del := func(workaround bool) op {
		return func(t *testing.T, rt *CloudRunSandboxRuntime, key string) {
			withDeleteWorkaround(t, workaround)
			if err := rt.Delete(context.Background(), RunRef{ID: key}); err != nil {
				t.Fatalf("Delete %s: %v", key, err)
			}
		}
	}

	present := func(t *testing.T, e *sandboxStateEntry, key string) {
		if e == nil || e.AgentID != key || e.Nonce == "" {
			t.Errorf("entry %s = %+v, want it written with a nonce", key, e)
		}
	}
	stopped := func(t *testing.T, e *sandboxStateEntry, key string) {
		if e == nil || !e.Stopped || e.ExitCode == nil || *e.ExitCode != 3 {
			t.Errorf("entry %s = %+v, want it stopped with exit 3", key, e)
		}
	}
	gone := func(t *testing.T, e *sandboxStateEntry, key string) {
		if e != nil {
			t.Errorf("entry %s = %+v, want it removed", key, e)
		}
	}

	cases := []struct {
		name   string
		seed   string // "", "nonce" (entries written through the store) or "legacy" (raw JSON, no nonce)
		op     op
		expect func(t *testing.T, e *sandboxStateEntry, key string)
	}{
		{"put", "", put, present},
		{"remove", "nonce", remove, gone},
		{"mark stopped", "nonce", mark, stopped},
		{"reconcile removal", "nonce", reconcile, gone},
		{"removal after plain delete", "nonce", del(false), gone},
		{"removal after workaround delete", "nonce", del(true), gone},
		{"legacy remove", "legacy", remove, gone},
		{"legacy mark stopped", "legacy", mark, stopped},
		{"legacy reconcile removal", "legacy", reconcile, gone},
		{"legacy removal after plain delete", "legacy", del(false), gone},
		{"legacy removal after workaround delete", "legacy", del(true), gone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bin := writeMockBin(t, `[ "$1" = wait ] && exec sleep 60
exit 0`)
			path := filepath.Join(t.TempDir(), "state.json")
			switch tc.seed {
			case "nonce":
				seed := newSandboxStateStore(path)
				seed.add(runEntry("k1", "run-k1"))
				seed.add(runEntry("k2", "run-k2"))
			case "legacy":
				raw := `{"k1": {"sandbox_name": "k1", "agent_id": "k1", "labels": {"scion.name": "k1"}},
"k2": {"sandbox_name": "k2", "agent_id": "k2", "labels": {"scion.name": "k2"}}}`
				if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
					t.Fatal(err)
				}
			}
			base := &CloudRunSandboxRuntime{bin: bin, state: &sandboxStateStore{path: path}}
			rtX := siblingRuntime(t, base, bin)
			rtY := siblingRuntime(t, base, bin) // loaded before X writes
			// An entry written after both instances loaded: no write may drop it.
			newSandboxStateStore(path).add(runEntry("z", "run-z"))

			tc.op(t, rtX, "k1")
			tc.op(t, rtY, "k2")

			file := readStateForTest(t, path)
			tc.expect(t, file["k1"], "k1")
			tc.expect(t, file["k2"], "k2")
			if file["z"] == nil {
				t.Error("entry z, written by a third instance, was dropped")
			}
		})
	}
}

// -----------------------------------------------------------------------
// S0d: a late exit watcher never marks a newer run's entry stopped.
// Mutant S0d: mark-stopped is not a compare-and-set (it marks whatever
// entry holds the key).
// -----------------------------------------------------------------------

func TestCloudRunSandboxWatch_LateWatcherDoesNotMarkNewerRun(t *testing.T) {
	dir := t.TempDir()
	release := filepath.Join(dir, "release")
	bin := writeMockBin(t, fmt.Sprintf(`if [ "$1" = wait ]; then
  while [ ! -f %q ]; do sleep 0.02; done
  echo 0; exit 0
fi
exit 0`, release))
	rtX := newWorkaroundTestRuntime(t, bin)
	rtY := siblingRuntime(t, rtX, "")

	nonceA, err := rtX.state.casPut("dev", nonceAbsent, runEntry("dev", "run-a"))
	if err != nil {
		t.Fatal(err)
	}
	watchDone := make(chan struct{})
	go func() {
		rtX.watchSandbox(context.Background(), "dev", nonceA)
		close(watchDone)
	}()

	// Instance Y deletes run A, and run B re-uses the name.
	if err := rtY.Delete(context.Background(), RunRef{ID: "dev", RunID: "run-a"}); err != nil {
		t.Fatalf("Delete A on Y: %v", err)
	}
	if _, err := rtY.state.casPut("dev", nonceAbsent, runEntry("dev", "run-b")); err != nil {
		t.Fatalf("write B: %v", err)
	}

	touch(t, release) // X's watcher for A now fires
	select {
	case <-watchDone:
	case <-time.After(10 * time.Second):
		t.Fatal("watcher did not return")
	}
	e := readStateForTest(t, rtX.state.path)["dev"]
	if e == nil || e.Labels[api.LabelRunID] != "run-b" {
		t.Fatalf("file entry = %+v, want run B's", e)
	}
	if e.Stopped {
		t.Error("X's late watcher for run A marked run B's entry stopped")
	}
}

// -----------------------------------------------------------------------
// S0e: a stale reconcile never removes a newer entry, and never hangs.
// Mutant S0e-cas: the reconcile removal is not a compare-and-set.
// Mutant S0e-mu: the removal is made inside the store-mutex section.
// -----------------------------------------------------------------------

func TestSandboxReconcile_StaleSnapshotKeepsNewerEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	stoppedA := runEntry("dev", "run-a")
	stoppedA.Stopped = true
	newSandboxStateStore(path).add(stoppedA)

	z := newSandboxStateStore(path) // snapshot holds the stopped A
	prev := sandboxReconcileBeforeRemoveHook
	t.Cleanup(func() { sandboxReconcileBeforeRemoveHook = prev })
	sandboxReconcileBeforeRemoveHook = func(name string) {
		if name != "dev" {
			return
		}
		// Run B writes its entry to the key before Z removes A.
		w := newSandboxStateStore(path)
		if _, err := w.casPut("dev", nonceOf(w.get("dev")), runEntry("dev", "run-b")); err != nil {
			t.Errorf("write B: %v", err)
		}
	}

	done := make(chan struct{})
	go func() {
		z.reconcile(writeMockBin(t, "exit 0"))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("reconcile did not return within 5s (deadlock)")
	}
	e := readStateForTest(t, path)["dev"]
	if e == nil || e.Labels[api.LabelRunID] != "run-b" || e.Stopped {
		t.Errorf("file entry = %+v, want run B's live entry", e)
	}
}

// -----------------------------------------------------------------------
// S0f: a corrupt state file is never overwritten.
// Mutant S0f: a mutation treats an unreadable file as empty and writes.
// -----------------------------------------------------------------------

func TestSandboxState_CorruptFileNeverOverwritten(t *testing.T) {
	var logs bytes.Buffer
	setRuntimeLogHandler(t, slog.NewTextHandler(&logs, nil))

	path := filepath.Join(t.TempDir(), "state.json")
	corrupt := []byte(`{"dev": {"sandbox_name": "dev", `)
	if err := os.WriteFile(path, corrupt, 0600); err != nil {
		t.Fatal(err)
	}
	ss := newSandboxStateStore(path)
	unchanged := func(t *testing.T, what string) {
		t.Helper()
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, corrupt) {
			t.Errorf("%s overwrote the corrupt state file: %q", what, got)
		}
	}
	if _, err := ss.casPut("dev", nonceAbsent, runEntry("dev", "run-a")); err == nil || errors.Is(err, errStateChanged) {
		t.Errorf("casPut = %v, want a read error", err)
	}
	unchanged(t, "casPut")
	if err := ss.casRemove("dev", nonceAbsent); err == nil || errors.Is(err, errStateChanged) {
		t.Errorf("casRemove = %v, want a read error", err)
	}
	unchanged(t, "casRemove")
	if _, err := ss.casMarkStopped("dev", nonceAbsent, nil, nil); err == nil || errors.Is(err, errStateChanged) {
		t.Errorf("casMarkStopped = %v, want a read error", err)
	}
	unchanged(t, "casMarkStopped")

	// Run and Delete fail closed before calling the sandbox CLI.
	rt, log := newFenceSandboxRuntime(t, 0)
	rt.state = ss
	if _, err := rt.Run(context.Background(), fenceRunConfig(t, "run-a")); err == nil {
		t.Error("Run on a corrupt state file succeeded")
	}
	unchanged(t, "Run")
	if err := rt.Delete(context.Background(), RunRef{ID: "dev"}); err == nil {
		t.Error("Delete on a corrupt state file succeeded")
	}
	unchanged(t, "Delete")
	if calls := sandboxCalls(t, log); calls != "" {
		t.Errorf("sandbox CLI called on a corrupt state file:\n%s", calls)
	}
	if got := logs.String(); strings.Count(got, "level=ERROR") < 3 || !strings.Contains(got, "state file is unreadable") {
		t.Errorf("want an ERROR naming the unreadable state file at load, Run and Delete:\n%s", got)
	}
}

// -----------------------------------------------------------------------
// S0g: a lock waiter honours its ctx.
// Mutant S0g: the lock wait ignores ctx (a plain blocking acquire).
// -----------------------------------------------------------------------

func TestCloudRunSandbox_NameLockWaitHonoursCtx(t *testing.T) {
	rt, log := newFenceSandboxRuntime(t, 0)
	rt.state.add(runEntry("dev", "run-a"))

	unlock, err := lockSandboxNameCtx(context.Background(), "dev")
	if err != nil {
		t.Fatal(err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			unlock()
		}
	}
	t.Cleanup(release)

	calls := map[string]func(ctx context.Context) error{
		"Delete": func(ctx context.Context) error { return rt.Delete(ctx, RunRef{ID: "dev"}) },
		"Run": func(ctx context.Context) error {
			_, err := rt.Run(ctx, fenceRunConfig(t, "run-a"))
			return err
		},
	}
	for _, name := range []string{"Delete", "Run"} {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		start := time.Now()
		done := make(chan error, 1)
		go func(call func(context.Context) error) { done <- call(ctx) }(calls[name])
		select {
		case err := <-done:
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("%s = %v, want context.DeadlineExceeded", name, err)
			}
			if elapsed := time.Since(start); elapsed > time.Second {
				t.Errorf("%s returned after %v, want about 100ms", name, elapsed)
			}
		case <-time.After(2 * time.Second):
			t.Errorf("%s still waiting on the lock 2s after its 100ms ctx ended", name)
			release()
			<-done
		}
		cancel()
	}
	if calls := sandboxCalls(t, log); calls != "" {
		t.Errorf("sandbox CLI called while the lock was held:\n%s", calls)
	}
	if e := readStateForTest(t, rt.state.path)["dev"]; e == nil || e.Labels[api.LabelRunID] != "run-a" {
		t.Errorf("file entry = %+v, want it untouched", e)
	}
}

// -----------------------------------------------------------------------
// G1 (refinement of the strict CAS): Delete removes the entry another
// instance's watcher marked stopped during the delete, and nothing else.
// Mutant G1: removeAfterDelete skips the re-read after a mismatch.
// Mutant G1b: the re-read check accepts any entry (not only the same run
// marked stopped).
// -----------------------------------------------------------------------

func TestCloudRunSandboxDelete_RemovalAfterConcurrentChange(t *testing.T) {
	for _, workaround := range []bool{false, true} {
		t.Run(fmt.Sprintf("workaround=%v", workaround), func(t *testing.T) {
			withDeleteWorkaround(t, workaround)

			t.Run("same run marked stopped by another instance's watcher", func(t *testing.T) {
				dir := t.TempDir()
				deleting := filepath.Join(dir, "deleting")
				ack := filepath.Join(dir, "ack")
				bin := writeMockBin(t, fmt.Sprintf(`case "$1" in
  wait)   while [ ! -f %[1]q ]; do sleep 0.02; done; echo 0; exit 0 ;;
  delete) touch %[1]q; while [ ! -f %[2]q ]; do sleep 0.02; done; exit 0 ;;
  *)      exit 0 ;;
esac`, deleting, ack))
				rtX := newWorkaroundTestRuntime(t, bin)
				rtY := siblingRuntime(t, rtX, "")
				nonceA, err := rtX.state.casPut("dev", nonceAbsent, runEntry("dev", "run-a"))
				if err != nil {
					t.Fatal(err)
				}
				watchDone := make(chan struct{})
				go func() {
					rtX.watchSandbox(context.Background(), "dev", nonceA)
					close(watchDone)
				}()
				delDone := make(chan error, 1)
				go func() { delDone <- rtY.Delete(context.Background(), RunRef{ID: "dev", RunID: "run-a"}) }()

				select {
				case <-watchDone:
				case <-time.After(10 * time.Second):
					t.Fatal("watcher did not return")
				}
				if e := readStateForTest(t, rtX.state.path)["dev"]; e == nil || !e.Stopped {
					t.Fatalf("setup: X's watcher did not mark A stopped: %+v", e)
				}
				touch(t, ack)
				if err := <-delDone; err != nil {
					t.Fatalf("Delete: %v", err)
				}
				if e := readStateForTest(t, rtX.state.path)["dev"]; e != nil {
					t.Errorf("entry left after Delete: %+v, want removed", e)
				}
			})

			t.Run("another run's entry is left alone", func(t *testing.T) {
				dir := t.TempDir()
				deleting := filepath.Join(dir, "deleting")
				ack := filepath.Join(dir, "ack")
				bin := writeMockBin(t, fmt.Sprintf(`case "$1" in
  delete) touch %[1]q; while [ ! -f %[2]q ]; do sleep 0.02; done; exit 0 ;;
  *)      exit 0 ;;
esac`, deleting, ack))
				rt := newWorkaroundTestRuntime(t, bin)
				rt.state.add(runEntry("dev", "run-a"))

				delDone := make(chan error, 1)
				go func() { delDone <- rt.Delete(context.Background(), RunRef{ID: "dev", RunID: "run-a"}) }()
				waitForFile(t, deleting)
				// Another writer (not fenced: another process) puts run B's
				// stopped entry under the key during the delete.
				w := newSandboxStateStore(rt.state.path)
				b := runEntry("dev", "run-b")
				b.Stopped = true
				if _, err := w.casPut("dev", nonceOf(w.get("dev")), b); err != nil {
					t.Fatal(err)
				}
				touch(t, ack)
				if err := <-delDone; err != nil {
					t.Fatalf("Delete: %v", err)
				}
				if e := readStateForTest(t, rt.state.path)["dev"]; e == nil || e.Labels[api.LabelRunID] != "run-b" {
					t.Errorf("file entry = %+v, want run B's left alone", e)
				}
			})
		})
	}
}

// -----------------------------------------------------------------------
// G2 (refinement of the strict CAS): Run's final record after the key
// changed under the name lock.
// Mutant G2-strict: no re-read/retry (a benign change fails the start).
// Mutant G2-blind: the retry skips the held check.
// Mutant G2-delete: on a conflict the start deletes the sandbox by name.
// Mutant G2-mismatch-delete: on a second mismatch the start deletes the
// sandbox by name.
// Mutant G2-nocleanup: on a first-put read or write failure (not a nonce
// mismatch) the start does not delete its own sandbox.
// Mutant G2-reread-nocleanup: the same, on a failed re-read.
// Mutant G2-retry-nocleanup: the same, on a failed retried put.
// -----------------------------------------------------------------------

// newArgsLogSandboxRuntime is newFenceSandboxRuntime (exec answers 0, wait
// blocks) with a mock that logs each call's full argv, one per line.
func newArgsLogSandboxRuntime(t *testing.T) (*CloudRunSandboxRuntime, string) {
	t.Helper()
	log := filepath.Join(t.TempDir(), "calls.log")
	bin := writeMockBin(t, fmt.Sprintf(`[ "$1" = wait ] && exec sleep 60
echo "$*" >> %q
exit 0`, log))
	rt := newWorkaroundTestRuntime(t, bin)
	cancelWatchersOnCleanup(t, rt)
	return rt, log
}

// deleteCalls returns the logged delete invocations.
func deleteCalls(t *testing.T, log string) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(sandboxCalls(t, log), "\n") {
		if strings.HasPrefix(line, "delete") {
			out = append(out, line)
		}
	}
	return out
}

func setRecordRunHook(t *testing.T, hook func(name, stage string)) {
	t.Helper()
	prev := sandboxRecordRunHook
	sandboxRecordRunHook = hook
	t.Cleanup(func() { sandboxRecordRunHook = prev })
}

// seedStoppedOld records a stopped entry of an older run under "dev".
func seedStoppedOld(rt *CloudRunSandboxRuntime) {
	old := runEntry("dev", "run-old")
	old.Stopped = true
	rt.state.add(old)
}

// removeKeyHook returns an after-launch hook in which another instance
// removes the key (a benign change: the first put then mismatches).
func removeKeyHook(t *testing.T, path string) func(string) {
	return func(string) {
		w := newSandboxStateStore(path)
		if err := w.casRemove("dev", nonceOf(w.get("dev"))); err != nil {
			t.Errorf("remove old entry: %v", err)
		}
	}
}

func TestCloudRunSandboxRun_RecordAfterConcurrentChange(t *testing.T) {
	t.Run("benign change: retried", func(t *testing.T) {
		rt, log := newArgsLogSandboxRuntime(t)
		seedStoppedOld(rt)
		setAfterLaunchHook(t, removeKeyHook(t, rt.state.path))
		if _, err := rt.Run(context.Background(), fenceRunConfig(t, "run-a")); err != nil {
			t.Fatalf("Run A = %v, want success", err)
		}
		e := readStateForTest(t, rt.state.path)["dev"]
		if e == nil || e.Labels[api.LabelRunID] != "run-a" || e.Stopped {
			t.Errorf("file entry = %+v, want run A's live entry", e)
		}
		if d := deleteCalls(t, log); len(d) != 0 {
			t.Errorf("deletes = %q, want none", d)
		}
	})

	// The own-sandbox cleanup cases: the state cannot be read at one of the
	// three record steps. The sandbox is deleted by name with --force, and
	// the unreadable file is left as it is.
	corrupt := []byte(`{"dev": `)
	for _, tc := range []struct {
		name  string
		stage string // "" = first put (after-launch hook), else the record hook stage
	}{
		{"state unreadable at first put: own sandbox deleted", ""},
		{"state unreadable at re-read: own sandbox deleted", "reread"},
		{"state unreadable at retried put: own sandbox deleted", "retry"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt, log := newArgsLogSandboxRuntime(t)
			seedStoppedOld(rt)
			corruptFile := func() {
				if err := os.WriteFile(rt.state.path, corrupt, 0600); err != nil {
					t.Errorf("corrupt state file: %v", err)
				}
			}
			if tc.stage == "" {
				setAfterLaunchHook(t, func(string) { corruptFile() })
			} else {
				setAfterLaunchHook(t, removeKeyHook(t, rt.state.path))
				setRecordRunHook(t, func(_, stage string) {
					if stage == tc.stage {
						corruptFile()
					}
				})
			}
			_, err := rt.Run(context.Background(), fenceRunConfig(t, "run-a"))
			if err == nil || errors.Is(err, ErrRunConflict) || errors.Is(err, errStateChanged) {
				t.Fatalf("Run A = %v, want a state read error", err)
			}
			if d := deleteCalls(t, log); len(d) != 1 || d[0] != "delete --force dev" {
				t.Errorf("deletes = %q, want exactly [\"delete --force dev\"]", d)
			}
			if got, _ := os.ReadFile(rt.state.path); !bytes.Equal(got, corrupt) {
				t.Errorf("corrupt state file overwritten: %q", got)
			}
		})
	}

	t.Run("second mismatch: no delete", func(t *testing.T) {
		var logs bytes.Buffer
		setRuntimeLogHandler(t, slog.NewTextHandler(&logs, nil))

		rt, log := newArgsLogSandboxRuntime(t)
		seedStoppedOld(rt)
		setAfterLaunchHook(t, removeKeyHook(t, rt.state.path))
		setRecordRunHook(t, func(_, stage string) {
			if stage != "retry" {
				return
			}
			// Another writer (not fenced: another process) records run B
			// between the re-read and the retried put.
			w := newSandboxStateStore(rt.state.path)
			if _, err := w.casPut("dev", nonceOf(w.get("dev")), runEntry("dev", "run-b")); err != nil {
				t.Errorf("write B: %v", err)
			}
		})
		_, err := rt.Run(context.Background(), fenceRunConfig(t, "run-a"))
		if !errors.Is(err, errStateChanged) {
			t.Fatalf("Run A = %v, want errStateChanged", err)
		}
		if d := deleteCalls(t, log); len(d) != 0 {
			t.Errorf("deletes = %q, want none: the key holds an entry this Run did not write", d)
		}
		e := readStateForTest(t, rt.state.path)["dev"]
		if e == nil || e.Labels[api.LabelRunID] != "run-b" || e.Stopped {
			t.Errorf("file entry = %+v, want run B's live entry", e)
		}
		if got := logs.String(); !strings.Contains(got, "level=WARN") || !strings.Contains(got, "sandbox=dev") || !strings.Contains(got, "untracked") {
			t.Errorf("no WARN naming the sandbox as untracked:\n%s", got)
		}
	})

	t.Run("another run took the key: conflict, no delete", func(t *testing.T) {
		var logs bytes.Buffer
		setRuntimeLogHandler(t, slog.NewTextHandler(&logs, nil))

		rt, log := newArgsLogSandboxRuntime(t)
		seedStoppedOld(rt)
		setAfterLaunchHook(t, func(string) {
			// Another writer (not fenced: another process) records run B live.
			w := newSandboxStateStore(rt.state.path)
			if _, err := w.casPut("dev", nonceOf(w.get("dev")), runEntry("dev", "run-b")); err != nil {
				t.Errorf("write B: %v", err)
			}
		})
		_, err := rt.Run(context.Background(), fenceRunConfig(t, "run-a"))
		if !errors.Is(err, ErrRunConflict) {
			t.Fatalf("Run A = %v, want ErrRunConflict", err)
		}
		e := readStateForTest(t, rt.state.path)["dev"]
		if e == nil || e.Labels[api.LabelRunID] != "run-b" || e.Stopped {
			t.Errorf("file entry = %+v, want run B's live entry", e)
		}
		if d := deleteCalls(t, log); len(d) != 0 {
			t.Errorf("deletes = %q, want none after the conflict", d)
		}
		got := logs.String()
		if !strings.Contains(got, "level=WARN") || !strings.Contains(got, "sandbox=dev") || !strings.Contains(got, "untracked") {
			t.Errorf("no WARN naming the sandbox as possibly untracked:\n%s", got)
		}
	})
}
