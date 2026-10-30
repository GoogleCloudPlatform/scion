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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/maintenance"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// testInstaller is a maintenance.Installer double with a fixed status. It
// records Restart calls and never touches the host.
type testInstaller struct {
	status     maintenance.InstallerStatus
	restartErr error

	mu       sync.Mutex
	restarts int
}

func newTestInstaller(strategy string) *testInstaller {
	st := maintenance.InstallerStatus{Strategy: strategy, Supported: strategy != maintenance.StrategyUnsupported}
	if !st.Supported {
		st.Reason = "symlink layout: /opt/scion/current is not a symlink"
		st.MigrateHint = maintenance.MigrateLayoutHint
	}
	return &testInstaller{status: st}
}

func (f *testInstaller) Name() string                                           { return f.status.Strategy }
func (f *testInstaller) Preflight(context.Context) error                        { return nil }
func (f *testInstaller) Stage(_ context.Context, tag, _ string) (string, error) { return tag, nil }
func (f *testInstaller) Activate(context.Context, string) error                 { return nil }
func (f *testInstaller) Retained() []maintenance.InstalledRelease               { return nil }
func (f *testInstaller) Status() maintenance.InstallerStatus                    { return f.status }
func (f *testInstaller) Restart(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restarts++
	return f.restartErr
}

func (f *testInstaller) restartCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.restarts
}

func putPendingUpdate(t *testing.T, s store.Store, p PendingUpdate) {
	t.Helper()
	b, _ := json.Marshal(p)
	if _, err := s.UpsertHubSetting(context.Background(), HubSettingSectionPendingUpdate, b, "test", -1, "managed"); err != nil {
		t.Fatal(err)
	}
}

func getLastUpdate(t *testing.T, s store.Store) *LastUpdate {
	t.Helper()
	setting, err := s.GetHubSetting(context.Background(), HubSettingSectionLastUpdate)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var last LastUpdate
	if err := json.Unmarshal(setting.Value, &last); err != nil {
		t.Fatal(err)
	}
	return &last
}

func requireNoPendingUpdate(t *testing.T, s store.Store) {
	t.Helper()
	if _, err := s.GetHubSetting(context.Background(), HubSettingSectionPendingUpdate); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("pending_update still present: %v", err)
	}
}

func newRestartingRun(t *testing.T, s store.Store) *store.MaintenanceOperationRun {
	t.Helper()
	run := newRun(t, s)
	run.Status = store.MaintenanceStatusRestarting
	run.Result = `{"from":"nightly-20261023","to":"nightly-20261024","strategy":"symlink"}`
	run.Log = "installed\n"
	if err := s.UpdateMaintenanceRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	return run
}

func TestReconcilePendingUpdate(t *testing.T) {
	ctx := context.Background()
	pendingFor := func(runID string) PendingUpdate {
		return PendingUpdate{RunID: runID, From: "nightly-20261023", To: "nightly-20261024", Strategy: "symlink", At: "2026-10-24T03:00:00Z"}
	}

	t.Run("version matches: completed and verified", func(t *testing.T) {
		srv, s := newTestServerWithStore(t)
		run := newRestartingRun(t, s)
		putPendingUpdate(t, s, pendingFor(run.ID))

		if err := srv.reconcilePendingUpdate(ctx, "nightly-20261024"); err != nil {
			t.Fatal(err)
		}
		got, _ := s.GetMaintenanceRun(ctx, run.ID)
		if got.Status != store.MaintenanceStatusCompleted || got.CompletedAt == nil {
			t.Fatalf("run = %+v, want completed", got)
		}
		if !strings.Contains(got.Result, `"verification":"verified"`) || !strings.Contains(got.Log, "update verified") {
			t.Errorf("result/log = %s / %q", got.Result, got.Log)
		}
		last := getLastUpdate(t, s)
		if last == nil || last.Status != LastUpdateVerified || last.RunID != run.ID ||
			last.From != "nightly-20261023" || last.To != "nightly-20261024" || last.At == "" {
			t.Fatalf("last_update = %+v", last)
		}
		requireNoPendingUpdate(t, s)
	})

	t.Run("version mismatch: failed", func(t *testing.T) {
		srv, s := newTestServerWithStore(t)
		run := newRestartingRun(t, s)
		putPendingUpdate(t, s, pendingFor(run.ID))

		if err := srv.reconcilePendingUpdate(ctx, "nightly-20261023"); err != nil {
			t.Fatal(err)
		}
		got, _ := s.GetMaintenanceRun(ctx, run.ID)
		if got.Status != store.MaintenanceStatusFailed || got.CompletedAt == nil {
			t.Fatalf("run = %+v, want failed", got)
		}
		if !strings.Contains(got.Result, "nightly-20261024") || !strings.Contains(got.Result, `"verification":"failed"`) {
			t.Errorf("result = %s", got.Result)
		}
		if last := getLastUpdate(t, s); last == nil || last.Status != LastUpdateFailed {
			t.Fatalf("last_update = %+v", last)
		}
		requireNoPendingUpdate(t, s)
	})

	t.Run("missing run", func(t *testing.T) {
		srv, s := newTestServerWithStore(t)
		putPendingUpdate(t, s, pendingFor("no-such-run"))

		if err := srv.reconcilePendingUpdate(ctx, "nightly-20261024"); err != nil {
			t.Fatal(err)
		}
		if last := getLastUpdate(t, s); last == nil || last.Status != LastUpdateVerified || last.RunID != "no-such-run" {
			t.Fatalf("last_update = %+v", last)
		}
		requireNoPendingUpdate(t, s)
	})

	t.Run("no pending update", func(t *testing.T) {
		srv, s := newTestServerWithStore(t)
		running := newRun(t, s) // a "running" run is the abort sweep's, not ours
		if err := srv.reconcilePendingUpdate(ctx, "nightly-20261024"); err != nil {
			t.Fatal(err)
		}
		if last := getLastUpdate(t, s); last != nil {
			t.Fatalf("last_update written without a pending update: %+v", last)
		}
		if got, _ := s.GetMaintenanceRun(ctx, running.ID); got.Status != store.MaintenanceStatusRunning {
			t.Fatalf("running run touched: %+v", got)
		}
	})

	t.Run("stale restarting run without pending update is failed", func(t *testing.T) {
		srv, s := newTestServerWithStore(t)
		run := newRestartingRun(t, s)
		if err := srv.reconcilePendingUpdate(ctx, "nightly-20261024"); err != nil {
			t.Fatal(err)
		}
		got, _ := s.GetMaintenanceRun(ctx, run.ID)
		if got.Status != store.MaintenanceStatusFailed || got.CompletedAt == nil {
			t.Fatalf("run = %+v, want failed", got)
		}
	})

	t.Run("idempotent", func(t *testing.T) {
		srv, s := newTestServerWithStore(t)
		run := newRestartingRun(t, s)
		putPendingUpdate(t, s, pendingFor(run.ID))
		if err := srv.reconcilePendingUpdate(ctx, "nightly-20261024"); err != nil {
			t.Fatal(err)
		}
		first := getLastUpdate(t, s)
		// A pending row surviving a failed clear is reconciled again
		// without changing the finished run.
		putPendingUpdate(t, s, pendingFor(run.ID))
		if err := srv.reconcilePendingUpdate(ctx, "nightly-20261024"); err != nil {
			t.Fatal(err)
		}
		if err := srv.reconcilePendingUpdate(ctx, "nightly-20261024"); err != nil {
			t.Fatal(err)
		}
		got, _ := s.GetMaintenanceRun(ctx, run.ID)
		if got.Status != store.MaintenanceStatusCompleted || strings.Count(got.Log, "update verified") != 1 {
			t.Fatalf("run = %+v", got)
		}
		if last := getLastUpdate(t, s); last == nil || last.Status != first.Status || last.RunID != first.RunID {
			t.Fatalf("last_update = %+v, first %+v", last, first)
		}
		requireNoPendingUpdate(t, s)
	})

	t.Run("unreadable pending update is discarded", func(t *testing.T) {
		srv, s := newTestServerWithStore(t)
		if _, err := s.UpsertHubSetting(ctx, HubSettingSectionPendingUpdate, json.RawMessage(`"oops"`), "test", -1, "managed"); err != nil {
			t.Fatal(err)
		}
		if err := srv.reconcilePendingUpdate(ctx, "nightly-20261024"); err != nil {
			t.Fatal(err)
		}
		requireNoPendingUpdate(t, s)
	})
}

func TestUpdateOutcome(t *testing.T) {
	p := PendingUpdate{To: "v0.3.0-preview.4"}
	if got := updateOutcome(p, "v0.3.0-preview.4"); got != LastUpdateVerified {
		t.Errorf("match = %q", got)
	}
	if got := updateOutcome(p, "nightly-20261023"); got != LastUpdateFailed {
		t.Errorf("mismatch = %q", got)
	}
	if got := updateOutcome(PendingUpdate{}, ""); got != LastUpdateFailed {
		t.Errorf("empty target = %q", got)
	}
}

// TestAbortRunningMaintenanceOps_SkipsRestartingWithPendingUpdate pins the
// boot order contract: the abort sweep fails "running" runs but leaves a
// "restarting" run with a pending update for reconciliation.
func TestAbortRunningMaintenanceOps_SkipsRestartingWithPendingUpdate(t *testing.T) {
	ctx := context.Background()
	_, s := newTestServerWithStore(t)
	restarting := newRestartingRun(t, s)
	putPendingUpdate(t, s, PendingUpdate{RunID: restarting.ID, To: "nightly-20261024"})
	running := newRun(t, s)

	if _, _, err := s.AbortRunningMaintenanceOps(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetMaintenanceRun(ctx, restarting.ID); got.Status != store.MaintenanceStatusRestarting {
		t.Fatalf("restarting run aborted: %+v", got)
	}
	if got, _ := s.GetMaintenanceRun(ctx, running.ID); got.Status != store.MaintenanceStatusFailed {
		t.Fatalf("running run not aborted: %+v", got)
	}
	if _, err := s.GetHubSetting(ctx, HubSettingSectionPendingUpdate); err != nil {
		t.Fatalf("pending_update removed by the abort sweep: %v", err)
	}
}

// failingSettingStore fails UpsertHubSetting for one section while its
// fault switch is armed.
type failingSettingStore struct {
	store.Store
	section string
	fault   *storeFaultSwitch
}

func (f *failingSettingStore) UpsertHubSetting(ctx context.Context, section string, value json.RawMessage, updatedBy string, expectedRevision int64, origin string) (*store.HubSetting, error) {
	if section == f.section && f.fault.Active() {
		return nil, errors.New("disk full")
	}
	return f.Store.UpsertHubSetting(ctx, section, value, updatedBy, expectedRevision, origin)
}

func TestLaunchMaintenanceRun_WritesPendingUpdateBeforeRestart(t *testing.T) {
	srv, s := newTestServerWithStore(t)
	run := newRun(t, s)
	srv.launchMaintenanceRun(run, &restartingExecutor{}, map[string]string{})
	waitForRestartRecorded(t, s, run.ID)

	var p PendingUpdate
	found, err := srv.readHubSettingJSON(context.Background(), HubSettingSectionPendingUpdate, &p)
	if err != nil || !found {
		t.Fatalf("pending_update not written: found=%v err=%v", found, err)
	}
	if p.RunID != run.ID || p.From != "nightly-20261023" || p.To != "nightly-20261024" ||
		p.Strategy != "symlink" || p.Snapshot != "" || p.At == "" {
		t.Fatalf("pending_update = %+v", p)
	}
	setting, _ := s.GetHubSetting(context.Background(), HubSettingSectionPendingUpdate)
	if !strings.Contains(string(setting.Value), `"snapshot":""`) {
		t.Errorf("pending_update must carry snapshot even when empty: %s", setting.Value)
	}
}

func TestLaunchMaintenanceRun_PendingWriteFailureDoesNotRestart(t *testing.T) {
	srv, s := newTestServerWithStore(t)
	_, fault := installStoreFault(t, srv, func(inner store.Store, f *storeFaultSwitch) *failingSettingStore {
		return &failingSettingStore{Store: inner, section: HubSettingSectionPendingUpdate, fault: f}
	})
	fault.Arm()
	run := newRun(t, s)
	srv.launchMaintenanceRun(run, &restartingExecutor{}, map[string]string{})
	got := waitForRunStatus(t, s, run.ID, func(r *store.MaintenanceOperationRun) bool {
		return r.Status == store.MaintenanceStatusFailed
	})
	if strings.Contains(got.Log, "restart requested") {
		t.Fatalf("restarted after a failed pending_update write: %q", got.Log)
	}
	if !strings.Contains(got.Result, "record pending update") {
		t.Errorf("result = %s", got.Result)
	}
	requireNoPendingUpdate(t, s)
}

func TestLaunchMaintenanceRun_FailureAfterHookClearsPendingUpdate(t *testing.T) {
	srv, s := newTestServerWithStore(t)
	run := newRun(t, s)
	srv.launchMaintenanceRun(run, &restartingExecutor{hookErr: errors.New("restart failed")}, map[string]string{})
	waitForRunStatus(t, s, run.ID, func(r *store.MaintenanceOperationRun) bool {
		return r.Status == store.MaintenanceStatusFailed
	})
	requireNoPendingUpdate(t, s)
}

// TestUpdateRun_RestartingThenVerifiedAfterReboot is the Phase 1 acceptance
// path with test doubles: an update run through the before-restart hook ends
// "restarting" with a pending update; a "reboot" on the target version then
// completes it with last_update verified.
func TestUpdateRun_RestartingThenVerifiedAfterReboot(t *testing.T) {
	ctx := context.Background()
	srv, s := newTestServerWithStore(t)
	run := newRun(t, s)
	srv.launchMaintenanceRun(run, &restartingExecutor{}, map[string]string{})
	got := waitForRestartRecorded(t, s, run.ID)
	if got.Status != store.MaintenanceStatusRestarting {
		t.Fatalf("status before reboot = %q, want restarting", got.Status)
	}

	// Boot order of New(): reconciliation, then the abort sweep.
	if err := srv.reconcilePendingUpdate(ctx, "nightly-20261024"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AbortRunningMaintenanceOps(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetMaintenanceRun(ctx, run.ID)
	if got.Status != store.MaintenanceStatusCompleted {
		t.Fatalf("status after reboot = %q, want completed", got.Status)
	}
	if last := getLastUpdate(t, s); last == nil || last.Status != LastUpdateVerified || last.RunID != run.ID {
		t.Fatalf("last_update = %+v", last)
	}
}

func getUpdateStatus(t *testing.T, srv *Server) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/maintenance/update-status", nil)
	rr := httptest.NewRecorder()
	srv.handleUpdateStatus(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func TestHandleUpdateStatus_Shape(t *testing.T) {
	srv, _ := newTestServerWithStore(t)
	srv.config.MaintenanceConfig = MaintenanceConfig{
		DeploymentTier: "binary",
		UpdatePolicy:   "notify",
		RepoPath:       "/home/scion/secret-checkout",
		RepoBranch:     "main",
		Installer:      newTestInstaller(maintenance.StrategySymlink),
	}
	body := getUpdateStatus(t, srv)

	for _, k := range []string{"current", "mode", "channel", "pin", "installer", "updatePolicy", "pendingUpdate", "lastUpdate"} {
		if _, ok := body[k]; !ok {
			t.Errorf("missing key %q in %v", k, body)
		}
	}
	if body["pendingUpdate"] != nil || body["lastUpdate"] != nil {
		t.Errorf("pending/last must be null when absent: %v", body)
	}
	if body["pin"] != "latest" || body["updatePolicy"] != "notify" {
		t.Errorf("pin/updatePolicy = %v/%v", body["pin"], body["updatePolicy"])
	}
	cur := body["current"].(map[string]any)
	for _, k := range []string{"version", "channel", "commit", "buildTime", "isGitBuild"} {
		if _, ok := cur[k]; !ok {
			t.Errorf("current lacks %q: %v", k, cur)
		}
	}
	mode := body["mode"].(map[string]any)
	if mode["effective"] != "release" || mode["repoConfigured"] != true || mode["repoBranch"] != "main" {
		t.Errorf("mode = %v", mode)
	}
	inst := body["installer"].(map[string]any)
	if inst["strategy"] != "symlink" || inst["supported"] != true {
		t.Errorf("installer = %v", inst)
	}
	ch := body["channel"].(map[string]any)
	for _, k := range []string{"effective", "configured", "source"} {
		if _, ok := ch[k]; !ok {
			t.Errorf("channel lacks %q: %v", k, ch)
		}
	}
}

func TestHandleUpdateStatus_MethodNotAllowed(t *testing.T) {
	srv, _ := newTestServerWithStore(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/maintenance/update-status", nil)
	rr := httptest.NewRecorder()
	srv.handleUpdateStatus(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d", rr.Code)
	}
}

func TestBuildUpdateStatus(t *testing.T) {
	ctx := context.Background()
	release := runningBuild{Version: "nightly-20261023", Commit: "abc123def456", BuildTime: "2026-10-23T02:00:00Z"}

	t.Run("installer strategies", func(t *testing.T) {
		for _, strategy := range []string{maintenance.StrategySymlink, maintenance.StrategySudoInstall, maintenance.StrategyUnsupported} {
			srv, _ := newTestServerWithStore(t)
			inst := newTestInstaller(strategy)
			srv.config.MaintenanceConfig = MaintenanceConfig{DeploymentTier: "binary", Installer: inst}
			st, err := srv.buildUpdateStatus(ctx, release)
			if err != nil {
				t.Fatal(err)
			}
			if st.Installer != inst.status {
				t.Errorf("%s: installer = %+v, want %+v", strategy, st.Installer, inst.status)
			}
			if strategy == maintenance.StrategyUnsupported && (st.Installer.Supported || st.Installer.Reason == "" || st.Installer.MigrateHint == "") {
				t.Errorf("unsupported must carry reason and hint: %+v", st.Installer)
			}
		}
	})

	t.Run("no installer is unsupported", func(t *testing.T) {
		srv, _ := newTestServerWithStore(t)
		st, err := srv.buildUpdateStatus(ctx, release)
		if err != nil {
			t.Fatal(err)
		}
		if st.Installer.Strategy != maintenance.StrategyUnsupported || st.Installer.Supported {
			t.Errorf("installer = %+v", st.Installer)
		}
	})

	t.Run("repoConfigured without leaking the path", func(t *testing.T) {
		for _, repo := range []string{"", "/home/scion/very-private-checkout"} {
			srv, _ := newTestServerWithStore(t)
			srv.config.MaintenanceConfig = MaintenanceConfig{RepoPath: repo, DeploymentTier: "binary"}
			st, err := srv.buildUpdateStatus(ctx, release)
			if err != nil {
				t.Fatal(err)
			}
			if st.Mode.RepoConfigured != (repo != "") {
				t.Errorf("repo %q: repoConfigured = %v", repo, st.Mode.RepoConfigured)
			}
			b, _ := json.Marshal(st)
			if repo != "" && strings.Contains(string(b), repo) {
				t.Errorf("update-status leaks the repo path: %s", b)
			}
		}
	})

	t.Run("channel source", func(t *testing.T) {
		srv, _ := newTestServerWithStore(t)
		srv.config.MaintenanceConfig = MaintenanceConfig{ReleaseChannel: "preview"}
		st, _ := srv.buildUpdateStatus(ctx, release)
		if st.Channel != (updateStatusChannel{Effective: "preview", Configured: "preview", Source: ChannelSourceConfig}) {
			t.Errorf("configured channel = %+v", st.Channel)
		}
		if st.Current.Channel != "nightly" {
			t.Errorf("current.channel = %q", st.Current.Channel)
		}

		srv.config.MaintenanceConfig = MaintenanceConfig{}
		st, _ = srv.buildUpdateStatus(ctx, release)
		if st.Channel != (updateStatusChannel{Effective: "nightly", Configured: "", Source: ChannelSourceDetected}) {
			t.Errorf("detected channel = %+v", st.Channel)
		}
	})

	t.Run("mode from deployment tier", func(t *testing.T) {
		srv, _ := newTestServerWithStore(t)
		for tier, want := range map[string]string{"source": UpdateModeGit, "binary": UpdateModeRelease, "": UpdateModeRelease} {
			srv.config.MaintenanceConfig = MaintenanceConfig{DeploymentTier: tier}
			st, _ := srv.buildUpdateStatus(ctx, release)
			if st.Mode.Effective != want {
				t.Errorf("tier %q: mode = %q, want %q", tier, st.Mode.Effective, want)
			}
		}
	})

	t.Run("current build", func(t *testing.T) {
		srv, _ := newTestServerWithStore(t)
		st, _ := srv.buildUpdateStatus(ctx, release)
		if st.Current != (updateStatusCurrent{Version: "nightly-20261023", Channel: "nightly", Commit: "abc123def456",
			BuildTime: "2026-10-23T02:00:00Z", IsGitBuild: false}) {
			t.Errorf("current = %+v", st.Current)
		}
		st, _ = srv.buildUpdateStatus(ctx, runningBuild{Commit: "0123456789abcdef"})
		if !st.Current.IsGitBuild || st.Current.Version != "01234567" || st.Current.Channel != "" {
			t.Errorf("git build current = %+v", st.Current)
		}
	})

	t.Run("pending and last update", func(t *testing.T) {
		srv, s := newTestServerWithStore(t)
		putPendingUpdate(t, s, PendingUpdate{RunID: "r1", From: "a", To: "nightly-20261024", Strategy: "symlink", At: "t"})
		last, _ := json.Marshal(LastUpdate{RunID: "r0", From: "x", To: "y", Status: LastUpdateVerified, At: "t0"})
		if _, err := s.UpsertHubSetting(ctx, HubSettingSectionLastUpdate, last, "test", -1, "managed"); err != nil {
			t.Fatal(err)
		}
		st, err := srv.buildUpdateStatus(ctx, release)
		if err != nil {
			t.Fatal(err)
		}
		if st.PendingUpdate == nil || st.PendingUpdate.RunID != "r1" || st.PendingUpdate.To != "nightly-20261024" {
			t.Errorf("pendingUpdate = %+v", st.PendingUpdate)
		}
		if st.LastUpdate == nil || st.LastUpdate.RunID != "r0" || st.LastUpdate.Status != LastUpdateVerified {
			t.Errorf("lastUpdate = %+v", st.LastUpdate)
		}
	})
}

func postRestart(t *testing.T, srv *Server) *httptest.ResponseRecorder {
	t.Helper()
	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/maintenance/restart", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminRestart(rr, req)
	return rr
}

func TestHandleAdminRestart_Installer(t *testing.T) {
	t.Run("symlink restarts after responding", func(t *testing.T) {
		srv, _ := newTestServerWithStore(t)
		// No sudo may run: an exec would fail with this PATH and give 500.
		t.Setenv("PATH", t.TempDir())
		inst := newTestInstaller(maintenance.StrategySymlink)
		srv.config.MaintenanceConfig.Installer = inst

		rr := postRestart(t, srv)
		if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"restarting"`) {
			t.Fatalf("response = %d %s", rr.Code, rr.Body.String())
		}
		if n := inst.restartCount(); n != 0 {
			t.Fatalf("restart requested before the response was written (%d)", n)
		}
		deadline := time.Now().Add(5 * time.Second)
		for inst.restartCount() == 0 {
			if time.Now().After(deadline) {
				t.Fatal("installer restart never requested")
			}
			time.Sleep(10 * time.Millisecond)
		}
	})

	t.Run("sudo-install restarts through the installer", func(t *testing.T) {
		srv, _ := newTestServerWithStore(t)
		t.Setenv("PATH", t.TempDir())
		inst := newTestInstaller(maintenance.StrategySudoInstall)
		srv.config.MaintenanceConfig.Installer = inst

		rr := postRestart(t, srv)
		if rr.Code != http.StatusOK {
			t.Fatalf("response = %d %s", rr.Code, rr.Body.String())
		}
		if inst.restartCount() != 1 {
			t.Fatalf("restarts = %d, want 1", inst.restartCount())
		}
	})

	t.Run("sudo-install failure is a generic 500", func(t *testing.T) {
		srv, _ := newTestServerWithStore(t)
		inst := newTestInstaller(maintenance.StrategySudoInstall)
		inst.restartErr = errors.New("sudo: a password is required")
		srv.config.MaintenanceConfig.Installer = inst

		rr := postRestart(t, srv)
		if rr.Code != http.StatusInternalServerError {
			t.Fatalf("response = %d %s", rr.Code, rr.Body.String())
		}
		if strings.Contains(rr.Body.String(), "password") || !strings.Contains(rr.Body.String(), "Failed to initiate restart") {
			t.Errorf("body = %s", rr.Body.String())
		}
	})

	t.Run("unsupported keeps the sudo systemctl path", func(t *testing.T) {
		srv, _ := newTestServerWithStore(t)
		t.Setenv("PATH", t.TempDir()) // sudo cannot be found
		inst := newTestInstaller(maintenance.StrategyUnsupported)
		srv.config.MaintenanceConfig.Installer = inst

		rr := postRestart(t, srv)
		if rr.Code != http.StatusInternalServerError {
			t.Fatalf("response = %d %s", rr.Code, rr.Body.String())
		}
		if inst.restartCount() != 0 {
			t.Fatal("the unsupported installer must not be asked to restart")
		}
	})
}

// paramRecordingExecutor captures the parameters of an update-binary run.
type paramRecordingExecutor struct {
	mu     sync.Mutex
	params map[string]string
	ran    chan struct{}
}

func newParamRecordingExecutor() *paramRecordingExecutor {
	return &paramRecordingExecutor{ran: make(chan struct{}, 1)}
}

func (e *paramRecordingExecutor) Run(_ context.Context, _ io.Writer, p map[string]string) error {
	e.mu.Lock()
	e.params = p
	e.mu.Unlock()
	e.ran <- struct{}{}
	return nil
}

func releaseCheckResult(available bool) *maintenance.ReleaseUpdateCheckResult {
	return &maintenance.ReleaseUpdateCheckResult{
		Tier: "binary", UpdateAvailable: available, CurrentVersion: "nightly-20261020",
		LatestVersion: "nightly-20261023", Channel: "nightly", Class: "upgrade",
	}
}

type releaseCheckProbe struct {
	checks       int
	gotOpts      maintenance.ReleaseCheckOptions
	resolved     int
	executor     *paramRecordingExecutor
	checkResult  *maintenance.ReleaseUpdateCheckResult
	checkErr     error
	resolveError error
}

func (p *releaseCheckProbe) deps(running string) releaseCheckDeps {
	return releaseCheckDeps{
		running: running,
		check: func(_ context.Context, o maintenance.ReleaseCheckOptions) (*maintenance.ReleaseUpdateCheckResult, error) {
			p.checks++
			p.gotOpts = o
			return p.checkResult, p.checkErr
		},
		updateExecutor: func() (MaintenanceExecutor, error) {
			p.resolved++
			return p.executor, p.resolveError
		},
	}
}

func TestRunReleaseUpdateCheck(t *testing.T) {
	ctx := context.Background()

	t.Run("git mode is skipped", func(t *testing.T) {
		srv, _ := newTestServerWithStore(t)
		srv.config.MaintenanceConfig = MaintenanceConfig{DeploymentTier: "source", UpdatePolicy: "auto"}
		p := &releaseCheckProbe{checkResult: releaseCheckResult(true), executor: newParamRecordingExecutor()}
		srv.runReleaseUpdateCheck(ctx, p.deps("nightly-20261020"))
		if p.checks != 0 || p.resolved != 0 {
			t.Fatalf("git mode checked=%d resolved=%d", p.checks, p.resolved)
		}
	})

	t.Run("checks the effective channel", func(t *testing.T) {
		srv, _ := newTestServerWithStore(t)
		srv.config.MaintenanceConfig = MaintenanceConfig{DeploymentTier: "binary", UpdatePolicy: "notify",
			ReleaseChannel: "preview", GitHubRepo: "org/repo"}
		p := &releaseCheckProbe{checkResult: releaseCheckResult(false)}
		srv.runReleaseUpdateCheck(ctx, p.deps("nightly-20261023"))
		if p.gotOpts.Channel != "preview" || p.gotOpts.CurrentVersion != "nightly-20261023" || p.gotOpts.Repo != "org/repo" {
			t.Fatalf("check options = %+v", p.gotOpts)
		}
	})

	t.Run("git build without a channel is skipped", func(t *testing.T) {
		srv, _ := newTestServerWithStore(t)
		srv.config.MaintenanceConfig = MaintenanceConfig{DeploymentTier: "binary", UpdatePolicy: "notify"}
		p := &releaseCheckProbe{checkResult: releaseCheckResult(true)}
		srv.runReleaseUpdateCheck(ctx, p.deps(""))
		if p.checks != 0 {
			t.Fatal("a git build with no release_channel must not be checked")
		}
	})

	t.Run("notify stores the update", func(t *testing.T) {
		srv, s := newTestServerWithStore(t)
		srv.config.MaintenanceConfig = MaintenanceConfig{DeploymentTier: "binary", UpdatePolicy: "notify",
			Installer: newTestInstaller(maintenance.StrategySymlink)}
		p := &releaseCheckProbe{checkResult: releaseCheckResult(true), executor: newParamRecordingExecutor()}
		srv.runReleaseUpdateCheck(ctx, p.deps("nightly-20261020"))
		if _, err := s.GetHubSetting(ctx, HubSettingSectionUpdateAvailable); err != nil {
			t.Fatalf("update_available not stored: %v", err)
		}
		if runs, _ := s.ListMaintenanceRuns(ctx, "update-binary", 5); len(runs) != 0 || p.resolved != 0 {
			t.Fatalf("notify started a run: %+v", runs)
		}
	})

	t.Run("auto creates a latest:<channel> run", func(t *testing.T) {
		srv, s := newTestServerWithStore(t)
		srv.config.MaintenanceConfig = MaintenanceConfig{DeploymentTier: "binary", UpdatePolicy: "auto",
			Installer: newTestInstaller(maintenance.StrategySymlink)}
		exec := newParamRecordingExecutor()
		p := &releaseCheckProbe{checkResult: releaseCheckResult(true), executor: exec}
		srv.runReleaseUpdateCheck(ctx, p.deps("nightly-20261020"))

		select {
		case <-exec.ran:
		case <-time.After(5 * time.Second):
			t.Fatal("auto update executor never ran")
		}
		exec.mu.Lock()
		params := exec.params
		exec.mu.Unlock()
		if len(params) != 1 || params[maintenance.ParamTargetVersion] != "latest:nightly" {
			t.Fatalf("params = %v, want only target_version=latest:nightly", params)
		}
		runs, _ := s.ListMaintenanceRuns(ctx, "update-binary", 5)
		if len(runs) != 1 || runs[0].StartedBy != "system:auto-update" {
			t.Fatalf("runs = %+v", runs)
		}
	})

	t.Run("auto with an unsupported installer starts no run", func(t *testing.T) {
		srv, s := newTestServerWithStore(t)
		srv.config.MaintenanceConfig = MaintenanceConfig{DeploymentTier: "binary", UpdatePolicy: "auto",
			Installer: newTestInstaller(maintenance.StrategyUnsupported)}
		p := &releaseCheckProbe{checkResult: releaseCheckResult(true), executor: newParamRecordingExecutor()}
		srv.runReleaseUpdateCheck(ctx, p.deps("nightly-20261020"))
		if p.resolved != 0 {
			t.Fatal("the update executor was resolved for an unsupported installer")
		}
		if runs, _ := s.ListMaintenanceRuns(ctx, "update-binary", 5); len(runs) != 0 {
			t.Fatalf("runs = %+v", runs)
		}
		if _, err := s.GetHubSetting(ctx, HubSettingSectionUpdateAvailable); err != nil {
			t.Fatalf("the update must still be notified: %v", err)
		}
	})

	t.Run("auto skips while an update is in flight", func(t *testing.T) {
		srv, s := newTestServerWithStore(t)
		srv.config.MaintenanceConfig = MaintenanceConfig{DeploymentTier: "binary", UpdatePolicy: "auto",
			Installer: newTestInstaller(maintenance.StrategySymlink)}
		newRestartingRun(t, s)
		p := &releaseCheckProbe{checkResult: releaseCheckResult(true), executor: newParamRecordingExecutor()}
		srv.runReleaseUpdateCheck(ctx, p.deps("nightly-20261020"))
		if runs, _ := s.ListMaintenanceRuns(ctx, "update-binary", 5); len(runs) != 1 {
			t.Fatalf("runs = %+v", runs)
		}
	})

	t.Run("no update does nothing", func(t *testing.T) {
		srv, s := newTestServerWithStore(t)
		srv.config.MaintenanceConfig = MaintenanceConfig{DeploymentTier: "binary", UpdatePolicy: "auto",
			Installer: newTestInstaller(maintenance.StrategySymlink)}
		p := &releaseCheckProbe{checkResult: releaseCheckResult(false), executor: newParamRecordingExecutor()}
		srv.runReleaseUpdateCheck(ctx, p.deps("nightly-20261023"))
		if p.resolved != 0 {
			t.Fatal("executor resolved with no update")
		}
		if _, err := s.GetHubSetting(ctx, HubSettingSectionUpdateAvailable); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("update_available = %v", err)
		}
	})

	t.Run("auto with the real executor validates latest:<channel>", func(t *testing.T) {
		srv, s := newTestServerWithStore(t)
		srv.config.MaintenanceConfig = MaintenanceConfig{DeploymentTier: "binary", UpdatePolicy: "auto",
			ReleaseChannel: "nightly"}
		// A channel the executor does not know is refused before a run is
		// created.
		exec := maintenance.NewBinaryUpdateExecutor(maintenance.BinaryUpdateOptions{
			CurrentVersion: "nightly-20261020", Installer: newTestInstaller(maintenance.StrategySymlink),
		})
		srv.autoApplyUpdate(ctx, "beta", exec)
		if runs, _ := s.ListMaintenanceRuns(ctx, "update-binary", 5); len(runs) != 0 {
			t.Fatalf("a refused auto update created a run: %+v", runs)
		}
	})
}

// orderCheckingStore records, when a run is written as "restarting", whether
// system.pending_update already existed, and fails that write while its
// fault switch is armed.
type orderCheckingStore struct {
	store.Store
	fault *storeFaultSwitch

	mu             sync.Mutex
	pendingAtWrite *bool
}

func (o *orderCheckingStore) UpdateMaintenanceRun(ctx context.Context, run *store.MaintenanceOperationRun) error {
	if run.Status == store.MaintenanceStatusRestarting {
		_, err := o.Store.GetHubSetting(ctx, HubSettingSectionPendingUpdate)
		present := err == nil
		o.mu.Lock()
		if o.pendingAtWrite == nil {
			o.pendingAtWrite = &present
		}
		o.mu.Unlock()
		if o.fault.Active() {
			return errors.New("database is locked")
		}
	}
	return o.Store.UpdateMaintenanceRun(ctx, run)
}

func TestMarkRunRestarting_PendingUpdateWrittenFirst(t *testing.T) {
	srv, s := newTestServerWithStore(t)
	o, _ := installStoreFault(t, srv, func(inner store.Store, f *storeFaultSwitch) *orderCheckingStore {
		return &orderCheckingStore{Store: inner, fault: f}
	})
	run := newRun(t, s)
	srv.launchMaintenanceRun(run, &restartingExecutor{}, map[string]string{})
	waitForRestartRecorded(t, s, run.ID)
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.pendingAtWrite == nil || !*o.pendingAtWrite {
		t.Fatal("the run was written as restarting before system.pending_update existed")
	}
}

func TestMarkRunRestarting_RunWriteFailureDoesNotRestart(t *testing.T) {
	srv, s := newTestServerWithStore(t)
	_, fault := installStoreFault(t, srv, func(inner store.Store, f *storeFaultSwitch) *orderCheckingStore {
		return &orderCheckingStore{Store: inner, fault: f}
	})
	fault.Arm()
	run := newRun(t, s)
	srv.launchMaintenanceRun(run, &restartingExecutor{}, map[string]string{})
	got := waitForRunStatus(t, s, run.ID, func(r *store.MaintenanceOperationRun) bool {
		return r.Status == store.MaintenanceStatusFailed
	})
	if strings.Contains(got.Log, "restart requested") || !strings.Contains(got.Result, "record run as restarting") {
		t.Fatalf("run = %+v", got)
	}
	requireNoPendingUpdate(t, s)
}
