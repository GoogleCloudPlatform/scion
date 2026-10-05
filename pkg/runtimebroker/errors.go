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

package runtimebroker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/templatecache"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// APIError represents a standardized error response.
type APIError struct {
	Code      string                 `json:"code"`
	Message   string                 `json:"message"`
	Details   map[string]interface{} `json:"details,omitempty"`
	RequestID string                 `json:"requestId,omitempty"`
}

// ErrorResponse wraps an APIError for JSON responses.
type ErrorResponse struct {
	Error APIError `json:"error"`
}

// Error codes matching the Runtime Broker API specification.
const (
	ErrCodeInvalidRequest     = "invalid_request"
	ErrCodeValidationError    = "validation_error"
	ErrCodeUnauthorized       = "unauthorized"
	ErrCodeForbidden          = "forbidden"
	ErrCodeAgentNotFound      = "agent_not_found"
	ErrCodeNotFound           = "not_found"
	ErrCodeConflict           = "conflict"
	ErrCodeMethodNotAllowed   = "method_not_allowed"
	ErrCodeInternalError      = "internal_error"
	ErrCodeRuntimeError       = "runtime_error"
	ErrCodeRuntimeUnavailable = "runtime_unavailable"
	ErrCodeHubUnreachable     = "hub_unreachable"
	ErrCodeTemplateError      = "template_error"
	ErrCodeSkillResolution    = "skill_resolution_failed"

	// ErrCodeAgentIdentityUnknown marks a delete/stop that could not be
	// verified as safe because a runtime process restart dropped the
	// in-memory record needed to tell "not found" apart from "exists, but
	// unidentifiable." Stable within this broker's own HTTP API, but what a
	// hub or CLI caller sees differs by which endpoint triggered it:
	//   - delete: the hub re-codes this broker's 409 as its own generic
	//     "conflict" error (pkg/hub/handlers_agents_core.go, the
	//     errors.As(err, &se) && se.StatusCode == http.StatusConflict check
	//     around its DispatchAgentDelete call), so a caller sees this code's
	//     message text, not the code itself;
	//   - stop: the hub's stop dispatch (pkg/hub/handlers_agent_lifecycle.go,
	//     the AgentActionStop case) has no equivalent check — every
	//     DispatchAgentStop error, this one included, becomes a generic 502
	//     "runtime_error" before the CLI sees it, so this 409 is not even
	//     distinguishable as a conflict there today.
	// Either way this constant lets broker-level callers and tests branch on
	// it, not (yet) the hub or the CLI.
	ErrCodeAgentIdentityUnknown = "agent_identity_unknown"

	// ErrCodeStaleDispatch marks a delete refused because it arrived after
	// the deadline the hub sent with it (notAfter, ptone/scion#2906): the
	// hub's claim on the delete may have lapsed, so acting could remove an
	// agent the user started again. Nothing was done.
	ErrCodeStaleDispatch = "stale_dispatch"

	// ErrCodeRuntimeLogsUnsupported marks a logs request that a runtime
	// declines to serve at all, rather than one that failed. The broker uses
	// this for pkg/runtime.ErrLogsNotSupported (pkg/runtime/capabilities.go),
	// which a runtime returns when serving logs at all would be unsafe or
	// impossible (for example, when reading them would expose another
	// tenant's output), so the runtime never makes the underlying call and
	// this code is the client-visible signal that the feature, not the
	// request, is the reason.
	ErrCodeRuntimeLogsUnsupported = "runtime_logs_unsupported"

	// ErrCodeRuntimeAttachUnsupported marks an attach request rejected
	// before the WebSocket upgrade because the target runtime declines
	// interactive attach outright (pkg/runtime.AttachCapableRuntime,
	// pkg/runtime/capabilities.go), rather than one that failed. Rejecting
	// here — instead of upgrading and only failing once the runtime's own
	// PTY dial rejects the stream — gives the caller a clean, pre-upgrade
	// error instead of an abnormal WebSocket close. Shares its wire value
	// with wsprotocol.ErrCodeRuntimeAttachUnsupported, which pkg/wsclient
	// reads back to map this to the same fixed message the post-upgrade
	// 4501 close code produces.
	ErrCodeRuntimeAttachUnsupported = wsprotocol.ErrCodeRuntimeAttachUnsupported
)

// writeError writes a JSON error response.
func writeError(w http.ResponseWriter, statusCode int, code, message string, details map[string]interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)

	resp := ErrorResponse{
		Error: APIError{
			Code:    code,
			Message: message,
			Details: details,
		},
	}

	_ = json.NewEncoder(w).Encode(resp)
}

// startAttemptedDetails returns the error details for a failure inside
// Manager.Start, so the hub knows the broker acted on the request (see
// api.BrokerErrorDetailStartAttempted).
func startAttemptedDetails(runID string) map[string]interface{} {
	d := map[string]interface{}{api.BrokerErrorDetailStartAttempted: true}
	if runID != "" {
		d[api.BrokerErrorDetailRunID] = runID
	}
	return d
}

// startFailureDetails is startAttemptedDetails plus the run the runtime
// holds for the agent now (api.BrokerErrorDetailCurrentRunID), when
// currentRunID can report one. mgr is the manager the start went to.
func (s *Server) startFailureDetails(ctx context.Context, mgr agent.Manager, id, projectID, runID string) map[string]interface{} {
	d := startAttemptedDetails(runID)
	if current, ok := s.currentRunID(ctx, mgr, id, projectID); ok {
		d[api.BrokerErrorDetailCurrentRunID] = current
	}
	return d
}

// currentRunID reports the scion.run_id of the agent's runtime entry after
// a failed start. It re-lists, under the request's ctx, allManagers(ctx)
// plus mgr (the one the start went to), scoped to projectID the way a
// delete resolves its target (collectAgentCandidates). allManagers honours
// a recorded runtime type on ctx (ptone/scion#2748): a restart carries one,
// so the list is limited to runtimes of the recorded type plus mgr; a
// start carries none, so every registered runtime is listed. Either way it
// sees what a later delete under the same ctx rules would see, including a
// previous entry left on another listed runtime.
//
//   - One container entry: its run, or "" when it carries no run label.
//   - Entries of more than one run: "", so the hub's next delete resolves
//     by name, as before run IDs. Name resolution fails closed (409) only
//     on ambiguity among the runtimes that delete itself lists; an entry
//     on a runtime outside its recorded type is not seen.
//   - No container entry on any runtime (file-only entries do not count):
//     ok is false. The hub then keeps the run it minted, so a delayed delete
//     cannot remove a same-name agent created later.
//   - Any List failure: ok is false; the hub falls back as for an older
//     broker.
func (s *Server) currentRunID(ctx context.Context, mgr agent.Manager, id, projectID string) (string, bool) {
	managers := s.allManagers(ctx)
	if mgr != nil && !slices.Contains(managers, mgr) {
		managers = append(managers, mgr)
	}
	cands, err := s.collectAgentCandidates(ctx, managers, id, projectID,
		"Agent start failed: could not re-list the agent to report its current run")
	if err != nil {
		return "", false
	}
	current, found := "", false
	for _, c := range cands {
		if c.entry.ContainerID == "" {
			continue
		}
		if found && c.entry.RunID != current {
			return "", true
		}
		current, found = c.entry.RunID, true
	}
	return current, found
}

// NotFound writes a 404 Not Found response.
func NotFound(w http.ResponseWriter, resource string) {
	code := ErrCodeNotFound
	if resource == "Agent" {
		code = ErrCodeAgentNotFound
	}
	writeError(w, http.StatusNotFound, code, resource+" not found", nil)
}

// BadRequest writes a 400 Bad Request response.
func BadRequest(w http.ResponseWriter, message string) {
	writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, message, nil)
}

// ValidationError writes a 400 Bad Request response for validation failures.
func ValidationError(w http.ResponseWriter, message string, details map[string]interface{}) {
	writeError(w, http.StatusBadRequest, ErrCodeValidationError, message, details)
}

// Unauthorized writes a 401 Unauthorized response.
func Unauthorized(w http.ResponseWriter) {
	writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
		"Authentication required", nil)
}

// Forbidden writes a 403 Forbidden response.
func Forbidden(w http.ResponseWriter) {
	writeError(w, http.StatusForbidden, ErrCodeForbidden,
		"Insufficient permissions", nil)
}

// MethodNotAllowed writes a 405 Method Not Allowed response. RFC 9110
// section 15.5.6 requires a 405 to carry an Allow header listing the methods
// the target resource supports. The signature requires at least one method,
// so a bare call does not compile; hack/check-method-not-allowed.sh remains
// as a backstop for helpers that keep a variadic-only signature.
func MethodNotAllowed(w http.ResponseWriter, allowedMethod string, otherMethods ...string) {
	methods := append([]string{allowedMethod}, otherMethods...)
	w.Header().Set("Allow", strings.Join(methods, ", "))
	writeError(w, http.StatusMethodNotAllowed, ErrCodeMethodNotAllowed,
		"Method not allowed", nil)
}

// Conflict writes a 409 Conflict response.
func Conflict(w http.ResponseWriter, message string) {
	writeError(w, http.StatusConflict, ErrCodeConflict, message, nil)
}

// StaleDispatch writes a 409 Conflict response with the stable
// ErrCodeStaleDispatch code.
func StaleDispatch(w http.ResponseWriter, message string) {
	writeError(w, http.StatusConflict, ErrCodeStaleDispatch, message, nil)
}

// AgentIdentityUnknown writes a 409 Conflict response with the stable
// ErrCodeAgentIdentityUnknown code for a delete/stop that a runtime process
// restart made impossible to verify as safe. message must stay generic —
// it must not name any runtime-specific scope (e.g. a namespace) or point
// at a runtime-specific document; a caller's own log line carries those
// details instead (see agentIdentityUnknownError).
func AgentIdentityUnknown(w http.ResponseWriter, message string) {
	writeError(w, http.StatusConflict, ErrCodeAgentIdentityUnknown, message, nil)
}

// RuntimeLogsUnsupported writes a 501 Not Implemented response with the
// stable ErrCodeRuntimeLogsUnsupported code for a runtime that declines to
// serve logs at all (pkg/runtime.ErrLogsNotSupported). message must not
// name any runtime-specific scope, worker, pod, namespace, actor or
// agent — see pkg/runtime.ErrLogsNotSupported for the fixed text this is
// meant to carry.
func RuntimeLogsUnsupported(w http.ResponseWriter, message string) {
	writeError(w, http.StatusNotImplemented, ErrCodeRuntimeLogsUnsupported, message, nil)
}

// RuntimeAttachUnsupported writes a 501 Not Implemented response with the
// stable ErrCodeRuntimeAttachUnsupported code for a runtime that declines
// interactive attach at all (pkg/runtime.AttachCapableRuntime). message must
// not name any runtime-specific scope, worker, pod, namespace, actor or
// agent, mirroring RuntimeLogsUnsupported.
func RuntimeAttachUnsupported(w http.ResponseWriter, message string) {
	writeError(w, http.StatusNotImplemented, ErrCodeRuntimeAttachUnsupported, message, nil)
}

// InternalError writes a 500 Internal Server Error response.
func InternalError(w http.ResponseWriter) {
	writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
		"Internal server error", nil)
}

// RuntimeError writes a 500 error for runtime failures.
func RuntimeError(w http.ResponseWriter, message string) {
	writeError(w, http.StatusInternalServerError, ErrCodeRuntimeError, message, nil)
}

// OpaqueError wraps a runtime-op failure (a container/pod runtime backend —
// Docker, Podman, Kubernetes, substrate — declining a lifecycle operation)
// with a fixed, identity-free message: Error() never returns the wrapped
// error's own text, since a real backend error routinely embeds a
// container ID, pod name, namespace, node name, image reference, or file
// path — internal infrastructure identity a broker client has no
// entitlement to see, whether or not it's authorized for the agent itself.
// The wrapped error stays reachable via Unwrap for server-side telemetry
// (span.SetStatus) and logging, neither of which is the HTTP response
// body.
type OpaqueError struct {
	msg string
	err error
}

// NewOpaqueError returns an OpaqueError whose Error() is exactly msg,
// wrapping err for telemetry/logging only.
func NewOpaqueError(msg string, err error) *OpaqueError {
	return &OpaqueError{msg: msg, err: err}
}

func (e *OpaqueError) Error() string { return e.msg }
func (e *OpaqueError) Unwrap() error { return e.err }

// runtimeOpError builds the OpaqueError a broker runtime-op handler
// (start, stop, restart, delete, exec, message, logs, list) writes to its
// client on failure: op names the operation in the fixed message (e.g.
// "stop agent", "list agents"), never anything about the specific agent,
// runtime, or backend involved. writeRuntimeOpError is the preferred caller
// for this; createAgent, the start/restart Manager.Start failure branches,
// and the stop handler's record-less-probe branch (handlers.go) call
// runtimeOpError/RuntimeError directly instead, each with its own inline
// log call (and, for create/start, its own span.SetStatus) rather than
// going through writeRuntimeOpError — raw err still always reaches the
// log on every path, just not through this one function.
func runtimeOpError(op string, err error) *OpaqueError {
	return NewOpaqueError(fmt.Sprintf("Failed to %s", op), err)
}

// writeRuntimeOpError is the call most runtime-op handlers (stop, restart,
// delete, exec, message, logs, list) make on failure: it logs err
// at scope op (plus any extra key/value pairs the caller has on hand — an
// agent or project ID, for instance) via s.agentLifecycleLog, records err on
// ctx's active span (trace.SpanFromContext(ctx) is a documented no-op when
// ctx carries none, so this is always safe to call), and writes the fixed,
// identity-free response body runtimeOpError builds.
//
// Not every runtime-op failure path routes through this one call — see
// runtimeOpError's own doc comment for the create/start/restart/stop sites
// that log and (mostly) set span status inline instead — but every one of
// them still logs err before building the fixed response body, so err
// always reaches the server's own log, never just the client's opaque
// "Failed to <op>" message.
//
// Never call this with a *startContextError: that type carries its own
// curated Status/Message from buildStartContext (a template/config
// problem, not runtime topology) and must go through
// writeStartContextError instead — routing it through runtimeOpError's
// fixed "Failed to <op>" message would discard a message that was already
// safe to show the caller verbatim.
func (s *Server) writeRuntimeOpError(w http.ResponseWriter, ctx context.Context, op string, err error, extra ...any) {
	args := make([]any, 0, len(extra)+2)
	args = append(args, "op", op)
	args = append(args, extra...)
	args = append(args, "error", err)
	s.agentLifecycleLog.Error("runtime op failed", args...)
	trace.SpanFromContext(ctx).SetStatus(codes.Error, err.Error())
	RuntimeError(w, runtimeOpError(op, err).Error())
}

// RuntimeUnavailable writes a 503 error for a transient container-runtime
// failure (e.g. `docker ps` still failing after internal retries). Unlike
// RuntimeError, this signals the caller should retry rather than treating
// the failure as permanent.
func RuntimeUnavailable(w http.ResponseWriter, message string) {
	writeError(w, http.StatusServiceUnavailable, ErrCodeRuntimeUnavailable, message, nil)
}

// agentLookupUnavailableMessage builds the generic "please retry" sentence
// used whenever an agent lookup fails because the container runtime itself
// is unavailable, rather than because the agent is genuinely missing.
// retrySuffix, if non-empty, is inserted into the sentence (e.g. PTY attach
// passes "the attach" to produce "please retry the attach in a moment");
// pass "" for the plain "please retry in a moment".
//
// This is factored out of AgentLookupUnavailable so that call sites which
// need the same wording but log their own distinct message (e.g. PTY
// attach's nil-result guard, which has no err to log) can build it without
// duplicating the sentence.
func agentLookupUnavailableMessage(agentID, retrySuffix string) string {
	retry := "retry"
	if retrySuffix != "" {
		retry = "retry " + retrySuffix
	}
	return fmt.Sprintf(
		"Unable to look up agent %q: the container runtime is temporarily unavailable. Please %s in a moment.",
		agentID, retry)
}

// AgentLookupUnavailable logs the underlying runtime error that caused an
// agent lookup to fail (server-side only — see ptone/scion#2164 for keeping
// raw runtime error text out of response bodies generally) and writes a
// generic 503 response built from agentID. Callers use this when
// errors.Is(err, ErrAgentListUnavailable): the container runtime itself
// failed to respond, which is not the same as the agent being genuinely
// missing, so the client should retry rather than be told "not found".
//
// op identifies the calling handler (e.g. "exec", "reset_auth", "stop",
// "restart", "pty_attach") and is included in the log line so call-site
// context isn't lost when several handlers share this one log message.
//
// retrySuffix is passed through to agentLookupUnavailableMessage.
func AgentLookupUnavailable(w http.ResponseWriter, err error, agentID, op, retrySuffix string) {
	slog.Warn("agent lookup failed: runtime listing unavailable", "agent_id", agentID, "op", op, "error", err)
	RuntimeUnavailable(w, agentLookupUnavailableMessage(agentID, retrySuffix))
}

// HubUnreachableError writes a 503 Service Unavailable response for Hub connectivity issues.
// This indicates that the Hub is temporarily unreachable and the operation should be retried.
func HubUnreachableError(w http.ResponseWriter, details string) {
	writeError(w, http.StatusServiceUnavailable, ErrCodeHubUnreachable,
		"Hub is unreachable. Check Hub connectivity or use solo mode.", map[string]interface{}{
			"details": details,
		})
}

// TemplateError writes a 500 error for template-related failures.
func TemplateError(w http.ResponseWriter, message string) {
	writeError(w, http.StatusInternalServerError, ErrCodeTemplateError, message, nil)
}

// Unprocessable writes a 422 Unprocessable Entity response.
func Unprocessable(w http.ResponseWriter, message string) {
	writeError(w, http.StatusUnprocessableEntity, ErrCodeValidationError, message, nil)
}

// skillResolutionHTTPStatus maps a SkillResolutionError.Code to an HTTP
// status. Each cause gets the status whose standard semantics best fit it:
//   - not_found: 404, a skill genuinely absent at the given ref.
//   - rate_limited: 429, with a Retry-After header when the server sent one
//     (see SkillResolutionFailed).
//   - timeout: 504, no response from GitHub within the request deadline.
//   - upstream_unavailable: 502, GitHub itself returned repeated 5xx.
//   - unreachable: 502, a network-level failure (DNS, connection refused,
//     TLS) rather than a response GitHub chose to send.
//   - forbidden: 403, the Hub's per-URI code for a gh:// ref the caller may
//     not resolve GitHub skills for in this project.
//   - anything else — including an uncategorized local failure and the Hub's
//     own per-URI codes for PreResolvedSkills (storage_error, internal_error,
//     federation_error) — keeps the existing 500, not a client error: the
//     caller did nothing wrong, so a 5xx is a more honest signal than a
//     guessed 4xx (#2546 R3, O1).
func skillResolutionHTTPStatus(code string) int {
	switch code {
	case agent.SkillErrCodeNotFound:
		return http.StatusNotFound
	case agent.SkillErrCodeForbidden:
		return http.StatusForbidden
	case agent.SkillErrCodeRateLimited:
		return http.StatusTooManyRequests
	case agent.SkillErrCodeTimeout:
		return http.StatusGatewayTimeout
	case agent.SkillErrCodeUpstreamUnavailable, agent.SkillErrCodeUnreachable:
		return http.StatusBadGateway
	default:
		return http.StatusInternalServerError
	}
}

// SkillResolutionFailed writes a response for a required skill reference that
// could not be resolved within the create deadline, naming the ref and the
// cause instead of folding the failure into a generic 500/502 (#2546). Every
// mapped status gets the same {skill, cause} detail payload so the response
// is actionable without broker logs, including the uncategorized/5xx default.
func SkillResolutionFailed(w http.ResponseWriter, err *agent.SkillResolutionError) {
	skillResolutionFailedWithDetails(w, err, nil)
}

// skillResolutionFailedWithDetails is SkillResolutionFailed with extra error
// details (for example the start markers from startFailureDetails) merged
// alongside the skill and cause.
func skillResolutionFailedWithDetails(w http.ResponseWriter, err *agent.SkillResolutionError, extra map[string]interface{}) {
	if err.Code == agent.SkillErrCodeRateLimited && err.RetryAfter != "" {
		w.Header().Set("Retry-After", err.RetryAfter)
	}
	details := make(map[string]interface{}, len(extra)+2)
	for k, v := range extra {
		details[k] = v
	}
	details["skill"] = err.URI
	details["cause"] = err.Code
	writeError(w, skillResolutionHTTPStatus(err.Code), ErrCodeSkillResolution,
		"Failed to provision agent: "+err.Error(), details)
}

// writeStartContextError writes the HTTP response for an error returned by
// buildStartContext, honoring startContextError.Status (e.g. the 400 the
// Kubernetes/"block" rejection sets, ptone/scion#2328) instead of collapsing
// every failure into a generic 500. Returns the status code actually
// written, so a caller that also tracks the dispatch attempt's status (e.g.
// createAgent's markAttemptFailed) can record the same value instead of
// hardcoding one.
//
// A Hub-connectivity failure (IsHubError) keeps its existing, more specific
// handling — a retryable 503 hub_unreachable, or a redacted 500
// template_error for any other hydration failure — ahead of the generic
// Status check; neither of those was the 500-collapsing bug this helper
// fixes. err need not be a *startContextError at all (any error
// buildStartContext could return, including ones from other call sites in
// this package): a plain error still gets the pre-existing generic 500
// behavior.
//
// Any 4xx Status — not just exactly 400 — is treated as a client-caused
// validation failure: buildStartContext only ever sets Status to a value it
// means as a client error, so collapsing just one 4xx (400) into the generic
// 500 path while honoring others would be an arbitrary distinction, not a
// deliberate one. sce.Message is written to the client verbatim here (never
// through runtimeOpError's redaction) since buildStartContext composes it
// itself as client-safe text for exactly this case.
//
// Every other path writes a client-safe, op-labeled message built by
// runtimeOpError (see its own doc comment for why) instead of err's own
// text, which can carry a container ID, pod name, namespace, node name,
// image reference, or file path a broker client has no entitlement to see —
// template hydration's own error text is no exception (it can carry a
// template path or a storage bucket/object name). Each such path logs the
// real error server-side first (preferring a *startContextError's
// OriginalErr, the actual underlying failure, over its own curated Message)
// so the detail reaches the broker's own diagnostics before being redacted
// out of the response body.
func (s *Server) writeStartContextError(w http.ResponseWriter, err error, op string) int {
	sce, ok := err.(*startContextError)
	if !ok {
		s.agentLifecycleLog.Warn("buildStartContext failed", "op", op, "error", err)
		RuntimeError(w, runtimeOpError(op, err).Error())
		return http.StatusInternalServerError
	}
	if sce.IsHubError {
		if templatecache.IsHubConnectivityError(sce.OriginalErr) {
			HubUnreachableError(w, sce.OriginalErr.Error())
			return http.StatusServiceUnavailable
		}
		s.agentLifecycleLog.Warn("buildStartContext failed", "op", op, "error", startContextDiagnostic(sce))
		TemplateError(w, runtimeOpError(op, err).Error())
		return http.StatusInternalServerError
	}
	if sce.Status >= 400 && sce.Status < 500 {
		writeError(w, sce.Status, ErrCodeValidationError, sce.Message, nil)
		return sce.Status
	}
	s.agentLifecycleLog.Warn("buildStartContext failed", "op", op, "error", startContextDiagnostic(sce))
	RuntimeError(w, runtimeOpError(op, err).Error())
	return http.StatusInternalServerError
}

// startContextDiagnostic returns the real, underlying error a
// *startContextError's own Message may have been built from: its
// OriginalErr when set (the actual failure buildStartContext composed
// Message around), or sce itself when OriginalErr is nil (Message IS the
// whole story — e.g. an unpinned image or an unrecognized GCP metadata
// mode — so there is nothing more specific to log). Used so a server-side
// log line records the real detail, not just the client-safe Message the
// caller is about to redact out of the response body.
func startContextDiagnostic(sce *startContextError) error {
	if sce.OriginalErr != nil {
		return sce.OriginalErr
	}
	return sce
}

// startContextSpanText is startContextDiagnostic's counterpart for a span
// status message, callable with buildStartContext's raw, not-yet-type-
// asserted return value: a caller recording err on an OTEL span via
// span.SetStatus(codes.Error, err.Error()) would otherwise record a
// *startContextError's own curated, client-safe Message (that type's
// Error() method returns Message verbatim) on an internal diagnostics
// surface that should carry the real failure instead — spans are never
// shown to the broker's own caller the way the HTTP response body is. When
// err is not a *startContextError at all, its own Error() text is already
// the real detail, so it passes through unchanged.
func startContextSpanText(err error) string {
	if sce, ok := err.(*startContextError); ok {
		return startContextDiagnostic(sce).Error()
	}
	return err.Error()
}
