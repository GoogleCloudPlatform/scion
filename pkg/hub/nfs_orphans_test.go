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
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for Phase 4 NFS agent-directory orphan reporting (ptone/scion#7932).

type nfsOrphanTestEnv struct {
	srv      *Server
	s        store.Store
	mountDir string
	subRoot  string
	logs     *captureHandler
}

func newNFSOrphanTestEnv(t *testing.T, mounted bool, expOn bool) *nfsOrphanTestEnv {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".scion"), 0o755))

	srv, s := newTestServerWithStore(t)
	logs := &captureHandler{}
	srv.agentLifecycleLog = slog.New(logs)
	srv.config.SoftDeleteRetention = 24 * time.Hour
	setExperimentOverrides(t, srv, map[string]bool{experiments.RuntimeOrphanReconcile: expOn})

	mountRoot := filepath.Join(home, "mnt")
	share := filepath.Join(mountRoot, "share1")
	subRoot := filepath.Join(share, "projects")
	srv.config.ProjectStorageConfig = &config.V1ProjectStorageConfig{
		Backend: "nfs",
		NFS: &config.V1NFSConfig{
			MountRoot:   mountRoot,
			SubPathRoot: "projects",
			Shares:      []config.V1NFSShare{{ID: "share1", Server: "10.0.0.2", Export: "/ws", PVName: "pv"}},
		},
	}
	if mounted {
		require.NoError(t, os.MkdirAll(subRoot, 0o755))
	} else {
		require.NoError(t, os.MkdirAll(share, 0o755))
	}

	return &nfsOrphanTestEnv{
		srv:      srv,
		s:        s,
		mountDir: share,
		subRoot:  subRoot,
		logs:     logs,
	}
}

func makeNFSFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

func TestNFSOrphans_ClassificationAndNeverDeletes(t *testing.T) {
	env := newNFSOrphanTestEnv(t, true, true)
	ctx := context.Background()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

	projID := uuid.NewString()
	require.NoError(t, env.s.CreateProject(ctx, &store.Project{ID: projID, Name: "proj", Slug: "proj"}))

	// Live agent: its root, home, and secrets must NOT be reported.
	liveID := uuid.NewString()
	require.NoError(t, env.s.CreateAgent(ctx, &store.Agent{
		ID: liveID, Slug: "live", Name: "live", ProjectID: projID, Phase: "running",
	}))
	// Soft-deleted within retention (1h < 24h): must NOT be reported.
	softRecentID := uuid.NewString()
	require.NoError(t, env.s.CreateAgent(ctx, &store.Agent{
		ID: softRecentID, Slug: "soft-recent", Name: "soft-recent", ProjectID: projID, Phase: "stopped",
	}))
	softRecent, err := env.s.GetAgent(ctx, softRecentID)
	require.NoError(t, err)
	softRecent.DeletedAt = now.Add(-1 * time.Hour)
	require.NoError(t, env.s.UpdateAgent(ctx, softRecent))

	// Soft-deleted past retention (48h > 24h): MUST be reported as deleted.
	softOldID := uuid.NewString()
	require.NoError(t, env.s.CreateAgent(ctx, &store.Agent{
		ID: softOldID, Slug: "soft-old", Name: "soft-old", ProjectID: projID, Phase: "stopped",
	}))
	softOld, err := env.s.GetAgent(ctx, softOldID)
	require.NoError(t, err)
	softOld.DeletedAt = now.Add(-48 * time.Hour)
	require.NoError(t, env.s.UpdateAgent(ctx, softOld))

	// Superseded child under live agent's slug (old incarnation ID).
	oldIncarnationID := uuid.NewString()
	// Completely gone agent (no row at all).
	goneID := uuid.NewString()
	// Unknown project on NFS.
	unknownProjID := uuid.NewString()
	strayID := uuid.NewString()

	// Create filesystem trees with marker files to verify nothing is ever removed.
	files := []string{
		filepath.Join(env.subRoot, projID, "agents", "live", "home-"+liveID, "keep.txt"),
		filepath.Join(env.subRoot, projID, "agents", "live", "secrets-"+liveID, "tok"),
		filepath.Join(env.subRoot, projID, "agents", "live", "home-"+oldIncarnationID, "old.txt"),
		filepath.Join(env.subRoot, projID, "agents", "live", "secrets-"+oldIncarnationID, "oldtok"),
		filepath.Join(env.subRoot, projID, "agents", "soft-recent", "home-"+softRecentID, "keep.txt"),
		filepath.Join(env.subRoot, projID, "agents", "soft-old", "home-"+softOldID, "stale.txt"),
		filepath.Join(env.subRoot, projID, "agents", "soft-old", "secrets-"+softOldID, "stale.tok"),
		filepath.Join(env.subRoot, projID, "agents", "gone", "home-"+goneID, "orphan.txt"),
		filepath.Join(env.subRoot, projID, "agents", "gone", "secrets-"+goneID, "orphan.tok"),
		filepath.Join(env.subRoot, unknownProjID, "agents", "stray", "home-"+strayID, "stray.txt"),
	}
	for _, f := range files {
		makeNFSFile(t, f, "data")
	}
	// Non-UUID child directory under live agent should be ignored.
	require.NoError(t, os.MkdirAll(filepath.Join(env.subRoot, projID, "agents", "live", "home-not-a-uuid"), 0o755))
	// Symlink under agents/ must never be followed or reported.
	outside := t.TempDir()
	require.NoError(t, os.Symlink(outside, filepath.Join(env.subRoot, projID, "agents", "symlink-agent")))

	report, err := env.srv.scanNFSOrphans(ctx, now, defaultNFSOrphanMaxDirs)
	require.NoError(t, err)
	assert.True(t, report.Available)
	assert.True(t, report.Complete)
	assert.False(t, report.Truncated)

	rows, err := env.s.ListRuntimeOrphans(ctx, store.RuntimeOrphanFilter{Kind: store.RuntimeOrphanKindNFSTree})
	require.NoError(t, err)

	type rowKey struct {
		target, project, slug, agentID, reason string
	}
	got := make([]rowKey, 0, len(rows))
	for _, r := range rows {
		assert.Equal(t, store.RuntimeOrphanKindNFSTree, r.Kind)
		assert.Equal(t, store.RuntimeOrphanBrokerHub, r.BrokerID)
		got = append(got, rowKey{r.RuntimeTarget, r.ProjectID, r.Slug, r.AgentID, r.Reason})
	}
	want := []rowKey{
		{"agent-home", projID, "live", oldIncarnationID, orphanSuperseded},
		{"agent-secrets", projID, "live", oldIncarnationID, orphanSuperseded},
		{"agent-root", projID, "soft-old", "", orphanDeleted},
		{"agent-home", projID, "soft-old", softOldID, orphanDeleted},
		{"agent-secrets", projID, "soft-old", softOldID, orphanDeleted},
		{"agent-root", projID, "gone", "", orphanNoAgent},
		{"agent-home", projID, "gone", goneID, orphanNoAgent},
		{"agent-secrets", projID, "gone", goneID, orphanNoAgent},
		{"agent-root", unknownProjID, "stray", "", orphanUnknownProject},
		{"agent-home", unknownProjID, "stray", strayID, orphanUnknownProject},
	}
	assert.ElementsMatch(t, want, got)

	// Re-running must create 0 new rows and never delete any file.
	report2, err := env.srv.scanNFSOrphans(ctx, now.Add(time.Hour), defaultNFSOrphanMaxDirs)
	require.NoError(t, err)
	assert.Equal(t, 0, report2.OrphansCreated)
	for _, f := range files {
		assert.FileExists(t, f, "report-only scan must never remove %s", f)
	}

	// Admin API lists kind=nfs_tree rows.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/runtime-orphans?kind=nfs_tree", nil)
	rr := httptest.NewRecorder()
	env.srv.handleAdminRuntimeOrphans(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)
	var apiResp RuntimeOrphansResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &apiResp))
	assert.Len(t, apiResp.Orphans, len(want))

	// Removing an orphan tree externally causes the next complete scan to resolve it.
	require.NoError(t, os.RemoveAll(filepath.Join(env.subRoot, unknownProjID)))
	report3, err := env.srv.scanNFSOrphans(ctx, now.Add(2*time.Hour), defaultNFSOrphanMaxDirs)
	require.NoError(t, err)
	assert.True(t, report3.Complete)
	rowsAfter, err := env.s.ListRuntimeOrphans(ctx, store.RuntimeOrphanFilter{Kind: store.RuntimeOrphanKindNFSTree})
	require.NoError(t, err)
	assert.Len(t, rowsAfter, len(want)-2)
}

func TestNFSOrphans_BoundedScanNoFalseResolution(t *testing.T) {
	env := newNFSOrphanTestEnv(t, true, true)
	ctx := context.Background()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

	projID := uuid.NewString()
	require.NoError(t, env.s.CreateProject(ctx, &store.Project{ID: projID, Name: "proj", Slug: "proj"}))

	for _, slug := range []string{"a1", "a2", "a3", "a4"} {
		require.NoError(t, os.MkdirAll(filepath.Join(env.subRoot, projID, "agents", slug), 0o755))
	}

	// Complete scan records all 4.
	full, err := env.srv.scanNFSOrphans(ctx, now, 100)
	require.NoError(t, err)
	assert.True(t, full.Complete)
	assert.False(t, full.Truncated)
	assert.Equal(t, 4, full.OrphansFound)

	// Bounded scan stops at 2 directories and must NOT remove the other 2 rows.
	bounded, err := env.srv.scanNFSOrphans(ctx, now.Add(time.Hour), 2)
	require.NoError(t, err)
	assert.True(t, bounded.Truncated)
	assert.False(t, bounded.Complete)
	assert.Equal(t, 2, bounded.DirsVisited)

	rows, err := env.s.ListRuntimeOrphans(ctx, store.RuntimeOrphanFilter{Kind: store.RuntimeOrphanKindNFSTree})
	require.NoError(t, err)
	assert.Len(t, rows, 4, "truncated scan must not resolve unvisited rows")
}

func TestNFSOrphans_UnavailableLogsOnce(t *testing.T) {
	env := newNFSOrphanTestEnv(t, false, true)
	ctx := context.Background()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

	r1, err := env.srv.scanNFSOrphans(ctx, now, defaultNFSOrphanMaxDirs)
	require.NoError(t, err)
	assert.False(t, r1.Available)

	r2, err := env.srv.scanNFSOrphans(ctx, now.Add(time.Hour), defaultNFSOrphanMaxDirs)
	require.NoError(t, err)
	assert.False(t, r2.Available)

	const unavailMsg = "nfs orphan scan unavailable: hub has no mounted shared project storage"
	assert.Len(t, env.logs.records(unavailMsg), 1, "unavailability must be logged once")

	// When experiment is off, the recurring handler does nothing (no logs, no writes).
	envOff := newNFSOrphanTestEnv(t, false, false)
	envOff.srv.nfsOrphanScanHandler()(ctx)
	assert.Empty(t, envOff.logs.records(unavailMsg))
}

func TestNFSOrphans_MaintenanceOpAndSingleton(t *testing.T) {
	env := newNFSOrphanTestEnv(t, true, false)
	ctx := context.Background()

	// Verify experiment gating on HTTP route (operations are seeded during Migrate).
	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")

	getOp := func() int {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/maintenance/operations/"+entadapter.NFSOrphanScanKey, nil)
		req = req.WithContext(contextWithIdentity(req.Context(), admin))
		rr := httptest.NewRecorder()
		env.srv.handleAdminMaintenanceOps(rr, req)
		return rr.Code
	}
	assert.Equal(t, http.StatusNotFound, getOp(), "404 while experiment is off")

	setExperimentOverrides(t, env.srv, map[string]bool{experiments.RuntimeOrphanReconcile: true})
	assert.Equal(t, http.StatusOK, getOp(), "200 while experiment is on")

	// Executor runs and writes summary without error.
	projID := uuid.NewString()
	require.NoError(t, env.s.CreateProject(ctx, &store.Project{ID: projID, Name: "proj", Slug: "proj"}))
	require.NoError(t, os.MkdirAll(filepath.Join(env.subRoot, projID, "agents", "orphan-dir"), 0o755))

	exec, err := env.srv.resolveMaintenanceExecutor(entadapter.NFSOrphanScanKey)
	require.NoError(t, err)
	var buf bytes.Buffer
	require.NoError(t, exec.Run(ctx, &buf, nil))
	assert.Contains(t, buf.String(), "found 1 orphan trees (1 newly recorded)")

	// Recurring singleton job is registered with 24h cadence.
	env.srv.scheduler = NewScheduler(env.srv.store, logging.Subsystem("hub.scheduler"))
	env.srv.registerSchedulerHandlers()
	var found *RecurringHandler
	for i := range env.srv.scheduler.recurring {
		if env.srv.scheduler.recurring[i].Name == "nfs-orphan-scan" {
			found = &env.srv.scheduler.recurring[i]
			break
		}
	}
	require.NotNil(t, found, "nfs-orphan-scan recurring singleton must be registered")
	assert.True(t, found.Singleton, "nfs-orphan-scan must be registered via RegisterRecurringSingleton")
	assert.Equal(t, 24*60, found.Interval)
}
