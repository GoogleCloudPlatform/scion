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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHubInitFirewall_V1RoundTrip(t *testing.T) {
	gc := ConvertV1ServerToGlobalConfig(&V1ServerConfig{
		Hub: &V1ServerHubConfig{
			InitFirewall: &V1InitFirewallConfig{
				AllowDomains: []string{"artifactory.corp.example", "pypi.org"},
				AllowSSH:     true,
			},
		},
	})
	require.NotNil(t, gc.Hub.InitFirewall)
	assert.Equal(t, []string{"artifactory.corp.example", "pypi.org"}, gc.Hub.InitFirewall.AllowDomains)
	assert.True(t, gc.Hub.InitFirewall.AllowSSH)

	v1 := ConvertGlobalToV1ServerConfig(gc)
	require.NotNil(t, v1.Hub)
	require.NotNil(t, v1.Hub.InitFirewall)
	assert.Equal(t, []string{"artifactory.corp.example", "pypi.org"}, v1.Hub.InitFirewall.AllowDomains)
	assert.True(t, v1.Hub.InitFirewall.AllowSSH)

	unset := ConvertV1ServerToGlobalConfig(&V1ServerConfig{Hub: &V1ServerHubConfig{}})
	assert.Nil(t, unset.Hub.InitFirewall)
	pol, err := unset.Hub.InitFirewall.Normalize()
	require.NoError(t, err)
	assert.True(t, pol.IsEmpty())
}

func TestHubInitFirewall_LoadGlobalConfig(t *testing.T) {
	t.Run("valid settings.yaml normalizes and sorts", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("HOME", dir)
		scionDir := filepath.Join(dir, ".scion")
		require.NoError(t, os.MkdirAll(scionDir, 0o755))
		settingsYAML := `schema_version: "1"
server:
  hub:
    init_firewall:
      allow_domains:
        - "Artifactory.Corp.Example."
        - "pypi.org"
        - "artifactory.corp.example"
      allow_ssh: true
`
		require.NoError(t, os.WriteFile(filepath.Join(scionDir, "settings.yaml"), []byte(settingsYAML), 0o600))

		gc, err := LoadGlobalConfig("")
		require.NoError(t, err)
		require.NotNil(t, gc.Hub.InitFirewall)
		assert.Equal(t, []string{"artifactory.corp.example", "pypi.org"}, gc.Hub.InitFirewall.AllowDomains)
		assert.True(t, gc.Hub.InitFirewall.AllowSSH)

		pol, err := gc.Hub.InitFirewall.Normalize()
		require.NoError(t, err)
		assert.Equal(t, []string{"artifactory.corp.example", "pypi.org"}, pol.AllowDomains)
		assert.True(t, pol.AllowSSH)
	})

	t.Run("invalid entry fails LoadGlobalConfig naming the entry", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("HOME", dir)
		scionDir := filepath.Join(dir, ".scion")
		require.NoError(t, os.MkdirAll(scionDir, 0o755))
		settingsYAML := `schema_version: "1"
server:
  hub:
    init_firewall:
      allow_domains:
        - "artifactory.corp.example"
        - "bad;name"
`
		require.NoError(t, os.WriteFile(filepath.Join(scionDir, "settings.yaml"), []byte(settingsYAML), 0o600))

		_, err := LoadGlobalConfig("")
		require.Error(t, err)
		assert.True(t, strings.Contains(err.Error(), "bad;name"), "error %q must name invalid entry", err.Error())
		assert.Contains(t, err.Error(), "server.hub.init_firewall")
	})

	t.Run("env overrides load and validate", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("HOME", dir)
		scionDir := filepath.Join(dir, ".scion")
		require.NoError(t, os.MkdirAll(scionDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(scionDir, "settings.yaml"), []byte("schema_version: \"1\"\nserver:\n  hub:\n    port: 8080\n"), 0o600))

		t.Setenv("SCION_SERVER_HUB_INITFIREWALL_ALLOWDOMAINS", "Registry.Example.Com,artifactory.corp.example")
		t.Setenv("SCION_SERVER_HUB_INITFIREWALL_ALLOWSSH", "true")

		gc, err := LoadGlobalConfig("")
		require.NoError(t, err)
		require.NotNil(t, gc.Hub.InitFirewall)
		assert.Equal(t, []string{"artifactory.corp.example", "registry.example.com"}, gc.Hub.InitFirewall.AllowDomains)
		assert.True(t, gc.Hub.InitFirewall.AllowSSH)
	})

	t.Run("invalid env override fails LoadGlobalConfig", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("HOME", dir)
		scionDir := filepath.Join(dir, ".scion")
		require.NoError(t, os.MkdirAll(scionDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(scionDir, "settings.yaml"), []byte("schema_version: \"1\"\nserver:\n  hub:\n    port: 8080\n"), 0o600))

		t.Setenv("SCION_SERVER_HUB_INITFIREWALL_ALLOWDOMAINS", "*.evil.example")

		_, err := LoadGlobalConfig("")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "server.hub.init_firewall")
		assert.Contains(t, err.Error(), "*.evil.example")
	})
}
