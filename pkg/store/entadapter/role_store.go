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

package entadapter

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/predicate"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/rolebinding"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/roledefinition"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// RoleStore implements store.RoleStore using Ent ORM.
type RoleStore struct {
	client *ent.Client
}

// NewRoleStore creates a new Ent-backed RoleStore.
func NewRoleStore(client *ent.Client) *RoleStore {
	return &RoleStore{client: client}
}

// entRoleDefinitionToStore converts an Ent RoleDefinition entity to a store.RoleDefinition model.
func entRoleDefinitionToStore(rd *ent.RoleDefinition) *store.RoleDefinition {
	return &store.RoleDefinition{
		ID:          rd.ID.String(),
		Name:        rd.Name,
		Description: rd.Description,
		ScopeType:   string(rd.ScopeType),
		Permissions: rd.Permissions,
		System:      rd.System,
		CreatedAt:   rd.Created,
		UpdatedAt:   rd.Updated,
	}
}

// entRoleBindingToStore converts an Ent RoleBinding entity to a store.RoleBinding model.
func entRoleBindingToStore(rb *ent.RoleBinding) *store.RoleBinding {
	result := &store.RoleBinding{
		ID:            rb.ID.String(),
		PrincipalType: string(rb.PrincipalType),
		PrincipalID:   rb.PrincipalID,
		ScopeType:     string(rb.ScopeType),
		ScopeID:       rb.ScopeID,
		NotBefore:     rb.NotBefore,
		ExpiresAt:     rb.ExpiresAt,
		CreatedBy:     rb.CreatedBy,
		CreatedAt:     rb.Created,
	}
	if rb.RoleDefinitionID != nil {
		result.RoleDefinitionID = rb.RoleDefinitionID.String()
	}
	return result
}

// GetRoleDefinition retrieves a role definition by ID.
func (r *RoleStore) GetRoleDefinition(ctx context.Context, id string) (*store.RoleDefinition, error) {
	uid, err := parseGetID(id)
	if err != nil {
		return nil, err
	}
	rd, err := r.client.RoleDefinition.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}
	return entRoleDefinitionToStore(rd), nil
}

// GetRoleDefinitionsByIDs retrieves role definitions by a list of IDs.
// Missing IDs are silently omitted from the result map.
func (r *RoleStore) GetRoleDefinitionsByIDs(ctx context.Context, ids []string) (map[string]*store.RoleDefinition, error) {
	if len(ids) == 0 {
		return map[string]*store.RoleDefinition{}, nil
	}
	uids := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		uid, err := uuid.Parse(id)
		if err != nil {
			continue
		}
		uids = append(uids, uid)
	}
	if len(uids) == 0 {
		return map[string]*store.RoleDefinition{}, nil
	}
	rds, err := r.client.RoleDefinition.Query().
		Where(roledefinition.IDIn(uids...)).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	result := make(map[string]*store.RoleDefinition, len(rds))
	for _, rd := range rds {
		srd := entRoleDefinitionToStore(rd)
		result[srd.ID] = srd
	}
	return result, nil
}

// GetRoleDefinitionByName retrieves a role definition by name and scope type.
func (r *RoleStore) GetRoleDefinitionByName(ctx context.Context, name string, scopeType string) (*store.RoleDefinition, error) {
	rd, err := r.client.RoleDefinition.Query().
		Where(
			roledefinition.NameEQ(name),
			roledefinition.ScopeTypeEQ(roledefinition.ScopeType(scopeType)),
		).
		Only(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return entRoleDefinitionToStore(rd), nil
}

// ListRoleDefinitions returns all role definitions.
func (r *RoleStore) ListRoleDefinitions(ctx context.Context) ([]*store.RoleDefinition, error) {
	rds, err := r.client.RoleDefinition.Query().
		Order(ent.Asc(roledefinition.FieldName)).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	result := make([]*store.RoleDefinition, len(rds))
	for i, rd := range rds {
		result[i] = entRoleDefinitionToStore(rd)
	}
	return result, nil
}

// CreateRoleDefinition creates a new role definition.
func (r *RoleStore) CreateRoleDefinition(ctx context.Context, rd *store.RoleDefinition) (*store.RoleDefinition, error) {
	builder := r.client.RoleDefinition.Create().
		SetName(rd.Name).
		SetScopeType(roledefinition.ScopeType(rd.ScopeType)).
		SetSystem(rd.System)

	if rd.ID != "" {
		uid, err := parseUUID(rd.ID)
		if err != nil {
			return nil, err
		}
		builder.SetID(uid)
	}
	if rd.Description != "" {
		builder.SetDescription(rd.Description)
	}
	if len(rd.Permissions) > 0 {
		builder.SetPermissions(rd.Permissions)
	}

	created, err := builder.Save(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return entRoleDefinitionToStore(created), nil
}

// UpdateRoleDefinition updates an existing role definition.
// System roles (System == true) cannot be updated.
func (r *RoleStore) UpdateRoleDefinition(ctx context.Context, rd *store.RoleDefinition) (*store.RoleDefinition, error) {
	uid, err := parseGetID(rd.ID)
	if err != nil {
		return nil, err
	}

	// Fetch existing to check system flag.
	existing, err := r.client.RoleDefinition.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}
	if existing.System {
		return nil, fmt.Errorf("%w: system roles cannot be modified", store.ErrInvalidInput)
	}

	builder := r.client.RoleDefinition.UpdateOneID(uid).
		SetName(rd.Name).
		SetDescription(rd.Description).
		SetPermissions(rd.Permissions)
	// ScopeType is intentionally not updatable.

	updated, err := builder.Save(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return entRoleDefinitionToStore(updated), nil
}

// UpdateSystemRoleDefinitionPermissions updates the permissions list of a
// system role definition. Unlike UpdateRoleDefinition, this method bypasses
// the system-role guard and is intended for startup backfill operations.
func (r *RoleStore) UpdateSystemRoleDefinitionPermissions(ctx context.Context, id string, permissions []string) error {
	uid, err := parseGetID(id)
	if err != nil {
		return err
	}

	_, err = r.client.RoleDefinition.UpdateOneID(uid).
		SetPermissions(permissions).
		Save(ctx)
	if err != nil {
		return mapError(err)
	}
	return nil
}

// DeleteRoleDefinition deletes a role definition by ID.
// System roles (System == true) cannot be deleted.
// Returns an error if any bindings reference the role.
func (r *RoleStore) DeleteRoleDefinition(ctx context.Context, id string) error {
	uid, err := parseGetID(id)
	if err != nil {
		return err
	}

	// Fetch existing to check system flag.
	existing, err := r.client.RoleDefinition.Get(ctx, uid)
	if err != nil {
		return mapError(err)
	}
	if existing.System {
		return fmt.Errorf("%w: system roles cannot be deleted", store.ErrInvalidInput)
	}

	// Check for active bindings referencing this role.
	count, err := r.client.RoleBinding.Query().
		Where(rolebinding.RoleDefinitionIDEQ(uid)).
		Count(ctx)
	if err != nil {
		return mapError(err)
	}
	if count > 0 {
		return fmt.Errorf("%w: role has %d active binding(s)", store.ErrInvalidInput, count)
	}

	if err := r.client.RoleDefinition.DeleteOneID(uid).Exec(ctx); err != nil {
		return mapError(err)
	}
	return nil
}

// ListAllRoleBindings returns all role bindings (admin view) with pagination.
// A limit of 0 defaults to 100. The maximum allowed limit is 1000.
func (r *RoleStore) ListAllRoleBindings(ctx context.Context, limit, offset int) ([]*store.RoleBinding, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	if offset < 0 {
		offset = 0
	}

	rbs, err := r.client.RoleBinding.Query().
		Order(ent.Desc(rolebinding.FieldCreated)).
		Limit(limit).
		Offset(offset).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	result := make([]*store.RoleBinding, len(rbs))
	for i, rb := range rbs {
		result[i] = entRoleBindingToStore(rb)
	}
	return result, nil
}

// CountAllRoleBindings returns the total number of role bindings.
func (r *RoleStore) CountAllRoleBindings(ctx context.Context) (int, error) {
	return r.client.RoleBinding.Query().Count(ctx)
}

// GetRoleBinding retrieves a role binding by ID.
func (r *RoleStore) GetRoleBinding(ctx context.Context, id string) (*store.RoleBinding, error) {
	uid, err := parseGetID(id)
	if err != nil {
		return nil, err
	}
	rb, err := r.client.RoleBinding.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}
	return entRoleBindingToStore(rb), nil
}

// ListRoleBindingsForPrincipal returns all role bindings for a given principal.
func (r *RoleStore) ListRoleBindingsForPrincipal(ctx context.Context, principalType, principalID string) ([]*store.RoleBinding, error) {
	rbs, err := r.client.RoleBinding.Query().
		Where(
			rolebinding.PrincipalTypeEQ(rolebinding.PrincipalType(principalType)),
			rolebinding.PrincipalIDEQ(principalID),
		).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	result := make([]*store.RoleBinding, len(rbs))
	for i, rb := range rbs {
		result[i] = entRoleBindingToStore(rb)
	}
	return result, nil
}

// ListRoleBindingsForScope returns all role bindings for a given scope.
func (r *RoleStore) ListRoleBindingsForScope(ctx context.Context, scopeType, scopeID string) ([]*store.RoleBinding, error) {
	rbs, err := r.client.RoleBinding.Query().
		Where(
			rolebinding.ScopeTypeEQ(rolebinding.ScopeType(scopeType)),
			rolebinding.ScopeIDEQ(scopeID),
		).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	result := make([]*store.RoleBinding, len(rbs))
	for i, rb := range rbs {
		result[i] = entRoleBindingToStore(rb)
	}
	return result, nil
}

// directUserOnlyRoles lists role names that require a direct user principal.
// Group and agent principals are not allowed for these roles.
var directUserOnlyRoles = map[string]struct{}{
	store.SystemRoleSuperAdmin: {},
	store.ProjectRoleOwner:     {},
}

// CreateRoleBinding creates a new role binding.
//
// Validation enforced:
//   - RoleDefinitionID must reference a valid role definition
//   - Scope type must match the role definition's scope type
//   - PrincipalType must be one of user/agent/group
//   - super-admin and project-owner bindings are direct-user-only
//   - super-admin is reconciler-only (D10)
//   - Duplicate (role, principal, scope) is rejected via unique index
//   - Lifecycle fields (NotBefore, ExpiresAt) are stored but not evaluated
func (r *RoleStore) CreateRoleBinding(ctx context.Context, rb *store.RoleBinding) (*store.RoleBinding, error) {
	// Validate principal type.
	switch rb.PrincipalType {
	case store.RoleBindingPrincipalUser, store.RoleBindingPrincipalAgent, store.RoleBindingPrincipalGroup:
		// valid
	default:
		return nil, fmt.Errorf("%w: invalid principal_type: %s", store.ErrInvalidInput, rb.PrincipalType)
	}

	rdUID, err := parseUUID(rb.RoleDefinitionID)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid role_definition_id: %s", store.ErrInvalidInput, rb.RoleDefinitionID)
	}

	// Resolve the role definition — required for scope matching and privilege checks.
	rd, err := r.client.RoleDefinition.Get(ctx, rdUID)
	if err != nil {
		return nil, fmt.Errorf("resolving role definition for binding guard: %w", mapError(err))
	}

	// Scope type must match the role definition's scope type.
	if rb.ScopeType != string(rd.ScopeType) {
		return nil, fmt.Errorf("%w: role %q requires scope %q but got %q",
			store.ErrScopeMismatch, rd.Name, string(rd.ScopeType), rb.ScopeType)
	}

	// D10 guard: refuse super-admin bindings from non-reconciler callers.
	if rd.Name == store.SystemRoleSuperAdmin && !store.IsSuperAdminBindingAllowed(rb.CreatedBy) {
		return nil, store.ErrSuperAdminBindingRestricted
	}

	// Direct-user-only guard: super-admin and project-owner reject non-user principals.
	if _, ok := directUserOnlyRoles[rd.Name]; ok && rb.PrincipalType != store.RoleBindingPrincipalUser {
		return nil, fmt.Errorf("%w: role %q is direct-user-only", store.ErrDirectUserOnly, rd.Name)
	}

	// Scope shape validation: project scope requires a non-empty ScopeID,
	// system scope requires an empty ScopeID.
	if rb.ScopeType == store.RoleScopeProject && rb.ScopeID == "" {
		return nil, fmt.Errorf("%w: project scope requires a non-empty scope_id", store.ErrInvalidInput)
	}
	if rb.ScopeType == store.RoleScopeSystem && rb.ScopeID != "" {
		return nil, fmt.Errorf("%w: system scope must have empty scope_id", store.ErrInvalidInput)
	}

	// Principal existence validation.
	principalUID, err := parseUUID(rb.PrincipalID)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid principal_id: %s", store.ErrInvalidInput, rb.PrincipalID)
	}
	switch rb.PrincipalType {
	case store.RoleBindingPrincipalUser:
		if _, err := r.client.User.Get(ctx, principalUID); err != nil {
			return nil, fmt.Errorf("%w: user %s", store.ErrNotFound, rb.PrincipalID)
		}
	case store.RoleBindingPrincipalAgent:
		if _, err := r.client.Agent.Get(ctx, principalUID); err != nil {
			return nil, fmt.Errorf("%w: agent %s", store.ErrNotFound, rb.PrincipalID)
		}
	case store.RoleBindingPrincipalGroup:
		if _, err := r.client.Group.Get(ctx, principalUID); err != nil {
			return nil, fmt.Errorf("%w: group %s", store.ErrNotFound, rb.PrincipalID)
		}
	}

	builder := r.client.RoleBinding.Create().
		SetNillableRoleDefinitionID(&rdUID).
		SetPrincipalType(rolebinding.PrincipalType(rb.PrincipalType)).
		SetPrincipalID(rb.PrincipalID).
		SetScopeType(rolebinding.ScopeType(rb.ScopeType)).
		SetScopeID(rb.ScopeID)

	if rb.ID != "" {
		uid, err := parseUUID(rb.ID)
		if err != nil {
			return nil, err
		}
		builder.SetID(uid)
	}
	if rb.CreatedBy != "" {
		builder.SetCreatedBy(rb.CreatedBy)
	}

	// Lifecycle fields — stored as-is; activation evaluation is the kernel's job.
	if rb.NotBefore != nil {
		builder.SetNotBefore(*rb.NotBefore)
	}
	if rb.ExpiresAt != nil {
		builder.SetExpiresAt(*rb.ExpiresAt)
	}

	created, err := builder.Save(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return entRoleBindingToStore(created), nil
}

// DeleteRoleBinding deletes a role binding by ID.
func (r *RoleStore) DeleteRoleBinding(ctx context.Context, id string) error {
	uid, err := parseGetID(id)
	if err != nil {
		return err
	}
	if err := r.client.RoleBinding.DeleteOneID(uid).Exec(ctx); err != nil {
		return mapError(err)
	}
	return nil
}

// ListRoleBindingsForPrincipals returns all role bindings matching any of the
// given principals, optionally filtered by scope type and scope ID.
// This is the authorization hot-path query — it uses IN clauses to resolve
// all bindings for the entire principal closure in a single query.
func (r *RoleStore) ListRoleBindingsForPrincipals(ctx context.Context, principals []store.PrincipalRef, scopeTypes []string, scopeIDs []string) ([]*store.RoleBinding, error) {
	if len(principals) == 0 {
		return nil, nil
	}

	// Build OR predicates for each principal in the closure.
	principalPreds := make([]predicate.RoleBinding, 0, len(principals))
	for _, p := range principals {
		principalPreds = append(principalPreds, rolebinding.And(
			rolebinding.PrincipalTypeEQ(rolebinding.PrincipalType(p.Type)),
			rolebinding.PrincipalIDEQ(p.ID),
		))
	}

	query := r.client.RoleBinding.Query().
		Where(rolebinding.Or(principalPreds...))

	// Optional scope type filter.
	if len(scopeTypes) > 0 {
		stPreds := make([]rolebinding.ScopeType, 0, len(scopeTypes))
		for _, st := range scopeTypes {
			stPreds = append(stPreds, rolebinding.ScopeType(st))
		}
		query = query.Where(rolebinding.ScopeTypeIn(stPreds...))
	}

	// Optional scope ID filter.
	// If scopeIDs filter is present, always include system-scoped bindings
	// (they use scope_id="" by convention; callers should not need to know this).
	if len(scopeIDs) > 0 {
		query = query.Where(rolebinding.Or(
			rolebinding.ScopeIDIn(scopeIDs...),
			rolebinding.ScopeTypeEQ(rolebinding.ScopeType("system")),
		))
	}

	rbs, err := query.All(ctx)
	if err != nil {
		return nil, mapError(err)
	}

	result := make([]*store.RoleBinding, len(rbs))
	for i, rb := range rbs {
		result[i] = entRoleBindingToStore(rb)
	}
	return result, nil
}

// DeleteRoleBindingsForPrincipal deletes all role bindings where the given
// principal is the bound principal. Returns the number of deleted bindings.
// Used for cascade delete when a group is deleted.
func (r *RoleStore) DeleteRoleBindingsForPrincipal(ctx context.Context, principalType, principalID string) (int, error) {
	if principalID == "" {
		return 0, fmt.Errorf("%w: principalID must not be empty", store.ErrInvalidInput)
	}
	n, err := r.client.RoleBinding.Delete().
		Where(
			rolebinding.PrincipalTypeEQ(rolebinding.PrincipalType(principalType)),
			rolebinding.PrincipalIDEQ(principalID),
		).
		Exec(ctx)
	if err != nil {
		return 0, mapError(err)
	}
	return n, nil
}

// DeleteRoleBindingsForScope deletes all role bindings scoped to the given
// scope type and ID. Returns the number of deleted bindings.
// Used for cascade delete when a project is deleted.
func (r *RoleStore) DeleteRoleBindingsForScope(ctx context.Context, scopeType, scopeID string) (int, error) {
	if scopeID == "" {
		return 0, fmt.Errorf("%w: scopeID must not be empty", store.ErrInvalidInput)
	}
	n, err := r.client.RoleBinding.Delete().
		Where(
			rolebinding.ScopeTypeEQ(rolebinding.ScopeType(scopeType)),
			rolebinding.ScopeIDEQ(scopeID),
		).
		Exec(ctx)
	if err != nil {
		return 0, mapError(err)
	}
	return n, nil
}

// GetProjectMembership returns the project membership for a user in a project.
// It looks up role bindings scoped to the project for the user and resolves
// the role definition name.
func (r *RoleStore) GetProjectMembership(ctx context.Context, projectID, userID string) (*store.ProjectMembership, error) {
	rbs, err := r.client.RoleBinding.Query().
		Where(
			rolebinding.PrincipalTypeEQ(rolebinding.PrincipalTypeUser),
			rolebinding.PrincipalIDEQ(userID),
			rolebinding.ScopeTypeEQ(rolebinding.ScopeTypeProject),
			rolebinding.ScopeIDEQ(projectID),
		).
		WithRoleDefinition().
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	if len(rbs) == 0 {
		return nil, store.ErrNotFound
	}

	// Return the highest-privilege binding (owner > admin > member).
	var best *ent.RoleBinding
	for _, rb := range rbs {
		if best == nil {
			best = rb
			continue
		}
		if roleRank(rb) > roleRank(best) {
			best = rb
		}
	}

	roleName := ""
	if best.Edges.RoleDefinition != nil {
		roleName = best.Edges.RoleDefinition.Name
	}
	return &store.ProjectMembership{
		ProjectID:     projectID,
		UserID:        userID,
		Role:          roleName,
		RoleBindingID: best.ID.String(),
	}, nil
}

// ListProjectMembers returns all members of a project via role bindings.
func (r *RoleStore) ListProjectMembers(ctx context.Context, projectID string) ([]*store.ProjectMembership, error) {
	rbs, err := r.client.RoleBinding.Query().
		Where(
			rolebinding.PrincipalTypeEQ(rolebinding.PrincipalTypeUser),
			rolebinding.ScopeTypeEQ(rolebinding.ScopeTypeProject),
			rolebinding.ScopeIDEQ(projectID),
		).
		WithRoleDefinition().
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}

	// Group by principal_id and pick highest-privilege binding per user.
	byUser := make(map[string]*ent.RoleBinding)
	for _, rb := range rbs {
		existing, ok := byUser[rb.PrincipalID]
		if !ok || roleRank(rb) > roleRank(existing) {
			byUser[rb.PrincipalID] = rb
		}
	}

	result := make([]*store.ProjectMembership, 0, len(byUser))
	for userID, rb := range byUser {
		roleName := ""
		if rb.Edges.RoleDefinition != nil {
			roleName = rb.Edges.RoleDefinition.Name
		}
		result = append(result, &store.ProjectMembership{
			ProjectID:     projectID,
			UserID:        userID,
			Role:          roleName,
			RoleBindingID: rb.ID.String(),
		})
	}
	return result, nil
}

// IsProjectMember returns true if the user has any project-scoped role binding
// for the given project.
func (r *RoleStore) IsProjectMember(ctx context.Context, projectID, userID string) (bool, error) {
	count, err := r.client.RoleBinding.Query().
		Where(
			rolebinding.PrincipalTypeEQ(rolebinding.PrincipalTypeUser),
			rolebinding.PrincipalIDEQ(userID),
			rolebinding.ScopeTypeEQ(rolebinding.ScopeTypeProject),
			rolebinding.ScopeIDEQ(projectID),
		).
		Count(ctx)
	if err != nil {
		return false, mapError(err)
	}
	return count > 0, nil
}

// roleRank returns a numeric ranking for role bindings based on the role
// definition name. Higher rank = more privilege.
func roleRank(rb *ent.RoleBinding) int {
	if rb.Edges.RoleDefinition == nil {
		return 0
	}
	switch rb.Edges.RoleDefinition.Name {
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
