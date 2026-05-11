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
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/spf13/cobra"
)

var sharedDirCmd = &cobra.Command{
	Use:     "shared-dir",
	Aliases: []string{"sd"},
	Short:   "Manage project shared directories",
	Long:    `Shared directories provide filesystem-level state sharing between agents in a project.`,
}

var sharedDirListCmd = &cobra.Command{
	Use:   "list",
	Short: "List shared directories for the current project",
	RunE: func(cmd *cobra.Command, args []string) error {
		projectDir, err := config.GetResolvedProjectDir(projectPath)
		if err != nil {
			return fmt.Errorf("failed to resolve project: %w", err)
		}

		settings, _, err := config.LoadEffectiveSettings(projectDir)
		if err != nil {
			return fmt.Errorf("failed to load settings: %w", err)
		}

		if len(settings.SharedDirs) == 0 {
			if isJSONOutput() {
				return outputJSON([]interface{}{})
			}
			fmt.Println("No shared directories configured.")
			return nil
		}

		infos, err := config.GetSharedDirInfos(projectDir, settings.SharedDirs)
		if err != nil {
			return fmt.Errorf("failed to get shared dir info: %w", err)
		}

		if isJSONOutput() {
			return outputJSON(infos)
		}

		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tMODE\tMOUNT\tPROVISIONED")
		for _, info := range infos {
			mode := "read-write"
			if info.ReadOnly {
				mode = "read-only"
			}
			mount := fmt.Sprintf("/scion-volumes/%s", info.Name)
			if info.InWorkspace {
				mount = fmt.Sprintf("/workspace/.scion-volumes/%s", info.Name)
			}
			provisioned := "no"
			if info.Exists {
				provisioned = "yes"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", info.Name, mode, mount, provisioned)
		}
		return tw.Flush()
	},
}

var sharedDirCreateCmd = &cobra.Command{
	Use:   "create <name>",
	Short: "Create a new shared directory",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]

		// Validate the name
		if err := api.ValidateSharedDirs([]api.SharedDir{{Name: name}}); err != nil {
			return err
		}

		projectDir, err := config.GetResolvedProjectDir(projectPath)
		if err != nil {
			return fmt.Errorf("failed to resolve project: %w", err)
		}

		// Load current settings to check for duplicates
		vs, err := config.LoadSingleFileVersioned(projectDir)
		if err != nil {
			return fmt.Errorf("failed to load settings: %w", err)
		}

		for _, d := range vs.SharedDirs {
			if d.Name == name {
				return fmt.Errorf("shared directory %q already exists", name)
			}
		}

		sdReadOnly, _ := cmd.Flags().GetBool("read-only")
		sdInWorkspace, _ := cmd.Flags().GetBool("in-workspace")

		newDir := api.SharedDir{
			Name:        name,
			ReadOnly:    sdReadOnly,
			InWorkspace: sdInWorkspace,
		}

		vs.SharedDirs = append(vs.SharedDirs, newDir)
		if err := config.SaveVersionedSettings(projectDir, vs); err != nil {
			return fmt.Errorf("failed to save settings: %w", err)
		}

		// Create the host-side directory
		if err := config.EnsureSharedDirs(projectDir, []api.SharedDir{newDir}); err != nil {
			return fmt.Errorf("failed to provision shared directory: %w", err)
		}

		if isJSONOutput() {
			return outputJSON(ActionResult{
				Status:  "success",
				Command: "shared-dir create",
				Message: fmt.Sprintf("Shared directory '%s' created.", name),
				Details: map[string]interface{}{
					"name":         name,
					"read_only":    sdReadOnly,
					"in_workspace": sdInWorkspace,
				},
			})
		}

		fmt.Printf("Shared directory '%s' created.\n", name)
		return nil
	},
}

var sharedDirRemoveCmd = &cobra.Command{
	Use:   "remove <name>",
	Short: "Remove a shared directory",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]

		projectDir, err := config.GetResolvedProjectDir(projectPath)
		if err != nil {
			return fmt.Errorf("failed to resolve project: %w", err)
		}

		vs, err := config.LoadSingleFileVersioned(projectDir)
		if err != nil {
			return fmt.Errorf("failed to load settings: %w", err)
		}

		found := false
		var updated []api.SharedDir
		for _, d := range vs.SharedDirs {
			if d.Name == name {
				found = true
				continue
			}
			updated = append(updated, d)
		}

		if !found {
			return fmt.Errorf("shared directory %q not found", name)
		}

		if !autoConfirm {
			fmt.Printf("Remove shared directory '%s' and all its contents? [y/N] ", name)
			var response string
			fmt.Scanln(&response)
			if response != "y" && response != "Y" {
				fmt.Println("Cancelled.")
				return nil
			}
		}

		vs.SharedDirs = updated
		if err := config.SaveVersionedSettings(projectDir, vs); err != nil {
			return fmt.Errorf("failed to save settings: %w", err)
		}

		if err := config.RemoveSharedDir(projectDir, name); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to remove host directory: %v\n", err)
		}

		if isJSONOutput() {
			return outputJSON(ActionResult{
				Status:  "success",
				Command: "shared-dir remove",
				Message: fmt.Sprintf("Shared directory '%s' removed.", name),
			})
		}

		fmt.Printf("Shared directory '%s' removed.\n", name)
		return nil
	},
}

var sharedDirInfoCmd = &cobra.Command{
	Use:   "info <name>",
	Short: "Show details about a shared directory",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]

		projectDir, err := config.GetResolvedProjectDir(projectPath)
		if err != nil {
			return fmt.Errorf("failed to resolve project: %w", err)
		}

		settings, _, err := config.LoadEffectiveSettings(projectDir)
		if err != nil {
			return fmt.Errorf("failed to load settings: %w", err)
		}

		var target *api.SharedDir
		for _, d := range settings.SharedDirs {
			if d.Name == name {
				d := d
				target = &d
				break
			}
		}
		if target == nil {
			return fmt.Errorf("shared directory %q not found", name)
		}

		infos, err := config.GetSharedDirInfos(projectDir, []api.SharedDir{*target})
		if err != nil {
			return fmt.Errorf("failed to get shared dir info: %w", err)
		}
		info := infos[0]

		if isJSONOutput() {
			return outputJSON(info)
		}

		fmt.Printf("Name:         %s\n", info.Name)
		if info.ReadOnly {
			fmt.Println("Mode:         read-only")
		} else {
			fmt.Println("Mode:         read-write")
		}
		if info.InWorkspace {
			fmt.Printf("Mount:        /workspace/.scion-volumes/%s\n", info.Name)
		} else {
			fmt.Printf("Mount:        /scion-volumes/%s\n", info.Name)
		}
		fmt.Printf("Host path:    %s\n", info.HostPath)
		if info.Exists {
			fmt.Println("Provisioned:  yes")
		} else {
			fmt.Println("Provisioned:  no")
		}

		return nil
	},
}

func init() {
	rootCmd.AddCommand(sharedDirCmd)
	sharedDirCmd.AddCommand(sharedDirListCmd)
	sharedDirCmd.AddCommand(sharedDirCreateCmd)
	sharedDirCmd.AddCommand(sharedDirRemoveCmd)
	sharedDirCmd.AddCommand(sharedDirInfoCmd)

	sharedDirCreateCmd.Flags().Bool("read-only", false, "Mount as read-only for agents")
	sharedDirCreateCmd.Flags().Bool("in-workspace", false, "Mount inside the workspace tree instead of /scion-volumes")
}
