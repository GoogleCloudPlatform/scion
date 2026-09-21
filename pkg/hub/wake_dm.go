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

	switch phase {
	case state.PhaseSuspended:
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

		// Resume the suspended agent. continue=true tells the harness to
		// restore its prior session rather than starting fresh.
		if err := dispatcher.DispatchAgentStart(ctx, agent, "", true); err != nil {
			return nil, &AgentDMError{
				Code:       ErrCodeRuntimeError,
				Message:    "Failed to wake agent: " + err.Error(),
				HTTPStatus: http.StatusBadGateway,
			}
		}

		// Transition to 'starting' while waiting for readiness.
		statusUpdate := store.AgentStatusUpdate{Phase: string(state.PhaseStarting)}
		if err := s.store.UpdateAgentStatus(ctx, agent.ID, statusUpdate); err != nil {
			s.messageLog.Error("wake: failed to update agent phase to starting",
				"agent_id", agent.ID, "error", err)
			return nil, &AgentDMError{
				Code:       ErrCodeInternalError,
				Message:    "failed to update agent status after resume",
				HTTPStatus: http.StatusInternalServerError,
			}
		}
		agent.Phase = string(state.PhaseStarting)
		s.events.PublishAgentStatus(ctx, agent)

		// Wait for the agent to report its first activity (readiness signal).
		if err := s.waitForAgentReady(ctx, agent.ID, 30*time.Second); err != nil {
			// On readiness failure, mark the agent as errored for visibility.
			_ = s.store.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{
				Phase:   string(state.PhaseError),
				Message: "Failed to become ready after wake",
			})
			return nil, &AgentDMError{
				Code:       ErrCodeRuntimeError,
				Message:    "Agent resumed but did not become ready: " + err.Error(),
				HTTPStatus: http.StatusBadGateway,
			}
		}

		// Agent is ready — transition to 'running'.
		statusUpdate = store.AgentStatusUpdate{Phase: string(state.PhaseRunning)}
		if err := s.store.UpdateAgentStatus(ctx, agent.ID, statusUpdate); err != nil {
			s.messageLog.Error("wake: failed to update agent phase to running",
				"agent_id", agent.ID, "error", err)
			return nil, &AgentDMError{
				Code:       ErrCodeInternalError,
				Message:    "failed to update agent status after readiness",
				HTTPStatus: http.StatusInternalServerError,
			}
		}
		agent.Phase = string(state.PhaseRunning)
		s.events.PublishAgentStatus(ctx, agent)

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
