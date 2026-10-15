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

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHubAndBrokerCommandsRunOutsideProject covers ptone/scion#3317: commands
// that act on the hub, a login or this machine's broker must pass root's
// project gate in a fresh HOME outside any project, without --global.
func TestHubAndBrokerCommandsRunOutsideProject(t *testing.T) {
	cmds := []*cobra.Command{
		hubAuthLoginCmd, hubAuthLogoutCmd,
		hubStatusCmd, hubEnableCmd, hubDisableCmd,
		brokerRegisterCmd, brokerDeregisterCmd, brokerStartCmd, brokerStopCmd,
		brokerRestartCmd, brokerStatusCmd, brokerHubsCmd, brokerJoinCmd,
	}
	for _, c := range cmds {
		t.Run(c.CommandPath(), func(t *testing.T) {
			setupNoProjectPreRun(t)
			restoreSilenceUsage(t, c)
			assert.NoError(t, rootCmd.PersistentPreRunE(c, []string{}))
		})
	}
}

// TestProjectScopedHubAndBrokerCommandsStillRequireProject keeps the #3317
// exemptions narrow: commands that act on a project keep the gate.
func TestProjectScopedHubAndBrokerCommandsStillRequireProject(t *testing.T) {
	cmds := []*cobra.Command{hubLinkCmd, hubUnlinkCmd, brokerProvideCmd, brokerWithdrawCmd}
	for _, c := range cmds {
		t.Run(c.CommandPath(), func(t *testing.T) {
			setupNoProjectPreRun(t)
			restoreSilenceUsage(t, c)
			err := rootCmd.PersistentPreRunE(c, []string{})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "not in a scion project")
		})
	}

	// A "status" command that is not a direct child of "scion hub" is not
	// exempted by name alone.
	setupNoProjectPreRun(t)
	other := &cobra.Command{Use: "other"}
	status := &cobra.Command{Use: "status"}
	other.AddCommand(status)
	err := rootCmd.PersistentPreRunE(status, []string{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not in a scion project")
}

// resetConfigSetFlags restores config set's --global flag after a real
// rootCmd execution, since pflag keeps values across Execute calls.
func resetConfigSetFlags(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		configGlobal = false
		if f := configSetCmd.Flags().Lookup("global"); f != nil {
			f.Changed = false
		}
		if f := rootCmd.PersistentFlags().Lookup("global"); f != nil {
			f.Changed = false
		}
		rootCmd.SetArgs(nil)
	})
}

// TestConfigSetGlobalOutsideProject covers ptone/scion#3317: config set's own
// --global flag shadows the root --global, so `scion config set --global k v`
// (the next step `scion init --machine` prints) failed outside a project.
// Every placement of --global must now write the global settings file in a
// fresh HOME outside any project.
func TestConfigSetGlobalOutsideProject(t *testing.T) {
	cases := map[string][]string{
		"subcommand flag":  {"config", "set", "--global", "image_registry", "registry.example.test/team"},
		"root and sub":     {"--global", "config", "set", "--global", "image_registry", "registry.example.test/team"},
		"root placement":   {"--global", "config", "set", "image_registry", "registry.example.test/team"},
		"flag after value": {"config", "set", "image_registry", "registry.example.test/team", "--global"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			setupNoProjectPreRun(t)
			restoreAllSilenceUsage(t)
			resetConfigSetFlags(t)

			rootCmd.SetArgs(args)
			require.NoError(t, rootCmd.Execute())

			home := os.Getenv("HOME")
			data, err := os.ReadFile(filepath.Join(home, ".scion", "settings.yaml"))
			require.NoError(t, err)
			assert.Contains(t, string(data), "registry.example.test/team")
		})
	}
}

// TestConfigSetExistingGlobalForms pins that forms that already worked
// before the #3317 fix keep their behavior: -g global config set (with
// and without the subcommand's --global) writes the global file, and
// config set --global inside a project writes the global file and leaves
// the project's settings untouched.
func TestConfigSetExistingGlobalForms(t *testing.T) {
	for name, args := range map[string][]string{
		"-g global":          {"-g", "global", "config", "set", "image_registry", "registry.example.test/team"},
		"-g global --global": {"-g", "global", "config", "set", "--global", "image_registry", "registry.example.test/team"},
	} {
		t.Run(name, func(t *testing.T) {
			setupNoProjectPreRun(t)
			restoreAllSilenceUsage(t)
			resetConfigSetFlags(t)

			rootCmd.SetArgs(args)
			require.NoError(t, rootCmd.Execute())

			data, err := os.ReadFile(filepath.Join(os.Getenv("HOME"), ".scion", "settings.yaml"))
			require.NoError(t, err)
			assert.Contains(t, string(data), "registry.example.test/team")
		})
	}

	t.Run("--global inside a project", func(t *testing.T) {
		setupNoProjectPreRun(t)
		restoreAllSilenceUsage(t)
		resetConfigSetFlags(t)

		wd, err := os.Getwd()
		require.NoError(t, err)
		projectDir := filepath.Join(wd, ".scion")
		require.NoError(t, os.MkdirAll(projectDir, 0o755))
		projectSettings := filepath.Join(projectDir, "settings.yaml")
		const projectContent = "schema_version: \"1\"\n"
		require.NoError(t, os.WriteFile(projectSettings, []byte(projectContent), 0o644))

		rootCmd.SetArgs([]string{"config", "set", "--global", "image_registry", "registry.example.test/team"})
		require.NoError(t, rootCmd.Execute())

		data, err := os.ReadFile(filepath.Join(os.Getenv("HOME"), ".scion", "settings.yaml"))
		require.NoError(t, err)
		assert.Contains(t, string(data), "registry.example.test/team")

		got, err := os.ReadFile(projectSettings)
		require.NoError(t, err)
		assert.Equal(t, projectContent, string(got), "project settings must be untouched")
	})
}

// TestConfigSetLocalOutsideProjectStillFails pins that only --global lifts
// the gate for config set: a project-level set outside a project still fails.
func TestConfigSetLocalOutsideProjectStillFails(t *testing.T) {
	setupNoProjectPreRun(t)
	restoreAllSilenceUsage(t)
	resetConfigSetFlags(t)

	rootCmd.SetArgs([]string{"config", "set", "image_registry", "registry.example.test/team"})
	err := rootCmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not in a scion project")
}

// setupProjectWithoutRegistry is setupNoProjectPreRun plus a project in the
// working directory, no hub selected and no image_registry configured.
func setupProjectWithoutRegistry(t *testing.T) (home string) {
	t.Helper()
	setupNoProjectPreRun(t)
	origHubEndpoint := hubEndpoint
	t.Cleanup(func() { hubEndpoint = origHubEndpoint })
	hubEndpoint = ""
	for _, k := range []string{"SCION_HUB_ENDPOINT", "SCION_HUB_URL", "SCION_HUB_TOKEN", "SCION_HUB_API_KEY", "SCION_HUB_ENABLED", "SCION_PROJECT_ID", "SCION_IMAGE_REGISTRY"} {
		unsetEnvForTest(t, k)
	}

	wd, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(wd, ".scion"), 0o755))
	home = os.Getenv("HOME")
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".scion"), 0o755))
	return home
}

// unsetEnvForTest unsets key for the test and restores it afterwards. An
// empty value is not enough here: the settings loaders read SCION_* vars.
func unsetEnvForTest(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "") // registers the restore
	require.NoError(t, os.Unsetenv(key))
}

func writeGateGlobalSettings(t *testing.T, home, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(home, ".scion", "settings.yaml"), []byte(content), 0o644))
}

// TestRootPreRunDoesNotRequireImageRegistry covers ptone/scion#3318. The
// root pre-run used to require image_registry for project commands unless
// the work went to a hub. That check was removed with local mode: the CLI
// never starts containers itself, and the broker and server keep their own
// registry checks. No variant of hub selection, including none at all,
// needs a registry before the command runs.
func TestRootPreRunDoesNotRequireImageRegistry(t *testing.T) {
	newListCmd := func() *cobra.Command { return &cobra.Command{Use: "list"} }

	cases := []struct {
		name  string
		setup func(t *testing.T, home string)
	}{
		{"no hub endpoint", func(t *testing.T, home string) {}},
		{"endpoint from --hub flag", func(t *testing.T, home string) {
			writeGateGlobalSettings(t, home, "schema_version: \"1\"\nhub:\n  enabled: true\n")
			hubEndpoint = "https://hub.example.test"
		}},
		{"endpoint from settings", func(t *testing.T, home string) {
			writeGateGlobalSettings(t, home, "schema_version: \"1\"\nhub:\n  enabled: true\n  endpoint: https://hub.example.test\n")
		}},
		{"the current hub", func(t *testing.T, home string) {
			saveTestContext(t, "https://hub.example.test", "")
		}},
		{"endpoint from environment", func(t *testing.T, home string) {
			t.Setenv("SCION_HUB_ENDPOINT", "https://hub.example.test")
		}},
		{"hub project slug for --project", func(t *testing.T, home string) {
			projectPath = "my-hub-project"
		}},
		{"--hub flag alone", func(t *testing.T, home string) {
			hubEndpoint = "https://hub.example.test"
		}},
		{"stale hub.enabled=false with a current hub", func(t *testing.T, home string) {
			writeGateGlobalSettings(t, home, "schema_version: \"1\"\nhub:\n  enabled: false\n")
			saveTestContext(t, "https://hub.example.test", "")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := setupProjectWithoutRegistry(t)
			tc.setup(t, home)
			assert.NoError(t, rootCmd.PersistentPreRunE(newListCmd(), []string{}))
		})
	}

	// With no hub and no registry, 'scion list' reaches the hub check and
	// reports that no hub is selected, not a missing registry.
	t.Run("list with no hub and no registry", func(t *testing.T) {
		setupProjectWithoutRegistry(t)
		restoreAllSilenceUsage(t)
		rootCmd.SetArgs([]string{"list"})
		t.Cleanup(func() { rootCmd.SetArgs(nil) })
		err := rootCmd.Execute()
		require.Error(t, err)
		assert.ErrorIs(t, err, errHubNotConfigured)
		assert.Contains(t, err.Error(), "No hub selected.")
		assert.NotContains(t, err.Error(), "image_registry")
	})
}
