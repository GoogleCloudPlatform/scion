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

// restoreCmd represents the restore command
var restoreCmd = &cobra.Command{
	Use:               "restore <agent>",
	Short:             "Restore a soft-deleted agent",
	Long:              `Restore a previously soft-deleted agent, making it visible in listings again.`,
	ValidArgsFunction: getAgentNames,
	Args:              cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		agentName := api.Slugify(args[0])

		hubCtx, err := CheckHubAvailability(projectPath)
		if err != nil {
			return err
		}

		if hubCtx == nil {
			return fmt.Errorf("restore requires Hub integration (soft-delete is a Hub feature)")
		}

		PrintUsingHub(hubCtx.Endpoint)

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		agent, err := hubCtx.Client.ProjectAgents(hubCtx.ProjectID).Restore(ctx, agentName)
		if err != nil {
			return wrapHubError(fmt.Errorf("failed to restore agent: %w", err))
		}

		phase, _ := hubAgentPhaseActivity(agent.Phase, agent.Activity, agent.Status)
		statusf("Agent '%s' restored (phase: %s).\n", agent.Name, phase)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(restoreCmd)
}
