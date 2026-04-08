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

// GroveType indicates the kind of grove.
type GroveType string

const (
	GroveTypeGlobal   GroveType = "global"
	GroveTypeGit      GroveType = "git"
	GroveTypeExternal GroveType = "external"
)

// GroveStatus indicates the health of a grove config.
type GroveStatus string

const (
	GroveStatusOK       GroveStatus = "ok"
	GroveStatusOrphaned GroveStatus = "orphaned"
)

// GroveInfo describes a discovered grove.
type GroveInfo struct {
	Name          string      `json:"name"`
	GroveID       string      `json:"grove_id,omitempty"`
	Type          GroveType   `json:"type"`
	ConfigPath    string      `json:"config_path"`
	WorkspacePath string      `json:"workspace_path,omitempty"`
	Status        GroveStatus `json:"status"`
	AgentCount    int         `json:"agent_count"`
	// agentsPath overrides the default agents directory derivation.
	// Used for legacy git groves where agents are a sibling of .scion/.
	agentsPath string
}

// AgentsDir returns the path to the agents directory for this grove.
func (g GroveInfo) AgentsDir() string {
	if g.agentsPath != "" {
		return g.agentsPath
	}
	return filepath.Join(g.ConfigPath, "agents")
}

// DiscoverGroves scans for all known groves on this machine.
// It checks the global grove, then scans ~/.scion/grove-configs/ for
// external and git grove configs.
func DiscoverGroves() ([]GroveInfo, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}

	var groves []GroveInfo

	// 1. Global grove
	globalDir := filepath.Join(home, GlobalDir)
	if info, err := os.Stat(globalDir); err == nil && info.IsDir() {
		gi := GroveInfo{
			Name:       "global",
			Type:       GroveTypeGlobal,
			ConfigPath: globalDir,
			Status:     GroveStatusOK,
		}
		gi.AgentCount = countAgents(filepath.Join(globalDir, "agents"))
		if settings, err := LoadSettings(globalDir); err == nil {
			gi.GroveID = settings.GroveID
		}
		groves = append(groves, gi)
	}

	// 2. Scan grove-configs directory
	groveConfigsDir := filepath.Join(home, GlobalDir, "grove-configs")
	entries, err := os.ReadDir(groveConfigsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return groves, nil
		}
		return groves, err
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		dirName := entry.Name()
		slug := ExtractSlugFromExternalDir(dirName)
		if slug == "" {
			continue
		}

		configPath := filepath.Join(groveConfigsDir, dirName, DotScion)
		legacyAgentsSibling := filepath.Join(groveConfigsDir, dirName, "agents")

		_, scionErr := os.Stat(configPath)
		_, legacyAgentsErr := os.Stat(legacyAgentsSibling)
		scionExists := scionErr == nil
		legacyAgentsExist := legacyAgentsErr == nil

		var gi GroveInfo
		switch {
		case scionExists:
			// .scion/ exists — distinguish external vs git by checking for
			// a workspace_path in settings (external groves point back to
			// their original project directory).
			if settings, err := LoadSettings(configPath); err == nil && settings.WorkspacePath != "" {
				gi = groveInfoFromExternal(configPath, dirName, slug)
			} else {
				agentsDir := filepath.Join(configPath, "agents")
				gi = groveInfoFromGitExternalWithConfig(configPath, agentsDir, dirName, slug)
			}
		case legacyAgentsExist:
			// Legacy git grove: agents/ as sibling without .scion/ dir.
			gi = groveInfoFromGitExternal(legacyAgentsSibling, dirName, slug)
		default:
			// No .scion and no agents dir — orphaned leftover.
			gi = GroveInfo{
				Name:       slug,
				Type:       GroveTypeGit,
				ConfigPath: filepath.Join(groveConfigsDir, dirName),
				Status:     GroveStatusOrphaned,
			}
		}

		groves = append(groves, gi)
	}

	return groves, nil
}

// groveInfoFromExternal builds a GroveInfo for a non-git external grove.
func groveInfoFromExternal(configPath, dirName, slug string) GroveInfo {
	gi := GroveInfo{
		Name:       slug,
		Type:       GroveTypeExternal,
		ConfigPath: configPath,
		Status:     GroveStatusOK,
	}

	settings, err := LoadSettings(configPath)
	if err == nil {
		gi.GroveID = settings.GroveID
		gi.WorkspacePath = settings.WorkspacePath
	}

	gi.AgentCount = countAgents(filepath.Join(configPath, "agents"))

	// Check if workspace still exists and has a valid marker pointing back here
	if gi.WorkspacePath != "" {
		if !isValidWorkspace(gi.WorkspacePath, gi.GroveID, configPath) {
			gi.Status = GroveStatusOrphaned
		}
	} else {
		// No workspace path recorded — orphaned
		gi.Status = GroveStatusOrphaned
	}

	return gi
}

// groveInfoFromGitExternalWithConfig builds a GroveInfo for a git grove that has
// an external config dir (.scion/) with agents stored at .scion/agents/.
// This is the layout produced by initInRepoGrove after the config externalization change.
func groveInfoFromGitExternalWithConfig(configPath, agentsDir, dirName, slug string) GroveInfo {
	gi := GroveInfo{
		Name:       slug,
		Type:       GroveTypeGit,
		ConfigPath: configPath,
		Status:     GroveStatusOK,
	}
	gi.AgentCount = countAgents(agentsDir)
	if settings, err := LoadSettings(configPath); err == nil {
		gi.GroveID = settings.GroveID
	}
	if gi.GroveID == "" {
		if marker, workspacePath, err := readWorkspaceMarkerForSlug(slug); err == nil {
			gi.GroveID = marker.GroveID
			gi.WorkspacePath = workspacePath
		}
	}
	if gi.AgentCount == 0 {
		gi.Status = GroveStatusOrphaned
	}
	return gi
}

func readWorkspaceMarkerForSlug(slug string) (*GroveMarker, string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, "", err
	}
	workspacePath := filepath.Join(home, GlobalDir, "groves", slug)
	markerPath := filepath.Join(workspacePath, DotScion)
	marker, err := ReadGroveMarker(markerPath)
	if err != nil {
		return nil, "", err
	}
	return marker, workspacePath, nil
}

// groveInfoFromGitExternal builds a GroveInfo for a legacy git grove's external agents
// directory (no .scion/ subdir). If the agents directory is empty, the grove is marked
// as orphaned since there is no config to link back to the source project.
func groveInfoFromGitExternal(agentsDir, dirName, slug string) GroveInfo {
	gi := GroveInfo{
		Name:       slug,
		Type:       GroveTypeGit,
		ConfigPath: filepath.Join(filepath.Dir(agentsDir), DotScion),
		agentsPath: agentsDir, // legacy: agents as sibling of .scion/
		Status:     GroveStatusOK,
	}

	gi.AgentCount = countAgents(agentsDir)

	if gi.AgentCount == 0 {
		// No agents and no .scion directory — this is an orphaned leftover
		// (e.g. from a deleted workspace or test run).
		gi.Status = GroveStatusOrphaned
	} else {
		// Has agents but no .scion — can't determine workspace path.
		gi.WorkspacePath = "(git repo)"
	}

	return gi
}

// isValidWorkspace checks if a workspace path exists and has a valid .scion
// marker or directory pointing back to the expected grove config.
// For external (non-git) groves, configPath is the expected grove-config path;
// the workspace marker must resolve to the same path.
func isValidWorkspace(workspacePath, expectedGroveID string, configPath ...string) bool {
	markerPath := filepath.Join(workspacePath, DotScion)
	info, err := os.Stat(markerPath)
	if err != nil {
		return false
	}

	if info.IsDir() {
		// Git grove — check grove-id file
		if expectedGroveID != "" {
			if id, err := ReadGroveID(markerPath); err == nil {
				return id == expectedGroveID
			}
		}
		return true
	}

	// Non-git grove — read marker and verify it resolves to the expected config path
	marker, err := ReadGroveMarker(markerPath)
	if err != nil {
		return false
	}

	// If a config path was provided, check that the marker resolves to it.
	// This catches the case where a marker was deleted and re-created with a
	// new grove-id, leaving the old grove-config orphaned.
	if len(configPath) > 0 && configPath[0] != "" {
		resolved, err := marker.ExternalGrovePath()
		if err != nil {
			return false
		}
		return filepath.Clean(resolved) == filepath.Clean(configPath[0])
	}

	if expectedGroveID != "" {
		return marker.GroveID == expectedGroveID
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

// FindOrphanedGroveConfigs returns grove configs that are orphaned
// (their workspace no longer exists or no longer points back to them).
func FindOrphanedGroveConfigs() ([]GroveInfo, error) {
	groves, err := DiscoverGroves()
	if err != nil {
		return nil, err
	}

	var orphaned []GroveInfo
	for _, g := range groves {
		if g.Status == GroveStatusOrphaned {
			orphaned = append(orphaned, g)
		}
	}
	return orphaned, nil
}

// RemoveGroveConfig removes an external grove config directory.
func RemoveGroveConfig(configPath string) error {
	// The configPath points to the .scion subdirectory or the grove-configs/<slug__uuid> directory.
	// We want to remove the grove-configs/<slug__uuid> directory.
	parent := configPath
	if filepath.Base(parent) == DotScion {
		parent = filepath.Dir(parent)
	}

	// Safety: only remove if it's under grove-configs/
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	groveConfigsDir := filepath.Join(home, GlobalDir, "grove-configs")
	if !strings.HasPrefix(parent, groveConfigsDir) {
		return os.ErrPermission
	}

	return os.RemoveAll(parent)
}

// ReconnectGrove updates the workspace_path in an external grove's settings
// to point to a new workspace location. It also updates the marker file
// at the new workspace path.
func ReconnectGrove(configPath, newWorkspacePath string) error {
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
