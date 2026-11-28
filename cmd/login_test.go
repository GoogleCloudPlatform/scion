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
	"reflect"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoginCommand_SharesRunEAndFlagsWithHubAuthLogin(t *testing.T) {
	top := findCommand(t, "login")
	nested := findCommand(t, "hub", "auth", "login")

	require.NotNil(t, top)
	require.NotNil(t, nested)
	assert.NotSame(t, top, nested, "top-level login and hub auth login must be distinct cobra.Command instances")

	assert.Equal(t, "Log in to the Scion Hub (alias for 'scion hub auth login')", top.Short)
	assert.Equal(t, reflect.ValueOf(nested.RunE).Pointer(), reflect.ValueOf(top.RunE).Pointer(),
		"top-level login and hub auth login must share RunE")

	for _, flagName := range []string{"no-browser", "provider"} {
		topFlag := top.Flags().Lookup(flagName)
		nestedFlag := nested.Flags().Lookup(flagName)
		require.NotNil(t, topFlag, "top-level login missing --%s", flagName)
		require.NotNil(t, nestedFlag, "hub auth login missing --%s", flagName)
		assert.Equal(t, nestedFlag.DefValue, topFlag.DefValue, "--%s default mismatch", flagName)
		assert.Equal(t, nestedFlag.Usage, topFlag.Usage, "--%s usage mismatch", flagName)
	}

	// Hidden deprecated --hub-url stays only on 'scion hub auth login'.
	assert.Nil(t, top.Flags().Lookup("hub-url"), "top-level login must not carry deprecated --hub-url")
	nestedHubURL := nested.Flags().Lookup("hub-url")
	require.NotNil(t, nestedHubURL)
	assert.True(t, nestedHubURL.Hidden)
	assert.Equal(t, "use --hub", nestedHubURL.Deprecated)
}

func TestLoginCommand_FlagsIsolatedFromHubAuthLogin(t *testing.T) {
	setupLoginNoCurrentHubTest(t)

	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)

	// Setting deprecated hubAuthHubURL on hubAuthLoginCmd must not affect top-level loginCmd.
	hubAuthHubURL = "https://stale-deprecated.example.test"
	hubEndpoint = srv.URL
	require.NoError(t, resolveInvocationTarget(loginCmd, ModeHuman))

	c := newTopLevelLoginCommand()
	require.NoError(t, c.Flags().Set("no-browser", "true"))
	var out, errOut bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&errOut)
	c.SetContext(context.Background())

	err := c.RunE(c, nil)
	require.Error(t, err, "test server returns 404 for device flow")
	assert.Contains(t, out.String(), "Authenticating with Hub at "+srv.URL)
	assert.NotContains(t, out.String(), "stale-deprecated.example.test")
	assert.False(t, hubAuthNoBrowser, "setting --no-browser on login command must not mutate global hubAuthNoBrowser")
	noBrowserOnNested, getErr := hubAuthLoginCmd.Flags().GetBool("no-browser")
	require.NoError(t, getErr)
	assert.False(t, noBrowserOnNested, "setting --no-browser on login must not affect hub auth login")
}

func TestLoginCommand_RefusedInAgentMode(t *testing.T) {
	assert.False(t, agentAllowed["login"], "login must remain human-mode only (not in agentAllowed)")

	// Build a copy of the real root tree (as gateTestTree does) and apply agent mode.
	root, _ := gateTestTree(t)
	applyModeRestrictions(root, ModeAgent)

	// Top-level 'login' must be removed from the command tree.
	for _, c := range root.Commands() {
		assert.NotEqual(t, "login", c.Name(), "login must be removed in agent mode")
	}

	// Direct invocation and --help must report agentModeUnavailableError, like 'hub auth login'.
	err := agentModeGatedInvocationError(root, []string{"login"})
	require.Error(t, err)
	assert.True(t, isAgentModeUnavailableError(err), "got %v", err)
	assert.Equal(t, `command "scion login" is not available in agent mode`, err.Error())

	errHelp := agentModeGatedInvocationError(root, []string{"login", "--help"})
	require.Error(t, errHelp)
	assert.True(t, isAgentModeUnavailableError(errHelp), "got %v", errHelp)
	assert.Equal(t, `command "scion login" is not available in agent mode`, errHelp.Error())

	// 'scion help login' must also report unavailable in agent mode.
	errHelpTopic := agentModeRemovedCommand(root, []string{"login"})
	require.Error(t, errHelpTopic)
	assert.True(t, isAgentModeUnavailableError(errHelpTopic), "got %v", errHelpTopic)
	assert.Equal(t, `command "scion login" is not available in agent mode`, errHelpTopic.Error())

	// Same check for 'hub auth login' to confirm parity.
	errHubAuth := agentModeRemovedCommand(root, []string{"hub", "auth", "login"})
	require.Error(t, errHubAuth)
	assert.True(t, isAgentModeUnavailableError(errHubAuth), "got %v", errHubAuth)
	assert.Equal(t, `command "scion hub auth" is not available in agent mode`, errHubAuth.Error())
}

func TestLoginCommand_AllowedOnWindows(t *testing.T) {
	assert.True(t, windowsAllowed["login"], "login must be in windowsAllowed")

	root, ran := gateTestTree(t)
	applyPlatformRestrictions(root, "windows")

	var found *cobra.Command
	for _, c := range root.Commands() {
		if c.Name() == "login" {
			found = c
			break
		}
	}
	require.NotNil(t, found)
	assert.False(t, found.Hidden, "login must not be hidden on Windows")
	assert.Nil(t, windowsRefusal(found), "login must not be refused on Windows")

	root.SetArgs([]string{"login"})
	require.NoError(t, root.Execute())
	assert.Equal(t, []string{"login"}, *ran)
}
