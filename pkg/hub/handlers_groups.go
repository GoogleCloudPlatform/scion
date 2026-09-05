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
	cursorBinding := authorizedListCursorBinding("groups", filter)
	if cursor != "" {
		if err := validateAuthorizedListCursor(cursor, cursorBinding); err != nil {
			BadRequest(w, err.Error())
			return
		}
	}
	var groupItems []store.Group
	var nextCursor string
	var totalCount int
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
			writeAuthorizedListError(w, err)
			return
		}
		groupItems, nextCursor, totalCount = result.Items, result.NextCursor, result.TotalCount
	} else {
		// Unauthenticated: return empty list (no identity to authorize against).
		groupItems = []store.Group{}
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
		Groups:       groups,
		NextCursor:   nextCursor,
		TotalCount:   totalCount,
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
	if !s.authorize(w, r, groupResource(group), ActionUpdate) {
		return
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

	if err := s.store.DeleteGroup(ctx, group.ID); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

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
	if !s.authorize(w, r, groupResource(group), ActionAddMember) {
		return
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
	//
	// The hierarchy is defined over user membership in the group, so it cannot be
	// evaluated for an agent or broker caller — which is why the pre-#591 form of
	// this guard let those callers grant any role at all. Such a caller reaches
	// this point only through an explicit addMember policy, and is held to adding
	// plain members: escalating someone to admin or owner requires a caller whose
	// own standing in the group can be checked.
	userIdent, isUserCaller := GetIdentityFromContext(ctx).(UserIdentity)
	if !isUserCaller {
		if req.Role != store.GroupMemberRoleMember {
			writeError(w, http.StatusForbidden, ErrCodeForbidden,
				"Only group owners can add owners or admins", nil)
			return
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
			callerMembership, err := s.store.GetGroupMembership(ctx, groupID, store.GroupMemberTypeUser, userIdent.ID())
			switch req.Role {
			case store.GroupMemberRoleOwner, store.GroupMemberRoleAdmin:
				if err != nil || callerMembership.Role != store.GroupMemberRoleOwner {
					writeError(w, http.StatusForbidden, ErrCodeForbidden,
						"Only group owners can add owners or admins", nil)
					return
				}
			case store.GroupMemberRoleMember:
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

	// CanDelegate check: ensure the actor has sufficient authority to grant
	// the membership. Because membership in a role-bearing group confers all
	// of that group's role-binding authority, the actor must hold every
	// permission that becomes newly reachable.
	//
	// This applies to ALL callers (user AND agent) and ALL member types
	// (user, group, agent). An agent with group.addMember authority must
	// pass the same delegation test as a user caller — group governance role
	// does NOT substitute for resource authority.
	var canDelegateResult, canDelegateReason string
	if s.authzService != nil {
		actorIdentity := GetIdentityFromContext(ctx)
		if actorIdentity != nil {
			grantDesc := GrantDescriptor{
				Type:    GrantTypeGroupMembership,
				GroupID: groupID,
			}
			delegateDecision := s.authzService.CanDelegate(ctx, actorIdentity, grantDesc)
			canDelegateResult = "allow"
			canDelegateReason = delegateDecision.Reason
			if !delegateDecision.Allowed {
				logAuthzDenial(r, actorIdentity, groupResource(group), ActionAddMember,
					"CanDelegate denied: "+delegateDecision.Reason)
				writeForbidden(w, "Cannot grant authority you do not hold: "+delegateDecision.Reason)
				return
			}
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
	if s.quotaService != nil {
		membershipID := fmt.Sprintf("%s:%s:%s", groupID, member.MemberType, member.MemberID)
		if err := s.quotaService.CheckAndReserve(ctx, "max_members_per_group", groupID, "group", groupID, membershipID); err != nil {
			if errors.Is(err, store.ErrQuotaExceeded) {
				writeError(w, http.StatusTooManyRequests, ErrCodeQuotaExceeded,
					"quota exceeded: max_members_per_group", nil)
				return
			}
			if errors.Is(err, ErrQuotaLockContention) {
				writeError(w, http.StatusTooManyRequests, ErrCodeQuotaExceeded,
					"quota check temporarily unavailable, please retry", nil)
				return
			}
			writeError(w, http.StatusInternalServerError, ErrCodeRuntimeError, "quota check failed", nil)
			return
		}
	}

	if err := s.store.AddGroupMember(ctx, member); err != nil {
		if s.quotaService != nil {
			membershipID := fmt.Sprintf("%s:%s:%s", groupID, member.MemberType, member.MemberID)
			s.quotaService.Release(ctx, "max_members_per_group", membershipID)
		}
		if err == store.ErrAlreadyExists {
			Conflict(w, "Member already exists in this group")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	auditRecord := &store.MutationAuditRecord{
		MutationType:      "group_member_add",
		TargetType:        "group_membership",
		TargetID:          group.ID,
		AfterSummary:      `{"groupId":"` + group.ID + `","memberType":"` + member.MemberType + `","memberId":"` + member.MemberID + `","role":"` + member.Role + `"}`,
		CanDelegateResult: canDelegateResult,
		CanDelegateReason: canDelegateReason,
	}
	s.emitMutationAudit(r.Context(), auditRecord)

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
	if !s.authorize(w, r, groupResource(group), ActionRemoveMember) {
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
	if s.quotaService != nil {
		membershipID := fmt.Sprintf("%s:%s:%s", group.ID, memberType, memberID)
		s.quotaService.Release(ctx, "max_members_per_group", membershipID)
	}

	s.emitMutationAudit(r.Context(), &store.MutationAuditRecord{
		MutationType:  "group_member_remove",
		TargetType:    "group_membership",
		TargetID:      group.ID,
		BeforeSummary: `{"groupId":"` + group.ID + `","memberType":"` + memberType + `","memberId":"` + memberID + `"}`,
	})

	s.groupsLogger().Info("group member removed",
		"group_id", group.ID,
		"member_type", memberType,
		"member_id", memberID)

	w.WriteHeader(http.StatusNoContent)
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
