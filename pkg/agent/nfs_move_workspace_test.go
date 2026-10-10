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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The target broker confirms a moved agent's workspace through its own
// mount of the export, on any runtime: the agent's own directory for
// clone/empty-per-agent, the project checkout for shared workspaces. A
// missing workspace, or an export not mounted here, is ErrMoveWorkspaceMissing.
func TestCheckNFSMoveWorkspace(t *testing.T) {
	setup := func(t *testing.T) (*AgentManager, string, string) {
		t.Helper()
		f := newSharedDirStorageRunFixture(t)
		mountRoot := filepath.Join(t.TempDir(), "nfs")
		f.writeGlobalSettings(t, fmt.Sprintf(nfsWorkspaceStartYAML, mountRoot))
		m := NewManager(&runtime.MockRuntime{NameFunc: func() string { return "docker" }}).(*AgentManager)
		return m, f.projectScionDir, mountRoot
	}

	t.Run("export not mounted", func(t *testing.T) {
		m, projectPath, _ := setup(t)
		_, err := m.CheckNFSMoveWorkspace(projectPath, testNFSWorkspaceProjectID, "test-agent", MoveWorkspaceAgentDir)
		require.True(t, errors.Is(err, ErrMoveWorkspaceMissing), "err = %v", err)
	})
	t.Run("agent dir missing then present", func(t *testing.T) {
		m, projectPath, mountRoot := setup(t)
		require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, "share-1"), 0o755))
		_, err := m.CheckNFSMoveWorkspace(projectPath, testNFSWorkspaceProjectID, "test-agent", MoveWorkspaceAgentDir)
		require.True(t, errors.Is(err, ErrMoveWorkspaceMissing), "err = %v", err)
		assert.Contains(t, err.Error(), "does not exist")

		ws := filepath.Join(nfsTestAgentDir(mountRoot, "test-agent"), provision.AgentWorkspaceDir)
		require.NoError(t, os.MkdirAll(ws, 0o755))
		path, err := m.CheckNFSMoveWorkspace(projectPath, testNFSWorkspaceProjectID, "test-agent", MoveWorkspaceAgentDir)
		require.NoError(t, err)
		real, _ := filepath.EvalSymlinks(ws)
		assert.Equal(t, real, path)
	})
	t.Run("workspace is a regular file", func(t *testing.T) {
		m, projectPath, mountRoot := setup(t)
		ws := filepath.Join(nfsTestAgentDir(mountRoot, "test-agent"), provision.AgentWorkspaceDir)
		require.NoError(t, os.MkdirAll(filepath.Dir(ws), 0o755))
		require.NoError(t, os.WriteFile(ws, []byte("not a directory"), 0o644))
		_, err := m.CheckNFSMoveWorkspace(projectPath, testNFSWorkspaceProjectID, "test-agent", MoveWorkspaceAgentDir)
		require.True(t, errors.Is(err, ErrMoveWorkspaceMissing), "err = %v", err)
		assert.Contains(t, err.Error(), "is not a directory")
	})
	t.Run("project checkout", func(t *testing.T) {
		m, projectPath, mountRoot := setup(t)
		require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, "share-1"), 0o755))
		_, err := m.CheckNFSMoveWorkspace(projectPath, testNFSWorkspaceProjectID, "test-agent", MoveWorkspaceProject)
		require.True(t, errors.Is(err, ErrMoveWorkspaceMissing), "err = %v", err)
		require.NoError(t, os.MkdirAll(nfsTestWorkspaceDir(mountRoot), 0o755))
		_, err = m.CheckNFSMoveWorkspace(projectPath, testNFSWorkspaceProjectID, "test-agent", MoveWorkspaceProject)
		require.NoError(t, err)
	})
	t.Run("unknown kind", func(t *testing.T) {
		m, projectPath, mountRoot := setup(t)
		require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, "share-1"), 0o755))
		_, err := m.CheckNFSMoveWorkspace(projectPath, testNFSWorkspaceProjectID, "test-agent", "bogus")
		require.Error(t, err)
	})
}
