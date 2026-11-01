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

package hub

import (
	"context"
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
)

// Phase 4: rebuild-server (mode git) through the update lifecycle.

const (
	gitModeTestCommit  = "0123456789abcdef0123456789abcdef01234567"
	gitModeTestRelease = "git-0123456789ab"
)

func newRestartingRebuildRun(t *testing.T, s store.Store) *store.MaintenanceOperationRun {
	t.Helper()
	run := &store.MaintenanceOperationRun{
		ID: api.NewUUID(), OperationKey: "rebuild-server",
		Status: store.MaintenanceStatusRestarting, StartedAt: time.Now(), StartedBy: "admin@example.com",
		Result: `{"from":"nightly-20261023","to":"` + gitModeTestRelease + `","strategy":"sudo-install"}`,
	}
	if err := s.CreateMaintenanceRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	return run
}

func gitPendingUpdate(runID string) PendingUpdate {
	return PendingUpdate{
		RunID: runID, From: "nightly-20261023", To: gitModeTestRelease, Strategy: "sudo-install",
		At: "2026-11-01T03:00:00Z", SetBy: "admin@example.com",
		Mode: UpdateModeGit, Commit: gitModeTestCommit,
	}
}

func TestSelectionFromPending_GitMode(t *testing.T) {
	at := time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC)
	sel, skip := selectionFromPending(gitPendingUpdate("r1"), at)
	if skip != "" {
		t.Fatalf("a verified rebuild must record a selection, skipped: %s", skip)
	}
	want := ReleaseSelection{Mode: UpdateModeGit, Version: gitModeTestRelease,
		SetBy: "admin@example.com", SetAt: "2026-11-01T04:00:00Z", RunID: "r1"}
	if sel != want {
		t.Fatalf("selection = %+v, want %+v", sel, want)
	}
	mc := MaintenanceConfig{DeploymentTier: "binary", ReleaseChannel: "nightly"}
	if mode := effectiveUpdateMode(mc, &sel); mode != UpdateModeGit {
		t.Errorf("effective mode = %q, want git", mode)
	}
	// No channel or pin in git mode: the configured channel applies again
	// once the operator switches back to a release.
	if ch, src := effectiveReleaseChannel(mc, "", &sel); ch != "nightly" || src != ChannelSourceConfig {
		t.Errorf("effective channel = %q (%s), want the configured nightly", ch, src)
	}
	if pin := effectivePin(&sel); pin != maintenance.PinLatest {
		t.Errorf("pin = %q, want latest", pin)
	}

	// A pre-Phase-4 row (no mode) keeps the release rules.
	old := gitPendingUpdate("r2")
	old.Mode = ""
	if _, skip := selectionFromPending(old, at); skip == "" {
		t.Error("a release row without a channel must not record a selection")
	}
}

func TestReconcilePendingUpdate_GitRebuild(t *testing.T) {
	ctx := context.Background()

	t.Run("verified by the release name records git mode", func(t *testing.T) {
		srv, s := newTestServerWithStore(t)
		putReleaseSelection(t, s, ReleaseSelection{Mode: UpdateModeRelease, Channel: "nightly", Pin: "latest", Version: "nightly-20261023"})
		run := newRestartingRebuildRun(t, s)
		putPendingUpdate(t, s, gitPendingUpdate(run.ID))

		if err := srv.reconcilePendingUpdate(ctx, gitModeTestRelease); err != nil {
			t.Fatal(err)
		}
		got, _ := s.GetMaintenanceRun(ctx, run.ID)
		if got.Status != store.MaintenanceStatusCompleted {
			t.Fatalf("run = %+v, want completed", got)
		}
		if last := getLastUpdate(t, s); last == nil || last.Status != LastUpdateVerified || last.To != gitModeTestRelease || last.Mode != UpdateModeGit {
			t.Fatalf("last_update = %+v", last)
		}
		sel := getReleaseSelection(t, s)
		if sel == nil || sel.Mode != UpdateModeGit || sel.Version != gitModeTestRelease || sel.Channel != "" || sel.RunID != run.ID {
			t.Fatalf("selection = %+v, want git mode", sel)
		}
		requireNoPendingUpdate(t, s)
	})

	t.Run("another build keeps the old selection", func(t *testing.T) {
		srv, s := newTestServerWithStore(t)
		before := ReleaseSelection{Mode: UpdateModeRelease, Channel: "nightly", Pin: "latest", Version: "nightly-20261023"}
		putReleaseSelection(t, s, before)
		run := newRestartingRebuildRun(t, s)
		putPendingUpdate(t, s, gitPendingUpdate(run.ID))

		if err := srv.reconcilePendingUpdate(ctx, "nightly-20261023"); err != nil {
			t.Fatal(err)
		}
		got, _ := s.GetMaintenanceRun(ctx, run.ID)
		if got.Status != store.MaintenanceStatusFailed {
			t.Fatalf("run = %+v, want failed", got)
		}
		if sel := getReleaseSelection(t, s); sel == nil || *sel != before {
			t.Fatalf("selection = %+v, want unchanged %+v", sel, before)
		}
	})

	t.Run("git then release then git", func(t *testing.T) {
		srv, s := newTestServerWithStore(t)
		run := newRestartingRebuildRun(t, s)
		putPendingUpdate(t, s, gitPendingUpdate(run.ID))
		if err := srv.reconcilePendingUpdate(ctx, gitModeTestRelease); err != nil {
			t.Fatal(err)
		}

		upd := newRestartingRun(t, s)
		putPendingUpdate(t, s, PendingUpdate{RunID: upd.ID, From: gitModeTestRelease, To: "v0.3.0-preview.4",
			Strategy: "sudo-install", At: "2026-11-01T05:00:00Z", Channel: "preview", Pin: "latest",
			SetBy: "admin@example.com", Mode: UpdateModeRelease})
		if err := srv.reconcilePendingUpdate(ctx, "v0.3.0-preview.4"); err != nil {
			t.Fatal(err)
		}
		if sel := getReleaseSelection(t, s); sel == nil || sel.Mode != UpdateModeRelease || sel.Channel != "preview" {
			t.Fatalf("selection after the release update = %+v", sel)
		}

		back := newRestartingRebuildRun(t, s)
		p := gitPendingUpdate(back.ID)
		p.From = "v0.3.0-preview.4"
		putPendingUpdate(t, s, p)
		if err := srv.reconcilePendingUpdate(ctx, gitModeTestRelease); err != nil {
			t.Fatal(err)
		}
		if sel := getReleaseSelection(t, s); sel == nil || sel.Mode != UpdateModeGit {
			t.Fatalf("selection after the rebuild = %+v", sel)
		}
	})

	t.Run("stale restarting rebuild run is failed", func(t *testing.T) {
		srv, s := newTestServerWithStore(t)
		run := newRestartingRebuildRun(t, s)
		if err := srv.reconcilePendingUpdate(ctx, gitModeTestRelease); err != nil {
			t.Fatal(err)
		}
		got, _ := s.GetMaintenanceRun(ctx, run.ID)
		if got.Status != store.MaintenanceStatusFailed || got.CompletedAt == nil {
			t.Fatalf("run = %+v, want failed", got)
		}
	})
}

// gitRestartingExecutor reports a rebuild from Git to the before-restart
// hook.
type gitRestartingExecutor struct{}

func (e gitRestartingExecutor) Run(ctx context.Context, w io.Writer, p map[string]string) error {
	return e.RunWithBeforeRestart(ctx, w, p, nil)
}

func (gitRestartingExecutor) RunWithBeforeRestart(ctx context.Context, w io.Writer, _ map[string]string, before maintenance.BeforeRestartFunc) error {
	_, _ = fmt.Fprintln(w, "installed "+gitModeTestRelease)
	if before != nil {
		return before(ctx, maintenance.PendingRestart{From: "nightly-20261023", To: gitModeTestRelease,
			Strategy: "sudo-install", Mode: maintenance.UpdateModeGit, Commit: gitModeTestCommit})
	}
	return nil
}

func TestLaunchMaintenanceRun_RebuildRecordsGitPendingUpdate(t *testing.T) {
	srv, s := newTestServerWithStore(t)
	run := &store.MaintenanceOperationRun{
		ID: api.NewUUID(), OperationKey: "rebuild-server",
		Status: store.MaintenanceStatusRunning, StartedAt: time.Now(), StartedBy: "admin@example.com",
	}
	if err := s.CreateMaintenanceRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	srv.launchMaintenanceRun(run, gitRestartingExecutor{}, map[string]string{})
	got := waitForRestartRecorded(t, s, run.ID)
	if got.Status != store.MaintenanceStatusRestarting || !strings.Contains(got.Result, `"commit":"`+gitModeTestCommit+`"`) {
		t.Fatalf("run = %+v, want restarting with the commit", got)
	}
	var p PendingUpdate
	if found, err := srv.readHubSettingJSON(context.Background(), HubSettingSectionPendingUpdate, &p); err != nil || !found {
		t.Fatalf("pending_update: found=%v err=%v", found, err)
	}
	if p.Mode != UpdateModeGit || p.Commit != gitModeTestCommit || p.To != gitModeTestRelease || p.SetBy != "admin@example.com" {
		t.Fatalf("pending_update = %+v", p)
	}
}

func postCheckUpdates(t *testing.T, srv *Server, query string) *httptest.ResponseRecorder {
	t.Helper()
	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/maintenance/check-updates"+query, nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleCheckForUpdates(rr, req)
	return rr
}

func TestCheckForUpdates_ModeParam(t *testing.T) {
	t.Run("invalid mode", func(t *testing.T) {
		srv, _ := newTestServerWithStore(t)
		rr := postCheckUpdates(t, srv, "?mode=both")
		if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "invalid mode") {
			t.Fatalf("got %d: %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("git on a binary-tier hub checks the repo", func(t *testing.T) {
		srv, _ := newTestServerWithStore(t)
		cat := &fakeReleaseCatalog{err: errors.New("must not be asked")}
		srv.config.MaintenanceConfig = MaintenanceConfig{DeploymentTier: "binary", ReleaseCatalog: cat}
		rr := postCheckUpdates(t, srv, "?mode=git")
		// No repo path: the source checker's error, not the catalog's.
		if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "SCION_MAINTENANCE_REPO_PATH") {
			t.Fatalf("got %d: %s", rr.Code, rr.Body.String())
		}
		if cat.gets != 0 {
			t.Fatalf("catalog asked %d times for a git check", cat.gets)
		}
	})

	t.Run("release on a source-tier hub checks the catalog", func(t *testing.T) {
		srv, _ := newTestServerWithStore(t)
		cat := &fakeReleaseCatalog{err: errors.New("github down")}
		srv.config.MaintenanceConfig = MaintenanceConfig{DeploymentTier: "source", ReleaseChannel: "nightly", ReleaseCatalog: cat}
		rr := postCheckUpdates(t, srv, "?mode=release")
		if rr.Code != http.StatusInternalServerError || !strings.Contains(rr.Body.String(), "Failed to check for release updates") {
			t.Fatalf("got %d: %s", rr.Code, rr.Body.String())
		}
		if cat.gets == 0 {
			t.Fatal("catalog not asked for a release check")
		}
	})

	t.Run("no mode keeps the tier", func(t *testing.T) {
		srv, _ := newTestServerWithStore(t)
		srv.config.MaintenanceConfig = MaintenanceConfig{DeploymentTier: "source"}
		rr := postCheckUpdates(t, srv, "")
		if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "SCION_MAINTENANCE_REPO_PATH") {
			t.Fatalf("got %d: %s", rr.Code, rr.Body.String())
		}
	})
}
