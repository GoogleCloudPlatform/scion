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

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messagepolicy"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/spf13/cobra"
)

func newMessagePolicyTestCmd() *cobra.Command {
	var (
		reqFile           string
		body              string
		bodyFile          string
		surface           string
		msgType           string
		hasChoices        bool
		urgent            bool
		attachmentCount   int
		senderID          string
		senderSlug        string
		recipientKind     string
		recipientID       string
		recipientSlug     string
		convKind          string
		inlineName        string
		inlineMode        string
		inlineSurfaces    []string
		rule              string
		ruleFile          string
		onUncertain       string
		onFailed          string
		onFailedReasons   []string
		rejectReason      string
		rejectRemediation string
	)

	cmd := &cobra.Command{
		Use:   "test [id-or-name]",
		Short: "Dry-run a stored or inline message policy against a synthetic sample",
		Long: `Evaluate a stored project message policy (by UUID or name) or an inline CEL
rule against synthetic message facts without storing any policy, message, or
decision row.

The sample body is evaluated in-memory on the Hub and is never persisted or
echoed in the response. Dry-run testing evaluates surfaces enforced by this Hub
(when --surface is omitted, defaults to the policy's first surface or
agent_to_user).

Examples:
  # Dry-run a stored policy against a sample message body
  scion message-policy test no-prose-choices \
    --surface agent_to_user \
    --body "Would you like Option A or Option B?"

  # Dry-run an inline CEL rule before creating a policy
  scion message-policy test \
    --rule '!message.has_choices && message.body.contains("Option A")' \
    --on-failed reject \
    --mode enforce \
    --surface agent_to_user \
    --body "Option A or Option B?"`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if body != "" && bodyFile != "" {
				return newUsageError("--body and --body-file are mutually exclusive")
			}
			if rule != "" && ruleFile != "" {
				return newUsageError("--rule and --rule-file are mutually exclusive")
			}

			policyRef := ""
			if len(args) > 0 {
				policyRef = strings.TrimSpace(args[0])
			}

			var req hubclient.TestMessagePolicyRequest
			if reqFile != "" {
				raw, err := readBoundedInput(cmd, reqFile, maxMessagePolicyTestInputBytes, "test request file")
				if err != nil {
					return err
				}
				if err := decodeYAMLOrJSONStrict(raw, &req, "test request file"); err != nil {
					return err
				}
				if req.Policy != nil {
					if changed := changedInlinePolicyFlags(cmd, ruleFile); len(changed) > 0 {
						return newUsageError("cannot combine inline policy override flags (%s) with --file containing a nested 'policy' block", strings.Join(changed, ", "))
					}
					if hasTopLevelTestPolicyFields(&req) {
						return newUsageError("test request file cannot set both nested 'policy' and top-level policy fields")
					}
				}
			}

			sample := req.Sample
			if sample == nil {
				sample = &hubclient.MessagePolicyTestSample{
					Surface: req.Surface,
					Body:    req.Body,
				}
			}
			req.Surface = ""
			req.Body = ""

			if bodyFile != "" {
				rawBody, err := readBoundedInput(cmd, bodyFile, maxMessagePolicyTestInputBytes, "sample body file")
				if err != nil {
					return err
				}
				sample.Body = strings.TrimRight(string(rawBody), "\r\n")
			} else if cmd.Flags().Changed("body") {
				sample.Body = body
			}

			if cmd.Flags().Changed("surface") {
				sample.Surface = strings.TrimSpace(surface)
			}
			if cmd.Flags().Changed("type") {
				sample.Type = strings.TrimSpace(msgType)
			}
			if cmd.Flags().Changed("has-choices") {
				sample.HasChoices = hasChoices
			}
			if cmd.Flags().Changed("urgent") {
				sample.Urgent = urgent
			}
			if cmd.Flags().Changed("attachment-count") {
				sample.AttachmentCount = attachmentCount
			}
			if cmd.Flags().Changed("sender-id") || cmd.Flags().Changed("sender-slug") {
				if sample.Sender == nil {
					sample.Sender = &messagepolicy.SenderFacts{}
				}
				if cmd.Flags().Changed("sender-id") {
					sample.Sender.ID = strings.TrimSpace(senderID)
				}
				if cmd.Flags().Changed("sender-slug") {
					sample.Sender.Slug = strings.TrimSpace(senderSlug)
				}
			}
			if cmd.Flags().Changed("recipient-kind") || cmd.Flags().Changed("recipient-id") || cmd.Flags().Changed("recipient-slug") {
				if sample.Recipient == nil {
					sample.Recipient = &messagepolicy.RecipientFacts{}
				}
				if cmd.Flags().Changed("recipient-kind") {
					sample.Recipient.Kind = strings.TrimSpace(recipientKind)
				}
				if cmd.Flags().Changed("recipient-id") {
					sample.Recipient.ID = strings.TrimSpace(recipientID)
				}
				if cmd.Flags().Changed("recipient-slug") {
					sample.Recipient.Slug = strings.TrimSpace(recipientSlug)
				}
			}
			if cmd.Flags().Changed("conv-kind") {
				if sample.Conversation == nil {
					sample.Conversation = &messagepolicy.ConversationFacts{}
				}
				sample.Conversation.Kind = strings.TrimSpace(convKind)
			}
			req.Sample = sample

			// Apply inline/override policy flags.
			if cmd.Flags().Changed("name") {
				req.Name = strings.TrimSpace(inlineName)
			}
			if cmd.Flags().Changed("mode") {
				req.Mode = strings.TrimSpace(inlineMode)
			}
			if cmd.Flags().Changed("policy-surface") {
				if req.Scope == nil {
					req.Scope = &store.MessagePolicyScope{}
				}
				req.Scope.Surfaces = cleanStringSlice(inlineSurfaces)
			}
			if ruleFile != "" {
				rawRule, err := readBoundedInput(cmd, ruleFile, maxMessagePolicySpecFileBytes, "rule file")
				if err != nil {
					return err
				}
				req.ViolationRule = strings.TrimSpace(string(rawRule))
			} else if cmd.Flags().Changed("rule") {
				req.ViolationRule = rule
			}
			if cmd.Flags().Changed("on-uncertain") {
				req.OnUncertain = strings.TrimSpace(onUncertain)
			}
			if cmd.Flags().Changed("on-failed") {
				req.OnFailed = strings.TrimSpace(onFailed)
			}
			if cmd.Flags().Changed("on-failed-reason") {
				parsed, err := parseKeyValuePairs(onFailedReasons, "--on-failed-reason")
				if err != nil {
					return err
				}
				req.OnFailedReasons = parsed
			}
			if cmd.Flags().Changed("reject-reason") {
				req.RejectReason = rejectReason
			}
			if cmd.Flags().Changed("reject-remediation") {
				req.RejectRemediation = rejectRemediation
			}

			if policyRef == "" {
				hasInlineRule := strings.TrimSpace(req.ViolationRule) != "" ||
					(req.Policy != nil && strings.TrimSpace(req.Policy.ViolationRule) != "")
				if !hasInlineRule {
					return newUsageError("provide a stored policy <id-or-name> or an inline --rule / --rule-file / --file")
				}
			}
			if reqFile == "" && !cmd.Flags().Changed("body") && bodyFile == "" {
				return newUsageError("a synthetic sample body is required (--body, --body-file, or --file)")
			}

			svc, ctx, cancel, err := messagePolicyServiceForCmd(cmd)
			if err != nil {
				return err
			}
			defer cancel()

			var res *hubclient.TestMessagePolicyResponse
			if policyRef != "" {
				res, err = svc.TestPolicy(ctx, policyRef, &req)
			} else {
				res, err = svc.Test(ctx, &req)
			}
			if err != nil {
				return formatMessagePolicyAPIError(err)
			}

			if messagePolicyWantJSON(cmd) {
				return writeJSONTo(cmd.OutOrStdout(), res)
			}

			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "Policy:          %s", safeText(res.PolicyName))
			if res.PolicyID != "" {
				_, _ = fmt.Fprintf(out, " (id: %s, rev: %d)", safeText(res.PolicyID), res.PolicyRevision)
			}
			_, _ = fmt.Fprintln(out)
			_, _ = fmt.Fprintf(out, "Mode:            %s\n", safeText(res.Mode))
			_, _ = fmt.Fprintf(out, "Surface:         %s\n", safeText(res.Surface))
			_, _ = fmt.Fprintf(out, "Scope Matched:   %t\n", res.ScopeMatched)
			_, _ = fmt.Fprintf(out, "Class:           %s\n", safeText(res.Class))
			if res.ReasonCode != "" {
				_, _ = fmt.Fprintf(out, "Reason Code:     %s\n", safeText(res.ReasonCode))
			}
			_, _ = fmt.Fprintf(out, "Outcome:         %s\n", safeText(res.Outcome))
			_, _ = fmt.Fprintf(out, "Action:          %s\n", safeText(res.Action))
			if res.Reason != "" {
				_, _ = fmt.Fprintf(out, "Reason:          %s\n", safeText(res.Reason))
			}
			if res.Remediation != "" {
				_, _ = fmt.Fprintf(out, "Remediation:     %s\n", safeText(res.Remediation))
			}
			_, _ = fmt.Fprintf(out, "Body Bytes:      %d (truncated: %t)\n", res.BodyBytes, res.BodyTruncated)
			_, _ = fmt.Fprintf(out, "Elapsed:         %dms\n", res.ElapsedMs)
			return nil
		},
	}

	cmd.Flags().StringVarP(&reqFile, "file", "f", "", "YAML or JSON dry-run request file (- for stdin)")
	cmd.Flags().StringVar(&body, "body", "", "Synthetic sample message body (evaluated in-memory only)")
	cmd.Flags().StringVar(&bodyFile, "body-file", "", "Read synthetic sample message body from file (- for stdin)")
	cmd.Flags().StringVar(&surface, "surface", "", "Sample surface (default: policy's first surface, or agent_to_user)")
	cmd.Flags().StringVar(&msgType, "type", "", "Sample message type (default: message)")
	cmd.Flags().BoolVar(&hasChoices, "has-choices", false, "Set message.has_choices on the synthetic sample")
	cmd.Flags().BoolVar(&urgent, "urgent", false, "Set message.urgent on the synthetic sample")
	cmd.Flags().IntVar(&attachmentCount, "attachment-count", 0, "Set message.attachment_count on the synthetic sample")
	cmd.Flags().StringVar(&senderID, "sender-id", "", "Synthetic sender agent ID")
	cmd.Flags().StringVar(&senderSlug, "sender-slug", "", "Synthetic sender agent slug")
	cmd.Flags().StringVar(&recipientKind, "recipient-kind", "", "Synthetic recipient kind: user or conversation")
	cmd.Flags().StringVar(&recipientID, "recipient-id", "", "Synthetic recipient ID")
	cmd.Flags().StringVar(&recipientSlug, "recipient-slug", "", "Synthetic recipient slug")
	cmd.Flags().StringVar(&convKind, "conv-kind", "", "Synthetic conversation kind")

	// Inline rule / override flags.
	cmd.Flags().StringVar(&inlineName, "name", "", "Inline policy slug name (default: inline-test)")
	cmd.Flags().StringVar(&inlineMode, "mode", "", "Inline or override mode: shadow or enforce")
	cmd.Flags().StringSliceVar(&inlineSurfaces, "policy-surface", nil, "Inline or override policy scope surface(s)")
	cmd.Flags().StringVar(&rule, "rule", "", "Inline or override CEL boolean violation expression")
	cmd.Flags().StringVar(&ruleFile, "rule-file", "", "Read inline or override CEL expression from file")
	cmd.Flags().StringVar(&onUncertain, "on-uncertain", "", "Inline or override fallback outcome on uncertain evaluation")
	cmd.Flags().StringVar(&onFailed, "on-failed", "", "Inline or override fallback outcome on failed evaluation (admit or reject)")
	cmd.Flags().StringArrayVar(&onFailedReasons, "on-failed-reason", nil, "Inline or override per-reason failure fallback REASON=OUTCOME")
	cmd.Flags().StringVar(&rejectReason, "reject-reason", "", "Inline or override rejection reason template")
	cmd.Flags().StringVar(&rejectRemediation, "reject-remediation", "", "Inline or override advisory remediation template")
	cmd.Flags().Bool("json", false, "Output in JSON format")

	return cmd
}

func changedInlinePolicyFlags(cmd *cobra.Command, ruleFile string) []string {
	flagNames := []string{
		"name",
		"mode",
		"policy-surface",
		"rule",
		"rule-file",
		"on-uncertain",
		"on-failed",
		"on-failed-reason",
		"reject-reason",
		"reject-remediation",
	}
	var out []string
	for _, name := range flagNames {
		if cmd.Flags().Changed(name) || (name == "rule-file" && ruleFile != "") {
			out = append(out, "--"+name)
		}
	}
	return out
}

func hasTopLevelTestPolicyFields(req *hubclient.TestMessagePolicyRequest) bool {
	if req == nil {
		return false
	}
	return req.Name != "" ||
		req.Mode != "" ||
		req.Scope != nil ||
		req.ViolationRule != "" ||
		req.SystemOneRule != "" ||
		req.OnUncertain != "" ||
		req.OnFailed != "" ||
		len(req.OnFailedReasons) > 0 ||
		req.RejectReason != "" ||
		req.RejectRemediation != ""
}
