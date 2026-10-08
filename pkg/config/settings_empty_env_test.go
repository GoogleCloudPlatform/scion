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
)

// emptyEnvSettingsYAML holds a settings-file value for every env var the
// empty-env tests exercise.
const emptyEnvSettingsYAML = `schema_version: "1"
default_template: file-template
hub:
  endpoint: https://file-hub.example.com
server:
  broker:
    broker_name: file-broker
`

// envOverlayCase is one state of an env var: unset, exported but empty, or
// set to a value.
type envOverlayCase struct {
	name  string
	state string // "unset", "empty" or "set"
}

var envOverlayCases = []envOverlayCase{
	{"unset", "unset"},
	{"empty", "empty"},
	{"set", "set"},
}

// setupEmptyEnvProject writes emptyEnvSettingsYAML into a fresh project
// under a temporary HOME and clears SCION_* variables an agent container
// may have exported, so the test sees only the variable under test.
func setupEmptyEnvProject(t *testing.T) string {
	t.Helper()
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	unsetTestEnv(t,
		"SCION_HUB_ENDPOINT", "SCION_DEFAULT_TEMPLATE",
		"SCION_SERVER_BROKER_BROKER_NAME", "SCION_PROJECT",
		"SCION_PROJECT_ID", "SCION_HUB_PROJECT_ID", "SCION_AUTO_EXPOSE_PORTS")
	projectDir := filepath.Join(tmpDir, "my-project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(emptyEnvSettingsYAML), 0644))
	return projectDir
}

// applyEnvState puts name into the given state for the rest of the test.
func applyEnvState(t *testing.T, name, state, value string) {
	t.Helper()
	switch state {
	case "unset":
		// t.Setenv first so the original value is restored on cleanup.
		t.Setenv(name, "")
		require.NoError(t, os.Unsetenv(name))
	case "empty":
		t.Setenv(name, "")
	case "set":
		t.Setenv(name, value)
	}
}

// expectedFor returns the env value when the variable is set to a non-empty
// value, and the settings-file value otherwise: an exported but empty
// variable is treated as unset.
func expectedFor(state, fileValue, envValue string) string {
	if state == "set" {
		return envValue
	}
	return fileValue
}

func TestLoadSettingsKoanf_EmptyEnvTreatedAsUnset(t *testing.T) {
	vars := []struct {
		env, fileValue, envValue string
		get                      func(*Settings) string
	}{
		{"SCION_HUB_ENDPOINT", "https://file-hub.example.com", "https://env-hub.example.com", func(s *Settings) string { return s.GetHubEndpoint() }},
		{"SCION_DEFAULT_TEMPLATE", "file-template", "env-template", func(s *Settings) string { return s.DefaultTemplate }},
	}
	for _, v := range vars {
		for _, c := range envOverlayCases {
			t.Run(v.env+"/"+c.name, func(t *testing.T) {
				projectDir := setupEmptyEnvProject(t)
				applyEnvState(t, v.env, c.state, v.envValue)

				s, err := LoadSettingsKoanf(projectDir)
				require.NoError(t, err)
				assert.Equal(t, expectedFor(c.state, v.fileValue, v.envValue), v.get(s))
			})
		}
	}
}

func TestLoadVersionedSettings_EmptyEnvTreatedAsUnset(t *testing.T) {
	vars := []struct {
		env, fileValue, envValue string
		get                      func(*VersionedSettings) string
	}{
		{"SCION_HUB_ENDPOINT", "https://file-hub.example.com", "https://env-hub.example.com", func(vs *VersionedSettings) string {
			if vs.Hub == nil {
				return ""
			}
			return vs.Hub.Endpoint
		}},
		{"SCION_DEFAULT_TEMPLATE", "file-template", "env-template", func(vs *VersionedSettings) string { return vs.DefaultTemplate }},
		{"SCION_SERVER_BROKER_BROKER_NAME", "file-broker", "env-broker", func(vs *VersionedSettings) string {
			if vs.Server == nil || vs.Server.Broker == nil {
				return ""
			}
			return vs.Server.Broker.BrokerName
		}},
	}
	for _, v := range vars {
		for _, c := range envOverlayCases {
			t.Run(v.env+"/"+c.name, func(t *testing.T) {
				projectDir := setupEmptyEnvProject(t)
				applyEnvState(t, v.env, c.state, v.envValue)

				vs, err := LoadVersionedSettings(projectDir)
				require.NoError(t, err)
				assert.Equal(t, expectedFor(c.state, v.fileValue, v.envValue), v.get(vs))
			})
		}
	}
}
