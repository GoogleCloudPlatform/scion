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
	"errors"
	"net/http"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const (
	// projectPreStartHookScriptMaxBytes is the maximum allowed script size (64 KB).
	projectPreStartHookScriptMaxBytes = 64 * 1024

	// projectPreStartHookActivateSuffix is the URL suffix for the activate action.
	projectPreStartHookActivateSuffix = "/activate"
)

// CreateProjectPreStartHookRequest is the body for POST /pre-start-hooks.
type CreateProjectPreStartHookRequest struct {
	Name        string `json:"name"`
	Slug        string `json:"slug,omitempty"`
	Description string `json:"description,omitempty"`
	Script      string `json:"script"`
}

// UpdateProjectPreStartHookRequest is the body for PUT /pre-start-hooks/{id}.
type UpdateProjectPreStartHookRequest struct {
	Name        *string `json:"name,omitempty"`
	Description *string `json:"description,omitempty"`
	Script      *string `json:"script,omitempty"`
}

// ListProjectPreStartHooksResponse wraps the hook list for GET /pre-start-hooks.
type ListProjectPreStartHooksResponse struct {
	Hooks []*store.ProjectPreStartHook `json:"hooks"`
}

// handleProjectPreStartHooks handles GET (list) and POST (create) on
// /api/v1/projects/{projectId}/pre-start-hooks.
func (s *Server) handleProjectPreStartHooks(w http.ResponseWriter, r *http.Request, projectID string) {
	ctx := r.Context()

	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Project")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return
	}

	switch r.Method {
	case http.MethodGet:
		if userIdent, ok := identity.(UserIdentity); ok {
			decision := s.authzService.CheckAccess(ctx, userIdent, Resource{
				Type:    "project",
				ID:      project.ID,
				OwnerID: project.OwnerID,
			}, ActionRead)
			if !decision.Allowed {
				Forbidden(w)
				return
			}
		}

		hooks, err := s.store.ListProjectPreStartHooks(ctx, projectID)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		writeJSON(w, http.StatusOK, &ListProjectPreStartHooksResponse{Hooks: hooks})

	case http.MethodPost:
		if userIdent, ok := identity.(UserIdentity); ok {
			decision := s.authzService.CheckAccess(ctx, userIdent, Resource{
				Type:    "project",
				ID:      project.ID,
				OwnerID: project.OwnerID,
			}, ActionUpdate)
			if !decision.Allowed {
				Forbidden(w)
				return
			}
		} else {
			Forbidden(w)
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, projectPreStartHookScriptMaxBytes+4096)
		var req CreateProjectPreStartHookRequest
		if err := readJSON(r, &req); err != nil {
			BadRequest(w, "Invalid request body: "+err.Error())
			return
		}
		if req.Name == "" {
			ValidationError(w, "name is required", nil)
			return
		}
		if req.Script == "" {
			ValidationError(w, "script is required", nil)
			return
		}
		if len(req.Script) > projectPreStartHookScriptMaxBytes {
			BadRequest(w, "script exceeds 64 KB size limit")
			return
		}

		// Always run through Slugify so that a caller-supplied slug is
		// normalised (lowercased, special chars stripped) just as a
		// name-derived slug is. This prevents spaces, slashes, or other
		// URL-hostile characters from reaching the store.
		slug := req.Slug
		if slug == "" {
			slug = api.Slugify(req.Name)
		} else {
			slug = api.Slugify(slug)
		}
		if slug == "" {
			ValidationError(w, "slug is required (or provide a name that can be slugified)", nil)
			return
		}

		var createdBy string
		if userIdent, ok := identity.(UserIdentity); ok {
			createdBy = userIdent.Email()
		}

		hook, err := s.store.CreateProjectPreStartHook(ctx, &store.ProjectPreStartHook{
			ProjectID:   projectID,
			Name:        req.Name,
			Slug:        slug,
			Description: req.Description,
			Script:      req.Script,
			CreatedBy:   createdBy,
			UpdatedBy:   createdBy,
		})
		if err != nil {
			if errors.Is(err, store.ErrAlreadyExists) {
				Conflict(w, "a hook with this slug already exists in the project")
				return
			}
			writeErrorFromErr(w, err, "")
			return
		}
		writeJSON(w, http.StatusCreated, hook)

	default:
		MethodNotAllowed(w)
	}
}

// handleProjectPreStartHookByID handles requests on
// /api/v1/projects/{projectId}/pre-start-hooks/{hookId} and
// /api/v1/projects/{projectId}/pre-start-hooks/{hookId}/activate.
func (s *Server) handleProjectPreStartHookByID(w http.ResponseWriter, r *http.Request, projectID, hookPath string) {
	ctx := r.Context()

	// Split off the /activate suffix if present.
	isActivate := strings.HasSuffix(hookPath, projectPreStartHookActivateSuffix)
	hookID := hookPath
	if isActivate {
		hookID = strings.TrimSuffix(hookPath, projectPreStartHookActivateSuffix)
		hookID = strings.TrimSuffix(hookID, "/")
	}
	if hookID == "" {
		NotFound(w, "ProjectPreStartHook")
		return
	}

	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Project")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return
	}

	projectRes := Resource{
		Type:    "project",
		ID:      project.ID,
		OwnerID: project.OwnerID,
	}

	// POST .../activate
	if isActivate {
		if r.Method != http.MethodPost {
			MethodNotAllowed(w)
			return
		}
		if userIdent, ok := identity.(UserIdentity); ok {
			if !s.authzService.CheckAccess(ctx, userIdent, projectRes, ActionUpdate).Allowed {
				Forbidden(w)
				return
			}
		} else {
			Forbidden(w)
			return
		}

		hook, err := s.store.ActivateProjectPreStartHook(ctx, hookID, projectID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				NotFound(w, "ProjectPreStartHook")
				return
			}
			writeErrorFromErr(w, err, "")
			return
		}
		writeJSON(w, http.StatusOK, hook)
		return
	}

	switch r.Method {
	case http.MethodGet:
		if userIdent, ok := identity.(UserIdentity); ok {
			if !s.authzService.CheckAccess(ctx, userIdent, projectRes, ActionRead).Allowed {
				Forbidden(w)
				return
			}
		}
		hook, err := s.store.GetProjectPreStartHook(ctx, hookID, projectID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				NotFound(w, "ProjectPreStartHook")
				return
			}
			writeErrorFromErr(w, err, "")
			return
		}
		writeJSON(w, http.StatusOK, hook)

	case http.MethodPut:
		if userIdent, ok := identity.(UserIdentity); ok {
			if !s.authzService.CheckAccess(ctx, userIdent, projectRes, ActionUpdate).Allowed {
				Forbidden(w)
				return
			}
		} else {
			Forbidden(w)
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, projectPreStartHookScriptMaxBytes+4096)
		var req UpdateProjectPreStartHookRequest
		if err := readJSON(r, &req); err != nil {
			BadRequest(w, "Invalid request body: "+err.Error())
			return
		}
		if req.Script != nil && len(*req.Script) > projectPreStartHookScriptMaxBytes {
			BadRequest(w, "script exceeds 64 KB size limit")
			return
		}
		if req.Name != nil && *req.Name == "" {
			ValidationError(w, "name cannot be empty", nil)
			return
		}

		existing, err := s.store.GetProjectPreStartHook(ctx, hookID, projectID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				NotFound(w, "ProjectPreStartHook")
				return
			}
			writeErrorFromErr(w, err, "")
			return
		}

		// Apply partial updates.
		updated := &store.ProjectPreStartHook{ID: existing.ID, ProjectID: existing.ProjectID}
		if req.Name != nil {
			updated.Name = *req.Name
		} else {
			updated.Name = existing.Name
		}
		if req.Description != nil {
			updated.Description = *req.Description
		} else {
			updated.Description = existing.Description
		}
		if req.Script != nil {
			updated.Script = *req.Script
		} else {
			updated.Script = existing.Script
		}
		if userIdent, ok := identity.(UserIdentity); ok {
			updated.UpdatedBy = userIdent.Email()
		}

		if updated.Script == "" {
			ValidationError(w, "script cannot be set to empty", nil)
			return
		}

		hook, err := s.store.UpdateProjectPreStartHook(ctx, updated)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				NotFound(w, "ProjectPreStartHook")
				return
			}
			writeErrorFromErr(w, err, "")
			return
		}
		writeJSON(w, http.StatusOK, hook)

	case http.MethodDelete:
		if userIdent, ok := identity.(UserIdentity); ok {
			if !s.authzService.CheckAccess(ctx, userIdent, projectRes, ActionUpdate).Allowed {
				Forbidden(w)
				return
			}
		} else {
			Forbidden(w)
			return
		}

		if err := s.store.DeleteProjectPreStartHook(ctx, hookID, projectID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				NotFound(w, "ProjectPreStartHook")
				return
			}
			if errors.Is(err, store.ErrInvalidInput) {
				BadRequest(w, "cannot delete an active hook while other hooks exist; activate another hook first, then delete this one")
				return
			}
			writeErrorFromErr(w, err, "")
			return
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		MethodNotAllowed(w)
	}
}
