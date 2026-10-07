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
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

// hubUsersProvisionNameF is the display-name flag of hub users provision.
const hubUsersProvisionNameF = "display-name"

var (
	hubUsersOutputJSON    bool
	hubUsersProvisionName string
	hubUsersProvisionNote string
)

var hubUsersCmd = &cobra.Command{
	Use:   "users",
	Short: "Administer hub users",
	Long: `Administer hub user records.

Examples:
  scion hub users provision alice@example.com --display-name "Alice Smith"`,
}

var hubUsersProvisionCmd = &cobra.Command{
	Use:   "provision EMAIL",
	Short: "Pre-register a user",
	Long: `Pre-register a user on the hub.

This creates the same pending (invited) record as an admin invite, with an
optional display name. The person still signs in through a configured
sign-in provider; the record holds no credential. The role is assigned at
first sign-in from the hub's configured policy.

Running the command again with the same details is safe: it reports that
the user is already pre-registered.

Requires the user.invite permission (hub admins hold it).

Examples:
  scion hub users provision alice@example.com
  scion hub users provision bob@example.com --display-name "Bob" --note "Contractor, Q3"
  scion hub users provision carol@example.com --json`,
	Args: cobra.ExactArgs(1),
	RunE: runHubUsersProvision,
}

func init() {
	hubCmd.AddCommand(hubUsersCmd)
	hubUsersCmd.AddCommand(hubUsersProvisionCmd)

	hubUsersProvisionCmd.Flags().StringVar(&hubUsersProvisionName, hubUsersProvisionNameF, "", "Display name to store for the user")
	hubUsersProvisionCmd.Flags().StringVar(&hubUsersProvisionNote, "note", "", "Optional admin note")
	hubUsersCmd.PersistentFlags().BoolVar(&hubUsersOutputJSON, "json", false, "Output in JSON format")
}

func runHubUsersProvision(cmd *cobra.Command, args []string) error {
	_, client, err := loadHubClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

	req := &hubclient.ProvisionUserRequest{Email: args[0]}
	if cmd.Flags().Changed(hubUsersProvisionNameF) {
		name := hubUsersProvisionName
		req.DisplayName = &name
	}
	if cmd.Flags().Changed("note") {
		note := hubUsersProvisionNote
		req.Note = &note
	}
	return provisionHubUser(ctx, cmd.OutOrStdout(), client.Users(), req, hubUsersOutputJSON)
}

// provisionHubUser sends one provisioning request and prints the result.
func provisionHubUser(ctx context.Context, out io.Writer, users hubclient.UserService, req *hubclient.ProvisionUserRequest, asJSON bool) error {
	resp, err := users.Provision(ctx, req)
	if err != nil {
		var pe *hubclient.ProvisionError
		if errors.As(err, &pe) {
			return fmt.Errorf("failed to provision %s: %s", req.Email, provisionErrorText(pe))
		}
		return fmt.Errorf("failed to provision %s: %w", req.Email, err)
	}

	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(resp)
	}

	if resp.Created {
		_, _ = fmt.Fprintf(out, "Provisioned %s (status %s).\n", resp.User.Email, resp.User.Status)
	} else {
		_, _ = fmt.Fprintf(out, "%s is already pre-registered with these details (status %s).\n", resp.User.Email, resp.User.Status)
	}
	if resp.User.ID != "" {
		_, _ = fmt.Fprintf(out, "User ID: %s\n", resp.User.ID)
	}
	for _, w := range resp.Warnings {
		_, _ = fmt.Fprintf(out, "Warning: %s\n", provisionWarningText(w))
	}
	return nil
}

// provisionErrorText describes a 409 or 422 provisioning response.
func provisionErrorText(pe *hubclient.ProvisionError) string {
	switch pe.Reason {
	case hubclient.ProvisionReasonPendingUserExists:
		if pe.UserID != "" {
			return fmt.Sprintf("a pending record for this email exists with different details (user ID %s)", pe.UserID)
		}
		return "a pending record for this email exists with different details"
	case hubclient.ProvisionReasonSuspendedUserExists:
		return "this email belongs to a suspended user"
	case hubclient.ProvisionReasonUserExists:
		return "user already exists"
	default:
		return pe.Message
	}
}

// provisionWarningText describes an advisory provisioning warning.
func provisionWarningText(w string) string {
	switch w {
	case "reserved_identity":
		return "this email is a reserved platform identity; sign-in will be refused"
	case "domain_not_authorized":
		return "this email is outside the hub's authorized domains; sign-in will be refused"
	case "sign_in_currently_blocked_by_access_mode":
		return "the hub's access mode currently blocks all sign-ins (domain_restricted with no domains)"
	default:
		return w
	}
}
