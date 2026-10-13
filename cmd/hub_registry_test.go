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
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/credentials"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// importGlobalHubSettingsAtInit is the value of the import switch before
// TestMain changes it: the value the CLI runs with.
var importGlobalHubSettingsAtInit = importGlobalHubSettings

// The global hub import is on in the CLI; only the cmd tests' TestMain
// turns it off.
func TestImportGlobalHubSettings_OnByDefault(t *testing.T) {
	assert.True(t, importGlobalHubSettingsAtInit)
}

// enableGlobalHubImport turns the global hub import on for one test, which
// must use a temp HOME (setupContextTest does).
func enableGlobalHubImport(t *testing.T) {
	t.Helper()
	importGlobalHubSettings = true
	t.Cleanup(func() { importGlobalHubSettings = false })
}

// setupRegistryTest is setupContextTest for the hub list commands.
func setupRegistryTest(t *testing.T) {
	t.Helper()
	setupContextTest(t)
	restoreAllSilenceUsage(t)
	t.Cleanup(func() { rootCmd.SetArgs(nil) })
}

// closedHubURL returns the URL of a hub that does not respond.
func closedHubURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(nil)
	u := srv.URL
	srv.Close()
	return u
}

// storeTestLogin stores an OAuth login for endpoint.
func storeTestLogin(t *testing.T, endpoint string) {
	t.Helper()
	require.NoError(t, credentials.Store(endpoint, &credentials.TokenResponse{
		AccessToken: "tok", ExpiresIn: time.Hour,
		User: &credentials.User{Email: "alice@example.com"},
	}))
}

func contextFilePath() string {
	return filepath.Join(os.Getenv("HOME"), ".scion", "context.yaml")
}

func TestHubAdd(t *testing.T) {
	setupRegistryTest(t)
	srv := healthOnlyHub(t)

	stdout, stderr, err := runScion(t, "hub", "add", "prod", srv.URL+"/")
	require.NoError(t, err)
	assert.Equal(t, "Added hub 'prod' ("+srv.URL+").\n"+
		"Not logged in to hub 'prod'. Run: scion hub auth login --hub prod\n", stdout)
	assert.Empty(t, stderr, "a hub that responds gives no warning")

	c := readTestContext(t)
	require.NotNil(t, c.Hubs["prod"])
	assert.Equal(t, srv.URL, c.Hubs["prod"].Endpoint, "stored normalised")
	assert.Empty(t, c.CurrentHub, "without --use the hub is not made current")

	info, err := os.Stat(contextFilePath())
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestHubAdd_UseMakesCurrent(t *testing.T) {
	setupRegistryTest(t)
	srv := healthOnlyHub(t)
	storeTestLogin(t, srv.URL)

	stdout, _, err := runScion(t, "hub", "add", "prod", srv.URL, "--use")
	require.NoError(t, err)
	assert.Equal(t, "Added hub 'prod' ("+srv.URL+").\nNow using hub 'prod'.\n", stdout, "logged in: no login hint")
	assert.Equal(t, "prod", readTestContext(t).CurrentHub)
}

// An unreachable hub is added anyway, with a warning.
func TestHubAdd_UnreachableIsAWarning(t *testing.T) {
	setupRegistryTest(t)
	url := closedHubURL(t)

	stdout, stderr, err := runScion(t, "hub", "add", "offline", url)
	require.NoError(t, err)
	assert.Contains(t, stdout, "Added hub 'offline' ("+url+").")
	assert.Contains(t, stderr, "Warning: the hub at "+url+" did not respond")
	assert.NotNil(t, readTestContext(t).Hubs["offline"])
}

func TestHubAdd_Errors(t *testing.T) {
	setupRegistryTest(t)
	srv := healthOnlyHub(t)
	_, _, err := runScion(t, "hub", "add", "prod", srv.URL)
	require.NoError(t, err)
	before, err := os.ReadFile(contextFilePath())
	require.NoError(t, err)

	tests := []struct {
		name string
		args []string
		want string
	}{
		{"duplicate name", []string{"prod", "https://other.example.com"}, `a hub named "prod" already exists`},
		{"duplicate URL", []string{"production", srv.URL + "/"}, `is already registered as hub "prod"`},
		{"invalid name", []string{"Prod_1", "https://other.example.com"}, "invalid hub name"},
		{"invalid URL", []string{"other", "other.example.com"}, "invalid hub URL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := runScion(t, append([]string{"hub", "add"}, tt.args...)...)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
			after, err := os.ReadFile(contextFilePath())
			require.NoError(t, err)
			assert.Equal(t, string(before), string(after), "a refused add writes nothing")
		})
	}
}

func TestHubAdd_JSON(t *testing.T) {
	setupRegistryTest(t)
	srv := healthOnlyHub(t)
	stdout, _, err := runScion(t, "hub", "add", "prod", srv.URL, "--use", "--format", "json")
	require.NoError(t, err)
	var got map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(stdout), &got))
	assert.Equal(t, map[string]interface{}{
		"name": "prod", "endpoint": srv.URL, "current": true, "reachable": true, "auth": "none",
	}, got)
}

func TestHubAdd_CorruptContextNeedsForce(t *testing.T) {
	setupRegistryTest(t)
	require.NoError(t, os.MkdirAll(filepath.Dir(contextFilePath()), 0o700))
	require.NoError(t, os.WriteFile(contextFilePath(), []byte("version: [1\n"), 0o600))

	_, _, err := runScion(t, "hub", "add", "prod", closedHubURL(t))
	require.ErrorIs(t, err, config.ErrCLIContextCorrupt)
	assert.Contains(t, err.Error(), "--force")

	_, _, err = runScion(t, "hub", "add", "prod", "https://hub.example.com", "--force")
	require.NoError(t, err)
	assert.NotNil(t, readTestContext(t).Hubs["prod"])
}

func TestHubList(t *testing.T) {
	setupRegistryTest(t)

	stdout, _, err := runScion(t, "hub", "list")
	require.NoError(t, err)
	assert.Equal(t, "No hubs registered. Add one with 'scion hub add <name> <url>'.\n", stdout)

	up := healthOnlyHub(t)
	down := closedHubURL(t)
	require.NoError(t, config.UpdateCLIContext(func(c *config.CLIContext) error {
		require.NoError(t, c.AddHub("prod", up.URL))
		require.NoError(t, c.AddHub("staging", down))
		c.CurrentHub = "prod"
		return nil
	}))
	storeTestLogin(t, up.URL)

	stdout, _, err = runScion(t, "hub", "list")
	require.NoError(t, err)
	w := maxLen("ENDPOINT", up.URL, down)
	assert.Equal(t,
		"CURRENT  NAME     "+padRight("ENDPOINT", w)+"  AUTH\n"+
			"*        prod     "+padRight(up.URL, w)+"  oauth (alice@example.com)\n"+
			"         staging  "+padRight(down, w)+"  none\n",
		stdout)

	stdout, _, err = runScion(t, "hub", "list", "--check")
	require.NoError(t, err)
	assert.Contains(t, stdout, "REACHABLE")
	assert.Contains(t, stdout, "REACHABLE  VERIFIED")
	assert.Regexp(t, `prod .* oauth \(alice@example.com\)\s+yes\s+-\n`, stdout, "no identity endpoint: not verified")
	assert.Regexp(t, `staging .* none\s+no\s+-\n`, stdout)

	stdout, _, err = runScion(t, "hub", "list", "--check", "--format", "json")
	require.NoError(t, err)
	var rows []map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(stdout), &rows))
	require.Len(t, rows, 2)
	assert.Equal(t, map[string]interface{}{
		"current": true, "name": "prod", "endpoint": up.URL,
		"auth":      map[string]interface{}{"method": "oauth", "user": "alice@example.com"},
		"reachable": true,
	}, rows[0])
	assert.Equal(t, false, rows[1]["current"])
	assert.Equal(t, false, rows[1]["reachable"])
	assert.Equal(t, map[string]interface{}{"method": "none"}, rows[1]["auth"])
}

// maxLen returns the length of the longest of values: the width tabwriter
// gives their column.
func maxLen(values ...string) int {
	n := 0
	for _, v := range values {
		if len(v) > n {
			n = len(v)
		}
	}
	return n
}

// padRight pads s with spaces to width.
func padRight(s string, width int) string {
	if len(s) >= width {
		return s
	}
	return s + string(bytes.Repeat([]byte(" "), width-len(s)))
}

// rename rewrites the current hub and the known projects, and the login,
// which is keyed by the endpoint, keeps working.
func TestHubRename(t *testing.T) {
	setupRegistryTest(t)
	saveTestContext(t, ctxTestHub, ctxTestProjectID)
	storeTestLogin(t, ctxTestHub)

	stdout, _, err := runScion(t, "hub", "rename", "prod", "production")
	require.NoError(t, err)
	assert.Equal(t, "Renamed hub 'prod' to 'production'.\n", stdout)

	c := readTestContext(t)
	assert.Equal(t, "production", c.CurrentHub)
	assert.Nil(t, c.Hubs["prod"])
	require.NotNil(t, c.Hubs["production"])
	assert.Equal(t, ctxTestProjectID, c.Hubs["production"].CurrentProject.ID)
	require.Len(t, c.Projects, 1)
	assert.Equal(t, "production", c.Projects[0].Hub)

	stdout, _, err = runScion(t, "hub", "list")
	require.NoError(t, err)
	assert.Contains(t, stdout, "production  "+ctxTestHub+"  oauth (alice@example.com)")

	_, _, err = runScion(t, "hub", "rename", "nope", "x")
	require.Error(t, err)
	assert.Equal(t, "unknown hub 'nope'; registered hubs: production", err.Error())
}

func TestHubRemove_CurrentKeepsLogin(t *testing.T) {
	setupRegistryTest(t)
	saveTestContext(t, ctxTestHub, ctxTestProjectID)
	storeTestLogin(t, ctxTestHub)

	stdout, _, err := runScion(t, "hub", "remove", "prod")
	require.NoError(t, err)
	assert.Equal(t, "Removed hub 'prod' ("+ctxTestHub+").\n"+
		"It was the current hub; pick another with 'scion use --hub <name>' ('scion hub list' shows them).\n"+
		"Note: the stored login for "+ctxTestHub+" was kept, so adding the hub again needs no login; 'scion hub auth logout --hub "+ctxTestHub+"' removes it.\n",
		stdout)

	c := readTestContext(t)
	assert.Empty(t, c.CurrentHub)
	assert.Empty(t, c.Hubs)
	assert.Empty(t, c.Projects)
	assert.True(t, credentials.IsAuthenticated(ctxTestHub), "without --logout the login stays")
}

func TestHubRemove_Logout(t *testing.T) {
	setupRegistryTest(t)
	saveTestContext(t, ctxTestHub, "")
	require.NoError(t, config.UpdateCLIContext(func(c *config.CLIContext) error {
		return c.AddHub("staging", "https://staging.example.com")
	}))
	storeTestLogin(t, ctxTestHub+"/")
	storeTestLogin(t, "https://staging.example.com")

	stdout, _, err := runScion(t, "hub", "remove", "prod", "--logout")
	require.NoError(t, err)
	assert.Contains(t, stdout, "Removed the stored login for "+ctxTestHub+".")
	assert.False(t, credentials.IsAuthenticated(ctxTestHub))
	assert.True(t, credentials.IsAuthenticated("https://staging.example.com"), "other logins stay")
	assert.Empty(t, readTestContext(t).CurrentHub)

	_, _, err = runScion(t, "hub", "remove", "prod")
	require.Error(t, err)
	assert.Equal(t, "unknown hub 'prod'; registered hubs: staging", err.Error())
}

// The registry commands need no project and are not available to agents.
func TestHubRegistryCommands_GateAndAgentMode(t *testing.T) {
	for _, name := range []string{"add", "list", "rename", "remove"} {
		c := findCommand(t, "hub", name)
		assert.False(t, commandRequiresProject(c), "hub %s needs no project", name)
		assert.False(t, agentAllowed["hub."+name], "hub %s is human-only", name)
	}
	// Inside an agent container (SCION_HOST_UID) with a non-local hub, the
	// container check passes, so the refusal comes from the command.
	setupRegistryTest(t)
	t.Setenv("SCION_HOST_UID", "1000")
	t.Setenv("SCION_HUB_ENDPOINT", "https://hub.example.com")
	for _, args := range [][]string{{"hub", "list"}, {"hub", "add", "prod", "https://other.example.com"}} {
		_, _, err := runScion(t, args...)
		require.Error(t, err, "%v", args)
		assert.Equal(t, "the hub list is not available inside an agent", err.Error(), "%v", args)
	}
	_, err := os.Stat(contextFilePath())
	assert.True(t, os.IsNotExist(err), "nothing is written inside an agent")
}

// --hub takes a nickname: the hook replaces it with the entry's endpoint.
func TestResolveInvocationTarget_HubNickname(t *testing.T) {
	setupContextTest(t)
	saveTestContext(t, ctxTestHub, ctxTestProjectID)
	hubEndpoint = "prod"

	require.NoError(t, resolveInvocationTarget(findCommand(t, "list"), ModeHuman))
	assert.Equal(t, ctxTestHub, hubEndpoint)
	assert.Equal(t, "prod", hubFlagName)
	assert.Empty(t, hubSource, "a nickname is a --hub flag, not the context")
	assert.Equal(t, ctxTestProjectID, projectPath, "the effective hub's remembered project applies")
	assert.Equal(t, ctxTestHub, hubFlagEndpoint())
}

func TestResolveInvocationTarget_UnknownNicknameListsNames(t *testing.T) {
	setupContextTest(t)
	saveTestContext(t, ctxTestHub, "")
	require.NoError(t, config.UpdateCLIContext(func(c *config.CLIContext) error {
		return c.AddHub("local", "http://127.0.0.1:8080")
	}))
	hubEndpoint = "staging"
	err := resolveInvocationTarget(findCommand(t, "list"), ModeHuman)
	require.Error(t, err)
	assert.Equal(t, "unknown hub 'staging'; registered hubs: local, prod", err.Error())

	// Through the root command, the error stops the invocation.
	restoreAllSilenceUsage(t)
	_, _, err = runScion(t, "--hub", "staging", "use")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown hub 'staging'")
}

func TestResolveInvocationTarget_UnknownNicknameNoHubs(t *testing.T) {
	setupContextTest(t)
	hubEndpoint = "prod"
	err := resolveInvocationTarget(findCommand(t, "list"), ModeHuman)
	require.Error(t, err)
	assert.Equal(t, "unknown hub 'prod': no hubs are registered; add one with 'scion hub add <name> <url>', or pass the hub URL", err.Error())
}

// A URL passed to --hub is used as it is and is not registered.
func TestResolveInvocationTarget_HubURLIsOneOff(t *testing.T) {
	setupContextTest(t)
	hubEndpoint = "https://other.example.com/"
	require.NoError(t, resolveInvocationTarget(findCommand(t, "list"), ModeHuman))
	assert.Equal(t, "https://other.example.com/", hubEndpoint)
	assert.Empty(t, hubFlagName)
	_, err := os.Stat(contextFilePath())
	assert.True(t, os.IsNotExist(err), "no context file is created")

	saveTestContext(t, ctxTestHub, "")
	before, err := os.ReadFile(contextFilePath())
	require.NoError(t, err)
	hubEndpoint = "https://other.example.com"
	require.NoError(t, resolveInvocationTarget(findCommand(t, "list"), ModeHuman))
	after, err := os.ReadFile(contextFilePath())
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after), "the URL is not added to the hub list")
}

// An explicit nickname resolves for hub shadow too, which otherwise ignores
// the context; the server and runtime-broker subtrees never read
// context.yaml, so a nickname is left as it is there, even with an
// unreadable file.
func TestResolveInvocationTarget_NicknameScope(t *testing.T) {
	setupContextTest(t)
	saveTestContext(t, ctxTestHub, "")

	hubEndpoint = "prod"
	require.NoError(t, resolveInvocationTarget(findCommand(t, "hub", "shadow"), ModeHuman))
	assert.Equal(t, ctxTestHub, hubEndpoint)
	assert.Empty(t, hubSource)

	require.NoError(t, os.WriteFile(contextFilePath(), []byte("version: [1\n"), 0o600))
	for _, args := range [][]string{{"server", "start"}, {"runtime-broker", "register"}} {
		hubEndpoint = "prod"
		stderr := captureStderr(t, func() {
			require.NoError(t, resolveInvocationTarget(findCommand(t, args...), ModeHuman))
		})
		assert.Equal(t, "prod", hubEndpoint, "%v", args)
		assert.Empty(t, stderr, "%v reads no context.yaml", args)
	}

	// Elsewhere an unreadable file makes a nickname an error.
	hubEndpoint = "prod"
	err := resolveInvocationTarget(findCommand(t, "list"), ModeHuman)
	require.ErrorIs(t, err, config.ErrCLIContextCorrupt)
	assert.Contains(t, err.Error(), "cannot look up hub 'prod'")
}

// The first run that reads the context imports the global hub.endpoint as
// a hub entry, written once, without changing the settings file. It does
// not select the hub: outside a project the CLI keeps following the global
// hub.endpoint, including a later change to it.
func TestResolveInvocationTarget_ImportsGlobalEndpoint(t *testing.T) {
	setupContextTest(t)
	enableGlobalHubImport(t)
	settings := "hub:\n  endpoint: https://hub.example.com\n  transport:\n    mode: iap\n    audience: aud-1\n"
	writeHomeGlobalSettings(t, settings)

	require.NoError(t, resolveInvocationTarget(findCommand(t, "list"), ModeHuman))
	assert.Empty(t, hubEndpoint, "the imported hub is not the current hub, so nothing is filled")
	assert.Empty(t, hubSource)

	c := readTestContext(t)
	assert.Empty(t, c.CurrentHub)
	require.NotNil(t, c.Hubs["hub"])
	assert.Equal(t, ctxTestHub, c.Hubs["hub"].Endpoint)
	assert.Equal(t, &config.HubTransportConfig{Mode: "iap", Audience: "aud-1"}, c.Hubs["hub"].Transport)
	assert.True(t, c.GlobalImported())
	globalDir := filepath.Join(os.Getenv("HOME"), ".scion")
	got, err := os.ReadFile(filepath.Join(globalDir, "settings.yaml"))
	require.NoError(t, err)
	assert.Equal(t, settings, string(got), "no settings file is rewritten")

	// A later change to the global hub.endpoint is followed outside a
	// project; the context file is not rewritten.
	before, err := os.ReadFile(contextFilePath())
	require.NoError(t, err)
	writeHomeGlobalSettings(t, "hub:\n  endpoint: https://other.example.com\n")
	resetContextTargets()
	require.NoError(t, resolveInvocationTarget(findCommand(t, "list"), ModeHuman))
	assert.Empty(t, hubEndpoint)
	s, err := config.LoadSettings(globalDir)
	require.NoError(t, err)
	assert.Equal(t, "https://other.example.com", GetHubEndpoint(s))
	after, err := os.ReadFile(contextFilePath())
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after), "written once")
}

// Bare 'scion use' after the import shows the global settings as the
// source, with the entry's name while the endpoints match and '-' after
// the global endpoint changed; 'scion use <project>' then selects the hub.
func TestUse_AfterGlobalImport(t *testing.T) {
	srv, _ := setupUseTest(t)
	enableGlobalHubImport(t)

	stdout, _, err := runScion(t, "use", "--format", "json")
	require.NoError(t, err)
	var got useShowJSON
	require.NoError(t, json.Unmarshal([]byte(stdout), &got))
	assert.Equal(t, useHubJSON{Name: "local", Endpoint: srv.URL, Source: useSourceGlobalSettings}, got.Effective.Hub)
	assert.Nil(t, got.Context, "no hub is selected")

	stdout, _, err = runScion(t, "use")
	require.NoError(t, err)
	assert.Regexp(t, `^Hub:\s+local\s+`+regexp.QuoteMeta(srv.URL)+`\s+\(global settings\)\n`, stdout)

	writeHomeGlobalSettings(t, "hub:\n  endpoint: https://other.example.com\n")
	stdout, _, err = runScion(t, "use", "--format", "json")
	require.NoError(t, err)
	got = useShowJSON{}
	require.NoError(t, json.Unmarshal([]byte(stdout), &got))
	assert.Equal(t, useHubJSON{Endpoint: "https://other.example.com", Source: useSourceGlobalSettings}, got.Effective.Hub)
	stdout, _, err = runScion(t, "use")
	require.NoError(t, err)
	assert.Regexp(t, `^Hub:\s+-\s+https://other.example.com\s+\(global settings\)\n`, stdout)

	writeHomeGlobalSettings(t, "hub:\n  endpoint: "+srv.URL+"\n")
	_, _, err = runScion(t, "use", "foo")
	require.NoError(t, err)
	assert.Equal(t, "local", readTestContext(t).CurrentHub, "an explicit selection sets the current hub")
}

// An existing user's global hub.endpoint shows up in 'hub list' with no
// other command run, named "local" for the local workstation hub, and not
// marked current.
func TestHubList_ShowsImportedGlobalEndpoint(t *testing.T) {
	srv, _ := setupUseTest(t)
	enableGlobalHubImport(t)

	stdout, _, err := runScion(t, "hub", "list")
	require.NoError(t, err)
	w := maxLen("ENDPOINT", srv.URL)
	assert.Equal(t, "CURRENT  NAME   "+padRight("ENDPOINT", w)+"  AUTH\n"+
		"         local  "+padRight(srv.URL, w)+"  dev\n", stdout)
}

// The registry end to end: hub add, use --hub, hub list marks it current,
// and a rename keeps the login working.
func TestHubRegistry_AddUseListRename(t *testing.T) {
	setupRegistryTest(t)
	srv := healthOnlyHub(t)
	storeTestLogin(t, srv.URL)

	_, _, err := runScion(t, "hub", "add", "prod", srv.URL)
	require.NoError(t, err)
	stdout, _, err := runScion(t, "use", "--hub", "prod")
	require.NoError(t, err)
	assert.Equal(t, "Now using hub 'prod' ("+srv.URL+").\n"+
		"No project selected on hub 'prod'; run 'scion use <project>'.\n", stdout)

	stdout, _, err = runScion(t, "hub", "list")
	require.NoError(t, err)
	assert.Contains(t, stdout, "*        prod  "+srv.URL+"  oauth (alice@example.com)\n")

	_, _, err = runScion(t, "hub", "rename", "prod", "production")
	require.NoError(t, err)
	stdout, _, err = runScion(t, "hub", "list")
	require.NoError(t, err)
	assert.Contains(t, stdout, "*        production  "+srv.URL+"  oauth (alice@example.com)\n")
	stdout, _, err = runScion(t, "use")
	require.NoError(t, err)
	assert.Contains(t, stdout, "Auth:     logged in as alice@example.com (OAuth)")
}

// use --hub restores the project remembered for that hub.
func TestUseSwitchHub_RestoresProject(t *testing.T) {
	setupRegistryTest(t)
	saveTestContext(t, ctxTestHub, ctxTestProjectID)
	require.NoError(t, config.UpdateCLIContext(func(c *config.CLIContext) error {
		return c.AddHub("staging", "https://staging.example.com")
	}))
	storeTestLogin(t, ctxTestHub)
	storeTestLogin(t, "https://staging.example.com")

	stdout, _, err := runScion(t, "use", "--hub", "staging")
	require.NoError(t, err)
	assert.Equal(t, "Now using hub 'staging' (https://staging.example.com).\n"+
		"No project selected on hub 'staging'; run 'scion use <project>'.\n", stdout)
	assert.Equal(t, "staging", readTestContext(t).CurrentHub)

	stdout, _, err = runScion(t, "project", "use", "--hub", "prod")
	require.NoError(t, err)
	assert.Equal(t, "Now using project 'foo' on hub 'prod' ("+ctxTestHub+").\n", stdout)
	assert.Equal(t, "prod", readTestContext(t).CurrentHub)
}

// use --hub with a URL that is not registered adds it with a derived
// nickname; a registered URL switches to its entry.
func TestUseSwitchHub_URL(t *testing.T) {
	setupRegistryTest(t)

	stdout, _, err := runScion(t, "use", "--hub", "https://Hub.Example.com/")
	require.NoError(t, err)
	assert.Equal(t, "Registered hub 'hub' (https://hub.example.com); rename with 'scion hub rename'.\n"+
		"Now using hub 'hub' (https://hub.example.com).\n"+
		"No project selected on hub 'hub'; run 'scion use <project>'.\n"+
		"Not logged in to hub 'hub'. Run: scion hub auth login --hub hub\n", stdout)
	c := readTestContext(t)
	assert.Equal(t, "hub", c.CurrentHub)
	assert.Equal(t, ctxTestHub, c.Hubs["hub"].Endpoint)

	require.NoError(t, config.UpdateCLIContext(func(c *config.CLIContext) error {
		c.CurrentHub = ""
		return nil
	}))
	stdout, _, err = runScion(t, "use", "--hub", ctxTestHub, "--format", "json")
	require.NoError(t, err)
	var got map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(stdout), &got))
	assert.Equal(t, map[string]interface{}{
		"hub":        map[string]interface{}{"name": "hub", "endpoint": ctxTestHub, "source": "context"},
		"project":    nil,
		"registered": false,
	}, got)
	assert.Len(t, readTestContext(t).Hubs, 1)

	_, _, err = runScion(t, "use", "--hub", "localhost:8080")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid hub URL")
}

// use <project> --hub <URL> registers the URL and says so.
func TestUseSelectProject_HubURLRegisters(t *testing.T) {
	srv, _ := setupUseTest(t)
	writeHomeGlobalSettings(t, "")

	stdout, _, err := runScion(t, "use", "foo", "--hub", srv.URL)
	require.NoError(t, err)
	assert.Equal(t, "Registered hub 'local' ("+srv.URL+"); rename with 'scion hub rename'.\n"+
		"Now using project 'foo' on hub 'local' ("+srv.URL+").\n", stdout)
}

// The SCION_HUB_TOKEN warning appears only when the hub came from the
// context.
func TestUse_HubTokenWarning(t *testing.T) {
	srv, _ := setupUseTest(t)
	writeHomeGlobalSettings(t, "")
	saveTestContext(t, srv.URL, "")
	t.Setenv("SCION_HUB_TOKEN", "scoped-token")
	const warning = "Warning: SCION_HUB_TOKEN is set and will be sent to hub 'prod'\n"

	_, stderr, err := runScion(t, "use")
	require.NoError(t, err)
	assert.Contains(t, stderr, warning, "bare use, hub from the context")

	_, stderr, err = runScion(t, "use", "foo")
	require.NoError(t, err)
	assert.Contains(t, stderr, warning, "use <project>, hub from the context")

	_, stderr, err = runScion(t, "use", "foo", "--hub", "prod")
	require.NoError(t, err)
	assert.NotContains(t, stderr, "SCION_HUB_TOKEN", "hub from --hub")

	t.Setenv("SCION_HUB_ENDPOINT", srv.URL)
	_, stderr, err = runScion(t, "use")
	require.NoError(t, err)
	assert.NotContains(t, stderr, "SCION_HUB_TOKEN", "hub from the environment")
}

// The login hints name the hub's nickname when it has one.
func TestUse_LoginHintNamesNickname(t *testing.T) {
	setupRegistryTest(t)
	saveTestContext(t, ctxTestHub, "")
	stdout, _, err := runScion(t, "use")
	require.NoError(t, err)
	assert.Contains(t, stdout, "Auth:     not logged in; run 'scion hub auth login --hub prod'")

	_, _, err = runScion(t, "use", "foo")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Log in with: scion hub auth login --hub prod")
}

// --hub-url accepts a nickname.
func TestHubAuthURLFlag(t *testing.T) {
	setupContextTest(t)
	saveTestContext(t, ctxTestHub, "")

	got, err := hubAuthURLFlag("prod")
	require.NoError(t, err)
	assert.Equal(t, ctxTestHub, got)

	got, err = hubAuthURLFlag("https://other.example.com/")
	require.NoError(t, err)
	assert.Equal(t, "https://other.example.com/", got)

	got, err = hubAuthURLFlag("")
	require.NoError(t, err)
	assert.Empty(t, got)

	_, err = hubAuthURLFlag("staging")
	require.Error(t, err)
	assert.Equal(t, "unknown hub 'staging'; registered hubs: prod", err.Error())
}

// Only a URL given with --hub-url or --hub is added to the hub list after
// a login; it is announced and does not become current.
func TestHubAuthLogin_RegistersExplicitURL(t *testing.T) {
	origHubURL := hubAuthHubURL
	t.Cleanup(func() { hubAuthHubURL = origHubURL })
	const newHub = "https://new.example.com"

	tests := []struct {
		name     string
		setup    func(t *testing.T)
		explicit bool
	}{
		{"--hub-url URL", func(t *testing.T) { hubAuthHubURL = newHub + "/" }, true},
		{"--hub URL", func(t *testing.T) { hubEndpoint = newHub }, true},
		{"--hub-url nickname", func(t *testing.T) { hubAuthHubURL = "prod" }, false},
		{"--hub nickname", func(t *testing.T) {
			hubEndpoint = "prod"
			require.NoError(t, resolveInvocationTarget(findCommand(t, "hub", "auth", "login"), ModeHuman))
		}, false},
		{"environment", func(t *testing.T) { t.Setenv("SCION_HUB_ENDPOINT", newHub) }, false},
		{"settings", func(t *testing.T) { writeHomeGlobalSettings(t, "hub:\n  endpoint: "+newHub+"\n") }, false},
		{"current context", func(t *testing.T) {
			require.NoError(t, resolveInvocationTarget(findCommand(t, "hub", "auth", "login"), ModeHuman))
			require.Equal(t, targetSourceContext, hubSource)
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setupContextTest(t)
			hubAuthHubURL = ""
			saveTestContext(t, ctxTestHub, "")
			tt.setup(t)
			assert.Equal(t, tt.explicit, loginHubURLGivenExplicitly())
		})
	}

	setupContextTest(t)
	saveTestContext(t, ctxTestHub, "")
	var out, errOut bytes.Buffer
	registerLoginHub(&out, &errOut, newHub+"/")
	assert.Equal(t, "Registered hub 'new' ("+newHub+"); rename with 'scion hub rename'.\n", out.String())
	assert.Empty(t, errOut.String())
	c := readTestContext(t)
	assert.Equal(t, newHub, c.Hubs["new"].Endpoint)
	assert.Equal(t, "prod", c.CurrentHub, "a login never makes the hub current")

	// An endpoint that is already registered is not announced or rewritten.
	before, err := os.ReadFile(contextFilePath())
	require.NoError(t, err)
	out.Reset()
	registerLoginHub(&out, &errOut, "https://NEW.example.com")
	assert.Empty(t, out.String())
	after, err := os.ReadFile(contextFilePath())
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after))
}

// A hub list entry's transport wins over hub.transport in settings for
// that hub only; agents ignore the hub list.
func TestHubTransportSettings(t *testing.T) {
	setupContextTest(t)
	require.NoError(t, config.UpdateCLIContext(func(c *config.CLIContext) error {
		require.NoError(t, c.AddHub("prod", ctxTestHub))
		c.Hubs["prod"].Transport = &config.HubTransportConfig{Mode: "iap", Audience: "entry-aud"}
		return c.AddHub("plain", "https://plain.example.com")
	}))
	settings := &config.Settings{Hub: &config.HubClientConfig{Transport: &config.HubTransportConfig{Mode: "cloudrun_invoker", Audience: "settings-aud"}}}

	ts := hubTransportSettings(settings, ctxTestHub+"/")
	require.NotNil(t, ts)
	assert.Equal(t, "iap", ts.Mode)
	assert.Equal(t, "entry-aud", ts.Audience)

	ts = hubTransportSettings(settings, "https://plain.example.com")
	require.NotNil(t, ts)
	assert.Equal(t, "settings-aud", ts.Audience, "an entry without a transport falls back to settings")

	assert.Nil(t, hubTransportSettings(&config.Settings{}, "https://unknown.example.com"))

	t.Setenv("SCION_HOST_UID", "1000")
	ts = hubTransportSettings(settings, ctxTestHub)
	require.NotNil(t, ts)
	assert.Equal(t, "settings-aud", ts.Audience, "agents never read the hub list")
}

// A successful login adds a URL given with --hub-url to the hub list
// (announced, not made current); a hub from the environment is not added.
func TestFinishHubAuthLogin_RegistersOnlyExplicitURL(t *testing.T) {
	const newHub = "https://new.example.com"
	token := &hubclient.CLITokenResponse{AccessToken: "tok", ExpiresIn: 3600}

	t.Run("--hub-url", func(t *testing.T) {
		setupLoginContextTest(t)
		hubAuthHubURL = newHub + "/"
		require.NoError(t, resolveInvocationTarget(findCommand(t, "hub", "auth", "login"), ModeHuman))
		out := captureStdout(t, func() {
			require.NoError(t, finishHubAuthLogin(hubAuthLoginCmd, hubAuthHubURL, token))
		})
		assert.Contains(t, out, "Registered hub 'new' ("+newHub+"); rename with 'scion hub rename'.\n")
		c := readTestContext(t)
		require.NotNil(t, c.Hubs["new"])
		assert.Equal(t, newHub, c.Hubs["new"].Endpoint)
		assert.Equal(t, "prod", c.CurrentHub, "a login never makes the hub current")
		assert.True(t, credentials.IsAuthenticated(newHub))
	})

	t.Run("environment", func(t *testing.T) {
		setupLoginContextTest(t)
		t.Setenv("SCION_HUB_ENDPOINT", newHub)
		require.NoError(t, resolveInvocationTarget(findCommand(t, "hub", "auth", "login"), ModeHuman))
		hubURL := resolveHubAuthURL(hubAuthHubURL, hubEndpoint, os.Getenv, settingsHubEndpoint)
		require.Equal(t, newHub, hubURL)
		before, err := os.ReadFile(contextFilePath())
		require.NoError(t, err)
		out := captureStdout(t, func() {
			require.NoError(t, finishHubAuthLogin(hubAuthLoginCmd, hubURL, token))
		})
		assert.NotContains(t, out, "Registered hub")
		after, err := os.ReadFile(contextFilePath())
		require.NoError(t, err)
		assert.Equal(t, string(before), string(after), "the hub list is unchanged")
		assert.True(t, credentials.IsAuthenticated(newHub))
	})
}

// sameHubURL compares hub URLs after NormalizeEndpoint.
func TestSameHubURL_Normalised(t *testing.T) {
	assert.True(t, sameHubURL("https://hub.example.com:443", "https://hub.example.com"))
	assert.True(t, sameHubURL("HTTPS://Hub.Example.com/", "https://hub.example.com"))
	assert.False(t, sameHubURL("https://hub.example.com:8443", "https://hub.example.com"))
	assert.False(t, sameHubURL("https://hub.example.com/Base", "https://hub.example.com/base"), "the path keeps its case")

	// No false override note when the environment spells the current hub
	// with its default port.
	setupRegistryTest(t)
	saveTestContext(t, ctxTestHub, "")
	t.Setenv("SCION_HUB_ENDPOINT", "https://hub.example.com:443")
	stdout, stderr, err := runScion(t, "use", "--hub", "prod")
	require.NoError(t, err)
	assert.NotContains(t, stdout+stderr, "overrides the current hub")
}

// The AUTH column uses only four labels: oauth or oauth (<user>), dev,
// env token and none.
func TestHubAuthLabel(t *testing.T) {
	tests := []struct {
		info authInfo
		want string
	}{
		{authInfo{MethodType: "oauth", OAuthCreds: &credentials.HubCredentials{User: &credentials.User{Email: "alice@example.com"}}}, "oauth (alice@example.com)"},
		{authInfo{MethodType: "oauth"}, "oauth"},
		{authInfo{MethodType: "devauth", IsDevAuth: true}, "dev"},
		{authInfo{MethodType: "agent_token", IsDevAuth: true}, "dev"},
		{authInfo{MethodType: "bearer"}, "env token"},
		{authInfo{MethodType: "agent_token", Source: "SCION_AUTH_TOKEN env"}, "env token"},
		{authInfo{MethodType: "agent_token", Source: "scion-token file"}, "env token"},
		{authInfo{MethodType: "proxy-auth"}, "none"},
		{authInfo{MethodType: "none"}, "none"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, hubAuthLabel(tt.info), "%+v", tt.info)
	}
}

// A hub URL with user info never reaches context.yaml through
// 'scion use <project> --hub' or the login registration.
func TestHubURLWithUserInfoNotRegistered(t *testing.T) {
	t.Run("use <project> --hub", func(t *testing.T) {
		srv, calls := setupUseTest(t)
		writeHomeGlobalSettings(t, "")
		withCreds := strings.Replace(srv.URL, "http://", "http://user:pw@", 1)
		_, _, err := runScion(t, "use", "foo", "--hub", withCreds)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid hub URL")
		assert.Zero(t, calls.Load(), "refused before any hub call")
		_, statErr := os.Stat(contextFilePath())
		assert.True(t, os.IsNotExist(statErr), "nothing is written")
	})
	t.Run("global import", func(t *testing.T) {
		setupContextTest(t)
		enableGlobalHubImport(t)
		const withCreds = "https://user:pw@hub.example.com"
		writeHomeGlobalSettings(t, "hub:\n  endpoint: "+withCreds+"\n")
		for i := 0; i < 2; i++ {
			resetContextTargets()
			stderr := captureStderr(t, func() {
				require.NoError(t, resolveInvocationTarget(findCommand(t, "list"), ModeHuman))
			})
			assert.Empty(t, stderr, "the skipped import prints nothing (run %d)", i+1)
			assert.Empty(t, hubEndpoint)
		}
		_, statErr := os.Stat(contextFilePath())
		assert.True(t, os.IsNotExist(statErr), "nothing is written")
		// Outside a project the CLI still follows the global endpoint.
		s, err := config.LoadSettings(filepath.Join(os.Getenv("HOME"), ".scion"))
		require.NoError(t, err)
		assert.Equal(t, withCreds, GetHubEndpoint(s))
	})
	t.Run("login registration", func(t *testing.T) {
		setupContextTest(t)
		var out, errOut bytes.Buffer
		registerLoginHub(&out, &errOut, "https://user:pw@new.example.com")
		assert.Empty(t, out.String())
		assert.Contains(t, errOut.String(), "Warning: could not add https://user:pw@new.example.com to the hub list: invalid hub URL")
		_, statErr := os.Stat(contextFilePath())
		assert.True(t, os.IsNotExist(statErr), "nothing is written")
	})
}

// Hand-edited context.yaml: a current hub or a known project naming a hub
// that is not in the list, and a hub entry without an endpoint, never make a
// command panic or silently drop --hub; a hub key with no entry makes the
// file unreadable, which the commands report.
func TestHubCommands_HandEditedContext(t *testing.T) {
	t.Run("dangling references and an entry without endpoint", func(t *testing.T) {
		setupRegistryTest(t)
		require.NoError(t, os.MkdirAll(filepath.Dir(contextFilePath()), 0o700))
		require.NoError(t, os.WriteFile(contextFilePath(), []byte("version: 1\ncurrent_hub: ghost\nhubs:\n  prod: {}\n  ok:\n    endpoint: "+ctxTestHub+"\nprojects:\n  - {hub: missing, id: p1, slug: foo}\n"), 0o600))

		stdout, _, err := runScion(t, "hub", "list")
		require.NoError(t, err)
		assert.Contains(t, stdout, " ok ")
		assert.Contains(t, stdout, " prod ")
		assert.NotContains(t, stdout, "*", "no hub is current: current_hub names a hub that is not in the list")

		stdout, _, err = runScion(t, "use")
		require.NoError(t, err)
		assert.Equal(t, noHubSelectedMessage+"\n", stdout)

		const noEndpoint = "hub 'prod' has no endpoint; remove it with 'scion hub remove prod' and add it again with 'scion hub add prod <url>'"
		_, _, err = runScion(t, "--hub", "prod", "use")
		require.Error(t, err)
		assert.Equal(t, noEndpoint, err.Error())
		_, err = hubAuthURLFlag("prod")
		require.Error(t, err)
		assert.Equal(t, noEndpoint, err.Error())

		_, _, err = runScion(t, "hub", "remove", "ghost")
		require.Error(t, err)
		assert.Equal(t, "unknown hub 'ghost'; registered hubs: ok, prod", err.Error())

		_, _, err = runScion(t, "hub", "remove", "prod")
		require.NoError(t, err)
		_, _, err = runScion(t, "use", "--hub", "ok")
		require.NoError(t, err)
		assert.Equal(t, "ok", readTestContext(t).CurrentHub)
	})
	t.Run("hub key with no entry", func(t *testing.T) {
		setupRegistryTest(t)
		require.NoError(t, os.MkdirAll(filepath.Dir(contextFilePath()), 0o700))
		require.NoError(t, os.WriteFile(contextFilePath(), []byte("version: 1\nhubs:\n  prod:\n"), 0o600))

		stdout, stderr, err := runScion(t, "hub", "list")
		require.NoError(t, err)
		assert.Contains(t, stderr, "Warning: ignoring the current context")
		assert.Contains(t, stderr, `hub "prod" has no entry`)
		assert.Equal(t, "No hubs registered. Add one with 'scion hub add <name> <url>'.\n", stdout)

		_, _, err = runScion(t, "--hub", "prod", "use")
		require.ErrorIs(t, err, config.ErrCLIContextCorrupt)
		assert.Contains(t, err.Error(), "cannot look up hub 'prod'")
	})
}

// authCheckHub is a fake hub that answers health checks and accepts the
// identity call only with the bearer token accept, rejecting any other with
// 401. It records the Authorization header of each identity call.
func authCheckHub(t *testing.T, accept string) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		case "/api/v1/auth/me":
			mu.Lock()
			seen = append(seen, r.Header.Get("Authorization"))
			mu.Unlock()
			if r.Header.Get("Authorization") != "Bearer "+accept {
				w.WriteHeader(http.StatusUnauthorized)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"error": map[string]interface{}{"code": "unauthorized", "message": "invalid token"},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "u1", "email": "alice@example.com"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

// TestHubAuthFor_DevTokenOnlyOnLoopback: the dev token counts as a hub's
// credential only for a loopback hub; any other hub has none.
func TestHubAuthFor_DevTokenOnlyOnLoopback(t *testing.T) {
	setupContextTest(t)
	t.Setenv("SCION_DEV_TOKEN", "scion_dev_test")

	for _, endpoint := range []string{"http://127.0.0.1:8080", "http://localhost:8080", "http://[::1]:8080"} {
		assert.Equal(t, "dev", hubAuthLabel(hubAuthFor(endpoint)), endpoint)
	}
	for _, endpoint := range []string{"https://hub.example.com", "http://192.0.2.1:8080"} {
		info := hubAuthFor(endpoint)
		assert.Equal(t, "none", hubAuthLabel(info), endpoint)
		assert.Equal(t, "none", info.MethodType, endpoint)
	}

	// An OAuth login for a remote hub is still its credential.
	storeTestLogin(t, "https://hub.example.com")
	assert.Equal(t, "oauth (alice@example.com)", hubAuthLabel(hubAuthFor("https://hub.example.com")))
}

// TestHubAuthToken: 'hub list --check' sends a hub only its own login, or
// the dev token when the hub is on a loopback address; never another token.
func TestHubAuthToken(t *testing.T) {
	setupContextTest(t)
	t.Setenv("SCION_DEV_TOKEN", "scion_dev_test")
	oauth := authInfo{MethodType: "oauth", OAuthCreds: &credentials.HubCredentials{AccessToken: "login-tok"}}
	dev := authInfo{MethodType: "devauth", IsDevAuth: true}

	assert.Equal(t, "login-tok", hubAuthToken("https://hub.example.com", oauth), "a hub's own login is sent")
	assert.Equal(t, "scion_dev_test", hubAuthToken("http://127.0.0.1:8080", dev), "the dev token is sent to a loopback hub")
	assert.Empty(t, hubAuthToken("https://hub.example.com", dev), "no dev token for a remote hub")
	assert.Empty(t, hubAuthToken("http://127.0.0.1:8080", authInfo{MethodType: "bearer"}), "SCION_HUB_TOKEN is never sent")
	assert.Empty(t, hubAuthToken("http://127.0.0.1:8080", authInfo{MethodType: "agent_token"}), "an agent token is never sent")
	assert.Empty(t, hubAuthToken("http://127.0.0.1:8080", authInfo{MethodType: "none"}))
}

// TestHubList_CheckVerifiesAuth: --check asks each reachable hub whether
// it accepts the CLI's credential for it, sending only that credential.
func TestHubList_CheckVerifiesAuth(t *testing.T) {
	setupRegistryTest(t)
	t.Setenv("SCION_DEV_TOKEN", "scion_dev_test")
	devOK, devOKSeen := authCheckHub(t, "scion_dev_test")
	devBad, devBadSeen := authCheckHub(t, "scion_dev_other")
	login, loginSeen := authCheckHub(t, "tok")
	down := closedHubURL(t)
	require.NoError(t, config.UpdateCLIContext(func(c *config.CLIContext) error {
		require.NoError(t, c.AddHub("a-dev", devOK.URL))
		require.NoError(t, c.AddHub("b-dev-rejected", devBad.URL))
		require.NoError(t, c.AddHub("c-login", login.URL))
		require.NoError(t, c.AddHub("d-down", down))
		return nil
	}))
	storeTestLogin(t, login.URL)

	stdout, _, err := runScion(t, "hub", "list", "--check")
	require.NoError(t, err)
	assert.Regexp(t, `a-dev .* dev\s+yes\s+yes\n`, stdout, "a loopback hub that accepts the dev token shows yes")
	assert.Regexp(t, `b-dev-rejected .* dev\s+yes\s+no\n`, stdout, "a loopback hub that rejects the dev token shows no")
	assert.Regexp(t, `c-login .* oauth \(alice@example.com\)\s+yes\s+yes\n`, stdout, "a hub with a login is verified with it")
	assert.Regexp(t, `d-down .* dev\s+no\s+-\n`, stdout, "an unreachable hub is not verified")
	assert.Equal(t, []string{"Bearer scion_dev_test"}, devOKSeen())
	assert.Equal(t, []string{"Bearer scion_dev_test"}, devBadSeen())
	assert.Equal(t, []string{"Bearer tok"}, loginSeen(), "a hub with a login gets only its login")

	stdout, _, err = runScion(t, "hub", "list", "--check", "--format", "json")
	require.NoError(t, err)
	var rows []map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(stdout), &rows))
	require.Len(t, rows, 4)
	assert.Equal(t, true, rows[0]["verified"])
	assert.Equal(t, false, rows[1]["verified"])
	assert.Equal(t, true, rows[2]["verified"])
	_, has := rows[3]["verified"]
	assert.False(t, has, "verified is omitted when not checked")

	// Without --check no identity call is made.
	_, _, err = runScion(t, "hub", "list")
	require.NoError(t, err)
	assert.Len(t, devOKSeen(), 2, "only the two --check runs called the hub")
}

// TestHubList_CheckNeverSendsEnvToken: a token from the environment is not
// tied to a hub, so --check does not send it and the hub is not verified.
func TestHubList_CheckNeverSendsEnvToken(t *testing.T) {
	setupRegistryTest(t)
	t.Setenv("SCION_HUB_TOKEN", "env-token")
	srv, seen := authCheckHub(t, "env-token")
	require.NoError(t, config.UpdateCLIContext(func(c *config.CLIContext) error {
		return c.AddHub("prod", srv.URL)
	}))

	stdout, _, err := runScion(t, "hub", "list", "--check")
	require.NoError(t, err)
	assert.Regexp(t, `prod .* env token\s+yes\s+-\n`, stdout)
	assert.Empty(t, seen(), "no identity call with an env token")
}

// TestHubList_CheckRemoteHubGetsNoDevToken: a remote hub is never sent the
// dev token, so --check shows it with no credential and does not verify it.
func TestHubList_CheckRemoteHubGetsNoDevToken(t *testing.T) {
	setupRegistryTest(t)
	t.Setenv("SCION_DEV_TOKEN", "scion_dev_test")
	origResponds, origAccepts := hubResponds, hubAccepts
	t.Cleanup(func() { hubResponds, hubAccepts = origResponds, origAccepts })
	hubResponds = func(string) error { return nil }
	var called []string
	var mu sync.Mutex
	hubAccepts = func(endpoint, token string) (bool, bool) {
		mu.Lock()
		defer mu.Unlock()
		called = append(called, endpoint)
		return true, true
	}
	require.NoError(t, config.UpdateCLIContext(func(c *config.CLIContext) error {
		return c.AddHub("remote", "https://hub.example.com")
	}))

	stdout, _, err := runScion(t, "hub", "list", "--check")
	require.NoError(t, err)
	assert.Regexp(t, `remote .* none\s+yes\s+-\n`, stdout)
	assert.Empty(t, called, "no credential is sent to a remote hub")
}

// transportCheckHub is a fake hub that answers health checks and accepts
// the identity call only with the bearer token accept. It records the path
// and the headers of every request.
func transportCheckHub(t *testing.T, accept string) (*httptest.Server, func() map[string][]http.Header) {
	t.Helper()
	var mu sync.Mutex
	seen := map[string][]http.Header{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.URL.Path] = append(seen[r.URL.Path], r.Header.Clone())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		case r.URL.Path == "/api/v1/auth/me" && r.Header.Get("Authorization") == "Bearer "+accept:
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "u1", "email": "alice@example.com"})
		default:
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"error": map[string]interface{}{"code": "unauthorized", "message": "invalid token"},
			})
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() map[string][]http.Header {
		mu.Lock()
		defer mu.Unlock()
		out := map[string][]http.Header{}
		for k, v := range seen {
			out[k] = append([]http.Header(nil), v...)
		}
		return out
	}
}

// headersCarry reports whether any of hs has a header value containing s.
func headersCarry(hs []http.Header, s string) bool {
	for _, h := range hs {
		for _, values := range h {
			for _, v := range values {
				if strings.Contains(v, s) {
					return true
				}
			}
		}
	}
	return false
}

// TestHubList_CheckSendsOnlyEntryTransport: 'hub add' and 'hub list
// --check' reach hubs other than the effective one, so each hub gets only
// the transport auth of its own entry, never the one from the environment.
func TestHubList_CheckSendsOnlyEntryTransport(t *testing.T) {
	setupRegistryTest(t)
	t.Setenv("SCION_DEV_TOKEN", "scion_dev_test")
	t.Setenv("SCION_TRANSPORT_TOKEN", "env-transport-tok")
	t.Setenv("SCION_TRANSPORT_TOKEN_FILE", filepath.Join(t.TempDir(), "transport-token"))
	t.Setenv("SCION_TRANSPORT_MODE", "iap")
	origSource := hubEntryTransportSource
	t.Cleanup(func() { hubEntryTransportSource = origSource })
	var audiences []string
	var mu sync.Mutex
	hubEntryTransportSource = func(audience string) (transportauth.TokenSource, error) {
		mu.Lock()
		audiences = append(audiences, audience)
		mu.Unlock()
		src := transportauth.NewInjectedSource()
		src.SetToken("entry-transport-tok", time.Now().Add(time.Hour))
		return src, nil
	}
	plain, plainSeen := transportCheckHub(t, "scion_dev_test")
	guarded, guardedSeen := transportCheckHub(t, "scion_dev_test")
	require.NoError(t, config.UpdateCLIContext(func(c *config.CLIContext) error {
		if err := c.AddHub("guarded", guarded.URL); err != nil {
			return err
		}
		c.Hubs["guarded"].Transport = &config.HubTransportConfig{Mode: "iap", Audience: "entry-aud"}
		return nil
	}))

	_, _, err := runScion(t, "hub", "add", "plain", plain.URL)
	require.NoError(t, err)
	stdout, _, err := runScion(t, "hub", "list", "--check")
	require.NoError(t, err)
	assert.Regexp(t, `guarded .* dev\s+yes\s+yes\n`, stdout)
	assert.Regexp(t, `plain .* dev\s+yes\s+yes\n`, stdout)

	p := plainSeen()
	require.Len(t, p["/healthz"], 2, "hub add and hub list --check each check health")
	require.Len(t, p["/api/v1/auth/me"], 1)
	assert.False(t, headersCarry(p["/healthz"], "env-transport-tok"), "the health check never sends the environment's transport token")
	assert.False(t, headersCarry(p["/api/v1/auth/me"], "env-transport-tok"), "the identity call never sends the environment's transport token")
	assert.False(t, headersCarry(p["/healthz"], "entry-transport-tok"), "a hub without an entry transport gets none")

	g := guardedSeen()
	require.Len(t, g["/healthz"], 1)
	require.Len(t, g["/api/v1/auth/me"], 1)
	for _, h := range append(g["/healthz"], g["/api/v1/auth/me"]...) {
		assert.Equal(t, "Bearer entry-transport-tok", h.Get("Proxy-Authorization"), "the entry's transport, in the entry's mode")
	}
	assert.False(t, headersCarry(append(g["/healthz"], g["/api/v1/auth/me"]...), "env-transport-tok"))
	mu.Lock()
	defer mu.Unlock()
	assert.NotEmpty(t, audiences)
	for _, a := range audiences {
		assert.Equal(t, "entry-aud", a)
	}
}

// TestUseShow_DevTokenScope: bare 'scion use' applies the dev token rule of
// 'hub list': a remote hub with only a dev token has no credential.
func TestUseShow_DevTokenScope(t *testing.T) {
	setupRegistryTest(t)
	t.Setenv("SCION_DEV_TOKEN", "scion_dev_test")
	saveTestContext(t, ctxTestHub, ctxTestProjectID)

	stdout, _, err := runScion(t, "use")
	require.NoError(t, err)
	assert.Contains(t, stdout, "Auth:     not logged in; run 'scion hub auth login --hub prod'\n", "remote hub, dev token only")
	assert.NotContains(t, stdout, "Dev auth")

	stdout, _, err = runScion(t, "use", "--format", "json")
	require.NoError(t, err)
	var got struct {
		Effective struct {
			Auth map[string]interface{} `json:"auth"`
		} `json:"effective"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &got))
	assert.Equal(t, map[string]interface{}{"method": "none"}, got.Effective.Auth)
}

// TestHubStatus_DevTokenScope: hub status applies the same dev token rule.
// A hub that is not on a loopback name gets no dev token credential, so
// its auth method is none and nothing is verified with it. The fake hub
// listens on 127.0.0.2, which the CLI does not treat as loopback, and
// would accept the dev token if it were sent for verification.
func TestHubStatus_DevTokenScope(t *testing.T) {
	setupContextTest(t)
	t.Setenv("SCION_DEV_TOKEN", "scion_dev_test")
	ln, err := net.Listen("tcp", "127.0.0.2:0")
	if err != nil {
		t.Skipf("127.0.0.2 is not available here: %v", err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		case "/api/v1/auth/me":
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "u1", "email": "dev@example.com"})
		default:
			http.NotFound(w, r)
		}
	}))
	_ = srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	saveTestContext(t, srv.URL, ctxTestProjectID)
	outputFormat = "json"
	t.Cleanup(func() { outputFormat = "" })

	out := runHubStatusForTest(t)
	var got map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(out), &got), out)
	assert.Equal(t, "none", got["authMethod"], "a non-loopback hub gets no dev token credential")
	assert.Equal(t, false, got["isDevAuth"])
	_, verified := got["authVerified"]
	assert.False(t, verified, "no credential, so nothing is verified")
}

// TestScopeDevAuth_KeepsTransportAuth: for a remote hub, a dev token is
// replaced by what the CLI has without it: transport (proxy) auth when it
// is active, otherwise no credential. A loopback hub keeps the dev token.
func TestScopeDevAuth_KeepsTransportAuth(t *testing.T) {
	setupContextTest(t)
	t.Setenv("SCION_DEV_TOKEN", "scion_dev_test")
	for _, name := range []string{"SCION_TRANSPORT_TOKEN", "SCION_TRANSPORT_TOKEN_FILE", "SCION_TRANSPORT_MODE", "SCION_TRANSPORT_AUDIENCE", "SCION_HUB_OIDC_AUDIENCE"} {
		t.Setenv(name, "")
	}
	dev := authInfo{Method: "Dev auth", MethodType: "devauth", Source: "SCION_DEV_TOKEN env var", IsDevAuth: true}

	assert.Equal(t, "none", scopeDevAuth(dev, "https://hub.example.com").MethodType, "no transport auth: no credential")
	assert.Equal(t, dev, scopeDevAuth(dev, "http://127.0.0.1:8080"), "a loopback hub keeps the dev token")

	t.Setenv("SCION_TRANSPORT_TOKEN", "env-transport-tok")
	t.Setenv("SCION_TRANSPORT_TOKEN_FILE", filepath.Join(t.TempDir(), "transport-token"))
	got := scopeDevAuth(dev, "https://hub.example.com")
	assert.Equal(t, "proxy-auth", got.MethodType, "a stale dev token does not hide transport auth")
	assert.False(t, got.IsDevAuth)
	assert.Equal(t, "devauth", getAuthInfo(&config.Settings{}, "https://hub.example.com").MethodType, "getAuthInfo checks the dev token before transport auth")
}

// TestUse_HubLoginHintDevTokenScope: 'scion use --hub' prints the login
// hint for a remote hub with only a dev token, as 'hub add' does.
func TestUse_HubLoginHintDevTokenScope(t *testing.T) {
	setupRegistryTest(t)
	t.Setenv("SCION_DEV_TOKEN", "scion_dev_test")
	require.NoError(t, config.UpdateCLIContext(func(c *config.CLIContext) error {
		return c.AddHub("staging", "https://staging.example.com")
	}))

	stdout, _, err := runScion(t, "use", "--hub", "staging")
	require.NoError(t, err)
	assert.Contains(t, stdout, "Not logged in to hub 'staging'. Run: scion hub auth login --hub staging\n", "remote hub, dev token only")
}
