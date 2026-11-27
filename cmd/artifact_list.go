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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

var (
	artifactListMine      bool
	artifactListProject   string
	artifactListShared    bool
	artifactListReviewed  bool
	artifactListRequested bool
	artifactListDrafts    bool
	artifactListQuery     string
	artifactListLimit     int
	artifactListAll       bool
	artifactListCursor    string
)

var artifactListCmd = &cobra.Command{
	Use:   "list",
	Short: "List the artifacts you can read",
	Long: `List the artifacts you can read, newest first: those you own, those
shared with you, and those homed in projects whose artifacts you may read.

Each line shows the artifact's reference, title, current version, why you
see it (owned, project or shared) and when it last changed. Without --all,
one page is printed; a following page is announced on the last line.

Examples:
  scion artifact list
  scion artifact list --mine
  scion artifact list --project-id <project-id> --shared
  scion artifact list --query report --all`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		_, client, _, err := requireArtifactHubClient()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Minute)
		defer cancel()
		return listArtifacts(ctx, client.Artifacts(), cmd.OutOrStdout(), listArtifactsParams{
			Options: hubclient.ListArtifactsOptions{
				Query:          artifactListQuery,
				ReviewOnLatest: artifactListReviewed,
				Requested:      artifactListRequested,
				Drafts:         artifactListDrafts,
				OwnedOnly:      artifactListMine,
				Limit:          artifactListLimit,
				Scope:          artifactListProject,
				Shared:         artifactListShared,
				Cursor:         artifactListCursor,
			},
			All:  artifactListAll,
			JSON: isJSONOutput(),
		})
	},
}

func init() {
	artifactListCmd.Flags().BoolVar(&artifactListMine, "mine", false, "Only artifacts you own")
	artifactListCmd.Flags().StringVar(&artifactListProject, "project-id", "", "Only artifacts homed in, or shared with, this project (a project id)")
	artifactListCmd.Flags().BoolVar(&artifactListShared, "shared", false, "Only artifacts shared with you (with --project-id, shared with that project) from elsewhere")
	artifactListCmd.Flags().BoolVar(&artifactListReviewed, "reviewed", false, "Only artifacts whose current version is a review")
	artifactListCmd.Flags().BoolVar(&artifactListRequested, "requested", false, "Only artifacts where your review has been requested")
	artifactListCmd.Flags().BoolVar(&artifactListDrafts, "drafts", false, "Only artifacts where you have a saved review draft")
	artifactListCmd.Flags().StringVar(&artifactListQuery, "query", "", "Filter query (e.g. is:reviewed project:my-proj) or plain title/key text")
	artifactListCmd.Flags().IntVar(&artifactListLimit, "limit", 0, "Page size (default: the hub's)")
	artifactListCmd.Flags().BoolVar(&artifactListAll, "all", false, "Follow every page instead of printing one")
	artifactListCmd.Flags().StringVar(&artifactListCursor, "cursor", "", "Start at this page (the nextCursor of a previous --format json list)")
	artifactCmd.AddCommand(artifactListCmd)
}

// listArtifactsParams are the inputs of listArtifacts.
type listArtifactsParams struct {
	Options hubclient.ListArtifactsOptions
	All     bool
	JSON    bool
}

// maxListPages bounds how many pages --all follows, as a guard against a
// cursor that never ends.
const maxListPages = 1000

// listArtifacts prints the artifacts the caller can read: one page, or
// every page with p.All.
func listArtifacts(ctx context.Context, svc hubclient.ArtifactService, out io.Writer, p listArtifactsParams) error {
	if p.Options.Limit < 0 {
		return errors.New("--limit must not be negative")
	}
	opts := p.Options
	var items []hubclient.ArtifactListItem
	next := ""
	for page := 0; ; page++ {
		if page >= maxListPages {
			return fmt.Errorf("stopped after %d pages", maxListPages)
		}
		resp, err := svc.List(ctx, &opts)
		if err != nil {
			return fmt.Errorf("list artifacts: %w%s", err, artifactErrorHint(err, false))
		}
		items = append(items, resp.Artifacts...)
		next = resp.NextCursor
		if !p.All || next == "" {
			break
		}
		opts.Cursor = next
	}
	if p.JSON {
		if items == nil {
			items = []hubclient.ArtifactListItem{}
		}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(struct {
			Artifacts  []hubclient.ArtifactListItem `json:"artifacts"`
			NextCursor string                       `json:"nextCursor,omitempty"`
		}{items, next})
	}
	if len(items) == 0 {
		_, _ = fmt.Fprintln(out, "No artifacts.")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "REF\tTITLE\tVERSION\tACCESS\tSTATE\tUPDATED")
	for _, a := range items {
		access := safeText(a.Access)
		if access == "" {
			access = "-"
		}
		var states []string
		if a.ReviewOnLatest {
			states = append(states, "review on latest")
		}
		if a.ReviewRequested {
			states = append(states, "review requested")
		}
		if a.ReviewInDraft {
			states = append(states, "review in draft")
		}
		state := "-"
		if len(states) > 0 {
			state = strings.Join(states, ", ")
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\tv%d\t%s\t%s\t%s\n", safeText(artifacts.FormatRef(a.ID, 0)), safeText(a.Title), a.CurrentSeq, access, state,
			clitime.Format(a.UpdatedAt, clitime.Minute))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if next != "" {
		_, _ = fmt.Fprintf(out, "More artifacts follow; use --all to list them, or --cursor %s for the next page.\n", safeText(next))
	}
	return nil
}
