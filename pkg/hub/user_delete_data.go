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
	"sort"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// Agents, delegations and user-scoped data of a deleted user
// (ptone/scion#2769, ptone/scion#2143).
//
// A user cannot be deleted while agents or active delegations depend on the
// user: both delete paths (DELETE /api/v1/users/{id} and the deprecated
// allow-list delete) refuse with 409 user_delete_blocked and list the
// blockers in details.agents and details.edges, at most
// userDeleteBlockersLimit of each, with details.truncated set when more
// exist. GET /api/v1/users/{id}/delete-blockers returns the same lists
// without attempting the delete. On PostgreSQL the delete locks the user
// row (FOR UPDATE) before the check, and agent create and restore lock it
// shared (FOR KEY SHARE) and re-check that the user exists, so a create
// racing a delete either commits first (and the delete sees the agent) or
// fails. A delete that passes the check deactivates every edge with the
// user as the delegator (cause delegator_deleted) in the same
// transaction. After a delete commits, the user's user-scope secrets and
// env vars are removed as a best effort, and a startup sweep removes any
// left behind for users that do not exist.

// userDeleteBlockersLimit caps each list (agents, edges) in a refusal or a
// blocker listing. The check itself reads every blocker. It is a
// variable so tests can lower it.
var userDeleteBlockersLimit = 50

// ownedAgentRef identifies an agent that blocks the deletion of its owner.
// DeletionState is the agent's delete state ("" when no delete was asked
// for, or deleting, finalizing or failed), so a caller can tell an agent
// delete that is already under way.
type ownedAgentRef struct {
	ID            string `json:"id"`
	Slug          string `json:"slug"`
	ProjectID     string `json:"projectId"`
	DeletionState string `json:"deletionState"`
}

// delegatorEdgeRef identifies an active delegation edge, with the user as
// the delegator, that blocks the user's deletion.
type delegatorEdgeRef struct {
	ID           string `json:"id"`
	DelegateType string `json:"delegateType"`
	DelegateID   string `json:"delegateId"`
	ScopeType    string `json:"scopeType"`
	ScopeID      string `json:"scopeId"`
}

// userDeleteBlockers lists what blocks the deletion of a user. It is the
// body of GET /api/v1/users/{id}/delete-blockers and the details of the 409
// user_delete_blocked refusal. Agents and Edges are never nil, so they
// encode as [] when empty.
type userDeleteBlockers struct {
	Agents    []ownedAgentRef    `json:"agents"`
	Edges     []delegatorEdgeRef `json:"edges"`
	Truncated bool               `json:"truncated"`
}

func (b *userDeleteBlockers) empty() bool {
	return len(b.Agents) == 0 && len(b.Edges) == 0
}

// userDeleteBlockedError is returned by checkUserDeleteBlockersTx when
// agents or active delegations block the deletion. The surrounding
// transaction rolls back, so nothing is changed.
type userDeleteBlockedError struct {
	blockers userDeleteBlockers
}

func (e *userDeleteBlockedError) Error() string {
	if len(e.blockers.Agents) == 0 {
		return userHasDelegationsDeleteMessage
	}
	return userOwnsAgentsDeleteMessage
}

const userOwnsAgentsDeleteMessage = "cannot delete a user who owns agents — delete their agents first, including agents started by those agents or by the user's schedules"

// userHasDelegationsDeleteMessage is the message when only edges block: the
// user delegates to agents that exist.
const userHasDelegationsDeleteMessage = "cannot delete a user who has active delegations — delete the agents the user delegates to first"

// writeUserDeleteBlockedError writes the 409 user_delete_blocked response
// for a user deletion refused because agents or active delegations depend
// on the user; details carries the capped lists.
func writeUserDeleteBlockedError(w http.ResponseWriter, e *userDeleteBlockedError) {
	writeError(w, http.StatusConflict, ErrCodeUserDeleteBlocked, e.Error(),
		map[string]interface{}{
			"agents":    e.blockers.Agents,
			"edges":     e.blockers.Edges,
			"truncated": e.blockers.Truncated,
		})
}

// ownedAgentsPageSize is the page size used when listing a user's agents. It
// is a variable so tests can force several pages.
var ownedAgentsPageSize = 100

// checkUserDeleteBlockersTx refuses the deletion of userID while agents or
// active delegations depend on the user (listUserDeleteBlockers). It runs
// inside the delete transaction.
//
// It first locks the user row exclusively (SELECT ... FOR UPDATE on
// PostgreSQL; see store.UserStore.LockUserRow). Agent create (including the
// scheduler's) and restore take a shared lock on that user's row in their
// own transactions and re-check that the user exists (lockAgentGuardUserTx),
// so an agent this check would count either commits before it runs (and is
// listed) or waits for the delete and then fails. It returns
// store.ErrNotFound if the user does not exist, a
// *userDeletionInProgressError while a forced deletion of the user is in
// progress (its status is deleting, ptone/scion#2143), and a
// *userDeleteBlockedError when anything blocks.
func checkUserDeleteBlockersTx(ctx context.Context, tx store.Store, userID string) error {
	return checkUserDeleteBlockersModeTx(ctx, tx, userID, false)
}

// checkUserDeleteBlockersModeTx is checkUserDeleteBlockersTx. completing is
// set only by the completion of a forced deletion, which is the one delete
// that accepts a user whose status is deleting.
func checkUserDeleteBlockersModeTx(ctx context.Context, tx store.Store, userID string, completing bool) error {
	// agents.owner_id is a UUID column, so a user ID that is not a UUID
	// cannot own an agent, and it names no user row to lock.
	if _, err := uuid.Parse(userID); err == nil {
		if err := tx.LockUserRow(ctx, userID, true); err != nil {
			return err
		}
		if !completing {
			u, err := tx.GetUser(ctx, userID)
			if err != nil {
				return err
			}
			if u.Status == store.UserStatusDeleting {
				return userDeletionInProgressFor(ctx, tx, userID)
			}
		}
	}
	b, err := listUserDeleteBlockers(ctx, tx, userID)
	if err != nil {
		return err
	}
	if b.empty() {
		return nil
	}
	return &userDeleteBlockedError{blockers: *b}
}

// listUserDeleteBlockers returns what blocks the deletion of userID, read
// from st (the delete transaction, or the store for the read-only listing;
// it takes no lock). Each list is sorted by ID and capped at
// userDeleteBlockersLimit, with Truncated set when either has more.
//
// Agents are the user's agents: agents with OwnerID == userID, agents
// started by those agents (the user is the root of their ancestry), and
// agents the scheduler dispatched for the user's schedules (OwnerID empty,
// CreatedBy == userID). An agent the scheduler dispatched for a schedule an
// agent created records no ancestry, so it is not counted (recording
// ancestry on that path belongs to the scheduler rework). Soft-deleted
// agents do not count: the agent list hides them by default
// (AgentFilter.IncludeDeleted is false), and they are purged later; restore
// refuses an agent whose guard user (lockAgentGuardUserTx) no longer exists.
// Every other agent counts whatever its phase, including a stopped agent or
// one whose deletion is still in progress, since all of those still appear
// in the agent list.
//
// Edges are the active delegation edges with the user as the delegator
// whose delegate is an agent that exists and is not soft-deleted, and is not
// one of those agents (each agent is listed once). An edge to an agent that
// is gone grants nothing and nothing could be deleted to clear it, and an
// edge to a user (no production path creates one) could be cleared only by
// deleting that other user, so neither blocks; the delete deactivates them
// (deactivateUserDelegatorEdgesTx). Agent delegation grants the user issued
// do not block: their use-time checks deny a deleted issuer.
func listUserDeleteBlockers(ctx context.Context, st store.Store, userID string) (*userDeleteBlockers, error) {
	all, err := collectUserDeleteBlockers(ctx, st, userID)
	if err != nil {
		return nil, err
	}
	agents := make([]ownedAgentRef, 0, len(all.agents))
	for i := range all.agents {
		a := &all.agents[i]
		agents = append(agents, ownedAgentRef{ID: a.ID, Slug: a.Slug, ProjectID: a.ProjectID, DeletionState: a.DeletionState})
	}
	b := &userDeleteBlockers{Agents: agents, Edges: all.edges}
	if len(b.Agents) > userDeleteBlockersLimit {
		b.Agents, b.Truncated = b.Agents[:userDeleteBlockersLimit], true
	}
	if len(b.Edges) > userDeleteBlockersLimit {
		b.Edges, b.Truncated = b.Edges[:userDeleteBlockersLimit], true
	}
	return b, nil
}

// userDeleteBlockerSet is every blocker of a user deletion, uncapped: the
// agents (whole rows, sorted by ID) and the edges (sorted by ID), chosen
// as listUserDeleteBlockers describes. The forced delete needs the whole
// set: it authorizes every agent, compares the set inside its transaction
// and records one row per agent.
type userDeleteBlockerSet struct {
	agents []store.Agent
	edges  []delegatorEdgeRef
}

func (b *userDeleteBlockerSet) empty() bool {
	return len(b.agents) == 0 && len(b.edges) == 0
}

// agentIDs returns the IDs of the blocking agents as a set.
func (b *userDeleteBlockerSet) agentIDs() map[string]bool {
	ids := make(map[string]bool, len(b.agents))
	for i := range b.agents {
		ids[b.agents[i].ID] = true
	}
	return ids
}

// collectUserDeleteBlockers returns every blocker of the deletion of
// userID, uncapped, read from st (see listUserDeleteBlockers for which
// agents and edges block). It takes no lock.
func collectUserDeleteBlockers(ctx context.Context, st store.Store, userID string) (*userDeleteBlockerSet, error) {
	agents := []store.Agent{}
	seen := map[string]bool{}
	if _, err := uuid.Parse(userID); err == nil {
		// Direct agents (OwnerID == userID) and agents whose ancestry
		// contains the user, which are the agents started by the user's
		// agents (and their descendants; a user ID only appears as the
		// ancestry root). Agents the user created also record [userID] as
		// their ancestry, so the two queries overlap; seen drops the
		// duplicates. The OwnerID query also covers legacy agents with an
		// empty ancestry. The CreatedBy query finds agents the scheduler
		// dispatched for the user's schedules: they have no owner or
		// ancestry and record the user only as created_by, so only those
		// with an empty OwnerID count from it (an agent with an owner is
		// counted, or not, by the other two queries).
		for _, filter := range []store.AgentFilter{{OwnerID: userID}, {AncestorID: userID}, {CreatedBy: userID}} {
			opts := store.ListOptions{Limit: ownedAgentsPageSize, SkipTotalCount: true}
			for {
				page, err := st.ListAgents(ctx, filter, opts)
				if err != nil {
					return nil, fmt.Errorf("list agents owned by user: %w", err)
				}
				for _, a := range page.Items {
					if seen[a.ID] || (filter.CreatedBy != "" && a.OwnerID != "") {
						continue
					}
					seen[a.ID] = true
					agents = append(agents, a)
				}
				if page.NextCursor == "" || len(page.Items) == 0 {
					break
				}
				opts.Cursor = page.NextCursor
			}
		}
	}

	edges := []delegatorEdgeRef{}
	all, err := st.GetDelegationEdgesForDelegator(ctx, store.DelegationPrincipalUser, userID)
	if err != nil {
		return nil, fmt.Errorf("list delegation edges of user: %w", err)
	}
	for _, e := range all {
		if e.DelegateType == store.DelegationPrincipalAgent && seen[e.DelegateID] {
			continue
		}
		blocks, err := delegateBlocks(ctx, st, e.DelegateType, e.DelegateID)
		if err != nil {
			return nil, err
		}
		if !blocks {
			continue
		}
		edges = append(edges, delegatorEdgeRef{ID: e.ID, DelegateType: e.DelegateType, DelegateID: e.DelegateID, ScopeType: e.ScopeType, ScopeID: e.ScopeID})
	}

	sort.Slice(agents, func(i, j int) bool { return agents[i].ID < agents[j].ID })
	sort.Slice(edges, func(i, j int) bool { return edges[i].ID < edges[j].ID })
	return &userDeleteBlockerSet{agents: agents, edges: edges}, nil
}

// delegateBlocks reports whether the delegate of an edge makes the edge a
// blocker: an agent that exists and is not soft-deleted. An edge whose
// delegate is not an agent never blocks.
func delegateBlocks(ctx context.Context, st store.Store, delegateType, delegateID string) (bool, error) {
	if delegateType != store.DelegationPrincipalAgent {
		return false, nil
	}
	a, err := st.GetAgent(ctx, delegateID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("look up delegate agent %s: %w", delegateID, err)
	}
	return a.DeletedAt.IsZero(), nil
}

// deactivateUserDelegatorEdgesTx deactivates every active delegation edge
// with userID as the delegator, recording cause delegator_deleted under one
// new operation ID (the store records the time). A user delete calls it
// inside its transaction after checkUserDeleteBlockersTx passed, so it
// normally finds only edges that do not block (to an agent that is gone, or
// to a user). It deactivates every edge that is active when the sweep runs,
// including one reactivated after the check read but before the sweep. A
// restore whose delegator is not the guard user does not take this lock;
// serializing it is later-phase work.
func deactivateUserDelegatorEdgesTx(ctx context.Context, tx store.Store, userID string) error {
	d := store.Deactivation{Cause: store.EdgeDeactivationDelegatorDeleted, OpID: uuid.NewString()}
	if _, err := tx.DeactivateDelegationEdgesForDelegator(ctx, store.DelegationPrincipalUser, userID, d); err != nil {
		return fmt.Errorf("deactivate delegation edges of user: %w", err)
	}
	return nil
}

// cleanupOrphanedProvisionedUser deletes a user row the Google identity
// resolver provisioned and then lost to a concurrent binding of the same
// subject, only while nothing depends on it (ptone/scion#2143). In one
// transaction it locks the row exclusively (so agent create and restore
// for the user wait), and keeps the row, reporting false, when it has an
// external identity binding, a role binding, or anything that blocks a
// user delete (agents or delegations; collectUserDeleteBlockers).
// Otherwise it deactivates every edge the user delegated (cause
// delegator_deleted) and deletes the row. A row that is already gone
// reports false with no error.
func (s *Server) cleanupOrphanedProvisionedUser(ctx context.Context, userID string) (bool, error) {
	deleted := false
	err := s.store.WithTx(ctx, func(tx store.Store) error {
		if err := tx.LockUserRow(ctx, userID, true); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil
			}
			return err
		}
		bindings, err := tx.GetExternalIdentitiesByUserID(ctx, userID)
		if err != nil {
			return fmt.Errorf("list external identities of user: %w", err)
		}
		if len(bindings) > 0 {
			return nil
		}
		roles, err := tx.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
		if err != nil {
			return fmt.Errorf("list role bindings of user: %w", err)
		}
		if len(roles) > 0 {
			return nil
		}
		blockers, err := collectUserDeleteBlockers(ctx, tx, userID)
		if err != nil {
			return err
		}
		if !blockers.empty() {
			return nil
		}
		if err := deactivateUserDelegatorEdgesTx(ctx, tx, userID); err != nil {
			return err
		}
		if err := tx.DeleteUser(ctx, userID); err != nil {
			return fmt.Errorf("delete user: %w", err)
		}
		deleted = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return deleted, nil
}

// errAgentOwnerUserMissing is returned when the guard principal of an agent
// (the user the delete guard counts it for: its owner, its ancestry root or
// the principal of its schedule's latest revision, the revision principal;
// see lockAgentGuardUserTx) no longer exists
// (see lockUserPrincipalTx). That is normally a deleted user, but it can be
// a legacy root agent that has since been purged, because a missing root
// cannot be told apart from a deleted user; hence the neutral text.
var errAgentOwnerUserMissing = errors.New("the user or agent this agent belongs to no longer exists")

// errAgentOwnerUserDeleting is returned when the guard user of an agent
// (see lockAgentGuardUserTx) is being deleted: its status is deleting
// while a forced user delete stops its agents (ptone/scion#2143). Agent
// create and restore for that user's tree are refused until the deletion
// completes, after which errAgentOwnerUserMissing applies.
var errAgentOwnerUserDeleting = errors.New("the user this agent belongs to is being deleted")

// errAgentGuardUserLookup wraps a store error reading the locked guard
// user's row (its status). Agent create and restore answer 503: the guard
// could not be verified, and nothing was written.
var errAgentGuardUserLookup = errors.New("could not read the user this agent belongs to")

// lockUserPrincipalTx takes a shared lock on the row of principalID when it
// names a user, and reports whether it did (ptone/scion#2769). It returns
// errAgentOwnerUserDeleting when that user's status is deleting. It runs
// inside an agent create or restore transaction, before the agent row is
// written. Paired with the exclusive lock checkUserDeleteBlockersTx takes in
// the user delete transaction, it serializes the two on PostgreSQL.
//
// principalID is a polymorphic principal reference (a user or an agent). It
// returns false and no error when the ID is empty or not a UUID (not a user
// that can own an agent), or names an existing agent. It returns
// errAgentOwnerUserMissing when the ID is neither a user nor an agent (both
// lookups return not found), the existence rule relationshipSourceActive
// uses: the principal no longer exists. That is normally a deleted user, but
// it can be a legacy root agent that has since been purged, because a
// missing root cannot be told apart from a deleted user (see
// lockAgentGuardUserTx).
func lockUserPrincipalTx(ctx context.Context, tx store.Store, principalID string) (bool, error) {
	if principalID == "" {
		return false, nil
	}
	if _, err := uuid.Parse(principalID); err != nil {
		return false, nil
	}
	err := tx.LockUserRow(ctx, principalID, false)
	if err == nil {
		// The row is locked shared, so a forced delete's transaction
		// (which locks it exclusively before it writes the deleting
		// status) has either committed, and the status reads deleting
		// here, or waits for this transaction (ptone/scion#2143).
		u, gerr := tx.GetUser(ctx, principalID)
		if gerr != nil {
			return false, fmt.Errorf("%w: %w", errAgentGuardUserLookup, gerr)
		}
		if u.Status == store.UserStatusDeleting {
			return true, errAgentOwnerUserDeleting
		}
		return true, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return false, err
	}
	if _, err := tx.GetAgent(ctx, principalID); err == nil {
		return false, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return false, err
	}
	return false, errAgentOwnerUserMissing
}

// lockAgentGuardUserTx takes a shared lock on the row of the user that
// listUserDeleteBlockers would count for the new agent a, and re-checks
// that the user exists (ptone/scion#2769). That user is the first of these
// that is a user:
//
//   - a.OwnerID (an agent a user created directly);
//   - a.Ancestry[0], the ancestry root (an agent started by one of the
//     user's agents; a user ID only appears as the root);
//   - a.CreatedBy when a.OwnerID is empty (an agent the scheduler
//     dispatched for a user's schedule, which records the user only there).
//
// When the ancestry is non-empty and its root differs from a.OwnerID, the
// owner is the parent agent (only the root can be a user), so a.OwnerID is
// not a candidate: a descendant's guard user is its root user. That keeps a
// restore of a descendant whose parent agent row is gone (purged after its
// soft delete) from being refused while its root user exists. A legacy
// child whose parent had an empty ancestry ([parent]) and a legacy agent
// with an empty ancestry still check a.OwnerID, by existence.
//
// It runs in the create and restore transactions before the agent row is
// written, so a create or restore racing that user's delete either commits
// first (and the delete sees the agent and is refused) or waits for the
// delete and then fails with errAgentOwnerUserMissing. A candidate that
// names an existing agent is skipped; one that is neither a user nor an
// agent fails closed with errAgentOwnerUserMissing (lockUserPrincipalTx).
// So a descendant whose ancestry root was a legacy agent that has since been
// purged is refused (create and restore), because a missing root cannot be
// told apart from a deleted user; an owner-kind or root-kind column would
// remove the inference.
func lockAgentGuardUserTx(ctx context.Context, tx store.Store, a *store.Agent) error {
	candidates := []string{a.OwnerID}
	if len(a.Ancestry) > 0 && a.Ancestry[0] != a.OwnerID {
		candidates = []string{a.Ancestry[0]}
	}
	if a.OwnerID == "" {
		candidates = append(candidates, a.CreatedBy)
	}
	for _, id := range candidates {
		locked, err := lockUserPrincipalTx(ctx, tx, id)
		if err != nil || locked {
			return err
		}
	}
	return nil
}

// userScopedDataCleanupTimeout bounds the post-delete cleanup of one user's
// secrets and env vars, which may call an external secret backend.
const userScopedDataCleanupTimeout = 30 * time.Second

// removeUserScopedData deletes the user-scope env vars and secrets of a user
// that has been deleted. It runs after the delete transaction commits and is
// best effort: the secret backend may be external (GCP Secret Manager) and
// cannot join the transaction, so a failure is logged at Warn and never
// fails the request. Anything left behind is retried by the startup sweep
// (sweepOrphanedUserScopedData). It reports whether everything was removed.
func (s *Server) removeUserScopedData(ctx context.Context, userID string) bool {
	// The user's scheduled chat messages go too, on their own bounded
	// context (best effort; one left behind fails at fire time and is
	// purged later).
	defer s.deleteScheduledMessagesOfSender(ctx, userID)
	// So do the user's starred messages (best effort; upkeep purges any
	// left behind, and a deleted user cannot read them).
	defer s.deleteMessageStarsOfUser(ctx, userID)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), userScopedDataCleanupTimeout)
	defer cancel()
	return s.removeUserScopedDataCtx(ctx, userID)
}

// removeUserScopedDataCtx is removeUserScopedData bounded only by ctx.
func (s *Server) removeUserScopedDataCtx(ctx context.Context, userID string) bool {
	ok := true
	if n, err := s.store.DeleteEnvVarsByScope(ctx, store.ScopeUser, userID); err != nil {
		ok = false
		slog.Warn("user delete: failed to remove user-scope env vars",
			"user_id", userID, "error", err)
	} else if n > 0 {
		slog.Info("user delete: removed user-scope env vars", "user_id", userID, "count", n)
	}

	backend := s.GetSecretBackend()
	if backend == nil {
		return s.removeUserScopedSecretRowsWithoutBackend(ctx, userID) && ok
	}

	metas, err := backend.List(ctx, secret.Filter{Scope: secret.ScopeUser, ScopeID: userID})
	if err != nil {
		slog.Warn("user delete: failed to list user-scope secrets",
			"user_id", userID, "error", err)
		return false
	}
	removed := 0
	for _, m := range metas {
		if err := backend.Delete(ctx, m.Name, secret.ScopeUser, userID); err != nil && !errors.Is(err, store.ErrNotFound) {
			ok = false
			slog.Warn("user delete: failed to remove user-scope secret",
				"user_id", userID, "name", m.Name, "error", err)
			continue
		}
		removed++
	}
	if removed > 0 {
		slog.Info("user delete: removed user-scope secrets", "user_id", userID, "count", removed)
	}
	return ok
}

// removeUserScopedSecretRowsWithoutBackend handles a deleted user's
// user-scope secret rows when no secret backend is configured (the backend
// failed to initialize at startup). A row whose value is stored in the hub
// database (written by the local backend, encrypted or plaintext) is
// deleted, so the value does not outlive the user. A row with no value in the
// database only references an external backend (GCP Secret Manager); it is
// kept so a later sweep, once the backend is reachable, can remove the
// external value too. A row that holds a value and also names an external
// reference is deleted, and the dropped reference is logged at Warn. It
// reports whether every row was removed.
func (s *Server) removeUserScopedSecretRowsWithoutBackend(ctx context.Context, userID string) bool {
	rows, err := s.store.ListSecrets(ctx, store.SecretFilter{Scope: store.ScopeUser, ScopeID: userID})
	if err != nil {
		slog.Warn("user delete: failed to list user-scope secrets",
			"user_id", userID, "error", err)
		return false
	}
	ok := true
	removed, kept := 0, 0
	for _, row := range rows {
		// ListSecrets never returns the value, so read it to tell a
		// DB-stored value from an external reference.
		value, err := s.store.GetSecretValue(ctx, row.Key, store.ScopeUser, userID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			ok = false
			slog.Warn("user delete: failed to read user-scope secret",
				"user_id", userID, "name", row.Key, "error", err)
			continue
		}
		if value == "" {
			ok = false
			kept++
			continue
		}
		if row.SecretRef != "" {
			slog.Warn("user delete: dropping external secret reference with the secret row; remove the external value manually",
				"user_id", userID, "name", row.Key, "secret_ref", row.SecretRef)
		}
		if err := s.store.DeleteSecret(ctx, row.Key, store.ScopeUser, userID); err != nil && !errors.Is(err, store.ErrNotFound) {
			ok = false
			slog.Warn("user delete: failed to remove user-scope secret",
				"user_id", userID, "name", row.Key, "error", err)
			continue
		}
		removed++
	}
	if removed > 0 {
		slog.Info("user delete: removed user-scope secrets stored in the hub database",
			"user_id", userID, "count", removed)
	}
	if kept > 0 {
		slog.Warn("user delete: no secret backend configured; keeping user-scope secret rows that reference an external backend until a sweep can remove them",
			"user_id", userID, "count", kept)
	}
	return ok
}

// userScopedDataSweepBudget bounds the whole startup sweep, including every
// call to an external secret backend.
const userScopedDataSweepBudget = 2 * time.Minute

// startUserScopedDataSweep runs the startup sweep in a goroutine so slow
// database reads or a slow or unreachable secret backend cannot delay
// startup. The goroutine finds the missing users and removes their values
// under one total time budget (userScopedDataSweepBudget), which bounds the
// lookups as well as the removals. The sweep is non-fatal: failures are
// logged at Warn. The returned channel is closed when the goroutine ends.
func (s *Server) startUserScopedDataSweep(parent context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ctx, cancel := context.WithTimeout(parent, userScopedDataSweepBudget)
		defer cancel()
		missing, err := s.findOrphanedUserScopeIDs(ctx)
		if err != nil {
			slog.Warn("Failed to sweep user-scope data of deleted users", "error", err)
			return
		}
		if len(missing) == 0 {
			return
		}
		removed, kept := s.removeOrphanedUserScopedData(ctx, missing)
		slog.Info("Swept user-scope data of deleted users",
			"users_removed", removed, "users_kept_or_failed", kept)
	}()
	return done
}

// findOrphanedUserScopeIDs returns, sorted, the scope IDs of user-scope
// secrets and env vars whose user no longer exists. A user lookup error
// other than not-found leaves that ID out, so its values are kept. If ctx
// ends during the lookups, it returns ctx's error and no IDs.
func (s *Server) findOrphanedUserScopeIDs(ctx context.Context) ([]string, error) {
	scopeIDs := make(map[string]bool)
	envVars, err := s.store.ListEnvVars(ctx, store.EnvVarFilter{Scope: store.ScopeUser})
	if err != nil {
		return nil, fmt.Errorf("list user-scope env vars: %w", err)
	}
	for _, ev := range envVars {
		scopeIDs[ev.ScopeID] = true
	}
	secrets, err := s.store.ListSecrets(ctx, store.SecretFilter{Scope: store.ScopeUser})
	if err != nil {
		return nil, fmt.Errorf("list user-scope secrets: %w", err)
	}
	for _, sec := range secrets {
		scopeIDs[sec.ScopeID] = true
	}

	ids := make([]string, 0, len(scopeIDs))
	for id := range scopeIDs {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("user lookup: %w", err)
		}
		if id == "" {
			continue
		}
		if _, err := s.store.GetUser(ctx, id); err == nil {
			continue
		} else if !errors.Is(err, store.ErrNotFound) {
			slog.Warn("user-scope data sweep: user lookup failed, keeping values",
				"user_id", id, "error", err)
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

// removeOrphanedUserScopedData removes the user-scope data of each missing
// user under ctx. removed counts the users whose data was fully removed;
// kept counts the users with values left behind (a removal failure, a row
// kept for an external backend, or the budget running out).
func (s *Server) removeOrphanedUserScopedData(ctx context.Context, missing []string) (removed, kept int) {
	for _, id := range missing {
		if ctx.Err() != nil {
			kept++
			continue
		}
		if s.removeUserScopedDataCtx(ctx, id) {
			removed++
		} else {
			kept++
		}
	}
	return removed, kept
}

// sweepOrphanedUserScopedData removes user-scope secrets and env vars whose
// user no longer exists, synchronously under ctx. It catches values left
// behind by a failed post-delete cleanup or by deletes from before that
// cleanup existed. A user lookup error other than not-found leaves that
// user's values untouched. removed is the number of missing users whose
// values were all removed; kept is the number with values left behind.
func (s *Server) sweepOrphanedUserScopedData(ctx context.Context) (removed, kept int, err error) {
	missing, err := s.findOrphanedUserScopeIDs(ctx)
	if err != nil {
		return 0, 0, err
	}
	removed, kept = s.removeOrphanedUserScopedData(ctx, missing)
	return removed, kept, nil
}
