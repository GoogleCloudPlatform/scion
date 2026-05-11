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
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/spf13/cobra"
)

var deleteStopped bool

// deleteCmd represents the delete command
var deleteCmd = &cobra.Command{
	Use:               "delete <agent> [agent...]",
	Aliases:           []string{"rm"},
	Short:             "Delete one or more agents",
	Long:              `Stop and remove one or more agent containers and their associated files and worktrees.`,
	ValidArgsFunction: getMultiAgentNames,
	Args: func(cmd *cobra.Command, args []string) error {
		if deleteStopped {
			if len(args) > 0 {
				return fmt.Errorf("no arguments allowed when using --stopped")
			}
			return nil
		}
		if len(args) < 1 {
			return fmt.Errorf("requires at least 1 argument (agent name)")
		}
		return nil
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		// Normalize agent names to slugs for consistent lookup
		for i, a := range args {
			args[i] = api.Slugify(a)
		}

		projectDir, _ := config.GetResolvedProjectDir(projectPath)
		if preserveBranch && !util.IsGitRepoDir(projectDir) {
			fmt.Println("Warning: --preserve-branch used outside a git repository; this flag has no effect.")
		}

		// Check if Hub should be used, excluding all target agents from sync requirements.
		var excludedAgents []string
		if !deleteStopped {
			excludedAgents = args
		}
		hubCtx, err := CheckHubAvailabilityForAgents(projectPath, excludedAgents, false)
		if err != nil {
			return err
		}

		if deleteStopped {
			if hubCtx != nil {
				return deleteStoppedViaHub(hubCtx)
			}

			// Require an explicit grove context — error if not in a grove (unless --global)
			resolvedGrove, _, err := config.RequireProjectPath(projectPath)
			if err != nil {
				return err
			}

			rt := runtime.GetRuntime(projectPath, profile)
			mgr := agent.NewManager(rt)

			filters := map[string]string{
				"scion.agent":        "true",
				"scion.project_path": resolvedGrove,
				"scion.project":      config.GetProjectName(resolvedGrove),
			}

			agents, err := mgr.List(context.Background(), filters)
			if err != nil {
				return err
			}

			var deletedCount int
			for _, a := range agents {
				if a.ContainerID == "" {
					continue // No container
				}

				// Get the canonical agent name from labels (Docker Names field has leading slash)
				agentName := a.Labels["scion.name"]
				if agentName == "" {
					continue // Not a scion-managed container
				}

				status := strings.ToLower(a.ContainerStatus)
				// Check if running
				if strings.HasPrefix(status, "up") ||
					strings.HasPrefix(status, "running") ||
					strings.HasPrefix(status, "pending") ||
					strings.HasPrefix(status, "restarting") {
					continue
				}

				statusf("Deleting stopped agent '%s' (status: %s)...\n", agentName, a.ContainerStatus)

				targetProjectPath:= a.ProjectPath
				if targetProjectPath == "" {
					targetProjectPath = resolvedGrove
				}

				branchDeleted, err := mgr.Delete(context.Background(), agentName, true, targetProjectPath, !preserveBranch)
				if err != nil {
					fmt.Fprintf(os.Stderr, "Failed to delete agent '%s': %v\n", agentName, err)
					continue
				}

				if branchDeleted {
					statusf("Git branch associated with agent '%s' deleted.\n", agentName)
				}
				statusf("Agent '%s' deleted.\n", agentName)
				deletedCount++
			}

			if deletedCount == 0 {
				statusln("No stopped agents found.")
			}
			return nil
		}

		// Use Hub if available
		if hubCtx != nil {
			return deleteAgentsViaHub(hubCtx, args)
		}

		// Local mode - delete each agent
		var errs []string
		var results []map[string]interface{}
		for _, agentName := range args {
			if err := deleteAgentLocal(agentName); err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", agentName, err))
				if isJSONOutput() {
					results = append(results, map[string]interface{}{
						"agent":  agentName,
						"status": "error",
						"error":  err.Error(),
					})
				}
			} else if isJSONOutput() {
				results = append(results, map[string]interface{}{
					"agent":  agentName,
					"status": "success",
				})
			}
		}

		if isJSONOutput() {
			status := "success"
			if len(errs) > 0 {
				status = "partial"
			}
			return outputJSON(map[string]interface{}{
				"status":  status,
				"command": "delete",
				"results": results,
			})
		}

		if len(errs) > 0 {
			return fmt.Errorf("failed to delete some agents:\n  %s", strings.Join(errs, "\n  "))
		}
		return nil
	},
}

func deleteAgentsViaHub(hubCtx *HubContext, agentNames []string) error {
	PrintUsingHub(hubCtx.Endpoint)

	opts := &hubclient.DeleteAgentOptions{
		DeleteFiles:  true,
		RemoveBranch: !preserveBranch,
	}

	var errs []string
	var results []map[string]interface{}
	for _, agentName := range agentNames {
		statusf("Deleting agent '%s'...\n", agentName)

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)

		// Use grove-scoped client which supports agent lookup by name/slug
		if err := hubCtx.Client.ProjectAgents(hubCtx.ProjectID).Delete(ctx, agentName, opts); err != nil {
			cancel()
			errs = append(errs, fmt.Sprintf("%s: %v", agentName, wrapHubError(err)))
			if isJSONOutput() {
				results = append(results, map[string]interface{}{
					"agent":  agentName,
					"status": "error",
					"error":  err.Error(),
				})
			}
			continue
		}
		cancel()

		// Also clean up local agent files (worktree, agent directory).
		// The Hub dispatches container cleanup to the runtime broker, but local
		// filesystem artifacts must be removed by the CLI to avoid orphaned agents.
		branchDeleted, err := agent.DeleteAgentFiles(agentName, projectPath, !preserveBranch)
		if err != nil {
			statusf("Warning: Hub record deleted but local cleanup failed for '%s': %v\n", agentName, err)
			statusf("Run 'scion --no-hub delete %s' to retry targeted cleanup, or 'scion clean' to reset the grove.\n", agentName)
		}

		// Keep sync watermark current after a successful Hub delete. If hub server
		// time is unavailable in this flow, UpdateLastSyncedAt falls back to local UTC.
		if hubCtx != nil && hubCtx.ProjectPath != "" {
			hubsync.UpdateLastSyncedAt(hubCtx.ProjectPath, time.Time{}, hubCtx.IsGlobal)
			hubsync.RemoveSyncedAgent(hubCtx.ProjectPath, agentName)
		}
		if branchDeleted {
			statusf("Git branch associated with agent '%s' deleted.\n", agentName)
		}

		if isJSONOutput() {
			results = append(results, map[string]interface{}{
				"agent":  agentName,
				"status": "success",
			})
		} else {
			statusf("Agent '%s' deleted via Hub.\n", agentName)
		}
	}

	if isJSONOutput() {
		status := "success"
		if len(errs) > 0 {
			status = "partial"
		}
		return outputJSON(map[string]interface{}{
			"status":  status,
			"command": "delete",
			"results": results,
		})
	}

	if len(errs) > 0 {
		return fmt.Errorf("failed to delete some agents via Hub:\n  %s", strings.Join(errs, "\n  "))
	}
	return nil
}

func deleteStoppedViaHub(hubCtx *HubContext) error {
	PrintUsingHub(hubCtx.Endpoint)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	agentSvc := hubCtx.Client.ProjectAgents(hubCtx.ProjectID)
	resp, err := agentSvc.List(ctx, &hubclient.ListAgentsOptions{Phase: "stopped"})
	if err != nil {
		return wrapHubError(fmt.Errorf("failed to list agents via Hub: %w", err))
	}

	if len(resp.Agents) == 0 {
		statusln("No stopped agents found.")
		return nil
	}

	var agentNames []string
	for _, a := range resp.Agents {
		agentNames = append(agentNames, a.Name)
	}

	return deleteAgentsViaHub(hubCtx, agentNames)
}

func deleteAgentLocal(agentName string) error {
	rt := runtime.GetRuntime(projectPath, profile)
	mgr := agent.NewManager(rt)

	fmt.Printf("Deleting agent '%s'...\n", agentName)

	// We check if it exists in List to provide better feedback
	util.Debugf("delete: listing containers for %s", agentName)
	listStart := time.Now()
	agents, _ := mgr.List(context.Background(), map[string]string{"scion.name": agentName})
	util.Debugf("delete: container list completed in %v", time.Since(listStart))
	containerFound := false
	for _, a := range agents {
		if strings.EqualFold(a.Name, agentName) || a.ID == agentName || strings.EqualFold(strings.TrimPrefix(a.Name, "/"), agentName) {
			containerFound = true
			break
		}
	}

	if !containerFound {
		// Check if agent definition exists on the filesystem
		agentDirExists := false
		if projectDir, err := config.GetResolvedProjectDir(projectPath); err == nil {
			if _, err := os.Stat(filepath.Join(projectDir, "agents", agentName)); err == nil {
				agentDirExists = true
			}
		}
		if !agentDirExists {
			if globalDir, err := config.GetGlobalAgentsDir(); err == nil {
				if _, err := os.Stat(filepath.Join(globalDir, agentName)); err == nil {
					agentDirExists = true
				}
			}
		}
		if !agentDirExists {
			return fmt.Errorf("agent '%s' not found", agentName)
		}
		fmt.Println("No container found, removing agent definition...")
	}

	branchDeleted, err := mgr.Delete(context.Background(), agentName, true, projectPath, !preserveBranch)
	if err != nil {
		return err
	}

	if branchDeleted {
		fmt.Printf("Git branch associated with agent '%s' deleted.\n", agentName)
	}

	fmt.Printf("Agent '%s' deleted.\n", agentName)
	return nil
}

var preserveBranch bool

func init() {
	rootCmd.AddCommand(deleteCmd)
	deleteCmd.Flags().BoolVarP(&preserveBranch, "preserve-branch", "b", false, "Preserve the git branch associated with the worktree")
	deleteCmd.Flags().BoolVar(&deleteStopped, "stopped", false, "Delete all agents with stopped containers")
}
