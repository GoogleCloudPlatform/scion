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

//go:build unix

package projectfs

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testOtherPID = "660e8400-e29b-41d4-a716-446655440000"
	testAgentID  = "770e8400-e29b-41d4-a716-446655440000"
	testAgentID2 = "880e8400-e29b-41d4-a716-446655440000"
)

// seedProjectTree creates a full project tree under <hostBase>/projects/<pid>
// and returns its root.
func seedProjectTree(t *testing.T, hostBase, pid string) string {
	t.Helper()
	root := filepath.Join(hostBase, "projects", pid)
	for _, d := range []string{
		"workspace/.git", "workspace/worktrees/dev", ".scion", "provision",
		"shared-dirs/scratchpad", "agents/dev/workspace", "agents/dev/home-" + testAgentID,
	} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, d), 0o755))
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "workspace", "README.md"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".scion", "project-id"), []byte(pid), 0o644))
	return root
}

func delNFSLayout(hostBase string) Layout {
	return Layout{Backend: BackendNFS, HostBase: hostBase, SubPathRoot: "projects"}
}

// The project root goes with everything in it; a sibling project and the
// subpath root stay; a symlink inside the tree is unlinked, its target
// untouched; a second call finds nothing (idempotent).
func TestDeleteProject_RemovesProjectRootOnly(t *testing.T) {
	hostBase := t.TempDir()
	root := seedProjectTree(t, hostBase, testPID)
	sibling := seedProjectTree(t, hostBase, testOtherPID)
	outside := outsideDir(t)
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "workspace", "escape")))
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "shared-dirs", "scratchpad", "escape")))

	ds, err := delNFSLayout(hostBase).DeleteProject(testPID)
	require.NoError(t, err)
	require.Len(t, ds, 1)
	assert.Equal(t, TreeProjectRoot, ds[0].Tree)
	assert.Equal(t, OutcomeDeleted, ds[0].Outcome)
	assert.Equal(t, root, ds[0].Path)
	assert.NoDirExists(t, root)
	assert.DirExists(t, filepath.Join(sibling, "workspace", ".git"))
	assert.DirExists(t, filepath.Join(hostBase, "projects"))
	assert.FileExists(t, filepath.Join(outside, "secret.txt"))

	ds, err = delNFSLayout(hostBase).DeleteProject(testPID)
	require.NoError(t, err)
	require.Len(t, ds, 1)
	assert.Equal(t, OutcomeAbsent, ds[0].Outcome, "a repeated delete finds nothing")
}

// A symlink at the subpath root or at the project dir is refused at that
// component; nothing outside the project root is removed.
func TestDeleteProject_SymlinkedComponentsRefused(t *testing.T) {
	for _, linkAt := range []string{"projects", filepath.Join("projects", testPID)} {
		t.Run(linkAt, func(t *testing.T) {
			hostBase := t.TempDir()
			outside := outsideDir(t)
			require.NoError(t, os.MkdirAll(filepath.Join(outside, testPID, "workspace"), 0o755))
			require.NoError(t, os.MkdirAll(filepath.Join(outside, "workspace"), 0o755))
			link := filepath.Join(hostBase, linkAt)
			require.NoError(t, os.MkdirAll(filepath.Dir(link), 0o755))
			require.NoError(t, os.Symlink(outside, link))

			ds, err := delNFSLayout(hostBase).DeleteProject(testPID)
			require.NoError(t, err)
			require.Len(t, ds, 1)
			assert.Equal(t, OutcomeFailed, ds[0].Outcome)
			assert.Error(t, ds[0].Err)
			assert.True(t, Failed(ds))
			assert.ErrorIs(t, ds[0].Err, ErrRefused)
			assert.False(t, Retryable(ds[0]), "a refusal is not retried")
			assert.FileExists(t, filepath.Join(outside, "secret.txt"))
			assert.DirExists(t, filepath.Join(outside, testPID, "workspace"))
			assert.DirExists(t, filepath.Join(outside, "workspace"))
		})
	}
}

// The share's mount point itself may be behind a symlink (an operator's
// mount); the components below it are still walked without following.
func TestDeleteProject_HostBaseBehindSymlink(t *testing.T) {
	real := t.TempDir()
	root := seedProjectTree(t, real, testPID)
	link := filepath.Join(t.TempDir(), "mnt")
	require.NoError(t, os.Symlink(real, link))

	ds, err := delNFSLayout(link).DeleteProject(testPID)
	require.NoError(t, err)
	require.Len(t, ds, 1)
	assert.Equal(t, OutcomeDeleted, ds[0].Outcome)
	assert.NoDirExists(t, root)
}

// An invalid project ID is refused before the filesystem is touched.
func TestDeleteProject_InvalidIDRefused(t *testing.T) {
	hostBase := t.TempDir()
	sibling := seedProjectTree(t, hostBase, testOtherPID)
	for _, id := range []string{"", ".", "..", "../" + testOtherPID, "a/b", "/abs"} {
		ds, err := delNFSLayout(hostBase).DeleteProject(id)
		assert.Error(t, err, "id %q", id)
		assert.Empty(t, ds)
	}
	assert.DirExists(t, filepath.Join(sibling, "workspace"))
}

// A share that is not mounted is a failure, never a successful delete, and
// the mount path is not created.
func TestDeleteProject_MissingHostBaseFails(t *testing.T) {
	hostBase := filepath.Join(t.TempDir(), "not-mounted")
	ds, err := delNFSLayout(hostBase).DeleteProject(testPID)
	require.NoError(t, err)
	require.Len(t, ds, 1)
	assert.Equal(t, OutcomeFailed, ds[0].Outcome)
	assert.False(t, Retryable(ds[0]))
	assert.NoDirExists(t, hostBase)
}

// The transitional shared-dir split: the project root is removed from both
// shares; the same share is removed once.
func TestDeleteProject_SharedDirSplitRemovesBothRoots(t *testing.T) {
	ws, sd := t.TempDir(), t.TempDir()
	wsRoot := seedProjectTree(t, ws, testPID)
	sdRoot := seedProjectTree(t, sd, testPID)
	sdl := delNFSLayout(sd)
	l := delNFSLayout(ws)
	l.SharedDirs = &sdl

	ds, err := l.DeleteProject(testPID)
	require.NoError(t, err)
	require.Len(t, ds, 2)
	assert.NoDirExists(t, wsRoot)
	assert.NoDirExists(t, sdRoot)

	same := delNFSLayout(ws)
	same.SharedDirs = &Layout{Backend: BackendNFS, HostBase: ws, SubPathRoot: "projects"}
	ds, err = same.DeleteProject(testPID)
	require.NoError(t, err)
	assert.Len(t, ds, 1, "one share, one deletion")
}

// The local backend: nothing is removed by the authority (the slug-keyed
// trees keep their existing deleters), but a bad ID is still refused.
func TestDeleteProject_LocalBackendRemovesNothing(t *testing.T) {
	globalDir := t.TempDir()
	keep := filepath.Join(globalDir, "projects", "demo")
	require.NoError(t, os.MkdirAll(keep, 0o755))
	l := Layout{Backend: BackendLocal, HostBase: globalDir}

	ds, err := l.DeleteProject(testPID)
	require.NoError(t, err)
	assert.Empty(t, ds)
	assert.DirExists(t, keep)
	_, err = l.DeleteProject("../x")
	assert.Error(t, err)

	ds, err = l.DeleteAgent(testPID, "dev")
	require.NoError(t, err)
	assert.Empty(t, ds)
	ds, err = l.DeleteAgentLeaves(testPID, "dev", testAgentID)
	require.NoError(t, err)
	assert.Empty(t, ds)
}

// An agent delete removes agents/<slug> and nothing else of the project.
func TestDeleteAgent_RemovesAgentRootOnly(t *testing.T) {
	hostBase := t.TempDir()
	root := seedProjectTree(t, hostBase, testPID)
	require.NoError(t, os.MkdirAll(filepath.Join(root, "agents", "other", "workspace"), 0o755))
	outside := outsideDir(t)
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "agents", "dev", "workspace", "escape")))

	ds, err := delNFSLayout(hostBase).DeleteAgent(testPID, "dev")
	require.NoError(t, err)
	require.Len(t, ds, 1)
	assert.Equal(t, TreeAgentRoot, ds[0].Tree)
	assert.Equal(t, OutcomeDeleted, ds[0].Outcome)
	assert.NoDirExists(t, filepath.Join(root, "agents", "dev"))
	assert.DirExists(t, filepath.Join(root, "agents", "other", "workspace"))
	assert.DirExists(t, filepath.Join(root, "workspace", ".git"))
	assert.DirExists(t, filepath.Join(root, "shared-dirs", "scratchpad"))
	assert.FileExists(t, filepath.Join(outside, "secret.txt"))

	ds, err = delNFSLayout(hostBase).DeleteAgent(testPID, "dev")
	require.NoError(t, err)
	assert.Equal(t, OutcomeAbsent, ds[0].Outcome)
}

// Symlinks at agents/ or at the agent dir are refused; bad slugs and IDs
// are refused before anything is touched.
func TestDeleteAgent_ConfinementAndValidation(t *testing.T) {
	for _, linkAt := range []string{"agents", filepath.Join("agents", "dev")} {
		t.Run(linkAt, func(t *testing.T) {
			hostBase := t.TempDir()
			root := filepath.Join(hostBase, "projects", testPID)
			outside := outsideDir(t)
			require.NoError(t, os.MkdirAll(filepath.Join(outside, "dev"), 0o755))
			link := filepath.Join(root, linkAt)
			require.NoError(t, os.MkdirAll(filepath.Dir(link), 0o755))
			require.NoError(t, os.Symlink(outside, link))

			ds, err := delNFSLayout(hostBase).DeleteAgent(testPID, "dev")
			require.NoError(t, err)
			require.Len(t, ds, 1)
			assert.Equal(t, OutcomeFailed, ds[0].Outcome)
			assert.FileExists(t, filepath.Join(outside, "secret.txt"))
			assert.DirExists(t, filepath.Join(outside, "dev"))
		})
	}
	hostBase := t.TempDir()
	root := seedProjectTree(t, hostBase, testPID)
	for _, agent := range []string{"", "..", "../dev", "Dev", "a/b"} {
		_, err := delNFSLayout(hostBase).DeleteAgent(testPID, agent)
		assert.Error(t, err, "agent %q", agent)
	}
	_, err := delNFSLayout(hostBase).DeleteAgent("../"+testPID, "dev")
	assert.Error(t, err)
	assert.DirExists(t, filepath.Join(root, "agents", "dev"))
}

// The narrow agent delete removes only the agent's own ID-keyed leaves:
// another incarnation of the slug keeps its home and the shared workspace.
func TestDeleteAgentLeaves_OnlyOwnLeaves(t *testing.T) {
	hostBase := t.TempDir()
	root := seedProjectTree(t, hostBase, testPID)
	agentDir := filepath.Join(root, "agents", "dev")
	require.NoError(t, os.MkdirAll(filepath.Join(agentDir, "secrets-"+testAgentID), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(agentDir, "home-"+testAgentID2), 0o755))

	ds, err := delNFSLayout(hostBase).DeleteAgentLeaves(testPID, "dev", testAgentID)
	require.NoError(t, err)
	require.Len(t, ds, 2)
	assert.Equal(t, TreeAgentHome, ds[0].Tree)
	assert.Equal(t, TreeAgentSecrets, ds[1].Tree)
	assert.NoDirExists(t, filepath.Join(agentDir, "home-"+testAgentID))
	assert.NoDirExists(t, filepath.Join(agentDir, "secrets-"+testAgentID))
	assert.DirExists(t, filepath.Join(agentDir, "home-"+testAgentID2))
	assert.DirExists(t, filepath.Join(agentDir, "workspace"))

	_, err = delNFSLayout(hostBase).DeleteAgentLeaves(testPID, "dev", "not-a-uuid")
	assert.Error(t, err)
}

// An nfs share ID that is not one plain path element (for example "..")
// would put the host base at or above the mount root: the deletion is
// refused, and a tree beside the mount root is untouched.
func TestDeleteProject_ShareIDOutsideMountRootRefused(t *testing.T) {
	parent := t.TempDir()
	mountRoot := filepath.Join(parent, "mnt")
	require.NoError(t, os.MkdirAll(mountRoot, 0o755))
	victim := seedProjectTree(t, parent, testPID)
	l := Layout{Backend: BackendNFS, HostBase: filepath.Join(mountRoot, ".."), SubPathRoot: "projects", Share: Share{ID: ".."}}

	ds, err := l.DeleteProject(testPID)
	require.NoError(t, err)
	require.Len(t, ds, 1)
	assert.Equal(t, OutcomeFailed, ds[0].Outcome)
	ds, err = l.DeleteAgent(testPID, "dev")
	require.NoError(t, err)
	assert.Equal(t, OutcomeFailed, ds[0].Outcome)
	ds, err = l.DeleteAgentLeaves(testPID, "dev", testAgentID)
	require.NoError(t, err)
	assert.Equal(t, OutcomeFailed, ds[0].Outcome)
	assert.DirExists(t, filepath.Join(victim, "agents", "dev", "home-"+testAgentID))

	ok := Layout{Backend: BackendNFS, HostBase: filepath.Join(mountRoot, "share1"), SubPathRoot: "projects", Share: Share{ID: "share1"}}
	assert.NoError(t, ok.checkShareHostBase())
}

// A removal that fails part-way (here a directory that cannot be emptied)
// is retryable, unlike a refusal.
func TestDeleteProject_PartialFailureIsRetryable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	hostBase := t.TempDir()
	root := seedProjectTree(t, hostBase, testPID)
	ws := filepath.Join(root, "workspace")
	require.NoError(t, os.Chmod(ws, 0o555))
	t.Cleanup(func() { _ = os.Chmod(ws, 0o755) })

	ds, err := delNFSLayout(hostBase).DeleteProject(testPID)
	require.NoError(t, err)
	require.Len(t, ds, 1)
	assert.Equal(t, OutcomeFailed, ds[0].Outcome)
	assert.True(t, Retryable(ds[0]))

	require.NoError(t, os.Chmod(ws, 0o755))
	ds, err = delNFSLayout(hostBase).DeleteProject(testPID)
	require.NoError(t, err)
	assert.Equal(t, OutcomeDeleted, ds[0].Outcome)
	assert.NoDirExists(t, root)
}
