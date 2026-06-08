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

package entadapter

import (
	"context"
	"fmt"
	"sync"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/lifecyclehook"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/lifecyclehookagentphase"
	entschema "github.com/GoogleCloudPlatform/scion/pkg/ent/schema"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// LifecycleHookStore implements store.LifecycleHookStore using Ent ORM.
type LifecycleHookStore struct {
	client      *ent.Client
	dialectOnce sync.Once
	dialectName string
}

// NewLifecycleHookStore creates a new Ent-backed LifecycleHookStore.
func NewLifecycleHookStore(client *ent.Client) *LifecycleHookStore {
	return &LifecycleHookStore{client: client}
}

// usesRowLocks returns true when the underlying database supports SELECT …
// FOR UPDATE (i.e. Postgres). SQLite uses a single-writer lock instead, so
// ForUpdate must be skipped — it returns an error on SQLite.
func (s *LifecycleHookStore) usesRowLocks(ctx context.Context) bool {
	s.dialectOnce.Do(func() {
		_, _ = s.client.LifecycleHookAgentPhase.Query().
			Where(func(sel *entsql.Selector) { s.dialectName = sel.Dialect() }).
			Exist(ctx)
	})
	return s.dialectName == dialect.Postgres
}

// entLifecycleHookToStore converts an Ent LifecycleHook entity to a store model.
func entLifecycleHookToStore(h *ent.LifecycleHook) *store.LifecycleHook {
	sh := &store.LifecycleHook{
		ID:                h.ID.String(),
		Name:              h.Name,
		ScopeType:         string(h.ScopeType),
		ScopeID:           h.ScopeID,
		Trigger:           string(h.Trigger),
		ExecutionIdentity: h.ExecutionIdentity,
		Enabled:           h.Enabled,
		Created:           h.Created,
		Updated:           h.Updated,
		CreatedBy:         h.CreatedBy,
		StateVersion:      h.StateVersion,
	}
	if h.Selector != nil {
		sh.Selector = entSelectorToStore(h.Selector)
	}
	if h.Action != nil {
		sh.Action = entActionToStore(h.Action)
	}
	return sh
}

// entSelectorToStore converts an Ent schema selector to a store selector.
func entSelectorToStore(s *entschema.LifecycleHookSelector) *store.LifecycleHookSelector {
	if s == nil {
		return nil
	}
	return &store.LifecycleHookSelector{
		ProjectID: s.ProjectID,
		Template:  s.Template,
	}
}

// storeSelectorToEnt converts a store selector to an Ent schema selector.
func storeSelectorToEnt(s *store.LifecycleHookSelector) *entschema.LifecycleHookSelector {
	if s == nil {
		return nil
	}
	return &entschema.LifecycleHookSelector{
		ProjectID: s.ProjectID,
		Template:  s.Template,
	}
}

// entActionToStore converts an Ent schema action to a store action.
func entActionToStore(a *entschema.LifecycleHookAction) *store.LifecycleHookAction {
	if a == nil {
		return nil
	}
	return &store.LifecycleHookAction{
		Type:                 a.Type,
		Method:               a.Method,
		URL:                  a.URL,
		Headers:              a.Headers,
		Body:                 a.Body,
		OnError:              a.OnError,
		TimeoutSeconds:       a.TimeoutSeconds,
		AllowedUntrustedVars: a.AllowedUntrustedVars,
	}
}

// storeActionToEnt converts a store action to an Ent schema action.
func storeActionToEnt(a *store.LifecycleHookAction) *entschema.LifecycleHookAction {
	if a == nil {
		return nil
	}
	return &entschema.LifecycleHookAction{
		Type:                 a.Type,
		Method:               a.Method,
		URL:                  a.URL,
		Headers:              a.Headers,
		Body:                 a.Body,
		OnError:              a.OnError,
		TimeoutSeconds:       a.TimeoutSeconds,
		AllowedUntrustedVars: a.AllowedUntrustedVars,
	}
}

// CreateLifecycleHook creates a new lifecycle hook record.
func (s *LifecycleHookStore) CreateLifecycleHook(ctx context.Context, h *store.LifecycleHook) error {
	uid, err := parseUUID(h.ID)
	if err != nil {
		return err
	}

	if h.StateVersion <= 0 {
		h.StateVersion = 1
	}

	create := s.client.LifecycleHook.Create().
		SetID(uid).
		SetName(h.Name).
		SetScopeType(lifecyclehook.ScopeType(h.ScopeType)).
		SetTrigger(lifecyclehook.Trigger(h.Trigger)).
		SetEnabled(h.Enabled).
		SetStateVersion(h.StateVersion)

	if h.ScopeID != "" {
		create.SetScopeID(h.ScopeID)
	}
	if h.Selector != nil {
		create.SetSelector(storeSelectorToEnt(h.Selector))
	}
	if h.Action != nil {
		create.SetAction(storeActionToEnt(h.Action))
	}
	if h.ExecutionIdentity != "" {
		create.SetExecutionIdentity(h.ExecutionIdentity)
	}
	if h.CreatedBy != "" {
		create.SetCreatedBy(h.CreatedBy)
	}

	created, err := create.Save(ctx)
	if err != nil {
		return mapError(err)
	}

	h.Created = created.Created
	h.Updated = created.Updated
	h.StateVersion = created.StateVersion
	return nil
}

// GetLifecycleHook retrieves a lifecycle hook by ID.
func (s *LifecycleHookStore) GetLifecycleHook(ctx context.Context, id string) (*store.LifecycleHook, error) {
	uid, err := parseUUID(id)
	if err != nil {
		return nil, err
	}

	h, err := s.client.LifecycleHook.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}

	return entLifecycleHookToStore(h), nil
}

// UpdateLifecycleHook updates an existing lifecycle hook using optimistic
// locking via StateVersion. The update only matches rows whose current
// state_version equals the caller's expected version; on success the version
// is incremented.
func (s *LifecycleHookStore) UpdateLifecycleHook(ctx context.Context, h *store.LifecycleHook) error {
	uid, err := parseUUID(h.ID)
	if err != nil {
		return err
	}

	newVersion := h.StateVersion + 1

	update := s.client.LifecycleHook.Update().
		Where(
			lifecyclehook.IDEQ(uid),
			lifecyclehook.StateVersionEQ(h.StateVersion),
		).
		SetName(h.Name).
		SetScopeType(lifecyclehook.ScopeType(h.ScopeType)).
		SetTrigger(lifecyclehook.Trigger(h.Trigger)).
		SetEnabled(h.Enabled).
		SetStateVersion(newVersion)

	if h.ScopeID != "" {
		update.SetScopeID(h.ScopeID)
	} else {
		update.ClearScopeID()
	}
	if h.Selector != nil {
		update.SetSelector(storeSelectorToEnt(h.Selector))
	} else {
		update.ClearSelector()
	}
	if h.Action != nil {
		update.SetAction(storeActionToEnt(h.Action))
	} else {
		update.ClearAction()
	}
	if h.ExecutionIdentity != "" {
		update.SetExecutionIdentity(h.ExecutionIdentity)
	} else {
		update.ClearExecutionIdentity()
	}
	if h.CreatedBy != "" {
		update.SetCreatedBy(h.CreatedBy)
	}

	affected, err := update.Save(ctx)
	if err != nil {
		return mapError(err)
	}
	if affected == 0 {
		// No row matched id+version. Distinguish "not found" from "conflict".
		exists, existErr := s.client.LifecycleHook.Query().
			Where(lifecyclehook.IDEQ(uid)).
			Exist(ctx)
		if existErr != nil {
			return existErr
		}
		if !exists {
			return store.ErrNotFound
		}
		return store.ErrVersionConflict
	}

	// Reload to surface the server-managed updated timestamp.
	updated, err := s.client.LifecycleHook.Get(ctx, uid)
	if err != nil {
		return mapError(err)
	}
	h.Updated = updated.Updated
	h.StateVersion = updated.StateVersion
	return nil
}

// DeleteLifecycleHook removes a lifecycle hook by ID.
func (s *LifecycleHookStore) DeleteLifecycleHook(ctx context.Context, id string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}

	err = s.client.LifecycleHook.DeleteOneID(uid).Exec(ctx)
	if err != nil {
		return mapError(err)
	}
	return nil
}

// ListLifecycleHooks returns lifecycle hooks matching the filter criteria.
func (s *LifecycleHookStore) ListLifecycleHooks(ctx context.Context, filter store.LifecycleHookFilter, opts store.ListOptions) (*store.ListResult[store.LifecycleHook], error) {
	query := s.client.LifecycleHook.Query()

	if filter.ScopeType != "" {
		query.Where(lifecyclehook.ScopeTypeEQ(lifecyclehook.ScopeType(filter.ScopeType)))
	}
	if filter.ScopeID != "" {
		query.Where(lifecyclehook.ScopeIDEQ(filter.ScopeID))
	}
	if filter.Trigger != "" {
		query.Where(lifecyclehook.TriggerEQ(lifecyclehook.Trigger(filter.Trigger)))
	}
	if filter.Enabled != nil {
		query.Where(lifecyclehook.EnabledEQ(*filter.Enabled))
	}

	totalCount, err := query.Clone().Count(ctx)
	if err != nil {
		return nil, err
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}

	hooks, err := query.
		Order(lifecyclehook.ByCreated()).
		Limit(limit).
		All(ctx)
	if err != nil {
		return nil, err
	}

	items := make([]store.LifecycleHook, 0, len(hooks))
	for _, h := range hooks {
		items = append(items, *entLifecycleHookToStore(h))
	}

	return &store.ListResult[store.LifecycleHook]{
		Items:      items,
		TotalCount: totalCount,
	}, nil
}

// CompareAndSetHookPhase atomically records newPhase as the last-processed
// phase for the given agent. Returns changed=true only when the phase actually
// changed (or the row was inserted for the first time).
//
// The implementation uses a transaction with SELECT FOR UPDATE (Postgres) or
// implicit serialization (SQLite) to achieve atomicity across concurrent hub
// instances.
func (s *LifecycleHookStore) CompareAndSetHookPhase(ctx context.Context, agentID, newPhase string) (bool, error) {
	// Detect dialect BEFORE opening a transaction — with SQLite's
	// MaxOpenConns=1 the dialect-probe query would deadlock if the
	// tx already held the single connection.
	useLock := s.usesRowLocks(ctx)

	tx, err := s.client.Tx(ctx)
	if err != nil {
		return false, fmt.Errorf("compare-and-set hook phase: begin tx: %w", err)
	}
	// Rollback is a no-op after Commit succeeds.
	defer tx.Rollback()

	// Query for existing row. ForUpdate serialises concurrent CAS
	// attempts in Postgres; in SQLite the single-writer lock suffices
	// and ForUpdate is not supported.
	q := tx.LifecycleHookAgentPhase.Query().
		Where(lifecyclehookagentphase.AgentIDEQ(agentID))
	if useLock {
		q = q.ForUpdate()
	}
	existing, err := q.Only(ctx)

	if ent.IsNotFound(err) {
		// No existing row — first transition for this agent.
		if err := tx.LifecycleHookAgentPhase.Create().
			SetAgentID(agentID).
			SetLastPhase(newPhase).
			Exec(ctx); err != nil {
			// On Postgres, a concurrent first-insert race means two
			// transactions both see NotFound and attempt INSERT. The
			// loser hits a unique-constraint violation. This is safe
			// (no double-fire) — treat it as "another instance won the
			// first insert" and return changed=false.
			if ent.IsConstraintError(err) {
				// The tx is now poisoned; rollback is handled by defer.
				return false, nil
			}
			return false, fmt.Errorf("compare-and-set hook phase: insert: %w", err)
		}
		// Only report a transition if the commit actually succeeds. A failed
		// commit rolls the insert back, so returning true would falsely signal
		// a recorded transition and could cause a duplicate hook firing. A
		// deferred unique-constraint violation at commit time means another
		// instance won the first insert — safe, treat as no transition.
		if err := tx.Commit(); err != nil {
			if ent.IsConstraintError(err) {
				return false, nil
			}
			return false, fmt.Errorf("compare-and-set hook phase: commit insert: %w", err)
		}
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("compare-and-set hook phase: query: %w", err)
	}

	// Row exists — no-op if the phase is the same.
	if existing.LastPhase == newPhase {
		return false, tx.Commit()
	}

	// Phase differs — update.
	if err := tx.LifecycleHookAgentPhase.UpdateOneID(existing.ID).
		SetLastPhase(newPhase).
		Exec(ctx); err != nil {
		return false, fmt.Errorf("compare-and-set hook phase: update: %w", err)
	}
	// As with the insert path, only report a transition if the commit lands —
	// a failed commit rolls back the update.
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("compare-and-set hook phase: commit update: %w", err)
	}
	return true, nil
}

// DeleteHookPhase removes the stored last-processed phase for an agent.
// No error is returned if the row does not exist.
func (s *LifecycleHookStore) DeleteHookPhase(ctx context.Context, agentID string) error {
	_, err := s.client.LifecycleHookAgentPhase.Delete().
		Where(lifecyclehookagentphase.AgentIDEQ(agentID)).
		Exec(ctx)
	return err
}
