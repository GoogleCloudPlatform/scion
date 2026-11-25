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
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

var (
	artifactRequestReviewUsers []string
	artifactRequestReviewNote  string
	artifactRequestReviewJSON  bool
)

var artifactRequestReviewCmd = &cobra.Command{
	Use:   "request-review <ref>",
	Short: "Ask one or more users to review an artifact",
	Long: `Ask one or more users to review an artifact (requires the hub.artifacts_review_requests experiment).

Each --user flag accepts a user ID, user:<id>, or email address. Repeat
--user to request reviews from up to 10 users at once.

Examples:
  scion artifact request-review scion://artifact/5f1c2d3e-... --user alice@example.com
  scion artifact request-review scion://artifact/5f1c2d3e-... --user <user-id> --note "Please check section 3"
  scion artifact request-review scion://artifact/5f1c2d3e-... --user alice@example.com --user bob@example.com --json`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		_, client, _, err := requireArtifactHubClient()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
		defer cancel()
		return requestArtifactReview(ctx, client.Artifacts(), client.Users(), cmd.OutOrStdout(), args[0], requestReviewOptions{
			Users: artifactRequestReviewUsers,
			Note:  artifactRequestReviewNote,
			JSON:  artifactRequestReviewJSON || isJSONOutput(),
		})
	},
}

func init() {
	artifactRequestReviewCmd.Flags().StringArrayVar(&artifactRequestReviewUsers, "user", nil, "User ID, user:<id>, or email to request a review from (repeatable)")
	artifactRequestReviewCmd.Flags().StringVar(&artifactRequestReviewNote, "note", "", "Optional message for the reviewer (at most 1000 characters)")
	artifactRequestReviewCmd.Flags().BoolVar(&artifactRequestReviewJSON, "json", false, "Print the raw JSON response")
	artifactCmd.AddCommand(artifactRequestReviewCmd)
}

type requestReviewOptions struct {
	Users []string
	Note  string
	JSON  bool
}

func resolveReviewerPrincipal(ctx context.Context, users hubclient.UserService, raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errors.New("--user must not be empty")
	}
	if id, ok := strings.CutPrefix(s, "user:"); ok {
		id = strings.TrimSpace(id)
		if id == "" {
			return "", fmt.Errorf("--user %q: missing user ID", raw)
		}
		return "user:" + id, nil
	}
	if !strings.Contains(s, "@") {
		return "user:" + s, nil
	}
	if users == nil {
		return "", fmt.Errorf("cannot resolve email %q without user lookup service; pass a user ID instead", s)
	}
	lower := strings.ToLower(s)
	var matches []hubclient.User
	cursor := ""
	for page := 0; ; page++ {
		if page >= maxResolutionPages {
			return "", fmt.Errorf("--user %q: too many matching users; pass the user ID instead", raw)
		}
		resp, err := users.List(ctx, &hubclient.ListUsersOptions{
			Search: s,
			Page:   apiclient.PageOptions{Cursor: cursor},
		})
		if err != nil {
			return "", fmt.Errorf("resolve --user %q: %w", raw, err)
		}
		for _, u := range resp.Users {
			if strings.ToLower(u.Email) == lower {
				matches = append(matches, u)
			}
		}
		if resp.Page.NextCursor == "" {
			break
		}
		cursor = resp.Page.NextCursor
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("--user %q: no matching user found", raw)
	case 1:
		return "user:" + matches[0].ID, nil
	default:
		return "", fmt.Errorf("--user %q: matched %d users; pass the user ID instead", raw, len(matches))
	}
}

func requestArtifactReview(ctx context.Context, svc hubclient.ArtifactService, users hubclient.UserService, out io.Writer, ref string, opts requestReviewOptions) error {
	id, _, err := artifacts.ParseRef(ref)
	if err != nil {
		return err
	}
	if len(opts.Users) == 0 {
		return errors.New("at least one --user is required")
	}
	reviewers := make([]string, 0, len(opts.Users))
	for _, u := range opts.Users {
		principal, err := resolveReviewerPrincipal(ctx, users, u)
		if err != nil {
			return err
		}
		reviewers = append(reviewers, principal)
	}
	resp, err := svc.RequestReview(ctx, id, &hubclient.ArtifactReviewRequestsInput{
		Reviewers: reviewers,
		Note:      opts.Note,
	})
	if err != nil {
		return fmt.Errorf("request review: %w%s", err, artifactErrorHint(err, false))
	}
	if opts.JSON {
		return writeJSONTo(out, resp)
	}
	for _, r := range resp.Requests {
		var status string
		switch r.Outcome {
		case artifacts.ReviewRequestOutcomeCreated:
			status = "requested"
			if r.Access == artifacts.ReviewRequestAccessRaised {
				status += ", gave Can review"
			}
		case artifacts.ReviewRequestOutcomeReminded:
			status = "reminded"
		case artifacts.ReviewRequestOutcomeUnchanged:
			status = "already requested"
		case artifacts.ReviewRequestOutcomeRefused:
			status = fmt.Sprintf("refused (%s)", safeText(r.Code))
		default:
			status = safeText(r.Outcome)
		}
		if r.NotifySkipped != "" {
			status += fmt.Sprintf(" (not notified: %s)", safeText(r.NotifySkipped))
		}
		_, _ = fmt.Fprintf(out, "%s: %s\n", safeText(r.Reviewer), status)
	}
	return nil
}
