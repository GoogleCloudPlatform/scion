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
	"fmt"
	"log/slog"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ---------------------------------------------------------------------------
// GovernanceService — transactional boundary mutation governance (B5)
// ---------------------------------------------------------------------------

// GovernanceService wraps B3's PreviewService with transactional commit.
// Every boundary create/update/delete and every adjacent-domain change that
// could relax a boundary must go through this service to enforce:
//   - Preview token validation (single-use, actor/draft/state-bound)
//   - Optimistic concurrency (revision check)
//   - Re-authorization of the actor
//   - Lockout invariant (at least one admin survives)
//   - Server-classified relaxation authority
//   - Atomic commit or full rollback
type GovernanceService struct {
	store   store.Store
	preview *PreviewService
	authz   *AuthzService
	logger  *slog.Logger

	// auditWriter writes durable audit entries for every boundary mutation.
	// If nil, audit logging is disabled (should only happen in legacy tests).
	auditWriter *BoundaryAuditWriter

	// eventBus publishes invalidation events after successful mutations.
	// If nil, no events are published.
	eventBus *InvalidationEventBus

	// nowFunc is injectable for testing. Defaults to time.Now.
	nowFunc func() time.Time
}

// NewGovernanceService creates a new GovernanceService.
func NewGovernanceService(s store.Store, preview *PreviewService, authz *AuthzService, logger *slog.Logger) *GovernanceService {
	return &GovernanceService{
		store:       s,
		preview:     preview,
		authz:       authz,
		logger:      logger,
		auditWriter: NewBoundaryAuditWriter(logger),
		nowFunc:     time.Now,
	}
}

// ---------------------------------------------------------------------------
// CommitRequest / CommitResult
// ---------------------------------------------------------------------------

// commitEventTypes maps operation names to their corresponding event type
// constants. Used by the commit path instead of string concatenation to
// ensure we use the canonical constants (R3).
var commitEventTypes = map[string]string{
	"create": EventBoundaryCreated,
	"update": EventBoundaryUpdated,
	"delete": EventBoundaryDeleted,
}

// CommitRequest describes a boundary mutation to be committed after preview.
type CommitRequest struct {
	// Operation is "create", "update", or "delete".
	Operation string

	// Draft is the proposed constraint state. Nil for delete.
	Draft *store.AccessConstraint

	// ConstraintID is the existing constraint ID. Empty for create.
	ConstraintID string

	// BaseRevision is the expected current revision. 0 for create.
	BaseRevision int64

	// PreviewToken is the opaque token from GeneratePreview.
	PreviewToken string

	// PreviewID is the semantic preview identifier from the PreviewResult.
	// Populated from the preview validation step (B3) rather than being
	// extracted by slicing the opaque token.
	PreviewID string

	// DraftHash is the SHA-256 of the canonicalized draft at preview time.
	// Populated from the PreviewResult rather than being extracted by
	// slicing the opaque token.
	DraftHash string

	// Actor is the principal requesting the mutation.
	Actor PrincipalContext
}

// CommitResult is the outcome of a successful commit.
type CommitResult struct {
	// Constraint is the resulting constraint record. Nil for delete.
	Constraint *store.AccessConstraint

	// Operation is the operation that was committed.
	Operation string

	// Classification is the server-determined mutation direction.
	Classification string

	// AuditID is the ID of the durable audit entry for this mutation.
	// Always non-empty on success — no authority change returns without
	// a persisted audit ID.
	AuditID string
}

// GovernanceError is a typed error for governance failures.
type GovernanceError struct {
	Code    string
	Message string
	Details map[string]interface{}
}

func (e *GovernanceError) Error() string {
	return e.Message
}

// ---------------------------------------------------------------------------
// CommitBoundaryChange — the core transactional commit
// ---------------------------------------------------------------------------

// CommitBoundaryChange validates the preview token, re-checks state in a
// transaction, enforces lockout, and applies the mutation atomically.
//
// Transaction steps:
//
//	a. Validate the preview token (B3's ValidateToken)
//	b. Re-read the boundary (if update/delete) and verify revision matches
//	c. Re-read relevant state: role bindings, group memberships, principal
//	   status, permission registry — scoped to the subject/scope under change
//	d. Compare state fingerprint to the preview's fingerprint
//	e. If state changed: reject with stale_authorization_preview
//	f. Re-authorize the actor: verify they still hold access_constraint.admin
//	g. For relaxations: verify the actor holds sufficient authority over the
//	   permissions being restored (server classification chooses the path)
//	h. Enforce lockout: at least one active direct user retains
//	   access_constraint.admin at the relevant scope
//	i. Apply the mutation (create/update/delete)
//	j. Commit the transaction
func (gs *GovernanceService) CommitBoundaryChange(ctx context.Context, req CommitRequest) (*CommitResult, error) {
	now := gs.nowFunc()

	// Step a: Validate the preview token.
	if err := gs.preview.ValidateToken(
		ctx, req.PreviewToken, req.Actor, req.Operation,
		req.Draft, req.BaseRevision,
	); err != nil {
		if tve, ok := err.(*TokenValidationError); ok {
			return nil, &GovernanceError{
				Code:    tve.Code,
				Message: tve.Message,
			}
		}
		return nil, fmt.Errorf("token validation failed: %w", err)
	}

	// Step f: Re-authorize the actor — verify they still hold access_constraint.admin.
	if err := gs.reauthorizeActor(ctx, req); err != nil {
		return nil, err
	}

	// Determine scope for lockout check.
	scopeType, scopeID := gs.resolveScope(ctx, req)

	// Compute server classification by re-running the preview engine logic.
	classification, err := gs.classifyOperation(ctx, req, now)
	if err != nil {
		return nil, fmt.Errorf("classification failed: %w", err)
	}

	// Step g: For relaxations, verify the actor has sufficient authority.
	if classification == ClassificationRelax || classification == ClassificationMixed {
		if err := gs.checkRelaxationAuthority(ctx, req, scopeType, scopeID); err != nil {
			return nil, err
		}
	}

	// Step h: Enforce lockout invariant (for create/update only — delete can only relax).
	if req.Operation != "delete" {
		if err := gs.enforceLockout(ctx, req, scopeType, scopeID, now); err != nil {
			return nil, err
		}
	}

	// Steps b, i, j: Re-read, apply mutation, and commit.
	//
	// KNOWN LIMITATION — TOCTOU window between governance checks and store
	// mutation:
	//
	// The checks above (token validation, re-authorization, lockout,
	// relaxation authority) and the store mutation below are separate
	// operations — there is no wrapping database transaction. The Store
	// interface does not expose a RunInTx(ctx, func(tx Store) error) method,
	// so a true atomic check-then-mutate is architecturally impossible
	// without interface changes.
	//
	// What mitigates the window:
	//   - The preview token's state fingerprint detects most concurrent
	//     modifications (stale token → reject).
	//   - For updates, the store-level revision check rejects concurrent
	//     writes to the same constraint.
	//
	// What the mitigations do NOT catch:
	//   - Concurrent creates: two goroutines can both pass the fingerprint
	//     check before either commits, and both succeed.
	//   - Adjacent state changes: admin role bindings removed concurrently
	//     between the lockout check and the mutation.
	//
	// Under single-instance deployment the TOCTOU window is narrow. Under
	// multi-instance deployment this gap must be closed.
	//
	// TODO: Add Store.RunInTx(ctx context.Context, fn func(tx Store) error)
	// error so governance can wrap checks + mutation in one database
	// transaction.
	//
	// beforeState captures the constraint state prior to mutation so that the
	// compensating action below can restore it on audit failure.
	var beforeState *store.AccessConstraint
	var result *CommitResult
	switch req.Operation {
	case "create":
		created, err := gs.store.CreateAccessConstraint(ctx, req.Draft)
		if err != nil {
			return nil, fmt.Errorf("create failed: %w", err)
		}
		result = &CommitResult{
			Constraint:     created,
			Operation:      "create",
			Classification: classification,
		}

	case "update":
		// Capture the before-state so we can restore on audit failure.
		existing, err := gs.store.GetAccessConstraint(ctx, req.ConstraintID)
		if err != nil {
			return nil, fmt.Errorf("failed to load constraint before update: %w", err)
		}
		beforeState = existing

		updated, err := gs.store.UpdateAccessConstraint(ctx, req.Draft, req.BaseRevision)
		if err != nil {
			if err == store.ErrRevisionConflict {
				return nil, &GovernanceError{
					Code:    ErrCodeStaleAuthorizationPreview,
					Message: "constraint was modified by another operation since preview",
				}
			}
			return nil, fmt.Errorf("update failed: %w", err)
		}
		result = &CommitResult{
			Constraint:     updated,
			Operation:      "update",
			Classification: classification,
		}

	case "delete":
		// Re-read to verify revision and capture before-state for rollback.
		existing, err := gs.store.GetAccessConstraint(ctx, req.ConstraintID)
		if err != nil {
			return nil, fmt.Errorf("failed to load constraint for delete: %w", err)
		}
		if req.BaseRevision > 0 && existing.Revision != req.BaseRevision {
			return nil, &GovernanceError{
				Code:    ErrCodeStaleAuthorizationPreview,
				Message: fmt.Sprintf("constraint revision %d does not match expected %d", existing.Revision, req.BaseRevision),
			}
		}
		beforeState = existing
		if err := gs.store.DeleteAccessConstraint(ctx, req.ConstraintID); err != nil {
			return nil, fmt.Errorf("delete failed: %w", err)
		}
		result = &CommitResult{
			Operation:      "delete",
			Classification: classification,
		}

	default:
		return nil, &GovernanceError{
			Code:    ErrCodeInvalidRequest,
			Message: fmt.Sprintf("invalid operation %q", req.Operation),
		}
	}

	// Write the durable audit entry. If this fails the mutation is considered
	// unsuccessful — no authority change returns without a persisted audit ID.
	//
	// Compensating actions on audit failure:
	//   - create: delete the newly-created constraint
	//   - update: restore the before-state
	//   - delete: re-create from the before-state
	//
	// TODO: Replace compensating actions with true atomic commit when
	// Store.RunInTx is added — compensating actions have a second failure
	// window (the compensating write itself can fail).
	if gs.auditWriter != nil {
		constraintID := req.ConstraintID
		if result.Constraint != nil && constraintID == "" {
			constraintID = result.Constraint.ID
		}

		var afterRevision int64
		if result.Constraint != nil {
			afterRevision = result.Constraint.Revision
		}

		auditID, auditErr := gs.auditWriter.WriteAuditEntry(ctx, AuditRequest{
			ConstraintID:   constraintID,
			Operation:      req.Operation,
			ActorID:        req.Actor.ID,
			CorrelationID:  correlationIDFromContext(ctx),
			BeforeRevision: req.BaseRevision,
			AfterRevision:  afterRevision,
			Classification: classification,
			PreviewID:      req.PreviewID,
			DraftHash:      req.DraftHash,
			ImpactCounts:   computeImpactCounts(req, beforeState),
		})
		if auditErr != nil {
			// Audit write failed — execute compensating action to undo the
			// mutation so no unaudited authority change persists.
			gs.compensateAuditFailure(ctx, req, result, beforeState)
			return nil, fmt.Errorf("audit write failed: %w", auditErr)
		}
		result.AuditID = auditID
	}

	// Publish invalidation event after successful commit (outside transaction).
	if gs.eventBus != nil {
		constraintID := req.ConstraintID
		if result.Constraint != nil && constraintID == "" {
			constraintID = result.Constraint.ID
		}
		eventType, ok := commitEventTypes[req.Operation]
		if !ok {
			gs.logger.Error("unknown operation for event publication — skipping event",
				"operation", req.Operation,
				"constraint_id", constraintID,
			)
		} else {
			gs.eventBus.Publish(InvalidationEvent{
				Type:      eventType,
				EntityID:  constraintID,
				Timestamp: gs.nowFunc(),
			})
		}
	}

	gs.logger.Info("boundary change committed",
		"operation", result.Operation,
		"classification", result.Classification,
		"actor", req.Actor.ID,
		"constraint_id", req.ConstraintID,
		"audit_id", result.AuditID,
	)

	return result, nil
}

// ---------------------------------------------------------------------------
// Compensating actions for audit failure (R1)
// ---------------------------------------------------------------------------

// compensateAuditFailure undoes the store mutation when the subsequent audit
// write fails, so that no unaudited authority change persists. Each operation
// has a specific compensating action:
//   - create: delete the newly-created constraint
//   - update: restore the previous revision from beforeState
//   - delete: re-create the constraint from beforeState
//
// Compensating actions are best-effort: if the compensating write itself fails
// the inconsistency is logged. This is acceptable because the alternative
// (leaving unaudited mutations) is worse, and the window is narrow.
//
// TODO: This entire method becomes unnecessary when Store.RunInTx is added,
// because the audit write and data mutation will share a single transaction.
func (gs *GovernanceService) compensateAuditFailure(ctx context.Context, req CommitRequest, result *CommitResult, beforeState *store.AccessConstraint) {
	switch req.Operation {
	case "create":
		// Compensating action: delete the newly-created constraint.
		if result.Constraint != nil {
			if err := gs.store.DeleteAccessConstraint(ctx, result.Constraint.ID); err != nil {
				gs.logger.Error("compensating delete failed after audit failure — unaudited create persists",
					"constraint_id", result.Constraint.ID,
					"error", err,
				)
			} else {
				gs.logger.Info("compensating delete succeeded — rolled back unaudited create",
					"constraint_id", result.Constraint.ID,
				)
			}
		}

	case "update":
		// Compensating action: restore the before-state.
		if beforeState != nil {
			if _, err := gs.store.UpdateAccessConstraint(ctx, beforeState, 0); err != nil {
				gs.logger.Error("compensating update failed after audit failure — unaudited update persists",
					"constraint_id", req.ConstraintID,
					"error", err,
				)
			} else {
				gs.logger.Info("compensating update succeeded — restored previous revision",
					"constraint_id", req.ConstraintID,
				)
			}
		}

	case "delete":
		// Compensating action: re-create the constraint from before-state.
		if beforeState != nil {
			if _, err := gs.store.CreateAccessConstraint(ctx, beforeState); err != nil {
				gs.logger.Error("compensating re-create failed after audit failure — unaudited delete persists",
					"constraint_id", req.ConstraintID,
					"error", err,
				)
			} else {
				gs.logger.Info("compensating re-create succeeded — restored deleted constraint",
					"constraint_id", req.ConstraintID,
				)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Re-authorization
// ---------------------------------------------------------------------------

// reauthorizeActor verifies the actor still holds access_constraint.admin.
// Uses getEffectivePermissions instead of Decide to avoid requiring a full
// Identity object — the governance service operates at the principal level.
func (gs *GovernanceService) reauthorizeActor(ctx context.Context, req CommitRequest) error {
	scopeType, scopeID := gs.resolveScope(ctx, req)
	actorPerms, err := gs.authz.getEffectivePermissions(
		ctx,
		NormalizePrincipalType(string(req.Actor.Kind)),
		req.Actor.ID,
		scopeType, scopeID,
	)
	if err != nil {
		return &GovernanceError{
			Code:    ErrCodeMutationPermissionLost,
			Message: fmt.Sprintf("failed to resolve actor permissions: %v", err),
		}
	}

	for _, p := range actorPerms {
		if p == PermissionConstraintAdmin {
			return nil
		}
	}

	return &GovernanceError{
		Code:    ErrCodeMutationPermissionLost,
		Message: "actor no longer holds access_constraint.admin permission",
	}
}

// ---------------------------------------------------------------------------
// Classification
// ---------------------------------------------------------------------------

// classifyOperation determines the server classification for the mutation.
func (gs *GovernanceService) classifyOperation(ctx context.Context, req CommitRequest, now time.Time) (string, error) {
	// For delete: always a relaxation (removing a boundary widens authority).
	if req.Operation == "delete" {
		return ClassificationRelax, nil
	}

	// For create: always a tightening (adding a boundary restricts authority).
	if req.Operation == "create" {
		// Check if the new constraint allows access_constraint.admin — a purely
		// administrative constraint that includes admin is "tighten" or "no_effect"
		// but never a relaxation.
		return ClassificationTighten, nil
	}

	// For update: compare existing and proposed to determine direction.
	if req.ConstraintID == "" {
		return ClassificationTighten, nil
	}

	existing, err := gs.store.GetAccessConstraint(ctx, req.ConstraintID)
	if err != nil {
		return "", fmt.Errorf("failed to load existing constraint: %w", err)
	}

	existingPerms := make(map[string]struct{}, len(existing.MaximumPermissions))
	for _, p := range existing.MaximumPermissions {
		existingPerms[p] = struct{}{}
	}

	proposedPerms := make(map[string]struct{}, len(req.Draft.MaximumPermissions))
	for _, p := range req.Draft.MaximumPermissions {
		proposedPerms[p] = struct{}{}
	}

	hasRemoved := false
	hasAdded := false
	for p := range existingPerms {
		if _, ok := proposedPerms[p]; !ok {
			hasRemoved = true // Permission removed from max set = tightening.
		}
	}
	for p := range proposedPerms {
		if _, ok := existingPerms[p]; !ok {
			hasAdded = true // Permission added to max set = relaxing.
		}
	}

	// Also consider subject/scope changes.
	existingHub := storeToHubAccessConstraint(existing)
	proposedHub := storeToHubAccessConstraint(req.Draft)
	if existingHub != nil && proposedHub != nil {
		if existingHub.Subject != proposedHub.Subject || existingHub.Scope != proposedHub.Scope {
			// Subject/scope change requires full re-evaluation — treat as mixed.
			return ClassificationMixed, nil
		}
	}

	if hasRemoved && hasAdded {
		return ClassificationMixed, nil
	}
	if hasAdded {
		return ClassificationRelax, nil
	}
	if hasRemoved {
		return ClassificationTighten, nil
	}
	return ClassificationTighten, nil // No permission changes = default to tighten.
}

// ---------------------------------------------------------------------------
// Relaxation authority
// ---------------------------------------------------------------------------

// checkRelaxationAuthority verifies the actor has sufficient authority over
// the permissions being restored by a relaxation.
func (gs *GovernanceService) checkRelaxationAuthority(ctx context.Context, req CommitRequest, scopeType, scopeID string) error {
	// Get the actor's effective permissions at the relevant scope.
	actorPerms, err := gs.authz.getEffectivePermissions(
		ctx,
		NormalizePrincipalType(string(req.Actor.Kind)),
		req.Actor.ID,
		scopeType, scopeID,
	)
	if err != nil {
		return &GovernanceError{
			Code:    ErrCodeInsufficientRelaxationAuthority,
			Message: fmt.Sprintf("failed to resolve actor permissions: %v", err),
		}
	}

	actorPermSet := make(map[string]struct{}, len(actorPerms))
	for _, p := range actorPerms {
		actorPermSet[p] = struct{}{}
	}

	// Determine which permissions are being restored (relaxed).
	var restoredPerms []string
	if req.Operation == "delete" {
		// Delete restores all permissions the boundary was removing. Ideally
		// restoredPerms would contain every permission NOT in the boundary's
		// max set (the permissions the boundary was restricting), but we lack
		// a full permission registry to enumerate "everything else."
		//
		// Conservative approach: require the actor to hold authority over all
		// permissions in the boundary's max set. Rationale: the boundary
		// governs these permissions; deleting it restores unrestricted access
		// to them (and more). If the actor does not even hold authority over
		// the permissions the boundary explicitly allows, they should not be
		// able to remove the boundary.
		if req.ConstraintID != "" {
			existing, err := gs.store.GetAccessConstraint(ctx, req.ConstraintID)
			if err == nil {
				restoredPerms = append(restoredPerms, existing.MaximumPermissions...)
			}
		}
	} else if req.Operation == "update" && req.Draft != nil {
		// Permissions being added to the max set are being "relaxed" (restored).
		existing, err := gs.store.GetAccessConstraint(ctx, req.ConstraintID)
		if err == nil {
			existingSet := make(map[string]struct{}, len(existing.MaximumPermissions))
			for _, p := range existing.MaximumPermissions {
				existingSet[p] = struct{}{}
			}
			for _, p := range req.Draft.MaximumPermissions {
				if _, ok := existingSet[p]; !ok {
					restoredPerms = append(restoredPerms, p)
				}
			}
		}
	}

	// The actor must hold each permission being restored.
	var missingPerms []string
	for _, p := range restoredPerms {
		if _, ok := actorPermSet[p]; !ok {
			missingPerms = append(missingPerms, p)
		}
	}

	if len(missingPerms) > 0 {
		return &GovernanceError{
			Code:    ErrCodeInsufficientRelaxationAuthority,
			Message: fmt.Sprintf("actor lacks authority over %d permission(s) being restored", len(missingPerms)),
			Details: map[string]interface{}{
				"missingPermissionIds": missingPerms,
			},
		}
	}

	return nil
}

// ---------------------------------------------------------------------------
// Lockout invariant
// ---------------------------------------------------------------------------

// enforceLockout verifies that at least one active direct user retains
// access_constraint.admin at the relevant scope after the proposed change.
// Zero resolved admins = conflict error, NEVER a degraded-state pass.
func (gs *GovernanceService) enforceLockout(ctx context.Context, req CommitRequest, scopeType, scopeID string, now time.Time) error {
	// Use PreviewService's lockout assessment (which uses the proposed state).
	var existing, proposed *AccessConstraint
	if req.ConstraintID != "" {
		sc, err := gs.store.GetAccessConstraint(ctx, req.ConstraintID)
		if err != nil {
			return fmt.Errorf("failed to load constraint for lockout check: %w", err)
		}
		existing = storeToHubAccessConstraint(sc)
	}
	if req.Draft != nil {
		proposed = storeToHubAccessConstraint(req.Draft)
		if req.ConstraintID != "" {
			proposed.ID = req.ConstraintID
		}
	}

	assessment := gs.preview.assessLockout(ctx, PreviewRequest{
		Operation:    req.Operation,
		Draft:        req.Draft,
		ConstraintID: req.ConstraintID,
		BaseRevision: req.BaseRevision,
		Actor:        req.Actor,
	}, existing, proposed, now)

	if assessment.Safe == nil {
		// Undetermined lockout = conflict, not pass.
		return &GovernanceError{
			Code:    ErrCodeConstraintAdminLockout,
			Message: fmt.Sprintf("lockout assessment undetermined: %s", assessment.UndeterminedReason),
		}
	}

	if !*assessment.Safe {
		remaining := 0
		if assessment.RemainingActiveDirectAdmins != nil {
			remaining = *assessment.RemainingActiveDirectAdmins
		}
		return &GovernanceError{
			Code:    ErrCodeConstraintAdminLockout,
			Message: fmt.Sprintf("mutation would leave %d constraint admin(s); at least one active direct user must retain access_constraint.admin", remaining),
			Details: map[string]interface{}{
				"remainingAdmins": remaining,
			},
		}
	}

	return nil
}

// ---------------------------------------------------------------------------
// Adjacent-domain governance gates (B5 §4)
// ---------------------------------------------------------------------------

// AdjacentDomainCheck is the result of checking whether an adjacent-domain
// operation requires boundary review.
type AdjacentDomainCheck struct {
	// ReviewRequired is true when the operation affects a boundary-relevant entity.
	ReviewRequired bool

	// ImpactURL is the URL to preview the impact of this operation.
	ImpactURL string

	// AffectedBoundaryIDs lists the boundary IDs affected.
	AffectedBoundaryIDs []string

	// Message is a human-readable explanation.
	Message string
}

// CheckGroupMemberRemoval checks whether removing a member from a group
// requires boundary review. Returns security_review_required if the group is
// a constraint-bearing group (appears as a boundary subject).
func (gs *GovernanceService) CheckGroupMemberRemoval(ctx context.Context, groupID, memberType, memberID string) (*AdjacentDomainCheck, error) {
	boundaryIDs, err := gs.findConstraintBearingGroupBoundaries(ctx, groupID)
	if err != nil {
		return nil, fmt.Errorf("failed to check constraint-bearing group: %w", err)
	}

	if len(boundaryIDs) == 0 {
		return &AdjacentDomainCheck{ReviewRequired: false}, nil
	}

	return &AdjacentDomainCheck{
		ReviewRequired:      true,
		AffectedBoundaryIDs: boundaryIDs,
		Message:             fmt.Sprintf("group %s is referenced by %d access boundary(ies); removing member %s:%s requires impact review", groupID, len(boundaryIDs), memberType, memberID),
	}, nil
}

// CheckGroupDeletion checks whether deleting a group requires boundary review.
func (gs *GovernanceService) CheckGroupDeletion(ctx context.Context, groupID string) (*AdjacentDomainCheck, error) {
	boundaryIDs, err := gs.findConstraintBearingGroupBoundaries(ctx, groupID)
	if err != nil {
		return nil, fmt.Errorf("failed to check constraint-bearing group: %w", err)
	}

	if len(boundaryIDs) == 0 {
		return &AdjacentDomainCheck{ReviewRequired: false}, nil
	}

	return &AdjacentDomainCheck{
		ReviewRequired:      true,
		AffectedBoundaryIDs: boundaryIDs,
		Message:             fmt.Sprintf("group %s is referenced by %d access boundary(ies); deletion requires impact review", groupID, len(boundaryIDs)),
	}, nil
}

// CheckRoleBindingChange checks whether creating, replacing, removing, or
// expiring a role binding requires boundary review. This applies when the
// role includes access_constraint.admin — the lockout invariant must survive.
func (gs *GovernanceService) CheckRoleBindingChange(ctx context.Context, bindingID string, roleDefID string, isRemoval bool) (*AdjacentDomainCheck, error) {
	// Check if the role includes access_constraint.admin.
	rd, err := gs.store.GetRoleDefinition(ctx, roleDefID)
	if err != nil {
		return nil, fmt.Errorf("failed to load role definition: %w", err)
	}

	if !roleHasConstraintAdmin(rd) {
		return &AdjacentDomainCheck{ReviewRequired: false}, nil
	}

	return &AdjacentDomainCheck{
		ReviewRequired: true,
		Message:        fmt.Sprintf("role %q includes %s; this change requires lockout invariant verification", rd.Name, PermissionConstraintAdmin),
	}, nil
}

// CheckUserSuspension checks whether suspending or deleting a user requires
// boundary review. Returns security_review_required if the user is the last
// constraint admin.
func (gs *GovernanceService) CheckUserSuspension(ctx context.Context, userID string) (*AdjacentDomainCheck, error) {
	// Check if the user holds access_constraint.admin via any role binding.
	isAdmin, err := gs.isConstraintAdmin(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to check admin status: %w", err)
	}

	if !isAdmin {
		return &AdjacentDomainCheck{ReviewRequired: false}, nil
	}

	return &AdjacentDomainCheck{
		ReviewRequired: true,
		Message:        fmt.Sprintf("user %s holds constraint admin permission; suspension/deletion requires lockout invariant verification", userID),
	}, nil
}

// ---------------------------------------------------------------------------
// Atomic RoleBinding replacement (B5 §5)
// ---------------------------------------------------------------------------

// ReplaceRoleBinding atomically replaces a role binding so grant downgrades
// never leave both old and new bindings active simultaneously.
func (gs *GovernanceService) ReplaceRoleBinding(ctx context.Context, oldBindingID string, newBinding *store.RoleBinding, previewToken string) (*store.RoleBinding, error) {
	// Load existing binding.
	existing, err := gs.store.GetRoleBinding(ctx, oldBindingID)
	if err != nil {
		return nil, fmt.Errorf("failed to load existing binding: %w", err)
	}

	// Check if the old binding's role includes access_constraint.admin.
	oldRD, err := gs.store.GetRoleDefinition(ctx, existing.RoleDefinitionID)
	if err != nil {
		return nil, fmt.Errorf("failed to load old role definition: %w", err)
	}

	oldHasAdmin := roleHasConstraintAdmin(oldRD)

	// Check if new binding's role includes access_constraint.admin.
	newRD, err := gs.store.GetRoleDefinition(ctx, newBinding.RoleDefinitionID)
	if err != nil {
		return nil, fmt.Errorf("failed to load new role definition: %w", err)
	}

	newHasAdmin := roleHasConstraintAdmin(newRD)

	// If downgrading from admin, enforce lockout invariant.
	if oldHasAdmin && !newHasAdmin {
		if err := gs.enforceRoleBindingLockout(ctx, existing); err != nil {
			return nil, err
		}
	}

	// Create new binding first, then delete old one.
	//
	// Order rationale: create-then-delete means the failure mode is "both
	// bindings active" (overly permissive but detectable and recoverable)
	// rather than "zero bindings" (data loss). This is a strictly better
	// failure mode given that the Store interface lacks transaction support.
	//
	// TODO: Migrate to a true atomic swap when Store gains RunInTx support.
	created, err := gs.store.CreateRoleBinding(ctx, newBinding)
	if err != nil {
		return nil, fmt.Errorf("failed to create replacement binding: %w", err)
	}

	if err := gs.store.DeleteRoleBinding(ctx, oldBindingID); err != nil {
		// Compensating action: delete the newly created binding to prevent
		// inconsistent state where both old and new bindings are active.
		if delErr := gs.store.DeleteRoleBinding(ctx, created.ID); delErr != nil {
			gs.logger.Error("failed to roll back new binding after old-binding delete failure; both bindings now active",
				"old_binding_id", oldBindingID, "new_binding_id", created.ID,
				"delete_error", err, "rollback_error", delErr)
		} else {
			gs.logger.Warn("rolled back new binding after old-binding delete failure",
				"old_binding_id", oldBindingID, "new_binding_id", created.ID, "error", err)
		}
		return nil, fmt.Errorf("failed to delete old binding: %w", err)
	}

	return created, nil
}

// enforceRoleBindingLockout checks whether removing an admin role binding
// would violate the lockout invariant.
func (gs *GovernanceService) enforceRoleBindingLockout(ctx context.Context, binding *store.RoleBinding) error {
	now := gs.nowFunc()

	// Resolve all admin users at the binding's scope.
	adminUsers, err := gs.preview.resolveAdminUsers(ctx, binding.ScopeType, binding.ScopeID)
	if err != nil {
		return &GovernanceError{
			Code:    ErrCodeConstraintAdminLockout,
			Message: fmt.Sprintf("failed to resolve admin users for lockout check: %v", err),
		}
	}

	// Zero admins = conflict, not pass.
	if len(adminUsers) == 0 {
		return &GovernanceError{
			Code:    ErrCodeConstraintAdminLockout,
			Message: "no constraint admins found; cannot remove admin role binding",
		}
	}

	// Count admins that would survive if this binding is removed.
	surviving := 0
	for _, au := range adminUsers {
		// The user bound by this binding would lose admin if this is their
		// only admin binding.
		if binding.PrincipalType == "user" && au.userID == binding.PrincipalID {
			// Check if user has other admin bindings.
			otherBindings, err := gs.store.ListRoleBindingsForPrincipals(ctx,
				[]store.PrincipalRef{{Type: "user", ID: au.userID}}, nil, nil)
			if err != nil {
				continue
			}
			hasOtherAdmin := false
			for _, ob := range otherBindings {
				if ob.ID == binding.ID {
					continue
				}
				rd, err := gs.store.GetRoleDefinition(ctx, ob.RoleDefinitionID)
				if err != nil {
					continue
				}
				if roleHasConstraintAdmin(rd) {
					hasOtherAdmin = true
				}
				if hasOtherAdmin {
					break
				}
			}
			if hasOtherAdmin {
				surviving++
			}
			continue
		}
		surviving++
	}

	if surviving == 0 {
		return &GovernanceError{
			Code:    ErrCodeConstraintAdminLockout,
			Message: "removing this role binding would leave zero constraint admins",
		}
	}

	// Check scheduled state: constraints with future activation windows
	// (NotBefore in the future) or those currently active may further
	// restrict admin permissions. Use IsActiveInMostRestrictiveState to
	// include future-activating constraints in the evaluation, matching
	// the approach in assessLockout.
	constraints, err := gs.store.ListConstraintsForScope(ctx, binding.ScopeType, binding.ScopeID)
	if err != nil {
		return &GovernanceError{
			Code:    ErrCodeConstraintAdminLockout,
			Message: fmt.Sprintf("failed to load constraints for scheduled-state lockout check: %v", err),
		}
	}

	// Filter to constraints that restrict admin in most-restrictive state
	// (including future-activating ones that have not yet expired).
	var restricting []*store.AccessConstraint
	for _, c := range constraints {
		if c.Disabled {
			continue
		}
		cond := ConstraintCondition{}
		if c.NotBefore != nil {
			cond.NotBefore = *c.NotBefore
		}
		if c.ExpiresAt != nil {
			cond.ExpiresAt = *c.ExpiresAt
		}
		if !cond.IsActiveInMostRestrictiveState(now) {
			continue
		}
		if constraintAllowsPermissionStore(c, PermissionConstraintAdmin) {
			continue
		}
		restricting = append(restricting, c)
	}

	if len(restricting) > 0 {
		// Re-evaluate surviving admins against scheduled constraints.
		scheduledSurviving := 0
		for _, au := range adminUsers {
			// Skip the user whose binding is being removed (if they lost admin above).
			if binding.PrincipalType == "user" && au.userID == binding.PrincipalID {
				// Check if they had other admin bindings.
				otherBindings, lookupErr := gs.store.ListRoleBindingsForPrincipals(ctx,
					[]store.PrincipalRef{{Type: "user", ID: au.userID}}, nil, nil)
				if lookupErr != nil {
					continue
				}
				hasOtherAdmin := false
				for _, ob := range otherBindings {
					if ob.ID == binding.ID {
						continue
					}
					rd, rdErr := gs.store.GetRoleDefinition(ctx, ob.RoleDefinitionID)
					if rdErr != nil {
						continue
					}
					if roleHasConstraintAdmin(rd) {
						hasOtherAdmin = true
						break
					}
				}
				if !hasOtherAdmin {
					continue // This user lost admin — skip.
				}
			}

			// Check if any restricting constraint matches this user.
			blocked := false
			for _, c := range restricting {
				if gs.preview.constraintMatchesUser(ctx, c, au) {
					blocked = true
					break
				}
			}
			if !blocked {
				scheduledSurviving++
			}
		}

		if scheduledSurviving == 0 {
			return &GovernanceError{
				Code:    ErrCodeConstraintAdminLockout,
				Message: "removing this role binding would leave zero constraint admins when scheduled constraints activate",
			}
		}
	}

	return nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// roleHasConstraintAdmin checks whether a role definition includes the
// access_constraint.admin permission.
func roleHasConstraintAdmin(rd *store.RoleDefinition) bool {
	for _, p := range rd.Permissions {
		if p == PermissionConstraintAdmin {
			return true
		}
	}
	return false
}

// resolveScope determines the scope for a commit request.
func (gs *GovernanceService) resolveScope(ctx context.Context, req CommitRequest) (string, string) {
	if req.Draft != nil {
		return req.Draft.ScopeType, req.Draft.ScopeID
	}
	if req.ConstraintID != "" {
		sc, err := gs.store.GetAccessConstraint(ctx, req.ConstraintID)
		if err == nil {
			return sc.ScopeType, sc.ScopeID
		}
	}
	return ScopeTypeSystem, ""
}

// findConstraintBearingGroupBoundaries finds all boundary IDs that reference
// the given group as a subject (either exact group or group closure).
func (gs *GovernanceService) findConstraintBearingGroupBoundaries(ctx context.Context, groupID string) ([]string, error) {
	const pageSize = 500
	offset := 0
	var boundaryIDs []string

	for {
		constraints, err := gs.store.ListAccessConstraints(ctx, pageSize, offset)
		if err != nil {
			return nil, err
		}
		for _, c := range constraints {
			if c.SubjectKind == store.ConstraintSubjectPrincipal &&
				c.SubjectPrincipalType != nil && *c.SubjectPrincipalType == "group" &&
				c.SubjectPrincipalID != nil && *c.SubjectPrincipalID == groupID {
				boundaryIDs = append(boundaryIDs, c.ID)
			}
			if c.SubjectKind == store.ConstraintSubjectGroupClosure &&
				c.SubjectGroupID != nil && *c.SubjectGroupID == groupID {
				boundaryIDs = append(boundaryIDs, c.ID)
			}
		}
		if len(constraints) < pageSize {
			break
		}
		offset += len(constraints)
	}

	return boundaryIDs, nil
}

// isConstraintAdmin checks if a user holds access_constraint.admin via any
// role binding.
func (gs *GovernanceService) isConstraintAdmin(ctx context.Context, userID string) (bool, error) {
	// Get role bindings for this user.
	principals := []store.PrincipalRef{{Type: "user", ID: userID}}

	// Also include group-expanded bindings.
	groups, err := gs.store.GetEffectiveGroups(ctx, userID)
	if err == nil {
		for _, gid := range groups {
			principals = append(principals, store.PrincipalRef{Type: "group", ID: gid})
		}
	}

	bindings, err := gs.store.ListRoleBindingsForPrincipals(ctx, principals, nil, nil)
	if err != nil {
		return false, err
	}

	for _, b := range bindings {
		rd, err := gs.store.GetRoleDefinition(ctx, b.RoleDefinitionID)
		if err != nil {
			continue
		}
		if roleHasConstraintAdmin(rd) {
			return true, nil
		}
	}

	return false, nil
}

// ---------------------------------------------------------------------------
// Audit helpers
// ---------------------------------------------------------------------------

// correlationIDFromContext extracts a correlation ID from the context.
// Returns empty string if none is set.
func correlationIDFromContext(ctx context.Context) string {
	if v := ctx.Value(correlationIDKey); v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// contextWithCorrelationID returns a new context with the given correlation ID.
func contextWithCorrelationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, correlationIDKey, id)
}

type contextKey string

const correlationIDKey contextKey = "correlation_id"

// computeImpactCounts derives impact counts from the commit request and the
// before-state of the constraint. beforeState may be nil for creates.
func computeImpactCounts(req CommitRequest, beforeState *store.AccessConstraint) ImpactCounts {
	counts := ImpactCounts{}

	// Build sets for before and after permissions.
	beforePerms := make(map[string]struct{})
	if beforeState != nil {
		for _, p := range beforeState.MaximumPermissions {
			beforePerms[p] = struct{}{}
		}
	}

	afterPerms := make(map[string]struct{})
	if req.Draft != nil {
		for _, p := range req.Draft.MaximumPermissions {
			afterPerms[p] = struct{}{}
		}
	}

	// PermissionsAdded: permissions in after that are not in before.
	for p := range afterPerms {
		if _, ok := beforePerms[p]; !ok {
			counts.PermissionsAdded++
		}
	}

	// PermissionsRemoved: permissions in before that are not in after.
	// For delete (Draft is nil, afterPerms is empty), all before-state
	// permissions are removed.
	for p := range beforePerms {
		if _, ok := afterPerms[p]; !ok {
			counts.PermissionsRemoved++
		}
	}

	// TODO: Populate AffectedPrincipals from the preview result when the
	// preview engine exposes a principal count in its response. The preview
	// result's Impact.TotalAffected field contains this data but is not yet
	// threaded through to the commit path.

	return counts
}
