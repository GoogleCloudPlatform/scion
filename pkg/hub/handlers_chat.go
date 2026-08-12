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
	"net/http"
	"strconv"
	"strings"
	"time"
)

// handleChatThreads handles GET /api/v1/chat/threads.
// Returns the thread rail for the authenticated user: a list of agents
// they have conversed with, each with last-message preview and an
// unread indicator. Reads from webchat_thread — no aggregate query
// over the messages table (AC19a).
func (s *Server) handleChatThreads(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		Forbidden(w)
		return
	}

	if s.webChatStore == nil {
		writeJSON(w, http.StatusOK, chatThreadsResponse{Threads: []chatThreadEntry{}})
		return
	}

	q := r.URL.Query()
	projectID := q.Get("projectId")
	if projectID == "" {
		BadRequest(w, "projectId is required")
		return
	}

	limit := 50 // default
	if limitStr := q.Get("limit"); limitStr != "" {
		if n, err := strconv.Atoi(limitStr); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}

	threads, err := s.webChatStore.GetThreads(r.Context(), user.ID(), projectID, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to fetch threads"})
		return
	}

	// Batch-fetch the last messages by ID.
	messageIDs := make([]string, 0, len(threads))
	for _, t := range threads {
		if t.LastMessageID != "" {
			messageIDs = append(messageIDs, t.LastMessageID)
		}
	}

	messageMap := make(map[string]lastMessageInfo, len(messageIDs))
	for _, id := range messageIDs {
		msg, err := s.store.GetMessage(r.Context(), id)
		if err != nil || msg == nil {
			continue
		}
		messageMap[id] = lastMessageInfo{
			Msg:       truncatePreview(msg.Msg, 120),
			Sender:    msg.Sender,
			CreatedAt: msg.CreatedAt,
			Type:      msg.Type,
		}
	}

	// Build response entries, joining with agent info from the store.
	entries := make([]chatThreadEntry, 0, len(threads))
	for _, t := range threads {
		entry := chatThreadEntry{
			AgentID: t.AgentID,
		}

		// Try to enrich with agent metadata from the store.
		// The agentID in webchat_thread may be a slug or UUID depending on
		// how the thread was created (see webchannel.go O1 comment).
		agent, err := s.store.GetAgent(r.Context(), t.AgentID)
		if err != nil {
			// Try by slug within the project
			agent, err = s.store.GetAgentBySlug(r.Context(), projectID, t.AgentID)
		}
		if err == nil && agent != nil {
			entry.AgentID = agent.ID
			entry.AgentSlug = agent.Slug
			entry.AgentName = agent.Name
			entry.Phase = agent.Phase
			entry.Activity = agent.Activity
		}

		// Add last message preview
		if info, ok := messageMap[t.LastMessageID]; ok {
			entry.LastMessage = &info
		}

		// Compute unread: last_activity_at > last_read_at (AC19a — pure comparison, no count)
		entry.HasUnread = t.LastReadAt == nil || t.LastActivityAt.After(*t.LastReadAt)

		entries = append(entries, entry)
	}

	writeJSON(w, http.StatusOK, chatThreadsResponse{Threads: entries})
}

// handleChatThreadRoutes dispatches sub-routes under /api/v1/chat/threads/.
// Currently handles POST /api/v1/chat/threads/{agentId}/read.
func (s *Server) handleChatThreadRoutes(w http.ResponseWriter, r *http.Request) {
	// Parse: /api/v1/chat/threads/{agentId}/read
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/chat/threads/")
	parts := strings.SplitN(path, "/", 2)

	if len(parts) < 2 || parts[1] != "read" {
		http.NotFound(w, r)
		return
	}

	agentID := parts[0]
	if agentID == "" {
		BadRequest(w, "agentId is required")
		return
	}

	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		Forbidden(w)
		return
	}

	if s.webChatStore == nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}

	q := r.URL.Query()
	projectID := q.Get("projectId")
	if projectID == "" {
		BadRequest(w, "projectId is required")
		return
	}

	if err := s.webChatStore.MarkThreadRead(r.Context(), user.ID(), projectID, agentID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to mark thread read"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// --- Response types ---

type chatThreadsResponse struct {
	Threads []chatThreadEntry `json:"threads"`
}

type chatThreadEntry struct {
	AgentID     string           `json:"agentId"`
	AgentSlug   string           `json:"agentSlug,omitempty"`
	AgentName   string           `json:"agentName,omitempty"`
	Phase       string           `json:"phase,omitempty"`
	Activity    string           `json:"activity,omitempty"`
	LastMessage *lastMessageInfo `json:"lastMessage,omitempty"`
	HasUnread   bool             `json:"hasUnread"`
}

type lastMessageInfo struct {
	Msg       string    `json:"msg"`
	Sender    string    `json:"sender"`
	CreatedAt time.Time `json:"createdAt"`
	Type      string    `json:"type"`
}

// truncatePreview truncates a message to maxLen runes for rail preview.
func truncatePreview(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen]) + "..."
}
