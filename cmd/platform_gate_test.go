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
	"errors"
	"fmt"
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

// TestWindowsGate_ClearsPersistentHooksOnGatedCommands: cobra runs only
// the nearest persistent pre-run hook, so a gated command (or a gated
// parent, such as project above project use) that kept one would replace
// root's hook, which returns the refusal. Every command gets all four
// persistent hooks here; after gating, no gated command has any, and every
// allowed command keeps all four (control).
func TestWindowsGate_ClearsPersistentHooksOnGatedCommands(t *testing.T) {
	root, _ := gateTestTree(t)
	var set func(cmd *cobra.Command)
	set = func(cmd *cobra.Command) {
		for _, child := range cmd.Commands() {
			child.PersistentPreRun = func(*cobra.Command, []string) {}
			child.PersistentPreRunE = func(*cobra.Command, []string) error { return nil }
			child.PersistentPostRun = func(*cobra.Command, []string) {}
			child.PersistentPostRunE = func(*cobra.Command, []string) error { return nil }
			set(child)
		}
	}
	set(root)

	applyPlatformRestrictions(root, "windows")

	project := resolveCommandPath(root, "project")
	require.NotNil(t, project)
	require.False(t, windowsAllowed["project"], "control: project is a gated parent")
	require.True(t, windowsAllowed["project.use"], "control: project use is an allowed child")
	gated := 0
	var walk func(cmd *cobra.Command, prefix string)
	walk = func(cmd *cobra.Command, prefix string) {
		for _, child := range cmd.Commands() {
			path := child.Name()
			if prefix != "" {
				path = prefix + "." + path
			}
			hooks := []bool{child.PersistentPreRun != nil, child.PersistentPreRunE != nil, child.PersistentPostRun != nil, child.PersistentPostRunE != nil}
			if windowsAllowed[path] {
				assert.Equal(t, []bool{true, true, true, true}, hooks, "%s is allowed and must keep its persistent hooks", path)
			} else {
				gated++
				assert.Equal(t, []bool{false, false, false, false}, hooks, "%s is gated and must have no persistent hooks", path)
			}
			walk(child, path)
		}
	}
	walk(root, "")
	assert.NotZero(t, gated, "control: the tree has gated commands")
}

// TestWindowsGate_RefusesDespitePersistentHook: a gated command given its
// own persistent hooks still gets the refusal from root's hook, and none of
// its hooks run. Controls: an allowed command keeps and runs its hooks, and
// project use, an allowed child of the gated project, runs with root's hook
// while project's own hooks (cleared) do not run.
func TestWindowsGate_RefusesDespitePersistentHook(t *testing.T) {
	for _, tc := range []struct {
		name      string
		args      []string
		wantRan   []string
		wantHooks []string
		refused   string
	}{
		{"gated command", []string{"server", "start"}, nil, nil, "server start"},
		{"gated parent", []string{"project"}, nil, nil, "project"},
		{"allowed command", []string{"version"}, []string{"version"}, []string{"version pre", "version post"}, ""},
		{"allowed child of a gated parent", []string{"project", "use", "foo"}, []string{"project.use"}, nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, ran := gateTestTree(t)
			rootHook := 0
			// As the real root's PersistentPreRunE does.
			root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
				rootHook++
				return windowsRefusal(cmd)
			}
			var hooks []string
			for _, path := range []string{"server.start", "project", "version"} {
				c := resolveCommandPath(root, path)
				require.NotNil(t, c, path)
				name := strings.ReplaceAll(path, ".", " ")
				c.PersistentPreRunE = func(*cobra.Command, []string) error {
					hooks = append(hooks, name+" pre")
					return nil
				}
				c.PersistentPostRunE = func(*cobra.Command, []string) error {
					hooks = append(hooks, name+" post")
					return nil
				}
			}

			applyPlatformRestrictions(root, "windows")
			root.SetArgs(tc.args)
			err := root.Execute()

			if tc.refused != "" {
				require.Error(t, err)
				assert.True(t, errors.Is(err, errUnsupportedOnWindows), "got %v", err)
				assert.Contains(t, err.Error(), "scion "+tc.refused+" is not supported on Windows")
				assert.Equal(t, 1, rootHook, "root's hook must run and refuse")
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.wantRan, emptyToNil(*ran))
			assert.Equal(t, tc.wantHooks, hooks)
		})
	}
}

// emptyToNil returns nil for an empty slice, so a recorder that was never
// appended to compares equal to a nil expectation.
func emptyToNil(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}

// completionProbes gives each named command a ValidArgsFunction and a
// --probe flag with a registered completion function, each recording its
// call in *called and returning one candidate.
func completionProbes(t *testing.T, root *cobra.Command, called *[]string, paths ...string) {
	t.Helper()
	for _, path := range paths {
		c := resolveCommandPath(root, path)
		require.NotNil(t, c, path)
		name := strings.ReplaceAll(path, ".", " ")
		c.ValidArgsFunction = func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
			*called = append(*called, name+" args")
			return []string{"arg-candidate"}, cobra.ShellCompDirectiveNoFileComp
		}
		if c.Flags().Lookup("probe") == nil {
			c.Flags().String("probe", "", "")
		}
		require.NoError(t, c.RegisterFlagCompletionFunc("probe", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
			*called = append(*called, name+" flag")
			return []string{"flag-candidate"}, cobra.ShellCompDirectiveNoFileComp
		}))
	}
}

// TestWindowsGate_CompletionSkipsGatedCommands: on the Windows form, a
// shell completion request (cobra's __complete / __completeNoDesc) for a
// gated command, or under a gated parent, is answered with only the
// NoFileComp directive, and neither the command's ValidArgsFunction nor
// its flag completion function runs. The probes are installed after
// gating, so the answer does not rest on the gate having cleared
// ValidArgsFunction. Controls: allowed commands, including project use
// under the gated project, and the same request off Windows, go through
// cobra and call their completion functions.
func TestWindowsGate_CompletionSkipsGatedCommands(t *testing.T) {
	probed := []string{"server.start", "project", "hub.status", "project.use"}
	for _, args := range [][]string{
		{cobra.ShellCompRequestCmd, "server", "start", ""},
		{cobra.ShellCompRequestCmd, "server", "start", "--probe", ""},
		{cobra.ShellCompRequestCmd, "server", "start", "--probe="},
		{cobra.ShellCompNoDescRequestCmd, "server", "start", "x", ""},
		// Intended: under the gated project nothing is completed, so
		// "scion project <TAB>" does not offer the allowed "use" on Windows.
		{cobra.ShellCompRequestCmd, "project", ""},
		{cobra.ShellCompRequestCmd, "project", "--probe", ""},
		// Global flags before __complete: cobra still treats the request
		// as a completion request, so the gate must too.
		{"--debug", cobra.ShellCompRequestCmd, "server", "start", "--probe", ""},
		{"--project", "x", cobra.ShellCompRequestCmd, "server", "start", ""},
		{"-g", "x", cobra.ShellCompRequestCmd, "server", "start", ""},
		{"--project=x", cobra.ShellCompRequestCmd, "server", "start", ""},
		{"--debug", cobra.ShellCompNoDescRequestCmd, "server", "start", "--probe", ""},
	} {
		t.Run("gated "+strings.Join(args, " "), func(t *testing.T) {
			root, _ := gateTestTree(t)
			applyPlatformRestrictions(root, "windows")
			var called []string
			completionProbes(t, root, &called, probed...)
			var out bytes.Buffer

			require.True(t, completeGated(root, args, &out), "a request for a gated command must be answered by the gate")
			assert.Equal(t, fmt.Sprintf(":%d\n", cobra.ShellCompDirectiveNoFileComp), out.String(), "only the NoFileComp directive")
			assert.Empty(t, called, "no completion function of a gated command may run")
			assertNoCompletionPlaceholder(t, root)

			// Control: the same request handed to cobra directly does call
			// the gated command's completion function.
			root.SetArgs(args)
			require.NoError(t, root.Execute())
			assert.NotEmpty(t, called, "control: cobra alone runs the gated command's completion function")
		})
	}

	for _, tc := range []struct {
		goos string
		args []string
		want string
	}{
		{"windows", []string{cobra.ShellCompRequestCmd, "hub", "status", ""}, "hub status args"},
		{"windows", []string{cobra.ShellCompRequestCmd, "hub", "status", "--probe", ""}, "hub status flag"},
		{"windows", []string{cobra.ShellCompRequestCmd, "project", "use", ""}, "project use args"},
		{"windows", []string{cobra.ShellCompRequestCmd, "project", "use", "--probe", ""}, "project use flag"},
		{"windows", []string{"--debug", cobra.ShellCompRequestCmd, "hub", "status", "--probe", ""}, "hub status flag"},
		{"linux", []string{cobra.ShellCompRequestCmd, "server", "start", ""}, "server start args"},
		{"linux", []string{cobra.ShellCompRequestCmd, "server", "start", "--probe", ""}, "server start flag"},
	} {
		t.Run("control "+tc.goos+" "+strings.Join(tc.args, " "), func(t *testing.T) {
			root, _ := gateTestTree(t)
			applyPlatformRestrictions(root, tc.goos)
			var called []string
			completionProbes(t, root, &called, probed...)
			var gateOut bytes.Buffer
			require.False(t, completeGated(root, tc.args, &gateOut), "the gate must leave this request to cobra")
			assert.Empty(t, gateOut.String())
			assertNoCompletionPlaceholder(t, root)

			var out bytes.Buffer
			root.SetOut(&out)
			root.SetArgs(tc.args)
			require.NoError(t, root.Execute())
			assert.Equal(t, []string{tc.want}, called)
			assert.Contains(t, out.String(), "-candidate")
		})
	}

	// A command named __complete already on root (cobra adds its own while
	// it executes): the request is still recognised by name, so a gated
	// target is still answered by the gate and its probe does not run.
	t.Run("gated with a __complete command already on root", func(t *testing.T) {
		root, _ := gateTestTree(t)
		applyPlatformRestrictions(root, "windows")
		var called []string
		completionProbes(t, root, &called, probed...)
		existing := &cobra.Command{Use: cobra.ShellCompRequestCmd, Hidden: true, DisableFlagParsing: true, Run: func(*cobra.Command, []string) {}}
		root.AddCommand(existing)
		var out bytes.Buffer
		require.True(t, completeGated(root, []string{cobra.ShellCompRequestCmd, "server", "start", "--probe", ""}, &out),
			"a completion request must be recognised by name, whichever __complete command Find returns")
		assert.Equal(t, fmt.Sprintf(":%d\n", cobra.ShellCompDirectiveNoFileComp), out.String())
		assert.Empty(t, called, "no completion function of a gated command may run")
		root.RemoveCommand(existing)
		assertNoCompletionPlaceholder(t, root)
	})

	// Not a completion request: never answered by the gate.
	root, _ := gateTestTree(t)
	applyPlatformRestrictions(root, "windows")
	var out bytes.Buffer
	assert.False(t, completeGated(root, []string{"server", "start"}, &out))
	assert.False(t, completeGated(root, []string{cobra.ShellCompRequestCmd}, &out))
	assert.Empty(t, out.String())
}

// assertNoCompletionPlaceholder checks that completeGated left no command
// named like cobra's completion request in the tree: cobra adds its own
// when it executes.
func assertNoCompletionPlaceholder(t *testing.T, root *cobra.Command) {
	t.Helper()
	for _, c := range root.Commands() {
		assert.NotEqual(t, cobra.ShellCompRequestCmd, c.Name(), "completeGated must remove its placeholder")
	}
}

// allowedAncestorHooks walks the tree under root and, for every command the
// Windows gate refuses (its path is not in windowsAllowed, as gateCommands
// decides), checks each ancestor below root that is allowed on Windows: an
// allowed ancestor keeps its persistent hooks, and cobra runs the nearest
// persistent pre-run hook only, so a hook there would run instead of root's
// refusing hook for the gated descendant. It returns the number of (gated
// command, allowed ancestor) pairs checked and one message per hook found.
func allowedAncestorHooks(root *cobra.Command) (pairs int, found []string) {
	var walk func(cmd *cobra.Command, prefix string, allowedAncestors []*cobra.Command, ancestorPaths []string)
	walk = func(cmd *cobra.Command, prefix string, allowedAncestors []*cobra.Command, ancestorPaths []string) {
		for _, child := range cmd.Commands() {
			path := child.Name()
			if prefix != "" {
				path = prefix + "." + path
			}
			if !windowsAllowed[path] {
				for i, a := range allowedAncestors {
					pairs++
					for _, h := range []struct {
						name string
						set  bool
					}{
						{"PersistentPreRun", a.PersistentPreRun != nil},
						{"PersistentPreRunE", a.PersistentPreRunE != nil},
						{"PersistentPostRun", a.PersistentPostRun != nil},
						{"PersistentPostRunE", a.PersistentPostRunE != nil},
					} {
						if h.set {
							found = append(found, fmt.Sprintf("%s (allowed on Windows) has %s, which would run instead of root's hook for the gated %s",
								strings.ReplaceAll(ancestorPaths[i], ".", " "), h.name, strings.ReplaceAll(path, ".", " ")))
						}
					}
				}
			}
			nextAllowed, nextPaths := allowedAncestors, ancestorPaths
			if windowsAllowed[path] {
				nextAllowed = append(append([]*cobra.Command(nil), allowedAncestors...), child)
				nextPaths = append(append([]string(nil), ancestorPaths...), path)
			}
			walk(child, path, nextAllowed, nextPaths)
		}
	}
	walk(root, "", nil, nil)
	return pairs, found
}

// TestWindowsGate_NoPersistentHooksOnAllowedAncestorsOfGatedCommands: on the
// real command tree, no command allowed on Windows that has a gated
// descendant defines a persistent hook (root excluded: its hook is the one
// that refuses). The gate clears hooks only on gated commands, so such a
// hook would replace root's refusal for the gated descendant. The control
// plants a PersistentPreRunE on hub (allowed, with gated descendants such
// as hub secret migrate) in a copied tree and shows the check reports it.
func TestWindowsGate_NoPersistentHooksOnAllowedAncestorsOfGatedCommands(t *testing.T) {
	pairs, found := allowedAncestorHooks(rootCmd)
	t.Logf("checked %d (gated command, allowed ancestor) pairs", pairs)
	assert.NotZero(t, pairs, "the real tree must have gated commands below allowed ancestors (otherwise the check is vacuous)")
	assert.Empty(t, found, "allowed ancestors of gated commands must have no persistent hooks")

	root, _ := gateTestTree(t)
	require.True(t, windowsAllowed["hub"], "control: hub is allowed")
	require.False(t, windowsAllowed["hub.secret.migrate"], "control: hub secret migrate is gated")
	hub := resolveCommandPath(root, "hub")
	require.NotNil(t, hub)
	require.NotNil(t, resolveCommandPath(root, "hub.secret.migrate"))
	_, before := allowedAncestorHooks(root)
	require.Empty(t, before, "control: the copied tree carries no hooks before the plant")
	hub.PersistentPreRunE = func(*cobra.Command, []string) error { return nil }

	cpairs, after := allowedAncestorHooks(root)
	assert.NotZero(t, cpairs)
	require.NotEmpty(t, after, "control: a hook planted on hub must be reported")
	joined := strings.Join(after, "\n")
	assert.Contains(t, joined, "hub (allowed on Windows) has PersistentPreRunE")
	assert.Contains(t, joined, "the gated hub secret migrate")
}

// windowsUngatePrerequisiteCommands are the commands that stay refused on
// Windows until their prerequisites hold: agent file removal fails closed on
// Windows and the pkg/runtime Windows tests run on the Windows job. Each
// entry is a command path as in windowsAllowed, with why it is held back.
var windowsUngatePrerequisiteCommands = map[string]string{
	// Local agent file removal: cleanupAfterHubDelete ->
	// agent.DeleteAgentFiles -> util.RemoveAllSafe and util.RemoveWorktree.
	// 'stop --rm' runs the same cleanup after the stop.
	"delete": "removes the agent's local files and worktree",
	"stop":   "--rm removes the agent's local files and worktree",
	// The co-located Runtime Broker runs in this process (or in the server
	// daemon this binary starts): runtimebroker -> AgentManager.Start ->
	// Runtime.Run (Docker, Podman, Apple) -> buildCommonRunArgs, which
	// registers the git metadata bind sources (bindChecked,
	// registerPrivateGitConfig, registerReadOnlyBaseGitMounts).
	"server.start":           "runs the co-located Runtime Broker (git bind sources)",
	"server.restart":         "restarts the server daemon with its Runtime Broker",
	"runtime-broker.start":   "runs the Runtime Broker (git bind sources)",
	"runtime-broker.restart": "restarts the Runtime Broker",
	// Agent launches: the Hub dispatches them to a Runtime Broker, which
	// runs the AgentManager.Start path above; on a workstation that broker
	// is the co-located one.
	"start":  "launches an agent through a Runtime Broker (git bind sources)",
	"resume": "relaunches an agent through a Runtime Broker (git bind sources)",
}

// TestWindowsGate_RefusesUngatePrerequisiteCommands checks that delete,
// stop (with and without --rm) and every command that reaches the pkg/runtime
// git bind-source code stay refused on Windows: none is in windowsAllowed,
// each is marked for root's hook, and running it returns the refusal without
// running the command. Aliases and flag positions are covered too.
func TestWindowsGate_RefusesUngatePrerequisiteCommands(t *testing.T) {
	// The commands and the --rm flag exist, so the refusals below are not
	// vacuous.
	for path := range windowsUngatePrerequisiteCommands {
		require.NotNil(t, resolveCommandPath(rootCmd, path), "%s must be a real command", path)
		assert.False(t, windowsAllowed[path], "%s must stay out of windowsAllowed: %s", path, windowsUngatePrerequisiteCommands[path])
	}
	require.NotNil(t, resolveCommandPath(rootCmd, "stop").Flags().Lookup("rm"), "stop must define --rm")

	t.Run("marked", func(t *testing.T) {
		root, _ := gateTestTree(t)
		applyPlatformRestrictions(root, "windows")
		for path := range windowsUngatePrerequisiteCommands {
			assert.Error(t, windowsRefusal(resolveCommandPath(root, path)), "%s must be refused by root's hook", path)
		}
	})

	for _, tc := range []struct {
		args []string
		path string
	}{
		{[]string{"delete", "agent-x"}, "delete"},
		{[]string{"rm", "agent-x"}, "delete"}, // via the alias
		{[]string{"delete", "agent-x", "agent-y", "--no-such-flag"}, "delete"},
		{[]string{"stop", "agent-x"}, "stop"},
		{[]string{"stop", "--rm", "agent-x"}, "stop"},
		{[]string{"stop", "agent-x", "--rm"}, "stop"},
		{[]string{"stop", "--rm=true", "agent-x"}, "stop"},
		{[]string{"server", "start"}, "server start"},
		{[]string{"server", "start", "--foreground"}, "server start"},
		{[]string{"server", "restart"}, "server restart"},
		{[]string{"runtime-broker", "start"}, "runtime-broker start"},
		{[]string{"broker", "start", "--foreground"}, "runtime-broker start"}, // via the alias
		{[]string{"runtime-broker", "restart"}, "runtime-broker restart"},
		{[]string{"start", "agent-x"}, "start"},
		{[]string{"start", "agent-x", "do", "the", "task"}, "start"},
		{[]string{"run", "agent-x"}, "start"}, // via the alias
		{[]string{"resume", "agent-x"}, "resume"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
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
}
