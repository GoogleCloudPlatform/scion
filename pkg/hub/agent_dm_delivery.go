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
const finalizationTimeout = 5 * time.Second

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
// transitions. If the parent context is still active, it returns a derived
// context with the finalization timeout. If the parent is already cancelled,
// it returns a new bounded context detached from the parent so that known
// outcomes are not silently discarded.
func finalizationContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent.Err() != nil {
		// Parent cancelled — use a fresh bounded context.
		return context.WithTimeout(context.Background(), finalizationTimeout)
	}
	// Parent alive — derive from it but add a deadline.
	return context.WithTimeout(parent, finalizationTimeout)
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
	finCtx, finCancel := finalizationContext(ctx)
	defer finCancel()
	return s.store.MarkMessageFailed(finCtx, msgID, reason)
}

// dispatchFailedError constructs an AgentDMError for a definite dispatch
// failure. The message ID is included in the error details so callers can
// correlate the persisted record with the failure (AC-2).
func dispatchFailedError(msgID string, dispatchErr error) *AgentDMError {
	return &AgentDMError{
		Code:       ErrCodeDeliveryFailed,
		Message:    "message persisted but delivery to target agent failed",
		HTTPStatus: http.StatusBadGateway,
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
