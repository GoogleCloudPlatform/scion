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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// server.hub.gcp_project_id in a v1 settings.yaml reaches
// GlobalConfig.Hub.GCPProjectID (ptone/scion#3830); before, the key was
// silently dropped and only SCION_SERVER_HUB_GCPPROJECTID set it.

const hubGCPProjectIDSettingsYAML = `schema_version: "1"
server:
  hub:
    gcp_project_id: file-project
`

func TestHubGCPProjectID_V1YAMLRoundTrip(t *testing.T) {
	var v1 V1ServerConfig
	require.NoError(t, yaml.Unmarshal([]byte("hub:\n  gcp_project_id: yaml-project\n"), &v1))
	require.NotNil(t, v1.Hub)
	assert.Equal(t, "yaml-project", v1.Hub.GCPProjectID)

	gc := ConvertV1ServerToGlobalConfig(&v1)
	assert.Equal(t, "yaml-project", gc.Hub.GCPProjectID)

	back := ConvertGlobalToV1ServerConfig(gc)
	require.NotNil(t, back.Hub)
	assert.Equal(t, "yaml-project", back.Hub.GCPProjectID)

	out, err := yaml.Marshal(back)
	require.NoError(t, err)
	assert.Contains(t, string(out), "gcp_project_id: yaml-project")
}

func TestHubGCPProjectID_UnsetStaysEmpty(t *testing.T) {
	gc := ConvertV1ServerToGlobalConfig(&V1ServerConfig{Hub: &V1ServerHubConfig{Port: 9000}})
	assert.Empty(t, gc.Hub.GCPProjectID)
	out, err := yaml.Marshal(ConvertGlobalToV1ServerConfig(gc))
	require.NoError(t, err)
	assert.NotContains(t, string(out), "gcp_project_id")
}

func writeHubGCPProjectHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	globalDir := filepath.Join(home, ".scion")
	require.NoError(t, os.MkdirAll(globalDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(hubGCPProjectIDSettingsYAML), 0o644))
	return globalDir
}

func TestHubGCPProjectID_LoadGlobalConfigFromSettingsYAML(t *testing.T) {
	globalDir := writeHubGCPProjectHome(t)
	t.Setenv("SCION_SERVER_HUB_GCPPROJECTID", "")

	gc, err := LoadGlobalConfig(globalDir)
	require.NoError(t, err)
	assert.Equal(t, "file-project", gc.Hub.GCPProjectID)
}

func TestHubGCPProjectID_EnvWinsOverSettingsYAML(t *testing.T) {
	globalDir := writeHubGCPProjectHome(t)
	t.Setenv("SCION_SERVER_HUB_GCPPROJECTID", "env-project")

	gc, err := LoadGlobalConfig(globalDir)
	require.NoError(t, err)
	assert.Equal(t, "env-project", gc.Hub.GCPProjectID)
}

func TestHubGCPProjectID_VersionedSettingsHoldsKey(t *testing.T) {
	globalDir := writeHubGCPProjectHome(t)
	t.Setenv("SCION_SERVER_HUB_GCPPROJECTID", "")

	vs, err := LoadVersionedSettings(globalDir)
	require.NoError(t, err)
	require.NotNil(t, vs.Server)
	require.NotNil(t, vs.Server.Hub)
	assert.Equal(t, "file-project", vs.Server.Hub.GCPProjectID)
}

func TestHubGCPProjectID_EnvTableEntryFromSchema(t *testing.T) {
	path, alias, ok := KeyForServerEnv("SCION_SERVER_HUB_GCPPROJECTID")
	require.True(t, ok)
	assert.False(t, alias)
	assert.Equal(t, HubGCPProjectIDKey, path)
	assert.Empty(t, serverEnvExtraKeys, "the key has a schema node now")
	for _, e := range ServerEnvEntries() {
		if e.Path == HubGCPProjectIDKey {
			assert.Equal(t, "hub.gcpProjectId", e.GlobalConfig)
			assert.Equal(t, "string", e.Type)
			return
		}
	}
	t.Fatalf("no env table entry for %s", HubGCPProjectIDKey)
}

// The settings schema accepts the key (before ptone/scion#3830 it had no
// node), and still rejects a non-string value.
func TestHubGCPProjectID_SchemaAcceptsKey(t *testing.T) {
	errs, err := ValidateSettings([]byte(hubGCPProjectIDSettingsYAML), "1")
	require.NoError(t, err)
	assert.Empty(t, errs, "server.hub.gcp_project_id must validate")

	errs, err = ValidateSettings([]byte("schema_version: \"1\"\nserver:\n  hub:\n    gcp_project_id: 7\n"), "1")
	require.NoError(t, err)
	assert.NotEmpty(t, errs, "a non-string server.hub.gcp_project_id must not validate")
}

// writeHubGCPProjectLegacyHome writes settings.yaml (settingsYAML) and a
// legacy server.yaml that sets hub.gcpProjectId, and returns the global dir.
func writeHubGCPProjectLegacyHome(t *testing.T, settingsYAML string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SCION_SERVER_HUB_GCPPROJECTID", "")
	globalDir := filepath.Join(home, ".scion")
	require.NoError(t, os.MkdirAll(globalDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(settingsYAML), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "server.yaml"), []byte("hub:\n  gcpProjectId: legacy-project\n"), 0o644))
	return globalDir
}

// settings.yaml with a server block replaces server.yaml as a whole, not
// key by key: a legacy hub.gcpProjectId is not read when the server block
// has no gcp_project_id.
func TestHubGCPProjectID_ServerBlockIgnoresLegacyServerYAML(t *testing.T) {
	globalDir := writeHubGCPProjectLegacyHome(t, "schema_version: \"1\"\nserver:\n  hub:\n    port: 9810\n")
	gc, err := LoadGlobalConfig(globalDir)
	require.NoError(t, err)
	assert.Equal(t, 9810, gc.Hub.Port, "the settings.yaml server block is used")
	assert.Empty(t, gc.Hub.GCPProjectID, "server.yaml is ignored as a whole")
}

// Without a server key in settings.yaml, the legacy server.yaml is read and
// its hub.gcpProjectId applies.
func TestHubGCPProjectID_LegacyServerYAMLWithoutServerBlock(t *testing.T) {
	globalDir := writeHubGCPProjectLegacyHome(t, "schema_version: \"1\"\n")
	gc, err := LoadGlobalConfig(globalDir)
	require.NoError(t, err)
	assert.Equal(t, "legacy-project", gc.Hub.GCPProjectID)
}
