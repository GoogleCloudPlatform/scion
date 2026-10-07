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
	"sort"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// reincarnatePatchNeedsUpdateMsg is the 403 body for a user caller whose
// reincarnate request has patch flags but who lacks agent.update.
const reincarnatePatchNeedsUpdateMsg = "Reincarnate patch flags (--service-account, --role, --model, --thinking-level, --harness-auth, --image) need permission to update the agent (agent.update) in addition to lifecycle; a user access token cannot grant it, so use a signed-in session"

// maxHandoffBytes bounds the reincarnate request's handoff text (design §3.2).
const maxHandoffBytes = 256 * 1024

// ReincarnateAgentRequest is the request body for
// POST /api/v1/projects/{projectId}/agents/{agentIdOrSlug}/reincarnate and
// its ID-addressed twin POST /api/v1/agents/{agentId}/reincarnate (design
// §3.2). Besides Handoff and DryRun it accepts the patch fields of
// ptone/scion#3302 (ServiceAccount, Role, Image, Model, ThinkingLevel,
// HarnessAuth). The remaining override fields are accepted on the wire (so
// a newer CLI gets a clear 400) but rejected if set.
type ReincarnateAgentRequest struct {
	Handoff string `json:"handoff,omitempty"`
	DryRun  bool   `json:"dryRun,omitempty"`
	// TargetBroker (a broker ID, name or slug) asks to move the agent to
	// that broker; it must mount the same NFS export as the current one.
	// Empty, or the agent's current broker, is a plain reincarnation.
	TargetBroker string `json:"targetBroker,omitempty"`

	// SharedDirBackends changes the recorded shared-dir storage backend of
	// the named shared dirs (dir name to "nfs", the only supported value).
	// Only the agent's record on its broker changes; no data is copied,
	// moved or deleted. Not accepted for a self-reincarnation.
	SharedDirBackends map[string]string `json:"sharedDirBackends,omitempty"`
	// AllowEmptySharedDir, with SharedDirBackends, skips the start check
	// that refuses an empty nfs directory while the dir's previous local
	// directory is not empty.
	AllowEmptySharedDir bool `json:"allowEmptySharedDir,omitempty"`

	// Patch fields (ptone/scion#3302): each changes the next generation's
	// setting and is kept by later reincarnations. Empty (nil for
	// ThinkingLevel) means unchanged.
	//
	// ServiceAccount is a GCP service account ID to assign ("assign"
	// metadata mode), checked exactly as create checks it.
	ServiceAccount string `json:"serviceAccount,omitempty"`
	// Role is the agent role (none, readonly, baseline, full), checked
	// exactly as create checks an explicit role.
	Role          string `json:"role,omitempty"`
	Image         string `json:"image,omitempty"`
	Model         string `json:"model,omitempty"`
	ThinkingLevel *int   `json:"thinkingLevel,omitempty"`
	HarnessAuth   string `json:"harnessAuth,omitempty"`

	// Overrides not yet supported; any non-zero value here is a 400.
	HarnessConfig  string            `json:"harnessConfig,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	TemplateHash   string            `json:"templateHash,omitempty"`
	ResetOverrides bool              `json:"resetOverrides,omitempty"`
	Rollback       bool              `json:"rollback,omitempty"`
}

// hasUnsupportedOverrides reports whether the request sets any override
// field that is not yet supported.
func (r ReincarnateAgentRequest) hasUnsupportedOverrides() bool {
	return r.HarnessConfig != "" || len(r.Env) > 0 || r.TemplateHash != "" || r.ResetOverrides || r.Rollback
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

	// Patched lists the request's patch fields (ptone/scion#3302), in
	// display order. Its presence also tells a CLI the hub applied them.
	Patched []string `json:"patched,omitempty"`
	// Old and new values of patch fields not otherwise on the plan, set
	// only when the request patched that field.
	Role           *FieldChange `json:"role,omitempty"`
	ServiceAccount *FieldChange `json:"serviceAccount,omitempty"`
	ThinkingLevel  *FieldChange `json:"thinkingLevel,omitempty"`
	HarnessAuth    *FieldChange `json:"harnessAuth,omitempty"`
	// SharedDirBackends and AllowEmptySharedDir echo the request's explicit
	// shared dir backend change.
	SharedDirBackends   map[string]string `json:"sharedDirBackends,omitempty"`
	AllowEmptySharedDir bool              `json:"allowEmptySharedDir,omitempty"`
}

// validateSharedDirBackendRequest checks a reincarnate request's explicit
// shared dir backend change. Whether each dir is one of the agent's shared
// dirs, and whether the broker has a complete nfs block, is checked by the
// broker, which holds the project settings and the agent's record.
func validateSharedDirBackendRequest(req ReincarnateAgentRequest) error {
	if len(req.SharedDirBackends) == 0 {
		if req.AllowEmptySharedDir {
			return fmt.Errorf("allowEmptySharedDir needs a shared dir backend change")
		}
		return nil
	}
	names := make([]string, 0, len(req.SharedDirBackends))
	for name := range req.SharedDirBackends {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := api.ValidateSharedDirs([]api.SharedDir{{Name: name}}); err != nil {
			return fmt.Errorf("sharedDirBackends: invalid shared dir name %q", name)
		}
		if backend := req.SharedDirBackends[name]; backend != "nfs" {
			return fmt.Errorf("sharedDirBackends: shared dir %q: only a change to the nfs backend is supported (got %q)", name, backend)
		}
	}
	return nil
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
//     check, as long as the request patches nothing (D2's "no override"
//     condition). A self request with a patch flag gets the full lifecycle
//     check in handleReincarnateAgent once the body is read
//     (ptone/scion#3302, decision D1).
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
	if err := validateSharedDirBackendRequest(req); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeValidationError, err.Error(), nil)
		return
	}
	// A shared dir backend change is an operator action: the self
	// exemption in authorizeAgentReincarnate does not cover it.
	if (len(req.SharedDirBackends) > 0 || req.AllowEmptySharedDir) && isSelfRequest(ctx, agent) {
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"an agent cannot change its own shared dir backend; ask a user or another agent with lifecycle access", nil)
		return
	}
	if !validateReincarnatePatchRequest(w, req) {
		return
	}
	// Decision D1 (ptone/scion#3302): the D2 self exemption in
	// authorizeAgentReincarnate holds only for a request that patches
	// nothing. A self request with a patch flag needs the same lifecycle
	// authority as reincarnating another agent.
	if req.hasPatch() && isSelfRequest(ctx, agent) {
		if !s.authorizeAgentLifecycle(w, r, agent, ActionLifecycle) {
			return
		}
	}
	// Decision D4 (ptone/scion#3302): a patch flag edits the agent's
	// config, so a user caller (session or UAT) also needs ActionUpdate on
	// the target, the same gate the agent PATCH applies (applyAgentUpdate).
	// agent.update has no UAT scope, so a UAT cannot patch. Agent callers
	// stay on the lifecycle check (D1). Runs before any side effect, for a
	// dry run too.
	if req.hasPatch() && GetUserIdentityFromContext(ctx) != nil {
		if !s.authorizeMsg(w, r, agentResource(agent), ActionUpdate, reincarnatePatchNeedsUpdateMsg) {
			return
		}
	}

	project, err := s.store.GetProject(ctx, agent.ProjectID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Patch access checks (ptone/scion#3302): --role runs create's role
	// lattice, --service-account create's assignment gate. Then the
	// authority re-record: a requester other than the agent itself becomes
	// the agent's recorded delegator, so it must pass CanDelegate and its
	// ceiling must cover the role the next generation runs with (the
	// patched role, else the stored one). All of this runs before the
	// broker and the agent state are examined and before anything is
	// written, so a refused request claims nothing and a dry run reports
	// the same refusal.
	patch, ok := s.resolveReincarnatePatch(w, r, agent, project, req)
	if !ok {
		return
	}
	targetRole, _ := agentRoleAndScopes(agent)
	if req.Role != "" {
		targetRole = AgentRole(req.Role)
	}
	auth, ok := s.reincarnateAuthorityFor(w, r, agent, targetRole, req.Role != "")
	if !ok {
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
	if moveTarget != nil && (len(req.SharedDirBackends) > 0 || req.AllowEmptySharedDir) {
		writeError(w, http.StatusBadRequest, ErrCodeValidationError,
			"a shared dir backend change cannot be combined with a move to another broker", nil)
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
	// Empty-per-agent workspaces are broker-local, unsynced state.
	// A4 (ptone/scion#2727): an empty-per-agent workspace can move when it
	// is on the shared export (placement export) and both brokers see the
	// same export (equal identity markers): the target finds it in place.
	// miller79/scion#167: on the same broker (no move target, including
	// `--broker <current>`) the broker reuses the private workspace in
	// place, gated below on a local-disk runtime and the broker's
	// ReprovisionEmptyPerAgent capability. Any other empty-per-agent
	// request (a move whose workspace is not movable) is still refused.
	emptyPerAgentMove := moveTarget != nil && project.IsEmptyPerAgent() && s.emptyPerAgentWorkspaceMovable(ctx, agent, moveTarget)
	sameBrokerEmptyPerAgent := project.IsEmptyPerAgent() && moveTarget == nil
	workspaceModeErr := ""
	if project.IsEmptyPerAgent() && !emptyPerAgentMove && !sameBrokerEmptyPerAgent {
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
	if workspaceModeErr == "" && (agent.AppliedConfig == nil || (!emptyPerAgentMove && (project.IsWorktreePerAgent() ||
		(hasGitClone && project.IsSharedWorkspace()) ||
		switchedToCloneOnly ||
		!api.ReincarnateEligible(hasGitClone, effectiveWorkspace, project.IsEmptyPerAgent())))) {
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
		s.planReincarnateMove(w, r, req, agent, auth, project, moveTarget, workspaceModeErr, hasGitClone || emptyPerAgentMove, admittedDeletionClaim, patch)
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
	// miller79/scion#167: in-place reuse is safe only where the private
	// workspace is a directory on the broker's own disk. Checked, like the
	// capability below, before anything is planned or stopped, so a dry
	// run reports the same verdict. An unrecorded runtime falls back to
	// resolveAgentRuntime (the value enrichAgents displays), which returns
	// "" when the broker's profiles are ambiguous: still refused.
	if sameBrokerEmptyPerAgent && !api.IsLocalDiskRuntime(effectiveAgentRuntime(agent, broker)) {
		writeError(w, http.StatusBadRequest, ErrCodeValidationError,
			"empty-per-agent reincarnation is supported only on local-disk runtimes (docker, podman, container)", nil)
		return
	}
	if sameBrokerEmptyPerAgent && !broker.Capabilities.ReprovisionEmptyPerAgent {
		// An older broker advertises Reprovision and EmptyPerAgentWorkspace
		// but its reprovision refuses empty-per-agent, which would only
		// surface after the worker had stopped the agent. Refuse up front.
		writeError(w, http.StatusPreconditionFailed, ErrCodeUnsupportedCapability,
			"runtime broker does not support in-place empty-per-agent reincarnation; upgrade the broker", nil)
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
	fresh, warnings, err := s.buildPatchedAppliedConfig(ctx, agent, project, imageRegistry, patch)
	if err != nil {
		if writeWorkspaceStorageUnavailable(w, err) {
			return
		}
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"failed to resolve new configuration: "+err.Error(), nil)
		return
	}
	if len(req.SharedDirBackends) > 0 {
		fresh.SharedDirBackendChanges = req.SharedDirBackends
		fresh.AllowEmptySharedDir = req.AllowEmptySharedDir
	}
	// Fail fast, as start and restart do, when the GCP identity the fresh
	// config will run with is no longer allowed for this agent. Checked
	// before the claim and the worker's stop, so a refused request leaves
	// the agent as it was, and a dry run reports the same refusal.
	runAs := *agent
	runAs.AppliedConfig = fresh
	if s.gcpIdentityStartRefusal(ctx, w, &runAs, "reincarnate") {
		return
	}

	plan := computeReincarnationPlan(agent.AppliedConfig, fresh, warnings, imageRegistry)
	addPatchToPlan(&plan, agent.AppliedConfig, fresh, req)
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

	s.startReincarnation(w, r, agent, auth, fresh, plan, targetGeneration, req.Handoff, admittedDeletionClaim,
		brokerIDIfSet(targetBrokerID, agent.RuntimeBrokerID), targetBrokerID, nil)
}

// startReincarnation claims the agent, records the reincarnation (with the
// authority re-record auth, nil for a self-reincarnation) and starts the
// detached worker, answering 202. sourceBrokerID and targetBrokerID are
// echoed in the response (both empty unless the request named a target);
// move is non-nil for a cross-broker move.
func (s *Server) startReincarnation(w http.ResponseWriter, r *http.Request, agent *store.Agent, auth *reincarnateAuthority, fresh *store.AgentAppliedConfig, plan ReincarnationPlan, targetGeneration int, handoff string, admittedDeletionClaim int64, sourceBrokerID, targetBrokerID string, move *reincarnationMove) {
	ctx := r.Context()

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
		Handoff:               handoff,
		SourceBrokerID:        moveSourceID(move),
		TargetBrokerID:        moveTargetID(move),
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
	go s.runReincarnationWorker(context.Background(), agent.ID, rec.ID, rec.PreviousAppliedConfig, fresh, handoff, claimedAt, requestedBy, &plan, targetGeneration, admittedDeletionClaim, move)

	writeJSON(w, http.StatusAccepted, ReincarnateAgentResponse{
		AgentID:        agent.ID,
		Generation:     targetGeneration,
		State:          store.AgentReincarnationStatePending,
		Plan:           plan,
		SourceBrokerID: sourceBrokerID,
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

func moveSourceID(m *reincarnationMove) string {
	if m == nil {
		return ""
	}
	return m.SourceBrokerID
}

func moveTargetID(m *reincarnationMove) string {
	if m == nil {
		return ""
	}
	return m.TargetBrokerID
}

// brokerIDIfSet returns id when target is non-empty, else "".
func brokerIDIfSet(target, id string) string {
	if target == "" {
		return ""
	}
	return id
}

// planReincarnateMove answers a move of agent to dst: it runs the move
// eligibility checks and returns the first refusal. Otherwise a dry run gets
// 200 with the reincarnation plan and the verdict, writing no agent, broker,
// project or quota state, and a real request starts the move (202; the
// worker re-checks eligibility before its first side effect). The
// passthrough re-check may record its authorization decision in the audit
// log and call IAM, like every passthrough gate. A patch (validated and
// authorized by the caller) is part of the plan and of the config the agent
// is provisioned with on the target.
func (s *Server) planReincarnateMove(w http.ResponseWriter, r *http.Request, req ReincarnateAgentRequest, agent *store.Agent, auth *reincarnateAuthority, project *store.Project, dst *store.RuntimeBroker, workspaceModeErr string, cloneMode bool, admittedDeletionClaim int64, patch *reincarnatePatch) {
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
	fresh, warnings, err := s.buildPatchedAppliedConfig(ctx, agent, project, imageRegistry, patch)
	if err != nil {
		if writeWorkspaceStorageUnavailable(w, err) {
			return
		}
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"failed to resolve new configuration: "+err.Error(), nil)
		return
	}
	// A dry-run move reports the same GCP identity refusal as start and
	// as an in-place reincarnate, so every dry-run variant agrees.
	runAs := *agent
	runAs.AppliedConfig = fresh
	if s.gcpIdentityStartRefusal(ctx, w, &runAs, "reincarnate") {
		return
	}

	plan := computeReincarnationPlan(agent.AppliedConfig, fresh, warnings, imageRegistry)
	addPatchToPlan(&plan, agent.AppliedConfig, fresh, req)
	in.Profile = effectiveRuntimeProfileName(fresh.Profile, project)
	// The access probes judge the config the next generation runs with: a
	// --service-account patch replaces a passthrough identity, so the
	// passthrough gate (and the self-move passthrough refusal) follow the
	// patched GCP identity, not the outgoing one.
	in.Probes = s.moveProbesFor(r, project, fresh)
	v, ref := evaluateMoveEligibility(in)
	if ref != nil {
		writeMoveRefusal(w, ref, v)
		return
	}
	if req.DryRun {
		writeJSON(w, http.StatusOK, ReincarnateAgentResponse{
			AgentID:        agent.ID,
			Generation:     agent.Generation + 1,
			State:          "planned",
			Plan:           plan,
			SourceBrokerID: src.ID,
			TargetBrokerID: dst.ID,
			MoveVerdict:    &v,
		})
		return
	}
	s.startReincarnation(w, r, agent, auth, fresh, plan, agent.Generation+1, req.Handoff, admittedDeletionClaim, src.ID, dst.ID, &reincarnationMove{
		SourceBrokerID:    src.ID,
		TargetBrokerID:    dst.ID,
		ProjectID:         project.ID,
		SelfMove:          in.SelfMove,
		AgentDirWorkspace: cloneMode,
		Profile:           in.Profile,
	})
}

// emptyPerAgentWorkspaceMovable reports whether an empty-per-agent agent's
// workspace can move to dst: its last start placed it on the shared export,
// and the source and dst report the same export with equal identity
// markers.
func (s *Server) emptyPerAgentWorkspaceMovable(ctx context.Context, agent *store.Agent, dst *store.RuntimeBroker) bool {
	if !isWorkspacePlacementOnExport(agent.WorkspacePlacement) {
		return false
	}
	src, err := s.store.GetRuntimeBroker(ctx, agent.RuntimeBrokerID)
	if err != nil {
		return false
	}
	if !api.SameWorkspaceExport(src.WorkspaceStorage, dst.WorkspaceStorage) {
		return false
	}
	srcID, dstID := src.WorkspaceStorage.NFS.ExportID, dst.WorkspaceStorage.NFS.ExportID
	return srcID != "" && srcID == dstID
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

// effectiveAgentRuntime is the agent's recorded runtime, or, when none is
// recorded yet, the one resolveAgentRuntime derives from the broker's
// profiles ("" when ambiguous).
func effectiveAgentRuntime(agent *store.Agent, broker *store.RuntimeBroker) string {
	if agent.Runtime != "" {
		return agent.Runtime
	}
	return resolveAgentRuntime(agent, broker)
}

// reincarnateAuthorityFor decides what authority a reincarnation of agent
// re-records. role is the role the next generation runs with: the stored
// role, or the --role patch (roleChanged). A self-reincarnation re-records
// nothing: it returns nil, and the existing edge with its frozen provenance
// and ceiling stays in force; when it changes the role it must still pass
// CanDelegate and the ceiling for the new role, as create requires of an
// agent granting a role. Any other requester becomes the recorded
// delegator, so it must pass CanDelegate for role, and its source ceiling
// must cover role (childRoleWithinCeiling, role explicit). On a refusal the
// response is written (403, 503 for a ceiling lookup fault, or 500 for a
// nil agent) and ok is false; nothing has been written to the store.
func (s *Server) reincarnateAuthorityFor(w http.ResponseWriter, r *http.Request, agent *store.Agent, role AgentRole, roleChanged bool) (auth *reincarnateAuthority, ok bool) {
	if agent == nil {
		s.agentLifecycleLog.Error("reincarnate: nil agent in reincarnateAuthorityFor")
		InternalError(w)
		return nil, false
	}
	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	agentIdent := GetAgentIdentityFromContext(ctx)
	self := agentIdent != nil && agentIdent.ID() == agent.ID
	if self && !roleChanged {
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
	if self {
		// Decision D1: a self request does not re-record the edge.
		return nil, true
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
