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
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/maintenance"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
	"github.com/GoogleCloudPlatform/scion/pkg/version"
	"github.com/GoogleCloudPlatform/scion/pkg/version/update"
)

// downgradeTestSnapshotter is a maintenance.DBSnapshotter for hub tests.
type downgradeTestSnapshotter struct{ unsupported string }

func (d downgradeTestSnapshotter) SnapshotStatus() maintenance.DBSnapshotStatus {
	if d.unsupported != "" {
		return maintenance.DBSnapshotStatus{Reason: d.unsupported}
	}
	return maintenance.DBSnapshotStatus{Supported: true, Dir: "/opt/scion/snapshots"}
}

func (d downgradeTestSnapshotter) Snapshot(context.Context, string, io.Writer) (string, error) {
	return "/opt/scion/snapshots/hub-test.db", nil
}

// dialectOverrideStore reports another SQL dialect for the store's
// database handle (storeDB), to stand in for a PostgreSQL hub.
type dialectOverrideStore struct {
	store.Store
	db      *sql.DB
	dialect string
}

func (d dialectOverrideStore) DB() *sql.DB     { return d.db }
func (d dialectOverrideStore) Dialect() string { return d.dialect }

// downgradeAPIError is the standard API error envelope with details.
type downgradeAPIError struct {
	Error struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	} `json:"error"`
}

// newPostgresLikeServer returns a server over a fresh SQLite store whose
// database handle reports the PostgreSQL dialect, with mc as its
// maintenance config.
func newPostgresLikeServer(t *testing.T, mc MaintenanceConfig) *Server {
	t.Helper()
	base, s := newTestServerWithStore(t)
	db, _ := base.storeDB()
	pg := &Server{
		store:          dialectOverrideStore{Store: s, db: db, dialect: "postgres"},
		maintenanceLog: logging.Subsystem("hub.maintenance"),
	}
	pg.config.MaintenanceConfig = mc
	return pg
}

func setRunningVersion(t *testing.T, v string) {
	t.Helper()
	prev := version.Version
	version.Version = v
	t.Cleanup(func() { version.Version = prev })
}

func TestExecuteOperation_DowngradeContract(t *testing.T) {
	setRunningVersion(t, "nightly-20261023")
	srv, s := newTestServerWithStore(t)
	srv.config.MaintenanceConfig = MaintenanceConfig{
		DeploymentTier: "binary",
		DBSnapshotter:  downgradeTestSnapshotter{},
		ReleaseSource: &countingReleaseSource{rels: map[string]time.Time{
			"nightly-20261020": time.Date(2026, 10, 20, 4, 0, 0, 0, time.UTC),
			"nightly-20261023": time.Date(2026, 10, 23, 4, 0, 0, 0, time.UTC),
			"v0.3.0-preview.4": time.Date(2026, 10, 22, 18, 0, 0, 0, time.UTC),
			"v0.3.0-preview.1": time.Date(2026, 10, 1, 18, 0, 0, 0, time.UTC),
		}},
	}

	tests := []struct {
		name, body string
		status     int
		code       string
		class      string // details.class of a 409 confirmation refusal
	}{
		{"missing confirmation", `{"params":{"target_version":"nightly-20261020"}}`, http.StatusConflict,
			maintenance.CodeDowngradeRequiresConfirmation, "downgrade"},
		{"mismatched confirmation", `{"params":{"target_version":"nightly-20261020","confirm_downgrade":"nightly-20261021"}}`,
			http.StatusConflict, maintenance.CodeDowngradeRequiresConfirmation, "downgrade"},
		{"cross-channel older, answered before a run", `{"params":{"target_version":"v0.3.0-preview.4"}}`,
			http.StatusConflict, maintenance.CodeDowngradeRequiresConfirmation, "switch-older"},
		{"below the floor", `{"params":{"target_version":"nightly-20261008","confirm_downgrade":"nightly-20261008"}}`,
			http.StatusConflict, maintenance.CodeBelowDowngradeFloor, ""},
		{"cross-channel below the floor", `{"params":{"target_version":"v0.3.0-preview.1","confirm_downgrade":"v0.3.0-preview.1"}}`,
			http.StatusConflict, maintenance.CodeBelowDowngradeFloor, ""},
		{"confirmation is not a tag", `{"params":{"target_version":"nightly-20261020","confirm_downgrade":"yes"}}`,
			http.StatusBadRequest, maintenance.CodeInvalidParam, ""},
		{"unknown param is still refused", `{"params":{"target_version":"nightly-20261020","confirm_downgrade":"nightly-20261020","force":"1"}}`,
			http.StatusBadRequest, maintenance.CodeUnknownParam, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rr := postOperationRun(t, srv, "update-binary", tt.body)
			if rr.Code != tt.status {
				t.Fatalf("status = %d, want %d: %s", rr.Code, tt.status, rr.Body.String())
			}
			var e downgradeAPIError
			if err := json.Unmarshal(rr.Body.Bytes(), &e); err != nil {
				t.Fatal(err)
			}
			if e.Error.Code != tt.code {
				t.Fatalf("code = %q, want %q: %s", e.Error.Code, tt.code, rr.Body.String())
			}
			if tt.class != "" {
				risks, _ := e.Error.Details["risks"].([]any)
				if len(risks) < 5 || e.Error.Details["class"] != tt.class ||
					e.Error.Details["snapshotPath"] != "/opt/scion/snapshots" ||
					e.Error.Details["floor"] != version.DowngradeFloor {
					t.Fatalf("details = %#v", e.Error.Details)
				}
			}
			if tt.code == maintenance.CodeBelowDowngradeFloor && e.Error.Details["floor"] != version.DowngradeFloor {
				t.Fatalf("details = %#v", e.Error.Details)
			}
			runs, err := s.ListMaintenanceRuns(context.Background(), "update-binary", 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(runs) != 0 {
				t.Fatalf("a run was created for a refused request: %+v", runs)
			}
		})
	}

	t.Run("correct confirmation starts a run", func(t *testing.T) {
		// No installer is configured, so the run fails at preflight,
		// before any network access.
		rr := postOperationRun(t, srv, "update-binary",
			`{"params":{"target_version":"nightly-20261020","confirm_downgrade":"nightly-20261020"}}`)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
		}
		var started struct {
			RunID string `json:"runId"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &started); err != nil || started.RunID == "" {
			t.Fatalf("body = %s", rr.Body.String())
		}
		waitForRunStatus(t, s, started.RunID, func(r *store.MaintenanceOperationRun) bool {
			return r.Status == store.MaintenanceStatusFailed
		})
	})

	t.Run("PostgreSQL refuses downgrades", func(t *testing.T) {
		mc := srv.config.MaintenanceConfig
		mc.DBSnapshotter = nil
		pg := newPostgresLikeServer(t, mc)
		rr := postOperationRun(t, pg, "update-binary",
			`{"params":{"target_version":"nightly-20261020","confirm_downgrade":"nightly-20261020"}}`)
		if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), maintenance.CodeDowngradeUnsupportedDriver) ||
			!strings.Contains(rr.Body.String(), "PostgreSQL") {
			t.Fatalf("response = %d %s", rr.Code, rr.Body.String())
		}
	})
}

func TestUpdateSnapshotter(t *testing.T) {
	ctx := context.Background()

	t.Run("in-memory SQLite is unsupported", func(t *testing.T) {
		srv, _ := newTestServerWithStore(t)
		st := srv.updateSnapshotter(ctx).SnapshotStatus()
		if st.Supported || !strings.Contains(st.Reason, "in-memory") {
			t.Fatalf("status = %+v", st)
		}
	})

	t.Run("PostgreSQL is unsupported", func(t *testing.T) {
		st := newPostgresLikeServer(t, MaintenanceConfig{}).updateSnapshotter(ctx).SnapshotStatus()
		if st.Supported || !strings.Contains(st.Reason, "PostgreSQL") {
			t.Fatalf("status = %+v", st)
		}
	})

	t.Run("file-backed SQLite snapshots by installer strategy", func(t *testing.T) {
		dbFile := filepath.Join(t.TempDir(), "hub.db")
		s, err := newTestStore(t, dbFile)
		if err != nil {
			t.Fatal(err)
		}
		srv := &Server{store: s, maintenanceLog: logging.Subsystem("hub.maintenance")}
		root := t.TempDir()
		srv.config.MaintenanceConfig = MaintenanceConfig{InstallRoot: root,
			Installer: newTestInstaller(maintenance.StrategySymlink)}
		st := srv.updateSnapshotter(ctx).SnapshotStatus()
		if !st.Supported || st.Dir != filepath.Join(root, "snapshots") {
			t.Fatalf("symlink: status = %+v", st)
		}
		srv.config.MaintenanceConfig.Installer = newTestInstaller(maintenance.StrategySudoInstall)
		st = srv.updateSnapshotter(ctx).SnapshotStatus()
		if !st.Supported || st.Dir != filepath.Join(filepath.Dir(dbFile), "update-snapshots") {
			t.Fatalf("sudo-install: status = %+v", st)
		}
	})

	t.Run("the configured snapshotter wins", func(t *testing.T) {
		srv, _ := newTestServerWithStore(t)
		srv.config.MaintenanceConfig.DBSnapshotter = downgradeTestSnapshotter{}
		if !srv.updateSnapshotter(ctx).SnapshotStatus().Supported {
			t.Fatal("configured snapshotter ignored")
		}
	})
}

func TestUpdateSnapshotDir(t *testing.T) {
	mc := MaintenanceConfig{InstallRoot: "/opt/scion"}
	if got := updateSnapshotDir(mc, maintenance.StrategySymlink, "/home/scion/.scion/hub.db"); got != filepath.Join("/opt/scion", "snapshots") {
		t.Errorf("symlink = %q", got)
	}
	if got := updateSnapshotDir(mc, maintenance.StrategySudoInstall, "/home/scion/.scion/hub.db"); got != filepath.Join("/home/scion/.scion", "update-snapshots") {
		t.Errorf("sudo-install = %q", got)
	}
	if got := updateSnapshotDir(MaintenanceConfig{}, maintenance.StrategySymlink, "/data/hub.db"); got != filepath.Join("/data", "update-snapshots") {
		t.Errorf("symlink without install root = %q", got)
	}
}

func TestBuildUpdateStatus_Downgrade(t *testing.T) {
	ctx := context.Background()
	srv, _ := newTestServerWithStore(t)
	srv.config.MaintenanceConfig.DBSnapshotter = downgradeTestSnapshotter{}
	resp, err := srv.buildUpdateStatus(ctx, runningBuild{Version: "nightly-20261023"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Downgrade.Floor != version.DowngradeFloor || !resp.Downgrade.SnapshotsSupported {
		t.Fatalf("downgrade = %+v", resp.Downgrade)
	}

	resp, err = newPostgresLikeServer(t, MaintenanceConfig{}).buildUpdateStatus(ctx, runningBuild{Version: "nightly-20261023"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Downgrade.SnapshotsSupported {
		t.Fatal("snapshotsSupported must be false on PostgreSQL")
	}

	// An in-memory SQLite hub cannot snapshot either.
	srv.config.MaintenanceConfig.DBSnapshotter = nil
	body := getUpdateStatus(t, srv)
	dg, ok := body["downgrade"].(map[string]any)
	if !ok || dg["floor"] != version.DowngradeFloor || dg["snapshotsSupported"] != false {
		t.Fatalf("downgrade = %#v", body["downgrade"])
	}
}

func writeRollbackMarker(t *testing.T, root, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, rollbackMarkerName), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadRollbackMarker(t *testing.T) {
	const valid = `{"at":"2026-10-24T03:10:00Z","from":"nightly-20261024","to":"nightly-20261023"}` + "\n"

	t.Run("missing", func(t *testing.T) {
		if m, err := readRollbackMarker(t.TempDir()); m != nil || err != nil {
			t.Fatalf("m = %+v, err = %v", m, err)
		}
		if m, err := readRollbackMarker(""); m != nil || err != nil {
			t.Fatalf("no install root: m = %+v, err = %v", m, err)
		}
	})

	t.Run("valid", func(t *testing.T) {
		root := t.TempDir()
		writeRollbackMarker(t, root, valid)
		m, err := readRollbackMarker(root)
		if err != nil || m == nil || *m != (RollbackMarker{At: "2026-10-24T03:10:00Z", From: "nightly-20261024", To: "nightly-20261023"}) {
			t.Fatalf("m = %+v, err = %v", m, err)
		}
		if _, err := os.Stat(filepath.Join(root, rollbackMarkerName)); err != nil {
			t.Fatal("the marker was removed")
		}
	})

	for name, content := range map[string]string{
		"malformed":   "{not json",
		"no at":       `{"from":"a","to":"b"}`,
		"bad at":      `{"at":"yesterday","from":"a","to":"b"}`,
		"no from":     `{"at":"2026-10-24T03:10:00Z","to":"b"}`,
		"no to":       `{"at":"2026-10-24T03:10:00Z","from":"a"}`,
		"oversized":   `{"at":"2026-10-24T03:10:00Z","from":"a","to":"b","pad":"` + strings.Repeat("x", maxRollbackMarkerSize) + `"}`,
		"two objects": valid + valid,
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeRollbackMarker(t, root, content)
			if m, err := readRollbackMarker(root); err == nil {
				t.Fatalf("m = %+v, want an error", m)
			}
		})
	}

	t.Run("a directory is refused", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, rollbackMarkerName), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := readRollbackMarker(root); err == nil {
			t.Fatal("want an error")
		}
	})

	t.Run("a symlink is not followed", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("symlinks need privileges on Windows")
		}
		root := t.TempDir()
		target := filepath.Join(t.TempDir(), "elsewhere")
		if err := os.WriteFile(target, []byte(valid), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(root, rollbackMarkerName)); err != nil {
			t.Fatal(err)
		}
		if m, err := readRollbackMarker(root); err == nil || m != nil {
			t.Fatalf("m = %+v, err = %v; want a refusal", m, err)
		}
	})
}

func getSeenRollbackMarker(t *testing.T, s store.Store) *seenRollbackMarker {
	t.Helper()
	setting, err := s.GetHubSetting(context.Background(), HubSettingSectionRollbackMarker)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var seen seenRollbackMarker
	if err := json.Unmarshal(setting.Value, &seen); err != nil {
		t.Fatal(err)
	}
	return &seen
}

func TestReconcilePendingUpdate_RollbackMarker(t *testing.T) {
	ctx := context.Background()
	const marker = `{"at":"2026-10-24T03:10:00Z","from":"nightly-20261024","to":"nightly-20261023"}` + "\n"
	pendingFor := func(runID string) PendingUpdate {
		return PendingUpdate{RunID: runID, From: "nightly-20261023", To: "nightly-20261024", Strategy: "symlink", At: "2026-10-24T03:00:00Z"}
	}
	setup := func(t *testing.T, markerContent string) (*Server, store.Store, string) {
		srv, s := newTestServerWithStore(t)
		root := t.TempDir()
		srv.config.MaintenanceConfig.InstallRoot = root
		if markerContent != "" {
			writeRollbackMarker(t, root, markerContent)
		}
		return srv, s, root
	}

	t.Run("a matching marker records rolled_back once", func(t *testing.T) {
		srv, s, root := setup(t, marker)
		run := newRestartingRun(t, s)
		putPendingUpdate(t, s, pendingFor(run.ID))
		if err := srv.reconcilePendingUpdate(ctx, "nightly-20261023"); err != nil {
			t.Fatal(err)
		}
		last := getLastUpdate(t, s)
		if last == nil || last.Status != LastUpdateRolledBack || last.RunID != run.ID || last.From != "nightly-20261023" ||
			last.To != "nightly-20261024" || last.At != "2026-10-24T03:10:00Z" || last.Restored != "nightly-20261023" {
			t.Fatalf("last_update = %+v", last)
		}
		got, _ := s.GetMaintenanceRun(ctx, run.ID)
		if got.Status != store.MaintenanceStatusFailed || !strings.Contains(got.Result, "boot-failure guard") ||
			!strings.Contains(got.Result, `"verification":"rolled_back"`) {
			t.Fatalf("run = %+v", got)
		}
		requireNoPendingUpdate(t, s)
		if seen := getSeenRollbackMarker(t, s); seen == nil || seen.At != "2026-10-24T03:10:00Z" || seen.RunID != run.ID {
			t.Fatalf("seen marker = %+v", seen)
		}
		if _, err := os.Stat(filepath.Join(root, rollbackMarkerName)); err != nil {
			t.Fatal("the marker was removed")
		}

		// The same marker (same "at") is not reported for a later failure.
		run2 := newRestartingRun(t, s)
		putPendingUpdate(t, s, pendingFor(run2.ID))
		if err := srv.reconcilePendingUpdate(ctx, "nightly-20261023"); err != nil {
			t.Fatal(err)
		}
		if last := getLastUpdate(t, s); last.Status != LastUpdateFailed || last.RunID != run2.ID {
			t.Fatalf("second last_update = %+v, want failed", last)
		}

		// A rewritten marker (new "at") is reported again.
		writeRollbackMarker(t, root, strings.Replace(marker, "03:10:00Z", "05:10:00Z", 1))
		run3 := newRestartingRun(t, s)
		p3 := pendingFor(run3.ID)
		p3.At = "2026-10-24T05:00:00Z"
		putPendingUpdate(t, s, p3)
		if err := srv.reconcilePendingUpdate(ctx, "nightly-20261023"); err != nil {
			t.Fatal(err)
		}
		if last := getLastUpdate(t, s); last.Status != LastUpdateRolledBack || last.RunID != run3.ID {
			t.Fatalf("third last_update = %+v, want rolled_back", last)
		}
	})

	t.Run("a marker for another release is failed, and seen", func(t *testing.T) {
		srv, s, _ := setup(t, strings.Replace(marker, `"from":"nightly-20261024"`, `"from":"nightly-20261022"`, 1))
		run := newRestartingRun(t, s)
		putPendingUpdate(t, s, pendingFor(run.ID))
		if err := srv.reconcilePendingUpdate(ctx, "nightly-20261023"); err != nil {
			t.Fatal(err)
		}
		if last := getLastUpdate(t, s); last.Status != LastUpdateFailed {
			t.Fatalf("last_update = %+v", last)
		}
		if seen := getSeenRollbackMarker(t, s); seen == nil || seen.RunID != "" {
			t.Fatalf("seen marker = %+v", seen)
		}
	})

	t.Run("a marker older than the pending update does not match", func(t *testing.T) {
		srv, s, _ := setup(t, strings.Replace(marker, "2026-10-24T03:10:00Z", "2026-10-23T03:10:00Z", 1))
		run := newRestartingRun(t, s)
		putPendingUpdate(t, s, pendingFor(run.ID))
		if err := srv.reconcilePendingUpdate(ctx, "nightly-20261023"); err != nil {
			t.Fatal(err)
		}
		if last := getLastUpdate(t, s); last.Status != LastUpdateFailed {
			t.Fatalf("last_update = %+v", last)
		}
	})

	t.Run("a verified update ignores the marker", func(t *testing.T) {
		srv, s, _ := setup(t, marker)
		run := newRestartingRun(t, s)
		putPendingUpdate(t, s, pendingFor(run.ID))
		if err := srv.reconcilePendingUpdate(ctx, "nightly-20261024"); err != nil {
			t.Fatal(err)
		}
		if last := getLastUpdate(t, s); last.Status != LastUpdateVerified {
			t.Fatalf("last_update = %+v", last)
		}
	})

	t.Run("no pending update: seen, no last_update", func(t *testing.T) {
		srv, s, _ := setup(t, marker)
		if err := srv.reconcilePendingUpdate(ctx, "nightly-20261023"); err != nil {
			t.Fatal(err)
		}
		if last := getLastUpdate(t, s); last != nil {
			t.Fatalf("last_update = %+v", last)
		}
		if seen := getSeenRollbackMarker(t, s); seen == nil || seen.At != "2026-10-24T03:10:00Z" {
			t.Fatalf("seen marker = %+v", seen)
		}
	})

	t.Run("a malformed marker is tolerated", func(t *testing.T) {
		srv, s, _ := setup(t, "garbage")
		run := newRestartingRun(t, s)
		putPendingUpdate(t, s, pendingFor(run.ID))
		if err := srv.reconcilePendingUpdate(ctx, "nightly-20261023"); err != nil {
			t.Fatal(err)
		}
		if last := getLastUpdate(t, s); last.Status != LastUpdateFailed {
			t.Fatalf("last_update = %+v", last)
		}
		if seen := getSeenRollbackMarker(t, s); seen != nil {
			t.Fatalf("a malformed marker was recorded: %+v", seen)
		}
	})
}

func TestUpdateOutcome_RollbackMarker(t *testing.T) {
	p := PendingUpdate{To: "nightly-20261024", At: "2026-10-24T03:00:00Z"}
	m := &RollbackMarker{At: "2026-10-24T03:10:00Z", From: "nightly-20261024", To: "nightly-20261023"}
	if got := updateOutcome(p, "nightly-20261023", m); got != LastUpdateRolledBack {
		t.Fatalf("outcome = %q", got)
	}
	if got := updateOutcome(p, "nightly-20261024", m); got != LastUpdateVerified {
		t.Fatalf("outcome = %q", got)
	}
	if got := updateOutcome(PendingUpdate{To: "nightly-20261024"}, "nightly-20261023", m); got != LastUpdateRolledBack {
		t.Fatalf("outcome without a pending time = %q", got)
	}
}

func TestReleaseClass_BelowFloor(t *testing.T) {
	const floor = "nightly-20261009"
	cur := update.Ref{Version: "nightly-20261023", Date: time.Date(2026, 10, 23, 4, 0, 0, 0, time.UTC)}
	tests := []struct {
		name string
		rel  maintenance.CatalogRelease
		want string
	}{
		{"older nightly below the floor", maintenance.CatalogRelease{Tag: "nightly-20261008"}, "below-floor"},
		{"the floor itself", maintenance.CatalogRelease{Tag: floor}, "downgrade"},
		{"preview published before the floor", maintenance.CatalogRelease{Tag: "v0.3.0-preview.1",
			PublishedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}, "below-floor"},
		{"local preview without a date", maintenance.CatalogRelease{Tag: "v0.3.0-preview.1"}, "switch-older"},
		{"upgrade", maintenance.CatalogRelease{Tag: "nightly-20261024"}, "upgrade"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := releaseClass(cur, tt.rel, floor); got != tt.want {
				t.Fatalf("class = %q, want %q", got, tt.want)
			}
		})
	}
	old := update.Ref{Version: "nightly-20261001"}
	if got := releaseClass(old, maintenance.CatalogRelease{Tag: "nightly-20261001"}, floor); got != "same" {
		t.Fatalf("running release below the floor = %q, want same", got)
	}
}

func TestWriteMaintenanceRequestError_Details(t *testing.T) {
	rr := httptest.NewRecorder()
	writeMaintenanceRequestError(rr, &maintenance.RequestError{Code: maintenance.CodeDowngradeRequiresConfirmation,
		Message: "confirm", Details: map[string]any{"risks": []string{"r1"}}})
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d", rr.Code)
	}
	var e downgradeAPIError
	if err := json.Unmarshal(rr.Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	if e.Error.Code != maintenance.CodeDowngradeRequiresConfirmation || e.Error.Details["risks"] == nil {
		t.Fatalf("body = %s", rr.Body.String())
	}
}

func TestMarkRunRestarting_WritesUpdateState(t *testing.T) {
	ctx := context.Background()
	p := maintenance.PendingRestart{From: "v0.3.0-preview.4", To: "nightly-20261024", Strategy: maintenance.StrategySymlink,
		Channel: "nightly", Pin: maintenance.PinLatest, FromPublishedAt: time.Date(2026, 10, 22, 18, 0, 0, 0, time.UTC)}

	t.Run("symlink layout: from is the release previous points to", func(t *testing.T) {
		srv, s := newTestServerWithStore(t)
		root := t.TempDir()
		srv.config.MaintenanceConfig.InstallRoot = root
		if runtime.GOOS != "windows" {
			if err := os.Symlink(filepath.Join("releases", "v0.3.0-preview.4"), filepath.Join(root, "previous")); err != nil {
				t.Fatal(err)
			}
		}
		run := newRun(t, s)
		if err := srv.markRunRestarting(ctx, run, &runLogBuffer{}, p); err != nil {
			t.Fatal(err)
		}
		st, err := maintenance.ReadUpdateState(root)
		if err != nil || st == nil {
			t.Fatalf("st = %+v, err = %v", st, err)
		}
		want := maintenance.UpdateState{Target: "nightly-20261024", From: "v0.3.0-preview.4", RunID: run.ID,
			PreviousSafe: true, FromPublishedAt: "2026-10-22T18:00:00Z"}
		if *st != want {
			t.Fatalf("st = %+v, want %+v", st, want)
		}
	})

	t.Run("other installers write nothing", func(t *testing.T) {
		srv, s := newTestServerWithStore(t)
		root := t.TempDir()
		srv.config.MaintenanceConfig.InstallRoot = root
		q := p
		q.Strategy = maintenance.StrategySudoInstall
		if err := srv.markRunRestarting(ctx, newRun(t, s), &runLogBuffer{}, q); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(filepath.Join(root, maintenance.UpdateStateFileName)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("update state written for sudo-install: %v", err)
		}
	})

	t.Run("a failed write does not stop the restart", func(t *testing.T) {
		srv, s := newTestServerWithStore(t)
		srv.config.MaintenanceConfig.InstallRoot = filepath.Join(t.TempDir(), "missing")
		if err := srv.markRunRestarting(ctx, newRun(t, s), &runLogBuffer{}, p); err != nil {
			t.Fatalf("markRunRestarting = %v, want nil (best-effort update state)", err)
		}
	})
}

// reconcileFailStore fails every hub-setting read, so boot reconciliation
// returns an error.
type reconcileFailStore struct{ store.Store }

func (reconcileFailStore) GetHubSetting(context.Context, string) (*store.HubSetting, error) {
	return nil, errors.New("database unavailable")
}

func TestUpdateStateVerified_AfterListening(t *testing.T) {
	ctx := context.Background()
	write := func(t *testing.T, root, runID string) {
		t.Helper()
		if err := maintenance.WriteUpdateState(root, maintenance.UpdateState{Target: "nightly-20261024", From: "nightly-20261023",
			RunID: runID, Migrated: true, PreviousSafe: true}); err != nil {
			t.Fatal(err)
		}
	}
	verified := func(t *testing.T, root string) bool {
		t.Helper()
		st, err := maintenance.ReadUpdateState(root)
		if err != nil || st == nil {
			t.Fatalf("st = %+v, err = %v", st, err)
		}
		return st.Verified
	}

	t.Run("reconciliation alone does not verify; the listening hook does", func(t *testing.T) {
		srv, s := newTestServerWithStore(t)
		root := t.TempDir()
		srv.config.MaintenanceConfig.InstallRoot = root
		run := newRestartingRun(t, s)
		write(t, root, run.ID)
		putPendingUpdate(t, s, PendingUpdate{RunID: run.ID, From: "nightly-20261023", To: "nightly-20261024", At: "2026-10-24T03:00:00Z"})
		if err := srv.reconcilePendingUpdate(ctx, "nightly-20261024"); err != nil {
			t.Fatal(err)
		}
		if verified(t, root) {
			t.Fatal("verified before the listener was bound")
		}
		srv.markUpdateStateVerified("nightly-20261024")
		if !verified(t, root) {
			t.Fatal("not verified after the listening hook")
		}
	})

	t.Run("a deploy.sh install without a pending update", func(t *testing.T) {
		srv, _ := newTestServerWithStore(t)
		root := t.TempDir()
		srv.config.MaintenanceConfig.InstallRoot = root
		write(t, root, maintenance.UpdateStateRunIDDeploy)
		srv.markUpdateStateVerified("nightly-20261024")
		if !verified(t, root) {
			t.Fatal("deploy.sh install not verified")
		}
	})

	t.Run("a failed reconciliation does not block it (R1)", func(t *testing.T) {
		_, s := newTestServerWithStore(t)
		srv := &Server{store: reconcileFailStore{s}, maintenanceLog: logging.Subsystem("hub.maintenance")}
		root := t.TempDir()
		srv.config.MaintenanceConfig.InstallRoot = root
		write(t, root, "r1")
		if err := srv.reconcilePendingUpdate(ctx, "nightly-20261024"); err == nil {
			t.Fatal("want a reconciliation error")
		}
		srv.markUpdateStateVerified("nightly-20261024")
		if !verified(t, root) {
			t.Fatal("a reconciliation error kept the target unverified")
		}
	})

	t.Run("a rolled-back hub leaves it unverified", func(t *testing.T) {
		srv, _ := newTestServerWithStore(t)
		root := t.TempDir()
		srv.config.MaintenanceConfig.InstallRoot = root
		write(t, root, "r1")
		srv.markUpdateStateVerified("nightly-20261023")
		if verified(t, root) {
			t.Fatal("verified a target the hub does not run")
		}
	})

	t.Run("the exported hook uses the running version", func(t *testing.T) {
		setRunningVersion(t, "nightly-20261024")
		srv, _ := newTestServerWithStore(t)
		root := t.TempDir()
		srv.config.MaintenanceConfig.InstallRoot = root
		write(t, root, "r1")
		srv.MarkUpdateStateVerified()
		if !verified(t, root) {
			t.Fatal("MarkUpdateStateVerified did not verify the running target")
		}
	})
}

func TestListenThenNotify(t *testing.T) {
	fired := make(chan struct{}, 1)
	ln, err := listenThenNotify("127.0.0.1:0", func() { fired <- struct{}{} })
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	select {
	case <-fired:
	case <-time.After(5 * time.Second):
		t.Fatal("the hook did not run after a successful bind")
	}

	// The port is now taken: the bind fails and the hook never runs.
	called := make(chan struct{}, 1)
	if ln2, err := listenThenNotify(ln.Addr().String(), func() { called <- struct{}{} }); err == nil {
		_ = ln2.Close()
		t.Fatal("binding a taken port succeeded")
	}
	select {
	case <-called:
		t.Fatal("the hook ran after a failed bind")
	case <-time.After(200 * time.Millisecond):
	}

	// No hook is fine.
	ln3, err := listenThenNotify("127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = ln3.Close()
}
