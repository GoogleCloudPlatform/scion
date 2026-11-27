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
	"math"
	"net/http"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

const (
	usageUnknownCell = "—"
)

// usageLoadHubClient loads the Hub client and UAT flag using the canonical
// project-resolution loader. Overridden only in pure unit tests that do not
// exercise the full filesystem/context/Hub resolver stack.
var usageLoadHubClient = loadHubClientForProjectResolution

// usageResolveProject resolves the active Hub project using the canonical
// CLI precedence chain (cmd/hub_project_resolver.go). Overridden only in pure
// unit tests; integration tests drive the real resolveHubProject.
var usageResolveProject = resolveHubProject

// usageCmd is the top-level `scion usage` command (Tokenomics P2.3, ptone/scion#9239).
// Allowed in agent mode (agentAllowed in cmd/cli_mode.go; scoped to the calling
// agent's own project or agent) and on Windows (windowsAllowed in cmd/platform_gate.go).
var usageCmd = newUsageCmd()

type usageFlags struct {
	user  string
	agent string
	all   bool
	since string
	until string
	by    []string
	limit int
	json  bool
}

func newUsageCmd() *cobra.Command {
	var f usageFlags
	cmd := &cobra.Command{
		Use:   "usage",
		Short: "Show Track 1 token and call usage",
		Long: `Show Track 1 LLM token and API call usage from the Hub usage ledger.

By default, scopes to the current project when invoked inside one (or the
calling agent's own project in agent mode), and otherwise to your own user
usage (/api/v1/me/usage).

Unknown token buckets are displayed as "—" (never fabricated as 0). Partial
values (where some events in a bucket had unknown counts) are marked with "*",
and saturated int64 sums are marked with "+".

Requires the tokenomics.usage_ledger experiment on the Hub.

Examples:
  scion usage
  scion usage --since 7d --by model
  scion usage --project my-proj --since 24h --by hour
  scion usage --user me --since 30d --by project,day
  scion usage --agent my-agent --since 7d
  scion usage --all --since 7d --by project
  scion usage --since 7d --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUsage(cmd, f)
		},
	}

	cmd.Flags().StringVar(&f.user, "user", "", `Query usage for a user ID (or "me" for the current user)`)
	cmd.Flags().StringVar(&f.agent, "agent", "", "Query usage for a single agent ID or slug")
	cmd.Flags().BoolVar(&f.all, "all", false, "Query hub-wide usage across all projects (hub admin only)")
	cmd.Flags().StringVar(&f.since, "since", "", "Window start (RFC 3339, YYYY-MM-DD, or relative like 7d, 24h)")
	cmd.Flags().StringVar(&f.until, "until", "", "Window end (RFC 3339 or YYYY-MM-DD)")
	cmd.Flags().StringSliceVar(&f.by, "by", nil, "Group by dimensions (comma-separated: model,harness,agent,user,project,day,hour)")
	cmd.Flags().IntVar(&f.limit, "limit", 0, "Maximum number of grouped rows returned (1..500)")
	cmd.Flags().BoolVar(&f.json, "json", false, "Output raw JSON response from the Hub")

	markFlagsForMode(cmd, ModeHuman, "user", "all")
	return cmd
}

func init() {
	rootCmd.AddCommand(usageCmd)
}

func runUsage(cmd *cobra.Command, f usageFlags) error {
	mode := resolveMode()
	if err := checkFlagsForMode(cmd, mode); err != nil {
		return err
	}

	// Only a user-typed --project or --global counts as an explicit project
	// scope; a `scion use` context-injected project (projectSource ==
	// targetSourceContext) is a default, not an explicit flag, so --all,
	// --user, and --agent work cleanly when a context project is set.
	explicitProject := (projectPath != "" && projectSource != targetSourceContext) || globalMode
	scopeCount := 0
	if explicitProject {
		scopeCount++
	}
	if strings.TrimSpace(f.user) != "" {
		scopeCount++
	}
	if strings.TrimSpace(f.agent) != "" {
		scopeCount++
	}
	if f.all {
		scopeCount++
	}
	if scopeCount > 1 {
		return newUsageError("at most one of --project/--global, --user, --agent, or --all may be specified")
	}

	groupBy, err := normalizeUsageGroupByFlag(f.by)
	if err != nil {
		return newUsageError("%s", err.Error())
	}
	if strings.TrimSpace(f.agent) != "" && len(groupBy) > 0 {
		return newUsageError("--by is not supported with --agent (agent usage always returns a by_model breakdown)")
	}
	if strings.TrimSpace(f.agent) != "" && f.limit > 0 {
		return newUsageError("--limit is not supported with --agent")
	}
	if f.limit < 0 {
		return newUsageError("--limit must be a positive integer")
	}

	client, uat, err := usageLoadHubClient()
	if err != nil {
		return err
	}

	parentCtx := cmd.Context()
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	ctx, cancel := context.WithTimeout(parentCtx, 30*time.Second)
	defer cancel()

	opts := &hubclient.UsageQueryOptions{
		Since:   strings.TrimSpace(f.since),
		Until:   strings.TrimSpace(f.until),
		GroupBy: groupBy,
		Limit:   f.limit,
	}

	wantJSON := f.json || isJSONOutput()

	// Single-agent scope.
	if agentRef := strings.TrimSpace(f.agent); agentRef != "" {
		agentID, resolveErr := resolveUsageAgentID(ctx, client, uat, agentRef)
		if resolveErr != nil {
			return resolveErr
		}
		resp, err := client.Usage().GetAgent(ctx, agentID, opts)
		if err != nil {
			return formatUsageAPIError(err)
		}
		if wantJSON {
			return writeUsageRawJSON(cmd.OutOrStdout(), resp.Raw, resp)
		}
		writeAgentUsageTable(cmd.OutOrStdout(), resp)
		return nil
	}

	var summary *hubclient.UsageSummaryResponse
	switch {
	case f.all:
		summary, err = client.Usage().GetGlobal(ctx, opts)
	case strings.TrimSpace(f.user) != "":
		u := strings.TrimSpace(f.user)
		if strings.EqualFold(u, "me") {
			summary, err = client.Usage().GetMe(ctx, opts)
		} else {
			summary, err = client.Usage().GetUser(ctx, u, opts)
		}
	case explicitProject:
		res, resolveErr := usageResolveProject(ctx, client, uat, true)
		if resolveErr != nil {
			return resolveErr
		}
		if res.UnlinkedDir != "" {
			return unlinkedScopeProjectError(res.UnlinkedDir)
		}
		if res.ID == "" {
			return errCannotInferProject
		}
		summary, err = client.Usage().GetProject(ctx, res.ID, opts)
	case mode == ModeAgent:
		res, resolveErr := usageResolveProject(ctx, client, uat, true)
		if resolveErr != nil {
			return resolveErr
		}
		if res.ID == "" {
			return fmt.Errorf("cannot determine project ID for agent scope (SCION_PROJECT_ID is not set)")
		}
		summary, err = client.Usage().GetProject(ctx, res.ID, opts)
	default:
		res, resolveErr := usageResolveProject(ctx, client, uat, false)
		if resolveErr != nil {
			return resolveErr
		}
		if res.ID != "" {
			summary, err = client.Usage().GetProject(ctx, res.ID, opts)
		} else {
			if res.UnlinkedDir != "" {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Note: directory project %s is not linked to a hub; showing your user usage (/me) instead.\n", res.UnlinkedDir)
			}
			summary, err = client.Usage().GetMe(ctx, opts)
		}
	}
	if err != nil {
		return formatUsageAPIError(err)
	}

	if wantJSON {
		return writeUsageRawJSON(cmd.OutOrStdout(), summary.Raw, summary)
	}
	writeScopedUsageTable(cmd.OutOrStdout(), summary)
	return nil
}

func normalizeUsageGroupByFlag(raw []string) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	allowed := map[string]bool{
		"model":   true,
		"harness": true,
		"agent":   true,
		"user":    true,
		"project": true,
		"day":     true,
		"hour":    true,
	}
	seen := make(map[string]bool, len(raw))
	out := make([]string, 0, len(raw))
	hasDay, hasHour := false, false
	for _, entry := range raw {
		for _, part := range strings.Split(entry, ",") {
			d := strings.TrimSpace(part)
			if d == "" {
				continue
			}
			if !allowed[d] {
				return nil, fmt.Errorf("invalid --by dimension %q (allowed: model, harness, agent, user, project, day, hour)", d)
			}
			if seen[d] {
				continue
			}
			seen[d] = true
			if d == "day" {
				hasDay = true
			}
			if d == "hour" {
				hasHour = true
			}
			out = append(out, d)
		}
	}
	if hasDay && hasHour {
		return nil, errors.New("--by cannot specify both day and hour")
	}
	return out, nil
}

func resolveUsageAgentID(ctx context.Context, client hubclient.Client, uat bool, agentRef string) (string, error) {
	if _, err := uuid.Parse(agentRef); err == nil {
		return agentRef, nil
	}
	res, err := usageResolveProject(ctx, client, uat, false)
	if err != nil {
		return "", err
	}
	if res.ID == "" {
		return "", fmt.Errorf("cannot resolve agent %q outside a linked project; pass the agent UUID or run inside its project", agentRef)
	}
	if client == nil || client.ProjectAgents(res.ID) == nil {
		return "", fmt.Errorf("cannot resolve agent %q: project agent client unavailable", agentRef)
	}
	id, err := resolveAgentIDForSubscription(ctx, client, res.ID, agentRef)
	if err != nil {
		return "", fmt.Errorf("cannot resolve agent %q in project %s: %w (pass the agent UUID to query an agent in another project)", agentRef, res.ID, err)
	}
	return id, nil
}

func formatUsageAPIError(err error) error {
	var apiErr *apiclient.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.StatusCode {
		case http.StatusNotFound:
			if strings.Contains(strings.ToLower(apiErr.Message), "route") {
				return errors.New("usage reporting is not enabled on this hub (experiment tokenomics.usage_ledger)")
			}
		}
	}
	return err
}

func writeUsageRawJSON(out io.Writer, raw json.RawMessage, fallback any) error {
	if len(raw) > 0 {
		trimmed := strings.TrimSpace(string(raw))
		_, err := fmt.Fprintln(out, trimmed)
		return err
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(fallback)
}

func writeScopedUsageTable(out io.Writer, resp *hubclient.UsageSummaryResponse) {
	if resp == nil {
		return
	}

	scopeLabel := resp.Scope.Kind
	if resp.Scope.ID != "" {
		scopeLabel = fmt.Sprintf("%s (%s)", resp.Scope.Kind, resp.Scope.ID)
	}
	_, _ = fmt.Fprintf(out, "Scope:   %s\n", scopeLabel)
	_, _ = fmt.Fprintf(out, "Window:  %s\n", formatUsageWindow(resp.Window))
	_, _ = fmt.Fprintf(out, "Status:  %s\n", formatUsageStatusLine(resp.MeasurementStatus, resp.StaleAgents, resp.UnmeasuredAgents))
	_, _ = fmt.Fprintln(out)

	var hasPartial, hasSaturated bool
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "CALLS\tERRORS\tINPUT\tOUTPUT\tCACHE READ\tCACHE WRITE\tREASONING\tSUMMABLE")
	row := formatTokenCells(resp.Tokens, &hasPartial, &hasSaturated)
	_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
		formatCommaInt(resp.Calls),
		formatCommaInt(resp.ErrorCalls),
		row.input, row.output, row.cacheRead, row.cacheWrite, row.reasoning, row.summable,
	)
	_ = tw.Flush()

	if len(resp.Groups) > 0 {
		_, _ = fmt.Fprintln(out)
		gtw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		dims := resolveGroupColumns(resp.GroupBy, resp.Groups)
		headers := make([]string, 0, len(dims)+8)
		for _, d := range dims {
			headers = append(headers, strings.ToUpper(d))
		}
		headers = append(headers, "CALLS", "ERRORS", "INPUT", "OUTPUT", "CACHE READ", "CACHE WRITE", "REASONING", "SUMMABLE")
		_, _ = fmt.Fprintln(gtw, strings.Join(headers, "\t"))

		for _, g := range resp.Groups {
			cells := make([]string, 0, len(dims)+8)
			for _, d := range dims {
				cells = append(cells, formatGroupDimCell(d, g, resp.Window.Granularity))
			}
			tcells := formatTokenCells(g.Tokens, &hasPartial, &hasSaturated)
			cells = append(cells,
				formatCommaInt(g.Calls),
				formatCommaInt(g.ErrorCalls),
				tcells.input, tcells.output, tcells.cacheRead, tcells.cacheWrite, tcells.reasoning, tcells.summable,
			)
			_, _ = fmt.Fprintln(gtw, strings.Join(cells, "\t"))
		}
		_ = gtw.Flush()
	}

	writeUsageFootnotes(out, hasPartial, hasSaturated, resp.Truncated)
}

func writeAgentUsageTable(out io.Writer, resp *hubclient.AgentUsageResponse) {
	if resp == nil {
		return
	}

	_, _ = fmt.Fprintf(out, "Scope:   agent (%s)\n", resp.AgentID)
	if resp.ProjectID != "" {
		_, _ = fmt.Fprintf(out, "Project: %s\n", resp.ProjectID)
	}
	_, _ = fmt.Fprintf(out, "Window:  %s\n", formatUsageWindow(resp.Window))
	_, _ = fmt.Fprintf(out, "Status:  %s\n", formatUsageStatusLine(resp.MeasurementStatus, 0, 0))
	_, _ = fmt.Fprintln(out)

	var hasPartial, hasSaturated bool
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "CALLS\tERRORS\tINPUT\tOUTPUT\tCACHE READ\tCACHE WRITE\tREASONING\tSUMMABLE")
	row := formatTokenCells(resp.Tokens, &hasPartial, &hasSaturated)
	_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
		formatCommaInt(resp.Calls),
		formatCommaInt(resp.ErrorCalls),
		row.input, row.output, row.cacheRead, row.cacheWrite, row.reasoning, row.summable,
	)
	_ = tw.Flush()

	if len(resp.ByModel) > 0 {
		_, _ = fmt.Fprintln(out)
		mtw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(mtw, "MODEL\tHARNESS\tCALLS\tERRORS\tINPUT\tOUTPUT\tCACHE READ\tCACHE WRITE\tREASONING\tSUMMABLE")
		for _, m := range resp.ByModel {
			modelName := m.Model
			if modelName == "" {
				modelName = usageUnknownCell
			}
			harnessName := m.Harness
			if harnessName == "" {
				harnessName = usageUnknownCell
			}
			tcells := formatTokenCells(m.Tokens, &hasPartial, &hasSaturated)
			_, _ = fmt.Fprintf(mtw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				modelName,
				harnessName,
				formatCommaInt(m.Calls),
				formatCommaInt(m.ErrorCalls),
				tcells.input, tcells.output, tcells.cacheRead, tcells.cacheWrite, tcells.reasoning, tcells.summable,
			)
		}
		_ = mtw.Flush()
	}

	writeUsageFootnotes(out, hasPartial, hasSaturated, false)
}

func formatUsageWindow(w hubclient.UsageWindow) string {
	loc := clitime.Zone()
	sinceStr := w.Since.In(loc).Format("2006-01-02 15:04 MST")
	untilStr := w.Until.In(loc).Format("2006-01-02 15:04 MST")
	if w.Granularity != "" {
		return fmt.Sprintf("%s .. %s (%s)", sinceStr, untilStr, w.Granularity)
	}
	return fmt.Sprintf("%s .. %s", sinceStr, untilStr)
}

func formatUsageStatusLine(status string, staleAgents, unmeasuredAgents int) string {
	if status == "" {
		status = usageUnknownCell
	}
	var notes []string
	if staleAgents > 0 {
		notes = append(notes, fmt.Sprintf("%d stale", staleAgents))
	}
	if unmeasuredAgents > 0 {
		notes = append(notes, fmt.Sprintf("%d unmeasured", unmeasuredAgents))
	}
	if len(notes) == 0 {
		return status
	}
	return fmt.Sprintf("%s (%s)", status, strings.Join(notes, ", "))
}

type formattedTokenCells struct {
	input      string
	output     string
	cacheRead  string
	cacheWrite string
	reasoning  string
	summable   string
}

func formatTokenCells(t hubclient.UsageTokens, hasPartial, hasSaturated *bool) formattedTokenCells {
	var inUnk, outUnk, crUnk, cwUnk, reasUnk int64
	if t.UnknownEvents != nil {
		inUnk = t.UnknownEvents.Input
		outUnk = t.UnknownEvents.Output
		crUnk = t.UnknownEvents.CacheRead
		cwUnk = t.UnknownEvents.CacheWrite
		reasUnk = t.UnknownEvents.Reasoning
	}
	summablePartial := inUnk > 0 || outUnk > 0 || crUnk > 0 || cwUnk > 0

	formatBucket := func(val *int64, unknown int64, saturated bool) string {
		if val == nil {
			return usageUnknownCell
		}
		s := formatCommaInt(*val)
		if unknown > 0 {
			s += "*"
			*hasPartial = true
		}
		if saturated || *val == math.MaxInt64 {
			s += "+"
			*hasSaturated = true
		}
		return s
	}

	var sumUnk int64
	if summablePartial {
		sumUnk = 1
	}

	return formattedTokenCells{
		input:      formatBucket(t.Input, inUnk, false),
		output:     formatBucket(t.Output, outUnk, false),
		cacheRead:  formatBucket(t.CacheRead, crUnk, false),
		cacheWrite: formatBucket(t.CacheWrite, cwUnk, false),
		reasoning:  formatBucket(t.Reasoning, reasUnk, false),
		summable:   formatBucket(t.SummableTokens, sumUnk, t.Saturated),
	}
}

func resolveGroupColumns(groupBy []string, groups []hubclient.UsageGroupRow) []string {
	if len(groupBy) > 0 {
		return groupBy
	}
	// Fallback if a server omits group_by: infer from first group row.
	if len(groups) == 0 {
		return nil
	}
	g := groups[0]
	var dims []string
	if g.BucketStart != nil {
		dims = append(dims, "day")
	}
	if g.ProjectID != nil {
		dims = append(dims, "project")
	}
	if g.UserID != nil {
		dims = append(dims, "user")
	}
	if g.AgentID != nil {
		dims = append(dims, "agent")
	}
	if g.Harness != nil {
		dims = append(dims, "harness")
	}
	if g.Model != nil {
		dims = append(dims, "model")
	}
	return dims
}

func formatGroupDimCell(dim string, g hubclient.UsageGroupRow, granularity string) string {
	switch dim {
	case "day":
		if g.BucketStart == nil {
			return usageUnknownCell
		}
		return g.BucketStart.UTC().Format("2006-01-02")
	case "hour":
		if g.BucketStart == nil {
			return usageUnknownCell
		}
		if granularity == "day" {
			return g.BucketStart.UTC().Format("2006-01-02")
		}
		return g.BucketStart.In(clitime.Zone()).Format("2006-01-02 15:04 MST")
	case "project":
		return formatOptionalDim(g.ProjectID)
	case "user":
		return formatOptionalDim(g.UserID)
	case "agent":
		return formatOptionalDim(g.AgentID)
	case "harness":
		return formatOptionalDim(g.Harness)
	case "model":
		return formatOptionalDim(g.Model)
	default:
		return usageUnknownCell
	}
}

func formatOptionalDim(p *string) string {
	if p == nil || *p == "" {
		return usageUnknownCell
	}
	return *p
}

func writeUsageFootnotes(out io.Writer, hasPartial, hasSaturated, truncated bool) {
	if !hasPartial && !hasSaturated && !truncated {
		return
	}
	_, _ = fmt.Fprintln(out)
	if hasPartial {
		_, _ = fmt.Fprintln(out, "* partial: some events in this bucket had unknown token counts")
	}
	if hasSaturated {
		_, _ = fmt.Fprintln(out, "+ saturated: value reached the int64 saturation ceiling")
	}
	if truncated {
		_, _ = fmt.Fprintln(out, "Note: grouped results were truncated; pass --limit to show more rows (totals above are complete).")
	}
}

func formatCommaInt(n int64) string {
	if n < 0 {
		if n == math.MinInt64 {
			return "-9,223,372,036,854,775,808"
		}
		return "-" + formatCommaInt(-n)
	}
	s := strconv.FormatInt(n, 10)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	rem := len(s) % 3
	if rem > 0 {
		b.WriteString(s[:rem])
	}
	for i := rem; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}
