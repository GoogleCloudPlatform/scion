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

// Agents and user-scoped data of a deleted user (ptone/scion#2769).
//
// A user who still owns agents cannot be deleted: both delete paths
// (DELETE /api/v1/users/{id} and the deprecated allow-list delete) refuse
// with 409 conflict and list the agents in details.agents. On PostgreSQL the
// delete locks the user row (FOR UPDATE) before the check, and agent create
// and restore lock it shared (FOR KEY SHARE) and re-check that the user
// exists, so a create racing a delete either commits first (and the delete
// sees the agent) or fails. After a delete commits, the user's user-scope
// secrets and env vars are removed as a best effort, and a startup sweep
// removes any left behind for users that no longer exist.

// ownedAgentRef identifies an agent that blocks the deletion of its owner.
type ownedAgentRef struct {
	ID        string `json:"id"`
	Slug      string `json:"slug"`
	ProjectID string `json:"projectId"`
}

// userOwnsAgentsDeleteError is returned by checkUserOwnsNoAgentsTx when the
// user still owns agents. The surrounding transaction rolls back, so nothing
// is changed.
type userOwnsAgentsDeleteError struct {
	agents []ownedAgentRef
}

func (e *userOwnsAgentsDeleteError) Error() string {
	return userOwnsAgentsDeleteMessage
}

const userOwnsAgentsDeleteMessage = "cannot delete a user who owns agents — delete their agents first, including agents started by those agents or by the user's schedules"

// writeUserOwnsAgentsDeleteError writes the 409 conflict response for a user
// deletion refused because the user owns agents; details.agents lists them.
func writeUserOwnsAgentsDeleteError(w http.ResponseWriter, e *userOwnsAgentsDeleteError) {
	writeError(w, http.StatusConflict, ErrCodeConflict, userOwnsAgentsDeleteMessage,
		map[string]interface{}{"agents": e.agents})
}

// ownedAgentsPageSize is the page size used when listing a user's agents. It
// is a variable so tests can force several pages.
var ownedAgentsPageSize = 100

// checkUserOwnsNoAgentsTx refuses the deletion of userID while the user's
// agents exist: agents with OwnerID == userID, agents started by those
// agents (the user is the root of their ancestry), and agents the scheduler
// dispatched for the user's schedules (OwnerID empty, CreatedBy == userID).
// An agent the scheduler dispatched for a schedule an agent created records
// no ancestry, so it is not counted (recording ancestry on that path belongs
// to the scheduler rework). It runs inside the delete transaction.
//
// It first locks the user row exclusively (SELECT ... FOR UPDATE on
// PostgreSQL; see store.UserStore.LockUserRow). Agent create (including the
// scheduler's) and restore take a shared lock on that user's row in their
// own transactions and re-check that the user exists (lockAgentGuardUserTx),
// so an agent this check would count either commits before it runs (and is
// listed) or waits for the delete and then fails. It returns
// store.ErrNotFound if the user no longer exists.
//
// Soft-deleted agents do not count: the agent list hides them by default
// (AgentFilter.IncludeDeleted is false), and they are purged later; restore
// refuses an agent whose guard user (lockAgentGuardUserTx) no longer exists.
// Every other agent counts whatever its phase, including a stopped agent or
// one whose deletion is still in progress, since all of those still appear
// in the agent list.
func checkUserOwnsNoAgentsTx(ctx context.Context, tx store.Store, userID string) error {
	// agents.owner_id is a UUID column, so a user ID that is not a UUID
	// cannot own an agent (and the store rejects it as a filter value).
	if _, err := uuid.Parse(userID); err != nil {
		return nil
	}
	if err := tx.LockUserRow(ctx, userID, true); err != nil {
		return err
	}
	var owned []ownedAgentRef
	seen := map[string]bool{}
	// Direct agents (OwnerID == userID) and agents whose ancestry contains
	// the user, which are the agents started by the user's agents (and their
	// descendants; a user ID only appears as the ancestry root). Agents the
	// user created also record [userID] as their ancestry, so the two
	// queries overlap; seen drops the duplicates. The OwnerID query still
	// covers legacy agents with an empty ancestry. The CreatedBy query finds
	// agents the scheduler dispatched for the user's schedules: they have no
	// owner or ancestry and record the user only as created_by, so only
	// those with an empty OwnerID count from it (an agent with an owner is
	// counted, or not, by the other two queries).
	for _, filter := range []store.AgentFilter{{OwnerID: userID}, {AncestorID: userID}, {CreatedBy: userID}} {
		opts := store.ListOptions{Limit: ownedAgentsPageSize, SkipTotalCount: true}
		for {
			page, err := tx.ListAgents(ctx, filter, opts)
			if err != nil {
				return fmt.Errorf("list agents owned by user: %w", err)
			}
			for _, a := range page.Items {
				if seen[a.ID] || (filter.CreatedBy != "" && a.OwnerID != "") {
					continue
				}
				seen[a.ID] = true
				owned = append(owned, ownedAgentRef{ID: a.ID, Slug: a.Slug, ProjectID: a.ProjectID})
			}
			if page.NextCursor == "" || len(page.Items) == 0 {
				break
			}
			opts.Cursor = page.NextCursor
		}
	}
	if len(owned) == 0 {
		return nil
	}
	sort.Slice(owned, func(i, j int) bool { return owned[i].ID < owned[j].ID })
	return &userOwnsAgentsDeleteError{agents: owned}
}

// errAgentOwnerUserMissing is returned when the user an agent belongs to
// (the user the delete guard counts it for: its owner, its ancestry root or
// the creator of its schedule, see lockAgentGuardUserTx) no longer exists
// (see lockUserPrincipalTx).
var errAgentOwnerUserMissing = errors.New("the user this agent belongs to no longer exists")

// lockUserPrincipalTx takes a shared lock on the row of principalID when it
// names a user, and reports whether it did (ptone/scion#2769). It runs
// inside an agent create or restore transaction, before the agent row is
// written. Paired with the exclusive lock checkUserOwnsNoAgentsTx takes in
// the user delete transaction, it serializes the two on PostgreSQL.
//
// principalID is a polymorphic principal reference (a user or an agent). It
// returns false and no error when the ID is empty or not a UUID (not a user
// that can own an agent), or names an existing agent. It returns
// errAgentOwnerUserMissing when the ID is neither a user nor an agent (both
// lookups return not found), the existence rule relationshipSourceActive
// uses: the principal was a user that has been deleted.
func lockUserPrincipalTx(ctx context.Context, tx store.Store, principalID string) (bool, error) {
	if principalID == "" {
		return false, nil
	}
	if _, err := uuid.Parse(principalID); err != nil {
		return false, nil
	}
	err := tx.LockUserRow(ctx, principalID, false)
	if err == nil {
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
// checkUserOwnsNoAgentsTx would count for the new agent a, and re-checks
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

// startUserScopedDataSweep runs the startup sweep in the background so a slow
// or unreachable secret backend cannot delay startup. The missing users are
// found before it returns (database reads only); their values are removed in
// a goroutine under one total time budget (userScopedDataSweepBudget). The
// sweep is non-fatal: failures are logged at Warn. The returned channel is
// closed when the goroutine ends.
func (s *Server) startUserScopedDataSweep(parent context.Context) <-chan struct{} {
	done := make(chan struct{})
	missing, err := s.findOrphanedUserScopeIDs(parent)
	if err != nil {
		slog.Warn("Failed to sweep user-scope data of deleted users", "error", err)
		close(done)
		return done
	}
	if len(missing) == 0 {
		close(done)
		return done
	}
	go func() {
		defer close(done)
		ctx, cancel := context.WithTimeout(parent, userScopedDataSweepBudget)
		defer cancel()
		removed, kept := s.removeOrphanedUserScopedData(ctx, missing)
		slog.Info("Swept user-scope data of deleted users",
			"users_removed", removed, "users_kept_or_failed", kept)
	}()
	return done
}

// findOrphanedUserScopeIDs returns, sorted, the scope IDs of user-scope
// secrets and env vars whose user no longer exists. A user lookup error
// other than not-found leaves that ID out, so its values are kept.
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
