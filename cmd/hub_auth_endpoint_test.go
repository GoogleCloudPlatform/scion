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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hubSources is one combination of the inputs the hub of an invocation
// comes from: hubEndpoint as resolveInvocationTarget leaves it (empty, the
// --hub flag, a link entry or the current context) and the two environment
// variables.
type hubSources struct {
	resolved, resolvedSource, envEndpoint, envURL string
}

// allHubSources returns every combination of the inputs, each set input
// carrying a distinct URL.
func allHubSources() []hubSources {
	var out []hubSources
	for _, r := range []hubSources{
		{},
		{resolved: "https://flag"},
		{resolved: "https://link", resolvedSource: targetSourceDirectory},
		{resolved: "https://context", resolvedSource: targetSourceContext},
	} {
		for mask := 0; mask < 4; mask++ {
			c := r
			if mask&1 != 0 {
				c.envEndpoint = "https://env-endpoint"
			}
			if mask&2 != 0 {
				c.envURL = "https://env-url"
			}
			out = append(out, c)
		}
	}
	return out
}

// setHubSources applies c to the package variables and the environment.
func setHubSources(t *testing.T, c hubSources) {
	t.Helper()
	hubEndpoint, hubSource = c.resolved, c.resolvedSource
	t.Setenv("SCION_HUB_ENDPOINT", c.envEndpoint)
	t.Setenv("SCION_HUB_URL", c.envURL)
}

// writeHubSettingsEverywhere writes hub.endpoint (and hub.transport) into
// the global settings and into the settings of a directory project, and
// moves into that project. The CLI must read none of it.
func writeHubSettingsEverywhere(t *testing.T) {
	t.Helper()
	writeHomeGlobalSettings(t, "hub:\n  endpoint: https://global-settings.example.com\n  transport:\n    mode: iap\n    audience: aud-global\n")
	makeDirProject(t, "https://project-settings.example.com")
}

// TestInvocationHub_Precedence pins the hub of an invocation: hubEndpoint
// (the --hub flag, a link entry or the current context, as the resolver
// filled it, with that source), then SCION_HUB_ENDPOINT, then
// SCION_HUB_URL. A hub.endpoint in the global or the project settings is
// never used.
func TestInvocationHub_Precedence(t *testing.T) {
	setupContextTest(t)
	writeHubSettingsEverywhere(t)
	for _, c := range allHubSources() {
		setHubSources(t, c)
		wantEP, wantSrc := "", useSourceNone
		switch {
		case c.resolved != "" && c.resolvedSource != "":
			wantEP, wantSrc = c.resolved, c.resolvedSource
		case c.resolved != "":
			wantEP, wantSrc = c.resolved, useSourceFlag
		case c.envEndpoint != "":
			wantEP, wantSrc = c.envEndpoint, useSourceEnv
		case c.envURL != "":
			wantEP, wantSrc = c.envURL, useSourceEnv
		}
		ep, src := invocationHub()
		assert.Equal(t, wantEP, ep, "inputs %+v", c)
		assert.Equal(t, wantSrc, src, "inputs %+v", c)
		assert.Equal(t, wantEP, invocationHubEndpoint(), "inputs %+v", c)
		assert.Equal(t, wantEP != "", hubConfigured(), "inputs %+v", c)
	}
}

// TestHubConfigured_IgnoresSettings: with hub.endpoint in the global and
// the project settings and nothing else, no hub is selected, so hub
// commands fail with the not-configured error.
func TestHubConfigured_IgnoresSettings(t *testing.T) {
	setupContextTest(t)
	writeHubSettingsEverywhere(t)
	require.NoError(t, resolveInvocationTarget(findCommand(t, "list"), ModeHuman))
	assert.False(t, hubConfigured(), "a settings endpoint selected a hub")
	assert.Empty(t, invocationHubEndpoint())
	assert.Empty(t, realHintHubEndpoint(), "the hub-check hint names no hub (the test binary stubs hintHubEndpoint)")
	_, err := getHubClient(nil)
	assert.ErrorIs(t, err, errHubNotConfigured)
}

// TestHubAuthURL: hub auth login and logout use the hub every hub command
// uses; only the deprecated --hub-url comes first, as a URL or a hub
// nickname.
func TestHubAuthURL(t *testing.T) {
	setupContextTest(t)
	writeHubSettingsEverywhere(t)
	for _, c := range allHubSources() {
		setHubSources(t, c)
		got, err := hubAuthURL("")
		require.NoError(t, err)
		assert.Equal(t, invocationHubEndpoint(), got, "inputs %+v", c)
		assert.Equal(t, invocationHubEndpoint(), getDefaultHubURL(), "logout, inputs %+v", c)

		got, err = hubAuthURL("https://hub-url.example.com")
		require.NoError(t, err)
		assert.Equal(t, "https://hub-url.example.com", got, "--hub-url wins, inputs %+v", c)
	}

	saveTestContext(t, ctxTestHub, "")
	got, err := hubAuthURL("prod")
	require.NoError(t, err)
	assert.Equal(t, ctxTestHub, got, "--hub-url nickname")
	_, err = hubAuthURL("nosuchhub")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown hub 'nosuchhub'")
}

// TestHubAuthLogin_NoHubSelected: without a selected hub, login fails with
// the shared not-configured error, whatever the settings say.
func TestHubAuthLogin_NoHubSelected(t *testing.T) {
	setupContextTest(t)
	writeHubSettingsEverywhere(t)
	origURL := hubAuthHubURL
	t.Cleanup(func() { hubAuthHubURL = origURL })
	hubAuthHubURL = ""
	err := runHubAuthLogin(hubAuthLoginCmd, nil)
	require.ErrorIs(t, err, errHubNotConfigured)
}

// TestHubAuthLoginHelpDocumentsPrecedence: the login help names every hub
// source, the link of the project directory and the current hub included,
// and no settings key.
func TestHubAuthLoginHelpDocumentsPrecedence(t *testing.T) {
	long := hubAuthLoginCmd.Long
	for _, want := range []string{
		"1. the root --hub flag",
		"2. the SCION_HUB_ENDPOINT environment variable, then SCION_HUB_URL",
		"3. the hub the current project directory is linked to ('scion hub link')",
		"4. the current hub ('scion use --hub <name>')",
		"--hub-url is a deprecated alias of --hub",
		"A login never changes settings files.",
		"To make hub <name> the current hub, run 'scion use --hub <name>'.",
		"the hub logged in to becomes the current hub",
		"A login never switches a hub that is already current.",
	} {
		assert.Contains(t, long, want)
	}
	assert.NotContains(t, long, "hub.endpoint")
}

// TestHubAuthLoginHubURLDeprecated: --hub-url is a hidden, deprecated
// alias of --hub.
func TestHubAuthLoginHubURLDeprecated(t *testing.T) {
	f := hubAuthLoginCmd.Flags().Lookup("hub-url")
	require.NotNil(t, f)
	assert.True(t, f.Hidden, "--hub-url is hidden")
	assert.Equal(t, "use --hub", f.Deprecated)
	assert.NotContains(t, hubAuthLoginCmd.Flags().FlagUsages(), "hub-url")
}

// TestInvocationHub_AgentEnvironmentWithoutSettings covers the CLI inside
// an agent container: the hub comes from the agent's environment and no
// --hub flag is needed. SCION_HUB_ENDPOINT wins over SCION_HUB_URL, and
// --hub wins over both.
func TestInvocationHub_AgentEnvironmentWithoutSettings(t *testing.T) {
	setupContextTest(t)
	t.Setenv("SCION_HUB_URL", "https://hub-url.example")
	assert.Equal(t, "https://hub-url.example", invocationHubEndpoint())

	t.Setenv("SCION_HUB_ENDPOINT", "https://hub-endpoint.example")
	assert.Equal(t, "https://hub-endpoint.example", invocationHubEndpoint())

	hubEndpoint = "https://flag.example"
	assert.Equal(t, "https://flag.example", invocationHubEndpoint())
}

// TestHubHelpTextsPointAtTheHubList: no help or error text tells users to
// set hub.endpoint, which the CLI does not read; they name the hub list
// commands instead.
func TestHubHelpTextsPointAtTheHubList(t *testing.T) {
	texts := map[string]string{
		"hub":            hubCmd.Long,
		"hub link":       hubLinkCmd.Long,
		"hub enable":     errHubEnableRemoved.Error(),
		"conversation":   findCommand(t, "conversation").Long,
		"messages":       findCommand(t, "messages").Long,
		"notifications":  findCommand(t, "notifications").Long,
		"runtime-broker": brokerJoinCmd.Long,
	}
	for name, text := range texts {
		assert.NotContains(t, text, "config set hub.endpoint", name)
		assert.NotContains(t, text, "hub.endpoint in settings", name)
		assert.Contains(t, text, "scion use --hub <name>", name)
	}
	for _, name := range []string{"hub", "hub link", "hub enable", "conversation", "messages", "notifications"} {
		assert.Contains(t, texts[name], "scion hub add <name> <url>", name)
	}
	for _, name := range []string{"hub", "hub link", "hub enable"} {
		assert.Contains(t, strings.ReplaceAll(texts[name], "\n", " "), "scion server start", name)
	}
}

// TestHubSettingsFilesUnchangedByResolution: resolving the hub, including
// the one-time imports of the global and a linked project's endpoint,
// never writes a settings file.
func TestHubSettingsFilesUnchangedByResolution(t *testing.T) {
	home := setupContextTest(t)
	enableGlobalHubImport(t)
	writeHomeGlobalSettings(t, "hub:\n  endpoint: https://global.example.com\n")
	listTestHub(t, "project", "https://project.example.com")
	dotScion := makeLinkedDirProject(t, "https://project.example.com")
	globalFile := filepath.Join(home, ".scion", "settings.yaml")
	projectFile := filepath.Join(dotScion, "settings.yaml")
	beforeGlobal, err := os.ReadFile(globalFile)
	require.NoError(t, err)
	beforeProject, err := os.ReadFile(projectFile)
	require.NoError(t, err)

	require.NoError(t, resolveInvocationTarget(findCommand(t, "list"), ModeHuman))
	require.Equal(t, "https://project.example.com", hubEndpoint, "the imported link is used")

	afterGlobal, err := os.ReadFile(globalFile)
	require.NoError(t, err)
	afterProject, err := os.ReadFile(projectFile)
	require.NoError(t, err)
	assert.Equal(t, string(beforeGlobal), string(afterGlobal))
	assert.Equal(t, string(beforeProject), string(afterProject))
}
