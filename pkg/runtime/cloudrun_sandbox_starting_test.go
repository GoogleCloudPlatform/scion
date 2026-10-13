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
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
)

// CR-1 (ptone/scion#4757): the Starting entry Run writes before `sandbox
// run`, the launch classification, the start's cleanup, and List and
// reconcile of Starting entries. Each test names the fault check (a
// deliberate production change) it must catch, by the issue's ID.

// -----------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------

// startEnv holds the paths a starting-test mock binary uses: the state
// file, the call log, a copy target for the state file, and two marker
// files (mark: `run` or the probe has begun; release: let a blocking `run`
// finish).
type startEnv struct {
	state, log, copy, mark, release, pid string
}

// startScripts are shell fragments for each verb of the mock binary. They
// may use $STATE, $COPY, $MARK, $RELEASE and $PIDFILE (a background
// process's PID, killed when the test ends). An empty fragment exits 0.
type startScripts struct {
	run, exec, del string
}

// Shell fragments for startScripts.
const (
	// blockUntilKilled marks, then blocks until ctx kills it (exec, so the
	// kill hits the blocking process itself).
	blockUntilKilled = `touch "$MARK"; exec sleep 60`
	// blockUntilReleased marks, then blocks until the test creates
	// $RELEASE, then exits 0.
	blockUntilReleased = `touch "$MARK"; while [ ! -f "$RELEASE" ]; do sleep 0.02; done; exit 0`
	// copyState copies the state file as `run` sees it.
	copyState = `cp "$STATE" "$COPY"`
)

// mockStartBin writes a mock sandbox binary for env: wait blocks (a live
// sandbox), every other call is logged with its full argv, one per line,
// then runs its verb's fragment.
func mockStartBin(t *testing.T, env startEnv, sc startScripts) string {
	t.Helper()
	orZero := func(s string) string {
		if s == "" {
			return "exit 0"
		}
		return s
	}
	return writeMockBin(t, fmt.Sprintf(`STATE=%q; COPY=%q; MARK=%q; RELEASE=%q; PIDFILE=%q
[ "$1" = wait ] && exec sleep 60
echo "$*" >> %q
case "$1" in
  run) %s ;;
  exec) %s ;;
  delete) %s ;;
esac
exit 0`, env.state, env.copy, env.mark, env.release, env.pid, env.log, orZero(sc.run), orZero(sc.exec), orZero(sc.del)))
}

// newStartingRuntime returns a runtime on a fresh state file whose binary
// runs sc, and its paths. Its exit watchers are cancelled at the end.
func newStartingRuntime(t *testing.T, sc startScripts) (*CloudRunSandboxRuntime, startEnv) {
	t.Helper()
	dir := t.TempDir()
	env := startEnv{
		state:   filepath.Join(dir, "state.json"),
		log:     filepath.Join(dir, "calls.log"),
		copy:    filepath.Join(dir, "state-at-run.json"),
		mark:    filepath.Join(dir, "mark"),
		release: filepath.Join(dir, "release"),
		pid:     filepath.Join(dir, "bg.pid"),
	}
	t.Cleanup(func() { killPIDFile(env.pid) })
	rt := &CloudRunSandboxRuntime{
		bin:          mockStartBin(t, env, sc),
		state:        newSandboxStateStore(env.state),
		rootDir:      t.TempDir(),
		watchCancels: make(map[string]context.CancelFunc),
	}
	cancelWatchersOnCleanup(t, rt)
	return rt, env
}

// killPIDFile kills the process whose PID is in path, if any.
func killPIDFile(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 {
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Kill()
		}
	}
}

// sandboxRunCalls returns the logged `run` invocations.
func sandboxRunCalls(t *testing.T, log string) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(sandboxCalls(t, log), "\n") {
		if line == "run" || strings.HasPrefix(line, "run ") {
			out = append(out, line)
		}
	}
	return out
}

// wantDeletes fails the test unless exactly n deletes were logged, each
// `delete --force dev`.
func wantDeletes(t *testing.T, log string, n int) {
	t.Helper()
	d := deleteCalls(t, log)
	ok := len(d) == n
	for _, c := range d {
		ok = ok && c == "delete --force dev"
	}
	if !ok {
		t.Errorf("deletes = %q, want %d x \"delete --force dev\"", d, n)
	}
}

// fileEntry returns the state file's entry for "dev" (nil if none).
func fileEntry(t *testing.T, path string) *sandboxStateEntry {
	t.Helper()
	return readStateForTest(t, path)["dev"]
}

// startingCfg is fenceRunConfig with project fields, a template and a
// harness config label, so the full entry can be checked.
func startingCfg(t *testing.T, runID string) RunConfig {
	t.Helper()
	cfg := fenceRunConfig(t, runID)
	cfg.Project = "proj"
	cfg.ProjectID = "pid-1"
	cfg.Template = "tmpl"
	cfg.Labels[projectkeys.LabelProject] = "proj"
	cfg.Labels[projectkeys.LabelProjectID] = "pid-1"
	cfg.Labels["scion.harness_config"] = "hc"
	return cfg
}

// runSandboxAsync runs rt.Run in a goroutine and returns its error channel.
func runSandboxAsync(ctx context.Context, rt *CloudRunSandboxRuntime, cfg RunConfig) chan error {
	errc := make(chan error, 1)
	go func() {
		_, err := rt.Run(ctx, cfg)
		errc <- err
	}()
	return errc
}

// recvErr waits up to 30 s for an error from errc.
func recvErr(t *testing.T, errc chan error, what string) error {
	t.Helper()
	select {
	case err := <-errc:
		return err
	case <-time.After(30 * time.Second):
		t.Fatalf("%s did not return within 30 s", what)
		return nil
	}
}

// setHook sets *hook for one test.
func setHook[T any](t *testing.T, hook *T, v T) {
	t.Helper()
	prev := *hook
	*hook = v
	t.Cleanup(func() { *hook = prev })
}

// setDuration sets *d for one test.
func setDuration(t *testing.T, d *time.Duration, v time.Duration) {
	t.Helper()
	prev := *d
	*d = v
	t.Cleanup(func() { *d = prev })
}

// setNow sets sandboxNow to return now for one test.
func setNow(t *testing.T, now time.Time) {
	t.Helper()
	prev := sandboxNow
	sandboxNow = func() time.Time { return now }
	t.Cleanup(func() { sandboxNow = prev })
}

// crashLeftState runs a start of runID through a runtime whose `run`
// copies the state file and then fails on its own, and returns the copy:
// the state file as a crash during `sandbox run` would leave it.
func crashLeftState(t *testing.T, cfg RunConfig) []byte {
	t.Helper()
	rt, env := newStartingRuntime(t, startScripts{run: copyState + `; exit 1`})
	if _, err := rt.Run(context.Background(), cfg); err == nil {
		t.Fatal("crash-left setup: Run succeeded, want the self-failed run")
	}
	data, err := os.ReadFile(env.copy)
	if err != nil {
		t.Fatalf("crash-left setup: no copy of the state file: %v", err)
	}
	return data
}

// installState writes data as rt's state file and returns a new instance
// on it (cache loaded now), as the factory builds after a restart.
func installState(t *testing.T, rt *CloudRunSandboxRuntime, data []byte) *CloudRunSandboxRuntime {
	t.Helper()
	if err := os.WriteFile(rt.state.path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return siblingRuntime(t, rt, "")
}

// preCleanForTest mirrors start's pre-clean (pkg/agent/run.go, step 0;
// pkg/runtime cannot import pkg/agent): list the agent's entries by
// scion.name, skip other projects (matchProjectForTest), return a running
// entry when there is no task, and otherwise delete each entry, scoped to
// its run. It reports whether it returned a running entry.
func preCleanForTest(ctx context.Context, rt *CloudRunSandboxRuntime, task bool, projectName, projectID string) (bool, error) {
	agents, err := rt.List(ctx, map[string]string{"scion.name": "dev"})
	if err != nil {
		return false, nil // a failed listing skips pre-clean
	}
	for _, a := range agents {
		if !matchProjectForTest(a, projectName, projectID) {
			continue
		}
		if a.Phase == "running" && !task {
			return true, nil
		}
		if err := rt.Delete(ctx, RunRef{ID: AgentOperationID(a), RunID: a.RunID}); err != nil {
			return false, fmt.Errorf("failed to cleanup existing container: %w", err)
		}
	}
	return false, nil
}

// matchProjectForTest mirrors matchAgentProject (pkg/agent/run.go).
func matchProjectForTest(a api.AgentInfo, projectName, projectID string) bool {
	if projectID != "" {
		if l := projectkeys.ProjectIDFromLabels(a.Labels); l != "" {
			return l == projectID
		}
		if a.ProjectID != "" {
			return a.ProjectID == projectID
		}
	}
	if projectName != "" {
		if l := projectkeys.ProjectNameFromLabels(a.Labels); l != "" {
			return l == projectName
		}
		if a.Project != "" {
			return a.Project == projectName
		}
	}
	return true
}

// writeStartingEntryForTest writes a Starting entry of runID under "dev"
// directly to the file at path (as another process would), with the
// given times, and returns its nonce.
func writeStartingEntryForTest(t *testing.T, path, runID string, at, until time.Time) sandboxNonce {
	t.Helper()
	e := runEntry("dev", runID)
	e.Starting = true
	e.StartingAt = &at
	e.StartingUntil = &until
	ss := newSandboxStateStore(path)
	_, n, err := ss.diskEntry("dev")
	if err != nil {
		t.Fatal(err)
	}
	nonce, err := ss.casPut("dev", n, e)
	if err != nil {
		t.Fatal(err)
	}
	return nonce
}

// goneBin returns a mock binary whose exec probe always answers "not
// found"; other verbs succeed, and wait blocks.
func goneBin(t *testing.T) string {
	t.Helper()
	return writeMockBin(t, `[ "$1" = wait ] && exec sleep 60
if [ "$1" = exec ]; then echo "sandbox not found" >&2; exit 1; fi
exit 0`)
}

// reconcileNow constructs another store on path and reconciles it with
// bin, as a new runtime instance does.
func reconcileNow(path, bin string) {
	newSandboxStateStore(path).reconcile(bin)
}

// -----------------------------------------------------------------------
// The Starting entry
// -----------------------------------------------------------------------

// Fault check S2: the Starting entry is written after `sandbox run`.
// The copy of the state file taken when `run` is invoked holds this run's
// full Starting entry, with StartingUntil = StartingAt + grace (sync); the
// promoted entry is no longer Starting.
func TestCloudRunSandboxStarting_WrittenBeforeRun(t *testing.T) {
	rt, env := newStartingRuntime(t, startScripts{run: copyState})
	cfg := startingCfg(t, "run-a")
	// An earlier run's entrypoint output in the agent's /scion home, so the
	// log offset is not 0.
	earlier := []byte("earlier run's output\n")
	home := filepath.Join(rt.rootDir, "agents", "dev", "home")
	if err := os.MkdirAll(home, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, entrypointLogFile), earlier, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run = %v, want success", err)
	}
	copied, err := readSandboxStateFile(env.copy)
	if err != nil {
		t.Fatalf("read the copy taken at run: %v", err)
	}
	e := copied["dev"]
	if e == nil || !e.Starting {
		t.Fatalf("state at run = %+v, want this run's Starting entry", e)
	}
	if e.Labels[api.LabelRunID] != "run-a" || e.AgentID != "dev" || e.Project != "proj" || e.ProjectID != "pid-1" ||
		e.Template != "tmpl" || e.HarnessConfig != "hc" || e.Image != "img" ||
		e.Labels[projectkeys.LabelProjectID] != "pid-1" || e.Labels["scion.name"] != "dev" ||
		e.AgentHome == "" || e.Workspace == "" || e.Nonce == "" || e.EntrypointLogOffset != int64(len(earlier)) {
		t.Errorf("Starting entry is not the full entry: %+v", e)
	}
	if e.StartingAt == nil || e.StartingUntil == nil || !e.StartingUntil.Equal(e.StartingAt.Add(sandboxStartingGrace)) {
		t.Errorf("StartingAt/StartingUntil = %v/%v, want StartingUntil = StartingAt + %v", e.StartingAt, e.StartingUntil, sandboxStartingGrace)
	}
	final := fileEntry(t, env.state)
	if final == nil || final.Starting || final.StartingAt != nil || final.StartingUntil != nil || final.Labels[api.LabelRunID] != "run-a" {
		t.Errorf("entry after the start = %+v, want run A's promoted entry", final)
	}
}

// Fault check S2b: a failed Starting write is ignored.
// The state file is corrupt when the Starting entry is written: Run fails,
// `run` is never invoked, and the file is not overwritten.
func TestCloudRunSandboxStarting_WriteFailureNoRun(t *testing.T) {
	rt, env := newStartingRuntime(t, startScripts{})
	corrupt := []byte(`{"dev": `)
	setHook(t, &sandboxBeforeStartingWriteHook, func(string) {
		if err := os.WriteFile(env.state, corrupt, 0600); err != nil {
			t.Errorf("corrupt state: %v", err)
		}
	})
	_, err := rt.Run(context.Background(), startingCfg(t, "run-a"))
	if err == nil {
		t.Fatal("Run succeeded, want the state write error")
	}
	if r := sandboxRunCalls(t, env.log); len(r) != 0 {
		t.Errorf("`run` invoked %d times after a failed Starting write, want 0", len(r))
	}
	if got, _ := os.ReadFile(env.state); !bytes.Equal(got, corrupt) {
		t.Errorf("corrupt state file overwritten: %q", got)
	}
}

// -----------------------------------------------------------------------
// Self-failed and never-started runs: no delete
// -----------------------------------------------------------------------

// Fault check S8: a self-failed `run` deletes by name, or keeps its entry.
// No other entry exists; `run` exits 1 with "already exists" (sync): no
// delete, no entry, and the run error text is unchanged.
func TestCloudRunSandboxStarting_SelfFailedNoDelete(t *testing.T) {
	rt, env := newStartingRuntime(t, startScripts{run: `echo "sandbox dev already exists" >&2; exit 1`})
	_, err := rt.Run(context.Background(), startingCfg(t, "run-a"))
	if err == nil || !strings.HasPrefix(err.Error(), "cloudrun-sandbox: run failed: ") || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("Run = %v, want the unchanged run-failed error", err)
	}
	wantDeletes(t, env.log, 0)
	if e := fileEntry(t, env.state); e != nil {
		t.Errorf("entry after a self-failed run = %+v, want none", e)
	}
}

// Fault check S8u (lead's addition): a self-failed `run` whose entry cannot be
// removed (state file unreadable) deletes by name.
// `run` corrupts the state file, then exits 1 on its own: no delete, and
// the file is left as it is.
func TestCloudRunSandboxStarting_SelfFailedUnreadableNoDelete(t *testing.T) {
	rt, env := newStartingRuntime(t, startScripts{run: `printf '{"dev": ' > "$STATE"; echo "sandbox dev already exists" >&2; exit 1`})
	if _, err := rt.Run(context.Background(), startingCfg(t, "run-a")); err == nil {
		t.Fatal("Run succeeded, want the self-failed run's error")
	}
	wantDeletes(t, env.log, 0)
	if got, _ := os.ReadFile(env.state); string(got) != `{"dev": ` {
		t.Errorf("state file = %q, want it left corrupt", got)
	}
}

// Fault check S8a: self-failed is classified by cancellation instead of the
// exit status. `run` exits 1 on its own; a hook after the command returns
// and before classification cancels ctx: no delete, entry removed.
func TestCloudRunSandboxStarting_SelfExitThenCancel(t *testing.T) {
	rt, env := newStartingRuntime(t, startScripts{run: `echo "boom" >&2; exit 1`})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	setHook(t, &sandboxLaunchReturnedHook, func(string) { cancel() })
	if _, err := rt.Run(ctx, startingCfg(t, "run-a")); err == nil {
		t.Fatal("Run succeeded, want the self-failed run's error")
	}
	wantDeletes(t, env.log, 0)
	if e := fileEntry(t, env.state); e != nil {
		t.Errorf("entry = %+v, want removed", e)
	}
}

// Fault check S8e: never-started is classified as "may exist".
// The binary is missing when `run` is started (Start fails); it appears
// right after, so any cleanup delete would be logged: no delete, no entry.
func TestCloudRunSandboxStarting_NeverStartedNoDelete(t *testing.T) {
	rt, env := newStartingRuntime(t, startScripts{})
	logging := rt.bin
	missing := filepath.Join(t.TempDir(), "sandbox")
	rt.bin = missing
	setHook(t, &sandboxLaunchReturnedHook, func(string) {
		data, err := os.ReadFile(logging)
		if err == nil {
			err = os.WriteFile(missing, data, 0755)
		}
		if err != nil {
			t.Errorf("install the binary: %v", err)
		}
	})
	_, err := rt.Run(context.Background(), startingCfg(t, "run-a"))
	if !errors.Is(err, fs.ErrNotExist) || !strings.HasPrefix(err.Error(), "cloudrun-sandbox: run failed: ") {
		t.Fatalf("Run = %v, want the run-failed error wrapping not-exist", err)
	}
	wantDeletes(t, env.log, 0)
	if e := fileEntry(t, env.state); e != nil {
		t.Errorf("entry = %+v, want none", e)
	}
}

// Fault check S8d: the Starting write discards a live entry it observed.
// (a) A live unlabelled entry: refused, no `run`, entry intact. (b) A live
// entry of this run, then `run` exits 1 on its own: that entry is restored
// with its original nonce, and no delete.
func TestCloudRunSandboxStarting_ObservedLiveEntry(t *testing.T) {
	t.Run("live unlabelled entry: refused", func(t *testing.T) {
		rt, env := newStartingRuntime(t, startScripts{})
		rt.state.add(runEntry("dev", ""))
		before := fileEntry(t, env.state)
		_, err := rt.Run(context.Background(), startingCfg(t, "run-a"))
		if !errors.Is(err, ErrRunConflict) {
			t.Fatalf("Run = %v, want ErrRunConflict", err)
		}
		if r := sandboxRunCalls(t, env.log); len(r) != 0 {
			t.Errorf("`run` invoked %d times, want 0", len(r))
		}
		if after := fileEntry(t, env.state); after == nil || after.Nonce != before.Nonce {
			t.Errorf("entry = %+v, want the unlabelled entry untouched", after)
		}
	})
	t.Run("live entry of this run: restored after a self-exit", func(t *testing.T) {
		rt, env := newStartingRuntime(t, startScripts{run: `echo "boom" >&2; exit 1`})
		rt.state.add(runEntry("dev", "run-a"))
		before := fileEntry(t, env.state)
		if _, err := rt.Run(context.Background(), startingCfg(t, "run-a")); err == nil {
			t.Fatal("Run succeeded, want the self-failed run's error")
		}
		wantDeletes(t, env.log, 0)
		after := fileEntry(t, env.state)
		if after == nil || after.Nonce != before.Nonce || after.Starting || after.Labels[api.LabelRunID] != "run-a" ||
			!after.CreatedAt.Equal(before.CreatedAt) {
			t.Errorf("entry = %+v, want the prior entry restored with nonce %q", after, before.Nonce)
		}
	})
}

// -----------------------------------------------------------------------
// Killed run, cancelled probe, dead on arrival: forced delete
// -----------------------------------------------------------------------

// Fault check S8b: the sync cleanup is skipped for a killed run or a cancelled
// probe. Either way (sync): `delete --force dev` is issued, no entry is
// left, and a cancelled probe still returns the bare ctx error.
func TestCloudRunSandboxStarting_SyncCancelForcedDelete(t *testing.T) {
	for _, tc := range []struct {
		name string
		sc   startScripts
	}{
		{"run killed", startScripts{run: blockUntilKilled}},
		{"probe cancelled", startScripts{exec: blockUntilKilled}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt, env := newStartingRuntime(t, tc.sc)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			errc := runSandboxAsync(ctx, rt, startingCfg(t, "run-a"))
			waitForFile(t, env.mark)
			cancel()
			err := recvErr(t, errc, "Run")
			if tc.sc.exec != "" {
				// The sync error text is unchanged: the bare ctx error.
				if !errors.Is(err, context.Canceled) || err.Error() != context.Canceled.Error() {
					t.Errorf("Run = %v, want bare context.Canceled", err)
				}
			} else if err == nil || !strings.HasPrefix(err.Error(), "cloudrun-sandbox: run failed: ") {
				t.Errorf("Run = %v, want the run-failed error", err)
			}
			wantDeletes(t, env.log, 1)
			if e := fileEntry(t, env.state); e != nil {
				t.Errorf("entry = %+v, want none", e)
			}
		})
	}
}

// Fault check S8f: the forced delete is skipped because the async hooks are
// set. OnResourceCreated is a recorder and a blocking `run` is killed by
// ctx: `delete --force` is still issued and no entry is left.
func TestCloudRunSandboxStarting_ForcedDeleteIgnoresHooks(t *testing.T) {
	rt, env := newStartingRuntime(t, startScripts{run: blockUntilKilled})
	var reported atomic.Int32
	cfg := startingCfg(t, "run-a")
	cfg.OnResourceCreated = func(api.ResourceHandle) { reported.Add(1) }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := runSandboxAsync(ctx, rt, cfg)
	waitForFile(t, env.mark)
	cancel()
	if err := recvErr(t, errc, "Run"); err == nil {
		t.Fatal("Run succeeded, want the killed run's error")
	}
	wantDeletes(t, env.log, 1)
	if e := fileEntry(t, env.state); e != nil {
		t.Errorf("entry = %+v, want none", e)
	}
	// CR-1 reports no handle; ptone/scion#4758 adds the report.
	t.Logf("handles reported: %d", reported.Load())
}

// Fault check S8c: the sync cleanup deletes when the key holds another nonce.
// During the probe another writer rewrites the key with run B's entry,
// then the probe is cancelled: no delete, and B's entry stays.
func TestCloudRunSandboxStarting_CleanupSkipsOtherNonce(t *testing.T) {
	rt, env := newStartingRuntime(t, startScripts{exec: blockUntilKilled})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := runSandboxAsync(ctx, rt, startingCfg(t, "run-a"))
	waitForFile(t, env.mark)
	w := newSandboxStateStore(env.state)
	if _, err := w.casPut("dev", nonceOf(w.get("dev")), runEntry("dev", "run-b")); err != nil {
		t.Fatalf("write B: %v", err)
	}
	cancel()
	if err := recvErr(t, errc, "Run"); err == nil {
		t.Fatal("Run succeeded, want the cancelled probe's error")
	}
	wantDeletes(t, env.log, 0)
	if e := fileEntry(t, env.state); e == nil || e.Labels[api.LabelRunID] != "run-b" {
		t.Errorf("entry = %+v, want run B's", e)
	}
}

// -----------------------------------------------------------------------
// List and pre-clean
// -----------------------------------------------------------------------

// Fault check S9: List reports a Starting entry as `running`.
// A crash-left Starting entry is listed as `starting`; a start of run B
// pre-cleans it (one delete), then starts.
func TestCloudRunSandboxStarting_CrashLeftListedAndPreCleaned(t *testing.T) {
	left := crashLeftState(t, startingCfg(t, "run-a"))
	base, env := newStartingRuntime(t, startScripts{})
	rt := installState(t, base, left)

	agents, err := rt.List(context.Background(), map[string]string{"scion.name": "dev"})
	if err != nil || len(agents) != 1 || agents[0].Phase != "starting" || agents[0].ContainerStatus != "starting" || agents[0].RunID != "run-a" {
		t.Fatalf("List = %+v, %v, want run A's entry as starting", agents, err)
	}
	if running, err := preCleanForTest(context.Background(), rt, false, "proj", "pid-1"); err != nil || running {
		t.Fatalf("pre-clean = running %v, %v; want the crash-left entry deleted", running, err)
	}
	wantDeletes(t, env.log, 1)
	if _, err := rt.Run(context.Background(), startingCfg(t, "run-b")); err != nil {
		t.Fatalf("Run B after pre-clean = %v, want success", err)
	}
	if e := fileEntry(t, env.state); e == nil || e.Starting || e.Labels[api.LabelRunID] != "run-b" {
		t.Errorf("entry = %+v, want run B's promoted entry", e)
	}
}

// Fault check S9c: the Starting entry lacks the project fields.
// A crash-left Starting entry with project labels is listed with its
// project and matched by pre-clean's project filter, which deletes it.
func TestCloudRunSandboxStarting_CrashLeftCarriesProjectFields(t *testing.T) {
	left := crashLeftState(t, startingCfg(t, "run-a"))
	base, env := newStartingRuntime(t, startScripts{})
	rt := installState(t, base, left)

	agents, err := rt.List(context.Background(), map[string]string{"scion.name": "dev"})
	if err != nil || len(agents) != 1 {
		t.Fatalf("List = %+v, %v, want run A's entry", agents, err)
	}
	a := agents[0]
	if a.Project != "proj" || a.ProjectID != "pid-1" ||
		projectkeys.ProjectIDFromLabels(a.Labels) != "pid-1" || projectkeys.ProjectNameFromLabels(a.Labels) != "proj" {
		t.Errorf("listed project fields = %q/%q, labels %v; want proj/pid-1", a.Project, a.ProjectID, a.Labels)
	}
	if !matchProjectForTest(a, "proj", "pid-1") || matchProjectForTest(a, "other", "pid-2") {
		t.Errorf("project filter does not match the crash-left entry exactly: %+v", a)
	}
	if _, err := preCleanForTest(context.Background(), rt, false, "proj", "pid-1"); err != nil {
		t.Fatalf("pre-clean: %v", err)
	}
	wantDeletes(t, env.log, 1)
}

// Fault check S9b: an in-flight Starting entry is visible to List.
// Start A blocks in `run`; a concurrent start B pre-cleans and runs: B's
// pre-clean issues no delete, B is refused once A is through, and A's
// sandbox is untouched (one `run`, A's entry promoted).
func TestCloudRunSandboxStarting_InFlightHiddenFromConcurrentStart(t *testing.T) {
	rt, env := newStartingRuntime(t, startScripts{run: blockUntilReleased})
	errA := runSandboxAsync(context.Background(), rt, startingCfg(t, "run-a"))
	waitForFile(t, env.mark)

	cfgB := startingCfg(t, "run-b")
	errB := make(chan error, 1)
	go func() {
		if _, err := preCleanForTest(context.Background(), rt, false, "proj", "pid-1"); err != nil {
			errB <- err
			return
		}
		_, err := rt.Run(context.Background(), cfgB)
		errB <- err
	}()
	time.Sleep(300 * time.Millisecond)
	touch(t, env.release)

	if err := recvErr(t, errA, "Run A"); err != nil {
		t.Fatalf("Run A = %v, want success", err)
	}
	if err := recvErr(t, errB, "start B"); !errors.Is(err, ErrRunConflict) {
		t.Errorf("start B = %v, want ErrRunConflict", err)
	}
	wantDeletes(t, env.log, 0)
	if r := sandboxRunCalls(t, env.log); len(r) != 1 {
		t.Errorf("%d `run` calls, want 1 (A's)", len(r))
	}
	if e := fileEntry(t, env.state); e == nil || e.Starting || e.Labels[api.LabelRunID] != "run-a" {
		t.Errorf("entry = %+v, want run A's promoted entry", e)
	}
}

// Fault check S10f: List reports a stale cached Starting entry wrongly.
// (a) Instance Y caches Starting{A}, then A is promoted in the file: Y
// lists it as running, and a no-task pre-clean through Y issues no delete.
// (b) Y caches Starting{A} (not in flight), and the file now holds the
// Starting entry of B, a start in flight: Y omits it, and a concurrent
// pre-clean through Y issues no delete.
func TestCloudRunSandboxStarting_ListRereadsStaleCache(t *testing.T) {
	t.Run("promoted in the file: running", func(t *testing.T) {
		base, env := newStartingRuntime(t, startScripts{})
		now := time.Now()
		writeStartingEntryForTest(t, env.state, "run-a", now, now.Add(time.Hour))
		y := siblingRuntime(t, base, "")
		if e := y.state.get("dev"); e == nil || !e.Starting {
			t.Fatalf("Y's cache = %+v, want Starting{A}", e)
		}
		w := newSandboxStateStore(env.state)
		promoted := runEntry("dev", "run-a")
		if _, err := w.casPut("dev", nonceOf(w.get("dev")), promoted); err != nil {
			t.Fatal(err)
		}

		agents, err := y.List(context.Background(), map[string]string{"scion.name": "dev"})
		if err != nil || len(agents) != 1 || agents[0].Phase != "running" {
			t.Fatalf("Y.List = %+v, %v, want A as running", agents, err)
		}
		if running, err := preCleanForTest(context.Background(), y, false, "", ""); err != nil || !running {
			t.Errorf("pre-clean through Y = running %v, %v; want the running entry returned", running, err)
		}
		wantDeletes(t, env.log, 0)
	})
	t.Run("file holds an in-flight start: omitted", func(t *testing.T) {
		rt, env := newStartingRuntime(t, startScripts{run: blockUntilReleased})
		now := time.Now()
		writeStartingEntryForTest(t, env.state, "run-a", now, now.Add(time.Hour))
		y := siblingRuntime(t, rt, "")
		newSandboxStateStore(env.state).remove("dev")

		errB := runSandboxAsync(context.Background(), rt, startingCfg(t, "run-b"))
		waitForFile(t, env.mark)

		agents, err := y.List(context.Background(), map[string]string{"scion.name": "dev"})
		if err != nil || len(agents) != 0 {
			t.Errorf("Y.List = %+v, %v, want nothing (B is in flight)", agents, err)
		}
		pre := make(chan error, 1)
		go func() {
			_, err := preCleanForTest(context.Background(), y, false, "", "")
			pre <- err
		}()
		time.Sleep(300 * time.Millisecond)
		touch(t, env.release)
		if err := recvErr(t, errB, "Run B"); err != nil {
			t.Fatalf("Run B = %v, want success", err)
		}
		if err := recvErr(t, pre, "pre-clean"); err != nil {
			t.Errorf("pre-clean through Y = %v", err)
		}
		wantDeletes(t, env.log, 0)
		if e := fileEntry(t, env.state); e == nil || e.Starting || e.Labels[api.LabelRunID] != "run-b" {
			t.Errorf("entry = %+v, want run B's promoted entry", e)
		}
	})
}

// -----------------------------------------------------------------------
// Probe-success promotion and the launch output
// -----------------------------------------------------------------------

// Fault check S10c: probe success flips a flag instead of upserting.
// Another instance removes the Starting entry mid-start: afterwards the
// entry exists, no longer Starting.
func TestCloudRunSandboxStarting_RemovedMidStartRecreated(t *testing.T) {
	rt, env := newStartingRuntime(t, startScripts{})
	setAfterLaunchHook(t, func(string) {
		w := newSandboxStateStore(env.state)
		if err := w.casRemove("dev", nonceOf(w.get("dev"))); err != nil {
			t.Errorf("remove: %v", err)
		}
	})
	if _, err := rt.Run(context.Background(), startingCfg(t, "run-a")); err != nil {
		t.Fatalf("Run = %v, want success", err)
	}
	if e := fileEntry(t, env.state); e == nil || e.Starting || e.Labels[api.LabelRunID] != "run-a" {
		t.Errorf("entry = %+v, want run A's entry re-created, not Starting", e)
	}
	wantDeletes(t, env.log, 0)
}

// Fault check S10d (amended with the lead's approval, see ptone/scion#4757):
// the launch output is captured through a pipe, with or without a
// WaitDelay, instead of the unlinked file. `run` exits 0 leaving a
// descendant that holds its output and writes to it 6 s later; the probe
// passes. The start succeeds well before the write (no wait on the held
// output), with no delete, and the descendant's late write succeeds (it
// stays alive: a pipe whose read end was closed would kill it with
// SIGPIPE, or fail the write).
func TestCloudRunSandboxStarting_LaunchOutputHolderSurvives(t *testing.T) {
	rt, env := newStartingRuntime(t, startScripts{run: `(sleep 6; echo "late output" && touch "$MARK") & exit 0`})
	begin := time.Now()
	errc := runSandboxAsync(context.Background(), rt, startingCfg(t, "run-a"))
	var err error
	select {
	case err = <-errc:
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not return within 20 s")
	}
	elapsed := time.Since(begin)
	if err != nil {
		t.Fatalf("Run = %v, want success", err)
	}
	if elapsed > 4*time.Second {
		t.Errorf("Run took %v, want well under the descendant's 6 s: it waited on the held output", elapsed)
	}
	wantDeletes(t, env.log, 0)
	if e := fileEntry(t, env.state); e == nil || e.Starting {
		t.Errorf("entry = %+v, want the promoted entry", e)
	}

	// The descendant writes at about 6 s; allow up to 15 s more.
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(env.mark); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the descendant holding the launch output did not survive its late write")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Fault check R1 (CR-1 review r3): when the output file cannot be created, the
// launch falls back to a pipe. With no temp dir, `run` exits 0 leaving a
// descendant that holds its output for 8 s: the start still returns at
// once, with success (stdio is /dev/null).
func TestCloudRunSandboxStarting_LaunchOutputFallbackNoPipe(t *testing.T) {
	rt, env := newStartingRuntime(t, startScripts{run: `sleep 8 & echo $! > "$PIDFILE"; exit 0`})
	cfg := startingCfg(t, "run-a")
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing")) // newSandboxCommandOutput fails
	begin := time.Now()
	errc := runSandboxAsync(context.Background(), rt, cfg)
	var err error
	select {
	case err = <-errc:
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not return within 20 s")
	}
	if err != nil {
		t.Fatalf("Run = %v, want success", err)
	}
	if d := time.Since(begin); d > 4*time.Second {
		t.Errorf("Run took %v, want well under the descendant's 8 s: the fallback waited on a held pipe", d)
	}
	wantDeletes(t, env.log, 0)
}

// Fault check O1 (CR-1 review r3): the launch output is shortened at
// 64 KiB inside an environment value (or read back only up to 64 KiB).
// `run` prints output whose 64 KiB point falls inside an env value, then
// fails: the error holds no part of the value, and is marked truncated.
func TestCloudRunSandboxStarting_LaunchOutputShortenedOutsideValue(t *testing.T) {
	const value = "v4lu3-OVERLAP-0123456789abcdef"
	pad := maxSandboxCommandOutput - 10 // the cut falls 10 bytes into the value
	rt, _ := newStartingRuntime(t, startScripts{
		run: fmt.Sprintf(`head -c %d /dev/zero | tr '\0' x; printf '%%s tail' %q; exit 1`, pad, value),
	})
	cfg := startingCfg(t, "run-a")
	cfg.Env = append(cfg.Env, "API_TOKEN="+value)
	_, err := rt.Run(context.Background(), cfg)
	if err == nil {
		t.Fatal("Run succeeded, want the self-failed run's error")
	}
	got := err.Error()
	if strings.Contains(got, value[:10]) {
		t.Errorf("run error holds part of the env value (%q)", value[:10])
	}
	if !strings.Contains(got, strings.Repeat("x", 64)) || !strings.HasSuffix(got, "...(truncated))") {
		t.Errorf("run error does not hold the output cut at %d bytes; tail: %q", maxSandboxCommandOutput, got[max(0, len(got)-80):])
	}
}

// Fault checks R2 (CR-1 review r4): (a) a 192 KiB read, processed, then
// shortened at 64 KiB; (b) raw output cut at 64 KiB
// without moving back off a spanning value. `run` echoes its argv, which
// carries two external env values of about 100 KB each, then fails: the
// error holds no part of either value.
func TestCloudRunSandboxStarting_LaunchOutputLargeValuesShortened(t *testing.T) {
	bigA := strings.Repeat("a1b2c3d4", 12500)
	bigB := strings.Repeat("q9w8e7r6", 12500)
	rt, _ := newStartingRuntime(t, startScripts{run: `printf '%s ' "$@"; exit 1`})
	cfg := startingCfg(t, "run-a")
	cfg.Env = append(cfg.Env, "BIG_A="+bigA, "BIG_B="+bigB)
	_, err := rt.Run(context.Background(), cfg)
	if err == nil {
		t.Fatal("Run succeeded, want the self-failed run's error")
	}
	got := err.Error()
	for name, v := range map[string]string{"BIG_A": bigA, "BIG_B": bigB} {
		if strings.Contains(got, v[:32]) {
			t.Errorf("run error holds part of %s's value", name)
		}
	}
	if !strings.HasSuffix(got, "...(truncated))") {
		t.Errorf("run error is not marked truncated; tail: %q", got[max(0, len(got)-80):])
	}
}

// classifySandboxLaunch, end to end through runSandboxLaunch.
func TestClassifySandboxLaunch(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	for _, tc := range []struct {
		name   string
		bin    string
		script string
		want   sandboxLaunchOutcome
	}{
		{"exit 0", "", "exit 0", sandboxLaunchOK},
		{"exit 1", "", "exit 1", sandboxLaunchExitedOnItsOwn},
		{"killed by a signal", "", "kill -9 $$", sandboxLaunchMayExist},
		{"binary missing", missing, "", sandboxLaunchNeverStarted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := tc.bin
			if bin == "" {
				bin = writeMockBin(t, tc.script)
			}
			_, started, err := runSandboxLaunch(context.Background(), bin, "run", "dev")
			if got := classifySandboxLaunch(started, err); got != tc.want {
				t.Errorf("outcome = %d, want %d (err %v)", got, tc.want, err)
			}
		})
	}
}

// -----------------------------------------------------------------------
// Reconcile and the cleanup horizon
// -----------------------------------------------------------------------

// Fault check S10: reconcile probes a young Starting entry.
// With a not-found probe, a young entry is kept; an old one (StartingAt +
// grace and StartingUntil both past) is removed.
func TestSandboxReconcile_StartingYoungKeptOldRemoved(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name      string
		at, until time.Time
		kept      bool
	}{
		{"young", now, now.Add(time.Minute), true},
		{"old", now.Add(-time.Hour), now.Add(-time.Minute), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			writeStartingEntryForTest(t, path, "run-x", tc.at, tc.until)
			reconcileNow(path, goneBin(t))
			if got := readStateForTest(t, path)["dev"] != nil; got != tc.kept {
				t.Errorf("entry kept = %v, want %v", got, tc.kept)
			}
		})
	}
}

// Fault check S10b: reconcile adopts an old, live Starting entry.
// An old Starting entry whose probe passes stays Starting, unchanged.
func TestSandboxReconcile_StartingOldLiveNotAdopted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	now := time.Now()
	nonce := writeStartingEntryForTest(t, path, "run-x", now.Add(-time.Hour), now.Add(-time.Minute))
	reconcileNow(path, writeMockBin(t, `[ "$1" = wait ] && exec sleep 60
exit 0`))
	e := readStateForTest(t, path)["dev"]
	if e == nil || !e.Starting || nonceOf(e) != nonce {
		t.Errorf("entry = %+v, want it still Starting with nonce %s", e, nonce)
	}
}

// Fault check S10e: reconcile removes an in-flight Starting entry.
// In flight: grace and horizon terms are ms and the start's ctx has no
// deadline, so only the in-flight rule keeps the entry. A blocking `run`
// is inside the start; another instance reconciles after the margins pass
// with a not-found probe: the entry survives. Then `run` is killed:
// `delete --force` is issued. Another process: an entry written directly
// to the file, not in flight, with StartingAt older than the grace and
// StartingUntil in the future, is kept.
func TestSandboxReconcile_StartingInFlightKept(t *testing.T) {
	t.Run("in flight", func(t *testing.T) {
		setDuration(t, &sandboxStartingGrace, time.Millisecond)
		setDuration(t, &asyncLaunchAdmissionSkew, time.Millisecond)
		setDuration(t, &asyncLaunchReportTTL, time.Millisecond)
		setDuration(t, &asyncLaunchCleanupBudget, time.Millisecond)
		setDuration(t, &asyncCleanupSlack, time.Millisecond)

		rt, env := newStartingRuntime(t, startScripts{run: blockUntilKilled})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		errc := runSandboxAsync(ctx, rt, startingCfg(t, "run-a"))
		waitForFile(t, env.mark)
		time.Sleep(50 * time.Millisecond) // past StartingAt + grace and StartingUntil

		reconcileNow(env.state, goneBin(t))
		if e := fileEntry(t, env.state); e == nil || !e.Starting || e.Labels[api.LabelRunID] != "run-a" {
			t.Errorf("entry after reconcile = %+v, want run A's in-flight Starting entry kept", e)
		}

		cancel()
		if err := recvErr(t, errc, "Run"); err == nil {
			t.Fatal("Run succeeded, want the killed run's error")
		}
		wantDeletes(t, env.log, 1)
	})
	t.Run("another process", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "state.json")
		now := time.Now()
		writeStartingEntryForTest(t, path, "run-x", now.Add(-time.Hour), now.Add(time.Hour))
		reconcileNow(path, goneBin(t))
		if readStateForTest(t, path)["dev"] == nil {
			t.Error("entry before its StartingUntil removed, want kept")
		}
	})
}

// Fault check S10g: StartingUntil ignores a term of the horizon.
// Grace is ms and each horizon term at least 100 ms, all distinct. A start
// with ctx deadline D: (i) the copy taken at `run` has StartingUntil =
// D + skew + report window + cleanup budget + slack; (ii) `run` is killed
// at D and the delete fails with another error, so the entry is kept; a
// reconcile at StartingUntil - 100 ms with a not-found probe keeps it, and
// one at StartingUntil + 100 ms removes it.
func TestCloudRunSandboxStarting_StartingUntilHorizon(t *testing.T) {
	setDuration(t, &sandboxStartingGrace, time.Millisecond)
	setDuration(t, &asyncLaunchAdmissionSkew, 100*time.Millisecond)
	setDuration(t, &asyncLaunchReportTTL, 200*time.Millisecond)
	setDuration(t, &asyncLaunchCleanupBudget, 300*time.Millisecond)
	setDuration(t, &asyncCleanupSlack, 400*time.Millisecond)

	rt, env := newStartingRuntime(t, startScripts{
		run: copyState + `; exec sleep 60`,
		del: `echo "permission denied" >&2; exit 1`,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	d, _ := ctx.Deadline()
	if _, err := rt.Run(ctx, startingCfg(t, "run-a")); err == nil {
		t.Fatal("Run succeeded, want the killed run's error")
	}

	copied, err := readSandboxStateFile(env.copy)
	if err != nil {
		t.Fatal(err)
	}
	want := d.Add(1000 * time.Millisecond)
	e := copied["dev"]
	if e == nil || e.StartingUntil == nil || !e.StartingUntil.Equal(want) {
		t.Fatalf("StartingUntil at run = %+v, want D + 1000ms = %v", e, want)
	}

	wantDeletes(t, env.log, 1)
	kept := fileEntry(t, env.state)
	if kept == nil || !kept.Starting {
		t.Fatalf("entry after a failed cleanup delete = %+v, want it kept", kept)
	}

	gone := goneBin(t)
	setNow(t, want.Add(-100*time.Millisecond))
	reconcileNow(env.state, gone)
	if fileEntry(t, env.state) == nil {
		t.Error("reconcile at StartingUntil - 100ms removed the entry, want kept")
	}
	setNow(t, want.Add(100*time.Millisecond))
	reconcileNow(env.state, gone)
	if e := fileEntry(t, env.state); e != nil {
		t.Errorf("reconcile at StartingUntil + 100ms kept %+v, want removed", e)
	}
}

// AsyncCleanupHorizon is the broker's deadline arithmetic.
func TestAsyncCleanupHorizon(t *testing.T) {
	d := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	want := d.Add(20*time.Second + 10*time.Minute + 60*time.Second + 60*time.Second)
	if got := AsyncCleanupHorizon(d); !got.Equal(want) {
		t.Errorf("AsyncCleanupHorizon(%v) = %v, want %v", d, got, want)
	}
}
