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

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/wsclient"
	"github.com/spf13/cobra"
)

// attachCmd represents the attach command
var attachCmd = &cobra.Command{
	Use:   "attach <agent>",
	Short: "Attach to an agent's interactive session",
	Long: `Attach to the interactive session of a running agent.
If the agent was started with tmux support, this will attach to the tmux session.`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: getAgentNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		agentName := api.Slugify(args[0])

		// Check if Hub is enabled
		hubCtx, err := CheckHubAvailabilityForAgent(projectPath, agentName, false)
		if err != nil {
			return err
		}

		if hubCtx != nil {
			return attachViaHub(hubCtx, agentName)
		}

		// Try to resolve grove info for better error messages
		projectDir, _ := config.GetResolvedProjectDir(projectPath)
		projectName := config.GetProjectName(projectDir)
		targetProjectPath:= projectPath

		// Verify agent exists
		found := false
		if projectDir != "" {
			agentDir := filepath.Join(projectDir, "agents", agentName)
			if _, err := os.Stat(filepath.Join(agentDir, "scion-agent.json")); err == nil {
				found = true
			}
		}

		if !found {
			// If user didn't specify a grove, try global fallback
			if projectPath == "" {
				globalDir, _ := config.GetGlobalDir()
				if globalDir != "" && globalDir != projectDir {
					globalAgentDir := filepath.Join(globalDir, "agents", agentName)
					if _, err := os.Stat(filepath.Join(globalAgentDir, "scion-agent.json")); err == nil {
						found = true
						targetProjectPath = globalDir
						// Update display info
						projectDir = globalDir
						projectName = "global"
						fmt.Printf("Agent '%s' not found in local project, using global agent.\n", agentName)
					}
				}
			}
		}

		if !found {
			return fmt.Errorf("agent '%s' not found in project '%s'", agentName, projectName)
		}

		rt := runtime.GetRuntime(targetProjectPath, profile)

		// Use grove-scoped lookup to find the exact container,
		// preventing cross-grove collision when agents share a name.
		filter := map[string]string{"scion.name": agentName, "scion.project": projectName}
		agents, listErr := rt.List(context.Background(), filter)
		attachID := agentName
		if listErr == nil && len(agents) > 0 {
			attachID = agents[0].ContainerID
		}

		fmt.Printf("Attaching to agent '%s' (project: %s)...\n", agentName, projectName)
		err = rt.Attach(context.Background(), attachID)
		if err != nil {
			// If the error is "not found", we can augment it with grove info
			if err.Error() == fmt.Sprintf("agent '%s' not found", attachID) ||
				err.Error() == fmt.Sprintf("agent '%s' container not found. It may have exited and been removed.", attachID) {
				return fmt.Errorf("agent '%s' not found in project '%s'", agentName, projectName)
			}
			return err
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(attachCmd)
}

// attachViaHub attaches to an agent via Hub WebSocket connection.
func attachViaHub(hubCtx *HubContext, agentName string) error {
	PrintUsingHub(hubCtx.Endpoint)

	// Get the grove ID for this project
	projectID, err := GetProjectID(hubCtx)
	if err != nil {
		return wrapHubError(err)
	}

	// Get agent details from Hub to verify it exists and is running
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	agent, err := hubCtx.Client.ProjectAgents(projectID).Get(ctx, agentName)
	if err != nil {
		return wrapHubError(fmt.Errorf("failed to get agent '%s': %w", agentName, err))
	}

	// Check agent lifecycle status - the agent must be running to attach.
	agentPhase, _ := hubAgentPhaseActivity(agent.Phase, agent.Activity, agent.Status)
	if agentPhase != string(state.PhaseRunning) {
		// Build a helpful error message with available status info
		statusInfo := agent.Status
		if statusInfo == "" {
			statusInfo = "unknown"
		}
		if agent.ContainerStatus != "" {
			statusInfo += fmt.Sprintf(", container: %s", agent.ContainerStatus)
		}
		return fmt.Errorf("agent '%s' is not running (phase: %s)\n\nStart the agent first with: scion start %s",
			agentName, statusInfo, agentName)
	}

	// Get access token for WebSocket authentication
	token := getHubAccessToken(hubCtx.Endpoint)
	if token == "" {
		return fmt.Errorf("no access token found for Hub\n\nPlease login first: scion hub auth login")
	}

	fmt.Printf("Attaching to agent '%s' via Hub...\n", agentName)

	// Connect via WebSocket
	// Use agent UUID for the PTY endpoint
	agentID := agent.ID
	if agentID == "" {
		agentID = agentName // Fall back to name if ID not set
	}

	return wsclient.AttachToAgent(context.Background(), hubCtx.Endpoint, token, agentID)
}
