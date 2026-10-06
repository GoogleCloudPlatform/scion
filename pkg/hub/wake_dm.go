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
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ---------------------------------------------------------------------------
// Shared wake helper for agent DM delivery (#1691)
// ---------------------------------------------------------------------------
//
// wakeAgentForDM implements the resume-before-delivery lifecycle for agent DMs.
// Both the outbound handler (deliveryAgentDM path) and the structured/inbound
// handler (agent-to-agent path) call this helper through ExecuteAgentDM so that
// every agent DM observes the same wake behavior.
//
// The helper runs AFTER all admission checks (rate limit, message length,
// authorization, attachment rejection) so that denied requests cannot
// resume an agent (AC-2).

// WakeOutcome enumerates the possible outcomes of a wake attempt.
type WakeOutcome string

const (
	// WakeNotRequested indicates wake was not requested by the caller.
	WakeNotRequested WakeOutcome = "not_requested"

	// WakeAlreadyRunning indicates the target agent was already running.
	// No resume was invoked.
	WakeAlreadyRunning WakeOutcome = "already_running"

	// WakeResumed indicates the target agent was suspended and has been
	// successfully resumed and is now ready for message delivery.
	WakeResumed WakeOutcome = "resumed"
	// WakeDeferred indicates another start of the target agent was already
	// in progress (it holds the agent's start claim): the message is kept,
	// deferred, and the sender gets no error.
	WakeDeferred WakeOutcome = "deferred"
)

// WakeResult is the typed result of a wake attempt.
type WakeResult struct {
	// Outcome is the wake result state.
	Outcome WakeOutcome
}

// wakeAgentForDM resumes a suspended target agent before DM delivery.
// It validates the agent's lifecycle phase and runtime, dispatches a resume
// command, and waits for the agent to become ready.
//
// Returns (*WakeResult, nil) on success or (nil, *AgentDMError) on failure.
// On failure, no message should be dispatched (AC-4).
func (s *Server) wakeAgentForDM(ctx context.Context, agent *store.Agent) (*WakeResult, *AgentDMError) {
	phase := state.Phase(agent.Phase)

	// A create launch in flight or left incomplete skips the wake in any
	// phase. The answer comes from the start gate, so a delete in progress
	// still wins over the launch refusal.
	if launchStartRefusal(agent, time.Now()).refuses() {
		if ref := s.startGate(ctx, agent, startEntryWake); ref.refuses() {
			s.messageLog.Info("wake: skipped by the start gate",
				"agent_id", agent.ID, "code", ref.Code)
			return nil, ref.dmError()
		}
	}

	switch phase {
	case state.PhaseSuspended:
		// Start gate (design ptone/scion#2483 §2.1): a delete in progress,
		// an incomplete create or an in-flight launch skips the wake before
		// any quota reservation or start dispatch, so the message is not
		// delivered either (AC-4). The sender's DM authz already ran.
		if ref := s.startGate(ctx, agent, startEntryWake); ref.refuses() {
			s.messageLog.Info("wake: skipped by the start gate",
				"agent_id", agent.ID, "code", ref.Code)
			return nil, ref.dmError()
		}

		// Managed runtimes do not support the suspend/resume lifecycle.
		// Return an explicit error rather than silently pretending to resume.
		if isManagedAgentRuntime(agent.Runtime) {
			return nil, &AgentDMError{
				Code:       ErrCodeUnsupportedCapability,
				Message:    "wake is not supported for managed agent runtimes; the target agent's runtime does not support suspend/resume",
				HTTPStatus: http.StatusUnprocessableEntity,
				Details: map[string]interface{}{
					"agent_id": agent.ID,
					"runtime":  agent.Runtime,
				},
			}
		}

		dispatcher := s.GetDispatcher()
		if dispatcher == nil {
			return nil, &AgentDMError{
				Code:       ErrCodeUnavailable,
				Message:    "dispatch not available — server may still be starting up",
				HTTPStatus: http.StatusServiceUnavailable,
			}
		}
		if agent.RuntimeBrokerID == "" {
			return nil, &AgentDMError{
				Code:       ErrCodeUnavailable,
				Message:    "agent has no runtime broker assigned",
				HTTPStatus: http.StatusServiceUnavailable,
			}
		}

		// Resume the suspended agent (continue=true restores its prior
		// session) under a start claim of kind wake, which records run
		// intent running. startAgentCore re-reserves the broker capacity
		// released at suspend and marks the agent starting for the dispatch
		// (beginStartDispatch; rolled back if the start fails), and the
		// claim is held until the readiness wait ends. A readiness timeout
		// releases the claim like a success: the start itself was accepted.
		var statusErr *AgentDMError
		var readyErr error
		err := s.startAgentCore(ctx, agent, StartOpts{Kind: store.StartClaimWake, Resume: true, KeepCallerDeadline: true, AfterStart: func(ctx context.Context, st startedState) error {
			// Re-assert 'starting' (beginStartDispatch already wrote it) and
			// clear the previous generation's leftovers while the lifecycle
			// op is still held: a heartbeat guarded during the dispatch may
			// have stored the old container's exit message, exit fields and
			// container status. Clearing them now, before the readiness
			// wait, leaves alone anything the new container posts later
			// (its first status, which is the readiness signal).
			statusUpdate := store.AgentStatusUpdate{
				Phase:                 string(state.PhaseStarting),
				ClearTerminalRemnants: true,
				ContainerStatus:       agent.ContainerStatus,
			}
			if err := s.store.UpdateAgentStatus(ctx, agent.ID, statusUpdate); err != nil {
				s.messageLog.Error("wake: failed to update agent phase to starting",
					"agent_id", agent.ID, "error", err)
				statusErr = &AgentDMError{
					Code:       ErrCodeInternalError,
					Message:    "failed to update agent status after resume",
					HTTPStatus: http.StatusInternalServerError,
				}
				return nil
			}
			agent.Phase = string(state.PhaseStarting)
			// The dispatch leg is over. End the lifecycle op so the
			// readiness wait sees a heartbeat-reported exit. Residual (as
			// before ptone/scion#2014): a heartbeat gathered before the
			// resume that reports the suspended container stopped, landing
			// after this point, moves the row to stopped and releases the
			// slot while the new container is up; the running write below
			// does not re-reserve, so the count stays low until the hourly
			// backfill.
			st.EndOp()
			// Publish from a re-read: a delete that claimed the row meanwhile
			// must not be painted over (design ptone/scion#2483 note F).
			s.publishAgentStatusFresh(ctx, agent)
			// Wait for the agent to report its first activity (readiness signal).
			readyErr = s.waitForAgentReady(ctx, agent.ID, 30*time.Second)
			if readyErr != nil {
				return nil
			}
			// Agent is ready — transition to 'running', still under the
			// claim, so a stop or start recorded meanwhile is never painted
			// over. A plain phase write: the message, stalled marker and
			// exit fields on the row now belong to the new generation.
			if err := s.store.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{Phase: string(state.PhaseRunning)}); err != nil {
				s.messageLog.Error("wake: failed to update agent phase to running",
					"agent_id", agent.ID, "error", err)
				statusErr = &AgentDMError{
					Code:       ErrCodeInternalError,
					Message:    "failed to update agent status after readiness",
					HTTPStatus: http.StatusInternalServerError,
				}
				return nil
			}
			agent.Phase = string(state.PhaseRunning)
			return nil
		}})
		if err != nil {
			var quotaErr *startQuotaError
			if errors.As(err, &quotaErr) {
				if errors.Is(err, store.ErrQuotaExceeded) {
					return nil, &AgentDMError{
						Code:       ErrCodeQuotaExceeded,
						Message:    quotaExceededMessage(store.LimitMaxAgentsPerBroker),
						HTTPStatus: http.StatusTooManyRequests,
					}
				}
				return nil, &AgentDMError{
					Code:       ErrCodeRuntimeError,
					Message:    "quota check failed: " + err.Error(),
					HTTPStatus: http.StatusInternalServerError,
				}
			}
			var held *store.ClaimHeldError
			if errors.As(err, &held) {
				// Another start holds the agent's claim: it is already
				// starting. The message is kept (deferred) and the sender
				// gets no error.
				s.messageLog.Info("wake: another start or a stop holds the agent; message deferred",
					"agent_id", agent.ID, "holder", string(held.Kind))
				agent.StartClaimKind = held.Kind // for the delivery note
				return &WakeResult{Outcome: WakeDeferred}, nil
			}
			if errors.Is(err, errStartingWrite) && deleteClaimedDuringDispatch(err, agent.ID) == nil {
				if errors.Is(err, store.ErrPhaseMismatch) {
					// The agent left suspended after it was read: nothing
					// was dispatched.
					return nil, &AgentDMError{
						Code:       ErrCodeConflict,
						Message:    "Failed to wake agent: its phase changed; retry",
						HTTPStatus: http.StatusConflict,
					}
				}
				return nil, &AgentDMError{
					Code:       ErrCodeRuntimeError,
					Message:    "Failed to wake agent: " + err.Error(),
					HTTPStatus: http.StatusInternalServerError,
				}
			}
			// A delete claimed the row after the start gate passed: the
			// start was refused (ptone/scion#2550).
			if ref := deleteClaimedDuringDispatch(err, agent.ID); ref != nil {
				return nil, ref.dmError()
			}
			if refusal := s.launchRefusalFromError(ctx, agent.ID, err); refusal != nil {
				s.messageLog.Info("wake: skipped, agent create is launching or incomplete",
					"agent_id", agent.ID, "code", refusal.Code)
				return nil, refusal.dmError()
			}
			if errors.Is(err, errBrokerLacksEmptyPerAgent) {
				// Fail closed like the other dispatch sites (design #2703 D3).
				return nil, &AgentDMError{
					Code:       ErrCodeUnsupportedCapability,
					Message:    "Failed to wake agent: " + err.Error(),
					HTTPStatus: http.StatusPreconditionFailed,
				}
			}
			return nil, &AgentDMError{
				Code:       ErrCodeRuntimeError,
				Message:    "Failed to wake agent: " + err.Error(),
				HTTPStatus: http.StatusBadGateway,
			}
		}
		if statusErr != nil {
			return nil, statusErr
		}

		if err := readyErr; err != nil {
			// A readiness timeout does not mean the container has exited —
			// the harness may just be slow, or hung, while still occupying
			// the broker slot. Leave Phase unset (a no-op field on
			// UpdateAgentStatus) rather than transitioning to
			// state.PhaseError: error is an uncounted phase for
			// max_agents_per_broker (isBrokerQuotaCountedPhase), so writing
			// it here would let both this write's own bookkeeping and the
			// periodic ReconcileStaleBrokerQuotaReservations sweep release
			// the reservation while the container may still be running
			// (ptone/scion#1984). Staying in "starting" keeps the slot
			// counted; the real release happens once the runtime broker
			// reports a confirmed exit over heartbeat, which drives
			// reconcileBrokerQuotaOnPhaseChange the normal way. The Message
			// field is still recorded for visibility.
			//
			// Use a detached context so this cleanup write succeeds even if
			// the parent context was cancelled (e.g. client disconnect).
			cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			_ = s.store.UpdateAgentStatus(cleanupCtx, agent.ID, store.AgentStatusUpdate{
				Message: "Failed to become ready after wake",
			})
			cleanupCancel()
			return nil, &AgentDMError{
				Code:       ErrCodeRuntimeError,
				Message:    "Agent resumed but did not become ready; message was not delivered: " + err.Error(),
				HTTPStatus: http.StatusBadGateway,
			}
		}

		// Agent is ready — transition to 'running'.
		// The running write ran under the claim (above); publish from a
		// re-read: a delete that claimed the row meanwhile
		// must not be painted over (design ptone/scion#2483 note F).
		s.publishAgentStatusFresh(ctx, agent)

		return &WakeResult{Outcome: WakeResumed}, nil

	case state.PhaseRunning:
		// Already running — no-op. Do not restart.
		return &WakeResult{Outcome: WakeAlreadyRunning}, nil

	case state.PhaseStopped:
		return nil, &AgentDMError{
			Code:       ErrCodeValidationError,
			Message:    "Agent is stopped, not suspended — use 'scion resume' to restart it with its previous state",
			HTTPStatus: http.StatusBadRequest,
		}

	case state.PhaseError:
		return nil, &AgentDMError{
			Code:       ErrCodeValidationError,
			Message:    "Agent is in error state — use 'scion resume' to restart",
			HTTPStatus: http.StatusBadRequest,
		}

	default:
		return nil, &AgentDMError{
			Code:       ErrCodeValidationError,
			Message:    fmt.Sprintf("Agent is not yet running (phase: %s) — wait for it to reach running state", agent.Phase),
			HTTPStatus: http.StatusBadRequest,
		}
	}
}

// validateGroupMemberDeliverable is validateAgentDeliverable for a group[]
// member. Group messages never wake agents, so a suspended member gets a
// reason that points the sender at a direct, waking message instead of the
// generic "use --wake" hint (which group sends do not accept).
func validateGroupMemberDeliverable(agent *store.Agent) *AgentDMError {
	err := validateAgentDeliverable(agent)
	if err != nil && state.Phase(agent.Phase) == state.PhaseSuspended {
		err.Message = fmt.Sprintf("agent %q is suspended; group messages do not wake agents — message it directly with --wake", agent.Slug)
	}
	return err
}

// validateAgentDeliverable checks that the target agent is in a state that
// can accept message delivery when wake was NOT requested. Returns nil when
// the agent is running, or a typed error describing why delivery is not
// possible.
func validateAgentDeliverable(agent *store.Agent) *AgentDMError {
	phase := state.Phase(agent.Phase)
	switch phase {
	case state.PhaseRunning:
		return nil
	case state.PhaseSuspended:
		return &AgentDMError{
			Code:       ErrCodeAgentNotRunning,
			Message:    fmt.Sprintf("agent %q is suspended; use --wake to resume and deliver", agent.Slug),
			HTTPStatus: http.StatusConflict,
		}
	case state.PhaseStopped:
		return &AgentDMError{
			Code:       ErrCodeAgentNotRunning,
			Message:    fmt.Sprintf("agent %q is stopped; use 'scion resume' to restart it with its previous state", agent.Slug),
			HTTPStatus: http.StatusConflict,
		}
	case state.PhaseError:
		return &AgentDMError{
			Code:       ErrCodeAgentNotRunning,
			Message:    fmt.Sprintf("agent %q is in error state; use 'scion resume' to restart", agent.Slug),
			HTTPStatus: http.StatusConflict,
		}
	default:
		return &AgentDMError{
			Code:       ErrCodeAgentNotRunning,
			Message:    fmt.Sprintf("agent %q is not yet running (phase: %s); wait for it to reach running state", agent.Slug, agent.Phase),
			HTTPStatus: http.StatusConflict,
		}
	}
}

// deferredReason is the note a deferred delivery reports: a migrating
// recipient, or another start of the recipient already in progress.
func deferredReason(agent *store.Agent) string {
	if reincarnationInFlight(agent) {
		return "agent is reincarnating"
	}
	if agent.StartClaimKind == store.StartClaimStop {
		return "a stop is in progress for the agent"
	}
	return "agent is already starting"
}
