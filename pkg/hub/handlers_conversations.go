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
	"net/http"
	"strconv"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// conversationResponse wraps a conversation with its participants for API responses.
type conversationResponse struct {
	store.Conversation
	Participants []store.ConversationParticipant `json:"participants,omitempty"`
}

// conversationListResponse is the response for listing conversations.
type conversationListResponse struct {
	Conversations []conversationResponse `json:"conversations"`
	Cursor        string                 `json:"cursor,omitempty"`
}

// createConversationRequest is the request body for creating a conversation.
type createConversationRequest struct {
	DisplayName string `json:"displayName"`
	ProjectID   string `json:"projectId"`
	Kind        string `json:"kind"`
}

// setDefaultAgentRequest is the request body for setting the default agent.
type setDefaultAgentRequest struct {
	AgentID string `json:"agentId"`
}

// handleListConversations handles GET /api/v1/conversations.
// Lists conversations for the authenticated caller (user or agent).
func (s *Server) handleListConversations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}

	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Forbidden(w)
		return
	}

	principalKind := identity.Type()
	principalID := identity.ID()

	// Get all conversations for the caller.
	conversations, err := s.store.GetConversationsForPrincipal(ctx, principalKind, principalID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	q := r.URL.Query()
	kindFilter := q.Get("kind")
	surfaceFilter := q.Get("surface")
	projectFilter := q.Get("project_id")
	limitStr := q.Get("limit")

	limit := 0
	if limitStr != "" {
		if n, parseErr := strconv.Atoi(limitStr); parseErr == nil && n > 0 {
			limit = n
		}
	}

	// Apply client-side filtering.
	var filtered []store.Conversation
	for _, conv := range conversations {
		if kindFilter != "" && conv.Kind != kindFilter {
			continue
		}
		if surfaceFilter != "" && conv.Surface != surfaceFilter {
			continue
		}
		if projectFilter != "" {
			if conv.ProjectID == nil || *conv.ProjectID != projectFilter {
				continue
			}
		}
		filtered = append(filtered, conv)
	}

	// Apply limit.
	if limit > 0 && len(filtered) > limit {
		filtered = filtered[:limit]
	}

	// Build response with participants for each conversation.
	result := conversationListResponse{
		Conversations: make([]conversationResponse, 0, len(filtered)),
	}
	for _, conv := range filtered {
		participants, pErr := s.store.ListParticipants(ctx, conv.ID)
		if pErr != nil {
			writeErrorFromErr(w, pErr, "")
			return
		}
		result.Conversations = append(result.Conversations, conversationResponse{
			Conversation: conv,
			Participants: participants,
		})
	}

	writeJSON(w, http.StatusOK, result)
}

// handleConversationRoutes handles requests under /api/v1/conversations/.
// Routes:
//   - GET /api/v1/conversations/{id}                 — Get a single conversation
//   - GET /api/v1/conversations/{id}/messages         — List messages in a conversation
//   - PUT /api/v1/conversations/{id}/default-agent    — Set the default agent
func (s *Server) handleConversationRoutes(w http.ResponseWriter, r *http.Request) {
	id, action := extractAction(r, "/api/v1/conversations")

	if id == "" {
		// POST /api/v1/conversations — handled by handleCreateConversation via mux
		if r.Method == http.MethodPost {
			s.handleCreateConversation(w, r)
			return
		}
		MethodNotAllowed(w, http.MethodGet, http.MethodPost)
		return
	}

	switch action {
	case "":
		s.handleGetConversation(w, r, id)
	case "messages":
		s.handleConvListMessages(w, r, id)
	case "default-agent":
		s.handleSetDefaultAgent(w, r, id)
	default:
		NotFound(w, "Conversation action")
	}
}

// handleGetConversation handles GET /api/v1/conversations/{id}.
func (s *Server) handleGetConversation(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}

	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Forbidden(w)
		return
	}

	conv, err := s.store.GetConversation(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "Conversation")
		return
	}

	// Authorization: caller must be a participant.
	isParticipant, err := isConversationParticipant(ctx, s.store, id, identity.Type(), identity.ID())
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if !isParticipant {
		Forbidden(w)
		return
	}

	participants, err := s.store.ListParticipants(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, conversationResponse{
		Conversation: *conv,
		Participants: participants,
	})
}

// handleConvListMessages handles GET /api/v1/conversations/{id}/messages.
func (s *Server) handleConvListMessages(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}

	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Forbidden(w)
		return
	}

	// Authorization: caller must be a participant.
	isParticipant, err := isConversationParticipant(ctx, s.store, id, identity.Type(), identity.ID())
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if !isParticipant {
		Forbidden(w)
		return
	}

	q := r.URL.Query()
	filter := store.MessageFilter{
		ConversationID: id,
	}

	if before := q.Get("before"); before != "" {
		if t, parseErr := time.Parse(time.RFC3339, before); parseErr == nil {
			filter.Before = t
		}
	}
	if after := q.Get("after"); after != "" {
		if t, parseErr := time.Parse(time.RFC3339, after); parseErr == nil {
			filter.After = t
		}
	}

	opts := store.ListOptions{}
	if limitStr := q.Get("limit"); limitStr != "" {
		if n, parseErr := strconv.Atoi(limitStr); parseErr == nil && n > 0 {
			opts.Limit = n
		}
	}
	if cursor := q.Get("cursor"); cursor != "" {
		opts.Cursor = cursor
	}

	result, err := s.store.ListMessages(ctx, filter, opts)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, result)
}

// handleCreateConversation handles POST /api/v1/conversations.
func (s *Server) handleCreateConversation(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}

	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Forbidden(w)
		return
	}

	var req createConversationRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body")
		return
	}

	if req.DisplayName == "" {
		BadRequest(w, "displayName is required")
		return
	}

	kind := req.Kind
	if kind == "" {
		kind = "group"
	}
	if kind != "group" && kind != "direct" {
		BadRequest(w, "kind must be 'group' or 'direct'")
		return
	}

	now := time.Now().UTC()
	conv := &store.Conversation{
		ID:             api.NewUUID(),
		Kind:           kind,
		Surface:        "native",
		DisplayName:    req.DisplayName,
		DriftState:     "active",
		LastActivityAt: now,
		CreatedAt:      now,
	}

	if req.ProjectID != "" {
		conv.ProjectID = &req.ProjectID
	}

	if err := s.store.CreateConversation(ctx, conv); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Auto-add the caller as a participant.
	participant := &store.ConversationParticipant{
		ID:             api.NewUUID(),
		ConversationID: conv.ID,
		PrincipalKind:  identity.Type(),
		PrincipalID:    identity.ID(),
		Role:           "member",
		JoinedAt:       now,
	}

	if err := s.store.AddParticipant(ctx, participant); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusCreated, conversationResponse{
		Conversation: *conv,
		Participants: []store.ConversationParticipant{*participant},
	})
}

// handleSetDefaultAgent handles PUT /api/v1/conversations/{id}/default-agent.
func (s *Server) handleSetDefaultAgent(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPut {
		MethodNotAllowed(w, http.MethodPut)
		return
	}

	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Forbidden(w)
		return
	}

	// Authorization: caller must be a participant.
	isParticipant, err := isConversationParticipant(ctx, s.store, id, identity.Type(), identity.ID())
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if !isParticipant {
		Forbidden(w)
		return
	}

	var req setDefaultAgentRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body")
		return
	}

	if req.AgentID == "" {
		BadRequest(w, "agentId is required")
		return
	}

	conv, err := s.store.GetConversation(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "Conversation")
		return
	}

	conv.DefaultAgentID = &req.AgentID
	if err := s.store.UpdateConversation(ctx, conv); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, conv)
}

// isConversationParticipant checks whether a principal is an active participant
// of a conversation. This is the shared authorization helper used across all
// conversation endpoints that require participant access.
func isConversationParticipant(ctx context.Context, st store.Store, conversationID, principalKind, principalID string) (bool, error) {
	participants, err := st.ListParticipants(ctx, conversationID)
	if err != nil {
		return false, err
	}
	for _, p := range participants {
		if p.PrincipalKind == principalKind && p.PrincipalID == principalID {
			return true, nil
		}
	}
	return false, nil
}

