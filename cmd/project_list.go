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

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/spf13/cobra"
)

var projectListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List all known projects on this machine",
	Long:    `List all projects known to scion, including the global project and all project workspaces. Shows workspace path, type, agent count, and status for each project.

Orphaned projects (where the workspace no longer exists) are flagged.
Use 'scion project prune' to clean them up.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		projects, err := config.DiscoverProjects()
		if err != nil {
			return fmt.Errorf("failed to discover projects: %w", err)
		}

		if isJSONOutput() {
			if projects == nil {
				projects = []config.ProjectInfo{}
			}
			return outputJSON(projects)
		}

		if len(projects) == 0 {
			fmt.Println("No projects found. Run 'scion init' to create one.")
			return nil
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tTYPE\tAGENTS\tSTATUS\tWORKSPACE")
		for _, g := range projects {
			workspace := g.WorkspacePath
			if workspace == "" {
				workspace = "-"
			}
			fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\n",
				g.Name, g.Type, g.AgentCount, g.Status, workspace)
		}
		w.Flush()

		return nil
	},
}

func init() {
	projectCmd.AddCommand(projectListCmd)
}
