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

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/accessconstraint"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/predicate"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// AccessConstraintStore implements store.AccessConstraintStore using Ent ORM.
type AccessConstraintStore struct {
	client *ent.Client
}

// NewAccessConstraintStore creates a new Ent-backed AccessConstraintStore.
func NewAccessConstraintStore(client *ent.Client) *AccessConstraintStore {
	return &AccessConstraintStore{client: client}
}

// entAccessConstraintToStore converts an Ent AccessConstraint entity to a
// store.AccessConstraint model.
func entAccessConstraintToStore(e *ent.AccessConstraint) *store.AccessConstraint {
	result := &store.AccessConstraint{
		ID:                 e.ID.String(),
		Name:               e.Name,
		SubjectKind:        string(e.SubjectKind),
		ScopeType:          string(e.ScopeType),
		ScopeID:            e.ScopeID,
		MaximumPermissions: e.MaximumPermissions,
		NotBefore:          e.NotBefore,
		ExpiresAt:          e.ExpiresAt,
		Disabled:           e.Disabled,
		CreatedBy:          e.CreatedBy,
		CreatedAt:          e.Created,
		UpdatedAt:          e.Updated,
	}
	if e.SubjectPrincipalType != nil {
		result.SubjectPrincipalType = e.SubjectPrincipalType
	}
	if e.SubjectPrincipalID != nil {
		result.SubjectPrincipalID = e.SubjectPrincipalID
	}
	if e.SubjectGroupID != nil {
		result.SubjectGroupID = e.SubjectGroupID
	}
	return result
}

// CreateAccessConstraint creates a new access constraint.
func (s *AccessConstraintStore) CreateAccessConstraint(ctx context.Context, c *store.AccessConstraint) (*store.AccessConstraint, error) {
	builder := s.client.AccessConstraint.Create().
		SetName(c.Name).
		SetSubjectKind(accessconstraint.SubjectKind(c.SubjectKind)).
		SetScopeType(accessconstraint.ScopeType(c.ScopeType)).
		SetScopeID(c.ScopeID).
		SetMaximumPermissions(c.MaximumPermissions).
		SetDisabled(c.Disabled).
		SetCreatedBy(c.CreatedBy)

	if c.SubjectPrincipalType != nil {
		builder.SetSubjectPrincipalType(*c.SubjectPrincipalType)
	}
	if c.SubjectPrincipalID != nil {
		builder.SetSubjectPrincipalID(*c.SubjectPrincipalID)
	}
	if c.SubjectGroupID != nil {
		builder.SetSubjectGroupID(*c.SubjectGroupID)
	}
	if c.NotBefore != nil {
		builder.SetNotBefore(*c.NotBefore)
	}
	if c.ExpiresAt != nil {
		builder.SetExpiresAt(*c.ExpiresAt)
	}

	created, err := builder.Save(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return entAccessConstraintToStore(created), nil
}

// GetAccessConstraint retrieves an access constraint by ID.
func (s *AccessConstraintStore) GetAccessConstraint(ctx context.Context, id string) (*store.AccessConstraint, error) {
	uid, err := parseGetID(id)
	if err != nil {
		return nil, err
	}
	e, err := s.client.AccessConstraint.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}
	return entAccessConstraintToStore(e), nil
}

// UpdateAccessConstraint updates an existing access constraint.
func (s *AccessConstraintStore) UpdateAccessConstraint(ctx context.Context, c *store.AccessConstraint) (*store.AccessConstraint, error) {
	uid, err := parseGetID(c.ID)
	if err != nil {
		return nil, err
	}

	builder := s.client.AccessConstraint.UpdateOneID(uid).
		SetName(c.Name).
		SetSubjectKind(accessconstraint.SubjectKind(c.SubjectKind)).
		SetScopeType(accessconstraint.ScopeType(c.ScopeType)).
		SetScopeID(c.ScopeID).
		SetMaximumPermissions(c.MaximumPermissions).
		SetDisabled(c.Disabled)

	if c.SubjectPrincipalType != nil {
		builder.SetSubjectPrincipalType(*c.SubjectPrincipalType)
	} else {
		builder.ClearSubjectPrincipalType()
	}
	if c.SubjectPrincipalID != nil {
		builder.SetSubjectPrincipalID(*c.SubjectPrincipalID)
	} else {
		builder.ClearSubjectPrincipalID()
	}
	if c.SubjectGroupID != nil {
		builder.SetSubjectGroupID(*c.SubjectGroupID)
	} else {
		builder.ClearSubjectGroupID()
	}
	if c.NotBefore != nil {
		builder.SetNotBefore(*c.NotBefore)
	} else {
		builder.ClearNotBefore()
	}
	if c.ExpiresAt != nil {
		builder.SetExpiresAt(*c.ExpiresAt)
	} else {
		builder.ClearExpiresAt()
	}

	updated, err := builder.Save(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return entAccessConstraintToStore(updated), nil
}

// DeleteAccessConstraint deletes an access constraint by ID.
func (s *AccessConstraintStore) DeleteAccessConstraint(ctx context.Context, id string) error {
	uid, err := parseGetID(id)
	if err != nil {
		return err
	}
	err = s.client.AccessConstraint.DeleteOneID(uid).Exec(ctx)
	if err != nil {
		return mapError(err)
	}
	return nil
}

// ListAccessConstraints returns all access constraints with pagination.
func (s *AccessConstraintStore) ListAccessConstraints(ctx context.Context, limit, offset int) ([]*store.AccessConstraint, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}

	entities, err := s.client.AccessConstraint.Query().
		Order(ent.Asc(accessconstraint.FieldCreated)).
		Limit(limit).
		Offset(offset).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}

	result := make([]*store.AccessConstraint, len(entities))
	for i, e := range entities {
		result[i] = entAccessConstraintToStore(e)
	}
	return result, nil
}

// CountAccessConstraints returns the total number of access constraints.
func (s *AccessConstraintStore) CountAccessConstraints(ctx context.Context) (int, error) {
	count, err := s.client.AccessConstraint.Query().Count(ctx)
	if err != nil {
		return 0, mapError(err)
	}
	return count, nil
}

// ResolveApplicableConstraints returns all constraints that may apply to the
// given principals and scopes.
func (s *AccessConstraintStore) ResolveApplicableConstraints(
	ctx context.Context,
	principals []store.PrincipalRef,
	scopeTypes []string,
	scopeIDs []string,
) ([]*store.AccessConstraint, error) {
	// Build subject predicates: match all_principals, or specific principals.
	var subjectPreds []predicate.AccessConstraint

	// Always include all_principals constraints.
	subjectPreds = append(subjectPreds,
		accessconstraint.SubjectKindEQ(accessconstraint.SubjectKindAllPrincipals))

	// Build principal and group_closure predicates from the principal closure.
	for _, p := range principals {
		// Exact principal match.
		subjectPreds = append(subjectPreds, accessconstraint.And(
			accessconstraint.SubjectKindEQ(accessconstraint.SubjectKindPrincipal),
			accessconstraint.SubjectPrincipalIDEQ(p.ID),
		))
		// Group closure match — the principal's groups are in the closure.
		subjectPreds = append(subjectPreds, accessconstraint.And(
			accessconstraint.SubjectKindEQ(accessconstraint.SubjectKindGroupClosure),
			accessconstraint.SubjectGroupIDEQ(p.ID),
		))
	}

	// Build scope predicates.
	var scopePreds []predicate.AccessConstraint
	// System-scoped constraints always apply.
	scopePreds = append(scopePreds,
		accessconstraint.ScopeTypeEQ(accessconstraint.ScopeTypeSystem))
	// Project-scoped constraints apply only to matching projects.
	for _, scopeID := range scopeIDs {
		if scopeID != "" {
			scopePreds = append(scopePreds, accessconstraint.And(
				accessconstraint.ScopeTypeEQ(accessconstraint.ScopeTypeProject),
				accessconstraint.ScopeIDEQ(scopeID),
			))
		}
	}

	// Only return non-disabled constraints.
	query := s.client.AccessConstraint.Query().
		Where(
			accessconstraint.DisabledEQ(false),
			accessconstraint.Or(subjectPreds...),
			accessconstraint.Or(scopePreds...),
		)

	entities, err := query.All(ctx)
	if err != nil {
		return nil, mapError(err)
	}

	result := make([]*store.AccessConstraint, len(entities))
	for i, e := range entities {
		result[i] = entAccessConstraintToStore(e)
	}
	return result, nil
}

// ListConstraintsForScope returns all constraints scoped to the given scope.
func (s *AccessConstraintStore) ListConstraintsForScope(ctx context.Context, scopeType, scopeID string) ([]*store.AccessConstraint, error) {
	var preds []predicate.AccessConstraint

	if scopeType == "system" {
		// For system scope, include system-scoped constraints.
		preds = append(preds, accessconstraint.ScopeTypeEQ(accessconstraint.ScopeTypeSystem))
	} else {
		// For project scope, include both system-scoped and project-scoped constraints.
		preds = append(preds,
			accessconstraint.Or(
				accessconstraint.ScopeTypeEQ(accessconstraint.ScopeTypeSystem),
				accessconstraint.And(
					accessconstraint.ScopeTypeEQ(accessconstraint.ScopeTypeProject),
					accessconstraint.ScopeIDEQ(scopeID),
				),
			),
		)
	}

	entities, err := s.client.AccessConstraint.Query().
		Where(preds...).
		Order(ent.Asc(accessconstraint.FieldCreated)).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}

	result := make([]*store.AccessConstraint, len(entities))
	for i, e := range entities {
		result[i] = entAccessConstraintToStore(e)
	}
	return result, nil
}

// DisableAccessConstraint disables a constraint (for offline recovery).
func (s *AccessConstraintStore) DisableAccessConstraint(ctx context.Context, id string) error {
	uid, err := parseGetID(id)
	if err != nil {
		return err
	}
	err = s.client.AccessConstraint.UpdateOneID(uid).
		SetDisabled(true).
		Exec(ctx)
	if err != nil {
		return mapError(err)
	}
	return nil
}
