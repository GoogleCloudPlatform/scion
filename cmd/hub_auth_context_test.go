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

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupLoginContextTest prepares a login whose hub comes from the current
// context: a global settings file and a directory project, neither with a
// hub endpoint. It returns the two settings file paths.
func setupLoginContextTest(t *testing.T) (globalFile, projectFile string) {
	t.Helper()
	setupContextTest(t)
	origHubURL := hubAuthHubURL
	t.Cleanup(func() { hubAuthHubURL = origHubURL })
	hubAuthHubURL = ""
	writeHomeGlobalSettings(t, "image_registry: registry.example.test/team\n")
	saveTestContext(t, ctxTestHub, ctxTestProjectID)
	root := makeDirProject(t, "")
	globalFile = filepath.Join(os.Getenv("HOME"), ".scion", "settings.yaml")
	projectFile = filepath.Join(root, ".scion", "settings.yaml")
	return globalFile, projectFile
}

func readFileT(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(data)
}

// loginRecord runs recordLoginHub for hubURL and returns what it printed
// on its two writers, checking that nothing went to the process stdout.
func loginRecord(t *testing.T, hubURL string) (out, errOut string) {
	t.Helper()
	var o, e strings.Builder
	stdout := captureStdout(t, func() { recordLoginHub(&o, &e, hubURL) })
	assert.Empty(t, stdout, "output goes to the given writers")
	return o.String(), e.String()
}

// TestRecordLoginHub_ContextHubLeavesSettings: a login on the hub the
// current context selected stores nothing in any settings file and says
// which hub it was.
func TestRecordLoginHub_ContextHubLeavesSettings(t *testing.T) {
	globalFile, projectFile := setupLoginContextTest(t)
	globalBefore, projectBefore := readFileT(t, globalFile), readFileT(t, projectFile)

	require.NoError(t, resolveInvocationTarget(findCommand(t, "hub", "auth", "login"), ModeHuman))
	require.Equal(t, targetSourceContext, hubSource)
	hubURL, err := hubAuthURL(hubAuthHubURL)
	require.NoError(t, err)
	require.Equal(t, ctxTestHub, hubURL)

	out, errOut := loginRecord(t, hubURL)
	assert.Equal(t, "Logged in to "+ctxTestHub+" (current hub, selected with 'scion use').\n", out)
	assert.Empty(t, errOut)
	assert.Equal(t, globalBefore, readFileT(t, globalFile), "global settings unchanged")
	assert.Equal(t, projectBefore, readFileT(t, projectFile), "project settings unchanged")
}

// TestRecordLoginHub_HubURLAddedNotSaved: with an explicit --hub-url, the
// hub is added to the hub list, nothing is saved in settings, the current
// hub stays, and a hint says how to select the new hub.
func TestRecordLoginHub_HubURLAddedNotSaved(t *testing.T) {
	globalFile, projectFile := setupLoginContextTest(t)
	globalBefore, projectBefore := readFileT(t, globalFile), readFileT(t, projectFile)
	hubAuthHubURL = "https://other.example.com"

	require.NoError(t, resolveInvocationTarget(findCommand(t, "hub", "auth", "login"), ModeHuman))
	hubURL, err := hubAuthURL(hubAuthHubURL)
	require.NoError(t, err)
	require.Equal(t, "https://other.example.com", hubURL)

	out, _ := loginRecord(t, hubURL)
	assert.Contains(t, out, "To make it the current hub, run 'scion use --hub other'.\n")
	assert.Equal(t, "Registered hub 'other' (https://other.example.com); rename with 'scion hub rename'.\n"+
		"To make it the current hub, run 'scion use --hub other'.\n", out)
	assert.Equal(t, globalBefore, readFileT(t, globalFile), "global settings unchanged")
	assert.Equal(t, projectBefore, readFileT(t, projectFile), "project settings unchanged")
	c := readTestContext(t)
	assert.NotNil(t, c.Hubs["other"])
	assert.Equal(t, "prod", c.CurrentHub, "a login does not switch the current hub")
}

// TestRecordLoginHub_FlagHubs: --hub with a nickname of a hub that is not
// current gets the hint only; --hub naming the current hub gets nothing;
// a hub from the environment gets nothing and is not added.
func TestRecordLoginHub_FlagHubs(t *testing.T) {
	t.Run("nickname, not current", func(t *testing.T) {
		setupLoginContextTest(t)
		require.NoError(t, config.UpdateCLIContext(func(c *config.CLIContext) error {
			return c.AddHub("staging", "https://staging.example.com")
		}))
		hubEndpoint = "staging"
		require.NoError(t, resolveInvocationTarget(findCommand(t, "hub", "auth", "login"), ModeHuman))
		out, _ := loginRecord(t, hubEndpoint)
		assert.Equal(t, "To make it the current hub, run 'scion use --hub staging'.\n", out)
	})
	t.Run("the current hub by URL", func(t *testing.T) {
		setupLoginContextTest(t)
		hubEndpoint = ctxTestHub + "/"
		require.NoError(t, resolveInvocationTarget(findCommand(t, "hub", "auth", "login"), ModeHuman))
		out, _ := loginRecord(t, hubEndpoint)
		assert.Empty(t, out)
	})
	t.Run("environment", func(t *testing.T) {
		setupLoginContextTest(t)
		t.Setenv("SCION_HUB_ENDPOINT", "https://env.example.com")
		require.NoError(t, resolveInvocationTarget(findCommand(t, "hub", "auth", "login"), ModeHuman))
		hubURL, err := hubAuthURL("")
		require.NoError(t, err)
		require.Equal(t, "https://env.example.com", hubURL)
		out, _ := loginRecord(t, hubURL)
		assert.Empty(t, out)
		_, h := readTestContext(t).HubByEndpoint(hubURL)
		assert.Nil(t, h, "a hub from the environment is not added")
	})
	t.Run("inside an agent", func(t *testing.T) {
		setupLoginContextTest(t)
		hubAuthHubURL = "https://other.example.com"
		t.Setenv("SCION_HOST_UID", "1000")
		out, _ := loginRecord(t, "https://other.example.com")
		assert.Empty(t, out)
		_, h := readTestContext(t).HubByEndpoint("https://other.example.com")
		assert.Nil(t, h)
	})
}

// setupLoginLinkTest prepares a login whose hub comes from the link entry
// of a linked directory project: a global settings file and a directory
// project, neither with a hub endpoint, and a current hub that differs
// from the linked one. It returns the two settings file paths.
func setupLoginLinkTest(t *testing.T) (globalFile, projectFile string) {
	t.Helper()
	setupContextTest(t)
	origHubURL := hubAuthHubURL
	t.Cleanup(func() { hubAuthHubURL = origHubURL })
	hubAuthHubURL = ""
	writeHomeGlobalSettings(t, "image_registry: registry.example.test/team\n")
	saveTestContext(t, ctxTestHub, ctxTestProjectID)
	dotScion := makeLinkedDirProject(t, "")
	saveLinkEntry(t, dotScion, linkEntryTestHub, "linked")
	return filepath.Join(os.Getenv("HOME"), ".scion", "settings.yaml"), filepath.Join(dotScion, "settings.yaml")
}

// TestRecordLoginHub_LinkEntryHubLeavesSettings: a login on the hub a
// linked directory resolves to through its link entry stores nothing in
// any settings file and says which hub it was.
func TestRecordLoginHub_LinkEntryHubLeavesSettings(t *testing.T) {
	globalFile, projectFile := setupLoginLinkTest(t)
	globalBefore, projectBefore := readFileT(t, globalFile), readFileT(t, projectFile)

	require.NoError(t, resolveInvocationTarget(findCommand(t, "hub", "auth", "login"), ModeHuman))
	require.Equal(t, targetSourceDirectory, hubSource)
	hubURL, err := hubAuthURL(hubAuthHubURL)
	require.NoError(t, err)
	require.Equal(t, linkEntryTestHub, hubURL)

	out, _ := loginRecord(t, hubURL)
	assert.Equal(t, "Logged in to "+linkEntryTestHub+" (the hub this directory is linked to).\n", out)
	assert.Equal(t, globalBefore, readFileT(t, globalFile), "global settings unchanged")
	assert.Equal(t, projectBefore, readFileT(t, projectFile), "project settings unchanged")
}

// TestRecordLoginHub_LinkEntryHubURLNotSaved: in a linked directory, an
// explicit --hub-url is added to the hub list and saved in no settings
// file.
func TestRecordLoginHub_LinkEntryHubURLNotSaved(t *testing.T) {
	globalFile, projectFile := setupLoginLinkTest(t)
	globalBefore, projectBefore := readFileT(t, globalFile), readFileT(t, projectFile)
	hubAuthHubURL = "https://other.example.com"

	require.NoError(t, resolveInvocationTarget(findCommand(t, "hub", "auth", "login"), ModeHuman))
	hubURL, err := hubAuthURL(hubAuthHubURL)
	require.NoError(t, err)
	require.Equal(t, "https://other.example.com", hubURL)

	_, _ = loginRecord(t, hubURL)
	assert.Equal(t, globalBefore, readFileT(t, globalFile), "global settings unchanged")
	assert.Equal(t, projectBefore, readFileT(t, projectFile), "project settings unchanged")
	_, h := readTestContext(t).HubByEndpoint(hubURL)
	assert.NotNil(t, h, "the --hub-url hub is added to the hub list")
}
