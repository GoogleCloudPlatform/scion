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
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestProjectDir creates a non-git project-config directory laid out the
// way config.GetSharedDirsBasePath expects
// (<tmp>/project-configs/<slug>__<uuid>/.scion), matching the fixtures in
// pkg/config/shared_dirs_test.go.
func newTestProjectDir(t *testing.T) string {
	t.Helper()
	tmpDir := t.TempDir()
	projectDir := filepath.Join(tmpDir, "project-configs", "test__abc12345", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	return projectDir
}

// TestResolveSharedDirs_Unset_MatchesLegacyBehavior is test (b) / AC1: with
// shared_dir_storage unset, resolveSharedDirs must produce output identical
// to calling config.EnsureSharedDirs + config.SharedDirsToVolumeMounts
// directly (today's pre-existing run.go:952-972 behaviour), and no
// SharedDirRealization.
func TestResolveSharedDirs_Unset_MatchesLegacyBehavior(t *testing.T) {
	dirs := []api.SharedDir{
		{Name: "build-cache"},
		{Name: "artifacts", ReadOnly: true},
	}

	for _, tc := range []struct {
		name  string
		sdCfg *config.V1SharedDirStorageConfig
	}{
		{name: "nil config", sdCfg: nil},
		{name: "empty backend", sdCfg: &config.V1SharedDirStorageConfig{}},
		{name: "explicit local backend", sdCfg: &config.V1SharedDirStorageConfig{Backend: "local"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			legacyDir := newTestProjectDir(t)
			require.NoError(t, config.EnsureSharedDirs(legacyDir, dirs))
			wantVolumes, err := config.SharedDirsToVolumeMounts(legacyDir, dirs, "/workspace")
			require.NoError(t, err)

			gotDir := newTestProjectDir(t)
			gotVolumes, gotRealization, err := resolveSharedDirs(tc.sdCfg, gotDir, "pid-1", "docker", dirs, "/workspace")
			require.NoError(t, err)
			assert.Nil(t, gotRealization, "no SharedDirRealization for local/unset backend")

			require.Len(t, gotVolumes, len(wantVolumes))
			for i := range wantVolumes {
				// Sources differ only in the tmpDir prefix (each test uses its
				// own project dir) — compare the parts that matter: the
				// directory name suffix and the container-side target/flags.
				assert.Equal(t, filepath.Base(wantVolumes[i].Source), filepath.Base(gotVolumes[i].Source))
				assert.Equal(t, wantVolumes[i].Target, gotVolumes[i].Target)
				assert.Equal(t, wantVolumes[i].ReadOnly, gotVolumes[i].ReadOnly)
			}
		})
	}
}

// TestResolveSharedDirs_NoDirs covers the len(dirs)==0 short-circuit that
// existed before this change (no EnsureSharedDirs/SharedDirsToVolumeMounts
// call, no SCION_VOLUMES env var to set).
func TestResolveSharedDirs_NoDirs(t *testing.T) {
	volumes, realization, err := resolveSharedDirs(nil, "/nonexistent", "pid-1", "docker", nil, "/workspace")
	require.NoError(t, err)
	assert.Nil(t, volumes)
	assert.Nil(t, realization)
}

// nfsSharedDirStorageCfg returns a valid shared_dir_storage nfs config
// rooted at mountRoot, mirroring design deploy-config-explore §3.2.1.
func nfsSharedDirStorageCfg(mountRoot string) *config.V1SharedDirStorageConfig {
	return &config.V1SharedDirStorageConfig{
		Backend: "nfs",
		NFS: &config.V1NFSConfig{
			MountRoot: mountRoot,
			Shares: []config.V1NFSShare{
				{ID: "scion-shared", Server: "10.128.15.241", Export: "/srv/scion-shared", PVName: "scion-shared"},
			},
		},
	}
}

// TestResolveSharedDirs_NFS_MissingProjectID_FailsClosed is test (c) / AC4:
// a missing hub project ID must produce an error naming the project ID,
// never a silent fallback to local disk.
func TestResolveSharedDirs_NFS_MissingProjectID_FailsClosed(t *testing.T) {
	hostBase := t.TempDir() // present, so this isn't what fails
	sdCfg := nfsSharedDirStorageCfg(filepath.Dir(hostBase))
	sdCfg.NFS.Shares[0].ID = filepath.Base(hostBase)

	dirs := []api.SharedDir{{Name: "scratchpad"}}
	volumes, realization, err := resolveSharedDirs(sdCfg, "/unused", "" /* projectID */, "docker", dirs, "/workspace")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "hub project ID")
	assert.Nil(t, volumes)
	assert.Nil(t, realization)
}

// TestResolveSharedDirs_NFS_InvalidConfig_FailsClosed exercises
// V1SharedDirStorageConfig.Validate() being consulted before any resolution
// is attempted (AC4/AC5 — a broken shared_dir_storage block must error with
// a message naming the problem, not silently resolve garbage paths).
func TestResolveSharedDirs_NFS_InvalidConfig_FailsClosed(t *testing.T) {
	sdCfg := &config.V1SharedDirStorageConfig{Backend: "nfs"} // no NFS block
	dirs := []api.SharedDir{{Name: "scratchpad"}}

	_, _, err := resolveSharedDirs(sdCfg, "/unused", "pid-1", "docker", dirs, "/workspace")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "server.shared_dir_storage")
}

// TestResolveSharedDirs_NFS_MissingHostBase_LocalContainerRuntime_FailsClosed
// is the Docker/Podman/Apple half of test (c) / AC4: when the resolved NFS
// host base does not exist on disk and the runtime bind-mounts host paths
// directly, resolveSharedDirs must error naming the missing host base
// instead of creating it (design §3.2.3: "never create the host base
// itself").
func TestResolveSharedDirs_NFS_MissingHostBase_LocalContainerRuntime_FailsClosed(t *testing.T) {
	tmpDir := t.TempDir()
	missingBase := filepath.Join(tmpDir, "does-not-exist")
	sdCfg := nfsSharedDirStorageCfg(tmpDir)
	sdCfg.NFS.Shares[0].ID = "does-not-exist"

	dirs := []api.SharedDir{{Name: "scratchpad"}}
	for _, runtimeName := range []string{"docker", "podman", "container"} {
		t.Run(runtimeName, func(t *testing.T) {
			volumes, realization, err := resolveSharedDirs(sdCfg, "/unused", "pid-1", runtimeName, dirs, "/workspace")
			require.Error(t, err)
			assert.Contains(t, err.Error(), missingBase)
			assert.Nil(t, volumes)
			assert.Nil(t, realization)

			// The host base itself must never be created.
			_, statErr := os.Stat(missingBase)
			assert.True(t, os.IsNotExist(statErr), "host base must not be created")
		})
	}
}

// TestResolveSharedDirs_NFS_MissingHostBase_Kubernetes_Succeeds is the T1
// compatibility case from design §3.3: a kubernetes-runtime broker that has
// no local copy of the NFS export (e.g. a future in-cluster broker) must
// still resolve — the K8s side never needs the host base to exist locally,
// only the PVC/subPath naming.
func TestResolveSharedDirs_NFS_MissingHostBase_Kubernetes_Succeeds(t *testing.T) {
	tmpDir := t.TempDir()
	missingBase := filepath.Join(tmpDir, "does-not-exist")
	sdCfg := nfsSharedDirStorageCfg(tmpDir)
	sdCfg.NFS.Shares[0].ID = "does-not-exist"

	dirs := []api.SharedDir{{Name: "scratchpad"}}
	volumes, realization, err := resolveSharedDirs(sdCfg, "/unused", "pid-1", "kubernetes", dirs, "/workspace")
	require.NoError(t, err)
	// The Docker-shaped mounts are still computed (design §3.2.3: "existing,
	// now wired" runs unconditionally) — they end up unused because the K8s
	// runtime mounts shared dirs via SharedDirStorage/subPath, and buildPod
	// skips RunConfig.Volumes entries whose target matches a shared dir
	// target (see TestBuildPod_SharedDirs_SkipsLocalVolumesForSharedDirTargets).
	require.Len(t, volumes, 1)
	assert.Equal(t, "/scion-volumes/scratchpad", volumes[0].Target)
	require.NotNil(t, realization)
	assert.Equal(t, "nfs", realization.Backend)
	assert.Equal(t, "scion-shared", realization.PVClaimName)
	assert.Equal(t, "projects/pid-1/shared-dirs/scratchpad", realization.SubPaths["scratchpad"])

	_, statErr := os.Stat(missingBase)
	assert.True(t, os.IsNotExist(statErr), "host base must not be created")
}

// TestResolveSharedDirs_NFS_HostBasePresent_MkdirsSharedDirs is a real
// (non-stubbed) integration check: when the resolved host base exists on
// disk, resolveSharedDirs must mkdir each shared dir (0o2775) under it —
// never the host base itself — and return a Docker bind-mount volume
// pointing at the same path used to populate the K8s subPath.
func TestResolveSharedDirs_NFS_HostBasePresent_MkdirsSharedDirs(t *testing.T) {
	mountRoot := t.TempDir()
	shareID := "scion-shared"
	hostBase := filepath.Join(mountRoot, shareID)
	require.NoError(t, os.MkdirAll(hostBase, 0o775))
	// Simulate the export root as provisioned in design §3.5.5 ("owned by
	// scion:scion mode 2775"). The kernel only ever applies setgid to a new
	// directory via inheritance from an already-setgid parent (mkdir(2)'s
	// mode argument alone can't set it) — and os.FileMode's ModeSetgid bit
	// must be OR'd in explicitly; the traditional octal literal 0o2775 is
	// not the same bit in Go's os package and os.Chmod would silently
	// ignore it.
	require.NoError(t, os.Chmod(hostBase, os.ModeSetgid|0o775))

	sdCfg := nfsSharedDirStorageCfg(mountRoot)
	sdCfg.NFS.Shares[0].ID = shareID

	dirs := []api.SharedDir{{Name: "scratchpad"}}
	volumes, realization, err := resolveSharedDirs(sdCfg, "/unused", "pid-42", "docker", dirs, "/workspace")
	require.NoError(t, err)
	require.Len(t, volumes, 1)

	wantHostPath := filepath.Join(hostBase, "projects", "pid-42", "shared-dirs", "scratchpad")
	assert.Equal(t, wantHostPath, volumes[0].Source)
	assert.Equal(t, "/scion-volumes/scratchpad", volumes[0].Target)

	info, statErr := os.Stat(wantHostPath)
	require.NoError(t, statErr, "shared dir should have been mkdir'd")
	assert.True(t, info.IsDir())
	// os.MkdirAll applies the process umask like any other mkdir, so only
	// assert the bits that matter here: setgid inherited from the already-
	// setgid host base, and the owner has full access. Exact group/other
	// bits depend on the test process umask.
	assert.NotZero(t, info.Mode()&os.ModeSetgid, "setgid should be inherited from the setgid host base")
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm()&0o700, "owner should have full rwx")

	require.NotNil(t, realization)
	assert.Equal(t, "projects/pid-42/shared-dirs/scratchpad", realization.SubPaths["scratchpad"])

	// The host base itself is a pre-existing directory that was NOT created
	// by resolveSharedDirs and must not be treated as a shared dir mount.
	assert.NotEqual(t, hostBase, wantHostPath)
}

func TestIsLocalContainerRuntime(t *testing.T) {
	for _, name := range []string{"docker", "podman", "container"} {
		assert.True(t, isLocalContainerRuntime(name), "%s should be a local-container runtime", name)
	}
	for _, name := range []string{"kubernetes", "cloudrun", "cloudrun-sandbox", ""} {
		assert.False(t, isLocalContainerRuntime(name), "%s should not be a local-container runtime", name)
	}
}
