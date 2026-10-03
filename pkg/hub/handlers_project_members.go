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
	"github.com/google/uuid"
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
	// RoleKind is "builtin" or "custom". Additive field (ptone/scion#2529 P1);
	// every existing consumer of projectMemberInfo ignores unknown JSON
	// fields. No `omitempty` (review r1 L5): the field must appear on every
	// endpoint that returns a binding, and every construction site sets it
	// via projectRoleKind, so it is never the empty string in practice.
	RoleKind string `json:"roleKind"`
}

// projectMemberGroup is one principal's project membership: its built-in
// role (if any) plus every custom role it holds. It is the item type of
// GET …/members?groupBy=principal and, wrapped in
// projectMemberGroupMutationResponse, the body of PUT
// …/members/principals/{type}/{id} (ptone/scion#2529).
type projectMemberGroup struct {
	PrincipalType        string              `json:"principalType"`
	PrincipalID          string              `json:"principalId"`
	PrincipalDisplayName string              `json:"principalDisplayName,omitempty"`
	BuiltInRoleName      string              `json:"builtInRoleName"`
	Bindings             []projectMemberInfo `json:"bindings"`
}

// projectMemberGroupMutationResponse is the PUT …/members/principals/{type}/{id}
// response: the principal's post-state group plus whether anything changed.
// Changed lives here rather than on projectMemberGroup so the grouped GET
// does not carry a meaningless "changed":false on every row; the embedded
// struct flattens, so the PUT body is the same JSON object either way.
type projectMemberGroupMutationResponse struct {
	projectMemberGroup
	// Changed deliberately has no `omitempty`: the idempotent PUT response
	// must show `"changed":false` explicitly, not omit the field, so clients
	// can distinguish it from a response that never set it.
	Changed bool `json:"changed"`
}

// listProjectMemberGroupsResponse is the GET …/members?groupBy=principal
// response. TotalCount counts principals, not bindings, and limit/offset
// page over principals, so one principal's bindings never straddle a page.
type listProjectMemberGroupsResponse struct {
	Items        []projectMemberGroup    `json:"items"`
	TotalCount   int                     `json:"totalCount"`
	Capabilities *MembershipCapabilities `json:"_capabilities,omitempty"`
}

// memberListGroupByPrincipal is the only supported value of the members
// list's groupBy query parameter.
const memberListGroupByPrincipal = "principal"

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

	groupBy := r.URL.Query().Get("groupBy")
	if groupBy != "" && groupBy != memberListGroupByPrincipal {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, `groupBy must be "principal" when set`, nil)
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

	if groupBy == memberListGroupByPrincipal {
		s.writeProjectMemberGroups(w, r, projectID, bindings, limit, offset)
		return
	}

	totalCount := len(bindings)

	// Apply pagination.
	if limit <= 0 {
		limit = 100 // default
	}
	if offset > len(bindings) {
		offset = len(bindings)
	}
	// Written so a huge limit cannot overflow offset+limit (ptone/scion#2529,
	// review r2 L-OVF).
	end := len(bindings)
	if limit < len(bindings)-offset {
		end = offset + limit
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

	writeJSON(w, http.StatusOK, listProjectMembersResponse{
		Items:        items,
		TotalCount:   totalCount,
		Capabilities: s.memberListCapabilities(ctx, projectID),
	})
}

// memberListCapabilities returns the calling user's membership capabilities
// for the members list responses, or nil for a non-user identity.
//
// RS1: Server-derived operation/target capabilities replace C0 owner-only
// advisory capability. Capabilities are computed per the governance matrix.
func (s *Server) memberListCapabilities(ctx context.Context, projectID string) *MembershipCapabilities {
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		return nil
	}
	user, ok := identity.(UserIdentity)
	if !ok {
		return nil
	}
	// Capabilities are advisory: with no membership service (as every
	// sibling handler guards for) omit them rather than panic.
	if s.membershipService == nil {
		return nil
	}
	return s.membershipService.ComputeCapabilities(ctx, user.ID(), projectID)
}

// writeProjectMemberGroups writes GET …/members?groupBy=principal
// (ptone/scion#2529): one projectMemberGroup per principal holding any
// project-scope binding, paginated by principal.
//
// Order is deterministic so pages are stable: built-in tier (owner > admin >
// member > none), then display name (case-insensitive, then exact), then
// principal ID, then principal type. Within a group, bindings are built-in
// first, then custom roles by name. Every binding carries the same
// enrichment (roleName, source, display names, roleKind) as the flat list.
func (s *Server) writeProjectMemberGroups(w http.ResponseWriter, r *http.Request, projectID string, bindings []*store.RoleBinding, limit, offset int) {
	ctx := r.Context()

	type principalKey struct{ principalType, principalID string }

	enrich := newProjectMemberEnricher(s)
	var order []principalKey
	byPrincipal := make(map[principalKey][]*store.RoleBinding)
	for _, b := range bindings {
		if b == nil {
			continue
		}
		key := principalKey{b.PrincipalType, b.PrincipalID}
		if _, seen := byPrincipal[key]; !seen {
			order = append(order, key)
		}
		byPrincipal[key] = append(byPrincipal[key], b)
	}

	all := make([]projectMemberGroup, 0, len(order))
	for _, key := range order {
		all = append(all, *enrich.group(ctx, key.principalType, key.principalID, byPrincipal[key]))
	}
	sort.Slice(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if la, lb := projectRoleLevel(a.BuiltInRoleName), projectRoleLevel(b.BuiltInRoleName); la != lb {
			return la > lb
		}
		if la, lb := strings.ToLower(a.PrincipalDisplayName), strings.ToLower(b.PrincipalDisplayName); la != lb {
			return la < lb
		}
		if a.PrincipalDisplayName != b.PrincipalDisplayName {
			return a.PrincipalDisplayName < b.PrincipalDisplayName
		}
		if a.PrincipalID != b.PrincipalID {
			return a.PrincipalID < b.PrincipalID
		}
		return a.PrincipalType < b.PrincipalType
	})

	totalCount := len(all)
	if limit <= 0 {
		limit = 100 // default, same as the per-binding list
	}
	if offset > totalCount {
		offset = totalCount
	}
	// Written so a huge limit cannot overflow offset+limit.
	end := totalCount
	if limit < totalCount-offset {
		end = offset + limit
	}

	writeJSON(w, http.StatusOK, listProjectMemberGroupsResponse{
		Items:        all[offset:end],
		TotalCount:   totalCount,
		Capabilities: s.memberListCapabilities(ctx, projectID),
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
	// resolveMemberPrincipal so the PUT/DELETE principal endpoints share
	// this resolution logic with POST).
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
// (ptone/scion#2529 P1). Extracted from addProjectMember so
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
// P1).
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

// canonicalMemberPrincipalID returns the canonical spelling of a member
// principal address and whether it is well formed. A user ID or agent ID is
// returned as uuid.UUID.String() (lower-case, dashed), so the alternative
// spellings uuid.Parse also accepts (upper case, "urn:uuid:", braces, no
// dashes) address the same principal instead of storing a second,
// non-canonical principal_id that grants nothing, slips past the
// one-built-in check and cannot be removed by the canonical ID
// (ptone/scion#2529). A user email and any group address are returned
// unchanged; the resolver handles them.
func canonicalMemberPrincipalID(principalType, principalID string) (string, bool) {
	switch principalType {
	case store.RoleBindingPrincipalUser:
		if strings.Contains(principalID, "@") {
			return principalID, true
		}
		u, err := uuid.Parse(principalID)
		if err != nil {
			return "", false
		}
		return u.String(), true
	case store.RoleBindingPrincipalAgent:
		u, err := uuid.Parse(principalID)
		if err != nil {
			return "", false
		}
		return u.String(), true
	}
	return principalID, true
}

// validateMemberPrincipalAddress rejects a user principal addressed by
// something that is neither an email nor a well-formed user ID, and an agent
// principal addressed by anything but a well-formed agent ID, writing a 400
// invalid_request (the code P1 already uses for unresolvable principal
// addressing) and returning false. Without it the malformed ID reached the
// store, whose validation error surfaced as a 500 on PUT (ptone/scion#2529,
// review r2 L-500) and as "no bindings" 404 on DELETE. On success it returns
// the canonical address (see canonicalMemberPrincipalID), which the caller
// must use in place of the raw path segment.
func validateMemberPrincipalAddress(w http.ResponseWriter, principalType, principalID string) (string, bool) {
	canonical, ok := canonicalMemberPrincipalID(principalType, principalID)
	if ok {
		return canonical, true
	}
	if principalType == store.RoleBindingPrincipalAgent {
		BadRequest(w, "agent principal must be addressed by agent ID: "+principalID)
	} else {
		BadRequest(w, "user principal must be addressed by user ID or email: "+principalID)
	}
	return "", false
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
	// the credential_insufficient code the PUT/DELETE acceptance criteria
	// (ptone/scion#2529 acceptance 7) ask for uniformly across UAT and agent
	// credentials.
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
	principalID, ok = validateMemberPrincipalAddress(w, principalType, principalID)
	if !ok {
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

	group := newProjectMemberEnricher(s).group(ctx, principalType, resolvedPrincipalID, result.After)

	status := http.StatusOK
	if result.Created {
		status = http.StatusCreated
	}
	writeJSON(w, status, projectMemberGroupMutationResponse{projectMemberGroup: *group, Changed: result.Changed})
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
	principalID, ok = validateMemberPrincipalAddress(w, principalType, principalID)
	if !ok {
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

// projectMemberEnricher resolves the role names and display names a member
// binding is enriched with, caching each lookup for the life of one request.
type projectMemberEnricher struct {
	s            *Server
	roleNames    map[string]string // roleDefinitionID → roleName ("" if unresolved)
	displayNames map[string]string // principalType + ":" + principalID → display name
}

func newProjectMemberEnricher(s *Server) *projectMemberEnricher {
	return &projectMemberEnricher{s: s, roleNames: map[string]string{}, displayNames: map[string]string{}}
}

func (e *projectMemberEnricher) roleName(ctx context.Context, roleDefinitionID string) string {
	if name, ok := e.roleNames[roleDefinitionID]; ok {
		return name
	}
	name := ""
	if rd, err := e.s.store.GetRoleDefinition(ctx, roleDefinitionID); err == nil && rd != nil {
		name = rd.Name
	}
	e.roleNames[roleDefinitionID] = name
	return name
}

func (e *projectMemberEnricher) displayName(ctx context.Context, principalType, principalID string) string {
	k := principalType + ":" + principalID
	if name, ok := e.displayNames[k]; ok {
		return name
	}
	name := e.s.resolveGroupMemberDisplayName(ctx, principalType, principalID)
	e.displayNames[k] = name
	return name
}

// group assembles one principal's projectMemberGroup from its project-scope
// bindings. It is the single group builder for the grouped
// members GET and the PUT principal endpoint. Bindings are ordered built-in
// first, then custom roles alphabetically by name. A principal holds at most
// one built-in role per project (the D4 partial unique index); if legacy data
// ever carries more than one, the group reports the highest.
func (e *projectMemberEnricher) group(ctx context.Context, principalType, principalID string, bindings []*store.RoleBinding) *projectMemberGroup {
	group := &projectMemberGroup{
		PrincipalType:        principalType,
		PrincipalID:          principalID,
		PrincipalDisplayName: e.displayName(ctx, principalType, principalID),
	}

	infos := make([]projectMemberInfo, 0, len(bindings))
	for _, b := range bindings {
		if b == nil {
			continue
		}
		roleName := e.roleName(ctx, b.RoleDefinitionID)
		info := projectMemberInfo{
			RoleBinding:          *b,
			RoleName:             roleName,
			Source:               "direct",
			RoleKind:             projectRoleKind(roleName),
			PrincipalDisplayName: group.PrincipalDisplayName,
			CreatedByDisplayName: e.displayName(ctx, store.GroupMemberTypeUser, b.CreatedBy),
		}
		if info.RoleKind == roleKindBuiltIn && projectRoleLevel(roleName) > projectRoleLevel(group.BuiltInRoleName) {
			group.BuiltInRoleName = roleName
		}
		infos = append(infos, info)
	}

	sortProjectMemberBindings(infos)

	group.Bindings = infos
	return group
}

// sortProjectMemberBindings orders one principal's bindings built-in first,
// then custom roles alphabetically by name, with the binding ID as the final
// tie-break so the order is fully deterministic.
func sortProjectMemberBindings(infos []projectMemberInfo) {
	sort.SliceStable(infos, func(i, j int) bool {
		iBuiltIn := infos[i].RoleKind == roleKindBuiltIn
		jBuiltIn := infos[j].RoleKind == roleKindBuiltIn
		if iBuiltIn != jBuiltIn {
			return iBuiltIn
		}
		if infos[i].RoleName != infos[j].RoleName {
			return infos[i].RoleName < infos[j].RoleName
		}
		return infos[i].ID < infos[j].ID
	})
}

// ---------------------------------------------------------------------------
// GET /api/v1/projects/{id}/members/assignable-roles (ptone/scion#2529)
// ---------------------------------------------------------------------------

// listAssignableRolesResponse is the GET …/members/assignable-roles body.
type listAssignableRolesResponse struct {
	Items []AssignableProjectRole `json:"items"`
}

// handleProjectAssignableRoles lists the project-scoped roles (never system
// roles) with whether the calling user could grant each one through PUT
// …/members/principals/{type}/{id}. Read-only. Gated by project.manage, so
// a member without manage gets 403; the role catalogue is not exposed
// through hub role.read here.
func (s *Server) handleProjectAssignableRoles(w http.ResponseWriter, r *http.Request, projectID string) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}
	ctx := r.Context()

	// A non-user identity (an agent token) is refused before resource
	// authorization with the code and message the members PUT gives it, so
	// this view reports what the PUT would do for that caller.
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

	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Project")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	if s.membershipService == nil {
		http.Error(w, "membership service not configured", http.StatusInternalServerError)
		return
	}
	items, err := s.membershipService.AssignableRoles(ctx, user, projectID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	writeJSON(w, http.StatusOK, listAssignableRolesResponse{Items: items})
}
