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
	"net/http"
	"strconv"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

type ListUsersResponse struct {
	Users        []UserWithCapabilities `json:"users"`
	NextCursor   string                 `json:"nextCursor,omitempty"`
	TotalCount   int                    `json:"totalCount"`
	Capabilities *Capabilities          `json:"_capabilities,omitempty"`
}

func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listUsers(w, r)
	case http.MethodPost:
		s.createUser(w, r)
	default:
		MethodNotAllowed(w)
	}
}

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	query := r.URL.Query()

	filter := store.UserFilter{
		Role:   query.Get("role"),
		Status: query.Get("status"),
		Search: query.Get("search"),
	}

	limit := 50
	if l := query.Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
			limit = parsed
		}
	}

	result, err := s.store.ListUsers(ctx, filter, store.ListOptions{
		Limit:   limit,
		Cursor:  query.Get("cursor"),
		SortBy:  query.Get("sort"),
		SortDir: query.Get("dir"),
	})
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Compute per-item capabilities (users have no scope-level create action)
	identity := GetIdentityFromContext(ctx)
	users := make([]UserWithCapabilities, 0, len(result.Items))
	if identity != nil {
		resources := make([]Resource, len(result.Items))
		for i := range result.Items {
			resources[i] = userResource(&result.Items[i])
		}
		caps := s.authzService.ComputeCapabilitiesBatch(ctx, identity, resources, "user")
		for i := range result.Items {
			if !capabilityAllows(caps[i], ActionRead) {
				continue
			}
			users = append(users, UserWithCapabilities{User: result.Items[i], Cap: caps[i]})
		}
	} else {
		for i := range result.Items {
			users = append(users, UserWithCapabilities{User: result.Items[i]})
		}
	}

	totalCount := result.TotalCount
	if identity != nil && len(users) < len(result.Items) {
		totalCount = len(users)
	}

	writeJSON(w, http.StatusOK, ListUsersResponse{
		Users:      users,
		NextCursor: result.NextCursor,
		TotalCount: totalCount,
	})
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	// User creation is managed by the hub's internal sign-in flows (OAuth).
	// Direct API creation is not permitted.
	writeError(w, http.StatusForbidden, ErrCodeForbidden,
		"user creation is managed through sign-in flows and cannot be performed via the API", nil)
}

func (s *Server) handleUserByID(w http.ResponseWriter, r *http.Request) {
	id := extractID(r, "/api/v1/users")

	if id == "" {
		NotFound(w, "User")
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.getUser(w, r, id)
	case http.MethodPatch:
		s.updateUser(w, r, id)
	case http.MethodDelete:
		s.deleteUser(w, r, id)
	default:
		MethodNotAllowed(w)
	}
}

func (s *Server) getUser(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()
	user, err := s.store.GetUser(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	resp := UserWithCapabilities{User: *user}
	if identity := GetIdentityFromContext(ctx); identity != nil {
		resp.Cap = s.authzService.ComputeCapabilities(ctx, identity, userResource(user))
	}

	writeJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// PATCH /api/v1/users/{id} — per-field permission enforcement (R3)
//
// Each mutable field category requires a distinct permission:
//   - role:                 user.promote + CanDelegate(super-admin)
//   - status:               user.suspend
//   - displayName/prefs:    user.update (self-service for own record)
//
// A mixed PATCH must hold ALL required permissions before any write.
// Unknown JSON fields are rejected. The mutation is atomic/fail-closed.
// ---------------------------------------------------------------------------

// userPatchPayload is the strict set of allowed fields for PATCH /api/v1/users/{id}.
type userPatchPayload struct {
	DisplayName *string                `json:"displayName,omitempty"`
	Role        *string                `json:"role,omitempty"`
	Status      *string                `json:"status,omitempty"`
	Preferences *store.UserPreferences `json:"preferences,omitempty"`
}

func (s *Server) updateUser(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	// Fail closed: authorization service must be available.
	if s.authzService == nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"authorization service unavailable", nil)
		return
	}

	// Require an authenticated user identity.
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return
	}
	actor, ok := identity.(UserIdentity)
	if !ok {
		Forbidden(w)
		return
	}

	user, err := s.store.GetUser(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Parse with strict field rejection: decode into raw map to check for
	// unknown fields, then decode into the typed struct.
	var rawFields map[string]json.RawMessage
	if err := readJSON(r, &rawFields); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	allowedFields := map[string]bool{
		"displayName": true, "role": true, "status": true, "preferences": true,
	}
	for field := range rawFields {
		if !allowedFields[field] {
			BadRequest(w, fmt.Sprintf("unknown field %q; allowed fields are displayName, role, status, preferences", field))
			return
		}
	}

	// Re-parse into typed struct.
	var updates userPatchPayload
	for field, raw := range rawFields {
		switch field {
		case "displayName":
			var v string
			if err := json.Unmarshal(raw, &v); err != nil {
				BadRequest(w, "invalid displayName: "+err.Error())
				return
			}
			updates.DisplayName = &v
		case "role":
			var v string
			if err := json.Unmarshal(raw, &v); err != nil {
				BadRequest(w, "invalid role: "+err.Error())
				return
			}
			updates.Role = &v
		case "status":
			var v string
			if err := json.Unmarshal(raw, &v); err != nil {
				BadRequest(w, "invalid status: "+err.Error())
				return
			}
			updates.Status = &v
		case "preferences":
			var v store.UserPreferences
			if err := json.Unmarshal(raw, &v); err != nil {
				BadRequest(w, "invalid preferences: "+err.Error())
				return
			}
			updates.Preferences = &v
		}
	}

	// Validate role field eagerly — reject unsupported values even if the
	// role matches the current value.
	if updates.Role != nil {
		switch *updates.Role {
		case "admin", "member":
			// valid canonical roles
		default:
			BadRequest(w, fmt.Sprintf("unsupported role %q; valid values are \"admin\" and \"member\"", *updates.Role))
			return
		}
	}

	// Validate status field.
	if updates.Status != nil {
		switch *updates.Status {
		case "active", "suspended":
			// valid
		default:
			BadRequest(w, fmt.Sprintf("unsupported status %q; valid values are \"active\" and \"suspended\"", *updates.Status))
			return
		}
	}

	// ── Permission pre-check: require ALL permissions before any write ──
	//
	// Each field category requires its own permission. A mixed PATCH with
	// role + status must hold BOTH user.promote and user.suspend.

	needsPromote := updates.Role != nil
	needsSuspend := updates.Status != nil
	needsUpdate := updates.DisplayName != nil || updates.Preferences != nil

	// Self-service: a user may update their own displayName/preferences
	// without the cross-user user.update permission.
	isSelf := actor.ID() == user.ID
	needsCrossUserUpdate := needsUpdate && !isSelf

	if needsPromote {
		if err := s.checkUserPromotePermission(ctx, w, actor, user, *updates.Role); err != nil {
			return
		}
	}

	if needsSuspend {
		decision := s.authzService.Decide(ctx, AuthzRequest{
			Principal:  principalContextForIdentity(actor),
			Credential: credentialContextForIdentity(actor),
			Resource:   Resource{Type: "user", ID: user.ID},
			Action:     Action("suspend"),
			Permission: "user.suspend",
		})
		if !decision.Allowed {
			writeForbidden(w, "requires user.suspend permission")
			return
		}
		// Cannot suspend yourself.
		if isSelf {
			writeError(w, http.StatusConflict, ErrCodeConflict,
				"cannot change your own status", nil)
			return
		}
	}

	if needsCrossUserUpdate {
		decision := s.authzService.Decide(ctx, AuthzRequest{
			Principal:  principalContextForIdentity(actor),
			Credential: credentialContextForIdentity(actor),
			Resource:   Resource{Type: "user", ID: user.ID},
			Action:     Action("update"),
			Permission: "user.update",
		})
		if !decision.Allowed {
			writeForbidden(w, "requires user.update permission to modify another user's profile")
			return
		}
	}

	// ── Execute mutations atomically ──

	beforeRole := user.Role
	beforeStatus := user.Status

	// Role changes are handled through the canonical role-binding path.
	if needsPromote {
		if err := s.updateUserRole(ctx, w, actor, user, *updates.Role); err != nil {
			return
		}
	}

	if updates.Status != nil {
		user.Status = *updates.Status
	}
	if updates.DisplayName != nil {
		user.DisplayName = *updates.DisplayName
	}
	if updates.Preferences != nil {
		user.Preferences = updates.Preferences
	}

	// Persist metadata changes (role changes are already persisted atomically
	// inside updateUserRole's transaction, but displayName/status/preferences
	// still need to be saved).
	if needsSuspend || needsUpdate {
		if err := s.store.UpdateUser(ctx, user); err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
	}

	// ── Audit records ──

	if needsPromote && beforeRole != user.Role {
		s.emitMutationAudit(ctx, &store.MutationAuditRecord{
			MutationType: "user_role_change",
			TargetType:   "user",
			TargetID:     user.ID,
			BeforeSummary: fmt.Sprintf(`{"role":%q}`, beforeRole),
			AfterSummary:  fmt.Sprintf(`{"role":%q}`, user.Role),
		})
	}

	if needsSuspend && beforeStatus != user.Status {
		mutationType := "user_suspend"
		if user.Status == "active" {
			mutationType = "user_reactivate"
		}
		s.emitMutationAudit(ctx, &store.MutationAuditRecord{
			MutationType: mutationType,
			TargetType:   "user",
			TargetID:     user.ID,
			BeforeSummary: fmt.Sprintf(`{"status":%q}`, beforeStatus),
			AfterSummary:  fmt.Sprintf(`{"status":%q}`, user.Status),
		})
	}

	writeJSON(w, http.StatusOK, user)
}

// checkUserPromotePermission verifies the actor has user.promote + CanDelegate
// authority for the role transition. Writes HTTP error and returns non-nil on failure.
func (s *Server) checkUserPromotePermission(
	ctx context.Context, w http.ResponseWriter,
	actor UserIdentity, user *store.User, newRole string,
) error {
	// Check user.promote permission first.
	decision := s.authzService.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(actor),
		Credential: credentialContextForIdentity(actor),
		Resource:   Resource{Type: "user", ID: user.ID},
		Action:     Action("promote"),
		Permission: "user.promote",
	})
	if !decision.Allowed {
		writeForbidden(w, "requires user.promote permission")
		return fmt.Errorf("user.promote denied")
	}

	// Additionally require CanDelegate for the super-admin role binding.
	isPromotion := newRole == "admin" && user.Role != "admin"
	isDemotion := newRole != "admin" && user.Role == "admin"
	isSameRoleAdmin := newRole == "admin" && user.Role == "admin"

	if isPromotion || isDemotion || isSameRoleAdmin {
		superAdminRD, err := s.store.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
		if err != nil {
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
				"super-admin role definition not found", nil)
			return err
		}
		canDel := s.authzService.CanDelegate(ctx, actor, GrantDescriptor{
			Type:             GrantTypeRoleBinding,
			RoleDefinitionID: superAdminRD.ID,
			ScopeType:        store.RoleScopeSystem,
		})
		if !canDel.Allowed {
			writeForbidden(w, "insufficient authority to modify super-admin role binding: "+canDel.Reason)
			return fmt.Errorf("CanDelegate denied: %s", canDel.Reason)
		}
	}

	return nil
}

// updateUserRole handles role changes through the canonical role-binding path.
// On success it mutates user.Role in-place (persisted inside the transaction).
// On failure it writes an HTTP error and returns a non-nil error.
//
// Authorization is checked by the caller (checkUserPromotePermission).
// This method handles self-lockout, last-admin, and atomic transaction.
func (s *Server) updateUserRole(
	ctx context.Context,
	w http.ResponseWriter,
	actor UserIdentity,
	user *store.User,
	newRole string,
) error {
	// Self-lockout guard: an admin cannot demote themselves.
	if user.Role == "admin" && newRole != "admin" && actor.ID() == user.ID {
		writeError(w, http.StatusConflict, ErrCodeConflict,
			"cannot demote yourself; ask another admin to change your role", nil)
		return fmt.Errorf("self-lockout")
	}

	isPromotion := newRole == "admin" && user.Role != "admin"
	isDemotion := newRole != "admin" && user.Role == "admin"

	// Resolve the super-admin role definition.
	superAdminRD, err := s.store.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"super-admin role definition not found", nil)
		return fmt.Errorf("super-admin role definition lookup: %w", err)
	}

	// Execute the role transition atomically inside a transaction.
	err = s.store.WithTx(ctx, func(tx store.Store) error {
		return s.executeRoleTransition(ctx, tx, user, newRole, superAdminRD, isPromotion, isDemotion)
	})
	if err != nil {
		if errors.Is(err, errLastSuperAdmin) {
			writeError(w, http.StatusConflict, ErrCodeConflict, err.Error(), nil)
		} else {
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
				"role transition failed: "+err.Error(), nil)
		}
		return err
	}

	return nil
}

// errLastSuperAdmin is returned when demoting the last super-admin would
// leave zero system-scoped super-admin bindings.
var errLastSuperAdmin = errors.New("cannot demote the last super-admin; promote another user first")

// executeRoleTransition performs the binding mutations and User.Role update
// inside a store transaction. All operations use the transactional store (tx).
func (s *Server) executeRoleTransition(
	ctx context.Context,
	tx store.Store,
	user *store.User,
	newRole string,
	superAdminRD *store.RoleDefinition,
	isPromotion, isDemotion bool,
) error {
	if isPromotion {
		if err := s.createSuperAdminBindingTx(ctx, tx, user.ID, superAdminRD); err != nil {
			return fmt.Errorf("create super-admin binding: %w", err)
		}
	}

	if isDemotion {
		if err := s.checkLastSuperAdminTx(ctx, tx, user.ID, superAdminRD); err != nil {
			return err
		}
		if err := s.deleteSuperAdminBindingTx(ctx, tx, user.ID, superAdminRD); err != nil {
			return fmt.Errorf("delete super-admin binding: %w", err)
		}
	}

	// Same-role repair: fix inconsistencies between User.Role and bindings.
	if !isPromotion && !isDemotion {
		if newRole == "admin" {
			if err := s.createSuperAdminBindingTx(ctx, tx, user.ID, superAdminRD); err != nil {
				return fmt.Errorf("repair super-admin binding: %w", err)
			}
		} else {
			_ = s.deleteSuperAdminBindingTx(ctx, tx, user.ID, superAdminRD)
		}
	}

	// Ensure hub-member binding on demotion or member role.
	if isDemotion || newRole == "member" {
		if err := s.ensureHubMemberBindingTx(ctx, tx, user.ID); err != nil {
			slog.Warn("failed to ensure hub-member binding", "user_id", user.ID, "error", err)
		}
	}

	// Update User.Role inside the same transaction.
	user.Role = newRole
	if err := tx.UpdateUser(ctx, user); err != nil {
		return fmt.Errorf("update User.Role: %w", err)
	}

	return nil
}

// createSuperAdminBindingTx idempotently creates a system-scoped super-admin
// role binding. Preserves SystemReconcileCreatedBy sentinel (D10 store guard).
func (s *Server) createSuperAdminBindingTx(
	ctx context.Context, tx store.Store,
	userID string, rd *store.RoleDefinition,
) error {
	_, err := tx.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
		ScopeID:          "",
		CreatedBy:        store.SystemReconcileCreatedBy,
	})
	if err != nil && errors.Is(err, store.ErrAlreadyExists) {
		return nil
	}
	return err
}

// deleteSuperAdminBindingTx removes all system-scoped super-admin role
// bindings for the given user. Returns error on any deletion failure.
func (s *Server) deleteSuperAdminBindingTx(
	ctx context.Context, tx store.Store,
	userID string, rd *store.RoleDefinition,
) error {
	bindings, err := tx.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
	if err != nil {
		return fmt.Errorf("list bindings for deletion: %w", err)
	}
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeSystem && b.RoleDefinitionID == rd.ID {
			if err := tx.DeleteRoleBinding(ctx, b.ID); err != nil {
				return fmt.Errorf("delete binding %s: %w", b.ID, err)
			}
			slog.Info("deleted super-admin binding via admin role mutation",
				"user_id", userID, "binding_id", b.ID)
		}
	}
	return nil
}

// checkLastSuperAdminTx verifies that removing the given user's super-admin
// binding would not leave zero active system-scoped super-admin bindings.
//
// R3 fix: ignores expired/scheduled bindings. Counts unique active direct
// user principals (deduplicated by PrincipalID) to prevent duplicate bindings
// from inflating the count.
func (s *Server) checkLastSuperAdminTx(
	ctx context.Context, tx store.Store,
	userID string, rd *store.RoleDefinition,
) error {
	systemBindings, err := tx.ListRoleBindingsForScope(ctx, store.RoleScopeSystem, "")
	if err != nil {
		return fmt.Errorf("failed to verify admin count: %w", err)
	}

	now := time.Now()
	activeAdminUsers := make(map[string]bool)
	var targetHasActiveBinding bool

	for _, b := range systemBindings {
		if b.RoleDefinitionID != rd.ID || b.PrincipalType != store.RoleBindingPrincipalUser {
			continue
		}
		// Skip expired bindings.
		if b.ExpiresAt != nil && now.After(*b.ExpiresAt) {
			continue
		}
		// Skip scheduled (not yet active) bindings.
		if b.NotBefore != nil && now.Before(*b.NotBefore) {
			continue
		}
		activeAdminUsers[b.PrincipalID] = true
		if b.PrincipalID == userID {
			targetHasActiveBinding = true
		}
	}

	if !targetHasActiveBinding {
		return nil
	}

	if len(activeAdminUsers) <= 1 {
		return errLastSuperAdmin
	}

	return nil
}

// ensureHubMemberBindingTx idempotently creates a system-scoped hub-member
// role binding for the given user.
func (s *Server) ensureHubMemberBindingTx(ctx context.Context, tx store.Store, userID string) error {
	hubMemberRD, err := tx.GetRoleDefinitionByName(ctx, store.SystemRoleHubMember, store.RoleScopeSystem)
	if err != nil {
		return fmt.Errorf("hub-member role definition lookup: %w", err)
	}
	_, err = tx.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: hubMemberRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
		ScopeID:          "",
		CreatedBy:        store.SystemReconcileCreatedBy,
	})
	if err != nil && errors.Is(err, store.ErrAlreadyExists) {
		return nil
	}
	return err
}

// ---------------------------------------------------------------------------
// DELETE /api/v1/users/{id} — user deletion (R3)
//
// Authorization: requires user.delete permission (super-admin only by default).
// Guards: self-deletion and last-effective-super-admin deletion are prevented.
// Cleanup + deletion are transactional as feasible. Session credential boundary
// is enforced via the authz service.
// ---------------------------------------------------------------------------

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	// Fail closed: authorization service must be available.
	if s.authzService == nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"authorization service unavailable", nil)
		return
	}

	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return
	}
	actor, ok := identity.(UserIdentity)
	if !ok {
		Forbidden(w)
		return
	}

	// Self-deletion guard.
	if actor.ID() == id {
		writeError(w, http.StatusConflict, ErrCodeConflict,
			"cannot delete your own account", nil)
		return
	}

	// Authorization: require user.delete permission.
	decision := s.authzService.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(actor),
		Credential: credentialContextForIdentity(actor),
		Resource:   Resource{Type: "user", ID: id},
		Action:     Action("delete"),
		Permission: "user.delete",
	})
	if !decision.Allowed {
		writeForbidden(w, "requires user.delete permission")
		return
	}

	// Load the target user (needed for last-admin check and audit).
	user, err := s.store.GetUser(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Last-admin guard: prevent deleting the last effective super-admin.
	if user.Role == "admin" {
		superAdminRD, err := s.store.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
		if err == nil {
			if err := s.checkLastSuperAdminTx(ctx, s.store, user.ID, superAdminRD); err != nil {
				writeError(w, http.StatusConflict, ErrCodeConflict,
					"cannot delete the last super-admin; promote another user first", nil)
				return
			}
		}
	}

	// Clean up user-scoped skill injections before deleting the user record.
	if n, err := s.store.DeleteSkillInjectionsByScope(ctx, store.SkillInjectionScopeUser, id); err != nil {
		slog.Warn("failed to delete user skill injections", "user_id", id, "error", err)
	} else if n > 0 {
		slog.Info("deleted user skill injections", "user_id", id, "count", n)
	}

	if err := s.store.DeleteUser(ctx, id); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Audit record.
	s.emitMutationAudit(ctx, &store.MutationAuditRecord{
		MutationType:  "user_delete",
		TargetType:    "user",
		TargetID:      id,
		BeforeSummary: fmt.Sprintf(`{"email":%q,"role":%q,"status":%q}`, user.Email, user.Role, user.Status),
	})

	w.WriteHeader(http.StatusNoContent)
}
