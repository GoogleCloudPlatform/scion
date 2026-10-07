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
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hubURLSources is one combination of the inputs hub auth login reads.
type hubURLSources struct {
	hubURL, rootHub, settings, envEndpoint, envURL string
}

func (c hubURLSources) getenv(k string) string {
	switch k {
	case "SCION_HUB_ENDPOINT":
		return c.envEndpoint
	case "SCION_HUB_URL":
		return c.envURL
	}
	return ""
}

func (c hubURLSources) settingsEndpoint() string { return c.settings }

// allHubURLSources returns every combination of set/unset for the five
// inputs, each set input carrying a distinct URL.
func allHubURLSources() []hubURLSources {
	var out []hubURLSources
	for mask := 0; mask < 32; mask++ {
		pick := func(bit int, v string) string {
			if mask&(1<<bit) != 0 {
				return v
			}
			return ""
		}
		out = append(out, hubURLSources{
			hubURL:      pick(0, "https://hub-url"),
			rootHub:     pick(1, "https://root-hub"),
			settings:    pick(2, "https://settings"),
			envEndpoint: pick(3, "https://env-endpoint"),
			envURL:      pick(4, "https://env-url"),
		})
	}
	return out
}

// TestResolveHubAuthURL_Precedence pins the hub URL order of hub auth login
// (ptone/scion#3627): --hub-url > --hub > settings > SCION_HUB_ENDPOINT >
// SCION_HUB_URL.
func TestResolveHubAuthURL_Precedence(t *testing.T) {
	tests := []struct {
		name                string
		in                  hubURLSources
		wantURL, wantSource string
	}{
		{"hub-url wins", hubURLSources{"https://a", "https://b", "https://c", "https://d", "https://e"}, "https://a", "--hub-url"},
		{"root --hub next", hubURLSources{"", "https://b", "https://c", "https://d", "https://e"}, "https://b", "--hub"},
		{"settings beat env", hubURLSources{"", "", "https://c", "https://d", "https://e"}, "https://c", "settings"},
		{"SCION_HUB_ENDPOINT next", hubURLSources{"", "", "", "https://d", "https://e"}, "https://d", "env"},
		{"SCION_HUB_URL last", hubURLSources{"", "", "", "", "https://e"}, "https://e", "env"},
		{"none", hubURLSources{}, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			url, src := resolveHubAuthURL(tt.in.hubURL, tt.in.rootHub, tt.in.getenv, tt.in.settingsEndpoint)
			assert.Equal(t, tt.wantURL, url)
			assert.Equal(t, tt.wantSource, src)
		})
	}
}

// TestResolveHubAuthURL_AllCombinations checks every combination of flags,
// settings and environment: the first set input in precedence order wins.
func TestResolveHubAuthURL_AllCombinations(t *testing.T) {
	for _, c := range allHubURLSources() {
		want, wantSource := "", ""
		for _, cand := range []struct{ v, src string }{
			{c.hubURL, "--hub-url"},
			{c.rootHub, "--hub"},
			{c.settings, "settings"},
			{c.envEndpoint, "env"},
			{c.envURL, "env"},
		} {
			if cand.v != "" {
				want, wantSource = cand.v, cand.src
				break
			}
		}
		url, src := resolveHubAuthURL(c.hubURL, c.rootHub, c.getenv, c.settingsEndpoint)
		assert.Equal(t, want, url, "inputs %+v", c)
		assert.Equal(t, wantSource, src, "inputs %+v", c)
	}
}

type fakeHubSettings string

func (f fakeHubSettings) GetHubEndpoint() string { return string(f) }

// TestResolveHubAuthURL_AgreesWithGetHubEndpoint guards against drift: with
// no --hub-url, hub auth login must pick the same hub as every other hub
// command (GetHubEndpoint) for the same flag, settings and environment.
func TestResolveHubAuthURL_AgreesWithGetHubEndpoint(t *testing.T) {
	origHub, origNoHub := hubEndpoint, noHub
	t.Cleanup(func() { hubEndpoint, noHub = origHub, origNoHub })
	noHub = false

	for _, c := range allHubURLSources() {
		if c.hubURL != "" {
			continue
		}
		hubEndpoint = c.rootHub
		t.Setenv("SCION_HUB_ENDPOINT", c.envEndpoint)
		t.Setenv("SCION_HUB_URL", c.envURL)

		want := GetHubEndpoint(fakeHubSettings(c.settings))
		got, _ := resolveHubAuthURL("", hubEndpoint, os.Getenv, c.settingsEndpoint)
		assert.Equal(t, want, got, "inputs %+v", c)
	}
}

func TestHubAuthLoginHelpDocumentsPrecedence(t *testing.T) {
	assert.Contains(t, hubAuthLoginCmd.Long, "1. --hub-url")
	assert.Contains(t, hubAuthLoginCmd.Long, "2. the root --hub flag")
	assert.Contains(t, hubAuthLoginCmd.Long, "3. hub.endpoint in settings")
	assert.Contains(t, hubAuthLoginCmd.Long, "4. the SCION_HUB_ENDPOINT")
	assert.Contains(t, hubAuthLoginCmd.Long, "5. the SCION_HUB_URL")
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
		HubURL: "https://hub.example.com", ProjectPath: globalDir, IsGlobal: true, TargetGlobal: true,
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
		HubURL: "https://hub.example.com", ProjectPath: globalDir, IsGlobal: true, TargetGlobal: true,
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
		HubURL: "https://hub.example.com", ProjectPath: globalDir, IsGlobal: true, TargetGlobal: true,
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
		HubURL: "https://hub.example.com", ProjectPath: globalDir, IsGlobal: true, TargetGlobal: true,
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
		HubURL: "https://hub.example.com", ProjectPath: globalDir, IsGlobal: true, TargetGlobal: true,
		Interactive: true,
		Confirm:     func(string) bool { t.Fatal("must not prompt"); return false },
	}))
	data, err := os.ReadFile(filepath.Join(globalDir, "settings.yaml"))
	require.NoError(t, err)
	assert.Equal(t, src, string(data))
	assert.Empty(t, out.String())
}

// loginProject creates a project directory (with a .scion settings file
// holding projectSettings) under the isolated HOME, changes into it, and
// returns its resolved project path.
func loginProject(t *testing.T, projectSettings string) string {
	t.Helper()
	root := t.TempDir()
	scionDir := filepath.Join(root, ".scion")
	require.NoError(t, os.MkdirAll(scionDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(scionDir, "settings.yaml"), []byte(projectSettings), 0o644))
	t.Chdir(root)
	resolved, isGlobal, err := config.ResolveProjectPath("")
	require.NoError(t, err)
	require.False(t, isGlobal)
	return resolved
}

func projectFileHub(t *testing.T, projectPath string) *config.HubClientConfig {
	t.Helper()
	s, err := config.LoadSettingsFromDir(config.GetProjectConfigDir(projectPath))
	require.NoError(t, err)
	return s.Hub
}

// TestPersistLoginEndpoint_ProjectWithoutHubConfigSavesGlobal: inside a
// project that carries no hub configuration, login saves the endpoint to
// global settings (credentials are global; project settings are often
// tracked in git).
func TestPersistLoginEndpoint_ProjectWithoutHubConfigSavesGlobal(t *testing.T) {
	globalDir := loginEndpointHome(t, "schema_version: \"1\"\n")
	proj := loginProject(t, "schema_version: \"1\"\n")

	target := loginEndpointTargetGlobal(proj, false, false)
	assert.True(t, target)
	var out bytes.Buffer
	require.NoError(t, persistLoginEndpoint(&out, loginEndpointOptions{
		HubURL: "https://hub.example.com", ProjectPath: proj, TargetGlobal: target,
	}))

	gs, err := config.LoadSettingsFromDir(globalDir)
	require.NoError(t, err)
	require.NotNil(t, gs.Hub)
	assert.Equal(t, "https://hub.example.com", gs.Hub.Endpoint)
	if h := projectFileHub(t, proj); h != nil {
		assert.Empty(t, h.Endpoint, "project settings untouched")
	}
	assert.Contains(t, out.String(), "to global settings")
}

// TestPersistLoginEndpoint_ProjectWithHubConfigSavesProject: a project that
// already carries hub configuration gets the endpoint in its own settings.
func TestPersistLoginEndpoint_ProjectWithHubConfigSavesProject(t *testing.T) {
	globalDir := loginEndpointHome(t, "schema_version: \"1\"\n")
	proj := loginProject(t, "schema_version: \"1\"\nhub:\n  enabled: true\n")

	target := loginEndpointTargetGlobal(proj, false, false)
	assert.False(t, target)
	var out bytes.Buffer
	require.NoError(t, persistLoginEndpoint(&out, loginEndpointOptions{
		HubURL: "https://hub.example.com", ProjectPath: proj, TargetGlobal: target,
	}))

	h := projectFileHub(t, proj)
	require.NotNil(t, h)
	assert.Equal(t, "https://hub.example.com", h.Endpoint)
	gs, err := config.LoadSettingsFromDir(globalDir)
	require.NoError(t, err)
	if gs.Hub != nil {
		assert.Empty(t, gs.Hub.Endpoint, "global settings untouched")
	}
	assert.Contains(t, out.String(), "to project settings")
}

func TestLoginEndpointTargetGlobal(t *testing.T) {
	loginEndpointHome(t, "schema_version: \"1\"\n")
	plain := loginProject(t, "schema_version: \"1\"\n")
	assert.True(t, loginEndpointTargetGlobal(plain, false, false), "default: global")
	assert.False(t, loginEndpointTargetGlobal(plain, false, true), "explicit --global=false: project")
	assert.True(t, loginEndpointTargetGlobal(plain, true, true), "global context is always global")
}

func TestExplicitNotGlobalFlag(t *testing.T) {
	newFlags := func(args ...string) *pflag.FlagSet {
		fs := pflag.NewFlagSet("t", pflag.ContinueOnError)
		fs.Bool("global", false, "")
		require.NoError(t, fs.Parse(args))
		return fs
	}
	assert.False(t, explicitNotGlobalFlag(newFlags()), "not given")
	assert.True(t, explicitNotGlobalFlag(newFlags("--global=false")), "explicit false")
	assert.False(t, explicitNotGlobalFlag(newFlags("--global")), "true")
	assert.False(t, explicitNotGlobalFlag(newFlags("--global=true")), "explicit true")
	assert.False(t, explicitNotGlobalFlag(pflag.NewFlagSet("none", pflag.ContinueOnError)), "no such flag")
}
