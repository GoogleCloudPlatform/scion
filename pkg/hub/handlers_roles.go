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
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ---------------------------------------------------------------------------
// Request / Response types
// ---------------------------------------------------------------------------

// createRoleDefinitionRequest is the payload for POST /api/v1/admin/roles.
type createRoleDefinitionRequest struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	ScopeType   string   `json:"scopeType"`
	Permissions []string `json:"permissions"`
}

// updateRoleDefinitionRequest is the payload for PUT /api/v1/admin/roles/:id.
type updateRoleDefinitionRequest struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Permissions []string `json:"permissions"`
}

// createRoleBindingRequest is the payload for POST /api/v1/admin/role-bindings.
type createRoleBindingRequest struct {
	RoleDefinitionID string     `json:"roleDefinitionId"`
	PrincipalType    string     `json:"principalType"`
	PrincipalID      string     `json:"principalId"`
	ScopeType        string     `json:"scopeType"`
	ScopeID          string     `json:"scopeId"`
	NotBefore        *time.Time `json:"notBefore,omitempty"`
	ExpiresAt        *time.Time `json:"expiresAt,omitempty"`
}

// listRoleDefinitionsResponse wraps the list result for the API.
type listRoleDefinitionsResponse struct {
	Items      []*store.RoleDefinition `json:"items"`
	TotalCount int                     `json:"totalCount"`
}

// RoleBindingInfo is a role binding enriched with human-friendly display info.
type RoleBindingInfo struct {
	store.RoleBinding
	PrincipalDisplayName string `json:"principalDisplayName,omitempty"`
	ScopeDisplayName     string `json:"scopeDisplayName,omitempty"`
	CreatedByDisplayName string `json:"createdByDisplayName,omitempty"`
}

// listRoleBindingsResponse wraps the list result for the API.
type listRoleBindingsResponse struct {
	Items      []RoleBindingInfo `json:"items"`
	TotalCount int               `json:"totalCount"`
}

// listPermissionsResponse wraps the permissions registry for the API.
type listPermissionsResponse struct {
	Items      []permissions.Permission `json:"items"`
	TotalCount int                      `json:"totalCount"`
}

// ---------------------------------------------------------------------------
// Route handlers: Role Definitions
// ---------------------------------------------------------------------------

// handleAdminRoles handles GET (list) and POST (create) on
// /api/v1/admin/roles.
// Authorization: route guard checks role.read for GET.
// POST requires role.create via inline Decide.
func (s *Server) handleAdminRoles(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listRoleDefinitions(w, r)
	case http.MethodPost:
		user, ok := s.requireWritePermissionForRole(w, r, "role.create", "create")
		if !ok {
			return
		}
		s.createRoleDefinition(w, r, user)
	default:
		MethodNotAllowed(w)
	}
}

// handleAdminRoleByID handles GET / PUT / DELETE on
// /api/v1/admin/roles/:id.
func (s *Server) handleAdminRoleByID(w http.ResponseWriter, r *http.Request) {
	id := extractID(r, "/api/v1/admin/roles")
	if id == "" {
		BadRequest(w, "role definition ID is required")
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.getRoleDefinition(w, r, id)
	case http.MethodPut:
		user, ok := s.requireWritePermissionForRole(w, r, "role.update", "update")
		if !ok {
			return
		}
		s.updateRoleDefinition(w, r, id, user)
	case http.MethodDelete:
		user, ok := s.requireWritePermissionForRole(w, r, "role.delete", "delete")
		if !ok {
			return
		}
		s.deleteRoleDefinition(w, r, id, user)
	default:
		MethodNotAllowed(w)
	}
}

// ---------------------------------------------------------------------------
// Route handlers: Role Bindings
// ---------------------------------------------------------------------------

// handleAdminRoleBindings handles GET (list) and POST (create) on
// /api/v1/admin/role-bindings.
// Authorization: route guard checks role_binding.read for GET.
// POST requires role_binding.create via inline Decide.
func (s *Server) handleAdminRoleBindings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listRoleBindings(w, r)
	case http.MethodPost:
		user, ok := s.requireWritePermissionForRoleBinding(w, r, "role_binding.create", "create")
		if !ok {
			return
		}
		s.createRoleBinding(w, r, user)
	default:
		MethodNotAllowed(w)
	}
}

// handleAdminRoleBindingByID handles DELETE on /api/v1/admin/role-bindings/:id
// and GET on /api/v1/admin/role-bindings/user/:userID.
func (s *Server) handleAdminRoleBindingByID(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/admin/role-bindings/")
	path = strings.TrimSuffix(path, "/")

	// Handle user-specific binding lookup.
	if strings.HasPrefix(path, "user/") {
		userID := strings.TrimPrefix(path, "user/")
		if userID == "" {
			BadRequest(w, "user ID is required")
			return
		}
		if r.Method != http.MethodGet {
			MethodNotAllowed(w)
			return
		}
		s.listBindingsForUser(w, r, userID)
		return
	}

	// Otherwise it's a binding ID.
	id := path
	if id == "" {
		BadRequest(w, "role binding ID is required")
		return
	}

	switch r.Method {
	case http.MethodDelete:
		user, ok := s.requireWritePermissionForRoleBinding(w, r, "role_binding.delete", "delete")
		if !ok {
			return
		}
		s.deleteRoleBinding(w, r, id, user)
	default:
		MethodNotAllowed(w)
	}
}

// ---------------------------------------------------------------------------
// Route handler: Permissions Registry
// ---------------------------------------------------------------------------

// handleAdminPermissions handles GET on /api/v1/admin/permissions.
func (s *Server) handleAdminPermissions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}
	s.listPermissions(w, r)
}

// ---------------------------------------------------------------------------
// CRUD: Role Definitions
// ---------------------------------------------------------------------------

func (s *Server) listRoleDefinitions(w http.ResponseWriter, r *http.Request) {
	defs, err := s.store.ListRoleDefinitions(r.Context())
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if defs == nil {
		defs = []*store.RoleDefinition{}
	}
	writeJSON(w, http.StatusOK, listRoleDefinitionsResponse{
		Items:      defs,
		TotalCount: len(defs),
	})
}

func (s *Server) getRoleDefinition(w http.ResponseWriter, r *http.Request, id string) {
	def, err := s.store.GetRoleDefinition(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Role Definition")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}
	writeJSON(w, http.StatusOK, def)
}

func (s *Server) createRoleDefinition(w http.ResponseWriter, r *http.Request, user UserIdentity) {
	var req createRoleDefinitionRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "invalid request body: "+err.Error())
		return
	}

	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		BadRequest(w, "name is required")
		return
	}

	if req.ScopeType != store.RoleScopeSystem && req.ScopeType != store.RoleScopeProject {
		BadRequest(w, "scopeType must be \"system\" or \"project\"")
		return
	}

	// Validate permissions against registry.
	if err := validatePermissionIDs(req.Permissions); err != nil {
		BadRequest(w, err.Error())
		return
	}

	// CanDelegate check: actor must hold all permissions in the custom role.
	if s.authzService != nil {
		decision := s.authzService.CanDelegate(r.Context(), user, GrantDescriptor{
			Type:                  GrantTypeCustomRole,
			CustomRolePermissions: req.Permissions,
			ScopeType:             req.ScopeType,
		})
		if !decision.Allowed {
			writeForbidden(w, "cannot create role: "+decision.Reason)
			return
		}
	}

	rd := &store.RoleDefinition{
		Name:        req.Name,
		Description: req.Description,
		ScopeType:   req.ScopeType,
		Permissions: req.Permissions,
		System:      false, // Only seed creates system roles.
	}

	created, err := s.store.CreateRoleDefinition(r.Context(), rd)
	if err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			Conflict(w, "a role definition with this name and scope already exists")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	slog.Info("role definition created",
		"role_id", created.ID, "name", created.Name, "actor", user.Email())

	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) updateRoleDefinition(w http.ResponseWriter, r *http.Request, id string, user UserIdentity) {
	var req updateRoleDefinitionRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "invalid request body: "+err.Error())
		return
	}

	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		BadRequest(w, "name is required")
		return
	}

	// Fetch existing to check system flag.
	existing, err := s.store.GetRoleDefinition(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Role Definition")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	if existing.System {
		writeForbidden(w, "system roles cannot be modified")
		return
	}

	// Validate permissions against registry.
	if err := validatePermissionIDs(req.Permissions); err != nil {
		BadRequest(w, err.Error())
		return
	}

	// CanDelegate check: actor must hold all permissions in the updated role.
	if s.authzService != nil {
		decision := s.authzService.CanDelegate(r.Context(), user, GrantDescriptor{
			Type:                  GrantTypeCustomRole,
			CustomRolePermissions: req.Permissions,
			ScopeType:             existing.ScopeType,
		})
		if !decision.Allowed {
			writeForbidden(w, "cannot update role: "+decision.Reason)
			return
		}
	}

	existing.Name = req.Name
	existing.Description = req.Description
	existing.Permissions = req.Permissions

	updated, err := s.store.UpdateRoleDefinition(r.Context(), existing)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Role Definition")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	slog.Info("role definition updated",
		"role_id", updated.ID, "name", updated.Name, "actor", user.Email())

	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) deleteRoleDefinition(w http.ResponseWriter, r *http.Request, id string, user UserIdentity) {
	// Fetch first to check system flag and include name in logs.
	def, err := s.store.GetRoleDefinition(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Role Definition")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	if def.System {
		writeForbidden(w, "system roles cannot be deleted")
		return
	}

	if err := s.store.DeleteRoleDefinition(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Role Definition")
			return
		}
		if errors.Is(err, store.ErrInvalidInput) {
			Conflict(w, err.Error())
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	slog.Info("role definition deleted",
		"role_id", def.ID, "name", def.Name, "actor", user.Email())

	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// CRUD: Role Bindings
// ---------------------------------------------------------------------------

func (s *Server) listRoleBindings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	limit, offset := parsePaginationParams(r)

	bindings, err := s.store.ListAllRoleBindings(ctx, limit, offset)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if bindings == nil {
		bindings = []*store.RoleBinding{}
	}

	total, err := s.store.CountAllRoleBindings(ctx)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Enrich bindings with human-friendly display names.
	enriched := make([]RoleBindingInfo, 0, len(bindings))
	for i, b := range bindings {
		if b == nil {
			slog.Warn("nil role binding in list result, skipping", "index", i)
			continue
		}
		info := RoleBindingInfo{RoleBinding: *b}
		info.PrincipalDisplayName = s.resolveGroupMemberDisplayName(ctx, b.PrincipalType, b.PrincipalID)
		info.CreatedByDisplayName = s.resolveGroupMemberDisplayName(ctx, store.GroupMemberTypeUser, b.CreatedBy)
		if b.ScopeType == store.RoleScopeProject && b.ScopeID != "" {
			project, err := s.store.GetProject(ctx, b.ScopeID)
			if err == nil && project != nil {
				info.ScopeDisplayName = project.Name
			}
		}
		enriched = append(enriched, info)
	}

	writeJSON(w, http.StatusOK, listRoleBindingsResponse{
		Items:      enriched,
		TotalCount: total,
	})
}

// parsePaginationParams extracts limit and offset from query parameters.
// Returns 0 for limit (store uses default) and 0 for offset if not provided.
func parsePaginationParams(r *http.Request) (limit, offset int) {
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}
	return limit, offset
}

func (s *Server) createRoleBinding(w http.ResponseWriter, r *http.Request, user UserIdentity) {
	var req createRoleBindingRequest
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
	if req.PrincipalType != store.RoleBindingPrincipalUser && req.PrincipalType != store.RoleBindingPrincipalAgent && req.PrincipalType != store.RoleBindingPrincipalGroup {
		BadRequest(w, "principalType must be \"user\", \"agent\", or \"group\"")
		return
	}
	if req.PrincipalID == "" {
		BadRequest(w, "principalId is required")
		return
	}

	// Resolve email to UUID for user principals (mirrors addGroupMember pattern).
	if req.PrincipalType == store.RoleBindingPrincipalUser && strings.Contains(req.PrincipalID, "@") {
		resolvedUser, err := s.store.GetUserByEmail(r.Context(), req.PrincipalID)
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

	// Verify group exists for group principals.
	// Try UUID first, fall back to slug lookup (mirrors email→UUID for users).
	if req.PrincipalType == store.RoleBindingPrincipalGroup {
		g, err := s.store.GetGroup(r.Context(), req.PrincipalID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				g, err = s.store.GetGroupBySlug(r.Context(), req.PrincipalID)
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

	if req.ScopeType != store.RoleScopeSystem && req.ScopeType != store.RoleScopeProject {
		BadRequest(w, "scopeType must be \"system\" or \"project\"")
		return
	}
	if req.ScopeType == store.RoleScopeProject && req.ScopeID == "" {
		BadRequest(w, "scope_id is required when scope_type is 'project'")
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

	// CanDelegate check: security invariant — the actor must hold all
	// permissions granted by the target role.
	if s.authzService != nil {
		decision := s.authzService.CanDelegate(r.Context(), user, GrantDescriptor{
			Type:             GrantTypeRoleBinding,
			RoleDefinitionID: req.RoleDefinitionID,
			ScopeType:        req.ScopeType,
			ScopeID:          req.ScopeID,
		})
		if !decision.Allowed {
			writeForbidden(w, "cannot create binding: "+decision.Reason)
			return
		}
	}

	rb := &store.RoleBinding{
		RoleDefinitionID: req.RoleDefinitionID,
		PrincipalType:    req.PrincipalType,
		PrincipalID:      req.PrincipalID,
		ScopeType:        req.ScopeType,
		ScopeID:          req.ScopeID,
		NotBefore:        req.NotBefore,
		ExpiresAt:        req.ExpiresAt,
		CreatedBy:        user.ID(),
	}

	created, err := s.store.CreateRoleBinding(r.Context(), rb)
	if err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			Conflict(w, "this role binding already exists")
			return
		}
		if errors.Is(err, store.ErrSuperAdminBindingRestricted) {
			writeForbidden(w, "super-admin role bindings can only be created by the system reconciler")
			return
		}
		if errors.Is(err, store.ErrNotFound) {
			BadRequest(w, "role definition not found")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	slog.Info("role binding created",
		"binding_id", created.ID, "role_definition_id", req.RoleDefinitionID,
		"principal", req.PrincipalType+":"+req.PrincipalID, "actor", user.Email())

	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) deleteRoleBinding(w http.ResponseWriter, r *http.Request, id string, user UserIdentity) {
	ctx := r.Context()

	// Verify the binding exists before deleting.
	binding, err := s.store.GetRoleBinding(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Role Binding")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// PM1: last-owner protection — cannot delete the last direct-user
	// project-owner binding. Every project must retain at least one.
	//
	// Known limitation (O2): The count-then-delete sequence is not
	// transactional, so two concurrent deletions of different owner bindings
	// could both pass the check and leave the project with zero owners.
	// Risk is low (requires simultaneous admin operations on the same
	// project) and is mitigated by the offline recovery command (RC1)
	// which can detect and repair orphaned projects. This matches the
	// AC1 TOCTOU pattern accepted elsewhere in the authorization layer.
	if binding.ScopeType == store.RoleScopeProject && binding.PrincipalType == store.RoleBindingPrincipalUser {
		roleDef, rdErr := s.store.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
		if rdErr != nil {
			slog.Error("last-owner check: failed to look up project-owner role definition", "error", rdErr)
			writeErrorFromErr(w, rdErr, "")
			return
		}
		if binding.RoleDefinitionID == roleDef.ID {
			ownerCount, countErr := s.countDirectOwnerBindings(ctx, binding.ScopeID)
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

	if err := s.store.DeleteRoleBinding(ctx, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Role Binding")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	slog.Info("role binding deleted",
		"binding_id", id, "actor", user.Email())

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listBindingsForUser(w http.ResponseWriter, r *http.Request, userID string) {
	ctx := r.Context()
	bindings, err := s.store.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if bindings == nil {
		bindings = []*store.RoleBinding{}
	}

	// Enrich bindings with human-friendly display names.
	enriched := make([]RoleBindingInfo, 0, len(bindings))
	for i, b := range bindings {
		if b == nil {
			slog.Warn("nil role binding in list result, skipping", "index", i)
			continue
		}
		info := RoleBindingInfo{RoleBinding: *b}
		info.PrincipalDisplayName = s.resolveGroupMemberDisplayName(ctx, b.PrincipalType, b.PrincipalID)
		info.CreatedByDisplayName = s.resolveGroupMemberDisplayName(ctx, store.GroupMemberTypeUser, b.CreatedBy)
		if b.ScopeType == store.RoleScopeProject && b.ScopeID != "" {
			project, err := s.store.GetProject(ctx, b.ScopeID)
			if err == nil && project != nil {
				info.ScopeDisplayName = project.Name
			}
		}
		enriched = append(enriched, info)
	}

	writeJSON(w, http.StatusOK, listRoleBindingsResponse{
		Items:      enriched,
		TotalCount: len(enriched),
	})
}

// ---------------------------------------------------------------------------
// Permissions Registry
// ---------------------------------------------------------------------------

func (s *Server) listPermissions(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, listPermissionsResponse{
		Items:      permissions.Registry,
		TotalCount: len(permissions.Registry),
	})
}

// ---------------------------------------------------------------------------
// Authorization helpers
// ---------------------------------------------------------------------------

// requireWritePermissionForRole checks that the authenticated user has the
// specified role permission.
func (s *Server) requireWritePermissionForRole(w http.ResponseWriter, r *http.Request, permission string, action string) (UserIdentity, bool) {
	identity := GetIdentityFromContext(r.Context())
	if identity == nil {
		writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "authentication required", nil)
		return nil, false
	}
	user, ok := identity.(UserIdentity)
	if !ok {
		Forbidden(w)
		return nil, false
	}
	if s.authzService == nil {
		Forbidden(w)
		return nil, false
	}
	decision := s.authzService.Decide(r.Context(), AuthzRequest{
		Principal:  principalContextForIdentity(user),
		Credential: credentialContextForIdentity(user),
		Resource:   Resource{Type: "role", ID: "hub"},
		Action:     Action(action),
		Permission: permission,
	})
	if !decision.Allowed {
		Forbidden(w)
		return nil, false
	}
	return user, true
}

// requireWritePermissionForRoleBinding checks that the authenticated user has
// the specified role_binding permission.
func (s *Server) requireWritePermissionForRoleBinding(w http.ResponseWriter, r *http.Request, permission string, action string) (UserIdentity, bool) {
	identity := GetIdentityFromContext(r.Context())
	if identity == nil {
		writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "authentication required", nil)
		return nil, false
	}
	user, ok := identity.(UserIdentity)
	if !ok {
		Forbidden(w)
		return nil, false
	}
	if s.authzService == nil {
		Forbidden(w)
		return nil, false
	}
	decision := s.authzService.Decide(r.Context(), AuthzRequest{
		Principal:  principalContextForIdentity(user),
		Credential: credentialContextForIdentity(user),
		Resource:   Resource{Type: "role_binding", ID: "hub"},
		Action:     Action(action),
		Permission: permission,
	})
	if !decision.Allowed {
		Forbidden(w)
		return nil, false
	}
	return user, true
}

// validatePermissionIDs checks that all provided IDs exist in the permissions registry.
func validatePermissionIDs(ids []string) error {
	valid := make(map[string]bool, len(permissions.Registry))
	for _, p := range permissions.Registry {
		valid[p.ID] = true
	}
	var invalid []string
	for _, id := range ids {
		if !valid[id] {
			invalid = append(invalid, id)
		}
	}
	if len(invalid) > 0 {
		return errors.New("invalid permission IDs: " + strings.Join(invalid, ", "))
	}
	return nil
}
