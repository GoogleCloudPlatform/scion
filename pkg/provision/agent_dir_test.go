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

package provision

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// agentDirInput is the input the Kubernetes init container builds for a
// clone-per-agent agent: the agent directory is mounted, AgentID is the
// slug and AgentName the branch.
func agentDirInput(agentDir, slug, branch string) ProvisionInput {
	return ProvisionInput{
		Resolved:            ResolvedWorkspace{HostPath: agentDir},
		ProjectID:           "proj-1",
		AgentID:             slug,
		AgentName:           branch,
		Mode:                store.SharingModeClonePerAgent,
		NFSUID:              4321,
		NFSGID:              8765,
		RequireChownSuccess: true,
	}
}

type chownCall struct {
	path     string
	uid, gid int
}

// recordChownCalls replaces lchownFile for the test and records each call.
func recordChownCalls(t *testing.T, fail func(string) bool) *[]chownCall {
	t.Helper()
	var mu sync.Mutex
	var calls []chownCall
	orig := lchownFile
	lchownFile = func(name string, uid, gid int) error {
		mu.Lock()
		calls = append(calls, chownCall{name, uid, gid})
		mu.Unlock()
		if fail != nil && fail(name) {
			return errors.New("operation not permitted")
		}
		return nil
	}
	t.Cleanup(func() { lchownFile = orig })
	return &calls
}

func chownedPaths(calls []chownCall) []string {
	var paths []string
	for _, c := range calls {
		paths = append(paths, c.path)
	}
	sort.Strings(paths)
	return paths
}

func newAgentDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "agents", "agent-1")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	return dir
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// First start: an empty workspace is created, the branch is recorded, the
// workspace directory and the record are chowned, and the sentinel is
// written next to the workspace. The workspace stays empty so the agent
// container's clone step can fill it.
func TestProvisionAgentDir_FirstStart(t *testing.T) {
	agentDir := newAgentDir(t)
	calls := recordChownCalls(t, nil)

	require.NoError(t, ProvisionAgentDir(agentDirInput(agentDir, "agent-1", "scion/agent-1")))

	workspace := filepath.Join(agentDir, AgentWorkspaceDir)
	assert.Empty(t, dirNames(t, workspace))
	assert.Equal(t, "scion/agent-1", readAgentBranch(filepath.Join(agentDir, AgentBranchFile)))
	assert.FileExists(t, filepath.Join(agentDir, ProvisionSentinelFile))
	assert.Equal(t, []chownCall{
		{workspace, 4321, 8765},
		{filepath.Join(agentDir, AgentBranchFile), 4321, 8765},
	}, *calls)
}

// Without NFSUID/NFSGID the chown targets 1000:1000, as for the shared
// workspace.
func TestProvisionAgentDir_DefaultOwner(t *testing.T) {
	agentDir := newAgentDir(t)
	calls := recordChownCalls(t, nil)
	in := agentDirInput(agentDir, "agent-1", "scion/agent-1")
	in.NFSUID, in.NFSGID = 0, 0
	require.NoError(t, ProvisionAgentDir(in))
	require.NotEmpty(t, *calls)
	for _, c := range *calls {
		assert.Equal(t, 1000, c.uid, c.path)
		assert.Equal(t, 1000, c.gid, c.path)
	}
}

// A restart with the same branch reuses the kept workspace: its files stay,
// and nothing is chowned again.
func TestProvisionAgentDir_RestartReusesWorkspace(t *testing.T) {
	agentDir := newAgentDir(t)
	recordChownCalls(t, nil)
	in := agentDirInput(agentDir, "agent-1", "scion/agent-1")
	require.NoError(t, ProvisionAgentDir(in))
	workspace := filepath.Join(agentDir, AgentWorkspaceDir)
	require.NoError(t, os.Mkdir(filepath.Join(workspace, ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "work.txt"), []byte("kept"), 0o644))

	calls := recordChownCalls(t, nil)
	require.NoError(t, ProvisionAgentDir(in))
	assert.Empty(t, *calls, "a kept workspace is not chowned again")
	data, err := os.ReadFile(filepath.Join(workspace, "work.txt"))
	require.NoError(t, err)
	assert.Equal(t, "kept", string(data))
}

// A kept workspace created for another branch is refused, and nothing is
// changed.
func TestProvisionAgentDir_BranchMismatch(t *testing.T) {
	agentDir := newAgentDir(t)
	recordChownCalls(t, nil)
	require.NoError(t, ProvisionAgentDir(agentDirInput(agentDir, "agent-1", "scion/agent-1")))
	workspace := filepath.Join(agentDir, AgentWorkspaceDir)
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "work.txt"), []byte("kept"), 0o644))

	calls := recordChownCalls(t, nil)
	err := ProvisionAgentDir(agentDirInput(agentDir, "agent-1", "feature/other"))
	require.ErrorIs(t, err, ErrAgentBranchMismatch)
	assert.Contains(t, err.Error(), `"scion/agent-1"`)
	assert.Contains(t, err.Error(), `"feature/other"`)
	assert.Equal(t, "scion/agent-1", readAgentBranch(filepath.Join(agentDir, AgentBranchFile)))
	assert.FileExists(t, filepath.Join(workspace, "work.txt"))
	assert.Empty(t, *calls)
}

// A workspace that is still empty (the clone never ran, or a failed clone
// was cleaned up) takes the new branch: the record is rewritten.
func TestProvisionAgentDir_EmptyWorkspaceTakesNewBranch(t *testing.T) {
	agentDir := newAgentDir(t)
	recordChownCalls(t, nil)
	require.NoError(t, ProvisionAgentDir(agentDirInput(agentDir, "agent-1", "scion/agent-1")))

	calls := recordChownCalls(t, nil)
	require.NoError(t, ProvisionAgentDir(agentDirInput(agentDir, "agent-1", "feature/other")))
	assert.Equal(t, "feature/other", readAgentBranch(filepath.Join(agentDir, AgentBranchFile)))
	assert.Equal(t, []string{
		filepath.Join(agentDir, AgentBranchFile),
		filepath.Join(agentDir, AgentWorkspaceDir),
	}, chownedPaths(*calls))
}

// A workspace that holds only marker entries, such as the mount point of a
// shared dir the kubelet created inside it and a failed clone kept, counts
// as empty, the same as for the clone step: it takes the new branch and its
// directory is chowned.
func TestProvisionAgentDir_MarkerOnlyWorkspaceTakesNewBranch(t *testing.T) {
	for _, marker := range []string{".scion-volumes", ".scion", ".agents"} {
		t.Run(marker, func(t *testing.T) {
			agentDir := newAgentDir(t)
			recordChownCalls(t, nil)
			require.NoError(t, ProvisionAgentDir(agentDirInput(agentDir, "agent-1", "scion/agent-1")))
			workspace := filepath.Join(agentDir, AgentWorkspaceDir)
			require.NoError(t, os.MkdirAll(filepath.Join(workspace, marker, "x"), 0o755))

			calls := recordChownCalls(t, nil)
			require.NoError(t, ProvisionAgentDir(agentDirInput(agentDir, "agent-1", "feature/other")))
			assert.Equal(t, "feature/other", readAgentBranch(filepath.Join(agentDir, AgentBranchFile)))
			assert.Equal(t, []string{
				filepath.Join(agentDir, AgentBranchFile),
				workspace,
			}, chownedPaths(*calls))
			assert.DirExists(t, filepath.Join(workspace, marker, "x"), "marker entries are left as they are")
		})
	}
}

// Any entry besides the marker entries makes the workspace populated.
func TestWorkspaceHasContent(t *testing.T) {
	missing, err := WorkspaceHasContent(filepath.Join(t.TempDir(), "missing"))
	require.NoError(t, err)
	assert.False(t, missing)

	dir := t.TempDir()
	for _, marker := range []string{".scion", ".scion-volumes", ".agents"} {
		require.NoError(t, os.Mkdir(filepath.Join(dir, marker), 0o755))
	}
	has, err := WorkspaceHasContent(dir)
	require.NoError(t, err)
	assert.False(t, has, "marker entries only")

	for _, name := range []string{".git", ".scion-other", "README.md"} {
		sub := t.TempDir()
		require.NoError(t, os.Mkdir(filepath.Join(sub, ".scion-volumes"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(sub, name), nil, 0o644))
		has, err := WorkspaceHasContent(sub)
		require.NoError(t, err)
		assert.True(t, has, name)
	}
}

// A populated workspace without a record (an older image prepared the agent
// directory) is kept and gets the requested branch recorded.
func TestProvisionAgentDir_PopulatedWithoutRecord(t *testing.T) {
	agentDir := newAgentDir(t)
	workspace := filepath.Join(agentDir, AgentWorkspaceDir)
	require.NoError(t, os.Mkdir(workspace, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "work.txt"), []byte("kept"), 0o644))

	calls := recordChownCalls(t, nil)
	require.NoError(t, ProvisionAgentDir(agentDirInput(agentDir, "agent-1", "scion/agent-1")))
	assert.Equal(t, "scion/agent-1", readAgentBranch(filepath.Join(agentDir, AgentBranchFile)))
	assert.Equal(t, []string{filepath.Join(agentDir, AgentBranchFile)}, chownedPaths(*calls))
}

// Delete with files, then create again: the workspace and the record are
// gone, the moved-aside directory is purged, and the next start begins
// with an empty workspace on any branch.
func TestAgentWorkspace_DeleteAndRecreate(t *testing.T) {
	agentDir := newAgentDir(t)
	recordChownCalls(t, nil)
	require.NoError(t, ProvisionAgentDir(agentDirInput(agentDir, "agent-1", "scion/agent-1")))
	workspace := filepath.Join(agentDir, AgentWorkspaceDir)
	require.NoError(t, os.MkdirAll(filepath.Join(workspace, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "sub", "f"), []byte("x"), 0o644))
	unrelated := filepath.Join(agentDir, ".other")
	require.NoError(t, os.WriteFile(unrelated, []byte("x"), 0o644))

	require.NoError(t, RemoveAgentWorkspace(context.Background(), agentDir, testLockWait))
	assert.NoDirExists(t, workspace)
	assert.NoFileExists(t, filepath.Join(agentDir, AgentBranchFile))
	moved, err := filepath.Glob(filepath.Join(agentDir, removedAgentWorkspacePrefix+"*"))
	require.NoError(t, err)
	require.Len(t, moved, 1)

	require.NoError(t, PurgeRemovedAgentWorkspaces(agentDir))
	assert.NoDirExists(t, moved[0])
	assert.FileExists(t, unrelated, "the purge deletes only moved-aside workspaces")
	assert.FileExists(t, filepath.Join(agentDir, ProvisionSentinelFile))

	require.NoError(t, ProvisionAgentDir(agentDirInput(agentDir, "agent-1", "feature/other")))
	assert.Empty(t, dirNames(t, workspace))
	assert.Equal(t, "feature/other", readAgentBranch(filepath.Join(agentDir, AgentBranchFile)))
}

// Delete without a workspace, or without an agent directory, is not an
// error and changes nothing.
func TestRemoveAgentWorkspace_NothingToRemove(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "agents", "agent-1")
	require.NoError(t, RemoveAgentWorkspace(context.Background(), missing, testLockWait))
	assert.NoDirExists(t, missing)

	agentDir := newAgentDir(t)
	require.NoError(t, RemoveAgentWorkspace(context.Background(), agentDir, testLockWait))
	require.NoError(t, PurgeRemovedAgentWorkspaces(agentDir))
	require.NoError(t, PurgeRemovedAgentWorkspaces(missing))
}

// Symlinks are refused and left in place: an agent directory or a
// workspace that is a symlink, and a workspace that is a file.
func TestAgentDir_RefusesNonPlainDirs(t *testing.T) {
	recordChownCalls(t, nil)
	target := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(target, "keep"), []byte("x"), 0o644))
	require.NoError(t, os.Mkdir(filepath.Join(target, removedAgentWorkspacePrefix+"keep"), 0o755))

	// The agent directory is a symlink.
	link := filepath.Join(t.TempDir(), "agent-1")
	require.NoError(t, os.Symlink(target, link))
	assert.Error(t, ProvisionAgentDir(agentDirInput(link, "agent-1", "scion/agent-1")))
	assert.Error(t, RemoveAgentWorkspace(context.Background(), link, testLockWait))
	require.NoError(t, PurgeRemovedAgentWorkspaces(link))
	assert.Equal(t, []string{removedAgentWorkspacePrefix + "keep", "keep"}, dirNames(t, target))

	// The workspace is a symlink.
	agentDir := newAgentDir(t)
	require.NoError(t, os.Symlink(target, filepath.Join(agentDir, AgentWorkspaceDir)))
	assert.Error(t, ProvisionAgentDir(agentDirInput(agentDir, "agent-1", "scion/agent-1")))
	assert.Error(t, RemoveAgentWorkspace(context.Background(), agentDir, testLockWait))
	assert.Equal(t, []string{removedAgentWorkspacePrefix + "keep", "keep"}, dirNames(t, target))
	fi, err := os.Lstat(filepath.Join(agentDir, AgentWorkspaceDir))
	require.NoError(t, err)
	assert.NotZero(t, fi.Mode()&os.ModeSymlink, "left in place")

	// The workspace is a file.
	agentDir = newAgentDir(t)
	require.NoError(t, os.WriteFile(filepath.Join(agentDir, AgentWorkspaceDir), []byte("x"), 0o644))
	assert.Error(t, ProvisionAgentDir(agentDirInput(agentDir, "agent-1", "scion/agent-1")))
	assert.Error(t, RemoveAgentWorkspace(context.Background(), agentDir, testLockWait))
	assert.FileExists(t, filepath.Join(agentDir, AgentWorkspaceDir))
}

// A symlinked branch record is not read and is replaced.
func TestProvisionAgentDir_SymlinkedRecordIgnored(t *testing.T) {
	agentDir := newAgentDir(t)
	recordChownCalls(t, nil)
	other := filepath.Join(t.TempDir(), "branch")
	require.NoError(t, os.WriteFile(other, []byte("feature/other\n"), 0o644))
	require.NoError(t, os.Symlink(other, filepath.Join(agentDir, AgentBranchFile)))
	workspace := filepath.Join(agentDir, AgentWorkspaceDir)
	require.NoError(t, os.Mkdir(workspace, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "f"), []byte("x"), 0o644))

	require.NoError(t, ProvisionAgentDir(agentDirInput(agentDir, "agent-1", "scion/agent-1")))
	assert.Equal(t, "scion/agent-1", readAgentBranch(filepath.Join(agentDir, AgentBranchFile)))
	data, err := os.ReadFile(other)
	require.NoError(t, err)
	assert.Equal(t, "feature/other\n", string(data), "the symlink target is not written")
}

func TestProvisionAgentDir_RejectsBadInput(t *testing.T) {
	recordChownCalls(t, nil)
	for name, mutate := range map[string]func(*ProvisionInput){
		"shared-plain mode":   func(in *ProvisionInput) { in.Mode = store.SharingModeSharedPlain },
		"worktree mode":       func(in *ProvisionInput) { in.Mode = store.SharingModeWorktreePerAgent },
		"empty slug":          func(in *ProvisionInput) { in.AgentID = "" },
		"dot-dot slug":        func(in *ProvisionInput) { in.AgentID = ".." },
		"slug with slash":     func(in *ProvisionInput) { in.AgentID = "a/b" },
		"upper-case slug":     func(in *ProvisionInput) { in.AgentID = "Agent-1" },
		"empty branch":        func(in *ProvisionInput) { in.AgentName = " " },
		"branch with newline": func(in *ProvisionInput) { in.AgentName = "a\nb" },
		"leading space":       func(in *ProvisionInput) { in.AgentName = " scion/agent-1" },
		"trailing space":      func(in *ProvisionInput) { in.AgentName = "scion/agent-1 " },
		"trailing tab":        func(in *ProvisionInput) { in.AgentName = "scion/agent-1\t" },
		"no host path":        func(in *ProvisionInput) { in.Resolved.HostPath = "" },
		"missing agent dir":   func(in *ProvisionInput) { in.Resolved.HostPath = filepath.Join(in.Resolved.HostPath, "missing") },
	} {
		t.Run(name, func(t *testing.T) {
			agentDir := newAgentDir(t)
			in := agentDirInput(agentDir, "agent-1", "scion/agent-1")
			mutate(&in)
			require.Error(t, ProvisionAgentDir(in))
			assert.Empty(t, dirNames(t, agentDir), "nothing is created")
		})
	}
}

// A failed chown is fatal unless best-effort chown was requested.
func TestProvisionAgentDir_ChownFailure(t *testing.T) {
	agentDir := newAgentDir(t)
	recordChownCalls(t, func(string) bool { return true })
	err := ProvisionAgentDir(agentDirInput(agentDir, "agent-1", "scion/agent-1"))
	require.Error(t, err)
	assert.NoFileExists(t, filepath.Join(agentDir, ProvisionSentinelFile))

	in := agentDirInput(agentDir, "agent-1", "scion/agent-1")
	in.RequireChownSuccess = false
	require.NoError(t, ProvisionAgentDir(in))
	assert.FileExists(t, filepath.Join(agentDir, ProvisionSentinelFile))
}

// Shared dirs are created and chowned while empty; a populated one is left
// as it is.
func TestProvisionAgentDir_SharedDirs(t *testing.T) {
	agentDir := newAgentDir(t)
	base := t.TempDir()
	empty := filepath.Join(base, "shared-dirs", "scratchpad")
	populated := filepath.Join(base, "shared-dirs", "cache")
	require.NoError(t, os.MkdirAll(populated, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(populated, "f"), []byte("x"), 0o644))
	calls := recordChownCalls(t, nil)

	in := agentDirInput(agentDir, "agent-1", "scion/agent-1")
	in.Resolved.SharedDirs = map[string]ResolvedSharedDir{
		"scratchpad": {HostPath: empty},
		"cache":      {HostPath: populated},
	}
	require.NoError(t, ProvisionAgentDir(in))
	assert.DirExists(t, empty)
	paths := chownedPaths(*calls)
	assert.Contains(t, paths, empty)
	assert.NotContains(t, paths, populated)
}

// Provisioning and removal take the agent directory's lock: while another
// holder has it, both give up after their wait and change nothing.
func TestAgentDir_Lock(t *testing.T) {
	agentDir := newAgentDir(t)
	recordChownCalls(t, nil)
	require.NoError(t, ProvisionAgentDir(agentDirInput(agentDir, "agent-1", "scion/agent-1")))

	held, err := acquireFileLock(context.Background(), agentDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = held.release() })

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	in := agentDirInput(agentDir, "agent-1", "feature/other")
	in.Ctx = ctx
	require.Error(t, ProvisionAgentDir(in))
	assert.Equal(t, "scion/agent-1", readAgentBranch(filepath.Join(agentDir, AgentBranchFile)))

	require.Error(t, RemoveAgentWorkspace(context.Background(), agentDir, 300*time.Millisecond))
	assert.DirExists(t, filepath.Join(agentDir, AgentWorkspaceDir))

	require.NoError(t, held.release())
	require.NoError(t, RemoveAgentWorkspace(context.Background(), agentDir, testLockWait))
	assert.NoDirExists(t, filepath.Join(agentDir, AgentWorkspaceDir))
}

// Removal is not stopped by the caller's cancellation: cancelled while it
// waits for the lock, it still takes the lock once it is free and removes
// the workspace.
func TestRemoveAgentWorkspace_IgnoresCancellation(t *testing.T) {
	agentDir := newAgentDir(t)
	recordChownCalls(t, nil)
	require.NoError(t, ProvisionAgentDir(agentDirInput(agentDir, "agent-1", "scion/agent-1")))
	held, err := acquireFileLock(context.Background(), agentDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = held.release() })

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
		time.Sleep(200 * time.Millisecond)
		_ = held.release()
	}()
	require.NoError(t, RemoveAgentWorkspace(ctx, agentDir, testLockWait))
	assert.NoDirExists(t, filepath.Join(agentDir, AgentWorkspaceDir))
}

// Empty-per-agent (design #2703 P3): the same steps as clone-per-agent
// except the branch. The workspace is created and chowned, the sentinel is
// written, and no branch record is checked or written, so any branch input
// (or none) is accepted. A restart keeps the workspace's files and chowns
// nothing again; a record left by an earlier clone-per-agent agent of the
// same name is left as it is and does not refuse the start.
func TestProvisionAgentDir_EmptyPerAgent(t *testing.T) {
	agentDir := newAgentDir(t)
	calls := recordChownCalls(t, nil)
	in := agentDirInput(agentDir, "agent-1", "")
	in.Mode = store.SharingModeEmptyPerAgent

	require.NoError(t, ProvisionAgentDir(in))
	workspace := filepath.Join(agentDir, AgentWorkspaceDir)
	assert.Empty(t, dirNames(t, workspace))
	assert.NoFileExists(t, filepath.Join(agentDir, AgentBranchFile))
	assert.FileExists(t, filepath.Join(agentDir, ProvisionSentinelFile))
	assert.Equal(t, []chownCall{{workspace, 4321, 8765}}, *calls)

	require.NoError(t, os.WriteFile(filepath.Join(workspace, "notes.txt"), []byte("kept"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(agentDir, AgentBranchFile), []byte("scion/old\n"), 0o644))
	calls = recordChownCalls(t, nil)
	in.AgentName = "feature/ignored"
	require.NoError(t, ProvisionAgentDir(in))
	assert.Empty(t, *calls, "a workspace with files is not chowned again")
	assert.FileExists(t, filepath.Join(workspace, "notes.txt"))
	assert.Equal(t, "scion/old", readAgentBranch(filepath.Join(agentDir, AgentBranchFile)), "an existing record is not rewritten")
}

// Empty-per-agent still needs the agent's slug and an agent directory.
func TestProvisionAgentDir_EmptyPerAgentRejectsBadInput(t *testing.T) {
	recordChownCalls(t, nil)
	for name, mutate := range map[string]func(*ProvisionInput){
		"empty slug":        func(in *ProvisionInput) { in.AgentID = "" },
		"dot-dot slug":      func(in *ProvisionInput) { in.AgentID = ".." },
		"upper-case slug":   func(in *ProvisionInput) { in.AgentID = "Agent-1" },
		"no host path":      func(in *ProvisionInput) { in.Resolved.HostPath = "" },
		"missing agent dir": func(in *ProvisionInput) { in.Resolved.HostPath = filepath.Join(in.Resolved.HostPath, "missing") },
	} {
		t.Run(name, func(t *testing.T) {
			agentDir := newAgentDir(t)
			in := agentDirInput(agentDir, "agent-1", "")
			in.Mode = store.SharingModeEmptyPerAgent
			mutate(&in)
			require.Error(t, ProvisionAgentDir(in))
			assert.Empty(t, dirNames(t, agentDir), "nothing is created")
		})
	}
}

// Delete of an empty-per-agent agent's files removes only its workspace:
// the agent directory and anything else in it (such as a home directory
// kept next to the workspace) stay.
func TestRemoveAgentWorkspace_EmptyPerAgentKeepsSiblings(t *testing.T) {
	agentDir := newAgentDir(t)
	recordChownCalls(t, nil)
	in := agentDirInput(agentDir, "agent-1", "")
	in.Mode = store.SharingModeEmptyPerAgent
	require.NoError(t, ProvisionAgentDir(in))
	workspace := filepath.Join(agentDir, AgentWorkspaceDir)
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "notes.txt"), []byte("x"), 0o644))
	home := filepath.Join(agentDir, "home-agent-id-1")
	require.NoError(t, os.MkdirAll(home, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".profile"), []byte("x"), 0o644))

	require.NoError(t, RemoveAgentWorkspace(context.Background(), agentDir, testLockWait))
	require.NoError(t, PurgeRemovedAgentWorkspaces(agentDir))
	assert.NoDirExists(t, workspace)
	assert.FileExists(t, filepath.Join(home, ".profile"))
	assert.FileExists(t, filepath.Join(agentDir, ProvisionSentinelFile))
}
