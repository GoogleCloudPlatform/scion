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

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

var pendingSecretApproveNoProgeny bool

var hubSecretPendingApproveCmd = &cobra.Command{
	Use:   "approve ID",
	Short: "Approve a secret an agent saved for your account",
	Long: `Approve a pending secret, by the ID 'scion hub secret pending list' shows.

The secret is written to your user scope exactly as the agent saved it. It is
delivered to your agents in every project from their next start. Use
--no-progeny to keep it from agents those agents create.

Examples:
  scion hub secret pending approve 0b6f...
  scion hub secret pending approve 0b6f... --no-progeny`,
	Args: cobra.ExactArgs(1),
	RunE: runSecretPendingApprove,
}

func init() {
	hubSecretPendingCmd.AddCommand(hubSecretPendingApproveCmd)
	hubSecretPendingApproveCmd.Flags().BoolVar(&pendingSecretApproveNoProgeny, "no-progeny", false, "Do not share the secret with agents created by your agents")
}

func runSecretPendingApprove(_ *cobra.Command, args []string) error {
	id := args[0]
	if id == "" {
		return newUsageError("ID cannot be empty")
	}
	client, ctx, cancel, err := pendingSecretClient()
	if err != nil {
		return err
	}
	defer cancel()

	req := &hubclient.ApprovePendingSecretRequest{}
	if pendingSecretApproveNoProgeny {
		off := false
		req.AllowProgeny = &off
	}
	resp, err := client.Secrets().ApprovePending(ctx, id, req)
	if err != nil {
		return fmt.Errorf("failed to approve pending secret: %w", err)
	}
	fmt.Printf("Approved secret %s (scope: %s)\n", resp.Key, resp.Scope)
	return nil
}
