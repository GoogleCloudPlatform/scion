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
// Credential boundary enforcement (R4-C5)
//
// User mutation endpoints (PATCH, DELETE) require an interactive session JWT
// or dev credential. Broker, agent, UAT, and federation credentials are
// rejected at the boundary.
// ---------------------------------------------------------------------------

// allowedMutationCredentials is the closed set of credential kinds permitted
// for user mutation endpoints.
var allowedMutationCredentials = map[CredentialKind]bool{
	CredentialKindInteractive: true,
	CredentialKindDev:         true,
}

// requireSessionCredential verifies the request uses an interactive session
// JWT or dev credential. Returns the actor UserIdentity on success, writes
// an HTTP error and returns nil on failure.
func (s *Server) requireSessionCredential(w http.ResponseWriter, ctx context.Context) (UserIdentity, bool) {
	if s.authzService == nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"authorization service unavailable", nil)
		return nil, false
	}

	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return nil, false
	}

	actor, ok := identity.(UserIdentity)
	if !ok {
		Forbidden(w)
		return nil, false
	}

	// Enforce credential boundary: only interactive session JWTs and dev
	// tokens are allowed for user mutations.
	cred := GetCredentialContextFromContext(ctx)
	if !allowedMutationCredentials[cred.Kind] {
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			fmt.Sprintf("user mutations require an interactive session; credential kind %q is not allowed", cred.Kind), nil)
		return nil, false
	}

	return actor, true
}

// ---------------------------------------------------------------------------
// PATCH /api/v1/users/{id} — per-field permission enforcement (R3/R4)
//
// Each mutable field category requires a distinct permission:
//   - role:                 user.promote + CanDelegate(super-admin)
//   - status:               user.suspend
//   - displayName/prefs:    user.update (self-service for own record)
//
// A mixed PATCH must hold ALL required permissions before any write.
// Unknown JSON fields are rejected. ALL mutations (bindings + User record +
// audit) execute in a single atomic transaction (R4-C1). Audit records are
// synchronous and transactional (R4-C3).
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

	// Credential boundary + identity check (R4-C5).
	actor, ok := s.requireSessionCredential(w, ctx)
	if !ok {
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

	needsPromote := updates.Role != nil
	needsSuspend := updates.Status != nil
	needsUpdate := updates.DisplayName != nil || updates.Preferences != nil

	isSelf := actor.ID() == user.ID
	needsCrossUserUpdate := needsUpdate && !isSelf

	// Pre-resolve super-admin role definition and canonical binding state
	// before authorization. Canonical state comes from bindings, not User.Role,
	// to prevent stale-state bypass. Both active AND any-lifecycle states are
	// queried: CanDelegate requires HasAny, self-lockout requires HasActive
	// (R4-fix lifecycle).
	var superAdminRD *store.RoleDefinition
	var preAuthBindingState superAdminBindingState
	if needsPromote {
		superAdminRD, err = s.store.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
		if err != nil {
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
				"super-admin role definition not found", nil)
			return
		}
		preAuthBindingState, err = s.superAdminBindingStateForUser(ctx, s.store, user.ID, superAdminRD)
		if err != nil {
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
				"failed to check canonical binding state", nil)
			return
		}
	}

	if needsPromote {
		if err := s.checkUserPromotePermission(ctx, w, actor, user, *updates.Role, preAuthBindingState); err != nil {
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
			writeForbiddenStructured(w, "requires user.suspend permission", "user", Action("suspend"))
			return
		}
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
			writeForbiddenStructured(w, "requires user.update permission to modify another user's profile", "user", Action("update"))
			return
		}
	}

	// Self-lockout guard: uses canonical binding state, not User.Role (R4-fix).
	// Only applies when an ACTIVE binding exists — removing a scheduled-only
	// binding on yourself is not self-demotion of current authority.
	if needsPromote && preAuthBindingState.HasActive && *updates.Role != "admin" && isSelf {
		writeError(w, http.StatusConflict, ErrCodeConflict,
			"cannot demote yourself; ask another admin to change your role", nil)
		return
	}

	// ── Execute ALL mutations in a single atomic transaction (R4-C1) ──

	// Build actor audit metadata outside the transaction.
	auditActor := s.buildAuditActorFromContext(ctx)

	err = s.store.WithTx(ctx, func(tx store.Store) error {
		// Re-read user inside the transaction for consistency.
		txUser, err := tx.GetUser(ctx, id)
		if err != nil {
			return fmt.Errorf("re-read user in tx: %w", err)
		}

		// Capture canonical before-state from the transactional read for
		// truthful audit records and change detection (R4-fix: not stale pre-tx).
		beforeRole := txUser.Role
		beforeStatus := txUser.Status

		// Role transition: derives classification from canonical binding state
		// inside the transaction, not from User.Role (R4-fix).
		var bindingMutation bindingMutationKind
		if needsPromote {
			var err error
			bindingMutation, err = s.executeRoleTransition(ctx, tx, txUser, *updates.Role, superAdminRD, actor.ID(), preAuthBindingState)
			if err != nil {
				return err
			}
		}

		// Status change.
		if updates.Status != nil {
			txUser.Status = *updates.Status
		}

		// Profile metadata.
		if updates.DisplayName != nil {
			txUser.DisplayName = *updates.DisplayName
		}
		if updates.Preferences != nil {
			txUser.Preferences = updates.Preferences
		}

		// Persist all User record changes in the same transaction.
		if needsSuspend || needsUpdate || needsPromote {
			if err := tx.UpdateUser(ctx, txUser); err != nil {
				return fmt.Errorf("persist user changes: %w", err)
			}
		}

		// Synchronous audit records (R4-C3): written inside the transaction
		// so they roll back if the transaction fails. beforeRole/beforeStatus
		// come from the in-tx re-read for truthful audit (R4-fix).
		//
		// Audit fires on EITHER a User.Role change OR a binding mutation,
		// so same-role canonical repairs (creating a missing binding or
		// removing a stale one) are always recorded (R4-fix audit).
		if needsPromote && (beforeRole != txUser.Role || bindingMutation != bindingMutationNone) {
			mutationType := "user_role_change"
			if beforeRole == txUser.Role && bindingMutation != bindingMutationNone {
				// Same-role but binding changed — use a repair-specific type.
				mutationType = "user_role_binding_" + string(bindingMutation)
			}
			if err := tx.CreateMutationAudit(ctx, &store.MutationAuditRecord{
				MutationType:        mutationType,
				ActorPrincipalKind:  auditActor.kind,
				ActorPrincipalID:    auditActor.id,
				ActorCredentialID:   auditActor.credID,
				ActorCredentialType: auditActor.credType,
				TargetType:          "user",
				TargetID:            txUser.ID,
				BeforeSummary:       fmt.Sprintf(`{"role":%q,"binding":%q}`, beforeRole, bindingMutation),
				AfterSummary:        fmt.Sprintf(`{"role":%q}`, txUser.Role),
				Timestamp:           time.Now(),
			}); err != nil {
				return fmt.Errorf("audit role change: %w", err)
			}
		}

		if needsSuspend && beforeStatus != txUser.Status {
			mutationType := "user_suspend"
			if txUser.Status == "active" {
				mutationType = "user_reactivate"
			}
			if err := tx.CreateMutationAudit(ctx, &store.MutationAuditRecord{
				MutationType:        mutationType,
				ActorPrincipalKind:  auditActor.kind,
				ActorPrincipalID:    auditActor.id,
				ActorCredentialID:   auditActor.credID,
				ActorCredentialType: auditActor.credType,
				TargetType:          "user",
				TargetID:            txUser.ID,
				BeforeSummary:       fmt.Sprintf(`{"status":%q}`, beforeStatus),
				AfterSummary:        fmt.Sprintf(`{"status":%q}`, txUser.Status),
				Timestamp:           time.Now(),
			}); err != nil {
				return fmt.Errorf("audit status change: %w", err)
			}
		}

		// Copy the transactional state back for the response.
		*user = *txUser
		return nil
	})

	if err != nil {
		if errors.Is(err, errLastSuperAdmin) || errors.Is(err, errSelfDemotion) {
			writeError(w, http.StatusConflict, ErrCodeConflict, err.Error(), nil)
		} else if errors.Is(err, errBindingStateDrift) {
			writeError(w, http.StatusConflict, "binding_state_drift", err.Error(), nil)
		} else {
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
				"user update failed: "+err.Error(), nil)
		}
		return
	}

	writeJSON(w, http.StatusOK, user)
}

// auditActorInfo holds pre-resolved actor metadata for audit records.
type auditActorInfo struct {
	kind     string
	id       string
	credID   string
	credType string
}

// buildAuditActorFromContext extracts actor identity and credential metadata
// from the request context for use in transactional audit records.
func (s *Server) buildAuditActorFromContext(ctx context.Context) auditActorInfo {
	var info auditActorInfo
	if identity := GetIdentityFromContext(ctx); identity != nil {
		info.kind = identity.Type()
		info.id = identity.ID()
	}
	cred := GetCredentialContextFromContext(ctx)
	if cred.Kind != "" {
		info.credID = cred.ID
		info.credType = string(cred.Kind)
	}
	return info
}

// superAdminBindingState describes the lifecycle state of a user's super-admin
// bindings. Used by pre-auth and in-tx classification to handle all lifecycle
// states correctly (R4-fix lifecycle).
type superAdminBindingState struct {
	// HasAny is true when ANY matching binding exists (active, scheduled, or expired).
	// Used for CanDelegate determination: any involvement with super-admin bindings
	// requires delegation authority.
	HasAny bool
	// HasActive is true when an ACTIVE binding exists (within its valid time window).
	// Used for self-lockout guard (only when removing active authority) and
	// last-admin count (only active users with active bindings count).
	HasActive bool
}

// superAdminBindingStateForUser queries both the active and any-lifecycle
// binding state for a user's super-admin bindings.
func (s *Server) superAdminBindingStateForUser(
	ctx context.Context, st store.Store,
	userID string, rd *store.RoleDefinition,
) (superAdminBindingState, error) {
	bindings, err := st.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
	if err != nil {
		return superAdminBindingState{}, fmt.Errorf("list bindings for user: %w", err)
	}
	now := time.Now()
	var state superAdminBindingState
	for _, b := range bindings {
		if b.ScopeType != store.RoleScopeSystem || b.RoleDefinitionID != rd.ID {
			continue
		}
		state.HasAny = true
		// Check if this particular binding is currently active.
		if b.ExpiresAt != nil && now.After(*b.ExpiresAt) {
			continue // expired
		}
		if b.NotBefore != nil && now.Before(*b.NotBefore) {
			continue // scheduled, not yet active
		}
		state.HasActive = true
	}
	return state, nil
}

// checkUserPromotePermission verifies the actor has user.promote + CanDelegate
// authority for the role transition. Writes HTTP error and returns non-nil on failure.
func (s *Server) checkUserPromotePermission(
	ctx context.Context, w http.ResponseWriter,
	actor UserIdentity, user *store.User, newRole string,
	bindingState superAdminBindingState,
) error {
	decision := s.authzService.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(actor),
		Credential: credentialContextForIdentity(actor),
		Resource:   Resource{Type: "user", ID: user.ID},
		Action:     Action("promote"),
		Permission: "user.promote",
	})
	if !decision.Allowed {
		writeForbiddenStructured(w, "requires user.promote permission", "user", Action("promote"))
		return fmt.Errorf("user.promote denied")
	}

	// Determine if this operation involves super-admin bindings.
	// CanDelegate is required whenever the target has ANY binding (active,
	// scheduled, or expired) or the new role is admin. This prevents the
	// stale-state bypass AND ensures scheduled/expired bindings cannot be
	// silently manipulated without delegation authority (R4-fix lifecycle).
	wantsBinding := newRole == "admin"
	involvesSuper := bindingState.HasAny || wantsBinding

	if involvesSuper {
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
			writeForbiddenStructured(w, "insufficient authority to modify super-admin role binding: "+canDel.Reason, "role_binding", Action("create"))
			return fmt.Errorf("CanDelegate denied: %s", canDel.Reason)
		}
	}

	return nil
}

// errLastSuperAdmin is returned when demoting/deleting the last active
// super-admin would leave zero authenticatable system-scoped super-admin users.
var errLastSuperAdmin = errors.New("cannot remove the last super-admin; promote another user first")

// errSelfDemotion is returned when the transactional canonical-state recheck
// detects the actor is removing their own super-admin binding. This catches
// the TOCTOU window between the pre-tx self-lockout guard and the in-tx
// binding mutation.
var errSelfDemotion = errors.New("cannot demote yourself; ask another admin to change your role")

// bindingMutationKind describes what happened to super-admin bindings during
// a role transition. Used for truthful audit records even when User.Role
// doesn't change (R4-fix audit).
type bindingMutationKind string

const (
	bindingMutationNone      bindingMutationKind = ""
	bindingMutationCreated   bindingMutationKind = "grant"   // new active binding created
	bindingMutationRevoked   bindingMutationKind = "revoke"  // active binding removed
	bindingMutationRepaired  bindingMutationKind = "repair"  // stale replaced with active
	bindingMutationCleanedUp bindingMutationKind = "cleanup" // expired/scheduled removed, no active existed
)

// errBindingStateDrift is returned when the canonical binding state inside the
// transaction differs from the pre-authorization check in a way that would
// bypass CanDelegate. The caller must retry (R6 TOCTOU fix).
var errBindingStateDrift = errors.New("binding state changed since authorization check; retry the operation")

// executeRoleTransition performs the binding mutations inside a store
// transaction. Classification is derived from canonical binding state (whether
// super-admin bindings exist and their lifecycle state) inside the transaction,
// NOT from User.Role which may be stale.
//
// Lifecycle handling (R4-fix lifecycle):
//   - member: removes ALL matching bindings (active, scheduled, expired)
//   - admin: ensures exactly one active binding exists; stale rows are
//     deleted then a fresh active binding is created
//
// TOCTOU safety (R6): preAuthState records the binding state that was
// authorized before the transaction. If the in-tx state has bindings that
// weren't present at preauth (txState.HasAny && !preAuthState.HasAny), the
// operation is rejected to prevent concurrent promotion from creating a
// binding that is then silently removed without CanDelegate verification.
//
// Returns what binding mutation occurred for truthful audit (R4-fix audit).
func (s *Server) executeRoleTransition(
	ctx context.Context,
	tx store.Store,
	user *store.User,
	newRole string,
	superAdminRD *store.RoleDefinition,
	actorID string,
	preAuthState superAdminBindingState,
) (bindingMutationKind, error) {
	// Determine canonical binding state inside the transaction (R4-fix).
	txState, err := s.superAdminBindingStateForUser(ctx, tx, user.ID, superAdminRD)
	if err != nil {
		return bindingMutationNone, fmt.Errorf("check canonical binding state in tx: %w", err)
	}

	// TOCTOU guard (R6): if the in-tx state has super-admin bindings that
	// weren't present at pre-authorization time, a concurrent operation
	// created them between the preauth check and this transaction. The
	// CanDelegate check may not have covered this new state, so fail
	// closed rather than silently mutating bindings without authorization.
	if txState.HasAny && !preAuthState.HasAny {
		return bindingMutationNone, errBindingStateDrift
	}

	wantsBinding := newRole == "admin"
	mutation := bindingMutationNone

	switch {
	case wantsBinding && !txState.HasActive:
		// Need an active binding. If stale (expired/scheduled) bindings exist,
		// delete them first to avoid unique constraint conflicts, then create
		// a fresh active binding (R4-fix lifecycle).
		if txState.HasAny {
			if err := s.deleteSuperAdminBindingTx(ctx, tx, user.ID, superAdminRD); err != nil {
				return bindingMutationNone, fmt.Errorf("clean up stale bindings: %w", err)
			}
			mutation = bindingMutationRepaired
		} else {
			mutation = bindingMutationCreated
		}
		if err := s.createSuperAdminBindingTx(ctx, tx, user.ID, superAdminRD); err != nil {
			return bindingMutationNone, fmt.Errorf("create super-admin binding: %w", err)
		}

	case !wantsBinding && txState.HasAny:
		// Remove ALL super-admin bindings (active + scheduled + expired).
		// Governance guards only apply when an active binding is being removed.
		if txState.HasActive {
			// Self-lockout re-check inside tx (catches TOCTOU).
			if user.ID == actorID {
				return bindingMutationNone, errSelfDemotion
			}
			if err := s.checkLastSuperAdminTx(ctx, tx, user.ID, superAdminRD); err != nil {
				return bindingMutationNone, err
			}
			mutation = bindingMutationRevoked
		} else {
			// Only expired/scheduled bindings — no governance needed, just cleanup.
			mutation = bindingMutationCleanedUp
		}
		if err := s.deleteSuperAdminBindingTx(ctx, tx, user.ID, superAdminRD); err != nil {
			return bindingMutationNone, fmt.Errorf("delete super-admin binding: %w", err)
		}

	case wantsBinding && txState.HasActive:
		// Active binding already exists — idempotent. No mutation to audit.
		mutation = bindingMutationNone

		// !wantsBinding && !txState.HasAny: nothing to do.
	}

	// Ensure hub-member binding for members.
	if !wantsBinding {
		if err := s.ensureHubMemberBindingTx(ctx, tx, user.ID); err != nil {
			return bindingMutationNone, fmt.Errorf("ensure hub-member binding: %w", err)
		}
	}

	user.Role = newRole
	return mutation, nil
}

// createSuperAdminBindingTx idempotently creates a system-scoped super-admin
// role binding. Uses SystemReconcileCreatedBy sentinel (D10 store guard).
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
// bindings for the given user.
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
// binding would not leave zero authenticatable system-scoped super-admin users.
//
// Concurrency: acquires a SELECT FOR UPDATE lock on the super-admin role
// definition row before reading bindings. This serializes concurrent
// demotion/deletion transactions that both try to verify the last-admin
// invariant, preventing the READ COMMITTED race where two demotions both
// observe the other admin and both commit (R4-fix concurrency).
//
// R4-C4: resolves each unique binding principal to a User record and counts
// only users with status "active". Suspended/invited users are not counted as
// surviving admins. Fails closed on user lookup errors.
func (s *Server) checkLastSuperAdminTx(
	ctx context.Context, tx store.Store,
	userID string, rd *store.RoleDefinition,
) error {
	// Acquire serialization lock on the role definition row to prevent
	// concurrent last-admin checks from racing (R4-fix concurrency).
	if err := tx.LockRoleDefinitionForAdminGuard(ctx, rd.ID); err != nil {
		return fmt.Errorf("acquire admin-guard lock: %w", err)
	}

	systemBindings, err := tx.ListRoleBindingsForScope(ctx, store.RoleScopeSystem, "")
	if err != nil {
		return fmt.Errorf("failed to verify admin count: %w", err)
	}

	now := time.Now()
	candidateUserIDs := make(map[string]bool)
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
		candidateUserIDs[b.PrincipalID] = true
		if b.PrincipalID == userID {
			targetHasActiveBinding = true
		}
	}

	if !targetHasActiveBinding {
		return nil
	}

	// Resolve each candidate to verify they are active (can authenticate).
	// Only users with status "active" count as surviving admins (R4-C4).
	activeAdminCount := 0
	for uid := range candidateUserIDs {
		if uid == userID {
			continue // exclude the user being removed
		}
		u, err := tx.GetUser(ctx, uid)
		if err != nil {
			// Fail closed: if we can't verify a user exists and is active,
			// don't count them as a surviving admin.
			slog.Warn("checkLastSuperAdminTx: failed to resolve binding principal",
				"principal_id", uid, "error", err)
			continue
		}
		if u.Status == "active" {
			activeAdminCount++
		}
	}

	if activeAdminCount == 0 {
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
// DELETE /api/v1/users/{id} — user deletion (R3/R4)
//
// Authorization: requires user.delete permission via session credential.
// Guards: self-deletion and last-active-super-admin are prevented based on
// bindings (not User.Role). All operations — last-admin check, skill cleanup,
// user deletion, and audit — execute in a single atomic transaction (R4-C2).
// ---------------------------------------------------------------------------

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	// Credential boundary + identity check (R4-C5).
	actor, ok := s.requireSessionCredential(w, ctx)
	if !ok {
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
		writeForbiddenStructured(w, "requires user.delete permission", "user", Action("delete"))
		return
	}

	// Load the target user (needed for binding-based last-admin check).
	user, err := s.store.GetUser(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Resolve super-admin role definition (needed for binding-based check).
	superAdminRD, err := s.store.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"super-admin role definition not found", nil)
		return
	}

	auditActor := s.buildAuditActorFromContext(ctx)

	// Execute everything in a single atomic transaction (R4-C2).
	err = s.store.WithTx(ctx, func(tx store.Store) error {
		// Last-admin guard based on bindings, not User.Role (R4-C2).
		if err := s.checkLastSuperAdminTx(ctx, tx, user.ID, superAdminRD); err != nil {
			return err
		}

		// Clean up user-scoped skill injections.
		if _, err := tx.DeleteSkillInjectionsByScope(ctx, store.SkillInjectionScopeUser, id); err != nil {
			return fmt.Errorf("delete skill injections: %w", err)
		}

		// Delete the user record.
		if err := tx.DeleteUser(ctx, id); err != nil {
			return fmt.Errorf("delete user: %w", err)
		}

		// Synchronous audit record (R4-C3).
		if err := tx.CreateMutationAudit(ctx, &store.MutationAuditRecord{
			MutationType:        "user_delete",
			ActorPrincipalKind:  auditActor.kind,
			ActorPrincipalID:    auditActor.id,
			ActorCredentialID:   auditActor.credID,
			ActorCredentialType: auditActor.credType,
			TargetType:          "user",
			TargetID:            id,
			BeforeSummary:       fmt.Sprintf(`{"email":%q,"role":%q,"status":%q}`, user.Email, user.Role, user.Status),
			Timestamp:           time.Now(),
		}); err != nil {
			return fmt.Errorf("audit delete: %w", err)
		}

		return nil
	})

	if err != nil {
		if errors.Is(err, errLastSuperAdmin) {
			writeError(w, http.StatusConflict, ErrCodeConflict,
				"cannot delete the last super-admin; promote another user first", nil)
		} else {
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
				"user deletion failed: "+err.Error(), nil)
		}
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
