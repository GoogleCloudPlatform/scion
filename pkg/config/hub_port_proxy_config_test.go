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

func TestHubPortProxyConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    time.Duration
		wantErr []string // substrings; nil = valid
	}{
		{name: "unset is the default", value: "", want: 60 * time.Second},
		{name: "at min", value: "5s", want: 5 * time.Second},
		{name: "at max", value: "10m", want: 10 * time.Minute},
		{name: "typical", value: "90s", want: 90 * time.Second},
		{name: "below min", value: "4s", wantErr: []string{"server.hub.port_proxy.response_header_timeout", "between 5s and 10m0s"}},
		{name: "above max", value: "11m", wantErr: []string{"response_header_timeout", "between 5s and 10m0s"}},
		{name: "zero", value: "0s", wantErr: []string{"response_header_timeout", "between 5s and 10m0s"}},
		{name: "bare zero", value: "0", wantErr: []string{"response_header_timeout"}},
		{name: "negative", value: "-30s", wantErr: []string{"response_header_timeout"}},
		{name: "malformed", value: "a minute", wantErr: []string{"response_header_timeout", "a minute"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := HubPortProxyConfig{ResponseHeaderTimeout: tt.value}
			d, err := c.ResponseHeaderTimeoutDuration()
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
	assert.Equal(t, PortProxyDefaultResponseHeaderTimeout, 60*time.Second)
}

func portProxySchemaEnvVar(t *testing.T, field string) string {
	t.Helper()
	data, err := schemasFS.ReadFile(settingsSchemaFiles["1"])
	require.NoError(t, err)
	var root map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &root))
	prop := findSchemaProperty(t, root, "server", "hub", "port_proxy", field)
	envVar, _ := prop["x-env-var"].(string)
	require.NotEmpty(t, envVar, "schema: server.hub.port_proxy.%s has no x-env-var", field)
	return envVar
}

// TestPortProxyResponseHeaderTimeout_EnvMapping:
// SCION_SERVER_HUB_PORTPROXY_RESPONSEHEADERTIMEOUT is the schema's env var
// for server.hub.port_proxy.response_header_timeout, maps to that key in
// the opsettings keyspace and the hub config, and overrides the file.
func TestPortProxyResponseHeaderTimeout_EnvMapping(t *testing.T) {
	const env = "SCION_SERVER_HUB_PORTPROXY_RESPONSEHEADERTIMEOUT"
	assert.Equal(t, env, portProxySchemaEnvVar(t, "response_header_timeout"))
	assert.Equal(t, "server.hub.port_proxy.response_header_timeout", serverEnvToOpsettingsKey("HUB_PORTPROXY_RESPONSEHEADERTIMEOUT"))
	assert.Equal(t, "hub.portProxy.responseHeaderTimeout", envKeyToConfigKey("HUB_PORTPROXY_RESPONSEHEADERTIMEOUT"))
	assert.Equal(t, "server.hub.port_proxy.response_header_timeout", versionedEnvKeyMapper("SCION_SERVER_HUB_PORT_PROXY_RESPONSE_HEADER_TIMEOUT"))

	t.Setenv(env, "30s")
	k := LoadEnvKoanf()
	assert.Equal(t, "30s", k.String("server.hub.port_proxy.response_header_timeout"), "keys: %v", k.Keys())

	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	configPath := filepath.Join(tmpDir, "settings.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte("schema_version: \"1\"\nserver:\n  hub:\n    port_proxy:\n      response_header_timeout: 2m\n"), 0644))
	cfg, err := LoadGlobalConfig(configPath)
	require.NoError(t, err)
	assert.Equal(t, "30s", cfg.Hub.PortProxy.ResponseHeaderTimeout, "the env var overrides the file")
	d, err := cfg.Hub.PortProxy.ResponseHeaderTimeoutDuration()
	require.NoError(t, err)
	assert.Equal(t, 30*time.Second, d)
}

// TestLoadGlobalConfig_PortProxySettings: the file value loads, and an
// out-of-range value loads as configured and fails Validate.
func TestLoadGlobalConfig_PortProxySettings(t *testing.T) {
	for _, tc := range []struct {
		value   string
		wantErr bool
	}{{"2m", false}, {"1s", true}, {"0s", true}} {
		t.Run(tc.value, func(t *testing.T) {
			tmpDir := t.TempDir()
			t.Setenv("HOME", tmpDir)
			configPath := filepath.Join(tmpDir, "settings.yaml")
			require.NoError(t, os.WriteFile(configPath, []byte("schema_version: \"1\"\nserver:\n  hub:\n    port_proxy:\n      response_header_timeout: "+tc.value+"\n"), 0644))
			cfg, err := LoadGlobalConfig(configPath)
			require.NoError(t, err)
			assert.Equal(t, tc.value, cfg.Hub.PortProxy.ResponseHeaderTimeout)
			if tc.wantErr {
				assert.ErrorContains(t, cfg.Hub.PortProxy.Validate(), "server.hub.port_proxy.response_header_timeout")
			} else {
				assert.NoError(t, cfg.Hub.PortProxy.Validate())
			}
		})
	}
}

// TestPortProxyConfig_V1RoundTrip: the setting survives the V1 <-> global
// conversions.
func TestPortProxyConfig_V1RoundTrip(t *testing.T) {
	gc := &GlobalConfig{}
	gc.Hub.PortProxy.ResponseHeaderTimeout = "45s"
	v1 := ConvertGlobalToV1ServerConfig(gc)
	require.NotNil(t, v1.Hub)
	require.NotNil(t, v1.Hub.PortProxy)
	assert.Equal(t, "45s", v1.Hub.PortProxy.ResponseHeaderTimeout)
	back := ConvertV1ServerToGlobalConfig(v1)
	assert.Equal(t, "45s", back.Hub.PortProxy.ResponseHeaderTimeout)

	assert.Nil(t, ConvertGlobalToV1ServerConfig(&GlobalConfig{}).Hub.PortProxy, "an unset setting is not written")
}

func TestHubPortProxyConfig_ViewTTL(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    time.Duration
		wantErr []string // substrings; nil = valid
	}{
		{name: "unset is the default", value: "", want: 2 * time.Hour},
		{name: "at min", value: "15m", want: 15 * time.Minute},
		{name: "at max", value: "24h", want: 24 * time.Hour},
		{name: "typical", value: "4h", want: 4 * time.Hour},
		{name: "below min", value: "14m59s", wantErr: []string{"server.hub.port_proxy.view_ttl", "between 15m0s and 24h0m0s"}},
		{name: "above max", value: "24h1s", wantErr: []string{"view_ttl", "between 15m0s and 24h0m0s"}},
		{name: "zero", value: "0s", wantErr: []string{"view_ttl"}},
		{name: "negative", value: "-2h", wantErr: []string{"view_ttl"}},
		{name: "malformed", value: "two hours", wantErr: []string{"view_ttl", "two hours"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := HubPortProxyConfig{ViewTTL: tt.value}
			d, err := c.ViewTTLDuration()
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
	assert.Equal(t, 2*time.Hour, PortProxyDefaultViewTTL)
	assert.False(t, HubPortProxyConfig{ViewTTL: "1h"}.IsZero())
}

// TestPortProxyViewTTL_EnvMapping: SCION_SERVER_HUB_PORTPROXY_VIEWTTL is
// the schema's env var for server.hub.port_proxy.view_ttl and overrides
// the file.
func TestPortProxyViewTTL_EnvMapping(t *testing.T) {
	const env = "SCION_SERVER_HUB_PORTPROXY_VIEWTTL"
	assert.Equal(t, env, portProxySchemaEnvVar(t, "view_ttl"))
	assert.Equal(t, "server.hub.port_proxy.view_ttl", serverEnvToOpsettingsKey("HUB_PORTPROXY_VIEWTTL"))
	assert.Equal(t, "hub.portProxy.viewTtl", envKeyToConfigKey("HUB_PORTPROXY_VIEWTTL"))

	t.Setenv(env, "30m")
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	configPath := filepath.Join(tmpDir, "settings.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte("schema_version: \"1\"\nserver:\n  hub:\n    port_proxy:\n      view_ttl: 3h\n"), 0644))
	cfg, err := LoadGlobalConfig(configPath)
	require.NoError(t, err)
	assert.Equal(t, "30m", cfg.Hub.PortProxy.ViewTTL, "the env var overrides the file")
	d, err := cfg.Hub.PortProxy.ViewTTLDuration()
	require.NoError(t, err)
	assert.Equal(t, 30*time.Minute, d)
}

// TestLoadGlobalConfig_PortProxyViewTTL: the file value loads, survives
// the V1 <-> global conversions, and an out-of-range value fails Validate.
func TestLoadGlobalConfig_PortProxyViewTTL(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	configPath := filepath.Join(tmpDir, "settings.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte("schema_version: \"1\"\nserver:\n  hub:\n    port_proxy:\n      view_ttl: 25h\n"), 0644))
	cfg, err := LoadGlobalConfig(configPath)
	require.NoError(t, err)
	assert.Equal(t, "25h", cfg.Hub.PortProxy.ViewTTL)
	assert.ErrorContains(t, cfg.Hub.PortProxy.Validate(), "server.hub.port_proxy.view_ttl")

	gc := &GlobalConfig{}
	gc.Hub.PortProxy.ViewTTL = "45m"
	v1 := ConvertGlobalToV1ServerConfig(gc)
	require.NotNil(t, v1.Hub)
	require.NotNil(t, v1.Hub.PortProxy)
	assert.Equal(t, "45m", v1.Hub.PortProxy.ViewTTL)
	assert.Equal(t, "45m", ConvertV1ServerToGlobalConfig(v1).Hub.PortProxy.ViewTTL)
}
