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

// TestBuildHubServerConfig_CopiesInternalURL pins the copy of
// server.hub.internal_url (ptone/scion#555) into the hub's ServerConfig,
// and that it stays empty when unset.
func TestBuildHubServerConfig_CopiesInternalURL(t *testing.T) {
	cfg := &config.GlobalConfig{}
	hubCfg := buildHubServerConfig(cfg, "https://hub.example.com", "", nil, false, "", nil)
	assert.Empty(t, hubCfg.InternalURL)

	cfg.Hub.InternalURL = "http://127.0.0.1:8080"
	hubCfg = buildHubServerConfig(cfg, "https://hub.example.com", "", nil, false, "", nil)
	assert.Equal(t, "https://hub.example.com", hubCfg.HubEndpoint)
	assert.Equal(t, "http://127.0.0.1:8080", hubCfg.InternalURL)
}

// TestValidateServerPreflight_InternalURL: a hub process validates and
// normalizes server.hub.internal_url at startup.
func TestValidateServerPreflight_InternalURL(t *testing.T) {
	t.Cleanup(resetServerFlags)

	resetServerFlags()
	enableHub = true
	cfg := &config.GlobalConfig{}
	cfg.Hub.InternalURL = "http://u:supersecret@127.0.0.1:8080"
	err := validateServerPreflight(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), config.HubInternalURLKey)
	assert.NotContains(t, err.Error(), "supersecret")

	cfg.Hub.InternalURL = "HTTP://127.0.0.1:8080/"
	require.NoError(t, validateServerPreflight(cfg))
	assert.Equal(t, "http://127.0.0.1:8080", cfg.Hub.InternalURL)

	cfg.Hub.InternalURL = ""
	require.NoError(t, validateServerPreflight(cfg))
	assert.Empty(t, cfg.Hub.InternalURL)
}
