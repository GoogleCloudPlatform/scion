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

package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// allowShadowedSchemaEnv is the environment spelling of
// server.database.allow_shadowed_schema.
const allowShadowedSchemaEnv = "SCION_SERVER_DATABASE_ALLOWSHADOWEDSCHEMA"

// TestServerMigrateAllowShadowedSchema covers how server migrate reads the
// search_path guard override (ptone/scion#4432). Before the guard, server
// migrate loaded no server config at all, so a missing settings file must
// still work and give the default, and the environment override must work
// without one.
func TestServerMigrateAllowShadowedSchema(t *testing.T) {
	// isolate gives the test an empty HOME and working directory (config
	// loading also reads ./server.yaml) and the real server migrate
	// --config value, which is always empty because the flag is registered
	// on server start only.
	isolate := func(t *testing.T) string {
		t.Helper()
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Chdir(t.TempDir())
		orig := serverConfigPath
		t.Cleanup(func() { serverConfigPath = orig })
		serverConfigPath = ""
		return home
	}

	t.Run("no settings file gives the default", func(t *testing.T) {
		isolate(t)
		allow, err := serverMigrateAllowShadowedSchema()
		require.NoError(t, err)
		assert.False(t, allow)
	})

	t.Run("environment override without a settings file", func(t *testing.T) {
		isolate(t)
		t.Setenv(allowShadowedSchemaEnv, "true")
		allow, err := serverMigrateAllowShadowedSchema()
		require.NoError(t, err)
		assert.True(t, allow)
	})

	t.Run("settings file override", func(t *testing.T) {
		home := isolate(t)
		dir := filepath.Join(home, config.GlobalDir)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte(
			"schema_version: \"1\"\nserver:\n  database:\n    driver: postgres\n    allow_shadowed_schema: true\n"), 0o600))
		allow, err := serverMigrateAllowShadowedSchema()
		require.NoError(t, err)
		assert.True(t, allow)
	})

	// A server config that does not load now fails server migrate, as it
	// fails server start. One such config: a legacy server.yaml with
	// runtimeBroker.instances.
	t.Run("config that does not load fails", func(t *testing.T) {
		home := isolate(t)
		dir := filepath.Join(home, config.GlobalDir)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "server.yaml"), []byte(
			"runtimeBroker:\n  enabled: true\n  instances:\n    - key: local-docker\n      name: n\n      runtimeTarget:\n        type: docker\n"), 0o600))
		_, err := serverMigrateAllowShadowedSchema()
		require.Error(t, err)
		assert.True(t, errors.Is(err, config.ErrRuntimeBrokerInstancesInServerYAML), "got %v", err)
	})
}
