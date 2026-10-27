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
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
)

// scion hub delegation: agent delegation grant management
// (.design/agent-delegation.md §18.8). User mode only: the command is not in
// agentAllowed, so it is removed in agent mode. The scion CLI never
// consumes a delegated credential; an agent exchanges its grants through
// sciontool.

// agentDelegationAttachNotice repeats the consequences of delegating
// agent:attach (.design/agent-delegation.md §11.9). create --help shows it,
// and create prints it whenever agent:attach is selected.
const agentDelegationAttachNotice = `agent:attach gives the agent shell access in target agents' containers. That includes each
target's credentials and material: its agent token (~/.scion/scion-token, refreshable), its
environment and files, and its own sciontool delegation socket with any delegated credentials it
holds. The agent can then act with the target's own authority, which this grant does not bound.
An open session can outlive revocation and the credential's expiry, and revoking cannot retract
anything already copied. Attach is checked only when a session starts or reconnects.`

var hubDelegationCmd = &cobra.Command{
	Use:   "delegation",
	Short: "Delegate a bounded, time-limited subset of your Hub access to an agent",
	Long: `Delegate a bounded, time-limited subset of your Hub access to an agent you own or created.

The agent exchanges a grant for short-lived credentials through sciontool; the scion CLI never
handles those credentials. Every use is checked against your live access at that moment: if you
lose access, or the grant is revoked or expires, the agent's delegated access stops. Grant
management needs your interactive login; an access token is refused.

The Hub must have the hub.agent_delegation experiment enabled for issuance and exchange. Lists,
reads, revoke and revoke-all always work, so grants can be reviewed and ended at any time.`,
}

var hubDelegationCreateCmd = &cobra.Command{
	Use:   "create <agent>",
	Short: "Grant an agent a bounded, time-limited subset of your permissions",
	Long: `Grant <agent> (ID or name) a subset of your permissions, bounded to one project or to the Hub.
You must be the agent's owner or the user it was created for.

Delegated credentials last 15 minutes by default (at most 60) and at most 2 are active per grant.
The agent cannot pass the grant on. The decision bounds what the agent is allowed to do, not the
computation that follows it: a message instructs an independently authorized recipient.

When --permission includes agent:attach:
` + agentDelegationAttachNotice,
	Args: cobra.ExactArgs(1),
	RunE: runHubDelegationCreate,
}

var hubDelegationListCmd = &cobra.Command{
	Use:   "list [<agent>]",
	Short: "List delegation grants of an agent, or the grants you issued",
	Long: `List the delegation grants bound to <agent> that you may see, or, without an argument, the
grants you issued. Revoked and expired grants are listed too; the list is not paginated.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runHubDelegationList,
}

var hubDelegationGetCmd = &cobra.Command{
	Use:   "get <agent> <grant-id>",
	Short: "Show one grant, with its status and active credentials",
	Args:  cobra.ExactArgs(2),
	RunE:  runHubDelegationGet,
}

var hubDelegationRevokeCmd = &cobra.Command{
	Use:   "revoke <agent> <grant-id>",
	Short: "Revoke a grant and every credential exchanged from it, or one credential",
	Args:  cobra.ExactArgs(2),
	RunE:  runHubDelegationRevoke,
}

var hubDelegationRevokeAllCmd = &cobra.Command{
	Use:   "revoke-all (--mine | <agent>)",
	Short: "Revoke every grant you issued, or every grant of one agent",
	Long: `Revoke every unrevoked grant you issued (--mine), or every unrevoked grant bound to <agent>
that you may revoke this way. Grants you may see but not revoke this way are reported as
skipped; revoke them one by one with 'scion hub delegation revoke'.

It asks for confirmation on a terminal (default No). --yes (-y) or --non-interactive confirms; without
a terminal and without either flag it refuses.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runHubDelegationRevokeAll,
}

var (
	delegationPermissions []string
	delegationBoundary    string
	delegationBoundaryPrj string
	delegationExpires     string
	delegationName        string
	delegationPurpose     string
	delegationLabels      []string
	delegationMaxCredTTL  time.Duration
	delegationCredential  string
	delegationMine        bool
	delegationJSON        bool
)

func init() {
	hubCmd.AddCommand(hubDelegationCmd)
	hubDelegationCmd.AddCommand(hubDelegationCreateCmd, hubDelegationListCmd, hubDelegationGetCmd,
		hubDelegationRevokeCmd, hubDelegationRevokeAllCmd)

	f := hubDelegationCreateCmd.Flags()
	f.StringArrayVar(&delegationPermissions, "permission", nil, "Permission to delegate, repeatable (for example agent:read, agent:list, project:read, agent:message, agent:attach)")
	f.StringVar(&delegationBoundary, "boundary", "project", "Grant boundary: project (one project) or hub (the whole Hub)")
	f.StringVar(&delegationBoundaryPrj, "boundary-project", "", "Project ID of a project boundary (default: the agent's project)")
	f.StringVar(&delegationExpires, "expires", "", "Grant lifetime, for example 12h or 7d (default 7d, at most 30d)")
	f.StringVar(&delegationName, "name", "", "Short name for the grant, shown in lists and audit (required)")
	f.StringVar(&delegationPurpose, "purpose", "", "Free-text note (descriptive only, never used for authorization)")
	f.StringArrayVar(&delegationLabels, "label", nil, "Descriptive label key=value, repeatable")
	f.DurationVar(&delegationMaxCredTTL, "max-credential-ttl", 0, "Lifetime cap of each delegated credential (default 15m, at most 60m)")

	hubDelegationRevokeCmd.Flags().StringVar(&delegationCredential, "credential", "", "Revoke only this credential of the grant")
	hubDelegationRevokeAllCmd.Flags().BoolVar(&delegationMine, "mine", false, "Revoke every grant you issued")
	hubDelegationRevokeAllCmd.Flags().BoolVarP(&autoConfirm, "yes", "y", false, "Answer Yes to the confirmation prompt (required to confirm when stdin is not a terminal)")
	hubDelegationRevokeAllCmd.Flags().BoolVar(&nonInteractive, "non-interactive", false, "Non-interactive mode: implies --yes (answers Yes to every confirmation), errors on ambiguous prompts")
	for _, c := range []*cobra.Command{hubDelegationCreateCmd, hubDelegationListCmd, hubDelegationGetCmd, hubDelegationRevokeAllCmd} {
		c.Flags().BoolVar(&delegationJSON, "json", false, "Print the Hub's answer as JSON")
	}
}

// hubDelegationClientFn returns the Hub client of the delegation commands.
// Tests replace it.
var hubDelegationClientFn = func() (hubclient.Client, error) {
	_, client, err := loadHubClient()
	return client, err
}

// hubDelegationConfirm asks for confirmation of a destructive delegation
// command: --yes or --non-interactive confirms; otherwise an interactive
// terminal is asked (default No), and without a terminal it is refused.
// Tests replace it.
var hubDelegationConfirm = func(prompt string) bool {
	return hubsync.ConfirmAction(prompt, false, autoConfirm || nonInteractive)
}

// maxDelegationExpiresDays is the largest whole number of days that fits in a
// time.Duration (106751); a larger day count would overflow.
const maxDelegationExpiresDays = int(math.MaxInt64 / int64(24*time.Hour))

// parseDelegationExpires parses a grant lifetime: a Go duration, or a whole
// number of days with a "d" suffix.
func parseDelegationExpires(s string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n <= 0 || n > maxDelegationExpiresDays {
			return 0, fmt.Errorf("invalid --expires %q: use a duration such as 12h or a number of days such as 7d", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("invalid --expires %q: use a duration such as 12h or a number of days such as 7d", s)
	}
	return d, nil
}

// buildCreateAgentDelegationRequest turns the create flags into a request.
// projectOfAgent is the agent's project, the default boundary.
func buildCreateAgentDelegationRequest(projectOfAgent string, now time.Time) (*hubclient.CreateAgentDelegationRequest, error) {
	if len(delegationPermissions) == 0 {
		return nil, newUsageError("at least one --permission is required")
	}
	if strings.TrimSpace(delegationName) == "" {
		return nil, newUsageError("--name is required")
	}
	req := &hubclient.CreateAgentDelegationRequest{
		Permissions: append([]string(nil), delegationPermissions...),
		Name:        delegationName,
		Purpose:     delegationPurpose,
	}
	switch delegationBoundary {
	case "hub":
		if delegationBoundaryPrj != "" {
			return nil, newUsageError("--boundary-project applies to --boundary project only")
		}
		req.Boundary = hubclient.AgentDelegationBoundary{Kind: "hub"}
	case "project", "":
		projectID := delegationBoundaryPrj
		if projectID == "" {
			projectID = projectOfAgent
		}
		if projectID == "" {
			return nil, newUsageError("the agent has no project; pass --boundary-project or --boundary hub")
		}
		req.Boundary = hubclient.AgentDelegationBoundary{Kind: "project", ProjectID: projectID}
	default:
		return nil, newUsageError("invalid --boundary %q: must be project or hub", delegationBoundary)
	}
	if delegationExpires != "" {
		d, err := parseDelegationExpires(delegationExpires)
		if err != nil {
			return nil, asUsageError(err)
		}
		exp := now.Add(d).UTC()
		req.ExpiresAt = &exp
	}
	for _, l := range delegationLabels {
		k, v, ok := strings.Cut(l, "=")
		if !ok || k == "" {
			return nil, newUsageError("invalid --label %q: use key=value", l)
		}
		if req.Labels == nil {
			req.Labels = map[string]string{}
		}
		req.Labels[k] = v
	}
	if delegationMaxCredTTL != 0 {
		if delegationMaxCredTTL < time.Second || delegationMaxCredTTL > time.Hour {
			return nil, newUsageError("invalid --max-credential-ttl %s: must be between 1s and 60m", delegationMaxCredTTL)
		}
		secs := int(delegationMaxCredTTL / time.Second)
		req.MaxCredentialTTLSeconds = &secs
	}
	return req, nil
}

func delegatesAttach(permissions []string) bool {
	return slices.Contains(permissions, "agent:attach") || slices.Contains(permissions, "agent.attach")
}

func runHubDelegationCreate(cmd *cobra.Command, args []string) error {
	client, err := hubDelegationClientFn()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()
	agent, err := resolveReferenceAgent(ctx, client.Agents(), args[0])
	if err != nil {
		return err
	}
	req, err := buildCreateAgentDelegationRequest(agent.ProjectID, time.Now())
	if err != nil {
		return err
	}
	if delegatesAttach(req.Permissions) {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Note: %s\n\n", agentDelegationAttachNotice)
	}
	g, err := client.AgentDelegations().Create(ctx, agent.ID, req)
	if err != nil {
		return fmt.Errorf("failed to create the grant: %w", err)
	}
	if delegationJSON {
		return writeJSONTo(cmd.OutOrStdout(), g)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Created grant %s for agent %s\n", g.ID, g.AgentID)
	writeAgentDelegationDetail(cmd.OutOrStdout(), g)
	return nil
}

func agentDelegationBoundaryText(b hubclient.AgentDelegationBoundary) string {
	if b.Kind == "project" {
		return "project " + b.ProjectID
	}
	return b.Kind
}

func writeAgentDelegationDetail(out io.Writer, g *hubclient.AgentDelegationGrant) {
	_, _ = fmt.Fprintf(out, "  Name:        %s (user-supplied)\n", g.Name)
	_, _ = fmt.Fprintf(out, "  Status:      %s\n", g.Status)
	_, _ = fmt.Fprintf(out, "  Boundary:    %s\n", agentDelegationBoundaryText(g.Boundary))
	_, _ = fmt.Fprintf(out, "  Permissions: %s\n", strings.Join(g.Permissions, ", "))
	_, _ = fmt.Fprintf(out, "  Issuer:      %s\n", g.IssuerUserID)
	_, _ = fmt.Fprintf(out, "  Expires:     %s\n", clitime.Format(g.ExpiresAt, clitime.Full))
	if g.RevokedAt != nil {
		_, _ = fmt.Fprintf(out, "  Revoked:     %s by %s (%s)\n", clitime.Format(*g.RevokedAt, clitime.Full), g.RevokedBy, g.RevokeReason)
	}
	_, _ = fmt.Fprintf(out, "  Active credentials: %d\n", g.ActiveCredentialCount)
	for _, c := range g.ActiveCredentials {
		_, _ = fmt.Fprintf(out, "    %s  expires %s\n", c.ID, clitime.Format(c.ExpiresAt, clitime.Full))
	}
}

func runHubDelegationList(cmd *cobra.Command, args []string) error {
	client, err := hubDelegationClientFn()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()
	var resp *hubclient.ListAgentDelegationsResponse
	if len(args) == 1 {
		agent, err := resolveReferenceAgent(ctx, client.Agents(), args[0])
		if err != nil {
			return err
		}
		resp, err = client.AgentDelegations().List(ctx, agent.ID)
		if err != nil {
			return fmt.Errorf("failed to list grants: %w", err)
		}
	} else {
		resp, err = client.AgentDelegations().ListMine(ctx)
		if err != nil {
			return fmt.Errorf("failed to list grants: %w", err)
		}
	}
	out := cmd.OutOrStdout()
	if delegationJSON {
		return writeJSONTo(out, resp)
	}
	if len(resp.Delegations) == 0 {
		_, _ = fmt.Fprintln(out, "No grants.")
		return nil
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tAGENT\tSTATUS\tBOUNDARY\tPERMISSIONS\tEXPIRES\tACTIVE\tNAME")
	for _, g := range resp.Delegations {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\n", g.ID, g.AgentID, g.Status, agentDelegationBoundaryText(g.Boundary),
			strings.Join(g.Permissions, ","), clitime.Format(g.ExpiresAt, clitime.Minute), g.ActiveCredentialCount, g.Name)
	}
	return w.Flush()
}

func runHubDelegationGet(cmd *cobra.Command, args []string) error {
	client, err := hubDelegationClientFn()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()
	agent, err := resolveReferenceAgent(ctx, client.Agents(), args[0])
	if err != nil {
		return err
	}
	g, err := client.AgentDelegations().Get(ctx, agent.ID, args[1])
	if err != nil {
		if apiclient.IsNotFoundError(err) {
			return fmt.Errorf("grant %s not found", args[1])
		}
		return fmt.Errorf("failed to read grant %s: %w", args[1], err)
	}
	if delegationJSON {
		return writeJSONTo(cmd.OutOrStdout(), g)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Grant %s for agent %s\n", g.ID, g.AgentID)
	writeAgentDelegationDetail(cmd.OutOrStdout(), g)
	return nil
}

func runHubDelegationRevoke(cmd *cobra.Command, args []string) error {
	client, err := hubDelegationClientFn()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()
	agent, err := resolveReferenceAgent(ctx, client.Agents(), args[0])
	if err != nil {
		return err
	}
	if delegationCredential != "" {
		if err := client.AgentDelegations().RevokeCredential(ctx, agent.ID, args[1], delegationCredential); err != nil {
			return fmt.Errorf("failed to revoke credential %s: %w", delegationCredential, err)
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Revoked credential %s of grant %s.\n", delegationCredential, args[1])
		return nil
	}
	if err := client.AgentDelegations().Revoke(ctx, agent.ID, args[1]); err != nil {
		if apiclient.IsNotFoundError(err) {
			return fmt.Errorf("grant %s not found", args[1])
		}
		return fmt.Errorf("failed to revoke grant %s: %w", args[1], err)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Revoked grant %s and its credentials.\n", args[1])
	return nil
}

func runHubDelegationRevokeAll(cmd *cobra.Command, args []string) error {
	if delegationMine == (len(args) == 1) {
		return newUsageError("pass either --mine or <agent>")
	}
	client, err := hubDelegationClientFn()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()
	var resp *hubclient.RevokeAllAgentDelegationsResponse
	if delegationMine {
		if !hubDelegationConfirm("Revoke every unrevoked agent delegation grant you issued? This cannot be undone") {
			return fmt.Errorf("revoke-all cancelled")
		}
		resp, err = client.AgentDelegations().RevokeAllMine(ctx)
	} else {
		agent, rerr := resolveReferenceAgent(ctx, client.Agents(), args[0])
		if rerr != nil {
			return rerr
		}
		if !hubDelegationConfirm(fmt.Sprintf("Revoke every unrevoked agent delegation grant of agent %s that you may revoke? This cannot be undone", agent.ID)) {
			return fmt.Errorf("revoke-all cancelled")
		}
		resp, err = client.AgentDelegations().RevokeAll(ctx, agent.ID)
	}
	if err != nil {
		return fmt.Errorf("failed to revoke grants: %w", err)
	}
	out := cmd.OutOrStdout()
	if delegationJSON {
		return writeJSONTo(out, resp)
	}
	_, _ = fmt.Fprintf(out, "Revoked %d grant(s).\n", len(resp.Revoked))
	for _, id := range resp.Revoked {
		_, _ = fmt.Fprintf(out, "  revoked %s\n", id)
	}
	if len(resp.Skipped) > 0 {
		_, _ = fmt.Fprintf(out, "Skipped %d grant(s) you may not revoke this way:\n", len(resp.Skipped))
		for _, s := range resp.Skipped {
			_, _ = fmt.Fprintf(out, "  skipped %s (agent %s, %s)\n", s.GrantID, s.AgentID, s.Reason)
		}
	}
	return nil
}
