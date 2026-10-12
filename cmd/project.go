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
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"bufio"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/spf13/cobra"
)

var globalInit bool
var initImageRegistry string

// projectCmd represents the project command
var projectCmd = &cobra.Command{
	Use:     "project",
	Aliases: []string{"group"},
	Short:   "Manage scion projects",
	Long:    `A project is the grouping construct for a set of agents. The .scion folder represents a project.`,
}

// projectInitCmd represents the init subcommand for project
var projectInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Initialize a new project",
	Long: `Initialize a new project by creating the .scion directory structure
and seeding the default template. 

By default, it initializes in:
- The root of the current git repo if run inside a repo
- The current directory

With --global, it initializes in the user's home folder.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		embedOnlyHarnesses := harness.EmbedOnlyHarnesses()

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

			opts := config.InitMachineOpts{
				ImageRegistry: registryValue,
				Force:         machineInitForce,
			}
			if err := config.InitMachine(embedOnlyHarnesses, opts); err != nil {
				return fmt.Errorf("failed to initialize global config: %w", err)
			}

			if isJSONOutput() {
				details := map[string]interface{}{"global": true, "machine": true}
				if registryValue != "" {
					details["image_registry"] = registryValue
				}
				return outputJSON(ActionResult{
					Status:  "success",
					Command: "project init",
					Message: "scion project successfully initialized.",
					Details: details,
				})
			}

			fmt.Println("scion project successfully initialized.")
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

		// Check if ~/.scion/ exists; error if not since global project is required
		if globalDir, err := config.GetGlobalDir(); err == nil {
			if _, err := os.Stat(globalDir); os.IsNotExist(err) {
				return fmt.Errorf("global scion directory (~/.scion/) does not exist.\nRun 'scion init --machine' first to set up the global configuration")
			}
		}

		// Check for existing project at or above current directory
		if _, rootDir, found := config.GetEnclosingProjectPath(); found {
			wd, _ := os.Getwd()
			if filepath.Clean(wd) == filepath.Clean(rootDir) {
				// Re-running init in an existing project is allowed — it ensures
				// the project structure is intact (dirs, gitignore, etc.).
				if !isJSONOutput() {
					fmt.Println("Project already initialized. Ensuring integrity...")
				}
				// Fall through to InitProject which is idempotent
			} else if !isJSONOutput() {
				// Inform user about parent project — nested projects are allowed
				fmt.Printf("Note: parent project exists at %s. Initializing nested project.\n", rootDir)
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
			fmt.Println("Initializing scion project...")
		}
		if err := config.InitProject("", embedOnlyHarnesses); err != nil {
			return fmt.Errorf("failed to initialize project: %w", err)
		}

		// Resolve the projectID and save it to settings.
		// For non-git projects, targetDir (.scion) is now a marker file, so we must
		// resolve through it to the external config path. The projectID is already
		// generated during InitProject — read it back rather than generating a new one.
		var projectID string
		markerPath := filepath.Join(filepath.Dir(targetDir), config.DotScion)
		if config.IsProjectMarkerFile(markerPath) {
			// Non-git project: read projectID from marker, save to external settings
			marker, err := config.ReadProjectMarker(markerPath)
			if err == nil {
				projectID = marker.ProjectID
				// projectID is already written during initExternalProject
			}
		} else {
			// Git project: read projectID from file, save to in-repo settings
			projectID, _ = config.ReadProjectID(targetDir)
			if projectID == "" {
				projectID = config.GenerateProjectID()
			}
			if err := config.UpdateSetting(targetDir, "project_id", projectID, false); err != nil {
				if !isJSONOutput() {
					fmt.Printf("Warning: failed to save project_id: %v\n", err)
				}
			}
		}

		if isJSONOutput() {
			return outputJSON(ActionResult{
				Status:  "success",
				Command: "project init",
				Message: "scion project successfully initialized.",
				Details: map[string]interface{}{
					"projectId": projectID,
					"path":      targetDir,
				},
			})
		}

		fmt.Println("scion project successfully initialized.")
		fmt.Printf("Project ID: %s\n", projectID)

		// Prompt for Hub registration if Hub is configured
		if err := promptHubRegistration(false); err != nil {
			// Non-fatal: just log the error
			fmt.Printf("Note: %v\n", err)
		}

		return nil
	},
}

// promptHubRegistration links the newly initialised project to the hub,
// through requireHub: on the local workstation hub it links without a
// prompt, on a remote hub it asks first. When no hub is configured or the
// local server is not running, it returns a note-level error (the caller
// prints it as "Note: ..."): the project links on first use once a hub is
// up, so init itself does not fail.
func promptHubRegistration(isGlobal bool) error {
	var gp string
	if isGlobal {
		gp = "global"
	}
	if _, err := requireHub(gp); err != nil {
		var notConfigured *hubsync.HubNotConfiguredError
		if errors.As(err, &notConfigured) {
			return errors.New("no hub is configured yet; this project links on first use. " +
				"Start the local hub with 'scion server start' (first run opens setup), " +
				"or set a remote one with 'scion config set hub.endpoint <url>'")
		}
		var notRunning *hubsync.LocalServerNotRunningError
		if errors.As(err, &notRunning) {
			return errors.New("the local scion server is not running; this project links on first use. " +
				"Start it with 'scion server start'")
		}
		return err
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

	// Get the broker name for display: the configured name, else the hostname
	brokerName = config.LocalBrokerName("")
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

	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Scion runs agents in containers. You need to build and push container images")
	fmt.Fprintln(os.Stderr, "to a registry you control before starting agents.")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "  See: image-build/README.md for build instructions")
	fmt.Fprintln(os.Stderr, "  Quick start: image-build/scripts/build-images.sh --registry <registry> --push")
	fmt.Fprintln(os.Stderr)

	if !util.IsTerminal() {
		return ""
	}

	reader := bufio.NewReader(os.Stdin)
	fmt.Fprint(os.Stderr, "Image registry path (e.g., ghcr.io/myorg) — enter to skip: ")
	input, err := reader.ReadString('\n')
	if err != nil {
		return ""
	}
	return strings.TrimSpace(input)
}

func init() {
	rootCmd.AddCommand(projectCmd)
	projectCmd.AddCommand(projectInitCmd)

	projectInitCmd.Flags().BoolVar(&globalInit, "global", false, "Initialize the global project in the home directory")
	projectInitCmd.Flags().BoolVar(&machineInit, "machine", false, "Perform full machine-level setup (seeds harness-configs, templates, settings)")
	projectInitCmd.Flags().StringVar(&initImageRegistry, "image-registry", "", "Container image registry path (e.g., ghcr.io/myorg)")
}
