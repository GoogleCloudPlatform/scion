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
	"log/slog"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// agentScopedSecretCleanupTimeout bounds post-commit external secret cleanup
// for a hard-deleted or purged agent (e.g. GCP Secret Manager calls).
const agentScopedSecretCleanupTimeout = 30 * time.Second

// listAgentSecretNames returns the key names of all agent-scoped secrets
// currently stored for agentID. Called before a hard delete or purge
// transaction removes the DB rows so that post-commit cleanup can delete
// external (GCP Secret Manager) values that cannot join the DB transaction.
// Returns nil on error or when no secret backend is configured.
func (s *Server) listAgentSecretNames(ctx context.Context, agentID string) []string {
	if s.store == nil || agentID == "" {
		return nil
	}
	rows, err := s.store.ListSecrets(ctx, store.SecretFilter{
		Scope:   store.ScopeAgent,
		ScopeID: agentID,
	})
	if err != nil {
		slog.Warn("agent delete: failed to list agent-scope secrets before delete",
			"agent_id", agentID, "error", err)
		return nil
	}
	if len(rows) == 0 {
		return nil
	}
	keys := make([]string, 0, len(rows))
	for _, r := range rows {
		keys = append(keys, r.Key)
	}
	return keys
}

// removeAgentScopedSecrets deletes agent-scoped secrets from the secret
// backend (such as GCP Secret Manager) after a hard delete or retention purge
// commits. Local-backend DB rows are already deleted inside the transaction
// via DeleteSecretsByScope; this handles external backends whose values cannot
// join the DB transaction. Modeled on removeUserScopedData: best-effort,
// bounded by agentScopedSecretCleanupTimeout, logs at Warn on failure and
// never fails the delete.
func (s *Server) removeAgentScopedSecrets(ctx context.Context, agentID string, keys []string) bool {
	if agentID == "" {
		return true
	}
	backend := s.GetSecretBackend()
	if backend == nil {
		return true
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), agentScopedSecretCleanupTimeout)
	defer cancel()
	return s.removeAgentScopedSecretsCtx(cctx, backend, agentID, keys)
}

func (s *Server) removeAgentScopedSecretsCtx(ctx context.Context, backend secret.SecretBackend, agentID string, keys []string) bool {
	if len(keys) == 0 {
		// Fall back to listing from the backend in case any rows remain.
		metas, err := backend.List(ctx, secret.Filter{Scope: secret.ScopeAgent, ScopeID: agentID})
		if err != nil {
			slog.Warn("agent delete: failed to list agent-scope secrets for cleanup",
				"agent_id", agentID, "error", err)
			return false
		}
		if len(metas) == 0 {
			return true
		}
		keys = make([]string, 0, len(metas))
		for _, m := range metas {
			keys = append(keys, m.Name)
		}
	}

	ok := true
	removed := 0
	for _, key := range keys {
		if err := backend.Delete(ctx, key, secret.ScopeAgent, agentID); err != nil && !errors.Is(err, store.ErrNotFound) {
			ok = false
			slog.Warn("agent delete: failed to remove agent-scope secret from backend; retry cleanup or remove manually",
				"agent_id", agentID, "name", key, "error", err)
			continue
		}
		removed++
	}
	if removed > 0 {
		slog.Info("agent delete: removed agent-scope secrets from backend",
			"agent_id", agentID, "count", removed)
	}
	return ok
}
