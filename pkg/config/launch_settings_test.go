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

// This file covers the three async-launch settings
// (async_agent_launch, launch_timeout, launch_keepalive_seconds) round-trip
// through V1<->Global config conversion, and that their documented
// SCION_SERVER_HUB_* environment variables actually reach GlobalConfig via
// the server-startup path (LoadGlobalConfig -> applyEnvOverrides ->
// envKeyToConfigKey), both with and without a settings.yaml `server:` key
// present. The env var names are read from the schema itself (not
// hard-coded) so a future rename that isn't also reflected in the loader
// path fails this test, rather than silently reintroducing the mismatch
// this file was added to catch.
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// schemaEnvVar reads the x-env-var for server.hub.<field> from the v1
// settings schema, using the same findSchemaProperty helper as
// TestSchema_DefaultUserRoleEnvVarMapsToOwnKey.
func schemaEnvVar(t *testing.T, field string) string {
	t.Helper()
	data, err := schemasFS.ReadFile(settingsSchemaFiles["1"])
	require.NoError(t, err)
	var root map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &root))
	prop := findSchemaProperty(t, root, "server", "hub", field)
	envVar, _ := prop["x-env-var"].(string)
	require.NotEmpty(t, envVar, "schema: server.hub.%s has no x-env-var", field)
	return envVar
}

func TestLoadGlobalConfig_AsyncLaunchEnvVars(t *testing.T) {
	asyncEnvVar := schemaEnvVar(t, "async_agent_launch")
	timeoutEnvVar := schemaEnvVar(t, "launch_timeout")
	keepaliveEnvVar := schemaEnvVar(t, "launch_keepalive_seconds")

	setEnv := func(t *testing.T) {
		t.Helper()
		t.Setenv(asyncEnvVar, "true")
		t.Setenv(timeoutEnvVar, "10m")
		t.Setenv(keepaliveEnvVar, "20")
	}
	assertApplied := func(t *testing.T, cfg *GlobalConfig) {
		t.Helper()
		assert.True(t, cfg.Hub.AsyncAgentLaunch, "%s should set Hub.AsyncAgentLaunch", asyncEnvVar)
		assert.Equal(t, 10*time.Minute, cfg.Hub.LaunchTimeout, "%s should set Hub.LaunchTimeout", timeoutEnvVar)
		assert.Equal(t, 20, cfg.Hub.LaunchKeepaliveSeconds, "%s should set Hub.LaunchKeepaliveSeconds", keepaliveEnvVar)
	}

	t.Run("without a settings.yaml (legacy env-only path)", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		setEnv(t)

		cfg, err := LoadGlobalConfig("")
		require.NoError(t, err)
		assertApplied(t, cfg)
	})

	t.Run("with a settings.yaml server key (applyEnvOverrides path)", func(t *testing.T) {
		tmpDir := t.TempDir()
		t.Setenv("HOME", tmpDir)
		configPath := filepath.Join(tmpDir, "settings.yaml")
		configContent := `
schema_version: "1"
server:
  hub:
    port: 9999
`
		require.NoError(t, os.WriteFile(configPath, []byte(configContent), 0644))
		setEnv(t)

		cfg, err := LoadGlobalConfig(configPath)
		require.NoError(t, err)
		assertApplied(t, cfg)
		// Confirms this went through the settings.yaml path, not the legacy
		// fallback, so the assertion above is exercising applyEnvOverrides.
		assert.Equal(t, 9999, cfg.Hub.Port)
	})
}

func TestConvertV1ServerToGlobalConfig_AsyncLaunchSettings(t *testing.T) {
	asyncLaunch := true
	keepalive := 20
	v1 := &V1ServerConfig{
		Hub: &V1ServerHubConfig{
			AsyncAgentLaunch:       &asyncLaunch,
			LaunchTimeout:          "10m",
			LaunchKeepaliveSeconds: &keepalive,
		},
	}
	gc := ConvertV1ServerToGlobalConfig(v1)
	assert.True(t, gc.Hub.AsyncAgentLaunch)
	assert.Equal(t, 10*time.Minute, gc.Hub.LaunchTimeout)
	assert.Equal(t, 20, gc.Hub.LaunchKeepaliveSeconds)
}

func TestConvertV1ServerToGlobalConfig_LaunchTimeoutInvalidIgnored(t *testing.T) {
	v1 := &V1ServerConfig{
		Hub: &V1ServerHubConfig{
			LaunchTimeout: "not-a-duration",
		},
	}
	gc := ConvertV1ServerToGlobalConfig(v1)
	// Invalid duration string is ignored (consistent with stalled_threshold);
	// LaunchTimeout stays at its zero value here. The Hub-side minimum clamp
	// (New(), server.go) is what actually prevents a too-small or absent
	// value from reaching production, not this conversion step.
	assert.Equal(t, time.Duration(0), gc.Hub.LaunchTimeout)
}

func TestConvertV1ServerToGlobalConfig_AsyncLaunchSettingsAbsent(t *testing.T) {
	v1 := &V1ServerConfig{Hub: &V1ServerHubConfig{}}
	gc := ConvertV1ServerToGlobalConfig(v1)
	assert.False(t, gc.Hub.AsyncAgentLaunch)
	assert.Equal(t, time.Duration(0), gc.Hub.LaunchTimeout)
	assert.Equal(t, 0, gc.Hub.LaunchKeepaliveSeconds)
}

func TestConvertGlobalToV1ServerConfig_AsyncLaunchSettingsRoundTrip(t *testing.T) {
	gc := &GlobalConfig{}
	gc.Hub.AsyncAgentLaunch = true
	gc.Hub.LaunchTimeout = 7 * time.Minute
	gc.Hub.LaunchKeepaliveSeconds = 25

	v1 := ConvertGlobalToV1ServerConfig(gc)
	require := assert.New(t)
	require.NotNil(v1.Hub)
	require.NotNil(v1.Hub.AsyncAgentLaunch)
	require.True(*v1.Hub.AsyncAgentLaunch)
	require.Equal("7m0s", v1.Hub.LaunchTimeout)
	require.NotNil(v1.Hub.LaunchKeepaliveSeconds)
	require.Equal(25, *v1.Hub.LaunchKeepaliveSeconds)

	// Round trip back.
	gc2 := ConvertV1ServerToGlobalConfig(v1)
	require.Equal(gc.Hub.AsyncAgentLaunch, gc2.Hub.AsyncAgentLaunch)
	require.Equal(gc.Hub.LaunchTimeout, gc2.Hub.LaunchTimeout)
	require.Equal(gc.Hub.LaunchKeepaliveSeconds, gc2.Hub.LaunchKeepaliveSeconds)
}

func TestConvertGlobalToV1ServerConfig_AsyncLaunchSettingsZeroOmitted(t *testing.T) {
	gc := &GlobalConfig{}
	v1 := ConvertGlobalToV1ServerConfig(gc)
	require := assert.New(t)
	require.NotNil(v1.Hub)
	require.Nil(v1.Hub.AsyncAgentLaunch, "false must not round-trip as an explicit *bool")
	require.Empty(v1.Hub.LaunchTimeout)
	require.Nil(v1.Hub.LaunchKeepaliveSeconds)
}
