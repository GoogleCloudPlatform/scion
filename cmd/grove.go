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

	"bufio"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/spf13/cobra"
)

var globalInit bool
var initImageRegistry string

// groveCmd represents the grove command
var groveCmd = &cobra.Command{
	Use:     "grove",
	Aliases: []string{"group"},
	Short:   "Manage scion groves (agent groups)",
	Long:    `A grove is the grouping construct for a set of agents. The .scion folder represents a grove.`,
}

// groveInitCmd represents the init subcommand for grove
var groveInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Initialize a new grove",
	Long: `Initialize a new grove by creating the .scion directory structure
and seeding the default template. 

By default, it initializes in:
- The root of the current git repo if run inside a repo
- The current directory

With --global, it initializes in the user's home folder.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		harnesses := harness.All()

		if globalInit || machineInit {
			if !isJSONOutput() {
				fmt.Println("Initializing global scion directory...")
			}

			// Resolve image registry: flag > existing settings > prompt > skip
			registryValue := initImageRegistry
			if registryValue == "" {
				// Check if existing global settings already have a registry configured
				if globalDir, err := config.GetGlobalDir(); err == nil {
					if vs, err := config.LoadVersionedSettings(globalDir); err == nil {
						registryValue = vs.ImageRegistry
					}
				}
			}
			if registryValue == "" && !isJSONOutput() {
				registryValue = promptImageRegistry()
			}

			opts := config.InitMachineOpts{ImageRegistry: registryValue, Force: machineInitForce}
			if err := config.InitMachine(harnesses, opts); err != nil {
				return fmt.Errorf("failed to initialize global config: %w", err)
			}

			if isJSONOutput() {
				details := map[string]interface{}{"global": true, "machine": true}
				if registryValue != "" {
					details["image_registry"] = registryValue
				}
				return outputJSON(ActionResult{
					Status:  "success",
					Command: "grove init",
					Message: "scion grove successfully initialized.",
					Details: details,
				})
			}

			fmt.Println("scion grove successfully initialized.")
			if registryValue != "" {
				fmt.Printf("Image registry: %s\n", registryValue)
			} else {
				fmt.Println()
				fmt.Println("Note: image_registry is not configured. Agents cannot start without it.")
				fmt.Println("  Build images first — see image-build/README.md")
				fmt.Println("  Then run: scion config set --global image_registry <your-registry>")
			}

			// Prompt for Hub registration if Hub is configured
			if err := promptHubRegistration(true); err != nil {
				// Non-fatal: just log the error
				fmt.Printf("Note: %v\n", err)
			}

			return nil
		}

		// Check if ~/.scion/ exists; error if not since global grove is required
		if globalDir, err := config.GetGlobalDir(); err == nil {
			if _, err := os.Stat(globalDir); os.IsNotExist(err) {
				return fmt.Errorf("global scion directory (~/.scion/) does not exist.\nRun 'scion init --machine' first to set up the global configuration")
			}
		}

		// Check for existing grove at or above current directory
		if _, rootDir, found := config.GetEnclosingProjectPath(); found {
			wd, _ := os.Getwd()
			if filepath.Clean(wd) == filepath.Clean(rootDir) {
				// Re-running init in an existing grove is allowed — it ensures
				// the grove structure is intact (dirs, gitignore, etc.).
				if !isJSONOutput() {
					fmt.Println("Grove already initialized. Ensuring integrity...")
				}
				// Fall through to InitProject which is idempotent
			} else if !isJSONOutput() {
				// Inform user about parent grove — nested groves are allowed
				fmt.Printf("Note: parent grove exists at %s. Initializing nested grove.\n", rootDir)
			}
		}

		// Determine target directory
		targetDir, err := config.GetTargetProjectDir()
		if err != nil {
			return fmt.Errorf("failed to determine project directory: %w", err)
		}

		// Check if we're in a subdirectory of a git repo
		wd, _ := os.Getwd()
		if util.IsGitRepo() {
			repoRoot, err := util.RepoRoot()
			if err == nil && repoRoot != "" {
				expectedTarget := filepath.Join(repoRoot, config.DotScion)
				if targetDir == expectedTarget && wd != repoRoot {
					fmt.Printf("Note: Creating .scion at repository root (%s)\n", repoRoot)
				}
			}
		}

		if !isJSONOutput() {
			fmt.Println("Initializing scion project grove...")
		}
		if err := config.InitProject("", harnesses); err != nil {
			return fmt.Errorf("failed to initialize project grove: %w", err)
		}

		// Resolve the grove_id and save it to settings.
		// For non-git groves, targetDir (.scion) is now a marker file, so we must
		// resolve through it to the external config path. The grove-id is already
		// generated during InitProject — read it back rather than generating a new one.
		var projectID string
		markerPath := filepath.Join(filepath.Dir(targetDir), config.DotScion)
		if config.IsProjectMarkerFile(markerPath) {
			// Non-git grove: read grove-id from marker, save to external settings
			marker, err := config.ReadProjectMarker(markerPath)
			if err == nil {
				projectID = marker.ProjectID
				// grove_id is already written during initExternalGrove
			}
		} else {
			// Git grove: read grove-id from file, save to in-repo settings
			projectID, _ = config.ReadProjectID(targetDir)
			if projectID == "" {
				projectID = config.GenerateProjectIDForDir(filepath.Dir(targetDir))
			}
			if err := config.UpdateSetting(targetDir, "grove_id", projectID, false); err != nil {
				if !isJSONOutput() {
					fmt.Printf("Warning: failed to save grove_id: %v\n", err)
				}
			}
		}

		if isJSONOutput() {
			return outputJSON(ActionResult{
				Status:  "success",
				Command: "grove init",
				Message: "scion grove successfully initialized.",
				Details: map[string]interface{}{
					"groveId": projectID,
					"path":    targetDir,
				},
			})
		}

		fmt.Println("scion grove successfully initialized.")
		fmt.Printf("Grove ID: %s\n", projectID)

		// Prompt for Hub registration if Hub is configured
		if err := promptHubRegistration(false); err != nil {
			// Non-fatal: just log the error
			fmt.Printf("Note: %v\n", err)
		}

		return nil
	},
}

// promptHubRegistration checks if Hub is configured and prompts to register the grove.
func promptHubRegistration(isGlobal bool) error {
	// Skip if --no-hub is set
	if noHub {
		return nil
	}

	// Resolve grove path
	var gp string
	if isGlobal {
		gp = "global"
	}
	resolvedPath, _, err := config.ResolveProjectPath(gp)
	if err != nil {
		return nil // Silently skip if we can't resolve path
	}

	settings, err := config.LoadSettings(resolvedPath)
	if err != nil {
		return nil // Silently skip if we can't load settings
	}

	// Only prompt if Hub is explicitly enabled (not just configured with an endpoint)
	if !settings.IsHubEnabled() {
		return nil
	}

	// Step 1: Prompt to link grove to Hub
	if !hubsync.ShowInitLinkPrompt(autoConfirm) {
		return nil
	}

	// Create Hub client
	client, err := getHubClient(settings)
	if err != nil {
		return fmt.Errorf("failed to create Hub client: %w", err)
	}

	// Check health first
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := client.Health(ctx); err != nil {
		return fmt.Errorf("Hub is not responding: %w", err)
	}

	// Get grove info
	var projectName string
	var gitRemote string
	projectID := settings.ProjectID

	if isGlobal {
		projectName = "global"
	} else {
		gitRemote = util.GetGitRemote()
		if gitRemote != "" {
			projectName = util.ExtractRepoName(gitRemote)
		} else {
			projectName = config.GetProjectName(resolvedPath)
		}
	}

	// Register grove without broker info first
	req := &hubclient.RegisterProjectRequest{
		ID:        projectID,
		Name:      projectName,
		GitRemote: util.NormalizeGitRemote(gitRemote),
		Path:      resolvedPath,
	}

	ctxReg, cancelReg := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelReg()

	resp, err := client.Projects().Register(ctxReg, req)
	if err != nil {
		return fmt.Errorf("registration failed: %w", err)
	}

	// Enable Hub integration
	_ = config.UpdateSetting(resolvedPath, "hub.enabled", "true", isGlobal)

	if resp.Created {
		fmt.Printf("Created new grove on Hub: %s (ID: %s)\n", resp.Project.Name, resp.Project.ID)
	} else {
		fmt.Printf("Linked to existing grove on Hub: %s (ID: %s)\n", resp.Project.Name, resp.Project.ID)
	}
	// Store the hub grove ID separately if it differs from the local grove_id
	if resp.Project.ID != projectID {
		if err := config.UpdateSetting(resolvedPath, "hub.groveId", resp.Project.ID, isGlobal); err != nil {
			fmt.Printf("Warning: failed to save hub grove ID: %v\n", err)
		}
		projectID = resp.Project.ID
	}

	// Show any auto-provided brokers
	ctxProviders, cancelProviders := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelProviders()

	providersResp, err := client.Projects().ListProviders(ctxProviders, resp.Project.ID)
	if err == nil && providersResp != nil && len(providersResp.Providers) > 0 {
		fmt.Println()
		fmt.Println("Brokers providing for this grove:")
		for _, p := range providersResp.Providers {
			autoTag := ""
			if p.Status == "online" {
				autoTag = " (online)"
			}
			fmt.Printf("  - %s%s\n", p.BrokerName, autoTag)
		}
	}

	// Step 2: Check if this host is a registered broker and offer to add as provider
	localBrokerID, localBrokerName := getLocalBrokerInfo(settings)
	if localBrokerID != "" {
		// Check if this broker is already a provider
		alreadyProvider := false
		if providersResp != nil {
			for _, p := range providersResp.Providers {
				if p.BrokerID == localBrokerID {
					alreadyProvider = true
					break
				}
			}
		}

		if !alreadyProvider {
			fmt.Println()
			if hubsync.ShowInitProvidePrompt(localBrokerName, resp.Project.Name, autoConfirm) {
				// Add this broker as a provider
				ctxAdd, cancelAdd := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancelAdd()

				addReq := &hubclient.AddProviderRequest{
					BrokerID:  localBrokerID,
					LocalPath: resolvedPath,
				}

				_, err := client.Projects().AddProvider(ctxAdd, resp.Project.ID, addReq)
				if err != nil {
					fmt.Printf("Warning: failed to add broker as provider: %v\n", err)
				} else {
					fmt.Printf("Host registered as provider: %s\n", localBrokerName)
				}
			}
		}
	}

	return nil
}

// getLocalBrokerInfo returns the local broker ID and name if this host is registered as a broker.
func getLocalBrokerInfo(settings *config.Settings) (brokerID, brokerName string) {
	// First check brokercredentials store
	credStore := brokercredentials.NewStore("")
	creds, err := credStore.Load()
	if err == nil && creds != nil && creds.BrokerID != "" {
		brokerID = creds.BrokerID
	}

	// Fall back to global settings
	if brokerID == "" {
		globalDir, err := config.GetGlobalDir()
		if err == nil {
			globalSettings, err := config.LoadSettings(globalDir)
			if err == nil && globalSettings.Hub != nil && globalSettings.Hub.BrokerID != "" {
				brokerID = globalSettings.Hub.BrokerID
			}
		}
	}

	// Get hostname for display
	brokerName, _ = os.Hostname()
	if brokerName == "" {
		if brokerID != "" && len(brokerID) >= 8 {
			brokerName = brokerID[:8]
		} else {
			brokerName = "local-host"
		}
	}

	return brokerID, brokerName
}

// promptImageRegistry prompts the user for their container image registry path.
// Returns the entered value or empty string if skipped.
func promptImageRegistry() string {
	if nonInteractive {
		return ""
	}

	fmt.Println()
	fmt.Println("Scion runs agents in containers. You need to build and push container images")
	fmt.Println("to a registry you control before starting agents.")
	fmt.Println()
	fmt.Println("  See: image-build/README.md for build instructions")
	fmt.Println("  Quick start: image-build/scripts/build-images.sh --registry <registry> --push")
	fmt.Println()

	if !util.IsTerminal() {
		return ""
	}

	reader := bufio.NewReader(os.Stdin)
	fmt.Print("Image registry path (e.g., ghcr.io/myorg) — enter to skip: ")
	input, err := reader.ReadString('\n')
	if err != nil {
		return ""
	}
	return strings.TrimSpace(input)
}

func init() {
	rootCmd.AddCommand(groveCmd)
	groveCmd.AddCommand(groveInitCmd)

	groveInitCmd.Flags().BoolVar(&globalInit, "global", false, "Initialize the global grove in the home directory")
	groveInitCmd.Flags().BoolVar(&machineInit, "machine", false, "Perform full machine-level setup (seeds harness-configs, templates, settings)")
	groveInitCmd.Flags().StringVar(&initImageRegistry, "image-registry", "", "Container image registry path (e.g., ghcr.io/myorg)")
}
