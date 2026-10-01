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
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
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
	// RoleKind is "builtin" or "custom". Additive field (ptone/scion#2529 P1,
	// design.md §3.1); every existing consumer of projectMemberInfo ignores
	// unknown JSON fields. No `omitempty` (review r1 L5): design.md §3.1 says
	// it appears "on every endpoint", and every construction site sets it via
	// projectRoleKind, so it is never the empty string in practice.
	RoleKind string `json:"roleKind"`
}

// projectMemberGroup is one principal's project membership: its built-in
// role (if any) plus every custom role it holds, as returned by
// PUT/DELETE …/members/principals/{type}/{id} (ptone/scion#2529 P1,
// design.md §3.1).
type projectMemberGroup struct {
	PrincipalType        string              `json:"principalType"`
	PrincipalID          string              `json:"principalId"`
	PrincipalDisplayName string              `json:"principalDisplayName,omitempty"`
	BuiltInRoleName      string              `json:"builtInRoleName"`
	Bindings             []projectMemberInfo `json:"bindings"`
	// Changed deliberately has no `omitempty`: the idempotent PUT response
	// must show `"changed":false` explicitly (design.md §3.6), not omit the
	// field, so clients can distinguish it from a response that never set it.
	Changed bool `json:"changed"`
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
		MethodNotAllowed(w, http.MethodGet, http.MethodPost)
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
		MethodNotAllowed(w, http.MethodPatch, http.MethodDelete)
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
			RoleKind:    projectRoleKind(roleName),
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

	// Resolve a user email or group slug to its canonical ID (extracted as
	// resolveMemberPrincipal, design.md §3.1, so the PUT/DELETE principal
	// endpoints share this resolution logic with POST).
	if req.PrincipalType == store.RoleBindingPrincipalUser || req.PrincipalType == store.RoleBindingPrincipalGroup {
		resolvedID, err := s.resolveMemberPrincipal(ctx, req.PrincipalType, req.PrincipalID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				if req.PrincipalType == store.RoleBindingPrincipalUser {
					BadRequest(w, "user not found with email: "+req.PrincipalID)
				} else {
					BadRequest(w, "group not found: "+req.PrincipalID)
				}
				return
			}
			writeErrorFromErr(w, err, "")
			return
		}
		req.PrincipalID = resolvedID
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
	if s.membershipService == nil {
		http.Error(w, "membership service not configured", http.StatusInternalServerError)
		return
	}
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
		RoleKind:    projectRoleKind(roleName),
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
	if s.membershipService == nil {
		http.Error(w, "membership service not configured", http.StatusInternalServerError)
		return
	}
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
		RoleKind:    projectRoleKind(roleName),
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
	if s.membershipService == nil {
		http.Error(w, "membership service not configured", http.StatusInternalServerError)
		return
	}
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
		MethodNotAllowed(w, http.MethodPost)
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
	if s.membershipService == nil {
		http.Error(w, "membership service not configured", http.StatusInternalServerError)
		return
	}
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

// ---------------------------------------------------------------------------
// resolveMemberPrincipal — shared user-email / group-slug resolution
// (ptone/scion#2529 P1, design.md §3.1). Extracted from addProjectMember so
// POST /members and PUT/DELETE …/members/principals/{type}/{id} resolve
// principals the same way. Returns store.ErrNotFound when a user email or
// group slug does not resolve; callers format their own error message so
// existing response text (and existing tests) is unchanged.
// ---------------------------------------------------------------------------

func (s *Server) resolveMemberPrincipal(ctx context.Context, principalType, principalID string) (string, error) {
	switch principalType {
	case store.RoleBindingPrincipalUser:
		if !strings.Contains(principalID, "@") {
			return principalID, nil
		}
		u, err := s.store.GetUserByEmail(ctx, principalID)
		if err != nil {
			return "", err
		}
		if u == nil {
			return "", store.ErrNotFound
		}
		return u.ID, nil
	case store.RoleBindingPrincipalGroup:
		g, err := s.store.GetGroup(ctx, principalID)
		if err == nil {
			return g.ID, nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return "", err
		}
		g, err = s.store.GetGroupBySlug(ctx, principalID)
		if err != nil {
			return "", err
		}
		return g.ID, nil
	default:
		return principalID, nil
	}
}

// ---------------------------------------------------------------------------
// PUT/DELETE /api/v1/projects/{id}/members/principals/{principalType}/{principalId}
// — atomic "set this principal's whole project role set" (ptone/scion#2529
// P1, design.md §3.1, §3.2).
// ---------------------------------------------------------------------------

// setMemberRolesRequestBody is the payload for
// PUT …/members/principals/{type}/{id}.
type setMemberRolesRequestBody struct {
	RoleDefinitionIDs         []string   `json:"roleDefinitionIds"`
	ExpectedRoleDefinitionIDs *[]string  `json:"expectedRoleDefinitionIds,omitempty"`
	NotBefore                 *time.Time `json:"notBefore,omitempty"`
	ExpiresAt                 *time.Time `json:"expiresAt,omitempty"`
}

// handleProjectMemberPrincipal dispatches PUT and DELETE for
// …/members/principals/{principalType}/{principalId}.
func (s *Server) handleProjectMemberPrincipal(w http.ResponseWriter, r *http.Request, projectID, principalType, principalID string) {
	switch r.Method {
	case http.MethodPut:
		s.putProjectMemberPrincipal(w, r, projectID, principalType, principalID)
	case http.MethodDelete:
		s.deleteProjectMemberPrincipal(w, r, projectID, principalType, principalID)
	default:
		MethodNotAllowed(w, http.MethodPut, http.MethodDelete)
	}
}

// validatePrincipalType checks principalType against the three types the
// members API supports, writing a 400 invalid_request and returning false
// if it is anything else.
func validatePrincipalType(w http.ResponseWriter, principalType string) bool {
	switch principalType {
	case store.RoleBindingPrincipalUser, store.RoleBindingPrincipalAgent, store.RoleBindingPrincipalGroup:
		return true
	default:
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "principalType must be \"user\", \"agent\", or \"group\"", nil)
		return false
	}
}

func (s *Server) putProjectMemberPrincipal(w http.ResponseWriter, r *http.Request, projectID, principalType, principalID string) {
	ctx := r.Context()

	// L3 (review r1): the credential-kind gate runs before resource
	// authorization, mirroring checkMembershipCredential's own position as
	// the FIRST check in SetMemberRoles — "you need an interactive user
	// session to mutate membership at all" is independent of, and prior to,
	// what permissions that credential happens to map to. Without this, an
	// agent token (which no permission in the registry maps project.manage
	// to, so it can never pass the authorize() call below anyway) would
	// still surface as a generic resource-authorization denial rather than
	// the credential_insufficient code design.md §12 P1 / acceptance 7 ask
	// for uniformly across UAT and agent credentials.
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return
	}
	user, ok := identity.(UserIdentity)
	if !ok {
		writeError(w, http.StatusForbidden, ErrCodeMembershipCredentialInsufficient, "membership mutations require an authenticated user identity", nil)
		return
	}

	if !s.authorize(w, r, Resource{Type: "project", ID: projectID}, ActionManage) {
		return
	}

	if !validatePrincipalType(w, principalType) {
		return
	}

	var body setMemberRolesRequestBody
	if err := readJSON(r, &body); err != nil {
		BadRequest(w, "invalid request body: "+err.Error())
		return
	}

	// Validate lifecycle fields (same rules as POST /members).
	if body.ExpiresAt != nil && !body.ExpiresAt.After(time.Now()) {
		BadRequest(w, "expiresAt must be in the future")
		return
	}
	if body.NotBefore != nil && body.ExpiresAt != nil && !body.ExpiresAt.After(*body.NotBefore) {
		BadRequest(w, "expiresAt must be after notBefore")
		return
	}

	resolvedPrincipalID, err := s.resolveMemberPrincipal(ctx, principalType, principalID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			if principalType == store.RoleBindingPrincipalUser {
				BadRequest(w, "user not found with email: "+principalID)
			} else {
				BadRequest(w, "group not found: "+principalID)
			}
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	if s.membershipService == nil {
		http.Error(w, "membership service not configured", http.StatusInternalServerError)
		return
	}

	result, decision := s.membershipService.SetMemberRoles(ctx, SetMemberRolesRequest{
		ProjectID:       projectID,
		PrincipalType:   principalType,
		PrincipalID:     resolvedPrincipalID,
		Actor:           user,
		DesiredRoleIDs:  body.RoleDefinitionIDs,
		ExpectedRoleIDs: body.ExpectedRoleDefinitionIDs,
		NotBefore:       body.NotBefore,
		ExpiresAt:       body.ExpiresAt,
	})
	if decision != nil && !decision.Allowed {
		slog.Info("project member set-roles denied",
			"project_id", projectID, "actor", user.Email(), "principal", principalType+":"+resolvedPrincipalID,
			"denial_code", decision.DenialCode, "reason", decision.Reason)
		writeError(w, decision.HTTPStatus, decision.DenialCode, decision.Reason, decision.Details)
		return
	}

	group := s.buildProjectMemberGroup(ctx, principalType, resolvedPrincipalID, result.After)
	group.Changed = result.Changed

	status := http.StatusOK
	if result.Created {
		status = http.StatusCreated
	}
	writeJSON(w, status, group)
}

func (s *Server) deleteProjectMemberPrincipal(w http.ResponseWriter, r *http.Request, projectID, principalType, principalID string) {
	ctx := r.Context()

	// L3 (review r1): see putProjectMemberPrincipal — credential-kind gate
	// before resource authorization.
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return
	}
	user, ok := identity.(UserIdentity)
	if !ok {
		writeError(w, http.StatusForbidden, ErrCodeMembershipCredentialInsufficient, "membership mutations require an authenticated user identity", nil)
		return
	}

	if !s.authorize(w, r, Resource{Type: "project", ID: projectID}, ActionManage) {
		return
	}

	if !validatePrincipalType(w, principalType) {
		return
	}

	resolvedPrincipalID, err := s.resolveMemberPrincipal(ctx, principalType, principalID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Member")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	if s.membershipService == nil {
		http.Error(w, "membership service not configured", http.StatusInternalServerError)
		return
	}

	_, decision := s.membershipService.SetMemberRoles(ctx, SetMemberRolesRequest{
		ProjectID:     projectID,
		PrincipalType: principalType,
		PrincipalID:   resolvedPrincipalID,
		Actor:         user,
		RemoveAll:     true,
	})
	if decision != nil && !decision.Allowed {
		slog.Info("project member remove-all denied",
			"project_id", projectID, "actor", user.Email(), "principal", principalType+":"+resolvedPrincipalID,
			"denial_code", decision.DenialCode, "reason", decision.Reason)
		writeError(w, decision.HTTPStatus, decision.DenialCode, decision.Reason, decision.Details)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// buildProjectMemberGroup assembles the projectMemberGroup response for the
// PUT/DELETE principal endpoints from the principal's post-state bindings.
// Bindings are ordered built-in first, then custom roles alphabetically by
// name (design.md §3.1).
func (s *Server) buildProjectMemberGroup(ctx context.Context, principalType, principalID string, bindings []*store.RoleBinding) *projectMemberGroup {
	group := &projectMemberGroup{
		PrincipalType: principalType,
		PrincipalID:   principalID,
	}
	group.PrincipalDisplayName = s.resolveGroupMemberDisplayName(ctx, principalType, principalID)

	infos := make([]projectMemberInfo, 0, len(bindings))
	for _, b := range bindings {
		if b == nil {
			continue
		}
		roleName, roleKind := "", roleKindCustom
		if rd, err := s.store.GetRoleDefinition(ctx, b.RoleDefinitionID); err == nil && rd != nil {
			roleName = rd.Name
			roleKind = projectRoleKind(rd.Name)
			if roleKind == roleKindBuiltIn {
				group.BuiltInRoleName = rd.Name
			}
		}
		info := projectMemberInfo{
			RoleBinding: *b,
			RoleName:    roleName,
			Source:      "direct",
			RoleKind:    roleKind,
		}
		info.PrincipalDisplayName = group.PrincipalDisplayName
		info.CreatedByDisplayName = s.resolveGroupMemberDisplayName(ctx, store.GroupMemberTypeUser, b.CreatedBy)
		infos = append(infos, info)
	}

	sort.Slice(infos, func(i, j int) bool {
		iBuiltIn := infos[i].RoleKind == roleKindBuiltIn
		jBuiltIn := infos[j].RoleKind == roleKindBuiltIn
		if iBuiltIn != jBuiltIn {
			return iBuiltIn
		}
		return infos[i].RoleName < infos[j].RoleName
	})

	group.Bindings = infos
	return group
}
