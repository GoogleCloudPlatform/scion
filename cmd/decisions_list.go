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
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

const decisionQuestionMaxWidth = 50

func newDecisionsListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List Decisions waiting on you, or the Decisions made",
		Long: `List Decisions.

In human mode, lists the Decisions in the current project (or across all of
your member projects with --all-projects) that are waiting on you (the default,
--waiting) or the log of Decisions made (--made). Search with -q/--search
(question, answer, key, agent or conversation name, at least 2 characters) and
narrow with --conversation and --agent, which take a name (case-insensitive, #
optional) or a key or ID. With --all-projects, --project narrows to one member
project by name, slug or ID, and --sort selects project or recent order. On
--made, also narrow with --answered-by, --provenance, --since and --until
(answer time).

In agent mode, lists the Decisions your agent has asked. By default only
pending Decisions; pass --decided for answered active Decisions, --withdrawn
for withdrawn Decisions, --dismissed for dismissed Decisions, or --all to
include pending, decided, replaced, withdrawn and dismissed versions. Search
with -q/--search (question, key, effective answer label, or dismissal reason,
2 to 256 characters).

Examples:
  scion decisions list
  scion decisions list -q release
  scion decisions list --all-projects
  scion decisions list --all-projects --project scion --sort recent
  scion decisions list --made --conversation release --since 2026-11-01
  scion decisions list --made --answered-by me --json
  scion decisions list --decided          # agent mode
  scion decisions list --withdrawn        # agent mode
  scion decisions list --dismissed        # agent mode
  scion decisions list --all -q release   # agent mode
  scion decisions list --all --json       # agent mode`,
		Args: cobra.NoArgs,
		RunE: runDecisionsList,
	}
	// Agent mode.
	cmd.Flags().Bool("pending", false, "List pending Decisions only (default)")
	cmd.Flags().Bool("decided", false, "List decided active Decisions only")
	cmd.Flags().Bool("withdrawn", false, "List withdrawn Decisions only")
	cmd.Flags().Bool("dismissed", false, "List dismissed Decisions only")
	cmd.Flags().Bool("all", false, "List pending, decided, replaced, withdrawn, and dismissed Decisions")
	markFlagsForMode(cmd, ModeAgent, "pending", "decided", "withdrawn", "dismissed", "all")
	// Human mode.
	cmd.Flags().Bool("waiting", false, "List Decisions waiting on you (default)")
	cmd.Flags().Bool("made", false, "List the Decisions made (the answer log)")
	cmd.Flags().Bool("all-projects", false, "List Decisions across all of your member projects")
	cmd.Flags().String("sort", "", "With --all-projects: sort order, project or recent")
	cmd.Flags().String("conversation", "", "Only this conversation: a name such as #release or \"DM with nc-x\", or its key")
	cmd.Flags().String("agent", "", "Only Decisions asked by this agent: a name or an agent ID")
	cmd.Flags().String("answered-by", "", "With --made: only answers by this user: me or a user ID")
	cmd.Flags().String("provenance", "", "With --made: direct, interpreted, confirmed, overridden or dismissed")
	cmd.Flags().String("since", "", "With --made: answered on or after this day (YYYY-MM-DD, UTC) or time (RFC3339)")
	cmd.Flags().String("until", "", "With --made: answered on or before this day (YYYY-MM-DD, UTC) or time (RFC3339)")
	cmd.Flags().Int("limit", defaultDecisionsListLimit, fmt.Sprintf("Maximum number of prompts or answers to list (1 to %d)", maxDecisionsListLimit))
	markFlagsForMode(cmd, ModeHuman, "waiting", "made", "all-projects", "sort", "conversation", "agent",
		"answered-by", "provenance", "since", "until", "limit")
	// Shared across human and agent mode.
	cmd.Flags().StringP("search", "q", "", "Search Decisions (2 to 256 characters; question, key and answer, plus agent and conversation names in human mode)")
	cmd.Flags().Bool("json", false, "Output in JSON format")
	return cmd
}

func resolveDecisionsListState(cmd *cobra.Command) (string, error) {
	pending, _ := cmd.Flags().GetBool("pending")
	decided, _ := cmd.Flags().GetBool("decided")
	withdrawn, _ := cmd.Flags().GetBool("withdrawn")
	dismissed, _ := cmd.Flags().GetBool("dismissed")
	all, _ := cmd.Flags().GetBool("all")

	setCount := 0
	if pending {
		setCount++
	}
	if decided {
		setCount++
	}
	if withdrawn {
		setCount++
	}
	if dismissed {
		setCount++
	}
	if all {
		setCount++
	}
	if setCount > 1 {
		return "", newUsageError("pass at most one of --pending, --decided, --withdrawn, --dismissed, or --all")
	}
	switch {
	case decided:
		return "decided", nil
	case withdrawn:
		return "withdrawn", nil
	case dismissed:
		return "dismissed", nil
	case all:
		return "all", nil
	default:
		return "pending", nil
	}
}

// runDecisionsList lists the caller's project Decisions in human mode and
// the agent's own decisions in agent mode (design C1).
func runDecisionsList(cmd *cobra.Command, _ []string) error {
	mode := resolveMode()
	if err := checkFlagsForMode(cmd, mode); err != nil {
		return err
	}
	if mode == ModeHuman {
		return runHumanDecisionsList(cmd)
	}
	return runAgentDecisionsList(cmd)
}

func runAgentDecisionsList(cmd *cobra.Command) error {
	state, err := resolveDecisionsListState(cmd)
	if err != nil {
		return err
	}
	search, _ := cmd.Flags().GetString("search")
	if search = strings.TrimSpace(search); search != "" {
		if n := utf8.RuneCountInString(search); n < minDecisionsSearchRunes || n > maxDecisionsSearchRunes {
			return newUsageError("--search must be %d to %d characters", minDecisionsSearchRunes, maxDecisionsSearchRunes)
		}
	}

	client, err := decisionsClientFactory()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

	items := make([]hubclient.AgentDecisionListItem, 0)
	cursor := ""
	for {
		page, err := client.Decisions().List(ctx, &hubclient.ListAgentDecisionsOptions{
			State:  state,
			Query:  search,
			Cursor: cursor,
			Limit:  200,
		})
		if err != nil {
			var apiErr *apiclient.APIError
			if state == "withdrawn" && errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusBadRequest && strings.Contains(apiErr.Message, "invalid state") {
				return errors.New(errMsgDecisionWithdrawUnsupported)
			}
			return formatDecisionAPIError(err)
		}
		if search != "" && page.Query == "" {
			return errors.New(errMsgDecisionSearchUnsupported)
		}
		items = append(items, page.Items...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}

	if decisionsWantJSON(cmd) {
		return writeJSONTo(cmd.OutOrStdout(), hubclient.AgentDecisionListResponse{
			Items: items,
			Query: search,
		})
	}

	if len(items) == 0 {
		if search != "" {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "No %s decisions match %q.\n", state, safeText(search))
			return nil
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "No %s decisions.\n", state)
		return nil
	}

	writeDecisionsListTable(cmd.OutOrStdout(), items)
	return nil
}

func writeDecisionsListTable(out io.Writer, items []hubclient.AgentDecisionListItem) {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "KEY\tVER\tSTATUS\tQUESTION\tANSWER\tANSWERED BY\tPROVENANCE\tCONVERSATION\tAGE")
	for _, it := range items {
		question := truncateRunes(collapseSingleLine(it.Question), decisionQuestionMaxWidth, true)
		answer := "-"
		answeredBy := decisionAnswererDisplay(it.Answer)
		provenance := decisionProvenanceDisplay(it.Answer)
		switch {
		case it.Dismissed != nil:
			answer = decisionDismissedSummary(it.Dismissed, decisionQuestionMaxWidth)
			if who := collapseSingleLine(it.Dismissed.ByName); who != "" {
				answeredBy = who
			}
			provenance = "dismissed"
		case it.Answer != nil && it.Answer.Label != "":
			answer = formatDecisionListAnswer(it.Answer.ChoiceID, it.Answer.Label, it.Answer.OtherText)
		case it.Withdrawn != nil:
			detail := "withdrawn " + clitime.Format(it.Withdrawn.At, clitime.Minute)
			if it.Withdrawn.Reason != "" {
				detail += ": " + truncateRunes(collapseSingleLine(it.Withdrawn.Reason), decisionQuestionMaxWidth, true)
			}
			answer = detail
		}
		conv := safeText(it.ConversationID)
		if conv == "" {
			conv = "-"
		}
		age := clitime.Ago(it.CreatedAt)
		_, _ = fmt.Fprintf(tw, "%s\tv%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			safeText(it.Key),
			it.Version,
			safeText(it.Status),
			question,
			answer,
			answeredBy,
			provenance,
			conv,
			age,
		)
	}
	_ = tw.Flush()
}
