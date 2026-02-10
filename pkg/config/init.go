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
	"os"
	"path/filepath"
	"runtime"

	"github.com/google/uuid"
	"github.com/ptone/scion-agent/pkg/api"
	"github.com/ptone/scion-agent/pkg/util"
	"gopkg.in/yaml.v3"
)

//go:embed all:embeds/*
var EmbedsFS embed.FS

func GetDefaultSettingsData() ([]byte, error) {
	// Load embedded YAML defaults
	data, err := EmbedsFS.ReadFile("embeds/default_settings.yaml")
	if err != nil {
		return nil, err
	}

	var settings Settings
	if err := yaml.Unmarshal(data, &settings); err != nil {
		return nil, err
	}

	// Apply OS-specific runtime adjustment for local profile
	if local, ok := settings.Profiles["local"]; ok {
		if runtime.GOOS == "darwin" {
			local.Runtime = "container"
		} else {
			local.Runtime = "docker"
		}
		settings.Profiles["local"] = local
	}

	// Return JSON for backward compatibility with callers expecting JSON
	return json.MarshalIndent(settings, "", "  ")
}

// SeedCommonFiles seeds the common files for a harness template.
// genericEmbedDir is usually "common".
// specificEmbedDir is the harness specific dir in embeds (e.g. "gemini").
func SeedCommonFiles(templateDir, genericEmbedDir, specificEmbedDir, configDirName string, force bool) error {
	homeDir := filepath.Join(templateDir, "home")
	// Create directories
	dirs := []string{
		templateDir,
		homeDir,
		filepath.Join(homeDir, ".config", "gcloud"),
	}
	if configDirName != "" {
		dirs = append(dirs, filepath.Join(homeDir, configDirName))
	}

	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", dir, err)
		}
	}

	// Helper to read embedded file
	readEmbed := func(dir, name string) string {
		data, err := EmbedsFS.ReadFile(filepath.Join("embeds", dir, name))
		if err != nil {
			// Fallback to gemini if not found in specific dir
			// Only fallback for non-opencode harnesses
			if dir != "opencode" {
				data, err = EmbedsFS.ReadFile(filepath.Join("embeds", "gemini", name))
				if err == nil {
					return string(data)
				}
			}
			return ""
		}
		return string(data)
	}

	// Helper to read scion-agent config, preferring YAML over JSON
	readScionAgentConfig := func(dir string) string {
		// Try YAML first
		data, err := EmbedsFS.ReadFile(filepath.Join("embeds", dir, "scion-agent.yaml"))
		if err == nil {
			return string(data)
		}
		// Fall back to JSON
		data, err = EmbedsFS.ReadFile(filepath.Join("embeds", dir, "scion-agent.json"))
		if err == nil {
			return string(data)
		}
		// Fallback to gemini if not found (except for opencode)
		if dir != "opencode" {
			data, err = EmbedsFS.ReadFile(filepath.Join("embeds", "gemini", "scion-agent.yaml"))
			if err == nil {
				return string(data)
			}
			data, err = EmbedsFS.ReadFile(filepath.Join("embeds", "gemini", "scion-agent.json"))
			if err == nil {
				return string(data)
			}
		}
		return ""
	}

	// Read scion-agent config (YAML or JSON)
	scionAgentConfigStr := readScionAgentConfig(specificEmbedDir)

	// Seed template files
	files := []struct {
		path    string
		content string
		mode    os.FileMode
	}{
		{filepath.Join(templateDir, "scion-agent.yaml"), scionAgentConfigStr, 0644},
		{filepath.Join(homeDir, ".bashrc"), readEmbed(specificEmbedDir, "bashrc"), 0644},
		{filepath.Join(homeDir, ".tmux.conf"), readEmbed(genericEmbedDir, ".tmux.conf"), 0644},
	}

	if configDirName != "" {
		files = append(files, []struct {
			path    string
			content string
			mode    os.FileMode
		}{
			{filepath.Join(homeDir, configDirName, "settings.json"), readEmbed(specificEmbedDir, "settings.json"), 0644},
			{filepath.Join(homeDir, configDirName, "system_prompt.md"), readEmbed(specificEmbedDir, "system_prompt.md"), 0644},
		}...)
	}

	for _, f := range files {
		if f.content == "" {
			continue
		}
		baseName := filepath.Base(f.path)
		// Force overwrite for critical config files
		if force || baseName == "settings.json" {
			if err := os.WriteFile(f.path, []byte(f.content), f.mode); err != nil {
				return fmt.Errorf("failed to write file %s: %w", f.path, err)
			}
			continue
		}

		if _, err := os.Stat(f.path); os.IsNotExist(err) {
			if err := os.WriteFile(f.path, []byte(f.content), f.mode); err != nil {
				return fmt.Errorf("failed to write file %s: %w", f.path, err)
			}
		}
	}

	return nil
}

// GenerateGroveID creates a grove ID based on git context.
// For git repos with remote: normalized remote URL (e.g., github.com/org/repo)
// For git repos without remote: UUID
// For non-git directories: UUID
func GenerateGroveID() string {
	if util.IsGitRepo() {
		remote := util.GetGitRemote()
		if remote != "" {
			return util.NormalizeGitRemote(remote)
		}
	}
	return uuid.New().String()
}

// GenerateGroveIDForDir creates a grove ID based on git context for the specified directory.
func GenerateGroveIDForDir(dir string) string {
	if util.IsGitRepoDir(dir) {
		remote := util.GetGitRemoteDir(dir)
		if remote != "" {
			return util.NormalizeGitRemote(remote)
		}
	}
	return uuid.New().String()
}

// IsInsideGrove returns true if the current working directory or any parent contains a .scion directory.
func IsInsideGrove() bool {
	_, ok := FindProjectRoot()
	return ok
}

// GetEnclosingGrovePath returns the path to the enclosing .scion directory if one exists,
// along with the root directory containing it.
func GetEnclosingGrovePath() (grovePath string, rootDir string, found bool) {
	wd, err := os.Getwd()
	if err != nil {
		return "", "", false
	}

	dir := wd
	for {
		p := filepath.Join(dir, DotScion)
		if info, err := os.Stat(p); err == nil && info.IsDir() {
			if abs, err := filepath.EvalSymlinks(p); err == nil {
				return abs, dir, true
			}
			return p, dir, true
		}

		parent := filepath.Dir(dir)
		if parent == dir { // Reached filesystem root
			break
		}
		dir = parent
	}
	return "", "", false
}

func InitProject(targetDir string, harnesses []api.Harness) error {
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

	// Create grove-level settings file if it doesn't exist
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		return fmt.Errorf("failed to create settings directory: %w", err)
	}
	// Check if any settings file exists (YAML or JSON)
	settingsPath := GetSettingsPath(projectDir)
	if settingsPath == "" {
		// No settings file exists, seed with default YAML settings
		defaultSettings, err := GetDefaultSettingsDataYAML()
		if err != nil {
			// Fall back to JSON defaults
			defaultSettings, err = GetDefaultSettingsData()
			if err != nil {
				return fmt.Errorf("failed to read default settings: %w", err)
			}
		}
		newSettingsPath := filepath.Join(projectDir, "settings.yaml")
		if err := os.WriteFile(newSettingsPath, defaultSettings, 0644); err != nil {
			return fmt.Errorf("failed to seed settings.yaml: %w", err)
		}
	}

	templatesDir := filepath.Join(projectDir, "templates")
	agentsDir := filepath.Join(projectDir, "agents")

	if err := os.MkdirAll(agentsDir, 0755); err != nil {
		return fmt.Errorf("failed to create agents directory: %w", err)
	}

	for _, h := range harnesses {
		if err := h.SeedTemplateDir(filepath.Join(templatesDir, h.Name()), false); err != nil {
			return fmt.Errorf("failed to seed %s template: %w", h.Name(), err)
		}
	}

	return nil
}

func InitGlobal(harnesses []api.Harness) error {
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
		// No settings file exists, seed with default YAML settings
		defaultSettings, err := GetDefaultSettingsDataYAML()
		if err != nil {
			// Fall back to JSON defaults
			defaultSettings, err = GetDefaultSettingsData()
			if err != nil {
				return fmt.Errorf("failed to read default settings: %w", err)
			}
		}
		newSettingsPath := filepath.Join(globalDir, "settings.yaml")
		if err := os.WriteFile(newSettingsPath, defaultSettings, 0644); err != nil {
			return fmt.Errorf("failed to seed global settings.yaml: %w", err)
		}
	}

	templatesDir := filepath.Join(globalDir, "templates")
	agentsDir := filepath.Join(globalDir, "agents")

	if err := os.MkdirAll(agentsDir, 0755); err != nil {
		return fmt.Errorf("failed to create global agents directory: %w", err)
	}

	for _, h := range harnesses {
		if err := h.SeedTemplateDir(filepath.Join(templatesDir, h.Name()), false); err != nil {
			return fmt.Errorf("failed to seed global %s template: %w", h.Name(), err)
		}
	}

	return nil
}
