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
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/credentials"
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
	assert.Regexp(t, `prod .* oauth \(alice@example.com\)\s+yes\n`, stdout)
	assert.Regexp(t, `staging .* none\s+no\n`, stdout)

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
	setupRegistryTest(t)
	t.Setenv("SCION_HOST_UID", "1000")
	_, _, err := runScion(t, "hub", "list")
	require.Error(t, err)
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
// the current hub, written once, without changing the settings file.
func TestResolveInvocationTarget_ImportsGlobalEndpoint(t *testing.T) {
	setupContextTest(t)
	enableGlobalHubImport(t)
	settings := "hub:\n  endpoint: https://hub.example.com\n  transport:\n    mode: iap\n    audience: aud-1\n"
	writeHomeGlobalSettings(t, settings)

	require.NoError(t, resolveInvocationTarget(findCommand(t, "list"), ModeHuman))
	assert.Equal(t, ctxTestHub, hubEndpoint)
	assert.Equal(t, targetSourceContext, hubSource)

	c := readTestContext(t)
	assert.Equal(t, "hub", c.CurrentHub)
	require.NotNil(t, c.Hubs["hub"])
	assert.Equal(t, &config.HubTransportConfig{Mode: "iap", Audience: "aud-1"}, c.Hubs["hub"].Transport)
	assert.True(t, c.GlobalImported())
	got, err := os.ReadFile(filepath.Join(os.Getenv("HOME"), ".scion", "settings.yaml"))
	require.NoError(t, err)
	assert.Equal(t, settings, string(got), "no settings file is rewritten")

	before, err := os.ReadFile(contextFilePath())
	require.NoError(t, err)
	resetContextTargets()
	require.NoError(t, resolveInvocationTarget(findCommand(t, "list"), ModeHuman))
	after, err := os.ReadFile(contextFilePath())
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after), "written once")
}

// Phase acceptance: an existing user's global hub.endpoint shows up in
// 'hub list' with no other command run; the local workstation hub is
// named "local".
func TestHubList_ShowsImportedGlobalEndpoint(t *testing.T) {
	srv, _ := setupUseTest(t)
	enableGlobalHubImport(t)

	stdout, _, err := runScion(t, "hub", "list")
	require.NoError(t, err)
	w := maxLen("ENDPOINT", srv.URL)
	assert.Equal(t, "CURRENT  NAME   "+padRight("ENDPOINT", w)+"  AUTH\n"+
		"*        local  "+padRight(srv.URL, w)+"  dev\n", stdout)
}

// Phase acceptance: hub add, use --hub, hub list marks it current, and a
// rename keeps the login working.
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
