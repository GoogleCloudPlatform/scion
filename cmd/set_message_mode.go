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
	"encoding/json"
	"fmt"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

var (
	setMessageModeCascade bool
	setMessageModeDryRun  bool
)

// setMessageModeCmd represents the set-message-mode command
var setMessageModeCmd = &cobra.Command{
	Use:   "set-message-mode <agent> <mode>",
	Short: "Change an agent's messaging mode",
	Long: `Change an agent's messaging mode.

Valid modes:
  none     - No messaging (sealed)
  lineage  - Ancestry users and project owners only
  branch   - Parent/child agents and ancestry users
  project  - All agents and users in the project (default)
  hub      - Project messaging plus permitted agents in other projects
             on this Hub (granting it requires permission)

Branch rule: two agents can message each other in branch mode only when
both are in branch mode and one is the other's direct parent. Siblings,
grandparents and grandchildren are denied, as is any pair that mixes
branch with another mode.

Default for a new agent (applied once, at creation):
  1. the --message-mode flag of scion create or scion start
  2. the template's message_mode
  3. the parent agent's mode, when an agent creates the agent
  4. project
A default that resolves to hub is still a new grant and needs permission.
This command changes the mode of an existing agent; it does not re-run
the defaults.

Use --cascade to apply the mode to all descendants.
Use --dry-run to preview cascade effects without applying.`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		agentName := api.Slugify(args[0])
		mode := args[1]

		// Client-side validation
		if err := validateMessageMode(mode); err != nil {
			return asUsageError(err)
		}

		hubCtx, err := CheckHubAvailabilityForAgent(projectPath, agentName, false)
		if err != nil {
			return err
		}
		if hubCtx == nil {
			return fmt.Errorf("set-message-mode requires Hub mode")
		}

		PrintUsingHub(hubCtx.Endpoint)

		projectID, err := GetProjectID(hubCtx)
		if err != nil {
			return wrapHubError(err)
		}

		ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
		defer cancel()

		agentSvc := hubCtx.Client.ProjectAgents(projectID)

		req := &hubclient.SetMessageModeRequest{
			Mode:    mode,
			Cascade: setMessageModeCascade || setMessageModeDryRun,
		}

		opts := &hubclient.SetMessageModeOptions{
			DryRun: setMessageModeDryRun,
		}

		resp, err := agentSvc.SetMessageMode(ctx, agentName, req, opts)
		if err != nil {
			return wrapHubError(fmt.Errorf("failed to set message mode: %w", err))
		}

		if isJSONOutput() {
			return outputJSON(resp)
		}

		if setMessageModeDryRun {
			statusf("Dry-run: would change agent %q from %q to %q\n", resp.AgentID, resp.Previous, resp.Mode)
			if len(resp.Cascade) > 0 {
				var cascade struct {
					Count   int `json:"count"`
					Details []struct {
						AgentName   string `json:"agent_name"`
						CurrentMode string `json:"current_mode"`
						NewMode     string `json:"new_mode"`
					} `json:"details"`
				}
				if err := json.Unmarshal(resp.Cascade, &cascade); err == nil {
					statusf("Cascade would affect %d agent(s):\n", cascade.Count)
					for _, d := range cascade.Details {
						statusf("  %s: %s → %s\n", d.AgentName, d.CurrentMode, d.NewMode)
					}
				}
			}
		} else {
			if resp.Previous == resp.Mode {
				statusf("Agent %q is already in %q mode (no-op).\n", resp.AgentID, resp.Mode)
			} else {
				statusf("Agent %q message mode changed: %s → %s\n", resp.AgentID, resp.Previous, resp.Mode)
			}
			if len(resp.Cascade) > 0 {
				var cascade struct {
					Count int `json:"count"`
				}
				if err := json.Unmarshal(resp.Cascade, &cascade); err == nil && cascade.Count > 0 {
					statusf("Cascaded to %d descendant(s).\n", cascade.Count)
				}
			}
		}

		return nil
	},
}

func init() {
	setMessageModeCmd.Flags().BoolVar(&setMessageModeCascade, "cascade", false, "Apply the mode change to all descendant agents")
	setMessageModeCmd.Flags().BoolVar(&setMessageModeDryRun, "dry-run", false, "Preview cascade effects without applying changes")
	rootCmd.AddCommand(setMessageModeCmd)
}
