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
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func relayRoleEnvVar(t *testing.T) string {
	t.Helper()
	data, err := schemasFS.ReadFile(settingsSchemaFiles["1"])
	require.NoError(t, err)
	var root map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &root))
	prop := findSchemaProperty(t, root, "server", "database", "relay_role")
	envVar, _ := prop["x-env-var"].(string)
	require.Equal(t, "SCION_SERVER_DATABASE_RELAYROLE", envVar)
	return envVar
}

func TestLoadGlobalConfig_RelayRole(t *testing.T) {
	envVar := relayRoleEnvVar(t)

	writeSettings := func(t *testing.T, dir, database string) string {
		t.Helper()
		path := filepath.Join(dir, "settings.yaml")
		content := "schema_version: \"1\"\nserver:\n  hub:\n    port: 9999\n  database:\n    driver: postgres\n" + database
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
		return path
	}

	t.Run("default empty", func(t *testing.T) {
		tmp := t.TempDir()
		t.Setenv("HOME", tmp)
		t.Setenv(envVar, "")
		cfg, err := LoadGlobalConfig(writeSettings(t, tmp, ""))
		require.NoError(t, err)
		assert.Equal(t, 9999, cfg.Hub.Port)
		assert.Empty(t, cfg.Database.RelayRole)
	})

	t.Run("settings.yaml key", func(t *testing.T) {
		tmp := t.TempDir()
		t.Setenv("HOME", tmp)
		t.Setenv(envVar, "")
		cfg, err := LoadGlobalConfig(writeSettings(t, tmp, "    relay_role: tfha-gke-h3-relay\n"))
		require.NoError(t, err)
		assert.Equal(t, 9999, cfg.Hub.Port)
		assert.Equal(t, "tfha-gke-h3-relay", cfg.Database.RelayRole)
	})

	t.Run("env var with settings.yaml", func(t *testing.T) {
		tmp := t.TempDir()
		t.Setenv("HOME", tmp)
		t.Setenv(envVar, "tfha-gke-h3-relay")
		cfg, err := LoadGlobalConfig(writeSettings(t, tmp, ""))
		require.NoError(t, err)
		assert.Equal(t, 9999, cfg.Hub.Port)
		assert.Equal(t, "tfha-gke-h3-relay", cfg.Database.RelayRole)
	})

	t.Run("env var without settings.yaml", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		t.Setenv(envVar, "tfha-gke-h3-relay")
		cfg, err := LoadGlobalConfig("")
		require.NoError(t, err)
		assert.Equal(t, "tfha-gke-h3-relay", cfg.Database.RelayRole)
	})

	t.Run("env var is a recognized setting", func(t *testing.T) {
		assert.Empty(t, FindUnmatchedSettingsEnv([]string{envVar + "=tfha-gke-h3-relay"}, nil))
	})
}

func TestRelayRole_V1RoundTrip(t *testing.T) {
	gc := ConvertV1ServerToGlobalConfig(&V1ServerConfig{
		Database: &V1DatabaseConfig{Driver: "postgres", RelayRole: "tfha-gke-h3-relay"},
	})
	assert.Equal(t, "tfha-gke-h3-relay", gc.Database.RelayRole)

	v1 := ConvertGlobalToV1ServerConfig(gc)
	require.NotNil(t, v1.Database)
	assert.Equal(t, "tfha-gke-h3-relay", v1.Database.RelayRole)
}
