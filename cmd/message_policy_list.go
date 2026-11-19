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
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

func newMessagePolicyListCmd() *cobra.Command {
	var (
		mode    string
		enabled string
		limit   int
		cursor  string
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List CEL message policies for the project",
		Long: `List message policies configured on the target project, along with this Hub's
enforcement coverage declaration (enforced_surfaces and uncovered_agent_paths).

Available in both human mode and agent mode (read-only).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts := &hubclient.ListMessagePoliciesOptions{
				Mode:   strings.TrimSpace(mode),
				Limit:  limit,
				Cursor: strings.TrimSpace(cursor),
			}
			if trimmed := strings.TrimSpace(enabled); trimmed != "" {
				parsed, err := strconv.ParseBool(trimmed)
				if err != nil {
					return newUsageError("invalid --enabled value %q: must be true or false", enabled)
				}
				opts.Enabled = &parsed
			}

			svc, ctx, cancel, err := messagePolicyServiceForCmd(cmd)
			if err != nil {
				return err
			}
			defer cancel()

			resp, err := svc.List(ctx, opts)
			if err != nil {
				return formatMessagePolicyAPIError(err)
			}

			if messagePolicyWantJSON(cmd) {
				return writeJSONTo(cmd.OutOrStdout(), resp)
			}

			out := cmd.OutOrStdout()
			if len(resp.Policies) == 0 {
				_, _ = fmt.Fprintln(out, "No message policies configured for this project.")
				_, _ = fmt.Fprintln(out)
				writeMessagePolicyCoverage(out, resp.Coverage)
				return nil
			}

			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(tw, "NAME\tID\tMODE\tENABLED\tSURFACES\tON FAILED\tREV\tUPDATED")
			for _, pol := range resp.Policies {
				updated := "-"
				if !pol.UpdatedAt.IsZero() {
					updated = clitime.Ago(pol.UpdatedAt)
				}
				_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%t\t%s\t%s\t%d\t%s\n",
					safeText(pol.Name),
					safeText(pol.ID),
					safeText(pol.Mode),
					pol.Enabled,
					safeListOrNone(pol.Scope.Surfaces),
					safeText(pol.OnFailed),
					pol.Revision,
					updated,
				)
			}
			_ = tw.Flush()

			if resp.NextCursor != "" {
				_, _ = fmt.Fprintf(out, "\nNext cursor: %s\n", safeText(resp.NextCursor))
			}
			_, _ = fmt.Fprintln(out)
			writeMessagePolicyCoverage(out, resp.Coverage)
			return nil
		},
	}

	cmd.Flags().StringVar(&mode, "mode", "", "Filter by mode: shadow or enforce")
	cmd.Flags().StringVar(&enabled, "enabled", "", "Filter by enabled state: true or false")
	cmd.Flags().IntVar(&limit, "limit", 0, "Maximum number of policies to return (1..200)")
	cmd.Flags().StringVar(&cursor, "cursor", "", "Pagination cursor from a previous list response")
	cmd.Flags().Bool("json", false, "Output in JSON format")

	return cmd
}
