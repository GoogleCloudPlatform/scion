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
	"net/http"
	"strconv"
	"strings"

	"github.com/ptone/scion-agent/pkg/api"
	"github.com/ptone/scion-agent/pkg/store"
)

// ============================================================================
// Group Endpoints
// ============================================================================

// ListGroupsResponse is the response for listing groups.
type ListGroupsResponse struct {
	Groups     []store.Group `json:"groups"`
	NextCursor string        `json:"nextCursor,omitempty"`
	TotalCount int           `json:"totalCount"`
}

// CreateGroupRequest is the request body for creating a group.
type CreateGroupRequest struct {
	Name        string            `json:"name"`
	Slug        string            `json:"slug,omitempty"`
	Description string            `json:"description,omitempty"`
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

// ListGroupMembersResponse is the response for listing group members.
type ListGroupMembersResponse struct {
	Members []store.GroupMember `json:"members"`
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
		OwnerID:  query.Get("ownerId"),
		ParentID: query.Get("parentId"),
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

	writeJSON(w, http.StatusOK, ListGroupsResponse{
		Groups:     result.Items,
		NextCursor: result.NextCursor,
		TotalCount: result.TotalCount,
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

	slug := req.Slug
	if slug == "" {
		slug = api.Slugify(req.Name)
	}

	group := &store.Group{
		ID:          api.NewUUID(),
		Name:        req.Name,
		Slug:        slug,
		Description: req.Description,
		ParentID:    req.ParentID,
		Labels:      req.Labels,
		Annotations: req.Annotations,
		OwnerID:     req.OwnerID,
		// CreatedBy: TODO: Get from auth context
	}

	if err := s.store.CreateGroup(ctx, group); err != nil {
		if err == store.ErrAlreadyExists {
			Conflict(w, "Group with this slug already exists")
			return
		}
		writeErrorFromErr(w, err, "")
		return
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

	writeJSON(w, http.StatusOK, group)
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
		s.addGroupMember(w, r, group.ID)
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

	writeJSON(w, http.StatusOK, ListGroupMembersResponse{
		Members: members,
	})
}

func (s *Server) addGroupMember(w http.ResponseWriter, r *http.Request, groupID string) {
	ctx := r.Context()

	var req AddGroupMemberRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if req.MemberType == "" {
		ValidationError(w, "memberType is required", nil)
		return
	}
	if req.MemberType != store.GroupMemberTypeUser && req.MemberType != store.GroupMemberTypeGroup {
		ValidationError(w, "memberType must be 'user' or 'group'", nil)
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

	// Check for cycles when adding a group as a member
	if req.MemberType == store.GroupMemberTypeGroup {
		wouldCycle, err := s.store.WouldCreateCycle(ctx, groupID, req.MemberID)
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
		MemberID:   req.MemberID,
		Role:       req.Role,
		// AddedBy: TODO: Get from auth context
	}

	if err := s.store.AddGroupMember(ctx, member); err != nil {
		if err == store.ErrAlreadyExists {
			Conflict(w, "Member already exists in this group")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusCreated, member)
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

	if memberType != store.GroupMemberTypeUser && memberType != store.GroupMemberTypeGroup {
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
		s.removeGroupMember(w, r, group.ID, memberType, memberID)
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

func (s *Server) removeGroupMember(w http.ResponseWriter, r *http.Request, groupID, memberType, memberID string) {
	ctx := r.Context()

	if err := s.store.RemoveGroupMember(ctx, groupID, memberType, memberID); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
