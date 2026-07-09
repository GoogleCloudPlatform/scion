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

package plugin

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// DiscoveredPlugin represents a plugin found during discovery.
type DiscoveredPlugin struct {
	Name        string
	Type        string // "broker" (additional types may be added in future)
	Path        string // absolute path to the binary (empty for self-managed plugins)
	Config      map[string]string
	FromConfig  bool   // true if found via settings, false if auto-discovered
	SelfManaged bool   // true if the plugin manages its own process lifecycle
	Address     string // RPC address for self-managed plugins
}

// DiscoverPlugins finds all available plugins from settings configuration and
// filesystem scanning. Discovery order:
//  1. Explicit path in settings
//  2. Scan ~/.scion/plugins/<type>/ directory
//  3. Search $PATH for scion-plugin-<name> (lower priority)
func DiscoverPlugins(cfg PluginsConfig, pluginsDir string, logger *slog.Logger) []DiscoveredPlugin {
	var discovered []DiscoveredPlugin

	// 1. From settings configuration
	for name, entry := range cfg.Broker {
		if entry.ConfigFile != "" {
			if entry.Config == nil {
				entry.Config = make(map[string]string)
			}
			entry.Config["config_file"] = entry.ConfigFile
		}
		if entry.SelfManaged {
			discovered = append(discovered, DiscoveredPlugin{
				Name:        name,
				Type:        PluginTypeBroker,
				Config:      entry.Config,
				FromConfig:  true,
				SelfManaged: true,
				Address:     entry.Address,
			})
			continue
		}
		path := resolvePluginPath(name, PluginTypeBroker, entry.Path, pluginsDir, logger)
		if path == "" {
			logger.Warn("Plugin binary not found", "type", PluginTypeBroker, "name", name)
			continue
		}
		discovered = append(discovered, DiscoveredPlugin{
			Name:       name,
			Type:       PluginTypeBroker,
			Path:       path,
			Config:     entry.Config,
			FromConfig: true,
		})
	}

	// 2. Scan filesystem for plugins not already in settings
	configuredNames := make(map[string]bool)
	for _, d := range discovered {
		configuredNames[d.Type+":"+d.Name] = true
	}

	for _, pluginType := range []string{PluginTypeBroker} {
		scanDir := filepath.Join(pluginsDir, pluginType)
		scanned := scanPluginDir(scanDir, pluginType, logger)
		for _, sd := range scanned {
			key := sd.Type + ":" + sd.Name
			if !configuredNames[key] {
				discovered = append(discovered, sd)
				configuredNames[key] = true
			}
		}
	}

	return discovered
}

// resolvePluginPath resolves the absolute path to a plugin binary.
// It checks: explicit path → plugins dir → $PATH.
func resolvePluginPath(name, pluginType, explicitPath, pluginsDir string, logger *slog.Logger) string {
	binaryName := PluginBinaryPrefix + name

	// 1. Explicit path from settings
	if explicitPath != "" {
		expanded := expandPath(explicitPath)
		if _, err := os.Stat(expanded); err == nil {
			return expanded
		}
		logger.Warn("Explicit plugin path not found", "path", explicitPath, "name", name)
	}

	// 2. Scan ~/.scion/plugins/<type>/ directory
	typeDir := filepath.Join(pluginsDir, pluginType)
	candidate := filepath.Join(typeDir, binaryName)
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}

	// 3. Search $PATH
	if path, err := exec.LookPath(binaryName); err == nil {
		return path
	}

	return ""
}

// scanPluginDir scans a directory for plugin binaries matching the naming convention.
func scanPluginDir(dir, pluginType string, logger *slog.Logger) []DiscoveredPlugin {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Debug("Failed to scan plugin directory", "dir", dir, "error", err)
		}
		return nil
	}

	var discovered []DiscoveredPlugin
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, PluginBinaryPrefix) {
			continue
		}

		pluginName := strings.TrimPrefix(name, PluginBinaryPrefix)
		if pluginName == "" {
			continue
		}

		fullPath := filepath.Join(dir, name)
		info, err := entry.Info()
		if err != nil {
			logger.Debug("Failed to stat plugin file", "path", fullPath, "error", err)
			continue
		}

		// Check if executable
		if info.Mode()&0111 == 0 {
			logger.Debug("Plugin file is not executable", "path", fullPath)
			continue
		}

		discovered = append(discovered, DiscoveredPlugin{
			Name:       pluginName,
			Type:       pluginType,
			Path:       fullPath,
			FromConfig: false,
		})

		logger.Debug("Discovered plugin", "type", pluginType, "name", pluginName, "path", fullPath)
	}

	return discovered
}

// DefaultPluginsDir returns the default plugins directory path (~/.scion/plugins).
func DefaultPluginsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get user home directory: %w", err)
	}
	return filepath.Join(home, ".scion", "plugins"), nil
}

// expandPath expands ~ to the user's home directory.
func expandPath(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return path
		}
		return filepath.Join(home, path[2:])
	}
	return path
}
