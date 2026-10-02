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

// Task 2.3 (ptone/scion#2197, master design ptone/scion#2184, contract
// .design/agent-keys-contract.md §6.1): the temporary message-raw bridge.
//
// tryAgentKeysMessageBridge classifies a legacy /message request as raw or
// not *before* authorizeAgentMessage runs on either public route shape
// (handlers_agents_core.go's AgentActionMessage branch, handlers_projects_
// core.go's AgentActionMessage branch). This placement is an invariant, not
// an implementation convenience: authorizeAgentMessage can deny a request
// before handleAgentMessage is ever reached, so classifying raw *inside*
// handleAgentMessage (the natural reading of "both routes share one
// handler") would only ever run after message-mode authorization already
// passed -- breaking the auth-parity contract requires (AK-24/AK-25) and
// decision 5 ("message modes do not grant or deny keys").
//
// When this function selects raw, the request is handled by the bridge
// entirely: it is normalized into an agentkeys.Request and delegated to the
// same authorizeAgentKeys / admitAndDispatchAgentKeys path the direct
// /keys routes use (task 2.2) -- never reimplemented, never bypassed. When
// it does not select raw (an ordinary message, Plain, or a body this
// function could not parse), the request falls through completely
// unaffected: the HTTP body is restored byte-for-byte so the existing
// authorizeAgentMessage -> handleAgentMessage path sees exactly what it
// would have seen without this function existing at all.
//
// No conversation resolution, message persistence, observer/mention/
// notification side effect, or legacy message validation runs on the
// raw-selected path: every outcome (accepted or rejected) is produced
// before any of those exist on this path at all (contract §6.1, issue
// #2197 acceptance criteria).

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/agentkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// agentKeysBridgePreAuthMaxBodyBytes bounds the bridge's own pre-
// authorization body read (contract §6.1/§6.4, AK-52). No bound of any kind
// exists on the message path today (readJSON is a bare json.NewDecoder.
// Decode with no MaxBytesReader anywhere upstream of it) -- this is a new,
// disclosed behaviour change the bridge introduces, not a reused limit. See
// the contract's §6.1 arithmetic for why 2 MiB (not, say, 1 MiB) is the
// chosen ceiling: it leaves roughly 1 MiB of headroom over the worst case
// of every *bounded* field in a legacy message body.
const agentKeysBridgePreAuthMaxBodyBytes = 2 * 1024 * 1024

// tryAgentKeysMessageBridge is called from both routers' AgentActionMessage
// branch, after the target agent is already resolved (the same resolution
// authorizeAgentMessage itself would use) and after the caller is already
// known to be authenticated, but before authorizeAgentMessage runs.
//
// target is the already-resolved agent the surrounding dispatch code
// resolved for this request (contract §6.1: "no separate bridge resolution
// step exists"). urlAgentRef is the raw URL path segment the client sent
// for the agent ({id} on the top-level route, {id-or-slug} on the
// project-scoped route) -- used only for the legacy recipient-field
// comparison (contract §6.1's exact recipient rule). keysPath is the
// successor /keys URL surfaced to the caller as migration guidance.
//
// Returns handled == true when the response has already been written in
// full (a dispatch outcome or any rejection) and the caller must return
// immediately without calling authorizeAgentMessage or handleAgentMessage.
// Returns handled == false when r.Body has been restored byte-for-byte and
// the caller must proceed exactly as before this function existed.
//
// projectScoped distinguishes which router called this function: it governs
// step 6's own ordering, which must mirror whichever route shape's direct
// /keys flow it stands in for (see agentKeysBridgeCrossProjectDecision).
func (s *Server) tryAgentKeysMessageBridge(w http.ResponseWriter, r *http.Request, target *store.Agent, urlAgentRef, keysPath string, projectScoped bool) (handled bool) {
	if r.Body == nil {
		return false
	}

	limited := http.MaxBytesReader(w, r.Body, agentKeysBridgePreAuthMaxBodyBytes)
	body, err := io.ReadAll(limited)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			// Generic, non-keys 413: raw-vs-not is not yet known at this
			// point, so this is not an agentkeys.Outcome at all (contract
			// §6.1/§6.4). No operation ID: no operation is recognized to
			// exist yet, the same as any other pre-classification failure.
			writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large",
				"request body exceeds the maximum size", nil)
			return true
		}
		// Any other read failure (truncated body, client disconnect): the
		// bytes actually read might still happen to decode as a complete,
		// raw-selected JSON value even though the read did not finish
		// successfully (classification only requires a valid *leading*
		// JSON value; trailing bytes are already ignored by construction,
		// AK-54) -- reproduced in review: a body whose first Read call
		// returns a complete raw document together with a non-EOF error.
		// Falling through unconditionally in that case would let an
		// incomplete, accidentally-raw-shaped read reach the legacy
		// message path as if it were trustworthy input, breaking the
		// branch-point invariant and the zero-side-effect guarantee.
		// Classify the partial bytes with the identical step-1 decode; a
		// raw-selected result here is never processed further -- the read
		// itself is not trustworthy enough to admit, authorize or dispatch
		// -- so it fails closed as a bridge-shaped 400, not a silent
		// fall-through and not a guess at what the client meant to send.
		var partial MessageRequest
		if decErr := json.NewDecoder(bytes.NewReader(body)).Decode(&partial); decErr == nil &&
			(partial.Raw || (partial.StructuredMessage != nil && partial.StructuredMessage.Raw)) {
			// This is a bridge response like any other raw-selected outcome
			// (review r2 finding 3): a client whose read broke mid-flight is
			// exactly the kind of legacy caller the migration guidance (and
			// its explicit retry-limitation text) is for, so the headers
			// must be set here too, not only on the steps-2-9 success path.
			setAgentKeysBridgeMigrationHeaders(w, keysPath)
			s.writeAgentKeysBridgeFixedRejection(w, r, len(body), agentkeys.OutcomeInvalidRequest,
				"request body could not be read in full")
			return true
		}
		// Not raw (or not decodable at all): best-effort restore of
		// whatever was read, then let the unchanged message path hit the
		// same underlying condition itself and produce today's existing
		// error, rather than inventing a new one here.
		r.Body = io.NopCloser(bytes.NewReader(body))
		return false
	}

	restore := func() { r.Body = io.NopCloser(bytes.NewReader(body)) }

	// Step 1 (contract §6.1 "Bridge check order"): classification parity.
	// Decode the identical buffered bytes with the identical mechanism
	// handleAgentMessage's own readJSON uses -- the same MessageRequest
	// type, via encoding/json.Decoder.Decode -- so case-variant field
	// names, duplicate keys and trailing bytes classify identically on
	// both sides by construction (AK-53/AK-54). Any non-nil Decode error
	// means not-raw regardless of which fields were already populated
	// before it occurred (AK-59): this request falls through to the
	// unchanged message path, where readJSON will hit the identical error.
	var req MessageRequest
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&req); err != nil {
		restore()
		return false
	}

	rawSelected := req.Raw || (req.StructuredMessage != nil && req.StructuredMessage.Raw)
	if !rawSelected {
		restore()
		return false
	}

	// From here on this request never reaches authorizeAgentMessage or
	// handleAgentMessage (contract §6.1's branch-point invariant): it is
	// handled by the bridge in full, ending in authorizeAgentKeys /
	// ExecuteAgentKeys exclusively or an explicit rejection.
	setAgentKeysBridgeMigrationHeaders(w, keysPath)

	// Step 2: raw+plain conflict -- 400, no operation ID.
	plainSelected := req.Plain || (req.StructuredMessage != nil && req.StructuredMessage.Plain)
	if plainSelected {
		s.writeAgentKeysBridgeFixedRejection(w, r, len(body), agentkeys.OutcomeInvalidRequest,
			"raw and plain are mutually exclusive")
		return true
	}

	// Steps 3-4: keys-content parity. Re-decode the identical buffered
	// bytes into a shadow struct whose JSON tags match MessageRequest/
	// StructuredMessage exactly, then strictly decode each present
	// candidate with agentkeys.ValidateKeysJSON -- never reusing the
	// lenient string classification above already produced, which would
	// silently repair invalid UTF-8 / lone surrogate escapes into U+FFFD
	// instead of rejecting them (contract's "Keys-content parity", AK-60).
	var shadow struct {
		Message           json.RawMessage `json:"message"`
		StructuredMessage struct {
			Msg json.RawMessage `json:"msg"`
		} `json:"structured_message"`
	}
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&shadow); err != nil {
		// Unreachable in practice (step 1's decode into the richer
		// MessageRequest type already succeeded against the same bytes),
		// but fail closed with a fixed, content-free message rather than
		// assume it cannot happen.
		s.writeAgentKeysBridgeFixedRejection(w, r, len(body), agentkeys.OutcomeInvalidRequest, "invalid JSON")
		return true
	}

	msgVal, msgPresent, msgErr := decodeAgentKeysBridgeCandidate(shadow.Message)
	nestedVal, nestedPresent, nestedErr := decodeAgentKeysBridgeCandidate(shadow.StructuredMessage.Msg)

	var keys string
	switch {
	case msgErr != nil:
		s.writeAgentKeysBridgeMalformed(w, r, len(body), msgErr)
		return true
	case nestedErr != nil:
		s.writeAgentKeysBridgeMalformed(w, r, len(body), nestedErr)
		return true
	case msgPresent && nestedPresent && msgVal != nestedVal:
		// AK-41: two present, non-null, differing candidates -- rejected,
		// not merged, regardless of which one an implementation happens to
		// check first (keys-content parity's "either order" requirement).
		s.writeAgentKeysBridgeFixedRejection(w, r, len(body), agentkeys.OutcomeInvalidRequest,
			"message and structured_message.msg disagree")
		return true
	case !msgPresent && !nestedPresent:
		// Neither field present (or both null): no keys content at all --
		// the same "empty" shape AK-7/AK-38 reject, by a different route
		// (absent vs. empty-string), with the identical outcome.
		s.writeAgentKeysBridgeFixedRejection(w, r, len(body), agentkeys.OutcomeInvalidRequest,
			"message or structured_message.msg is required")
		return true
	case msgPresent:
		keys = msgVal
	default:
		keys = nestedVal
	}

	// Step 5: mint the operation ID now that every malformed-input check
	// (steps 2-4) has passed, exactly matching contract §2.5's general
	// policy ("minted as soon as validation succeeds, before authorization
	// and before any resolution outcome is reported"). Every step from
	// here on carries it.
	operationID := uuid.NewString()

	// Step 6: agent cross-project refusal, strictly before step 7's
	// unsupported-combination check (contract's "Unambiguous precedence",
	// AK-51) -- an input that would trigger both always yields
	// cross_project_keys_unsupported, never raw_combination_unsupported.
	// See agentKeysBridgeCrossProjectDecision for why this step's own
	// ordering is itself route-shape-dependent (lead ruling on review r1
	// finding 7: the bridge matches direct /keys on the same route shape,
	// and the two shapes' own orderings already differ -- 2.1/2.2's
	// approved ordering, not something 2.3 may change).
	if denial := s.agentKeysBridgeCrossProjectDecision(r, target, projectScoped); denial != nil {
		s.finishAgentKeysDeniedDecision(w, r, operationID,
			agentKeysAuditTarget{AgentID: target.ID, ProjectID: target.ProjectID}, len(keys),
			*denial, agentKeysRouteRawBridge)
		return true
	}

	// Step 7: every other "Rejected" row in contract §6.1's legacy field
	// table -- 422 raw_combination_unsupported on the first one that
	// fails.
	if violation := agentKeysBridgeCombinationViolation(&req, target, urlAgentRef); violation != "" {
		s.finishAgentKeysDeniedDecision(w, r, operationID,
			agentKeysAuditTarget{AgentID: target.ID, ProjectID: target.ProjectID}, len(keys),
			KeysAuthzDecision{Allowed: false, Outcome: agentkeys.OutcomeRawCombinationUnsupported, Reason: violation},
			agentKeysRouteRawBridge)
		return true
	}

	// Step 8: the same, single authorization gate the direct /keys routes
	// use -- never authorizeAgentMessage (contract's auth-parity
	// requirement, AK-24/AK-25; this also carries the #2460 attach-
	// relationship requirement for agent callers automatically, since it
	// is the identical function).
	decision := s.authorizeAgentKeys(r, target)
	if !decision.Allowed {
		s.finishAgentKeysDenied(w, r, operationID, target, len(keys), decision, agentKeysRouteRawBridge)
		return true
	}

	// Step 9: delegate to the identical admission/dispatch/audit path task
	// 2.2 already implements -- budgets, running-phase/runtime checks, the
	// execute-before deadline, one typed dispatch, and the terminal audit
	// and response. The bridge never reimplements, reorders, or bypasses
	// any of this.
	s.admitAndDispatchAgentKeys(w, r, operationID, target, keys, agentKeysRouteRawBridge)
	return true
}

// agentKeysBridgeCrossProjectDecision evaluates the bridge's step 6
// (contract §6.1) with a route-shape-dependent ordering, so the bridge
// matches whichever route shape's own direct /keys decision it stands in
// for (review r1 finding 7, lead ruling): the two shapes do not decide a
// missing-scope-and-cross-project agent caller the same way, because their
// approved 2.1/2.2 orderings differ, not because the contract wants two
// different outcomes for the same caller.
//
//   - Project-scoped: delegates to authorizeAgentKeysCrossProject exactly as
//     handleAgentActionKeysProjectScoped does -- the project-scoped /keys
//     route decides this refusal from the caller's identity and the URL's
//     own resolved project alone, before any target-agent lookup and
//     therefore before ScopeAgentLifecycle is ever checked (contract §3.1
//     invariant 4, AK-21c). A missing-scope, cross-project caller still
//     gets cross_project_keys_unsupported on this shape.
//   - Top-level: authorizeAgentKeys itself checks ScopeAgentLifecycle
//     before project equality (authorize_agentkeys.go's agent-credential
//     branch), so a missing-scope, cross-project caller gets keys_denied,
//     not cross_project_keys_unsupported, on the top-level /keys route.
//     Reproducing that here requires checking scope first: when scope is
//     absent this function returns nil (no decision yet) so step 8's full
//     authorizeAgentKeys call decides it exactly as top-level /keys would;
//     when scope is present and the project differs, this function denies
//     here (before step 7's field-table check), preserving the contract's
//     own cross-project-before-combination precedence (AK-51) for a caller
//     who does hold the scope.
//
// Both branches call denyAgentKeysCrossProject (never a hand-rolled
// KeysAuthzDecision), so the bridge's audit trail carries the identical
// logAuthzDenial record a direct /keys cross-project denial would, on
// either shape. Both branches also gate on identity.Type() == "agent"
// specifically (not just a successful AgentIdentity type assertion), so a
// federated agent identity (ProjectID() == "") is never matched here and
// instead reaches step 8, where authorizeAgentKeys's default branch denies
// it with the ordinary keys_denied every other unsupported principal kind
// gets -- exactly like a federated caller hitting /keys directly.
func (s *Server) agentKeysBridgeCrossProjectDecision(r *http.Request, target *store.Agent, projectScoped bool) *KeysAuthzDecision {
	if projectScoped {
		return s.authorizeAgentKeysCrossProject(r, target.ProjectID)
	}

	identity := GetIdentityFromContext(r.Context())
	if identity == nil || identity.Type() != "agent" {
		return nil
	}
	agentIdent, ok := identity.(AgentIdentity)
	if !ok {
		return nil
	}
	if !agentIdent.HasScope(ScopeAgentLifecycle) {
		// Defer entirely to step 8: a direct top-level /keys call would
		// deny this caller with keys_denied (missing scope, checked before
		// project) regardless of project, so this function must not
		// preempt that with cross_project_keys_unsupported.
		return nil
	}
	if agentIdent.ProjectID() == target.ProjectID {
		return nil
	}
	// Same reason text authorizeAgentKeysCrossProject itself uses (and
	// denyAgentKeysCrossProject's own "keys: " prefix already identifies the
	// gate; the audit record's "route" field already identifies the surface
	// -- review r2 finding 2), so this is the one place the bridge's own
	// authz-denial record would otherwise differ from a direct /keys denial
	// for no reason.
	denial := s.denyAgentKeysCrossProject(r, agentResource(target), "agent project mismatch")
	return &denial
}

// decodeAgentKeysBridgeCandidate interprets raw as one of: absent (len(raw)
// == 0, the key never appeared in the body), a JSON null literal (treated
// identically to absent per contract §6.1), or a JSON string decoded with
// agentkeys.ValidateKeysJSON. A non-string, non-null value is reported as
// present with a decode error (ValidateKeysJSON itself rejects anything not
// starting with a quote).
func decodeAgentKeysBridgeCandidate(raw json.RawMessage) (value string, present bool, err error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return "", false, nil
	}
	s, verr := agentkeys.ValidateKeysJSON(raw)
	if verr != nil {
		return "", true, verr
	}
	return s, true, nil
}

// agentKeysBridgeCombinationViolation evaluates every "Rejected if
// set/true/non-empty" row of the contract's §6.1 legacy field table that
// decodeAgentKeysBridgeCandidate and the raw/plain checks above do not
// already cover, returning a short, fixed, content-free reason string on
// the first violation found, or "" if none applies. Every rejection here
// is 422 raw_combination_unsupported (AK-43) and affects zero targets.
//
// Fields not referenced here (sender/sender_id, version/timestamp/type,
// urgent, status) are the table's "Ignored" rows: never read for any
// decision.
func agentKeysBridgeCombinationViolation(req *MessageRequest, target *store.Agent, urlAgentRef string) string {
	switch {
	case req.Interrupt:
		return "interrupt is not supported for raw delivery via the message bridge"
	case req.Notify:
		return "notify is not supported for raw delivery via the message bridge"
	case req.Wake:
		return "wake is not supported for raw delivery via the message bridge"
	case len(req.Mentions) > 0:
		return "mentions are not supported for raw delivery via the message bridge"
	case req.Surface != "":
		return "surface-addressed conversation resolution is not supported for raw delivery via the message bridge"
	case req.ExternalRef != "":
		return "external_ref-addressed conversation resolution is not supported for raw delivery via the message bridge"
	case req.ParentRef != "":
		return "parent_ref-addressed conversation resolution is not supported for raw delivery via the message bridge"
	}

	sm := req.StructuredMessage
	if sm == nil {
		return ""
	}

	switch {
	case sm.Recipients != "":
		return "multiple recipients are not supported for raw delivery via the message bridge"
	case len(sm.Attachments) > 0:
		return "attachments are not supported for raw delivery via the message bridge"
	case len(sm.Metadata) > 0:
		return "metadata is not supported for raw delivery via the message bridge"
	case sm.Channel != "":
		return "channel addressing is not supported for raw delivery via the message bridge"
	case sm.ThreadID != "":
		return "thread_id addressing is not supported for raw delivery via the message bridge"
	case sm.ConversationID != "":
		return "conversation_id addressing is not supported for raw delivery via the message bridge"
	case sm.DeliveryText != "":
		return "delivery_text is not supported for raw delivery via the message bridge"
	case sm.Broadcasted:
		return "broadcasted delivery is not supported for raw delivery via the message bridge"
	case sm.ObserverOnly:
		return "observer-only delivery is not supported for raw delivery via the message bridge"
	case sm.RecipientID != "" && sm.RecipientID != target.ID:
		return "recipient_id does not match the request target"
	case sm.Recipient != "" && !agentKeysBridgeRecipientMatches(sm.Recipient, target, urlAgentRef):
		return "recipient does not match the request target"
	}
	return ""
}

// agentKeysBridgeRecipientMatches implements the contract's exact
// recipient-comparison rule (§6.1): a bare "not naming the URL's target"
// rule is not precise enough, because legacy callers may address the
// target by its raw URL segment, canonical slug, UUID, or an unslugified
// display name.
func agentKeysBridgeRecipientMatches(recipient string, target *store.Agent, urlAgentRef string) bool {
	if messages.IsGroupRecipient(recipient) || strings.HasPrefix(recipient, "user:") {
		return false
	}
	value := strings.TrimPrefix(recipient, "agent:")
	switch value {
	case urlAgentRef, target.Slug, target.ID:
		return true
	}
	return target.Slug != "" && api.Slugify(value) == target.Slug
}

// writeAgentKeysBridgeMalformed writes a malformed-input rejection (steps
// 2-4) produced by agentkeys.ValidateKeysJSON, preserving its Outcome
// (OutcomeInvalidRequest or OutcomePayloadTooLarge) rather than collapsing
// every such error to one fixed code.
func (s *Server) writeAgentKeysBridgeMalformed(w http.ResponseWriter, r *http.Request, bodyBytes int, err error) {
	outcome := agentkeys.OutcomeInvalidRequest
	if ve, ok := agentkeys.AsValidationError(err); ok {
		outcome = ve.Outcome
	}
	s.logAgentKeysValidationAudit(r, outcome, bodyBytes, agentKeysRouteRawBridge)
	writeAgentKeysValidationError(w, outcome, err.Error())
}

// writeAgentKeysBridgeFixedRejection writes a bridge-specific malformed-
// input rejection (steps 2-4) that has no corresponding
// agentkeys.ValidationError of its own (the raw+plain conflict, a
// message/structured_message.msg disagreement, or neither candidate being
// present).
func (s *Server) writeAgentKeysBridgeFixedRejection(w http.ResponseWriter, r *http.Request, bodyBytes int, outcome agentkeys.Outcome, message string) {
	s.logAgentKeysValidationAudit(r, outcome, bodyBytes, agentKeysRouteRawBridge)
	writeAgentKeysValidationError(w, outcome, message)
}

// setAgentKeysBridgeMigrationHeaders attaches deprecation/migration
// guidance to every bridge response (accepted or rejected) as response
// headers, never as part of the frozen JSON envelope (contract §2.4/§2.4a
// freeze agentkeys.Response and the error envelope shape; these headers
// are additive transport metadata, not a new field on either). keysPath is
// the successor /keys URL for this exact request (top-level or
// project-scoped, matching the route the legacy request arrived on).
//
// The migration guidance explicitly states the legacy-client retry
// limitation (issue #2197 scope: "never claim unchanged legacy clients are
// end-to-end replay-safe"): this bridge cannot disable retries already
// configured in an old message-API client, so a 5xx/timeout from this
// bridge may still cause that client to resend.
func setAgentKeysBridgeMigrationHeaders(w http.ResponseWriter, keysPath string) {
	w.Header().Set("Deprecation", "true")
	w.Header().Set("Link", fmt.Sprintf("<%s>; rel=\"successor-version\"", keysPath))
	w.Header().Set("X-Scion-Keys-Migration", fmt.Sprintf(
		"Raw keystroke delivery via this message endpoint is deprecated; use POST %s instead. "+
			"This compatibility bridge cannot disable retries already configured in an old "+
			"message-API client -- upgrade the client or disable its retry/backoff before "+
			"relying on raw delivery through this endpoint.", keysPath))
}
