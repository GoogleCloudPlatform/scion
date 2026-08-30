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
	"sort"
	"time"
)

// Scope type constants used by the kernel. These mirror the store constants but
// are redeclared here so the kernel package has no store dependency.
const (
	ScopeTypeSystem  = "system"
	ScopeTypeProject = "project"
)

// CandidateBinding is a pre-resolved role binding presented to the kernel for
// evaluation. The store layer resolves these from the database; the kernel
// never queries a store.
type CandidateBinding struct {
	// BindingID is the unique identifier of this role binding.
	BindingID string

	// RoleDefinitionID references the role whose permissions this binding grants.
	RoleDefinitionID string

	// PrincipalType is "user", "agent", or "group".
	PrincipalType string

	// PrincipalID is the ID of the bound principal.
	PrincipalID string

	// ScopeType is "system" or "project".
	ScopeType string

	// ScopeID is empty for system-scoped bindings, or a project ID.
	ScopeID string

	// NotBefore is the earliest time this binding is active. Zero means
	// no lower bound.
	NotBefore time.Time

	// ExpiresAt is the time after which this binding is inactive. Zero
	// means no expiration.
	ExpiresAt time.Time
}

// RolePermissions maps a role definition to its permission set. It is a
// resolved snapshot — the kernel does not query role definitions from a store.
type RolePermissions struct {
	// RoleID is the unique identifier of the role definition.
	RoleID string

	// RoleName is the human-readable name.
	RoleName string

	// ScopeType is "system" or "project".
	ScopeType string

	// Permissions is the set of canonical permission IDs this role grants.
	Permissions map[string]struct{}
}

// HasPermission returns true if the role contains the given permission ID.
func (rp *RolePermissions) HasPermission(permissionID string) bool {
	if rp == nil {
		return false
	}
	_, ok := rp.Permissions[permissionID]
	return ok
}

// NewRolePermissions constructs a RolePermissions from a list of permission IDs.
func NewRolePermissions(roleID, roleName, scopeType string, perms []string) *RolePermissions {
	m := make(map[string]struct{}, len(perms))
	for _, p := range perms {
		m[p] = struct{}{}
	}
	return &RolePermissions{
		RoleID:      roleID,
		RoleName:    roleName,
		ScopeType:   scopeType,
		Permissions: m,
	}
}

// Restriction represents an intrinsic restriction that can only subtract from
// granted authority. Examples: credential caveats, delegation ceilings,
// principal suspension.
type Restriction struct {
	// Kind identifies the restriction type: "credential_scope",
	// "delegation_ceiling", "suspension".
	Kind string

	// Description is a human-readable explanation.
	Description string

	// Check returns true if the given permission is ALLOWED by this
	// restriction. Returning false means the restriction removes the
	// permission. A nil Check function is treated as "allow everything"
	// (the restriction has no effect).
	Check func(permissionID string) bool
}

// ResourceContext describes the target resource being authorized. The kernel
// uses this for scope containment checks.
type ResourceContext struct {
	// ResourceType is the type of the target resource (e.g. "agent", "project").
	ResourceType string

	// ResourceID is the unique identifier of the target resource.
	ResourceID string

	// OwnerID is the resource's owner user ID, if applicable.
	OwnerID string

	// ProjectID is the authoritative project that owns this resource.
	// Used for scope containment: a project-scoped binding matches only
	// if its ScopeID equals this value.
	ProjectID string

	// Ancestry is the ordered ancestor chain [root, ..., parent].
	Ancestry []string
}

// KernelRequest contains all resolved inputs for a single authorization
// decision. The kernel is a pure function over these inputs — it has no
// database calls, no side effects, and injectable time.
type KernelRequest struct {
	// Permission is the canonical permission ID being checked (e.g.
	// "agent.create").
	Permission string

	// PrincipalClosure is the set of principal IDs to consider. It
	// contains the direct principal and all transitive group memberships.
	// The map key is the principal ID.
	PrincipalClosure map[string]struct{}

	// MembershipPaths maps each principal ID in the closure to the
	// membership chain that connects the requesting principal to it.
	// For the direct principal, this is a single-element slice.
	// For a group, it is [requesting principal, ..., group].
	MembershipPaths map[string][]string

	// Resource describes the target resource.
	Resource ResourceContext

	// CandidateBindings are the pre-resolved role bindings to evaluate.
	CandidateBindings []CandidateBinding

	// RoleDefinitions maps role definition ID to its resolved permission set.
	RoleDefinitions map[string]*RolePermissions

	// Restrictions are intrinsic restrictions applied after the grant union.
	Restrictions []Restriction

	// Now is the evaluation time. It is injectable for deterministic testing.
	Now time.Time
}

// KernelDecision is the output of the Evaluate function.
type KernelDecision struct {
	// Allowed is true if the requested permission is granted after all
	// restrictions are applied.
	Allowed bool

	// Provenance contains the full decision trace.
	Provenance KernelProvenance
}

// Evaluate is the pure authorization kernel. It determines whether the
// requested permission is granted given the resolved inputs.
//
// The evaluation follows a fixed order:
//  1. Identify active, applicable bindings from the candidate set.
//  2. Union the permissions from all active bindings.
//  3. Apply restrictions (credential caveats, delegation ceilings, suspension).
//  4. Return allow/deny with full provenance.
//
// Design invariants:
//   - Default deny: no matching bindings = deny.
//   - A false activation condition suppresses only that binding's grant.
//   - Restrictions can only subtract, never add authority.
//   - Owner/admin roles pass through restrictions like any other grant.
//   - Missing attributes and errors fail closed.
func Evaluate(req KernelRequest) KernelDecision {
	if req.Permission == "" {
		return denyDecision(req.Permission, "no permission specified")
	}
	if len(req.PrincipalClosure) == 0 {
		return denyDecision(req.Permission, "empty principal closure")
	}

	// Phase 1: Identify active, applicable bindings and union their permissions.
	effectivePermissions := make(map[string]struct{})
	var grantingBindings []GrantProvenance
	var rejectedCandidates []GrantProvenance

	for i := range req.CandidateBindings {
		cb := &req.CandidateBindings[i]
		prov := evaluateBinding(req, cb)

		if prov.Contributed {
			grantingBindings = append(grantingBindings, prov)
			// Union the role's permissions into the effective set.
			role := req.RoleDefinitions[cb.RoleDefinitionID]
			if role != nil {
				for perm := range role.Permissions {
					effectivePermissions[perm] = struct{}{}
				}
			}
		} else {
			rejectedCandidates = append(rejectedCandidates, prov)
		}
	}

	// Phase 2: Check if the requested permission is in the unioned set
	// before applying restrictions.
	_, hasPermission := effectivePermissions[req.Permission]

	// Phase 3: Apply restrictions. Restrictions can only subtract.
	var restrictions []RestrictionResult
	for _, r := range req.Restrictions {
		rr := RestrictionResult{
			Kind:        r.Kind,
			Description: r.Description,
		}
		if r.Check != nil && !r.Check(req.Permission) {
			rr.Applied = true
			rr.Detail = "permission removed by " + r.Kind
			hasPermission = false
			// Remove from effective permissions too.
			delete(effectivePermissions, req.Permission)
		}
		restrictions = append(restrictions, rr)
	}

	// Build effective permissions list for provenance.
	effectivePermsList := sortedKeys(effectivePermissions)

	// Phase 4: Build the decision.
	prov := KernelProvenance{
		Permission:           req.Permission,
		Granted:              hasPermission,
		GrantingBindings:     grantingBindings,
		RejectedCandidates:   rejectedCandidates,
		Restrictions:         restrictions,
		EffectivePermissions: effectivePermsList,
	}

	if !hasPermission {
		reasons := buildDenyReasons(grantingBindings, rejectedCandidates, restrictions, req.Permission, effectivePermissions)
		prov.DenyReasons = reasons
	}

	return KernelDecision{
		Allowed:    hasPermission,
		Provenance: prov,
	}
}

// evaluateBinding checks whether a single candidate binding contributes to
// the effective permission set. It checks principal closure membership,
// activation conditions, scope containment, and role permission content.
func evaluateBinding(req KernelRequest, cb *CandidateBinding) GrantProvenance {
	role := req.RoleDefinitions[cb.RoleDefinitionID]

	prov := GrantProvenance{
		BindingID:     cb.BindingID,
		PrincipalID:   cb.PrincipalID,
		PrincipalType: cb.PrincipalType,
		ScopeType:     cb.ScopeType,
		ScopeID:       cb.ScopeID,
	}
	if role != nil {
		prov.RoleID = role.RoleID
		prov.RoleName = role.RoleName
		prov.Permissions = sortedKeys(role.Permissions)
	}

	// Set membership path from the closure.
	if paths, ok := req.MembershipPaths[cb.PrincipalID]; ok {
		prov.MembershipPath = paths
	} else {
		prov.MembershipPath = []string{cb.PrincipalID}
	}

	// Check 1: Is the binding's principal in the principal closure?
	if _, inClosure := req.PrincipalClosure[cb.PrincipalID]; !inClosure {
		prov.RejectReasons = append(prov.RejectReasons, "principal not in closure")
		return prov
	}

	// Check 2: Is the role definition known?
	if role == nil {
		prov.RejectReasons = append(prov.RejectReasons, "unknown role definition: "+cb.RoleDefinitionID)
		return prov
	}

	// Check 3: Activation conditions (notBefore / expiresAt).
	activation := evaluateActivation(cb, req.Now)
	prov.ActivationResult = activation
	if !activation.Active {
		if !activation.NotBeforeSatisfied {
			prov.RejectReasons = append(prov.RejectReasons, "binding not yet active (notBefore)")
		}
		if !activation.ExpiresAtSatisfied {
			prov.RejectReasons = append(prov.RejectReasons, "binding expired (expiresAt)")
		}
		return prov
	}

	// Check 4: Scope containment.
	if !scopeApplies(cb, req.Resource) {
		prov.RejectReasons = append(prov.RejectReasons, "scope does not apply to target resource")
		return prov
	}

	// Check 5: Does the role contain the requested permission?
	// Note: Even if the role doesn't contain the specific requested permission,
	// the binding may still contribute OTHER permissions to the effective set.
	// For the kernel, we mark it as contributing if the role has any permissions
	// (since we union all permissions from active bindings).
	if len(role.Permissions) > 0 {
		prov.Contributed = true
	}

	return prov
}

// evaluateActivation checks a binding's time-based activation conditions.
func evaluateActivation(cb *CandidateBinding, now time.Time) ActivationResult {
	result := ActivationResult{
		NotBefore: cb.NotBefore,
		ExpiresAt: cb.ExpiresAt,
	}

	// notBefore: satisfied if zero or now >= notBefore.
	result.NotBeforeSatisfied = cb.NotBefore.IsZero() || !now.Before(cb.NotBefore)

	// expiresAt: satisfied if zero or now < expiresAt.
	result.ExpiresAtSatisfied = cb.ExpiresAt.IsZero() || now.Before(cb.ExpiresAt)

	result.Active = result.NotBeforeSatisfied && result.ExpiresAtSatisfied
	return result
}

// scopeApplies returns true if the binding's scope covers the target resource.
//   - System scope applies to everything.
//   - Project scope applies only when the binding's ScopeID matches the
//     resource's authoritative ProjectID.
func scopeApplies(cb *CandidateBinding, resource ResourceContext) bool {
	switch cb.ScopeType {
	case ScopeTypeSystem:
		return true
	case ScopeTypeProject:
		// A project-scoped binding applies when its ScopeID matches the
		// resource's project. An empty resource ProjectID means the scope
		// does not apply (fail closed).
		if resource.ProjectID == "" {
			return false
		}
		return cb.ScopeID == resource.ProjectID
	default:
		// Unknown scope type: fail closed.
		return false
	}
}

// buildDenyReasons constructs a human-readable list of reasons for a deny
// decision.
func buildDenyReasons(
	granting []GrantProvenance,
	rejected []GrantProvenance,
	restrictions []RestrictionResult,
	permission string,
	effectivePerms map[string]struct{},
) []string {
	var reasons []string

	// Check if the permission was granted by some binding but removed by
	// a restriction.
	restrictionApplied := false
	for _, r := range restrictions {
		if r.Applied {
			restrictionApplied = true
			reasons = append(reasons, "restriction removed permission: "+r.Kind+" - "+r.Detail)
		}
	}

	if !restrictionApplied {
		if len(granting) == 0 && len(rejected) == 0 {
			reasons = append(reasons, "no candidate bindings")
		} else if len(granting) == 0 {
			reasons = append(reasons, "no active binding grants permission \""+permission+"\"")
		} else {
			// Some bindings contributed but none granted this specific permission.
			reasons = append(reasons, "active bindings do not include permission \""+permission+"\"")
		}
	}

	return reasons
}

// denyDecision returns a KernelDecision with Allowed=false and the given reason.
func denyDecision(permission string, reason string) KernelDecision {
	return KernelDecision{
		Allowed: false,
		Provenance: KernelProvenance{
			Permission:  permission,
			Granted:     false,
			DenyReasons: []string{reason},
		},
	}
}

// sortedKeys returns the keys of a map in sorted order.
func sortedKeys(m map[string]struct{}) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
