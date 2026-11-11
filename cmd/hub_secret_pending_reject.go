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

	"github.com/spf13/cobra"
)

var hubSecretPendingRejectCmd = &cobra.Command{
	Use:   "reject ID",
	Short: "Reject a secret an agent saved for your account",
	Long: `Reject a pending secret, by the ID 'scion hub secret pending list' shows.

The pending secret is deleted. Any existing secret of the same name is kept.

Examples:
  scion hub secret pending reject 0b6f...`,
	Args: cobra.ExactArgs(1),
	RunE: runSecretPendingReject,
}

func init() {
	hubSecretPendingCmd.AddCommand(hubSecretPendingRejectCmd)
}

func runSecretPendingReject(_ *cobra.Command, args []string) error {
	id := args[0]
	if id == "" {
		return newUsageError("ID cannot be empty")
	}
	client, ctx, cancel, err := pendingSecretClient()
	if err != nil {
		return err
	}
	defer cancel()

	resp, err := client.Secrets().RejectPending(ctx, id)
	if err != nil {
		return fmt.Errorf("failed to reject pending secret: %w", err)
	}
	fmt.Printf("Rejected secret %s\n", resp.Key)
	return nil
}
