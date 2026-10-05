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
// with 409 conflict and list the agents in details.agents. After a delete
// commits, the user's user-scope secrets and env vars are removed as a best
// effort, and a startup sweep removes any left behind for users that no
// longer exist.

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

const userOwnsAgentsDeleteMessage = "cannot delete a user who owns agents — delete their agents first"

// writeUserOwnsAgentsDeleteError writes the 409 conflict response for a user
// deletion refused because the user owns agents; details.agents lists them.
func writeUserOwnsAgentsDeleteError(w http.ResponseWriter, e *userOwnsAgentsDeleteError) {
	writeError(w, http.StatusConflict, ErrCodeConflict, userOwnsAgentsDeleteMessage,
		map[string]interface{}{"agents": e.agents})
}

// ownedAgentsPageSize is the page size used when listing a user's agents.
const ownedAgentsPageSize = 100

// checkUserOwnsNoAgentsTx refuses the deletion of userID while agents with
// OwnerID == userID exist. It runs inside the delete transaction.
//
// Soft-deleted agents do not count: the agent list hides them by default
// (AgentFilter.IncludeDeleted is false), and they are purged later. Every
// other agent counts whatever its phase, including a stopped agent or one
// whose deletion is still in progress, since all of those still appear in
// the agent list.
//
// The check is a read, so an agent created for the user after it and before
// the transaction commits is not seen.
func checkUserOwnsNoAgentsTx(ctx context.Context, tx store.Store, userID string) error {
	// agents.owner_id is a UUID column, so a user ID that is not a UUID
	// cannot own an agent (and the store rejects it as a filter value).
	if _, err := uuid.Parse(userID); err != nil {
		return nil
	}
	var owned []ownedAgentRef
	opts := store.ListOptions{Limit: ownedAgentsPageSize, SkipTotalCount: true}
	for {
		page, err := tx.ListAgents(ctx, store.AgentFilter{OwnerID: userID}, opts)
		if err != nil {
			return fmt.Errorf("list agents owned by user: %w", err)
		}
		for _, a := range page.Items {
			owned = append(owned, ownedAgentRef{ID: a.ID, Slug: a.Slug, ProjectID: a.ProjectID})
		}
		if page.NextCursor == "" || len(page.Items) == 0 {
			break
		}
		opts.Cursor = page.NextCursor
	}
	if len(owned) == 0 {
		return nil
	}
	sort.Slice(owned, func(i, j int) bool { return owned[i].ID < owned[j].ID })
	return &userOwnsAgentsDeleteError{agents: owned}
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
		// No secret backend is configured, so the secret API cannot have
		// written any values. Rows left from an earlier configuration are
		// kept: their values may live in an external backend that this hub
		// cannot reach now, and removing the rows would lose the reference.
		return ok
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

// sweepOrphanedUserScopedData removes user-scope secrets and env vars whose
// user no longer exists. It runs at startup and catches values left behind by
// a failed post-delete cleanup or by deletes from before that cleanup
// existed. A user lookup error other than not-found leaves that user's values
// untouched. It returns the number of missing users whose values it removed.
func (s *Server) sweepOrphanedUserScopedData(ctx context.Context) (int, error) {
	scopeIDs := make(map[string]bool)
	envVars, err := s.store.ListEnvVars(ctx, store.EnvVarFilter{Scope: store.ScopeUser})
	if err != nil {
		return 0, fmt.Errorf("list user-scope env vars: %w", err)
	}
	for _, ev := range envVars {
		scopeIDs[ev.ScopeID] = true
	}
	secrets, err := s.store.ListSecrets(ctx, store.SecretFilter{Scope: store.ScopeUser})
	if err != nil {
		return 0, fmt.Errorf("list user-scope secrets: %w", err)
	}
	for _, sec := range secrets {
		scopeIDs[sec.ScopeID] = true
	}

	ids := make([]string, 0, len(scopeIDs))
	for id := range scopeIDs {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	swept := 0
	for _, id := range ids {
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
		s.removeUserScopedData(ctx, id)
		swept++
	}
	return swept, nil
}
