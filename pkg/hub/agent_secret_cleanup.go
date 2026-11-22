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
// Returns nil on error or when s.store is nil.
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

// withAgentSecretCleanup captures any agent-scoped secret names for agentID
// before fn runs a hard-delete or purge transaction (which removes the DB
// rows), and runs best-effort post-commit external (GCP) secret cleanup when
// fn commits with n > 0. Keeping the store call inside fn preserves per-site
// authz-guard exemptions while making external cleanup structural across all
// hard-delete callers.
func (s *Server) withAgentSecretCleanup(ctx context.Context, agentID string, fn func() (int, error)) (int, error) {
	var agentSecretKeys []string
	if s.GetSecretBackend() != nil {
		agentSecretKeys = s.listAgentSecretNames(ctx, agentID)
	}
	n, err := fn()
	if err == nil && n > 0 && len(agentSecretKeys) > 0 {
		s.removeAgentScopedSecrets(ctx, agentID, agentSecretKeys)
	}
	return n, err
}

// snapshotAgentSecrets reads the metadata and values of all agent-scoped
// secrets belonging to agentID so env-gather recreate can carry them over to
// the replacement agent before the old agent is hard-deleted. Returns (nil, nil)
// when no secret backend is configured or the agent holds no agent-scope secrets.
func (s *Server) snapshotAgentSecrets(ctx context.Context, agentID string) ([]*secret.SecretWithValue, error) {
	backend := s.GetSecretBackend()
	if backend == nil || agentID == "" {
		return nil, nil
	}
	metas, err := backend.List(ctx, secret.Filter{
		Scope:   secret.ScopeAgent,
		ScopeID: agentID,
	})
	if err != nil {
		return nil, err
	}
	if len(metas) == 0 {
		return nil, nil
	}
	carried := make([]*secret.SecretWithValue, 0, len(metas))
	for _, m := range metas {
		sv, err := backend.Get(ctx, m.Name, secret.ScopeAgent, agentID)
		if err != nil {
			return nil, err
		}
		carried = append(carried, sv)
	}
	return carried, nil
}

// restoreCarriedAgentSecrets writes agent-scoped secrets carried over from an
// env-gather recreated agent onto the replacement agent. If any write fails,
// any secrets already written to agent.ID are deleted best-effort so no
// orphaned external values remain under the new ID, and the error is returned
// for the caller to fail closed and roll back the new agent row.
func (s *Server) restoreCarriedAgentSecrets(ctx context.Context, agent *store.Agent, carried []*secret.SecretWithValue) error {
	if len(carried) == 0 || agent == nil {
		return nil
	}
	backend := s.GetSecretBackend()
	if backend == nil {
		return secret.ErrNoSecretBackend
	}
	written := make([]string, 0, len(carried))
	for _, sv := range carried {
		_, _, err := backend.Set(ctx, &secret.SetSecretInput{
			Name:          sv.Name,
			Value:         sv.Value,
			SecretType:    sv.SecretType,
			Target:        sv.Target,
			Scope:         secret.ScopeAgent,
			ScopeID:       agent.ID,
			ProjectID:     agent.ProjectID,
			Description:   sv.Description,
			InjectionMode: sv.InjectionMode,
			AllowProgeny:  sv.AllowProgeny,
			CreatedBy:     sv.CreatedBy,
			UpdatedBy:     sv.UpdatedBy,
		})
		if err != nil {
			if len(written) > 0 {
				s.removeAgentScopedSecrets(ctx, agent.ID, written)
			}
			return err
		}
		written = append(written, sv.Name)
	}
	return nil
}
