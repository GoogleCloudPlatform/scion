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
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestProjectDeletionServiceForNFSCleanup builds a ProjectDeletionService
// with just enough set (a logger and the shared-dir Layout) to exercise
// cleanupNFSSharedDirTree directly -- that method touches only svc.logger,
// svc.sharedDirLayout and the filesystem, never svc.store or svc.authz.
func newTestProjectDeletionServiceForNFSCleanup(layout projectfs.Layout, layoutErr error) *ProjectDeletionService {
	return &ProjectDeletionService{
		logger:          slog.Default(),
		sharedDirLayout: func() (projectfs.Layout, error) { return layout, layoutErr },
	}
}

// nfsCleanupLayout is the shared-dir Layout of an nfs storage block whose
// share scion-shared is mounted under mountRoot.
func nfsCleanupLayout(t *testing.T, mountRoot string) projectfs.Layout {
	t.Helper()
	layout, err := config.NFSLayout(&config.V1NFSConfig{
		MountRoot: mountRoot,
		Shares:    []config.V1NFSShare{{ID: "scion-shared", PVName: "pv"}},
	})
	require.NoError(t, err)
	return layout
}

// On a shared Layout, project delete removes the project's shared-dir tree
// from the share (and nothing of any other project).
func TestCleanupNFSSharedDirTree_SharedLayout_RemovesTree(t *testing.T) {
	mountRoot := t.TempDir()
	projectID := "pid-nfs-backend"
	leaf := filepath.Join(mountRoot, "scion-shared", "projects", projectID, "shared-dirs", "scratchpad")
	require.NoError(t, os.MkdirAll(leaf, 0o2775))
	require.NoError(t, os.WriteFile(filepath.Join(leaf, "gone.txt"), []byte("must be removed"), 0o644))
	other := filepath.Join(mountRoot, "scion-shared", "projects", "pid-other", "shared-dirs", "scratchpad")
	require.NoError(t, os.MkdirAll(other, 0o2775))

	svc := newTestProjectDeletionServiceForNFSCleanup(nfsCleanupLayout(t, mountRoot), nil)
	svc.cleanupNFSSharedDirTree(context.Background(), projectID)

	_, err := os.Stat(filepath.Join(mountRoot, "scion-shared", "projects", projectID))
	assert.True(t, os.IsNotExist(err), "the project's shared-dir tree must be removed on a shared Layout")
	_, err = os.Stat(other)
	assert.NoError(t, err, "other projects' trees must survive")
}

// The legacy split (shared dirs on the shared_dir_storage export while
// workspaces are not on nfs) cleans the export the shared dirs are on.
func TestCleanupNFSSharedDirTree_LegacySplit_RemovesTreeFromSharedDirExport(t *testing.T) {
	mountRoot := t.TempDir()
	projectID := "pid-split"
	leaf := filepath.Join(mountRoot, "scion-shared", "projects", projectID, "shared-dirs", "scratchpad")
	require.NoError(t, os.MkdirAll(leaf, 0o2775))

	ps, _, err := config.SynthesizeProjectStorage(nil, nil, &config.V1SharedDirStorageConfig{Backend: "nfs",
		NFS: &config.V1NFSConfig{MountRoot: mountRoot, Shares: []config.V1NFSShare{{ID: "scion-shared", PVName: "pv"}}}}, nil)
	require.NoError(t, err)
	full, err := config.ProjectStorageLayout(ps, config.DefaultVolumeMountBase)
	require.NoError(t, err)
	require.NotNil(t, full.SharedDirs, "fixture must be the legacy split")

	svc := newTestProjectDeletionServiceForNFSCleanup(full.SharedDirLayout(), nil)
	svc.cleanupNFSSharedDirTree(context.Background(), projectID)

	_, err = os.Stat(filepath.Join(mountRoot, "scion-shared", "projects", projectID))
	assert.True(t, os.IsNotExist(err))
}

// On the local Layout no project has a tree on any share (one backend for
// every agent), so cleanup is a silent no-op, even when an nfs export with
// a tree for the project is mounted.
func TestCleanupNFSSharedDirTree_LocalLayout_NoOp(t *testing.T) {
	mountRoot := t.TempDir()
	projectID := "pid-local"
	leaf := filepath.Join(mountRoot, "scion-shared", "projects", projectID, "shared-dirs", "scratchpad")
	require.NoError(t, os.MkdirAll(leaf, 0o2775))

	buf := captureSlog(t)
	svc := newTestProjectDeletionServiceForNFSCleanup(projectfs.Layout{Backend: projectfs.BackendLocal}, nil)
	svc.cleanupNFSSharedDirTree(context.Background(), projectID)

	assert.NotContains(t, buf.String(), projectID)
	_, err := os.Stat(leaf)
	assert.NoError(t, err, "nothing is removed on the local Layout")
}

// A missing share mount on a shared Layout logs an ERROR naming the
// project and never creates the export path.
func TestCleanupNFSSharedDirTree_MissingExport_LogsErrorAndSkips(t *testing.T) {
	mountRoot := filepath.Join(t.TempDir(), "not-mounted")

	buf := captureSlog(t)
	svc := newTestProjectDeletionServiceForNFSCleanup(nfsCleanupLayout(t, mountRoot), nil)
	svc.cleanupNFSSharedDirTree(context.Background(), "pid-missing-export")

	logged := buf.String()
	assert.Contains(t, logged, "level=ERROR")
	assert.Contains(t, logged, "pid-missing-export")
	_, err := os.Stat(mountRoot)
	assert.True(t, os.IsNotExist(err), "cleanup must not create the export path")
}

// A symlinked project directory on the share is refused (DeleteProjectTree's
// symlink refusal): nothing it points at is removed.
func TestCleanupNFSSharedDirTree_SymlinkedProjectDirRefused(t *testing.T) {
	mountRoot := t.TempDir()
	outside := t.TempDir()
	victim := filepath.Join(outside, "shared-dirs", "scratchpad")
	require.NoError(t, os.MkdirAll(victim, 0o755))
	projects := filepath.Join(mountRoot, "scion-shared", "projects")
	require.NoError(t, os.MkdirAll(projects, 0o755))
	require.NoError(t, os.Symlink(outside, filepath.Join(projects, "pid-symlink")))

	buf := captureSlog(t)
	svc := newTestProjectDeletionServiceForNFSCleanup(nfsCleanupLayout(t, mountRoot), nil)
	svc.cleanupNFSSharedDirTree(context.Background(), "pid-symlink")

	assert.Contains(t, buf.String(), "level=ERROR")
	_, err := os.Stat(victim)
	assert.NoError(t, err, "a symlink target must never be removed")
}

// A Layout that cannot be built logs an ERROR naming the project and skips;
// an invalid project ID is refused before the filesystem is touched; a
// service without a Layout does nothing.
func TestCleanupNFSSharedDirTree_FailClosedCases(t *testing.T) {
	t.Run("layout error", func(t *testing.T) {
		buf := captureSlog(t)
		svc := newTestProjectDeletionServiceForNFSCleanup(projectfs.Layout{}, errors.New("bad storage block"))
		svc.cleanupNFSSharedDirTree(context.Background(), "pid-layout-error")
		assert.Contains(t, buf.String(), "level=ERROR")
		assert.Contains(t, buf.String(), "pid-layout-error")
	})
	t.Run("invalid project ID", func(t *testing.T) {
		mountRoot := t.TempDir()
		buf := captureSlog(t)
		svc := newTestProjectDeletionServiceForNFSCleanup(nfsCleanupLayout(t, mountRoot), nil)
		svc.cleanupNFSSharedDirTree(context.Background(), "../victim")
		assert.Contains(t, buf.String(), "level=ERROR")
	})
	t.Run("no layout", func(t *testing.T) {
		buf := captureSlog(t)
		(&ProjectDeletionService{logger: slog.Default()}).cleanupNFSSharedDirTree(context.Background(), "pid-no-layout")
		assert.Empty(t, buf.String())
	})
}

// An invalid project ID never reaches the filesystem: cleanup refuses
// before resolving anything, and the share's other trees are untouched.
func TestCleanupNFSSharedDirTree_InvalidProjectIDRefused(t *testing.T) {
	mountRoot := t.TempDir()
	other := filepath.Join(mountRoot, "scion-shared", "projects", "pid-other", "shared-dirs", "scratchpad")
	require.NoError(t, os.MkdirAll(other, 0o2775))
	svc := newTestProjectDeletionServiceForNFSCleanup(nfsCleanupLayout(t, mountRoot), nil)
	for _, id := range []string{"../pid-other", "", "a/b", ".."} {
		svc.cleanupNFSSharedDirTree(context.Background(), id)
	}
	_, err := os.Stat(other)
	assert.NoError(t, err, "no other project's tree is touched")
}
