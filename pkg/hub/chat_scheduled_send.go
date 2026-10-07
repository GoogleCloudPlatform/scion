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
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// Scheduled send in native web chat (ptone/scion#3666).
//
// A user schedules a message in a topic; the hub keeps it in
// webchat_scheduled_message (visible only to that user) and a sweeper on
// every hub replica sends it at fire time through sendChatMessage, the same
// function the live send handler uses, as an ordinary message from the
// user. A compare-and-set claim (pending -> sending) makes exactly one
// replica deliver each row. Nothing is decided from what was true at
// schedule time: the sender, the topic, its project and the sender's access
// are all checked again at fire time.
//
// Everything here is gated by the web.chat_scheduled_send experiment: while
// it is off the routes answer 404 and the sweeper holds pending rows.

const (
	// scheduledSendTick is how often each replica looks for due messages;
	// a message is sent at most about this long after its fire time.
	scheduledSendTick = 10 * time.Second
	// scheduledSendBatch bounds how many due messages one tick handles.
	scheduledSendBatch = 50
	// scheduledSendClientType is the client type of the identity a
	// scheduled message is sent with, and the executor recorded for it.
	scheduledSendClientType = "scheduled-send"
)

// ChatScheduledEvent is published to the sender on
// user.<id>.chat.scheduled when one of their scheduled messages changes.
type ChatScheduledEvent struct {
	// Action is created, cancelled, sent or failed.
	Action           string                   `json:"action"`
	ScheduledMessage scheduledMessageResponse `json:"scheduledMessage"`
}

// scheduledMessageResponse is the API form of a scheduled chat message.
type scheduledMessageResponse struct {
	ID              string    `json:"id"`
	ConversationKey string    `json:"conversationKey"`
	Content         string    `json:"content"`
	ReplyToID       string    `json:"replyToId,omitempty"`
	FireAt          time.Time `json:"fireAt"`
	Status          string    `json:"status"`
	FailureReason   string    `json:"failureReason,omitempty"`
	MessageID       string    `json:"messageId,omitempty"`
	CreatedAt       time.Time `json:"createdAt"`
	UpdatedAt       time.Time `json:"updatedAt"`
}

func newScheduledMessageResponse(m *ScheduledChatMessage) scheduledMessageResponse {
	return scheduledMessageResponse{
		ID:              m.ID,
		ConversationKey: m.ConversationKey,
		Content:         m.Content,
		ReplyToID:       m.ReplyToID,
		FireAt:          m.FireAt.UTC(),
		Status:          m.Status,
		FailureReason:   m.FailureReason,
		MessageID:       m.MessageID,
		CreatedAt:       m.CreatedAt.UTC(),
		UpdatedAt:       m.UpdatedAt.UTC(),
	}
}

// scheduledSendLog returns the logger for scheduled-send audit records.
func scheduledSendLog() *slog.Logger {
	return logging.Subsystem("hub.chat.scheduled-send")
}

// auditScheduledMessage records a create, cancel or fire of a scheduled
// message, with the sender as principal and scheduled-send as executor.
func auditScheduledMessage(ctx context.Context, action string, m *ScheduledChatMessage, outcome string) {
	scheduledSendLog().Info("scheduled chat message",
		"audit_action", "chat.scheduled."+action,
		"principal_type", "user",
		"principal_id", m.SenderUserID,
		"executor", scheduledSendClientType,
		"scheduled_message_id", m.ID,
		"conversation_key", m.ConversationKey,
		"outcome", outcome,
		"request_id", logging.RequestIDFromContext(ctx),
	)
}

func (s *Server) publishScheduledMessage(ctx context.Context, action string, m *ScheduledChatMessage) {
	if s.events == nil {
		return
	}
	s.events.PublishChatScheduledEvent(ctx, m.SenderUserID, ChatScheduledEvent{
		Action:           action,
		ScheduledMessage: newScheduledMessageResponse(m),
	})
}

// scheduledMessageStore returns the scheduled-message store, or nil when
// native chat storage is unavailable.
func (s *Server) scheduledMessageStore() ScheduledMessageStore {
	s.mu.RLock()
	wcs := s.webChatStore
	s.mu.RUnlock()
	if wcs == nil {
		return nil
	}
	return scheduledMessageStoreFrom(wcs)
}

// ---------------------------------------------------------------------------
// HTTP: /api/v1/chat/conversations/{key}/scheduled[/{id}]
// ---------------------------------------------------------------------------

// handleConversationScheduledRoutes serves the scheduled-message routes of
// a conversation. rest is the path after "scheduled" ("" or "/{id}").
func (s *Server) handleConversationScheduledRoutes(w http.ResponseWriter, r *http.Request, key, rest string) {
	s.requireExperiment(experiments.ChatScheduledSend, func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(rest, "/")
		switch {
		case rest == "":
			switch r.Method {
			case http.MethodGet:
				s.handleScheduledList(w, r, key)
			case http.MethodPost:
				s.handleScheduledCreate(w, r, key)
			default:
				MethodNotAllowed(w, http.MethodGet, http.MethodPost)
			}
		case id != "" && !strings.Contains(id, "/"):
			if r.Method != http.MethodDelete {
				MethodNotAllowed(w, http.MethodDelete)
				return
			}
			s.handleScheduledCancel(w, r, key, id)
		default:
			http.NotFound(w, r)
		}
	})(w, r)
}

// scheduledSendCaller returns the caller of a scheduled-message route, or
// writes the refusal. Only an interactive, unscoped user session may use
// these routes: agents cannot send chat messages at all, and a scoped
// access token's restrictions could not be carried to fire time.
func scheduledSendCaller(w http.ResponseWriter, r *http.Request) UserIdentity {
	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		Forbidden(w)
		return nil
	}
	if IsScopedUserIdentity(user) {
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"scoped access tokens cannot schedule chat messages", nil)
		return nil
	}
	return user
}

// scheduledSendRejectDM refuses direct-message keys: scheduled send is
// available in topics only for now.
func scheduledSendRejectDM(w http.ResponseWriter, key string) bool {
	if strings.HasPrefix(key, "dm:") {
		BadRequest(w, "scheduled send is not available in direct messages")
		return true
	}
	return false
}

// handleScheduledCreate implements POST …/{key}/scheduled.
func (s *Server) handleScheduledCreate(w http.ResponseWriter, r *http.Request, key string) {
	user := scheduledSendCaller(w, r)
	if user == nil || scheduledSendRejectDM(w, key) {
		return
	}
	ctx := r.Context()

	// The same conversation access check as a live send.
	target, serr := s.authorizeChatSend(ctx, user, key)
	if serr != nil {
		serr.write(w)
		return
	}
	sms := scheduledMessageStoreFrom(target.wcs)
	if sms == nil {
		writeError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Chat not available", nil)
		return
	}

	// Scheduling counts against the sender's chat send allowance.
	if !s.allowChatSend(w, user.ID(), chatSenderHuman) {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1048576)
	var body struct {
		Content        string   `json:"content"`
		ReplyToID      string   `json:"reply_to_id,omitempty"`
		FireAt         string   `json:"fire_at"`
		IdempotencyKey string   `json:"idempotency_key,omitempty"`
		Attachments    []string `json:"attachments,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		BadRequest(w, "invalid request body")
		return
	}
	if len(body.Attachments) > 0 {
		ValidationError(w, "attachments cannot be scheduled", nil)
		return
	}
	content, _, serr := s.validateChatSendInput(ctx, target, chatSendInput{Content: body.Content})
	if serr != nil {
		serr.write(w)
		return
	}
	fireAt, err := time.Parse(time.RFC3339, body.FireAt)
	if err != nil {
		ValidationError(w, "fire_at must be an RFC 3339 timestamp", nil)
		return
	}
	now := time.Now().UTC()
	fireAt = fireAt.UTC()
	if !fireAt.After(now) {
		ValidationError(w, "fire_at must be in the future", nil)
		return
	}
	idemKey := body.IdempotencyKey
	if idemKey == "" {
		idemKey = api.NewUUID()
	}

	row, existed, err := sms.CreateScheduledMessage(ctx, &ScheduledChatMessage{
		ID:              api.NewUUID(),
		SenderUserID:    user.ID(),
		ConversationKey: key,
		ProjectID:       target.ProjectID,
		Content:         content,
		ReplyToID:       body.ReplyToID,
		IdempotencyKey:  idemKey,
		FireAt:          fireAt,
		Status:          ScheduledMessagePending,
		CreatedAt:       now,
		UpdatedAt:       now,
	})
	if err != nil {
		slog.Error("scheduled send: create failed", "error", err)
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to schedule message", nil)
		return
	}
	if existed {
		// A retry of a create that already succeeded.
		writeJSON(w, http.StatusOK, newScheduledMessageResponse(row))
		return
	}
	auditScheduledMessage(ctx, "create", row, "ok")
	s.publishScheduledMessage(ctx, "created", row)
	writeJSON(w, http.StatusCreated, newScheduledMessageResponse(row))
}

// handleScheduledList implements GET …/{key}/scheduled: the caller's own
// pending, sending and failed messages in the conversation, only while the
// caller can still access it.
func (s *Server) handleScheduledList(w http.ResponseWriter, r *http.Request, key string) {
	user := scheduledSendCaller(w, r)
	if user == nil || scheduledSendRejectDM(w, key) {
		return
	}
	ctx := r.Context()
	target, serr := s.authorizeChatSend(ctx, user, key)
	if serr != nil {
		serr.write(w)
		return
	}
	sms := scheduledMessageStoreFrom(target.wcs)
	if sms == nil {
		writeError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Chat not available", nil)
		return
	}
	rows, err := sms.ListScheduledMessages(ctx, user.ID(), key)
	if err != nil {
		slog.Error("scheduled send: list failed", "error", err)
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to list scheduled messages", nil)
		return
	}
	out := make([]scheduledMessageResponse, 0, len(rows))
	for i := range rows {
		out = append(out, newScheduledMessageResponse(&rows[i]))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"scheduledMessages": out})
}

// handleScheduledCancel implements DELETE …/{key}/scheduled/{id}. Only the
// sender's own pending message can be cancelled; one that is already being
// sent or was sent answers 409.
func (s *Server) handleScheduledCancel(w http.ResponseWriter, r *http.Request, key, id string) {
	user := scheduledSendCaller(w, r)
	if user == nil || scheduledSendRejectDM(w, key) {
		return
	}
	ctx := r.Context()
	sms := s.scheduledMessageStore()
	if sms == nil {
		writeError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Chat not available", nil)
		return
	}
	row, err := sms.GetScheduledMessage(ctx, user.ID(), id)
	if err != nil {
		slog.Error("scheduled send: get failed", "error", err)
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to cancel scheduled message", nil)
		return
	}
	if row == nil || row.ConversationKey != key {
		NotFound(w, "Scheduled message")
		return
	}
	now := time.Now().UTC()
	cancelled, err := sms.CancelScheduledMessage(ctx, user.ID(), id, now)
	if err != nil {
		slog.Error("scheduled send: cancel failed", "error", err)
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to cancel scheduled message", nil)
		return
	}
	if !cancelled {
		// Lost to the sweeper's claim, or not pending to begin with.
		current, err := sms.GetScheduledMessage(ctx, user.ID(), id)
		if err == nil && current != nil && current.Status == ScheduledMessageCancelled {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		status := ""
		if current != nil {
			status = current.Status
		}
		writeError(w, http.StatusConflict, ErrCodeConflict,
			"scheduled message can no longer be cancelled", map[string]interface{}{"status": status})
		return
	}
	row.Status = ScheduledMessageCancelled
	row.UpdatedAt = now
	auditScheduledMessage(ctx, "cancel", row, "ok")
	s.publishScheduledMessage(ctx, "cancelled", row)
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Sweeper
// ---------------------------------------------------------------------------

// startScheduledSendSweeper starts the scheduled-message sweeper. Every
// replica runs one; the claim in the store makes delivery exactly-once
// across them. It stops when ctx is cancelled.
func (s *Server) startScheduledSendSweeper(ctx context.Context) {
	if !s.nativeChatEnabled() {
		return
	}
	go func() {
		ticker := time.NewTicker(scheduledSendTick)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.sweepScheduledMessages(ctx, time.Now().UTC())
			}
		}
	}()
}

// sweepScheduledMessages sends the messages due at now that this replica
// manages to claim, and returns how many it claimed. While the experiment
// is off it does nothing, so pending messages are held, not sent or failed.
func (s *Server) sweepScheduledMessages(ctx context.Context, now time.Time) int {
	if !s.experimentEnabled(experiments.ChatScheduledSend) {
		return 0
	}
	sms := s.scheduledMessageStore()
	if sms == nil {
		return 0
	}
	due, err := sms.ListDueScheduledMessages(ctx, now, scheduledSendBatch)
	if err != nil {
		scheduledSendLog().Warn("scheduled send: listing due messages failed", "error", err)
		return 0
	}
	claimed := 0
	for i := range due {
		if ctx.Err() != nil {
			break
		}
		row := due[i]
		ok, err := sms.ClaimScheduledMessage(ctx, row.ID, time.Now().UTC())
		if err != nil {
			scheduledSendLog().Warn("scheduled send: claim failed", "id", row.ID, "error", err)
			continue
		}
		if !ok {
			continue // cancelled, or another replica claimed it
		}
		claimed++
		row.Status = ScheduledMessageSending
		s.fireScheduledMessage(ctx, sms, &row)
	}
	return claimed
}

// scheduledFireCheck is the outcome of the pre-send checks of a claimed row.
type scheduledFireCheck struct {
	user      UserIdentity
	replyToID string
	// reason is set when the row must fail without sending.
	reason string
	// transient is set when a check could not be completed (a store
	// error); the claim is released and the row retried next tick.
	transient bool
}

// checkScheduledFire runs the fire-time checks of a claimed row, in order:
// the sender exists and is active; the topic exists and its current
// project exists and grants the sender read access; and the reply-to
// message is still in the same conversation (if not, the message is sent
// without it). Nothing stored at schedule time is used to grant access.
func (s *Server) checkScheduledFire(ctx context.Context, m *ScheduledChatMessage) scheduledFireCheck {
	// Sender: the identity is rebuilt from the current user record, never
	// more privileged than a live session for that user.
	u, err := s.store.GetUser(ctx, m.SenderUserID)
	if errors.Is(err, store.ErrNotFound) {
		return scheduledFireCheck{reason: ScheduledFailureSenderInactive}
	}
	if err != nil {
		return scheduledFireCheck{transient: true}
	}
	if u.Status != store.UserStatusActive {
		return scheduledFireCheck{reason: ScheduledFailureSenderInactive}
	}
	user := NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, u.Role, scheduledSendClientType)

	if strings.HasPrefix(m.ConversationKey, "dm:") {
		// Not schedulable in this version; never sent.
		return scheduledFireCheck{reason: ScheduledFailureNoAccess}
	}

	s.mu.RLock()
	wcs := s.webChatStore
	s.mu.RUnlock()
	if wcs == nil {
		return scheduledFireCheck{transient: true}
	}
	topic, err := wcs.GetTopic(ctx, m.ConversationKey)
	if err != nil {
		return scheduledFireCheck{transient: true}
	}
	if topic == nil {
		return scheduledFireCheck{reason: ScheduledFailureConversationGone}
	}
	project, err := s.store.GetProject(ctx, topic.ProjectID)
	if errors.Is(err, store.ErrNotFound) {
		return scheduledFireCheck{reason: ScheduledFailureConversationGone}
	}
	if err != nil || project == nil {
		return scheduledFireCheck{transient: true}
	}
	if !s.authzService.CheckAccess(ctx, user, projectResource(project), ActionRead).Allowed {
		return scheduledFireCheck{reason: ScheduledFailureNoAccess}
	}

	replyToID := m.ReplyToID
	if replyToID != "" {
		refs, err := s.store.GetMessagesByIDs(ctx, []string{replyToID})
		if err != nil {
			return scheduledFireCheck{transient: true}
		}
		if ref := refs[replyToID]; ref == nil || ref.ThreadID != m.ConversationKey {
			replyToID = ""
		}
	}
	return scheduledFireCheck{user: user, replyToID: replyToID}
}

// scheduledFailureFromSendError maps a sendChatMessage error to a failure
// reason.
func scheduledFailureFromSendError(serr *chatSendError) string {
	switch serr.Status {
	case http.StatusForbidden:
		return ScheduledFailureNoAccess
	case http.StatusNotFound:
		return ScheduledFailureConversationGone
	default:
		return ScheduledFailureDeliveryError
	}
}

// fireScheduledMessage delivers a row this replica has claimed. Once
// sendChatMessage has been called the row never returns to pending: it
// ends sent or failed, so a message is sent at most once.
func (s *Server) fireScheduledMessage(ctx context.Context, sms ScheduledMessageStore, m *ScheduledChatMessage) {
	ctx = ContextWithExecutor(ctx, ExecutorContext{Kind: scheduledSendClientType, ID: "scheduled_message:" + m.ID})

	check := s.checkScheduledFire(ctx, m)
	if check.transient {
		if err := sms.ReleaseScheduledMessage(ctx, m.ID, time.Now().UTC()); err != nil {
			scheduledSendLog().Warn("scheduled send: release failed", "id", m.ID, "error", err)
		}
		return
	}
	if check.reason != "" {
		s.failScheduledMessage(ctx, sms, m, check.reason)
		return
	}

	ctx = contextWithIdentity(ctx, check.user)
	var (
		resp *chatMessageResponse
		serr *chatSendError
	)
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				scheduledSendLog().Error("scheduled send: panic during delivery", "id", m.ID, "panic", fmt.Sprint(rec))
				resp, serr = nil, newChatSendError(http.StatusInternalServerError, "INTERNAL", "panic during delivery", nil)
			}
		}()
		// Never an interrupt and never a wake: a scheduled message reaches
		// agents as an ordinary queued message.
		resp, serr = s.sendChatMessage(ctx, check.user, m.ConversationKey, chatSendInput{
			Content:   m.Content,
			ReplyToID: check.replyToID,
		})
	}()
	if serr != nil {
		scheduledSendLog().Info("scheduled send: delivery refused", "id", m.ID, "status", serr.Status, "code", serr.Code)
		s.failScheduledMessage(ctx, sms, m, scheduledFailureFromSendError(serr))
		return
	}

	now := time.Now().UTC()
	if err := sms.MarkScheduledMessageSent(ctx, m.ID, resp.ID, now); err != nil {
		scheduledSendLog().Error("scheduled send: recording sent state failed", "id", m.ID, "message_id", resp.ID, "error", err)
	}
	m.Status = ScheduledMessageSent
	m.MessageID = resp.ID
	m.UpdatedAt = now
	auditScheduledMessage(ctx, "fire", m, "sent")
	s.publishScheduledMessage(ctx, "sent", m)
}

func (s *Server) failScheduledMessage(ctx context.Context, sms ScheduledMessageStore, m *ScheduledChatMessage, reason string) {
	now := time.Now().UTC()
	if err := sms.MarkScheduledMessageFailed(ctx, m.ID, reason, now); err != nil {
		scheduledSendLog().Error("scheduled send: recording failed state failed", "id", m.ID, "error", err)
	}
	m.Status = ScheduledMessageFailed
	m.FailureReason = reason
	m.UpdatedAt = now
	auditScheduledMessage(ctx, "fire", m, "failed:"+reason)
	s.publishScheduledMessage(ctx, "failed", m)
}
