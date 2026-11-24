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
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Honest dispatch state for buffered broker delivery (ptone/scion#1820).
//
// The runtime broker accepts non-interrupt messages into a short debounce
// buffer and answers 200 before the message is actually typed into the
// agent's terminal. The hub therefore marks the row "dispatched" before the
// real delivery attempt. When the buffer later fails to deliver (container
// gone, tmux exec error), the broker reports the hub message IDs back through
// POST /api/v1/runtime-brokers/{id}/message-failures so the row can be moved
// to "failed" instead of silently remaining "dispatched".
//
// The hub message ID is carried to the broker out-of-band via the request
// context (never via client-controllable StructuredMessage fields), so only
// hub code paths that explicitly opt in can cause a row to be reported on.

type dispatchMessageIDKey struct{}

// withDispatchMessageID attaches the persisted hub message ID to ctx so that
// broker clients include it on the wire as MessageRequest.message_id.
func withDispatchMessageID(ctx context.Context, messageID string) context.Context {
	if messageID == "" {
		return ctx
	}
	return context.WithValue(ctx, dispatchMessageIDKey{}, messageID)
}

// dispatchMessageIDFromContext returns the hub message ID attached with
// withDispatchMessageID, or "" if none.
func dispatchMessageIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(dispatchMessageIDKey{}).(string)
	return id
}

// maxMessageFailuresPerReport bounds the work a single report can trigger.
//
// Failures are applied sequentially (one GetMessage/GetAgent/markFailed per
// entry): the store has no batch dispatch-state update, and reports are small
// in practice. The runtime broker sends one report per failed buffered
// message as flushes fail, so failures trickle in rather than arriving in
// bulk. The cap keeps a single request's worst-case cost low; a broker with
// more to report can split it across requests.
const maxMessageFailuresPerReport = 50

const messageFailureCodeAwaitingSession = "awaiting_session"

// Hold-until-session bounds (ptone/scion#7881; H2 confirmed). Variables so
// tests can exercise bounds without waiting minutes.
var (
	messageHoldTTL            = 10 * time.Minute
	messageHoldMaxProofAge    = 2 * time.Minute
	maxHeldMessagesPerAgent   = 100
	hubConduitDeliverRuntimes = map[string]bool{
		"cloudrun":           true,
		"cloudrun-instances": true,
	}
)

// messageDeliveryFailure is one failed delivery reported by a broker.
type messageDeliveryFailure struct {
	MessageID string `json:"messageId"`
	AgentID   string `json:"agentId,omitempty"`
	ProjectID string `json:"projectId,omitempty"`
	Code      string `json:"code,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// messageDeliveryConfirmation is one successful session injection reported by
// a broker (ptone/scion#9015).
type messageDeliveryConfirmation struct {
	MessageID string `json:"messageId"`
	AgentID   string `json:"agentId,omitempty"`
	ProjectID string `json:"projectId,omitempty"`
}

// messageDeliveryFailuresRequest is the body of a message-failures report.
type messageDeliveryFailuresRequest struct {
	Failures []messageDeliveryFailure `json:"failures"`
	// Deliveries are hub messages whose session injection succeeded on the
	// broker (ptone/scion#9015). Optional and additive for mixed-version hubs.
	Deliveries []messageDeliveryConfirmation `json:"deliveries,omitempty"`
	// AttachmentOutcomes are the per-file outcomes of attachment deliveries
	// (ptone/scion#4596); logged and counted only.
	AttachmentOutcomes []attachmentDeliveryOutcome `json:"attachmentOutcomes,omitempty"`
}

// attachmentDeliveryOutcome is one file's delivery outcome reported by a
// broker.
type attachmentDeliveryOutcome struct {
	MessageID    string `json:"messageId"`
	AgentID      string `json:"agentId,omitempty"`
	AttachmentID string `json:"attachmentId,omitempty"`
	Outcome      string `json:"outcome"`
	Reason       string `json:"reason,omitempty"`
}

// attachmentOutcomeCounter counts the attachment delivery outcomes brokers
// report, by key (attachmentOutcomeKey).
type attachmentOutcomeCounter struct {
	mu     sync.Mutex
	counts map[string]int64
}

// Inc adds one to key.
func (c *attachmentOutcomeCounter) Inc(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counts[key]++
}

// Snapshot returns a copy of the counts.
func (c *attachmentOutcomeCounter) Snapshot() map[string]int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int64, len(c.counts))
	for k, v := range c.counts {
		out[k] = v
	}
	return out
}

// attachmentDeliveryOutcomes counts the attachment delivery outcomes
// brokers report, by outcome and reason (ptone/scion#4596). It stays
// unexported until a metrics collector reads it.
var attachmentDeliveryOutcomes = &attachmentOutcomeCounter{counts: map[string]int64{}}

// maxAttachmentOutcomesPerReport bounds the outcomes one report logs.
const maxAttachmentOutcomesPerReport = 100

// messageDeliveryFailuresResponse reports how many rows were updated.
type messageDeliveryFailuresResponse struct {
	Marked    int `json:"marked"`
	Delivered int `json:"delivered,omitempty"`
	Held      int `json:"held,omitempty"`
	Ignored   int `json:"ignored"`
}

// handleBrokerMessageFailures processes a broker's report of messages that
// were accepted into its delivery buffer but could not be delivered.
//
// Only the broker itself may report, and only for messages whose recipient
// agent is assigned to that broker. Rows not in pending, dispatched,
// delivering or partial state are left untouched, so a report can never
// resurrect or overwrite a terminal state set by the hub (delivered,
// failed, deferred, no_recipient).
func (s *Server) handleBrokerMessageFailures(w http.ResponseWriter, r *http.Request, brokerID string) {
	ctx := r.Context()

	brokerIdent := GetBrokerIdentityFromContext(ctx)
	if brokerIdent == nil || brokerIdent.BrokerID() != brokerID {
		logAuthzDenial(r, GetIdentityFromContext(ctx), Resource{Type: permissions.ResourceBroker, ID: brokerID}, ActionUpdate,
			"message-failures may only be reported by the broker itself")
		Forbidden(w)
		return
	}

	var req messageDeliveryFailuresRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}
	if len(req.Failures) > maxMessageFailuresPerReport {
		BadRequest(w, "too many failures in one report")
		return
	}
	if len(req.Deliveries) > maxMessageFailuresPerReport {
		BadRequest(w, "too many deliveries in one report")
		return
	}
	for _, f := range req.Failures {
		if f.Code != "" && f.Code != messageFailureCodeAwaitingSession {
			BadRequest(w, "invalid failure code")
			return
		}
	}
	if len(req.AttachmentOutcomes) > maxAttachmentOutcomesPerReport {
		BadRequest(w, "too many attachment outcomes in one report")
		return
	}
	for _, o := range req.AttachmentOutcomes {
		key := attachmentOutcomeKey(o)
		attachmentDeliveryOutcomes.Inc(key)
		s.messageLog.Info("attachment delivery outcome", "broker_id", brokerID, "message_id", o.MessageID,
			"agent_id", o.AgentID, "attachment_id", o.AttachmentID, "outcome", o.Outcome, "reason", o.Reason)
	}

	resp := messageDeliveryFailuresResponse{}
	for _, f := range req.Failures {
		switch s.applyBrokerMessageFailureOutcome(ctx, brokerID, f) {
		case brokerFailureMarked:
			resp.Marked++
		case brokerFailureHeld:
			resp.Held++
		default:
			resp.Ignored++
		}
	}
	for _, d := range req.Deliveries {
		if s.applyBrokerMessageDeliveryOutcome(ctx, brokerID, d) {
			resp.Delivered++
		} else {
			resp.Ignored++
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

type brokerFailureOutcome int

const (
	brokerFailureIgnored brokerFailureOutcome = iota
	brokerFailureMarked
	brokerFailureHeld
)

// agentUsesConduitDelivery reports whether target's runtime uses Conduit
// deliver-message rather than exec. Checks recorded Runtime first, falling
// back to PinnedRuntimeTargetType and broker runtime resolution while an
// agent is still starting before its first heartbeat.
func (s *Server) agentUsesConduitDelivery(ctx context.Context, agent *store.Agent) bool {
	if agent == nil {
		return false
	}
	rt := strings.ToLower(strings.TrimSpace(agent.Runtime))
	if rt == "" {
		rt = strings.ToLower(strings.TrimSpace(agent.PinnedRuntimeTargetType))
	}
	if rt == "" && agent.RuntimeBrokerID != "" && s.store != nil {
		if b, err := s.store.GetRuntimeBroker(ctx, agent.RuntimeBrokerID); err == nil && b != nil {
			if flatCloudRunBroker(b) {
				return true
			}
			if resolved := resolveAgentRuntime(agent, b); resolved != "" {
				rt = strings.ToLower(strings.TrimSpace(resolved))
			} else {
				profileName := ""
				if agent.AppliedConfig != nil {
					profileName = agent.AppliedConfig.Profile
				}
				if _, profType, ok := resolveAgentRuntimeProfileType(b, profileName); ok {
					rt = strings.ToLower(strings.TrimSpace(profType))
				} else if b.RuntimeTarget != nil {
					if _, canonical := flatTargetTypeCheck(b.RuntimeTarget.Type); canonical && b.RuntimeTarget.Type != "" {
						rt = strings.ToLower(strings.TrimSpace(b.RuntimeTarget.Type))
					}
				}
			}
		}
	}
	return hubConduitDeliverRuntimes[rt]
}

// applyBrokerMessageFailureOutcome validates and applies one reported failure.
func (s *Server) applyBrokerMessageFailureOutcome(ctx context.Context, brokerID string, f messageDeliveryFailure) brokerFailureOutcome {
	if f.MessageID == "" {
		return brokerFailureIgnored
	}
	msg, err := s.store.GetMessage(ctx, f.MessageID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			slog.Warn("message-failures: message lookup failed",
				"broker_id", brokerID, "message_id", f.MessageID, "error", err)
		}
		return brokerFailureIgnored
	}

	// Ownership: the recipient agent must be assigned to the reporting broker.
	// Always derive ownership from the stored msg.RecipientID; request body
	// agentId and projectId are informational only and never trusted.
	recipientAgentID := msg.RecipientID
	if recipientAgentID == "" || !strings.HasPrefix(msg.Recipient, "agent:") {
		return brokerFailureIgnored
	}
	agent, err := s.store.GetAgent(ctx, recipientAgentID)
	if err != nil || agent == nil || agent.RuntimeBrokerID != brokerID {
		slog.Warn("message-failures: broker reported failure for message it does not own",
			"broker_id", brokerID, "message_id", f.MessageID)
		return brokerFailureIgnored
	}

	// Hold-until-session CAS (ptone/scion#7881): only when the broker sent
	// code=awaiting_session, hold is enabled, the agent uses conduit delivery,
	// and the store CAS verifies a fresh hub-stamped no_session proof on a
	// dispatched row under the per-agent cap. Otherwise fall through and fail
	// the row as today.
	if f.Code == messageFailureCodeAwaitingSession &&
		msg.DispatchState == store.MessageDispatchDispatched &&
		s.conduitDeliverEnabled() &&
		s.agentUsesConduitDelivery(ctx, agent) {
		finCtx, finCancel := finalizationContext(ctx)
		held, hErr := s.store.HoldMessageAwaitingSession(finCtx, store.HoldMessageParams{
			MessageID:   msg.ID,
			AgentID:     agent.ID,
			MaxProofAge: messageHoldMaxProofAge,
			HoldTTL:     messageHoldTTL,
			MaxHeld:     s.heldMessagesCap(),
		})
		finCancel()
		if hErr != nil {
			slog.Error("message-failures: hold awaiting_session CAS failed",
				"broker_id", brokerID, "message_id", msg.ID, "agent_id", agent.ID, "error", hErr)
		} else if held {
			if s.messageLog != nil {
				s.messageLog.Info("message held awaiting conduit session",
					"message_id", msg.ID,
					"agent_id", agent.ID,
					"project_id", agent.ProjectID,
					"broker_id", brokerID,
				)
			}
			// If a conduit session advertising deliver-message connected in the
			// window between /deliver's 503 and this hold CAS committing (M-09),
			// trigger re-drive immediately rather than waiting for the 30s sweep.
			s.maybeScheduleHeldMessageRedriveIfConnected(agent.ID)
			return brokerFailureHeld
		}
	}

	switch msg.DispatchState {
	case store.MessageDispatchDispatched, store.MessageDispatchPending:
	case store.MessageDispatchPartial, store.MessageDispatchDelivering:
		// A conduit delivery that may have pasted (gap G5 of the v2.22
		// freeze): the row is failed and the sender told, as with a
		// partial exec delivery. delivery_id and delivery_outcome stay, so
		// a same-id /deliver still replays without calling the agent. A
		// delivered row is never failed.
	default:
		return brokerFailureIgnored
	}

	reason := sanitizeFailureReason(f.Reason)
	if reason == "" {
		reason = "broker delivery failed after buffering"
	}
	// One conditional write decides (review R4): a /deliver finish racing
	// this report wins or loses atomically, a delivered row is never
	// failed, and the notices below go out only when this call made the
	// transition.
	finCtx, finCancel := finalizationContext(ctx)
	won, err := s.store.FailUndeliveredMessage(finCtx, msg.ID, reason)
	finCancel()
	if err != nil {
		slog.Error("message-failures: failed to mark message failed",
			"broker_id", brokerID, "message_id", msg.ID, "error", err)
		return brokerFailureIgnored
	}
	if !won {
		return brokerFailureIgnored
	}
	if s.messageLog != nil {
		s.messageLog.Warn("message delivery failed after broker acceptance; marked failed",
			"message_id", msg.ID,
			"agent_id", agent.ID,
			"project_id", agent.ProjectID,
			"broker_id", brokerID,
			"reason", reason,
		)
	}

	s.notifyStoredMessageSenderFailed(ctx, agent, msg, reason)
	return brokerFailureMarked
}

// applyBrokerMessageDeliveryOutcome validates broker ownership and atomically
// transitions a pending/dispatched message row to delivered, pushing a live
// SSE update to human senders (ptone/scion#9015).
func (s *Server) applyBrokerMessageDeliveryOutcome(ctx context.Context, brokerID string, d messageDeliveryConfirmation) bool {
	if d.MessageID == "" {
		return false
	}
	msg, err := s.store.GetMessage(ctx, d.MessageID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			slog.Warn("message-failures: delivery message lookup failed",
				"broker_id", brokerID, "message_id", d.MessageID, "error", err)
		}
		return false
	}

	// Ownership: the recipient agent must be assigned to the reporting broker.
	// Always derive ownership from the stored msg.RecipientID; request body
	// agentId and projectId are informational only and never trusted.
	recipientAgentID := msg.RecipientID
	if recipientAgentID == "" || !strings.HasPrefix(msg.Recipient, "agent:") {
		return false
	}
	agent, err := s.store.GetAgent(ctx, recipientAgentID)
	if err != nil || agent == nil || agent.RuntimeBrokerID != brokerID {
		slog.Warn("message-failures: broker reported delivery for message it does not own",
			"broker_id", brokerID, "message_id", d.MessageID)
		return false
	}

	switch msg.DispatchState {
	case store.MessageDispatchDispatched, store.MessageDispatchPending:
	default:
		return false
	}

	finCtx, finCancel := finalizationContext(ctx)
	defer finCancel()
	won, err := s.store.MarkMessageDelivered(finCtx, msg.ID)
	if err != nil {
		slog.Error("message-failures: failed to mark message delivered",
			"broker_id", brokerID, "message_id", msg.ID, "error", err)
		return false
	}
	if !won {
		return false
	}
	if s.messageLog != nil {
		s.messageLog.Info("message delivery confirmed by broker; marked delivered",
			"message_id", msg.ID,
			"agent_id", agent.ID,
			"project_id", agent.ProjectID,
			"broker_id", brokerID,
		)
	}

	s.notifyStoredMessageSenderDelivered(finCtx, msg)
	return true
}

// notifyStoredMessageSenderDelivered pushes a live SSE update to connected
// human browser clients after dispatch_state=delivered has committed.
func (s *Server) notifyStoredMessageSenderDelivered(ctx context.Context, msg *store.Message) {
	if msg == nil {
		return
	}
	if strings.HasPrefix(msg.Sender, "user:") && s.events != nil {
		cp := *msg
		cp.DispatchState = store.MessageDispatchDelivered
		s.events.PublishUserMessage(ctx, &cp, nil, nil)
	}
}

// notifyStoredMessageSenderFailed notifies an agent sender (via DELIVERY_FAILED
// notice) or connected human browser clients (via PublishUserMessage) after the
// stored message row's dispatch_state=failed write has already committed.
func (s *Server) notifyStoredMessageSenderFailed(ctx context.Context, agent *store.Agent, msg *store.Message, reason string) {
	if msg == nil {
		return
	}
	// Tell an agent sender its message was not delivered, mirroring the
	// DELIVERY_FAILED notice sent for synchronous broker failures.
	if proxy := s.GetMessageBrokerProxy(); proxy != nil && strings.HasPrefix(msg.Sender, "agent:") && msg.SenderID != "" {
		var projectID, agentSlug string
		if agent != nil {
			projectID = agent.ProjectID
			agentSlug = agent.Slug
		} else {
			projectID = msg.ProjectID
			agentSlug = strings.TrimPrefix(msg.Recipient, "agent:")
		}
		proxy.publishDeliveryFailed(ctx, projectID, agentSlug, &messages.StructuredMessage{
			Sender:   msg.Sender,
			SenderID: msg.SenderID,
		}, errors.New(reason))
	}

	// A human sender has no terminal to inject a DELIVERY_FAILED notice
	// into. Instead, re-publish the message so any connected browser
	// showing this conversation gets the updated dispatch state pushed live
	// (rather than only on next reload) and renders the "Failed" delivery
	// badge the web chat client already supports for its own outbound
	// messages (ptone/scion#1866).
	if strings.HasPrefix(msg.Sender, "user:") && s.events != nil {
		cp := *msg
		cp.DispatchState = store.MessageDispatchFailed
		cp.DispatchFailureReason = &reason
		s.events.PublishUserMessage(ctx, &cp, nil, nil)
	}
}

// notifyStoredMessageSenderUnconfirmed notifies an agent sender (via
// DELIVERY_UNCONFIRMED notice) or connected human browser clients (via
// PublishUserMessage with dispatchFailureCode=delivery_unconfirmed) after
// dispatch_state=failed has been committed for an ambiguous delivery outcome —
// either when ExpireAndListStuckPendingMessages expires a stuck pending row
// with store.MessageExpiredStuckPendingReason (ptone/scion#7405) or when a
// background auto-wake dispatch finalizes an ambiguous timeout/cancellation
// via FailUndeliveredMessage with dispatchOutcomeUnknownReason. In both cases
// delivery may have occurred, so unconfirmed wording tells the sender to check
// before resending.
func (s *Server) notifyStoredMessageSenderUnconfirmed(ctx context.Context, agent *store.Agent, msg *store.Message) {
	if msg == nil {
		return
	}
	if proxy := s.GetMessageBrokerProxy(); proxy != nil && strings.HasPrefix(msg.Sender, "agent:") && msg.SenderID != "" {
		var agentSlug string
		if agent != nil && agent.Slug != "" {
			agentSlug = agent.Slug
		} else {
			agentSlug = strings.TrimPrefix(msg.Recipient, "agent:")
		}
		proxy.publishDeliveryUnconfirmed(ctx, agentSlug, &messages.StructuredMessage{
			Sender:   msg.Sender,
			SenderID: msg.SenderID,
		})
	}

	if strings.HasPrefix(msg.Sender, "user:") && s.events != nil {
		cp := *msg
		cp.DispatchState = store.MessageDispatchFailed
		reason := store.MessageExpiredStuckPendingReason
		cp.DispatchFailureReason = &reason
		s.events.PublishUserMessage(ctx, &cp, nil, nil)
	}
}

// maxFailureReasonBytes bounds the broker-supplied failure reason that is
// stored on the message row and echoed to the sender.
const maxFailureReasonBytes = 512

// maxFailureReasonScanBytes bounds how much input sanitizeFailureReason
// scans. It matches the 64KiB cap already applied to broker error bodies,
// so dropped runes ahead of the real text (e.g. a long run of zero-width
// characters) cannot push that text out of the scanned window; the scan
// stops early once the output is full, so typical inputs cost no more.
const maxFailureReasonScanBytes = 64 << 10

// sanitizeFailureReason makes a broker-supplied reason safe to store and to
// deliver into another agent's terminal: invalid UTF-8 is dropped, and of
// the display-unsafe runes (isDisplayUnsafeRune: Cc, Cf, Zl, Zp) the line
// breaking and whitespace ones (LF, CR, TAB, NEL, U+2028, U+2029) become
// spaces while all others (ESC, BEL, NUL, C1 controls, bidi overrides and
// isolates, zero-width characters, BOM, ...) are removed. The result is
// truncated to maxFailureReasonBytes on a rune boundary.
func sanitizeFailureReason(reason string) string {
	// Bound the work on arbitrarily large input. A rune split by this cut
	// decodes as invalid UTF-8 and is dropped below.
	if len(reason) > maxFailureReasonScanBytes {
		reason = reason[:maxFailureReasonScanBytes]
	}
	var b strings.Builder
	b.Grow(min(len(reason), maxFailureReasonBytes))
	for i := 0; i < len(reason); {
		r, size := utf8.DecodeRuneInString(reason[i:])
		i += size
		if r == utf8.RuneError && size == 1 {
			continue // invalid UTF-8
		}
		if isDisplayUnsafeRune(r) {
			if !unicode.IsSpace(r) {
				continue
			}
			r = ' '
		}
		if b.Len()+utf8.RuneLen(r) > maxFailureReasonBytes {
			break
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

// attachmentOutcomeKey is the counter key of a reported outcome: one of a
// fixed set, so a report cannot add keys.
func attachmentOutcomeKey(o attachmentDeliveryOutcome) string {
	if o.Outcome == "delivered" {
		return "delivered"
	}
	switch o.Reason {
	case messaging.AttachmentReasonNotDownloaded, messaging.AttachmentReasonVerification, messaging.AttachmentReasonNotWritten:
		return "not_delivered: " + o.Reason
	}
	return "not_delivered: other"
}
