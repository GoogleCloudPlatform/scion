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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHubConduitConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     HubConduitConfig
		wantErr []string // substrings; nil = valid
	}{
		{name: "zero", cfg: HubConduitConfig{}},
		{name: "all valid", cfg: HubConduitConfig{
			GrantKeyActivation: "15m", ReconnectWindow: "10s", TCPAllowedPorts: []int{22, 3000},
			InternalListen: ":9810", InternalAdvertise: "http://10.0.0.5:9810", PeerAuth: "HMAC",
			PeerServiceAccounts: []string{"hub@p.iam.gserviceaccount.com"}, PeerAudience: "aud",
		}},
		{name: "activation at minimum", cfg: HubConduitConfig{GrantKeyActivation: "1m"}},
		{name: "activation below minimum", cfg: HubConduitConfig{GrantKeyActivation: "30s"}, wantErr: []string{"grant_key_activation", "at least 1m0s"}},
		{name: "activation malformed", cfg: HubConduitConfig{GrantKeyActivation: "soon"}, wantErr: []string{"grant_key_activation"}},
		{name: "reconnect window zero", cfg: HubConduitConfig{ReconnectWindow: "0s"}},
		{name: "reconnect window at max", cfg: HubConduitConfig{ReconnectWindow: "5m"}},
		{name: "reconnect window above max", cfg: HubConduitConfig{ReconnectWindow: "6m"}, wantErr: []string{"reconnect_window", "between 0s and 5m0s"}},
		{name: "reconnect window negative", cfg: HubConduitConfig{ReconnectWindow: "-1s"}, wantErr: []string{"reconnect_window"}},
		{name: "reconnect window malformed", cfg: HubConduitConfig{ReconnectWindow: "fast"}, wantErr: []string{"reconnect_window"}},
		{name: "port out of range", cfg: HubConduitConfig{TCPAllowedPorts: []int{0, 65536}}, wantErr: []string{"port 0 is outside", "port 65536 is outside"}},
		{name: "port duplicated", cfg: HubConduitConfig{TCPAllowedPorts: []int{22, 22}}, wantErr: []string{"port 22 is listed twice"}},
		{name: "listen without port", cfg: HubConduitConfig{InternalListen: "10.0.0.5"}, wantErr: []string{"internal_listen"}},
		{name: "listen bad port", cfg: HubConduitConfig{InternalListen: "10.0.0.5:http"}, wantErr: []string{"internal_listen"}},
		{name: "listen port too big", cfg: HubConduitConfig{InternalListen: ":70000"}, wantErr: []string{"internal_listen"}},
		{name: "advertise https", cfg: HubConduitConfig{InternalAdvertise: "https://relay.internal:9810/"}},
		{name: "advertise no scheme", cfg: HubConduitConfig{InternalAdvertise: "10.0.0.5:9810"}, wantErr: []string{"internal_advertise"}},
		{name: "advertise ws scheme", cfg: HubConduitConfig{InternalAdvertise: "ws://10.0.0.5:9810"}, wantErr: []string{"internal_advertise"}},
		{name: "advertise with path", cfg: HubConduitConfig{InternalAdvertise: "http://10.0.0.5:9810/x"}, wantErr: []string{"internal_advertise"}},
		{name: "advertise with query", cfg: HubConduitConfig{InternalAdvertise: "http://10.0.0.5:9810?a=b"}, wantErr: []string{"internal_advertise"}},
		{name: "peer auth oidc", cfg: HubConduitConfig{PeerAuth: "oidc"}},
		{name: "peer auth unknown", cfg: HubConduitConfig{PeerAuth: "mtls"}, wantErr: []string{"peer_auth"}},
		{name: "peer SA not an email", cfg: HubConduitConfig{PeerServiceAccounts: []string{"hub"}}, wantErr: []string{"peer_service_accounts"}},
		{name: "peer SA padded", cfg: HubConduitConfig{PeerServiceAccounts: []string{" hub@p.iam.gserviceaccount.com"}}, wantErr: []string{"peer_service_accounts"}},
		{name: "instance id", cfg: HubConduitConfig{InstanceID: "hub-east-1.example_0"}},
		{name: "instance id with space", cfg: HubConduitConfig{InstanceID: "hub 1"}, wantErr: []string{"instance_id"}},
		{name: "instance id non-ascii", cfg: HubConduitConfig{InstanceID: "hüb"}, wantErr: []string{"instance_id"}},
		{name: "instance id too long", cfg: HubConduitConfig{InstanceID: strings.Repeat("a", ConduitMaxInstanceIDLen+1)}, wantErr: []string{"instance_id", "at most 128"}},
		{name: "every problem reported", cfg: HubConduitConfig{GrantKeyActivation: "1s", ReconnectWindow: "1h", PeerAuth: "x"},
			wantErr: []string{"grant_key_activation", "reconnect_window", "peer_auth"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if tt.wantErr == nil {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			for _, want := range tt.wantErr {
				assert.Contains(t, err.Error(), want)
			}
		})
	}
}

func TestHubConduitConfig_Durations(t *testing.T) {
	tests := []struct {
		name           string
		cfg            HubConduitConfig
		wantActivation time.Duration
		wantWindow     time.Duration
	}{
		{name: "unset means default", cfg: HubConduitConfig{}},
		{name: "set", cfg: HubConduitConfig{GrantKeyActivation: "20m", ReconnectWindow: "2s"}, wantActivation: 20 * time.Minute, wantWindow: 2 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, err := tt.cfg.GrantKeyActivationDuration()
			require.NoError(t, err)
			assert.Equal(t, tt.wantActivation, a)
			w, err := tt.cfg.ReconnectWindowDuration()
			require.NoError(t, err)
			assert.Equal(t, tt.wantWindow, w)
		})
	}
}

func TestHubConduitConfig_IsZero(t *testing.T) {
	for name, c := range map[string]HubConduitConfig{
		"activation": {GrantKeyActivation: "1m"},
		"ports":      {TCPAllowedPorts: []int{22}},
		"listen":     {InternalListen: ":1"},
		"advertise":  {InternalAdvertise: "http://h:1"},
		"peer auth":  {PeerAuth: "hmac"},
		"peer SAs":   {PeerServiceAccounts: []string{"a@b"}},
		"audience":   {PeerAudience: "a"},
		"window":     {ReconnectWindow: "1s"},
		"instance":   {InstanceID: "hub-0"},
	} {
		assert.False(t, c.IsZero(), name)
	}
	assert.True(t, HubConduitConfig{}.IsZero())
}

func TestConduitConfig_V1RoundTrip(t *testing.T) {
	gc := &GlobalConfig{}
	gc.Hub.Conduit = HubConduitConfig{
		GrantKeyActivation: "20m", TCPAllowedPorts: []int{22, 8080}, InternalListen: ":9810",
		InternalAdvertise: "http://10.0.0.5:9810", PeerAuth: "oidc",
		PeerServiceAccounts: []string{"hub@p.iam.gserviceaccount.com"}, PeerAudience: "aud", ReconnectWindow: "7s",
		InstanceID: "hub-0",
	}
	v1 := ConvertGlobalToV1ServerConfig(gc)
	require.NotNil(t, v1.Hub)
	require.NotNil(t, v1.Hub.Conduit)
	assert.Equal(t, "7s", v1.Hub.Conduit.ReconnectWindow)
	assert.Equal(t, gc.Hub.Conduit, ConvertV1ServerToGlobalConfig(v1).Hub.Conduit)

	assert.Nil(t, ConvertGlobalToV1ServerConfig(&GlobalConfig{}).Hub.Conduit, "an unset conduit block is omitted")
}

// conduitSchemaEnvVar reads the x-env-var of server.hub.conduit.<field>
// from the v1 settings schema.
func conduitSchemaEnvVar(t *testing.T, field string) string {
	t.Helper()
	data, err := schemasFS.ReadFile(settingsSchemaFiles["1"])
	require.NoError(t, err)
	var root map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &root))
	prop := findSchemaProperty(t, root, "server", "hub", "conduit", field)
	envVar, _ := prop["x-env-var"].(string)
	require.NotEmpty(t, envVar, "schema: server.hub.conduit.%s has no x-env-var", field)
	return envVar
}

func TestLoadGlobalConfig_ConduitSettings(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	configPath := filepath.Join(tmpDir, "settings.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(`
schema_version: "1"
server:
  hub:
    conduit:
      grant_key_activation: 20m
      tcp_allowed_ports: [22, 3000]
      internal_listen: ":9810"
      peer_auth: hmac
      reconnect_window: 3s
`), 0644))

	cfg, err := LoadGlobalConfig(configPath)
	require.NoError(t, err)
	c := cfg.Hub.Conduit
	assert.Equal(t, "20m", c.GrantKeyActivation)
	assert.Equal(t, []int{22, 3000}, c.TCPAllowedPorts)
	assert.Equal(t, ":9810", c.InternalListen)
	assert.Equal(t, "hmac", c.PeerAuth)
	assert.Equal(t, "3s", c.ReconnectWindow)

	t.Run("env override", func(t *testing.T) {
		t.Setenv(conduitSchemaEnvVar(t, "reconnect_window"), "9s")
		cfg, err := LoadGlobalConfig(configPath)
		require.NoError(t, err)
		assert.Equal(t, "9s", cfg.Hub.Conduit.ReconnectWindow)
	})

	t.Run("instance id env", func(t *testing.T) {
		t.Setenv(conduitSchemaEnvVar(t, "instance_id"), "hub-east-1")
		cfg, err := LoadGlobalConfig(configPath)
		require.NoError(t, err)
		assert.Equal(t, "hub-east-1", cfg.Hub.Conduit.InstanceID)
	})
}
