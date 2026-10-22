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
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/credentials"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupLoginNoCurrentHubTest prepares a login with no current hub: no
// context.yaml and no --hub-url.
func setupLoginNoCurrentHubTest(t *testing.T) {
	t.Helper()
	setupContextTest(t)
	origHubURL := hubAuthHubURL
	t.Cleanup(func() { hubAuthHubURL = origHubURL })
	hubAuthHubURL = ""
}

// readContextFileOrEmpty returns context.yaml, or "" when it does not
// exist.
func readContextFileOrEmpty(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(contextFilePath())
	if os.IsNotExist(err) {
		return ""
	}
	require.NoError(t, err)
	return string(data)
}

// With no current hub, a successful login to a hub given with --hub or
// --hub-url (URL or nickname) makes that hub current and says so once.
func TestRecordLoginHub_NoCurrentHubSelectsLoginHub(t *testing.T) {
	const newHub = "https://new.example.com"
	token := &hubclient.CLITokenResponse{AccessToken: "tok", ExpiresIn: 3600}

	for _, tt := range []struct {
		name  string
		setup func(t *testing.T)
	}{
		{"--hub URL", func(t *testing.T) { hubEndpoint = newHub + "/" }},
		{"--hub-url URL", func(t *testing.T) { hubAuthHubURL = newHub }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			setupLoginNoCurrentHubTest(t)
			tt.setup(t)
			require.NoError(t, resolveInvocationTarget(findCommand(t, "hub", "auth", "login"), ModeHuman))
			hubURL, err := hubAuthURL(hubAuthHubURL)
			require.NoError(t, err)

			out := captureStdout(t, func() {
				require.NoError(t, finishHubAuthLogin(hubAuthLoginCmd, hubURL, token))
			})
			assert.Contains(t, out, "Registered hub 'new' ("+newHub+"); rename with 'scion hub rename'.\n"+
				"Hub 'new' ("+newHub+") is now the current hub.\n")
			assert.NotContains(t, out, "the current hub, run")
			c := readTestContext(t)
			require.NotNil(t, c.Hubs["new"])
			assert.Equal(t, newHub, c.Hubs["new"].Endpoint)
			assert.Equal(t, "new", c.CurrentHub, "the first login's hub becomes current")
			assert.True(t, credentials.IsAuthenticated(newHub))
		})
	}

	t.Run("--hub nickname", func(t *testing.T) {
		setupLoginNoCurrentHubTest(t)
		require.NoError(t, config.UpdateCLIContext(func(c *config.CLIContext) error {
			return c.AddHub("staging", "https://staging.example.com")
		}))
		hubEndpoint = "staging"
		require.NoError(t, resolveInvocationTarget(findCommand(t, "hub", "auth", "login"), ModeHuman))
		out, errOut := loginRecord(t, hubEndpoint)
		assert.Equal(t, "Hub 'staging' (https://staging.example.com) is now the current hub.\n", out)
		assert.Empty(t, errOut)
		assert.Equal(t, "staging", readTestContext(t).CurrentHub, "the nickname's hub becomes current")
	})

	t.Run("URL with user info is not registered or selected", func(t *testing.T) {
		setupLoginNoCurrentHubTest(t)
		hubAuthHubURL = "https://user:pw@new.example.com"
		out, errOut := loginRecord(t, hubAuthHubURL)
		assert.Empty(t, out)
		assert.Contains(t, errOut, "Warning: could not add https://user:xxxxx@new.example.com to the hub list")
		assert.Empty(t, readContextFileOrEmpty(t), "nothing is written")
	})
}

// With a current hub, a login to another hub never switches it: the hub is
// added and the hint says how to select it.
func TestRecordLoginHub_CurrentHubNotSwitched(t *testing.T) {
	setupLoginNoCurrentHubTest(t)
	saveTestContext(t, ctxTestHub, "")
	hubAuthHubURL = "https://other.example.com"
	require.NoError(t, resolveInvocationTarget(findCommand(t, "hub", "auth", "login"), ModeHuman))

	out, errOut := loginRecord(t, hubAuthHubURL)
	assert.Equal(t, "Registered hub 'other' (https://other.example.com); rename with 'scion hub rename'.\n"+
		"To make hub 'other' the current hub, run 'scion use --hub other'.\n", out)
	assert.Empty(t, errOut)
	c := readTestContext(t)
	assert.NotNil(t, c.Hubs["other"])
	assert.Equal(t, "prod", c.CurrentHub, "the current hub stays")
}

// With no current hub, a login whose hub comes from the environment does
// not write context.yaml.
func TestFinishHubAuthLogin_EnvHubNoCurrentHubWritesNothing(t *testing.T) {
	const envHub = "https://env.example.com"
	token := &hubclient.CLITokenResponse{AccessToken: "tok", ExpiresIn: 3600}
	setupLoginNoCurrentHubTest(t)
	t.Setenv("SCION_HUB_ENDPOINT", envHub)
	require.NoError(t, resolveInvocationTarget(findCommand(t, "hub", "auth", "login"), ModeHuman))
	hubURL, err := hubAuthURL(hubAuthHubURL)
	require.NoError(t, err)
	require.Equal(t, envHub, hubURL)
	before := readContextFileOrEmpty(t)

	out := captureStdout(t, func() {
		require.NoError(t, finishHubAuthLogin(hubAuthLoginCmd, hubURL, token))
	})
	assert.NotContains(t, out, "is now the current hub")
	assert.NotContains(t, out, "Registered hub")
	assert.Equal(t, before, readContextFileOrEmpty(t), "context.yaml is unchanged")
	assert.True(t, credentials.IsAuthenticated(envHub))
}

// A failed login writes nothing: no hub entry, no current hub, no
// credential.
func TestHubAuthLogin_FailureWritesNothing(t *testing.T) {
	setupLoginNoCurrentHubTest(t)
	origNoBrowser, origCtx := hubAuthNoBrowser, hubAuthLoginCmd.Context()
	t.Cleanup(func() {
		hubAuthNoBrowser = origNoBrowser
		hubAuthLoginCmd.SetOut(nil)
		hubAuthLoginCmd.SetErr(nil)
		hubAuthLoginCmd.SetContext(origCtx)
	})
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	hubAuthHubURL = srv.URL
	hubAuthNoBrowser = true // the device flow: its first request gets a 404
	var out, errOut bytes.Buffer
	hubAuthLoginCmd.SetOut(&out)
	hubAuthLoginCmd.SetErr(&errOut)
	hubAuthLoginCmd.SetContext(context.Background())

	require.Error(t, runHubAuthLogin(hubAuthLoginCmd, nil), "the test hub has no login endpoints")
	assert.NotContains(t, out.String(), "is now the current hub")
	assert.Empty(t, readContextFileOrEmpty(t), "nothing is written")
	assert.False(t, credentials.IsAuthenticated(hubAuthHubURL))
}

// With no current hub, --hub-url with a nickname makes that hub current.
func TestRecordLoginHub_NoCurrentHubHubURLNickname(t *testing.T) {
	setupLoginNoCurrentHubTest(t)
	require.NoError(t, config.UpdateCLIContext(func(c *config.CLIContext) error {
		return c.AddHub("staging", "https://staging.example.com")
	}))
	hubAuthHubURL = "staging"
	require.NoError(t, resolveInvocationTarget(findCommand(t, "hub", "auth", "login"), ModeHuman))
	hubURL, err := hubAuthURL(hubAuthHubURL)
	require.NoError(t, err)
	require.Equal(t, "https://staging.example.com", hubURL)

	out, errOut := loginRecord(t, hubURL)
	assert.Equal(t, "Hub 'staging' (https://staging.example.com) is now the current hub.\n", out)
	assert.Empty(t, errOut)
	assert.Equal(t, "staging", readTestContext(t).CurrentHub, "the --hub-url nickname's hub becomes current")
}

// With no current hub, a bare login in a linked directory makes the hub of
// the directory's link entry current and says so after the usual line.
func TestRecordLoginHub_LinkEntryHubNoCurrentHubSelected(t *testing.T) {
	setupLoginNoCurrentHubTest(t)
	dotScion := makeLinkedDirProject(t, "")
	saveLinkEntry(t, dotScion, linkEntryTestHub, "linked")
	require.Empty(t, readTestContext(t).CurrentHub)

	require.NoError(t, resolveInvocationTarget(findCommand(t, "hub", "auth", "login"), ModeHuman))
	require.Equal(t, targetSourceDirectory, hubSource)
	hubURL, err := hubAuthURL(hubAuthHubURL)
	require.NoError(t, err)
	require.Equal(t, linkEntryTestHub, hubURL)

	out, errOut := loginRecord(t, hubURL)
	assert.Equal(t, "Logged in to "+linkEntryTestHub+" (the hub this directory is linked to).\n"+
		"Hub 'linked' ("+linkEntryTestHub+") is now the current hub.\n", out)
	assert.Empty(t, errOut)
	assert.Equal(t, "linked", readTestContext(t).CurrentHub, "the link entry's hub becomes current")
}
