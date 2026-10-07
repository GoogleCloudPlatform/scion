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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// Membership loss handling (ptone/scion#3433).
//
// Every path that can end a user's access to a project writes a
// MembershipLossCheck in the same transaction ("re-evaluate this user").
// The processor below evaluates the user's live admission to the project on
// committed state, after taking the project's membership lock. When the
// user is no longer admitted, every agent the user roots in the project
// (the delegation-descendant walk, including agents created by those agents
// and soft-deleted ones) gets a durable hold, its credentials are revoked
// and its run intent is set to stopped, all in one transaction; the
// container stop follows after commit and is retried until it is confirmed.
//
// The reconciler drains the outbox, retries unconfirmed stops, scans for
// expired role bindings and runs a periodic full sweep. None of it is gated
// by a setting.

// membershipLossLease is how long a claimed check stays with its claimer
// (a variable so tests can shorten it).
var membershipLossLease = 2 * time.Minute

const (
	// membershipLossBatch is the number of checks claimed per drain round.
	membershipLossBatch = 20
	// membershipLossMaxDrainRounds bounds one drain call.
	membershipLossMaxDrainRounds = 10
	// membershipLossProcessTimeout bounds the immediate post-commit drain.
	membershipLossProcessTimeout = 60 * time.Second
	// membershipLossMaxWalkRounds bounds the hold-and-continue rounds of one
	// (user, project) evaluation when the walk reaches its node bound.
	membershipLossMaxWalkRounds = 50
	// membershipLossDepthParkAttempts is the number of claims after which a
	// check whose walk keeps reaching the depth bound is parked: completed
	// with an error log and a membership_loss_parked audit record. The
	// agents beyond the bound stay refused by the live standing check, and
	// the full sweep raises the case again every hour.
	membershipLossDepthParkAttempts = 5

	// membershipLossDrainInterval, membershipExpiryScanInterval and
	// membershipFullSweepInterval are the reconciler cadences, in minutes.
	membershipLossDrainInterval  = 1
	membershipExpiryScanInterval = 5
	membershipFullSweepInterval  = 60
	// membershipExpiryWindow is how far back the expiry scan looks; it
	// overlaps successive scans so a missed tick is covered.
	membershipExpiryWindow = 15 * time.Minute
)

// Audit mutation types written by membership loss handling.
const (
	mutationTypeAgentHoldSet               = "agent_hold_set"
	mutationTypeAgentHoldCredentialRevoke  = "agent_credential_revoke"
	mutationTypeMembershipLossProcessed    = "membership_loss_processed"
	mutationTypeMembershipLossParked       = "membership_loss_parked"
	mutationTypeAgentHoldStopDispatched    = "agent_hold_stop_dispatched"
	mutationTypeAgentHoldCleared           = "agent_hold_cleared"
	agentHoldCredentialRevokeReason        = "agent_hold"
	membershipLossSystemActorKind          = "system"
	membershipLossSystemActorID            = "hub"
	membershipLossDepthLimitErrorTextStart = "descendant depth limit"
)

// errMembershipLossDepthLimit marks a walk that reached the depth bound with
// agents still below it: the remaining agents cannot be reached by holding
// more nodes.
var errMembershipLossDepthLimit = errors.New(membershipLossDepthLimitErrorTextStart + " reached")

// descendantQueryBounds are the walk bounds the processor and the reconciler
// use; tests lower them.
var descendantQueryBounds = struct {
	MaxDepth int
	MaxNodes int
}{MaxDepth: store.DefaultDescendantMaxDepth, MaxNodes: store.DefaultDescendantMaxNodes}

// isUUID reports whether id parses as a UUID.
func isUUID(id string) bool {
	_, err := uuid.Parse(id)
	return err == nil
}

// enqueueMembershipLossTx writes a membership loss check for (userID,
// projectID) on tx (projectID empty: every project the user roots agents
// in). A principal ID that is not a UUID cannot root holds and is skipped.
func enqueueMembershipLossTx(ctx context.Context, tx store.Store, userID, projectID string, trigger store.MembershipLossTrigger, actor AuditActor) error {
	if !isUUID(userID) {
		return nil
	}
	if projectID != "" && !isUUID(projectID) {
		return nil
	}
	kind, id := actor.PrincipalKind, actor.PrincipalID
	if kind == "" {
		kind, id = membershipLossSystemActorKind, membershipLossSystemActorID
	}
	check := &store.MembershipLossCheck{
		UserID:        userID,
		ProjectID:     projectID,
		Trigger:       trigger,
		ActorKind:     kind,
		ActorID:       id,
		CorrelationID: actor.CorrelationID,
	}
	if err := tx.EnqueueMembershipLossCheck(ctx, check); err != nil {
		return fmt.Errorf("enqueue membership loss check: %w", err)
	}
	return nil
}

// enqueueMembershipLossForPrincipalTx enqueues checks for a role-binding
// principal: the user itself, or every user that is a transitive member of
// a group principal.
func enqueueMembershipLossForPrincipalTx(ctx context.Context, tx store.Store, principalType, principalID, projectID string, trigger store.MembershipLossTrigger, actor AuditActor) error {
	switch principalType {
	case store.RoleBindingPrincipalUser:
		return enqueueMembershipLossTx(ctx, tx, principalID, projectID, trigger, actor)
	case store.RoleBindingPrincipalGroup:
		users, err := transitiveGroupMemberUsers(ctx, tx, principalID)
		if err != nil {
			return err
		}
		for _, u := range users {
			if err := enqueueMembershipLossTx(ctx, tx, u, projectID, trigger, actor); err != nil {
				return err
			}
		}
	}
	return nil
}

// transitiveGroupMemberUsers returns the user IDs that are members of the
// group directly or through nested groups.
func transitiveGroupMemberUsers(ctx context.Context, st store.Store, groupID string) ([]string, error) {
	const maxGroups = 10000
	seenGroups := map[string]bool{}
	seenUsers := map[string]bool{}
	var users []string
	queue := []string{groupID}
	for len(queue) > 0 {
		g := queue[0]
		queue = queue[1:]
		if seenGroups[g] {
			continue
		}
		seenGroups[g] = true
		if len(seenGroups) > maxGroups {
			return nil, fmt.Errorf("group membership expansion exceeds %d groups", maxGroups)
		}
		members, err := st.GetGroupMembers(ctx, g)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			return nil, fmt.Errorf("expand group %s members: %w", g, err)
		}
		for _, m := range members {
			switch m.MemberType {
			case store.GroupMemberTypeUser:
				if !seenUsers[m.MemberID] {
					seenUsers[m.MemberID] = true
					users = append(users, m.MemberID)
				}
			case store.GroupMemberTypeGroup:
				queue = append(queue, m.MemberID)
			}
		}
	}
	sort.Strings(users)
	return users, nil
}

// kickMembershipLossChecks drains the outbox in the background right after
// a removal commits, bounded by a timeout. Failures stay in the outbox and
// are retried by the reconciler.
func (s *Server) kickMembershipLossChecks() {
	if s == nil || s.store == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), membershipLossProcessTimeout)
		defer cancel()
		s.drainMembershipLossChecks(ctx)
	}()
}

// drainMembershipLossChecks claims and processes claimable checks until the
// outbox has none left or the round bound is reached. It returns the number
// of checks completed.
func (s *Server) drainMembershipLossChecks(ctx context.Context) int {
	completed := 0
	for round := 0; round < membershipLossMaxDrainRounds; round++ {
		checks, err := s.store.ClaimMembershipLossChecks(ctx, membershipLossBatch, membershipLossLease)
		if err != nil {
			slog.Error("membership loss: claim failed", "error", err)
			return completed
		}
		if len(checks) == 0 {
			return completed
		}
		for _, c := range checks {
			if s.processClaimedMembershipLossCheck(ctx, c) {
				completed++
			}
		}
	}
	return completed
}

// processClaimedMembershipLossCheck processes one claimed check and settles
// its claim: completed only when processing returned no error; failed (and
// so claimable again after the lease) otherwise. A lost claim means another
// instance owns the check now: not a failure, nothing to retry here. It
// reports whether the check was completed.
func (s *Server) processClaimedMembershipLossCheck(ctx context.Context, c *store.MembershipLossCheck) bool {
	err := s.processMembershipLossCheck(ctx, c)
	if err == nil {
		return s.completeMembershipLossCheck(ctx, c)
	}
	if errors.Is(err, errMembershipLossDepthLimit) && c.Attempts >= membershipLossDepthParkAttempts {
		// Permanent: holding more nodes cannot reach the agents below the
		// depth bound. Park the check visibly instead of retrying it
		// silently forever; the live standing check keeps refusing those
		// agents and the full sweep raises the case again.
		slog.Error("membership loss: check parked after repeated depth limit",
			"check_id", c.ID, "user_id", c.UserID, "project_id", c.ProjectID,
			"attempts", c.Attempts, "error", err)
		s.writeMembershipLossAudit(ctx, s.store, c, mutationTypeMembershipLossParked, "user", c.UserID, map[string]interface{}{
			"project_id": c.ProjectID, "attempts": c.Attempts, "depth_limit": true, "trigger": string(c.Trigger),
		})
		return s.completeMembershipLossCheck(ctx, c)
	}
	slog.Error("membership loss: check failed; it will be retried",
		"check_id", c.ID, "user_id", c.UserID, "project_id", c.ProjectID,
		"attempts", c.Attempts, "error", err)
	if ferr := s.store.FailMembershipLossCheck(ctx, c.ID, c.Attempts, err.Error()); ferr != nil && !errors.Is(ferr, store.ErrClaimLost) {
		slog.Error("membership loss: recording the failure failed", "check_id", c.ID, "error", ferr)
	}
	return false
}

func (s *Server) completeMembershipLossCheck(ctx context.Context, c *store.MembershipLossCheck) bool {
	err := s.store.CompleteMembershipLossCheck(ctx, c.ID, c.Attempts)
	if err == nil {
		return true
	}
	if errors.Is(err, store.ErrClaimLost) {
		// Another instance claimed the check after our lease expired; it
		// owns the outcome now.
		slog.Info("membership loss: claim lost on completion; another instance owns the check", "check_id", c.ID)
		return false
	}
	slog.Error("membership loss: completing the check failed", "check_id", c.ID, "error", err)
	return false
}

// processMembershipLossCheck evaluates one check. A nil return means every
// (user, project) pair it names was fully evaluated and, where the user is
// no longer admitted, every agent the user roots there is held.
func (s *Server) processMembershipLossCheck(ctx context.Context, c *store.MembershipLossCheck) error {
	if c == nil || c.UserID == "" {
		return nil
	}
	projects := []string{c.ProjectID}
	if c.ProjectID == "" {
		var err error
		projects, err = s.projectsRootedByUser(ctx, c.UserID)
		if err != nil {
			return err
		}
	}
	var errs []error
	for _, p := range projects {
		if err := s.processMembershipLossPair(ctx, c, c.UserID, p); err != nil {
			errs = append(errs, fmt.Errorf("project %s: %w", p, err))
		}
	}
	return errors.Join(errs...)
}

// projectsRootedByUser returns the projects in which userID delegates to an
// agent or is recorded as an agent's owner or creator.
func (s *Server) projectsRootedByUser(ctx context.Context, userID string) ([]string, error) {
	set := map[string]bool{}
	edges, err := s.store.GetDelegationEdgesForDelegator(ctx, store.DelegationPrincipalUser, userID)
	if err != nil {
		return nil, fmt.Errorf("list user delegation edges: %w", err)
	}
	for _, e := range edges {
		if e.DelegateType == store.DelegationPrincipalAgent && e.ScopeType == store.RoleScopeProject && e.ScopeID != "" {
			set[e.ScopeID] = true
		}
	}
	for _, f := range []store.AgentFilter{
		{OwnerID: userID, IncludeDeleted: true},
		{CreatedBy: userID, IncludeDeleted: true},
	} {
		if err := s.forEachAgent(ctx, f, func(a *store.Agent) error {
			if a.ProjectID != "" {
				set[a.ProjectID] = true
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

// forEachAgent pages through the agents matching filter.
func (s *Server) forEachAgent(ctx context.Context, filter store.AgentFilter, fn func(*store.Agent) error) error {
	cursor := ""
	for {
		res, err := s.store.ListAgents(ctx, filter, store.ListOptions{Limit: 500, Cursor: cursor, SkipTotalCount: true})
		if err != nil {
			return fmt.Errorf("list agents: %w", err)
		}
		for i := range res.Items {
			if err := fn(&res.Items[i]); err != nil {
				return err
			}
		}
		if res.NextCursor == "" || len(res.Items) == 0 {
			return nil
		}
		cursor = res.NextCursor
	}
}

// membershipLossPairOutcome summarises one processed (user, project) pair.
type membershipLossPairOutcome struct {
	admitted bool
	held     []string // agent IDs newly held, in hold order
	stop     []string // newly held agents that are not soft-deleted
}

// processMembershipLossPair evaluates userID's admission to projectID and,
// when it has ended, holds every agent the user roots in the project. The
// walk skips agents already held for this root, so when it reaches its node
// bound the found agents are held and the walk runs again, further into the
// tree; a tree of any size is eventually fully held. Reaching the depth
// bound returns errMembershipLossDepthLimit after holding what was found.
func (s *Server) processMembershipLossPair(ctx context.Context, c *store.MembershipLossCheck, userID, projectID string) error {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// The project is gone and its agents with it: nothing to hold.
			return nil
		}
		return fmt.Errorf("project lookup: %w", err)
	}

	var allStop []string
	defer func() {
		// Post-commit: stop every newly held agent's container. Failures
		// are retried by the reconciler (the hold is already durable).
		for _, id := range allStop {
			s.recordHeldRunIntent(ctx, id)
			_ = s.stopHeldAgent(ctx, id, 1)
		}
	}()

	for round := 0; round < membershipLossMaxWalkRounds; round++ {
		outcome, walkErr, err := s.membershipLossRound(ctx, c, userID, projectID)
		if err != nil {
			return err
		}
		allStop = append(allStop, outcome.stop...)
		if outcome.admitted {
			return nil
		}
		if walkErr == nil {
			return nil
		}
		if errors.Is(walkErr, store.ErrDescendantLimit) {
			if len(outcome.held) == 0 {
				slog.Error("membership loss: descendant walk reached its depth bound; agents below it are refused live but not held",
					"check_id", c.ID, "user_id", userID, "project_id", projectID)
				return errMembershipLossDepthLimit
			}
			slog.Warn("membership loss: descendant walk reached its node bound; held the agents found and continuing",
				"check_id", c.ID, "user_id", userID, "project_id", projectID, "held", len(outcome.held))
			continue
		}
		return walkErr
	}
	return fmt.Errorf("descendant walk still incomplete after %d rounds", membershipLossMaxWalkRounds)
}

// membershipLossRound runs one transaction: lock the project, evaluate the
// user's admission, walk and hold. walkErr is the walk's error when the
// agents it did return were held (ErrDescendantLimit); err aborts the round
// with nothing written.
func (s *Server) membershipLossRound(ctx context.Context, c *store.MembershipLossCheck, userID, projectID string) (membershipLossPairOutcome, error, error) {
	var outcome membershipLossPairOutcome
	var walkErr error
	txErr := s.store.WithTx(ctx, func(tx store.Store) error {
		outcome = membershipLossPairOutcome{}
		walkErr = nil
		if err := tx.LockProjectForMembership(ctx, projectID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				outcome.admitted = true // project gone: nothing to do
				return nil
			}
			return fmt.Errorf("lock project: %w", err)
		}
		// Read after the lock, through the transaction: no membership
		// write on the project is in flight.
		admitted, err := s.userAdmittedByIDOn(ctx, tx, userID, projectID)
		if err != nil {
			return fmt.Errorf("admission check: %w", err)
		}
		if admitted {
			outcome.admitted = true
			s.writeMembershipLossAudit(ctx, tx, c, mutationTypeMembershipLossProcessed, "user", userID, map[string]interface{}{
				"project_id": projectID, "admitted": true, "held": 0, "trigger": string(c.Trigger),
			})
			return nil
		}

		res, werr := tx.ListDelegationDescendants(ctx, store.DescendantQuery{
			RootType:           store.DelegationPrincipalUser,
			RootID:             userID,
			ProjectID:          projectID,
			IncludeSoftDeleted: true,
			LegacyLinks:        true,
			SkipHeldForRoot:    true,
			MaxDepth:           descendantQueryBounds.MaxDepth,
			MaxNodes:           descendantQueryBounds.MaxNodes,
		})
		if werr != nil && !errors.Is(werr, store.ErrDescendantLimit) {
			return fmt.Errorf("descendant walk: %w", werr)
		}
		walkErr = werr
		held, stop, err := s.holdAgentsTx(ctx, tx, c, userID, projectID, res.Agents)
		if err != nil {
			return err
		}
		outcome.held, outcome.stop = held, stop
		s.writeMembershipLossAudit(ctx, tx, c, mutationTypeMembershipLossProcessed, "user", userID, map[string]interface{}{
			"project_id": projectID, "admitted": false, "held": len(held),
			"already_held_skipped": true, "truncated": werr != nil, "trigger": string(c.Trigger),
		})
		return nil
	})
	if txErr != nil {
		return membershipLossPairOutcome{}, nil, txErr
	}
	slog.Info("membership loss: processed",
		"check_id", c.ID, "user_id", userID, "project_id", projectID,
		"admitted", outcome.admitted, "held", len(outcome.held), "walk_incomplete", walkErr != nil)
	return outcome, walkErr, nil
}

// userAdmittedByID reports whether userID names an existing user admitted to
// projectID. A missing user is not admitted.
func (s *Server) userAdmittedByID(ctx context.Context, userID, projectID string) (bool, error) {
	return s.userAdmittedByIDOn(ctx, s.store, userID, projectID)
}

// userAdmittedByIDOn is userAdmittedByID reading through st. Inside a
// transaction st is the transaction store: on PostgreSQL (READ COMMITTED)
// each read sees the state committed before it, and on SQLite the
// transaction's own connection is the only one.
func (s *Server) userAdmittedByIDOn(ctx context.Context, st store.Store, userID, projectID string) (bool, error) {
	user, err := st.GetUser(ctx, userID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	if user == nil {
		return false, nil
	}
	return s.userAdmittedToProjectOn(ctx, st, user, projectID, nil)
}

// holdAgentsTx holds refs for root userID inside tx: agent rows locked in
// ascending ID order, holds inserted, credentials revoked and run intent set
// to stopped for live agents, with one audit record per agent. It returns
// the IDs held and those of them to stop after commit.
func (s *Server) holdAgentsTx(ctx context.Context, tx store.Store, c *store.MembershipLossCheck, userID, projectID string, refs []store.DescendantRef) ([]string, []string, error) {
	if len(refs) == 0 {
		return nil, nil, nil
	}
	ids := make([]string, 0, len(refs))
	byID := make(map[string]store.DescendantRef, len(refs))
	for _, r := range refs {
		ids = append(ids, r.AgentID)
		byID[r.AgentID] = r
	}
	sort.Strings(ids)
	if err := tx.LockAgentRows(ctx, ids); err != nil {
		return nil, nil, fmt.Errorf("lock agent rows: %w", err)
	}

	holds := make([]*store.AgentHold, 0, len(ids))
	for _, id := range ids {
		r := byID[id]
		holds = append(holds, &store.AgentHold{
			AgentID:           id,
			ProjectID:         projectID,
			Cause:             store.AgentHoldCauseOwnerAccessEnded,
			RootPrincipalType: store.AgentHoldRootUser,
			RootPrincipalID:   userID,
			ViaAgentID:        r.ViaID,
			Trigger:           c.Trigger,
			ActorKind:         c.ActorKind,
			ActorID:           c.ActorID,
			CorrelationID:     c.ID,
		})
	}
	if _, err := tx.CreateAgentHolds(ctx, holds); err != nil {
		return nil, nil, fmt.Errorf("create agent holds: %w", err)
	}

	var stop []string
	for _, id := range ids {
		r := byID[id]
		a, err := tx.GetAgent(ctx, id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			return nil, nil, fmt.Errorf("agent lookup: %w", err)
		}
		softDeleted := !a.DeletedAt.IsZero()
		revoked, err := tx.RevokeAgentCredentialsByAgent(ctx, id, membershipLossSystemActorKind, agentHoldCredentialRevokeReason)
		if err != nil {
			return nil, nil, fmt.Errorf("revoke agent credentials: %w", err)
		}
		if !softDeleted {
			// The run intent is recorded right after commit (the run-intent
			// writer runs on the store's own connection, not inside a
			// transaction); until then the hold already refuses every
			// start.
			stop = append(stop, id)
		}
		s.writeMembershipLossAudit(ctx, tx, c, mutationTypeAgentHoldSet, "agent", id, map[string]interface{}{
			"reason":       string(store.AgentHoldCauseOwnerAccessEnded),
			"trigger":      string(c.Trigger),
			"root":         "user:" + userID,
			"project_id":   projectID,
			"via_agent_id": r.ViaID,
			"depth":        r.Depth,
			"link":         string(r.Link),
			"soft_deleted": softDeleted,
		})
		s.writeMembershipLossAudit(ctx, tx, c, mutationTypeAgentHoldCredentialRevoke, "agent_credential", id, map[string]interface{}{
			"revoked": revoked, "reason": agentHoldCredentialRevokeReason,
		})
	}
	return ids, stop, nil
}

// writeMembershipLossAudit writes an audit record on st with the check's
// actor and correlation. Inside a transaction a write failure aborts it
// through the caller's later writes; it is logged here.
func (s *Server) writeMembershipLossAudit(ctx context.Context, st store.Store, c *store.MembershipLossCheck, mutationType, targetType, targetID string, after map[string]interface{}) {
	summary, _ := json.Marshal(after)
	record := &store.MutationAuditRecord{
		MutationType:       mutationType,
		TargetType:         targetType,
		TargetID:           targetID,
		AfterSummary:       string(summary),
		ActorPrincipalKind: c.ActorKind,
		ActorPrincipalID:   c.ActorID,
		CorrelationID:      c.ID,
		Timestamp:          time.Now(),
	}
	applyHubActorFallback(record)
	if err := st.CreateMutationAudit(ctx, record); err != nil {
		slog.Error("membership loss: audit write failed", "mutation_type", mutationType, "target_id", targetID, "error", err)
	}
}

// recordHeldRunIntent sets a newly held agent's run intent to stopped, so
// nothing tries to bring it back up. A failure is logged; the hold refuses
// every start regardless, and the stop retry records the intent again.
func (s *Server) recordHeldRunIntent(ctx context.Context, agentID string) {
	if _, _, err := s.store.SwapRunIntent(ctx, agentID, store.RunIntentStopped); err != nil &&
		!errors.Is(err, store.ErrDeleteInProgress) && !errors.Is(err, store.ErrNotFound) {
		slog.Warn("membership loss: recording the stopped run intent of a held agent failed",
			"agent_id", agentID, "error", err)
	}
}

// heldAgentNeedsStop reports whether a held agent still has a container the
// hub should stop: its run intent is stopped and its phase is not one in
// which no container runs.
func heldAgentNeedsStop(a *store.Agent) bool {
	if a == nil || !a.DeletedAt.IsZero() || a.RuntimeBrokerID == "" {
		return false
	}
	switch state.Phase(a.Phase) {
	case state.PhaseSuspended, state.PhaseStopped, state.PhaseCreated, state.PhaseError:
		return false
	}
	return true
}

// stopHeldAgent stops the container of a held agent. The phase moves to
// suspended (stopped when the harness cannot resume) only when the stop is
// confirmed; a failure is logged and audited and the reconciler retries.
func (s *Server) stopHeldAgent(ctx context.Context, agentID string, attempt int) error {
	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	if !heldAgentNeedsStop(agent) {
		return nil
	}
	dispatcher := s.GetDispatcher()
	if dispatcher == nil {
		slog.Warn("membership loss: held agent still running; no dispatcher to stop it, will retry",
			"agent_id", agentID, "attempt", attempt)
		return errors.New("no dispatcher")
	}

	defer s.beginLifecycleOp(agent.ID)()
	if _, err := s.recordRunIntent(ctx, agent, store.RunIntentStopped); err != nil {
		slog.Warn("membership loss: recording run intent before the stop failed; will retry",
			"agent_id", agentID, "attempt", attempt, "error", err)
		return err
	}
	stopRunID := agent.RunID
	stopErr := dispatcher.DispatchAgentStop(ctx, agent)
	result := "ok"
	if stopErr != nil {
		result = "failed"
	}
	s.emitMutationAudit(ctx, &store.MutationAuditRecord{
		MutationType:       mutationTypeAgentHoldStopDispatched,
		TargetType:         "agent",
		TargetID:           agent.ID,
		AfterSummary:       fmt.Sprintf(`{"result":%q,"attempt":%d}`, result, attempt),
		ActorPrincipalKind: membershipLossSystemActorKind,
		ActorPrincipalID:   membershipLossSystemActorID,
		Timestamp:          time.Now(),
	})
	if stopErr != nil {
		slog.Warn("membership loss: stopping a held agent failed; the hold and credential revoke are in effect and the stop will be retried",
			"agent_id", agentID, "attempt", attempt, "error", stopErr)
		return stopErr
	}

	newPhase := string(state.PhaseSuspended)
	if ok, _ := s.harnessSupportsResume(agent); !ok {
		newPhase = string(state.PhaseStopped)
	}
	recorded, err := s.recordStopStatus(ctx, agent.ID, stopRunID, "hold", store.AgentStatusUpdate{
		Phase:           newPhase,
		ContainerStatus: "stopped",
		Activity:        "",
	})
	if err != nil {
		return err
	}
	if !recorded {
		return nil
	}
	agent.Phase = newPhase
	agent.ContainerStatus = "stopped"
	agent.Activity = ""
	s.releaseBrokerQuota(ctx, agent)
	if s.events != nil {
		s.events.PublishAgentStatus(ctx, agent)
	}
	return nil
}

// heldStopAttempts counts stop retries per agent within this process, for
// the audit record and logs.
var heldStopAttempts sync.Map

// retryHeldAgentStops retries the stop of every held agent whose container
// may still be running.
func (s *Server) retryHeldAgentStops(ctx context.Context) {
	err := s.forEachActiveHoldAgent(ctx, func(agentID string) error {
		agent, err := s.store.GetAgent(ctx, agentID)
		if err != nil || !heldAgentNeedsStop(agent) {
			heldStopAttempts.Delete(agentID)
			return nil
		}
		n := 1
		if v, ok := heldStopAttempts.Load(agentID); ok {
			n = v.(int) + 1
		}
		heldStopAttempts.Store(agentID, n)
		if err := s.stopHeldAgent(ctx, agentID, n); err == nil {
			heldStopAttempts.Delete(agentID)
		}
		return nil
	})
	if err != nil {
		slog.Error("membership loss: listing held agents for stop retry failed", "error", err)
	}
}

// forEachActiveHoldAgent calls fn once per agent with an active hold, project
// by project.
func (s *Server) forEachActiveHoldAgent(ctx context.Context, fn func(agentID string) error) error {
	projects, err := s.allProjectIDs(ctx)
	if err != nil {
		return err
	}
	for _, p := range projects {
		seen := map[string]bool{}
		cursor := ""
		for {
			res, err := s.store.ListActiveAgentHoldsByProject(ctx, p, store.ListOptions{Limit: 500, Cursor: cursor})
			if err != nil {
				return err
			}
			for _, h := range res.Items {
				if seen[h.AgentID] {
					continue
				}
				seen[h.AgentID] = true
				if err := fn(h.AgentID); err != nil {
					return err
				}
			}
			if res.NextCursor == "" || len(res.Items) == 0 {
				break
			}
			cursor = res.NextCursor
		}
	}
	return nil
}

// allProjectIDs returns every project ID.
func (s *Server) allProjectIDs(ctx context.Context) ([]string, error) {
	var out []string
	cursor := ""
	for {
		res, err := s.store.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{Limit: 500, Cursor: cursor, SkipTotalCount: true})
		if err != nil {
			return nil, fmt.Errorf("list projects: %w", err)
		}
		for _, p := range res.Items {
			out = append(out, p.ID)
		}
		if res.NextCursor == "" || len(res.Items) == 0 {
			return out, nil
		}
		cursor = res.NextCursor
	}
}

// ----------------------------------------------------------------------------
// Reconciler
// ----------------------------------------------------------------------------

// registerMembershipStandingReconciler registers the reconciler's recurring
// tasks. They read no setting: enforcement cannot be switched off.
func (s *Server) registerMembershipStandingReconciler() {
	// Draining is safe on every instance: claims skip rows another
	// instance holds.
	s.scheduler.RegisterRecurring("membership-loss-drain", membershipLossDrainInterval, func(ctx context.Context) {
		s.drainMembershipLossChecks(ctx)
		s.retryHeldAgentStops(ctx)
	})
	s.scheduler.RegisterRecurringSingleton("membership-expiry-scan", membershipExpiryScanInterval,
		store.LockMembershipStandingSweep, func(ctx context.Context) {
			if err := s.membershipExpiryScan(ctx, time.Now()); err != nil {
				slog.Error("membership standing: expiry scan failed", "error", err)
			}
		})
	s.scheduler.RegisterRecurringSingleton("membership-standing-sweep", membershipFullSweepInterval,
		store.LockMembershipStandingSweep, func(ctx context.Context) {
			if _, err := s.membershipFullSweep(ctx); err != nil {
				slog.Error("membership standing: full sweep failed", "error", err)
			}
		})
}

// membershipExpiryScan enqueues a check for every principal whose project
// role binding expired within membershipExpiryWindow before now.
func (s *Server) membershipExpiryScan(ctx context.Context, now time.Time) error {
	const page = 1000
	from := now.Add(-membershipExpiryWindow)
	var expired []*store.RoleBinding
	for offset := 0; ; offset += page {
		rbs, err := s.store.ListAllRoleBindings(ctx, store.RoleBindingListOptions{Limit: page, Offset: offset, SortOrder: "asc"})
		if err != nil {
			return fmt.Errorf("list role bindings: %w", err)
		}
		for _, rb := range rbs {
			if rb.ExpiresAt == nil || rb.ScopeType != store.RoleScopeProject {
				continue
			}
			if rb.ExpiresAt.After(from) && !rb.ExpiresAt.After(now) {
				expired = append(expired, rb)
			}
		}
		if len(rbs) < page {
			break
		}
	}
	if len(expired) == 0 {
		return nil
	}
	actor := AuditActor{PrincipalKind: membershipLossSystemActorKind, PrincipalID: membershipLossSystemActorID}
	err := s.store.WithTx(ctx, func(tx store.Store) error {
		for _, rb := range expired {
			if err := enqueueMembershipLossForPrincipalTx(ctx, tx, rb.PrincipalType, rb.PrincipalID, rb.ScopeID, store.MembershipLossTriggerBindingExpiry, actor); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.drainMembershipLossChecks(ctx)
	return nil
}

// membershipSweepResult is what one full sweep found.
type membershipSweepResult struct {
	// Pairs is the number of (root user, project) pairs evaluated.
	Pairs int
	// NotAdmittedPairs is the number of pairs whose user is no longer
	// admitted to the project.
	NotAdmittedPairs int
	// WouldHold is the number of agents not yet held that the sweep's
	// checks will hold (agents rooted at a user who is not admitted).
	WouldHold int
	// WalkIncomplete is the number of pairs whose count is a lower bound
	// because the walk reached a bound.
	WalkIncomplete int
	// NoResolvableRoot is the number of live agents with no resolvable
	// root user. They cannot be held (a hold names its root) and are
	// refused live at every standing site.
	NoResolvableRoot int
	// Enqueued is the number of checks enqueued.
	Enqueued int
}

// membershipSweepFirst makes the first sweep of a process the measured one.
var membershipSweepFirst sync.Once

// membershipFullSweep finds every (root user, project) pair from the agents'
// chains, evaluates the user's admission (memoised per pair), counts the
// agents that would be held, logs the counts, then enqueues a check for each
// non-admitted pair with agents still to hold and drains the outbox.
func (s *Server) membershipFullSweep(ctx context.Context) (membershipSweepResult, error) {
	var res membershipSweepResult
	pairs := map[[2]string]bool{}
	err := s.forEachAgent(ctx, store.AgentFilter{IncludeDeleted: true}, func(a *store.Agent) error {
		if a.ProjectID == "" {
			return nil
		}
		root, err := s.resolveChainRoot(ctx, s.store, a, false)
		if err != nil {
			if errors.Is(err, errAgentNotInStanding) {
				if a.DeletedAt.IsZero() {
					res.NoResolvableRoot++
				}
				return nil
			}
			return err
		}
		pairs[[2]string{root, a.ProjectID}] = true
		// The owner and creator of an agent that is reached by an edge
		// from someone else also root it through the legacy links.
		for _, u := range []string{a.OwnerID, a.CreatedBy} {
			if u != "" && u != root && isUUID(u) {
				if _, uerr := s.store.GetUser(ctx, u); uerr == nil {
					pairs[[2]string{u, a.ProjectID}] = true
				}
			}
		}
		return nil
	})
	if err != nil {
		return res, err
	}

	keys := make([][2]string, 0, len(pairs))
	for k := range pairs {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	})

	var toEnqueue [][2]string
	for _, k := range keys {
		res.Pairs++
		admitted, err := s.userAdmittedByID(ctx, k[0], k[1])
		if err != nil {
			return res, fmt.Errorf("admission check for user %s in project %s: %w", k[0], k[1], err)
		}
		if admitted {
			continue
		}
		res.NotAdmittedPairs++
		walk, werr := s.store.ListDelegationDescendants(ctx, store.DescendantQuery{
			RootType:           store.DelegationPrincipalUser,
			RootID:             k[0],
			ProjectID:          k[1],
			IncludeSoftDeleted: true,
			LegacyLinks:        true,
			SkipHeldForRoot:    true,
			MaxDepth:           descendantQueryBounds.MaxDepth,
			MaxNodes:           descendantQueryBounds.MaxNodes,
		})
		if werr != nil && !errors.Is(werr, store.ErrDescendantLimit) {
			return res, fmt.Errorf("descendant walk for user %s in project %s: %w", k[0], k[1], werr)
		}
		res.WouldHold += len(walk.Agents)
		if werr != nil {
			res.WalkIncomplete++
		}
		if len(walk.Agents) > 0 || werr != nil {
			toEnqueue = append(toEnqueue, k)
		}
	}

	first := false
	membershipSweepFirst.Do(func() { first = true })
	logArgs := []interface{}{
		"pairs", res.Pairs,
		"not_admitted_pairs", res.NotAdmittedPairs,
		"agents_to_hold", res.WouldHold,
		"walks_incomplete", res.WalkIncomplete,
		"agents_without_resolvable_root", res.NoResolvableRoot,
		"first_sweep_since_start", first,
	}
	if first || res.WouldHold > 0 || res.NoResolvableRoot > 0 {
		slog.Info("membership standing sweep: measured before holding", logArgs...)
	} else {
		slog.Debug("membership standing sweep: measured before holding", logArgs...)
	}

	if len(toEnqueue) > 0 {
		actor := AuditActor{PrincipalKind: membershipLossSystemActorKind, PrincipalID: membershipLossSystemActorID}
		err := s.store.WithTx(ctx, func(tx store.Store) error {
			for _, k := range toEnqueue {
				if err := enqueueMembershipLossTx(ctx, tx, k[0], k[1], store.MembershipLossTriggerReconcile, actor); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return res, err
		}
		res.Enqueued = len(toEnqueue)
	}
	s.drainMembershipLossChecks(ctx)
	return res, nil
}

// ----------------------------------------------------------------------------
// Restore hook
// ----------------------------------------------------------------------------

// membershipRestoreHookName names the restore hook that holds a restored
// agent whose root user is no longer admitted.
const membershipRestoreHookName = "membership-standing-restore-check"

// registerMembershipRestoreHook registers the restore hook once per server.
func (s *Server) registerMembershipRestoreHook() {
	s.RegisterRestoreHook(membershipRestoreHookName, s.membershipRestoreHook)
}

// membershipRestoreHook runs inside the restore transaction: when the
// restored agent has no active hold and the user its chain is rooted at is
// not admitted to its project, it inserts a hold. The restore itself
// proceeds; a later start is refused by the start gate. An agent with no
// resolvable root is not held here (a hold names its root); it is refused
// live by the standing check.
func (s *Server) membershipRestoreHook(ctx context.Context, tx store.Store, agent *store.Agent, actor AuditActor) error {
	if agent == nil || agent.ProjectID == "" {
		return nil
	}
	held, err := tx.HasActiveAgentHold(ctx, agent.ID)
	if err != nil {
		return fmt.Errorf("restore standing check: hold lookup: %w", err)
	}
	if held {
		return nil
	}
	root, err := s.resolveChainRoot(ctx, tx, agent, false)
	if err != nil {
		if errors.Is(err, errAgentNotInStanding) {
			return nil
		}
		return fmt.Errorf("restore standing check: root lookup: %w", err)
	}
	admitted, err := s.userAdmittedByIDOn(ctx, tx, root, agent.ProjectID)
	if err != nil {
		return fmt.Errorf("restore standing check: admission: %w", err)
	}
	if admitted {
		return nil
	}
	kind, id := actor.PrincipalKind, actor.PrincipalID
	if kind == "" {
		kind, id = membershipLossSystemActorKind, membershipLossSystemActorID
	}
	c := &store.MembershipLossCheck{
		ID:        uuid.NewString(),
		UserID:    root,
		ProjectID: agent.ProjectID,
		Trigger:   store.MembershipLossTriggerRestoreCheck,
		ActorKind: kind,
		ActorID:   id,
	}
	if _, err := tx.CreateAgentHolds(ctx, []*store.AgentHold{{
		AgentID:           agent.ID,
		ProjectID:         agent.ProjectID,
		Cause:             store.AgentHoldCauseOwnerAccessEnded,
		RootPrincipalType: store.AgentHoldRootUser,
		RootPrincipalID:   root,
		Trigger:           store.MembershipLossTriggerRestoreCheck,
		ActorKind:         kind,
		ActorID:           id,
		CorrelationID:     c.ID,
	}}); err != nil {
		return fmt.Errorf("restore standing check: create hold: %w", err)
	}
	if _, err := tx.RevokeAgentCredentialsByAgent(ctx, agent.ID, membershipLossSystemActorKind, agentHoldCredentialRevokeReason); err != nil {
		return fmt.Errorf("restore standing check: revoke credentials: %w", err)
	}
	s.writeMembershipLossAudit(ctx, tx, c, mutationTypeAgentHoldSet, "agent", agent.ID, map[string]interface{}{
		"reason":     string(store.AgentHoldCauseOwnerAccessEnded),
		"trigger":    string(store.MembershipLossTriggerRestoreCheck),
		"root":       "user:" + root,
		"project_id": agent.ProjectID,
	})
	return nil
}

// enqueueGroupChangeLoss enqueues a check (every project) for each user
// after a group change committed, then processes them right away. The group
// paths have no transaction of their own, so this runs as a separate
// statement after the change; if it fails, the full sweep catches the users
// and the live standing check refuses their agents meanwhile.
func (s *Server) enqueueGroupChangeLoss(ctx context.Context, users []string) {
	if len(users) == 0 {
		return
	}
	actor := auditActorFromContext(ctx)
	err := s.store.WithTx(ctx, func(tx store.Store) error {
		for _, u := range users {
			if err := enqueueMembershipLossTx(ctx, tx, u, "", store.MembershipLossTriggerGroupChange, actor); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		slog.Error("membership loss: enqueue after group change failed; the full sweep will re-evaluate",
			"users", len(users), "error", err)
		return
	}
	s.kickMembershipLossChecks()
}
