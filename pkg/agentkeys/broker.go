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

import (
	"errors"
	"net/http"
	"time"
)

// BrokerRoutePath is the URL path template for the dedicated broker keys
// route, both as the Hub's public route and as the internal Hub->broker
// route (HTTP transport and, tunneled verbatim, the control-channel
// transport — see BrokerRouteMethod doc comment). "{id}" is the runtime
// broker's agent-resolution identifier: the agent's **slug**, matching
// every existing broker route (see brokerHTTPTransport.MessageAgent,
// pkg/hub/broker_http_transport.go:328-333, which passes agent.Slug as the
// path segment it calls "agentID"). It is not the Hub UUID —
// BrokerRequest.AgentID below is.
//
// This deliberately reuses the existing generic action-route shape
// (api.RuntimeBrokerAgentActionMethod's "/api/v1/agents/{id}/{action}"
// pattern, action="keys") so the broker's HTTP mux needs no new pattern —
// but the action's handler is dedicated: it must not call the shared
// sendMessage/message-logging code path (pkg/runtimebroker/handlers.go),
// per the "Option B" decision recorded in the contract doc. 1.1/1.2 register
// this action; 0.1 intentionally does not (see agent_actions.go's
// AgentActionKeys doc comment).
const BrokerRoutePath = "/api/v1/agents/{id}/keys"

// BrokerRouteMethod is the HTTP method for BrokerRoutePath.
//
// There is no separate control-channel RPC method registry in this
// codebase: pkg/hub/controlchannel_client.go tunnels the same HTTP method
// and path used by the HTTP transport inside a wsprotocol.RequestEnvelope
// (see ControlChannelBrokerClient.MessageAgent for the existing pattern).
// The "control-channel method name" the master design asks 0.1 to freeze is
// therefore this same constant: "POST", tunneled to BrokerRoutePath.
const BrokerRouteMethod = http.MethodPost

// BrokerProjectIDQueryParam is the query-string parameter carrying the
// canonical project ID on BrokerRoutePath, matching every existing broker
// route's "?projectId=" convention (e.g. brokerHTTPTransport.MessageAgent).
const BrokerProjectIDQueryParam = "projectId"

// DefaultAdmissionWindow is the maximum lifetime of a Hub-issued
// execute-before deadline for a single keys dispatch, from the moment the
// Hub admits the request. See CapExecuteBefore.
const DefaultAdmissionWindow = 30 * time.Second

// Target identifies the runtime broker and agent a keys dispatch is
// addressed to. It carries exactly the fields
// pkg/hub.AgentDispatcher's existing methods already take from an
// already-resolved, already-authorized *store.Agent (RuntimeBrokerID,
// agent.ID, agent.Slug, agent.ProjectID) — see
// HTTPAgentDispatcher.DispatchAgentMessage, pkg/hub/httpdispatcher.go:2728,
// which reads exactly these four fields off *store.Agent before calling
// RuntimeBrokerClient.MessageAgent.
//
// Target is a Hub-internal call parameter, never serialized: task 1.2's
// Dispatcher implementation builds a BrokerRequest from AgentID/ProjectID
// plus its own arguments and resolves BrokerClient's brokerID/brokerEndpoint
// from RuntimeBrokerID; only AgentSlug, ProjectID and the BrokerRequest
// fields cross the wire (via BrokerClient.ExecuteKeys).
type Target struct {
	// RuntimeBrokerID identifies which broker owns the agent, for endpoint
	// resolution and HMAC/broker-secret lookup — the same value every
	// existing AgentDispatcher method keys broker lookup on.
	RuntimeBrokerID string

	// AgentID is the canonical (Hub-resolved) agent UUID.
	AgentID string

	// AgentSlug is the broker-resolution identifier: goes in BrokerRoutePath's
	// "{id}" segment, exactly like every other broker route.
	AgentSlug string

	// ProjectID is the canonical (Hub-resolved) project ID owning the agent.
	ProjectID string

	// Runtime is the runtime type the Hub recorded for the agent (store
	// Agent.Runtime), or "" if none. It is sent as the broker's recorded
	// runtime query parameter so the broker looks for the agent only in
	// runtimes of that type (ptone/scion#2748); it is not part of
	// BrokerRequest's body.
	Runtime string
}

// BrokerRequest is the typed, internal Hub->broker keys dispatch contract —
// the exact body BrokerClient.ExecuteKeys sends. It carries strictly less
// than the public Request: no caller identity, no session/tmux options,
// nothing the broker could use to reinterpret scope. JSON field names match
// the existing broker wire convention (snake_case; compare
// brokerHTTPTransport.MessageAgent's "project_id", "message_id").
type BrokerRequest struct {
	// ProjectID is the canonical (Hub-resolved) project ID owning the
	// target agent, duplicated from the "projectId" query parameter into
	// the body per the existing MessageAgent convention. The broker
	// resolves the target agent by (AgentSlug from the path, ProjectID
	// here) using the same resolution rule as every other broker route
	// (pkg/runtimebroker/handlers.go's matchesAgent: slug/name/container-ID
	// match, then project-label/field match) — it must not fall back to an
	// unscoped name match.
	ProjectID string `json:"project_id"`

	// AgentID is the canonical (Hub-resolved) agent UUID being targeted, and
	// is a hard identity-binding requirement for task 1.1, not an
	// audit-only field: every runtime-broker container already carries this
	// value in its "agent_id" label (pkg/agent/run.go:1245, set from
	// opts.Env["SCION_AGENT_ID"] at run.go:99-103; the Hub dispatcher
	// populates that env var from the agent's canonical ID at
	// pkg/runtimebroker/start_context.go:403-405 — the label is not new and
	// nothing about it needs to be added). Task 1.1's dedicated keys handler
	// passes this field through as SendKeys's expectedAgentID argument;
	// SendKeys itself — not the handler — resolves the target, checks the
	// resolved container's "agent_id" label against it, and executes on
	// that same resolved container, all within one call (see SendKeys's doc
	// comment below). The handler must not resolve or check the label
	// itself before calling SendKeys: a separate handler-side check
	// followed by a second, independent resolution inside SendKeys would
	// leave exactly the gap this field exists to close open again — a
	// same-slug agent recreated (deleted and rebuilt, or restarted with a
	// new identity) between the two lookups would receive input meant for
	// the agent that was originally authorized. Fail closed on a missing or
	// empty "agent_id" label (e.g. a container started outside the Hub's
	// own dispatch path): SendKeys treats it as ErrTargetNotFound, the same
	// as a mismatch — never a slug-only match.
	AgentID string `json:"agent_id"`

	// OperationID correlates this dispatch across Hub and broker audit
	// records. It is not an idempotency key: the broker must not treat a
	// retried OperationID as "already handled" and must not use it to
	// suppress or dedupe a second injection.
	OperationID string `json:"operation_id"`

	// ExecuteBefore is the Hub-issued execution-admission deadline, in UTC.
	// Callers must call ExecuteBefore.UTC() before constructing a
	// BrokerRequest that will be marshaled: time.Time's JSON encoding
	// includes whatever zone offset the value carries, and the contract
	// requires comparing this deadline against the broker's own clock
	// unambiguously. Encoded as RFC 3339 with nanoseconds (Go's default
	// time.Time JSON encoding, e.g. "2026-09-29T12:00:00.123456789Z").
	//
	// The broker must reject the dispatch once time.Now().UTC() is at or
	// after this time — at broker admission, after any control-channel
	// semaphore/target-lock wait, and immediately before runtime execution
	// — and must not extend it. A zero value is invalid and must fail
	// closed (see CapExecuteBefore).
	ExecuteBefore time.Time `json:"execute_before"`

	// Keys is the exact byte-for-byte string to inject, already validated
	// against MaxBytes. The broker must not re-trim, re-split or otherwise
	// transform it before the single tmux send-keys call.
	Keys string `json:"keys"`
}

// BrokerResult is the typed result of a broker keys dispatch — the exact
// body BrokerClient.ExecuteKeys returns. Success is HTTP 200 with
// Outcome == OutcomeDispatched; there is no other success shape. Every other
// combination — a non-2xx status, or a 2xx body whose Outcome is not
// OutcomeDispatched — is a failure, and BrokerClient.ExecuteKeys must return
// it as (BrokerResult{}, error), not as a populated BrokerResult with a nil
// error: 2.2 only ever inspects Outcome after an error, via
// ClassifyDispatchError, never on the success path. See ClassifyDispatchError
// and BrokerOutcomeError for how a broker-decided failure Outcome reaches
// 2.2, and the HTTP status/body agreement rule there for what an adapter
// must do when the two disagree or the body cannot be parsed.
type BrokerResult struct {
	// OperationID echoes BrokerRequest.OperationID.
	OperationID string `json:"operation_id"`

	// Outcome is OutcomeDispatched on success. It is not otherwise present
	// on a value returned to 2.2 — a failure travels as an error (see above).
	Outcome Outcome `json:"outcome"`

	// Message is an optional human-readable detail. It must never contain
	// the injected key content, runtime command argv, or runtime
	// stdout/stderr — see the contract's audit and redaction rules.
	Message string `json:"message,omitempty"`
}

// brokerAssertableOutcomes is the allowlist of Outcome values the broker
// itself may decide and report back over the wire, via BrokerOutcomeError.
// Every other Outcome is decided exclusively Hub-side (validation, auth,
// rate limiting, cross-project policy) before a request ever reaches a
// broker, so a broker asserting one of those would be a transport bug, not
// a legitimate decision — ValidBrokerOutcome and ClassifyDispatchError both
// treat anything outside this list as OutcomeKeysOutcomeUnknown rather than
// trusting it.
var brokerAssertableOutcomes = map[Outcome]bool{
	OutcomeNotFound:         true, // target does not exist, or AgentID does not match the resolved container's "agent_id" label
	OutcomeAgentNotRunning:  true,
	OutcomeTerminalNotReady: true,
	OutcomeKeysUnsupported:  true, // managed backend or unsupported runtime, decided once the dedicated handler is reached
	OutcomeKeysUnavailable:  true, // the broker's own admission check found ExecuteBefore already past
}

// ValidBrokerOutcome reports whether o is one the broker may assert about
// itself. BrokerOutcomeError's constructor does not enforce this — callers
// (task 1.2's adapters) must check it before wrapping a broker-reported
// outcome, and ClassifyDispatchError re-checks it defensively regardless.
func ValidBrokerOutcome(o Outcome) bool {
	return brokerAssertableOutcomes[o]
}

// BrokerOutcomeError is how a BrokerClient/Dispatcher implementation reports
// a well-formed, allow-listed outcome the broker decided about itself — the
// Hub-side equivalent of the three manager-level sentinel errors below
// (ErrTargetNotFound, ErrAgentNotRunning, ErrTerminalNotReady), for the
// decisions only the broker (not the Hub) is positioned to make.
//
// Task 1.2's adapters construct one when, and only when, one of:
//
//   - the response's HTTP status equals HTTPStatus(outcome), the body
//     decodes as a BrokerResult, and ValidBrokerOutcome(body.Outcome) is
//     true — the ordinary case, covering a dedicated keys handler's own
//     OutcomeNotFound, OutcomeAgentNotRunning, OutcomeTerminalNotReady, or an
//     expired-deadline OutcomeKeysUnavailable found at broker admission.
//   - **the one specified exception**: the response's HTTP status is 404 and
//     the body does *not* decode as a BrokerResult with a valid allow-listed
//     outcome. This is the "broker lacks the keys route entirely" case: an
//     old broker's generic unrecognized-action handler
//     (pkg/runtimebroker/handlers.go's handleAgentAction, via
//     api.RuntimeBrokerAgentActionMethod returning ok=false) answers 404
//     with runtimebroker's ordinary error envelope
//     ({"error":{"code":"not_found","message":"Action not found"}}, from
//     NotFound(w, "Action")) — a shape with no top-level "outcome" field at
//     all, structurally distinct from BrokerResult's {"outcome":...,
//     "operation_id":...}. Adapters construct
//     BrokerOutcomeError{Outcome: OutcomeKeysUnsupported} for this case.
//     Without this exception, an old, not-yet-upgraded broker would report
//     OutcomeKeysOutcomeUnknown for every keys attempt, when #2184 requires
//     "old brokers return keys_unsupported" and AK-28 requires the same.
//
// Any other response shape after the request was sent — an unparseable body
// on a non-404 status, a status/body disagreement, or a 2xx whose Outcome is
// not OutcomeDispatched — must not become a BrokerOutcomeError; it
// classifies as OutcomeKeysOutcomeUnknown instead (see ClassifyDispatchError)
// because, unlike a plain 404, those shapes do not rule out that a real
// handler started executing before producing a malformed response.
type BrokerOutcomeError struct {
	Outcome Outcome
	Message string
}

func (e *BrokerOutcomeError) Error() string {
	if e.Message != "" {
		return string(e.Outcome) + ": " + e.Message
	}
	return string(e.Outcome)
}

// Frozen manager signature (task 1.1, pkg/agent.Manager/AgentManager):
//
//	SendKeys(ctx context.Context, projectID, agentSlug, expectedAgentID, keys string) error
//
// renaming and replacing today's MessageRaw(ctx, agentID, projectID,
// keys string) error (pkg/agent/manager.go:72,309) once 1.1 ships it — 0.1
// does not touch pkg/agent/manager.go itself (see the contract doc's
// "Deliberately not touched by this task" list).
//
// SendKeys must resolve the target container by (projectID, agentSlug),
// check its "agent_id" label against expectedAgentID, and Exec on that same
// resolved container — all within this one call, with no second resolution
// step in between the check and the Exec. This is the identity-binding
// requirement from §4.1 stated at the one call site that can actually
// enforce it atomically: a caller that checks the label via one lookup and
// then invokes a *different* method that re-resolves by slug alone (the way
// today's MessageRaw does — filter by name+project, List, Exec on the
// result) reopens exactly the recreate-inside-the-window gap identity
// binding exists to close, because the container found on the second lookup
// is not guaranteed to be the one the first lookup checked.
//
// SendKeys returns one of three sentinels, and only when it can prove the
// corresponding condition before any Exec attempt — the same
// "provably didn't happen" standard ClassifyDispatchError applies one layer
// up: ErrTargetNotFound (resolution found no matching container, or the
// resolved container's "agent_id" label is missing or does not equal
// expectedAgentID — fail closed, never a slug-only match), ErrAgentNotRunning
// (a matching, identity-confirmed container exists but is not running), or
// ErrTerminalNotReady (running, identity-confirmed, but its terminal/tmux
// session cannot accept input). Any other failure — including one where the
// tmux send-keys call itself may have partially run — must be a plain
// (unwrapped) error, which the broker's dedicated keys handler must not
// attempt to reclassify as one of the three sentinels above: it becomes an
// ambiguous response that ultimately reaches the Hub side as
// OutcomeKeysOutcomeUnknown.
//
// These three sentinels are manager-level and broker-process-internal: the
// runtime broker's dedicated keys handler (also task 1.1) is responsible for
// translating whichever one SendKeys returns into the matching
// BrokerResult/BrokerOutcomeError (ErrTargetNotFound → OutcomeNotFound,
// ErrAgentNotRunning → OutcomeAgentNotRunning, ErrTerminalNotReady →
// OutcomeTerminalNotReady) before the HTTP response ever leaves the broker
// process. Task 1.2's Hub-side BrokerClient/Dispatcher implementations never
// see or wrap these three directly — they only ever construct a
// *BrokerOutcomeError after decoding that already-translated HTTP response
// (see BrokerOutcomeError's doc). ErrNotDispatched, below, is the one
// sentinel task 1.2 does use directly, for failures the Hub side proves on
// its own without any broker response to translate.
var (
	// ErrTargetNotFound means SendKeys could not identify one specific,
	// already-authorized container to act on: either resolution by
	// (projectID, agentSlug) found no matching container, or it found one
	// whose "agent_id" label is missing, empty, or does not equal
	// expectedAgentID. Both cases fail closed to "not found" rather than
	// falling back to a slug-only match — from the caller's perspective,
	// proceeding on an unconfirmed identity is indistinguishable from
	// targeting the wrong agent. Translated by the broker's keys handler to
	// OutcomeNotFound (404).
	ErrTargetNotFound = errors.New("agentkeys: target not found")

	// ErrAgentNotRunning means SendKeys resolved and identity-confirmed the
	// target but it was not in a running phase, proven before any Exec
	// attempt. Translated by the broker's keys handler to
	// OutcomeAgentNotRunning (409).
	ErrAgentNotRunning = errors.New("agentkeys: agent not running")

	// ErrTerminalNotReady means SendKeys resolved and identity-confirmed a
	// running target, but its terminal/tmux session could not accept input,
	// proven before any Exec attempt. Translated by the broker's keys
	// handler to OutcomeTerminalNotReady (409).
	ErrTerminalNotReady = errors.New("agentkeys: terminal not ready")

	// ErrNotDispatched means the call is proven to have failed before it
	// reached, or before any broker handler began runtime execution on, the
	// target: network refused or no synchronous route to the broker at all
	// (including a control-channel target with no immediate route —
	// HybridBrokerClient's existing routeForward/undeliverable case, see the
	// doc comment below), or the Hub's own pre-send check found
	// ExecuteBefore already past. Unlike the three sentinels above, this one
	// is Hub-side only: task 1.2's Dispatcher/BrokerClient implementations
	// return it directly, without any broker response to translate. Maps to
	// OutcomeKeysUnavailable (503).
	//
	// This sentinel is exclusively for failures the Hub side can prove on
	// its own, without a broker response to interpret — it does not cover
	// "the broker responded but doesn't support keys" (that is
	// OutcomeKeysUnsupported, asserted by the broker itself and carried by
	// BrokerOutcomeError, or inferred Hub-side per the detection rule on
	// BrokerOutcomeError's doc) or "the broker's own admission check found
	// the deadline expired" (also OutcomeKeysUnavailable, but via
	// BrokerOutcomeError, since that determination happens broker-side,
	// after semaphore/target-lock waits ErrNotDispatched cannot see).
	//
	// Callers must never return this once a request has actually been sent
	// to the runtime: "sent, response lost" must classify as
	// OutcomeKeysOutcomeUnknown (ClassifyDispatchError's default case — wrap
	// no sentinel, or a fresh unwrapped error), not ErrNotDispatched.
	// Concrete requirement for task 1.2:
	// HybridBrokerClient.MessageAgent's existing pattern
	// (pkg/hub/controlchannel_client.go:745-753) returns ErrMessageDeferred
	// for routeForward/undeliverable, meaning "queue it, a durable send will
	// happen later" — keys has no durable queue, so the keys equivalent of
	// that branch must return ErrNotDispatched, never a deferred/queued
	// error of any kind.
	ErrNotDispatched = errors.New("agentkeys: dispatch did not start")
)

// ClassifyDispatchError maps a non-nil error returned by
// BrokerClient.ExecuteKeys or Dispatcher.DispatchAgentKeys to the Outcome
// task 2.2 must report. This is the single place that decides "did it
// definitely not happen" (503) vs "unknown" (502/504) vs "the broker decided
// X" — see the rationale in .design/agent-keys-contract.md §4.1/4.3. Passing
// nil is a caller bug (success has no error to classify);
// ClassifyDispatchError returns OutcomeKeysOutcomeUnknown for it so a
// defensive caller fails safe rather than panicking, but callers should not
// rely on that: check err != nil first.
//
// ClassifyDispatchError recognizes exactly two things: a *BrokerOutcomeError
// (trusted only if its Outcome passes ValidBrokerOutcome — this function
// re-checks that itself rather than trusting whatever the adapter
// constructed, so a broker that somehow asserts an outcome outside the
// allowlist, or a transport bug in an adapter, cannot cause a
// Hub-decided-only outcome to be reported for a request the broker actually
// handled) and ErrNotDispatched, the one sentinel task 1.2's
// Dispatcher/BrokerClient implementations return directly. It does **not**
// match ErrTargetNotFound, ErrAgentNotRunning or ErrTerminalNotReady:
// those three are manager-level and broker-process-internal (see their doc
// comments above) — a correct task 1.1/1.2 implementation translates each of
// them into a *BrokerOutcomeError before any response leaves the broker
// process, so ClassifyDispatchError never needs to recognize them by name.
// If one of the three ever did reach this function unwrapped — a bug
// forwarding a manager error verbatim instead of translating it — this
// function still fails safe: it falls through to OutcomeKeysOutcomeUnknown,
// the same as any other unrecognized error, rather than guessing that the
// forwarding was reliable.
//
// Every adapter (HTTP, control-channel, hybrid, and the authenticated
// wrapper — see the contract's §4 adapter list) must be single-attempt: no
// SDK WithRetry, no HTTP redirect following, no tunnel-reconnect resend, and
// no routing fallback after an uncertain send. An adapter that retries
// internally has already violated the no-replay requirement before this
// function is ever called.
func ClassifyDispatchError(err error) Outcome {
	if err == nil {
		return OutcomeKeysOutcomeUnknown
	}

	var boe *BrokerOutcomeError
	if errors.As(err, &boe) {
		if ValidBrokerOutcome(boe.Outcome) {
			return boe.Outcome
		}
		return OutcomeKeysOutcomeUnknown
	}

	if errors.Is(err, ErrNotDispatched) {
		return OutcomeKeysUnavailable
	}
	return OutcomeKeysOutcomeUnknown
}

// ErrMissingDeadline is returned by CapExecuteBefore when requestDeadline is
// zero. The contract requires failing closed on a missing or invalid
// internal deadline rather than inventing one.
var ErrMissingDeadline = errors.New("agentkeys: request deadline is required and must be non-zero")

// ErrInvalidWindow is returned by CapExecuteBefore when window is not
// positive. A zero or negative window is a caller misconfiguration, not a
// valid "cap to right now" instruction, and must fail loudly rather than
// silently producing an already-expired deadline.
var ErrInvalidWindow = errors.New("agentkeys: admission window must be positive")

// CapExecuteBefore computes the Hub-issued execute-before timestamp for a
// keys dispatch: min(requestDeadline, now+window). The broker cannot extend
// this deadline; window should normally be DefaultAdmissionWindow.
//
// It fails closed: a zero requestDeadline (missing/invalid internal
// deadline) returns ErrMissingDeadline rather than silently falling back to
// now+window, per the contract's "fail-closed on missing/invalid deadline"
// rule. A non-positive window returns ErrInvalidWindow rather than silently
// producing a deadline at or before now. A requestDeadline that has already
// passed is not treated specially here — it caps to itself, and the
// caller's own deadline check (e.g. ctx.Err()) is expected to have already
// rejected the request.
func CapExecuteBefore(now, requestDeadline time.Time, window time.Duration) (time.Time, error) {
	if requestDeadline.IsZero() {
		return time.Time{}, ErrMissingDeadline
	}
	if window <= 0 {
		return time.Time{}, ErrInvalidWindow
	}
	windowDeadline := now.Add(window)
	if requestDeadline.Before(windowDeadline) {
		return requestDeadline, nil
	}
	return windowDeadline, nil
}
