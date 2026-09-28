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

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// BoundaryKind identifies the credential-side boundary a UAT is issued
// under. Canonical definition lives in pkg/hub/permissions (so permission
// metadata can reference it without an import cycle); this is an alias for
// ergonomic use within pkg/hub.
type BoundaryKind = permissions.BoundaryKind

const (
	BoundaryKindProject = permissions.BoundaryKindProject
	BoundaryKindHub     = permissions.BoundaryKindHub
)

// TokenBoundary is the credential-side boundary of a UAT: confined to one
// project, or spanning the hub. A.2/D.1 own persisting this (store/ent
// schema and mint/issuance wiring); A.1 defines the type, validity, and
// matching semantics only.
//
// The hub boundary means the credential is not restricted to one project —
// it does not by itself mean the holder has access to every project. Every
// request still requires the holder's current, live authority on the
// resolved target (see ProjectTargetAdmission) plus the credential's exact
// permission set.
type TokenBoundary struct {
	Kind      BoundaryKind
	ProjectID string // set iff Kind == BoundaryKindProject
}

// Valid rejects malformed boundary combinations. Calls permissions.ValidBoundary
// so the same rule is reachable from pkg/store (F-2): pkg/store cannot
// import pkg/hub, but D.1's store-layer validation needs this exact rule
// too — a shared table test pins agreement.
func (b TokenBoundary) Valid() bool {
	return permissions.ValidBoundary(b.Kind, b.ProjectID)
}

// TargetScopeKind classifies a resolved authorization target for boundary
// matching purposes only.
type TargetScopeKind string

const (
	TargetScopeProject TargetScopeKind = "project"
	TargetScopeHub     TargetScopeKind = "hub"
	TargetScopeUnknown TargetScopeKind = "unknown"
)

// TargetScope is resolved from a Resource (plus TargetScopeEvidence for
// collection-level requests) for boundary matching. It is not an
// authorization decision on its own and never implies ownership of the
// target.
type TargetScope struct {
	Kind      TargetScopeKind
	ProjectID string // set iff Kind == TargetScopeProject
}

// Valid mirrors TokenBoundary.Valid(): a malformed TargetScope (Project
// with no ID, or Hub/Unknown carrying a ProjectID) must never reach
// BoundaryAllows as if it were well-formed.
func (t TargetScope) Valid() bool {
	switch t.Kind {
	case TargetScopeProject:
		return t.ProjectID != ""
	case TargetScopeHub, TargetScopeUnknown:
		return t.ProjectID == ""
	default:
		return false
	}
}

// TargetScopeEvidence carries explicit, caller-declared classification for
// collection-level operations that ResolveTargetScope cannot recover from
// Resource fields alone: an empty Resource.ID is not by itself evidence of
// "this is a creation request" — that would conflate an intentional
// collection-level action with a malformed existing-resource request that
// simply failed to populate an ID. Evidence MUST be constructed by trusted
// server-side operation/handler code that already knows which operation it
// is executing — NEVER from a client-supplied request field.
type TargetScopeEvidence struct {
	// IsCollectionLevel, when true, declares this request as a
	// collection-level action (e.g. "create a new project", "create a
	// global-catalog skill") rather than an instance lookup on Resource.
	IsCollectionLevel bool
	// CollectionScope is required when IsCollectionLevel is true:
	// TargetScopeHub for project.create / skill.create_global-style
	// global-catalog creation, or TargetScopeProject (with
	// CollectionProjectID set) for "create a new resource inside this
	// already-identified, already-access-checked project."
	CollectionScope     TargetScopeKind
	CollectionProjectID string // set iff CollectionScope == TargetScopeProject
	// PermissionID names the canonical permission the collection-level
	// request is for. ResolveTargetScope cross-checks CollectionScope
	// against permissions.CollectionTargetClassesFor(PermissionID) — a
	// per-permission REVIEWED SET (not a Hub/Project-exclusive boolean),
	// which can be empty for instance-only actions (agent.attach, etc. —
	// always target an existing resource, never collection-level) or
	// contain both Project and Hub-reachable classes (skill.list: an
	// existing project's skills AND the hub-wide catalog). A mismatch
	// between CollectionScope and the reviewed set, or between the
	// evidence and the Resource's own facts, resolves Unknown.
	PermissionID string
}

// ResolveTargetScope classifies a request for boundary matching from the
// resolved Resource plus explicit collection-level evidence. It computes a
// small set of independent facts about the Resource and, for the
// collection-evidence path, validates ALL of them against the evidence and
// against each other before returning anything other than Unknown — no
// single field is allowed to "win" over a contradictory one (pat-refactor
// R1, 2026-09-28: a full contradiction matrix, not case-by-case checks).
//
// Non-collection-level path, applied in order:
//
//  1. r.Type == permissions.ResourceProject and r.ID != "": TargetScopeProject.
//  2. r.Type == permissions.ResourceHub: TargetScopeHub.
//  3. hasProjectParent(r) — UNLESS hasReviewedGlobalScopeKind(r) also holds
//     (a resource cannot be both project-parented and globally scoped) or
//     r.ParentType=="project" with an empty r.ParentID (malformed parent):
//     either contradiction resolves Unknown; otherwise TargetScopeProject.
//  4. r.ParentType == "system" — UNLESS r.ParentID is also set (a
//     contradiction): TargetScopeHub.
//  5. r.Type is "broker" or "runtime_broker": TargetScopeHub.
//  6. hasReviewedGlobalScopeKind(r): TargetScopeHub.
//  7. Anything else: TargetScopeUnknown. Never equate missing resource
//     parent metadata with hub scope.
//
// Collection-level path (evidence.IsCollectionLevel): see resolveCollectionEvidence.
func ResolveTargetScope(r Resource, evidence TargetScopeEvidence) TargetScope {
	// Supplied-but-unrecognized metadata is a contradiction, not absent
	// metadata — validated BEFORE any fast path, including the coherent
	// existing-project/Hub-type cases, so a caller cannot smuggle a
	// contradictory extra fact past them (pat-refactor R1, 2026-09-28).
	if !recognizedFactsCoherent(r) {
		return TargetScope{Kind: TargetScopeUnknown}
	}

	if evidence.IsCollectionLevel {
		return resolveCollectionEvidence(r, evidence)
	}

	if r.Type == permissions.ResourceProject && r.ID != "" {
		return TargetScope{Kind: TargetScopeProject, ProjectID: r.ID}
	}
	if r.Type == permissions.ResourceHub {
		return TargetScope{Kind: TargetScopeHub}
	}

	hasProjectParent := r.ParentType == "project" && r.ParentID != ""
	hasProjectParentMalformed := r.ParentType == "project" && r.ParentID == ""
	hasSystemParent := r.ParentType == "system"
	hasSystemParentMalformed := hasSystemParent && r.ParentID != ""
	hasGlobalScope := hasReviewedGlobalScopeKind(r.Type, r.ScopeKind)

	if hasProjectParentMalformed || hasSystemParentMalformed {
		return TargetScope{Kind: TargetScopeUnknown}
	}
	if hasProjectParent && hasGlobalScope {
		return TargetScope{Kind: TargetScopeUnknown}
	}

	if hasProjectParent {
		return TargetScope{Kind: TargetScopeProject, ProjectID: r.ParentID}
	}
	if hasSystemParent {
		return TargetScope{Kind: TargetScopeHub}
	}
	if r.Type == "broker" || r.Type == "runtime_broker" {
		return TargetScope{Kind: TargetScopeHub}
	}
	if hasGlobalScope {
		return TargetScope{Kind: TargetScopeHub}
	}

	return TargetScope{Kind: TargetScopeUnknown}
}

// resolveCollectionEvidence is ResolveTargetScope's collection-level branch,
// factored out for the full contradiction matrix pat-refactor's R1 review
// requires. Facts are computed ONCE and checked against both the evidence
// and each other; any contradiction denies (Unknown), and evidence never
// overrides a contradictory resource fact.
func resolveCollectionEvidence(r Resource, evidence TargetScopeEvidence) TargetScope {
	// Fact: a collection-level request must not also name an existing
	// resource instance.
	if r.ID != "" {
		return TargetScope{Kind: TargetScopeUnknown}
	}
	// recognizedFactsCoherent is already checked by the caller
	// (ResolveTargetScope) before dispatching here, but resolveCollectionEvidence
	// is unexported and only reachable that way — no separate check needed.

	// Fact: if both the Resource and the permission imply a resource type,
	// they must agree — a caller cannot mismatch which resource family a
	// collection-level request is for.
	expectedType := registryResourceType(evidence.PermissionID)
	if r.Type != "" && expectedType != "" && r.Type != expectedType {
		return TargetScope{Kind: TargetScopeUnknown}
	}
	effectiveType := r.Type
	if effectiveType == "" {
		effectiveType = expectedType
	}

	// Fact: the permission's OWN reviewed collection-class set. A reviewed
	// but EMPTY set (e.g. agent.attach/delete/token_refresh — instance-only
	// actions that always target an existing resource) denies regardless of
	// which CollectionScope the caller claims: a permission's resource
	// family being capable of living in a project does not make every
	// action on it a valid collection-level operation.
	classes, reviewed := permissions.CollectionTargetClassesFor(evidence.PermissionID)
	if !reviewed || len(classes) == 0 {
		return TargetScope{Kind: TargetScopeUnknown}
	}

	// Facts about the resource's own parent/scope metadata, independent of
	// the evidence — computed once, checked against both branches below.
	hasProjectParent := r.ParentType == "project" && r.ParentID != ""
	hasProjectParentMalformed := r.ParentType == "project" && r.ParentID == ""
	hasSystemParent := r.ParentType == "system"
	hasSystemParentMalformed := hasSystemParent && r.ParentID != ""
	hasGlobalScope := hasReviewedGlobalScopeKind(effectiveType, r.ScopeKind)
	hasExplicitProjectScope := hasExplicitProjectScopeKind(effectiveType, r.ScopeKind)

	if hasProjectParentMalformed || hasSystemParentMalformed {
		return TargetScope{Kind: TargetScopeUnknown}
	}
	if hasProjectParent && hasGlobalScope {
		return TargetScope{Kind: TargetScopeUnknown}
	}

	switch evidence.CollectionScope {
	case TargetScopeHub:
		if evidence.CollectionProjectID != "" {
			return TargetScope{Kind: TargetScopeUnknown} // stray project ID on Hub evidence
		}
		if !classesInclude(classes, permissions.TargetClassKindGlobalCatalog, permissions.TargetClassKindHubResource) {
			return TargetScope{Kind: TargetScopeUnknown} // Hub is not a reviewed class for this permission
		}
		if hasProjectParent {
			return TargetScope{Kind: TargetScopeUnknown} // resource independently claims a project parent
		}
		// hasSystemParent agrees with Hub evidence — not a contradiction.
		if hasExplicitProjectScope {
			return TargetScope{Kind: TargetScopeUnknown} // resource's own ScopeKind explicitly says "project", contradicting Hub evidence
		}
		return TargetScope{Kind: TargetScopeHub}
	case TargetScopeProject:
		if evidence.CollectionProjectID == "" {
			return TargetScope{Kind: TargetScopeUnknown}
		}
		if !classesInclude(classes, permissions.TargetClassKindProjectScoped) {
			return TargetScope{Kind: TargetScopeUnknown} // Project is not a reviewed class for this permission
		}
		if hasProjectParent && r.ParentID != evidence.CollectionProjectID {
			return TargetScope{Kind: TargetScopeUnknown} // resource names a DIFFERENT project than the evidence
		}
		if hasSystemParent {
			return TargetScope{Kind: TargetScopeUnknown} // resource independently claims system/hub scope
		}
		if hasGlobalScope {
			return TargetScope{Kind: TargetScopeUnknown} // resource's own ScopeKind claims global/user scope
		}
		return TargetScope{Kind: TargetScopeProject, ProjectID: evidence.CollectionProjectID}
	default:
		return TargetScope{Kind: TargetScopeUnknown}
	}
}

func classesInclude(classes []permissions.TargetClassKind, want ...permissions.TargetClassKind) bool {
	for _, c := range classes {
		for _, w := range want {
			if c == w {
				return true
			}
		}
	}
	return false
}

// hasReviewedGlobalScopeKind reports whether scopeKind marks resourceType's
// record as scoped to a user or globally, rather than to a project. RESTRICTED
// to the reviewed resource types that actually carry this ScopeKind concept
// (skill/template/harness_config) — an arbitrary/unexpected resource type
// that happens to have ScopeKind set to "user"/"global"/"core" (e.g. by a
// bug in an unrelated resource-building call site) must NOT be classified
// Hub on that basis alone (pat-refactor R1, 2026-09-28). store.SkillScopeUser,
// store.TemplateScopeUser, and store.HarnessConfigScopeUser are all the
// literal string "user" (and likewise "global"/"core"), so a single string
// comparison covers every resource-specific constant once the resource type
// itself is confirmed reviewed.
// recognizedFactsCoherent validates that r's own metadata is internally
// well-formed BEFORE any classification is attempted: an unrecognized
// ParentType value, an orphan ParentID with no declared ParentType, or an
// unrecognized ScopeKind value on a resource type that has reviewed
// scope-kind semantics are all contradictions — supplied-but-unrecognized
// metadata is NOT the same as absent metadata, and must deny rather than be
// silently treated as "no claim" (pat-refactor R1, 2026-09-28).
func recognizedFactsCoherent(r Resource) bool {
	switch r.ParentType {
	case "", "project", "system":
	default:
		return false
	}
	if r.ParentType == "" && r.ParentID != "" {
		return false // orphan ParentID with no declared ParentType
	}
	if isReviewedScopeKindResourceType(r.Type) && !isRecognizedScopeKindValue(r.ScopeKind) {
		return false
	}
	return true
}

// isReviewedScopeKindResourceType reports whether resourceType is one of
// the three types with reviewed ScopeKind semantics.
func isReviewedScopeKindResourceType(resourceType string) bool {
	switch resourceType {
	case permissions.ResourceSkill, permissions.ResourceTemplate, permissions.ResourceHarnessConfig:
		return true
	default:
		return false
	}
}

// isRecognizedScopeKindValue reports whether scopeKind is a value
// ResolveTargetScope understands for a resource type with reviewed
// scope-kind semantics. Empty means "not supplied" — not itself a
// contradiction; any other unrecognized value is.
func isRecognizedScopeKindValue(scopeKind string) bool {
	switch scopeKind {
	case "", store.SkillScopeProject, store.SkillScopeGlobal, store.SkillScopeCore, store.SkillScopeUser:
		return true
	default:
		return false
	}
}

func hasReviewedGlobalScopeKind(resourceType, scopeKind string) bool {
	switch resourceType {
	case permissions.ResourceSkill, permissions.ResourceTemplate, permissions.ResourceHarnessConfig:
	default:
		return false
	}
	switch scopeKind {
	case store.SkillScopeUser, store.SkillScopeGlobal, store.SkillScopeCore:
		return true
	default:
		return false
	}
}

// hasExplicitProjectScopeKind reports whether scopeKind is resourceType's
// own explicit "project" scope-kind constant (store.SkillScopeProject and
// the Template/HarnessConfig equivalents are all the literal "project").
// RESTRICTED to the same three reviewed resource types as
// hasReviewedGlobalScopeKind, for the same reason: an arbitrary type cannot
// carry this concept.
func hasExplicitProjectScopeKind(resourceType, scopeKind string) bool {
	switch resourceType {
	case permissions.ResourceSkill, permissions.ResourceTemplate, permissions.ResourceHarnessConfig:
		return scopeKind == store.SkillScopeProject
	default:
		return false
	}
}

// BoundaryAllows reports whether a credential boundary may even reach a
// target scope, before any permission, project-access, or relationship-
// grant check. Returns false if either b or t is invalid (see Valid() on
// each), or if t.Kind is Unknown — for every boundary kind, including Hub:
// an unresolvable target is never implicitly hub-reachable. A valid hub
// boundary allows valid project and hub targets; a valid project boundary
// allows only a valid project target with a matching ProjectID.
func BoundaryAllows(b TokenBoundary, t TargetScope) bool {
	if !b.Valid() || !t.Valid() {
		return false
	}
	if t.Kind == TargetScopeUnknown {
		return false
	}
	switch b.Kind {
	case BoundaryKindHub:
		return t.Kind == TargetScopeProject || t.Kind == TargetScopeHub
	case BoundaryKindProject:
		return t.Kind == TargetScopeProject && t.ProjectID == b.ProjectID
	default:
		return false
	}
}

// ProjectAccessSource records which evidence established project access,
// for explain/audit provenance.
type ProjectAccessSource string

const (
	// ProjectAccessSourceMembership is a direct, active project-scoped role
	// binding for the principal.
	ProjectAccessSourceMembership ProjectAccessSource = "membership"
	// ProjectAccessSourceGroup is an active project-scoped role binding
	// reached through one of the principal's effective groups.
	ProjectAccessSourceGroup ProjectAccessSource = "group"
	// ProjectAccessSourceSystemRole is an active system-scope role binding,
	// target-applicable (via applyHubWideScopeFilters), carrying the exact
	// requested permission.
	ProjectAccessSourceSystemRole ProjectAccessSource = "system_role"
)

// ErrProjectAccessDenied is returned (wrapped with context) whenever a
// project-access evidence function fails closed. Callers should treat any
// non-nil error identically to ok=false.
var ErrProjectAccessDenied = errors.New("project access evidence check failed closed")

// ErrUnsupportedPrincipalKind is returned by ProjectMembershipEvidence,
// SystemAuthorityProof, and ProjectTargetAdmission for any PrincipalKind
// other than PrincipalKindUser or PrincipalKindDev (local users, including
// the dev/local-user adapter). This is an ALLOWLIST check — an empty or
// unrecognized Kind value also fails closed into this error, not just the
// enumerated agent/federated/broker kinds. No CredentialKind branching
// anywhere in these functions, so a future agent-delegation extension (G)
// can reuse them with an issuer.
var ErrUnsupportedPrincipalKind = errors.New("project access evidence supports local user principals only")

func requireLocalUserPrincipal(principal PrincipalContext) error {
	switch principal.Kind {
	case PrincipalKindUser, PrincipalKindDev:
		if principal.ID == "" {
			return fmt.Errorf("%w: empty principal ID", ErrProjectAccessDenied)
		}
		return nil
	default:
		return ErrUnsupportedPrincipalKind
	}
}

// bindingActivationOK applies the same activation-window evaluation the
// kernel uses (evaluateActivation via CandidateBinding), rather than the
// simpler isBindingActive helper used elsewhere: evaluateActivation fails
// closed when Now is zero and the binding carries a time condition,
// matching the kernel-grade semantics this file's admission checks rely on.
func bindingActivationOK(b *store.RoleBinding, now time.Time) bool {
	cb := &CandidateBinding{BindingID: b.ID}
	if b.NotBefore != nil {
		cb.NotBefore = *b.NotBefore
	}
	if b.ExpiresAt != nil {
		cb.ExpiresAt = *b.ExpiresAt
	}
	return evaluateActivation(cb, now).Active
}

// principalClosure resolves the direct principal plus its effective groups
// into the []store.PrincipalRef shape ListRoleBindingsForPrincipals expects,
// along with the sets needed to classify which binding matched directly vs.
// through a group. Shared by ProjectMembershipEvidence and the system-scope
// queries below so principal/group resolution semantics cannot drift.
func (a *AuthzService) principalClosure(ctx context.Context, principal PrincipalContext) (refs []store.PrincipalRef, directKey string, groupKeys map[string]bool, err error) {
	normalizedType := NormalizePrincipalType(string(principal.Kind))
	refs = []store.PrincipalRef{{Type: normalizedType, ID: principal.ID}}
	directKey = normalizedType + ":" + principal.ID
	groupKeys = map[string]bool{}

	var groupIDs []string
	switch normalizedType {
	case store.RoleBindingPrincipalUser:
		groupIDs, err = a.store.GetEffectiveGroups(ctx, principal.ID)
	case store.RoleBindingPrincipalAgent:
		groupIDs, err = a.store.GetEffectiveGroupsForAgent(ctx, principal.ID)
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, "", nil, fmt.Errorf("group resolution failed (fail-closed): %w", err)
	}
	err = nil
	for _, gid := range groupIDs {
		refs = append(refs, store.PrincipalRef{Type: "group", ID: gid})
		groupKeys["group:"+gid] = true
	}
	return refs, directKey, groupKeys, nil
}

// scopedRoleBindingPermissions is the shared building block behind both
// getProjectScopedPermissions's project-scope query and the system-scope
// queries below (design review #7): given a pre-fetched binding list, it
// unions the permission IDs granted by every ACTIVE binding whose ScopeType
// matches scopeType (and, for project scope, whose ScopeID matches
// scopeID), then applies the same access-constraint reduction
// (loadAccessConstraintRestrictions) already relied on elsewhere.
// A role definition referenced by an active binding but missing/unloadable
// is a data-integrity error, not a routine "skip and continue" case: this
// function propagates that error to the caller (fail closed) rather than
// silently omitting the binding's permissions, per this contract's error
// policy (pat-refactor review, 2026-09-28).
func (a *AuthzService) scopedRoleBindingPermissions(ctx context.Context, bindings []*store.RoleBinding, scopeType, scopeID string, closure map[string]struct{}, resourceCtx ResourceContext, now time.Time) ([]string, error) {
	seen := make(map[string]bool)
	var result []string
	for _, b := range bindings {
		if b.ScopeType != scopeType {
			continue
		}
		if scopeType == ScopeTypeProject && b.ScopeID != scopeID {
			continue
		}
		if !bindingActivationOK(b, now) {
			continue
		}
		rd, rdErr := a.store.GetRoleDefinition(ctx, b.RoleDefinitionID)
		if rdErr != nil {
			return nil, fmt.Errorf("resolve role definition %q for binding %q: %w", b.RoleDefinitionID, b.ID, rdErr)
		}
		if rd == nil {
			return nil, fmt.Errorf("role definition %q for binding %q not found", b.RoleDefinitionID, b.ID)
		}
		for _, permID := range rd.Permissions {
			if !seen[permID] {
				seen[permID] = true
				result = append(result, permID)
			}
		}
	}
	if len(result) > 0 {
		restrictions := a.loadAccessConstraintRestrictions(ctx, closure, resourceCtx)
		result = applyRestrictions(result, restrictions)
	}
	return result, nil
}

// applyRestrictions filters permIDs down to those that survive every
// restriction's Check, mirroring the pattern in getProjectScopedPermissions.
// A restriction with a nil Check denies everything (fail closed).
func applyRestrictions(permIDs []string, restrictions []Restriction) []string {
	if len(restrictions) == 0 {
		return permIDs
	}
	var filtered []string
	for _, permID := range permIDs {
		blocked := false
		for _, r := range restrictions {
			if r.Check == nil || !r.Check(permID) {
				blocked = true
				break
			}
		}
		if !blocked {
			filtered = append(filtered, permID)
		}
	}
	return filtered
}

// ProjectMembershipEvidence reports whether principal has a direct or
// group-expanded ACTIVE project-scoped role binding for projectID,
// permission-agnostic — membership is access, independent of which specific
// permissions the bound role carries. Fails closed on an empty projectID, a
// non-Active user status, or any store error. Local user principals only
// (see ErrUnsupportedPrincipalKind).
//
// Deliberately not built from isProjectOwnerOrAdmin (its direct-membership
// branch has no activation-window check).
func (a *AuthzService) ProjectMembershipEvidence(ctx context.Context, principal PrincipalContext, projectID string) (bool, ProjectAccessSource, error) {
	if err := requireLocalUserPrincipal(principal); err != nil {
		return false, "", err
	}
	if projectID == "" {
		return false, "", fmt.Errorf("%w: empty project ID", ErrProjectAccessDenied)
	}
	if err := a.requireActiveUser(ctx, principal); err != nil {
		return false, "", err
	}

	refs, directKey, groupKeys, err := a.principalClosure(ctx, principal)
	if err != nil {
		return false, "", fmt.Errorf("%w: %v", ErrProjectAccessDenied, err)
	}
	bindings, err := a.store.ListRoleBindingsForPrincipals(ctx, refs, nil, nil)
	if err != nil {
		return false, "", fmt.Errorf("%w: binding resolution failed: %v", ErrProjectAccessDenied, err)
	}

	now := time.Now()
	for _, b := range bindings {
		if b.ScopeType != ScopeTypeProject || b.ScopeID != projectID {
			continue
		}
		if !bindingActivationOK(b, now) {
			continue
		}
		compositeKey := b.PrincipalType + ":" + b.PrincipalID
		if compositeKey == directKey {
			return true, ProjectAccessSourceMembership, nil
		}
		if groupKeys[compositeKey] {
			return true, ProjectAccessSourceGroup, nil
		}
	}
	return false, "", nil
}

func (a *AuthzService) requireActiveUser(ctx context.Context, principal PrincipalContext) error {
	normalizedType := NormalizePrincipalType(string(principal.Kind))
	if normalizedType != store.RoleBindingPrincipalUser {
		return nil
	}
	user, err := a.store.GetUser(ctx, principal.ID)
	if err != nil {
		return fmt.Errorf("%w: user lookup failed: %v", ErrProjectAccessDenied, err)
	}
	if user == nil || user.Status != store.UserStatusActive {
		return fmt.Errorf("%w: user is not active", ErrProjectAccessDenied)
	}
	return nil
}

// ProjectTargetClass identifies the class of target a permission is
// evaluated against, WITHOUT a target ID — mint time has no real target
// yet.
type ProjectTargetClass struct {
	ResourceType string // permissions.ResourceAgent, etc.
	ScopeKind    string // e.g. store.SkillScopeProject; empty where the resource type has no scope-kind concept
}

// ContemplatedProjectClass returns the definite project-scoped
// ProjectTargetClass for permissionID, for use where a real project ID is
// known but no real target instance exists yet (Project-boundary mint, and
// SystemAuthorityProof's caller in general) — never global/core for the
// scope-kind-split types (skill/template/harness_config), empty ScopeKind
// for every other resource type.
func ContemplatedProjectClass(permissionID string) ProjectTargetClass {
	resourceType := registryResourceType(permissionID)
	class := ProjectTargetClass{ResourceType: resourceType}
	switch resourceType {
	case permissions.ResourceSkill:
		class.ScopeKind = store.SkillScopeProject
	case permissions.ResourceTemplate:
		class.ScopeKind = store.TemplateScopeProject
	case permissions.ResourceHarnessConfig:
		class.ScopeKind = store.HarnessConfigScopeProject
	}
	return class
}

// validateRealProjectClass rejects a class that cannot represent a REAL
// project target for permissionID: an empty or mismatched ResourceType
// (which would route around applyHubWideScopeFilters's curated-catalog
// dispatch entirely), or — for the three resource types with a genuine
// curated hub-wide-catalog carve-out (skill/template/harness_config) — any
// ScopeKind other than that type's own "project" constant. This is an
// ALLOWLIST, not a blocklist: an unrecognized ScopeKind value for one of
// those three types is rejected just as surely as a known global/user
// value (pat-refactor R4, 2026-09-28) — a real project-target proof is
// never "the global catalog," and never a value the reviewer hasn't seen.
// Every OTHER resource type (agent, project, scheduled_event,
// gcp_service_account, and material-delivery types such as secret/env_var/
// skill_injection) has no curated hub-wide/project ScopeKind concept at
// all — applyHubWideScopeFilters passes it through unchanged regardless of
// ScopeKind, so any caller-supplied value is accepted for those types; this
// validator only restricts the three where ScopeKind is semantically load-
// bearing.
// validRealProjectScopeKinds is the explicit, reviewed ALLOWLIST of
// ScopeKind values each resource type may carry for a REAL (not
// contemplated) project-target class. A resource type absent from this map
// has NO reviewed scope-kind semantics at all: its class.ScopeKind must be
// exactly empty — never an arbitrary accepted string (pat-refactor R4,
// 2026-09-28: replaces an earlier "every other resource type accepts ANY
// ScopeKind" exemption, which was not a reviewed-class contract). Material-
// delivery resource types are pre-registered here per pat-f2-arch-3's plan
// (2026-09-28) — F.2 owns adding the corresponding Registry permission
// rows; this table already has their reviewed allowed values so F needs no
// separate exemption once those rows land, and the execution-project
// projectID argument stays distinct from the material's own source class.
var validRealProjectScopeKinds = map[string][]string{
	permissions.ResourceSkill:         {store.SkillScopeProject},
	permissions.ResourceTemplate:      {store.TemplateScopeProject},
	permissions.ResourceHarnessConfig: {store.HarnessConfigScopeProject},
	"secret":                          {"project", "hub", "user", "runtime_broker"},
	"env_var":                         {"project", "hub", "user", "runtime_broker"},
	"skill_injection":                 {"project", "hub", "user", "runtime_broker"},
}

func validateRealProjectClass(permissionID string, class ProjectTargetClass) error {
	expected := registryResourceType(permissionID)
	if class.ResourceType == "" || class.ResourceType != expected {
		return fmt.Errorf("%w: class resource type %q does not match permission %q's resource type %q", ErrProjectAccessDenied, class.ResourceType, permissionID, expected)
	}
	allowed, hasScopeSemantics := validRealProjectScopeKinds[class.ResourceType]
	if !hasScopeSemantics {
		if class.ScopeKind != "" {
			return fmt.Errorf("%w: resource type %q has no reviewed scope-kind semantics; class scope kind must be empty, got %q", ErrProjectAccessDenied, class.ResourceType, class.ScopeKind)
		}
		return nil
	}
	for _, v := range allowed {
		if class.ScopeKind == v {
			return nil
		}
	}
	return fmt.Errorf("%w: class scope kind %q is not a registered valid value for resource type %q (valid: %v)", ErrProjectAccessDenied, class.ScopeKind, class.ResourceType, allowed)
}

func registryResourceType(permissionID string) string {
	for _, p := range permissions.Registry {
		if p.ID == permissionID {
			return p.Resource
		}
	}
	return ""
}

// applyHubWideScopeFilters is the single shared dispatcher for the
// curated-role scope filters: it strips a curated hub-member/hub-viewer (or
// synthetic agent-catalog) system-scope candidate binding unless the
// contemplated/actual target is genuinely the hub-wide catalog for that
// resource type. Both Decide's kernel path (steps 5c/5d/5e) and
// SystemAuthorityProof/MintTimeSystemGrant call this one function — not a
// copy of filterHubWideSkillGrants/Template/HarnessConfig — so the two
// paths cannot drift apart.
func applyHubWideScopeFilters(candidates []CandidateBinding, roleDefs map[string]*RolePermissions, class ProjectTargetClass) []CandidateBinding {
	switch class.ResourceType {
	case permissions.ResourceSkill:
		return filterHubWideSkillGrants(candidates, roleDefs, class.ScopeKind)
	case permissions.ResourceTemplate:
		return filterHubWideTemplateGrants(candidates, roleDefs, class.ScopeKind)
	case permissions.ResourceHarnessConfig:
		return filterHubWideHarnessConfigGrants(candidates, roleDefs, class.ScopeKind)
	default:
		return candidates
	}
}

// activeSystemScopeCandidates loads principal's active system-scope role
// bindings as kernel CandidateBinding/RolePermissions structures, ready for
// applyHubWideScopeFilters. Shared by SystemAuthorityProof and
// MintTimeSystemGrant.
func (a *AuthzService) activeSystemScopeCandidates(ctx context.Context, principal PrincipalContext) ([]CandidateBinding, map[string]*RolePermissions, []store.PrincipalRef, error) {
	refs, _, _, err := a.principalClosure(ctx, principal)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%w: %v", ErrProjectAccessDenied, err)
	}
	bindings, err := a.store.ListRoleBindingsForPrincipals(ctx, refs, nil, nil)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%w: binding resolution failed: %v", ErrProjectAccessDenied, err)
	}
	now := time.Now()
	var systemBindings []*store.RoleBinding
	for _, b := range bindings {
		if b.ScopeType != ScopeTypeSystem {
			continue
		}
		if !bindingActivationOK(b, now) {
			continue
		}
		systemBindings = append(systemBindings, b)
	}
	roleDefs, err := a.loadRoleDefinitions(ctx, collectRoleDefinitionIDs(systemBindings))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%w: role resolution failed: %v", ErrProjectAccessDenied, err)
	}
	candidates := toCandidateBindings(systemBindings)
	return candidates, roleDefs, refs, nil
}

func candidateSetHasPermission(candidates []CandidateBinding, roleDefs map[string]*RolePermissions, permissionID string) bool {
	for _, cb := range candidates {
		rp := roleDefs[cb.RoleDefinitionID]
		if rp == nil {
			continue
		}
		if rp.HasPermission(permissionID) {
			return true
		}
	}
	return false
}

// SystemAuthorityProof reports whether principal's ACTIVE system-scope role
// grants permissionID in a way that is TARGET-APPLICABLE — reusing
// applyHubWideScopeFilters (the same semantics as filterHubWideSkillGrants/
// Template/HarnessConfig), not raw permission-ID membership: a hub-member's
// catalog-only skill.read does not qualify for a project-scoped skill
// target, because applyHubWideScopeFilters strips that candidate binding
// before permissionID is checked, exactly as Decide does today.
//
// projectID is REQUIRED (not empty) — constraint reduction is
// project-scoped. Never call this with a blank projectID to fake a
// project-agnostic check; use MintTimeSystemGrant for that. Requires
// permissions.AppliesToExistingProjectTarget(permissionID) to be reviewed
// true; an unreviewed or hub-only ID denies (ok=false, err=nil). Never
// recurses into Decide. Local user principals only.
func (a *AuthzService) SystemAuthorityProof(ctx context.Context, principal PrincipalContext, projectID, permissionID string, class ProjectTargetClass) (bool, error) {
	if err := requireLocalUserPrincipal(principal); err != nil {
		return false, err
	}
	if projectID == "" {
		return false, fmt.Errorf("%w: SystemAuthorityProof requires a non-empty projectID", ErrProjectAccessDenied)
	}
	if err := a.requireActiveUser(ctx, principal); err != nil {
		return false, err
	}
	if applies, reviewed := permissions.AppliesToExistingProjectTarget(permissionID); !reviewed || !applies {
		return false, nil
	}
	if err := validateRealProjectClass(permissionID, class); err != nil {
		return false, err
	}

	candidates, roleDefs, refs, err := a.activeSystemScopeCandidates(ctx, principal)
	if err != nil {
		return false, err
	}
	filtered := applyHubWideScopeFilters(candidates, roleDefs, class)
	if !candidateSetHasPermission(filtered, roleDefs, permissionID) {
		return false, nil
	}

	// Access-constraint reduction, project-scoped.
	closure := make(map[string]struct{}, len(refs))
	for _, p := range refs {
		closure[p.Type+":"+p.ID] = struct{}{}
	}
	restrictions := a.loadAccessConstraintRestrictions(ctx, closure, ResourceContext{ProjectID: projectID})
	survivors := applyRestrictions([]string{permissionID}, restrictions)
	return len(survivors) == 1, nil
}

// MintTimeSystemGrant answers HUB-BOUNDARY MINT-TIME eligibility only — no
// real target or project exists yet, for EITHER a project-scoped or a
// global-catalog reading. A SEPARATE facade from SystemAuthorityProof
// (never called with a blank projectID — that function always requires a
// real one): mint-time hub-boundary contemplation is inherently
// project-agnostic. Iterates permissions.SupportedTargetClassesFor(permissionID)
// and succeeds if applyHubWideScopeFilters leaves permissionID intact for
// the principal's active, constraint-reduced (hub-wide ResourceContext, no
// ProjectID) system-scope grants for ANY supported class. This is the fix
// for "a hub-member's catalog-only skill.read must remain hub-boundary
// mint-eligible for the global catalog" while ProjectTargetAdmission still
// denies that same user for an unrelated project-scoped skill at use time,
// where the real target's actual ScopeKind resolves the class without
// contemplation.
func (a *AuthzService) MintTimeSystemGrant(ctx context.Context, principal PrincipalContext, permissionID string) (bool, error) {
	if err := requireLocalUserPrincipal(principal); err != nil {
		return false, err
	}
	if err := a.requireActiveUser(ctx, principal); err != nil {
		return false, err
	}
	classes := permissions.SupportedTargetClassesFor(permissionID)
	if len(classes) == 0 {
		return false, nil
	}

	candidates, roleDefs, refs, err := a.activeSystemScopeCandidates(ctx, principal)
	if err != nil {
		return false, err
	}
	closure := make(map[string]struct{}, len(refs))
	for _, p := range refs {
		closure[p.Type+":"+p.ID] = struct{}{}
	}
	restrictions := a.loadAccessConstraintRestrictions(ctx, closure, ResourceContext{})

	resourceType := registryResourceType(permissionID)
	for _, ck := range classes {
		class := ProjectTargetClass{ResourceType: resourceType}
		switch ck {
		case permissions.TargetClassKindGlobalCatalog:
			switch resourceType {
			case permissions.ResourceSkill:
				class.ScopeKind = store.SkillScopeGlobal
			case permissions.ResourceTemplate:
				class.ScopeKind = store.TemplateScopeGlobal
			case permissions.ResourceHarnessConfig:
				class.ScopeKind = store.HarnessConfigScopeGlobal
			}
		case permissions.TargetClassKindProjectScoped:
			class = ContemplatedProjectClass(permissionID)
		case permissions.TargetClassKindHubResource:
			// No curated hub-wide/project split for this resource type;
			// applyHubWideScopeFilters passes candidates through unchanged
			// regardless of ScopeKind. class stays {ResourceType, ""}.
		default:
			// An unrecognized TargetClassKind value (e.g. a future enum
			// member this switch has not been updated for) must never
			// silently fall through to an under-specified class and
			// potentially grant on it — skip it explicitly (pat-refactor
			// R5, 2026-09-28): fail closed on an unknown class, the same
			// way an absent SupportedTargetClasses entry already does.
			continue
		}
		filtered := applyHubWideScopeFilters(candidates, roleDefs, class)
		if !candidateSetHasPermission(filtered, roleDefs, permissionID) {
			continue
		}
		if survivors := applyRestrictions([]string{permissionID}, restrictions); len(survivors) == 1 {
			return true, nil
		}
	}
	return false, nil
}

// hasAnyProjectBinding reports whether principal holds permissionID via an
// active, constraint-reduced project-scoped role binding in ANY project (not
// a specific one) — used by CanMintSelector's hub-boundary
// project-applicable-permission branch, where mint time has no single
// project to check against. Restricted to permissions reviewed applicable
// to an existing project target: a hub-only permission (e.g. broker.create)
// must never be satisfied by an incidental project-role grant, and never
// recursed into for one it cannot apply to. Role-load errors are
// propagated, not swallowed.
func (a *AuthzService) hasAnyProjectBinding(ctx context.Context, principal PrincipalContext, permissionID string) (bool, error) {
	if applies, reviewed := permissions.AppliesToExistingProjectTarget(permissionID); !reviewed || !applies {
		return false, nil
	}
	refs, _, _, err := a.principalClosure(ctx, principal)
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrProjectAccessDenied, err)
	}
	bindings, err := a.store.ListRoleBindingsForPrincipals(ctx, refs, []string{store.RoleScopeProject}, nil)
	if err != nil {
		return false, fmt.Errorf("%w: binding resolution failed: %v", ErrProjectAccessDenied, err)
	}
	now := time.Now()
	byProject := make(map[string][]*store.RoleBinding)
	for _, b := range bindings {
		if b.ScopeType != ScopeTypeProject {
			continue
		}
		if !bindingActivationOK(b, now) {
			continue
		}
		byProject[b.ScopeID] = append(byProject[b.ScopeID], b)
	}
	if len(byProject) == 0 {
		return false, nil
	}

	var allBindings []*store.RoleBinding
	for _, bs := range byProject {
		allBindings = append(allBindings, bs...)
	}
	roleDefs, err := a.loadRoleDefinitions(ctx, collectRoleDefinitionIDs(allBindings))
	if err != nil {
		return false, fmt.Errorf("%w: role definition resolution failed: %v", ErrProjectAccessDenied, err)
	}

	closure := make(map[string]struct{}, len(refs))
	for _, p := range refs {
		closure[p.Type+":"+p.ID] = struct{}{}
	}

	// Evaluate EACH project independently, with that project's own
	// access-constraint reduction (ResourceContext{ProjectID: thatProject})
	// — a constraint governing one project must not be diluted by merging
	// its bindings with an unconstrained second project's, and must not be
	// evaluated as a system-wide constraint (ResourceContext{}) instead
	// (pat-refactor R2, 2026-09-28). Succeeds only if at least one actual
	// project's own constrained grant survives.
	for projectID, projBindings := range byProject {
		if !candidateSetHasPermission(toCandidateBindings(projBindings), roleDefs, permissionID) {
			continue
		}
		restrictions := a.loadAccessConstraintRestrictions(ctx, closure, ResourceContext{ProjectID: projectID})
		if survivors := applyRestrictions([]string{permissionID}, restrictions); len(survivors) == 1 {
			return true, nil
		}
	}
	return false, nil
}

// permissionSurvivesProjectConstraints reports whether permissionID is NOT
// stripped by projectID's own access-constraint reduction for principal —
// i.e. whether a governing constraint on this specific project would still
// allow permissionID. Does not check membership or any role's permission
// set; it answers only the constraint-reduction question, for reuse by
// both the single-project (Project boundary) and any-project (Hub
// boundary) relationship-eligibility checks.
func (a *AuthzService) permissionSurvivesProjectConstraints(ctx context.Context, principal PrincipalContext, projectID, permissionID string) (bool, error) {
	refs, _, _, err := a.principalClosure(ctx, principal)
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrProjectAccessDenied, err)
	}
	closure := make(map[string]struct{}, len(refs))
	for _, p := range refs {
		closure[p.Type+":"+p.ID] = struct{}{}
	}
	restrictions := a.loadAccessConstraintRestrictions(ctx, closure, ResourceContext{ProjectID: projectID})
	survivors := applyRestrictions([]string{permissionID}, restrictions)
	return len(survivors) == 1, nil
}

// hasRelevantProjectAdmission reports whether principal has an active
// project-scoped role binding in AT LEAST ONE project WHERE that project's
// own access-constraint reduction does not strip permissionID — the
// "relevant project admission" evidence for a relationship-eligible
// selector under a Hub boundary. This prevents a governing constraint that
// excludes the permission for every project the principal belongs to from
// being worked around via the relationship path (pat-refactor R3,
// 2026-09-28): relationship mint candidacy is permission-agnostic about
// WHICH role a project membership carries, but it must still respect a
// constraint that specifically strips the selected permission. Does not
// enumerate any specific owned target — project-level only.
func (a *AuthzService) hasRelevantProjectAdmission(ctx context.Context, principal PrincipalContext, permissionID string) (bool, error) {
	refs, _, _, err := a.principalClosure(ctx, principal)
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrProjectAccessDenied, err)
	}
	bindings, err := a.store.ListRoleBindingsForPrincipals(ctx, refs, []string{store.RoleScopeProject}, nil)
	if err != nil {
		return false, fmt.Errorf("%w: binding resolution failed: %v", ErrProjectAccessDenied, err)
	}
	now := time.Now()
	projects := map[string]bool{}
	for _, b := range bindings {
		if b.ScopeType != ScopeTypeProject {
			continue
		}
		if bindingActivationOK(b, now) {
			projects[b.ScopeID] = true
		}
	}
	for projectID := range projects {
		ok, err := a.permissionSurvivesProjectConstraints(ctx, principal, projectID, permissionID)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

// ProjectAdmissionResult is the outcome of ProjectTargetAdmission.
type ProjectAdmissionResult struct {
	Admitted bool
	Source   ProjectAccessSource // zero value when !Admitted
}

type projectAdmissionCacheKey struct {
	principalKind PrincipalKind
	principalID   string
	projectID     string
	permissionID  string
	class         ProjectTargetClass
}

// ProjectAdmissionCache is an optional request-scoped memo shared across
// multiple ProjectTargetAdmission calls in one request (e.g. B.2's
// request-local authority cache). nil is safe (unmemoized). Never persisted
// or shared ACROSS requests. Errors are NEVER cached — a failed lookup is
// recomputed on the next call, never remembered as a denial or an allow.
type ProjectAdmissionCache struct {
	mu    sync.Mutex
	cache map[projectAdmissionCacheKey]ProjectAdmissionResult
}

// NewProjectAdmissionCache constructs an empty, ready-to-use cache.
func NewProjectAdmissionCache() *ProjectAdmissionCache {
	return &ProjectAdmissionCache{cache: make(map[projectAdmissionCacheKey]ProjectAdmissionResult)}
}

func (c *ProjectAdmissionCache) get(key projectAdmissionCacheKey) (ProjectAdmissionResult, bool) {
	if c == nil {
		return ProjectAdmissionResult{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.cache[key]
	return v, ok
}

func (c *ProjectAdmissionCache) put(key projectAdmissionCacheKey, v ProjectAdmissionResult) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cache[key] = v
}

// ErrProjectMismatch is returned by ProjectTargetAdmission when target's
// resolved project does not equal the supplied projectID — callers must not
// pass a mismatched pair.
var ErrProjectMismatch = errors.New("target project does not match requested projectID")

// ProjectTargetAdmission is the ONE runtime composition C.1 (from inside
// enforceUATConstraints) and D.1's cross-project bearer gate call for an
// ACTUAL request with a resolved target. Composes ProjectMembershipEvidence
// OR SystemAuthorityProof(permissionID, class-derived-from-target's actual
// ScopeKind) for projectID/permissionID/target. Returns ErrProjectMismatch
// if target's resolved project (via ResolveTargetScope with zero evidence,
// since a real instance never needs collection-level evidence) does not
// equal projectID. Any error denies. Local user principals only.
func (a *AuthzService) ProjectTargetAdmission(ctx context.Context, principal PrincipalContext, projectID, permissionID string, target Resource, memo *ProjectAdmissionCache) (ProjectAdmissionResult, error) {
	targetScope := ResolveTargetScope(target, TargetScopeEvidence{})
	if targetScope.Kind != TargetScopeProject || targetScope.ProjectID != projectID {
		return ProjectAdmissionResult{}, fmt.Errorf("%w: target resolves to %q, requested %q", ErrProjectMismatch, targetScope.ProjectID, projectID)
	}
	class := ProjectTargetClass{ResourceType: target.Type, ScopeKind: target.ScopeKind}
	return a.ProjectAdmissionForClass(ctx, principal, projectID, permissionID, class, memo)
}

// ProjectAdmissionForClass is the class-explicit form of ProjectTargetAdmission,
// for a caller that already knows the target class without needing to
// resolve it from a full Resource — e.g. F.2's material delivery, where the
// material's own scope class can differ from the execution project's
// resource type/scope. ProjectTargetAdmission delegates to this after
// resolving class from target.
//
// Composes ProjectMembershipEvidence(ctx, principal, projectID) OR
// SystemAuthorityProof(ctx, principal, projectID, permissionID, class). Any
// error denies. The memo (nil-safe) is keyed on
// (principal, projectID, permissionID, class); errors are NEVER memoized —
// a failed lookup is recomputed on the next call, never remembered as a
// denial or an allow.
func (a *AuthzService) ProjectAdmissionForClass(ctx context.Context, principal PrincipalContext, projectID, permissionID string, class ProjectTargetClass, memo *ProjectAdmissionCache) (ProjectAdmissionResult, error) {
	if err := requireLocalUserPrincipal(principal); err != nil {
		return ProjectAdmissionResult{}, err
	}
	if projectID == "" {
		return ProjectAdmissionResult{}, fmt.Errorf("%w: empty project ID", ErrProjectAccessDenied)
	}
	// Validate class against permissionID BEFORE either branch (pat-refactor
	// R4, 2026-09-28): a successful membership check must not skip
	// class/permission coherence entirely — an unknown or mismatched class
	// denies regardless of which admission path would otherwise have been
	// tried.
	if err := validateRealProjectClass(permissionID, class); err != nil {
		return ProjectAdmissionResult{}, err
	}

	key := projectAdmissionCacheKey{principalKind: principal.Kind, principalID: principal.ID, projectID: projectID, permissionID: permissionID, class: class}
	if cached, ok := memo.get(key); ok {
		return cached, nil
	}

	if ok, source, err := a.ProjectMembershipEvidence(ctx, principal, projectID); err != nil {
		return ProjectAdmissionResult{}, err
	} else if ok {
		result := ProjectAdmissionResult{Admitted: true, Source: source}
		memo.put(key, result)
		return result, nil
	}

	ok, err := a.SystemAuthorityProof(ctx, principal, projectID, permissionID, class)
	if err != nil {
		return ProjectAdmissionResult{}, err
	}
	result := ProjectAdmissionResult{}
	if ok {
		result = ProjectAdmissionResult{Admitted: true, Source: ProjectAccessSourceSystemRole}
	}
	memo.put(key, result)
	return result, nil
}

// MintDenialReason is a stable, exported machine code — never free text —
// so callers can branch on it without parsing prose.
type MintDenialReason string

const (
	MintDenialNone                    MintDenialReason = ""
	MintDenialProjectAccessRequired   MintDenialReason = "project_access_required"
	MintDenialUnknownSelector         MintDenialReason = "unknown_selector"
	MintDenialBoundaryNotAllowed      MintDenialReason = "boundary_not_allowed"
	MintDenialFlatRoleInsufficient    MintDenialReason = "flat_role_insufficient"
	MintDenialNoRelationshipCandidacy MintDenialReason = "no_relationship_candidacy"
)

// SelectorEligibility is one selector's result within a CanMintSelector call.
type SelectorEligibility struct {
	Selector string
	OK       bool
	Reason   MintDenialReason
}

// hasProjectRoleFlatPermission reports whether principal currently holds
// permissionID as a flat subset of their project-scoped role in projectID —
// the existing, unchanged project-boundary flat-role mint rule.
func (a *AuthzService) hasProjectRoleFlatPermission(ctx context.Context, principal PrincipalContext, projectID, permissionID string) (bool, error) {
	perms, err := a.getProjectScopedPermissions(ctx, string(principal.Kind), principal.ID, projectID)
	if err != nil {
		return false, err
	}
	for _, p := range perms {
		if p == permissionID {
			return true, nil
		}
	}
	return false, nil
}

// ErrEmptySelectorList is returned by CanMintSelector for a nil/empty
// selectors argument: an empty request is rejected explicitly, never
// silently treated as a trivially successful validation (pat-refactor
// review, 2026-09-28).
var ErrEmptySelectorList = errors.New("no selectors requested")

// CanMintSelector answers, for every requested selector at once, whether
// principal may select it for boundary — never "does a target already
// exist," never a grant by itself. Principal and boundary are validated
// BEFORE the selector list, so an invalid principal/boundary is never
// masked by an empty-list short-circuit; an empty/nil selectors list is
// then rejected explicitly with ErrEmptySelectorList.
//
// For a Project boundary, ProjectMembershipEvidence is loaded ONCE for the
// whole batch. For each selector, resolved to SelectorMapping.PermissionIDs:
// admitted := membershipOK; if not, admitted requires EVERY permission ID in
// the expansion (all alias members) to individually pass
// SystemAuthorityProof(ctx, principal, boundary.ProjectID, permID,
// ContemplatedProjectClass(permID)). If admitted, each permission without a
// MintEligibilityRegistry descriptor additionally needs the project role's
// OWN flat permission subset (hasProjectRoleFlatPermission) when admission
// came from membership — when admission instead came from system authority
// for that exact permission, that proof already suffices (no redundant,
// narrower recheck).
//
// For a Hub boundary, EACH permission ID is evaluated by hubPermissionEligible:
// a flat/system path (MintTimeSystemGrant OR any active project-scoped
// binding) OR a relationship alternative (a MintEligibilityRelationship
// source for this exact principal kind/resource type/permission, combined
// with relevant — not target-enumerated — project admission). The
// relationship path is a genuine ALTERNATIVE, not gated behind the flat/
// system path succeeding first: an ordinary project member eligible for
// their own agent's attach must be able to mint that selector under a hub
// boundary even with no system role and no blanket project permission grant.
//
// Any failure sets MintDenialProjectAccessRequired for Project-boundary
// admission, or the specific MintEligibilityRegistry-derived reason
// otherwise — no oracle distinguishing "no membership" from "wrong
// permission" for the admission gate itself.
func (a *AuthzService) CanMintSelector(ctx context.Context, principal PrincipalContext, boundary TokenBoundary, selectors []string) ([]SelectorEligibility, error) {
	if err := requireLocalUserPrincipal(principal); err != nil {
		return nil, err
	}
	if !boundary.Valid() {
		return nil, fmt.Errorf("%w: invalid token boundary", ErrProjectAccessDenied)
	}
	if len(selectors) == 0 {
		return nil, ErrEmptySelectorList
	}

	var membershipOK bool
	if boundary.Kind == BoundaryKindProject {
		var err error
		membershipOK, _, err = a.ProjectMembershipEvidence(ctx, principal, boundary.ProjectID)
		if err != nil {
			return nil, err
		}
	}

	results := make([]SelectorEligibility, 0, len(selectors))
	for _, selector := range selectors {
		mapping, ok := permissions.ResolveSelector(selector)
		if !ok {
			results = append(results, SelectorEligibility{Selector: selector, OK: false, Reason: MintDenialUnknownSelector})
			continue
		}
		boundaryOK := false
		for _, allowed := range mapping.AllowedBoundaries {
			if allowed == boundary.Kind {
				boundaryOK = true
				break
			}
		}
		if !boundaryOK {
			results = append(results, SelectorEligibility{Selector: selector, OK: false, Reason: MintDenialBoundaryNotAllowed})
			continue
		}

		if boundary.Kind == BoundaryKindHub {
			eligible, reason, err := a.hubSelectorEligible(ctx, principal, mapping.PermissionIDs)
			if err != nil {
				return nil, err
			}
			results = append(results, SelectorEligibility{Selector: selector, OK: eligible, Reason: reason})
			continue
		}

		admitted, err := a.selectorAdmitted(ctx, principal, boundary, membershipOK, mapping.PermissionIDs)
		if err != nil {
			return nil, err
		}
		if !admitted {
			results = append(results, SelectorEligibility{Selector: selector, OK: false, Reason: MintDenialProjectAccessRequired})
			continue
		}

		eligible, reason, err := a.selectorMintEligible(ctx, principal, boundary, membershipOK, mapping.PermissionIDs)
		if err != nil {
			return nil, err
		}
		results = append(results, SelectorEligibility{Selector: selector, OK: eligible, Reason: reason})
	}
	return results, nil
}

// selectorAdmitted implements the PROJECT-boundary admission step
// (membership or per-permission system authority) for every permission ID
// in a selector's expansion. Hub-boundary admission+eligibility is handled
// entirely by hubSelectorEligible instead (see CanMintSelector).
func (a *AuthzService) selectorAdmitted(ctx context.Context, principal PrincipalContext, boundary TokenBoundary, membershipOK bool, permIDs []string) (bool, error) {
	if membershipOK {
		return true, nil
	}
	for _, permID := range permIDs {
		ok, err := a.SystemAuthorityProof(ctx, principal, boundary.ProjectID, permID, ContemplatedProjectClass(permID))
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

// hubSelectorEligible evaluates every permission ID in a Hub-boundary
// selector's expansion via hubPermissionEligible, combining admission and
// mint eligibility into one per-permission decision (the flat/system path
// and the relationship path are genuine alternatives — see CanMintSelector).
func (a *AuthzService) hubSelectorEligible(ctx context.Context, principal PrincipalContext, permIDs []string) (bool, MintDenialReason, error) {
	for _, permID := range permIDs {
		ok, err := a.hubPermissionEligible(ctx, principal, permID)
		if err != nil {
			return false, MintDenialNone, err
		}
		if !ok {
			return false, MintDenialProjectAccessRequired, nil
		}
	}
	return true, MintDenialNone, nil
}

// hubPermissionEligible answers, for ONE permission ID under a Hub boundary:
// may principal select it? Two alternative paths, either sufficient:
//
//   - Flat/system: MintTimeSystemGrant(ctx, principal, permID) OR held via
//     an active, constraint-reduced project-scoped role binding in ANY
//     project (hasAnyProjectBinding, itself restricted to project-applicable
//     permissions).
//   - Relationship: permID has a MintEligibilityRegistry descriptor with a
//     MintEligibilityRelationship source whose RelationshipTypes include one
//     resolving via permissions.RelationshipPolicyMintEligible for this
//     EXACT principal kind and permID's resource type, AND the principal has
//     relevant project admission where that project's OWN access-constraint
//     reduction does not strip permID (hasRelevantProjectAdmission — no
//     specific target enumerated, but NOT constraint-agnostic: a governing
//     constraint excluding permID from every project the principal belongs
//     to still denies, pat-refactor R3). This is what lets an ordinary
//     project member mint agent:attach under a hub boundary without any
//     system role or blanket project permission grant, while still
//     respecting a constraint that specifically strips that permission.
func (a *AuthzService) hubPermissionEligible(ctx context.Context, principal PrincipalContext, permID string) (bool, error) {
	ok, err := a.MintTimeSystemGrant(ctx, principal, permID)
	if err != nil {
		return false, err
	}
	if ok {
		return true, nil
	}
	ok, err = a.hasAnyProjectBinding(ctx, principal, permID)
	if err != nil {
		return false, err
	}
	if ok {
		return true, nil
	}

	descriptor, hasDescriptor := permissions.MintEligibilityRegistry[permID]
	if !hasDescriptor {
		return false, nil
	}
	resourceType := registryResourceType(permID)
	for _, src := range descriptor.Sources {
		if src.Kind != permissions.MintEligibilityRelationship {
			continue
		}
		for _, relType := range src.RelationshipTypes {
			if !permissions.RelationshipPolicyMintEligible(relType, string(principal.Kind), resourceType, permID) {
				continue
			}
			hasAny, err := a.hasRelevantProjectAdmission(ctx, principal, permID)
			if err != nil {
				return false, err
			}
			if hasAny {
				return true, nil
			}
		}
	}
	return false, nil
}

// selectorMintEligible evaluates MintEligibilityRegistry for every
// permission ID in a PROJECT-boundary selector's expansion. A permission
// absent from the registry defaults to MintEligibilityFlatRole, whose
// eligibility is ALWAYS the project role's own flat permission subset
// (hasProjectRoleFlatPermission) — regardless of how admission was
// established. Flat mint eligibility remains project-binding-only by
// design (pat-refactor R3, 2026-09-28): a super-admin's system-authority
// admission for a project they are not a member of lets them pass the
// admission gate, but it does NOT widen what a flat (non-relationship)
// selector can mint on a project-scoped token — that ceiling is always the
// project's own role definition. Only MintEligibilityRelationship
// selectors (declared resource-relative, e.g. agent.attach/port_access) can
// be minted without a matching project role, via RelationshipPolicyMintEligible
// below, which does not depend on membershipOK either.
func (a *AuthzService) selectorMintEligible(ctx context.Context, principal PrincipalContext, boundary TokenBoundary, membershipOK bool, permIDs []string) (bool, MintDenialReason, error) {
	for _, permID := range permIDs {
		descriptor, hasDescriptor := permissions.MintEligibilityRegistry[permID]
		if !hasDescriptor {
			ok, err := a.hasProjectRoleFlatPermission(ctx, principal, boundary.ProjectID, permID)
			if err != nil {
				return false, MintDenialNone, err
			}
			if !ok {
				return false, MintDenialFlatRoleInsufficient, nil
			}
			continue
		}
		eligible := false
		for _, src := range descriptor.Sources {
			switch src.Kind {
			case permissions.MintEligibilityFlatRole:
				// An explicit FlatRole source is still the project role's
				// own flat permission subset — it must actually be proven,
				// never assumed true just because the descriptor names this
				// source (pat-refactor R3, 2026-09-28: this exact branch
				// was previously `eligible = true` unconditionally).
				ok, err := a.hasProjectRoleFlatPermission(ctx, principal, boundary.ProjectID, permID)
				if err != nil {
					return false, MintDenialNone, err
				}
				if ok {
					eligible = true
				}
			case permissions.MintEligibilityRelationship:
				resourceType := registryResourceType(permID)
				for _, relType := range src.RelationshipTypes {
					// MintEligible, not RelationshipPolicyAllows: a mint
					// eligibility reference must resolve to a row explicitly
					// marked mint-eligible (pat-b-lead correction), not
					// merely a row that permits the action at runtime
					// (RelationshipPolicyAllows also matches e.g. read-only
					// progeny rows that are never mint-eligible). Checked
					// against the actual principal kind, not any kind.
					if !permissions.RelationshipPolicyMintEligible(relType, string(principal.Kind), resourceType, permID) {
						continue
					}
					// Relationship candidacy is permission-agnostic about
					// WHICH role a project membership carries, but it must
					// still respect a constraint that specifically strips
					// this exact permission from the project boundary
					// being minted against (pat-refactor R3): a governing
					// constraint on boundary.ProjectID cannot be worked
					// around via the relationship path.
					constraintOK, err := a.permissionSurvivesProjectConstraints(ctx, principal, boundary.ProjectID, permID)
					if err != nil {
						return false, MintDenialNone, err
					}
					if constraintOK {
						eligible = true
						break
					}
				}
			}
			if eligible {
				break
			}
		}
		if !eligible {
			return false, MintDenialNoRelationshipCandidacy, nil
		}
	}
	return true, MintDenialNone, nil
}
