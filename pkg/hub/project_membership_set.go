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
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ---------------------------------------------------------------------------
// SetMemberRoles — atomic "set roles for principal" engine (ptone/scion#2529
// P1, design.md §3.2, §3.3, §3.4, §3.6, and design-d3-addendum.md §4).
//
// This file is the only place that decides whether an actor may create or
// remove a custom project-scoped role binding (design-d3-addendum.md A1).
// It is additive: AddMember, UpdateMemberRole and RemoveMember in
// project_membership_service.go are untouched, and every rs1_*/rs2_*/rs3_*/
// d002_*/pm1_* test keeps passing unmodified.
// ---------------------------------------------------------------------------

// Hub-level role_binding permission IDs. Named here (rather than inline
// strings scattered across call sites) because customRoleAuthorityFromStore's
// signature deliberately asks for "create" or "delete" authority separately
// (design-d3-addendum.md §4 item 2): under a later authority model (option
// (b) in the addendum) these may map to different checks.
const (
	PermRoleBindingCreate = "role_binding.create"
	PermRoleBindingDelete = "role_binding.delete"
)

// SetMemberRolesRequest describes a declarative "set the principal's whole
// project role set" mutation. One exported method, SetMemberRoles, serves
// both PUT (RemoveAll=false) and DELETE (RemoveAll=true, "set to empty").
type SetMemberRolesRequest struct {
	ProjectID     string
	PrincipalType string
	PrincipalID   string
	Actor         UserIdentity

	// DesiredRoleIDs is the caller-supplied set of role definition IDs. It
	// need not be de-duplicated by the caller; SetMemberRoles de-duplicates
	// and validates it. Ignored (forced empty) when RemoveAll is true.
	DesiredRoleIDs []string

	// ExpectedRoleIDs, when non-nil, is a precondition: the principal's
	// current role definition IDs must equal this set (as a set, not an
	// order) or the request fails with 409 membership_changed. A non-nil
	// empty slice means "the principal must not already be a member".
	ExpectedRoleIDs *[]string

	// RemoveAll makes this the DELETE path: every one of the principal's
	// project-scope bindings is removed, governance permitting.
	RemoveAll bool

	// NotBefore/ExpiresAt apply only to bindings newly created by this
	// request; a binding kept across the request (and the built-in side of
	// a built-in role change) preserves its own lifecycle fields.
	NotBefore *time.Time
	ExpiresAt *time.Time
}

// SetMemberRolesResult is the outcome of a successful SetMemberRoles call.
type SetMemberRolesResult struct {
	Before  []*store.RoleBinding
	After   []*store.RoleBinding
	Created bool // the principal had no project-scope bindings before this call
	Changed bool // false only for the idempotent re-PUT-of-the-same-set case
}

// ---------------------------------------------------------------------------
// rolePlan — the pure diff between current and desired role sets.
// ---------------------------------------------------------------------------

// builtInRoleChange identifies the built-in-role half of a plan, when the
// principal's single built-in role (or lack of one) changes. Old is also an
// element of rolePlan.Remove; New is also an element of rolePlan.Create.
type builtInRoleChange struct {
	Old *store.RoleBinding
	New *store.RoleDefinition
}

// rolePlan is the diff between a principal's current project-scope bindings
// and the desired role definitions, produced by planRoleSet. It never
// mutates anything; SetMemberRoles applies it inside a transaction.
type rolePlan struct {
	Keep          []*store.RoleBinding    // role in both sets: untouched (ID, createdAt, lifecycle preserved)
	Remove        []*store.RoleBinding    // role in current only
	Create        []*store.RoleDefinition // role in desired only
	BuiltInChange *builtInRoleChange      // set when a built-in role is being replaced by another
}

// isEmpty reports whether the plan has nothing to do.
func (p rolePlan) isEmpty() bool {
	return len(p.Remove) == 0 && len(p.Create) == 0
}

// hasCustomCreate reports whether the plan creates any custom (non-built-in)
// role binding.
func (p rolePlan) hasCustomCreate() bool {
	for _, d := range p.Create {
		if !store.IsBuiltInProjectMembershipRole(d.Name) {
			return true
		}
	}
	return false
}

// hasCustomRemove reports whether the plan removes any custom role binding.
// defs maps the current bindings' role definition IDs to their definitions.
func (p rolePlan) hasCustomRemove(defs map[string]*store.RoleDefinition) bool {
	for _, b := range p.Remove {
		if rd := defs[b.RoleDefinitionID]; rd != nil && !store.IsBuiltInProjectMembershipRole(rd.Name) {
			return true
		}
	}
	return false
}

// planChange is one governance-relevant change in a rolePlan: either a
// create, a remove, or (for a built-in role swap) an update evaluated
// against both the old and the new role name.
type planChange struct {
	op       MembershipOp
	roleName string
}

// changes enumerates the governance-relevant changes in the plan. defs maps
// the current bindings' role definition IDs to their definitions (needed to
// name removed roles). A built-in swap is reported once as two
// MembershipOpUpdate changes (old name, then new name), matching
// UpdateMemberRole's existing governance evaluation; it is excluded from the
// plain Remove/Create enumeration below it.
func (p rolePlan) changes(defs map[string]*store.RoleDefinition) []planChange {
	var out []planChange
	var builtInOldBindingID, builtInNewRoleID string
	if p.BuiltInChange != nil {
		builtInOldBindingID = p.BuiltInChange.Old.ID
		builtInNewRoleID = p.BuiltInChange.New.ID
		oldName := ""
		if rd := defs[p.BuiltInChange.Old.RoleDefinitionID]; rd != nil {
			oldName = rd.Name
		}
		out = append(out, planChange{op: MembershipOpUpdate, roleName: oldName})
		out = append(out, planChange{op: MembershipOpUpdate, roleName: p.BuiltInChange.New.Name})
	}
	for _, b := range p.Remove {
		if b.ID == builtInOldBindingID {
			continue
		}
		if rd := defs[b.RoleDefinitionID]; rd != nil {
			out = append(out, planChange{op: MembershipOpRemove, roleName: rd.Name})
		}
	}
	for _, d := range p.Create {
		if d.ID == builtInNewRoleID {
			continue
		}
		out = append(out, planChange{op: MembershipOpAdd, roleName: d.Name})
	}
	return out
}

// planRoleSet computes the diff between a principal's current project-scope
// bindings and the desired role definitions (design.md §3.2). current and
// desired are both already de-duplicated by role definition ID by the
// caller. currentDefs maps every binding in current to its role definition.
func planRoleSet(current []*store.RoleBinding, currentDefs map[string]*store.RoleDefinition, desired []*store.RoleDefinition) rolePlan {
	var plan rolePlan

	desiredByID := make(map[string]*store.RoleDefinition, len(desired))
	for _, d := range desired {
		desiredByID[d.ID] = d
	}

	matched := make(map[string]bool, len(desired))
	for _, b := range current {
		if d, ok := desiredByID[b.RoleDefinitionID]; ok {
			plan.Keep = append(plan.Keep, b)
			matched[d.ID] = true
		} else {
			plan.Remove = append(plan.Remove, b)
		}
	}
	for _, d := range desired {
		if !matched[d.ID] {
			plan.Create = append(plan.Create, d)
		}
	}

	// Identify the built-in swap, if any, for lifecycle inheritance
	// (design.md §3.2 step 5: "The new built-in in a BuiltInChange inherits
	// NotBefore/ExpiresAt from Old").
	var oldBuiltIn *store.RoleBinding
	for _, b := range plan.Remove {
		if rd := currentDefs[b.RoleDefinitionID]; rd != nil && store.IsBuiltInProjectMembershipRole(rd.Name) {
			oldBuiltIn = b
			break
		}
	}
	var newBuiltIn *store.RoleDefinition
	for _, d := range plan.Create {
		if store.IsBuiltInProjectMembershipRole(d.Name) {
			newBuiltIn = d
			break
		}
	}
	if oldBuiltIn != nil && newBuiltIn != nil {
		plan.BuiltInChange = &builtInRoleChange{Old: oldBuiltIn, New: newBuiltIn}
	}

	return plan
}

// ---------------------------------------------------------------------------
// Custom-role authority — the ONE function that decides whether an actor may
// create or remove a custom project-scoped role binding
// (design-d3-addendum.md §4 item 1, acceptance A1).
// ---------------------------------------------------------------------------

// customRoleAuthority is the result of customRoleAuthorityFromStore.
type customRoleAuthority struct {
	Allowed bool
	// Via records how authority was granted: "project_owner" or
	// "hub_role_binding". Recorded in the custom-row audit summary
	// (design-d3-addendum.md §4 item 5). Empty when Allowed is false.
	Via    string
	Reason string
}

const (
	customRoleAuthorityViaOwner = "project_owner"
	customRoleAuthorityViaHub   = "hub_role_binding"
)

// customRoleAuthorityFromStore is the single evaluator for "may this actor
// create or remove a custom project-scoped role binding in this project".
// It is called with svc.store before the transaction and with tx inside it,
// so there is exactly one implementation for both checks
// (design-d3-addendum.md §2 item 1, §4 item 1).
//
// Today (design-d3-addendum.md §3 option (c)): a direct project owner always
// has authority; otherwise an actor with NO project role of their own (the
// same "actorRole == \"\"" condition that gates the built-in governance
// override in checkGovernance / reevaluateActorTx) falls back to the
// existing system-scope hub override (actorHasHubRoleBindingAuthorityTx,
// which is itself store-generic and safe to call pre-transaction). This
// deliberately reuses the same two conditions "owner OR (no project role AND
// hub role_binding.*)" that already govern custom-role bind/unbind today
// (findings.md §1), just decided in one function instead of inline at each
// call site. review r1 F2: an actor who already holds a project role (e.g.
// project-admin) does NOT get the hub fallback just because they separately
// hold hub role_binding.* — that would let the one actor-authority function
// disagree with built-in governance about what "hub override" means for the
// same actor (design-d3-addendum.md Part 0 (iv)).
//
// perm is PermRoleBindingCreate or PermRoleBindingDelete, asked separately
// even though both resolve to the same check today: a later authority model
// (design-d3-addendum.md §3 option (b), seeding role_binding.* to
// project-owner) would map them to different permissions without changing
// this function's signature or any call site.
//
// No other function in this package may decide custom-role grant/revoke
// authority (design-d3-addendum.md acceptance A1).
func (svc *ProjectMembershipService) customRoleAuthorityFromStore(ctx context.Context, s store.Store, actorID, projectID, perm string) (customRoleAuthority, error) {
	isDirectOwner, err := svc.isActorDirectOwnerFromStore(ctx, s, actorID, projectID)
	if err != nil {
		return customRoleAuthority{}, fmt.Errorf("direct-owner lookup for custom role authority: %w", err)
	}
	if isDirectOwner {
		return customRoleAuthority{Allowed: true, Via: customRoleAuthorityViaOwner}, nil
	}

	deniedDecision := customRoleAuthority{
		Allowed: false,
		Reason:  "custom role changes require role-binding authority in this project (project owners)",
	}

	// F2: the hub role_binding.* fallback applies only to an actor with NO
	// project role of their own. A project-admin (or member) who separately
	// holds hub role_binding.* is refused here exactly like one who does
	// not — they must use the hub-admin role-bindings API, not this one.
	actorRole, err := svc.projectEffectiveRoleFromStore(ctx, s, actorID, projectID)
	if err != nil {
		return customRoleAuthority{}, fmt.Errorf("project role lookup for custom role authority: %w", err)
	}
	if actorRole != "" {
		return deniedDecision, nil
	}

	op := MembershipOpAdd
	if perm == PermRoleBindingDelete {
		op = MembershipOpRemove
	}
	hasHubAuthority, err := svc.actorHasHubRoleBindingAuthorityTx(ctx, s, actorID, op)
	if err != nil {
		return customRoleAuthority{}, fmt.Errorf("hub role-binding authority lookup for custom role authority: %w", err)
	}
	if hasHubAuthority {
		return customRoleAuthority{Allowed: true, Via: customRoleAuthorityViaHub}, nil
	}

	return deniedDecision, nil
}

// ---------------------------------------------------------------------------
// Escalation guard (ii) — structural refusal of role_binding.*-bearing
// custom roles (design.md Part 0 escalation guard (ii); review r1 F1).
//
// customRoleAuthorityFromStore above decides WHO may create or remove a
// custom role binding; this guard decides WHICH custom roles may be created
// at all, regardless of who is asking. It runs before, and independently of,
// customRoleAuthorityFromStore and CanDelegate, so it also refuses the hub
// role_binding.* override actor — the one actor CanDelegate cannot refuse,
// because that actor's own ceiling already includes role_binding.create/
// delete (findings.md §2, addendum §2 item 4). Granting a custom role that
// itself carries role_binding.* would mint a project-scoped delegation grant
// that the addendum calls out as inert today but live the moment project
// scope is honored (design-d3-addendum.md §2 item 4) — this endpoint must
// not be the one that creates those latent grants.
// ---------------------------------------------------------------------------

// roleBindingPermissionPrefix is the permission namespace this guard refuses
// in any newly CREATED custom role.
const roleBindingPermissionPrefix = "role_binding."

// roleContainsRoleBindingPermission reports whether rd carries any
// role_binding.* permission.
func roleContainsRoleBindingPermission(rd *store.RoleDefinition) bool {
	for _, p := range rd.Permissions {
		if strings.HasPrefix(p, roleBindingPermissionPrefix) {
			return true
		}
	}
	return false
}

// checkNoRoleBindingPermissionInCreatedCustomRoles refuses any CREATED
// (never merely kept) custom role whose permissions include a role_binding.*
// permission, for EVERY actor. Built-in roles are exempt: they are matrix-
// governed separately and never carry custom permission lists.
func checkNoRoleBindingPermissionInCreatedCustomRoles(creates []*store.RoleDefinition) *MembershipDecision {
	for _, d := range creates {
		if store.IsBuiltInProjectMembershipRole(d.Name) {
			continue
		}
		if roleContainsRoleBindingPermission(d) {
			return &MembershipDecision{
				Allowed:    false,
				DenialCode: ErrCodeRoleAssignmentForbidden,
				Reason:     "a custom role containing a role_binding.* permission cannot be granted through this endpoint",
				HTTPStatus: 403,
				Details:    map[string]interface{}{"roleDefinitionId": d.ID, "roleName": d.Name},
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Built-in governance — unchanged matrix, kept strictly separate from custom
// authority (design-d3-addendum.md §2 item 2, §4 item 2 / acceptance A2).
// ---------------------------------------------------------------------------

// checkBuiltInChangeGovernance applies the existing governance matrix
// (isOperationPermitted) and the direct-owner requirement to one built-in
// role change. hubOverride here is the SYSTEM-SCOPE-ONLY hub role_binding
// override (reevaluateActorTx / actorHasHubRoleBindingAuthority[Tx]) — never
// the custom-role authority above. This is what keeps a custom-only holder
// (even one whose custom role carries role_binding.create) from bypassing
// the built-in matrix: their effective built-in actorRole is "", and the
// override here only looks at SYSTEM-scope bindings.
func (svc *ProjectMembershipService) checkBuiltInChangeGovernance(actorRole string, isDirectOwner, hubOverride bool, op MembershipOp, roleName string) *MembershipDecision {
	if hubOverride {
		return nil
	}
	if !svc.isOperationPermitted(actorRole, op, roleName) {
		code := ErrCodeRoleAssignmentForbidden
		if isProtectedRole(roleName) {
			code = ErrCodeTargetRoleProtected
		}
		return &MembershipDecision{
			Allowed:    false,
			DenialCode: code,
			Reason:     fmt.Sprintf("actor role %q cannot %s target role %q", actorRole, op, roleName),
			HTTPStatus: 403,
		}
	}
	if requiresDirectOwner(roleName) && !isDirectOwner {
		return &MembershipDecision{
			Allowed:    false,
			DenialCode: ErrCodeRoleAssignmentForbidden,
			Reason:     "only direct project owners can manage admin and owner roles",
			HTTPStatus: 403,
		}
	}
	return nil
}

// governanceDecisionForChange dispatches one plan change to the built-in
// matrix or to the (precomputed) custom-role authority result, per
// design.md §3.3. customAuth holds the already-evaluated authority for
// whichever of PermRoleBindingCreate/PermRoleBindingDelete the plan needs;
// it is computed once per phase (pre-transaction, then again under lock) by
// the caller via customRoleAuthorityFromStore — never recomputed here.
func (svc *ProjectMembershipService) governanceDecisionForChange(actorRole string, isDirectOwner, hubOverride bool, customAuth map[string]customRoleAuthority, ch planChange) *MembershipDecision {
	if store.IsBuiltInProjectMembershipRole(ch.roleName) {
		return svc.checkBuiltInChangeGovernance(actorRole, isDirectOwner, hubOverride, ch.op, ch.roleName)
	}
	perm := PermRoleBindingCreate
	if ch.op == MembershipOpRemove {
		perm = PermRoleBindingDelete
	}
	auth := customAuth[perm]
	if !auth.Allowed {
		return &MembershipDecision{
			Allowed:    false,
			DenialCode: ErrCodeRoleAssignmentForbidden,
			Reason:     "custom role changes require role-binding authority in this project (project owners)",
			HTTPStatus: 403,
			Details:    map[string]interface{}{"requiredPermission": perm},
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// reevaluateActorTx — the built-in-governance authority re-evaluation under
// lock (design.md §3.2 Phase T step 2). Custom-role authority is NOT
// re-evaluated here; SetMemberRoles calls customRoleAuthorityFromStore(tx)
// separately so the two decisions stay independent (addendum §2 item 2).
// ---------------------------------------------------------------------------

func (svc *ProjectMembershipService) reevaluateActorTx(ctx context.Context, tx store.Store, actorID, projectID string, needCreate, needDelete bool) (actorRole string, isDirectOwner, hubOverride bool, err error) {
	actorRole, err = svc.projectEffectiveRoleFromStore(ctx, tx, actorID, projectID)
	if err != nil {
		return "", false, false, fmt.Errorf("authority lookup failed under lock: %w", err)
	}
	if actorRole != "" {
		isDirectOwner, err = svc.isActorDirectOwnerFromStore(ctx, tx, actorID, projectID)
		if err != nil {
			return "", false, false, fmt.Errorf("owner lookup failed under lock: %w", err)
		}
		return actorRole, isDirectOwner, false, nil
	}

	// No built-in project role: both role_binding.create and
	// role_binding.delete authority are required when the plan has both
	// creates and removes (design.md §3.2 Phase T step 2).
	if needCreate {
		ok, hErr := svc.actorHasHubRoleBindingAuthorityTx(ctx, tx, actorID, MembershipOpAdd)
		if hErr != nil {
			return "", false, false, fmt.Errorf("hub authority revalidation failed (fail-closed): %w", hErr)
		}
		if !ok {
			return "", false, false, asGovernanceDenial(MembershipDecision{
				Allowed: false, DenialCode: ErrCodeRoleAssignmentForbidden,
				Reason: "actor has no project role (re-evaluated under lock)", HTTPStatus: 403,
			})
		}
	}
	if needDelete {
		ok, hErr := svc.actorHasHubRoleBindingAuthorityTx(ctx, tx, actorID, MembershipOpRemove)
		if hErr != nil {
			return "", false, false, fmt.Errorf("hub authority revalidation failed (fail-closed): %w", hErr)
		}
		if !ok {
			return "", false, false, asGovernanceDenial(MembershipDecision{
				Allowed: false, DenialCode: ErrCodeRoleAssignmentForbidden,
				Reason: "actor has no project role (re-evaluated under lock)", HTTPStatus: 403,
			})
		}
	}
	return "", false, true, nil
}

// ---------------------------------------------------------------------------
// Typed in-transaction errors
// ---------------------------------------------------------------------------

// governanceDenialError carries a fully-formed MembershipDecision out of a
// WithTx closure, so in-transaction denials keep their stable code, reason
// and Details (role_assignment_forbidden + requiredPermission, etc.).
type governanceDenialError struct {
	decision MembershipDecision
}

func (e *governanceDenialError) Error() string { return e.decision.Reason }

func asGovernanceDenial(d MembershipDecision) error {
	return &governanceDenialError{decision: d}
}

// membershipChangedError signals that the principal's role set changed
// between the pre-transaction read and the locked re-read (design.md §3.2
// Phase T step 3, §3.6 "stale dialogs").
type membershipChangedError struct {
	currentRoleDefinitionIDs []string
}

func (e *membershipChangedError) Error() string {
	return "membership changed since the request was built"
}

func membershipChangedDecision(currentRoleDefinitionIDs []string) *MembershipDecision {
	return &MembershipDecision{
		Allowed:    false,
		DenialCode: ErrCodeMembershipChanged,
		Reason:     "the principal's project roles changed since this request was built",
		HTTPStatus: 409,
		Details:    map[string]interface{}{"currentRoleDefinitionIds": currentRoleDefinitionIDs},
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// loadProjectPrincipalBindings loads a principal's project-scope bindings
// (built-in and custom) from s, which may be svc.store or a transactional
// store, plus their resolved role definitions.
func (svc *ProjectMembershipService) loadProjectPrincipalBindings(ctx context.Context, s store.Store, principalType, principalID, projectID string) ([]*store.RoleBinding, map[string]*store.RoleDefinition, error) {
	bindings, err := s.ListRoleBindingsForPrincipal(ctx, principalType, principalID)
	if err != nil {
		return nil, nil, fmt.Errorf("list bindings for principal %s/%s: %w", principalType, principalID, err)
	}
	var filtered []*store.RoleBinding
	defs := make(map[string]*store.RoleDefinition)
	for _, b := range bindings {
		if b == nil || b.ScopeType != store.RoleScopeProject || b.ScopeID != projectID {
			continue
		}
		if _, ok := defs[b.RoleDefinitionID]; !ok {
			rd, rdErr := s.GetRoleDefinition(ctx, b.RoleDefinitionID)
			if rdErr != nil {
				return nil, nil, fmt.Errorf("resolve role definition %s for binding %s: %w", b.RoleDefinitionID, b.ID, rdErr)
			}
			defs[b.RoleDefinitionID] = rd
		}
		filtered = append(filtered, b)
	}
	return filtered, defs, nil
}

// roleDefIDs returns the (possibly duplicate-free, since bindings never
// carry the same role definition twice per principal/project) role
// definition IDs of the given bindings.
func roleDefIDs(bindings []*store.RoleBinding) []string {
	ids := make([]string, 0, len(bindings))
	for _, b := range bindings {
		ids = append(ids, b.RoleDefinitionID)
	}
	return ids
}

// sameRoleDefSet reports whether bindings' role definition IDs are exactly
// the set of ids (order-independent, duplicate-insensitive).
func sameRoleDefSet(bindings []*store.RoleBinding, ids []string) bool {
	have := make(map[string]bool, len(bindings))
	for _, b := range bindings {
		have[b.RoleDefinitionID] = true
	}
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	if len(have) != len(want) {
		return false
	}
	for id := range want {
		if !have[id] {
			return false
		}
	}
	return true
}

// resolveDesiredRoleDefs de-duplicates ids and resolves each to a project-
// scoped role definition, applying the structural validation table from
// design.md §3.1: an unknown ID or a non-project-scoped role is
// invalid_role_set, and more than one built-in membership role is
// invalid_role_set.
func (svc *ProjectMembershipService) resolveDesiredRoleDefs(ctx context.Context, ids []string) ([]*store.RoleDefinition, *MembershipDecision) {
	seen := make(map[string]bool, len(ids))
	var defs []*store.RoleDefinition
	builtInCount := 0
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		rd, err := svc.store.GetRoleDefinition(ctx, id)
		if err != nil || rd == nil {
			return nil, &MembershipDecision{
				Allowed: false, DenialCode: ErrCodeInvalidRoleSet,
				Reason: "unknown role definition: " + id, HTTPStatus: 400,
				Details: map[string]interface{}{"roleDefinitionId": id},
			}
		}
		if rd.ScopeType != store.RoleScopeProject {
			return nil, &MembershipDecision{
				Allowed: false, DenialCode: ErrCodeInvalidRoleSet,
				Reason: "role is not project-scoped: " + rd.Name, HTTPStatus: 400,
				Details: map[string]interface{}{"roleDefinitionId": id, "roleName": rd.Name},
			}
		}
		if store.IsBuiltInProjectMembershipRole(rd.Name) {
			builtInCount++
			if builtInCount > 1 {
				return nil, &MembershipDecision{
					Allowed: false, DenialCode: ErrCodeInvalidRoleSet,
					Reason: "at most one built-in membership role may be set per principal", HTTPStatus: 400,
				}
			}
		}
		defs = append(defs, rd)
	}
	return defs, nil
}

// ---------------------------------------------------------------------------
// SetMemberRoles
// ---------------------------------------------------------------------------

// SetMemberRoles atomically replaces (PUT) or clears (DELETE, RemoveAll) a
// principal's whole project role set. See design.md §3.2 for the algorithm;
// the phase/step comments below mirror that section.
func (svc *ProjectMembershipService) SetMemberRoles(ctx context.Context, req SetMemberRolesRequest) (*SetMemberRolesResult, *MembershipDecision) {
	if denial := svc.checkMembershipCredential(ctx, req.Actor.ID()); denial != nil {
		return nil, denial
	}

	if req.RemoveAll {
		req.DesiredRoleIDs = nil
	} else if len(req.DesiredRoleIDs) == 0 {
		return nil, &MembershipDecision{
			Allowed: false, DenialCode: ErrCodeEmptyRoleSet,
			Reason:     "use DELETE …/principals/{type}/{id} to remove the member",
			HTTPStatus: 400,
		}
	}

	desiredDefs, denial := svc.resolveDesiredRoleDefs(ctx, req.DesiredRoleIDs)
	if denial != nil {
		return nil, denial
	}
	if !req.RemoveAll && len(desiredDefs) == 0 {
		return nil, &MembershipDecision{
			Allowed: false, DenialCode: ErrCodeEmptyRoleSet,
			Reason:     "use DELETE …/principals/{type}/{id} to remove the member",
			HTTPStatus: 400,
		}
	}

	// --- Phase P: pre-transaction reads and checks ------------------------

	current0, currentDefs0, err := svc.loadProjectPrincipalBindings(ctx, svc.store, req.PrincipalType, req.PrincipalID, req.ProjectID)
	if err != nil {
		return nil, &MembershipDecision{Allowed: false, DenialCode: "internal_error", Reason: err.Error(), HTTPStatus: 500}
	}

	if req.RemoveAll && len(current0) == 0 {
		return nil, &MembershipDecision{Allowed: false, DenialCode: "not_found", Reason: "principal has no bindings in this project", HTTPStatus: 404}
	}

	if req.ExpectedRoleIDs != nil && !sameRoleDefSet(current0, *req.ExpectedRoleIDs) {
		return nil, membershipChangedDecision(roleDefIDs(current0))
	}

	plan0 := planRoleSet(current0, currentDefs0, desiredDefs)
	if plan0.isEmpty() {
		// Idempotent: nothing to do. The precondition above has already
		// been evaluated (design.md §3.2 Phase P step 4).
		return &SetMemberRolesResult{Before: current0, After: current0, Created: false, Changed: false}, nil
	}

	// Escalation guard (ii), structural and actor-independent (F1): refuse
	// any created custom role carrying a role_binding.* permission before
	// anything else — including before actor authority is even determined,
	// so the hub role_binding.* override actor is refused exactly like
	// everyone else.
	if d := checkNoRoleBindingPermissionInCreatedCustomRoles(plan0.Create); d != nil {
		return nil, d
	}

	// Principal eligibility applies only to NEW bindings (plan0.Create):
	// keeping a custom role an ineligible principal already holds is
	// allowed (design-d3-addendum.md D1 / §4 item 6).
	for _, d := range plan0.Create {
		if !principalEligibleForRole(req.PrincipalType, d.Name) {
			return nil, &MembershipDecision{
				Allowed: false, DenialCode: ErrCodePrincipalIneligible,
				Reason:     fmt.Sprintf("role %q cannot be assigned to %s principals", d.Name, req.PrincipalType),
				HTTPStatus: 400,
				Details:    map[string]interface{}{"roleDefinitionId": d.ID, "roleName": d.Name},
			}
		}
	}

	actorRolePre := svc.projectEffectiveRole(ctx, req.Actor.ID(), req.ProjectID)
	hubOverridePre := false
	if actorRolePre == "" {
		needCreate := len(plan0.Create) > 0
		needDelete := len(plan0.Remove) > 0
		authorized := true
		if needCreate && !svc.actorHasHubRoleBindingAuthority(ctx, req.Actor.ID(), MembershipOpAdd) {
			authorized = false
		}
		if needDelete && !svc.actorHasHubRoleBindingAuthority(ctx, req.Actor.ID(), MembershipOpRemove) {
			authorized = false
		}
		if !authorized {
			return nil, &MembershipDecision{Allowed: false, DenialCode: ErrCodeRoleAssignmentForbidden, Reason: "actor has no project role", HTTPStatus: 403}
		}
		hubOverridePre = true
	}
	isDirectOwnerPre := false
	if !hubOverridePre {
		isDirectOwnerPre = svc.isActorDirectOwner(ctx, req.Actor.ID(), req.ProjectID)
	}

	customAuthPre := make(map[string]customRoleAuthority, 2)
	if plan0.hasCustomCreate() {
		auth, aErr := svc.customRoleAuthorityFromStore(ctx, svc.store, req.Actor.ID(), req.ProjectID, PermRoleBindingCreate)
		if aErr != nil {
			return nil, &MembershipDecision{Allowed: false, DenialCode: "internal_error", Reason: aErr.Error(), HTTPStatus: 500}
		}
		customAuthPre[PermRoleBindingCreate] = auth
	}
	if plan0.hasCustomRemove(currentDefs0) {
		auth, aErr := svc.customRoleAuthorityFromStore(ctx, svc.store, req.Actor.ID(), req.ProjectID, PermRoleBindingDelete)
		if aErr != nil {
			return nil, &MembershipDecision{Allowed: false, DenialCode: "internal_error", Reason: aErr.Error(), HTTPStatus: 500}
		}
		customAuthPre[PermRoleBindingDelete] = auth
	}

	for _, ch := range plan0.changes(currentDefs0) {
		if d := svc.governanceDecisionForChange(actorRolePre, isDirectOwnerPre, hubOverridePre, customAuthPre, ch); d != nil {
			return nil, d
		}
	}

	// Pre-transaction CanDelegate: once per created binding, not once per
	// request (design.md §3.2 Phase P step 6; escalation test (iii)).
	var oldBuiltInName string
	if plan0.BuiltInChange != nil {
		if rd := currentDefs0[plan0.BuiltInChange.Old.RoleDefinitionID]; rd != nil {
			oldBuiltInName = rd.Name
		}
	}
	canDelegateReasons := make(map[string]string, len(plan0.Create))
	if svc.authz != nil {
		for _, d := range plan0.Create {
			needsCanDelegate := true
			if plan0.BuiltInChange != nil && d.ID == plan0.BuiltInChange.New.ID {
				needsCanDelegate = projectRoleLevel(d.Name) > projectRoleLevel(oldBuiltInName)
			}
			if !needsCanDelegate {
				continue
			}
			delDecision := svc.authz.CanDelegate(ctx, req.Actor, GrantDescriptor{
				Type:             GrantTypeRoleBinding,
				RoleDefinitionID: d.ID,
				ScopeType:        store.RoleScopeProject,
				ScopeID:          req.ProjectID,
			})
			if !delDecision.Allowed {
				return nil, &MembershipDecision{
					Allowed: false, DenialCode: ErrCodeTargetRoleProtected,
					Reason:     "actor cannot delegate the requested role: " + delDecision.Reason,
					HTTPStatus: 403,
					Details:    map[string]interface{}{"roleDefinitionId": d.ID, "roleName": d.Name, "reason": delDecision.Reason},
				}
			}
			canDelegateReasons[d.ID] = delDecision.Reason
		}
	}

	// --- Phase T: inside the transaction -----------------------------------

	result := SetMemberRolesResult{Created: len(current0) == 0}
	txErr := svc.store.WithTx(ctx, func(tx store.Store) error {
		if err := tx.LockProjectForMembership(ctx, req.ProjectID); err != nil {
			return fmt.Errorf("lock project: %w", err)
		}

		actorRole, isDirectOwner, hubOverride, err := svc.reevaluateActorTx(ctx, tx, req.Actor.ID(), req.ProjectID, len(plan0.Create) > 0, len(plan0.Remove) > 0)
		if err != nil {
			return err
		}

		current1, currentDefs1, err := svc.loadProjectPrincipalBindings(ctx, tx, req.PrincipalType, req.PrincipalID, req.ProjectID)
		if err != nil {
			return fmt.Errorf("re-load bindings under lock: %w", err)
		}
		if req.ExpectedRoleIDs != nil && !sameRoleDefSet(current1, *req.ExpectedRoleIDs) {
			return &membershipChangedError{currentRoleDefinitionIDs: roleDefIDs(current1)}
		}
		if !sameRoleDefSet(current1, roleDefIDs(current0)) {
			return &membershipChangedError{currentRoleDefinitionIDs: roleDefIDs(current1)}
		}

		plan1 := planRoleSet(current1, currentDefs1, desiredDefs)

		// F1: re-check the structural role_binding.* guard under lock, from
		// the same desiredDefs resolved pre-transaction (design.md Part 0
		// residual-risk note: a role definition's permissions are not
		// re-fetched here, matching the accepted residual risk that a hub
		// admin could edit a bound role definition's permissions between
		// phases — out of scope for P1).
		if d := checkNoRoleBindingPermissionInCreatedCustomRoles(plan1.Create); d != nil {
			return asGovernanceDenial(*d)
		}

		customAuthTx := make(map[string]customRoleAuthority, 2)
		if plan1.hasCustomCreate() {
			auth, aErr := svc.customRoleAuthorityFromStore(ctx, tx, req.Actor.ID(), req.ProjectID, PermRoleBindingCreate)
			if aErr != nil {
				return fmt.Errorf("custom role authority (create) under lock: %w", aErr)
			}
			customAuthTx[PermRoleBindingCreate] = auth
		}
		if plan1.hasCustomRemove(currentDefs1) {
			auth, aErr := svc.customRoleAuthorityFromStore(ctx, tx, req.Actor.ID(), req.ProjectID, PermRoleBindingDelete)
			if aErr != nil {
				return fmt.Errorf("custom role authority (delete) under lock: %w", aErr)
			}
			customAuthTx[PermRoleBindingDelete] = auth
		}

		// R2-2 (review r2): Phase P's CanDelegate call ran once, before the
		// lock, against the actor's authority SOURCE at that moment
		// (actorRolePre/hubOverridePre/customAuthPre). It is not, and cannot
		// be, re-run in-tx (accepted FYI-2 residual). But if that source
		// itself changed between phases — e.g. a direct owner who also holds
		// hub role_binding.* is demoted from owner by a concurrent request
		// before this lock lands — the committed grant is no longer the one
		// CanDelegate evaluated: reevaluateActorTx above would now report
		// hubOverride instead of direct ownership, and a hub-admin-only
		// ceiling may refuse what the owner ceiling allowed. Unlike the
		// general FYI-2 residual, this is cheap to detect without re-running
		// CanDelegate: refuse to commit and let the client retry with a
		// fresh request if the actor's role, hub-override status, or any
		// asked custom-authority source moved.
		if actorRole != actorRolePre || hubOverride != hubOverridePre {
			return &membershipChangedError{currentRoleDefinitionIDs: roleDefIDs(current1)}
		}
		for _, perm := range []string{PermRoleBindingCreate, PermRoleBindingDelete} {
			pre, preAsked := customAuthPre[perm]
			post, postAsked := customAuthTx[perm]
			if preAsked != postAsked {
				continue // plan1 == plan0 by construction (current1 == current0 above)
			}
			if preAsked && pre.Via != post.Via {
				return &membershipChangedError{currentRoleDefinitionIDs: roleDefIDs(current1)}
			}
		}

		for _, ch := range plan1.changes(currentDefs1) {
			if d := svc.governanceDecisionForChange(actorRole, isDirectOwner, hubOverride, customAuthTx, ch); d != nil {
				return asGovernanceDenial(*d)
			}
		}

		// Apply the plan: every direct role-binding mutation for this request
		// goes through applyRolePlanTx (project_membership_service.go), the
		// one purpose-named step the authzop mutation catalog classifies for
		// this engine (review r1 F3).
		created, err := svc.applyRolePlanTx(ctx, tx, plan1, req.PrincipalType, req.PrincipalID, req.ProjectID, req.Actor.ID(), req.NotBefore, req.ExpiresAt)
		if err != nil {
			return err
		}

		var builtInNewBindingID string
		if plan1.BuiltInChange != nil {
			if cb := created[plan1.BuiltInChange.New.ID]; cb != nil {
				builtInNewBindingID = cb.ID
			}
		}

		// Last-owner guard, evaluated on the full post-state (design.md
		// §3.2 Phase T step 6).
		removedOwner := false
		for _, b := range plan1.Remove {
			if rd := currentDefs1[b.RoleDefinitionID]; rd != nil && rd.Name == store.ProjectRoleOwner && b.PrincipalType == store.RoleBindingPrincipalUser {
				removedOwner = true
			}
		}
		if removedOwner {
			count, cErr := svc.countActiveDirectOwnersFromStore(ctx, tx, req.ProjectID)
			if cErr != nil {
				return fmt.Errorf("post-state owner count: %w", cErr)
			}
			if count < 1 {
				return &lastOwnerError{projectID: req.ProjectID}
			}
		}

		// Audit: one row per binding change, sharing one CorrelationID
		// (design.md §3.4). createAuditRecord/auditActorFromContext fill
		// the correlation ID from the request context.
		var builtInOldBindingID string
		if plan1.BuiltInChange != nil {
			builtInOldBindingID = plan1.BuiltInChange.Old.ID
			oldName := ""
			if rd := currentDefs1[plan1.BuiltInChange.Old.RoleDefinitionID]; rd != nil {
				oldName = rd.Name
			}
			// N5 (review r1): carry roleKind and principalType here too — the
			// §13.6 contract is that every audit row carries roleKind for
			// uniform filtering, and a built-in swap is always
			// roleKind:"builtin" on both sides by construction
			// (planRoleSet only populates BuiltInChange from built-in role
			// names).
			if aErr := svc.createAuditRecord(ctx, tx, &store.MutationAuditRecord{
				MutationType: "project_member_role_change",
				TargetType:   "project_membership",
				TargetID:     req.ProjectID,
				BeforeSummary: marshalAuditJSON(map[string]string{
					"principalType": req.PrincipalType, "principalId": req.PrincipalID,
					"role": oldName, "roleKind": projectRoleKind(oldName),
				}),
				AfterSummary: marshalAuditJSON(map[string]string{
					"principalType": req.PrincipalType, "principalId": req.PrincipalID,
					"role": plan1.BuiltInChange.New.Name, "roleKind": projectRoleKind(plan1.BuiltInChange.New.Name),
				}),
			}); aErr != nil {
				return aErr
			}
		}
		for _, b := range plan1.Remove {
			if b.ID == builtInOldBindingID {
				continue
			}
			roleName, roleKind := "", roleKindCustom
			authVia := ""
			if rd := currentDefs1[b.RoleDefinitionID]; rd != nil {
				roleName = rd.Name
				roleKind = projectRoleKind(rd.Name)
				if roleKind != roleKindBuiltIn {
					authVia = customAuthTx[PermRoleBindingDelete].Via
				}
			}
			summary := map[string]string{
				"principalType": req.PrincipalType, "principalId": req.PrincipalID,
				"role": roleName, "roleKind": roleKind,
			}
			if authVia != "" {
				summary["authority"] = authVia
			}
			if aErr := svc.createAuditRecord(ctx, tx, &store.MutationAuditRecord{
				MutationType:  "project_member_remove",
				TargetType:    "project_membership",
				TargetID:      req.ProjectID,
				BeforeSummary: marshalAuditJSON(summary),
			}); aErr != nil {
				return aErr
			}
		}
		for _, d := range plan1.Create {
			cb := created[d.ID]
			if cb != nil && cb.ID == builtInNewBindingID {
				continue
			}
			roleKind := projectRoleKind(d.Name)
			summary := map[string]string{
				"principalType": req.PrincipalType, "principalId": req.PrincipalID,
				"role": d.Name, "roleKind": roleKind,
			}
			record := &store.MutationAuditRecord{
				MutationType: "project_member_add",
				TargetType:   "project_membership",
				TargetID:     req.ProjectID,
			}
			if roleKind == roleKindCustom {
				summary["authority"] = customAuthTx[PermRoleBindingCreate].Via
				record.CanDelegateResult = "allowed"
				record.CanDelegateReason = canDelegateReasons[d.ID]
			}
			record.AfterSummary = marshalAuditJSON(summary)
			if aErr := svc.createAuditRecord(ctx, tx, record); aErr != nil {
				return aErr
			}
		}

		after, _, err := svc.loadProjectPrincipalBindings(ctx, tx, req.PrincipalType, req.PrincipalID, req.ProjectID)
		if err != nil {
			return fmt.Errorf("load after-state under lock: %w", err)
		}
		result.Before = current1
		result.After = after
		result.Changed = true
		return nil
	})
	if txErr != nil {
		var gdErr *governanceDenialError
		if errors.As(txErr, &gdErr) {
			d := gdErr.decision
			return nil, &d
		}
		var mcErr *membershipChangedError
		if errors.As(txErr, &mcErr) {
			return nil, membershipChangedDecision(mcErr.currentRoleDefinitionIDs)
		}
		if isLastOwnerError(txErr) {
			return nil, lastOwnerDenial()
		}
		if errors.Is(txErr, store.ErrAlreadyExists) || errors.Is(txErr, store.ErrBuiltInMembershipConflict) {
			return nil, &MembershipDecision{Allowed: false, DenialCode: "conflict", Reason: txErr.Error(), HTTPStatus: 409}
		}
		return nil, &MembershipDecision{Allowed: false, DenialCode: "internal_error", Reason: txErr.Error(), HTTPStatus: 500}
	}

	svc.logger.Info("project member roles set via service",
		"project_id", req.ProjectID, "principal", req.PrincipalType+":"+req.PrincipalID,
		"actor", req.Actor.Email(), "created", result.Created)

	return &result, nil
}
