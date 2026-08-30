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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// =============================================================================
// Project-scope injected skills
// =============================================================================

// handleProjectInjectedSkills routes GET/POST/PUT on
// /api/v1/projects/{projectId}/injected-skills.
func (s *Server) handleProjectInjectedSkills(w http.ResponseWriter, r *http.Request, projectID string) {
	switch r.Method {
	case http.MethodGet:
		s.listProjectInjectedSkills(w, r, projectID)
	case http.MethodPost:
		s.addProjectInjectedSkill(w, r, projectID)
	case http.MethodPut:
		s.setProjectInjectedSkills(w, r, projectID)
	default:
		MethodNotAllowed(w)
	}
}

// handleProjectInjectedSkillByID routes DELETE on
// /api/v1/projects/{projectId}/injected-skills/{id}.
func (s *Server) handleProjectInjectedSkillByID(w http.ResponseWriter, r *http.Request, projectID, entryID string) {
	switch r.Method {
	case http.MethodDelete:
		s.removeProjectInjectedSkill(w, r, projectID, entryID)
	default:
		MethodNotAllowed(w)
	}
}

// listProjectInjectedSkills handles GET /api/v1/projects/{projectId}/injected-skills.
func (s *Server) listProjectInjectedSkills(w http.ResponseWriter, r *http.Request, projectID string) {
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

	// Read access check. Only UserIdentity callers (web users) are permitted;
	// agent/broker tokens are rejected by the else-guard below.
	if userIdent, ok := identity.(UserIdentity); ok {
		decision := s.authzService.CheckAccess(ctx, userIdent, projectResource(project), ActionRead)
		if !decision.Allowed {
			Forbidden(w)
			return
		}
	} else {
		Forbidden(w)
		return
	}

	sis, err := s.store.ListSkillInjections(ctx, store.SkillInjectionScopeProject, projectID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, api.SkillInjectionList{
		Entries: s.enrichSkillInjections(ctx, sis),
	})
}

// addProjectInjectedSkill handles POST /api/v1/projects/{projectId}/injected-skills.
func (s *Server) addProjectInjectedSkill(w http.ResponseWriter, r *http.Request, projectID string) {
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

	// Write access check (project owner/admin).
	if userIdent, ok := identity.(UserIdentity); ok {
		decision := s.authzService.CheckAccess(ctx, userIdent, projectResource(project), ActionUpdate)
		if !decision.Allowed {
			Forbidden(w)
			return
		}
	} else {
		Forbidden(w)
		return
	}

	var entry api.SkillInjectionEntry
	if err := readJSON(r, &entry); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}
	entry.SkillURI = strings.TrimSpace(entry.SkillURI)
	if entry.SkillURI == "" {
		ValidationError(w, "skillUri is required", nil)
		return
	}
	normalizedURI, err := api.NormalizeSkillURI(entry.SkillURI)
	if err != nil {
		ValidationError(w, err.Error(), nil)
		return
	}
	entry.SkillURI = normalizedURI

	var createdBy string
	if userIdent, ok := identity.(UserIdentity); ok {
		createdBy = userIdent.ID()
	}

	// Determine sort order: use the client-supplied value if non-zero, otherwise
	// append at the end (max existing sortOrder + 1).
	sortOrder := entry.SortOrder
	if sortOrder == 0 {
		existing, err := s.store.ListSkillInjections(ctx, store.SkillInjectionScopeProject, projectID)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		// Note: sort_order assignment is best-effort; concurrent POSTs may produce
		// duplicate sort_order values. Full ordering is maintained by the client via
		// the bulk PUT endpoint. See issue #548 for tracking.
		maxOrder := 0
		for _, e := range existing {
			if e.SortOrder > maxOrder {
				maxOrder = e.SortOrder
			}
		}
		sortOrder = maxOrder + 1
	}

	// allowProgeny is only valid on user-scoped skill injections
	if entry.AllowProgeny {
		ValidationError(w, "allowProgeny is only supported on user-scoped skill injections", map[string]interface{}{
			"field": "allowProgeny",
			"scope": store.SkillInjectionScopeProject,
		})
		return
	}

	si := &store.SkillInjection{
		Scope:     store.SkillInjectionScopeProject,
		ScopeID:   projectID,
		SkillURI:  entry.SkillURI,
		SkillAs:   entry.SkillAs,
		Optional:  entry.Optional,
		SortOrder: sortOrder,
		CreatedBy: createdBy,
	}

	if err := s.store.AddSkillInjection(ctx, si); err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			writeError(w, http.StatusConflict, ErrCodeConflict,
				"An entry for this skill URI already exists in this project", nil)
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusCreated, skillInjectionToEntry(*si))
}

// setProjectInjectedSkills handles PUT /api/v1/projects/{projectId}/injected-skills.
// Bulk-replaces the full list atomically.
func (s *Server) setProjectInjectedSkills(w http.ResponseWriter, r *http.Request, projectID string) {
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

	// Write access check (project owner/admin).
	if userIdent, ok := identity.(UserIdentity); ok {
		decision := s.authzService.CheckAccess(ctx, userIdent, projectResource(project), ActionUpdate)
		if !decision.Allowed {
			Forbidden(w)
			return
		}
	} else {
		Forbidden(w)
		return
	}

	var req api.SkillInjectionList
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	var createdBy string
	if userIdent, ok := identity.(UserIdentity); ok {
		createdBy = userIdent.ID()
	}

	// Validate and normalize all entries before committing any changes (N-2).
	seenURIs := make(map[string]bool)
	for i, entry := range req.Entries {
		uri := strings.TrimSpace(entry.SkillURI)
		if uri == "" {
			BadRequest(w, fmt.Sprintf("entry %d: skillUri is required", i))
			return
		}
		normalized, normErr := api.NormalizeSkillURI(uri)
		if normErr != nil {
			ValidationError(w, fmt.Sprintf("entry %d: %s", i, normErr.Error()), nil)
			return
		}
		req.Entries[i].SkillURI = normalized
		if seenURIs[normalized] {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
				fmt.Sprintf("duplicate skillUri in request: %s", normalized), nil)
			return
		}
		seenURIs[normalized] = true

		// allowProgeny is only valid on user-scoped skill injections
		if entry.AllowProgeny {
			ValidationError(w, "allowProgeny is only supported on user-scoped skill injections", map[string]interface{}{
				"field": "allowProgeny",
				"scope": store.SkillInjectionScopeProject,
				"entry": i,
			})
			return
		}
	}

	// Build set of explicitly-assigned sort orders (C4: collision-free defaults).
	explicit := make(map[int]bool)
	for _, e := range req.Entries {
		if e.SortOrder != 0 {
			explicit[e.SortOrder] = true
		}
	}
	nextDefault := 1
	injections := make([]store.SkillInjection, 0, len(req.Entries))
	for _, e := range req.Entries {
		so := e.SortOrder
		if so == 0 {
			for explicit[nextDefault] {
				nextDefault++
			}
			so = nextDefault
			explicit[nextDefault] = true
			nextDefault++
		}
		injections = append(injections, store.SkillInjection{
			Scope:     store.SkillInjectionScopeProject,
			ScopeID:   projectID,
			SkillURI:  e.SkillURI,
			SkillAs:   e.SkillAs,
			Optional:  e.Optional,
			SortOrder: so,
			CreatedBy: createdBy,
		})
	}
	if err := s.store.SetSkillInjections(ctx, store.SkillInjectionScopeProject, projectID, injections, createdBy); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Return the new list.
	sis, err := s.store.ListSkillInjections(ctx, store.SkillInjectionScopeProject, projectID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	writeJSON(w, http.StatusOK, api.SkillInjectionList{
		Entries: s.enrichSkillInjections(ctx, sis),
	})
}

// removeProjectInjectedSkill handles DELETE /api/v1/projects/{projectId}/injected-skills/{id}.
func (s *Server) removeProjectInjectedSkill(w http.ResponseWriter, r *http.Request, projectID, entryID string) {
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

	// Write access check.
	if userIdent, ok := identity.(UserIdentity); ok {
		decision := s.authzService.CheckAccess(ctx, userIdent, projectResource(project), ActionUpdate)
		if !decision.Allowed {
			Forbidden(w)
			return
		}
	} else {
		Forbidden(w)
		return
	}

	// Fetch-then-verify: confirm the entry belongs to this project before
	// deleting. This prevents IDOR where a project-A admin could delete a
	// project-B entry by guessing its UUID (C-2).
	projectEntries, err := s.store.ListSkillInjections(ctx, store.SkillInjectionScopeProject, projectID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	owned := false
	for _, e := range projectEntries {
		if e.ID == entryID {
			owned = true
			break
		}
	}
	if !owned {
		NotFound(w, "Skill injection entry")
		return
	}

	if err := s.store.RemoveSkillInjection(ctx, entryID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Skill injection entry")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// =============================================================================
// User-scope injected skills (/users/me/...)
// =============================================================================

// handleUserMeInjectedSkills routes GET/POST/PUT on
// /api/v1/users/me/injected-skills.
func (s *Server) handleUserMeInjectedSkills(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listUserInjectedSkills(w, r)
	case http.MethodPost:
		s.addUserInjectedSkill(w, r)
	case http.MethodPut:
		s.setUserInjectedSkills(w, r)
	default:
		MethodNotAllowed(w)
	}
}

// handleUserMeInjectedSkillByID routes DELETE on
// /api/v1/users/me/injected-skills/{id}.
func (s *Server) handleUserMeInjectedSkillByID(w http.ResponseWriter, r *http.Request, entryID string) {
	switch r.Method {
	case http.MethodDelete:
		s.removeUserInjectedSkill(w, r, entryID)
	default:
		MethodNotAllowed(w)
	}
}

// listUserInjectedSkills handles GET /api/v1/users/me/injected-skills.
func (s *Server) listUserInjectedSkills(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userIdent := GetUserIdentityFromContext(ctx)
	if userIdent == nil {
		Unauthorized(w)
		return
	}

	sis, err := s.store.ListSkillInjections(ctx, store.SkillInjectionScopeUser, userIdent.ID())
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, api.SkillInjectionList{
		Entries: s.enrichSkillInjections(ctx, sis),
	})
}

// addUserInjectedSkill handles POST /api/v1/users/me/injected-skills.
func (s *Server) addUserInjectedSkill(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userIdent := GetUserIdentityFromContext(ctx)
	if userIdent == nil {
		Unauthorized(w)
		return
	}

	var entry api.SkillInjectionEntry
	if err := readJSON(r, &entry); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}
	entry.SkillURI = strings.TrimSpace(entry.SkillURI)
	if entry.SkillURI == "" {
		ValidationError(w, "skillUri is required", nil)
		return
	}
	normalizedURI, err := api.NormalizeSkillURI(entry.SkillURI)
	if err != nil {
		ValidationError(w, err.Error(), nil)
		return
	}
	entry.SkillURI = normalizedURI

	// Determine sort order: use the client-supplied value if non-zero, otherwise
	// append at the end (max existing sortOrder + 1).
	sortOrder := entry.SortOrder
	if sortOrder == 0 {
		existing, err := s.store.ListSkillInjections(ctx, store.SkillInjectionScopeUser, userIdent.ID())
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		// Note: sort_order assignment is best-effort; concurrent POSTs may produce
		// duplicate sort_order values. Full ordering is maintained by the client via
		// the bulk PUT endpoint. See issue #548 for tracking.
		maxOrder := 0
		for _, e := range existing {
			if e.SortOrder > maxOrder {
				maxOrder = e.SortOrder
			}
		}
		sortOrder = maxOrder + 1
	}

	si := &store.SkillInjection{
		Scope:        store.SkillInjectionScopeUser,
		ScopeID:      userIdent.ID(),
		SkillURI:     entry.SkillURI,
		SkillAs:      entry.SkillAs,
		Optional:     entry.Optional,
		AllowProgeny: entry.AllowProgeny,
		SortOrder:    sortOrder,
		CreatedBy:    userIdent.ID(),
	}

	if err := s.store.AddSkillInjection(ctx, si); err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			writeError(w, http.StatusConflict, ErrCodeConflict,
				"An entry for this skill URI already exists in your list", nil)
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// Manage implicit progeny policy lifecycle
	s.ensureSkillProgenyPolicy(ctx, si)

	writeJSON(w, http.StatusCreated, skillInjectionToEntry(*si))
}

// setUserInjectedSkills handles PUT /api/v1/users/me/injected-skills.
func (s *Server) setUserInjectedSkills(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userIdent := GetUserIdentityFromContext(ctx)
	if userIdent == nil {
		Unauthorized(w)
		return
	}

	var req api.SkillInjectionList
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	// Validate and normalize all entries before committing any changes (N-2).
	seenURIs := make(map[string]bool)
	for i, entry := range req.Entries {
		uri := strings.TrimSpace(entry.SkillURI)
		if uri == "" {
			BadRequest(w, fmt.Sprintf("entry %d: skillUri is required", i))
			return
		}
		normalized, normErr := api.NormalizeSkillURI(uri)
		if normErr != nil {
			ValidationError(w, fmt.Sprintf("entry %d: %s", i, normErr.Error()), nil)
			return
		}
		req.Entries[i].SkillURI = normalized
		if seenURIs[normalized] {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
				fmt.Sprintf("duplicate skillUri in request: %s", normalized), nil)
			return
		}
		seenURIs[normalized] = true
	}

	// Build set of explicitly-assigned sort orders (C4: collision-free defaults).
	explicit := make(map[int]bool)
	for _, e := range req.Entries {
		if e.SortOrder != 0 {
			explicit[e.SortOrder] = true
		}
	}
	nextDefault := 1
	injections := make([]store.SkillInjection, 0, len(req.Entries))
	for _, e := range req.Entries {
		so := e.SortOrder
		if so == 0 {
			for explicit[nextDefault] {
				nextDefault++
			}
			so = nextDefault
			explicit[nextDefault] = true
			nextDefault++
		}
		injections = append(injections, store.SkillInjection{
			Scope:        store.SkillInjectionScopeUser,
			ScopeID:      userIdent.ID(),
			SkillURI:     e.SkillURI,
			SkillAs:      e.SkillAs,
			Optional:     e.Optional,
			AllowProgeny: e.AllowProgeny,
			SortOrder:    so,
			CreatedBy:    userIdent.ID(),
		})
	}
	// Collect old progeny IDs before the replace so we can clean up
	// their policies after the replace succeeds. Deleting before the
	// replace risks inconsistent state if the replace fails.
	var oldProgenyIDs []string
	if existing, err := s.store.ListSkillInjections(ctx, store.SkillInjectionScopeUser, userIdent.ID()); err == nil {
		for _, e := range existing {
			if e.AllowProgeny {
				oldProgenyIDs = append(oldProgenyIDs, e.ID)
			}
		}
	}

	if err := s.store.SetSkillInjections(ctx, store.SkillInjectionScopeUser, userIdent.ID(), injections, userIdent.ID()); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Clean up old progeny policies only after the replace succeeded.
	for _, id := range oldProgenyIDs {
		s.deleteSkillProgenyPolicy(ctx, id)
	}

	sis, err := s.store.ListSkillInjections(ctx, store.SkillInjectionScopeUser, userIdent.ID())
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Reconcile progeny policies for all user entries after bulk replace
	for i := range sis {
		s.ensureSkillProgenyPolicy(ctx, &sis[i])
	}

	writeJSON(w, http.StatusOK, api.SkillInjectionList{
		Entries: s.enrichSkillInjections(ctx, sis),
	})
}

// removeUserInjectedSkill handles DELETE /api/v1/users/me/injected-skills/{id}.
func (s *Server) removeUserInjectedSkill(w http.ResponseWriter, r *http.Request, entryID string) {
	ctx := r.Context()

	userIdent := GetUserIdentityFromContext(ctx)
	if userIdent == nil {
		Unauthorized(w)
		return
	}

	// Fetch-then-verify: confirm the entry belongs to this user before
	// deleting. This prevents IDOR where user A could delete user B's entry
	// by guessing its UUID (C-1).
	userEntries, err := s.store.ListSkillInjections(ctx, store.SkillInjectionScopeUser, userIdent.ID())
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	var ownedEntry *store.SkillInjection
	for _, e := range userEntries {
		if e.ID == entryID {
			ownedEntry = &e
			break
		}
	}
	if ownedEntry == nil {
		NotFound(w, "Skill injection entry")
		return
	}

	// Clean up progeny policy if the entry had AllowProgeny enabled
	if ownedEntry.AllowProgeny {
		s.deleteSkillProgenyPolicy(ctx, ownedEntry.ID)
	}

	if err := s.store.RemoveSkillInjection(ctx, entryID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Skill injection entry")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// =============================================================================
// Hub-scope injected skills
// =============================================================================

// handleHubInjectedSkills routes GET/PUT on
// /api/v1/hub/settings/injected-skills.
func (s *Server) handleHubInjectedSkills(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.getHubInjectedSkills(w, r)
	case http.MethodPut:
		s.setHubInjectedSkills(w, r)
	default:
		MethodNotAllowed(w)
	}
}

// getHubInjectedSkills handles GET /api/v1/hub/settings/injected-skills.
// Any authenticated user can read the hub-scope list (needed for merge at provision time).
func (s *Server) getHubInjectedSkills(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return
	}

	setting, err := s.store.GetHubSetting(ctx, "injected_skills")
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Not configured yet — return empty arrays.
			writeJSON(w, http.StatusOK, api.HubSkillInjectionSetting{
				System:      []api.SkillReference{},
				UserDefined: []api.SkillReference{},
			})
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	var hubSetting api.HubSkillInjectionSetting
	if err := json.Unmarshal(setting.Value, &hubSetting); err != nil {
		slog.Error("failed to parse hub injected_skills setting", "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"Failed to parse hub injected skills setting", nil)
		return
	}

	// Ensure non-nil arrays in response.
	if hubSetting.System == nil {
		hubSetting.System = []api.SkillReference{}
	}
	if hubSetting.UserDefined == nil {
		hubSetting.UserDefined = []api.SkillReference{}
	}

	writeJSON(w, http.StatusOK, hubSetting)
}

// setHubInjectedSkills handles PUT /api/v1/hub/settings/injected-skills.
// Hub admin only. Updates only the user_defined portion; system entries are immutable.
func (s *Server) setHubInjectedSkills(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Require hub.settings.update permission (user identity only).
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return
	}
	user, ok := identity.(UserIdentity)
	if !ok {
		Forbidden(w)
		return
	}
	if !s.authzService.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(user),
		Credential: credentialContextForIdentity(user),
		Resource:   Resource{Type: "hub", ID: "hub"},
		Action:     Action("update"),
		Permission: "hub.settings.update",
	}).Allowed {
		Forbidden(w)
		return
	}

	var req struct {
		UserDefined []api.SkillReference `json:"user_defined"`
	}
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}
	if req.UserDefined == nil {
		req.UserDefined = []api.SkillReference{}
	}

	// Read current setting to preserve the system entries.
	// If the stored blob exists but is corrupt, return 500 rather than
	// silently destroying system skill entries (H-1).
	var existing api.HubSkillInjectionSetting
	if setting, err := s.store.GetHubSetting(ctx, "injected_skills"); err == nil {
		if jsonErr := json.Unmarshal(setting.Value, &existing); jsonErr != nil {
			slog.Error("failed to parse existing hub injected_skills setting", "error", jsonErr)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
				"internal error: failed to parse current hub skill settings", nil)
			return
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		writeErrorFromErr(w, err, "")
		return
	}
	// System entries are never overwritten via this endpoint.
	existing.UserDefined = req.UserDefined

	raw, err := json.Marshal(existing)
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"Failed to serialize hub injected skills setting", nil)
		return
	}

	result, err := s.store.UpsertHubSetting(ctx, "injected_skills", raw, user.ID(), -1, "managed")
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	var updated api.HubSkillInjectionSetting
	if err := json.Unmarshal(result.Value, &updated); err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"Failed to parse updated hub injected skills setting", nil)
		return
	}
	if updated.System == nil {
		updated.System = []api.SkillReference{}
	}
	if updated.UserDefined == nil {
		updated.UserDefined = []api.SkillReference{}
	}
	writeJSON(w, http.StatusOK, updated)
}

// =============================================================================
// Progeny policy helpers for skill injections
// =============================================================================
//
// RG1 migration note: The RelationshipGrantResolver (authz_relationship.go)
// provides the target replacement for these DelegatedFrom Policy rows. At CO1
// cutover, the resolver is wired into the evaluator and these functions become
// no-ops. Until then, Policy rows are still created here so that the existing
// checkDelegation path (authz.go) continues to grant progeny access for newly
// created resources.

// skillProgenyPolicyName returns the canonical policy name for a progeny skill injection policy.
func skillProgenyPolicyName(skillInjectionID string) string {
	return "progeny-skill-access:" + skillInjectionID
}

// ensureSkillProgenyPolicy is a no-op after CO1 cutover. Progeny access is
// now handled by the RelationshipGrantResolver (authz_relationship.go).
func (s *Server) ensureSkillProgenyPolicy(_ context.Context, _ *store.SkillInjection) {}

// deleteSkillProgenyPolicy is a no-op after CO1 cutover.
func (s *Server) deleteSkillProgenyPolicy(_ context.Context, _ string) {}

// =============================================================================
// Shared helpers
// =============================================================================

// skillInjectionToEntry converts a store.SkillInjection to an api.SkillInjectionEntry.
func skillInjectionToEntry(si store.SkillInjection) api.SkillInjectionEntry {
	return api.SkillInjectionEntry{
		ID:           si.ID,
		SkillURI:     si.SkillURI,
		SkillAs:      si.SkillAs,
		Optional:     si.Optional,
		AllowProgeny: si.AllowProgeny,
		SortOrder:    si.SortOrder,
	}
}

// enrichSkillInjections converts store.SkillInjection records to api.SkillInjectionEntry
// records and enriches them with skill bank metadata where available.
// Enrichment is best-effort: any lookup error leaves SkillName/SkillSlug empty.
//
// To avoid N+1 queries, we batch-load all core and global skills in two list
// calls and build an in-memory slug→skill map before iterating the entries.
// Global entries take precedence over core entries when slugs collide.
func (s *Server) enrichSkillInjections(ctx context.Context, sis []store.SkillInjection) []api.SkillInjectionEntry {
	if len(sis) == 0 {
		return []api.SkillInjectionEntry{}
	}

	// Build slug→skill map via two batch queries (core first, then global so
	// that global entries win on slug collision — matching the previous per-entry
	// lookup priority).
	skillBySlug := make(map[string]*store.Skill)
	for _, scope := range []string{store.SkillScopeCore, store.SkillScopeGlobal} {
		result, err := s.store.ListSkills(ctx, store.SkillFilter{Scope: scope}, store.ListOptions{Limit: 1000})
		if err != nil || result == nil {
			continue
		}
		if result.TotalCount > len(result.Items) {
			slog.Warn("skill enrichment truncated: more skills exist than fetched",
				"scope", scope,
				"fetched", len(result.Items),
				"total", result.TotalCount)
		}
		for i := range result.Items {
			sk := &result.Items[i]
			skillBySlug[sk.Slug] = sk
		}
	}

	entries := make([]api.SkillInjectionEntry, 0, len(sis))
	for _, si := range sis {
		e := skillInjectionToEntry(si)
		baseURI := skillBaseURI(si.SkillURI)
		slug := skillSlugFromURI(baseURI)
		if slug != "" {
			if sk, ok := skillBySlug[slug]; ok {
				e.SkillName = sk.Name
				e.SkillSlug = sk.Slug
			}
		}
		entries = append(entries, e)
	}
	return entries
}

// skillBaseURI strips the version specifier from a skill URI.
// "scion://my-skill@1.0" → "scion://my-skill"; "scion://my-skill" → "scion://my-skill".
func skillBaseURI(uri string) string {
	if i := strings.LastIndex(uri, "@"); i > strings.Index(uri, "://") {
		return uri[:i]
	}
	return uri
}

// skillSlugFromURI extracts a slug from the last path segment of a skill URI.
// "scion://my-skill" → "my-skill"; "https://example.com/skills/my-skill" → "my-skill".
func skillSlugFromURI(uri string) string {
	// Strip scheme.
	if idx := strings.Index(uri, "://"); idx >= 0 {
		uri = uri[idx+3:]
	}
	// Take last path segment.
	if idx := strings.LastIndex(uri, "/"); idx >= 0 {
		uri = uri[idx+1:]
	}
	return uri
}
