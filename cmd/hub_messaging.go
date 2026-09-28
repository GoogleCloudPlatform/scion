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

// hubMessagingCmd is the parent command for hub messaging admin operations.
var hubMessagingCmd = &cobra.Command{
	Use:   "messaging",
	Short: "Manage Hub messaging settings",
	Long: `View and update Hub-level messaging settings.

These settings control hub-wide messaging features including the
conversation envelope switch and cross-project messaging enable flag.

Requires Hub admin privileges.`,
}

// hubMessagingGetCmd retrieves the current hub messaging settings.
var hubMessagingGetCmd = &cobra.Command{
	Use:   "get",
	Short: "Get current Hub messaging settings",
	Long: `Display the current Hub messaging settings.

Shows:
  - conversation_envelope_switch: whether the conversation envelope is active
  - cross_project_messaging_enabled: whether cross-project agent messaging is enabled
  - revision: settings revision (used for CAS on updates)`,
	RunE: func(cmd *cobra.Command, args []string) error {
		_, client, err := loadHubClient()
		if err != nil {
			return err
		}

		ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
		defer cancel()

		settings, err := client.Messaging().GetHubMessagingSettings(ctx)
		if err != nil {
			return fmt.Errorf("failed to get hub messaging settings: %w", err)
		}

		if hubOutputJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(settings)
		}

		fmt.Printf("Hub Messaging Settings (revision %d):\n", settings.Revision)
		if settings.ConversationEnvelopeSwitch != nil {
			fmt.Printf("  conversation_envelope_switch:    %v\n", *settings.ConversationEnvelopeSwitch)
		}
		if settings.CrossProjectMessagingEnabled != nil {
			fmt.Printf("  cross_project_messaging_enabled: %v\n", *settings.CrossProjectMessagingEnabled)
		}
		return nil
	},
}

var (
	hubMessagingSetCrossProject *bool
	hubMessagingSetEnvelope     *bool
	hubMessagingSetRevision     int64
)

// hubMessagingSetCmd updates the hub messaging settings.
var hubMessagingSetCmd = &cobra.Command{
	Use:   "set",
	Short: "Update Hub messaging settings",
	Long: `Update Hub-level messaging settings.

Security-critical flags (cross_project_messaging_enabled) require --revision
for compare-and-swap (CAS) protection against concurrent edits.

Examples:
  # Enable cross-project messaging
  scion hub messaging set --cross-project-enabled=true --revision 3

  # Disable conversation envelope
  scion hub messaging set --envelope-switch=false`,
	RunE: func(cmd *cobra.Command, args []string) error {
		_, client, err := loadHubClient()
		if err != nil {
			return err
		}

		ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
		defer cancel()

		req := &hubclient.UpdateHubMessagingRequest{}

		if cmd.Flags().Changed("cross-project-enabled") {
			val := *hubMessagingSetCrossProject
			req.CrossProjectMessagingEnabled = &val
			rev := hubMessagingSetRevision
			if !cmd.Flags().Changed("revision") {
				// Auto-fetch current revision for CAS if not explicitly provided.
				current, fetchErr := client.Messaging().GetHubMessagingSettings(ctx)
				if fetchErr != nil {
					return fmt.Errorf("failed to auto-fetch current revision: %w", fetchErr)
				}
				rev = current.Revision
			}
			req.ExpectedRevision = &rev
		}
		if cmd.Flags().Changed("envelope-switch") {
			val := *hubMessagingSetEnvelope
			req.ConversationEnvelopeSwitch = &val
		}

		result, err := client.Messaging().UpdateHubMessagingSettings(ctx, req)
		if err != nil {
			return fmt.Errorf("failed to update hub messaging settings: %w", err)
		}

		if hubOutputJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(result)
		}

		fmt.Printf("Hub Messaging Settings updated (revision %d):\n", result.Revision)
		if result.ConversationEnvelopeSwitch != nil {
			fmt.Printf("  conversation_envelope_switch:    %v\n", *result.ConversationEnvelopeSwitch)
		}
		if result.CrossProjectMessagingEnabled != nil {
			fmt.Printf("  cross_project_messaging_enabled: %v\n", *result.CrossProjectMessagingEnabled)
		}
		return nil
	},
}

func init() {
	hubCmd.AddCommand(hubMessagingCmd)
	hubMessagingCmd.AddCommand(hubMessagingGetCmd)
	hubMessagingCmd.AddCommand(hubMessagingSetCmd)

	hubMessagingSetCrossProject = hubMessagingSetCmd.Flags().Bool("cross-project-enabled", false, "Enable/disable cross-project messaging")
	hubMessagingSetEnvelope = hubMessagingSetCmd.Flags().Bool("envelope-switch", true, "Enable/disable conversation envelope")
	hubMessagingSetCmd.Flags().Int64Var(&hubMessagingSetRevision, "revision", 0, "Expected revision for CAS (required for cross-project changes)")
}
