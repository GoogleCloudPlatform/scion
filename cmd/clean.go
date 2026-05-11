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
	"os"
	"path/filepath"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/spf13/cobra"
)

var (
	cleanSkipHubCheck bool
)

// cleanCmd represents the clean command
var cleanCmd = &cobra.Command{
	Use:   "clean",
	Short: "Remove scion from a project",
	Long: `Remove the scion project configuration from the current project or global location.

This command will:
1. Check if the project is linked to a Hub (unless --skip-hub-check is set)
2. Offer to unlink from Hub if linked
3. Remove the local .scion directory

This is the reverse of 'scion init'.

Examples:
  # Clean the current project
  scion clean

  # Clean the global project
  scion clean --global

  # Clean without checking Hub status
  scion clean --skip-hub-check

  # Non-interactive mode (auto-confirm all prompts)
  scion clean --yes`,
	RunE: runClean,
}

func init() {
	rootCmd.AddCommand(cleanCmd)
	cleanCmd.Flags().BoolVar(&cleanSkipHubCheck, "skip-hub-check", false, "Skip Hub connectivity check")
}

func runClean(cmd *cobra.Command, args []string) error {
	// Resolve grove path
	gp := projectPath
	if gp == "" && globalMode {
		gp = "global"
	}

	resolvedPath, isGlobal, err := config.ResolveProjectPath(gp)
	if err != nil {
		return fmt.Errorf("failed to resolve project path: %w", err)
	}

	// Check if .scion directory exists
	if _, err := os.Stat(resolvedPath); os.IsNotExist(err) {
		return fmt.Errorf("no scion project found at %s", resolvedPath)
	}

	// Get grove name for display
	var projectName string
	if isGlobal {
		projectName = "global"
	} else {
		gitRemote := util.GetGitRemoteDir(filepath.Dir(resolvedPath))
		if gitRemote != "" {
			projectName = util.ExtractRepoName(gitRemote)
		} else {
			projectName = config.GetProjectName(resolvedPath)
		}
	}

	// Load settings
	settings, err := config.LoadSettings(resolvedPath)
	if err != nil {
		// Settings might not exist, continue with minimal info
		util.Debugf("Warning: failed to load settings: %v", err)
	}

	// Determine if we should check Hub status
	shouldCheckHub := !cleanSkipHubCheck

	// If Hub is explicitly disabled in settings, ask if user wants to check anyway
	if shouldCheckHub && settings != nil && !settings.IsHubEnabled() && !noHub {
		if hubsync.ShowCheckHubAnywayPrompt(autoConfirm) {
			shouldCheckHub = true
		} else {
			shouldCheckHub = false
		}
	}

	// Check Hub status
	hubLinked := false
	hubReachable := false

	if shouldCheckHub && settings != nil {
		endpoint := GetHubEndpoint(settings)
		if endpoint != "" {
			// Try to create Hub client and check connectivity
			client, clientErr := getHubClient(settings)
			if clientErr == nil {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()

				_, healthErr := client.Health(ctx)
				if healthErr == nil {
					hubReachable = true

					// Check if grove is registered on Hub
					lookupID := settings.GetHubProjectID()
					if lookupID == "" {
						lookupID = settings.ProjectID
					}
					if lookupID != "" {
						linked, _ := isProjectLinked(ctx, client, lookupID)
						hubLinked = linked
					}
				} else {
					// Hub unreachable - warn user
					fmt.Println()
					fmt.Printf("Warning: Hub at %s is not reachable.\n", endpoint)
					fmt.Println("Cleaning this project may leave it orphaned on the Hub.")
					fmt.Println("You may need to manually remove it from the Hub later.")
					fmt.Println()

					if !hubsync.ConfirmAction("Continue anyway?", false, autoConfirm) {
						return fmt.Errorf("clean cancelled")
					}
				}
			}
		}
	}

	// If linked to Hub, offer to unlink first
	if hubLinked && hubReachable {
		fmt.Println()
		fmt.Printf("Project '%s' is linked to the Hub.\n", projectName)

		if hubsync.ShowCleanUnlinkPrompt(projectName, autoConfirm) {
			// Unlink from Hub
			if err := config.UpdateSetting(resolvedPath, "hub.enabled", "false", isGlobal); err != nil {
				return fmt.Errorf("failed to unlink from Hub: %w", err)
			}
			fmt.Printf("Project '%s' has been unlinked from the Hub.\n", projectName)
			fmt.Println("The project and its agents remain on the Hub for other brokers.")
		}
		// Note: We don't actually need to do anything on the hub side since we're just
		// unlinking locally. The project record on Hub will remain for other brokers.
	}

	// Show final confirmation to remove .scion directory
	if !hubsync.ShowCleanConfirmPrompt(projectName, resolvedPath, isGlobal, autoConfirm) {
		return fmt.Errorf("clean cancelled")
	}

	// Remove the .scion directory
	if err := os.RemoveAll(resolvedPath); err != nil {
		return fmt.Errorf("failed to remove project directory: %w", err)
	}

	if isJSONOutput() {
		return outputJSON(ActionResult{
			Status:  "success",
			Command: "clean",
			Message: fmt.Sprintf("Project '%s' has been removed.", projectName),
			Details: map[string]interface{}{
				"project": projectName,
				"path":    resolvedPath,
				"global":  isGlobal,
			},
		})
	}

	fmt.Println()
	fmt.Printf("Project '%s' has been removed.\n", projectName)
	if isGlobal {
		fmt.Println("The global scion configuration has been cleaned.")
	} else {
		fmt.Println("The project scion configuration has been cleaned.")
	}
	fmt.Println("Run 'scion init' to create a new project.")

	return nil
}
