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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ---------------------------------------------------------------------------
// The shared per-role membership decision (ptone/scion#2646 item 2).
//
// The members PUT (SetMemberRoles) and GET …/members/assignable-roles
// (AssignableRoles) share the per-check logic and the refusal constructors
// through memberRoleDecision and its helpers. What is shared is how each
// individual check decides and how its refusal is built, not the order in
// which a request runs them. The fixed order inside memberRoleDecision,
//
//	credential → structural role_binding.* refusal → actor authority
//	(no project role and no hub role_binding.*) → governance (built-in
//	matrix or custom-role authority) → CanDelegate,
//
// binds only AssignableRoles, the one caller that selects more than one
// check per call (memberRoleCheckAll, one role at a time). It is also the
// only caller that selects memberRoleCheckCredential and
// memberRoleCheckActorAuthority.
//
// SetMemberRoles selects exactly one check per call and runs each across the
// whole plan (every created role through the structural check, then every
// change through governance, then every created role that needs it through
// CanDelegate), because which role's refusal a multi-role PUT reports first
// is part of its contract. Its stage order is therefore defined by the
// sequence of loops in SetMemberRoles, not by this function: its credential
// gate, principal eligibility and actor-authority refusal are plan-level
// code outside the per-role loops. Nothing here stops that order drifting
// from assignable-roles; TestAssignableRoles_ConsistentWithPut is what pins
// the two against each other.
//
// The actor-side input (memberActorAuthority) is computed once per request.
// Custom-role authority is gathered through customRoleAuthorities, which is
// store-parameterised so SetMemberRoles uses it both pre-transaction
// (svc.store) and under the lock (tx); customRoleAuthorityFromStore stays
// the single function that decides it. CanDelegate is only ever selected
// pre-transaction: it reads through the authz service's own store, and
// running it inside WithTx deadlocks SQLite.
// ---------------------------------------------------------------------------

// memberRoleCheck selects which steps of memberRoleDecision run.
type memberRoleCheck uint8

const (
	memberRoleCheckCredential memberRoleCheck = 1 << iota
	memberRoleCheckStructural
	memberRoleCheckActorAuthority
	memberRoleCheckGovernance
	memberRoleCheckCanDelegate

	memberRoleCheckAll = memberRoleCheckCredential | memberRoleCheckStructural |
		memberRoleCheckActorAuthority | memberRoleCheckGovernance | memberRoleCheckCanDelegate
)

// memberActorAuthority is the actor-side input to memberRoleDecision.
type memberActorAuthority struct {
	// credentialDenial, when set, refuses every role (the credential gate
	// runs before any other check).
	credentialDenial *MembershipDecision
	// authorityDenial, when set, refuses every role: the actor has no
	// project role and lacks the hub role_binding.* authority the request
	// needs.
	authorityDenial *MembershipDecision
	role            string
	isDirectOwner   bool
	// hubOverride is the SYSTEM-SCOPE-ONLY hub role_binding override (see
	// checkBuiltInChangeGovernance), never custom-role authority.
	hubOverride bool
	// customAuth holds customRoleAuthorityFromStore's result for whichever
	// of PermRoleBindingCreate/PermRoleBindingDelete was asked.
	customAuth map[string]customRoleAuthority
}

// memberActorAuthorityPreTx evaluates the actor-side Phase P checks once,
// against svc.store through the non-transactional helpers (which fail
// closed). needCreate/needDelete say whether the request creates/removes
// any binding (both hub authorities are required for a plan that does
// both); needCustomCreate/needCustomDelete say which custom-role authority
// to ask for. On an authority denial, custom-role authority is not asked.
// The credential gate is not evaluated here.
func (svc *ProjectMembershipService) memberActorAuthorityPreTx(ctx context.Context, actorID, projectID string, needCreate, needDelete, needCustomCreate, needCustomDelete bool) (*memberActorAuthority, error) {
	a := &memberActorAuthority{}
	a.role = svc.projectEffectiveRole(ctx, actorID, projectID)
	if a.role == "" {
		// The system-only hub override (ptone/scion#2646 item 1). Over HTTP
		// both callers sit behind a project.manage gate, so this branch is
		// reached only by an actor who holds project.manage WITHOUT any
		// built-in project role (direct or group-derived: those set a.role)
		// and who also holds SYSTEM-scope role_binding.create/delete:
		//   - a super-admin (system scope, every permission) who is not a
		//     member of the project — the common, intended case: platform
		//     admins managing a project they do not belong to
		//     (TestSetMemberRoles_HubOverride_ReachableBySuperAdminOverHTTP);
		//   - a holder of a custom role carrying project.manage, project-
		//     scoped on this project or system-scoped, who also holds system
		//     role_binding.* (e.g. hub-admin, which alone lacks
		//     project.manage and so fails the gate).
		// The override bypasses only the built-in governance matrix and the
		// direct-owner rule (checkBuiltInChangeGovernance); the structural
		// role_binding.* guard, custom-role authority, CanDelegate, the
		// last-owner guard and the in-tx revalidation (reevaluateActorTx)
		// still apply. Kept deliberately: removing it would leave
		// non-member super-admins unable to manage membership through these
		// endpoints.
		authorized := true
		if needCreate && !svc.actorHasHubRoleBindingAuthority(ctx, actorID, MembershipOpAdd) {
			authorized = false
		}
		if needDelete && !svc.actorHasHubRoleBindingAuthority(ctx, actorID, MembershipOpRemove) {
			authorized = false
		}
		if !authorized {
			a.authorityDenial = noProjectRoleDecision()
			return a, nil
		}
		a.hubOverride = true
	} else {
		a.isDirectOwner = svc.isActorDirectOwner(ctx, actorID, projectID)
	}

	customAuth, err := svc.customRoleAuthorities(ctx, svc.store, actorID, projectID, needCustomCreate, needCustomDelete)
	if err != nil {
		return nil, err
	}
	a.customAuth = customAuth
	return a, nil
}

// customRoleAuthorityError names which permission's custom-role authority
// lookup failed. Error() is the underlying error's text, unchanged.
type customRoleAuthorityError struct {
	perm string
	err  error
}

func (e *customRoleAuthorityError) Error() string { return e.err.Error() }

func (e *customRoleAuthorityError) Unwrap() error { return e.err }

// customRoleAuthorities asks customRoleAuthorityFromStore, through s, for
// PermRoleBindingCreate when needCreate and PermRoleBindingDelete when
// needDelete, in that order. s is svc.store pre-transaction and tx under
// the lock. A failed lookup is returned as a *customRoleAuthorityError.
func (svc *ProjectMembershipService) customRoleAuthorities(ctx context.Context, s store.Store, actorID, projectID string, needCreate, needDelete bool) (map[string]customRoleAuthority, error) {
	out := make(map[string]customRoleAuthority, 2)
	for _, p := range []struct {
		need bool
		perm string
	}{{needCreate, PermRoleBindingCreate}, {needDelete, PermRoleBindingDelete}} {
		if !p.need {
			continue
		}
		auth, err := svc.customRoleAuthorityFromStore(ctx, s, actorID, projectID, p.perm)
		if err != nil {
			return nil, &customRoleAuthorityError{perm: p.perm, err: err}
		}
		out[p.perm] = auth
	}
	return out, nil
}

// memberRoleDecision is the per-role membership decision shared by
// SetMemberRoles and AssignableRoles. It runs the selected checks for one
// role, in the fixed order documented above, and returns the first refusal,
// or nil when every selected check passes. When CanDelegate runs and
// allows, its reason is returned for the audit row.
//
// ch is the governance change (op and role name) and is read only by the
// governance check; rd is the created role definition and is read only by
// the structural and CanDelegate checks. a is read only by the credential,
// actor-authority and governance checks. memberRoleCheckCanDelegate must
// only be selected outside a transaction. A selected check whose input (a or
// rd) is nil is a caller bug and is refused with an internal error rather
// than a panic.
func (svc *ProjectMembershipService) memberRoleDecision(ctx context.Context, actor UserIdentity, projectID string, a *memberActorAuthority, ch planChange, rd *store.RoleDefinition, checks memberRoleCheck) (*MembershipDecision, string) {
	if checks&memberRoleCheckCredential != 0 {
		if a == nil {
			return memberRoleDecisionMissingInput("actor authority"), ""
		}
		if a.credentialDenial != nil {
			return a.credentialDenial, ""
		}
	}
	if checks&memberRoleCheckStructural != 0 {
		if rd == nil {
			return memberRoleDecisionMissingInput("role definition"), ""
		}
		if d := checkNoRoleBindingPermissionInCreatedCustomRoles([]*store.RoleDefinition{rd}); d != nil {
			return d, ""
		}
	}
	if checks&memberRoleCheckActorAuthority != 0 {
		if a == nil {
			return memberRoleDecisionMissingInput("actor authority"), ""
		}
		if a.authorityDenial != nil {
			return a.authorityDenial, ""
		}
	}
	if checks&memberRoleCheckGovernance != 0 {
		if a == nil {
			return memberRoleDecisionMissingInput("actor authority"), ""
		}
		if d := svc.governanceDecisionForChange(a.role, a.isDirectOwner, a.hubOverride, a.customAuth, ch); d != nil {
			return d, ""
		}
	}
	if checks&memberRoleCheckCanDelegate != 0 && svc.authz != nil {
		if rd == nil {
			return memberRoleDecisionMissingInput("role definition"), ""
		}
		delDecision := svc.authz.CanDelegate(ctx, actor, GrantDescriptor{
			Type:             GrantTypeRoleBinding,
			RoleDefinitionID: rd.ID,
			ScopeType:        store.RoleScopeProject,
			ScopeID:          projectID,
		})
		if !delDecision.Allowed {
			return canDelegateRefusal(rd, delDecision.Reason), ""
		}
		return nil, delDecision.Reason
	}
	return nil, ""
}

// memberRoleDecisionMissingInput is the internal-error refusal for a
// memberRoleDecision call that selected a check without supplying its input.
func memberRoleDecisionMissingInput(input string) *MembershipDecision {
	return &MembershipDecision{Allowed: false, DenialCode: ErrCodeInternalError, Reason: "membership decision: missing " + input, HTTPStatus: 500}
}
