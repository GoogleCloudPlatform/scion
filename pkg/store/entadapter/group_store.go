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

// Package entadapter provides Ent-backed implementations of the store interfaces.
package entadapter

import (
	"context"
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/group"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/groupmembership"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/user"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// GroupStore implements store.GroupStore using Ent ORM.
type GroupStore struct {
	client *ent.Client
}

// NewGroupStore creates a new Ent-backed GroupStore.
func NewGroupStore(client *ent.Client) *GroupStore {
	return &GroupStore{client: client}
}

// parseUUID parses a string UUID, returning store.ErrInvalidInput on failure.
func parseUUID(s string) (uuid.UUID, error) {
	uid, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, fmt.Errorf("%w: invalid UUID %q", store.ErrInvalidInput, s)
	}
	return uid, nil
}

// mapError converts Ent errors to store errors.
func mapError(err error) error {
	if err == nil {
		return nil
	}
	if ent.IsNotFound(err) {
		return store.ErrNotFound
	}
	if ent.IsConstraintError(err) {
		return store.ErrAlreadyExists
	}
	return err
}

// entGroupToStore converts an Ent Group entity to a store.Group model.
func entGroupToStore(g *ent.Group) *store.Group {
	sg := &store.Group{
		ID:          g.ID.String(),
		Name:        g.Name,
		Slug:        g.Slug,
		Description: g.Description,
		GroupType:   string(g.GroupType),
		Labels:      g.Labels,
		Annotations: g.Annotations,
		Created:     g.Created,
		Updated:     g.Updated,
		CreatedBy:   g.CreatedBy,
	}
	if g.GroveID != nil {
		sg.GroveID = g.GroveID.String()
	}
	if g.OwnerID != nil {
		sg.OwnerID = g.OwnerID.String()
	}
	return sg
}

// entMembershipToStore converts an Ent GroupMembership entity to a store.GroupMember model.
func entMembershipToStore(m *ent.GroupMembership) store.GroupMember {
	gm := store.GroupMember{
		GroupID: m.GroupID.String(),
		Role:    string(m.Role),
		AddedAt: m.AddedAt,
		AddedBy: m.AddedBy,
	}
	if m.UserID != nil {
		gm.MemberType = store.GroupMemberTypeUser
		gm.MemberID = m.UserID.String()
	} else if m.AgentID != nil {
		gm.MemberType = store.GroupMemberTypeAgent
		gm.MemberID = m.AgentID.String()
	}
	return gm
}

// CreateGroup creates a new group record.
func (s *GroupStore) CreateGroup(ctx context.Context, g *store.Group) error {
	uid, err := parseUUID(g.ID)
	if err != nil {
		return err
	}

	if g.GroupType == "" {
		g.GroupType = store.GroupTypeExplicit
	}

	create := s.client.Group.Create().
		SetID(uid).
		SetName(g.Name).
		SetSlug(g.Slug).
		SetDescription(g.Description).
		SetGroupType(group.GroupType(g.GroupType)).
		SetCreatedBy(g.CreatedBy)

	if g.GroveID != "" {
		groveUID, err := parseUUID(g.GroveID)
		if err != nil {
			return err
		}
		create.SetGroveID(groveUID)
	}
	if g.Labels != nil {
		create.SetLabels(g.Labels)
	}
	if g.Annotations != nil {
		create.SetAnnotations(g.Annotations)
	}
	if g.OwnerID != "" {
		ownerUID, err := parseUUID(g.OwnerID)
		if err != nil {
			return err
		}
		create.SetOwnerID(ownerUID)
	}

	// ParentID maps to parent_groups edge
	if g.ParentID != "" {
		parentUID, err := parseUUID(g.ParentID)
		if err != nil {
			return err
		}
		create.AddParentGroupIDs(parentUID)
	}

	created, err := create.Save(ctx)
	if err != nil {
		return mapError(err)
	}

	// Populate the store model with the created entity's timestamps
	g.Created = created.Created
	g.Updated = created.Updated
	return nil
}

// GetGroup retrieves a group by ID.
func (s *GroupStore) GetGroup(ctx context.Context, id string) (*store.Group, error) {
	uid, err := parseUUID(id)
	if err != nil {
		return nil, err
	}

	g, err := s.client.Group.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}

	return entGroupToStore(g), nil
}

// GetGroupBySlug retrieves a group by its slug.
func (s *GroupStore) GetGroupBySlug(ctx context.Context, slug string) (*store.Group, error) {
	g, err := s.client.Group.Query().
		Where(group.SlugEQ(slug)).
		Only(ctx)
	if err != nil {
		return nil, mapError(err)
	}

	return entGroupToStore(g), nil
}

// UpdateGroup updates an existing group.
func (s *GroupStore) UpdateGroup(ctx context.Context, g *store.Group) error {
	uid, err := parseUUID(g.ID)
	if err != nil {
		return err
	}

	update := s.client.Group.UpdateOneID(uid).
		SetName(g.Name).
		SetSlug(g.Slug).
		SetDescription(g.Description).
		SetCreatedBy(g.CreatedBy)

	if g.Labels != nil {
		update.SetLabels(g.Labels)
	} else {
		update.ClearLabels()
	}
	if g.Annotations != nil {
		update.SetAnnotations(g.Annotations)
	} else {
		update.ClearAnnotations()
	}
	if g.OwnerID != "" {
		ownerUID, err := parseUUID(g.OwnerID)
		if err != nil {
			return err
		}
		update.SetOwnerID(ownerUID)
	} else {
		update.ClearOwnerID()
	}
	if g.GroveID != "" {
		groveUID, err := parseUUID(g.GroveID)
		if err != nil {
			return err
		}
		update.SetGroveID(groveUID)
	} else {
		update.ClearGroveID()
	}

	updated, err := update.Save(ctx)
	if err != nil {
		return mapError(err)
	}

	g.Updated = updated.Updated
	return nil
}

// DeleteGroup removes a group by ID.
func (s *GroupStore) DeleteGroup(ctx context.Context, id string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}

	// Delete memberships first (Ent cascade may handle this, but be explicit)
	_, _ = s.client.GroupMembership.Delete().
		Where(groupmembership.GroupIDEQ(uid)).
		Exec(ctx)

	err = s.client.Group.DeleteOneID(uid).Exec(ctx)
	if err != nil {
		return mapError(err)
	}
	return nil
}

// ListGroups returns groups matching the filter criteria.
func (s *GroupStore) ListGroups(ctx context.Context, filter store.GroupFilter, opts store.ListOptions) (*store.ListResult[store.Group], error) {
	query := s.client.Group.Query()

	if filter.OwnerID != "" {
		ownerUID, err := parseUUID(filter.OwnerID)
		if err != nil {
			return nil, err
		}
		query.Where(group.OwnerIDEQ(ownerUID))
	}
	if filter.GroupType != "" {
		query.Where(group.GroupTypeEQ(group.GroupType(filter.GroupType)))
	}
	if filter.ParentID != "" {
		parentUID, err := parseUUID(filter.ParentID)
		if err != nil {
			return nil, err
		}
		query.Where(group.HasParentGroupsWith(group.IDEQ(parentUID)))
	}
	if filter.GroveID != "" {
		groveUID, err := parseUUID(filter.GroveID)
		if err != nil {
			return nil, err
		}
		query.Where(group.GroveIDEQ(groveUID))
	}

	// Get total count before pagination
	totalCount, err := query.Clone().Count(ctx)
	if err != nil {
		return nil, err
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}

	groups, err := query.
		Order(group.ByCreated()).
		Limit(limit).
		All(ctx)
	if err != nil {
		return nil, err
	}

	items := make([]store.Group, 0, len(groups))
	for _, g := range groups {
		items = append(items, *entGroupToStore(g))
	}

	return &store.ListResult[store.Group]{
		Items:      items,
		TotalCount: totalCount,
	}, nil
}

// AddGroupMember adds a user, agent, or group as a member of a group.
func (s *GroupStore) AddGroupMember(ctx context.Context, member *store.GroupMember) error {
	groupUID, err := parseUUID(member.GroupID)
	if err != nil {
		return err
	}

	memberUID, err := parseUUID(member.MemberID)
	if err != nil {
		return err
	}

	// Guard against modifying grove_agents groups
	g, err := s.client.Group.Get(ctx, groupUID)
	if err != nil {
		return mapError(err)
	}
	if g.GroupType == group.GroupTypeGroveAgents {
		return fmt.Errorf("%w: cannot manually modify members of grove_agents groups", store.ErrInvalidInput)
	}

	switch member.MemberType {
	case store.GroupMemberTypeUser:
		create := s.client.GroupMembership.Create().
			SetGroupID(groupUID).
			SetUserID(memberUID).
			SetRole(groupmembership.Role(member.Role)).
			SetAddedBy(member.AddedBy)
		m, err := create.Save(ctx)
		if err != nil {
			return mapError(err)
		}
		member.AddedAt = m.AddedAt

	case store.GroupMemberTypeAgent:
		create := s.client.GroupMembership.Create().
			SetGroupID(groupUID).
			SetAgentID(memberUID).
			SetRole(groupmembership.Role(member.Role)).
			SetAddedBy(member.AddedBy)
		m, err := create.Save(ctx)
		if err != nil {
			return mapError(err)
		}
		member.AddedAt = m.AddedAt

	case store.GroupMemberTypeGroup:
		// Group nesting uses the child_groups M2M edge
		_, err := s.client.Group.UpdateOneID(groupUID).
			AddChildGroupIDs(memberUID).
			Save(ctx)
		if err != nil {
			return mapError(err)
		}
		// Also create a GroupMembership-like record in the join table
		// is handled by the edge. Set AddedAt for the caller.
		if member.AddedAt.IsZero() {
			member.AddedAt = g.Updated
		}

	default:
		return fmt.Errorf("%w: unsupported member type %q", store.ErrInvalidInput, member.MemberType)
	}

	return nil
}

// RemoveGroupMember removes a member from a group.
func (s *GroupStore) RemoveGroupMember(ctx context.Context, groupID, memberType, memberID string) error {
	groupUID, err := parseUUID(groupID)
	if err != nil {
		return err
	}

	memberUID, err := parseUUID(memberID)
	if err != nil {
		return err
	}

	// Guard against modifying grove_agents groups
	g, err := s.client.Group.Get(ctx, groupUID)
	if err != nil {
		return mapError(err)
	}
	if g.GroupType == group.GroupTypeGroveAgents {
		return fmt.Errorf("%w: cannot manually modify members of grove_agents groups", store.ErrInvalidInput)
	}

	switch memberType {
	case store.GroupMemberTypeUser:
		count, err := s.client.GroupMembership.Delete().
			Where(
				groupmembership.GroupIDEQ(groupUID),
				groupmembership.UserIDEQ(memberUID),
			).Exec(ctx)
		if err != nil {
			return err
		}
		if count == 0 {
			return store.ErrNotFound
		}

	case store.GroupMemberTypeAgent:
		count, err := s.client.GroupMembership.Delete().
			Where(
				groupmembership.GroupIDEQ(groupUID),
				groupmembership.AgentIDEQ(memberUID),
			).Exec(ctx)
		if err != nil {
			return err
		}
		if count == 0 {
			return store.ErrNotFound
		}

	case store.GroupMemberTypeGroup:
		// Remove from child_groups edge
		_, err := s.client.Group.UpdateOneID(groupUID).
			RemoveChildGroupIDs(memberUID).
			Save(ctx)
		if err != nil {
			return mapError(err)
		}

	default:
		return fmt.Errorf("%w: unsupported member type %q", store.ErrInvalidInput, memberType)
	}

	return nil
}

// UpdateGroupMemberRole updates the role of an existing group member.
func (s *GroupStore) UpdateGroupMemberRole(ctx context.Context, groupID, memberType, memberID, newRole string) error {
	groupUID, err := parseUUID(groupID)
	if err != nil {
		return err
	}

	memberUID, err := parseUUID(memberID)
	if err != nil {
		return err
	}

	switch memberType {
	case store.GroupMemberTypeUser:
		count, err := s.client.GroupMembership.Update().
			Where(
				groupmembership.GroupIDEQ(groupUID),
				groupmembership.UserIDEQ(memberUID),
			).
			SetRole(groupmembership.Role(newRole)).
			Save(ctx)
		if err != nil {
			return err
		}
		if count == 0 {
			return store.ErrNotFound
		}

	case store.GroupMemberTypeAgent:
		count, err := s.client.GroupMembership.Update().
			Where(
				groupmembership.GroupIDEQ(groupUID),
				groupmembership.AgentIDEQ(memberUID),
			).
			SetRole(groupmembership.Role(newRole)).
			Save(ctx)
		if err != nil {
			return err
		}
		if count == 0 {
			return store.ErrNotFound
		}

	default:
		return fmt.Errorf("%w: unsupported member type %q for role update", store.ErrInvalidInput, memberType)
	}

	return nil
}

// GetGroupMembers returns all members of a group.
// For grove_agents groups, membership is resolved at query time by finding
// all agents that belong to the same grove.
func (s *GroupStore) GetGroupMembers(ctx context.Context, groupID string) ([]store.GroupMember, error) {
	groupUID, err := parseUUID(groupID)
	if err != nil {
		return nil, err
	}

	// Check if this is a grove_agents group — use query-time resolution
	g, err := s.client.Group.Get(ctx, groupUID)
	if err != nil {
		return nil, mapError(err)
	}

	if g.GroupType == group.GroupTypeGroveAgents && g.GroveID != nil {
		// Query-time resolution: find all agents in this grove
		agents, err := s.client.Agent.Query().
			Where(agent.GroveIDEQ(*g.GroveID)).
			All(ctx)
		if err != nil {
			return nil, err
		}

		members := make([]store.GroupMember, 0, len(agents))
		for _, a := range agents {
			members = append(members, store.GroupMember{
				GroupID:    groupID,
				MemberType: store.GroupMemberTypeAgent,
				MemberID:   a.ID.String(),
				Role:       store.GroupMemberRoleMember,
				AddedAt:    a.CreatedAt,
				AddedBy:    "system",
			})
		}
		return members, nil
	}

	var members []store.GroupMember

	// Query GroupMembership records (user and agent members)
	memberships, err := s.client.GroupMembership.Query().
		Where(groupmembership.GroupIDEQ(groupUID)).
		All(ctx)
	if err != nil {
		return nil, err
	}

	for _, m := range memberships {
		members = append(members, entMembershipToStore(m))
	}

	// Query child_groups edge (group members)
	children, err := s.client.Group.Query().
		Where(group.IDEQ(groupUID)).
		QueryChildGroups().
		All(ctx)
	if err != nil {
		return nil, err
	}

	for _, child := range children {
		members = append(members, store.GroupMember{
			GroupID:    groupID,
			MemberType: store.GroupMemberTypeGroup,
			MemberID:   child.ID.String(),
			Role:       store.GroupMemberRoleMember,
		})
	}

	return members, nil
}

// GetUserGroups returns all groups a user is a direct member of.
func (s *GroupStore) GetUserGroups(ctx context.Context, userID string) ([]store.GroupMember, error) {
	uid, err := parseUUID(userID)
	if err != nil {
		return nil, err
	}

	memberships, err := s.client.GroupMembership.Query().
		Where(groupmembership.UserIDEQ(uid)).
		All(ctx)
	if err != nil {
		return nil, err
	}

	members := make([]store.GroupMember, 0, len(memberships))
	for _, m := range memberships {
		members = append(members, entMembershipToStore(m))
	}

	return members, nil
}

// GetGroupMembership returns a specific membership record.
func (s *GroupStore) GetGroupMembership(ctx context.Context, groupID, memberType, memberID string) (*store.GroupMember, error) {
	groupUID, err := parseUUID(groupID)
	if err != nil {
		return nil, err
	}

	memberUID, err := parseUUID(memberID)
	if err != nil {
		return nil, err
	}

	switch memberType {
	case store.GroupMemberTypeUser:
		m, err := s.client.GroupMembership.Query().
			Where(
				groupmembership.GroupIDEQ(groupUID),
				groupmembership.UserIDEQ(memberUID),
			).Only(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		gm := entMembershipToStore(m)
		return &gm, nil

	case store.GroupMemberTypeAgent:
		m, err := s.client.GroupMembership.Query().
			Where(
				groupmembership.GroupIDEQ(groupUID),
				groupmembership.AgentIDEQ(memberUID),
			).Only(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		gm := entMembershipToStore(m)
		return &gm, nil

	case store.GroupMemberTypeGroup:
		// Check the child_groups edge
		exists, err := s.client.Group.Query().
			Where(group.IDEQ(groupUID)).
			QueryChildGroups().
			Where(group.IDEQ(memberUID)).
			Exist(ctx)
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, store.ErrNotFound
		}
		return &store.GroupMember{
			GroupID:    groupID,
			MemberType: store.GroupMemberTypeGroup,
			MemberID:   memberID,
			Role:       store.GroupMemberRoleMember,
		}, nil

	default:
		return nil, fmt.Errorf("%w: unsupported member type %q", store.ErrInvalidInput, memberType)
	}
}

// WouldCreateCycle checks if adding memberGroupID to groupID would create a cycle.
func (s *GroupStore) WouldCreateCycle(ctx context.Context, groupID, memberGroupID string) (bool, error) {
	if groupID == memberGroupID {
		return true, nil
	}

	groupUID, err := parseUUID(groupID)
	if err != nil {
		return false, err
	}

	memberUID, err := parseUUID(memberGroupID)
	if err != nil {
		return false, err
	}

	// BFS down through child_groups from memberGroupID looking for groupID
	visited := make(map[uuid.UUID]bool)
	return s.hasPathDown(ctx, memberUID, groupUID, visited, 0)
}

// hasPathDown performs BFS to detect if target is reachable from current through child_groups.
func (s *GroupStore) hasPathDown(ctx context.Context, current, target uuid.UUID, visited map[uuid.UUID]bool, depth int) (bool, error) {
	if current == target {
		return true, nil
	}
	if visited[current] || depth >= 10 {
		return false, nil
	}
	visited[current] = true

	children, err := s.client.Group.Query().
		Where(group.IDEQ(current)).
		QueryChildGroups().
		All(ctx)
	if err != nil {
		return false, err
	}

	for _, child := range children {
		found, err := s.hasPathDown(ctx, child.ID, target, visited, depth+1)
		if err != nil {
			return false, err
		}
		if found {
			return true, nil
		}
	}

	return false, nil
}

// GetEffectiveGroups returns all groups a user belongs to, including
// transitive memberships through nested groups.
func (s *GroupStore) GetEffectiveGroups(ctx context.Context, userID string) ([]string, error) {
	uid, err := parseUUID(userID)
	if err != nil {
		return nil, err
	}

	// Get direct group memberships for the user
	memberships, err := s.client.GroupMembership.Query().
		Where(groupmembership.UserIDEQ(uid)).
		All(ctx)
	if err != nil {
		return nil, err
	}

	// BFS upward through parent_groups
	visited := make(map[uuid.UUID]bool)
	var result []string
	queue := make([]uuid.UUID, 0, len(memberships))

	for _, m := range memberships {
		queue = append(queue, m.GroupID)
	}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		if visited[current] {
			continue
		}
		visited[current] = true
		result = append(result, current.String())

		// Find parent groups (groups that contain current as a child)
		parents, err := s.client.Group.Query().
			Where(group.IDEQ(current)).
			QueryParentGroups().
			All(ctx)
		if err != nil {
			return nil, err
		}

		for _, p := range parents {
			if !visited[p.ID] {
				queue = append(queue, p.ID)
			}
		}
	}

	return result, nil
}

// GetGroupByGroveID retrieves the grove_agents group associated with a grove.
func (s *GroupStore) GetGroupByGroveID(ctx context.Context, groveID string) (*store.Group, error) {
	uid, err := parseUUID(groveID)
	if err != nil {
		return nil, err
	}

	g, err := s.client.Group.Query().
		Where(
			group.GroupTypeEQ(group.GroupTypeGroveAgents),
			group.GroveIDEQ(uid),
		).
		Only(ctx)
	if err != nil {
		return nil, mapError(err)
	}

	return entGroupToStore(g), nil
}

// GetEffectiveGroupsForAgent returns all groups an agent belongs to,
// including the implicit grove_agents group and transitive parent groups.
func (s *GroupStore) GetEffectiveGroupsForAgent(ctx context.Context, agentID string) ([]string, error) {
	uid, err := parseUUID(agentID)
	if err != nil {
		return nil, err
	}

	// Get the agent to find its grove_id
	a, err := s.client.Agent.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}

	// Collect direct group IDs: explicit memberships + implicit grove group
	visited := make(map[uuid.UUID]bool)
	queue := make([]uuid.UUID, 0)

	// 1. Get explicit group memberships for the agent
	memberships, err := s.client.GroupMembership.Query().
		Where(groupmembership.AgentIDEQ(uid)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	for _, m := range memberships {
		queue = append(queue, m.GroupID)
	}

	// 2. Find the implicit grove_agents group for this agent's grove
	groveGroup, err := s.client.Group.Query().
		Where(
			group.GroupTypeEQ(group.GroupTypeGroveAgents),
			group.GroveIDEQ(a.GroveID),
		).
		Only(ctx)
	if err == nil {
		queue = append(queue, groveGroup.ID)
	}
	// If no grove group exists, that's fine — just skip it

	// 3. BFS upward through parent_groups (reuse same logic as GetEffectiveGroups)
	var result []string
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		if visited[current] {
			continue
		}
		visited[current] = true
		result = append(result, current.String())

		parents, err := s.client.Group.Query().
			Where(group.IDEQ(current)).
			QueryParentGroups().
			All(ctx)
		if err != nil {
			return nil, err
		}

		for _, p := range parents {
			if !visited[p.ID] {
				queue = append(queue, p.ID)
			}
		}
	}

	return result, nil
}

// CheckDelegatedAccess checks whether an agent's delegation relationship
// satisfies the given policy conditions.
func (s *GroupStore) CheckDelegatedAccess(ctx context.Context, agentID string, conditions *store.PolicyConditions) (bool, error) {
	if conditions == nil {
		return false, nil
	}
	if conditions.DelegatedFrom == nil && conditions.DelegatedFromGroup == "" {
		return false, nil
	}

	uid, err := parseUUID(agentID)
	if err != nil {
		return false, err
	}

	// Load agent with creator edge
	a, err := s.client.Agent.Query().
		Where(agent.IDEQ(uid)).
		WithCreator().
		Only(ctx)
	if err != nil {
		return false, mapError(err)
	}

	// Check delegation_enabled flag
	if !a.DelegationEnabled {
		return false, nil
	}

	// Check creator exists
	creator := a.Edges.Creator
	if creator == nil {
		return false, nil
	}

	// Suspended creators cannot be delegation sources
	if creator.Status == user.StatusSuspended {
		return false, nil
	}

	// Check DelegatedFrom condition (direct creator match)
	if conditions.DelegatedFrom != nil {
		if conditions.DelegatedFrom.PrincipalType == "user" &&
			conditions.DelegatedFrom.PrincipalID == creator.ID.String() {
			return true, nil
		}
		// DelegatedFrom was specified but didn't match
		return false, nil
	}

	// Check DelegatedFromGroup condition (creator is in specified group)
	if conditions.DelegatedFromGroup != "" {
		creatorGroups, err := s.GetEffectiveGroups(ctx, creator.ID.String())
		if err != nil {
			return false, err
		}
		for _, gid := range creatorGroups {
			if gid == conditions.DelegatedFromGroup {
				return true, nil
			}
		}
		return false, nil
	}

	return false, nil
}

// CountGroupMembersByRole counts how many members of a group have the given role.
func (s *GroupStore) CountGroupMembersByRole(ctx context.Context, groupID, role string) (int, error) {
	groupUID, err := parseUUID(groupID)
	if err != nil {
		return 0, err
	}

	count, err := s.client.GroupMembership.Query().
		Where(
			groupmembership.GroupIDEQ(groupUID),
			groupmembership.RoleEQ(groupmembership.Role(role)),
		).
		Count(ctx)
	if err != nil {
		return 0, err
	}

	return count, nil
}

// GetGroupsByIDs retrieves groups by a list of IDs.
// Returns only groups that exist; missing IDs are silently skipped.
func (s *GroupStore) GetGroupsByIDs(ctx context.Context, ids []string) ([]store.Group, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	uuids := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		uid, err := parseUUID(id)
		if err != nil {
			continue // skip invalid UUIDs
		}
		uuids = append(uuids, uid)
	}

	if len(uuids) == 0 {
		return nil, nil
	}

	groups, err := s.client.Group.Query().
		Where(group.IDIn(uuids...)).
		All(ctx)
	if err != nil {
		return nil, err
	}

	result := make([]store.Group, 0, len(groups))
	for _, g := range groups {
		result = append(result, *entGroupToStore(g))
	}

	return result, nil
}
