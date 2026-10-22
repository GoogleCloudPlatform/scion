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
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// Project visitors (ptone/scion#4603): grant users of a trusted issuer
// access to a project's agents by email, or by email domain through a
// visitor rule. Human mode only: the commands are not in agentAllowed
// (cli_mode.go).

var (
	projectVisitorsProject string
	projectVisitorsIssuer  string
	projectVisitorsEmail   string
	projectVisitorsDomain  string
	projectVisitorsExpires string
	projectVisitorsJSON    bool
)

// projectVisitorsCmd is the parent command for project visitor grants.
var projectVisitorsCmd = &cobra.Command{
	Use:   "visitors",
	Short: "Manage project visitors from trusted issuers",
	Long: `Grant, list and revoke visitor access to a project for users of an
external issuer this Hub trusts, by email or by email domain.

A visitor may message the project's agents; a visitor is not a project
member. The Hub admin must trust the issuer with allow_visitor_grants on,
and the email (or every email in the domain) must pass the issuer's
allowed_emails and allowed_domains. A visitor granted by email stays
pending until their first request from that issuer with the granted email.

A domain grant is a visitor rule: each user of the issuer whose verified
email is in exactly that domain (no subdomains) becomes a visitor on their
first request. Removing the rule removes those visitors; removing one of
them keeps the rule from adding that person again.

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
		rules, err := visitors.ListRules(ctx)
		if err != nil {
			return fmt.Errorf("failed to list project visitor rules: %w", err)
		}
		if projectVisitorsWantJSON() {
			return writeProjectVisitorsJSON(projectVisitorsListing{Visitors: list, Rules: rules})
		}
		return printProjectVisitors(os.Stdout, list, rules)
	},
}

// projectVisitorsListing is the JSON output of "list": the visitors, each
// with its source, and the visitor rules.
type projectVisitorsListing struct {
	Visitors []hubclient.ProjectVisitor     `json:"visitors"`
	Rules    []hubclient.ProjectVisitorRule `json:"rules"`
}

// printProjectVisitors writes the tables of "list": the visitors, with the
// source of each (email, or the domain of the rule that added it), then
// the visitor rules with how many visitors each has added.
func printProjectVisitors(out io.Writer, list []hubclient.ProjectVisitor, rules []hubclient.ProjectVisitorRule) error {
	ruleByID := make(map[string]hubclient.ProjectVisitorRule, len(rules))
	for _, r := range rules {
		ruleByID[r.ID] = r
	}
	added := make(map[string]int, len(rules))
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if len(list) == 0 {
		_, _ = fmt.Fprintln(tw, "No visitors.")
	} else {
		_, _ = fmt.Fprintln(tw, "EMAIL\tISSUER\tSTATUS\tSOURCE\tEXPIRES\tID")
		for _, v := range list {
			source := "email"
			if v.SourceRuleID != "" {
				added[v.SourceRuleID]++
				source = "rule " + v.SourceRuleID
				if r, ok := ruleByID[v.SourceRuleID]; ok {
					source = "domain " + r.Domain
				}
			}
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", v.Email, v.Issuer, formatVisitorStatus(v), source, formatVisitorExpiry(v.ExpiresAt), v.ID)
		}
	}
	if len(rules) > 0 {
		_, _ = fmt.Fprintln(tw)
		_, _ = fmt.Fprintln(tw, "DOMAIN\tISSUER\tVISITORS\tEXPIRES\tRULE ID")
		for _, r := range rules {
			domain := r.Domain
			if !r.IssuerAccepted {
				domain += " (issuer not accepted)"
			}
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\n", domain, r.Issuer, added[r.ID], formatVisitorExpiry(r.ExpiresAt), r.ID)
		}
	}
	return tw.Flush()
}

var projectVisitorsAddCmd = &cobra.Command{
	Use:   "add --issuer <issuer URL> (--email <email> | --domain <domain>) [--expires <duration>]",
	Short: "Grant users of a trusted issuer visitor access",
	Long: `Grant visitor access to the user of a trusted issuer with the given email,
or, with --domain, to every user of that issuer whose verified email is in
exactly that domain (a visitor rule; subdomains do not match).

--issuer is the issuer URL as configured in the Hub's trusted issuers.
--expires is optional: ` + expiryAcceptedForms + `.

Examples:
  scion project visitors add --issuer https://hub-a.example.com --email alice@corp.example --expires 30d
  scion project visitors add --issuer https://hub-a.example.com --domain corp.example`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		var expiresAt *time.Time
		if projectVisitorsExpires != "" {
			exp, err := parseExpiry(projectVisitorsExpires)
			if err != nil {
				return newUsageError("--expires: %v", err)
			}
			expiresAt = &exp
		}
		visitors, ctx, cancel, err := projectVisitorsService(cmd)
		if err != nil {
			return err
		}
		defer cancel()
		if projectVisitorsDomain != "" {
			rule, err := visitors.AddRule(ctx, &hubclient.AddProjectVisitorRuleRequest{
				Issuer: projectVisitorsIssuer, Domain: projectVisitorsDomain, ExpiresAt: expiresAt,
			})
			if err != nil {
				return fmt.Errorf("failed to add project visitor rule: %w", err)
			}
			if projectVisitorsWantJSON() {
				return writeProjectVisitorsJSON(rule)
			}
			fmt.Printf("Added visitor rule for domain %s (%s), expires %s. Matching users become visitors on their first request.\n",
				rule.Domain, rule.Issuer, formatVisitorExpiry(rule.ExpiresAt))
			return nil
		}
		req := &hubclient.AddProjectVisitorRequest{Issuer: projectVisitorsIssuer, Email: projectVisitorsEmail, ExpiresAt: expiresAt}
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
	Use:   "remove (<email or id> | --domain <domain>)",
	Short: "Revoke a visitor's access, or remove a visitor rule",
	Long: `Revoke a visitor's access to the project. The visitor's next request is refused.

Name the visitor by email or by the ID "list" shows. When the same email is
a visitor from more than one issuer, add --issuer. Revoking a visitor that
a visitor rule added keeps the rule from adding that person again.

To remove a visitor rule, name it with --domain (and --issuer when the
domain has rules for more than one issuer), or pass the rule ID "list"
shows. Removing a rule revokes every visitor it added; visitors granted by
email keep their access.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if (len(args) == 1) == (projectVisitorsDomain != "") {
			return newUsageError("name either a visitor (email or id) or a visitor rule (--domain)")
		}
		visitors, ctx, cancel, err := projectVisitorsService(cmd)
		if err != nil {
			return err
		}
		defer cancel()
		if projectVisitorsDomain != "" {
			return removeProjectVisitorRule(ctx, visitors, projectVisitorsDomain, projectVisitorsIssuer)
		}
		list, err := visitors.List(ctx)
		if err != nil {
			return fmt.Errorf("failed to list project visitors: %w", err)
		}
		if !visitorListHasID(list, args[0]) && !strings.Contains(args[0], "@") {
			// Not a visitor ID or an email: a rule ID.
			rules, err := visitors.ListRules(ctx)
			if err != nil {
				return fmt.Errorf("failed to list project visitor rules: %w", err)
			}
			for _, r := range rules {
				if r.ID == strings.TrimSpace(args[0]) {
					return removeProjectVisitorRuleByID(ctx, visitors, r)
				}
			}
		}
		v, err := matchProjectVisitor(list, args[0], projectVisitorsIssuer)
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

// visitorListHasID reports whether list has a visitor whose ID is id.
func visitorListHasID(list []hubclient.ProjectVisitor, id string) bool {
	id = strings.TrimSpace(id)
	for _, v := range list {
		if v.ID == id {
			return true
		}
	}
	return false
}

// removeProjectVisitorRule removes the visitor rule for domain, narrowed to
// issuer when issuer is set.
func removeProjectVisitorRule(ctx context.Context, visitors hubclient.ProjectVisitorService, domain, issuer string) error {
	rules, err := visitors.ListRules(ctx)
	if err != nil {
		return fmt.Errorf("failed to list project visitor rules: %w", err)
	}
	rule, err := matchProjectVisitorRule(rules, domain, issuer)
	if err != nil {
		return err
	}
	return removeProjectVisitorRuleByID(ctx, visitors, *rule)
}

func removeProjectVisitorRuleByID(ctx context.Context, visitors hubclient.ProjectVisitorService, rule hubclient.ProjectVisitorRule) error {
	if err := visitors.RemoveRule(ctx, rule.ID); err != nil {
		return fmt.Errorf("failed to remove project visitor rule: %w", err)
	}
	fmt.Printf("Removed visitor rule for domain %s (%s) and the visitors it added.\n", rule.Domain, rule.Issuer)
	return nil
}

// matchProjectVisitorRule finds the rule for domain (case-insensitive),
// narrowed to issuer when issuer is set.
func matchProjectVisitorRule(rules []hubclient.ProjectVisitorRule, domain, issuer string) (*hubclient.ProjectVisitorRule, error) {
	domain = strings.TrimSpace(domain)
	issuer = strings.TrimRight(strings.TrimSpace(issuer), "/")
	var matches []hubclient.ProjectVisitorRule
	for _, r := range rules {
		if strings.EqualFold(r.Domain, domain) && (issuer == "" || r.Issuer == issuer) {
			matches = append(matches, r)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("no visitor rule for domain %q in this project", domain)
	case 1:
		return &matches[0], nil
	default:
		return nil, newUsageError("domain %q has rules for more than one issuer; add --issuer", domain)
	}
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
	projectVisitorsAddCmd.Flags().StringVar(&projectVisitorsDomain, "domain", "", "Grant every user of that issuer whose verified email is in exactly this domain (a visitor rule)")
	projectVisitorsAddCmd.Flags().StringVar(&projectVisitorsExpires, "expires", "", "When the access expires: "+expiryAcceptedForms)
	_ = projectVisitorsAddCmd.MarkFlagRequired("issuer")
	projectVisitorsAddCmd.MarkFlagsOneRequired("email", "domain")
	projectVisitorsAddCmd.MarkFlagsMutuallyExclusive("email", "domain")

	projectVisitorsRemoveCmd.Flags().StringVar(&projectVisitorsIssuer, "issuer", "", "Issuer URL, when the email is a visitor (or the domain has rules) from more than one issuer")
	projectVisitorsRemoveCmd.Flags().StringVar(&projectVisitorsDomain, "domain", "", "Remove the visitor rule for this domain")
	projectVisitorsExtendCmd.Flags().StringVar(&projectVisitorsIssuer, "issuer", "", "Issuer URL, when the email is a visitor from more than one issuer")
	projectVisitorsExtendCmd.Flags().StringVar(&projectVisitorsExpires, "expires", "", "New expiry: "+expiryAcceptedForms)

	for _, c := range []*cobra.Command{projectVisitorsListCmd, projectVisitorsAddCmd, projectVisitorsExtendCmd} {
		c.Flags().BoolVar(&projectVisitorsJSON, "json", false, "Output in JSON format")
	}
}
