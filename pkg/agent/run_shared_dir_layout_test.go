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

// These tests exercise unix-only, Windows-gated agent storage code.
//go:build unix

package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Shared dirs placed by the broker's Layout (design fs-cleanup §P2, one backend for workspaces and shared dirs):
// one backend for workspaces and shared dirs, read from the global
// settings file, on every runtime. These tests drive Manager.Start end to
// end.

const sdsProfileShareID = "share-1"

func (f sharedDirStorageRunFixture) writeRawGlobalSettings(t *testing.T, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(f.globalScionDir, "settings.yaml"), []byte(content), 0o644))
}

type sdsCapture struct {
	ran int
	cfg runtime.RunConfig
}

func newSDSMockRuntime(name string, c *sdsCapture) *runtime.MockRuntime {
	return &runtime.MockRuntime{
		NameFunc: func() string { return name },
		RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
			c.ran++
			c.cfg = cfg
			return "mock-id", nil
		},
	}
}

func sdsStartOpts(f sharedDirStorageRunFixture, agent, profile string) api.StartOptions {
	return api.StartOptions{
		Name:        agent,
		ProjectPath: f.projectScionDir,
		Profile:     profile,
		NoAuth:      true,
		Env: map[string]string{
			"SCION_AGENT_ID":   "id-" + agent,
			"SCION_PROJECT_ID": "pid-sds",
		},
		SharedDirs: []api.SharedDir{{Name: "data"}},
	}
}

func sdsDataDirSource(cfg runtime.RunConfig) string {
	for _, v := range cfg.Volumes {
		if v.Target == "/scion-volumes/data" {
			return v.Source
		}
	}
	return ""
}

// sdsModeAndGID returns the permission bits (including setgid) and owning
// group of path.
func sdsModeAndGID(t *testing.T, path string) (os.FileMode, uint32) {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	st, ok := info.Sys().(*syscall.Stat_t)
	require.True(t, ok)
	return os.FileMode(st.Mode & 0o7777), st.Gid
}

// sdsStorageYAML returns a global settings file whose server section is
// storageYAML (indented under server:), with a docker profile "local" and a
// kubernetes profile "gke".
func sdsStorageYAML(storageYAML string) string {
	return `schema_version: "1"
active_profile: local
runtimes:
  docker:
    type: docker
  k8s:
    type: kubernetes
profiles:
  local:
    runtime: docker
  gke:
    runtime: k8s
server:
` + storageYAML
}

func sdsNFSBlock(key, backendLine, mountRoot string) string {
	return fmt.Sprintf(`  %s:
%s    nfs:
      mount_root: %s
      shares:
        - id: %s
          server: 10.0.0.2
          export: /vol
          pv_name: pv-1
`, key, backendLine, mountRoot, sdsProfileShareID)
}

// D9 closed: a Docker start bind-mounts every shared dir from
// <mount_root>/<share>/<subpath_root>/<project ID>/shared-dirs/<name> when
// the storage block is on nfs, whether it is server.project_storage or
// only the deprecated server.workspace_storage (which Docker used to
// ignore for shared dirs), and the leaf is created on the export.
func TestStartSharedDirLayout_DockerBindMountsFromExport(t *testing.T) {
	for name, storage := range map[string]func(mountRoot string) string{
		"project_storage nfs": func(m string) string {
			return sdsNFSBlock("project_storage", "    backend: nfs\n", m)
		},
		"legacy workspace_storage nfs only": func(m string) string {
			return sdsNFSBlock("workspace_storage", "    backend: nfs\n", m)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newSharedDirStorageRunFixture(t)
			mountRoot := filepath.Join(f.tmpDir, "nfs")
			require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, sdsProfileShareID), 0o775))
			f.writeRawGlobalSettings(t, sdsStorageYAML(storage(mountRoot)))

			var docker sdsCapture
			_, err := NewManager(newSDSMockRuntime("docker", &docker)).Start(context.Background(), sdsStartOpts(f, "docker-agent", "local"))
			require.NoError(t, err)
			require.Equal(t, 1, docker.ran)

			resolvedRoot, err := filepath.EvalSymlinks(mountRoot)
			require.NoError(t, err)
			want := filepath.Join(resolvedRoot, sdsProfileShareID, "projects", "pid-sds", "shared-dirs", "data")
			assert.Equal(t, want, sdsDataDirSource(docker.cfg))
			info, err := os.Stat(want)
			require.NoError(t, err)
			assert.True(t, info.IsDir(), "the leaf is created on the export")
			require.NotNil(t, docker.cfg.SharedDirStorage)
			assert.Equal(t, "nfs", docker.cfg.SharedDirStorage.Backend)
		})
	}
}

// The legacy split (server.shared_dir_storage nfs, workspace not on nfs)
// still places shared dirs on the shared_dir_storage export, on Docker and
// on Kubernetes, with that share's claim.
func TestStartSharedDirLayout_LegacySplitUsesSharedDirExport(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)
	mountRoot := filepath.Join(f.tmpDir, "nfs")
	require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, sdsProfileShareID), 0o775))
	f.writeRawGlobalSettings(t, sdsStorageYAML(sdsNFSBlock("shared_dir_storage", "    backend: nfs\n", mountRoot)))
	resolvedRoot, err := filepath.EvalSymlinks(mountRoot)
	require.NoError(t, err)
	want := filepath.Join(resolvedRoot, sdsProfileShareID, "projects", "pid-sds", "shared-dirs", "data")

	var docker sdsCapture
	_, err = NewManager(newSDSMockRuntime("docker", &docker)).Start(context.Background(), sdsStartOpts(f, "docker-agent", "local"))
	require.NoError(t, err)
	assert.Equal(t, want, sdsDataDirSource(docker.cfg))

	var k8s sdsCapture
	_, err = NewManager(newSDSMockRuntime("kubernetes", &k8s)).Start(context.Background(), sdsStartOpts(f, "gke-agent", "gke"))
	require.NoError(t, err)
	require.NotNil(t, k8s.cfg.SharedDirStorage)
	assert.Equal(t, "nfs", k8s.cfg.SharedDirStorage.Backend)
	assert.Equal(t, "pv-1", k8s.cfg.SharedDirStorage.PVClaimName)
	assert.Equal(t, "projects/pid-sds/shared-dirs/data", k8s.cfg.SharedDirStorage.SubPaths["data"])
	assert.Empty(t, k8s.cfg.WorkspaceBackendName, "the workspace stays where it is")
}

// The removed per-profile override is refused at agent start: with the
// global block on the local backend, a profile's shared_dir_storage_backend
// nfs is no longer applied, so starting that profile's agent would place
// its shared dirs on per-dir claims instead of the export. The start fails
// before the runtime runs anything, naming the key (ptone/scion#6371; this
// test asserted the silent ignore before).
func TestStartSharedDirLayout_ProfileOverrideRefused(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)
	mountRoot := filepath.Join(f.tmpDir, "nfs")
	require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, sdsProfileShareID), 0o775))
	f.writeRawGlobalSettings(t, `schema_version: "1"
active_profile: local
runtimes:
  k8s:
    type: kubernetes
profiles:
  gke:
    runtime: k8s
    shared_dir_storage_backend: nfs
server:
`+sdsNFSBlock("shared_dir_storage", "    backend: local\n", mountRoot))

	var k8s sdsCapture
	_, err := NewManager(newSDSMockRuntime("kubernetes", &k8s)).Start(context.Background(), sdsStartOpts(f, "gke-agent", "gke"))
	var se *config.StorageMigrationRequiredError
	require.ErrorAs(t, err, &se)
	assert.Contains(t, se.Error(), "profiles.gke.shared_dir_storage_backend")
	assert.Zero(t, k8s.ran, "the runtime must not run the agent")
	_, statErr := os.Stat(filepath.Join(mountRoot, sdsProfileShareID, "projects"))
	assert.True(t, os.IsNotExist(statErr), "nothing is created on the export")
}

// A missing mount of the share fails a Docker start (it bind-mounts the
// host path) before anything runs, naming the path.
func TestStartSharedDirLayout_MissingMountFailsClosed(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)
	mountRoot := filepath.Join(f.tmpDir, "not-mounted") // never created
	f.writeRawGlobalSettings(t, sdsStorageYAML(sdsNFSBlock("project_storage", "    backend: nfs\n", mountRoot)))

	var docker sdsCapture
	_, err := NewManager(newSDSMockRuntime("docker", &docker)).Start(context.Background(), sdsStartOpts(f, "docker-agent", "local"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "require this broker to have the share mounted")
	assert.Contains(t, err.Error(), filepath.Join(mountRoot, sdsProfileShareID))
	assert.Equal(t, 0, docker.ran)
	_, statErr := os.Stat(mountRoot)
	assert.True(t, os.IsNotExist(statErr), "the mount root must not be created")
}

// A global settings file that fails to load but mentions
// server.project_storage fails closed, like one mentioning
// shared_dir_storage.
func TestStartSharedDirLayout_MalformedSettingsMentioningProjectStorageFailsClosed(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)
	f.writeRawGlobalSettings(t, "schema_version: \"1\"\nserver:\n  project_storage:\n    backend: nfs\n  : : not yaml [\n")

	var docker sdsCapture
	_, err := NewManager(newSDSMockRuntime("docker", &docker)).Start(context.Background(), sdsStartOpts(f, "docker-agent", "local"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "project_storage")
	assert.Equal(t, 0, docker.ran)
}

// The broker creates the shared-dir chain on the export: setgid 2755
// upper directories and a setgid, group-writable 2775 leaf, all in the
// group of the setgid share directory. Nothing is chowned.
func TestStartSharedDirLayout_NFSLeafModes(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)
	mountRoot := filepath.Join(f.tmpDir, "nfs")
	hostBase := filepath.Join(mountRoot, sdsProfileShareID)
	require.NoError(t, os.MkdirAll(hostBase, 0o775))
	require.NoError(t, os.Chmod(hostBase, 0o2775))
	_, baseGID := sdsModeAndGID(t, hostBase)
	f.writeRawGlobalSettings(t, sdsStorageYAML(sdsNFSBlock("project_storage", "    backend: nfs\n", mountRoot)))

	var k8s sdsCapture
	_, err := NewManager(newSDSMockRuntime("kubernetes", &k8s)).Start(context.Background(), sdsStartOpts(f, "gke-agent", "gke"))
	require.NoError(t, err)
	require.Equal(t, 1, k8s.ran)

	for _, rel := range []string{"projects", "projects/pid-sds", "projects/pid-sds/shared-dirs"} {
		mode, gid := sdsModeAndGID(t, filepath.Join(hostBase, rel))
		assert.Equal(t, os.FileMode(0o2755), mode, "upper directory %s", rel)
		assert.Equal(t, baseGID, gid, "upper directory %s inherits the share directory's group", rel)
	}
	mode, gid := sdsModeAndGID(t, filepath.Join(hostBase, "projects/pid-sds/shared-dirs/data"))
	assert.Equal(t, os.FileMode(0o2775), mode, "leaf")
	assert.Equal(t, baseGID, gid, "leaf inherits the share directory's group")
}

// No storage block: the local layout, unchanged (no realization, no path
// on any export).
func TestStartSharedDirLayout_LocalBackendUnchanged(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)
	f.writeRawGlobalSettings(t, sdsStorageYAML("  hub:\n    auto_suspend_stalled: false\n"))
	var docker sdsCapture
	_, err := NewManager(newSDSMockRuntime("docker", &docker)).Start(context.Background(), sdsStartOpts(f, "docker-agent", "local"))
	require.NoError(t, err)
	assert.Nil(t, docker.cfg.SharedDirStorage)
	projectDir, err := config.GetResolvedProjectDir(f.projectScionDir)
	require.NoError(t, err)
	want, err := config.GetSharedDirPath(projectDir, "data")
	require.NoError(t, err)
	assert.Equal(t, want, sdsDataDirSource(docker.cfg))
}
