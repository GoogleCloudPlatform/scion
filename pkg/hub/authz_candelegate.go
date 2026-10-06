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
//   - Role binding creation (admin): wired in handlers_roles.go:createRoleBinding.
//   - Project membership: wired in handlers_project_members.go (add/update/delete).
//   - Group membership: wired in handlers_groups.go:authorizeGroupMemberGrant
//     (addGroupMember, and createGroup when a parent group is given).
//   - Agent delegation: wired in handlers_agents_core.go:createAgentInProject.
//   - Scheduled dispatch: wired in server.go:authorizeScheduledAgentCreate.
//   - Policy routes: removed in CO1 cutover.
//   - Custom role definition create/update: wired in handlers_roles.go:createRoleDefinition.

// GrantType identifies the kind of authority being delegated.
type GrantType string

const (
	GrantTypeRoleBinding       GrantType = "role_binding"
	GrantTypeGroupMembership   GrantType = "group_membership"
	GrantTypeAgentDelegation   GrantType = "agent_delegation"
	GrantTypeCustomRole        GrantType = "custom_role"
	GrantTypeProjectMembership GrantType = "project_membership"
)

// GrantDescriptor describes authority being created or modified.
type GrantDescriptor struct {
	// Type of grant being created.
	Type GrantType

	// For role bindings: the role definition being bound.
	RoleDefinitionID string
	RolePermissions  []string // resolved permissions if available

	// For group membership: the target group.
	GroupID string

	// For agent delegation: the role/scopes being delegated.
	AgentRole   string
	AgentScopes []AgentTokenScope
	ProjectID   string // target project

	// For custom roles: the permissions being defined.
	CustomRolePermissions []string

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

	// A delivery credential cannot delegate any permission. The check keys
	// on the concrete type and comes before every allow path below,
	// including the AgentRoleNone allow for an agent delegation with no
	// scopes.
	if _, ok := actor.(*hubDeliveryIdentity); ok {
		return Decision{Allowed: false, Reason: "delivery credential cannot delegate"}
	}

	// Scoped credentials (UAT) can only delegate within their credential scope.
	if scoped, ok := actor.(*ScopedUserIdentity); ok {
		if denied := a.enforceUATDelegation(ctx, scoped, grant); denied != nil {
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
	case GrantTypeProjectMembership:
		return a.canDelegateProjectMembership(ctx, actor, grant)
	default:
		return Decision{Allowed: false, Reason: "unknown grant type: " + string(grant.Type)}
	}
}

// enforceUATDelegation checks that a scoped UAT credential only delegates
// within its token boundary. It is the boundary half of CanDelegate for a
// UAT; the permission half is the ceiling intersection in
// actorHoldsAllPermissions, which every type-specific check applies.
//
//   - A grant with no scope type (group membership) is not confined here.
//     canDelegateGroupMembership denies system-scoped group authority to
//     every scoped credential.
//   - A system-scoped grant is denied for every UAT, of either boundary
//     kind.
//   - A grant with any other non-project scope type is denied.
//   - A project-scoped grant must name its project, and the token boundary
//     must allow that project (BoundaryAllows): a project boundary allows
//     only its own project, a hub boundary allows any project.
//   - An agent delegation must name the same project in ProjectID and
//     ScopeID, for every boundary kind, because the authority check runs
//     on ProjectID and the boundary check runs on ScopeID.
//   - Under a hub boundary the holder must also currently have access to
//     that project (ProjectTargetAdmission), for the permission of the
//     operation that creates the grant, on that project. The boundary
//     alone never admits a project.
func (a *AuthzService) enforceUATDelegation(ctx context.Context, scoped *ScopedUserIdentity, grant GrantDescriptor) *Decision {
	deny := func(reason string) *Decision {
		return &Decision{Allowed: false, Reason: reason}
	}
	if scoped == nil {
		return deny("scoped credential is missing")
	}

	switch grant.ScopeType {
	case "":
		return nil
	case store.RoleScopeSystem:
		return deny("scoped credential cannot create system-scoped grants")
	case store.RoleScopeProject:
	default:
		return deny("scoped credential cannot delegate a grant with an unknown scope type")
	}

	if grant.ScopeID == "" {
		return deny("scoped credential cannot delegate a project grant that names no project")
	}
	if grant.Type == GrantTypeAgentDelegation && grant.ProjectID != grant.ScopeID {
		return deny("scoped credential cannot delegate an agent grant that names two projects")
	}
	boundary := scoped.Boundary()
	if !BoundaryAllows(boundary, TargetScope{Kind: TargetScopeProject, ProjectID: grant.ScopeID}) {
		return deny("scoped credential cannot delegate outside its project")
	}
	if boundary.Kind != BoundaryKindHub {
		return nil
	}

	permissionID, target, ok := uatDelegationAdmissionTarget(grant)
	if !ok {
		return deny("scoped credential cannot delegate this grant type into a project")
	}
	admission, err := a.ProjectTargetAdmission(ctx, principalContextForIdentity(scoped), grant.ScopeID, permissionID, target, nil)
	if err != nil || !admission.Admitted {
		return deny("scoped credential requires current access to the grant's project")
	}
	return nil
}

// uatDelegationAdmissionTarget names the permission and the project target
// on which a hub-boundary UAT's project access is checked before it may
// create grant. The permission is the one the operation creating the grant
// enforces:
//
//   - an agent delegation is created by agent creation, which enforces
//     agent.create on an agent in the project;
//   - a project membership, or a project role binding for a built-in
//     project role, is created by the project membership routes, which
//     enforce project.manage on the project;
//   - a project role binding for a custom role is admitted on
//     project.manage on the project as its project-level access check. Its
//     route additionally enforces role_binding.create at hub level.
//
// Any other grant type has no project admission target, and ok is false.
// An agent delegation whose ProjectID differs from its ScopeID names two
// projects, and ok is false; enforceUATDelegation denies that case before
// it gets here.
func uatDelegationAdmissionTarget(grant GrantDescriptor) (permissionID string, target Resource, ok bool) {
	switch grant.Type {
	case GrantTypeAgentDelegation:
		if grant.ProjectID != grant.ScopeID {
			return "", Resource{}, false
		}
		return "agent.create", Resource{Type: "agent", ParentType: "project", ParentID: grant.ScopeID}, true
	case GrantTypeRoleBinding, GrantTypeProjectMembership:
		return "project.manage", Resource{Type: "project", ID: grant.ScopeID}, true
	default:
		return "", Resource{}, false
	}
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
// a group.
//
// RS1 D3 approved rule: group membership mutations are governed solely by
// group roles and bindings. The previous cross-domain project-authority scan
// (checking that the actor held all project permissions inherited through
// group bindings) has been removed per the approved governance appendix.
// Project-level delegation is governed at the point of assigning a project
// role to a group (via ProjectMembershipService), not at the point of
// managing group members.
//
// The check now covers only:
//   - System-scoped role bindings on the group closure: a scoped UAT must
//     not delegate system-scoped authority through group membership.
//   - System-scoped non-amplification: if the group inherits system-scoped
//     bindings, the actor must hold those permissions.
//
// Project-scoped bindings on the group are explicitly excluded from this
// check per D3.
func (a *AuthzService) canDelegateGroupMembership(ctx context.Context, actor Identity, grant GrantDescriptor) Decision {
	groupID := grant.GroupID
	if groupID == "" {
		return Decision{Allowed: true, Reason: "no group specified"}
	}

	// Build the principal closure of the target group: the group itself plus
	// all ancestor groups that transitively contain it.
	groupPrincipals := []store.PrincipalRef{
		{Type: store.RoleBindingPrincipalGroup, ID: groupID},
	}

	parentGroups, err := a.store.GetParentGroups(ctx, groupID)
	if err != nil {
		a.logger.Warn("failed to resolve parent groups for delegation check",
			"group_id", groupID, "error", err)
		return Decision{Allowed: false, Reason: "cannot resolve parent group closure"}
	}
	for _, pgID := range parentGroups {
		groupPrincipals = append(groupPrincipals, store.PrincipalRef{
			Type: store.RoleBindingPrincipalGroup,
			ID:   pgID,
		})
	}

	// Fetch ALL role bindings for the group closure in one batched query.
	bindings, err := a.store.ListRoleBindingsForPrincipals(ctx, groupPrincipals, nil, nil)
	if err != nil {
		a.logger.Warn("failed to list role bindings for group closure",
			"group_id", groupID, "error", err)
		return Decision{Allowed: false, Reason: "cannot resolve group role bindings"}
	}

	// If the group (and its ancestors) have no role bindings, no authority
	// is being delegated through this membership addition.
	if len(bindings) == 0 {
		return Decision{Allowed: true, Reason: "group has no role bindings; no authority delegation required"}
	}

	// RS1 D3: filter to system-scoped bindings only. Project-scoped bindings
	// are excluded because project-level delegation is governed at role
	// assignment time (ProjectMembershipService), not at group membership time.
	var systemBindings []*store.RoleBinding
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeSystem {
			systemBindings = append(systemBindings, b)
		}
	}

	// R4 gap (a): UAT scope bypass on group membership. If any binding on
	// the group closure is system-scoped, a project-scoped UAT must not be
	// allowed to delegate that authority.
	if scoped, ok := actor.(*ScopedUserIdentity); ok && scoped != nil {
		if len(systemBindings) > 0 {
			return Decision{
				Allowed: false,
				Reason:  "scoped credential cannot delegate system-scoped group authority",
			}
		}
	}

	// If no system-scoped bindings, group membership is purely group-governed
	// per D3. Allow the delegation.
	if len(systemBindings) == 0 {
		return Decision{Allowed: true, Reason: "RS1 D3: group membership governed by group roles; no system-scoped authority to delegate"}
	}

	// For system-scoped bindings, verify the actor holds each permission.
	type scopeKey struct {
		scopeType string
		scopeID   string
	}
	permsByScope := make(map[scopeKey]map[string]struct{})
	rdCache := make(map[string]*store.RoleDefinition)

	for _, b := range systemBindings {
		rd, ok := rdCache[b.RoleDefinitionID]
		if !ok {
			var rdErr error
			rd, rdErr = a.store.GetRoleDefinition(ctx, b.RoleDefinitionID)
			if rdErr != nil {
				a.logger.Warn("failed to resolve role definition for group binding",
					"binding_id", b.ID, "role_definition_id", b.RoleDefinitionID, "error", rdErr)
				return Decision{Allowed: false, Reason: "cannot resolve role definition for group binding"}
			}
			if rd == nil {
				a.logger.Warn("role definition not found for group binding",
					"binding_id", b.ID, "role_definition_id", b.RoleDefinitionID)
				return Decision{Allowed: false, Reason: "role definition not found for group binding"}
			}
			rdCache[b.RoleDefinitionID] = rd
		}
		sk := scopeKey{scopeType: b.ScopeType, scopeID: b.ScopeID}
		if permsByScope[sk] == nil {
			permsByScope[sk] = make(map[string]struct{})
		}
		for _, perm := range rd.Permissions {
			permsByScope[sk][perm] = struct{}{}
		}
	}

	for sk, perms := range permsByScope {
		permList := make([]string, 0, len(perms))
		for p := range perms {
			permList = append(permList, p)
		}
		decision := a.actorHoldsAllPermissions(ctx, actor, permList, sk.scopeType, sk.scopeID)
		if !decision.Allowed {
			return Decision{
				Allowed: false,
				Reason:  "actor cannot delegate inherited system-scoped group authority: " + decision.Reason,
			}
		}
	}

	return Decision{Allowed: true, Reason: "actor holds all system-scoped permissions inherited through group membership"}
}

// canDelegateAgent checks whether the actor holds all scopes/permissions
// that the new agent would receive.
func (a *AuthzService) canDelegateAgent(ctx context.Context, actor Identity, grant GrantDescriptor) Decision {
	// For agent callers: check scope-by-scope.
	if agentActor, ok := actor.(AgentIdentity); ok {
		return a.canAgentDelegateToAgent(agentActor, grant)
	}

	// For user callers: verify the user has agent-create authority in the
	// target project. Agent creation already caps the effective role to the
	// project maximum (and, for agent callers, the parent agent's role).
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
//
// Compares against effectiveAgentScopes(agentActor), not agentActor.Scopes()
// directly, so a verified legacy token (isLegacyPreSplitAgentJWT,
// ptone/scion#2339) is judged by the same effective scopes the synthetic
// grant and the credential-scope restriction use, rather than its literal,
// pre-split scope list.
func (a *AuthzService) canAgentDelegateToAgent(agentActor AgentIdentity, grant GrantDescriptor) Decision {
	// Resolve the requested role to scopes.
	// A role's ceilingOptionalRoleScopes are not required of the actor: the
	// child's ceiling is derived from the parent's ceiling-filtered role
	// coverage (agentSourceEffectCeiling), so the mint filter leaves such a
	// scope out of the child's tokens whenever the parent's chain does not
	// allow it. Scopes requested explicitly (grant.AgentScopes) are always
	// required.
	requestedRole := AgentRole(grant.AgentRole)
	var requestedScopes []AgentTokenScope
	for _, s := range ScopesForRole(requestedRole) {
		if !ceilingOptionalRoleScopes[s] {
			requestedScopes = append(requestedScopes, s)
		}
	}
	requestedScopes = append(requestedScopes, grant.AgentScopes...)

	actorScopes := effectiveAgentScopes(agentActor)
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

// canDelegateProjectMembership checks whether the actor can manage members in
// a project. RS1: relaxed from C0 owner-only to the CT1 D5 governance matrix.
// Both project-owner and project-admin (direct or group-derived) may manage
// membership. Target-role governance is enforced by ProjectMembershipService,
// not here — this gate only checks base membership management authority.
func (a *AuthzService) canDelegateProjectMembership(ctx context.Context, actor Identity, grant GrantDescriptor) Decision {
	if grant.ProjectID == "" {
		return Decision{Allowed: false, Reason: "project membership requires a project ID"}
	}
	userID := actor.ID()
	if a.isProjectOwner(ctx, userID, grant.ProjectID) {
		return Decision{Allowed: true, Reason: "project owner can manage membership"}
	}
	if a.isProjectOwnerOrAdmin(ctx, userID, grant.ProjectID) {
		return Decision{Allowed: true, Reason: "project admin can manage membership (RS1 governance matrix)"}
	}
	return Decision{Allowed: false, Reason: "only project owners and admins can manage project membership"}
}

// actorHoldsAllPermissions resolves the actor's effective permissions in the
// given scope and checks that every permission in targetPerms is held.
// Permissions come from role bindings. Credential caveats (UAT project scope,
// agent JWT scopes) are intersected with the resolved permissions so that
// delegation checks enforce the same restrictions as normal CheckAccess.
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

	// R4 gap (b): Intersect resolved permissions with credential caveats.
	// This mirrors the restrictions built in CheckAccess (steps 7a/7b) so
	// that delegation checks honour the same credential scope boundaries.
	actorPerms = a.intersectCredentialCaveats(actor, actorPerms)

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

// intersectCredentialCaveats filters a permission set through the credential
// restrictions carried by the actor's identity. If the actor has no credential
// caveats (full session), the permissions are returned unchanged.
func (a *AuthzService) intersectCredentialCaveats(actor Identity, perms []string) []string {
	var restriction *Restriction

	switch v := actor.(type) {
	case *ScopedUserIdentity:
		if v != nil {
			// Always apply the ceiling restriction, even when it has no
			// permission IDs at all: an empty or malformed ceiling denies
			// delegation of every permission rather than lifting the
			// restriction. CanDelegate applies the same ceiling as Decide;
			// no scope implies another.
			r := ceilingRestriction(v.Ceiling())
			restriction = &r
		}
	case AgentIdentity:
		// Zero Resource: CanDelegate reasons about a target-agnostic
		// permission set, never one resource instance, so the
		// gcp_service_account.use per-SA match (agentScopeRestriction's one
		// resource-aware exception) can never engage here, so
		// gcp_service_account.use is always filtered out of an agent's
		// delegable set.
		r := agentScopeRestriction(v, Resource{})
		restriction = &r
	}

	if restriction == nil || restriction.Check == nil {
		return perms
	}

	filtered := make([]string, 0, len(perms))
	for _, p := range perms {
		if restriction.Check(p) {
			filtered = append(filtered, p)
		}
	}
	return filtered
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
