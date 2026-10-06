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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestShouldShowUsageOnError covers the decision Execute makes about printing
// a command's usage block after a failed invocation. Regression test for
// ptone/scion#2089: Execute used to call cmd.Usage() unconditionally whenever
// autoHelp was enabled, ignoring cmd.SilenceUsage entirely — so a command
// opting into SilenceUsage (e.g. attach, once argument parsing has already
// succeeded) had no way to suppress usage on a runtime error.
//
// A parentless command with SilenceUsage set is also covered here (standing
// in for rootCmd, which sets SilenceUsage on itself only to stop cobra's own
// internal auto-print — not as an opt-out signal for this helper): it must
// still show usage, or root-level usage errors like an unknown command or an
// unknown global flag would silently lose their usage block. See
// TestExecuteUsageOnError_EndToEnd for the same case driven through the real
// rootCmd.ExecuteC() dispatch.
func TestShouldShowUsageOnError(t *testing.T) {
	tests := []struct {
		name         string
		cmd          *cobra.Command
		hasParent    bool
		autoHelp     bool
		silenceUsage bool
		want         bool
	}{
		{
			name:     "nil command never shows usage",
			cmd:      nil,
			autoHelp: true,
			want:     false,
		},
		{
			name:     "autoHelp disabled never shows usage",
			cmd:      &cobra.Command{Use: "other"},
			autoHelp: false,
			want:     false,
		},
		{
			name:     "a command that never opts in shows usage as before (no-op default)",
			cmd:      &cobra.Command{Use: "other"},
			autoHelp: true,
			want:     true,
		},
		{
			name:         "a subcommand that sets SilenceUsage suppresses usage",
			cmd:          &cobra.Command{Use: "attach"},
			hasParent:    true,
			autoHelp:     true,
			silenceUsage: true,
			want:         false,
		},
		{
			name:         "a parentless (root-like) command with SilenceUsage still shows usage",
			cmd:          &cobra.Command{Use: "scion"},
			hasParent:    false,
			autoHelp:     true,
			silenceUsage: true,
			want:         true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.cmd != nil {
				tt.cmd.SilenceUsage = tt.silenceUsage
				if tt.hasParent {
					parent := &cobra.Command{Use: "parent"}
					parent.AddCommand(tt.cmd)
				}
			}
			got := shouldShowUsageOnError(tt.cmd, tt.autoHelp)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestExecuteUsageOnError_EndToEnd drives the real cobra dispatch via
// rootCmd.ExecuteC() and feeds the result into shouldShowUsageOnError,
// exercising exactly the code path Execute() uses. This is the regression
// test for the root-command finding raised in review: the helper originally
// read cmd.SilenceUsage without checking whether cmd was the root command
// itself, so rootCmd's own SilenceUsage (set only to silence cobra's
// internal auto-print, see the doc comment on shouldShowUsageOnError) leaked
// into the decision and hid usage for genuine root-level usage errors — an
// unknown command or an unknown global flag.
func TestExecuteUsageOnError_EndToEnd(t *testing.T) {
	// attach's PersistentPreRunE requires project/registry context unless
	// Hub context is detected. Configure that once for the attach-scoped
	// subtests below so they reach cobra's flag/Args validation or RunE
	// instead of failing earlier on an unrelated "no project" or "no
	// image_registry" error.
	origNoHub := noHub
	origProjectPath := projectPath
	origSilenceUsage := attachCmd.SilenceUsage
	origNonInteractive := nonInteractive
	origAutoConfirm := autoConfirm
	origAutoHelp := autoHelp
	t.Cleanup(func() {
		noHub = origNoHub
		projectPath = origProjectPath
		attachCmd.SilenceUsage = origSilenceUsage
		// PersistentPreRunE mutates these (agent mode / cli.* settings) in
		// the agent-mode / interactive_disabled block; restore them so this
		// test doesn't leak state into whichever test runs next.
		nonInteractive = origNonInteractive
		autoConfirm = origAutoConfirm
		autoHelp = origAutoHelp
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
	})

	t.Setenv("SCION_HOST_UID", "")
	// A non-empty Hub endpoint makes config.IsHubContext() true, which is
	// what PersistentPreRunE uses to skip the image_registry requirement.
	// This doesn't affect the attach code path itself, which is gated
	// separately by the package-level noHub flag set below.
	t.Setenv("SCION_HUB_ENDPOINT", "https://hub.invalid.example")
	noHub = true
	projectPath = t.TempDir()

	run := func(t *testing.T, args []string) (*cobra.Command, error) {
		t.Helper()
		attachCmd.SilenceUsage = false
		var buf bytes.Buffer
		rootCmd.SetOut(&buf)
		rootCmd.SetErr(&buf)
		rootCmd.SetArgs(args)
		return rootCmd.ExecuteC()
	}

	tests := []struct {
		name string
		args []string
		want bool
	}{
		{"unknown root command still shows usage", []string{"totally-bogus-command-zzz"}, true},
		{"unknown root flag still shows usage", []string{"--totally-bogus-flag-zzz"}, true},
		{"attach with no args still shows usage", []string{"attach"}, true},
		{"attach with too many args still shows usage", []string{"attach", "a", "b"}, true},
		{"attach with an unknown flag still shows usage", []string{"attach", "--totally-bogus-flag-zzz", "x"}, true},
		{"attach runtime error hides usage", []string{"attach", "does-not-exist-xyz"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd, err := run(t, tt.args)
			require.Error(t, err)
			assert.Equal(t, tt.want, shouldShowUsageOnError(cmd, true))
			if tt.name == "attach runtime error hides usage" {
				// Make the intent explicit: this row's outcome depends on
				// cobra actually dispatching into attachCmd (and RunE running
				// far enough to hit the "not found" error), not on
				// PersistentPreRunE failing first for an unrelated reason.
				assert.Equal(t, attachCmd, cmd)
			}
		})
	}
}

// TestExecuteUsageOnError_CentralSilence is the regression test for
// ptone/scion#2859: Execute used to print the full Usage block after any RunE
// failure, because only attach opted into SilenceUsage. rootCmd's
// PersistentPreRunE now sets SilenceUsage on the executing command once the
// invocation is known to be well-formed, so argument/flag errors still show
// usage and runtime errors (hub or not) do not. A throwaway subcommand is
// mounted on the real rootCmd so the whole cobra dispatch path is exercised.
func TestExecuteUsageOnError_CentralSilence(t *testing.T) {
	origNoHub := noHub
	origProjectPath := projectPath
	origNonInteractive := nonInteractive
	origAutoConfirm := autoConfirm
	origAutoHelp := autoHelp
	origFormat := outputFormat

	var reqFlag string
	probe := &cobra.Command{
		Use:  "usage-probe-zzz <mode>",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			switch args[0] {
			case "hub":
				return wrapHubError(apiErr(500, "runtime_error", "hub exploded"))
			case "rawhub":
				return fmt.Errorf("delete failed: %w", apiErr(422, "runtime_error", "nope"))
			default:
				return errors.New("local runtime failure")
			}
		},
	}
	probe.Flags().StringVar(&reqFlag, "req", "", "required flag")
	require.NoError(t, probe.MarkFlagRequired("req"))
	rootCmd.AddCommand(probe)

	t.Cleanup(func() {
		rootCmd.RemoveCommand(probe)
		noHub = origNoHub
		projectPath = origProjectPath
		nonInteractive = origNonInteractive
		autoConfirm = origAutoConfirm
		autoHelp = origAutoHelp
		outputFormat = origFormat
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
	})

	hermeticCLIEnv(t)
	t.Setenv("SCION_HUB_ENDPOINT", "https://hub.invalid.example")
	noHub = true
	projectPath = t.TempDir()

	tests := []struct {
		name string
		args []string
		want bool
	}{
		{"missing positional arg shows usage", []string{"usage-probe-zzz", "--req", "x"}, true},
		{"too many positional args shows usage", []string{"usage-probe-zzz", "a", "b", "--req", "x"}, true},
		{"unknown flag shows usage", []string{"usage-probe-zzz", "local", "--req", "x", "--bogus-zzz"}, true},
		{"flag parse error shows usage", []string{"usage-probe-zzz", "local", "--req"}, true},
		{"missing required flag shows usage", []string{"usage-probe-zzz", "local"}, true},
		{"invalid --format value shows usage", []string{"usage-probe-zzz", "local", "--req", "x", "--format", "yaml"}, true},
		{"runtime error hides usage", []string{"usage-probe-zzz", "local", "--req", "x"}, false},
		{"hub API error hides usage", []string{"usage-probe-zzz", "hub", "--req", "x"}, false},
		{"wrapped raw hub API error hides usage", []string{"usage-probe-zzz", "rawhub", "--req", "x"}, false},
	}

	runProbe := func(t *testing.T, args []string) (*cobra.Command, error) {
		t.Helper()
		// SilenceUsage persists on the command across in-process
		// invocations; reset it as a fresh process would have it.
		probe.SilenceUsage = false
		outputFormat = ""
		reqFlag = ""
		probe.Flags().VisitAll(func(f *pflag.Flag) { f.Changed = false })
		var buf bytes.Buffer
		rootCmd.SetOut(&buf)
		rootCmd.SetErr(&buf)
		rootCmd.SetArgs(args)
		return rootCmd.ExecuteC()
	}

	// The rest of the hook runs after SilenceUsage is set, so a hook
	// failure that is not about the invocation (here: running outside any
	// scion project) is a runtime error and hides usage.
	t.Run("hook failure outside a project hides usage", func(t *testing.T) {
		setupNoProjectPreRun(t)
		cmd, err := runProbe(t, []string{"usage-probe-zzz", "local", "--req", "x"})
		require.Error(t, err)
		assert.Equal(t, probe, cmd)
		assert.NotContains(t, err.Error(), "local runtime failure", "must fail in the hook, before RunE")
		assert.False(t, showUsageForError(cmd, err, true), "error: %v", err)
	})

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd, err := runProbe(t, tt.args)
			require.Error(t, err)
			assert.Equal(t, probe, cmd, "dispatch must reach the probe command")
			assert.Equal(t, tt.want, showUsageForError(cmd, err, true), "error: %v", err)
		})
	}
}

// hermeticCLIEnv isolates a test that drives rootCmd from the developer's
// environment: HOME points at an empty temp dir (no global settings) and
// the SCION_* variables the CLI reads for agent/hub/project context are
// cleared. The test sets SCION_HUB_ENDPOINT itself if it needs it.
func hermeticCLIEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	for _, k := range []string{
		"SCION_HOST_UID", "SCION_AGENT_NAME", "SCION_AGENT_ID",
		"SCION_HUB_ENDPOINT", "SCION_HUB_URL", "SCION_PROJECT_ID",
		"SCION_PROJECT", "SCION_CREATOR",
	} {
		t.Setenv(k, "")
	}
}

// restoreSilenceUsage snapshots SilenceUsage on each of cmds and restores it
// at test end. Root's PersistentPreRunE sets SilenceUsage on the command it
// runs for (ptone/scion#2859), so a test that calls the hook directly on a
// package-level command would otherwise leave it set for later tests.
func restoreSilenceUsage(t *testing.T, cmds ...*cobra.Command) {
	t.Helper()
	orig := make([]bool, len(cmds))
	for i, c := range cmds {
		orig[i] = c.SilenceUsage
	}
	t.Cleanup(func() {
		for i, c := range cmds {
			c.SilenceUsage = orig[i]
		}
	})
}

// restoreAllSilenceUsage is restoreSilenceUsage for every command in the
// rootCmd tree, for tests that dispatch through rootCmd.Execute and so may
// leave SilenceUsage set on whichever command ran.
func restoreAllSilenceUsage(t *testing.T) {
	t.Helper()
	var all []*cobra.Command
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		all = append(all, c)
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(rootCmd)
	restoreSilenceUsage(t, all...)
}

// resetCommandFlags returns c's own flags to their defaults and clears
// c.SilenceUsage, as a fresh process would have them: flag values (and the
// package vars they are bound to), Changed and SilenceUsage all persist
// across in-process invocations of rootCmd.
func resetCommandFlags(t *testing.T, c *cobra.Command) {
	t.Helper()
	c.SilenceUsage = false
	// LocalFlags, not Flags: once cobra has merged root's persistent flags
	// into c.Flags() (Find and ExecuteC both do), resetting those would also
	// reset the root package vars a test set up (projectPath, noHub, ...).
	// The root --project Changed state is reset by resetRootProjectFlag.
	// Persistent flags inherited from non-root parents (e.g. project
	// messaging --project, hub allow-list --json) are not reset either; no
	// row uses one today.
	c.LocalFlags().VisitAll(func(f *pflag.Flag) {
		if sv, ok := f.Value.(pflag.SliceValue); ok {
			require.NoError(t, sv.Replace(nil))
		} else {
			require.NoError(t, f.Value.Set(f.DefValue))
		}
		f.Changed = false
	})
}

// saveCLIModeState restores, at test end, the package vars root's
// PersistentPreRunE mutates on every invocation (agent mode and
// cli.interactive_disabled set nonInteractive/autoConfirm; cli.autohelp sets
// autoHelp), so a test driving rootCmd cannot leak them into later tests.
func saveCLIModeState(t *testing.T) {
	t.Helper()
	origNonInteractive, origAutoConfirm, origAutoHelp := nonInteractive, autoConfirm, autoHelp
	t.Cleanup(func() {
		nonInteractive, autoConfirm, autoHelp = origNonInteractive, origAutoConfirm, origAutoHelp
	})
}

// resetRootProjectFlag clears the root --project/-g flag's Changed state
// (it persists across in-process invocations) now and at test end, and
// restores projectPath at test end.
func resetRootProjectFlag(t *testing.T) {
	t.Helper()
	f := rootCmd.PersistentFlags().Lookup("project")
	require.NotNil(t, f)
	origPath := projectPath
	f.Changed = false
	t.Cleanup(func() {
		f.Changed = false
		projectPath = origPath
	})
}

// TestExecuteUsageOnError_ArgsValidatorsKeepUsage covers the commands whose
// argument/flag checks used to live in RunE: with SilenceUsage now set
// centrally in root's PersistentPreRunE (ptone/scion#2859), a check in RunE
// would lose the usage block, so these checks moved into the commands' Args
// validators, which cobra runs before the hook. Each row drives the real
// rootCmd dispatch and must fail in that validator with usage shown.
func TestExecuteUsageOnError_ArgsValidatorsKeepUsage(t *testing.T) {
	origNoHub, origProjectPath := noHub, projectPath
	origScheduleIn, origScheduleAt, origScheduleType := scheduleIn, scheduleAt, scheduleType
	origScheduleAgent, origScheduleMessage := scheduleAgent, scheduleMessage
	origDC, origDA, origCP := recoverDisableConstraint, recoverDisableAll, recoverConfirmationPhrase
	t.Cleanup(func() {
		noHub, projectPath = origNoHub, origProjectPath
		scheduleIn, scheduleAt, scheduleType = origScheduleIn, origScheduleAt, origScheduleType
		scheduleAgent, scheduleMessage = origScheduleAgent, origScheduleMessage
		recoverDisableConstraint, recoverDisableAll, recoverConfirmationPhrase = origDC, origDA, origCP
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
	})
	hermeticCLIEnv(t)
	t.Setenv("SCION_HUB_ENDPOINT", "https://hub.invalid.example")
	noHub = true
	projectPath = t.TempDir()

	saveCLIModeState(t)

	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"template validate: neither name nor --all", []string{"templates", "validate"}, "requires a template name argument or --all flag"},
		{"template validate: name and --all", []string{"templates", "validate", "x", "--all"}, "cannot specify both a template name and --all"},
		{"harness-config validate: neither name nor --all", []string{"harness-config", "validate"}, "requires a harness-config name argument or --all flag"},
		{"templates sync: neither name nor --all", []string{"templates", "sync"}, "requires a template name argument or --all flag"},
		{"templates push: --name with --all", []string{"templates", "push", "--all", "--name", "n"}, "cannot use --name with --all"},
		{"template sync alias: name and --all", []string{"template", "sync", "x", "--all"}, "cannot specify both a template name and --all"},
		{"schedule create: no timing", []string{"schedule", "create", "--type", "message", "--agent", "a", "--message", "m"}, "either --in or --at is required"},
		{"schedule create: --in and --at", []string{"schedule", "create", "--in", "5m", "--at", "2030-01-01T00:00:00Z", "--agent", "a", "--message", "m"}, "--in and --at are mutually exclusive"},
		{"hub secret update: no metadata flag", []string{"hub", "secret", "update", "API_KEY"}, "at least one metadata flag must be provided"},
		{"hub secret update: bad --type", []string{"hub", "secret", "update", "API_KEY", "--type", "bogus"}, "type must be one of"},
		{"server recover-authz: no target flag", []string{"server", "recover-authz"}, "either --disable-constraint <id> or --disable-all-constraints is required"},
		{"server recover-authz: both target flags", []string{"server", "recover-authz", "--disable-constraint", "c1", "--disable-all-constraints"}, "mutually exclusive"},
		// The old --project <gcp> hint must beat root's required-flag check
		// (it used to be a PreRunE, which that check pre-empted).
		{"hub secret migrate: old --project hint", []string{"hub", "secret", "migrate", "--project", "my-gcp"}, gcpProjectFlagHint},
		{"hub secret migrate-names: old --project hint", []string{"hub", "secret", "migrate-names", "--project", "my-gcp"}, gcpProjectFlagHint},
		{"project service-accounts add: old --project hint", []string{"project", "service-accounts", "add", "sa@example.iam.gserviceaccount.com", "--project", "my-gcp"}, gcpProjectFlagHint},
		{"hub secret migrate: no flags -> required-flag error", []string{"hub", "secret", "migrate"}, `required flag(s) "gcp-project" not set`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target, _, err := rootCmd.Find(tt.args)
			require.NoError(t, err)
			resetCommandFlags(t, target)
			t.Cleanup(func() { resetCommandFlags(t, target) })
			resetRootProjectFlag(t)
			var buf bytes.Buffer
			rootCmd.SetOut(&buf)
			rootCmd.SetErr(&buf)
			rootCmd.SetArgs(tt.args)
			cmd, err := rootCmd.ExecuteC()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
			assert.Equal(t, target, cmd)
			assert.False(t, target.SilenceUsage, "must fail before root's hook sets SilenceUsage")
			assert.True(t, showUsageForError(cmd, err, true), "argument/flag error must show usage")
		})
	}
}

// TestUsageError covers the usageError marker (ptone/scion#2859): an
// argument/flag check in RunE returns it so the Usage block is still shown
// after root's hook set SilenceUsage, without changing the message, the
// wrapped error or the exit code.
func TestUsageError(t *testing.T) {
	parent := &cobra.Command{Use: "scion"}
	sub := &cobra.Command{Use: "sub"}
	parent.AddCommand(sub)
	sub.SilenceUsage = true // as root's PersistentPreRunE leaves it before RunE

	sentinel := errors.New("bad label")
	ue := newUsageError("--a and --b are mutually exclusive (%d)", 2)

	assert.Equal(t, "--a and --b are mutually exclusive (2)", ue.Error(), "message unchanged")
	assert.True(t, showUsageForError(sub, ue, true), "usage error shows usage after SilenceUsage")
	assert.True(t, showUsageForError(sub, fmt.Errorf("context: %w", ue), true), "a wrapped usage error still shows usage")
	assert.False(t, showUsageForError(sub, ue, false), "autoHelp=false still wins")
	assert.False(t, showUsageForError(nil, ue, true))
	assert.False(t, showUsageForError(sub, errors.New("runtime failure"), true), "a plain error after SilenceUsage hides usage")
	assert.Equal(t, 1, exitCodeFor(ue), "exit code unchanged")

	marked := asUsageError(fmt.Errorf("invalid label: %w", sentinel))
	assert.Equal(t, "invalid label: bad label", marked.Error())
	assert.ErrorIs(t, marked, sentinel, "the wrapped error stays reachable")
	assert.True(t, showUsageForError(sub, marked, true))
	assert.NoError(t, asUsageError(nil))
}

// TestExecuteUsageOnError_RunEUsageErrors drives a representative RunE
// argument/flag check from each large group converted to usageError
// through the real rootCmd dispatch: the check runs after root's hook set
// SilenceUsage, and must still show usage.
func TestExecuteUsageOnError_RunEUsageErrors(t *testing.T) {
	origNoHub, origProjectPath := noHub, projectPath
	origMsgIn, origMsgAt := msgIn, msgAt
	origScheduleName, origScheduleCron := scheduleName, scheduleCron
	origSortField := sortField
	origGlobalMode := globalMode
	t.Cleanup(func() {
		globalMode = origGlobalMode
		if f := rootCmd.PersistentFlags().Lookup("global"); f != nil {
			f.Changed = false
		}
		noHub, projectPath = origNoHub, origProjectPath
		msgIn, msgAt = origMsgIn, origMsgAt
		scheduleName, scheduleCron = origScheduleName, origScheduleCron
		sortField = origSortField
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
	})
	hermeticCLIEnv(t)
	t.Setenv("SCION_HUB_ENDPOINT", "https://hub.invalid.example")
	noHub = true
	tempProject := t.TempDir()
	projectPath = tempProject

	saveCLIModeState(t)

	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"message: --in and --at", []string{"message", "agent1", "hello", "--in", "5m", "--at", "2030-01-01T00:00:00Z"}, "--in and --at are mutually exclusive"},
		{"message: --thread-id without --channel", []string{"message", "agent1", "hello", "--thread-id", "t1"}, "--thread-id requires --channel to be set"},
		{"start: telemetry flags", []string{"start", "a1", "--enable-telemetry", "--disable-telemetry"}, "--enable-telemetry and --disable-telemetry are mutually exclusive"},
		{"create: bad --harness-auth", []string{"create", "a1", "--harness-auth", "bogus"}, "invalid --harness-auth value"},
		{"list: bad --sort", []string{"list", "--sort", "bogus"}, "invalid sort field"},
		{"hub env set: bad KEY=VALUE", []string{"hub", "env", "set", "novalue"}, "invalid format: expected KEY=VALUE or KEY VALUE"},
		{"hub secret set: bad key", []string{"hub", "secret", "set", "bad key", "v"}, "key cannot contain spaces"},
		{"schedule create-recurring: no --name", []string{"schedule", "create-recurring", "--cron", "0 * * * *"}, "--name is required"},
		{"skills publish: no --version", []string{"skills", "publish", "./skill"}, "--version is required"},
		{"sync to: no agent", []string{"sync", "to"}, "agent-level sync requires an agent name"},
		{"sync push: agent name given", []string{"sync", "push", "x"}, "'push' does not take an agent name"},
		{"start: negative --wait-timeout", []string{"start", "a1", "--wait-timeout=-5s"}, "--wait-timeout must not be negative"},
		{"service-accounts list: --assignable with --global", []string{"service-accounts", "list", "--global", "--assignable"}, "--assignable asks which accounts"},
		{"project skills add: skill URI with --from-directory", []string{"project", "skills", "add", "skill://foo", "--from-directory", "https://github.com/org/repo/tree/main/skills"}, "cannot combine a skill URI argument with --from-directory"},
		{"start: bad --template-scope", []string{"start", "a1", "--template-scope", "bogus"}, `unknown template scope "bogus"`},
		{"create: bad --template-scope", []string{"create", "a1", "--template-scope", "bogus"}, `unknown template scope "bogus"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target, _, err := rootCmd.Find(tt.args)
			require.NoError(t, err)
			resetCommandFlags(t, target)
			// Root persistent flags are not reset by resetCommandFlags; clear
			// --global, which one row sets.
			globalMode = false
			rootCmd.PersistentFlags().Lookup("global").Changed = false
			t.Cleanup(func() { resetCommandFlags(t, target) })
			var buf bytes.Buffer
			rootCmd.SetOut(&buf)
			rootCmd.SetErr(&buf)
			rootCmd.SetArgs(tt.args)
			var cmd *cobra.Command
			_, _ = captureStdIO(t, func() { cmd, err = rootCmd.ExecuteC() })
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
			assert.Equal(t, target, cmd)
			assert.True(t, target.SilenceUsage, "the check must run after root's hook (RunE), not in Args")
			assert.True(t, noHub, "the per-row flag reset must not undo the noHub setup")
			assert.Equal(t, tempProject, projectPath, "the per-row flag reset must not undo the temp project")
			assert.True(t, isUsageError(err), "error: %v", err)
			assert.True(t, showUsageForError(cmd, err, true))
		})
	}
}

// TestExecuteUsageOnError_HubPathUsageError covers usage-error sites on hub
// code paths, reached after the hub connection but before any hub API call:
// list --label (listAgentsViaHub) and a malformed conversation reference
// (resolveConversationRef). A mock hub answering only /healthz is enough to
// reach them.
func TestExecuteUsageOnError_HubPathUsageError(t *testing.T) {
	saveCLIModeState(t)
	origProjectPath, origNoHub := projectPath, noHub
	t.Cleanup(func() {
		projectPath, noHub = origProjectPath, origNoHub
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		t.Errorf("unexpected hub request %s %s: the argument/flag check must fail first", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(server.Close)

	hermeticCLIEnv(t)
	tmpHome := os.Getenv("HOME")
	// The endpoint is set in the env project's settings; the env var is
	// also set because root's hook only skips the image_registry check in
	// hub context (config.IsHubContext reads the environment).
	t.Setenv("SCION_HUB_ENDPOINT", server.URL)

	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"list: bad --label", []string{"list", "--label", "bad"}, `invalid label "bad": must be key=value`},
		{"conversation messages: bad reference", []string{"conversation", "messages", "conv:"}, `invalid conversation reference "conv:"`},
		{"conversation messages: unsupported reference type", []string{"conversation", "messages", "@user@example.com"}, "unsupported conversation reference type: @user@example.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target, _, err := rootCmd.Find(tt.args)
			require.NoError(t, err)
			resetCommandFlags(t, target)
			t.Cleanup(func() { resetCommandFlags(t, target) })
			// Set after the flag reset, and checked below, so the test runs
			// against the temp env project and never the repository's own
			// .scion.
			projectPath = setupEnvProject(t, tmpHome, server.URL)
			noHub = false
			var buf bytes.Buffer
			rootCmd.SetOut(&buf)
			rootCmd.SetErr(&buf)
			rootCmd.SetArgs(tt.args)
			var cmd *cobra.Command
			_, _ = captureStdIO(t, func() { cmd, err = rootCmd.ExecuteC() })
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
			assert.Equal(t, target, cmd)
			assert.True(t, strings.HasPrefix(projectPath, tmpHome), "must run against the temp env project, got %q", projectPath)
			assert.True(t, isUsageError(err), "error: %v", err)
			assert.True(t, showUsageForError(cmd, err, true))
		})
	}
}

func TestFormatFlagCheck(t *testing.T) {
	// Backup original values
	origFormat := outputFormat
	defer func() { outputFormat = origFormat }()

	// Clear SCION_HOST_UID so the agent-container check doesn't interfere
	t.Setenv("SCION_HOST_UID", "")

	// Bypass the git and workspace checks for this specific test
	origGlobal := globalMode
	globalMode = true
	defer func() { globalMode = origGlobal }()

	// Build a fake interactive command for testing rejection
	fakeAttachCmd := &cobra.Command{Use: "attach"}
	fakeAttachCmd.SetArgs([]string{})
	rootCmd.AddCommand(fakeAttachCmd)
	defer rootCmd.RemoveCommand(fakeAttachCmd)

	tests := []struct {
		name          string
		cmd           *cobra.Command
		format        string
		expectError   bool
		errorContains string
	}{
		{
			name:        "No format, other command",
			cmd:         &cobra.Command{Use: "other"},
			format:      "",
			expectError: false,
		},
		{
			name:        "Json format, list command",
			cmd:         listCmd,
			format:      "json",
			expectError: false,
		},
		{
			name:        "Plain format, list command",
			cmd:         listCmd,
			format:      "plain",
			expectError: false,
		},
		{
			name:          "Invalid format",
			cmd:           listCmd,
			format:        "yaml",
			expectError:   true,
			errorContains: "invalid format: yaml (allowed: json, plain)",
		},
		{
			name:        "Json format, non-interactive command",
			cmd:         &cobra.Command{Use: "other"},
			format:      "json",
			expectError: false,
		},
		{
			name:        "Json format, version command",
			cmd:         versionCmd,
			format:      "json",
			expectError: false,
		},
		{
			name:          "Json format, interactive command (attach)",
			cmd:           fakeAttachCmd,
			format:        "json",
			expectError:   true,
			errorContains: "--format json is not supported for 'scion attach'",
		},
	}

	// Test that look command clears outputFormat (json no-op)
	t.Run("Json format, look command (no-op)", func(t *testing.T) {
		outputFormat = "json"
		restoreSilenceUsage(t, lookCmd)
		err := rootCmd.PersistentPreRunE(lookCmd, []string{})
		if err != nil {
			assert.NotContains(t, err.Error(), "format")
		}
		assert.Empty(t, outputFormat, "outputFormat should be cleared for look command")
	})

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outputFormat = tt.format
			restoreSilenceUsage(t, tt.cmd)
			err := rootCmd.PersistentPreRunE(tt.cmd, []string{})

			if tt.expectError {
				assert.Error(t, err)
				if tt.errorContains != "" {
					assert.Contains(t, err.Error(), tt.errorContains)
				}
			} else {
				// If error is not nil, check if it's unrelated (e.g. git check)
				// But ideally we want no error.
				if err != nil {
					// Allow git check failure if it occurs, but ensure it's not a format error
					assert.NotContains(t, err.Error(), "format flag")
					assert.NotContains(t, err.Error(), "invalid format")
				}
			}
		})
	}
}

func TestHubAuthLoginDoesNotRequireImageRegistry(t *testing.T) {
	origGlobalMode := globalMode
	origProjectPath := projectPath
	origProfile := profile
	origOutputFormat := outputFormat
	origNoHub := noHub
	origHubEndpoint := hubEndpoint
	origNonInteractive := nonInteractive
	origAutoConfirm := autoConfirm
	defer func() {
		globalMode = origGlobalMode
		projectPath = origProjectPath
		profile = origProfile
		outputFormat = origOutputFormat
		noHub = origNoHub
		hubEndpoint = origHubEndpoint
		nonInteractive = origNonInteractive
		autoConfirm = origAutoConfirm
	}()

	t.Setenv("SCION_HOST_UID", "")

	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	if err := os.MkdirAll(filepath.Join(tmpHome, ".scion"), 0755); err != nil {
		t.Fatalf("failed to create test global scion dir: %v", err)
	}

	globalMode = true
	projectPath = ""
	profile = ""
	outputFormat = ""
	noHub = false
	hubEndpoint = "http://127.0.0.1:8080"
	nonInteractive = true
	autoConfirm = true

	restoreSilenceUsage(t, hubAuthLoginCmd)
	err := rootCmd.PersistentPreRunE(hubAuthLoginCmd, []string{})
	assert.NoError(t, err)
}

func TestServerStartDoesNotRequireImageRegistry(t *testing.T) {
	origGlobalMode := globalMode
	origProjectPath := projectPath
	origProfile := profile
	origOutputFormat := outputFormat
	origNoHub := noHub
	origNonInteractive := nonInteractive
	origAutoConfirm := autoConfirm
	defer func() {
		globalMode = origGlobalMode
		projectPath = origProjectPath
		profile = origProfile
		outputFormat = origOutputFormat
		noHub = origNoHub
		nonInteractive = origNonInteractive
		autoConfirm = origAutoConfirm
	}()

	t.Setenv("SCION_HOST_UID", "")

	// Create a temp home with a global .scion dir but NO image_registry
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	if err := os.MkdirAll(filepath.Join(tmpHome, ".scion"), 0755); err != nil {
		t.Fatalf("failed to create test global scion dir: %v", err)
	}

	globalMode = true
	projectPath = ""
	profile = ""
	outputFormat = ""
	noHub = true
	nonInteractive = true
	autoConfirm = true

	// serverStartCmd is a subcommand of serverCmd — its Name() is "start",
	// not "server". The PersistentPreRunE should still skip the image_registry
	// check because it's in the server subtree.
	restoreSilenceUsage(t, serverStartCmd)
	err := rootCmd.PersistentPreRunE(serverStartCmd, []string{})
	assert.NoError(t, err, "server start should not require image_registry")
}

// setupNoProjectPreRun prepares package-level command state for tests that
// exercise rootCmd.PersistentPreRunE outside any scion project. It saves and
// restores globalMode, projectPath, noHub, nonInteractive, autoConfirm,
// outputFormat, profile and autoHelp (mirroring the fields
// TestServerStartDoesNotRequireImageRegistry resets, so a stale value from
// another test — outputFormat in particular, which PersistentPreRunE
// rejects outright unless it's ""/json/plain — can't make these tests fail
// spuriously); clears SCION_HOST_UID and the leaked
// SCION_HUB_ENDPOINT/SCION_HUB_URL/SCION_PROJECT_ID env vars that would
// otherwise let config.IsHubContext() or FindProjectRoot() mask the "not in
// a scion project" failure these tests guard against, plus SCION_PROJECT
// and SCION_CREATOR, cleared defensively (see below); points HOME at a
// fresh temp dir; and changes into a temp dir with no .scion project
// anywhere above it (t.Chdir restores the working directory itself).
func setupNoProjectPreRun(t *testing.T) {
	t.Helper()

	origGlobalMode := globalMode
	origProjectPath := projectPath
	origNoHub := noHub
	origNonInteractive := nonInteractive
	origAutoConfirm := autoConfirm
	origOutputFormat := outputFormat
	origProfile := profile
	origAutoHelp := autoHelp
	t.Cleanup(func() {
		globalMode = origGlobalMode
		projectPath = origProjectPath
		noHub = origNoHub
		nonInteractive = origNonInteractive
		autoConfirm = origAutoConfirm
		outputFormat = origOutputFormat
		profile = origProfile
		autoHelp = origAutoHelp
	})

	t.Setenv("SCION_HOST_UID", "")
	// Clear leaked SCION_* env vars that make config.IsHubContext() true and
	// would otherwise let FindProjectRoot() synthesize a project path,
	// masking the "not in a scion project" failure these tests guard against.
	t.Setenv("SCION_HUB_ENDPOINT", "")
	t.Setenv("SCION_HUB_URL", "")
	t.Setenv("SCION_PROJECT_ID", "")
	// SCION_PROJECT and SCION_CREATOR aren't read by name on this code
	// path (the settings loaders bulk-load SCION_* vars but ignore these
	// two), but the sandbox container can still leak them (see
	// AGENTS.md, "Sandbox gotchas"), so clear them
	// defensively alongside the vars above to keep these tests isolated
	// against future readers.
	t.Setenv("SCION_PROJECT", "")
	t.Setenv("SCION_CREATOR", "")
	t.Setenv("HOME", t.TempDir())

	// A directory with no .scion project anywhere above it.
	t.Chdir(t.TempDir())

	globalMode = false
	projectPath = ""
	noHub = true
	nonInteractive = true
	autoConfirm = true
	outputFormat = ""
	profile = ""
	autoHelp = false
}

// TestHubSecretMigrateNamesAndMigrateDoNotRequireProject is a regression test
// for ptone/scion#2396: `scion hub secret migrate-names` (and its sibling
// `scion hub secret migrate`) operate directly against the Hub DB and GCP
// Secret Manager, never reading or resolving the current directory's scion
// project, so they must not fail with "not in a scion project" when run
// outside one — without requiring the --global workaround.
func TestHubSecretMigrateNamesAndMigrateDoNotRequireProject(t *testing.T) {
	setupNoProjectPreRun(t)

	for _, cmd := range []*cobra.Command{hubSecretMigrateNamesCmd, hubSecretMigrateCmd} {
		t.Run(cmd.CommandPath(), func(t *testing.T) {
			// The hook now also enforces required flags (ptone/scion#2859),
			// so supply --gcp-project as a real invocation must; this test
			// is about the project exemption, not flag validation.
			if f := cmd.Flags().Lookup("gcp-project"); f != nil {
				origVal, origChanged := f.Value.String(), f.Changed
				require.NoError(t, cmd.Flags().Set("gcp-project", "test-project"))
				t.Cleanup(func() {
					_ = f.Value.Set(origVal)
					f.Changed = origChanged
				})
			}
			restoreSilenceUsage(t, cmd)
			err := rootCmd.PersistentPreRunE(cmd, []string{})
			assert.NoError(t, err)
		})
	}
}

// TestOrdinaryCommandStillRequiresProject guards against the migrate-names
// exemption (ptone/scion#2396) becoming too broad: a command that isn't in
// any exemption list or subtree must still fail with "not in a scion
// project" when run outside one and without --global. It also checks three
// commands chosen to share something with the new exemption case's guard
// (`parentName == "secret" && commandInSubtree(cmd, "hub")`) without
// satisfying all of it, so that dropping either half of the guard would
// make this test fail:
//   - configMigrateCmd ("scion config migrate") shares the "migrate" name
//     but its parent is "config", not "secret", and it has no "hub"
//     ancestor.
//   - a synthetic "secret -> migrate-names" tree shares both the
//     "migrate-names" name and a "secret" parent, but (like the real
//     top-level "scion secret" command) has no "hub" ancestor. Dropping the
//     "commandInSubtree(cmd, "hub")" half of the guard would wrongly exempt
//     this tree.
//   - a synthetic "hub -> other -> migrate" tree has a "hub" ancestor, like
//     the real exemption target, but its parent is "other", not "secret".
//     Dropping the "parentName == "secret"" half of the guard would wrongly
//     exempt this tree.
func TestOrdinaryCommandStillRequiresProject(t *testing.T) {
	setupNoProjectPreRun(t)

	ordinaryCmd := &cobra.Command{Use: "other"}
	err := rootCmd.PersistentPreRunE(ordinaryCmd, []string{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not in a scion project")

	restoreSilenceUsage(t, configMigrateCmd)
	err = rootCmd.PersistentPreRunE(configMigrateCmd, []string{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not in a scion project")

	secretParent := &cobra.Command{Use: "secret"}
	migrateNamesChild := &cobra.Command{Use: "migrate-names"}
	secretParent.AddCommand(migrateNamesChild)
	err = rootCmd.PersistentPreRunE(migrateNamesChild, []string{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not in a scion project")

	hubParent := &cobra.Command{Use: "hub"}
	otherParent := &cobra.Command{Use: "other"}
	migrateChild := &cobra.Command{Use: "migrate"}
	hubParent.AddCommand(otherParent)
	otherParent.AddCommand(migrateChild)
	err = rootCmd.PersistentPreRunE(migrateChild, []string{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not in a scion project")
}

func TestDevAuthWarning(t *testing.T) {
	// Save and restore original flags
	origNoHub := noHub
	origHubEndpoint := hubEndpoint
	defer func() {
		noHub = origNoHub
		hubEndpoint = origHubEndpoint
	}()

	// Save and restore HOME
	origHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", origHome) }()

	// Create a temp directory for test settings
	tmpDir := t.TempDir()
	_ = os.Setenv("HOME", tmpDir)
	scionDir := filepath.Join(tmpDir, ".scion")
	if err := os.MkdirAll(scionDir, 0755); err != nil {
		t.Fatalf("failed to create test .scion dir: %v", err)
	}

	// Create settings.yaml with hub enabled
	settingsPath := filepath.Join(scionDir, "settings.yaml")
	settingsContent := `
hub:
  enabled: true
  endpoint: http://localhost:9810
`
	if err := os.WriteFile(settingsPath, []byte(settingsContent), 0644); err != nil {
		t.Fatalf("failed to write test settings: %v", err)
	}

	tests := []struct {
		name          string
		noHubFlag     bool
		hubEndpoint   string
		devTokenEnv   string
		devTokenFile  string
		expectWarning bool
	}{
		{
			name:          "No hub enabled, no warning",
			noHubFlag:     true,
			expectWarning: false,
		},
		{
			name:          "Local hub endpoint with dev token env var",
			noHubFlag:     false,
			hubEndpoint:   "http://localhost:9810",
			devTokenEnv:   "scion_dev_testtoken123",
			expectWarning: true,
		},
		{
			name:          "Hub endpoint via flag, no dev token",
			noHubFlag:     false,
			hubEndpoint:   "http://localhost:9810",
			devTokenEnv:   "",
			expectWarning: false,
		},
		{
			name:          "Remote hub with dev token env var warns",
			noHubFlag:     false,
			hubEndpoint:   "https://hub.demo.scion-ai.dev/",
			devTokenEnv:   "scion_dev_testtoken123",
			expectWarning: true,
		},
		{
			name:          "Remote hub with dev token file does not warn",
			noHubFlag:     false,
			hubEndpoint:   "https://hub.demo.scion-ai.dev/",
			devTokenFile:  "scion_dev_testtoken123",
			expectWarning: false,
		},
		{
			name:          "Local hub with dev token file warns",
			noHubFlag:     false,
			hubEndpoint:   "http://localhost:9810",
			devTokenFile:  "scion_dev_testtoken123",
			expectWarning: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set flags
			noHub = tt.noHubFlag
			hubEndpoint = tt.hubEndpoint

			// Set environment
			if tt.devTokenEnv != "" {
				_ = os.Setenv("SCION_DEV_TOKEN", tt.devTokenEnv)
				defer func() { _ = os.Unsetenv("SCION_DEV_TOKEN") }()
			} else {
				_ = os.Unsetenv("SCION_DEV_TOKEN")
			}
			_ = os.Unsetenv("SCION_DEV_TOKEN_FILE")
			// Clear v1 settings env var to prevent it from leaking dev auth
			origServerAuthToken := os.Getenv("SCION_AUTH_TOKEN")
			_ = os.Unsetenv("SCION_AUTH_TOKEN")
			defer func() { _ = os.Setenv("SCION_AUTH_TOKEN", origServerAuthToken) }()

			// Write dev token file if specified
			devTokenPath := filepath.Join(scionDir, "dev-token")
			if tt.devTokenFile != "" {
				_ = os.WriteFile(devTokenPath, []byte(tt.devTokenFile+"\n"), 0600)
				defer func() { _ = os.Remove(devTokenPath) }()
			} else {
				_ = os.Remove(devTokenPath)
			}

			// Capture stderr
			oldStderr := os.Stderr
			r, w, _ := os.Pipe()
			os.Stderr = w

			// Call the function (use empty project path as settings won't load in test env)
			printDevAuthWarningIfNeeded("")

			// Restore stderr and read output
			_ = w.Close()
			os.Stderr = oldStderr

			var buf bytes.Buffer
			_, _ = buf.ReadFrom(r)
			output := buf.String()

			if tt.expectWarning {
				assert.Contains(t, output, "WARNING")
				assert.Contains(t, output, "Development authentication enabled")
			} else {
				assert.NotContains(t, output, "WARNING")
			}
		})
	}
}

func TestNonInteractiveImpliesAutoConfirm(t *testing.T) {
	// Backup original values
	origAutoConfirm := autoConfirm
	origNonInteractive := nonInteractive
	origFormat := outputFormat
	defer func() {
		autoConfirm = origAutoConfirm
		nonInteractive = origNonInteractive
		outputFormat = origFormat
	}()

	// Ensure agent-mode auto-enable doesn't interfere with flag-level tests
	t.Setenv("SCION_CLI_MODE", "human")

	t.Run("nonInteractive sets autoConfirm true", func(t *testing.T) {
		autoConfirm = false
		nonInteractive = true
		outputFormat = ""

		// Run PersistentPreRunE - it should set autoConfirm = true
		_ = rootCmd.PersistentPreRunE(&cobra.Command{Use: "scion"}, []string{})

		assert.True(t, autoConfirm, "autoConfirm should be true when nonInteractive is set")
		assert.True(t, IsAutoConfirm(), "IsAutoConfirm() should return true")
		assert.True(t, IsNonInteractive(), "IsNonInteractive() should return true")
	})

	t.Run("autoConfirm without nonInteractive", func(t *testing.T) {
		autoConfirm = true
		nonInteractive = false
		outputFormat = ""

		_ = rootCmd.PersistentPreRunE(&cobra.Command{Use: "scion"}, []string{})

		assert.True(t, autoConfirm, "autoConfirm should remain true")
		assert.True(t, IsAutoConfirm(), "IsAutoConfirm() should return true")
		assert.False(t, IsNonInteractive(), "IsNonInteractive() should return false")
	})

	t.Run("neither flag set", func(t *testing.T) {
		autoConfirm = false
		nonInteractive = false
		outputFormat = ""

		_ = rootCmd.PersistentPreRunE(&cobra.Command{Use: "scion"}, []string{})

		assert.False(t, autoConfirm, "autoConfirm should remain false")
		assert.False(t, IsAutoConfirm(), "IsAutoConfirm() should return false")
		assert.False(t, IsNonInteractive(), "IsNonInteractive() should return false")
	})
}

func TestAgentModeImpliesNonInteractive(t *testing.T) {
	origAutoConfirm := autoConfirm
	origNonInteractive := nonInteractive
	origFormat := outputFormat
	defer func() {
		autoConfirm = origAutoConfirm
		nonInteractive = origNonInteractive
		outputFormat = origFormat
	}()

	t.Run("agent mode auto-enables non-interactive", func(t *testing.T) {
		t.Setenv("SCION_CLI_MODE", "agent")
		autoConfirm = false
		nonInteractive = false
		outputFormat = ""

		_ = rootCmd.PersistentPreRunE(&cobra.Command{Use: "scion"}, []string{})

		assert.True(t, nonInteractive, "nonInteractive should be auto-enabled in agent mode")
		assert.True(t, autoConfirm, "autoConfirm should be implied by nonInteractive")
	})

	t.Run("assistant mode does not auto-enable non-interactive", func(t *testing.T) {
		t.Setenv("SCION_CLI_MODE", "assistant")
		autoConfirm = false
		nonInteractive = false
		outputFormat = ""

		_ = rootCmd.PersistentPreRunE(&cobra.Command{Use: "scion"}, []string{})

		assert.False(t, nonInteractive, "nonInteractive should not be auto-enabled in assistant mode")
		assert.False(t, autoConfirm, "autoConfirm should remain false in assistant mode")
	})

	t.Run("skipped when already non-interactive", func(t *testing.T) {
		t.Setenv("SCION_CLI_MODE", "agent")
		autoConfirm = false
		nonInteractive = true
		outputFormat = ""

		_ = rootCmd.PersistentPreRunE(&cobra.Command{Use: "scion"}, []string{})

		assert.True(t, nonInteractive, "nonInteractive should remain true")
		assert.True(t, autoConfirm, "autoConfirm should be set by the flag-level check")
	})
}

func TestNonInteractiveFlagRegistered(t *testing.T) {
	// Verify the --non-interactive flag exists on the root command
	flag := rootCmd.PersistentFlags().Lookup("non-interactive")
	assert.NotNil(t, flag, "--non-interactive flag should be registered")
	assert.Equal(t, "false", flag.DefValue, "default value should be false")

	// Verify --yes flag still exists
	yesFlag := rootCmd.PersistentFlags().Lookup("yes")
	assert.NotNil(t, yesFlag, "--yes flag should be registered")
}

func TestHarnessConfigAliasRegistered(t *testing.T) {
	// Verify --harness-config and --harness flags exist on startCmd
	hcFlag := startCmd.Flags().Lookup("harness-config")
	assert.NotNil(t, hcFlag, "--harness-config flag should be registered on start")

	hFlag := startCmd.Flags().Lookup("harness")
	assert.NotNil(t, hFlag, "--harness flag should be registered on start")

	// Verify --harness-config and --harness flags exist on createCmd
	hcFlag = createCmd.Flags().Lookup("harness-config")
	assert.NotNil(t, hcFlag, "--harness-config flag should be registered on create")

	hFlag = createCmd.Flags().Lookup("harness")
	assert.NotNil(t, hFlag, "--harness flag should be registered on create")
}

func TestTelemetryFlagsRegistered(t *testing.T) {
	// Verify --enable-telemetry and --disable-telemetry flags exist on startCmd
	etFlag := startCmd.Flags().Lookup("enable-telemetry")
	assert.NotNil(t, etFlag, "--enable-telemetry flag should be registered on start")

	dtFlag := startCmd.Flags().Lookup("disable-telemetry")
	assert.NotNil(t, dtFlag, "--disable-telemetry flag should be registered on start")

	// Verify flags exist on resumeCmd
	etFlag = resumeCmd.Flags().Lookup("enable-telemetry")
	assert.NotNil(t, etFlag, "--enable-telemetry flag should be registered on resume")

	dtFlag = resumeCmd.Flags().Lookup("disable-telemetry")
	assert.NotNil(t, dtFlag, "--disable-telemetry flag should be registered on resume")
}

func TestTelemetryFlagsMutualExclusion(t *testing.T) {
	// Save and restore flag state
	origEnable := enableTelemetry
	origDisable := disableTelemetry
	defer func() {
		enableTelemetry = origEnable
		disableTelemetry = origDisable
	}()

	enableTelemetry = true
	disableTelemetry = true

	err := RunAgent(&cobra.Command{Use: "start"}, []string{"test-agent"}, false)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "--enable-telemetry and --disable-telemetry are mutually exclusive")
}

func TestCheckAgentContainerContext(t *testing.T) {
	// Save and restore original flag state
	origHubEndpoint := hubEndpoint
	defer func() { hubEndpoint = origHubEndpoint }()

	tests := []struct {
		name        string
		hostUID     string
		hubEndpoint string // flag value
		hubEnv      string // SCION_HUB_ENDPOINT env var
		networkMode string // SCION_NETWORK_MODE env var
		cmdName     string
		expectError bool
		errContains string
	}{
		{
			name:        "not in container — no error",
			hostUID:     "",
			cmdName:     "list",
			expectError: false,
		},
		{
			name:        "in container, no hub endpoint — error",
			hostUID:     "1000",
			cmdName:     "list",
			expectError: true,
			errContains: "no Hub endpoint is configured",
		},
		{
			name:        "in container, localhost hub — error",
			hostUID:     "1000",
			hubEnv:      "http://localhost:9810",
			cmdName:     "list",
			expectError: true,
			errContains: "points to localhost",
		},
		{
			name:        "in container, 127.0.0.1 hub — error",
			hostUID:     "1000",
			hubEnv:      "http://127.0.0.1:9810",
			cmdName:     "start",
			expectError: true,
			errContains: "points to localhost",
		},
		{
			name:        "in container, localhost hub with host networking — no error",
			hostUID:     "1000",
			hubEnv:      "http://localhost:8080",
			networkMode: "host",
			cmdName:     "list",
			expectError: false,
		},
		{
			name:        "in container, remote hub — no error",
			hostUID:     "1000",
			hubEnv:      "https://hub.scion.dev",
			cmdName:     "list",
			expectError: false,
		},
		{
			name:        "in container, remote hub via flag — no error",
			hostUID:     "1000",
			hubEndpoint: "https://hub.scion.dev",
			cmdName:     "list",
			expectError: false,
		},
		{
			name:        "in container, version command exempt",
			hostUID:     "1000",
			cmdName:     "version",
			expectError: false,
		},
		{
			name:        "in container, help command exempt",
			hostUID:     "1000",
			cmdName:     "help",
			expectError: false,
		},
		{
			name:        "in container, doctor command exempt",
			hostUID:     "1000",
			cmdName:     "doctor",
			expectError: false,
		},
		{
			name:        "in container, config command exempt",
			hostUID:     "1000",
			cmdName:     "config",
			expectError: false,
		},
		{
			name:        "in container, root command exempt",
			hostUID:     "1000",
			cmdName:     "scion",
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set env vars
			if tt.hostUID != "" {
				t.Setenv("SCION_HOST_UID", tt.hostUID)
			} else {
				t.Setenv("SCION_HOST_UID", "")
				_ = os.Unsetenv("SCION_HOST_UID")
			}
			if tt.hubEnv != "" {
				t.Setenv("SCION_HUB_ENDPOINT", tt.hubEnv)
			} else {
				t.Setenv("SCION_HUB_ENDPOINT", "")
				_ = os.Unsetenv("SCION_HUB_ENDPOINT")
			}
			t.Setenv("SCION_HUB_URL", "")
			_ = os.Unsetenv("SCION_HUB_URL")
			if tt.networkMode != "" {
				t.Setenv("SCION_NETWORK_MODE", tt.networkMode)
			} else {
				t.Setenv("SCION_NETWORK_MODE", "")
				_ = os.Unsetenv("SCION_NETWORK_MODE")
			}

			// Set flag
			hubEndpoint = tt.hubEndpoint

			cmd := &cobra.Command{Use: tt.cmdName}
			err := checkAgentContainerContext(cmd)

			if tt.expectError {
				assert.Error(t, err)
				if tt.errContains != "" {
					assert.Contains(t, err.Error(), tt.errContains)
				}
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestCheckAgentContainerContextConfigSubcommand(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "1000")
	t.Setenv("SCION_HUB_ENDPOINT", "")
	_ = os.Unsetenv("SCION_HUB_ENDPOINT")
	t.Setenv("SCION_HUB_URL", "")
	_ = os.Unsetenv("SCION_HUB_URL")

	origHubEndpoint := hubEndpoint
	hubEndpoint = ""
	defer func() { hubEndpoint = origHubEndpoint }()

	// config subcommand (e.g., "config set") should be exempt
	parentCmd := &cobra.Command{Use: "config"}
	childCmd := &cobra.Command{Use: "set"}
	parentCmd.AddCommand(childCmd)

	err := checkAgentContainerContext(childCmd)
	assert.NoError(t, err)
}

func TestIsLocalEndpoint(t *testing.T) {
	tests := []struct {
		endpoint string
		expected bool
	}{
		{"http://localhost:9810", true},
		{"http://127.0.0.1:9810", true},
		{"http://[::1]:9810", true},
		{"http://0.0.0.0:9810", true},
		{"https://hub.demo.scion-ai.dev/", false},
		{"https://example.com", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.endpoint, func(t *testing.T) {
			result := isLocalEndpoint(tt.endpoint)
			assert.Equal(t, tt.expected, result)
		})
	}
}
