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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// duplicateRoleDefinitionRequest is the payload for POST /api/v1/admin/roles/:id/duplicate.
type duplicateRoleDefinitionRequest struct {
	Name string `json:"name"`
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
	RoleName             string `json:"roleName,omitempty"`
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

// exportedRole is the portable representation of a custom role definition.
type exportedRole struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	ScopeType   string   `json:"scopeType"`
	Permissions []string `json:"permissions"`
}

// roleExportResponse is the envelope for GET /api/v1/admin/roles/export.
type roleExportResponse struct {
	Version    string         `json:"version"`
	ExportedAt string         `json:"exportedAt"`
	Roles      []exportedRole `json:"roles"`
}

// roleImportRequest is the payload for POST /api/v1/admin/roles/import.
type roleImportRequest struct {
	Version string         `json:"version"`
	Roles   []exportedRole `json:"roles"`
}

// roleImportResultItem describes the outcome for a single role in an import.
type roleImportResultItem struct {
	Name   string `json:"name"`
	Status string `json:"status"` // "created", "skipped", "error"
	Reason string `json:"reason,omitempty"`
	ID     string `json:"id,omitempty"`
}

// roleImportResponse is the response for POST /api/v1/admin/roles/import.
type roleImportResponse struct {
	Created int                    `json:"created"`
	Skipped int                    `json:"skipped"`
	Errors  int                    `json:"errors"`
	Items   []roleImportResultItem `json:"items"`
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
// /api/v1/admin/roles/:id and POST on /api/v1/admin/roles/:id/duplicate.
func (s *Server) handleAdminRoleByID(w http.ResponseWriter, r *http.Request) {
	id, action := extractAction(r, "/api/v1/admin/roles")
	if id == "" {
		BadRequest(w, "role definition ID is required")
		return
	}

	// Sub-resource action: POST /api/v1/admin/roles/:id/duplicate
	if action == "duplicate" {
		if r.Method != http.MethodPost {
			MethodNotAllowed(w)
			return
		}
		user, ok := s.requireWritePermissionForRole(w, r, "role.create", "create")
		if !ok {
			return
		}
		s.duplicateRoleDefinition(w, r, id, user)
		return
	}

	if action != "" {
		NotFound(w, "Route")
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
//
// Authorization is scope-aware:
//
//   - GET requires role_binding.read at hub scope (inline check —
//     the route guard is RouteAuthenticated, not RouteHubAdmin, because
//     POST needs scope-dependent auth).
//
//   - POST for project-scoped requests defers authorization to
//     ProjectMembershipService, which checks project.manage + the
//     governance matrix. This allows project owners (who lack hub-level
//     role_binding.create) to manage their project's membership.
//
//   - POST for system-scoped requests requires role_binding.create at
//     hub scope (super-admin / hub-admin only).
func (s *Server) handleAdminRoleBindings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		// Inline authorization: role_binding.read at hub scope.
		if !s.authorize(w, r, Resource{Type: "role_binding", ID: "hub"}, ActionRead) {
			return
		}
		s.listRoleBindings(w, r)
	case http.MethodPost:
		s.createRoleBindingScopeAware(w, r)
	default:
		MethodNotAllowed(w)
	}
}

// createRoleBindingScopeAware is the POST entry point for
// /api/v1/admin/role-bindings. It peeks at the request body to determine
// the scope and applies scope-appropriate authorization:
//
//   - Project-scoped requests skip the hub-level role_binding.create check.
//     Authorization is delegated to ProjectMembershipService which checks
//     project.manage and the governance matrix. This allows project owners
//     (who lack hub-level role_binding.create) to manage their project's
//     membership through the admin API.
//
//   - System-scoped requests require role_binding.create at hub scope —
//     same as before.
func (s *Server) createRoleBindingScopeAware(w http.ResponseWriter, r *http.Request) {
	// Read body so we can peek at scope and replay for createRoleBinding.
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		BadRequest(w, "failed to read request body")
		return
	}
	_ = r.Body.Close()

	// Lightweight peek: unmarshal just enough to know scope and role.
	var peek struct {
		ScopeType        string `json:"scopeType"`
		RoleDefinitionID string `json:"roleDefinitionId"`
	}
	if err := json.Unmarshal(bodyBytes, &peek); err != nil {
		BadRequest(w, "invalid request body: "+err.Error())
		return
	}

	// Extract identity (required for both paths).
	identity := GetIdentityFromContext(r.Context())
	if identity == nil {
		writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "authentication required", nil)
		return
	}
	user, ok := identity.(UserIdentity)
	if !ok {
		Forbidden(w)
		return
	}

	// Authorization routing:
	// - Built-in project roles (owner/admin/member) are authorized by the
	//   membership service (project.manage + governance matrix) — no hub-level
	//   role_binding.create needed. This allows project owners to manage
	//   members via the admin API.
	// - Custom project-scoped roles and system-scoped requests require
	//   hub-level role_binding.create permission.
	requireHubAuth := peek.ScopeType != store.RoleScopeProject
	if peek.ScopeType == store.RoleScopeProject && peek.RoleDefinitionID != "" {
		// Check if this is a built-in project role.
		roleDef, err := s.store.GetRoleDefinition(r.Context(), peek.RoleDefinitionID)
		if err == nil && !validProjectRoles[roleDef.Name] {
			// Custom project role — require hub-level auth.
			requireHubAuth = true
		}
		// If role lookup fails, createRoleBinding will handle the error.
	}

	if requireHubAuth {
		if s.authzService != nil {
			decision := s.authzService.Decide(r.Context(), AuthzRequest{
				Principal:  principalContextForIdentity(user),
				Credential: credentialContextForIdentity(user),
				Resource:   Resource{Type: "role_binding", ID: "hub"},
				Action:     Action("create"),
				Permission: "role_binding.create",
			})
			if !decision.Allowed {
				writeForbiddenStructured(w, "", "role_binding", Action("create"))
				return
			}
		}
	}

	// Replay the body so createRoleBinding can parse it normally.
	r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	s.createRoleBinding(w, r, user)
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

func (s *Server) duplicateRoleDefinition(w http.ResponseWriter, r *http.Request, sourceID string, user UserIdentity) {
	var req duplicateRoleDefinitionRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "invalid request body: "+err.Error())
		return
	}

	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		BadRequest(w, "name is required")
		return
	}

	// Fetch source role (works for both system and custom roles).
	source, err := s.store.GetRoleDefinition(r.Context(), sourceID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Role Definition")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// Validate permissions against registry (source may reference
	// permissions that were removed since seeding; reject if so).
	if err := validatePermissionIDs(source.Permissions); err != nil {
		BadRequest(w, err.Error())
		return
	}

	// CanDelegate check: actor must hold all permissions in the duplicated role.
	if s.authzService != nil {
		decision := s.authzService.CanDelegate(r.Context(), user, GrantDescriptor{
			Type:                  GrantTypeCustomRole,
			CustomRolePermissions: source.Permissions,
			ScopeType:             source.ScopeType,
		})
		if !decision.Allowed {
			writeForbidden(w, "cannot duplicate role: "+decision.Reason)
			return
		}
	}

	rd := &store.RoleDefinition{
		Name:        req.Name,
		Description: source.Description,
		ScopeType:   source.ScopeType,
		Permissions: source.Permissions,
		System:      false, // Duplicates are always custom roles.
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

	slog.Info("role definition duplicated",
		"source_id", sourceID, "new_role_id", created.ID,
		"name", created.Name, "actor", user.Email())

	writeJSON(w, http.StatusCreated, created)
}

// ---------------------------------------------------------------------------
// Export / Import: Role Definitions
// ---------------------------------------------------------------------------

// handleAdminRolesExport handles GET on /api/v1/admin/roles/export.
// Authorization: route guard checks role.read.
func (s *Server) handleAdminRolesExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}
	s.exportRoleDefinitions(w, r)
}

// handleAdminRolesImport handles POST on /api/v1/admin/roles/import.
// Authorization: route guard checks role.read; inline check requires role.create.
func (s *Server) handleAdminRolesImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}
	user, ok := s.requireWritePermissionForRole(w, r, "role.create", "create")
	if !ok {
		return
	}
	s.importRoleDefinitions(w, r, user)
}

// exportRoleDefinitions returns all custom (non-system) role definitions in a
// portable JSON format suitable for importing into another instance.
func (s *Server) exportRoleDefinitions(w http.ResponseWriter, r *http.Request) {
	defs, err := s.store.ListRoleDefinitions(r.Context())
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	var roles []exportedRole
	for _, d := range defs {
		if d.System {
			continue
		}
		roles = append(roles, exportedRole{
			Name:        d.Name,
			Description: d.Description,
			ScopeType:   d.ScopeType,
			Permissions: d.Permissions,
		})
	}
	if roles == nil {
		roles = []exportedRole{}
	}

	writeJSON(w, http.StatusOK, roleExportResponse{
		Version:    "1",
		ExportedAt: time.Now().UTC().Format(time.RFC3339),
		Roles:      roles,
	})
}

// systemRoleNames is the set of built-in role names that cannot be imported.
var systemRoleNames = map[string]bool{
	store.SystemRoleSuperAdmin: true,
	store.SystemRoleHubAdmin:   true,
	store.SystemRoleHubMember:  true,
	store.SystemRoleHubViewer:  true,
	store.ProjectRoleOwner:     true,
	store.ProjectRoleAdmin:     true,
	store.ProjectRoleMember:    true,
	store.AgentRoleDefNone:     true,
	store.AgentRoleDefReadonly: true,
	store.AgentRoleDefBaseline: true,
	store.AgentRoleDefFull:     true,
}

// importRoleDefinitions creates custom role definitions from a portable JSON
// export. System roles are rejected. Name conflicts with existing custom roles
// are skipped (reported as "skipped"). Each role's permissions are validated
// against the registry and checked via CanDelegate.
func (s *Server) importRoleDefinitions(w http.ResponseWriter, r *http.Request, user UserIdentity) {
	var req roleImportRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "invalid request body: "+err.Error())
		return
	}

	if req.Version != "1" {
		BadRequest(w, "unsupported export version: expected \"1\"")
		return
	}

	if len(req.Roles) == 0 {
		BadRequest(w, "no roles to import")
		return
	}

	resp := roleImportResponse{
		Items: make([]roleImportResultItem, 0, len(req.Roles)),
	}

	for _, role := range req.Roles {
		item := roleImportResultItem{Name: role.Name}

		// Validate name.
		name := strings.TrimSpace(role.Name)
		if name == "" {
			item.Status = "error"
			item.Reason = "name is required"
			resp.Errors++
			resp.Items = append(resp.Items, item)
			continue
		}

		// Reject system role names.
		if systemRoleNames[name] {
			item.Status = "error"
			item.Reason = "cannot import system role"
			resp.Errors++
			resp.Items = append(resp.Items, item)
			continue
		}

		// Validate scope type.
		if role.ScopeType != store.RoleScopeSystem && role.ScopeType != store.RoleScopeProject {
			item.Status = "error"
			item.Reason = "scopeType must be \"system\" or \"project\""
			resp.Errors++
			resp.Items = append(resp.Items, item)
			continue
		}

		// Validate permissions against registry.
		if err := validatePermissionIDs(role.Permissions); err != nil {
			item.Status = "error"
			item.Reason = err.Error()
			resp.Errors++
			resp.Items = append(resp.Items, item)
			continue
		}

		// CanDelegate check: actor must hold all permissions being imported.
		if s.authzService != nil {
			decision := s.authzService.CanDelegate(r.Context(), user, GrantDescriptor{
				Type:                  GrantTypeCustomRole,
				CustomRolePermissions: role.Permissions,
				ScopeType:             role.ScopeType,
			})
			if !decision.Allowed {
				item.Status = "error"
				item.Reason = "cannot delegate: " + decision.Reason
				resp.Errors++
				resp.Items = append(resp.Items, item)
				continue
			}
		}

		// Attempt to create. Name conflicts with existing roles are skipped.
		rd := &store.RoleDefinition{
			Name:        name,
			Description: role.Description,
			ScopeType:   role.ScopeType,
			Permissions: role.Permissions,
			System:      false,
		}
		created, err := s.store.CreateRoleDefinition(r.Context(), rd)
		if err != nil {
			if errors.Is(err, store.ErrAlreadyExists) {
				item.Status = "skipped"
				item.Reason = "role with this name and scope already exists"
				resp.Skipped++
				resp.Items = append(resp.Items, item)
				continue
			}
			item.Status = "error"
			item.Reason = err.Error()
			resp.Errors++
			resp.Items = append(resp.Items, item)
			continue
		}

		item.Status = "created"
		item.ID = created.ID
		resp.Created++
		resp.Items = append(resp.Items, item)

		slog.Info("role definition imported",
			"role_id", created.ID, "name", created.Name, "actor", user.Email())
	}

	writeJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// CRUD: Role Bindings
// ---------------------------------------------------------------------------

func (s *Server) listRoleBindings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	limit, offset := parsePaginationParams(r)

	// Parse optional sort parameters.
	sortBy := store.RoleBindingSortCreated // default
	if v := r.URL.Query().Get("sort_by"); v != "" {
		if !store.ValidRoleBindingSortField(v) {
			BadRequest(w, "invalid sort_by value: must be one of principal, role, created")
			return
		}
		sortBy = store.RoleBindingSortField(v)
	}
	sortOrder := "desc"
	if v := r.URL.Query().Get("sort_order"); v != "" {
		if v != "asc" && v != "desc" {
			BadRequest(w, "invalid sort_order value: must be asc or desc")
			return
		}
		sortOrder = v
	}

	bindings, err := s.store.ListAllRoleBindings(ctx, store.RoleBindingListOptions{
		Limit:     limit,
		Offset:    offset,
		SortBy:    sortBy,
		SortOrder: sortOrder,
	})
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

	s.enrichBindingRoleNames(ctx, enriched)

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

	// Agents are project-bound; system-scope bindings on agents are
	// ineffective because agent credentials are scoped to their project
	// and the live delegation ceiling prevents hub-level privilege.
	if req.PrincipalType == store.RoleBindingPrincipalAgent && req.ScopeType == store.RoleScopeSystem {
		BadRequest(w, "agent principals are project-bound and cannot hold system-scoped role bindings")
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

	// R4 brief item 2: project-scoped role-binding create for built-in
	// project roles (owner/admin/member) MUST route through
	// ProjectMembershipService for typed governance, delegation, lock,
	// last-owner protection, transactional audit, and D4 enforcement.
	// Custom project-scoped roles (e.g. agent delegation bindings) fall
	// through to the generic CanDelegate path below.
	// R5-1: fail-closed — if membershipService is nil for built-in roles,
	// return 500. Never fall through to direct store mutation.
	if req.ScopeType == store.RoleScopeProject {
		// Resolve the role definition to check if it's a built-in project role.
		roleDef, err := s.store.GetRoleDefinition(r.Context(), req.RoleDefinitionID)
		if err != nil {
			BadRequest(w, "role definition not found")
			return
		}
		if validProjectRoles[roleDef.Name] {
			// Built-in project role — route through membership service.
			if s.membershipService == nil {
				writeError(w, http.StatusInternalServerError, "internal_error",
					"membership service unavailable — project-scope mutations require governance", nil)
				return
			}
			mReq := MembershipRequest{
				Op:            MembershipOpAdd,
				ProjectID:     req.ScopeID,
				Actor:         user,
				PrincipalType: req.PrincipalType,
				PrincipalID:   req.PrincipalID,
				RoleDefID:     req.RoleDefinitionID,
				NotBefore:     req.NotBefore,
				ExpiresAt:     req.ExpiresAt,
			}
			result, denial := s.membershipService.AddMember(r.Context(), mReq)
			if denial != nil && !denial.Allowed {
				writeError(w, denial.HTTPStatus, denial.DenialCode, denial.Reason, nil)
				return
			}
			writeJSON(w, http.StatusCreated, result.Binding)
			return
		}
		// Custom project-scoped role — fall through to CanDelegate path.
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
			writeForbiddenStructured(w, "cannot create binding: "+decision.Reason, "role_binding", Action("create"))
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

	// R4 brief item 2: project-scoped role-binding delete MUST route
	// through ProjectMembershipService for serialization lock,
	// last-owner protection, typed governance, transactional audit,
	// and stable denial mapping. System-scoped operations remain generic.
	// R5-1: fail-closed — if membershipService is nil, return 500. Never fall
	// through to direct store mutation for project scope.
	if binding.ScopeType == store.RoleScopeProject {
		if s.membershipService == nil {
			writeError(w, http.StatusInternalServerError, "internal_error",
				"membership service unavailable — project-scope mutations require governance", nil)
			return
		}
		mReq := MembershipRequest{
			Op:        MembershipOpRemove,
			ProjectID: binding.ScopeID,
			Actor:     user,
			BindingID: id,
		}
		_, denial := s.membershipService.RemoveMember(ctx, mReq)
		if denial != nil && !denial.Allowed {
			writeError(w, denial.HTTPStatus, denial.DenialCode, denial.Reason, nil)
			return
		}
		slog.Info("role binding deleted (via membership service)",
			"binding_id", id, "actor", user.Email())
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// R6: system-scoped super-admin binding deletion must go through the
	// same invariant guards as user PATCH role demotion: CanDelegate,
	// last-admin serialized check, self-lockout, and transactional audit.
	// Without this, any hub-admin with role_binding.delete could delete
	// the sole super-admin binding and lock out the system.
	superAdminRD, err := s.store.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	if err != nil {
		writeErrorFromErr(w, err, "super-admin role definition lookup")
		return
	}
	if binding.RoleDefinitionID == superAdminRD.ID && binding.ScopeType == store.RoleScopeSystem {
		s.deleteSystemSuperAdminBinding(w, r, binding, user, superAdminRD)
		return
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

// deleteSystemSuperAdminBinding handles deletion of a system-scoped
// super-admin binding through the generic DELETE endpoint. It applies the
// same invariant guards as user PATCH role demotion (R6):
//
//   - Credential boundary: session/dev credentials only (no broker/agent/UAT/federation)
//   - CanDelegate: caller must have delegation authority over the super-admin role
//   - Self-lockout: caller cannot remove their own super-admin binding
//   - Last-admin: serialized check prevents removing the sole active admin
//   - Audit: transactional mutation audit record
//
// Without these guards, any hub-admin with role_binding.delete could delete
// the sole super-admin binding and lock out the system.
func (s *Server) deleteSystemSuperAdminBinding(
	w http.ResponseWriter, r *http.Request,
	binding *store.RoleBinding, actor UserIdentity, rd *store.RoleDefinition,
) {
	ctx := r.Context()

	// Credential boundary: super-admin binding mutations require interactive
	// session or dev credentials. Reject broker, agent JWT, UAT, and
	// federation tokens (R6 credential gate).
	cred := GetCredentialContextFromContext(ctx)
	if !allowedMutationCredentials[cred.Kind] {
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			fmt.Sprintf("super-admin binding deletion requires an interactive session; credential kind %q is not allowed", cred.Kind), nil)
		return
	}

	// CanDelegate: caller must have delegation authority over super-admin.
	canDel := s.authzService.CanDelegate(ctx, actor, GrantDescriptor{
		Type:             GrantTypeRoleBinding,
		RoleDefinitionID: rd.ID,
		ScopeType:        store.RoleScopeSystem,
	})
	if !canDel.Allowed {
		writeForbiddenStructured(w, "insufficient authority to delete super-admin binding: "+canDel.Reason, "role_binding", Action("delete"))
		return
	}

	// Self-lockout: prevent the actor from removing their own super-admin binding.
	if binding.PrincipalType == store.RoleBindingPrincipalUser && binding.PrincipalID == actor.ID() {
		writeError(w, http.StatusConflict, "self_lockout",
			"cannot delete your own super-admin binding; ask another admin", nil)
		return
	}

	// All mutations inside a single atomic transaction with serialization.
	var txErr error
	txErr = s.store.WithTx(ctx, func(tx store.Store) error {
		// Last-admin guard with serialization lock.
		if err := s.checkLastSuperAdminTx(ctx, tx, binding.PrincipalID, rd); err != nil {
			return err
		}

		// Delete the binding.
		if err := tx.DeleteRoleBinding(ctx, binding.ID); err != nil {
			return fmt.Errorf("delete super-admin binding: %w", err)
		}

		// Synchronous transactional audit.
		auditActor := s.buildAuditActorFromContext(ctx)
		if err := tx.CreateMutationAudit(ctx, &store.MutationAuditRecord{
			MutationType:        "role_binding_delete",
			ActorPrincipalKind:  auditActor.kind,
			ActorPrincipalID:    auditActor.id,
			ActorCredentialID:   auditActor.credID,
			ActorCredentialType: auditActor.credType,
			TargetType:          "role_binding",
			TargetID:            binding.ID,
			BeforeSummary:       fmt.Sprintf(`{"principal_type":%q,"principal_id":%q,"role":%q,"scope_type":%q}`, binding.PrincipalType, binding.PrincipalID, store.SystemRoleSuperAdmin, binding.ScopeType),
			AfterSummary:        `{"deleted":true,"source":"generic_delete_endpoint"}`,
			Timestamp:           time.Now(),
		}); err != nil {
			return fmt.Errorf("audit super-admin binding delete: %w", err)
		}

		return nil
	})
	if txErr != nil {
		if errors.Is(txErr, errLastSuperAdmin) {
			writeError(w, http.StatusConflict, "last_admin",
				txErr.Error(), nil)
			return
		}
		writeErrorFromErr(w, txErr, "")
		return
	}

	slog.Info("deleted super-admin binding via generic delete endpoint",
		"binding_id", binding.ID, "principal_id", binding.PrincipalID,
		"actor", actor.Email())
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

	s.enrichBindingRoleNames(ctx, enriched)

	writeJSON(w, http.StatusOK, listRoleBindingsResponse{
		Items:      enriched,
		TotalCount: len(enriched),
	})
}

// enrichBindingRoleNames resolves role-definition names for a slice of
// RoleBindingInfo, setting the RoleName field. Uses a single batch lookup
// to avoid N+1 queries, falling back to per-binding lookups when the batch
// API is unavailable.
func (s *Server) enrichBindingRoleNames(ctx context.Context, bindings []RoleBindingInfo) {
	// Collect unique role definition IDs.
	idSet := make(map[string]struct{}, len(bindings))
	for _, b := range bindings {
		if b.RoleDefinitionID != "" {
			idSet[b.RoleDefinitionID] = struct{}{}
		}
	}
	if len(idSet) == 0 {
		return
	}

	ids := make([]string, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
	}

	nameMap := make(map[string]string, len(ids))

	// Try batch lookup first.
	defs, err := s.store.GetRoleDefinitionsByIDs(ctx, ids)
	if err == nil {
		for _, d := range defs {
			nameMap[d.ID] = d.Name
		}
	} else {
		// Fallback: per-ID lookup.
		for _, id := range ids {
			rd, err := s.store.GetRoleDefinition(ctx, id)
			if err == nil {
				nameMap[rd.ID] = rd.Name
			}
		}
	}

	for i := range bindings {
		if name, ok := nameMap[bindings[i].RoleDefinitionID]; ok {
			bindings[i].RoleName = name
		}
	}
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
		writeForbiddenStructured(w, "", "role_binding", Action(action))
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
