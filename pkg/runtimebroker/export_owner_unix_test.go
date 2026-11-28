//go:build unix

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

package runtimebroker

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/stretchr/testify/require"
)

func TestReadOrCreateExportOwner_CreatesOnceThenReads(t *testing.T) {
	dir := t.TempDir()

	uid, gid, created, err := readOrCreateExportOwner(dir, 1000, 2000)
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, 1000, uid)
	require.Equal(t, 2000, gid)
	require.FileExists(t, filepath.Join(dir, api.ExportOwnerMarkerName))

	// A second broker with different ids reads the existing canonical owner.
	uid2, gid2, created2, err := readOrCreateExportOwner(dir, 3000, 4000)
	require.NoError(t, err)
	require.False(t, created2)
	require.Equal(t, 1000, uid2)
	require.Equal(t, 2000, gid2)
}

func TestReadOrCreateExportOwner_ConcurrentCreatorsAgree(t *testing.T) {
	dir := t.TempDir()
	const workers = 16
	uids := make([]int, workers)
	gids := make([]int, workers)
	created := make([]bool, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			uids[i], gids[i], created[i], errs[i] = readOrCreateExportOwner(dir, 1000+i, 2000+i)
		}(i)
	}
	close(start)
	wg.Wait()

	createdCount := 0
	for i := 0; i < workers; i++ {
		require.NoError(t, errs[i])
		require.Equal(t, uids[0], uids[i], "all concurrent creators must observe the winning uid")
		require.Equal(t, gids[0], gids[i], "all concurrent creators must observe the winning gid")
		if created[i] {
			createdCount++
		}
	}
	require.Equal(t, 1, createdCount, "exactly one concurrent creator must report creating the marker")
}

func TestReadOrCreateExportOwner_InvalidMarkerIsNotOverwritten(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, api.ExportOwnerMarkerName)
	require.NoError(t, os.WriteFile(marker, []byte("not-a-uid-gid\n"), 0o644))

	_, _, created, err := readOrCreateExportOwner(dir, 1000, 1000)
	require.Error(t, err)
	require.False(t, created)

	got, readErr := os.ReadFile(marker)
	require.NoError(t, readErr)
	require.Equal(t, "not-a-uid-gid\n", string(got))
}

func TestReadOrCreateExportOwner_MissingDirIsNotCreated(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist")
	_, _, created, err := readOrCreateExportOwner(dir, 1000, 1000)
	require.Error(t, err)
	require.False(t, created)
	require.NoDirExists(t, dir)
}

func TestEffectiveAgentOwnerIDs(t *testing.T) {
	nfsDefault := projectfs.Layout{Backend: projectfs.BackendNFS}
	nfsCustom := projectfs.Layout{Backend: projectfs.BackendNFS, UID: 2000, GID: 3000}
	local := projectfs.Layout{Backend: projectfs.BackendLocal}

	for _, tc := range []struct {
		name    string
		runtime string
		layout  projectfs.Layout
		wantUID int
		wantGID int
		wantOK  bool
	}{
		{"non-shared layout", "docker", local, 0, 0, false},
		{"docker default nfs", "docker", nfsDefault, 1000, 1000, true},
		{"docker custom nfs", "docker", nfsCustom, 2000, 3000, true},
		{"k8s ignores nfs uid, uses fsGroup gid", "kubernetes", nfsCustom, 1000, 3000, true},
		{"k8s default nfs", "kubernetes", nfsDefault, 1000, 1000, true},
		{"cloudrun-sandbox", "cloudrun-sandbox", nfsCustom, 0, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			uid, gid, ok := effectiveAgentOwnerIDs(tc.runtime, tc.layout)
			require.Equal(t, tc.wantOK, ok)
			require.Equal(t, tc.wantUID, uid)
			require.Equal(t, tc.wantGID, gid)
		})
	}
}

func TestShareHealth_OwnerCheck(t *testing.T) {
	ctx := context.Background()

	t.Run("missing marker is created on first check with INFO log and reports healthy", func(t *testing.T) {
		logs := captureDefaultLog(t)
		f := newShareVisibleFixture(t, "kubernetes", "10.0.0.2:/scion-workspaces/")
		marker := filepath.Join(f.hostBase, "projects", api.ExportOwnerMarkerName)
		require.NoFileExists(t, marker)
		f.srv.shareHealth.agentOwnerIDs = func() (int, int, bool) { return 1000, 1000, true }

		f.srv.checkShareHealth(ctx)
		info := f.srv.GetHealthInfo(ctx)
		require.Equal(t, "healthy", info.Checks[shareHealthCheck])
		require.Equal(t, "healthy", info.Status)

		raw, err := os.ReadFile(marker)
		require.NoError(t, err)
		require.Equal(t, "1000:1000\n", string(raw))

		// Creation logs INFO (in addition to share visible INFO); a second check
		// does not re-create or log creation again.
		infoAfterFirst := logs.count(slog.LevelInfo)
		require.GreaterOrEqual(t, infoAfterFirst, 2)
		f.srv.checkShareHealth(ctx)
		require.Equal(t, infoAfterFirst, logs.count(slog.LevelInfo))
	})

	t.Run("match reports healthy with no warning", func(t *testing.T) {
		f := newShareVisibleFixture(t, "kubernetes", "10.0.0.2:/scion-workspaces/")
		marker := filepath.Join(f.hostBase, "projects", api.ExportOwnerMarkerName)
		require.NoError(t, os.WriteFile(marker, []byte("1000:1000\n"), 0o644))
		f.srv.shareHealth.agentOwnerIDs = func() (int, int, bool) { return 1000, 1000, true }

		f.srv.checkShareHealth(ctx)
		info := f.srv.GetHealthInfo(ctx)
		require.Equal(t, "healthy", info.Checks[shareHealthCheck])
		require.Equal(t, "healthy", info.Status)
		hb := sendHealthHeartbeat(t, f.srv)
		require.NotNil(t, hb.Health)
		require.Equal(t, api.BrokerHealthHealthy, hb.Health.Checks[shareHealthCheck])
		require.Equal(t, api.BrokerHealthHealthy, hb.Health.Status)
	})

	t.Run("uid mismatch reports warning naming both ids without degrading status", func(t *testing.T) {
		logs := captureDefaultLog(t)
		f := newShareVisibleFixture(t, "kubernetes", "10.0.0.2:/scion-workspaces/")
		marker := filepath.Join(f.hostBase, "projects", api.ExportOwnerMarkerName)
		require.NoError(t, os.WriteFile(marker, []byte("2000:1000\n"), 0o644))
		f.srv.shareHealth.agentOwnerIDs = func() (int, int, bool) { return 1000, 1000, true }

		f.srv.checkShareHealth(ctx)
		f.srv.checkShareHealth(ctx) // second check must not duplicate WARN log
		info := f.srv.GetHealthInfo(ctx)
		got := info.Checks[shareHealthCheck]
		require.True(t, strings.HasPrefix(got, shareHealthOwnerMismatch+" ("), "got %q", got)
		require.Contains(t, got, "1000:1000")
		require.Contains(t, got, "2000:1000")
		require.Contains(t, got, api.ExportOwnerMarkerName)
		require.NotContains(t, got, "broker runtime uid/gid")
		assertNoPathValues(t, f, got)
		require.Equal(t, "healthy", info.Status)
		require.Equal(t, 1, logs.count(slog.LevelWarn))

		hb := sendHealthHeartbeat(t, f.srv)
		require.NotNil(t, hb.Health)
		require.Equal(t, api.BrokerHealthWarning, hb.Health.Checks[shareHealthCheck])
		require.Equal(t, api.BrokerHealthHealthy, hb.Health.Status)

		w := httptest.NewRecorder()
		f.srv.handleReadyz(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		require.Equal(t, http.StatusOK, w.Code)

		// Following the remediation (removing the stale marker) and re-checking
		// clears the warning and records the aligned owner.
		require.NoError(t, os.Remove(marker))
		f.srv.checkShareHealth(ctx)
		info = f.srv.GetHealthInfo(ctx)
		require.Equal(t, "healthy", info.Checks[shareHealthCheck])
		require.Equal(t, "healthy", info.Status)
		raw, err := os.ReadFile(marker)
		require.NoError(t, err)
		require.Equal(t, "1000:1000\n", string(raw))
	})

	t.Run("gid mismatch reports warning naming both ids", func(t *testing.T) {
		f := newShareVisibleFixture(t, "kubernetes", "10.0.0.2:/scion-workspaces/")
		marker := filepath.Join(f.hostBase, "projects", api.ExportOwnerMarkerName)
		require.NoError(t, os.WriteFile(marker, []byte("1000:2000\n"), 0o644))
		f.srv.shareHealth.agentOwnerIDs = func() (int, int, bool) { return 1000, 1000, true }

		f.srv.checkShareHealth(ctx)
		info := f.srv.GetHealthInfo(ctx)
		got := info.Checks[shareHealthCheck]
		require.True(t, strings.HasPrefix(got, shareHealthOwnerMismatch+" ("), "got %q", got)
		require.Contains(t, got, "1000:1000")
		require.Contains(t, got, "1000:2000")
		assertNoPathValues(t, f, got)
		require.Equal(t, "healthy", info.Status)
		hb := sendHealthHeartbeat(t, f.srv)
		require.NotNil(t, hb.Health)
		require.Equal(t, api.BrokerHealthWarning, hb.Health.Checks[shareHealthCheck])
	})

	t.Run("unknown recorded owner produces no warning", func(t *testing.T) {
		t.Run("malformed marker", func(t *testing.T) {
			f := newShareVisibleFixture(t, "kubernetes", "10.0.0.2:/scion-workspaces/")
			marker := filepath.Join(f.hostBase, "projects", api.ExportOwnerMarkerName)
			require.NoError(t, os.WriteFile(marker, []byte("corrupt\n"), 0o644))
			f.srv.shareHealth.agentOwnerIDs = func() (int, int, bool) { return 1000, 1000, true }

			f.srv.checkShareHealth(ctx)
			info := f.srv.GetHealthInfo(ctx)
			require.Equal(t, "healthy", info.Checks[shareHealthCheck])
			require.Equal(t, "healthy", info.Status)
		})

		t.Run("unreadable marker", func(t *testing.T) {
			f := newShareVisibleFixture(t, "kubernetes", "10.0.0.2:/scion-workspaces/")
			marker := filepath.Join(f.hostBase, "projects", api.ExportOwnerMarkerName)
			// A directory at the marker path causes readExportOwnerMarker to fail
			// with an I/O error regardless of process UID.
			require.NoError(t, os.Mkdir(marker, 0o755))
			f.srv.shareHealth.agentOwnerIDs = func() (int, int, bool) { return 1000, 1000, true }

			f.srv.checkShareHealth(ctx)
			info := f.srv.GetHealthInfo(ctx)
			require.Equal(t, "healthy", info.Checks[shareHealthCheck])
			require.Equal(t, "healthy", info.Status)
		})

		t.Run("agent owner unknown", func(t *testing.T) {
			f := newShareVisibleFixture(t, "kubernetes", "10.0.0.2:/scion-workspaces/")
			marker := filepath.Join(f.hostBase, "projects", api.ExportOwnerMarkerName)
			require.NoError(t, os.WriteFile(marker, []byte("2000:2000\n"), 0o644))
			f.srv.shareHealth.agentOwnerIDs = func() (int, int, bool) { return 0, 0, false }

			f.srv.checkShareHealth(ctx)
			info := f.srv.GetHealthInfo(ctx)
			require.Equal(t, "healthy", info.Checks[shareHealthCheck])
			require.Equal(t, "healthy", info.Status)
		})
	})

	t.Run("invalid settings and share not visible take precedence over owner warning", func(t *testing.T) {
		f := newShareVisibleFixture(t, "kubernetes", "10.0.0.2:/scion-workspaces/")
		marker := filepath.Join(f.hostBase, "projects", api.ExportOwnerMarkerName)
		require.NoError(t, os.WriteFile(marker, []byte("2000:2000\n"), 0o644))
		f.srv.shareHealth.agentOwnerIDs = func() (int, int, bool) { return 1000, 1000, true }

		f.srv.checkShareHealth(ctx)
		require.True(t, strings.HasPrefix(f.srv.GetHealthInfo(ctx).Checks[shareHealthCheck], shareHealthOwnerMismatch))

		// Invalid settings win over owner warning.
		f.srv.config.StorageError = invalidStorageErr()
		info := f.srv.GetHealthInfo(ctx)
		require.Equal(t, api.BrokerHealthUnavailable, info.Checks[shareHealthCheck])
		require.Equal(t, "degraded", info.Status)
		f.srv.config.StorageError = nil

		// Share not visible wins over owner warning.
		require.NoError(t, os.RemoveAll(filepath.Join(f.hostBase, "projects")))
		f.srv.checkShareHealth(ctx)
		info = f.srv.GetHealthInfo(ctx)
		require.True(t, strings.HasPrefix(info.Checks[shareHealthCheck], shareHealthNotVisible+" ("))
		require.Equal(t, "degraded", info.Status)
	})
}
