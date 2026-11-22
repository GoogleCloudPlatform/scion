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

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

var brokerUpdateAdminManaged bool

var brokerUpdateCmd = &cobra.Command{
	Use:   "update <broker>",
	Short: "Update a Runtime Broker's settings on the Hub",
	Long: `Update a Runtime Broker's administrative attributes on the Hub.

Examples:
  # Approve an existing broker as admin-managed
  scion runtime-broker update my-broker --admin-managed=true

  # Revoke admin-managed approval from a broker
  scion runtime-broker update my-broker --admin-managed=false`,
	Args: cobra.ExactArgs(1),
	RunE: runBrokerUpdate,
}

func init() {
	brokerCmd.AddCommand(brokerUpdateCmd)
	brokerUpdateCmd.Flags().BoolVar(&brokerUpdateAdminManaged, "admin-managed", false, "Set whether this broker is approved as admin-managed (true or false; requires broker.manage and experiment hub.admin_managed_brokers)")
}

func runBrokerUpdate(cmd *cobra.Command, args []string) error {
	if !cmd.Flags().Changed("admin-managed") {
		return fmt.Errorf("at least one update flag is required (e.g. --admin-managed=true|false)")
	}

	resolvedPath, _, err := config.ResolveProjectPath(projectPath)
	if err != nil {
		return fmt.Errorf("failed to resolve project path: %w", err)
	}

	settings, err := config.LoadSettings(resolvedPath)
	if err != nil {
		return fmt.Errorf("failed to load settings: %w", err)
	}

	client, err := getHubClient(settings)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	return runBrokerUpdateWithClient(ctx, cmd, client, args[0], brokerUpdateAdminManaged)
}

func runBrokerUpdateWithClient(ctx context.Context, cmd *cobra.Command, client hubclient.Client, brokerNameOrID string, adminManaged bool) error {
	broker, err := resolveBrokerByNameOrID(ctx, client, brokerNameOrID)
	if err != nil {
		return err
	}

	req := &hubclient.UpdateBrokerRequest{
		AdminManaged: &adminManaged,
	}
	updated, err := client.RuntimeBrokers().Update(ctx, broker.ID, req)
	if err != nil {
		return err
	}

	if isJSONOutput() {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(updated)
	}

	state := "revoked (adminManaged=false)"
	if updated.AdminManaged {
		state = "approved (adminManaged=true)"
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Updated runtime broker %q (%s): %s\n", updated.Name, updated.ID, state)
	return nil
}
