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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

var harnessConfigUpdateCmd = &cobra.Command{
	Use:   "update [name]",
	Short: "Re-import a harness-config from its source URL",
	Long: `Re-imports a harness-config from its stored source URL, pulling the latest
version from the remote source.

If --url is provided, it overrides (and updates) the stored source URL.
Use --all to re-import all harness-configs that have a stored source URL.

Examples:
  scion harness-config update my-claude
  scion harness-config update my-claude --url https://github.com/org/repo/tree/main/harness-configs/claude
  scion harness-config update --all`,
	Args: cobra.MaximumNArgs(1),
	RunE: runHarnessConfigUpdate,
}

func runHarnessConfigUpdate(cmd *cobra.Command, args []string) error {
	urlOverride, _ := cmd.Flags().GetString("url")
	all, _ := cmd.Flags().GetBool("all")

	if len(args) == 0 && !all {
		return fmt.Errorf("specify a harness-config name or use --all")
	}
	if all && urlOverride != "" {
		return fmt.Errorf("--all and --url cannot be used together")
	}

	var gp string
	if projectPath != "" {
		resolved, err := config.GetResolvedProjectDir(projectPath)
		if err != nil {
			return fmt.Errorf("failed to resolve project path %q: %w", projectPath, err)
		}
		gp = resolved
	} else if projectDir, err := config.GetResolvedProjectDir(""); err == nil {
		gp = projectDir
	}

	hubCtx, err := CheckHubAvailabilityWithOptions(gp, true)
	if err != nil {
		return err
	}
	if hubCtx == nil {
		return fmt.Errorf("Hub is not available. The update command requires a Hub connection.")
	}

	PrintUsingHub(hubCtx.Endpoint)

	ctx, cancel := context.WithTimeout(cmd.Context(), 120*time.Second)
	defer cancel()

	if all {
		return updateAllHarnessConfigs(ctx, hubCtx)
	}

	name := args[0]
	return updateSingleHarnessConfig(ctx, hubCtx, name, urlOverride)
}

func updateSingleHarnessConfig(ctx context.Context, hubCtx *HubContext, name, urlOverride string) error {
	resp, err := hubCtx.Client.HarnessConfigs().List(ctx, &hubclient.ListHarnessConfigsOptions{
		Name:   name,
		Status: "active",
	})
	if err != nil {
		return fmt.Errorf("failed to search Hub: %w", err)
	}
	if resp == nil {
		return fmt.Errorf("harness-config %q not found on Hub", name)
	}

	var match *hubclient.HarnessConfig
	for i := range resp.HarnessConfigs {
		if resp.HarnessConfigs[i].Name == name || resp.HarnessConfigs[i].Slug == name {
			match = &resp.HarnessConfigs[i]
			break
		}
	}
	if match == nil {
		return fmt.Errorf("harness-config %q not found on Hub", name)
	}

	sourceURL := urlOverride
	if sourceURL == "" {
		sourceURL = match.SourceURL
	}
	if sourceURL == "" {
		return fmt.Errorf("harness-config %q has no stored source URL. Use --url to specify one.", name)
	}

	if !isJSONOutput() {
		fmt.Printf("Updating %q from %s...\n", name, sourceURL)
	}

	result, err := hubCtx.Client.HarnessConfigs().Reimport(ctx, match.ID, urlOverride)
	if err != nil {
		return fmt.Errorf("reimport failed: %w", err)
	}
	if result == nil {
		return fmt.Errorf("reimport returned no result for %q", name)
	}

	if isJSONOutput() {
		return outputJSON(ActionResult{
			Status:  "success",
			Command: "harness-config update",
			Message: fmt.Sprintf("Updated %d harness-config(s)", result.Count),
			Details: map[string]interface{}{
				"name":     name,
				"imported": result.HarnessConfigs,
				"count":    result.Count,
			},
		})
	}

	fmt.Printf("Updated %d harness-config(s): %v\n", result.Count, result.HarnessConfigs)
	return nil
}

func updateAllHarnessConfigs(ctx context.Context, hubCtx *HubContext) error {
	resp, err := hubCtx.Client.HarnessConfigs().List(ctx, &hubclient.ListHarnessConfigsOptions{
		Status: "active",
	})
	if err != nil {
		return fmt.Errorf("failed to list harness-configs: %w", err)
	}
	if resp == nil {
		return fmt.Errorf("no harness-configs found")
	}

	var updated, skipped, failed int
	jsonOut := isJSONOutput()
	for _, hc := range resp.HarnessConfigs {
		if hc.SourceURL == "" {
			skipped++
			continue
		}
		if !jsonOut {
			fmt.Printf("Updating %q from %s...\n", hc.Name, hc.SourceURL)
		}
		_, err := hubCtx.Client.HarnessConfigs().Reimport(ctx, hc.ID, "")
		if err != nil {
			if !jsonOut {
				fmt.Printf("  Failed: %s\n", err)
			}
			failed++
			continue
		}
		if !jsonOut {
			fmt.Printf("  Done.\n")
		}
		updated++
	}

	if jsonOut {
		return outputJSON(ActionResult{
			Status:  "success",
			Command: "harness-config update --all",
			Message: fmt.Sprintf("Updated %d, skipped %d (no source URL), failed %d", updated, skipped, failed),
		})
	}

	fmt.Printf("\nUpdated %d, skipped %d (no source URL), failed %d\n", updated, skipped, failed)
	return nil
}

func init() {
	harnessConfigUpdateCmd.Flags().String("url", "", "Override the stored source URL")
	harnessConfigUpdateCmd.Flags().Bool("all", false, "Update all harness-configs that have a stored source URL")
}
