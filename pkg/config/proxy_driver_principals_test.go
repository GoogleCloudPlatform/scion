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
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WS2 A1 (ptone/scion#4240): auth.proxy.driverPrincipals and
// auth.proxy.driverTokenAudience parse from settings.yaml into the server
// config, survive the legacy round trip, and are described in the schema.
func TestProxyDriverSettings_Parse(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	configPath := filepath.Join(tmpDir, "settings.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(`
schema_version: "1"
server:
  auth:
    mode: proxy
    proxy:
      provider: iap
      iap:
        audience: /projects/1/global/backendServices/2
      driver_principals:
        - ui-driver@test-project.iam.gserviceaccount.com
        - second-driver@test-project.iam.gserviceaccount.com
      driver_token_audience: 1234-abcd.apps.googleusercontent.com
`), 0o644))

	cfg, err := LoadGlobalConfig(configPath)
	require.NoError(t, err)
	require.NotNil(t, cfg.Auth.Proxy)
	assert.Equal(t, []string{
		"ui-driver@test-project.iam.gserviceaccount.com",
		"second-driver@test-project.iam.gserviceaccount.com",
	}, cfg.Auth.Proxy.ProxyDriverPrincipals())
	assert.Equal(t, "1234-abcd.apps.googleusercontent.com", cfg.Auth.Proxy.ProxyDriverTokenAudience())

	v1 := ConvertGlobalToV1ServerConfig(cfg)
	require.NotNil(t, v1.Auth)
	require.NotNil(t, v1.Auth.Proxy)
	assert.Equal(t, cfg.Auth.Proxy.DriverPrincipals, v1.Auth.Proxy.DriverPrincipals)
	assert.Equal(t, cfg.Auth.Proxy.DriverTokenAudience, v1.Auth.Proxy.DriverTokenAudience)
	gc2 := ConvertV1ServerToGlobalConfig(v1)
	assert.Equal(t, cfg.Auth.Proxy.DriverPrincipals, gc2.Auth.Proxy.DriverPrincipals)
	assert.Equal(t, cfg.Auth.Proxy.DriverTokenAudience, gc2.Auth.Proxy.DriverTokenAudience)

	data, err := schemasFS.ReadFile(settingsSchemaFiles["1"])
	require.NoError(t, err)
	var root map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &root))
	assert.Equal(t, "array", findSchemaProperty(t, root, "server", "auth", "proxy", "driver_principals")["type"])
	assert.Equal(t, "string", findSchemaProperty(t, root, "server", "auth", "proxy", "driver_token_audience")["type"])
}

func TestProxyDriverSettings_NilSafeAccessors(t *testing.T) {
	var c *ProxyAuthConfig
	assert.Nil(t, c.ProxyDriverPrincipals())
	assert.Equal(t, "", c.ProxyDriverTokenAudience())
	assert.Equal(t, "aud", (&ProxyAuthConfig{DriverTokenAudience: "  aud "}).ProxyDriverTokenAudience())
}

// Entries are exact emails: trimmed and lower-cased, never patterns.
func TestNormalizeProxyDriverPrincipals(t *testing.T) {
	got, err := NormalizeProxyDriverPrincipals([]string{" UI-Driver@Example.COM ", "b@example.com"})
	require.NoError(t, err)
	assert.Equal(t, []string{"ui-driver@example.com", "b@example.com"}, got)

	got, err = NormalizeProxyDriverPrincipals(nil)
	require.NoError(t, err)
	assert.Nil(t, got)

	for name, list := range map[string][]string{
		"empty entry":      {""},
		"blank entry":      {"   "},
		"star wildcard":    {"*@example.com"},
		"domain wildcard":  {"driver@*.example.com"},
		"question pattern": {"driver?@example.com"},
		"bracket pattern":  {"driver[1]@example.com"},
		"brace pattern":    {"{a,b}@example.com"},
		"no at":            {"driver.example.com"},
		"two ats":          {"a@b@example.com"},
		"no local part":    {"@example.com"},
		"no domain":        {"driver@"},
		"inner space":      {"dri ver@example.com"},
		"tab":              {"driver@exa\tmple.com"},
		"repeat":           {"a@example.com", "A@Example.com"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NormalizeProxyDriverPrincipals(list)
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrInvalidProxyDriverPrincipal), err)
		})
	}
}

// A non-proxy hub with a non-empty list refuses to start; an empty list in
// proxy mode is accepted (web sessions are then unavailable, which the hub
// reports).
func TestValidateProxyDriverConfig(t *testing.T) {
	list := []string{"driver@example.com"}
	for _, mode := range []string{"", "oauth", "dev", "Proxy"} {
		_, err := ValidateProxyDriverConfig(mode, list)
		assert.ErrorIs(t, err, ErrProxyDriverPrincipalsWithoutProxyAuth, "mode %q", mode)
		got, err := ValidateProxyDriverConfig(mode, nil)
		assert.NoError(t, err, "mode %q with no list", mode)
		assert.Nil(t, got)
	}
	got, err := ValidateProxyDriverConfig("proxy", list)
	require.NoError(t, err)
	assert.Equal(t, list, got)
	got, err = ValidateProxyDriverConfig("proxy", nil)
	require.NoError(t, err)
	assert.Empty(t, got)
	_, err = ValidateProxyDriverConfig("proxy", []string{"*@example.com"})
	assert.ErrorIs(t, err, ErrInvalidProxyDriverPrincipal)
}
