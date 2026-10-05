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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// chatUnreadCountMaxProjects bounds how many distinct projects the unread
// count authorizes in one request. Member threads in projects past the
// bound are not counted; hitting it is logged.
const chatUnreadCountMaxProjects = 1000

// chatUnreadCountResponse is the body of GET /api/v1/chat/unread-count.
type chatUnreadCountResponse struct {
	// Conversations is Threads + DMs: the number to show on the badge.
	Conversations int `json:"conversations"`
	// Threads counts unmuted threads the caller is a member of, can read,
	// and has not read to the latest message.
	Threads int `json:"threads"`
	// DMs counts the caller's unmuted DMs with an unread latest message.
	DMs int `json:"dms"`
}

// handleChatUnreadCount handles GET /api/v1/chat/unread-count: the number of
// conversations with unread messages that the caller is a member of.
//
// A thread counts when the caller has an active user row in
// conversation_participants for the thread's conversation, can read the
// thread's project, has not muted the thread, and the thread's latest
// message is not the caller's read watermark. Participant rows are a
// listing index only, so the project read gate still applies. A DM counts
// when the caller is one of its parties (webchat_dm), has not muted it,
// and its latest message is not the caller's read watermark.
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

	topics, err := s.memberThreads(ctx, wcs, identity, userID)
	if err != nil {
		return resp, err
	}

	dms, err := wcs.ListDMs(ctx, userID)
	if err != nil {
		return resp, fmt.Errorf("list DMs: %w", err)
	}
	dmKeys := make([]string, 0, len(dms))
	for _, dm := range dms {
		dmKeys = append(dmKeys, dm.ConversationKey)
	}
	// The DM list derives unread from the latest message, not from the
	// webchat_dm watermark; use the same source so the badge and the list
	// agree.
	dmLast := s.nativeDMLastMessages(ctx, dmKeys)

	keys := make([]string, 0, len(topics)+len(dmKeys))
	for _, t := range topics {
		keys = append(keys, t.ID)
	}
	keys = append(keys, dmKeys...)
	readMap, err := chatReadStatesBatched(ctx, wcs, userID, keys, s.chatSpacesBatch.withDefaults().readStates)
	if err != nil {
		return resp, err
	}

	for _, t := range topics {
		if chatConversationUnread(t.LastMessageID, readMap, t.ID) {
			resp.Threads++
		}
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

// memberThreads returns the non-deleted topics whose conversation userID is
// an active participant of and whose project identity can read.
func (s *Server) memberThreads(ctx context.Context, wcs WebChatStore, identity Identity, userID string) ([]WebChatTopic, error) {
	convs, err := s.store.GetConversationsForPrincipal(ctx, "user", userID)
	if err != nil {
		return nil, fmt.Errorf("list member conversations: %w", err)
	}

	memberConvs := make(map[string]bool, len(convs))
	var projectIDs []string
	seenProject := make(map[string]bool)
	for _, c := range convs {
		if c.Kind != "group" || c.ProjectID == nil || *c.ProjectID == "" ||
			c.DeletedAt != nil || c.ArchivedAt != nil {
			continue
		}
		memberConvs[c.ID] = true
		if pid := *c.ProjectID; !seenProject[pid] {
			seenProject[pid] = true
			projectIDs = append(projectIDs, pid)
		}
	}
	if len(projectIDs) == 0 {
		return nil, nil
	}
	if len(projectIDs) > chatUnreadCountMaxProjects {
		s.messageLog.Warn("chat unread count: member projects over bound; truncating",
			"userID", userID, "projects", len(projectIDs), "bound", chatUnreadCountMaxProjects)
		projectIDs = projectIDs[:chatUnreadCountMaxProjects]
	}

	// Project read gate: participant rows are a listing index, not authz.
	projects, err := s.store.ListProjectSummaries(ctx,
		store.ProjectFilter{MemberProjectIDs: projectIDs},
		store.ListOptions{Limit: chatUnreadCountMaxProjects})
	if err != nil {
		return nil, fmt.Errorf("list member projects: %w", err)
	}
	resources := make([]Resource, len(projects.Items))
	for i := range projects.Items {
		resources[i] = projectResource(&projects.Items[i])
	}
	caps := s.authzService.ComputeCapabilitiesForActions(ctx, identity, resources, []Action{ActionRead})
	readable := make([]string, 0, len(projects.Items))
	for i := range projects.Items {
		if capabilityAllows(caps[i], ActionRead) {
			readable = append(readable, projects.Items[i].ID)
		}
	}

	batch := s.chatSpacesBatch.withDefaults().topics
	var out []WebChatTopic
	for start := 0; start < len(readable); start += batch {
		end := min(start+batch, len(readable))
		page, err := wcs.ListTopicsByProjects(ctx, readable[start:end])
		if err != nil {
			return nil, fmt.Errorf("list member topics: %w", err)
		}
		for _, t := range page {
			if t.ConversationID != "" && memberConvs[t.ConversationID] {
				out = append(out, t)
			}
		}
	}
	return out, nil
}

// chatReadStatesBatched reads userID's read states for keys in batches of
// batch keys, keyed by conversation key.
func chatReadStatesBatched(ctx context.Context, wcs WebChatStore, userID string, keys []string, batch int) (map[string]WebChatReadState, error) {
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
