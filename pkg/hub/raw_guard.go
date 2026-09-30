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
