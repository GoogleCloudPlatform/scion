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
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/maintenance"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/version"
)

// sudoRunner is a maintenance.CommandRunner for Detect and the sudo-install
// installer. `sudo -n -l <argv>` succeeds only for argv in allowed; Start
// runs startCmd (a real shell command, so its exit result is a real
// *exec.ExitError) in place of the requested command. Nothing runs sudo.
type sudoRunner struct {
	allowed  map[string]bool
	startCmd string
}

func (r *sudoRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	if name == "sudo" && len(args) > 2 && args[0] == "-n" && args[1] == "-l" && r.allowed[strings.Join(args[2:], " ")] {
		return nil, nil
	}
	return []byte("sudo: a password is required"), errors.New("exit status 1")
}

func (r *sudoRunner) Start(string, ...string) (<-chan error, error) {
	cmd := exec.Command("sh", "-c", r.startCmd)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	return done, nil
}

// detectForTest runs maintenance.Detect for a host whose binary is not in
// the install root, with repoPath as the maintenance repo and runner for
// sudo.
func detectForTest(t *testing.T, repoPath string, runner maintenance.CommandRunner) maintenance.Installer {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return maintenance.Detect(ctx, maintenance.DetectOptions{
		InstallRoot: filepath.Join(t.TempDir(), "opt-scion"),
		RepoPath:    repoPath,
		BinaryDest:  "/usr/local/bin/scion",
		ServiceName: "scion-hub",
		Executable:  func() (string, error) { return "/usr/local/bin/scion", nil },
		Runner:      runner,
	})
}

// R1: the installer's reason is built from host paths and sudo argv; the
// maintenance repo path must not reach update-status through it.
func TestBuildUpdateStatus_RedactsRepoPathFromInstaller(t *testing.T) {
	ctx := context.Background()
	release := runningBuild{Version: "nightly-20261023"}

	t.Run("reason and hint naming the repo path", func(t *testing.T) {
		repo := "/home/scion/private-checkout"
		srv, _ := newTestServerWithStore(t)
		inst := newTestInstaller(maintenance.StrategyUnsupported)
		inst.status.Reason = fmt.Sprintf("sudo-install: no sudoers rule allows `install -m 755 %s/scion.rebuild /usr/local/bin/scion`; maintenance repo path %s/ is not writable", repo, repo)
		inst.status.MigrateHint = "fix " + repo
		srv.config.MaintenanceConfig = MaintenanceConfig{RepoPath: repo + "/", DeploymentTier: "binary", Installer: inst}

		st, err := srv.buildUpdateStatus(ctx, release)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(st)
		if strings.Contains(string(b), repo) {
			t.Fatalf("update-status leaks the repo path: %s", b)
		}
		if !strings.Contains(st.Installer.Reason, repoPathPlaceholder+"/scion.rebuild") || !st.Mode.RepoConfigured {
			t.Errorf("reason = %q, repoConfigured = %v", st.Installer.Reason, st.Mode.RepoConfigured)
		}
		if inst.status.Reason == st.Installer.Reason {
			t.Error("the installer's own status was not copied before redaction")
		}
	})

	t.Run("real detection with a repo path and no sudo", func(t *testing.T) {
		repo := t.TempDir()
		srv, _ := newTestServerWithStore(t)
		inst := detectForTest(t, repo, &sudoRunner{})
		if runtime.GOOS == "linux" && !strings.Contains(inst.Status().Reason, repo) {
			t.Fatalf("precondition: detection reason should name the repo path, got %q", inst.Status().Reason)
		}
		srv.config.MaintenanceConfig = MaintenanceConfig{RepoPath: repo, DeploymentTier: "binary", Installer: inst}
		body := getUpdateStatus(t, srv)
		b, _ := json.Marshal(body)
		if strings.Contains(string(b), repo) {
			t.Fatalf("update-status leaks the repo path: %s", b)
		}
	})
}

func TestRedactInstallerStatus(t *testing.T) {
	st := maintenance.InstallerStatus{Strategy: "unsupported", Reason: "path /srv/repo is bad; /srv/repo/scion.rebuild", MigrateHint: "see /srv/repo"}
	got := redactInstallerStatus(st, "/srv/repo/")
	if strings.Contains(got.Reason+got.MigrateHint, "/srv/repo") {
		t.Fatalf("not redacted: %+v", got)
	}
	if got.Reason != "path <maintenance repo> is bad; <maintenance repo>/scion.rebuild" || got.Strategy != "unsupported" {
		t.Errorf("redacted = %+v", got)
	}
	if same := redactInstallerStatus(st, ""); same != st {
		t.Errorf("no repo path must leave the status alone: %+v", same)
	}

	t.Run("raw non-canonical spelling", func(t *testing.T) {
		raw := maintenance.InstallerStatus{Reason: "maintenance repo path /srv//repo is not writable; `install -m 755 /srv/repo/scion.rebuild /usr/local/bin/scion`"}
		got := redactInstallerStatus(raw, "/srv//repo")
		if strings.Contains(got.Reason, "/srv//repo") || strings.Contains(got.Reason, "/srv/repo") {
			t.Fatalf("not redacted: %q", got.Reason)
		}
	})

	t.Run("whole path segments only", func(t *testing.T) {
		other := maintenance.InstallerStatus{Reason: "running binary /srv/repository/scion is not under /opt/scion/releases"}
		if got := redactInstallerStatus(other, "/srv/repo"); got != other {
			t.Fatalf("a different path was touched: %q", got.Reason)
		}
	})

	t.Run("relative repo path is left alone", func(t *testing.T) {
		rel := maintenance.InstallerStatus{Reason: "running binary /usr/local/bin/scion is not under /opt/scion/releases"}
		if got := redactInstallerStatus(rel, "scion"); got != rel {
			t.Fatalf("relative path over-matched: %q", got.Reason)
		}
	})

	t.Run("symlink-resolved repo and resolved executable", func(t *testing.T) {
		base := t.TempDir()
		realDir := filepath.Join(base, "data", "scion")
		if err := os.MkdirAll(realDir, 0o755); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(base, "home-scion")
		if err := os.Symlink(realDir, link); err != nil {
			t.Fatal(err)
		}
		resolved, err := filepath.EvalSymlinks(realDir)
		if err != nil {
			t.Fatal(err)
		}
		in := maintenance.InstallerStatus{Reason: fmt.Sprintf(
			"symlink layout: running binary %s/scion is not under /opt/scion/releases; sudo-install: maintenance repo path %s is not writable", resolved, link)}
		got := redactInstallerStatus(in, link)
		if strings.Contains(got.Reason, resolved) || strings.Contains(got.Reason, link) {
			t.Fatalf("not redacted: %q", got.Reason)
		}
		if !strings.Contains(got.Reason, "running binary <maintenance repo>/scion") {
			t.Errorf("reason = %q", got.Reason)
		}
	})
}

// Boot reconciliation clears the update-available notification only when the
// update is verified; a failed update keeps it for a retry.
func TestReconcilePendingUpdate_UpdateAvailable(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		running   string
		wantClear bool
	}{
		{"nightly-20261024", true},
		{"nightly-20261023", false},
	} {
		t.Run(tt.running, func(t *testing.T) {
			srv, s := newTestServerWithStore(t)
			if _, err := s.UpsertHubSetting(ctx, HubSettingSectionUpdateAvailable, json.RawMessage(`{"version":"nightly-20261024"}`), "test", -1, "managed"); err != nil {
				t.Fatal(err)
			}
			run := newRestartingRun(t, s)
			putPendingUpdate(t, s, PendingUpdate{RunID: run.ID, From: "nightly-20261023", To: "nightly-20261024"})
			if err := srv.reconcilePendingUpdate(ctx, tt.running); err != nil {
				t.Fatal(err)
			}
			_, err := s.GetHubSetting(ctx, HubSettingSectionUpdateAvailable)
			cleared := errors.Is(err, store.ErrNotFound)
			if cleared != tt.wantClear {
				t.Fatalf("update_available cleared = %v (err %v), want %v", cleared, err, tt.wantClear)
			}
		})
	}
}

// installerRestartExecutor imitates update-binary's last steps on a real
// Installer: the before-restart hook, then Installer.Restart.
type installerRestartExecutor struct {
	inst       maintenance.Installer
	restartErr chan error
}

func (e *installerRestartExecutor) Run(ctx context.Context, w io.Writer, p map[string]string) error {
	return e.RunWithBeforeRestart(ctx, w, p, nil)
}

func (e *installerRestartExecutor) RunWithBeforeRestart(ctx context.Context, _ io.Writer, _ map[string]string, before maintenance.BeforeRestartFunc) error {
	if err := before(ctx, maintenance.PendingRestart{From: "nightly-20261023", To: "nightly-20261024", Strategy: e.inst.Name()}); err != nil {
		return err
	}
	err := e.inst.Restart(ctx)
	e.restartErr <- err
	return err
}

// L1: with only the plain restart rule, systemd's stop SIGTERMs the
// sudo/systemctl child together with the hub. That is a restart under way:
// the run stays restarting and pending_update is kept for reconciliation.
// A normal non-zero exit (no restart happened) still fails the run and
// clears pending_update.
func TestUpdateRun_PlainRestartChildExit(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the sudo-install installer is Linux only")
	}
	tests := []struct {
		name       string
		startCmd   string
		wantFailed bool
	}{
		{"killed by SIGTERM", "kill -TERM $$", false},
		{"exit status 1", "exit 1", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := t.TempDir()
			runner := &sudoRunner{allowed: map[string]bool{
				"install -m 755 " + filepath.Join(repo, maintenance.SudoStagingName) + " /usr/local/bin/scion": true,
				"systemctl restart scion-hub": true, // the plain rule only, no --no-block
			}, startCmd: tt.startCmd}
			inst := detectForTest(t, repo, runner)
			if inst.Name() != maintenance.StrategySudoInstall {
				t.Fatalf("precondition: detected %q (%s), want sudo-install", inst.Name(), inst.Status().Reason)
			}

			srv, s := newTestServerWithStore(t)
			run := newRun(t, s)
			ex := &installerRestartExecutor{inst: inst, restartErr: make(chan error, 1)}
			srv.launchMaintenanceRun(run, ex, map[string]string{})

			var restartErr error
			select {
			case restartErr = <-ex.restartErr:
			case <-time.After(20 * time.Second):
				t.Fatal("installer restart did not return")
			}
			if tt.wantFailed {
				if restartErr == nil {
					t.Fatal("a plain restart that exited 1 must fail")
				}
				waitForRunStatus(t, s, run.ID, func(r *store.MaintenanceOperationRun) bool {
					return r.Status == store.MaintenanceStatusFailed
				})
				requireNoPendingUpdate(t, s)
				return
			}
			if restartErr != nil {
				t.Fatalf("a signal-killed plain restart is under way, got %v", restartErr)
			}
			// The runner writes nothing after a successful hook; the run
			// stays restarting with its pending update.
			got := waitForRestartRecorded(t, s, run.ID)
			time.Sleep(200 * time.Millisecond)
			got, _ = s.GetMaintenanceRun(context.Background(), got.ID)
			if got.Status != store.MaintenanceStatusRestarting {
				t.Fatalf("status = %q, want restarting", got.Status)
			}
			if _, err := s.GetHubSetting(context.Background(), HubSettingSectionPendingUpdate); err != nil {
				t.Fatalf("pending_update cleared: %v", err)
			}
		})
	}
}

// L1: Restart Hub on a sudo-install host whose plain restart child is killed
// by the unit's stop answers 200, not 500.
func TestHandleAdminRestart_SignalKilledPlainRestart(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the sudo-install installer is Linux only")
	}
	repo := t.TempDir()
	inst := detectForTest(t, repo, &sudoRunner{allowed: map[string]bool{
		"install -m 755 " + filepath.Join(repo, maintenance.SudoStagingName) + " /usr/local/bin/scion": true,
		"systemctl restart scion-hub": true,
	}, startCmd: "kill -TERM $$"})
	if inst.Name() != maintenance.StrategySudoInstall {
		t.Fatalf("precondition: detected %q (%s)", inst.Name(), inst.Status().Reason)
	}
	srv, _ := newTestServerWithStore(t)
	srv.config.MaintenanceConfig.Installer = inst
	if rr := postRestart(t, srv); rr.Code != http.StatusOK {
		t.Fatalf("response = %d %s", rr.Code, rr.Body.String())
	}
}

// L2: pending_update is written before the run's restarting status, so a
// crash between the two leaves a pending update whose run is "running".
func TestReconcilePendingUpdate_RunStillRunning(t *testing.T) {
	ctx := context.Background()
	srv, s := newTestServerWithStore(t)
	run := newRun(t, s)
	putPendingUpdate(t, s, PendingUpdate{RunID: run.ID, From: "nightly-20261023", To: "nightly-20261024"})
	if err := srv.reconcilePendingUpdate(ctx, "nightly-20261024"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetMaintenanceRun(ctx, run.ID); got.Status != store.MaintenanceStatusCompleted {
		t.Fatalf("run = %+v, want completed", got)
	}
	if last := getLastUpdate(t, s); last == nil || last.Status != LastUpdateVerified {
		t.Fatalf("last_update = %+v", last)
	}
}

// L2: New() reconciles the pending update before the abort sweep. Were the
// order reversed, the "running" run would be aborted first and the pending
// update could no longer complete it.
func TestNew_ReconcilesPendingUpdateBeforeAbortSweep(t *testing.T) {
	prev := version.Version
	version.Version = "nightly-20261024"
	t.Cleanup(func() { version.Version = prev })

	s, err := newTestStore(t, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	running := newRun(t, s)
	restarting := newRestartingRun(t, s)
	putPendingUpdate(t, s, PendingUpdate{RunID: running.ID, From: "nightly-20261023", To: "nightly-20261024"})
	other := &store.MaintenanceOperationRun{
		ID: api.NewUUID(), OperationKey: "pull-images",
		Status: store.MaintenanceStatusRunning, StartedAt: time.Now(),
	}
	if err := s.CreateMaintenanceRun(ctx, other); err != nil {
		t.Fatal(err)
	}

	testServerWithStore(t, s) // runs New()

	if got, _ := s.GetMaintenanceRun(ctx, running.ID); got.Status != store.MaintenanceStatusCompleted {
		t.Fatalf("pending run = %q, want completed", got.Status)
	}
	if last := getLastUpdate(t, s); last == nil || last.Status != LastUpdateVerified || last.RunID != running.ID {
		t.Fatalf("last_update = %+v", last)
	}
	requireNoPendingUpdate(t, s)
	if got, _ := s.GetMaintenanceRun(ctx, restarting.ID); got.Status != store.MaintenanceStatusFailed {
		t.Fatalf("stale restarting run = %q, want failed", got.Status)
	}
	if got, _ := s.GetMaintenanceRun(ctx, other.ID); got.Status != store.MaintenanceStatusFailed {
		t.Fatalf("interrupted running run = %q, want failed by the abort sweep", got.Status)
	}
}

// panickingExecutor panics after the before-restart hook succeeded.
type panickingExecutor struct{}

func (panickingExecutor) Run(ctx context.Context, w io.Writer, p map[string]string) error {
	return panickingExecutor{}.RunWithBeforeRestart(ctx, w, p, nil)
}

func (panickingExecutor) RunWithBeforeRestart(ctx context.Context, _ io.Writer, _ map[string]string, before maintenance.BeforeRestartFunc) error {
	if err := before(ctx, maintenance.PendingRestart{From: "nightly-20261023", To: "nightly-20261024", Strategy: "symlink"}); err != nil {
		return err
	}
	panic("boom after the hook")
}

// L3: a panic after the hook fails the run and removes its pending update,
// so no stale pending update (or a later "verified" last_update) remains.
func TestLaunchMaintenanceRun_PanicAfterHookClearsPendingUpdate(t *testing.T) {
	srv, s := newTestServerWithStore(t)
	run := newRun(t, s)
	srv.launchMaintenanceRun(run, panickingExecutor{}, map[string]string{})
	got := waitForRunStatus(t, s, run.ID, func(r *store.MaintenanceOperationRun) bool {
		return r.Status == store.MaintenanceStatusFailed
	})
	if !strings.Contains(got.Result, "executor panic") {
		t.Fatalf("result = %s", got.Result)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := s.GetHubSetting(context.Background(), HubSettingSectionPendingUpdate); errors.Is(err, store.ErrNotFound) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pending_update left behind after a panic")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
