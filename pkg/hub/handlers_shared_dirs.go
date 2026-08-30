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
	"log/slog"
	"net/http"
	"os"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// handleProjectSharedDirs handles GET/POST on /api/v1/projects/{projectId}/shared-dirs.
func (s *Server) handleProjectSharedDirs(w http.ResponseWriter, r *http.Request, projectID string) {
	ctx := r.Context()

	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		if err == store.ErrNotFound {
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
		// Project isolation runs before the authorization check so a cross-project
		// agent caller keeps its 404 and is not told the project exists.
		if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
			if project.ID != agentIdent.ProjectID() {
				NotFound(w, "Project")
				return
			}
		}
		// Read access check
		if !s.authorize(w, r, Resource{
			Type:    "project",
			ID:      project.ID,
			OwnerID: project.OwnerID,
		}, ActionRead) {
			return
		}

		dirs := project.SharedDirs
		if dirs == nil {
			dirs = []api.SharedDir{}
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"sharedDirs": dirs,
		})

	case http.MethodPost:
		// Write access check
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

		var newDir api.SharedDir
		if err := readJSON(r, &newDir); err != nil {
			BadRequest(w, "Invalid request body: "+err.Error())
			return
		}

		// Validate
		if err := api.ValidateSharedDirs([]api.SharedDir{newDir}); err != nil {
			BadRequest(w, err.Error())
			return
		}

		// Check for duplicates
		for _, d := range project.SharedDirs {
			if d.Name == newDir.Name {
				BadRequest(w, "Shared directory "+newDir.Name+" already exists")
				return
			}
		}

		project.SharedDirs = append(project.SharedDirs, newDir)
		if err := s.store.UpdateProject(ctx, project); err != nil {
			writeErrorFromErr(w, err, "")
			return
		}

		s.events.PublishProjectUpdated(ctx, project)
		writeJSON(w, http.StatusCreated, newDir)

	default:
		MethodNotAllowed(w, http.MethodGet, http.MethodPost)
	}
}

// handleProjectSharedDirByName handles DELETE on /api/v1/projects/{projectId}/shared-dirs/{name}.
func (s *Server) handleProjectSharedDirByName(w http.ResponseWriter, r *http.Request, projectID, name string) {
	ctx := r.Context()

	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		if err == store.ErrNotFound {
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

	// Write access check
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

	switch r.Method {
	case http.MethodDelete:
		found := false
		updated := make([]api.SharedDir, 0, len(project.SharedDirs))
		for _, d := range project.SharedDirs {
			if d.Name == name {
				found = true
				continue
			}
			updated = append(updated, d)
		}

		if !found {
			NotFound(w, "Shared directory")
			return
		}

		project.SharedDirs = updated
		if err := s.store.UpdateProject(ctx, project); err != nil {
			writeErrorFromErr(w, err, "")
			return
		}

		// Best-effort host directory cleanup. The CLI does this via
		// config.RemoveSharedDir(); the hub resolves the host path through
		// its co-located broker (or hub-managed project path) and removes
		// the directory directly. If the path cannot be resolved (e.g. no
		// co-located broker), we log and continue — the DB record is already
		// removed.
		resolution, resolveErr := s.resolveSharedDirPath(ctx, project, name)
		switch {
		case resolveErr != nil:
			slog.WarnContext(ctx, "could not resolve shared directory host path for cleanup",
				"project_id", projectID, "name", name, "error", resolveErr)
		case !resolution.IsLocal:
			slog.WarnContext(ctx, "shared directory path is not local, skipping host cleanup",
				"project_id", projectID, "name", name)
		case resolution.Path == "" || resolution.Path == "/":
			slog.WarnContext(ctx, "resolved shared directory path is empty or root, skipping removal",
				"project_id", projectID, "name", name, "path", resolution.Path)
		default:
			if removeErr := os.RemoveAll(resolution.Path); removeErr != nil {
				slog.WarnContext(ctx, "failed to remove shared directory host path",
					"project_id", projectID, "name", name, "path", resolution.Path, "error", removeErr)
			}
		}

		s.events.PublishProjectUpdated(ctx, project)
		w.WriteHeader(http.StatusNoContent)

	default:
		MethodNotAllowed(w, http.MethodDelete)
	}
}
