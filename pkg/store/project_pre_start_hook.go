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

package store

import (
	"context"
	"time"
)

// Pre-start hook status constants.
const (
	ProjectPreStartHookStatusActive   = "active"
	ProjectPreStartHookStatusArchived = "archived"
)

// Pre-start hook scope constants. A hook is either attached to a single
// project ("project") or to the hub as a whole ("hub"). The hub-scoped hook
// acts as the fallback for projects that have no active project-scoped hook.
const (
	PreStartHookScopeProject = "project"
	PreStartHookScopeHub     = "hub"
)

// ProjectPreStartHook is a named shell script registered against a project or
// against the hub as a whole. When an agent is created the active hook's
// script content is inlined into AgentAppliedConfig and later staged by the
// broker at $HOME/.scion/hooks/pre-start.d/30-project-custom before the
// container starts.
//
// Scope selects between the two flavours: "project" hooks carry a ProjectID
// and apply to that project only; "hub" hooks have an empty ProjectID and
// apply to every project that has no active project-scoped hook.
type ProjectPreStartHook struct {
	ID string `json:"id"`
	// Scope is "project" or "hub". Empty is treated as "project" by the
	// store layer for backwards compatibility.
	Scope       string `json:"scope"`
	ProjectID   string `json:"projectId"`
	Name        string `json:"name"`
	Slug        string `json:"slug"`
	Description string `json:"description,omitempty"`
	// Script is the raw script content (e.g. #!/bin/sh ...).
	// Bounded to 64 KB at the Hub API layer.
	Script    string    `json:"script"`
	Status    string    `json:"status"` // "active" | "archived"
	CreatedBy string    `json:"createdBy,omitempty"`
	UpdatedBy string    `json:"updatedBy,omitempty"`
	Created   time.Time `json:"created"`
	Updated   time.Time `json:"updated"`
}

// ProjectPreStartHookStore defines project pre-start hook persistence operations.
type ProjectPreStartHookStore interface {
	// GetActiveProjectPreStartHook returns the single active hook for a project.
	// Returns store.ErrNotFound if no active hook is registered.
	GetActiveProjectPreStartHook(ctx context.Context, projectID string) (*ProjectPreStartHook, error)

	// GetProjectPreStartHook returns a specific hook by ID within a project.
	GetProjectPreStartHook(ctx context.Context, hookID, projectID string) (*ProjectPreStartHook, error)

	// ListProjectPreStartHooks returns all hooks for a project (all statuses),
	// ordered by creation time descending.
	ListProjectPreStartHooks(ctx context.Context, projectID string) ([]*ProjectPreStartHook, error)

	// CreateProjectPreStartHook creates a new hook and archives any existing
	// active hook for the same project atomically.
	CreateProjectPreStartHook(ctx context.Context, hook *ProjectPreStartHook) (*ProjectPreStartHook, error)

	// UpdateProjectPreStartHook updates the mutable fields of a hook (name,
	// description, script). Does not change status; call ActivateProjectPreStartHook
	// to change status.
	UpdateProjectPreStartHook(ctx context.Context, hook *ProjectPreStartHook) (*ProjectPreStartHook, error)

	// ActivateProjectPreStartHook sets the identified hook to "active" and
	// archives all other hooks for the same project atomically.
	ActivateProjectPreStartHook(ctx context.Context, hookID, projectID string) (*ProjectPreStartHook, error)

	// DeleteProjectPreStartHook hard-deletes a project-scoped hook.
	// Returns store.ErrInvalidInput if the hook is active and other hooks exist
	// in the same project scope (activate another hook first). Deleting the last
	// active hook (when it is the sole hook) is permitted so that operators can
	// fully clear a project's pre-start hooks.
	DeleteProjectPreStartHook(ctx context.Context, hookID, projectID string) error

	// --- Hub-scoped hooks -------------------------------------------------
	//
	// Hub-scoped hooks live in the same table with scope="hub" and an empty
	// project_id. They never appear in the project-scoped methods above, and
	// project-scoped hooks never appear in the methods below.

	// GetActiveHubPreStartHook returns the single active hub-scoped hook.
	// Returns store.ErrNotFound if no active hub hook is registered.
	GetActiveHubPreStartHook(ctx context.Context) (*ProjectPreStartHook, error)

	// GetHubPreStartHook returns a specific hub-scoped hook by ID.
	// Returns store.ErrNotFound if the ID does not identify a hub-scoped hook.
	GetHubPreStartHook(ctx context.Context, hookID string) (*ProjectPreStartHook, error)

	// ListHubPreStartHooks returns all hub-scoped hooks (all statuses),
	// ordered by creation time descending.
	ListHubPreStartHooks(ctx context.Context) ([]*ProjectPreStartHook, error)

	// CreateHubPreStartHook creates a new hub-scoped hook and archives any
	// existing active hub-scoped hook atomically.
	CreateHubPreStartHook(ctx context.Context, hook *ProjectPreStartHook) (*ProjectPreStartHook, error)

	// UpdateHubPreStartHook updates the mutable fields of a hub-scoped hook
	// (name, description, script). Does not change status; call
	// ActivateHubPreStartHook to change status.
	UpdateHubPreStartHook(ctx context.Context, hook *ProjectPreStartHook) (*ProjectPreStartHook, error)

	// ActivateHubPreStartHook sets the identified hub-scoped hook to "active"
	// and archives all other hub-scoped hooks atomically.
	ActivateHubPreStartHook(ctx context.Context, hookID string) (*ProjectPreStartHook, error)

	// DeleteHubPreStartHook hard-deletes a hub-scoped hook. Returns
	// store.ErrInvalidInput if the hook is currently active and other
	// hub-scoped hooks exist (activate another hook first). Deleting the last
	// remaining hub hook is allowed so operators can fully clear the hub hook.
	DeleteHubPreStartHook(ctx context.Context, hookID string) error
}
