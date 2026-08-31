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
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ---------------------------------------------------------------------------
// Request / Response types for project-scoped members API (PM1)
// ---------------------------------------------------------------------------

// projectMemberInfo is a role binding enriched with human-friendly fields
// for the project members UI.
type projectMemberInfo struct {
	store.RoleBinding
	RoleName             string `json:"roleName"`
	Source               string `json:"source"` // "direct" for direct bindings
	PrincipalDisplayName string `json:"principalDisplayName,omitempty"`
	CreatedByDisplayName string `json:"createdByDisplayName,omitempty"`
}

// listProjectMembersResponse wraps the paginated result for project members.
type listProjectMembersResponse struct {
	Items      []projectMemberInfo `json:"items"`
	TotalCount int                 `json:"totalCount"`
}

// addProjectMemberRequest is the payload for POST /api/v1/projects/{id}/members.
type addProjectMemberRequest struct {
	RoleDefinitionID string     `json:"roleDefinitionId"`
	PrincipalType    string     `json:"principalType"`
	PrincipalID      string     `json:"principalId"`
	NotBefore        *time.Time `json:"notBefore,omitempty"`
	ExpiresAt        *time.Time `json:"expiresAt,omitempty"`
}

// updateProjectMemberRequest is the payload for PATCH /api/v1/projects/{id}/members/{bindingID}.
type updateProjectMemberRequest struct {
	RoleDefinitionID string `json:"roleDefinitionId"`
}

// ---------------------------------------------------------------------------
// Valid project-scoped role names
// ---------------------------------------------------------------------------

var validProjectRoles = map[string]bool{
	store.ProjectRoleOwner:  true,
	store.ProjectRoleAdmin:  true,
	store.ProjectRoleMember: true,
}

// ---------------------------------------------------------------------------
// Route handler: /api/v1/projects/{id}/members[/{bindingID}]
// ---------------------------------------------------------------------------

// handleProjectMembers dispatches GET and POST for the collection endpoint.
func (s *Server) handleProjectMembers(w http.ResponseWriter, r *http.Request, projectID string) {
	switch r.Method {
	case http.MethodGet:
		s.listProjectMembers(w, r, projectID)
	case http.MethodPost:
		s.addProjectMember(w, r, projectID)
	default:
		MethodNotAllowed(w)
	}
}

// handleProjectMemberByID dispatches PATCH and DELETE for individual bindings.
func (s *Server) handleProjectMemberByID(w http.ResponseWriter, r *http.Request, projectID, bindingID string) {
	switch r.Method {
	case http.MethodPatch:
		s.updateProjectMemberRole(w, r, projectID, bindingID)
	case http.MethodDelete:
		s.removeProjectMember(w, r, projectID, bindingID)
	default:
		MethodNotAllowed(w)
	}
}

// ---------------------------------------------------------------------------
// GET /api/v1/projects/{id}/members — list project members
// ---------------------------------------------------------------------------

func (s *Server) listProjectMembers(w http.ResponseWriter, r *http.Request, projectID string) {
	ctx := r.Context()

	// Authorize: project.read at project scope.
	if !s.authorize(w, r, Resource{Type: "project", ID: projectID}, ActionRead) {
		return
	}

	// Verify the project exists.
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Project")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	limit, offset := parsePaginationParams(r)

	bindings, err := s.store.ListRoleBindingsForScope(ctx, store.RoleScopeProject, projectID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if bindings == nil {
		bindings = []*store.RoleBinding{}
	}

	totalCount := len(bindings)

	// Apply pagination.
	if limit <= 0 {
		limit = 100 // default
	}
	if offset > len(bindings) {
		offset = len(bindings)
	}
	end := offset + limit
	if end > len(bindings) {
		end = len(bindings)
	}
	page := bindings[offset:end]

	// Enrich with role name and display names. Cache role definitions
	// per request to avoid redundant lookups (rdCache pattern from
	// authz_candelegate.go:241-255).
	rdCache := make(map[string]string) // roleDefinitionID → roleName
	items := make([]projectMemberInfo, 0, len(page))
	for _, b := range page {
		if b == nil {
			continue
		}

		roleName, ok := rdCache[b.RoleDefinitionID]
		if !ok {
			rd, rdErr := s.store.GetRoleDefinition(ctx, b.RoleDefinitionID)
			if rdErr == nil && rd != nil {
				roleName = rd.Name
			}
			rdCache[b.RoleDefinitionID] = roleName
		}

		info := projectMemberInfo{
			RoleBinding: *b,
			RoleName:    roleName,
			Source:       "direct", // TODO: group-derived expansion deferred (needs architect ruling)
		}
		info.PrincipalDisplayName = s.resolveGroupMemberDisplayName(ctx, b.PrincipalType, b.PrincipalID)
		info.CreatedByDisplayName = s.resolveGroupMemberDisplayName(ctx, store.GroupMemberTypeUser, b.CreatedBy)

		items = append(items, info)
	}

	writeJSON(w, http.StatusOK, listProjectMembersResponse{
		Items:      items,
		TotalCount: totalCount,
	})
}

// ---------------------------------------------------------------------------
// POST /api/v1/projects/{id}/members — add a member
// ---------------------------------------------------------------------------

func (s *Server) addProjectMember(w http.ResponseWriter, r *http.Request, projectID string) {
	ctx := r.Context()

	// Authorize: project.manage at project scope.
	if !s.authorize(w, r, Resource{Type: "project", ID: projectID}, ActionManage) {
		return
	}

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

	var req addProjectMemberRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "invalid request body: "+err.Error())
		return
	}

	if req.RoleDefinitionID == "" {
		BadRequest(w, "roleDefinitionId is required")
		return
	}
	if req.PrincipalType == "" {
		BadRequest(w, "principalType is required")
		return
	}
	if req.PrincipalType != store.RoleBindingPrincipalUser &&
		req.PrincipalType != store.RoleBindingPrincipalAgent &&
		req.PrincipalType != store.RoleBindingPrincipalGroup {
		BadRequest(w, "principalType must be \"user\", \"agent\", or \"group\"")
		return
	}
	if req.PrincipalID == "" {
		BadRequest(w, "principalId is required")
		return
	}

	// Resolve email to UUID for user principals.
	if req.PrincipalType == store.RoleBindingPrincipalUser && strings.Contains(req.PrincipalID, "@") {
		resolvedUser, err := s.store.GetUserByEmail(ctx, req.PrincipalID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				BadRequest(w, "user not found with email: "+req.PrincipalID)
				return
			}
			writeErrorFromErr(w, err, "")
			return
		}
		if resolvedUser == nil {
			BadRequest(w, "user not found with email: "+req.PrincipalID)
			return
		}
		req.PrincipalID = resolvedUser.ID
	}

	// Verify group exists for group principals (slug fallback).
	if req.PrincipalType == store.RoleBindingPrincipalGroup {
		g, err := s.store.GetGroup(ctx, req.PrincipalID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				g, err = s.store.GetGroupBySlug(ctx, req.PrincipalID)
				if err != nil {
					if errors.Is(err, store.ErrNotFound) {
						BadRequest(w, "group not found: "+req.PrincipalID)
					} else {
						writeErrorFromErr(w, err, "")
					}
					return
				}
			} else {
				writeErrorFromErr(w, err, "")
				return
			}
		}
		req.PrincipalID = g.ID
	}

	// Validate that the target role is project-scoped.
	roleDef, err := s.store.GetRoleDefinition(ctx, req.RoleDefinitionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			BadRequest(w, "role definition not found")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}
	if roleDef.ScopeType != store.RoleScopeProject {
		BadRequest(w, "only project-scoped roles can be assigned to project members")
		return
	}
	if !validProjectRoles[roleDef.Name] {
		BadRequest(w, "invalid project role: "+roleDef.Name)
		return
	}

	// Validate lifecycle fields.
	if req.ExpiresAt != nil && !req.ExpiresAt.After(time.Now()) {
		BadRequest(w, "expiresAt must be in the future")
		return
	}
	if req.NotBefore != nil && req.ExpiresAt != nil && !req.ExpiresAt.After(*req.NotBefore) {
		BadRequest(w, "expiresAt must be after notBefore")
		return
	}

	// CanDelegate: project membership check — actor must be project owner/admin.
	if s.authzService != nil {
		decision := s.authzService.CanDelegate(ctx, user, GrantDescriptor{
			Type:      GrantTypeProjectMembership,
			ProjectID: projectID,
		})
		if !decision.Allowed {
			writeForbidden(w, "cannot add project member: "+decision.Reason)
			return
		}

		// CanDelegate: role binding escalation check — actor must hold all
		// permissions in the target role to prevent a project-admin from
		// minting a project-owner.
		decision = s.authzService.CanDelegate(ctx, user, GrantDescriptor{
			Type:             GrantTypeRoleBinding,
			RoleDefinitionID: req.RoleDefinitionID,
			ScopeType:        store.RoleScopeProject,
			ScopeID:          projectID,
		})
		if !decision.Allowed {
			writeForbidden(w, "cannot assign role: "+decision.Reason)
			return
		}
	}

	rb := &store.RoleBinding{
		RoleDefinitionID: req.RoleDefinitionID,
		PrincipalType:    req.PrincipalType,
		PrincipalID:      req.PrincipalID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		NotBefore:        req.NotBefore,
		ExpiresAt:        req.ExpiresAt,
		CreatedBy:        user.ID(),
	}

	created, err := s.store.CreateRoleBinding(ctx, rb)
	if err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			Conflict(w, "this member already has this role in this project")
			return
		}
		if errors.Is(err, store.ErrNotFound) {
			BadRequest(w, "role definition not found")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	slog.Info("project member added",
		"project_id", projectID, "binding_id", created.ID,
		"role", roleDef.Name, "principal", req.PrincipalType+":"+req.PrincipalID,
		"actor", user.Email())

	s.emitMutationAudit(ctx, &store.MutationAuditRecord{
		MutationType: "project_member_add",
		TargetType:   "project_membership",
		TargetID:     projectID,
		AfterSummary: `{"principalId":"` + req.PrincipalID + `","role":"` + roleDef.Name + `"}`,
	})

	// Return enriched response.
	info := projectMemberInfo{
		RoleBinding: *created,
		RoleName:    roleDef.Name,
		Source:       "direct",
	}
	info.PrincipalDisplayName = s.resolveGroupMemberDisplayName(ctx, created.PrincipalType, created.PrincipalID)
	info.CreatedByDisplayName = s.resolveGroupMemberDisplayName(ctx, store.GroupMemberTypeUser, created.CreatedBy)

	writeJSON(w, http.StatusCreated, info)
}

// ---------------------------------------------------------------------------
// PATCH /api/v1/projects/{id}/members/{bindingID} — change member role
// ---------------------------------------------------------------------------

func (s *Server) updateProjectMemberRole(w http.ResponseWriter, r *http.Request, projectID, bindingID string) {
	ctx := r.Context()

	// Authorize: project.manage at project scope.
	if !s.authorize(w, r, Resource{Type: "project", ID: projectID}, ActionManage) {
		return
	}

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

	var req updateProjectMemberRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "invalid request body: "+err.Error())
		return
	}

	if req.RoleDefinitionID == "" {
		BadRequest(w, "roleDefinitionId is required")
		return
	}

	// Fetch the existing binding.
	existing, err := s.store.GetRoleBinding(ctx, bindingID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Role Binding")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// Verify binding belongs to this project.
	if existing.ScopeType != store.RoleScopeProject || existing.ScopeID != projectID {
		NotFound(w, "Role Binding")
		return
	}

	// Validate the new role is project-scoped.
	newRoleDef, err := s.store.GetRoleDefinition(ctx, req.RoleDefinitionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			BadRequest(w, "role definition not found")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}
	if newRoleDef.ScopeType != store.RoleScopeProject {
		BadRequest(w, "only project-scoped roles can be assigned to project members")
		return
	}
	if !validProjectRoles[newRoleDef.Name] {
		BadRequest(w, "invalid project role: "+newRoleDef.Name)
		return
	}

	// Last-owner guard: if changing away from project-owner, ensure at least
	// one owner remains.
	oldRoleDef, err := s.store.GetRoleDefinition(ctx, existing.RoleDefinitionID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if oldRoleDef.Name == store.ProjectRoleOwner && newRoleDef.Name != store.ProjectRoleOwner {
		if existing.PrincipalType == store.RoleBindingPrincipalUser {
			ownerCount, countErr := s.countDirectOwnerBindings(ctx, projectID)
			if countErr != nil {
				writeErrorFromErr(w, countErr, "")
				return
			}
			if ownerCount <= 1 {
				writeError(w, http.StatusConflict, "LAST_OWNER",
					"Cannot change role of the last project owner — every project must retain at least one direct user owner", nil)
				return
			}
		}
	}

	// CanDelegate checks.
	if s.authzService != nil {
		decision := s.authzService.CanDelegate(ctx, user, GrantDescriptor{
			Type:      GrantTypeProjectMembership,
			ProjectID: projectID,
		})
		if !decision.Allowed {
			writeForbidden(w, "cannot modify project member: "+decision.Reason)
			return
		}

		decision = s.authzService.CanDelegate(ctx, user, GrantDescriptor{
			Type:             GrantTypeRoleBinding,
			RoleDefinitionID: req.RoleDefinitionID,
			ScopeType:        store.RoleScopeProject,
			ScopeID:          projectID,
		})
		if !decision.Allowed {
			writeForbidden(w, "cannot assign role: "+decision.Reason)
			return
		}
	}

	// Atomic role change: delete old binding, create new one.
	// This is a single logical operation — not the non-atomic
	// create-then-delete dance the old frontend used.
	if err := s.store.DeleteRoleBinding(ctx, bindingID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Role Binding")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	newBinding := &store.RoleBinding{
		RoleDefinitionID: req.RoleDefinitionID,
		PrincipalType:    existing.PrincipalType,
		PrincipalID:      existing.PrincipalID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		NotBefore:        existing.NotBefore,
		ExpiresAt:        existing.ExpiresAt,
		CreatedBy:        user.ID(),
	}

	created, err := s.store.CreateRoleBinding(ctx, newBinding)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	slog.Info("project member role changed",
		"project_id", projectID, "binding_id", created.ID,
		"old_role", oldRoleDef.Name, "new_role", newRoleDef.Name,
		"principal", existing.PrincipalType+":"+existing.PrincipalID,
		"actor", user.Email())

	s.emitMutationAudit(ctx, &store.MutationAuditRecord{
		MutationType: "project_member_role_change",
		TargetType:   "project_membership",
		TargetID:     projectID,
		BeforeSummary: `{"principalId":"` + existing.PrincipalID + `","role":"` + oldRoleDef.Name + `"}`,
		AfterSummary:  `{"principalId":"` + existing.PrincipalID + `","role":"` + newRoleDef.Name + `"}`,
	})

	info := projectMemberInfo{
		RoleBinding: *created,
		RoleName:    newRoleDef.Name,
		Source:       "direct",
	}
	info.PrincipalDisplayName = s.resolveGroupMemberDisplayName(ctx, created.PrincipalType, created.PrincipalID)
	info.CreatedByDisplayName = s.resolveGroupMemberDisplayName(ctx, store.GroupMemberTypeUser, created.CreatedBy)

	writeJSON(w, http.StatusOK, info)
}

// ---------------------------------------------------------------------------
// DELETE /api/v1/projects/{id}/members/{bindingID} — remove a member
// ---------------------------------------------------------------------------

func (s *Server) removeProjectMember(w http.ResponseWriter, r *http.Request, projectID, bindingID string) {
	ctx := r.Context()

	// Authorize: project.manage at project scope.
	if !s.authorize(w, r, Resource{Type: "project", ID: projectID}, ActionManage) {
		return
	}

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

	// Fetch the binding.
	binding, err := s.store.GetRoleBinding(ctx, bindingID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Role Binding")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// Verify binding belongs to this project.
	if binding.ScopeType != store.RoleScopeProject || binding.ScopeID != projectID {
		NotFound(w, "Role Binding")
		return
	}

	// Last-owner guard: cannot delete the last direct-user project-owner binding.
	if binding.PrincipalType == store.RoleBindingPrincipalUser {
		roleDef, rdErr := s.store.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
		if rdErr != nil {
			slog.Error("last-owner check: failed to look up project-owner role definition", "error", rdErr)
			writeErrorFromErr(w, rdErr, "")
			return
		}
		if binding.RoleDefinitionID == roleDef.ID {
			ownerCount, countErr := s.countDirectOwnerBindings(ctx, projectID)
			if countErr != nil {
				writeErrorFromErr(w, countErr, "")
				return
			}
			if ownerCount <= 1 {
				writeError(w, http.StatusConflict, "LAST_OWNER",
					"Cannot remove the last project owner — every project must retain at least one direct user owner", nil)
				return
			}
		}
	}

	// CanDelegate check.
	if s.authzService != nil {
		decision := s.authzService.CanDelegate(ctx, user, GrantDescriptor{
			Type:      GrantTypeProjectMembership,
			ProjectID: projectID,
		})
		if !decision.Allowed {
			writeForbidden(w, "cannot remove project member: "+decision.Reason)
			return
		}
	}

	if err := s.store.DeleteRoleBinding(ctx, bindingID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Role Binding")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// Resolve role name for logging.
	var roleName string
	if rd, rdErr := s.store.GetRoleDefinition(ctx, binding.RoleDefinitionID); rdErr == nil && rd != nil {
		roleName = rd.Name
	}

	slog.Info("project member removed",
		"project_id", projectID, "binding_id", bindingID,
		"role", roleName, "principal", binding.PrincipalType+":"+binding.PrincipalID,
		"actor", user.Email())

	s.emitMutationAudit(ctx, &store.MutationAuditRecord{
		MutationType:  "project_member_remove",
		TargetType:    "project_membership",
		TargetID:      projectID,
		BeforeSummary: `{"principalId":"` + binding.PrincipalID + `","role":"` + roleName + `"}`,
	})

	w.WriteHeader(http.StatusNoContent)
}
