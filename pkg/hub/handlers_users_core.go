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

	// Role changes are handled through the canonical role-binding path.
	if updates.Role != "" && updates.Role != user.Role {
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
// On success it mutates user.Role in-place (the caller persists).
// On failure it writes an HTTP error and returns a non-nil error.
func (s *Server) updateUserRole(
	ctx context.Context,
	w http.ResponseWriter,
	actor UserIdentity,
	user *store.User,
	newRole string,
) error {
	// Only "admin" and "member" are canonical roles backed by role bindings.
	// "viewer" has no distinct role definition; treat it as "member" if
	// requested but otherwise reject unknown values.
	switch newRole {
	case "admin", "member":
		// valid
	default:
		BadRequest(w, fmt.Sprintf("unsupported role %q; valid values are \"admin\" and \"member\"", newRole))
		return fmt.Errorf("unsupported role")
	}

	// Self-lockout guard: an admin cannot demote themselves.
	if user.Role == "admin" && newRole != "admin" && actor.ID() == user.ID {
		writeError(w, http.StatusConflict, ErrCodeConflict,
			"cannot demote yourself; ask another admin to change your role", nil)
		return fmt.Errorf("self-lockout")
	}

	// Authorization: role changes require role_binding.create at hub scope.
	if s.authzService != nil {
		decision := s.authzService.Decide(ctx, AuthzRequest{
			Principal:  principalContextForIdentity(actor),
			Credential: credentialContextForIdentity(actor),
			Resource:   Resource{Type: "role_binding", ID: "hub"},
			Action:     Action("create"),
			Permission: "role_binding.create",
		})
		if !decision.Allowed {
			Forbidden(w)
			return fmt.Errorf("forbidden")
		}
	}

	// Promotion: grant super-admin role binding.
	if newRole == "admin" && user.Role != "admin" {
		s.ensureSuperAdminBinding(ctx, user.ID)
		user.Role = "admin"
		return nil
	}

	// Demotion: revoke super-admin role binding.
	if newRole != "admin" && user.Role == "admin" {
		// Last-super-admin guard: refuse to demote if this is the last admin.
		if err := s.checkLastSuperAdmin(ctx, user.ID); err != nil {
			writeError(w, http.StatusConflict, ErrCodeConflict, err.Error(), nil)
			return err
		}
		s.deleteSuperAdminBinding(ctx, user.ID)
		user.Role = newRole
		return nil
	}

	// Same role — no-op for bindings, just sync the legacy field.
	user.Role = newRole
	return nil
}

// checkLastSuperAdmin returns an error if removing the given user's super-admin
// binding would leave zero system-scoped super-admin bindings.
func (s *Server) checkLastSuperAdmin(ctx context.Context, userID string) error {
	rd, err := s.store.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	if err != nil {
		// If the role definition doesn't exist, there can't be bindings to protect.
		return nil
	}

	// Count all system-scoped super-admin bindings.
	allBindings, err := s.store.ListAllRoleBindings(ctx, store.RoleBindingListOptions{Limit: 0})
	if err != nil {
		return fmt.Errorf("failed to verify admin count: %w", err)
	}

	var superAdminCount int
	for _, b := range allBindings {
		if b.ScopeType == store.RoleScopeSystem && b.RoleDefinitionID == rd.ID {
			superAdminCount++
		}
	}

	// If this user is the sole super-admin, block demotion.
	if superAdminCount <= 1 {
		// Verify this user actually has the binding (not just counting).
		userBindings, err := s.store.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
		if err != nil {
			return fmt.Errorf("failed to verify admin bindings: %w", err)
		}
		for _, b := range userBindings {
			if b.ScopeType == store.RoleScopeSystem && b.RoleDefinitionID == rd.ID {
				return errors.New("cannot demote the last super-admin; promote another user first")
			}
		}
	}
	return nil
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
