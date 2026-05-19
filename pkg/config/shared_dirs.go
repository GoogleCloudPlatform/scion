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
	"fmt"
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// SharedDirsSubdir is the subdirectory name under project-configs for shared directories.
const SharedDirsSubdir = "shared-dirs"

// GetSharedDirsBasePath returns the host-side base directory for shared dirs
// for the given project. For non-git projects (where projectDir is already the
// external project-config path), this is <projectDir>/../shared-dirs/.
// For git projects with split storage, this is
// ~/.scion/project-configs/<slug>__<uuid>/shared-dirs/.
func GetSharedDirsBasePath(projectDir string) (string, error) {
	// Check if this is a git project with split storage (has project-id file)
	if externalAgentsDir, err := GetGitProjectExternalAgentsDir(projectDir); err == nil && externalAgentsDir != "" {
		// externalAgentsDir is ~/.scion/project-configs/<slug>__<uuid>/.scion/agents
		// We want ~/.scion/project-configs/<slug>__<uuid>/shared-dirs
		// Go up past "agents" and ".scion" to reach the project-config root
		projectConfigRoot := filepath.Dir(filepath.Dir(externalAgentsDir))
		return filepath.Join(projectConfigRoot, SharedDirsSubdir), nil
	}

	// For non-git projects, projectDir is already resolved to
	// ~/.scion/project-configs/<slug>__<uuid>/.scion/
	// Go up one level to get the project-config root, then into shared-dirs
	parent := filepath.Dir(projectDir)
	// Verify we're in a project-configs or grove-configs directory structure
	parentBase := filepath.Base(filepath.Dir(parent))
	if parentBase == ProjectConfigsDir || parentBase == GroveConfigsDir || filepath.Base(parent) != DotScion {
		return filepath.Join(parent, SharedDirsSubdir), nil
	}

	return filepath.Join(parent, SharedDirsSubdir), nil
}

// GetSharedDirPath returns the host-side directory path for a specific shared dir.
func GetSharedDirPath(projectDir, name string) (string, error) {
	basePath, err := GetSharedDirsBasePath(projectDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(basePath, name), nil
}

// EnsureSharedDirs creates the host-side directories for all declared shared dirs.
func EnsureSharedDirs(projectDir string, dirs []api.SharedDir) error {
	if len(dirs) == 0 {
		return nil
	}

	basePath, err := GetSharedDirsBasePath(projectDir)
	if err != nil {
		return fmt.Errorf("failed to resolve shared dirs base path: %w", err)
	}

	for _, d := range dirs {
		dirPath := filepath.Join(basePath, d.Name)
		if err := os.MkdirAll(dirPath, 0755); err != nil {
			return fmt.Errorf("failed to create shared dir %q at %s: %w", d.Name, dirPath, err)
		}
	}
	return nil
}

// SharedDirsToVolumeMounts converts shared dir declarations into VolumeMount
// entries suitable for injection into a RunConfig. Each shared dir becomes a
// bind mount at either /scion-volumes/<name> or <containerWorkspace>/.scion-volumes/<name>.
// The containerWorkspace parameter specifies the container-side workspace path
// (e.g., /workspace or /repo-root/.scion/agents/foo/workspace for git worktrees).
func SharedDirsToVolumeMounts(projectDir string, dirs []api.SharedDir, containerWorkspace string) ([]api.VolumeMount, error) {
	if len(dirs) == 0 {
		return nil, nil
	}

	if containerWorkspace == "" {
		containerWorkspace = "/workspace"
	}

	var mounts []api.VolumeMount
	for _, d := range dirs {
		hostPath, err := GetSharedDirPath(projectDir, d.Name)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve path for shared dir %q: %w", d.Name, err)
		}

		target := fmt.Sprintf("/scion-volumes/%s", d.Name)
		if d.InWorkspace {
			target = fmt.Sprintf("%s/.scion-volumes/%s", containerWorkspace, d.Name)
		}

		mounts = append(mounts, api.VolumeMount{
			Source:   hostPath,
			Target:   target,
			ReadOnly: d.ReadOnly,
		})
	}
	return mounts, nil
}

// RemoveSharedDir removes the host-side directory for a shared dir.
func RemoveSharedDir(projectDir, name string) error {
	dirPath, err := GetSharedDirPath(projectDir, name)
	if err != nil {
		return err
	}
	return os.RemoveAll(dirPath)
}

// ListSharedDirInfo returns information about each shared dir on disk.
type SharedDirInfo struct {
	Name        string `json:"name"`
	ReadOnly    bool   `json:"read_only"`
	InWorkspace bool   `json:"in_workspace"`
	HostPath    string `json:"host_path"`
	Exists      bool   `json:"exists"`
}

// GetSharedDirInfos returns info for each declared shared dir.
func GetSharedDirInfos(projectDir string, dirs []api.SharedDir) ([]SharedDirInfo, error) {
	var infos []SharedDirInfo
	for _, d := range dirs {
		hostPath, err := GetSharedDirPath(projectDir, d.Name)
		if err != nil {
			return nil, err
		}
		exists := false
		if _, statErr := os.Stat(hostPath); statErr == nil {
			exists = true
		}
		infos = append(infos, SharedDirInfo{
			Name:        d.Name,
			ReadOnly:    d.ReadOnly,
			InWorkspace: d.InWorkspace,
			HostPath:    hostPath,
			Exists:      exists,
		})
	}
	return infos, nil
}
