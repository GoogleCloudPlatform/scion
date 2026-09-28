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
	// request is for. ResolveTargetScope cross-checks it: evidence is only
	// honored when permissions.ProjectTargetApplicability[PermissionID] ==
	// false (project.create, skill.create_global — genuinely hub-level
	// collection actions). Evidence supplied for a project-applicable
	// permission is a contract misuse and resolves Unknown, not Hub.
	PermissionID string
}

// ResolveTargetScope classifies a request for boundary matching from the
// resolved Resource plus explicit collection-level evidence.
//
// Rules, applied in order:
//
//  0. evidence.IsCollectionLevel: the evidence's CollectionScope must AGREE
//     with permissions.AppliesToExistingProjectTarget(evidence.PermissionID)'s
//     reviewed disposition — Hub evidence for a project-applicable
//     permission (or vice versa), an unreviewed permission ID, a stray
//     CollectionProjectID on Hub evidence, or a non-empty r.ID (a
//     collection-level request must not also name an existing resource
//     instance) are all contradictions and resolve Unknown. A
//     project-applicable permission legitimately uses CollectionScope ==
//     Project (e.g. agent.create: the agent is new, the project is not); a
//     hub-only permission legitimately uses CollectionScope == Hub (e.g.
//     project.create). Evidence never overrides a contradictory fact.
//  1. r.Type == permissions.ResourceProject and r.ID != "": TargetScopeProject.
//     An existing project resolves directly and unconditionally.
//  2. r.Type == permissions.ResourceHub (the Hub singleton resource
//     itself): TargetScopeHub.
//  3. r.ParentType == "project" and r.ParentID != "": TargetScopeProject —
//     UNLESS r.ScopeKind also indicates a global/user/core scope, which is
//     a contradiction (a resource cannot be both project-parented and
//     globally scoped) and resolves Unknown.
//  4. r.ParentType == "system": TargetScopeHub — UNLESS r.ParentID is also
//     set, a contradiction (the system sentinel must not carry a project
//     ID), which resolves Unknown.
//  5. r.Type is "broker" or "runtime_broker" (both occur as Resource.Type
//     in the codebase; neither has a project-scoped variant): TargetScopeHub.
//  6. r.ScopeKind indicates a global/user/core scope for skill/template/
//     harness_config: TargetScopeHub.
//  7. Anything else resolves TargetScopeUnknown. This deliberately does NOT
//     classify group/user/policy/gcp_service_account/quota/role/hub-adjacent
//     types as Hub by type alone: a genuinely hub-scoped instance of those
//     types is expected to reach rule 4 via an explicit ParentType ==
//     "system", not this fallback. Critical constraint preserved: never
//     equate missing resource parent metadata with hub scope.
func ResolveTargetScope(r Resource, evidence TargetScopeEvidence) TargetScope {
	if evidence.IsCollectionLevel {
		if r.ID != "" {
			// Contradiction: a collection-level request names an existing
			// resource instance.
			return TargetScope{Kind: TargetScopeUnknown}
		}
		applies, reviewed := permissions.AppliesToExistingProjectTarget(evidence.PermissionID)
		if !reviewed {
			return TargetScope{Kind: TargetScopeUnknown}
		}
		switch evidence.CollectionScope {
		case TargetScopeHub:
			if applies || evidence.CollectionProjectID != "" {
				// Contradiction: Hub evidence for a project-applicable
				// permission, or a stray project ID on Hub evidence.
				return TargetScope{Kind: TargetScopeUnknown}
			}
			return TargetScope{Kind: TargetScopeHub}
		case TargetScopeProject:
			if !applies || evidence.CollectionProjectID == "" {
				// Contradiction: Project evidence for a hub-only
				// permission, or no project named.
				return TargetScope{Kind: TargetScopeUnknown}
			}
			return TargetScope{Kind: TargetScopeProject, ProjectID: evidence.CollectionProjectID}
		default:
			return TargetScope{Kind: TargetScopeUnknown}
		}
	}

	if r.Type == permissions.ResourceProject && r.ID != "" {
		return TargetScope{Kind: TargetScopeProject, ProjectID: r.ID}
	}

	if r.Type == permissions.ResourceHub {
		return TargetScope{Kind: TargetScopeHub}
	}

	hasProjectParent := r.ParentType == "project" && r.ParentID != ""
	hasSystemParent := r.ParentType == "system"
	hasGlobalScope := isUserOrGlobalScopeKind(r.ScopeKind)

	if hasProjectParent && hasGlobalScope {
		return TargetScope{Kind: TargetScopeUnknown}
	}
	if hasSystemParent && r.ParentID != "" {
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

// isUserOrGlobalScopeKind reports whether a ScopeKind value (as used by
// skill/template/harness_config resources) marks a record as scoped to a
// user or globally, rather than to a project. store.SkillScopeUser,
// store.TemplateScopeUser, and store.HarnessConfigScopeUser are all the
// literal string "user" (and likewise "global"/"core"), so a single string
// comparison covers every resource-specific constant.
func isUserOrGlobalScopeKind(scopeKind string) bool {
	switch scopeKind {
	case store.SkillScopeUser, store.SkillScopeGlobal, store.SkillScopeCore:
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
	// Validate class against permissionID: a real project-target proof must
	// name the permission's own resource type (never empty, never
	// mismatched — an empty/wrong ResourceType would route around
	// applyHubWideScopeFilters's curated-catalog dispatch entirely), and must never
	// use a global-catalog/user scope kind (a real project target is never
	// "the global catalog" — that contradiction is MintTimeSystemGrant's
	// job, not this function's).
	if expected := registryResourceType(permissionID); class.ResourceType == "" || class.ResourceType != expected {
		return false, fmt.Errorf("%w: class resource type %q does not match permission %q's resource type %q", ErrProjectAccessDenied, class.ResourceType, permissionID, expected)
	}
	if isUserOrGlobalScopeKind(class.ScopeKind) {
		return false, fmt.Errorf("%w: SystemAuthorityProof cannot use a global-catalog/user-scope class for a real project target", ErrProjectAccessDenied)
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
	var active []*store.RoleBinding
	for _, b := range bindings {
		if b.ScopeType != ScopeTypeProject {
			continue
		}
		if !bindingActivationOK(b, now) {
			continue
		}
		active = append(active, b)
	}
	roleDefs, err := a.loadRoleDefinitions(ctx, collectRoleDefinitionIDs(active))
	if err != nil {
		return false, fmt.Errorf("%w: role definition resolution failed: %v", ErrProjectAccessDenied, err)
	}
	if !candidateSetHasPermission(toCandidateBindings(active), roleDefs, permissionID) {
		return false, nil
	}
	closure := make(map[string]struct{}, len(refs))
	for _, p := range refs {
		closure[p.Type+":"+p.ID] = struct{}{}
	}
	restrictions := a.loadAccessConstraintRestrictions(ctx, closure, ResourceContext{})
	survivors := applyRestrictions([]string{permissionID}, restrictions)
	return len(survivors) == 1, nil
}

// hasAnyProjectMembership reports whether principal has an active
// project-scoped role binding in ANY project at all, permission-agnostic —
// used as the "relevant project admission" evidence for a relationship-
// eligible selector under a Hub boundary (CanMintSelector), without
// enumerating any specific owned target.
func (a *AuthzService) hasAnyProjectMembership(ctx context.Context, principal PrincipalContext) (bool, error) {
	refs, _, _, err := a.principalClosure(ctx, principal)
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrProjectAccessDenied, err)
	}
	bindings, err := a.store.ListRoleBindingsForPrincipals(ctx, refs, []string{store.RoleScopeProject}, nil)
	if err != nil {
		return false, fmt.Errorf("%w: binding resolution failed: %v", ErrProjectAccessDenied, err)
	}
	now := time.Now()
	for _, b := range bindings {
		if b.ScopeType != ScopeTypeProject {
			continue
		}
		if bindingActivationOK(b, now) {
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
//     relevant project admission (hasAnyProjectMembership — permission-
//     agnostic, no specific target enumerated). This is what lets an
//     ordinary project member mint agent:attach under a hub boundary without
//     any system role or blanket project permission grant.
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
			hasAny, err := a.hasAnyProjectMembership(ctx, principal)
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
// absent from the registry defaults to MintEligibilityFlatRole. The extra
// project-role flat-subset check only applies when admission was
// established through membershipOK: a project member's OWN role may still
// lack this specific permission even though they are a member (membership
// is permission-agnostic by design). When admission was instead established
// without membership (system authority for this exact permission),
// selectorAdmitted has ALREADY individually verified every permID in this
// expansion — re-deriving eligibility from getProjectScopedPermissions alone
// would incorrectly deny a permission the admission step just proved via a
// different, equally valid path.
func (a *AuthzService) selectorMintEligible(ctx context.Context, principal PrincipalContext, boundary TokenBoundary, membershipOK bool, permIDs []string) (bool, MintDenialReason, error) {
	for _, permID := range permIDs {
		descriptor, hasDescriptor := permissions.MintEligibilityRegistry[permID]
		if !hasDescriptor {
			if membershipOK {
				ok, err := a.hasProjectRoleFlatPermission(ctx, principal, boundary.ProjectID, permID)
				if err != nil {
					return false, MintDenialNone, err
				}
				if !ok {
					return false, MintDenialFlatRoleInsufficient, nil
				}
			}
			continue
		}
		eligible := false
		for _, src := range descriptor.Sources {
			switch src.Kind {
			case permissions.MintEligibilityFlatRole:
				eligible = true
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
					if permissions.RelationshipPolicyMintEligible(relType, string(principal.Kind), resourceType, permID) {
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
