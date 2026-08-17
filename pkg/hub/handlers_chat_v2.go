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

// Wave-2 Native Chat Handlers
//
// This file implements the wave-2 chat API: spaces, shared threads, DMs,
// members, presence, typing indicators, notifications, attachments, and search.
//
// Architecture overview:
//
//   - Spaces are derived from projects — there is no separate space entity.
//     The space list is the set of projects the caller can ActionRead.
//   - Threads (webchat_topic) are shared, multi-participant conversations
//     within a space. Each space has an auto-created #general thread.
//   - DMs are identified by a canonical pair key (dm:agent:<uuid>:user:<uuid>
//     or dm:user:<uuid>:user:<uuid>) and are global, not project-scoped.
//   - Messages are persisted in the existing messages table with ThreadID
//     set to the topic UUID or DM key. Routing follows the three-tier model:
//     explicit @mentions → mentioned agents, else thread default_agent → that
//     agent, else no agent engaged (type:chat, human-to-human).
//   - Real-time delivery uses SSE via the stateManager: project-scoped
//     subjects for space threads, fan-out for DMs. Per-thread EventSource
//     (wave-1) is replaced by a single multiplexed connection.
//   - Storage uses the dual-dialect store (webchannel_store.go for SQLite,
//     webchannel_store_postgres.go for Postgres) with new webchat_topic,
//     webchat_read_state, webchat_user_prefs, and webchat_dm tables.
//   - Wave-1 tables (webchat_thread, webchat_thread_prefs) remain in place
//     but receive no new writes when the v2 flag is ON (write-stop).
//
// Feature flag: web.native_chat_v2 (default ON as of W9). When OFF, the
// frontend falls back to the wave-1 UI and endpoints in handlers_chat.go.
// The v2 API endpoints remain registered regardless of the flag state.

package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Route dispatchers
// ---------------------------------------------------------------------------

// handleChatSpaces handles GET /api/v1/chat/spaces.
// Returns visible spaces (projects the caller can read) with unread rollup
// and sort prefs.
func (s *Server) handleChatSpaces(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		Forbidden(w)
		return
	}

	ctx := r.Context()

	s.mu.RLock()
	wcs := s.webChatStore
	s.mu.RUnlock()

	if wcs == nil {
		writeJSON(w, http.StatusOK, chatSpacesResponse{Spaces: []chatSpaceEntry{}})
		return
	}

	// List all projects and filter by ActionRead using batch capability check.
	allProjects, err := s.store.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{Limit: 1000})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to list projects", nil)
		return
	}

	identity := GetIdentityFromContext(ctx)
	resources := make([]Resource, len(allProjects.Items))
	for i := range allProjects.Items {
		resources[i] = projectResource(&allProjects.Items[i])
	}
	caps := s.authzService.ComputeCapabilitiesBatch(ctx, identity, resources, "project")

	// Get user prefs.
	prefs, _ := wcs.GetUserPrefs(ctx, user.ID())

	var spaces []chatSpaceEntry
	for i, p := range allProjects.Items {
		if !capabilityAllows(caps[i], ActionRead) {
			continue
		}

		// Get topics for this space to compute unread count.
		topics, _ := wcs.ListTopics(ctx, p.ID)
		convKeys := make([]string, 0, len(topics))
		for _, t := range topics {
			convKeys = append(convKeys, t.ID)
		}

		var unreadCount int
		if len(convKeys) > 0 {
			readStates, _ := wcs.GetReadStates(ctx, user.ID(), convKeys)
			readMap := make(map[string]WebChatReadState, len(readStates))
			for _, rs := range readStates {
				readMap[rs.ConversationKey] = rs
			}
			for _, t := range topics {
				rs, ok := readMap[t.ID]
				// A muted thread is silent all the way up: it contributes
				// nothing to the space badge, so muting every unread thread in
				// a space clears the badge instead of leaving the space
				// shouting about threads the user asked to be quiet (#1029).
				// Mentions are covered by the same rule — the rail already
				// hides the mention dot on a muted thread, and a rollup that
				// disagreed with it would put two numbers on screen.
				if ok && rs.Muted {
					continue
				}
				if !ok || rs.LastReadMessageID == "" || (t.LastMessageID != "" && t.LastMessageID != rs.LastReadMessageID) {
					if t.LastMessageID != "" {
						unreadCount++
					}
				}
			}
		}

		spaces = append(spaces, chatSpaceEntry{
			ProjectID:   p.ID,
			ProjectName: p.Name,
			ProjectSlug: p.Slug,
			ThreadCount: len(topics),
			UnreadCount: unreadCount,
		})
	}

	if spaces == nil {
		spaces = []chatSpaceEntry{}
	}

	resp := chatSpacesResponse{
		Spaces: spaces,
	}
	if prefs != nil {
		resp.Prefs = &chatSpacePrefs{
			SpaceSortMode:  prefs.SpaceSortMode,
			SpaceOrder:     prefs.SpaceOrder,
			ThreadSortMode: prefs.ThreadSortMode,
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleChatSpaceRoutes dispatches sub-routes under /api/v1/chat/spaces/.
func (s *Server) handleChatSpaceRoutes(w http.ResponseWriter, r *http.Request) {
	// Parse: /api/v1/chat/spaces/{projectId}/threads
	//        /api/v1/chat/spaces/{projectId}/members
	//        /api/v1/chat/spaces/{projectId}/read
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/chat/spaces/")
	parts := strings.SplitN(path, "/", 2)

	if len(parts) < 2 {
		http.NotFound(w, r)
		return
	}

	projectID := parts[0]
	if projectID == "" {
		BadRequest(w, "projectId is required")
		return
	}

	action := parts[1]
	switch action {
	case "threads":
		s.handleSpaceThreads(w, r, projectID)
	case "members":
		s.handleSpaceMembers(w, r, projectID)
	case "read":
		s.handleSpaceRead(w, r, projectID)
	default:
		http.NotFound(w, r)
	}
}

// handleChatConversationRoutes dispatches sub-routes under /api/v1/chat/conversations/.
func (s *Server) handleChatConversationRoutes(w http.ResponseWriter, r *http.Request) {
	// Parse: /api/v1/chat/conversations/{key}/messages
	//        /api/v1/chat/conversations/{key}/read
	//        /api/v1/chat/conversations/{key}/typing
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/chat/conversations/")
	parts := strings.SplitN(path, "/", 2)

	if len(parts) < 2 {
		http.NotFound(w, r)
		return
	}

	key := parts[0]
	if key == "" {
		BadRequest(w, "conversation key is required")
		return
	}

	action := parts[1]
	// Check for sub-resource under messages (e.g., messages/{id}).
	if strings.HasPrefix(action, "messages/") {
		messageID := strings.TrimPrefix(action, "messages/")
		if messageID == "" {
			BadRequest(w, "message ID required")
			return
		}
		switch r.Method {
		case http.MethodPut:
			s.handleMessageEdit(w, r, key, messageID)
		case http.MethodDelete:
			s.handleMessageDelete(w, r, key, messageID)
		default:
			MethodNotAllowed(w)
		}
		return
	}
	switch action {
	case "messages":
		s.handleConversationMessages(w, r, key)
	case "read":
		s.handleConversationRead(w, r, key)
	case "typing":
		s.handleConversationTyping(w, r, key)
	case "interagent":
		s.handleConversationInteragent(w, r, key)
	case "mute":
		s.handleConversationMute(w, r, key)
	case "pin":
		s.handleConversationPin(w, r, key)
	default:
		http.NotFound(w, r)
	}
}

// handleChatTopicRoutes dispatches routes under /api/v1/chat/threads/ for
// wave-2 topic-level operations (PATCH, DELETE by topicId).
// The existing handleChatThreadRoutes handles the wave-1 {agentId}/read path.
// We register this separately on a path that doesn't conflict.
func (s *Server) handleChatTopicRoutes(w http.ResponseWriter, r *http.Request) {
	// Parse: /api/v1/chat/topics/{topicId}
	topicID := strings.TrimPrefix(r.URL.Path, "/api/v1/chat/topics/")
	if topicID == "" {
		BadRequest(w, "topicId is required")
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.handleTopicGet(w, r, topicID)
	case http.MethodPatch:
		s.handleTopicPatch(w, r, topicID)
	case http.MethodDelete:
		s.handleTopicDelete(w, r, topicID)
	default:
		MethodNotAllowed(w)
	}
}

// ---------------------------------------------------------------------------
// Thread CRUD
// ---------------------------------------------------------------------------

// handleSpaceThreads handles GET and POST /api/v1/chat/spaces/{projectId}/threads.
func (s *Server) handleSpaceThreads(w http.ResponseWriter, r *http.Request, projectID string) {
	switch r.Method {
	case http.MethodGet:
		s.handleListThreads(w, r, projectID)
	case http.MethodPost:
		s.handleCreateThread(w, r, projectID)
	default:
		MethodNotAllowed(w)
	}
}

// handleListThreads returns all non-deleted topics for a project, with per-user read state.
func (s *Server) handleListThreads(w http.ResponseWriter, r *http.Request, projectID string) {
	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		Forbidden(w)
		return
	}

	// Authorize project access.
	project, err := s.store.GetProject(r.Context(), projectID)
	if err != nil {
		NotFound(w, "Project")
		return
	}
	if !s.authorize(w, r, projectResource(project), ActionRead) {
		return
	}

	s.mu.RLock()
	wcs := s.webChatStore
	s.mu.RUnlock()

	if wcs == nil {
		writeJSON(w, http.StatusOK, chatTopicListResponse{Threads: []chatTopicEntry{}})
		return
	}

	// ListTopics lazily creates #general.
	topics, err := wcs.ListTopics(r.Context(), projectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to list threads", nil)
		return
	}

	// Batch-fetch read states.
	convKeys := make([]string, 0, len(topics))
	for _, t := range topics {
		convKeys = append(convKeys, t.ID)
	}
	readStates, _ := wcs.GetReadStates(r.Context(), user.ID(), convKeys)
	readMap := make(map[string]WebChatReadState, len(readStates))
	for _, rs := range readStates {
		readMap[rs.ConversationKey] = rs
	}

	entries := make([]chatTopicEntry, 0, len(topics))
	for _, t := range topics {
		entry := chatTopicEntry{
			ID:             t.ID,
			ProjectID:      t.ProjectID,
			Name:           t.Name,
			IsGeneral:      t.IsGeneral,
			DefaultAgent:   t.DefaultAgent,
			CreatedBy:      t.CreatedBy,
			CreatedAt:      t.CreatedAt,
			LastMessageID:  t.LastMessageID,
			LastActivityAt: t.LastActivityAt,
		}
		if rs, ok := readMap[t.ID]; ok {
			entry.LastReadMessageID = rs.LastReadMessageID
			entry.Pinned = rs.Pinned
			entry.Muted = rs.Muted
			entry.HasUnread = t.LastMessageID != "" && t.LastMessageID != rs.LastReadMessageID
		} else {
			entry.HasUnread = t.LastMessageID != ""
		}
		entries = append(entries, entry)
	}

	writeJSON(w, http.StatusOK, chatTopicListResponse{Threads: entries})
}

// threadNameRegexp validates thread names: no special characters.
var threadNameRegexp = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9 _\-]*$`)

// dmKeyRegexp validates DM conversation keys.
// Format: dm:(user|agent):<uuid>:(user|agent):<uuid>
var dmKeyRegexp = regexp.MustCompile(`^dm:(user|agent):[0-9a-f-]{36}:(user|agent):[0-9a-f-]{36}$`)

// validDMKey returns true if the key matches the expected DM key format.
func validDMKey(key string) bool {
	return dmKeyRegexp.MatchString(key)
}

// handleCreateThread creates a new thread in a space.
func (s *Server) handleCreateThread(w http.ResponseWriter, r *http.Request, projectID string) {
	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		Forbidden(w)
		return
	}

	project, err := s.store.GetProject(r.Context(), projectID)
	if err != nil {
		NotFound(w, "Project")
		return
	}
	if !s.authorize(w, r, projectResource(project), ActionRead) {
		return
	}

	s.mu.RLock()
	wcs := s.webChatStore
	s.mu.RUnlock()

	if wcs == nil {
		writeError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Chat not available", nil)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1048576)
	var body struct {
		Name         string `json:"name"`
		DefaultAgent string `json:"defaultAgent,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		BadRequest(w, "invalid request body")
		return
	}

	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" {
		ValidationError(w, "name is required", nil)
		return
	}
	nameRunes := []rune(body.Name)
	if len(nameRunes) > 100 {
		ValidationError(w, "name must be 100 characters or fewer", nil)
		return
	}
	if !threadNameRegexp.MatchString(body.Name) {
		ValidationError(w, "name contains invalid characters", nil)
		return
	}

	topicID := uuid.New().String()
	now := time.Now().UTC()
	topic := WebChatTopic{
		ID:             topicID,
		ProjectID:      projectID,
		Name:           body.Name,
		DefaultAgent:   body.DefaultAgent,
		CreatedBy:      user.ID(),
		CreatedAt:      now,
		LastActivityAt: now,
	}

	if err := wcs.CreateTopic(r.Context(), topic); err != nil {
		if strings.Contains(err.Error(), "name conflict") || strings.Contains(err.Error(), "UNIQUE constraint") {
			ValidationError(w, "a thread with that name already exists in this space", nil)
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to create thread", nil)
		return
	}

	// Publish topic created event.
	s.events.PublishChatTopicEvent(r.Context(), projectID, "created", topic)

	writeJSON(w, http.StatusCreated, topic)
}

// handleTopicGet handles GET /api/v1/chat/topics/{topicId}.
func (s *Server) handleTopicGet(w http.ResponseWriter, r *http.Request, topicID string) {
	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		Forbidden(w)
		return
	}

	s.mu.RLock()
	wcs := s.webChatStore
	s.mu.RUnlock()

	if wcs == nil {
		writeError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Chat not available", nil)
		return
	}

	topic, err := wcs.GetTopic(r.Context(), topicID)
	if err != nil || topic == nil {
		NotFound(w, "Thread")
		return
	}

	// Authorize project access.
	project, err := s.store.GetProject(r.Context(), topic.ProjectID)
	if err != nil {
		NotFound(w, "Project")
		return
	}
	if !s.authorize(w, r, projectResource(project), ActionRead) {
		return
	}

	writeJSON(w, http.StatusOK, topic)
}

// handleTopicPatch handles PATCH /api/v1/chat/topics/{topicId}.
func (s *Server) handleTopicPatch(w http.ResponseWriter, r *http.Request, topicID string) {
	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		Forbidden(w)
		return
	}

	s.mu.RLock()
	wcs := s.webChatStore
	s.mu.RUnlock()

	if wcs == nil {
		writeError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Chat not available", nil)
		return
	}

	topic, err := wcs.GetTopic(r.Context(), topicID)
	if err != nil || topic == nil {
		NotFound(w, "Thread")
		return
	}

	// Authorize project access.
	project, err := s.store.GetProject(r.Context(), topic.ProjectID)
	if err != nil {
		NotFound(w, "Project")
		return
	}
	if !s.authorize(w, r, projectResource(project), ActionRead) {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1048576)
	var body struct {
		Name         *string `json:"name"`
		DefaultAgent *string `json:"defaultAgent"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		BadRequest(w, "invalid request body")
		return
	}

	updates := TopicUpdate{}

	if body.Name != nil {
		name := strings.TrimSpace(*body.Name)
		if topic.IsGeneral {
			ValidationError(w, "cannot rename the #general thread", nil)
			return
		}
		if name == "" {
			ValidationError(w, "name cannot be empty", nil)
			return
		}
		nameRunes := []rune(name)
		if len(nameRunes) > 100 {
			ValidationError(w, "name must be 100 characters or fewer", nil)
			return
		}
		if !threadNameRegexp.MatchString(name) {
			ValidationError(w, "name contains invalid characters", nil)
			return
		}
		updates.Name = &name
	}

	if body.DefaultAgent != nil {
		updates.DefaultAgent = body.DefaultAgent
	}

	if updates.Name == nil && updates.DefaultAgent == nil {
		writeJSON(w, http.StatusOK, topic)
		return
	}

	if err := wcs.UpdateTopic(r.Context(), topicID, updates); err != nil {
		if strings.Contains(err.Error(), "name conflict") || strings.Contains(err.Error(), "UNIQUE constraint") {
			ValidationError(w, "a thread with that name already exists in this space", nil)
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to update thread", nil)
		return
	}

	// Fetch updated topic for response.
	updated, err := wcs.GetTopic(r.Context(), topicID)
	if err != nil || updated == nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to fetch updated thread", nil)
		return
	}

	s.events.PublishChatTopicEvent(r.Context(), updated.ProjectID, "updated", *updated)

	writeJSON(w, http.StatusOK, updated)
}

// handleTopicDelete handles DELETE /api/v1/chat/topics/{topicId}.
func (s *Server) handleTopicDelete(w http.ResponseWriter, r *http.Request, topicID string) {
	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		Forbidden(w)
		return
	}

	s.mu.RLock()
	wcs := s.webChatStore
	s.mu.RUnlock()

	if wcs == nil {
		writeError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Chat not available", nil)
		return
	}

	topic, err := wcs.GetTopic(r.Context(), topicID)
	if err != nil || topic == nil {
		NotFound(w, "Thread")
		return
	}

	project, err := s.store.GetProject(r.Context(), topic.ProjectID)
	if err != nil {
		NotFound(w, "Project")
		return
	}
	if !s.authorize(w, r, projectResource(project), ActionRead) {
		return
	}

	if err := wcs.DeleteTopic(r.Context(), topicID); err != nil {
		if strings.Contains(err.Error(), "#general") {
			ValidationError(w, "cannot delete the #general thread", nil)
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to delete thread", nil)
		return
	}

	s.events.PublishChatTopicEvent(r.Context(), topic.ProjectID, "deleted", *topic)

	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// ClearTopicDefaultAgent drops the default-agent binding from every topic in
// the agent's project that points at it, and republishes each affected topic
// so open clients stop offering a deleted agent as the thread's default.
//
// default_agent holds either an agent ID or a slug (the send path resolves
// both), so both are matched. Best-effort: a failure here leaves a stale
// default that the send path already tolerates by falling back to no agent.
func (s *Server) ClearTopicDefaultAgent(ctx context.Context, agentID, agentSlug, projectID string) {
	if projectID == "" || (agentID == "" && agentSlug == "") {
		return
	}

	s.mu.RLock()
	wcs := s.webChatStore
	s.mu.RUnlock()
	if wcs == nil {
		return
	}

	topics, err := wcs.ListTopics(ctx, projectID)
	if err != nil {
		slog.Warn("Failed to list topics while clearing deleted agent default",
			"project_id", projectID, "agent_id", agentID, "error", err)
		return
	}

	cleared := ""
	for _, t := range topics {
		if t.DefaultAgent == "" {
			continue
		}
		if t.DefaultAgent != agentID && t.DefaultAgent != agentSlug {
			continue
		}
		if err := wcs.UpdateTopic(ctx, t.ID, TopicUpdate{DefaultAgent: &cleared}); err != nil {
			slog.Warn("Failed to clear deleted agent as thread default",
				"topic_id", t.ID, "agent_id", agentID, "error", err)
			continue
		}
		t.DefaultAgent = ""
		s.events.PublishChatTopicEvent(ctx, projectID, "updated", t)
	}
}

// ---------------------------------------------------------------------------
// Send Path (the core of W2)
// ---------------------------------------------------------------------------

// handleConversationMessages handles GET and POST /api/v1/chat/conversations/{key}/messages.
func (s *Server) handleConversationMessages(w http.ResponseWriter, r *http.Request, key string) {
	switch r.Method {
	case http.MethodGet:
		s.handleConversationHistory(w, r, key)
	case http.MethodPost:
		s.handleConversationSend(w, r, key)
	default:
		MethodNotAllowed(w)
	}
}

// handleConversationSend implements POST /api/v1/chat/conversations/{key}/messages.
// This is the wave-2 send path with full routing per design §3/§4.3.
func (s *Server) handleConversationSend(w http.ResponseWriter, r *http.Request, key string) {
	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		Forbidden(w)
		return
	}

	ctx := r.Context()

	s.mu.RLock()
	wcs := s.webChatStore
	s.mu.RUnlock()

	if wcs == nil {
		writeError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Chat not available", nil)
		return
	}

	// --- Authorize ---
	var projectID string
	isDM := strings.HasPrefix(key, "dm:")
	if isDM {
		// Validate DM key format before any further processing.
		if !validDMKey(key) {
			BadRequest(w, "invalid DM key format")
			return
		}
		// DM key: verify the caller is one of the two participants.
		if !isDMParticipant(key, user.ID()) {
			Forbidden(w)
			return
		}
		// DMs are not project-scoped; derive project from agent if it's an
		// agent DM, or skip project check for user-user DMs.
	} else {
		// Topic key: look up topic to get project ID and check access.
		topic, err := wcs.GetTopic(ctx, key)
		if err != nil || topic == nil {
			NotFound(w, "Thread")
			return
		}
		projectID = topic.ProjectID
		project, err := s.store.GetProject(ctx, projectID)
		if err != nil {
			NotFound(w, "Project")
			return
		}
		if !s.authorize(w, r, projectResource(project), ActionRead) {
			return
		}
	}

	// --- Rate limit (#1054) ---
	// After authorization so an unauthorized caller cannot consume a
	// legitimate sender's allowance, and before the body is read so a flood
	// costs the hub as little as possible.
	//
	// Always the human class: this handler rejects anything that is not a
	// UserIdentity above, which is exactly why agent senders need their own
	// limit on the outbound-message path.
	if !s.allowChatSend(w, user.ID(), chatSenderHuman) {
		return
	}

	// --- Validate body ---
	r.Body = http.MaxBytesReader(w, r.Body, 1048576)
	var body struct {
		Content        string   `json:"content"`
		Attachments    []string `json:"attachments,omitempty"` // W7: attachment IDs
		ReplyToID      string   `json:"reply_to_id,omitempty"` // Phase-3: reply/quote
		IdempotencyKey string   `json:"idempotency_key,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		BadRequest(w, "invalid request body")
		return
	}

	content := strings.TrimSpace(body.Content)
	if content == "" && len(body.Attachments) == 0 {
		ValidationError(w, "content or attachments required", nil)
		return
	}
	if utf8.RuneCountInString(content) > messages.MaxMessageLength {
		ValidationError(w, fmt.Sprintf("message exceeds %d character limit", messages.MaxMessageLength), nil)
		return
	}
	if len(body.Attachments) > MaxAttachmentsPerMessage {
		ValidationError(w, fmt.Sprintf("too many attachments: %d (max %d)", len(body.Attachments), MaxAttachmentsPerMessage), nil)
		return
	}

	// W7: Validate attachment IDs and collect metadata.
	var attachmentRefs []AttachmentRef
	if len(body.Attachments) > 0 && wcs != nil {
		for _, aid := range body.Attachments {
			meta, err := wcs.GetAttachment(ctx, aid)
			if err != nil || meta == nil {
				ValidationError(w, fmt.Sprintf("attachment %q not found", aid), nil)
				return
			}
			// Verify the attachment belongs to the correct project.
			if projectID != "" && meta.ProjectID != projectID {
				ValidationError(w, fmt.Sprintf("attachment %q does not belong to this project", aid), nil)
				return
			}
			attachmentRefs = append(attachmentRefs, AttachmentRef{
				ID:       meta.ID,
				Name:     meta.Filename,
				MimeType: meta.MimeType,
				Size:     meta.Size,
			})
		}
	}

	// --- Idempotency check (#1055) ---
	// If the client supplied an idempotency key, check whether a message with
	// that key from this sender was already created recently. If so, return
	// the existing message ID (200 OK) instead of creating a duplicate.
	if body.IdempotencyKey != "" {
		if existingID, ok := s.chatIdempotency.Check(user.ID(), body.IdempotencyKey); ok {
			// Idempotency hit: return the existing message ID.
			// We return the minimal response (ID + current content) rather than
			// re-fetching the stored message, because the client already received
			// the full 201 response on the original send. This response only
			// signals "your message was already accepted."
			writeJSON(w, http.StatusOK, chatMessageResponse{
				ID:      existingID,
				Content: content,
				Sender:  "user:" + user.DisplayName(),
			})
			return
		}
	}

	// --- Resolve routing per design §3 ---
	senderEmail := user.Email()
	senderName := user.DisplayName()
	senderLabel := senderEmail
	if senderName != "" {
		senderLabel = senderName
	}

	// Resolve which project we're working in for agent resolution.
	if projectID == "" && isDM {
		projectID = resolveProjectFromDMKey(ctx, s, key)
	}

	// Step 1: Extract mentions.
	mentionNames := messages.ExtractMentions(content)

	// Step 2: Resolve agent mentions against project agents.
	var mentionedAgents []*store.Agent
	var mentionResults []messages.MentionResult
	if len(mentionNames) > 0 && projectID != "" {
		agentList, err := s.store.ListAgents(ctx, store.AgentFilter{ProjectID: projectID}, store.ListOptions{Limit: 200})
		if err == nil {
			agentInfos := make([]messages.AgentInfo, 0, len(agentList.Items))
			agentBySlug := make(map[string]*store.Agent, len(agentList.Items))
			for i := range agentList.Items {
				a := &agentList.Items[i]
				agentInfos = append(agentInfos, messages.AgentInfo{Slug: a.Slug, Name: a.Name})
				agentBySlug[strings.ToLower(a.Slug)] = a
			}
			mentionResults = messages.ResolveMentions(mentionNames, agentInfos, "")
			for _, mr := range mentionResults {
				if mr.Status == "delivered" {
					if a, ok := agentBySlug[strings.ToLower(mr.Slug)]; ok {
						mentionedAgents = append(mentionedAgents, a)
					}
				}
			}
		}
	}

	now := time.Now().UTC()

	// Closure to record idempotency after message creation.
	recordIdempotency := func(messageID string) {
		if body.IdempotencyKey != "" {
			s.chatIdempotency.Record(user.ID(), body.IdempotencyKey, messageID)
		}
	}

	// Step 3: Determine routing.
	if len(mentionedAgents) > 0 {
		// --- Agent-routed: explicit mentions ---
		msgID := s.sendAgentRouted(w, r, key, projectID, user, content, senderLabel, mentionedAgents, mentionNames, mentionResults, attachmentRefs, now, body.ReplyToID)
		if msgID == "" {
			return // error response already written by sendAgentRouted
		}
		recordIdempotency(msgID)
		// Ensure DM registry rows exist so the DM appears in the rail.
		if isDM {
			s.ensureDMRegistered(ctx, key, user.ID())
		}
		return
	}

	if isDM {
		// Agent DM implicit routing: when the key is dm:agent:<uuid>:user:<uuid>,
		// the agent is the implicit recipient (equivalent to default_agent for
		// topics). An explicit @mention of a different agent takes precedence
		// (handled above). Design §3, §4.3.
		if agentID := parseAgentDMKey(key); agentID != "" {
			dmAgent, err := s.store.GetAgent(ctx, agentID)
			if err == nil && dmAgent != nil {
				msgID := s.sendAgentRouted(w, r, key, projectID, user, content, senderLabel, []*store.Agent{dmAgent}, mentionNames, nil, attachmentRefs, now, body.ReplyToID)
				if msgID == "" {
					return // error response already written by sendAgentRouted
				}
				recordIdempotency(msgID)
				// Ensure DM registry rows exist so the DM appears in the rail.
				s.ensureDMRegistered(ctx, key, user.ID())
				return
			}
		}
	} else if projectID != "" {
		// Check if topic has a default_agent.
		topic, err := wcs.GetTopic(ctx, key)
		if err == nil && topic != nil && topic.DefaultAgent != "" {
			// Resolve the default agent. The default_agent field stores either
			// a slug (from /default command) or a UUID, so try both lookups.
			defaultAgent, err := s.store.GetAgentBySlug(ctx, projectID, topic.DefaultAgent)
			if err != nil || defaultAgent == nil {
				// Fall back to lookup by ID in case the value is a UUID.
				defaultAgent, err = s.store.GetAgent(ctx, topic.DefaultAgent)
			}
			if err == nil && defaultAgent != nil {
				msgID := s.sendAgentRouted(w, r, key, projectID, user, content, senderLabel, []*store.Agent{defaultAgent}, mentionNames, nil, attachmentRefs, now, body.ReplyToID)
				if msgID == "" {
					return // error response already written by sendAgentRouted
				}
				recordIdempotency(msgID)
				return
			}
		}
	}

	// --- Human-to-human message ---
	msgID := s.sendHumanToHuman(w, r, key, projectID, user, content, senderLabel, isDM, mentionNames, attachmentRefs, now, body.ReplyToID)
	if msgID == "" {
		return // error response already written by sendHumanToHuman
	}
	recordIdempotency(msgID)
}

// sendAgentRouted sends a message through the existing agent dispatch path.
// Returns the persisted message ID (empty on error).
func (s *Server) sendAgentRouted(w http.ResponseWriter, r *http.Request, key, projectID string, user UserIdentity,
	content, senderLabel string, agents []*store.Agent, mentionNames []string, mentionResults []messages.MentionResult,
	attachmentRefs []AttachmentRef, now time.Time, replyToID string) string {

	ctx := r.Context()

	if len(agents) == 0 {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "no agent to route to", nil)
		return ""
	}

	primaryAgent := agents[0]

	// Determine message type: explicit @mentions use type:mention so every
	// mentioned agent receives the same type. Default-agent and DM-implicit
	// routing (mentionResults == nil) keeps type:instruction.
	msgType := messages.TypeInstruction
	if mentionResults != nil {
		msgType = messages.TypeMention
	}

	// Build the structured message for the primary agent.
	msg := &messages.StructuredMessage{
		Version:     messages.Version,
		Timestamp:   now.Format(time.RFC3339),
		Sender:      "user:" + senderLabel,
		SenderID:    user.ID(),
		Recipient:   "agent:" + primaryAgent.Slug,
		RecipientID: primaryAgent.ID,
		Msg:         content,
		Type:        msgType,
		Channel:     "web",
		ThreadID:    key,
	}

	// For @-mention routing, add mention metadata so the agent sees the same
	// envelope shape as fan-out recipients.
	if mentionResults != nil {
		msg.Metadata = map[string]string{
			"mention_source":   "user:" + senderLabel,
			"mention_position": "body",
		}
	}

	// W7: Add attachment metadata and file paths for agent dispatch.
	if len(attachmentRefs) > 0 {
		// Embed attachment metadata in StructuredMessage.Metadata for SSE consumers.
		if msg.Metadata == nil {
			msg.Metadata = make(map[string]string)
		}
		refsJSON, _ := json.Marshal(attachmentRefs)
		msg.Metadata[attachmentsMetadataKey] = string(refsJSON)

		// For agent dispatch: pass container-visible file paths in Attachments
		// (same pattern as Discord plugin — agents receive []string of paths).
		// The attachment store lives on the hub host, which agent containers
		// cannot read, so each file is staged into the project's scratchpad
		// shared dir first. Staging is best-effort: when it is unavailable the
		// hub-local path is sent, which host-process agents can still read.
		s.mu.RLock()
		as := s.attachmentStore
		wcs := s.webChatStore
		s.mu.RUnlock()
		if localAS, ok := as.(*LocalDiskAttachmentStore); ok {
			staging := s.resolveAttachmentStaging(ctx, projectID)
			for _, ref := range attachmentRefs {
				// A file lives under the project it was uploaded to, which is not
				// this message's project when it was uploaded from a DM — those
				// uploads carry no project at all.
				storedIn := projectID
				if wcs != nil {
					if meta, err := wcs.GetAttachment(ctx, ref.ID); err == nil && meta != nil {
						storedIn = meta.ProjectID
					}
				}
				hostPath := localAS.FilePath(storedIn, ref.ID, ref.Name)
				agentPath := hostPath
				if staging != nil {
					staged, err := staging.stage(hostPath, ref.ID, ref.Name)
					if err != nil {
						s.messageLog.Error("Failed to stage attachment for agent",
							"attachment", ref.ID, "error", err)
					} else {
						agentPath = staged
					}
				}
				msg.Attachments = append(msg.Attachments, agentPath)
			}
		}
	}

	// Persist the message.
	storeMsg := &store.Message{
		ID:            api.NewUUID(),
		ProjectID:     projectID,
		Sender:        msg.Sender,
		SenderID:      msg.SenderID,
		Recipient:     msg.Recipient,
		RecipientID:   msg.RecipientID,
		Msg:           content,
		Type:          msgType,
		AgentID:       primaryAgent.ID,
		Channel:       "web",
		ThreadID:      key,
		DispatchState: store.MessageDispatchDispatched,
		CreatedAt:     now,
	}
	if err := s.store.CreateMessage(ctx, storeMsg); err != nil {
		s.messageLog.Error("Failed to persist agent-routed message", "error", err)
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to persist message", nil)
		return ""
	}

	// Phase-3: Store reply-to reference if provided.
	if replyToID != "" {
		s.mu.RLock()
		replyWcs := s.webChatStore
		s.mu.RUnlock()
		if replyWcs != nil {
			if err := replyWcs.SetMessageReplyTo(ctx, storeMsg.ID, replyToID); err != nil {
				slog.Error("Failed to store reply_to_id", "messageId", storeMsg.ID, "replyToId", replyToID, "error", err)
			}
		}
	}

	// Phase-3: Store reply-to reference if provided.
	if replyToID != "" {
		s.mu.RLock()
		replyWcs := s.webChatStore
		s.mu.RUnlock()
		if replyWcs != nil {
			if err := replyWcs.SetMessageReplyTo(ctx, storeMsg.ID, replyToID); err != nil {
				slog.Error("Failed to store reply_to_id", "messageId", storeMsg.ID, "replyToId", replyToID, "error", err)
			}
		}
	}

	// W7: Link attachments to the persisted message.
	s.mu.RLock()
	linkWcs := s.webChatStore
	s.mu.RUnlock()
	if linkWcs != nil && len(attachmentRefs) > 0 {
		for _, ref := range attachmentRefs {
			if err := linkWcs.LinkAttachmentToMessage(ctx, storeMsg.ID, ref.ID); err != nil {
				s.messageLog.Error("Failed to link attachment to message", "attachment", ref.ID, "error", err)
			}
		}
	}

	s.events.PublishUserMessage(ctx, storeMsg)

	// Dispatch to the primary agent.
	dispatcher := s.GetDispatcher()
	if dispatcher != nil {
		retryCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := dispatchWithBrokerRetry(retryCtx, dispatcher, primaryAgent, content, false, msg); err != nil {
			s.messageLog.Error("Failed to dispatch to agent", "agent", primaryAgent.Slug, "error", err)
			_ = s.store.MarkMessageFailed(ctx, storeMsg.ID, err.Error())
		}
	}

	// Handle additional mentioned agents (fan-out).
	if len(agents) > 1 {
		for _, mentionAgent := range agents[1:] {
			mentionMsg := messages.NewMention(msg.Sender, "agent:"+mentionAgent.Slug, content, msg.Recipient)
			mentionMsg.SenderID = msg.SenderID
			mentionMsg.RecipientID = mentionAgent.ID
			mentionMsg.Channel = "web"
			mentionMsg.ThreadID = key
			// W7: Copy attachment paths and metadata to mention messages.
			mentionMsg.Attachments = msg.Attachments
			mentionMsg.Metadata = msg.Metadata

			mentionStoreMsg := &store.Message{
				ID:            api.NewUUID(),
				ProjectID:     projectID,
				Sender:        mentionMsg.Sender,
				SenderID:      mentionMsg.SenderID,
				Recipient:     mentionMsg.Recipient,
				RecipientID:   mentionMsg.RecipientID,
				Msg:           content,
				Type:          messages.TypeMention,
				AgentID:       mentionAgent.ID,
				Channel:       "web",
				ThreadID:      key,
				DispatchState: store.MessageDispatchDispatched,
				CreatedAt:     now,
			}
			if err := s.store.CreateMessage(ctx, mentionStoreMsg); err != nil {
				s.messageLog.Error("Failed to persist mention message", "slug", mentionAgent.Slug, "error", err)
			} else {
				s.events.PublishUserMessage(ctx, mentionStoreMsg)
			}

			if dispatcher != nil {
				retryCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
				if err := dispatchWithBrokerRetry(retryCtx, dispatcher, mentionAgent, content, false, mentionMsg); err != nil {
					s.messageLog.Error("Failed to dispatch mention", "slug", mentionAgent.Slug, "error", err)
				}
				cancel()
			}
		}
	}

	// Update topic/DM watermark.
	s.touchConversationActivity(ctx, key, storeMsg.ID)

	// --- W6: Human mention notifications ---
	// Resolve @mentions that didn't match agents — they may be human members.
	// Fire in a goroutine to avoid blocking the response.
	if cn := s.getChatNotifier(); cn != nil && len(mentionNames) > 0 && projectID != "" {
		go s.fireHumanMentionNotifications(context.Background(), mentionNames, projectID, key, user.ID(), senderLabel, content)
	}

	writeJSON(w, http.StatusCreated, chatMessageResponse{
		ID:          storeMsg.ID,
		Content:     content,
		Sender:      storeMsg.Sender,
		SenderID:    storeMsg.SenderID,
		Type:        storeMsg.Type,
		CreatedAt:   now,
		Mentions:    mentionResults,
		Attachments: attachmentRefs,
	})
	return storeMsg.ID
}

// sendHumanToHuman persists a type:chat message for human-to-human communication.
// Returns the persisted message ID (empty on error).
func (s *Server) sendHumanToHuman(w http.ResponseWriter, r *http.Request, key, projectID string, user UserIdentity,
	content, senderLabel string, isDM bool, mentionNames []string, attachmentRefs []AttachmentRef, now time.Time, replyToID string) string {

	ctx := r.Context()

	s.mu.RLock()
	wcs := s.webChatStore
	s.mu.RUnlock()

	var recipient, recipientID string
	if isDM {
		peerEmail, peerID := resolveDMPeer(key, user.ID())
		if peerEmail != "" {
			recipient = "user:" + peerEmail
		} else {
			recipient = "user:" + peerID
		}
		recipientID = peerID
		// Look up the peer to get their email for a better recipient label.
		if peerID != "" && peerEmail == "" {
			if peerUser, err := s.store.GetUser(ctx, peerID); err == nil {
				recipient = "user:" + peerUser.Email
			}
		}
	} else {
		recipient = "thread:" + key
		recipientID = key
	}

	// The Ent messages schema requires a non-empty project_id (UUID).
	// User-to-user DMs are global (not project-scoped), so use the nil UUID
	// as a sentinel when no project context is available.
	msgProjectID := projectID
	if msgProjectID == "" {
		msgProjectID = uuid.Nil.String()
	}

	storeMsg := &store.Message{
		ID:            api.NewUUID(),
		ProjectID:     msgProjectID,
		Sender:        "user:" + senderLabel,
		SenderID:      user.ID(),
		Recipient:     recipient,
		RecipientID:   recipientID,
		Msg:           content,
		Type:          messages.TypeChat,
		Channel:       "web",
		ThreadID:      key,
		DispatchState: store.MessageDispatchDispatched,
		CreatedAt:     now,
	}

	if err := s.store.CreateMessage(ctx, storeMsg); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to persist message", nil)
		return ""
	}

	// Phase-3: Store reply-to reference if provided.
	if replyToID != "" && wcs != nil {
		if err := wcs.SetMessageReplyTo(ctx, storeMsg.ID, replyToID); err != nil {
			slog.Error("Failed to store reply_to_id", "messageId", storeMsg.ID, "replyToId", replyToID, "error", err)
		}
	}

	// Phase-3: Store reply-to reference if provided.
	if replyToID != "" && wcs != nil {
		if err := wcs.SetMessageReplyTo(ctx, storeMsg.ID, replyToID); err != nil {
			slog.Error("Failed to store reply_to_id", "messageId", storeMsg.ID, "replyToId", replyToID, "error", err)
		}
	}

	// W7: Link attachments to the persisted message.
	if wcs != nil && len(attachmentRefs) > 0 {
		for _, ref := range attachmentRefs {
			if err := wcs.LinkAttachmentToMessage(ctx, storeMsg.ID, ref.ID); err != nil {
				slog.Error("Failed to link attachment to message", "attachment", ref.ID, "error", err)
			}
		}
	}

	// Publish SSE event.
	s.events.PublishUserMessage(ctx, storeMsg)

	// For DMs, ensure DM registry rows exist for both participants. This must
	// precede the watermark update: touchConversationActivity is a plain
	// UPDATE and would affect zero rows on the first message of a DM.
	if isDM && wcs != nil {
		s.ensureDMRegistered(ctx, key, user.ID())
	}

	// Update conversation watermark.
	if wcs != nil {
		s.touchConversationActivity(ctx, key, storeMsg.ID)
	}

	// --- W6: Chat notifications ---
	if cn := s.getChatNotifier(); cn != nil {
		// DM received notification: notify the peer when a DM is sent.
		if isDM && recipientID != "" && recipientID != user.ID() {
			go cn.NotifyDMReceived(context.Background(), recipientID, ChatMessageContext{
				SenderID:        user.ID(),
				SenderName:      senderLabel,
				ConversationKey: key,
				Preview:         content,
				ProjectID:       projectID,
			})
		}
		// Human mention notifications.
		if len(mentionNames) > 0 && projectID != "" {
			go s.fireHumanMentionNotifications(context.Background(), mentionNames, projectID, key, user.ID(), senderLabel, content)
		}
	}

	writeJSON(w, http.StatusCreated, chatMessageResponse{
		ID:          storeMsg.ID,
		Content:     content,
		Sender:      storeMsg.Sender,
		SenderID:    storeMsg.SenderID,
		Type:        storeMsg.Type,
		CreatedAt:   now,
		Attachments: attachmentRefs,
	})
	return storeMsg.ID
}

// ---------------------------------------------------------------------------
// Message Edit / Delete (Phase 3)
// ---------------------------------------------------------------------------

// handleMessageEdit implements PUT /api/v1/chat/conversations/{key}/messages/{id}.
// Only the message sender can edit, and only if no agent has replied after it.
func (s *Server) handleMessageEdit(w http.ResponseWriter, r *http.Request, key, messageID string) {
	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		Forbidden(w)
		return
	}

	ctx := r.Context()

	s.mu.RLock()
	wcs := s.webChatStore
	s.mu.RUnlock()

	if wcs == nil {
		writeError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Chat not available", nil)
		return
	}

	// Fetch the message.
	msg, err := s.store.GetMessage(ctx, messageID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to fetch message", nil)
		return
	}
	if msg == nil {
		NotFound(w, "Message")
		return
	}

	// Verify the caller is the message sender.
	if msg.SenderID != user.ID() {
		Forbidden(w)
		return
	}

	// Verify conversation key matches.
	if msg.ThreadID != key {
		BadRequest(w, "message does not belong to this conversation")
		return
	}

	// Check no agent has replied after this message.
	if hasAgentReplyAfter(ctx, s.store, key, msg.CreatedAt) {
		writeError(w, http.StatusConflict, "AGENT_REPLIED", "Cannot edit: an agent has replied after this message", nil)
		return
	}

	// Parse the new content.
	var body struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		BadRequest(w, "invalid request body")
		return
	}
	content := strings.TrimSpace(body.Content)
	if content == "" {
		ValidationError(w, "content is required", nil)
		return
	}

	// Resolve projectID for event publishing.
	projectID := msg.ProjectID
	if projectID == uuid.Nil.String() {
		projectID = ""
	}

	// Update message content and mark as edited.
	//
	// Known limitation: content update and edited_at are separate DB writes.
	// If UpdateMessageContent succeeds but SetMessageEdited fails, the content
	// changes without an edited_at record. A full transactional fix would
	// require combining them into a single call, which is out of scope here.
	if err := wcs.UpdateMessageContent(ctx, messageID, content); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to update message", nil)
		return
	}

	now := time.Now().UTC()
	if err := wcs.SetMessageEdited(ctx, messageID, now); err != nil {
		slog.Error("Failed to set message edited_at", "messageId", messageID, "error", err)
	}

	// Publish SSE event.
	s.events.PublishChatMessageEdited(ctx, projectID, key, ChatMessageEditedEvent{
		ConversationKey: key,
		MessageID:       messageID,
		Content:         content,
		EditedAt:        now.Format(time.RFC3339Nano),
	})

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"messageId": messageID,
		"content":   content,
		"editedAt":  now,
	})
}

// handleMessageDelete implements DELETE /api/v1/chat/conversations/{key}/messages/{id}.
// Only the message sender can delete, and only if no agent has replied after it.
func (s *Server) handleMessageDelete(w http.ResponseWriter, r *http.Request, key, messageID string) {
	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		Forbidden(w)
		return
	}

	ctx := r.Context()

	s.mu.RLock()
	wcs := s.webChatStore
	s.mu.RUnlock()

	if wcs == nil {
		writeError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Chat not available", nil)
		return
	}

	// Fetch the message.
	msg, err := s.store.GetMessage(ctx, messageID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to fetch message", nil)
		return
	}
	if msg == nil {
		NotFound(w, "Message")
		return
	}

	// Verify the caller is the message sender.
	if msg.SenderID != user.ID() {
		Forbidden(w)
		return
	}

	// Verify conversation key matches.
	if msg.ThreadID != key {
		BadRequest(w, "message does not belong to this conversation")
		return
	}

	// Check no agent has replied after this message.
	if hasAgentReplyAfter(ctx, s.store, key, msg.CreatedAt) {
		writeError(w, http.StatusConflict, "AGENT_REPLIED", "Cannot delete: an agent has replied after this message", nil)
		return
	}

	// Resolve projectID for event publishing.
	projectID := msg.ProjectID
	if projectID == uuid.Nil.String() {
		projectID = ""
	}

	// Soft-delete: set deleted_at in extension table and redact content
	// in the main messages table so no other component can read it.
	now := time.Now().UTC()
	if err := wcs.SetMessageDeleted(ctx, messageID, now); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to delete message", nil)
		return
	}
	if err := wcs.UpdateMessageContent(ctx, messageID, ""); err != nil {
		slog.Error("Failed to clear message content on delete", "messageId", messageID, "error", err)
	}

	// Publish SSE event.
	s.events.PublishChatMessageDeleted(ctx, projectID, key, ChatMessageDeletedEvent{
		ConversationKey: key,
		MessageID:       messageID,
		DeletedAt:       now.Format(time.RFC3339Nano),
	})

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"messageId": messageID,
		"deletedAt": now,
	})
}

// hasAgentReplyAfter checks if any agent has sent a message in the same
// conversation after the given time. Used to guard edit/delete operations.
// Fail-closed: returns true (denying edit/delete) when the query errors,
// so a transient DB failure cannot be exploited to bypass the guard.
//
// The scan pages through results in small batches so memory stays bounded
// regardless of how many messages follow the target timestamp. Each page
// loads at most pageSize messages; as soon as one agent-sent message is
// found the function returns early.
func hasAgentReplyAfter(ctx context.Context, s store.Store, threadID string, after time.Time) bool {
	filter := store.MessageFilter{
		Channel:  "web",
		ThreadID: threadID,
		After:    after,
	}
	const pageSize = 50
	cursor := ""
	for {
		opts := store.ListOptions{Limit: pageSize}
		if cursor != "" {
			opts.Cursor = cursor
		}
		result, err := s.ListMessages(ctx, filter, opts)
		if err != nil || result == nil {
			return true // fail-closed: deny edit/delete when we can't verify
		}
		for _, msg := range result.Items {
			if strings.HasPrefix(msg.Sender, "agent:") {
				return true
			}
		}
		// No more pages — no agent reply found.
		if result.NextCursor == "" || len(result.Items) < pageSize {
			return false
		}
		cursor = result.NextCursor
	}
}

// ---------------------------------------------------------------------------
// Message History
// ---------------------------------------------------------------------------

// handleConversationHistory handles GET /api/v1/chat/conversations/{key}/messages.
//
// Query params:
//   - limit: page size (default 50, max 200)
//   - cursor: keyset pagination cursor from the previous page's nextCursor (optional)
//   - visibility: repeatable visibility filter (optional)
func (s *Server) handleConversationHistory(w http.ResponseWriter, r *http.Request, key string) {
	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		Forbidden(w)
		return
	}

	ctx := r.Context()

	s.mu.RLock()
	wcs := s.webChatStore
	s.mu.RUnlock()

	// Authorize.
	isDM := strings.HasPrefix(key, "dm:")
	if isDM {
		if !validDMKey(key) {
			BadRequest(w, "invalid DM key format")
			return
		}
		if !isDMParticipant(key, user.ID()) {
			Forbidden(w)
			return
		}
	} else {
		if wcs == nil {
			writeJSON(w, http.StatusOK, chatHistoryResponse{Messages: []store.Message{}})
			return
		}
		topic, err := wcs.GetTopic(ctx, key)
		if err != nil || topic == nil {
			NotFound(w, "Thread")
			return
		}
		project, err := s.store.GetProject(ctx, topic.ProjectID)
		if err != nil {
			NotFound(w, "Project")
			return
		}
		if !s.authorize(w, r, projectResource(project), ActionRead) {
			return
		}
	}

	// Parse query params.
	q := r.URL.Query()
	limit := 50
	if l := q.Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}

	filter := store.MessageFilter{
		Channel:  "web",
		ThreadID: key,
	}
	// Support visibility filter.
	if vis := q["visibility"]; len(vis) > 0 {
		filter.Visibility = vis
	}

	opts := store.ListOptions{
		Limit: limit,
		// Keyset pagination cursor. The client sends the opaque `nextCursor`
		// from the previous page back as `cursor` (see chat-thread.ts
		// fetchHistoryV2); reading any other parameter name silently drops it
		// and every page returns the newest messages again (#1027).
		Cursor: q.Get("cursor"),
	}

	result, err := s.store.ListMessages(ctx, filter, opts)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to fetch messages", nil)
		return
	}

	if result.Items == nil {
		result.Items = []store.Message{}
	}

	// W7: Enrich messages with attachment metadata using a single batch
	// query (R3 — avoids N+1 per-message queries on history pages).
	var messageAttachments map[string][]AttachmentRef
	if wcs != nil && len(result.Items) > 0 {
		msgIDs := make([]string, len(result.Items))
		for i, msg := range result.Items {
			msgIDs[i] = msg.ID
		}
		batchAttachments, err := wcs.GetAttachmentsByMessages(ctx, msgIDs)
		if err == nil && len(batchAttachments) > 0 {
			messageAttachments = make(map[string][]AttachmentRef, len(batchAttachments))
			for msgID, attachments := range batchAttachments {
				refs := make([]AttachmentRef, 0, len(attachments))
				for _, a := range attachments {
					refs = append(refs, AttachmentRef{
						ID:       a.ID,
						Name:     a.Filename,
						MimeType: a.MimeType,
						Size:     a.Size,
					})
				}
				messageAttachments[msgID] = refs
			}
		}
	}

	// Phase-3: Enrich messages with extension data (reply-to, edited, deleted)
	// and generate reply previews using a single batch query.
	var messageExtensions map[string]*WebChatMessageExt
	var replyPreviews map[string]chatReplyPreview
	if wcs != nil && len(result.Items) > 0 {
		msgIDs2 := make([]string, len(result.Items))
		for i, msg := range result.Items {
			msgIDs2[i] = msg.ID
		}
		exts, err := wcs.GetMessageExts(ctx, msgIDs2)
		if err == nil && len(exts) > 0 {
			messageExtensions = exts

			// Strip content from soft-deleted messages so the original
			// text is not leaked to clients over the wire.
			for i := range result.Items {
				if ext, ok := messageExtensions[result.Items[i].ID]; ok && ext.DeletedAt != nil {
					result.Items[i].Msg = ""
				}
			}

			// Collect referenced message IDs for reply previews.
			replyToIDs := make([]string, 0, len(exts))
			for _, ext := range exts {
				if ext.ReplyToID != "" {
					replyToIDs = append(replyToIDs, ext.ReplyToID)
				}
			}
			if len(replyToIDs) > 0 {
				// Also fetch extensions for the referenced messages so
				// we can detect deleted reply parents.
				replyExts, _ := wcs.GetMessageExts(ctx, replyToIDs)

				refMsgs, err := s.store.GetMessagesByIDs(ctx, replyToIDs)
				if err == nil && len(refMsgs) > 0 {
					replyPreviews = make(map[string]chatReplyPreview, len(refMsgs))
					for id, refMsg := range refMsgs {
						content := refMsg.Msg
						// If the referenced message is deleted, show
						// "[deleted]" instead of leaking the original text.
						if replyExts != nil {
							if rExt, ok := replyExts[id]; ok && rExt.DeletedAt != nil {
								content = "[deleted]"
							}
						}
						if content != "[deleted]" && len([]rune(content)) > 100 {
							content = string([]rune(content)[:100]) + "..."
						}
						replyPreviews[id] = chatReplyPreview{
							MessageID:  id,
							SenderName: refMsg.Sender,
							Content:    content,
						}
					}
				}
			}
		}
	}

	writeJSON(w, http.StatusOK, chatHistoryResponse{
		Messages:           result.Items,
		NextCursor:         result.NextCursor,
		TotalCount:         result.TotalCount,
		MessageAttachments: messageAttachments,
		MessageExtensions:  messageExtensions,
		ReplyPreviews:      replyPreviews,
	})
}

// ---------------------------------------------------------------------------
// Inter-Agent Messages
// ---------------------------------------------------------------------------

// handleConversationInteragent handles GET /api/v1/chat/conversations/{key}/interagent.
// Returns inter-agent messages exchanged by the DM agent with other agents,
// optionally scoped to a time range. Only valid for agent DMs.
func (s *Server) handleConversationInteragent(w http.ResponseWriter, r *http.Request, key string) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		Forbidden(w)
		return
	}

	// Only agent DMs are supported.
	if !strings.HasPrefix(key, "dm:agent:") {
		writeJSON(w, http.StatusOK, interagentResponse{Messages: []store.Message{}})
		return
	}
	if !validDMKey(key) {
		BadRequest(w, "invalid DM key format")
		return
	}
	if !isDMParticipant(key, user.ID()) {
		Forbidden(w)
		return
	}

	// Extract the agent UUID from the DM key.
	agentID := parseAgentDMKey(key)
	if agentID == "" {
		writeJSON(w, http.StatusOK, interagentResponse{Messages: []store.Message{}})
		return
	}

	ctx := r.Context()

	// Look up the agent to get its slug — needed for matching legacy
	// messages where SenderID was not populated (only the Sender text
	// field like "agent:<slug>" was set).
	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil || agent == nil {
		writeJSON(w, http.StatusOK, interagentResponse{Messages: []store.Message{}})
		return
	}

	q := r.URL.Query()

	// Parse optional time-range bounds.
	var after, before time.Time
	if v := q.Get("after"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			after = t
		}
	}
	if v := q.Get("before"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			before = t
		}
	}

	limit := 200
	if l := q.Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}

	opts := store.ListOptions{
		Limit: limit,
	}

	// Query 1: messages where the DM agent's UUID appears in sender_id
	// or recipient_id.
	filter := store.MessageFilter{
		ParticipantID: agentID,
		Before:        before,
		After:         after,
	}

	result, err := s.store.ListMessages(ctx, filter, opts)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to fetch inter-agent messages", nil)
		return
	}

	// Query 2 (backward-compat): messages where the agent was the sender
	// but sender_id was empty (legacy CLI-originated messages stored the
	// slug in the Sender text field only). This catches the "sent-by"
	// direction that ParticipantID misses for older data.
	agentSender := "agent:" + agent.Slug
	senderFilter := store.MessageFilter{
		Sender: agentSender,
		Before: before,
		After:  after,
	}
	senderResult, err := s.store.ListMessages(ctx, senderFilter, opts)
	if err != nil {
		// Non-fatal: proceed with the first query's results.
		slog.Error("Failed to fetch sender-based inter-agent messages", "error", err)
		senderResult = &store.ListResult[store.Message]{}
	}

	// Merge and deduplicate by message ID, keeping only agent-to-agent
	// messages (both sender and recipient are agents).
	seen := make(map[string]bool, len(result.Items))
	filtered := make([]store.Message, 0, len(result.Items)+len(senderResult.Items))
	for _, m := range result.Items {
		if strings.HasPrefix(m.Sender, "agent:") && strings.HasPrefix(m.Recipient, "agent:") {
			if !seen[m.ID] {
				seen[m.ID] = true
				filtered = append(filtered, m)
			}
		}
	}
	for _, m := range senderResult.Items {
		if strings.HasPrefix(m.Sender, "agent:") && strings.HasPrefix(m.Recipient, "agent:") {
			if !seen[m.ID] {
				seen[m.ID] = true
				filtered = append(filtered, m)
			}
		}
	}

	writeJSON(w, http.StatusOK, interagentResponse{Messages: filtered})
}

// interagentResponse is the response for the interagent endpoint.
type interagentResponse struct {
	Messages []store.Message `json:"messages"`
}

// ---------------------------------------------------------------------------
// Read Watermarks
// ---------------------------------------------------------------------------

// chatReadStateResponse is the GET /read payload. For a human-to-human DM it
// carries the peer's watermark so the sender can render "seen" on load,
// without waiting for the next read-state SSE event.
type chatReadStateResponse struct {
	ConversationKey       string `json:"conversationKey"`
	LastReadMessageID     string `json:"lastReadMessageId,omitempty"`
	PeerLastReadMessageID string `json:"peerLastReadMessageId,omitempty"`
	PeerLastReadAt        string `json:"peerLastReadAt,omitempty"`
}

// handleConversationRead handles GET and POST
// /api/v1/chat/conversations/{key}/read. POST advances the caller's read
// watermark; GET reports the caller's watermark and, for DMs, the peer's.
func (s *Server) handleConversationRead(w http.ResponseWriter, r *http.Request, key string) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		Forbidden(w)
		return
	}

	ctx := r.Context()

	s.mu.RLock()
	wcs := s.webChatStore
	s.mu.RUnlock()

	if wcs == nil {
		if r.Method == http.MethodGet {
			writeJSON(w, http.StatusOK, chatReadStateResponse{ConversationKey: key})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}

	isDM := strings.HasPrefix(key, "dm:")
	if !s.authorizeConversationAccess(w, r, wcs, key, user.ID()) {
		return
	}

	if r.Method == http.MethodGet {
		s.writeConversationReadState(w, r, wcs, key, user.ID(), isDM)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1048576)
	var body struct {
		MessageID string `json:"messageId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		BadRequest(w, "invalid request body")
		return
	}

	if body.MessageID == "" {
		ValidationError(w, "messageId is required", nil)
		return
	}

	if err := wcs.SetReadState(ctx, user.ID(), key, body.MessageID); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to update read state", nil)
		return
	}

	// Tell the DM peer their message has been seen. Best-effort: a dropped
	// event only costs the sender a "seen" tick until their next reload.
	s.events.PublishChatReadStateEvent(ctx, key, user.ID(), body.MessageID)

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// writeConversationReadState answers GET .../read. The peer fields are only
// populated for human-to-human DMs — agent DMs have no peer watermark, and a
// topic's read state is per-user with no single "peer" to report.
func (s *Server) writeConversationReadState(
	w http.ResponseWriter, r *http.Request, wcs WebChatStore, key, userID string, isDM bool,
) {
	ctx := r.Context()
	resp := chatReadStateResponse{ConversationKey: key}

	if rs, err := wcs.GetReadState(ctx, userID, key); err == nil && rs != nil {
		resp.LastReadMessageID = rs.LastReadMessageID
	}

	if isDM {
		for _, participantID := range dmUserParticipants(key) {
			if participantID == userID {
				continue
			}
			peerRS, err := wcs.GetReadState(ctx, participantID, key)
			if err != nil || peerRS == nil {
				continue
			}
			resp.PeerLastReadMessageID = peerRS.LastReadMessageID
			if !peerRS.LastReadAt.IsZero() {
				resp.PeerLastReadAt = peerRS.LastReadAt.UTC().Format(time.RFC3339Nano)
			}
			break
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

// authorizeConversationAccess authorizes the caller for a conversation key: a
// DM is reachable by its participants, a topic by anyone with read access to
// its project. It writes the error response itself and returns false when
// access is refused.
//
// The read, mute and pin handlers all call this rather than each carrying a
// copy. They have to agree — you should not be able to mute a conversation you
// cannot read — and three copies of the same twenty lines agree only until
// someone edits one of them.
func (s *Server) authorizeConversationAccess(
	w http.ResponseWriter, r *http.Request, wcs WebChatStore, key, userID string,
) bool {
	if strings.HasPrefix(key, "dm:") {
		if !validDMKey(key) {
			BadRequest(w, "invalid DM key format")
			return false
		}
		if !isDMParticipant(key, userID) {
			Forbidden(w)
			return false
		}
		return true
	}

	ctx := r.Context()
	topic, err := wcs.GetTopic(ctx, key)
	if err != nil || topic == nil {
		NotFound(w, "Thread")
		return false
	}
	project, err := s.store.GetProject(ctx, topic.ProjectID)
	if err != nil {
		NotFound(w, "Project")
		return false
	}
	return s.authorize(w, r, projectResource(project), ActionRead)
}

// handleConversationMute handles PUT /api/v1/chat/conversations/{key}/mute.
// Body: {"muted": bool}. A muted conversation raises no notifications
// (ChatNotifier already honours the flag) and shows no unread badge.
func (s *Server) handleConversationMute(w http.ResponseWriter, r *http.Request, key string) {
	if r.Method != http.MethodPut {
		MethodNotAllowed(w)
		return
	}

	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		Forbidden(w)
		return
	}

	s.mu.RLock()
	wcs := s.webChatStore
	s.mu.RUnlock()

	if wcs == nil {
		writeError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Chat not available", nil)
		return
	}

	if !s.authorizeConversationAccess(w, r, wcs, key, user.ID()) {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1048576)
	var body struct {
		Muted *bool `json:"muted"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		BadRequest(w, "invalid request body")
		return
	}
	if body.Muted == nil {
		ValidationError(w, "muted is required", nil)
		return
	}

	if err := wcs.SetMuted(r.Context(), user.ID(), key, *body.Muted); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to update mute state", nil)
		return
	}

	writeJSON(w, http.StatusOK, map[string]bool{"muted": *body.Muted})
}

// handleConversationPin handles PUT /api/v1/chat/conversations/{key}/pin.
// Body: {"pinned": bool}. Pinned threads sort above the rest of their space.
func (s *Server) handleConversationPin(w http.ResponseWriter, r *http.Request, key string) {
	if r.Method != http.MethodPut {
		MethodNotAllowed(w)
		return
	}

	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		Forbidden(w)
		return
	}

	s.mu.RLock()
	wcs := s.webChatStore
	s.mu.RUnlock()

	if wcs == nil {
		writeError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Chat not available", nil)
		return
	}

	if !s.authorizeConversationAccess(w, r, wcs, key, user.ID()) {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1048576)
	var body struct {
		Pinned *bool `json:"pinned"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		BadRequest(w, "invalid request body")
		return
	}
	if body.Pinned == nil {
		ValidationError(w, "pinned is required", nil)
		return
	}

	if err := wcs.SetPinned(r.Context(), user.ID(), key, *body.Pinned); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to update pin state", nil)
		return
	}

	writeJSON(w, http.StatusOK, map[string]bool{"pinned": *body.Pinned})
}

// handleSpaceRead handles POST /api/v1/chat/spaces/{projectId}/read.
func (s *Server) handleSpaceRead(w http.ResponseWriter, r *http.Request, projectID string) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		Forbidden(w)
		return
	}

	ctx := r.Context()

	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		NotFound(w, "Project")
		return
	}
	if !s.authorize(w, r, projectResource(project), ActionRead) {
		return
	}

	s.mu.RLock()
	wcs := s.webChatStore
	s.mu.RUnlock()

	if wcs == nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}

	topics, err := wcs.ListTopics(ctx, projectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to list threads", nil)
		return
	}

	for _, t := range topics {
		if t.LastMessageID != "" {
			_ = wcs.SetReadState(ctx, user.ID(), t.ID, t.LastMessageID)
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ---------------------------------------------------------------------------
// DM Endpoints
// ---------------------------------------------------------------------------

// handleChatDMs handles GET /api/v1/chat/dms.
func (s *Server) handleChatDMs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		Forbidden(w)
		return
	}

	ctx := r.Context()

	s.mu.RLock()
	wcs := s.webChatStore
	s.mu.RUnlock()

	if wcs == nil {
		writeJSON(w, http.StatusOK, chatDMListResponse{DMs: []chatDMEntry{}})
		return
	}

	dms, err := wcs.ListDMs(ctx, user.ID())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to list DMs", nil)
		return
	}

	entries := make([]chatDMEntry, 0, len(dms))
	for _, dm := range dms {
		entry := chatDMEntry{
			ConversationKey: dm.ConversationKey,
			PeerID:          dm.PeerID,
			PeerKind:        dm.PeerKind,
			LastMessageID:   dm.LastMessageID,
			LastActivityAt:  dm.LastActivityAt,
		}

		// Enrich with peer info.
		switch dm.PeerKind {
		case "user":
			if peerUser, err := s.store.GetUser(ctx, dm.PeerID); err == nil {
				entry.PeerName = peerUser.DisplayName
				entry.PeerEmail = peerUser.Email
				entry.PeerAvatar = peerUser.AvatarURL
			}
		case "agent":
			if peerAgent, err := s.store.GetAgent(ctx, dm.PeerID); err == nil {
				entry.PeerName = peerAgent.Name
				entry.PeerSlug = peerAgent.Slug
			}
		}

		// Get read state for unread indicator.
		rs, _ := wcs.GetReadState(ctx, user.ID(), dm.ConversationKey)
		if rs != nil {
			entry.LastReadMessageID = rs.LastReadMessageID
			entry.Muted = rs.Muted
			entry.HasUnread = dm.LastMessageID != "" && dm.LastMessageID != rs.LastReadMessageID
		} else {
			entry.HasUnread = dm.LastMessageID != ""
		}

		// Get last message preview.
		if dm.LastMessageID != "" {
			msgMap, err := s.store.GetMessagesByIDs(ctx, []string{dm.LastMessageID})
			if err == nil {
				if msg, ok := msgMap[dm.LastMessageID]; ok {
					entry.LastMessagePreview = truncatePreview(msg.Msg, 120)
					entry.LastMessageSender = msg.Sender
				}
			}
		}

		entries = append(entries, entry)
	}

	writeJSON(w, http.StatusOK, chatDMListResponse{DMs: entries})
}

// ---------------------------------------------------------------------------
// Members Endpoint
// ---------------------------------------------------------------------------

// handleSpaceMembers handles GET /api/v1/chat/spaces/{projectId}/members.
func (s *Server) handleSpaceMembers(w http.ResponseWriter, r *http.Request, projectID string) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		Forbidden(w)
		return
	}

	ctx := r.Context()

	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		NotFound(w, "Project")
		return
	}
	if !s.authorize(w, r, projectResource(project), ActionRead) {
		return
	}

	// --- Read presence manager ---
	s.mu.RLock()
	pm := s.presenceManager
	s.mu.RUnlock()

	// --- Humans: look up the project members group ---
	var humans []chatMemberEntry
	membersSlug := "project:" + project.Slug + ":members"
	group, err := s.store.GetGroupBySlug(ctx, membersSlug)
	if err == nil && group != nil {
		members, err := s.store.GetGroupMembers(ctx, group.ID)
		if err == nil {
			for _, m := range members {
				if m.MemberType != store.GroupMemberTypeUser {
					continue
				}
				u, err := s.store.GetUser(ctx, m.MemberID)
				if err != nil {
					continue
				}
				entry := chatMemberEntry{
					ID:          u.ID,
					Kind:        "user",
					DisplayName: u.DisplayName,
					Email:       u.Email,
					AvatarURL:   u.AvatarURL,
					Role:        m.Role,
				}
				if pm != nil {
					entry.PresenceState = string(pm.GetState(u.ID))
				}
				humans = append(humans, entry)
			}
		}
	}
	if humans == nil {
		humans = []chatMemberEntry{}
	}

	// --- Agents: list agents for the project ---
	var agents []chatMemberEntry
	agentList, err := s.store.ListAgents(ctx, store.AgentFilter{ProjectID: projectID}, store.ListOptions{Limit: 200})
	if err == nil {
		for _, a := range agentList.Items {
			entry := chatMemberEntry{
				ID:          a.ID,
				Kind:        "agent",
				DisplayName: a.Name,
				Slug:        a.Slug,
				Phase:       a.Phase,
				Activity:    a.Activity,
				ProjectID:   a.ProjectID,
				Message:     a.Message,
			}
			if !a.LastSeen.IsZero() {
				entry.LastSeen = a.LastSeen.UTC().Format(time.RFC3339)
			}
			switch {
			case !a.LastActivityEvent.IsZero():
				entry.LastActivityEvent = a.LastActivityEvent.UTC().Format(time.RFC3339)
			case !a.Updated.IsZero():
				entry.LastActivityEvent = a.Updated.UTC().Format(time.RFC3339)
			}
			agents = append(agents, entry)
		}
	}
	if agents == nil {
		agents = []chatMemberEntry{}
	}

	writeJSON(w, http.StatusOK, chatMembersResponse{
		Humans: humans,
		Agents: agents,
	})
}

// ---------------------------------------------------------------------------
// User Preferences (Wave-2 extensions)
// ---------------------------------------------------------------------------

// handleChatUserPrefs handles GET|PUT /api/v1/chat/user-prefs.
// This is separate from the wave-1 handleChatPrefs (which handles per-agent
// visibility mode). This handles rail sort preferences.
func (s *Server) handleChatUserPrefs(w http.ResponseWriter, r *http.Request) {
	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		Forbidden(w)
		return
	}

	ctx := r.Context()

	s.mu.RLock()
	wcs := s.webChatStore
	s.mu.RUnlock()

	if wcs == nil {
		writeError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Chat not available", nil)
		return
	}

	switch r.Method {
	case http.MethodGet:
		prefs, err := wcs.GetUserPrefs(ctx, user.ID())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to read preferences", nil)
			return
		}
		if prefs == nil {
			prefs = &WebChatUserPrefs{
				SpaceSortMode:  "activity",
				ThreadSortMode: "activity",
			}
		}
		writeJSON(w, http.StatusOK, prefs)

	case http.MethodPut:
		r.Body = http.MaxBytesReader(w, r.Body, 1048576)
		var body struct {
			SpaceSortMode  string `json:"spaceSortMode"`
			SpaceOrder     string `json:"spaceOrder"`
			ThreadSortMode string `json:"threadSortMode"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			BadRequest(w, "invalid request body")
			return
		}

		validSortModes := map[string]bool{"activity": true, "alpha": true, "custom": true}
		if body.SpaceSortMode != "" && !validSortModes[body.SpaceSortMode] {
			ValidationError(w, "spaceSortMode must be activity, alpha, or custom", nil)
			return
		}
		validThreadSortModes := map[string]bool{"activity": true, "alpha": true}
		if body.ThreadSortMode != "" && !validThreadSortModes[body.ThreadSortMode] {
			ValidationError(w, "threadSortMode must be activity or alpha", nil)
			return
		}

		prefs := WebChatUserPrefs{
			UserID:         user.ID(),
			SpaceSortMode:  body.SpaceSortMode,
			SpaceOrder:     body.SpaceOrder,
			ThreadSortMode: body.ThreadSortMode,
		}
		if prefs.SpaceSortMode == "" {
			prefs.SpaceSortMode = "activity"
		}
		if prefs.ThreadSortMode == "" {
			prefs.ThreadSortMode = "activity"
		}

		if err := wcs.SetUserPrefs(ctx, user.ID(), prefs); err != nil {
			writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to save preferences", nil)
			return
		}
		writeJSON(w, http.StatusOK, prefs)

	default:
		MethodNotAllowed(w)
	}
}

// ---------------------------------------------------------------------------
// Typing & Presence (W5)
// ---------------------------------------------------------------------------

// handleConversationTyping handles POST /api/v1/chat/conversations/{key}/typing.
// Publishes an ephemeral typing event after authorizing conversation access
// and applying server-side throttling (one event per 4s per user per conversation).
func (s *Server) handleConversationTyping(w http.ResponseWriter, r *http.Request, key string) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		Forbidden(w)
		return
	}

	ctx := r.Context()

	s.mu.RLock()
	wcs := s.webChatStore
	pm := s.presenceManager
	s.mu.RUnlock()

	// --- Authorize conversation access (W2 O3 deferred finding) ---
	isDM := strings.HasPrefix(key, "dm:")
	var projectID string
	if isDM {
		if !validDMKey(key) {
			BadRequest(w, "invalid DM key format")
			return
		}
		if !isDMParticipant(key, user.ID()) {
			Forbidden(w)
			return
		}
		// No project fan-out for DMs — see the publish below.
	} else {
		// Topic key: look up topic to get project ID and check access.
		if wcs == nil {
			writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
			return
		}
		topic, err := wcs.GetTopic(ctx, key)
		if err != nil || topic == nil {
			NotFound(w, "Thread")
			return
		}
		projectID = topic.ProjectID
		project, err := s.store.GetProject(ctx, projectID)
		if err != nil {
			NotFound(w, "Project")
			return
		}
		if !s.authorize(w, r, projectResource(project), ActionRead) {
			return
		}
	}

	// --- Server-side throttle: ignore if last typing from same user < 4s ---
	if pm != nil {
		if !pm.RecordTyping(key, user.ID()) {
			// Throttled — accept silently.
			writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
			return
		}
	}

	// --- Publish ephemeral typing event ---
	displayName := user.DisplayName()
	if displayName == "" {
		displayName = user.Email()
	}
	evt := TypingEvent{
		ThreadID:    key,
		UserID:      user.ID(),
		DisplayName: displayName,
	}
	if projectID != "" {
		s.events.PublishRaw("project."+projectID+".chat.typing", evt)
	}
	if isDM {
		// A DM never reaches a project subject: only the two participants care,
		// and a project publish would both echo the typist their own indicator
		// (they subscribe to their own spaces) and leak a private conversation's
		// activity to the rest of the space. Fan out to the participants'
		// user-scoped subjects the same way DM messages do (see
		// EventPublisher.PublishMessage).
		for _, id := range dmUserParticipants(key) {
			if id == user.ID() {
				continue // don't echo the sender their own typing event
			}
			s.events.PublishRaw("user."+id+".chat.typing", evt)
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleChatPresence handles POST /api/v1/chat/presence.
// Processes a heartbeat from the client, updating the in-memory presence map
// and publishing state transitions via SSE. Design §4.5.
func (s *Server) handleChatPresence(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		Forbidden(w)
		return
	}

	s.mu.RLock()
	pm := s.presenceManager
	s.mu.RUnlock()

	if pm == nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}

	// Parse optional projectIds from the body so we know which project
	// subjects to fan the presence transition out on.
	var body struct {
		ProjectIDs []string `json:"projectIds"`
	}
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, 65536)
		_ = json.NewDecoder(r.Body).Decode(&body)
	}

	displayName := user.DisplayName()
	if displayName == "" {
		displayName = user.Email()
	}

	pm.Heartbeat(r.Context(), user.ID(), displayName, body.ProjectIDs)

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleChatSearch handles GET /api/v1/chat/search.
// Searches chat messages with optional scoping by project or conversation.
//
// Query params:
//   - q: search text (required, minimum 2 characters)
//   - projectId: scope to a single project (optional)
//   - key: scope to a single conversation (optional)
//   - limit: max results (default 50, max 200)
//   - cursor: keyset pagination cursor (optional)
func (s *Server) handleChatSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		Forbidden(w)
		return
	}

	ctx := r.Context()
	q := r.URL.Query()

	// Validate query first (before any store access).
	query := strings.TrimSpace(q.Get("q"))
	if query == "" {
		ValidationError(w, "q is required", nil)
		return
	}
	if len([]rune(query)) < 2 {
		ValidationError(w, "q must be at least 2 characters", nil)
		return
	}

	s.mu.RLock()
	wcs := s.webChatStore
	s.mu.RUnlock()

	if wcs == nil {
		writeJSON(w, http.StatusOK, chatSearchResponse{Results: []ChatSearchResult{}})
		return
	}

	// Parse limit.
	limit := 50
	if l := q.Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}

	filter := ChatSearchFilter{
		Query:  query,
		Limit:  limit,
		Cursor: q.Get("cursor"),
	}

	// Scoping.
	projectID := q.Get("projectId")
	conversationKey := q.Get("key")

	if conversationKey != "" {
		// Scope to one conversation — authorize access.
		isDM := strings.HasPrefix(conversationKey, "dm:")
		if isDM {
			if !validDMKey(conversationKey) {
				BadRequest(w, "invalid DM key format")
				return
			}
			if !isDMParticipant(conversationKey, user.ID()) {
				Forbidden(w)
				return
			}
		} else {
			topic, err := wcs.GetTopic(ctx, conversationKey)
			if err != nil || topic == nil {
				NotFound(w, "Thread")
				return
			}
			project, err := s.store.GetProject(ctx, topic.ProjectID)
			if err != nil {
				NotFound(w, "Project")
				return
			}
			if !s.authorize(w, r, projectResource(project), ActionRead) {
				return
			}
		}
		filter.ConversationKey = conversationKey
	} else if projectID != "" {
		// Scope to one project — authorize access.
		project, err := s.store.GetProject(ctx, projectID)
		if err != nil {
			NotFound(w, "Project")
			return
		}
		if !s.authorize(w, r, projectResource(project), ActionRead) {
			return
		}
		filter.ProjectID = projectID
	} else {
		// Search all visible projects. Filter by user's accessible projects.
		allProjects, err := s.store.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{Limit: 1000})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to list projects", nil)
			return
		}

		identity := GetIdentityFromContext(ctx)
		resources := make([]Resource, len(allProjects.Items))
		for i := range allProjects.Items {
			resources[i] = projectResource(&allProjects.Items[i])
		}
		caps := s.authzService.ComputeCapabilitiesBatch(ctx, identity, resources, "project")

		var visibleIDs []string
		for i, p := range allProjects.Items {
			if capabilityAllows(caps[i], ActionRead) {
				visibleIDs = append(visibleIDs, p.ID)
			}
		}
		if len(visibleIDs) == 0 {
			writeJSON(w, http.StatusOK, chatSearchResponse{Results: []ChatSearchResult{}})
			return
		}
		filter.ProjectIDs = visibleIDs
	}

	results, nextCursor, err := wcs.SearchChatMessages(ctx, filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "search failed", nil)
		return
	}

	// Enrich results with thread/DM names.
	s.enrichSearchResults(ctx, wcs, results)

	if results == nil {
		results = []ChatSearchResult{}
	}

	writeJSON(w, http.StatusOK, chatSearchResponse{
		Results:    results,
		NextCursor: nextCursor,
	})
}

// ---------------------------------------------------------------------------
// Helper functions
// ---------------------------------------------------------------------------

// isDMParticipant checks whether the given userID is one of the two
// participants encoded in a DM conversation key by parsing tokens exactly.
func isDMParticipant(key, userID string) bool {
	// DM key formats:
	//   dm:agent:<agentUUID>:user:<userUUID>
	//   dm:user:<uuidA>:user:<uuidB>
	parts := strings.Split(key, ":")
	if len(parts) < 5 {
		return false
	}
	return parts[2] == userID || parts[4] == userID
}

// dmUserParticipants returns the user IDs named in a DM key, skipping the agent
// side of an agent DM. Keys have the form dm:<kind>:<id>:<kind>:<id>.
func dmUserParticipants(key string) []string {
	parts := strings.Split(key, ":")
	if len(parts) < 5 {
		return nil
	}
	var ids []string
	for _, i := range []int{1, 3} {
		if parts[i] != "user" {
			continue
		}
		id := parts[i+1]
		if id == "" || slices.Contains(ids, id) {
			continue
		}
		ids = append(ids, id)
	}
	return ids
}

// resolveDMPeer extracts the peer's ID from a DM key given the caller's ID.
// The caller can be either a user or an agent.
func resolveDMPeer(key, callerID string) (peerEmail, peerID string) {
	// dm:agent:<agentUUID>:user:<userUUID>
	// dm:user:<uuidA>:user:<uuidB>
	parts := strings.Split(key, ":")
	// Expected: [dm, kind1, id1, kind2, id2]
	if len(parts) < 5 {
		return "", ""
	}

	id1, id2 := parts[2], parts[4]

	if id1 == callerID {
		return "", id2
	}
	return "", id1
}

// parseAgentDMKey extracts the agent UUID from an agent-DM conversation key.
// Returns "" if the key is not an agent DM. Agent DM keys have the form
// dm:agent:<agentUUID>:user:<userUUID>.
func parseAgentDMKey(key string) string {
	parts := strings.Split(key, ":")
	if len(parts) >= 3 && parts[1] == "agent" {
		return parts[2]
	}
	return ""
}

// resolveProjectFromDMKey attempts to derive a project ID from a DM key.
// For agent DMs (dm:agent:<id>:user:<id>), looks up the agent's project.
func resolveProjectFromDMKey(ctx context.Context, s *Server, key string) string {
	parts := strings.Split(key, ":")
	if len(parts) >= 3 && parts[1] == "agent" {
		agent, err := s.store.GetAgent(ctx, parts[2])
		if err == nil && agent != nil {
			return agent.ProjectID
		}
	}
	return ""
}

// touchConversationActivity updates the watermark for a conversation.
func (s *Server) touchConversationActivity(ctx context.Context, key, messageID string) {
	s.mu.RLock()
	wcs := s.webChatStore
	s.mu.RUnlock()

	if wcs == nil {
		return
	}

	if strings.HasPrefix(key, "dm:") {
		if err := wcs.TouchDMActivity(ctx, key, messageID); err != nil {
			s.messageLog.Error("Failed to touch DM activity", "key", key, "error", err)
		}
	} else {
		if err := wcs.TouchTopicActivity(ctx, key, messageID); err != nil {
			s.messageLog.Error("Failed to touch topic activity", "key", key, "error", err)
		}
	}
}

// ensureDMRegistered ensures both participants in a DM have registry rows.
func (s *Server) ensureDMRegistered(ctx context.Context, key, callerID string) {
	s.mu.RLock()
	wcs := s.webChatStore
	s.mu.RUnlock()

	registerDMParticipants(ctx, wcs, key)
}

// registerDMParticipants upserts one webchat_dm row per participant of a DM
// conversation key (dm:<kind1>:<id1>:<kind2>:<id2>). It is a no-op for a nil
// store or a malformed key.
//
// Registration must happen before any TouchDMActivity call: TouchDMActivity is
// a plain UPDATE and silently affects zero rows when the registry rows do not
// exist yet — which is the case when an agent is the first to speak in a DM.
func registerDMParticipants(ctx context.Context, wcs WebChatStore, key string) {
	if wcs == nil {
		return
	}

	parts := strings.Split(key, ":")
	if len(parts) < 5 {
		return
	}

	kind1, id1 := parts[1], parts[2]
	kind2, id2 := parts[3], parts[4]

	// Upsert two rows — one per participant.
	now := time.Now().UTC()

	// Participant 1 → Peer 2.
	peerKind2 := kind2
	if peerKind2 != "agent" {
		peerKind2 = "user"
	}
	_ = wcs.UpsertDM(ctx, WebChatDM{
		ConversationKey: key,
		ParticipantID:   id1,
		PeerID:          id2,
		PeerKind:        peerKind2,
		LastActivityAt:  now,
	})

	// Participant 2 → Peer 1.
	peerKind1 := kind1
	if peerKind1 != "agent" {
		peerKind1 = "user"
	}
	_ = wcs.UpsertDM(ctx, WebChatDM{
		ConversationKey: key,
		ParticipantID:   id2,
		PeerID:          id1,
		PeerKind:        peerKind1,
		LastActivityAt:  now,
	})
}

// ---------------------------------------------------------------------------
// W6: Chat notification helpers
// ---------------------------------------------------------------------------

// fireHumanMentionNotifications resolves @mention names against project members
// (humans, not agents) and fires a notification for each match. The sender is
// excluded from notifications. Agent slugs are skipped — they already get
// type:mention messages through the existing pipeline.
func (s *Server) fireHumanMentionNotifications(ctx context.Context, mentionNames []string, projectID, conversationKey, senderUserID, senderName, messageContent string) {
	cn := s.getChatNotifier()
	if cn == nil {
		return
	}

	// Resolve human members for the project.
	humanMembers := s.resolveProjectHumanMembers(ctx, projectID)
	if len(humanMembers) == 0 {
		return
	}

	// Build a lookup by lowercase display name and email.
	type memberInfo struct {
		ID          string
		DisplayName string
	}
	lookup := make(map[string]memberInfo)
	for _, m := range humanMembers {
		if m.DisplayName != "" {
			lookup[strings.ToLower(m.DisplayName)] = memberInfo{ID: m.ID, DisplayName: m.DisplayName}
		}
		if m.Email != "" {
			// Also match by email prefix (before @).
			lookup[strings.ToLower(m.Email)] = memberInfo{ID: m.ID, DisplayName: m.DisplayName}
			if at := strings.IndexByte(m.Email, '@'); at > 0 {
				lookup[strings.ToLower(m.Email[:at])] = memberInfo{ID: m.ID, DisplayName: m.DisplayName}
			}
		}
	}

	// Resolve the conversation name for the notification message.
	conversationName := ""
	if !strings.HasPrefix(conversationKey, "dm:") {
		s.mu.RLock()
		wcs := s.webChatStore
		s.mu.RUnlock()
		if wcs != nil {
			if topic, err := wcs.GetTopic(ctx, conversationKey); err == nil && topic != nil {
				conversationName = topic.Name
			}
		}
	}

	seen := make(map[string]bool)
	for _, name := range mentionNames {
		lower := strings.ToLower(name)
		member, ok := lookup[lower]
		if !ok {
			continue
		}
		// Skip the sender — don't notify yourself.
		if member.ID == senderUserID {
			continue
		}
		// Deduplicate.
		if seen[member.ID] {
			continue
		}
		seen[member.ID] = true

		cn.NotifyMention(ctx, member.ID, ChatMessageContext{
			SenderID:         senderUserID,
			SenderName:       senderName,
			ConversationKey:  conversationKey,
			ConversationName: conversationName,
			Preview:          messageContent,
			ProjectID:        projectID,
		})
	}
}

// resolveProjectHumanMembers returns the human members of a project by
// looking up the project's members group. This is used to match @mentions
// against human display names.
func (s *Server) resolveProjectHumanMembers(ctx context.Context, projectID string) []chatMemberEntry {
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		return nil
	}

	membersSlug := "project:" + project.Slug + ":members"
	group, err := s.store.GetGroupBySlug(ctx, membersSlug)
	if err != nil || group == nil {
		return nil
	}

	members, err := s.store.GetGroupMembers(ctx, group.ID)
	if err != nil {
		return nil
	}

	var humans []chatMemberEntry
	for _, m := range members {
		if m.MemberType != store.GroupMemberTypeUser {
			continue
		}
		u, err := s.store.GetUser(ctx, m.MemberID)
		if err != nil {
			continue
		}
		humans = append(humans, chatMemberEntry{
			ID:          u.ID,
			Kind:        "user",
			DisplayName: u.DisplayName,
			Email:       u.Email,
		})
	}
	return humans
}

// ---------------------------------------------------------------------------
// W8 Search helpers
// ---------------------------------------------------------------------------

// enrichSearchResults populates the ThreadName field of search results by
// looking up topic names and DM peer names.
func (s *Server) enrichSearchResults(ctx context.Context, wcs WebChatStore, results []ChatSearchResult) {
	// Collect unique conversation keys.
	seen := make(map[string]bool, len(results))
	for _, r := range results {
		seen[r.ConversationKey] = true
	}

	// Resolve names.
	names := make(map[string]string, len(seen))
	for key := range seen {
		if key == "" {
			continue
		}
		if strings.HasPrefix(key, "dm:") {
			// For DMs, derive a name from the key format.
			parts := strings.Split(key, ":")
			if len(parts) >= 5 {
				peerKind := parts[1]
				peerID := parts[2]
				if peerKind == "agent" {
					if a, err := s.store.GetAgent(ctx, peerID); err == nil && a != nil {
						names[key] = "DM: " + a.Name
						continue
					}
				} else if peerKind == "user" {
					if u, err := s.store.GetUser(ctx, peerID); err == nil {
						names[key] = "DM: " + u.DisplayName
						continue
					}
				}
			}
			names[key] = "DM"
		} else {
			// Topic thread — look up name.
			if topic, err := wcs.GetTopic(ctx, key); err == nil && topic != nil {
				names[key] = "#" + topic.Name
			}
		}
	}

	// Apply names.
	for i := range results {
		if name, ok := names[results[i].ConversationKey]; ok {
			results[i].ThreadName = name
		}
	}
}

// ---------------------------------------------------------------------------
// Response types
// ---------------------------------------------------------------------------

type chatSpacesResponse struct {
	Spaces []chatSpaceEntry `json:"spaces"`
	Prefs  *chatSpacePrefs  `json:"prefs,omitempty"`
}

type chatSpaceEntry struct {
	ProjectID   string `json:"projectId"`
	ProjectName string `json:"projectName"`
	ProjectSlug string `json:"projectSlug"`
	ThreadCount int    `json:"threadCount"`
	UnreadCount int    `json:"unreadCount"`
}

type chatSpacePrefs struct {
	SpaceSortMode  string `json:"spaceSortMode"`
	SpaceOrder     string `json:"spaceOrder,omitempty"`
	ThreadSortMode string `json:"threadSortMode"`
}

type chatTopicListResponse struct {
	Threads []chatTopicEntry `json:"threads"`
}

type chatTopicEntry struct {
	ID                string    `json:"id"`
	ProjectID         string    `json:"projectId"`
	Name              string    `json:"name"`
	IsGeneral         bool      `json:"isGeneral"`
	DefaultAgent      string    `json:"defaultAgent,omitempty"`
	CreatedBy         string    `json:"createdBy"`
	CreatedAt         time.Time `json:"createdAt"`
	LastMessageID     string    `json:"lastMessageId,omitempty"`
	LastActivityAt    time.Time `json:"lastActivityAt"`
	LastReadMessageID string    `json:"lastReadMessageId,omitempty"`
	Pinned            bool      `json:"pinned"`
	Muted             bool      `json:"muted"`
	HasUnread         bool      `json:"hasUnread"`
}

type chatMessageResponse struct {
	ID          string                   `json:"id"`
	Content     string                   `json:"content"`
	Sender      string                   `json:"sender"`
	SenderID    string                   `json:"senderId"`
	Type        string                   `json:"type"`
	CreatedAt   time.Time                `json:"createdAt"`
	Mentions    []messages.MentionResult `json:"mentions,omitempty"`
	Attachments []AttachmentRef          `json:"attachments,omitempty"` // W7
}

type chatHistoryResponse struct {
	Messages           []store.Message               `json:"messages"`
	NextCursor         string                        `json:"nextCursor,omitempty"`
	TotalCount         int                           `json:"totalCount"`
	MessageAttachments map[string][]AttachmentRef    `json:"messageAttachments,omitempty"` // W7: keyed by message ID
	MessageExtensions  map[string]*WebChatMessageExt `json:"messageExtensions,omitempty"`  // Phase-3: keyed by message ID
	ReplyPreviews      map[string]chatReplyPreview   `json:"replyPreviews,omitempty"`      // Phase-3: keyed by reply-to message ID
}

// chatReplyPreview provides a truncated preview of the message being replied to.
type chatReplyPreview struct {
	MessageID  string `json:"messageId"`
	SenderName string `json:"senderName"`
	Content    string `json:"content"` // truncated to 100 chars
}

type chatDMListResponse struct {
	DMs []chatDMEntry `json:"dms"`
}

type chatDMEntry struct {
	ConversationKey    string    `json:"conversationKey"`
	PeerID             string    `json:"peerId"`
	PeerKind           string    `json:"peerKind"`
	PeerName           string    `json:"peerName,omitempty"`
	PeerEmail          string    `json:"peerEmail,omitempty"`
	PeerSlug           string    `json:"peerSlug,omitempty"`
	PeerAvatar         string    `json:"peerAvatar,omitempty"`
	LastMessageID      string    `json:"lastMessageId,omitempty"`
	LastActivityAt     time.Time `json:"lastActivityAt"`
	LastReadMessageID  string    `json:"lastReadMessageId,omitempty"`
	HasUnread          bool      `json:"hasUnread"`
	Muted              bool      `json:"muted"`
	LastMessagePreview string    `json:"lastMessagePreview,omitempty"`
	LastMessageSender  string    `json:"lastMessageSender,omitempty"`
}

type chatMembersResponse struct {
	Humans []chatMemberEntry `json:"humans"`
	Agents []chatMemberEntry `json:"agents"`
}

type chatSearchResponse struct {
	Results    []ChatSearchResult `json:"results"`
	NextCursor string             `json:"nextCursor,omitempty"`
}

type chatMemberEntry struct {
	ID            string `json:"id"`
	Kind          string `json:"kind"`
	DisplayName   string `json:"displayName"`
	Email         string `json:"email,omitempty"`
	AvatarURL     string `json:"avatarUrl,omitempty"`
	Slug          string `json:"slug,omitempty"`
	Role          string `json:"role,omitempty"`
	Phase         string `json:"phase,omitempty"`
	Activity      string `json:"activity,omitempty"`
	PresenceState string `json:"presenceState,omitempty"`
	// Agent-only fields. LastSeen is RFC3339; empty when the agent has
	// never reported in.
	LastSeen  string `json:"lastSeen,omitempty"`
	ProjectID string `json:"projectId,omitempty"`
	// Message is the agent's freeform status detail — the text the agent
	// detail page shows under "Detail" (e.g. "Waiting for user decision").
	// Named to match store.Agent so /api/v1/agents and this endpoint can be
	// consumed by the same client mapping.
	Message string `json:"message,omitempty"`
	// LastActivityEvent is when the agent last changed state, RFC3339, with
	// the record's update time as a fallback. Distinct from LastSeen, which
	// is a heartbeat and moves even when nothing happened.
	LastActivityEvent string `json:"lastActivityEvent,omitempty"`
}

// ---------------------------------------------------------------------------
// W7: Attachment upload / download handlers
// ---------------------------------------------------------------------------

// handleChatAttachments dispatches /api/v1/chat/attachments.
// POST = upload, GET with /{id} = download.
func (s *Server) handleChatAttachments(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.handleAttachmentUpload(w, r)
	default:
		MethodNotAllowed(w)
	}
}

// handleChatAttachmentByID dispatches /api/v1/chat/attachments/{id}.
func (s *Server) handleChatAttachmentByID(w http.ResponseWriter, r *http.Request) {
	// Extract attachment ID from path: /api/v1/chat/attachments/{id}
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/chat/attachments/")
	id := strings.TrimRight(path, "/")
	if id == "" {
		NotFound(w, "Attachment")
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.handleAttachmentDownload(w, r, id)
	default:
		MethodNotAllowed(w)
	}
}

// handleAttachmentUpload handles POST /api/v1/chat/attachments (multipart form).
func (s *Server) handleAttachmentUpload(w http.ResponseWriter, r *http.Request) {
	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		Forbidden(w)
		return
	}

	ctx := r.Context()

	s.mu.RLock()
	wcs := s.webChatStore
	as := s.attachmentStore
	s.mu.RUnlock()

	if wcs == nil || as == nil {
		writeError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Attachments not available", nil)
		return
	}

	// project_id scopes the upload to a space. DMs belong to no space, so it is
	// optional: an upload without one is stored project-less and reachable only
	// through the messages that reference it.
	projectID := r.FormValue("project_id")
	if projectID == "" {
		// Try multipart form value.
		if err := r.ParseMultipartForm(MaxAttachmentSize * MaxAttachmentsPerMessage); err == nil {
			projectID = r.FormValue("project_id")
		}
	}

	// Authorize: user must have read access to the project (same as sending
	// messages). A project-less upload has nothing to authorize against beyond
	// the authenticated identity the handler already established.
	if projectID != "" {
		project, err := s.store.GetProject(ctx, projectID)
		if err != nil {
			NotFound(w, "Project")
			return
		}
		if !s.authorize(w, r, projectResource(project), ActionRead) {
			return
		}
	}

	// Parse multipart form (limit total to MaxAttachmentSize * MaxAttachmentsPerMessage).
	r.Body = http.MaxBytesReader(w, r.Body, int64(MaxAttachmentSize*MaxAttachmentsPerMessage)+1024*1024)
	if err := r.ParseMultipartForm(MaxAttachmentSize * MaxAttachmentsPerMessage); err != nil {
		BadRequest(w, "invalid multipart form or request too large")
		return
	}

	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		ValidationError(w, "no files uploaded", nil)
		return
	}
	if len(files) > MaxAttachmentsPerMessage {
		ValidationError(w, fmt.Sprintf("too many files: %d (max %d)", len(files), MaxAttachmentsPerMessage), nil)
		return
	}

	// One bad file in a selection of ten used to lose the other nine. Each file
	// now succeeds or fails on its own and the response reports both, so the
	// composer can keep what worked and name what did not.
	results := make([]attachmentUploadResult, 0, len(files))
	failures := make([]attachmentUploadFailure, 0)
	internalError := false
	for _, fh := range files {
		result, err := s.storeUploadedFile(ctx, as, wcs, projectID, user.ID(), fh)
		if err != nil {
			var rejection attachmentRejection
			if !errors.As(err, &rejection) {
				internalError = true
				s.messageLog.Error("Attachment upload failed", "file", fh.Filename, "error", err)
			}
			failures = append(failures, attachmentUploadFailure{
				Name:  fh.Filename,
				Error: uploadFailureMessage(err),
			})
			continue
		}
		results = append(results, result)
	}

	// 201 whenever something was created, even alongside failures: the
	// response body is where per-file outcomes live, and a client that got
	// attachments back has to treat the request as having created them.
	// Nothing created means nothing to report as created — 400 for a batch the
	// caller can fix, 500 if the batch died on our side. 207 Multi-Status was
	// the other candidate and was passed over: it is a WebDAV code that
	// browsers and fetch wrappers treat as an oddity, for no gain over reading
	// the body that has to be read anyway.
	status := http.StatusCreated
	if len(results) == 0 {
		status = http.StatusBadRequest
		if internalError {
			status = http.StatusInternalServerError
		}
	}

	writeJSON(w, status, map[string]interface{}{
		"attachments": results,
		"failures":    failures,
	})
}

// attachmentRejection is a refusal the uploader caused and could fix — a
// blocked extension, an unreadable type, an oversized file. Anything else is
// ours and is reported as an internal error instead.
type attachmentRejection struct{ msg string }

func (e attachmentRejection) Error() string { return e.msg }

func rejectAttachment(format string, args ...interface{}) error {
	return attachmentRejection{msg: fmt.Sprintf(format, args...)}
}

// uploadFailureMessage renders a per-file failure for the composer. Rejections
// speak for themselves; internal failures are logged in full and summarised
// here, since their detail is about our storage, not the user's file.
func uploadFailureMessage(err error) string {
	var rejection attachmentRejection
	if errors.As(err, &rejection) {
		return rejection.msg
	}
	return "upload failed"
}

// storeUploadedFile validates, classifies, and stores one uploaded file.
func (s *Server) storeUploadedFile(
	ctx context.Context, as AttachmentStore, wcs WebChatStore,
	projectID, userID string, fh *multipart.FileHeader,
) (attachmentUploadResult, error) {
	if fh.Size > MaxAttachmentSize {
		return attachmentUploadResult{}, rejectAttachment(
			"file exceeds the maximum size of %d bytes", MaxAttachmentSize)
	}

	safeName, err := SanitizeFilename(fh.Filename)
	if err != nil {
		// Passed through without a prefix. Every error this can return already
		// names the problem — "invalid filename", or the refused extension —
		// and the failure entry carries the filename beside it, so a wrapper
		// only stacks two subjects on one line ("invalid filename: invalid
		// filename"). Nothing here echoes the uploader's text: the two
		// extension errors interpolate a key of our own blocklists (#1045).
		return attachmentUploadResult{}, attachmentRejection{msg: err.Error()}
	}

	file, err := fh.Open()
	if err != nil {
		return attachmentUploadResult{}, fmt.Errorf("open uploaded file: %w", err)
	}
	defer func() { _ = file.Close() }()

	// Classify from the content, not from the Content-Type the client put on
	// the part: that header is a claim the uploader controls.
	head := make([]byte, contentSniffLen)
	n, err := io.ReadFull(file, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return attachmentUploadResult{}, fmt.Errorf("read uploaded file: %w", err)
	}
	mimeType, err := ClassifyAttachment(safeName, head[:n])
	if err != nil {
		return attachmentUploadResult{}, attachmentRejection{msg: err.Error()}
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return attachmentUploadResult{}, fmt.Errorf("rewind uploaded file: %w", err)
	}

	meta, err := as.Save(ctx, projectID, safeName, file, fh.Size, mimeType)
	if err != nil {
		return attachmentUploadResult{}, fmt.Errorf("save file: %w", err)
	}
	meta.UploadedBy = userID

	if err := wcs.CreateAttachment(ctx, meta); err != nil {
		// The blob is already on disk and nothing will ever reach it again: the
		// download path finds an attachment through the row that just failed to
		// be written, so what is left is storage no one can list or delete.
		// Aborting the batch on the first failure used to cap that at one blob
		// per request; the per-file loop makes it ten (#1089).
		if delErr := as.Delete(ctx, projectID, meta.ID); delErr != nil {
			// The blob is orphaned after all. Say so: nothing else will.
			s.messageLog.Error("Failed to delete orphaned attachment blob",
				"project_id", projectID, "attachment", meta.ID, "error", delErr)
		}
		return attachmentUploadResult{}, fmt.Errorf("save attachment metadata: %w", err)
	}

	return attachmentUploadResult{
		ID:       meta.ID,
		Name:     meta.Filename,
		MimeType: meta.MimeType,
		Size:     meta.Size,
		URL:      "/api/v1/chat/attachments/" + meta.ID,
	}, nil
}

// handleAttachmentDownload handles GET /api/v1/chat/attachments/{id}.
func (s *Server) handleAttachmentDownload(w http.ResponseWriter, r *http.Request, id string) {
	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		Forbidden(w)
		return
	}

	ctx := r.Context()

	s.mu.RLock()
	wcs := s.webChatStore
	as := s.attachmentStore
	s.mu.RUnlock()

	if wcs == nil || as == nil {
		writeError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Attachments not available", nil)
		return
	}

	// Look up metadata from DB.
	meta, err := wcs.GetAttachment(ctx, id)
	if err != nil || meta == nil {
		NotFound(w, "Attachment")
		return
	}

	// Authorize: user must have read access to the project. An attachment
	// uploaded from a DM has no project (see handleAttachmentUpload); the
	// authenticated identity plus the unguessable attachment ID is all there is
	// to check, so the download proceeds.
	if meta.ProjectID != "" {
		project, err := s.store.GetProject(ctx, meta.ProjectID)
		if err != nil {
			NotFound(w, "Project")
			return
		}
		if !s.authorize(w, r, projectResource(project), ActionRead) {
			return
		}
	}

	// Get file from storage.
	reader, fileMeta, err := as.Get(ctx, meta.ProjectID, id)
	if err != nil {
		NotFound(w, "Attachment file")
		return
	}
	defer func() { _ = reader.Close() }()

	// Set headers.
	mimeType := meta.MimeType
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", mimeType)

	// R1: Prevent browsers from MIME-sniffing the response body.
	w.Header().Set("X-Content-Type-Options", "nosniff")

	// Content-Disposition: inline for images, attachment for everything else.
	disposition := "attachment"
	if IsImageMime(mimeType) {
		disposition = "inline"
	}
	// R2: Escape backslash and double-quote in the filename to prevent
	// Content-Disposition header injection (RFC 6266 §4.3).
	safeName := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(meta.Filename)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`%s; filename="%s"`, disposition, safeName))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", fileMeta.Size))
	w.Header().Set("Cache-Control", "private, max-age=3600")

	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, reader)
}

// attachmentUploadFailure is one file the batch could not take, named so the
// composer can say which of the dropped files did not make it and why.
type attachmentUploadFailure struct {
	Name  string `json:"name"`
	Error string `json:"error"`
}

type attachmentUploadResult struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	MimeType string `json:"mime"`
	Size     int64  `json:"size"`
	URL      string `json:"url"`
}
