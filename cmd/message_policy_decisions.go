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
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

func newMessagePolicyDecisionsCmd() *cobra.Command {
	var (
		policyFilter string
		mode         string
		surface      string
		classFilter  string
		action       string
		limit        int
		cursor       string
	)

	cmd := &cobra.Command{
		Use:   "decisions [policy-id-or-name]",
		Short: "List content-free message policy evaluation decisions",
		Long: `List content-free evaluation decisions recorded for project message policies.

Decision rows carry policy metadata, surface, sender/recipient identifiers,
message ID (when admitted or shadowed; empty when rejected before persistence),
classification, action (admitted, rejected, shadow), and rendered rejection
reason — never message body text.

Requires project owner/admin or Hub admin permissions (human mode only).`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			positionalRef := ""
			if len(args) > 0 {
				positionalRef = strings.TrimSpace(args[0])
			}
			if positionalRef != "" && strings.TrimSpace(policyFilter) != "" {
				return newUsageError("pass either [policy-id-or-name] or --policy, not both")
			}

			opts := &hubclient.ListMessagePolicyDecisionsOptions{
				PolicyID: strings.TrimSpace(policyFilter),
				Mode:     strings.TrimSpace(mode),
				Surface:  strings.TrimSpace(surface),
				Class:    strings.TrimSpace(classFilter),
				Action:   strings.TrimSpace(action),
				Limit:    limit,
				Cursor:   strings.TrimSpace(cursor),
			}

			svc, ctx, cancel, err := messagePolicyServiceForCmd(cmd)
			if err != nil {
				return err
			}
			defer cancel()

			var (
				resp *hubclient.ListMessagePolicyDecisionsResponse
			)
			if positionalRef != "" {
				resp, err = svc.ListPolicyDecisions(ctx, positionalRef, opts)
			} else {
				resp, err = svc.ListDecisions(ctx, opts)
			}
			if err != nil {
				return formatMessagePolicyAPIError(err)
			}

			if messagePolicyWantJSON(cmd) {
				return writeJSONTo(cmd.OutOrStdout(), resp)
			}

			out := cmd.OutOrStdout()
			if len(resp.Decisions) == 0 {
				_, _ = fmt.Fprintln(out, "No message policy decisions recorded.")
				return nil
			}

			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(tw, "TIME\tPOLICY\tMODE\tSURFACE\tCLASS\tACTION\tSENDER\tRECIPIENT\tMESSAGE ID\tELAPSED\tREASON")
			for _, dec := range resp.Decisions {
				when := "-"
				if !dec.CreatedAt.IsZero() {
					when = clitime.Ago(dec.CreatedAt)
				}
				polLabel := safeText(dec.PolicyName)
				if polLabel == "" {
					polLabel = safeText(dec.PolicyID)
				}
				msgID := "-"
				if dec.MessageID != "" {
					msgID = safeText(dec.MessageID)
				}
				reason := "-"
				if dec.Reason != "" {
					reason = collapseSingleLine(dec.Reason)
				} else if dec.CELErrorReason != "" {
					reason = "cel_error:" + safeText(dec.CELErrorReason)
				}
				_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s:%s\t%s\t%dms\t%s\n",
					when,
					polLabel,
					safeText(dec.Mode),
					safeText(dec.Surface),
					safeText(dec.Class),
					safeText(dec.Action),
					safeText(dec.SenderID),
					safeText(dec.RecipientKind),
					safeText(dec.RecipientID),
					msgID,
					dec.ElapsedMs,
					reason,
				)
			}
			_ = tw.Flush()

			if resp.NextCursor != "" {
				_, _ = fmt.Fprintf(out, "\nNext cursor: %s\n", safeText(resp.NextCursor))
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&policyFilter, "policy", "", "Filter decisions by policy UUID or name")
	cmd.Flags().StringVar(&mode, "mode", "", "Filter by mode: shadow or enforce")
	cmd.Flags().StringVar(&surface, "surface", "", "Filter by surface: agent_to_user or agent_to_conversation")
	cmd.Flags().StringVar(&classFilter, "class", "", "Filter by class: pass, violation, uncertain, or failed")
	cmd.Flags().StringVar(&action, "action", "", "Filter by action: admitted, rejected, or shadow")
	cmd.Flags().IntVar(&limit, "limit", 0, "Maximum number of decisions to return (1..200)")
	cmd.Flags().StringVar(&cursor, "cursor", "", "Pagination cursor from a previous decisions response")
	cmd.Flags().Bool("json", false, "Output in JSON format")

	return cmd
}
