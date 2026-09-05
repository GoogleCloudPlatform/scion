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
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ---------------------------------------------------------------------------
// ProjectMembershipService — RS1 bounded domain service
//
// All project membership and ownership mutations flow through this service.
// HTTP handlers validate transport input and delegate; they never directly
// create, update, or delete project RoleBindings. Seed/migration/recovery
// paths use narrow typed exemptions already modeled by AF1.
//
// The service implements:
//   - The CT1 D5 typed governance matrix (actor/operation/target authority)
//   - CT1 D4 one-binding-per-principal invariant with atomic replacement
//   - CT1 D1 atomic ownership transfer
//   - CT1 D2 active-at-decision-time lifecycle enforcement
//   - CT1 D3 group eligibility for project-admin
//   - Delegation checks (non-amplification / conditional-on-increase)
//   - Last-owner post-state invariant
//   - Durable audit for every authority change
//   - Stable denial codes (lower_snake_case)
// ---------------------------------------------------------------------------

// ProjectMembershipService is the bounded domain service for project
// membership and ownership mutations.
type ProjectMembershipService struct {
	store   store.Store
	authz   *AuthzService
	logger  *slog.Logger
	nowFunc func() time.Time
}

// NewProjectMembershipService creates a new ProjectMembershipService.
func NewProjectMembershipService(
	s store.Store,
	authz *AuthzService,
	logger *slog.Logger,
) *ProjectMembershipService {
	return &ProjectMembershipService{
		store:   s,
		authz:   authz,
		logger:  logger,
		nowFunc: time.Now,
	}
}

// createAuditRecord writes a mutation audit record synchronously within the
// caller's context (and transaction, if any). Unlike the fire-and-forget
// emitMutationAudit, this returns an error so the caller can roll back.
func (svc *ProjectMembershipService) createAuditRecord(ctx context.Context, txStore store.Store, record *store.MutationAuditRecord) error {
	// Populate actor identity from context if not already set.
	if record.ActorPrincipalKind == "" || record.ActorPrincipalID == "" {
		identity := GetIdentityFromContext(ctx)
		if identity != nil {
			record.ActorPrincipalKind = identity.Type()
			record.ActorPrincipalID = identity.ID()
			credential := GetCredentialContextFromContext(ctx)
			if credential.Kind != "" {
				record.ActorCredentialID = credential.ID
				record.ActorCredentialType = string(credential.Kind)
			}
		}
	}
	if record.Timestamp.IsZero() {
		record.Timestamp = svc.nowFunc()
	}
	return txStore.CreateMutationAudit(ctx, record)
}

// ---------------------------------------------------------------------------
// Request / Result / Decision types
// ---------------------------------------------------------------------------

// MembershipOp identifies the kind of membership mutation.
type MembershipOp string

const (
	MembershipOpAdd      MembershipOp = "add"
	MembershipOpUpdate   MembershipOp = "update"
	MembershipOpRemove   MembershipOp = "remove"
	MembershipOpTransfer MembershipOp = "transfer"
)

// MembershipRequest describes a project membership mutation.
type MembershipRequest struct {
	Op        MembershipOp
	ProjectID string
	Actor     UserIdentity

	// Add fields
	PrincipalType string
	PrincipalID   string
	RoleDefID     string
	NotBefore     *time.Time
	ExpiresAt     *time.Time

	// Update/Remove fields
	BindingID string

	// Update fields
	NewRoleDefID string

	// Transfer fields — the target user who will become the new owner.
	NewOwnerID string
}

// MembershipDecision captures the governance outcome.
type MembershipDecision struct {
	Allowed    bool
	DenialCode string
	Reason     string
	HTTPStatus int
}

// MembershipResult is the outcome of a successful membership mutation.
type MembershipResult struct {
	Binding  *store.RoleBinding
	OldRole  string
	NewRole  string
	Op       MembershipOp
	Replaced bool // true when an existing binding was atomically replaced
	Removed  *store.RoleBinding
	// For transfer: the old owner's new binding (downgraded to member).
	TransferOldOwnerBinding *store.RoleBinding
}

// ---------------------------------------------------------------------------
// Governance matrix — effective role resolution
// ---------------------------------------------------------------------------

// projectEffectiveRole returns the actor's effective project role, considering
// both direct bindings and group-derived bindings. Returns the highest-authority
// role found. "active-at-decision-time" per CT1 D2.
func (svc *ProjectMembershipService) projectEffectiveRole(ctx context.Context, userID, projectID string) string {
	now := svc.nowFunc()

	// 1. Direct user bindings.
	directBindings, err := svc.store.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
	if err != nil {
		return ""
	}

	rdCache := make(map[string]*store.RoleDefinition)
	getRoleDef := func(rdID string) *store.RoleDefinition {
		if rd, ok := rdCache[rdID]; ok {
			return rd
		}
		rd, err := svc.store.GetRoleDefinition(ctx, rdID)
		if err != nil {
			return nil
		}
		rdCache[rdID] = rd
		return rd
	}

	bestRole := ""
	for _, rb := range directBindings {
		if rb.ScopeType != store.RoleScopeProject || rb.ScopeID != projectID {
			continue
		}
		if !isBindingActive(rb, now) {
			continue
		}
		rd := getRoleDef(rb.RoleDefinitionID)
		if rd == nil {
			continue
		}
		bestRole = higherProjectRole(bestRole, rd.Name)
	}

	// 2. Group-derived bindings.
	groupIDs, err := svc.store.GetEffectiveGroups(ctx, userID)
	if err == nil && len(groupIDs) > 0 {
		var principals []store.PrincipalRef
		for _, gid := range groupIDs {
			principals = append(principals, store.PrincipalRef{Type: store.RoleBindingPrincipalGroup, ID: gid})
		}
		groupBindings, err := svc.store.ListRoleBindingsForPrincipals(ctx, principals, nil, nil)
		if err == nil {
			for _, rb := range groupBindings {
				if rb.ScopeType != store.RoleScopeProject || rb.ScopeID != projectID {
					continue
				}
				if !isBindingActive(rb, now) {
					continue
				}
				rd := getRoleDef(rb.RoleDefinitionID)
				if rd == nil {
					continue
				}
				// Groups can only confer admin or member, never owner.
				if rd.Name == store.ProjectRoleOwner {
					continue
				}
				bestRole = higherProjectRole(bestRole, rd.Name)
			}
		}
	}

	return bestRole
}

// isActorDirectOwner checks whether the actor has an active direct project-owner
// binding. This is the stricter check used for owner-level operations per CT1 D2.
func (svc *ProjectMembershipService) isActorDirectOwner(ctx context.Context, userID, projectID string) bool {
	return svc.authz.isProjectOwner(ctx, userID, projectID)
}

// ---------------------------------------------------------------------------
// R4 O-1: tx-aware authority re-evaluation helpers
//
// These variants read the actor's effective role and direct-owner status from
// the provided (transactional) store *after* the project lock has been
// acquired. This closes the TOCTOU window between pre-lock governance checks
// and the locked decision point: a concurrent demotion that commits between
// the pre-lock check and lock acquisition is visible to these helpers.
// ---------------------------------------------------------------------------

// projectEffectiveRoleFromStore is like projectEffectiveRole but reads from the
// provided store (which should be the transactional store inside a locked tx).
// R5 O-1: returns an error so callers can distinguish "no role" from "store
// failure" and surface 500 instead of a misleading 403.
func (svc *ProjectMembershipService) projectEffectiveRoleFromStore(ctx context.Context, s store.Store, userID, projectID string) (string, error) {
	now := svc.nowFunc()

	directBindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
	if err != nil {
		return "", fmt.Errorf("list direct bindings for actor %s: %w", userID, err)
	}

	rdCache := make(map[string]*store.RoleDefinition)
	getRoleDef := func(rdID string) (*store.RoleDefinition, error) {
		if rd, ok := rdCache[rdID]; ok {
			return rd, nil
		}
		rd, err := s.GetRoleDefinition(ctx, rdID)
		if err != nil {
			return nil, fmt.Errorf("get role definition %s: %w", rdID, err)
		}
		rdCache[rdID] = rd
		return rd, nil
	}

	bestRole := ""
	for _, rb := range directBindings {
		if rb.ScopeType != store.RoleScopeProject || rb.ScopeID != projectID {
			continue
		}
		if !isBindingActive(rb, now) {
			continue
		}
		rd, err := getRoleDef(rb.RoleDefinitionID)
		if err != nil {
			return "", err
		}
		bestRole = higherProjectRole(bestRole, rd.Name)
	}

	// Group-derived bindings (read from tx store).
	groupIDs, err := s.GetEffectiveGroups(ctx, userID)
	if err != nil {
		return "", fmt.Errorf("get effective groups for actor %s: %w", userID, err)
	}
	if len(groupIDs) > 0 {
		var principals []store.PrincipalRef
		for _, gid := range groupIDs {
			principals = append(principals, store.PrincipalRef{Type: store.RoleBindingPrincipalGroup, ID: gid})
		}
		groupBindings, err := s.ListRoleBindingsForPrincipals(ctx, principals, nil, nil)
		if err != nil {
			return "", fmt.Errorf("list group bindings: %w", err)
		}
		for _, rb := range groupBindings {
			if rb.ScopeType != store.RoleScopeProject || rb.ScopeID != projectID {
				continue
			}
			if !isBindingActive(rb, now) {
				continue
			}
			rd, err := getRoleDef(rb.RoleDefinitionID)
			if err != nil {
				return "", err
			}
			if rd.Name == store.ProjectRoleOwner {
				continue
			}
			bestRole = higherProjectRole(bestRole, rd.Name)
		}
	}

	return bestRole, nil
}

// isActorDirectOwnerFromStore checks whether the actor has an active direct
// project-owner binding using the provided (transactional) store.
// R5 O-1: returns an error so callers can distinguish "not owner" from "store
// failure" and surface 500 instead of a misleading 403.
func (svc *ProjectMembershipService) isActorDirectOwnerFromStore(ctx context.Context, s store.Store, userID, projectID string) (bool, error) {
	now := svc.nowFunc()
	bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
	if err != nil {
		return false, fmt.Errorf("list bindings for owner check %s: %w", userID, err)
	}
	ownerRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	if err != nil {
		return false, fmt.Errorf("get owner role definition: %w", err)
	}
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeProject && b.ScopeID == projectID &&
			b.RoleDefinitionID == ownerRD.ID && isBindingActive(b, now) {
			return true, nil
		}
	}
	return false, nil
}

// ---------------------------------------------------------------------------
// Global admin authority — hub-level role-binding permission override
// ---------------------------------------------------------------------------

// actorHasHubRoleBindingAuthority checks whether the actor holds the hub-level
// role_binding permission corresponding to the membership operation (create for
// add/update, delete for remove). This allows global administrators (super-admin,
// hub-admin, or any custom system-scoped role carrying these permissions) to
// manage project-scoped role bindings even when they have no project role.
//
// The check uses the AuthzService's effective permission resolver, which accounts
// for group-derived bindings, activation windows, and access constraints.
//
// Requirement: brief §1 — "A session-authenticated actor with effective
// hub/system role_binding.create or role_binding.delete authority must be able
// to manage project-scoped role bindings even when the actor is not a project
// member."
func (svc *ProjectMembershipService) actorHasHubRoleBindingAuthority(ctx context.Context, actorID string, op MembershipOp) bool {
	if svc.authz == nil {
		return false
	}
	var permission string
	switch op {
	case MembershipOpAdd, MembershipOpUpdate:
		permission = "role_binding.create"
	case MembershipOpRemove:
		permission = "role_binding.delete"
	default:
		return false
	}

	perms, err := svc.authz.getEffectivePermissions(ctx, "user", actorID, store.RoleScopeSystem, "")
	if err != nil {
		svc.logger.Warn("hub authority check failed (fail-closed)",
			"actor_id", actorID, "op", op, "error", err)
		return false
	}
	for _, p := range perms {
		if p == permission {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Governance check — the CT1 D5 typed governance matrix
// ---------------------------------------------------------------------------

// checkGovernance evaluates whether the actor is permitted to perform the
// described operation on the target role. Returns a decision with a stable
// denial code if denied.
//
// When the actor has no project role, a hub-level authority override is
// checked: actors with effective hub/system role_binding.create or
// role_binding.delete authority can manage project-scoped role bindings.
// Global admin override bypasses the project governance matrix and
// direct-owner requirement, but CanDelegate and last-owner invariants
// are enforced separately by the calling mutation methods.
func (svc *ProjectMembershipService) checkGovernance(ctx context.Context, req MembershipRequest, targetRoleName string) MembershipDecision {
	actorRole := svc.projectEffectiveRole(ctx, req.Actor.ID(), req.ProjectID)
	if actorRole == "" {
		// Brief §1: hub-level role_binding authority override. Server-side
		// re-proof — the handler-level check only gates the HTTP endpoint;
		// the domain service re-validates at its own boundary.
		if svc.actorHasHubRoleBindingAuthority(ctx, req.Actor.ID(), req.Op) {
			return MembershipDecision{Allowed: true}
		}
		return MembershipDecision{
			Allowed:    false,
			DenialCode: ErrCodeRoleAssignmentForbidden,
			Reason:     "actor has no project role",
			HTTPStatus: 403,
		}
	}

	// The governance matrix from CT1 D5:
	//
	// Actor role   | Target: member | Target: admin | Target: owner
	// -------------|----------------|---------------|---------------
	// member       | No             | No            | No
	// admin        | Yes (add/change/remove) | No   | No
	// owner        | Yes            | Yes           | Yes (last-owner guard)
	//
	// For update (role change), both old and new target roles must be governed.
	// For remove, the target role must be governed.
	// For add, the target role must be governed.

	permitted := svc.isOperationPermitted(actorRole, req.Op, targetRoleName)
	if !permitted {
		code := ErrCodeRoleAssignmentForbidden
		if isProtectedRole(targetRoleName) {
			code = ErrCodeTargetRoleProtected
		}
		return MembershipDecision{
			Allowed:    false,
			DenialCode: code,
			Reason:     fmt.Sprintf("actor role %q cannot %s target role %q", actorRole, req.Op, targetRoleName),
			HTTPStatus: 403,
		}
	}

	// Owner-level operations require a direct owner binding, not group-derived.
	// This check does not apply to hub-level global admins (already returned
	// above when actorRole == "").
	if requiresDirectOwner(targetRoleName) {
		if !svc.isActorDirectOwner(ctx, req.Actor.ID(), req.ProjectID) {
			return MembershipDecision{
				Allowed:    false,
				DenialCode: ErrCodeRoleAssignmentForbidden,
				Reason:     "only direct project owners can manage admin and owner roles",
				HTTPStatus: 403,
			}
		}
	}

	return MembershipDecision{Allowed: true}
}

// isOperationPermitted implements the governance matrix.
func (svc *ProjectMembershipService) isOperationPermitted(actorRole string, op MembershipOp, targetRole string) bool {
	switch actorRole {
	case store.ProjectRoleOwner:
		// Owners can perform any operation on any target role.
		return true
	case store.ProjectRoleAdmin:
		// Admins can only manage ordinary members.
		return targetRole == store.ProjectRoleMember
	default:
		// Members cannot manage membership.
		return false
	}
}

// requiresDirectOwner returns true if the target role requires the actor to
// hold a direct (not group-derived) project-owner binding.
func requiresDirectOwner(targetRole string) bool {
	return targetRole == store.ProjectRoleOwner || targetRole == store.ProjectRoleAdmin
}

// isProtectedRole returns true if the target role is protected (admin/owner).
func isProtectedRole(role string) bool {
	return role == store.ProjectRoleOwner || role == store.ProjectRoleAdmin
}

// ---------------------------------------------------------------------------
// Principal eligibility
// ---------------------------------------------------------------------------

// principalEligibleForRole checks whether the principal type is eligible for
// the target role. Per CT1 D3:
//   - project-owner: direct user only (permanent invariant)
//   - project-admin: direct user or group (D3 approved)
//   - project-member: user, agent, or group
func principalEligibleForRole(principalType, roleName string) bool {
	switch roleName {
	case store.ProjectRoleOwner:
		return principalType == store.RoleBindingPrincipalUser
	case store.ProjectRoleAdmin:
		// D3: groups are now eligible for project-admin.
		return principalType == store.RoleBindingPrincipalUser ||
			principalType == store.RoleBindingPrincipalGroup
	case store.ProjectRoleMember:
		return principalType == store.RoleBindingPrincipalUser ||
			principalType == store.RoleBindingPrincipalAgent ||
			principalType == store.RoleBindingPrincipalGroup
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// Core mutation operations
// ---------------------------------------------------------------------------

// AddMember adds a member to a project with governance, delegation, one-binding
// enforcement, and audit.
func (svc *ProjectMembershipService) AddMember(ctx context.Context, req MembershipRequest) (*MembershipResult, *MembershipDecision) {
	// Resolve target role definition.
	roleDef, err := svc.store.GetRoleDefinition(ctx, req.RoleDefID)
	if err != nil {
		return nil, &MembershipDecision{Allowed: false, DenialCode: "not_found", Reason: "role definition not found", HTTPStatus: 400}
	}
	if !validProjectRoles[roleDef.Name] {
		return nil, &MembershipDecision{Allowed: false, DenialCode: "forbidden", Reason: "invalid project role: " + roleDef.Name, HTTPStatus: 400}
	}
	if roleDef.ScopeType != store.RoleScopeProject {
		return nil, &MembershipDecision{Allowed: false, DenialCode: "forbidden", Reason: "only project-scoped roles can be assigned", HTTPStatus: 400}
	}

	// Principal eligibility.
	if !principalEligibleForRole(req.PrincipalType, roleDef.Name) {
		return nil, &MembershipDecision{
			Allowed:    false,
			DenialCode: ErrCodePrincipalIneligible,
			Reason:     fmt.Sprintf("role %q requires direct users only; cannot be assigned to %s principals", roleDef.Name, req.PrincipalType),
			HTTPStatus: 400,
		}
	}

	// Governance check.
	decision := svc.checkGovernance(ctx, req, roleDef.Name)
	if !decision.Allowed {
		return nil, &decision
	}

	// Delegation check (non-amplification): actor must hold all permissions
	// in the target role.
	if svc.authz != nil {
		delDecision := svc.authz.CanDelegate(ctx, req.Actor, GrantDescriptor{
			Type:             GrantTypeRoleBinding,
			RoleDefinitionID: req.RoleDefID,
			ScopeType:        store.RoleScopeProject,
			ScopeID:          req.ProjectID,
		})
		if !delDecision.Allowed {
			return nil, &MembershipDecision{
				Allowed:    false,
				DenialCode: ErrCodeTargetRoleProtected,
				Reason:     "actor cannot delegate the requested role: " + delDecision.Reason,
				HTTPStatus: 403,
			}
		}
	}

	// Pre-compute hub authority override BEFORE the transaction. This reads
	// system-scoped bindings which are not affected by the project lock, and
	// avoids a SQLite deadlock (svc.authz.store vs tx.store on the same DB).
	// The result is stable across the project lock because system-scoped
	// bindings are not mutated by project membership operations.
	preComputedHubOverride := svc.actorHasHubRoleBindingAuthority(ctx, req.Actor.ID(), req.Op)

	// R3-1 + O-1: acquire project lock and check existing bindings inside the
	// same transaction. The lock serializes concurrent membership mutations
	// for this project (FOR UPDATE on PostgreSQL; no-op on SQLite). The D4
	// existing-binding check inside the locked tx prevents two concurrent
	// AddMember requests from both seeing zero bindings and both creating.
	//
	// R4 O-1: the actor's effective role and direct-owner status are
	// re-evaluated from the transactional store AFTER acquiring the lock.
	// This closes the TOCTOU window where a concurrent demotion could change
	// the actor's authority between the pre-lock governance check and the
	// locked decision point.
	var result *MembershipResult
	txErr := svc.store.WithTx(ctx, func(tx store.Store) error {
		// Acquire project-scoped membership lock.
		if err := tx.LockProjectForMembership(ctx, req.ProjectID); err != nil {
			return fmt.Errorf("lock project: %w", err)
		}

		// R4 O-1: re-evaluate actor authority under lock.
		// R5 O-1: propagate store errors as 500 instead of masking as 403.
		actorRole, roleErr := svc.projectEffectiveRoleFromStore(ctx, tx, req.Actor.ID(), req.ProjectID)
		if roleErr != nil {
			return fmt.Errorf("authority lookup failed under lock: %w", roleErr)
		}
		// hubOverride tracks whether the actor is authorized through
		// hub-level role_binding authority rather than a project role.
		// When true, project governance matrix and direct-owner checks
		// are skipped, but CanDelegate and last-owner guards still apply.
		hubOverride := false
		actorIsDirectOwner := false
		if actorRole == "" {
			// Brief §1: hub-level authority override. Pre-computed before
			// the transaction to avoid SQLite deadlock; system-scoped
			// bindings are stable across the project lock.
			if preComputedHubOverride {
				hubOverride = true
			} else {
				return fmt.Errorf("governance:%d:%s", 403, "actor has no project role (re-evaluated under lock)")
			}
		}
		if !hubOverride {
			var ownerErr error
			actorIsDirectOwner, ownerErr = svc.isActorDirectOwnerFromStore(ctx, tx, req.Actor.ID(), req.ProjectID)
			if ownerErr != nil {
				return fmt.Errorf("owner lookup failed under lock: %w", ownerErr)
			}
			if !svc.isOperationPermitted(actorRole, req.Op, roleDef.Name) {
				return fmt.Errorf("governance:%d:%s", 403, fmt.Sprintf("actor role %q cannot %s target role %q (re-evaluated under lock)", actorRole, req.Op, roleDef.Name))
			}
			if requiresDirectOwner(roleDef.Name) && !actorIsDirectOwner {
				return fmt.Errorf("governance:%d:%s", 403, "only direct project owners can manage admin and owner roles (re-evaluated under lock)")
			}
		}

		// One-binding invariant (D4): re-check under lock.
		existingBindings, err := svc.findExistingDirectBindingsFromStore(ctx, tx, req.PrincipalType, req.PrincipalID, req.ProjectID)
		if err != nil {
			return fmt.Errorf("check existing bindings: %w", err)
		}
		if len(existingBindings) > 0 {
			// Atomic replacement inline (D4): replace existing binding(s) with
			// the new role inside this locked transaction.
			primary := svc.highestAuthorityBindingFromStore(ctx, tx, existingBindings)
			if primary == nil {
				primary = existingBindings[0]
			}
			oldRoleDef, rdErr := tx.GetRoleDefinition(ctx, primary.RoleDefinitionID)
			if rdErr != nil {
				return fmt.Errorf("resolve existing role: %w", rdErr)
			}
			// Idempotent: same role and single binding → return existing.
			if oldRoleDef.Name == roleDef.Name && len(existingBindings) == 1 {
				result = &MembershipResult{Binding: primary, NewRole: roleDef.Name, Op: MembershipOpAdd, Replaced: false}
				return nil
			}
			// Governance check for the old role (since we're replacing it).
			// actorRole already re-evaluated under lock above.
			// Hub-override actors bypass the project governance matrix.
			if !hubOverride {
				if !svc.isOperationPermitted(actorRole, req.Op, oldRoleDef.Name) {
					reason := fmt.Sprintf("actor role %q cannot %s target role %q", actorRole, req.Op, oldRoleDef.Name)
					if isProtectedRole(oldRoleDef.Name) {
						reason = "target role is protected: " + reason
					}
					return fmt.Errorf("governance:%d:%s", 403, reason)
				}
				if requiresDirectOwner(oldRoleDef.Name) && !actorIsDirectOwner {
					return fmt.Errorf("governance:%d:%s", 403, "only direct project owners can manage admin and owner roles")
				}
			}
			// Last-owner guard for demotions.
			if oldRoleDef.Name == store.ProjectRoleOwner && roleDef.Name != store.ProjectRoleOwner {
				if primary.PrincipalType == store.RoleBindingPrincipalUser {
					if err := svc.enforceLastOwnerTx(ctx, tx, req.ProjectID); err != nil {
						return err
					}
				}
			}
			replaced, rErr := svc.replaceBindingTx(ctx, tx, existingBindings, roleDef.ID, req.Actor.ID())
			if rErr != nil {
				return rErr
			}
			if aErr := svc.createAuditRecord(ctx, tx, &store.MutationAuditRecord{
				MutationType: "project_member_role_change",
				TargetType:   "project_membership",
				TargetID:     req.ProjectID,
				BeforeSummary: marshalAuditJSON(map[string]string{
					"principalId": primary.PrincipalID,
					"role":        oldRoleDef.Name,
				}),
				AfterSummary: marshalAuditJSON(map[string]string{
					"principalId": primary.PrincipalID,
					"role":        roleDef.Name,
				}),
			}); aErr != nil {
				return aErr
			}
			result = &MembershipResult{Binding: replaced, OldRole: oldRoleDef.Name, NewRole: roleDef.Name, Op: MembershipOpAdd, Replaced: true}
			return nil
		}

		// Create the new binding and audit record.
		created, cErr := tx.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: req.RoleDefID,
			PrincipalType:    req.PrincipalType,
			PrincipalID:      req.PrincipalID,
			ScopeType:        store.RoleScopeProject,
			ScopeID:          req.ProjectID,
			NotBefore:        req.NotBefore,
			ExpiresAt:        req.ExpiresAt,
			CreatedBy:        req.Actor.ID(),
		})
		if cErr != nil {
			return cErr
		}
		if aErr := svc.createAuditRecord(ctx, tx, &store.MutationAuditRecord{
			MutationType: "project_member_add",
			TargetType:   "project_membership",
			TargetID:     req.ProjectID,
			AfterSummary: marshalAuditJSON(map[string]string{
				"principalType": req.PrincipalType,
				"principalId":   req.PrincipalID,
				"role":          roleDef.Name,
			}),
		}); aErr != nil {
			return aErr
		}
		result = &MembershipResult{Binding: created, NewRole: roleDef.Name, Op: MembershipOpAdd}
		return nil
	})
	if txErr != nil {
		if txErr == store.ErrAlreadyExists {
			return nil, &MembershipDecision{Allowed: false, DenialCode: "conflict", Reason: "this member already has this role in this project", HTTPStatus: 409}
		}
		if isLastOwnerError(txErr) {
			return nil, lastOwnerDenial()
		}
		if govDenial := isGovernanceError(txErr); govDenial != nil {
			return nil, govDenial
		}
		return nil, &MembershipDecision{Allowed: false, DenialCode: "internal_error", Reason: "failed to create binding: " + txErr.Error(), HTTPStatus: 500}
	}

	svc.logger.Info("project member added via service",
		"project_id", req.ProjectID, "binding_id", result.Binding.ID,
		"role", roleDef.Name, "principal", req.PrincipalType+":"+req.PrincipalID,
		"actor", req.Actor.Email())

	return result, nil
}

// UpdateMemberRole changes a member's role with governance, delegation,
// last-owner guard, one-binding enforcement, and audit.
func (svc *ProjectMembershipService) UpdateMemberRole(ctx context.Context, req MembershipRequest) (*MembershipResult, *MembershipDecision) {
	// Fetch existing binding.
	existing, err := svc.store.GetRoleBinding(ctx, req.BindingID)
	if err != nil {
		if err == store.ErrNotFound {
			return nil, &MembershipDecision{Allowed: false, DenialCode: "not_found", Reason: "binding not found", HTTPStatus: 404}
		}
		return nil, &MembershipDecision{Allowed: false, DenialCode: "internal_error", Reason: err.Error(), HTTPStatus: 500}
	}
	if existing.ScopeType != store.RoleScopeProject || existing.ScopeID != req.ProjectID {
		return nil, &MembershipDecision{Allowed: false, DenialCode: "not_found", Reason: "binding not found in this project", HTTPStatus: 404}
	}

	// Resolve old and new role definitions.
	oldRoleDef, err := svc.store.GetRoleDefinition(ctx, existing.RoleDefinitionID)
	if err != nil {
		return nil, &MembershipDecision{Allowed: false, DenialCode: "internal_error", Reason: "cannot resolve old role", HTTPStatus: 500}
	}
	newRoleDef, err := svc.store.GetRoleDefinition(ctx, req.NewRoleDefID)
	if err != nil {
		return nil, &MembershipDecision{Allowed: false, DenialCode: "not_found", Reason: "new role definition not found", HTTPStatus: 400}
	}
	if !validProjectRoles[newRoleDef.Name] {
		return nil, &MembershipDecision{Allowed: false, DenialCode: "forbidden", Reason: "invalid project role: " + newRoleDef.Name, HTTPStatus: 400}
	}
	if newRoleDef.ScopeType != store.RoleScopeProject {
		return nil, &MembershipDecision{Allowed: false, DenialCode: "forbidden", Reason: "only project-scoped roles can be assigned", HTTPStatus: 400}
	}

	// Principal eligibility for new role.
	if !principalEligibleForRole(existing.PrincipalType, newRoleDef.Name) {
		return nil, &MembershipDecision{
			Allowed:    false,
			DenialCode: ErrCodePrincipalIneligible,
			Reason:     fmt.Sprintf("role %q cannot be assigned to %s principals", newRoleDef.Name, existing.PrincipalType),
			HTTPStatus: 400,
		}
	}

	// Governance: check both old and new target roles.
	decision := svc.checkGovernance(ctx, req, oldRoleDef.Name)
	if !decision.Allowed {
		return nil, &decision
	}
	if oldRoleDef.Name != newRoleDef.Name {
		decision = svc.checkGovernance(ctx, req, newRoleDef.Name)
		if !decision.Allowed {
			return nil, &decision
		}
	}

	// Delegation: conditional-on-increase. Check only when authority increases.
	if projectRoleLevel(newRoleDef.Name) > projectRoleLevel(oldRoleDef.Name) {
		if svc.authz != nil {
			delDecision := svc.authz.CanDelegate(ctx, req.Actor, GrantDescriptor{
				Type:             GrantTypeRoleBinding,
				RoleDefinitionID: req.NewRoleDefID,
				ScopeType:        store.RoleScopeProject,
				ScopeID:          req.ProjectID,
			})
			if !delDecision.Allowed {
				return nil, &MembershipDecision{
					Allowed:    false,
					DenialCode: ErrCodeTargetRoleProtected,
					Reason:     "actor cannot delegate the new role: " + delDecision.Reason,
					HTTPStatus: 403,
				}
			}
		}
	}

	// Pre-compute hub authority override before the transaction.
	preComputedHubOverride := svc.actorHasHubRoleBindingAuthority(ctx, req.Actor.ID(), req.Op)

	// Atomic replacement inside a transaction: create new binding, delete old
	// binding, enforce last-owner guard, and write audit record.
	// R3-1: acquire project lock to prevent write-skew under PostgreSQL.
	// R2-R1: the last-owner check is inside the transaction to prevent TOCTOU.
	// R4 O-1: re-evaluate actor authority and re-fetch target inside the lock.
	var created *store.RoleBinding
	txErr := svc.store.WithTx(ctx, func(tx store.Store) error {
		// Acquire project-scoped membership lock.
		if err := tx.LockProjectForMembership(ctx, req.ProjectID); err != nil {
			return fmt.Errorf("lock project: %w", err)
		}

		// R4 O-1: re-evaluate actor authority under lock.
		// R5 O-1: propagate store errors as 500 instead of masking as 403.
		actorRole, roleErr := svc.projectEffectiveRoleFromStore(ctx, tx, req.Actor.ID(), req.ProjectID)
		if roleErr != nil {
			return fmt.Errorf("authority lookup failed under lock: %w", roleErr)
		}
		hubOverride := false
		if actorRole == "" {
			if preComputedHubOverride {
				hubOverride = true
			} else {
				return fmt.Errorf("governance:%d:%s", 403, "actor has no project role (re-evaluated under lock)")
			}
		}

		// Re-fetch the target binding under lock to detect concurrent removal.
		existing, err = tx.GetRoleBinding(ctx, req.BindingID)
		if err != nil {
			if err == store.ErrNotFound {
				return fmt.Errorf("governance:%d:%s", 404, "binding not found (re-fetched under lock)")
			}
			return fmt.Errorf("re-fetch binding: %w", err)
		}
		// Re-resolve old role under lock.
		oldRoleDef, err = tx.GetRoleDefinition(ctx, existing.RoleDefinitionID)
		if err != nil {
			return fmt.Errorf("resolve old role under lock: %w", err)
		}

		// Re-validate governance under lock with fresh actor role.
		// Hub-override actors bypass project governance matrix.
		if !hubOverride {
			actorIsDirectOwner, ownerErr := svc.isActorDirectOwnerFromStore(ctx, tx, req.Actor.ID(), req.ProjectID)
			if ownerErr != nil {
				return fmt.Errorf("owner lookup failed under lock: %w", ownerErr)
			}
			if !svc.isOperationPermitted(actorRole, req.Op, oldRoleDef.Name) {
				return fmt.Errorf("governance:%d:%s", 403, fmt.Sprintf("actor role %q cannot %s target role %q (re-evaluated under lock)", actorRole, req.Op, oldRoleDef.Name))
			}
			if oldRoleDef.Name != newRoleDef.Name {
				if !svc.isOperationPermitted(actorRole, req.Op, newRoleDef.Name) {
					return fmt.Errorf("governance:%d:%s", 403, fmt.Sprintf("actor role %q cannot %s target role %q (re-evaluated under lock)", actorRole, req.Op, newRoleDef.Name))
				}
			}
			if requiresDirectOwner(oldRoleDef.Name) && !actorIsDirectOwner {
				return fmt.Errorf("governance:%d:%s", 403, "only direct project owners can manage admin and owner roles (re-evaluated under lock)")
			}
			if oldRoleDef.Name != newRoleDef.Name && requiresDirectOwner(newRoleDef.Name) && !actorIsDirectOwner {
				return fmt.Errorf("governance:%d:%s", 403, "only direct project owners can manage admin and owner roles (re-evaluated under lock)")
			}
		}

		// Last-owner guard (inside tx for serialization).
		if oldRoleDef.Name == store.ProjectRoleOwner && newRoleDef.Name != store.ProjectRoleOwner {
			if existing.PrincipalType == store.RoleBindingPrincipalUser {
				if err := svc.enforceLastOwnerTx(ctx, tx, req.ProjectID); err != nil {
					return err
				}
			}
		}

		// Delete old binding before creating replacement to satisfy the D4
		// partial unique index (one binding per principal per project).
		if err := tx.DeleteRoleBinding(ctx, req.BindingID); err != nil {
			return fmt.Errorf("delete old binding: %w", err)
		}
		created, err = tx.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: req.NewRoleDefID,
			PrincipalType:    existing.PrincipalType,
			PrincipalID:      existing.PrincipalID,
			ScopeType:        store.RoleScopeProject,
			ScopeID:          req.ProjectID,
			NotBefore:        existing.NotBefore,
			ExpiresAt:        existing.ExpiresAt,
			CreatedBy:        req.Actor.ID(),
		})
		if err != nil {
			return fmt.Errorf("create replacement binding: %w", err)
		}
		return svc.createAuditRecord(ctx, tx, &store.MutationAuditRecord{
			MutationType: "project_member_role_change",
			TargetType:   "project_membership",
			TargetID:     req.ProjectID,
			BeforeSummary: marshalAuditJSON(map[string]string{
				"principalId": existing.PrincipalID,
				"role":        oldRoleDef.Name,
			}),
			AfterSummary: marshalAuditJSON(map[string]string{
				"principalId": existing.PrincipalID,
				"role":        newRoleDef.Name,
			}),
		})
	})
	if txErr != nil {
		if isLastOwnerError(txErr) {
			return nil, lastOwnerDenial()
		}
		if govDenial := isGovernanceError(txErr); govDenial != nil {
			return nil, govDenial
		}
		return nil, &MembershipDecision{Allowed: false, DenialCode: "internal_error", Reason: "role change failed: " + txErr.Error(), HTTPStatus: 500}
	}

	svc.logger.Info("project member role changed via service",
		"project_id", req.ProjectID, "old_role", oldRoleDef.Name,
		"new_role", newRoleDef.Name, "principal", existing.PrincipalType+":"+existing.PrincipalID,
		"actor", req.Actor.Email())

	return &MembershipResult{
		Binding: created,
		OldRole: oldRoleDef.Name,
		NewRole: newRoleDef.Name,
		Op:      MembershipOpUpdate,
	}, nil
}

// RemoveMember removes a project member with governance, last-owner guard,
// and audit.
func (svc *ProjectMembershipService) RemoveMember(ctx context.Context, req MembershipRequest) (*MembershipResult, *MembershipDecision) {
	// Fetch binding.
	binding, err := svc.store.GetRoleBinding(ctx, req.BindingID)
	if err != nil {
		if err == store.ErrNotFound {
			return nil, &MembershipDecision{Allowed: false, DenialCode: "not_found", Reason: "binding not found", HTTPStatus: 404}
		}
		return nil, &MembershipDecision{Allowed: false, DenialCode: "internal_error", Reason: err.Error(), HTTPStatus: 500}
	}
	if binding.ScopeType != store.RoleScopeProject || binding.ScopeID != req.ProjectID {
		return nil, &MembershipDecision{Allowed: false, DenialCode: "not_found", Reason: "binding not found in this project", HTTPStatus: 404}
	}

	// Resolve target role.
	roleDef, err := svc.store.GetRoleDefinition(ctx, binding.RoleDefinitionID)
	if err != nil {
		return nil, &MembershipDecision{Allowed: false, DenialCode: "internal_error", Reason: "cannot resolve role", HTTPStatus: 500}
	}

	// Governance check.
	decision := svc.checkGovernance(ctx, req, roleDef.Name)
	if !decision.Allowed {
		return nil, &decision
	}

	// Pre-compute hub authority override before the transaction.
	preComputedHubOverride := svc.actorHasHubRoleBindingAuthority(ctx, req.Actor.ID(), req.Op)

	// Delete the binding, enforce last-owner guard, and write audit inside a
	// transaction. R3-1: acquire project lock to prevent write-skew.
	// R2-R1: the last-owner check MUST be inside the transaction.
	// R4 O-1: re-evaluate actor authority and re-fetch target inside the lock.
	txErr := svc.store.WithTx(ctx, func(tx store.Store) error {
		// Acquire project-scoped membership lock.
		if err := tx.LockProjectForMembership(ctx, req.ProjectID); err != nil {
			return fmt.Errorf("lock project: %w", err)
		}

		// R4 O-1: re-evaluate actor authority under lock.
		// R5 O-1: propagate store errors as 500 instead of masking as 403.
		actorRole, roleErr := svc.projectEffectiveRoleFromStore(ctx, tx, req.Actor.ID(), req.ProjectID)
		if roleErr != nil {
			return fmt.Errorf("authority lookup failed under lock: %w", roleErr)
		}
		hubOverride := false
		if actorRole == "" {
			// Hub-level authority pre-computed before the transaction to
			// avoid SQLite deadlock; system-scoped bindings are stable
			// across the project lock.
			if preComputedHubOverride {
				hubOverride = true
			} else {
				return fmt.Errorf("governance:%d:%s", 403, "actor has no project role (re-evaluated under lock)")
			}
		}

		// Re-fetch the target binding under lock.
		binding, err = tx.GetRoleBinding(ctx, req.BindingID)
		if err != nil {
			if err == store.ErrNotFound {
				return fmt.Errorf("governance:%d:%s", 404, "binding not found (re-fetched under lock)")
			}
			return fmt.Errorf("re-fetch binding: %w", err)
		}
		// Re-resolve role under lock.
		roleDef, err = tx.GetRoleDefinition(ctx, binding.RoleDefinitionID)
		if err != nil {
			return fmt.Errorf("resolve role under lock: %w", err)
		}

		// Re-validate governance under lock.
		// Hub-override actors bypass project governance matrix.
		if !hubOverride {
			if !svc.isOperationPermitted(actorRole, req.Op, roleDef.Name) {
				return fmt.Errorf("governance:%d:%s", 403, fmt.Sprintf("actor role %q cannot %s target role %q (re-evaluated under lock)", actorRole, req.Op, roleDef.Name))
			}
			if requiresDirectOwner(roleDef.Name) {
				actorIsDirectOwner, ownerErr := svc.isActorDirectOwnerFromStore(ctx, tx, req.Actor.ID(), req.ProjectID)
				if ownerErr != nil {
					return fmt.Errorf("owner lookup failed under lock: %w", ownerErr)
				}
				if !actorIsDirectOwner {
					return fmt.Errorf("governance:%d:%s", 403, "only direct project owners can manage admin and owner roles (re-evaluated under lock)")
				}
			}
		}

		// Last-owner guard (inside tx for serialization).
		if roleDef.Name == store.ProjectRoleOwner && binding.PrincipalType == store.RoleBindingPrincipalUser {
			if err := svc.enforceLastOwnerTx(ctx, tx, req.ProjectID); err != nil {
				return err
			}
		}

		if err := tx.DeleteRoleBinding(ctx, req.BindingID); err != nil {
			return err
		}
		return svc.createAuditRecord(ctx, tx, &store.MutationAuditRecord{
			MutationType: "project_member_remove",
			TargetType:   "project_membership",
			TargetID:     req.ProjectID,
			BeforeSummary: marshalAuditJSON(map[string]string{
				"principalId": binding.PrincipalID,
				"role":        roleDef.Name,
			}),
		})
	})
	if txErr != nil {
		if isLastOwnerError(txErr) {
			return nil, lastOwnerDenial()
		}
		if govDenial := isGovernanceError(txErr); govDenial != nil {
			return nil, govDenial
		}
		if txErr == store.ErrNotFound {
			return nil, &MembershipDecision{Allowed: false, DenialCode: "not_found", Reason: "binding not found", HTTPStatus: 404}
		}
		return nil, &MembershipDecision{Allowed: false, DenialCode: "internal_error", Reason: txErr.Error(), HTTPStatus: 500}
	}

	svc.logger.Info("project member removed via service",
		"project_id", req.ProjectID, "binding_id", req.BindingID,
		"role", roleDef.Name, "principal", binding.PrincipalType+":"+binding.PrincipalID,
		"actor", req.Actor.Email())

	return &MembershipResult{
		Binding: binding,
		OldRole: roleDef.Name,
		Op:      MembershipOpRemove,
		Removed: binding,
	}, nil
}

// TransferOwnership atomically transfers project ownership from one user to
// another. The new owner gets a project-owner binding; the current actor's
// owner binding is downgraded to project-member. This is one atomic operation
// with post-state owner invariant verification per CT1 D1.
func (svc *ProjectMembershipService) TransferOwnership(ctx context.Context, req MembershipRequest) (*MembershipResult, *MembershipDecision) {
	if req.NewOwnerID == "" {
		return nil, &MembershipDecision{Allowed: false, DenialCode: "forbidden", Reason: "newOwnerId is required", HTTPStatus: 400}
	}

	// Actor must be a direct project owner.
	if !svc.isActorDirectOwner(ctx, req.Actor.ID(), req.ProjectID) {
		return nil, &MembershipDecision{
			Allowed:    false,
			DenialCode: ErrCodeRoleAssignmentForbidden,
			Reason:     "only direct project owners can transfer ownership",
			HTTPStatus: 403,
		}
	}

	// Self-transfer is a no-op.
	if req.NewOwnerID == req.Actor.ID() {
		return nil, &MembershipDecision{Allowed: false, DenialCode: "conflict", Reason: "cannot transfer ownership to yourself", HTTPStatus: 409}
	}

	// Verify new owner is a valid user.
	newOwner, err := svc.store.GetUser(ctx, req.NewOwnerID)
	if err != nil {
		return nil, &MembershipDecision{Allowed: false, DenialCode: "not_found", Reason: "target user not found", HTTPStatus: 400}
	}
	if newOwner.Status != "active" {
		return nil, &MembershipDecision{Allowed: false, DenialCode: ErrCodePrincipalIneligible, Reason: "target user is not active", HTTPStatus: 400}
	}

	// Resolve role definitions.
	ownerRoleDef, err := svc.store.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	if err != nil {
		return nil, &MembershipDecision{Allowed: false, DenialCode: "internal_error", Reason: "cannot resolve owner role", HTTPStatus: 500}
	}
	memberRoleDef, err := svc.store.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	if err != nil {
		return nil, &MembershipDecision{Allowed: false, DenialCode: "internal_error", Reason: "cannot resolve member role", HTTPStatus: 500}
	}

	// All transfer mutations, post-state invariant, and audit happen in one
	// transaction. If anything fails, the entire transfer is rolled back.
	// R4 O-1: re-verify actor is still a direct owner after acquiring the lock.
	var newOwnerBinding, oldActorBinding *store.RoleBinding
	txErr := svc.store.WithTx(ctx, func(tx store.Store) error {
		// R3-1: acquire project-scoped membership lock.
		if err := tx.LockProjectForMembership(ctx, req.ProjectID); err != nil {
			return fmt.Errorf("lock project: %w", err)
		}

		// R4 O-1: re-verify actor is still a direct owner under lock.
		// R5 O-1: propagate store errors as 500 instead of masking as 403.
		stillOwner, ownerErr := svc.isActorDirectOwnerFromStore(ctx, tx, req.Actor.ID(), req.ProjectID)
		if ownerErr != nil {
			return fmt.Errorf("owner lookup failed under lock: %w", ownerErr)
		}
		if !stillOwner {
			return fmt.Errorf("governance:%d:%s", 403, "actor is no longer a direct project owner (re-evaluated under lock)")
		}

		// Step 1: Give the new owner a project-owner binding (or replace existing).
		existingNewOwnerBindings, findErr := svc.findExistingDirectBindingsFromStore(ctx, tx, store.RoleBindingPrincipalUser, req.NewOwnerID, req.ProjectID)
		if findErr != nil {
			return fmt.Errorf("find new owner bindings: %w", findErr)
		}
		if len(existingNewOwnerBindings) > 0 {
			// Replace existing binding(s) with owner role — delete all, create one.
			var rErr error
			newOwnerBinding, rErr = svc.replaceBindingTx(ctx, tx, existingNewOwnerBindings, ownerRoleDef.ID, req.Actor.ID())
			if rErr != nil {
				return fmt.Errorf("promote new owner: %w", rErr)
			}
		} else {
			var cErr error
			newOwnerBinding, cErr = tx.CreateRoleBinding(ctx, &store.RoleBinding{
				RoleDefinitionID: ownerRoleDef.ID,
				PrincipalType:    store.RoleBindingPrincipalUser,
				PrincipalID:      req.NewOwnerID,
				ScopeType:        store.RoleScopeProject,
				ScopeID:          req.ProjectID,
				CreatedBy:        req.Actor.ID(),
			})
			if cErr != nil {
				return fmt.Errorf("create owner binding: %w", cErr)
			}
		}

		// Step 2: Downgrade the actor's owner binding to member.
		actorBindings, findErr := svc.findExistingDirectBindingsFromStore(ctx, tx, store.RoleBindingPrincipalUser, req.Actor.ID(), req.ProjectID)
		if findErr != nil {
			return fmt.Errorf("find actor bindings: %w", findErr)
		}
		if len(actorBindings) > 0 {
			var rErr error
			oldActorBinding, rErr = svc.replaceBindingTx(ctx, tx, actorBindings, memberRoleDef.ID, req.Actor.ID())
			if rErr != nil {
				return fmt.Errorf("downgrade old owner: %w", rErr)
			}
		}

		// Post-state invariant: verify at least one active direct owner exists.
		// This query runs inside the transaction so it sees the committed state.
		ownerCount, countErr := svc.countActiveDirectOwnersFromStore(ctx, tx, req.ProjectID)
		if countErr != nil {
			return fmt.Errorf("post-state owner count: %w", countErr)
		}
		if ownerCount == 0 {
			return fmt.Errorf("post-state invariant violation: zero active direct owners after transfer")
		}

		// Audit record inside the same transaction.
		return svc.createAuditRecord(ctx, tx, &store.MutationAuditRecord{
			MutationType: "project_ownership_transfer",
			TargetType:   "project_membership",
			TargetID:     req.ProjectID,
			BeforeSummary: marshalAuditJSON(map[string]string{
				"oldOwnerId": req.Actor.ID(),
			}),
			AfterSummary: marshalAuditJSON(map[string]string{
				"newOwnerId":   req.NewOwnerID,
				"oldOwnerRole": store.ProjectRoleMember,
				"newOwnerRole": store.ProjectRoleOwner,
			}),
		})
	})
	if txErr != nil {
		if govDenial := isGovernanceError(txErr); govDenial != nil {
			return nil, govDenial
		}
		return nil, &MembershipDecision{Allowed: false, DenialCode: "internal_error", Reason: "ownership transfer failed: " + txErr.Error(), HTTPStatus: 500}
	}

	svc.logger.Info("project ownership transferred via service",
		"project_id", req.ProjectID,
		"old_owner", req.Actor.ID(),
		"new_owner", req.NewOwnerID,
		"actor", req.Actor.Email())

	return &MembershipResult{
		Binding:                 newOwnerBinding,
		OldRole:                 store.ProjectRoleOwner,
		NewRole:                 store.ProjectRoleOwner,
		Op:                      MembershipOpTransfer,
		TransferOldOwnerBinding: oldActorBinding,
	}, nil
}

// ---------------------------------------------------------------------------
// Capabilities — server-derived operation/target capabilities
// ---------------------------------------------------------------------------

// MembershipCapabilities describes what actions the actor can perform on
// project membership. This replaces the C0 owner-only advisory capability.
type MembershipCapabilities struct {
	CanManageMembers bool     `json:"canManageMembers"`
	CanManageAdmins  bool     `json:"canManageAdmins"`
	CanManageOwners  bool     `json:"canManageOwners"`
	CanTransfer      bool     `json:"canTransfer"`
	Actions          []string `json:"actions"` // backward compat
}

// ComputeCapabilities returns the membership capabilities for the given actor.
func (svc *ProjectMembershipService) ComputeCapabilities(ctx context.Context, userID, projectID string) *MembershipCapabilities {
	caps := &MembershipCapabilities{Actions: []string{}}

	effectiveRole := svc.projectEffectiveRole(ctx, userID, projectID)
	switch effectiveRole {
	case store.ProjectRoleOwner:
		isDirectOwner := svc.isActorDirectOwner(ctx, userID, projectID)
		if isDirectOwner {
			caps.CanManageMembers = true
			caps.CanManageAdmins = true
			caps.CanManageOwners = true
			caps.CanTransfer = true
			caps.Actions = []string{
				"manage_members", "manage_admins", "manage_owners", "transfer_ownership",
			}
		} else {
			// D3 guarantees groups cannot hold project-owner, so effective
			// owner without direct binding should not happen. Log a
			// fail-closed detection warning and grant admin-level caps.
			svc.logger.Warn("D3 violation detection: effective owner without direct binding",
				"user_id", userID, "project_id", projectID)
			caps.CanManageMembers = true
			caps.Actions = []string{"manage_members"}
		}
	case store.ProjectRoleAdmin:
		caps.CanManageMembers = true
		caps.Actions = []string{"manage_members"}
	default:
		// Member or no role: read-only.
	}

	return caps
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// findExistingDirectBindingsFromStore is like findExistingDirectBindings but
// uses the provided store (which may be a transactional store).
// O-1: errors are propagated, not silently swallowed.
func (svc *ProjectMembershipService) findExistingDirectBindingsFromStore(ctx context.Context, s store.Store, principalType, principalID, projectID string) ([]*store.RoleBinding, error) {
	bindings, err := s.ListRoleBindingsForPrincipal(ctx, principalType, principalID)
	if err != nil {
		return nil, fmt.Errorf("list bindings for principal %s/%s: %w", principalType, principalID, err)
	}
	var result []*store.RoleBinding
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeProject && b.ScopeID == projectID {
			result = append(result, b)
		}
	}
	return result, nil
}

// replaceBindingTx creates a new binding with the given role and deletes all
// old bindings inside the provided transactional store. All errors are
// propagated — no silent swallowing (R-6).
// O-2: uses the highest-authority binding's lifecycle metadata (NotBefore/ExpiresAt),
// not an arbitrary old[0].
func (svc *ProjectMembershipService) replaceBindingTx(ctx context.Context, tx store.Store, old []*store.RoleBinding, newRoleDefID, createdBy string) (*store.RoleBinding, error) {
	if len(old) == 0 {
		return nil, fmt.Errorf("replaceBindingTx: no bindings to replace")
	}
	// Use the highest-authority binding's metadata for the replacement.
	// Must use tx (not svc.store) to avoid SQLite deadlock inside a transaction.
	primary := svc.highestAuthorityBindingFromStore(ctx, tx, old)
	if primary == nil {
		primary = old[0]
	}
	// Delete all old bindings first to satisfy the D4 partial unique index
	// (one binding per principal per project). Must delete before creating
	// the replacement to avoid a constraint violation.
	for _, b := range old {
		if err := tx.DeleteRoleBinding(ctx, b.ID); err != nil {
			return nil, fmt.Errorf("delete old binding %s: %w", b.ID, err)
		}
	}

	created, err := tx.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: newRoleDefID,
		PrincipalType:    primary.PrincipalType,
		PrincipalID:      primary.PrincipalID,
		ScopeType:        primary.ScopeType,
		ScopeID:          primary.ScopeID,
		NotBefore:        primary.NotBefore,
		ExpiresAt:        primary.ExpiresAt,
		CreatedBy:        createdBy,
	})
	if err != nil {
		return nil, fmt.Errorf("create replacement: %w", err)
	}

	return created, nil
}

// highestAuthorityBindingFromStore is like highestAuthorityBinding but reads
// role definitions from the provided store (which may be a transactional
// store). This variant must be used when called from inside a WithTx
// callback to avoid SQLite deadlocks.
func (svc *ProjectMembershipService) highestAuthorityBindingFromStore(ctx context.Context, s store.Store, bindings []*store.RoleBinding) *store.RoleBinding {
	if len(bindings) <= 1 {
		if len(bindings) == 1 {
			return bindings[0]
		}
		return nil
	}
	var best *store.RoleBinding
	bestLevel := -1
	for _, b := range bindings {
		rd, err := s.GetRoleDefinition(ctx, b.RoleDefinitionID)
		if err != nil {
			continue
		}
		level := projectRoleLevel(rd.Name)
		if level > bestLevel {
			bestLevel = level
			best = b
		}
	}
	return best
}

// enforceLastOwnerTx checks that at least two active direct owners exist,
// reading from the provided (transactional) store. Returns an error if the
// invariant would be violated so the surrounding transaction rolls back.
//
// R2-R1: This MUST be called inside WithTx, not outside it. Reading owner
// count outside a transaction creates a TOCTOU race where two concurrent
// demotions both observe count=2 and both commit, leaving zero owners.
func (svc *ProjectMembershipService) enforceLastOwnerTx(ctx context.Context, tx store.Store, projectID string) error {
	count, err := svc.countActiveDirectOwnersFromStore(ctx, tx, projectID)
	if err != nil {
		return fmt.Errorf("cannot verify owner count: %w", err)
	}
	if count <= 1 {
		return &lastOwnerError{projectID: projectID}
	}
	return nil
}

// lastOwnerError is returned by enforceLastOwnerTx when the last-owner
// invariant would be violated. Callers can type-assert to produce the
// appropriate denial response.
type lastOwnerError struct {
	projectID string
}

func (e *lastOwnerError) Error() string {
	return "cannot remove or demote the last project owner — at least one active direct user owner must remain"
}

// isLastOwnerError returns true if the error is a last-owner invariant violation.
func isLastOwnerError(err error) bool {
	_, ok := err.(*lastOwnerError)
	return ok
}

// isGovernanceError checks whether a transaction error is a governance denial
// produced by the in-tx re-evaluation helpers. Returns a MembershipDecision
// if so.
func isGovernanceError(err error) *MembershipDecision {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "governance:") {
		return nil
	}
	// Format: "governance:STATUS:REASON"
	parts := strings.SplitN(msg, ":", 3)
	if len(parts) < 3 {
		return nil
	}
	status := 403
	if _, scanErr := fmt.Sscanf(parts[1], "%d", &status); scanErr != nil {
		status = 403
	}
	code := ErrCodeRoleAssignmentForbidden
	if status == 404 {
		code = "not_found"
	}
	return &MembershipDecision{
		Allowed:    false,
		DenialCode: code,
		Reason:     parts[2],
		HTTPStatus: status,
	}
}

// lastOwnerDenial converts a lastOwnerError into a MembershipDecision.
func lastOwnerDenial() *MembershipDecision {
	return &MembershipDecision{
		Allowed:    false,
		DenialCode: ErrCodeLastOwner,
		Reason:     "cannot remove or demote the last project owner — at least one active direct user owner must remain",
		HTTPStatus: 409,
	}
}

// countActiveDirectOwners counts active direct-user project-owner bindings.
// Uses the same activation semantics as isProjectOwner per CT1 D2.
func (svc *ProjectMembershipService) countActiveDirectOwners(ctx context.Context, projectID string) (int, error) {
	return svc.countActiveDirectOwnersFromStore(ctx, svc.store, projectID)
}

// countActiveDirectOwnersFromStore is like countActiveDirectOwners but uses
// the provided store (which may be a transactional store).
func (svc *ProjectMembershipService) countActiveDirectOwnersFromStore(ctx context.Context, s store.Store, projectID string) (int, error) {
	bindings, err := s.ListRoleBindingsForScope(ctx, store.RoleScopeProject, projectID)
	if err != nil {
		return 0, err
	}
	ownerRoleDef, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	if err != nil {
		return 0, err
	}
	now := svc.nowFunc()
	count := 0
	for _, b := range bindings {
		if b.PrincipalType != store.RoleBindingPrincipalUser || b.RoleDefinitionID != ownerRoleDef.ID {
			continue
		}
		if !isBindingActive(b, now) {
			continue
		}
		count++
	}
	return count, nil
}

// higherProjectRole returns the higher-authority project role of the two.
func higherProjectRole(a, b string) string {
	if projectRoleLevel(a) >= projectRoleLevel(b) {
		return a
	}
	return b
}

// projectRoleLevel returns the authority level of a project role.
// Higher values mean more authority.
func projectRoleLevel(role string) int {
	switch role {
	case store.ProjectRoleOwner:
		return 3
	case store.ProjectRoleAdmin:
		return 2
	case store.ProjectRoleMember:
		return 1
	default:
		return 0
	}
}

// marshalAuditJSON marshals a map to JSON for audit records. Falls back to
// fmt.Sprint on error.
func marshalAuditJSON(m map[string]string) string {
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Sprint(m)
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// R-3: Migration/backfill for existing multi-role principals
// ---------------------------------------------------------------------------

// MultiRoleMigrationResult captures the outcome of the migration.
type MultiRoleMigrationResult struct {
	ProjectID    string
	PrincipalID  string
	KeptRole     string
	DeletedCount int
	// NonComparable is set when the bindings include custom/non-comparable
	// roles that cannot be deterministically ordered.
	NonComparable bool
	Error         error
}

// MigrateMultiRoleBindings scans all project-scoped bindings and consolidates
// any principal with more than one direct binding per project. For built-in
// roles, the highest-authority binding is kept (using projectRoleLevel). For
// custom or non-comparable roles, the migration fails closed (returns error)
// to prevent data loss on unexpected role configurations.
//
// This is idempotent: re-running on a clean database is a no-op.
// R2-R2: paginates through all projects; no silent cap.
func (svc *ProjectMembershipService) MigrateMultiRoleBindings(ctx context.Context) ([]MultiRoleMigrationResult, error) {
	// Paginate through all projects (R2-R2: no silent 10k cap).
	const pageSize = 500
	var allProjects []store.Project
	cursor := ""
	for {
		projectResult, err := svc.store.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{
			Limit:  pageSize,
			Cursor: cursor,
		})
		if err != nil {
			return nil, fmt.Errorf("listing projects (cursor=%s): %w", cursor, err)
		}
		allProjects = append(allProjects, projectResult.Items...)
		if projectResult.NextCursor == "" || len(projectResult.Items) < pageSize {
			break
		}
		cursor = projectResult.NextCursor
	}

	svc.logger.Info("migration: scanning projects for multi-role bindings",
		"project_count", len(allProjects))

	var results []MultiRoleMigrationResult

	for _, p := range allProjects {
		bindings, err := svc.store.ListRoleBindingsForScope(ctx, store.RoleScopeProject, p.ID)
		if err != nil {
			results = append(results, MultiRoleMigrationResult{
				ProjectID: p.ID, Error: err,
			})
			continue
		}

		// Group bindings by principal key (type:id).
		type principalKey struct {
			Type string
			ID   string
		}
		grouped := make(map[principalKey][]*store.RoleBinding)
		for _, b := range bindings {
			key := principalKey{Type: b.PrincipalType, ID: b.PrincipalID}
			grouped[key] = append(grouped[key], b)
		}

		for key, pBindings := range grouped {
			if len(pBindings) <= 1 {
				continue // no duplicate
			}

			// Resolve all roles and check for non-comparable.
			type bindingRole struct {
				binding *store.RoleBinding
				role    string
				level   int
			}
			var brs []bindingRole
			hasNonComparable := false
			for _, b := range pBindings {
				rd, rdErr := svc.store.GetRoleDefinition(ctx, b.RoleDefinitionID)
				if rdErr != nil {
					hasNonComparable = true
					continue
				}
				level := projectRoleLevel(rd.Name)
				if level == 0 && !validProjectRoles[rd.Name] {
					// Custom role — cannot compare to built-in roles.
					hasNonComparable = true
				}
				brs = append(brs, bindingRole{binding: b, role: rd.Name, level: level})
			}

			if hasNonComparable {
				svc.logger.Warn("migration: skipping principal with non-comparable roles",
					"project_id", p.ID, "principal", key.Type+":"+key.ID,
					"binding_count", len(pBindings))
				results = append(results, MultiRoleMigrationResult{
					ProjectID:     p.ID,
					PrincipalID:   key.ID,
					NonComparable: true,
					DeletedCount:  0,
				})
				continue
			}

			// Find the highest-authority binding.
			best := brs[0]
			for _, br := range brs[1:] {
				if br.level > best.level {
					best = br
				}
			}

			// Delete all except the best, inside a transaction.
			var deleted int
			txErr := svc.store.WithTx(ctx, func(tx store.Store) error {
				for _, br := range brs {
					if br.binding.ID == best.binding.ID {
						continue
					}
					if err := tx.DeleteRoleBinding(ctx, br.binding.ID); err != nil {
						return fmt.Errorf("delete binding %s: %w", br.binding.ID, err)
					}
					deleted++
				}
				return nil
			})

			result := MultiRoleMigrationResult{
				ProjectID:    p.ID,
				PrincipalID:  key.ID,
				KeptRole:     best.role,
				DeletedCount: deleted,
				Error:        txErr,
			}
			results = append(results, result)

			if txErr == nil {
				svc.logger.Info("migration: consolidated multi-role bindings",
					"project_id", p.ID, "principal", key.Type+":"+key.ID,
					"kept_role", best.role, "deleted", deleted)
			} else {
				svc.logger.Error("migration: failed to consolidate",
					"project_id", p.ID, "principal", key.Type+":"+key.ID,
					"error", txErr)
			}
		}
	}

	return results, nil
}
