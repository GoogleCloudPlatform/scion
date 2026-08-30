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

	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
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
	agentID := q.Get("agent")
	if agentID != "" {
		filter.AgentID = agentID
	}
	if msgType := q.Get("type"); msgType != "" {
		filter.Type = msgType
	}

	// Phase 8 read-switch: when ConversationReadSwitch is ON and an agent
	// filter is provided, resolve the DM conversation and add ConversationID
	// to the filter so reads use the conversation model.
	// R-9: agentID is client-supplied and may be a slug. Resolve to UUID
	// before the DM key derivation. On lookup failure, skip the conversation
	// path WITHOUT calling IncFallback — a bad reference is not a migration
	// gap, and mixing the two corrupts the S4 readiness metric.
	//
	// G3: fallback REMOVED. When the agent resolves but the conversation
	// does not, return a typed 409 error instead of silently using the old
	// filter. Agent lookup failures still skip the block (R-9 discipline)
	// but are now counted by SwitchBypassMetrics for coverage visibility.
	if ops := s.GetOperationalSettings(); ops != nil && ops.ConversationReadSwitch() {
		if agentID != "" {
			if resolvedAgent, lookupErr := s.store.GetAgent(r.Context(), agentID); lookupErr == nil && resolvedAgent != nil {
				convResult := messaging.ResolveDMConversationForRead(r.Context(), s.store, s.messageLog, "agent", resolvedAgent.ID, "user", user.ID())
				if convResult != nil {
					filter.ConversationID = convResult.ConversationID
				} else {
					slog.Warn("read-switch: DM conversation not resolved for agent message list",
						"agent_id", resolvedAgent.ID, "user_id", user.ID())
					writeError(w, http.StatusConflict, ErrCodeConversationNotResolved,
						"Conversation could not be resolved for this agent; the read-switch is ON but no matching conversation record exists",
						nil)
					return
				}
			} else {
				// G3-e: switch ON, agent lookup failed — track bypass reason.
				// R-9 discipline: not IncFallback (readiness), but SwitchBypass (coverage).
				if _, parseErr := uuid.Parse(agentID); parseErr != nil {
					messaging.SwitchBypassMetrics.IncSlugParam()
				} else {
					messaging.SwitchBypassMetrics.IncAgentNotFound()
				}
			}
		} else {
			// G3-e: switch ON but no agent param — no DM key to derive.
			messaging.SwitchBypassMetrics.IncNonDMKey()
		}
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
// Returns messages involving the specified agent, filtered by the caller's
// permission level. Users who can manage the agent (owners, project admins,
// global admins) see all messages; other users see only messages where they
// are a participant (sender or recipient), preserving privacy across users
// who share read access to the same agent.
func (s *Server) handleAgentMessages(w http.ResponseWriter, r *http.Request, agentID string) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	if !checkAgentReadScope(w, r) {
		return
	}

	ctx := r.Context()
	ctx, span := tracer.Start(ctx, "hub.message.list")
	defer span.End()
	// Note: HTTP error status is recorded by the otelhttp parent span.
	span.SetAttributes(
		attribute.String("scion.agent.id", agentID),
	)
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

	// Check manage first — manage implies read and lets us skip a second
	// authz lookup for users who have it.
	res := agentResource(agent)
	canManage := s.authzService.CheckAccess(ctx, user, res, ActionManage)
	if !canManage.Allowed {
		decision := s.authzService.CheckAccess(ctx, user, res, ActionRead)
		if !decision.Allowed {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "Access denied", nil)
			return
		}
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

	// Users who can manage the agent (owners, project admins, global admins)
	// see all messages including those from chat integrations. Other users
	// only see messages where they are a participant, preserving privacy.
	filter := store.MessageFilter{
		AgentID: agentID,
	}
	if !canManage.Allowed {
		filter.ParticipantID = user.ID()
	}

	// Optional query params for chat filtering.
	channel := q.Get("channel")
	if channel != "" {
		filter.Channel = channel
	}
	if visParams := q["visibility"]; len(visParams) > 0 {
		filter.Visibility = visParams
	}
	// Intentionally ignore parse errors on "before" — an invalid value is
	// silently dropped, matching the convention used by "limit" above and
	// other query params in the codebase.
	if beforeStr := q.Get("before"); beforeStr != "" {
		if t, err := time.Parse(time.RFC3339, beforeStr); err == nil {
			filter.Before = t
		}
	}

	// Phase 8 read-switch: when ConversationReadSwitch is ON, resolve the
	// conversation and add ConversationID to the filter. For agent messages
	// with a web channel, the conversation is a DM between the agent and the
	// requesting user. For thread-scoped queries, resolve via the thread key.
	//
	// G3: fallback REMOVED at both sub-paths. When the switch is ON and
	// resolution fails, return a typed 409 error. Non-web channels still
	// skip the block (no conversation model for external surfaces).
	if ops := s.GetOperationalSettings(); ops != nil && ops.ConversationReadSwitch() {
		threadID := q.Get("thread_id")
		if threadID != "" && agent.ProjectID != "" {
			convResult := messaging.ResolveThreadConversationForRead(ctx, s.store, s.messageLog, threadID, agent.ProjectID)
			if convResult != nil {
				filter.ConversationID = convResult.ConversationID
			} else {
				slog.Warn("read-switch: thread conversation not resolved for agent messages",
					"thread_id", threadID, "project_id", agent.ProjectID, "agent_id", agent.ID)
				writeError(w, http.StatusConflict, ErrCodeConversationNotResolved,
					"Conversation could not be resolved for this thread; the read-switch is ON but no matching conversation record exists",
					nil)
				return
			}
		} else if channel == "web" || channel == "" {
			// Default: DM conversation between agent and current user.
			// R-1: Use agent.ID (UUID) not agentID (raw handler param, may be a slug).
			// The resolved agent is already in scope from GetAgent above.
			convResult := messaging.ResolveDMConversationForRead(ctx, s.store, s.messageLog, "agent", agent.ID, "user", user.ID())
			if convResult != nil {
				filter.ConversationID = convResult.ConversationID
			} else {
				slog.Warn("read-switch: DM conversation not resolved for agent messages",
					"agent_id", agent.ID, "user_id", user.ID())
				writeError(w, http.StatusConflict, ErrCodeConversationNotResolved,
					"Conversation could not be resolved for this agent; the read-switch is ON but no matching conversation record exists",
					nil)
				return
			}
		} else {
			// G3-e: switch ON, non-web channel with no thread_id — conversation
			// scoping not applied. Track for coverage visibility.
			messaging.SwitchBypassMetrics.IncNonWebChannel()
		}
	}

	result, err := s.store.ListMessages(ctx, filter, opts)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, result)
}

// handleAgentMessagesStream handles GET /api/v1/agents/{id}/messages/stream.
// Streams new messages involving a specific agent in real time. Users who
// can manage the agent see all messages; others see only their own.
// Unlike /message-logs/stream this does not depend on Cloud Logging: it
// subscribes to the in-process event bus that handleAgentOutboundMessage
// and handleAgentMessage already publish to, so it works on any hub
// deployment with no additional configuration.
func (s *Server) handleAgentMessagesStream(w http.ResponseWriter, r *http.Request, agentID string) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	// Real-time streaming requires a subscription-capable EventPublisher
	// (ChannelEventPublisher or PostgresEventPublisher). The no-op publisher
	// returns a nil channel, so fail fast before hitting the store to avoid a
	// wasted DB roundtrip on hubs without a configured publisher.
	if _, isNoop := s.events.(noopEventPublisher); isNoop || s.events == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented",
			"Real-time message streaming is not available on this hub", nil)
		return
	}
	ep := s.events

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

	// Check manage first — manage implies read and lets us skip a second
	// authz lookup for users who have it.
	res := agentResource(agent)
	canManage := s.authzService.CheckAccess(ctx, user, res, ActionManage)
	if !canManage.Allowed {
		decision := s.authzService.CheckAccess(ctx, user, res, ActionRead)
		if !decision.Allowed {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "Access denied", nil)
			return
		}
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

	// Users who can manage the agent see all messages; others only see
	// messages where they are a participant.
	userID := user.ID()
	filterStream := !canManage.Allowed

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
			if filterStream {
				var payload UserMessageEvent
				if err := json.Unmarshal(evt.Data, &payload); err != nil {
					continue
				}
				if payload.SenderID != userID && payload.RecipientID != userID {
					continue
				}
			}
			_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", evt.Data)
			flusher.Flush()
		case <-heartbeat.C:
			_, _ = fmt.Fprintf(w, ":heartbeat %d\n\n", time.Now().UnixMilli())
			flusher.Flush()
		case <-timeout.C:
			_, _ = fmt.Fprintf(w, "event: timeout\ndata: {\"message\":\"stream timeout, please reconnect\"}\n\n")
			flusher.Flush()
			return
		case <-ctx.Done():
			return
		}
	}
}
