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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

var logsTail int

// logsCmd represents the logs command
var logsCmd = &cobra.Command{
	Use:               "logs <agent>",
	Short:             "Get logs of an agent",
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: getAgentNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		agentName := api.Slugify(args[0])
		hubCtx, err := requireHub(projectPath)
		if err != nil {
			return err
		}
		return getHubLogs(cmd.Context(), hubCtx, agentName)
	},
}

func init() {
	rootCmd.AddCommand(logsCmd)
	logsCmd.Flags().IntVarP(&logsTail, "tail", "n", 100, "Number of lines from end")
}

// getHubLogs retrieves agent logs via the hub relay (hub -> broker -> agent.log).
func getHubLogs(ctx context.Context, hubCtx *HubContext, agentName string) error {
	PrintUsingHub(hubCtx.Endpoint)

	tctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	agent, err := hubCtx.Client.ProjectAgents(hubCtx.ProjectID).Get(tctx, agentName)
	if err != nil {
		return wrapHubError(fmt.Errorf("failed to get agent '%s': %w", agentName, err))
	}

	if strings.HasPrefix(agent.Runtime, "managed:") {
		fmt.Println("Managed agent logs are available in GCP Cloud Logging.")
		fmt.Println()
		fmt.Println("View logs in the Google Cloud Console:")
		fmt.Println("  https://console.cloud.google.com/logs")
		fmt.Println()
		fmt.Println("Use scion look to view the agent's current interaction output.")
		return nil
	}

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
