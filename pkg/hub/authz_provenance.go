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

// RestrictionProvenance records detailed metadata about a restriction source,
// including the boundary or credential that imposed it.
type RestrictionProvenance struct {
	// Kind is the restriction type: "access_constraint", "credential_scope",
	// "delegation_ceiling", "suspension".
	Kind string `json:"kind"`

	// Description is a human-readable explanation.
	Description string `json:"description"`

	// Applied is true if this restriction actually removed the requested
	// permission.
	Applied bool `json:"applied"`

	// Detail gives specifics about why the restriction applied or not.
	Detail string `json:"detail,omitempty"`

	// BoundaryName is the name of the access constraint (boundary) that
	// imposed this restriction. Empty for non-boundary restrictions.
	BoundaryName string `json:"boundaryName,omitempty"`

	// BoundaryID is the ID of the access constraint (boundary).
	BoundaryID string `json:"boundaryId,omitempty"`

	// BoundaryScope describes the scope of the boundary (e.g., "system",
	// "project:proj-123").
	BoundaryScope string `json:"boundaryScope,omitempty"`
}

// GrantDetail carries provenance for a single grant (active or inactive)
// in the external-facing DecisionProvenance.
type GrantDetail struct {
	// BindingID is the role binding ID.
	BindingID string `json:"bindingId"`

	// RoleID is the role definition ID.
	RoleID string `json:"roleId"`

	// RoleName is the human-readable role name.
	RoleName string `json:"roleName"`

	// ScopeType is "system" or "project".
	ScopeType string `json:"scopeType"`

	// ScopeID is the project ID for project-scoped bindings.
	ScopeID string `json:"scopeId,omitempty"`

	// PrincipalType is the type of the bound principal.
	PrincipalType string `json:"principalType"`

	// PrincipalID is the ID of the bound principal.
	PrincipalID string `json:"principalId"`

	// ContainsRequested is true when this binding's role contains the
	// specific requested permission.
	ContainsRequested bool `json:"containsRequested"`

	// MembershipPath shows how the requesting principal reaches this
	// binding's principal.
	MembershipPath []string `json:"membershipPath,omitempty"`

	// Permissions lists the permissions this binding's role grants.
	Permissions []string `json:"permissions,omitempty"`

	// InactiveReason explains why this grant did not contribute,
	// when it is an inactive grant.
	InactiveReason string `json:"inactiveReason,omitempty"`

	// RejectReasons lists all the reasons this binding was rejected.
	RejectReasons []string `json:"rejectReasons,omitempty"`
}

// MembershipPathDetail describes a typed path from the requesting principal
// to a subject in the principal closure.
type MembershipPathDetail struct {
	// TargetID is the typed principal identifier (e.g., "group:engineers").
	TargetID string `json:"targetId"`

	// Path is the chain from the requesting principal through intermediate
	// groups to the target. Each element is a typed principal ID.
	Path []string `json:"path"`

	// Kind describes the match type: "direct", "group_membership", or
	// "group_closure".
	Kind string `json:"kind"`
}

// DecisionProvenance is the external-facing provenance record attached to a
// Decision. It carries enough detail for the explain API to show exactly why
// an authorization decision was reached.
type DecisionProvenance struct {
	// Permission is the canonical permission ID that was checked.
	Permission string `json:"permission"`

	// Grants lists the active grants that contributed to the allow set.
	Grants []GrantDetail `json:"grants"`

	// InactiveGrants lists grants that exist but did not contribute
	// (expired, wrong scope, principal not in closure, etc.).
	InactiveGrants []GrantDetail `json:"inactiveGrants"`

	// Restrictions lists every restriction that was evaluated, with full
	// boundary metadata where applicable.
	Restrictions []RestrictionProvenance `json:"restrictions"`

	// MembershipPaths describes the typed paths from the requesting principal
	// to each subject in the principal closure.
	MembershipPaths []MembershipPathDetail `json:"membershipPaths"`

	// StatusRestrictions lists credential and status-based reductions
	// (e.g., UAT scope, agent JWT scope, delegation ceiling).
	StatusRestrictions []RestrictionProvenance `json:"statusRestrictions,omitempty"`

	// Errors lists resolution errors that may have affected the outcome.
	Errors []string `json:"errors,omitempty"`

	// EffectivePermissions lists all permissions the principal holds after
	// applying all restrictions. Only populated when Explain=true.
	EffectivePermissions []string `json:"effectivePermissions,omitempty"`

	// DenyReasons summarizes why a deny decision was reached.
	DenyReasons []string `json:"denyReasons,omitempty"`

	// Relationships lists every relationship candidate evaluated for the
	// request, accepted or rejected. Only populated when Explain=true.
	Relationships []RelationshipCandidateResult `json:"relationships,omitempty"`

	// Actor and Purpose echo AuthzRequest.Actor/Purpose. Audit-only.
	Actor   *DecisionActor `json:"actor,omitempty"`
	Purpose string         `json:"purpose,omitempty"`
}
