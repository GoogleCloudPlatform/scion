/*
Copyright 2026 The Scion Authors.
*/

package commands

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

// resetRootCmdState makes a test that drives the package-global rootCmd
// independent of test order. cobra keeps parsed flag values (including the
// auto-registered --help flags) and the configured args/output writers on
// the command objects across Execute calls, so a test that runs e.g.
// "status --help" (directly, or via the unknown-status-type path in
// status.go) leaves every later "status ..." invocation printing help
// instead of running. This resets the whole command tree now and again at
// cleanup.
func resetRootCmdState(t *testing.T) {
	t.Helper()
	reset := func() {
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetIn(nil)
		resetCommandFlags(t, rootCmd)
	}
	reset()
	t.Cleanup(reset)
}

// resetCommandFlags restores every flag on cmd and its subcommands to its
// default value and clears Changed.
//
// Supported flag kinds: scalar values whose Set replaces the value and
// accepts their own DefValue (bool, string, int*, uint*, float*, duration,
// ... - everything sciontool registers today). Slice/array flags
// (pflag.SliceValue) and map flags (stringTo*) are NOT supported: their
// values keep an unexported "changed" bit, and once it is set, Set appends
// or merges instead of replacing. pflag has no public API to clear it, so a
// reset would leave the next parse merging into stale state, which is the
// ptone/scion#2653 bug class. Registering such a flag makes this helper fail
// loudly with the flag name, so the helper has to be extended in the same
// change.
func resetCommandFlags(t *testing.T, cmd *cobra.Command) {
	t.Helper()
	if err := resetFlags(cmd); err != nil {
		t.Fatal(err)
	}
}

// resetFlags is resetCommandFlags returning the first failure instead of
// failing a test, so the failure modes can be tested directly.
func resetFlags(cmd *cobra.Command) error {
	var firstErr error
	resetFlag := func(f *pflag.Flag) {
		if firstErr != nil {
			return
		}
		if _, isSlice := f.Value.(pflag.SliceValue); isSlice || strings.HasPrefix(f.Value.Type(), "stringTo") {
			firstErr = fmt.Errorf("reset flag --%s on %q: slice/map flag type %q is not supported by resetCommandFlags; extend it", f.Name, cmd.CommandPath(), f.Value.Type())
			return
		}
		if err := f.Value.Set(f.DefValue); err != nil {
			firstErr = fmt.Errorf("reset flag --%s (%s) on %q: %w", f.Name, f.Value.Type(), cmd.CommandPath(), err)
			return
		}
		f.Changed = false
	}
	cmd.Flags().VisitAll(resetFlag)
	cmd.PersistentFlags().VisitAll(resetFlag)
	if firstErr != nil {
		return firstErr
	}
	for _, child := range cmd.Commands() {
		if err := resetFlags(child); err != nil {
			return err
		}
	}
	return nil
}

// setTestLogPath points pkg/sciontool/log at path for the duration of the
// test and restores the TestMain sandbox log path afterwards, so later
// tests do not log into a removed t.TempDir.
func setTestLogPath(t *testing.T, path string) {
	t.Helper()
	log.SetLogPath(path)
	t.Cleanup(func() { log.SetLogPath(filepath.Join(sandboxHomeDir, "agent.log")) })
}

func TestResetCommandFlagsRestoresDefaults(t *testing.T) {
	root := &cobra.Command{Use: "root"}
	child := &cobra.Command{Use: "child", Run: func(*cobra.Command, []string) {}}
	root.AddCommand(child)
	var verbose bool
	var name string
	var count int
	var wait time.Duration
	root.PersistentFlags().BoolVar(&verbose, "verbose", false, "")
	child.Flags().StringVar(&name, "name", "def", "")
	child.Flags().IntVar(&count, "count", 3, "")
	child.Flags().DurationVar(&wait, "wait", time.Second, "")
	root.SetOut(new(strings.Builder))

	// First parse: non-default values, and --help left set (the leftover
	// state behind ptone/scion#2653).
	root.SetArgs([]string{"child", "--verbose", "--name=first", "--count=7", "--wait=2m", "--help"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !verbose || name != "first" || count != 7 || wait != 2*time.Minute {
		t.Fatalf("test bug: flags not applied: verbose=%v name=%q count=%d wait=%v", verbose, name, count, wait)
	}

	resetCommandFlags(t, root)
	if verbose || name != "def" || count != 3 || wait != time.Second {
		t.Fatalf("after reset: verbose=%v name=%q count=%d wait=%v, want false \"def\" 3 1s", verbose, name, count, wait)
	}
	for _, f := range []*pflag.Flag{
		root.PersistentFlags().Lookup("verbose"),
		child.Flags().Lookup("name"),
		child.Flags().Lookup("count"),
		child.Flags().Lookup("wait"),
		child.Flags().Lookup("help"),
	} {
		if f == nil {
			t.Fatal("expected flag missing")
		}
		if f.Changed || f.Value.String() != f.DefValue {
			t.Errorf("--%s not reset: changed=%v value=%q default=%q", f.Name, f.Changed, f.Value.String(), f.DefValue)
		}
	}

	// Next parse: new values must replace (not merge with) the defaults,
	// unset flags must keep their defaults, and without --help the command
	// must run again.
	ran := false
	child.Run = func(*cobra.Command, []string) { ran = true }
	root.SetArgs([]string{"child", "--name=second", "--count=9"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Fatal("child did not run after reset; --help state leaked")
	}
	if verbose || name != "second" || count != 9 || wait != time.Second {
		t.Fatalf("next parse: verbose=%v name=%q count=%d wait=%v, want false \"second\" 9 1s", verbose, name, count, wait)
	}
	for flag, wantChanged := range map[*pflag.Flag]bool{
		root.PersistentFlags().Lookup("verbose"): false,
		child.Flags().Lookup("name"):             true,
		child.Flags().Lookup("count"):            true,
		child.Flags().Lookup("wait"):             false,
		child.Flags().Lookup("help"):             false,
	} {
		if flag.Changed != wantChanged {
			t.Errorf("next parse: --%s changed=%v, want %v", flag.Name, flag.Changed, wantChanged)
		}
	}
}

func TestResetFlagsRejectsSliceAndMapFlags(t *testing.T) {
	for _, tc := range []struct {
		name     string
		register func(*pflag.FlagSet)
		flag     string
		typ      string
	}{
		{"string slice", func(fs *pflag.FlagSet) { fs.StringSlice("tags", []string{"x"}, "") }, "--tags", "stringSlice"},
		{"string array", func(fs *pflag.FlagSet) { fs.StringArray("items", nil, "") }, "--items", "stringArray"},
		{"int slice", func(fs *pflag.FlagSet) { fs.IntSlice("ports", nil, "") }, "--ports", "intSlice"},
		{"string to string", func(fs *pflag.FlagSet) { fs.StringToString("labels", nil, "") }, "--labels", "stringToString"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := &cobra.Command{Use: "root"}
			child := &cobra.Command{Use: "child"}
			root.AddCommand(child)
			tc.register(child.Flags())

			err := resetFlags(root)
			if err == nil {
				t.Fatalf("resetFlags accepted a %s flag; it must fail loudly", tc.typ)
			}
			for _, want := range []string{tc.flag, "root child", tc.typ, "not supported"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

// TestResetFlagsResetsPersistentFlagsWithoutExecute covers the
// PersistentFlags visit: a persistent flag changed directly (no Execute, so
// cobra never merged it into the command's Flags() set) must be reset too.
func TestResetFlagsResetsPersistentFlagsWithoutExecute(t *testing.T) {
	root := &cobra.Command{Use: "root"}
	root.AddCommand(&cobra.Command{Use: "child"})
	var verbose bool
	root.PersistentFlags().BoolVar(&verbose, "verbose", false, "")
	if err := root.PersistentFlags().Set("verbose", "true"); err != nil {
		t.Fatal(err)
	}
	f := root.PersistentFlags().Lookup("verbose")
	if !verbose || !f.Changed {
		t.Fatalf("test bug: verbose=%v changed=%v before reset", verbose, f.Changed)
	}
	if root.Flags().Lookup("verbose") != nil {
		t.Fatal("test bug: persistent flag already merged into Flags(); the case no longer isolates the PersistentFlags visit")
	}

	if err := resetFlags(root); err != nil {
		t.Fatal(err)
	}
	if verbose || f.Changed {
		t.Fatalf("after reset: verbose=%v changed=%v, want false false", verbose, f.Changed)
	}
}
