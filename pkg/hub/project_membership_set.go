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
	"net/http"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ---------------------------------------------------------------------------
// SetMemberRoles — atomic "set roles for principal" engine (ptone/scion#2529
// P1): PUT/DELETE a principal's whole project role set in one transaction,
// replacing the former add/update/remove-one-at-a-time flow for this surface.
//
// This file is the only place that decides whether an actor may create or
// remove a custom project-scoped role binding (ptone/scion#2529 acceptance
// A1). It is additive: AddMember, UpdateMemberRole and RemoveMember in
// project_membership_service.go are untouched, and every rs1_*/rs2_*/rs3_*/
// d002_*/pm1_* test keeps passing unmodified.
// ---------------------------------------------------------------------------

// Hub-level role_binding permission IDs. Named here (rather than inline
// strings scattered across call sites) because customRoleAuthorityFromStore's
// signature deliberately asks for "create" or "delete" authority separately:
// a later authority model could map them to different checks without
// changing the signature (ptone/scion#2529).
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
// bindings and the desired role definitions. current and desired are both
// already de-duplicated by role definition ID by the caller. currentDefs
// maps every binding in current to its role definition.
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

	// Identify the built-in swap, if any, for lifecycle inheritance: the new
	// built-in in a BuiltInChange inherits NotBefore/ExpiresAt from Old
	// (ptone/scion#2529).
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
// create or remove a custom project-scoped role binding (ptone/scion#2529
// acceptance A1).
// ---------------------------------------------------------------------------

// customRoleAuthority is the result of customRoleAuthorityFromStore.
type customRoleAuthority struct {
	Allowed bool
	// Via records how authority was granted: "project_owner" or
	// "hub_role_binding". Recorded in the custom-row audit summary. Empty
	// when Allowed is false.
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
// so there is exactly one implementation for both checks.
//
// Today: a direct project owner always has authority; otherwise an actor
// with NO project role of their own (the same "actorRole == \"\"" condition
// that gates the built-in governance override in checkGovernance /
// reevaluateActorTx) falls back to the existing system-scope hub override
// (actorHasHubRoleBindingAuthorityTx, which is itself store-generic and safe
// to call pre-transaction). This deliberately reuses the same two
// conditions "owner OR (no project role AND hub role_binding.*)" that
// already govern custom-role bind/unbind today, just decided in one
// function instead of inline at each call site. review r1 F2: an actor who
// already holds a project role (e.g. project-admin) does NOT get the hub
// fallback just because they separately hold hub role_binding.* — that
// would let the one actor-authority function disagree with built-in
// governance about what "hub override" means for the same actor.
//
// perm is PermRoleBindingCreate or PermRoleBindingDelete, asked separately
// even though both resolve to the same check today: a later authority model
// (e.g. seeding role_binding.* to project-owner) would map them to
// different permissions without changing this function's signature or any
// call site.
//
// No other function in this package may decide custom-role grant/revoke
// authority (ptone/scion#2529 acceptance A1).
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
// custom roles (ptone/scion#2529 Part 0 escalation guard (ii); review r1 F1).
//
// customRoleAuthorityFromStore above decides WHO may create or remove a
// custom role binding; this guard decides WHICH custom roles may be created
// at all, regardless of who is asking. It runs before, and independently of,
// customRoleAuthorityFromStore and CanDelegate, so it also refuses the hub
// role_binding.* override actor — the one actor CanDelegate cannot refuse,
// because that actor's own ceiling already includes role_binding.create/
// delete. Granting a custom role that itself carries role_binding.* would
// mint a project-scoped delegation grant that is inert today (role_binding.*
// is currently evaluated at system scope only) but live the moment project
// scope is honored — this endpoint must not be the one that creates those
// latent grants.
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

// roleDefinitionRefetchError reports which role definition
// refetchRoleDefinitionsTx failed to re-read, wrapping the store error so
// callers can still test it with errors.Is (e.g. store.ErrNotFound).
type roleDefinitionRefetchError struct {
	roleDefinitionID string
	err              error
}

func (e *roleDefinitionRefetchError) Error() string {
	return fmt.Sprintf("re-fetch role definition %s under lock: %v", e.roleDefinitionID, e.err)
}

func (e *roleDefinitionRefetchError) Unwrap() error { return e.err }

// refetchRoleDefinitionsTx re-fetches each of defs through tx (rather than
// trusting the pre-transaction-resolved pointers), by ID, preserving order.
// Used by the in-tx role_binding.* re-check (review r1 A-O2) so a role
// definition's permissions edited between Phase P and Phase T are seen by
// the re-check, instead of silently reusing the Phase P snapshot. The
// project lock does not cover role definitions; an edit committed after
// this read is the accepted FYI-2 residual. A failed read is returned as a
// *roleDefinitionRefetchError naming the failing ID; a (nil, nil) read is
// reported as a failed read wrapping store.ErrNotFound.
func refetchRoleDefinitionsTx(ctx context.Context, tx store.Store, defs []*store.RoleDefinition) ([]*store.RoleDefinition, error) {
	refetched := make([]*store.RoleDefinition, 0, len(defs))
	for _, d := range defs {
		rd, err := tx.GetRoleDefinition(ctx, d.ID)
		if err != nil {
			return nil, &roleDefinitionRefetchError{roleDefinitionID: d.ID, err: err}
		}
		if rd == nil {
			// A store returning (nil, nil) must not leak a nil definition
			// into the result (checkNoRoleBindingPermissionInCreatedCustomRoles
			// would dereference it). Treat it as a deleted role so the
			// caller maps it to 400 invalid_role_set, as for ErrNotFound.
			return nil, &roleDefinitionRefetchError{roleDefinitionID: d.ID, err: store.ErrNotFound}
		}
		refetched = append(refetched, rd)
	}
	return refetched, nil
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
// authority (ptone/scion#2529 acceptance A2).
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
// matrix or to the (precomputed) custom-role authority result. customAuth
// holds the already-evaluated authority for whichever of
// PermRoleBindingCreate/PermRoleBindingDelete the plan needs;
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
// lock. Custom-role authority is NOT re-evaluated here; SetMemberRoles calls
// customRoleAuthorityFromStore(tx) separately so the two decisions stay
// independent (ptone/scion#2529).
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

	// No built-in project role: the system-only hub override, revalidated
	// under the lock. Who reaches it (only actors who pass the members
	// endpoints' project.manage gate with no built-in project role, in
	// practice a non-member super-admin) is documented at the pre-tx branch
	// in memberActorAuthorityPreTx (ptone/scion#2646 item 1). Both
	// role_binding.create and role_binding.delete authority are required
	// when the plan has both creates and removes.
	if needCreate {
		ok, hErr := svc.actorHasHubRoleBindingAuthorityTx(ctx, tx, actorID, MembershipOpAdd)
		if hErr != nil {
			return "", false, false, fmt.Errorf("hub authority revalidation failed (fail-closed): %w", hErr)
		}
		if !ok {
			return "", false, false, asGovernanceDenial(*noProjectRoleUnderLockDecision())
		}
	}
	if needDelete {
		ok, hErr := svc.actorHasHubRoleBindingAuthorityTx(ctx, tx, actorID, MembershipOpRemove)
		if hErr != nil {
			return "", false, false, fmt.Errorf("hub authority revalidation failed (fail-closed): %w", hErr)
		}
		if !ok {
			return "", false, false, asGovernanceDenial(*noProjectRoleUnderLockDecision())
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

// governanceDenial builds the in-transaction refusal for a re-evaluated
// governance check that carries only a status and a reason: 404 maps to
// not_found, 409 to conflict, and anything else to
// role_assignment_forbidden. The decision has no Details.
func governanceDenial(status int, reason string) error {
	code := ErrCodeRoleAssignmentForbidden
	switch status {
	case http.StatusNotFound:
		code = "not_found"
	case http.StatusConflict:
		code = "conflict"
	}
	return asGovernanceDenial(MembershipDecision{
		Allowed:    false,
		DenialCode: code,
		Reason:     reason,
		HTTPStatus: status,
	})
}

// membershipChangedError signals that the principal's role set changed
// between the pre-transaction read and the locked re-read, i.e. a stale
// client-side dialog (ptone/scion#2529).
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
		Details: map[string]interface{}{
			"currentRoleDefinitionIds": currentRoleDefinitionIDs,
			"cause":                    causePrincipalRolesChanged,
		},
	}
}

// actorAuthorityChangedError signals the R2-2 case: the PRINCIPAL's role set
// did not change (current1 == current0, already checked before this error
// can be raised), but the ACTOR's own authority source moved between Phase P
// and Phase T — e.g. a direct owner who also holds hub role_binding.* is
// demoted from ownership by a concurrent request before the lock lands.
// Distinct from membershipChangedError (review r3 R3-1) because reusing that
// error's "the principal's project roles changed" reason makes a false claim
// here, and misleads a P3 Add-mode client (expected: []) into reporting
// "already a member" when no membership exists yet.
type actorAuthorityChangedError struct {
	currentRoleDefinitionIDs []string
}

func (e *actorAuthorityChangedError) Error() string {
	return "actor authority changed since the request was built"
}

// Discriminator values for MembershipDecision.Details["cause"] on a 409
// membership_changed response, so a client (P3) can tell "the principal you
// were editing changed" (reload and treat as already-a-member in Add mode)
// apart from "your own authority changed" (retry the same request; the
// principal is unaffected) instead of conflating both under one reason
// string (review r3 R3-1).
const (
	causePrincipalRolesChanged = "principal_roles_changed"
	causeActorAuthorityChanged = "actor_authority_changed"
)

func actorAuthorityChangedDecision(currentRoleDefinitionIDs []string) *MembershipDecision {
	return &MembershipDecision{
		Allowed:    false,
		DenialCode: ErrCodeMembershipChanged,
		Reason:     "your authority in this project changed while the request was being processed; retry",
		HTTPStatus: 409,
		Details: map[string]interface{}{
			"currentRoleDefinitionIds": currentRoleDefinitionIDs,
			"cause":                    causeActorAuthorityChanged,
		},
	}
}

// actorAuthoritySnapshot captures the actor-authority inputs that
// customRoleAuthorityFromStore/the built-in governance override are a
// function of, at one phase of SetMemberRoles.
type actorAuthoritySnapshot struct {
	role        string
	hubOverride bool
	// customAuth is keyed by whichever of PermRoleBindingCreate/
	// PermRoleBindingDelete the plan asked for at that phase.
	customAuth map[string]customRoleAuthority
}

// actorAuthorityChanged reports whether the actor's authority SOURCE moved
// between pre (Phase P) and post (Phase T, under lock) — not whether the
// outcome of any single governance check changed, but whether the inputs
// that fed Phase P's CanDelegate call are still the inputs that will govern
// the commit (review r2 R2-2).
//
// It checks three components as independent, defence-in-depth guards, even
// though today role equality alone already implies the other two can't
// differ (review r3 R3-3):
//   - hubOverride is fully determined by role == "": Phase P sets
//     pre.hubOverride that way (or returns early), and reevaluateActorTx
//     sets post.hubOverride the same way (or returns an error). Equal roles
//     imply equal hubOverride.
//   - A pre-allowed Via is either "project_owner" (requires role == "owner";
//     groups cannot confer owner, so this is exactly an active direct-owner
//     binding) or "hub_role_binding" (requires role == ""). A denied
//     pre-Via would already have returned 403 in Phase P. So equal roles
//     imply equal Via for every perm actually asked.
//
// A perm asked in one phase's customAuth but not the other's is also treated
// as changed (fail closed), even though plan1 == plan0 by construction
// (current1 == current0) means this is unreached today: an asked-set
// mismatch is exactly the kind of unexpected divergence this guard exists to
// catch, and treating it as "unchanged" would defeat that purpose (review
// r1 R2).
//
// Each sub-check is kept, rather than collapsed to "role != role", in case a
// future authority source (e.g. a group-mediated hub override) decouples
// hubOverride or Via from role — see TestActorAuthorityChanged
// (project_membership_plan_test.go), which drives each branch independently
// so a regression that reintroduces such coupling is caught even though no
// production path can reach it today.
func actorAuthorityChanged(pre, post actorAuthoritySnapshot) bool {
	if pre.role != post.role {
		return true
	}
	if pre.hubOverride != post.hubOverride {
		return true
	}
	for _, perm := range []string{PermRoleBindingCreate, PermRoleBindingDelete} {
		preAuth, preAsked := pre.customAuth[perm]
		postAuth, postAsked := post.customAuth[perm]
		if preAsked != postAsked {
			// An asked-set mismatch means the plan diverged between phases;
			// refuse. plan1 == plan0 by construction (current1 == current0)
			// means this is unreached today, but failing open here would
			// silently drop the one case the mismatch itself signals.
			return true
		}
		if preAsked && preAuth.Via != postAuth.Via {
			return true
		}
	}
	return false
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
// scoped role definition, applying the structural validation rule: an
// unknown ID or a non-project-scoped role is invalid_role_set, and more than
// one built-in membership role is invalid_role_set.
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
// principal's whole project role set. It runs in two phases: Phase P
// (pre-transaction reads and checks, against svc.store) computes and
// authorizes a plan; Phase T (inside one WithTx, under the project lock)
// re-reads and re-authorizes against tx before applying it, so that nothing
// decided in Phase P is trusted to still hold once the lock is held. The
// phase/step comments below mark each part of that sequence.
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
		// been evaluated.
		return &SetMemberRolesResult{Before: current0, After: current0, Created: false, Changed: false}, nil
	}

	// Escalation guard (ii), structural and actor-independent (F1): refuse
	// any created custom role carrying a role_binding.* permission before
	// anything else — including before actor authority is even determined,
	// so the hub role_binding.* override actor is refused exactly like
	// everyone else. memberRoleDecision reads no actor authority for this
	// check.
	for _, d := range plan0.Create {
		if dec, _ := svc.memberRoleDecision(ctx, req.Actor, req.ProjectID, nil, planChange{}, d, memberRoleCheckStructural); dec != nil {
			return nil, dec
		}
	}

	// Principal eligibility applies only to NEW bindings (plan0.Create):
	// keeping a custom role an ineligible principal already holds is
	// allowed (ptone/scion#2529 acceptance D1).
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
	// Project members groups cannot be granted roles. Only new bindings are
	// refused: keeping an unchanged set (the idempotent return above) and
	// removing roles stay allowed so existing bindings can be cleaned up.
	if len(plan0.Create) > 0 && isProjectMembersGroupPrincipal(ctx, svc.store, req.PrincipalType, req.PrincipalID) {
		return nil, projectMembersGroupPrincipalDecision(req.PrincipalID)
	}

	authPre, aErr := svc.memberActorAuthorityPreTx(ctx, req.Actor.ID(), req.ProjectID,
		len(plan0.Create) > 0, len(plan0.Remove) > 0, plan0.hasCustomCreate(), plan0.hasCustomRemove(currentDefs0))
	if aErr != nil {
		return nil, &MembershipDecision{Allowed: false, DenialCode: "internal_error", Reason: aErr.Error(), HTTPStatus: 500}
	}
	if authPre.authorityDenial != nil {
		return nil, authPre.authorityDenial
	}

	for _, ch := range plan0.changes(currentDefs0) {
		if d, _ := svc.memberRoleDecision(ctx, req.Actor, req.ProjectID, authPre, ch, nil, memberRoleCheckGovernance); d != nil {
			return nil, d
		}
	}

	// Pre-transaction CanDelegate: once per created binding, not once per
	// request (escalation test (iii)).
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
			dec, reason := svc.memberRoleDecision(ctx, req.Actor, req.ProjectID, authPre, planChange{}, d, memberRoleCheckCanDelegate)
			if dec != nil {
				return nil, dec
			}
			canDelegateReasons[d.ID] = reason
		}
	}

	// The addressed principal must exist before a binding is created for it.
	// Without this the binding create inside the transaction failed with the
	// store's not-found and surfaced as a 500 (ptone/scion#2529). Checked
	// last in Phase P, so every earlier refusal keeps its code.
	if len(plan0.Create) > 0 {
		if d := svc.principalExistsDecision(ctx, req.PrincipalType, req.PrincipalID); d != nil {
			return nil, d
		}
	}

	// --- Phase T: inside the transaction -----------------------------------

	result := SetMemberRolesResult{Created: len(current0) == 0}
	lossEnqueued := false
	txErr := svc.store.WithTx(ctx, func(tx store.Store) error {
		lossEnqueued = false
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

		// F1 / A-O2 (review r1): re-check the structural role_binding.*
		// guard under lock, against the CREATED role definitions' permissions
		// re-fetched through tx — not the desiredDefs pointers resolved
		// pre-transaction — so a role definition edited between Phase P and
		// this point (e.g. role_binding.create added to a role already
		// accepted by the pre-tx guard) is caught here too, using the same
		// error as the pre-tx guard. The project lock does not cover role
		// definitions; an edit committed after this read is the accepted
		// FYI-2 residual.
		//
		// R5-2 (review r5): a created role definition deleted between Phase
		// P and this read (DeleteRoleDefinition refuses only roles that still
		// have bindings) is reported exactly as Phase P reports an unknown ID
		// (resolveDesiredRoleDefs): 400 invalid_role_set with
		// details.roleDefinitionId. The request names a role that does not
		// exist by the time it is applied, so a retry cannot succeed; 409
		// membership_changed would invite a pointless retry.
		refetchedCreates, err := refetchRoleDefinitionsTx(ctx, tx, plan1.Create)
		if err != nil {
			var rfErr *roleDefinitionRefetchError
			if errors.As(err, &rfErr) && errors.Is(err, store.ErrNotFound) {
				return asGovernanceDenial(MembershipDecision{
					Allowed: false, DenialCode: ErrCodeInvalidRoleSet,
					Reason: "unknown role definition: " + rfErr.roleDefinitionID, HTTPStatus: 400,
					Details: map[string]interface{}{"roleDefinitionId": rfErr.roleDefinitionID},
				})
			}
			return fmt.Errorf("re-fetch created role definitions under lock: %w", err)
		}
		for _, d := range refetchedCreates {
			if dec, _ := svc.memberRoleDecision(ctx, req.Actor, req.ProjectID, nil, planChange{}, d, memberRoleCheckStructural); dec != nil {
				return asGovernanceDenial(*dec)
			}
		}

		customAuthTx, err := svc.customRoleAuthorities(ctx, tx, req.Actor.ID(), req.ProjectID, plan1.hasCustomCreate(), plan1.hasCustomRemove(currentDefs1))
		if err != nil {
			var caErr *customRoleAuthorityError
			if errors.As(err, &caErr) {
				op := "create"
				if caErr.perm == PermRoleBindingDelete {
					op = "delete"
				}
				return fmt.Errorf("custom role authority (%s) under lock: %w", op, caErr.err)
			}
			return err // defensive: customRoleAuthorities only returns *customRoleAuthorityError today
		}
		authTx := &memberActorAuthority{role: actorRole, isDirectOwner: isDirectOwner, hubOverride: hubOverride, customAuth: customAuthTx}

		// R2-2 (review r2): Phase P's CanDelegate call ran once, before the
		// lock, against the actor's authority SOURCE at that moment
		// (authPre: role, hubOverride, customAuth). It is not, and cannot
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
		// asked custom-authority source moved. review r3 R3-1: this is the
		// actor's OWN authority changing, not the principal's role set (that
		// was already checked above), so it gets its own error/discriminator
		// rather than reusing membershipChangedError's "principal" wording.
		if actorAuthorityChanged(
			actorAuthoritySnapshot{role: authPre.role, hubOverride: authPre.hubOverride, customAuth: authPre.customAuth},
			actorAuthoritySnapshot{role: actorRole, hubOverride: hubOverride, customAuth: customAuthTx},
		) {
			return &actorAuthorityChangedError{currentRoleDefinitionIDs: roleDefIDs(current1)}
		}

		// Governance only: CanDelegate is never re-run under the lock (see
		// R2-2 above, and memberRoleDecision on SQLite deadlocks).
		for _, ch := range plan1.changes(currentDefs1) {
			if d, _ := svc.memberRoleDecision(ctx, req.Actor, req.ProjectID, authTx, ch, nil, memberRoleCheckGovernance); d != nil {
				return asGovernanceDenial(*d)
			}
		}

		// Last-owner guard, part 1 (ptone/scion#2769): note, before the
		// plan is applied, whether it removes a usable owner binding.
		var removedOwners []*store.RoleBinding
		for _, b := range plan1.Remove {
			if rd := currentDefs1[b.RoleDefinitionID]; rd != nil && rd.Name == store.ProjectRoleOwner {
				removedOwners = append(removedOwners, b)
			}
		}
		now := svc.nowFunc() // one instant for the pre-state and post-state checks
		removedUsable, err := anyUsableOwnerBinding(ctx, tx, removedOwners, now)
		if err != nil {
			return fmt.Errorf("cannot verify usable owner: %w", err)
		}

		// Apply the plan: every direct role-binding mutation for this request
		// goes through applyRolePlanTx (project_membership_service.go), the
		// one purpose-named step the authzop mutation catalog classifies for
		// this engine (review r1 F3).
		created, err := svc.applyRolePlanTx(ctx, tx, plan1, req.PrincipalType, req.PrincipalID, req.ProjectID, req.Actor.ID(), req.NotBefore, req.ExpiresAt)
		if err != nil {
			return err
		}
		// A plan that deletes a binding can end the principal's access:
		// re-evaluate it (ptone/scion#3433). A no-op when access continues.
		if len(plan1.Remove) > 0 {
			lossTrigger := store.MembershipLossTriggerMemberRoleChange
			if req.RemoveAll {
				lossTrigger = store.MembershipLossTriggerMemberPrincipalDelete
			}
			if err := enqueueMembershipLossForPrincipalTx(ctx, tx, req.PrincipalType, req.PrincipalID, req.ProjectID, lossTrigger, auditActorFromContext(ctx)); err != nil {
				return err
			}
			lossEnqueued = true
		}

		var builtInNewBindingID string
		if plan1.BuiltInChange != nil {
			if cb := created[plan1.BuiltInChange.New.ID]; cb != nil {
				builtInNewBindingID = cb.ID
			}
		}

		// Last-owner guard, part 2: evaluated on the full post-state, inside
		// the same transaction as the mutations it may roll back.
		if len(removedOwners) > 0 {
			if err := enforceOwnerRemovalTx(ctx, tx, req.ProjectID, now, removedUsable); err != nil {
				return err
			}
		}

		// Audit: one row per binding change, sharing one CorrelationID.
		// createAuditRecord/auditActorFromContext fill the correlation ID
		// from the request context.
		var builtInOldBindingID string
		if plan1.BuiltInChange != nil {
			builtInOldBindingID = plan1.BuiltInChange.Old.ID
			oldName := ""
			if rd := currentDefs1[plan1.BuiltInChange.Old.RoleDefinitionID]; rd != nil {
				oldName = rd.Name
			}
			// N5 (review r1): carry roleKind and principalType here too — the
			// contract is that every audit row carries roleKind for uniform
			// filtering, and a built-in swap is always roleKind:"builtin" on
			// both sides by construction (planRoleSet only populates
			// BuiltInChange from built-in role names).
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
		var acErr *actorAuthorityChangedError
		if errors.As(txErr, &acErr) {
			return nil, actorAuthorityChangedDecision(acErr.currentRoleDefinitionIDs)
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
		if d := storeMembersGroupPrincipalDecision(txErr); d != nil {
			return nil, d
		}
		return nil, &MembershipDecision{Allowed: false, DenialCode: "internal_error", Reason: txErr.Error(), HTTPStatus: 500}
	}

	if lossEnqueued {
		svc.notifyMembershipLoss()
	}

	svc.logger.Info("project member roles set via service",
		"project_id", req.ProjectID, "principal", req.PrincipalType+":"+req.PrincipalID,
		"actor", req.Actor.Email(), "created", result.Created)

	return &result, nil
}

// noProjectRoleDecision is the refusal for an actor with no project role and
// no hub role_binding.* authority for the requested operation. Shared by
// SetMemberRoles, AssignableRoles and the legacy single-binding paths
// (checkGovernance, used by AddMember/UpdateMemberRole/RemoveMember) so all
// report it identically (ptone/scion#2600).
func noProjectRoleDecision() *MembershipDecision {
	return &MembershipDecision{Allowed: false, DenialCode: ErrCodeRoleAssignmentForbidden, Reason: "actor has no project role", HTTPStatus: 403}
}

// noProjectRoleUnderLockDecision is noProjectRoleDecision's in-transaction
// twin: the actor's project role and hub role_binding.* authority were
// re-evaluated under the project lock and neither holds any more. Shared by
// reevaluateActorTx (SetMemberRoles Phase T) and the legacy AddMember /
// UpdateMemberRole transactions (ptone/scion#2600).
func noProjectRoleUnderLockDecision() *MembershipDecision {
	return &MembershipDecision{Allowed: false, DenialCode: ErrCodeRoleAssignmentForbidden, Reason: "actor has no project role (re-evaluated under lock)", HTTPStatus: 403}
}

// canDelegateRefusal is the refusal for a created binding of rd that
// CanDelegate denied with reason. Shared by SetMemberRoles, AssignableRoles
// and the legacy AddMember so all report the same code, reason and details.
func canDelegateRefusal(rd *store.RoleDefinition, reason string) *MembershipDecision {
	return canDelegateRefusalFor(rd, "the requested role", reason)
}

// canDelegateRefusalFor is canDelegateRefusal with the role named by subject
// in the message ("actor cannot delegate <subject>: <reason>"). Only the
// legacy UpdateMemberRole uses a subject other than "the requested role"
// ("the new role"), which its PATCH response has always carried; the code,
// status and details are the same for every caller (ptone/scion#2600). The
// legacy POST/PATCH handlers do not render Details, so their response bodies
// are unchanged by carrying them.
func canDelegateRefusalFor(rd *store.RoleDefinition, subject, reason string) *MembershipDecision {
	return &MembershipDecision{
		Allowed: false, DenialCode: ErrCodeTargetRoleProtected,
		Reason:     "actor cannot delegate " + subject + ": " + reason,
		HTTPStatus: 403,
		Details:    map[string]interface{}{"roleDefinitionId": rd.ID, "roleName": rd.Name, "reason": reason},
	}
}

// principalExistsDecision refuses a user or agent principal ID that names no
// record with the 400 invalid_request an unknown email already gets on the
// members PUT. Only the addressed principal's not-found is mapped; any other
// store error is a 500. Groups are looked up during address resolution, so
// they pass through.
func (svc *ProjectMembershipService) principalExistsDecision(ctx context.Context, principalType, principalID string) *MembershipDecision {
	var err error
	switch principalType {
	case store.RoleBindingPrincipalUser:
		_, err = svc.store.GetUser(ctx, principalID)
	case store.RoleBindingPrincipalAgent:
		_, err = svc.store.GetAgent(ctx, principalID)
	default:
		return nil
	}
	if err == nil {
		return nil
	}
	if errors.Is(err, store.ErrNotFound) {
		return &MembershipDecision{
			Allowed: false, DenialCode: ErrCodeInvalidRequest,
			Reason:     principalType + " not found: " + principalID,
			HTTPStatus: 400,
		}
	}
	return &MembershipDecision{Allowed: false, DenialCode: ErrCodeInternalError, Reason: err.Error(), HTTPStatus: 500}
}
