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
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
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
	switch action {
	case "messages":
		s.handleConversationMessages(w, r, key)
	case "read":
		s.handleConversationRead(w, r, key)
	case "typing":
		s.handleConversationTyping(w, r, key)
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

	// --- Validate body ---
	r.Body = http.MaxBytesReader(w, r.Body, 1048576)
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
	if utf8.RuneCountInString(content) > messages.MaxMessageLength {
		ValidationError(w, fmt.Sprintf("message exceeds %d character limit", messages.MaxMessageLength), nil)
		return
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

	// Step 3: Determine routing.
	if len(mentionedAgents) > 0 {
		// --- Agent-routed: explicit mentions ---
		s.sendAgentRouted(w, r, key, projectID, user, content, senderLabel, mentionedAgents, mentionResults, now)
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
				s.sendAgentRouted(w, r, key, projectID, user, content, senderLabel, []*store.Agent{dmAgent}, nil, now)
				// Ensure DM registry rows exist so the DM appears in the rail.
				s.ensureDMRegistered(ctx, key, user.ID())
				return
			}
		}
	} else if projectID != "" {
		// Check if topic has a default_agent.
		topic, err := wcs.GetTopic(ctx, key)
		if err == nil && topic != nil && topic.DefaultAgent != "" {
			// Resolve the default agent.
			defaultAgent, err := s.store.GetAgent(ctx, topic.DefaultAgent)
			if err == nil && defaultAgent != nil {
				s.sendAgentRouted(w, r, key, projectID, user, content, senderLabel, []*store.Agent{defaultAgent}, nil, now)
				return
			}
		}
	}

	// --- Human-to-human message ---
	s.sendHumanToHuman(w, r, key, projectID, user, content, senderLabel, isDM, now)
}

// sendAgentRouted sends a message through the existing agent dispatch path.
func (s *Server) sendAgentRouted(w http.ResponseWriter, r *http.Request, key, projectID string, user UserIdentity,
	content, senderLabel string, agents []*store.Agent, mentionResults []messages.MentionResult, now time.Time) {

	ctx := r.Context()

	if len(agents) == 0 {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "no agent to route to", nil)
		return
	}

	primaryAgent := agents[0]

	// Build the structured message for the primary agent.
	msg := &messages.StructuredMessage{
		Version:     messages.Version,
		Timestamp:   now.Format(time.RFC3339),
		Sender:      "user:" + senderLabel,
		SenderID:    user.ID(),
		Recipient:   "agent:" + primaryAgent.Slug,
		RecipientID: primaryAgent.ID,
		Msg:         content,
		Type:        messages.TypeInstruction,
		Channel:     "web",
		ThreadID:    key,
	}

	// Persist the instruction message.
	storeMsg := &store.Message{
		ID:            api.NewUUID(),
		ProjectID:     projectID,
		Sender:        msg.Sender,
		SenderID:      msg.SenderID,
		Recipient:     msg.Recipient,
		RecipientID:   msg.RecipientID,
		Msg:           content,
		Type:          messages.TypeInstruction,
		AgentID:       primaryAgent.ID,
		Channel:       "web",
		ThreadID:      key,
		DispatchState: store.MessageDispatchDispatched,
		CreatedAt:     now,
	}
	if err := s.store.CreateMessage(ctx, storeMsg); err != nil {
		s.messageLog.Error("Failed to persist agent-routed message", "error", err)
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to persist message", nil)
		return
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

	writeJSON(w, http.StatusCreated, chatMessageResponse{
		ID:        storeMsg.ID,
		Content:   content,
		Sender:    storeMsg.Sender,
		SenderID:  storeMsg.SenderID,
		Type:      storeMsg.Type,
		CreatedAt: now,
		Mentions:  mentionResults,
	})
}

// sendHumanToHuman persists a type:chat message for human-to-human communication.
func (s *Server) sendHumanToHuman(w http.ResponseWriter, r *http.Request, key, projectID string, user UserIdentity,
	content, senderLabel string, isDM bool, now time.Time) {

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
		ID:          api.NewUUID(),
		ProjectID:   msgProjectID,
		Sender:      "user:" + senderLabel,
		SenderID:    user.ID(),
		Recipient:   recipient,
		RecipientID: recipientID,
		Msg:         content,
		Type:        messages.TypeChat,
		Channel:     "web",
		ThreadID:    key,
		CreatedAt:   now,
	}

	if err := s.store.CreateMessage(ctx, storeMsg); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to persist message", nil)
		return
	}

	// Publish SSE event.
	s.events.PublishUserMessage(ctx, storeMsg)

	// Update conversation watermark.
	if wcs != nil {
		s.touchConversationActivity(ctx, key, storeMsg.ID)
	}

	// For DMs, ensure DM registry rows exist for both participants.
	if isDM && wcs != nil {
		s.ensureDMRegistered(ctx, key, user.ID())
	}

	writeJSON(w, http.StatusCreated, chatMessageResponse{
		ID:        storeMsg.ID,
		Content:   content,
		Sender:    storeMsg.Sender,
		SenderID:  storeMsg.SenderID,
		Type:      storeMsg.Type,
		CreatedAt: now,
	})
}

// ---------------------------------------------------------------------------
// Message History
// ---------------------------------------------------------------------------

// handleConversationHistory handles GET /api/v1/chat/conversations/{key}/messages.
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
		Limit:  limit,
		Cursor: q.Get("before"),
	}

	result, err := s.store.ListMessages(ctx, filter, opts)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to fetch messages", nil)
		return
	}

	if result.Items == nil {
		result.Items = []store.Message{}
	}

	writeJSON(w, http.StatusOK, chatHistoryResponse{
		Messages:   result.Items,
		NextCursor: result.NextCursor,
		TotalCount: result.TotalCount,
	})
}

// ---------------------------------------------------------------------------
// Read Watermarks
// ---------------------------------------------------------------------------

// handleConversationRead handles POST /api/v1/chat/conversations/{key}/read.
func (s *Server) handleConversationRead(w http.ResponseWriter, r *http.Request, key string) {
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
	s.mu.RUnlock()

	if wcs == nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}

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

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
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
				humans = append(humans, chatMemberEntry{
					ID:          u.ID,
					Kind:        "user",
					DisplayName: u.DisplayName,
					Email:       u.Email,
					AvatarURL:   u.AvatarURL,
					Role:        m.Role,
				})
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
			agents = append(agents, chatMemberEntry{
				ID:          a.ID,
				Kind:        "agent",
				DisplayName: a.Name,
				Slug:        a.Slug,
				Phase:       a.Phase,
				Activity:    a.Activity,
			})
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
// Typing & Presence stubs (W5)
// ---------------------------------------------------------------------------

// handleConversationTyping handles POST /api/v1/chat/conversations/{key}/typing.
// Stub for W5 — publishes an ephemeral typing event.
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

	// Publish ephemeral typing event on the project subject.
	// For now just accept and acknowledge — full implementation in W5.
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleChatPresence handles POST /api/v1/chat/presence.
// Stub for W5 — heartbeat endpoint.
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

	// Full presence implementation in W5.
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleChatSearch handles GET /api/v1/chat/search.
// Stub for W8.
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

	// Full search implementation in W8.
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"results":    []interface{}{},
		"totalCount": 0,
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
	ID        string                   `json:"id"`
	Content   string                   `json:"content"`
	Sender    string                   `json:"sender"`
	SenderID  string                   `json:"senderId"`
	Type      string                   `json:"type"`
	CreatedAt time.Time                `json:"createdAt"`
	Mentions  []messages.MentionResult `json:"mentions,omitempty"`
}

type chatHistoryResponse struct {
	Messages   []store.Message `json:"messages"`
	NextCursor string          `json:"nextCursor,omitempty"`
	TotalCount int             `json:"totalCount"`
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
	LastMessagePreview string    `json:"lastMessagePreview,omitempty"`
	LastMessageSender  string    `json:"lastMessageSender,omitempty"`
}

type chatMembersResponse struct {
	Humans []chatMemberEntry `json:"humans"`
	Agents []chatMemberEntry `json:"agents"`
}

type chatMemberEntry struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	DisplayName string `json:"displayName"`
	Email       string `json:"email,omitempty"`
	AvatarURL   string `json:"avatarUrl,omitempty"`
	Slug        string `json:"slug,omitempty"`
	Role        string `json:"role,omitempty"`
	Phase       string `json:"phase,omitempty"`
	Activity    string `json:"activity,omitempty"`
}
