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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// clearSettingsEnv sets every SCION_* variable of the process to empty for
// the test, which the settings loaders treat as unset.
func clearSettingsEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		if name, _, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(name, "SCION_") {
			t.Setenv(name, "")
		}
	}
}

// configTestProject creates a project with the given project settings YAML
// and global settings YAML (each skipped when empty) in a fresh HOME, and
// points the commands at it.
func configTestProject(t *testing.T, globalYAML, projectYAML string) (root, globalDir string) {
	t.Helper()
	clearSettingsEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(t.TempDir())
	globalDir = filepath.Join(home, ".scion")
	require.NoError(t, os.MkdirAll(globalDir, 0o755))
	if globalYAML != "" {
		require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(globalYAML), 0o644))
	}
	root = t.TempDir()
	scionDir := filepath.Join(root, ".scion")
	require.NoError(t, os.MkdirAll(scionDir, 0o755))
	if projectYAML != "" {
		require.NoError(t, os.WriteFile(filepath.Join(scionDir, "settings.yaml"), []byte(projectYAML), 0o644))
	}
	origProjectPath, origFormat := projectPath, outputFormat
	t.Cleanup(func() { projectPath, outputFormat = origProjectPath, origFormat })
	projectPath = root
	return root, globalDir
}

// runConfigListJSON runs "config list --format json" and returns its map.
func runConfigListJSON(t *testing.T) map[string]map[string]string {
	t.Helper()
	outputFormat = "json"
	var runErr error
	out := captureStdout(t, func() { runErr = configListCmd.RunE(configListCmd, nil) })
	require.NoError(t, runErr)
	var m map[string]map[string]string
	require.NoError(t, json.Unmarshal([]byte(out), &m), out)
	return m
}

// runConfigGetJSON runs "config get --format json key".
func runConfigGetJSON(t *testing.T, key string) map[string]string {
	t.Helper()
	outputFormat = "json"
	var runErr error
	out := captureStdout(t, func() { runErr = configGetCmd.RunE(configGetCmd, []string{key}) })
	require.NoError(t, runErr, key)
	var m map[string]string
	require.NoError(t, json.Unmarshal([]byte(out), &m), out)
	return m
}

// TestConfigListGetAgree: for every environment and file combination,
// config get gives each key listed by config list the same value and
// source (ptone/scion#3919 acceptance 1).
func TestConfigListGetAgree(t *testing.T) {
	cases := []struct {
		name, global, project string
		env                   map[string]string
		check                 map[string][2]string // key -> {value, source prefix}
	}{
		{name: "defaults only",
			check: map[string][2]string{"cli.autohelp": {"true", "default"}, "active_profile": {"local", "default"}}},
		{name: "global file", global: "schema_version: \"1\"\nhub:\n  endpoint: https://g.example.com\n",
			check: map[string][2]string{"hub.endpoint": {"https://g.example.com", "global "}}},
		{name: "project over global",
			global:  "schema_version: \"1\"\ncli:\n  autohelp: false\n",
			project: "schema_version: \"1\"\ncli:\n  autohelp: true\nactive_profile: remote\n",
			check:   map[string][2]string{"cli.autohelp": {"true", "project "}, "active_profile": {"remote", "project "}}},
		{name: "env over files",
			project: "schema_version: \"1\"\nhub:\n  endpoint: https://p.example.com\n",
			env:     map[string]string{"SCION_HUB_ENDPOINT": "https://env.example.com"},
			check:   map[string][2]string{"hub.endpoint": {"https://env.example.com", "env SCION_HUB_ENDPOINT"}}},
		{name: "legacy-format project file",
			project: "harnesses: {}\nhub:\n  endpoint: https://legacy.example.com\n",
			check:   map[string][2]string{"hub.endpoint": {"https://legacy.example.com", "project "}}},
		{name: "masked broker token",
			global: "schema_version: \"1\"\nserver:\n  broker:\n    broker_token: do-not-print\n",
			check:  map[string][2]string{"hub.brokerToken": {"********", "global "}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			configTestProject(t, tc.global, tc.project)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			list := runConfigListJSON(t)
			require.NotEmpty(t, list)
			for key, lv := range list {
				gv := runConfigGetJSON(t, key)
				assert.Equal(t, lv["value"], gv["value"], "%s value: list vs get", key)
				assert.Equal(t, lv["source"], gv["source"], "%s source: list vs get", key)
			}
			for key, want := range tc.check {
				lv, ok := list[key]
				require.True(t, ok, "config list lacks %s", key)
				assert.Equal(t, want[0], lv["value"], key)
				assert.True(t, strings.HasPrefix(lv["source"], want[1]), "%s source %q, want prefix %q", key, lv["source"], want[1])
			}
		})
	}
}

// config list's text output shows each value with its source, <unset> for
// a setting without a value, and never a credential's value.
func TestConfigList_TextShowsSourcesAndMasks(t *testing.T) {
	configTestProject(t, "schema_version: \"1\"\nserver:\n  broker:\n    broker_token: do-not-print\n", "schema_version: \"1\"\nactive_profile: remote\n")
	outputFormat = ""
	var runErr error
	out := captureStdout(t, func() { runErr = configListCmd.RunE(configListCmd, nil) })
	require.NoError(t, runErr)
	assert.Contains(t, out, "Effective Settings:")
	assert.Contains(t, out, "  active_profile: remote  (project ")
	assert.Contains(t, out, "  cli.autohelp: true  (default)")
	assert.Contains(t, out, "  hub.endpoint: <unset>")
	assert.Contains(t, out, "  hub.brokerToken: ********  (global ")
	assert.NotContains(t, out, "do-not-print")
}

// config get prints the value alone, or with --source its source too; a
// credential is masked.
func TestConfigGet_SourceFlagAndMask(t *testing.T) {
	configTestProject(t, "schema_version: \"1\"\nserver:\n  broker:\n    broker_token: do-not-print\n", "")
	outputFormat = ""
	origSource := configGetSource
	t.Cleanup(func() { configGetSource = origSource })

	run := func(key string) string {
		var runErr error
		out := captureStdout(t, func() { runErr = configGetCmd.RunE(configGetCmd, []string{key}) })
		require.NoError(t, runErr)
		return out
	}
	configGetSource = false
	assert.Equal(t, "true\n", run("cli.autohelp"))
	assert.Equal(t, "********\n", run("hub.brokerToken"))
	configGetSource = true
	assert.Equal(t, "true  (default)\n", run("cli.autohelp"))
	assert.Equal(t, "<unset>\n", run("hub.endpoint"))
	assert.True(t, strings.HasPrefix(run("hub.brokerToken"), "********  (global "))
}

// runConfigUnset runs "config unset key" through the command's RunE.
func runConfigUnset(t *testing.T, global bool, key string) (stdout, stderr string, err error) {
	t.Helper()
	orig := configUnsetGlobal
	t.Cleanup(func() { configUnsetGlobal = orig })
	configUnsetGlobal = global
	stdout, stderr = captureStdoutStderr(t, func() {
		err = configUnsetCmd.RunE(configUnsetCmd, []string{key})
	})
	return stdout, stderr, err
}

// TestConfigUnset_AutohelpRemovesKey: config unset cli.autohelp removes the
// key from the project file, so the setting inherits the default instead
// of staying false (ptone/scion#3919 acceptance 2).
func TestConfigUnset_AutohelpRemovesKey(t *testing.T) {
	root, _ := configTestProject(t, "", "schema_version: \"1\"\n# keep me\nactive_profile: remote\ncli:\n  autohelp: false\n")
	outputFormat = ""

	stdout, _, err := runConfigUnset(t, false, "cli.autohelp")
	require.NoError(t, err)
	assert.Contains(t, stdout, "Removed local setting 'cli.autohelp'")
	assert.Contains(t, stdout, "Now: cli.autohelp: true  (default)")

	data, err := os.ReadFile(filepath.Join(root, ".scion", "settings.yaml"))
	require.NoError(t, err)
	assert.NotContains(t, string(data), "autohelp")
	assert.Contains(t, string(data), "# keep me")
	assert.Contains(t, string(data), "active_profile: remote")

	stdout, _, err = runConfigUnset(t, false, "cli.autohelp")
	require.NoError(t, err)
	assert.Contains(t, stdout, "is not set; nothing changed")
}

// config unset warns when an environment variable still sets the key, and
// refuses a key no layer reads.
func TestConfigUnset_EnvPinnedAndRefusedKey(t *testing.T) {
	configTestProject(t, "", "schema_version: \"1\"\nhub:\n  endpoint: https://p.example.com\n")
	outputFormat = ""
	t.Setenv("SCION_HUB_ENDPOINT", "https://env.example.com")

	_, stderr, err := runConfigUnset(t, false, "hub.endpoint")
	require.NoError(t, err)
	assert.Contains(t, stderr, "still set by SCION_HUB_ENDPOINT")

	_, _, err = runConfigUnset(t, false, "hub.token")
	require.Error(t, err)
	assert.ErrorIs(t, err, config.ErrSettingNotWritable)
}

// The documented way back from agent mode (SCION_CLI_MODE=human scion
// config unset cli.mode) prints a one-line note saying what cli.mode is
// without the variable, not the still-set warning (ptone/scion#5483).
func TestConfigUnset_CLIModeEnvNote(t *testing.T) {
	_, globalDir := configTestProject(t, "schema_version: \"1\"\ncli:\n  mode: agent\n", "schema_version: \"1\"\ncli:\n  mode: agent\n")
	outputFormat = ""
	t.Setenv("SCION_CLI_MODE", "human")

	_, stderr, err := runConfigUnset(t, false, "cli.mode")
	require.NoError(t, err)
	assert.NotContains(t, stderr, "still set by")
	want := "Note: SCION_CLI_MODE sets cli.mode for this command; without it, cli.mode is now agent (global " +
		filepath.Join(globalDir, "settings.yaml") + ").\n"
	assert.Contains(t, stderr, want)
	assert.Equal(t, 1, strings.Count(stderr, "Note: SCION_CLI_MODE"), "stderr: %s", stderr)

	// With the global setting removed too, nothing sets it.
	_, stderr, err = runConfigUnset(t, true, "cli.mode")
	require.NoError(t, err)
	assert.NotContains(t, stderr, "still set by")
	assert.Contains(t, stderr, "Note: SCION_CLI_MODE sets cli.mode for this command; without it, cli.mode is now <unset>.\n")
}

// resetConfigUnsetFlags restores config unset's --global flag after a real
// rootCmd execution, since pflag keeps values across Execute calls.
func resetConfigUnsetFlags(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		configUnsetGlobal = false
		if f := configUnsetCmd.Flags().Lookup("global"); f != nil {
			f.Changed = false
		}
		if f := rootCmd.PersistentFlags().Lookup("global"); f != nil {
			f.Changed = false
		}
		rootCmd.SetArgs(nil)
	})
}

// TestConfigUnsetGlobalOutsideProject: like config set --global
// (ptone/scion#3317), config unset --global runs outside any project and
// edits the global settings file, for every placement of --global.
func TestConfigUnsetGlobalOutsideProject(t *testing.T) {
	cases := map[string][]string{
		"subcommand flag": {"config", "unset", "--global", "image_registry"},
		"root and sub":    {"--global", "config", "unset", "--global", "image_registry"},
		"root placement":  {"--global", "config", "unset", "image_registry"},
		"flag after key":  {"config", "unset", "image_registry", "--global"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			setupNoProjectPreRun(t)
			restoreAllSilenceUsage(t)
			resetConfigUnsetFlags(t)
			home := os.Getenv("HOME")
			globalDir := filepath.Join(home, ".scion")
			require.NoError(t, os.MkdirAll(globalDir, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"),
				[]byte("schema_version: \"1\"\nimage_registry: registry.example.test/team\nactive_profile: local\n"), 0o644))

			rootCmd.SetArgs(args)
			require.NoError(t, rootCmd.Execute())

			data, err := os.ReadFile(filepath.Join(globalDir, "settings.yaml"))
			require.NoError(t, err)
			assert.NotContains(t, string(data), "registry.example.test/team")
			assert.Contains(t, string(data), "active_profile: local")
		})
	}
}

// cli.mode is settable with config set and is then the CLI mode, with no
// SCION_CLI_MODE (ptone/scion#3314); a removed or unknown mode is refused.
func TestConfigSetCLIModeResolvesMode(t *testing.T) {
	configTestProject(t, "schema_version: \"1\"\n", "")
	outputFormat = ""

	_, _, err := runConfigSet(t, true, "cli.mode", "agent")
	require.NoError(t, err)
	assert.Equal(t, ModeAgent, resolveMode())

	v := runConfigGetJSON(t, "cli.mode")
	assert.Equal(t, "agent", v["value"])
	assert.True(t, strings.HasPrefix(v["source"], "global "), v["source"])

	outputFormat = ""
	for _, bad := range []string{"assistant", "robot"} {
		_, _, err := runConfigSet(t, true, "cli.mode", bad)
		require.Error(t, err, bad)
	}

	_, _, err = runConfigUnset(t, true, "cli.mode")
	require.NoError(t, err)
	assert.Equal(t, ModeHuman, resolveMode())
}

// config set cli.mode agent says that the config commands go away and how
// to return to human mode (agent mode removes config unset too).
func TestConfigSetCLIModeAgentPrintsWayBack(t *testing.T) {
	configTestProject(t, "schema_version: \"1\"\n", "")
	outputFormat = ""

	_, stderr, err := runConfigSet(t, true, "cli.mode", "agent")
	require.NoError(t, err)
	assert.Contains(t, stderr, `agent mode removes the "config" commands`)
	assert.Contains(t, stderr, "SCION_CLI_MODE=human scion config unset cli.mode")

	_, stderr, err = runConfigSet(t, true, "cli.mode", "human")
	require.NoError(t, err)
	assert.NotContains(t, stderr, "agent mode removes")
}

// config list and config get --source print a value the same way: <unset>
// without a value, and "(unknown source)" for a value with no recorded
// layer.
func TestFormatValueSource(t *testing.T) {
	assert.Equal(t, "<unset>", formatValueSource(config.SettingValue{Key: "k"}))
	assert.Equal(t, "x  (global /h/.scion/settings.yaml)", formatValueSource(config.SettingValue{Key: "k", Value: "x", Source: "global /h/.scion/settings.yaml"}))
	assert.Equal(t, "x  (unknown source)", formatValueSource(config.SettingValue{Key: "k", Value: "x"}))
	assert.Equal(t, "k: x  (unknown source)", formatSettingValue(config.SettingValue{Key: "k", Value: "x"}))
}

// When the edited file still sets the key in a form config unset does not
// edit (here a key spelt in another case), unset says so instead of
// "nothing changed".
func TestConfigUnset_ReportsKeyStillSet(t *testing.T) {
	configTestProject(t, "", "schema_version: \"1\"\nhub:\n  Endpoint: https://case.example.com\n")
	outputFormat = ""

	stdout, _, err := runConfigUnset(t, false, "hub.endpoint")
	require.NoError(t, err)
	assert.NotContains(t, stdout, "nothing changed")
	assert.Contains(t, stdout, "was not removed: it is still set in")
	assert.Contains(t, stdout, "in a form config unset does not edit")
	assert.Contains(t, stdout, "edit that file by hand")
}

// In a project whose settings are kept in its external settings directory
// (it has a project-id file), config unset edits that directory; a key set
// only in the in-repo settings file is reported as set in a file config
// unset does not edit.
func TestConfigUnset_ReportsInRepoFileNotEdited(t *testing.T) {
	root, _ := configTestProject(t, "", "schema_version: \"1\"\nhub:\n  endpoint: https://in-repo.example.com\n")
	require.NoError(t, os.WriteFile(filepath.Join(root, ".scion", "project-id"), []byte("split-project-id\n"), 0o644))
	outputFormat = ""

	stdout, _, err := runConfigUnset(t, false, "hub.endpoint")
	require.NoError(t, err)
	assert.NotContains(t, stdout, "nothing changed")
	assert.Contains(t, stdout, "which config unset does not edit")
	assert.Contains(t, stdout, filepath.Join(root, ".scion", "settings.yaml"))
}
