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
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

var pendingSecretListJSON bool

var hubSecretPendingListCmd = &cobra.Command{
	Use:   "list",
	Short: "List secrets waiting for your approval",
	Long: `List the user secrets your agents saved that wait for your approval.

Shows each secret's name, type, target, the agent and project that wrote it,
and when it expires. Values are never shown.

Examples:
  scion hub secret pending list
  scion hub secret pending list --json`,
	Args: cobra.NoArgs,
	RunE: runSecretPendingList,
}

func init() {
	hubSecretPendingCmd.AddCommand(hubSecretPendingListCmd)
	hubSecretPendingListCmd.Flags().BoolVar(&pendingSecretListJSON, "json", false, "Output in JSON format")
}

// pendingSecretListOutput is the JSON shape of "scion hub secret pending list".
type pendingSecretListOutput struct {
	PendingSecrets []hubclient.PendingSecret `json:"pendingSecrets"`
}

func runSecretPendingList(_ *cobra.Command, _ []string) error {
	client, ctx, cancel, err := pendingSecretClient()
	if err != nil {
		return err
	}
	defer cancel()

	resp, err := client.Secrets().ListPending(ctx)
	if err != nil {
		return fmt.Errorf("failed to list pending secrets: %w", err)
	}

	if wantJSON(pendingSecretListJSON) {
		out := pendingSecretListOutput{PendingSecrets: resp.PendingSecrets}
		if out.PendingSecrets == nil {
			out.PendingSecrets = []hubclient.PendingSecret{}
		}
		return outputJSON(out)
	}

	if len(resp.PendingSecrets) == 0 {
		fmt.Println("No secrets are waiting for your approval")
		return nil
	}

	fmt.Println("Secrets waiting for your approval:")
	for _, p := range resp.PendingSecrets {
		fmt.Printf("\n%s  %s\n", p.ID, terminalText(p.Key))
		fmt.Printf("  Type:       %s\n", secretTypeLabel(p.Type))
		fmt.Printf("  Target:     %s\n", terminalText(p.Target))
		fmt.Printf("  Written by: agent %s (%s) in project %s (%s)\n",
			terminalText(p.AgentSlug), p.AgentID, terminalText(p.ProjectName), p.ProjectID)
		fmt.Printf("  Written at: %s\n", clitime.Format(p.Created, clitime.Full))
		if !p.Updated.Equal(p.Created) {
			fmt.Printf("  Replaced:   %s\n", clitime.Format(p.Updated, clitime.Full))
		}
		fmt.Printf("  Expires:    %s\n", clitime.Format(p.ExpiresAt, clitime.Full))
		effect := "delivered to your agents in all projects"
		if p.AllowProgeny {
			effect += ", and to agents they create"
		}
		fmt.Printf("  Effect:     %s\n", effect)
		if p.ReplacesExisting {
			fmt.Println("  Replaces:   your existing secret of this name")
		}
	}
	return nil
}
