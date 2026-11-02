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
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

const (
	srvcfgReadScope  = "hub_config:read"
	srvcfgWriteScope = "hub_config:update"
)

var (
	srvcfgGetSection   string
	srvcfgSetFile      string
	srvcfgSetString    bool
	srvcfgNoValidate   bool
	srvcfgDryRun       bool
	srvcfgRevisions    []string
	srvcfgUnsetSection string
	srvcfgUnsetReset   bool
	srvcfgUnsetYes     bool
)

var hubSettingsCmd = &cobra.Command{
	Use:   "settings",
	Short: "View and change the Hub's server configuration",
	Long: `View and change the Hub's server configuration: operational settings,
agent defaults, runtimes, profiles, harness configs, telemetry and
federation.

Keys are dotted paths as 'scion hub settings schema' lists them, for
example server.hub.public_url, default_max_turns or runtimes. Keys below a
listed key are accepted too (runtimes.k8s.namespace).

Changes are compare-and-set: each command sends the revision of every
section it changes as read just before, so a change made in between is
reported as a conflict instead of being overwritten.

Needs a Hub admin login, or an access token on a hub boundary with the
hub_config:read scope (get, schema) or hub_config:update scope (set,
unset). Project defaults and experiments have their own commands:
'scion hub project-defaults' and 'scion hub experiments'.`,
}

var hubSettingsGetCmd = &cobra.Command{
	Use:   "get [KEY]",
	Short: "Show the server configuration, a section or one key",
	Long: `Show the server configuration.

With no argument, lists each section with where its values come from
(db: saved on the Hub; file or default: the Hub's startup configuration),
its revision and when it was last changed. With KEY, prints that key's
value. With --section, prints every key of the section.

Secret values are shown masked.

Examples:
  scion hub settings get
  scion hub settings get server.hub.public_url
  scion hub settings get --section agent_defaults
  scion hub settings get runtimes --format yaml`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		svc, ctx, cancel, err := adminSettingsClient(cmd)
		if err != nil {
			return err
		}
		defer cancel()
		key := ""
		if len(args) == 1 {
			key = args[0]
		}
		return runHubSettingsGet(ctx, svc, cmd.OutOrStdout(), key, srvcfgGetSection)
	},
}

var hubSettingsSetCmd = &cobra.Command{
	Use:   "set KEY VALUE [KEY VALUE ...]",
	Short: "Change server configuration keys",
	Long: `Change one or more server configuration keys in one compare-and-set save.

VALUE is read as YAML: true, 42, 1.5, "text", [a, b] or {k: v}. Use
--string to send a value as text as it is. A key below a listed key
(runtimes.k8s.namespace) changes that part of the current value and saves
the whole value, so other entries are kept.

--file reads a YAML or JSON document of keys instead of arguments; a null
in it unsets the key.

Examples:
  scion hub settings set default_max_turns 200
  scion hub settings set server.hub.admin_emails '[ops@example.com]'
  scion hub settings set default_model claude-opus --string
  scion hub settings set runtimes.k8s.namespace agents
  scion hub settings set --file settings.yaml
  scion hub settings set default_max_turns 200 --revision agent_defaults=7`,
	RunE: func(cmd *cobra.Command, args []string) error {
		changes, err := setArgsToChanges(args, srvcfgSetFile, srvcfgSetString)
		if err != nil {
			return err
		}
		pins, err := parseRevisionPins(srvcfgRevisions)
		if err != nil {
			return err
		}
		svc, ctx, cancel, err := adminSettingsClient(cmd)
		if err != nil {
			return err
		}
		defer cancel()
		return runHubSettingsWrite(ctx, svc, cmd.OutOrStdout(), cmd.ErrOrStderr(), changes, srvcfgWriteOptions{
			Plan:   settingsPlanOptions{AllowUnknown: srvcfgNoValidate, Revisions: pins},
			DryRun: srvcfgDryRun,
			File:   srvcfgSetFile,
		})
	},
}

var hubSettingsUnsetCmd = &cobra.Command{
	Use:   "unset KEY [KEY ...]",
	Short: "Clear server configuration keys",
	Long: `Clear one or more server configuration keys, so the Hub uses its
default for them.

A listed key is cleared on the Hub. A key below a listed key
(runtimes.docker, profiles.small.runtime) is removed from the current value,
and the rest is saved.

Some keys cannot be cleared this way: the GCP permission-check keys need a
value, and the agent role defaults are read-only here.

--section NAME --reset instead deletes the section's saved values, so the
Hub falls back to its startup configuration for the whole section. It asks
for confirmation (--yes answers it).

Examples:
  scion hub settings unset default_max_turns
  scion hub settings unset runtimes.old-runtime
  scion hub settings unset --section telemetry --reset --yes`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if srvcfgUnsetReset || srvcfgUnsetSection != "" {
			if !srvcfgUnsetReset || srvcfgUnsetSection == "" || len(args) > 0 {
				return fmt.Errorf("use --section NAME --reset on their own, without keys")
			}
			svc, ctx, cancel, err := adminSettingsClient(cmd)
			if err != nil {
				return err
			}
			defer cancel()
			return runHubSettingsReset(ctx, svc, cmd.OutOrStdout(), cmd.ErrOrStderr(), srvcfgUnsetSection, srvcfgUnsetYes)
		}
		if len(args) == 0 {
			return fmt.Errorf("name at least one key to unset")
		}
		changes := make([]settingsChange, 0, len(args))
		for _, k := range args {
			changes = append(changes, settingsChange{Key: k, Unset: true})
		}
		pins, err := parseRevisionPins(srvcfgRevisions)
		if err != nil {
			return err
		}
		svc, ctx, cancel, err := adminSettingsClient(cmd)
		if err != nil {
			return err
		}
		defer cancel()
		return runHubSettingsWrite(ctx, svc, cmd.OutOrStdout(), cmd.ErrOrStderr(), changes, srvcfgWriteOptions{
			Plan:   settingsPlanOptions{AllowUnknown: srvcfgNoValidate, Revisions: pins},
			DryRun: srvcfgDryRun,
		})
	},
}

var hubSettingsSchemaCmd = &cobra.Command{
	Use:   "schema [SECTION]",
	Short: "List the settings sections and keys, or show a section's schema",
	Long: `List the server configuration sections with their keys. Keys marked
"(restart)" take effect only after a Hub restart.

With SECTION, prints that section's JSON schema.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		svc, ctx, cancel, err := adminSettingsClient(cmd)
		if err != nil {
			return err
		}
		defer cancel()
		section := ""
		if len(args) == 1 {
			section = args[0]
		}
		return runHubSettingsSchema(ctx, svc, cmd.OutOrStdout(), section)
	},
}

// adminSettingsClient loads the Hub client for the settings commands.
func adminSettingsClient(cmd *cobra.Command) (hubclient.AdminSettingsService, context.Context, context.CancelFunc, error) {
	if _, err := settingsOutputFormat(); err != nil {
		return nil, nil, nil, err
	}
	_, client, err := loadHubClient()
	if err != nil {
		return nil, nil, nil, err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 60*time.Second)
	return client.AdminSettings(), ctx, cancel, nil
}

// setArgsToChanges reads KEY VALUE pairs, or a --file document.
func setArgsToChanges(args []string, file string, asString bool) ([]settingsChange, error) {
	if file != "" {
		if len(args) > 0 {
			return nil, fmt.Errorf("use either --file or KEY VALUE arguments, not both")
		}
		return nil, nil // read once the schema is known (runHubSettingsWrite)
	}
	if len(args) == 0 || len(args)%2 != 0 {
		return nil, fmt.Errorf("want KEY VALUE pairs (got %d argument(s))", len(args))
	}
	changes := make([]settingsChange, 0, len(args)/2)
	for i := 0; i < len(args); i += 2 {
		v, err := parseSettingsValue(args[i+1], asString)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", args[i], err)
		}
		changes = append(changes, settingsChange{Key: args[i], Value: v})
	}
	return changes, nil
}

// readSettingsDocument reads a YAML or JSON settings document from path
// ("-" is stdin).
func readSettingsDocument(path string, stdin io.Reader) (map[string]interface{}, error) {
	var data []byte
	var err error
	if path == "-" {
		data, err = io.ReadAll(stdin)
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var doc interface{}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	norm, err := normalizeYAMLValue(doc)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	m, ok := norm.(map[string]interface{})
	if !ok || len(m) == 0 {
		return nil, fmt.Errorf("%s: want a non-empty mapping of settings keys", path)
	}
	return m, nil
}

func runHubSettingsGet(ctx context.Context, svc hubclient.AdminSettingsService, out io.Writer, key, section string) error {
	format, err := settingsOutputFormat()
	if err != nil {
		return err
	}
	if key != "" && section != "" {
		return fmt.Errorf("use either KEY or --section, not both")
	}
	cfg, err := svc.GetServerConfig(ctx)
	if err != nil {
		return reportSettingsError(out, os.Stderr, format, err, settingsScopeHint{Scope: srvcfgReadScope})
	}

	switch {
	case key != "":
		var ix *settingsKeyIndex
		if schema, serr := svc.GetServerConfigSchema(ctx); serr == nil {
			ix = newSettingsKeyIndex(schema)
		}
		norm := normalizeSettingsKey(key)
		sectionName := ""
		if ix != nil {
			if _, sec, ok := ix.resolve(norm); ok {
				sectionName = sec
			}
		}
		raw, ok := cfg.Lookup(strings.Join(settingsBodyPath(norm), "."))
		if !ok {
			msg := fmt.Sprintf("%q is not set", key)
			if ix != nil {
				if _, _, known := ix.resolve(norm); !known {
					msg = fmt.Sprintf("%q is not a server configuration key", key)
					if s := ix.suggest(norm); len(s) > 0 {
						msg += "; did you mean " + strings.Join(s, ", ") + "?"
					}
				}
			}
			return fmt.Errorf("%s", msg)
		}
		if format != "" {
			view := map[string]interface{}{"key": norm, "value": raw}
			if sectionName != "" {
				view["section"] = sectionName
				if meta, ok := cfg.SectionMetadata[sectionName]; ok {
					view["revision"] = meta.Revision
					view["source"] = meta.Source
				}
			}
			return writeStructured(out, format, view)
		}
		text, err := formatSettingsValue(raw)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintln(out, text)
		return nil

	case section != "":
		schema, err := svc.GetServerConfigSchema(ctx)
		if err != nil {
			return reportSettingsError(out, os.Stderr, format, err, settingsScopeHint{Scope: srvcfgReadScope})
		}
		sec, ok := schema[section]
		if !ok {
			return fmt.Errorf("no settings section %q (see 'scion hub settings schema')", section)
		}
		if len(sec.KoanfPaths) == 0 {
			return fmt.Errorf("section %q has no keys in the server configuration", section)
		}
		values := map[string]json.RawMessage{}
		keys := append([]string(nil), sec.KoanfPaths...)
		sort.Strings(keys)
		for _, k := range keys {
			if raw, ok := cfg.Lookup(strings.Join(settingsBodyPath(k), ".")); ok {
				values[k] = raw
			}
		}
		meta := cfg.SectionMetadata[section]
		if format != "" {
			return writeStructured(out, format, map[string]interface{}{
				"section": section, "source": meta.Source, "revision": meta.Revision, "values": values,
			})
		}
		_, _ = fmt.Fprintf(out, "%s (source %s, revision %d)\n", section, orDash(meta.Source), meta.Revision)
		for _, k := range keys {
			raw, ok := values[k]
			if !ok {
				_, _ = fmt.Fprintf(out, "  %s: (not set)\n", k)
				continue
			}
			text, err := formatSettingsValue(raw)
			if err != nil {
				return err
			}
			if strings.Contains(text, "\n") {
				_, _ = fmt.Fprintf(out, "  %s:\n    %s\n", k, strings.ReplaceAll(text, "\n", "\n    "))
			} else {
				_, _ = fmt.Fprintf(out, "  %s: %s\n", k, text)
			}
		}
		return nil

	default:
		if format != "" {
			return writeStructured(out, format, cfg)
		}
		if cfg.SettingsTier != "" {
			_, _ = fmt.Fprintf(out, "Settings stored in: %s\n", cfg.SettingsTier)
		}
		if len(cfg.SectionMetadata) == 0 {
			_, _ = fmt.Fprintln(out, "The Hub reports no per-section metadata; use --format yaml to see every value.")
			return nil
		}
		names := make([]string, 0, len(cfg.SectionMetadata))
		for n := range cfg.SectionMetadata {
			names = append(names, n)
		}
		sort.Strings(names)
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "SECTION\tSOURCE\tREVISION\tUPDATED\tBY")
		for _, n := range names {
			m := cfg.SectionMetadata[n]
			updated := "-"
			if m.UpdatedAt != nil {
				updated = clitime.Format(*m.UpdatedAt, clitime.Minute)
			}
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\n", n, orDash(m.Source), m.Revision, updated, orDash(m.UpdatedBy))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
		_, _ = fmt.Fprintln(out, "\nShow values with 'scion hub settings get --section NAME' or 'scion hub settings get KEY'.")
		return nil
	}
}

// srvcfgWriteOptions tunes runHubSettingsWrite.
type srvcfgWriteOptions struct {
	Plan   settingsPlanOptions
	DryRun bool
	// File is a settings document to read instead of changes.
	File  string
	Stdin io.Reader
}

func runHubSettingsWrite(ctx context.Context, svc hubclient.AdminSettingsService, out, errw io.Writer, changes []settingsChange, opts srvcfgWriteOptions) error {
	format, err := settingsOutputFormat()
	if err != nil {
		return err
	}
	if opts.Stdin == nil {
		opts.Stdin = os.Stdin
	}
	schema, err := svc.GetServerConfigSchema(ctx)
	if err != nil {
		return reportSettingsError(out, errw, format, err, settingsScopeHint{Scope: srvcfgReadScope})
	}
	ix := newSettingsKeyIndex(schema)
	if len(changes) == 0 && opts.File != "" {
		doc, err := readSettingsDocument(opts.File, opts.Stdin)
		if err != nil {
			return err
		}
		changes = settingsChangesFromDocument(doc, ix)
	}
	cfg, err := svc.GetServerConfig(ctx)
	if err != nil {
		return reportSettingsError(out, errw, format, err, settingsScopeHint{Scope: srvcfgReadScope})
	}
	plan, err := buildSettingsWrite(cfg, ix, changes, opts.Plan)
	if err != nil {
		return err
	}
	if len(plan.Unlisted) > 0 {
		_, _ = fmt.Fprintf(errw, "Note: sending keys the Hub schema does not list: %s\n", strings.Join(plan.Unlisted, ", "))
	}

	if opts.DryRun {
		body := make(map[string]interface{}, len(plan.Body)+1)
		for k, v := range plan.Body {
			body[k] = v
		}
		body = maskSensitiveSettings(body).(map[string]interface{})
		if len(plan.Revisions) > 0 {
			body["expected_revisions"] = plan.Revisions
		}
		if format == "yaml" {
			return writeYAMLValue(out, body)
		}
		return writeStructured(out, "json", body)
	}

	res, err := svc.UpdateServerConfig(ctx, plan.Body, plan.Revisions)
	if err != nil {
		return reportSettingsError(out, errw, format, err, settingsScopeHint{Scope: srvcfgWriteScope})
	}
	if format != "" {
		return writeStructured(out, format, res)
	}
	printServerConfigSaveResult(out, errw, res, ix)
	return nil
}

func printServerConfigSaveResult(out, errw io.Writer, res *hubclient.ServerConfigSaveResult, ix *settingsKeyIndex) {
	written := res.Reload.Applied
	switch {
	case len(written) == 0 && len(res.Unchanged) > 0:
		_, _ = fmt.Fprintf(out, "No change: %s already had these values.\n", strings.Join(res.Unchanged, ", "))
	case len(written) > 0:
		_, _ = fmt.Fprintf(out, "Saved: %s.\n", strings.Join(written, ", "))
		if len(res.Unchanged) > 0 {
			_, _ = fmt.Fprintf(out, "Unchanged: %s.\n", strings.Join(res.Unchanged, ", "))
		}
	default:
		_, _ = fmt.Fprintln(out, "Saved.")
	}
	if len(res.Reload.AppliedKeys) > 0 {
		_, _ = fmt.Fprintf(out, "In effect now: %s\n", strings.Join(res.Reload.AppliedKeys, ", "))
	}
	if len(res.Reload.PendingRestart) > 0 {
		_, _ = fmt.Fprintf(out, "Takes effect after a Hub restart: %s\n", strings.Join(res.Reload.PendingRestart, ", "))
	}
	for _, s := range res.Reload.Skipped {
		_, _ = fmt.Fprintf(out, "Saved but not in use: %s (%s)\n", s.Key, s.Reason)
	}
	if len(res.FileKeys) > 0 {
		_, _ = fmt.Fprintf(out, "Written to the Hub's settings file: %s\n", strings.Join(res.FileKeys, ", "))
	}
	for _, w := range res.Warnings {
		_, _ = fmt.Fprintf(errw, "Warning: %s\n", w)
	}
}

func runHubSettingsReset(ctx context.Context, svc hubclient.AdminSettingsService, out, errw io.Writer, section string, yes bool) error {
	format, err := settingsOutputFormat()
	if err != nil {
		return err
	}
	prompt := fmt.Sprintf("Delete the saved values of section %q, so the Hub uses its startup configuration for it?", section)
	if !hubsync.ConfirmAction(prompt, false, yes) {
		return fmt.Errorf("reset cancelled")
	}
	if err := svc.ResetServerConfigSection(ctx, section); err != nil {
		return reportSettingsError(out, errw, format, err, settingsScopeHint{Scope: srvcfgWriteScope, Retry: srvcfgResetRetryHint})
	}
	if format != "" {
		return writeStructured(out, format, map[string]interface{}{"status": "reset", "section": section})
	}
	_, _ = fmt.Fprintf(out, "Section %s reset to the Hub's startup configuration.\n", section)
	return nil
}

func runHubSettingsSchema(ctx context.Context, svc hubclient.AdminSettingsService, out io.Writer, section string) error {
	format, err := settingsOutputFormat()
	if err != nil {
		return err
	}
	schema, err := svc.GetServerConfigSchema(ctx)
	if err != nil {
		return reportSettingsError(out, os.Stderr, format, err, settingsScopeHint{Scope: srvcfgReadScope})
	}
	if section != "" {
		sec, ok := schema[section]
		if !ok {
			return fmt.Errorf("no settings section %q", section)
		}
		var v interface{}
		if len(sec.Schema) > 0 {
			if err := json.Unmarshal(sec.Schema, &v); err != nil {
				return err
			}
		}
		if format == "yaml" {
			return writeYAMLValue(out, v)
		}
		return writeStructured(out, "json", v)
	}
	if format != "" {
		return writeStructured(out, format, schema)
	}
	names := make([]string, 0, len(schema))
	for n := range schema {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		sec := schema[n]
		restart := map[string]bool{}
		for _, r := range sec.RestartRequired {
			restart[r] = true
		}
		switch {
		case n == projectDefaultsSection:
			_, _ = fmt.Fprintf(out, "%s  (use 'scion hub project-defaults')\n", n)
		case len(sec.KoanfPaths) == 0:
			_, _ = fmt.Fprintf(out, "%s  (not in the server configuration; managed by its own command or page)\n", n)
			continue
		default:
			_, _ = fmt.Fprintln(out, n)
		}
		keys := append([]string(nil), sec.KoanfPaths...)
		sort.Strings(keys)
		for _, k := range keys {
			if restart[k] {
				_, _ = fmt.Fprintf(out, "  %s  (restart)\n", k)
			} else {
				_, _ = fmt.Fprintf(out, "  %s\n", k)
			}
		}
	}
	return nil
}

func init() {
	hubCmd.AddCommand(hubSettingsCmd)
	hubSettingsCmd.AddCommand(hubSettingsGetCmd, hubSettingsSetCmd, hubSettingsUnsetCmd, hubSettingsSchemaCmd)

	hubSettingsGetCmd.Flags().StringVar(&srvcfgGetSection, "section", "", "Show every key of this section")

	hubSettingsSetCmd.Flags().StringVar(&srvcfgSetFile, "file", "", "Read a YAML or JSON document of keys to change ('-' for stdin)")
	hubSettingsSetCmd.Flags().BoolVar(&srvcfgSetString, "string", false, "Send every VALUE as text, without reading it as YAML")
	for _, c := range []*cobra.Command{hubSettingsSetCmd, hubSettingsUnsetCmd} {
		c.Flags().BoolVar(&srvcfgNoValidate, "no-validate", false, "Send keys the Hub's schema does not list, and let the Hub decide")
		c.Flags().BoolVar(&srvcfgDryRun, "dry-run", false, "Print the request body, with its expected revisions, without sending it (values of secret keys are masked)")
		c.Flags().StringArrayVar(&srvcfgRevisions, "revision", nil, "Expected revision of a section, as SECTION=N (repeatable); default: the revision just read")
	}

	hubSettingsUnsetCmd.Flags().StringVar(&srvcfgUnsetSection, "section", "", "Section to reset (with --reset)")
	hubSettingsUnsetCmd.Flags().BoolVar(&srvcfgUnsetReset, "reset", false, "Delete the section's saved values (with --section)")
	hubSettingsUnsetCmd.Flags().BoolVarP(&srvcfgUnsetYes, "yes", "y", false, "Answer Yes to the confirmation prompt (required to confirm when stdin is not a terminal)")
}
