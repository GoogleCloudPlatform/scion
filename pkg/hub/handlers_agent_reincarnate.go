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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
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
	// TargetBroker (a broker ID, name or slug) asks to move the agent to
	// that broker; it must mount the same NFS export as the current one.
	// Empty, or the agent's current broker, is a plain reincarnation. Only
	// a dry run is carried out for a different broker; a real move returns
	// 501.
	TargetBroker string `json:"targetBroker,omitempty"`

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
	// SourceBrokerID and TargetBrokerID are set when the request named a
	// target broker. They are equal for a plain reincarnation.
	SourceBrokerID string `json:"sourceBrokerId,omitempty"`
	TargetBrokerID string `json:"targetBrokerId,omitempty"`
	// MoveVerdict is the move eligibility verdict of a dry-run move.
	MoveVerdict *MoveVerdict `json:"moveVerdict,omitempty"`
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
//   - A user needs the agent lifecycle permission (ActionLifecycle), the same
//     policy as stop/start/restart (design §3.8 Amendment A24). ActionUpdate
//     (agent.update) has no UATScope in the permission registry, so the UAT
//     project-constraint gate denied every User Access Token outright,
//     including one scoped to agent:lifecycle or agent:manage -- only
//     session/OAuth users and agent tokens could ever reincarnate. The
//     built-in roles grant agent.update and agent.lifecycle together, so
//     this does not widen access for any existing role.
//   - An agent reincarnating ANOTHER agent needs project:agent:lifecycle
//     within its own project and agent.lifecycle on the target, same as
//     stop/start (authorizeAgentLifecycle).
//   - An agent reincarnating ITSELF is allowed for any role, with no scope
//     check. Phase 1 accepts no request overrides, so the "no override"
//     condition D2 attaches to the self exemption always holds; a Phase 3
//     override on a self-reincarnation will need its own, stricter check
//     (design §3.6a) added at that handler, not here.
func (s *Server) authorizeAgentReincarnate(w http.ResponseWriter, r *http.Request, agent *store.Agent) bool {
	identity := GetIdentityFromContext(r.Context())
	if identity != nil && identity.Type() == "agent" {
		if agentIdent, ok := identity.(AgentIdentity); ok && agent != nil && agentIdent.ID() == agent.ID {
			// Self-reincarnation: any role, no scope required (D2).
			return true
		}
	}
	return s.authorizeAgentLifecycle(w, r, agent, ActionLifecycle)
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

	// Start gate (design ptone/scion#2483 §2.1): a delete in progress, an
	// incomplete create or an in-flight launch is refused, the last with
	// 409 agent_launching, since the worker would stop and reprovision a
	// launching agent.
	if ref := s.startGate(ctx, agent, startEntryReincarnate); ref.refuses() {
		ref.write(w)
		return
	}
	// The delete claim the gate admitted; the worker pins the failed-marker
	// clear just before its completion write to it (see
	// clearFailedDeletion), so a delete that claims after this point keeps
	// its marker.
	admittedDeletionClaim := agent.DeletionClaim

	// Authority re-record: a requester other than the agent itself becomes
	// the agent's recorded delegator, so it must pass CanDelegate and its
	// ceiling must cover the stored role. Checked right after the start
	// gate, before the request body, the broker and the agent state are
	// examined and before anything is written, so a refused request claims
	// nothing and a dry run reports the same refusal.
	auth, ok := s.reincarnateAuthorityFor(w, r, agent)
	if !ok {
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

	project, err := s.store.GetProject(ctx, agent.ProjectID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// A target broker other than the agent's current one is a move. The
	// target is resolved without writing anything (no provider link).
	var moveTarget *store.RuntimeBroker
	targetBrokerID := ""
	if req.TargetBroker != "" {
		// A move needs a source broker; refuse before resolving the target.
		if agent.RuntimeBrokerID == "" {
			writeError(w, http.StatusBadRequest, ErrCodeValidationError,
				"cannot move an agent that is not currently assigned to a broker", nil)
			return
		}
		dst, ambiguous, err := s.resolveMoveTargetBroker(ctx, req.TargetBroker, agent.RuntimeBrokerID, project.ID)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		if len(ambiguous) > 0 {
			writeMoveTargetAmbiguous(w, req.TargetBroker, ambiguous)
			return
		}
		if dst == nil {
			s.writeMoveTargetNotFound(ctx, w, req.TargetBroker, project)
			return
		}
		targetBrokerID = dst.ID
		if dst.ID != agent.RuntimeBrokerID {
			moveTarget = dst
		}
	}
	if moveTarget != nil && !req.DryRun {
		writeError(w, http.StatusNotImplemented, ErrCodeNotImplemented,
			"moving an agent to another broker is not yet implemented; use --dry-run to check eligibility", nil)
		return
	}

	// Design §3.4 Amendments A2/A4/A23/A23.1/A23.2: eligible workspaces are
	// clone-per-agent (a real GitClone, on a project that is neither
	// worktree-per-agent nor shared — A23.1 R3: a project can be switched to
	// shared after an agent was created as clone-per-agent, via a project
	// label update, so this must be re-checked here rather than assumed from
	// how populateAgentConfig behaves today) or an explicit mount (no
	// GitClone, but a non-empty EFFECTIVE Workspace — shared-workspace and
	// hub-managed projects), via the shared api.ReincarnateEligible
	// predicate. GitClone alone does NOT identify clone-per-agent mode:
	// populateAgentConfig (handlers_agent_create_helpers.go) sets GitClone
	// for every git-remote project that is not shared-workspace, and that
	// includes worktree-per-agent — so a worktree-per-agent agent has a
	// non-nil GitClone and would otherwise pass the predicate. It is
	// excluded here, separately, because only the Hub has the project
	// record the predicate itself cannot see (api.ReincarnateEligible's doc
	// comment).
	//
	// "Effective" Workspace (A23.1 R1): linkedProjectPath resolves a
	// registered ProjectProvider.LocalPath for the agent's broker, exactly
	// as the dispatcher's resolveDispatchProjectInfo does, and
	// effectiveDispatchWorkspace applies buildCreateRequest's own "a linked
	// local provider clears an absolute Workspace" rule. Gating on the raw
	// AppliedConfig.Workspace let a linked shared project look eligible
	// here, then get stopped and refused by the broker with a 409 — a
	// regression from Phase 1's clean up-front 400. Evaluating the same
	// effective value the dispatcher will actually send keeps the two from
	// disagreeing.
	//
	// A23.2 FYI-b: the mirror of R3, for the reverse switch. A project can
	// also switch TO clone-per-agent after an agent was created as
	// shared-workspace or hub-managed: the agent's stored GitClone is still
	// nil, but a fresh reincarnation would derive a GitClone from the
	// project's CURRENT mode — and this agent has no existing real clone on
	// disk for the broker's GitClone branch to find, so (without this
	// check) it would be stopped and then refused by the broker's own "no
	// existing git clone" 409, the same stop-then-refuse shape R1 fixed for
	// the other direction. The linked-provider case is excluded here
	// (linkedProjectPath == "") because when a provider IS linked,
	// effectiveDispatchWorkspace already clears the workspace to "" and the
	// generic ReincarnateEligible check below catches it as "neither".
	//
	// Checked before any plan is computed or anything persisted, as defense
	// in depth alongside the broker's own refusal (Reprovision refuses to
	// touch a workspace it did not find already on disk): this is what
	// makes --dry-run report the restriction too, instead of a dry run
	// showing a plan that a real request could not safely execute.
	// Empty-per-agent workspaces are broker-local, unsynced state: the only
	// possible reincarnation would be a fresh empty directory, silently
	// discarding work. Refused explicitly in v1 (design #2703 D4).
	workspaceModeErr := ""
	if project.IsEmptyPerAgent() {
		workspaceModeErr = `reincarnate does not yet support "Empty directory per agent" (empty-per-agent) workspaces`
	}

	hasGitClone := agent.AppliedConfig != nil && agent.AppliedConfig.GitClone != nil
	var effectiveWorkspace string
	var linkedProjectPath string
	if agent.AppliedConfig != nil {
		linkedProjectPath = s.linkedProjectPath(ctx, agent)
		effectiveWorkspace = effectiveDispatchWorkspace(agent.AppliedConfig.Workspace, linkedProjectPath)
	}
	switchedToCloneOnly := !hasGitClone && project.GitRemote != "" && !project.IsSharedWorkspace() &&
		linkedProjectPath == "" && effectiveWorkspace != ""
	if workspaceModeErr == "" && (agent.AppliedConfig == nil || project.IsWorktreePerAgent() ||
		(hasGitClone && project.IsSharedWorkspace()) ||
		switchedToCloneOnly ||
		!api.ReincarnateEligible(hasGitClone, effectiveWorkspace)) {
		// FYI-6 (review p1b-r1): the generic message now covers every
		// eligible mode, not just clone-per-agent.
		workspaceModeErr = "reincarnate requires a clone-per-agent, shared-workspace or hub-managed workspace"
		if project.IsWorktreePerAgent() {
			workspaceModeErr = "reincarnate does not yet support worktree-per-agent workspaces"
		}
	}
	// A linked project's workspace is a broker-local path, never on a
	// shared export, so it cannot move.
	if moveTarget != nil && workspaceModeErr == "" && linkedProjectPath != "" {
		workspaceModeErr = "reincarnate --broker does not support linked projects; the workspace is local to the current broker"
	}
	if moveTarget != nil {
		s.planReincarnateMove(w, r, agent, project, moveTarget, workspaceModeErr, hasGitClone)
		return
	}
	if workspaceModeErr != "" {
		writeError(w, http.StatusBadRequest, ErrCodeValidationError, workspaceModeErr, nil)
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

	// AC-8 / design §3.4 Amendment A3, moved ahead of the plan computation
	// (design §3.4 Amendment A11 item 3): a reincarnation already in flight
	// (or one that failed and is retryable — ReincarnationStateFailed is
	// excluded, same as the real-path claim below) must 409 on a --dry-run
	// request too, not just on a real one. Checked here, before any plan is
	// computed, so a dry run against a busy agent reports the conflict
	// instead of silently computing and returning a plan that a concurrent
	// real request could invalidate before the caller ever acts on it.
	if agent.ReincarnationState != store.ReincarnationStateNone && agent.ReincarnationState != store.ReincarnationStateFailed {
		Conflict(w, "a reincarnation is already pending for this agent")
		return
	}

	// AC-2's "dry-run changed nothing" and the real path's plan are computed
	// by the exact same call — buildFreshAppliedConfig only reads from the
	// store (templates, harness configs, pre-start hooks, skills settings),
	// it never writes. Any store writes happen only below this point, and
	// only for a non-dry-run request.
	imageRegistry := dispatchImageRegistry(dispatcher)
	fresh, warnings, err := s.buildFreshAppliedConfig(ctx, agent, project, imageRegistry)
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"failed to resolve new configuration: "+err.Error(), nil)
		return
	}
	plan := computeReincarnationPlan(agent.AppliedConfig, fresh, warnings, imageRegistry)
	targetGeneration := agent.Generation + 1

	if req.DryRun {
		writeJSON(w, http.StatusOK, ReincarnateAgentResponse{
			AgentID:    agent.ID,
			Generation: targetGeneration,
			State:      "planned",
			Plan:       plan,
			// A named target here is the agent's current broker.
			SourceBrokerID: brokerIDIfSet(targetBrokerID, agent.RuntimeBrokerID),
			TargetBrokerID: targetBrokerID,
		})
		return
	}

	// The claim is guarded by the agent row's own optimistic lock
	// (state_version), and the claim and the reincarnation record commit
	// together, so a version conflict or any other failure leaves no record
	// stuck in "pending".
	//
	// The already-pending/already-starting check itself lives above, before
	// the plan is computed, so it also gates --dry-run; a concurrent real
	// request could slip in between that check and this claim, and the
	// claim's state_version CAS catches that race.
	agent.ReincarnationState = store.ReincarnationStatePending
	// Design §3.4 Amendment A6.6: ReincarnationUpdatedAt (not Updated) is
	// what the replica-safe sweep's agent-state backstop keys its staleness
	// check on, because Updated is also bumped by every broker heartbeat.
	claimedAt := time.Now()
	agent.ReincarnationUpdatedAt = &claimedAt

	requestedBy := ""
	requesterIdentity := GetIdentityFromContext(ctx)
	if requesterIdentity != nil {
		requestedBy = requesterIdentity.ID()
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
	// The claim, the reincarnation record, the authority re-record, the
	// reincarnate-claim hooks and the audit record commit in one
	// transaction (reincarnateClaimTx), so a failure at any step leaves the
	// agent unclaimed with no record behind it.
	// The claim also requires that no start claim is held, live or
	// unconfirmed: a start whose outcome is unknown may still create a
	// container this reincarnation would then compete with.
	if err := s.reincarnateClaimTx(ctx, agent, rec, auth, auditActorFromContext(ctx)); err != nil {
		var held *store.ClaimHeldError
		if errors.Is(err, store.ErrVersionConflict) {
			Conflict(w, "agent was concurrently modified; retry")
			return
		}
		if errors.As(err, &held) {
			Conflict(w, "a start is in progress for this agent; retry once it completes")
			return
		}
		if errors.Is(err, store.ErrClaimPredicate) {
			Conflict(w, "a reincarnation is already pending for this agent")
			return
		}
		if errors.Is(err, errAgentCreateWriteInvalid) {
			s.agentLifecycleLog.Error("handleReincarnateAgent: incomplete authority re-record", "agent_id", agent.ID, "error", err)
			InternalError(w)
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}
	// A reincarnation starts the agent (also one that was stopped): record
	// that the agent is meant to run, now, before the worker's start, so a
	// stop the user records during the reincarnation is newer and wins.
	// Limits: the intent is written after the claim commits, so a stop
	// recorded in that short gap is overwritten by this running intent; and
	// if the write fails, the reincarnation proceeds with only a warning
	// (the intent keeps its previous value).
	if _, err := s.recordRunIntent(ctx, agent, store.RunIntentRunning); err != nil {
		s.agentLifecycleLog.Warn("Reincarnate: failed to record run intent", "agent_id", agent.ID, "error", err)
	}

	// design §3.4 Amendment A3: the requester and the creator must both
	// learn of a failure. The creator is already subscribed (from create);
	// if the requester is a different principal than the agent itself and
	// has no existing subscription, subscribe them now via the same
	// mechanism create's --notify uses. failReincarnation's
	// PublishAgentStatus on phase=error then reaches both through the
	// ordinary subscription-dispatch path — no new delivery path needed.
	// Deliberately after the record insert above: if that insert fails, the
	// request errors out and there is no reincarnation to be notified about,
	// so subscribing first would leave a subscription with nothing behind it.
	s.ensureReincarnateRequesterSubscribed(ctx, agent, requesterIdentity, requestedBy)

	// Detached background worker (design §3.1, §3.7): the request returns
	// 202 before any teardown starts, and the worker runs on a context
	// independent of this request's. A self-migration stops the calling
	// container mid-flight, which would cancel r.Context() and abort the
	// worker if it inherited it — exactly the self-deletion
	// context-cancellation hazard §3.0 identifies for the rejected design.
	// claimedAt (Nit, p2a-r1 review): the preamble's catch-up window start is
	// this exact claim instant, not rec.RequestedAt — the store stamps that
	// a few ms later inside CreateAgentReincarnation, after the gate in the
	// three delivery paths could already have started deferring messages.
	go s.runReincarnationWorker(context.Background(), agent.ID, rec.ID, rec.PreviousAppliedConfig, fresh, req.Handoff, claimedAt, requestedBy, &plan, targetGeneration, admittedDeletionClaim)

	writeJSON(w, http.StatusAccepted, ReincarnateAgentResponse{
		AgentID:    agent.ID,
		Generation: targetGeneration,
		State:      store.AgentReincarnationStatePending,
		Plan:       plan,
		// A named target here is the agent's current broker.
		SourceBrokerID: brokerIDIfSet(targetBrokerID, agent.RuntimeBrokerID),
		TargetBrokerID: targetBrokerID,
	})
}

// isSelfRequest reports whether the caller is the agent itself.
func isSelfRequest(ctx context.Context, agent *store.Agent) bool {
	identity := GetIdentityFromContext(ctx)
	if identity == nil || identity.Type() != "agent" {
		return false
	}
	agentIdent, ok := identity.(AgentIdentity)
	return ok && agentIdent.ID() == agent.ID
}

// brokerIDIfSet returns id when target is non-empty, else "".
func brokerIDIfSet(target, id string) string {
	if target == "" {
		return ""
	}
	return id
}

// planReincarnateMove answers a dry-run move of agent to dst: it runs the
// move eligibility checks and returns the first refusal, or 200 with the
// reincarnation plan and the verdict. It writes no agent, broker, project or
// quota state; the passthrough re-check may record its authorization
// decision in the audit log and call IAM, like every passthrough gate.
func (s *Server) planReincarnateMove(w http.ResponseWriter, r *http.Request, agent *store.Agent, project *store.Project, dst *store.RuntimeBroker, workspaceModeErr string, cloneMode bool) {
	ctx := r.Context()
	src, err := s.store.GetRuntimeBroker(ctx, agent.RuntimeBrokerID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	in := moveEligibilityInput{
		Agent:              agent,
		Src:                src,
		Dst:                dst,
		WorkspaceModeError: workspaceModeErr,
		CloneMode:          cloneMode,
		SelfMove:           isSelfRequest(ctx, agent),
		Probes:             s.moveProbesFor(r, project, agent.AppliedConfig),
	}
	if workspaceModeErr != "" {
		v, ref := evaluateMoveEligibility(in)
		writeMoveRefusal(w, ref, v)
		return
	}

	dispatcher := s.GetDispatcher()
	if dispatcher == nil {
		writeError(w, http.StatusBadRequest, ErrCodeValidationError,
			"agent reincarnation requires hub mode with a runtime broker dispatcher", nil)
		return
	}
	if agent.ReincarnationState != store.ReincarnationStateNone && agent.ReincarnationState != store.ReincarnationStateFailed {
		Conflict(w, "a reincarnation is already pending for this agent")
		return
	}

	// buildFreshAppliedConfig only reads; the plan and the profile the
	// agent would run under come from the same call the real path uses.
	imageRegistry := dispatchImageRegistry(dispatcher)
	fresh, warnings, err := s.buildFreshAppliedConfig(ctx, agent, project, imageRegistry)
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"failed to resolve new configuration: "+err.Error(), nil)
		return
	}
	in.Profile = effectiveRuntimeProfileName(fresh.Profile, project)
	v, ref := evaluateMoveEligibility(in)
	if ref != nil {
		writeMoveRefusal(w, ref, v)
		return
	}
	writeJSON(w, http.StatusOK, ReincarnateAgentResponse{
		AgentID:        agent.ID,
		Generation:     agent.Generation + 1,
		State:          "planned",
		Plan:           computeReincarnationPlan(agent.AppliedConfig, fresh, warnings, imageRegistry),
		SourceBrokerID: src.ID,
		TargetBrokerID: dst.ID,
		MoveVerdict:    &v,
	})
}

// linkedProjectPath resolves the local path a runtime broker has registered
// for the agent's project via a ProjectProvider, exactly as the dispatcher's
// resolveDispatchProjectInfo does for a real dispatch — used by
// handleReincarnateAgent's eligibility gate to compute the effective
// dispatched workspace (design §3.4 Amendment A23.1, review p1b-r1 R1).
// Returns "" when the agent has no broker assigned yet, or when no provider
// (or no LocalPath) is registered for it — the gate then evaluates the raw
// Workspace, matching a non-linked (hub-native) dispatch.
func (s *Server) linkedProjectPath(ctx context.Context, agent *store.Agent) string {
	if agent.RuntimeBrokerID == "" {
		return ""
	}
	provider, err := s.store.GetProjectProvider(ctx, agent.ProjectID, agent.RuntimeBrokerID)
	if err != nil || provider == nil {
		return ""
	}
	return provider.LocalPath
}

// ensureReincarnateRequesterSubscribed implements design §3.4 Amendment
// A3.8: a reincarnate requester who is not the agent itself, and
// who is not already subscribed to it, gets a notification subscription via
// the same createNotifySubscription mechanism create's --notify flag uses.
// The creator is already subscribed from create, so this closes the gap for
// a coordinator or other requester who is not the creator — failReincarnation's
// PublishAgentStatus on phase=error then reaches both through the ordinary
// subscription-dispatch path.
//
// No-ops for a self-request (the agent does not need to subscribe to
// itself) and for any identity type this can't resolve to a subscriber
// (there is no "requester" concept for a nil identity, and this handler is
// unreachable without one — authorizeAgentReincarnate already required it).
// Best-effort: a lookup or write failure here must not fail the
// reincarnate request itself, so errors are logged, not returned.
func (s *Server) ensureReincarnateRequesterSubscribed(ctx context.Context, agent *store.Agent, identity Identity, requestedBy string) {
	if identity == nil {
		return
	}

	var subscriberType, subscriberID string
	switch identity.Type() {
	case "agent":
		agentIdent, ok := identity.(AgentIdentity)
		if !ok || agentIdent.ID() == agent.ID {
			return // self-request: nothing to subscribe.
		}
		requesterAgent, err := s.store.GetAgent(ctx, agentIdent.ID())
		if err != nil {
			s.agentLifecycleLog.Warn("reincarnate: failed to resolve requester agent for notify subscription",
				"agent_id", agent.ID, "requester_id", agentIdent.ID(), "error", err)
			return
		}
		subscriberType = store.SubscriberTypeAgent
		subscriberID = requesterAgent.Slug
	case "user", "dev":
		userIdent, ok := identity.(UserIdentity)
		if !ok {
			return
		}
		subscriberType = store.SubscriberTypeUser
		subscriberID = userIdent.ID()
	default:
		return
	}

	existing, err := s.store.GetNotificationSubscriptions(ctx, agent.ID)
	if err != nil {
		s.agentLifecycleLog.Warn("reincarnate: failed to check existing notify subscriptions",
			"agent_id", agent.ID, "error", err)
		return
	}
	for _, sub := range existing {
		if sub.SubscriberType == subscriberType && sub.SubscriberID == subscriberID {
			return // already subscribed (e.g. the creator, or a repeat requester).
		}
	}

	s.createNotifySubscription(ctx, agent.ID, agent.ProjectID, subscriberType, subscriberID, requestedBy)
}

// reincarnateAuthorityFor decides what authority a reincarnation of agent
// re-records. A self-reincarnation re-records nothing: it
// returns nil, and the existing edge with its frozen provenance and ceiling
// stays in force. Any other requester becomes the recorded delegator, so it
// must pass CanDelegate for the stored role, and its source ceiling must
// cover that role (childRoleWithinCeiling, role explicit). On a refusal the
// response is written (403, 503 for a ceiling lookup fault, or 500 for a
// nil agent) and ok is false; nothing has been written to the store.
func (s *Server) reincarnateAuthorityFor(w http.ResponseWriter, r *http.Request, agent *store.Agent) (auth *reincarnateAuthority, ok bool) {
	if agent == nil {
		s.agentLifecycleLog.Error("reincarnate: nil agent in reincarnateAuthorityFor")
		InternalError(w)
		return nil, false
	}
	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil && agentIdent.ID() == agent.ID {
		return nil, true
	}
	resource := Resource{Type: "agent", ID: agent.ID, ParentType: "project", ParentID: agent.ProjectID}
	if identity == nil {
		writeForbidden(w, "Reincarnation requires an authenticated requester")
		return nil, false
	}
	if s.authzService == nil {
		s.agentLifecycleLog.Error("handleReincarnateAgent: no authorization service to re-record authority", "agent_id", agent.ID)
		InternalError(w)
		return nil, false
	}

	role, _ := agentRoleAndScopes(agent)
	decision := s.authzService.CanDelegate(ctx, identity, GrantDescriptor{
		Type:      GrantTypeAgentDelegation,
		AgentRole: string(role),
		ProjectID: agent.ProjectID,
		ScopeType: store.RoleScopeProject,
		ScopeID:   agent.ProjectID,
	})
	if !decision.Allowed {
		logAuthzDenial(r, identity, resource, ActionLifecycle, "CanDelegate denied: "+decision.Reason)
		writeForbidden(w, "Cannot delegate agent authority you do not hold: "+decision.Reason)
		return nil, false
	}

	ceiling, prov, err := s.authzService.sourceEffectCeiling(ctx, identity)
	if err != nil {
		cause, structural := ceilingDenyCauseForError(err)
		if !structural {
			s.agentLifecycleLog.Error("handleReincarnateAgent: effect ceiling lookup failed",
				"agent_id", agent.ID, "error", err)
			writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable,
				"Unable to evaluate the credential's delegation ceiling; retry later", nil)
			return nil, false
		}
		logAuthzDenial(r, identity, resource, ActionLifecycle,
			"effect ceiling denied: "+string(cause)+": "+err.Error())
		writeForbiddenDenial(w, ceilingSourceDenialMessage(cause), DeniedByDelegationCeiling)
		return nil, false
	}
	if _, cause, allowed := childRoleWithinCeiling(ceiling, role, true); !allowed {
		msg := fmt.Sprintf("the credential's scopes do not cover the agent's role %q", role)
		logAuthzDenial(r, identity, resource, ActionLifecycle,
			"effect ceiling denied: "+string(cause)+": "+msg)
		writeForbiddenDenial(w, msg, DeniedByDelegationCeiling)
		return nil, false
	}

	delegatorType := store.DelegationPrincipalUser
	if GetAgentIdentityFromContext(ctx) != nil {
		delegatorType = store.DelegationPrincipalAgent
	}
	return &reincarnateAuthority{
		DelegatorType: delegatorType,
		DelegatorID:   identity.ID(),
		Role:          string(role),
		Ceiling:       ceiling,
		Provenance:    prov,
	}, true
}
