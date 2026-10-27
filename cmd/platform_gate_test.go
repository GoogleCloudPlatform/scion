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
	"errors"
	"io"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gateTestTree copies the real command tree into a fresh tree: names,
// aliases, and each command's own flags with their annotations (so
// required flags and flag groups behave as in the real binary). Every
// command records that it ran. The gate can then be applied and executed
// without touching the shared rootCmd: flags are copied, not shared, so
// stripping annotations on the copy leaves the real flags alone. The root
// gets cobra's default help and completion commands, which the real root
// only adds when it executes.
func gateTestTree(t *testing.T) (root *cobra.Command, ran *[]string) {
	t.Helper()
	ran = &[]string{}
	var clone func(src *cobra.Command, path string) *cobra.Command
	clone = func(src *cobra.Command, path string) *cobra.Command {
		c := &cobra.Command{
			Use:     src.Name(),
			Aliases: append([]string(nil), src.Aliases...),
			Run:     func(*cobra.Command, []string) { *ran = append(*ran, path) },
		}
		copyFlags(c.Flags(), src.LocalNonPersistentFlags())
		copyFlags(c.PersistentFlags(), src.PersistentFlags())
		for _, child := range src.Commands() {
			childPath := child.Name()
			if path != "" {
				childPath = path + "." + childPath
			}
			c.AddCommand(clone(child, childPath))
		}
		return c
	}
	root = clone(rootCmd, "")
	root.SilenceErrors, root.SilenceUsage = true, true
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()
	return root, ran
}

// copyFlags adds a copy of every flag in src to dst. The copy shares the
// flag's value (the bound variable) but has its own annotations map.
func copyFlags(dst, src *pflag.FlagSet) {
	src.VisitAll(func(f *pflag.Flag) {
		if dst.Lookup(f.Name) != nil {
			return
		}
		nf := *f
		nf.Annotations = make(map[string][]string, len(f.Annotations))
		for k, v := range f.Annotations {
			nf.Annotations[k] = append([]string(nil), v...)
		}
		dst.AddFlag(&nf)
	})
}

// gateState is what the gate changes on one command.
type gateState struct {
	hidden, hasRun, hasRunE, marked bool
}

func gateStates(root *cobra.Command) map[string]gateState {
	states := map[string]gateState{}
	var walk func(cmd *cobra.Command, prefix string)
	walk = func(cmd *cobra.Command, prefix string) {
		for _, child := range cmd.Commands() {
			path := child.Name()
			if prefix != "" {
				path = prefix + "." + path
			}
			_, marked := child.Annotations[windowsGatedAnnotation]
			states[path] = gateState{hidden: child.Hidden, hasRun: child.Run != nil, hasRunE: child.RunE != nil, marked: marked}
			walk(child, path)
		}
	}
	walk(root, "")
	return states
}

func TestWindowsGate_RefusesServerAndBroker(t *testing.T) {
	for _, tc := range []struct {
		args []string
		path string
	}{
		{[]string{"server", "start"}, "server start"},
		{[]string{"broker", "join"}, "runtime-broker join"}, // via the alias
		{[]string{"runtime-broker", "start"}, "runtime-broker start"},
		{[]string{"whoami"}, "whoami"},
		{[]string{"expose"}, "expose"},
		{[]string{"server"}, "server"},
		// Commands with required flags or flag groups, given none: the
		// refusal, not a required-flag error. The hub secret migrate tools
		// also write the local hub database.
		{[]string{"hub", "secret", "migrate"}, "hub secret migrate"},
		{[]string{"hub", "secret", "migrate-names"}, "hub secret migrate-names"},
		{[]string{"server", "migrate"}, "server migrate"},
		{[]string{"project", "visitors", "add"}, "project visitors add"},
		// Arguments and flags the real command would reject still get the
		// refusal, not a usage error.
		{[]string{"server", "start", "extra-arg", "--no-such-flag"}, "server start"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			root, ran := gateTestTree(t)
			applyPlatformRestrictions(root, "windows")
			root.SetArgs(tc.args)

			err := root.Execute()

			require.Error(t, err)
			assert.True(t, errors.Is(err, errUnsupportedOnWindows), "got %v", err)
			assert.Equal(t, "scion "+tc.path+" is not supported on Windows; the Windows build supports hub client commands only (see docs: Windows)", err.Error())
			assert.Empty(t, *ran, "a gated command must not run")
		})
	}

	// Positive controls: allowlisted commands still run on Windows.
	for _, tc := range []struct {
		args []string
		path string
	}{
		{[]string{"version"}, "version"},
		{[]string{"hub", "status"}, "hub.status"},
		{[]string{"tunnel", "status"}, "tunnel.status"},
	} {
		t.Run("runs "+tc.path, func(t *testing.T) {
			root, ran := gateTestTree(t)
			applyPlatformRestrictions(root, "windows")
			root.SetArgs(tc.args)

			require.NoError(t, root.Execute())
			assert.Equal(t, []string{tc.path}, *ran)
		})
	}
}

// TestWindowsGate_AllowsUseOnlyUnderProject: 'scion use' and 'scion project
// use' (with the parent's aliases) run on Windows,
// while the parent 'project' stays gated, and so do its other subcommands.
// The refusals are the control that the gate is applied.
func TestWindowsGate_AllowsUseOnlyUnderProject(t *testing.T) {
	for _, tc := range []struct {
		args []string
		path string
	}{
		{[]string{"use"}, "use"},
		{[]string{"use", "foo"}, "use"},
		{[]string{"project", "use", "foo"}, "project.use"},
		{[]string{"projects", "use", "foo"}, "project.use"},
		{[]string{"group", "use"}, "project.use"},
	} {
		t.Run("runs "+strings.Join(tc.args, " "), func(t *testing.T) {
			root, ran := gateTestTree(t)
			applyPlatformRestrictions(root, "windows")
			root.SetArgs(tc.args)

			require.NoError(t, root.Execute())
			assert.Equal(t, []string{tc.path}, *ran)
		})
	}
	for _, tc := range []struct {
		args []string
		path string
	}{
		{[]string{"project"}, "project"},
		{[]string{"project", "init"}, "project init"},
		{[]string{"project", "list"}, "project list"},
		{[]string{"projects", "list"}, "project list"},
	} {
		t.Run("refuses "+tc.path, func(t *testing.T) {
			root, ran := gateTestTree(t)
			applyPlatformRestrictions(root, "windows")
			root.SetArgs(tc.args)

			err := root.Execute()
			require.Error(t, err)
			assert.True(t, errors.Is(err, errUnsupportedOnWindows), "got %v", err)
			assert.Contains(t, err.Error(), "scion "+tc.path+" is not supported on Windows")
			assert.Empty(t, *ran, "a gated command must not run")
		})
	}
	root, _ := gateTestTree(t)
	applyPlatformRestrictions(root, "windows")
	for _, path := range []string{"use", "project.use"} {
		c := resolveCommandPath(root, path)
		require.NotNil(t, c, path)
		assert.NoError(t, windowsRefusal(c), "%s is allowed: root's hook must not refuse it", path)
		assert.False(t, c.Hidden, "%s is allowed and must stay visible", path)
	}
}

// TestWindowsGate_DropsFlagValidationOnGatedCommands: no gated command
// may fail cobra's required-flag or flag-group checks, which run before
// RunE and would show a usage error instead of the refusal. The control
// shows the copied tree does carry those rules before gating.
func TestWindowsGate_DropsFlagValidationOnGatedCommands(t *testing.T) {
	root, _ := gateTestTree(t)
	migrate := resolveCommandPath(root, "hub.secret.migrate")
	require.NotNil(t, migrate)
	require.Error(t, migrate.ValidateRequiredFlags(), "control: hub secret migrate has required flags before gating")
	visitors := resolveCommandPath(root, "project.visitors.add")
	require.NotNil(t, visitors)
	require.Error(t, visitors.ValidateFlagGroups(), "control: project visitors add has a one-required group before gating")

	applyPlatformRestrictions(root, "windows")

	var walk func(cmd *cobra.Command, prefix string)
	walk = func(cmd *cobra.Command, prefix string) {
		for _, child := range cmd.Commands() {
			path := child.Name()
			if prefix != "" {
				path = prefix + "." + path
			}
			if !windowsAllowed[path] {
				assert.NoError(t, child.ValidateRequiredFlags(), "%s: gated, must have no required flags", path)
				assert.NoError(t, child.ValidateFlagGroups(), "%s: gated, must have no flag groups", path)
			}
			walk(child, path)
		}
	}
	walk(root, "")
	// The real tree keeps its rules: the copy's annotations were stripped.
	assert.Error(t, resolveCommandPath(rootCmd, "hub.secret.migrate").ValidateRequiredFlags())
}

// TestWindowsGate_RefusesBeforeProjectResolution: root's PersistentPreRunE
// returns the refusal for a gated command before the project requirement,
// so "scion start" outside a project says "not supported on Windows", not
// "not in a scion project". The control runs the same hook on the same
// command ungated and gets the project error.
func TestWindowsGate_RefusesBeforeProjectResolution(t *testing.T) {
	root, _ := gateTestTree(t)
	start := resolveCommandPath(root, "start")
	require.NotNil(t, start)

	setupNoProjectPreRun(t)
	restoreSilenceUsage(t, start)
	err := rootCmd.PersistentPreRunE(start, []string{"agent-x"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not in a scion project", "control: ungated start needs a project")

	applyPlatformRestrictions(root, "windows")
	setupNoProjectPreRun(t)
	err = rootCmd.PersistentPreRunE(start, []string{"agent-x"})
	require.Error(t, err)
	assert.True(t, errors.Is(err, errUnsupportedOnWindows), "got %v", err)
	assert.Equal(t, "scion start is not supported on Windows; the Windows build supports hub client commands only (see docs: Windows)", err.Error())

	// An allowed command is not refused by the hook.
	version := resolveCommandPath(root, "version")
	require.NotNil(t, version)
	assert.NoError(t, windowsRefusal(version))
}

func TestWindowsGate_HidesOnlyGatedCommands(t *testing.T) {
	root, _ := gateTestTree(t)
	before := gateStates(root)
	applyPlatformRestrictions(root, "windows")

	for path, st := range gateStates(root) {
		if windowsAllowed[path] {
			assert.Equal(t, before[path], st, "%s is allowed and must be left unchanged", path)
			assert.False(t, st.hidden, "%s is allowed and must stay visible", path)
			continue
		}
		assert.True(t, st.marked, "%s is gated and must be marked for root's hook", path)
		assert.True(t, st.hidden, "%s is gated and must be hidden", path)
		assert.False(t, st.hasRun, "%s is gated and must have no Run", path)
		assert.True(t, st.hasRunE, "%s is gated and must refuse through RunE", path)
	}
}

// TestWindowsGate_AllowlistEntriesExist keeps the allowlist free of stale
// entries: every key must name a real command.
func TestWindowsGate_AllowlistEntriesExist(t *testing.T) {
	root, _ := gateTestTree(t)
	var missing []string
	for path := range windowsAllowed {
		if resolveCommandPath(root, path) == nil {
			missing = append(missing, path)
		}
	}
	sort.Strings(missing)
	assert.Empty(t, missing, "windowsAllowed keys that are not real commands")
}

func TestWindowsGate_NoopOnUnix(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			root, ran := gateTestTree(t)
			before := gateStates(root)

			applyPlatformRestrictions(root, goos)

			assert.Equal(t, before, gateStates(root), "the tree must be unchanged on %s", goos)
			root.SetArgs([]string{"server", "start"})
			require.NoError(t, root.Execute())
			assert.Equal(t, []string{"server.start"}, *ran)
		})
	}
}
