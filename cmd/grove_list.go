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

var groveListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List all known groves on this machine",
	Long: `List all groves known to scion, including the global grove and
all project groves (both git-based and external). Shows workspace path,
type, agent count, and status for each grove.

Orphaned groves (where the workspace no longer exists) are flagged.
Use 'scion grove prune' to clean them up.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		groves, err := config.DiscoverProjects()
		if err != nil {
			return fmt.Errorf("failed to discover groves: %w", err)
		}

		if isJSONOutput() {
			if groves == nil {
				groves = []config.ProjectInfo{}
			}
			return outputJSON(groves)
		}

		if len(groves) == 0 {
			fmt.Println("No groves found. Run 'scion init' to create one.")
			return nil
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tTYPE\tAGENTS\tSTATUS\tWORKSPACE")
		for _, g := range groves {
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
	groveCmd.AddCommand(groveListCmd)
}
