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
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

var projectMessagingProject string

// projectMessagingCmd is the parent command for project messaging policy operations.
var projectMessagingCmd = &cobra.Command{
	Use:   "messaging",
	Short: "Manage project messaging policy",
	Long: `View and update a project's cross-project messaging inbound policy.

The inbound policy controls which external agents can message agents
in this project:
  - none:    No cross-project inbound messages (default)
  - members: Only agents whose origin user is a member of this project
  - any:     Any agent on the Hub whose mode permits sending

Requires project ownership or Hub admin.`,
}

// projectMessagingGetCmd retrieves the current project messaging policy.
var projectMessagingGetCmd = &cobra.Command{
	Use:   "get",
	Short: "Get current project messaging policy",
	Long: `Display the current project messaging policy.

Shows the configured and effective cross-project inbound policy.
The effective policy accounts for the hub-level cross-project switch:
when the hub switch is off, the effective policy is always "none".`,
	RunE: func(cmd *cobra.Command, args []string) error {
		settings, client, err := loadHubClient()
		if err != nil {
			return err
		}

		projectID, err := resolveProjectID(settings, projectMessagingProject)
		if err != nil {
			return err
		}

		ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
		defer cancel()

		policy, err := client.Messaging().GetProjectMessagingPolicy(ctx, projectID)
		if err != nil {
			return fmt.Errorf("failed to get project messaging policy: %w", err)
		}

		if hubOutputJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(policy)
		}

		fmt.Printf("Project Messaging Policy (revision %d):\n", policy.Revision)
		fmt.Printf("  cross_project_inbound:           %s\n", policy.CrossProjectInbound)
		fmt.Printf("  effective_cross_project_inbound:  %s\n", policy.EffectiveCrossProjectInbound)
		fmt.Printf("  hub_cross_project_enabled:        %v\n", policy.HubCrossProjectEnabled)
		return nil
	},
}

var (
	projectMessagingSetPolicy   string
	projectMessagingSetRevision int64
)

// projectMessagingSetCmd updates the project messaging policy.
var projectMessagingSetCmd = &cobra.Command{
	Use:   "set",
	Short: "Update project messaging policy",
	Long: `Update a project's cross-project messaging inbound policy.

Requires --revision for compare-and-swap (CAS) protection.

Valid policies:
  - none:    No cross-project inbound messages
  - members: Accept from agents whose origin user is a project member
  - any:     Accept from any Hub agent with appropriate mode

Examples:
  # Allow member-origin agents to message into this project
  scion project messaging set --policy members --revision 0

  # Open project to all Hub agents
  scion project messaging set --policy any --revision 1`,
	RunE: func(cmd *cobra.Command, args []string) error {
		settings, client, err := loadHubClient()
		if err != nil {
			return err
		}

		projectID, err := resolveProjectID(settings, projectMessagingProject)
		if err != nil {
			return err
		}

		ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
		defer cancel()

		switch projectMessagingSetPolicy {
		case "none", "members", "any":
			// valid
		default:
			return fmt.Errorf("invalid policy %q: must be none, members, or any", projectMessagingSetPolicy)
		}

		rev := projectMessagingSetRevision
		if !cmd.Flags().Changed("revision") {
			// Auto-fetch current revision for CAS if not explicitly provided.
			current, fetchErr := client.Messaging().GetProjectMessagingPolicy(ctx, projectID)
			if fetchErr != nil {
				return fmt.Errorf("failed to auto-fetch current revision: %w", fetchErr)
			}
			rev = current.Revision
		}

		req := &hubclient.UpdateProjectMessagingPolicyRequest{
			CrossProjectInbound: projectMessagingSetPolicy,
			ExpectedRevision:    rev,
		}

		result, err := client.Messaging().UpdateProjectMessagingPolicy(ctx, projectID, req)
		if err != nil {
			return fmt.Errorf("failed to update project messaging policy: %w", err)
		}

		if hubOutputJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(result)
		}

		fmt.Printf("Project Messaging Policy updated (revision %d):\n", result.Revision)
		fmt.Printf("  cross_project_inbound:           %s\n", result.CrossProjectInbound)
		fmt.Printf("  effective_cross_project_inbound:  %s\n", result.EffectiveCrossProjectInbound)
		fmt.Printf("  hub_cross_project_enabled:        %v\n", result.HubCrossProjectEnabled)
		return nil
	},
}

func init() {
	projectCmd.AddCommand(projectMessagingCmd)
	projectMessagingCmd.AddCommand(projectMessagingGetCmd)
	projectMessagingCmd.AddCommand(projectMessagingSetCmd)

	projectMessagingCmd.PersistentFlags().StringVar(&projectMessagingProject, "project", "", "Project ID or slug")

	projectMessagingSetCmd.Flags().StringVar(&projectMessagingSetPolicy, "policy", "", "Inbound policy: none, members, or any")
	projectMessagingSetCmd.Flags().Int64Var(&projectMessagingSetRevision, "revision", 0, "Expected revision for CAS")
	_ = projectMessagingSetCmd.MarkFlagRequired("policy")
}
