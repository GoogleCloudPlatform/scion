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
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

const (
	errMsgInboxExperimentRequired = "Inbox is not enabled on this hub (experiment web.inbox_dashboard)"
	errHintInboxForbiddenScope    = "a user access token needs the inbox:read scope; agents, visitors and federated users cannot read the inbox"
	inboxPreviewQuestionMaxRunes  = 100
)

const (
	// inboxMergedSecretsKey is the synthetic display key for the merged
	// Secrets row (combining agent_secrets and pending_secrets).
	inboxMergedSecretsKey = "secrets"
)

// inboxCategoryOrder is the canonical display order for inbox categories
// (design §6.3, §9, amended 2026-11-26 for the single merged Secrets row).
var inboxCategoryOrder = []string{
	hubclient.InboxCategoryDecisions,
	inboxMergedSecretsKey,
	hubclient.InboxCategoryWaiting,
	hubclient.InboxCategoryReviews,
	hubclient.InboxCategoryChat,
}

// inboxClientTarget resolves the Hub client and Hub base URL for `scion inbox`.
// Overridden in tests.
var inboxClientTarget = func() (hubclient.Client, string, error) {
	resolvedPath, _, err := config.ResolveProjectPath("")
	if err != nil {
		return nil, "", fmt.Errorf("failed to resolve settings path: %w", err)
	}
	settings, err := config.LoadSettings(resolvedPath)
	if err != nil {
		return nil, "", fmt.Errorf("failed to load settings: %w", err)
	}
	if !hubConfigured() {
		return nil, "", errHubNotConfigured
	}
	client, err := getHubClient(settings)
	if err != nil {
		return nil, "", err
	}
	return client, invocationHubEndpoint(), nil
}

// inboxCmd is the top-level `scion inbox` command (design §6.7, ptone/scion#9008).
// Human-mode only (absent from agentAllowed in cmd/cli_mode.go) and allowed on
// Windows (listed in windowsAllowed in cmd/platform_gate.go).
var inboxCmd = newInboxCmd()

func newInboxCmd() *cobra.Command {
	var (
		previews int
		chat     string
	)
	cmd := &cobra.Command{
		Use:   "inbox",
		Short: "Show a summary of items waiting on you across your projects",
		Long: `Show a read-only summary of items waiting on you across your Hub projects:
decisions, agents waiting for input, pending secrets, reviews, and unread chat
conversations.

Requires the web.inbox_dashboard experiment on the Hub. Under user access token
(UAT) authentication, the token must carry the inbox:read scope; pending secrets
are session-only and are shown as unavailable with a browser link.

Examples:
  scion inbox
  scion inbox --previews 5
  scion inbox --previews 0
  scion inbox --chat mentions
  scion inbox --format json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInbox(cmd, previews, chat)
		},
	}
	cmd.Flags().IntVar(&previews, "previews", hubclient.InboxSummaryDefaultPreviews,
		fmt.Sprintf("Maximum previews per category (0..%d; 0 returns counts only)", hubclient.InboxSummaryMaxPreviews))
	cmd.Flags().StringVar(&chat, "chat", hubclient.InboxChatFilterAll,
		"Which unread conversations populate chat previews: all, dms, or mentions")
	return cmd
}

func init() {
	rootCmd.AddCommand(inboxCmd)
}

func runInbox(cmd *cobra.Command, previews int, chat string) error {
	if previews < 0 || previews > hubclient.InboxSummaryMaxPreviews {
		return newUsageError("--previews must be between 0 and %d", hubclient.InboxSummaryMaxPreviews)
	}
	chat = strings.TrimSpace(chat)
	switch chat {
	case "", hubclient.InboxChatFilterAll:
		chat = hubclient.InboxChatFilterAll
	case hubclient.InboxChatFilterDMs, hubclient.InboxChatFilterMentions:
	default:
		return newUsageError("--chat must be one of %s, %s, %s",
			hubclient.InboxChatFilterAll, hubclient.InboxChatFilterDMs, hubclient.InboxChatFilterMentions)
	}

	client, hubBaseURL, err := inboxClientTarget()
	if err != nil {
		return err
	}

	opts := &hubclient.InboxSummaryOptions{}
	if cmd.Flags().Changed("previews") {
		p := previews
		opts.Previews = &p
	}
	if cmd.Flags().Changed("chat") || chat != hubclient.InboxChatFilterAll {
		opts.Chat = chat
	}

	parentCtx := cmd.Context()
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	ctx, cancel := context.WithTimeout(parentCtx, 30*time.Second)
	defer cancel()

	summary, err := client.Users().InboxSummary(ctx, opts)
	if err != nil {
		return formatInboxAPIError(err)
	}

	if isJSONOutput() {
		if len(summary.Raw) > 0 {
			return writeJSONTo(cmd.OutOrStdout(), summary.Raw)
		}
		return writeJSONTo(cmd.OutOrStdout(), summary)
	}

	writeInboxSummary(cmd.OutOrStdout(), summary, hubBaseURL)
	return nil
}

type inboxDisabledError struct{}

func (inboxDisabledError) Error() string { return errMsgInboxExperimentRequired }

func formatInboxAPIError(err error) error {
	var apiErr *apiclient.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.StatusCode {
		case http.StatusNotFound:
			return inboxDisabledError{}
		case http.StatusForbidden:
			return fmt.Errorf("%w\nhint: %s", err, errHintInboxForbiddenScope)
		}
	}
	return err
}

func writeInboxSummary(out io.Writer, summary *hubclient.InboxSummaryResponse, hubBaseURL string) {
	if summary == nil {
		return
	}

	header := fmt.Sprintf("INBOX  %s waiting on you", formatInboxCount(summary.Total, summary.TotalCapped))
	if gen := formatInboxGeneratedAt(summary.GeneratedAt); gen != "" {
		header += fmt.Sprintf("  (generated %s)", gen)
	}
	_, _ = fmt.Fprintln(out, header)

	keys := orderedInboxCategoryKeys(summary.Categories)
	shown := 0
	for _, key := range keys {
		if key == inboxMergedSecretsKey {
			if writeMergedSecretsCategory(out, summary.Categories, hubBaseURL) {
				shown++
			}
			continue
		}
		cat, ok := summary.Categories[key]
		if !ok || cat.State == hubclient.InboxStateDisabled {
			continue
		}
		shown++
		writeInboxCategory(out, key, cat, hubBaseURL)
	}
	if shown == 0 {
		_, _ = fmt.Fprintln(out, "  (no inbox categories enabled on this hub)")
	}
}

func orderedInboxCategoryKeys(categories map[string]hubclient.InboxCategory) []string {
	keys := make([]string, 0, len(categories))
	seen := make(map[string]bool, len(inboxCategoryOrder)+2)
	_, hasAS := categories[hubclient.InboxCategoryAgentSecrets]
	_, hasPS := categories[hubclient.InboxCategoryPendingSecrets]
	for _, k := range inboxCategoryOrder {
		seen[k] = true
		if k == inboxMergedSecretsKey {
			seen[hubclient.InboxCategoryAgentSecrets] = true
			seen[hubclient.InboxCategoryPendingSecrets] = true
			if hasAS || hasPS {
				keys = append(keys, k)
			}
			continue
		}
		if _, ok := categories[k]; ok {
			keys = append(keys, k)
		}
	}
	var extras []string
	for k := range categories {
		if !seen[k] {
			extras = append(extras, k)
		}
	}
	sort.Strings(extras)
	return append(keys, extras...)
}

func writeMergedSecretsCategory(out io.Writer, categories map[string]hubclient.InboxCategory, hubBaseURL string) bool {
	asCat, hasAS := categories[hubclient.InboxCategoryAgentSecrets]
	psCat, hasPS := categories[hubclient.InboxCategoryPendingSecrets]
	if !hasAS && !hasPS {
		return false
	}

	asState := hubclient.InboxStateDisabled
	if hasAS {
		asState = asCat.State
	}
	psState := hubclient.InboxStateDisabled
	if hasPS {
		psState = psCat.State
	}

	// Hidden only when both are disabled.
	if asState == hubclient.InboxStateDisabled && psState == hubclient.InboxStateDisabled {
		return false
	}

	const label = "Secrets"

	// Error takes precedence if either category errored.
	if asState == hubclient.InboxStateError || psState == hubclient.InboxStateError {
		code := ""
		if asState == hubclient.InboxStateError {
			code = safeText(asCat.ErrorCode)
		}
		if code == "" && psState == hubclient.InboxStateError {
			code = safeText(psCat.ErrorCode)
		}
		if code == "" {
			code = hubclient.InboxErrorInternal
		}
		_, _ = fmt.Fprintf(out, "%-16s %4s   error (%s)\n", label, "!", code)
		return true
	}

	asOK := asState == hubclient.InboxStateOK
	psOK := psState == hubclient.InboxStateOK

	// If neither is ok (both unavailable, or one unavailable and one disabled),
	// show the unavailable note.
	if !asOK && !psOK {
		var targets []string
		if hasAS && asState == hubclient.InboxStateUnavailable {
			if t := inboxWebTarget(hubBaseURL, asCat.Href); t != "" {
				targets = append(targets, t)
			}
		}
		if hasPS && psState == hubclient.InboxStateUnavailable {
			if t := inboxWebTarget(hubBaseURL, psCat.Href); t != "" {
				targets = append(targets, t)
			}
		}
		if len(targets) > 0 {
			_, _ = fmt.Fprintf(out, "%-16s %4s   unavailable with token auth (open %s in the browser)\n",
				label, "–", strings.Join(targets, ", "))
		} else {
			_, _ = fmt.Fprintf(out, "%-16s %4s   unavailable with token auth\n", label, "–")
		}
		return true
	}

	totalCount := 0
	capped := false
	breakdown := map[string]int{}
	var targets []string

	if asOK {
		totalCount += asCat.Count
		if asCat.Capped {
			capped = true
			breakdown["secretRequestsCapped"] = 1
		}
		breakdown["secretRequests"] = asCat.Count
		breakdown[hubclient.InboxBreakdownAgents] = asCat.Breakdown[hubclient.InboxBreakdownAgents]
		if t := inboxWebTarget(hubBaseURL, asCat.Href); t != "" {
			targets = append(targets, t)
		}
	}
	if psOK {
		totalCount += psCat.Count
		if psCat.Capped {
			capped = true
			breakdown["pendingSecretsCapped"] = 1
		}
		breakdown["pendingSecrets"] = psCat.Count
		if t := inboxWebTarget(hubBaseURL, psCat.Href); t != "" {
			targets = append(targets, t)
		}
	}

	// Distinct union of projects: prefer agent_secrets.breakdown.secretsProjects
	// when agent_secrets is ok; otherwise fall back to pending_secrets.breakdown.projects.
	projects := 0
	if asOK {
		if sp, ok := asCat.Breakdown[hubclient.InboxBreakdownSecretsProjects]; ok {
			projects = sp
		} else {
			projects = asCat.Breakdown[hubclient.InboxBreakdownProjects]
			if psOK && projects == 0 {
				projects = psCat.Breakdown[hubclient.InboxBreakdownProjects]
			}
		}
	} else if psOK {
		projects = psCat.Breakdown[hubclient.InboxBreakdownProjects]
	}
	breakdown[hubclient.InboxBreakdownProjects] = projects

	countStr := formatInboxCount(totalCount, capped)
	detail := strings.Join(targets, "  ")
	if detail != "" {
		_, _ = fmt.Fprintf(out, "%-16s %4s   %s\n", label, countStr, detail)
	} else {
		_, _ = fmt.Fprintf(out, "%-16s %4s\n", label, countStr)
	}

	if prose := formatInboxProse(inboxMergedSecretsKey, totalCount, capped, breakdown); prose != "" {
		_, _ = fmt.Fprintf(out, "  %s\n", prose)
	}
	if asOK {
		for _, raw := range asCat.Previews {
			if line := formatInboxPreview(hubclient.InboxCategoryAgentSecrets, raw); line != "" {
				_, _ = fmt.Fprintf(out, "  • %s\n", line)
			}
		}
	}
	if psOK {
		for _, raw := range psCat.Previews {
			if line := formatInboxPreview(hubclient.InboxCategoryPendingSecrets, raw); line != "" {
				_, _ = fmt.Fprintf(out, "  • %s\n", line)
			}
		}
	}
	return true
}

func writeInboxCategory(out io.Writer, key string, cat hubclient.InboxCategory, hubBaseURL string) {
	label := inboxCategoryLabel(key)
	var countStr, detail string

	// Rely on State rather than CountsTowardTotal when deciding how to render
	// a category: non-ok categories never count toward the inbox total.
	switch cat.State {
	case hubclient.InboxStateOK:
		countStr = formatInboxCount(cat.Count, cat.Capped)
		detail = inboxCategoryOKDetail(key, cat, hubBaseURL)
	case hubclient.InboxStateUnavailable:
		countStr = "–"
		target := inboxWebTarget(hubBaseURL, cat.Href)
		if target != "" {
			detail = fmt.Sprintf("unavailable with token auth (open %s in the browser)", target)
		} else {
			detail = "unavailable with token auth"
		}
	case hubclient.InboxStateError:
		countStr = "!"
		code := safeText(cat.ErrorCode)
		if code == "" {
			code = hubclient.InboxErrorInternal
		}
		detail = fmt.Sprintf("error (%s)", code)
	default:
		countStr = "–"
		detail = safeText(cat.State)
	}

	if detail != "" {
		_, _ = fmt.Fprintf(out, "%-16s %4s   %s\n", label, countStr, detail)
	} else {
		_, _ = fmt.Fprintf(out, "%-16s %4s\n", label, countStr)
	}

	if cat.State != hubclient.InboxStateOK {
		return
	}
	if prose := formatInboxProse(key, cat.Count, cat.Capped, cat.Breakdown); prose != "" {
		_, _ = fmt.Fprintf(out, "  %s\n", prose)
	}
	for _, raw := range cat.Previews {
		if line := formatInboxPreview(key, raw); line != "" {
			_, _ = fmt.Fprintf(out, "  • %s\n", line)
		}
	}
}

func inboxCategoryLabel(key string) string {
	switch key {
	case hubclient.InboxCategoryDecisions:
		return "Decisions"
	case hubclient.InboxCategoryAgentSecrets:
		return "Agent Secrets"
	case hubclient.InboxCategoryWaiting:
		return "Agents Waiting"
	case hubclient.InboxCategoryPendingSecrets:
		return "Pending Secrets"
	case hubclient.InboxCategoryReviews:
		return "Reviews"
	case hubclient.InboxCategoryChat:
		return "Chat"
	default:
		return safeText(key)
	}
}

func inboxCategoryOKDetail(key string, cat hubclient.InboxCategory, hubBaseURL string) string {
	var parts []string
	switch key {
	case hubclient.InboxCategoryDecisions:
		parts = append(parts, "scion decisions list")
	case hubclient.InboxCategoryWaiting:
		parts = append(parts, "scion look <agent>")
	case hubclient.InboxCategoryPendingSecrets:
		if target := inboxWebTarget(hubBaseURL, cat.Href); target != "" {
			parts = append(parts, target)
		}
	case hubclient.InboxCategoryReviews:
		if cat.Breakdown[hubclient.InboxBreakdownRequested] > 0 {
			parts = append(parts, "scion notifications")
		}
		if target := inboxWebTarget(hubBaseURL, cat.Href); target != "" {
			parts = append(parts, target)
		}
	case hubclient.InboxCategoryChat:
		if target := inboxWebTarget(hubBaseURL, cat.Href); target != "" {
			parts = append(parts, target)
		}
	default:
		if target := inboxWebTarget(hubBaseURL, cat.Href); target != "" {
			parts = append(parts, target)
		}
	}
	return strings.Join(parts, "  ")
}

func formatInboxPreview(category string, raw json.RawMessage) string {
	switch category {
	case hubclient.InboxCategoryDecisions:
		var p hubclient.InboxDecisionPreview
		if err := json.Unmarshal(raw, &p); err == nil {
			var parts []string
			if proj := collapseSingleLine(p.ProjectName); proj != "" {
				parts = append(parts, proj)
			}
			if q := truncateRunes(collapseSingleLine(p.Question), inboxPreviewQuestionMaxRunes, true); q != "" {
				parts = append(parts, fmt.Sprintf("%q", q))
			}
			if asker := collapseSingleLine(p.AskerAgentName); asker != "" {
				parts = append(parts, asker)
			}
			if !p.CreatedAt.IsZero() {
				parts = append(parts, clitime.Ago(p.CreatedAt))
			}
			if len(parts) > 0 {
				return strings.Join(parts, " · ")
			}
		}
	case hubclient.InboxCategoryWaiting:
		var p hubclient.InboxWaitingAgentPreview
		if err := json.Unmarshal(raw, &p); err == nil {
			var parts []string
			agentLabel := collapseSingleLine(p.AgentSlug)
			if agentLabel == "" {
				agentLabel = collapseSingleLine(p.AgentName)
			}
			proj := collapseSingleLine(p.ProjectName)
			switch {
			case proj != "" && agentLabel != "":
				parts = append(parts, proj+" / "+agentLabel)
			case agentLabel != "":
				parts = append(parts, agentLabel)
			case proj != "":
				parts = append(parts, proj)
			}
			if q := truncateRunes(collapseSingleLine(p.Question), inboxPreviewQuestionMaxRunes, true); q != "" {
				parts = append(parts, fmt.Sprintf("%q", q))
			}
			ts := p.Since
			if ts.IsZero() {
				ts = p.CreatedAt
			}
			if !ts.IsZero() {
				parts = append(parts, clitime.Ago(ts))
			}
			if len(parts) > 0 {
				return strings.Join(parts, " · ")
			}
		}
	case hubclient.InboxCategoryPendingSecrets:
		var p hubclient.InboxPendingSecretPreview
		if err := json.Unmarshal(raw, &p); err == nil {
			var parts []string
			if k := collapseSingleLine(p.Key); k != "" {
				parts = append(parts, k)
			} else if p.Kind != "" {
				parts = append(parts, collapseSingleLine(p.Kind))
			}
			if slug := collapseSingleLine(p.AgentSlug); slug != "" {
				parts = append(parts, slug)
			}
			if proj := collapseSingleLine(p.ProjectName); proj != "" {
				parts = append(parts, proj)
			}
			if !p.ExpiresAt.IsZero() {
				parts = append(parts, "expires "+clitime.Relative(p.ExpiresAt))
			} else if !p.CreatedAt.IsZero() {
				parts = append(parts, clitime.Ago(p.CreatedAt))
			}
			if len(parts) > 0 {
				return strings.Join(parts, " · ")
			}
		}
	case hubclient.InboxCategoryReviews:
		var p hubclient.InboxReviewPreview
		if err := json.Unmarshal(raw, &p); err == nil {
			var parts []string
			title := collapseSingleLine(p.Title)
			if title == "" {
				title = collapseSingleLine(p.ArtifactID)
			}
			if kindLabel := inboxReviewKindLabel(p.Kind); kindLabel != "" && title != "" {
				parts = append(parts, fmt.Sprintf("[%s] %s", kindLabel, title))
			} else if title != "" {
				parts = append(parts, title)
			}
			if scope := collapseSingleLine(p.ScopeRef); scope != "" {
				parts = append(parts, scope)
			} else if proj := collapseSingleLine(p.ProjectName); proj != "" {
				parts = append(parts, proj)
			}
			if by := collapseSingleLine(p.RequestedBy); by != "" {
				parts = append(parts, "from "+by)
			}
			if !p.CreatedAt.IsZero() {
				parts = append(parts, clitime.Ago(p.CreatedAt))
			}
			if len(parts) > 0 {
				return strings.Join(parts, " · ")
			}
		}
	case hubclient.InboxCategoryChat:
		var p hubclient.InboxChatPreview
		if err := json.Unmarshal(raw, &p); err == nil {
			var parts []string
			label := collapseSingleLine(p.Label)
			if label == "" {
				label = collapseSingleLine(p.ConversationKey)
			}
			if kindLabel := inboxChatKindLabel(p.Kind); kindLabel != "" && label != "" {
				parts = append(parts, fmt.Sprintf("[%s] %s", kindLabel, label))
			} else if label != "" {
				parts = append(parts, label)
			}
			if proj := collapseSingleLine(p.ProjectName); proj != "" {
				parts = append(parts, proj)
			}
			ts := p.LastMessageAt
			if ts.IsZero() {
				ts = p.CreatedAt
			}
			if !ts.IsZero() {
				parts = append(parts, clitime.Ago(ts))
			}
			if len(parts) > 0 {
				return strings.Join(parts, " · ")
			}
		}
	}

	// Generic fallback for unknown categories or future preview shapes.
	var generic struct {
		ID          string    `json:"id"`
		ProjectName string    `json:"projectName"`
		Title       string    `json:"title"`
		Label       string    `json:"label"`
		Key         string    `json:"key"`
		CreatedAt   time.Time `json:"createdAt"`
	}
	if err := json.Unmarshal(raw, &generic); err == nil {
		var parts []string
		if proj := collapseSingleLine(generic.ProjectName); proj != "" {
			parts = append(parts, proj)
		}
		for _, candidate := range []string{generic.Title, generic.Label, generic.Key, generic.ID} {
			if c := collapseSingleLine(candidate); c != "" {
				parts = append(parts, c)
				break
			}
		}
		if !generic.CreatedAt.IsZero() {
			parts = append(parts, clitime.Ago(generic.CreatedAt))
		}
		if len(parts) > 0 {
			return strings.Join(parts, " · ")
		}
	}
	return ""
}

func inboxReviewKindLabel(kind string) string {
	switch kind {
	case "to_address":
		return "to address"
	case "requested":
		return "requested"
	default:
		return collapseSingleLine(kind)
	}
}

func inboxChatKindLabel(kind string) string {
	switch kind {
	case "dm":
		return "DM"
	case "mention":
		return "mention"
	case "thread":
		return "thread"
	default:
		return collapseSingleLine(kind)
	}
}

func formatInboxCount(count int, capped bool) string {
	s := strconv.Itoa(count)
	if capped {
		return s + "+"
	}
	return s
}

func formatInboxGeneratedAt(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return ""
	}
	return clitime.Format(t, clitime.Clock)
}

func inboxWebTarget(hubBaseURL, href string) string {
	href = strings.TrimSpace(href)
	if href == "" {
		return ""
	}
	if strings.HasPrefix(href, "http://") || strings.HasPrefix(href, "https://") {
		return safeText(href)
	}
	base := strings.TrimRight(strings.TrimSpace(hubBaseURL), "/")
	if base == "" {
		return safeText(href)
	}
	if !strings.HasPrefix(href, "/") {
		href = "/" + href
	}
	return safeText(base + href)
}

func pluralizeInbox(n int, singular, plural string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", singular)
	}
	return fmt.Sprintf("%d %s", n, plural)
}
