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
	"strings"
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
// (not merely same-basename — round 1 review finding #5/#8) to calling
// config.EnsureSharedDirs + config.SharedDirsToVolumeMounts directly
// (today's pre-existing run.go:952-972 behaviour), and no
// SharedDirRealization. Uses a single project dir for both the "legacy" and
// "new" calls, since both operations are idempotent mkdirs.
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
			projectDir := newTestProjectDir(t)

			require.NoError(t, config.EnsureSharedDirs(projectDir, dirs))
			wantVolumes, err := config.SharedDirsToVolumeMounts(projectDir, dirs, "/workspace")
			require.NoError(t, err)
			require.NotEmpty(t, wantVolumes)

			gotVolumes, gotRealization, err := resolveSharedDirs(tc.sdCfg, projectDir, "pid-1", "docker", dirs, "/workspace")
			require.NoError(t, err)
			assert.Nil(t, gotRealization, "no SharedDirRealization for local/unset backend")

			assert.Equal(t, wantVolumes, gotVolumes, "byte-identical to the legacy call (AC1)")

			basePath, err := config.GetSharedDirsBasePath(projectDir)
			require.NoError(t, err)
			for _, d := range dirs {
				info, statErr := os.Stat(filepath.Join(basePath, d.Name))
				require.NoError(t, statErr, "shared dir %q should exist", d.Name)
				assert.True(t, info.IsDir())
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
// a message naming the problem, not silently resolve garbage paths). Round 1
// review finding #7: assert the *specific* message, not just the common
// "server.shared_dir_storage" prefix every error shares — otherwise removing
// the Validate() call entirely (and letting Resolve's own, different error
// through) would still pass.
func TestResolveSharedDirs_NFS_InvalidConfig_FailsClosed(t *testing.T) {
	dirs := []api.SharedDir{{Name: "scratchpad"}}

	t.Run("nil NFS block", func(t *testing.T) {
		sdCfg := &config.V1SharedDirStorageConfig{Backend: "nfs"} // no NFS block
		_, _, err := resolveSharedDirs(sdCfg, "/unused", "pid-1", "docker", dirs, "/workspace")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no nfs block is configured")
	})

	t.Run("unknown backend", func(t *testing.T) {
		for _, backend := range []string{"nsf", "NFS ", "Nfs", "garbage"} {
			t.Run(backend, func(t *testing.T) {
				sdCfg := &config.V1SharedDirStorageConfig{Backend: backend}
				_, _, err := resolveSharedDirs(sdCfg, "/unused", "pid-1", "docker", dirs, "/workspace")
				require.Error(t, err)
				assert.Contains(t, err.Error(), "must be")
			})
		}
	})
}

// TestResolveSharedDirs_NFS_ResolveLevelMisconfig_FailsClosed covers the two
// misconfigurations Validate() itself does not catch as errors on their own
// combination but that nfsBackend.Resolve would otherwise silently turn into
// a relative or malformed host path: empty mount_root and an empty
// shares[0].id (round 1 review finding #7 — "resolver-level cases").
func TestResolveSharedDirs_NFS_ResolveLevelMisconfig_FailsClosed(t *testing.T) {
	dirs := []api.SharedDir{{Name: "scratchpad"}}

	t.Run("empty mount_root", func(t *testing.T) {
		sdCfg := &config.V1SharedDirStorageConfig{
			Backend: "nfs",
			NFS: &config.V1NFSConfig{
				MountRoot: "",
				Shares:    []config.V1NFSShare{{ID: "scion-shared"}},
			},
		}
		_, _, err := resolveSharedDirs(sdCfg, "/unused", "pid-1", "docker", dirs, "/workspace")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "mount_root is empty")
	})

	t.Run("empty shares[0].id", func(t *testing.T) {
		sdCfg := &config.V1SharedDirStorageConfig{
			Backend: "nfs",
			NFS: &config.V1NFSConfig{
				MountRoot: "/srv",
				Shares:    []config.V1NFSShare{{ID: ""}},
			},
		}
		_, _, err := resolveSharedDirs(sdCfg, "/unused", "pid-1", "docker", dirs, "/workspace")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "shares[0].id is empty")
	})
}

// TestResolveSharedDirs_NFS_UnsupportedRuntime_FailsClosed is item C8/T11:
// with backend=nfs, a runtime that neither bind-mounts a host path
// (Docker/Podman/Apple) nor consumes RunConfig.SharedDirStorage
// (Kubernetes) — e.g. cloudrun — must fail closed instead of silently
// succeeding with unread volumes/realization (design G5).
func TestResolveSharedDirs_NFS_UnsupportedRuntime_FailsClosed(t *testing.T) {
	hostBase := t.TempDir()
	sdCfg := nfsSharedDirStorageCfg(filepath.Dir(hostBase))
	sdCfg.NFS.Shares[0].ID = filepath.Base(hostBase)
	dirs := []api.SharedDir{{Name: "scratchpad"}}

	for _, runtimeName := range []string{"cloudrun", "cloudrun-sandbox", "unknown-runtime"} {
		t.Run(runtimeName, func(t *testing.T) {
			volumes, realization, err := resolveSharedDirs(sdCfg, "/unused", "pid-1", runtimeName, dirs, "/workspace")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "not supported on runtime")
			assert.Contains(t, err.Error(), runtimeName)
			assert.Nil(t, volumes)
			assert.Nil(t, realization)
		})
	}
}

// TestResolveSharedDirs_NFS_TraversalProjectID_GuardBeforeMkdir is item
// C3/T4: the export-root isolation guard must run before any directory is
// created on disk. A malformed/traversal project ID must both error and
// leave no directory behind outside the export root.
func TestResolveSharedDirs_NFS_TraversalProjectID_GuardBeforeMkdir(t *testing.T) {
	mountRoot := t.TempDir()
	shareID := "scion-shared"
	hostBase := filepath.Join(mountRoot, shareID)
	require.NoError(t, os.MkdirAll(hostBase, 0o775))

	sdCfg := nfsSharedDirStorageCfg(mountRoot)
	sdCfg.NFS.Shares[0].ID = shareID
	dirs := []api.SharedDir{{Name: "scratchpad"}}

	// "projects/../../escaped/shared-dirs/scratchpad" cleans to a path
	// outside hostBase entirely (a sibling of hostBase's parent).
	traversalProjectID := "../../escaped"
	sdRelPath := filepath.Join("projects", traversalProjectID, "shared-dirs", "scratchpad")
	escapedPath := filepath.Join(hostBase, sdRelPath)
	require.False(t, strings.HasPrefix(escapedPath, hostBase+string(filepath.Separator)),
		"test fixture must actually escape hostBase, got %q", escapedPath)

	volumes, realization, err := resolveSharedDirs(sdCfg, "/unused", traversalProjectID, "docker", dirs, "/workspace")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "isolation violation")
	assert.Nil(t, volumes)
	assert.Nil(t, realization)

	_, statErr := os.Stat(escapedPath)
	assert.True(t, os.IsNotExist(statErr), "escaped path must not have been created: %s", escapedPath)
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
// disk, resolveSharedDirs must mkdir each shared dir it creates and chmod it
// to 0o2775 (design §3.5(5)) — never the host base itself — and return a
// Docker bind-mount volume pointing at the same path used to populate the
// K8s subPath.
func TestResolveSharedDirs_NFS_HostBasePresent_MkdirsSharedDirs(t *testing.T) {
	mountRoot := t.TempDir()
	shareID := "scion-shared"
	hostBase := filepath.Join(mountRoot, shareID)
	require.NoError(t, os.MkdirAll(hostBase, 0o775))

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
	// resolveSharedDirs chmods the leaf it created explicitly (item 13):
	// unlike mkdir(2) (which only ever applies setgid via inheritance from
	// an already-setgid parent), chmod(2) is not subject to the process
	// umask, so the resulting mode is deterministic here regardless of the
	// host base's own mode.
	assert.Equal(t, os.FileMode(0o775), info.Mode().Perm(), "leaf permission bits")
	assert.NotZero(t, info.Mode()&os.ModeSetgid, "leaf setgid bit")

	require.NotNil(t, realization)
	assert.Equal(t, "projects/pid-42/shared-dirs/scratchpad", realization.SubPaths["scratchpad"])

	// The host base itself is a pre-existing directory that was NOT created
	// by resolveSharedDirs and must not be treated as a shared dir mount.
	assert.NotEqual(t, hostBase, wantHostPath)
}

// TestResolveSharedDirs_NFS_PreexistingSharedDir_NotChmoded is the other
// half of item 13: resolveSharedDirs must not chmod a shared dir that
// already existed before this call (an operator or a previous agent may
// have set it up deliberately).
func TestResolveSharedDirs_NFS_PreexistingSharedDir_NotChmoded(t *testing.T) {
	mountRoot := t.TempDir()
	shareID := "scion-shared"
	hostBase := filepath.Join(mountRoot, shareID)
	require.NoError(t, os.MkdirAll(hostBase, 0o775))

	leaf := filepath.Join(hostBase, "projects", "pid-42", "shared-dirs", "scratchpad")
	require.NoError(t, os.MkdirAll(leaf, 0o700))
	before, statErr := os.Stat(leaf)
	require.NoError(t, statErr)
	require.Zero(t, before.Mode()&os.ModeSetgid, "fixture sanity check: must start without setgid")

	sdCfg := nfsSharedDirStorageCfg(mountRoot)
	sdCfg.NFS.Shares[0].ID = shareID
	dirs := []api.SharedDir{{Name: "scratchpad"}}

	_, _, err := resolveSharedDirs(sdCfg, "/unused", "pid-42", "docker", dirs, "/workspace")
	require.NoError(t, err)

	after, statErr := os.Stat(leaf)
	require.NoError(t, statErr)
	assert.Equal(t, before.Mode(), after.Mode(), "pre-existing shared dir must not be chmod'd")
}

func TestIsLocalContainerRuntime(t *testing.T) {
	for _, name := range []string{"docker", "podman", "container"} {
		assert.True(t, isLocalContainerRuntime(name), "%s should be a local-container runtime", name)
	}
	for _, name := range []string{"kubernetes", "cloudrun", "cloudrun-sandbox", ""} {
		assert.False(t, isLocalContainerRuntime(name), "%s should not be a local-container runtime", name)
	}
}

func TestIsKubernetesRuntime(t *testing.T) {
	assert.True(t, isKubernetesRuntime("kubernetes"))
	for _, name := range []string{"docker", "podman", "container", "cloudrun", "cloudrun-sandbox", ""} {
		assert.False(t, isKubernetesRuntime(name), "%s should not be the kubernetes runtime", name)
	}
}
