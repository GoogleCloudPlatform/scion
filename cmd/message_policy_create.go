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
	"github.com/spf13/cobra"
)

func newMessagePolicyCreateCmd() *cobra.Command {
	var (
		specFile          string
		description       string
		mode              string
		enabled           bool
		surfaces          []string
		senderAgentIDs    []string
		allAgents         bool
		recipientKinds    []string
		rule              string
		ruleFile          string
		onUncertain       string
		onFailed          string
		onFailedReasons   []string
		rejectReason      string
		rejectRemediation string
	)

	cmd := &cobra.Command{
		Use:   "create [name]",
		Short: "Create a CEL message policy for the project",
		Long: `Create a CEL message policy for the target project.

Message policies evaluate ONLY outbound agent sends on surfaces enforced by
this Hub (reported under 'Coverage (this Hub)' by 'scion message-policy list',
currently agent_to_user and agent_to_conversation). Uncovered agent paths,
broadcasts, scheduled deliveries, user-originated sends, and inbound external
bridges are NOT evaluated. Supplying an uncovered surface returns 400
surface_not_covered.

You may supply policy fields directly with flags or load a YAML/JSON spec file
with --file (- for stdin) and optionally override fields via flags.

Examples:
  # Create a shadow policy flagging prose choices in agent-to-user messages
  scion message-policy create no-prose-choices \
    --surface agent_to_user \
    --mode shadow \
    --on-failed admit \
    --rule '!message.has_choices && message.body.matches("(?i)option\\s+[ab12]")' \
    --reject-reason 'Present discrete choices using structured options instead of prose.' \
    --reject-remediation 'Revise the message to avoid prose option menus.'

  # Create a policy from a YAML or JSON specification file
  scion message-policy create --file policy.yaml`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if rule != "" && ruleFile != "" {
				return newUsageError("--rule and --rule-file are mutually exclusive")
			}

			var req hubclient.CreateMessagePolicyRequest
			if specFile != "" {
				raw, err := readBoundedInput(cmd, specFile, maxMessagePolicySpecFileBytes, "policy file")
				if err != nil {
					return err
				}
				if err := decodeYAMLOrJSONStrict(raw, &req, "policy file"); err != nil {
					return err
				}
			}

			if len(args) > 0 {
				req.Name = strings.TrimSpace(args[0])
			}
			if cmd.Flags().Changed("description") {
				req.Description = description
			}
			if cmd.Flags().Changed("mode") {
				req.Mode = strings.TrimSpace(mode)
			}
			if cmd.Flags().Changed("enabled") {
				enabledVal := enabled
				req.Enabled = &enabledVal
			}
			if cmd.Flags().Changed("surface") {
				req.Scope.Surfaces = cleanStringSlice(surfaces)
			}
			if cmd.Flags().Changed("sender-agent") {
				req.Scope.Sender.AgentIDs = cleanStringSlice(senderAgentIDs)
			}
			if cmd.Flags().Changed("all-agents") {
				req.Scope.Sender.AllAgents = allAgents
			}
			if cmd.Flags().Changed("recipient-kind") {
				req.Scope.Recipient.Kinds = cleanStringSlice(recipientKinds)
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

			if strings.TrimSpace(req.Name) == "" {
				return newUsageError("policy name is required (pass <name> or set name in --file)")
			}
			if len(req.Scope.Surfaces) == 0 {
				return newUsageError("at least one --surface is required (e.g. --surface agent_to_user)")
			}
			if strings.TrimSpace(req.ViolationRule) == "" {
				return newUsageError("a CEL violation rule is required (pass --rule, --rule-file, or violation_rule in --file)")
			}
			if strings.TrimSpace(req.OnFailed) == "" {
				return newUsageError("--on-failed is required (admit or reject)")
			}

			svc, ctx, cancel, err := messagePolicyServiceForCmd(cmd)
			if err != nil {
				return err
			}
			defer cancel()

			created, err := svc.Create(ctx, &req)
			if err != nil {
				return formatMessagePolicyAPIError(err)
			}

			if messagePolicyWantJSON(cmd) {
				return writeJSONTo(cmd.OutOrStdout(), created)
			}

			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Created message policy %q (id: %s, mode: %s, revision: %d).\n\n",
				safeText(created.Name), safeText(created.ID), safeText(created.Mode), created.Revision)
			writeMessagePolicyDetail(cmd.OutOrStdout(), created)
			return nil
		},
	}

	cmd.Flags().StringVarP(&specFile, "file", "f", "", "YAML or JSON policy specification file (- for stdin)")
	cmd.Flags().StringVar(&description, "description", "", "Human-readable policy description")
	cmd.Flags().StringVar(&mode, "mode", "", "Enforcement mode: shadow (default) or enforce")
	cmd.Flags().BoolVar(&enabled, "enabled", true, "Whether the policy is active")
	cmd.Flags().StringSliceVar(&surfaces, "surface", nil, "Covered surface(s) to evaluate on this Hub (e.g. agent_to_user, agent_to_conversation)")
	cmd.Flags().StringSliceVar(&senderAgentIDs, "sender-agent", nil, "Restrict policy to specific project sender agent UUID(s)")
	cmd.Flags().BoolVar(&allAgents, "all-agents", false, "Match all sender agents in the project")
	cmd.Flags().StringSliceVar(&recipientKinds, "recipient-kind", nil, "Restrict recipient kind(s): user, conversation")
	cmd.Flags().StringVar(&rule, "rule", "", "CEL boolean violation expression (true means violation)")
	cmd.Flags().StringVar(&ruleFile, "rule-file", "", "Read CEL boolean violation expression from file (- for stdin)")
	cmd.Flags().StringVar(&onUncertain, "on-uncertain", "", "Fallback outcome on uncertain evaluation: admit or reject")
	cmd.Flags().StringVar(&onFailed, "on-failed", "", "Fallback outcome on failed evaluation: admit or reject")
	cmd.Flags().StringArrayVar(&onFailedReasons, "on-failed-reason", nil, "Per-reason failure fallback override REASON=OUTCOME (repeatable)")
	cmd.Flags().StringVar(&rejectReason, "reject-reason", "", "Rejection reason template returned on HTTP 422")
	cmd.Flags().StringVar(&rejectRemediation, "reject-remediation", "", "Advisory remediation template returned on HTTP 422")
	cmd.Flags().Bool("json", false, "Output in JSON format")

	return cmd
}

func cleanStringSlice(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if trimmed := strings.TrimSpace(s); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
