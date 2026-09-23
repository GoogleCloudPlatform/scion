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
	"golang.org/x/sys/unix"
)

// withZeroUmask temporarily sets the process umask to 0 so an exact
// mkdir(at) permission-bit assertion is deterministic regardless of the
// ambient shell/CI umask, and restores the original umask on cleanup. Only
// needed by tests that assert the LITERAL requested mode of a
// freshly-created directory (round 6 disposition item 2 / N1: the
// intermediate-directory mode assertion).
func withZeroUmask(t *testing.T) {
	t.Helper()
	old := unix.Umask(0)
	t.Cleanup(func() { unix.Umask(old) })
}

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
// config.SharedDirsToVolumeMounts directly (today's pre-existing
// run.go:952-972 behaviour), and no SharedDirRealization.
//
// resolveSharedDirs is called FIRST, on a fresh project dir with no
// pre-existing shared dirs, and the "dirs exist" assertion runs against
// that call's own output — round 2 review finding T4 (mutant S9): the
// previous version called config.EnsureSharedDirs itself before
// resolveSharedDirs, so the "shared dir should exist" assertion passed
// whether or not resolveSharedDirs's local branch created anything. The
// `want` volumes are computed afterwards via SharedDirsToVolumeMounts alone
// (no mkdir — it does no I/O), against the same project dir, so the two
// calls' paths match exactly.
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

			gotVolumes, gotRealization, err := resolveSharedDirs(tc.sdCfg, projectDir, "pid-1", "docker", dirs, "/workspace")
			require.NoError(t, err)
			assert.Nil(t, gotRealization, "no SharedDirRealization for local/unset backend")

			basePath, err := config.GetSharedDirsBasePath(projectDir)
			require.NoError(t, err)
			for _, d := range dirs {
				info, statErr := os.Stat(filepath.Join(basePath, d.Name))
				require.NoError(t, statErr, "resolveSharedDirs should have created shared dir %q", d.Name)
				assert.True(t, info.IsDir())
			}

			wantVolumes, err := config.SharedDirsToVolumeMounts(projectDir, dirs, "/workspace")
			require.NoError(t, err)
			require.NotEmpty(t, wantVolumes)

			assert.Equal(t, wantVolumes, gotVolumes, "byte-identical to the legacy call (AC1)")
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

// newResolvedTempDir returns a fresh temp directory with any symlinks in its
// own path already resolved (round 4 review finding C2): t.TempDir() is not
// guaranteed symlink-free — e.g. macOS puts it under /var, which is itself a
// symlink to /private/var — so building an "expected" path from the raw
// t.TempDir() value and comparing it against an already-resolved Source
// would fail spuriously there. CI is Linux-only, where this is a no-op, but
// this repo ships an Apple `container` runtime and has macOS developers.
func newResolvedTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	return dir
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

// TestResolveSharedDirs_NFS_ResolveLevelMisconfig_FailsClosed covers empty
// mount_root and an empty shares[0].id: Validate() (called from
// resolveSharedDirs before Resolve) does catch both of these, so they never
// reach nfsBackend.Resolve, which would otherwise silently turn them into a
// relative or malformed host path (round 1 review finding #7; comment
// corrected per round 2 review finding C4, which noted Validate does catch
// these — the "resolver-level" framing describes what Resolve would do if
// this validation were ever skipped, not a gap in Validate itself).
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
// C3/T4: no directory may be created on disk before every validation and
// guard has passed. A traversal project ID must both error and leave no
// directory behind outside the export root. Since round 2 (security review
// finding F1), a "/"-containing project ID is now rejected by
// validSharedDirProjectID before Resolve is ever called — an earlier and
// stronger guarantee than the old export-root guard (ValidateNotExportRoot,
// still exercised by TestSharedDirStorage_NFS_PoC_* below for the inputs
// that reach it), but the "nothing created outside the export" property
// this test asserts still holds.
func TestResolveSharedDirs_NFS_TraversalProjectID_GuardBeforeMkdir(t *testing.T) {
	mountRoot := newResolvedTempDir(t)
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
	assert.Contains(t, err.Error(), "invalid hub project ID")
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
	// Round 6 disposition item 2 / N1 (optional, done): assert the
	// intermediate directories' created mode too, not just the leaf's.
	// mkdirat's mode argument IS subject to the process umask (unlike the
	// leaf's explicit fchmod below), so force umask=0 for this test to make
	// the assertion deterministic.
	withZeroUmask(t)

	mountRoot := newResolvedTempDir(t)
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

	// The intermediate directories the walk created (projects,
	// projects/pid-42, projects/pid-42/shared-dirs) get the plain mkdirat
	// mode (0o775, no setgid) — only the leaf is ever chmod'd/setgid'd
	// (design §3.5(5); round 3 review item 13).
	for _, rel := range []string{"projects", filepath.Join("projects", "pid-42"), filepath.Join("projects", "pid-42", "shared-dirs")} {
		intermediate := filepath.Join(hostBase, rel)
		info, statErr := os.Stat(intermediate)
		require.NoError(t, statErr, "intermediate dir %q should have been mkdir'd", rel)
		assert.Equal(t, os.FileMode(0o775), info.Mode().Perm(), "intermediate dir %q permission bits", rel)
		assert.Zero(t, info.Mode()&os.ModeSetgid, "intermediate dir %q must not be setgid", rel)
	}

	require.NotNil(t, realization)
	assert.Equal(t, "projects/pid-42/shared-dirs/scratchpad", realization.SubPaths["scratchpad"])

	// The host base itself is a pre-existing directory that was NOT created
	// by resolveSharedDirs and must not be treated as a shared dir mount.
	assert.NotEqual(t, hostBase, wantHostPath)
}

// TestResolveSharedDirs_NFS_ExistingParentChain_NewLeafGetsSetgid is round 6
// disposition item 2 (tst L1, mutant W12): every prior
// setgid/2775 assertion ran on a chain created fully fresh by this same
// call, which cannot distinguish "alreadyExisted reflects the LEAF" (what
// the code does) from a mutant that took `alreadyExisted` from an earlier
// walked component instead. If it did, a SECOND shared dir resolved into an
// already-existing projects/<pid>/shared-dirs (the common case: another
// shared dir, or another agent, in the same project) would wrongly be
// treated as "already existed" and skip the fchmod, leaving a brand-new
// leaf at the plain mkdirat mode (0o775, no setgid) instead of 0o2775 —
// violating design §3.5(5) group inheritance silently, with every existing
// test still green. This test pre-creates the parent chain (including a
// sibling shared dir) and resolves a NEW leaf into it.
func TestResolveSharedDirs_NFS_ExistingParentChain_NewLeafGetsSetgid(t *testing.T) {
	withZeroUmask(t)

	mountRoot := newResolvedTempDir(t)
	shareID := "scion-shared"
	hostBase := filepath.Join(mountRoot, shareID)
	sharedDirsParent := filepath.Join(hostBase, "projects", "pid-42", "shared-dirs")
	require.NoError(t, os.MkdirAll(sharedDirsParent, 0o750))

	// A sibling shared dir already exists — e.g. from an earlier call for a
	// different shared-dir name, or a previous agent in the same project.
	existingSibling := filepath.Join(sharedDirsParent, "existing")
	require.NoError(t, os.MkdirAll(existingSibling, 0o2775))

	pidDir := filepath.Join(hostBase, "projects", "pid-42")
	beforeParent, statErr := os.Stat(sharedDirsParent)
	require.NoError(t, statErr)
	beforePid, statErr := os.Stat(pidDir)
	require.NoError(t, statErr)
	beforeSibling, statErr := os.Stat(existingSibling)
	require.NoError(t, statErr)

	sdCfg := nfsSharedDirStorageCfg(mountRoot)
	sdCfg.NFS.Shares[0].ID = shareID
	dirs := []api.SharedDir{{Name: "newdir"}}

	volumes, _, err := resolveSharedDirs(sdCfg, "/unused", "pid-42", "docker", dirs, "/workspace")
	require.NoError(t, err)
	require.Len(t, volumes, 1)

	newLeaf := filepath.Join(sharedDirsParent, "newdir")
	info, statErr := os.Stat(newLeaf)
	require.NoError(t, statErr, "new shared dir should have been mkdir'd")
	assert.Equal(t, os.FileMode(0o775), info.Mode().Perm(), "new leaf permission bits")
	assert.NotZero(t, info.Mode()&os.ModeSetgid,
		"new leaf must be setgid even though its parent chain already existed (mutant W12)")

	// Nothing that already existed was touched.
	afterParent, statErr := os.Stat(sharedDirsParent)
	require.NoError(t, statErr)
	assert.Equal(t, beforeParent.Mode(), afterParent.Mode(), "pre-existing shared-dirs parent must not be chmod'd")

	afterPid, statErr := os.Stat(pidDir)
	require.NoError(t, statErr)
	assert.Equal(t, beforePid.Mode(), afterPid.Mode(), "pre-existing pid directory must not be chmod'd")

	afterSibling, statErr := os.Stat(existingSibling)
	require.NoError(t, statErr)
	assert.Equal(t, beforeSibling.Mode(), afterSibling.Mode(), "pre-existing sibling shared dir must not be chmod'd")
}

// TestResolveSharedDirs_NFS_PreexistingSharedDir_NotChmoded is the other
// half of item 13: resolveSharedDirs must not chmod a shared dir that
// already existed before this call (an operator or a previous agent may
// have set it up deliberately).
func TestResolveSharedDirs_NFS_PreexistingSharedDir_NotChmoded(t *testing.T) {
	mountRoot := newResolvedTempDir(t)
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

// TestResolveSharedDirs_NFS_PoC_TraversalNamesAndProjectID is round 2
// security review finding F1 (HIGH): a shared-dir name or a project ID that
// climbs out of the project's own subtree in the NFS layout
// (<HostBase>/<subpath_root>/<projectID>/shared-dirs/<name>) must be
// rejected before Resolve, for both a local-container and a kubernetes
// runtime name. Confirms the three published PoC inputs, that nothing is
// created anywhere under the export (round 3 review finding C2/T1 part 2 —
// the previous version only compared mountRoot's direct-child count, which
// can't change no matter where inside the pre-existing hostBase a rejected
// PoC would have created something), and — per round 3 finding T1 part 1 —
// that the specific layer expected to catch each input is the one that
// actually does (api.ValidateSharedDirs for the name PoCs,
// validSharedDirProjectID for the project-ID PoC), not just "some layer".
func TestResolveSharedDirs_NFS_PoC_TraversalNamesAndProjectID(t *testing.T) {
	pocs := []struct {
		name        string
		dirName     string
		projectID   string
		wantErrSubs string // the specific validation layer that must catch this input
	}{
		// name escapes to every project's shared dirs (no victim ID needed).
		{name: "name climbs to the projects root", dirName: "../../../projects", projectID: "pid-1", wantErrSubs: "invalid name"},
		// name escapes to one specific victim project.
		{name: "name climbs into a victim project", dirName: "../../victim/shared-dirs/scratchpad", projectID: "pid-1", wantErrSubs: "invalid name"},
		// project ID itself escapes into a victim project's subtree.
		{name: "project ID climbs into a victim project", dirName: "scratchpad", projectID: "../victim", wantErrSubs: "invalid hub project ID"},
	}

	for _, poc := range pocs {
		t.Run(poc.name, func(t *testing.T) {
			for _, runtimeName := range []string{"docker", "kubernetes"} {
				t.Run(runtimeName, func(t *testing.T) {
					mountRoot := newResolvedTempDir(t)
					shareID := "scion-shared"
					hostBase := filepath.Join(mountRoot, shareID)
					require.NoError(t, os.MkdirAll(hostBase, 0o775))

					sdCfg := nfsSharedDirStorageCfg(mountRoot)
					sdCfg.NFS.Shares[0].ID = shareID
					dirs := []api.SharedDir{{Name: poc.dirName}}

					volumes, realization, err := resolveSharedDirs(
						sdCfg, "/unused", poc.projectID, runtimeName, dirs, "/workspace")
					require.Error(t, err, "PoC input must be rejected")
					assert.Contains(t, err.Error(), poc.wantErrSubs,
						"expected the primary validation layer to catch this input, not just some layer")
					assert.Nil(t, volumes)
					assert.Nil(t, realization)

					assertDirEmpty(t, hostBase)
				})
			}
		})
	}
}

// TestConfineSharedDir and TestValidSharedDirProjectID_RejectsTraversal
// moved to pkg/shareddirs (as TestConfineLeaf and
// TestValidProjectID_RejectsTraversal) in Phase 2 item 1 (pure move),
// alongside the ConfineLeaf/ValidProjectID functions they test. Assertions
// are unchanged; only the package and the call sites' names moved.

// TestResolveSharedDirs_NFS_SymlinkedLeaf_FailsClosed is round 2 security
// review finding S-F3 (superseded in round 5 by the race-free component
// walk in shared_dir_storage_unix.go): a shared dir whose leaf directory is
// a symlink (plantable by anything that can write to the export, e.g.
// another all_squash-ed NFS client) pointing outside the export must be
// refused — the component walk's openat(O_NOFOLLOW) refuses to open or
// create through the existing symlink at all — and nothing outside the
// export may be touched.
func TestResolveSharedDirs_NFS_SymlinkedLeaf_FailsClosed(t *testing.T) {
	mountRoot := newResolvedTempDir(t)
	shareID := "scion-shared"
	hostBase := filepath.Join(mountRoot, shareID)
	leafParent := filepath.Join(hostBase, "projects", "pid-1", "shared-dirs")
	require.NoError(t, os.MkdirAll(leafParent, 0o775))

	outside := t.TempDir()
	leaf := filepath.Join(leafParent, "scratchpad")
	require.NoError(t, os.Symlink(outside, leaf))

	before, statErr := os.Lstat(leaf)
	require.NoError(t, statErr)

	sdCfg := nfsSharedDirStorageCfg(mountRoot)
	sdCfg.NFS.Shares[0].ID = shareID
	dirs := []api.SharedDir{{Name: "scratchpad"}}

	volumes, realization, err := resolveSharedDirs(sdCfg, "/unused", "pid-1", "docker", dirs, "/workspace")
	require.Error(t, err, "a leaf symlink pointing outside the export must be refused")
	// Round 4 review nit T4/S-N2 (still pinned after the round 5 rewrite):
	// the component walk's openat(O_NOFOLLOW) on the leaf is what refuses
	// this, before anything is created or chmod'd.
	assert.Contains(t, err.Error(), "is a symlink")
	assert.Nil(t, volumes)
	assert.Nil(t, realization)

	// The pre-existing symlink is untouched — chmod must never have run.
	after, statErr := os.Lstat(leaf)
	require.NoError(t, statErr)
	assert.Equal(t, before.Mode(), after.Mode(), "symlinked leaf must not be chmod'd")
	assert.NotZero(t, after.Mode()&os.ModeSymlink, "leaf should still be a symlink")

	// Nothing was created inside the symlink's target (disposition 7': "do
	// not create anything outside the host base on any failure path").
	assertDirEmpty(t, outside)
}

// TestResolveSharedDirs_NFS_SymlinkedIntermediate_FailsClosed is the other
// half of S-F3: a symlinked intermediate component (here, the project
// directory itself) pointing OUTSIDE the export must be refused before
// anything is created inside its target. Round 5 review finding C1=T1=S-L2:
// this is now caught by openat(O_NOFOLLOW) in
// createSharedDirViaComponentWalk, the same layer that catches every other
// symlinked-component case in this file — in-base or outside, leaf or
// intermediate — since O_NOFOLLOW refuses ANY symlink at that exact
// component regardless of where it points.
func TestResolveSharedDirs_NFS_SymlinkedIntermediate_FailsClosed(t *testing.T) {
	mountRoot := newResolvedTempDir(t)
	shareID := "scion-shared"
	hostBase := filepath.Join(mountRoot, shareID)
	projectsDir := filepath.Join(hostBase, "projects")
	require.NoError(t, os.MkdirAll(projectsDir, 0o775))

	outside := t.TempDir()
	require.NoError(t, os.Symlink(outside, filepath.Join(projectsDir, "pid-1")))

	sdCfg := nfsSharedDirStorageCfg(mountRoot)
	sdCfg.NFS.Shares[0].ID = shareID
	dirs := []api.SharedDir{{Name: "scratchpad"}}

	volumes, realization, err := resolveSharedDirs(sdCfg, "/unused", "pid-1", "docker", dirs, "/workspace")
	require.Error(t, err, "a symlinked intermediate component pointing outside the export must be refused")
	// Round 4 review nit T4/S-N2 (still pinned after the round 5 rewrite):
	// the component walk's O_NOFOLLOW open is the primary layer.
	assert.Contains(t, err.Error(), "is a symlink")
	assert.Nil(t, volumes)
	assert.Nil(t, realization)

	// openat(O_NOFOLLOW) refuses to traverse the symlink at all, so nothing
	// is ever created inside its target.
	assertDirEmpty(t, outside)
}

// TestResolveSharedDirs_NFS_SymlinkedShareDirsComponent_FailsClosed extends
// the intermediate-symlink case to the "shared-dirs" component itself,
// pointing OUTSIDE the export.
func TestResolveSharedDirs_NFS_SymlinkedShareDirsComponent_FailsClosed(t *testing.T) {
	mountRoot := newResolvedTempDir(t)
	shareID := "scion-shared"
	hostBase := filepath.Join(mountRoot, shareID)
	projectDir := filepath.Join(hostBase, "projects", "pid-1")
	require.NoError(t, os.MkdirAll(projectDir, 0o775))

	outside := t.TempDir()
	require.NoError(t, os.Symlink(outside, filepath.Join(projectDir, "shared-dirs")))

	sdCfg := nfsSharedDirStorageCfg(mountRoot)
	sdCfg.NFS.Shares[0].ID = shareID
	dirs := []api.SharedDir{{Name: "scratchpad"}}

	volumes, realization, err := resolveSharedDirs(sdCfg, "/unused", "pid-1", "docker", dirs, "/workspace")
	require.Error(t, err, "a symlinked shared-dirs component pointing outside the export must be refused")
	assert.Contains(t, err.Error(), "is a symlink")
	assert.Nil(t, volumes)
	assert.Nil(t, realization)
	assertDirEmpty(t, outside)
}

// TestResolveSharedDirs_NFS_ComponentSymlinkToExportRoot_FailsClosed covers
// the "component -> export root" shape round 5 disposition item 2 lists
// explicitly: a component that resolves back to the host base itself must
// be refused, and nothing may be created at the export root as a side
// effect.
func TestResolveSharedDirs_NFS_ComponentSymlinkToExportRoot_FailsClosed(t *testing.T) {
	mountRoot := newResolvedTempDir(t)
	shareID := "scion-shared"
	hostBase := filepath.Join(mountRoot, shareID)
	projectDir := filepath.Join(hostBase, "projects", "pid-1")
	require.NoError(t, os.MkdirAll(projectDir, 0o775))

	// "shared-dirs" resolves to the host base itself.
	require.NoError(t, os.Symlink(filepath.Join("..", ".."), filepath.Join(projectDir, "shared-dirs")))

	sdCfg := nfsSharedDirStorageCfg(mountRoot)
	sdCfg.NFS.Shares[0].ID = shareID
	dirs := []api.SharedDir{{Name: "scratchpad"}}

	volumes, realization, err := resolveSharedDirs(sdCfg, "/unused", "pid-1", "docker", dirs, "/workspace")
	require.Error(t, err, "a component symlinked to the export root must be refused")
	assert.Contains(t, err.Error(), "is a symlink")
	assert.Nil(t, volumes)
	assert.Nil(t, realization)

	_, statErr := os.Stat(filepath.Join(hostBase, "scratchpad"))
	assert.True(t, os.IsNotExist(statErr), "nothing should be created at the export root")
}

// TestResolveSharedDirs_NFS_SubPathRootComponentSymlinkToDot_FailsClosed is
// round 6 review finding #4 (hy-rev-6): the FIRST component the walk
// processes is subpath_root ("projects" by default). There was no unit test
// pinning that O_NOFOLLOW applies uniformly starting on the very first
// iteration — a mutant that skipped it only for the first component would
// have survived. Here "projects" itself is a symlink resolving back to the
// host base (".") instead of a real directory.
func TestResolveSharedDirs_NFS_SubPathRootComponentSymlinkToDot_FailsClosed(t *testing.T) {
	mountRoot := newResolvedTempDir(t)
	shareID := "scion-shared"
	hostBase := filepath.Join(mountRoot, shareID)
	require.NoError(t, os.MkdirAll(hostBase, 0o775))
	require.NoError(t, os.Symlink(".", filepath.Join(hostBase, "projects")))

	sdCfg := nfsSharedDirStorageCfg(mountRoot)
	sdCfg.NFS.Shares[0].ID = shareID
	dirs := []api.SharedDir{{Name: "scratchpad"}}

	volumes, realization, err := resolveSharedDirs(sdCfg, "/unused", "pid-1", "docker", dirs, "/workspace")
	require.Error(t, err, "a symlinked subpath_root (first walk component) resolving to the export root must be refused")
	assert.Contains(t, err.Error(), "is a symlink")
	assert.Nil(t, volumes)
	assert.Nil(t, realization)

	_, statErr := os.Stat(filepath.Join(hostBase, "pid-1"))
	assert.True(t, os.IsNotExist(statErr), "nothing should be created at the export root")
}

// TestResolveSharedDirs_NFS_SubPathRootComponentSymlinkedOutside_FailsClosed
// is the outside-pointing half of round 6 finding #4: the first walk
// component (subpath_root) is a symlink escaping the export entirely, not
// just resolving back inside it.
func TestResolveSharedDirs_NFS_SubPathRootComponentSymlinkedOutside_FailsClosed(t *testing.T) {
	mountRoot := newResolvedTempDir(t)
	shareID := "scion-shared"
	hostBase := filepath.Join(mountRoot, shareID)
	require.NoError(t, os.MkdirAll(hostBase, 0o775))

	outside := t.TempDir()
	require.NoError(t, os.Symlink(outside, filepath.Join(hostBase, "projects")))

	sdCfg := nfsSharedDirStorageCfg(mountRoot)
	sdCfg.NFS.Shares[0].ID = shareID
	dirs := []api.SharedDir{{Name: "scratchpad"}}

	volumes, realization, err := resolveSharedDirs(sdCfg, "/unused", "pid-1", "docker", dirs, "/workspace")
	require.Error(t, err, "a symlinked subpath_root (first walk component) pointing outside the export must be refused")
	assert.Contains(t, err.Error(), "is a symlink")
	assert.Nil(t, volumes)
	assert.Nil(t, realization)
	assertDirEmpty(t, outside)
}

// TestResolveSharedDirs_NFS_PidComponentSymlinkToDotDot_FailsClosed is round
// 6 review finding #4's second listed shape: projects/<pid> -> ".." (a
// relative symlink resolving to the export root, the two-dot form, as
// distinct from the existing "resolves to the host base via an absolute
// target" and "-> victim" shapes already covered above).
func TestResolveSharedDirs_NFS_PidComponentSymlinkToDotDot_FailsClosed(t *testing.T) {
	mountRoot := newResolvedTempDir(t)
	shareID := "scion-shared"
	hostBase := filepath.Join(mountRoot, shareID)
	projectsDir := filepath.Join(hostBase, "projects")
	require.NoError(t, os.MkdirAll(projectsDir, 0o775))
	require.NoError(t, os.Symlink("..", filepath.Join(projectsDir, "pid-1")))

	sdCfg := nfsSharedDirStorageCfg(mountRoot)
	sdCfg.NFS.Shares[0].ID = shareID
	dirs := []api.SharedDir{{Name: "scratchpad"}}

	volumes, realization, err := resolveSharedDirs(sdCfg, "/unused", "pid-1", "docker", dirs, "/workspace")
	require.Error(t, err, "a pid component symlinked to \"..\" (the export root) must be refused")
	assert.Contains(t, err.Error(), "is a symlink")
	assert.Nil(t, volumes)
	assert.Nil(t, realization)

	_, statErr := os.Stat(filepath.Join(hostBase, "shared-dirs"))
	assert.True(t, os.IsNotExist(statErr), "nothing should be created at the export root")
}

// TestResolveSharedDirs_NFS_InBaseSymlinkedLeaf_ToVictim_FailsClosed is
// round 4 review finding C1/T1/S-M1: a shared dir's leaf that is a RELATIVE
// symlink to another project's shared dir — staying entirely INSIDE the
// host base — must be refused before any mkdir or chmod runs. The
// component walk's O_NOFOLLOW open on the leaf itself catches this.
func TestResolveSharedDirs_NFS_InBaseSymlinkedLeaf_ToVictim_FailsClosed(t *testing.T) {
	mountRoot := newResolvedTempDir(t)
	shareID := "scion-shared"
	hostBase := filepath.Join(mountRoot, shareID)

	victimDir := filepath.Join(hostBase, "projects", "victim", "shared-dirs", "scratchpad")
	require.NoError(t, os.MkdirAll(victimDir, 0o775))
	require.NoError(t, os.WriteFile(filepath.Join(victimDir, "secret.txt"), []byte("x"), 0o644))
	victimBefore, statErr := os.Stat(victimDir)
	require.NoError(t, statErr)

	attackerSharedDirs := filepath.Join(hostBase, "projects", "pid-1", "shared-dirs")
	require.NoError(t, os.MkdirAll(attackerSharedDirs, 0o775))
	require.NoError(t, os.Symlink(
		filepath.Join("..", "..", "victim", "shared-dirs", "scratchpad"),
		filepath.Join(attackerSharedDirs, "scratchpad")))

	sdCfg := nfsSharedDirStorageCfg(mountRoot)
	sdCfg.NFS.Shares[0].ID = shareID
	dirs := []api.SharedDir{{Name: "scratchpad"}}

	volumes, realization, err := resolveSharedDirs(sdCfg, "/unused", "pid-1", "docker", dirs, "/workspace")
	require.Error(t, err, "a relative in-export symlink to another project's shared dir must be refused")
	assert.Contains(t, err.Error(), "is a symlink")
	assert.Nil(t, volumes)
	assert.Nil(t, realization)

	victimAfter, statErr := os.Stat(victimDir)
	require.NoError(t, statErr)
	assert.Equal(t, victimBefore.Mode(), victimAfter.Mode(), "victim's directory must not be chmod'd")
	entries, err := os.ReadDir(victimDir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "secret.txt", entries[0].Name(), "nothing new should be created in the victim's directory")
}

// TestResolveSharedDirs_NFS_InBaseSymlinkedIntermediate_ToVictim_FailsClosed
// is the intermediate-component half of the same finding: the project's own
// directory (projects/<pid>) is a relative symlink to another project,
// staying inside the host base, and the victim's leaf ALREADY EXISTS. The
// component walk's O_NOFOLLOW open on "pid-1" itself catches this — the
// walk never even reaches the leaf.
func TestResolveSharedDirs_NFS_InBaseSymlinkedIntermediate_ToVictim_FailsClosed(t *testing.T) {
	mountRoot := newResolvedTempDir(t)
	shareID := "scion-shared"
	hostBase := filepath.Join(mountRoot, shareID)

	victimDir := filepath.Join(hostBase, "projects", "victim", "shared-dirs", "scratchpad")
	require.NoError(t, os.MkdirAll(victimDir, 0o775))
	require.NoError(t, os.WriteFile(filepath.Join(victimDir, "secret.txt"), []byte("x"), 0o644))
	victimBefore, statErr := os.Stat(victimDir)
	require.NoError(t, statErr)

	projectsDir := filepath.Join(hostBase, "projects")
	require.NoError(t, os.MkdirAll(projectsDir, 0o775))
	require.NoError(t, os.Symlink("victim", filepath.Join(projectsDir, "pid-1")))

	sdCfg := nfsSharedDirStorageCfg(mountRoot)
	sdCfg.NFS.Shares[0].ID = shareID
	dirs := []api.SharedDir{{Name: "scratchpad"}}

	volumes, realization, err := resolveSharedDirs(sdCfg, "/unused", "pid-1", "docker", dirs, "/workspace")
	require.Error(t, err, "a relative in-export symlinked intermediate component must be refused")
	assert.Contains(t, err.Error(), "is a symlink")
	assert.Nil(t, volumes)
	assert.Nil(t, realization)

	victimAfter, statErr := os.Stat(victimDir)
	require.NoError(t, statErr)
	assert.Equal(t, victimBefore.Mode(), victimAfter.Mode(), "victim's directory must not be chmod'd")
	entries, err := os.ReadDir(victimDir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "secret.txt", entries[0].Name(), "nothing new should be created in the victim's directory")
}

// TestResolveSharedDirs_NFS_InBaseSymlinkedIntermediate_ToVictim_NoLeaf_FailsClosed
// is round 5 review finding C2/T2/S-L1's core requirement: the SAME
// intermediate-symlink shape as above, but the victim's leaf does NOT
// already exist. At round 4 (os.Root), this created a new setgid directory
// inside the victim's tree before the post-hoc equality check refused the
// call ("nothing created in the victim" was not met). The component walk
// closes this: openat(O_NOFOLLOW) on "pid-1" fails before the walk ever
// reaches "shared-dirs" or the leaf, so nothing is created anywhere past
// the symlinked component, regardless of what does or doesn't already
// exist beyond it.
func TestResolveSharedDirs_NFS_InBaseSymlinkedIntermediate_ToVictim_NoLeaf_FailsClosed(t *testing.T) {
	mountRoot := newResolvedTempDir(t)
	shareID := "scion-shared"
	hostBase := filepath.Join(mountRoot, shareID)

	// The victim project exists, and even has a shared-dirs directory, but
	// NOT the specific "scratchpad" leaf the attacker is requesting.
	victimSharedDirs := filepath.Join(hostBase, "projects", "victim", "shared-dirs")
	require.NoError(t, os.MkdirAll(victimSharedDirs, 0o775))

	projectsDir := filepath.Join(hostBase, "projects")
	require.NoError(t, os.Symlink("victim", filepath.Join(projectsDir, "pid-1")))

	sdCfg := nfsSharedDirStorageCfg(mountRoot)
	sdCfg.NFS.Shares[0].ID = shareID
	dirs := []api.SharedDir{{Name: "scratchpad"}}

	volumes, realization, err := resolveSharedDirs(sdCfg, "/unused", "pid-1", "docker", dirs, "/workspace")
	require.Error(t, err, "a relative in-export symlinked intermediate component must be refused even with no pre-existing victim leaf")
	assert.Contains(t, err.Error(), "is a symlink")
	assert.Nil(t, volumes)
	assert.Nil(t, realization)

	entries, err := os.ReadDir(victimSharedDirs)
	require.NoError(t, err)
	assert.Empty(t, entries, "no new leaf should be created in the victim's shared-dirs")
}

// TestResolveSharedDirs_NFS_InBaseSymlinkedSharedDirsComponent_ToVictim_NoLeaf_FailsClosed
// is the "shared-dirs -> ../B/shared-dirs" shape round 5 disposition item 2
// lists explicitly, with no pre-existing victim leaf.
func TestResolveSharedDirs_NFS_InBaseSymlinkedSharedDirsComponent_ToVictim_NoLeaf_FailsClosed(t *testing.T) {
	mountRoot := newResolvedTempDir(t)
	shareID := "scion-shared"
	hostBase := filepath.Join(mountRoot, shareID)

	victimSharedDirs := filepath.Join(hostBase, "projects", "victim", "shared-dirs")
	require.NoError(t, os.MkdirAll(victimSharedDirs, 0o775))

	attackerProjectDir := filepath.Join(hostBase, "projects", "pid-1")
	require.NoError(t, os.MkdirAll(attackerProjectDir, 0o775))
	require.NoError(t, os.Symlink(
		filepath.Join("..", "victim", "shared-dirs"),
		filepath.Join(attackerProjectDir, "shared-dirs")))

	sdCfg := nfsSharedDirStorageCfg(mountRoot)
	sdCfg.NFS.Shares[0].ID = shareID
	dirs := []api.SharedDir{{Name: "scratchpad"}}

	volumes, realization, err := resolveSharedDirs(sdCfg, "/unused", "pid-1", "docker", dirs, "/workspace")
	require.Error(t, err, "a symlinked shared-dirs component pointing at a real victim project must be refused")
	assert.Contains(t, err.Error(), "is a symlink")
	assert.Nil(t, volumes)
	assert.Nil(t, realization)

	entries, err := os.ReadDir(victimSharedDirs)
	require.NoError(t, err)
	assert.Empty(t, entries, "no new leaf should be created in the victim's shared-dirs")
}

// TestResolveSharedDirs_NFS_InBaseSymlinkedLeaf_ExistingVictimLeaf_ModeUnchanged
// is the deterministic counterpart to the r5-security-probe's
// TestProbeR5_RaceChmodExistingVictim (which found 7,032/20,000 chmods of
// an existing victim leaf under the round-4 os.Root implementation): here
// the victim's leaf ALREADY EXISTS at mode 0700 before the attacker's
// symlinked leaf is resolved. Because the leaf component itself
// ("scratchpad" under the attacker's shared-dirs) is a symlink,
// openat(O_NOFOLLOW) refuses it outright — the victim's fd is never
// opened, so fchmod is never called on it, and its mode must remain
// exactly 0700 (not 0775, not 02775).
func TestResolveSharedDirs_NFS_InBaseSymlinkedLeaf_ExistingVictimLeaf_ModeUnchanged(t *testing.T) {
	mountRoot := newResolvedTempDir(t)
	shareID := "scion-shared"
	hostBase := filepath.Join(mountRoot, shareID)

	victimDir := filepath.Join(hostBase, "projects", "victim", "shared-dirs", "scratchpad")
	require.NoError(t, os.MkdirAll(victimDir, 0o700))

	attackerSharedDirs := filepath.Join(hostBase, "projects", "pid-1", "shared-dirs")
	require.NoError(t, os.MkdirAll(attackerSharedDirs, 0o775))
	require.NoError(t, os.Symlink(
		filepath.Join("..", "..", "victim", "shared-dirs", "scratchpad"),
		filepath.Join(attackerSharedDirs, "scratchpad")))

	sdCfg := nfsSharedDirStorageCfg(mountRoot)
	sdCfg.NFS.Shares[0].ID = shareID
	dirs := []api.SharedDir{{Name: "scratchpad"}}

	volumes, realization, err := resolveSharedDirs(sdCfg, "/unused", "pid-1", "docker", dirs, "/workspace")
	require.Error(t, err, "a relative in-export symlink to an existing victim leaf must be refused")
	assert.Contains(t, err.Error(), "is a symlink")
	assert.Nil(t, volumes)
	assert.Nil(t, realization)

	info, statErr := os.Stat(victimDir)
	require.NoError(t, statErr)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm(), "existing victim leaf at 0700 must stay 0700")
}

// TestResolveSharedDirs_NFS_LeafIsRegularFile_FailsClosed is round 5
// disposition item 3 (round 4's untested "exists but is not a directory"
// branch, mutant L2): a regular file blocking the leaf must fail closed
// rather than being handed out as the Docker bind Source.
func TestResolveSharedDirs_NFS_LeafIsRegularFile_FailsClosed(t *testing.T) {
	mountRoot := newResolvedTempDir(t)
	shareID := "scion-shared"
	hostBase := filepath.Join(mountRoot, shareID)
	leafParent := filepath.Join(hostBase, "projects", "pid-1", "shared-dirs")
	require.NoError(t, os.MkdirAll(leafParent, 0o775))
	require.NoError(t, os.WriteFile(filepath.Join(leafParent, "scratchpad"), []byte("not a directory"), 0o644))

	sdCfg := nfsSharedDirStorageCfg(mountRoot)
	sdCfg.NFS.Shares[0].ID = shareID
	dirs := []api.SharedDir{{Name: "scratchpad"}}

	volumes, realization, err := resolveSharedDirs(sdCfg, "/unused", "pid-1", "docker", dirs, "/workspace")
	require.Error(t, err, "a regular file at the leaf must be refused")
	assert.Contains(t, err.Error(), "not a directory")
	assert.Nil(t, volumes)
	assert.Nil(t, realization)
}

// TestResolveSharedDirs_NFS_IntermediateIsRegularFile_FailsClosed is the
// intermediate-component half of disposition item 3: a regular file
// blocking an intermediate component must also fail closed.
func TestResolveSharedDirs_NFS_IntermediateIsRegularFile_FailsClosed(t *testing.T) {
	mountRoot := newResolvedTempDir(t)
	shareID := "scion-shared"
	hostBase := filepath.Join(mountRoot, shareID)
	projectsDir := filepath.Join(hostBase, "projects")
	require.NoError(t, os.MkdirAll(projectsDir, 0o775))
	// "pid-1" is a regular file instead of a directory.
	require.NoError(t, os.WriteFile(filepath.Join(projectsDir, "pid-1"), []byte("not a directory"), 0o644))

	sdCfg := nfsSharedDirStorageCfg(mountRoot)
	sdCfg.NFS.Shares[0].ID = shareID
	dirs := []api.SharedDir{{Name: "scratchpad"}}

	volumes, realization, err := resolveSharedDirs(sdCfg, "/unused", "pid-1", "docker", dirs, "/workspace")
	require.Error(t, err, "a regular file at an intermediate component must be refused")
	assert.Contains(t, err.Error(), "not a directory")
	assert.Nil(t, volumes)
	assert.Nil(t, realization)
}

// TestResolveSharedDirs_NFS_SymlinkedHostBase_StillWorks is round 3 review
// item 4 / disposition 7': a symlinked mount_root (or share directory) is a
// legitimate operator layout (e.g. mount_root pointing at a separately
// mounted disk) and must still work — the confinement check and the
// component walk (round 5) must accept it, not just reject attacker-planted
// symlinks. Also pins that the returned Docker bind source is the resolved
// real path, not the unresolved (symlinked) one.
func TestResolveSharedDirs_NFS_SymlinkedHostBase_StillWorks(t *testing.T) {
	mountRoot := newResolvedTempDir(t)
	shareID := "scion-shared"
	realHostBase := filepath.Join(newResolvedTempDir(t), "real-export")
	require.NoError(t, os.MkdirAll(realHostBase, 0o775))
	symlinkedHostBase := filepath.Join(mountRoot, shareID)
	require.NoError(t, os.Symlink(realHostBase, symlinkedHostBase))

	sdCfg := nfsSharedDirStorageCfg(mountRoot)
	sdCfg.NFS.Shares[0].ID = shareID
	dirs := []api.SharedDir{{Name: "scratchpad"}}

	volumes, realization, err := resolveSharedDirs(sdCfg, "/unused", "pid-1", "docker", dirs, "/workspace")
	require.NoError(t, err)
	require.Len(t, volumes, 1)
	require.NotNil(t, realization)

	wantResolvedSource := filepath.Join(realHostBase, "projects", "pid-1", "shared-dirs", "scratchpad")
	assert.Equal(t, wantResolvedSource, volumes[0].Source,
		"the bind source must be the resolved real path, not the symlinked mount_root")

	info, statErr := os.Stat(wantResolvedSource)
	require.NoError(t, statErr, "the shared dir should exist under the real (resolved) export")
	assert.True(t, info.IsDir())
}

// assertDirEmpty is a test helper asserting dir has no entries — used to
// confirm nothing was created outside the export through a symlink
// (round 3 disposition 7').
func assertDirEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries, "expected %q to remain empty", dir)
}

// TestResolveSharedDirs_NFS_CustomSubPathRoot_StillWorks is round 3 review
// item 4/T2: subpath_root is a documented operator field (§3.2.1) with a
// default of "projects" — the confinement check (which duplicates that
// default) must still accept a non-default value rather than false-positive
// reject every agent start for an otherwise valid config.
func TestResolveSharedDirs_NFS_CustomSubPathRoot_StillWorks(t *testing.T) {
	for _, runtimeName := range []string{"docker", "kubernetes"} {
		t.Run(runtimeName, func(t *testing.T) {
			mountRoot := newResolvedTempDir(t)
			shareID := "scion-shared"
			hostBase := filepath.Join(mountRoot, shareID)
			require.NoError(t, os.MkdirAll(hostBase, 0o775))

			sdCfg := nfsSharedDirStorageCfg(mountRoot)
			sdCfg.NFS.Shares[0].ID = shareID
			sdCfg.NFS.SubPathRoot = "shared"
			dirs := []api.SharedDir{{Name: "scratchpad"}}

			volumes, realization, err := resolveSharedDirs(sdCfg, "/unused", "pid-1", runtimeName, dirs, "/workspace")
			require.NoError(t, err)
			require.NotNil(t, realization)
			assert.Equal(t, "shared/pid-1/shared-dirs/scratchpad", realization.SubPaths["scratchpad"])

			if runtimeName == "docker" {
				require.Len(t, volumes, 1)
				wantSource := filepath.Join(hostBase, "shared", "pid-1", "shared-dirs", "scratchpad")
				assert.Equal(t, wantSource, volumes[0].Source)
				info, statErr := os.Stat(wantSource)
				require.NoError(t, statErr)
				assert.True(t, info.IsDir())
			}
		})
	}
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
