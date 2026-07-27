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
	entpsh "github.com/GoogleCloudPlatform/scion/pkg/ent/projectprestarthook"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ProjectPreStartHookStore implements store.ProjectPreStartHookStore using Ent ORM.
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

// GetActiveProjectPreStartHook returns the single active hook for a project.
func (s *ProjectPreStartHookStore) GetActiveProjectPreStartHook(ctx context.Context, projectID string) (*store.ProjectPreStartHook, error) {
	e, err := s.client.ProjectPreStartHook.Query().
		Where(
			entpsh.ProjectID(projectID),
			entpsh.StatusEQ(entpsh.StatusActive),
		).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, store.ErrNotFound
		}
		return nil, fmt.Errorf("get active project pre-start hook: %w", err)
	}
	return entPSHToStore(e), nil
}

// GetProjectPreStartHook returns a specific hook by ID within a project.
func (s *ProjectPreStartHookStore) GetProjectPreStartHook(ctx context.Context, hookID, projectID string) (*store.ProjectPreStartHook, error) {
	uid, err := parseUUID(hookID)
	if err != nil {
		return nil, store.ErrNotFound
	}
	e, err := s.client.ProjectPreStartHook.Query().
		Where(
			entpsh.ID(uid),
			entpsh.ProjectID(projectID),
		).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, store.ErrNotFound
		}
		return nil, fmt.Errorf("get project pre-start hook: %w", err)
	}
	return entPSHToStore(e), nil
}

// ListProjectPreStartHooks returns all hooks for a project (all statuses),
// ordered by creation time descending.
func (s *ProjectPreStartHookStore) ListProjectPreStartHooks(ctx context.Context, projectID string) ([]*store.ProjectPreStartHook, error) {
	rows, err := s.client.ProjectPreStartHook.Query().
		Where(entpsh.ProjectID(projectID)).
		Order(ent.Desc(entpsh.FieldCreated)).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("list project pre-start hooks: %w", err)
	}
	out := make([]*store.ProjectPreStartHook, len(rows))
	for i, e := range rows {
		out[i] = entPSHToStore(e)
	}
	return out, nil
}

// CreateProjectPreStartHook creates a new hook and atomically archives any
// existing active hook for the same project. The new hook is always created
// with status "active"; passing any other status is rejected to prevent
// callers from inserting an archived hook without going through the
// archive-on-create semantics.
func (s *ProjectPreStartHookStore) CreateProjectPreStartHook(ctx context.Context, hook *store.ProjectPreStartHook) (*store.ProjectPreStartHook, error) {
	now := time.Now()
	hook.Created = now
	hook.Updated = now
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

	// If the new hook is active, archive any existing active hook first.
	if hook.Status == store.ProjectPreStartHookStatusActive {
		if err := tx.ProjectPreStartHook.Update().
			Where(
				entpsh.ProjectID(hook.ProjectID),
				entpsh.StatusEQ(entpsh.StatusActive),
			).
			SetStatus(entpsh.StatusArchived).
			SetUpdated(now).
			Exec(ctx); err != nil {
			_ = tx.Rollback()
			return nil, fmt.Errorf("archive existing active hooks: %w", err)
		}
	}

	create := tx.ProjectPreStartHook.Create().
		SetProjectID(hook.ProjectID).
		SetName(hook.Name).
		SetSlug(hook.Slug).
		SetScript(hook.Script).
		SetStatus(entpsh.Status(hook.Status)).
		SetCreated(hook.Created).
		SetUpdated(hook.Updated)
	if hook.ID != "" {
		uid, err := parseUUID(hook.ID)
		if err == nil {
			create = create.SetID(uid)
		}
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
			return nil, fmt.Errorf("%w: slug %q already exists in project", store.ErrAlreadyExists, hook.Slug)
		}
		return nil, fmt.Errorf("create project pre-start hook: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	return entPSHToStore(e), nil
}

// UpdateProjectPreStartHook updates the mutable fields of a hook.
func (s *ProjectPreStartHookStore) UpdateProjectPreStartHook(ctx context.Context, hook *store.ProjectPreStartHook) (*store.ProjectPreStartHook, error) {
	uid, err := parseUUID(hook.ID)
	if err != nil {
		return nil, store.ErrNotFound
	}

	now := time.Now()
	// Add a project-ID predicate so a caller cannot accidentally update a hook
	// that belongs to a different project even if they somehow have the UUID.
	upd := s.client.ProjectPreStartHook.UpdateOneID(uid).
		Where(entpsh.ProjectID(hook.ProjectID)).
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
		return nil, fmt.Errorf("update project pre-start hook: %w", err)
	}
	return entPSHToStore(e), nil
}

// ActivateProjectPreStartHook sets the identified hook to "active" and archives
// all other hooks for the same project atomically.
func (s *ProjectPreStartHookStore) ActivateProjectPreStartHook(ctx context.Context, hookID, projectID string) (*store.ProjectPreStartHook, error) {
	uid, err := parseUUID(hookID)
	if err != nil {
		return nil, store.ErrNotFound
	}

	tx, err := s.client.Tx(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}

	now := time.Now()

	// Archive all currently-active hooks for this project.
	if err := tx.ProjectPreStartHook.Update().
		Where(
			entpsh.ProjectID(projectID),
			entpsh.StatusEQ(entpsh.StatusActive),
		).
		SetStatus(entpsh.StatusArchived).
		SetUpdated(now).
		Exec(ctx); err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("archive existing active hooks: %w", err)
	}

	// Activate the target hook.
	e, err := tx.ProjectPreStartHook.UpdateOneID(uid).
		Where(entpsh.ProjectID(projectID)).
		SetStatus(entpsh.StatusActive).
		SetUpdated(now).
		Save(ctx)
	if err != nil {
		_ = tx.Rollback()
		if ent.IsNotFound(err) {
			return nil, store.ErrNotFound
		}
		return nil, fmt.Errorf("activate project pre-start hook: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	return entPSHToStore(e), nil
}

// DeleteProjectPreStartHook hard-deletes a hook. Returns store.ErrInvalidInput
// if the hook is currently active AND is not the only hook in the project.
// Deleting the last remaining active hook (with no archived hooks to fall
// back to) is allowed so that operators can fully remove all pre-start hooks.
func (s *ProjectPreStartHookStore) DeleteProjectPreStartHook(ctx context.Context, hookID, projectID string) error {
	uid, err := parseUUID(hookID)
	if err != nil {
		return store.ErrNotFound
	}

	// Verify the hook exists in this project and check its status.
	e, err := s.client.ProjectPreStartHook.Query().
		Where(
			entpsh.ID(uid),
			entpsh.ProjectID(projectID),
		).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return store.ErrNotFound
		}
		return fmt.Errorf("get project pre-start hook for delete: %w", err)
	}

	// If the hook is active, only reject the delete when there are other hooks
	// still in the project. If this is the last/only hook, a hard delete is
	// allowed so operators can fully clear all pre-start hooks.
	if e.Status == entpsh.StatusActive {
		total, err := s.client.ProjectPreStartHook.Query().
			Where(entpsh.ProjectID(projectID)).
			Count(ctx)
		if err != nil {
			return fmt.Errorf("count hooks for delete guard: %w", err)
		}
		if total > 1 {
			return fmt.Errorf("%w: cannot delete an active hook while other hooks exist; activate another hook first", store.ErrInvalidInput)
		}
		// total == 1: this is the only hook — fall through to delete.
	}

	if err := s.client.ProjectPreStartHook.DeleteOneID(uid).Exec(ctx); err != nil {
		if ent.IsNotFound(err) {
			return store.ErrNotFound
		}
		return fmt.Errorf("delete project pre-start hook: %w", err)
	}
	return nil
}
