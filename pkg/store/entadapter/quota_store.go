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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entitlementbinding"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/limitdefinition"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/usagereservation"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// QuotaStore implements store.QuotaStore using Ent ORM.
type QuotaStore struct {
	client *ent.Client
}

// NewQuotaStore creates a new Ent-backed QuotaStore.
func NewQuotaStore(client *ent.Client) *QuotaStore {
	return &QuotaStore{client: client}
}

// ---------------------------------------------------------------------------
// Conversion helpers
// ---------------------------------------------------------------------------

func entLimitDefinitionToStore(ld *ent.LimitDefinition) *store.LimitDefinition {
	return &store.LimitDefinition{
		ID:           ld.ID.String(),
		Name:         ld.Name,
		ResourceType: ld.ResourceType,
		Unit:         ld.Unit,
		Description:  ld.Description,
		DefaultValue: ld.DefaultValue,
		System:       ld.System,
		CreatedAt:    ld.CreatedAt,
		UpdatedAt:    ld.UpdatedAt,
	}
}

func entEntitlementBindingToStore(eb *ent.EntitlementBinding) *store.EntitlementBinding {
	return &store.EntitlementBinding{
		ID:                eb.ID.String(),
		LimitDefinitionID: eb.LimitDefinitionID.String(),
		SubjectType:       string(eb.SubjectType),
		SubjectID:         eb.SubjectID,
		ScopeType:         string(eb.ScopeType),
		ScopeID:           eb.ScopeID,
		Value:             eb.Value,
		CreatedBy:         eb.CreatedBy,
		CreatedAt:         eb.CreatedAt,
		UpdatedAt:         eb.UpdatedAt,
	}
}

func entUsageReservationToStore(ur *ent.UsageReservation) *store.UsageReservation {
	return &store.UsageReservation{
		ID:                ur.ID.String(),
		LimitDefinitionID: ur.LimitDefinitionID.String(),
		SubjectID:         ur.SubjectID,
		ScopeType:         string(ur.ScopeType),
		ScopeID:           ur.ScopeID,
		ResourceID:        ur.ResourceID,
		Reserved:          ur.Reserved,
		CreatedAt:         ur.CreatedAt,
		ReleasedAt:        ur.ReleasedAt,
	}
}

// ---------------------------------------------------------------------------
// LimitDefinition CRUD
// ---------------------------------------------------------------------------

// CreateLimitDefinition creates a new limit definition.
func (q *QuotaStore) CreateLimitDefinition(ctx context.Context, def *store.LimitDefinition) (*store.LimitDefinition, error) {
	builder := q.client.LimitDefinition.Create().
		SetName(def.Name).
		SetResourceType(def.ResourceType).
		SetUnit(def.Unit).
		SetDescription(def.Description).
		SetDefaultValue(def.DefaultValue).
		SetSystem(def.System)

	if def.ID != "" {
		uid, err := parseUUID(def.ID)
		if err != nil {
			return nil, err
		}
		builder.SetID(uid)
	}

	created, err := builder.Save(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return entLimitDefinitionToStore(created), nil
}

// GetLimitDefinition retrieves a limit definition by ID.
func (q *QuotaStore) GetLimitDefinition(ctx context.Context, id string) (*store.LimitDefinition, error) {
	uid, err := parseGetID(id)
	if err != nil {
		return nil, err
	}
	ld, err := q.client.LimitDefinition.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}
	return entLimitDefinitionToStore(ld), nil
}

// GetLimitDefinitionByName retrieves a limit definition by name.
func (q *QuotaStore) GetLimitDefinitionByName(ctx context.Context, name string) (*store.LimitDefinition, error) {
	ld, err := q.client.LimitDefinition.Query().
		Where(limitdefinition.NameEQ(name)).
		Only(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return entLimitDefinitionToStore(ld), nil
}

// ListLimitDefinitions returns all limit definitions.
func (q *QuotaStore) ListLimitDefinitions(ctx context.Context) ([]*store.LimitDefinition, error) {
	lds, err := q.client.LimitDefinition.Query().
		Order(ent.Asc(limitdefinition.FieldName)).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	result := make([]*store.LimitDefinition, len(lds))
	for i, ld := range lds {
		result[i] = entLimitDefinitionToStore(ld)
	}
	return result, nil
}

// UpdateLimitDefinition updates an existing limit definition.
func (q *QuotaStore) UpdateLimitDefinition(ctx context.Context, def *store.LimitDefinition) (*store.LimitDefinition, error) {
	uid, err := parseGetID(def.ID)
	if err != nil {
		return nil, err
	}
	updated, err := q.client.LimitDefinition.UpdateOneID(uid).
		SetName(def.Name).
		SetResourceType(def.ResourceType).
		SetUnit(def.Unit).
		SetDescription(def.Description).
		SetDefaultValue(def.DefaultValue).
		Save(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return entLimitDefinitionToStore(updated), nil
}

// DeleteLimitDefinition deletes a limit definition by ID.
func (q *QuotaStore) DeleteLimitDefinition(ctx context.Context, id string) error {
	uid, err := parseGetID(id)
	if err != nil {
		return err
	}
	err = q.client.LimitDefinition.DeleteOneID(uid).Exec(ctx)
	return mapError(err)
}

// ---------------------------------------------------------------------------
// EntitlementBinding CRUD
// ---------------------------------------------------------------------------

// CreateEntitlementBinding creates a new entitlement binding.
func (q *QuotaStore) CreateEntitlementBinding(ctx context.Context, binding *store.EntitlementBinding) (*store.EntitlementBinding, error) {
	ldUID, err := parseUUID(binding.LimitDefinitionID)
	if err != nil {
		return nil, err
	}

	builder := q.client.EntitlementBinding.Create().
		SetLimitDefinitionID(ldUID).
		SetSubjectType(entitlementbinding.SubjectType(binding.SubjectType)).
		SetSubjectID(binding.SubjectID).
		SetScopeType(entitlementbinding.ScopeType(binding.ScopeType)).
		SetScopeID(binding.ScopeID).
		SetValue(binding.Value).
		SetCreatedBy(binding.CreatedBy)

	if binding.ID != "" {
		uid, err := parseUUID(binding.ID)
		if err != nil {
			return nil, err
		}
		builder.SetID(uid)
	}

	created, err := builder.Save(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return entEntitlementBindingToStore(created), nil
}

// GetEntitlementBinding retrieves an entitlement binding by ID.
func (q *QuotaStore) GetEntitlementBinding(ctx context.Context, id string) (*store.EntitlementBinding, error) {
	uid, err := parseGetID(id)
	if err != nil {
		return nil, err
	}
	eb, err := q.client.EntitlementBinding.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}
	return entEntitlementBindingToStore(eb), nil
}

// ListEntitlementBindings returns all entitlement bindings for a limit definition.
func (q *QuotaStore) ListEntitlementBindings(ctx context.Context, limitDefinitionID string) ([]*store.EntitlementBinding, error) {
	ldUID, err := parseUUID(limitDefinitionID)
	if err != nil {
		return nil, err
	}
	ebs, err := q.client.EntitlementBinding.Query().
		Where(entitlementbinding.LimitDefinitionIDEQ(ldUID)).
		Order(ent.Asc(entitlementbinding.FieldCreatedAt)).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	result := make([]*store.EntitlementBinding, len(ebs))
	for i, eb := range ebs {
		result[i] = entEntitlementBindingToStore(eb)
	}
	return result, nil
}

// ListEntitlementBindingsForSubject returns all entitlement bindings for a given subject.
func (q *QuotaStore) ListEntitlementBindingsForSubject(ctx context.Context, subjectType, subjectID string) ([]*store.EntitlementBinding, error) {
	ebs, err := q.client.EntitlementBinding.Query().
		Where(
			entitlementbinding.SubjectTypeEQ(entitlementbinding.SubjectType(subjectType)),
			entitlementbinding.SubjectIDEQ(subjectID),
		).
		Order(ent.Asc(entitlementbinding.FieldCreatedAt)).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	result := make([]*store.EntitlementBinding, len(ebs))
	for i, eb := range ebs {
		result[i] = entEntitlementBindingToStore(eb)
	}
	return result, nil
}

// UpdateEntitlementBinding updates an existing entitlement binding.
func (q *QuotaStore) UpdateEntitlementBinding(ctx context.Context, binding *store.EntitlementBinding) (*store.EntitlementBinding, error) {
	uid, err := parseGetID(binding.ID)
	if err != nil {
		return nil, err
	}
	updated, err := q.client.EntitlementBinding.UpdateOneID(uid).
		SetValue(binding.Value).
		Save(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return entEntitlementBindingToStore(updated), nil
}

// DeleteEntitlementBinding deletes an entitlement binding by ID.
func (q *QuotaStore) DeleteEntitlementBinding(ctx context.Context, id string) error {
	uid, err := parseGetID(id)
	if err != nil {
		return err
	}
	err = q.client.EntitlementBinding.DeleteOneID(uid).Exec(ctx)
	return mapError(err)
}

// ---------------------------------------------------------------------------
// UsageReservation operations
// ---------------------------------------------------------------------------

// CreateUsageReservation creates a new usage reservation.
func (q *QuotaStore) CreateUsageReservation(ctx context.Context, reservation *store.UsageReservation) (*store.UsageReservation, error) {
	ldUID, err := parseUUID(reservation.LimitDefinitionID)
	if err != nil {
		return nil, err
	}

	builder := q.client.UsageReservation.Create().
		SetLimitDefinitionID(ldUID).
		SetSubjectID(reservation.SubjectID).
		SetScopeType(usagereservation.ScopeType(reservation.ScopeType)).
		SetScopeID(reservation.ScopeID).
		SetResourceID(reservation.ResourceID).
		SetReserved(reservation.Reserved)

	if reservation.ID != "" {
		uid, err := parseUUID(reservation.ID)
		if err != nil {
			return nil, err
		}
		builder.SetID(uid)
	}

	created, err := builder.Save(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return entUsageReservationToStore(created), nil
}

// CountActiveReservations counts non-released reservations for a specific
// limit, subject, and scope.
func (q *QuotaStore) CountActiveReservations(ctx context.Context, limitDefinitionID, subjectID, scopeType, scopeID string) (int64, error) {
	ldUID, err := parseUUID(limitDefinitionID)
	if err != nil {
		return 0, err
	}
	count, err := q.client.UsageReservation.Query().
		Where(
			usagereservation.LimitDefinitionIDEQ(ldUID),
			usagereservation.SubjectIDEQ(subjectID),
			usagereservation.ScopeTypeEQ(usagereservation.ScopeType(scopeType)),
			usagereservation.ScopeIDEQ(scopeID),
			usagereservation.ReleasedAtIsNil(),
		).
		Count(ctx)
	if err != nil {
		return 0, mapError(err)
	}
	return int64(count), nil
}

// ReleaseReservation releases a reservation by setting released_at.
// Matches on limit_definition_id and resource_id.
func (q *QuotaStore) ReleaseReservation(ctx context.Context, limitDefinitionID, resourceID string) error {
	ldUID, err := parseUUID(limitDefinitionID)
	if err != nil {
		return err
	}
	now := time.Now()
	n, err := q.client.UsageReservation.Update().
		Where(
			usagereservation.LimitDefinitionIDEQ(ldUID),
			usagereservation.ResourceIDEQ(resourceID),
			usagereservation.ReleasedAtIsNil(),
		).
		SetReleasedAt(now).
		Save(ctx)
	if err != nil {
		return mapError(err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// ReleaseReservationsByResource releases all reservations for a given resource ID.
func (q *QuotaStore) ReleaseReservationsByResource(ctx context.Context, resourceID string) error {
	now := time.Now()
	_, err := q.client.UsageReservation.Update().
		Where(
			usagereservation.ResourceIDEQ(resourceID),
			usagereservation.ReleasedAtIsNil(),
		).
		SetReleasedAt(now).
		Save(ctx)
	return mapError(err)
}

// ListActiveReservations returns active (non-released) reservations for a limit and scope.
func (q *QuotaStore) ListActiveReservations(ctx context.Context, limitDefinitionID, scopeType, scopeID string) ([]*store.UsageReservation, error) {
	ldUID, err := parseUUID(limitDefinitionID)
	if err != nil {
		return nil, err
	}
	urs, err := q.client.UsageReservation.Query().
		Where(
			usagereservation.LimitDefinitionIDEQ(ldUID),
			usagereservation.ScopeTypeEQ(usagereservation.ScopeType(scopeType)),
			usagereservation.ScopeIDEQ(scopeID),
			usagereservation.ReleasedAtIsNil(),
		).
		Order(ent.Asc(usagereservation.FieldCreatedAt)).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	result := make([]*store.UsageReservation, len(urs))
	for i, ur := range urs {
		result[i] = entUsageReservationToStore(ur)
	}
	return result, nil
}
