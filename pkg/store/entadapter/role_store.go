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

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
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

// CreateRoleBinding creates a new role binding.
//
// Security invariant (D10): the super-admin role definition is bindable only
// by the system reconciler (CreatedBy == store.SystemReconcileCreatedBy).
// Any other caller receives ErrSuperAdminBindingRestricted. This guard lives
// at the store level so that every code path — current and future — that
// creates a role binding is covered.
func (r *RoleStore) CreateRoleBinding(ctx context.Context, rb *store.RoleBinding) (*store.RoleBinding, error) {
	rdUID, err := parseUUID(rb.RoleDefinitionID)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid role_definition_id: %s", store.ErrInvalidInput, rb.RoleDefinitionID)
	}

	// D10 guard: refuse super-admin bindings from non-reconciler callers.
	rd, err := r.client.RoleDefinition.Get(ctx, rdUID)
	if err != nil {
		return nil, fmt.Errorf("resolving role definition for binding guard: %w", mapError(err))
	}
	if rd.Name == store.SystemRoleSuperAdmin && !store.IsSuperAdminBindingAllowed(rb.CreatedBy) {
		return nil, store.ErrSuperAdminBindingRestricted
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
