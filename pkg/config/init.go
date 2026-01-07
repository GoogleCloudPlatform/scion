package config

import (
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

//go:embed all:embeds/*
var embedsFS embed.FS

func GetDefaultSettingsData() ([]byte, error) {
	data, err := embedsFS.ReadFile("embeds/default_settings.json")
	if err != nil {
		return nil, err
	}

	var settings Settings
	if err := json.Unmarshal(data, &settings); err == nil {
		if local, ok := settings.Profiles["local"]; ok {
			if runtime.GOOS == "darwin" {
				local.Runtime = "container"
			} else {
				local.Runtime = "docker"
			}
			settings.Profiles["local"] = local
			if updated, err := json.MarshalIndent(settings, "", "  "); err == nil {
				return updated, nil
			}
		}
	}
	return data, nil
}

func SeedTemplateDir(templateDir, templateName, harness, embedDir, configDirName string, force bool) error {
	homeDir := filepath.Join(templateDir, "home")
	// Create directories
	dirs := []string{
		templateDir,
		homeDir,
		filepath.Join(homeDir, configDirName),
		filepath.Join(homeDir, ".config", "gcloud"),
	}

	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", dir, err)
		}
	}

	// Helper to read embedded file
	readEmbed := func(name string) string {
		data, err := embedsFS.ReadFile(filepath.Join("embeds", embedDir, name))
		if err != nil {
			// Fallback to gemini if not found in harness dir
			data, err = embedsFS.ReadFile(filepath.Join("embeds", "gemini", name))
			if err != nil {
				return ""
			}
		}
		return string(data)
	}

	readCommonEmbed := func(name string) string {
		data, err := embedsFS.ReadFile(filepath.Join("embeds", "common", name))
		if err != nil {
			return ""
		}
		return string(data)
	}

	scionJSONStr := readEmbed("scion-agent.json")

	mdFile := "gemini.md"
	claudeJSON := ""
	if harness == "claude" {
		mdFile = "claude.md"
		claudeJSON = readEmbed(".claude.json")
	}

	// Seed template files
	files := []struct {
		path    string
		content string
		mode    os.FileMode
	}{
		{filepath.Join(templateDir, "scion-agent.json"), scionJSONStr, 0644},
		{filepath.Join(homeDir, "scion_hook.py"), readEmbed("scion_hook.py"), 0644},
		{filepath.Join(homeDir, "scion_tool.py"), readCommonEmbed("scion_tool.py"), 0644},
		{filepath.Join(homeDir, "sciontool"), "#!/bin/bash\npython3 $HOME/scion_tool.py \"$@\"\n", 0755},
		{filepath.Join(homeDir, configDirName, "settings.json"), readEmbed("settings.json"), 0644},
		{filepath.Join(homeDir, configDirName, "system_prompt.md"), readEmbed("system_prompt.md"), 0644},
		{filepath.Join(homeDir, configDirName, mdFile), readEmbed(mdFile), 0644},
		{filepath.Join(homeDir, ".bashrc"), readEmbed("bashrc"), 0644},
		{filepath.Join(homeDir, ".tmux.conf"), readCommonEmbed(".tmux.conf"), 0644},
	}

	if claudeJSON != "" {
		files = append(files, struct {
			path    string
			content string
			mode    os.FileMode
		}{filepath.Join(homeDir, ".claude.json"), claudeJSON, 0644})
	}

	for _, f := range files {
		// Always write settings.json and .claude.json to ensure they match current defaults
		baseName := filepath.Base(f.path)
		if force || baseName == "settings.json" || baseName == ".claude.json" {
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

func InitProject(targetDir string) error {
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
	settingsPath := filepath.Join(projectDir, "settings.json")
	if _, err := os.Stat(settingsPath); os.IsNotExist(err) {
		// Seed with default settings
		defaultSettings, err := GetDefaultSettingsData()
		if err != nil {
			return fmt.Errorf("failed to read default settings: %w", err)
		}
		if err := os.WriteFile(settingsPath, defaultSettings, 0644); err != nil {
			return fmt.Errorf("failed to seed settings.json: %w", err)
		}
	}

	templatesDir := filepath.Join(projectDir, "templates")
	agentsDir := filepath.Join(projectDir, "agents")

	if err := os.MkdirAll(agentsDir, 0755); err != nil {
		return fmt.Errorf("failed to create agents directory: %w", err)
	}

	if err := SeedTemplateDir(filepath.Join(templatesDir, "gemini"), "gemini", "gemini", "gemini", ".gemini", false); err != nil {
		return fmt.Errorf("failed to seed gemini template: %w", err)
	}

	return SeedTemplateDir(filepath.Join(templatesDir, "claude"), "claude", "claude", "claude", ".claude", false)
}

func InitGlobal() error {
	globalDir, err := GetGlobalDir()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(globalDir, 0755); err != nil {
		return fmt.Errorf("failed to create global directory: %w", err)
	}

	// Create global settings file if it doesn't exist
	settingsPath := filepath.Join(globalDir, "settings.json")
	if _, err := os.Stat(settingsPath); os.IsNotExist(err) {
		defaultSettings, err := GetDefaultSettingsData()
		if err != nil {
			return fmt.Errorf("failed to read default settings: %w", err)
		}
		if err := os.WriteFile(settingsPath, defaultSettings, 0644); err != nil {
			return fmt.Errorf("failed to seed global settings.json: %w", err)
		}
	}

	templatesDir := filepath.Join(globalDir, "templates")
	agentsDir := filepath.Join(globalDir, "agents")

	if err := os.MkdirAll(agentsDir, 0755); err != nil {
		return fmt.Errorf("failed to create global agents directory: %w", err)
	}

	if err := SeedTemplateDir(filepath.Join(templatesDir, "gemini"), "gemini", "gemini", "gemini", ".gemini", false); err != nil {
		return fmt.Errorf("failed to seed global gemini template: %w", err)
	}

	return SeedTemplateDir(filepath.Join(templatesDir, "claude"), "claude", "claude", "claude", ".claude", false)
}
