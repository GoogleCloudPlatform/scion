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

// Review-request notifications (ptone/scion#9296, design §4.7): the hub's
// artifacts.ReviewRequestNotifier. In Phase 1 (ptone/scion#9306), only user
// requesters and in-app user notifications are implemented.

var _ artifacts.ReviewRequestNotifier = (*artifactHost)(nil)

const reviewRequestTitleMaxRunes = 120

// reviewRequestAskedPhrase is the fixed phrase shared between review-request
// notification templates (NotifyReviewRequest, grantNoticeMessage) and the
// legacy inbox parser (reviewRequestedByFromMessage in inbox_reviews.go) so
// they cannot drift.
const reviewRequestAskedPhrase = " asked you to review "

var (
	errReviewRequestNoticeActor       = errors.New("hub: review request notices require a user actor in phase 1")
	errReviewRequestNoticeUnavailable = errors.New("hub: review request notices are unavailable")
	errReviewRequestDismissNoTarget   = errors.New("hub: no user recipient for review request dismiss notice")
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
func (h *artifactHost) NotifyReviewRequest(ctx context.Context, n artifacts.ReviewRequestNotice) error {
	if n.RequesterKind != artifacts.PrincipalKindUser || n.RequesterRef == "" {
		return errReviewRequestNoticeActor
	}
	if h.server == nil {
		return errReviewRequestNoticeUnavailable
	}
	label := h.server.grantNoticeLabel(ctx, n.ReviewerUserID, n.RequesterRef)
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

// NotifyRequestDismissed implements artifacts.ReviewRequestNotifier: best-effort
// acknowledges the reviewer's unacknowledged ARTIFACT_REVIEW_REQUESTED
// notifications for the artifact, then writes an in-app
// ARTIFACT_REVIEW_DISMISSED notification to the requester and, when different,
// to the artifact's user owner.
func (h *artifactHost) NotifyRequestDismissed(ctx context.Context, n artifacts.ReviewRequestDismissNotice) error {
	if h.server == nil {
		return errReviewRequestNoticeUnavailable
	}
	if n.ReviewerKind == artifacts.PrincipalKindUser && n.ReviewerRef != "" && n.ArtifactID != "" {
		h.ackReviewRequestNotifications(ctx, n.ReviewerRef, n.ArtifactID)
	}
	var recipients []string
	if n.RequesterKind == artifacts.PrincipalKindUser && n.RequesterRef != "" {
		recipients = append(recipients, n.RequesterRef)
	}
	if n.OwnerKind == artifacts.PrincipalKindUser && n.OwnerRef != "" && n.OwnerRef != n.RequesterRef {
		recipients = append(recipients, n.OwnerRef)
	}
	if len(recipients) == 0 {
		return errReviewRequestDismissNoTarget
	}

	title := cleanNotificationText(n.ArtifactTitle, reviewRequestTitleMaxRunes)
	if title == "" {
		title = "untitled"
	}
	excerpt := cleanNotificationText(n.ResponseNote, reviewExcerptMaxRunes)

	var firstErr error
	for _, recipientID := range recipients {
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
