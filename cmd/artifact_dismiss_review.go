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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

var artifactDismissReviewNote string

var artifactDismissReviewCmd = &cobra.Command{
	Use:   "dismiss-review <ref>",
	Short: "Dismiss a review request addressed to you",
	Long: `Dismiss an open review request addressed to you on an artifact (requires the hub.artifacts_review_requests experiment).

Optionally include a --note explaining why you are declining; the note is sent
to the requester (and artifact owner if different).

Examples:
  scion artifact dismiss-review scion://artifact/5f1c2d3e-...
  scion artifact dismiss-review scion://artifact/5f1c2d3e-... --note "Out of office this week"`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		_, client, _, err := requireArtifactHubClient()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
		defer cancel()
		return dismissArtifactReview(ctx, client.Artifacts(), cmd.OutOrStdout(), args[0], dismissReviewOptions{
			Note: artifactDismissReviewNote,
			JSON: isJSONOutput(),
		})
	},
}

func init() {
	artifactDismissReviewCmd.Flags().StringVar(&artifactDismissReviewNote, "note", "", "Optional message for the requester (at most 1000 characters)")
	artifactCmd.AddCommand(artifactDismissReviewCmd)
}

type dismissReviewOptions struct {
	Note string
	JSON bool
}

func dismissArtifactReview(ctx context.Context, svc hubclient.ArtifactService, out io.Writer, ref string, opts dismissReviewOptions) error {
	id, _, err := artifacts.ParseRef(ref)
	if err != nil {
		return err
	}
	resp, err := svc.DismissReviewRequest(ctx, id, opts.Note)
	if err != nil {
		return fmt.Errorf("dismiss review: %w%s", err, artifactErrorHint(err, false))
	}
	if opts.JSON {
		return writeJSONTo(out, resp)
	}
	if resp.AuthorNotified {
		_, _ = fmt.Fprintf(out, "Dismissed review request on %s (author notified)\n", artifacts.FormatRef(id, 0))
	} else {
		_, _ = fmt.Fprintf(out, "Dismissed review request on %s\n", artifacts.FormatRef(id, 0))
	}
	return nil
}
