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
	"gopkg.in/yaml.v3"
)

// server.hub.internal_url (ptone/scion#555) is the optional, operator-only
// hub_url handed to broker plugins for Hub API calls.

const hubInternalURLSettingsYAML = `schema_version: "1"
server:
  hub:
    internal_url: http://127.0.0.1:8080
`

func TestHubInternalURL_V1YAMLRoundTrip(t *testing.T) {
	var v1 V1ServerConfig
	require.NoError(t, yaml.Unmarshal([]byte("hub:\n  internal_url: http://127.0.0.1:8080\n"), &v1))
	require.NotNil(t, v1.Hub)
	assert.Equal(t, "http://127.0.0.1:8080", v1.Hub.InternalURL)

	gc := ConvertV1ServerToGlobalConfig(&v1)
	assert.Equal(t, "http://127.0.0.1:8080", gc.Hub.InternalURL)

	back := ConvertGlobalToV1ServerConfig(gc)
	require.NotNil(t, back.Hub)
	assert.Equal(t, "http://127.0.0.1:8080", back.Hub.InternalURL)
}

func TestHubInternalURL_UnsetStaysEmpty(t *testing.T) {
	gc := ConvertV1ServerToGlobalConfig(&V1ServerConfig{Hub: &V1ServerHubConfig{Port: 9000, PublicURL: "https://hub.example.com"}})
	assert.Empty(t, gc.Hub.InternalURL)
	assert.Equal(t, "https://hub.example.com", gc.Hub.Endpoint)
	out, err := yaml.Marshal(ConvertGlobalToV1ServerConfig(gc))
	require.NoError(t, err)
	assert.NotContains(t, string(out), "internal_url")
}

func writeHubInternalURLHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	globalDir := filepath.Join(home, ".scion")
	require.NoError(t, os.MkdirAll(globalDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(hubInternalURLSettingsYAML), 0o644))
	return globalDir
}

func TestHubInternalURL_LoadGlobalConfigFromSettingsYAML(t *testing.T) {
	globalDir := writeHubInternalURLHome(t)
	t.Setenv("SCION_SERVER_HUB_INTERNALURL", "")

	gc, err := LoadGlobalConfig(globalDir)
	require.NoError(t, err)
	assert.Equal(t, "http://127.0.0.1:8080", gc.Hub.InternalURL)
}

func TestHubInternalURL_EnvWinsOverSettingsYAML(t *testing.T) {
	globalDir := writeHubInternalURLHome(t)
	t.Setenv("SCION_SERVER_HUB_INTERNALURL", "http://10.0.0.5:9810")

	gc, err := LoadGlobalConfig(globalDir)
	require.NoError(t, err)
	assert.Equal(t, "http://10.0.0.5:9810", gc.Hub.InternalURL)
}

func TestHubInternalURL_EnvTableEntryFromSchema(t *testing.T) {
	path, alias, ok := KeyForServerEnv("SCION_SERVER_HUB_INTERNALURL")
	require.True(t, ok)
	assert.False(t, alias)
	assert.Equal(t, HubInternalURLKey, path)
	path, alias, ok = KeyForServerEnv("SCION_SERVER_HUB_INTERNAL_URL")
	require.True(t, ok)
	assert.True(t, alias)
	assert.Equal(t, HubInternalURLKey, path)
	for _, e := range ServerEnvEntries() {
		if e.Path == HubInternalURLKey {
			assert.Equal(t, "hub.internalUrl", e.GlobalConfig)
			assert.Equal(t, "string", e.Type)
			return
		}
	}
	t.Fatalf("no env table entry for %s", HubInternalURLKey)
}

func TestHubInternalURL_SchemaAcceptsKey(t *testing.T) {
	errs, err := ValidateSettings([]byte(hubInternalURLSettingsYAML), "1")
	require.NoError(t, err)
	assert.Empty(t, errs)
}

func TestValidateHubInternalURL(t *testing.T) {
	got, err := ValidateHubInternalURL("")
	require.NoError(t, err)
	assert.Empty(t, got, "unset stays unset")

	got, err = ValidateHubInternalURL("HTTP://127.0.0.1:8080/")
	require.NoError(t, err)
	assert.Equal(t, "http://127.0.0.1:8080", got)

	for _, bad := range []string{"ftp://h", "http://h/path", "http://u:secret@h", "http://h?x=1", "http://h:0"} {
		_, err := ValidateHubInternalURL(bad)
		require.Error(t, err, bad)
		assert.True(t, strings.HasPrefix(err.Error(), HubInternalURLKey+": "), err.Error())
		assert.NotContains(t, err.Error(), "secret")
	}

	// agent_endpoint keeps its own key in its messages.
	_, err = ValidateAgentEndpoint("ftp://h")
	require.Error(t, err)
	assert.True(t, strings.HasPrefix(err.Error(), "server.hub.agent_endpoint: "), err.Error())
}
