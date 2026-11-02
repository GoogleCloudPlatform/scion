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
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/spf13/cobra"
)

// experimentsScope admits both reading and changing experiment overrides.
const experimentsScope = "hub_experiments:update"

// experimentsRetryHint is the revision-conflict guidance of the experiments
// commands.
const experimentsRetryHint = "The experiment settings changed since they were read. Re-run the command to apply the change to the current overrides, or pin the revision with --revision N."

var (
	experimentsRevision         int64
	experimentsResetAll         bool
	experimentsConfirmMalformed bool
	experimentsYes              bool
)

var hubExperimentsCmd = &cobra.Command{
	Use:   "experiments",
	Short: "View and override the Hub's experiments",
	Long: `View the Hub's experiments and override whether each one is on.

An override replaces an experiment's built-in default hub-wide; resetting
it returns the experiment to its default.

Changes are compare-and-set on the experiment settings revision: by default
the revision read just before the change, or --revision N.

Needs a Hub admin login, or an access token on a hub boundary with the
hub_experiments:update scope (also needed to list them).`,
}

var hubExperimentsListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List experiments with their defaults and overrides",
	Args:    cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		svc, ctx, cancel, err := adminSettingsClient(cmd)
		if err != nil {
			return err
		}
		defer cancel()
		return runExperimentsList(ctx, svc, cmd.OutOrStdout(), cmd.ErrOrStderr())
	},
}

func experimentOverrideCmd(use, short string, value *bool) *cobra.Command {
	return &cobra.Command{
		Use:   use + " NAME [NAME ...]",
		Short: short,
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, ctx, cancel, err := adminSettingsClient(cmd)
			if err != nil {
				return err
			}
			defer cancel()
			var rev *int64
			if cmd.Flags().Changed("revision") {
				rev = &experimentsRevision
			}
			return runExperimentsOverride(ctx, svc, cmd.OutOrStdout(), cmd.ErrOrStderr(), args, value, rev)
		},
	}
}

var (
	experimentOn  = true
	experimentOff = false

	hubExperimentsEnableCmd  = experimentOverrideCmd("enable", "Turn experiments on hub-wide (override)", &experimentOn)
	hubExperimentsDisableCmd = experimentOverrideCmd("disable", "Turn experiments off hub-wide (override)", &experimentOff)
)

var hubExperimentsResetCmd = &cobra.Command{
	Use:   "reset [NAME ...]",
	Short: "Return experiments to their built-in defaults",
	Long: `Remove the override of the named experiments, so each returns to its
built-in default.

--all removes every override, including overrides of experiments this Hub
no longer knows. It asks for confirmation (--yes answers it). When the
stored overrides are unreadable, --all also needs --confirm-malformed.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if experimentsResetAll == (len(args) > 0) {
			return fmt.Errorf("name experiments to reset, or use --all")
		}
		svc, ctx, cancel, err := adminSettingsClient(cmd)
		if err != nil {
			return err
		}
		defer cancel()
		var rev *int64
		if cmd.Flags().Changed("revision") {
			rev = &experimentsRevision
		}
		if experimentsResetAll {
			return runExperimentsResetAll(ctx, svc, cmd.OutOrStdout(), cmd.ErrOrStderr(), rev, experimentsConfirmMalformed, experimentsYes)
		}
		return runExperimentsOverride(ctx, svc, cmd.OutOrStdout(), cmd.ErrOrStderr(), args, nil, rev)
	},
}

func runExperimentsList(ctx context.Context, svc hubclient.AdminSettingsService, out, errw io.Writer) error {
	format, err := settingsOutputFormat()
	if err != nil {
		return err
	}
	exps, err := svc.GetExperiments(ctx)
	if err != nil {
		return reportSettingsError(out, errw, format, err, settingsScopeHint{Scope: experimentsScope, Retry: experimentsRetryHint})
	}
	if format != "" {
		return writeStructured(out, format, exps)
	}
	printExperiments(out, errw, exps)
	return nil
}

func printExperiments(out, errw io.Writer, exps *hubclient.AdminExperiments) {
	_, _ = fmt.Fprintf(out, "Experiment settings revision %d\n", exps.Revision)
	if exps.Malformed {
		_, _ = fmt.Fprintln(errw, "Warning: the stored overrides are unreadable; every experiment uses its default. Reset with 'scion hub experiments reset --all --confirm-malformed'.")
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "NAME\tSTAGE\tDEFAULT\tOVERRIDE\tENABLED\tNOTES")
	for _, e := range exps.Experiments {
		override := "-"
		if e.Override != nil {
			override = onOff(*e.Override)
		}
		notes := ""
		if e.ReviewOverdue {
			notes = "review overdue"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", e.Name, orDash(e.Stage), onOff(e.Default), override, onOff(e.Enabled), notes)
	}
	_ = tw.Flush()
	if len(exps.UnknownOverrides) > 0 {
		names := make([]string, 0, len(exps.UnknownOverrides))
		for n := range exps.UnknownOverrides {
			names = append(names, n)
		}
		sort.Strings(names)
		_, _ = fmt.Fprintf(errw, "Note: overrides of experiments this Hub does not know: %s (removed by 'reset --all').\n", strings.Join(names, ", "))
	}
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// runExperimentsOverride sets (value true or false) or clears (value nil)
// the override of each named experiment in one compare-and-set write.
func runExperimentsOverride(ctx context.Context, svc hubclient.AdminSettingsService, out, errw io.Writer, names []string, value *bool, rev *int64) error {
	format, err := settingsOutputFormat()
	if err != nil {
		return err
	}
	if err := checkExperimentsRevision(rev); err != nil {
		return err
	}
	cur, err := svc.GetExperiments(ctx)
	if err != nil {
		return reportSettingsError(out, errw, format, err, settingsScopeHint{Scope: experimentsScope, Retry: experimentsRetryHint})
	}
	// The Hub refuses every per-name change while the stored overrides are
	// unreadable; say so before sending one, and name the way out.
	if cur.Malformed {
		return fmt.Errorf("the stored experiment overrides are unreadable, so no single override can be changed; reset them all with 'scion hub experiments reset --all --confirm-malformed'")
	}
	known := map[string]bool{}
	for _, e := range cur.Experiments {
		known[e.Name] = true
	}
	overrides := make(map[string]*bool, len(names))
	for _, n := range names {
		_, unknownOverride := cur.UnknownOverrides[n]
		if !known[n] && (value != nil || !unknownOverride) {
			return fmt.Errorf("no experiment %q (see 'scion hub experiments list')", n)
		}
		if _, dup := overrides[n]; dup {
			return fmt.Errorf("%q is given more than once", n)
		}
		if value == nil {
			overrides[n] = nil
		} else {
			v := *value
			overrides[n] = &v
		}
	}
	expected := cur.Revision
	if rev != nil {
		expected = *rev
	}
	res, err := svc.UpdateExperiments(ctx, overrides, expected)
	if err != nil {
		return reportSettingsError(out, errw, format, err, settingsScopeHint{Scope: experimentsScope, Retry: experimentsRetryHint})
	}
	if format != "" {
		return writeStructured(out, format, res)
	}
	_, _ = fmt.Fprintln(out, "Saved.")
	printExperiments(out, errw, res)
	return nil
}

func runExperimentsResetAll(ctx context.Context, svc hubclient.AdminSettingsService, out, errw io.Writer, rev *int64, confirmMalformed, yes bool) error {
	format, err := settingsOutputFormat()
	if err != nil {
		return err
	}
	if err := checkExperimentsRevision(rev); err != nil {
		return err
	}
	cur, err := svc.GetExperiments(ctx)
	if err != nil {
		return reportSettingsError(out, errw, format, err, settingsScopeHint{Scope: experimentsScope, Retry: experimentsRetryHint})
	}
	if cur.Malformed && !confirmMalformed {
		return fmt.Errorf("the stored overrides are unreadable; add --confirm-malformed to reset them")
	}
	if !hubsync.ConfirmAction("Remove every experiment override, returning all experiments to their defaults?", false, yes) {
		return fmt.Errorf("reset cancelled")
	}
	expected := cur.Revision
	if rev != nil {
		expected = *rev
	}
	res, err := svc.ResetExperiments(ctx, expected, cur.Malformed && confirmMalformed)
	if err != nil {
		return reportSettingsError(out, errw, format, err, settingsScopeHint{Scope: experimentsScope, Retry: experimentsRetryHint})
	}
	if format != "" {
		return writeStructured(out, format, res)
	}
	_, _ = fmt.Fprintln(out, "Every experiment override removed.")
	printExperiments(out, errw, res)
	return nil
}

// checkExperimentsRevision refuses a negative --revision, as the settings
// pins do; nil means none was given.
func checkExperimentsRevision(rev *int64) error {
	if rev != nil && *rev < 0 {
		return fmt.Errorf("--revision %d: the revision must be a non-negative integer", *rev)
	}
	return nil
}

func init() {
	hubCmd.AddCommand(hubExperimentsCmd)
	hubExperimentsCmd.AddCommand(hubExperimentsListCmd, hubExperimentsEnableCmd, hubExperimentsDisableCmd, hubExperimentsResetCmd)
	for _, c := range []*cobra.Command{hubExperimentsEnableCmd, hubExperimentsDisableCmd, hubExperimentsResetCmd} {
		c.Flags().Int64Var(&experimentsRevision, "revision", 0, "Expected experiment settings revision (default: the revision just read)")
	}
	hubExperimentsResetCmd.Flags().BoolVar(&experimentsResetAll, "all", false, "Remove every override")
	hubExperimentsResetCmd.Flags().BoolVar(&experimentsConfirmMalformed, "confirm-malformed", false, "With --all: confirm resetting unreadable stored overrides")
	hubExperimentsResetCmd.Flags().BoolVarP(&experimentsYes, "yes", "y", false, "Answer Yes to the confirmation prompt (required to confirm when stdin is not a terminal)")
}
