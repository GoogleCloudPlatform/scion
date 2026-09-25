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
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/shareddirs"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// handleProjectSharedDirRoutes is the single authorized entry point for the
// project shared-dirs subtree: list/create, delete-by-name, archive download,
// and the file operations (list, download, upload, write, delete) reached
// under /api/v1/projects/{projectId}/shared-dirs[/{name}[/archive|/files...]].
//
// The project is loaded once here and passed down, and the gate below runs
// before sdPath is parsed into a leaf call, so every arm reached through this
// dispatcher is authorized by construction. This is a floor: a leaf below may
// still apply a stricter check of its own (e.g. restricting a write to a
// UserIdentity caller), and this function does not relax any of those.
func (s *Server) handleProjectSharedDirRoutes(w http.ResponseWriter, r *http.Request, projectID, sdPath string) {
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

	// Project isolation runs before the authorization check so a cross-project
	// agent caller keeps its 404 and is not told the project exists.
	if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
		if project.ID != agentIdent.ProjectID() {
			NotFound(w, "Project")
			return
		}
	}

	// SECURITY-GATE: CheckAccess — GET/HEAD only need project read access,
	// since listing, downloading and archiving shared-dir contents exposes
	// project data without changing it; every other method needs update
	// access, because it creates, overwrites or removes files on disk.
	action := ActionRead
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		action = ActionUpdate
	}
	if !s.authorize(w, r, projectResource(project), action) {
		return
	}

	if sdPath == "" {
		s.handleProjectSharedDirs(w, r, project)
		return
	}

	// Split into name and optional sub-path (e.g. "my-dir/files/some/path")
	parts := strings.SplitN(sdPath, "/", 2)
	name := parts[0]
	rest := ""
	if len(parts) > 1 {
		rest = parts[1]
	}
	switch {
	case rest == "archive":
		s.handleProjectSharedDirArchive(w, r, project, name)
	case strings.HasPrefix(rest, "files"):
		filePath := strings.TrimPrefix(rest, "files")
		filePath = strings.TrimPrefix(filePath, "/")
		s.handleSharedDirFiles(w, r, project, name, filePath)
	case rest == "":
		s.handleProjectSharedDirByName(w, r, project, name)
	default:
		NotFound(w, "Resource")
	}
}

// handleProjectSharedDirs handles GET/POST on /api/v1/projects/{projectId}/shared-dirs.
// The project has already been loaded and authorized by
// handleProjectSharedDirRoutes; the checks below are additional to that gate,
// not a replacement for it.
func (s *Server) handleProjectSharedDirs(w http.ResponseWriter, r *http.Request, project *store.Project) {
	ctx := r.Context()

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
// The project has already been loaded and authorized by
// handleProjectSharedDirRoutes; the check below is additional to that gate,
// not a replacement for it.
func (s *Server) handleProjectSharedDirByName(w http.ResponseWriter, r *http.Request, project *store.Project, name string) {
	ctx := r.Context()
	projectID := project.ID

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
		case resolution.Backend == "nfs":
			// NFS: remove the leaf via DeleteSharedDir, an fd-based walk
			// (subPathRoot/projectID/shared-dirs/<name>) that refuses a
			// symlinked structural component and touches only this one
			// named leaf.
			if removeErr := shareddirs.DeleteSharedDir(resolution.NFSHostBase, resolution.NFSSubPathRoot, project.ID, name); removeErr != nil {
				slog.WarnContext(ctx, "failed to remove NFS shared directory",
					"project_id", projectID, "name", name, "host_base", resolution.NFSHostBase, "error", removeErr)
			}
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
