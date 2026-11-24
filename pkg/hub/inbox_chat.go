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
	"net/url"
	"sort"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const (
	// inboxChatHref is the "View all" drill-in for the chat category.
	// The chat space rail filter is currently stored only in localStorage
	// (not hydrated from URL query parameters), so link to /chat.
	inboxChatHref = "/chat"

	// InboxChatKind* are the valid kind values for InboxChatPreview.
	InboxChatKindDM      = "dm"
	InboxChatKindMention = "mention"
	InboxChatKindThread  = "thread"
)

// InboxChatPreview is one preview item in the "chat" category.
// Never contains message bodies or excerpts.
type InboxChatPreview struct {
	ID              string    `json:"id"`
	Kind            string    `json:"kind"`
	ProjectID       string    `json:"projectId"`
	ProjectName     string    `json:"projectName"`
	CreatedAt       time.Time `json:"createdAt"`
	Href            string    `json:"href"`
	ConversationKey string    `json:"conversationKey"`
	Label           string    `json:"label"`
	LastMessageAt   time.Time `json:"lastMessageAt"`
}

type inboxChatCandidate struct {
	kind            string
	conversationKey string
	lastMessageAt   time.Time
	thread          *chatUnreadThread
	dm              *chatUnreadDM
}

func (s *Server) inboxChatCategory(ctx context.Context, v inboxViewer, previews int, chatFilter string) InboxCategory {
	if !s.nativeChatEnabled() {
		return inboxDisabledCategory(inboxChatHref)(ctx, v, previews, chatFilter)
	}
	s.mu.RLock()
	wcs := s.webChatStore
	s.mu.RUnlock()
	if wcs == nil {
		return inboxDisabledCategory(inboxChatHref)(ctx, v, previews, chatFilter)
	}

	detail, err := s.chatUnreadDetail(ctx, v.user, v.userID, v.token)
	if err != nil {
		return inboxChatError(ctx, err)
	}
	if detail.storeNil {
		return inboxDisabledCategory(inboxChatHref)(ctx, v, previews, chatFilter)
	}

	threadKeys := make([]string, 0, len(detail.unreadThreads))
	for _, ut := range detail.unreadThreads {
		threadKeys = append(threadKeys, ut.topic.ID)
	}
	mentionKeys, err := chatUnreadMentionKeysBatched(ctx, wcs, v.userID, threadKeys, s.chatSpacesBatch.withDefaults().readStates)
	if err != nil {
		return inboxChatError(ctx, err)
	}

	mentionsCount := 0
	for _, ut := range detail.unreadThreads {
		if mentionKeys[ut.topic.ID] {
			mentionsCount++
		}
	}

	breakdown := map[string]int{
		"dms":                 detail.counts.DMs,
		"mentions":            mentionsCount,
		"unreadConversations": detail.counts.Conversations,
	}

	items := make([]any, 0)
	if previews > 0 {
		items = s.buildInboxChatPreviews(ctx, detail, mentionKeys, previews, chatFilter)
	}

	return InboxCategory{
		State:             InboxStateOK,
		Count:             detail.counts.Conversations,
		Capped:            false,
		CountsTowardTotal: true,
		Href:              inboxChatHref,
		Previews:          items,
		Breakdown:         breakdown,
	}
}

func inboxChatError(ctx context.Context, err error) InboxCategory {
	code := inboxCategoryErrorCode(ctx, err)
	slog.Warn("inbox summary: category source failed",
		"category", InboxCategoryChat,
		"error_code", code,
		"error", err)
	return InboxCategory{
		State:             InboxStateError,
		Count:             0,
		Capped:            false,
		CountsTowardTotal: true,
		Href:              inboxChatHref,
		ErrorCode:         code,
		Previews:          []any{},
	}
}

func chatUnreadMentionKeysBatched(ctx context.Context, wcs WebChatStore, userID string, keys []string, batch int) (map[string]bool, error) {
	out := make(map[string]bool)
	if len(keys) == 0 {
		return out, nil
	}
	if batch <= 0 {
		batch = defaultChatSpacesReadStateBatch
	}
	for start := 0; start < len(keys); start += batch {
		end := min(start+batch, len(keys))
		found, err := wcs.UnreadMentionKeys(ctx, userID, keys[start:end])
		if err != nil {
			return nil, fmt.Errorf("unread mentions: %w", err)
		}
		for k, v := range found {
			if v {
				out[k] = true
			}
		}
	}
	return out, nil
}

func (s *Server) buildInboxChatPreviews(
	ctx context.Context,
	detail chatUnreadDetail,
	mentionKeys map[string]bool,
	previews int,
	chatFilter string,
) []any {
	candidates := make([]inboxChatCandidate, 0, len(detail.unreadThreads)+len(detail.unreadDMs))

	if chatFilter == InboxChatFilterAll || chatFilter == InboxChatFilterDMs {
		for i := range detail.unreadDMs {
			ud := &detail.unreadDMs[i]
			candidates = append(candidates, inboxChatCandidate{
				kind:            InboxChatKindDM,
				conversationKey: ud.dm.ConversationKey,
				lastMessageAt:   ud.lastMessageAt,
				dm:              ud,
			})
		}
	}

	if chatFilter == InboxChatFilterAll || chatFilter == InboxChatFilterMentions {
		for i := range detail.unreadThreads {
			ut := &detail.unreadThreads[i]
			isMention := mentionKeys[ut.topic.ID]
			if chatFilter == InboxChatFilterMentions && !isMention {
				continue
			}
			kind := InboxChatKindThread
			if isMention {
				kind = InboxChatKindMention
			}
			lastAt := ut.topic.LastActivityAt
			if lastAt.IsZero() {
				lastAt = ut.topic.CreatedAt
			}
			candidates = append(candidates, inboxChatCandidate{
				kind:            kind,
				conversationKey: ut.topic.ID,
				lastMessageAt:   lastAt,
				thread:          ut,
			})
		}
	}

	sort.Slice(candidates, func(i, j int) bool {
		if !candidates[i].lastMessageAt.Equal(candidates[j].lastMessageAt) {
			return candidates[i].lastMessageAt.After(candidates[j].lastMessageAt)
		}
		return candidates[i].conversationKey < candidates[j].conversationKey
	})

	if len(candidates) > previews {
		candidates = candidates[:previews]
	}

	// Batch-load user peer labels only for the selected DM previews.
	var userPeerIDs []string
	seenUsers := make(map[string]bool)
	for _, c := range candidates {
		if c.dm != nil && c.dm.dm.PeerKind == "user" && c.dm.dm.PeerID != "" && !seenUsers[c.dm.dm.PeerID] {
			seenUsers[c.dm.dm.PeerID] = true
			userPeerIDs = append(userPeerIDs, c.dm.dm.PeerID)
		}
	}
	var peerUsers map[string]*store.User
	if len(userPeerIDs) > 0 {
		var err error
		if peerUsers, err = s.store.GetUsersByIDs(ctx, userPeerIDs); err != nil {
			slog.Warn("inbox summary: chat peer-user read failed",
				"users", len(userPeerIDs), "error", err)
		}
	}

	items := make([]any, 0, len(candidates))
	for _, c := range candidates {
		if c.thread != nil {
			ut := c.thread
			projectName := ""
			if ut.project != nil {
				projectName = ut.project.Name
			}
			items = append(items, InboxChatPreview{
				ID:              ut.topic.ID,
				Kind:            c.kind,
				ProjectID:       ut.topic.ProjectID,
				ProjectName:     projectName,
				CreatedAt:       c.lastMessageAt,
				Href:            inboxChatThreadHref(ut.project, ut.topic),
				ConversationKey: ut.topic.ID,
				Label:           inboxChatThreadLabel(ut.topic),
				LastMessageAt:   c.lastMessageAt,
			})
			continue
		}

		ud := c.dm
		projectID := ""
		projectName := ""
		if ud.dm.PeerKind == "agent" && ud.peerAgent != nil {
			if proj := detail.visibleSpaces[ud.peerAgent.ProjectID]; proj != nil {
				projectID = proj.ID
				projectName = proj.Name
			}
		}
		items = append(items, InboxChatPreview{
			ID:              ud.dm.ConversationKey,
			Kind:            InboxChatKindDM,
			ProjectID:       projectID,
			ProjectName:     projectName,
			CreatedAt:       c.lastMessageAt,
			Href:            chatDMWebPath(ud.dm.ConversationKey),
			ConversationKey: ud.dm.ConversationKey,
			Label:           inboxChatDMLabel(ud, peerUsers),
			LastMessageAt:   c.lastMessageAt,
		})
	}
	return items
}

func inboxChatThreadHref(proj *store.Project, topic WebChatTopic) string {
	if proj != nil && proj.Slug != "" {
		return "/chat/" + url.PathEscape(proj.Slug) + "/" + url.PathEscape(topic.ID)
	}
	return "/chat/space/" + url.PathEscape(topic.ProjectID) + "/thread/" + url.PathEscape(topic.ID)
}

func inboxChatThreadLabel(topic WebChatTopic) string {
	if topic.Name != "" {
		return topic.Name
	}
	if topic.IsGeneral {
		return "general"
	}
	return topic.ID
}

func inboxChatDMLabel(ud *chatUnreadDM, peerUsers map[string]*store.User) string {
	switch ud.dm.PeerKind {
	case "user":
		if u := peerUsers[ud.dm.PeerID]; u != nil {
			if u.DisplayName != "" {
				return u.DisplayName
			}
			if u.Email != "" {
				return u.Email
			}
		}
	case "agent":
		if ud.peerAgent != nil {
			if ud.peerAgent.Name != "" {
				return ud.peerAgent.Name
			}
			if ud.peerAgent.Slug != "" {
				return ud.peerAgent.Slug
			}
		}
	}
	return ud.dm.PeerID
}
