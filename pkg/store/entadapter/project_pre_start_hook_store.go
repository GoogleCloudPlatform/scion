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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/predicate"
	entpsh "github.com/GoogleCloudPlatform/scion/pkg/ent/projectprestarthook"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ProjectPreStartHookStore implements store.ProjectPreStartHookStore using Ent ORM.
//
// Both project-scoped and hub-scoped hooks live in the same table, told apart
// by the `scope` column. Every query below is scope-qualified so the two
// flavours can never leak into each other's result sets.
type ProjectPreStartHookStore struct {
	client *ent.Client
}

// NewProjectPreStartHookStore creates a new Ent-backed ProjectPreStartHookStore.
func NewProjectPreStartHookStore(client *ent.Client) *ProjectPreStartHookStore {
	return &ProjectPreStartHookStore{client: client}
}

// entPSHToStore converts an Ent ProjectPreStartHook entity to the store model.
func entPSHToStore(e *ent.ProjectPreStartHook) *store.ProjectPreStartHook {
	return &store.ProjectPreStartHook{
		ID:          e.ID.String(),
		Scope:       string(e.Scope),
		ProjectID:   e.ProjectID,
		Name:        e.Name,
		Slug:        e.Slug,
		Description: e.Description,
		Script:      e.Script,
		Status:      string(e.Status),
		CreatedBy:   e.CreatedBy,
		UpdatedBy:   e.UpdatedBy,
		Created:     e.Created,
		Updated:     e.Updated,
	}
}

// pshScopePreds builds the predicates that confine a query to a single scope.
// For project scope the project_id is included; hub-scoped rows carry an empty
// project_id and are identified by the scope column alone.
func pshScopePreds(scope entpsh.Scope, projectID string) []predicate.ProjectPreStartHook {
	preds := []predicate.ProjectPreStartHook{entpsh.ScopeEQ(scope)}
	if scope == entpsh.ScopeProject {
		preds = append(preds, entpsh.ProjectID(projectID))
	}
	return preds
}

// getActiveHook returns the single active hook within a scope.
func (s *ProjectPreStartHookStore) getActiveHook(ctx context.Context, scope entpsh.Scope, projectID string) (*store.ProjectPreStartHook, error) {
	preds := append(pshScopePreds(scope, projectID), entpsh.StatusEQ(entpsh.StatusActive))
	e, err := s.client.ProjectPreStartHook.Query().Where(preds...).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, store.ErrNotFound
		}
		return nil, fmt.Errorf("get active %s pre-start hook: %w", scope, err)
	}
	return entPSHToStore(e), nil
}

// getHook returns a specific hook by ID within a scope.
func (s *ProjectPreStartHookStore) getHook(ctx context.Context, scope entpsh.Scope, hookID, projectID string) (*store.ProjectPreStartHook, error) {
	uid, err := parseUUID(hookID)
	if err != nil {
		return nil, store.ErrNotFound
	}
	preds := append(pshScopePreds(scope, projectID), entpsh.ID(uid))
	e, err := s.client.ProjectPreStartHook.Query().Where(preds...).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, store.ErrNotFound
		}
		return nil, fmt.Errorf("get %s pre-start hook: %w", scope, err)
	}
	return entPSHToStore(e), nil
}

// listHooks returns all hooks within a scope, newest first.
func (s *ProjectPreStartHookStore) listHooks(ctx context.Context, scope entpsh.Scope, projectID string) ([]*store.ProjectPreStartHook, error) {
	rows, err := s.client.ProjectPreStartHook.Query().
		Where(pshScopePreds(scope, projectID)...).
		Order(ent.Desc(entpsh.FieldCreated)).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("list %s pre-start hooks: %w", scope, err)
	}
	out := make([]*store.ProjectPreStartHook, len(rows))
	for i, e := range rows {
		out[i] = entPSHToStore(e)
	}
	return out, nil
}

// createHook creates a new hook within a scope and atomically archives the
// scope's existing active hook. The new hook is always created with status
// "active"; passing any other status is rejected to prevent callers from
// inserting an archived hook without going through the archive-on-create
// semantics.
func (s *ProjectPreStartHookStore) createHook(ctx context.Context, scope entpsh.Scope, hook *store.ProjectPreStartHook) (*store.ProjectPreStartHook, error) {
	projectID := ""
	if scope == entpsh.ScopeProject {
		if hook.ProjectID == "" {
			return nil, fmt.Errorf("%w: project-scoped pre-start hook requires a project ID", store.ErrInvalidInput)
		}
		projectID = hook.ProjectID
	}

	now := time.Now()
	hook.Created = now
	hook.Updated = now
	hook.Scope = string(scope)
	hook.ProjectID = projectID
	// Normalise: treat empty status as active; reject any other value.
	if hook.Status == "" {
		hook.Status = store.ProjectPreStartHookStatusActive
	}
	if hook.Status != store.ProjectPreStartHookStatusActive {
		return nil, fmt.Errorf("%w: new hooks must be created with status %q, got %q",
			store.ErrInvalidInput, store.ProjectPreStartHookStatusActive, hook.Status)
	}

	tx, err := s.client.Tx(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}

	// Archive any existing active hook in the same scope.
	archivePreds := append(pshScopePreds(scope, projectID), entpsh.StatusEQ(entpsh.StatusActive))
	if err := tx.ProjectPreStartHook.Update().
		Where(archivePreds...).
		SetStatus(entpsh.StatusArchived).
		SetUpdated(now).
		Exec(ctx); err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("archive existing active hooks: %w", err)
	}

	create := tx.ProjectPreStartHook.Create().
		SetScope(scope).
		SetProjectID(projectID).
		SetName(hook.Name).
		SetSlug(hook.Slug).
		SetScript(hook.Script).
		SetStatus(entpsh.Status(hook.Status)).
		SetCreated(hook.Created).
		SetUpdated(hook.Updated)
	if hook.ID != "" {
		uid, err := parseUUID(hook.ID)
		if err != nil {
			_ = tx.Rollback()
			return nil, fmt.Errorf("%w: invalid hook ID: %w", store.ErrInvalidInput, err)
		}
		create = create.SetID(uid)
	}
	if hook.Description != "" {
		create = create.SetDescription(hook.Description)
	}
	if hook.CreatedBy != "" {
		create = create.SetCreatedBy(hook.CreatedBy)
	}
	if hook.UpdatedBy != "" {
		create = create.SetUpdatedBy(hook.UpdatedBy)
	}

	e, err := create.Save(ctx)
	if err != nil {
		_ = tx.Rollback()
		if ent.IsConstraintError(err) {
			return nil, fmt.Errorf("%w: slug %q already exists in %s scope", store.ErrAlreadyExists, hook.Slug, scope)
		}
		return nil, fmt.Errorf("create %s pre-start hook: %w", scope, err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	return entPSHToStore(e), nil
}

// updateHook updates the mutable fields of a hook within a scope.
func (s *ProjectPreStartHookStore) updateHook(ctx context.Context, scope entpsh.Scope, hook *store.ProjectPreStartHook) (*store.ProjectPreStartHook, error) {
	uid, err := parseUUID(hook.ID)
	if err != nil {
		return nil, store.ErrNotFound
	}

	now := time.Now()
	// The scope (and, for project scope, the project-ID) predicates ensure a
	// caller cannot update a hook outside the scope they addressed, even if
	// they somehow have the UUID.
	upd := s.client.ProjectPreStartHook.UpdateOneID(uid).
		Where(pshScopePreds(scope, hook.ProjectID)...).
		SetUpdated(now)
	if hook.Name != "" {
		upd = upd.SetName(hook.Name)
	}
	if hook.Script != "" {
		upd = upd.SetScript(hook.Script)
	}
	// Description can be explicitly cleared by setting to "".
	upd = upd.SetDescription(hook.Description)
	if hook.UpdatedBy != "" {
		upd = upd.SetUpdatedBy(hook.UpdatedBy)
	}

	e, err := upd.Save(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, store.ErrNotFound
		}
		return nil, fmt.Errorf("update %s pre-start hook: %w", scope, err)
	}
	return entPSHToStore(e), nil
}

// activateHook sets the identified hook to "active" and archives all other
// hooks in the same scope atomically.
func (s *ProjectPreStartHookStore) activateHook(ctx context.Context, scope entpsh.Scope, hookID, projectID string) (*store.ProjectPreStartHook, error) {
	uid, err := parseUUID(hookID)
	if err != nil {
		return nil, store.ErrNotFound
	}

	tx, err := s.client.Tx(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}

	now := time.Now()

	// Archive all currently-active hooks in this scope.
	archivePreds := append(pshScopePreds(scope, projectID), entpsh.StatusEQ(entpsh.StatusActive))
	if err := tx.ProjectPreStartHook.Update().
		Where(archivePreds...).
		SetStatus(entpsh.StatusArchived).
		SetUpdated(now).
		Exec(ctx); err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("archive existing active hooks: %w", err)
	}

	// Activate the target hook.
	e, err := tx.ProjectPreStartHook.UpdateOneID(uid).
		Where(pshScopePreds(scope, projectID)...).
		SetStatus(entpsh.StatusActive).
		SetUpdated(now).
		Save(ctx)
	if err != nil {
		_ = tx.Rollback()
		if ent.IsNotFound(err) {
			return nil, store.ErrNotFound
		}
		return nil, fmt.Errorf("activate %s pre-start hook: %w", scope, err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	return entPSHToStore(e), nil
}

// deleteHook hard-deletes a hook within a scope. Returns store.ErrInvalidInput
// if the hook is currently active AND is not the only hook in the scope.
// Deleting the last remaining active hook (with no archived hooks to fall back
// to) is allowed so that operators can fully remove all pre-start hooks.
//
// The read, the guard count, and the delete all run inside one transaction:
// evaluated separately, two concurrent deletes of the last two hooks in a scope
// could each observe total > 1, each be rejected, or — with different
// interleavings — both proceed and leave the scope empty unintentionally.
func (s *ProjectPreStartHookStore) deleteHook(ctx context.Context, scope entpsh.Scope, hookID, projectID string) error {
	uid, err := parseUUID(hookID)
	if err != nil {
		return store.ErrNotFound
	}

	tx, err := s.client.Tx(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}

	// Verify the hook exists in this scope and check its status.
	preds := append(pshScopePreds(scope, projectID), entpsh.ID(uid))
	e, err := tx.ProjectPreStartHook.Query().Where(preds...).Only(ctx)
	if err != nil {
		_ = tx.Rollback()
		if ent.IsNotFound(err) {
			return store.ErrNotFound
		}
		return fmt.Errorf("get %s pre-start hook for delete: %w", scope, err)
	}

	// If the hook is active, only reject the delete when there are other hooks
	// still in the scope. If this is the last/only hook, a hard delete is
	// allowed so operators can fully clear all pre-start hooks.
	if e.Status == entpsh.StatusActive {
		total, err := tx.ProjectPreStartHook.Query().
			Where(pshScopePreds(scope, projectID)...).
			Count(ctx)
		if err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("count hooks for delete guard: %w", err)
		}
		if total > 1 {
			_ = tx.Rollback()
			return fmt.Errorf("%w: cannot delete an active hook while other hooks exist; activate another hook first", store.ErrInvalidInput)
		}
		// total == 1: this is the only hook — fall through to delete.
	}

	if err := tx.ProjectPreStartHook.DeleteOneID(uid).Exec(ctx); err != nil {
		_ = tx.Rollback()
		if ent.IsNotFound(err) {
			return store.ErrNotFound
		}
		return fmt.Errorf("delete %s pre-start hook: %w", scope, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// =============================================================================
// Project-scoped API
// =============================================================================

// GetActiveProjectPreStartHook returns the single active hook for a project.
func (s *ProjectPreStartHookStore) GetActiveProjectPreStartHook(ctx context.Context, projectID string) (*store.ProjectPreStartHook, error) {
	return s.getActiveHook(ctx, entpsh.ScopeProject, projectID)
}

// GetProjectPreStartHook returns a specific hook by ID within a project.
func (s *ProjectPreStartHookStore) GetProjectPreStartHook(ctx context.Context, hookID, projectID string) (*store.ProjectPreStartHook, error) {
	return s.getHook(ctx, entpsh.ScopeProject, hookID, projectID)
}

// ListProjectPreStartHooks returns all hooks for a project (all statuses),
// ordered by creation time descending.
func (s *ProjectPreStartHookStore) ListProjectPreStartHooks(ctx context.Context, projectID string) ([]*store.ProjectPreStartHook, error) {
	return s.listHooks(ctx, entpsh.ScopeProject, projectID)
}

// CreateProjectPreStartHook creates a new hook and atomically archives any
// existing active hook for the same project.
func (s *ProjectPreStartHookStore) CreateProjectPreStartHook(ctx context.Context, hook *store.ProjectPreStartHook) (*store.ProjectPreStartHook, error) {
	return s.createHook(ctx, entpsh.ScopeProject, hook)
}

// UpdateProjectPreStartHook updates the mutable fields of a hook.
func (s *ProjectPreStartHookStore) UpdateProjectPreStartHook(ctx context.Context, hook *store.ProjectPreStartHook) (*store.ProjectPreStartHook, error) {
	return s.updateHook(ctx, entpsh.ScopeProject, hook)
}

// ActivateProjectPreStartHook sets the identified hook to "active" and archives
// all other hooks for the same project atomically.
func (s *ProjectPreStartHookStore) ActivateProjectPreStartHook(ctx context.Context, hookID, projectID string) (*store.ProjectPreStartHook, error) {
	return s.activateHook(ctx, entpsh.ScopeProject, hookID, projectID)
}

// DeleteProjectPreStartHook hard-deletes a hook. Returns store.ErrInvalidInput
// if the hook is currently active AND is not the only hook in the project.
func (s *ProjectPreStartHookStore) DeleteProjectPreStartHook(ctx context.Context, hookID, projectID string) error {
	return s.deleteHook(ctx, entpsh.ScopeProject, hookID, projectID)
}

// =============================================================================
// Hub-scoped API
// =============================================================================

// GetActiveHubPreStartHook returns the single active hub-scoped hook.
func (s *ProjectPreStartHookStore) GetActiveHubPreStartHook(ctx context.Context) (*store.ProjectPreStartHook, error) {
	return s.getActiveHook(ctx, entpsh.ScopeHub, "")
}

// GetHubPreStartHook returns a specific hub-scoped hook by ID.
func (s *ProjectPreStartHookStore) GetHubPreStartHook(ctx context.Context, hookID string) (*store.ProjectPreStartHook, error) {
	return s.getHook(ctx, entpsh.ScopeHub, hookID, "")
}

// ListHubPreStartHooks returns all hub-scoped hooks (all statuses), ordered by
// creation time descending.
func (s *ProjectPreStartHookStore) ListHubPreStartHooks(ctx context.Context) ([]*store.ProjectPreStartHook, error) {
	return s.listHooks(ctx, entpsh.ScopeHub, "")
}

// CreateHubPreStartHook creates a new hub-scoped hook and atomically archives
// any existing active hub-scoped hook.
func (s *ProjectPreStartHookStore) CreateHubPreStartHook(ctx context.Context, hook *store.ProjectPreStartHook) (*store.ProjectPreStartHook, error) {
	return s.createHook(ctx, entpsh.ScopeHub, hook)
}

// UpdateHubPreStartHook updates the mutable fields of a hub-scoped hook.
func (s *ProjectPreStartHookStore) UpdateHubPreStartHook(ctx context.Context, hook *store.ProjectPreStartHook) (*store.ProjectPreStartHook, error) {
	return s.updateHook(ctx, entpsh.ScopeHub, hook)
}

// ActivateHubPreStartHook sets the identified hub-scoped hook to "active" and
// archives all other hub-scoped hooks atomically.
func (s *ProjectPreStartHookStore) ActivateHubPreStartHook(ctx context.Context, hookID string) (*store.ProjectPreStartHook, error) {
	return s.activateHook(ctx, entpsh.ScopeHub, hookID, "")
}

// DeleteHubPreStartHook hard-deletes a hub-scoped hook. Returns
// store.ErrInvalidInput if the hook is currently active AND other hub-scoped
// hooks exist.
func (s *ProjectPreStartHookStore) DeleteHubPreStartHook(ctx context.Context, hookID string) error {
	return s.deleteHook(ctx, entpsh.ScopeHub, hookID, "")
}
