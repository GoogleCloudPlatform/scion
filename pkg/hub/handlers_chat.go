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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
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

	// Verify the caller has read access to the project (security fix — wave-2 §4.2).
	project, err := s.store.GetProject(r.Context(), projectID)
	if err != nil {
		NotFound(w, "Project")
		return
	}
	if !s.authorize(w, r, projectResource(project), ActionRead) {
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

	// Batch-fetch the last messages by ID (design Section 5.3).
	messageIDs := make([]string, 0, len(threads))
	for _, t := range threads {
		if t.LastMessageID != "" {
			messageIDs = append(messageIDs, t.LastMessageID)
		}
	}

	rawMessages, err := s.store.GetMessagesByIDs(r.Context(), messageIDs)
	if err != nil {
		rawMessages = nil // degrade gracefully — previews will be empty
	}

	messageMap := make(map[string]lastMessageInfo, len(rawMessages))
	for id, msg := range rawMessages {
		messageMap[id] = lastMessageInfo{
			Msg:       truncatePreview(msg.Msg, 120),
			Sender:    msg.Sender,
			CreatedAt: msg.CreatedAt,
			Type:      msg.Type,
		}
	}

	// Batch-fetch agents by ID. Collect all agent IDs from threads, then
	// look them up in a single query instead of N individual GetAgent calls.
	agentIDs := make([]string, 0, len(threads))
	for _, t := range threads {
		agentIDs = append(agentIDs, t.AgentID)
	}

	agentMap, err := s.store.GetAgentsByIDs(r.Context(), agentIDs)
	if err != nil {
		agentMap = nil // degrade gracefully — names will be empty
	}

	// For agents not found by ID (may be stored as slugs), try slug lookup.
	// Collect missing IDs and batch-query by slug using GetAgentBySlug.
	// This path handles the case where webchat_thread stores slugs instead
	// of UUIDs (see webchannel.go O1 comment).
	slugLookups := make([]string, 0)
	for _, t := range threads {
		if _, ok := agentMap[t.AgentID]; !ok {
			slugLookups = append(slugLookups, t.AgentID)
		}
	}
	slugAgentMap := make(map[string]*store.Agent)
	for _, slug := range slugLookups {
		agent, err := s.store.GetAgentBySlug(r.Context(), projectID, slug)
		if err == nil && agent != nil {
			slugAgentMap[slug] = agent
		}
	}

	// Build response entries, joining with pre-fetched data.
	entries := make([]chatThreadEntry, 0, len(threads))
	for _, t := range threads {
		entry := chatThreadEntry{
			AgentID: t.AgentID,
		}

		// Enrich with agent metadata from the batch-fetched maps.
		if agent, ok := agentMap[t.AgentID]; ok {
			entry.AgentID = agent.ID
			entry.AgentSlug = agent.Slug
			entry.AgentName = agent.Name
			entry.Phase = agent.Phase
			entry.Activity = agent.Activity
		} else if agent, ok := slugAgentMap[t.AgentID]; ok {
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
