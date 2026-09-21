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
	"fmt"
	"math"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ---------------------------------------------------------------------------
// Agent DM Operation — typed input, result, and errors (#1688)
// ---------------------------------------------------------------------------
//
// ExecuteAgentDM is the single internal operation for agent-to-agent direct
// message delivery. Both the outbound handler (handleAgentOutboundMessage,
// deliveryAgentDM path) and the structured/inbound handler
// (handleAgentMessage, agent-to-agent path) route their DM branches
// through this operation.
//
// The operation consolidates:
//   - Aggregate send budget (rate limiting) — AC-2
//   - Message length validation — AC-3
//   - Authorization (mode + cross-project) — AC-3
//   - Foreign attachment rejection (#1687) — AC-3
//   - Attachment ingestion
//   - Message persistence
//   - SSE event publication
//   - Delivery text rendering
//   - Dispatch to target agent runtime
//   - Observer publication
//
// Adapters retain HTTP serialization, routing/conversation resolution,
// group/human routing, wake handling, mention processing, and notification
// subscription. These concerns are outside the core DM operation.

// AgentDMInput is the typed, normalized input for the shared agent DM
// operation. Adapters construct this from their respective request formats
// after resolving routing and conversation context.
type AgentDMInput struct {
	// SenderAgent is the agent originating the DM (freshly read from store).
	SenderAgent *store.Agent

	// SenderIdentity is the authenticated caller identity. Used for
	// authorization checks.
	SenderIdentity Identity

	// TargetAgent is the agent receiving the DM (freshly read from store).
	TargetAgent *store.Agent

	// Msg is the plain text message body.
	Msg string

	// Type is the message type (e.g. "input-needed", "instruction").
	// Used for rate limit class derivation. The type-class reservation
	// system is preserved: the aggregate ceiling is always charged, so
	// relabelling traffic cannot buy extra allowance.
	Type string

	// Urgent requests priority delivery to the target agent.
	Urgent bool

	// Interrupt requests that the target agent's harness be interrupted
	// before delivery (structured/inbound path only).
	Interrupt bool

	// Attachments are file paths for attachment ingestion. Cross-project
	// DMs reject non-empty attachments before ingestion.
	Attachments []string

	// Metadata is the message metadata map.
	Metadata map[string]string

	// ConversationID is the resolved conversation UUID. May be empty when
	// conversation resolution was skipped or failed with write-deny OFF.
	ConversationID string

	// ConvResult holds the full conversation metadata when resolution
	// succeeded. Nil when conversation resolution was skipped or failed.
	ConvResult *messaging.ConversationResult

	// Asserted is true when the caller supplied an explicit conversation_id
	// that was authorized. False for derived conversations.
	Asserted bool

	// Channel is the delivery channel (e.g. "web", "discord").
	Channel string

	// ThreadID is the thread/DM key for the message.
	ThreadID string

	// ProjectID is the project context for the message store record.
	// Adapters set this from their own convention (sender's or target's
	// project, depending on the path).
	ProjectID string

	// GroupID correlates group-set messages in the store. Empty for
	// direct 1:1 DMs.
	GroupID string
}

// AgentDMOutcome enumerates the possible delivery result states.
// The contract distinguishes accepted, failed, and ambiguous delivery;
// no API claims exactly-once or automatic replay (AC-5).
type AgentDMOutcome string

const (
	// AgentDMAccepted: message persisted and broker/managed-runtime
	// accepted the dispatch. The message row is in "dispatched" state.
	// API wording is "dispatched" — does not promise harness consumption.
	AgentDMAccepted AgentDMOutcome = "accepted"

	// AgentDMFailed: a pre-flight check failed (rate limit, authorization,
	// validation, attachment rejection), persistence failed, or dispatch
	// was definitively rejected. Pre-flight failures produce no side
	// effects; post-persistence failures persist the "failed" state on
	// the message row and return non-2xx with the message ID.
	AgentDMFailed AgentDMOutcome = "failed"

	// AgentDMAmbiguous: message was persisted and dispatch may or may not
	// have succeeded — e.g. the broker accepted the message but the
	// MarkMessageDispatched CAS failed, or context was cancelled mid-flight.
	// The message row may be in "pending" or "dispatched" state. Callers
	// must NOT assume delivery and must NOT automatically replay.
	// No blind retry guidance is returned.
	AgentDMAmbiguous AgentDMOutcome = "ambiguous"
)

// AgentDMResult is the typed result of the shared agent DM operation.
type AgentDMResult struct {
	// Outcome is the delivery result state.
	Outcome AgentDMOutcome

	// MessageID is the persisted message's UUID. Empty when Outcome is
	// AgentDMFailed (no persistence occurred).
	MessageID string

	// Recipient is the wire-format recipient (e.g. "agent:my-agent").
	Recipient string

	// RecipientID is the target agent's UUID.
	RecipientID string

	// DispatchErr is non-nil when Outcome is AgentDMAmbiguous. It
	// captures the dispatch failure reason for logging/diagnostics.
	// Callers should NOT expose this to end users.
	DispatchErr error
}

// AgentDMError is a typed error from the shared DM operation. It carries
// enough information for adapters to render the appropriate HTTP response
// without re-evaluating the failure.
type AgentDMError struct {
	// Code is the machine-readable error code (e.g. ErrCodeRateLimited).
	Code string

	// Message is the human-readable error description.
	Message string

	// HTTPStatus is the appropriate HTTP status code for this error.
	HTTPStatus int

	// Details is an optional map of structured error metadata.
	Details map[string]interface{}

	// RetryAfter is set when the error is a rate-limit rejection.
	// Adapters should set the Retry-After header to this duration.
	RetryAfter time.Duration
}

func (e *AgentDMError) Error() string { return e.Message }

// ExecuteAgentDM is the single internal operation for agent-to-agent DM
// delivery. It performs all admission checks before any side effects, then
// persists, publishes, and dispatches the message.
//
// Returns (*AgentDMResult, nil) on success or ambiguous delivery, or
// (nil, *AgentDMError) on pre-flight failure.
func (s *Server) ExecuteAgentDM(ctx context.Context, input *AgentDMInput) (*AgentDMResult, *AgentDMError) {
	// ── Phase 1: Admission checks (no side effects) ─────────────────────
	// All checks must pass before any content/lifecycle effects (AC-3).

	// 1. Rate limit — aggregate send budget (AC-2).
	// The traffic class is derived from the message type, but the aggregate
	// ceiling is always charged, so switching type cannot bypass the budget.
	class := chatSenderClassForMessageType(input.Type)
	rateLimitDecision := s.chatSendLimiter.Allow(input.SenderAgent.ID, class)
	if !rateLimitDecision.Allowed {
		seconds := int(math.Ceil(rateLimitDecision.RetryAfter.Seconds()))
		dmErr := &AgentDMError{
			Code: ErrCodeRateLimited,
			Message: fmt.Sprintf("send rate limit exceeded (%d %s per minute); retry in %ds",
				int(rateLimitDecision.Limit), rateLimitDecision.LimitClass.noun(), seconds),
			HTTPStatus: http.StatusTooManyRequests,
			RetryAfter: rateLimitDecision.RetryAfter,
		}
		// Audit: body-free denial record (#1690).
		LogDMAdmission(DMAuditEntryForDenial(input, ErrCodeRateLimited, dmErr.Message))
		return nil, dmErr
	}

	// 2. Message length validation.
	if msgLen := utf8.RuneCountInString(input.Msg); msgLen > messages.MaxMessageLength {
		dmErr := &AgentDMError{
			Code: ErrCodeValidationError,
			Message: fmt.Sprintf("message exceeds %d character limit (current: %d chars). Consider splitting into multiple messages using multiple scion message invocations",
				messages.MaxMessageLength, msgLen),
			HTTPStatus: http.StatusUnprocessableEntity,
		}
		// Audit: body-free denial record (#1690).
		LogDMAdmission(DMAuditEntryForDenial(input, ErrCodeValidationError, "message length exceeded"))
		return nil, dmErr
	}

	// 3. Authorization — mode + cross-project checks.
	allowed, reason, authDecision := s.authorizeAgentMessage(ctx, input.SenderIdentity, input.TargetAgent, false)
	if !allowed {
		denialCode := mapReasonToCode(reason)
		if authDecision != nil && authDecision.Code != "" {
			denialCode = string(authDecision.Code)
		}
		s.messageLog.Warn("agent DM authorization denied",
			"sender_id", input.SenderAgent.ID,
			"target_agent_id", input.TargetAgent.ID,
			"reason", reason,
			"denial_code", denialCode,
		)
		// Audit: body-free denial record with policy revisions (#1690).
		// CrossProject is derived from agent records by DMAuditEntryFromInput,
		// not from the decision — avoids relying on the authorization path
		// to set it.
		auditDecision := &MessageDecision{
			Allowed: false,
			Code:    MessageDenialCode(denialCode),
			Reason:  reason,
		}
		if authDecision != nil {
			auditDecision.HubPolicyRevision = authDecision.HubPolicyRevision
			auditDecision.ProjectPolicyRevision = authDecision.ProjectPolicyRevision
		}
		LogDMAdmission(DMAuditEntryFromInput(input, auditDecision))
		return nil, &AgentDMError{
			Code:       ErrCodeMessageDenied,
			Message:    "Message delivery denied",
			HTTPStatus: http.StatusForbidden,
			Details: map[string]interface{}{
				"reason":        denialCode,
				"senderMode":    input.SenderAgent.MessageMode,
				"recipientMode": input.TargetAgent.MessageMode,
			},
		}
	}

	// 4. Foreign attachment rejection (#1687).
	// Cross-project DMs are text-only until managed transfer is implemented.
	if len(input.Attachments) > 0 && input.SenderAgent.ProjectID != input.TargetAgent.ProjectID {
		// Audit: body-free denial record (#1690).
		LogDMAdmission(DMAuditEntryForDenial(input, string(MessageDenialCrossProjectAttachUnsupported),
			"cross-project attachment transfer not supported"))
		return nil, &AgentDMError{
			Code:       ErrCodeUnsupportedCapability,
			Message:    "cross-project attachment transfer is not supported; send text-only messages across projects",
			HTTPStatus: http.StatusUnprocessableEntity,
			Details: map[string]interface{}{
				"reason": string(MessageDenialCrossProjectAttachUnsupported),
			},
		}
	}

	// 5. Dispatch availability pre-check (#1689).
	// Verify dispatch infrastructure before persistence so that missing
	// dispatcher/broker does not leave orphaned pending rows or falsely
	// report success. This is a pre-flight check: managed-runtime backend
	// resolution and actual dispatch errors are handled post-persistence.
	if dmErr := s.checkDispatchAvailability(input); dmErr != nil {
		return nil, dmErr
	}

	// ── Phase 2: Side effects ───────────────────────────────────────────

	// 6. Attachment ingestion.
	attachmentRefs := s.ingestAgentAttachments(ctx, input.ProjectID, input.SenderAgent.ID, input.Attachments)

	// 7. Build store message.
	// DispatchState is set to "pending" — the message row is its own
	// durable dispatch intent. It transitions to "dispatched" only after
	// broker/managed-runtime acceptance (AC-1, #1689).
	msgID := api.NewUUID()
	now := time.Now()

	storeMsg := &store.Message{
		ID:             msgID,
		ProjectID:      input.ProjectID,
		Sender:         "agent:" + input.SenderAgent.Slug,
		SenderID:       input.SenderAgent.ID,
		Recipient:      "agent:" + input.TargetAgent.Slug,
		RecipientID:    input.TargetAgent.ID,
		Msg:            input.Msg,
		Type:           input.Type,
		Urgent:         input.Urgent,
		AgentID:        input.SenderAgent.ID,
		Channel:        input.Channel,
		ThreadID:       input.ThreadID,
		ConversationID: input.ConversationID,
		GroupID:        input.GroupID,
		CreatedAt:      now,
		DispatchState:  store.MessageDispatchPending,
	}

	// 7a. Stamp server-derived provenance (#1690).
	// SenderProjectID and RecipientProjectID are always derived from the
	// authoritative agent records, never from client-supplied metadata.
	StampProvenance(storeMsg, input.SenderAgent, input.TargetAgent)

	// 7b. Build structured message for dispatch and observer publication.
	structuredMsg := &messages.StructuredMessage{
		Sender:               storeMsg.Sender,
		SenderID:             storeMsg.SenderID,
		Recipient:            storeMsg.Recipient,
		RecipientID:          storeMsg.RecipientID,
		Msg:                  storeMsg.Msg,
		Type:                 storeMsg.Type,
		Urgent:               storeMsg.Urgent,
		Attachments:          input.Attachments,
		Channel:              input.Channel,
		ThreadID:             input.ThreadID,
		Metadata:             input.Metadata,
		ConversationID:       input.ConversationID,
		ConversationAsserted: input.Asserted,
	}

	// Stamp attachment metadata onto the structured message.
	if encoded, ok := attachmentRefsMetadata(attachmentRefs); ok {
		if structuredMsg.Metadata == nil {
			structuredMsg.Metadata = make(map[string]string, 1)
		}
		structuredMsg.Metadata[attachmentsMetadataKey] = encoded
	}

	// 8. Persist message (AC-3: CreateMessage failure prevents dispatch).
	if err := s.store.CreateMessage(ctx, storeMsg); err != nil {
		s.messageLog.Error("agent DM: failed to persist message", "error", err)
		return nil, &AgentDMError{
			Code:       ErrCodeInternalError,
			Message:    "Failed to persist message",
			HTTPStatus: http.StatusInternalServerError,
		}
	}

	// 8a. Audit: body-free admission allow record (#1690).
	// Logged after persistence so the message ID is available as correlation.
	// CrossProject is derived from agent records by DMAuditEntryFromInput,
	// not from the decision.
	allowDecision := &MessageDecision{Allowed: true}
	if authDecision != nil {
		allowDecision.HubPolicyRevision = authDecision.HubPolicyRevision
		allowDecision.ProjectPolicyRevision = authDecision.ProjectPolicyRevision
	}
	admissionEntry := DMAuditEntryFromInput(input, allowDecision)
	admissionEntry.CorrelationID = msgID
	LogDMAdmission(admissionEntry)

	// 8b. Validate persisted provenance against authoritative records (#1690).
	if !ValidateProvenance(storeMsg, input.SenderAgent, input.TargetAgent) {
		s.messageLog.Error("agent DM: provenance validation failed after persist",
			"message_id", msgID,
			"sender_agent_id", input.SenderAgent.ID,
			"target_agent_id", input.TargetAgent.ID,
		)
	}

	// 9. Publish SSE event.
	s.events.PublishUserMessage(ctx, storeMsg, attachmentRefs)

	// 10. Render delivery text envelope.
	if s.writeDenyEnabled() {
		structuredMsg.DeliveryText = messaging.RenderDeliveryText(messaging.RenderDeliveryInput{
			MessageID:  storeMsg.ID,
			ConvResult: input.ConvResult,
			Msg:        structuredMsg,
			CreatedAt:  storeMsg.CreatedAt,
		})
	}

	// 11. Dispatch to target agent runtime (#1689).
	// Dispatch outcome determines the final message state:
	//   - Success → CAS pending→dispatched (AC-1)
	//   - Definite failure → persist failed state, return non-2xx (AC-2)
	//   - Ambiguous (context cancelled) → leave pending, return ambiguous (AC-4)
	var dispatchErr error
	if isManagedAgentRuntime(input.TargetAgent.Runtime) {
		dispatchErr = s.managedAgentMessage(ctx, input.TargetAgent, input.Msg, input.Urgent || input.Interrupt)
	} else if dispatcher := s.GetDispatcher(); dispatcher != nil && input.TargetAgent.RuntimeBrokerID != "" {
		retryCtx, retryCancel := context.WithTimeout(ctx, 30*time.Second)
		dispatchErr = dispatchWithBrokerRetry(retryCtx, dispatcher, input.TargetAgent, input.Msg, input.Urgent || input.Interrupt, structuredMsg)
		retryCancel()
	}

	// 12. Finalize dispatch state (#1689).
	if dispatchErr != nil {
		if isAmbiguousDispatchError(dispatchErr) {
			// Ambiguous: dispatch may have succeeded but confirmation was
			// lost (e.g. context cancelled mid-flight). Leave the row in
			// pending state for inspection; return ambiguous outcome (AC-4).
			s.messageLog.Warn("agent DM: dispatch ambiguous (message persisted as pending)",
				"sender_id", input.SenderAgent.ID,
				"target_agent_id", input.TargetAgent.ID,
				"message_id", msgID,
				"error", dispatchErr,
			)
			// Audit: dispatch outcome (#1690).
			LogDMDispatchOutcome(input.SenderAgent, input.TargetAgent, msgID, DispatchFailed, dispatchErr)
			return &AgentDMResult{
				Outcome:     AgentDMAmbiguous,
				MessageID:   msgID,
				Recipient:   storeMsg.Recipient,
				RecipientID: storeMsg.RecipientID,
				DispatchErr: dispatchErr,
			}, nil
		}

		// Definite failure: dispatch was not accepted by the broker or
		// managed runtime (AC-2). Record the failed state.
		s.messageLog.Error("agent DM: dispatch failed (message persisted)",
			"sender_id", input.SenderAgent.ID,
			"target_agent_id", input.TargetAgent.ID,
			"message_id", msgID,
			"error", dispatchErr,
		)
		// Audit: dispatch outcome (#1690).
		LogDMDispatchOutcome(input.SenderAgent, input.TargetAgent, msgID, DispatchFailed, dispatchErr)
		if markErr := s.markFailed(ctx, msgID, dispatchErr.Error()); markErr != nil {
			s.messageLog.Error("agent DM: failed to mark message failed",
				"message_id", msgID, "error", markErr)
		}
		return nil, dispatchFailedError(msgID)
	}

	// Dispatch succeeded — CAS pending→dispatched (AC-1).
	dispatched, casErr := s.markDispatched(ctx, msgID)
	if casErr != nil {
		// Store failure after broker acceptance: the message IS dispatched
		// but our state tracking is uncertain (AC-3). Return ambiguous
		// with the stable message ID so the caller can correlate.
		s.messageLog.Error("agent DM: MarkMessageDispatched store error (message dispatched)",
			"message_id", msgID, "error", casErr)
		// Audit: dispatch outcome (#1690) — dispatch succeeded but state tracking failed.
		LogDMDispatchOutcome(input.SenderAgent, input.TargetAgent, msgID, DispatchSucceeded, casErr)
		return &AgentDMResult{
			Outcome:     AgentDMAmbiguous,
			MessageID:   msgID,
			Recipient:   storeMsg.Recipient,
			RecipientID: storeMsg.RecipientID,
			DispatchErr: fmt.Errorf("dispatch succeeded but state transition failed: %w", casErr),
		}, nil
	}
	if !dispatched {
		// CAS miss — already transitioned by concurrent process.
		// The dispatch itself succeeded; log for visibility but not an error.
		s.messageLog.Warn("agent DM: MarkMessageDispatched CAS miss (already transitioned)",
			"message_id", msgID)
	}

	// Audit: dispatch outcome (#1690).
	LogDMDispatchOutcome(input.SenderAgent, input.TargetAgent, msgID, DispatchSucceeded, nil)

	// 13. Observer publication.
	// Publish observer-only message for agent-to-agent visibility.
	// ConversationAsserted is forced false on the observer copy to
	// preserve the pre-refactor observer envelope shape.
	// Cross-project DMs strip body and attachment metadata from the
	// observer message (#1687).
	if bp := s.GetMessageBrokerProxy(); bp != nil {
		observerMsg := *structuredMsg
		observerMsg.ObserverOnly = true
		observerMsg.ConversationAsserted = false
		if input.SenderAgent.ProjectID != input.TargetAgent.ProjectID {
			sanitizeCrossProjectObserver(&observerMsg)
		}
		if err := bp.PublishMessage(ctx, input.TargetAgent.ProjectID, &observerMsg); err != nil {
			s.messageLog.Error("agent DM: observer publish failed",
				"target_agent_id", input.TargetAgent.ID, "error", err)
		}
	}

	// 14. Log delivery.
	s.logMessage("agent DM: message dispatched",
		"sender_agent_id", input.SenderAgent.ID,
		"target_agent_id", input.TargetAgent.ID,
		"project_id", input.ProjectID,
		"message_id", msgID,
	)

	// Return accepted result.
	return &AgentDMResult{
		Outcome:     AgentDMAccepted,
		MessageID:   msgID,
		Recipient:   storeMsg.Recipient,
		RecipientID: storeMsg.RecipientID,
	}, nil
}

// WriteAgentDMError writes an AgentDMError as an HTTP response. Adapters
// call this to translate operation errors into wire format.
func WriteAgentDMError(w http.ResponseWriter, dmErr *AgentDMError) {
	if dmErr.RetryAfter > 0 {
		seconds := int(math.Ceil(dmErr.RetryAfter.Seconds()))
		w.Header().Set("Retry-After", fmt.Sprintf("%d", seconds))
	}
	writeError(w, dmErr.HTTPStatus, dmErr.Code, dmErr.Message, dmErr.Details)
}

// WriteAgentDMResult writes an AgentDMResult as an HTTP JSON response.
// Adapters call this to translate operation results into wire format.
//
// The status wording is "dispatched" (not "sent" or "delivered") to indicate
// that the broker/managed-runtime accepted the message without promising
// harness consumption (AC-1, #1689).
//
// Ambiguous outcomes (dispatch succeeded but state tracking failed) use
// HTTP 202 Accepted with status "ambiguous" and the stable message ID
// for caller correlation. No blind retry guidance is returned (AC-4).
func WriteAgentDMResult(w http.ResponseWriter, result *AgentDMResult) {
	switch result.Outcome {
	case AgentDMAmbiguous:
		writeJSON(w, http.StatusAccepted, map[string]interface{}{
			"message_id":   result.MessageID,
			"status":       "ambiguous",
			"recipient":    result.Recipient,
			"recipient_id": result.RecipientID,
		})
	default:
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"message_id":   result.MessageID,
			"status":       "dispatched",
			"recipient":    result.Recipient,
			"recipient_id": result.RecipientID,
		})
	}
}
