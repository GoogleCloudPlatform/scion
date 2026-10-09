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

	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/v2"
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
		{name: "authz recheck interval at min", cfg: HubConduitConfig{AuthzRecheckInterval: "1s"}},
		{name: "authz recheck interval at max", cfg: HubConduitConfig{AuthzRecheckInterval: "10m"}},
		{name: "authz recheck interval below min", cfg: HubConduitConfig{AuthzRecheckInterval: "500ms"}, wantErr: []string{"authz_recheck_interval", "between 1s and 10m0s"}},
		{name: "authz recheck interval above max", cfg: HubConduitConfig{AuthzRecheckInterval: "11m"}, wantErr: []string{"authz_recheck_interval"}},
		{name: "authz recheck interval malformed", cfg: HubConduitConfig{AuthzRecheckInterval: "often"}, wantErr: []string{"authz_recheck_interval"}},
		{name: "stream authz max at min", cfg: HubConduitConfig{StreamAuthzMax: HubConduitStreamAuthzMax{User: "1m", Broker: "1m", Agent: "1m"}}},
		{name: "stream authz max at max", cfg: HubConduitConfig{StreamAuthzMax: HubConduitStreamAuthzMax{User: "168h", Broker: "168h", Agent: "168h"}}},
		{name: "stream authz max user below min", cfg: HubConduitConfig{StreamAuthzMax: HubConduitStreamAuthzMax{User: "59s"}}, wantErr: []string{"stream_authz_max.user", "between 1m0s and 168h0m0s"}},
		{name: "stream authz max user above max", cfg: HubConduitConfig{StreamAuthzMax: HubConduitStreamAuthzMax{User: "169h"}}, wantErr: []string{"stream_authz_max.user"}},
		{name: "stream authz max user malformed", cfg: HubConduitConfig{StreamAuthzMax: HubConduitStreamAuthzMax{User: "a while"}}, wantErr: []string{"stream_authz_max.user"}},
		{name: "stream authz max broker validated", cfg: HubConduitConfig{StreamAuthzMax: HubConduitStreamAuthzMax{Broker: "30s"}}, wantErr: []string{"stream_authz_max.broker"}},
		{name: "stream authz max agent validated", cfg: HubConduitConfig{StreamAuthzMax: HubConduitStreamAuthzMax{Agent: "8d"}}, wantErr: []string{"stream_authz_max.agent"}},
		{name: "stream authz max every field reported", cfg: HubConduitConfig{StreamAuthzMax: HubConduitStreamAuthzMax{User: "0s", Broker: "-1h", Agent: "x"}},
			wantErr: []string{"stream_authz_max.user", "stream_authz_max.broker", "stream_authz_max.agent"}},
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
		{name: "peer SA comma list in one entry", cfg: HubConduitConfig{PeerServiceAccounts: []string{"a@p.iam.gserviceaccount.com,b@p.iam.gserviceaccount.com"}}, wantErr: []string{"peer_service_accounts"}},
		{name: "peer SA with inner space", cfg: HubConduitConfig{PeerServiceAccounts: []string{"a@p.iam.gserviceaccount.com b@p"}}, wantErr: []string{"peer_service_accounts"}},
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

// TestHubConduitConfig_StreamAuthzMaxDurations: unset values take the Q6
// defaults (8h for user streams, 24h for broker and agent streams); set
// values are parsed per field.
func TestHubConduitConfig_StreamAuthzMaxDurations(t *testing.T) {
	tests := []struct {
		name string
		cfg  HubConduitStreamAuthzMax
		want ConduitStreamAuthzMax
	}{
		{name: "defaults", want: ConduitStreamAuthzMax{User: 8 * time.Hour, Broker: 24 * time.Hour, Agent: 24 * time.Hour}},
		{name: "user only", cfg: HubConduitStreamAuthzMax{User: "2h"}, want: ConduitStreamAuthzMax{User: 2 * time.Hour, Broker: 24 * time.Hour, Agent: 24 * time.Hour}},
		{name: "all set", cfg: HubConduitStreamAuthzMax{User: "30m", Broker: "12h", Agent: "168h"}, want: ConduitStreamAuthzMax{User: 30 * time.Minute, Broker: 12 * time.Hour, Agent: 168 * time.Hour}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := HubConduitConfig{StreamAuthzMax: tt.cfg}.StreamAuthzMaxDurations()
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
	assert.Equal(t, 8*time.Hour, ConduitDefaultUserStreamAuthzMax)
	assert.Equal(t, 24*time.Hour, ConduitDefaultServiceStreamAuthzMax)
	_, err := HubConduitConfig{StreamAuthzMax: HubConduitStreamAuthzMax{User: "soon"}}.StreamAuthzMaxDurations()
	assert.ErrorContains(t, err, "stream_authz_max.user")
}

// TestHubConduitConfig_StreamAuthzMaxTypoIsUnused: stream_authz_max is a
// typed struct, so a misspelt field is reported as an unrecognized key
// (and the default applies) rather than silently accepted.
func TestHubConduitConfig_StreamAuthzMaxTypoIsUnused(t *testing.T) {
	k := koanf.New(".")
	require.NoError(t, k.Load(confmap.Provider(map[string]interface{}{
		"hub.conduit.streamAuthzMax.users": "2h",
		"hub.conduit.streamAuthzMax.agent": "12h",
	}, "."), nil))
	var gc GlobalConfig
	unused, err := decodeCollectingUnused(k, &gc)
	require.NoError(t, err)
	assert.Contains(t, strings.ToLower(strings.Join(unused, ",")), "streamauthzmax.users")
	assert.Equal(t, HubConduitStreamAuthzMax{Agent: "12h"}, gc.Hub.Conduit.StreamAuthzMax)
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
		"recheck":    {AuthzRecheckInterval: "30s"},
		"authz max":  {StreamAuthzMax: HubConduitStreamAuthzMax{Agent: "1h"}},
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
		InstanceID: "hub-0", AuthzRecheckInterval: "45s",
		StreamAuthzMax: HubConduitStreamAuthzMax{User: "4h", Broker: "20h", Agent: "22h"},
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
      authz_recheck_interval: 30s
      stream_authz_max:
        user: 4h
        agent: 12h
`), 0644))

	cfg, err := LoadGlobalConfig(configPath)
	require.NoError(t, err)
	c := cfg.Hub.Conduit
	assert.Equal(t, "20m", c.GrantKeyActivation)
	assert.Equal(t, []int{22, 3000}, c.TCPAllowedPorts)
	assert.Equal(t, ":9810", c.InternalListen)
	assert.Equal(t, "hmac", c.PeerAuth)
	assert.Equal(t, "3s", c.ReconnectWindow)
	assert.Equal(t, "30s", c.AuthzRecheckInterval)
	d, err := c.AuthzRecheckIntervalDuration()
	require.NoError(t, err)
	assert.Equal(t, 30*time.Second, d)
	assert.Equal(t, HubConduitStreamAuthzMax{User: "4h", Agent: "12h"}, c.StreamAuthzMax)
	m, err := c.StreamAuthzMaxDurations()
	require.NoError(t, err)
	assert.Equal(t, ConduitStreamAuthzMax{User: 4 * time.Hour, Broker: 24 * time.Hour, Agent: 12 * time.Hour}, m)

	t.Run("env override", func(t *testing.T) {
		t.Setenv(conduitSchemaEnvVar(t, "reconnect_window"), "9s")
		cfg, err := LoadGlobalConfig(configPath)
		require.NoError(t, err)
		assert.Equal(t, "9s", cfg.Hub.Conduit.ReconnectWindow)
	})

	t.Run("list env vars", func(t *testing.T) {
		tests := []struct {
			name, sas, ports string
			wantSAs          []string
			wantPorts        []int
		}{
			{name: "comma lists, trimmed", sas: "a@p.iam.gserviceaccount.com, b@p.iam.gserviceaccount.com ", ports: "22, 3000,",
				wantSAs: []string{"a@p.iam.gserviceaccount.com", "b@p.iam.gserviceaccount.com"}, wantPorts: []int{22, 3000}},
			{name: "single entries", sas: "a@p.iam.gserviceaccount.com", ports: "8080",
				wantSAs: []string{"a@p.iam.gserviceaccount.com"}, wantPorts: []int{8080}},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Setenv(conduitSchemaEnvVar(t, "peer_service_accounts"), tt.sas)
				t.Setenv(conduitSchemaEnvVar(t, "tcp_allowed_ports"), tt.ports)
				cfg, err := LoadGlobalConfig(configPath)
				require.NoError(t, err)
				assert.Equal(t, tt.wantSAs, cfg.Hub.Conduit.PeerServiceAccounts)
				assert.Equal(t, tt.wantPorts, cfg.Hub.Conduit.TCPAllowedPorts)
				assert.NoError(t, cfg.Hub.Conduit.Validate())
			})
		}
	})

	t.Run("authz recheck interval env", func(t *testing.T) {
		t.Setenv(conduitSchemaEnvVar(t, "authz_recheck_interval"), "15s")
		cfg, err := LoadGlobalConfig(configPath)
		require.NoError(t, err)
		assert.Equal(t, "15s", cfg.Hub.Conduit.AuthzRecheckInterval)
	})

	t.Run("stream authz max env", func(t *testing.T) {
		data, err := schemasFS.ReadFile(settingsSchemaFiles["1"])
		require.NoError(t, err)
		var root map[string]interface{}
		require.NoError(t, json.Unmarshal(data, &root))
		want := map[string]string{"user": "3h", "broker": "30h", "agent": "40h"}
		for field, v := range want {
			prop := findSchemaProperty(t, root, "server", "hub", "conduit", "stream_authz_max", field)
			envVar, _ := prop["x-env-var"].(string)
			require.Equal(t, "SCION_SERVER_HUB_CONDUIT_STREAMAUTHZMAX_"+strings.ToUpper(field), envVar)
			t.Setenv(envVar, v)
		}
		cfg, err := LoadGlobalConfig(configPath)
		require.NoError(t, err)
		assert.Equal(t, HubConduitStreamAuthzMax{User: "3h", Broker: "30h", Agent: "40h"}, cfg.Hub.Conduit.StreamAuthzMax)
		assert.NoError(t, cfg.Hub.Conduit.Validate())
	})

	t.Run("instance id env", func(t *testing.T) {
		t.Setenv(conduitSchemaEnvVar(t, "instance_id"), "hub-east-1")
		cfg, err := LoadGlobalConfig(configPath)
		require.NoError(t, err)
		assert.Equal(t, "hub-east-1", cfg.Hub.Conduit.InstanceID)
	})
}

// TestConduitListKeysSplit: the bootstrap (opsettings) and v1 env paths
// split the conduit list settings' comma-separated env values.
func TestConduitListKeysSplit(t *testing.T) {
	for _, keys := range [][]string{commaSplitKoanfKeys, conduitV1EnvListKeys} {
		k := koanf.New(".")
		require.NoError(t, k.Load(confmap.Provider(map[string]interface{}{
			"server.hub.conduit.peer_service_accounts": "a@p.iam.gserviceaccount.com, b@p.iam.gserviceaccount.com",
			"server.hub.conduit.tcp_allowed_ports":     "22,3000",
		}, "."), nil))
		splitKoanfListKeys(k, keys)
		assert.Equal(t, []string{"a@p.iam.gserviceaccount.com", "b@p.iam.gserviceaccount.com"}, k.Strings("server.hub.conduit.peer_service_accounts"))
		assert.Equal(t, []int{22, 3000}, k.Ints("server.hub.conduit.tcp_allowed_ports"))
	}
}
