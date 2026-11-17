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
	"strings"

	"github.com/spf13/cobra"
)

func newMessagePolicyGetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get <id-or-name>",
		Short: "Get details of a project message policy",
		Long: `Inspect a project message policy by UUID or canonical policy name, including
its CEL violation rule, fallback behavior, and explicit M1 enforcement coverage
(agent_to_user and agent_to_conversation only).

Available in both human mode and agent mode (read-only).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref := strings.TrimSpace(args[0])
			if ref == "" {
				return newUsageError("policy ID or name is required")
			}

			svc, ctx, cancel, err := messagePolicyServiceForCmd(cmd)
			if err != nil {
				return err
			}
			defer cancel()

			pol, err := svc.Get(ctx, ref)
			if err != nil {
				return formatMessagePolicyAPIError(err)
			}

			if messagePolicyWantJSON(cmd) {
				return writeJSONTo(cmd.OutOrStdout(), pol)
			}

			writeMessagePolicyDetail(cmd.OutOrStdout(), pol)
			return nil
		},
	}

	cmd.Flags().Bool("json", false, "Output in JSON format")
	return cmd
}
