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

//go:build !no_sqlite

package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/maintenance"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/version"
)

func postOperationRun(t *testing.T, srv *Server, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/maintenance/operations/"+key+"/run",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminMaintenanceOps(rr, req)
	return rr
}

func TestExecuteOperation_ParamAllowlist(t *testing.T) {
	srv, s := newTestServerWithStore(t)
	prev := version.Version
	version.Version = "nightly-20261023"
	t.Cleanup(func() { version.Version = prev })

	tests := []struct {
		name, key, body string
		status          int
		code            string
	}{
		{"download_url", "update-binary", `{"params":{"target_version":"nightly-20261024","download_url":"https://evil.example.com/x.tar.gz"}}`, http.StatusBadRequest, maintenance.CodeUnknownParam},
		{"checksums_url", "update-binary", `{"params":{"target_version":"nightly-20261024","checksums_url":"https://evil.example.com/SHA256SUMS"}}`, http.StatusBadRequest, maintenance.CodeUnknownParam},
		{"current_version", "update-binary", `{"params":{"target_version":"nightly-20261024","current_version":"dev"}}`, http.StatusBadRequest, maintenance.CodeUnknownParam},
		{"non-string unknown param", "update-binary", `{"params":{"target_version":"nightly-20261024","download_url":5}}`, http.StatusBadRequest, maintenance.CodeUnknownParam},
		{"missing target", "update-binary", `{"params":{}}`, http.StatusBadRequest, maintenance.CodeInvalidParam},
		{"not a tag", "update-binary", `{"params":{"target_version":"../../x"}}`, http.StatusBadRequest, maintenance.CodeInvalidParam},
		{"same version", "update-binary", `{"params":{"target_version":"nightly-20261023"}}`, http.StatusConflict, maintenance.CodeAlreadyRunning},
		{"older version (in-memory database: no snapshot)", "update-binary", `{"params":{"target_version":"nightly-20261020"}}`, http.StatusConflict, maintenance.CodeDowngradeUnsupportedDriver},
		{"rebuild-server unknown", "rebuild-server", `{"params":{"branch":"main","repo":"/tmp/x"}}`, http.StatusBadRequest, maintenance.CodeUnknownParam},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rr := postOperationRun(t, srv, tt.key, tt.body)
			if rr.Code != tt.status {
				t.Fatalf("status = %d, want %d: %s", rr.Code, tt.status, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), `"`+tt.code+`"`) {
				t.Fatalf("body lacks code %q: %s", tt.code, rr.Body.String())
			}
			runs, err := s.ListMaintenanceRuns(context.Background(), tt.key, 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(runs) != 0 {
				t.Fatalf("a run was created for a refused request: %+v", runs)
			}
		})
	}
}

func TestExecuteOperation_RestartingRunBlocksNewRun(t *testing.T) {
	srv, s := newTestServerWithStore(t)
	if err := s.CreateMaintenanceRun(context.Background(), &store.MaintenanceOperationRun{
		ID: tid("run-restarting"), OperationKey: "pull-images",
		Status: store.MaintenanceStatusRestarting, StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if rr := postOperationRun(t, srv, "pull-images", `{"params":{}}`); rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rr.Code, rr.Body.String())
	}
}

// restartingExecutor imitates update-binary: it logs, calls the hook, then
// (when the hook succeeds) "restarts" and logs again.
type restartingExecutor struct {
	hookErr error // returned by Run after a failed hook, wrapped
}

func (e *restartingExecutor) Run(ctx context.Context, w io.Writer, p map[string]string) error {
	return e.RunWithBeforeRestart(ctx, w, p, nil)
}

func (e *restartingExecutor) RunWithBeforeRestart(ctx context.Context, w io.Writer, _ map[string]string, before maintenance.BeforeRestartFunc) error {
	_, _ = fmt.Fprintln(w, "installed nightly-20261024")
	if before != nil {
		if err := before(ctx, maintenance.PendingRestart{From: "nightly-20261023", To: "nightly-20261024", Strategy: "symlink"}); err != nil {
			return fmt.Errorf("hook: %w", err)
		}
	}
	if e.hookErr != nil {
		return e.hookErr
	}
	_, _ = fmt.Fprintln(w, "restart requested")
	return nil
}

func waitForRunStatus(t *testing.T, s store.Store, runID string, done func(*store.MaintenanceOperationRun) bool) *store.MaintenanceOperationRun {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		run, err := s.GetMaintenanceRun(context.Background(), runID)
		if err != nil {
			t.Fatal(err)
		}
		if done(run) {
			return run
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s did not settle: %+v", runID, run)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitForRestartRecorded waits until the before-restart hook of run has
// finished: system.pending_update names it and the run is "restarting"
// (the hook's last write). The runner writes nothing after that.
func waitForRestartRecorded(t *testing.T, s store.Store, runID string) *store.MaintenanceOperationRun {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(5 * time.Second)
	for {
		run, err := s.GetMaintenanceRun(ctx, runID)
		if err != nil {
			t.Fatal(err)
		}
		pending, perr := s.GetHubSetting(ctx, HubSettingSectionPendingUpdate)
		if run.Status == store.MaintenanceStatusRestarting && perr == nil &&
			strings.Contains(string(pending.Value), runID) {
			return run
		}
		if time.Now().After(deadline) {
			t.Fatalf("restart of run %s not recorded: run=%+v pending err=%v", runID, run, perr)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func newRun(t *testing.T, s store.Store) *store.MaintenanceOperationRun {
	t.Helper()
	run := &store.MaintenanceOperationRun{
		ID: api.NewUUID(), OperationKey: "update-binary",
		Status: store.MaintenanceStatusRunning, StartedAt: time.Now(), StartedBy: "admin@example.com",
	}
	if err := s.CreateMaintenanceRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	return run
}

func TestLaunchMaintenanceRun_RecordsRestartingBeforeRestart(t *testing.T) {
	srv, s := newTestServerWithStore(t)
	ctx := context.Background()
	if _, err := s.UpsertHubSetting(ctx, HubSettingSectionUpdateAvailable, json.RawMessage(`{"version":"nightly-20261024"}`), "test", -1, "managed"); err != nil {
		t.Fatal(err)
	}

	run := newRun(t, s)
	srv.launchMaintenanceRun(run, &restartingExecutor{}, map[string]string{})

	got := waitForRestartRecorded(t, s, run.ID)
	if got.Status != store.MaintenanceStatusRestarting {
		t.Fatalf("status = %q, want restarting", got.Status)
	}
	if got.CompletedAt != nil {
		t.Fatalf("CompletedAt set on a restarting run: %v", got.CompletedAt)
	}
	var result map[string]string
	if err := json.Unmarshal([]byte(got.Result), &result); err != nil {
		t.Fatalf("result %q: %v", got.Result, err)
	}
	if result["from"] != "nightly-20261023" || result["to"] != "nightly-20261024" || result["strategy"] != "symlink" {
		t.Fatalf("result = %v", result)
	}
	if !strings.Contains(got.Log, "installed nightly-20261024") {
		t.Fatalf("log = %q", got.Log)
	}
	// The notification stays until boot reconciliation verifies the update.
	if _, err := s.GetHubSetting(ctx, HubSettingSectionUpdateAvailable); err != nil {
		t.Fatalf("update_available cleared before the update was verified: %v", err)
	}
}

func TestLaunchMaintenanceRun_FailureAfterHookIsFailed(t *testing.T) {
	srv, s := newTestServerWithStore(t)
	ctx := context.Background()
	if _, err := s.UpsertHubSetting(ctx, HubSettingSectionUpdateAvailable, json.RawMessage(`{"version":"nightly-20261024"}`), "test", -1, "managed"); err != nil {
		t.Fatal(err)
	}
	run := newRun(t, s)
	srv.launchMaintenanceRun(run, &restartingExecutor{hookErr: errors.New("restart failed")}, map[string]string{})
	got := waitForRunStatus(t, s, run.ID, func(r *store.MaintenanceOperationRun) bool {
		return r.Status == store.MaintenanceStatusFailed
	})
	if got.CompletedAt == nil || !strings.Contains(got.Result, "restart failed") {
		t.Fatalf("run = %+v", got)
	}
	// The hub did not change version: the Apply banner must survive.
	if _, err := s.GetHubSetting(ctx, HubSettingSectionUpdateAvailable); err != nil {
		t.Fatalf("update_available lost after a failure past the hook: %v", err)
	}
}

func TestLaunchMaintenanceRun_RequestErrorCodeInResult(t *testing.T) {
	srv, s := newTestServerWithStore(t)
	run := newRun(t, s)
	exec := maintenance.NewBinaryUpdateExecutor(maintenance.BinaryUpdateOptions{CurrentVersion: "nightly-20261023"})
	srv.launchMaintenanceRun(run, exec, map[string]string{"target_version": "nightly-20261023"})
	got := waitForRunStatus(t, s, run.ID, func(r *store.MaintenanceOperationRun) bool {
		return r.Status == store.MaintenanceStatusFailed
	})
	if !strings.Contains(got.Result, `"code":"`+maintenance.CodeAlreadyRunning+`"`) {
		t.Fatalf("result = %s", got.Result)
	}
}

func TestExecuteOperation_UpdateAndRebuildAreExclusive(t *testing.T) {
	prev := version.Version
	version.Version = "nightly-20261023"
	t.Cleanup(func() { version.Version = prev })

	tests := []struct {
		name, runningKey, runningStatus, postKey, body string
	}{
		{"rebuild running blocks update", "rebuild-server", store.MaintenanceStatusRunning, "update-binary", `{"params":{"target_version":"nightly-20261024"}}`},
		{"update running blocks rebuild", "update-binary", store.MaintenanceStatusRunning, "rebuild-server", `{"params":{}}`},
		{"update restarting blocks rebuild", "update-binary", store.MaintenanceStatusRestarting, "rebuild-server", `{"params":{}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, s := newTestServerWithStore(t)
			blockingID := api.NewUUID()
			if err := s.CreateMaintenanceRun(context.Background(), &store.MaintenanceOperationRun{
				ID: blockingID, OperationKey: tt.runningKey, Status: tt.runningStatus, StartedAt: time.Now(),
			}); err != nil {
				t.Fatal(err)
			}
			rr := postOperationRun(t, srv, tt.postKey, tt.body)
			if rr.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409: %s", rr.Code, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), blockingID) || !strings.Contains(rr.Body.String(), tt.runningKey) {
				t.Fatalf("body does not name the blocking run: %s", rr.Body.String())
			}
			runs, err := s.ListMaintenanceRuns(context.Background(), tt.postKey, 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(runs) != 0 {
				t.Fatalf("run created despite the exclusive group: %+v", runs)
			}
		})
	}
}

func TestCreateExclusiveMaintenanceRun(t *testing.T) {
	srv, s := newTestServerWithStore(t)
	ctx := context.Background()
	first := &store.MaintenanceOperationRun{ID: api.NewUUID(), OperationKey: "rebuild-server",
		Status: store.MaintenanceStatusRunning, StartedAt: time.Now()}
	if blocking, err := srv.createExclusiveMaintenanceRun(ctx, first); err != nil || blocking != nil {
		t.Fatalf("first create: blocking=%v err=%v", blocking, err)
	}
	second := &store.MaintenanceOperationRun{ID: api.NewUUID(), OperationKey: "update-binary",
		Status: store.MaintenanceStatusRunning, StartedAt: time.Now()}
	blocking, err := srv.createExclusiveMaintenanceRun(ctx, second)
	if err != nil || blocking == nil || blocking.ID != first.ID {
		t.Fatalf("second create: blocking=%v err=%v", blocking, err)
	}
	if _, err := s.GetMaintenanceRun(ctx, second.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("blocked run was created: %v", err)
	}
	// Unrelated operations are not part of the group.
	other := &store.MaintenanceOperationRun{ID: api.NewUUID(), OperationKey: "pull-images",
		Status: store.MaintenanceStatusRunning, StartedAt: time.Now()}
	if blocking, err := srv.createExclusiveMaintenanceRun(ctx, other); err != nil || blocking != nil {
		t.Fatalf("pull-images create: blocking=%v err=%v", blocking, err)
	}
}

func TestMaintenanceRunContext(t *testing.T) {
	ctx, cancel := maintenanceRunContext("update-binary")
	defer cancel()
	if dl, ok := ctx.Deadline(); !ok || time.Until(dl) > 20*time.Minute {
		t.Fatalf("update-binary deadline = %v, %v", dl, ok)
	}
	ctx2, cancel2 := maintenanceRunContext("pull-images")
	defer cancel2()
	if _, ok := ctx2.Deadline(); ok {
		t.Fatal("pull-images got a deadline")
	}
}

type cleanupCtxTestKey struct{}

func TestMaintenanceCleanupContext_DetachedAndBounded(t *testing.T) {
	parent, cancel := context.WithCancel(context.WithValue(context.Background(), cleanupCtxTestKey{}, "trace-1"))
	cancel()
	ctx, cancelCleanup := maintenanceCleanupContext(parent)
	defer cancelCleanup()
	if err := ctx.Err(); err != nil {
		t.Fatalf("cleanup ctx inherited the parent's cancellation: %v", err)
	}
	dl, ok := ctx.Deadline()
	if !ok || time.Until(dl) > maintenanceCleanupTimeout {
		t.Fatalf("cleanup ctx deadline = %v, %v; want bounded by %v", dl, ok, maintenanceCleanupTimeout)
	}
	if v := ctx.Value(cleanupCtxTestKey{}); v != "trace-1" {
		t.Fatalf("cleanup ctx lost the parent's values: %v", v)
	}
}

// The before-restart hook's failure path clears the pending update it just
// wrote. Its ctx may be the one that failed the write (cancelled, expired);
// the clear must still happen, or boot reconciliation would find a stale
// pending update for a run that never restarted.
func TestClearPendingUpdateAfterFailure_ParentCancelled(t *testing.T) {
	srv, s := newTestServerWithStore(t)
	run := newRun(t, s)
	if err := srv.writeHubSettingJSON(context.Background(), HubSettingSectionPendingUpdate,
		PendingUpdate{RunID: run.ID, From: "nightly-20261023", To: "nightly-20261024", Strategy: "symlink"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	srv.clearPendingUpdateAfterFailure(ctx, run.ID)

	if _, err := s.GetHubSetting(context.Background(), HubSettingSectionPendingUpdate); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("pending_update survived a clear on a cancelled ctx: %v", err)
	}
}

// Another run's pending update is left alone, also on the cleanup path.
func TestClearPendingUpdateAfterFailure_OtherRunKept(t *testing.T) {
	srv, s := newTestServerWithStore(t)
	run := newRun(t, s)
	if err := srv.writeHubSettingJSON(context.Background(), HubSettingSectionPendingUpdate,
		PendingUpdate{RunID: "another-run", To: "nightly-20261024"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	srv.clearPendingUpdateAfterFailure(ctx, run.ID)
	if _, err := s.GetHubSetting(context.Background(), HubSettingSectionPendingUpdate); err != nil {
		t.Fatalf("another run's pending_update was cleared: %v", err)
	}
}

// cancelOnRestartingStore fails the run write that records "restarting": it
// first cancels the hook's ctx (as a shutdown or deadline would) and then
// returns that ctx's error, so the failure path runs with a dead ctx.
type cancelOnRestartingStore struct {
	store.Store
	fault  *storeFaultSwitch
	cancel context.CancelFunc
}

func (c *cancelOnRestartingStore) UpdateMaintenanceRun(ctx context.Context, run *store.MaintenanceOperationRun) error {
	if c.fault.Active() && run.Status == store.MaintenanceStatusRestarting {
		c.cancel()
		return ctx.Err()
	}
	return c.Store.UpdateMaintenanceRun(ctx, run)
}

// Regression test at the call site Gemini flagged: when markRunRestarting's
// run write fails because its ctx was cancelled, the pending update it has
// just written is still cleared (the runner's own clear does not run, as
// the run never became "restarting").
func TestMarkRunRestarting_FailureOnCancelledCtxClearsPendingUpdate(t *testing.T) {
	srv, s := newTestServerWithStore(t)
	run := newRun(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, fault := installStoreFault(t, srv, func(inner store.Store, f *storeFaultSwitch) *cancelOnRestartingStore {
		return &cancelOnRestartingStore{Store: inner, fault: f, cancel: cancel}
	})
	fault.Arm()

	err := srv.markRunRestarting(ctx, run, &runLogBuffer{}, maintenance.PendingRestart{
		From: "nightly-20261023", To: "nightly-20261024", Strategy: "symlink",
	})
	if err == nil || !strings.Contains(err.Error(), "record run as restarting") {
		t.Fatalf("markRunRestarting = %v, want the run write failure", err)
	}
	if ctx.Err() == nil {
		t.Fatal("test setup: the hook ctx was not cancelled by the failing write")
	}
	requireNoPendingUpdate(t, s)
	if run.Status == store.MaintenanceStatusRestarting {
		t.Fatalf("run left as restarting after a failed write: %+v", run)
	}
}
