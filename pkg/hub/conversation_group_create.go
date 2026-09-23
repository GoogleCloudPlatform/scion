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

// Package hub provides the Scion Hub API server.
package hub

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// apiError carries an HTTP status, a machine-readable code, and a message
// for internal seams (like createGroupConversation) that need to report a
// failure without direct access to an http.ResponseWriter. The caller writes
// it with write once it decides the handler should stop.
type apiError struct {
	status  int
	code    string
	message string
}

// write sends e as a standard JSON error response.
func (e *apiError) write(w http.ResponseWriter) {
	writeError(w, e.status, e.code, e.message, nil)
}

// validateThreadName trims and validates a thread/topic name. Rules (shared
// with handleCreateThread, handlers_chat_v2.go): non-empty after trimming,
// at most 100 runes, and matching threadNameRegexp. Returns the trimmed name.
//
// This is the "one common requirement for thread names" decided in the
// chat-thread-bridge design (Q1): the conversation-create API and the web
// thread-create API enforce identical name rules.
func validateThreadName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", errors.New("name is required")
	}
	if nameRunes := []rune(name); len(nameRunes) > 100 {
		return "", errors.New("name must be 100 characters or fewer")
	}
	if !threadNameRegexp.MatchString(name) {
		return "", errors.New("name contains invalid characters")
	}
	return name, nil
}

// isTopicNameConflict reports whether err represents a topic/thread name
// uniqueness violation raised by the webchat store (SQLite or Postgres).
// This centralizes the string sniffing that handleCreateThread, UpdateTopic,
// and handleConversationPromote each did independently, so every call site
// maps the same underlying store errors to the same outcome.
//
// The match is deliberately narrower than "any unique/duplicate-key error":
// CreateTopic, UpdateTopic, and PromoteDM each write to both webchat_topic
// and conversations in the same transaction, so the same call can also fail
// on the unrelated conversations(surface, external_ref) partial unique index
// (e.g. a DEF-156 lookup-then-insert race). A bare "unique" or "duplicate
// key" substring match would misreport that as a name conflict. So beyond
// requiring a unique-violation shape, this also requires a signal that ties
// the violation to the topic name index specifically:
//   - SQLite (modernc.org/sqlite, the hub's actual driver — pkg/ent/entc/client.go:80 —
//     registered as "sqlite") reports column names, not the index name, even
//     for a named expression index — e.g. "constraint failed: UNIQUE
//     constraint failed: webchat_topic.project_id, webchat_topic.name (2067)".
//   - Postgres reports the constraint/index name verbatim — e.g.
//     `duplicate key value violates unique constraint "idx_webchat_topic_project_name"`.
//
// Verified against the real modernc.org/sqlite error text for both the topic
// name index and the conversations external_ref index (they differ exactly
// as described above — the extra "constraint failed: " prefix and " (2067)"
// suffix don't affect a Contains match); the Postgres format matches the
// standard "duplicate key value violates unique constraint %q" wording this
// codebase already relies on elsewhere (entadapter/conversation_store.go
// isUniqueConstraintError).
func isTopicNameConflict(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	if strings.Contains(msg, "name conflict") {
		return true
	}
	isUniqueViolation := strings.Contains(msg, "UNIQUE constraint") || strings.Contains(msg, "duplicate key")
	if !isUniqueViolation {
		return false
	}
	return strings.Contains(msg, "idx_webchat_topic_project_name") ||
		strings.Contains(msg, "webchat_topic.name")
}

// createGroupParams bundles the inputs needed to mint a group conversation
// on a given surface.
type createGroupParams struct {
	ProjectID   string
	DisplayName string
	CreatedBy   string // creator's principal ID; stored as topic.CreatedBy
}

// createGroupConversation mints a group conversation on the given surface.
// Only "native" exists today; it creates the webchat topic (which owns
// naming and mints the linked conversation atomically) and publishes the
// topic "created" SSE event, exactly like handleCreateThread.
//
// This is structured as a surface dispatch rather than a native-only
// rewrite (design doc chat-thread-bridge §3.6a) so that a future
// external-surface create (e.g. discord) lands as one additive case here,
// without a "surface" request field or stub existing yet.
func (s *Server) createGroupConversation(ctx context.Context, surface string, p createGroupParams) (*store.Conversation, *apiError) {
	switch surface {
	case "native":
		return s.createNativeGroupConversation(ctx, p)
	default:
		// Unreachable today: the only caller (handleCreateConversation)
		// always passes "native", and there is no request field yet to
		// choose otherwise (design §3.6a — no surface field until a second
		// surface exists). Reaching here is a programmer error, not a bad
		// request, so it gets a 500 rather than a user-facing 400.
		slog.ErrorContext(ctx, "createGroupConversation: unreachable surface", "surface", surface)
		return nil, &apiError{status: http.StatusInternalServerError, code: ErrCodeInternalError, message: "unsupported surface"}
	}
}

// createNativeGroupConversation is the "native" case of createGroupConversation.
// See design doc chat-thread-bridge §3.2-§3.4.
func (s *Server) createNativeGroupConversation(ctx context.Context, p createGroupParams) (*store.Conversation, *apiError) {
	name, err := validateThreadName(p.DisplayName)
	if err != nil {
		return nil, &apiError{status: http.StatusBadRequest, code: ErrCodeValidationError, message: err.Error()}
	}

	s.mu.RLock()
	wcs := s.webChatStore
	s.mu.RUnlock()
	if wcs == nil {
		return nil, &apiError{status: http.StatusServiceUnavailable, code: "SERVICE_UNAVAILABLE", message: "Chat not available"}
	}

	now := time.Now().UTC()
	topic := WebChatTopic{
		ID:        api.NewUUID(),
		ProjectID: p.ProjectID,
		Name:      name,
		// Pre-generate the ConversationID (design §3.4): CreateTopic takes
		// WebChatTopic by value and never returns the minted ID back to the
		// caller. Because the topic ID is freshly generated, CreateTopic's
		// (native, extRef) lookup-hit reuse path cannot fire for it, so this
		// is guaranteed to be the ID CreateTopic actually uses.
		ConversationID: api.NewUUID(),
		CreatedBy:      p.CreatedBy,
		CreatedAt:      now,
		LastActivityAt: now,
	}

	if err := wcs.CreateTopic(ctx, topic); err != nil {
		if isTopicNameConflict(err) {
			return nil, &apiError{status: http.StatusConflict, code: "NAME_CONFLICT", message: "a conversation with that name already exists in this project"}
		}
		slog.ErrorContext(ctx, "createGroupConversation: CreateTopic failed", "error", err)
		return nil, &apiError{status: http.StatusInternalServerError, code: ErrCodeInternalError, message: "failed to create conversation"}
	}

	// Read back the conversation CreateTopic minted atomically alongside the
	// topic. A not-found here means the SQLite hasConversationsTable() gate
	// was false, which cannot happen on a hub where the conversation API
	// itself works (design §3.4) — treat it as an internal error.
	conv, err := s.store.GetConversation(ctx, topic.ConversationID)
	if err != nil {
		slog.ErrorContext(ctx, "createGroupConversation: read-back of newly created conversation failed",
			"conversationID", topic.ConversationID, "error", err)
		return nil, &apiError{status: http.StatusInternalServerError, code: ErrCodeInternalError, message: "failed to load created conversation"}
	}

	s.events.PublishChatTopicEvent(ctx, p.ProjectID, "created", topic)

	return conv, nil
}

// linkedTopic returns the webchat topic linked to conv, or nil if conv has
// no linked topic — an external-surface group (no "thread:" external_ref),
// a legacy unlinked native group, or no webchat store wired.
//
// It requires topic.ConversationID == conv.ID (design doc §0, mirroring the
// reverse-pointer direction from the chat-thread-bridge design §2.6.3): a
// topic whose external_ref round-trips to conv's ID but no longer points
// back at it is treated the same as "no topic", rather than silently
// updating the wrong topic.
func (s *Server) linkedTopic(ctx context.Context, conv *store.Conversation) (*WebChatTopic, error) {
	_, topicID, err := messaging.ParseThreadConversationExternalRef(conv.ExternalRef)
	if err != nil {
		// Not a thread-backed conversation: external-surface group, or a
		// legacy conversation with no (or a non-thread) external_ref.
		return nil, nil
	}

	s.mu.RLock()
	wcs := s.webChatStore
	s.mu.RUnlock()
	if wcs == nil {
		return nil, nil
	}

	topic, err := wcs.GetTopic(ctx, topicID)
	if err != nil {
		return nil, fmt.Errorf("linkedTopic: get topic %s: %w", topicID, err)
	}
	if topic == nil || topic.ConversationID != conv.ID {
		return nil, nil
	}
	return topic, nil
}

// setGroupDefaultAgent sets or clears the default agent of a group
// conversation. agent == nil clears. The caller has already authorized and
// validated the request (design doc §3.1) — this is the single write path
// for the group default agent, converging conversations.default_agent_id
// (the owner, design doc §0) and webchat_topic.default_agent (the
// projection) in one transaction when the conversation is topic-linked.
func (s *Server) setGroupDefaultAgent(ctx context.Context, conv *store.Conversation, agent *store.Agent) error {
	topic, err := s.linkedTopic(ctx, conv)
	if err != nil {
		return err
	}

	var newDefaultAgentID *string
	if agent != nil {
		id := agent.ID
		newDefaultAgentID = &id
	}

	if topic == nil {
		// External-surface or unlinked group: conversation column only
		// (design doc §3.1 table, "else" branch).
		conv.DefaultAgentID = newDefaultAgentID
		if err := s.store.UpdateConversation(ctx, conv); err != nil {
			return fmt.Errorf("setGroupDefaultAgent: update conversation: %w", err)
		}
		return nil
	}

	s.mu.RLock()
	wcs := s.webChatStore
	s.mu.RUnlock()
	if wcs == nil {
		return errors.New("setGroupDefaultAgent: webchat store unavailable")
	}

	// The projection value is the agent slug — the form the UI shows and
	// the web send path resolves first (design doc §3.1); the owner value
	// is always the UUID. Both go through one TopicUpdate so UpdateTopic
	// converges them in the same transaction.
	slug, id := "", ""
	if agent != nil {
		slug, id = agent.Slug, agent.ID
	}
	if err := wcs.UpdateTopic(ctx, topic.ID, TopicUpdate{
		DefaultAgent:   &slug,
		DefaultAgentID: &id,
	}); err != nil {
		return fmt.Errorf("setGroupDefaultAgent: update topic: %w", err)
	}
	conv.DefaultAgentID = newDefaultAgentID

	updated, err := wcs.GetTopic(ctx, topic.ID)
	if err != nil {
		return fmt.Errorf("setGroupDefaultAgent: reload topic: %w", err)
	}
	if updated != nil {
		s.events.PublishChatTopicEvent(ctx, updated.ProjectID, "updated", *updated)
	}
	return nil
}

// ensureGroupParticipants records agents as participants of a group
// conversation (design doc §3.3, F2b). Participant rows are a listing
// index only — §3.2 already made project membership the read authority —
// so this is best-effort: a failure here is logged at warn and never fails
// the caller's send (AC-12).
//
// Uses store.EnsureParticipant, the same idempotent, race-safe primitive
// handleAgentMessage's own auto-registration already uses (existing row —
// active or soft-removed — is left untouched; a concurrent insert loses
// the race safely rather than erroring). This avoids the plain AddParticipant
// duplicate-key error on every repeat message from an already-participating
// agent.
//
// Rule: participant means woken (design §3.3 / §4). Callers must pass only
// agents that were actually dispatched into the conversation, not every
// agent merely named (e.g. an @-mention denied by authorization, or a
// human-mention notification target).
func (s *Server) ensureGroupParticipants(ctx context.Context, conversationID string, agents []*store.Agent) {
	if conversationID == "" {
		return
	}
	for _, agent := range agents {
		if agent == nil || agent.ID == "" {
			continue
		}
		if err := s.store.EnsureParticipant(ctx, &store.ConversationParticipant{
			ConversationID: conversationID,
			PrincipalKind:  "agent",
			PrincipalID:    agent.ID,
			Role:           "member",
		}); err != nil {
			slog.WarnContext(ctx, "ensureGroupParticipants: ensure participant failed",
				"conversationID", conversationID, "agentID", agent.ID, "error", err)
		}
	}
}

// registerGroupPrimary records the primary recipient of a handleAgentMessage
// dispatch as a participant of a thread-derived group conversation (review
// round 2 finding #2, factored out per round 3 finding #7).
//
// handleAgentMessage's caller-supplied conversation_id branch already
// registers its primary recipient before dispatch (pre-existing, out of
// scope per the round-2 design addendum). The thread-derived branch
// (DeriveConversationKey, no conversation_id) does not, so without this
// call the mention co-recipients processMentions adds would be the only
// participants — the agent the message was actually addressed to and
// dispatched to would have no row. Call this once per dispatch path, after
// that path's own dispatch step has already returned success:
//   - the agent-DM fork, after ExecuteAgentDM
//   - the managed-runtime path, after managedAgentMessage
//   - the broker-dispatched path, after dispatchWithBrokerRetry
//
// groupConversationID is empty for direct conversations and for the
// caller-supplied conversation_id branch's own registration path, so this
// is a deliberate no-op there; ensureGroupParticipants' use of
// store.EnsureParticipant also makes a redundant call harmless.
func (s *Server) registerGroupPrimary(ctx context.Context, groupConversationID string, agent *store.Agent) {
	if groupConversationID == "" {
		return
	}
	s.ensureGroupParticipants(ctx, groupConversationID, []*store.Agent{agent})
}
