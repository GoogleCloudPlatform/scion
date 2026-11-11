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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConfigUnknownSubcommand_RejectsRemovedGroveAlias is a regression test
// for the removed "config cd-grove" alias. configCmd has no subcommand
// named "cd-grove", but before configCmd was made Runnable, cobra silently
// fell through to config's own help with exit status 0 for any
// unrecognized config subcommand — including this one — instead of
// reporting an error. That made a removed command indistinguishable from a
// typo and let old scripts' error checks pass silently. This executes the
// real rootCmd, since the behavior depends on cobra's command-resolution
// path through the actual tree, not a synthetic one.
func TestConfigUnknownSubcommand_RejectsRemovedGroveAlias(t *testing.T) {
	restoreAllSilenceUsage(t)
	var buf bytes.Buffer
	rootCmd.SetArgs([]string{"config", "cd-grove"})
	rootCmd.SetOut(&buf)
	rootCmd.SetErr(&buf)
	defer func() {
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
	}()

	err := rootCmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown command "cd-grove" for "scion config"`)
}

// TestConfigBareInvocation_PrintsHelpOutsideProject is a regression test
// for a side effect of making configCmd Runnable: once ValidateArgs runs
// for "config", execution would otherwise continue into root's
// PersistentPreRunE, which requires an active scion project for "config"
// (it is not in that hook's exempt command list). A bare "scion config"
// run outside any project would then fail with "not in a scion project"
// instead of printing config's help, even though a plain "scion config"
// never did anything project-specific before. configCmd's Args validator
// returns pflag.ErrHelp for zero args (and for a leading "help" argument,
// since cobra only auto-registers a real "help" subcommand on the root
// command) specifically to make cobra print help and stop *before* that
// hook runs. This test runs both cases from a temp directory that is not a
// scion project, with a clean HOME, so it fails loudly if that
// short-circuit regresses for either one — a mutation that only
// special-cases zero args (dropping the "help" branch) passes unless the
// "help" sub-case below is present.
func TestConfigBareInvocation_PrintsHelpOutsideProject(t *testing.T) {
	restoreAllSilenceUsage(t)
	cases := []struct {
		name string
		args []string
	}{
		{name: "bare", args: []string{"config"}},
		{name: "help", args: []string{"config", "help"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Chdir(t.TempDir())

			var buf bytes.Buffer
			rootCmd.SetArgs(tc.args)
			rootCmd.SetOut(&buf)
			rootCmd.SetErr(&buf)
			defer func() {
				rootCmd.SetArgs(nil)
				rootCmd.SetOut(nil)
				rootCmd.SetErr(nil)
			}()

			err := rootCmd.Execute()
			require.NoError(t, err)
			assert.Contains(t, buf.String(), "View and modify settings for scion-agent")
		})
	}
}

// setupProjectSettings writes vs as a versioned settings.yaml into a fresh
// temp project's .scion directory and returns the project root — not the
// .scion dir itself — matching how projectPath is set directly elsewhere in
// this package (see attach_test.go) to skip project-root discovery.
//
// It also isolates HOME to a separate temp dir (LoadVersionedSettings layers
// ~/.scion/settings.yaml under the project file, so without this an
// unrelated or invalid global settings file on the machine running the test
// could change or break it — see TestConfigBareInvocation_PrintsHelpOutsideProject
// for the same pattern) and clears SCION_PROJECT/SCION_PROJECT_ID/
// SCION_PROJECT_PATH, which the ambient environment sometimes sets (e.g.
// inside an agent container) and which LoadVersionedSettings' env layer
// would otherwise merge in.
func setupProjectSettings(t *testing.T, vs *config.VersionedSettings) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	for _, name := range []string{"SCION_PROJECT", "SCION_PROJECT_ID", "SCION_PROJECT_PATH"} {
		t.Setenv(name, "")
	}

	root := t.TempDir()
	scionDir := filepath.Join(root, ".scion")
	require.NoError(t, os.MkdirAll(scionDir, 0o755))
	require.NoError(t, config.SaveVersionedSettings(scionDir, vs))
	return root
}

// TestConfigGetCmd_NestedProfileAndRuntimeKeys is a cmd-level regression test
// for ptone/scion#2261: "scion config get" dotted paths into the profiles
// and runtimes maps must resolve through the real command wiring (project
// resolution -> LoadVersionedSettings -> GetVersionedSettingValue -> print),
// not just the underlying pkg/config function.
func TestConfigGetCmd_NestedProfileAndRuntimeKeys(t *testing.T) {
	vs := &config.VersionedSettings{
		SchemaVersion: "1",
		Profiles: map[string]config.V1ProfileConfig{
			"ci": {Runtime: "docker", ImageRegistry: "ghcr.io/example"},
		},
		Runtimes: map[string]config.V1RuntimeConfig{
			"local": {Type: "docker", Namespace: "ns1"},
		},
	}
	root := setupProjectSettings(t, vs)

	origProjectPath := projectPath
	t.Cleanup(func() { projectPath = origProjectPath })
	projectPath = root

	tests := []struct {
		key  string
		want string
	}{
		{"profiles.ci.runtime", "docker"},
		{"profiles.ci.image_registry", "ghcr.io/example"},
		{"runtimes.local.namespace", "ns1"},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			var runErr error
			out := captureStdout(t, func() {
				runErr = configGetCmd.RunE(configGetCmd, []string{tt.key})
			})
			require.NoError(t, runErr)
			assert.Equal(t, tt.want+"\n", out)
		})
	}
}

// TestConfigGetCmd_NestedKeyErrors verifies that a missing profile/runtime
// name, an unknown field on an otherwise-valid entry, and a structured
// (non-scalar) field all return a non-nil error naming the offending key
// from configGetCmd's RunE, which callers propagate to a non-zero process
// exit, rather than succeeding silently or printing nothing.
func TestConfigGetCmd_NestedKeyErrors(t *testing.T) {
	vs := &config.VersionedSettings{
		SchemaVersion: "1",
		Profiles: map[string]config.V1ProfileConfig{
			"ci": {Runtime: "docker"},
		},
		Runtimes: map[string]config.V1RuntimeConfig{
			"local": {Type: "docker", Env: map[string]string{"FOO": "bar"}},
		},
	}
	root := setupProjectSettings(t, vs)

	origProjectPath := projectPath
	t.Cleanup(func() { projectPath = origProjectPath })
	projectPath = root

	tests := []struct {
		name    string
		key     string
		wantErr string
	}{
		{"missing profile name", "profiles.missing.runtime", `no profile named "missing"`},
		{"unknown field", "profiles.ci.nope", `has no field "nope"`},
		{"non-scalar field", "runtimes.local.env", "is not a scalar value"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var runErr error
			out := captureStdout(t, func() {
				runErr = configGetCmd.RunE(configGetCmd, []string{tt.key})
			})
			require.Error(t, runErr)
			assert.Contains(t, runErr.Error(), tt.key)
			assert.Contains(t, runErr.Error(), tt.wantErr)
			// The CLI contract is error-on-stderr with a non-zero exit and
			// nothing on stdout, so `$(scion config get ...)` in a script never
			// captures a partial value alongside the error. A regression that
			// prints before returning the error would otherwise pass silently.
			assert.Empty(t, out)
		})
	}
}

// TestConfigSetGlobalHelpNamesSettingsYAML: config set --global writes
// ~/.scion/settings.yaml, and the help must say so.
func TestConfigSetGlobalHelpNamesSettingsYAML(t *testing.T) {
	usage := configSetCmd.Flags().Lookup("global").Usage
	assert.Contains(t, usage, "settings.yaml")
	assert.NotContains(t, usage, "settings.json")
	assert.NotContains(t, configCmd.Long, "settings.json")
}
