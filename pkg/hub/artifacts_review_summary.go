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
	"fmt"
	"log/slog"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/plaintext"
	"github.com/google/uuid"
)

// Review summary notifications (ptone/scion#6853). When a reviewer other
// than the owner, admitted by the finalize request to submit a review (a
// review, write or admin grant), finalizes a review of an artifact a user
// owns, the owner gets one in-app notification of kind
// store.NotificationKindArtifactReviewSummary. Its Message is the hub's own
// text and names the reviewer; its Excerpt is the start of the review's
// summary note, cleaned here when the row is built; its resource fields
// come from the review record. It is stored for the owner and published on
// the owner's own subject only, like every user-addressed notification. It
// is not sent to chat plugins or other outside channels. The full note
// stays on the artifact page.

const (
	// reviewExcerptMaxRunes caps the review summary excerpt, ellipsis
	// included.
	reviewExcerptMaxRunes = 240
	// reviewerLabelMaxRunes caps the reviewer's name in the message,
	// ellipsis included.
	reviewerLabelMaxRunes = 64
)

// cleanNotificationText is the build-time strip for a notification field
// that holds another principal's words: single-line plain text of at most
// maxRunes runes, with control and format characters removed and
// whitespace collapsed (plaintext.Clean).
func cleanNotificationText(s string, maxRunes int) string {
	return plaintext.Clean(s, maxRunes)
}

// notifyArtifactReviewSummary creates the review summary notification for
// the user who owns the reviewed artifact and publishes it on that user's
// subject. It does nothing unless the owner is a user, the reviewer is
// someone else and was admitted to submit the review by the finalize
// request (ReviewNotice.SubmitAuthorized), and the artifact id is a
// canonical UUID.
// Every failure is logged and skipped: it never changes the finalize
// result.
func (s *Server) notifyArtifactReviewSummary(ctx context.Context, n artifacts.ReviewNotice) {
	if n.OwnerKind != artifacts.PrincipalKindUser || n.OwnerRef == "" || !n.SubmitAuthorized {
		return
	}
	if n.ReviewerKind == n.OwnerKind && n.ReviewerRef == n.OwnerRef {
		return
	}
	log := slog.With("artifact_id", n.ArtifactID, "seq", n.Seq)
	if id, err := uuid.Parse(n.ArtifactID); err != nil || id.String() != n.ArtifactID {
		log.WarnContext(ctx, "artifacts: review summary skipped: artifact id is not a canonical UUID")
		return
	}
	owner, err := s.store.GetUser(ctx, n.OwnerRef)
	if err != nil {
		log.WarnContext(ctx, "artifacts: review summary skipped: owner not found", "error", err)
		return
	}
	notif := buildReviewSummaryNotification(n, owner.ID, s.reviewerLabel(ctx, n.ReviewerKind, n.ReviewerRef))
	if err := s.store.CreateNotification(ctx, notif); err != nil {
		log.ErrorContext(ctx, "artifacts: review summary notification not stored", "error", err)
		return
	}
	if s.events != nil {
		s.events.PublishUserNotification(ctx, notif)
	}
}

// buildReviewSummaryNotification is the notification for review notice n,
// addressed to user ownerID, naming the reviewer as label (already
// cleaned). The resource fields and the project come from the notice
// alone; the note only ever becomes the Excerpt.
func buildReviewSummaryNotification(n artifacts.ReviewNotice, ownerID, label string) *store.Notification {
	excerpt := cleanNotificationText(n.Note, reviewExcerptMaxRunes)
	message := fmt.Sprintf("Review (v%d) of your artifact submitted by %s.", n.Seq, label)
	if excerpt != "" {
		message = fmt.Sprintf("Review summary from %s (v%d of your artifact):", label, n.Seq)
	}
	projectID := uuid.Nil.String()
	if n.ScopeKind == artifacts.ScopeKindProject {
		if id, err := uuid.Parse(n.ScopeRef); err == nil && id.String() == n.ScopeRef {
			projectID = n.ScopeRef
		}
	}
	return &store.Notification{
		ID:             api.NewUUID(),
		SubscriptionID: store.CreatorFallbackSubscriptionID,
		AgentID:        uuid.Nil.String(),
		ProjectID:      projectID,
		SubscriberType: store.SubscriberTypeUser,
		SubscriberID:   ownerID,
		Status:         store.NotificationStatusArtifactReviewSubmitted,
		Message:        message,
		CreatedAt:      time.Now(),
		Kind:           store.NotificationKindArtifactReviewSummary,
		ResourceType:   store.NotificationResourceArtifact,
		ResourceID:     n.ArtifactID,
		Excerpt:        excerpt,
	}
}

// reviewerLabel is how a review summary names its reviewer: a user's
// display name, or an agent's name (or slug) marked "(agent)", cleaned and
// capped like the excerpt. A reviewer that cannot be looked up, or whose
// name cleans to nothing, is "a user", "an agent" or "a reviewer".
func (s *Server) reviewerLabel(ctx context.Context, kind, ref string) string {
	switch kind {
	case artifacts.PrincipalKindUser:
		if u, err := s.store.GetUser(ctx, ref); err == nil {
			if l := cleanNotificationText(u.DisplayName, reviewerLabelMaxRunes); l != "" {
				return l
			}
		}
		return "a user"
	case artifacts.PrincipalKindAgent:
		if a, err := s.store.GetAgent(ctx, ref); err == nil {
			name := a.Name
			if name == "" {
				name = a.Slug
			}
			if l := cleanNotificationText(name, reviewerLabelMaxRunes); l != "" {
				return l + " (agent)"
			}
		}
		return "an agent"
	}
	return "a reviewer"
}
