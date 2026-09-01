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
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ---------------------------------------------------------------------------
// ProjectDeletionService — RS3 bounded domain service
//
// Project deletion flows through this service. The HTTP handler validates
// transport input and delegates; it never directly performs authorization,
// cascading deletes, or audit for project deletion.
//
// The service implements:
//   - Fail-closed authorization composition (base permission + ownership/ancestry)
//   - Protected target governance (direct owner or super-admin/hub-admin)
//   - Actor status/credential ceiling enforcement
//   - Atomic security state and audit within a transaction
//   - Explicit external effect ordering with failure contracts
//   - Concurrency serialization via project lock
//   - Idempotent handling of already-deleted targets
//   - Stable denial codes (lower_snake_case)
// ---------------------------------------------------------------------------

// ProjectDeletionService is the bounded domain service for project deletion.
type ProjectDeletionService struct {
	store   store.Store
	authz   *AuthzService
	logger  *slog.Logger
	nowFunc func() time.Time
}

// NewProjectDeletionService creates a new ProjectDeletionService.
func NewProjectDeletionService(
	s store.Store,
	authz *AuthzService,
	logger *slog.Logger,
) *ProjectDeletionService {
	return &ProjectDeletionService{
		store:   s,
		authz:   authz,
		logger:  logger,
		nowFunc: time.Now,
	}
}

// ---------------------------------------------------------------------------
// Request / Result / Decision types
// ---------------------------------------------------------------------------

// ProjectDeleteRequest describes a project deletion request.
type ProjectDeleteRequest struct {
	ProjectID string
	Actor     UserIdentity
}

// ProjectDeleteDecision captures the authorization/governance outcome.
type ProjectDeleteDecision struct {
	Allowed    bool
	DenialCode string
	Reason     string
	HTTPStatus int
}

// ProjectDeleteResult is the outcome of a successful project deletion.
type ProjectDeleteResult struct {
	Project *store.Project // The project record before deletion.
}

// ---------------------------------------------------------------------------
// Denial codes for project deletion
// ---------------------------------------------------------------------------

const (
	// ErrCodeProjectDeleteForbidden indicates the actor lacks authority to
	// delete the project.
	ErrCodeProjectDeleteForbidden = "forbidden"

	// ErrCodeUserSuspended indicates the actor's account is suspended.
	ErrCodeUserSuspended = "user_suspended"

	// ErrCodeCredentialInsufficient indicates the credential does not meet
	// the requirements for project deletion.
	ErrCodeCredentialInsufficient = "credential_insufficient"
)

// ---------------------------------------------------------------------------
// Core deletion operation
// ---------------------------------------------------------------------------

// Delete performs a governed project deletion with authorization, atomic
// security-state cleanup, and audit. External effects (broker dispatch,
// filesystem, quota) are NOT performed here — they are the caller's
// responsibility after a successful return, with explicit ordering.
//
// The method:
//  1. Resolves the target project (fail-closed on not-found).
//  2. Checks base permission (project.delete).
//  3. Enforces ownership/ancestry governance.
//  4. Checks actor status (not suspended).
//  5. Checks credential ceiling (session JWT only, no scoped UAT/agent).
//  6. Acquires the project membership lock for serialization.
//  7. Re-evaluates authority under lock (TOCTOU closure).
//  8. Performs security-relevant cascading deletes within the transaction.
//  9. Deletes the project row.
//  10. Writes the atomic audit record.
//
// Returns (result, nil) on success, (nil, decision) on denial.
func (svc *ProjectDeletionService) Delete(ctx context.Context, req ProjectDeleteRequest) (*ProjectDeleteResult, *ProjectDeleteDecision) {
	// 1. Resolve the target project.
	project, err := svc.store.GetProject(ctx, req.ProjectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Non-oracular: deleted/nonexistent targets return 404 uniformly
			// for authorized callers. Since we haven't checked authorization
			// yet, return 404 which is indistinguishable from "unauthorized"
			// for external callers (no existence oracle).
			return nil, &ProjectDeleteDecision{
				Allowed:    false,
				DenialCode: ErrCodeNotFound,
				Reason:     "project not found",
				HTTPStatus: 404,
			}
		}
		svc.logger.Error("failed to resolve project for deletion",
			"project_id", req.ProjectID, "error", err)
		return nil, &ProjectDeleteDecision{
			Allowed:    false,
			DenialCode: ErrCodeProjectDeleteForbidden,
			Reason:     "internal error resolving project",
			HTTPStatus: 500,
		}
	}

	// 2. Check base permission (project.delete) via the authz evaluator.
	if svc.authz != nil {
		resource := Resource{
			Type:    "project",
			ID:      project.ID,
			OwnerID: project.OwnerID,
			Labels:  project.Labels,
		}
		decision := svc.authz.CheckAccess(ctx, req.Actor, resource, ActionDelete)
		if !decision.Allowed {
			return nil, &ProjectDeleteDecision{
				Allowed:    false,
				DenialCode: ErrCodeProjectDeleteForbidden,
				Reason:     "insufficient permissions for project deletion",
				HTTPStatus: 403,
			}
		}
	}

	// 3. Enforce ownership/ancestry governance.
	// Project deletion requires the actor to be:
	//   - A direct project owner (active binding), OR
	//   - A super-admin (system-scoped), OR
	//   - A hub-admin (system-scoped)
	// Group-derived ownership does NOT confer deletion authority.
	govDecision := svc.checkDeletionGovernance(ctx, req)
	if !govDecision.Allowed {
		return nil, &govDecision
	}

	// 4. Check actor status — suspended users cannot delete projects.
	if err := svc.checkActorStatus(ctx, req.Actor.ID()); err != nil {
		return nil, err
	}

	// 5. Credential ceiling — project deletion is restricted to full session.
	// Scoped UATs and agent JWTs are not admitted.
	// Dev credentials (local development mode) are equivalent to interactive.
	credential := GetCredentialContextFromContext(ctx)
	if credential.Kind != "" && credential.Kind != CredentialKindInteractive && credential.Kind != CredentialKindDev {
		return nil, &ProjectDeleteDecision{
			Allowed:    false,
			DenialCode: ErrCodeCredentialInsufficient,
			Reason:     "project deletion requires a full session credential",
			HTTPStatus: 403,
		}
	}

	// Pre-compute system-scoped admin status outside the transaction.
	// These are system-scoped bindings (not project-scoped), so they are
	// unaffected by the project lock and safe to read outside the tx.
	// Reading them inside the tx would deadlock on SQLite's single-connection
	// pool because svc.authz uses the outer store, which shares the same
	// connection the transaction holds.
	isAdmin := false
	if svc.authz != nil {
		isAdmin = svc.authz.IsSystemAdmin(ctx, req.Actor.ID()) || svc.authz.IsHubAdmin(ctx, req.Actor.ID())
	}

	// 6–10. Transactional phase: lock, re-check, cascade, delete, audit.
	var result *ProjectDeleteResult
	txErr := svc.store.WithTx(ctx, func(tx store.Store) error {
		// 6. Acquire project-scoped serialization lock.
		if err := tx.LockProjectForMembership(ctx, req.ProjectID); err != nil {
			return fmt.Errorf("lock project for deletion: %w", err)
		}

		// 7. Re-evaluate governance under lock to close TOCTOU window.
		// A concurrent role change that commits between the pre-lock check
		// and lock acquisition is visible here.
		// System-admin/hub-admin status is pre-computed; only project-scoped
		// ownership is re-evaluated from the transactional store.
		reGov := svc.checkDeletionGovernanceFromStore(ctx, tx, req, isAdmin)
		if !reGov.Allowed {
			return fmt.Errorf("governance:%d:%s", reGov.HTTPStatus, reGov.Reason)
		}

		// 8. Cascade security-relevant state within the transaction.
		if err := svc.cascadeSecurityState(ctx, tx, req.ProjectID); err != nil {
			return fmt.Errorf("cascade security state: %w", err)
		}

		// 9. Delete the project row.
		if err := tx.DeleteProject(ctx, req.ProjectID); err != nil {
			return fmt.Errorf("delete project: %w", err)
		}

		// 10. Write atomic audit record.
		auditRecord := &store.MutationAuditRecord{
			MutationType: "project_delete",
			TargetType:   "project",
			TargetID:     project.ID,
			BeforeSummary: marshalDeletionAuditJSON(map[string]string{
				"project_id":   project.ID,
				"project_name": project.Name,
				"project_slug": project.Slug,
				"owner_id":     project.OwnerID,
			}),
		}
		if err := svc.createAuditRecord(ctx, tx, auditRecord); err != nil {
			return fmt.Errorf("audit record: %w", err)
		}

		result = &ProjectDeleteResult{Project: project}
		return nil
	})

	if txErr != nil {
		// Parse governance denial from tx error.
		if status, code, reason, ok := parseGovernanceError(txErr); ok {
			return nil, &ProjectDeleteDecision{
				Allowed:    false,
				DenialCode: code,
				Reason:     reason,
				HTTPStatus: status,
			}
		}
		svc.logger.Error("project deletion transaction failed",
			"project_id", req.ProjectID, "error", txErr)
		return nil, &ProjectDeleteDecision{
			Allowed:    false,
			DenialCode: ErrCodeProjectDeleteForbidden,
			Reason:     "project deletion failed",
			HTTPStatus: 500,
		}
	}

	return result, nil
}

// ---------------------------------------------------------------------------
// Governance checks
// ---------------------------------------------------------------------------

// checkDeletionGovernance evaluates ownership/ancestry governance for project
// deletion. Uses the pre-lock store (initial check).
func (svc *ProjectDeletionService) checkDeletionGovernance(ctx context.Context, req ProjectDeleteRequest) ProjectDeleteDecision {
	actorID := req.Actor.ID()

	// Super-admin and hub-admin bypass project-level governance.
	if svc.authz != nil {
		if svc.authz.IsSystemAdmin(ctx, actorID) {
			return ProjectDeleteDecision{Allowed: true}
		}
		if svc.authz.IsHubAdmin(ctx, actorID) {
			return ProjectDeleteDecision{Allowed: true}
		}
	}

	// Direct project owner check.
	if svc.authz != nil && svc.authz.isProjectOwner(ctx, actorID, req.ProjectID) {
		return ProjectDeleteDecision{Allowed: true}
	}

	// Group-derived admin/member: denied. Stale OwnerID: denied.
	return ProjectDeleteDecision{
		Allowed:    false,
		DenialCode: ErrCodeProjectDeleteForbidden,
		Reason:     "only direct project owners, super-admins, or hub-admins can delete projects",
		HTTPStatus: 403,
	}
}

// checkDeletionGovernanceFromStore re-evaluates governance from the
// transactional store after lock acquisition (TOCTOU closure).
// The isAdmin flag is pre-computed outside the transaction to avoid
// accessing svc.authz (which uses the outer store) from inside the tx,
// which would deadlock on SQLite's single-connection pool.
func (svc *ProjectDeletionService) checkDeletionGovernanceFromStore(ctx context.Context, tx store.Store, req ProjectDeleteRequest, isAdmin bool) ProjectDeleteDecision {
	// Super-admin and hub-admin bypass: pre-computed outside the
	// transaction since these are system-scoped, not project-scoped.
	if isAdmin {
		return ProjectDeleteDecision{Allowed: true}
	}

	// Re-evaluate direct ownership from the transactional store.
	actorID := req.Actor.ID()
	now := svc.nowFunc()
	bindings, err := tx.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, actorID)
	if err != nil {
		return ProjectDeleteDecision{
			Allowed:    false,
			DenialCode: ErrCodeProjectDeleteForbidden,
			Reason:     "authority lookup failed under lock",
			HTTPStatus: 500,
		}
	}

	for _, b := range bindings {
		if b.ScopeType != store.RoleScopeProject || b.ScopeID != req.ProjectID {
			continue
		}
		if !isBindingActive(b, now) {
			continue
		}
		rd, rdErr := tx.GetRoleDefinition(ctx, b.RoleDefinitionID)
		if rdErr != nil {
			continue
		}
		if rd.Name == store.ProjectRoleOwner {
			return ProjectDeleteDecision{Allowed: true}
		}
	}

	return ProjectDeleteDecision{
		Allowed:    false,
		DenialCode: ErrCodeProjectDeleteForbidden,
		Reason:     "actor is not a direct project owner (re-evaluated under lock)",
		HTTPStatus: 403,
	}
}

// checkActorStatus verifies the actor is not suspended.
func (svc *ProjectDeletionService) checkActorStatus(ctx context.Context, actorID string) *ProjectDeleteDecision {
	user, err := svc.store.GetUser(ctx, actorID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return &ProjectDeleteDecision{
				Allowed:    false,
				DenialCode: ErrCodeProjectDeleteForbidden,
				Reason:     "actor user not found",
				HTTPStatus: 403,
			}
		}
		return &ProjectDeleteDecision{
			Allowed:    false,
			DenialCode: ErrCodeProjectDeleteForbidden,
			Reason:     "failed to check actor status",
			HTTPStatus: 500,
		}
	}
	if user.Status == "suspended" {
		return &ProjectDeleteDecision{
			Allowed:    false,
			DenialCode: ErrCodeUserSuspended,
			Reason:     "suspended users cannot delete projects",
			HTTPStatus: 403,
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Transactional cascades
// ---------------------------------------------------------------------------

// cascadeSecurityState removes all security-relevant project-scoped state
// within the transaction. This ensures no orphaned authority, credentials,
// or security state survives project deletion.
func (svc *ProjectDeletionService) cascadeSecurityState(ctx context.Context, tx store.Store, projectID string) error {
	// 1. Role bindings — all project-scoped authority grants.
	if n, err := tx.DeleteRoleBindingsForScope(ctx, store.RoleScopeProject, projectID); err != nil {
		return fmt.Errorf("cascade role bindings: %w", err)
	} else if n > 0 {
		svc.logger.Info("cascade-deleted project role bindings", "project_id", projectID, "count", n)
	}

	// 2. Groups — project-scoped groups (which may carry role bindings).
	if groups, err := tx.ListGroups(ctx, store.GroupFilter{ProjectID: projectID}, store.ListOptions{Limit: 1000}); err == nil {
		for _, g := range groups.Items {
			if err := tx.DeleteGroup(ctx, g.ID); err != nil {
				return fmt.Errorf("cascade group %s: %w", g.ID, err)
			}
		}
	} else {
		return fmt.Errorf("list project groups: %w", err)
	}

	// 3. Secrets — project-scoped secrets.
	if _, err := tx.DeleteSecretsByScope(ctx, store.ScopeProject, projectID); err != nil {
		return fmt.Errorf("cascade secrets: %w", err)
	}

	// 4. Env vars — project-scoped environment variables.
	if _, err := tx.DeleteEnvVarsByScope(ctx, store.ScopeProject, projectID); err != nil {
		return fmt.Errorf("cascade env vars: %w", err)
	}

	// 5. Skill injections — project-scoped skill injections.
	if _, err := tx.DeleteSkillInjectionsByScope(ctx, store.SkillInjectionScopeProject, projectID); err != nil {
		return fmt.Errorf("cascade skill injections: %w", err)
	}

	// 6. GCP service account registrations — project-scoped.
	if sas, err := tx.ListGCPServiceAccounts(ctx, store.GCPServiceAccountFilter{
		Scope:   store.ScopeProject,
		ScopeID: projectID,
	}); err == nil {
		for _, sa := range sas {
			if err := tx.DeleteGCPServiceAccount(ctx, sa.ID); err != nil {
				return fmt.Errorf("cascade GCP SA %s: %w", sa.ID, err)
			}
		}
	} else {
		return fmt.Errorf("list project GCP SAs: %w", err)
	}

	// 7. Templates — project-scoped (DB records only; storage files are external).
	if _, err := tx.DeleteTemplatesByScope(ctx, store.ScopeProject, projectID); err != nil {
		return fmt.Errorf("cascade templates: %w", err)
	}

	// 8. Harness configs — project-scoped (DB records only; storage files are external).
	if _, err := tx.DeleteHarnessConfigsByScope(ctx, store.ScopeProject, projectID); err != nil {
		return fmt.Errorf("cascade harness configs: %w", err)
	}

	return nil
}

// ---------------------------------------------------------------------------
// Audit
// ---------------------------------------------------------------------------

// createAuditRecord writes a mutation audit record within the transaction.
func (svc *ProjectDeletionService) createAuditRecord(ctx context.Context, txStore store.Store, record *store.MutationAuditRecord) error {
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

// marshalDeletionAuditJSON marshals a map to JSON for audit before/after fields.
func marshalDeletionAuditJSON(m map[string]string) string {
	b, err := json.Marshal(m)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// Governance error parsing (reuses RS1 pattern)
// ---------------------------------------------------------------------------

// parseGovernanceError extracts governance denial details from a formatted
// transaction error. Format: "governance:<status>:<reason>"
func parseGovernanceError(err error) (status int, code string, reason string, ok bool) {
	msg := err.Error()
	var s int
	var r string
	if n, _ := fmt.Sscanf(msg, "governance:%d:", &s); n == 1 {
		// Extract reason after second colon.
		idx := 0
		colons := 0
		for i, c := range msg {
			if c == ':' {
				colons++
				if colons == 2 {
					idx = i + 1
					break
				}
			}
		}
		if idx > 0 && idx < len(msg) {
			r = msg[idx:]
		}
		return s, ErrCodeProjectDeleteForbidden, r, true
	}
	return 0, "", "", false
}
