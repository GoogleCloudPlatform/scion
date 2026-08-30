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
	"errors"
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
	Name               string                 `json:"name"`
	Subject            subjectSelectorRequest  `json:"subject"`
	Scope              constraintScopeRequest  `json:"scope"`
	MaximumPermissions []string               `json:"maximumPermissions"`
	Condition          *constraintConditionReq `json:"condition,omitempty"`
}

// updateAccessConstraintRequest is the payload for PATCH /api/v1/admin/access-constraints/:id.
type updateAccessConstraintRequest struct {
	Name               *string                 `json:"name,omitempty"`
	Subject            *subjectSelectorRequest  `json:"subject,omitempty"`
	Scope              *constraintScopeRequest  `json:"scope,omitempty"`
	MaximumPermissions []string                `json:"maximumPermissions,omitempty"`
	Condition          *constraintConditionReq  `json:"condition,omitempty"`
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
	if err := s.checkConstraintLockout(r, sc, false); err != nil {
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

	if len(req.MaximumPermissions) > 0 {
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
	if err := s.checkConstraintLockout(r, existing, false); err != nil {
		writeForbidden(w, "lockout prevention: "+err.Error())
		return
	}

	updated, err := s.store.UpdateAccessConstraint(r.Context(), existing)
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
// The check evaluates time conditions in their most restrictive state: a
// scheduled constraint that is not yet active is treated as if it were active,
// because it will eventually become active and the lockout check must account
// for that future state.
func (s *Server) checkConstraintLockout(r *http.Request, proposed *store.AccessConstraint, isDelete bool) error {
	// If the proposed constraint does not restrict the constraint-admin
	// permission, it cannot cause a lockout.
	if !isDelete {
		if constraintAllowsPermission(proposed, PermissionConstraintAdmin) {
			return nil
		}
	}

	// Load all constraints at this scope to compute the combined state.
	constraints, err := s.store.ListConstraintsForScope(r.Context(), proposed.ScopeType, proposed.ScopeID)
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
	if !found && !isDelete {
		constraints = append(constraints, proposed)
	}

	// Check if any constraint with an all_principals subject would remove
	// constraint-admin. This is the lockout scenario.
	now := time.Now()
	for _, c := range constraints {
		if c.Disabled {
			continue
		}

		// Check most restrictive state for time conditions.
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

		// Check if this constraint removes constraint-admin.
		if constraintAllowsPermission(c, PermissionConstraintAdmin) {
			continue // Constraint allows admin — no lockout risk from this one.
		}

		// This constraint removes constraint-admin. Check if it targets everyone.
		if c.SubjectKind == store.ConstraintSubjectAllPrincipals {
			return errors.New("constraint would remove constraint-admin permission from all principals at this scope; at least one direct user must retain it")
		}
	}

	return nil
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

// buildConstraintPreview builds a preview of the constraint's blast radius.
func (s *Server) buildConstraintPreview(r *http.Request, sc *store.AccessConstraint) *ConstraintPreview {
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

	// Resolve affected principals based on subject kind.
	switch sc.SubjectKind {
	case store.ConstraintSubjectPrincipal:
		if sc.SubjectPrincipalType != nil && sc.SubjectPrincipalID != nil {
			displayName := s.resolveGroupMemberDisplayName(r.Context(), *sc.SubjectPrincipalType, *sc.SubjectPrincipalID)
			preview.AffectedPrincipals = []AffectedPrincipal{
				{
					PrincipalType: *sc.SubjectPrincipalType,
					PrincipalID:   *sc.SubjectPrincipalID,
					DisplayName:   displayName,
				},
			}
		}
	case store.ConstraintSubjectGroupClosure:
		if sc.SubjectGroupID != nil {
			displayName := s.resolveGroupMemberDisplayName(r.Context(), "group", *sc.SubjectGroupID)
			preview.AffectedPrincipals = []AffectedPrincipal{
				{
					PrincipalType: "group",
					PrincipalID:   *sc.SubjectGroupID,
					DisplayName:   displayName + " (and all effective members)",
				},
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
	}

	return preview
}
