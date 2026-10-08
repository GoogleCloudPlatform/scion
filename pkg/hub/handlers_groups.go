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
	"net/http"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ============================================================================
// Group Endpoints
//
// Phase 1E separation: GroupMembership.Role is now used ONLY for group
// governance (who can add/remove members, who can change group settings).
// It is NOT used for resource authorization decisions. Project membership
// and resource authorization are determined by role bindings (RoleBinding
// with scope_type="project"). See pkg/hub/authz.go:isProjectOwnerOrAdmin
// for the canonical project authorization check.
// ============================================================================

// ListGroupsResponse is the response for listing groups.
type ListGroupsResponse struct {
	Groups     []GroupWithCapabilities `json:"groups"`
	NextCursor string                  `json:"nextCursor,omitempty"`
	TotalCount int                     `json:"totalCount"`
	// TotalCountApproximate marks TotalCount as a lower bound rather than an
	// exact count (ptone/scion#1916 follow-up, C3) — see ListTemplatesResponse.
	TotalCountApproximate bool          `json:"totalCountApproximate,omitempty"`
	Capabilities          *Capabilities `json:"_capabilities,omitempty"`
}

// CreateGroupRequest is the request body for creating a group.
type CreateGroupRequest struct {
	Name        string            `json:"name"`
	Slug        string            `json:"slug,omitempty"`
	Description string            `json:"description,omitempty"`
	GroupType   string            `json:"groupType,omitempty"`
	ParentID    string            `json:"parentId,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	OwnerID     string            `json:"ownerId,omitempty"`
}

// UpdateGroupRequest is the request body for updating a group.
type UpdateGroupRequest struct {
	Name        string            `json:"name,omitempty"`
	Description string            `json:"description,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	OwnerID     string            `json:"ownerId,omitempty"`
}

// GroupMemberInfo is a group member enriched with human-friendly display info.
type GroupMemberInfo struct {
	store.GroupMember
	DisplayName string `json:"displayName,omitempty"`
}

// ListGroupMembersResponse is the response for listing group members.
type ListGroupMembersResponse struct {
	Members []GroupMemberInfo `json:"members"`
}

// AddGroupMemberRequest is the request body for adding a member to a group.
type AddGroupMemberRequest struct {
	MemberType string `json:"memberType"` // "user" or "group"
	MemberID   string `json:"memberId"`
	Role       string `json:"role"` // "member", "admin", "owner"
}

// handleGroups handles GET and POST on /api/v1/groups
func (s *Server) handleGroups(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listGroups(w, r)
	case http.MethodPost:
		s.createGroup(w, r)
	default:
		MethodNotAllowed(w, http.MethodGet, http.MethodPost)
	}
}

func (s *Server) listGroups(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	query := r.URL.Query()

	filter := store.GroupFilter{
		OwnerID:   query.Get("ownerId"),
		ParentID:  query.Get("parentId"),
		GroupType: query.Get("groupType"),
		ProjectID: query.Get("projectId"),
		Search:    query.Get("search"),
	}

	identity := GetIdentityFromContext(ctx)
	limit, err := parseAuthorizedListLimit(query.Get("limit"))
	if err != nil {
		BadRequest(w, err.Error())
		return
	}
	cursor := query.Get("cursor")
	cursorBinding := scopedCursorBinding("groups", filter, identity)
	// Opened (and re-sealed on the way out, below) once here for both
	// branches below -- the admin branch's direct store query and the
	// non-admin authorizedList scan -- so this endpoint's cursor format
	// never depends on which branch hasAdminView selects (see
	// listAuthorizedOrAll's doc comment for the same reasoning; groups
	// can't use that helper directly because of the three-way
	// admin/non-admin/unauthenticated split below).
	cursor, err = openAndValidateListCursor(s.listCursorSealer, cursor, cursorBinding)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	var groupItems []store.Group
	var nextCursor string
	var totalCount int
	var totalApprox bool
	// Check if user has admin-level list visibility via permission.
	hasAdminView := false
	if identity != nil {
		if user, ok := identity.(UserIdentity); ok {
			hasAdminView = s.authzService.Decide(ctx, AuthzRequest{
				Principal:  principalContextForIdentity(user),
				Credential: credentialContextForIdentity(user),
				Resource:   Resource{Type: "group", ID: "hub"},
				Action:     Action("list"),
				Permission: "group.list",
			}).Allowed
		}
	}
	if hasAdminView {
		// Admin view: direct store query without authorization filtering.
		result, err := s.store.ListGroups(ctx, filter, store.ListOptions{Limit: limit, Cursor: cursor, CursorBinding: cursorBinding})
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		groupItems, nextCursor, totalCount = result.Items, result.NextCursor, result.TotalCount
	} else if identity != nil {
		// Authenticated non-admin: use authorizedList for policy-enforced filtering.
		result, err := authorizedList(ctx, identity, cursor, limit, func(ctx context.Context, cursor string, limit int) (authorizedCandidatePage[store.Group], error) {
			page, err := s.store.ListGroups(ctx, filter, store.ListOptions{Limit: limit, Cursor: cursor, SkipTotalCount: true, CursorBinding: cursorBinding})
			if err != nil {
				return authorizedCandidatePage[store.Group]{}, err
			}
			return authorizedCandidatePage[store.Group]{Items: page.Items, NextCursor: page.NextCursor}, nil
		}, groupResource, func(g *store.Group) string { return authorizedListCursor(g.Created, g.ID, cursorBinding) }, s.authzService.AuthorizeReadBatch)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		groupItems, nextCursor, totalCount, totalApprox = result.Items, result.NextCursor, result.TotalCount, result.TotalCountApproximate
	} else {
		// Unauthenticated: return empty list (no identity to authorize against).
		groupItems = []store.Group{}
	}
	if nextCursor != "" {
		sealed, err := s.listCursorSealer.Seal(nextCursor, cursorBinding)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		nextCursor = sealed
	}
	groups := make([]GroupWithCapabilities, 0, len(groupItems))
	if identity == nil {
		for i := range groupItems {
			groups = append(groups, GroupWithCapabilities{Group: groupItems[i]})
		}
	} else {
		resources := make([]Resource, len(groupItems))
		for i := range groupItems {
			resources[i] = groupResource(&groupItems[i])
		}
		for i, cap := range s.authzService.ComputeCapabilitiesBatch(ctx, identity, resources, "group") {
			groups = append(groups, GroupWithCapabilities{Group: groupItems[i], Cap: cap})
		}
	}

	var scopeCap *Capabilities
	if identity != nil {
		scopeCap = s.authzService.ComputeScopeCapabilities(ctx, identity, "", "", "group")
	}
	writeJSON(w, http.StatusOK, ListGroupsResponse{
		Groups:                groups,
		NextCursor:            nextCursor,
		TotalCount:            totalCount,
		TotalCountApproximate: totalApprox,
		Capabilities:          scopeCap,
	})
}

func (s *Server) createGroup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req CreateGroupRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if req.Name == "" {
		ValidationError(w, "name is required", nil)
		return
	}

	if !s.authorize(w, r, Resource{Type: "group"}, ActionCreate) {
		return
	}

	// Validate and default GroupType
	groupType := req.GroupType
	if groupType == "" {
		groupType = store.GroupTypeExplicit
	}
	if groupType != store.GroupTypeExplicit && groupType != store.GroupTypeProjectAgents {
		ValidationError(w, "groupType must be 'explicit' or 'project_agents'", nil)
		return
	}
	if groupType == store.GroupTypeProjectAgents {
		ValidationError(w, "project_agents groups are system-managed and cannot be created via API", nil)
		return
	}

	slug := req.Slug
	if slug == "" {
		slug = api.Slugify(req.Name)
	}
	if strings.HasPrefix(slug, "project:") {
		ValidationError(w, "the 'project:' slug prefix is reserved for system use", nil)
		return
	}

	// The project members group marker keys are system-written only
	// (ptone/scion#2599): createProjectMembersGroup and the entadapter set
	// them directly through the store, never through this handler. This
	// guard is hygiene, not a fix for a reachable state: CreateGroupRequest
	// has no ProjectID, and every marker consumer requires one, so a
	// POST-created group is never treated as a members group. Rejecting the
	// keys here, as updateGroup does on PATCH, keeps "markers are
	// system-written only" true on every group API path, and stops a stray
	// marker from becoming meaningful if a group ever gains a ProjectID.
	if setsProjectMembersGroupMarkerKey(nil, req.Annotations) {
		ValidationError(w, "project members group marker annotations are system-written and cannot be added", nil)
		return
	}

	groupID := api.NewUUID()

	// A group created under a parent becomes a member of that parent and
	// inherits the parent's role bindings, so the caller needs the same
	// authority on the parent as adding a group member to it would require,
	// and the new membership counts toward the parent's member limit. Both
	// checks run before anything is created.
	var parentEdge *store.GroupMember
	var canDelegateResult, canDelegateReason string
	if req.ParentID != "" {
		parent, err := s.store.GetGroup(ctx, req.ParentID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				ValidationError(w, "parent group not found: "+req.ParentID, nil)
				return
			}
			writeErrorFromErr(w, err, "")
			return
		}
		var ok bool
		canDelegateResult, canDelegateReason, ok = s.authorizeChildGroupGrant(w, r, parent)
		if !ok {
			return
		}
		parentEdge = &store.GroupMember{
			GroupID:    parent.ID,
			MemberType: store.GroupMemberTypeGroup,
			MemberID:   groupID,
			Role:       store.GroupMemberRoleMember,
		}
		if !s.reserveGroupMemberSlot(w, ctx, parentEdge) {
			return
		}
	}

	ownerID := req.OwnerID
	createdBy := ""
	if identity := GetIdentityFromContext(ctx); identity != nil {
		createdBy = identity.ID()
		if ownerID == "" {
			ownerID = identity.ID()
		}
	}

	group := &store.Group{
		ID:          groupID,
		Name:        req.Name,
		Slug:        slug,
		Description: req.Description,
		GroupType:   groupType,
		ParentID:    req.ParentID,
		Labels:      req.Labels,
		Annotations: req.Annotations,
		OwnerID:     ownerID,
		CreatedBy:   createdBy,
	}

	if err := s.store.CreateGroup(ctx, group); err != nil {
		if parentEdge != nil {
			s.releaseGroupMemberSlot(ctx, parentEdge.GroupID, parentEdge.MemberType, parentEdge.MemberID)
		}
		if err == store.ErrAlreadyExists {
			Conflict(w, "Group with this slug already exists")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	if parentEdge != nil {
		s.auditGroupMemberAdd(ctx, parentEdge, canDelegateResult, canDelegateReason)
	}

	s.groupsLogger().Info("group created",
		"group_id", group.ID,
		"slug", group.Slug,
		"group_type", group.GroupType,
		"created_by", createdBy)

	// Add the creating user as an owner of the new group
	if createdBy != "" {
		if err := s.store.AddGroupMember(ctx, &store.GroupMember{
			GroupID:    group.ID,
			MemberType: store.GroupMemberTypeUser,
			MemberID:   createdBy,
			Role:       store.GroupMemberRoleOwner,
		}); err != nil && err != store.ErrAlreadyExists {
			// Log but don't fail the group creation
			s.groupsLogger().Warn("failed to add creator as owner of new group",
				"group", group.ID, "user", createdBy, "error", err)
		}
	}

	writeJSON(w, http.StatusCreated, group)
}

// handleGroupRoutes handles /api/v1/groups/{groupId}/...
func (s *Server) handleGroupRoutes(w http.ResponseWriter, r *http.Request) {
	// Extract group ID and remaining path
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/groups/")
	if path == "" {
		NotFound(w, "Group")
		return
	}

	// Parse the group ID
	parts := strings.SplitN(path, "/", 2)
	groupID := parts[0]
	subPath := ""
	if len(parts) > 1 {
		subPath = parts[1]
	}

	// Check for nested /members path
	if strings.HasPrefix(subPath, "members") {
		memberPath := strings.TrimPrefix(subPath, "members")
		memberPath = strings.TrimPrefix(memberPath, "/")
		if memberPath == "" {
			s.handleGroupMembers(w, r, groupID)
		} else {
			s.handleGroupMemberByID(w, r, groupID, memberPath)
		}
		return
	}

	// Otherwise handle as group resource
	if subPath != "" {
		NotFound(w, "Group resource")
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.getGroup(w, r, groupID)
	case http.MethodPatch:
		s.updateGroup(w, r, groupID)
	case http.MethodDelete:
		s.deleteGroup(w, r, groupID)
	default:
		MethodNotAllowed(w, http.MethodGet, http.MethodPatch, http.MethodDelete)
	}
}

func (s *Server) getGroup(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	group, err := s.store.GetGroup(ctx, id)
	if err != nil {
		if err == store.ErrNotFound {
			// Try by slug
			group, err = s.store.GetGroupBySlug(ctx, id)
			if err != nil {
				writeErrorFromErr(w, err, "")
				return
			}
		} else {
			writeErrorFromErr(w, err, "")
			return
		}
	}

	resp := GroupWithCapabilities{Group: *group}
	if identity := GetIdentityFromContext(ctx); identity != nil {
		resp.Cap = s.authzService.ComputeCapabilities(ctx, identity, groupResource(group))
	}

	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) updateGroup(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	group, err := s.store.GetGroup(ctx, id)
	if err != nil {
		if err == store.ErrNotFound {
			group, err = s.store.GetGroupBySlug(ctx, id)
			if err != nil {
				writeErrorFromErr(w, err, "")
				return
			}
		} else {
			writeErrorFromErr(w, err, "")
			return
		}
	}

	// Enforce authorization: only group owner or admins can update
	if !s.authorize(w, r, groupResource(group), ActionUpdate) {
		return
	}
	if !s.requireSessionForRoleBoundGroup(w, r, group) {
		return
	}

	var req UpdateGroupRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	// A project members group carries no owner (ptone/scion#2599): the
	// owner relationship would grant group.* outside the project's role
	// bindings. Reject setting one, even by a hub admin. Both the stored
	// group and the patched annotations are checked. A PATCH that adds the
	// marker and an owner together is also rejected by the add-marker guard
	// below; checking the patched annotations here is defence in depth.
	if req.OwnerID != "" {
		patched := *group
		if req.Annotations != nil {
			patched.Annotations = req.Annotations
		}
		if hasProjectMembersGroupMarker(group) || hasProjectMembersGroupMarker(&patched) {
			ValidationError(w, "ownerId cannot be set on a project members group", nil)
			return
		}
	}

	// The marker annotations identify a project members group to the owner
	// guard above and to the owner-clearing startup backfill. req.Annotations
	// replaces the whole map, so without this check a PATCH could strip the
	// marker and a later PATCH could then set an owner that no guard or
	// backfill would catch. Reject any PATCH that removes or changes either
	// marker key on a marked group (ptone/scion#2599).
	//
	// The markers are system-written only: createProjectMembersGroup and the
	// entadapter set them directly through the store, never through the
	// group API. createGroup rejects them on POST, and a PATCH may not add
	// either marker key to an unmarked group either. On a group that has a
	// ProjectID, a mistaken request adding a marker would otherwise become
	// irreversible through the API once the immutability check above
	// applied to it.
	if req.Annotations != nil && changesProjectMembersGroupMarker(group.Annotations, req.Annotations) {
		if hasProjectMembersGroupMarker(group) {
			ValidationError(w, "project members group marker annotations cannot be removed or changed", nil)
			return
		}
		if setsProjectMembersGroupMarkerKey(group.Annotations, req.Annotations) {
			ValidationError(w, "project members group marker annotations are system-written and cannot be added", nil)
			return
		}
	}

	if req.Name != "" {
		group.Name = req.Name
	}
	if req.Description != "" {
		group.Description = req.Description
	}
	if req.Labels != nil {
		group.Labels = req.Labels
	}
	if req.Annotations != nil {
		group.Annotations = req.Annotations
	}
	if req.OwnerID != "" {
		group.OwnerID = req.OwnerID
	}

	if err := s.store.UpdateGroup(ctx, group); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	s.groupsLogger().Info("group updated",
		"group_id", group.ID,
		"slug", group.Slug)

	writeJSON(w, http.StatusOK, group)
}

func (s *Server) deleteGroup(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	// Try to get the group first (by ID or slug)
	group, err := s.store.GetGroup(ctx, id)
	if err != nil {
		if err == store.ErrNotFound {
			group, err = s.store.GetGroupBySlug(ctx, id)
			if err != nil {
				writeErrorFromErr(w, err, "")
				return
			}
		} else {
			writeErrorFromErr(w, err, "")
			return
		}
	}

	// Enforce authorization: only group owner or admins can delete
	if !s.authorize(w, r, groupResource(group), ActionDelete) {
		return
	}

	// Constraint-coverage gate (R5): if this group participates in any
	// AccessConstraint, deleting it silently removes the constraint's subject.
	// Require access_constraint.admin permission to proceed.
	if !s.requireConstraintAdminForGroup(w, r, group.ID) {
		return
	}

	if group.GroupType == store.GroupTypeProjectAgents {
		BadRequest(w, "project_agents groups are system-managed and cannot be deleted via API")
		return
	}
	if !s.requireSessionForRoleBoundGroup(w, r, group) {
		return
	}

	if err := s.store.DeleteGroup(ctx, group.ID); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	s.publishConduitAuthzChanged(conduitAuthzMatch{})
	s.groupsLogger().Info("group deleted",
		"group_id", group.ID,
		"slug", group.Slug)

	w.WriteHeader(http.StatusNoContent)
}

// handleGroupMembers handles GET and POST on /api/v1/groups/{groupId}/members
func (s *Server) handleGroupMembers(w http.ResponseWriter, r *http.Request, groupID string) {
	ctx := r.Context()

	// Verify group exists (by ID or slug)
	group, err := s.store.GetGroup(ctx, groupID)
	if err != nil {
		if err == store.ErrNotFound {
			group, err = s.store.GetGroupBySlug(ctx, groupID)
			if err != nil {
				writeErrorFromErr(w, err, "")
				return
			}
		} else {
			writeErrorFromErr(w, err, "")
			return
		}
	}

	switch r.Method {
	case http.MethodGet:
		s.listGroupMembers(w, r, group.ID)
	case http.MethodPost:
		s.addGroupMember(w, r, group)
	default:
		MethodNotAllowed(w, http.MethodGet, http.MethodPost)
	}
}

func (s *Server) listGroupMembers(w http.ResponseWriter, r *http.Request, groupID string) {
	ctx := r.Context()

	members, err := s.store.GetGroupMembers(ctx, groupID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Enrich members with human-friendly display names
	enriched := make([]GroupMemberInfo, len(members))
	for i, m := range members {
		enriched[i] = GroupMemberInfo{GroupMember: m}
		enriched[i].DisplayName = s.resolveGroupMemberDisplayName(ctx, m.MemberType, m.MemberID)
	}

	writeJSON(w, http.StatusOK, ListGroupMembersResponse{
		Members: enriched,
	})
}

// resolveGroupMemberDisplayName looks up a human-friendly name for a group member.
func (s *Server) resolveGroupMemberDisplayName(ctx context.Context, memberType, memberID string) string {
	switch memberType {
	case store.GroupMemberTypeUser:
		user, err := s.store.GetUser(ctx, memberID)
		if err != nil {
			return ""
		}
		if user.DisplayName != "" {
			return user.DisplayName
		}
		return user.Email
	case store.GroupMemberTypeGroup:
		group, err := s.store.GetGroup(ctx, memberID)
		if err != nil {
			return ""
		}
		return group.Name
	case store.GroupMemberTypeAgent:
		agent, err := s.store.GetAgent(ctx, memberID)
		if err != nil {
			return ""
		}
		return agent.Name
	}
	return ""
}

func (s *Server) addGroupMember(w http.ResponseWriter, r *http.Request, group *store.Group) {
	ctx := r.Context()
	groupID := group.ID

	var req AddGroupMemberRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if req.MemberType == "" {
		ValidationError(w, "memberType is required", nil)
		return
	}
	if req.MemberType != store.GroupMemberTypeUser && req.MemberType != store.GroupMemberTypeGroup && req.MemberType != store.GroupMemberTypeAgent {
		ValidationError(w, "memberType must be 'user', 'group', or 'agent'", nil)
		return
	}
	if req.MemberID == "" {
		ValidationError(w, "memberId is required", nil)
		return
	}
	if req.Role == "" {
		req.Role = store.GroupMemberRoleMember
	}
	if req.Role != store.GroupMemberRoleMember && req.Role != store.GroupMemberRoleAdmin && req.Role != store.GroupMemberRoleOwner {
		ValidationError(w, "role must be 'member', 'admin', or 'owner'", nil)
		return
	}

	// Authorization on the group: addMember, the role hierarchy, and the
	// delegation check (see authorizeGroupMemberGrant).
	canDelegateResult, canDelegateReason, ok := s.authorizeGroupMemberGrant(w, r, group, req.Role)
	if !ok {
		return
	}
	if !s.requireSessionForRoleBoundGroup(w, r, group) {
		return
	}

	// Resolve the member ID from human-friendly identifiers.
	// For users: accept email addresses in addition to UUIDs.
	// For groups: accept slugs in addition to UUIDs.
	resolvedID := req.MemberID
	switch req.MemberType {
	case store.GroupMemberTypeUser:
		// If it looks like an email address, resolve it
		if strings.Contains(req.MemberID, "@") {
			user, err := s.store.GetUserByEmail(ctx, req.MemberID)
			if err != nil {
				if err == store.ErrNotFound {
					ValidationError(w, "user not found with email: "+req.MemberID, nil)
					return
				}
				writeErrorFromErr(w, err, "")
				return
			}
			resolvedID = user.ID
		} else {
			// Verify the user ID exists
			if _, err := s.store.GetUser(ctx, req.MemberID); err != nil {
				if err == store.ErrNotFound {
					ValidationError(w, "user not found: "+req.MemberID, nil)
					return
				}
				writeErrorFromErr(w, err, "")
				return
			}
		}
	case store.GroupMemberTypeGroup:
		// Try as ID first, then as slug
		memberGroup, err := s.store.GetGroup(ctx, req.MemberID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				var slugErr error
				memberGroup, slugErr = s.store.GetGroupBySlug(ctx, req.MemberID)
				if slugErr != nil {
					if errors.Is(slugErr, store.ErrNotFound) {
						ValidationError(w, "group not found: "+req.MemberID, nil)
						return
					}
					writeErrorFromErr(w, slugErr, "")
					return
				}
				resolvedID = memberGroup.ID
			} else {
				writeErrorFromErr(w, err, "")
				return
			}
		}
		// Project members groups are system-managed and cannot be nested
		// as a child of another group.
		if store.IsProjectMembersGroup(memberGroup) {
			ValidationError(w, projectMembersGroupPrincipalMessage,
				projectMembersGroupPrincipalDetails(memberGroup.ID))
			return
		}
	case store.GroupMemberTypeAgent:
		if _, err := s.store.GetAgent(ctx, req.MemberID); err != nil {
			if err == store.ErrNotFound {
				ValidationError(w, "agent not found: "+req.MemberID, nil)
				return
			}
			writeErrorFromErr(w, err, "")
			return
		}
	}

	// Check for cycles when adding a group as a member
	if req.MemberType == store.GroupMemberTypeGroup {
		wouldCycle, err := s.store.WouldCreateCycle(ctx, groupID, resolvedID)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		if wouldCycle {
			BadRequest(w, "Adding this group would create a cycle in the group hierarchy")
			return
		}
	}

	member := &store.GroupMember{
		GroupID:    groupID,
		MemberType: req.MemberType,
		MemberID:   resolvedID,
		Role:       req.Role,
	}

	// Set AddedBy from auth context
	if identity := GetIdentityFromContext(ctx); identity != nil {
		member.AddedBy = identity.ID()
	}

	// Quota enforcement: check members-per-group limit before addition.
	if !s.reserveGroupMemberSlot(w, ctx, member) {
		return
	}

	if err := s.store.AddGroupMember(ctx, member); err != nil {
		s.releaseGroupMemberSlot(ctx, member.GroupID, member.MemberType, member.MemberID)
		if err == store.ErrAlreadyExists {
			Conflict(w, "Member already exists in this group")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	s.auditGroupMemberAdd(ctx, member, canDelegateResult, canDelegateReason)

	s.groupsLogger().Info("group member added",
		"group_id", groupID,
		"member_type", member.MemberType,
		"member_id", member.MemberID,
		"role", member.Role)

	// Return enriched response with display name
	resp := GroupMemberInfo{
		GroupMember: *member,
		DisplayName: s.resolveGroupMemberDisplayName(ctx, member.MemberType, member.MemberID),
	}
	writeJSON(w, http.StatusCreated, resp)
}

// groupMembershipQuotaID is the max_members_per_group reservation resource ID
// for one membership of a group.
func groupMembershipQuotaID(groupID, memberType, memberID string) string {
	return fmt.Sprintf("%s:%s:%s", groupID, memberType, memberID)
}

// reserveGroupMemberSlot reserves a max_members_per_group slot on
// member.GroupID for member, writing the refusal response and returning false
// when the group is at its limit or the check fails. Callers release the slot
// with releaseGroupMemberSlot if the membership is then not created.
func (s *Server) reserveGroupMemberSlot(w http.ResponseWriter, ctx context.Context, member *store.GroupMember) bool {
	if s.quotaService == nil {
		return true
	}
	membershipID := groupMembershipQuotaID(member.GroupID, member.MemberType, member.MemberID)
	err := s.quotaService.CheckAndReserve(ctx, store.LimitMaxMembersPerGroup, member.GroupID, "group", member.GroupID, membershipID)
	if err == nil {
		return true
	}
	if errors.Is(err, store.ErrQuotaExceeded) {
		writeError(w, http.StatusTooManyRequests, ErrCodeQuotaExceeded,
			"quota exceeded: max_members_per_group", nil)
		return false
	}
	if errors.Is(err, ErrQuotaLockContention) {
		writeError(w, http.StatusTooManyRequests, ErrCodeQuotaExceeded,
			"quota check temporarily unavailable, please retry", nil)
		return false
	}
	writeError(w, http.StatusInternalServerError, ErrCodeRuntimeError, "quota check failed", nil)
	return false
}

// releaseGroupMemberSlot releases the max_members_per_group reservation for
// one membership of a group (best-effort).
func (s *Server) releaseGroupMemberSlot(ctx context.Context, groupID, memberType, memberID string) {
	if s.quotaService == nil {
		return
	}
	s.quotaService.Release(ctx, store.LimitMaxMembersPerGroup, groupMembershipQuotaID(groupID, memberType, memberID))
}

// auditGroupMemberAdd writes the group_member_add mutation audit record for a
// membership that was just created, with the CanDelegate result and reason
// returned by authorizeGroupMemberGrant.
func (s *Server) auditGroupMemberAdd(ctx context.Context, member *store.GroupMember, canDelegateResult, canDelegateReason string) {
	s.emitMutationAudit(ctx, &store.MutationAuditRecord{
		MutationType:      "group_member_add",
		TargetType:        "group_membership",
		TargetID:          member.GroupID,
		AfterSummary:      `{"groupId":"` + member.GroupID + `","memberType":"` + member.MemberType + `","memberId":"` + member.MemberID + `","role":"` + member.Role + `"}`,
		CanDelegateResult: canDelegateResult,
		CanDelegateReason: canDelegateReason,
	})
}

// authorizeGroupMemberGrant runs the authorization for adding a member with
// the given role to group, writing the refusal response and returning ok=false
// when the caller may not. It is shared by addGroupMember and by createGroup
// when a new group is created under a parent group (the new group becomes a
// member of the parent, so it needs the same authority on the parent):
//
//  1. group.addMember on the group;
//  2. the role hierarchy within the group;
//  3. CanDelegate for a membership of the group.
//
// On success it returns the CanDelegate result and reason for the audit
// record.
func (s *Server) authorizeGroupMemberGrant(w http.ResponseWriter, r *http.Request, group *store.Group, role string) (canDelegateResult, canDelegateReason string, ok bool) {
	ctx := r.Context()

	// Only group owners, group admins or callers granted group.addMember
	// may add members.
	if !s.authorize(w, r, groupResource(group), ActionAddMember) {
		return "", "", false
	}

	// Enforce role-hierarchy: only owners can add owners/admins; admins can only add members.
	// Platform admins and group resource owners are exempt from the role-hierarchy check.
	//
	// The hierarchy is defined over user membership in the group, so it cannot be
	// evaluated for an agent or broker caller — which is why an earlier form of
	// this guard let those callers grant any role at all. Such a caller reaches
	// this point only through an explicit addMember policy, and is held to adding
	// plain members: making someone an admin or owner requires a caller whose
	// own standing in the group can be checked.
	userIdent, isUserCaller := GetIdentityFromContext(ctx).(UserIdentity)
	if !isUserCaller {
		if role != store.GroupMemberRoleMember {
			writeError(w, http.StatusForbidden, ErrCodeForbidden,
				"Only group owners can add owners or admins", nil)
			return "", "", false
		}
	} else {
		isResourceOwner := group.OwnerID != "" && group.OwnerID == userIdent.ID()
		isPlatformAdmin := s.authzService.Decide(ctx, AuthzRequest{
			Principal:  principalContextForIdentity(userIdent),
			Credential: credentialContextForIdentity(userIdent),
			Resource:   Resource{Type: "group", ID: "hub"},
			Action:     Action("update"),
			Permission: "group.update",
		}).Allowed
		if !isResourceOwner && !isPlatformAdmin {
			callerMembership, err := s.store.GetGroupMembership(ctx, group.ID, store.GroupMemberTypeUser, userIdent.ID())
			// Not being a member is a refusal below; any other store error
			// is reported as such rather than as a 403.
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				writeErrorFromErr(w, err, "")
				return "", "", false
			}
			switch role {
			case store.GroupMemberRoleOwner, store.GroupMemberRoleAdmin:
				if err != nil || callerMembership.Role != store.GroupMemberRoleOwner {
					writeError(w, http.StatusForbidden, ErrCodeForbidden,
						"Only group owners can add owners or admins", nil)
					return "", "", false
				}
			case store.GroupMemberRoleMember:
				if err != nil {
					writeError(w, http.StatusForbidden, ErrCodeForbidden,
						"Only group owners or admins can add members", nil)
					return "", "", false
				}
				if callerMembership.Role != store.GroupMemberRoleOwner && callerMembership.Role != store.GroupMemberRoleAdmin {
					writeError(w, http.StatusForbidden, ErrCodeForbidden,
						"Only group owners or admins can add members", nil)
					return "", "", false
				}
			}
		}
	}

	// CanDelegate check: ensure the actor has sufficient authority to grant
	// the membership. Because membership in a role-bearing group confers all
	// of that group's role-binding authority, the actor must hold every
	// permission that becomes newly reachable.
	//
	// This applies to ALL callers (user AND agent) and ALL member types
	// (user, group, agent). An agent with group.addMember authority must
	// pass the same delegation test as a user caller — group governance role
	// does NOT substitute for resource authority.
	if s.authzService != nil {
		actorIdentity := GetIdentityFromContext(ctx)
		if actorIdentity != nil {
			grantDesc := GrantDescriptor{
				Type:    GrantTypeGroupMembership,
				GroupID: group.ID,
			}
			delegateDecision := s.authzService.CanDelegate(ctx, actorIdentity, grantDesc)
			canDelegateResult = "allow"
			canDelegateReason = delegateDecision.Reason
			if !delegateDecision.Allowed {
				logAuthzDenial(r, actorIdentity, groupResource(group), ActionAddMember,
					"CanDelegate denied: "+delegateDecision.Reason)
				writeForbidden(w, "Cannot grant authority you do not hold: "+delegateDecision.Reason)
				return "", "", false
			}
		}
	}

	return canDelegateResult, canDelegateReason, true
}

// authorizeChildGroupGrant runs the authorization for creating a group under
// parent. The new group becomes a member of parent, so the caller needs the
// authority authorizeGroupMemberGrant requires for a plain member, and the
// group rule of requireSessionForRoleBoundGroup applies to parent. It writes
// the refusal response and returns ok=false when the caller may not.
func (s *Server) authorizeChildGroupGrant(w http.ResponseWriter, r *http.Request, parent *store.Group) (canDelegateResult, canDelegateReason string, ok bool) {
	canDelegateResult, canDelegateReason, ok = s.authorizeGroupMemberGrant(w, r, parent, store.GroupMemberRoleMember)
	if !ok {
		return "", "", false
	}
	if !s.requireSessionForRoleBoundGroup(w, r, parent) {
		return "", "", false
	}
	return canDelegateResult, canDelegateReason, true
}

// handleGroupMemberByID handles DELETE on /api/v1/groups/{groupId}/members/{type}/{id}
func (s *Server) handleGroupMemberByID(w http.ResponseWriter, r *http.Request, groupID, memberPath string) {
	ctx := r.Context()

	// Parse memberPath as "type/id"
	parts := strings.SplitN(memberPath, "/", 2)
	if len(parts) != 2 {
		NotFound(w, "Member")
		return
	}
	memberType := parts[0]
	memberID := parts[1]

	if memberType != store.GroupMemberTypeUser && memberType != store.GroupMemberTypeGroup && memberType != store.GroupMemberTypeAgent {
		NotFound(w, "Member")
		return
	}

	// Verify group exists (by ID or slug)
	group, err := s.store.GetGroup(ctx, groupID)
	if err != nil {
		if err == store.ErrNotFound {
			group, err = s.store.GetGroupBySlug(ctx, groupID)
			if err != nil {
				writeErrorFromErr(w, err, "")
				return
			}
		} else {
			writeErrorFromErr(w, err, "")
			return
		}
	}

	switch r.Method {
	case http.MethodGet:
		s.getGroupMember(w, r, group.ID, memberType, memberID)
	case http.MethodDelete:
		s.removeGroupMember(w, r, group, memberType, memberID)
	default:
		MethodNotAllowed(w, http.MethodGet, http.MethodDelete)
	}
}

func (s *Server) getGroupMember(w http.ResponseWriter, r *http.Request, groupID, memberType, memberID string) {
	ctx := r.Context()

	member, err := s.store.GetGroupMembership(ctx, groupID, memberType, memberID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, member)
}

func (s *Server) removeGroupMember(w http.ResponseWriter, r *http.Request, group *store.Group, memberType, memberID string) {
	ctx := r.Context()

	// Enforce authorization: only group owner or admins can remove members
	if !s.authorize(w, r, groupResource(group), ActionRemoveMember) {
		return
	}
	if !s.requireSessionForRoleBoundGroup(w, r, group) {
		return
	}

	// Constraint-coverage gate (R5): if this group participates in any
	// AccessConstraint, removing a member silently relaxes that constraint.
	// Require access_constraint.admin permission to proceed.
	if !s.requireConstraintAdminForGroup(w, r, group.ID) {
		return
	}

	// Prevent removing the last owner of a group
	if memberType == store.GroupMemberTypeUser {
		membership, err := s.store.GetGroupMembership(ctx, group.ID, memberType, memberID)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		if membership.Role == store.GroupMemberRoleOwner {
			ownerCount, err := s.store.CountGroupMembersByRole(ctx, group.ID, store.GroupMemberRoleOwner)
			if err != nil {
				writeErrorFromErr(w, err, "")
				return
			}
			if ownerCount <= 1 {
				BadRequest(w, "Cannot remove the last owner of a group")
				return
			}
		}
	}

	if err := s.store.RemoveGroupMember(ctx, group.ID, memberType, memberID); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Release quota reservation for the removed member (best-effort).
	s.releaseGroupMemberSlot(ctx, group.ID, memberType, memberID)

	s.emitMutationAudit(r.Context(), &store.MutationAuditRecord{
		MutationType:  "group_member_remove",
		TargetType:    "group_membership",
		TargetID:      group.ID,
		BeforeSummary: `{"groupId":"` + group.ID + `","memberType":"` + memberType + `","memberId":"` + memberID + `"}`,
	})

	if memberType == store.GroupMemberTypeUser {
		s.publishConduitAuthzChanged(conduitAuthzMatch{UserID: memberID})
	} else {
		s.publishConduitAuthzChanged(conduitAuthzMatch{})
	}
	s.groupsLogger().Info("group member removed",
		"group_id", group.ID,
		"member_type", memberType,
		"member_id", memberID)

	w.WriteHeader(http.StatusNoContent)
}

// groupClosureHasRoleBinding reports whether the group, or any group that
// transitively contains it, is the principal of a role binding of any
// scope. A member of the group holds the authority of every such binding.
func (s *Server) groupClosureHasRoleBinding(ctx context.Context, groupID string) (bool, error) {
	principals := []store.PrincipalRef{{Type: store.RoleBindingPrincipalGroup, ID: groupID}}
	parents, err := s.store.GetParentGroups(ctx, groupID)
	if err != nil {
		return false, err
	}
	for _, pid := range parents {
		principals = append(principals, store.PrincipalRef{Type: store.RoleBindingPrincipalGroup, ID: pid})
	}
	bindings, err := s.store.ListRoleBindingsForPrincipals(ctx, principals, nil, nil)
	if err != nil {
		return false, err
	}
	return len(bindings) > 0, nil
}

// requireSessionForRoleBoundGroup refuses a user access token on a change
// to a group whose closure carries a role binding: updating or deleting the
// group, adding or removing a member, or creating a child group under it.
// Such a change moves role-binding authority, so it is session-only with the
// GOV_PENDING reason (session_only_gate.go). A group with no role binding in
// its closure, and every other credential, proceed. It runs after the group
// authorization check, and in deleteGroup after the system-managed and
// constraint checks. Returns false when the response has been written.
func (s *Server) requireSessionForRoleBoundGroup(w http.ResponseWriter, r *http.Request, group *store.Group) bool {
	if !IsScopedUserIdentity(GetIdentityFromContext(r.Context())) {
		return true
	}
	bound, err := s.groupClosureHasRoleBinding(r.Context(), group.ID)
	if err != nil {
		// Fail closed: the closure cannot be resolved.
		writeError(w, http.StatusInternalServerError, ErrCodeRuntimeError,
			"failed to check role bindings for group", nil)
		return false
	}
	if bound {
		writeSessionOnlyDenial(w, ErrCodeForbidden,
			"changing a group that carries a role binding requires an interactive session",
			authzop.ReasonGovernancePending)
		return false
	}
	return true
}

// isConstraintBearingGroup checks whether the given group ID appears as a
// subject in any AccessConstraint. A group is constraint-bearing if any
// constraint targets it via a "principal" subject with type "group" or via
// a "group_closure" subject. Modifying membership of or deleting a
// constraint-bearing group requires access_constraint.admin permission.
func (s *Server) isConstraintBearingGroup(ctx context.Context, groupID string) (bool, error) {
	// R-1 fix: page through all constraints instead of using (0,0) which
	// defaults to limit=100, silently missing constraint #101+.
	const pageSize = 500
	offset := 0
	for {
		constraints, err := s.store.ListAccessConstraints(ctx, pageSize, offset)
		if err != nil {
			return false, err
		}
		for _, c := range constraints {
			if c.SubjectKind == store.ConstraintSubjectPrincipal &&
				c.SubjectPrincipalType != nil && *c.SubjectPrincipalType == "group" &&
				c.SubjectPrincipalID != nil && *c.SubjectPrincipalID == groupID {
				return true, nil
			}
			if c.SubjectKind == store.ConstraintSubjectGroupClosure &&
				c.SubjectGroupID != nil && *c.SubjectGroupID == groupID {
				return true, nil
			}
		}
		if len(constraints) < pageSize {
			break
		}
		offset += len(constraints)
	}
	return false, nil
}

// requireConstraintAdminForGroup checks whether the group is constraint-bearing
// and, if so, requires the caller to hold access_constraint.admin permission.
// Returns true if the operation may proceed, false if it was denied (response
// already written).
func (s *Server) requireConstraintAdminForGroup(w http.ResponseWriter, r *http.Request, groupID string) bool {
	ctx := r.Context()
	isCB, err := s.isConstraintBearingGroup(ctx, groupID)
	if err != nil {
		// Fail closed: if we cannot determine constraint status, deny.
		writeError(w, http.StatusInternalServerError, ErrCodeRuntimeError,
			"failed to check constraint status for group", nil)
		return false
	}
	if !isCB {
		return true // not constraint-bearing — no extra check needed
	}

	// Group is constraint-bearing: require access_constraint.admin.
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "authentication required", nil)
		return false
	}
	if s.authzService == nil {
		Forbidden(w)
		return false
	}
	decision := s.authzService.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(identity),
		Credential: credentialContextForIdentity(identity),
		Resource:   Resource{Type: "access_constraint", ID: "hub"},
		Action:     ActionManage,
		Permission: PermissionConstraintAdmin,
	})
	if !decision.Allowed {
		writeForbidden(w, "group is referenced by an access constraint; access_constraint.admin permission required")
		return false
	}
	return true
}
