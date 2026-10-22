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
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/credentials"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupConfigHubTest isolates a config hub.endpoint test: a fresh HOME
// outside any project (setupRegistryTest), the config commands' flags reset
// after each run, and a fixed name for new hub entries.
func setupConfigHubTest(t *testing.T) {
	t.Helper()
	setupRegistryTest(t)
	origBaseName, origResponds := useHubBaseName, hubResponds
	useHubBaseName = func(endpoint string) string { return config.DeriveHubName(endpoint) }
	t.Cleanup(func() { useHubBaseName, hubResponds = origBaseName, origResponds })
	resetConfigCommandFlags(t)
}

// resetConfigCommandFlags resets the flags of the config commands and the
// root --global flag before and after the test, since pflag keeps values
// across Execute calls.
func resetConfigCommandFlags(t *testing.T) {
	t.Helper()
	reset := func() {
		for _, c := range []*cobra.Command{configSetCmd, configGetCmd, configUnsetCmd, configListCmd} {
			c.Flags().VisitAll(func(f *pflag.Flag) {
				f.Changed = false
				_ = f.Value.Set(f.DefValue)
			})
		}
		if f := rootCmd.PersistentFlags().Lookup("global"); f != nil {
			f.Changed = false
			_ = f.Value.Set(f.DefValue)
		}
		configGlobal, configUnsetGlobal, configGetSource, globalMode = false, false, false, false
	}
	reset()
	t.Cleanup(reset)
}

// runConfig runs a scion command line and then resets the config flags for
// the next run.
func runConfig(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	stdout, stderr, err := runScion(t, args...)
	resetConfigCommandFlags(t)
	return stdout, stderr, err
}

// globalSettingsPath is the global settings file of the test HOME.
func globalSettingsPath() string {
	return filepath.Join(os.Getenv("HOME"), ".scion", "settings.yaml")
}

// assertNoHubEndpointInSettings fails when the global settings file names a
// hub endpoint.
func assertNoHubEndpointInSettings(t *testing.T) {
	t.Helper()
	data, err := os.ReadFile(globalSettingsPath())
	if os.IsNotExist(err) {
		return
	}
	require.NoError(t, err)
	assert.NotContains(t, string(data), "endpoint", "config set wrote the hub into the global settings")
}

// config set --global hub.endpoint adds the hub, makes it current, prints
// one line, and writes no settings file; config get, config list and hub
// status then report it as the current hub.
func TestConfigSetGlobalHubEndpoint_SelectsTheHub(t *testing.T) {
	setupConfigHubTest(t)
	srv := healthOnlyHub(t)

	stdout, stderr, err := runConfig(t, "config", "set", "--global", "hub.endpoint", srv.URL+"/")
	require.NoError(t, err)
	assert.Empty(t, stderr, "a hub that responds gives no warning")

	c := readTestContext(t)
	require.NotEmpty(t, c.CurrentHub)
	require.NotNil(t, c.Hubs[c.CurrentHub])
	assert.Equal(t, srv.URL, c.Hubs[c.CurrentHub].Endpoint, "stored normalised")
	assert.Len(t, c.Hubs, 1)
	assert.Equal(t, "Hub '"+c.CurrentHub+"' ("+srv.URL+") is now the current hub.\n", stdout)
	assertNoHubEndpointInSettings(t)

	stdout, _, err = runConfig(t, "--global", "config", "get", "--source", "hub.endpoint")
	require.NoError(t, err)
	assert.Equal(t, srv.URL+"  (current hub '"+c.CurrentHub+"')\n", stdout)
	scope := hubStatusScope(false)
	assert.Equal(t, useSourceContext, scope.Source, "hub status agrees")
	assert.Equal(t, srv.URL, scope.Endpoint)

	stdout, _, err = runConfig(t, "--global", "--format", "json", "config", "list")
	require.NoError(t, err)
	var list map[string]map[string]string
	require.NoError(t, json.Unmarshal([]byte(stdout), &list), stdout)
	assert.Equal(t, srv.URL, list["hub.endpoint"]["value"])
	assert.Equal(t, "current hub '"+c.CurrentHub+"'", list["hub.endpoint"]["source"])
}

// The root --global flag selects the global layer too.
func TestConfigSetGlobalHubEndpoint_RootGlobalFlag(t *testing.T) {
	setupConfigHubTest(t)
	srv := healthOnlyHub(t)

	_, _, err := runConfig(t, "--global", "config", "set", "hub.endpoint", srv.URL)
	require.NoError(t, err)
	c := readTestContext(t)
	require.NotNil(t, c.CurrentHubEntry())
	assert.Equal(t, srv.URL, c.CurrentHubEntry().Endpoint)
	assertNoHubEndpointInSettings(t)
}

// An entry with the same normalised endpoint is reused under its own name.
func TestConfigSetGlobalHubEndpoint_ReusesTheEntry(t *testing.T) {
	setupConfigHubTest(t)
	srv := healthOnlyHub(t)
	require.NoError(t, config.UpdateCLIContext(func(c *config.CLIContext) error {
		return c.AddHub("prod", srv.URL)
	}))

	stdout, _, err := runConfig(t, "config", "set", "--global", "hub.endpoint", srv.URL+"/")
	require.NoError(t, err)
	assert.Equal(t, "Hub 'prod' ("+srv.URL+") is now the current hub.\n", stdout)
	c := readTestContext(t)
	assert.Equal(t, "prod", c.CurrentHub)
	assert.Len(t, c.Hubs, 1)
}

// A new entry is named from the host, with a number when the name is taken.
func TestConfigSetGlobalHubEndpoint_NumbersANameCollision(t *testing.T) {
	setupConfigHubTest(t)
	hubResponds = func(string, bool) error { return nil }
	require.NoError(t, config.UpdateCLIContext(func(c *config.CLIContext) error {
		return c.AddHub("hub", "https://hub.example.org")
	}))

	stdout, _, err := runConfig(t, "config", "set", "--global", "hub.endpoint", "https://hub.example.com")
	require.NoError(t, err)
	assert.Equal(t, "Hub 'hub-2' (https://hub.example.com) is now the current hub.\n", stdout)
	c := readTestContext(t)
	assert.Equal(t, "hub-2", c.CurrentHub)
	assert.Equal(t, "https://hub.example.org", c.Hubs["hub"].Endpoint)
}

// A URL hub add refuses is refused, and nothing changes.
func TestConfigSetGlobalHubEndpoint_RefusesWhatHubAddRefuses(t *testing.T) {
	setupConfigHubTest(t)
	hubResponds = func(string, bool) error { return nil }
	saveTestContext(t, ctxTestHub, "")
	before, err := os.ReadFile(contextFilePath())
	require.NoError(t, err)

	for _, u := range []string{
		"https://user:placeholder@hub.example.org",
		"https://hub.example.org/?x=1",
		"https://hub.example.org/#frag",
		"hub.example.org",
	} {
		stdout, _, err := runConfig(t, "config", "set", "--global", "hub.endpoint", u)
		require.Error(t, err, u)
		assert.Contains(t, err.Error(), "invalid hub URL", u)
		assert.NotContains(t, err.Error()+stdout, "placeholder", u)
		_, _, addErr := runScion(t, "hub", "add", "other", u)
		assert.Error(t, addErr, "hub add accepts %s", u)
	}
	after, err := os.ReadFile(contextFilePath())
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after))
	assertNoHubEndpointInSettings(t)
}

// An unreachable hub is selected anyway, with hub add's warning.
func TestConfigSetGlobalHubEndpoint_UnreachableWarns(t *testing.T) {
	setupConfigHubTest(t)
	u := closedHubURL(t)

	stdout, stderr, err := runConfig(t, "config", "set", "--global", "hub.endpoint", u)
	require.NoError(t, err)
	assert.Contains(t, stderr, "Warning: the hub at "+u+" did not respond")
	c := readTestContext(t)
	require.NotNil(t, c.CurrentHubEntry())
	assert.Equal(t, u, c.CurrentHubEntry().Endpoint)
	assert.Equal(t, "Hub '"+c.CurrentHub+"' ("+u+") is now the current hub.\n", stdout)
}

// In a project, config set hub.endpoint writes the project's link entry
// (with its project ID), leaves the current hub and the settings alone, and
// config get then reports the link.
func TestConfigSetProjectHubEndpoint_WritesTheLink(t *testing.T) {
	setupConfigHubTest(t)
	hubResponds = func(string, bool) error { return nil }
	saveTestContext(t, ctxTestHub, "")
	root := makeDirProject(t, "")
	settingsPath := filepath.Join(root, ".scion", "settings.yaml")
	settingsBefore, err := os.ReadFile(settingsPath)
	require.NoError(t, err)

	stdout, _, err := runConfig(t, "config", "set", "hub.endpoint", "https://team.example.org")
	require.NoError(t, err)
	assert.Equal(t, "This project is now linked to hub 'team' (https://team.example.org).\n", stdout)

	c := readTestContext(t)
	assert.Equal(t, "prod", c.CurrentHub, "the current hub is unchanged")
	p, h := c.LinkEntryFor(config.LinkPathForProject(filepath.Join(root, ".scion")))
	require.NotNil(t, p)
	assert.Equal(t, "dir-project-id", p.ID)
	assert.Equal(t, "team", p.Hub)
	assert.Equal(t, "https://team.example.org", h.Endpoint)

	settingsAfter, err := os.ReadFile(settingsPath)
	require.NoError(t, err)
	assert.Equal(t, string(settingsBefore), string(settingsAfter), "the project settings are unchanged")

	stdout, _, err = runConfig(t, "config", "get", "--source", "hub.endpoint")
	require.NoError(t, err)
	assert.Equal(t, "https://team.example.org  (link hub 'team')\n", stdout)
	scope := hubStatusScope(false)
	assert.Equal(t, useSourceDirectory, scope.Source, "hub status agrees")
	assert.Equal(t, "https://team.example.org", scope.Endpoint)
}

// config get reports the hub of the environment and of --hub with their
// sources, and a settings value is never a source.
func TestConfigGetHubEndpoint_Sources(t *testing.T) {
	setupConfigHubTest(t)
	writeHomeGlobalSettings(t, "hub:\n  endpoint: https://settings.example.org\n")

	stdout, _, err := runConfig(t, "--global", "config", "get", "--source", "hub.endpoint")
	require.NoError(t, err)
	assert.Equal(t, "<unset>\n", stdout, "the settings file is not a hub source")

	saveTestContext(t, ctxTestHub, "")
	t.Setenv("SCION_HUB_ENDPOINT", "https://env.example.org")
	stdout, _, err = runConfig(t, "--global", "config", "get", "--source", "hub.endpoint")
	require.NoError(t, err)
	assert.Equal(t, "https://env.example.org  (env SCION_HUB_ENDPOINT)\n", stdout)

	stdout, _, err = runConfig(t, "--global", "--hub", "prod", "config", "get", "--source", "hub.endpoint")
	require.NoError(t, err)
	assert.Equal(t, ctxTestHub+"  (flag --hub 'prod')\n", stdout)
}

// config unset --global hub.endpoint clears the current hub; the entry and
// its stored login stay, so selecting the hub again needs no new login.
func TestConfigUnsetGlobalHubEndpoint_ClearsTheCurrentHub(t *testing.T) {
	setupConfigHubTest(t)
	hubResponds = func(string, bool) error { return nil }
	saveTestContext(t, ctxTestHub, "")
	storeTestLogin(t, ctxTestHub)

	stdout, _, err := runConfig(t, "config", "unset", "--global", "hub.endpoint")
	require.NoError(t, err)
	assert.Equal(t, "Hub 'prod' ("+ctxTestHub+") is no longer the current hub; it stays in the hub list.\n"+
		"Now: hub.endpoint: <unset>\n", stdout)
	c := readTestContext(t)
	assert.Empty(t, c.CurrentHub)
	require.NotNil(t, c.Hubs["prod"])
	assert.True(t, credentials.IsAuthenticated(ctxTestHub), "the stored login is kept")

	stdout, _, err = runConfig(t, "config", "unset", "--global", "hub.endpoint")
	require.NoError(t, err)
	assert.Contains(t, stdout, "No hub is current; the hub list is unchanged.")

	_, _, err = runConfig(t, "config", "set", "--global", "hub.endpoint", ctxTestHub)
	require.NoError(t, err)
	assert.Equal(t, "prod", readTestContext(t).CurrentHub)
	assert.True(t, credentials.IsAuthenticated(ctxTestHub))
	assertNoHubEndpointInSettings(t)
}

// In a project, config unset hub.endpoint removes the project's link entry;
// the current hub applies again.
func TestConfigUnsetProjectHubEndpoint_RemovesTheLink(t *testing.T) {
	setupConfigHubTest(t)
	hubResponds = func(string, bool) error { return nil }
	saveTestContext(t, ctxTestHub, "")
	root := makeDirProject(t, "")
	_, _, err := runConfig(t, "config", "set", "hub.endpoint", "https://team.example.org")
	require.NoError(t, err)

	stdout, _, err := runConfig(t, "config", "unset", "hub.endpoint")
	require.NoError(t, err)
	assert.Equal(t, "This project is no longer linked to hub 'team' (https://team.example.org).\n"+
		"Now: hub.endpoint: "+ctxTestHub+"  (current hub 'prod')\n", stdout)
	c := readTestContext(t)
	p, _ := c.LinkEntryFor(config.LinkPathForProject(filepath.Join(root, ".scion")))
	assert.Nil(t, p)
	assert.Equal(t, "prod", c.CurrentHub)
	require.NotNil(t, c.Hubs["team"], "the hub entry stays")

	stdout, _, err = runConfig(t, "config", "unset", "hub.endpoint")
	require.NoError(t, err)
	assert.Contains(t, stdout, "This project is not linked to a hub; nothing changed.")
}

// config unset --global hub.endpoint also removes a legacy hub.endpoint from
// the global settings file (the server and the broker still fall back to
// it), says so on stderr, and keeps the rest of the file; with no legacy
// key it says nothing about settings.
func TestConfigUnsetGlobalHubEndpoint_RemovesTheLegacySetting(t *testing.T) {
	setupConfigHubTest(t)
	writeHomeGlobalSettings(t, "image_registry: registry.example.test/team\nhub:\n  endpoint: https://legacy.example.org\n")
	saveTestContext(t, ctxTestHub, "")

	stdout, stderr, err := runConfig(t, "config", "unset", "--global", "hub.endpoint")
	require.NoError(t, err)
	assert.Contains(t, stdout, "Hub 'prod' ("+ctxTestHub+") is no longer the current hub")
	assert.Contains(t, stderr, "Removed the legacy hub.endpoint from "+globalSettingsPath())
	assertNoHubEndpointInSettings(t)
	data, err := os.ReadFile(globalSettingsPath())
	require.NoError(t, err)
	assert.Contains(t, string(data), "registry.example.test/team", "the rest of the file is kept")

	// No hub current, no legacy key: nothing about settings.
	_, stderr, err = runConfig(t, "config", "unset", "--global", "hub.endpoint")
	require.NoError(t, err)
	assert.NotContains(t, stderr, "legacy")
}

// With no hub current, the legacy key is still removed, and the JSON result
// says so.
func TestConfigUnsetGlobalHubEndpoint_LegacySettingWithoutCurrentHubJSON(t *testing.T) {
	setupConfigHubTest(t)
	writeHomeGlobalSettings(t, "hub:\n  endpoint: https://legacy.example.org\n")

	stdout, stderr, err := runConfig(t, "--format", "json", "config", "unset", "--global", "hub.endpoint")
	require.NoError(t, err)
	assert.Contains(t, stderr, "Removed the legacy hub.endpoint from ")
	var res ActionResult
	require.NoError(t, json.Unmarshal([]byte(stdout), &res), stdout)
	assert.Equal(t, "No hub is current; the hub list is unchanged.", res.Message)
	assert.Equal(t, true, res.Details["legacy_setting_removed"])
	assert.Equal(t, false, res.Details["removed"])
	assertNoHubEndpointInSettings(t)
}

// config set --global hub.endpoint with --format json reports the hub, the
// scope and whether the entry was created.
func TestConfigSetGlobalHubEndpoint_JSON(t *testing.T) {
	setupConfigHubTest(t)
	hubResponds = func(string, bool) error { return nil }

	stdout, _, err := runConfig(t, "--format", "json", "config", "set", "--global", "hub.endpoint", "https://team.example.org/")
	require.NoError(t, err)
	var res ActionResult
	require.NoError(t, json.Unmarshal([]byte(stdout), &res), stdout)
	assert.Equal(t, "Hub 'team' (https://team.example.org) is now the current hub.", res.Message)
	assert.Equal(t, "team", res.Details["hub"])
	assert.Equal(t, "https://team.example.org", res.Details["endpoint"])
	assert.Equal(t, "global", res.Details["scope"])
	assert.Equal(t, true, res.Details["created"])
	assert.Equal(t, true, res.Details["reachable"])
	assert.Equal(t, "https://team.example.org", res.Details["value"])
	assert.Equal(t, "current hub 'team'", res.Details["source"])
}

// When the hub config set selects does not apply, because the environment or
// the directory's link chooses another hub, set says so on stderr and still
// prints its one line on stdout.
func TestConfigSetGlobalHubEndpoint_SaysWhenAnotherHubApplies(t *testing.T) {
	t.Run("environment", func(t *testing.T) {
		setupConfigHubTest(t)
		hubResponds = func(string, bool) error { return nil }
		t.Setenv("SCION_HUB_ENDPOINT", "https://env.example.org")

		stdout, stderr, err := runConfig(t, "config", "set", "--global", "hub.endpoint", "https://team.example.org")
		require.NoError(t, err)
		assert.Equal(t, "Hub 'team' (https://team.example.org) is now the current hub.\n", stdout)
		assert.Equal(t, "Warning: hub.endpoint is still set by SCION_HUB_ENDPOINT\n", stderr)
		assert.Equal(t, "team", readTestContext(t).CurrentHub)
	})
	t.Run("directory link", func(t *testing.T) {
		setupConfigHubTest(t)
		hubResponds = func(string, bool) error { return nil }
		root := makeDirProject(t, "")
		saveLinkEntry(t, filepath.Join(root, ".scion"), "https://linked.example.org", "linked")

		stdout, stderr, err := runConfig(t, "config", "set", "--global", "hub.endpoint", "https://team.example.org")
		require.NoError(t, err)
		assert.Equal(t, "Hub 'team' (https://team.example.org) is now the current hub.\n", stdout)
		assert.Equal(t, "Note: here hub.endpoint still comes from link hub 'linked': https://linked.example.org\n", stderr)
	})
	t.Run("--hub flag", func(t *testing.T) {
		setupConfigHubTest(t)
		hubResponds = func(string, bool) error { return nil }

		stdout, stderr, err := runConfig(t, "--hub", "https://flag.example.org", "config", "set", "--global", "hub.endpoint", "https://team.example.org")
		require.NoError(t, err)
		assert.Equal(t, "Hub 'team' (https://team.example.org) is now the current hub.\n", stdout)
		assert.Equal(t, "Note: here hub.endpoint still comes from flag --hub: https://flag.example.org\n", stderr)
	})
	t.Run("the environment names the hub, a directory link names another", func(t *testing.T) {
		setupConfigHubTest(t)
		hubResponds = func(string, bool) error { return nil }
		root := makeDirProject(t, "")
		saveLinkEntry(t, filepath.Join(root, ".scion"), "https://linked.example.org", "linked")
		t.Setenv("SCION_HUB_ENDPOINT", "https://team.example.org")

		_, stderr, err := runConfig(t, "config", "set", "--global", "hub.endpoint", "https://team.example.org")
		require.NoError(t, err)
		assert.Equal(t, "Note: here hub.endpoint still comes from link hub 'linked': https://linked.example.org\n", stderr)
	})
	t.Run("the same hub: nothing to say", func(t *testing.T) {
		setupConfigHubTest(t)
		hubResponds = func(string, bool) error { return nil }
		t.Setenv("SCION_HUB_ENDPOINT", "https://team.example.org/")

		_, stderr, err := runConfig(t, "config", "set", "--global", "hub.endpoint", "https://team.example.org")
		require.NoError(t, err)
		assert.Empty(t, stderr)
	})
}

// In a linked project whose link was never imported, config unset
// hub.endpoint (run while the environment chooses the hub, so the import does
// not run first) marks the project as imported: a later plain run, with the
// import on, does not link it to the hub its settings name (a hub in the hub
// list), and the current hub applies. A second unset changes nothing.
func TestConfigUnsetProjectHubEndpoint_StopsTheImport(t *testing.T) {
	setupConfigHubTest(t)
	enableGlobalHubImport(t)
	saveTestContext(t, ctxTestHub, "")
	// The project's hub is in the hub list, so without the mark a plain run
	// would link the project to it.
	require.NoError(t, config.UpdateCLIContext(func(c *config.CLIContext) error {
		return c.AddHub("settings", "https://settings.example.org")
	}))
	dotScion := makeLinkedDirProject(t, "https://settings.example.org")
	linkPath := config.LinkPathForProject(dotScion)

	t.Setenv("SCION_HUB_ENDPOINT", "https://env.example.org")
	stdout, _, err := runConfig(t, "config", "unset", "hub.endpoint")
	require.NoError(t, err)
	assert.Contains(t, stdout, "This project is not linked to a hub; nothing changed.")
	assert.True(t, readTestContext(t).ProjectImported(linkPath), "the project is marked as imported")

	before, err := os.ReadFile(contextFilePath())
	require.NoError(t, err)
	_, _, err = runConfig(t, "config", "unset", "hub.endpoint")
	require.NoError(t, err)
	after, err := os.ReadFile(contextFilePath())
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after), "context.yaml is not rewritten")

	t.Setenv("SCION_HUB_ENDPOINT", "")
	stdout, _, err = runConfig(t, "config", "get", "--source", "hub.endpoint")
	require.NoError(t, err)
	assert.Equal(t, ctxTestHub+"  (current hub 'prod')\n", stdout, "the project's settings were not imported into a link")
	p, _ := readTestContext(t).LinkEntryFor(linkPath)
	assert.Nil(t, p, "no link entry was imported")
}

// When the global settings still spell hub.endpoint in a form config unset
// does not edit, the global unset says so and names the file.
func TestConfigUnsetGlobalHubEndpoint_WarnsAboutAFormItCannotEdit(t *testing.T) {
	setupConfigHubTest(t)
	writeHomeGlobalSettings(t, "schema_version: \"1\"\nhub:\n  Endpoint: https://legacy-case.example.org\n")

	_, stderr, err := runConfig(t, "config", "unset", "--global", "hub.endpoint")
	require.NoError(t, err)
	assert.Contains(t, stderr, "Warning: hub.endpoint is still set in "+globalSettingsPath()+" in a form config unset does not edit; remove it by hand")

	// The environment setting the hub too does not hide the file.
	t.Setenv("SCION_HUB_ENDPOINT", "https://env.example.org")
	_, stderr, err = runConfig(t, "config", "unset", "--global", "hub.endpoint")
	require.NoError(t, err)
	assert.Contains(t, stderr, "Warning: hub.endpoint is still set in "+globalSettingsPath()+" in a form config unset does not edit; remove it by hand")
	assert.Contains(t, stderr, "Warning: hub.endpoint is still set by SCION_HUB_ENDPOINT")
}

// When the first-run import of the global settings' hub exists only in
// memory (context.yaml cannot be written), config get still names the hub
// entry: the name comes from the invocation's resolution, not from the file.
func TestConfigGetHubEndpoint_NamesAnImportHeldInMemory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permissions are not enforced for root")
	}
	setupConfigHubTest(t)
	enableGlobalHubImport(t)
	writeHomeGlobalSettings(t, "hub:\n  endpoint: https://global.example.org\n")
	dir := filepath.Dir(globalSettingsPath())
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	stdout, stderr, err := runConfig(t, "--global", "config", "get", "--source", "hub.endpoint")
	require.NoError(t, err)
	assert.Equal(t, "https://global.example.org  (current hub 'global')\n", stdout)
	assert.Contains(t, stderr, "could not save the imported hub settings")
	_, statErr := os.Stat(contextFilePath())
	assert.True(t, os.IsNotExist(statErr), "context.yaml was not written")
}

// config set prints at most one line on stderr, and only when another hub
// than the one it set applies: the cases of the --hub flag, the environment
// and the directory's link, at both scopes.
func TestConfigSetHubEndpoint_ReportsAnotherHubOnlyWhenOneApplies(t *testing.T) {
	const (
		newHub  = "https://team.example.org"
		flagHub = "https://flag.example.org"
		envHub  = "https://env.example.org"
		linkHub = "https://linked.example.org"
	)
	cases := []struct {
		name    string
		global  bool
		current bool   // a current hub is set
		link    string // the directory's link entry ("" for none)
		env     string
		flag    string
		want    string
	}{
		{name: "global, --hub names the new hub", global: true, flag: newHub},
		{name: "global, --hub names the new hub, the directory link another", global: true, flag: newHub, link: linkHub,
			want: "Note: here hub.endpoint still comes from link hub 'linked': " + linkHub + "\n"},
		{name: "global, the directory link names the new hub", global: true, link: newHub},
		{name: "project, --hub names another hub", flag: flagHub,
			want: "Note: here hub.endpoint still comes from flag --hub: " + flagHub + "\n"},
		{name: "project, --hub names the new hub", flag: newHub},
		{name: "project, the environment names another hub", env: envHub,
			want: "Warning: hub.endpoint is still set by SCION_HUB_ENDPOINT\n"},
		{name: "project, the environment names the new hub", env: newHub},
		{name: "project, a current hub", current: true},
		{name: "project, an older link to another hub", link: linkHub},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setupConfigHubTest(t)
			hubResponds = func(string, bool) error { return nil }
			if tc.current {
				saveTestContext(t, ctxTestHub, "")
			}
			root := makeDirProject(t, "")
			if tc.link != "" {
				saveLinkEntry(t, filepath.Join(root, ".scion"), tc.link, "linked")
			}
			t.Setenv("SCION_HUB_ENDPOINT", tc.env)
			var args []string
			if tc.flag != "" {
				args = append(args, "--hub", tc.flag)
			}
			args = append(args, "config", "set")
			if tc.global {
				args = append(args, "--global")
			}
			args = append(args, "hub.endpoint", newHub)

			stdout, stderr, err := runConfig(t, args...)
			require.NoError(t, err)
			assert.Equal(t, 1, strings.Count(stdout, "\n"), "stdout keeps one line: %q", stdout)
			assert.Equal(t, tc.want, stderr)
		})
	}
}
