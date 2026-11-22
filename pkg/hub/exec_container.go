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

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ExecContainerState is the resolved state of a user's registered execution
// container (design §4.4, §5.1).
type ExecContainerState string

const (
	ExecContainerStateUnconfigured ExecContainerState = "unconfigured"
	ExecContainerStateReady        ExecContainerState = "ready"
	ExecContainerStateStarting     ExecContainerState = "starting"
	ExecContainerStateStopped      ExecContainerState = "stopped"
	ExecContainerStateSuspended    ExecContainerState = "suspended"
	ExecContainerStateUnavailable  ExecContainerState = "unavailable"
)

// ExecContainerResolution is the typed result returned by ResolveExecContainer
// (design §5.1). Agent is nil unless State is one of ready, starting, stopped,
// or suspended.
type ExecContainerResolution struct {
	State ExecContainerState
	Agent *store.Agent
	RunID string
}

// ResolveExecContainer resolves userID's registered execution container
// (design §5.1). It re-runs the full eligibility check (§4.3 step 3) on every
// call as userID acting as itself and never returns an Agent the user is not
// currently eligible to use.
//
//   - When hub.exec_containers is off, returns unconfigured.
//   - When no row exists, returns unconfigured.
//   - When any eligibility check fails or authz is indeterminate, returns
//     unavailable (fail closed) with Agent == nil.
func (s *Server) ResolveExecContainer(ctx context.Context, userID string) (ExecContainerResolution, error) {
	if !s.experimentEnabled(experiments.ExecContainers) {
		return ExecContainerResolution{State: ExecContainerStateUnconfigured}, nil
	}

	row, err := s.store.GetUserExecContainer(ctx, userID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ExecContainerResolution{State: ExecContainerStateUnconfigured}, nil
		}
		return ExecContainerResolution{}, err
	}

	user, err := s.store.GetUser(ctx, userID)
	if err != nil {
		// Missing user or lookup fault: fail closed to unavailable.
		return ExecContainerResolution{State: ExecContainerStateUnavailable}, nil
	}
	ident := NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, "internal")
	return s.resolveExecContainerRow(ctx, ident, row.AgentID)
}

// resolveExecContainerForIdentity resolves the caller's registered execution
// container using an already-authenticated human UserIdentity.
func (s *Server) resolveExecContainerForIdentity(ctx context.Context, actor UserIdentity) (ExecContainerResolution, error) {
	if !s.experimentEnabled(experiments.ExecContainers) {
		return ExecContainerResolution{State: ExecContainerStateUnconfigured}, nil
	}

	row, err := s.store.GetUserExecContainer(ctx, actor.ID())
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ExecContainerResolution{State: ExecContainerStateUnconfigured}, nil
		}
		return ExecContainerResolution{}, err
	}

	return s.resolveExecContainerRow(ctx, actor, row.AgentID)
}

func (s *Server) resolveExecContainerRow(ctx context.Context, actor UserIdentity, agentID string) (ExecContainerResolution, error) {
	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ExecContainerResolution{State: ExecContainerStateUnavailable}, nil
		}
		return ExecContainerResolution{}, err
	}

	if !s.isExecContainerEligible(ctx, actor, agent) {
		return ExecContainerResolution{State: ExecContainerStateUnavailable}, nil
	}

	return ExecContainerResolution{
		State: mapExecContainerState(agent),
		Agent: agent,
		RunID: agent.RunID,
	}, nil
}

// isExecContainerEligible evaluates the single eligibility predicate from
// design §4.3 step 3:
//   - agent exists and is not deleted, AND
//   - isShellAgent(agent), AND
//   - authorizeAgentTargetAction(ctx, actor, agent, ActionAttach) == nil.
//
// When agent != nil, both isShellAgent and authorizeAgentTargetAction are
// evaluated without short-circuiting so there is no timing branch between
// ineligible agent kinds. An indeterminate authz decision returns a non-nil
// denial and therefore evaluates to false (fail closed).
func (s *Server) isExecContainerEligible(ctx context.Context, actor UserIdentity, agent *store.Agent) bool {
	if agent == nil || !agent.DeletedAt.IsZero() {
		return false
	}
	shellOK := isShellAgent(agent)
	attachOK := s.authorizeAgentTargetAction(ctx, actor, agent, ActionAttach) == nil
	return shellOK && attachOK
}

// mapExecContainerState maps an eligible shell agent's lifecycle state to an
// ExecContainerState per design §4.4.
func mapExecContainerState(agent *store.Agent) ExecContainerState {
	phase := state.Phase(agent.Phase)
	activity := state.Activity(agent.Activity)

	if agent.IdleSuspendClaimActive || phase == state.PhaseSuspended {
		return ExecContainerStateSuspended
	}
	switch phase {
	case state.PhaseProvisioning, state.PhaseStarting:
		return ExecContainerStateStarting
	case state.PhaseRunning:
		if activity == state.ActivityInitializing {
			return ExecContainerStateStarting
		}
		return ExecContainerStateReady
	default:
		return ExecContainerStateStopped
	}
}
