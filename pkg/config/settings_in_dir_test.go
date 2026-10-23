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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUpdateSettingInDir_WritesDirWithProjectIDInPlace: a project
// configuration directory that holds a project-id file (the shared-backend
// layout <ProjectRoot>/.scion) is written in place. UpdateSetting would
// treat it as a split git project and redirect the write to
// ~/.scion/project-configs; UpdateSettingInDir never does.
func TestUpdateSettingInDir_WritesDirWithProjectIDInPlace(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	const projectID = "aaaaaaaa-0000-4000-8000-0000000000a1"
	dir := filepath.Join(t.TempDir(), "projects", projectID, ".scion")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, projectkeys.ProjectIDFile), []byte(projectID+"\n"), 0o644))
	defaults, err := GetProjectDefaultSettingsYAML()
	require.NoError(t, err)
	require.NoError(t, CreateSettingsFileIfMissing(filepath.Join(dir, "settings.yaml"), defaults))

	require.NoError(t, UpdateSettingInDir(dir, "hub.enabled", "true"))
	require.NoError(t, UpdateSettingInDir(dir, "hub.endpoint", "https://hub.example.com"))
	require.NoError(t, UpdateSettingInDir(dir, projectkeys.ConfigHubProjectIDKey, projectID))

	data, err := os.ReadFile(filepath.Join(dir, "settings.yaml"))
	require.NoError(t, err)
	assert.Contains(t, string(data), "https://hub.example.com")
	assert.Contains(t, string(data), projectID)
	assert.Contains(t, string(data), `schema_version: "1"`, "the file stays in the v1 format")
	_, err = os.Stat(filepath.Join(home, GlobalDir, ProjectConfigsDir))
	assert.True(t, os.IsNotExist(err), "nothing is written to the external project config directory")
}

func TestUpdateSettingInDir_CreatesV1AndRefusesBadInput(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	require.NoError(t, UpdateSettingInDir(dir, "hub.enabled", "true"), "a missing settings file is created")
	data, err := os.ReadFile(filepath.Join(dir, "settings.yaml"))
	require.NoError(t, err)
	assert.Contains(t, string(data), `schema_version: "1"`)

	require.Error(t, UpdateSettingInDir("", "hub.enabled", "true"), "a directory is required")
	require.Error(t, UpdateSettingInDir(dir, "no.such.key", "x"), "an unknown key is refused")

	legacy := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(legacy, "settings.json"), []byte(`{"active_profile":"local"}`), 0o644))
	require.Error(t, UpdateSettingInDir(legacy, "hub.enabled", "true"), "a legacy-format file is refused")
}
