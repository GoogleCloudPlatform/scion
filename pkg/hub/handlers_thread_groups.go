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
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// Project-scoped chat thread groups: /api/v1/thread-groups
// (ptone/scion#4424, design sections 2 and 3).
//
// A thread group is metadata over a project's group conversations, so it
// uses exactly the checks a project group conversation uses, with no new
// authorization model:
//
//   - read (list, get): requireInboxCredential, the strict agent
//     same-project rule, and project:read (s.authorize ActionRead) on the
//     group's project. A user access token needs nothing more for a
//     project-scoped read (authorizeTokenConversationRead).
//   - mutation (create, rename, add or remove a thread, delete): the read
//     checks, plus inbox:write for the project for a user access token
//     (authorizeSelfScoped, as handleCreateConversation does). Delete is for
//     users only; an agent gets 403, mirroring handleTopicDelete.

// threadGroupListResponse is the body of GET /api/v1/thread-groups.
type threadGroupListResponse struct {
	ThreadGroups []ThreadGroupResponse `json:"threadGroups"`
}

// createThreadGroupRequest is the body of POST /api/v1/thread-groups.
type createThreadGroupRequest struct {
	ProjectID string `json:"projectId"`
	Name      string `json:"name"`
}

// renameThreadGroupRequest is the body of PATCH /api/v1/thread-groups/{id}.
type renameThreadGroupRequest struct {
	Name string `json:"name"`
}

// addThreadGroupThreadRequest is the body of
// POST /api/v1/thread-groups/{id}/threads. Exactly one field is set:
// a conversation ID, or a webchat topic (thread) ID that is resolved within
// the group's project only.
type addThreadGroupThreadRequest struct {
	ConversationID string `json:"conversationId"`
	ThreadID       string `json:"threadId"`
}

// handleThreadGroups serves /api/v1/thread-groups (list and create).
func (s *Server) handleThreadGroups(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleListThreadGroups(w, r)
	case http.MethodPost:
		s.handleCreateThreadGroup(w, r)
	default:
		MethodNotAllowed(w, http.MethodGet, http.MethodPost)
	}
}

// handleThreadGroupRoutes serves /api/v1/thread-groups/{id},
// /api/v1/thread-groups/{id}/threads and
// /api/v1/thread-groups/{id}/threads/{conversationId}.
func (s *Server) handleThreadGroupRoutes(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/thread-groups/")
	parts := strings.Split(rest, "/")
	if parts[0] == "" {
		NotFound(w, "Thread group")
		return
	}
	id := parts[0]
	switch {
	case len(parts) == 1:
		switch r.Method {
		case http.MethodGet:
			s.handleGetThreadGroup(w, r, id)
		case http.MethodPatch:
			s.handleRenameThreadGroup(w, r, id)
		case http.MethodDelete:
			s.handleDeleteThreadGroup(w, r, id)
		default:
			MethodNotAllowed(w, http.MethodGet, http.MethodPatch, http.MethodDelete)
		}
	case len(parts) == 2 && parts[1] == "threads":
		if r.Method != http.MethodPost {
			MethodNotAllowed(w, http.MethodPost)
			return
		}
		s.handleAddThreadGroupThread(w, r, id)
	case len(parts) == 3 && parts[1] == "threads" && parts[2] != "":
		if r.Method != http.MethodDelete {
			MethodNotAllowed(w, http.MethodDelete)
			return
		}
		s.handleRemoveThreadGroupThread(w, r, id, parts[2])
	default:
		NotFound(w, "route")
	}
}

// writeThreadGroupStoreError is the one place a thread group store error
// becomes an HTTP response: ErrNotFound is 404 for the named resource,
// ErrAlreadyExists is 409 (a name conflict), ErrInvalidInput is 400 with
// the store's message, and anything else goes through writeErrorFromErr.
func writeThreadGroupStoreError(w http.ResponseWriter, err error, resource string) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		NotFound(w, resource)
	case errors.Is(err, store.ErrAlreadyExists):
		Conflict(w, "a thread group with this name already exists in the project")
	case errors.Is(err, store.ErrInvalidInput):
		ValidationError(w, strings.TrimSuffix(err.Error(), ": "+store.ErrInvalidInput.Error()), nil)
	default:
		writeErrorFromErr(w, err, "")
	}
}

// threadGroupsPerProjectError is the 422 for a project at
// store.MaxThreadGroupsPerProject groups.
func threadGroupsPerProjectError() *apiError {
	return &apiError{status: http.StatusUnprocessableEntity, code: ErrCodeQuotaExceeded,
		message: fmt.Sprintf("a project can have at most %d thread groups", store.MaxThreadGroupsPerProject)}
}

// threadGroupFullError is the 422 for a thread group at
// store.MaxThreadsPerThreadGroup threads.
func threadGroupFullError() *apiError {
	return &apiError{status: http.StatusUnprocessableEntity, code: ErrCodeQuotaExceeded,
		message: fmt.Sprintf("a thread group can hold at most %d threads", store.MaxThreadsPerThreadGroup)}
}

// threadGroupCreatorKind returns the created_by_kind for identity.
func threadGroupCreatorKind(identity Identity) string {
	if _, ok := identity.(AgentIdentity); ok {
		return store.ThreadGroupCreatorAgent
	}
	return store.ThreadGroupCreatorUser
}

// authorizeThreadGroupProject applies the thread group checks for
// projectID: the strict agent same-project rule, project:read on the
// project, and, when mutate is set and the credential is a user access
// token, inbox:write for the project. A missing project is 403; any other
// store error passes through. It writes the response and returns false on
// deny. The caller has already run requireInboxCredential.
func (s *Server) authorizeThreadGroupProject(w http.ResponseWriter, r *http.Request, cls inboxCredential, projectID string, mutate bool) bool {
	ctx := r.Context()
	if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
		if agentIdent.ProjectID() != projectID {
			Forbidden(w)
			return false
		}
	}
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			Forbidden(w)
			return false
		}
		writeErrorFromErr(w, err, "")
		return false
	}
	if !s.authorize(w, r, projectResource(project), ActionRead) {
		return false
	}
	if mutate && cls == inboxCredentialToken && !s.authorizeSelfScoped(w, r, permInboxWrite, projectID) {
		return false
	}
	return true
}

// loadAuthorizedThreadGroup reads thread group id and authorizes the caller
// on its project. It writes the response and returns nil on any failure.
func (s *Server) loadAuthorizedThreadGroup(w http.ResponseWriter, r *http.Request, cls inboxCredential, id string, mutate bool) *store.ThreadGroup {
	g, err := s.store.GetThreadGroup(r.Context(), id)
	if err != nil {
		writeThreadGroupStoreError(w, err, "Thread group")
		return nil
	}
	if !s.authorizeThreadGroupProject(w, r, cls, g.ProjectID, mutate) {
		return nil
	}
	return g
}

// writeThreadGroup re-reads thread group id, writes it with status and
// returns it, or writes the error and returns nil.
func (s *Server) writeThreadGroup(w http.ResponseWriter, r *http.Request, id string, status int) *ThreadGroupResponse {
	g, err := s.store.GetThreadGroup(r.Context(), id)
	if err != nil {
		writeThreadGroupStoreError(w, err, "Thread group")
		return nil
	}
	resp, err := s.threadGroupResponse(r.Context(), g)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return nil
	}
	writeJSON(w, status, resp)
	return &resp
}

// publishThreadGroup re-reads thread group id and publishes it with action.
// A read failure is logged; the mutation it reports has already committed.
func (s *Server) publishThreadGroup(ctx context.Context, id, action string) {
	g, err := s.store.GetThreadGroup(ctx, id)
	if err != nil {
		slog.WarnContext(ctx, "thread groups: read for event failed", "threadGroupID", id, "action", action, "error", err)
		return
	}
	resp, err := s.threadGroupResponse(ctx, g)
	if err != nil {
		slog.WarnContext(ctx, "thread groups: build event failed", "threadGroupID", id, "action", action, "error", err)
		return
	}
	s.events.PublishChatThreadGroupEvent(ctx, g.ProjectID, action, resp)
}

// handleListThreadGroups handles GET /api/v1/thread-groups?projectId=P.
// projectId defaults to an agent caller's project and is required for
// everyone else.
func (s *Server) handleListThreadGroups(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cls, _, ok := requireInboxCredential(w, r)
	if !ok {
		return
	}
	projectID := r.URL.Query().Get("projectId")
	if projectID == "" {
		if ai := GetAgentIdentityFromContext(ctx); ai != nil {
			projectID = ai.ProjectID()
		}
	}
	if projectID == "" {
		BadRequest(w, "projectId is required")
		return
	}
	if !s.authorizeThreadGroupProject(w, r, cls, projectID, false) {
		return
	}
	groups, err := s.store.ListThreadGroups(ctx, projectID)
	if err != nil {
		writeThreadGroupStoreError(w, err, "Project")
		return
	}
	resp, err := s.threadGroupResponses(ctx, projectID, groups)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	writeJSON(w, http.StatusOK, threadGroupListResponse{ThreadGroups: resp})
}

// handleCreateThreadGroup handles POST /api/v1/thread-groups.
func (s *Server) handleCreateThreadGroup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Forbidden(w)
		return
	}
	cls, _, ok := requireInboxCredential(w, r)
	if !ok {
		return
	}
	var req createThreadGroupRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body")
		return
	}
	// projectId defaults to an agent caller's project, as in
	// handleCreateConversation.
	if req.ProjectID == "" {
		if ai, ok := identity.(AgentIdentity); ok {
			req.ProjectID = ai.ProjectID()
		}
	}
	if req.ProjectID == "" {
		BadRequest(w, "projectId is required")
		return
	}
	if !s.authorizeThreadGroupProject(w, r, cls, req.ProjectID, true) {
		return
	}
	if _, err := store.ValidateThreadGroupName(req.Name); err != nil {
		writeThreadGroupStoreError(w, err, "Thread group")
		return
	}
	// The limit is soft: a best-effort check before the write, with no
	// transactional count, so concurrent creates can pass it together.
	count, err := s.store.CountThreadGroups(ctx, req.ProjectID)
	if err != nil {
		writeThreadGroupStoreError(w, err, "Project")
		return
	}
	if count >= store.MaxThreadGroupsPerProject {
		threadGroupsPerProjectError().write(w)
		return
	}
	g := &store.ThreadGroup{
		ProjectID:     req.ProjectID,
		Name:          req.Name,
		CreatedBy:     identity.ID(),
		CreatedByKind: threadGroupCreatorKind(identity),
	}
	if err := s.store.CreateThreadGroup(ctx, g); err != nil {
		writeThreadGroupStoreError(w, err, "Thread group")
		return
	}
	resp, err := s.threadGroupResponse(ctx, g)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	s.events.PublishChatThreadGroupEvent(ctx, g.ProjectID, ThreadGroupEventCreated, resp)
	writeJSON(w, http.StatusCreated, resp)
}

// handleGetThreadGroup handles GET /api/v1/thread-groups/{id}.
func (s *Server) handleGetThreadGroup(w http.ResponseWriter, r *http.Request, id string) {
	cls, _, ok := requireInboxCredential(w, r)
	if !ok {
		return
	}
	g := s.loadAuthorizedThreadGroup(w, r, cls, id, false)
	if g == nil {
		return
	}
	resp, err := s.threadGroupResponse(r.Context(), g)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleRenameThreadGroup handles PATCH /api/v1/thread-groups/{id}.
func (s *Server) handleRenameThreadGroup(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()
	cls, _, ok := requireInboxCredential(w, r)
	if !ok {
		return
	}
	g := s.loadAuthorizedThreadGroup(w, r, cls, id, true)
	if g == nil {
		return
	}
	var req renameThreadGroupRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body")
		return
	}
	if err := s.store.RenameThreadGroup(ctx, g.ID, req.Name); err != nil {
		writeThreadGroupStoreError(w, err, "Thread group")
		return
	}
	if resp := s.writeThreadGroup(w, r, g.ID, http.StatusOK); resp != nil {
		s.events.PublishChatThreadGroupEvent(ctx, resp.ProjectID, ThreadGroupEventUpdated, *resp)
	}
}

// handleDeleteThreadGroup handles DELETE /api/v1/thread-groups/{id}. Only a
// user may delete a group (design section 3, Q3); the member threads become
// ungrouped and are not deleted.
func (s *Server) handleDeleteThreadGroup(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()
	cls, _, ok := requireInboxCredential(w, r)
	if !ok {
		return
	}
	if GetUserIdentityFromContext(ctx) == nil {
		Forbidden(w)
		return
	}
	g := s.loadAuthorizedThreadGroup(w, r, cls, id, true)
	if g == nil {
		return
	}
	snapshot, err := s.threadGroupResponse(ctx, g)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if err := s.store.DeleteThreadGroup(ctx, g.ID); err != nil {
		writeThreadGroupStoreError(w, err, "Thread group")
		return
	}
	s.events.PublishChatThreadGroupEvent(ctx, g.ProjectID, ThreadGroupEventDeleted, snapshot)
	w.WriteHeader(http.StatusNoContent)
}

// handleAddThreadGroupThread handles POST /api/v1/thread-groups/{id}/threads.
// It is idempotent, and moves the thread out of any other group of the
// project (design section 2, Q4).
func (s *Server) handleAddThreadGroupThread(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Forbidden(w)
		return
	}
	cls, _, ok := requireInboxCredential(w, r)
	if !ok {
		return
	}
	g := s.loadAuthorizedThreadGroup(w, r, cls, id, true)
	if g == nil {
		return
	}
	var req addThreadGroupThreadRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body")
		return
	}
	convID, apiErr := s.resolveThreadGroupMember(ctx, identity, g.ProjectID, req)
	if apiErr != nil {
		apiErr.write(w)
		return
	}

	// Find where the thread is filed now (a point lookup): already here is
	// a no-op, and a move also changes the group it leaves.
	fromGroupID, err := s.store.GetThreadGroupIDForConversation(ctx, convID)
	if err != nil {
		writeThreadGroupStoreError(w, err, "Thread group")
		return
	}
	if fromGroupID == g.ID {
		s.writeThreadGroup(w, r, g.ID, http.StatusOK)
		return
	}
	// The limit is soft: a best-effort check of the members just read, with
	// no transactional count, so concurrent adds can pass it together.
	if len(g.Members) >= store.MaxThreadsPerThreadGroup {
		threadGroupFullError().write(w)
		return
	}
	if err := s.store.SetThreadGroupMember(ctx, g.ID, convID, identity.ID()); err != nil {
		writeThreadGroupStoreError(w, err, "Thread group")
		return
	}
	resp := s.writeThreadGroup(w, r, g.ID, http.StatusOK)
	if resp == nil {
		return
	}
	s.events.PublishChatThreadGroupEvent(ctx, resp.ProjectID, ThreadGroupEventUpdated, *resp)
	if fromGroupID != "" {
		s.publishThreadGroup(ctx, fromGroupID, ThreadGroupEventUpdated)
	}
}

// resolveThreadGroupMember validates the thread being added to a group of
// projectID and returns its conversation ID (design section 2):
//   - exactly one of conversationId and threadId is set; a threadId resolves
//     through GetTopicConversationIDInProject, so a topic of another project
//     is not found;
//   - the conversation exists and is not deleted, and a thread-backed
//     conversation's topic is not deleted;
//   - the conversation is a group conversation of projectID (another
//     project's conversation is not found, so its existence is not
//     disclosed). A direct conversation is 400 only for one of its own two
//     participants; anyone else gets the not-found answer, so holding a
//     DM's ID does not reveal that the DM exists;
//   - it is not the project's #general thread (400).
func (s *Server) resolveThreadGroupMember(ctx context.Context, identity Identity, projectID string, req addThreadGroupThreadRequest) (string, *apiError) {
	notFound := &apiError{status: http.StatusNotFound, code: ErrCodeNotFound, message: "Thread not found"}
	if (req.ConversationID == "") == (req.ThreadID == "") {
		return "", &apiError{status: http.StatusBadRequest, code: ErrCodeInvalidRequest, message: "exactly one of conversationId and threadId is required"}
	}
	convID := req.ConversationID
	if req.ThreadID != "" {
		s.mu.RLock()
		wcs := s.webChatStore
		s.mu.RUnlock()
		if wcs == nil {
			return "", notFound
		}
		id, err := wcs.GetTopicConversationIDInProject(ctx, projectID, req.ThreadID)
		if errors.Is(err, store.ErrNotFound) {
			return "", notFound
		}
		if err != nil {
			return "", &apiError{status: http.StatusInternalServerError, code: ErrCodeInternalError, message: "failed to resolve thread"}
		}
		if id == "" {
			return "", notFound
		}
		convID = id
	}

	conv, err := s.store.GetConversation(ctx, convID)
	if errors.Is(err, store.ErrNotFound) {
		return "", notFound
	}
	if err != nil {
		return "", &apiError{status: http.StatusInternalServerError, code: ErrCodeInternalError, message: "failed to read conversation"}
	}
	if conv.DeletedAt != nil {
		return "", notFound
	}
	if conv.Kind == "direct" {
		if !authorizeDMRead(conv, identity.Type(), identity.ID()) {
			return "", notFound
		}
		return "", &apiError{status: http.StatusBadRequest, code: ErrCodeInvalidRequest, message: "direct conversations cannot be added to a thread group"}
	}
	if conv.Kind != "group" || conv.ProjectID == nil || *conv.ProjectID != projectID {
		return "", notFound
	}
	if _, _, perr := messaging.ParseThreadConversationExternalRef(conv.ExternalRef); perr == nil {
		topic, err := s.linkedTopic(ctx, conv)
		if err != nil {
			return "", &apiError{status: http.StatusInternalServerError, code: ErrCodeInternalError, message: "failed to read thread"}
		}
		if topic == nil {
			// Thread-backed, but the topic is deleted or unlinked.
			return "", notFound
		}
		if topic.IsGeneral {
			return "", &apiError{status: http.StatusBadRequest, code: ErrCodeInvalidRequest, message: "the #general thread cannot be added to a thread group"}
		}
	}
	return conv.ID, nil
}

// handleRemoveThreadGroupThread handles
// DELETE /api/v1/thread-groups/{id}/threads/{conversationId}. It is
// idempotent: a conversation that is not in the group is 204 too.
func (s *Server) handleRemoveThreadGroupThread(w http.ResponseWriter, r *http.Request, id, conversationID string) {
	ctx := r.Context()
	cls, _, ok := requireInboxCredential(w, r)
	if !ok {
		return
	}
	g := s.loadAuthorizedThreadGroup(w, r, cls, id, true)
	if g == nil {
		return
	}
	if _, err := uuid.Parse(conversationID); err != nil {
		// A malformed ID cannot be a member: idempotent 204.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	member := false
	for _, m := range g.Members {
		if m.ConversationID == conversationID {
			member = true
			break
		}
	}
	if err := s.store.RemoveThreadGroupMember(ctx, g.ID, conversationID); err != nil {
		writeThreadGroupStoreError(w, err, "Thread group")
		return
	}
	if member {
		s.publishThreadGroup(ctx, g.ID, ThreadGroupEventUpdated)
	}
	w.WriteHeader(http.StatusNoContent)
}

// threadGroupForNewConversation validates the threadGroupId of a
// POST /api/v1/conversations before the conversation is minted (design
// section 2): the group must exist in projectID (404 otherwise, so another
// project's group is not disclosed), and it must have room for one more
// thread (422). An agent must belong to projectID (403). The caller has
// already authorized projectID.
func (s *Server) threadGroupForNewConversation(ctx context.Context, threadGroupID, projectID string) (*store.ThreadGroup, *apiError) {
	// The strict agent same-project rule of every thread group mutation
	// (design section 3): handleCreateConversation authorizes only
	// project:read on the body project.
	if ai := GetAgentIdentityFromContext(ctx); ai != nil && ai.ProjectID() != projectID {
		return nil, &apiError{status: http.StatusForbidden, code: ErrCodeForbidden, message: "agents can only file threads into thread groups of their own project"}
	}
	g, err := s.store.GetThreadGroup(ctx, threadGroupID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, &apiError{status: http.StatusNotFound, code: ErrCodeNotFound, message: "Thread group not found"}
	}
	if err != nil {
		return nil, &apiError{status: http.StatusInternalServerError, code: ErrCodeInternalError, message: "failed to read thread group"}
	}
	if g.ProjectID != projectID {
		return nil, &apiError{status: http.StatusNotFound, code: ErrCodeNotFound, message: "Thread group not found"}
	}
	// Soft limit, as in handleAddThreadGroupThread.
	if len(g.Members) >= store.MaxThreadsPerThreadGroup {
		return nil, threadGroupFullError()
	}
	return g, nil
}

// fileNewConversation files a just-created conversation into thread group
// g and publishes the group. The conversation already exists and has been
// announced, so a failure (for example the group was deleted meanwhile) is
// logged rather than failing a create that succeeded.
func (s *Server) fileNewConversation(ctx context.Context, g *store.ThreadGroup, conversationID, addedBy string) {
	if err := s.store.SetThreadGroupMember(ctx, g.ID, conversationID, addedBy); err != nil {
		slog.WarnContext(ctx, "handleCreateConversation: filing into thread group failed",
			"conversationID", conversationID, "threadGroupID", g.ID, "error", err)
		return
	}
	s.publishThreadGroup(ctx, g.ID, ThreadGroupEventUpdated)
}

// removeDeletedTopicFromThreadGroups takes the conversation of a just
// deleted topic out of its thread group (design section 7a). It is best
// effort: a failure is logged, and reads skip a member whose topic is
// deleted anyway. The chat.thread_group event for the group it leaves is
// published after the cleanup.
func (s *Server) removeDeletedTopicFromThreadGroups(ctx context.Context, topic *WebChatTopic) {
	if topic == nil || topic.ConversationID == "" {
		return
	}
	fromGroupID, err := s.store.GetThreadGroupIDForConversation(ctx, topic.ConversationID)
	if err != nil {
		slog.WarnContext(ctx, "handleTopicDelete: thread group lookup failed",
			"topicID", topic.ID, "conversationID", topic.ConversationID, "error", err)
	}
	if err := s.store.RemoveThreadGroupMembersForConversation(ctx, topic.ConversationID); err != nil {
		slog.WarnContext(ctx, "handleTopicDelete: thread group member cleanup failed",
			"topicID", topic.ID, "conversationID", topic.ConversationID, "error", err)
		return
	}
	if fromGroupID != "" {
		s.publishThreadGroup(ctx, fromGroupID, ThreadGroupEventUpdated)
	}
}
