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
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/spf13/cobra"
)

var projectReconnectCmd = &cobra.Command{
	Use:   "reconnect <new-workspace-path>",
	Short: "Reconnect a project to a moved workspace",
	Long: `Update the workspace_path in a project's settings when the workspace
directory has been moved to a new location. This fixes projects that show
as 'orphaned' in 'scion project list' because their workspace was relocated.

The command must be run from within the moved workspace directory, or the
new workspace path can be provided as an argument. The project is identified
by the .scion marker file in the workspace.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		var workspacePath string
		var err error

		if len(args) > 0 {
			workspacePath, err = filepath.Abs(args[0])
			if err != nil {
				return fmt.Errorf("invalid path: %w", err)
			}
		} else {
			workspacePath, err = os.Getwd()
			if err != nil {
				return fmt.Errorf("failed to get working directory: %w", err)
			}
		}

		// Verify workspace exists
		if _, err := os.Stat(workspacePath); err != nil {
			return fmt.Errorf("workspace path does not exist: %s", workspacePath)
		}

		// Find the .scion marker file
		markerPath := filepath.Join(workspacePath, config.DotScion)
		if !config.IsProjectMarkerFile(markerPath) {
			return fmt.Errorf("no .scion marker file found at %s\nReconnect only works for non-git projects with externalized storage", workspacePath)
		}

		// Read the marker to find the external config
		marker, err := config.ReadProjectMarker(markerPath)
		if err != nil {
			return fmt.Errorf("invalid .scion marker file: %w", err)
		}

		configPath, err := marker.ExternalProjectPath()
		if err != nil {
			return fmt.Errorf("failed to resolve external project path: %w", err)
		}

		// Verify config exists
		if _, err := os.Stat(configPath); err != nil {
			return fmt.Errorf("external project config not found at %s\nThe project may need to be re-initialized with 'scion init'", configPath)
		}

		// Update workspace_path
		if err := config.ReconnectProject(configPath, workspacePath); err != nil {
			return fmt.Errorf("failed to update workspace path: %w", err)
		}

		if isJSONOutput() {
			return outputJSON(ActionResult{
				Status:  "success",
				Command: "project reconnect",
				Message: fmt.Sprintf("Project %q reconnected to %s", marker.ProjectName, workspacePath),
				Details: map[string]interface{}{
					"project_name":   marker.ProjectName,
					"project_id":     marker.ProjectID,
					"config_path":    configPath,
					"workspace_path": workspacePath,
				},
			})
		}

		fmt.Printf("Project %q reconnected to %s\n", marker.ProjectName, workspacePath)
		return nil
	},
}

func init() {
	projectCmd.AddCommand(projectReconnectCmd)
}
