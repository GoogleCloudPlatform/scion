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
	"strings"
)

// ProjectType indicates the kind of project.
type ProjectType string

const (
	ProjectTypeGlobal   ProjectType = "global"
	ProjectTypeGit      ProjectType = "git"
	ProjectTypeExternal ProjectType = "external"
)

// ProjectStatus indicates the health of a project config.
type ProjectStatus string

const (
	ProjectStatusOK       ProjectStatus = "ok"
	ProjectStatusOrphaned ProjectStatus = "orphaned"
)

// ProjectInfo describes a discovered project.
type ProjectInfo struct {
	Name          string        `json:"name"`
	ProjectID     string        `json:"project_id,omitempty"`
	GroveID       string        `json:"grove_id,omitempty"`
	Type          ProjectType   `json:"type"`
	ConfigPath    string        `json:"config_path"`
	WorkspacePath string        `json:"workspace_path,omitempty"`
	Status        ProjectStatus `json:"status"`
	AgentCount    int           `json:"agent_count"`
	// agentsPath overrides the default agents directory derivation.
	// Used for legacy git projects where agents are a sibling of .scion/.
	agentsPath string
}

// AgentsDir returns the path to the agents directory for this project.
func (g ProjectInfo) AgentsDir() string {
	if g.agentsPath != "" {
		return g.agentsPath
	}
	return filepath.Join(g.ConfigPath, "agents")
}

// DiscoverProjects scans for all known projects on this machine.
// It checks the global project, then scans ~/.scion/project-configs/ and
// the legacy ~/.scion/grove-configs/ for external and git project configs.
func DiscoverProjects() ([]ProjectInfo, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}

	var projects []ProjectInfo
	seenSlugs := make(map[string]bool)

	// 1. Global project
	globalDir := filepath.Join(home, GlobalDir)
	if info, err := os.Stat(globalDir); err == nil && info.IsDir() {
		pi := ProjectInfo{
			Name:       "global",
			Type:       ProjectTypeGlobal,
			ConfigPath: globalDir,
			Status:     ProjectStatusOK,
		}
		pi.AgentCount = countAgents(filepath.Join(globalDir, "agents"))
		if settings, err := LoadSettings(globalDir); err == nil {
			pi.ProjectID = settings.ProjectID
			pi.GroveID = settings.ProjectID
		}
		projects = append(projects, pi)
		seenSlugs["global"] = true
	}

	// 2. Scan project-configs directory (preferred)
	projectConfigsDir := filepath.Join(home, GlobalDir, ProjectConfigsDir)
	projects = scanConfigDir(projects, projectConfigsDir, seenSlugs)

	// 3. Scan legacy grove-configs directory
	groveConfigsDir := filepath.Join(home, GlobalDir, GroveConfigsDir)
	projects = scanConfigDir(projects, groveConfigsDir, seenSlugs)

	return projects, nil
}

// scanConfigDir is a helper for DiscoverProjects to scan a directory for project configs.
func scanConfigDir(projects []ProjectInfo, configDir string, seenSlugs map[string]bool) []ProjectInfo {
	entries, err := os.ReadDir(configDir)
	if err != nil {
		return projects
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		dirName := entry.Name()
		slug := ExtractSlugFromExternalDir(dirName)
		if slug == "" || seenSlugs[slug] {
			continue
		}

		configPath := filepath.Join(configDir, dirName, DotScion)
		legacyAgentsSibling := filepath.Join(configDir, dirName, "agents")

		_, scionErr := os.Stat(configPath)
		_, legacyAgentsErr := os.Stat(legacyAgentsSibling)
		scionExists := scionErr == nil
		legacyAgentsExist := legacyAgentsErr == nil

		var pi ProjectInfo
		switch {
		case scionExists:
			// .scion/ exists — distinguish external vs git by checking for
			// a workspace_path in settings (external projects point back to
			// their original project directory).
			if settings, err := LoadSettings(configPath); err == nil && settings.WorkspacePath != "" {
				pi = projectInfoFromExternal(configPath, dirName, slug)
			} else {
				agentsDir := filepath.Join(configPath, "agents")
				pi = projectInfoFromGitExternalWithConfig(configPath, agentsDir, dirName, slug)
			}
		case legacyAgentsExist:
			// Legacy git project: agents/ as sibling without .scion/ dir.
			pi = projectInfoFromGitExternal(legacyAgentsSibling, dirName, slug)
		default:
			// No .scion and no agents dir — orphaned leftover.
			pi = ProjectInfo{
				Name:       slug,
				Type:       ProjectTypeGit,
				ConfigPath: filepath.Join(configDir, dirName),
				Status:     ProjectStatusOrphaned,
			}
		}

		projects = append(projects, pi)
		seenSlugs[slug] = true
	}

	return projects
}

// projectInfoFromExternal builds a ProjectInfo for a non-git external project.
func projectInfoFromExternal(configPath, dirName, slug string) ProjectInfo {
	pi := ProjectInfo{
		Name:       slug,
		Type:       ProjectTypeExternal,
		ConfigPath: configPath,
		Status:     ProjectStatusOK,
	}

	settings, err := LoadSettings(configPath)
	if err == nil {
		pi.ProjectID = settings.ProjectID
		pi.GroveID = settings.ProjectID
		pi.WorkspacePath = settings.WorkspacePath
	}

	pi.AgentCount = countAgents(filepath.Join(configPath, "agents"))

	// Check if workspace still exists and has a valid marker pointing back here
	if pi.WorkspacePath != "" {
		if !isValidWorkspace(pi.WorkspacePath, pi.ProjectID, configPath) {
			pi.Status = ProjectStatusOrphaned
		}
	} else {
		// No workspace path recorded — orphaned
		pi.Status = ProjectStatusOrphaned
	}

	return pi
}

// projectInfoFromGitExternalWithConfig builds a ProjectInfo for a git project that has
// an external config dir (.scion/) with agents stored at .scion/agents/.
// This is the layout produced by initInRepoProject after the config externalization change.
func projectInfoFromGitExternalWithConfig(configPath, agentsDir, dirName, slug string) ProjectInfo {
	pi := ProjectInfo{
		Name:       slug,
		Type:       ProjectTypeGit,
		ConfigPath: configPath,
		Status:     ProjectStatusOK,
	}
	pi.AgentCount = countAgents(agentsDir)
	if settings, err := LoadSettings(configPath); err == nil {
		pi.ProjectID = settings.ProjectID
		pi.GroveID = settings.ProjectID
	}
	if pi.ProjectID == "" {
		if marker, workspacePath, err := readWorkspaceMarkerForSlug(slug); err == nil {
			pi.ProjectID = marker.ProjectID
			pi.WorkspacePath = workspacePath
		}
	}
	if pi.AgentCount == 0 {
		pi.Status = ProjectStatusOrphaned
	}
	return pi
}

func readWorkspaceMarkerForSlug(slug string) (*ProjectMarker, string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, "", err
	}

	// 1. Try projects/
	workspacePath := filepath.Join(home, GlobalDir, ProjectsDir, slug)
	markerPath := filepath.Join(workspacePath, DotScion)
	if marker, err := ReadProjectMarker(markerPath); err == nil {
		return marker, workspacePath, nil
	}

	// 2. Fallback to legacy groves/
	workspacePath = filepath.Join(home, GlobalDir, GrovesDir, slug)
	markerPath = filepath.Join(workspacePath, DotScion)
	if marker, err := ReadProjectMarker(markerPath); err == nil {
		return marker, workspacePath, nil
	}

	return nil, "", os.ErrNotExist
}

// projectInfoFromGitExternal builds a ProjectInfo for a legacy git project's external agents
// directory (no .scion/ subdir). If the agents directory is empty, the project is marked
// as orphaned since there is no config to link back to the source project.
func projectInfoFromGitExternal(agentsDir, dirName, slug string) ProjectInfo {
	pi := ProjectInfo{
		Name:       slug,
		Type:       ProjectTypeGit,
		ConfigPath: filepath.Join(filepath.Dir(agentsDir), DotScion),
		agentsPath: agentsDir, // legacy: agents as sibling of .scion/
		Status:     ProjectStatusOK,
	}

	pi.AgentCount = countAgents(agentsDir)

	if pi.AgentCount == 0 {
		// No agents and no .scion directory — this is an orphaned leftover
		// (e.g. from a deleted workspace or test run).
		pi.Status = ProjectStatusOrphaned
	} else {
		// Has agents but no .scion — can't determine workspace path.
		pi.WorkspacePath = "(git repo)"
	}

	return pi
}

// isValidWorkspace checks if a workspace path exists and has a valid .scion
// marker or directory pointing back to the expected project config.
// For external (non-git) projects, configPath is the expected project-config path;
// the workspace marker must resolve to the same path.
func isValidWorkspace(workspacePath, expectedProjectID string, configPath ...string) bool {
	markerPath := filepath.Join(workspacePath, DotScion)
	info, err := os.Stat(markerPath)
	if err != nil {
		return false
	}

	if info.IsDir() {
		// Git project — check project-id file
		if expectedProjectID != "" {
			if id, err := ReadProjectID(markerPath); err == nil {
				return id == expectedProjectID
			}
		}
		return true
	}

	// Non-git project — read marker and verify it resolves to the expected config path
	marker, err := ReadProjectMarker(markerPath)
	if err != nil {
		return false
	}

	// If a config path was provided, check that the marker resolves to it.
	// This catches the case where a marker was deleted and re-created with a
	// new project-id, leaving the old project-config orphaned.
	if len(configPath) > 0 && configPath[0] != "" {
		resolved, err := marker.ExternalProjectPath()
		if err != nil {
			return false
		}
		return filepath.Clean(resolved) == filepath.Clean(configPath[0])
	}

	if expectedProjectID != "" {
		return marker.ProjectID == expectedProjectID
	}
	return true
}

// countAgents counts agent subdirectories in an agents directory.
func countAgents(agentsDir string) int {
	return len(ListAgentNames(agentsDir))
}

// ListAgentNames returns the names of agent subdirectories in an agents directory.
func ListAgentNames(agentsDir string) []string {
	entries, err := os.ReadDir(agentsDir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			names = append(names, e.Name())
		}
	}
	return names
}

// FindOrphanedProjectConfigs returns project configs that are orphaned
// (their workspace no longer exists or no longer points back to them).
func FindOrphanedProjectConfigs() ([]ProjectInfo, error) {
	projects, err := DiscoverProjects()
	if err != nil {
		return nil, err
	}

	var orphaned []ProjectInfo
	for _, g := range projects {
		if g.Status == ProjectStatusOrphaned {
			orphaned = append(orphaned, g)
		}
	}
	return orphaned, nil
}

// RemoveProjectConfig removes an external project config directory.
func RemoveProjectConfig(configPath string) error {
	// The configPath points to the .scion subdirectory or the project-configs/<slug__uuid> directory.
	// We want to remove the project-configs/<slug__uuid> directory.
	parent := configPath
	if filepath.Base(parent) == DotScion {
		parent = filepath.Dir(parent)
	}

	// Safety: only remove if it's under project-configs/ or legacy grove-configs/
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	projectConfigsDir := filepath.Join(home, GlobalDir, ProjectConfigsDir)
	groveConfigsDir := filepath.Join(home, GlobalDir, GroveConfigsDir)

	if !strings.HasPrefix(parent, projectConfigsDir) && !strings.HasPrefix(parent, groveConfigsDir) {
		return os.ErrPermission
	}

	return os.RemoveAll(parent)
}

// ReconnectProject updates the workspace_path in an external project's settings
// to point to a new workspace location. It also updates the marker file
// at the new workspace path.
func ReconnectProject(configPath, newWorkspacePath string) error {
	absWorkspace, err := filepath.Abs(newWorkspacePath)
	if err != nil {
		return err
	}

	// Update settings.yaml
	if err := UpdateSetting(configPath, "workspace_path", absWorkspace, false); err != nil {
		return err
	}

	return nil
}
