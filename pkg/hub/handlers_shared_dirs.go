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
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// handleProjectSharedDirs handles GET/POST on /api/v1/groves/{groveId}/shared-dirs.
func (s *Server) handleProjectSharedDirs(w http.ResponseWriter, r *http.Request, groveID string) {
	ctx := r.Context()

	grove, err := s.store.GetProject(ctx, groveID)
	if err != nil {
		if err == store.ErrNotFound {
			NotFound(w, "Grove")
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
		// Read access check
		if userIdent, ok := identity.(UserIdentity); ok {
			decision := s.authzService.CheckAccess(ctx, userIdent, Resource{
				Type:    "grove",
				ID:      grove.ID,
				OwnerID: grove.OwnerID,
			}, ActionRead)
			if !decision.Allowed {
				Forbidden(w)
				return
			}
		}

		dirs := grove.SharedDirs
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
				Type:    "grove",
				ID:      grove.ID,
				OwnerID: grove.OwnerID,
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
		for _, d := range grove.SharedDirs {
			if d.Name == newDir.Name {
				BadRequest(w, "Shared directory "+newDir.Name+" already exists")
				return
			}
		}

		grove.SharedDirs = append(grove.SharedDirs, newDir)
		if err := s.store.UpdateProject(ctx, grove); err != nil {
			writeErrorFromErr(w, err, "")
			return
		}

		s.events.PublishProjectUpdated(ctx, grove)
		writeJSON(w, http.StatusCreated, newDir)

	default:
		MethodNotAllowed(w)
	}
}

// handleProjectSharedDirByName handles DELETE on /api/v1/groves/{groveId}/shared-dirs/{name}.
func (s *Server) handleProjectSharedDirByName(w http.ResponseWriter, r *http.Request, groveID, name string) {
	ctx := r.Context()

	grove, err := s.store.GetProject(ctx, groveID)
	if err != nil {
		if err == store.ErrNotFound {
			NotFound(w, "Grove")
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
			Type:    "grove",
			ID:      grove.ID,
			OwnerID: grove.OwnerID,
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
		updated := make([]api.SharedDir, 0, len(grove.SharedDirs))
		for _, d := range grove.SharedDirs {
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

		grove.SharedDirs = updated
		if err := s.store.UpdateProject(ctx, grove); err != nil {
			writeErrorFromErr(w, err, "")
			return
		}

		s.events.PublishProjectUpdated(ctx, grove)
		w.WriteHeader(http.StatusNoContent)

	default:
		MethodNotAllowed(w)
	}
}
