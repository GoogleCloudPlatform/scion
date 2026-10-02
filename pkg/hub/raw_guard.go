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
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

// This file implements Phase 0.2 of the agent-keys cutover (ptone/scion#2184,
// task ptone/scion#2192): guards that reject unsafe raw messaging forms
// before any side effect (conversation resolution, mention work, attachment
// ingestion, wake/lifecycle calls, persistence, observer/notification
// effects or dispatch of any kind).
//
// These are containment guards only, not a new permanent raw policy engine.
// Until the Phase 2.3 bridge replaces it with the dedicated /keys operation,
// raw delivery remains supported for exactly one shape: an unadorned direct
// message to a single, non-managed, same-project agent. Every other
// combination described in ptone/scion#2184 ("Raw compatibility and
// removal") is rejected here.

// crossProjectRawUnsupported reports whether a raw agent-sender DM crosses
// project boundaries. It is an unconditional inequality with no carve-out
// for an empty project ID on either side. Both agent_dm_operation.go step 4b
// and handlers_agent_messaging.go's early HTTP-layer check call this shared
// definition, so the two checks cannot drift apart: an empty stored sender
// ProjectID is a mismatch, not a carve-out.
func crossProjectRawUnsupported(senderProjectID, targetProjectID string) bool {
	return senderProjectID != targetProjectID
}

// rawGuardViolation describes why a raw-flagged messaging request must be
// rejected, and the HTTP/machine-outcome codes to report for it.
type rawGuardViolation struct {
	httpStatus int
	errCode    string
	denial     MessageDenialCode
	message    string
}

// writeRawGuardViolation writes the HTTP error response for a raw guard
// violation using the shared error envelope.
func writeRawGuardViolation(w http.ResponseWriter, v *rawGuardViolation) {
	writeError(w, v.httpStatus, v.errCode, v.message, map[string]interface{}{
		"reason": string(v.denial),
	})
}

func rawPlainConflict() *rawGuardViolation {
	return &rawGuardViolation{
		httpStatus: http.StatusBadRequest,
		errCode:    ErrCodeInvalidRequest,
		denial:     MessageDenialRawPlainConflict,
		message:    "raw and plain are mutually exclusive",
	}
}

func unsupportedRaw(code MessageDenialCode, message string) *rawGuardViolation {
	return &rawGuardViolation{
		httpStatus: http.StatusUnprocessableEntity,
		errCode:    ErrCodeUnsupportedCapability,
		denial:     code,
		message:    message,
	}
}

// rawMessageGuardInput carries every field relevant to deciding whether a
// raw-flagged message request must be rejected before side effects. Fields
// come from the request wrapper (mentions, wake, interrupt, conversation
// resolution inputs) as well as the assembled StructuredMessage itself
// (plain, attachments, observer-only, conversation addressing).
type rawMessageGuardInput struct {
	Msg              *messages.StructuredMessage
	ExplicitMentions int
	Wake             bool
	Interrupt        bool
	Surface          string
	ExternalRef      string
	ParentRef        string
	IsGroupRecipient bool
}

// evaluateRawMessageGuard returns a non-nil violation when Msg.Raw is set
// and the request uses any messaging capability that raw delivery does not
// support. It returns nil for every non-raw message (Plain and normal
// messages are unaffected) and for the single still-supported raw shape:
// an unadorned direct message.
//
// Callers MUST invoke this before conversation resolution, mention
// processing, attachment ingestion, wake/lifecycle calls, persistence or
// dispatch of any kind (ptone/scion#2192).
//
// Explicit decision: raw+Notify is allowed and left unguarded. Notify only
// subscribes the sender to the target's status notifications — it does not
// change addressing, fan-out, or the raw payload's terminal delivery, so
// it is orthogonal to the capabilities this guard restricts.
// raw+metadata["group_id"] is rejected below instead of allowed, because it
// is a side channel for simulating group membership without the group[]
// recipient syntax IsGroupRecipient checks.
func evaluateRawMessageGuard(in rawMessageGuardInput) *rawGuardViolation {
	msg := in.Msg
	if msg == nil || !msg.Raw {
		return nil
	}

	if msg.Plain {
		return rawPlainConflict()
	}
	if in.IsGroupRecipient {
		return unsupportedRaw(MessageDenialRawGroupUnsupported,
			"raw delivery does not support group or broadcast recipients")
	}
	if gid, ok := msg.Metadata["group_id"]; ok && gid != "" {
		// metadata["group_id"] is how per-recipient fan-out messages carry
		// their shared correlation id back to the store (see the group_id
		// propagation in handleAgentMessage/handleGroupMessage). This side
		// channel does not use the group[] recipient syntax that
		// IsGroupRecipient checks above, so raw+metadata.group_id is
		// rejected the same way.
		return unsupportedRaw(MessageDenialRawGroupUnsupported,
			"raw delivery does not support group_id metadata")
	}
	if in.ExplicitMentions > 0 {
		return unsupportedRaw(MessageDenialRawMentionsUnsupported,
			"raw delivery does not support explicit mentions or CC")
	}
	if len(msg.Attachments) > 0 {
		return unsupportedRaw(MessageDenialRawAttachUnsupported,
			"raw delivery does not support attachments")
	}
	if in.Wake {
		return unsupportedRaw(MessageDenialRawWakeUnsupported,
			"raw delivery does not support wake")
	}
	if in.Interrupt || msg.Urgent {
		// msg.Urgent is a second spelling of "interrupt": the CLI maps
		// `scion message --raw --interrupt` onto StructuredMessage.Urgent
		// (cmd/message.go), and ExecuteAgentDM dispatches with
		// input.Urgent || input.Interrupt as the interrupt flag
		// (agent_dm_operation.go). Checking only req.Interrupt would let
		// this spelling through — exactly the silent downgrade this guard
		// exists to stop. `scion keys` always sends urgent=false, so
		// rejecting this does not affect the still-supported caller.
		return unsupportedRaw(MessageDenialRawInterruptUnsupported,
			"raw delivery does not support interrupt")
	}
	if msg.ObserverOnly {
		return unsupportedRaw(MessageDenialRawObserverUnsupported,
			"raw delivery does not support observer-only messages")
	}
	if isRawConversationAddressed(msg, in.Surface, in.ExternalRef, in.ParentRef) {
		return unsupportedRaw(MessageDenialRawConversationUnsupported,
			"raw delivery does not support conversation-addressed messages")
	}
	return nil
}

// isRawConversationAddressed reports whether the message explicitly
// addresses a conversation rather than being a plain direct message. The
// still-supported raw shape carries no ThreadID/Channel/ConversationID at
// all, so any of these being set is conversation addressing.
//
// There is no "dm:"-prefixed ThreadID carve-out here: ValidateLegacyMessage
// requires Channel whenever ThreadID is set, and a non-empty Channel is
// rejected by the Channel check below — so a raw request with a "dm:"
// ThreadID and no Channel would fail validation regardless, and one with a
// Channel is already caught. A carve-out for that case would be dead code.
func isRawConversationAddressed(msg *messages.StructuredMessage, surface, externalRef, parentRef string) bool {
	if msg.ConversationID != "" {
		return true
	}
	if surface != "" || externalRef != "" || parentRef != "" {
		return true
	}
	if msg.Channel != "" {
		return true
	}
	if msg.ThreadID != "" {
		return true
	}
	return false
}

// rejectRawScheduledPayload returns an error when the advanced scheduled-
// message payload JSON carries a "raw" key at all (including raw:false).
// Scheduled delivery does not forward StructuredMessage.Raw —
// MessageEventPayload has no Raw field — so this decode-time tombstone
// rejects the key explicitly rather than let a caller believe scheduled raw
// keystroke delivery is supported.
func rejectRawScheduledPayload(payload string) error {
	if payload == "" {
		return nil
	}
	var probe struct {
		Raw json.RawMessage `json:"raw"`
	}
	if err := json.Unmarshal([]byte(payload), &probe); err != nil {
		// Malformed payload JSON is reported by the normal payload
		// validation path; nothing more to do here.
		return nil
	}
	if probe.Raw != nil {
		return fmt.Errorf("raw delivery is not supported for scheduled messages")
	}
	return nil
}

// validateScheduledPayloadShape rejects a payload that is not, at the top
// level, a JSON object — a bare array, string, number, boolean, or `null`,
// or syntactically invalid JSON outright.
//
// A bare `null` needs its own check because it is not a decode error:
// encoding/json treats a JSON `null` as a no-op for any destination type,
// struct or map alike, so `json.Unmarshal([]byte("null"), &anything)`
// returns a nil error without touching the destination. Decoding into
// `map[string]json.RawMessage` surfaces this directly — the map comes back
// nil with no error — which a struct decode (as used further down the
// validation sequence) cannot distinguish from "decoded, all fields zero".
//
// This is step 1 of the three-step validation order and must
// run before both the raw-key tombstone probe and the event-type struct
// decode, so that "not an object at all" is reported as a single, sanitized
// 400 regardless of what either later step would have made of the value.
func (s *Server) validateScheduledPayloadShape(w http.ResponseWriter, payload string) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(payload), &m); err != nil || m == nil {
		BadRequest(w, "payload must be a valid JSON object")
		return false
	}
	return true
}

// validateScheduledEventPayloadJSON rejects an advanced Payload whose fields
// don't match the event type's payload struct (ptone/scion#2200: "a
// malformed payload gets a sanitized 400 before persistence"). Without this,
// a mistyped-field payload reaches storage unvalidated:
// authorizeScheduledMessageAuthoring's own decode for target resolution
// tolerates a parse failure by design (it falls back to convenience fields
// when the payload doesn't yield a target), and rejectRawScheduledPayload
// likewise treats a decode failure as "nothing more to do here" — neither
// one is the authoritative shape check.
//
// This function only catches field-type mismatches (e.g. {"agentName":5}).
// Top-level shape (non-object, null, syntax errors) is the responsibility of
// validateScheduledPayloadShape, which callers must run first — see
// validateAndRejectScheduledPayload, which runs both in the ruling's
// required order. Calling this function alone on a non-object payload would
// still report a decode error for most non-object payloads — but not for a
// bare `null`, which is a silent no-op here for the same reason it is one in
// validateScheduledPayloadShape (see that function's doc comment). Ordering
// also matters for the "raw" key: see below.
//
// Unknown fields are accepted (plain encoding/json.Unmarshal semantics, no
// DisallowUnknownFields): a "raw" key is not a field on either payload
// struct, so this function alone would decode a payload like
// {"raw":true,"agentName":5} as a *type* mismatch (400) even though the
// ruling requires 422 for any valid-JSON payload carrying "raw" — which is
// exactly why validateAndRejectScheduledPayload runs the raw-key tombstone
// before this struct decode, not after.
//
// Writes a sanitized 400 (never echoing the malformed body back) and
// returns false on a decode failure; returns true for an empty payload
// (nothing to validate) or a payload that decodes cleanly into the event
// type's struct (regardless of which optional fields it set — field-level
// *requirements* such as "message is required" are a different, weaker
// class of check that this function does not perform at all: an advanced
// Payload of "{}" decodes cleanly here and is persisted, with the
// "message is required" check only ever applied on the separate
// convenience-field branch in handlers_scheduled_events.go.)
func (s *Server) validateScheduledEventPayloadJSON(w http.ResponseWriter, eventType, payload string) bool {
	if payload == "" {
		return true
	}
	var decodeErr error
	switch eventType {
	case "message":
		var p MessageEventPayload
		decodeErr = json.Unmarshal([]byte(payload), &p)
	case "dispatch_agent":
		var p DispatchAgentEventPayload
		decodeErr = json.Unmarshal([]byte(payload), &p)
	default:
		// Every caller validates eventType against the closed
		// {message, dispatch_agent} set before reaching here; this branch
		// is unreachable in practice. It does not fail closed — it falls
		// back to a syntax-only check
		// because there is no struct to decode into for an unknown type.
		if !json.Valid([]byte(payload)) {
			decodeErr = fmt.Errorf("invalid JSON")
		}
	}
	if decodeErr != nil {
		BadRequest(w, "payload must be a valid JSON object for the "+eventType+" event type")
		return false
	}
	return true
}

// validateAndRejectScheduledPayload runs the full payload-validation
// sequence in the order the ruling requires:
//
//  1. validateScheduledPayloadShape: syntax + top-level-object shape -> 400
//  2. rejectRawScheduledPayload: the "raw" key tombstone -> 422
//  3. validateScheduledEventPayloadJSON: the event-type struct decode -> 400
//
// The order matters: a valid JSON object carrying "raw" plus some unrelated
// mistyped field (e.g. {"raw":true,"agentName":5}) must stop at step 2 with
// 422, not fall through to step 3's 400. Running the struct decode before
// the raw probe — as an earlier revision of this validation did — collapsed
// that case into a generic 400, contradicting "valid JSON carrying a raw key
// stays 422."
//
// Returns true, writing nothing, when payload is empty (nothing to
// validate) or passes all three steps; returns false after writing the
// appropriate error response otherwise.
func (s *Server) validateAndRejectScheduledPayload(w http.ResponseWriter, eventType, payload string) bool {
	if payload == "" {
		return true
	}
	if !s.validateScheduledPayloadShape(w, payload) {
		return false
	}
	if err := rejectRawScheduledPayload(payload); err != nil {
		writeError(w, http.StatusUnprocessableEntity, ErrCodeUnsupportedCapability, err.Error(),
			map[string]interface{}{"reason": string(MessageDenialRawSchedulingUnsupported)})
		return false
	}
	return s.validateScheduledEventPayloadJSON(w, eventType, payload)
}
