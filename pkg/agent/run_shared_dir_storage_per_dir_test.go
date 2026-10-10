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

package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Per-dir shared_dir_storage backends driven through Manager.Start: one
// project with a notes dir on nfs and a gocache dir on local disk.

// perDirSettingsYAML returns a global settings file whose "local" docker
// profile and "gke" kubernetes profile both carry profileExtra (indented
// profile keys), with a local global backend and an nfs block at
// mountRoot.
func perDirSettingsYAML(mountRoot, profileExtra string) string {
	return fmt.Sprintf(`schema_version: "1"
active_profile: local
runtimes:
  docker:
    type: docker
  k8s:
    type: kubernetes
profiles:
  local:
    runtime: docker
%[2]s  gke:
    runtime: k8s
%[2]sserver:
  shared_dir_storage:
    backend: local
    nfs:
      mount_root: %[1]s
      shares:
        - id: %[3]s
          pv_name: pv-1
`, mountRoot, profileExtra, sdsProfileShareID)
}

const notesOnNFS = "    shared_dir_storage_backends:\n      notes: nfs\n      not-in-project: nfs\n"

func perDirStartOpts(f sharedDirStorageRunFixture, agent, profile string) api.StartOptions {
	opts := sdsStartOpts(f, agent, profile)
	opts.SharedDirs = []api.SharedDir{{Name: "notes"}, {Name: "gocache"}}
	return opts
}

func perDirRecord(t *testing.T, f sharedDirStorageRunFixture, agent string) *sharedDirStorageRecord {
	t.Helper()
	rec, err := loadSharedDirStorageRecord(config.ResolveAgentDir(f.projectScionDir, agent))
	require.NoError(t, err)
	return rec
}

func volumeSource(cfg runtime.RunConfig, target string) string {
	for _, v := range cfg.Volumes {
		if v.Target == target {
			return v.Source
		}
	}
	return ""
}

func newPerDirFixture(t *testing.T) (sharedDirStorageRunFixture, string) {
	f := newSharedDirStorageRunFixture(t)
	mountRoot := filepath.Join(f.tmpDir, "nfs")
	require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, sdsProfileShareID), 0o775))
	return f, mountRoot
}

// Kubernetes: only notes is mounted from the shared claim; gocache keeps
// its local backend. The record names notes.
func TestStartSharedDirStoragePerDir_Kubernetes(t *testing.T) {
	f, mountRoot := newPerDirFixture(t)
	f.writeRawGlobalSettings(t, perDirSettingsYAML(mountRoot, notesOnNFS))

	var k8s sdsCapture
	_, err := NewManager(newSDSMockRuntime("kubernetes", &k8s)).Start(context.Background(), perDirStartOpts(f, "gke-agent", "gke"))
	require.NoError(t, err)
	require.Equal(t, 1, k8s.ran)
	sds := k8s.cfg.SharedDirStorage
	require.NotNil(t, sds)
	assert.Equal(t, map[string]string{"notes": "projects/pid-sds/shared-dirs/notes"}, sds.SubPaths)
	assert.Equal(t, map[string]bool{"gocache": true}, sds.LocalDirs)
	assert.True(t, sds.Serves("notes"))
	assert.False(t, sds.Serves("gocache"))

	rec := perDirRecord(t, f, "gke-agent")
	require.NotNil(t, rec)
	assert.Equal(t, "local", rec.Backend)
	assert.Equal(t, map[string]string{"notes": "nfs"}, rec.Dirs, "an entry for a dir the project lacks is not recorded")
}

// Docker: notes is bind-mounted from the export, gocache from the local
// layout.
func TestStartSharedDirStoragePerDir_Docker(t *testing.T) {
	f, mountRoot := newPerDirFixture(t)
	f.writeRawGlobalSettings(t, perDirSettingsYAML(mountRoot, notesOnNFS))

	var docker sdsCapture
	_, err := NewManager(newSDSMockRuntime("docker", &docker)).Start(context.Background(), perDirStartOpts(f, "docker-agent", "local"))
	require.NoError(t, err)
	require.Equal(t, 1, docker.ran)
	notes := volumeSource(docker.cfg, "/scion-volumes/notes")
	gocache := volumeSource(docker.cfg, "/scion-volumes/gocache")
	require.NotEmpty(t, notes)
	require.NotEmpty(t, gocache)
	assert.Contains(t, notes, filepath.Join(sdsProfileShareID, "projects", "pid-sds", "shared-dirs", "notes"))
	assert.NotContains(t, gocache, mountRoot, "gocache keeps the local layout")
}

// An agent whose record predates per-dir backends (no dirs) keeps every
// dir on its recorded backend after per-dir settings are added.
func TestStartSharedDirStoragePerDir_OldRecordKeepsEveryDir(t *testing.T) {
	f, mountRoot := newPerDirFixture(t)
	f.writeRawGlobalSettings(t, perDirSettingsYAML(mountRoot, ""))

	var first sdsCapture
	_, err := NewManager(newSDSMockRuntime("kubernetes", &first)).Start(context.Background(), perDirStartOpts(f, "old-agent", "gke"))
	require.NoError(t, err)
	require.Nil(t, first.cfg.SharedDirStorage)
	data, err := os.ReadFile(filepath.Join(config.ResolveAgentDir(f.projectScionDir, "old-agent"), sharedDirStorageRecordFile))
	require.NoError(t, err)
	assert.Equal(t, `{"backend":"local"}`+"\n", string(data), "a uniform agent's record is unchanged in shape")

	f.writeRawGlobalSettings(t, perDirSettingsYAML(mountRoot, notesOnNFS))

	var restart sdsCapture
	_, err = NewManager(newSDSMockRuntime("kubernetes", &restart)).Start(context.Background(), perDirStartOpts(f, "old-agent", ""))
	require.NoError(t, err)
	require.Equal(t, 1, restart.ran)
	assert.Nil(t, restart.cfg.SharedDirStorage, "no dir moves for an agent recorded before per-dir settings")
	rec := perDirRecord(t, f, "old-agent")
	require.NotNil(t, rec)
	assert.Equal(t, "local", rec.Backend)
	assert.Empty(t, rec.Dirs, "the record is not rewritten")
}

// An old-format nfs record keeps every dir on nfs, even with a per-dir
// local entry in settings.
func TestStartSharedDirStoragePerDir_OldNFSRecordKeepsEveryDir(t *testing.T) {
	f, mountRoot := newPerDirFixture(t)
	f.writeRawGlobalSettings(t, perDirSettingsYAML(mountRoot, "    shared_dir_storage_backend: nfs\n"))
	var first sdsCapture
	_, err := NewManager(newSDSMockRuntime("kubernetes", &first)).Start(context.Background(), perDirStartOpts(f, "old-agent", "gke"))
	require.NoError(t, err)
	data, err := os.ReadFile(filepath.Join(config.ResolveAgentDir(f.projectScionDir, "old-agent"), sharedDirStorageRecordFile))
	require.NoError(t, err)
	require.Equal(t, `{"backend":"nfs"}`+"\n", string(data))

	f.writeRawGlobalSettings(t, perDirSettingsYAML(mountRoot, "    shared_dir_storage_backends:\n      gocache: local\n"))
	var k8s sdsCapture
	_, err = NewManager(newSDSMockRuntime("kubernetes", &k8s)).Start(context.Background(), perDirStartOpts(f, "old-agent", ""))
	require.NoError(t, err)
	sds := k8s.cfg.SharedDirStorage
	require.NotNil(t, sds)
	assert.Len(t, sds.SubPaths, 2)
	assert.Empty(t, sds.LocalDirs)
}

// A per-dir record is kept after the setting that produced it is removed,
// and a dir added to the project later uses the record's backend.
func TestStartSharedDirStoragePerDir_RecordKeptAndAddedDirUsesRecordBackend(t *testing.T) {
	f, mountRoot := newPerDirFixture(t)
	f.writeRawGlobalSettings(t, perDirSettingsYAML(mountRoot, notesOnNFS))

	var first sdsCapture
	_, err := NewManager(newSDSMockRuntime("kubernetes", &first)).Start(context.Background(), perDirStartOpts(f, "agent", "gke"))
	require.NoError(t, err)

	f.writeRawGlobalSettings(t, perDirSettingsYAML(mountRoot, "    shared_dir_storage_backends:\n      later: nfs\n"))

	opts := perDirStartOpts(f, "agent", "")
	opts.SharedDirs = append(opts.SharedDirs, api.SharedDir{Name: "later"})
	var restart sdsCapture
	_, err = NewManager(newSDSMockRuntime("kubernetes", &restart)).Start(context.Background(), opts)
	require.NoError(t, err)
	sds := restart.cfg.SharedDirStorage
	require.NotNil(t, sds)
	assert.Equal(t, map[string]string{"notes": "projects/pid-sds/shared-dirs/notes"}, sds.SubPaths)
	assert.Equal(t, map[string]bool{"gocache": true, "later": true}, sds.LocalDirs)
}

// A profile single value nfs overrides a runtime entry's gocache=local;
// naming gocache in the profile's own map pins it to local.
func TestStartSharedDirStoragePerDir_ProfileSingleBeatsRuntimeEntry(t *testing.T) {
	f, mountRoot := newPerDirFixture(t)
	settings := func(profileMap string) string {
		return fmt.Sprintf(`schema_version: "1"
active_profile: gke
runtimes:
  k8s:
    type: kubernetes
    shared_dir_storage_backends:
      gocache: local
profiles:
  gke:
    runtime: k8s
    shared_dir_storage_backend: nfs
%sserver:
  shared_dir_storage:
    backend: local
    nfs:
      mount_root: %s
      shares:
        - id: %s
          pv_name: pv-1
`, profileMap, mountRoot, sdsProfileShareID)
	}
	f.writeRawGlobalSettings(t, settings(""))
	var unpinned sdsCapture
	_, err := NewManager(newSDSMockRuntime("kubernetes", &unpinned)).Start(context.Background(), perDirStartOpts(f, "unpinned", "gke"))
	require.NoError(t, err)
	require.NotNil(t, unpinned.cfg.SharedDirStorage)
	assert.Len(t, unpinned.cfg.SharedDirStorage.SubPaths, 2, "gocache follows the profile single value")

	f.writeRawGlobalSettings(t, settings("    shared_dir_storage_backends:\n      gocache: local\n"))
	var pinned sdsCapture
	_, err = NewManager(newSDSMockRuntime("kubernetes", &pinned)).Start(context.Background(), perDirStartOpts(f, "pinned", "gke"))
	require.NoError(t, err)
	require.NotNil(t, pinned.cfg.SharedDirStorage)
	assert.Equal(t, map[string]bool{"gocache": true}, pinned.cfg.SharedDirStorage.LocalDirs)
	rec := perDirRecord(t, f, "pinned")
	assert.Equal(t, "nfs", rec.Backend)
	assert.Equal(t, map[string]string{"gocache": "local"}, rec.Dirs)
}

// With an NFS workspace and per-dir backends, Start pre-creates the local
// dirs on the workspace claim, where the pod mounts them, and not the dirs
// served by shared_dir_storage nfs.
func TestStartSharedDirStoragePerDir_NFSWorkspacePreCreatesLocalDirsOnClaim(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)
	wsMountRoot := filepath.Join(f.tmpDir, "ws-nfs")
	require.NoError(t, os.MkdirAll(filepath.Join(wsMountRoot, "share-1"), 0o755))
	sdMountRoot := filepath.Join(f.tmpDir, "sd-nfs")
	require.NoError(t, os.MkdirAll(filepath.Join(sdMountRoot, "sd-share"), 0o755))
	f.writeRawGlobalSettings(t, fmt.Sprintf(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
    shared_dir_storage_backends:
      notes: nfs
server:
`+nfsWorkspaceStartYAML+`  shared_dir_storage:
    backend: local
    nfs:
      mount_root: %s
      shares:
        - id: sd-share
          pv_name: sd-pv
`, wsMountRoot, sdMountRoot))

	var k8s sdsCapture
	_, err := NewManager(newSDSMockRuntime("kubernetes", &k8s)).Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: f.projectScionDir,
		NoAuth:      true,
		Env: map[string]string{
			"SCION_AGENT_ID":   "agent-2530",
			"SCION_PROJECT_ID": testNFSWorkspaceProjectID,
		},
		GitClone:   &api.GitCloneConfig{URL: "https://example.com/repo.git"},
		SharedDirs: []api.SharedDir{{Name: "notes"}, {Name: "gocache"}},
	})
	require.NoError(t, err)
	require.Equal(t, 1, k8s.ran)
	require.NotNil(t, k8s.cfg.SharedDirStorage)
	assert.Equal(t, map[string]bool{"gocache": true}, k8s.cfg.SharedDirStorage.LocalDirs)

	claimDirs := filepath.Join(wsMountRoot, "share-1", "projects", testNFSWorkspaceProjectID, "shared-dirs")
	info, statErr := os.Stat(filepath.Join(claimDirs, "gocache"))
	require.NoError(t, statErr, "the local dir is pre-created on the workspace claim")
	assert.True(t, info.IsDir())
	_, statErr = os.Stat(filepath.Join(claimDirs, "notes"))
	assert.True(t, os.IsNotExist(statErr), "the nfs dir is not created on the workspace claim")
	_, statErr = os.Stat(filepath.Join(sdMountRoot, "sd-share", "projects", testNFSWorkspaceProjectID, "shared-dirs", "notes"))
	assert.NoError(t, statErr, "the nfs dir is created on the shared-dir export")
}
