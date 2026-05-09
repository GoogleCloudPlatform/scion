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

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

var saOutputJSON bool

var projectServiceAccountsCmd = &cobra.Command{
	Use:     "service-accounts",
	Aliases: []string{"sa"},
	Short:   "Manage GCP service accounts for a project",
	Long: `Manage GCP service accounts registered for use by agents in a project.

Service accounts are registered with the Hub and used to provide agents
with transparent GCP identity via metadata server emulation. No key
material is stored — the Hub impersonates the SA at token-generation time.

Examples:
  scion project service-accounts list
  scion project service-accounts add agent-worker@project.iam.gserviceaccount.com --project my-project
  scion project service-accounts verify <id>
  scion project service-accounts remove <id>`,
}

var saAddCmd = &cobra.Command{
	Use:   "add EMAIL",
	Short: "Register a GCP service account",
	Long: `Register a GCP service account for use by agents in this project.

The Hub will verify it can impersonate this service account via the
IAM Credentials API. The Hub's own service account must have
roles/iam.serviceAccountTokenCreator on the target SA.

Examples:
  scion project service-accounts add agent-worker@my-project.iam.gserviceaccount.com --project my-project
  scion project service-accounts add agent-worker@my-project.iam.gserviceaccount.com --project my-project --name "Worker SA"`,
	Args: cobra.ExactArgs(1),
	RunE: runSAAdd,
}

var saListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List registered GCP service accounts",
	Long: `List all GCP service accounts registered for this project.

Examples:
  scion project service-accounts list
  scion project service-accounts list --json`,
	Args: cobra.NoArgs,
	RunE: runSAList,
}

var saRemoveCmd = &cobra.Command{
	Use:     "remove ID",
	Aliases: []string{"rm", "delete"},
	Short:   "Remove a GCP service account registration",
	Long: `Remove a registered GCP service account from this project.

This does not delete the service account in GCP — it only removes the
registration from the Hub.

Examples:
  scion project service-accounts remove <id>`,
	Args: cobra.ExactArgs(1),
	RunE: runSARemove,
}

var saVerifyCmd = &cobra.Command{
	Use:   "verify ID",
	Short: "Verify the Hub can impersonate a service account",
	Long: `Verify that the Hub can generate tokens for a registered service account.

This calls the IAM Credentials API to confirm the Hub's identity has
roles/iam.serviceAccountTokenCreator on the target SA.

Examples:
  scion project service-accounts verify <id>`,
	Args: cobra.ExactArgs(1),
	RunE: runSAVerify,
}

var saMintCmd = &cobra.Command{
	Use:   "mint",
	Short: "Mint a new GCP service account in the Hub's project",
	Long: `Create a new GCP service account in the Hub's own GCP project.

The minted SA is permissionless by default — no IAM roles are granted.
The Hub automatically configures itself to impersonate the SA for token
generation. You can later grant IAM permissions on your own GCP projects.

Examples:
  scion project service-accounts mint
  scion project service-accounts mint --account-id my-pipeline
  scion project service-accounts mint --account-id my-pipeline --name "My Pipeline SA"`,
	Args: cobra.NoArgs,
	RunE: runSAMint,
}

var (
	saProjectID   string
	saDisplayName string
	saMintID      string
)

func init() {
	projectCmd.AddCommand(projectServiceAccountsCmd)
	projectServiceAccountsCmd.AddCommand(saAddCmd)
	projectServiceAccountsCmd.AddCommand(saListCmd)
	projectServiceAccountsCmd.AddCommand(saRemoveCmd)
	projectServiceAccountsCmd.AddCommand(saVerifyCmd)
	projectServiceAccountsCmd.AddCommand(saMintCmd)

	saAddCmd.Flags().StringVar(&saProjectID, "project", "", "GCP project ID (required)")
	saAddCmd.Flags().StringVar(&saDisplayName, "name", "", "Display name for the service account")
	_ = saAddCmd.MarkFlagRequired("project")

	saMintCmd.Flags().StringVar(&saMintID, "account-id", "", "Custom account ID (will be prefixed with scion-)")
	saMintCmd.Flags().StringVar(&saDisplayName, "name", "", "Display name for the service account")

	saListCmd.Flags().BoolVar(&saOutputJSON, "json", false, "Output in JSON format")
}

// resolveProjectForSA resolves the project ID and creates a hub client for SA operations.
func resolveProjectForSA() (hubclient.Client, string, error) {
	resolvedPath, _, err := config.ResolveProjectPath(projectPath)
	if err != nil {
		return nil, "", fmt.Errorf("failed to resolve project path: %w", err)
	}

	settings, err := config.LoadSettings(resolvedPath)
	if err != nil {
		return nil, "", fmt.Errorf("failed to load settings: %w", err)
	}

	client, err := getHubClient(settings)
	if err != nil {
		return nil, "", err
	}

	projectID := ""
	if settings.Hub != nil && settings.Hub.ProjectID != "" {
		projectID = settings.Hub.ProjectID
	}
	if projectID == "" {
		return nil, "", fmt.Errorf("project not linked to Hub. Use 'scion hub link' first")
	}

	return client, projectID, nil
}

func runSAAdd(cmd *cobra.Command, args []string) error {
	email := args[0]

	client, projectID, err := resolveProjectForSA()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req := &hubclient.CreateGCPServiceAccountRequest{
		Email:       email,
		ProjectID:   saProjectID,
		DisplayName: saDisplayName,
	}

	sa, err := client.GCPServiceAccounts(projectID).Create(ctx, req)
	if err != nil {
		return fmt.Errorf("failed to register service account: %w", err)
	}

	if isJSONOutput() {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(sa)
	}

	fmt.Printf("Registered service account: %s\n", sa.Email)
	fmt.Printf("  ID:       %s\n", sa.ID)
	fmt.Printf("  Project:  %s\n", sa.ProjectID)
	if sa.DisplayName != "" {
		fmt.Printf("  Name:     %s\n", sa.DisplayName)
	}
	fmt.Printf("  Verified: %v\n", sa.Verified)

	return nil
}

func runSAList(cmd *cobra.Command, args []string) error {
	client, projectID, err := resolveProjectForSA()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sas, err := client.GCPServiceAccounts(projectID).List(ctx)
	if err != nil {
		return fmt.Errorf("failed to list service accounts: %w", err)
	}

	if saOutputJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(sas)
	}

	if len(sas) == 0 {
		fmt.Println("No GCP service accounts registered for this project.")
		fmt.Println("Use 'scion project service-accounts add' to register one.")
		return nil
	}

	fmt.Printf("GCP Service Accounts (%d):\n", len(sas))
	fmt.Printf("%-36s  %-45s  %-20s  %s\n", "ID", "EMAIL", "PROJECT", "VERIFIED")
	fmt.Printf("%-36s  %-45s  %-20s  %s\n",
		"------------------------------------",
		"---------------------------------------------",
		"--------------------",
		"--------")
	for _, sa := range sas {
		verified := "no"
		if sa.Verified {
			verified = "yes"
		}
		fmt.Printf("%-36s  %-45s  %-20s  %s\n",
			sa.ID,
			truncate(sa.Email, 45),
			truncate(sa.ProjectID, 20),
			verified)
	}

	return nil
}

func runSARemove(cmd *cobra.Command, args []string) error {
	saID := args[0]

	client, projectID, err := resolveProjectForSA()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := client.GCPServiceAccounts(projectID).Delete(ctx, saID); err != nil {
		return fmt.Errorf("failed to remove service account: %w", err)
	}

	fmt.Printf("Removed service account %s\n", saID)
	return nil
}

func runSAMint(cmd *cobra.Command, args []string) error {
	client, projectID, err := resolveProjectForSA()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	req := &hubclient.MintGCPServiceAccountRequest{
		AccountID:   saMintID,
		DisplayName: saDisplayName,
	}

	sa, err := client.GCPServiceAccounts(projectID).Mint(ctx, req)
	if err != nil {
		return fmt.Errorf("failed to mint service account: %w", err)
	}

	if isJSONOutput() {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(sa)
	}

	fmt.Printf("Minted service account: %s\n", sa.Email)
	fmt.Printf("  ID:        %s\n", sa.ID)
	fmt.Printf("  Project:   %s\n", sa.ProjectID)
	if sa.DisplayName != "" {
		fmt.Printf("  Name:      %s\n", sa.DisplayName)
	}
	fmt.Printf("  Verified:  %v\n", sa.Verified)
	fmt.Printf("  Managed:   %v\n", sa.Managed)

	return nil
}

func runSAVerify(cmd *cobra.Command, args []string) error {
	saID := args[0]

	client, projectID, err := resolveProjectForSA()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sa, err := client.GCPServiceAccounts(projectID).Verify(ctx, saID)
	if err != nil {
		return fmt.Errorf("verification failed: %w", err)
	}

	if isJSONOutput() {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(sa)
	}

	fmt.Printf("Service account verified: %s\n", sa.Email)
	fmt.Printf("  ID:          %s\n", sa.ID)
	fmt.Printf("  Project:     %s\n", sa.ProjectID)
	fmt.Printf("  Verified:    %v\n", sa.Verified)
	fmt.Printf("  Verified At: %s\n", sa.VerifiedAt.Format(time.RFC3339))

	return nil
}
