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
	"log/slog"
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
	// For agent callers, also apply Hub-off cross-project guard (silently
	// exclude rather than 403 mid-list, per design §7).
	agentIdent := GetAgentIdentityFromContext(ctx)
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
		// Hub-off guard: agent callers must not see cross-project DMs when
		// the feature is disabled. Silently filter rather than 403 mid-list.
		if agentIdent != nil && !s.isCrossProjectReadAllowed(ctx, &conv, agentIdent) {
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
		// Hub-off guard: deny agent callers from reading cross-project DMs
		// when the feature is disabled (design §7).
		if !s.enforceCrossProjectReadGate(w, r, conv) {
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
		// Hub-off guard: deny agent callers from reading cross-project DMs
		// when the feature is disabled (design §7).
		if !s.enforceCrossProjectReadGate(w, r, conv) {
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

	// Authorization: for direct conversations, use canonical DM key (kind+ID).
	// For group conversations, use participant rows.
	conv, err := s.store.GetConversation(ctx, conversationID)
	if err != nil {
		writeErrorFromErr(w, err, "Conversation")
		return
	}

	if conv.Kind == "direct" {
		if !authorizeDMRead(conv, identity.Type(), identity.ID()) {
			Forbidden(w)
			return
		}
		// Hub-off guard: deny agent callers from reading cross-project DMs
		// when the feature is disabled (design §7).
		if !s.enforceCrossProjectReadGate(w, r, conv) {
			return
		}
	} else {
		isParticipant, partErr := isConversationParticipant(ctx, s.store, conversationID, identity.Type(), identity.ID())
		if partErr != nil {
			writeErrorFromErr(w, partErr, "")
			return
		}
		if !isParticipant {
			Forbidden(w)
			return
		}
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

	// Q2 (design doc chat-thread-bridge §7, ptone decided (a)): a group
	// conversation must always be project-scoped; only DMs are global. Check
	// this before any write (AC-10: no conversation/topic row on this path) —
	// an agent with no projectId already fell back to its token project
	// above, so only a user with neither a body projectId nor a token
	// project reaches this.
	if req.ProjectID == "" {
		BadRequest(w, "projectId is required")
		return
	}

	// kind == "group" from here (direct was rejected above). Route creation
	// through the same atomic topic-creation path every other native group
	// mint site uses (design doc: chat-thread-bridge §3), instead of a bare
	// store.CreateConversation that never mints a topic or an external_ref.
	conv, apiErr := s.createGroupConversation(ctx, "native", createGroupParams{
		ProjectID:   req.ProjectID,
		DisplayName: req.DisplayName,
		CreatedBy:   identity.ID(),
	})
	if apiErr != nil {
		apiErr.write(w)
		return
	}

	// Auto-add the caller as a participant. This is a listing-index entry
	// only (design doc §2.4.2.1 / §3.5) — project membership remains the
	// access authority for group conversations. The ConversationParticipant
	// principal_kind enum only allows "user"/"agent" (ent schema); the dev
	// pseudo-identity (Identity.Type()=="dev") is neither, so skip the insert
	// rather than fail the create after the topic already exists.
	participants := []store.ConversationParticipant{}
	if identity.Type() != "dev" {
		participant := &store.ConversationParticipant{
			ID:             api.NewUUID(),
			ConversationID: conv.ID,
			PrincipalKind:  identity.Type(),
			PrincipalID:    identity.ID(),
			Role:           "member",
			JoinedAt:       time.Now().UTC(),
		}

		if err := s.store.AddParticipant(ctx, participant); err != nil {
			// The topic and its linked conversation already exist and have
			// already been announced (PublishChatTopicEvent, inside
			// createGroupConversation). Failing the request here would give
			// the client a 500 for a resource that in fact exists, and a
			// retry with the same name would now 409 NAME_CONFLICT with no
			// way to recover the participant row. Log and proceed: the
			// web-visible topic is the authoritative artifact (participants
			// are a listing index, not the access authority — §2.4.2.1), and
			// the response's empty participants array tells the caller the
			// insert didn't happen.
			slog.ErrorContext(ctx, "handleCreateConversation: AddParticipant failed after topic commit",
				"conversationID", conv.ID, "externalRef", conv.ExternalRef, "error", err)
		} else {
			participants = append(participants, *participant)
		}
	}

	writeJSON(w, http.StatusCreated, conversationResponse{
		Conversation: *conv,
		Participants: participants,
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

// isCrossProjectReadAllowed is the non-response-writing predicate form
// of enforceCrossProjectReadGate. It returns true if the conversation
// should be visible to the given agent identity, false if it should be
// silently filtered out (e.g. from list results). Human callers are
// never passed to this function — the caller must check.
func (s *Server) isCrossProjectReadAllowed(ctx context.Context, conv *store.Conversation, agentIdent AgentIdentity) bool {
	if conv.Kind != "direct" {
		return true
	}

	kindA, idA, kindB, idB, err := messages.ParseDMKey(conv.ExternalRef)
	if err != nil {
		// Unparseable key — fail closed: exclude from list.
		return false
	}

	// Find the peer.
	var peerKind, peerID string
	callerID := agentIdent.ID()
	switch {
	case kindA == "agent" && idA == callerID:
		peerKind, peerID = kindB, idB
	case kindB == "agent" && idB == callerID:
		peerKind, peerID = kindA, idA
	default:
		// Caller not in key — prior isCanonicalDMParticipant check handles this.
		return true
	}

	if peerKind != "agent" {
		return true // human-agent DM — always visible.
	}

	peerAgent, err := s.store.GetAgent(ctx, peerID)
	if err != nil {
		slog.Error("isCrossProjectReadAllowed: database error looking up peer agent", "peer_id", peerID, "error", err)
		return false
	}
	if peerAgent == nil {
		return false
	}

	if agentIdent.ProjectID() == peerAgent.ProjectID {
		return true // Same project — always visible.
	}

	// Cross-project: check Hub gate.
	return s.crossProjectMessagingEnabled()
}

// enforceCrossProjectReadGate denies agent callers from reading
// conversations that cross project boundaries when the Hub-level
// cross-project messaging switch is disabled. Returns true if access
// is allowed; writes a 403 and returns false if denied.
//
// Human callers are always allowed (authorized audit survives Hub
// disable per design §7). Same-project conversations and human-agent
// DMs are always allowed. The peer is derived from the canonical DM
// key (ExternalRef), not from mutable participant rows.
func (s *Server) enforceCrossProjectReadGate(w http.ResponseWriter, r *http.Request, conv *store.Conversation) bool {
	agentIdent := GetAgentIdentityFromContext(r.Context())
	if agentIdent == nil {
		// Human callers always pass — authorized audit survives Hub disable.
		return true
	}

	if conv.Kind != "direct" {
		return true
	}

	kindA, idA, kindB, idB, err := messages.ParseDMKey(conv.ExternalRef)
	if err != nil {
		// Unparseable key — fail closed.
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"cross-project read denied: unparseable conversation key", nil)
		return false
	}

	// Find the peer: the slot whose (kind, id) does not match the caller.
	var peerKind, peerID string
	callerID := agentIdent.ID()
	switch {
	case kindA == "agent" && idA == callerID:
		peerKind, peerID = kindB, idB
	case kindB == "agent" && idB == callerID:
		peerKind, peerID = kindA, idA
	default:
		// Caller is not named in the key — authorizeDMRead should have
		// caught this already. Allow through; the prior check is
		// authoritative.
		return true
	}

	// If the peer is not an agent, it's a human-agent DM — always allowed.
	if peerKind != "agent" {
		return true
	}

	// Look up the peer agent to compare project IDs.
	peerAgent, err := s.store.GetAgent(r.Context(), peerID)
	if err != nil {
		slog.Error("enforceCrossProjectReadGate: database error looking up peer agent", "peer_id", peerID, "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"Internal server error verifying peer agent", nil)
		return false
	}
	if peerAgent == nil {
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"cross-project read denied: peer agent not found", nil)
		return false
	}

	// Same-project DMs are always allowed regardless of Hub setting.
	if agentIdent.ProjectID() == peerAgent.ProjectID {
		return true
	}

	// Cross-project: check the Hub gate.
	if !s.crossProjectMessagingEnabled() {
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"cross-project messaging is disabled", nil)
		return false
	}

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
