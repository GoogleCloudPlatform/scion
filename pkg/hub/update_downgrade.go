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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"entgo.io/ent/dialect"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/maintenance"
	"github.com/GoogleCloudPlatform/scion/pkg/version"
)

// updateSnapshotDirName is the snapshot directory next to the SQLite
// database on hubs without the symlink install layout (no scion-owned
// install root to put <install_root>/snapshots in).
const updateSnapshotDirName = "update-snapshots"

// updateSnapshotter returns the DBSnapshotter of the update-binary executor:
// MaintenanceConfig.DBSnapshotter when set, otherwise one derived from the
// store. Only a file-backed SQLite database can be snapshotted; PostgreSQL
// and in-memory databases get an unsupported snapshotter (downgrades are
// then refused, upgrades go ahead without a snapshot).
//
// Snapshots go to <install_root>/snapshots on the symlink layout (design
// D1), and to update-snapshots/ next to the database file otherwise
// (sudo-install starter hubs have no writable install root).
func (s *Server) updateSnapshotter(ctx context.Context) maintenance.DBSnapshotter {
	mc := s.config.MaintenanceConfig
	if mc.DBSnapshotter != nil {
		return mc.DBSnapshotter
	}
	db, dbDialect := s.storeDB()
	switch {
	case db == nil:
		return maintenance.NewUnsupportedDBSnapshotter("the hub store has no SQL database")
	case dbDialect == dialect.Postgres:
		return maintenance.NewUnsupportedDBSnapshotter("PostgreSQL database; snapshots are taken on SQLite only")
	case dbDialect != dialect.SQLite && dbDialect != "sqlite":
		return maintenance.NewUnsupportedDBSnapshotter(fmt.Sprintf("%q database; snapshots are taken on SQLite only", dbDialect))
	}
	var file string
	if err := db.QueryRowContext(ctx, "SELECT file FROM pragma_database_list WHERE name = 'main'").Scan(&file); err != nil {
		s.maintenanceLog.Warn("Cannot locate the database file for update snapshots", "error", err)
		return maintenance.NewUnsupportedDBSnapshotter("the database file cannot be located")
	}
	if file == "" {
		return maintenance.NewUnsupportedDBSnapshotter("in-memory database")
	}
	var settings string
	if dir, err := config.GetGlobalDir(); err == nil {
		settings = filepath.Join(dir, "settings.yaml")
	}
	return maintenance.NewSQLiteSnapshotter(maintenance.SQLiteSnapshotOptions{
		DB:           db,
		Dir:          updateSnapshotDir(mc, s.updateInstaller().Name(), file),
		SettingsPath: settings,
	})
}

// updateSnapshotDir is where update snapshots of the database file dbFile
// go for an installer strategy (see updateSnapshotter).
func updateSnapshotDir(mc MaintenanceConfig, strategy, dbFile string) string {
	if strategy == maintenance.StrategySymlink && mc.InstallRoot != "" {
		return filepath.Join(mc.InstallRoot, maintenance.SnapshotsDirName)
	}
	return filepath.Join(filepath.Dir(dbFile), updateSnapshotDirName)
}

// updateStatusDowngrade is update-status's "downgrade" block.
type updateStatusDowngrade struct {
	// Floor is the oldest release this binary installs
	// (version.DowngradeFloor).
	Floor string `json:"floor"`
	// SnapshotsSupported is false on PostgreSQL (and in-memory) hubs,
	// which refuse downgrades.
	SnapshotsSupported bool `json:"snapshotsSupported"`
}

// Rollback marker of the boot-failure guard (design D1, contract agreed
// with deploy.sh): <install_root>/rollback.marker, one line of JSON
// {"at":"<RFC 3339 UTC>","from":"<failed tag>","to":"<restored tag>"},
// written by the install owner with an atomic rename and rewritten on each
// rollback. The hub only reads it: it never deletes it, never follows it if
// it is a symlink, and reports each marker (by "at") once.
const (
	rollbackMarkerName    = "rollback.marker"
	maxRollbackMarkerSize = 4096
	// HubSettingSectionRollbackMarker records the last rollback marker the
	// hub has seen, so each is reported once.
	HubSettingSectionRollbackMarker = "system.rollback_marker"
)

// RollbackMarker is the content of the rollback marker file.
type RollbackMarker struct {
	At   string `json:"at"`
	From string `json:"from"` // the release that failed to start
	To   string `json:"to"`   // the release the guard restored
}

// seenRollbackMarker is the system.rollback_marker row.
type seenRollbackMarker struct {
	At     string `json:"at"`
	From   string `json:"from"`
	To     string `json:"to"`
	SeenAt string `json:"seenAt"`
	// RunID is the update run reported as rolled back ("" when the marker
	// matched no pending update).
	RunID string `json:"runId,omitempty"`
}

// readRollbackMarker reads the rollback marker under installRoot. It
// returns nil, nil when there is none. A marker that is not a regular file
// (a symlink, a directory), larger than maxRollbackMarkerSize, or not the
// agreed JSON is an error.
func readRollbackMarker(installRoot string) (*RollbackMarker, error) {
	if installRoot == "" {
		return nil, nil
	}
	path := filepath.Join(installRoot, rollbackMarkerName)
	before, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file (%s)", path, before.Mode().Type())
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil // removed or being replaced; the next boot reads it
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	// The path may have been swapped for a symlink between the Lstat and
	// the Open: only read the file that was checked.
	opened, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(before, opened) {
		return nil, fmt.Errorf("%s changed while it was read", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxRollbackMarkerSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxRollbackMarkerSize {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, maxRollbackMarkerSize)
	}
	var m RollbackMarker
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &m); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	if _, err := time.Parse(time.RFC3339, m.At); err != nil {
		return nil, fmt.Errorf("%s: invalid \"at\" %q", path, m.At)
	}
	if m.From == "" || m.To == "" {
		return nil, fmt.Errorf("%s: \"from\" and \"to\" are required", path)
	}
	return &m, nil
}

// newRollbackMarker returns the rollback marker when there is one the hub
// has not seen yet (nil otherwise). A missing, unreadable or malformed
// marker is nil, with a warning for the latter two: it must never block
// boot reconciliation.
func (s *Server) newRollbackMarker(ctx context.Context) *RollbackMarker {
	m, err := readRollbackMarker(s.config.MaintenanceConfig.InstallRoot)
	if err != nil {
		s.maintenanceLog.Warn("Ignoring an unreadable rollback marker", "error", err)
		return nil
	}
	if m == nil {
		return nil
	}
	var seen seenRollbackMarker
	if found, err := s.readHubSettingJSON(ctx, HubSettingSectionRollbackMarker, &seen); err != nil && !found {
		s.maintenanceLog.Warn("Cannot read the last seen rollback marker; not reporting the marker", "error", err)
		return nil
	} else if found && err == nil && seen.At == m.At {
		return nil
	}
	return m
}

// recordRollbackMarker stores m as seen, so it is not reported again.
// runID is the update run it was reported for ("" for none).
func (s *Server) recordRollbackMarker(ctx context.Context, m *RollbackMarker, runID string) error {
	return s.writeHubSettingJSON(ctx, HubSettingSectionRollbackMarker, seenRollbackMarker{
		At:     m.At,
		From:   m.From,
		To:     m.To,
		SeenAt: time.Now().UTC().Format(time.RFC3339),
		RunID:  runID,
	})
}

// markerMatchesPending reports whether marker m records the rollback of
// pending update p: the guard rolled back from p's target, and not before
// p was written (when both times parse).
func markerMatchesPending(m *RollbackMarker, p PendingUpdate) bool {
	if m == nil || p.To == "" || m.From != p.To {
		return false
	}
	at, err1 := time.Parse(time.RFC3339, m.At)
	pAt, err2 := time.Parse(time.RFC3339, p.At)
	if err1 == nil && err2 == nil && at.Before(pAt) {
		return false
	}
	return true
}

// writeUpdateStateBeforeRestart writes <install_root>/update-state.json
// for the boot-failure guard just before the self-restart of an update
// (best-effort: a failure is logged, the update goes on). Only the symlink
// layout has the guard. from is the release `previous` now points to (the
// guard checks it against that link), else the running version.
func (s *Server) writeUpdateStateBeforeRestart(runID string, p maintenance.PendingRestart) {
	mc := s.config.MaintenanceConfig
	if p.Strategy != maintenance.StrategySymlink || mc.InstallRoot == "" {
		return
	}
	from := p.From
	if link, err := os.Readlink(filepath.Join(mc.InstallRoot, "previous")); err == nil && filepath.Base(link) != "" {
		from = filepath.Base(link)
	}
	st := maintenance.UpdateState{
		Target:       p.To,
		From:         from,
		RunID:        runID,
		PreviousSafe: true,
	}
	if !p.FromPublishedAt.IsZero() {
		st.FromPublishedAt = p.FromPublishedAt.UTC().Format(time.RFC3339)
	}
	if err := maintenance.WriteUpdateState(mc.InstallRoot, st); err != nil {
		s.maintenanceLog.Warn("Failed to write the update state for the boot-failure guard", "run_id", runID, "error", err)
	}
}

// MarkUpdateStateVerified sets verified=true in the update-state file when
// this binary is its target. Call it once the listener serving the hub API
// is bound (SetOnListening on the hub or, in combined mode, the web
// server): only a binary that got that far counts as healthy, so a target
// that crash-loops before it listens (a port clash, a failing init) stays
// unverified and the boot-failure guard can still roll it back. It does
// not depend on boot reconciliation or the database. Best-effort: a
// failure is logged.
func (s *Server) MarkUpdateStateVerified() {
	s.markUpdateStateVerified(version.Version)
}

// markUpdateStateVerified sets verified=true in the update-state file when
// running is its target (best-effort, logged), whether the target was
// installed by an update run or by deploy.sh.
func (s *Server) markUpdateStateVerified(running string) {
	changed, err := maintenance.MarkUpdateStateVerified(s.config.MaintenanceConfig.InstallRoot, running)
	switch {
	case err != nil:
		s.maintenanceLog.Warn("Failed to mark the update state verified", "error", err)
	case changed:
		s.maintenanceLog.Info("Update state marked verified", "version", running)
	}
}
