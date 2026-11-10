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
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

var pendingSecretShowJSON bool

var hubSecretPendingShowCmd = &cobra.Command{
	Use:   "show ID",
	Short: "Show a secret waiting for your approval",
	Long: `Show one pending secret, by the ID 'scion hub secret pending list' shows.

Shows its name, type, target, value length, the agent and project that wrote
it, when it was written and expires, what approving it would do, and the
project secrets of the same name it would override. The value is not shown;
use 'scion hub secret pending reveal' for that.

Examples:
  scion hub secret pending show 0b6f...
  scion hub secret pending show 0b6f... --json`,
	Args: cobra.ExactArgs(1),
	RunE: runSecretPendingShow,
}

func init() {
	hubSecretPendingCmd.AddCommand(hubSecretPendingShowCmd)
	hubSecretPendingShowCmd.Flags().BoolVar(&pendingSecretShowJSON, "json", false, "Output in JSON format")
}

func runSecretPendingShow(_ *cobra.Command, args []string) error {
	id := args[0]
	if id == "" {
		return newUsageError("ID cannot be empty")
	}
	client, ctx, cancel, err := pendingSecretClient()
	if err != nil {
		return err
	}
	defer cancel()

	resp, err := client.Secrets().ListWithPending(ctx)
	if err != nil {
		return fmt.Errorf("failed to show pending secret: %w", err)
	}
	var item *hubclient.PendingSecretItem
	for _, it := range resp.Items {
		if it.Pending != nil && it.Pending.ID == id {
			item = it.Pending
			break
		}
	}
	if item == nil {
		return fmt.Errorf("pending secret %s not found (it may have been approved, rejected or expired)", id)
	}
	if item.OverridesProjectSecrets == nil {
		item.OverridesProjectSecrets = []hubclient.ProjectSecretOverride{}
	}

	if wantJSON(pendingSecretShowJSON) {
		return outputJSON(item)
	}

	p := item.PendingSecret
	fmt.Printf("%s  %s\n", p.ID, p.Key)
	fmt.Printf("  Type:       %s\n", secretTypeLabel(p.Type))
	fmt.Printf("  Target:     %s\n", p.Target)
	fmt.Printf("  Length:     %d bytes\n", p.ValueLen)
	fmt.Printf("  Written by: agent %s (%s) in project %s (%s)\n", p.AgentSlug, p.AgentID, p.ProjectName, p.ProjectID)
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
	if len(item.OverridesProjectSecrets) > 0 {
		names := make([]string, 0, len(item.OverridesProjectSecrets))
		for _, o := range item.OverridesProjectSecrets {
			names = append(names, o.ProjectName)
		}
		fmt.Printf("  Overrides:  project secret %s in %s\n", p.Key, strings.Join(names, ", "))
	}
	if p.ReplacesExisting {
		fmt.Println("  Replaces:   your existing secret of this name")
	}
	if p.Description != "" {
		fmt.Printf("  Description: %s\n", p.Description)
	}
	return nil
}
