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
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ---------------------------------------------------------------------------
// Request / Response types
// ---------------------------------------------------------------------------

// createAccessConstraintRequest is the payload for POST /api/v1/admin/access-constraints.
type createAccessConstraintRequest struct {
	Name               string                  `json:"name"`
	Subject            subjectSelectorRequest  `json:"subject"`
	Scope              constraintScopeRequest  `json:"scope"`
	MaximumPermissions []string                `json:"maximumPermissions"`
	Condition          *constraintConditionReq `json:"condition,omitempty"`
}

// updateAccessConstraintRequest is the payload for PATCH /api/v1/admin/access-constraints/:id.
type updateAccessConstraintRequest struct {
	Name               *string                 `json:"name,omitempty"`
	Subject            *subjectSelectorRequest `json:"subject,omitempty"`
	Scope              *constraintScopeRequest `json:"scope,omitempty"`
	MaximumPermissions []string                `json:"maximumPermissions,omitempty"`
	Condition          *constraintConditionReq `json:"condition,omitempty"`
}

type subjectSelectorRequest struct {
	Kind          string `json:"kind"`
	PrincipalType string `json:"principalType,omitempty"`
	PrincipalID   string `json:"principalId,omitempty"`
	GroupID       string `json:"groupId,omitempty"`
}

type constraintScopeRequest struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`
}

type constraintConditionReq struct {
	NotBefore *time.Time `json:"notBefore,omitempty"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

// accessConstraintResponse is the API response for a single constraint.
type accessConstraintResponse struct {
	*store.AccessConstraint
	Preview *ConstraintPreview `json:"preview,omitempty"`
}

// listAccessConstraintsResponse wraps the list result for the API.
type listAccessConstraintsResponse struct {
	Items      []*store.AccessConstraint `json:"items"`
	TotalCount int                       `json:"totalCount"`
}

// ---------------------------------------------------------------------------
// Route handlers
// ---------------------------------------------------------------------------

// handleAdminAccessConstraints handles GET (list) and POST (create) on
// /api/v1/admin/access-constraints.
func (s *Server) handleAdminAccessConstraints(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listAccessConstraints(w, r)
	case http.MethodPost:
		user, ok := s.requireConstraintAdminPermission(w, r, PermissionConstraintAdmin, "create")
		if !ok {
			return
		}
		s.createAccessConstraint(w, r, user)
	default:
		MethodNotAllowed(w)
	}
}

// handleAdminAccessConstraintByID handles GET / PATCH / DELETE on
// /api/v1/admin/access-constraints/:id.
func (s *Server) handleAdminAccessConstraintByID(w http.ResponseWriter, r *http.Request) {
	id := extractID(r, "/api/v1/admin/access-constraints")
	if id == "" {
		BadRequest(w, "access constraint ID is required")
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.getAccessConstraint(w, r, id)
	case http.MethodPatch:
		user, ok := s.requireConstraintAdminPermission(w, r, PermissionConstraintAdmin, "update")
		if !ok {
			return
		}
		s.updateAccessConstraint(w, r, id, user)
	case http.MethodDelete:
		user, ok := s.requireConstraintAdminPermission(w, r, PermissionConstraintAdmin, "delete")
		if !ok {
			return
		}
		s.deleteAccessConstraint(w, r, id, user)
	default:
		MethodNotAllowed(w)
	}
}

// ---------------------------------------------------------------------------
// CRUD: Access Constraints
// ---------------------------------------------------------------------------

func (s *Server) listAccessConstraints(w http.ResponseWriter, r *http.Request) {
	limit, offset := parsePaginationParams(r)

	constraints, err := s.store.ListAccessConstraints(r.Context(), limit, offset)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if constraints == nil {
		constraints = []*store.AccessConstraint{}
	}

	total, err := s.store.CountAccessConstraints(r.Context())
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, listAccessConstraintsResponse{
		Items:      constraints,
		TotalCount: total,
	})
}

func (s *Server) getAccessConstraint(w http.ResponseWriter, r *http.Request, id string) {
	c, err := s.store.GetAccessConstraint(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Access Constraint")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// Build blast-radius preview.
	preview := s.buildConstraintPreview(r, c)

	writeJSON(w, http.StatusOK, accessConstraintResponse{
		AccessConstraint: c,
		Preview:          preview,
	})
}

func (s *Server) createAccessConstraint(w http.ResponseWriter, r *http.Request, user UserIdentity) {
	var req createAccessConstraintRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "invalid request body: "+err.Error())
		return
	}

	// Validate required fields.
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		BadRequest(w, "name is required")
		return
	}

	// Validate subject.
	subject := SubjectSelector{
		Kind:          SubjectKind(req.Subject.Kind),
		PrincipalType: req.Subject.PrincipalType,
		PrincipalID:   req.Subject.PrincipalID,
		GroupID:       req.Subject.GroupID,
	}
	if err := subject.Validate(); err != nil {
		BadRequest(w, "invalid subject: "+err.Error())
		return
	}

	// Validate scope.
	scope := ConstraintScopeRef{
		Type: req.Scope.Type,
		ID:   req.Scope.ID,
	}
	if err := scope.Validate(); err != nil {
		BadRequest(w, "invalid scope: "+err.Error())
		return
	}

	// Validate maximum permissions.
	if len(req.MaximumPermissions) == 0 {
		BadRequest(w, "maximumPermissions must contain at least one permission")
		return
	}
	if err := validatePermissionIDs(req.MaximumPermissions); err != nil {
		BadRequest(w, err.Error())
		return
	}

	// Build store model.
	sc := &store.AccessConstraint{
		Name:               req.Name,
		SubjectKind:        string(subject.Kind),
		ScopeType:          scope.Type,
		ScopeID:            scope.ID,
		MaximumPermissions: req.MaximumPermissions,
		CreatedBy:          user.ID(),
	}

	// Set subject fields based on kind.
	switch subject.Kind {
	case SubjectKindPrincipal:
		sc.SubjectPrincipalType = &subject.PrincipalType
		sc.SubjectPrincipalID = &subject.PrincipalID
	case SubjectKindGroupClosure:
		sc.SubjectGroupID = &subject.GroupID
	}

	// Set condition (time window).
	if req.Condition != nil {
		sc.NotBefore = req.Condition.NotBefore
		sc.ExpiresAt = req.Condition.ExpiresAt
	}

	// Lockout prevention: after this constraint is created, at least one active
	// direct user must retain constraint-admin permission at this scope.
	if err := s.checkConstraintLockout(r, sc); err != nil {
		writeForbidden(w, "lockout prevention: "+err.Error())
		return
	}

	created, err := s.store.CreateAccessConstraint(r.Context(), sc)
	if err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			Conflict(w, "an access constraint with this name and scope already exists")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	slog.Info("access constraint created",
		"constraint_id", created.ID, "name", created.Name, "actor", user.Email())

	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) updateAccessConstraint(w http.ResponseWriter, r *http.Request, id string, user UserIdentity) {
	var req updateAccessConstraintRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "invalid request body: "+err.Error())
		return
	}

	// Fetch existing.
	existing, err := s.store.GetAccessConstraint(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Access Constraint")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// Apply updates.
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			BadRequest(w, "name cannot be empty")
			return
		}
		existing.Name = name
	}

	if req.Subject != nil {
		subject := SubjectSelector{
			Kind:          SubjectKind(req.Subject.Kind),
			PrincipalType: req.Subject.PrincipalType,
			PrincipalID:   req.Subject.PrincipalID,
			GroupID:       req.Subject.GroupID,
		}
		if err := subject.Validate(); err != nil {
			BadRequest(w, "invalid subject: "+err.Error())
			return
		}
		existing.SubjectKind = string(subject.Kind)
		switch subject.Kind {
		case SubjectKindPrincipal:
			existing.SubjectPrincipalType = &subject.PrincipalType
			existing.SubjectPrincipalID = &subject.PrincipalID
			existing.SubjectGroupID = nil
		case SubjectKindGroupClosure:
			existing.SubjectGroupID = &subject.GroupID
			existing.SubjectPrincipalType = nil
			existing.SubjectPrincipalID = nil
		case SubjectKindAllPrincipals:
			existing.SubjectPrincipalType = nil
			existing.SubjectPrincipalID = nil
			existing.SubjectGroupID = nil
		}
	}

	if req.Scope != nil {
		scope := ConstraintScopeRef{
			Type: req.Scope.Type,
			ID:   req.Scope.ID,
		}
		if err := scope.Validate(); err != nil {
			BadRequest(w, "invalid scope: "+err.Error())
			return
		}
		existing.ScopeType = scope.Type
		existing.ScopeID = scope.ID
	}

	if req.MaximumPermissions != nil {
		if len(req.MaximumPermissions) == 0 {
			BadRequest(w, "maximumPermissions must contain at least one permission")
			return
		}
		if err := validatePermissionIDs(req.MaximumPermissions); err != nil {
			BadRequest(w, err.Error())
			return
		}
		existing.MaximumPermissions = req.MaximumPermissions
	}

	if req.Condition != nil {
		existing.NotBefore = req.Condition.NotBefore
		existing.ExpiresAt = req.Condition.ExpiresAt
	}

	// Lockout prevention check.
	if err := s.checkConstraintLockout(r, existing); err != nil {
		writeForbidden(w, "lockout prevention: "+err.Error())
		return
	}

	updated, err := s.store.UpdateAccessConstraint(r.Context(), existing, 0)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Access Constraint")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	slog.Info("access constraint updated",
		"constraint_id", updated.ID, "name", updated.Name, "actor", user.Email())

	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) deleteAccessConstraint(w http.ResponseWriter, r *http.Request, id string, user UserIdentity) {
	// Verify it exists.
	existing, err := s.store.GetAccessConstraint(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Access Constraint")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	if err := s.store.DeleteAccessConstraint(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Access Constraint")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	slog.Info("access constraint deleted",
		"constraint_id", existing.ID, "name", existing.Name, "actor", user.Email())

	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Authorization helpers
// ---------------------------------------------------------------------------

// requireConstraintAdminPermission checks that the authenticated user has
// the specified constraint administration permission.
func (s *Server) requireConstraintAdminPermission(w http.ResponseWriter, r *http.Request, permission string, action string) (UserIdentity, bool) {
	identity := GetIdentityFromContext(r.Context())
	if identity == nil {
		writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "authentication required", nil)
		return nil, false
	}
	user, ok := identity.(UserIdentity)
	if !ok {
		Forbidden(w)
		return nil, false
	}
	if s.authzService == nil {
		Forbidden(w)
		return nil, false
	}
	decision := s.authzService.Decide(r.Context(), AuthzRequest{
		Principal:  principalContextForIdentity(user),
		Credential: credentialContextForIdentity(user),
		Resource:   Resource{Type: "access_constraint", ID: "hub"},
		Action:     Action(action),
		Permission: permission,
	})
	if !decision.Allowed {
		Forbidden(w)
		return nil, false
	}
	return user, true
}

// ---------------------------------------------------------------------------
// Governance: lockout prevention
// ---------------------------------------------------------------------------

// checkConstraintLockout verifies that after applying the proposed constraint,
// at least one active direct user retains constraint-admin permission at the
// relevant scope.
//
// The algorithm:
//  1. Resolve all direct users who currently hold access_constraint.admin at
//     this scope via role bindings (including group-expanded bindings).
//  2. Load all constraints at this scope, merge in the proposed change.
//  3. For each admin user, simulate the full constraint set against that
//     user's principal closure. If any constraint (in its most-restrictive
//     time state) would remove access_constraint.admin, the user is blocked.
//  4. If at least one admin user survives unconstrained, the operation is
//     allowed. If none survive, reject.
func (s *Server) checkConstraintLockout(r *http.Request, proposed *store.AccessConstraint) error {
	ctx := r.Context()

	// Fast path: if the proposed constraint allows constraint-admin, it
	// cannot cause a lockout by itself. We still need to check combined
	// state though — another existing constraint might already be blocking
	// admin, and this change could close the last remaining gap.
	// However, if the proposed constraint allows admin AND is a new creation
	// (ID is empty), it strictly cannot make things worse. Skip.
	if proposed.ID == "" && constraintAllowsPermission(proposed, PermissionConstraintAdmin) {
		return nil
	}

	// Step 1: Find all direct users with constraint-admin at this scope.
	adminUsers, err := s.resolveConstraintAdminUsers(ctx, proposed.ScopeType, proposed.ScopeID)
	if err != nil {
		slog.Warn("lockout check: failed to resolve admin users", "error", err)
		return errors.New("failed to resolve constraint admin users for lockout check")
	}

	if len(adminUsers) == 0 {
		// No admin users found — this is already a degraded state.
		// Allow the operation rather than blocking everything.
		slog.Warn("lockout check: no constraint admin users found at scope",
			"scopeType", proposed.ScopeType, "scopeID", proposed.ScopeID)
		return nil
	}

	// Step 2: Load all constraints at this scope and merge proposed change.
	constraints, err := s.store.ListConstraintsForScope(ctx, proposed.ScopeType, proposed.ScopeID)
	if err != nil {
		return errors.New("failed to load existing constraints for lockout check")
	}

	// Add or update the proposed constraint in the list.
	found := false
	for i, c := range constraints {
		if c.ID == proposed.ID {
			constraints[i] = proposed
			found = true
			break
		}
	}
	if !found {
		constraints = append(constraints, proposed)
	}

	// Filter to constraints that would be active in most-restrictive state
	// and that remove constraint-admin.
	now := time.Now()
	var restrictingConstraints []*store.AccessConstraint
	for _, c := range constraints {
		if c.Disabled {
			continue
		}
		condition := ConstraintCondition{}
		if c.NotBefore != nil {
			condition.NotBefore = *c.NotBefore
		}
		if c.ExpiresAt != nil {
			condition.ExpiresAt = *c.ExpiresAt
		}
		if !condition.IsActiveInMostRestrictiveState(now) {
			continue
		}
		if constraintAllowsPermission(c, PermissionConstraintAdmin) {
			continue // This constraint does not restrict admin.
		}
		restrictingConstraints = append(restrictingConstraints, c)
	}

	// If no constraints restrict admin, no lockout is possible.
	if len(restrictingConstraints) == 0 {
		return nil
	}

	// Step 3: For each admin user, check if any restricting constraint
	// applies to them. If ALL admin users are blocked, reject.
	for _, au := range adminUsers {
		if !s.userBlockedByConstraints(ctx, au, restrictingConstraints) {
			return nil // At least one admin user survives.
		}
	}

	// Step 4: All admin users are blocked.
	return errors.New("constraint would remove constraint-admin permission from all users who currently hold it at this scope; at least one direct user must retain it")
}

// adminUserInfo holds identity data for a user with constraint-admin.
type adminUserInfo struct {
	userID   string
	groupIDs []string // effective group membership (for group_closure matching)
}

// resolveConstraintAdminUsers finds all direct users who hold the
// access_constraint.admin permission at the given scope via role bindings.
func (s *Server) resolveConstraintAdminUsers(ctx context.Context, scopeType, scopeID string) ([]adminUserInfo, error) {
	// Get all role bindings at this scope.
	bindings, err := s.store.ListRoleBindingsForScope(ctx, scopeType, scopeID)
	if err != nil {
		return nil, fmt.Errorf("list role bindings for scope: %w", err)
	}

	// Also include system-scope bindings if we're checking a project scope,
	// because system-scoped roles apply everywhere.
	if scopeType == ScopeTypeProject {
		sysBindings, err := s.store.ListRoleBindingsForScope(ctx, ScopeTypeSystem, "")
		if err != nil {
			return nil, fmt.Errorf("list system role bindings: %w", err)
		}
		bindings = append(bindings, sysBindings...)
	}

	// Resolve which role definitions grant constraint-admin.
	roleDefs, err := s.store.ListRoleDefinitions(ctx)
	if err != nil {
		return nil, fmt.Errorf("list role definitions: %w", err)
	}
	adminRoleIDs := make(map[string]bool)
	for _, rd := range roleDefs {
		for _, p := range rd.Permissions {
			if p == PermissionConstraintAdmin {
				adminRoleIDs[rd.ID] = true
				break
			}
		}
	}

	// Collect direct user principals with admin role bindings.
	// Also track groups that have admin bindings (for group-expanded resolution).
	directUserIDs := make(map[string]bool)
	adminGroupIDs := make(map[string]bool)
	for _, b := range bindings {
		if !adminRoleIDs[b.RoleDefinitionID] {
			continue
		}
		switch b.PrincipalType {
		case store.RoleBindingPrincipalUser:
			directUserIDs[b.PrincipalID] = true
		case store.RoleBindingPrincipalGroup:
			adminGroupIDs[b.PrincipalID] = true
		}
	}

	// Expand group memberships to find users who get admin via groups.
	for gid := range adminGroupIDs {
		members, err := s.store.GetGroupMembers(ctx, gid)
		if err != nil {
			slog.Warn("lockout check: failed to get group members", "groupID", gid, "error", err)
			continue
		}
		for _, m := range members {
			if m.MemberType == store.GroupMemberTypeUser {
				directUserIDs[m.MemberID] = true
			}
		}
	}

	// Build admin user info with group closure for each user.
	var result []adminUserInfo
	for uid := range directUserIDs {
		groupIDs, err := s.store.GetEffectiveGroups(ctx, uid)
		if err != nil {
			slog.Warn("lockout check: failed to get effective groups for user",
				"userID", uid, "error", err)
			groupIDs = nil
		}
		result = append(result, adminUserInfo{
			userID:   uid,
			groupIDs: groupIDs,
		})
	}

	return result, nil
}

// userBlockedByConstraints returns true if all of the given restricting
// constraints (which remove constraint-admin) apply to this user.
// The user is blocked if ANY restricting constraint matches them.
func (s *Server) userBlockedByConstraints(_ context.Context, user adminUserInfo, constraints []*store.AccessConstraint) bool {
	// Build the user's principal closure: user ID + all group IDs.
	closure := make(map[string]struct{}, 1+len(user.groupIDs))
	closure[user.userID] = struct{}{}
	for _, gid := range user.groupIDs {
		closure[gid] = struct{}{}
	}

	for _, c := range constraints {
		switch c.SubjectKind {
		case store.ConstraintSubjectAllPrincipals:
			return true // all_principals blocks everyone
		case store.ConstraintSubjectPrincipal:
			if c.SubjectPrincipalType != nil && *c.SubjectPrincipalType == "user" &&
				c.SubjectPrincipalID != nil && *c.SubjectPrincipalID == user.userID {
				return true
			}
			// A principal-kind constraint targeting a group also blocks users
			// whose closure includes that group.
			if c.SubjectPrincipalType != nil && *c.SubjectPrincipalType == "group" &&
				c.SubjectPrincipalID != nil {
				if _, ok := closure[*c.SubjectPrincipalID]; ok {
					return true
				}
			}
		case store.ConstraintSubjectGroupClosure:
			if c.SubjectGroupID != nil {
				if _, ok := closure[*c.SubjectGroupID]; ok {
					return true
				}
			}
		}
	}

	return false
}

// constraintAllowsPermission checks whether a constraint's maximum permissions
// include the given permission.
func constraintAllowsPermission(c *store.AccessConstraint, permissionID string) bool {
	for _, p := range c.MaximumPermissions {
		if p == permissionID {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Blast-radius preview
// ---------------------------------------------------------------------------

// maxPreviewMembers caps group_closure expansion to avoid expensive queries.
const maxPreviewMembers = 50

// buildConstraintPreview builds a preview of the constraint's blast radius,
// including per-principal permission deltas.
func (s *Server) buildConstraintPreview(r *http.Request, sc *store.AccessConstraint) *ConstraintPreview {
	ctx := r.Context()

	maxPerms := make(map[string]struct{}, len(sc.MaximumPermissions))
	for _, p := range sc.MaximumPermissions {
		maxPerms[p] = struct{}{}
	}

	// Find permissions that would be restricted (permissions NOT in the
	// maximum set).
	var restricted []string
	for _, p := range permissions.Registry {
		if _, ok := maxPerms[p.ID]; !ok {
			restricted = append(restricted, p.ID)
		}
	}

	preview := &ConstraintPreview{
		ConstraintID:          sc.ID,
		ConstraintName:        sc.Name,
		RestrictedPermissions: restricted,
	}

	// Resolve affected principals and compute per-principal permission deltas.
	switch sc.SubjectKind {
	case store.ConstraintSubjectPrincipal:
		if sc.SubjectPrincipalType != nil && sc.SubjectPrincipalID != nil {
			ap := s.buildAffectedPrincipal(ctx, *sc.SubjectPrincipalType, *sc.SubjectPrincipalID,
				sc.ScopeType, sc.ScopeID, maxPerms)
			preview.AffectedPrincipals = []AffectedPrincipal{ap}
		}

	case store.ConstraintSubjectGroupClosure:
		if sc.SubjectGroupID != nil {
			// Include the group entity itself.
			groupDisplayName := s.resolveGroupMemberDisplayName(ctx, "group", *sc.SubjectGroupID)
			groupEntry := AffectedPrincipal{
				PrincipalType: "group",
				PrincipalID:   *sc.SubjectGroupID,
				DisplayName:   groupDisplayName + " (group closure)",
			}
			preview.AffectedPrincipals = []AffectedPrincipal{groupEntry}

			// Expand group members and compute per-member deltas.
			members, err := s.store.GetGroupMembers(ctx, *sc.SubjectGroupID)
			if err != nil {
				slog.Warn("preview: failed to get group members",
					"groupID", *sc.SubjectGroupID, "error", err)
				break
			}

			count := 0
			for _, m := range members {
				if m.MemberType != store.GroupMemberTypeUser {
					continue
				}
				if count >= maxPreviewMembers {
					preview.Truncated = true
					break
				}
				ap := s.buildAffectedPrincipal(ctx, m.MemberType, m.MemberID,
					sc.ScopeType, sc.ScopeID, maxPerms)
				preview.AffectedPrincipals = append(preview.AffectedPrincipals, ap)
				count++
			}
		}

	case store.ConstraintSubjectAllPrincipals:
		preview.AffectedPrincipals = []AffectedPrincipal{
			{
				PrincipalType: "all",
				PrincipalID:   "*",
				DisplayName:   "All principals",
			},
		}
		preview.Truncated = true
	}

	return preview
}

// buildAffectedPrincipal resolves a single principal's effective permissions
// at the given scope and computes the permission delta against the constraint.
func (s *Server) buildAffectedPrincipal(
	ctx context.Context,
	principalType, principalID string,
	scopeType, scopeID string,
	maxPerms map[string]struct{},
) AffectedPrincipal {
	displayName := s.resolveGroupMemberDisplayName(ctx, principalType, principalID)
	ap := AffectedPrincipal{
		PrincipalType: principalType,
		PrincipalID:   principalID,
		DisplayName:   displayName,
	}

	// Resolve effective permissions via the authz service.
	if s.authzService == nil {
		return ap
	}

	currentPerms, err := s.authzService.getEffectivePermissions(ctx, principalType, principalID, scopeType, scopeID)
	if err != nil {
		slog.Warn("preview: failed to resolve effective permissions",
			"principalType", principalType, "principalID", principalID, "error", err)
		return ap
	}

	ap.CurrentPermissions = currentPerms

	// Compute proposed (intersection with constraint) and removed (set difference).
	var proposed, removed []string
	for _, p := range currentPerms {
		if _, ok := maxPerms[p]; ok {
			proposed = append(proposed, p)
		} else {
			removed = append(removed, p)
		}
	}
	ap.ProposedPermissions = proposed
	ap.RemovedPermissions = removed

	return ap
}
