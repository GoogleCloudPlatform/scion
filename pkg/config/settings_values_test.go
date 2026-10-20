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

	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// settingSourceCase is one environment-and-file combination for the
// setting sources table.
type settingSourceCase struct {
	name    string
	global  string
	project string
	env     map[string]string
	idFile  string
	// want maps a key to its expected value and source kind: "default",
	// "global", "project", "env SCION_X" or "project-id file"; an empty
	// kind means unset.
	want map[string][2]string
}

func TestResolveSettingValues_SourcesAndAgreement(t *testing.T) {
	cases := []settingSourceCase{
		{
			name: "no files",
			want: map[string][2]string{
				"cli.autohelp":   {"true", "default"},
				"active_profile": {"local", "default"},
				"hub.endpoint":   {"", ""},
				"cli.mode":       {"", ""},
			},
		},
		{
			name:   "global only",
			global: "schema_version: \"1\"\ncli:\n  autohelp: false\nhub:\n  endpoint: https://g.example.com\n",
			want: map[string][2]string{
				"cli.autohelp":   {"false", "global"},
				"hub.endpoint":   {"https://g.example.com", "global"},
				"active_profile": {"local", "default"},
			},
		},
		{
			name:    "project over global",
			global:  "schema_version: \"1\"\ncli:\n  autohelp: false\nhub:\n  endpoint: https://g.example.com\n",
			project: "schema_version: \"1\"\ncli:\n  autohelp: true\n  mode: agent\nactive_profile: remote\n",
			want: map[string][2]string{
				"cli.autohelp":   {"true", "project"},
				"cli.mode":       {"agent", "project"},
				"active_profile": {"remote", "project"},
				"hub.endpoint":   {"https://g.example.com", "global"},
			},
		},
		{
			name:    "repeated value takes over the source",
			global:  "schema_version: \"1\"\nhub:\n  endpoint: https://same.example.com\n",
			project: "schema_version: \"1\"\nhub:\n  endpoint: https://same.example.com\n",
			want: map[string][2]string{
				"hub.endpoint": {"https://same.example.com", "project"},
			},
		},
		{
			name:    "key in another case",
			project: "schema_version: \"1\"\nhub:\n  Endpoint: https://case.example.com\n",
			want: map[string][2]string{
				"hub.endpoint": {"https://case.example.com", "project"},
			},
		},
		{
			name:    "env over files",
			global:  "schema_version: \"1\"\nhub:\n  endpoint: https://g.example.com\n",
			project: "schema_version: \"1\"\ncli:\n  autohelp: true\n",
			env:     map[string]string{"SCION_HUB_ENDPOINT": "https://env.example.com", "SCION_CLI_AUTOHELP": "false"},
			want: map[string][2]string{
				"hub.endpoint": {"https://env.example.com", "env SCION_HUB_ENDPOINT"},
				"cli.autohelp": {"false", "env SCION_CLI_AUTOHELP"},
			},
		},
		{
			name:    "legacy-format project file",
			global:  "schema_version: \"1\"\n",
			project: "harnesses: {}\nhub:\n  endpoint: https://legacy.example.com\n  brokerId: broker-l\n",
			want: map[string][2]string{
				"hub.endpoint": {"https://legacy.example.com", "project"},
				"hub.brokerId": {"broker-l", "project"},
			},
		},
		{
			name:    "project-id file",
			project: "schema_version: \"1\"\nhub:\n  project_id: settings-id\n",
			idFile:  "file-id",
			want: map[string][2]string{
				projectkeys.ConfigHubProjectIDKey: {"file-id", "project-id file"},
			},
		},
		{
			name:   "secret masked",
			global: "schema_version: \"1\"\nserver:\n  broker:\n    broker_token: secret-value\n    broker_id: b-1\n",
			want: map[string][2]string{
				"hub.brokerToken": {"********", "global"},
				"hub.brokerId":    {"b-1", "global"},
			},
		},
		{
			name:   "hub connections and transport",
			global: "schema_version: \"1\"\nhub:\n  transport:\n    mode: iap\nhub_connections:\n  main:\n    endpoint: https://main.example.com\n",
			want: map[string][2]string{
				"hub.transport.mode":            {"iap", "global"},
				"hub.transport.audience":        {"", ""},
				"hub_connections.main.endpoint": {"https://main.example.com", "global"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			projectDir := setupSettingsViewTest(t, tc.global, tc.project)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if tc.idFile != "" {
				require.NoError(t, os.WriteFile(filepath.Join(projectDir, projectkeys.ProjectIDFile), []byte(tc.idFile+"\n"), 0o644))
			}

			values, err := ResolveSettingValues(projectDir)
			require.NoError(t, err)
			byKey := map[string]SettingValue{}
			for _, v := range values {
				byKey[v.Key] = v
			}

			// list and get agree on every listed key.
			for _, v := range values {
				got, err := ResolveSettingValue(projectDir, v.Key)
				require.NoError(t, err, v.Key)
				assert.Equal(t, v, got, "config get %s must equal config list", v.Key)
				if v.Value == "" {
					assert.Empty(t, v.Source, "%s: an unset setting has no source", v.Key)
				} else {
					assert.NotEmpty(t, v.Source, "%s: a set setting has a source", v.Key)
				}
			}

			for key, want := range tc.want {
				v, ok := byKey[key]
				require.True(t, ok, "config list lacks %s", key)
				assert.Equal(t, want[0], v.Value, "%s value", key)
				switch kind := want[1]; {
				case kind == "":
					assert.Empty(t, v.Source, "%s source", key)
				case strings.HasPrefix(kind, "env ") || kind == "default":
					assert.Equal(t, kind, v.Source, "%s source", key)
				default:
					assert.True(t, strings.HasPrefix(v.Source, kind+" "), "%s source = %q, want %s <file>", key, v.Source, kind)
				}
			}
		})
	}
}

// config get accepts the project ID aliases and the profile and runtime
// paths, with their sources.
func TestResolveSettingValue_AliasesAndNestedKeys(t *testing.T) {
	projectDir := setupSettingsViewTest(t, "schema_version: \"1\"\nprofiles:\n  local:\n    runtime: podman\n", "schema_version: \"1\"\nhub:\n  project_id: p-1\n")

	for _, key := range []string{"project_id", "hub.project_id", "hub.projectId"} {
		v, err := ResolveSettingValue(projectDir, key)
		require.NoError(t, err)
		assert.Equal(t, "p-1", v.Value, key)
		assert.True(t, strings.HasPrefix(v.Source, "project "), "%s source %q", key, v.Source)
	}
	v, err := ResolveSettingValue(projectDir, "profiles.local.runtime")
	require.NoError(t, err)
	assert.Equal(t, "podman", v.Value)
	assert.True(t, strings.HasPrefix(v.Source, "global "), v.Source)

	_, err = ResolveSettingValue(projectDir, "no.such.key")
	assert.Error(t, err)
}

// cli.mode is settable (ptone/scion#3314), and only to a mode that exists.
func TestUpdateSetting_CLIMode(t *testing.T) {
	projectDir := setupSettingsViewTest(t, "schema_version: \"1\"\n", "")
	require.NoError(t, UpdateSetting(projectDir, "cli.mode", "agent", false))
	s, err := LoadSettings(projectDir)
	require.NoError(t, err)
	require.NotNil(t, s.CLI)
	assert.Equal(t, "agent", s.CLI.Mode)
	v, err := ResolveSettingValue(projectDir, "cli.mode")
	require.NoError(t, err)
	assert.Equal(t, "agent", v.Value)

	for _, bad := range []string{"assistant", "robot"} {
		err := UpdateSetting(projectDir, "cli.mode", bad, false)
		require.Error(t, err, bad)
	}
	require.NoError(t, UpdateSetting(projectDir, "cli.mode", "", false), "an empty value clears the key")
	v, err = ResolveSettingValue(projectDir, "cli.mode")
	require.NoError(t, err)
	assert.Empty(t, v.Value)
}

// When layers spell a key in different cases, which spelling the decode
// used is not recorded, so no source is named rather than a possibly wrong
// layer. A single spelling still finds its layer.
func TestResolveSettingValue_DifferentSpellingsInTwoLayersHaveNoSource(t *testing.T) {
	projectDir := setupSettingsViewTest(t,
		"schema_version: \"1\"\nhub:\n  Endpoint: https://a.example.com\n",
		"schema_version: \"1\"\nHub:\n  endpoint: https://b.example.com\n")
	for i := 0; i < 20; i++ {
		v, err := ResolveSettingValue(projectDir, "hub.endpoint")
		require.NoError(t, err)
		require.NotEmpty(t, v.Value)
		assert.Empty(t, v.Source, "two spellings in two layers: the source must not be guessed")
	}

	assert.Equal(t, "global f", lookupSource(map[string]string{"hub.Endpoint": "global f"}, "hub.endpoint"))
	assert.Equal(t, "global f", lookupSource(map[string]string{"hub.Endpoint": "global f", "HUB.endpoint": "global f"}, "hub.endpoint"))
	assert.Equal(t, "", lookupSource(map[string]string{"hub.Endpoint": "global f", "Hub.endpoint": "project g"}, "hub.endpoint"))
	assert.Equal(t, "project g", lookupSource(map[string]string{"hub.endpoint": "project g", "Hub.Endpoint": "global f"}, "hub.endpoint"))
}
