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

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const (
	inboxReviewsHref = "/artifacts?review_on_latest=1&owner=me"

	// InboxReviewsRequestedHref and InboxReviewsDraftsHref are the legacy-param
	// filter URLs for the requested and drafts buckets (design §4.10).
	InboxReviewsRequestedHref = "/artifacts?requested=1"
	InboxReviewsDraftsHref    = "/artifacts?drafts=1"

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
	before, _, ok := strings.Cut(msg, reviewRequestAskedPhrase)
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
		CountsTowardTotal: false,
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
			CountsTowardTotal: false,
			Href:              inboxReviewsHref,
			Previews:          []any{},
		}
	}

	if !requestCredentialBindsIdentity(ctx) {
		return InboxCategory{
			State:             InboxStateUnavailable,
			Count:             0,
			Capped:            false,
			CountsTowardTotal: false,
			Href:              inboxReviewsHref,
			Previews:          []any{},
		}
	}

	svc := s.artifactRefResolver()
	draftsEnabled := s.experimentEnabled(experiments.ArtifactsReviewDrafts)
	requestsEnabled := s.experimentEnabled(experiments.ArtifactsReviewRequests)

	// 1. Owned artifacts with review on latest, plus durable open review
	// requests (when hub.artifacts_review_requests is on) and saved drafts
	// (when hub.artifacts_review_drafts is on).
	summary, err := svc.ReviewStateSummary(ctx, artifacts.ReviewSummaryOptions{
		Previews: previews,
		Drafts:   draftsEnabled,
	})
	if err != nil {
		return inboxReviewsErrorCategory(ctx, err)
	}

	toAddressCount := summary.ToAddress.Count
	toAddressCapped := summary.ToAddress.Capped
	toAddressItems := summary.ToAddress.Items
	draftsCapped := summary.Drafts != nil && summary.Drafts.Capped

	var (
		requestedCount    int
		requestedCapped   bool
		requestedPreviews []InboxReviewPreview
	)
	if requestsEnabled {
		requestedCount = summary.Requested.Count
		requestedCapped = summary.Requested.Capped
		if previews > 0 && len(summary.Requested.Items) > 0 {
			requestedPreviews = make([]InboxReviewPreview, 0, len(summary.Requested.Items))
			for _, it := range summary.Requested.Items {
				previewID := it.ID
				createdAt := it.UpdatedAt
				requestedBy := ""
				if it.Request != nil {
					if it.Request.ID != "" {
						previewID = it.Request.ID
					}
					if !it.Request.CreatedAt.IsZero() {
						createdAt = it.Request.CreatedAt
					}
					requestedBy = it.Request.Requester.DisplayName
					if requestedBy == "" {
						requestedBy = it.Request.Requester.Ref
					}
				}
				requestedPreviews = append(requestedPreviews, InboxReviewPreview{
					ID:          previewID,
					Kind:        InboxReviewKindRequested,
					ProjectID:   it.ScopeRef,
					CreatedAt:   createdAt,
					Href:        artifactPagePath(it.ScopeRef, it.ID),
					ArtifactID:  it.ID,
					Title:       it.Title,
					ScopeRef:    it.ScopeRef,
					RequestedBy: requestedBy,
				})
			}
		}
	} else {
		// Gate-off fallback (§4.9): keep the notification-scan source while
		// hub.artifacts_review_requests is off. Delete this branch when the
		// experiment graduates.
		requestedCount, requestedCapped, requestedPreviews, err = s.inboxRequestedFromNotifications(ctx, v, svc, previews)
		if err != nil {
			return inboxReviewsErrorCategory(ctx, err)
		}
	}

	items := make([]any, 0, previews)
	if previews > 0 && (len(toAddressItems) > 0 || len(requestedPreviews) > 0) {
		candidates := make([]InboxReviewPreview, 0, len(toAddressItems)+len(requestedPreviews))
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
		candidates = append(candidates, requestedPreviews...)

		sort.Slice(candidates, func(i, j int) bool {
			if !candidates[i].CreatedAt.Equal(candidates[j].CreatedAt) {
				return candidates[i].CreatedAt.After(candidates[j].CreatedAt)
			}
			return candidates[i].ID > candidates[j].ID
		})
		if len(candidates) > previews {
			candidates = candidates[:previews]
		}

		identity := GetIdentityFromContext(ctx)
		projectNames := make(map[string]string, len(candidates))
		for i := range candidates {
			pid := candidates[i].ProjectID
			if pid == "" {
				continue
			}
			name, cached := projectNames[pid]
			if !cached {
				if s.authzService != nil && !isNilIdentity(identity) {
					if proj, perr := s.store.GetProject(ctx, pid); perr == nil && proj != nil {
						if s.authzService.CheckAccess(ctx, identity, projectResource(proj), ActionRead).Allowed {
							name = proj.Name
						}
					}
				}
				projectNames[pid] = name
			}
			candidates[i].ProjectName = name
		}

		for i := range candidates {
			items = append(items, candidates[i])
		}
	}

	breakdown := map[string]int{
		InboxBreakdownToAddress: toAddressCount,
		InboxBreakdownRequested: requestedCount,
	}
	if summary.Drafts != nil {
		breakdown[InboxBreakdownDrafts] = summary.Drafts.Count
	}

	return InboxCategory{
		State:             InboxStateOK,
		Count:             toAddressCount + requestedCount,
		Capped:            toAddressCapped || requestedCapped || draftsCapped,
		CountsTowardTotal: true,
		Href:              inboxReviewsHref,
		Previews:          items,
		Breakdown:         breakdown,
	}
}

// inboxRequestedFromNotifications is the legacy notification-scan source for
// the "requested" review count while hub.artifacts_review_requests is off
// (design §4.9). Delete this helper when the experiment graduates.
func (s *Server) inboxRequestedFromNotifications(ctx context.Context, v inboxViewer, svc *artifacts.Service, previews int) (int, bool, []InboxReviewPreview, error) {
	notifs, err := s.store.GetNotifications(ctx, store.SubscriberTypeUser, v.userID, true)
	if err != nil {
		return 0, false, nil, err
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
	readableInfo, requestedCapped, err := svc.ReadableArtifacts(ctx, requestedIDs)
	if err != nil {
		return 0, false, nil, err
	}
	for _, n := range deduped {
		if _, ok := readableInfo[n.ResourceID]; ok {
			requestedCount++
			if previews > 0 && len(readableNotifs) < previews {
				readableNotifs = append(readableNotifs, n)
			}
		}
	}

	var out []InboxReviewPreview
	if previews > 0 && len(readableNotifs) > 0 {
		out = make([]InboxReviewPreview, 0, len(readableNotifs))
		for _, n := range readableNotifs {
			art := readableInfo[n.ResourceID]
			out = append(out, InboxReviewPreview{
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
	}
	return requestedCount, requestedCapped, out, nil
}
