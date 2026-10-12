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

func TestCheckDoctorServerHubEndpoint(t *testing.T) {
	server := doctorServerHost{configured: true}
	broker := doctorServerHost{configured: true, brokerOnly: true}
	tests := []struct {
		name   string
		host   doctorServerHost
		legacy string
		setup  func(cfg *config.GlobalConfig)
		want   bool
	}{
		{name: "not a server host: nothing, even with a remote hub.endpoint", host: doctorServerHost{}, legacy: "https://hub.example.com", want: false},
		{name: "server host, remote legacy value", host: server, legacy: "https://hub.example.com", want: true},
		{name: "server host, no legacy value", host: server, legacy: "", want: false},
		{name: "server host, workstation self-written loopback", host: server, legacy: "http://127.0.0.1:8080", want: false},
		{name: "server host, loopback on another port", host: server, legacy: "http://127.0.0.1:9999", want: true},
		{
			name: "server host, server keys set", host: server, legacy: "https://hub.example.com",
			setup: func(cfg *config.GlobalConfig) {
				cfg.Hub.Endpoint = "https://hub.example.com"
				cfg.RuntimeBroker.HubEndpoint = "https://hub.example.com"
			},
			want: false,
		},
		{name: "standalone broker, loopback legacy value", host: broker, legacy: "http://127.0.0.1:8080", want: true},
		{
			name: "standalone broker, server keys set", host: broker, legacy: "https://hub.example.com",
			setup: func(cfg *config.GlobalConfig) {
				cfg.Hub.Endpoint = "https://hub.example.com"
				cfg.RuntimeBroker.HubEndpoint = "https://hub.example.com"
			},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateServerHubEndpointEnv(t)
			cfg := newDeprecationTestConfig()
			if tt.setup != nil {
				tt.setup(cfg)
			}
			settings := &config.Settings{Hub: &config.HubClientConfig{Endpoint: tt.legacy}}
			res, ok := checkDoctorServerHubEndpoint(cfg, settings, tt.host, 8080)
			require.Equal(t, tt.want, ok)
			if !tt.want {
				return
			}
			assert.Equal(t, "server-hub-endpoint", res.Name)
			assert.Equal(t, "warn", res.Status)
			assert.Equal(t, serverHubEndpointDeprecationWarning, res.Message)
			assert.Empty(t, res.Remediation)
		})
	}
}

func TestDetectDoctorServerHost(t *testing.T) {
	t.Run("no settings, no daemon", func(t *testing.T) {
		globalDir := isolateServerHubEndpointEnv(t)
		assert.Equal(t, doctorServerHost{}, detectDoctorServerHost(globalDir))
	})
	t.Run("CLI-only settings", func(t *testing.T) {
		globalDir := isolateServerHubEndpointEnv(t)
		writeGlobalSettings(t, globalDir, "hub:\n  endpoint: https://hub.example.com\n")
		assert.Equal(t, doctorServerHost{}, detectDoctorServerHost(globalDir))
	})
	t.Run("server block", func(t *testing.T) {
		globalDir := isolateServerHubEndpointEnv(t)
		writeGlobalSettings(t, globalDir, "server:\n  mode: hosted\n")
		assert.Equal(t, doctorServerHost{configured: true}, detectDoctorServerHost(globalDir))
	})
}

// TestCheckDoctorServerHubEndpointHere runs the doctor entry point against
// a fresh home: a server block plus SCION_HUB_ENDPOINT reports the warning,
// and the same value on a CLI-only host reports nothing.
func TestCheckDoctorServerHubEndpointHere(t *testing.T) {
	t.Run("server host", func(t *testing.T) {
		globalDir := isolateServerHubEndpointEnv(t)
		writeGlobalSettings(t, globalDir, "server:\n  mode: hosted\n")
		t.Setenv("SCION_HUB_ENDPOINT", "https://hub.example.com")
		res, ok := checkDoctorServerHubEndpointHere()
		require.True(t, ok)
		assert.Equal(t, serverHubEndpointDeprecationWarning, res.Message)
	})
	t.Run("CLI-only host", func(t *testing.T) {
		isolateServerHubEndpointEnv(t)
		t.Setenv("SCION_HUB_ENDPOINT", "https://hub.example.com")
		_, ok := checkDoctorServerHubEndpointHere()
		assert.False(t, ok)
	})
}
