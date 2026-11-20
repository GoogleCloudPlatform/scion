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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestValidateServerPreflight_SystemOneMaxEvalTime: an out-of-range or
// malformed server.hub.system_one.max_eval_time fails hub startup with a
// message naming the setting and its range; an unset or in-range value starts.
// A broker-only process skips hub-only preflight validation.
func TestValidateServerPreflight_SystemOneMaxEvalTime(t *testing.T) {
	t.Cleanup(resetServerFlags)
	for _, tc := range []struct {
		value   string
		wantErr string
	}{
		{value: ""},
		{value: "500ms"},
		{value: "3s"},
		{value: "30s"},
		{value: "0s", wantErr: "must be > 0 and <= 30s"},
		{value: "-1s", wantErr: "must be > 0 and <= 30s"},
		{value: "31s", wantErr: "must be > 0 and <= 30s"},
		{value: "soon", wantErr: "invalid server.hub.system_one.max_eval_time"},
	} {
		t.Run("value "+tc.value, func(t *testing.T) {
			resetServerFlags()
			enableHub = true
			cfg := &config.GlobalConfig{}
			cfg.Hub.SystemOne.MaxEvalTime = tc.value
			err := validateServerPreflight(cfg)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), "server.hub.system_one.max_eval_time")
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}

	t.Run("broker-only skips hub system_one check", func(t *testing.T) {
		resetServerFlags()
		enableHub = false
		cfg := &config.GlobalConfig{}
		cfg.Hub.SystemOne.MaxEvalTime = "soon"
		require.NoError(t, validateServerPreflight(cfg))
	})
}

// TestBuildHubServerConfig_SystemOneMaxEvalTime: the setting reaches the hub
// ServerConfig via StartupServerSettings when set, and stays nil when unset.
func TestBuildHubServerConfig_SystemOneMaxEvalTime(t *testing.T) {
	cfg := &config.GlobalConfig{}
	hubCfg := buildHubServerConfig(cfg, "https://hub.example.com", "", nil, false, "", nil)
	require.NotNil(t, hubCfg.StartupServerSettings)
	if hubCfg.StartupServerSettings.Hub != nil {
		assert.Nil(t, hubCfg.StartupServerSettings.Hub.SystemOne)
	}

	cfg.Hub.SystemOne.MaxEvalTime = "5s"
	cfg.Hub.SystemOne.SecretNames = map[string][]string{"gemini_decisions": {"GEMINI_API_KEY_BILLING_B"}}
	hubCfg = buildHubServerConfig(cfg, "https://hub.example.com", "", nil, false, "", nil)
	require.NotNil(t, hubCfg.StartupServerSettings)
	require.NotNil(t, hubCfg.StartupServerSettings.Hub)
	require.NotNil(t, hubCfg.StartupServerSettings.Hub.SystemOne)
	assert.Equal(t, "5s", hubCfg.StartupServerSettings.Hub.SystemOne.MaxEvalTime)
	assert.Equal(t, map[string][]string{"gemini_decisions": {"GEMINI_API_KEY_BILLING_B"}}, hubCfg.StartupServerSettings.Hub.SystemOne.SecretNames)
}
