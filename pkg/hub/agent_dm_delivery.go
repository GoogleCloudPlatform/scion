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
)

// finalizationTimeout is the maximum time allowed for post-dispatch state
// transitions (MarkMessageDispatched / MarkMessageFailed) when the original
// request context has been cancelled. This prevents silently discarding
// known outcomes (AC-3).
//
// 5s is ample for a single-row CAS on the local store; anything slower
// indicates a store problem that waiting longer would not fix.
const finalizationTimeout = 5 * time.Second

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

// markDispatched transitions a message from pending to dispatched after
// broker/managed-runtime acceptance. Uses a bounded finalization context
// so request cancellation does not discard the known outcome.
//
// Returns (dispatched=true) on successful CAS, (dispatched=false) if the
// row was already transitioned (duplicate protection), or an error on
// store failure.
func (s *Server) markDispatched(ctx context.Context, msgID string) (bool, error) {
	finCtx, finCancel := finalizationContext(ctx)
	defer finCancel()
	return s.store.MarkMessageDispatched(finCtx, msgID)
}

// markFailed records a definite dispatch failure on the message row.
// Uses a bounded finalization context for request cancellation resilience.
func (s *Server) markFailed(ctx context.Context, msgID string, reason string) error {
	return markMessageFailed(ctx, s.store, msgID, reason)
}

// messageFailureMarker is the slice of the store needed to record a dispatch
// failure. Narrow so MessageBrokerProxy (which has no *Server) can share
// markMessageFailed.
type messageFailureMarker interface {
	MarkMessageFailed(ctx context.Context, id string, reason string) error
}

// markMessageFailed records a definite dispatch failure on a message row on a
// finalizationContext derived from ctx (ptone/scion#1838). The dispatch ctx is
// frequently already expired by the time a failure is known —
// dispatchWithBrokerRetry returns ErrBrokerTimeout exactly when it fires — and
// a mark on that ctx would fail, leaving the row "dispatched" forever (the
// broker message sweep only reprocesses "pending" rows). Every
// MarkMessageFailed call site in pkg/hub should go through this helper (or
// Server.markFailed) rather than calling the store directly.
//
// The reason is passed through sanitizeFailureReason before it is persisted
// as dispatch_failure_reason (ptone/scion#1841): on the synchronous paths it
// is a raw dispatch error that can embed an arbitrary broker response body.
// Sanitizing is idempotent, so already-sanitized reasons are unaffected.
func markMessageFailed(ctx context.Context, st messageFailureMarker, msgID string, reason string) error {
	finCtx, finCancel := finalizationContext(ctx)
	defer finCancel()
	return st.MarkMessageFailed(finCtx, msgID, sanitizeFailureReason(reason))
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

// isAmbiguousDispatchError returns true if the dispatch error indicates
// the message may have been accepted by the runtime but confirmation was
// lost — e.g. context cancellation or deadline during the network call.
// These errors should result in an ambiguous outcome rather than a
// definite failure.
func isAmbiguousDispatchError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
