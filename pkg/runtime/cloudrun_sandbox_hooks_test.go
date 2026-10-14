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
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// CR-2 (ptone/scion#4758): the sandbox runtime's async launch hooks (the
// sandbox_layout and launching checkpoints and the sandbox handle), the
// hooks-active rule (Run deletes nothing on a failure path) and
// DeleteResource. Each test names the fault check (a deliberate production
// change) it must catch, by the issue's ID.

// -----------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------

// errTestAbort is what a recorder's checkpoint returns at its abort step.
var errTestAbort = errors.New("launch aborted by the test")

// sandboxHookRecorder records an async launch's checkpoints and handles, and
// aborts at abortAt (if set).
type sandboxHookRecorder struct {
	abortAt string

	mu      sync.Mutex
	steps   []string
	handles []api.ResourceHandle
}

// install sets cfg's async hooks to the recorder.
func (h *sandboxHookRecorder) install(cfg *RunConfig) {
	cfg.Checkpoint = func(_ context.Context, step string) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.steps = append(h.steps, step)
		if step == h.abortAt {
			return errTestAbort
		}
		return nil
	}
	cfg.OnResourceCreated = func(handle api.ResourceHandle) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.handles = append(h.handles, handle)
	}
}

func (h *sandboxHookRecorder) gotSteps() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.steps...)
}

func (h *sandboxHookRecorder) gotHandles() []api.ResourceHandle {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]api.ResourceHandle(nil), h.handles...)
}

// hooksCfg is startingCfg for runID with rec's hooks installed.
func hooksCfg(t *testing.T, runID string, rec *sandboxHookRecorder) RunConfig {
	t.Helper()
	cfg := startingCfg(t, runID)
	rec.install(&cfg)
	return cfg
}

// sandboxHandle is the handle a start of runID reports for "dev".
func sandboxHandle(runID string) api.ResourceHandle {
	return api.ResourceHandle{Kind: api.ResourceKindSandbox, Name: "dev", UID: runID}
}

// wantOneHandle fails the test unless rec holds exactly the handle of
// runID.
func wantOneHandle(t *testing.T, rec *sandboxHookRecorder, runID string) {
	t.Helper()
	if got, want := rec.gotHandles(), []api.ResourceHandle{sandboxHandle(runID)}; !reflect.DeepEqual(got, want) {
		t.Errorf("handles = %+v, want %+v", got, want)
	}
}

// wantEntryOf fails the test unless the state file's "dev" entry belongs
// to runID, and returns it.
func wantEntryOf(t *testing.T, path, runID string) *sandboxStateEntry {
	t.Helper()
	e := fileEntry(t, path)
	if e == nil || e.Labels[api.LabelRunID] != runID {
		t.Fatalf("entry = %+v, want run %s's entry", e, runID)
	}
	return e
}

// cleanupNoDelete runs the launch cleanup for runID's handle and fails the
// test unless it returns nil without issuing a delete.
func cleanupNoDelete(t *testing.T, rt *CloudRunSandboxRuntime, env startEnv, runID string) {
	t.Helper()
	if err := rt.DeleteResource(context.Background(), sandboxHandle(runID)); err != nil {
		t.Errorf("DeleteResource = %v, want nil", err)
	}
	wantDeletes(t, env.log, 0)
}

// cleanupDeletes runs the launch cleanup for runID's handle and fails the
// test unless it returns nil after one `delete --force dev` and leaves no
// entry.
func cleanupDeletes(t *testing.T, rt *CloudRunSandboxRuntime, env startEnv, runID string) {
	t.Helper()
	if err := rt.DeleteResource(context.Background(), sandboxHandle(runID)); err != nil {
		t.Errorf("DeleteResource = %v, want nil", err)
	}
	wantDeletes(t, env.log, 1)
	if e := fileEntry(t, env.state); e != nil {
		t.Errorf("entry after the cleanup = %+v, want none", e)
	}
}

// -----------------------------------------------------------------------
// Checkpoints and the handle
// -----------------------------------------------------------------------

// Fault check S1: the launching checkpoint is dropped (S1b: the
// sandbox_layout checkpoint is dropped). An abort at either checkpoint
// launches nothing, writes no entry and reports no handle; the checkpoints
// run in order; an abort at sandbox_layout also leaves no layout.
func TestCloudRunSandboxHooks_CheckpointAbort(t *testing.T) {
	for _, tc := range []struct {
		step      string
		wantSteps []string
	}{
		{CheckpointStepLaunching, []string{CheckpointStepSandboxLayout, CheckpointStepLaunching}},
		{CheckpointStepSandboxLayout, []string{CheckpointStepSandboxLayout}},
	} {
		t.Run(tc.step, func(t *testing.T) {
			rt, env := newStartingRuntime(t, startScripts{})
			rec := &sandboxHookRecorder{abortAt: tc.step}
			_, err := rt.Run(context.Background(), hooksCfg(t, "run-a", rec))
			if !errors.Is(err, errTestAbort) {
				t.Fatalf("Run = %v, want the checkpoint's error", err)
			}
			if r := sandboxRunCalls(t, env.log); len(r) != 0 {
				t.Errorf("`run` invoked %d times, want 0", len(r))
			}
			wantDeletes(t, env.log, 0)
			if e := fileEntry(t, env.state); e != nil {
				t.Errorf("entry = %+v, want none", e)
			}
			if h := rec.gotHandles(); len(h) != 0 {
				t.Errorf("handles = %+v, want none", h)
			}
			if got := rec.gotSteps(); !reflect.DeepEqual(got, tc.wantSteps) {
				t.Errorf("checkpoints = %q, want %q", got, tc.wantSteps)
			}
			if tc.step == CheckpointStepSandboxLayout {
				if _, err := os.Stat(filepath.Join(rt.rootDir, "agents", "dev")); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("layout dir after an abort at %s: stat err = %v, want not exist", tc.step, err)
				}
			}
		})
	}
}

// A successful start with the hooks set passes both checkpoints in order,
// reports exactly one sandbox handle (UID = run ID) and deletes nothing.
func TestCloudRunSandboxHooks_SuccessReportsOneHandle(t *testing.T) {
	rt, env := newStartingRuntime(t, startScripts{})
	rec := &sandboxHookRecorder{}
	if _, err := rt.Run(context.Background(), hooksCfg(t, "run-a", rec)); err != nil {
		t.Fatalf("Run = %v, want success", err)
	}
	want := []string{CheckpointStepSandboxLayout, CheckpointStepLaunching}
	if got := rec.gotSteps(); !reflect.DeepEqual(got, want) {
		t.Errorf("checkpoints = %q, want %q", got, want)
	}
	wantOneHandle(t, rec, "run-a")
	wantDeletes(t, env.log, 0)
	if e := wantEntryOf(t, env.state, "run-a"); e.Starting {
		t.Errorf("entry = %+v, want it promoted", e)
	}
}

// Fault check S3: the handle is reported after `sandbox run` returns.
// With the hooks active, a blocking `run` killed by ctx, or a blocking
// probe cancelled by ctx: the handle {sandbox, dev, run-a} is already
// recorded when `run` is invoked (run killed) and exactly one is recorded;
// Run deletes nothing and the entry stays; the cleanup, through another
// runtime instance (as the broker's), then issues `delete --force dev`
// and removes the entry.
func TestCloudRunSandboxHooks_KilledRunHandleAndCleanup(t *testing.T) {
	for _, tc := range []struct {
		name string
		sc   startScripts
	}{
		{"run killed", startScripts{run: blockUntilKilled}},
		{"probe cancelled", startScripts{exec: blockUntilKilled}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt, env := newStartingRuntime(t, tc.sc)
			rec := &sandboxHookRecorder{}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			errc := runSandboxAsync(ctx, rt, hooksCfg(t, "run-a", rec))
			waitForFile(t, env.mark)
			if h := rec.gotHandles(); len(h) != 1 {
				t.Errorf("handles while %s blocks = %+v, want the one handle reported before `run`", tc.name, h)
			}
			cancel()
			if err := recvErr(t, errc, "Run"); err == nil {
				t.Fatal("Run succeeded, want the cancelled start's error")
			}
			wantOneHandle(t, rec, "run-a")
			wantDeletes(t, env.log, 0)
			if e := wantEntryOf(t, env.state, "run-a"); !e.Starting {
				t.Errorf("entry = %+v, want this start's Starting entry kept", e)
			}
			cleanupDeletes(t, siblingRuntime(t, rt, ""), env, "run-a")
		})
	}
}

// Fault check S7: the dead-on-arrival delete still runs with the hooks
// active. Every probe fails: Run returns the dead-on-arrival error,
// deletes nothing and keeps the entry; the cleanup then deletes the
// sandbox and removes the entry.
func TestCloudRunSandboxHooks_DeadOnArrivalNoDelete(t *testing.T) {
	rt, env := newStartingRuntime(t, startScripts{exec: `echo "control server unavailable" >&2; exit 1`})
	rec := &sandboxHookRecorder{}
	_, err := rt.Run(context.Background(), hooksCfg(t, "run-a", rec))
	if err == nil || !strings.Contains(err.Error(), "dead on arrival") {
		t.Fatalf("Run = %v, want the dead-on-arrival error", err)
	}
	wantOneHandle(t, rec, "run-a")
	wantDeletes(t, env.log, 0)
	wantEntryOf(t, env.state, "run-a")
	cleanupDeletes(t, rt, env, "run-a")
}

// Fault check S7b (review F3): the promote step's failStart ignores the
// hooks. With the hooks active, the probe passes and the state file is
// then made unreadable, so the promote write fails: Run returns an error
// and deletes nothing (without the hooks, failStart would delete by name).
func TestCloudRunSandboxHooks_PromoteFailureNoDelete(t *testing.T) {
	rt, env := newStartingRuntime(t, startScripts{})
	setHook(t, &sandboxAfterLaunchHook, func(string) {
		if err := os.WriteFile(env.state, []byte(`{"dev": `), 0600); err != nil {
			t.Errorf("corrupt the state file: %v", err)
		}
	})
	rec := &sandboxHookRecorder{}
	if _, err := rt.Run(context.Background(), hooksCfg(t, "run-a", rec)); err == nil {
		t.Fatal("Run succeeded, want the promote write's error")
	}
	wantOneHandle(t, rec, "run-a")
	wantDeletes(t, env.log, 0)
}

// -----------------------------------------------------------------------
// Self-failed and never-started runs with the hooks active
// -----------------------------------------------------------------------

// Fault check S8A (async half of S8): with the hooks active, `run` exits 1
// with "already exists" and no other entry exists: no delete, no entry;
// the cleanup returns nil with no delete.
func TestCloudRunSandboxHooks_SelfFailedNoDelete(t *testing.T) {
	rt, env := newStartingRuntime(t, startScripts{run: `echo "sandbox dev already exists" >&2; exit 1`})
	rec := &sandboxHookRecorder{}
	if _, err := rt.Run(context.Background(), hooksCfg(t, "run-a", rec)); err == nil {
		t.Fatal("Run succeeded, want the self-failed run's error")
	}
	wantOneHandle(t, rec, "run-a")
	wantDeletes(t, env.log, 0)
	if e := fileEntry(t, env.state); e != nil {
		t.Errorf("entry = %+v, want none", e)
	}
	cleanupNoDelete(t, rt, env, "run-a")
}

// Fault check S8aA (async half of S8a): with the hooks active, `run` exits
// 1 on its own, then a hook after the command returns cancels ctx: no
// delete, no entry; the cleanup returns nil with no delete.
func TestCloudRunSandboxHooks_SelfExitThenCancel(t *testing.T) {
	rt, env := newStartingRuntime(t, startScripts{run: `echo "boom" >&2; exit 1`})
	rec := &sandboxHookRecorder{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	setHook(t, &sandboxLaunchReturnedHook, func(string) { cancel() })
	if _, err := rt.Run(ctx, hooksCfg(t, "run-a", rec)); err == nil {
		t.Fatal("Run succeeded, want the self-failed run's error")
	}
	wantDeletes(t, env.log, 0)
	if e := fileEntry(t, env.state); e != nil {
		t.Errorf("entry = %+v, want removed", e)
	}
	cleanupNoDelete(t, rt, env, "run-a")
}

// Fault check S8dA (async half of S8d(b)): with the hooks active, a live
// entry of this run is restored (original nonce) after `run` exits 1 on
// its own, and Run deletes nothing; the cleanup then deletes that sandbox
// (same run) and removes the entry.
func TestCloudRunSandboxHooks_RestoredEntryCleanedUp(t *testing.T) {
	rt, env := newStartingRuntime(t, startScripts{run: `echo "boom" >&2; exit 1`})
	rt.state.add(runEntry("dev", "run-a"))
	before := fileEntry(t, env.state)
	rec := &sandboxHookRecorder{}
	if _, err := rt.Run(context.Background(), hooksCfg(t, "run-a", rec)); err == nil {
		t.Fatal("Run succeeded, want the self-failed run's error")
	}
	wantDeletes(t, env.log, 0)
	if after := wantEntryOf(t, env.state, "run-a"); after.Nonce != before.Nonce || after.Starting {
		t.Fatalf("entry = %+v, want the prior entry restored with nonce %q", after, before.Nonce)
	}
	cleanupDeletes(t, rt, env, "run-a")
}

// Fault check S8eA (async half of S8e): with the hooks active, the binary
// is missing when `run` is started; it appears right after, so a delete
// would be logged: no delete, no entry; the cleanup returns nil with no
// delete.
func TestCloudRunSandboxHooks_NeverStartedNoDelete(t *testing.T) {
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
	rec := &sandboxHookRecorder{}
	if _, err := rt.Run(context.Background(), hooksCfg(t, "run-a", rec)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Run = %v, want the run-failed error wrapping not-exist", err)
	}
	wantDeletes(t, env.log, 0)
	if e := fileEntry(t, env.state); e != nil {
		t.Errorf("entry = %+v, want none", e)
	}
	cleanupNoDelete(t, rt, env, "run-a")
}

// Fault check S10eA (async tail of S10e): grace and horizon terms are ms
// and ctx has no deadline, so only the in-flight rule keeps the entry. A
// blocking `run` with the hooks active; a reconcile with a not-found probe
// keeps the entry; then `run` is killed: Run deletes nothing, and the
// cleanup issues `delete --force dev` and removes the entry.
func TestCloudRunSandboxHooks_InFlightReconcileThenCleanup(t *testing.T) {
	setDuration(t, &sandboxStartingGrace, time.Millisecond)
	setDuration(t, &asyncLaunchAdmissionSkew, time.Millisecond)
	setDuration(t, &asyncLaunchReportTTL, time.Millisecond)
	setDuration(t, &asyncLaunchCleanupBudget, time.Millisecond)
	setDuration(t, &asyncCleanupSlack, time.Millisecond)

	rt, env := newStartingRuntime(t, startScripts{run: blockUntilKilled})
	rec := &sandboxHookRecorder{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := runSandboxAsync(ctx, rt, hooksCfg(t, "run-a", rec))
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
	wantDeletes(t, env.log, 0)
	cleanupDeletes(t, rt, env, "run-a")
}

// -----------------------------------------------------------------------
// DeleteResource
// -----------------------------------------------------------------------

// Fault check S4: the cleanup deletes when the run differs. The file holds
// run B's entry and the handle is run A's: nil, no delete, B intact.
func TestCloudRunSandboxDeleteResource_OtherRun(t *testing.T) {
	rt, env := newStartingRuntime(t, startScripts{})
	rt.state.add(runEntry("dev", "run-b"))
	before := fileEntry(t, env.state)
	cleanupNoDelete(t, rt, env, "run-a")
	if after := wantEntryOf(t, env.state, "run-b"); after.Nonce != before.Nonce {
		t.Errorf("entry = %+v, want run B's entry untouched", after)
	}
}

// Fault check S5: the cleanup deletes an unlabelled entry. nil, no delete,
// the entry intact.
func TestCloudRunSandboxDeleteResource_Unlabelled(t *testing.T) {
	rt, env := newStartingRuntime(t, startScripts{})
	rt.state.add(runEntry("dev", ""))
	before := fileEntry(t, env.state)
	cleanupNoDelete(t, rt, env, "run-a")
	if after := fileEntry(t, env.state); after == nil || after.Nonce != before.Nonce {
		t.Errorf("entry = %+v, want the unlabelled entry untouched", after)
	}
}

// Fault check S6: the cleanup runs without the name lock. While the lock
// is held, DeleteResource does not return within 200 ms; once it is
// released, it deletes this run's sandbox.
func TestCloudRunSandboxDeleteResource_WaitsForLock(t *testing.T) {
	rt, env := newStartingRuntime(t, startScripts{})
	rt.state.add(runEntry("dev", "run-a"))
	unlock, err := lockSandboxNameCtx(context.Background(), "dev")
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release := func() { once.Do(unlock) }
	t.Cleanup(release)

	done := make(chan error, 1)
	go func() { done <- rt.DeleteResource(context.Background(), sandboxHandle("run-a")) }()
	select {
	case err := <-done:
		t.Fatalf("DeleteResource returned (%v) while the name lock was held, want it to wait", err)
	case <-time.After(200 * time.Millisecond):
	}
	wantDeletes(t, env.log, 0)
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("DeleteResource = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("DeleteResource did not return within 10 s of the unlock")
	}
	wantDeletes(t, env.log, 1)
	if e := fileEntry(t, env.state); e != nil {
		t.Errorf("entry = %+v, want removed", e)
	}
}

// Fault check S6b: the cleanup reads the cache. Instance X's cache holds
// run A's entry; another instance has since replaced it in the file with
// run B's: X's cleanup of A's handle issues no delete, and B stays.
func TestCloudRunSandboxDeleteResource_ReadsFile(t *testing.T) {
	x, env := newStartingRuntime(t, startScripts{})
	x.state.add(runEntry("dev", "run-a"))
	y := siblingRuntime(t, x, "")
	if _, err := y.state.casPut("dev", nonceOf(y.state.get("dev")), runEntry("dev", "run-b")); err != nil {
		t.Fatalf("write B: %v", err)
	}
	if c := x.state.get("dev"); c == nil || c.Labels[api.LabelRunID] != "run-a" {
		t.Fatalf("X's cache = %+v, want run A's stale entry", c)
	}
	cleanupNoDelete(t, x, env, "run-a")
	wantEntryOf(t, env.state, "run-b")
}

// Fault check S6c: a failed cleanup delete counts as success. The mock
// delete exits 1 with another error: an error is returned and the entry
// is kept.
func TestCloudRunSandboxDeleteResource_FailedDeleteKeepsEntry(t *testing.T) {
	rt, env := newStartingRuntime(t, startScripts{del: `echo "permission denied" >&2; exit 1`})
	rt.state.add(runEntry("dev", "run-a"))
	if err := rt.DeleteResource(context.Background(), sandboxHandle("run-a")); err == nil {
		t.Error("DeleteResource = nil after a failed delete, want an error")
	}
	wantDeletes(t, env.log, 1)
	wantEntryOf(t, env.state, "run-a")
}

// Fault check S11: an empty UID is accepted. The cleanup refuses it with an
// error, issues no delete and leaves the entry.
func TestCloudRunSandboxDeleteResource_EmptyUID(t *testing.T) {
	rt, env := newStartingRuntime(t, startScripts{})
	rt.state.add(runEntry("dev", "run-a"))
	if err := rt.DeleteResource(context.Background(), sandboxHandle("")); err == nil {
		t.Error("DeleteResource with an empty UID = nil, want an error")
	}
	wantDeletes(t, env.log, 0)
	wantEntryOf(t, env.state, "run-a")
}

// A handle of another kind is refused with an error, without a delete.
func TestCloudRunSandboxDeleteResource_OtherKind(t *testing.T) {
	rt, env := newStartingRuntime(t, startScripts{})
	rt.state.add(runEntry("dev", "run-a"))
	h := sandboxHandle("run-a")
	h.Kind = api.ResourceKindContainer
	if err := rt.DeleteResource(context.Background(), h); err == nil {
		t.Error("DeleteResource of a container handle = nil, want an error")
	}
	wantDeletes(t, env.log, 0)
	wantEntryOf(t, env.state, "run-a")
}

// -----------------------------------------------------------------------
// ObserveResourceCreated only (a synchronous start on a flat broker)
// -----------------------------------------------------------------------

// Fault check S12: the sandbox handle bypasses launchHooks.created, so an
// ObserveResourceCreated-only caller never sees it (S12b: the observer
// makes the runtime skip its own synchronous cleanup). With only
// ObserveResourceCreated set (no OnResourceCreated, so the hooks are not
// active), every start reports exactly one handle {sandbox, dev, run-a} to
// the observer, and the runtime keeps its synchronous cleanup: a
// successful start deletes nothing; a killed `run` is deleted by the
// runtime itself (one `delete --force`, no entry left), so the journal's
// later cleanup (DeleteResource) finds nothing and deletes nothing more; a
// self-failed `run` deletes nothing and leaves no entry, and the cleanup
// deletes nothing either.
func TestCloudRunSandboxHooks_ObserverOnlySyncStart(t *testing.T) {
	for _, tc := range []struct {
		name        string
		sc          startScripts
		wantErr     bool
		runtimeDels int
	}{
		{"success", startScripts{}, false, 0},
		{"run killed", startScripts{run: blockUntilKilled}, true, 1},
		{"self-failed", startScripts{run: `echo "sandbox dev already exists" >&2; exit 1`}, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt, env := newStartingRuntime(t, tc.sc)
			cfg := startingCfg(t, "run-a")
			var mu sync.Mutex
			var observed []api.ResourceHandle
			cfg.ObserveResourceCreated = func(h api.ResourceHandle) {
				mu.Lock()
				defer mu.Unlock()
				observed = append(observed, h)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			errc := runSandboxAsync(ctx, rt, cfg)
			if tc.sc.run == blockUntilKilled {
				waitForFile(t, env.mark)
				cancel()
			}
			if err := recvErr(t, errc, "Run"); (err != nil) != tc.wantErr {
				t.Fatalf("Run = %v, want error %v", err, tc.wantErr)
			}
			mu.Lock()
			got := append([]api.ResourceHandle(nil), observed...)
			mu.Unlock()
			if want := []api.ResourceHandle{sandboxHandle("run-a")}; !reflect.DeepEqual(got, want) {
				t.Errorf("observed handles = %+v, want %+v", got, want)
			}
			wantDeletes(t, env.log, tc.runtimeDels)
			if !tc.wantErr {
				return
			}
			if e := fileEntry(t, env.state); e != nil {
				t.Errorf("entry after the failed start = %+v, want none", e)
			}
			// The journal's cleanup of the observed handle (CleanupLaunch ->
			// DeleteResource) finds no entry and deletes nothing more.
			if err := rt.DeleteResource(context.Background(), sandboxHandle("run-a")); err != nil {
				t.Errorf("DeleteResource = %v, want nil", err)
			}
			wantDeletes(t, env.log, tc.runtimeDels)
		})
	}
}
