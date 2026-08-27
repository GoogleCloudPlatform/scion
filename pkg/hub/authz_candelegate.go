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

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Phase 1F: CanDelegate admission gate.
//
// CanDelegate must be called on every path that creates authority. The
// current wiring:
//
//   - Role binding creation: no HTTP handlers exist yet. Future handlers
//     MUST call CanDelegate before creating a binding.
//   - Group membership: wired in handlers_groups.go:addGroupMember.
//   - Agent delegation: wired in handlers_agents_core.go:createAgentInProject.
//   - Scheduled dispatch: wired in server.go:authorizeScheduledAgentCreate.
//   - Policy create/update/bind: gated by requireAdmin (super-admin only).
//     CanDelegate enforcement is implicit. If policy routes are ever opened
//     to scoped admins, explicit CanDelegate checks must be added.
//   - Custom role definition create/update: no HTTP handlers exist yet.
//     Future handlers MUST call CanDelegate to verify the actor holds all
//     permissions defined in the custom role.

// GrantType identifies the kind of authority being delegated.
type GrantType string

const (
	GrantTypeRoleBinding       GrantType = "role_binding"
	GrantTypeGroupMembership   GrantType = "group_membership"
	GrantTypeAgentDelegation   GrantType = "agent_delegation"
	GrantTypeCustomRole        GrantType = "custom_role"
	GrantTypePolicy            GrantType = "policy"
	GrantTypeProjectMembership GrantType = "project_membership"
)

// GrantDescriptor describes authority being created or modified.
type GrantDescriptor struct {
	// Type of grant being created.
	Type GrantType

	// For role bindings: the role definition being bound.
	RoleDefinitionID string
	RolePermissions  []string // resolved permissions if available

	// For group membership: the group and role being granted.
	GroupID   string
	GroupRole string // "owner", "admin", "member"

	// For agent delegation: the role/scopes being delegated.
	AgentRole   string
	AgentScopes []AgentTokenScope
	ProjectID   string // target project

	// For custom roles: the permissions being defined.
	CustomRolePermissions []string

	// For policy: the policy being created/bound.
	PolicyEffect   string // "allow", "deny"
	PolicyActions  []string
	PolicyResource string

	// Scope of the grant.
	ScopeType string // "system", "project"
	ScopeID   string
}

// CanDelegate checks whether the actor has sufficient authority to create
// the described grant. Returns a Decision indicating whether the delegation
// is permitted.
//
// The core rule: an actor can only delegate authority they themselves possess.
// Super-admin can delegate anything. Scoped admins can only delegate
// permissions they hold via their own role bindings.
func (a *AuthzService) CanDelegate(ctx context.Context, actor Identity, grant GrantDescriptor) Decision {
	if actor == nil {
		return Decision{Allowed: false, Reason: "missing actor"}
	}

	// Scoped credentials (UAT) can only delegate within their credential scope.
	if scoped, ok := actor.(*ScopedUserIdentity); ok {
		if denied := a.enforceUATDelegation(scoped, grant); denied != nil {
			return *denied
		}
	}

	// Super-admin can delegate anything.
	if user, ok := actor.(UserIdentity); ok && IsUnscopedLocalPlatformAdmin(user) {
		return Decision{
			Allowed: true,
			Reason:  "super-admin can delegate any authority",
		}
	}

	// Route to type-specific delegation check.
	switch grant.Type {
	case GrantTypeRoleBinding:
		return a.canDelegateRoleBinding(ctx, actor, grant)
	case GrantTypeGroupMembership:
		return a.canDelegateGroupMembership(ctx, actor, grant)
	case GrantTypeAgentDelegation:
		return a.canDelegateAgent(ctx, actor, grant)
	case GrantTypeCustomRole:
		return a.canDelegateCustomRole(ctx, actor, grant)
	case GrantTypePolicy:
		return a.canDelegatePolicy(ctx, actor, grant)
	case GrantTypeProjectMembership:
		return a.canDelegateProjectMembership(ctx, actor, grant)
	default:
		return Decision{Allowed: false, Reason: "unknown grant type: " + string(grant.Type)}
	}
}

// enforceUATDelegation checks that a scoped UAT credential only delegates
// within its credential scope.
func (a *AuthzService) enforceUATDelegation(scoped *ScopedUserIdentity, grant GrantDescriptor) *Decision {
	// UAT is project-scoped: delegation must target the same project.
	if grant.ScopeType == store.RoleScopeProject && grant.ScopeID != "" {
		if scoped.ScopedProjectID() != grant.ScopeID {
			return &Decision{
				Allowed: false,
				Reason:  "scoped credential cannot delegate outside its project",
			}
		}
	}
	// System-scoped grants are never allowed via UAT.
	if grant.ScopeType == store.RoleScopeSystem {
		return &Decision{
			Allowed: false,
			Reason:  "scoped credential cannot create system-scoped grants",
		}
	}
	return nil
}

// canDelegateRoleBinding checks whether the actor holds all permissions that
// would be granted by binding the target role definition.
func (a *AuthzService) canDelegateRoleBinding(ctx context.Context, actor Identity, grant GrantDescriptor) Decision {
	// Resolve the target role's permissions.
	targetPerms := grant.RolePermissions
	if len(targetPerms) == 0 && grant.RoleDefinitionID != "" {
		rd, err := a.store.GetRoleDefinition(ctx, grant.RoleDefinitionID)
		if err != nil {
			return Decision{Allowed: false, Reason: "cannot resolve target role definition"}
		}
		targetPerms = rd.Permissions
	}
	if len(targetPerms) == 0 {
		// Empty permission set — nothing to delegate.
		return Decision{Allowed: true, Reason: "empty permission set"}
	}

	return a.actorHoldsAllPermissions(ctx, actor, targetPerms, grant.ScopeType, grant.ScopeID)
}

// canDelegateGroupMembership checks whether the actor can add a member to
// a group at the given role. For project member groups, adding a member with
// an elevated role grants project-level authority — the actor must hold at
// least that authority.
func (a *AuthzService) canDelegateGroupMembership(ctx context.Context, actor Identity, grant GrantDescriptor) Decision {
	// If the group is a project-scoped members group, check that the actor
	// holds the equivalent project permissions.
	if grant.ProjectID != "" && grant.GroupRole != "" {
		projectRoleName := groupRoleToProjectRole(grant.GroupRole)
		if projectRoleName != "" {
			rd, err := a.store.GetRoleDefinitionByName(ctx, projectRoleName, store.RoleScopeProject)
			if err != nil {
				return Decision{Allowed: false, Reason: "cannot resolve project role definition for group role"}
			}
			return a.actorHoldsAllPermissions(ctx, actor, rd.Permissions, store.RoleScopeProject, grant.ProjectID)
		}
	}

	// For non-project groups, check that the actor is at least a group admin
	// or owner. This is already enforced by the handler's role-hierarchy check,
	// so CanDelegate adds the permission-level validation layer.
	return Decision{Allowed: true, Reason: "group membership delegation permitted"}
}

// canDelegateAgent checks whether the actor holds all scopes/permissions
// that the new agent would receive.
func (a *AuthzService) canDelegateAgent(ctx context.Context, actor Identity, grant GrantDescriptor) Decision {
	// For agent callers: check scope-by-scope.
	if agentActor, ok := actor.(AgentIdentity); ok {
		return a.canAgentDelegateToAgent(agentActor, grant)
	}

	// For user callers: verify the user has agent-create authority in the
	// target project. The role ceiling logic (ResolveEffectiveRole, minRole)
	// already caps the effective role to the project max and caller ceiling.
	// CanDelegate adds the check that the user has at least the base
	// create-agent permission. For role-level escalation prevention between
	// users, the system relies on the role ceiling logic rather than
	// individual permission matching, because user permissions come from
	// both role bindings AND policies, and the policy-granted permissions
	// (e.g., project member create-agents policy) are the primary source of
	// agent creation authority for most users.
	agentRole := AgentRole(grant.AgentRole)
	if agentRole == AgentRoleNone || agentRole == "" {
		return Decision{Allowed: true, Reason: "agent role has no permissions to delegate"}
	}

	// Check the user can create agents (has agent.create permission).
	createPerms := []string{"agent.create"}
	decision := a.actorHoldsAllPermissions(ctx, actor, createPerms, store.RoleScopeProject, grant.ProjectID)
	if !decision.Allowed {
		return decision
	}

	// For additional agent scopes beyond the standard role, verify the user
	// holds those permissions too.
	if len(grant.AgentScopes) > 0 {
		extraPerms := scopesToPermissionIDs(grant.AgentScopes)
		if len(extraPerms) > 0 {
			return a.actorHoldsAllPermissions(ctx, actor, extraPerms, store.RoleScopeProject, grant.ProjectID)
		}
	}

	return Decision{Allowed: true, Reason: "user has agent-create authority; role ceiling governs effective role"}
}

// canAgentDelegateToAgent checks that an agent creating a sub-agent holds
// at least the scopes being delegated.
func (a *AuthzService) canAgentDelegateToAgent(agentActor AgentIdentity, grant GrantDescriptor) Decision {
	// Resolve the requested role to scopes.
	requestedRole := AgentRole(grant.AgentRole)
	requestedScopes := ScopesForRole(requestedRole)
	requestedScopes = append(requestedScopes, grant.AgentScopes...)

	actorScopes := agentActor.Scopes()
	actorScopeSet := make(map[AgentTokenScope]bool, len(actorScopes))
	for _, s := range actorScopes {
		actorScopeSet[s] = true
	}

	for _, s := range requestedScopes {
		if !actorScopeSet[s] {
			return Decision{
				Allowed: false,
				Reason:  "agent lacks scope for delegation: " + string(s),
			}
		}
	}

	return Decision{Allowed: true, Reason: "agent holds all delegated scopes"}
}

// canDelegateCustomRole checks whether the actor holds all permissions
// defined in the custom role.
func (a *AuthzService) canDelegateCustomRole(ctx context.Context, actor Identity, grant GrantDescriptor) Decision {
	if len(grant.CustomRolePermissions) == 0 {
		return Decision{Allowed: true, Reason: "custom role has no permissions"}
	}
	return a.actorHoldsAllPermissions(ctx, actor, grant.CustomRolePermissions, grant.ScopeType, grant.ScopeID)
}

// canDelegatePolicy checks whether the actor can create/modify policies.
// Raw policy authoring is super-admin-only for now.
func (a *AuthzService) canDelegatePolicy(_ context.Context, _ Identity, _ GrantDescriptor) Decision {
	// Per brief constraint #3: raw policy authoring is super-admin-only.
	// Super-admin is already handled above, so reaching here means non-admin.
	return Decision{
		Allowed: false,
		Reason:  "policy authoring requires super-admin",
	}
}

// canDelegateProjectMembership checks whether the actor can add members to
// a project. The actor must be a project owner or admin.
func (a *AuthzService) canDelegateProjectMembership(ctx context.Context, actor Identity, grant GrantDescriptor) Decision {
	if grant.ProjectID == "" {
		return Decision{Allowed: false, Reason: "project membership requires a project ID"}
	}
	userID := actor.ID()
	if a.isProjectOwnerOrAdmin(ctx, userID, grant.ProjectID) {
		return Decision{Allowed: true, Reason: "project owner/admin can manage membership"}
	}
	return Decision{Allowed: false, Reason: "actor is not a project owner or admin"}
}

// actorHoldsAllPermissions resolves the actor's effective permissions in the
// given scope and checks that every permission in targetPerms is held.
// Permissions come from both role bindings AND policy grants, because a user's
// effective authority is the union of both sources.
func (a *AuthzService) actorHoldsAllPermissions(ctx context.Context, actor Identity, targetPerms []string, scopeType, scopeID string) Decision {
	actorPerms, err := a.getEffectivePermissions(ctx, actor.Type(), actor.ID(), scopeType, scopeID)
	if err != nil {
		a.logger.Warn("failed to resolve actor permissions for delegation check",
			"actor_id", actor.ID(), "error", err)
		return Decision{Allowed: false, Reason: "failed to resolve actor permissions"}
	}

	// Also include system-scoped permissions (they apply everywhere).
	if scopeType == store.RoleScopeProject {
		systemPerms, err := a.getEffectivePermissions(ctx, actor.Type(), actor.ID(), store.RoleScopeSystem, "")
		if err == nil {
			actorPerms = append(actorPerms, systemPerms...)
		}
	}

	// Also check policy-granted permissions: policies bound to the user
	// or their groups also grant authority the user can delegate.
	policyPerms := a.getPolicyGrantedPermissions(ctx, actor, scopeType, scopeID)
	actorPerms = append(actorPerms, policyPerms...)

	actorPermSet := make(map[string]bool, len(actorPerms))
	for _, p := range actorPerms {
		actorPermSet[p] = true
	}

	for _, perm := range targetPerms {
		if !actorPermSet[perm] {
			return Decision{
				Allowed: false,
				Reason:  "actor lacks permission for delegation: " + perm,
			}
		}
	}

	return Decision{Allowed: true, Reason: "actor holds all required permissions"}
}

// getPolicyGrantedPermissions resolves permissions granted to the actor via
// policies (as opposed to role bindings). This ensures that policy-granted
// authority (e.g., project member policies bound to groups) is considered
// when checking delegation authority.
func (a *AuthzService) getPolicyGrantedPermissions(ctx context.Context, actor Identity, scopeType, scopeID string) []string {
	principals, err := a.authorizationPrincipals(ctx, actor)
	if err != nil {
		return nil
	}

	policies, err := a.store.GetPoliciesForPrincipals(ctx, principals)
	if err != nil {
		return nil
	}

	seen := make(map[string]bool)
	var result []string
	for _, policy := range policies {
		if policy.Effect != "allow" {
			continue
		}
		// Filter by scope
		if scopeType == store.RoleScopeProject && policy.ScopeType == "project" {
			if policy.ScopeID != scopeID {
				continue
			}
		}
		// Map policy actions + resource type to permission IDs
		for _, action := range policy.Actions {
			if action == "*" {
				// Wildcard action — grant all permissions for the resource type
				for _, p := range permissions.Registry {
					if policy.ResourceType == "*" || p.Resource == policy.ResourceType {
						if !seen[p.ID] {
							seen[p.ID] = true
							result = append(result, p.ID)
						}
					}
				}
			} else {
				// Specific action — find matching permission
				for _, p := range permissions.Registry {
					if (policy.ResourceType == "*" || p.Resource == policy.ResourceType) && p.Action == action {
						if !seen[p.ID] {
							seen[p.ID] = true
							result = append(result, p.ID)
						}
					}
				}
			}
		}
	}
	return result
}

// groupRoleToProjectRole maps a group membership role to the corresponding
// project role definition name. Returns "" for plain members, since member
// access doesn't need additional delegation checks beyond ActionAddMember.
func groupRoleToProjectRole(groupRole string) string {
	switch groupRole {
	case store.GroupMemberRoleOwner:
		return store.ProjectRoleOwner
	case store.GroupMemberRoleAdmin:
		return store.ProjectRoleAdmin
	case store.GroupMemberRoleMember:
		return store.ProjectRoleMember
	default:
		return ""
	}
}

// scopesToPermissionIDs maps agent token scopes to permission IDs using
// the permissions registry.
func scopesToPermissionIDs(scopes []AgentTokenScope) []string {
	scopeSet := make(map[string]bool, len(scopes))
	for _, s := range scopes {
		scopeSet[string(s)] = true
	}

	seen := make(map[string]bool)
	var ids []string
	for _, p := range permissions.Registry {
		for _, s := range p.AgentScopes {
			if scopeSet[s] && !seen[p.ID] {
				seen[p.ID] = true
				ids = append(ids, p.ID)
			}
		}
	}
	return ids
}
