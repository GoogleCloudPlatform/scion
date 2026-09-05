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

import "time"

// GrantProvenance records why a particular binding contributed (or failed to
// contribute) a permission. Every evaluated candidate produces one of these,
// whether it granted or was rejected.
type GrantProvenance struct {
	// BindingID is the ID of the RoleBinding that was evaluated.
	BindingID string

	// RoleID is the ID of the RoleDefinition the binding references.
	RoleID string

	// RoleName is the human-readable name of the RoleDefinition.
	RoleName string

	// ScopeType is "system" or "project".
	ScopeType string

	// ScopeID is empty for system scope, or a project ID.
	ScopeID string

	// PrincipalID is the principal on the binding (may be a group).
	PrincipalID string

	// PrincipalType is "user", "agent", or "group".
	PrincipalType string

	// MembershipPath describes how the requesting principal reaches this
	// binding's principal. For direct bindings this is a single-element
	// slice containing the principal ID. For group-derived bindings, this
	// is the chain [requesting principal, ..., bound group].
	MembershipPath []string

	// Contributed is true if this binding successfully contributed
	// permissions to the effective set. False means it was a candidate
	// but was rejected for one of the reasons recorded below.
	Contributed bool

	// ContainsRequested is true when the binding's role contains the
	// specific requested permission (not just any permission). R-4 fix:
	// used by kernelDecisionToDecision to select the correct granting
	// binding for Decision.BindingID / RoleName / audit output.
	ContainsRequested bool

	// ActivationResult records the outcome of evaluating the binding's
	// activation conditions (notBefore, expiresAt).
	ActivationResult ActivationResult

	// Permissions lists the permissions this binding's role contains.
	// Populated for provenance/explain output.
	Permissions []string

	// RejectReasons lists the reasons this binding did not contribute,
	// when Contributed is false. Empty when Contributed is true.
	RejectReasons []string
}

// ActivationResult records the evaluation of a binding's time-based
// activation conditions.
type ActivationResult struct {
	// Active is true when all activation conditions are satisfied.
	Active bool

	// NotBefore is the binding's earliest activation time. Zero means no
	// lower bound.
	NotBefore time.Time

	// ExpiresAt is the binding's expiration time. Zero means no expiration.
	ExpiresAt time.Time

	// NotBeforeSatisfied is true when the current time is at or after NotBefore
	// (or NotBefore is zero).
	NotBeforeSatisfied bool

	// ExpiresAtSatisfied is true when the current time is before ExpiresAt
	// (or ExpiresAt is zero).
	ExpiresAtSatisfied bool

	// FailClosedReason is set when the activation check fails closed due to
	// missing evaluation context (e.g. zero Now with time-conditioned binding).
	FailClosedReason string
}

// RestrictionResult records how a single restriction reduced (or did not
// reduce) the candidate permission set.
type RestrictionResult struct {
	// Kind describes the restriction type. Examples: "credential_scope",
	// "delegation_ceiling", "suspension".
	Kind string

	// Description is a human-readable explanation of the restriction.
	Description string

	// Applied is true if this restriction actually removed the requested
	// permission from the candidate set.
	Applied bool

	// Detail gives specifics: e.g. "UAT scoped to project X, permission
	// not in UAT scopes" or "principal suspended".
	Detail string

	// BoundaryName is the name of the access constraint (boundary) that
	// imposed this restriction.
	BoundaryName string

	// BoundaryID is the ID of the access constraint (boundary).
	BoundaryID string

	// BoundaryScopeType is the scope type ("system", "project").
	BoundaryScopeType string

	// BoundaryScopeID is the scope ID (e.g., project ID).
	BoundaryScopeID string
}

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
}

// KernelProvenance is the full provenance record for a single Evaluate call.
// It explains the complete decision: which candidates were considered, which
// contributed, which restrictions were evaluated, and why the final decision
// was allow or deny.
type KernelProvenance struct {
	// Permission is the canonical permission ID that was checked.
	Permission string

	// Granted is true if the permission was ultimately allowed.
	Granted bool

	// GrantingBindings lists the bindings that contributed the permission
	// before restrictions were applied. Empty for deny decisions.
	GrantingBindings []GrantProvenance

	// RejectedCandidates lists bindings that were considered but did not
	// contribute. Each entry explains why it was rejected.
	RejectedCandidates []GrantProvenance

	// Restrictions lists every restriction that was evaluated. Even
	// restrictions that did not apply are recorded for explain output.
	Restrictions []RestrictionResult

	// DenyReasons summarizes why a deny decision was reached. Empty for
	// allow decisions.
	DenyReasons []string

	// EffectivePermissions lists all permissions the principal holds after
	// unioning all active grants and applying all restrictions. This is
	// the full effective authority, not just the single checked permission.
	EffectivePermissions []string
}
