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
	"text/tabwriter"
	"time"

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

In human mode, lists the Decisions in the current project that are waiting on
you (the default, --waiting) or the log of Decisions made (--made). Search
with -q/--search (question, answer, key, agent or conversation name, at least
2 characters) and narrow with --conversation and --agent, which take a name
(case-insensitive, # optional) or a key or ID. On --made, also narrow with
--answered-by, --provenance, --since and --until (answer time).

In agent mode, lists the decisions your agent has asked. By default only
pending decisions; pass --decided for answered active decisions or --all to
include pending, decided and replaced versions.

Examples:
  scion decisions list
  scion decisions list -q release
  scion decisions list --made --conversation release --since 2026-11-01
  scion decisions list --made --answered-by me --json
  scion decisions list --decided        # agent mode
  scion decisions list --all --json     # agent mode`,
		Args: cobra.NoArgs,
		RunE: runDecisionsList,
	}
	// Agent mode.
	cmd.Flags().Bool("pending", false, "List pending decisions only (default)")
	cmd.Flags().Bool("decided", false, "List decided active decisions only")
	cmd.Flags().Bool("all", false, "List pending, decided, and replaced decisions")
	markFlagsForMode(cmd, ModeAgent, "pending", "decided", "all")
	// Human mode.
	cmd.Flags().Bool("waiting", false, "List Decisions waiting on you (default)")
	cmd.Flags().Bool("made", false, "List the Decisions made (the answer log)")
	cmd.Flags().StringP("search", "q", "", "Search question, answer, key, agent and conversation names (2 to 256 characters)")
	cmd.Flags().String("conversation", "", "Only this conversation: a name such as #release or \"DM with nc-x\", or its key")
	cmd.Flags().String("agent", "", "Only Decisions asked by this agent: a name or an agent ID")
	cmd.Flags().String("answered-by", "", "With --made: only answers by this user: me or a user ID")
	cmd.Flags().String("provenance", "", "With --made: direct, interpreted, confirmed or overridden")
	cmd.Flags().String("since", "", "With --made: answered on or after this day (YYYY-MM-DD, UTC) or time (RFC3339)")
	cmd.Flags().String("until", "", "With --made: answered on or before this day (YYYY-MM-DD, UTC) or time (RFC3339)")
	cmd.Flags().Int("limit", defaultDecisionsListLimit, fmt.Sprintf("Maximum number of prompts or answers to list (1 to %d)", maxDecisionsListLimit))
	markFlagsForMode(cmd, ModeHuman, "waiting", "made", "search", "conversation", "agent",
		"answered-by", "provenance", "since", "until", "limit")
	cmd.Flags().Bool("json", false, "Output in JSON format")
	return cmd
}

func resolveDecisionsListState(cmd *cobra.Command) (string, error) {
	pending, _ := cmd.Flags().GetBool("pending")
	decided, _ := cmd.Flags().GetBool("decided")
	all, _ := cmd.Flags().GetBool("all")

	setCount := 0
	if pending {
		setCount++
	}
	if decided {
		setCount++
	}
	if all {
		setCount++
	}
	if setCount > 1 {
		return "", newUsageError("pass at most one of --pending, --decided, or --all")
	}
	switch {
	case decided:
		return "decided", nil
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
			Cursor: cursor,
			Limit:  200,
		})
		if err != nil {
			return formatDecisionAPIError(err)
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
		})
	}

	if len(items) == 0 {
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
		if it.Answer != nil && it.Answer.Label != "" {
			answer = collapseSingleLine(it.Answer.Label)
		}
		answeredBy := decisionAnswererDisplay(it.Answer)
		provenance := decisionProvenanceDisplay(it.Answer)
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
