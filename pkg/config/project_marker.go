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
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"gopkg.in/yaml.v3"
)

// ProjectMarker represents the content of a .scion marker file.
// When .scion is a file (not a directory), it points to an external
// grove-config directory under ~/.scion/project-configs/.
type ProjectMarker struct {
	ProjectID   string `yaml:"grove-id"`
	ProjectName string `yaml:"grove-name"`
	ProjectSlug string `yaml:"grove-slug"`
}

// ShortUUID returns a short form of the grove ID for use in directory names.
func (m ProjectMarker) ShortUUID() string {
	id := strings.ReplaceAll(m.ProjectID, "-", "")
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// DirName returns the directory name used under ~/.scion/project-configs/.
func (m ProjectMarker) DirName() string {
	return fmt.Sprintf("%s__%s", m.ProjectSlug, m.ShortUUID())
}

// ExternalProjectPath returns the absolute path to the external grove config
// directory: ~/.scion/project-configs/<grove-slug>__<short-uuid>/.scion/
func (m ProjectMarker) ExternalProjectPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, GlobalDir, "project-configs", m.DirName(), DotScion), nil
}

// ReadProjectMarker reads and parses a .scion marker file.
func ReadProjectMarker(path string) (*ProjectMarker, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var marker ProjectMarker
	if err := yaml.Unmarshal(data, &marker); err != nil {
		return nil, fmt.Errorf("invalid grove marker at %s: %w", path, err)
	}
	if marker.ProjectID == "" || marker.ProjectSlug == "" {
		return nil, fmt.Errorf("invalid grove marker at %s: missing grove-id or grove-slug", path)
	}
	return &marker, nil
}

// WriteProjectMarker writes a ProjectMarker to the given path as a YAML file.
func WriteProjectMarker(path string, marker *ProjectMarker) error {
	data, err := yaml.Marshal(marker)
	if err != nil {
		return fmt.Errorf("failed to marshal grove marker: %w", err)
	}
	return os.WriteFile(path, data, 0644)
}

// ResolveProjectMarker reads a .scion marker file and returns the resolved
// external grove path. Returns an error if the marker is invalid or the
// external path cannot be computed.
func ResolveProjectMarker(markerPath string) (string, error) {
	marker, err := ReadProjectMarker(markerPath)
	if err != nil {
		return "", err
	}
	return marker.ExternalProjectPath()
}

// IsProjectMarkerFile returns true if the given path is a regular file
// (not a directory) that could be a grove marker. Does not validate content.
func IsProjectMarkerFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.IsDir()
}

// IsOldStyleNonGitProject returns true if the path is a .scion directory
// in a non-git project (not the global ~/.scion/). This indicates an
// old-format grove that needs to be re-initialized.
func IsOldStyleNonGitProject(scionPath string) bool {
	info, err := os.Stat(scionPath)
	if err != nil || !info.IsDir() {
		return false
	}

	// Don't flag the global grove
	home, err := os.UserHomeDir()
	if err == nil {
		globalDir := filepath.Join(home, GlobalDir)
		if abs, err := filepath.Abs(scionPath); err == nil {
			evalAbs, _ := filepath.EvalSymlinks(abs)
			evalGlobal, _ := filepath.EvalSymlinks(globalDir)
			if evalAbs == evalGlobal {
				return false
			}
		}
	}

	// Check if the parent directory is a git repo
	parent := filepath.Dir(scionPath)
	gitDir := filepath.Join(parent, ".git")
	if _, err := os.Stat(gitDir); err == nil {
		return false // Git grove — not old-style (handled by Phase 3)
	}

	return true
}

// IsHubContext returns true if hub context environment variables are available,
// indicating the CLI is running inside a hub-connected agent container where
// grove data should be accessed via the Hub API rather than the local filesystem.
// Checks SCION_HUB_ENDPOINT (primary), SCION_HUB_URL (legacy), and
// SCION_GROVE_ID (always set for broker-dispatched agents).
func IsHubContext() bool {
	return os.Getenv("SCION_HUB_ENDPOINT") != "" ||
		os.Getenv("SCION_HUB_URL") != "" ||
		os.Getenv("SCION_GROVE_ID") != ""
}

// WriteWorkspaceMarker writes a minimal .scion marker file into a workspace
// directory so that in-container CLI can discover the grove context.
// This is called during agent provisioning for git groves (where the worktree
// doesn't contain .scion because it's gitignored) and for hub-native groves.
func WriteWorkspaceMarker(workspacePath string, groveID, groveName, groveSlug string) error {
	if groveID == "" || groveSlug == "" {
		return fmt.Errorf("grove-id and grove-slug are required for workspace marker")
	}
	marker := &ProjectMarker{
		ProjectID:   groveID,
		ProjectName: groveName,
		ProjectSlug: groveSlug,
	}
	return WriteProjectMarker(filepath.Join(workspacePath, DotScion), marker)
}

// ExtractSlugFromExternalDir extracts the grove slug from an external
// grove-config directory name in the format "slug__shortuuid".
func ExtractSlugFromExternalDir(dirName string) string {
	if parts := strings.SplitN(dirName, "__", 2); len(parts) == 2 {
		return parts[0]
	}
	return ""
}

// ReadProjectID reads the grove-id file from a git grove's .scion directory.
func ReadProjectID(projectDir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(projectDir, "grove-id"))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// WriteProjectID writes a grove-id file to a git grove's .scion directory.
func WriteProjectID(projectDir string, groveID string) error {
	return os.WriteFile(filepath.Join(projectDir, "grove-id"), []byte(groveID+"\n"), 0644)
}

// GetGitProjectExternalConfigDir returns the external config directory for a git grove.
// Git groves store settings and templates externally at ~/.scion/project-configs/<slug>__<uuid>/.scion/
// while keeping worktrees in-repo.
// Returns ("", nil) if the grove-id file does not exist (not yet initialized for split storage).
func GetGitProjectExternalConfigDir(projectDir string) (string, error) {
	groveID, err := ReadProjectID(projectDir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}

	groveName := GetProjectName(projectDir)
	groveSlug := api.Slugify(groveName)
	marker := &ProjectMarker{
		ProjectID:   groveID,
		ProjectName: groveName,
		ProjectSlug: groveSlug,
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, GlobalDir, "project-configs", marker.DirName(), DotScion), nil
}

// GetGitProjectExternalAgentsDir returns the external agents directory for a git grove.
// Git groves store agent homes externally at ~/.scion/project-configs/<slug>__<uuid>/.scion/agents/
// while keeping worktrees in-repo.
// Returns ("", nil) if the grove-id file does not exist (not yet initialized for split storage).
func GetGitProjectExternalAgentsDir(projectDir string) (string, error) {
	groveID, err := ReadProjectID(projectDir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}

	groveName := GetProjectName(projectDir)
	groveSlug := api.Slugify(groveName)
	marker := &ProjectMarker{
		ProjectID:   groveID,
		ProjectName: groveName,
		ProjectSlug: groveSlug,
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, GlobalDir, "project-configs", marker.DirName(), DotScion, "agents"), nil
}

// GetAgentHomePath returns the correct home directory path for an agent.
// For git groves with split storage (grove-id file exists), this returns
// the external path under ~/.scion/project-configs/.
// For non-git groves (projectDir already resolved to external via marker),
// or git groves without split storage, returns the in-repo path.
func GetAgentHomePath(projectDir, agentName string) string {
	if externalDir, err := GetGitProjectExternalAgentsDir(projectDir); err == nil && externalDir != "" {
		return filepath.Join(externalDir, agentName, "home")
	}
	return filepath.Join(projectDir, "agents", agentName, "home")
}

// GetAgentDir returns the broker-side directory for an agent's per-agent state
// files (prompt.md, scion-agent.json, and — in worktree mode — the workspace
// subdir).
//
// For shared-workspace git groves (sharedWorkspace == true and a grove-id
// marker exists), this returns the external path under
// ~/.scion/project-configs/<slug>__<uuid>/.scion/agents/<name>/ so that sibling
// agents do not see each other's state via the shared /workspace mount. See
// .design/hub-shared-workspace-isolation.md for the threat model.
//
// For all other modes (worktree mode, non-git groves, or shared-workspace
// groves without an initialized grove-id), this returns the in-grove path
// <projectDir>/agents/<name>/ — preserving the worktree-relative layout that
// git's worktree pointers depend on.
func GetAgentDir(projectDir, agentName string, sharedWorkspace bool) string {
	if sharedWorkspace {
		if externalDir, err := GetGitProjectExternalAgentsDir(projectDir); err == nil && externalDir != "" {
			return filepath.Join(externalDir, agentName)
		}
	}
	return filepath.Join(projectDir, "agents", agentName)
}

// ResolveAgentDir returns the broker-side per-agent state directory when the
// shared-workspace mode is not known to the caller. It probes the external
// path first (used by shared-workspace groves), then falls back to the
// in-grove path. A read-time companion to GetAgentDir, used by code paths
// that look up an existing agent by name without carrying the
// sharedWorkspace flag through the call stack.
//
// Returns the external path only when a grove-id marker exists *and* the
// external per-agent directory contains scion-agent.json (which never lives
// external in worktree mode — only home/ does). Otherwise returns the
// in-grove path.
func ResolveAgentDir(projectDir, agentName string) string {
	if externalDir, err := GetGitProjectExternalAgentsDir(projectDir); err == nil && externalDir != "" {
		ext := filepath.Join(externalDir, agentName)
		if _, err := os.Stat(filepath.Join(ext, "scion-agent.json")); err == nil {
			return ext
		}
	}
	return filepath.Join(projectDir, "agents", agentName)
}
