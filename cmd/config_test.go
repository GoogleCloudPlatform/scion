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
	"testing"

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
