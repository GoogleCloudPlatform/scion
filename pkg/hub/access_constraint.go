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
	"errors"
	"fmt"
	"time"
)

// ---------------------------------------------------------------------------
// Subject selector — three kinds only
// ---------------------------------------------------------------------------

// SubjectKind enumerates the permitted subject selector types.
type SubjectKind string

const (
	// SubjectKindPrincipal targets an exact user, agent, or group.
	SubjectKindPrincipal SubjectKind = "principal"

	// SubjectKindGroupClosure targets all effective members of a group.
	SubjectKindGroupClosure SubjectKind = "group_closure"

	// SubjectKindAllPrincipals targets every principal in the system.
	SubjectKindAllPrincipals SubjectKind = "all_principals"
)

// SubjectSelector identifies the principals affected by an access constraint.
// Exactly one of the three kinds must be selected.
type SubjectSelector struct {
	// Kind is the selector type.
	Kind SubjectKind `json:"kind"`

	// PrincipalType is "user", "agent", or "group". Required when Kind is
	// SubjectKindPrincipal.
	PrincipalType string `json:"principalType,omitempty"`

	// PrincipalID is the ID of the targeted principal. Required when Kind is
	// SubjectKindPrincipal.
	PrincipalID string `json:"principalId,omitempty"`

	// GroupID is the group whose effective membership closure is constrained.
	// Required when Kind is SubjectKindGroupClosure.
	GroupID string `json:"groupId,omitempty"`
}

// Validate checks that the subject selector is well-formed.
// It rejects orphaned fields: principal-specific fields must be empty when
// kind is group_closure or all_principals, and group-specific fields must be
// empty when kind is principal.
func (s SubjectSelector) Validate() error {
	switch s.Kind {
	case SubjectKindPrincipal:
		if s.PrincipalType == "" {
			return errors.New("principalType is required for principal subject")
		}
		if s.PrincipalType != "user" && s.PrincipalType != "agent" && s.PrincipalType != "group" {
			return fmt.Errorf("invalid principalType %q: must be user, agent, or group", s.PrincipalType)
		}
		if s.PrincipalID == "" {
			return errors.New("principalId is required for principal subject")
		}
		// Reject orphaned group field on principal kind.
		if s.GroupID != "" {
			return errors.New("groupId must be empty for principal subject")
		}
	case SubjectKindGroupClosure:
		if s.GroupID == "" {
			return errors.New("groupId is required for group_closure subject")
		}
		// Reject orphaned principal fields on group_closure kind.
		if s.PrincipalID != "" {
			return errors.New("principalId must be empty for group_closure subject")
		}
		if s.PrincipalType != "" {
			return errors.New("principalType must be empty for group_closure subject")
		}
	case SubjectKindAllPrincipals:
		// Reject orphaned fields on all_principals kind.
		if s.PrincipalID != "" {
			return errors.New("principalId must be empty for all_principals subject")
		}
		if s.PrincipalType != "" {
			return errors.New("principalType must be empty for all_principals subject")
		}
		if s.GroupID != "" {
			return errors.New("groupId must be empty for all_principals subject")
		}
	default:
		return fmt.Errorf("invalid subject kind %q: must be principal, group_closure, or all_principals", s.Kind)
	}
	return nil
}

// MatchesPrincipalClosure returns true if this subject selector matches any
// principal in the typed closure. The closure uses composite "type:id" keys
// (e.g. "user:u1", "group:g1", "agent:a1").
//
// For SubjectKindPrincipal, the constraint's PrincipalType and PrincipalID
// are compared against the typed closure entries — a constraint targeting
// {principal, group, G} matches when "group:G" is in the closure. This
// ensures consistency with the lockout guard and group-mutation gate, which
// both honor group-targeted principal constraints.
func (s SubjectSelector) MatchesPrincipalClosure(
	typedClosure map[string]struct{},
) bool {
	switch s.Kind {
	case SubjectKindPrincipal:
		// Look up the typed key: the constraint's principalType + principalID.
		key := s.PrincipalType + ":" + s.PrincipalID
		_, ok := typedClosure[key]
		return ok
	case SubjectKindGroupClosure:
		// A group_closure constraint matches if the group is in the principal's closure.
		key := "group:" + s.GroupID
		_, ok := typedClosure[key]
		return ok
	case SubjectKindAllPrincipals:
		return true
	default:
		return false
	}
}

// NormalizePrincipalType maps concrete principal kinds to the canonical types
// used in constraint subjects. Dev and federated variants resolve groups
// through the same paths as their base types and must be treated identically
// for constraint matching.
//
// This is the single canonical normalization — both Decide and ResolveListScopes
// must call this before comparing against constraint subjects.
func NormalizePrincipalType(t string) string {
	switch t {
	case "user", "dev", "federated_user":
		return "user"
	case "agent", "federated_agent":
		return "agent"
	default:
		return t
	}
}

// ---------------------------------------------------------------------------
// Scope reference
// ---------------------------------------------------------------------------

// ConstraintScopeRef identifies the scope of an access constraint.
type ConstraintScopeRef struct {
	// Type is "system" or "project".
	Type string `json:"type"`

	// ID is empty for system scope, or a project ID for project scope.
	ID string `json:"id,omitempty"`
}

// Validate checks that the scope reference is well-formed.
func (s ConstraintScopeRef) Validate() error {
	switch s.Type {
	case ScopeTypeSystem:
		// ID must be empty for system scope.
		if s.ID != "" {
			return errors.New("scope ID must be empty for system scope")
		}
	case ScopeTypeProject:
		if s.ID == "" {
			return errors.New("project ID is required for project scope")
		}
	default:
		return fmt.Errorf("invalid scope type %q: must be system or project", s.Type)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Constraint condition — typed time window only for v1
// ---------------------------------------------------------------------------

// ConstraintCondition represents a typed condition that controls when a
// constraint is active. In v1, only a time window is supported.
type ConstraintCondition struct {
	// NotBefore is the earliest time the constraint is active.
	// Zero means no lower bound (active immediately).
	NotBefore time.Time `json:"notBefore,omitempty"`

	// ExpiresAt is the time after which the constraint is no longer active.
	// Zero means no expiration (active indefinitely).
	ExpiresAt time.Time `json:"expiresAt,omitempty"`
}

// IsActive returns true if the constraint condition is currently active at
// the given evaluation time.
func (c ConstraintCondition) IsActive(now time.Time) bool {
	if !c.NotBefore.IsZero() && now.Before(c.NotBefore) {
		return false
	}
	if !c.ExpiresAt.IsZero() && !now.Before(c.ExpiresAt) {
		return false
	}
	return true
}

// IsActiveInMostRestrictiveState returns true if the constraint would be
// active at any future point. Used for lockout prevention: if the constraint
// has a future NotBefore, it will eventually become active, so the lockout
// check must consider it.
func (c ConstraintCondition) IsActiveInMostRestrictiveState(now time.Time) bool {
	// If already expired, it will never be active again.
	if !c.ExpiresAt.IsZero() && !now.Before(c.ExpiresAt) {
		return false
	}
	// Otherwise the constraint is or will become active.
	return true
}

// ---------------------------------------------------------------------------
// AccessConstraint — the core model
// ---------------------------------------------------------------------------

// AccessConstraint is a named maximum-permissions boundary. It can only
// reduce otherwise granted authority — it cannot create authority.
//
// Multiple constraints compose by intersection: a permission must be in
// ALL applicable constraints' MaximumPermissions to survive.
type AccessConstraint struct {
	// ID is the unique identifier.
	ID string `json:"id"`

	// Name is the human-readable name.
	Name string `json:"name"`

	// Subject identifies which principals are affected.
	Subject SubjectSelector `json:"subject"`

	// Scope is the scope at which this constraint applies.
	Scope ConstraintScopeRef `json:"scope"`

	// MaximumPermissions is the set of permission IDs that constrained
	// principals may hold. Any permission NOT in this set is denied.
	// A newly registered permission is outside this boundary until
	// explicitly added.
	MaximumPermissions []string `json:"maximumPermissions"`

	// Condition is the time window during which this constraint is active.
	Condition ConstraintCondition `json:"condition"`

	// Disabled indicates this constraint has been deactivated (e.g. by
	// offline recovery). It is not evaluated when disabled.
	Disabled bool `json:"disabled,omitempty"`

	// Revision is a monotonic counter incremented on every update.
	// Used for optimistic concurrency control.
	Revision int64 `json:"revision"`

	// Purpose is a human-readable description of why this constraint exists.
	Purpose string `json:"purpose,omitempty"`

	// UpdatedBy is the principal who last modified this constraint.
	UpdatedBy string `json:"updatedBy,omitempty"`

	// CreatedBy is the user ID of the creator.
	CreatedBy string `json:"createdBy"`

	// CreatedAt is the creation timestamp.
	CreatedAt time.Time `json:"createdAt"`

	// UpdatedAt is the last update timestamp.
	UpdatedAt time.Time `json:"updatedAt,omitempty"`

	// Degraded indicates the constraint record has invalid stored data.
	// Set by the conversion layer when validation fails on a stored record.
	// B7's ResolutionHealth uses this to report unhealthy constraints.
	Degraded bool `json:"degraded,omitempty"`
}

// Validate checks that the constraint is well-formed.
func (c *AccessConstraint) Validate() error {
	if c.Name == "" {
		return errors.New("name is required")
	}
	if err := c.Subject.Validate(); err != nil {
		return fmt.Errorf("invalid subject: %w", err)
	}
	if err := c.Scope.Validate(); err != nil {
		return fmt.Errorf("invalid scope: %w", err)
	}
	if len(c.MaximumPermissions) == 0 {
		return errors.New("maximumPermissions must contain at least one permission")
	}
	return nil
}

// MaximumPermissionSet returns the set of maximum permissions as a map for
// efficient lookup.
func (c *AccessConstraint) MaximumPermissionSet() map[string]struct{} {
	m := make(map[string]struct{}, len(c.MaximumPermissions))
	for _, p := range c.MaximumPermissions {
		m[p] = struct{}{}
	}
	return m
}

// IsActive returns true if the constraint is currently active at the given
// evaluation time, considering both the disabled flag and time conditions.
func (c *AccessConstraint) IsActive(now time.Time) bool {
	if c.Disabled {
		return false
	}
	return c.Condition.IsActive(now)
}

// ---------------------------------------------------------------------------
// Constraint-admin permission constant
// ---------------------------------------------------------------------------

const (
	// PermissionConstraintAdmin is the permission ID required for constraint
	// administration. This must be registered in the permissions registry.
	PermissionConstraintAdmin = "access_constraint.admin"
)

// ---------------------------------------------------------------------------
// Blast-radius preview types
// ---------------------------------------------------------------------------

// ConstraintPreview shows the blast radius of a constraint: which principals
// are affected and how their effective authority changes.
type ConstraintPreview struct {
	// ConstraintID is the ID of the constraint being previewed.
	ConstraintID string `json:"constraintId"`

	// ConstraintName is the name of the constraint being previewed.
	ConstraintName string `json:"constraintName"`

	// AffectedPrincipals lists the principals whose authority is reduced.
	AffectedPrincipals []AffectedPrincipal `json:"affectedPrincipals"`

	// RestrictedPermissions lists permissions that would be removed from
	// at least one principal's effective set.
	RestrictedPermissions []string `json:"restrictedPermissions"`

	// Truncated is true when the affected principals list is incomplete
	// (e.g., for all_principals constraints where enumerating every
	// principal is not feasible).
	Truncated bool `json:"truncated,omitempty"`
}

// AffectedPrincipal describes how a constraint affects one principal.
type AffectedPrincipal struct {
	// PrincipalType is "user", "agent", or "group".
	PrincipalType string `json:"principalType"`

	// PrincipalID is the principal's ID.
	PrincipalID string `json:"principalId"`

	// DisplayName is a human-readable name for the principal.
	DisplayName string `json:"displayName,omitempty"`

	// CurrentPermissions lists the permissions the principal currently holds
	// (before the constraint).
	CurrentPermissions []string `json:"currentPermissions,omitempty"`

	// ProposedPermissions lists the permissions the principal would hold
	// after the constraint is applied.
	ProposedPermissions []string `json:"proposedPermissions,omitempty"`

	// RemovedPermissions lists the permissions that would be removed.
	RemovedPermissions []string `json:"removedPermissions,omitempty"`
}
