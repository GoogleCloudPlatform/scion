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
	"log/slog"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const (
	inboxReviewsHref = "/artifacts?review_pending=1&owner=me"

	// InboxReviewKind* are the valid values for InboxReviewPreview.Kind.
	InboxReviewKindToAddress = "to_address"
	InboxReviewKindRequested = "requested"
)

// InboxReviewPreview is one preview item in the "reviews" category (design §4.4).
type InboxReviewPreview struct {
	ID             string    `json:"id"`
	Kind           string    `json:"kind"`
	ProjectID      string    `json:"projectId"`
	ProjectName    string    `json:"projectName"`
	CreatedAt      time.Time `json:"createdAt"`
	Href           string    `json:"href"`
	ArtifactID     string    `json:"artifactId"`
	Title          string    `json:"title"`
	ScopeRef       string    `json:"scopeRef"`
	RequestedBy    string    `json:"requestedBy,omitempty"`
	NotificationID string    `json:"notificationId,omitempty"`
}

func artifactPagePath(scopeRef, artifactID string) string {
	return "/projects/" + url.PathEscape(scopeRef) + "/artifacts/" + url.PathEscape(artifactID)
}

func reviewRequestedByFromMessage(msg string) string {
	before, _, ok := strings.Cut(msg, " asked you to review an artifact")
	if !ok {
		return ""
	}
	return strings.TrimSpace(before)
}

func inboxReviewsErrorCategory(ctx context.Context, err error) InboxCategory {
	code := inboxCategoryErrorCode(ctx, err)
	slog.Warn("inbox summary: category source failed",
		"category", InboxCategoryReviews,
		"error_code", code,
		"error", err)
	return InboxCategory{
		State:             InboxStateError,
		Count:             0,
		Capped:            false,
		CountsTowardTotal: true,
		Href:              inboxReviewsHref,
		ErrorCode:         code,
		Previews:          []any{},
	}
}

func (s *Server) inboxReviewsCategory(ctx context.Context, v inboxViewer, previews int, _ string) InboxCategory {
	if !s.experimentEnabled(experiments.Artifacts) || !s.artifactsConfig().Enabled {
		return InboxCategory{
			State:             InboxStateDisabled,
			Count:             0,
			Capped:            false,
			CountsTowardTotal: true,
			Href:              inboxReviewsHref,
			Previews:          []any{},
		}
	}

	svc := s.inboxArtifactService()

	// 1. Owned artifacts whose current version is a submitted review awaiting owner action.
	toAddressCount, toAddressCapped, toAddressItems, err := svc.ReviewPendingSummary(ctx, previews)
	if err != nil {
		return inboxReviewsErrorCategory(ctx, err)
	}

	// 2. Unacknowledged ARTIFACT_REVIEW_REQUESTED notifications, deduped by
	// ResourceID (newest first), whose artifact the caller can still read.
	notifs, err := s.store.GetNotifications(ctx, store.SubscriberTypeUser, v.userID, true)
	if err != nil {
		return inboxReviewsErrorCategory(ctx, err)
	}
	if v.token != nil {
		notifs = scopeNotificationsForToken(s.newSelfScopeCheck(ctx, v.token, permInboxRead), notifs)
	}

	var matching []store.Notification
	for _, n := range notifs {
		if n.Status != store.NotificationStatusArtifactReviewRequested || n.ResourceID == "" {
			continue
		}
		if n.ResourceType != "" && n.ResourceType != store.NotificationResourceArtifact {
			continue
		}
		matching = append(matching, n)
	}

	// Sort newest-first before deduplicating by ResourceID so we keep the
	// latest request notification for each artifact.
	sort.Slice(matching, func(i, j int) bool {
		if !matching[i].CreatedAt.Equal(matching[j].CreatedAt) {
			return matching[i].CreatedAt.After(matching[j].CreatedAt)
		}
		return matching[i].ID > matching[j].ID
	})

	deduped := make([]store.Notification, 0, len(matching))
	requestedIDs := make([]string, 0, len(matching))
	seenArtifact := make(map[string]bool, len(matching))
	for _, n := range matching {
		if seenArtifact[n.ResourceID] {
			continue
		}
		seenArtifact[n.ResourceID] = true
		deduped = append(deduped, n)
		requestedIDs = append(requestedIDs, n.ResourceID)
	}

	requestedCount := 0
	var readableNotifs []store.Notification
	readableInfo, err := svc.ReadableArtifacts(ctx, requestedIDs)
	if err != nil {
		return inboxReviewsErrorCategory(ctx, err)
	}
	for _, n := range deduped {
		if _, ok := readableInfo[n.ResourceID]; ok {
			requestedCount++
			if previews > 0 && len(readableNotifs) < previews {
				readableNotifs = append(readableNotifs, n)
			}
		}
	}

	items := make([]any, 0, previews)
	if previews > 0 && (len(toAddressItems) > 0 || len(readableNotifs) > 0) {
		candidates := make([]InboxReviewPreview, 0, len(toAddressItems)+len(readableNotifs))
		for _, it := range toAddressItems {
			candidates = append(candidates, InboxReviewPreview{
				ID:         it.ID,
				Kind:       InboxReviewKindToAddress,
				ProjectID:  it.ScopeRef,
				CreatedAt:  it.UpdatedAt,
				Href:       artifactPagePath(it.ScopeRef, it.ID),
				ArtifactID: it.ID,
				Title:      it.Title,
				ScopeRef:   it.ScopeRef,
			})
		}
		for _, n := range readableNotifs {
			art := readableInfo[n.ResourceID]
			candidates = append(candidates, InboxReviewPreview{
				ID:             n.ID,
				Kind:           InboxReviewKindRequested,
				ProjectID:      art.ScopeRef,
				CreatedAt:      n.CreatedAt,
				Href:           artifactPagePath(art.ScopeRef, art.ID),
				ArtifactID:     art.ID,
				Title:          art.Title,
				ScopeRef:       art.ScopeRef,
				RequestedBy:    reviewRequestedByFromMessage(n.Message),
				NotificationID: n.ID,
			})
		}

		sort.Slice(candidates, func(i, j int) bool {
			if !candidates[i].CreatedAt.Equal(candidates[j].CreatedAt) {
				return candidates[i].CreatedAt.After(candidates[j].CreatedAt)
			}
			return candidates[i].ID > candidates[j].ID
		})
		if len(candidates) > previews {
			candidates = candidates[:previews]
		}

		projectNames := make(map[string]string, len(candidates))
		for i := range candidates {
			pid := candidates[i].ProjectID
			if pid == "" {
				continue
			}
			name, cached := projectNames[pid]
			if !cached {
				if proj, perr := s.store.GetProject(ctx, pid); perr == nil && proj != nil {
					name = proj.Name
				}
				projectNames[pid] = name
			}
			candidates[i].ProjectName = name
		}

		for i := range candidates {
			items = append(items, candidates[i])
		}
	}

	return InboxCategory{
		State:             InboxStateOK,
		Count:             toAddressCount + requestedCount,
		Capped:            toAddressCapped,
		CountsTowardTotal: true,
		Href:              inboxReviewsHref,
		Previews:          items,
		Breakdown: map[string]int{
			"toAddress": toAddressCount,
			"requested": requestedCount,
		},
	}
}
