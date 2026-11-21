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

func TestBuildHubServerConfig_CopiesDefaultMessageMode(t *testing.T) {
	cfg := &config.GlobalConfig{}
	cfg.Hub.DefaultMessageMode = "lineage"

	hubCfg := buildHubServerConfig(cfg, "https://hub.example.com", "", nil, false, "", nil)
	assert.Equal(t, "lineage", hubCfg.DefaultMessageMode)

	emptyCfg := buildHubServerConfig(&config.GlobalConfig{}, "https://hub.example.com", "", nil, false, "", nil)
	assert.Empty(t, emptyCfg.DefaultMessageMode)
}

func TestValidateServerPreflight_DefaultMessageMode(t *testing.T) {
	t.Cleanup(resetServerFlags)

	t.Run("hub enabled, refuses hub mode", func(t *testing.T) {
		resetServerFlags()
		enableHub = true
		cfg := &config.GlobalConfig{}
		cfg.Hub.DefaultMessageMode = "hub"

		err := validateServerPreflight(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "server.hub.default_message_mode")
	})

	t.Run("hub enabled, refuses unknown mode", func(t *testing.T) {
		resetServerFlags()
		enableHub = true
		cfg := &config.GlobalConfig{}
		cfg.Hub.DefaultMessageMode = "bogus"

		err := validateServerPreflight(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "server.hub.default_message_mode")
	})

	for _, ok := range []string{"", "none", "lineage", "branch", "project"} {
		t.Run("hub enabled, accepts "+ok, func(t *testing.T) {
			resetServerFlags()
			enableHub = true
			cfg := &config.GlobalConfig{}
			cfg.Hub.DefaultMessageMode = ok

			require.NoError(t, validateServerPreflight(cfg))
		})
	}

	t.Run("broker-only mode skips hub validation", func(t *testing.T) {
		resetServerFlags()
		enableHub = false
		cfg := &config.GlobalConfig{}
		cfg.Hub.DefaultMessageMode = "hub"

		require.NoError(t, validateServerPreflight(cfg))
	})
}
