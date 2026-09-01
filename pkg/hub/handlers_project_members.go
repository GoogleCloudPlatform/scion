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
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ---------------------------------------------------------------------------
// Request / Response types for project-scoped members API (PM1 → RS1)
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
	Items        []projectMemberInfo     `json:"items"`
	TotalCount   int                     `json:"totalCount"`
	Capabilities *MembershipCapabilities `json:"_capabilities,omitempty"`
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

// transferOwnershipRequest is the payload for POST /api/v1/projects/{id}/transfer-ownership.
type transferOwnershipRequest struct {
	NewOwnerID string `json:"newOwnerId"`
}

// ---------------------------------------------------------------------------
// Valid project-scoped role names
// ---------------------------------------------------------------------------

var validProjectRoles = map[string]bool{
	store.ProjectRoleOwner:  true,
	store.ProjectRoleAdmin:  true,
	store.ProjectRoleMember: true,
}

// directUserOnlyProjectRoles are project roles that can only be assigned to
// direct user principals. RS1 D3: project-admin group eligibility is now
// approved and implemented, so only project-owner remains in this set.
var directUserOnlyProjectRoles = map[string]bool{
	store.ProjectRoleOwner: true,
	// D3: project-admin removed — groups may now hold project-admin.
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

	// Enrich with role name and display names.
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
			Source:      "direct",
		}
		info.PrincipalDisplayName = s.resolveGroupMemberDisplayName(ctx, b.PrincipalType, b.PrincipalID)
		info.CreatedByDisplayName = s.resolveGroupMemberDisplayName(ctx, store.GroupMemberTypeUser, b.CreatedBy)

		items = append(items, info)
	}

	// RS1: Server-derived operation/target capabilities replace C0 owner-only
	// advisory capability. Capabilities are computed per the governance matrix.
	var memberCaps *MembershipCapabilities
	if identity := GetIdentityFromContext(ctx); identity != nil {
		if user, ok := identity.(UserIdentity); ok {
			memberCaps = s.membershipService.ComputeCapabilities(ctx, user.ID(), projectID)
		}
	}

	writeJSON(w, http.StatusOK, listProjectMembersResponse{
		Items:        items,
		TotalCount:   totalCount,
		Capabilities: memberCaps,
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

	// Validate lifecycle fields.
	if req.ExpiresAt != nil && !req.ExpiresAt.After(time.Now()) {
		BadRequest(w, "expiresAt must be in the future")
		return
	}
	if req.NotBefore != nil && req.ExpiresAt != nil && !req.ExpiresAt.After(*req.NotBefore) {
		BadRequest(w, "expiresAt must be after notBefore")
		return
	}

	// RS1: Delegate to the project membership service. The service implements
	// governance matrix, delegation checks, one-binding invariant, and audit.
	result, decision := s.membershipService.AddMember(ctx, MembershipRequest{
		Op:            MembershipOpAdd,
		ProjectID:     projectID,
		Actor:         user,
		PrincipalType: req.PrincipalType,
		PrincipalID:   req.PrincipalID,
		RoleDefID:     req.RoleDefinitionID,
		NotBefore:     req.NotBefore,
		ExpiresAt:     req.ExpiresAt,
	})
	if decision != nil && !decision.Allowed {
		slog.Info("project member add denied",
			"project_id", projectID, "actor", user.Email(),
			"denial_code", decision.DenialCode, "reason", decision.Reason)
		writeError(w, decision.HTTPStatus, decision.DenialCode, decision.Reason, nil)
		return
	}

	// Resolve role name for response.
	var roleName string
	if result.Binding != nil {
		if rd, err := s.store.GetRoleDefinition(ctx, result.Binding.RoleDefinitionID); err == nil {
			roleName = rd.Name
		}
	}

	// Return enriched response.
	info := projectMemberInfo{
		RoleBinding: *result.Binding,
		RoleName:    roleName,
		Source:      "direct",
	}
	info.PrincipalDisplayName = s.resolveGroupMemberDisplayName(ctx, result.Binding.PrincipalType, result.Binding.PrincipalID)
	info.CreatedByDisplayName = s.resolveGroupMemberDisplayName(ctx, store.GroupMemberTypeUser, result.Binding.CreatedBy)

	status := http.StatusCreated
	if result.Replaced {
		status = http.StatusOK // atomic replacement returns 200, not 201
	}
	writeJSON(w, status, info)
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

	// RS1: Delegate to the project membership service.
	result, decision := s.membershipService.UpdateMemberRole(ctx, MembershipRequest{
		Op:           MembershipOpUpdate,
		ProjectID:    projectID,
		Actor:        user,
		BindingID:    bindingID,
		NewRoleDefID: req.RoleDefinitionID,
	})
	if decision != nil && !decision.Allowed {
		slog.Info("project member role change denied",
			"project_id", projectID, "actor", user.Email(),
			"denial_code", decision.DenialCode, "reason", decision.Reason)
		writeError(w, decision.HTTPStatus, decision.DenialCode, decision.Reason, nil)
		return
	}

	// Enrich response.
	var roleName string
	if result.Binding != nil {
		if rd, err := s.store.GetRoleDefinition(ctx, result.Binding.RoleDefinitionID); err == nil {
			roleName = rd.Name
		}
	}

	info := projectMemberInfo{
		RoleBinding: *result.Binding,
		RoleName:    roleName,
		Source:      "direct",
	}
	info.PrincipalDisplayName = s.resolveGroupMemberDisplayName(ctx, result.Binding.PrincipalType, result.Binding.PrincipalID)
	info.CreatedByDisplayName = s.resolveGroupMemberDisplayName(ctx, store.GroupMemberTypeUser, result.Binding.CreatedBy)

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

	// RS1: Delegate to the project membership service.
	_, decision := s.membershipService.RemoveMember(ctx, MembershipRequest{
		Op:        MembershipOpRemove,
		ProjectID: projectID,
		Actor:     user,
		BindingID: bindingID,
	})
	if decision != nil && !decision.Allowed {
		slog.Info("project member removal denied",
			"project_id", projectID, "actor", user.Email(),
			"denial_code", decision.DenialCode, "reason", decision.Reason)
		writeError(w, decision.HTTPStatus, decision.DenialCode, decision.Reason, nil)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// POST /api/v1/projects/{id}/transfer-ownership — atomic ownership transfer
// ---------------------------------------------------------------------------

// handleTransferOwnership handles the atomic ownership transfer endpoint.
func (s *Server) handleTransferOwnership(w http.ResponseWriter, r *http.Request, projectID string) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

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

	var req transferOwnershipRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "invalid request body: "+err.Error())
		return
	}

	if req.NewOwnerID == "" {
		// Try email resolution.
		BadRequest(w, "newOwnerId is required")
		return
	}

	// Resolve email to UUID if needed.
	if strings.Contains(req.NewOwnerID, "@") {
		resolvedUser, err := s.store.GetUserByEmail(ctx, req.NewOwnerID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				BadRequest(w, "user not found with email: "+req.NewOwnerID)
				return
			}
			writeErrorFromErr(w, err, "")
			return
		}
		req.NewOwnerID = resolvedUser.ID
	}

	// Delegate to the membership service.
	result, decision := s.membershipService.TransferOwnership(ctx, MembershipRequest{
		Op:         MembershipOpTransfer,
		ProjectID:  projectID,
		Actor:      user,
		NewOwnerID: req.NewOwnerID,
	})
	if decision != nil && !decision.Allowed {
		slog.Info("project ownership transfer denied",
			"project_id", projectID, "actor", user.Email(),
			"denial_code", decision.DenialCode, "reason", decision.Reason)
		writeError(w, decision.HTTPStatus, decision.DenialCode, decision.Reason, nil)
		return
	}

	// Build response.
	type transferResponse struct {
		NewOwnerBinding *store.RoleBinding `json:"newOwnerBinding"`
		OldOwnerBinding *store.RoleBinding `json:"oldOwnerBinding,omitempty"`
		Message         string             `json:"message"`
	}

	resp := transferResponse{
		NewOwnerBinding: result.Binding,
		OldOwnerBinding: result.TransferOldOwnerBinding,
		Message:         fmt.Sprintf("Ownership transferred to %s", req.NewOwnerID),
	}

	writeJSON(w, http.StatusOK, resp)
}

// ErrCodePrincipalIneligible indicates the principal type cannot hold the
// requested role.
const ErrCodePrincipalIneligible = "principal_ineligible"
