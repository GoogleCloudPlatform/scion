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

package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestResolveHubAuthURL_Precedence pins the hub URL order of hub auth login
// (ptone/scion#3537): --hub-url > --hub > SCION_HUB_ENDPOINT > settings.
func TestResolveHubAuthURL_Precedence(t *testing.T) {
	env := func(v string) func(string) string {
		return func(k string) string {
			if k == "SCION_HUB_ENDPOINT" {
				return v
			}
			return ""
		}
	}
	settings := func(v string) func() string { return func() string { return v } }

	tests := []struct {
		name                    string
		hubURL, rootHub, envVal string
		settingsVal             string
		wantURL, wantSource     string
	}{
		{"hub-url wins", "https://a", "https://b", "https://c", "https://d", "https://a", "--hub-url"},
		{"root --hub next", "", "https://b", "https://c", "https://d", "https://b", "--hub"},
		{"env next", "", "", "https://c", "https://d", "https://c", "env"},
		{"settings last", "", "", "", "https://d", "https://d", "settings"},
		{"none", "", "", "", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			url, src := resolveHubAuthURL(tt.hubURL, tt.rootHub, env(tt.envVal), settings(tt.settingsVal))
			assert.Equal(t, tt.wantURL, url)
			assert.Equal(t, tt.wantSource, src)
		})
	}
}

func TestHubAuthLoginHelpDocumentsPrecedence(t *testing.T) {
	assert.Contains(t, hubAuthLoginCmd.Long, "1. --hub-url")
	assert.Contains(t, hubAuthLoginCmd.Long, "2. the root --hub flag")
	assert.Contains(t, hubAuthLoginCmd.Long, "3. the SCION_HUB_ENDPOINT")
	assert.Contains(t, hubAuthLoginCmd.Long, "4. hub.endpoint in settings")
}

// loginEndpointHome sets up an isolated HOME with a global settings file and
// returns the global dir.
func loginEndpointHome(t *testing.T, settingsYAML string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SCION_HUB_ENDPOINT", "")
	t.Setenv("SCION_HUB_URL", "")
	globalDir := filepath.Join(home, ".scion")
	require.NoError(t, os.MkdirAll(globalDir, 0o755))
	if settingsYAML != "" {
		require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(settingsYAML), 0o644))
	}
	return globalDir
}

// TestPersistLoginEndpoint_SavesEndpointWhenUnset covers ptone/scion#3532:
// after login against a hub with no hub.endpoint configured, the endpoint is
// saved so hub status and other hub commands find the hub.
func TestPersistLoginEndpoint_SavesEndpointWhenUnset(t *testing.T) {
	globalDir := loginEndpointHome(t, "schema_version: \"1\"\n")
	var out bytes.Buffer
	asked := 0
	require.NoError(t, persistLoginEndpoint(&out, loginEndpointOptions{
		HubURL: "https://hub.example.com", ProjectPath: globalDir, IsGlobal: true,
		Interactive: false,
		Confirm:     func(string) bool { asked++; return true },
	}))

	s, err := config.LoadSettingsFromDir(globalDir)
	require.NoError(t, err)
	require.NotNil(t, s.Hub)
	assert.Equal(t, "https://hub.example.com", s.Hub.Endpoint)
	assert.False(t, s.IsHubEnabled(), "never enabled without an interactive answer")
	assert.Zero(t, asked, "no prompt when not interactive")
	assert.Contains(t, out.String(), "Saved hub endpoint https://hub.example.com to global settings")
	assert.Contains(t, out.String(), "scion hub enable")
}

func TestPersistLoginEndpoint_InteractiveEnable(t *testing.T) {
	globalDir := loginEndpointHome(t, "")
	var out bytes.Buffer
	require.NoError(t, persistLoginEndpoint(&out, loginEndpointOptions{
		HubURL: "https://hub.example.com", ProjectPath: globalDir, IsGlobal: true,
		Interactive: true,
		Confirm:     func(string) bool { return true },
	}))
	s, err := config.LoadSettingsFromDir(globalDir)
	require.NoError(t, err)
	assert.Equal(t, "https://hub.example.com", s.Hub.Endpoint)
	assert.True(t, s.IsHubEnabled())
	assert.Contains(t, out.String(), "Hub mode enabled (global scope)")
}

func TestPersistLoginEndpoint_InteractiveDecline(t *testing.T) {
	globalDir := loginEndpointHome(t, "")
	var out bytes.Buffer
	require.NoError(t, persistLoginEndpoint(&out, loginEndpointOptions{
		HubURL: "https://hub.example.com", ProjectPath: globalDir, IsGlobal: true,
		Interactive: true,
		Confirm:     func(string) bool { return false },
	}))
	s, err := config.LoadSettingsFromDir(globalDir)
	require.NoError(t, err)
	assert.False(t, s.IsHubEnabled())
	assert.Contains(t, out.String(), "scion hub enable")
}

func TestPersistLoginEndpoint_NeverOverwritesOtherEndpoint(t *testing.T) {
	const src = "schema_version: \"1\"\nhub:\n  endpoint: https://other.example.com\n"
	globalDir := loginEndpointHome(t, src)
	var out bytes.Buffer
	require.NoError(t, persistLoginEndpoint(&out, loginEndpointOptions{
		HubURL: "https://hub.example.com", ProjectPath: globalDir, IsGlobal: true,
		Interactive: true,
		Confirm:     func(string) bool { t.Fatal("must not prompt"); return false },
	}))
	data, err := os.ReadFile(filepath.Join(globalDir, "settings.yaml"))
	require.NoError(t, err)
	assert.Equal(t, src, string(data))
	assert.Contains(t, out.String(), "left unchanged")
	assert.Contains(t, out.String(), "--hub https://hub.example.com")
}

func TestPersistLoginEndpoint_SameEndpointAlreadyEnabled(t *testing.T) {
	const src = "schema_version: \"1\"\nhub:\n  enabled: true\n  endpoint: https://hub.example.com/\n"
	globalDir := loginEndpointHome(t, src)
	var out bytes.Buffer
	require.NoError(t, persistLoginEndpoint(&out, loginEndpointOptions{
		HubURL: "https://hub.example.com", ProjectPath: globalDir, IsGlobal: true,
		Interactive: true,
		Confirm:     func(string) bool { t.Fatal("must not prompt"); return false },
	}))
	data, err := os.ReadFile(filepath.Join(globalDir, "settings.yaml"))
	require.NoError(t, err)
	assert.Equal(t, src, string(data))
	assert.Empty(t, out.String())
}
