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

func observabilitySchemaEnvVar(t *testing.T, field string) string {
	t.Helper()
	data, err := schemasFS.ReadFile(settingsSchemaFiles["1"])
	require.NoError(t, err)
	var root map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &root))
	prop := findSchemaProperty(t, root, "server", "hub", "observability", field)
	envVar, _ := prop["x-env-var"].(string)
	require.NotEmpty(t, envVar, "schema: server.hub.observability.%s has no x-env-var", field)
	return envVar
}

// The env spelling of each switch is the schema's x-env-var, and it maps to
// the switch in every env mapper: the opsettings keyspace, the GlobalConfig
// camelCase keyspace and the versioned settings keyspace (canonical and
// underscored spelling).
func TestHubObservability_EnvMapping(t *testing.T) {
	for _, tc := range []struct {
		field, env, camel, underscored string
	}{
		{"metrics_enabled", "SCION_SERVER_HUB_OBSERVABILITY_METRICSENABLED", "hub.observability.metricsEnabled", "SCION_SERVER_HUB_OBSERVABILITY_METRICS_ENABLED"},
		{"tracing_enabled", "SCION_SERVER_HUB_OBSERVABILITY_TRACINGENABLED", "hub.observability.tracingEnabled", "SCION_SERVER_HUB_OBSERVABILITY_TRACING_ENABLED"},
	} {
		t.Run(tc.field, func(t *testing.T) {
			key := "server.hub.observability." + tc.field
			assert.Equal(t, tc.env, observabilitySchemaEnvVar(t, tc.field))
			suffix := tc.env[len("SCION_SERVER_"):]
			assert.Equal(t, key, serverEnvToOpsettingsKey(suffix))
			assert.Equal(t, tc.camel, envKeyToConfigKey(suffix))
			assert.Equal(t, key, versionedEnvKeyMapper(tc.env))
			assert.Equal(t, key, versionedEnvKeyMapper(tc.underscored))
		})
	}
	assert.Equal(t, HubMetricsEnabledKey, "server.hub.observability.metrics_enabled")
	assert.Equal(t, HubTracingEnabledKey, "server.hub.observability.tracing_enabled")
}

// The switches load from settings.yaml, an env value overrides the file,
// and an absent switch stays unset (nil), which keeps the earlier rule.
func TestLoadGlobalConfig_HubObservability(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	configPath := filepath.Join(tmpDir, "settings.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte("schema_version: \"1\"\nserver:\n  hub:\n    observability:\n      metrics_enabled: false\n"), 0644))

	cfg, err := LoadGlobalConfig(configPath)
	require.NoError(t, err)
	require.NotNil(t, cfg.Hub.Observability.MetricsEnabled)
	assert.False(t, *cfg.Hub.Observability.MetricsEnabled)
	assert.Nil(t, cfg.Hub.Observability.TracingEnabled, "an absent switch stays unset")

	t.Setenv("SCION_SERVER_HUB_OBSERVABILITY_METRICSENABLED", "true")
	t.Setenv("SCION_SERVER_HUB_OBSERVABILITY_TRACINGENABLED", "false")
	cfg, err = LoadGlobalConfig(configPath)
	require.NoError(t, err)
	require.NotNil(t, cfg.Hub.Observability.MetricsEnabled)
	assert.True(t, *cfg.Hub.Observability.MetricsEnabled, "the env var overrides the file")
	require.NotNil(t, cfg.Hub.Observability.TracingEnabled)
	assert.False(t, *cfg.Hub.Observability.TracingEnabled)

	k := LoadEnvKoanf()
	assert.True(t, k.Bool(HubMetricsEnabledKey), "keys: %v", k.Keys())
	assert.True(t, k.Exists(HubTracingEnabledKey))
	assert.False(t, k.Bool(HubTracingEnabledKey))
}

// The v1 settings struct and GlobalConfig carry the switches both ways
// without sharing storage.
func TestHubObservability_V1RoundTrip(t *testing.T) {
	on, off := true, false
	gc := &GlobalConfig{}
	gc.Hub.Observability = HubObservabilityConfig{MetricsEnabled: &on, TracingEnabled: &off}

	v1 := ConvertGlobalToV1ServerConfig(gc)
	require.NotNil(t, v1.Hub.Observability)
	require.NotNil(t, v1.Hub.Observability.MetricsEnabled)
	require.NotNil(t, v1.Hub.Observability.TracingEnabled)
	assert.True(t, *v1.Hub.Observability.MetricsEnabled)
	assert.False(t, *v1.Hub.Observability.TracingEnabled)
	on = false
	assert.True(t, *v1.Hub.Observability.MetricsEnabled, "the v1 value must not share storage")

	assert.Nil(t, ConvertGlobalToV1ServerConfig(&GlobalConfig{}).Hub.Observability, "unset switches are omitted")
	assert.True(t, HubObservabilityConfig{}.IsZero())
	assert.False(t, HubObservabilityConfig{TracingEnabled: &off}.IsZero())
}
