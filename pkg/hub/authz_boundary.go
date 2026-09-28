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
//  1. evidence.IsCollectionLevel: honored only when permissions.
//     ProjectTargetApplicability[evidence.PermissionID] is reviewed false
//     (a genuinely hub-level collection action). A mismatch — evidence
//     supplied for a project-applicable permission, or an unrecognized
//     CollectionScope — resolves Unknown, never Hub: evidence never
//     overrides a contradictory fact.
//  2. r.Type == permissions.ResourceHub (the Hub singleton resource
//     itself): TargetScopeHub. This is the one resource type with no
//     per-instance concept at all, so it is safe to classify by type
//     alone.
//  3. r.ParentType == "project" and r.ParentID != "": TargetScopeProject.
//     This applies uniformly to every resource type that carries project
//     containment via ParentType/ParentID, including role_binding and
//     access_constraint when a resource resolver populates their
//     project-scoped instances this way — no resource-type family is
//     special-cased out of this rule.
//  4. r.ParentType == "system": TargetScopeHub. This sentinel (matching the
//     existing convention at capabilities.go:222) is an explicit, positive
//     declaration that a resource resolver must set for a confirmed
//     system/hub-scoped instance of a mixed-scope type (role_binding,
//     access_constraint, and similarly any future project-or-system-scoped
//     type) — it is NOT the zero value, so it cannot be produced by
//     accident. Resolvers for those types (owned by whichever tracker
//     wires this — B.2/D.2 touch those handlers) must set ParentType to
//     "project" or "system" explicitly; A.1 defines the contract, not that
//     wiring.
//  5. r.Type is "broker" or "runtime_broker" (both occur as Resource.Type
//     in the codebase; neither has a project-scoped variant): TargetScopeHub.
//  6. r.ScopeKind indicates a global/user/core scope for skill/template/
//     harness_config (their own ScopeKind, distinct from ParentType):
//     TargetScopeHub. These types have no project containment by design
//     when scoped this way. BoundaryAllows only answers "can this
//     credential's boundary reach a resource with no project home";
//     ownership/progeny authorization remains a separate relationship-grant
//     check (B), evaluated after boundary and project-access gates pass.
//  7. Anything else — including a resource type with no ParentType set at
//     all and no ScopeKind evidence — resolves TargetScopeUnknown. This
//     deliberately does NOT classify group/user/policy/gcp_service_account/
//     quota/role/hub-adjacent types as Hub by type alone: a genuinely
//     hub-scoped instance of those types is expected to reach rule 4 via an
//     explicit ParentType == "system" (or rule 2 for the Hub type itself),
//     not this fallback. Critical constraint preserved: never equate
//     missing resource parent metadata with hub scope.
func ResolveTargetScope(r Resource, evidence TargetScopeEvidence) TargetScope {
	if evidence.IsCollectionLevel {
		applies, reviewed := permissions.AppliesToExistingProjectTarget(evidence.PermissionID)
		if !reviewed || applies {
			// Misuse: either the permission hasn't been reviewed, or it IS
			// project-applicable, so collection-level evidence contradicts
			// the permission's own reviewed disposition. Fail unknown.
			return TargetScope{Kind: TargetScopeUnknown}
		}
		switch evidence.CollectionScope {
		case TargetScopeHub:
			return TargetScope{Kind: TargetScopeHub}
		case TargetScopeProject:
			if evidence.CollectionProjectID == "" {
				return TargetScope{Kind: TargetScopeUnknown}
			}
			return TargetScope{Kind: TargetScopeProject, ProjectID: evidence.CollectionProjectID}
		default:
			return TargetScope{Kind: TargetScopeUnknown}
		}
	}

	if r.Type == permissions.ResourceHub {
		return TargetScope{Kind: TargetScopeHub}
	}

	if r.ParentType == "project" && r.ParentID != "" {
		return TargetScope{Kind: TargetScopeProject, ProjectID: r.ParentID}
	}

	if r.ParentType == "system" {
		return TargetScope{Kind: TargetScopeHub}
	}

	if r.Type == "broker" || r.Type == "runtime_broker" {
		return TargetScope{Kind: TargetScopeHub}
	}

	if isUserOrGlobalScopeKind(r.ScopeKind) {
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
func (a *AuthzService) scopedRoleBindingPermissions(ctx context.Context, bindings []*store.RoleBinding, scopeType, scopeID string, closure map[string]struct{}, resourceCtx ResourceContext, now time.Time) []string {
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
			a.logger.Warn("failed to resolve role definition for scoped binding",
				"binding_id", b.ID, "role_definition_id", b.RoleDefinitionID, "scope_type", scopeType, "error", rdErr)
			continue
		}
		if rd == nil {
			a.logger.Warn("role definition not found for scoped binding",
				"binding_id", b.ID, "role_definition_id", b.RoleDefinitionID, "scope_type", scopeType)
			continue
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
	return result
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
// active project-scoped role binding in ANY project (not a specific one) —
// used by CanMintSelector's hub-boundary project-applicable-permission
// branch, where mint time has no single project to check against.
func (a *AuthzService) hasAnyProjectBinding(ctx context.Context, principal PrincipalContext, permissionID string) (bool, error) {
	refs, _, _, err := a.principalClosure(ctx, principal)
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrProjectAccessDenied, err)
	}
	bindings, err := a.store.ListRoleBindingsForPrincipals(ctx, refs, nil, nil)
	if err != nil {
		return false, fmt.Errorf("%w: binding resolution failed: %v", ErrProjectAccessDenied, err)
	}
	now := time.Now()
	for _, b := range bindings {
		if b.ScopeType != ScopeTypeProject {
			continue
		}
		if !bindingActivationOK(b, now) {
			continue
		}
		rd, err := a.store.GetRoleDefinition(ctx, b.RoleDefinitionID)
		if err != nil || rd == nil {
			continue
		}
		for _, permID := range rd.Permissions {
			if permID == permissionID {
				return true, nil
			}
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
	if err := requireLocalUserPrincipal(principal); err != nil {
		return ProjectAdmissionResult{}, err
	}
	if projectID == "" {
		return ProjectAdmissionResult{}, fmt.Errorf("%w: empty project ID", ErrProjectAccessDenied)
	}

	targetScope := ResolveTargetScope(target, TargetScopeEvidence{})
	if targetScope.Kind != TargetScopeProject || targetScope.ProjectID != projectID {
		return ProjectAdmissionResult{}, fmt.Errorf("%w: target resolves to %q, requested %q", ErrProjectMismatch, targetScope.ProjectID, projectID)
	}

	class := ProjectTargetClass{ResourceType: target.Type, ScopeKind: target.ScopeKind}
	key := projectAdmissionCacheKey{principalKind: principal.Kind, principalID: principal.ID, projectID: projectID, permissionID: permissionID, class: class}
	if memo != nil {
		if cached, ok := memo.get(key); ok {
			return cached, nil
		}
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

// CanMintSelector answers, for every requested selector at once, whether
// principal may select it for boundary — never "does a target already
// exist," never a grant by itself.
//
// Admission: for a Project boundary, ProjectMembershipEvidence is loaded
// ONCE for the whole batch. For each selector, resolved to
// SelectorMapping.PermissionIDs: admitted := membershipOK; if not, admitted
// requires EVERY permission ID in the expansion (all alias members) to
// individually pass SystemAuthorityProof(ctx, principal, boundary.ProjectID,
// permID, ContemplatedProjectClass(permID)) — a super-admin with exact
// agent.delete authority and no membership row is admitted for an
// agent.delete-eligible selector, but not for an unrelated one their system
// role doesn't hold. For a Hub boundary, per permission ID in the
// expansion: MintTimeSystemGrant(ctx, principal, permID) OR (held via an
// active project-scoped role binding in ANY project). Boundary kind itself
// is never evidence. Any admission failure sets
// MintDenialProjectAccessRequired — no oracle distinguishing "no
// membership" from "wrong permission."
//
// If admitted, each selector is evaluated against MintEligibilityRegistry:
// MintEligibilityFlatRole selectors use the existing flat
// role-permission-subset check (Project boundary) or the same admission
// evidence (Hub boundary, already established above); MintEligibilityRelationship
// selectors check permissions.RelationshipPolicyAllows for at least one
// declared relationship type.
//
// Empty selectors returns an empty result slice and nil error.
func (a *AuthzService) CanMintSelector(ctx context.Context, principal PrincipalContext, boundary TokenBoundary, selectors []string) ([]SelectorEligibility, error) {
	if len(selectors) == 0 {
		return nil, nil
	}
	if !boundary.Valid() {
		return nil, fmt.Errorf("%w: invalid token boundary", ErrProjectAccessDenied)
	}
	if err := requireLocalUserPrincipal(principal); err != nil {
		return nil, err
	}

	var membershipOK bool
	var membershipErr error
	if boundary.Kind == BoundaryKindProject {
		membershipOK, _, membershipErr = a.ProjectMembershipEvidence(ctx, principal, boundary.ProjectID)
		if membershipErr != nil {
			return nil, membershipErr
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

// selectorAdmitted implements CanMintSelector's admission step (membership
// or per-permission system authority) for every permission ID in a
// selector's expansion.
func (a *AuthzService) selectorAdmitted(ctx context.Context, principal PrincipalContext, boundary TokenBoundary, membershipOK bool, permIDs []string) (bool, error) {
	if boundary.Kind == BoundaryKindProject {
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

	// Hub boundary: per permission ID, MintTimeSystemGrant OR held via any
	// active project-scoped binding.
	for _, permID := range permIDs {
		ok, err := a.MintTimeSystemGrant(ctx, principal, permID)
		if err != nil {
			return false, err
		}
		if ok {
			continue
		}
		ok, err = a.hasAnyProjectBinding(ctx, principal, permID)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

// selectorMintEligible evaluates MintEligibilityRegistry for every
// permission ID in a selector's expansion. A permission absent from the
// registry defaults to MintEligibilityFlatRole (already satisfied by the
// admission step above for a Hub boundary; re-checked as the existing flat
// role subset for a Project boundary).
func (a *AuthzService) selectorMintEligible(ctx context.Context, principal PrincipalContext, boundary TokenBoundary, permIDs []string) (bool, MintDenialReason, error) {
	for _, permID := range permIDs {
		descriptor, hasDescriptor := permissions.MintEligibilityRegistry[permID]
		if !hasDescriptor {
			if boundary.Kind == BoundaryKindProject {
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
					// progeny rows that are never mint-eligible).
					if permissions.RelationshipPolicyMintEligible(relType, resourceType, permID) {
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
