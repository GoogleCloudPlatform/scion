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
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// agentSecretCleanupCollectorKey is the context key for collecting agent-scope
// secret key names inside a hard-delete or compensation transaction (from the
// same rows DeleteSecretsByScope removes) so post-commit external backend
// cleanup (e.g. GCP Secret Manager) deletes every key that existed at commit,
// including keys written after any pre-transaction snapshot.
type agentSecretCleanupCollectorKey struct{}

type agentSecretCleanupCollector struct {
	mu   sync.Mutex
	keys []string
}

func (c *agentSecretCleanupCollector) add(keys []string) {
	if c == nil || len(keys) == 0 {
		return
	}
	c.mu.Lock()
	c.keys = append(c.keys, keys...)
	c.mu.Unlock()
}

func (c *agentSecretCleanupCollector) snapshot() []string {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.keys) == 0 {
		return nil
	}
	out := make([]string, len(c.keys))
	copy(out, c.keys)
	return out
}

func withAgentSecretCleanupCollector(ctx context.Context, c *agentSecretCleanupCollector) context.Context {
	return context.WithValue(ctx, agentSecretCleanupCollectorKey{}, c)
}

// recordAgentSecretCleanupKeys records agent-scope secret key names captured
// inside a hard-delete transaction for post-commit external backend cleanup.
func recordAgentSecretCleanupKeys(ctx context.Context, keys []string) {
	if c, ok := ctx.Value(agentSecretCleanupCollectorKey{}).(*agentSecretCleanupCollector); ok && c != nil {
		c.add(keys)
	}
}

// errAgentDeletedDuringSecretWrite is returned when an agent is hard-deleted or
// soft-deleted while an agent-scope secret write is in flight.
var errAgentDeletedDuringSecretWrite = errors.New("agent deleted during secret write")

// compensateIfAgentDeleted re-reads agentID from the store after an agent-scope
// secret write. If the agent is gone (store.ErrNotFound) or soft-deleted
// (!DeletedAt.IsZero()), it removes the written keys from the secret backend
// and returns true. Uses a detached, bounded context so a cancelled request
// context cannot skip the recheck or leave orphaned external secrets.
func (s *Server) compensateIfAgentDeleted(ctx context.Context, agentID string, keys []string) bool {
	if s.store == nil || agentID == "" {
		return false
	}
	cctx, cancel := detachedCleanupContext(ctx, agentScopedSecretCleanupTimeout)
	defer cancel()
	a, err := s.store.GetAgent(cctx, agentID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && !a.DeletedAt.IsZero()) {
		s.removeAgentScopedSecrets(cctx, agentID, keys)
		return true
	}
	return false
}

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

// externalSecretNamer is an optional interface implemented by external secret
// backends (e.g. GCPBackend) that can report the backend-specific secret name
// for operator remediation logging when post-commit deletion fails.
type externalSecretNamer interface {
	ExternalSecretName(name, scope, scopeID string) string
}

// removeAgentScopedSecrets deletes every agent-scoped secret for agentID that
// the backend currently lists, plus any extra keys seeded in keys (for example,
// keys captured inside a hard-delete or project-delete transaction before the
// DB metadata rows were removed, which GCPBackend.List can no longer see after
// commit). Best-effort, bounded by agentScopedSecretCleanupTimeout; each failed
// delete is retried once and remaining failures are logged at Error level with
// the backend secret name when available.
func (s *Server) removeAgentScopedSecrets(ctx context.Context, agentID string, keys []string) bool {
	if agentID == "" {
		return true
	}
	backend := s.GetSecretBackend()
	if backend == nil {
		return true
	}
	return s.removeAgentScopedSecretsCtx(ctx, backend, agentID, keys)
}

func (s *Server) removeAgentScopedSecretsCtx(ctx context.Context, backend secret.SecretBackend, agentID string, keys []string) bool {
	cctx, cancel := detachedCleanupContext(ctx, agentScopedSecretCleanupTimeout)
	defer cancel()

	// Merge seeded keys with any rows the backend still lists (e.g. a local
	// backend or un-deleted rows). On GCPBackend, List reads DB metadata rows
	// that the hard-delete transaction already removed, so the in-transaction
	// captured keys passed via keys are authoritative for external deletion.
	ok := true
	seen := make(map[string]struct{}, len(keys))
	merged := make([]string, 0, len(keys))
	for _, k := range keys {
		if _, dup := seen[k]; !dup {
			seen[k] = struct{}{}
			merged = append(merged, k)
		}
	}
	metas, err := backend.List(cctx, secret.Filter{Scope: secret.ScopeAgent, ScopeID: agentID})
	if err != nil {
		ok = false
		slog.Error("agent delete: failed to list agent-scope secrets for post-commit cleanup",
			"agent_id", agentID, "error", err)
	} else {
		for _, m := range metas {
			if _, dup := seen[m.Name]; !dup {
				seen[m.Name] = struct{}{}
				merged = append(merged, m.Name)
			}
		}
	}
	if len(merged) == 0 {
		return ok
	}

	namer, _ := backend.(externalSecretNamer)
	removed := 0
	for _, key := range merged {
		var delErr error
		for attempt := 0; attempt < 2; attempt++ {
			delErr = backend.Delete(cctx, key, secret.ScopeAgent, agentID)
			if delErr == nil || errors.Is(delErr, store.ErrNotFound) {
				delErr = nil
				break
			}
			if cctx.Err() != nil {
				break
			}
		}
		if delErr != nil {
			ok = false
			attrs := []any{
				"agent_id", agentID,
				"name", key,
				"error", delErr,
			}
			if namer != nil {
				attrs = append(attrs, "secret_name", namer.ExternalSecretName(key, secret.ScopeAgent, agentID))
			}
			slog.Error("agent delete: failed to remove agent-scope secret from backend after retry; remove manually", attrs...)
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

// withAgentSecretCleanup runs fn (a hard-delete, purge, env-gather recreate,
// or create compensation transaction) with an in-transaction key collector
// attached to txCtx so agentHardDeleteWork captures the exact agent-scope key
// set from the rows it deletes before DeleteSecretsByScope removes them. It
// also captures a pre-transaction snapshot for callers that do not invoke
// agentHardDeleteWork (such as PurgeDeletedAgentsByID on already soft-deleted
// agents, where new secret writes are refused). When fn commits with n > 0, it
// runs best-effort post-commit external backend cleanup for the merged key set.
func (s *Server) withAgentSecretCleanup(ctx context.Context, agentID string, fn func(txCtx context.Context) (int, error)) (int, error) {
	return s.withAgentSecretCleanupOpts(ctx, agentID, false, fn)
}

// withAgentSecretCleanupOnCompensation is withAgentSecretCleanup for create
// compensation paths: if fn returns n == 0 because the agent row is already
// absent (ErrNotFound), it still runs best-effort external backend cleanup so
// secrets written by the failed create cannot remain orphaned.
func (s *Server) withAgentSecretCleanupOnCompensation(ctx context.Context, agentID string, fn func(txCtx context.Context) (int, error)) (int, error) {
	return s.withAgentSecretCleanupOpts(ctx, agentID, true, fn)
}

func (s *Server) withAgentSecretCleanupOpts(ctx context.Context, agentID string, cleanupWhenAbsent bool, fn func(txCtx context.Context) (int, error)) (int, error) {
	var preKeys []string
	if s.GetSecretBackend() != nil {
		preKeys = s.listAgentSecretNames(ctx, agentID)
	}
	collector := &agentSecretCleanupCollector{}
	txCtx := withAgentSecretCleanupCollector(ctx, collector)
	n, err := fn(txCtx)
	if err == nil && n > 0 {
		keys := append(preKeys, collector.snapshot()...)
		s.removeAgentScopedSecrets(ctx, agentID, keys)
		return n, err
	}
	if cleanupWhenAbsent && err == nil && n == 0 && s.store != nil {
		cctx, cancel := detachedCleanupContext(ctx, agentScopedSecretCleanupTimeout)
		defer cancel()
		if _, gerr := s.store.GetAgent(cctx, agentID); errors.Is(gerr, store.ErrNotFound) {
			keys := append(preKeys, collector.snapshot()...)
			s.removeAgentScopedSecrets(cctx, agentID, keys)
		}
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
			// Include sv.Name in rollback: an external backend such as
			// GCPBackend.Set may write the secret version before failing on the
			// DB metadata upsert or a deadline.
			s.removeAgentScopedSecrets(ctx, agent.ID, append(written, sv.Name))
			return err
		}
		written = append(written, sv.Name)
	}
	return nil
}
