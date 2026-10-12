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
	writeGlobalSettings(t, "image_registry: registry.example.test/team\n")
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

// TestRecordLoginEndpoint_ContextHubLeavesSettings: a login on the hub the
// current context selected stores no hub.endpoint (and no hub.enabled) in
// any settings file and says so.
func TestRecordLoginEndpoint_ContextHubLeavesSettings(t *testing.T) {
	globalFile, projectFile := setupLoginContextTest(t)
	globalBefore, projectBefore := readFileT(t, globalFile), readFileT(t, projectFile)

	resolveInvocationTarget(findCommand(t, "hub", "auth", "login"), ModeHuman)
	require.Equal(t, targetSourceContext, hubSource)
	hubURL := resolveHubAuthURL(hubAuthHubURL, hubEndpoint, os.Getenv, settingsHubEndpoint)
	require.Equal(t, ctxTestHub, hubURL)

	out := captureStdout(t, func() { recordLoginEndpoint(hubAuthLoginCmd, hubURL) })
	assert.Equal(t, "Logged in to "+ctxTestHub+" (current hub, selected with 'scion use'); settings unchanged.\n", out)
	assert.Equal(t, globalBefore, readFileT(t, globalFile), "global settings unchanged")
	assert.Equal(t, projectBefore, readFileT(t, projectFile), "project settings unchanged")
}

// TestRecordLoginEndpoint_HubURLStillPersists: with an explicit --hub-url
// the endpoint is saved as before, even when a context hub exists.
func TestRecordLoginEndpoint_HubURLStillPersists(t *testing.T) {
	globalFile, _ := setupLoginContextTest(t)
	hubAuthHubURL = "https://other.example.com"

	resolveInvocationTarget(findCommand(t, "hub", "auth", "login"), ModeHuman)
	hubURL := resolveHubAuthURL(hubAuthHubURL, hubEndpoint, os.Getenv, settingsHubEndpoint)
	require.Equal(t, "https://other.example.com", hubURL)

	out := captureStdout(t, func() { recordLoginEndpoint(hubAuthLoginCmd, hubURL) })
	assert.Contains(t, out, "Saved hub endpoint https://other.example.com to global settings")
	s, err := config.LoadSettingsFromDir(filepath.Dir(globalFile))
	require.NoError(t, err)
	require.NotNil(t, s.Hub)
	assert.Equal(t, "https://other.example.com", s.Hub.Endpoint)
}
