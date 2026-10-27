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
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
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

	ds, err := delNFSLayout(hostBase).RemoveProjectTree(testPID)
	require.NoError(t, err)
	require.Len(t, ds, 1)
	assert.Equal(t, TreeProjectRoot, ds[0].Tree)
	assert.Equal(t, OutcomeDeleted, ds[0].Outcome)
	assert.Equal(t, root, ds[0].Path)
	assert.NoDirExists(t, root)
	assert.DirExists(t, filepath.Join(sibling, "workspace", ".git"))
	assert.DirExists(t, filepath.Join(hostBase, "projects"))
	assert.FileExists(t, filepath.Join(outside, "secret.txt"))

	ds, err = delNFSLayout(hostBase).RemoveProjectTree(testPID)
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

			ds, err := delNFSLayout(hostBase).RemoveProjectTree(testPID)
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

	ds, err := delNFSLayout(link).RemoveProjectTree(testPID)
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
		ds, err := delNFSLayout(hostBase).RemoveProjectTree(id)
		assert.Error(t, err, "id %q", id)
		assert.Empty(t, ds)
	}
	assert.DirExists(t, filepath.Join(sibling, "workspace"))
}

// A share that is not mounted is a failure, never a successful delete, and
// the mount path is not created.
func TestDeleteProject_MissingHostBaseFails(t *testing.T) {
	hostBase := filepath.Join(t.TempDir(), "not-mounted")
	ds, err := delNFSLayout(hostBase).RemoveProjectTree(testPID)
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

	ds, err := l.RemoveProjectTree(testPID)
	require.NoError(t, err)
	require.Len(t, ds, 2)
	assert.NoDirExists(t, wsRoot)
	assert.NoDirExists(t, sdRoot)

	same := delNFSLayout(ws)
	same.SharedDirs = &Layout{Backend: BackendNFS, HostBase: ws, SubPathRoot: "projects"}
	ds, err = same.RemoveProjectTree(testPID)
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

	ds, err := l.RemoveProjectTree(testPID)
	require.NoError(t, err)
	assert.Empty(t, ds)
	assert.DirExists(t, keep)
	_, err = l.RemoveProjectTree("../x")
	assert.Error(t, err)

	ds, err = l.RemoveAgentStorage(testPID, "dev", testAgentID)
	require.NoError(t, err)
	assert.Empty(t, ds)
}

// An agent delete removes the agent's own home and secrets, and then
// agents/<slug> when nothing else is left in it; nothing else of the
// project is touched, and a repeated delete finds nothing.
func TestDeleteAgent_RemovesOwnLeavesAndEmptyAgentRoot(t *testing.T) {
	hostBase := t.TempDir()
	root := seedProjectTree(t, hostBase, testPID)
	agentDir := filepath.Join(root, "agents", "dev")
	require.NoError(t, os.RemoveAll(filepath.Join(agentDir, "workspace"))) // the broker removed it
	require.NoError(t, os.MkdirAll(filepath.Join(agentDir, "secrets-"+testAgentID), 0o700))
	outside := outsideDir(t)
	require.NoError(t, os.Symlink(outside, filepath.Join(agentDir, "home-"+testAgentID, "escape")))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "agents", "other", "workspace"), 0o755))

	ds, err := delNFSLayout(hostBase).RemoveAgentStorage(testPID, "dev", testAgentID)
	require.NoError(t, err)
	require.Len(t, ds, 3)
	for i, tree := range []TreeKind{TreeAgentHome, TreeAgentSecrets, TreeAgentRoot} {
		assert.Equal(t, tree, ds[i].Tree)
		assert.Equal(t, OutcomeDeleted, ds[i].Outcome, string(tree))
	}
	assert.NoDirExists(t, agentDir)
	assert.DirExists(t, filepath.Join(root, "agents", "other", "workspace"))
	assert.DirExists(t, filepath.Join(root, "workspace", ".git"))
	assert.DirExists(t, filepath.Join(root, "shared-dirs", "scratchpad"))
	assert.FileExists(t, filepath.Join(outside, "secret.txt"))

	ds, err = delNFSLayout(hostBase).RemoveAgentStorage(testPID, "dev", testAgentID)
	require.NoError(t, err)
	for _, d := range ds {
		assert.Equal(t, OutcomeAbsent, d.Outcome, string(d.Tree))
	}
}

// The agent's own workspace is never removed: while it (or another agent
// incarnation's home of the same slug) is there, agents/<slug> is kept
// with the pending-broker reason, and only the agent's own leaves go.
func TestDeleteAgent_KeepsWorkspaceAndOtherLeaves(t *testing.T) {
	hostBase := t.TempDir()
	root := seedProjectTree(t, hostBase, testPID)
	agentDir := filepath.Join(root, "agents", "dev")
	require.NoError(t, os.WriteFile(filepath.Join(agentDir, "workspace", "unpushed.txt"), []byte("work"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(agentDir, "home-"+testAgentID2), 0o755))

	ds, err := delNFSLayout(hostBase).RemoveAgentStorage(testPID, "dev", testAgentID)
	require.NoError(t, err)
	require.Len(t, ds, 3)
	assert.Equal(t, OutcomeDeleted, ds[0].Outcome)
	assert.Equal(t, TreeAgentRoot, ds[2].Tree)
	assert.Equal(t, OutcomeKept, ds[2].Outcome)
	assert.Equal(t, AgentWorkspacePendingReason, ds[2].Reason)
	assert.NoDirExists(t, filepath.Join(agentDir, "home-"+testAgentID))
	assert.FileExists(t, filepath.Join(agentDir, "workspace", "unpushed.txt"))
	assert.DirExists(t, filepath.Join(agentDir, "home-"+testAgentID2))
}

// Symlinks at agents/ or at the agent dir are refused; bad slugs and IDs
// are refused before anything is touched.
func TestDeleteAgent_ConfinementAndValidation(t *testing.T) {
	for _, linkAt := range []string{"agents", filepath.Join("agents", "dev")} {
		t.Run(linkAt, func(t *testing.T) {
			hostBase := t.TempDir()
			root := filepath.Join(hostBase, "projects", testPID)
			outside := outsideDir(t)
			require.NoError(t, os.MkdirAll(filepath.Join(outside, "dev", "home-"+testAgentID), 0o755))
			require.NoError(t, os.MkdirAll(filepath.Join(outside, "home-"+testAgentID), 0o755))
			link := filepath.Join(root, linkAt)
			require.NoError(t, os.MkdirAll(filepath.Dir(link), 0o755))
			require.NoError(t, os.Symlink(outside, link))

			ds, err := delNFSLayout(hostBase).RemoveAgentStorage(testPID, "dev", testAgentID)
			require.NoError(t, err)
			require.Len(t, ds, 3)
			for _, d := range ds {
				assert.Equal(t, OutcomeFailed, d.Outcome, string(d.Tree))
				assert.False(t, Retryable(d))
			}
			assert.FileExists(t, filepath.Join(outside, "secret.txt"))
			assert.DirExists(t, filepath.Join(outside, "dev", "home-"+testAgentID))
			assert.DirExists(t, filepath.Join(outside, "home-"+testAgentID))
		})
	}
	hostBase := t.TempDir()
	root := seedProjectTree(t, hostBase, testPID)
	for _, agent := range []string{"", "..", "../dev", "Dev", "a/b"} {
		_, err := delNFSLayout(hostBase).RemoveAgentStorage(testPID, agent, testAgentID)
		assert.Error(t, err, "agent %q", agent)
	}
	for _, id := range []string{"", "not-a-uuid", "../" + testAgentID} {
		_, err := delNFSLayout(hostBase).RemoveAgentStorage(testPID, "dev", id)
		assert.Error(t, err, "agent ID %q", id)
	}
	_, err := delNFSLayout(hostBase).RemoveAgentStorage("../"+testPID, "dev", testAgentID)
	assert.Error(t, err)
	assert.DirExists(t, filepath.Join(root, "agents", "dev", "home-"+testAgentID))
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

	ds, err := l.RemoveProjectTree(testPID)
	require.NoError(t, err)
	require.Len(t, ds, 1)
	assert.Equal(t, OutcomeFailed, ds[0].Outcome)
	ds, err = l.RemoveAgentStorage(testPID, "dev", testAgentID)
	require.NoError(t, err)
	for _, d := range ds {
		assert.Equal(t, OutcomeFailed, d.Outcome, string(d.Tree))
	}
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

	ds, err := delNFSLayout(hostBase).RemoveProjectTree(testPID)
	require.NoError(t, err)
	require.Len(t, ds, 1)
	assert.Equal(t, OutcomeFailed, ds[0].Outcome)
	assert.True(t, Retryable(ds[0]))

	require.NoError(t, os.Chmod(ws, 0o755))
	ds, err = delNFSLayout(hostBase).RemoveProjectTree(testPID)
	require.NoError(t, err)
	assert.Equal(t, OutcomeDeleted, ds[0].Outcome)
	assert.NoDirExists(t, root)
}

// A share whose mount point exists but whose tree does not (the share is
// not mounted) fails the deletion; it never reads as absent.
func TestDeleteProject_ShareNotMountedFails(t *testing.T) {
	hostBase := t.TempDir() // the mount point, with nothing mounted on it
	ds, err := delNFSLayout(hostBase).RemoveProjectTree(testPID)
	require.NoError(t, err)
	require.Len(t, ds, 1)
	assert.Equal(t, OutcomeFailed, ds[0].Outcome)
	assert.ErrorContains(t, ds[0].Err, "share not mounted")
	assert.False(t, Retryable(ds[0]))
}

// The tombstone of a deferred deletion is written into the project's
// provision/ directory, listed with its time, and cleared; a project root
// that does not exist needs none; a missing tree is an error for the
// listing (the share may not be mounted).
func TestDeleteAfterTombstone(t *testing.T) {
	hostBase := t.TempDir()
	root := seedProjectTree(t, hostBase, testPID)
	seedProjectTree(t, hostBase, testOtherPID)
	l := delNFSLayout(hostBase)
	after := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

	ds, err := l.MarkDeleteAfter(testPID, after)
	require.NoError(t, err)
	require.Len(t, ds, 1)
	assert.Equal(t, OutcomeKept, ds[0].Outcome)
	assert.Contains(t, ds[0].Reason, "deferred until 2026-10-27T12:00:00Z")
	data, err := os.ReadFile(filepath.Join(root, "provision", DeleteAfterFile))
	require.NoError(t, err)
	assert.Equal(t, "2026-10-27T12:00:00Z\n", string(data))
	assert.DirExists(t, filepath.Join(root, "workspace", ".git"), "nothing is deleted")

	got, err := l.DeferredDeletions()
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, testPID, got[0].ProjectID)
	assert.True(t, got[0].After.Equal(after))
	assert.Equal(t, root, got[0].Path)

	require.NoError(t, l.ClearDeleteAfter(testPID))
	got, err = l.DeferredDeletions()
	require.NoError(t, err)
	assert.Empty(t, got)
	require.NoError(t, l.ClearDeleteAfter(testPID), "clearing again is nothing to do")

	ds, err = l.MarkDeleteAfter("770e8400-e29b-41d4-a716-446655440099", after)
	require.NoError(t, err)
	assert.Equal(t, OutcomeAbsent, ds[0].Outcome, "no tree, no tombstone")

	_, err = delNFSLayout(t.TempDir()).DeferredDeletions()
	assert.Error(t, err, "a share without its tree is reported")
	_, err = l.MarkDeleteAfter("../x", after)
	assert.Error(t, err)
}

// The tombstone's directory is outside every directory an agent mounts:
// the workspace in every mode (project-level and per agent), shared dirs,
// homes and secrets.
func TestDeleteAfterTombstoneOutsideAgentMounts(t *testing.T) {
	l := Layout{Backend: BackendNFS, HostBase: "/mnt/nfs/share1", SubPathRoot: "projects", Homes: BackendNFS}
	tomb, err := l.DeleteAfterPath(testPID)
	require.NoError(t, err)
	var mounted []string
	for _, mode := range []Mode{ModeSharedPlain, ModeClonePerAgent, ModeWorktreePerAgent, ModeEmptyPerAgent} {
		for _, agent := range []string{"", "dev"} {
			r, err := l.Resolve(Project{ID: testPID}, agent, WorkspaceMount(mode, ""))
			require.NoError(t, err)
			mounted = append(mounted, r.HostPath)
		}
	}
	for _, name := range []string{"scratchpad", "data"} {
		r, err := l.Resolve(Project{ID: testPID}, "", SharedDirMount(api.SharedDir{Name: name}, ""))
		require.NoError(t, err)
		mounted = append(mounted, r.HostPath)
	}
	for _, f := range []func(string, string, string) (Resolved, error){l.AgentHome, l.AgentSecrets} {
		r, err := f(testPID, "dev", testAgentID)
		require.NoError(t, err)
		mounted = append(mounted, r.HostPath)
	}
	for _, m := range mounted {
		rel, err := filepath.Rel(m, tomb.HostPath)
		require.NoError(t, err)
		assert.True(t, strings.HasPrefix(rel, ".."), "the tombstone dir %s must not be inside mount %s", tomb.HostPath, m)
		rel, err = filepath.Rel(tomb.HostPath, m)
		require.NoError(t, err)
		assert.True(t, strings.HasPrefix(rel, ".."), "mount %s must not be inside the tombstone dir", m)
	}
}
