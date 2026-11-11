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
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests in this file stub lchownFile to refuse every chown, leaving each
// entry owned by the test process. That is what root in the Kubernetes init
// container sees on a squashing NFS export: a refused chown of entries that
// already have the wanted owner. The wanted owner is the test process's own
// uid:gid; 0 would mean the default owner 1000, so the tests need a non-root
// uid and gid.
func requireNonRootOwner(t *testing.T) {
	t.Helper()
	if os.Getuid() == 0 || os.Getgid() == 0 {
		t.Skip("needs a non-zero uid and gid: 0 selects the default owner 1000")
	}
}

// agentDirOwnerInput is agentDirInput with the given owner.
func agentDirOwnerInput(agentDir string, uid, gid int) ProvisionInput {
	in := agentDirInput(agentDir, "agent-1", "scion/agent-1")
	in.NFSUID, in.NFSGID = uid, gid
	return in
}

// An EPERM chown of entries already owned by NFSUID:NFSGID does not fail
// ProvisionAgentDir under RequireChownSuccess, and the sentinel is written.
func TestProvisionAgentDir_EPERM_OwnerMatches_Succeeds(t *testing.T) {
	requireNonRootOwner(t)
	agentDir := newAgentDir(t)
	stubLchown(t, syscall.EPERM)

	require.NoError(t, ProvisionAgentDir(agentDirOwnerInput(agentDir, os.Getuid(), os.Getgid())))
	assert.FileExists(t, filepath.Join(agentDir, ProvisionSentinelFile))
}

// EPERM on an entry with another owner stays fatal, and names the owner.
func TestProvisionAgentDir_EPERM_OwnerMismatch_Fails(t *testing.T) {
	requireNonRootOwner(t)
	agentDir := newAgentDir(t)
	stubLchown(t, syscall.EPERM)

	err := ProvisionAgentDir(agentDirOwnerInput(agentDir, os.Getuid()+1, os.Getgid()))
	require.Error(t, err)
	assert.ErrorIs(t, err, syscall.EPERM)
	assert.Contains(t, err.Error(), "owner is")
	assert.NoFileExists(t, filepath.Join(agentDir, ProvisionSentinelFile))
}

// Any other chown error stays fatal even when the owner already matches.
func TestProvisionAgentDir_NonEPERM_OwnerMatches_Fails(t *testing.T) {
	requireNonRootOwner(t)
	agentDir := newAgentDir(t)
	stubLchown(t, syscall.EIO)

	err := ProvisionAgentDir(agentDirOwnerInput(agentDir, os.Getuid(), os.Getgid()))
	require.Error(t, err)
	assert.ErrorIs(t, err, syscall.EIO)
	assert.NoFileExists(t, filepath.Join(agentDir, ProvisionSentinelFile))
}

// provisionMountedWorktreeForRestart provisions agent-1's mounted worktree
// with chown succeeding, and returns the input for its restart. A restart
// reuses the worktree, so the only chowns it makes are the single,
// non-recursive ones of the files the worktree step rewrote (.git/config,
// the sharer marker).
func provisionMountedWorktreeForRestart(t *testing.T, uid, gid int) (string, ProvisionInput) {
	t.Helper()
	origin := initBareGitRepo(t)
	workspace := filepath.Join(t.TempDir(), "workspace")
	recordLchown(t, nil)
	require.NoError(t, ProvisionShared(mountedWorktreeInput(workspace, origin, "agent-1", "agent-one")))
	in := mountedWorktreeInput(workspace, origin, "agent-1", "agent-one")
	in.NFSUID, in.NFSGID = uid, gid
	return workspace, in
}

func TestProvisionShared_MountedWorktree_EPERM_OwnerMatches_Succeeds(t *testing.T) {
	requireNonRootOwner(t)
	_, in := provisionMountedWorktreeForRestart(t, os.Getuid(), os.Getgid())
	stubLchown(t, syscall.EPERM)

	require.NoError(t, ProvisionShared(in))
}

func TestProvisionShared_MountedWorktree_EPERM_OwnerMismatch_Fails(t *testing.T) {
	requireNonRootOwner(t)
	workspace, in := provisionMountedWorktreeForRestart(t, os.Getuid()+1, os.Getgid())
	stubLchown(t, syscall.EPERM)

	err := ProvisionShared(in)
	require.Error(t, err)
	assert.ErrorIs(t, err, syscall.EPERM)
	assert.Contains(t, err.Error(), "owner is")
	assert.Contains(t, err.Error(), filepath.Join(workspace, ".git"), "the failing chown is of the shared .git, not the worktree")
}

func TestProvisionShared_MountedWorktree_NonEPERM_OwnerMatches_Fails(t *testing.T) {
	requireNonRootOwner(t)
	_, in := provisionMountedWorktreeForRestart(t, os.Getuid(), os.Getgid())
	stubLchown(t, syscall.EIO)

	err := ProvisionShared(in)
	require.Error(t, err)
	assert.ErrorIs(t, err, syscall.EIO)
}

// recreateEmptyDir: a refused chown of the new directory, which already has
// the owner it is given, still creates it and applies the mode.
func TestRecreateEmptyDir_EPERM_OwnerMatches_Succeeds(t *testing.T) {
	requireNonRootOwner(t)
	dir := filepath.Join(t.TempDir(), "agent-1")
	stubLchown(t, syscall.EPERM)

	require.NoError(t, recreateEmptyDir(dir, 0o750, os.Getuid(), os.Getgid()))
	fi, err := os.Lstat(dir)
	require.NoError(t, err)
	assert.True(t, fi.IsDir())
	assert.Equal(t, os.FileMode(0o750), fi.Mode().Perm(), "the mode step still runs")
}

func TestRecreateEmptyDir_EPERM_OwnerMismatch_Fails(t *testing.T) {
	requireNonRootOwner(t)
	dir := filepath.Join(t.TempDir(), "agent-1")
	stubLchown(t, syscall.EPERM)

	err := recreateEmptyDir(dir, 0o750, os.Getuid()+1, os.Getgid())
	require.Error(t, err)
	assert.ErrorIs(t, err, syscall.EPERM)
	assert.Contains(t, err.Error(), "owner is")
}

func TestRecreateEmptyDir_NonEPERM_OwnerMatches_Fails(t *testing.T) {
	requireNonRootOwner(t)
	dir := filepath.Join(t.TempDir(), "agent-1")
	stubLchown(t, syscall.EIO)

	err := recreateEmptyDir(dir, 0o750, os.Getuid(), os.Getgid())
	require.Error(t, err)
	assert.ErrorIs(t, err, syscall.EIO)
}
