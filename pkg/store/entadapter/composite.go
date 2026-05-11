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

package entadapter

import (
	"context"
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	entuser "github.com/GoogleCloudPlatform/scion/pkg/ent/user"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// CompositeStore wraps an existing store.Store and overrides group and policy
// operations with Ent-backed implementations.
type CompositeStore struct {
	store.Store
	groups   *GroupStore
	policies *PolicyStore
	client   *ent.Client
}

// NewCompositeStore creates a CompositeStore that delegates group and policy
// operations to Ent-backed stores while forwarding all other operations to the
// underlying store.
func NewCompositeStore(base store.Store, client *ent.Client) *CompositeStore {
	return &CompositeStore{
		Store:    base,
		groups:   NewGroupStore(client),
		policies: NewPolicyStore(client),
		client:   client,
	}
}

// Close closes both the Ent client and the underlying store.
func (c *CompositeStore) Close() error {
	if err := c.client.Close(); err != nil {
		_ = c.Store.Close()
		return err
	}
	return c.Store.Close()
}

// GroupStore method overrides — delegate to Ent-backed GroupStore.

func (c *CompositeStore) CreateGroup(ctx context.Context, group *store.Group) error {
	// Ensure the project exists in the Ent database before creating the group,
	// since projects are stored in the base (SQLite) store but groups are in Ent
	// which has a foreign key constraint on project_id.
	if group.ProjectID != "" {
		if err := c.ensureEntProject(ctx, group.ProjectID); err != nil {
			return fmt.Errorf("ensuring project in ent store: %w", err)
		}
	}
	return c.groups.CreateGroup(ctx, group)
}

func (c *CompositeStore) GetGroup(ctx context.Context, id string) (*store.Group, error) {
	return c.groups.GetGroup(ctx, id)
}

func (c *CompositeStore) GetGroupBySlug(ctx context.Context, slug string) (*store.Group, error) {
	return c.groups.GetGroupBySlug(ctx, slug)
}

func (c *CompositeStore) UpdateGroup(ctx context.Context, group *store.Group) error {
	return c.groups.UpdateGroup(ctx, group)
}

func (c *CompositeStore) DeleteGroup(ctx context.Context, id string) error {
	return c.groups.DeleteGroup(ctx, id)
}

func (c *CompositeStore) ListGroups(ctx context.Context, filter store.GroupFilter, opts store.ListOptions) (*store.ListResult[store.Group], error) {
	return c.groups.ListGroups(ctx, filter, opts)
}

func (c *CompositeStore) AddGroupMember(ctx context.Context, member *store.GroupMember) error {
	switch member.MemberType {
	case store.GroupMemberTypeUser:
		if err := c.ensureEntUser(ctx, member.MemberID); err != nil {
			return fmt.Errorf("ensuring user in ent store: %w", err)
		}
	case store.GroupMemberTypeAgent:
		if err := c.ensureEntAgent(ctx, member.MemberID); err != nil {
			return fmt.Errorf("ensuring agent in ent store: %w", err)
		}
	}
	return c.groups.AddGroupMember(ctx, member)
}

// ensureEntUser checks if a user exists in the Ent database and, if not,
// creates a minimal shadow record from the base store. This is needed because
// the Ent database has foreign key constraints on group memberships, but users
// may only exist in the base (main SQLite) database.
func (c *CompositeStore) ensureEntUser(ctx context.Context, userID string) error {
	uid, err := parseUUID(userID)
	if err != nil {
		return err
	}

	// Check if user already exists in Ent
	exists, err := c.client.User.Query().Where(entuser.IDEQ(uid)).Exist(ctx)
	if err != nil {
		return fmt.Errorf("checking ent user existence: %w", err)
	}
	if exists {
		return nil
	}

	// Fetch from the base store
	u, err := c.Store.GetUser(ctx, userID)
	if err != nil {
		return fmt.Errorf("fetching user from base store: %w", err)
	}

	// Create a minimal shadow record in Ent
	_, err = c.client.User.Create().
		SetID(uid).
		SetEmail(u.Email).
		SetDisplayName(u.DisplayName).
		SetRole(entuser.Role(u.Role)).
		Save(ctx)
	if err != nil {
		// Another goroutine may have created it concurrently
		if ent.IsConstraintError(err) {
			return nil
		}
		return fmt.Errorf("creating shadow user in ent: %w", err)
	}

	return nil
}

// ensureEntAgent checks if an agent exists in the Ent database and, if not,
// creates a minimal shadow record from the base store. The agent.s project is
// also ensured to exist in Ent since it is a required FK.
func (c *CompositeStore) ensureEntAgent(ctx context.Context, agentID string) error {
	uid, err := parseUUID(agentID)
	if err != nil {
		return err
	}

	// Check if agent already exists in Ent
	_, getErr := c.client.Agent.Get(ctx, uid)
	if getErr == nil {
		return nil // already exists
	}
	if !ent.IsNotFound(getErr) {
		return fmt.Errorf("checking ent agent existence: %w", getErr)
	}

	// Fetch from the base store
	a, err := c.Store.GetAgent(ctx, agentID)
	if err != nil {
		return fmt.Errorf("fetching agent from base store: %w", err)
	}

	// Ensure the project exists in Ent first (required FK)
	if err := c.ensureEntProject(ctx, a.ProjectID); err != nil {
		return fmt.Errorf("ensuring project in ent store: %w", err)
	}

	projectUID, err := parseUUID(a.ProjectID)
	if err != nil {
		return err
	}

	// Create a minimal shadow record in Ent
	_, err = c.client.Agent.Create().
		SetID(uid).
		SetName(a.Name).
		SetSlug(a.Slug).
		SetProjectID(projectUID).
		Save(ctx)
	if err != nil {
		if ent.IsConstraintError(err) {
			return nil
		}
		return fmt.Errorf("creating shadow agent in ent: %w", err)
	}

	return nil
}

// ensureEntProject checks if a project exists in the Ent database and, if not,
// creates a minimal shadow record from the base store.
func (c *CompositeStore) ensureEntProject(ctx context.Context, projectID string) error {
	uid, err := parseUUID(projectID)
	if err != nil {
		return err
	}

	_, getErr := c.client.Project.Get(ctx, uid)
	if getErr == nil {
		return nil
	}
	if !ent.IsNotFound(getErr) {
		return fmt.Errorf("checking ent project existence: %w", getErr)
	}

	g, err := c.Store.GetProject(ctx, projectID)
	if err != nil {
		return fmt.Errorf("fetching project from base store: %w", err)
	}

	_, err = c.client.Project.Create().
		SetID(uid).
		SetName(g.Name).
		SetSlug(g.Slug).
		Save(ctx)
	if err != nil {
		if ent.IsConstraintError(err) {
			return nil
		}
		return fmt.Errorf("creating shadow project in ent: %w", err)
	}

	return nil
}

func (c *CompositeStore) UpdateGroupMemberRole(ctx context.Context, groupID, memberType, memberID, newRole string) error {
	return c.groups.UpdateGroupMemberRole(ctx, groupID, memberType, memberID, newRole)
}

func (c *CompositeStore) RemoveGroupMember(ctx context.Context, groupID, memberType, memberID string) error {
	return c.groups.RemoveGroupMember(ctx, groupID, memberType, memberID)
}

func (c *CompositeStore) GetGroupMembers(ctx context.Context, groupID string) ([]store.GroupMember, error) {
	return c.groups.GetGroupMembers(ctx, groupID)
}

func (c *CompositeStore) GetUserGroups(ctx context.Context, userID string) ([]store.GroupMember, error) {
	return c.groups.GetUserGroups(ctx, userID)
}

func (c *CompositeStore) GetGroupMembership(ctx context.Context, groupID, memberType, memberID string) (*store.GroupMember, error) {
	return c.groups.GetGroupMembership(ctx, groupID, memberType, memberID)
}

func (c *CompositeStore) WouldCreateCycle(ctx context.Context, groupID, memberGroupID string) (bool, error) {
	return c.groups.WouldCreateCycle(ctx, groupID, memberGroupID)
}

func (c *CompositeStore) GetEffectiveGroups(ctx context.Context, userID string) ([]string, error) {
	return c.groups.GetEffectiveGroups(ctx, userID)
}

func (c *CompositeStore) GetGroupByProjectID(ctx context.Context, projectID string) (*store.Group, error) {
	return c.groups.GetGroupByProjectID(ctx, projectID)
}

func (c *CompositeStore) GetEffectiveGroupsForAgent(ctx context.Context, agentID string) ([]string, error) {
	return c.groups.GetEffectiveGroupsForAgent(ctx, agentID)
}

func (c *CompositeStore) CheckDelegatedAccess(ctx context.Context, agentID string, conditions *store.PolicyConditions) (bool, error) {
	return c.groups.CheckDelegatedAccess(ctx, agentID, conditions)
}

func (c *CompositeStore) GetGroupsByIDs(ctx context.Context, ids []string) ([]store.Group, error) {
	return c.groups.GetGroupsByIDs(ctx, ids)
}

func (c *CompositeStore) CountGroupMembersByRole(ctx context.Context, groupID, role string) (int, error) {
	return c.groups.CountGroupMembersByRole(ctx, groupID, role)
}

// PolicyStore method overrides — delegate to Ent-backed PolicyStore.

func (c *CompositeStore) CreatePolicy(ctx context.Context, policy *store.Policy) error {
	return c.policies.CreatePolicy(ctx, policy)
}

func (c *CompositeStore) GetPolicy(ctx context.Context, id string) (*store.Policy, error) {
	return c.policies.GetPolicy(ctx, id)
}

func (c *CompositeStore) UpdatePolicy(ctx context.Context, policy *store.Policy) error {
	return c.policies.UpdatePolicy(ctx, policy)
}

func (c *CompositeStore) DeletePolicy(ctx context.Context, id string) error {
	return c.policies.DeletePolicy(ctx, id)
}

func (c *CompositeStore) ListPolicies(ctx context.Context, filter store.PolicyFilter, opts store.ListOptions) (*store.ListResult[store.Policy], error) {
	return c.policies.ListPolicies(ctx, filter, opts)
}

func (c *CompositeStore) AddPolicyBinding(ctx context.Context, binding *store.PolicyBinding) error {
	return c.policies.AddPolicyBinding(ctx, binding)
}

func (c *CompositeStore) RemovePolicyBinding(ctx context.Context, policyID, principalType, principalID string) error {
	return c.policies.RemovePolicyBinding(ctx, policyID, principalType, principalID)
}

func (c *CompositeStore) GetPolicyBindings(ctx context.Context, policyID string) ([]store.PolicyBinding, error) {
	return c.policies.GetPolicyBindings(ctx, policyID)
}

func (c *CompositeStore) GetPoliciesForPrincipal(ctx context.Context, principalType, principalID string) ([]store.Policy, error) {
	return c.policies.GetPoliciesForPrincipal(ctx, principalType, principalID)
}

func (c *CompositeStore) GetPoliciesForPrincipals(ctx context.Context, principals []store.PrincipalRef) ([]store.Policy, error) {
	return c.policies.GetPoliciesForPrincipals(ctx, principals)
}
