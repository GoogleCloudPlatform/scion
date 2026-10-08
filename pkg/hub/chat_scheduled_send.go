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
	// after a transient error), sent or failed.
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
// The outcome is the row's resulting status, with the failure reason in its
// own field.
func auditScheduledMessage(ctx context.Context, action string, m *ScheduledChatMessage) {
	scheduledSendLog().Info("scheduled chat message",
		"audit_action", "chat.scheduled."+action,
		"principal_type", "user",
		"principal_id", m.SenderUserID,
		"executor", scheduledSendClientType,
		"scheduled_message_id", m.ID,
		"conversation_key", m.ConversationKey,
		"status", m.Status,
		"failure_reason", m.FailureReason,
		"message_id", m.MessageID,
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
	user := scheduledSendCaller(w, r, ActionCreate)
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
	if len(body.ReplyToID) > scheduledMaxReplyToIDLen {
		ValidationError(w, fmt.Sprintf("reply_to_id exceeds %d characters", scheduledMaxReplyToIDLen), nil)
		return
	}
	if len(body.IdempotencyKey) > scheduledMaxIdempotencyKeyLen {
		ValidationError(w, fmt.Sprintf("idempotency_key exceeds %d characters", scheduledMaxIdempotencyKeyLen), nil)
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
		// A concurrent retry with the same idempotency key won the insert.
		s.writeScheduledReplay(w, row, key)
		return
	}
	auditScheduledMessage(ctx, "create", row)
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
// caller can still access it.
func (s *Server) handleScheduledList(w http.ResponseWriter, r *http.Request, key string) {
	user := scheduledSendCaller(w, r, ActionRead)
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
	user := scheduledSendCaller(w, r, ActionDelete)
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
	auditScheduledMessage(ctx, "cancel", row)
	s.publishScheduledMessage(ctx, "cancelled", row)
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
				rt.running.Add(1)
				rt.mu.Unlock()
				go func() {
					defer rt.running.Done()
					s.sweepScheduledMessages(loopCtx, time.Now().UTC())
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
	ok, err := sms.ClaimScheduledMessage(claimCtx, row.ID, time.Now().UTC())
	cancelClaim()
	if err != nil {
		scheduledSendLog().Warn("scheduled send: claim failed", "id", row.ID, "error", err)
		return false, scheduledClaimError
	}
	if !ok {
		return false, scheduledNotClaimed
	}
	row.Status = ScheduledMessageSending
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

// scheduledFailureFromSendError maps a sendChatMessage error at fire time
// to a failure reason. A 404 is a delivery error, not conversation_gone:
// checkScheduledFire has just shown that the topic and its project exist,
// and sendChatMessage also answers 404 for a store error while reading
// them.
func scheduledFailureFromSendError(serr *chatSendError) string {
	if serr.Status == http.StatusForbidden {
		return ScheduledFailureNoAccess
	}
	return ScheduledFailureDeliveryError
}

// scheduledRefusalReason is the failure reason for a send refused at fire
// time. A refusal on a delivery that was cut short (ctx done) says nothing
// about access, so it is a delivery error; the send may have started, so
// it is not retried.
func scheduledRefusalReason(ctx context.Context, serr *chatSendError) string {
	if ctx.Err() != nil {
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
	stopAbort := context.AfterFunc(s.scheduledRuntime().abortCtx, cancel)
	defer stopAbort()

	check := s.checkScheduledFire(ctx, m)
	// Cut short (shutdown) during or right after the checks: nothing was
	// sent, and a check may have failed only because of that, so the row
	// goes back to pending rather than failing.
	if ctx.Err() != nil {
		check.transient = true
	}
	if check.transient {
		fctx, fcancel := finalizeContext(base)
		defer fcancel()
		now := time.Now().UTC()
		if err := sms.ReleaseScheduledMessage(fctx, m.ID, now); err != nil {
			scheduledSendLog().Warn("scheduled send: release failed", "id", m.ID, "error", err)
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
		s.failScheduledMessage(base, sms, m, scheduledRefusalReason(ctx, serr))
		return false
	}

	now := time.Now().UTC()
	fctx, fcancel := finalizeContext(base)
	defer fcancel()
	if err := sms.MarkScheduledMessageSent(fctx, m.ID, resp.ID, now); err != nil {
		scheduledSendLog().Error("scheduled send: recording sent state failed", "id", m.ID, "message_id", resp.ID, "error", err)
	}
	m.Status = ScheduledMessageSent
	m.MessageID = resp.ID
	m.UpdatedAt = now
	auditScheduledMessage(base, "fire", m)
	s.publishScheduledMessage(base, "sent", m)
	return false
}

// failScheduledMessage records a claimed row as failed, on a fresh context
// derived from base (see finalizeContext).
func (s *Server) failScheduledMessage(base context.Context, sms ScheduledMessageStore, m *ScheduledChatMessage, reason string) {
	ctx, cancel := finalizeContext(base)
	defer cancel()
	now := time.Now().UTC()
	if err := sms.MarkScheduledMessageFailed(ctx, m.ID, reason, now); err != nil {
		scheduledSendLog().Error("scheduled send: recording failed state failed", "id", m.ID, "error", err)
	}
	m.Status = ScheduledMessageFailed
	m.FailureReason = reason
	m.UpdatedAt = now
	auditScheduledMessage(ctx, "fire", m)
	s.publishScheduledMessage(ctx, "failed", m)
}
