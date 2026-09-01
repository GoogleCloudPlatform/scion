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
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
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
		// Guard against malformed selectors: a missing PrincipalType would
		// produce a typed key like ":someID" which could never match a
		// well-formed closure entry.
		if s.PrincipalType == "" {
			return false
		}
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

// ---------------------------------------------------------------------------
// Preview request/result types — B3 impact & preview engine
// ---------------------------------------------------------------------------

// PreviewRequest describes a boundary mutation to be previewed before commit.
type PreviewRequest struct {
	// Operation is "create", "update", or "delete".
	Operation string

	// Draft is the proposed constraint state. Nil for delete.
	Draft *store.AccessConstraint

	// ConstraintID is the existing constraint ID. Empty for create.
	ConstraintID string

	// BaseRevision is the expected current revision. 0 for create.
	BaseRevision int64

	// Actor is the principal requesting the mutation.
	Actor PrincipalContext
}

// PreviewResult is the complete impact analysis for a proposed boundary mutation.
// It includes a single-use, time-limited token that must be presented at commit.
type PreviewResult struct {
	// PreviewID is a unique identifier for this preview.
	PreviewID string `json:"previewId"`

	// PreviewToken is an opaque, server-signed single-use token.
	PreviewToken string `json:"previewToken"`

	// GeneratedAt is when the preview was computed.
	GeneratedAt time.Time `json:"generatedAt"`

	// ExpiresAt is when the preview token expires (generatedAt + 5 minutes).
	ExpiresAt time.Time `json:"expiresAt"`

	// Operation is "create", "update", or "delete".
	Operation string `json:"operation"`

	// ConstraintID is the constraint being modified. Empty for create.
	ConstraintID string `json:"constraintId,omitempty"`

	// BaseRevision is the revision the preview was computed against. 0 for create.
	BaseRevision int64 `json:"baseRevision,omitempty"`

	// DraftHash is the SHA-256 of the canonicalized draft JSON.
	DraftHash string `json:"draftHash"`

	// Classification is the direction of the mutation's effect on authority.
	Classification string `json:"classification"`

	// Completeness tracks whether the preview is fully computed.
	Completeness PreviewCompleteness `json:"completeness"`

	// Lockout is the post-state lockout assessment.
	Lockout LockoutAssessment `json:"lockout"`

	// Impact is the aggregate blast radius.
	Impact BoundaryImpact `json:"impact"`

	// TemporalStates describes the effect at each temporal transition.
	TemporalStates []TemporalImpact `json:"temporalStates"`

	// AffectedPage is the first page of affected principals.
	AffectedPage AffectedPrincipalsPage `json:"principalsPage"`

	// Intersecting lists other boundaries that overlap this one.
	Intersecting []IntersectingBoundary `json:"intersectingBoundaries"`

	// Warnings are advisory messages about the mutation.
	Warnings []PreviewWarning `json:"warnings"`

	// CommitBlocked is non-nil when the commit would be rejected.
	CommitBlocked *CommitBlockedReason `json:"commitBlocked,omitempty"`

	// allImpacted stores the full list of impacted principals for pagination.
	// Not serialized — used internally by ListAffectedPrincipals.
	allImpacted []ImpactedPrincipal `json:"-"`
}

// ---------------------------------------------------------------------------
// Classification constants
// ---------------------------------------------------------------------------

const (
	// ClassificationTighten means all affected principals lose permissions, none gain.
	ClassificationTighten = "tighten"

	// ClassificationRelax means all affected principals gain/maintain permissions, none lose.
	ClassificationRelax = "relax"

	// ClassificationMixed means some principals lose and some gain.
	ClassificationMixed = "mixed"

	// ClassificationNoEffect is a display-only subtype: removing nothing
	// and restoring nothing. Still structurally a relaxation and does not
	// bypass review.
	ClassificationNoEffect = "no_effect"
)

// ---------------------------------------------------------------------------
// Completeness
// ---------------------------------------------------------------------------

// PreviewCompleteness tracks whether a preview is fully computed.
// A preview with Complete=false CANNOT be committed.
type PreviewCompleteness struct {
	// Complete is true when the full impact is computed.
	Complete bool `json:"complete"`

	// Truncated is true when the affected principals list is incomplete.
	Truncated bool `json:"truncated"`

	// Degraded is true when resolution errors reduced precision.
	Degraded bool `json:"degraded"`

	// Reasons lists the specific causes of incompleteness.
	Reasons []IncompletenessReason `json:"reasons"`
}

// IncompletenessReason describes why a preview is not complete.
type IncompletenessReason struct {
	// Code is one of: MEMBERSHIP_RESOLUTION_FAILED, SUBJECT_SET_TOO_LARGE,
	// PERMISSION_RESOLUTION_FAILED, TIME_BUDGET_EXCEEDED.
	Code string `json:"code"`

	// Message is a human-readable description.
	Message string `json:"message"`

	// Details provides additional context.
	Details map[string]interface{} `json:"details,omitempty"`
}

// Incompleteness reason codes.
const (
	IncompletenessCodeMembershipFailed = "MEMBERSHIP_RESOLUTION_FAILED"
	IncompletenessCodeSubjectTooLarge  = "SUBJECT_SET_TOO_LARGE"
	IncompletenessCodePermissionFailed = "PERMISSION_RESOLUTION_FAILED"
	IncompletenessCodeTimeBudget       = "TIME_BUDGET_EXCEEDED"
)

// ---------------------------------------------------------------------------
// Impact
// ---------------------------------------------------------------------------

// BoundaryImpact is the aggregate blast radius of a boundary mutation.
type BoundaryImpact struct {
	// AffectedPrincipalCount is the total number of affected principals.
	AffectedPrincipalCount int `json:"affectedPrincipalCount"`

	// AffectedPrincipalCountExact is false when the count is a lower bound.
	AffectedPrincipalCountExact bool `json:"affectedPrincipalCountExact"`

	// LosingPrincipalCount is how many principals lose permissions.
	LosingPrincipalCount int `json:"losingPrincipalCount"`

	// RegainingPrincipalCount is how many principals gain permissions.
	RegainingPrincipalCount int `json:"regainingPrincipalCount"`

	// NoEffectPrincipalCount is how many affected principals see no change.
	NoEffectPrincipalCount int `json:"noEffectPrincipalCount"`

	// PermissionDiffs lists per-permission impact.
	PermissionDiffs []PermissionImpact `json:"permissionDiffs"`

	// PermissionDiffsTruncated is true when the diff list is incomplete.
	PermissionDiffsTruncated bool `json:"permissionDiffsTruncated"`

	// Current is the pre-mutation state summary.
	Current PermissionSummary `json:"current"`

	// Proposed is the post-mutation state summary.
	Proposed PermissionSummary `json:"proposed"`
}

// PermissionSummary is a count of effective permissions.
type PermissionSummary struct {
	EffectivePermissionCount int `json:"effectivePermissionCount"`
}

// PermissionImpact is the per-permission diff.
type PermissionImpact struct {
	// PermissionID is the permission being affected.
	PermissionID string `json:"permissionId"`

	// LosingCount is how many principals lose this permission.
	LosingCount int `json:"losingCount"`

	// RegainingCount is how many principals regain this permission.
	RegainingCount int `json:"regainingCount"`
}

// ---------------------------------------------------------------------------
// Temporal impact
// ---------------------------------------------------------------------------

// TemporalImpact describes the effect at one segment of a time-bounded
// boundary's timeline.
type TemporalImpact struct {
	// From is the start of this temporal window.
	From time.Time `json:"from"`

	// Until is the end of this window. Zero means indefinite.
	Until *time.Time `json:"until"`

	// Label describes this window (e.g., "before activation", "active period").
	Label string `json:"label"`

	// Classification is the direction of effect in this window.
	Classification string `json:"classification"`

	// AffectedPrincipalCount is the count of affected principals in this window.
	AffectedPrincipalCount int `json:"affectedPrincipalCount"`

	// RemovedPermissionCount is the total permissions removed in this window.
	RemovedPermissionCount int `json:"removedPermissionCount"`

	// Note is an optional human-readable note.
	Note string `json:"note,omitempty"`
}

// ---------------------------------------------------------------------------
// Lockout assessment
// ---------------------------------------------------------------------------

// LockoutAssessment evaluates whether the proposed change would lock out
// all constraint admins.
type LockoutAssessment struct {
	// Safe is nil when degraded and the answer is genuinely unknown.
	Safe *bool `json:"safe"`

	// RemainingActiveDirectAdmins is nil when degraded.
	RemainingActiveDirectAdmins *int `json:"remainingActiveDirectAdmins"`

	// ActorRetainsMutationAuthority indicates whether the actor still has
	// permission to modify constraints after this change.
	ActorRetainsMutationAuthority *bool `json:"actorRetainsMutationAuthority"`

	// CheckedPermissionIDs lists the permission IDs that were checked.
	CheckedPermissionIDs []string `json:"checkedPermissionIds"`

	// UndeterminedReason explains why Safe is nil.
	UndeterminedReason string `json:"undeterminedReason,omitempty"`
}

// ---------------------------------------------------------------------------
// Intersecting boundaries
// ---------------------------------------------------------------------------

// IntersectingBoundary describes another boundary that overlaps this one.
type IntersectingBoundary struct {
	// ID is the boundary's unique identifier.
	ID string `json:"id"`

	// Name is the boundary name. Nil when redacted.
	Name string `json:"name,omitempty"`

	// ScopeType is "system" or "project".
	ScopeType string `json:"scopeType"`

	// ScopeID is the project ID for project scope.
	ScopeID string `json:"scopeId,omitempty"`

	// Relationship describes the observed interaction.
	Relationship string `json:"relationship"`

	// OverlappingPermissionCount is the number of shared permissions.
	OverlappingPermissionCount int `json:"overlappingPermissionCount"`

	// NetEffectNote is a human-readable description of the net effect.
	NetEffectNote string `json:"netEffectNote"`
}

// Intersecting boundary relationship types.
const (
	RelationshipNarrows          = "narrows"
	RelationshipOverlaps         = "overlaps"
	RelationshipLimitsRelaxation = "limits_relaxation"
	RelationshipBlocksRelaxation = "blocks_relaxation"
)

// ---------------------------------------------------------------------------
// Preview warnings
// ---------------------------------------------------------------------------

// PreviewWarning is an advisory message about a boundary mutation.
type PreviewWarning struct {
	// Code identifies the warning type.
	Code string `json:"code"`

	// Severity is "info", "warning", or "error".
	Severity string `json:"severity"`

	// Message is a human-readable description.
	Message string `json:"message"`

	// Details provides additional context.
	Details map[string]interface{} `json:"details,omitempty"`
}

// Warning codes.
const (
	WarningCodeLargeBlastRadius        = "LARGE_BLAST_RADIUS"
	WarningCodeRelaxationIncluded      = "RELAXATION_INCLUDED"
	WarningCodeRelaxationMasked        = "RELAXATION_MASKED_BY_INTERSECTION"
	WarningCodeDestructivePermRestored = "DESTRUCTIVE_PERMISSION_RESTORED"
	WarningCodePreviewDegraded         = "PREVIEW_DEGRADED"
	WarningCodeScheduledActivation     = "SCHEDULED_ACTIVATION"
)

// ---------------------------------------------------------------------------
// Commit blocking
// ---------------------------------------------------------------------------

// CommitBlockedReason explains why a preview's commit would be rejected.
type CommitBlockedReason struct {
	// Code is the error code for the rejection.
	Code string `json:"code"`

	// Message is a human-readable explanation.
	Message string `json:"message"`

	// MissingPermissionIDs lists permissions the actor lacks.
	MissingPermissionIDs []string `json:"missingPermissionIds,omitempty"`
}

// ---------------------------------------------------------------------------
// Affected principals page
// ---------------------------------------------------------------------------

// AffectedPrincipalsPage is a paginated list of affected principals.
type AffectedPrincipalsPage struct {
	// Items is the current page of affected principals.
	Items []ImpactedPrincipal `json:"items"`

	// NextPageToken is the cursor for the next page. Empty when on last page.
	NextPageToken string `json:"nextPageToken,omitempty"`

	// TotalCount is the total number of affected principals.
	TotalCount int `json:"totalCount"`

	// TotalCountExact is false when TotalCount is a lower bound.
	TotalCountExact bool `json:"totalCountExact"`
}

// ImpactedPrincipal is a detailed affected-principal row for the preview.
type ImpactedPrincipal struct {
	// PrincipalType is "user", "agent", or "group".
	PrincipalType string `json:"principalType"`

	// PrincipalID is the principal's ID.
	PrincipalID string `json:"principalId"`

	// DisplayName is a human-readable name.
	DisplayName string `json:"displayName,omitempty"`

	// ChangeKind is "loses", "regains", "mixed", or "no_effect".
	ChangeKind string `json:"changeKind"`

	// MembershipPaths describes how the principal is reached by the subject.
	MembershipPaths []MembershipPath `json:"membershipPaths"`

	// CurrentPermissionCount is the number of current effective permissions.
	CurrentPermissionCount int `json:"currentPermissionCount"`

	// ProposedPermissionCount is the number of proposed effective permissions.
	ProposedPermissionCount int `json:"proposedPermissionCount"`

	// RemovedPermissions lists permissions being removed.
	RemovedPermissions []string `json:"removedPermissions"`

	// RegainedPermissions lists permissions being restored.
	RegainedPermissions []string `json:"regainedPermissions"`

	// RemovedPermissionCount is authoritative even when the list is truncated.
	RemovedPermissionCount int `json:"removedPermissionCount"`

	// GrantSources lists where the principal's authority comes from.
	GrantSources []GrantSource `json:"grantSources"`
}

// MembershipPath is a chain from the principal to the boundary's subject.
type MembershipPath struct {
	// Hops is the ordered chain, principal first, subject last.
	Hops []PrincipalRef `json:"hops"`

	// Direct is true when the principal is a direct member.
	Direct bool `json:"direct"`

	// Truncated is true when the path is incomplete.
	Truncated bool `json:"truncated"`
}

// PrincipalRef is a resolved principal reference.
type PrincipalRef struct {
	Type        string `json:"type"`
	ID          string `json:"id"`
	DisplayName string `json:"displayName,omitempty"`
}

// GrantSource describes where a principal's authority comes from.
type GrantSource struct {
	Kind      string `json:"kind"` // "role_binding" or "relationship_grant"
	ID        string `json:"id"`
	RoleName  string `json:"roleName,omitempty"`
	ScopeType string `json:"scopeType"`
	ScopeID   string `json:"scopeId,omitempty"`
}

// ---------------------------------------------------------------------------
// Async preview job
// ---------------------------------------------------------------------------

// PreviewJob tracks an asynchronous preview computation.
type PreviewJob struct {
	// JobID is the unique identifier for this job.
	JobID string `json:"jobId"`

	// Status is "accepted", "running", "succeeded", "failed", or "cancelled".
	Status string `json:"status"`

	// Operation is "create", "update", or "delete".
	Operation string `json:"operation"`

	// Progress reports computation progress. Nil when indeterminate.
	Progress *JobProgress `json:"progress,omitempty"`

	// Result is populated when Status is "succeeded".
	Result *PreviewResult `json:"result,omitempty"`

	// Error is the failure reason when Status is "failed".
	Error string `json:"error,omitempty"`

	// mu protects Status, Result, Error from concurrent access.
	mu sync.Mutex `json:"-"`

	// cancel stops the background goroutine when the job is cancelled.
	cancel context.CancelFunc `json:"-"`

	// allImpacted stores the full set of impacted principals for pagination.
	allImpacted []ImpactedPrincipal `json:"-"`
}

// JobProgress tracks async job computation progress.
type JobProgress struct {
	// Phase describes the current processing phase.
	Phase string `json:"phase"`

	// ProcessedCount is the number of items processed so far.
	ProcessedCount int `json:"processedCount"`

	// TotalCount is the total number of items. Nil when indeterminate.
	TotalCount *int `json:"totalCount"`

	// Determinate is true when TotalCount is known.
	Determinate bool `json:"determinate"`
}

// Preview job status constants.
const (
	JobStatusAccepted  = "accepted"
	JobStatusRunning   = "running"
	JobStatusSucceeded = "succeeded"
	JobStatusFailed    = "failed"
	JobStatusCancelled = "cancelled"
)

// ---------------------------------------------------------------------------
// Preview token error codes (lower_snake_case per repo convention G2)
// ---------------------------------------------------------------------------

const (
	// ErrCodePreviewTokenExpired is returned when the preview token has expired.
	ErrCodePreviewTokenExpired = "preview_token_expired"

	// ErrCodePreviewTokenReplay is returned when the token has already been used.
	ErrCodePreviewTokenReplay = "preview_token_replay"

	// ErrCodePreviewActorMismatch is returned when the actor does not match the token.
	ErrCodePreviewActorMismatch = "preview_actor_mismatch"

	// ErrCodePreviewOperationMismatch is returned when the operation does not match.
	ErrCodePreviewOperationMismatch = "preview_operation_mismatch"

	// ErrCodePreviewDraftModified is returned when the draft hash does not match.
	ErrCodePreviewDraftModified = "preview_draft_modified"

	// ErrCodePreviewRevisionMismatch is returned when the object revision changed.
	ErrCodePreviewRevisionMismatch = "preview_revision_mismatch"

	// ErrCodePreviewStateMismatch is returned when related state changed since preview.
	ErrCodePreviewStateMismatch = "preview_state_mismatch"

	// ErrCodePreviewIncomplete is returned when an incomplete preview is committed.
	ErrCodePreviewIncomplete = "preview_incomplete"

	// ErrCodeConstraintAdminLockout is returned when a mutation would lock out all admins.
	ErrCodeConstraintAdminLockout = "constraint_admin_lockout"

	// ErrCodePreviewTokenInvalid is returned when the token structure is malformed.
	ErrCodePreviewTokenInvalid = "preview_token_invalid"
)

// TokenValidationError is a typed error for preview token validation failures.
type TokenValidationError struct {
	Code    string
	Message string
}

func (e *TokenValidationError) Error() string {
	return e.Message
}
