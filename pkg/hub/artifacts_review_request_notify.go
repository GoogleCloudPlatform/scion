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

package hub

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Review-request notifications (ptone/scion#9296, ptone/scion#5548, design §4.7):
// the hub's artifacts.ReviewRequestNotifier. Supports both user and agent
// requesters, in-app user notifications, and system-message dismiss delivery to
// agent authors/requesters via artifacts_review.go.

var _ artifacts.ReviewRequestNotifier = (*artifactHost)(nil)

const reviewRequestTitleMaxRunes = 120

// reviewRequestAskedPhrase is the fixed phrase shared between review-request
// notification templates (NotifyReviewRequest, grantNoticeMessage) and the
// legacy inbox parser (reviewRequestedByFromMessage in inbox_reviews.go) so
// they cannot drift.
const reviewRequestAskedPhrase = " asked you to review "

var (
	errReviewRequestNoticeActor       = errors.New("hub: review request notices require a user or agent actor")
	errReviewRequestNoticeUnavailable = errors.New("hub: review request notices are unavailable")
	errReviewRequestDismissNoTarget   = errors.New("hub: no recipient for review request dismiss notice")
)

// IsInteractiveSession implements artifacts.ReviewRequestNotifier: reports
// whether the caller in ctx holds an interactive, unscoped user session.
func (h *artifactHost) IsInteractiveSession(ctx context.Context) bool {
	_, ok := interactiveUserSession(ctx)
	return ok
}

// MayNotifyReviewer implements artifacts.ReviewRequestNotifier: checks that the
// caller is an interactive, unscoped user session and can read reviewerUserID
// (anti-spam user visibility, §4.3 step 4) by delegating to MayNotifyGrant so
// the named-user subject check stays on the single allow-listed path.
func (h *artifactHost) MayNotifyReviewer(ctx context.Context, reviewerUserID string) (bool, error) {
	return h.MayNotifyGrant(ctx, reviewerUserID)
}

// NotifyReviewRequest implements artifacts.ReviewRequestNotifier: writes an
// in-app ARTIFACT_REVIEW_REQUESTED notification to the requested reviewer.
// Accepts both user requesters and agent requesters (labelled "agent <slug>").
func (h *artifactHost) NotifyReviewRequest(ctx context.Context, n artifacts.ReviewRequestNotice) error {
	if n.RequesterRef == "" || (n.RequesterKind != artifacts.PrincipalKindUser && n.RequesterKind != artifacts.PrincipalKindAgent) {
		return errReviewRequestNoticeActor
	}
	if h.server == nil {
		return errReviewRequestNoticeUnavailable
	}
	var label string
	switch n.RequesterKind {
	case artifacts.PrincipalKindUser:
		label = h.server.grantNoticeLabel(ctx, n.ReviewerUserID, n.RequesterRef)
	case artifacts.PrincipalKindAgent:
		label = h.server.reviewRequestAgentLabel(ctx, n.RequesterRef)
	}
	title := cleanNotificationText(n.ArtifactTitle, reviewRequestTitleMaxRunes)
	if title == "" {
		title = "untitled"
	}
	msg := fmt.Sprintf("%s%s%q (v%d): %s", label, reviewRequestAskedPhrase, title, n.RequestedSeq, artifacts.FormatRef(n.ArtifactID, 0))
	excerpt := cleanNotificationText(n.Note, reviewExcerptMaxRunes)
	return h.server.writeUserNotification(ctx, userNotice{
		Kind:             store.NotificationKindArtifactReviewRequest,
		ResourceType:     store.NotificationResourceArtifact,
		ResourceID:       n.ArtifactID,
		Status:           store.NotificationStatusArtifactReviewRequested,
		ProjectID:        n.HomeScope,
		SubscriberUserID: n.ReviewerUserID,
		Message:          msg,
		Excerpt:          excerpt,
	})
}

// reviewRequestAgentLabel returns "agent <slug>" for an agent requester, or
// "an agent" if the agent cannot be looked up or has no slug (design §4.7).
func (s *Server) reviewRequestAgentLabel(ctx context.Context, agentID string) string {
	if s.store != nil && agentID != "" {
		if a, err := s.store.GetAgent(ctx, agentID); err == nil && a != nil {
			if slug := cleanNotificationText(a.Slug, 80); slug != "" {
				return "agent " + slug
			}
		}
	}
	return "an agent"
}

// NotifyRequestDismissed implements artifacts.ReviewRequestNotifier: best-effort
// acknowledges the reviewer's unacknowledged ARTIFACT_REVIEW_REQUESTED
// notifications for the artifact, then notifies the requester and, when
// different, the artifact owner. User recipients receive an in-app
// ARTIFACT_REVIEW_DISMISSED notification; agent recipients receive a
// SystemCategoryArtifactReview system message via artifacts_review.go dispatch
// (design §4.7 [D: dismiss-agent-delivery]).
func (h *artifactHost) NotifyRequestDismissed(ctx context.Context, n artifacts.ReviewRequestDismissNotice) error {
	if h.server == nil {
		return errReviewRequestNoticeUnavailable
	}
	if n.ReviewerKind == artifacts.PrincipalKindUser && n.ReviewerRef != "" && n.ArtifactID != "" {
		h.ackReviewRequestNotifications(ctx, n.ReviewerRef, n.ArtifactID)
	}
	var userRecipients []string
	if n.RequesterKind == artifacts.PrincipalKindUser && n.RequesterRef != "" {
		userRecipients = append(userRecipients, n.RequesterRef)
	}
	if n.OwnerKind == artifacts.PrincipalKindUser && n.OwnerRef != "" &&
		(n.RequesterKind != artifacts.PrincipalKindUser || n.OwnerRef != n.RequesterRef) {
		userRecipients = append(userRecipients, n.OwnerRef)
	}
	type agentTarget struct {
		id          string
		isRequester bool
	}
	var agentRecipients []agentTarget
	if n.RequesterKind == artifacts.PrincipalKindAgent && n.RequesterRef != "" {
		agentRecipients = append(agentRecipients, agentTarget{id: n.RequesterRef, isRequester: true})
	}
	if n.OwnerKind == artifacts.PrincipalKindAgent && n.OwnerRef != "" &&
		(n.RequesterKind != artifacts.PrincipalKindAgent || n.OwnerRef != n.RequesterRef) {
		agentRecipients = append(agentRecipients, agentTarget{id: n.OwnerRef, isRequester: false})
	}
	if len(userRecipients) == 0 && len(agentRecipients) == 0 {
		return errReviewRequestDismissNoTarget
	}

	title := cleanNotificationText(n.ArtifactTitle, reviewRequestTitleMaxRunes)
	if title == "" {
		title = "untitled"
	}
	excerpt := cleanNotificationText(n.ResponseNote, reviewExcerptMaxRunes)

	var firstErr error
	for _, recipientID := range userRecipients {
		label := h.server.grantNoticeLabel(ctx, recipientID, n.ReviewerRef)
		var msg string
		if n.RequesterKind == artifacts.PrincipalKindUser && recipientID == n.RequesterRef {
			msg = fmt.Sprintf("%s dismissed your review request on %q", label, title)
		} else {
			msg = fmt.Sprintf("%s dismissed a review request on %q", label, title)
		}
		if err := h.server.writeUserNotification(ctx, userNotice{
			Kind:             store.NotificationKindArtifactReviewRequest,
			ResourceType:     store.NotificationResourceArtifact,
			ResourceID:       n.ArtifactID,
			Status:           store.NotificationStatusArtifactReviewDismissed,
			ProjectID:        n.HomeScope,
			SubscriberUserID: recipientID,
			Message:          msg,
			Excerpt:          excerpt,
		}); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	var anyAgentDelivered bool
	for _, target := range agentRecipients {
		if h.server.deliverReviewRequestDismissToAgent(ctx, n, target.id, target.isRequester) {
			anyAgentDelivered = true
		}
	}
	if len(userRecipients) == 0 && !anyAgentDelivered {
		return errReviewRequestNoticeUnavailable
	}
	return firstErr
}

// RequestClosed implements artifacts.ReviewRequestNotifier: best-effort
// acknowledges the reviewer's unacknowledged ARTIFACT_REVIEW_REQUESTED
// notifications for artifactID when a review request is withdrawn, expired, or
// fulfilled.
func (h *artifactHost) RequestClosed(ctx context.Context, artifactID, reviewerUserID string) {
	if reviewerUserID == "" || artifactID == "" {
		return
	}
	h.ackReviewRequestNotifications(ctx, reviewerUserID, artifactID)
}

// ackReviewRequestNotifications best-effort acknowledges the reviewer's
// unacknowledged ARTIFACT_REVIEW_REQUESTED notifications for artifactID so the
// Inbox "requested" count clears on dismiss. Errors are logged and never undo
// the committed dismiss.
func (h *artifactHost) ackReviewRequestNotifications(ctx context.Context, reviewerUserID, artifactID string) {
	if h.server == nil || h.server.store == nil {
		return
	}
	notifs, err := h.server.store.GetNotifications(ctx, store.SubscriberTypeUser, reviewerUserID, true)
	if err != nil {
		slog.WarnContext(ctx, "artifacts: list review-request notifications for ack failed",
			"artifact", artifactID, "reviewer", reviewerUserID, "error", err)
		return
	}
	for _, notif := range notifs {
		if notif.Status != store.NotificationStatusArtifactReviewRequested || notif.ResourceID != artifactID {
			continue
		}
		if notif.ResourceType != "" && notif.ResourceType != store.NotificationResourceArtifact {
			continue
		}
		if err := h.server.store.AcknowledgeNotification(ctx, notif.ID); err != nil {
			slog.WarnContext(ctx, "artifacts: acknowledge review-request notification failed",
				"artifact", artifactID, "notification", notif.ID, "error", err)
		}
	}
}
