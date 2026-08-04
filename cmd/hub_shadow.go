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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/credentials"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/spf13/cobra"
)

// hubShadowCmd creates a local shadow of a remote hub project.
var hubShadowCmd = &cobra.Command{
	Use:   "shadow <project-ref>",
	Short: "Shadow a Hub project in the current directory",
	Long: `Create a local shadow of a Hub project in the current directory.

A shadowed project is a directory linked to a Hub project purely for CLI
routing — no workspace content, no provider registration, and no broker
involvement. It allows running commands like 'scion list', 'scion look',
and 'scion attach' against hub-managed agents from any workstation.

The <project-ref> can be a project slug, UUID, or git URL.

Examples:
  # Shadow by slug
  scion hub shadow my-project

  # Shadow by UUID
  scion hub shadow 1dfdd6c7-f077-4acd-bde2-978d12f34f9a

  # Shadow by git URL
  scion hub shadow https://github.com/example/repo.git

  # Shadow with a specific hub endpoint
  scion hub shadow my-project --hub https://hub.example.com`,
	Args: cobra.ExactArgs(1),
	RunE: runHubShadow,
}

// hubUnshadowCmd removes the shadow link from the current directory.
var hubUnshadowCmd = &cobra.Command{
	Use:   "unshadow",
	Short: "Remove the Hub shadow from the current directory",
	Long: `Remove the shadow link from the current directory.

This removes the .scion marker file, disconnecting this directory from
the Hub project. Only works if the current directory is a shadowed project.

Examples:
  # Remove the shadow
  scion hub unshadow`,
	RunE: runHubUnshadow,
}

func init() {
	hubCmd.AddCommand(hubShadowCmd)
	hubCmd.AddCommand(hubUnshadowCmd)
}

func runHubShadow(cmd *cobra.Command, args []string) error {
	projectRef := args[0]

	// Check if .scion already exists in the current directory.
	wd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get working directory: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to find home directory: %w", err)
	}
	dotScionPath := filepath.Join(wd, config.DotScion)
	if info, err := os.Stat(dotScionPath); err == nil {
		if info.IsDir() {
			return fmt.Errorf("this directory is already a Scion project (has a .scion directory).\n" +
				"Use 'scion hub link' to link an existing project to the Hub")
		}
		// .scion is a file — check if it's a shadow marker (idempotent replace per D2).
		marker, mErr := config.ReadProjectMarker(dotScionPath)
		if mErr != nil || !marker.IsShadow() {
			return fmt.Errorf("this directory already has a .scion marker file for a non-shadow project.\n" +
				"Remove it first or use a different directory")
		}
		// Existing shadow marker — will be replaced below.
	}

	// Load settings from global (or whatever fallback is available) to get hub config.
	fallbackPath, _, err := config.ResolveProjectPath("")
	if err != nil {
		// No project found — try loading global settings directly.
		fallbackPath = filepath.Join(home, config.GlobalDir)
	}

	settings, err := config.LoadSettings(fallbackPath)
	if err != nil {
		// Create minimal settings to work with --hub flag
		settings = &config.Settings{}
	}

	// Determine hub endpoint
	endpoint := GetHubEndpoint(settings)
	if hubEndpoint != "" {
		endpoint = hubEndpoint
	}
	if endpoint == "" {
		return fmt.Errorf("hub endpoint not configured.\n\n" +
			"Configure via:\n" +
			"  scion hub shadow <project> --hub <url>\n" +
			"  scion config set hub.endpoint <url>\n" +
			"  SCION_HUB_ENDPOINT=<url>")
	}

	// Verify authentication
	authInfo := getAuthInfo(settings, endpoint)
	if authInfo.MethodType == "none" {
		// Also check credentials directly for the endpoint
		if !credentials.IsAuthenticated(endpoint) {
			return fmt.Errorf("not authenticated to Hub at %s\n\nPlease log in first:\n  scion hub auth login --hub %s", endpoint, endpoint)
		}
	}

	// Create Hub client
	client, err := getHubClient(settings)
	if err != nil {
		return fmt.Errorf("failed to create Hub client: %w", err)
	}

	// Health check
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if _, err := client.Health(ctx); err != nil {
		return fmt.Errorf("hub at %s is not responding: %w", endpoint, hubclient.HintProxyError(err))
	}

	// Resolve the project on the hub
	project, err := hubsync.ResolveProjectOnHub(ctx, client, projectRef)
	if err != nil {
		return err
	}

	// Determine the slug — use the hub's slug if available, otherwise generate one.
	slug := project.Slug
	if slug == "" {
		slug = api.Slugify(project.Name)
	}

	// Write the .scion marker file with type: shadow
	marker := &config.ProjectMarker{
		ProjectID:   project.ID,
		ProjectName: project.Name,
		ProjectSlug: slug,
		Type:        "shadow",
	}
	if err := config.WriteProjectMarker(dotScionPath, marker); err != nil {
		return fmt.Errorf("failed to write project marker: %w", err)
	}

	// Create the external project-config directory with settings
	configDir := filepath.Join(home, config.GlobalDir, config.ProjectConfigsDir, marker.DirName(), config.DotScion)
	hubEnabled := true
	vs := &config.VersionedSettings{
		SchemaVersion: "1",
		ProjectType:   string(config.ProjectTypeShadow),
		WorkspacePath: wd,
		Hub: &config.V1HubClientConfig{
			Enabled:   &hubEnabled,
			Endpoint:  endpoint,
			ProjectID: project.ID,
		},
	}
	if err := config.SaveVersionedSettings(configDir, vs); err != nil {
		// Clean up the marker file on failure
		_ = os.Remove(dotScionPath)
		return fmt.Errorf("failed to write project config: %w", err)
	}

	fmt.Printf("Shadowed project '%s' (slug: %s) from hub at %s\n", project.Name, slug, endpoint)
	fmt.Println("\nYou can now use commands like:")
	fmt.Println("  scion list          - List agents")
	fmt.Println("  scion look <agent>  - View agent output")
	fmt.Println("  scion attach <agent> - Attach to agent")
	fmt.Println("\nTo remove the shadow: scion hub unshadow")

	return nil
}

func runHubUnshadow(cmd *cobra.Command, args []string) error {
	wd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get working directory: %w", err)
	}

	dotScionPath := filepath.Join(wd, config.DotScion)
	info, err := os.Stat(dotScionPath)
	if err != nil {
		return fmt.Errorf("no .scion marker found in the current directory")
	}

	if info.IsDir() {
		return fmt.Errorf("this directory is a full Scion project, not a shadow.\n" +
			"Use 'scion hub unlink' to unlink from the Hub")
	}

	marker, err := config.ReadProjectMarker(dotScionPath)
	if err != nil {
		return fmt.Errorf("failed to read .scion marker: %w", err)
	}

	if !marker.IsShadow() {
		return fmt.Errorf("this directory has a project marker but is not a shadow.\n" +
			"Only shadow markers can be removed with 'hub unshadow'")
	}

	// Remove the marker file
	if err := os.Remove(dotScionPath); err != nil {
		return fmt.Errorf("failed to remove .scion marker: %w", err)
	}

	util.Debugf("Removed shadow marker for project %s (ID: %s)", marker.ProjectName, marker.ProjectID)

	// Clean up the settings directory under ~/.scion/project-configs/<slug>__<shortuuid>/
	settingsCleaned := false
	globalDir, globalErr := config.GetGlobalDir()
	if globalErr == nil && globalDir != "" {
		configDir := filepath.Join(globalDir, config.ProjectConfigsDir, marker.DirName())
		if err := os.RemoveAll(configDir); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not remove settings directory %s: %v\n", configDir, err)
		} else {
			settingsCleaned = true
		}
	}

	fmt.Printf("Removed shadow for project '%s' from this directory.\n", marker.ProjectName)
	if settingsCleaned {
		fmt.Println("Local settings directory cleaned up.")
	}
	fmt.Println("The project and its agents remain on the Hub.")

	return nil
}
