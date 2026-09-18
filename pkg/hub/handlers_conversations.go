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
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
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

// handleListConversations handles GET /api/v1/conversations (list) and
// POST /api/v1/conversations (create). Go's http.ServeMux matches the
// exact path (no trailing slash) to this handler, so POST must be
// dispatched here to be reachable.
func (s *Server) handleListConversations(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		// fall through to list logic below
	case http.MethodPost:
		s.handleCreateConversation(w, r)
		return
	default:
		MethodNotAllowed(w, http.MethodGet, http.MethodPost)
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
		n, parseErr := strconv.Atoi(limitStr)
		if parseErr != nil || n < 1 {
			BadRequest(w, "invalid 'limit' parameter: must be a positive integer")
			return
		}
		limit = n
	}

	// Apply client-side filtering.
	// For direct conversations, verify canonical DM key authorization to ensure
	// stale or forged participant rows cannot grant list visibility.
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
		// For direct conversations, verify the caller is named in the canonical
		// DM key. A stale participant row that does not match the key must not
		// grant listing visibility.
		if conv.Kind == "direct" && !isCanonicalDMParticipant(conv.ExternalRef, principalKind, principalID) {
			continue
		}
		filtered = append(filtered, conv)
	}

	// Apply limit.
	if limit > 0 && len(filtered) > limit {
		filtered = filtered[:limit]
	}

	// Build response WITHOUT participants (available via GET /conversations/{id}).
	// This avoids an N+1 query — one ListParticipants call per conversation.
	result := conversationListResponse{
		Conversations: make([]conversationResponse, 0, len(filtered)),
	}
	for _, conv := range filtered {
		result.Conversations = append(result.Conversations, conversationResponse{
			Conversation: conv,
		})
	}

	writeJSON(w, http.StatusOK, result)
}

// handleConversationRoutes handles requests under /api/v1/conversations/.
// Routes:
//   - GET /api/v1/conversations/{id}                       — Get a single conversation
//   - GET /api/v1/conversations/{id}/messages              — List messages in a conversation
//   - GET /api/v1/conversations/{id}/messages/{messageID}  — Get a single message
//   - PUT /api/v1/conversations/{id}/default-agent          — Set the default agent
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

	if messageID, ok := strings.CutPrefix(action, "messages/"); ok {
		if messageID == "" || strings.Contains(messageID, "/") {
			NotFound(w, "Message")
			return
		}
		s.handleGetConversationMessage(w, r, id, messageID)
		return
	}

	switch action {
	case "":
		s.handleGetConversation(w, r, id)
	case "messages":
		if r.Method == http.MethodPost {
			s.handleCPMConversationSend(w, r, id)
			return
		}
		s.handleConvListMessages(w, r, id)
	case "default-agent":
		s.handleSetDefaultAgent(w, r, id)
	case "participants":
		s.handleAddParticipant(w, r, id)
	case "leave":
		s.handleLeaveConversation(w, r, id)
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

	// Authorization: for direct conversations, use canonical DM key (kind+ID).
	// This prevents a third principal from reading a DM by adding a participant
	// row or knowing its UUID. A matching ID with the wrong principal kind is
	// denied. For group conversations, use participant rows.
	if conv.Kind == "direct" {
		if !authorizeDMRead(conv, identity.Type(), identity.ID()) {
			Forbidden(w)
			return
		}
	}

	// Fetch participants — used for authorization (groups) and response.
	participants, err := s.store.ListParticipants(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// For non-direct conversations, authorize via participant rows.
	if conv.Kind != "direct" {
		isParticipant := false
		for _, p := range participants {
			if p.PrincipalKind == identity.Type() && p.PrincipalID == identity.ID() {
				isParticipant = true
				break
			}
		}
		if !isParticipant {
			Forbidden(w)
			return
		}
	}

	// For direct conversations, filter participants to only include canonical
	// members (those named in the DM key). This prevents extraneous or legacy
	// participant rows from leaking into the API response.
	if conv.Kind == "direct" {
		var canonical []store.ConversationParticipant
		for _, p := range participants {
			if isCanonicalDMParticipant(conv.ExternalRef, p.PrincipalKind, p.PrincipalID) {
				canonical = append(canonical, p)
			}
		}
		participants = canonical
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

	// Authorization: for direct conversations, use canonical DM key (kind+ID).
	// For group conversations, use participant rows.
	conv, err := s.store.GetConversation(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "Conversation")
		return
	}

	if conv.Kind == "direct" {
		if !authorizeDMRead(conv, identity.Type(), identity.ID()) {
			Forbidden(w)
			return
		}
	} else {
		isParticipant, partErr := isConversationParticipant(ctx, s.store, id, identity.Type(), identity.ID())
		if partErr != nil {
			writeErrorFromErr(w, partErr, "")
			return
		}
		if !isParticipant {
			Forbidden(w)
			return
		}
	}

	q := r.URL.Query()
	filter := store.MessageFilter{
		ConversationID: id,
	}

	if before := q.Get("before"); before != "" {
		t, parseErr := time.Parse(time.RFC3339, before)
		if parseErr != nil {
			BadRequest(w, "invalid 'before' parameter: must be RFC3339 format")
			return
		}
		filter.Before = t
	}
	if after := q.Get("after"); after != "" {
		t, parseErr := time.Parse(time.RFC3339, after)
		if parseErr != nil {
			BadRequest(w, "invalid 'after' parameter: must be RFC3339 format")
			return
		}
		filter.After = t
	}

	opts := store.ListOptions{}
	if limitStr := q.Get("limit"); limitStr != "" {
		n, parseErr := strconv.Atoi(limitStr)
		if parseErr != nil || n < 1 {
			BadRequest(w, "invalid 'limit' parameter: must be a positive integer")
			return
		}
		opts.Limit = n
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

// handleGetConversationMessage handles GET /api/v1/conversations/{id}/messages/{messageID}.
func (s *Server) handleGetConversationMessage(w http.ResponseWriter, r *http.Request, conversationID, messageID string) {
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

	isParticipant, err := isConversationParticipant(ctx, s.store, conversationID, identity.Type(), identity.ID())
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if !isParticipant {
		Forbidden(w)
		return
	}

	msg, err := s.store.GetMessage(ctx, messageID)
	if err != nil {
		writeErrorFromErr(w, err, "Message")
		return
	}
	if msg.ConversationID != conversationID {
		NotFound(w, "Message")
		return
	}

	writeJSON(w, http.StatusOK, msg)
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

	// Direct conversations must only be created through the canonical
	// principal-pair minting path (ResolveOrCreateDMConversation). Generic
	// create remains the group creation surface. This prevents bypassing
	// the immutable two-party identity constraint of DMs.
	if kind == "direct" {
		BadRequest(w, "direct conversations cannot be created through this endpoint; use the messaging API to send a direct message")
		return
	}
	if kind != "group" {
		BadRequest(w, "kind must be 'group'")
		return
	}

	// Verify the project exists when a project ID is provided.
	if req.ProjectID != "" {
		_, err := s.store.GetProject(ctx, req.ProjectID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeError(w, http.StatusNotFound, "not_found", "project not found", nil)
				return
			}
			writeErrorFromErr(w, err, "")
			return
		}

		// Authorize: caller must have read access to the project.
		if !s.authorize(w, r, Resource{Type: "project", ID: req.ProjectID}, ActionRead) {
			return
		}
	}

	// Default to the caller's project when no project ID is specified.
	if req.ProjectID == "" {
		if ai, ok := identity.(AgentIdentity); ok {
			if callerProject := ai.ProjectID(); callerProject != "" {
				req.ProjectID = callerProject
			}
		}
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

	// Fetch the conversation first: DM-specific rejection must run before
	// the participant-row auth check so that DMs without participant rows
	// return 400 (bad request) instead of 403 (forbidden).
	conv, err := s.store.GetConversation(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "Conversation")
		return
	}

	// DM conversations have exactly two immutable principals — setting a
	// default agent is a group-conversation operation and is not meaningful
	// for direct conversations.
	if conv.Kind == "direct" {
		BadRequest(w, "cannot set default agent on a direct conversation")
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

	// Verify the agent exists before setting it as default.
	agent, err := s.store.GetAgent(ctx, req.AgentID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "agent not found", nil)
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// Verify the agent belongs to the same project as the conversation.
	if conv.ProjectID != nil && agent.ProjectID != *conv.ProjectID {
		BadRequest(w, "agent does not belong to the conversation's project")
		return
	}

	// Authorize: caller must have read access to the agent's project.
	if agent.ProjectID != "" {
		if !s.authorize(w, r, Resource{Type: "project", ID: agent.ProjectID}, ActionRead) {
			return
		}
	}

	conv.DefaultAgentID = &req.AgentID
	if err := s.store.UpdateConversation(ctx, conv); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, conversationResponse{
		Conversation: *conv,
	})
}

// addParticipantRequest is the request body for adding a participant to a conversation.
type addParticipantRequest struct {
	PrincipalKind string `json:"principalKind"`
	PrincipalID   string `json:"principalId"`
}

// handleAddParticipant handles POST /api/v1/conversations/{id}/participants.
func (s *Server) handleAddParticipant(w http.ResponseWriter, r *http.Request, id string) {
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

	var req addParticipantRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body")
		return
	}

	if req.PrincipalKind == "" || req.PrincipalID == "" {
		BadRequest(w, "principalKind and principalId are required")
		return
	}

	if req.PrincipalKind != "user" && req.PrincipalKind != "agent" {
		BadRequest(w, "principalKind must be 'user' or 'agent'")
		return
	}

	// Reject participant addition for direct conversations. DM membership is
	// immutable: it is derived from the canonical two-principal key. Adding a
	// third principal would not grant them read access (key-based auth denies
	// it), but the rejection must be explicit.
	conv, err := s.store.GetConversation(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "Conversation")
		return
	}
	if conv.Kind == "direct" {
		BadRequest(w, "cannot add participants to a direct conversation")
		return
	}

	// For agent principals, verify the agent exists and belongs to the
	// conversation's project. Without this check, a cross-project agent
	// could be added as a participant and read messages via ListMessages
	// (which uses participant-based auth only, no project check).
	if req.PrincipalKind == "agent" {
		agent, agentErr := s.store.GetAgent(ctx, req.PrincipalID)
		if agentErr != nil {
			if errors.Is(agentErr, store.ErrNotFound) {
				writeError(w, http.StatusNotFound, "not_found", "agent not found", nil)
				return
			}
			writeErrorFromErr(w, agentErr, "")
			return
		}

		if conv.ProjectID != nil && agent.ProjectID != *conv.ProjectID {
			BadRequest(w, "agent does not belong to the conversation's project")
			return
		}
	}

	now := time.Now().UTC()
	participant := &store.ConversationParticipant{
		ID:             api.NewUUID(),
		ConversationID: id,
		PrincipalKind:  req.PrincipalKind,
		PrincipalID:    req.PrincipalID,
		Role:           "member",
		JoinedAt:       now,
	}

	if err := s.store.AddParticipant(ctx, participant); err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			writeError(w, http.StatusConflict, "already_exists", "participant already exists", nil)
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusCreated, participant)
}

// handleLeaveConversation handles POST /api/v1/conversations/{id}/leave.
func (s *Server) handleLeaveConversation(w http.ResponseWriter, r *http.Request, id string) {
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

	if err := s.store.RemoveParticipant(ctx, id, identity.Type(), identity.ID()); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "participant not found", nil)
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	w.WriteHeader(http.StatusNoContent)
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

// authorizeDMRead checks whether a principal may read a conversation.
//
// For direct conversations, authorization is derived from the canonical DM key
// (principal kind AND ID), not from the participants table. This is strictly
// tighter than a participant-table scan: a third principal cannot read a DM by
// adding a participant row or knowing its UUID. A matching ID with the wrong
// principal kind is denied.
//
// For group conversations, authorization uses participant rows (existing
// behavior). Group conversations have project-level authorization enforced
// elsewhere; participant presence authorizes listing/reading.
func authorizeDMRead(conv *store.Conversation, principalKind, principalID string) bool {
	if conv.Kind == "direct" {
		return isCanonicalDMParticipant(conv.ExternalRef, principalKind, principalID)
	}
	// For non-direct conversations, caller must check participant rows separately.
	// Return true here to fall through to the existing participant check.
	return true
}

// isCanonicalDMParticipant checks whether a (kind, id) pair is named in a
// direct conversation's canonical DM key. This is the single source of truth
// for DM access: participant rows are listing preferences only.
func isCanonicalDMParticipant(externalRef, principalKind, principalID string) bool {
	kindA, idA, kindB, idB, err := messages.ParseDMKey(externalRef)
	if err != nil {
		// Fail closed: unparseable key (old format, empty, corrupt) → deny.
		return false
	}
	return (principalKind == kindA && principalID == idA) ||
		(principalKind == kindB && principalID == idB)
}
