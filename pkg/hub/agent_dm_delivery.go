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

// ---------------------------------------------------------------------------
// Agent DM Delivery Lifecycle Helpers (#1689)
// ---------------------------------------------------------------------------
//
// These helpers implement truthful broker/managed-runtime delivery outcomes
// for the shared ExecuteAgentDM operation. They ensure that:
//
//   - Messages are persisted as transient "pending" and transition to
//     "dispatched" only after broker/managed-runtime acceptance.
//   - Definite dispatch failures return non-2xx and persist "failed" state.
//   - Missing dispatcher/broker returns an explicit availability error
//     before persistence.
//   - Post-dispatch state transitions use a bounded finalization context
//     so request cancellation does not silently discard known outcomes.
//   - CAS on MarkMessageDispatched prevents duplicate dispatch.

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// finalizationTimeout is the maximum time allowed per post-dispatch state
// transition attempt (MarkMessageDispatched / MarkMessageFailed) when the
// original request context has been cancelled. This prevents silently
// discarding known outcomes (AC-3).
//
// 5s is ample for a single-row CAS on the local store; transient lock or
// deadline failures are retried up to finalizationMaxAttempts times on fresh
// detached contexts (ptone/scion#7405).
const finalizationTimeout = 5 * time.Second

// finalizationMaxAttempts bounds how many detached store attempts
// markDispatched and markMessageFailed make before giving up. Only the DB
// state transition is retried — never the broker delivery (ptone/scion#7405).
const finalizationMaxAttempts = 3

// finalizationRetryBackoff is the initial pause between finalization store
// retries (doubled after each attempt). Variable so tests can adjust if needed.
var finalizationRetryBackoff = 50 * time.Millisecond

// deliveryNoticeTimeout bounds the detached budget for sending a
// DELIVERY_FAILED notice back to the sender (ptone/scion#1838). Unlike the
// row CAS above, a notice resolves the sender agent and then dispatches
// through the runtime broker, which is a network round trip to a possibly
// slow or recovering broker. 15s gives that dispatch room to complete
// without letting a stuck broker pin the goroutine indefinitely.
const deliveryNoticeTimeout = 15 * time.Second

// checkDispatchAvailability verifies that dispatch infrastructure is
// available for the target agent. Returns nil if dispatch can proceed,
// or an *AgentDMError if dispatch is unavailable.
//
// This check runs before message persistence so that unavailable dispatch
// does not leave orphaned pending rows or falsely report success (AC-2).
func (s *Server) checkDispatchAvailability(input *AgentDMInput) *AgentDMError {
	if s.ShuttingDown() {
		return shuttingDownDMError("")
	}

	if isManagedAgentRuntime(input.TargetAgent.Runtime) {
		// Managed runtime dispatch is always available at pre-check time;
		// backend resolution happens at dispatch time and any failure there
		// is a definite dispatch failure, not an availability gap.
		return nil
	}

	if input.TargetAgent.RuntimeBrokerID == "" {
		// No broker configured — there is no dispatch path to the agent.
		return &AgentDMError{
			Code:       ErrCodeDeliveryFailed,
			Message:    "target agent has no runtime broker; dispatch unavailable",
			HTTPStatus: http.StatusUnprocessableEntity,
			Details: map[string]interface{}{
				"reason": "no_runtime_broker",
			},
		}
	}

	if s.GetDispatcher() == nil {
		// Broker is configured but the dispatch infrastructure is not ready.
		return &AgentDMError{
			Code:       ErrCodeUnavailable,
			Message:    "message dispatch infrastructure is currently unavailable",
			HTTPStatus: http.StatusServiceUnavailable,
		}
	}

	return nil
}

// finalizationContext returns a context suitable for post-dispatch state
// transitions. It uses context.WithoutCancel to detach from the parent's
// cancellation signal while preserving its values, then applies a bounded
// timeout. This ensures that critical state-transition writes (e.g.
// MarkMessageDispatched / MarkMessageFailed) complete even if the parent
// request context is cancelled mid-flight.
func finalizationContext(parent context.Context) (context.Context, context.CancelFunc) {
	return detachedContext(parent, finalizationTimeout)
}

// detachedContext detaches from parent's cancellation (keeping its values)
// and applies timeout d. finalizationContext is the row-CAS flavour; use
// detachedContext directly when the post-dispatch work needs a different
// budget (e.g. deliveryNoticeTimeout).
func detachedContext(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), d)
}

// agentDMDispatchTimeout caps one ExecuteAgentDM dispatch, broker retries
// included.
const agentDMDispatchTimeout = 30 * time.Second

// dispatchContext returns the context ExecuteAgentDM dispatches on: it keeps
// parent's values and deadline but not its cancellation, and is capped at
// d (ptone/scion#4827). The row is already persisted when the dispatch
// starts, so a caller that goes away must not abort the dispatch and leave
// the row pending; only a deadline (the caller's or d) can still end it
// ambiguous.
func dispatchContext(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), d)
	if dl, ok := parent.Deadline(); ok {
		dctx, dcancel := context.WithDeadline(ctx, dl)
		return dctx, func() { dcancel(); cancel() }
	}
	return ctx, cancel
}

func isNonRetryableFinalizationError(err error) bool {
	return errors.Is(err, store.ErrInvalidInput) || errors.Is(err, store.ErrNotFound)
}

func retryFinalization(ctx context.Context, fn func(finCtx context.Context) error) error {
	backoff := finalizationRetryBackoff
	var lastErr error
	for attempt := 0; attempt < finalizationMaxAttempts; attempt++ {
		if attempt > 0 && backoff > 0 {
			time.Sleep(backoff)
			backoff *= 2
		}
		finCtx, finCancel := finalizationContext(ctx)
		err := fn(finCtx)
		finCancel()
		if err == nil {
			return nil
		}
		lastErr = err
		if isNonRetryableFinalizationError(err) {
			break
		}
	}
	return lastErr
}

// markDispatched transitions a message from pending to dispatched after
// broker/managed-runtime acceptance. Uses fresh bounded finalization contexts
// across up to finalizationMaxAttempts attempts so request cancellation or a
// transient store stall does not discard the known outcome (ptone/scion#7405).
//
// Returns (dispatched=true) on successful CAS, (dispatched=false) if the
// row was already transitioned (duplicate protection), or an error on
// store failure after exhausting retries.
func (s *Server) markDispatched(ctx context.Context, msgID string) (bool, error) {
	var dispatched bool
	err := retryFinalization(ctx, func(finCtx context.Context) error {
		var casErr error
		dispatched, casErr = s.store.MarkMessageDispatched(finCtx, msgID)
		return casErr
	})
	return dispatched, err
}

// markFailed records a definite dispatch failure on the message row.
// Uses bounded finalization contexts for request cancellation resilience.
func (s *Server) markFailed(ctx context.Context, msgID string, reason string) error {
	return markMessageFailed(ctx, s.store, msgID, reason)
}

// failUndelivered atomically transitions an undelivered message row to
// dispatch_state="failed" on fresh bounded finalization contexts and reports
// whether this call made the transition. Rows already marked "delivered" or
// "failed" are left untouched and return (false, nil), so callers that emit
// async sender notices only notify when they own the transition.
func (s *Server) failUndelivered(ctx context.Context, msgID string, reason string) (bool, error) {
	sanitized := sanitizeFailureReason(reason)
	var won bool
	err := retryFinalization(ctx, func(finCtx context.Context) error {
		var failErr error
		won, failErr = s.store.FailUndeliveredMessage(finCtx, msgID, sanitized)
		return failErr
	})
	return won, err
}

// messageFailureMarker is the slice of the store needed to record a dispatch
// failure. Narrow so MessageBrokerProxy (which has no *Server) can share
// markMessageFailed.
type messageFailureMarker interface {
	MarkMessageFailed(ctx context.Context, id string, reason string) error
}

// markMessageFailed records a definite dispatch failure on a message row on
// fresh finalizationContexts derived from ctx (ptone/scion#1838,
// ptone/scion#7405). The dispatch ctx is frequently already expired by the
// time a failure is known — dispatchWithBrokerRetry returns ErrBrokerTimeout
// exactly when it fires — and a mark on that ctx would fail, leaving the row
// "dispatched" or "pending". Every MarkMessageFailed call site in pkg/hub
// should go through this helper (or Server.markFailed) rather than calling the
// store directly.
//
// The reason is passed through sanitizeFailureReason before it is persisted
// as dispatch_failure_reason (ptone/scion#1841): on the synchronous paths it
// is a raw dispatch error that can embed an arbitrary broker response body.
// Sanitizing is idempotent, so already-sanitized reasons are unaffected.
func markMessageFailed(ctx context.Context, st messageFailureMarker, msgID string, reason string) error {
	sanitized := sanitizeFailureReason(reason)
	return retryFinalization(ctx, func(finCtx context.Context) error {
		return st.MarkMessageFailed(finCtx, msgID, sanitized)
	})
}

// shuttingDownDMError constructs a retryable 503 AgentDMError when the hub
// is shutting down (ptone/scion#7405). When msgID is non-empty (the narrow
// race where BeginShutdown fired after CreateMessage and before
// dispatchShutdownGate), message_id is included in details so callers can
// correlate the failed record; when empty, refusal occurred before persistence.
func shuttingDownDMError(msgID string) *AgentDMError {
	var details map[string]interface{}
	if msgID != "" {
		details = map[string]interface{}{
			"message_id": msgID,
		}
	}
	return &AgentDMError{
		Code:       ErrCodeUnavailable,
		Message:    "the server is shutting down; retry shortly",
		HTTPStatus: http.StatusServiceUnavailable,
		RetryAfter: shutdownRetryAfterSeconds * time.Second,
		Details:    details,
	}
}

// dispatchFailedError constructs an AgentDMError for a definite dispatch
// failure. The message ID is included in the error details so callers can
// correlate the persisted record with the failure (AC-2).
func dispatchFailedError(msgID string) *AgentDMError {
	return &AgentDMError{
		Code:       ErrCodeDeliveryFailed,
		Message:    "message persisted but delivery to target agent failed",
		HTTPStatus: http.StatusBadGateway,
		Details: map[string]interface{}{
			"message_id": msgID,
		},
	}
}

// agentNotRunningDispatchError returns the typed error for a DM whose target
// the broker reported as having no running container (broker 404
// agent_not_found). The message is persisted and marked failed.
func agentNotRunningDispatchError(msgID string) *AgentDMError {
	return &AgentDMError{
		Code:       ErrCodeAgentNotRunning,
		Message:    "message persisted but the target agent has no running container",
		HTTPStatus: http.StatusConflict,
		Details: map[string]interface{}{
			"message_id": msgID,
		},
	}
}

// dispatchOutcomeUnknownReason is recorded as dispatch_failure_reason when an
// agent DM's dispatch ends with an unknown outcome after the message was
// stored (for example, the dispatch deadline expired mid-flight), finalizing
// the row right away instead of leaving it pending until TTL expiry
// (ptone/scion#5111).
const dispatchOutcomeUnknownReason = "dispatch outcome unknown"

// isAmbiguousDispatchError returns true if the dispatch error indicates
// the message may have been accepted by the runtime but confirmation was
// lost — e.g. context cancellation or deadline during the network call.
// Callers receive an ambiguous outcome so they do not blindly retry, while the
// stored row is finalized as failed with dispatchOutcomeUnknownReason unless
// already delivered (ptone/scion#5111).
func isAmbiguousDispatchError(err error) bool {
	if isPreDispatchDeliveryError(err) {
		return false
	}
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
