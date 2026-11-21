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
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

// Human-mode scion decisions list (design C1, section 5.4).
const (
	defaultDecisionsListLimit = 50
	maxDecisionsListLimit     = 200
	minDecisionsSearchRunes   = 2
	maxDecisionsSearchRunes   = 256
	maxDecisionsFacetOptions  = 500

	errMsgDecisionSearchUnsupported = "this hub does not support decision search"
)

// decisionsHumanTarget returns the hub client and the ID of the project the
// human-mode list reads.
var decisionsHumanTarget = func() (hubclient.Client, string, error) {
	settings, client, err := requireHubClient()
	if err != nil {
		return nil, "", err
	}
	projectID, err := resolveProjectID(settings, "")
	if err != nil {
		return nil, "", err
	}
	return client, projectID, nil
}

// decisionsAllProjectsClient returns the hub client for --all-projects,
// ignoring projectPath so --project names a Hub project filter rather than a
// local directory.
var decisionsAllProjectsClient = func() (hubclient.Client, error) {
	// --project is deliberately not a directory here (matching root.go's hubOnly skip).
	resolvedPath, _, err := config.ResolveProjectPath("")
	if err != nil {
		return nil, fmt.Errorf("failed to resolve settings path: %w", err)
	}
	settings, err := config.LoadSettings(resolvedPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load settings: %w", err)
	}
	if !hubConfigured() {
		return nil, errHubNotConfigured
	}
	return getHubClient(settings)
}

// decisionsAmbiguousError is a --conversation or --agent name that matches
// more than one facet option. It exits with status 2.
type decisionsAmbiguousError struct{ msg string }

func (e *decisionsAmbiguousError) Error() string { return e.msg }
func (e *decisionsAmbiguousError) ExitCode() int { return 2 }

// humanDecisionsListOptions validates the human-mode flags into list
// options. --conversation, --agent and --answered-by me are resolved later.
func humanDecisionsListOptions(cmd *cobra.Command) (hubclient.ListProjectDecisionsOptions, error) {
	flags := cmd.Flags()
	waiting, _ := flags.GetBool("waiting")
	made, _ := flags.GetBool("made")
	if waiting && made {
		return hubclient.ListProjectDecisionsOptions{}, newUsageError("pass at most one of --waiting or --made")
	}
	opts := hubclient.ListProjectDecisionsOptions{Tab: hubclient.ProjectDecisionsTabPending}
	if made {
		opts.Tab = hubclient.ProjectDecisionsTabMade
	}

	limit, _ := flags.GetInt("limit")
	if limit < 1 || limit > maxDecisionsListLimit {
		return opts, newUsageError("--limit must be between 1 and %d", maxDecisionsListLimit)
	}
	opts.Limit = limit

	search, _ := flags.GetString("search")
	if search = strings.TrimSpace(search); search != "" {
		if n := utf8.RuneCountInString(search); n < minDecisionsSearchRunes || n > maxDecisionsSearchRunes {
			return opts, newUsageError("--search must be %d to %d characters", minDecisionsSearchRunes, maxDecisionsSearchRunes)
		}
		opts.Query = search
	}

	for _, name := range []string{"answered-by", "provenance", "since", "until"} {
		if flags.Changed(name) && !made {
			return opts, newUsageError("--%s needs --made", name)
		}
	}
	opts.UserID, _ = flags.GetString("answered-by")
	opts.UserID = strings.TrimSpace(opts.UserID)
	provenance, _ := flags.GetString("provenance")
	switch provenance = strings.TrimSpace(provenance); provenance {
	case "", "direct", "interpreted", "confirmed", "overridden", "dismissed":
		opts.Provenance = provenance
	default:
		return opts, newUsageError("--provenance must be one of direct, interpreted, confirmed, overridden, dismissed")
	}
	for flag, dst := range map[string]*string{"since": &opts.From, "until": &opts.To} {
		v, _ := flags.GetString(flag)
		v = strings.TrimSpace(v)
		if v != "" && !validDecisionDateBound(v) {
			return opts, newUsageError("--%s must be YYYY-MM-DD or RFC3339", flag)
		}
		*dst = v
	}
	return opts, nil
}

// validDecisionDateBound reports whether v is a UTC day (YYYY-MM-DD) or an
// RFC3339 time, the forms the hub accepts for from and to.
func validDecisionDateBound(v string) bool {
	if _, err := time.Parse("2006-01-02", v); err == nil {
		return true
	}
	_, err := time.Parse(time.RFC3339Nano, v)
	return err == nil
}

func runHumanDecisionsList(cmd *cobra.Command) error {
	allProjects, _ := cmd.Flags().GetBool("all-projects")
	sortMode, _ := cmd.Flags().GetString("sort")
	sortMode = strings.TrimSpace(sortMode)
	if cmd.Flags().Changed("sort") && !allProjects {
		return newUsageError("--sort needs --all-projects")
	}
	switch sortMode {
	case "", hubclient.DecisionsSortProject, hubclient.DecisionsSortRecent:
	default:
		return newUsageError("--sort must be %s or %s", hubclient.DecisionsSortProject, hubclient.DecisionsSortRecent)
	}

	opts, err := humanDecisionsListOptions(cmd)
	if err != nil {
		return err
	}
	if allProjects {
		return runHumanAllProjectsDecisionsList(cmd, opts, sortMode)
	}
	conversation, _ := cmd.Flags().GetString("conversation")
	conversation = strings.TrimSpace(conversation)
	agent, _ := cmd.Flags().GetString("agent")
	agent = strings.TrimSpace(agent)

	client, projectID, err := decisionsHumanTarget()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()
	svc := client.ProjectDecisions(projectID)

	// Names resolve against the facets, which cover the whole scope.
	if conversation != "" || agent != "" {
		facets, err := svc.List(ctx, &hubclient.ListProjectDecisionsOptions{Tab: opts.Tab, Limit: 1})
		if err != nil {
			return formatProjectDecisionsError(err)
		}
		if !facets.SupportsSearch() {
			return errors.New(errMsgDecisionSearchUnsupported)
		}
		if conversation != "" {
			if opts.Conversation, err = resolveDecisionConversation(conversation, facets.Conversations, decisionFacetScope{}); err != nil {
				return err
			}
		}
		if agent != "" {
			if opts.AgentID, err = resolveDecisionAgent(agent, facets.Agents, decisionFacetScope{}); err != nil {
				return err
			}
		}
	}

	wantJSON := decisionsWantJSON(cmd)
	meID, resolvedUserID, err := resolveDecisionsViewer(ctx, client, opts.UserID, opts.Tab == hubclient.ProjectDecisionsTabMade && !wantJSON)
	if err != nil {
		return err
	}
	opts.UserID = resolvedUserID

	resp, err := svc.List(ctx, &opts)
	if err != nil {
		return formatProjectDecisionsError(err)
	}
	if opts.Query != "" && !resp.SupportsSearch() {
		return errors.New(errMsgDecisionSearchUnsupported)
	}

	if wantJSON {
		return writeJSONTo(cmd.OutOrStdout(), resp)
	}
	filtered := opts.Query != "" || opts.Conversation != "" || opts.AgentID != ""
	if opts.Tab == hubclient.ProjectDecisionsTabMade {
		filtered = filtered || opts.UserID != "" || opts.Provenance != "" || opts.From != "" || opts.To != ""
		writeMadeDecisionsTable(cmd.OutOrStdout(), resp, meID, filtered)
		return nil
	}
	writeWaitingDecisionsTable(cmd.OutOrStdout(), resp, filtered)
	return nil
}

// formatProjectDecisionsError maps a project decisions API error: a 404
// from the experiment gate names the experiment, a missing project says so.
func formatProjectDecisionsError(err error) error {
	var apiErr *apiclient.APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
		if strings.HasPrefix(apiErr.Message, "Project") {
			return errors.New("project not found on this hub")
		}
		return errors.New(errMsgDecisionsExperimentRequired)
	}
	return formatDecisionAPIError(err)
}

// normDecisionName folds a conversation or agent name for matching: case is
// ignored and a leading # is optional.
func normDecisionName(s string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(s), "#"))
}

type decisionFacetScope struct {
	projectID     string
	projectLabels map[string]string
	truncated     bool
}

// resolveDecisionConversation maps --conversation to a chat key: a facet
// key as is, else the one facet whose label matches. A value that matches
// no label but has the shape of a chat key (a topic UUID or a dm: key) is
// passed through, so the hub answers it (empty when not readable). When
// scope.projectLabels is non-nil (--all-projects without --project), ambiguity
// lines name each match's project and suggest --project. When scope.truncated
// is true (the whole-scope facet hit the 500-entry cap), a name miss or an
// unnarrowed single match exits 2 requiring a key rather than guessing.
func resolveDecisionConversation(value string, convs []hubclient.ProjectDecisionConversation, scope decisionFacetScope) (string, error) {
	for _, c := range convs {
		if c.Key == value {
			return c.Key, nil
		}
	}
	var matches []hubclient.ProjectDecisionConversation
	for _, c := range convs {
		if normDecisionName(c.Label) == normDecisionName(value) {
			matches = append(matches, c)
		}
	}
	switch len(matches) {
	case 1:
		m := matches[0]
		if scope.truncated && (scope.projectID == "" || m.ProjectID != scope.projectID) {
			line := fmt.Sprintf("  %s  %s", safeText(m.Label), safeText(m.Key))
			if proj := lookupDecisionProjectLabel(scope.projectLabels, m.ProjectID); proj != "" {
				line = fmt.Sprintf("  %s  %s  %s", proj, safeText(m.Label), safeText(m.Key))
			}
			return "", &decisionsAmbiguousError{msg: fmt.Sprintf(
				"facet list truncated (%d conversations): --conversation %q may also match another project; pass the conversation key:\n%s",
				maxDecisionsFacetOptions, safeText(value), line)}
		}
		return m.Key, nil
	case 0:
		if _, err := uuid.Parse(value); err == nil || strings.HasPrefix(value, "dm:") {
			return value, nil
		}
		if scope.truncated {
			return "", &decisionsAmbiguousError{msg: fmt.Sprintf(
				"facet list truncated (%d conversations): pass the conversation key for %q",
				maxDecisionsFacetOptions, safeText(value))}
		}
		return "", fmt.Errorf("no conversation with Decisions matches %q", safeText(value))
	}
	lines := make([]string, 0, len(matches))
	for _, m := range matches {
		if proj := lookupDecisionProjectLabel(scope.projectLabels, m.ProjectID); proj != "" {
			lines = append(lines, fmt.Sprintf("  %s  %s  %s", proj, safeText(m.Label), safeText(m.Key)))
		} else {
			lines = append(lines, fmt.Sprintf("  %s  %s", safeText(m.Label), safeText(m.Key)))
		}
	}
	hint := "pass a key"
	if scope.projectLabels != nil {
		hint = "pass a key or --project <name>"
	}
	return "", &decisionsAmbiguousError{msg: fmt.Sprintf("--conversation %q matches %d conversations; %s:\n%s",
		safeText(value), len(matches), hint, strings.Join(lines, "\n"))}
}

// resolveDecisionAgent maps --agent to an agent ID: a facet ID as is, else
// the one facet whose name matches. An unmatched UUID is passed through.
// When scope.projectLabels is non-nil (--all-projects without --project),
// ambiguity lines name each match's project and suggest --project. When
// scope.truncated is true (the whole-scope facet hit the 500-entry cap), a
// name miss or an unnarrowed single match exits 2 requiring an agent ID.
func resolveDecisionAgent(value string, agents []hubclient.ProjectDecisionActor, scope decisionFacetScope) (string, error) {
	for _, a := range agents {
		if a.ID == value {
			return a.ID, nil
		}
	}
	var matches []hubclient.ProjectDecisionActor
	for _, a := range agents {
		if normDecisionName(a.Name) == normDecisionName(value) {
			matches = append(matches, a)
		}
	}
	switch len(matches) {
	case 1:
		m := matches[0]
		if scope.truncated && (scope.projectID == "" || m.ProjectID != scope.projectID) {
			name := safeText(m.Name)
			if m.Hint != "" {
				name += " (" + safeText(m.Hint) + ")"
			}
			line := fmt.Sprintf("  %s  %s", name, safeText(m.ID))
			if proj := lookupDecisionProjectLabel(scope.projectLabels, m.ProjectID); proj != "" {
				line = fmt.Sprintf("  %s  %s  %s", proj, name, safeText(m.ID))
			}
			return "", &decisionsAmbiguousError{msg: fmt.Sprintf(
				"facet list truncated (%d agents): --agent %q may also match another project; pass the agent ID:\n%s",
				maxDecisionsFacetOptions, safeText(value), line)}
		}
		return m.ID, nil
	case 0:
		if _, err := uuid.Parse(value); err == nil {
			return value, nil
		}
		if scope.truncated {
			return "", &decisionsAmbiguousError{msg: fmt.Sprintf(
				"facet list truncated (%d agents): pass the agent ID for %q",
				maxDecisionsFacetOptions, safeText(value))}
		}
		return "", fmt.Errorf("no agent that asked Decisions matches %q", safeText(value))
	}
	lines := make([]string, 0, len(matches))
	for _, m := range matches {
		name := safeText(m.Name)
		if m.Hint != "" {
			name += " (" + safeText(m.Hint) + ")"
		}
		if proj := lookupDecisionProjectLabel(scope.projectLabels, m.ProjectID); proj != "" {
			lines = append(lines, fmt.Sprintf("  %s  %s  %s", proj, name, safeText(m.ID)))
		} else {
			lines = append(lines, fmt.Sprintf("  %s  %s", name, safeText(m.ID)))
		}
	}
	hint := "pass an ID"
	if scope.projectLabels != nil {
		hint = "pass an ID or --project <name>"
	}
	return "", &decisionsAmbiguousError{msg: fmt.Sprintf("--agent %q matches %d agents; %s:\n%s",
		safeText(value), len(matches), hint, strings.Join(lines, "\n"))}
}

// decisionRowWaitingOnYou reports whether row d of prompt p is open and
// waiting on the viewer: pending, or decided by an unconfirmed interpreted
// answer, with no waiting-on-others reason.
func decisionRowWaitingOnYou(p hubclient.ProjectDecisionPrompt, d hubclient.DecisionView) bool {
	open := d.Status == "pending" || (d.Status == "decided" && d.Decision != nil && d.Decision.Provenance == "agent_interpreted")
	return open && p.WaitingReasons[d.RowID] == ""
}

func writeWaitingDecisionsTable(out io.Writer, resp *hubclient.ProjectDecisionsResponse, filtered bool) {
	counts := resp.Counts
	if resp.FilteredCounts != nil {
		counts = *resp.FilteredCounts
	}
	var rows [][]string
	for _, p := range resp.Prompts {
		if !p.WaitingOnYou {
			continue
		}
		for _, d := range p.Decisions {
			if !decisionRowWaitingOnYou(p, d) {
				continue
			}
			rows = append(rows, []string{
				safeText(d.Key),
				fmt.Sprintf("v%d", d.Version),
				truncateRunes(collapseSingleLine(d.Question), decisionQuestionMaxWidth, true),
				orDash(safeText(p.AskerAgentName)),
				orDash(safeText(p.ConversationLabel)),
				clitime.Ago(p.CreatedAt),
			})
		}
	}
	if len(rows) == 0 {
		if filtered {
			_, _ = fmt.Fprintln(out, "No Decisions waiting on you match.")
		} else {
			_, _ = fmt.Fprintln(out, "No Decisions waiting on you.")
		}
	} else {
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "KEY\tVER\tQUESTION\tAGENT\tCONVERSATION\tAGE")
		for _, r := range rows {
			_, _ = fmt.Fprintln(tw, strings.Join(r, "\t"))
		}
		_ = tw.Flush()
	}
	// Pages list waiting-on-you prompts before waiting-on-others ones, so
	// a next page holds more of yours only while rows shown fall short of
	// the waiting-on-you count.
	if resp.NextCursor != "" && len(rows) < counts.WaitingOnYou {
		_, _ = fmt.Fprintf(out, "More Decisions are waiting on you; raise --limit (up to %d) to list them.\n", maxDecisionsListLimit)
	}
	if counts.WaitingOnOthers > 0 {
		_, _ = fmt.Fprintf(out, "%d more waiting on others (use --json for details)\n", counts.WaitingOnOthers)
	}
}

func writeMadeDecisionsTable(out io.Writer, resp *hubclient.ProjectDecisionsResponse, meID string, filtered bool) {
	if len(resp.Entries) == 0 {
		if filtered {
			_, _ = fmt.Fprintln(out, "No Decisions made match.")
		} else {
			_, _ = fmt.Fprintln(out, "No Decisions made.")
		}
		return
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "WHEN\tKEY\tVER\tQUESTION\tANSWER\tANSWERED BY\tPROVENANCE\tAGENT\tCONVERSATION")
	for _, e := range resp.Entries {
		answeredBy := orDash(safeText(e.AnsweredByName))
		if meID != "" && e.AnsweredByUserID == meID {
			answeredBy = "you"
		}
		_, _ = fmt.Fprintln(tw, strings.Join([]string{
			clitime.Format(e.At, clitime.Date),
			safeText(e.Decision.Key),
			fmt.Sprintf("v%d", e.Decision.Version),
			truncateRunes(collapseSingleLine(e.Decision.Question), decisionQuestionMaxWidth, true),
			orDash(formatDecisionListAnswer(e.ChoiceID, e.Label, e.OtherText)),
			answeredBy,
			decisionProvenanceDisplay(&hubclient.DecisionAnswerView{Provenance: e.Provenance, Outcome: e.Outcome}),
			orDash(safeText(e.Decision.AskerAgentName)),
			orDash(safeText(e.ConversationLabel)),
		}, "\t"))
	}
	_ = tw.Flush()
	if resp.NextCursor != "" {
		_, _ = fmt.Fprintf(out, "More answers; raise --limit (up to %d) to list them.\n", maxDecisionsListLimit)
	}
}

func resolveDecisionsViewer(ctx context.Context, client hubclient.Client, userID string, wantYouLabel bool) (string, string, error) {
	isMeFilter := strings.EqualFold(userID, "me")
	if !isMeFilter && !wantYouLabel {
		return "", userID, nil
	}
	var meID string
	me, err := client.Auth().Me(ctx)
	if err == nil && me != nil {
		meID = me.ID
	}
	if isMeFilter {
		// An empty ID would silently drop the filter and list everyone's answers.
		switch {
		case err != nil:
			return "", "", fmt.Errorf("resolve --answered-by me: %w", err)
		case meID == "":
			return "", "", errors.New("resolve --answered-by me: the hub returned an empty identity")
		}
		userID = meID
	}
	return meID, userID, nil
}

func allProjectsFlagProjectFilter(cmd *cobra.Command) string {
	pf := cmd.Flags().Lookup("project")
	if pf == nil {
		pf = cmd.InheritedFlags().Lookup("project")
	}
	if pf == nil || !pf.Changed {
		return ""
	}
	return strings.TrimSpace(pf.Value.String())
}

func runHumanAllProjectsDecisionsList(cmd *cobra.Command, base hubclient.ListProjectDecisionsOptions, sortMode string) error {
	conversation, _ := cmd.Flags().GetString("conversation")
	conversation = strings.TrimSpace(conversation)
	agent, _ := cmd.Flags().GetString("agent")
	agent = strings.TrimSpace(agent)
	projectFilter := allProjectsFlagProjectFilter(cmd)

	client, err := decisionsAllProjectsClient()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()
	svc := client.MyDecisions()

	myOpts := hubclient.ListMyDecisionsOptions{
		Tab:        base.Tab,
		Sort:       sortMode,
		Limit:      base.Limit,
		Query:      base.Query,
		UserID:     base.UserID,
		Provenance: base.Provenance,
		From:       base.From,
		To:         base.To,
	}

	if projectFilter != "" || conversation != "" || agent != "" {
		facets, err := svc.List(ctx, &hubclient.ListMyDecisionsOptions{Tab: base.Tab, Limit: 1})
		if err != nil {
			return formatProjectDecisionsError(err)
		}
		if !facets.SupportsSearch() {
			return errors.New(errMsgDecisionSearchUnsupported)
		}
		var projectLabels map[string]string
		if projectFilter != "" {
			var inFacet bool
			if myOpts.ProjectID, inFacet, err = resolveDecisionProject(ctx, client, projectFilter, facets.Projects); err != nil {
				return err
			}
			// When --project resolved a hub project outside the top-200 facet
			// and --conversation or --agent also needs facet resolution, re-fetch
			// facets scoped to ProjectID so the hub swaps that project in.
			if !inFacet && (conversation != "" || agent != "") {
				facets, err = svc.List(ctx, &hubclient.ListMyDecisionsOptions{
					Tab:       base.Tab,
					Limit:     1,
					ProjectID: myOpts.ProjectID,
				})
				if err != nil {
					return formatProjectDecisionsError(err)
				}
			}
		} else {
			projectLabels = decisionProjectLabels(facets.Projects)
		}
		convScope := decisionFacetScope{
			projectID:     myOpts.ProjectID,
			projectLabels: projectLabels,
			truncated:     len(facets.Conversations) >= maxDecisionsFacetOptions,
		}
		agentScope := decisionFacetScope{
			projectID:     myOpts.ProjectID,
			projectLabels: projectLabels,
			truncated:     len(facets.Agents) >= maxDecisionsFacetOptions,
		}
		convs := facets.Conversations
		agents := facets.Agents
		if myOpts.ProjectID != "" {
			// Profile facets cover the whole scope (design D5); narrow options to
			// the selected project so shared names like #general or coordinator
			// resolve within --project rather than colliding across projects.
			convs = filterConversationsByProject(facets.Conversations, myOpts.ProjectID)
			agents = filterAgentsByProject(facets.Agents, myOpts.ProjectID)
		}
		if conversation != "" {
			if myOpts.Conversation, err = resolveDecisionConversation(conversation, convs, convScope); err != nil {
				return err
			}
		}
		if agent != "" {
			if myOpts.AgentID, err = resolveDecisionAgent(agent, agents, agentScope); err != nil {
				return err
			}
		}
	}

	wantJSON := decisionsWantJSON(cmd)
	meID, resolvedUserID, err := resolveDecisionsViewer(ctx, client, myOpts.UserID, myOpts.Tab == hubclient.ProjectDecisionsTabMade && !wantJSON)
	if err != nil {
		return err
	}
	myOpts.UserID = resolvedUserID

	resp, err := svc.List(ctx, &myOpts)
	if err != nil {
		return formatProjectDecisionsError(err)
	}
	if myOpts.Query != "" && !resp.SupportsSearch() {
		return errors.New(errMsgDecisionSearchUnsupported)
	}

	if wantJSON {
		return writeJSONTo(cmd.OutOrStdout(), resp)
	}
	filtered := myOpts.ProjectID != "" || myOpts.Query != "" || myOpts.Conversation != "" || myOpts.AgentID != ""
	if myOpts.Tab == hubclient.ProjectDecisionsTabMade {
		filtered = filtered || myOpts.UserID != "" || myOpts.Provenance != "" || myOpts.From != "" || myOpts.To != ""
		writeAllProjectsMadeDecisionsTable(cmd.OutOrStdout(), resp, meID, filtered)
		return nil
	}
	writeAllProjectsWaitingDecisionsTable(cmd.OutOrStdout(), resp, filtered)
	return nil
}

// resolveDecisionProject maps --project (with --all-projects) to a project ID
// and reports whether the match came from the top-200 facet: a facet ID as is,
// else the one facet whose name or slug matches case-insensitively. When no
// facet matches (for example a member project past the 200-project cap), an
// unmatched UUID is passed through and a name or slug is looked up via the
// hub's project list so "pass --project <name> to see others" works past 200.
func resolveDecisionProject(
	ctx context.Context,
	client hubclient.Client,
	value string,
	projects []hubclient.ProjectDecisionProjectRef,
) (string, bool, error) {
	for _, p := range projects {
		if p.ID == value {
			return p.ID, true, nil
		}
	}
	norm := normDecisionName(value)
	seen := make(map[string]bool)
	var matches []hubclient.ProjectDecisionProjectRef
	for _, p := range projects {
		if seen[p.ID] {
			continue
		}
		if (p.Name != "" && normDecisionName(p.Name) == norm) || (p.Slug != "" && normDecisionName(p.Slug) == norm) {
			seen[p.ID] = true
			matches = append(matches, p)
		}
	}
	inFacet := len(matches) > 0
	if !inFacet {
		if _, err := uuid.Parse(value); err == nil {
			return value, false, nil
		}
		hubMatches, err := lookupHubDecisionProjects(ctx, client, value)
		if err != nil {
			return "", false, err
		}
		matches = hubMatches
	}
	switch len(matches) {
	case 1:
		return matches[0].ID, inFacet, nil
	case 0:
		return "", false, fmt.Errorf("no project with Decisions matches %q", safeText(value))
	}
	lines := make([]string, 0, len(matches))
	for _, m := range matches {
		label := safeText(m.Name)
		if m.Slug != "" && !strings.EqualFold(m.Name, m.Slug) {
			label += " (" + safeText(m.Slug) + ")"
		}
		if label == "" {
			label = safeText(m.Slug)
		}
		lines = append(lines, fmt.Sprintf("  %s  %s", label, safeText(m.ID)))
	}
	return "", false, &decisionsAmbiguousError{msg: fmt.Sprintf("--project %q matches %d projects; pass an ID:\n%s",
		safeText(value), len(matches), strings.Join(lines, "\n"))}
}

func lookupHubDecisionProjects(ctx context.Context, client hubclient.Client, value string) ([]hubclient.ProjectDecisionProjectRef, error) {
	clean := strings.TrimPrefix(strings.TrimSpace(value), "#")
	if clean == "" || client == nil {
		return nil, nil
	}
	norm := normDecisionName(clean)
	seen := make(map[string]bool)
	var matches []hubclient.ProjectDecisionProjectRef
	for _, opts := range []*hubclient.ListProjectsOptions{
		{Slug: clean},
		{Name: clean},
	} {
		resp, err := client.Projects().List(ctx, opts)
		if err != nil {
			var apiErr *apiclient.APIError
			if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusForbidden || apiErr.StatusCode == http.StatusNotFound) {
				// A scoped credential without project:list cannot browse /api/v1/projects;
				// fall back to no match rather than failing on the secondary lookup.
				continue
			}
			return nil, formatDecisionAPIError(err)
		}
		for _, p := range resp.Projects {
			if seen[p.ID] {
				continue
			}
			if (p.Name != "" && normDecisionName(p.Name) == norm) || (p.Slug != "" && normDecisionName(p.Slug) == norm) {
				seen[p.ID] = true
				matches = append(matches, hubclient.ProjectDecisionProjectRef{
					ID:   p.ID,
					Name: p.Name,
					Slug: p.Slug,
				})
			}
		}
	}
	return matches, nil
}

func decisionProjectDisplay(ref hubclient.ProjectDecisionProjectRef) string {
	if ref.Slug != "" {
		return safeText(ref.Slug)
	}
	if ref.Name != "" {
		return safeText(ref.Name)
	}
	if ref.ID != "" {
		return safeText(ref.ID)
	}
	return "-"
}

func decisionProjectLabels(projects []hubclient.ProjectDecisionProjectRef) map[string]string {
	out := make(map[string]string, len(projects))
	for _, p := range projects {
		if p.ID == "" {
			continue
		}
		label := decisionProjectDisplay(p)
		if p.Name != "" && p.Slug != "" && !strings.EqualFold(p.Name, p.Slug) {
			label = safeText(p.Name) + " (" + safeText(p.Slug) + ")"
		}
		out[p.ID] = label
	}
	return out
}

func lookupDecisionProjectLabel(projectLabels map[string]string, projectID string) string {
	if projectLabels == nil {
		return ""
	}
	if label := projectLabels[projectID]; label != "" {
		return label
	}
	return safeText(projectID)
}

func filterConversationsByProject(convs []hubclient.ProjectDecisionConversation, projectID string) []hubclient.ProjectDecisionConversation {
	out := make([]hubclient.ProjectDecisionConversation, 0, len(convs))
	for _, c := range convs {
		if c.ProjectID == projectID {
			out = append(out, c)
		}
	}
	return out
}

func filterAgentsByProject(agents []hubclient.ProjectDecisionActor, projectID string) []hubclient.ProjectDecisionActor {
	out := make([]hubclient.ProjectDecisionActor, 0, len(agents))
	for _, a := range agents {
		if a.ProjectID == projectID {
			out = append(out, a)
		}
	}
	return out
}

func writeAllProjectsWaitingDecisionsTable(out io.Writer, resp *hubclient.MyDecisionsResponse, filtered bool) {
	counts := resp.Counts
	if resp.FilteredCounts != nil {
		counts = *resp.FilteredCounts
	}
	var rows [][]string
	for _, p := range resp.Prompts {
		if !p.WaitingOnYou {
			continue
		}
		projLabel := decisionProjectDisplay(p.Project)
		for _, d := range p.Decisions {
			if !decisionRowWaitingOnYou(p, d) {
				continue
			}
			rows = append(rows, []string{
				projLabel,
				safeText(d.Key),
				fmt.Sprintf("v%d", d.Version),
				truncateRunes(collapseSingleLine(d.Question), decisionQuestionMaxWidth, true),
				orDash(safeText(p.AskerAgentName)),
				orDash(safeText(p.ConversationLabel)),
				clitime.Ago(p.CreatedAt),
			})
		}
	}
	if len(rows) == 0 {
		if filtered {
			_, _ = fmt.Fprintln(out, "No Decisions waiting on you match.")
		} else {
			_, _ = fmt.Fprintln(out, "No Decisions waiting on you.")
		}
	} else {
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "PROJECT\tKEY\tVER\tQUESTION\tAGENT\tCONVERSATION\tAGE")
		for _, r := range rows {
			_, _ = fmt.Fprintln(tw, strings.Join(r, "\t"))
		}
		_ = tw.Flush()
	}
	if resp.NextCursor != "" && len(rows) < counts.WaitingOnYou {
		_, _ = fmt.Fprintf(out, "More Decisions are waiting on you; raise --limit (up to %d) to list them.\n", maxDecisionsListLimit)
	}
	if counts.WaitingOnOthers > 0 {
		_, _ = fmt.Fprintf(out, "%d more waiting on others (use --json for details)\n", counts.WaitingOnOthers)
	}
	if resp.TruncatedProjects {
		_, _ = fmt.Fprintln(out, "Showing your 200 most recently active projects; pass --project <name> to see others.")
	}
}

func writeAllProjectsMadeDecisionsTable(out io.Writer, resp *hubclient.MyDecisionsResponse, meID string, filtered bool) {
	entries := resp.Entries
	anyGroupMore := false
	if len(entries) == 0 && len(resp.Groups) > 0 {
		for _, g := range resp.Groups {
			entries = append(entries, g.Entries...)
			if g.More {
				anyGroupMore = true
			}
		}
	}
	if len(entries) == 0 {
		if filtered {
			_, _ = fmt.Fprintln(out, "No Decisions made match.")
		} else {
			_, _ = fmt.Fprintln(out, "No Decisions made.")
		}
		if resp.TruncatedProjects {
			_, _ = fmt.Fprintln(out, "Showing your 200 most recently active projects; pass --project <name> to see others.")
		}
		return
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "PROJECT\tWHEN\tKEY\tVER\tQUESTION\tANSWER\tANSWERED BY\tPROVENANCE\tAGENT\tCONVERSATION")
	for _, e := range entries {
		answeredBy := orDash(safeText(e.AnsweredByName))
		if meID != "" && e.AnsweredByUserID == meID {
			answeredBy = "you"
		}
		_, _ = fmt.Fprintln(tw, strings.Join([]string{
			decisionProjectDisplay(e.Project),
			clitime.Format(e.At, clitime.Date),
			safeText(e.Decision.Key),
			fmt.Sprintf("v%d", e.Decision.Version),
			truncateRunes(collapseSingleLine(e.Decision.Question), decisionQuestionMaxWidth, true),
			orDash(formatDecisionListAnswer(e.ChoiceID, e.Label, e.OtherText)),
			answeredBy,
			decisionProvenanceDisplay(&hubclient.DecisionAnswerView{Provenance: e.Provenance, Outcome: e.Outcome}),
			orDash(safeText(e.Decision.AskerAgentName)),
			orDash(safeText(e.ConversationLabel)),
		}, "\t"))
	}
	_ = tw.Flush()
	if resp.NextCursor != "" {
		_, _ = fmt.Fprintf(out, "More answers; raise --limit (up to %d) to list them.\n", maxDecisionsListLimit)
	}
	if anyGroupMore {
		_, _ = fmt.Fprintln(out, "Some projects have more than 5 answers; pass --sort recent or --project <name> to list them.")
	}
	if resp.TruncatedProjects {
		_, _ = fmt.Fprintln(out, "Showing your 200 most recently active projects; pass --project <name> to see others.")
	}
}
