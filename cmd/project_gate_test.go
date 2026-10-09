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
		brokerRestartCmd, brokerStatusCmd, brokerHubsCmd,
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
// working directory and noHub cleared, so a registry-checked command reaches
// the image_registry check with no registry configured.
func setupProjectWithoutRegistry(t *testing.T) (home string) {
	t.Helper()
	setupNoProjectPreRun(t)
	noHub = false
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

// TestRegistryCheckSkippedForHubDispatch covers ptone/scion#3318: the
// image_registry check is skipped for hub-dispatched work whether the hub
// endpoint comes from --hub, settings or the environment.
func TestRegistryCheckSkippedForHubDispatch(t *testing.T) {
	newListCmd := func() *cobra.Command { return &cobra.Command{Use: "list"} }

	t.Run("baseline: local work needs a registry", func(t *testing.T) {
		setupProjectWithoutRegistry(t)
		err := rootCmd.PersistentPreRunE(newListCmd(), []string{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "image_registry is not configured")
	})

	t.Run("endpoint from --hub flag", func(t *testing.T) {
		home := setupProjectWithoutRegistry(t)
		writeGateGlobalSettings(t, home, "schema_version: \"1\"\nhub:\n  enabled: true\n")
		hubEndpoint = "https://hub.example.test"
		assert.NoError(t, rootCmd.PersistentPreRunE(newListCmd(), []string{}))
	})

	t.Run("endpoint from settings", func(t *testing.T) {
		home := setupProjectWithoutRegistry(t)
		writeGateGlobalSettings(t, home, "schema_version: \"1\"\nhub:\n  enabled: true\n  endpoint: https://hub.example.test\n")
		assert.NoError(t, rootCmd.PersistentPreRunE(newListCmd(), []string{}))
	})

	t.Run("endpoint from environment", func(t *testing.T) {
		setupProjectWithoutRegistry(t)
		t.Setenv("SCION_HUB_ENDPOINT", "https://hub.example.test")
		assert.NoError(t, rootCmd.PersistentPreRunE(newListCmd(), []string{}))
	})

	t.Run("hub project slug for --project", func(t *testing.T) {
		setupProjectWithoutRegistry(t)
		projectPath = "my-hub-project"
		assert.NoError(t, rootCmd.PersistentPreRunE(newListCmd(), []string{}))
	})

	t.Run("--hub flag without hub enabled is still local", func(t *testing.T) {
		setupProjectWithoutRegistry(t)
		hubEndpoint = "https://hub.example.test"
		err := rootCmd.PersistentPreRunE(newListCmd(), []string{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "image_registry is not configured")
	})

	t.Run("--no-hub keeps the check", func(t *testing.T) {
		home := setupProjectWithoutRegistry(t)
		writeGateGlobalSettings(t, home, "schema_version: \"1\"\nhub:\n  enabled: true\n  endpoint: https://hub.example.test\n")
		noHub = true
		err := rootCmd.PersistentPreRunE(newListCmd(), []string{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "image_registry is not configured")
	})
}
