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
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/spf13/cobra"
)

func newMessagePolicyUpdateCmd() *cobra.Command {
	var (
		specFile          string
		name              string
		description       string
		mode              string
		enabled           bool
		surfaces          []string
		senderAgentIDs    []string
		allAgents         bool
		recipientKinds    []string
		rule              string
		ruleFile          string
		systemOneRule     string
		systemOneRuleFile string
		onUncertain       string
		onFailed          string
		onFailedReasons   []string
		rejectReason      string
		rejectRemediation string
		expectedRevision  int64
	)

	cmd := &cobra.Command{
		Use:   "update <id-or-name>",
		Short: "Update an existing message policy for the project",
		Long: `Update an existing project message policy by UUID or slug name.

Only specified flags (or fields present in --file) are modified; omitted fields
preserve their stored values. Every update increments the policy's revision.
Use --expected-revision for optimistic concurrency control.

Requires project owner/admin or Hub admin permissions (human mode only).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			policyRef := strings.TrimSpace(args[0])
			if policyRef == "" {
				return newUsageError("policy id or name is required")
			}
			if rule != "" && ruleFile != "" {
				return newUsageError("--rule and --rule-file are mutually exclusive")
			}
			if systemOneRule != "" && systemOneRuleFile != "" {
				return newUsageError("--system-one-rule and --system-one-rule-file are mutually exclusive")
			}

			var req hubclient.PatchMessagePolicyRequest
			if specFile != "" {
				raw, err := readBoundedInput(cmd, specFile, maxMessagePolicySpecFileBytes, "policy patch file")
				if err != nil {
					return err
				}
				if err := decodeYAMLOrJSONStrict(raw, &req, "policy patch file"); err != nil {
					return err
				}
			}

			if cmd.Flags().Changed("name") {
				v := strings.TrimSpace(name)
				req.Name = &v
			}
			if cmd.Flags().Changed("description") {
				v := description
				req.Description = &v
			}
			if cmd.Flags().Changed("mode") {
				v := strings.TrimSpace(mode)
				req.Mode = &v
			}
			if cmd.Flags().Changed("enabled") {
				v := enabled
				req.Enabled = &v
			}
			if cmd.Flags().Changed("surface") || cmd.Flags().Changed("sender-agent") || cmd.Flags().Changed("all-agents") || cmd.Flags().Changed("recipient-kind") {
				if req.Scope == nil {
					req.Scope = &store.MessagePolicyScope{}
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
			}
			if ruleFile != "" {
				rawRule, err := readBoundedInput(cmd, ruleFile, maxMessagePolicySpecFileBytes, "rule file")
				if err != nil {
					return err
				}
				v := strings.TrimSpace(string(rawRule))
				req.ViolationRule = &v
			} else if cmd.Flags().Changed("rule") {
				v := rule
				req.ViolationRule = &v
			}
			if systemOneRuleFile != "" {
				rawS1, err := readBoundedInput(cmd, systemOneRuleFile, maxMessagePolicySpecFileBytes, "system-one rule file")
				if err != nil {
					return err
				}
				v := strings.TrimSpace(string(rawS1))
				req.SystemOneRule = &v
			} else if cmd.Flags().Changed("system-one-rule") {
				v := systemOneRule
				req.SystemOneRule = &v
			}
			if cmd.Flags().Changed("on-uncertain") {
				v := strings.TrimSpace(onUncertain)
				req.OnUncertain = &v
			}
			if cmd.Flags().Changed("on-failed") {
				v := strings.TrimSpace(onFailed)
				req.OnFailed = &v
			}
			if cmd.Flags().Changed("on-failed-reason") {
				parsed, err := parseKeyValuePairs(onFailedReasons, "--on-failed-reason")
				if err != nil {
					return err
				}
				req.OnFailedReasons = &parsed
			}
			if cmd.Flags().Changed("reject-reason") {
				v := rejectReason
				req.RejectReason = &v
			}
			if cmd.Flags().Changed("reject-remediation") {
				v := rejectRemediation
				req.RejectRemediation = &v
			}
			if cmd.Flags().Changed("expected-revision") {
				req.ExpectedRevision = expectedRevision
			}

			svc, ctx, cancel, err := messagePolicyServiceForCmd(cmd)
			if err != nil {
				return err
			}
			defer cancel()

			updated, err := svc.Update(ctx, policyRef, &req)
			if err != nil {
				return formatMessagePolicyAPIError(err)
			}

			if messagePolicyWantJSON(cmd) {
				return writeJSONTo(cmd.OutOrStdout(), updated)
			}

			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Updated message policy %q (id: %s, mode: %s, revision: %d).\n\n",
				safeText(updated.Name), safeText(updated.ID), safeText(updated.Mode), updated.Revision)
			writeMessagePolicyDetail(cmd.OutOrStdout(), updated)
			return nil
		},
	}

	cmd.Flags().StringVarP(&specFile, "file", "f", "", "YAML or JSON policy patch file (- for stdin)")
	cmd.Flags().StringVar(&name, "name", "", "New canonical policy slug name")
	cmd.Flags().StringVar(&description, "description", "", "Human-readable policy description")
	cmd.Flags().StringVar(&mode, "mode", "", "Enforcement mode: shadow or enforce")
	cmd.Flags().BoolVar(&enabled, "enabled", true, "Whether the policy is active")
	cmd.Flags().StringSliceVar(&surfaces, "surface", nil, "Covered surface(s) to evaluate on this Hub")
	cmd.Flags().StringSliceVar(&senderAgentIDs, "sender-agent", nil, "Restrict policy to specific project sender agent UUID(s)")
	cmd.Flags().BoolVar(&allAgents, "all-agents", false, "Match all sender agents in the project")
	cmd.Flags().StringSliceVar(&recipientKinds, "recipient-kind", nil, "Restrict recipient kind(s): user, conversation")
	cmd.Flags().StringVar(&rule, "rule", "", "CEL boolean violation expression (true means violation)")
	cmd.Flags().StringVar(&ruleFile, "rule-file", "", "Read CEL boolean violation expression from file (- for stdin)")
	cmd.Flags().StringVar(&systemOneRule, "system-one-rule", "", "System One judgments rule JSON (<= 16 KiB; pass empty string to clear)")
	cmd.Flags().StringVar(&systemOneRuleFile, "system-one-rule-file", "", "Read System One judgments rule JSON from file (- for stdin)")
	cmd.Flags().StringVar(&onUncertain, "on-uncertain", "", "Fallback outcome on uncertain evaluation: admit or reject")
	cmd.Flags().StringVar(&onFailed, "on-failed", "", "Fallback outcome on failed evaluation: admit or reject")
	cmd.Flags().StringArrayVar(&onFailedReasons, "on-failed-reason", nil, "Per-reason failure fallback override REASON=OUTCOME (repeatable)")
	cmd.Flags().StringVar(&rejectReason, "reject-reason", "", "Rejection reason template returned on HTTP 422")
	cmd.Flags().StringVar(&rejectRemediation, "reject-remediation", "", "Advisory remediation template returned on HTTP 422")
	cmd.Flags().Int64Var(&expectedRevision, "expected-revision", 0, "Require stored policy revision to match before updating")
	cmd.Flags().Bool("json", false, "Output in JSON format")

	return cmd
}
