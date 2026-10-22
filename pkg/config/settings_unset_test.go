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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func readUnsetSettingsFile(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(GetSettingsPath(dir))
	require.NoError(t, err)
	return string(data)
}

// Unsetting cli.autohelp removes the key, so the setting inherits (here the
// embedded default true), instead of being written false. The YAML file is
// edited in place: comments and other keys survive, and the emptied cli
// section goes with the key.
func TestUnsetSetting_RemovesKeyAndInherits(t *testing.T) {
	project := "schema_version: \"1\"\n# project settings\nactive_profile: remote # pinned\ncli:\n  autohelp: false\n"
	projectDir := setupSettingsViewTest(t, "", project)

	res, err := UnsetSetting(projectDir, "cli.autohelp", false)
	require.NoError(t, err)
	assert.True(t, res.Removed)
	got := readUnsetSettingsFile(t, projectDir)
	assert.NotContains(t, got, "autohelp")
	assert.NotContains(t, got, "cli:", "the emptied parent section is removed")
	assert.Contains(t, got, "# project settings")
	assert.Contains(t, got, "active_profile: remote # pinned")

	v, err := ResolveSettingValue(projectDir, "cli.autohelp")
	require.NoError(t, err)
	assert.Equal(t, "true", v.Value)
	assert.Equal(t, "default", v.Source)
}

// A parent that holds other keys stays.
func TestUnsetSetting_KeepsNonEmptyParent(t *testing.T) {
	projectDir := setupSettingsViewTest(t, "", "schema_version: \"1\"\ncli:\n  autohelp: false\n  mode: agent\n")
	res, err := UnsetSetting(projectDir, "cli.autohelp", false)
	require.NoError(t, err)
	assert.True(t, res.Removed)
	got := readUnsetSettingsFile(t, projectDir)
	assert.NotContains(t, got, "autohelp")
	assert.Contains(t, got, "mode: agent")
}

// A key that is not set changes nothing: no write, and a legacy-format file
// is not migrated.
func TestUnsetSetting_AbsentKeyWritesNothing(t *testing.T) {
	for _, tc := range []struct{ name, content string }{
		{"versioned", "schema_version: \"1\"\nactive_profile: remote\n"},
		{"legacy", "harnesses: {}\nactive_profile: remote\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			projectDir := setupSettingsViewTest(t, "", tc.content)
			path := GetSettingsPath(projectDir)
			old := time.Now().Add(-time.Hour)
			require.NoError(t, os.Chtimes(path, old, old))

			res, err := UnsetSetting(projectDir, "hub.endpoint", false)
			require.NoError(t, err)
			assert.False(t, res.Removed)
			assert.Equal(t, tc.content, readUnsetSettingsFile(t, projectDir))
			info, err := os.Stat(path)
			require.NoError(t, err)
			assert.True(t, info.ModTime().Equal(old), "the file was rewritten")
		})
	}

	t.Run("no file", func(t *testing.T) {
		projectDir := setupSettingsViewTest(t, "", "")
		res, err := UnsetSetting(projectDir, "cli.autohelp", false)
		require.NoError(t, err)
		assert.False(t, res.Removed)
		_, err = os.Stat(filepath.Join(projectDir, "settings.yaml"))
		assert.True(t, os.IsNotExist(err), "no settings file is created")
	})
}

// A legacy-format file that sets the key is migrated to v1 first, as config
// set migrates it, and the key is then removed.
func TestUnsetSetting_LegacyFileWithKeyIsMigrated(t *testing.T) {
	projectDir := setupSettingsViewTest(t, "", "harnesses: {}\nhub:\n  endpoint: https://legacy.example.com\n  brokerId: b-1\n")
	res, err := UnsetSetting(projectDir, "hub.endpoint", false)
	require.NoError(t, err)
	assert.True(t, res.Removed)
	got := readUnsetSettingsFile(t, projectDir)
	assert.Contains(t, got, "schema_version")
	assert.NotContains(t, got, "endpoint")
	assert.Contains(t, got, "broker_id: b-1", "the other keys are migrated, not lost")
}

// A JSON settings file is edited as data.
func TestUnsetSetting_JSONFile(t *testing.T) {
	projectDir := setupSettingsViewTest(t, "", "")
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "settings.json"),
		[]byte(`{"schema_version": "1", "cli": {"autohelp": false}, "active_profile": "remote"}`), 0o644))
	res, err := UnsetSetting(projectDir, "cli.autohelp", false)
	require.NoError(t, err)
	assert.True(t, res.Removed)
	got := readUnsetSettingsFile(t, projectDir)
	assert.NotContains(t, got, "autohelp")
	assert.NotContains(t, got, `"cli"`)
	assert.Contains(t, got, `"active_profile": "remote"`)
}

// The write policy applies first: a key no layer reads, a global-only key
// at the project layer, and a key config set does not accept are refused,
// and nothing is written.
func TestUnsetSetting_RefusedKeys(t *testing.T) {
	content := "schema_version: \"1\"\nactive_profile: remote\n"
	projectDir := setupSettingsViewTest(t, "", content)
	for _, key := range []string{"hub.token", "server.auth.email", "hub_connections.main.endpoint", "no.such.key"} {
		_, err := UnsetSetting(projectDir, key, false)
		require.Error(t, err, key)
	}
	_, err := UnsetSetting(projectDir, "hub.token", false)
	assert.True(t, errors.Is(err, ErrSettingNotWritable))
	assert.Equal(t, content, readUnsetSettingsFile(t, projectDir))
}

// --global edits the global file and leaves the project file alone; an
// environment variable that sets the key still applies afterwards.
func TestUnsetSetting_GlobalAndEnvPinned(t *testing.T) {
	projectDir := setupSettingsViewTest(t,
		"schema_version: \"1\"\nhub:\n  endpoint: https://g.example.com\n",
		"schema_version: \"1\"\nhub:\n  endpoint: https://p.example.com\n")
	t.Setenv("SCION_HUB_ENDPOINT", "https://env.example.com")

	res, err := UnsetSetting("", "hub.endpoint", true)
	require.NoError(t, err)
	assert.True(t, res.Removed)
	globalDir, err := GetGlobalDir()
	require.NoError(t, err)
	assert.NotContains(t, readUnsetSettingsFile(t, globalDir), "endpoint")
	assert.Contains(t, readUnsetSettingsFile(t, projectDir), "https://p.example.com")

	v, err := ResolveSettingValue(projectDir, "hub.endpoint")
	require.NoError(t, err)
	assert.Equal(t, "https://env.example.com", v.Value)
	assert.Equal(t, "env SCION_HUB_ENDPOINT", v.Source)
}

// Unsetting the hub project ID removes hub.project_id from the project's
// settings (for a project with a project-id file, its external settings
// directory, where config set writes too); the project-id file is left
// alone and still sets the value.
func TestUnsetSetting_ProjectIDFileStays(t *testing.T) {
	projectDir := setupSettingsViewTest(t, "", "schema_version: \"1\"\n")
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "project-id"), []byte("file-id\n"), 0o644))
	require.NoError(t, UpdateSetting(projectDir, "hub.project_id", "settings-id", false))
	settingsDir := GetProjectConfigDir(projectDir)
	require.Contains(t, readUnsetSettingsFile(t, settingsDir), "settings-id")

	res, err := UnsetSetting(projectDir, "project_id", false)
	require.NoError(t, err)
	assert.True(t, res.Removed)
	assert.NotContains(t, readUnsetSettingsFile(t, settingsDir), "settings-id")
	_, err = os.Stat(filepath.Join(projectDir, "project-id"))
	require.NoError(t, err)

	v, err := ResolveSettingValue(projectDir, "hub.project_id")
	require.NoError(t, err)
	assert.Equal(t, "file-id", v.Value)
	assert.True(t, strings.HasPrefix(v.Source, "project-id file "), v.Source)
}

// When an alias is on the path, the in-place edit refuses and the file is
// rewritten from its data, read again under the lock, and only when the
// result still loads.
func TestUnsetSetting_AliasOnPathRewritesFromData(t *testing.T) {
	content := "schema_version: \"1\"\nbase: &b\n  autohelp: false\n  mode: agent\ncli: *b\nactive_profile: remote\n"
	projectDir := setupSettingsViewTest(t, "", content)

	res, err := UnsetSetting(projectDir, "cli.autohelp", false)
	require.NoError(t, err)
	assert.True(t, res.Removed)
	got := readUnsetSettingsFile(t, projectDir)
	assert.Contains(t, got, "cli:\n    mode: agent\n", "cli keeps its other key, without autohelp")
	assert.Contains(t, got, "active_profile: remote")

	v, err := ResolveSettingValue(projectDir, "cli.autohelp")
	require.NoError(t, err)
	assert.Equal(t, "default", v.Source)
}

// A JSON settings file keeps the values as written: &, < and > are not
// HTML-escaped.
func TestUnsetSetting_JSONKeepsValuesUnescaped(t *testing.T) {
	projectDir := setupSettingsViewTest(t, "", "")
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "settings.json"),
		[]byte(`{"schema_version": "1", "cli": {"autohelp": false}, "hub": {"endpoint": "https://h.example.com/?a=1&b=<2>"}}`), 0o644))
	_, err := UnsetSetting(projectDir, "cli.autohelp", false)
	require.NoError(t, err)
	got := readUnsetSettingsFile(t, projectDir)
	assert.Contains(t, got, "https://h.example.com/?a=1&b=<2>")
	assert.NotContains(t, got, `\u0026`)
}
