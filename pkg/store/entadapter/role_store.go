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
	"errors"
	"fmt"
	"time"

	"entgo.io/ent/dialect"
	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/predicate"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/projectvisitorrule"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/rolebinding"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/roledefinition"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// RoleStore implements store.RoleStore using Ent ORM.
type RoleStore struct {
	client *ent.Client
	// now is the clock the project membership reads evaluate binding
	// activation windows against. It is the Go wall clock, the same clock
	// the authorization kernel is given; tests replace it.
	now func() time.Time
}

// NewRoleStore creates a new Ent-backed RoleStore.
func NewRoleStore(client *ent.Client) *RoleStore {
	return &RoleStore{client: client, now: time.Now}
}

// roleBindingActiveAt reports whether rb's activation window contains now.
// It applies the authorization kernel's rule (evaluateActivation in
// pkg/hub/authz_kernel.go): an unset bound is unbounded, NotBefore is
// inclusive (active when now >= NotBefore) and ExpiresAt is exclusive
// (active when now < ExpiresAt). A zero now with any bound set is
// inactive, as in the kernel. A nil binding is never active.
func roleBindingActiveAt(rb *ent.RoleBinding, now time.Time) bool {
	if rb == nil {
		return false
	}
	if rb.NotBefore == nil && rb.ExpiresAt == nil {
		return true
	}
	if now.IsZero() {
		return false
	}
	if rb.NotBefore != nil && now.Before(*rb.NotBefore) {
		return false
	}
	if rb.ExpiresAt != nil && !now.Before(*rb.ExpiresAt) {
		return false
	}
	return true
}

// activeRoleBindings returns the bindings in rbs whose activation window
// contains the store clock's current time. A RoleStore built without
// NewRoleStore has no clock set and uses time.Now. The result is a new
// slice; rbs is not modified.
func (r *RoleStore) activeRoleBindings(rbs []*ent.RoleBinding) []*ent.RoleBinding {
	nowFn := r.now
	if nowFn == nil {
		nowFn = time.Now
	}
	now := nowFn()
	active := rbs[:0:0]
	for _, rb := range rbs {
		if roleBindingActiveAt(rb, now) {
			active = append(active, rb)
		}
	}
	return active
}

// entRoleDefinitionToStore converts an Ent RoleDefinition entity to a store.RoleDefinition model.
func entRoleDefinitionToStore(rd *ent.RoleDefinition) *store.RoleDefinition {
	confers := rd.ConfersMembership
	return &store.RoleDefinition{
		ID:                rd.ID.String(),
		Name:              rd.Name,
		Description:       rd.Description,
		ScopeType:         string(rd.ScopeType),
		Permissions:       rd.Permissions,
		System:            rd.System,
		CreatedAt:         rd.Created,
		UpdatedAt:         rd.Updated,
		ConfersMembership: &confers,
		IssuanceID:        rd.IssuanceID,
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
		IssuanceID:    rb.IssuanceID,
	}
	if rb.RoleDefinitionID != nil {
		result.RoleDefinitionID = rb.RoleDefinitionID.String()
	}
	if rb.SourceRuleID != nil {
		ruleID := rb.SourceRuleID.String()
		result.SourceRuleID = &ruleID
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
//
// A role defined by an admin-kind test identity carries its issuance ID
// (grant shaping), so the identity's purge removes it; it can never be a
// system role.
func (r *RoleStore) CreateRoleDefinition(ctx context.Context, rd *store.RoleDefinition) (*store.RoleDefinition, error) {
	shaping, err := grantShapingFor(ctx, r.client)
	if err != nil {
		return nil, err
	}
	if shaping != nil && rd.System {
		return nil, store.ErrTestAdminGrantRefused
	}
	builder := r.client.RoleDefinition.Create().
		SetName(rd.Name).
		SetScopeType(roledefinition.ScopeType(rd.ScopeType)).
		SetSystem(rd.System).
		SetNillableIssuanceID(issuanceStamp(shaping))

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
	// An unset ConfersMembership keeps the schema default (true).
	if rd.ConfersMembership != nil {
		builder.SetConfersMembership(*rd.ConfersMembership)
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
	// The grant shaping check and the update run in one transaction (the
	// caller's, inside store.WithTx).
	var out *store.RoleDefinition
	err = withEntTx(ctx, r.client, func(c *ent.Client) error {
		var uerr error
		out, uerr = updateRoleDefinition(ctx, c, uid, rd)
		return uerr
	})
	return out, err
}

// updateRoleDefinition is UpdateRoleDefinition on client c. Under shaping
// (an admin-kind test identity, admin tier A3) only a role stamped with the
// same issuance may change, and only while no role binding without that
// stamp names it: a change to any other role, or to a role a person's
// binding names, could not be bounded by the identity's expiry.
func updateRoleDefinition(ctx context.Context, c *ent.Client, uid uuid.UUID, rd *store.RoleDefinition) (*store.RoleDefinition, error) {
	existing, err := c.RoleDefinition.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}
	if existing.System {
		return nil, fmt.Errorf("%w: system roles cannot be modified", store.ErrInvalidInput)
	}
	shaping, err := grantShapingFor(ctx, c)
	if err != nil {
		return nil, err
	}
	if shaping != nil {
		if !shapedByIssuance(shaping, existing.IssuanceID) {
			return nil, store.ErrTestAdminGrantRefused
		}
		boundOutside, err := c.RoleBinding.Query().
			Where(rolebinding.RoleDefinitionIDEQ(uid), rolebinding.Or(
				rolebinding.IssuanceIDIsNil(), rolebinding.IssuanceIDNEQ(shaping.IssuanceID))).
			Exist(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		if boundOutside {
			return nil, store.ErrTestAdminGrantRefused
		}
	}

	builder := c.RoleDefinition.UpdateOneID(uid).
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

// withEntTx runs fn in a transaction on client, or on client itself when
// client is already transactional (inside store.WithTx).
func withEntTx(ctx context.Context, client *ent.Client, fn func(*ent.Client) error) error {
	tx, err := client.Tx(ctx)
	if err != nil {
		if errors.Is(err, ent.ErrTxStarted) {
			return fn(client)
		}
		return err
	}
	if err := fn(tx.Client()); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// UpdateSystemRoleDefinitionPermissions updates the permissions list of a
// system role definition. Unlike UpdateRoleDefinition, this method bypasses
// the system-role guard and is intended for startup backfill operations.
func (r *RoleStore) UpdateSystemRoleDefinitionPermissions(ctx context.Context, id string, permissions []string) error {
	uid, err := parseGetID(id)
	if err != nil {
		return err
	}
	// Startup backfill only: never under an admin-kind test identity.
	shaping, err := grantShapingFor(ctx, r.client)
	if err != nil {
		return err
	}
	if shaping != nil {
		return store.ErrTestAdminGrantRefused
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

// LockRoleDefinitionForAdminGuard acquires a serialization lock on the role
// definition row. On PostgreSQL this runs SELECT id FROM role_definitions
// WHERE id = $1 FOR UPDATE, serializing concurrent last-admin guard checks.
// On SQLite all writes are database-serialized, so a plain existence check
// provides the same ordering guarantee.
func (r *RoleStore) LockRoleDefinitionForAdminGuard(ctx context.Context, roleDefinitionID string) error {
	uid, err := parseGetID(roleDefinitionID)
	if err != nil {
		return err
	}

	q := r.client.RoleDefinition.Query().Where(roledefinition.IDEQ(uid))
	if r.client.Driver().Dialect() == dialect.Postgres {
		q = q.ForUpdate()
	}

	exists, err := q.Exist(ctx)
	if err != nil {
		return fmt.Errorf("lock role definition for admin guard: %w", mapError(err))
	}
	if !exists {
		return store.ErrNotFound
	}
	return nil
}

// ListAllRoleBindings returns all role bindings (admin view) with pagination
// and configurable sort order.  The primary sort is determined by
// opts.SortBy (default: created); a secondary sort by scope_type then scope_id
// is always appended, followed by a final tie-breaker on id for determinism.
func (r *RoleStore) ListAllRoleBindings(ctx context.Context, opts store.RoleBindingListOptions) ([]*store.RoleBinding, error) {
	if opts.Limit <= 0 {
		opts.Limit = 100
	}
	if opts.Limit > 1000 {
		opts.Limit = 1000
	}
	if opts.Offset < 0 {
		opts.Offset = 0
	}

	// Determine the Ent direction helper.
	dir := ent.Desc // default for "created"
	switch opts.SortOrder {
	case "asc":
		dir = ent.Asc
	case "desc":
		dir = ent.Desc
	}

	// Map the primary sort field.
	primaryField := rolebinding.FieldCreated // default
	switch opts.SortBy {
	case store.RoleBindingSortPrincipal:
		primaryField = rolebinding.FieldPrincipalID
	case store.RoleBindingSortRole:
		primaryField = rolebinding.FieldRoleDefinitionID
	case store.RoleBindingSortCreated:
		primaryField = rolebinding.FieldCreated
	}

	rbs, err := r.client.RoleBinding.Query().
		Order(
			dir(primaryField),
			ent.Asc(rolebinding.FieldScopeType),
			ent.Asc(rolebinding.FieldScopeID),
			ent.Asc(rolebinding.FieldID),
		).
		Limit(opts.Limit).
		Offset(opts.Offset).
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

// checkTestAdminSystemBinding refuses a system-scoped binding on an
// admin-kind test identity unless it is the issuance binding: created by
// the issuance path, with an expiry that does not outlive the identity. A
// missing expiry is refused, so no system authority on an admin-kind row
// can be permanent (CreateTestAdminUser is the only intended writer).
//
// The expiries are compared at their stored precision (testAdminExpiry),
// and for an allowed issuance binding the expiry to store is returned at
// that precision, so the stored binding never outlives the stored
// identity on either backend. It is nil for any other binding.
//
// The issuance binding's role must be the kind's role (test_hub_admin:
// hub-admin, test_super_admin: super-admin), and a super-admin binding with
// the issuance CreatedBy is admitted for no other principal.
func checkTestAdminSystemBinding(u *ent.User, rb *store.RoleBinding, roleName string) (*time.Time, error) {
	if roleName == store.SystemRoleSuperAdmin && rb.CreatedBy == store.TestIdentityIssuanceCreatedBy &&
		string(u.Kind) != store.UserKindTestSuperAdmin {
		return nil, store.ErrSuperAdminBindingRestricted
	}
	if rb.ScopeType != store.RoleScopeSystem || !store.IsTestAdminKind(string(u.Kind)) {
		return nil, nil
	}
	if rb.CreatedBy == store.TestIdentityIssuanceCreatedBy && roleName != testAdminKindSystemRole(string(u.Kind)) {
		return nil, fmt.Errorf("%w: a %s test identity's issuance binding must hold the %s role", store.ErrInvalidInput, u.Kind, testAdminKindSystemRole(string(u.Kind)))
	}
	if rb.CreatedBy != store.TestIdentityIssuanceCreatedBy {
		return nil, fmt.Errorf("%w: an admin-kind test identity holds only its issuance binding at system scope", store.ErrInvalidInput)
	}
	if rb.ExpiresAt == nil || u.ExpiresAt == nil {
		return nil, fmt.Errorf("%w: an admin-kind test identity's system binding must expire no later than the identity", store.ErrInvalidInput)
	}
	exp := testAdminExpiry(*rb.ExpiresAt)
	if exp.After(testAdminExpiry(*u.ExpiresAt)) {
		return nil, fmt.Errorf("%w: an admin-kind test identity's system binding must expire no later than the identity", store.ErrInvalidInput)
	}
	return &exp, nil
}

// testAdminKindSystemRole is the system role an admin-kind test identity's
// issuance binding holds.
func testAdminKindSystemRole(kind string) string {
	if kind == store.UserKindTestSuperAdmin {
		return store.SystemRoleSuperAdmin
	}
	return store.SystemRoleHubAdmin
}

// testAdminExpiry is t at the precision timestamps are stored at (the
// Postgres timestamp precision, claimTimeResolution). An admin-kind
// identity's expiry and its issuance binding's are written and compared
// at it, so a value compares the same before and after a round trip.
func testAdminExpiry(t time.Time) time.Time { return t.UTC().Truncate(claimTimeResolution) }

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
//   - A federated account is refused (ErrFederatedAccountRefused)
//   - A role that does not confer membership is refused
//     (ErrNonMembershipRoleRefused); CreateVisitorRoleBinding binds it
func (r *RoleStore) CreateRoleBinding(ctx context.Context, rb *store.RoleBinding) (*store.RoleBinding, error) {
	return r.createRoleBinding(ctx, rb, false)
}

// CreateVisitorRoleBinding creates a visitor grant: a binding of the seeded
// project-visitor role, at project scope, to a federated account. It shares
// CreateRoleBinding's validation, and differs only in what it admits: the
// role must be the system project-visitor role (which does not confer
// membership), the scope must be a project and the principal must be a
// user whose account is UserKindFederated. Anything else is refused with
// store.ErrVisitorBindingRefused. An empty RoleDefinitionID resolves the
// project-visitor role by name. It runs on whichever client the store
// holds, so it joins the transaction of a store passed to WithTx.
func (r *RoleStore) CreateVisitorRoleBinding(ctx context.Context, rb *store.RoleBinding) (*store.RoleBinding, error) {
	if rb == nil {
		return nil, store.ErrVisitorBindingRefused
	}
	if rb.RoleDefinitionID == "" {
		rd, err := r.client.RoleDefinition.Query().
			Where(
				roledefinition.NameEQ(store.ProjectRoleVisitor),
				roledefinition.ScopeTypeEQ(roledefinition.ScopeTypeProject),
			).
			Only(ctx)
		if err != nil {
			return nil, fmt.Errorf("resolving the project-visitor role: %w", mapError(err))
		}
		resolved := *rb
		resolved.RoleDefinitionID = rd.ID.String()
		rb = &resolved
	}
	return r.createRoleBinding(ctx, rb, true)
}

// visitorRoleDefinition returns the seeded project-visitor role definition
// (see isVisitorRole).
func (r *RoleStore) visitorRoleDefinition(ctx context.Context) (*ent.RoleDefinition, error) {
	rd, err := r.client.RoleDefinition.Query().
		Where(
			roledefinition.NameEQ(store.ProjectRoleVisitor),
			roledefinition.ScopeTypeEQ(roledefinition.ScopeTypeProject),
		).
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolving the project-visitor role: %w", mapError(err))
	}
	if !isVisitorRole(rd) {
		return nil, store.ErrVisitorBindingRefused
	}
	return rd, nil
}

// ListVisitorRoleBindings returns the project-visitor bindings of a
// project (see store.Store.ListVisitorRoleBindings).
func (r *RoleStore) ListVisitorRoleBindings(ctx context.Context, projectID string) ([]*store.RoleBinding, error) {
	if projectID == "" {
		return nil, fmt.Errorf("%w: a project ID is required", store.ErrInvalidInput)
	}
	rd, err := r.visitorRoleDefinition(ctx)
	if err != nil {
		return nil, err
	}
	rbs, err := r.client.RoleBinding.Query().
		Where(
			rolebinding.RoleDefinitionIDEQ(rd.ID),
			rolebinding.ScopeTypeEQ(rolebinding.ScopeTypeProject),
			rolebinding.ScopeIDEQ(projectID),
		).
		Order(ent.Asc(rolebinding.FieldCreated), ent.Asc(rolebinding.FieldID)).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]*store.RoleBinding, len(rbs))
	for i, rb := range rbs {
		out[i] = entRoleBindingToStore(rb)
	}
	return out, nil
}

// UpdateVisitorRoleBindingWindow replaces the window of a project-visitor
// binding (see store.Store.UpdateVisitorRoleBindingWindow).
func (r *RoleStore) UpdateVisitorRoleBindingWindow(ctx context.Context, id string, notBefore, expiresAt *time.Time) (*store.RoleBinding, error) {
	uid, err := parseGetID(id)
	if err != nil {
		return nil, err
	}
	rd, err := r.visitorRoleDefinition(ctx)
	if err != nil {
		return nil, err
	}
	existing, err := r.client.RoleBinding.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}
	if existing.RoleDefinitionID == nil || *existing.RoleDefinitionID != rd.ID {
		return nil, store.ErrVisitorBindingRefused
	}
	// Grant shaping (admin tier A3): an admin-kind test identity changes the
	// window only of a visitor binding it made (the same issuance), and the
	// window still ends no later than the identity; any other change could
	// not be bounded by its expiry.
	shaping, err := grantShapingFor(ctx, r.client)
	if err != nil {
		return nil, err
	}
	if shaping != nil {
		if !shapedByIssuance(shaping, existing.IssuanceID) {
			return nil, store.ErrTestAdminGrantRefused
		}
		expiresAt = shaping.ClampExpiry(expiresAt)
	}
	update := r.client.RoleBinding.UpdateOneID(uid)
	if notBefore != nil {
		update.SetNotBefore(*notBefore)
	} else {
		update.ClearNotBefore()
	}
	if expiresAt != nil {
		update.SetExpiresAt(*expiresAt)
	} else {
		update.ClearExpiresAt()
	}
	updated, err := update.Save(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return entRoleBindingToStore(updated), nil
}

// visitorBindingSourceRule checks the source rule ruleID of a visitor
// binding at project scope projectID for the federated account u: the
// rule must exist, be of that project and of the account's home issuer,
// and be active now (ProjectVisitorRule.ActiveAt). It returns the rule,
// whose window the binding takes, or ErrVisitorBindingRefused.
//
// On Postgres the rule row is read FOR SHARE, so in a transaction the
// rule cannot be removed or have its window changed until the binding
// commits: DeleteProjectVisitorRule and UpdateProjectVisitorRuleWindow
// take the row FOR UPDATE before they touch bindings, so they either wait
// for this binding and then include it, or run first, and this read then
// sees the removed rule or the new window. SQLite serializes writes, so a
// plain read gives the same order.
func (r *RoleStore) visitorBindingSourceRule(ctx context.Context, ruleID, projectID string, u *ent.User) (*ent.ProjectVisitorRule, error) {
	ruleUID, err := uuid.Parse(ruleID)
	if err != nil {
		return nil, store.ErrVisitorBindingRefused
	}
	q := r.client.ProjectVisitorRule.Query().Where(projectvisitorrule.IDEQ(ruleUID))
	if r.client.Driver().Dialect() == dialect.Postgres {
		q = q.ForShare()
	}
	rule, err := q.Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, store.ErrVisitorBindingRefused
		}
		return nil, mapError(err)
	}
	if rule.ProjectID != projectID || u.HomeIssuer == nil || *u.HomeIssuer != rule.Issuer {
		return nil, store.ErrVisitorBindingRefused
	}
	nowFn := r.now
	if nowFn == nil {
		nowFn = time.Now
	}
	if !entProjectVisitorRuleToStore(rule).ActiveAt(nowFn()) {
		return nil, store.ErrVisitorBindingRefused
	}
	return rule, nil
}

// isVisitorRole reports whether rd is the seeded project-visitor role: a
// system role of that name, at project scope, that does not confer
// membership. A custom role of the same name is not.
func isVisitorRole(rd *ent.RoleDefinition) bool {
	return rd.Name == store.ProjectRoleVisitor &&
		rd.ScopeType == roledefinition.ScopeTypeProject &&
		rd.System &&
		!rd.ConfersMembership
}

// createRoleBinding is CreateRoleBinding (visitor false) and
// CreateVisitorRoleBinding (visitor true).
func (r *RoleStore) createRoleBinding(ctx context.Context, rb *store.RoleBinding, visitor bool) (*store.RoleBinding, error) {
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

	// The visitor path binds only the project-visitor role, at project
	// scope, to a user principal (the account kind is checked below). The
	// general path never binds a role that does not confer membership.
	if visitor {
		if !isVisitorRole(rd) || rb.ScopeType != store.RoleScopeProject || rb.PrincipalType != store.RoleBindingPrincipalUser {
			return nil, store.ErrVisitorBindingRefused
		}
	} else if !rd.ConfersMembership {
		return nil, fmt.Errorf("%w: role %q", store.ErrNonMembershipRoleRefused, rd.Name)
	}
	// Only the visitor path records the visitor rule that created a
	// binding.
	if rb.SourceRuleID != nil && !visitor {
		return nil, fmt.Errorf("%w: a source rule is set only on a visitor binding", store.ErrInvalidInput)
	}

	// Scope type must match the role definition's scope type.
	if rb.ScopeType != string(rd.ScopeType) {
		return nil, fmt.Errorf("%w: role %q requires scope %q but got %q",
			store.ErrScopeMismatch, rd.Name, string(rd.ScopeType), rb.ScopeType)
	}

	// Grant shaping (admin tier A3): a binding written for an admin-kind
	// test identity expires no later than the identity and carries its
	// issuance ID; one written by such an identity without the shaping is
	// refused. The shaped binding still passes every guard below,
	// including D10: its CreatedBy is the identity, never a reconciler or
	// issuance sentinel, so a super-admin binding is refused.
	shaping, err := grantShapingFor(ctx, r.client, rb.CreatedBy)
	if err != nil {
		return nil, err
	}
	if shaping != nil {
		stored := *rb
		stored.ExpiresAt = shaping.ClampExpiry(rb.ExpiresAt)
		rb = &stored
	}
	// A visitor rule's binding is written only at a visitor's first
	// contact, never under shaping: an admin-kind test identity adds no
	// visitor rule (CreateProjectVisitorRule).
	if shaping != nil && rb.SourceRuleID != nil {
		return nil, store.ErrTestAdminGrantRefused
	}

	// No super-admin binding is ever written under shaping, whatever
	// CreatedBy the caller names (design §4.5).
	if shaping != nil && rd.Name == store.SystemRoleSuperAdmin {
		return nil, store.ErrSuperAdminBindingRestricted
	}

	// D10 guard: refuse super-admin bindings from non-reconciler callers.
	// The one other writer is test identity issuance: its binding is
	// admitted only for a test_super_admin row, checked with the principal
	// below (checkTestAdminSystemBinding).
	if rd.Name == store.SystemRoleSuperAdmin && !store.IsSuperAdminBindingAllowed(rb.CreatedBy) &&
		rb.CreatedBy != store.TestIdentityIssuanceCreatedBy {
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

	// Principal existence validation. sourceRuleUID is the checked
	// SourceRuleID of a visitor binding (set in the user case below).
	var sourceRuleUID *uuid.UUID
	principalUID, err := parseUUID(rb.PrincipalID)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid principal_id: %s", store.ErrInvalidInput, rb.PrincipalID)
	}
	switch rb.PrincipalType {
	case store.RoleBindingPrincipalUser:
		u, err := r.client.User.Get(ctx, principalUID)
		if err != nil {
			return nil, fmt.Errorf("%w: user %s", store.ErrNotFound, rb.PrincipalID)
		}
		// A federated account receives no role binding through the
		// general path, and the visitor path binds only federated
		// accounts.
		federated := string(u.Kind) == store.UserKindFederated
		if visitor && !federated {
			return nil, store.ErrVisitorBindingRefused
		}
		if !visitor && federated {
			return nil, store.ErrFederatedAccountRefused
		}
		if visitor && rb.SourceRuleID != nil {
			rule, err := r.visitorBindingSourceRule(ctx, *rb.SourceRuleID, rb.ScopeID, u)
			if err != nil {
				return nil, err
			}
			ruleUID := rule.ID
			sourceRuleUID = &ruleUID
			// A rule's binding carries the rule's current window, read
			// in this transaction, whatever window the caller passed.
			stored := *rb
			stored.NotBefore = rule.NotBefore
			stored.ExpiresAt = rule.ExpiresAt
			rb = &stored
		}
		exp, err := checkTestAdminSystemBinding(u, rb, rd.Name)
		if err != nil {
			return nil, err
		}
		if exp != nil {
			stored := *rb
			stored.ExpiresAt = exp
			rb = &stored
		}
	case store.RoleBindingPrincipalAgent:
		if _, err := r.client.Agent.Get(ctx, principalUID); err != nil {
			return nil, fmt.Errorf("%w: agent %s", store.ErrNotFound, rb.PrincipalID)
		}
	case store.RoleBindingPrincipalGroup:
		g, err := r.client.Group.Get(ctx, principalUID)
		if err != nil {
			return nil, fmt.Errorf("%w: group %s", store.ErrNotFound, rb.PrincipalID)
		}
		// Project members groups are system-managed and cannot be granted
		// roles, on any scope.
		if store.IsProjectMembersGroup(entGroupToStore(g)) {
			return nil, fmt.Errorf("%w: group %s", store.ErrProjectMembersGroupPrincipal, rb.PrincipalID)
		}
	}

	// D4 enforcement: at most one built-in membership role per principal per
	// project. This application-level check provides clean error messages for
	// sequential callers. The database partial unique index
	// (idx_rolebinding_one_membership_per_principal_per_project) catches
	// concurrent races that slip past this check.
	var membershipKind *string
	// Only a system role of a built-in name is a built-in membership role; a
	// custom row that holds such a name is not (ptone/scion#5519).
	if rd.System && store.IsBuiltInProjectMembershipRole(rd.Name) {
		mk := store.MembershipKindBuiltin
		membershipKind = &mk

		// Check for existing built-in membership in the same project.
		// Option B: if the existing binding has the exact same role_definition_id,
		// fall through to Save() so the Ent unique index produces ErrAlreadyExists
		// (preserving idempotent retry semantics for callers like
		// createProjectOwnerRoleBindingTx, seed backfill, etc.). Only block when
		// a *different* built-in membership role already exists.
		existing, qErr := r.client.RoleBinding.Query().
			Where(
				rolebinding.PrincipalTypeEQ(rolebinding.PrincipalType(rb.PrincipalType)),
				rolebinding.PrincipalIDEQ(rb.PrincipalID),
				rolebinding.ScopeTypeEQ(rolebinding.ScopeTypeProject),
				rolebinding.ScopeIDEQ(rb.ScopeID),
				rolebinding.MembershipKindNotNil(),
			).
			First(ctx)
		if qErr == nil && existing != nil {
			// Exact same role definition → idempotent retry; let Save()
			// hit the Ent unique index and return ErrAlreadyExists.
			if existing.RoleDefinitionID != nil && *existing.RoleDefinitionID == rdUID {
				// Fall through — the existing binding is for the same role.
			} else {
				return nil, fmt.Errorf("%w: principal %s:%s already has a built-in membership in scope %s",
					store.ErrBuiltInMembershipConflict, rb.PrincipalType, rb.PrincipalID, rb.ScopeID)
			}
		}
		// ent.NotFoundError means no existing membership — proceed.
	}

	// The shaping clamp is applied again last, after every window set
	// above (a visitor rule's, an issuance binding's), so none of them can
	// end the binding after the identity.
	if shaping != nil {
		stored := *rb
		stored.ExpiresAt = shaping.ClampExpiry(rb.ExpiresAt)
		rb = &stored
	}

	builder := r.client.RoleBinding.Create().
		SetNillableRoleDefinitionID(&rdUID).
		SetPrincipalType(rolebinding.PrincipalType(rb.PrincipalType)).
		SetPrincipalID(rb.PrincipalID).
		SetScopeType(rolebinding.ScopeType(rb.ScopeType)).
		SetScopeID(rb.ScopeID).
		SetNillableMembershipKind(membershipKind).
		SetNillableSourceRuleID(sourceRuleUID).
		SetNillableIssuanceID(issuanceStamp(shaping))

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
		// The application-level membership check (above) handles "different
		// built-in role already exists" → ErrBuiltInMembershipConflict.
		// When Save() fails here for a membership binding, it's either:
		//  (a) exact duplicate (same role) that fell through the app check →
		//      Ent unique index produces ErrAlreadyExists (idempotent path), or
		//  (b) concurrent race where another goroutine inserted a different
		//      built-in role between the app check and Save() → partial unique
		//      index produces ErrAlreadyExists (correct rejection, acceptable
		//      as a generic "already exists" since the race is rare).
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

// confersMembership matches role bindings whose role definition confers
// project membership. A binding of a role that does not (project-visitor)
// is never membership, and neither is one whose role definition is unset.
func confersMembership() predicate.RoleBinding {
	return rolebinding.HasRoleDefinitionWith(roledefinition.ConfersMembership(true))
}

// GetProjectMembership returns the project membership for a user in a project.
// It looks up role bindings scoped to the project for the user and resolves
// the role definition name. Only bindings whose activation window contains
// the current time count (roleBindingActiveAt), and only of roles that
// confer membership (confersMembership); a user with none has no
// membership and gets ErrNotFound.
func (r *RoleStore) GetProjectMembership(ctx context.Context, projectID, userID string) (*store.ProjectMembership, error) {
	rbs, err := r.client.RoleBinding.Query().
		Where(
			rolebinding.PrincipalTypeEQ(rolebinding.PrincipalTypeUser),
			rolebinding.PrincipalIDEQ(userID),
			rolebinding.ScopeTypeEQ(rolebinding.ScopeTypeProject),
			rolebinding.ScopeIDEQ(projectID),
			confersMembership(),
		).
		WithRoleDefinition().
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	rbs = r.activeRoleBindings(rbs)
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
// Only bindings whose activation window contains the current time count
// (roleBindingActiveAt), and only of roles that confer membership
// (confersMembership): a visitor is not listed.
func (r *RoleStore) ListProjectMembers(ctx context.Context, projectID string) ([]*store.ProjectMembership, error) {
	rbs, err := r.client.RoleBinding.Query().
		Where(
			rolebinding.PrincipalTypeEQ(rolebinding.PrincipalTypeUser),
			rolebinding.ScopeTypeEQ(rolebinding.ScopeTypeProject),
			rolebinding.ScopeIDEQ(projectID),
			confersMembership(),
		).
		WithRoleDefinition().
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	rbs = r.activeRoleBindings(rbs)

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
// for the given project whose activation window contains the current time
// (roleBindingActiveAt) and whose role confers membership
// (confersMembership).
func (r *RoleStore) IsProjectMember(ctx context.Context, projectID, userID string) (bool, error) {
	rbs, err := r.client.RoleBinding.Query().
		Where(
			rolebinding.PrincipalTypeEQ(rolebinding.PrincipalTypeUser),
			rolebinding.PrincipalIDEQ(userID),
			rolebinding.ScopeTypeEQ(rolebinding.ScopeTypeProject),
			rolebinding.ScopeIDEQ(projectID),
			confersMembership(),
		).
		All(ctx)
	if err != nil {
		return false, mapError(err)
	}
	return len(r.activeRoleBindings(rbs)) > 0, nil
}

// roleRank returns a numeric ranking for role bindings based on the role
// definition name. Higher rank = more privilege.
func roleRank(rb *ent.RoleBinding) int {
	if rb.Edges.RoleDefinition == nil || !rb.Edges.RoleDefinition.System {
		return 0
	}
	switch rb.Edges.RoleDefinition.Name {
	case store.ProjectRoleOwner:
		return 4
	case store.ProjectRoleAdmin:
		return 3
	case store.ProjectRoleMember:
		return 2
	case store.ProjectRoleViewer:
		return 1
	default:
		return 0
	}
}
