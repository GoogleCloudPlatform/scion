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

	"github.com/ptone/scion-agent/pkg/config"
	"github.com/ptone/scion-agent/pkg/hubsync"
	"github.com/ptone/scion-agent/pkg/util"
	"github.com/spf13/cobra"
)

var (
	cleanSkipHubCheck bool
)

// cleanCmd represents the clean command
var cleanCmd = &cobra.Command{
	Use:   "clean",
	Short: "Remove scion from a grove",
	Long: `Remove the scion grove configuration from the current project or global location.

This command will:
1. Check if the grove is linked to a Hub (unless --skip-hub-check is set)
2. Offer to unlink from Hub if linked
3. Remove the local .scion directory

This is the reverse of 'scion init'.

Examples:
  # Clean the current project grove
  scion clean

  # Clean the global grove
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
	gp := grovePath
	if gp == "" && globalMode {
		gp = "global"
	}

	resolvedPath, isGlobal, err := config.ResolveGrovePath(gp)
	if err != nil {
		return fmt.Errorf("failed to resolve grove path: %w", err)
	}

	// Check if .scion directory exists
	if _, err := os.Stat(resolvedPath); os.IsNotExist(err) {
		return fmt.Errorf("no scion grove found at %s", resolvedPath)
	}

	// Get grove name for display
	var groveName string
	if isGlobal {
		groveName = "global"
	} else {
		gitRemote := util.GetGitRemoteDir(filepath.Dir(resolvedPath))
		if gitRemote != "" {
			groveName = util.ExtractRepoName(gitRemote)
		} else {
			groveName = filepath.Base(filepath.Dir(resolvedPath))
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
					if settings.GroveID != "" {
						linked, _ := isGroveLinked(ctx, client, settings.GroveID)
						hubLinked = linked
					}
				} else {
					// Hub unreachable - warn user
					fmt.Println()
					fmt.Printf("Warning: Hub at %s is not reachable.\n", endpoint)
					fmt.Println("Cleaning this grove may leave it orphaned on the Hub.")
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
		fmt.Printf("Grove '%s' is linked to the Hub.\n", groveName)

		if hubsync.ShowCleanUnlinkPrompt(groveName, autoConfirm) {
			// Unlink from Hub
			if err := config.UpdateSetting(resolvedPath, "hub.enabled", "false", isGlobal); err != nil {
				return fmt.Errorf("failed to unlink from Hub: %w", err)
			}
			fmt.Printf("Grove '%s' has been unlinked from the Hub.\n", groveName)
			fmt.Println("The grove and its agents remain on the Hub for other brokers.")
		}
		// Note: We don't actually need to do anything on the hub side since we're just
		// unlinking locally. The grove record on Hub will remain for other brokers.
	}

	// Show final confirmation to remove .scion directory
	if !hubsync.ShowCleanConfirmPrompt(groveName, resolvedPath, isGlobal, autoConfirm) {
		return fmt.Errorf("clean cancelled")
	}

	// Remove the .scion directory
	if err := os.RemoveAll(resolvedPath); err != nil {
		return fmt.Errorf("failed to remove grove directory: %w", err)
	}

	fmt.Println()
	fmt.Printf("Grove '%s' has been removed.\n", groveName)
	if isGlobal {
		fmt.Println("The global scion configuration has been cleaned.")
	} else {
		fmt.Println("The project scion configuration has been cleaned.")
	}
	fmt.Println("Run 'scion init' to create a new grove.")

	return nil
}
