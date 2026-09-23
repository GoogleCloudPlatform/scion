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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// maxHandoffBytes bounds the reincarnate request's handoff text (design §3.2).
const maxHandoffBytes = 256 * 1024

// ReincarnateAgentRequest is the request body for
// POST /api/v1/projects/{projectId}/agents/{agentIdOrSlug}/reincarnate and
// its ID-addressed twin POST /api/v1/agents/{agentId}/reincarnate (design
// §3.2). Phase 1 supports only Handoff and DryRun; every override field is
// accepted on the wire (so a Phase-3-aware CLI talking to a Phase-1 hub gets
// a clear 400) but rejected if set.
type ReincarnateAgentRequest struct {
	Handoff string `json:"handoff,omitempty"`
	DryRun  bool   `json:"dryRun,omitempty"`

	// Phase 3 overrides — not yet supported; any non-zero value here is a 400.
	Image          string            `json:"image,omitempty"`
	HarnessConfig  string            `json:"harnessConfig,omitempty"`
	HarnessAuth    string            `json:"harnessAuth,omitempty"`
	Model          string            `json:"model,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	TemplateHash   string            `json:"templateHash,omitempty"`
	ResetOverrides bool              `json:"resetOverrides,omitempty"`
	Rollback       bool              `json:"rollback,omitempty"`
}

// hasUnsupportedOverrides reports whether the request sets any Phase 3
// override field.
func (r ReincarnateAgentRequest) hasUnsupportedOverrides() bool {
	return r.Image != "" || r.HarnessConfig != "" || r.HarnessAuth != "" || r.Model != "" ||
		len(r.Env) > 0 || r.TemplateHash != "" || r.ResetOverrides || r.Rollback
}

// ReincarnateAgentResponse is the response body for a reincarnate request:
// 202 for a persisted (pending) reincarnation, or 200 for a dry run.
type ReincarnateAgentResponse struct {
	AgentID    string            `json:"agentId"`
	Generation int               `json:"generation"` // target generation
	State      string            `json:"state"`      // pending|planned (Phase 1)
	Plan       ReincarnationPlan `json:"plan"`
}

// FieldChange describes an old→new change to a single scalar field on the
// reincarnation plan.
type FieldChange struct {
	Old string `json:"old,omitempty"`
	New string `json:"new,omitempty"`
}

// KeyDiff describes an old→new change to a set of map keys (e.g. env var
// names), by name only — never by value, since env values may be secrets.
type KeyDiff struct {
	Added   []string `json:"added,omitempty"`
	Removed []string `json:"removed,omitempty"`
	Changed []string `json:"changed,omitempty"`
}

// ReincarnationPlan is the old→new diff returned by both a dry run and a real
// reincarnate request (design §3.2).
type ReincarnationPlan struct {
	Template   FieldChange `json:"template"`
	Image      FieldChange `json:"image"`
	HarnessCfg FieldChange `json:"harnessConfig"`
	Model      FieldChange `json:"model"`
	EnvKeys    KeyDiff     `json:"envKeys"`
	Branch     string      `json:"branch"`
	Warnings   []string    `json:"warnings,omitempty"`
}

// authorizeAgentReincarnate gates POST .../reincarnate for every caller kind
// (design §3.8, decision D2):
//   - A user needs ActionUpdate on the agent, the same policy as agent update.
//   - An agent reincarnating ANOTHER agent needs project:agent:lifecycle
//     within its own project, same as stop/start (authorizeAgentLifecycle).
//   - An agent reincarnating ITSELF is allowed for any role, with no scope
//     check. Phase 1 accepts no request overrides, so the "no override"
//     condition D2 attaches to the self exemption always holds; a Phase 3
//     override on a self-reincarnation will need its own, stricter check
//     (design §3.6a) added at that handler, not here.
func (s *Server) authorizeAgentReincarnate(w http.ResponseWriter, r *http.Request, agent *store.Agent) bool {
	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return false
	}
	resource := agentResource(agent)

	switch identity.Type() {
	case "agent":
		agentIdent, ok := identity.(AgentIdentity)
		if !ok {
			logAuthzDenial(r, identity, resource, ActionLifecycle, "invalid agent identity")
			writeForbidden(w, "")
			return false
		}
		if agentIdent.ID() == agent.ID {
			// Self-reincarnation: any role, no scope required (D2).
			return true
		}
		if !agentIdent.HasScope(ScopeAgentLifecycle) {
			logAuthzDenial(r, identity, resource, ActionLifecycle, "missing scope "+string(ScopeAgentLifecycle))
			writeForbidden(w, "Missing required scope: "+string(ScopeAgentLifecycle))
			return false
		}
		if agentIdent.ProjectID() != agent.ProjectID {
			logAuthzDenial(r, identity, resource, ActionLifecycle, "agent project mismatch")
			writeForbidden(w, "Agents can only manage agents within their own project")
			return false
		}
		return true

	case "user", "dev":
		userIdent, ok := identity.(UserIdentity)
		if !ok {
			logAuthzDenial(r, identity, resource, ActionUpdate, "invalid user identity")
			writeForbidden(w, "")
			return false
		}
		decision := s.authzService.CheckAccess(ctx, userIdent, resource, ActionUpdate)
		if !decision.Allowed {
			logAuthzDenial(r, identity, resource, ActionUpdate, decision.Reason)
			writeForbidden(w, "")
			return false
		}
		return true

	default:
		logAuthzDenial(r, identity, resource, ActionUpdate, "identity type may not reincarnate agents")
		writeForbidden(w, "")
		return false
	}
}

// handleReincarnateAgent implements POST .../agents/{id}/reincarnate (design
// §3.1, §3.2) for both the ID-addressed and project-scoped routes, which
// resolve id/slug to an agent.ID before calling this. It validates, checks
// broker capability, computes the reincarnation plan, and — for a non-dry-run
// request — persists a pending AgentReincarnation record and returns 202
// before any teardown, then completes the migration in a detached background
// worker (see runReincarnationWorker).
func (s *Server) handleReincarnateAgent(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	agent, err := s.store.GetAgent(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	if !s.authorizeAgentReincarnate(w, r, agent) {
		return
	}

	var req ReincarnateAgentRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}
	if len(req.Handoff) > maxHandoffBytes {
		ValidationError(w, fmt.Sprintf("handoff exceeds %d bytes", maxHandoffBytes), nil)
		return
	}
	if req.hasUnsupportedOverrides() {
		writeError(w, http.StatusBadRequest, ErrCodeValidationError,
			"config overrides are not yet supported for scion reincarnate", nil)
		return
	}

	// Amendment A2 / p1b-r1 R4: Phase 1 targets clone-per-agent workspaces
	// only (design §7). GitClone is set exactly for that mode
	// (populateAgentConfig) — nil covers both worktree-per-agent and
	// shared-workspace. Checked here, before any plan is computed or
	// anything persisted, as defense in depth alongside the broker's own
	// refusal (Reprovision refuses to touch a non-clone workspace): this is
	// what makes --dry-run report the restriction too, instead of a dry run
	// showing a plan that a real request could not safely execute.
	if agent.AppliedConfig == nil || agent.AppliedConfig.GitClone == nil {
		writeError(w, http.StatusBadRequest, ErrCodeValidationError,
			"reincarnate currently supports clone-per-agent workspaces only", nil)
		return
	}

	dispatcher := s.GetDispatcher()
	if dispatcher == nil {
		writeError(w, http.StatusBadRequest, ErrCodeValidationError,
			"agent reincarnation requires hub mode with a runtime broker dispatcher", nil)
		return
	}
	if agent.RuntimeBrokerID == "" {
		writeError(w, http.StatusBadRequest, ErrCodeValidationError,
			"agent has no runtime broker assigned", nil)
		return
	}
	if !s.checkBrokerAvailability(w, r, agent) {
		return
	}
	broker, err := s.store.GetRuntimeBroker(ctx, agent.RuntimeBrokerID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if broker.Capabilities == nil || !broker.Capabilities.Reprovision {
		// AC-9: an old broker without the reprovision capability gets 412,
		// and the agent is left completely untouched (checked before any
		// plan is computed or anything is persisted).
		writeError(w, http.StatusPreconditionFailed, ErrCodeUnsupportedCapability,
			"runtime broker does not support agent reincarnation; upgrade the broker", nil)
		return
	}

	project, err := s.store.GetProject(ctx, agent.ProjectID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// AC-2's "dry-run changed nothing" and the real path's plan are computed
	// by the exact same call — buildFreshAppliedConfig only reads from the
	// store (templates, harness configs, pre-start hooks, skills settings),
	// it never writes. Any store writes happen only below this point, and
	// only for a non-dry-run request.
	fresh, warnings, err := s.buildFreshAppliedConfig(ctx, agent, project)
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"failed to resolve new configuration: "+err.Error(), nil)
		return
	}
	plan := computeReincarnationPlan(agent.AppliedConfig, fresh, warnings)
	targetGeneration := agent.Generation + 1

	if req.DryRun {
		writeJSON(w, http.StatusOK, ReincarnateAgentResponse{
			AgentID:    agent.ID,
			Generation: targetGeneration,
			State:      "planned",
			Plan:       plan,
		})
		return
	}

	// AC-8 / p1b-r1 R1: claim the agent BEFORE creating the reincarnation
	// record, guarded by the agent row's own optimistic lock (state_version).
	// The previous order — create the record, then a guarded UpdateAgent —
	// was check-then-act: a version conflict (or any error) on that second
	// write left the just-created record stuck in "pending" forever, with no
	// worker running for it and no API to clear it, wedging every later
	// request behind a permanent 409. Claiming first means a conflict here
	// happens before anything else is written, so there is nothing to leave
	// behind: the request simply fails, unclaimed.
	if agent.ReincarnationState != store.ReincarnationStateNone && agent.ReincarnationState != store.ReincarnationStateFailed {
		Conflict(w, "a reincarnation is already pending for this agent")
		return
	}

	previousReincarnationState := agent.ReincarnationState
	agent.ReincarnationState = store.ReincarnationStatePending
	if err := s.store.UpdateAgent(ctx, agent); err != nil {
		if errors.Is(err, store.ErrVersionConflict) {
			Conflict(w, "agent was concurrently modified; retry")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	requestedBy := ""
	if identity := GetIdentityFromContext(ctx); identity != nil {
		requestedBy = identity.ID()
	}

	rec := &store.AgentReincarnation{
		AgentID:               agent.ID,
		FromGeneration:        agent.Generation,
		ToGeneration:          targetGeneration,
		RequestedBy:           requestedBy,
		State:                 store.AgentReincarnationStatePending,
		PreviousAppliedConfig: agent.AppliedConfig,
		Handoff:               req.Handoff,
	}
	if err := s.store.CreateAgentReincarnation(ctx, rec); err != nil {
		// The claim above already landed. Revert it so this failure does not
		// wedge the agent behind a permanent 409 with no record to show for
		// it. Best effort: if the revert itself fails, log loudly — an
		// operator can clear agents.reincarnation_state by hand, which is a
		// far smaller recovery than an unrecoverable stuck claim.
		agent.ReincarnationState = previousReincarnationState
		if revertErr := s.store.UpdateAgent(ctx, agent); revertErr != nil {
			s.agentLifecycleLog.Error("handleReincarnateAgent: failed to revert claimed reincarnation_state after record creation failure",
				"agent_id", agent.ID, "revert_error", revertErr, "original_error", err)
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// Detached background worker (design §3.1, §3.7): the request returns
	// 202 before any teardown starts, and the worker runs on a context
	// independent of this request's. A self-migration stops the calling
	// container mid-flight, which would cancel r.Context() and abort the
	// worker if it inherited it — exactly the self-deletion
	// context-cancellation hazard §3.0 identifies for the rejected design.
	go s.runReincarnationWorker(context.Background(), agent.ID, rec.ID, fresh, req.Handoff)

	writeJSON(w, http.StatusAccepted, ReincarnateAgentResponse{
		AgentID:    agent.ID,
		Generation: targetGeneration,
		State:      store.AgentReincarnationStatePending,
		Plan:       plan,
	})
}
