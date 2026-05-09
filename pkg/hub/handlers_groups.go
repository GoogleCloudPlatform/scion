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
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ============================================================================
// Group Endpoints
// ============================================================================

// ListGroupsResponse is the response for listing groups.
type ListGroupsResponse struct {
	Groups       []GroupWithCapabilities `json:"groups"`
	NextCursor   string                  `json:"nextCursor,omitempty"`
	TotalCount   int                     `json:"totalCount"`
	Capabilities *Capabilities           `json:"_capabilities,omitempty"`
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
		MethodNotAllowed(w)
	}
}

func (s *Server) listGroups(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	query := r.URL.Query()

	filter := store.GroupFilter{
		OwnerID:   query.Get("ownerId"),
		ParentID:  query.Get("parentId"),
		GroupType: query.Get("groupType"),
		ProjectID:   query.Get("projectId"),
	}

	limit := 50
	if l := query.Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
			limit = parsed
		}
	}

	result, err := s.store.ListGroups(ctx, filter, store.ListOptions{
		Limit:  limit,
		Cursor: query.Get("cursor"),
	})
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Compute per-item and scope capabilities
	identity := GetIdentityFromContext(ctx)
	groups := make([]GroupWithCapabilities, len(result.Items))
	if identity != nil {
		resources := make([]Resource, len(result.Items))
		for i := range result.Items {
			resources[i] = groupResource(&result.Items[i])
		}
		caps := s.authzService.ComputeCapabilitiesBatch(ctx, identity, resources, "group")
		for i := range result.Items {
			groups[i] = GroupWithCapabilities{Group: result.Items[i], Cap: caps[i]}
		}
	} else {
		for i := range result.Items {
			groups[i] = GroupWithCapabilities{Group: result.Items[i]}
		}
	}

	var scopeCap *Capabilities
	if identity != nil {
		scopeCap = s.authzService.ComputeScopeCapabilities(ctx, identity, "", "", "group")
	}

	writeJSON(w, http.StatusOK, ListGroupsResponse{
		Groups:       groups,
		NextCursor:   result.NextCursor,
		TotalCount:   result.TotalCount,
		Capabilities: scopeCap,
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

	ownerID := req.OwnerID
	createdBy := ""
	if identity := GetIdentityFromContext(ctx); identity != nil {
		createdBy = identity.ID()
		if ownerID == "" {
			ownerID = identity.ID()
		}
	}

	group := &store.Group{
		ID:          api.NewUUID(),
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
		if err == store.ErrAlreadyExists {
			Conflict(w, "Group with this slug already exists")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// Add the creating user as an owner of the new group
	if createdBy != "" {
		if err := s.store.AddGroupMember(ctx, &store.GroupMember{
			GroupID:    group.ID,
			MemberType: store.GroupMemberTypeUser,
			MemberID:   createdBy,
			Role:       store.GroupMemberRoleOwner,
		}); err != nil && err != store.ErrAlreadyExists {
			// Log but don't fail the group creation
			slog.Warn("failed to add creator as owner of new group",
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
		MethodNotAllowed(w)
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
	if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
		decision := s.authzService.CheckAccess(ctx, userIdent, groupResource(group), ActionUpdate)
		if !decision.Allowed {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "Access denied", nil)
			return
		}
	}

	var req UpdateGroupRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
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
	if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
		decision := s.authzService.CheckAccess(ctx, userIdent, groupResource(group), ActionDelete)
		if !decision.Allowed {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "Access denied", nil)
			return
		}
	}

	if group.GroupType == store.GroupTypeProjectAgents {
		BadRequest(w, "project_agents groups are system-managed and cannot be deleted via API")
		return
	}

	if err := s.store.DeleteGroup(ctx, group.ID); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

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
		MethodNotAllowed(w)
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

	// Enforce authorization: only group owner or admins can add members
	if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
		decision := s.authzService.CheckAccess(ctx, userIdent, groupResource(group), ActionAddMember)
		if !decision.Allowed {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "Access denied", nil)
			return
		}
	}

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

	// Enforce role-hierarchy: only owners can add owners/admins; admins can only add members.
	// Platform admins and group resource owners bypass the role-hierarchy check.
	if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
		isResourceOwner := group.OwnerID != "" && group.OwnerID == userIdent.ID()
		isPlatformAdmin := userIdent.Role() == "admin"
		if !isResourceOwner && !isPlatformAdmin {
			callerMembership, err := s.store.GetGroupMembership(ctx, groupID, store.GroupMemberTypeUser, userIdent.ID())
			if req.Role == store.GroupMemberRoleOwner || req.Role == store.GroupMemberRoleAdmin {
				if err != nil || callerMembership.Role != store.GroupMemberRoleOwner {
					writeError(w, http.StatusForbidden, ErrCodeForbidden,
						"Only group owners can add owners or admins", nil)
					return
				}
			} else if req.Role == store.GroupMemberRoleMember {
				if err != nil {
					writeError(w, http.StatusForbidden, ErrCodeForbidden,
						"Only group owners or admins can add members", nil)
					return
				}
				if callerMembership.Role != store.GroupMemberRoleOwner && callerMembership.Role != store.GroupMemberRoleAdmin {
					writeError(w, http.StatusForbidden, ErrCodeForbidden,
						"Only group owners or admins can add members", nil)
					return
				}
			}
		}
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
		if _, err := s.store.GetGroup(ctx, req.MemberID); err != nil {
			if err == store.ErrNotFound {
				memberGroup, slugErr := s.store.GetGroupBySlug(ctx, req.MemberID)
				if slugErr != nil {
					ValidationError(w, "group not found: "+req.MemberID, nil)
					return
				}
				resolvedID = memberGroup.ID
			} else {
				writeErrorFromErr(w, err, "")
				return
			}
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

	if err := s.store.AddGroupMember(ctx, member); err != nil {
		if err == store.ErrAlreadyExists {
			Conflict(w, "Member already exists in this group")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// Return enriched response with display name
	resp := GroupMemberInfo{
		GroupMember: *member,
		DisplayName: s.resolveGroupMemberDisplayName(ctx, member.MemberType, member.MemberID),
	}
	writeJSON(w, http.StatusCreated, resp)
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
		MethodNotAllowed(w)
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
	if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
		decision := s.authzService.CheckAccess(ctx, userIdent, groupResource(group), ActionRemoveMember)
		if !decision.Allowed {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "Access denied", nil)
			return
		}
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

	w.WriteHeader(http.StatusNoContent)
}
