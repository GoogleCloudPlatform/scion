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

	"github.com/ptone/scion-agent/pkg/brokercredentials"
	"github.com/ptone/scion-agent/pkg/config"
	"github.com/ptone/scion-agent/pkg/harness"
	"github.com/ptone/scion-agent/pkg/hubclient"
	"github.com/ptone/scion-agent/pkg/hubsync"
	"github.com/ptone/scion-agent/pkg/util"
	"github.com/spf13/cobra"
)

var globalInit bool

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

		if globalInit {
			fmt.Println("Initializing global scion directory...")
			if err := config.InitGlobal(harnesses); err != nil {
				return fmt.Errorf("failed to initialize global config: %w", err)
			}
			fmt.Println("scion grove successfully initialized.")

			// Prompt for Hub registration if Hub is configured
			if err := promptHubRegistration(true); err != nil {
				// Non-fatal: just log the error
				fmt.Printf("Note: %v\n", err)
			}

			return nil
		}

		// Check for nested grove - error if already inside a scion project
		if grovePath, rootDir, found := config.GetEnclosingGrovePath(); found {
			wd, _ := os.Getwd()
			if filepath.Clean(wd) == filepath.Clean(rootDir) {
				return fmt.Errorf("already inside a scion project at %s. skipping re-initialization", rootDir)
			}
			// Allow initialization if the found grove is the global one
			// This permits project groves to exist when ~/.scion exists
			globalDir, err := config.GetGlobalDir()
			if err != nil || filepath.Clean(grovePath) != filepath.Clean(globalDir) {
				return fmt.Errorf("already inside a scion project at %s. Nested groves are not supported", rootDir)
			}
			// Found grove is the global one - allow project initialization to proceed
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

		fmt.Println("Initializing scion project grove...")
		if err := config.InitProject("", harnesses); err != nil {
			return fmt.Errorf("failed to initialize project grove: %w", err)
		}

		// Generate and save grove_id
		groveID := config.GenerateGroveIDForDir(filepath.Dir(targetDir))
		if err := config.UpdateSetting(targetDir, "grove_id", groveID, false); err != nil {
			fmt.Printf("Warning: failed to save grove_id: %v\n", err)
		}

		fmt.Println("scion grove successfully initialized.")
		fmt.Printf("Grove ID: %s\n", groveID)

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
	resolvedPath, _, err := config.ResolveGrovePath(gp)
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
	var groveName string
	var gitRemote string
	groveID := settings.GroveID

	if isGlobal {
		groveName = "global"
	} else {
		gitRemote = util.GetGitRemote()
		if gitRemote != "" {
			groveName = util.ExtractRepoName(gitRemote)
		} else {
			groveName = filepath.Base(filepath.Dir(resolvedPath))
		}
	}

	// Register grove without broker info first
	req := &hubclient.RegisterGroveRequest{
		ID:        groveID,
		Name:      groveName,
		GitRemote: util.NormalizeGitRemote(gitRemote),
		Path:      resolvedPath,
	}

	ctxReg, cancelReg := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelReg()

	resp, err := client.Groves().Register(ctxReg, req)
	if err != nil {
		return fmt.Errorf("registration failed: %w", err)
	}

	// Enable Hub integration
	_ = config.UpdateSetting(resolvedPath, "hub.enabled", "true", isGlobal)

	if resp.Created {
		fmt.Printf("Created new grove on Hub: %s (ID: %s)\n", resp.Grove.Name, resp.Grove.ID)
	} else {
		fmt.Printf("Linked to existing grove on Hub: %s (ID: %s)\n", resp.Grove.Name, resp.Grove.ID)
		// Update local grove_id to match the hub grove's ID
		if resp.Grove.ID != groveID {
			if err := config.UpdateSetting(resolvedPath, "grove_id", resp.Grove.ID, isGlobal); err != nil {
				fmt.Printf("Warning: failed to update local grove_id: %v\n", err)
			}
		}
		groveID = resp.Grove.ID
	}

	// Show any auto-provided brokers
	ctxProviders, cancelProviders := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelProviders()

	providersResp, err := client.Groves().ListProviders(ctxProviders, resp.Grove.ID)
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
			if hubsync.ShowInitProvidePrompt(localBrokerName, resp.Grove.Name, autoConfirm) {
				// Add this broker as a provider
				ctxAdd, cancelAdd := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancelAdd()

				addReq := &hubclient.AddProviderRequest{
					BrokerID:  localBrokerID,
					LocalPath: resolvedPath,
				}

				_, err := client.Groves().AddProvider(ctxAdd, resp.Grove.ID, addReq)
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

func init() {
	rootCmd.AddCommand(groveCmd)
	groveCmd.AddCommand(groveInitCmd)

	groveInitCmd.Flags().BoolVar(&globalInit, "global", false, "Initialize the global grove in the home directory")
}
