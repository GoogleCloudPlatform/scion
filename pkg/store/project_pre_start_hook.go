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

// ProjectPreStartHook is a named shell script registered against a project.
// When an agent is created in the project the active hook's script content is
// inlined into AgentAppliedConfig and later staged by the broker at
// $HOME/.scion/hooks/pre-start.d/30-project-custom before the container starts.
type ProjectPreStartHook struct {
	ID          string    `json:"id"`
	ProjectID   string    `json:"projectId"`
	Name        string    `json:"name"`
	Slug        string    `json:"slug"`
	Description string    `json:"description,omitempty"`
	// Script is the raw script content (e.g. #!/bin/sh ...).
	// Bounded to 64 KB at the Hub API layer.
	Script      string    `json:"script"`
	Status      string    `json:"status"` // "active" | "archived"
	CreatedBy   string    `json:"createdBy,omitempty"`
	UpdatedBy   string    `json:"updatedBy,omitempty"`
	Created     time.Time `json:"created"`
	Updated     time.Time `json:"updated"`
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

	// DeleteProjectPreStartHook hard-deletes a hook. Returns store.ErrInvalidInput
	// if the hook is currently active (caller must archive it first).
	DeleteProjectPreStartHook(ctx context.Context, hookID, projectID string) error
}
