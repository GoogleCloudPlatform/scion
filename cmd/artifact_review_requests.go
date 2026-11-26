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
	"text/tabwriter"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

var artifactReviewRequestsJSON bool

var artifactReviewRequestsCmd = &cobra.Command{
	Use:   "review-requests <ref>",
	Short: "List review requests on an artifact",
	Long: `List review requests on an artifact (requires the hub.artifacts_review_requests experiment).

Publishers see every open request and up to 50 most recent closed requests.
Other readers see only requests addressed to themselves.

Examples:
  scion artifact review-requests scion://artifact/5f1c2d3e-...
  scion artifact review-requests scion://artifact/5f1c2d3e-... --json`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		_, client, _, err := requireArtifactHubClient()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
		defer cancel()
		return listArtifactReviewRequests(ctx, client.Artifacts(), cmd.OutOrStdout(), args[0], artifactReviewRequestsJSON || isJSONOutput())
	},
}

func init() {
	artifactReviewRequestsCmd.Flags().BoolVar(&artifactReviewRequestsJSON, "json", false, "Print the raw JSON response")
	artifactCmd.AddCommand(artifactReviewRequestsCmd)
}

func formatReviewRequestPrincipal(p hubclient.ArtifactReviewRequestPrincipal, defaultKind string) string {
	if p.DisplayName != "" {
		return safeText(p.DisplayName)
	}
	kind := p.Kind
	if kind == "" {
		kind = defaultKind
	}
	if kind != "" && p.Ref != "" {
		return safeText(kind + ":" + p.Ref)
	}
	return safeText(p.Ref)
}

func formatReviewRequestState(r hubclient.ArtifactReviewRequest) string {
	parts := []string{safeText(r.State)}
	if r.InDraft {
		parts = append(parts, "in draft")
	}
	if r.ReviewerCanReview != nil && !*r.ReviewerCanReview {
		parts = append(parts, "no access")
	}
	return strings.Join(parts, ", ")
}

func listArtifactReviewRequests(ctx context.Context, svc hubclient.ArtifactService, out io.Writer, ref string, jsonOut bool) error {
	id, _, err := artifacts.ParseRef(ref)
	if err != nil {
		return err
	}
	resp, err := svc.ListReviewRequests(ctx, id)
	if err != nil {
		return fmt.Errorf("list review requests: %w%s", err, artifactErrorHint(err, false))
	}
	if jsonOut {
		if resp.Requests == nil {
			resp.Requests = []hubclient.ArtifactReviewRequest{}
		}
		return writeJSONTo(out, resp)
	}
	if len(resp.Requests) == 0 {
		_, _ = fmt.Fprintf(out, "%s has no review requests\n", artifacts.FormatRef(id, 0))
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "REVIEWER\tSTATE\tREQUESTED\tBY\tNOTE/RESPONSE")
	for _, r := range resp.Requests {
		noteText := r.Note
		if r.ResponseNote != "" {
			noteText = r.ResponseNote
		}
		requested := fmt.Sprintf("v%d", r.RequestedSeq)
		if r.ResolvedSeq != nil {
			requested = fmt.Sprintf("v%d (v%d)", r.RequestedSeq, *r.ResolvedSeq)
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			formatReviewRequestPrincipal(r.Reviewer, artifacts.PrincipalKindUser),
			formatReviewRequestState(r),
			requested,
			formatReviewRequestPrincipal(r.Requester, ""),
			safeText(noteText),
		)
	}
	return tw.Flush()
}
