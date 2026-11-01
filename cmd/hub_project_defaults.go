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
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

const (
	projDefaultsReadScope  = "hub_project_defaults:read"
	projDefaultsWriteScope = "hub_project_defaults:update"
)

// projDefaultsRetryHint is the revision-conflict guidance of the
// project-defaults commands, which take no revision.
const projDefaultsRetryHint = "The project defaults changed while the Hub saved them. Re-run the command to apply the change to the current values."

var projDefaultsSetString bool

var hubProjectDefaultsCmd = &cobra.Command{
	Use:   "project-defaults",
	Short: "View and change the Hub's defaults for new projects",
	Long: `View and change the defaults the Hub applies to new projects, for
example default_scratchpad (whether a new project gets a scratchpad shared
directory).

Needs a Hub admin login, or an access token on a hub boundary with the
hub_project_defaults:read scope (get) or hub_project_defaults:update scope
(set, unset).

A change made by someone else while the Hub saves yours is reported as a
conflict; re-run the command to retry.`,
}

var hubProjectDefaultsGetCmd = &cobra.Command{
	Use:   "get [KEY]",
	Short: "Show the project defaults, or one of them",
	Args:  cobra.MaximumNArgs(1),
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
		return runProjectDefaultsGet(ctx, svc, cmd.OutOrStdout(), cmd.ErrOrStderr(), key)
	},
}

var hubProjectDefaultsSetCmd = &cobra.Command{
	Use:   "set KEY VALUE [KEY VALUE ...]",
	Short: "Change project defaults",
	Long: `Change one or more project defaults. VALUE is read as YAML (true, 42,
"text"); --string sends it as text.

Example:
  scion hub project-defaults set default_scratchpad false`,
	RunE: func(cmd *cobra.Command, args []string) error {
		changes, err := setArgsToChanges(args, "", projDefaultsSetString)
		if err != nil {
			return err
		}
		svc, ctx, cancel, err := adminSettingsClient(cmd)
		if err != nil {
			return err
		}
		defer cancel()
		return runProjectDefaultsWrite(ctx, svc, cmd.OutOrStdout(), cmd.ErrOrStderr(), changes)
	},
}

var hubProjectDefaultsUnsetCmd = &cobra.Command{
	Use:   "unset KEY [KEY ...]",
	Short: "Clear project defaults, so the Hub's built-in default applies",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		changes := make([]settingsChange, 0, len(args))
		for _, k := range args {
			changes = append(changes, settingsChange{Key: k, Unset: true})
		}
		svc, ctx, cancel, err := adminSettingsClient(cmd)
		if err != nil {
			return err
		}
		defer cancel()
		return runProjectDefaultsWrite(ctx, svc, cmd.OutOrStdout(), cmd.ErrOrStderr(), changes)
	},
}

// projectDefaultsKey accepts "default_scratchpad" or
// "project_defaults.default_scratchpad".
func projectDefaultsKey(k string) string {
	return strings.TrimPrefix(strings.TrimSpace(k), projectDefaultsSection+".")
}

func runProjectDefaultsGet(ctx context.Context, svc hubclient.AdminSettingsService, out, errw io.Writer, key string) error {
	format, err := settingsOutputFormat()
	if err != nil {
		return err
	}
	cur, err := svc.GetProjectDefaults(ctx)
	if err != nil {
		return reportSettingsError(out, errw, format, err, settingsScopeHint{Scope: projDefaultsReadScope, Retry: projDefaultsRetryHint})
	}
	if key != "" {
		k := projectDefaultsKey(key)
		raw, ok := cur[k]
		if !ok {
			return fmt.Errorf("%q is not a project default (known: %s)", key, strings.Join(sortedRawKeys(cur), ", "))
		}
		if format != "" {
			return writeStructured(out, format, map[string]json.RawMessage{k: raw})
		}
		text, err := formatSettingsValue(raw)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintln(out, text)
		return nil
	}
	if format != "" {
		return writeStructured(out, format, cur)
	}
	return printRawMap(out, cur)
}

// runProjectDefaultsWrite saves the changes. Keys are checked against the
// keys the Hub returns, because the Hub ignores a key it does not know.
func runProjectDefaultsWrite(ctx context.Context, svc hubclient.AdminSettingsService, out, errw io.Writer, changes []settingsChange) error {
	format, err := settingsOutputFormat()
	if err != nil {
		return err
	}
	cur, err := svc.GetProjectDefaults(ctx)
	if err != nil {
		return reportSettingsError(out, errw, format, err, settingsScopeHint{Scope: projDefaultsReadScope, Retry: projDefaultsRetryHint})
	}
	body := make(map[string]interface{}, len(changes))
	for _, ch := range changes {
		k := projectDefaultsKey(ch.Key)
		if _, ok := cur[k]; !ok {
			return fmt.Errorf("%q is not a project default (known: %s)", ch.Key, strings.Join(sortedRawKeys(cur), ", "))
		}
		if _, dup := body[k]; dup {
			return fmt.Errorf("%q is given more than once", k)
		}
		if ch.Unset {
			body[k] = nil
		} else {
			body[k] = ch.Value
		}
	}
	res, err := svc.UpdateProjectDefaults(ctx, body)
	if err != nil {
		return reportSettingsError(out, errw, format, err, settingsScopeHint{Scope: projDefaultsWriteScope, Retry: projDefaultsRetryHint})
	}
	if format != "" {
		return writeStructured(out, format, res)
	}
	_, _ = fmt.Fprintln(out, "Saved. Project defaults now:")
	return printRawMap(out, res)
}

func sortedRawKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func printRawMap(out io.Writer, m map[string]json.RawMessage) error {
	for _, k := range sortedRawKeys(m) {
		text, err := formatSettingsValue(m[k])
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(out, "  %s: %s\n", k, text)
	}
	return nil
}

func init() {
	hubCmd.AddCommand(hubProjectDefaultsCmd)
	hubProjectDefaultsCmd.AddCommand(hubProjectDefaultsGetCmd, hubProjectDefaultsSetCmd, hubProjectDefaultsUnsetCmd)
	hubProjectDefaultsSetCmd.Flags().BoolVar(&projDefaultsSetString, "string", false, "Send every VALUE as text, without reading it as YAML")
}
