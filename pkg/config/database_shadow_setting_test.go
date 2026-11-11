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

// allowShadowedSchemaEnvVar reads the documented env var for
// server.database.allow_shadowed_schema from the v1 settings schema, so a
// rename that is not also reflected in the loader fails here.
func allowShadowedSchemaEnvVar(t *testing.T) string {
	t.Helper()
	data, err := schemasFS.ReadFile(settingsSchemaFiles["1"])
	require.NoError(t, err)
	var root map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &root))
	prop := findSchemaProperty(t, root, "server", "database", "allow_shadowed_schema")
	envVar, _ := prop["x-env-var"].(string)
	require.Equal(t, "SCION_SERVER_DATABASE_ALLOWSHADOWEDSCHEMA", envVar)
	return envVar
}

// TestLoadGlobalConfig_AllowShadowedSchema covers the PostgreSQL search_path
// shadow override: off by default, set from settings.yaml, and set from its
// environment variable on both the settings.yaml and the legacy path.
func TestLoadGlobalConfig_AllowShadowedSchema(t *testing.T) {
	envVar := allowShadowedSchemaEnvVar(t)

	writeSettings := func(t *testing.T, dir, database string) string {
		t.Helper()
		path := filepath.Join(dir, "settings.yaml")
		content := "schema_version: \"1\"\nserver:\n  hub:\n    port: 9999\n  database:\n    driver: postgres\n" + database
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
		return path
	}

	t.Run("default off", func(t *testing.T) {
		tmp := t.TempDir()
		t.Setenv("HOME", tmp)
		t.Setenv(envVar, "")
		cfg, err := LoadGlobalConfig(writeSettings(t, tmp, ""))
		require.NoError(t, err)
		assert.Equal(t, 9999, cfg.Hub.Port, "must load through the settings.yaml path")
		assert.False(t, cfg.Database.AllowShadowedSchema)
	})

	t.Run("settings.yaml key", func(t *testing.T) {
		tmp := t.TempDir()
		t.Setenv("HOME", tmp)
		t.Setenv(envVar, "")
		cfg, err := LoadGlobalConfig(writeSettings(t, tmp, "    allow_shadowed_schema: true\n"))
		require.NoError(t, err)
		assert.Equal(t, 9999, cfg.Hub.Port, "must load through the settings.yaml path")
		assert.True(t, cfg.Database.AllowShadowedSchema)
	})

	t.Run("env var with settings.yaml", func(t *testing.T) {
		tmp := t.TempDir()
		t.Setenv("HOME", tmp)
		t.Setenv(envVar, "true")
		cfg, err := LoadGlobalConfig(writeSettings(t, tmp, ""))
		require.NoError(t, err)
		assert.Equal(t, 9999, cfg.Hub.Port, "must load through the settings.yaml path")
		assert.True(t, cfg.Database.AllowShadowedSchema)
	})

	t.Run("env var without settings.yaml", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		t.Setenv(envVar, "true")
		cfg, err := LoadGlobalConfig("")
		require.NoError(t, err)
		assert.True(t, cfg.Database.AllowShadowedSchema)
	})

	t.Run("env var is a recognized setting", func(t *testing.T) {
		assert.Empty(t, FindUnmatchedSettingsEnv([]string{envVar + "=true"}, nil))
	})
}

// TestAllowShadowedSchema_V1RoundTrip checks the V1 <-> GlobalConfig
// conversion keeps the setting in both directions.
func TestAllowShadowedSchema_V1RoundTrip(t *testing.T) {
	gc := ConvertV1ServerToGlobalConfig(&V1ServerConfig{
		Database: &V1DatabaseConfig{Driver: "postgres", AllowShadowedSchema: true},
	})
	assert.True(t, gc.Database.AllowShadowedSchema)

	v1 := ConvertGlobalToV1ServerConfig(gc)
	require.NotNil(t, v1.Database)
	assert.True(t, v1.Database.AllowShadowedSchema)
}
