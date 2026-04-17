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

// CompositeStore wraps an existing store.Store and overrides group, policy,
// and workflow run operations with Ent-backed implementations.
type CompositeStore struct {
	store.Store
	groups       *GroupStore
	policies     *PolicyStore
	workflowRuns *WorkflowRunStore
	client       *ent.Client
}

// NewCompositeStore creates a CompositeStore that delegates group, policy, and
// workflow run operations to Ent-backed stores while forwarding all other
// operations to the underlying store.
func NewCompositeStore(base store.Store, client *ent.Client) *CompositeStore {
	return &CompositeStore{
		Store:        base,
		groups:       NewGroupStore(client),
		policies:     NewPolicyStore(client),
		workflowRuns: NewWorkflowRunStore(client),
		client:       client,
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
	// Ensure the grove exists in the Ent database before creating the group,
	// since groves are stored in the base (SQLite) store but groups are in Ent
	// which has a foreign key constraint on grove_id.
	if group.GroveID != "" {
		if err := c.ensureEntGrove(ctx, group.GroveID); err != nil {
			return fmt.Errorf("ensuring grove in ent store: %w", err)
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
// creates a minimal shadow record from the base store. The agent's grove is
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

	// Ensure the grove exists in Ent first (required FK)
	if err := c.ensureEntGrove(ctx, a.GroveID); err != nil {
		return fmt.Errorf("ensuring grove in ent store: %w", err)
	}

	groveUID, err := parseUUID(a.GroveID)
	if err != nil {
		return err
	}

	// Create a minimal shadow record in Ent
	_, err = c.client.Agent.Create().
		SetID(uid).
		SetName(a.Name).
		SetSlug(a.Slug).
		SetGroveID(groveUID).
		Save(ctx)
	if err != nil {
		if ent.IsConstraintError(err) {
			return nil
		}
		return fmt.Errorf("creating shadow agent in ent: %w", err)
	}

	return nil
}

// ensureEntGrove checks if a grove exists in the Ent database and, if not,
// creates a minimal shadow record from the base store.
func (c *CompositeStore) ensureEntGrove(ctx context.Context, groveID string) error {
	uid, err := parseUUID(groveID)
	if err != nil {
		return err
	}

	_, getErr := c.client.Grove.Get(ctx, uid)
	if getErr == nil {
		return nil
	}
	if !ent.IsNotFound(getErr) {
		return fmt.Errorf("checking ent grove existence: %w", getErr)
	}

	g, err := c.Store.GetGrove(ctx, groveID)
	if err != nil {
		return fmt.Errorf("fetching grove from base store: %w", err)
	}

	_, err = c.client.Grove.Create().
		SetID(uid).
		SetName(g.Name).
		SetSlug(g.Slug).
		Save(ctx)
	if err != nil {
		if ent.IsConstraintError(err) {
			return nil
		}
		return fmt.Errorf("creating shadow grove in ent: %w", err)
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

func (c *CompositeStore) GetGroupByGroveID(ctx context.Context, groveID string) (*store.Group, error) {
	return c.groups.GetGroupByGroveID(ctx, groveID)
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

// WorkflowRunStore method overrides — delegate to Ent-backed WorkflowRunStore.

func (c *CompositeStore) CreateWorkflowRun(ctx context.Context, run *store.WorkflowRun) error {
	// Ensure the grove shadow record exists in the Ent database before creating
	// the workflow run (which has a required FK to the grove).
	if run.GroveID != "" {
		if err := c.ensureEntGrove(ctx, run.GroveID); err != nil {
			return err
		}
	}
	// Ensure the user shadow record exists in the Ent database.
	if run.CreatedByUserID != nil {
		if err := c.ensureEntUser(ctx, *run.CreatedByUserID); err != nil {
			// Non-fatal: log and continue; user FK is optional on the run.
			_ = err
		}
	}
	// Ensure the agent shadow record exists in the Ent database.
	// The agent's grove_id comes from the run itself since the handler
	// already validated grove isolation (agentIdent.GroveID() == run.GroveID).
	if run.CreatedByAgentID != nil {
		if err := c.ensureEntAgentForRun(ctx, *run.CreatedByAgentID, run.GroveID); err != nil {
			// Non-fatal: if the shadow record cannot be created (e.g. agent
			// exists in base store but was just deleted), proceed without it.
			// The FK field will be silently dropped by workflowrun_store when
			// the UUID parse fails; here the UUID is valid so we accept the risk.
			_ = err
		}
	}
	return c.workflowRuns.CreateWorkflowRun(ctx, run)
}

// ensureEntAgentForRun ensures an agent shadow record exists in Ent for a
// workflow run's created_by_agent_id FK. Unlike ensureEntAgent (which requires
// the agent to be in the base store), this falls back to creating a minimal
// stub using only the agent ID and grove ID when the base-store lookup fails.
// This handles agent-JWT-initiated runs where the agent JWT is valid but the
// agent was not yet synced to the local replica store (e.g. in tests).
func (c *CompositeStore) ensureEntAgentForRun(ctx context.Context, agentID, groveID string) error {
	uid, err := parseUUID(agentID)
	if err != nil {
		return err
	}

	// Check if agent already exists in Ent.
	if _, getErr := c.client.Agent.Get(ctx, uid); getErr == nil {
		return nil
	} else if !ent.IsNotFound(getErr) {
		return fmt.Errorf("checking ent agent existence: %w", getErr)
	}

	// Ensure the grove exists in Ent first (required FK on Agent).
	if groveID != "" {
		if err := c.ensureEntGrove(ctx, groveID); err != nil {
			return fmt.Errorf("ensuring grove for agent shadow: %w", err)
		}
	}

	groveUID, err := parseUUID(groveID)
	if err != nil {
		return err
	}

	// Try to fetch full details from the base store for a richer shadow record.
	name := "agent-" + uid.String()[:8]
	slug := name
	a, baseErr := c.Store.GetAgent(ctx, agentID)
	if baseErr == nil {
		name = a.Name
		slug = a.Slug
	}

	_, err = c.client.Agent.Create().
		SetID(uid).
		SetName(name).
		SetSlug(slug).
		SetGroveID(groveUID).
		Save(ctx)
	if err != nil {
		if ent.IsConstraintError(err) {
			return nil // created concurrently — that is fine
		}
		return fmt.Errorf("creating shadow agent in ent for run: %w", err)
	}
	return nil
}

func (c *CompositeStore) GetWorkflowRun(ctx context.Context, id string) (*store.WorkflowRun, error) {
	return c.workflowRuns.GetWorkflowRun(ctx, id)
}

func (c *CompositeStore) ListWorkflowRuns(ctx context.Context, opts store.WorkflowRunListOptions) (*store.ListResult[store.WorkflowRun], error) {
	return c.workflowRuns.ListWorkflowRuns(ctx, opts)
}

func (c *CompositeStore) CancelWorkflowRun(ctx context.Context, id string) (*store.WorkflowRun, error) {
	return c.workflowRuns.CancelWorkflowRun(ctx, id)
}

func (c *CompositeStore) TransitionWorkflowRun(ctx context.Context, id string, update store.WorkflowRunTransition, fromStatus []string) (*store.WorkflowRun, error) {
	return c.workflowRuns.TransitionWorkflowRun(ctx, id, update, fromStatus)
}
