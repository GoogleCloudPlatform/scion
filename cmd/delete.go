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
	"strings"
	"time"

	"github.com/ptone/scion-agent/pkg/agent"
	"github.com/ptone/scion-agent/pkg/config"
	"github.com/ptone/scion-agent/pkg/hubclient"
	"github.com/ptone/scion-agent/pkg/runtime"
	"github.com/ptone/scion-agent/pkg/util"
	"github.com/spf13/cobra"
)

var deleteStopped bool

// deleteCmd represents the delete command
var deleteCmd = &cobra.Command{
	Use:               "delete <agent> [agent...]",
	Aliases:           []string{"rm"},
	Short:             "Delete one or more agents",
	Long:              `Stop and remove one or more agent containers and their associated files and worktrees.`,
	ValidArgsFunction: getAgentNames,
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
		projectDir, _ := config.GetResolvedProjectDir(grovePath)
		if preserveBranch && !util.IsGitRepoDir(projectDir) {
			fmt.Println("Warning: --preserve-branch used outside a git repository; this flag has no effect.")
		}

		// For delete with --stopped, we can't specify a target agent
		// For multi-agent delete, pass the first agent name to exclude from sync requirements
		var targetAgent string
		if !deleteStopped && len(args) > 0 {
			targetAgent = args[0]
		}

		// Check if Hub should be used, excluding the target agent from sync requirements
		hubCtx, err := CheckHubAvailabilityForAgent(grovePath, targetAgent, false)
		if err != nil {
			return err
		}

		if deleteStopped {
			// --stopped flag with Hub is not yet supported
			if hubCtx != nil {
				return fmt.Errorf("--stopped flag is not yet supported when using Hub integration\n\nTo delete stopped agents locally, use: scion --no-hub delete --stopped")
			}

			rt := runtime.GetRuntime(grovePath, profile)
			mgr := agent.NewManager(rt)
			agents, err := mgr.List(context.Background(), nil)
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

				fmt.Printf("Deleting stopped agent '%s' (status: %s)...\n", agentName, a.ContainerStatus)

				targetGrovePath := a.GrovePath
				if targetGrovePath == "" {
					targetGrovePath = grovePath
				}

				branchDeleted, err := mgr.Delete(context.Background(), agentName, true, targetGrovePath, !preserveBranch)
				if err != nil {
					fmt.Printf("Failed to delete agent '%s': %v\n", agentName, err)
					continue
				}

				if branchDeleted {
					fmt.Printf("Git branch associated with agent '%s' deleted.\n", agentName)
				}
				fmt.Printf("Agent '%s' deleted.\n", agentName)
				deletedCount++
			}

			if deletedCount == 0 {
				fmt.Println("No stopped agents found.")
			}
			return nil
		}

		// Use Hub if available
		if hubCtx != nil {
			return deleteAgentsViaHub(hubCtx, args)
		}

		// Local mode - delete each agent
		var errors []string
		for _, agentName := range args {
			if err := deleteAgentLocal(agentName); err != nil {
				errors = append(errors, fmt.Sprintf("%s: %v", agentName, err))
			}
		}

		if len(errors) > 0 {
			return fmt.Errorf("failed to delete some agents:\n  %s", strings.Join(errors, "\n  "))
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

	var errors []string
	for _, agentName := range agentNames {
		fmt.Printf("Deleting agent '%s'...\n", agentName)

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)

		// Use grove-scoped client which supports agent lookup by name/slug
		if err := hubCtx.Client.GroveAgents(hubCtx.GroveID).Delete(ctx, agentName, opts); err != nil {
			cancel()
			errors = append(errors, fmt.Sprintf("%s: %v", agentName, wrapHubError(err)))
			continue
		}
		cancel()

		fmt.Printf("Agent '%s' deleted via Hub.\n", agentName)
	}

	if len(errors) > 0 {
		return fmt.Errorf("failed to delete some agents via Hub:\n  %s", strings.Join(errors, "\n  "))
	}
	return nil
}

func deleteAgentLocal(agentName string) error {
	effectiveProfile := profile
	if effectiveProfile == "" {
		effectiveProfile = agent.GetSavedRuntime(agentName, grovePath)
	}

	rt := runtime.GetRuntime(grovePath, effectiveProfile)
	mgr := agent.NewManager(rt)

	fmt.Printf("Deleting agent '%s'...\n", agentName)

	// We check if it exists in List to provide better feedback
	agents, _ := mgr.List(context.Background(), map[string]string{"scion.name": agentName})
	containerFound := false
	for _, a := range agents {
		if a.Name == agentName || a.ID == agentName || strings.TrimPrefix(a.Name, "/") == agentName {
			containerFound = true
			break
		}
	}

	if !containerFound {
		fmt.Println("No container found, removing agent definition...")
	}

	branchDeleted, err := mgr.Delete(context.Background(), agentName, true, grovePath, !preserveBranch)
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
