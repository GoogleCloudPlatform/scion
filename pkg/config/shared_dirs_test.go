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

package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnsureSharedDirs(t *testing.T) {
	tmpDir := t.TempDir()
	// Simulate a non-git grove external config path:
	// ~/.scion/project-configs/test__abc12345/.scion/
	projectDir := filepath.Join(tmpDir, "project-configs", "test__abc12345", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	dirs := []api.SharedDir{
		{Name: "build-cache"},
		{Name: "artifacts", ReadOnly: true},
		{Name: "workspace-cache", InWorkspace: true},
	}

	err := EnsureSharedDirs(projectDir, dirs)
	require.NoError(t, err)

	// Verify directories were created
	basePath := filepath.Join(tmpDir, "project-configs", "test__abc12345", SharedDirsSubdir)
	for _, d := range dirs {
		dirPath := filepath.Join(basePath, d.Name)
		info, err := os.Stat(dirPath)
		require.NoError(t, err, "shared dir %q should exist", d.Name)
		assert.True(t, info.IsDir(), "shared dir %q should be a directory", d.Name)
	}
}

func TestEnsureSharedDirs_Empty(t *testing.T) {
	err := EnsureSharedDirs("/nonexistent", nil)
	require.NoError(t, err)
}

func TestSharedDirsToVolumeMounts(t *testing.T) {
	tmpDir := t.TempDir()
	projectDir := filepath.Join(tmpDir, "project-configs", "test__abc12345", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	dirs := []api.SharedDir{
		{Name: "build-cache"},
		{Name: "artifacts", ReadOnly: true},
		{Name: "workspace-cache", InWorkspace: true},
	}

	mounts, err := SharedDirsToVolumeMounts(projectDir, dirs, "")
	require.NoError(t, err)
	require.Len(t, mounts, 3)

	// build-cache: default mount path, read-write
	assert.Contains(t, mounts[0].Source, "build-cache")
	assert.Equal(t, "/scion-volumes/build-cache", mounts[0].Target)
	assert.False(t, mounts[0].ReadOnly)

	// artifacts: default mount path, read-only
	assert.Contains(t, mounts[1].Source, "artifacts")
	assert.Equal(t, "/scion-volumes/artifacts", mounts[1].Target)
	assert.True(t, mounts[1].ReadOnly)

	// workspace-cache: in-workspace mount path (default /workspace)
	assert.Contains(t, mounts[2].Source, "workspace-cache")
	assert.Equal(t, "/workspace/.scion-volumes/workspace-cache", mounts[2].Target)
	assert.False(t, mounts[2].ReadOnly)
}

func TestSharedDirsToVolumeMounts_GitWorktreeWorkspace(t *testing.T) {
	tmpDir := t.TempDir()
	projectDir := filepath.Join(tmpDir, "project-configs", "test__abc12345", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	dirs := []api.SharedDir{
		{Name: "build-cache"},
		{Name: "workspace-cache", InWorkspace: true},
	}

	containerWorkspace := "/repo-root/.scion/agents/my-agent/workspace"
	mounts, err := SharedDirsToVolumeMounts(projectDir, dirs, containerWorkspace)
	require.NoError(t, err)
	require.Len(t, mounts, 2)

	// build-cache: default mount path (not affected by container workspace)
	assert.Equal(t, "/scion-volumes/build-cache", mounts[0].Target)

	// workspace-cache: uses the git worktree container workspace path
	assert.Equal(t, "/repo-root/.scion/agents/my-agent/workspace/.scion-volumes/workspace-cache", mounts[1].Target)
}

func TestSharedDirsToVolumeMounts_Empty(t *testing.T) {
	mounts, err := SharedDirsToVolumeMounts("/whatever", nil, "")
	require.NoError(t, err)
	assert.Nil(t, mounts)
}

func TestGetSharedDirInfos(t *testing.T) {
	tmpDir := t.TempDir()
	projectDir := filepath.Join(tmpDir, "project-configs", "test__abc12345", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	dirs := []api.SharedDir{
		{Name: "existing"},
		{Name: "missing"},
	}

	// Create only the first one
	basePath := filepath.Join(tmpDir, "project-configs", "test__abc12345", SharedDirsSubdir)
	require.NoError(t, os.MkdirAll(filepath.Join(basePath, "existing"), 0755))

	infos, err := GetSharedDirInfos(projectDir, dirs)
	require.NoError(t, err)
	require.Len(t, infos, 2)

	assert.Equal(t, "existing", infos[0].Name)
	assert.True(t, infos[0].Exists)

	assert.Equal(t, "missing", infos[1].Name)
	assert.False(t, infos[1].Exists)
}

func TestRemoveSharedDir(t *testing.T) {
	tmpDir := t.TempDir()
	projectDir := filepath.Join(tmpDir, "project-configs", "test__abc12345", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	dirs := []api.SharedDir{{Name: "to-remove"}}
	require.NoError(t, EnsureSharedDirs(projectDir, dirs))

	dirPath, err := GetSharedDirPath(projectDir, "to-remove")
	require.NoError(t, err)
	_, err = os.Stat(dirPath)
	require.NoError(t, err, "dir should exist before removal")

	err = RemoveSharedDir(projectDir, "to-remove")
	require.NoError(t, err)

	_, err = os.Stat(dirPath)
	assert.True(t, os.IsNotExist(err), "dir should not exist after removal")
}

func TestGetSharedDirsBasePath_GitProject(t *testing.T) {
	tmpDir := t.TempDir()

	// Simulate a git grove with split storage
	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	// Create the external agents dir structure
	groveConfigDir := filepath.Join(tmpDir, ".scion", "project-configs", "myproject__abc12345")
	agentsDir := filepath.Join(groveConfigDir, "agents")
	require.NoError(t, os.MkdirAll(agentsDir, 0755))

	// Create a git grove project dir with grove-id file
	projectDir := filepath.Join(tmpDir, "myproject", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	// Write grove-id
	projectID := "abc12345-test-id"
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "grove-id"), []byte(projectID+"\n"), 0644))

	basePath, err := GetSharedDirsBasePath(projectDir)
	require.NoError(t, err)
	assert.Contains(t, basePath, SharedDirsSubdir)
	assert.Contains(t, basePath, "project-configs")
}
