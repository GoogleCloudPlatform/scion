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
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUnknownSubcommandHelpError: "<group> <unknown> --help" is an unknown
// command for every group that rejects positional arguments, and every
// other help path is untouched (ptone/scion#4579).
func TestUnknownSubcommandHelpError(t *testing.T) {
	for _, args := range [][]string{
		{"artifact", "nosuch", "--help"},
		{"artifact", "nosuch", "-h"},
		{"artifact", "--help", "nosuch"},
		{"hub", "nosuch", "--help"},
		{"config", "nosuch", "--help"},
		{"artifact", "--format", "json", "nosuch", "--help"},
		{"--project", "x", "artifact", "nosuch", "-h"},
		{"artifact", "--yes", "nosuch", "--help"},
	} {
		err := unknownSubcommandHelpError(rootCmd, args)
		if assert.Error(t, err, "%q", args) {
			assert.Contains(t, err.Error(), `unknown command "nosuch" for "scion `, "%q", args)
		}
	}
	for _, args := range [][]string{
		{"--help"},
		{"-h"},
		{},
		{"artifact"},
		{"artifact", "--help"},
		{"artifact", "publish", "--help"},
		{"artifact", "versions", "-h"},
		{"artifact", "help"},
		{"artifact", "help", "--help"},
		{"help", "artifact"},
		{"help", "artifact", "nosuch"},
		{"hub", "--help"},
		{"config", "-h"},
		{"artifact", "nosuch"},
		{"artifact", "nosuch", "--", "--help"},
		{"nosuch", "--help"},
		{"artifact", "--format", "json", "--help"},
		{"--project", "x", "artifact", "--help"},
		{"-g", "x", "artifact", "-h"},
		{"artifact", "--format=json", "--help"},
		{"artifact", "--debug", "--help"},
		{"hub", "--hub", "https://hub.example", "--help"},
		{"artifact", "list", "--format", "json", "--help"},
	} {
		assert.NoError(t, unknownSubcommandHelpError(rootCmd, args), "%q", args)
	}
}

// TestUnknownSubcommandHelpErrorScope: only a Runnable group whose own
// Args validator rejects the leftover arguments is affected; a group that
// accepts positional arguments, and a group that is not Runnable, keep
// cobra's behaviour; a subcommand the agent mode removed is unknown.
func TestUnknownSubcommandHelpErrorScope(t *testing.T) {
	root := &cobra.Command{Use: "scion"}
	takesArg := &cobra.Command{Use: "send", Args: cobra.MaximumNArgs(1), Run: func(*cobra.Command, []string) {}}
	takesArg.AddCommand(&cobra.Command{Use: "list", Run: func(*cobra.Command, []string) {}})
	plain := &cobra.Command{Use: "plain"}
	plain.AddCommand(&cobra.Command{Use: "show", Run: func(*cobra.Command, []string) {}})
	art := &cobra.Command{Use: "artifact", Args: artifactCmd.Args, Run: artifactCmd.Run}
	for _, v := range []string{"share", "get"} {
		art.AddCommand(&cobra.Command{Use: v, Run: func(*cobra.Command, []string) {}})
	}
	root.AddCommand(takesArg, plain, art)

	assert.NoError(t, unknownSubcommandHelpError(root, []string{"send", "bob", "--help"}), "a group that accepts the argument is untouched")
	assert.NoError(t, unknownSubcommandHelpError(root, []string{"plain", "nosuch", "--help"}), "a non-Runnable group is untouched")
	assert.NoError(t, unknownSubcommandHelpError(root, []string{"artifact", "share", "--help"}), "share exists in human mode")

	applyModeRestrictions(root, ModeAgent)
	err := unknownSubcommandHelpError(root, []string{"artifact", "share", "--help"})
	require.Error(t, err, "share is removed in agent mode")
	assert.Contains(t, err.Error(), `unknown command "share" for "scion artifact"`)

	// The groups' validator answers pflag.ErrHelp for "help", which is
	// left alone.
	assert.ErrorIs(t, artifactCmd.Args(artifactCmd, []string{"help"}), pflag.ErrHelp)
}

// TestUnknownSubcommandHelpCLI runs the CLI in a child process:
// "<group> <unknown> --help" exits non-zero with "unknown command", and the
// existing help paths still print help and exit 0. It covers the call in
// Execute, which the tests above do not.
func TestUnknownSubcommandHelpCLI(t *testing.T) {
	dir, home := t.TempDir(), t.TempDir()
	for _, args := range [][]string{
		{"artifact", "nosuch", "--help"},
		{"hub", "nosuch", "--help"},
		{"config", "nosuch", "--help"},
	} {
		res := runSSHCLI(t, dir, home, nil, args...)
		assert.NotEqual(t, 0, res.code, "%q: %s", args, res.stdout)
		assert.Contains(t, res.stderr, `unknown command "nosuch" for "scion `+args[0]+`"`, "%q", args)
	}
	for _, args := range [][]string{
		{"--help"},
		{"artifact", "--help"},
		{"artifact", "publish", "--help"},
		{"help", "artifact"},
	} {
		res := runSSHCLI(t, dir, home, nil, args...)
		assert.Equal(t, 0, res.code, "%q: %s", args, res.stderr)
		assert.Contains(t, res.stdout, "Usage:", "%q", args)
	}
	// The agent-mode case (a subcommand the mode removed) is covered by
	// TestUnknownSubcommandHelpErrorScope: the child process clears every
	// inherited SCION_ variable, SCION_CLI_MODE included.
}

// TestPositionalArgs: flags and their values are removed the way cobra's
// stripFlags removes them: a flag that takes a value consumes the next
// argument, a boolean does not, "--name=value" is one argument, and "--"
// ends the flags. --help and -h are booleans.
func TestPositionalArgs(t *testing.T) {
	cmd := &cobra.Command{Use: "g"}
	cmd.Flags().StringP("name", "n", "", "")
	cmd.Flags().BoolP("all", "a", false, "")
	for _, tc := range []struct {
		args []string
		want []string
	}{
		{[]string{"--name", "v", "x"}, []string{"x"}},
		{[]string{"-n", "v", "x"}, []string{"x"}},
		{[]string{"--name=v", "x"}, []string{"x"}},
		{[]string{"--all", "x"}, []string{"x"}},
		{[]string{"-a", "x"}, []string{"x"}},
		{[]string{"--help", "x"}, []string{"x"}},
		{[]string{"-h", "x"}, []string{"x"}},
		{[]string{"--unknown", "v", "x"}, []string{"x"}},
		{[]string{"x", "--", "y"}, []string{"x"}},
		{[]string{"--name"}, nil},
		{[]string{"-", "x"}, []string{"x"}},
	} {
		assert.Equal(t, tc.want, positionalArgs(cmd, tc.args), "%q", tc.args)
	}
}
