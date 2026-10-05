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
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/shareddirs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

const testNFSWorkspaceProjectID = "proj-2530"

// resolveTestNFSWorkspace resolves the NFS workspace for testNFSWorkspaceProjectID
// with the real nfsBackend, so the tests use the same paths Start does.
func resolveTestNFSWorkspace(t *testing.T, mountRoot string, sharedDirNames ...string) runtime.ResolvedWorkspace {
	t.Helper()
	backend := runtime.NewNFSBackend(&config.V1NFSConfig{
		MountRoot: mountRoot,
		Shares:    []config.V1NFSShare{{ID: "share-1", PVName: "ws-pv"}},
	})
	res, err := backend.Resolve(runtime.ResolveInput{ProjectID: testNFSWorkspaceProjectID, SharedDirNames: sharedDirNames})
	require.NoError(t, err)
	return res
}

// statMode returns the full mode_t (including setgid) plus owner of path.
func statMode(t *testing.T, path string) unix.Stat_t {
	t.Helper()
	var st unix.Stat_t
	require.NoError(t, unix.Lstat(path, &st))
	return st
}

// defaultACL returns the directory's default ACL xattr, or nil when the
// filesystem does not support or has no default ACL.
func defaultACL(path string) []byte {
	buf := make([]byte, 256)
	n, err := unix.Getxattr(path, "system.posix_acl_default", buf)
	if err != nil {
		return nil
	}
	return buf[:n]
}

func TestEnsureNFSWorkspaceLeaf_CreatesMissingDirectory(t *testing.T) {
	mountRoot := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, "share-1"), 0o755))
	res := resolveTestNFSWorkspace(t, mountRoot)

	preCreated, err := ensureNFSWorkspaceLeaf("kubernetes", testNFSWorkspaceProjectID, res, "ws-pv", nil)
	require.NoError(t, err)
	assert.True(t, preCreated)

	info, err := os.Stat(res.HostPath)
	require.NoError(t, err, "workspace directory must exist before the pod is created")
	assert.True(t, info.IsDir())
	assert.Equal(t, filepath.Join(mountRoot, "share-1", "projects", testNFSWorkspaceProjectID, "workspace"), res.HostPath)
}

// The workspace leaf must get exactly the treatment a shared-dir leaf on the
// same export gets: same mode bits (setgid 2775 leaf, 2755 intermediates),
// same owner and group, and the same default ACL.
func TestEnsureNFSWorkspaceLeaf_MatchesSharedDirLeafModeAndOwnership(t *testing.T) {
	mountRoot := t.TempDir()
	hostBase := filepath.Join(mountRoot, "share-1")
	require.NoError(t, os.MkdirAll(hostBase, 0o755))
	res := resolveTestNFSWorkspace(t, mountRoot)

	_, err := ensureNFSWorkspaceLeaf("kubernetes", testNFSWorkspaceProjectID, res, "ws-pv", nil)
	require.NoError(t, err)

	// Shared-dir leaf in a second, independent tree so neither creation
	// influences the other's intermediates.
	otherBase := filepath.Join(t.TempDir(), "share-1")
	require.NoError(t, os.MkdirAll(otherBase, 0o755))
	sdRel := filepath.Join("projects", testNFSWorkspaceProjectID, "shared-dirs", "scratch")
	fd, existed, err := shareddirs.EnsureLeaf(otherBase, sdRel)
	require.NoError(t, err)
	require.False(t, existed)
	_ = shareddirs.CloseFd(fd)

	wsLeaf := statMode(t, res.HostPath)
	sdLeaf := statMode(t, filepath.Join(otherBase, sdRel))
	assert.Equal(t, uint32(0o2775), wsLeaf.Mode&0o7777, "workspace leaf mode")
	assert.Equal(t, sdLeaf.Mode&0o7777, wsLeaf.Mode&0o7777, "workspace leaf mode must match a shared-dir leaf")
	assert.Equal(t, sdLeaf.Uid, wsLeaf.Uid, "owner must match a shared-dir leaf")
	assert.Equal(t, sdLeaf.Gid, wsLeaf.Gid, "group must match a shared-dir leaf")
	assert.Equal(t, defaultACL(filepath.Join(otherBase, sdRel)), defaultACL(res.HostPath),
		"default ACL must match a shared-dir leaf")

	wsParent := statMode(t, filepath.Dir(res.HostPath))
	sdProject := statMode(t, filepath.Join(otherBase, "projects", testNFSWorkspaceProjectID))
	assert.Equal(t, uint32(0o2755), wsParent.Mode&0o7777, "project directory mode")
	assert.Equal(t, sdProject.Mode&0o7777, wsParent.Mode&0o7777, "project directory mode must match the shared-dir chain")
}

// An existing directory is never modified. It only counts as prepared (and
// so relaxes the provisioning chown) when it already has setgid and group
// write; a directory made by hand or by the kubelet does not.
func TestEnsureNFSWorkspaceLeaf_LeavesExistingDirectoryAlone(t *testing.T) {
	cases := []struct {
		name         string
		mode         os.FileMode
		wantPrepared bool
	}{
		{name: "0750 not prepared", mode: 0o750, wantPrepared: false},
		{name: "0755 not prepared", mode: 0o755, wantPrepared: false},
		{name: "0770 group write without setgid", mode: 0o770, wantPrepared: false},
		{name: "2755 setgid without group write", mode: 0o2755, wantPrepared: false},
		{name: "2775 prepared", mode: 0o2775, wantPrepared: true},
		{name: "2770 prepared", mode: 0o2770, wantPrepared: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mountRoot := t.TempDir()
			res := resolveTestNFSWorkspace(t, mountRoot)
			require.NoError(t, os.MkdirAll(res.HostPath, 0o755))
			require.NoError(t, unix.Chmod(res.HostPath, uint32(tc.mode.Perm())|uint32(setgidBit(tc.mode))))
			marker := filepath.Join(res.HostPath, "README.md")
			require.NoError(t, os.WriteFile(marker, []byte("keep me"), 0o640))
			before := statMode(t, res.HostPath)
			require.Equal(t, uint32(tc.mode.Perm())|uint32(setgidBit(tc.mode)), before.Mode&0o7777, "fixture mode")

			for i := 0; i < 2; i++ { // idempotent: a second call is also a no-op
				prepared, err := ensureNFSWorkspaceLeaf("kubernetes", testNFSWorkspaceProjectID, res, "ws-pv", nil)
				require.NoError(t, err)
				assert.Equal(t, tc.wantPrepared, prepared)
			}

			after := statMode(t, res.HostPath)
			assert.Equal(t, before.Mode, after.Mode, "existing directory mode must not change")
			assert.Equal(t, before.Uid, after.Uid)
			assert.Equal(t, before.Gid, after.Gid)
			assert.Nil(t, defaultACL(res.HostPath), "no ACL may be added to an existing directory")
			got, err := os.ReadFile(marker)
			require.NoError(t, err)
			assert.Equal(t, "keep me", string(got), "existing contents must be kept")
		})
	}
}

// setgidBit returns unix.S_ISGID when the test case's mode literal carries
// the setgid bit (0o2000), which os.FileMode does not keep in Perm().
func setgidBit(mode os.FileMode) uint32 {
	if uint32(mode)&0o2000 != 0 {
		return unix.S_ISGID
	}
	return 0
}

// A path component of the export mount that is a regular file is reported
// as an export problem, and nothing is created.
func TestEnsureNFSWorkspaceLeaf_ExportMountNotADirectory(t *testing.T) {
	t.Run("mount root is a file", func(t *testing.T) {
		mountRoot := filepath.Join(t.TempDir(), "nfs")
		require.NoError(t, os.WriteFile(mountRoot, nil, 0o644))
		res := resolveTestNFSWorkspace(t, mountRoot)

		prepared, err := ensureNFSWorkspaceLeaf("kubernetes", testNFSWorkspaceProjectID, res, "ws-pv", nil)
		require.Error(t, err)
		assert.False(t, prepared)
		assert.True(t, errors.Is(err, unix.ENOTDIR), "want not a directory, got %v", err)
		assert.Contains(t, err.Error(), "check export mount")
		assert.Contains(t, err.Error(), nfsWorkspaceExportHint)
		info, statErr := os.Stat(mountRoot)
		require.NoError(t, statErr)
		assert.False(t, info.IsDir(), "the file must be left as it is")
	})

	t.Run("share directory is a file", func(t *testing.T) {
		mountRoot := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(mountRoot, "share-1"), nil, 0o644))
		res := resolveTestNFSWorkspace(t, mountRoot)

		prepared, err := ensureNFSWorkspaceLeaf("kubernetes", testNFSWorkspaceProjectID, res, "ws-pv", nil)
		require.Error(t, err)
		assert.False(t, prepared)
		assert.Contains(t, err.Error(), "check export mount")
		assert.Contains(t, err.Error(), "not a directory")
		assert.Contains(t, err.Error(), nfsWorkspaceExportHint)
		entries, readErr := os.ReadDir(mountRoot)
		require.NoError(t, readErr)
		assert.Len(t, entries, 1, "nothing may be created next to the file")
	})
}

// A resolved workspace whose relative path is not local to the host base, or
// whose host path does not match it, is rejected before anything is created.
func TestEnsureNFSWorkspaceLeaf_RejectsUnexpectedWorkspacePath(t *testing.T) {
	mountRoot := t.TempDir()
	hostBase := filepath.Join(mountRoot, "share-1")
	require.NoError(t, os.MkdirAll(hostBase, 0o755))
	cases := []struct {
		name     string
		rel      string
		hostPath string
	}{
		{name: "parent directory", rel: "../x", hostPath: filepath.Join(hostBase, "../x")},
		{name: "absolute path", rel: "/x/workspace", hostPath: filepath.Join(hostBase, "/x/workspace")},
		{name: "empty path", rel: "", hostPath: hostBase},
		{name: "host path mismatch", rel: "projects/" + testNFSWorkspaceProjectID + "/workspace",
			hostPath: filepath.Join(mountRoot, "elsewhere", "workspace")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := runtime.ResolvedWorkspace{
				Backend:            "nfs",
				HostBase:           hostBase,
				ServerRelativePath: tc.rel,
				HostPath:           tc.hostPath,
			}
			prepared, err := ensureNFSWorkspaceLeaf("kubernetes", testNFSWorkspaceProjectID, res, "ws-pv", nil)
			require.Error(t, err)
			assert.False(t, prepared)
			assert.Contains(t, err.Error(), "unexpected workspace path")
			entries, readErr := os.ReadDir(hostBase)
			require.NoError(t, readErr)
			assert.Empty(t, entries, "nothing may be created")
			_, statErr := os.Stat(filepath.Join(mountRoot, "x"))
			assert.True(t, os.IsNotExist(statErr))
		})
	}
}

// Shared dirs served from the workspace claim get their directories created
// in the same step, with the shared-dir leaf treatment.
func TestEnsureNFSWorkspaceLeaf_CreatesClaimSharedDirs(t *testing.T) {
	mountRoot := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, "share-1"), 0o755))
	res := resolveTestNFSWorkspace(t, mountRoot, "scratchpad", "cache")

	prepared, err := ensureNFSWorkspaceLeaf("kubernetes", testNFSWorkspaceProjectID, res, "ws-pv", []string{"scratchpad", "cache"})
	require.NoError(t, err)
	assert.True(t, prepared)
	projectDir := filepath.Join(mountRoot, "share-1", "projects", testNFSWorkspaceProjectID)
	for _, name := range []string{"scratchpad", "cache"} {
		st := statMode(t, filepath.Join(projectDir, "shared-dirs", name))
		assert.Equal(t, uint32(0o2775), st.Mode&0o7777, "shared dir %s mode", name)
	}
	assert.Equal(t, uint32(0o2755), statMode(t, filepath.Join(projectDir, "shared-dirs")).Mode&0o7777)
	assert.Equal(t, uint32(0o2775), statMode(t, res.HostPath).Mode&0o7777)
}

// An existing claim shared dir without setgid and group write keeps the
// provisioning chown strict, even though the workspace itself was created.
func TestEnsureNFSWorkspaceLeaf_ExistingUnpreparedSharedDirStaysStrict(t *testing.T) {
	mountRoot := t.TempDir()
	res := resolveTestNFSWorkspace(t, mountRoot, "scratchpad")
	sdPath := filepath.Join(filepath.Dir(res.HostPath), "shared-dirs", "scratchpad")
	require.NoError(t, os.MkdirAll(sdPath, 0o755))

	prepared, err := ensureNFSWorkspaceLeaf("kubernetes", testNFSWorkspaceProjectID, res, "ws-pv", []string{"scratchpad"})
	require.NoError(t, err)
	assert.False(t, prepared)
	assert.Equal(t, uint32(0o2775), statMode(t, res.HostPath).Mode&0o7777, "the workspace is still created")
	assert.Equal(t, uint32(0o755), statMode(t, sdPath).Mode&0o7777, "the existing shared dir is left alone")
}

// An existing workspace directory that is not prepared does not stop the
// missing shared dirs from being created.
func TestEnsureNFSWorkspaceLeaf_UnpreparedWorkspaceStillCreatesSharedDirs(t *testing.T) {
	mountRoot := t.TempDir()
	res := resolveTestNFSWorkspace(t, mountRoot, "scratchpad")
	require.NoError(t, os.MkdirAll(res.HostPath, 0o755))
	require.NoError(t, os.Chmod(res.HostPath, 0o755))

	prepared, err := ensureNFSWorkspaceLeaf("kubernetes", testNFSWorkspaceProjectID, res, "ws-pv", []string{"scratchpad"})
	require.NoError(t, err)
	assert.False(t, prepared)
	assert.Equal(t, uint32(0o755), statMode(t, res.HostPath).Mode&0o7777, "the existing workspace is left alone")
	sdPath := filepath.Join(filepath.Dir(res.HostPath), "shared-dirs", "scratchpad")
	assert.Equal(t, uint32(0o2775), statMode(t, sdPath).Mode&0o7777, "the missing shared dir is created")
}

// The shared-dir directories created here must be the ones the workspace
// backend resolved (and the runtime mounts); a mismatch is an error.
func TestEnsureNFSWorkspaceLeaf_SharedDirMustMatchResolvedPath(t *testing.T) {
	cases := []struct {
		name string
		edit func(res *runtime.ResolvedWorkspace)
	}{
		{name: "not resolved", edit: func(res *runtime.ResolvedWorkspace) { delete(res.SharedDirs, "scratchpad") }},
		{name: "different path", edit: func(res *runtime.ResolvedWorkspace) {
			res.SharedDirs["scratchpad"] = runtime.ResolvedSharedDir{ServerRelativePath: "projects/" + testNFSWorkspaceProjectID + "/other/scratchpad"}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mountRoot := t.TempDir()
			require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, "share-1"), 0o755))
			res := resolveTestNFSWorkspace(t, mountRoot, "scratchpad")
			tc.edit(&res)

			prepared, err := ensureNFSWorkspaceLeaf("kubernetes", testNFSWorkspaceProjectID, res, "ws-pv", []string{"scratchpad"})
			require.Error(t, err)
			assert.False(t, prepared)
			assert.Contains(t, err.Error(), "shared dir \"scratchpad\" resolves to")
			_, statErr := os.Stat(filepath.Join(mountRoot, "share-1", "projects"))
			assert.True(t, os.IsNotExist(statErr), "nothing may be created")
		})
	}
}

// The workspace backend resolves each shared dir to the path the Kubernetes
// runtime mounts (projects/<pid>/shared-dirs/<name>, next to the workspace).
func TestEnsureNFSWorkspaceLeaf_ResolvedSharedDirLayout(t *testing.T) {
	res := resolveTestNFSWorkspace(t, t.TempDir(), "scratchpad")
	assert.Equal(t, "projects/"+testNFSWorkspaceProjectID+"/workspace", res.ServerRelativePath)
	assert.Equal(t, "projects/"+testNFSWorkspaceProjectID+"/shared-dirs/scratchpad", res.SharedDirs["scratchpad"].ServerRelativePath)
}

func TestEnsureNFSWorkspaceLeaf_SharedDirFailures(t *testing.T) {
	t.Run("invalid name", func(t *testing.T) {
		mountRoot := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, "share-1"), 0o755))
		res := resolveTestNFSWorkspace(t, mountRoot)

		prepared, err := ensureNFSWorkspaceLeaf("kubernetes", testNFSWorkspaceProjectID, res, "ws-pv", []string{"../other"})
		require.Error(t, err)
		assert.False(t, prepared)
		assert.Contains(t, err.Error(), "shared_dirs")
		_, statErr := os.Stat(filepath.Join(mountRoot, "share-1", "projects"))
		assert.True(t, os.IsNotExist(statErr), "nothing may be created")
	})

	t.Run("blocked by a file", func(t *testing.T) {
		mountRoot := t.TempDir()
		res := resolveTestNFSWorkspace(t, mountRoot, "scratchpad")
		sdRoot := filepath.Join(filepath.Dir(res.HostPath), "shared-dirs")
		require.NoError(t, os.MkdirAll(sdRoot, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(sdRoot, "scratchpad"), nil, 0o644))

		prepared, err := ensureNFSWorkspaceLeaf("kubernetes", testNFSWorkspaceProjectID, res, "ws-pv", []string{"scratchpad"})
		require.Error(t, err)
		assert.False(t, prepared)
		assert.Contains(t, err.Error(), "shared-dirs/scratchpad")
		assert.Contains(t, err.Error(), nfsWorkspaceExportHint)
	})
}

func TestEnsureNFSWorkspaceLeaf_CreateFailureReturnsExportError(t *testing.T) {
	t.Run("path blocked by a file", func(t *testing.T) {
		mountRoot := t.TempDir()
		res := resolveTestNFSWorkspace(t, mountRoot)
		require.NoError(t, os.MkdirAll(filepath.Dir(res.HostPath), 0o755))
		require.NoError(t, os.WriteFile(res.HostPath, nil, 0o644))

		preCreated, err := ensureNFSWorkspaceLeaf("kubernetes", testNFSWorkspaceProjectID, res, "ws-pv", nil)
		require.Error(t, err)
		assert.False(t, preCreated)
		assert.Contains(t, err.Error(), "not a directory")
		assert.Contains(t, err.Error(), nfsWorkspaceExportHint)
	})

	t.Run("workspace is a symlink", func(t *testing.T) {
		mountRoot := t.TempDir()
		res := resolveTestNFSWorkspace(t, mountRoot)
		require.NoError(t, os.MkdirAll(filepath.Dir(res.HostPath), 0o755))
		target := t.TempDir()
		require.NoError(t, os.Symlink(target, res.HostPath))

		preCreated, err := ensureNFSWorkspaceLeaf("kubernetes", testNFSWorkspaceProjectID, res, "ws-pv", nil)
		require.Error(t, err)
		assert.False(t, preCreated)
		assert.Contains(t, err.Error(), "symlink")
		assert.Contains(t, err.Error(), nfsWorkspaceExportHint)
		entries, readErr := os.ReadDir(target)
		require.NoError(t, readErr)
		assert.Empty(t, entries, "nothing may be created through the symlink")
	})
}

// captureNFSLeafWarnings collects slog records at Warn and above for the
// duration of the test.
func captureNFSLeafWarnings(t *testing.T) func() []string {
	t.Helper()
	var records []slog.Record
	old := slog.Default()
	slog.SetDefault(slog.New(&collectHandler{records: &records}))
	t.Cleanup(func() { slog.SetDefault(old) })
	return func() []string {
		var msgs []string
		for _, r := range records {
			if r.Level >= slog.LevelWarn {
				msgs = append(msgs, r.Message)
			}
		}
		return msgs
	}
}

const nfsLeafLeftToNodeWarning = "the broker could not create the directory, so the node will create it"

// A directory the broker is not allowed to create is left to the node, as
// before: no error, not prepared (so the chown stays strict), and a warning.
func TestEnsureNFSWorkspaceLeaf_PermissionDeniedLeavesDirectoryToNode(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}

	t.Run("workspace", func(t *testing.T) {
		warnings := captureNFSLeafWarnings(t)
		mountRoot := t.TempDir()
		res := resolveTestNFSWorkspace(t, mountRoot)
		projectDir := filepath.Dir(res.HostPath)
		require.NoError(t, os.MkdirAll(projectDir, 0o755))
		require.NoError(t, os.Chmod(projectDir, 0o555))
		t.Cleanup(func() { _ = os.Chmod(projectDir, 0o755) })

		prepared, err := ensureNFSWorkspaceLeaf("kubernetes", testNFSWorkspaceProjectID, res, "ws-pv", nil)
		require.NoError(t, err)
		assert.False(t, prepared)
		_, statErr := os.Stat(res.HostPath)
		assert.True(t, os.IsNotExist(statErr))
		// The workspace and the provisioning state directory are both
		// left to the node.
		require.Len(t, warnings(), 2)
		assert.Contains(t, warnings()[0], nfsLeafLeftToNodeWarning)
		assert.Contains(t, warnings()[1], nfsLeafLeftToNodeWarning)
	})

	t.Run("workspace denied, shared dir still created", func(t *testing.T) {
		warnings := captureNFSLeafWarnings(t)
		mountRoot := t.TempDir()
		res := resolveTestNFSWorkspace(t, mountRoot, "scratchpad")
		projectDir := filepath.Dir(res.HostPath)
		sdRoot := filepath.Join(projectDir, "shared-dirs")
		require.NoError(t, os.MkdirAll(sdRoot, 0o755))
		require.NoError(t, os.Chmod(projectDir, 0o555))
		t.Cleanup(func() { _ = os.Chmod(projectDir, 0o755) })

		prepared, err := ensureNFSWorkspaceLeaf("kubernetes", testNFSWorkspaceProjectID, res, "ws-pv", []string{"scratchpad"})
		require.NoError(t, err)
		assert.False(t, prepared)
		_, statErr := os.Stat(res.HostPath)
		assert.True(t, os.IsNotExist(statErr))
		assert.Equal(t, uint32(0o2775), statMode(t, filepath.Join(sdRoot, "scratchpad")).Mode&0o7777,
			"the shared dir is still created")
		require.Len(t, warnings(), 2, "workspace and provisioning state directory")
	})

	t.Run("shared dir", func(t *testing.T) {
		warnings := captureNFSLeafWarnings(t)
		mountRoot := t.TempDir()
		res := resolveTestNFSWorkspace(t, mountRoot, "scratchpad")
		sdRoot := filepath.Join(filepath.Dir(res.HostPath), "shared-dirs")
		require.NoError(t, os.MkdirAll(sdRoot, 0o755))
		require.NoError(t, os.Chmod(sdRoot, 0o555))
		t.Cleanup(func() { _ = os.Chmod(sdRoot, 0o755) })

		prepared, err := ensureNFSWorkspaceLeaf("kubernetes", testNFSWorkspaceProjectID, res, "ws-pv", []string{"scratchpad"})
		require.NoError(t, err)
		assert.False(t, prepared)
		assert.Equal(t, uint32(0o2775), statMode(t, res.HostPath).Mode&0o7777, "the workspace is still created")
		_, statErr := os.Stat(filepath.Join(sdRoot, "scratchpad"))
		assert.True(t, os.IsNotExist(statErr))
		require.Len(t, warnings(), 1)
		assert.Contains(t, warnings()[0], nfsLeafLeftToNodeWarning)
	})
}

func TestEnsureNFSWorkspaceLeaf_NoOpOutsideKubernetesNFS(t *testing.T) {
	cases := []struct {
		name        string
		runtimeName string
		backend     string
		pvClaim     string
	}{
		{name: "docker runtime", runtimeName: "docker", backend: "nfs", pvClaim: "ws-pv"},
		{name: "local backend", runtimeName: "kubernetes", backend: "local", pvClaim: "ws-pv"},
		{name: "no PV claim", runtimeName: "kubernetes", backend: "nfs", pvClaim: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mountRoot := t.TempDir()
			require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, "share-1"), 0o755))
			res := resolveTestNFSWorkspace(t, mountRoot)
			res.Backend = tc.backend

			preCreated, err := ensureNFSWorkspaceLeaf(tc.runtimeName, testNFSWorkspaceProjectID, res, tc.pvClaim, nil)
			require.NoError(t, err)
			assert.False(t, preCreated)
			_, err = os.Stat(filepath.Join(mountRoot, "share-1", "projects"))
			assert.True(t, os.IsNotExist(err), "nothing may be created on the export")
		})
	}
}

// Without a broker-side mount of the export, the previous behavior is kept:
// no error, nothing created (the node creates the directory at pod start).
func TestEnsureNFSWorkspaceLeaf_ExportNotMountedKeepsPreviousBehavior(t *testing.T) {
	mountRoot := filepath.Join(t.TempDir(), "not-mounted")
	res := resolveTestNFSWorkspace(t, mountRoot)

	preCreated, err := ensureNFSWorkspaceLeaf("kubernetes", testNFSWorkspaceProjectID, res, "ws-pv", nil)
	require.NoError(t, err)
	assert.False(t, preCreated, "nothing was created, so the chown must stay strict")
	_, err = os.Stat(mountRoot)
	assert.True(t, os.IsNotExist(err), "the mount root must never be created")
}

func TestEnsureNFSWorkspaceLeaf_RejectsInvalidProjectID(t *testing.T) {
	mountRoot := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, "share-1"), 0o755))
	res := resolveTestNFSWorkspace(t, mountRoot)

	preCreated, err := ensureNFSWorkspaceLeaf("kubernetes", "../other", res, "ws-pv", nil)
	require.Error(t, err)
	assert.False(t, preCreated)
	assert.Contains(t, err.Error(), "invalid project ID")
}

// nfsWorkspaceStartYAML configures workspace_storage nfs under "server:".
const nfsWorkspaceStartYAML = `  workspace_storage:
    backend: nfs
    nfs:
      mount_root: %s
      shares:
        - id: share-1
          server: 10.0.0.2
          export: /scion-workspaces
          pv_name: ws-pv
`

// startNFSWorkspaceAgent runs Manager.Start against a mock runtime with the
// given name and reports whether the workspace directory existed at the
// moment the runtime was asked to create the pod, plus the RunConfig it got.
func startNFSWorkspaceAgent(t *testing.T, runtimeName, mountRoot string) (existedAtRun bool, ranCount int, cfg runtime.RunConfig, err error) {
	t.Helper()
	return startNFSWorkspaceAgentWith(t, runtimeName, mountRoot, "", nil)
}

// startNFSWorkspaceAgentWith is startNFSWorkspaceAgent with extra "server:"
// settings and shared dirs.
func startNFSWorkspaceAgentWith(t *testing.T, runtimeName, mountRoot, extraServerYAML string, sharedDirs []api.SharedDir) (existedAtRun bool, ranCount int, cfg runtime.RunConfig, err error) {
	t.Helper()
	f := newSharedDirStorageRunFixture(t)
	f.writeGlobalSettings(t, fmt.Sprintf(nfsWorkspaceStartYAML, mountRoot)+extraServerYAML)
	wsPath := filepath.Join(mountRoot, "share-1", "projects", testNFSWorkspaceProjectID, "workspace")

	mockRT := &runtime.MockRuntime{
		NameFunc: func() string { return runtimeName },
		RunFunc: func(ctx context.Context, rc runtime.RunConfig) (string, error) {
			ranCount++
			cfg = rc
			info, statErr := os.Stat(wsPath)
			existedAtRun = statErr == nil && info.IsDir()
			return "mock-id", nil
		},
	}
	_, err = NewManager(mockRT).Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: f.projectScionDir,
		NoAuth:      true,
		Env: map[string]string{
			"SCION_AGENT_ID":   "agent-2530",
			"SCION_PROJECT_ID": testNFSWorkspaceProjectID,
		},
		GitClone:   &api.GitCloneConfig{URL: "https://example.com/repo.git"},
		SharedDirs: sharedDirs,
	})
	return existedAtRun, ranCount, cfg, err
}

// Start must create the workspace directory before the runtime creates the
// pod, and mark it as pre-created so the init container's chown is relaxed.
func TestStartNFSWorkspace_PreCreatesWorkspaceBeforeRun(t *testing.T) {
	mountRoot := filepath.Join(t.TempDir(), "nfs")
	require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, "share-1"), 0o755))

	existedAtRun, ranCount, cfg, err := startNFSWorkspaceAgent(t, "kubernetes", mountRoot)
	require.NoError(t, err)
	require.Equal(t, 1, ranCount)
	assert.True(t, existedAtRun, "workspace directory must exist when the pod is created")
	assert.True(t, cfg.NFSWorkspacePreCreated)
	assert.Equal(t, "projects/"+testNFSWorkspaceProjectID+"/workspace", cfg.NFSSubPath)
}

// Without a broker mount the previous behavior is kept: Start succeeds,
// nothing is created, and the chown stays strict.
func TestStartNFSWorkspace_ExportNotMounted_StaysStrict(t *testing.T) {
	mountRoot := filepath.Join(t.TempDir(), "nfs")

	existedAtRun, ranCount, cfg, err := startNFSWorkspaceAgent(t, "kubernetes", mountRoot)
	require.NoError(t, err)
	require.Equal(t, 1, ranCount)
	assert.False(t, existedAtRun)
	assert.False(t, cfg.NFSWorkspacePreCreated)
}

// Non-Kubernetes runtimes are unaffected: nothing is created for them and
// the marker stays unset.
func TestStartNFSWorkspace_NonKubernetesUnaffected(t *testing.T) {
	mountRoot := filepath.Join(t.TempDir(), "nfs")
	require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, "share-1"), 0o755))

	existedAtRun, ranCount, cfg, err := startNFSWorkspaceAgent(t, "docker", mountRoot)
	require.NoError(t, err)
	require.Equal(t, 1, ranCount)
	assert.False(t, existedAtRun)
	assert.False(t, cfg.NFSWorkspacePreCreated)
}

// A workspace directory that cannot be created fails Start before the
// runtime is ever called.
func TestStartNFSWorkspace_CreateFailureFailsBeforeRun(t *testing.T) {
	mountRoot := filepath.Join(t.TempDir(), "nfs")
	projectDir := filepath.Join(mountRoot, "share-1", "projects", testNFSWorkspaceProjectID)
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "workspace"), nil, 0o644))

	_, ranCount, _, err := startNFSWorkspaceAgent(t, "kubernetes", mountRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), nfsWorkspaceExportHint)
	assert.Equal(t, 0, ranCount, "the pod must not be created")
}

// Shared dirs mounted from the workspace claim (shared_dir_storage unset) are
// created before the pod, next to the workspace.
func TestStartNFSWorkspace_PreCreatesClaimSharedDirs(t *testing.T) {
	mountRoot := filepath.Join(t.TempDir(), "nfs")
	require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, "share-1"), 0o755))

	_, ranCount, cfg, err := startNFSWorkspaceAgentWith(t, "kubernetes", mountRoot, "",
		[]api.SharedDir{{Name: "scratchpad"}})
	require.NoError(t, err)
	require.Equal(t, 1, ranCount)
	assert.True(t, cfg.NFSWorkspacePreCreated)
	sdPath := filepath.Join(mountRoot, "share-1", "projects", testNFSWorkspaceProjectID, "shared-dirs", "scratchpad")
	assert.Equal(t, uint32(0o2775), statMode(t, sdPath).Mode&0o7777)
}

// Shared dirs with their own NFS storage are not on the workspace claim, so
// nothing is created for them under the workspace's project directory.
func TestStartNFSWorkspace_SharedDirStorageNFSNotOnClaim(t *testing.T) {
	mountRoot := filepath.Join(t.TempDir(), "nfs")
	require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, "share-1"), 0o755))
	sdMountRoot := filepath.Join(t.TempDir(), "sd-nfs")
	require.NoError(t, os.MkdirAll(filepath.Join(sdMountRoot, "sd-share"), 0o755))

	_, ranCount, cfg, err := startNFSWorkspaceAgentWith(t, "kubernetes", mountRoot,
		sprintfServerYAML(sharedDirStorageNFSGlobalYAML, sdMountRoot, "sd-share", "sd-pv"),
		[]api.SharedDir{{Name: "scratchpad"}})
	require.NoError(t, err)
	require.Equal(t, 1, ranCount)
	require.NotNil(t, cfg.SharedDirStorage)
	assert.True(t, cfg.NFSWorkspacePreCreated)
	_, statErr := os.Stat(filepath.Join(mountRoot, "share-1", "projects", testNFSWorkspaceProjectID, "shared-dirs"))
	assert.True(t, os.IsNotExist(statErr), "no shared-dirs directory may be created on the workspace claim")
}

// A directory the broker is not allowed to create does not stop Start: the
// pod is still created, the node creates the directory, and the chown stays
// strict.
func TestStartNFSWorkspace_PermissionDeniedLeavesDirectoryToNode(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	mountRoot := filepath.Join(t.TempDir(), "nfs")
	projectDir := filepath.Join(mountRoot, "share-1", "projects", testNFSWorkspaceProjectID)
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	require.NoError(t, os.Chmod(projectDir, 0o555))
	t.Cleanup(func() { _ = os.Chmod(projectDir, 0o755) })

	existedAtRun, ranCount, cfg, err := startNFSWorkspaceAgentWith(t, "kubernetes", mountRoot, "",
		[]api.SharedDir{{Name: "scratchpad"}})
	require.NoError(t, err)
	require.Equal(t, 1, ranCount, "the pod must still be created")
	assert.False(t, existedAtRun)
	assert.False(t, cfg.NFSWorkspacePreCreated)
}

// Start checks the configured workspace_storage.nfs uid/gid before the
// runtime gets them: a broker only warns about invalid settings at
// startup, so an out-of-range id must stop the start here with the full
// key named, before any leaf directory is created on the export, and the
// pod is never created. Valid values, including the default, still start.
func TestStartNFSWorkspace_OwnerIDRange(t *testing.T) {
	tests := []struct {
		name      string
		ownerYAML string
		wantField string
		// wantUID/wantGID are the ids the runtime must get; 0 means
		// unset, which the runtime reads as the default 1000.
		wantUID, wantGID int
	}{
		{name: "unset uses default", ownerYAML: ""},
		{name: "explicit", ownerYAML: "      uid: 2000\n      gid: 3000\n", wantUID: 2000, wantGID: 3000},
		{name: "maximum", ownerYAML: "      uid: 4294967294\n      gid: 4294967294\n", wantUID: 4294967294, wantGID: 4294967294},
		{name: "negative one uid", ownerYAML: "      uid: -1\n", wantField: "server.workspace_storage.nfs.uid"},
		{name: "negative gid", ownerYAML: "      gid: -5\n", wantField: "server.workspace_storage.nfs.gid"},
		{name: "unsigned sentinel uid", ownerYAML: "      uid: 4294967295\n", wantField: "server.workspace_storage.nfs.uid"},
		{name: "unsigned sentinel gid", ownerYAML: "      gid: 4294967295\n", wantField: "server.workspace_storage.nfs.gid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mountRoot := filepath.Join(t.TempDir(), "nfs")
			require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, "share-1"), 0o755))
			projectsDir := filepath.Join(mountRoot, "share-1", "projects")
			f := newSharedDirStorageRunFixture(t)
			f.writeGlobalSettings(t, fmt.Sprintf(nfsWorkspaceStartYAML, mountRoot+"\n"+strings.TrimSuffix(tt.ownerYAML, "\n")))
			ran := false
			var got runtime.RunConfig
			mockRT := &runtime.MockRuntime{
				NameFunc: func() string { return "kubernetes" },
				RunFunc: func(ctx context.Context, rc runtime.RunConfig) (string, error) {
					ran = true
					got = rc
					return "mock-id", nil
				},
			}
			_, err := NewManager(mockRT).Start(context.Background(), api.StartOptions{
				Name:        "test-agent",
				ProjectPath: f.projectScionDir,
				NoAuth:      true,
				Env:         map[string]string{"SCION_PROJECT_ID": testNFSWorkspaceProjectID},
				GitClone:    &api.GitCloneConfig{URL: "https://example.com/repo.git"},
			})
			if tt.wantField != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantField)
				assert.False(t, ran, "the pod must not be created")
				_, statErr := os.Stat(projectsDir)
				assert.True(t, os.IsNotExist(statErr),
					"no leaf directory may be created on the export for an invalid id (stat err: %v)", statErr)
				return
			}
			require.NoError(t, err)
			require.True(t, ran)
			assert.DirExists(t, filepath.Join(projectsDir, testNFSWorkspaceProjectID, "workspace"))
			assert.Equal(t, tt.wantUID, got.NFSUID)
			assert.Equal(t, tt.wantGID, got.NFSGID)
		})
	}
}
