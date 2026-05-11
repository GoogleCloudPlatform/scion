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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/spf13/cobra"
)

var (
	logsTail   int
	logsFollow bool
)

// logsCmd represents the logs command
var logsCmd = &cobra.Command{
	Use:               "logs <agent>",
	Short:             "Get logs of an agent",
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: getAgentNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		agentName := api.Slugify(args[0])

		// Validate --follow restrictions
		if logsFollow {
			if nonInteractive {
				return fmt.Errorf("--follow is not supported in non-interactive mode")
			}
		}

		// Check if Hub is enabled
		hubCtx, err := CheckHubAvailabilityWithOptions(projectPath, true)
		if err != nil {
			return err
		}
		if hubCtx != nil {
			if logsFollow {
				return fmt.Errorf("--follow is not supported in hub mode")
			}
			return getHubLogs(cmd.Context(), hubCtx, agentName)
		}

		// Local mode: read from filesystem
		rt := runtime.GetRuntime(projectPath, profile)

		// Find the agent to get its grove path
		agents, err := rt.List(context.Background(), map[string]string{
			"scion.agent": "true",
			"scion.name":  agentName,
		})
		if err != nil {
			return fmt.Errorf("failed to find agent %s: %w", agentName, err)
		}
		if len(agents) == 0 {
			return fmt.Errorf("agent %s not found", agentName)
		}

		a := agents[0]
		if a.ProjectPath == "" {
			return fmt.Errorf("agent %s has no project path configured", agentName)
		}

		agentLogPath := filepath.Join(a.ProjectPath, "agents", agentName, "home", "agent.log")
		if _, err := os.Stat(agentLogPath); os.IsNotExist(err) {
			return fmt.Errorf("log file not found: %s\n\nThe agent may not have started yet or does not produce logs", agentLogPath)
		}

		data, err := os.ReadFile(agentLogPath)
		if err != nil {
			return fmt.Errorf("failed to read log file: %w", err)
		}

		fmt.Print(string(data))
		return nil
	},
}

func init() {
	rootCmd.AddCommand(logsCmd)
	logsCmd.Flags().IntVarP(&logsTail, "tail", "n", 100, "Number of lines from end")
	logsCmd.Flags().BoolVarP(&logsFollow, "follow", "f", false, "Stream logs in real-time")
}

// getHubLogs retrieves agent logs via the hub relay (hub -> broker -> agent.log).
func getHubLogs(ctx context.Context, hubCtx *HubContext, agentName string) error {
	PrintUsingHub(hubCtx.Endpoint)

	client := hubCtx.Client.ProjectAgents(hubCtx.ProjectID)

	logs, err := client.GetLogs(ctx, agentName, &hubclient.GetLogsOptions{
		Tail: logsTail,
	})
	if err != nil {
		return err
	}

	fmt.Print(logs)
	return nil
}
