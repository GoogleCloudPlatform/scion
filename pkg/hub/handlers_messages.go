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
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// handleMessages handles GET /api/v1/messages.
// Lists messages for the authenticated user.
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		Forbidden(w)
		return
	}

	q := r.URL.Query()

	filter := store.MessageFilter{
		RecipientID: user.ID(),
	}
	if q.Get("unread") == "true" {
		filter.OnlyUnread = true
	}
	if projectID := q.Get("project"); projectID != "" {
		filter.ProjectID = projectID
	}
	if agentID := q.Get("agent"); agentID != "" {
		filter.AgentID = agentID
	}
	if msgType := q.Get("type"); msgType != "" {
		filter.Type = msgType
	}

	opts := store.ListOptions{}
	if limitStr := q.Get("limit"); limitStr != "" {
		if n, err := strconv.Atoi(limitStr); err == nil && n > 0 {
			opts.Limit = n
		}
	}
	if cursor := q.Get("cursor"); cursor != "" {
		opts.Cursor = cursor
	}

	result, err := s.store.ListMessages(r.Context(), filter, opts)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, result)
}

// handleMessageRoutes handles requests under /api/v1/messages/.
// Routes:
//   - GET  /api/v1/messages/{id}        — Get a single message
//   - POST /api/v1/messages/{id}/read   — Mark a message as read
//   - POST /api/v1/messages/read-all    — Mark all messages as read
func (s *Server) handleMessageRoutes(w http.ResponseWriter, r *http.Request) {
	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		Forbidden(w)
		return
	}

	id, action := extractAction(r, "/api/v1/messages")

	// POST /api/v1/messages/read-all
	if id == "read-all" && r.Method == http.MethodPost {
		if err := s.store.MarkAllMessagesRead(r.Context(), user.ID()); err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		slog.Info("All messages marked as read", "userID", user.ID())
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}

	if id == "" {
		NotFound(w, "Message")
		return
	}

	// POST /api/v1/messages/{id}/read
	if action == "read" && r.Method == http.MethodPost {
		// Verify the message is addressed to this user before marking read.
		msg, err := s.store.GetMessage(r.Context(), id)
		if err != nil {
			writeErrorFromErr(w, err, "Message")
			return
		}
		if msg.RecipientID != user.ID() {
			Forbidden(w)
			return
		}
		if err := s.store.MarkMessageRead(r.Context(), id); err != nil {
			writeErrorFromErr(w, err, "Message")
			return
		}
		slog.Info("Message marked as read", "messageID", id, "userID", user.ID())
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}

	// GET /api/v1/messages/{id}
	if action == "" && r.Method == http.MethodGet {
		msg, err := s.store.GetMessage(r.Context(), id)
		if err != nil {
			writeErrorFromErr(w, err, "Message")
			return
		}
		// Only allow access to messages addressed to this user.
		if msg.RecipientID != user.ID() {
			Forbidden(w)
			return
		}
		writeJSON(w, http.StatusOK, msg)
		return
	}

	MethodNotAllowed(w)
}

// handleAgentMessages handles GET /api/v1/agents/{id}/messages.
// Returns both sides of the conversation between the authenticated user
// and the specified agent (messages where the user is either the sender
// or the recipient, scoped to this agent). Authorisation is enforced via
// the agent read permission rather than a per-row recipient check, so
// agents' outbound replies and the user's own sent messages both render
// in the viewer.
func (s *Server) handleAgentMessages(w http.ResponseWriter, r *http.Request, agentID string) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	ctx := r.Context()
	user := GetUserIdentityFromContext(ctx)
	if user == nil {
		Forbidden(w)
		return
	}

	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	decision := s.authzService.CheckAccess(ctx, user, agentResource(agent), ActionRead)
	if !decision.Allowed {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "Access denied", nil)
		return
	}

	q := r.URL.Query()
	opts := store.ListOptions{}
	if limitStr := q.Get("limit"); limitStr != "" {
		if n, err := strconv.Atoi(limitStr); err == nil && n > 0 {
			opts.Limit = n
		}
	}
	if cursor := q.Get("cursor"); cursor != "" {
		opts.Cursor = cursor
	}

	filter := store.MessageFilter{
		AgentID:       agentID,
		ParticipantID: user.ID(),
	}

	result, err := s.store.ListMessages(ctx, filter, opts)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, result)
}

// handleAgentMessagesStream handles GET /api/v1/agents/{id}/messages/stream.
// Streams new messages involving a specific agent in real time, scoped to
// the conversation between the current authenticated user and the agent.
// Unlike /message-logs/stream this does not depend on Cloud Logging: it
// subscribes to the in-process event bus that handleAgentOutboundMessage
// and handleAgentMessage already publish to, so it works on any hub
// deployment with no additional configuration.
func (s *Server) handleAgentMessagesStream(w http.ResponseWriter, r *http.Request, agentID string) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	// The event bus is an interface; only ChannelEventPublisher supports
	// subscription. Check this before hitting the store so noop-publisher
	// hubs fail fast without a wasted DB roundtrip.
	ep, ok := s.events.(*ChannelEventPublisher)
	if !ok {
		writeError(w, http.StatusNotImplemented, "not_implemented",
			"Real-time message streaming is not available on this hub", nil)
		return
	}

	ctx := r.Context()
	user := GetUserIdentityFromContext(ctx)
	if user == nil {
		Forbidden(w)
		return
	}

	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	decision := s.authzService.CheckAccess(ctx, user, agentResource(agent), ActionRead)
	if !decision.Allowed {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "Access denied", nil)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	// Clear the server's write deadline for this long-lived connection —
	// without this the global WriteTimeout will kill the stream and cause
	// reconnection churn, matching the pattern in web.go's SSE handler.
	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(time.Time{}); err != nil {
		slog.Debug("Failed to clear write deadline for messages stream", "error", err)
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher.Flush()

	ch, unsubscribe := ep.Subscribe("agent." + agent.ID + ".message")
	defer unsubscribe()

	userID := user.ID()
	heartbeat := time.NewTicker(30 * time.Second)
	defer heartbeat.Stop()

	// Server-side timeout matching handlers_logs.go: tell the client to
	// reconnect after 10 minutes so long-lived connections don't accumulate.
	timeout := time.NewTimer(10 * time.Minute)
	defer timeout.Stop()

	for {
		select {
		case evt, ok := <-ch:
			if !ok {
				return
			}
			// Filter: only forward events where the current user is a
			// participant. Without this the stream would include messages
			// from every other user's conversation with this agent.
			var payload UserMessageEvent
			if err := json.Unmarshal(evt.Data, &payload); err != nil {
				continue
			}
			if payload.SenderID != userID && payload.RecipientID != userID {
				continue
			}
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", evt.Data)
			flusher.Flush()
		case <-heartbeat.C:
			fmt.Fprintf(w, ":heartbeat %d\n\n", time.Now().UnixMilli())
			flusher.Flush()
		case <-timeout.C:
			fmt.Fprintf(w, "event: timeout\ndata: {\"message\":\"stream timeout, please reconnect\"}\n\n")
			flusher.Flush()
			return
		case <-ctx.Done():
			return
		}
	}
}
