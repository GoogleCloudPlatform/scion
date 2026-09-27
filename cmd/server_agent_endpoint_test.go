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

// TestBuildHubServerConfig_CopiesAgentEndpoint pins the field copy that would
// otherwise leave a running Hub silently ignoring server.hub.agent_endpoint:
// buildHubServerConfig is the single place that turns GlobalConfig into the
// hub.ServerConfig a real Hub is constructed from.
func TestBuildHubServerConfig_CopiesAgentEndpoint(t *testing.T) {
	cfg := &config.GlobalConfig{}
	cfg.Hub.AgentEndpoint = "http://192.0.2.10:8080"

	hubCfg := buildHubServerConfig(cfg, "https://hub.example.com", "", nil, false, "", nil)

	assert.Equal(t, "https://hub.example.com", hubCfg.HubEndpoint)
	assert.Equal(t, "http://192.0.2.10:8080", hubCfg.AgentEndpoint)
}

// TestBuildHubServerConfig_AgentEndpointEmptyByDefault proves the unset case
// carries no value through.
func TestBuildHubServerConfig_AgentEndpointEmptyByDefault(t *testing.T) {
	cfg := &config.GlobalConfig{}

	hubCfg := buildHubServerConfig(cfg, "https://hub.example.com", "", nil, false, "", nil)

	assert.Equal(t, "https://hub.example.com", hubCfg.HubEndpoint)
	assert.Empty(t, hubCfg.AgentEndpoint)
}

// TestValidateServerPreflight_AgentEndpoint covers the startup wiring for
// server.hub.agent_endpoint: the Hub validates and normalizes it only when
// this process runs the Hub (enableHub == true); a broker-only process never
// installs a dispatcher and never reads the value, so it is not checked.
func TestValidateServerPreflight_AgentEndpoint(t *testing.T) {
	t.Cleanup(resetServerFlags)

	t.Run("hub enabled, invalid value errors and names the setting", func(t *testing.T) {
		resetServerFlags()
		enableHub = true
		cfg := &config.GlobalConfig{}
		cfg.Hub.AgentEndpoint = "ftp://x"

		err := validateServerPreflight(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "server.hub.agent_endpoint")
	})

	t.Run("hub enabled, invalid value with credentials never echoes them", func(t *testing.T) {
		resetServerFlags()
		enableHub = true
		cfg := &config.GlobalConfig{}
		cfg.Hub.AgentEndpoint = "http://someuser:supersecret@hub.example.com"

		err := validateServerPreflight(cfg)
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "someuser")
		assert.NotContains(t, err.Error(), "supersecret")
	})

	t.Run("hub enabled, empty value is fine", func(t *testing.T) {
		resetServerFlags()
		enableHub = true
		cfg := &config.GlobalConfig{}

		require.NoError(t, validateServerPreflight(cfg))
	})

	t.Run("hub enabled, valid value is normalized in place", func(t *testing.T) {
		resetServerFlags()
		enableHub = true
		cfg := &config.GlobalConfig{}
		cfg.Hub.AgentEndpoint = "HTTP://hub-internal.example.com/"

		require.NoError(t, validateServerPreflight(cfg))
		assert.Equal(t, "http://hub-internal.example.com", cfg.Hub.AgentEndpoint)
	})

	t.Run("broker-only mode (hub disabled) skips validation of an unused value", func(t *testing.T) {
		resetServerFlags()
		enableHub = false
		cfg := &config.GlobalConfig{}
		cfg.Hub.AgentEndpoint = "ftp://x"

		require.NoError(t, validateServerPreflight(cfg))
		assert.Equal(t, "ftp://x", cfg.Hub.AgentEndpoint, "broker-only mode must not rewrite a value it never reads")
	})
}

// TestResolveTransportAudience covers the auth.transport OIDC audience
// derivation for cloudrun_invoker mode. It must never read
// server.hub.agent_endpoint: Cloud Run's own IAM check validates minted
// tokens against the Hub's public URL, so the caller passes the resolved
// public hub endpoint, not the agent-only override.
func TestResolveTransportAudience(t *testing.T) {
	tests := []struct {
		name         string
		oidcAudience string
		mode         string
		hubEndpoint  string
		want         string
	}{
		{
			name:         "explicit audience wins regardless of mode",
			oidcAudience: "explicit-audience",
			mode:         "cloudrun_invoker",
			hubEndpoint:  "https://hub.example.com",
			want:         "explicit-audience",
		},
		{
			name:        "derives from the public hub endpoint for cloudrun_invoker",
			mode:        "cloudrun_invoker",
			hubEndpoint: "https://hub.example.com",
			want:        "https://hub.example.com",
		},
		{
			name:        "no derivation for other transport modes",
			mode:        "iap",
			hubEndpoint: "https://hub.example.com",
			want:        "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveTransportAudience(tt.oidcAudience, tt.mode, tt.hubEndpoint)
			assert.Equal(t, tt.want, got)
		})
	}
}
