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
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/spf13/cobra"
)

var projectPruneCmd = &cobra.Command{
	Use:   "prune",
	Short: "Remove orphaned project configs",
	Long: `Detect and remove project configs in ~/.scion/project-configs/ whose
workspaces no longer exist. This cleans up leftover configuration from
deleted or moved projects.

Any running agent containers belonging to orphaned projects will be stopped
and removed before the project config is deleted.

Use 'scion project list' to see all projects and their status first.
Use 'scion project reconnect' to fix a project whose workspace moved.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		orphaned, err := config.FindOrphanedProjectConfigs()
		if err != nil {
			return fmt.Errorf("failed to scan for orphaned projects: %w", err)
		}

		if len(orphaned) == 0 {
			if isJSONOutput() {
				return outputJSON(ActionResult{
					Status:  "success",
					Command: "project prune",
					Message: "No orphaned project configs found.",
				})
			}
			fmt.Println("No orphaned project configs found.")
			return nil
		}

		if isJSONOutput() {
			results := make([]map[string]interface{}, 0, len(orphaned))
			for _, g := range orphaned {
				results = append(results, map[string]interface{}{
					"name":           g.Name,
					"config_path":    g.ConfigPath,
					"workspace_path": g.WorkspacePath,
					"agent_count":    g.AgentCount,
				})
			}

			if !autoConfirm {
				return outputJSON(ActionResult{
					Status:  "pending",
					Command: "project prune",
					Message: fmt.Sprintf("Found %d orphaned project config(s). Use --yes to confirm removal.", len(orphaned)),
					Details: map[string]interface{}{"orphaned": results},
				})
			}

			var removed []string
			for _, g := range orphaned {
				cleanupOrphanedProject(g)
				if err := config.RemoveProjectConfig(g.ConfigPath); err != nil {
					return fmt.Errorf("failed to remove %s: %w", g.ConfigPath, err)
				}
				removed = append(removed, g.Name)
			}

			return outputJSON(ActionResult{
				Status:  "success",
				Command: "project prune",
				Message: fmt.Sprintf("Removed %d orphaned project config(s).", len(removed)),
				Details: map[string]interface{}{"removed": removed},
			})
		}

		// Interactive mode
		fmt.Printf("Found %d orphaned project config(s):\n\n", len(orphaned))
		for _, g := range orphaned {
			workspace := g.WorkspacePath
			if workspace == "" {
				workspace = "(no workspace path)"
			}
			fmt.Printf("  %s\n", g.Name)
			fmt.Printf("    Config: %s\n", g.ConfigPath)
			fmt.Printf("    Workspace: %s\n", workspace)
			if g.AgentCount > 0 {
				fmt.Printf("    Agents: %d (containers will be stopped and removed)\n", g.AgentCount)
			}
			fmt.Println()
		}

		if !autoConfirm {
			if nonInteractive {
				return fmt.Errorf("orphaned project configs found; use --yes to confirm removal")
			}
			if !util.IsTerminal() {
				return fmt.Errorf("orphaned project configs found; use --yes to confirm removal in non-terminal mode")
			}
			fmt.Print("Remove these orphaned configs? [y/N] ")
			reader := bufio.NewReader(os.Stdin)
			input, err := reader.ReadString('\n')
			if err != nil {
				return err
			}
			if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(input)), "y") {
				fmt.Println("Aborted.")
				return nil
			}
		}

		for _, g := range orphaned {
			cleanupOrphanedProject(g)
			if err := config.RemoveProjectConfig(g.ConfigPath); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to remove %s: %v\n", g.Name, err)
				continue
			}
			fmt.Printf("Removed: %s\n", g.Name)
		}

		return nil
	},
}

// cleanupOrphanedProject stops any running containers for the orphaned project's
// agents before the project config is removed. Errors are best-effort and logged
// as warnings.
func cleanupOrphanedProject(g config.ProjectInfo) {
	if g.AgentCount == 0 {
		return
	}

	agentNames := config.ListAgentNames(g.AgentsDir())
	if len(agentNames) == 0 {
		return
	}

	rt := runtime.GetRuntime("", profile)
	mgr := agent.NewManager(rt)
	ctx := context.Background()

	stopped := agent.StopProjectContainers(ctx, mgr, g.Name, agentNames)
	for _, name := range stopped {
		fmt.Fprintf(os.Stderr, "Stopped container for agent '%s'\n", name)
	}
}

func init() {
	projectCmd.AddCommand(projectPruneCmd)
}
