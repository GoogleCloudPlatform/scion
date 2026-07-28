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
	"strconv"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// hubPreStartHookBasePath is the top-level route prefix for hub-scoped hooks.
const hubPreStartHookBasePath = "/api/v1/pre-start-hooks/"

// Authorization model for hub-scoped pre-start hooks:
//
//   - Read (GET list, GET detail): any authenticated *user*. Project owners
//     who are not hub admins need this to render the "Inherited from hub"
//     indicator on the project settings page. Agent identities are rejected —
//     an agent has no reason to enumerate hub-wide hook policy. Non-admin
//     callers get the hook metadata with the script body stripped from list
//     responses (hub scripts can carry infrastructure secrets).
//   - Mutations (POST, PUT, DELETE, POST .../activate): hub admin only, and
//     never via a project-scoped User Access Token. A hub hook runs as root on
//     every agent whose project has no project-scoped hook, so changing it is a
//     hub-admin concern. See requireHubAdmin.
//
// Request/response shapes are shared with the project-scoped handlers
// (CreateProjectPreStartHookRequest, UpdateProjectPreStartHookRequest,
// ListProjectPreStartHooksResponse) — the payloads are identical.

// requireHubAdmin authorizes a hub-wide mutation. It is stricter than
// requireAdmin: in addition to demanding the admin role it rejects
// ScopedUserIdentity (a User Access Token). A UAT embeds the minting user's
// role, so an admin-minted project-scoped CI token would otherwise pass a plain
// role check and be able to install a hub-wide hook script that executes as
// root inside every agent container. UATs are confined to a single project and
// must never affect hub-wide policy.
func (s *Server) requireHubAdmin(w http.ResponseWriter, r *http.Request) (UserIdentity, bool) {
	identity := GetUserIdentityFromContext(r.Context())
	if identity == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required", nil)
		return nil, false
	}
	if identity.Role() != store.UserRoleAdmin {
		Forbidden(w)
		return nil, false
	}
	if _, scoped := identity.(*ScopedUserIdentity); scoped {
		Forbidden(w)
		return nil, false
	}
	return identity, true
}

// requireHubHookReader authorizes a hub hook read. Any authenticated user
// identity qualifies (including UAT-scoped ones — reading hub hook metadata is
// needed to render the inherited-hook banner); agent identities do not.
// The second return value reports whether the caller is a full hub admin,
// which controls whether script bodies are included in list responses.
func (s *Server) requireHubHookReader(w http.ResponseWriter, r *http.Request) (UserIdentity, bool) {
	identity := GetUserIdentityFromContext(r.Context())
	if identity == nil {
		Unauthorized(w)
		return nil, false
	}
	return identity, true
}

// isHubAdminIdentity reports whether the identity may see hub hook script
// bodies in list responses.
func isHubAdminIdentity(identity UserIdentity) bool {
	if identity == nil {
		return false
	}
	if _, scoped := identity.(*ScopedUserIdentity); scoped {
		return false
	}
	return identity.Role() == store.UserRoleAdmin
}

// handleHubPreStartHooks handles GET (list) and POST (create) on
// /api/v1/pre-start-hooks.
func (s *Server) handleHubPreStartHooks(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	switch r.Method {
	case http.MethodGet:
		identity, ok := s.requireHubHookReader(w, r)
		if !ok {
			return
		}

		query := r.URL.Query()

		var hooks []*store.ProjectPreStartHook
		if query.Get("status") == store.ProjectPreStartHookStatusActive {
			// The project settings page asks for ?status=active&limit=1 to
			// render the inherited-hook banner. Serve that from the dedicated
			// single-row lookup; "no active hub hook" is an empty list, not 404.
			hook, err := s.store.GetActiveHubPreStartHook(ctx)
			switch {
			case err == nil:
				hooks = []*store.ProjectPreStartHook{hook}
			case errors.Is(err, store.ErrNotFound):
				hooks = nil
			default:
				writeErrorFromErr(w, err, "")
				return
			}
		} else {
			var err error
			hooks, err = s.store.ListHubPreStartHooks(ctx)
			if err != nil {
				writeErrorFromErr(w, err, "")
				return
			}
		}

		if raw := query.Get("limit"); raw != "" {
			limit, err := strconv.Atoi(raw)
			if err != nil || limit < 0 {
				BadRequest(w, "limit must be a non-negative integer")
				return
			}
			if limit < len(hooks) {
				hooks = hooks[:limit]
			}
		}

		// Hub hook scripts may embed infrastructure secrets. Non-admin callers
		// only need {id, name, slug, status, scope} for the inherited banner.
		if !isHubAdminIdentity(identity) {
			redacted := make([]*store.ProjectPreStartHook, 0, len(hooks))
			for _, h := range hooks {
				if h == nil {
					continue
				}
				copied := *h
				copied.Script = ""
				redacted = append(redacted, &copied)
			}
			hooks = redacted
		}

		writeJSON(w, http.StatusOK, &ListProjectPreStartHooksResponse{Hooks: hooks})

	case http.MethodPost:
		identity, ok := s.requireHubAdmin(w, r)
		if !ok {
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

		// Always run through Slugify so a caller-supplied slug is normalised
		// (lowercased, special chars stripped) exactly as a name-derived slug is.
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

		createdBy := identity.Email()

		hook, err := s.store.CreateHubPreStartHook(ctx, &store.ProjectPreStartHook{
			Scope:       store.PreStartHookScopeHub,
			Name:        req.Name,
			Slug:        slug,
			Description: req.Description,
			Script:      req.Script,
			CreatedBy:   createdBy,
			UpdatedBy:   createdBy,
		})
		if err != nil {
			if errors.Is(err, store.ErrAlreadyExists) {
				Conflict(w, "a hub hook with this slug already exists")
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

// handleHubPreStartHookByID handles requests on
// /api/v1/pre-start-hooks/{hookId} and
// /api/v1/pre-start-hooks/{hookId}/activate.
func (s *Server) handleHubPreStartHookByID(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	path := strings.TrimPrefix(r.URL.Path, hubPreStartHookBasePath)
	path = strings.TrimSuffix(path, "/")
	if path == "" {
		NotFound(w, "HubPreStartHook")
		return
	}

	parts := strings.SplitN(path, "/", 2)
	hookID := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}
	if hookID == "" {
		NotFound(w, "HubPreStartHook")
		return
	}

	switch action {
	case "":
		// fall through to the method switch below
	case "activate":
		if r.Method != http.MethodPost {
			MethodNotAllowed(w)
			return
		}
		if _, ok := s.requireHubAdmin(w, r); !ok {
			return
		}

		hook, err := s.store.ActivateHubPreStartHook(ctx, hookID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				NotFound(w, "HubPreStartHook")
				return
			}
			writeErrorFromErr(w, err, "")
			return
		}
		writeJSON(w, http.StatusOK, hook)
		return
	default:
		NotFound(w, "HubPreStartHook")
		return
	}

	switch r.Method {
	case http.MethodGet:
		if _, ok := s.requireHubHookReader(w, r); !ok {
			return
		}

		hook, err := s.store.GetHubPreStartHook(ctx, hookID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				NotFound(w, "HubPreStartHook")
				return
			}
			writeErrorFromErr(w, err, "")
			return
		}
		writeJSON(w, http.StatusOK, hook)

	case http.MethodPut:
		identity, ok := s.requireHubAdmin(w, r)
		if !ok {
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

		existing, err := s.store.GetHubPreStartHook(ctx, hookID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				NotFound(w, "HubPreStartHook")
				return
			}
			writeErrorFromErr(w, err, "")
			return
		}

		// Apply partial updates.
		updated := &store.ProjectPreStartHook{
			ID:    existing.ID,
			Scope: store.PreStartHookScopeHub,
		}
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
		updated.UpdatedBy = identity.Email()

		if updated.Script == "" {
			ValidationError(w, "script cannot be set to empty", nil)
			return
		}

		hook, err := s.store.UpdateHubPreStartHook(ctx, updated)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				NotFound(w, "HubPreStartHook")
				return
			}
			writeErrorFromErr(w, err, "")
			return
		}
		writeJSON(w, http.StatusOK, hook)

	case http.MethodDelete:
		if _, ok := s.requireHubAdmin(w, r); !ok {
			return
		}

		if err := s.store.DeleteHubPreStartHook(ctx, hookID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				NotFound(w, "HubPreStartHook")
				return
			}
			if errors.Is(err, store.ErrInvalidInput) {
				BadRequest(w, "cannot delete an active hook while other hub hooks exist; activate another hook first, then delete this one")
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
