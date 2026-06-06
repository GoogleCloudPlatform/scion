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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/spf13/cobra"
)

var resetAuthCmd = &cobra.Command{
	Use:   "reset-auth <agent>",
	Short: "Reset authentication for a running agent",
	Long: `Inject a fresh Hub token into a running agent without restarting it.

This is useful when an agent's token has expired and cannot be refreshed
(e.g., after hub signing key rotation). The command generates a new token
on the Hub, pushes it into the agent's container, and signals the agent
to restart its token refresh loop.

The agent must be running — stopped agents get a fresh token on next start.`,
	Args: cobra.ExactArgs(1),
	ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return getAgentNames(cmd, args, toComplete)
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		agentName := api.Slugify(args[0])

		hubCtx, err := CheckHubAvailability(projectPath)
		if err != nil {
			return err
		}
		if hubCtx == nil {
			return fmt.Errorf("reset-auth requires Hub connectivity (hub not configured)")
		}

		return resetAuthViaHub(hubCtx, agentName)
	},
}

func init() {
	rootCmd.AddCommand(resetAuthCmd)
}

func resetAuthViaHub(hubCtx *HubContext, agentName string) error {
	PrintUsingHub(hubCtx.Endpoint)
	statusf("Resetting auth for agent '%s'...\n", agentName)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	projectID, err := GetProjectID(hubCtx)
	if err != nil {
		return wrapHubError(err)
	}

	agentSvc := hubCtx.Client.ProjectAgents(projectID)
	if err := agentSvc.ResetAuth(ctx, agentName); err != nil {
		return wrapHubError(fmt.Errorf("failed to reset auth via Hub: %w", err))
	}

	statusf("Auth reset dispatched for agent '%s'. The agent will pick up the new token shortly.\n", agentName)
	return nil
}
