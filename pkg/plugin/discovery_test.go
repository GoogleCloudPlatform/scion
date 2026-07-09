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
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExpandPath(t *testing.T) {
	home, _ := os.UserHomeDir()

	tests := []struct {
		input    string
		expected string
	}{
		{"~/plugins/broker/nats", filepath.Join(home, "plugins/broker/nats")},
		{"/absolute/path", "/absolute/path"},
		{"relative/path", "relative/path"},
	}

	for _, tt := range tests {
		result := expandPath(tt.input)
		assert.Equal(t, tt.expected, result)
	}
}

func TestScanPluginDir_Empty(t *testing.T) {
	dir := t.TempDir()
	logger := slog.Default()

	plugins := scanPluginDir(dir, PluginTypeBroker, logger)
	assert.Empty(t, plugins)
}

func TestScanPluginDir_NonExistent(t *testing.T) {
	logger := slog.Default()

	plugins := scanPluginDir("/nonexistent/path", PluginTypeBroker, logger)
	assert.Empty(t, plugins)
}

func TestScanPluginDir_FindsPlugins(t *testing.T) {
	dir := t.TempDir()
	logger := slog.Default()

	// Create a valid plugin binary (executable)
	pluginPath := filepath.Join(dir, "scion-plugin-nats")
	require.NoError(t, os.WriteFile(pluginPath, []byte("#!/bin/sh\n"), 0755))

	// Create a non-plugin file
	require.NoError(t, os.WriteFile(filepath.Join(dir, "other-binary"), []byte(""), 0755))

	// Create a non-executable plugin file
	require.NoError(t, os.WriteFile(filepath.Join(dir, "scion-plugin-noexec"), []byte(""), 0644))

	// Create a directory with the plugin prefix
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "scion-plugin-isdir"), 0755))

	plugins := scanPluginDir(dir, PluginTypeBroker, logger)
	assert.Len(t, plugins, 1)
	assert.Equal(t, "nats", plugins[0].Name)
	assert.Equal(t, PluginTypeBroker, plugins[0].Type)
	assert.Equal(t, pluginPath, plugins[0].Path)
	assert.False(t, plugins[0].FromConfig)
}

func TestDiscoverPlugins_FromConfig(t *testing.T) {
	dir := t.TempDir()
	logger := slog.Default()

	// Create broker plugin directory
	brokerDir := filepath.Join(dir, "broker")
	require.NoError(t, os.MkdirAll(brokerDir, 0755))

	// Create a plugin binary
	pluginPath := filepath.Join(brokerDir, "scion-plugin-nats")
	require.NoError(t, os.WriteFile(pluginPath, []byte("#!/bin/sh\n"), 0755))

	cfg := PluginsConfig{
		Broker: map[string]PluginEntry{
			"nats": {
				Config: map[string]string{"url": "nats://localhost:4222"},
			},
		},
	}

	discovered := DiscoverPlugins(cfg, dir, logger)
	assert.Len(t, discovered, 1)
	assert.Equal(t, "nats", discovered[0].Name)
	assert.Equal(t, PluginTypeBroker, discovered[0].Type)
	assert.Equal(t, pluginPath, discovered[0].Path)
	assert.True(t, discovered[0].FromConfig)
	assert.Equal(t, "nats://localhost:4222", discovered[0].Config["url"])
}

func TestDiscoverPlugins_ExplicitPath(t *testing.T) {
	dir := t.TempDir()
	logger := slog.Default()

	// Create plugin at a custom location
	customPath := filepath.Join(dir, "custom", "my-nats-plugin")
	require.NoError(t, os.MkdirAll(filepath.Dir(customPath), 0755))
	require.NoError(t, os.WriteFile(customPath, []byte("#!/bin/sh\n"), 0755))

	cfg := PluginsConfig{
		Broker: map[string]PluginEntry{
			"nats": {
				Path: customPath,
			},
		},
	}

	discovered := DiscoverPlugins(cfg, dir, logger)
	assert.Len(t, discovered, 1)
	assert.Equal(t, customPath, discovered[0].Path)
}

func TestDiscoverPlugins_AutoDiscovery(t *testing.T) {
	dir := t.TempDir()
	logger := slog.Default()

	// Create auto-discoverable broker plugin
	brokerDir := filepath.Join(dir, "broker")
	require.NoError(t, os.MkdirAll(brokerDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(brokerDir, "scion-plugin-nats"), []byte("#!/bin/sh\n"), 0755))

	// Empty config - should auto-discover
	cfg := PluginsConfig{}

	discovered := DiscoverPlugins(cfg, dir, logger)
	assert.Len(t, discovered, 1)
	assert.Equal(t, "nats", discovered[0].Name)
	assert.Equal(t, PluginTypeBroker, discovered[0].Type)
	assert.False(t, discovered[0].FromConfig)
}

func TestDiscoverPlugins_NoDuplicates(t *testing.T) {
	dir := t.TempDir()
	logger := slog.Default()

	// Create plugin in the standard location
	brokerDir := filepath.Join(dir, "broker")
	require.NoError(t, os.MkdirAll(brokerDir, 0755))
	pluginPath := filepath.Join(brokerDir, "scion-plugin-nats")
	require.NoError(t, os.WriteFile(pluginPath, []byte("#!/bin/sh\n"), 0755))

	// Also configure it in settings — should not appear twice
	cfg := PluginsConfig{
		Broker: map[string]PluginEntry{
			"nats": {
				Config: map[string]string{"url": "nats://localhost"},
			},
		},
	}

	discovered := DiscoverPlugins(cfg, dir, logger)
	assert.Len(t, discovered, 1)
}

func TestDiscoverPlugins_MissingBinary(t *testing.T) {
	dir := t.TempDir()
	logger := slog.Default()

	cfg := PluginsConfig{
		Broker: map[string]PluginEntry{
			"nonexistent": {
				Config: map[string]string{"url": "nats://localhost"},
			},
		},
	}

	discovered := DiscoverPlugins(cfg, dir, logger)
	assert.Empty(t, discovered)
}

func TestDiscoverPlugins_SelfManaged(t *testing.T) {
	dir := t.TempDir()
	logger := slog.Default()

	cfg := PluginsConfig{
		Broker: map[string]PluginEntry{
			"googlechat": {
				SelfManaged: true,
				Address:     "localhost:9090",
				Config:      map[string]string{"project_id": "my-gcp-project"},
			},
		},
	}

	discovered := DiscoverPlugins(cfg, dir, logger)
	require.Len(t, discovered, 1)
	assert.Equal(t, "googlechat", discovered[0].Name)
	assert.Equal(t, PluginTypeBroker, discovered[0].Type)
	assert.True(t, discovered[0].SelfManaged)
	assert.Equal(t, "localhost:9090", discovered[0].Address)
	assert.Empty(t, discovered[0].Path) // No binary path for self-managed plugins
	assert.True(t, discovered[0].FromConfig)
}

func TestDiscoverPlugins_MixedModes(t *testing.T) {
	dir := t.TempDir()
	logger := slog.Default()

	// Create a regular plugin binary
	brokerDir := filepath.Join(dir, "broker")
	require.NoError(t, os.MkdirAll(brokerDir, 0755))
	pluginPath := filepath.Join(brokerDir, "scion-plugin-nats")
	require.NoError(t, os.WriteFile(pluginPath, []byte("#!/bin/sh\n"), 0755))

	cfg := PluginsConfig{
		Broker: map[string]PluginEntry{
			"nats": {
				Config: map[string]string{"url": "nats://localhost:4222"},
			},
			"googlechat": {
				SelfManaged: true,
				Address:     "localhost:9090",
				Config:      map[string]string{"project_id": "my-gcp-project"},
			},
		},
	}

	discovered := DiscoverPlugins(cfg, dir, logger)
	require.Len(t, discovered, 2)

	// Find each by name since map iteration order is non-deterministic
	var nats, chat *DiscoveredPlugin
	for i := range discovered {
		switch discovered[i].Name {
		case "nats":
			nats = &discovered[i]
		case "googlechat":
			chat = &discovered[i]
		}
	}

	require.NotNil(t, nats)
	assert.False(t, nats.SelfManaged)
	assert.Equal(t, pluginPath, nats.Path)

	require.NotNil(t, chat)
	assert.True(t, chat.SelfManaged)
	assert.Empty(t, chat.Path)
	assert.Equal(t, "localhost:9090", chat.Address)
}

func TestDiscoverPlugins_ConfigFilePropagated(t *testing.T) {
	dir := t.TempDir()
	logger := slog.Default()

	brokerDir := filepath.Join(dir, "broker")
	require.NoError(t, os.MkdirAll(brokerDir, 0755))
	pluginPath := filepath.Join(brokerDir, "scion-plugin-telegram")
	require.NoError(t, os.WriteFile(pluginPath, []byte("#!/bin/sh\n"), 0755))

	cfg := PluginsConfig{
		Broker: map[string]PluginEntry{
			"telegram": {
				ConfigFile: "/etc/scion/telegram.yaml",
				Config:     map[string]string{"webhook_listen": ":9094"},
			},
		},
	}

	discovered := DiscoverPlugins(cfg, dir, logger)
	require.Len(t, discovered, 1)
	assert.Equal(t, "/etc/scion/telegram.yaml", discovered[0].Config["config_file"])
	assert.Equal(t, ":9094", discovered[0].Config["webhook_listen"])
}

func TestDiscoverPlugins_ConfigFilePropagated_SelfManaged(t *testing.T) {
	dir := t.TempDir()
	logger := slog.Default()

	cfg := PluginsConfig{
		Broker: map[string]PluginEntry{
			"googlechat": {
				SelfManaged: true,
				Address:     "localhost:9090",
				ConfigFile:  "/etc/scion/googlechat.yaml",
				Config:      map[string]string{"project_id": "my-project"},
			},
		},
	}

	discovered := DiscoverPlugins(cfg, dir, logger)
	require.Len(t, discovered, 1)
	assert.Equal(t, "/etc/scion/googlechat.yaml", discovered[0].Config["config_file"])
	assert.Equal(t, "my-project", discovered[0].Config["project_id"])
}

func TestDiscoverPlugins_NoConfigFile_NoKey(t *testing.T) {
	dir := t.TempDir()
	logger := slog.Default()

	brokerDir := filepath.Join(dir, "broker")
	require.NoError(t, os.MkdirAll(brokerDir, 0755))
	pluginPath := filepath.Join(brokerDir, "scion-plugin-nats")
	require.NoError(t, os.WriteFile(pluginPath, []byte("#!/bin/sh\n"), 0755))

	cfg := PluginsConfig{
		Broker: map[string]PluginEntry{
			"nats": {
				Config: map[string]string{"url": "nats://localhost:4222"},
			},
		},
	}

	discovered := DiscoverPlugins(cfg, dir, logger)
	require.Len(t, discovered, 1)
	assert.Empty(t, discovered[0].Config["config_file"])
}

func TestDefaultPluginsDir(t *testing.T) {
	dir, err := DefaultPluginsDir()
	require.NoError(t, err)

	home, _ := os.UserHomeDir()
	assert.Equal(t, filepath.Join(home, ".scion", "plugins"), dir)
}
