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
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// Restore of deleted built-in harness configs and templates
// (ptone/scion#3544). Hub-only: the hub re-creates the global rows from its
// own embedded catalog. Mode availability (D6): assistant yes, agent no —
// neither command is in agentAllowed (cli_mode.go).

var harnessConfigRestoreCmd = &cobra.Command{
	Use:   "restore <name>... | --all",
	Short: "Restore deleted built-in harness configs on the Hub",
	Long: `Re-creates deleted built-in harness configs on the Hub from the Hub's
embedded defaults. A deleted built-in is not re-created by a Hub restart or
upgrade; this command is how you get it back.

Built-ins that still exist are left unchanged and reported as already
present. To refresh an existing Hub harness config from its source, use
'scion harness-config update' (re-import); 'scion harness-config reset' resets
the local copy to the embedded defaults.`,
	Example: `  scion harness-config restore claude
  scion harness-config restore claude codex
  scion harness-config restore --all`,
	Args: namesOrAllArgs("harness-config", false),
	RunE: func(cmd *cobra.Command, args []string) error {
		all, _ := cmd.Flags().GetBool("all")
		hubCtx, err := restoreHubContext()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		return runBuiltinRestore(ctx, os.Stdout, "harness-config",
			hubCtx.Client.HarnessConfigs().Restore, args, all)
	},
}

var templatesRestoreCmd = &cobra.Command{
	Use:   "restore [default] | --all",
	Short: "Restore the deleted built-in default template on the Hub",
	Long: `Re-creates the deleted built-in template ("default") on the Hub from the
Hub's embedded defaults. A deleted built-in is not re-created by a Hub
restart or upgrade; this command is how you get it back.

With no argument, every built-in template is restored (today that is only
"default"). A template that still exists is left unchanged and reported as
already present.`,
	Example: `  scion templates restore
  scion templates restore default
  scion templates restore --all`,
	Args: namesOrAllArgs("template", true),
	RunE: func(cmd *cobra.Command, args []string) error {
		all, _ := cmd.Flags().GetBool("all")
		if len(args) == 0 {
			all = true
		}
		hubCtx, err := restoreHubContext()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		return runBuiltinRestore(ctx, os.Stdout, "template",
			hubCtx.Client.Templates().Restore, args, all)
	},
}

// namesOrAllArgs accepts one or more names or --all, not both. With
// allowNone, no arguments is also accepted (the command treats it as --all).
func namesOrAllArgs(noun string, allowNone bool) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		all, _ := cmd.Flags().GetBool("all")
		if all && len(args) > 0 {
			return fmt.Errorf("cannot specify both %s names and --all", noun)
		}
		if !all && len(args) == 0 && !allowNone {
			return fmt.Errorf("requires at least one %s name or --all", noun)
		}
		return nil
	}
}

// restoreHubContext resolves the Hub connection the same way the Hub-only
// delete commands do. Restore needs a Hub; there is no local fallback.
func restoreHubContext() (*HubContext, error) {
	var gp string
	if projectPath != "" {
		if resolved, err := config.GetResolvedProjectDir(projectPath); err == nil {
			gp = resolved
		}
	} else if projectDir, err := config.GetResolvedProjectDir(""); err == nil {
		gp = projectDir
	}

	hubCtx, err := CheckHubAvailabilityWithOptions(gp, true)
	if err != nil {
		return nil, err
	}
	if hubCtx == nil {
		return nil, fmt.Errorf("hub integration is not enabled, configure via 'scion config set hub.enabled true' and 'scion config set hub.endpoint <url>'")
	}
	PrintUsingHub(hubCtx.Endpoint)
	return hubCtx, nil
}

type builtinRestoreFunc func(ctx context.Context, req *hubclient.RestoreBuiltinsRequest) (*hubclient.RestoreBuiltinsResponse, error)

// runBuiltinRestore sends the restore request and reports the result.
func runBuiltinRestore(ctx context.Context, out io.Writer, noun string, restore builtinRestoreFunc, names []string, all bool) error {
	req := &hubclient.RestoreBuiltinsRequest{All: all}
	if !all {
		req.Names = names
	}
	resp, err := restore(ctx, req)
	if err != nil {
		return fmt.Errorf("failed to restore built-in %ss: %w", noun, err)
	}
	if resp == nil {
		// apiclient.DecodeResponse returns (nil, nil) for a 204; the restore
		// routes never send one, but do not dereference a nil response.
		return fmt.Errorf("failed to restore built-in %ss: hub returned an empty response", noun)
	}

	msg := builtinRestoreSummary(noun, resp)
	if isJSONOutput() {
		return outputJSON(ActionResult{
			Status:  "success",
			Command: noun + " restore",
			Message: msg,
			Details: map[string]interface{}{
				"restored":       resp.Restored,
				"alreadyPresent": resp.AlreadyPresent,
			},
		})
	}
	_, _ = fmt.Fprintln(out, msg)
	return nil
}

func builtinRestoreSummary(noun string, resp *hubclient.RestoreBuiltinsResponse) string {
	var parts []string
	if len(resp.Restored) > 0 {
		parts = append(parts, fmt.Sprintf("Restored %s(s): %s.", noun, strings.Join(resp.Restored, ", ")))
	}
	if len(resp.AlreadyPresent) > 0 {
		parts = append(parts, fmt.Sprintf("Already present, unchanged: %s.", strings.Join(resp.AlreadyPresent, ", ")))
	}
	if len(parts) == 0 {
		return fmt.Sprintf("No built-in %ss to restore.", noun)
	}
	return strings.Join(parts, " ")
}

func init() {
	harnessConfigRestoreCmd.Flags().Bool("all", false, "Restore every built-in harness config that is missing")
	harnessConfigCmd.AddCommand(harnessConfigRestoreCmd)

	templatesRestoreCmd.Flags().Bool("all", false, "Restore every built-in template that is missing")
	templatesCmd.AddCommand(templatesRestoreCmd)
	// The singular 'scion template restore' alias is registered with the
	// rest of the alias tree in templates.go.
}
