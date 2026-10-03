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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/agentkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
	"github.com/google/uuid"
)

// Raw keystroke delivery through message requests has been removed
// (.design/agent-keys-contract.md §6, AK-35/AK-36). Keystrokes are sent
// only through the dedicated keys routes (POST /api/v1/agents/{id}/keys and
// its project-scoped twin, `scion keys` in the CLI).
//
// StructuredMessage and every message request DTO no longer have a Raw
// field, so encoding/json would silently drop a "raw" member and turn an old
// raw request into an ordinary message. This file is the rejection adapter
// that prevents that: every message ingress that used to accept raw probes
// the request body with messages.HasRetiredRawField before decoding it, and
// answers a match with 422 raw_input_removed before any side effect
// (authorization side effects, sender synthesis, conversation resolution,
// mention work, attachment ingestion, wake, persistence, events or
// dispatch). Any value counts, including false, null, a wrong type or a
// malformed value, in either the top-level or the nested spelling.
//
// The ingresses, and where each probe runs:
//
//   - POST /api/v1/agents/{id}/message and
//     POST /api/v1/projects/{p}/agents/{a}/message: top-level "raw" and
//     "structured_message.raw", after target resolution and before
//     authorizeAgentMessage (where the removed bridge used to run).
//   - POST /api/v1/projects/{p}/broadcast: top-level "raw" and
//     "structured_message.raw", before decoding.
//   - POST /api/v1/broker/inbound and /api/v1/broker/inbound/routed: top-level
//     "raw" and "message.raw", before decoding and therefore before plugin
//     sender synthesis.
//   - Scheduled-event and recurring-schedule advanced Payload JSON: a
//     top-level "raw" key, after the object-shape check and before the
//     event-type decode (validateAndRejectScheduledPayload).
//
// The runtime broker's own /message handler carries the same probe
// (pkg/runtimebroker), so a hub that never sends raw is not the only
// defence on the broker side.

// rawTombstonePreAuthMaxBodyBytes bounds the buffered body read of every
// message ingress the tombstone probes: the two public /message routes,
// project broadcast, and broker inbound/routed. The removed raw bridge
// introduced this 2 MiB bound on the /message routes (contract §6.1/§6.4,
// AK-52) because it buffered the body before authorization ran; the
// tombstone buffers at the same point on every ingress, so every ingress
// keeps a bound. A larger body gets a generic 413 that is not a keys
// outcome.
const rawTombstonePreAuthMaxBodyBytes = 2 * 1024 * 1024

// rawIngress names the ingress a raw_input_removed rejection came from, for
// the response details and the audit record. Content-free by construction.
type rawIngress string

const (
	rawIngressAgentMessage        rawIngress = "agent_message"
	rawIngressProjectAgentMessage rawIngress = "project_agent_message"
	rawIngressBroadcast           rawIngress = "broadcast"
	rawIngressBrokerInbound       rawIngress = "broker_inbound"
	rawIngressBrokerInboundRouted rawIngress = "broker_inbound_routed"
	rawIngressScheduledPayload    rawIngress = "scheduled_payload"
)

// rawInputRemovedReplacement is the generic replacement named in a
// raw_input_removed response. The responses never name a resolved target:
// the message routes reject the retired field before message
// authorization, so echoing the target's ID would map a slug to a UUID for
// a caller who may have no authority on that agent.
const rawInputRemovedReplacement = "POST /api/v1/agents/{id}/keys"

// rawInputRemovedProjectReplacement is the generic replacement named by the
// project-scoped message route.
const rawInputRemovedProjectReplacement = "POST /api/v1/projects/{projectId}/agents/{agentIdOrSlug}/keys"

// rejectRetiredRawMessageBody buffers r.Body (bounded by maxBytes when it is
// positive), restores it byte-for-byte for the caller's own decode, and
// rejects the request with 422 raw_input_removed when the buffered bytes
// carry a retired raw member at the top level or inside one of nested.
//
// Returns handled == true when a response has already been written (a
// raw_input_removed rejection, or a 413 for a body over maxBytes) and the
// caller must return without any further processing. Returns false when the
// caller must continue exactly as it would have without this check.
//
// A read error other than the size bound fails closed when the bytes read so
// far already carry a raw member: an incomplete read is never trusted to
// reach the message path, and it can never be delivered as keys either.
// Otherwise the partial bytes are restored and the caller's own decode
// reports the condition as it always has.
func (s *Server) rejectRetiredRawMessageBody(w http.ResponseWriter, r *http.Request, ingress rawIngress, target agentKeysAuditTarget, replacement string, maxBytes int64, nested ...string) (handled bool) {
	if r.Body == nil {
		return false
	}
	reader := io.Reader(r.Body)
	if maxBytes > 0 {
		reader = http.MaxBytesReader(w, r.Body, maxBytes)
	}
	body, err := io.ReadAll(reader)
	r.Body = io.NopCloser(bytes.NewReader(body))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large",
				"request body exceeds the maximum size", nil)
			return true
		}
	}
	if !messages.HasRetiredRawField(body, nested...) {
		return false
	}
	s.writeRawInputRemoved(w, r, ingress, target, replacement)
	return true
}

// writeRawInputRemoved writes the 422 raw_input_removed response and its
// content-free audit record. Like every keys outcome decided inside an
// authenticated handler, it carries a fresh operation ID (contract §2.5's
// operation-ID policy lists raw_input_removed among the outcomes that do).
// The response never echoes any part of the request.
func (s *Server) writeRawInputRemoved(w http.ResponseWriter, r *http.Request, ingress rawIngress, target agentKeysAuditTarget, replacement string) {
	if replacement == "" {
		replacement = rawInputRemovedReplacement
	}
	operationID := uuid.NewString()
	s.logRawInputRemovedAudit(r, operationID, ingress, target)
	writeError(w, http.StatusUnprocessableEntity, string(agentkeys.OutcomeRawInputRemoved),
		messages.RawInputRemovedMessage, map[string]interface{}{
			"operation_id": operationID,
			"ingress":      string(ingress),
			"replacement":  replacement,
		})
}

// logRawInputRemovedAudit writes the same content-free "agent keys audit"
// record shape logAgentKeysAudit writes, tagged with the
// agentKeysRouteRawRemoved route and the ingress name. It never logs any
// part of the request body.
func (s *Server) logRawInputRemovedAudit(r *http.Request, operationID string, ingress rawIngress, target agentKeysAuditTarget) {
	ctx := r.Context()
	actorType, actorID, sourceProjectID, credential := agentKeysAuditActor(ctx)
	slog.Info("agent keys audit",
		"event", string(agentKeysAuditEventOutcome),
		"operation_id", operationID,
		"actor_type", actorType,
		"actor_id", actorID,
		"source_project_id", sourceProjectID,
		"target_agent_id", target.AgentID,
		"target_project_id", target.ProjectID,
		"credential_kind", string(credential.Kind),
		"credential_id", credential.ID,
		"route", string(agentKeysRouteRawRemoved),
		"ingress", string(ingress),
		"input_bytes", 0,
		"decision", string(agentkeys.OutcomeRawInputRemoved),
		"duration_ms", int64(0),
		"request_id", logging.RequestIDFromContext(ctx),
	)
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
// when the payload doesn't yield a target), and the raw-key probe only
// looks for a "raw" member — neither one is the authoritative shape check.
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
		// {message, dispatch_agent} set before reaching here, so this branch
		// is unreachable in practice. Fail closed for an unrecognized event
		// type rather than falling back to a lenient syntax-only check: if a
		// future event type is ever added to the closed set without a
		// matching case here, its payload must not silently skip structural
		// validation.
		decodeErr = fmt.Errorf("unsupported event type: %s", eventType)
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
//  2. messages.HasRetiredRawField: the "raw" key tombstone -> 422
//     raw_input_removed
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
func (s *Server) validateAndRejectScheduledPayload(w http.ResponseWriter, r *http.Request, eventType, payload string) bool {
	if payload == "" {
		return true
	}
	if !s.validateScheduledPayloadShape(w, payload) {
		return false
	}
	if messages.HasRetiredRawField([]byte(payload)) {
		s.writeRawInputRemoved(w, r, rawIngressScheduledPayload, agentKeysAuditTarget{}, "")
		return false
	}
	return s.validateScheduledEventPayloadJSON(w, eventType, payload)
}
