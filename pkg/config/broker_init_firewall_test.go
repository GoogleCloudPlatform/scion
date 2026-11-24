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

func TestBrokerInitFirewall_V1RoundTrip(t *testing.T) {
	gc := ConvertV1ServerToGlobalConfig(&V1ServerConfig{
		Broker: &V1BrokerConfig{
			InitFirewall: &V1InitFirewallConfig{
				AllowDomains: []string{"pypi.org", "files.pythonhosted.org"},
				AllowSSH:     true,
			},
		},
	})
	require.NotNil(t, gc.RuntimeBroker.InitFirewall)
	assert.Equal(t, []string{"pypi.org", "files.pythonhosted.org"}, gc.RuntimeBroker.InitFirewall.AllowDomains)
	assert.True(t, gc.RuntimeBroker.InitFirewall.AllowSSH)

	v1 := ConvertGlobalToV1ServerConfig(gc)
	require.NotNil(t, v1.Broker)
	require.NotNil(t, v1.Broker.InitFirewall)
	assert.Equal(t, []string{"pypi.org", "files.pythonhosted.org"}, v1.Broker.InitFirewall.AllowDomains)
	assert.True(t, v1.Broker.InitFirewall.AllowSSH)

	unset := ConvertV1ServerToGlobalConfig(&V1ServerConfig{Broker: &V1BrokerConfig{}})
	assert.Nil(t, unset.RuntimeBroker.InitFirewall)
	pol, err := unset.RuntimeBroker.InitFirewall.Normalize()
	require.NoError(t, err)
	assert.True(t, pol.IsEmpty())
}

func TestBrokerInitFirewall_LoadGlobalConfig(t *testing.T) {
	t.Run("valid settings.yaml normalizes and sorts", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("HOME", dir)
		scionDir := filepath.Join(dir, ".scion")
		require.NoError(t, os.MkdirAll(scionDir, 0o755))
		settingsYAML := `schema_version: "1"
server:
  broker:
    enabled: true
    init_firewall:
      allow_domains:
        - "PyPI.org."
        - "files.pythonhosted.org"
        - "pypi.org"
      allow_ssh: true
`
		require.NoError(t, os.WriteFile(filepath.Join(scionDir, "settings.yaml"), []byte(settingsYAML), 0o600))

		gc, err := LoadGlobalConfig("")
		require.NoError(t, err)
		require.NotNil(t, gc.RuntimeBroker.InitFirewall)
		assert.Equal(t, []string{"files.pythonhosted.org", "pypi.org"}, gc.RuntimeBroker.InitFirewall.AllowDomains)
		assert.True(t, gc.RuntimeBroker.InitFirewall.AllowSSH)

		pol, err := gc.RuntimeBroker.InitFirewall.Normalize()
		require.NoError(t, err)
		assert.Equal(t, []string{"files.pythonhosted.org", "pypi.org"}, pol.AllowDomains)
		assert.True(t, pol.AllowSSH)
	})

	t.Run("invalid entry fails LoadGlobalConfig naming the entry", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("HOME", dir)
		scionDir := filepath.Join(dir, ".scion")
		require.NoError(t, os.MkdirAll(scionDir, 0o755))
		settingsYAML := `schema_version: "1"
server:
  broker:
    enabled: true
    init_firewall:
      allow_domains:
        - "pypi.org"
        - "bad;name"
`
		require.NoError(t, os.WriteFile(filepath.Join(scionDir, "settings.yaml"), []byte(settingsYAML), 0o600))

		_, err := LoadGlobalConfig("")
		require.Error(t, err)
		assert.True(t, strings.Contains(err.Error(), "bad;name"), "error %q must name invalid entry", err.Error())
		assert.Contains(t, err.Error(), "server.broker.init_firewall")
	})

	t.Run("env overrides load and validate", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("HOME", dir)
		scionDir := filepath.Join(dir, ".scion")
		require.NoError(t, os.MkdirAll(scionDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(scionDir, "settings.yaml"), []byte("schema_version: \"1\"\nserver:\n  broker:\n    enabled: true\n"), 0o600))

		t.Setenv("SCION_SERVER_RUNTIMEBROKER_INITFIREWALL_ALLOWDOMAINS", "pypi.org,files.pythonhosted.org")
		t.Setenv("SCION_SERVER_RUNTIMEBROKER_INITFIREWALL_ALLOWSSH", "true")

		gc, err := LoadGlobalConfig("")
		require.NoError(t, err)
		require.NotNil(t, gc.RuntimeBroker.InitFirewall)
		assert.Equal(t, []string{"files.pythonhosted.org", "pypi.org"}, gc.RuntimeBroker.InitFirewall.AllowDomains)
		assert.True(t, gc.RuntimeBroker.InitFirewall.AllowSSH)
	})
}
