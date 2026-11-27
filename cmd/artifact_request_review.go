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
	artifactRequestReviewUsers     []string
	artifactRequestReviewDelegator bool
	artifactRequestReviewNote      string
	artifactRequestReviewWithdraw  bool
	artifactRequestReviewJSON      bool
)

var artifactRequestReviewCmd = &cobra.Command{
	Use:   "request-review <ref>",
	Short: "Ask one or more users to review an artifact, or withdraw a request",
	Long: `Ask one or more users to review an artifact, or withdraw an open review
request with --withdraw (requires the hub.artifacts_review_requests experiment).

Each --user flag accepts a user ID, user:<id>, or email address. Repeat
--user to request reviews from up to 10 users at once.

In agent mode, --delegator requests a review from the agent's delegating user
without needing to pass their user ID.

Note: --withdraw resolves the request ID through the artifact's review-requests
list view, so a requester who has lost publish access cannot withdraw a request
from the CLI.

Examples:
  scion artifact request-review scion://artifact/5f1c2d3e-... --user alice@example.com
  scion artifact request-review scion://artifact/5f1c2d3e-... --user <user-id> --note "Please check section 3"
  scion artifact request-review scion://artifact/5f1c2d3e-... --user alice@example.com --user bob@example.com --json
  scion artifact request-review scion://artifact/5f1c2d3e-... --delegator --note "Ready for review"
  scion artifact request-review scion://artifact/5f1c2d3e-... --withdraw --user alice@example.com`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if artifactRequestReviewDelegator && resolveMode() != ModeAgent {
			return errors.New("--delegator can only be used in agent mode")
		}
		_, client, _, err := requireArtifactHubClient()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
		defer cancel()
		return requestArtifactReview(ctx, client.Artifacts(), client.Users(), cmd.OutOrStdout(), args[0], requestReviewOptions{
			Users:     artifactRequestReviewUsers,
			Delegator: artifactRequestReviewDelegator,
			Note:      artifactRequestReviewNote,
			Withdraw:  artifactRequestReviewWithdraw,
			JSON:      artifactRequestReviewJSON || isJSONOutput(),
		})
	},
}

func init() {
	artifactRequestReviewCmd.Flags().StringArrayVar(&artifactRequestReviewUsers, "user", nil, "User ID, user:<id>, or email to request a review from (repeatable)")
	artifactRequestReviewCmd.Flags().BoolVar(&artifactRequestReviewDelegator, "delegator", false, "Request a review from the agent's delegating user (agent mode only)")
	artifactRequestReviewCmd.Flags().StringVar(&artifactRequestReviewNote, "note", "", "Optional message for the reviewer (at most 1000 characters)")
	artifactRequestReviewCmd.Flags().BoolVar(&artifactRequestReviewWithdraw, "withdraw", false, "Withdraw the open review request for each --user")
	artifactRequestReviewCmd.Flags().BoolVar(&artifactRequestReviewJSON, "json", false, "Print the raw JSON response")
	artifactCmd.AddCommand(artifactRequestReviewCmd)
}

type requestReviewOptions struct {
	Users     []string
	Delegator bool
	Note      string
	Withdraw  bool
	JSON      bool
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
	if opts.Withdraw {
		if opts.Delegator {
			return errors.New("--delegator cannot be used with --withdraw; pass --user <id>")
		}
		if opts.Note != "" {
			return errors.New("--note cannot be used with --withdraw")
		}
	}
	if len(opts.Users) == 0 && !opts.Delegator {
		return errors.New("at least one --user (or --delegator in agent mode) is required")
	}
	reviewers := make([]string, 0, len(opts.Users))
	for _, u := range opts.Users {
		principal, err := resolveReviewerPrincipal(ctx, users, u)
		if err != nil {
			return err
		}
		reviewers = append(reviewers, principal)
	}
	if opts.Withdraw {
		return withdrawArtifactReviewRequests(ctx, svc, out, id, reviewers, opts.JSON)
	}
	resp, err := svc.RequestReview(ctx, id, &hubclient.ArtifactReviewRequestsInput{
		Reviewers: reviewers,
		Delegator: opts.Delegator,
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

func withdrawArtifactReviewRequests(ctx context.Context, svc hubclient.ArtifactService, out io.Writer, id string, reviewers []string, jsonOut bool) error {
	listed, err := svc.ListReviewRequests(ctx, id)
	if err != nil {
		return fmt.Errorf("list review requests: %w%s", err, artifactErrorHint(err, false))
	}
	openByUser := make(map[string]hubclient.ArtifactReviewRequest, len(listed.Requests))
	for _, r := range listed.Requests {
		if r.State == artifacts.ReviewRequestStateOpen {
			openByUser[r.Reviewer.Ref] = r
		}
	}
	var withdrawn []hubclient.ArtifactReviewRequest
	for _, principal := range reviewers {
		uid := strings.TrimPrefix(principal, "user:")
		openReq, ok := openByUser[uid]
		if !ok {
			return fmt.Errorf("withdraw review request: no open review request for %s", safeText(principal))
		}
		resp, err := svc.WithdrawReviewRequest(ctx, id, openReq.ID)
		if err != nil {
			return fmt.Errorf("withdraw review request for %s: %w%s", safeText(principal), err, artifactErrorHint(err, false))
		}
		withdrawn = append(withdrawn, resp.Request)
		if !jsonOut {
			_, _ = fmt.Fprintf(out, "%s: withdrawn\n", safeText(principal))
		}
	}
	if jsonOut {
		if len(withdrawn) == 1 {
			return writeJSONTo(out, hubclient.ArtifactWithdrawReviewRequestResponse{Request: withdrawn[0]})
		}
		return writeJSONTo(out, hubclient.ArtifactListReviewRequestsResponse{Requests: withdrawn})
	}
	return nil
}
