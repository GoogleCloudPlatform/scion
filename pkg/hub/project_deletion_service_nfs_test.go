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
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestProjectDeletionServiceForNFSCleanup builds a ProjectDeletionService
// with just enough set (a logger) to exercise cleanupNFSSharedDirTree
// directly -- that method touches only svc.logger and the filesystem/global
// settings, never svc.store or svc.authz.
func newTestProjectDeletionServiceForNFSCleanup() *ProjectDeletionService {
	return &ProjectDeletionService{logger: slog.Default()}
}

// TestCleanupNFSSharedDirTree_LocalBackendWithNFSBlockPresent_TreeSurvives:
// backend: local with a populated nfs sub-block underneath it (e.g. left
// over from a prior nfs configuration) must not trigger any NFS cleanup --
// cleanupNFSSharedDirTree gates strictly on sdCfg.Backend == "nfs", matching
// resolveNFSSharedDirPath's own gate. The project's shared-dir tree on the
// NFS export must survive project deletion untouched.
func TestCleanupNFSSharedDirTree_LocalBackendWithNFSBlockPresent_TreeSurvives(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	globalScionDir := filepath.Join(tmpHome, ".scion")
	require.NoError(t, os.MkdirAll(globalScionDir, 0755))

	mountRoot := filepath.Join(tmpHome, "srv")
	settingsYAML := `schema_version: "1"
server:
  shared_dir_storage:
    backend: local
    nfs:
      mount_root: ` + mountRoot + `
      shares:
        - id: scion-shared
          pv_name: pv
`
	require.NoError(t, os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(settingsYAML), 0644))

	projectID := "pid-local-with-nfs-block"
	leaf := filepath.Join(mountRoot, "scion-shared", "projects", projectID, "shared-dirs", "scratchpad")
	require.NoError(t, os.MkdirAll(leaf, 0o2775))
	require.NoError(t, os.WriteFile(filepath.Join(leaf, "keep.txt"), []byte("must survive"), 0o644))

	svc := newTestProjectDeletionServiceForNFSCleanup()
	svc.cleanupNFSSharedDirTree(context.Background(), projectID)

	content, err := os.ReadFile(filepath.Join(leaf, "keep.txt"))
	require.NoError(t, err, "the shared-dir tree must survive when backend is local, regardless of a populated nfs block")
	assert.Equal(t, "must survive", string(content))
}

// TestCleanupNFSSharedDirTree_NFSBackend_RemovesTree is the positive
// counterpart: with backend: nfs actually configured, cleanup must remove
// the project's shared-dir tree, confirming the local-backend test above is
// actually distinguishing on Backend and not just never doing anything.
func TestCleanupNFSSharedDirTree_NFSBackend_RemovesTree(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	globalScionDir := filepath.Join(tmpHome, ".scion")
	require.NoError(t, os.MkdirAll(globalScionDir, 0755))

	mountRoot := filepath.Join(tmpHome, "srv")
	settingsYAML := `schema_version: "1"
server:
  shared_dir_storage:
    backend: nfs
    nfs:
      mount_root: ` + mountRoot + `
      shares:
        - id: scion-shared
          pv_name: pv
`
	require.NoError(t, os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(settingsYAML), 0644))

	projectID := "pid-nfs-backend"
	leaf := filepath.Join(mountRoot, "scion-shared", "projects", projectID, "shared-dirs", "scratchpad")
	require.NoError(t, os.MkdirAll(leaf, 0o2775))
	require.NoError(t, os.WriteFile(filepath.Join(leaf, "gone.txt"), []byte("must be removed"), 0o644))

	svc := newTestProjectDeletionServiceForNFSCleanup()
	svc.cleanupNFSSharedDirTree(context.Background(), projectID)

	_, err := os.Stat(filepath.Join(mountRoot, "scion-shared", "projects", projectID))
	assert.True(t, os.IsNotExist(err), "the project's NFS shared-dir tree must be removed when backend is nfs")
}

// TestCleanupNFSSharedDirTree_UnreadableSettingsMentioningKey_LogsErrorAndSkips:
// a global settings file that fails to parse, but plausibly mentions
// shared_dir_storage, must log exactly one ERROR record naming the project
// and skip cleanup -- never silently do nothing, and never touch either the
// conventional local project-configs layout or a pre-existing NFS export
// tree it can no longer address.
func TestCleanupNFSSharedDirTree_UnreadableSettingsMentioningKey_LogsErrorAndSkips(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	globalScionDir := filepath.Join(tmpHome, ".scion")
	require.NoError(t, os.MkdirAll(globalScionDir, 0755))

	mountRoot := filepath.Join(tmpHome, "srv")
	projectID := "pid-unreadable-mentions-key"
	exportLeaf := filepath.Join(mountRoot, "scion-shared", "projects", projectID, "shared-dirs", "scratchpad")
	require.NoError(t, os.MkdirAll(exportLeaf, 0o2775))
	require.NoError(t, os.WriteFile(filepath.Join(exportLeaf, "keep.txt"), []byte("export sentinel"), 0o644))

	localLeaf := config.SharedDirHostPath(tmpHome, "unreadable-mentions-key", projectID, "scratchpad")
	require.NoError(t, os.MkdirAll(localLeaf, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(localLeaf, "keep.txt"), []byte("local sentinel"), 0o644))

	// A v1-tagged file that mentions shared_dir_storage but fails to parse.
	require.NoError(t, os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"),
		[]byte("schema_version: \"1\"\nserver:\n  shared_dir_storage:\n    backend: nfs\n    nfs: [unterminated\n"), 0644))

	buf := captureSlog(t)
	svc := newTestProjectDeletionServiceForNFSCleanup()
	svc.cleanupNFSSharedDirTree(context.Background(), projectID)

	logged := buf.String()
	assert.Contains(t, logged, "level=ERROR", "an unreadable settings file mentioning shared_dir_storage must log at ERROR")
	assert.Contains(t, logged, projectID, "the ERROR record must name the project")

	exportContent, err := os.ReadFile(filepath.Join(exportLeaf, "keep.txt"))
	require.NoError(t, err, "the export tree must survive: cleanup could not even resolve it")
	assert.Equal(t, "export sentinel", string(exportContent))

	localContent, err := os.ReadFile(filepath.Join(localLeaf, "keep.txt"))
	require.NoError(t, err, "the local project-configs tree must never be touched by NFS cleanup")
	assert.Equal(t, "local sentinel", string(localContent))
}

// TestCleanupNFSSharedDirTree_LegacyFormatMentioningKey_LogsErrorAndSkips:
// the same fail-closed logging requirement, this time for a legacy-format
// settings file (no schema_version) that still mentions shared_dir_storage.
func TestCleanupNFSSharedDirTree_LegacyFormatMentioningKey_LogsErrorAndSkips(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	globalScionDir := filepath.Join(tmpHome, ".scion")
	require.NoError(t, os.MkdirAll(globalScionDir, 0755))

	mountRoot := filepath.Join(tmpHome, "srv")
	projectID := "pid-legacy-mentions-key"
	exportLeaf := filepath.Join(mountRoot, "scion-shared", "projects", projectID, "shared-dirs", "scratchpad")
	require.NoError(t, os.MkdirAll(exportLeaf, 0o2775))
	require.NoError(t, os.WriteFile(filepath.Join(exportLeaf, "keep.txt"), []byte("export sentinel"), 0o644))

	localLeaf := config.SharedDirHostPath(tmpHome, "legacy-mentions-key", projectID, "scratchpad")
	require.NoError(t, os.MkdirAll(localLeaf, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(localLeaf, "keep.txt"), []byte("local sentinel"), 0o644))

	// A legacy-format file (no schema_version) that still mentions the key.
	require.NoError(t, os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"),
		[]byte("server:\n  shared_dir_storage:\n    backend: nfs\n"), 0644))

	buf := captureSlog(t)
	svc := newTestProjectDeletionServiceForNFSCleanup()
	svc.cleanupNFSSharedDirTree(context.Background(), projectID)

	logged := buf.String()
	assert.Contains(t, logged, "level=ERROR", "a legacy-format settings file mentioning shared_dir_storage must log at ERROR")
	assert.Contains(t, logged, projectID, "the ERROR record must name the project")

	exportContent, err := os.ReadFile(filepath.Join(exportLeaf, "keep.txt"))
	require.NoError(t, err, "the export tree must survive: cleanup could not even resolve it")
	assert.Equal(t, "export sentinel", string(exportContent))

	localContent, err := os.ReadFile(filepath.Join(localLeaf, "keep.txt"))
	require.NoError(t, err, "the local project-configs tree must never be touched by NFS cleanup")
	assert.Equal(t, "local sentinel", string(localContent))
}

// sdsOverrideSettingsYAML is a global settings file whose global backend is
// local while the gke profile overrides it to nfs.
func sdsOverrideSettingsYAML(mountRoot string) string {
	return `schema_version: "1"
runtimes:
  k8s:
    type: kubernetes
profiles:
  gke:
    runtime: k8s
    shared_dir_storage_backend: nfs
server:
  shared_dir_storage:
    backend: local
    nfs:
      mount_root: ` + mountRoot + `
      shares:
        - id: scion-shared
          pv_name: pv
`
}

// When only a profile override selects nfs, cleanup removes the project's
// tree from the export if the export is reachable on this host.
func TestCleanupNFSSharedDirTree_ProfileOverrideOnly_RemovesTree(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	globalScionDir := filepath.Join(tmpHome, ".scion")
	require.NoError(t, os.MkdirAll(globalScionDir, 0755))
	mountRoot := filepath.Join(tmpHome, "srv")
	require.NoError(t, os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(sdsOverrideSettingsYAML(mountRoot)), 0644))

	projectID := "pid-override-only"
	leaf := filepath.Join(mountRoot, "scion-shared", "projects", projectID, "shared-dirs", "scratchpad")
	require.NoError(t, os.MkdirAll(leaf, 0o2775))
	require.NoError(t, os.WriteFile(filepath.Join(leaf, "gone.txt"), []byte("x"), 0o644))

	svc := newTestProjectDeletionServiceForNFSCleanup()
	svc.cleanupNFSSharedDirTree(context.Background(), projectID)

	_, err := os.Stat(filepath.Join(mountRoot, "scion-shared", "projects", projectID))
	assert.True(t, os.IsNotExist(err), "an nfs profile override must make delete clean the project's tree")
}

// When only a profile override selects nfs and the export is not mounted
// on this host, cleanup warns and returns; it never logs an error and
// never creates anything.
func TestCleanupNFSSharedDirTree_ProfileOverrideOnly_MissingExport_WarnsAndSkips(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	globalScionDir := filepath.Join(tmpHome, ".scion")
	require.NoError(t, os.MkdirAll(globalScionDir, 0755))
	mountRoot := filepath.Join(tmpHome, "not-mounted")
	require.NoError(t, os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(sdsOverrideSettingsYAML(mountRoot)), 0644))

	buf := captureSlog(t)
	svc := newTestProjectDeletionServiceForNFSCleanup()
	svc.cleanupNFSSharedDirTree(context.Background(), "pid-missing-export")

	logged := buf.String()
	assert.Contains(t, logged, "level=WARN")
	assert.Contains(t, logged, "not reachable on this host")
	assert.Contains(t, logged, "pid-missing-export")
	assert.NotContains(t, logged, "level=ERROR")
	_, err := os.Stat(mountRoot)
	assert.True(t, os.IsNotExist(err), "cleanup must not create the export path")
}

// An nfs override held only in the DB settings overlay (the file has
// none) also makes delete clean the project's tree.
func TestCleanupNFSSharedDirTree_OverlayOverride_RemovesTree(t *testing.T) {
	old := config.GetGlobalSettingsOverlay()
	t.Cleanup(func() { config.SetGlobalSettingsOverlay(old) })

	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	globalScionDir := filepath.Join(tmpHome, ".scion")
	require.NoError(t, os.MkdirAll(globalScionDir, 0755))
	mountRoot := filepath.Join(tmpHome, "srv")
	require.NoError(t, os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
server:
  shared_dir_storage:
    backend: local
    nfs:
      mount_root: `+mountRoot+`
      shares:
        - id: scion-shared
          pv_name: pv
`), 0644))

	o := config.NewSettingsOverlay()
	o.Update(map[string]config.V1RuntimeConfig{"k8s": {Type: "kubernetes"}},
		map[string]config.V1ProfileConfig{"gke": {Runtime: "k8s", SharedDirStorageBackend: "nfs"}}, nil, "")
	config.SetGlobalSettingsOverlay(o)

	projectID := "pid-overlay-override"
	leaf := filepath.Join(mountRoot, "scion-shared", "projects", projectID, "shared-dirs", "scratchpad")
	require.NoError(t, os.MkdirAll(leaf, 0o2775))

	svc := newTestProjectDeletionServiceForNFSCleanup()
	svc.cleanupNFSSharedDirTree(context.Background(), projectID)

	_, err := os.Stat(filepath.Join(mountRoot, "scion-shared", "projects", projectID))
	assert.True(t, os.IsNotExist(err))
}
