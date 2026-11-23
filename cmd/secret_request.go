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
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/msgaction"
	"github.com/spf13/cobra"
)

const (
	errMsgSecretRequestAgentsOnly = "this command is for agents; it is available only in agent mode"
	secretRequestDetailMaxWidth   = 50
)

var secretRequestsClientFactory = func() (hubclient.Client, error) {
	_, client, err := requireHubClient()
	return client, err
}

func newSecretRequestCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "request",
		Short: "List or withdraw this agent's secret requests",
		Long: `Inspect and withdraw secret requests made by this agent (agent mode only).

Commands:
  scion secret request list [--pending | --fulfilled | --withdrawn | --dismissed | --expired | --all]
  scion secret request withdraw <key>... [--set] [--reason <text>]`,
	}
	cmd.AddCommand(
		newSecretRequestListCmd(),
		newSecretRequestWithdrawCmd(),
	)
	return cmd
}

func newSecretRequestListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List secret requests made by this agent",
		Long: `List secret requests sent by this agent (agent mode only).

By default only pending requests are listed. Pass --fulfilled, --withdrawn,
--dismissed, --expired, or --all to view other states. Secret values are never
included in the output.

Examples:
  scion secret request list
  scion secret request list --fulfilled
  scion secret request list --all
  scion secret request list --all --json`,
		Args: cobra.NoArgs,
		RunE: runSecretRequestList,
	}
	cmd.Flags().Bool("pending", false, "List pending secret requests only (default)")
	cmd.Flags().Bool("fulfilled", false, "List fulfilled secret requests only")
	cmd.Flags().Bool("withdrawn", false, "List withdrawn secret requests only")
	cmd.Flags().Bool("dismissed", false, "List dismissed secret requests only")
	cmd.Flags().Bool("expired", false, "List expired secret requests only")
	cmd.Flags().Bool("all", false, "List secret requests in all states")
	cmd.Flags().Bool("json", false, "Output in JSON format")
	return cmd
}

func newSecretRequestWithdrawCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "withdraw <key>...",
		Short: "Withdraw pending secret request(s) that are no longer needed",
		Long: `Withdraw one or more pending secret requests made by this agent (agent mode only).

Withdrawal is all-or-nothing over the named keys' pending versions. Repeating a
withdrawal on an already-withdrawn key is an idempotent no-op reported as
"already withdrawn". Keys that are already fulfilled, dismissed, expired, or
currently being fulfilled cannot be withdrawn.

Pass --set to also withdraw every other pending secret request on each named
key's card.

Examples:
  scion secret request withdraw GITHUB_TOKEN --reason "no longer needed"
  scion secret request withdraw GITHUB_TOKEN DEPLOY_KEY
  scion secret request withdraw GITHUB_TOKEN --set --reason "switched approach"
  scion secret request withdraw GITHUB_TOKEN --json`,
		RunE: runSecretRequestWithdraw,
	}
	cmd.Flags().Bool("set", false, "Also withdraw every other pending secret request on each key's card")
	cmd.Flags().String("reason", "", fmt.Sprintf("Optional plain-text reason why the secret is no longer needed (at most %d characters)", msgaction.MaxReasonRunes))
	cmd.Flags().Bool("json", false, "Output in JSON format")
	return cmd
}

func secretRequestsWantJSON(cmd *cobra.Command) bool {
	if isJSONOutput() {
		return true
	}
	v, err := cmd.Flags().GetBool("json")
	return err == nil && v
}

func resolveSecretRequestListState(cmd *cobra.Command) (string, error) {
	pending, _ := cmd.Flags().GetBool("pending")
	fulfilled, _ := cmd.Flags().GetBool("fulfilled")
	withdrawn, _ := cmd.Flags().GetBool("withdrawn")
	dismissed, _ := cmd.Flags().GetBool("dismissed")
	expired, _ := cmd.Flags().GetBool("expired")
	all, _ := cmd.Flags().GetBool("all")

	setCount := 0
	for _, v := range []bool{pending, fulfilled, withdrawn, dismissed, expired, all} {
		if v {
			setCount++
		}
	}
	if setCount > 1 {
		return "", newUsageError("pass at most one of --pending, --fulfilled, --withdrawn, --dismissed, --expired, or --all")
	}
	switch {
	case fulfilled:
		return "fulfilled", nil
	case withdrawn:
		return "withdrawn", nil
	case dismissed:
		return "dismissed", nil
	case expired:
		return "expired", nil
	case all:
		return "all", nil
	default:
		return "pending", nil
	}
}

func runSecretRequestList(cmd *cobra.Command, _ []string) error {
	if resolveMode() != ModeAgent {
		return errors.New(errMsgSecretRequestAgentsOnly)
	}
	state, err := resolveSecretRequestListState(cmd)
	if err != nil {
		return err
	}

	client, err := secretRequestsClientFactory()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

	items := make([]messages.SecretRequestView, 0)
	cursor := ""
	for {
		page, err := client.Secrets().ListRequests(ctx, &hubclient.ListAgentSecretRequestsOptions{
			State:  state,
			Cursor: cursor,
			Limit:  100,
		})
		if err != nil {
			return formatSecretRequestAPIError(err)
		}
		items = append(items, page.Items...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}

	if secretRequestsWantJSON(cmd) {
		return writeJSONTo(cmd.OutOrStdout(), messages.AgentSecretRequestListResponse{
			Items: items,
		})
	}

	if len(items) == 0 {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "No %s secret requests.\n", state)
		return nil
	}

	writeSecretRequestsListTable(cmd.OutOrStdout(), items)
	return nil
}

func secretRequestResponderDisplay(name, email, fallback string) string {
	if s := collapseSingleLine(name); s != "" {
		return s
	}
	if s := collapseSingleLine(email); s != "" {
		return s
	}
	if s := collapseSingleLine(fallback); s != "" {
		return s
	}
	return "-"
}

func writeSecretRequestsListTable(out io.Writer, items []messages.SecretRequestView) {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "KEY\tVER\tTYPE\tSTATUS\tDETAIL\tBY\tMESSAGE\tAGE")
	for _, it := range items {
		secretType := safeText(it.SecretType)
		if secretType == "" {
			secretType = messages.SecretRequestTypeEnvironment
		}
		status := safeText(it.Status)
		if it.Submitting {
			status = "submitting"
		}
		detail := "-"
		by := "-"
		switch {
		case it.Fulfilled != nil:
			detail = "fulfilled " + clitime.Format(it.Fulfilled.At, clitime.Minute)
			if it.Fulfilled.SecretVersion > 0 {
				detail += fmt.Sprintf(" (secret v%d)", it.Fulfilled.SecretVersion)
			}
			by = secretRequestResponderDisplay(it.Fulfilled.ByName, it.Fulfilled.ByEmail, it.Fulfilled.ByUserID)
		case it.Dismissed != nil:
			detail = "dismissed " + clitime.Format(it.Dismissed.At, clitime.Minute)
			if r := collapseSingleLine(it.Dismissed.Reason); r != "" {
				detail += ": " + truncateRunes(r, secretRequestDetailMaxWidth, true)
			}
			by = secretRequestResponderDisplay(it.Dismissed.ByName, it.Dismissed.ByEmail, it.Dismissed.ByUserID)
		case it.Withdrawn != nil:
			detail = "withdrawn " + clitime.Format(it.Withdrawn.At, clitime.Minute)
			if r := collapseSingleLine(it.Withdrawn.Reason); r != "" {
				detail += ": " + truncateRunes(r, secretRequestDetailMaxWidth, true)
			}
			by = secretRequestResponderDisplay(it.Withdrawn.ByName, "", it.Withdrawn.By)
		case it.ReplacedBy != nil:
			detail = fmt.Sprintf("replaced by v%d · %s", it.ReplacedBy.Version, clitime.Format(it.ReplacedBy.At, clitime.Minute))
		case it.Expired != nil:
			detail = "expired " + clitime.Format(it.Expired.At, clitime.Minute)
		case it.Description != "":
			detail = truncateRunes(collapseSingleLine(it.Description), secretRequestDetailMaxWidth, true)
		}
		msgID := safeText(it.MessageID)
		if msgID == "" {
			msgID = "-"
		}
		age := clitime.Ago(it.CreatedAt)
		_, _ = fmt.Fprintf(tw, "%s\tv%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
			safeText(it.Key),
			it.Version,
			secretType,
			status,
			detail,
			by,
			msgID,
			age,
		)
	}
	_ = tw.Flush()
}

func parseSecretRequestWithdrawArgs(args []string) ([]string, error) {
	if len(args) == 0 {
		return nil, newUsageError("provide at least one secret request <key>")
	}
	if len(args) > messages.MaxSecretRequestsPerMessage {
		return nil, newUsageError("at most %d secret request keys may be withdrawn at once", messages.MaxSecretRequestsPerMessage)
	}
	seenNorms := make(map[string]bool, len(args))
	keys := make([]string, 0, len(args))
	for _, raw := range args {
		k := strings.TrimSpace(raw)
		if k == "" {
			return nil, newUsageError("secret request <key> must be non-empty")
		}
		norm := messages.SecretRequestKeyNorm(k)
		if seenNorms[norm] {
			return nil, newUsageError("duplicate secret request key %q", k)
		}
		seenNorms[norm] = true
		keys = append(keys, k)
	}
	return keys, nil
}

func runSecretRequestWithdraw(cmd *cobra.Command, args []string) error {
	if resolveMode() != ModeAgent {
		return errors.New(errMsgSecretRequestAgentsOnly)
	}

	keys, err := parseSecretRequestWithdrawArgs(args)
	if err != nil {
		return err
	}

	wholeSet, _ := cmd.Flags().GetBool("set")
	rawReason, _ := cmd.Flags().GetString("reason")
	reason := strings.TrimSpace(rawReason)
	if utf8.RuneCountInString(reason) > msgaction.MaxReasonRunes {
		return newUsageError("--reason must be at most %d characters", msgaction.MaxReasonRunes)
	}

	client, err := secretRequestsClientFactory()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

	resp, err := client.Secrets().WithdrawRequests(ctx, &hubclient.WithdrawSecretRequestsRequest{
		Keys:     keys,
		WholeSet: wholeSet,
		Reason:   reason,
	})
	if err != nil {
		return formatSecretRequestAPIError(err)
	}

	if secretRequestsWantJSON(cmd) {
		return writeJSONTo(cmd.OutOrStdout(), resp)
	}

	writeSecretRequestsWithdrawResult(cmd.OutOrStdout(), keys, resp)
	return nil
}

func writeSecretRequestsWithdrawResult(out io.Writer, requestedKeys []string, resp *messages.WithdrawSecretRequestsResponse) {
	type outcome struct {
		view  messages.SecretRequestView
		label string
	}
	byNorm := make(map[string]outcome, len(resp.Withdrawn)+len(resp.AlreadyWithdrawn))
	ordered := make([]outcome, 0, len(resp.Withdrawn)+len(resp.AlreadyWithdrawn))
	for _, r := range resp.Withdrawn {
		o := outcome{view: r, label: "withdrawn"}
		byNorm[messages.SecretRequestKeyNorm(r.Key)] = o
		ordered = append(ordered, o)
	}
	for _, r := range resp.AlreadyWithdrawn {
		o := outcome{view: r, label: "already withdrawn"}
		byNorm[messages.SecretRequestKeyNorm(r.Key)] = o
		ordered = append(ordered, o)
	}

	printed := make(map[string]bool, len(ordered))
	for _, rawKey := range requestedKeys {
		norm := messages.SecretRequestKeyNorm(rawKey)
		if o, ok := byNorm[norm]; ok && !printed[norm] {
			printed[norm] = true
			_, _ = fmt.Fprintf(out, "%s v%d %s\n", safeText(o.view.Key), o.view.Version, o.label)
		}
	}
	for _, o := range ordered {
		norm := messages.SecretRequestKeyNorm(o.view.Key)
		if !printed[norm] {
			printed[norm] = true
			_, _ = fmt.Fprintf(out, "%s v%d %s\n", safeText(o.view.Key), o.view.Version, o.label)
		}
	}
}

func formatSecretRequestAPIError(err error) error {
	if err == nil {
		return nil
	}
	var apiErr *apiclient.APIError
	if !errors.As(err, &apiErr) {
		return err
	}
	if apiErr.Code == "experiment_disabled" {
		if msg := strings.TrimSpace(apiErr.Message); msg != "" {
			return errors.New(safeText(msg))
		}
		return errors.New(errMsgSecretRequestsExperimentRequired)
	}
	if apiErr.StatusCode == http.StatusNotFound {
		return errors.New(errMsgSecretRequestsExperimentRequired)
	}
	if rawList, ok := apiErr.Details["failures"].([]interface{}); ok && len(rawList) > 0 {
		var lines []string
		for _, raw := range rawList {
			entry, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			if line := formatSecretRequestWithdrawFailure(entry); line != "" {
				lines = append(lines, line)
			}
		}
		if len(lines) > 0 {
			return errors.New(strings.Join(lines, "\n"))
		}
	}
	if strings.TrimSpace(apiErr.Message) != "" {
		return errors.New(safeText(apiErr.Message))
	}
	return err
}

func formatSecretRequestWithdrawFailure(entry map[string]interface{}) string {
	if msg, ok := entry["message"].(string); ok && strings.TrimSpace(msg) != "" {
		return safeText(msg)
	}
	key, _ := entry["key"].(string)
	reason, _ := entry["reason"].(string)
	if key == "" {
		return ""
	}
	switch reason {
	case msgaction.ConflictAlreadyFulfilled:
		if sr, ok := entry["secretRequest"].(map[string]interface{}); ok {
			if ful, ok := sr["fulfilled"].(map[string]interface{}); ok {
				who, _ := ful["byName"].(string)
				if who == "" {
					who, _ = ful["byEmail"].(string)
				}
				if who != "" {
					return fmt.Sprintf("%s: already fulfilled by %s", safeText(key), safeText(who))
				}
			}
		}
		return fmt.Sprintf("%s: already fulfilled", safeText(key))
	case msgaction.ConflictDismissed:
		if sr, ok := entry["secretRequest"].(map[string]interface{}); ok {
			if dsm, ok := sr["dismissed"].(map[string]interface{}); ok {
				if who, _ := dsm["byName"].(string); who != "" {
					return fmt.Sprintf("%s: dismissed by %s", safeText(key), safeText(who))
				}
			}
		}
		return fmt.Sprintf("%s: dismissed", safeText(key))
	case msgaction.ConflictExpired:
		return fmt.Sprintf("%s: expired", safeText(key))
	case msgaction.ConflictSubmitting:
		return fmt.Sprintf("%s: currently being fulfilled", safeText(key))
	case "not_found":
		return fmt.Sprintf("%s: secret request not found", safeText(key))
	default:
		if reason != "" {
			return fmt.Sprintf("%s: %s", safeText(key), safeText(reason))
		}
		return safeText(key)
	}
}
