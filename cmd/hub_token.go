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
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/spf13/cobra"
)

var (
	tokenOutputJSON bool
)

// hubTokenCmd is the parent command for user access token operations.
var hubTokenCmd = &cobra.Command{
	Use:   "token",
	Short: "Manage user access tokens",
	Long: `Manage user access tokens for CI/CD and automation.

User access tokens (UATs) are scoped, revocable bearer tokens for
non-interactive authentication. Each token carries a set of action
permissions and a boundary: a single project (the default), or the whole
Hub (--boundary hub), for hub-level scopes such as hub_health:read and for
one token that works across every project you can reach.

A token never grants more than your account holds. Every request that
uses it is checked against your current access: if your role, group or
project membership changes, or your account is suspended, the token's reach
changes with it on the next request.

Most CLI commands that work in a project first look the project up, which
needs the project:read scope, and project:list when the CLI finds the
project by its git remote, slug or name. Include both in tokens used with
the CLI. project:manage does not include them.
Scopes common CLI flows need:

  Any command run in a project   project:read, project:list
  scion list                     project:read, agent:list
  scion look, scion logs         project:read, agent:read
  scion start / create           project:read, agent:create, agent:read,
                                 and for any agent role above none:
                                 skill:read, skill:list, template:read,
                                 template:list, harness_config:read,
                                 harness_config:list (or pass --role none)
  scion message                  project:read, agent:message
  scion attach                   project:read, agent:attach
  scion stop, suspend, resume,   project:read, agent:lifecycle
    restore
  scion delete                   project:read, agent:delete
  scion service-accounts list,   project:read (a project's accounts)
    project service-accounts list
` + projectSettingsScopeLines() + `
` + hubSettingsScopeTable() + `
Hub tokens and project names: an ordinary user's hub token cannot select
project:list (only a hub-wide role that grants it can), so the CLI cannot
find a project by name, slug or git remote under it. Pass project IDs
(--project <id>), or run inside a linked checkout.

Token scopes limit what the CLI can do on the Hub. Local actions, such
as scion clean, act on the local machine with the user's file
permissions, and token scopes don't limit them.

A stored interactive login (from scion hub auth login) takes precedence
over SCION_HUB_TOKEN. To run the CLI under a scoped token, use an
environment with no stored login: a dedicated OS user, an isolated HOME,
or log out first.

Examples:
  # Create a token for CI that can create and monitor agents
  scion hub token create \
    --project my-project \
    --name "github-actions" \
    --scopes project:read,project:list,agent:create,agent:read,agent:list,skill:read,skill:list,template:read,template:list,harness_config:read,harness_config:list \
    --expires 90d

  # Create a hub-boundary token for "scion doctor" Platform Health
  # (needs a hub admin)
  scion hub token create --boundary hub \
    --name health-check \
    --scopes hub_health:read,broker:read,agent:list \
    --expires 30d

  # List your tokens
  scion hub token list

  # See which scopes you may currently select for a hub token
  scion hub token scopes --boundary hub

  # Revoke a token
  scion hub token revoke <token-id>

  # Delete a token permanently
  scion hub token delete <token-id>

  # Use the token in CI
  export SCION_HUB_TOKEN=scion_pat_...
  scion start ci-tests --project my-project --type default "Run tests"`,
}

// projectSettingsScopeLines renders the scion project settings rows of the
// "Scopes common CLI flows need" table from the scopes the commands'
// refusal hints name (on a project or hub boundary).
func projectSettingsScopeLines() string {
	var b strings.Builder
	for _, row := range []scopeRow{
		{[]string{"scion project settings get"}, []string{projSettingsReadScope}},
		{[]string{"scion project settings set,", "  unset"}, []string{projSettingsReadScope, projSettingsWriteScope}},
	} {
		b.WriteString(renderScopeRow(row))
	}
	return b.String()
}

// scopeRow is one row of a scopes table in the hub token help: the
// commands and the scopes their calls need.
type scopeRow struct {
	commands []string // one entry per printed line
	scopes   []string
}

// hubSettingsScopeRows lists the scopes of the hub settings commands, from
// the same constants their refusal hints name. A command that reads the
// current value before saving (set, unset) needs the read scope too; a
// section reset (unset --section NAME --reset) does not read first.
func hubSettingsScopeRows() []scopeRow {
	return []scopeRow{
		{[]string{"scion hub settings get,", "  schema"}, []string{srvcfgReadScope}},
		{[]string{"scion hub settings set, unset"}, []string{srvcfgReadScope, srvcfgWriteScope}},
		{[]string{"scion hub settings unset", "  --section NAME --reset"}, []string{srvcfgWriteScope}},
		{[]string{"scion hub project-defaults", "  get"}, []string{projDefaultsReadScope}},
		{[]string{"scion hub project-defaults", "  set, unset"}, []string{projDefaultsReadScope, projDefaultsWriteScope}},
		{[]string{"scion hub experiments list,", "  enable, disable, reset"}, []string{experimentsScope}},
	}
}

// hubSettingsScopeTable renders hubSettingsScopeRows in the layout of the
// "Scopes common CLI flows need" table: commands in a 31-column field, and
// scopes wrapped so that no line is wider than hubTokenHelpWidth.
func hubSettingsScopeTable() string {
	var b strings.Builder
	b.WriteString("Hub settings commands need a hub-boundary token (--boundary hub):\n\n")
	for _, row := range hubSettingsScopeRows() {
		b.WriteString(renderScopeRow(row))
	}
	return b.String()
}

// renderScopeRow renders one row: commands in a 31-column field, scopes
// wrapped so that no line is wider than hubTokenHelpWidth.
func renderScopeRow(row scopeRow) string {
	var b strings.Builder
	scopeLines := wrapScopes(row.scopes, hubTokenHelpWidth-2-31)
	n := len(row.commands)
	if len(scopeLines) > n {
		n = len(scopeLines)
	}
	for i := 0; i < n; i++ {
		label, scopes := "", ""
		if i < len(row.commands) {
			label = row.commands[i]
		}
		if i < len(scopeLines) {
			scopes = scopeLines[i]
		}
		b.WriteString(strings.TrimRight(fmt.Sprintf("  %-31s%s", label, scopes), " "))
		b.WriteString("\n")
	}
	return b.String()
}

// hubTokenHelpWidth is the widest line the hub settings table prints.
const hubTokenHelpWidth = 72

// wrapScopes joins scopes with ", " into lines of at most width characters
// (a single longer scope gets its own line); every line but the last ends
// with a comma.
func wrapScopes(scopes []string, width int) []string {
	var lines []string
	cur := ""
	for _, sc := range scopes {
		switch {
		case cur == "":
			cur = sc
		case len(cur)+2+len(sc)+1 <= width:
			cur += ", " + sc
		default:
			lines = append(lines, cur+",")
			cur = sc
		}
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return lines
}

var hubTokenCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a new access token",
	Long: fmt.Sprintf(`Create a new user access token scoped to a project, or to the whole Hub.

The token value is displayed only once on creation. Store it securely.

Boundary (--boundary):
  project  The default. The token works only in the project named by
           --project (required).
  hub      The token is not tied to a project (do not pass --project).
           Hub-level scopes such as hub_health:read, broker:read and
           broker:create are only selectable on a hub-boundary token, and
           project scopes apply in every project you can currently access.
           Each scope is checked against your current access when the
           token is created and on every request.

Scopes are restrictions, not grants: selecting a scope limits what the
token may ever do, but access to any specific target is still checked on
every request against your current authority there. For agent:attach,
that means your own agents and their descendants. For agent:port_access,
it also includes agents in projects where your role grants
agent.port_access (project owners and admins). Either is checked per
agent, not enumerated when you select the scope. Run
"scion hub token scopes --project <project>" (or "--boundary hub") to see
which scopes you may currently select and why.

Available scopes:
%s

Expiry (--expires) accepts %s.
%s.
Default: 90 days. Maximum: 1 year.

CLI use: most CLI commands look the project up first, which needs
project:read and project:list. See "scion hub token --help" for the
scopes common CLI flows need.

Examples:
  scion hub token create --project my-project --name ci-token --scopes project:read,project:list,agent:create,agent:read,skill:read,skill:list,template:read,template:list,harness_config:read,harness_config:list
  scion hub token create --project my-project --name deploy --scopes project:read,project:list,agent:manage --expires 30d
  scion hub token create --boundary hub --name health-check --scopes hub_health:read,broker:read,agent:list`, permissions.UATScopeHelp(), expiryAcceptedForms, expiryUnitNote),
	Args: cobra.NoArgs,
	RunE: runTokenCreate,
}

var hubTokenScopesCmd = &cobra.Command{
	Use:   "scopes",
	Short: "List token scopes and mint eligibility",
	Long: `List every scope accepted by "hub token create --scopes", and with
--project or --boundary hub, report which of them you may currently select
as a restriction on a token of that boundary.

Boundary (--boundary):
  project  Eligibility for a token scoped to --project (required). This is
           the default when --project is given.
  hub      Eligibility for a hub-boundary token (do not pass --project).
           A hub that cannot compute hub eligibility shows the scope
           catalog instead, with the boundaries each scope allows.

Eligibility answers only "may you select this restriction" -- it is not a
target list, and it is not a promise of access. Whether a resulting token
can actually reach a given agent, port, or other target is still checked on
every request, independently of what was eligible at mint time.

Examples:
  scion hub token scopes
  scion hub token scopes --project my-project
  scion hub token scopes --boundary hub
  scion hub token scopes --project my-project --json`,
	Args: cobra.NoArgs,
	RunE: runTokenScopes,
}

var hubTokenListCmd = &cobra.Command{
	Use:   "list",
	Short: "List your access tokens",
	Long: `List all user access tokens for the authenticated user.

The BOUNDARY column shows "hub" for a hub-boundary token and
"project:<project ID>" for a project token. --wide adds a PURPOSE column.

Filters:
  --boundary project|hub  Show only tokens of that boundary.
  --project <project>     Show only project tokens of that project. It never
                          matches hub tokens, even though a hub token can
                          reach the project, so it cannot be combined with
                          --boundary hub.

Examples:
  scion hub token list
  scion hub token list --wide
  scion hub token list --json
  scion hub token list --boundary hub
  scion hub token list --project my-project`,
	Args: cobra.NoArgs,
	RunE: runTokenList,
}

var hubTokenRevokeCmd = &cobra.Command{
	Use:   "revoke TOKEN-ID",
	Short: "Revoke an access token",
	Long: `Revoke a user access token. The token will no longer be accepted
for authentication but will still appear in listings as revoked.

Examples:
  scion hub token revoke abc123`,
	Args: cobra.ExactArgs(1),
	RunE: runTokenRevoke,
}

var hubTokenDeleteCmd = &cobra.Command{
	Use:   "delete TOKEN-ID",
	Short: "Delete an access token permanently",
	Long: `Permanently delete a user access token. This cannot be undone.

Examples:
  scion hub token delete abc123`,
	Args: cobra.ExactArgs(1),
	RunE: runTokenDelete,
}

var (
	tokenCreateName     string
	tokenCreateProject  string
	tokenCreateBoundary string
	tokenCreateScopes   []string
	tokenCreateExpires  string
	tokenCreatePurpose  string
	tokenCreateLabels   []string
	tokenListProject    string
	tokenListBoundary   string
	tokenListWide       bool
	tokenScopesProject  string
	tokenScopesBoundary string
)

func init() {
	hubCmd.AddCommand(hubTokenCmd)
	hubTokenCmd.AddCommand(hubTokenCreateCmd)
	hubTokenCmd.AddCommand(hubTokenListCmd)
	hubTokenCmd.AddCommand(hubTokenRevokeCmd)
	hubTokenCmd.AddCommand(hubTokenDeleteCmd)
	hubTokenCmd.AddCommand(hubTokenScopesCmd)

	hubTokenCreateCmd.Flags().StringVar(&tokenCreateName, "name", "", "Token name/label (required)")
	hubTokenCreateCmd.Flags().StringVar(&tokenCreateProject, "project", "", "Project name or ID to scope the token to (required with --boundary project)")
	hubTokenCreateCmd.Flags().StringVar(&tokenCreateBoundary, "boundary", tokenBoundaryProject, "Token boundary: project (default; needs --project) or hub (no --project)")

	hubTokenCreateCmd.Flags().StringArrayVar(&tokenCreateScopes, "scopes", nil, "Scope to grant (required, repeatable; also accepts a comma-separated list)")
	hubTokenCreateCmd.Flags().StringVar(&tokenCreateExpires, "expires", "", "Expiry: "+expiryAcceptedForms+" (default: 90d)")
	// --json was checked in runTokenCreate but never registered here, so it
	// silently fell back to text output. Register it explicitly.
	hubTokenCreateCmd.Flags().BoolVar(&tokenOutputJSON, "json", false, "Output in JSON format")
	// E.1 descriptive credential metadata: optional, bounded, immutable
	// after issuance (there is no update command). Requires a hub connection,
	// like the rest of `scion hub token`.
	hubTokenCreateCmd.Flags().StringVar(&tokenCreatePurpose, "purpose", "", "Optional bounded description of what this token is for")
	hubTokenCreateCmd.Flags().StringArrayVar(&tokenCreateLabels, "label", nil, "Optional bounded label as key=value (repeatable)")

	_ = hubTokenCreateCmd.MarkFlagRequired("name")
	_ = hubTokenCreateCmd.MarkFlagRequired("scopes")

	hubTokenListCmd.Flags().BoolVar(&tokenOutputJSON, "json", false, "Output in JSON format")
	hubTokenListCmd.Flags().StringVar(&tokenListProject, "project", "", "Show only project tokens of this project (name or ID); never matches hub tokens")
	hubTokenListCmd.Flags().StringVar(&tokenListBoundary, "boundary", "", "Show only tokens of this boundary: project or hub")
	hubTokenListCmd.Flags().BoolVar(&tokenListWide, "wide", false, "Also show each token's purpose")

	hubTokenScopesCmd.Flags().StringVar(&tokenScopesProject, "project", "", "Project name or ID to compute mint eligibility for (project boundary)")
	hubTokenScopesCmd.Flags().StringVar(&tokenScopesBoundary, "boundary", "", "Boundary to compute mint eligibility for: project (needs --project) or hub (no --project)")
	hubTokenScopesCmd.Flags().BoolVar(&tokenOutputJSON, "json", false, "Output in JSON format")
}

// Token boundary kinds accepted by --boundary.
const (
	tokenBoundaryProject = hubclient.BoundaryProject
	tokenBoundaryHub     = hubclient.BoundaryHub
)

// hubBoundaryDescription describes a hub-boundary token in create output.
const hubBoundaryDescription = "hub (any project you can currently access; each request is checked against your current access)"

// validateTokenCreateBoundary checks --boundary against --project: a
// project boundary needs a project, a hub boundary must not name one.
func validateTokenCreateBoundary(boundary, project string) error {
	switch boundary {
	case "", tokenBoundaryProject:
		if project == "" {
			return newUsageError("--project is required (or use --boundary hub for a token not tied to a project)")
		}
	case tokenBoundaryHub:
		if project != "" {
			return newUsageError("--project cannot be used with --boundary hub")
		}
	default:
		return newUsageError("invalid --boundary %q: use project or hub", boundary)
	}
	return nil
}

func runTokenCreate(cmd *cobra.Command, args []string) error {
	if err := validateTokenCreateBoundary(tokenCreateBoundary, tokenCreateProject); err != nil {
		return err
	}
	hubBoundary := tokenCreateBoundary == tokenBoundaryHub

	_, client, uat, err := loadHubClientUAT()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Resolve project name/slug to ID
	var project *hubclient.Project
	if !hubBoundary {
		project, err = resolveProjectByNameOrID(ctx, client, tokenCreateProject, uat)
		if err != nil {
			return fmt.Errorf("failed to resolve project %q: %w", tokenCreateProject, err)
		}
	}

	scopes := splitCommaList(tokenCreateScopes)
	if len(scopes) == 0 {
		return newUsageError("--scopes must specify at least one scope")
	}

	var expiresAt *time.Time
	if tokenCreateExpires != "" {
		t, err := parseExpiry(tokenCreateExpires)
		if err != nil {
			return newUsageError("invalid --expires value: %w", err)
		}
		expiresAt = &t
	}

	labels, err := parseLabelFlags(tokenCreateLabels)
	if err != nil {
		return err
	}

	req := &hubclient.CreateTokenRequest{
		Name:      tokenCreateName,
		Scopes:    scopes,
		ExpiresAt: expiresAt,
		Purpose:   tokenCreatePurpose,
		Labels:    labels,
	}
	if hubBoundary {
		req.Boundary = &hubclient.TokenBoundary{Kind: hubclient.BoundaryHub}
	} else {
		// Both forms name the same project: the explicit boundary, and the
		// shorthand a hub without the boundary field reads.
		req.ProjectID = project.ID
		req.Boundary = &hubclient.TokenBoundary{Kind: hubclient.BoundaryProject, ProjectID: project.ID}
	}

	resp, err := client.Tokens().Create(ctx, req)
	if err != nil {
		if selector, reason, ok := hubclient.AsScopeViolation(err); ok {
			// An older hub's scope_violation body may carry no details; in
			// that case naming an empty selector would be misleading, but
			// the hint to check eligibility is still useful either way.
			if selector != "" {
				fmt.Fprintf(os.Stderr, "Denied scope %q (%s).\n", selector, scopeReasonText(reason))
			}
			if hubBoundary {
				fmt.Fprintln(os.Stderr, "Run `scion hub token scopes --boundary hub` to see which scopes you may currently select and why.")
			} else {
				fmt.Fprintf(os.Stderr, "Run `scion hub token scopes --project %s` to see which scopes you may currently select and why.\n", tokenCreateProject)
			}
		}
		return fmt.Errorf("failed to create token: %w", err)
	}

	if tokenOutputJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(resp)
	}

	fmt.Printf("Created access token: %s\n", resp.AccessToken.Name)
	fmt.Printf("  ID:      %s\n", resp.AccessToken.ID)
	if hubBoundary {
		fmt.Printf("  Boundary: %s\n", hubBoundaryDescription)
	} else {
		fmt.Printf("  Project:   %s (%s)\n", project.Name, project.ID)
	}
	fmt.Printf("  Scopes:  %s\n", strings.Join(resp.AccessToken.Scopes, ", "))
	if resp.AccessToken.ExpiresAt != nil {
		fmt.Printf("  Expires: %s\n", clitime.Format(*resp.AccessToken.ExpiresAt, clitime.Full))
	}
	if resp.AccessToken.Purpose != "" {
		fmt.Printf("  Purpose: %s\n", resp.AccessToken.Purpose)
	}
	if len(resp.AccessToken.Labels) > 0 {
		fmt.Printf("  Labels:  %s\n", formatLabels(resp.AccessToken.Labels))
	}
	fmt.Println()
	fmt.Printf("Token: %s\n", resp.Token)
	fmt.Println()
	fmt.Println("This token will not be shown again. Store it securely.")

	return nil
}

func runTokenList(cmd *cobra.Command, args []string) error {
	if err := validateTokenListFilters(tokenListBoundary, tokenListProject); err != nil {
		return err
	}

	_, client, uat, err := loadHubClientUAT()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := client.Tokens().List(ctx)
	if err != nil {
		return fmt.Errorf("failed to list tokens: %w", err)
	}

	// Optionally filter by project
	var projectID string
	if tokenListProject != "" {
		project, err := resolveProjectByNameOrID(ctx, client, tokenListProject, uat)
		if err != nil {
			return fmt.Errorf("failed to resolve project %q: %w", tokenListProject, err)
		}
		projectID = project.ID
	}

	items := filterTokens(resp.Items, tokenListBoundary, projectID)

	if tokenOutputJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]interface{}{"items": items})
	}

	if len(items) == 0 {
		fmt.Println("No access tokens found.")
		return nil
	}

	printTokenTable(items, tokenListWide)
	return nil
}

// tokenListPurposeWidth is the width of the PURPOSE column under --wide.
const tokenListPurposeWidth = 30

// printTokenTable renders token list output as text. Every row shows the
// token's boundary; wide adds a truncated PURPOSE column.
func printTokenTable(items []hubclient.TokenInfo, wide bool) {
	header := []string{"NAME", "ID", "PREFIX", "STATUS", "BOUNDARY", "EXPIRES"}
	widths := []int{20, 36, 16, 10, 44, 25}
	if wide {
		header = append(header, "PURPOSE")
		widths = append(widths, tokenListPurposeWidth)
	}
	printRow := func(cols []string, last string) {
		for i, c := range cols {
			fmt.Printf("%-*s  ", widths[i], c)
		}
		fmt.Println(last)
	}
	rule := make([]string, len(widths))
	for i, w := range widths {
		rule[i] = strings.Repeat("-", w)
	}
	printRow(header, "SCOPES")
	printRow(rule, "------")

	for _, t := range items {
		status := "active"
		if t.Revoked {
			status = "revoked"
		} else if t.ExpiresAt != nil && t.ExpiresAt.Before(time.Now()) {
			status = "expired"
		}

		expires := "never"
		if t.ExpiresAt != nil {
			expires = clitime.Format(*t.ExpiresAt, clitime.Full)
		}

		cols := []string{truncate(t.Name, 20), t.ID, t.Prefix, status, tokenBoundaryLabel(t), expires}
		if wide {
			cols = append(cols, truncate(t.Purpose, tokenListPurposeWidth))
		}
		printRow(cols, strings.Join(t.Scopes, ","))
	}
}

// tokenBoundaryLabel renders a token's boundary for the BOUNDARY column:
// "hub", "project:<project ID>" ("project" when the hub reports no project
// ID), or "unknown" when the hub reports neither a boundary nor a project.
func tokenBoundaryLabel(t hubclient.TokenInfo) string {
	switch kind := t.BoundaryKind(); kind {
	case hubclient.BoundaryProject:
		if id := tokenProjectID(t); id != "" {
			return "project:" + id
		}
		return hubclient.BoundaryProject
	case "":
		return "unknown"
	default:
		return kind
	}
}

// tokenProjectID returns the project of a project-boundary token, or ""
// for any other boundary.
func tokenProjectID(t hubclient.TokenInfo) string {
	if t.BoundaryKind() != hubclient.BoundaryProject {
		return ""
	}
	if t.Boundary != nil && t.Boundary.ProjectID != "" {
		return t.Boundary.ProjectID
	}
	return t.ProjectID
}

// validateTokenListFilters checks --boundary against --project for hub
// token list. --project selects project tokens, so it cannot be combined
// with --boundary hub.
func validateTokenListFilters(boundary, project string) error {
	switch boundary {
	case "", tokenBoundaryProject:
	case tokenBoundaryHub:
		if project != "" {
			return newUsageError("--project cannot be used with --boundary hub")
		}
	default:
		return newUsageError("invalid --boundary %q: use project or hub", boundary)
	}
	return nil
}

// filterTokens keeps the tokens of the given boundary kind (all when
// boundary is empty) and, when projectID is set, only project-boundary
// tokens of that project. A hub token never matches a project filter, and
// a token of unknown boundary matches no boundary filter.
func filterTokens(items []hubclient.TokenInfo, boundary, projectID string) []hubclient.TokenInfo {
	if boundary == "" && projectID == "" {
		return items
	}
	var filtered []hubclient.TokenInfo
	for _, t := range items {
		if boundary != "" && t.BoundaryKind() != boundary {
			continue
		}
		if projectID != "" && tokenProjectID(t) != projectID {
			continue
		}
		filtered = append(filtered, t)
	}
	return filtered
}

// validateTokenScopesBoundary checks --boundary against --project for hub
// token scopes: --project alone means the project boundary, a project
// boundary needs a project, and a hub boundary must not name one.
func validateTokenScopesBoundary(boundary, project string) error {
	switch boundary {
	case "":
	case tokenBoundaryProject:
		if project == "" {
			return newUsageError("--project is required with --boundary project")
		}
	case tokenBoundaryHub:
		if project != "" {
			return newUsageError("--project cannot be used with --boundary hub")
		}
	default:
		return newUsageError("invalid --boundary %q: use project or hub", boundary)
	}
	return nil
}

// unsupportedBoundaryNotice is printed when the hub cannot compute
// hub-boundary eligibility and the CLI shows the catalog instead.
const unsupportedBoundaryNotice = "this hub cannot list hub eligibility; hub-only scopes are marked in the catalog"

func runTokenScopes(cmd *cobra.Command, args []string) error {
	if err := validateTokenScopesBoundary(tokenScopesBoundary, tokenScopesProject); err != nil {
		return err
	}
	hubBoundary := tokenScopesBoundary == tokenBoundaryHub

	_, client, uat, err := loadHubClientUAT()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	opts := &hubclient.ListScopesOptions{}
	if hubBoundary {
		opts.Boundary = hubclient.BoundaryHub
	} else if tokenScopesProject != "" {
		project, err := resolveProjectByNameOrID(ctx, client, tokenScopesProject, uat)
		if err != nil {
			return fmt.Errorf("failed to resolve project %q: %w", tokenScopesProject, err)
		}
		opts.ProjectID = project.ID
	}

	withEligibility := hubBoundary || tokenScopesProject != ""
	catalogFallback := false
	resp, err := client.Tokens().ListScopes(ctx, opts)
	if err != nil && hubBoundary && isUnsupportedBoundary(err) {
		fmt.Fprintln(os.Stderr, unsupportedBoundaryNotice)
		resp, err = client.Tokens().ListScopes(ctx, nil)
		if err == nil {
			resp = filterScopesForBoundary(resp, hubclient.BoundaryHub)
			withEligibility = false
			catalogFallback = true
		}
	}
	if err != nil {
		return fmt.Errorf("failed to list scopes: %w", err)
	}

	if tokenOutputJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(resp)
	}

	if catalogFallback {
		printScopeCatalogWithBoundaries(resp)
		return nil
	}
	printScopeTable(resp, withEligibility)
	return nil
}

// isUnsupportedBoundary reports whether err is the unsupported_boundary
// error a hub without hub-boundary eligibility returns for boundary=hub.
func isUnsupportedBoundary(err error) bool {
	var apiErr *apiclient.APIError
	return errors.As(err, &apiErr) && apiErr.Code == "unsupported_boundary"
}

// filterScopesForBoundary drops the scopes a token of the given boundary
// may not select (per AllowedBoundaries), and every alias that expands to
// one of them. A scope that publishes no AllowedBoundaries is kept; the hub
// still checks every scope on create.
func filterScopesForBoundary(resp *hubclient.ScopesResponse, kind string) *hubclient.ScopesResponse {
	removed := make(map[string]bool)
	out := &hubclient.ScopesResponse{}
	for _, s := range resp.Scopes {
		if len(s.AllowedBoundaries) > 0 && !slices.Contains(s.AllowedBoundaries, kind) {
			removed[s.ID] = true
			continue
		}
		out.Scopes = append(out.Scopes, s)
	}
	for _, a := range resp.Aliases {
		keep := true
		for _, m := range a.ExpandsTo {
			if removed[m] {
				keep = false
				break
			}
		}
		if keep {
			out.Aliases = append(out.Aliases, a)
		}
	}
	return out
}

// printScopeCatalogWithBoundaries renders the scope catalog with a
// BOUNDARIES column, so the scopes only a hub token may select are marked.
func printScopeCatalogWithBoundaries(resp *hubclient.ScopesResponse) {
	fmt.Printf("%-24s  %-12s  %s\n", "SCOPE", "BOUNDARIES", "DESCRIPTION")
	for _, s := range resp.Scopes {
		boundaries := "-"
		if len(s.AllowedBoundaries) > 0 {
			boundaries = strings.Join(s.AllowedBoundaries, ",")
		}
		fmt.Printf("%-24s  %-12s  %s\n", s.ID, boundaries, s.Description)
	}
	for _, a := range resp.Aliases {
		fmt.Printf("%-24s  %-12s  %s\n", a.ID, "-", a.Description)
	}
}

// scopeReasonText returns the CLI text for an eligibility or
// scope_violation reason code. Codes without a text are shown as-is.
func scopeReasonText(reason string) string {
	switch reason {
	case "authority_required":
		return "your account has no current access that includes this permission"
	default:
		return reason
	}
}

// printScopeTable renders GET /api/v1/auth/scopes output as text. When
// withEligibility is false (no boundary requested), it prints the selector
// catalog. Otherwise it adds ELIGIBLE/REASON columns -- eligible
// answers only "may you select this restriction," never a target list.
func printScopeTable(resp *hubclient.ScopesResponse, withEligibility bool) {
	if !withEligibility {
		fmt.Printf("%-24s  %s\n", "SCOPE", "DESCRIPTION")
		for _, s := range resp.Scopes {
			fmt.Printf("%-24s  %s\n", s.ID, s.Description)
		}
		for _, a := range resp.Aliases {
			fmt.Printf("%-24s  %s\n", a.ID, a.Description)
		}
		return
	}

	fmt.Printf("%-24s  %-9s  %s\n", "SCOPE", "ELIGIBLE", "REASON")
	for _, s := range resp.Scopes {
		eligible, reason := "-", ""
		if s.Eligibility != nil {
			eligible = fmt.Sprintf("%v", s.Eligibility.Eligible)
			reason = scopeReasonText(s.Eligibility.Reason)
		}
		fmt.Printf("%-24s  %-9s  %s\n", s.ID, eligible, reason)
	}
	for _, a := range resp.Aliases {
		eligible, reason := "-", ""
		if a.Eligibility != nil {
			eligible = fmt.Sprintf("%v", a.Eligibility.Eligible)
			if !a.Eligibility.Eligible && len(a.Eligibility.IneligibleMembers) > 0 {
				reason = "members: " + strings.Join(a.Eligibility.IneligibleMembers, ",")
			}
		}
		fmt.Printf("%-24s  %-9s  %s\n", a.ID, eligible, reason)
	}
}

func runTokenRevoke(cmd *cobra.Command, args []string) error {
	tokenID := args[0]

	_, client, err := loadHubClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := client.Tokens().Revoke(ctx, tokenID); err != nil {
		return fmt.Errorf("failed to revoke token: %w", err)
	}

	fmt.Printf("Token %s revoked.\n", tokenID)
	return nil
}

func runTokenDelete(cmd *cobra.Command, args []string) error {
	tokenID := args[0]

	_, client, err := loadHubClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := client.Tokens().Delete(ctx, tokenID); err != nil {
		return fmt.Errorf("failed to delete token: %w", err)
	}

	fmt.Printf("Token %s deleted.\n", tokenID)
	return nil
}

// parseLabelFlags parses repeated --label key=value flags into a map,
// rejecting a repeated key rather than silently keeping the last value
// (review finding F12). It does not otherwise enforce the bounded label
// schema; the hub validates and rejects out-of-schema labels server-side
// (E.1), so the CLI reports the server's error rather than duplicating the
// rule set.
func parseLabelFlags(labels []string) (map[string]string, error) {
	if len(labels) == 0 {
		return nil, nil
	}
	result := make(map[string]string, len(labels))
	for _, l := range labels {
		key, value, ok := strings.Cut(l, "=")
		if !ok {
			return nil, newUsageError("invalid --label %q: expected key=value", l)
		}
		if _, exists := result[key]; exists {
			return nil, newUsageError("duplicate --label key %q", key)
		}
		result[key] = value
	}
	return result, nil
}

// formatLabels renders labels as a stable, comma-separated key=value list.
func formatLabels(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+labels[k])
	}
	return strings.Join(parts, ", ")
}

// expiryAcceptedForms describes every --expires form parseExpiry accepts. It
// is shared by the --expires flag usage, the command's long help and the
// parse error so the three cannot drift apart.
const expiryAcceptedForms = "a positive duration in minutes (90m), hours (2h), days (30d) or years (1y), or an RFC 3339 date (2026-12-31T00:00:00Z)"

// expiryUnitNote spells out that "m" is minutes, since it could be read as
// months. It is shown in the command's long help.
const expiryUnitNote = "m means minutes; there is no month unit (use 30d or 1y for longer)"

// parseExpiry parses an expiry string as either a duration shorthand
// (90m, 2h, 30d, 1y) or an RFC 3339 timestamp.
func parseExpiry(s string) (time.Time, error) {
	return parseExpiryAt(s, time.Now().UTC())
}

// parseExpiryAt is parseExpiry with an explicit reference time, so tests can
// assert exact results.
func parseExpiryAt(s string, now time.Time) (time.Time, error) {
	// Try RFC 3339 first
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}

	invalid := fmt.Errorf("%q is not a valid expiry: expected %s", s, expiryAcceptedForms)

	// Try duration shorthand: Nm (minutes), Nh (hours), Nd (days) or Ny (years)
	s = strings.TrimSpace(s)
	if len(s) < 2 {
		return time.Time{}, invalid
	}

	unit := s[len(s)-1]
	numStr := s[:len(s)-1]

	// Every unit parses its number strictly, so Go-style or fractional
	// inputs such as 1h30m, 1.5h, 1.5d or 3xd are rejected rather than read
	// as a shorter duration.
	var step time.Duration
	switch unit {
	case 'm':
		step = time.Minute
	case 'h':
		step = time.Hour
	case 'd':
		step = 24 * time.Hour
	case 'y':
		// A year is a fixed 365 days, not a calendar year, so 1y always
		// fits the hub's 365-day maximum token lifetime even when the
		// following year contains 29 February.
		step = 365 * 24 * time.Hour
	default:
		return time.Time{}, invalid
	}
	n, err := strconv.Atoi(numStr)
	if err != nil || n <= 0 {
		return time.Time{}, invalid
	}
	// Values above the hub's maximum token lifetime (1y, 365d, 8760h or
	// 525600m) are rejected here, before multiplying, so a huge number
	// cannot overflow into a negative duration (an expiry in the past).
	if int64(n) > int64(store.UATMaxExpiry/step) {
		return time.Time{}, fmt.Errorf("%q exceeds the maximum expiry of 1 year (1y, %dd, %dh or %dm)",
			s, int64(store.UATMaxExpiry/(24*time.Hour)), int64(store.UATMaxExpiry/time.Hour),
			int64(store.UATMaxExpiry/time.Minute))
	}
	return now.Add(time.Duration(n) * step), nil
}
