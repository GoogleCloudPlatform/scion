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
	"sync"
	"sync/atomic"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// Scheduled send in native web chat (ptone/scion#3666).
//
// A user schedules a message in a topic or direct message; the hub keeps it in
// webchat_scheduled_message (visible only to that user) and a sweeper on
// every hub replica sends it at fire time through sendChatMessage, the same
// function the live send handler uses, as an ordinary message from the
// user. A compare-and-set claim (pending -> sending) makes exactly one
// replica deliver each row. Nothing is decided from what was true at
// schedule time: the sender, the conversation (a topic and its project, or
// a DM and its peer) and the sender's access are all checked again at fire
// time.
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
	// scheduledClaimTimeout bounds the claim of one row, and
	// scheduledFinalizeTimeout each final state write (release, sent,
	// failed). Both run on fresh contexts detached from shutdown and from
	// the delivery bound, so a claimed row is always released or finalized.
	scheduledClaimTimeout    = 10 * time.Second
	scheduledFinalizeTimeout = 10 * time.Second
	// scheduledMaxActivePerSender caps a sender's pending and sending
	// messages across all conversations. Delivery at fire time is not
	// rate limited; this cap is what bounds it.
	scheduledMaxActivePerSender = 50
	// scheduledMinLead and scheduledMaxHorizon bound fire_at at create.
	scheduledMinLead    = 60 * time.Second
	scheduledMaxHorizon = 90 * 24 * time.Hour
	// Length limits for client-supplied identifiers.
	scheduledMaxReplyToIDLen      = 128
	scheduledMaxIdempotencyKeyLen = 255
)

// scheduledLateCutoff is how late a message may still be sent. A message
// found due later than this (the hub was down, or the experiment was off)
// fails as missed instead, and the sender may ask to send it now.
const scheduledLateCutoff = 60 * time.Minute

// scheduledMissed reports whether m is more than scheduledLateCutoff past
// its fire time at now.
func scheduledMissed(m *ScheduledChatMessage, now time.Time) bool {
	return now.Sub(m.FireAt) > scheduledLateCutoff
}

// scheduledDeliveryBudget bounds the fire-time checks and the send of one
// claimed message. A send dispatches to the primary agent and each
// @mentioned agent one after another, each bounded by chatWakeDeliveryBudget
// (as on the live path, where the request context has no deadline), so the
// budget covers that worst case plus a margin for the checks and
// persistence: a scheduled send is never cut shorter than a live one. It
// is a variable so tests can shorten it.
var scheduledDeliveryBudget = time.Duration(1+messages.MaxMentionRecipients)*chatWakeDeliveryBudget + 30*time.Second

// ErrCodeScheduledLimit is returned when a sender already has the maximum
// number of pending scheduled messages.
const ErrCodeScheduledLimit = "scheduled_limit_reached"

// ChatScheduledEvent is published to the sender on
// user.<id>.chat.scheduled when one of their scheduled messages changes.
type ChatScheduledEvent struct {
	// Action is created, cancelled, sending, released (back to pending
	// after a transient error), sent, failed, requeued (pending again,
	// due now, at the sender's request) or dismissed.
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
		case strings.HasSuffix(id, "/send-now") && scheduledRowID(id, "/send-now") != "":
			if r.Method != http.MethodPost {
				MethodNotAllowed(w, http.MethodPost)
				return
			}
			s.handleScheduledSendNow(w, r, key, scheduledRowID(id, "/send-now"))
		case strings.HasSuffix(id, "/dismiss") && scheduledRowID(id, "/dismiss") != "":
			if r.Method != http.MethodPost {
				MethodNotAllowed(w, http.MethodPost)
				return
			}
			s.handleScheduledDismiss(w, r, key, scheduledRowID(id, "/dismiss"))
		default:
			http.NotFound(w, r)
		}
	})(w, r)
}

// scheduledSendCaller returns the caller of a scheduled-message route, or
// writes the refusal. Only an interactive, unscoped user session may use
// these routes (design §2.4): the credential must be an interactive (or
// local dev) session, and scoped access tokens, federated identities,
// broker requests on behalf of a user and agents are refused. A message is
// sent later as the user, and only such a session matches that. action is
// the route's action, for the denial log.
func scheduledSendCaller(w http.ResponseWriter, r *http.Request, action Action) UserIdentity {
	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	user := GetUserIdentityFromContext(ctx)
	if user == nil || user.ID() == "" {
		Forbidden(w)
		return nil
	}
	deny := func(reason string) UserIdentity {
		logAuthzDenial(r, identity, Resource{Type: "chat_scheduled_message"}, action, reason)
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"scheduled messages require a signed-in session", nil)
		return nil
	}
	switch GetCredentialContextFromContext(ctx).Kind {
	case CredentialKindInteractive, CredentialKindDev:
	default:
		return deny("credential kind may not schedule chat messages")
	}
	if IsScopedUserIdentity(user) {
		return deny("scoped user access token")
	}
	if _, federated := user.(FederatedIdentity); federated {
		return deny("federated identity")
	}
	return user
}

// handleScheduledCreate implements POST …/{key}/scheduled.
func (s *Server) handleScheduledCreate(w http.ResponseWriter, r *http.Request, key string) {
	user := scheduledSendCaller(w, r, ActionCreate)
	if user == nil {
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
	if len(body.ReplyToID) > scheduledMaxReplyToIDLen {
		ValidationError(w, fmt.Sprintf("reply_to_id exceeds %d characters", scheduledMaxReplyToIDLen), nil)
		return
	}
	if len(body.IdempotencyKey) > scheduledMaxIdempotencyKeyLen {
		ValidationError(w, fmt.Sprintf("idempotency_key exceeds %d characters", scheduledMaxIdempotencyKeyLen), nil)
		return
	}
	content, _, serr := s.validateChatSendInput(ctx, user, target, chatSendInput{Content: body.Content})
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
	idemKey := body.IdempotencyKey
	if idemKey == "" {
		idemKey = api.NewUUID()
	} else {
		// A retry of a create that already succeeded answers with that row,
		// before the time and count checks, which it passed then.
		existing, err := sms.GetScheduledMessageByIdempotencyKey(ctx, user.ID(), idemKey)
		if err != nil {
			slog.Error("scheduled send: idempotency lookup failed", "error", err)
			writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to schedule message", nil)
			return
		}
		if existing != nil {
			s.writeScheduledReplay(w, existing, key)
			return
		}
	}
	if fireAt.Before(now.Add(scheduledMinLead)) {
		ValidationError(w, "fire_at must be at least 60 seconds in the future", nil)
		return
	}
	if fireAt.After(now.Add(scheduledMaxHorizon)) {
		ValidationError(w, "fire_at must be within 90 days", nil)
		return
	}
	active, err := sms.CountActiveScheduledMessages(ctx, user.ID())
	if err != nil {
		slog.Error("scheduled send: count failed", "error", err)
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to schedule message", nil)
		return
	}
	if active >= scheduledMaxActivePerSender {
		writeError(w, http.StatusConflict, ErrCodeScheduledLimit,
			fmt.Sprintf("you already have %d scheduled messages; cancel one or wait for it to be sent", scheduledMaxActivePerSender),
			map[string]interface{}{"limit": scheduledMaxActivePerSender})
		return
	}

	// The stored project is for cleanup and filtering only: the topic's
	// project, or an agent DM's agent's project (user DMs have none).
	projectID := target.ProjectID
	if target.IsDM {
		projectID = resolveProjectFromDMKey(ctx, s, key)
	}
	row, existed, err := sms.CreateScheduledMessage(ctx, &ScheduledChatMessage{
		ID:              api.NewUUID(),
		SenderUserID:    user.ID(),
		ConversationKey: key,
		ProjectID:       projectID,
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
		// A concurrent retry with the same idempotency key won the insert.
		s.writeScheduledReplay(w, row, key)
		return
	}
	s.auditScheduledMessage(ctx, ScheduledAuditCreate, row)
	s.publishScheduledMessage(ctx, "created", row)
	writeJSON(w, http.StatusCreated, newScheduledMessageResponse(row))
}

// writeScheduledReplay answers a create whose idempotency key the sender
// already used: 200 with that row, or 409 if it belongs to a different
// conversation.
func (s *Server) writeScheduledReplay(w http.ResponseWriter, row *ScheduledChatMessage, key string) {
	if row.ConversationKey != key {
		writeError(w, http.StatusConflict, ErrCodeConflict,
			"idempotency_key was already used for a message in another conversation", nil)
		return
	}
	writeJSON(w, http.StatusOK, newScheduledMessageResponse(row))
}

// handleScheduledList implements GET …/{key}/scheduled: the caller's own
// pending, sending and failed messages in the conversation, only while the
// caller can still read the topic, or is a participant of the DM.
func (s *Server) handleScheduledList(w http.ResponseWriter, r *http.Request, key string) {
	user := scheduledSendCaller(w, r, ActionRead)
	if user == nil {
		return
	}
	ctx := r.Context()
	var sms ScheduledMessageStore
	if strings.HasPrefix(key, "dm:") {
		// A DM lists for its participants with the first two steps of
		// authorizeChatSend (the same helper), so the responses for an
		// invalid key or a caller who is not a participant match a live
		// send. The peer check is not run: a message that failed because
		// the peer changed stays visible to its sender so it can be
		// dismissed or copied. A well-formed but non-canonical key lists
		// nothing, since rows exist only under canonical keys (create runs
		// the full check), and the rows are the caller's own, as for
		// cancel and dismiss.
		if serr := authorizeDMKeyParticipant(ctx, user, key); serr != nil {
			serr.write(w)
			return
		}
		sms = s.scheduledMessageStore()
	} else {
		target, serr := s.authorizeChatSend(ctx, user, key)
		if serr != nil {
			serr.write(w)
			return
		}
		sms = scheduledMessageStoreFrom(target.wcs)
	}
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
	user := scheduledSendCaller(w, r, ActionDelete)
	if user == nil {
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
	s.auditScheduledMessage(ctx, ScheduledAuditCancel, row)
	s.publishScheduledMessage(ctx, "cancelled", row)
	w.WriteHeader(http.StatusNoContent)
}

// scheduledRowID returns the row ID of a "{id}{suffix}" path segment, or
// "" when it is not one.
func scheduledRowID(rest, suffix string) string {
	id := strings.TrimSuffix(rest, suffix)
	if id == "" || strings.Contains(id, "/") {
		return ""
	}
	return id
}

// handleScheduledSendNow implements POST …/{key}/scheduled/{id}/send-now:
// the sender asks again for a message that failed as missed or interrupted.
// It is handled like a new schedule: the same caller and conversation
// access checks, send allowance, content validation and pending cap as
// POST …/scheduled. The row then becomes pending, due now, and the sweeper
// runs the full set of fire-time checks before sending it.
func (s *Server) handleScheduledSendNow(w http.ResponseWriter, r *http.Request, key, id string) {
	user := scheduledSendCaller(w, r, ActionUpdate)
	if user == nil {
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
	if !s.allowChatSend(w, user.ID(), chatSenderHuman) {
		return
	}

	row, err := sms.GetScheduledMessage(ctx, user.ID(), id)
	if err != nil {
		slog.Error("scheduled send: get failed", "error", err)
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to send scheduled message", nil)
		return
	}
	if row == nil || row.ConversationKey != key {
		NotFound(w, "Scheduled message")
		return
	}
	if !scheduledSendNowAllowed(row) {
		writeError(w, http.StatusConflict, ErrCodeConflict,
			"only a missed or interrupted message can be sent now",
			map[string]interface{}{"status": row.Status, "failureReason": row.FailureReason})
		return
	}
	if _, _, serr := s.validateChatSendInput(ctx, user, target, chatSendInput{Content: row.Content}); serr != nil {
		serr.write(w)
		return
	}
	active, err := sms.CountActiveScheduledMessages(ctx, user.ID())
	if err != nil {
		slog.Error("scheduled send: count failed", "error", err)
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to send scheduled message", nil)
		return
	}
	if active >= scheduledMaxActivePerSender {
		writeError(w, http.StatusConflict, ErrCodeScheduledLimit,
			fmt.Sprintf("you already have %d scheduled messages; cancel one or wait for it to be sent", scheduledMaxActivePerSender),
			map[string]interface{}{"limit": scheduledMaxActivePerSender})
		return
	}

	now := time.Now().UTC()
	ok, err := sms.SendNowScheduledMessage(ctx, user.ID(), id, now, now)
	if err != nil {
		slog.Error("scheduled send: send now failed", "error", err)
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to send scheduled message", nil)
		return
	}
	if !ok {
		writeError(w, http.StatusConflict, ErrCodeConflict,
			"only a missed or interrupted message can be sent now", nil)
		return
	}
	updated, err := sms.GetScheduledMessage(ctx, user.ID(), id)
	if err != nil || updated == nil {
		slog.Error("scheduled send: reading row after send now failed", "error", err)
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to send scheduled message", nil)
		return
	}
	s.auditScheduledMessage(ctx, ScheduledAuditSendNow, updated)
	s.publishScheduledMessage(ctx, "requeued", updated)
	writeJSON(w, http.StatusOK, newScheduledMessageResponse(updated))
}

// scheduledSendNowAllowed reports whether the sender may ask for m to be
// sent now: only a message that failed as missed or interrupted.
func scheduledSendNowAllowed(m *ScheduledChatMessage) bool {
	return m.Status == ScheduledMessageFailed &&
		(m.FailureReason == ScheduledFailureMissed || m.FailureReason == ScheduledFailureInterrupted)
}

// handleScheduledDismiss implements POST …/{key}/scheduled/{id}/dismiss:
// the sender removes a failed message from the thread (it becomes
// cancelled and is purged with the other final rows). Like cancel, it
// needs no conversation access, so a message that failed for lack of
// access can still be dismissed.
func (s *Server) handleScheduledDismiss(w http.ResponseWriter, r *http.Request, key, id string) {
	user := scheduledSendCaller(w, r, ActionUpdate)
	if user == nil {
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
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to dismiss scheduled message", nil)
		return
	}
	if row == nil || row.ConversationKey != key {
		NotFound(w, "Scheduled message")
		return
	}
	now := time.Now().UTC()
	dismissed, err := sms.DismissScheduledMessage(ctx, user.ID(), id, now)
	if err != nil {
		slog.Error("scheduled send: dismiss failed", "error", err)
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to dismiss scheduled message", nil)
		return
	}
	if !dismissed {
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
			"only a failed scheduled message can be dismissed", map[string]interface{}{"status": status})
		return
	}
	row.Status = ScheduledMessageCancelled
	row.UpdatedAt = now
	s.auditScheduledMessage(ctx, ScheduledAuditDismiss, row)
	s.publishScheduledMessage(ctx, "dismissed", row)
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Sweeper
// ---------------------------------------------------------------------------

// scheduledSendWorkers bounds how many senders' messages one replica
// delivers at the same time. Each sender has at most one message in
// delivery, so a slow message or one sender's batch never holds up other
// senders beyond the busy workers.
const scheduledSendWorkers = 4

// scheduledSendYieldAfter is how many messages a worker delivers for one
// sender before it gives up its slot when that sender has more due and
// another sender is waiting for a worker. A sender that yielded is listed
// after the others on the next sweep, so more senders with long batches
// than there are workers take turns, and a sender with one message gets a
// slot within a sweep or two. Without contention a worker keeps going.
const scheduledSendYieldAfter = 5

// scheduledStopGrace is how long stopping the sweeper lets deliveries in
// progress run before cutting them short (see stopScheduledSendSweeper). It
// is a variable so tests can shorten it.
var scheduledStopGrace = 15 * time.Second

// scheduledSendRuntime is the sweeper's state on one replica.
//
// The runtime is single-use: once stopped it stays stopped (the abort
// context is never renewed), and a later start or sweep does nothing.
type scheduledSendRuntime struct {
	mu sync.Mutex
	// stopped is set by stopScheduledSendSweeper; start and sweeps check it
	// under mu, so no work starts after a stop has begun.
	stopped bool
	// inFlight holds the senders with a message being claimed or delivered.
	inFlight map[string]struct{}
	// yielded holds the senders whose worker gave up its slot with
	// messages still due (scheduledSendYieldAfter); they are listed last
	// until they get a slot again or have nothing due.
	yielded map[string]struct{}
	// waiting is set when a sweep skipped a sender because all workers
	// were busy, and reset at the start of each sweep. Workers yield only
	// while it is set, so a sender with many due messages and no one
	// waiting keeps its worker.
	waiting bool
	// workers is a semaphore of scheduledSendWorkers slots.
	workers chan struct{}
	// running tracks the ticker loop and every sweep and delivery.
	running sync.WaitGroup
	// stopLoop stops the ticker loop; nil until started or once stopped.
	stopLoop context.CancelFunc
	// abortCtx is cancelled when stopping gives up waiting: deliveries in
	// progress are cut short, then finalized on their own contexts.
	abortCtx context.Context
	abort    context.CancelFunc
	// upkeepBusy is set while an upkeep pass runs (scheduledUpkeep);
	// lastPurge, under mu, is when this replica last purged old rows.
	upkeepBusy atomic.Bool
	lastPurge  time.Time
	// afterAbort registers the cut-short of one delivery on abortCtx; nil
	// means context.AfterFunc. Test-only: tests set it to stall that
	// propagation; it is never set in production.
	afterAbort func(ctx context.Context, f func()) (stop func() bool)
}

// aborted reports whether stopping has cut deliveries short. It reads
// abortCtx directly: the cancel that context.AfterFunc propagates to a
// delivery context runs on its own goroutine and may land later.
func (rt *scheduledSendRuntime) aborted() bool {
	return rt.abortCtx.Err() != nil
}

// onAbort runs f once abortCtx is cancelled (see afterAbort).
func (rt *scheduledSendRuntime) onAbort(f func()) (stop func() bool) {
	rt.mu.Lock()
	register := rt.afterAbort
	rt.mu.Unlock()
	if register == nil {
		register = context.AfterFunc
	}
	return register(rt.abortCtx, f)
}

// scheduledRuntime returns the replica's sweeper state, creating it once.
func (s *Server) scheduledRuntime() *scheduledSendRuntime {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.scheduledSend == nil {
		abortCtx, abort := context.WithCancel(context.Background())
		s.scheduledSend = &scheduledSendRuntime{
			inFlight: make(map[string]struct{}),
			yielded:  make(map[string]struct{}),
			workers:  make(chan struct{}, scheduledSendWorkers),
			abortCtx: abortCtx,
			abort:    abort,
		}
	}
	return s.scheduledSend
}

// startScheduledSendSweeper starts the scheduled-message sweeper. Every
// replica runs one; the claim in the store makes each message delivered by
// one replica only. Each tick starts a sweep without waiting for earlier
// ones, so a slow delivery does not delay the next tick. It stops when ctx
// is cancelled or CleanupResources runs.
func (s *Server) startScheduledSendSweeper(ctx context.Context) {
	if !s.nativeChatEnabled() {
		return
	}
	rt := s.scheduledRuntime()
	loopCtx, cancel := context.WithCancel(ctx)
	rt.mu.Lock()
	if rt.stopped {
		rt.mu.Unlock()
		cancel()
		return
	}
	rt.stopLoop = cancel
	rt.running.Add(1)
	rt.mu.Unlock()
	go func() {
		defer rt.running.Done()
		ticker := time.NewTicker(scheduledSendTick)
		defer ticker.Stop()
		for {
			select {
			case <-loopCtx.Done():
				return
			case <-ticker.C:
				rt.mu.Lock()
				if rt.stopped {
					rt.mu.Unlock()
					return
				}
				rt.running.Add(2)
				rt.mu.Unlock()
				now := time.Now().UTC()
				go func() {
					defer rt.running.Done()
					s.sweepScheduledMessages(loopCtx, now)
				}()
				go func() {
					defer rt.running.Done()
					s.scheduledUpkeep(loopCtx, now)
				}()
			}
		}
	}()
}

// stopScheduledSendSweeper stops the sweeper and lets deliveries in progress
// finish for up to scheduledStopGrace (or until ctx ends, if sooner). It
// then cuts them short: a dispatch in progress returns, the message is
// recorded as it would be after any dispatch error, and the row is
// finalized sent or failed on its own context. Stopping therefore takes at
// most scheduledStopGrace + scheduledFinalizeTimeout (about 25 s), and never
// longer than ctx allows. The runtime cannot be started again afterwards.
func (s *Server) stopScheduledSendSweeper(ctx context.Context) {
	rt := s.scheduledRuntime()
	rt.mu.Lock()
	rt.stopped = true
	stop := rt.stopLoop
	rt.stopLoop = nil
	rt.mu.Unlock()
	if stop != nil {
		stop()
	}
	done := make(chan struct{})
	go func() {
		rt.running.Wait()
		close(done)
	}()
	grace := time.NewTimer(scheduledStopGrace)
	defer grace.Stop()
	select {
	case <-done:
		return
	case <-ctx.Done():
	case <-grace.C:
	}
	rt.abort()
	final := time.NewTimer(scheduledFinalizeTimeout)
	defer final.Stop()
	select {
	case <-done:
	case <-ctx.Done():
		scheduledSendLog().Warn("scheduled send: shutdown deadline reached while deliveries were finishing")
	case <-final.C:
		scheduledSendLog().Warn("scheduled send: deliveries did not finish after being cut short")
	}
}

// waitScheduledDeliveries waits for every sweep and delivery started on
// this replica (tests).
func (s *Server) waitScheduledDeliveries() {
	s.scheduledRuntime().running.Wait()
}

// sweepScheduledMessages delivers the messages due at now that this
// replica manages to claim, and returns how many it claimed. The due list
// has one row per sender (the sender's oldest), so senders with many due
// messages cannot fill it. Each sender gets one worker, which delivers
// that sender's due messages one after another, fetching the next one
// after each delivery; at most scheduledSendWorkers senders are delivered
// at a time, and a sender that already has a delivery in progress (from
// an earlier tick) is skipped until it finishes. The experiment is checked
// before each claim; while it is off nothing is claimed, so pending
// messages are held, not sent or failed.
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

	rt := s.scheduledRuntime()
	due = rt.yieldedLast(due)
	rt.mu.Lock()
	rt.waiting = false
	rt.mu.Unlock()
	var claimed atomic.Int64
	var batch sync.WaitGroup
	for i := range due {
		if ctx.Err() != nil {
			break
		}
		first := due[i]
		sender := first.SenderUserID
		rt.mu.Lock()
		if rt.stopped {
			rt.mu.Unlock()
			break
		}
		_, busy := rt.inFlight[sender]
		if !busy {
			select {
			case rt.workers <- struct{}{}:
				rt.inFlight[sender] = struct{}{}
				delete(rt.yielded, sender)
				batch.Add(1)
				rt.running.Add(1)
			default:
				busy = true // all workers busy: try again next tick
				rt.waiting = true
			}
		}
		rt.mu.Unlock()
		if busy {
			continue
		}
		go func() {
			defer rt.running.Done()
			defer batch.Done()
			defer func() {
				rt.mu.Lock()
				delete(rt.inFlight, sender)
				rt.mu.Unlock()
				<-rt.workers
			}()
			claimed.Add(int64(s.deliverSenderDue(ctx, rt, sms, &first, now)))
		}()
	}
	batch.Wait()
	return int(claimed.Load())
}

// yieldedLast returns due with the senders that recently yielded their
// worker moved after the others, keeping the order within each group. When
// due is the complete list (shorter than scheduledSendBatch), senders not
// in it have nothing due and their yielded mark is dropped.
func (rt *scheduledSendRuntime) yieldedLast(due []ScheduledChatMessage) []ScheduledChatMessage {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.yielded) == 0 {
		return due
	}
	if len(due) < scheduledSendBatch {
		present := make(map[string]bool, len(due))
		for _, row := range due {
			present[row.SenderUserID] = true
		}
		for sender := range rt.yielded {
			if !present[sender] {
				delete(rt.yielded, sender)
			}
		}
	}
	out := make([]ScheduledChatMessage, 0, len(due))
	var later []ScheduledChatMessage
	for _, row := range due {
		if _, y := rt.yielded[row.SenderUserID]; y {
			later = append(later, row)
		} else {
			out = append(out, row)
		}
	}
	return append(out, later...)
}

// deliverSenderDue delivers one sender's due messages one after another,
// starting with row, and returns how many it claimed. It stops when the
// sweeper stops, the experiment is turned off, a claim fails, a row is
// handed back to pending (it is retried next tick), or the sender has no
// more due messages; after scheduledSendYieldAfter claims with more due, it
// yields its slot (see scheduledSendYieldAfter).
func (s *Server) deliverSenderDue(ctx context.Context, rt *scheduledSendRuntime, sms ScheduledMessageStore, row *ScheduledChatMessage, now time.Time) int {
	claimed := 0
	attempted := make(map[string]bool)
	for row != nil && !attempted[row.ID] {
		if ctx.Err() != nil || !s.experimentEnabled(experiments.ChatScheduledSend) {
			return claimed
		}
		attempted[row.ID] = true
		ok, outcome := s.claimAndFire(ctx, sms, row)
		if ok {
			claimed++
		}
		if outcome == scheduledClaimError || outcome == scheduledReleased {
			return claimed
		}
		next, err := sms.NextDueScheduledMessage(ctx, row.SenderUserID, now)
		if err != nil {
			scheduledSendLog().Warn("scheduled send: fetching next due message failed", "error", err)
			return claimed
		}
		if next != nil && claimed >= scheduledSendYieldAfter {
			rt.mu.Lock()
			yield := rt.waiting // only when another sender is waiting for a worker
			if yield {
				rt.yielded[next.SenderUserID] = struct{}{}
			}
			rt.mu.Unlock()
			if yield {
				return claimed
			}
		}
		row = next
	}
	return claimed
}

// scheduledFireOutcome is how claimAndFire ended for one row.
type scheduledFireOutcome int

const (
	scheduledNotClaimed scheduledFireOutcome = iota // cancelled, or claimed elsewhere
	scheduledClaimError                             // the claim could not be made
	scheduledFinalized                              // sent or failed
	scheduledReleased                               // handed back to pending
)

// claimAndFire claims one due row and, if this replica won the claim,
// delivers it. It reports whether the row was claimed and how it ended.
func (s *Server) claimAndFire(ctx context.Context, sms ScheduledMessageStore, row *ScheduledChatMessage) (bool, scheduledFireOutcome) {
	// The claim, once started, is not cut short by shutdown either: a
	// claim that commits must be followed by delivery or release.
	claimCtx, cancelClaim := context.WithTimeout(context.WithoutCancel(ctx), scheduledClaimTimeout)
	claimedAt := time.Now().UTC()
	ok, err := sms.ClaimScheduledMessage(claimCtx, row.ID, claimedAt)
	cancelClaim()
	if err != nil {
		scheduledSendLog().Warn("scheduled send: claim failed", "id", row.ID, "error", err)
		return false, scheduledClaimError
	}
	if !ok {
		return false, scheduledNotClaimed
	}
	row.Status = ScheduledMessageSending
	row.ClaimedAt = &claimedAt
	s.publishScheduledMessage(ctx, "sending", row)
	if s.fireScheduledMessage(ctx, sms, row) {
		return true, scheduledReleased
	}
	return true, scheduledFinalized
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
// the row is not more than scheduledLateCutoff late; the sender exists and
// is active; the conversation checks (checkScheduledDMFire for a direct
// message, checkScheduledTopicFire for a topic); and the reply-to message
// is still in the same conversation (if not, the message is sent without
// it). Nothing stored at schedule time is used to grant access.
func (s *Server) checkScheduledFire(ctx context.Context, m *ScheduledChatMessage) scheduledFireCheck {
	// Too late: sending now could surprise everyone in the conversation,
	// so it is not sent; the sender can still ask to send it now.
	if scheduledMissed(m, time.Now()) {
		return scheduledFireCheck{reason: ScheduledFailureMissed}
	}

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
		if check, done := s.checkScheduledDMFire(ctx, user, m); done {
			return check
		}
	} else if check, done := s.checkScheduledTopicFire(ctx, user, m); done {
		return check
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

// checkScheduledTopicFire runs the topic checks of a claimed row: the
// topic exists, and its current project exists and grants the sender read
// access. done is false when every check passed.
func (s *Server) checkScheduledTopicFire(ctx context.Context, user UserIdentity, m *ScheduledChatMessage) (scheduledFireCheck, bool) {
	s.mu.RLock()
	wcs := s.webChatStore
	s.mu.RUnlock()
	if wcs == nil {
		return scheduledFireCheck{transient: true}, true
	}
	topic, err := wcs.GetTopic(ctx, m.ConversationKey)
	if err != nil {
		return scheduledFireCheck{transient: true}, true
	}
	if topic == nil {
		return scheduledFireCheck{reason: ScheduledFailureConversationGone}, true
	}
	project, err := s.store.GetProject(ctx, topic.ProjectID)
	if errors.Is(err, store.ErrNotFound) {
		return scheduledFireCheck{reason: ScheduledFailureConversationGone}, true
	}
	if err != nil || project == nil {
		return scheduledFireCheck{transient: true}, true
	}
	if !s.authzService.CheckAccess(ctx, user, projectResource(project), ActionRead).Allowed {
		return scheduledFireCheck{reason: ScheduledFailureNoAccess}, true
	}
	return scheduledFireCheck{}, false
}

// checkScheduledDMFire runs the direct-message checks of a claimed row
// with the same conversation access check as a live send
// (authorizeChatSend): a well-formed key naming the sender, and a peer
// that still exists and that the sender may still message. A refusal is
// no_access, whichever check refused it, as on the live path. An agent
// peer that was deleted but can still be addressed is recipient_gone:
// nothing is sent to it. done is false when every check passed.
func (s *Server) checkScheduledDMFire(ctx context.Context, user UserIdentity, m *ScheduledChatMessage) (scheduledFireCheck, bool) {
	if _, serr := s.authorizeChatSend(ctx, user, m.ConversationKey); serr != nil {
		// A refusal of the sender's access is answered as not found but
		// marked accessRefused; it is no_access, like a 403.
		switch {
		case serr.Status == http.StatusServiceUnavailable:
			return scheduledFireCheck{transient: true}, true
		case serr.Status == http.StatusForbidden || serr.accessRefused:
			return scheduledFireCheck{reason: ScheduledFailureNoAccess}, true
		default:
			return scheduledFireCheck{reason: ScheduledFailureConversationGone}, true
		}
	}
	if agentID := parseAgentDMKey(m.ConversationKey); agentID != "" {
		agent, err := s.store.GetAgent(ctx, agentID)
		if errors.Is(err, store.ErrNotFound) || (err == nil && agent == nil) {
			return scheduledFireCheck{reason: ScheduledFailureNoAccess}, true
		}
		if err != nil {
			return scheduledFireCheck{transient: true}, true
		}
		if !agent.DeletedAt.IsZero() {
			return scheduledFireCheck{reason: ScheduledFailureRecipientGone}, true
		}
	}
	return scheduledFireCheck{}, false
}

// scheduledFailureFromSendError maps a sendChatMessage error at fire time
// to a failure reason. sendChatMessage answers a refusal of the sender's
// access as not found (404); such a refusal is marked accessRefused and is
// no_access, like a 403. Any other 404 is a delivery error, not
// conversation_gone: the conversation checks have just passed, and
// sendChatMessage also answers 404 for a store error while reading the
// conversation.
func scheduledFailureFromSendError(serr *chatSendError) string {
	if serr.Status == http.StatusForbidden || serr.accessRefused {
		return ScheduledFailureNoAccess
	}
	return ScheduledFailureDeliveryError
}

// scheduledRefusalReason is the failure reason for a send refused at fire
// time. A refusal on a delivery that was cut short (its context done, or
// the runtime aborted) says nothing about access, so it is a delivery
// error; the send may have started, so it is not retried.
func scheduledRefusalReason(cutShort bool, serr *chatSendError) string {
	if cutShort {
		return ScheduledFailureDeliveryError
	}
	return scheduledFailureFromSendError(serr)
}

// finalizeContext returns a fresh context for one final state write,
// detached from both shutdown and the delivery bound.
func finalizeContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), scheduledFinalizeTimeout)
}

// fireScheduledMessage delivers a row this replica has claimed. Once
// sendChatMessage has been called the row never returns to pending: it
// ends sent or failed, so a message is sent at most once.
//
// The checks and the send run on a context detached from ctx's
// cancellation (server shutdown) and bounded by scheduledDeliveryBudget;
// each final state write gets its own fresh context (finalizeContext), so
// neither a shutdown nor a slow send leaves the row in sending.
//
// It reports whether the row was handed back to pending.
func (s *Server) fireScheduledMessage(ctx context.Context, sms ScheduledMessageStore, m *ScheduledChatMessage) (released bool) {
	base := ContextWithExecutor(context.WithoutCancel(ctx), ExecutorContext{Kind: scheduledSendClientType, ID: "scheduled_message:" + m.ID})
	ctx, cancel := context.WithTimeout(base, scheduledDeliveryBudget)
	defer cancel()
	// Stopping the sweeper cuts a delivery short after its grace period.
	// The propagated cancel runs on its own goroutine, so every decision
	// below also reads the abort directly (rt.aborted) and never depends on
	// when that cancel lands.
	rt := s.scheduledRuntime()
	stopAbort := rt.onAbort(cancel)
	defer stopAbort()
	cutShort := func() bool { return ctx.Err() != nil || rt.aborted() }

	check := s.checkScheduledFire(ctx, m)
	// Cut short (shutdown) during or right after the checks: nothing was
	// sent, and a check may have failed only because of that, so the row
	// goes back to pending rather than failing.
	if cutShort() {
		check.transient = true
	}
	if check.transient {
		fctx, fcancel := finalizeContext(base)
		defer fcancel()
		now := time.Now().UTC()
		ok, err := sms.ReleaseScheduledMessage(fctx, m.ID, scheduledClaimOf(m), now)
		if err != nil {
			scheduledSendLog().Warn("scheduled send: release failed", "id", m.ID, "error", err)
			return
		}
		if !ok {
			scheduledSendLog().Warn("scheduled send: row changed before release; not released", "id", m.ID)
			return
		}
		m.Status = ScheduledMessagePending
		m.ClaimedAt = nil
		m.UpdatedAt = now
		s.publishScheduledMessage(base, "released", m)
		return true
	}
	if check.reason != "" {
		s.failScheduledMessage(base, sms, m, check.reason)
		return false
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
		// A refusal observed after an abort is a delivery error: an aborted
		// delivery says nothing about access.
		s.failScheduledMessage(base, sms, m, scheduledRefusalReason(cutShort(), serr))
		return false
	}

	now := time.Now().UTC()
	fctx, fcancel := finalizeContext(base)
	defer fcancel()
	ok, err := sms.MarkScheduledMessageSent(fctx, m.ID, resp.ID, scheduledClaimOf(m), now)
	if err != nil {
		scheduledSendLog().Error("scheduled send: recording sent state failed", "id", m.ID, "message_id", resp.ID, "error", err)
	} else if !ok {
		// The row is no longer this delivery's (marked interrupted,
		// deleted or claimed again): leave it as it is.
		scheduledSendLog().Warn("scheduled send: row changed during delivery; sent state not recorded",
			"id", m.ID, "message_id", resp.ID)
		return false
	}
	m.Status = ScheduledMessageSent
	m.MessageID = resp.ID
	m.UpdatedAt = now
	s.auditScheduledMessage(base, ScheduledAuditFire, m)
	s.publishScheduledMessage(base, "sent", m)
	return false
}

// scheduledClaimOf returns the claim a final write of m is fenced by (the
// zero time, which matches no row, if m carries none).
func scheduledClaimOf(m *ScheduledChatMessage) time.Time {
	if m.ClaimedAt == nil {
		return time.Time{}
	}
	return *m.ClaimedAt
}

// failScheduledMessage records a claimed row as failed, on a fresh context
// derived from base (see finalizeContext).
func (s *Server) failScheduledMessage(base context.Context, sms ScheduledMessageStore, m *ScheduledChatMessage, reason string) {
	ctx, cancel := finalizeContext(base)
	defer cancel()
	now := time.Now().UTC()
	ok, err := sms.MarkScheduledMessageFailed(ctx, m.ID, reason, scheduledClaimOf(m), now)
	if err != nil {
		scheduledSendLog().Error("scheduled send: recording failed state failed", "id", m.ID, "error", err)
	} else if !ok {
		scheduledSendLog().Warn("scheduled send: row changed during delivery; failed state not recorded",
			"id", m.ID, "failure_reason", reason)
		return
	}
	m.Status = ScheduledMessageFailed
	m.FailureReason = reason
	m.UpdatedAt = now
	s.auditScheduledMessage(ctx, ScheduledAuditFire, m)
	s.publishScheduledMessage(ctx, "failed", m)
}
