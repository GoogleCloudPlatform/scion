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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHubSystemOneConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    time.Duration
		wantErr []string
	}{
		{name: "unset is 3s default", value: "", want: 3 * time.Second},
		{name: "sub-second", value: "250ms", want: 250 * time.Millisecond},
		{name: "explicit default", value: "3s", want: 3 * time.Second},
		{name: "at hard max 30s", value: "30s", want: 30 * time.Second},
		{name: "above hard max", value: "31s", wantErr: []string{"server.hub.system_one.max_eval_time", "<= 30s"}},
		{name: "zero", value: "0s", wantErr: []string{"server.hub.system_one.max_eval_time", "> 0"}},
		{name: "bare zero", value: "0", wantErr: []string{"server.hub.system_one.max_eval_time", "> 0"}},
		{name: "negative", value: "-1s", wantErr: []string{"server.hub.system_one.max_eval_time", "> 0"}},
		{name: "malformed", value: "three seconds", wantErr: []string{"server.hub.system_one.max_eval_time", "three seconds"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := HubSystemOneConfig{MaxEvalTime: tt.value}
			d, err := c.MaxEvalTimeDuration()
			verr := c.Validate()
			if tt.wantErr == nil {
				require.NoError(t, err)
				require.NoError(t, verr)
				assert.Equal(t, tt.want, d)
				return
			}
			require.Error(t, err)
			require.Error(t, verr)
			for _, sub := range tt.wantErr {
				assert.Contains(t, verr.Error(), sub)
			}
		})
	}
	assert.Equal(t, 3*time.Second, SystemOneDefaultMaxEvalTime)
	assert.Equal(t, 30*time.Second, SystemOneMaxEvalTimeLimit)
	assert.True(t, HubSystemOneConfig{}.IsZero())
	assert.False(t, HubSystemOneConfig{MaxEvalTime: "5s"}.IsZero())
}

func TestHubSystemOneConfig_EnvAndSchemaMapping(t *testing.T) {
	data, err := schemasFS.ReadFile(settingsSchemaFiles["1"])
	require.NoError(t, err)
	var root map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &root))
	prop := findSchemaProperty(t, root, "server", "hub", "system_one", "max_eval_time")
	envVar, _ := prop["x-env-var"].(string)

	const env = "SCION_SERVER_HUB_SYSTEMONE_MAXEVALTIME"
	assert.Equal(t, env, envVar)
	assert.Equal(t, "server.hub.system_one.max_eval_time", serverEnvToOpsettingsKey("HUB_SYSTEMONE_MAXEVALTIME"))
	assert.Equal(t, "server.hub.system_one.max_eval_time", serverEnvToOpsettingsKey("HUB_SYSTEM_ONE_MAX_EVAL_TIME"))
	assert.Equal(t, "hub.systemOne.maxEvalTime", envKeyToConfigKey("HUB_SYSTEMONE_MAXEVALTIME"))
	assert.Equal(t, "hub.systemOne.maxEvalTime", envKeyToConfigKey("HUB_SYSTEM_ONE_MAX_EVAL_TIME"))
	assert.Equal(t, "server.hub.system_one.max_eval_time", versionedEnvKeyMapper("SCION_SERVER_HUB_SYSTEM_ONE_MAX_EVAL_TIME"))

	t.Setenv(env, "5s")
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	configPath := filepath.Join(tmpDir, "settings.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte("schema_version: \"1\"\nserver:\n  hub:\n    system_one:\n      max_eval_time: 10s\n"), 0644))

	cfg, err := LoadGlobalConfig(configPath)
	require.NoError(t, err)
	assert.Equal(t, "5s", cfg.Hub.SystemOne.MaxEvalTime, "env overrides file")
	d, err := cfg.Hub.SystemOne.MaxEvalTimeDuration()
	require.NoError(t, err)
	assert.Equal(t, 5*time.Second, d)
}

func TestHubSystemOneConfig_V1RoundTripAndSchemaValidation(t *testing.T) {
	gc := &GlobalConfig{}
	gc.Hub.SystemOne.MaxEvalTime = "4s"
	v1 := ConvertGlobalToV1ServerConfig(gc)
	require.NotNil(t, v1.Hub)
	require.NotNil(t, v1.Hub.SystemOne)
	assert.Equal(t, "4s", v1.Hub.SystemOne.MaxEvalTime)

	back := ConvertV1ServerToGlobalConfig(v1)
	assert.Equal(t, "4s", back.Hub.SystemOne.MaxEvalTime)

	assert.Nil(t, ConvertGlobalToV1ServerConfig(&GlobalConfig{}).Hub.SystemOne, "unset setting is omitted")

	rawYAML := []byte("schema_version: \"1\"\nserver:\n  hub:\n    system_one:\n      max_eval_time: 15s\n")
	errs, err := ValidateSettings(rawYAML, "1")
	require.NoError(t, err)
	assert.Empty(t, errs)

	unknownYAML := []byte("schema_version: \"1\"\nserver:\n  hub:\n    system_one:\n      unknown_field: 15s\n")
	errs, err = ValidateSettings(unknownYAML, "1")
	require.NoError(t, err)
	assert.NotEmpty(t, errs, "additionalProperties: false rejects unknown fields")
}
