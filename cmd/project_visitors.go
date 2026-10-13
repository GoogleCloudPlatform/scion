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
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// Project visitors (ptone/scion#4603): grant users of a trusted issuer
// access to a project's agents by email. Human mode only: the commands
// are not in agentAllowed (cli_mode.go).

var (
	projectVisitorsProject string
	projectVisitorsIssuer  string
	projectVisitorsEmail   string
	projectVisitorsExpires string
	projectVisitorsJSON    bool
)

// projectVisitorsCmd is the parent command for project visitor grants.
var projectVisitorsCmd = &cobra.Command{
	Use:   "visitors",
	Short: "Manage project visitors from trusted issuers",
	Long: `Grant, list and revoke visitor access to a project for users of an
external issuer this Hub trusts.

A visitor may message the project's agents; a visitor is not a project
member. The Hub admin must trust the issuer with allow_visitor_grants on,
and the email must pass the issuer's allowed_emails and allowed_domains.
A visitor stays pending until their first request from that issuer with
the granted email.

Requires project owner or admin or hub admin, an interactive session (not a
user access credential), and the federation.project_visitors experiment on
the Hub. Granting and extending a window also need every permission of the
project-visitor role on the project.`,
}

var projectVisitorsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List the project's visitors",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		visitors, ctx, cancel, err := projectVisitorsService(cmd)
		if err != nil {
			return err
		}
		defer cancel()
		list, err := visitors.List(ctx)
		if err != nil {
			return fmt.Errorf("failed to list project visitors: %w", err)
		}
		if projectVisitorsWantJSON() {
			return writeProjectVisitorsJSON(list)
		}
		if len(list) == 0 {
			fmt.Println("No visitors.")
			return nil
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "EMAIL\tISSUER\tSTATUS\tEXPIRES\tID")
		for _, v := range list {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", v.Email, v.Issuer, formatVisitorStatus(v), formatVisitorExpiry(v.ExpiresAt), v.ID)
		}
		return tw.Flush()
	},
}

var projectVisitorsAddCmd = &cobra.Command{
	Use:   "add --issuer <issuer URL> --email <email> [--expires <duration>]",
	Short: "Grant a user of a trusted issuer visitor access",
	Long: `Grant visitor access to the user of a trusted issuer with the given email.

--issuer is the issuer URL as configured in the Hub's trusted issuers.
--expires is optional: ` + expiryAcceptedForms + `.

Example:
  scion project visitors add --issuer https://hub-a.example.com --email alice@corp.example --expires 30d`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		req := &hubclient.AddProjectVisitorRequest{Issuer: projectVisitorsIssuer, Email: projectVisitorsEmail}
		if projectVisitorsExpires != "" {
			exp, err := parseExpiry(projectVisitorsExpires)
			if err != nil {
				return newUsageError("--expires: %v", err)
			}
			req.ExpiresAt = &exp
		}
		visitors, ctx, cancel, err := projectVisitorsService(cmd)
		if err != nil {
			return err
		}
		defer cancel()
		v, err := visitors.Add(ctx, req)
		if err != nil {
			return fmt.Errorf("failed to add project visitor: %w", err)
		}
		if projectVisitorsWantJSON() {
			return writeProjectVisitorsJSON(v)
		}
		fmt.Printf("Added visitor %s (%s), status %s, expires %s.\n", v.Email, v.Issuer, v.Status, formatVisitorExpiry(v.ExpiresAt))
		return nil
	},
}

var projectVisitorsRemoveCmd = &cobra.Command{
	Use:   "remove <email or id>",
	Short: "Revoke a visitor's access",
	Long: `Revoke a visitor's access to the project. The visitor's next request is refused.

Name the visitor by email or by the ID "list" shows. When the same email is
a visitor from more than one issuer, add --issuer.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		visitors, ctx, cancel, err := projectVisitorsService(cmd)
		if err != nil {
			return err
		}
		defer cancel()
		v, err := resolveProjectVisitor(ctx, visitors, args[0], projectVisitorsIssuer)
		if err != nil {
			return err
		}
		if err := visitors.Remove(ctx, v.ID); err != nil {
			return fmt.Errorf("failed to remove project visitor: %w", err)
		}
		fmt.Printf("Removed visitor %s (%s).\n", v.Email, v.Issuer)
		return nil
	},
}

var projectVisitorsExtendCmd = &cobra.Command{
	Use:   "extend <email or id> --expires <duration>",
	Short: "Change when a visitor's access expires",
	Long: `Set a new expiry on a visitor's access, counted from now.

--expires is required: ` + expiryAcceptedForms + `.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if projectVisitorsExpires == "" {
			return newUsageError("--expires is required")
		}
		exp, err := parseExpiry(projectVisitorsExpires)
		if err != nil {
			return newUsageError("--expires: %v", err)
		}
		visitors, ctx, cancel, err := projectVisitorsService(cmd)
		if err != nil {
			return err
		}
		defer cancel()
		v, err := resolveProjectVisitor(ctx, visitors, args[0], projectVisitorsIssuer)
		if err != nil {
			return err
		}
		updated, err := visitors.Update(ctx, v.ID, &hubclient.UpdateProjectVisitorRequest{NotBefore: v.NotBefore, ExpiresAt: &exp})
		if err != nil {
			return fmt.Errorf("failed to extend project visitor: %w", err)
		}
		if projectVisitorsWantJSON() {
			return writeProjectVisitorsJSON(updated)
		}
		fmt.Printf("Visitor %s (%s) now expires %s.\n", updated.Email, updated.Issuer, formatVisitorExpiry(updated.ExpiresAt))
		return nil
	},
}

// projectVisitorsService returns the visitor service of the selected
// project and a request context with its cancel function.
func projectVisitorsService(cmd *cobra.Command) (hubclient.ProjectVisitorService, context.Context, context.CancelFunc, error) {
	settings, client, err := loadHubClient()
	if err != nil {
		return nil, nil, nil, err
	}
	projectID, err := resolveProjectID(settings, projectVisitorsProject)
	if err != nil {
		return nil, nil, nil, err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	return client.ProjectVisitors(projectID), ctx, cancel, nil
}

// resolveProjectVisitor finds the visitor named by ref: its ID, or its
// email (case-insensitive), narrowed to issuer when issuer is set.
func resolveProjectVisitor(ctx context.Context, visitors hubclient.ProjectVisitorService, ref, issuer string) (*hubclient.ProjectVisitor, error) {
	list, err := visitors.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list project visitors: %w", err)
	}
	return matchProjectVisitor(list, ref, issuer)
}

// matchProjectVisitor is resolveProjectVisitor's match over a listed set.
func matchProjectVisitor(list []hubclient.ProjectVisitor, ref, issuer string) (*hubclient.ProjectVisitor, error) {
	ref = strings.TrimSpace(ref)
	issuer = strings.TrimRight(strings.TrimSpace(issuer), "/")
	var matches []hubclient.ProjectVisitor
	for _, v := range list {
		if v.ID == ref {
			return &v, nil
		}
		if strings.EqualFold(v.Email, ref) && (issuer == "" || v.Issuer == issuer) {
			matches = append(matches, v)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("no visitor %q in this project", ref)
	case 1:
		return &matches[0], nil
	default:
		return nil, newUsageError("%q is a visitor from more than one issuer; add --issuer", ref)
	}
}

// formatVisitorStatus is the STATUS column of "list": the account status,
// marked when the Hub does not accept visitors from the issuer now (the
// grant is kept, but the visitor's requests are refused).
func formatVisitorStatus(v hubclient.ProjectVisitor) string {
	if !v.IssuerAccepted {
		return v.Status + " (issuer not accepted)"
	}
	return v.Status
}

func formatVisitorExpiry(t *time.Time) string {
	if t == nil {
		return "never"
	}
	return t.UTC().Format(time.RFC3339)
}

// projectVisitorsWantJSON reports whether the command prints JSON: --json
// on the command, or the global --format json.
func projectVisitorsWantJSON() bool {
	return projectVisitorsJSON || isJSONOutput()
}

func writeProjectVisitorsJSON(v interface{}) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func init() {
	projectCmd.AddCommand(projectVisitorsCmd)
	projectVisitorsCmd.AddCommand(projectVisitorsListCmd)
	projectVisitorsCmd.AddCommand(projectVisitorsAddCmd)
	projectVisitorsCmd.AddCommand(projectVisitorsRemoveCmd)
	projectVisitorsCmd.AddCommand(projectVisitorsExtendCmd)

	projectVisitorsCmd.PersistentFlags().StringVar(&projectVisitorsProject, "project", "", "Project ID or slug")

	projectVisitorsAddCmd.Flags().StringVar(&projectVisitorsIssuer, "issuer", "", "Issuer URL of a trusted issuer that allows visitor grants")
	projectVisitorsAddCmd.Flags().StringVar(&projectVisitorsEmail, "email", "", "The visitor's email at that issuer")
	projectVisitorsAddCmd.Flags().StringVar(&projectVisitorsExpires, "expires", "", "When the access expires: "+expiryAcceptedForms)
	_ = projectVisitorsAddCmd.MarkFlagRequired("issuer")
	_ = projectVisitorsAddCmd.MarkFlagRequired("email")

	projectVisitorsRemoveCmd.Flags().StringVar(&projectVisitorsIssuer, "issuer", "", "Issuer URL, when the email is a visitor from more than one issuer")
	projectVisitorsExtendCmd.Flags().StringVar(&projectVisitorsIssuer, "issuer", "", "Issuer URL, when the email is a visitor from more than one issuer")
	projectVisitorsExtendCmd.Flags().StringVar(&projectVisitorsExpires, "expires", "", "New expiry: "+expiryAcceptedForms)

	for _, c := range []*cobra.Command{projectVisitorsListCmd, projectVisitorsAddCmd, projectVisitorsExtendCmd} {
		c.Flags().BoolVar(&projectVisitorsJSON, "json", false, "Output in JSON format")
	}
}
