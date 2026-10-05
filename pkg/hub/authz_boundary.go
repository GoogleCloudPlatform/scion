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
// so the same rule is reachable from pkg/store: pkg/store cannot import
// pkg/hub, but store-layer validation needs this exact rule too — a shared
// table test pins agreement.
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
// resolved Resource plus explicit collection-level evidence, via ONE shared
// fact-computation-and-coherence matrix (computeTargetFacts) used by both
// the plain-resource and collection-evidence paths, so no fast path
// (existing project, Hub singleton) can return before EVERY supplied fact —
// parent type/ID and scope kind — has been checked together, including
// against a resource type that has no parent/scope concept at all:
// contradictory recognized facts must be rejected everywhere, not only in a
// subset of cases.
//
// Collection-level path (evidence.IsCollectionLevel): see resolveCollectionEvidence.
// Plain-resource path:
//
//  1. computeTargetFacts(r.Type, r) — any contradiction resolves Unknown
//     before any other rule runs.
//  2. r.Type == permissions.ResourceProject and r.ID != "": TargetScopeProject.
//  3. r.Type == permissions.ResourceHub: TargetScopeHub.
//  4. facts.hasProjectParent: TargetScopeProject.
//  5. facts.hasSystemParent: TargetScopeHub.
//  6. r.Type is "broker" or "runtime_broker": TargetScopeHub.
//  7. facts.hasGlobalScope: TargetScopeHub.
//  8. Anything else: TargetScopeUnknown. Never equate missing resource
//     parent metadata with hub scope.
func ResolveTargetScope(r Resource, evidence TargetScopeEvidence) TargetScope {
	if evidence.IsCollectionLevel {
		return resolveCollectionEvidence(r, evidence)
	}

	facts, ok := computeTargetFacts(r.Type, r)
	if !ok {
		return TargetScope{Kind: TargetScopeUnknown}
	}

	if r.Type == permissions.ResourceProject && r.ID != "" {
		return TargetScope{Kind: TargetScopeProject, ProjectID: r.ID}
	}
	if r.Type == permissions.ResourceHub {
		return TargetScope{Kind: TargetScopeHub}
	}
	if facts.hasProjectParent {
		return TargetScope{Kind: TargetScopeProject, ProjectID: r.ParentID}
	}
	if facts.hasSystemParent {
		return TargetScope{Kind: TargetScopeHub}
	}
	if r.Type == "broker" || r.Type == "runtime_broker" {
		return TargetScope{Kind: TargetScopeHub}
	}
	if facts.hasGlobalScope {
		return TargetScope{Kind: TargetScopeHub}
	}

	return TargetScope{Kind: TargetScopeUnknown}
}

// resolveCollectionEvidence is ResolveTargetScope's collection-level branch.
// The EFFECTIVE resource type (Resource.Type, or the permission's own
// registry type when Resource.Type is empty) is resolved FIRST, then
// computeTargetFacts validates and computes every fact against THAT
// effective type before any classification — so a bogus ScopeKind on an
// empty-Type Resource cannot slip through before the effective type is
// known to be, say, "skill".
func resolveCollectionEvidence(r Resource, evidence TargetScopeEvidence) TargetScope {
	// Fact: a collection-level request must not also name an existing
	// resource instance.
	if r.ID != "" {
		return TargetScope{Kind: TargetScopeUnknown}
	}

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

	facts, ok := computeTargetFacts(effectiveType, r)
	if !ok {
		return TargetScope{Kind: TargetScopeUnknown}
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

	switch evidence.CollectionScope {
	case TargetScopeHub:
		if evidence.CollectionProjectID != "" {
			return TargetScope{Kind: TargetScopeUnknown} // stray project ID on Hub evidence
		}
		if !classesInclude(classes, permissions.TargetClassKindGlobalCatalog, permissions.TargetClassKindHubResource) {
			return TargetScope{Kind: TargetScopeUnknown} // Hub is not a reviewed class for this permission
		}
		if facts.hasProjectParent {
			return TargetScope{Kind: TargetScopeUnknown} // resource independently claims a project parent
		}
		if facts.hasProjectScope {
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
		if facts.hasProjectParent && r.ParentID != evidence.CollectionProjectID {
			return TargetScope{Kind: TargetScopeUnknown} // resource names a DIFFERENT project than the evidence
		}
		if facts.hasSystemParent {
			return TargetScope{Kind: TargetScopeUnknown} // resource independently claims system/hub scope
		}
		if facts.hasGlobalScope {
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

// targetFacts is the fully-validated, independent fact set computeTargetFacts
// produces for one (effectiveType, Resource) pair.
type targetFacts struct {
	hasProjectParent bool
	hasSystemParent  bool
	hasGlobalScope   bool // explicit global/core/user ScopeKind, reviewed types only
	hasProjectScope  bool // explicit "project" ScopeKind, reviewed types only
}

// computeTargetFacts is the ONE shared validation matrix both
// ResolveTargetScope paths use. ok=false for ANY internal contradiction —
// checked together, before the caller is allowed to return anything for
// effectiveType/r:
//
//   - r.ParentType is not one of "", "project", "system" (unrecognized —
//     supplied-but-unrecognized is a contradiction, not absent metadata).
//   - r.ParentID is set with an empty r.ParentType (orphan ParentID).
//   - r.ParentType == "project" with an empty r.ParentID (malformed parent).
//   - r.ParentType == "system" with a non-empty r.ParentID (malformed parent).
//   - r.ParentType is set AT ALL on a resource type with NO parent concept —
//     project and Hub are singleton/global types; a project cannot have
//     a project OR system parent, and neither can the Hub singleton. A
//     supplied parent on either must be rejected rather than ignored, so
//     an existing project resource with a spurious project parent, a Hub
//     resource with a spurious project parent, and a project ID paired
//     with a malformed system parent are all rejected before any fast
//     path can return.
//   - r.ScopeKind is set on a resource type with NO scope-kind concept
//     (only skill/template/harness_config have one).
//   - r.ScopeKind is an unrecognized value for a resource type that DOES
//     have scope-kind semantics.
//   - The resulting facts cross-contradict: project-parented AND globally
//     scoped, or system-parented AND explicitly project-scoped (e.g. "plain
//     skill with system parent + ScopeKind=project").
func computeTargetFacts(effectiveType string, r Resource) (targetFacts, bool) {
	switch r.ParentType {
	case "", "project", "system":
	default:
		return targetFacts{}, false
	}
	if r.ParentType == "" && r.ParentID != "" {
		return targetFacts{}, false
	}
	if r.ParentType == "project" && r.ParentID == "" {
		return targetFacts{}, false
	}
	if r.ParentType == "system" && r.ParentID != "" {
		return targetFacts{}, false
	}
	if (effectiveType == permissions.ResourceProject || effectiveType == permissions.ResourceHub) && r.ParentType != "" {
		return targetFacts{}, false
	}

	hasScopeKindSemantics := isReviewedScopeKindResourceType(effectiveType)
	if !hasScopeKindSemantics && r.ScopeKind != "" {
		return targetFacts{}, false
	}
	if hasScopeKindSemantics && !isRecognizedScopeKindValue(r.ScopeKind) {
		return targetFacts{}, false
	}

	facts := targetFacts{
		hasProjectParent: r.ParentType == "project",
		hasSystemParent:  r.ParentType == "system",
	}
	if hasScopeKindSemantics {
		switch r.ScopeKind {
		case store.SkillScopeGlobal, store.SkillScopeCore, store.SkillScopeUser:
			facts.hasGlobalScope = true
		case store.SkillScopeProject:
			facts.hasProjectScope = true
		}
	}
	if facts.hasProjectParent && facts.hasGlobalScope {
		return targetFacts{}, false
	}
	if facts.hasSystemParent && facts.hasProjectScope {
		return targetFacts{}, false
	}
	return facts, true
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

// projectAccessLookupError marks a project-access evidence error caused by a
// store or resolution fault (principal closure, role bindings, role
// definitions, user record, access constraints), as opposed to a policy fact
// such as an inactive user, a mismatched target class, or an unsupported
// principal kind. It is transparent: Error and Unwrap return the wrapped
// error unchanged, so errors.Is(err, ErrProjectAccessDenied) and the error
// text are unaffected. Callers that map errors to a Decision use
// isProjectAccessLookupFault to tag the deny as DenyCauseResolutionError.
type projectAccessLookupError struct{ err error }

func (e *projectAccessLookupError) Error() string { return e.err.Error() }
func (e *projectAccessLookupError) Unwrap() error { return e.err }

// projectAccessLookupFault wraps err as a store/resolution fault. See
// projectAccessLookupError.
func projectAccessLookupFault(err error) error {
	return &projectAccessLookupError{err: err}
}

// isProjectAccessLookupFault reports whether err (or anything it wraps) was
// produced by projectAccessLookupFault.
func isProjectAccessLookupFault(err error) bool {
	var lookupErr *projectAccessLookupError
	return errors.As(err, &lookupErr)
}

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
	if cache := mintEligibilityCacheFromContext(ctx); cache != nil && cache.principalMatches(principal) {
		cache.mu.Lock()
		if cache.closureLoaded {
			refs, directKey, groupKeys, err = cache.closureRefs, cache.closureDirectKey, cache.closureGroupKeys, cache.closureErr
			cache.mu.Unlock()
			return refs, directKey, groupKeys, err
		}
		cache.mu.Unlock()

		refs, directKey, groupKeys, err = a.loadPrincipalClosure(ctx, principal)

		cache.mu.Lock()
		cache.closureLoaded = true
		cache.closureRefs, cache.closureDirectKey, cache.closureGroupKeys, cache.closureErr = refs, directKey, groupKeys, err
		cache.mu.Unlock()
		return refs, directKey, groupKeys, err
	}
	return a.loadPrincipalClosure(ctx, principal)
}

// loadPrincipalClosure is principalClosure's uncached body, split out so the
// mint-eligibility cache wrapper above never duplicates this logic.
func (a *AuthzService) loadPrincipalClosure(ctx context.Context, principal PrincipalContext) (refs []store.PrincipalRef, directKey string, groupKeys map[string]bool, err error) {
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
	for _, gid := range groupIDs {
		refs = append(refs, store.PrincipalRef{Type: "group", ID: gid})
		groupKeys["group:"+gid] = true
	}
	return refs, directKey, groupKeys, nil
}

// unionScopedRoleBindingPermissions is projectScopedGrants's binding-union
// step: given a pre-fetched binding list, it unions the permission IDs
// granted by every ACTIVE project-scoped binding whose ScopeID matches
// projectID. A role definition referenced by an active binding but
// missing/unloadable is a data-integrity error, not a routine "skip and
// continue" case: this function propagates that error to the caller (fail
// closed) rather than silently omitting the binding's permissions. It does
// not apply access-constraint reduction; the caller does that afterward.
func (a *AuthzService) unionScopedRoleBindingPermissions(ctx context.Context, bindings []*store.RoleBinding, projectID string, now time.Time) ([]string, error) {
	seen := make(map[string]bool)
	var result []string
	for _, b := range bindings {
		if b.ScopeType != ScopeTypeProject || b.ScopeID != projectID {
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
	return result, nil
}

// projectScopedGrants is the shared resolution behind getProjectScopedPermissions
// (authz.go, serving useraccesstoken.go's project-scoped mint-ceiling
// resolution) and projectScopedPermissionsStrict below (serving
// CanMintSelector's flat-role mint path): it resolves principal's
// group-expanded closure and the permission IDs granted by every active
// project-scoped role binding for projectID, via
// unionScopedRoleBindingPermissions. It does not apply access-constraint
// reduction; each caller does that afterward with its own error-handling
// choice, since one fails closed on a load error (a deny-all restriction)
// and the other returns the error.
func (a *AuthzService) projectScopedGrants(ctx context.Context, principalType, principalID, projectID string) (perms []string, closure map[string]struct{}, err error) {
	normalizedType := NormalizePrincipalType(principalType)

	principals := []store.PrincipalRef{{Type: normalizedType, ID: principalID}}
	var groupIDs []string
	var groupErr error
	switch normalizedType {
	case store.RoleBindingPrincipalUser:
		groupIDs, groupErr = a.store.GetEffectiveGroups(ctx, principalID)
	case store.RoleBindingPrincipalAgent:
		groupIDs, groupErr = a.store.GetEffectiveGroupsForAgent(ctx, principalID)
	}
	if groupErr != nil && !errors.Is(groupErr, store.ErrNotFound) {
		a.logger.Warn("failed to get effective groups for project-scoped permission resolution (fail-closed)",
			"principalType", principalType, "principalID", principalID, "error", groupErr)
		return nil, nil, fmt.Errorf("group resolution failed (fail-closed): %w", groupErr)
	}
	for _, gid := range groupIDs {
		principals = append(principals, store.PrincipalRef{Type: "group", ID: gid})
	}

	bindings, err := a.store.ListRoleBindingsForPrincipals(ctx, principals, nil, nil)
	if err != nil {
		return nil, nil, err
	}

	closure = make(map[string]struct{}, len(principals))
	for _, p := range principals {
		closure[p.Type+":"+p.ID] = struct{}{}
	}

	perms, err = a.unionScopedRoleBindingPermissions(ctx, bindings, projectID, time.Now())
	if err != nil {
		return nil, nil, err
	}
	return perms, closure, nil
}

// projectScopedPermissionsStrict is getProjectScopedPermissions's
// error-returning form, confined to CanMintSelector's flat-role mint path
// (hasProjectRoleFlatPermission). It shares projectScopedGrants's group and
// binding resolution with getProjectScopedPermissions, but applies
// access-constraint reduction via accessConstraintRestrictions instead of
// loadAccessConstraintRestrictions, so a transient constraint-table load
// failure surfaces as an error. Every error this function returns is wrapped
// in ErrProjectAccessDenied, matching every other CanMintSelector path's
// error classification.
func (a *AuthzService) projectScopedPermissionsStrict(ctx context.Context, principalType, principalID, projectID string) ([]string, error) {
	perms, closure, err := a.projectScopedGrants(ctx, principalType, principalID, projectID)
	if err != nil {
		return nil, projectAccessLookupFault(fmt.Errorf("%w: %v", ErrProjectAccessDenied, err))
	}
	if len(perms) == 0 {
		return perms, nil
	}
	restrictions, err := a.accessConstraintRestrictions(ctx, closure, ResourceContext{ProjectID: projectID})
	if err != nil {
		return nil, projectAccessLookupFault(fmt.Errorf("%w: access constraint load failed: %v", ErrProjectAccessDenied, err))
	}
	return applyRestrictions(perms, restrictions), nil
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

	directKey, groupKeys, bindings, err := a.projectMembershipInputs(ctx, principal)
	if err != nil {
		return false, "", err
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

// projectMembershipInputs loads the principal closure and its unscoped role
// bindings for ProjectMembershipEvidence. Its query shape (full closure, then
// ListRoleBindingsForPrincipals with nil scope filters) is exactly the one
// the request-local authz input memo serves, so when a memo is installed
// and the mint-eligibility cache is not, it reads through the memo instead
// of reloading: a scoped UAT's live project-access check and decide's own
// steps 2-3 for the same principal then share one memo entry. Without a
// memo (or with the mint-eligibility cache present) it loads exactly as
// before. Load failures are tagged as project-access lookup faults on both
// paths.
func (a *AuthzService) projectMembershipInputs(ctx context.Context, principal PrincipalContext) (directKey string, groupKeys map[string]bool, bindings []*store.RoleBinding, err error) {
	if mintEligibilityCacheFromContext(ctx) == nil {
		if h := a.inputsForPrincipal(ctx, principal); h != nil {
			refs, err := h.Principals()
			if err != nil {
				return "", nil, nil, projectAccessLookupFault(fmt.Errorf("%w: group resolution failed (fail-closed): %v", ErrProjectAccessDenied, err))
			}
			bindings, err := h.Bindings()
			if err != nil {
				return "", nil, nil, projectAccessLookupFault(fmt.Errorf("%w: binding resolution failed: %v", ErrProjectAccessDenied, err))
			}
			directKey, groupKeys = closureKeys(refs)
			return directKey, groupKeys, bindings, nil
		}
	}

	refs, directKey, groupKeys, err := a.principalClosure(ctx, principal)
	if err != nil {
		return "", nil, nil, projectAccessLookupFault(fmt.Errorf("%w: %v", ErrProjectAccessDenied, err))
	}
	bindings, err = a.store.ListRoleBindingsForPrincipals(ctx, refs, nil, nil)
	if err != nil {
		return "", nil, nil, projectAccessLookupFault(fmt.Errorf("%w: binding resolution failed: %v", ErrProjectAccessDenied, err))
	}
	return directKey, groupKeys, bindings, nil
}

// closureKeys derives principalClosure's directKey and groupKeys from a
// closure in authorizationPrincipals' shape: the direct principal first,
// followed by its group refs.
func closureKeys(refs []store.PrincipalRef) (directKey string, groupKeys map[string]bool) {
	groupKeys = map[string]bool{}
	for i, r := range refs {
		if i == 0 {
			directKey = r.Type + ":" + r.ID
			continue
		}
		if r.Type == "group" {
			groupKeys["group:"+r.ID] = true
		}
	}
	return directKey, groupKeys
}

func (a *AuthzService) requireActiveUser(ctx context.Context, principal PrincipalContext) error {
	normalizedType := NormalizePrincipalType(string(principal.Kind))
	if normalizedType != store.RoleBindingPrincipalUser {
		return nil
	}
	user, err := a.store.GetUser(ctx, principal.ID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return projectAccessLookupFault(fmt.Errorf("%w: user lookup failed: %v", ErrProjectAccessDenied, err))
	}
	// A missing user record is a policy fact (the holder no longer exists),
	// not a store fault: deny the same way as an inactive user, untagged.
	if err != nil || user == nil || user.Status != store.UserStatusActive {
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
// dispatch entirely), or an unrecognized ScopeKind for a resource type with
// reviewed scope-kind semantics (see validRealProjectScopeKinds). This is an
// ALLOWLIST, not a blocklist: for a resource type with reviewed semantics,
// an unrecognized ScopeKind value is rejected just as surely as a known but
// wrong one — a real project-target proof is never "the global catalog,"
// and never a value outside the reviewed set. A resource type with no entry
// in validRealProjectScopeKinds at all has no reviewed ScopeKind concept,
// so its class.ScopeKind must be exactly empty.
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

// validRealProjectScopeKinds is the explicit, reviewed ALLOWLIST of
// ScopeKind values each resource type may carry for a REAL (not
// contemplated) project-target class. A resource type absent from this map
// has NO reviewed scope-kind semantics at all: its class.ScopeKind must be
// exactly empty — never an arbitrary accepted string. Material resource
// types (secret/env_var/skill_injection) carry the reviewed scope kinds of
// their stored scope; their Registry rows are live, pinned by
// TestValidRealProjectScopeKinds_MaterialRowsNowLive.
var validRealProjectScopeKinds = map[string][]string{
	permissions.ResourceSkill:          {store.SkillScopeProject},
	permissions.ResourceTemplate:       {store.TemplateScopeProject},
	permissions.ResourceHarnessConfig:  {store.HarnessConfigScopeProject},
	permissions.ResourceSecret:         {store.ScopeProject, store.ScopeHub, store.ScopeUser, store.ScopeRuntimeBroker},
	permissions.ResourceEnvVar:         {store.ScopeProject, store.ScopeHub, store.ScopeUser, store.ScopeRuntimeBroker},
	permissions.ResourceSkillInjection: {store.ScopeProject, store.ScopeHub, store.ScopeUser, store.ScopeRuntimeBroker},
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

// mintEligibilityCacheKey is the context.Context key for an optional,
// per-call mint-eligibility cache (see mintEligibilityCache).
type mintEligibilityCacheKey struct{}

// mintEligibilityCache memoizes the principal's active system-scope
// candidate set and the full access-constraint table for the lifetime of
// ONE CanMintSelector call. Evaluating a selector expansion or a *:manage
// alias means calling SystemAuthorityProof/MintTimeSystemGrant/
// hasAnyProjectBinding/hasRelevantProjectAdmission once per permission (and
// permissionSurvivesProjectConstraints once per relevant project;
// hasProjectRoleFlatPermission/projectScopedPermissionsStrict once per
// flat-role permission, reading the constraint slot only — it resolves its
// own group/binding closure rather than using the cached one); without this
// cache, each of those calls independently re-resolves the principal's
// group/binding closure, reloads role definitions, and re-pages the entire
// access-constraint table. Carried via context (not a new parameter) so it
// requires no change to any exported function's signature: every reader
// falls back to loading fresh when the cache is absent (e.g. every caller
// outside CanMintSelector), so this is purely a performance optimization and
// never a source of eligibility facts: every reader that surfaces an error
// (closureErr/systemErr/constraintsErr) does so because the SAME load would
// have failed uncached too. The one externally visible difference is scope,
// not existence: a single failed page is shared by every permission in the
// batch that reaches it, where an uncached run could have retried the load
// per permission and possibly succeeded on a later attempt. It never turns a
// real failure into a fabricated denial or an allow — every caller listed
// above propagates the cached error as an error.
type mintEligibilityCache struct {
	mu sync.Mutex

	// principalSet/principalKind/principalID record the FIRST principal any
	// caller populated the closure/system slots for. CanMintSelector installs
	// a fresh cache per call for a single principal, so this is always the
	// same principal on every read in practice; it exists so a future caller
	// that reused one cache across two different principals is detected and
	// falls back to an uncached load (principalMatches) instead of silently
	// handing the second principal the first principal's closure/authority.
	// Deliberately does not gate constraintsLoaded/constraints: the
	// access-constraint table is global, not principal-scoped.
	principalSet  bool
	principalKind PrincipalKind
	principalID   string

	closureLoaded    bool
	closureRefs      []store.PrincipalRef
	closureDirectKey string
	closureGroupKeys map[string]bool
	closureErr       error

	systemLoaded     bool
	systemCandidates []CandidateBinding
	systemRoleDefs   map[string]*RolePermissions
	systemRefs       []store.PrincipalRef
	systemErr        error

	constraintsLoaded bool
	constraints       []*store.AccessConstraint
	constraintsErr    error
}

// Invariant (shared with authzInputMemo, see authz_request_inputs.go): never
// install authzInputMemo inside CanMintSelector or its callees. The two
// caches interact only at loadAllAccessConstraints, where the mint cache
// always wins when both are present; getCachedDelegationEdges never
// consults the mint cache at all. CanMintSelector defensively masks any
// authzInputMemo at entry so that the loadAllAccessConstraints interaction
// is never actually reached in production.
func withMintEligibilityCache(ctx context.Context, c *mintEligibilityCache) context.Context {
	return context.WithValue(ctx, mintEligibilityCacheKey{}, c)
}

func mintEligibilityCacheFromContext(ctx context.Context) *mintEligibilityCache {
	c, _ := ctx.Value(mintEligibilityCacheKey{}).(*mintEligibilityCache)
	return c
}

// principalMatches reports whether c's closure/system-scope slots may be
// used for principal: the first call records the principal, and every later
// call must match it exactly (both Kind and ID). A mismatch returns false,
// telling the caller to skip the cache (load uncached) rather than read or
// overwrite state populated for a different principal.
func (c *mintEligibilityCache) principalMatches(principal PrincipalContext) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.principalSet {
		c.principalSet = true
		c.principalKind = principal.Kind
		c.principalID = principal.ID
		return true
	}
	return c.principalKind == principal.Kind && c.principalID == principal.ID
}

// activeSystemScopeCandidates loads principal's active system-scope role
// bindings as kernel CandidateBinding/RolePermissions structures, ready for
// applyHubWideScopeFilters. Shared by SystemAuthorityProof and
// MintTimeSystemGrant.
func (a *AuthzService) activeSystemScopeCandidates(ctx context.Context, principal PrincipalContext) ([]CandidateBinding, map[string]*RolePermissions, []store.PrincipalRef, error) {
	cache := mintEligibilityCacheFromContext(ctx)
	usable := cache != nil && cache.principalMatches(principal)
	if usable {
		cache.mu.Lock()
		if cache.systemLoaded {
			candidates, roleDefs, refs, err := cache.systemCandidates, cache.systemRoleDefs, cache.systemRefs, cache.systemErr
			cache.mu.Unlock()
			return candidates, roleDefs, refs, err
		}
		cache.mu.Unlock()
	}

	candidates, roleDefs, refs, err := a.loadActiveSystemScopeCandidates(ctx, principal)

	if usable {
		cache.mu.Lock()
		cache.systemLoaded = true
		cache.systemCandidates, cache.systemRoleDefs, cache.systemRefs, cache.systemErr = candidates, roleDefs, refs, err
		cache.mu.Unlock()
	}
	return candidates, roleDefs, refs, err
}

// loadActiveSystemScopeCandidates is activeSystemScopeCandidates's
// uncached body, split out so the cache wrapper above never duplicates
// this logic.
func (a *AuthzService) loadActiveSystemScopeCandidates(ctx context.Context, principal PrincipalContext) ([]CandidateBinding, map[string]*RolePermissions, []store.PrincipalRef, error) {
	refs, _, _, err := a.principalClosure(ctx, principal)
	if err != nil {
		return nil, nil, nil, projectAccessLookupFault(fmt.Errorf("%w: %v", ErrProjectAccessDenied, err))
	}
	bindings, err := a.store.ListRoleBindingsForPrincipals(ctx, refs, nil, nil)
	if err != nil {
		return nil, nil, nil, projectAccessLookupFault(fmt.Errorf("%w: binding resolution failed: %v", ErrProjectAccessDenied, err))
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
		return nil, nil, nil, projectAccessLookupFault(fmt.Errorf("%w: role resolution failed: %v", ErrProjectAccessDenied, err))
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

	// Access-constraint reduction, project-scoped. Uses the error-returning
	// accessConstraintRestrictions rather than loadAccessConstraintRestrictions:
	// ProjectAdmissionForClass's memo must never cache a denial caused by a
	// transient constraint-table load failure, only a real, reviewable
	// admission decision — so the load error is returned here and propagated,
	// which ProjectAdmissionForClass already treats as unmemoized.
	closure := make(map[string]struct{}, len(refs))
	for _, p := range refs {
		closure[p.Type+":"+p.ID] = struct{}{}
	}
	restrictions, err := a.accessConstraintRestrictions(ctx, closure, ResourceContext{ProjectID: projectID})
	if err != nil {
		return false, projectAccessLookupFault(fmt.Errorf("%w: access constraint load failed: %v", ErrProjectAccessDenied, err))
	}
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
// ProjectID) system-scope grants for ANY supported class. A hub-member's
// catalog-only skill.read therefore remains hub-boundary mint-eligible for
// the global catalog, while ProjectTargetAdmission still resolves a real
// project target by its actual ScopeKind, independent of contemplation.
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
	// Uses the error-returning accessConstraintRestrictions rather than
	// loadAccessConstraintRestrictions: MintTimeSystemGrant has its own error
	// return, and CanMintSelector (its only production caller) already fails
	// the whole batch closed on an error, so a transient load failure should
	// surface as an error rather than be silently absorbed into a
	// stable-looking denial reason.
	restrictions, err := a.accessConstraintRestrictions(ctx, closure, ResourceContext{})
	if err != nil {
		return false, projectAccessLookupFault(fmt.Errorf("%w: access constraint load failed: %v", ErrProjectAccessDenied, err))
	}

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
			// potentially grant on it — skip it explicitly: fail closed on
			// an unknown class, the same way an absent SupportedTargetClasses
			// entry already does.
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
		return false, projectAccessLookupFault(fmt.Errorf("%w: %v", ErrProjectAccessDenied, err))
	}
	bindings, err := a.store.ListRoleBindingsForPrincipals(ctx, refs, []string{store.RoleScopeProject}, nil)
	if err != nil {
		return false, projectAccessLookupFault(fmt.Errorf("%w: binding resolution failed: %v", ErrProjectAccessDenied, err))
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
		return false, projectAccessLookupFault(fmt.Errorf("%w: role definition resolution failed: %v", ErrProjectAccessDenied, err))
	}

	closure := make(map[string]struct{}, len(refs))
	for _, p := range refs {
		closure[p.Type+":"+p.ID] = struct{}{}
	}

	// Evaluate EACH project independently, with that project's own
	// access-constraint reduction (ResourceContext{ProjectID: thatProject})
	// — a constraint governing one project must not be diluted by merging
	// its bindings with an unconstrained second project's, and must not be
	// evaluated as a system-wide constraint (ResourceContext{}) instead.
	// Succeeds only if at least one actual project's own constrained grant
	// survives.
	for projectID, projBindings := range byProject {
		if !candidateSetHasPermission(toCandidateBindings(projBindings), roleDefs, permissionID) {
			continue
		}
		// Error-returning form: hasAnyProjectBinding already propagates every
		// other internal failure (closure, binding-list, role-definition
		// resolution) as an error rather than a silent deny-all, so a
		// transient constraint-table load failure must do the same instead
		// of masquerading as an ordinary denial.
		restrictions, err := a.accessConstraintRestrictions(ctx, closure, ResourceContext{ProjectID: projectID})
		if err != nil {
			return false, projectAccessLookupFault(fmt.Errorf("%w: access constraint load failed: %v", ErrProjectAccessDenied, err))
		}
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
		return false, projectAccessLookupFault(fmt.Errorf("%w: %v", ErrProjectAccessDenied, err))
	}
	closure := make(map[string]struct{}, len(refs))
	for _, p := range refs {
		closure[p.Type+":"+p.ID] = struct{}{}
	}
	// Error-returning form, matching principalClosure's error propagation
	// just above: a transient constraint-table load failure must surface as
	// an error here too, not as an indistinguishable "constraint stripped
	// it" denial.
	restrictions, err := a.accessConstraintRestrictions(ctx, closure, ResourceContext{ProjectID: projectID})
	if err != nil {
		return false, projectAccessLookupFault(fmt.Errorf("%w: access constraint load failed: %v", ErrProjectAccessDenied, err))
	}
	survivors := applyRestrictions([]string{permissionID}, restrictions)
	return len(survivors) == 1, nil
}

// hasRelevantProjectAdmission reports whether principal has an active
// project-scoped role binding in AT LEAST ONE project WHERE that project's
// own access-constraint reduction does not strip permissionID — the
// "relevant project admission" evidence for a relationship-eligible
// selector under a Hub boundary. This prevents a governing constraint that
// excludes the permission for every project the principal belongs to from
// being worked around via the relationship path: relationship mint
// candidacy is permission-agnostic about WHICH role a project membership
// carries, but it must still respect a
// constraint that specifically strips the selected permission. Does not
// enumerate any specific owned target — project-level only.
func (a *AuthzService) hasRelevantProjectAdmission(ctx context.Context, principal PrincipalContext, permissionID string) (bool, error) {
	refs, _, _, err := a.principalClosure(ctx, principal)
	if err != nil {
		return false, projectAccessLookupFault(fmt.Errorf("%w: %v", ErrProjectAccessDenied, err))
	}
	bindings, err := a.store.ListRoleBindingsForPrincipals(ctx, refs, []string{store.RoleScopeProject}, nil)
	if err != nil {
		return false, projectAccessLookupFault(fmt.Errorf("%w: binding resolution failed: %v", ErrProjectAccessDenied, err))
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
// request-local authority cache). nil is safe (unmemoized), and so is its
// zero value — the underlying map is initialized lazily on first put, so a
// zero-value or literal-built ProjectAdmissionCache{} is ready to use without
// calling NewProjectAdmissionCache. Never persisted or shared ACROSS
// requests. An error returned FROM ProjectAdmissionForClass is NEVER cached
// — a failed lookup, including a failure to load the access-constraint
// table, is recomputed on the next call, never remembered as a denial or an
// allow.
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
	if c.cache == nil {
		c.cache = make(map[projectAdmissionCacheKey]ProjectAdmissionResult)
	}
	c.cache[key] = v
}

// ErrProjectMismatch is returned by ProjectTargetAdmission when target's
// resolved project does not equal the supplied projectID — callers must not
// pass a mismatched pair.
var ErrProjectMismatch = errors.New("target project does not match requested projectID")

// ProjectTargetAdmission is the ONE runtime composition the bearer gate
// (evaluateBearerGate, for every boundary kind) calls for an
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
// error RETURNED BY THIS FUNCTION denies and is never memoized — a failed
// lookup, including a failure to load the access-constraint table on the
// system-authority path, is recomputed on the next call, never remembered
// as a denial or an allow. The memo (nil-safe) is keyed on
// (principal, projectID, permissionID, class).
//
// class.ScopeKind must be exactly empty for a resource type with no
// reviewed scope-kind semantics, and one of validRealProjectScopeKinds's
// registered values otherwise (skill/template/harness_config require their
// own "project" constant, never empty) — validateRealProjectClass rejects
// anything else, including an empty ScopeKind for one of those three types.
// A caller building class by hand for a real skill/template/harness_config
// target must set ScopeKind explicitly; callers with a full Resource should
// use ProjectTargetAdmission with the canonical constructor for that
// resource type (skillResource, templateResource, harnessConfigResource)
// instead of assembling ProjectTargetClass directly.
func (a *AuthzService) ProjectAdmissionForClass(ctx context.Context, principal PrincipalContext, projectID, permissionID string, class ProjectTargetClass, memo *ProjectAdmissionCache) (ProjectAdmissionResult, error) {
	if err := requireLocalUserPrincipal(principal); err != nil {
		return ProjectAdmissionResult{}, err
	}
	if projectID == "" {
		return ProjectAdmissionResult{}, fmt.Errorf("%w: empty project ID", ErrProjectAccessDenied)
	}
	// Validate class against permissionID BEFORE either branch: a
	// successful membership check must not skip
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
	// Uses projectScopedPermissionsStrict, not getProjectScopedPermissions:
	// this is CanMintSelector's flat-role mint path, and a transient
	// constraint-load failure here must surface as an error like every other
	// CanMintSelector path, not a deny-all restriction.
	perms, err := a.projectScopedPermissionsStrict(ctx, string(principal.Kind), principal.ID, projectID)
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
// silently treated as a trivially successful validation.
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
// ContemplatedProjectClass(permID)).
//
// If admitted, each permission without a MintEligibilityRegistry descriptor
// additionally requires the project role's own flat permission subset
// (hasProjectRoleFlatPermission), regardless of whether admission came from
// membership or from system authority; system authority admits but never
// widens the flat ceiling.
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

	// Share one mint-eligibility cache across every selector/permission this
	// call evaluates below (see mintEligibilityCache) — resolving it once
	// per CanMintSelector call rather than once per permission.
	ctx = withMintEligibilityCache(ctx, &mintEligibilityCache{})

	// Defensive: hide any authzInputMemo (and its edges slot) an outer
	// caller may have installed, and make the mask sticky so nothing
	// reached from here can re-enable one. No production install site is
	// reachable from CanMintSelector today, but this keeps the two
	// caches' semantics from ever being able to interact.
	ctx = maskAllAuthzMemo(ctx)

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

		eligible, reason, err := a.selectorMintEligible(ctx, principal, boundary, mapping.PermissionIDs)
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
//     to still denies. This is what lets an ordinary
//     project member mint agent:attach under a hub boundary without any
//     system role or blanket project permission grant, while still
//     respecting a constraint that specifically strips that permission.
func (a *AuthzService) hubPermissionEligible(ctx context.Context, principal PrincipalContext, permID string) (bool, error) {
	if permissions.IsSelfPermission(permID) {
		return a.selfPermissionMintEligible(ctx, principal)
	}
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
	// hasRelevantProjectAdmission's result depends only on (principal,
	// permID), never on relType, so it is computed at most once here —
	// lazily, only once some RelationshipTypes entry is actually mint-
	// eligible for this principal kind/resource type/permission — rather
	// than once per matching relType.
	resourceType := registryResourceType(permID)
	relationshipCandidate := false
	for _, src := range descriptor.Sources {
		if src.Kind != permissions.MintEligibilityRelationship {
			continue
		}
		for _, relType := range src.RelationshipTypes {
			if permissions.RelationshipPolicyMintEligible(relType, permissions.RelationshipPrincipalKind(string(principal.Kind)), resourceType, permID) {
				relationshipCandidate = true
				break
			}
		}
		if relationshipCandidate {
			break
		}
	}
	if !relationshipCandidate {
		return false, nil
	}
	return a.hasRelevantProjectAdmission(ctx, principal, permID)
}

// selfPermissionMintEligible is the mint eligibility rule for a self
// permission (permissions.IsSelfPermission): the issuer is an active user.
// No role binding is consulted, because a self permission acts only on the
// holder's own records. Project-boundary admission (membership) still
// applies before this rule. A store fault is returned as an error; an
// inactive or missing user is ineligible.
func (a *AuthzService) selfPermissionMintEligible(ctx context.Context, principal PrincipalContext) (bool, error) {
	if err := a.requireActiveUser(ctx, principal); err != nil {
		if isProjectAccessLookupFault(err) {
			return false, err
		}
		return false, nil
	}
	return true, nil
}

// selectorMintEligible evaluates MintEligibilityRegistry for every
// permission ID in a PROJECT-boundary selector's expansion. A permission
// absent from the registry defaults to MintEligibilityFlatRole, whose
// eligibility is ALWAYS the project role's own flat permission subset
// (hasProjectRoleFlatPermission) — regardless of how admission was
// established. Flat mint eligibility remains project-binding-only by
// design: a super-admin's system-authority
// admission for a project they are not a member of lets them pass the
// admission gate, but it does NOT widen what a flat (non-relationship)
// selector can mint on a project-scoped token — that ceiling is always the
// project's own role definition. Only MintEligibilityRelationship
// selectors (declared resource-relative, e.g. agent.attach/port_access) can
// be minted without a matching project role, via RelationshipPolicyMintEligible
// below.
func (a *AuthzService) selectorMintEligible(ctx context.Context, principal PrincipalContext, boundary TokenBoundary, permIDs []string) (bool, MintDenialReason, error) {
	for _, permID := range permIDs {
		if permissions.IsSelfPermission(permID) {
			ok, err := a.selfPermissionMintEligible(ctx, principal)
			if err != nil {
				return false, MintDenialNone, err
			}
			if !ok {
				return false, MintDenialProjectAccessRequired, nil
			}
			continue
		}
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
		sawRelationshipSource := false
		for _, src := range descriptor.Sources {
			switch src.Kind {
			case permissions.MintEligibilityFlatRole:
				// An explicit FlatRole source is still the project role's
				// own flat permission subset — it must actually be proven,
				// never assumed true just because the descriptor names this
				// source: an explicit MintEligibilityFlatRole descriptor
				// does not exempt a permission from the flat project-role
				// check either.
				ok, err := a.hasProjectRoleFlatPermission(ctx, principal, boundary.ProjectID, permID)
				if err != nil {
					return false, MintDenialNone, err
				}
				if ok {
					eligible = true
				}
			case permissions.MintEligibilityRelationship:
				sawRelationshipSource = true
				resourceType := registryResourceType(permID)
				for _, relType := range src.RelationshipTypes {
					// MintEligible, not RelationshipPolicyAllows: a mint
					// eligibility reference must resolve to a row explicitly
					// marked mint-eligible, not merely a row that permits the
					// action at runtime (RelationshipPolicyAllows also
					// matches e.g. read-only progeny rows that are never
					// mint-eligible). Checked against the actual principal
					// kind, not any kind.
					if !permissions.RelationshipPolicyMintEligible(relType, permissions.RelationshipPrincipalKind(string(principal.Kind)), resourceType, permID) {
						continue
					}
					// Relationship candidacy is permission-agnostic about
					// WHICH role a project membership carries, but it must
					// still respect a constraint that specifically strips
					// this exact permission from the project boundary
					// being minted against: a governing
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
			if !sawRelationshipSource {
				return false, MintDenialFlatRoleInsufficient, nil
			}
			return false, MintDenialNoRelationshipCandidacy, nil
		}
	}
	return true, MintDenialNone, nil
}
