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
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

func newDecisionsShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <key>",
		Short: "Show all versions and answer history for a decision key",
		Long: `Show every version of a decision key belonging to your agent, including choices
and all recorded answer rows (both agent-interpreted and direct user answers).

Examples:
  scion decisions show D234
  scion decisions show D234 --json`,
		Args: cobra.ExactArgs(1),
		RunE: runDecisionsShow,
	}
	cmd.Flags().Bool("json", false, "Output in JSON format")
	return cmd
}

func runDecisionsShow(cmd *cobra.Command, args []string) error {
	key := strings.TrimSpace(args[0])
	if key == "" {
		return newUsageError("decision key is required")
	}

	client, err := decisionsClientFactory()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

	resp, err := client.Decisions().Show(ctx, key)
	if err != nil {
		return formatDecisionAPIError(err)
	}

	if decisionsWantJSON(cmd) {
		return writeJSONTo(cmd.OutOrStdout(), resp)
	}

	writeDecisionsShow(cmd.OutOrStdout(), resp)
	return nil
}

func writeDecisionsShow(out io.Writer, resp *hubclient.AgentDecisionShowResponse) {
	_, _ = fmt.Fprintf(out, "Decision %s (%s)\n", safeText(resp.Key), safeText(resp.Ref))
	for i, v := range resp.Versions {
		if i > 0 {
			_, _ = fmt.Fprintln(out)
		}
		header := fmt.Sprintf("v%d [%s]", v.Version, safeText(v.Status))
		if v.ReplacedBy != nil {
			header += fmt.Sprintf(" (replaced by v%d)", v.ReplacedBy.Version)
		}
		_, _ = fmt.Fprintln(out, header)
		_, _ = fmt.Fprintf(out, "  Question:     %s\n", collapseSingleLine(v.Question))
		if v.ConversationID != "" {
			_, _ = fmt.Fprintf(out, "  Conversation: %s\n", safeText(v.ConversationID))
		}
		if v.PromptMessageID != "" {
			_, _ = fmt.Fprintf(out, "  Prompt:       %s\n", safeText(v.PromptMessageID))
		}
		_, _ = fmt.Fprintf(out, "  Created:      %s\n", clitime.Format(v.CreatedAt, clitime.Minute))
		if v.DecidedAt != nil {
			_, _ = fmt.Fprintf(out, "  Decided:      %s\n", clitime.Format(*v.DecidedAt, clitime.Minute))
		}
		_, _ = fmt.Fprintln(out, "  Choices:")
		for _, c := range v.Choices {
			suffix := ""
			if v.DefaultChoiceID != "" && c.ID == v.DefaultChoiceID {
				suffix = " (default)"
			}
			_, _ = fmt.Fprintf(out, "    - %s: %s%s\n", safeText(c.ID), collapseSingleLine(c.Label), suffix)
		}
		if len(v.Answers) == 0 {
			_, _ = fmt.Fprintln(out, "  Answers:      none")
			continue
		}
		_, _ = fmt.Fprintln(out, "  Answers:")
		for _, a := range v.Answers {
			aCopy := a
			prov := decisionProvenanceDisplay(&aCopy)
			who := decisionAnswererDisplay(&aCopy)
			line := fmt.Sprintf("    - %s (%s) [%s] by %s via %s at %s",
				collapseSingleLine(a.Label),
				safeText(a.ChoiceID),
				prov,
				who,
				safeText(a.Surface),
				clitime.Format(a.At, clitime.Minute),
			)
			if a.SourceMessageID != "" {
				line += fmt.Sprintf(" (source %s)", safeText(a.SourceMessageID))
			}
			_, _ = fmt.Fprintln(out, line)
		}
	}
}
