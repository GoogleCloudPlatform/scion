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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The NFS worktree removal passes the guard: an agent worktree that is a
// symlink to the project's shared checkout (the worktrees directory itself
// is plain) is refused, and the checkout and the other agent's worktree
// survive.
func TestRemoveNFSWorktree_GuardRefusesWorktreeResolvingToCheckout(t *testing.T) {
	f, ws := setupNFSWorktreeRemoval(t)
	worktree := provision.WorktreePath(ws, "test-agent")
	require.NoError(t, os.RemoveAll(worktree))
	require.NoError(t, os.Symlink(ws, worktree))

	_, err := kubernetesTestManager("kubernetes").RemoveNFSWorktree(context.Background(), f.projectScionDir, testNFSWorkspaceProjectID, "test-agent")
	assert.True(t, errors.Is(err, ErrAgentFilesDeleteRefused), "err = %v, want ErrAgentFilesDeleteRefused", err)
	assert.FileExists(t, filepath.Join(ws, "README.md"))
	assert.True(t, provision.IsRealWorktreeDir(provision.WorktreePath(ws, "other-agent"), ws))
}

// The NFS agent workspace removal passes the guard: when the project's
// checkout path resolves to the agent directory, the agent directory is
// (or contains) a project workspace, so the removal is refused and the
// agent's workspace survives.
func TestRemoveNFSAgentFiles_GuardRefusesAgentDirHoldingCheckout(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)
	mountRoot := filepath.Join(t.TempDir(), "nfs")
	f.writeGlobalSettings(t, fmt.Sprintf(nfsWorkspaceStartYAML, mountRoot))
	agentDir := provisionTestAgentDir(t, mountRoot, "test-agent")
	runAgentWorkspacePurgeInline(t)
	ws := nfsTestWorkspaceDir(mountRoot)
	require.NoError(t, os.MkdirAll(filepath.Dir(ws), 0o755))
	require.NoError(t, os.Symlink(agentDir, ws))

	_, err := kubernetesTestManager("kubernetes").RemoveNFSAgentFiles(context.Background(), f.projectScionDir, testNFSWorkspaceProjectID, "test-agent")
	assert.True(t, errors.Is(err, ErrAgentFilesDeleteRefused), "err = %v, want ErrAgentFilesDeleteRefused", err)
	assert.FileExists(t, filepath.Join(agentDir, provision.AgentWorkspaceDir, "README.md"))
}
