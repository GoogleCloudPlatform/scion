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
	"log/slog"
	"net/http"
	"strconv"

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

func (s *Server) updateUser(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

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

	var updates struct {
		DisplayName string                 `json:"displayName,omitempty"`
		Role        string                 `json:"role,omitempty"`
		Status      string                 `json:"status,omitempty"`
		Preferences *store.UserPreferences `json:"preferences,omitempty"`
	}

	if err := readJSON(r, &updates); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	// Validate role field eagerly — reject unsupported values even if the
	// role matches the current value (item 6: no bypass via generic user.update).
	if updates.Role != "" {
		switch updates.Role {
		case "admin", "member":
			// valid canonical roles
		default:
			BadRequest(w, fmt.Sprintf("unsupported role %q; valid values are \"admin\" and \"member\"", updates.Role))
			return
		}
	}

	// Role changes are handled through the canonical role-binding path.
	// This is separated from metadata updates so that a role mutation failure
	// does not prevent unrelated metadata changes, and vice versa.
	if updates.Role != "" {
		if err := s.updateUserRole(ctx, w, actor, user, updates.Role); err != nil {
			// updateUserRole writes its own HTTP error responses.
			return
		}
	}

	if updates.DisplayName != "" {
		user.DisplayName = updates.DisplayName
	}
	if updates.Status != "" {
		user.Status = updates.Status
	}
	if updates.Preferences != nil {
		user.Preferences = updates.Preferences
	}

	if err := s.store.UpdateUser(ctx, user); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, user)
}

// updateUserRole handles role changes through the canonical role-binding path.
// On success it mutates user.Role in-place (the caller persists via UpdateUser).
// On failure it writes an HTTP error and returns a non-nil error.
//
// All binding mutations + User.Role updates are executed inside a store
// transaction to prevent divergence between the compatibility field and the
// canonical role bindings. Binding creation/deletion errors are propagated
// (not swallowed) so that a binding failure produces an HTTP failure.
//
// The role field is validated by the caller; this method assumes newRole is
// already one of "admin" or "member".
func (s *Server) updateUserRole(
	ctx context.Context,
	w http.ResponseWriter,
	actor UserIdentity,
	user *store.User,
	newRole string,
) error {
	// Fail closed: if the authorization service is not available, refuse.
	if s.authzService == nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"authorization service unavailable", nil)
		return fmt.Errorf("authzService nil")
	}

	// Self-lockout guard: an admin cannot demote themselves.
	if user.Role == "admin" && newRole != "admin" && actor.ID() == user.ID {
		writeError(w, http.StatusConflict, ErrCodeConflict,
			"cannot demote yourself; ask another admin to change your role", nil)
		return fmt.Errorf("self-lockout")
	}

	// Determine the direction of the role change for authorization.
	isPromotion := newRole == "admin" && user.Role != "admin"
	isDemotion := newRole != "admin" && user.Role == "admin"

	// Resolve the super-admin role definition (needed for CanDelegate and binding ops).
	superAdminRD, err := s.store.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"super-admin role definition not found", nil)
		return fmt.Errorf("super-admin role definition lookup: %w", err)
	}

	// Authorization via CanDelegate: the actor must hold sufficient authority
	// to grant/revoke the super-admin role binding. CanDelegate enforces that
	// the actor holds all permissions in the target role (or is super-admin),
	// and respects credential scope boundaries (UAT cannot mutate system scope).
	if isPromotion || isDemotion {
		decision := s.authzService.CanDelegate(ctx, actor, GrantDescriptor{
			Type:             GrantTypeRoleBinding,
			RoleDefinitionID: superAdminRD.ID,
			ScopeType:        store.RoleScopeSystem,
		})
		if !decision.Allowed {
			writeForbidden(w, "insufficient authority to modify super-admin role binding: "+decision.Reason)
			return fmt.Errorf("CanDelegate denied: %s", decision.Reason)
		}
	}

	// For same-role requests, we still need role_binding.create to repair
	// inconsistencies. Check basic admin permission.
	if !isPromotion && !isDemotion && newRole == user.Role {
		decision := s.authzService.Decide(ctx, AuthzRequest{
			Principal:  principalContextForIdentity(actor),
			Credential: credentialContextForIdentity(actor),
			Resource:   Resource{Type: "role_binding", ID: "hub"},
			Action:     Action("manage"),
			Permission: "role_binding.create",
		})
		if !decision.Allowed {
			Forbidden(w)
			return fmt.Errorf("forbidden")
		}
	}

	// Execute the role transition atomically inside a transaction.
	err = s.store.WithTx(ctx, func(tx store.Store) error {
		return s.executeRoleTransition(ctx, tx, user, newRole, superAdminRD, isPromotion, isDemotion)
	})
	if err != nil {
		// Map specific errors to HTTP status codes.
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
		// Create super-admin binding (idempotent via ErrAlreadyExists).
		if err := s.createSuperAdminBindingTx(ctx, tx, user.ID, superAdminRD); err != nil {
			return fmt.Errorf("create super-admin binding: %w", err)
		}
	}

	if isDemotion {
		// Last-admin guard: count system-scoped super-admin bindings held by
		// direct user principals. Uses ListRoleBindingsForScope (filtered) —
		// never ListAllRoleBindings with Limit:0.
		if err := s.checkLastSuperAdminTx(ctx, tx, user.ID, superAdminRD); err != nil {
			return err
		}
		// Delete all super-admin bindings for this user.
		if err := s.deleteSuperAdminBindingTx(ctx, tx, user.ID, superAdminRD); err != nil {
			return fmt.Errorf("delete super-admin binding: %w", err)
		}
	}

	// Same-role repair (item 4): fix inconsistencies between User.Role and bindings.
	if !isPromotion && !isDemotion {
		if newRole == "admin" {
			// User.Role=admin but binding might be missing — repair.
			if err := s.createSuperAdminBindingTx(ctx, tx, user.ID, superAdminRD); err != nil {
				return fmt.Errorf("repair super-admin binding: %w", err)
			}
		} else {
			// User.Role=member but stale super-admin binding might exist — clean up.
			_ = s.deleteSuperAdminBindingTx(ctx, tx, user.ID, superAdminRD) // best-effort cleanup
		}
	}

	// On demotion, ensure the user has a hub-member binding so they retain
	// basic directory access (supplemental R2 contract).
	if isDemotion || newRole == "member" {
		if err := s.ensureHubMemberBindingTx(ctx, tx, user.ID); err != nil {
			slog.Warn("failed to ensure hub-member binding", "user_id", user.ID, "error", err)
			// Non-fatal: hub-member binding is supplementary.
		}
	}

	// Update the compatibility User.Role field inside the same transaction.
	user.Role = newRole
	if err := tx.UpdateUser(ctx, user); err != nil {
		return fmt.Errorf("update User.Role: %w", err)
	}

	return nil
}

// createSuperAdminBindingTx idempotently creates a system-scoped super-admin
// role binding using the transactional store. Preserves the
// SystemReconcileCreatedBy sentinel required by the store guard (D10).
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
		return nil // idempotent
	}
	return err
}

// deleteSuperAdminBindingTx removes all system-scoped super-admin role
// bindings for the given user using the transactional store. Returns an error
// if any deletion fails (does not swallow errors).
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
// binding would not leave zero system-scoped super-admin bindings. Uses
// ListRoleBindingsForScope to get only system-scoped bindings (not
// ListAllRoleBindings which may be paginated). Counts only direct user
// principals (not group bindings) since super-admin is direct-user-only.
//
// Returns errLastSuperAdmin if the user is the sole super-admin.
func (s *Server) checkLastSuperAdminTx(
	ctx context.Context, tx store.Store,
	userID string, rd *store.RoleDefinition,
) error {
	// Get all system-scoped bindings.
	systemBindings, err := tx.ListRoleBindingsForScope(ctx, store.RoleScopeSystem, "")
	if err != nil {
		return fmt.Errorf("failed to verify admin count: %w", err)
	}

	// Count direct user principals holding super-admin.
	var superAdminCount int
	var targetHasBinding bool
	for _, b := range systemBindings {
		if b.RoleDefinitionID == rd.ID && b.PrincipalType == store.RoleBindingPrincipalUser {
			superAdminCount++
			if b.PrincipalID == userID {
				targetHasBinding = true
			}
		}
	}

	// If the target user doesn't have a binding, demotion is safe
	// (nothing to remove).
	if !targetHasBinding {
		return nil
	}

	// If removing this user's binding would leave zero super-admins, block.
	if superAdminCount <= 1 {
		return errLastSuperAdmin
	}

	return nil
}

// ensureHubMemberBindingTx idempotently creates a system-scoped hub-member
// role binding for the given user. This ensures demoted users retain basic
// directory access. Uses SystemReconcileCreatedBy sentinel (hub-member is not
// guarded like super-admin but we use a consistent creator for auditability).
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
		return nil // idempotent
	}
	return err
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	// Clean up user-scoped skill injections before deleting the user record.
	// These rows have no FK cascade, so they must be removed explicitly.
	if n, err := s.store.DeleteSkillInjectionsByScope(ctx, store.SkillInjectionScopeUser, id); err != nil {
		slog.Warn("failed to delete user skill injections", "user_id", id, "error", err)
	} else if n > 0 {
		slog.Info("deleted user skill injections", "user_id", id, "count", n)
	}

	if err := s.store.DeleteUser(ctx, id); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
