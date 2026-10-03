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

// ExecuteAgentKeys (task 2.2, ptone/scion#2196) is the sole authoritative
// implementation of the agent-keys operation, consumed by both public route
// shapes (handlers_agents_core.go's handleAgentAction and
// handlers_projects_core.go's handleProjectAgentAction). It replaces task
// 2.1's temporary routing seam wholesale, per the design-owner ruling
// recorded on ptone/scion#2195 and restated as this task's binding
// integration obligation on ptone/scion#2196:
//
//	ValidateBody -> mint one real operation ID -> preserve 2.1's
//	ordering/resolution behavior -> authorizeAgentKeys -> remaining
//	admission/dispatch
//
// so every outcome from validation onward -- not_found, keys_denied, both
// 422s, both 409s, 429, 503, 502/504, the internal 500, and 200 -- carries
// that operation ID (contract §2.5, §3 invariant 3).
//
// Two outcomes are written by this file without an operation ID:
// OutcomeInvalidRequest and OutcomePayloadTooLarge, both decided by
// beginAgentKeysRequest during validation itself, before an operation is
// recognized to exist. Two more no-ID cases are decided before this file
// ever runs, by shared code upstream of it: OutcomeUnauthorized (the shared
// auth middleware answers an unauthenticated request before any handler
// runs) and the project-scoped route's own shared project-resolution 404
// (handleProjectAgents, decided before dispatching to any action-specific
// code, including this one; this one is not an agentkeys.Outcome value at
// all).
//
// Full execution order (ptone/scion#2184's execution order, restated in
// .design/agent-keys-contract.md §2.5):
//
//	authenticate (middleware, outside this file)
//	  -> bounded strict decode (beginAgentKeysRequest)
//	  -> mint operation ID (beginAgentKeysRequest)
//	  -> resolve target/project, preserving 2.1's per-route ordering
//	     (handleAgentActionKeysTopLevel / handleAgentActionKeysProjectScoped)
//	  -> authorizeAgentKeys (task 2.1, unchanged)
//	  -> charge keys budget (admitAndDispatchAgentKeys)
//	  -> check running phase, runtime support and immediate broker route
//	     (admitAndDispatchAgentKeys)
//	  -> write admission audit
//	  -> one typed dispatch (agentkeys.Dispatcher, task 1.2)
//	  -> outcome audit and response
//
// No conversation resolver, message store, observer, mention parser,
// notification subscription, message-status callback or managed
// CreateInteraction call is reachable from this file (contract §4.4): a
// keys request never enters the messaging call graph at any outcome.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/agentkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
	"github.com/google/uuid"
)

// handleAgentActionKeysTopLevel implements the top-level route's
// ExecuteAgentKeys flow: resolve {id} first (this route's own lookup, the
// same way every other top-level action resolves its target), then compare
// projects inside authorizeAgentKeys itself (contract §3.1 "Option 1,
// chosen"). Called from handleAgentAction's api.AgentActionKeys branch.
func (s *Server) handleAgentActionKeysTopLevel(w http.ResponseWriter, r *http.Request, id string) {
	keys, operationID, ok := s.beginAgentKeysRequest(w, r)
	if !ok {
		return
	}

	target, err := s.store.GetAgent(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.finishAgentKeysNotFound(w, r, operationID, agentKeysAuditTarget{}, len(keys))
			return
		}
		s.finishAgentKeysInternalError(w, r, operationID, agentKeysAuditTarget{}, len(keys), err, agentKeysRouteKeys)
		return
	}

	decision := s.authorizeAgentKeys(r, target)
	if !decision.Allowed {
		s.finishAgentKeysDenied(w, r, operationID, target, len(keys), decision, agentKeysRouteKeys)
		return
	}

	s.admitAndDispatchAgentKeys(w, r, operationID, target, keys, agentKeysRouteKeys)
}

// handleAgentActionKeysProjectScoped implements the project-scoped route's
// ExecuteAgentKeys flow: the agent-credential cross-project refusal is
// decided before any agent-target lookup (contract §3.1 invariant 4,
// AK-21c), using only the caller's identity and the already-resolved
// {project} ID. Only once that passes is the target resolved, via the same
// canonical resolveProjectAgent the route's other actions use, so a store
// failure surfaces as a generic 5xx rather than being collapsed into a
// misleading 404. A resolution miss is reported as keys' own not_found
// (contract §3 invariant 3), not the shared resolver's
// agent_not_found/{agent_slug,project_id} shape.
//
// Called from handleProjectAgentAction's api.AgentActionKeys branch, after
// the shared handleProjectAgents project-resolution gate has already run
// (that gate's own 404 precedes this function entirely and carries no
// operation ID -- contract AK-21e/AK-21f).
func (s *Server) handleAgentActionKeysProjectScoped(w http.ResponseWriter, r *http.Request, projectID, agentIDOrSlug string) {
	keys, operationID, ok := s.beginAgentKeysRequest(w, r)
	if !ok {
		return
	}

	if denial := s.authorizeAgentKeysCrossProject(r, projectID); denial != nil {
		s.finishAgentKeysDeniedDecision(w, r, operationID, agentKeysAuditTarget{ProjectID: projectID}, len(keys), *denial, agentKeysRouteKeys)
		return
	}

	target, err := s.resolveProjectAgent(r.Context(), projectID, agentIDOrSlug)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.finishAgentKeysNotFound(w, r, operationID, agentKeysAuditTarget{ProjectID: projectID}, len(keys))
			return
		}
		s.finishAgentKeysInternalError(w, r, operationID, agentKeysAuditTarget{ProjectID: projectID}, len(keys), err, agentKeysRouteKeys)
		return
	}

	decision := s.authorizeAgentKeys(r, target)
	if !decision.Allowed {
		s.finishAgentKeysDenied(w, r, operationID, target, len(keys), decision, agentKeysRouteKeys)
		return
	}

	s.admitAndDispatchAgentKeys(w, r, operationID, target, keys, agentKeysRouteKeys)
}

// beginAgentKeysRequest implements contract §3 invariant 2 (ValidateBody
// precedes both agent-target resolution and authorization) and invariant 3's
// operation-ID half (minted as soon as validation succeeds, before
// authorization and before any agent-target resolution outcome is
// reported). agentkeys.ValidateBody is the only decoder any /keys handler
// may use for the request body (contract §5 "Decoding gaps"): the raw read
// is bounded by agentkeys.MaxHTTPBodyBytes via http.MaxBytesReader first, so
// a body larger than that ceiling is rejected (413) directly from the
// transport-level read without ever reaching ValidateBody.
//
// A validation failure is written here directly and carries no operation ID
// (contract §2.5's OperationID policy: OutcomeInvalidRequest and
// OutcomePayloadTooLarge are the only two outcomes, besides
// OutcomeUnauthorized, that never get one). It is still audited
// content-free, since actor identity is already established by the time
// this function runs (authentication precedes everything).
func (s *Server) beginAgentKeysRequest(w http.ResponseWriter, r *http.Request) (keys, operationID string, ok bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, agentkeys.MaxHTTPBodyBytes))
	if err != nil {
		// Only a body that actually exceeded MaxHTTPBodyBytes is
		// payload_too_large. http.MaxBytesReader wraps exactly that case in
		// a *http.MaxBytesError; any other read failure (client disconnect,
		// truncated body, a slow/aborted request) is a malformed request,
		// not an oversized one, and must not be misreported as the latter.
		outcome := agentkeys.OutcomeInvalidRequest
		message := "invalid request body"
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			outcome = agentkeys.OutcomePayloadTooLarge
			message = "request body exceeds the size limit"
		}
		s.logAgentKeysValidationAudit(r, outcome, 0, agentKeysRouteKeys)
		writeAgentKeysValidationError(w, outcome, message)
		return "", "", false
	}

	keys, verr := agentkeys.ValidateBody(body)
	if verr != nil {
		outcome := agentkeys.OutcomeInvalidRequest
		if ve, isVE := agentkeys.AsValidationError(verr); isVE {
			outcome = ve.Outcome
		}
		s.logAgentKeysValidationAudit(r, outcome, len(body), agentKeysRouteKeys)
		writeAgentKeysValidationError(w, outcome, verr.Error())
		return "", "", false
	}

	return keys, uuid.NewString(), true
}

// agentKeysAuditTarget carries whatever target identifiers are known at the
// time of an audit event. Both fields may be empty -- e.g. the
// project-scoped route's pre-resolution cross-project denial (AK-21c) knows
// only the URL {project}, never a resolved agent ID.
type agentKeysAuditTarget struct {
	AgentID   string
	ProjectID string
}

// agentKeysAuditEvent names which of the contract §5 "admission and outcome
// events" a logAgentKeysAudit call records: admission is the single record
// written immediately before dispatch is attempted (decision is always
// empty then -- there is no decision yet), and outcome is every terminal
// record, whether decided before dispatch was ever attempted (validation,
// denial, not_found, rate limit, unsupported runtime, not running) or after
// (the dispatch result itself). Kept as a separate field rather than
// overloading an empty decision string, so the two kinds of record can be
// told apart without relying on that coincidence.
type agentKeysAuditEvent string

const (
	agentKeysAuditEventAdmission agentKeysAuditEvent = "admission"
	agentKeysAuditEventOutcome   agentKeysAuditEvent = "outcome"
)

// agentKeysRoute distinguishes which public surface produced an agent-keys
// audit record. Contract §5 ("Audit and limits") requires this on every
// admission and outcome event ("route (keys or transitional raw)"). The
// transitional raw bridge has been removed, so the only routes left are the
// direct /keys routes and the raw_input_removed rejection of a message
// request that still carries the retired raw field.
type agentKeysRoute string

const (
	// agentKeysRouteKeys tags every audit record produced by the direct
	// POST .../keys routes (handleAgentActionKeysTopLevel/ProjectScoped).
	agentKeysRouteKeys agentKeysRoute = "keys"

	// agentKeysRouteRawRemoved tags the audit record for a message request
	// rejected with raw_input_removed because it still carries the retired
	// raw field (raw_tombstone.go). Nothing is ever delivered on this route.
	agentKeysRouteRawRemoved agentKeysRoute = "message_raw_removed"
)

// finishAgentKeysNotFound writes and audits keys' own not_found outcome
// (contract §3 invariant 3): a keys handler must not inherit another
// resolver's different 404 code/shape just because it is convenient to call
// into.
func (s *Server) finishAgentKeysNotFound(w http.ResponseWriter, r *http.Request, operationID string, audit agentKeysAuditTarget, inputBytes int) {
	// Only ever reached by the direct /keys handlers.
	s.logAgentKeysAudit(r, agentKeysAuditEventOutcome, operationID, audit, agentkeys.OutcomeNotFound, inputBytes, 0, agentKeysRouteKeys)
	writeAgentKeysOutcome(w, agentkeys.OutcomeNotFound, operationID, agentKeysOutcomeMessage(agentkeys.OutcomeNotFound), 0)
}

// finishAgentKeysInternalError writes and audits a generic 5xx for a
// target-resolution failure that is not store.ErrNotFound -- a store outage
// or similar, surfaced the same way writeErrorFromErr's default branch
// would surface it for any other route, but with details.operation_id set
// and a content-free audit record, since this is reached after
// beginAgentKeysRequest has already minted one (contract §3 invariant 3
// extends the operation-ID floor to every outcome after validation).
// GetAgent/GetAgentBySlug/resolveProjectAgent are read-only lookups: the
// only error class they can realistically produce, other than
// store.ErrNotFound (handled separately by the caller, before this function
// is ever reached), is a generic backend/transport failure, so this always
// reports the one generic 500 writeErrorFromErr's default branch would,
// rather than reproducing that switch's other branches for error classes a
// read-only lookup cannot produce.
func (s *Server) finishAgentKeysInternalError(w http.ResponseWriter, r *http.Request, operationID string, audit agentKeysAuditTarget, inputBytes int, err error, route agentKeysRoute) {
	const statusCode = http.StatusInternalServerError
	const code = ErrCodeInternalError
	const message = "Internal server error"

	slog.Error("agent keys: target resolution failed",
		"operation_id", operationID, "status", statusCode, "code", code, "error", err)
	s.logAgentKeysInternalErrorAudit(r, operationID, audit, code, inputBytes, route)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(ErrorResponse{
		Error: APIError{
			Code:    code,
			Message: message,
			Details: map[string]interface{}{"operation_id": operationID},
		},
	})
}

// finishAgentKeysDenied writes and audits a denial produced by
// authorizeAgentKeys against an already-resolved target -- keys_denied or
// cross_project_keys_unsupported -- now carrying the real operation ID
// minted by beginAgentKeysRequest. This replaces task 2.1's temporary
// writeAgentKeysAuthzDenial, which never carried one because validation did
// not exist yet at that point in the seam (contract §3's phase-boundary
// clarification).
func (s *Server) finishAgentKeysDenied(w http.ResponseWriter, r *http.Request, operationID string, target *store.Agent, inputBytes int, decision KeysAuthzDecision, route agentKeysRoute) {
	audit := agentKeysAuditTarget{}
	if target != nil {
		audit.AgentID = target.ID
		audit.ProjectID = target.ProjectID
	}
	s.finishAgentKeysDeniedDecision(w, r, operationID, audit, inputBytes, decision, route)
}

// finishAgentKeysDeniedDecision is finishAgentKeysDenied's variant for a
// denial reached before any target agent was resolved (the project-scoped
// route's pre-lookup cross-project refusal, AK-21c), where only the URL
// project ID is known. route tags the audit record (contract §5).
func (s *Server) finishAgentKeysDeniedDecision(w http.ResponseWriter, r *http.Request, operationID string, audit agentKeysAuditTarget, inputBytes int, decision KeysAuthzDecision, route agentKeysRoute) {
	s.logAgentKeysAudit(r, agentKeysAuditEventOutcome, operationID, audit, decision.Outcome, inputBytes, 0, route)
	writeAgentKeysOutcome(w, decision.Outcome, operationID, agentKeysOutcomeMessage(decision.Outcome), 0)
}

// admitAndDispatchAgentKeys performs every step of ptone/scion#2184's
// execution order that follows a successful authorizeAgentKeys decision:
// independent rate-limit admission, running-phase/runtime-support checks,
// the execute-before deadline, one typed dispatch, and the terminal
// audit+response. target is already resolved and authorized; this function
// does not re-resolve or re-authorize it (contract §4.4 -- the same facts
// already gathered are passed straight through to the dispatcher).
func (s *Server) admitAndDispatchAgentKeys(w http.ResponseWriter, r *http.Request, operationID string, target *store.Agent, keys string, route agentKeysRoute) {
	started := time.Now()
	audit := agentKeysAuditTarget{AgentID: target.ID, ProjectID: target.ProjectID}
	inputBytes := len(keys)

	deny := func(outcome agentkeys.Outcome, retryAfter time.Duration) {
		s.logAgentKeysAudit(r, agentKeysAuditEventOutcome, operationID, audit, outcome, inputBytes, time.Since(started), route)
		writeAgentKeysOutcome(w, outcome, operationID, agentKeysOutcomeMessage(outcome), retryAfter)
	}

	// Separate, independent token buckets (contract "Concrete defaults"):
	// per authenticated principal+project, and per target agent. Both must
	// allow the request; neither bucket can be topped up from the other,
	// and both are entirely separate from chatSendLimiter's aggregate DM
	// allowance -- keys cannot charge or evade it (issue #2196 AC3). Shared
	// by both public route shapes: the bucket keys depend only on the
	// caller's identity and the resolved target's project/agent ID, never
	// on which route resolved them, so a caller cannot reset either budget
	// by switching route shape.
	// allowBoth (not two separate Allow calls) checks both and consumes
	// from neither unless both allow, so a saturated target bucket cannot
	// drain a caller's principal+project budget for a request that was
	// going to be refused anyway.
	if allowed, retryAfter := allowBoth(
		s.keysPrincipalLimiter, agentKeysPrincipalBucketKey(r.Context(), target.ProjectID),
		s.keysTargetLimiter, target.ID,
	); !allowed {
		deny(agentkeys.OutcomeKeysRateLimited, retryAfter)
		return
	}

	// Runtime support: a managed backend never has a runtime-broker keys
	// route to dispatch to. Never downgraded to a message-based fallback
	// (AK-28).
	if isManagedAgentRuntime(target.Runtime) {
		deny(agentkeys.OutcomeKeysUnsupported, 0)
		return
	}

	// Running phase: no wake/start on a stopped or not-yet-running target
	// (AK-26). This is a Hub-side, store-level check using the agent's last
	// known phase; the broker's own SendKeys primitive (task 1.1) performs
	// an independent, container-level check immediately before execution,
	// closing the race this cached check cannot see.
	if target.Phase != string(state.PhaseRunning) {
		deny(agentkeys.OutcomeAgentNotRunning, 0)
		return
	}

	dispatcher, ok := s.GetDispatcher().(agentkeys.Dispatcher)
	if !ok {
		// No dispatcher configured, or the configured one does not
		// implement agentkeys.Dispatcher: proven Hub-side, before any
		// request could reach a broker -- the same standard
		// agentkeys.ErrNotDispatched documents. No immediate route exists
		// (AK-29); never a wake, queue, or fallback to messaging.
		deny(agentkeys.OutcomeKeysUnavailable, 0)
		return
	}

	// Execute-before deadline (contract §4.2, issue #2196 AC1):
	// min(request deadline, now+30s), fail-closed on a missing/invalid
	// deadline. This Hub establishes its own request deadline via
	// context.WithTimeout when the incoming request context carries none
	// (the ordinary case for a directly-dialed HTTP client), so the
	// resulting execute-before is, in that common case, exactly
	// now+DefaultAdmissionWindow -- CapExecuteBefore's fail-closed
	// zero-deadline guard exists for a request whose context deadline was
	// never established at all, not to special-case "the caller didn't ask
	// for one". Clock-synchronization assumption (contract §4.2): the Hub
	// and the runtime broker are assumed to have reasonably synchronized
	// clocks: DefaultAdmissionWindow (30s) has enough margin to absorb
	// ordinary clock skew between processes, but this is an assumption, not
	// a guarantee this code verifies. A dispatch attempt that begins before
	// the deadline can still be running when it passes: contract §4.2
	// requires the broker to enforce expiration at admission, after any
	// control-channel semaphore/target-lock wait, and immediately before
	// runtime execution, but once execution itself has started, a timeout
	// is reported as agentkeys.OutcomeKeysOutcomeUnknown (never a false
	// "definitely no effect" and never a false "delivered") --
	// ClassifyDispatchError below is what makes that determination.
	ctx := r.Context()
	requestDeadline, hasDeadline := ctx.Deadline()
	if !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, agentkeys.DefaultAdmissionWindow)
		defer cancel()
		requestDeadline, _ = ctx.Deadline()
	}
	executeBefore, err := agentkeys.CapExecuteBefore(time.Now(), requestDeadline, agentkeys.DefaultAdmissionWindow)
	if err != nil {
		// Fail closed on a missing/invalid deadline (contract §4.2) rather
		// than silently defaulting or dispatching with an unbounded
		// admission window.
		deny(agentkeys.OutcomeKeysUnavailable, 0)
		return
	}

	dispatchTarget := agentkeys.Target{
		RuntimeBrokerID: target.RuntimeBrokerID,
		AgentID:         target.ID,
		AgentSlug:       target.Slug,
		ProjectID:       target.ProjectID,
	}

	// Write the admission audit record before dispatch (ptone/scion#2184's
	// execution order: "write admission audit -> one dispatch -> outcome
	// audit"), then dispatch exactly once -- no SDK retry, no redirect
	// following, no reconnect resend, no routing fallback (contract §4.3;
	// task 1.2's adapters are already single-attempt, this call site adds
	// no retry of its own).
	s.logAgentKeysAudit(r, agentKeysAuditEventAdmission, operationID, audit, "", inputBytes, 0, route)
	result, dispatchErr := dispatcher.DispatchAgentKeys(ctx, dispatchTarget, operationID, executeBefore, keys)
	duration := time.Since(started)

	if dispatchErr != nil {
		outcome := agentkeys.ClassifyDispatchError(dispatchErr)
		s.logAgentKeysAudit(r, agentKeysAuditEventOutcome, operationID, audit, outcome, inputBytes, duration, route)
		writeAgentKeysOutcome(w, outcome, operationID, agentKeysOutcomeMessage(outcome), 0)
		return
	}

	// The response must echo the operation ID this handler minted, never
	// whatever the dispatcher happened to return. A correct
	// agentkeys.Dispatcher always echoes the same ID
	// back (1.2's decodeBrokerKeysResponse enforces this for the real HTTP
	// transport), but "exactly one real operation ID" is a property of
	// this handler, not something it may delegate to trusting every
	// current and future Dispatcher implementation to get right. A
	// mismatch is treated as an ambiguous outcome -- not a false success,
	// and not a guess at which ID is correct.
	//
	// Likewise, a nil dispatchErr is only ever a genuine success if
	// result.Outcome is also OutcomeDispatched: contract/pkg/agentkeys's
	// BrokerResult doc says "Success is HTTP 200 with
	// Outcome == OutcomeDispatched; there is no other success shape", and
	// every current real transport (1.2's decodeBrokerKeysResponse)
	// enforces that before ever returning a nil error. As with the
	// operation-ID check above, this handler does not rely on every
	// current and future Dispatcher implementation getting that right --
	// any other Outcome value paired with a nil error is itself an
	// ambiguous outcome, not a false "delivered".
	if result.OperationID != "" && result.OperationID != operationID {
		s.logAgentKeysAudit(r, agentKeysAuditEventOutcome, operationID, audit, agentkeys.OutcomeKeysOutcomeUnknown, inputBytes, duration, route)
		writeAgentKeysOutcome(w, agentkeys.OutcomeKeysOutcomeUnknown, operationID, agentKeysOutcomeMessage(agentkeys.OutcomeKeysOutcomeUnknown), 0)
		return
	}
	if result.Outcome != agentkeys.OutcomeDispatched {
		s.logAgentKeysAudit(r, agentKeysAuditEventOutcome, operationID, audit, agentkeys.OutcomeKeysOutcomeUnknown, inputBytes, duration, route)
		writeAgentKeysOutcome(w, agentkeys.OutcomeKeysOutcomeUnknown, operationID, agentKeysOutcomeMessage(agentkeys.OutcomeKeysOutcomeUnknown), 0)
		return
	}

	s.logAgentKeysAudit(r, agentKeysAuditEventOutcome, operationID, audit, agentkeys.OutcomeDispatched, inputBytes, duration, route)
	writeAgentKeysSuccess(w, operationID, target.ID)
}

// agentKeysPrincipalBucketKey derives the per-principal-per-project rate
// limit bucket key from the authenticated identity and the project the
// request is acting against.
//
// Documented per-Hub limiter semantics (issue #2196's "per-Hub limiter
// semantics" scope item): both keysPrincipalLimiter and keysTargetLimiter
// are in-memory, per-Hub-instance token buckets, not a distributed quota
// service (contract §5) -- N Hub replicas behind a load balancer allow N
// times the configured rate in aggregate, and a Hub restart resets both
// buckets to full. "project" here is always the *target's* resolved
// project, not a caller-asserted one. For an agent credential this is
// always equal to the caller's own project (a cross-project agent call is
// already refused before this function is ever reached); for a human
// caller -- who has no single fixed "home" project in this multi-project
// system -- it is the project the human is currently acting in, so the
// same operator issuing keys into two different projects is charged two
// independent budgets, and switching projects cannot be used to evade a
// budget already spent in one of them. Bucket keys are namespaced by
// identity type so a user and an agent that happen to share a raw ID cannot
// share or drain each other's bucket.
func agentKeysPrincipalBucketKey(ctx context.Context, targetProjectID string) string {
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		return "unknown:unknown:" + targetProjectID
	}
	return identity.Type() + ":" + identity.ID() + ":" + targetProjectID
}

// agentKeysAuditActor extracts the actor/credential fields every "agent keys
// audit" record shares, regardless of which of the three logging entry
// points (logAgentKeysAudit, logAgentKeysValidationAudit,
// logAgentKeysInternalErrorAudit) writes it.
func agentKeysAuditActor(ctx context.Context) (actorType, actorID, sourceProjectID string, credential CredentialContext) {
	identity := GetIdentityFromContext(ctx)
	if identity != nil {
		actorType = identity.Type()
		actorID = identity.ID()
		if agentIdent, ok := identity.(AgentIdentity); ok {
			sourceProjectID = agentIdent.ProjectID()
		}
	}
	credential = GetCredentialContextFromContext(ctx)
	return actorType, actorID, sourceProjectID, credential
}

// logAgentKeysAudit writes the content-free admission/outcome audit record
// the contract's §5 field list requires: timestamp (implicit via slog),
// event (admission or outcome), operation ID, authenticated actor kind/ID,
// source project (when applicable), target agent/project, credential
// ID/kind (when available), route, "input_bytes" (the decoded "keys"
// value's length), decision code (always an agentkeys.Outcome value on this
// path), and duration. decision is the empty string for the one
// admission-event call in admitAndDispatchAgentKeys, which precedes the
// dispatch outcome itself and so has none yet; every other call passes
// agentKeysAuditEventOutcome with a real decision, so the two kinds of
// record no longer rely on an empty decision string to tell them apart.
//
// Contract §5 lists both a "decision code" and a "dispatch outcome" field.
// This function deliberately merges them into the one "decision" field: for
// a post-dispatch outcome record (agentKeysAuditEventOutcome after
// admitAndDispatchAgentKeys calls dispatcher.DispatchAgentKeys), decision
// *is* the dispatch outcome (agentkeys.ClassifyDispatchError's result, or
// OutcomeDispatched), and every other outcome record has no separate
// "dispatch outcome" to report in the first place (dispatch was never
// attempted). A reader checking field-for-field against §5 should treat
// "decision" as covering both named fields, not as omitting one of them.
//
// This never logs the "keys" value, a preview or hash of it, terminal
// contents, bearer values, request JSON, or runtime command arguments --
// inputBytes (a length, not content) is the only thing this function knows
// about the request body at all.
//
// Two sibling entry points cover the audit records this function's own
// preconditions cannot produce: logAgentKeysValidationAudit (no operation ID
// exists yet, and the byte count is the raw body length, not the decoded
// "keys" length -- reported as "body_bytes" instead of "input_bytes" for
// that reason) and logAgentKeysInternalErrorAudit (the decision is not an
// agentkeys.Outcome value at all, so it is reported under a distinct
// "error_class" field instead of "decision").
func (s *Server) logAgentKeysAudit(r *http.Request, event agentKeysAuditEvent, operationID string, target agentKeysAuditTarget, outcome agentkeys.Outcome, inputBytes int, duration time.Duration, route agentKeysRoute) {
	ctx := r.Context()
	actorType, actorID, sourceProjectID, credential := agentKeysAuditActor(ctx)

	slog.Info("agent keys audit",
		"event", string(event),
		"operation_id", operationID,
		"actor_type", actorType,
		"actor_id", actorID,
		"source_project_id", sourceProjectID,
		"target_agent_id", target.AgentID,
		"target_project_id", target.ProjectID,
		"credential_kind", string(credential.Kind),
		"credential_id", credential.ID,
		"route", string(route),
		"input_bytes", inputBytes,
		"decision", string(outcome),
		"duration_ms", duration.Milliseconds(),
		"request_id", logging.RequestIDFromContext(ctx),
	)
}

// logAgentKeysValidationAudit writes the audit record for a pre-operation-ID
// validation failure (beginAgentKeysRequest): no operation ID or target
// exists yet, so this is always an "outcome" event with an empty target.
// bodyBytes is the length of the raw, not-yet-decoded request body --
// reported under "body_bytes" rather than logAgentKeysAudit's "input_bytes",
// since it measures something different (the whole JSON payload, not the
// decoded "keys" field).
func (s *Server) logAgentKeysValidationAudit(r *http.Request, outcome agentkeys.Outcome, bodyBytes int, route agentKeysRoute) {
	ctx := r.Context()
	actorType, actorID, sourceProjectID, credential := agentKeysAuditActor(ctx)

	slog.Info("agent keys audit",
		"event", string(agentKeysAuditEventOutcome),
		"operation_id", "",
		"actor_type", actorType,
		"actor_id", actorID,
		"source_project_id", sourceProjectID,
		"target_agent_id", "",
		"target_project_id", "",
		"credential_kind", string(credential.Kind),
		"credential_id", credential.ID,
		"route", string(route),
		"body_bytes", bodyBytes,
		"decision", string(outcome),
		"duration_ms", int64(0),
		"request_id", logging.RequestIDFromContext(ctx),
	)
}

// logAgentKeysInternalErrorAudit writes the audit record for
// finishAgentKeysInternalError's generic 5xx: errorClass is a Hub error code
// (e.g. ErrCodeInternalError), not an agentkeys.Outcome value, so it is
// reported under a distinct "error_class" field rather than "decision" --
// contract §5 defines "decision" as an agentkeys.Outcome value, and this
// path's code is not one.
func (s *Server) logAgentKeysInternalErrorAudit(r *http.Request, operationID string, target agentKeysAuditTarget, errorClass string, inputBytes int, route agentKeysRoute) {
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
		"route", string(route),
		"input_bytes", inputBytes,
		"decision", "",
		"error_class", errorClass,
		"duration_ms", int64(0),
		"request_id", logging.RequestIDFromContext(ctx),
	)
}

// agentKeysOutcomeMessage returns the fixed, sanitized, non-normative
// message (contract §2.4a) for outcome. Never interpolated with
// client-supplied data. A message for an outcome not in this table (which
// should not happen for any outcome this file ever passes to it) falls back
// to a generic string rather than panicking.
func agentKeysOutcomeMessage(outcome agentkeys.Outcome) string {
	switch outcome {
	case agentkeys.OutcomeKeysDenied:
		return "Insufficient permissions"
	case agentkeys.OutcomeCrossProjectKeysUnsupported:
		return "Cross-project keys access is not supported for agent callers"
	case agentkeys.OutcomeNotFound:
		return "Agent not found"
	case agentkeys.OutcomeAgentNotRunning:
		return "Agent is not running"
	case agentkeys.OutcomeTerminalNotReady:
		return "Agent terminal is not ready"
	case agentkeys.OutcomeKeysUnsupported:
		return "Keys are not supported for this agent"
	case agentkeys.OutcomeKeysRateLimited:
		return "Keys rate limit exceeded"
	case agentkeys.OutcomeKeysUnavailable:
		return "Keys dispatch is currently unavailable"
	case agentkeys.OutcomeKeysOutcomeUnknown:
		return "Keys dispatch outcome is unknown; do not retry automatically"
	default:
		return "Keys request could not be completed"
	}
}

// writeAgentKeysValidationError writes a pre-operation-ID validation
// failure (400 invalid_request or 413 payload_too_large): no
// "operation_id" key at all in details, per contract §2.4a ("omit the key
// entirely otherwise -- never a null or empty-string value").
func writeAgentKeysValidationError(w http.ResponseWriter, outcome agentkeys.Outcome, message string) {
	status, ok := agentkeys.HTTPStatus(outcome)
	if !ok {
		status = http.StatusBadRequest
		outcome = agentkeys.OutcomeInvalidRequest
	}
	writeError(w, status, string(outcome), message, nil)
}

// writeAgentKeysOutcome writes every outcome from validation onward
// (contract §3 invariant 3): the response always carries operationID in
// details.operation_id. message must be one of agentKeysOutcomeMessage's
// fixed strings -- never decision.Reason or any other client- or
// request-derived text (contract §2.4a).
func writeAgentKeysOutcome(w http.ResponseWriter, outcome agentkeys.Outcome, operationID, message string, retryAfter time.Duration) {
	status, ok := agentkeys.HTTPStatus(outcome)
	if !ok {
		status = http.StatusInternalServerError
	}
	if retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(retryAfter.Seconds()))))
	}
	writeError(w, status, string(outcome), message, map[string]interface{}{"operation_id": operationID})
}

// writeAgentKeysSuccess writes the 200 dispatched success shape (contract
// §2.4): HTTP 200 means the broker acknowledged successful terminal
// injection, not that the harness consumed or obeyed it.
func writeAgentKeysSuccess(w http.ResponseWriter, operationID, agentID string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(agentkeys.Response{
		Status:      agentkeys.StatusDispatched,
		OperationID: operationID,
		AgentID:     agentID,
	})
}
