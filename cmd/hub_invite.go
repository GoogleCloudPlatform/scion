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

var (
	inviteOutputJSON bool
	inviteExpires    string
	inviteMaxUses    int
	inviteNote       string
)

var hubInviteCmd = &cobra.Command{
	Use:   "invite",
	Short: "Manage invite codes",
	Long: `Manage invite codes for the hub.

Invite codes allow admins to generate time-limited links that new users
can use to join the hub when invite-only mode is enabled.

Examples:
  scion hub invite create --expires 1h
  scion hub invite list
  scion hub invite revoke <id>
  scion hub invite delete <id>`,
}

var hubInviteCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a new invite code",
	Long: `Create a new invite code with a specified expiration.

Expiration presets: 5m, 15m, 30m, 1h, 4h, 12h, 24h, 72h (3 days), 120h (5 days)

Examples:
  scion hub invite create --expires 1h
  scion hub invite create --expires 24h --max-uses 5 --note "Workshop"
  scion hub invite create --expires 120h --max-uses 0 --note "Open invite"`,
	RunE: runInviteCreate,
}

var hubInviteListCmd = &cobra.Command{
	Use:   "list",
	Short: "List invite codes",
	Long: `List all invite codes.

Examples:
  scion hub invite list
  scion hub invite list --json`,
	Args: cobra.NoArgs,
	RunE: runInviteList,
}

var hubInviteRevokeCmd = &cobra.Command{
	Use:   "revoke ID",
	Short: "Revoke an invite code",
	Long: `Revoke an active invite code so it can no longer be used.

Examples:
  scion hub invite revoke a1b2c3d4-e5f6-7890-abcd-ef1234567890`,
	Args: cobra.ExactArgs(1),
	RunE: runInviteRevoke,
}

var hubInviteDeleteCmd = &cobra.Command{
	Use:   "delete ID",
	Short: "Delete an invite code",
	Long: `Permanently delete an invite code.

Examples:
  scion hub invite delete a1b2c3d4-e5f6-7890-abcd-ef1234567890`,
	Args: cobra.ExactArgs(1),
	RunE: runInviteDelete,
}

func init() {
	hubCmd.AddCommand(hubInviteCmd)
	hubInviteCmd.AddCommand(hubInviteCreateCmd)
	hubInviteCmd.AddCommand(hubInviteListCmd)
	hubInviteCmd.AddCommand(hubInviteRevokeCmd)
	hubInviteCmd.AddCommand(hubInviteDeleteCmd)

	hubInviteCreateCmd.Flags().StringVar(&inviteExpires, "expires", "", "Expiration duration (e.g., 1h, 24h, 120h)")
	hubInviteCreateCmd.Flags().IntVar(&inviteMaxUses, "max-uses", 1, "Maximum uses (0 = unlimited)")
	hubInviteCreateCmd.Flags().StringVar(&inviteNote, "note", "", "Optional note")
	_ = hubInviteCreateCmd.MarkFlagRequired("expires")

	hubInviteCmd.Flags().BoolVar(&inviteOutputJSON, "json", false, "Output in JSON format")
	hubInviteListCmd.Flags().BoolVar(&inviteOutputJSON, "json", false, "Output in JSON format")
	hubInviteCreateCmd.Flags().BoolVar(&inviteOutputJSON, "json", false, "Output in JSON format")
}

func getInviteClient() (hubclient.Client, error) {
	resolvedPath, _, err := config.ResolveProjectPath(projectPath)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve project path: %w", err)
	}

	settings, err := config.LoadSettings(resolvedPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load settings: %w", err)
	}

	return getHubClient(settings)
}

func runInviteCreate(cmd *cobra.Command, args []string) error {
	if inviteExpires == "" {
		return fmt.Errorf("--expires is required")
	}

	// Validate the duration
	if _, err := time.ParseDuration(inviteExpires); err != nil {
		return fmt.Errorf("invalid --expires duration: %w", err)
	}

	client, err := getInviteClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := client.Invites().Create(ctx, &hubclient.InviteCreateRequest{
		ExpiresIn: inviteExpires,
		MaxUses:   inviteMaxUses,
		Note:      inviteNote,
	})
	if err != nil {
		return fmt.Errorf("failed to create invite: %w", err)
	}

	if inviteOutputJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(resp)
	}

	maxUsesLabel := fmt.Sprintf("%d", resp.Invite.MaxUses)
	if resp.Invite.MaxUses == 0 {
		maxUsesLabel = "unlimited"
	} else if resp.Invite.MaxUses == 1 {
		maxUsesLabel = "1 (single-use)"
	}

	fmt.Println("Invite created successfully.")
	fmt.Println()
	fmt.Printf("  Code:     %s\n", resp.Code)
	fmt.Printf("  Link:     %s\n", resp.InviteURL)
	fmt.Printf("  Expires:  %s\n", resp.Invite.ExpiresAt.Format(time.RFC3339))
	fmt.Printf("  Max uses: %s\n", maxUsesLabel)
	if resp.Invite.Note != "" {
		fmt.Printf("  Note:     %s\n", resp.Invite.Note)
	}
	fmt.Println()
	fmt.Println("Share this link with the person you want to invite.")
	fmt.Println("The code will not be shown again.")

	return nil
}

func runInviteList(cmd *cobra.Command, args []string) error {
	client, err := getInviteClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := client.Invites().List(ctx, "")
	if err != nil {
		return fmt.Errorf("failed to list invites: %w", err)
	}

	if inviteOutputJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(resp)
	}

	if len(resp.Items) == 0 {
		fmt.Println("No invite codes found.")
		return nil
	}

	fmt.Printf("%-38s %-14s %-10s %-8s %-22s %s\n", "ID", "PREFIX", "STATUS", "USES", "EXPIRES", "NOTE")
	for _, inv := range resp.Items {
		status := inviteStatus(inv)
		uses := fmt.Sprintf("%d", inv.UseCount)
		if inv.MaxUses > 0 {
			uses = fmt.Sprintf("%d/%d", inv.UseCount, inv.MaxUses)
		} else {
			uses = fmt.Sprintf("%d/∞", inv.UseCount)
		}
		fmt.Printf("%-38s %-14s %-10s %-8s %-22s %s\n",
			inv.ID,
			inv.CodePrefix,
			status,
			uses,
			inv.ExpiresAt.Format("2006-01-02 15:04 MST"),
			inv.Note,
		)
	}
	fmt.Printf("\nTotal: %d invite(s)\n", resp.TotalCount)

	return nil
}

func inviteStatus(inv hubclient.InviteCode) string {
	if inv.Revoked {
		return "revoked"
	}
	if time.Now().After(inv.ExpiresAt) {
		return "expired"
	}
	if inv.MaxUses > 0 && inv.UseCount >= inv.MaxUses {
		return "exhausted"
	}
	return "active"
}

func runInviteRevoke(cmd *cobra.Command, args []string) error {
	id := args[0]

	client, err := getInviteClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := client.Invites().Revoke(ctx, id); err != nil {
		return fmt.Errorf("failed to revoke invite: %w", err)
	}

	fmt.Printf("Invite %s has been revoked.\n", id)
	return nil
}

func runInviteDelete(cmd *cobra.Command, args []string) error {
	id := args[0]

	client, err := getInviteClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := client.Invites().Delete(ctx, id); err != nil {
		return fmt.Errorf("failed to delete invite: %w", err)
	}

	fmt.Printf("Invite %s has been deleted.\n", id)
	return nil
}
