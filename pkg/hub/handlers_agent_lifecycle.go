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
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func (s *Server) updateAgentStatus(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)

	// If identity is an agent, verify it's the same agent and has the correct scope
	if agentIdent, ok := identity.(AgentIdentity); ok {
		if agentIdent.ID() != id {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "Agents can only update their own status", nil)
			return
		}
		if !agentIdent.HasScope(ScopeAgentStatusUpdate) {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "Missing required scope: agent:status:update", nil)
			return
		}
	} else if identity == nil {
		Unauthorized(w)
		return
	}

	var status store.AgentStatusUpdate
	if err := readJSON(r, &status); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	// Guard against phase regressions and auto-correct phase from activity.
	if status.Phase != "" || status.Activity != "" {
		agent, err := s.store.GetAgent(ctx, id)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		guardAgentPhaseTransition(agent, &status)
	}

	if err := s.store.UpdateAgentStatus(ctx, id, status); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Publish status event (best-effort: fetch agent for ProjectID)
	if agent, err := s.store.GetAgent(ctx, id); err == nil {
		s.events.PublishAgentStatus(ctx, agent)
	} else {
		s.agentLifecycleLog.Warn("Failed to fetch agent for status event", "agent_id", id, "error", err)
	}

	w.WriteHeader(http.StatusOK)
}

// guardAgentPhaseTransition applies two guards to a status update:
//
//  1. Phase regression guard: rejects transitions that would move an agent
//     backward in its forward-progress lifecycle (e.g. running → starting).
//  2. Activity-driven phase auto-correction: when an activity that implies the
//     agent is running arrives but the phase is pre-running, auto-promotes the
//     phase to running.
func guardAgentPhaseTransition(agent *store.Agent, status *store.AgentStatusUpdate) {
	currentPhase := state.Phase(agent.Phase)

	// Guard 0: suspended is sticky against async status updates. When an agent
	// is suspended, its container is being torn down, and the dying container's
	// async sciontool /status POST (e.g. phase=stopped, activity=crashed) must
	// not clobber the suspended phase — otherwise a subsequent /start would not
	// see suspended and would skip the harness --continue (resume) flag.
	// Only explicit start/stop lifecycle actions may leave the suspended phase,
	// and those write phase directly without going through this guard.
	if currentPhase == state.PhaseSuspended {
		status.Phase = ""
		status.Activity = ""
		return
	}

	// Guard 1: reject phase regressions within the forward-progress lifecycle.
	if status.Phase != "" {
		newPhase := state.Phase(status.Phase)
		if currentPhase.IsActivePhase() && newPhase.IsActivePhase() &&
			newPhase.Ordinal() < currentPhase.Ordinal() {
			status.Phase = ""
		}
	}

	// Guard 2: if an activity that implies the agent is running arrives
	// without an explicit phase, and the current phase is pre-running,
	// auto-correct the phase to running.
	if status.Activity != "" && status.Phase == "" {
		activity := state.Activity(status.Activity)
		if activity.ImpliesRunning() && currentPhase.IsActivePhase() &&
			currentPhase != state.PhaseRunning {
			status.Phase = string(state.PhaseRunning)
		}
	}
}

// errHarnessNoResume is returned by suspendAgent when the agent's harness does
// not support session resume, so suspending would strand it. The wrapped reason
// carries harness-supplied context for the caller's error message.
type errHarnessNoResume struct {
	reason string
}

func (e *errHarnessNoResume) Error() string {
	if e.reason != "" {
		return e.reason
	}
	return "harness does not support session resume"
}

// harnessSupportsResume reports whether the agent's configured harness supports
// resuming a session. An empty harness name (no applied config) is treated as
// supported, matching the HTTP suspend handler's prior behavior of only
// rejecting when a harness was explicitly resolved and declared SupportNo.
func (s *Server) harnessSupportsResume(agent *store.Agent) (bool, string) {
	harnessName := ""
	if agent.AppliedConfig != nil {
		harnessName = agent.AppliedConfig.HarnessConfig
	}
	if harnessName == "" {
		return true, ""
	}
	caps := harness.New(harnessName).AdvancedCapabilities()
	if caps.Resume.Support == api.SupportNo {
		return false, caps.Resume.Reason
	}
	return true, ""
}

// suspendAgent performs the core SUSPEND action shared by the HTTP lifecycle
// handler and the auto-suspend scheduler: it validates harness resume support,
// syncs the workspace on stop, dispatches the container stop to the runtime
// broker, persists phase=suspended (container_status=stopped, activity cleared),
// and publishes the resulting status event. It returns *errHarnessNoResume when
// the harness cannot resume so callers can decline to suspend.
func (s *Server) suspendAgent(ctx context.Context, agent *store.Agent) error {
	if ok, reason := s.harnessSupportsResume(agent); !ok {
		return &errHarnessNoResume{reason: reason}
	}

	dispatcher := s.GetDispatcher()
	if dispatcher != nil && agent.RuntimeBrokerID != "" {
		s.syncWorkspaceOnStop(ctx, agent)
		if err := dispatcher.DispatchAgentStop(ctx, agent); err != nil {
			return err
		}
	}

	newPhase := string(state.PhaseSuspended)
	if err := s.store.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{
		Phase:           newPhase,
		ContainerStatus: "stopped",
		Activity:        "",
	}); err != nil {
		return err
	}

	agent.Phase = newPhase
	agent.ContainerStatus = "stopped"
	agent.Activity = ""
	s.events.PublishAgentStatus(ctx, agent)
	return nil
}

func (s *Server) handleAgentLifecycle(w http.ResponseWriter, r *http.Request, id, action string) {
	ctx := r.Context()

	agent, err := s.store.GetAgent(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	if !s.checkBrokerAvailability(w, r, agent) {
		return
	}

	var newPhase string
	var dispatchErr error

	// If a dispatcher is available, dispatch the operation to the runtime broker
	dispatcher := s.GetDispatcher()

	switch action {
	case api.AgentActionStart:
		newPhase = string(state.PhaseRunning)
		if dispatcher != nil && agent.RuntimeBrokerID != "" {
			// Resume the harness session only when the agent was suspended.
			resume := agent.Phase == string(state.PhaseSuspended)
			dispatchErr = dispatcher.DispatchAgentStart(ctx, agent, "", resume)
			// DispatchAgentStart applies the broker response in-place;
			// use the broker-reported phase if it was set.
			if dispatchErr == nil && agent.Phase != "" {
				newPhase = agent.Phase
			}
		}
	case api.AgentActionStop:
		newPhase = string(state.PhaseStopped)
		if dispatcher != nil && agent.RuntimeBrokerID != "" {
			// Before stopping, sync workspace back for hub-managed projects on remote brokers.
			// This is best-effort: failures are logged but don't block the stop.
			s.syncWorkspaceOnStop(ctx, agent)
			dispatchErr = dispatcher.DispatchAgentStop(ctx, agent)
		}
	case api.AgentActionSuspend:
		// Only running agents can be suspended via the HTTP lifecycle handler.
		// (The auto-suspend scheduler calls suspendAgent directly and already
		// restricts itself to running+stalled agents.)
		if agent.Phase != string(state.PhaseRunning) {
			writeError(w, http.StatusBadRequest, ErrCodeValidationError,
				fmt.Sprintf("Cannot suspend agent in phase %q. Only running agents can be suspended.", agent.Phase), nil)
			return
		}
		// Suspend is fully handled by the shared suspendAgent helper, which
		// validates harness resume support, dispatches the stop, persists
		// phase=suspended, and publishes the status event.
		if err := s.suspendAgent(ctx, agent); err != nil {
			var noResume *errHarnessNoResume
			if errors.As(err, &noResume) {
				writeError(w, http.StatusBadRequest, ErrCodeValidationError,
					fmt.Sprintf("Cannot suspend agent: %s. Use 'stop' instead.", noResume.Error()), nil)
				return
			}
			RuntimeError(w, "Failed to dispatch to runtime broker: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, agent)
		return
	case api.AgentActionRestart:
		newPhase = string(state.PhaseRunning)
		if dispatcher != nil && agent.RuntimeBrokerID != "" {
			// Restart is implemented as stop + start so that env vars
			// (API keys, secrets) are re-resolved from Hub storage.
			// Stop errors are tolerated: the container may already be
			// exited and some runtimes (podman) return non-standard
			// errors for stopping non-running containers. The subsequent
			// Start will handle cleanup of the exited container.
			if stopErr := dispatcher.DispatchAgentStop(ctx, agent); stopErr != nil {
				slog.Warn("Restart: stop dispatch failed, proceeding with start",
					"agent_id", id, "error", stopErr)
			}
			// Restart is stop + start: a fresh harness session, not a resume.
			dispatchErr = dispatcher.DispatchAgentStart(ctx, agent, "", false)
			// DispatchAgentStart applies the broker response in-place;
			// use the broker-reported phase if it was set.
			if dispatchErr == nil && agent.Phase != "" {
				newPhase = agent.Phase
			}
		}
	}

	// If dispatch failed, return error
	if dispatchErr != nil {
		RuntimeError(w, "Failed to dispatch to runtime broker: "+dispatchErr.Error())
		return
	}

	statusUpdate := store.AgentStatusUpdate{
		Phase: newPhase,
	}
	// When stopping, also update container status so the hub immediately
	// reflects the stopped state without waiting for the next heartbeat.
	// (Suspend is handled earlier via suspendAgent and returns before here.)
	if action == api.AgentActionStop {
		statusUpdate.ContainerStatus = "stopped"
		statusUpdate.Activity = ""
	}
	// When starting or restarting, propagate container status from broker response
	if (action == api.AgentActionStart || action == api.AgentActionRestart) && agent.ContainerStatus != "" {
		statusUpdate.ContainerStatus = agent.ContainerStatus
	}
	if err := s.store.UpdateAgentStatus(ctx, id, statusUpdate); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	agent.Phase = newPhase
	s.events.PublishAgentStatus(ctx, agent)

	writeJSON(w, http.StatusOK, agent)
}

// stopAllResult represents the outcome of stopping a single agent.
type stopAllResult struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// StopAllAgentsResponse is the response from the stop-all endpoint.
type StopAllAgentsResponse struct {
	Stopped int             `json:"stopped"`
	Failed  int             `json:"failed"`
	Total   int             `json:"total"`
	Scope   string          `json:"scope,omitempty"` // "all" or "own"
	Results []stopAllResult `json:"results"`
}

// handleStopAllAgents stops all running agents, optionally scoped to a project.
// Global (projectID=="") requires platform admin. Project-scoped allows any project
// member: owners/admins stop all agents, regular members stop only their own.
func (s *Server) handleStopAllAgents(w http.ResponseWriter, r *http.Request, projectID string) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	ctx := r.Context()

	userIdent := GetUserIdentityFromContext(ctx)
	if userIdent == nil {
		writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
			"Authentication required", nil)
		return
	}

	// Determine authorization and scope
	scope := "all"
	filter := store.AgentFilter{
		ProjectID: projectID,
		Phase:     string(state.PhaseRunning),
	}

	if projectID == "" {
		// Global stop-all: platform admin only
		if userIdent.Role() != "admin" {
			writeError(w, http.StatusForbidden, ErrCodeForbidden,
				"Only admins can stop all agents", nil)
			return
		}
	} else {
		// Project-scoped stop-all: any project member allowed
		isAdmin := userIdent.Role() == "admin"
		if !isAdmin {
			projectRole := s.resolveUserProjectRole(ctx, projectID, userIdent.ID())
			if projectRole == "" {
				writeError(w, http.StatusForbidden, ErrCodeForbidden,
					"You are not a member of this project", nil)
				return
			}
			// Regular members can only stop their own agents
			if projectRole != store.GroupMemberRoleOwner && projectRole != store.GroupMemberRoleAdmin {
				filter.OwnerID = userIdent.ID()
				scope = "own"
			}
		}
	}

	result, err := s.store.ListAgents(ctx, filter, store.ListOptions{
		Limit: 1000, // reasonable upper bound
	})
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	agents := result.Items
	if len(agents) == 0 {
		writeJSON(w, http.StatusOK, StopAllAgentsResponse{
			Scope:   scope,
			Results: []stopAllResult{},
		})
		return
	}

	dispatcher := s.GetDispatcher()

	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		results = make([]stopAllResult, 0, len(agents))
	)

	for i := range agents {
		agent := &agents[i]
		wg.Add(1)
		go func(agent *store.Agent) {
			defer wg.Done()

			res := stopAllResult{
				ID:   agent.ID,
				Name: agent.Name,
			}

			// Dispatch stop to broker
			var dispatchErr error
			if dispatcher != nil && agent.RuntimeBrokerID != "" {
				opCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
				defer cancel()
				s.syncWorkspaceOnStop(opCtx, agent)
				dispatchErr = dispatcher.DispatchAgentStop(opCtx, agent)
			}

			if dispatchErr != nil {
				res.Status = "error"
				res.Error = dispatchErr.Error()
				s.agentLifecycleLog.Warn("stop-all: failed to stop agent",
					"agent_id", agent.ID, "error", dispatchErr)
			} else {
				// Update agent status in store
				statusUpdate := store.AgentStatusUpdate{
					Phase:           string(state.PhaseStopped),
					ContainerStatus: "stopped",
					Activity:        "",
				}
				if updateErr := s.store.UpdateAgentStatus(ctx, agent.ID, statusUpdate); updateErr != nil {
					res.Status = "error"
					res.Error = updateErr.Error()
				} else {
					res.Status = "stopped"
					agent.Phase = string(state.PhaseStopped)
					s.events.PublishAgentStatus(ctx, agent)
				}
			}

			mu.Lock()
			results = append(results, res)
			mu.Unlock()
		}(agent)
	}

	wg.Wait()

	stopped := 0
	failed := 0
	for _, r := range results {
		if r.Status == "stopped" {
			stopped++
		} else {
			failed++
		}
	}

	writeJSON(w, http.StatusOK, StopAllAgentsResponse{
		Stopped: stopped,
		Failed:  failed,
		Total:   len(results),
		Scope:   scope,
		Results: results,
	})
}

// resolveUserProjectRole returns the user's role in the project's members group.
// Returns "" if the user is not a member of the project.
func (s *Server) resolveUserProjectRole(ctx context.Context, projectID, userID string) string {
	groups, err := s.store.ListGroups(ctx, store.GroupFilter{
		ProjectID: projectID,
		GroupType: store.GroupTypeExplicit,
	}, store.ListOptions{Limit: 10})
	if err != nil || len(groups.Items) == 0 {
		return ""
	}

	for _, g := range groups.Items {
		membership, err := s.store.GetGroupMembership(ctx, g.ID, store.GroupMemberTypeUser, userID)
		if err != nil {
			continue
		}
		return membership.Role
	}
	return ""
}
