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
	"time"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/spf13/cobra"
)

const (
	errMsgDecisionWithdrawUnsupported = "this hub does not support withdrawing decisions; update the hub"
	errMsgDecisionWithdrawAgentsOnly  = "this command is for agents; it is available only in agent mode"
)

func newDecisionsWithdrawCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "withdraw <key>...",
		Short: "Withdraw open Decision(s) that are no longer needed",
		Long: `Withdraw one or more open Decisions asked by your agent when the question is no
longer needed.

Withdrawn Decisions leave Waiting on you and stop counting towards your pending
cap. Repeating a withdrawal on an already-withdrawn Decision is an idempotent
no-op. Decisions that already have an answer (direct or interpreted awaiting
confirmation) cannot be withdrawn.

Pass --set to also withdraw every other pending Decision of the prompt each key
belongs to; answered siblings (including interpreted answers awaiting
confirmation) are left untouched.

Examples:
  scion decisions withdraw D234 --reason "CI found a regression"
  scion decisions withdraw D234 17 --reason "No longer needed"
  scion decisions withdraw D234 --set --reason "Moving release to Monday"
  scion decisions withdraw D234 --json`,
		RunE: runDecisionsWithdraw,
	}
	cmd.Flags().Bool("set", false, "Also withdraw every other pending Decision of each key's prompt")
	cmd.Flags().String("reason", "", "Optional plain-text reason why the Decision is no longer needed (at most 500 characters)")
	cmd.Flags().Bool("json", false, "Output in JSON format")
	return cmd
}

func parseDecisionsWithdrawArgs(args []string) ([]string, error) {
	if len(args) == 0 {
		return nil, newUsageError("provide at least one decision <key>")
	}
	if len(args) > messages.MaxDecisionsPerMessage {
		return nil, newUsageError("at most %d decision keys may be withdrawn at once", messages.MaxDecisionsPerMessage)
	}
	seenNorms := make(map[string]bool, len(args))
	keys := make([]string, 0, len(args))
	for _, raw := range args {
		k := strings.TrimSpace(raw)
		if k == "" {
			return nil, newUsageError("decision <key> must be non-empty")
		}
		norm := messages.DecisionKeyNorm(k)
		if seenNorms[norm] {
			return nil, newUsageError("duplicate decision key %q", k)
		}
		seenNorms[norm] = true
		keys = append(keys, k)
	}
	return keys, nil
}

func runDecisionsWithdraw(cmd *cobra.Command, args []string) error {
	if resolveMode() != ModeAgent {
		return errors.New(errMsgDecisionWithdrawAgentsOnly)
	}

	keys, err := parseDecisionsWithdrawArgs(args)
	if err != nil {
		return err
	}

	wholeSet, _ := cmd.Flags().GetBool("set")
	rawReason, _ := cmd.Flags().GetString("reason")
	reason := strings.TrimSpace(rawReason)
	if utf8.RuneCountInString(reason) > store.MaxDecisionWithdrawReasonRunes {
		return newUsageError("--reason must be at most %d characters", store.MaxDecisionWithdrawReasonRunes)
	}

	client, err := decisionsClientFactory()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

	resp, err := client.Decisions().Withdraw(ctx, &hubclient.WithdrawDecisionsRequest{
		Keys:     keys,
		WholeSet: wholeSet,
		Reason:   reason,
	})
	if err != nil {
		return formatDecisionWithdrawError(ctx, client, err)
	}

	if decisionsWantJSON(cmd) {
		return writeJSONTo(cmd.OutOrStdout(), resp)
	}

	writeDecisionsWithdrawResult(cmd.OutOrStdout(), keys, resp)
	return nil
}

func formatDecisionWithdrawError(ctx context.Context, client hubclient.Client, err error) error {
	var apiErr *apiclient.APIError
	if errors.As(err, &apiErr) {
		if apiErr.StatusCode == http.StatusMethodNotAllowed {
			return errors.New(errMsgDecisionWithdrawUnsupported)
		}
		if apiErr.StatusCode == http.StatusNotFound && apiErr.Code != "decision_not_found" {
			msg := strings.TrimSpace(apiErr.Message)
			if !strings.EqualFold(msg, "route not found") && !strings.EqualFold(msg, "not found") {
				return errors.New(errMsgDecisionWithdrawUnsupported)
			}
			if client != nil {
				if _, listErr := client.Decisions().List(ctx, &hubclient.ListAgentDecisionsOptions{Limit: 1}); listErr == nil {
					return errors.New(errMsgDecisionWithdrawUnsupported)
				}
			}
			return errors.New(errMsgDecisionsExperimentRequired)
		}
	}
	return formatDecisionAPIError(err)
}

func formatWithdrawFailureEntry(entry map[string]interface{}) string {
	if msg, ok := entry["message"].(string); ok && strings.TrimSpace(msg) != "" {
		return safeText(msg)
	}
	key, _ := entry["key"].(string)
	reason, _ := entry["reason"].(string)
	if key == "" {
		return ""
	}
	switch reason {
	case store.WithdrawReasonAlreadyAnswered:
		if dec, ok := entry["decision"].(map[string]interface{}); ok {
			if ans, ok := dec["decision"].(map[string]interface{}); ok {
				who, _ := ans["answeredByName"].(string)
				if who == "" {
					who, _ = ans["answeredByEmail"].(string)
				}
				label, _ := ans["label"].(string)
				if who != "" && label != "" {
					return fmt.Sprintf("%s: already answered by %s (%s)", safeText(key), safeText(who), collapseSingleLine(label))
				}
			}
		}
		return fmt.Sprintf("%s: already answered", safeText(key))
	case store.WithdrawReasonDismissed:
		if dec, ok := entry["decision"].(map[string]interface{}); ok {
			if dsm, ok := dec["dismissed"].(map[string]interface{}); ok {
				who, _ := dsm["byName"].(string)
				if who != "" {
					return fmt.Sprintf("%s: dismissed by %s", safeText(key), safeText(who))
				}
			}
		}
		return fmt.Sprintf("%s: dismissed", safeText(key))
	case store.WithdrawReasonNotFound:
		return fmt.Sprintf("%s: decision not found", safeText(key))
	default:
		if reason != "" {
			return fmt.Sprintf("%s: %s", safeText(key), safeText(reason))
		}
		return safeText(key)
	}
}

func writeDecisionsWithdrawResult(out io.Writer, requestedKeys []string, resp *hubclient.WithdrawDecisionsResponse) {
	type outcome struct {
		view  hubclient.DecisionView
		label string
	}
	byNorm := make(map[string]outcome, len(resp.Withdrawn)+len(resp.AlreadyWithdrawn))
	ordered := make([]outcome, 0, len(resp.Withdrawn)+len(resp.AlreadyWithdrawn))
	for _, d := range resp.Withdrawn {
		o := outcome{view: d, label: "withdrawn"}
		byNorm[messages.DecisionKeyNorm(d.Key)] = o
		ordered = append(ordered, o)
	}
	for _, d := range resp.AlreadyWithdrawn {
		o := outcome{view: d, label: "already withdrawn"}
		byNorm[messages.DecisionKeyNorm(d.Key)] = o
		ordered = append(ordered, o)
	}

	printed := make(map[string]bool, len(ordered))
	for _, rawKey := range requestedKeys {
		norm := messages.DecisionKeyNorm(rawKey)
		if o, ok := byNorm[norm]; ok && !printed[norm] {
			printed[norm] = true
			_, _ = fmt.Fprintf(out, "%s v%d %s\n", safeText(o.view.Key), o.view.Version, o.label)
		}
	}
	for _, o := range ordered {
		norm := messages.DecisionKeyNorm(o.view.Key)
		if !printed[norm] {
			printed[norm] = true
			_, _ = fmt.Fprintf(out, "%s v%d %s\n", safeText(o.view.Key), o.view.Version, o.label)
		}
	}
}
