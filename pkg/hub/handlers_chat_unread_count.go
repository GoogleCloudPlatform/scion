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
	"net/http"
)

// chatUnreadCountResponse is the body of GET /api/v1/chat/unread-count.
type chatUnreadCountResponse struct {
	// Conversations is Threads + DMs: the number to show on the badge.
	Conversations int `json:"conversations"`
	// Threads counts the unmuted threads with an unread latest message in
	// the spaces the rail lists whose project the caller is a member of:
	// the sum of the unreadCount values GET /api/v1/chat/spaces reports.
	Threads int `json:"threads"`
	// DMs counts the caller's unmuted DMs with an unread latest message,
	// leaving out DMs with deleted agents.
	DMs int `json:"dms"`
}

// handleChatUnreadCount handles GET /api/v1/chat/unread-count: the number of
// conversations with unread messages, as the chat rail shows them.
//
// Threads are counted by the rollup GET /api/v1/chat/spaces uses
// (chatVisibleSpaces, chatMemberProjectIDs and chatSpaceRollups), so the
// badge's thread count is the sum of the rail's space badges: every
// unmuted thread with an unread latest message in a non-template project
// the caller can read and is an explicit member of (chatMemberProjectIDs;
// admin rights alone are not membership), whether or not the caller is a
// participant of the thread.
//
// A DM counts when the caller is one of its parties (webchat_dm), its peer
// is not a deleted agent, the caller has not muted it, and its latest
// message is not the caller's read watermark: the DMs the rail's Unread
// DMs list shows, from GET /api/v1/chat/dms.
//
// The cost is a constant number of batched queries however many
// conversations the caller has; it never fans out per project.
func (s *Server) handleChatUnreadCount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}
	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		Forbidden(w)
		return
	}

	resp, err := s.chatUnreadCount(r.Context(), GetIdentityFromContext(r.Context()), user.ID())
	if err != nil {
		s.messageLog.Error("chat unread count failed", "userID", user.ID(), "error", err)
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to count unread conversations", nil)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// chatUnreadCount computes the unread conversation count for userID, with
// identity deciding project read access.
func (s *Server) chatUnreadCount(ctx context.Context, identity Identity, userID string) (chatUnreadCountResponse, error) {
	var resp chatUnreadCountResponse

	s.mu.RLock()
	wcs := s.webChatStore
	s.mu.RUnlock()
	if wcs == nil {
		return resp, nil
	}

	spaces, err := s.chatVisibleSpaces(ctx, identity)
	if err != nil {
		return resp, err
	}
	members, err := s.chatMemberProjectIDs(ctx, userID)
	if err != nil {
		return resp, fmt.Errorf("member projects: %w", err)
	}
	rollups, err := chatSpaceRollupsStrict(ctx, wcs, userID, spaces, members, s.chatSpacesBatch)
	if err != nil {
		return resp, fmt.Errorf("space rollups: %w", err)
	}
	for _, ru := range rollups {
		resp.Threads += ru.unreadCount
	}

	dms, err := wcs.ListDMs(ctx, userID)
	if err != nil {
		return resp, fmt.Errorf("list DMs: %w", err)
	}
	// A DM with a deleted agent is not counted (the rail does not list it
	// as unread either); /chat/dms flags it as peerDeleted by the same rule.
	deletedPeers, err := s.chatDeletedAgentPeers(ctx, dms)
	if err != nil {
		return resp, err
	}
	dmKeys := make([]string, 0, len(dms))
	for _, dm := range dms {
		if dm.PeerKind == "agent" && deletedPeers[dm.PeerID] {
			continue
		}
		dmKeys = append(dmKeys, dm.ConversationKey)
	}
	// The DM list derives unread from the latest message, not from the
	// webchat_dm watermark; use the same source so the badge and the list
	// agree. Unlike the list, a failed read fails the count, as the thread
	// path does, rather than reporting every DM as read.
	dmLast, err := s.nativeDMLastMessagesStrict(ctx, dmKeys)
	if err != nil {
		return resp, fmt.Errorf("DM last messages: %w", err)
	}

	readMap, err := chatReadStatesBatched(ctx, wcs, userID, dmKeys, s.chatSpacesBatch.withDefaults().readStates)
	if err != nil {
		return resp, err
	}

	for _, key := range dmKeys {
		lastID := ""
		if msg := dmLast[key]; msg != nil {
			lastID = msg.ID
		}
		if chatConversationUnread(lastID, readMap, key) {
			resp.DMs++
		}
	}
	resp.Conversations = resp.Threads + resp.DMs
	return resp, nil
}

// chatConversationUnread reports whether a conversation whose latest message
// is lastMessageID counts as unread for the owner of readMap: it has a
// message, is not muted, and the read watermark is not at that message.
func chatConversationUnread(lastMessageID string, readMap map[string]WebChatReadState, key string) bool {
	if lastMessageID == "" {
		return false
	}
	rs, ok := readMap[key]
	if !ok {
		return true
	}
	return !rs.Muted && rs.LastReadMessageID != lastMessageID
}

// chatReadStatesBatched reads userID's read states for keys in batches of
// batch keys, keyed by conversation key. A batch of zero or less uses the
// default read-state batch size.
func chatReadStatesBatched(ctx context.Context, wcs WebChatStore, userID string, keys []string, batch int) (map[string]WebChatReadState, error) {
	if batch <= 0 {
		batch = defaultChatSpacesReadStateBatch
	}
	out := make(map[string]WebChatReadState, len(keys))
	for start := 0; start < len(keys); start += batch {
		end := min(start+batch, len(keys))
		states, err := wcs.GetReadStates(ctx, userID, keys[start:end])
		if err != nil {
			return nil, fmt.Errorf("read states: %w", err)
		}
		for _, rs := range states {
			out[rs.ConversationKey] = rs
		}
	}
	return out, nil
}
