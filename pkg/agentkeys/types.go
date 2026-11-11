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

package agentkeys

import "net/http"

// MaxBytes is the frozen size ceiling for the "keys" field, in UTF-8 bytes,
// checked before dispatch. See .design/agent-keys-contract.md "Concrete
// defaults".
const MaxBytes = 4096

// MaxHTTPBodyBytes bounds the raw request body read before JSON decoding,
// independent of and larger than MaxBytes. A JSON string can expand an
// input byte into a 6-byte \u00XX escape in the worst case, plus a small
// envelope ({"keys":"..."}); this ceiling is sized so that no *compactly
// encoded* request within MaxBytes is ever rejected by the body-size check,
// while still bounding worst-case allocation for oversized/malicious bodies
// before the byte-ceiling check runs. This qualification matters: JSON
// permits unlimited insignificant whitespace between tokens, so a request
// padded with enough of it could exceed MaxHTTPBodyBytes while still
// encoding a keys value within MaxBytes — such a request is rejected at the
// transport-level read (413), which is the correct outcome for a body that
// is not compactly encoded, not a bug in this ceiling. See
// TestMaxHTTPBodyBytes_CoversWorstCaseCompactEncoding for the arithmetic
// this ceiling must keep satisfying.
const MaxHTTPBodyBytes = 32 * 1024

// Request is the public wire shape both route shapes accept:
//
//	POST /api/v1/agents/{id}/keys
//	POST /api/v1/projects/{project}/agents/{id-or-slug}/keys
//
// Requests accept only this field; unknown fields, duplicate "keys" keys,
// non-string values, arrays and NUL bytes are rejected (ValidateBody).
// Whitespace is preserved verbatim: implementations must never trim, split,
// or tokenize Keys.
type Request struct {
	Keys string `json:"keys"`
}

// Response is the public success shape. HTTP 200 means the broker
// acknowledged successful terminal injection, not that the harness consumed
// or obeyed it. OperationID correlates audit and error records; it is not an
// idempotency key or a durable status resource.
type Response struct {
	Status      string `json:"status"`
	OperationID string `json:"operation_id"`
	AgentID     string `json:"agent_id"`
}

// StatusDispatched is the only defined value of Response.Status.
const StatusDispatched = "dispatched"

// Error responses reuse pkg/hub's existing, already-universal envelope
// (pkg/hub.ErrorResponse{Error: pkg/hub.APIError{Code, Message, Details,
// RequestID}}, written by pkg/hub/errors.go's writeError and parsed
// client-side by pkg/apiclient.ParseErrorResponse — the same shape every
// other Hub route, including 401 from the shared auth middleware, already
// uses). Keys introduces no new envelope type: Code is set to the Outcome
// string, and Details carries "operation_id" as a plain map entry
// (map[string]interface{}{"operation_id": id}) only for outcomes that have
// one — see the OperationID policy on the Outcome type doc below. This
// package defines no Go type for the envelope itself because the one that
// already exists in pkg/hub covers it exactly; agentkeys only needs to
// supply the Code string (Outcome) and know which outcomes get a Details
// entry, both of which live below.

// Outcome is a stable, machine-readable outcome/denial code, common to both
// public route shapes and the internal ExecuteAgentKeys operation. Values
// match .design/agent-keys-contract.md's HTTP/machine-outcome table
// verbatim; do not rename without updating that table and this const block
// together.
//
// OperationID policy (a recorded deviation from #2184 — see the contract's
// §2.4-2.5): authentication always runs first, outside and before the keys
// handler, exactly as #2184's execution order requires ("authenticate and
// bounded decode → resolve → authorize → …") — pkg/hub's shared
// UnifiedAuthMiddleware answers an unauthenticated request with 401 before
// any handler, including the keys handler, ever runs, so OutcomeUnauthorized
// is emitted with no operation ID: the request never reaches the code that
// would mint one. Once inside the (now-authenticated) handler, an operation
// ID is minted as soon as request validation succeeds
// (ValidateBody/ValidateKeys returning nil), before authorization
// (authorizeAgentKeys) runs, and from that point is included in every
// subsequent response and audit record for the request: OutcomeKeysDenied,
// OutcomeNotFound, OutcomeAgentNotRunning, OutcomeTerminalNotReady,
// OutcomeCrossProjectKeysUnsupported, OutcomeKeysUnsupported,
// OutcomeRawInputRemoved, OutcomeKeysRateLimited, OutcomeKeysUnavailable,
// OutcomeKeysOutcomeUnknown, and OutcomeDispatched. Only OutcomeUnauthorized (never reaches a handler),
// OutcomeInvalidRequest and OutcomePayloadTooLarge (both failures during
// validation itself, inside the handler but before an operation is
// recognized to exist) carry no operation ID. #2184 only requires
// "post-admission" results (after budget charge and phase/route checks) to
// carry one; this contract extends that floor to cover every in-handler
// denial too — not-found, keys_denied, and the agent cross-project refusal,
// all of which #2184's own execution order places before admission —
// because giving every audit-logged decision a stable, opaque correlation
// key costs nothing once the handler is running and is simpler to implement
// correctly than tracking the admission-phase boundary per outcome. It does
// not, and cannot, extend the floor to outcomes decided before the handler
// runs at all.
//
// Handlers must emit OutcomeNotFound (404) themselves, in this envelope,
// rather than reusing another resolver's pre-existing 404 shape that does
// not match it — for example, the project-scoped route's existing agent
// resolver writes code "agent_not_found" with
// details={"agent_slug",...,"project_id":...} (handlers_projects_core.go
// ~2408-2420) for its own callers; a keys handler behind that resolver must
// still answer with code "not_found" and (once authenticated and validated)
// an operation_id, not silently inherit the resolver's different code and
// details shape.
type Outcome string

const (
	// OutcomeDispatched is the success outcome (HTTP 200).
	OutcomeDispatched Outcome = "dispatched"

	// OutcomeInvalidRequest covers malformed shape: unknown/duplicate
	// fields, non-string or missing "keys", empty string, NUL byte (HTTP 400).
	OutcomeInvalidRequest Outcome = "invalid_request"

	// OutcomePayloadTooLarge means the keys field (or the request body)
	// exceeded its size ceiling (HTTP 413).
	OutcomePayloadTooLarge Outcome = "payload_too_large"

	// OutcomeUnauthorized means no authentication was presented (HTTP 401).
	OutcomeUnauthorized Outcome = "unauthorized"

	// OutcomeKeysDenied means authentication was presented but the caller
	// lacks live authority over the target (HTTP 403).
	OutcomeKeysDenied Outcome = "keys_denied"

	// OutcomeNotFound means the target agent does not exist or is out of
	// scope under the existing resource-disclosure policy (HTTP 404).
	OutcomeNotFound Outcome = "not_found"

	// OutcomeAgentNotRunning means the target cannot currently accept input
	// because it is not running (HTTP 409). No wake/start is performed.
	OutcomeAgentNotRunning Outcome = "agent_not_running"

	// OutcomeTerminalNotReady means the target is running but its terminal
	// session cannot currently accept input (HTTP 409).
	OutcomeTerminalNotReady Outcome = "terminal_not_ready"

	// OutcomeCrossProjectKeysUnsupported means an authenticated AGENT caller
	// tried to cross its own project boundary (HTTP 422). This is distinct
	// from a human operator selecting another project under their live
	// permissions and credential boundary, which is allowed — see the
	// contract's authorization table.
	OutcomeCrossProjectKeysUnsupported Outcome = "cross_project_keys_unsupported"

	// OutcomeKeysUnsupported means a managed backend, unsupported runtime,
	// or a broker lacking the keys route (HTTP 422). Never downgraded to a
	// message-based fallback.
	OutcomeKeysUnsupported Outcome = "keys_unsupported"

	// OutcomeRawInputRemoved is returned for a message request that still
	// carries the retired raw field (top level or nested, any value), with
	// guidance naming the keys route (HTTP 422). Raw keystroke delivery
	// through messages has been removed; nothing is delivered and no side
	// effect runs.
	OutcomeRawInputRemoved Outcome = "raw_input_removed"

	// OutcomeKeysRateLimited means an independent per-principal/project or
	// per-target budget was exceeded (HTTP 429). Retry-After describes
	// admission, not permission to replay an ambiguous operation.
	OutcomeKeysRateLimited Outcome = "keys_rate_limited"

	// OutcomeKeysUnavailable means dispatch definitively did not start:
	// offline broker, no immediate route, or expired admission (HTTP 503).
	OutcomeKeysUnavailable Outcome = "keys_unavailable"

	// OutcomeKeysOutcomeUnknown means dispatch may have run or partially
	// run (HTTP 502/504 or an uncertain local/network error). Never
	// automatically retried, and never reported as "delivered".
	OutcomeKeysOutcomeUnknown Outcome = "keys_outcome_unknown"
)

// HTTPStatus returns the HTTP status code the contract assigns to outcome.
// It returns (0, false) for OutcomeDispatched (success has its own 200
// response, not an error status) and for any unrecognized value.
//
// OutcomeKeysOutcomeUnknown maps to 502; callers whose local signal was a
// timeout rather than a bad gateway response should use 504 instead — both
// are contractually equivalent (uncertain outcome, no auto-retry) and
// HTTPStatus documents 502 as the representative code.
func HTTPStatus(o Outcome) (int, bool) {
	switch o {
	case OutcomeInvalidRequest:
		return http.StatusBadRequest, true
	case OutcomePayloadTooLarge:
		return http.StatusRequestEntityTooLarge, true
	case OutcomeUnauthorized:
		return http.StatusUnauthorized, true
	case OutcomeKeysDenied:
		return http.StatusForbidden, true
	case OutcomeNotFound:
		return http.StatusNotFound, true
	case OutcomeAgentNotRunning, OutcomeTerminalNotReady:
		return http.StatusConflict, true
	case OutcomeCrossProjectKeysUnsupported, OutcomeKeysUnsupported, OutcomeRawInputRemoved:
		return http.StatusUnprocessableEntity, true
	case OutcomeKeysRateLimited:
		return http.StatusTooManyRequests, true
	case OutcomeKeysUnavailable:
		return http.StatusServiceUnavailable, true
	case OutcomeKeysOutcomeUnknown:
		return http.StatusBadGateway, true
	default:
		return 0, false
	}
}
