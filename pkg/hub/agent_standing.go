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

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Agent standing (ptone/scion#3433).
//
// An agent is in good standing when it is live, not held, every agent on its
// upward chain in its project is live and not held, and the user at the root
// of that chain is active and admitted to the agent's project (project
// membership, or system authority that applies to the project). Every
// capability site that an agent credential or an agent's schedule reaches
// asks agentStanding before it acts; any absent value or failed lookup
// refuses.

// errAgentNotInStanding is the policy refusal returned by agentStanding. The
// specific reason is carried by *standingDenial for audit and logs only;
// callers map every refusal to the same response for their audience.
var errAgentNotInStanding = errors.New("agent is not in good standing")

// Standing reason codes. They appear in logs and audit records only, never
// in a response body.
const (
	standingReasonAgentMissing   = "agent_missing"
	standingReasonAgentDeleted   = "agent_deleted"
	standingReasonAgentHeld      = "agent_held"
	standingReasonChainHeld      = "chain_agent_held"
	standingReasonChainDeleted   = "chain_agent_deleted"
	standingReasonChainBroken    = "chain_broken"
	standingReasonChainTooDeep   = "chain_too_deep"
	standingReasonNoRoot         = "no_resolvable_root"
	standingReasonRootMissing    = "root_user_missing"
	standingReasonRootInactive   = "root_user_inactive"
	standingReasonRootNotAdmited = "root_user_not_admitted"
	standingReasonNoProject      = "agent_has_no_project"
)

// standingDenial is the error agentStanding returns for a policy refusal.
// errors.Is(err, errAgentNotInStanding) is true for every standingDenial.
type standingDenial struct {
	Reason  string
	AgentID string
	RootID  string
}

func (d *standingDenial) Error() string {
	return fmt.Sprintf("%s: %s (agent %s)", errAgentNotInStanding.Error(), d.Reason, d.AgentID)
}

func (d *standingDenial) Unwrap() error { return errAgentNotInStanding }

func denyStanding(reason, agentID, rootID string) error {
	return &standingDenial{Reason: reason, AgentID: agentID, RootID: rootID}
}

// standingReason returns the reason code of a standing refusal, "fault" for
// any other non-nil error, and "" for nil.
func standingReason(err error) string {
	if err == nil {
		return ""
	}
	var d *standingDenial
	if errors.As(err, &d) {
		return d.Reason
	}
	return "fault"
}

// standingPermission is the permission the root user's project admission is
// evaluated for. agent.read is held by every project role and is not part
// of the hub-member curated system role, so admission for it means project
// membership or system authority that applies to agents in the project.
const standingPermission = "agent.read"

// standingMaxChainDepth bounds the upward chain resolution. It is the
// delegation ceiling's chain bound: an agent deeper than this has no
// authority through the ceiling either, and is refused with
// standingReasonChainTooDeep (the descendant walk, which reaches deeper,
// still holds it).
const standingMaxChainDepth = maxDelegationDepth

// standingMemo is a per-request memo of agentStanding results. Only policy
// results (nil or a standingDenial) are remembered; faults are recomputed.
// It lives in the request context and dies with it.
type standingMemo struct {
	mu      sync.Mutex
	results map[string]error
}

type standingMemoKey struct{}

// withStandingMemo installs a fresh standing memo unless one is present.
func withStandingMemo(ctx context.Context) context.Context {
	if _, ok := ctx.Value(standingMemoKey{}).(*standingMemo); ok {
		return ctx
	}
	return context.WithValue(ctx, standingMemoKey{}, &standingMemo{results: map[string]error{}})
}

func standingMemoFrom(ctx context.Context) *standingMemo {
	m, _ := ctx.Value(standingMemoKey{}).(*standingMemo)
	return m
}

// agentHeld reports whether agentID has an active hold. An error means the
// caller must refuse.
func (s *Server) agentHeld(ctx context.Context, agentID string) (bool, error) {
	if s == nil || s.store == nil {
		return false, errors.New("agent hold lookup: store not available")
	}
	if agentID == "" {
		return false, errors.New("agent hold lookup: empty agent ID")
	}
	return s.store.HasActiveAgentHold(ctx, agentID)
}

// agentStanding returns nil only when the agent is in good standing (see the
// file comment). A policy refusal wraps errAgentNotInStanding; any other
// error is a lookup fault. Both refuse.
func (s *Server) agentStanding(ctx context.Context, agentID string) error {
	memo := standingMemoFrom(ctx)
	if memo != nil {
		memo.mu.Lock()
		res, ok := memo.results[agentID]
		memo.mu.Unlock()
		if ok {
			return res
		}
	}
	err := s.evaluateAgentStanding(ctx, agentID)
	if memo != nil && (err == nil || errors.Is(err, errAgentNotInStanding)) {
		memo.mu.Lock()
		memo.results[agentID] = err
		memo.mu.Unlock()
	}
	if err != nil {
		if errors.Is(err, errAgentNotInStanding) {
			slog.Debug("agent standing refused", "agent_id", agentID, "reason", standingReason(err))
		} else {
			slog.Warn("agent standing lookup failed", "agent_id", agentID, "error", err)
		}
	}
	return err
}

func (s *Server) evaluateAgentStanding(ctx context.Context, agentID string) error {
	if s == nil || s.store == nil || s.authzService == nil {
		return errors.New("agent standing: hub not fully configured")
	}
	if agentID == "" {
		return denyStanding(standingReasonAgentMissing, agentID, "")
	}
	// The root user's admission is evaluated for the root, not the
	// requester: never read the requester's memoized inputs.
	ctx = maskAuthzInputs(ctx)

	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return denyStanding(standingReasonAgentMissing, agentID, "")
		}
		return fmt.Errorf("agent standing: agent lookup: %w", err)
	}
	if agent == nil {
		return denyStanding(standingReasonAgentMissing, agentID, "")
	}
	return s.evaluateAgentRowStanding(ctx, agent, false)
}

// standingAdmission names the permission and class the root user's project
// admission is evaluated for.
type standingAdmission struct {
	permissionID string
	class        ProjectTargetClass
}

// defaultStandingAdmission is admission for agents in the project.
var defaultStandingAdmission = standingAdmission{
	permissionID: standingPermission,
	class:        ProjectTargetClass{ResourceType: permissions.ResourceAgent},
}

// agentStandingFor is agentStanding with the root user's admission
// evaluated for adm (unmemoised). The runtime material precheck uses it with
// the secret-use permission, the same admission its own root check applies.
func (s *Server) agentStandingFor(ctx context.Context, agentID string, adm standingAdmission) error {
	if s == nil || s.store == nil {
		return errors.New("agent standing: hub not fully configured")
	}
	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return denyStanding(standingReasonAgentMissing, agentID, "")
		}
		return fmt.Errorf("agent standing: agent lookup: %w", err)
	}
	return s.evaluateAgentRowStandingFor(ctx, agent, false, adm)
}

// evaluateAgentRowStanding is agentStanding for an already loaded row. With
// allowDeleted, a soft-deleted row is evaluated like a live one (used by
// entries that legitimately act on a deleted row, such as reincarnate).
func (s *Server) evaluateAgentRowStanding(ctx context.Context, agent *store.Agent, allowDeleted bool) error {
	return s.evaluateAgentRowStandingFor(ctx, agent, allowDeleted, defaultStandingAdmission)
}

// evaluateStoredAgentStanding re-reads the agent's row and evaluates its
// standing, so a caller's stale copy never decides. allowDeleted as for
// evaluateAgentRowStanding.
func (s *Server) evaluateStoredAgentStanding(ctx context.Context, agentID string, allowDeleted bool) error {
	if s == nil || s.store == nil {
		return errors.New("agent standing: hub not fully configured")
	}
	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return denyStanding(standingReasonAgentMissing, agentID, "")
		}
		return fmt.Errorf("agent standing: agent lookup: %w", err)
	}
	return s.evaluateAgentRowStanding(ctx, agent, allowDeleted)
}

func (s *Server) evaluateAgentRowStandingFor(ctx context.Context, agent *store.Agent, allowDeleted bool, adm standingAdmission) error {
	if s == nil || s.store == nil || s.authzService == nil {
		return errors.New("agent standing: hub not fully configured")
	}
	if agent == nil || agent.ID == "" {
		return denyStanding(standingReasonAgentMissing, "", "")
	}
	ctx = maskAuthzInputs(ctx)
	if !allowDeleted && !agent.DeletedAt.IsZero() {
		return denyStanding(standingReasonAgentDeleted, agent.ID, "")
	}
	if agent.ProjectID == "" {
		return denyStanding(standingReasonNoProject, agent.ID, "")
	}
	held, err := s.agentHeld(ctx, agent.ID)
	if err != nil {
		return fmt.Errorf("agent standing: hold lookup: %w", err)
	}
	if held {
		return denyStanding(standingReasonAgentHeld, agent.ID, "")
	}

	root, err := s.resolveChainRoot(ctx, s.store, agent, true)
	if err != nil {
		return err
	}
	return s.rootUserAdmitted(ctx, root, agent.ProjectID, agent.ID, adm)
}

// rootUserAdmitted checks that rootID names an active user admitted to
// projectID. agentID is only used in the refusal.
func (s *Server) rootUserAdmitted(ctx context.Context, rootID, projectID, agentID string, adm standingAdmission) error {
	user, err := s.store.GetUser(ctx, rootID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return denyStanding(standingReasonRootMissing, agentID, rootID)
		}
		return fmt.Errorf("agent standing: root user lookup: %w", err)
	}
	if user == nil {
		return denyStanding(standingReasonRootMissing, agentID, rootID)
	}
	if user.Status != store.UserStatusActive {
		return denyStanding(standingReasonRootInactive, agentID, rootID)
	}
	admitted, err := s.userAdmittedToProjectAs(ctx, s.store, user, projectID, adm, nil)
	if err != nil {
		return fmt.Errorf("agent standing: root user admission: %w", err)
	}
	if !admitted {
		return denyStanding(standingReasonRootNotAdmited, agentID, rootID)
	}
	return nil
}

// userAdmittedToProject reports whether user has live admission to
// projectID: project membership evidence, or system authority that applies
// to agents in the project. memo may be nil.
func (s *Server) userAdmittedToProject(ctx context.Context, user *store.User, projectID string, memo *ProjectAdmissionCache) (bool, error) {
	return s.userAdmittedToProjectOn(ctx, s.store, user, projectID, memo)
}

// authzFor returns the authorization service reading through st: the
// server's own for its store, otherwise one bound to st (a transaction, so
// the reads run on the transaction's connection).
func (s *Server) authzFor(st store.Store) *AuthzService {
	if st == s.store || s.authzService == nil {
		return s.authzService
	}
	return NewAuthzService(st, s.authzService.logger)
}

// userAdmittedToProjectOn is userAdmittedToProject reading through st.
func (s *Server) userAdmittedToProjectOn(ctx context.Context, st store.Store, user *store.User, projectID string, memo *ProjectAdmissionCache) (bool, error) {
	return s.userAdmittedToProjectAs(ctx, st, user, projectID, defaultStandingAdmission, memo)
}

// userAdmittedToProjectAs is userAdmittedToProjectOn for admission adm.
func (s *Server) userAdmittedToProjectAs(ctx context.Context, st store.Store, user *store.User, projectID string, adm standingAdmission, memo *ProjectAdmissionCache) (bool, error) {
	authz := s.authzFor(st)
	if authz == nil {
		return false, errors.New("project admission: authz service not available")
	}
	pc := PrincipalContext{
		Kind:     PrincipalKindUser,
		ID:       user.ID,
		Identity: NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, ""),
	}
	res, err := authz.ProjectAdmissionForClass(maskAuthzInputs(ctx), pc, projectID, adm.permissionID, adm.class, memo)
	if err != nil {
		return false, err
	}
	return res.Admitted, nil
}

// resolveChainRoot returns the ID of the user at the root of the agent's
// upward chain in its project; with checkChain it first checks that every
// agent on that chain is live and not held.
//
// The chain is followed through delegation edges in the agent's project:
// exactly one active edge per link, as edgeChainSourceResolver requires.
// Only when the agent has no active edge at all, or the chain ends in a
// migration-provenance edge, does resolution fall back to the stored
// links of the agent where the chain ended: its owner when the owner is a
// user, else its ancestry root user, else its creator when it has no owner.
// An agent named by those links is followed the same way. A broken chain
// (duplicate edges, a missing edge above the first link, a deleted link, a
// cycle, or a chain deeper than standingMaxChainDepth) refuses.
// resolveChainRoot reads through st (a transaction store inside one). With
// checkChain false it does not refuse a held or deleted agent on the chain
// (the membership sweep and the restore hook use it to find the user an
// agent is rooted at).
func (s *Server) resolveChainRoot(ctx context.Context, st store.Store, agent *store.Agent, checkChain bool) (string, error) {
	visited := map[string]bool{}
	projectID := agent.ProjectID
	current := agent
	// edgeOptional is true for the agent itself and for an agent reached
	// through a stored link: such an agent may predate delegation edges.
	// An agent reached through an edge must carry its own edge.
	edgeOptional := true
	for depth := 0; ; depth++ {
		if depth > standingMaxChainDepth {
			return "", denyStanding(standingReasonChainTooDeep, agent.ID, "")
		}
		if visited[current.ID] {
			return "", denyStanding(standingReasonChainBroken, agent.ID, "")
		}
		visited[current.ID] = true
		if checkChain && current.ID != agent.ID {
			if !current.DeletedAt.IsZero() {
				return "", denyStanding(standingReasonChainDeleted, agent.ID, "")
			}
			held, err := st.HasActiveAgentHold(ctx, current.ID)
			if err != nil {
				return "", fmt.Errorf("agent standing: chain hold lookup: %w", err)
			}
			if held {
				return "", denyStanding(standingReasonChainHeld, agent.ID, "")
			}
		}

		edges, err := st.GetDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, current.ID)
		if err != nil {
			return "", fmt.Errorf("agent standing: delegation edge lookup: %w", err)
		}
		var active []*store.DelegationEdge
		for _, e := range filterEdgesByScope(edges, store.RoleScopeProject, projectID) {
			if e.Active {
				active = append(active, e)
			}
		}

		useStoredLinks := false
		switch {
		case len(active) > 1:
			return "", denyStanding(standingReasonChainBroken, agent.ID, "")
		case len(active) == 0:
			if !edgeOptional {
				return "", denyStanding(standingReasonChainBroken, agent.ID, "")
			}
			useStoredLinks = true
		case isMigrationSentinel(active[0]):
			useStoredLinks = true
		}

		if !useStoredLinks {
			edge := active[0]
			switch edge.DelegatorType {
			case store.DelegationPrincipalUser:
				return edge.DelegatorID, nil
			case store.DelegationPrincipalAgent:
				parent, err := s.standingChainAgent(ctx, st, edge.DelegatorID, projectID, agent.ID)
				if err != nil {
					return "", err
				}
				current, edgeOptional = parent, false
				continue
			default:
				return "", denyStanding(standingReasonChainBroken, agent.ID, "")
			}
		}

		next, rootUser, err := s.storedLinkCandidate(ctx, st, current, projectID, agent.ID)
		if err != nil {
			return "", err
		}
		if rootUser != "" {
			return rootUser, nil
		}
		current, edgeOptional = next, true
	}
}

// standingChainAgent loads an agent named on the chain of origAgentID. A
// missing agent, or one outside the project, breaks the chain.
func (s *Server) standingChainAgent(ctx context.Context, st store.Store, id, projectID, origAgentID string) (*store.Agent, error) {
	a, err := st.GetAgent(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, denyStanding(standingReasonChainBroken, origAgentID, "")
		}
		return nil, fmt.Errorf("agent standing: chain agent lookup: %w", err)
	}
	if a == nil || a.ProjectID != projectID {
		return nil, denyStanding(standingReasonChainBroken, origAgentID, "")
	}
	return a, nil
}

// storedLinkCandidate resolves the next step of a chain from an agent's
// stored links, in order: its owner (a user ends the chain; an agent in the
// project continues it), else its ancestry root when that is a user, else,
// when it has no owner, its creator (user or agent). It returns either the
// next agent or the root user ID; with neither the agent has no resolvable
// root.
func (s *Server) storedLinkCandidate(ctx context.Context, st store.Store, a *store.Agent, projectID, origAgentID string) (*store.Agent, string, error) {
	tryUser := func(id string) (bool, error) {
		if id == "" {
			return false, nil
		}
		u, err := st.GetUser(ctx, id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return false, nil
			}
			return false, fmt.Errorf("agent standing: stored link user lookup: %w", err)
		}
		return u != nil, nil
	}
	tryAgent := func(id string) (*store.Agent, error) {
		if id == "" || id == a.ID {
			return nil, nil
		}
		next, err := st.GetAgent(ctx, id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, nil
			}
			return nil, fmt.Errorf("agent standing: stored link agent lookup: %w", err)
		}
		if next == nil || next.ProjectID != projectID {
			return nil, nil
		}
		return next, nil
	}

	if a.OwnerID != "" {
		if ok, err := tryUser(a.OwnerID); err != nil {
			return nil, "", err
		} else if ok {
			return nil, a.OwnerID, nil
		}
		if next, err := tryAgent(a.OwnerID); err != nil {
			return nil, "", err
		} else if next != nil {
			return next, "", nil
		}
	}
	if len(a.Ancestry) > 0 {
		if ok, err := tryUser(a.Ancestry[0]); err != nil {
			return nil, "", err
		} else if ok {
			return nil, a.Ancestry[0], nil
		}
	}
	if a.OwnerID == "" && a.CreatedBy != "" {
		if ok, err := tryUser(a.CreatedBy); err != nil {
			return nil, "", err
		} else if ok {
			return nil, a.CreatedBy, nil
		}
		if next, err := tryAgent(a.CreatedBy); err != nil {
			return nil, "", err
		} else if next != nil {
			return next, "", nil
		}
	}
	return nil, "", denyStanding(standingReasonNoRoot, origAgentID, "")
}

// agentSuspendedConflictMessage is the one refusal a user sees when starting,
// restarting, waking or reincarnating an agent that is held or not in good
// standing.
const agentSuspendedConflictMessage = "This agent is suspended. A project owner can resume it."

// agentSuspendedRefusal is the startGate refusal for an agent that is held
// or not in good standing.
func agentSuspendedRefusal(agentID string) *startRefusal {
	return &startRefusal{
		HTTPStatus: http.StatusConflict,
		Code:       ErrCodeConflict,
		Message:    agentSuspendedConflictMessage,
		Details:    map[string]interface{}{"agentId": agentID},
	}
}

// standingStartRefusal maps an agentStanding result to a startGate refusal:
// nil for good standing, the suspended conflict for a policy refusal, and an
// internal error for a lookup fault.
func standingStartRefusal(agentID string, err error) *startRefusal {
	if err == nil {
		return nil
	}
	if errors.Is(err, errAgentNotInStanding) {
		return agentSuspendedRefusal(agentID)
	}
	return &startRefusal{
		HTTPStatus: http.StatusInternalServerError,
		Code:       ErrCodeInternalError,
		Message:    "could not verify the agent's standing",
	}
}

// scheduledFireStanding refuses a scheduled event whose target agent is held
// or whose authoring agent is not in good standing (ptone/scion#3433).
// target is the message event's target agent, nil for an event with no
// target (dispatch_agent). A user author is checked live by the event's own
// authorization. A message event is refused with the one public scheduled
// message refusal; the specific cause is logged.
func (s *Server) scheduledFireStanding(ctx context.Context, evt store.ScheduledEvent, target *store.Agent) error {
	refuse := func(cause string, err error) error {
		slog.Warn("Scheduler: scheduled event refused at fire time",
			"eventID", evt.ID, "eventType", evt.EventType, "projectID", evt.ProjectID,
			"creator", evt.CreatedBy, "cause", cause, "error", err)
		if target != nil {
			return errScheduledMessageRefused
		}
		return fmt.Errorf("scheduled dispatch refused: %s", cause)
	}
	if target != nil {
		held, err := s.agentHeld(ctx, target.ID)
		if err != nil {
			return refuse("target hold lookup failed", err)
		}
		if held {
			return refuse("target agent is suspended", nil)
		}
	}
	if evt.CreatedBy == "" {
		return nil
	}
	creator, err := s.store.GetAgent(ctx, evt.CreatedBy)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return refuse("author lookup failed", err)
	}
	if creator == nil {
		return nil
	}
	if err := s.agentStanding(ctx, creator.ID); err != nil {
		return refuse("authoring agent is not in good standing", err)
	}
	return nil
}

// ErrAgentNotInStanding is returned by the dispatcher when the standing
// check refuses a start or restart.
var ErrAgentNotInStanding = errors.New("agent is suspended")

// dispatchStandingCheck is the dispatcher's standing check: the agent row is
// re-read so a stale caller copy cannot pass.
func (s *Server) dispatchStandingCheck(ctx context.Context, agent *store.Agent) error {
	if agent == nil {
		return errors.New("dispatch standing: nil agent")
	}
	return s.agentStanding(ctx, agent.ID)
}

// heldTargetDMError is the refusal for a message to a held agent: the same
// code and status as a suspended target.
func heldTargetDMError(agent *store.Agent) *AgentDMError {
	return &AgentDMError{
		Code:       ErrCodeAgentNotRunning,
		Message:    fmt.Sprintf("agent %q is suspended. A project owner can resume it.", agent.Slug),
		HTTPStatus: http.StatusConflict,
	}
}

// agentStandingForbidden runs agentStanding for an agent caller and writes
// the endpoint's generic refusal when it fails: 403 Forbidden for a policy
// refusal (the same response a caller with no access gets), 500 for a
// lookup fault. It reports whether it wrote a response.
func (s *Server) agentStandingForbidden(ctx context.Context, w http.ResponseWriter, agentID string) bool {
	err := s.agentStanding(ctx, agentID)
	if err == nil {
		return false
	}
	if errors.Is(err, errAgentNotInStanding) {
		Forbidden(w)
	} else {
		InternalError(w)
	}
	return true
}

// agentSuspensionView returns the API suspension view for one agent, nil
// when it has no active hold. A lookup error leaves the view out (the
// gates read holds on their own and fail closed).
func (s *Server) agentSuspensionView(ctx context.Context, agentID string) *store.AgentSuspension {
	if s == nil || s.store == nil || agentID == "" {
		return nil
	}
	holds, err := s.store.ListActiveAgentHolds(ctx, agentID)
	if err != nil {
		slog.Warn("agent suspension view: hold lookup failed", "agent_id", agentID, "error", err)
		return nil
	}
	if len(holds) == 0 {
		return nil
	}
	return &store.AgentSuspension{Held: true, Since: holds[0].CreatedAt}
}

// agentSuspensionViews returns the suspension views of agents, keyed by agent
// ID, reading the active holds once per project.
func (s *Server) agentSuspensionViews(ctx context.Context, agents []store.Agent) map[string]*store.AgentSuspension {
	out := map[string]*store.AgentSuspension{}
	if s == nil || s.store == nil || len(agents) == 0 {
		return out
	}
	projects := map[string]bool{}
	for i := range agents {
		if agents[i].ProjectID != "" {
			projects[agents[i].ProjectID] = true
		}
	}
	for p := range projects {
		cursor := ""
		for {
			res, err := s.store.ListActiveAgentHoldsByProject(ctx, p, store.ListOptions{Limit: 500, Cursor: cursor})
			if err != nil {
				slog.Warn("agent suspension view: hold listing failed", "project_id", p, "error", err)
				break
			}
			for _, h := range res.Items {
				if v, ok := out[h.AgentID]; !ok || h.CreatedAt.Before(v.Since) {
					out[h.AgentID] = &store.AgentSuspension{Held: true, Since: h.CreatedAt}
				}
			}
			if res.NextCursor == "" || len(res.Items) == 0 {
				break
			}
			cursor = res.NextCursor
		}
	}
	return out
}

// standingRefusalLeftToItems reports whether a standing refusal concerns the
// chain above the agent, which the per-item delegation checks of a material
// read refuse on their own (a held or deleted link is not live there, and a
// missing or broken link has no authority).
func standingRefusalLeftToItems(err error) bool {
	switch standingReason(err) {
	case standingReasonChainHeld, standingReasonChainDeleted, standingReasonChainBroken,
		standingReasonChainTooDeep, standingReasonRootMissing:
		return true
	}
	return false
}
