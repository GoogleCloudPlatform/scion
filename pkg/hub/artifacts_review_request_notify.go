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

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Review-request notifications (ptone/scion#9296, design §4.7): the hub's
// artifacts.ReviewRequestNotifier. In Phase 1 (ptone/scion#9306), only user
// requesters and in-app user notifications are implemented.

var _ artifacts.ReviewRequestNotifier = (*artifactHost)(nil)

const reviewRequestTitleMaxRunes = 120

var (
	errReviewRequestNoticeActor       = errors.New("hub: review request notices require a user actor in phase 1")
	errReviewRequestNoticeUnavailable = errors.New("hub: review request notices are unavailable")
	errReviewRequestDismissNoTarget   = errors.New("hub: no user recipient for review request dismiss notice")
)

// MayNotifyReviewer implements artifacts.ReviewRequestNotifier: checks that the
// calling user can read reviewerUserID (anti-spam user visibility, §4.3 step 4).
func (h *artifactHost) MayNotifyReviewer(ctx context.Context, reviewerUserID string) (bool, error) {
	if h.server == nil || h.server.store == nil || h.server.authzService == nil {
		return false, errReviewRequestNoticeUnavailable
	}
	caller := GetUserIdentityFromContext(ctx)
	if caller == nil || caller.ID() == "" {
		return false, nil
	}
	if _, federated := caller.(FederatedIdentity); federated {
		return false, nil
	}
	subject, _, err := h.server.namedUserSubject(ctx, reviewerUserID)
	if err != nil {
		return false, err
	}
	if subject == nil {
		return false, nil
	}
	return h.server.authzService.CheckAccess(ctx, caller, userResource(subject), ActionRead).Allowed, nil
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
	msg := fmt.Sprintf("%s asked you to review %q (v%d): %s", label, title, n.RequestedSeq, artifacts.FormatRef(n.ArtifactID, 0))
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

// NotifyRequestDismissed implements artifacts.ReviewRequestNotifier: writes an
// in-app ARTIFACT_REVIEW_DISMISSED notification to the requester and, when
// different, to the artifact's user owner.
func (h *artifactHost) NotifyRequestDismissed(ctx context.Context, n artifacts.ReviewRequestDismissNotice) error {
	if h.server == nil {
		return errReviewRequestNoticeUnavailable
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
		msg := fmt.Sprintf("%s dismissed your review request on %q", label, title)
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
