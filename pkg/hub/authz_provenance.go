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

	// Contributed is true if this binding successfully contributed the
	// requested permission. False means it was a candidate but was
	// rejected for one of the reasons recorded below.
	Contributed bool

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
