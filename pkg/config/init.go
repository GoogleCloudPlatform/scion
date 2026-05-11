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
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

// isGitRepoFunc is a package-level variable for testing. Defaults to util.IsGitRepo.
var isGitRepoFunc = util.IsGitRepo

// OverrideIsGitRepo replaces the git-repo detection function used by InitProject.
// It returns a restore function that must be called to reset the override.
// This is intended for use in tests outside the config package.
func OverrideIsGitRepo(fn func() bool) func() {
	orig := isGitRepoFunc
	isGitRepoFunc = fn
	return func() { isGitRepoFunc = orig }
}

//go:embed all:embeds/*
var EmbedsFS embed.FS

// getDefaultSettingsDataForRuntime generates default settings JSON with the
// specified runtime for the local profile. Handles both versioned and legacy formats.
func getDefaultSettingsDataForRuntime(targetRuntime string) ([]byte, error) {
	data, err := EmbedsFS.ReadFile("embeds/default_settings.yaml")
	if err != nil {
		return nil, err
	}

	version, _ := DetectSettingsFormat(data)
	if version != "" {
		var vs VersionedSettings
		if err := yaml.Unmarshal(data, &vs); err != nil {
			return nil, err
		}
		if local, ok := vs.Profiles["local"]; ok {
			local.Runtime = targetRuntime
			vs.Profiles["local"] = local
		}
		legacy := convertVersionedToLegacy(&vs)
		return json.MarshalIndent(legacy, "", "  ")
	}

	var settings Settings
	if err := yaml.Unmarshal(data, &settings); err != nil {
		return nil, err
	}
	if local, ok := settings.Profiles["local"]; ok {
		local.Runtime = targetRuntime
		settings.Profiles["local"] = local
	}
	return json.MarshalIndent(settings, "", "  ")
}

// GetDefaultSettingsData returns the embedded default settings in JSON format.
// This function adjusts the local profile runtime based on the OS. It is used as
// a fallback default for settings loaders; during init, DetectLocalRuntime is used
// instead for actual runtime probing.
func GetDefaultSettingsData() ([]byte, error) {
	targetRuntime := "docker"
	if runtime.GOOS == "darwin" {
		targetRuntime = "container"
	}
	return getDefaultSettingsDataForRuntime(targetRuntime)
}

// SeedFileFromFS writes a file from an embed.FS to a target path.
// If force is true, the file is always overwritten. Otherwise, it is only
// written if it does not already exist. alwaysOverwrite can be set to true
// for critical config files that should always match embedded defaults.
func SeedFileFromFS(fs embed.FS, basePath, fileName, targetPath string, force, alwaysOverwrite bool) error {
	data, err := fs.ReadFile(filepath.Join(basePath, fileName))
	if err != nil {
		return nil // File not in embeds, skip silently
	}

	if force || alwaysOverwrite {
		if err := os.WriteFile(targetPath, data, 0644); err != nil {
			return fmt.Errorf("failed to write file %s: %w", targetPath, err)
		}
		return nil
	}

	if _, err := os.Stat(targetPath); os.IsNotExist(err) {
		if err := os.WriteFile(targetPath, data, 0644); err != nil {
			return fmt.Errorf("failed to write file %s: %w", targetPath, err)
		}
	}
	return nil
}

// GenerateProjectID creates a new random project ID.
func GenerateProjectID() string {
	return uuid.New().String()
}

// GenerateProjectIDForDir creates a new random project ID.
// The dir parameter is accepted for API compatibility but does not affect
// the generated ID.
func GenerateProjectIDForDir(_ string) string {
	return uuid.New().String()
}

// IsInsideProject returns true if the current working directory or any parent contains a .scion directory.
func IsInsideProject() bool {
	_, ok := FindProjectRoot()
	return ok
}

// GetEnclosingProjectPath returns the path to the enclosing .scion directory if one exists,
// along with the root directory containing it.
func GetEnclosingProjectPath() (projectPath string, rootDir string, found bool) {
	wd, err := os.Getwd()
	if err != nil {
		return "", "", false
	}

	dir := wd
	for {
		p := filepath.Join(dir, DotScion)
		info, err := os.Stat(p)
		if err == nil {
			if info.IsDir() {
				if abs, err := filepath.EvalSymlinks(p); err == nil {
					return abs, dir, true
				}
				return p, dir, true
			}
			// .scion is a marker file — resolve to external path
			if resolved, err := ResolveProjectMarker(p); err == nil {
				return resolved, dir, true
			}
		}

		parent := filepath.Dir(dir)
		if parent == dir { // Reached filesystem root
			break
		}
		dir = parent
	}
	return "", "", false
}

// SeedAgnosticTemplate seeds the default agnostic template from embedded files.
// It recursively copies all files and directories into the target directory.
// Common home files (.tmux.conf, .zshrc, .gitconfig, .geminiignore) are
// embedded directly under embeds/templates/default/home/ and copied as part
// of the normal walk.
func SeedAgnosticTemplate(targetDir string, force bool) error {
	templateBase := "embeds/templates/default"

	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return fmt.Errorf("failed to create template directory %s: %w", targetDir, err)
	}

	if err := fs.WalkDir(EmbedsFS, templateBase, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Compute relative path from the template base
		relPath, err := filepath.Rel(templateBase, path)
		if err != nil {
			return fmt.Errorf("failed to compute relative path for %s: %w", path, err)
		}

		// Skip the root directory itself
		if relPath == "." {
			return nil
		}

		targetPath := filepath.Join(targetDir, relPath)

		if d.IsDir() {
			if err := os.MkdirAll(targetPath, 0755); err != nil {
				return fmt.Errorf("failed to create directory %s: %w", targetPath, err)
			}
			return nil
		}

		// Read embedded file
		data, err := EmbedsFS.ReadFile(path)
		if err != nil {
			return fmt.Errorf("failed to read embedded file %s: %w", relPath, err)
		}

		// Skip if file exists and force is false
		if !force {
			if _, err := os.Stat(targetPath); err == nil {
				return nil
			}
		}

		if err := os.WriteFile(targetPath, data, 0644); err != nil {
			return fmt.Errorf("failed to write file %s: %w", targetPath, err)
		}

		return nil
	}); err != nil {
		return err
	}

	return nil
}

// InitProjectOpts controls optional behavior for InitProject.
type InitProjectOpts struct {
	// SkipRuntimeCheck skips local container runtime detection.
	// Use this when initializing on a hub server where agents run on remote brokers.
	SkipRuntimeCheck bool
}

func InitProject(targetDir string, harnesses []api.Harness, opts ...InitProjectOpts) error {
	var opt InitProjectOpts
	if len(opts) > 0 {
		opt = opts[0]
	}

	isGit := isGitRepoFunc()

	var projectDir string
	var err error

	if targetDir != "" {
		projectDir = targetDir
	} else {
		projectDir, err = GetTargetProjectDir()
		if err != nil {
			return err
		}
	}

	// Enforce .scion/agents/ in .gitignore for git repos
	if isGit {
		root, err := util.RepoRoot()
		if err == nil {
			if err := EnsureScionGitignore(root); err != nil {
				return fmt.Errorf("failed to update .gitignore: %w", err)
			}
		}
	}

	// For non-git projects, externalize the project data.
	// The .scion entry in the project directory becomes a marker file pointing
	// to ~/.scion/project-configs/<slug>__<short-uuid>/.scion/
	if !isGit {
		return initExternalProject(projectDir, opt)
	}

	// Git project: create .scion as a directory (in-repo)
	return initInRepoProject(projectDir, opt)
}

// initExternalProject creates a non-git project with externalized data.
// The project directory gets a .scion marker file, and the actual project
// data lives under ~/.scion/project-configs/.
func initExternalProject(projectDir string, opt InitProjectOpts) error {
	// projectDir is the intended <project>/.scion path.
	projectRoot := filepath.Dir(projectDir)
	markerPath := filepath.Join(projectRoot, DotScion)

	// TODO(project-migration): Remove this check after a few releases.
	// Detect old-style non-git project (directory instead of marker file).
	if info, err := os.Stat(markerPath); err == nil && info.IsDir() {
		return fmt.Errorf("this project at %s uses an outdated directory format.\n"+
			"Non-git projects now use externalized storage. Please:\n"+
			"  1. Back up any custom templates from %s/templates/\n"+
			"  2. Remove the .scion directory: rm -rf %s\n"+
			"  3. Re-initialize: scion init",
			projectRoot, markerPath, markerPath)
	}

	// If a marker file already exists, read it and use the existing external path
	if IsProjectMarkerFile(markerPath) {
		resolved, err := ResolveProjectMarker(markerPath)
		if err != nil {
			return fmt.Errorf("existing project marker is invalid: %w", err)
		}
		// External project already set up — just ensure directories exist
		return ensureProjectDirs(resolved, opt)
	}

	// Generate new project identity
	projectID := GenerateProjectID()
	projectName := filepath.Base(projectRoot)
	projectSlug := api.Slugify(projectName)

	marker := &ProjectMarker{
		ProjectID:   projectID,
		ProjectName: projectName,
		ProjectSlug: projectSlug,
	}

	// Create external project directory
	externalPath, err := marker.ExternalProjectPath()
	if err != nil {
		return fmt.Errorf("failed to compute external project path: %w", err)
	}

	// Write settings with workspace-path and project_id before ensureProjectDirs
	// (which would create a settings.yaml without workspace_path if one doesn't exist yet).
	absProjectRoot, _ := filepath.Abs(projectRoot)
	if err := os.MkdirAll(externalPath, 0755); err != nil {
		return fmt.Errorf("failed to create external project directory: %w", err)
	}
	if GetSettingsPath(externalPath) == "" {
		if err := writeProjectSettings(externalPath, absProjectRoot, projectID, opt); err != nil {
			return err
		}
	}

	if err := ensureProjectDirs(externalPath, opt); err != nil {
		return err
	}

	// Ensure the project root directory exists before writing the marker
	if err := os.MkdirAll(projectRoot, 0755); err != nil {
		return fmt.Errorf("failed to create project directory: %w", err)
	}

	// Write the marker file
	if err := WriteProjectMarker(markerPath, marker); err != nil {
		return fmt.Errorf("failed to write project marker: %w", err)
	}

	return nil
}

// initInRepoProject creates a git project with .scion as a directory in the repo.
// Settings are stored externally at ~/.scion/project-configs/<slug>__<uuid>/.scion/
// and agent homes are stored externally at ~/.scion/project-configs/<slug>__<uuid>/.scion/agents/.
// Templates live in the in-repo .scion/templates/ so they can be committed to the repository.
func initInRepoProject(projectDir string, opt InitProjectOpts) error {
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		return fmt.Errorf("failed to create settings directory: %w", err)
	}

	// Ensure project-id file exists for split storage
	if _, err := ReadProjectID(projectDir); err != nil {
		if os.IsNotExist(err) {
			projectID := GenerateProjectIDForDir(filepath.Dir(projectDir))
			if err := WriteProjectID(projectDir, projectID); err != nil {
				return fmt.Errorf("failed to write project-id: %w", err)
			}
		} else {
			return fmt.Errorf("failed to read project-id: %w", err)
		}
	}

	// Seed settings.yaml in the external config dir (machine-specific, not committed)
	externalConfigDir, err := GetGitProjectExternalConfigDir(projectDir)
	if err != nil {
		return fmt.Errorf("failed to compute external config path: %w", err)
	}
	if err := ensureProjectSettingsFile(externalConfigDir, opt); err != nil {
		return err
	}

	// Create external agents directory for agent homes
	externalAgentsDir, err := GetGitProjectExternalAgentsDir(projectDir)
	if err != nil {
		return fmt.Errorf("failed to compute external agents path: %w", err)
	}
	if externalAgentsDir != "" {
		if err := os.MkdirAll(externalAgentsDir, 0755); err != nil {
			return fmt.Errorf("failed to create external agents directory: %w", err)
		}
	}

	// Create in-repo agents dir for git worktrees only
	if err := os.MkdirAll(filepath.Join(projectDir, "agents"), 0755); err != nil {
		return fmt.Errorf("failed to create agents directory: %w", err)
	}

	// Create in-repo templates dir — lives in-repo so project templates can be committed
	if err := os.MkdirAll(filepath.Join(projectDir, "templates"), 0755); err != nil {
		return fmt.Errorf("failed to create templates directory: %w", err)
	}

	return nil
}

// ensureProjectSettingsFile creates settings.yaml in configDir if it doesn't exist.
// Unlike ensureProjectConfigFiles, it does not create templates/ (used for git projects
// where templates live in-repo).
func ensureProjectSettingsFile(configDir string, opt InitProjectOpts) error {
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return fmt.Errorf("failed to create project config directory: %w", err)
	}

	settingsPath := GetSettingsPath(configDir)
	if settingsPath == "" {
		if !opt.SkipRuntimeCheck {
			if _, err := DetectLocalRuntime(); err != nil {
				return err
			}
		}

		defaultSettings, err := GetProjectDefaultSettingsYAML()
		if err != nil {
			return fmt.Errorf("failed to read default project settings: %w", err)
		}
		newSettingsPath := filepath.Join(configDir, "settings.yaml")
		if err := os.WriteFile(newSettingsPath, defaultSettings, 0644); err != nil {
			return fmt.Errorf("failed to seed settings.yaml: %w", err)
		}
	}

	return nil
}

// ensureProjectConfigFiles creates settings.yaml and templates/ in configDir.
// It does not create the agents/ directory — that is handled separately.
func ensureProjectConfigFiles(configDir string, opt InitProjectOpts) error {
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return fmt.Errorf("failed to create project config directory: %w", err)
	}

	settingsPath := GetSettingsPath(configDir)
	if settingsPath == "" {
		if !opt.SkipRuntimeCheck {
			if _, err := DetectLocalRuntime(); err != nil {
				return err
			}
		}

		defaultSettings, err := GetProjectDefaultSettingsYAML()
		if err != nil {
			return fmt.Errorf("failed to read default project settings: %w", err)
		}
		newSettingsPath := filepath.Join(configDir, "settings.yaml")
		if err := os.WriteFile(newSettingsPath, defaultSettings, 0644); err != nil {
			return fmt.Errorf("failed to seed settings.yaml: %w", err)
		}
	}

	if err := os.MkdirAll(filepath.Join(configDir, "templates"), 0755); err != nil {
		return fmt.Errorf("failed to create templates directory: %w", err)
	}

	return nil
}

// ensureProjectDirs creates the standard project subdirectories and seeds settings.
// Used for non-git projects and global project where config and agents share the same dir.
func ensureProjectDirs(projectDir string, opt InitProjectOpts) error {
	if err := ensureProjectConfigFiles(projectDir, opt); err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Join(projectDir, "agents"), 0755); err != nil {
		return fmt.Errorf("failed to create agents directory: %w", err)
	}

	return nil
}

// writeProjectSettings writes the initial settings.yaml for an external project,
// including the workspace-path field.
func writeProjectSettings(externalPath, workspacePath, projectID string, opt InitProjectOpts) error {
	if !opt.SkipRuntimeCheck {
		if _, err := DetectLocalRuntime(); err != nil {
			return err
		}
	}

	defaultSettings, err := GetProjectDefaultSettingsYAML()
	if err != nil {
		return fmt.Errorf("failed to read default project settings: %w", err)
	}

	// Parse default settings, add workspace-path, and re-marshal
	var settingsMap map[string]interface{}
	if err := yaml.Unmarshal(defaultSettings, &settingsMap); err != nil {
		return fmt.Errorf("failed to parse default project settings: %w", err)
	}
	settingsMap["workspace_path"] = workspacePath
	if projectID != "" {
		// In v1 format (schema_version: "1"), project_id is stored under
		// hub.project_id, not at the top level. Writing it at the top level
		// causes it to be silently dropped when UpdateVersionedSetting
		// round-trips through VersionedSettings (which has no top-level
		// project_id field), leading to the global hub.project_id bleeding
		// into local projects.
		if v := settingsMap["schema_version"]; v == "1" {
			hub, _ := settingsMap["hub"].(map[string]interface{})
			if hub == nil {
				hub = make(map[string]interface{})
				settingsMap["hub"] = hub
			}
			hub["grove_id"] = projectID
		} else {
			settingsMap["project_id"] = projectID
		}
	}

	data, err := yaml.Marshal(settingsMap)
	if err != nil {
		return fmt.Errorf("failed to marshal project settings: %w", err)
	}

	settingsFile := filepath.Join(externalPath, "settings.yaml")
	if err := os.WriteFile(settingsFile, data, 0644); err != nil {
		return fmt.Errorf("failed to write project settings.yaml: %w", err)
	}

	return nil
}

// InitMachineOpts controls optional behavior for InitMachine.
type InitMachineOpts struct {
	// ImageRegistry is the container image registry to configure.
	// If non-empty, it is written into settings after seeding.
	ImageRegistry string

	// Force overwrites existing template and harness-config files with the
	// versions embedded in the binary. Use this to refresh after a binary upgrade.
	Force bool
}

// InitMachine performs full global/machine-level setup: creates ~/.scion/,
// seeds settings, harness-configs, and the default agnostic template.
func InitMachine(harnesses []api.Harness, opts ...InitMachineOpts) error {
	globalDir, err := GetGlobalDir()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(globalDir, 0755); err != nil {
		return fmt.Errorf("failed to create global directory: %w", err)
	}

	// Create global settings file if it doesn't exist
	settingsPath := GetSettingsPath(globalDir)
	if settingsPath == "" {
		// Detect a functioning container runtime before seeding settings
		detectedRuntime, err := DetectLocalRuntime()
		if err != nil {
			return err
		}

		// Seed default YAML settings with the detected runtime
		defaultSettings, err := getDefaultSettingsYAMLForRuntime(detectedRuntime)
		if err != nil {
			// Fall back to JSON defaults
			defaultSettings, err = getDefaultSettingsDataForRuntime(detectedRuntime)
			if err != nil {
				return fmt.Errorf("failed to read default settings: %w", err)
			}
		}
		newSettingsPath := filepath.Join(globalDir, "settings.yaml")
		if err := os.WriteFile(newSettingsPath, defaultSettings, 0644); err != nil {
			return fmt.Errorf("failed to seed global settings.yaml: %w", err)
		}
	}

	var opt InitMachineOpts
	if len(opts) > 0 {
		opt = opts[0]
	}

	templatesDir := filepath.Join(globalDir, "templates")
	agentsDir := filepath.Join(globalDir, "agents")
	harnessConfigsDir := filepath.Join(globalDir, harnessConfigsDirName)

	if err := os.MkdirAll(agentsDir, 0755); err != nil {
		return fmt.Errorf("failed to create global agents directory: %w", err)
	}

	// Seed default agnostic template
	if err := SeedAgnosticTemplate(filepath.Join(templatesDir, "default"), opt.Force); err != nil {
		return fmt.Errorf("failed to seed global default agnostic template: %w", err)
	}

	for _, h := range harnesses {
		// Seed harness-config directory
		if err := SeedHarnessConfig(filepath.Join(harnessConfigsDir, h.Name()), h, opt.Force); err != nil {
			return fmt.Errorf("failed to seed global %s harness-config: %w", h.Name(), err)
		}
	}

	// Pre-populate a broker ID so this machine has a stable identity.
	// This will be overwritten if the user later registers with a Hub.
	if err := ensureBrokerID(globalDir); err != nil {
		return fmt.Errorf("failed to pre-populate broker ID: %w", err)
	}

	if opt.ImageRegistry != "" {
		if err := UpdateSetting(globalDir, "image_registry", opt.ImageRegistry, true); err != nil {
			return fmt.Errorf("failed to set image_registry: %w", err)
		}
	}

	return nil
}

// ensureBrokerID checks whether a broker ID already exists in the global settings
// and generates one if not. This gives the machine a stable identity before
// Hub registration.
func ensureBrokerID(globalDir string) error {
	settings, err := LoadSettings(globalDir)
	if err != nil {
		// If we can't load settings, skip — not critical
		return nil
	}

	// Check if broker ID is already set (via legacy or versioned path)
	if settings.Hub != nil && settings.Hub.BrokerID != "" {
		return nil
	}

	brokerID := uuid.New().String()
	return UpdateSetting(globalDir, "hub.brokerId", brokerID, true)
}

// InitGlobal is an alias for InitMachine, kept for backward compatibility.
func InitGlobal(harnesses []api.Harness, opts ...InitMachineOpts) error {
	return InitMachine(harnesses, opts...)
}

// EnsureScionGitignore ensures that .scion/agents/ is ignored by git at the
// given repo root. It uses git check-ignore to detect whether any existing
// pattern (in any .gitignore or global excludes) already covers the path.
// If not, it appends .scion/agents/ to the root .gitignore file.
// Only the agents directory is excluded; templates/ and other config can be committed.
func EnsureScionGitignore(repoRoot string) error {
	// Use git check-ignore for authoritative detection — this respects all
	// gitignore sources (nested .gitignore, global excludes, etc.).
	if util.IsIgnored(repoRoot, ".scion/agents/") {
		return nil
	}

	gitignorePath := filepath.Join(repoRoot, ".gitignore")

	content, err := os.ReadFile(gitignorePath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	// Append .scion/agents/ to .gitignore.
	var newContent string
	if len(content) > 0 && content[len(content)-1] != '\n' {
		newContent = string(content) + "\n.scion/agents/\n"
	} else {
		newContent = string(content) + ".scion/agents/\n"
	}

	return os.WriteFile(gitignorePath, []byte(newContent), 0644)
}
