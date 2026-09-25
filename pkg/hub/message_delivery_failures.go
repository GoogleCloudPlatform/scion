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
	"unicode"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
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

// messageDeliveryFailure is one failed delivery reported by a broker.
type messageDeliveryFailure struct {
	MessageID string `json:"messageId"`
	AgentID   string `json:"agentId,omitempty"`
	ProjectID string `json:"projectId,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// messageDeliveryFailuresRequest is the body of a message-failures report.
type messageDeliveryFailuresRequest struct {
	Failures []messageDeliveryFailure `json:"failures"`
}

// messageDeliveryFailuresResponse reports how many rows were updated.
type messageDeliveryFailuresResponse struct {
	Marked  int `json:"marked"`
	Ignored int `json:"ignored"`
}

// handleBrokerMessageFailures processes a broker's report of messages that
// were accepted into its delivery buffer but could not be delivered.
//
// Only the broker itself may report, and only for messages whose recipient
// agent is assigned to that broker. Rows not in pending/dispatched state are
// left untouched, so a report can never resurrect or overwrite a terminal
// state set by the hub.
func (s *Server) handleBrokerMessageFailures(w http.ResponseWriter, r *http.Request, brokerID string) {
	ctx := r.Context()

	brokerIdent := GetBrokerIdentityFromContext(ctx)
	if brokerIdent == nil || brokerIdent.BrokerID() != brokerID {
		logAuthzDenial(r, GetIdentityFromContext(ctx), Resource{Type: "runtime_broker", ID: brokerID}, ActionUpdate,
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

	resp := messageDeliveryFailuresResponse{}
	for _, f := range req.Failures {
		if s.applyBrokerMessageFailure(ctx, brokerID, f) {
			resp.Marked++
		} else {
			resp.Ignored++
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// applyBrokerMessageFailure validates and applies one reported failure.
// Returns true when the message row was moved to failed.
func (s *Server) applyBrokerMessageFailure(ctx context.Context, brokerID string, f messageDeliveryFailure) bool {
	if f.MessageID == "" {
		return false
	}
	msg, err := s.store.GetMessage(ctx, f.MessageID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			slog.Warn("message-failures: message lookup failed",
				"broker_id", brokerID, "message_id", f.MessageID, "error", err)
		}
		return false
	}

	// Ownership: the recipient agent must be assigned to the reporting broker.
	recipientAgentID := msg.RecipientID
	if recipientAgentID == "" || !strings.HasPrefix(msg.Recipient, "agent:") {
		return false
	}
	agent, err := s.store.GetAgent(ctx, recipientAgentID)
	if err != nil || agent == nil || agent.RuntimeBrokerID != brokerID {
		slog.Warn("message-failures: broker reported failure for message it does not own",
			"broker_id", brokerID, "message_id", f.MessageID)
		return false
	}

	switch msg.DispatchState {
	case store.MessageDispatchDispatched, store.MessageDispatchPending:
	default:
		return false
	}

	reason := sanitizeFailureReason(f.Reason)
	if reason == "" {
		reason = "broker delivery failed after buffering"
	}
	if err := s.markFailed(ctx, msg.ID, reason); err != nil {
		slog.Error("message-failures: failed to mark message failed",
			"broker_id", brokerID, "message_id", msg.ID, "error", err)
		return false
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

	// Tell an agent sender its message was not delivered, mirroring the
	// DELIVERY_FAILED notice sent for synchronous broker failures.
	if proxy := s.GetMessageBrokerProxy(); proxy != nil && strings.HasPrefix(msg.Sender, "agent:") && msg.SenderID != "" {
		proxy.publishDeliveryFailed(ctx, agent.ProjectID, agent.Slug, &messages.StructuredMessage{
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
		msg.DispatchState = store.MessageDispatchFailed
		msg.DispatchFailureReason = &reason
		s.events.PublishUserMessage(ctx, msg, nil)
	}
	return true
}

// maxFailureReasonBytes bounds the broker-supplied failure reason that is
// stored on the message row and echoed to the sender.
const maxFailureReasonBytes = 512

// sanitizeFailureReason makes a broker-supplied reason safe to store and to
// deliver into another agent's terminal: invalid UTF-8 is dropped, whitespace
// control characters (LF, CR, TAB, ...) become spaces, all other control
// characters (ESC, BEL, NUL, C1 controls, ...) are removed, and the result is
// truncated to maxFailureReasonBytes on a rune boundary.
func sanitizeFailureReason(reason string) string {
	// Bound the work on arbitrarily large input before any allocation. 4x
	// leaves room for dropped control/invalid bytes ahead of the rune-aware
	// truncation below; a rune split by this cut is dropped as invalid UTF-8.
	if len(reason) > maxFailureReasonBytes*4 {
		reason = reason[:maxFailureReasonBytes*4]
	}
	reason = strings.ToValidUTF8(reason, "")
	var b strings.Builder
	b.Grow(min(len(reason), maxFailureReasonBytes))
	for _, r := range reason {
		if unicode.IsControl(r) {
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
