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
		Short: "List choice decisions you have asked",
		Long: `List choice decisions asked by your agent identity.

By default, lists only pending decisions. Pass --decided for answered active
decisions or --all to include pending, decided, and replaced versions.

Examples:
  scion decisions list
  scion decisions list --decided
  scion decisions list --all
  scion decisions list --all --json`,
		Args: cobra.NoArgs,
		RunE: runDecisionsList,
	}
	cmd.Flags().Bool("pending", false, "List pending decisions only (default)")
	cmd.Flags().Bool("decided", false, "List decided active decisions only")
	cmd.Flags().Bool("all", false, "List pending, decided, and replaced decisions")
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

func runDecisionsList(cmd *cobra.Command, _ []string) error {
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
