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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestValidateServerPreflight_PortProxyResponseHeaderTimeout: an
// out-of-range or malformed server.hub.port_proxy.response_header_timeout
// fails hub startup with a message naming the setting and its range; an
// unset or in-range value starts.
func TestValidateServerPreflight_PortProxyResponseHeaderTimeout(t *testing.T) {
	t.Cleanup(resetServerFlags)
	for _, tc := range []struct {
		value   string
		wantErr string
	}{
		{value: ""},
		{value: "5s"},
		{value: "10m"},
		{value: "4s", wantErr: "must be between 5s and 10m0s"},
		{value: "11m", wantErr: "must be between 5s and 10m0s"},
		{value: "0s", wantErr: "must be between 5s and 10m0s"},
		{value: "soon", wantErr: "invalid server.hub.port_proxy.response_header_timeout"},
	} {
		t.Run("value "+tc.value, func(t *testing.T) {
			resetServerFlags()
			enableHub = true
			cfg := &config.GlobalConfig{}
			cfg.Hub.PortProxy.ResponseHeaderTimeout = tc.value
			err := validateServerPreflight(cfg)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), "server.hub.port_proxy.response_header_timeout")
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// TestBuildHubServerConfig_PortProxyResponseHeaderTimeout: the setting
// reaches the hub ServerConfig, defaulting to 60s.
func TestBuildHubServerConfig_PortProxyResponseHeaderTimeout(t *testing.T) {
	cfg := &config.GlobalConfig{}
	assert.Equal(t, 60*time.Second, buildHubServerConfig(cfg, "https://hub.example.com", "", nil, false, "", nil).PortProxyResponseHeaderTimeout)
	cfg.Hub.PortProxy.ResponseHeaderTimeout = "90s"
	assert.Equal(t, 90*time.Second, buildHubServerConfig(cfg, "https://hub.example.com", "", nil, false, "", nil).PortProxyResponseHeaderTimeout)
}
