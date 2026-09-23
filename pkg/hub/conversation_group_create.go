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
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
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
//   - SQLite (mattn/go-sqlite3) reports column names, not the index name,
//     even for a named expression index — e.g. "UNIQUE constraint failed:
//     webchat_topic.project_id, webchat_topic.name".
//   - Postgres reports the constraint/index name verbatim — e.g.
//     `duplicate key value violates unique constraint "idx_webchat_topic_project_name"`.
//
// Verified against the real mattn/go-sqlite3 error text for both the topic
// name index and the conversations external_ref index (they differ exactly
// as described above); the Postgres format matches the standard
// "duplicate key value violates unique constraint %q" wording this codebase
// already relies on elsewhere (entadapter/conversation_store.go isUniqueConstraintError).
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
		return nil, &apiError{status: http.StatusBadRequest, code: ErrCodeInvalidRequest, message: "unsupported surface"}
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
