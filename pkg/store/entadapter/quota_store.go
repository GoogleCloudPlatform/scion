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
	"strings"
	"sync/atomic"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entitlementbinding"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/limitdefinition"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/runtimebroker"
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

	// CreatedAt is normally left to the schema default (time.Now at
	// creation); callers that need to backdate a reservation — e.g. tests
	// simulating a reservation old enough to clear
	// reconcileMinReservationAge — may set it explicitly.
	if !reservation.CreatedAt.IsZero() {
		builder.SetCreatedAt(reservation.CreatedAt)
	}

	if reservation.ID != "" {
		uid, err := parseUUID(reservation.ID)
		if err != nil {
			return nil, err
		}
		builder.SetID(uid)
	}

	created, err := builder.Save(ctx)
	if err != nil {
		mapped := mapError(err)
		if errors.Is(mapped, store.ErrAlreadyExists) && isActiveReservationViolation(reservation, err) {
			return nil, store.ErrActiveReservationExists
		}
		return nil, mapped
	}
	return entUsageReservationToStore(created), nil
}

// isActiveReservationViolation reports whether a unique violation from
// CreateUsageReservation came from the unique active (limit_definition_id,
// resource_id) index. The table has two unique indexes, that one and the
// primary key. With a generated ID only the former can be violated; with a
// caller-supplied ID, the error text names the index's resource_id column
// (SQLite lists the columns; Postgres names the index after them), which a
// primary key violation does not.
func isActiveReservationViolation(reservation *store.UsageReservation, err error) bool {
	if reservation.ID == "" {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "resource_id")
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

// HasActiveReservation reports whether resourceID already holds a
// non-released reservation for the given limit, regardless of scope.
func (q *QuotaStore) HasActiveReservation(ctx context.Context, limitDefinitionID, resourceID string) (bool, error) {
	ldUID, err := parseUUID(limitDefinitionID)
	if err != nil {
		return false, err
	}
	count, err := q.client.UsageReservation.Query().
		Where(
			usagereservation.LimitDefinitionIDEQ(ldUID),
			usagereservation.ResourceIDEQ(resourceID),
			usagereservation.ReleasedAtIsNil(),
		).
		Count(ctx)
	if err != nil {
		return false, mapError(err)
	}
	return count > 0, nil
}

// GetActiveReservation returns resourceID's non-released reservation for
// the given limit, whatever its scope, or store.ErrNotFound when none.
func (q *QuotaStore) GetActiveReservation(ctx context.Context, limitDefinitionID, resourceID string) (*store.UsageReservation, error) {
	ldUID, err := parseUUID(limitDefinitionID)
	if err != nil {
		return nil, err
	}
	r, err := q.client.UsageReservation.Query().
		Where(
			usagereservation.LimitDefinitionIDEQ(ldUID),
			usagereservation.ResourceIDEQ(resourceID),
			usagereservation.ReleasedAtIsNil(),
		).
		Only(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return entUsageReservationToStore(r), nil
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

// ReleaseReservationByID releases the reservation with reservationID while
// it is still active, in one conditional write; store.ErrNotFound when no
// active row has that ID, including a malformed ID.
func (q *QuotaStore) ReleaseReservationByID(ctx context.Context, reservationID string) error {
	uid, err := parseGetID(reservationID)
	if err != nil {
		return err
	}
	n, err := q.client.UsageReservation.Update().
		Where(
			usagereservation.IDEQ(uid),
			usagereservation.ReleasedAtIsNil(),
		).
		SetReleasedAt(time.Now()).
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

// ListActiveReservationsByScopeType returns active (non-released)
// reservations for a limit across every scope ID of scopeType, ordered by
// created_at ascending, in one query.
func (q *QuotaStore) ListActiveReservationsByScopeType(ctx context.Context, limitDefinitionID, scopeType string) ([]*store.UsageReservation, error) {
	ldUID, err := parseUUID(limitDefinitionID)
	if err != nil {
		return nil, err
	}
	urs, err := q.client.UsageReservation.Query().
		Where(
			usagereservation.LimitDefinitionIDEQ(ldUID),
			usagereservation.ScopeTypeEQ(usagereservation.ScopeType(scopeType)),
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

// CountActiveReservationsByScope returns active (non-released) reservation
// counts for a limit and scopeType, grouped by scope ID, in one query.
func (q *QuotaStore) CountActiveReservationsByScope(ctx context.Context, limitDefinitionID, scopeType string) (map[string]int64, error) {
	ldUID, err := parseUUID(limitDefinitionID)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		ScopeID string `json:"scope_id"`
		Count   int64  `json:"count"`
	}
	err = q.client.UsageReservation.Query().
		Where(
			usagereservation.LimitDefinitionIDEQ(ldUID),
			usagereservation.ScopeTypeEQ(usagereservation.ScopeType(scopeType)),
			usagereservation.ReleasedAtIsNil(),
		).
		GroupBy(usagereservation.FieldScopeID).
		Aggregate(ent.Count()).
		Scan(ctx, &rows)
	if err != nil {
		return nil, mapError(err)
	}
	counts := make(map[string]int64, len(rows))
	for _, r := range rows {
		counts[r.ScopeID] = r.Count
	}
	return counts, nil
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

// ReleaseReservationIfAgent releases agentID's active reservation for the
// limit only if cond holds on the agent row, in one transaction: the agent
// row is read under SELECT ... FOR UPDATE on Postgres (SQLite serializes
// writers), cond is evaluated on that row, and the release is written in
// the same transaction. A malformed agent ID is a missing row
// (store.ErrNotFound).
func (q *QuotaStore) ReleaseReservationIfAgent(ctx context.Context, limitDefinitionID, agentID string, cond store.AgentReleaseCond) (bool, error) {
	ldUID, err := parseUUID(limitDefinitionID)
	if err != nil {
		return false, err
	}
	agentUID, err := parseGetID(agentID)
	if err != nil {
		return false, err
	}
	var resUID uuid.UUID
	if cond.ReservationID != "" {
		if resUID, err = parseGetID(cond.ReservationID); err != nil {
			return false, nil // no reservation can have that ID
		}
	}
	tx, err := q.client.Tx(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	aq := tx.Agent.Query().Where(agent.IDEQ(agentUID))
	if q.client.Driver().Dialect() == dialect.Postgres {
		aq = aq.ForUpdate()
	}
	row, err := aq.Only(ctx)
	if err != nil {
		return false, mapError(err)
	}
	if !cond.Holds(entAgentToStore(row)) {
		return false, nil
	}
	upd := tx.UsageReservation.Update().
		Where(
			usagereservation.LimitDefinitionIDEQ(ldUID),
			usagereservation.ResourceIDEQ(agentID),
			usagereservation.ReleasedAtIsNil(),
		)
	if cond.ReservationID != "" {
		upd = upd.Where(usagereservation.IDEQ(resUID))
	}
	n, err := upd.SetReleasedAt(time.Now()).Save(ctx)
	if err != nil {
		return false, mapError(err)
	}
	if err := tx.Commit(); err != nil {
		return false, mapError(err)
	}
	return n > 0, nil
}

// ReleaseReservationIfScopeBrokerAbsent releases the active reservation
// reservationID only while no Runtime Broker has the ID scopeID, in one
// conditional write:
//
//	UPDATE usage_reservations SET released_at = now
//	WHERE id = :rid AND scope_type = 'broker' AND scope_id = :scopeID
//	  AND released_at IS NULL
//	  AND NOT EXISTS (SELECT 1 FROM runtime_brokers WHERE id = :scopeUUID)
//
// scopeID is parsed first. An unparseable scope ID matches no broker (as
// GetRuntimeBroker reports ErrNotFound for it), so the row is released with
// no broker check. A parsed one is bound as the typed UUID, so the
// comparison is canonical and Postgres never casts a non-UUID literal.
func (q *QuotaStore) ReleaseReservationIfScopeBrokerAbsent(ctx context.Context, reservationID, scopeID string) (bool, error) {
	resUID, err := parseGetID(reservationID)
	if err != nil {
		return false, nil // no reservation can have that ID
	}
	upd := q.client.UsageReservation.Update().
		Where(
			usagereservation.IDEQ(resUID),
			usagereservation.ScopeTypeEQ(usagereservation.ScopeType(store.QuotaScopeBroker)),
			usagereservation.ScopeIDEQ(scopeID),
			usagereservation.ReleasedAtIsNil(),
		)
	if scopeUID, perr := uuid.Parse(scopeID); perr == nil {
		upd = upd.Where(func(s *entsql.Selector) {
			s.Where(entsql.NotExists(
				entsql.Select(runtimebroker.FieldID).
					From(entsql.Table(runtimebroker.Table)).
					Where(entsql.EQ(runtimebroker.FieldID, scopeUID)),
			))
		})
	}
	n, err := upd.SetReleasedAt(time.Now()).Save(ctx)
	if err != nil {
		return false, mapError(err)
	}
	return n > 0, nil
}

// TransferHookFunc is the in-transaction test hook of
// TransferBrokerReservation. It is called with the transaction's own tx, so
// an injected write or failure joins the transaction, at these steps:
//   - "locked": after step 3 completed (the agent row and its reservation
//     are read and locked, and the FromReservationID check passed), before
//     step 4 and any write. It does not fire on the step-2 AgentElsewhere
//     or Stale exits, or on the step-3 Stale exit.
//   - "released": after step 7 released the held row; only when step 7 ran.
//   - "before_insert": immediately before step 8, whenever step 8 runs.
//
// A non-nil return aborts the transfer with that error, rolling everything
// back. On SQLite (one pooled connection) a hook may only use tx or return
// an error; a store call outside tx self-deadlocks.
type TransferHookFunc func(ctx context.Context, tx *ent.Tx, step string, req store.BrokerReservationTransfer) error

// transferBrokerReservationTestHook holds the TransferHookFunc in effect;
// nil in production. An atomic pointer, so a background goroutine left over
// from an earlier test that reads it while a test sets it is not a data
// race.
var transferBrokerReservationTestHook atomic.Pointer[TransferHookFunc]

// SetTransferBrokerReservationTestHook installs fn as the in-transaction
// hook of TransferBrokerReservation and returns a func that restores the
// previous value. For tests only; the hook is nil in production. The hook
// is process-global: a test that sets it must not call t.Parallel.
func SetTransferBrokerReservationTestHook(fn TransferHookFunc) (restore func()) {
	var p *TransferHookFunc
	if fn != nil {
		p = &fn
	}
	prev := transferBrokerReservationTestHook.Swap(p)
	return func() { transferBrokerReservationTestHook.Store(prev) }
}

// Steps at which TransferBrokerReservation calls its test hook.
const (
	transferStepLocked       = "locked"
	transferStepReleased     = "released"
	transferStepBeforeInsert = "before_insert"
)

// runTransferHook calls the test hook, if one is set, at step.
func runTransferHook(ctx context.Context, tx *ent.Tx, step string, req store.BrokerReservationTransfer) error {
	if h := transferBrokerReservationTestHook.Load(); h != nil {
		return (*h)(ctx, tx, step, req)
	}
	return nil
}

// TransferBrokerReservation moves or records req.AgentID's active
// reservation for the limit onto req.ToBrokerID in one transaction (see
// store.QuotaStore.TransferBrokerReservation for the steps). Every
// statement runs through tx: on SQLite the pool has one connection, so a
// statement on q.client inside the transaction would deadlock. The agent
// row is locked first, then the reservation row (FOR UPDATE on Postgres
// only), the order ReleaseReservationIfAgent uses, so the two serialize. A
// malformed agent ID is a missing row (store.ErrNotFound); a malformed
// FromReservationID names no row (TransferStale).
func (q *QuotaStore) TransferBrokerReservation(ctx context.Context, req store.BrokerReservationTransfer) (store.BrokerReservationTransferResult, error) {
	var none store.BrokerReservationTransferResult
	if req.CreateIfAbsent && req.FromReservationID != "" {
		return none, store.ErrInvalidTransfer
	}
	ldUID, err := parseUUID(req.LimitDefinitionID)
	if err != nil {
		return none, err
	}
	agentUID, err := parseGetID(req.AgentID)
	if err != nil {
		return none, err
	}
	var fromUID uuid.UUID
	fromParsed := false
	if req.FromReservationID != "" {
		if fromUID, err = parseGetID(req.FromReservationID); err == nil {
			fromParsed = true
		}
	}
	postgres := q.client.Driver().Dialect() == dialect.Postgres

	tx, err := q.client.Tx(ctx)
	if err != nil {
		return none, mapError(err)
	}
	defer func() { _ = tx.Rollback() }()

	// 1. Lock the agent row.
	aq := tx.Agent.Query().Where(agent.IDEQ(agentUID))
	if postgres {
		aq = aq.ForUpdate()
	}
	row, err := aq.Only(ctx)
	if err != nil {
		return none, mapError(err)
	}
	a := entAgentToStore(row)
	if !a.DeletedAt.IsZero() {
		return none, store.ErrNotFound
	}

	// 2. Preconditions on the locked agent row.
	if req.RequireAgentOnTo && a.RuntimeBrokerID != req.ToBrokerID {
		return store.BrokerReservationTransferResult{Outcome: store.TransferAgentElsewhere}, nil
	}
	if req.Cond != nil && !req.Cond.Holds(a) {
		return store.BrokerReservationTransferResult{Outcome: store.TransferStale}, nil
	}

	// 3. The agent's active reservation for the limit, locked.
	rq := tx.UsageReservation.Query().Where(
		usagereservation.LimitDefinitionIDEQ(ldUID),
		usagereservation.ResourceIDEQ(req.AgentID),
		usagereservation.ReleasedAtIsNil(),
	)
	if postgres {
		rq = rq.ForUpdate()
	}
	held, err := rq.Only(ctx)
	switch {
	case ent.IsNotFound(err):
		held = nil
	case err != nil:
		return none, mapError(err)
	}
	if req.FromReservationID != "" && (held == nil || !fromParsed || held.ID != fromUID) {
		return store.BrokerReservationTransferResult{Outcome: store.TransferStale}, nil
	}
	if err := runTransferHook(ctx, tx, transferStepLocked, req); err != nil {
		return none, err
	}

	// 4. Already on the destination: no write.
	if held != nil && string(held.ScopeType) == store.QuotaScopeBroker && held.ScopeID == req.ToBrokerID {
		return store.BrokerReservationTransferResult{Outcome: store.TransferAlreadyOn, ReservationID: held.ID.String()}, nil
	}
	// 5. None held and none to create.
	if held == nil && !req.CreateIfAbsent {
		return store.BrokerReservationTransferResult{Outcome: store.TransferAbsent}, nil
	}

	// 6. The cap check on the destination, with Reserve's count predicate.
	if req.Cap > 0 {
		count, err := tx.UsageReservation.Query().
			Where(
				usagereservation.LimitDefinitionIDEQ(ldUID),
				usagereservation.SubjectIDEQ(req.ToBrokerID),
				usagereservation.ScopeTypeEQ(usagereservation.ScopeType(store.QuotaScopeBroker)),
				usagereservation.ScopeIDEQ(req.ToBrokerID),
				usagereservation.ReleasedAtIsNil(),
			).
			Count(ctx)
		if err != nil {
			return none, mapError(err)
		}
		if int64(count) >= req.Cap {
			return none, store.ErrQuotaExceeded
		}
	}

	res := store.BrokerReservationTransferResult{Outcome: store.TransferCreated}
	// 7. Release the held row. A zero-row release never falls through to
	// the insert.
	if held != nil {
		n, err := tx.UsageReservation.Update().
			Where(usagereservation.IDEQ(held.ID), usagereservation.ReleasedAtIsNil()).
			SetReleasedAt(time.Now()).
			Save(ctx)
		if err != nil {
			return none, mapError(err)
		}
		if n != 1 {
			return none, store.ErrTransferRaced
		}
		res = store.BrokerReservationTransferResult{
			Outcome:       store.TransferMoved,
			FromScopeType: string(held.ScopeType),
			FromScopeID:   held.ScopeID,
			ReleasedID:    held.ID.String(),
		}
		if err := runTransferHook(ctx, tx, transferStepReleased, req); err != nil {
			return none, err
		}
	}

	// 8. Insert the reservation on the destination.
	if err := runTransferHook(ctx, tx, transferStepBeforeInsert, req); err != nil {
		return none, err
	}
	created, err := tx.UsageReservation.Create().
		SetLimitDefinitionID(ldUID).
		SetSubjectID(req.ToBrokerID).
		SetScopeType(usagereservation.ScopeType(store.QuotaScopeBroker)).
		SetScopeID(req.ToBrokerID).
		SetResourceID(req.AgentID).
		SetReserved(1).
		SetCreatedAt(time.Now()).
		Save(ctx)
	if err != nil {
		if errors.Is(mapError(err), store.ErrAlreadyExists) {
			return none, store.ErrActiveReservationExists
		}
		return none, mapError(err)
	}
	if err := tx.Commit(); err != nil {
		return none, mapError(err)
	}
	res.ReservationID = created.ID.String()
	return res, nil
}
