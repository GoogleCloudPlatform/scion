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
	"github.com/GoogleCloudPlatform/scion/pkg/ent/delegationadoption"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// DelegationAdoptionStore implements store.DelegationAdoptionStore using Ent.
type DelegationAdoptionStore struct {
	client *ent.Client
}

// NewDelegationAdoptionStore creates a new Ent-backed DelegationAdoptionStore.
func NewDelegationAdoptionStore(client *ent.Client) *DelegationAdoptionStore {
	return &DelegationAdoptionStore{client: client}
}

func optionalString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func nillableString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func entDelegationAdoptionToStore(e *ent.DelegationAdoption) *store.DelegationAdoption {
	return &store.DelegationAdoption{
		ID:                e.ID.String(),
		CohortID:          e.CohortID,
		Origin:            e.Origin,
		PolicyVersion:     e.PolicyVersion,
		OriginalEdgeID:    optionalString(e.OriginalEdgeID),
		AdoptedEdgeID:     optionalString(e.AdoptedEdgeID),
		DelegateID:        e.DelegateID,
		DelegatorType:     e.DelegatorType,
		DelegatorID:       e.DelegatorID,
		ScopeID:           e.ScopeID,
		Role:              e.Role,
		Depth:             e.Depth,
		Status:            store.DelegationAdoptionStatus(e.Status),
		Reason:            e.Reason,
		BeforeFingerprint: e.BeforeFingerprint,
		AfterSummary:      e.AfterSummary,
		ActorKind:         e.ActorKind,
		ActorID:           e.ActorID,
		RevertedByKind:    e.RevertedByKind,
		RevertedByID:      e.RevertedByID,
		RevertSummary:     e.RevertSummary,
		RevertedAt:        e.RevertedAt,
		CreatedAt:         e.Created,
		UpdatedAt:         e.Updated,
	}
}

func invalidAdoption(format string, args ...any) error {
	return fmt.Errorf("%w: delegation adoption: %s", store.ErrInvalidInput, fmt.Sprintf(format, args...))
}

// validateDelegationAdoption checks the fields every write must satisfy:
// required identifiers, a known origin and status, and the edge IDs and
// revert fields each status implies. It returns store.ErrInvalidInput.
func validateDelegationAdoption(rec *store.DelegationAdoption) error {
	if rec == nil {
		return invalidAdoption("nil record")
	}
	if rec.CohortID == "" {
		return invalidAdoption("cohort ID is required")
	}
	if rec.DelegateID == "" {
		return invalidAdoption("delegate ID is required")
	}
	switch rec.Origin {
	case store.DelegationAdoptionOriginBoot, store.DelegationAdoptionOriginAdmin:
	default:
		return invalidAdoption("unknown origin %q", rec.Origin)
	}
	if rec.PolicyVersion < 0 || rec.Depth < 0 {
		return invalidAdoption("policy version and depth must not be negative")
	}
	reverted := rec.RevertedByKind != "" || rec.RevertedByID != "" || rec.RevertSummary != "" || rec.RevertedAt != nil
	switch rec.Status {
	case store.DelegationAdoptionPending:
		if rec.OriginalEdgeID == "" || rec.AdoptedEdgeID != "" {
			return invalidAdoption("a pending record names its original edge and no adopted edge")
		}
	case store.DelegationAdoptionAdopted:
		if rec.OriginalEdgeID == "" || rec.AdoptedEdgeID == "" || rec.OriginalEdgeID == rec.AdoptedEdgeID {
			return invalidAdoption("an adopted record names distinct original and adopted edges")
		}
	case store.DelegationAdoptionRecognized, store.DelegationAdoptionRecognizedAbovePolicy:
		if rec.AdoptedEdgeID == "" {
			return invalidAdoption("a recognized record names its adopted edge")
		}
	case store.DelegationAdoptionExcluded:
		if rec.Reason == "" || rec.AdoptedEdgeID != "" {
			return invalidAdoption("an excluded record has a reason and no adopted edge")
		}
	case store.DelegationAdoptionSkippedChanged:
		if rec.AdoptedEdgeID != "" {
			return invalidAdoption("a skipped record has no adopted edge")
		}
	case store.DelegationAdoptionReverted:
		if rec.OriginalEdgeID == "" || rec.AdoptedEdgeID == "" {
			return invalidAdoption("a reverted record names its original and adopted edges")
		}
		if rec.RevertedByKind == "" || rec.RevertedByID == "" || rec.RevertedAt == nil {
			return invalidAdoption("a reverted record names who reverted it and when")
		}
		return nil
	default:
		return invalidAdoption("unknown status %q", rec.Status)
	}
	if reverted {
		return invalidAdoption("revert fields are set only on a reverted record")
	}
	return nil
}

// CreateDelegationAdoption inserts rec. ID is generated when empty.
func (s *DelegationAdoptionStore) CreateDelegationAdoption(ctx context.Context, rec *store.DelegationAdoption) error {
	if err := validateDelegationAdoption(rec); err != nil {
		return err
	}
	builder := s.client.DelegationAdoption.Create().
		SetCohortID(rec.CohortID).
		SetOrigin(rec.Origin).
		SetPolicyVersion(rec.PolicyVersion).
		SetNillableOriginalEdgeID(nillableString(rec.OriginalEdgeID)).
		SetNillableAdoptedEdgeID(nillableString(rec.AdoptedEdgeID)).
		SetDelegateID(rec.DelegateID).
		SetDelegatorType(rec.DelegatorType).
		SetDelegatorID(rec.DelegatorID).
		SetScopeID(rec.ScopeID).
		SetRole(rec.Role).
		SetDepth(rec.Depth).
		SetStatus(string(rec.Status)).
		SetReason(rec.Reason).
		SetBeforeFingerprint(rec.BeforeFingerprint).
		SetAfterSummary(rec.AfterSummary).
		SetActorKind(rec.ActorKind).
		SetActorID(rec.ActorID).
		SetRevertedByKind(rec.RevertedByKind).
		SetRevertedByID(rec.RevertedByID).
		SetRevertSummary(rec.RevertSummary).
		SetNillableRevertedAt(rec.RevertedAt)
	if rec.ID != "" {
		uid, err := parseUUID(rec.ID)
		if err != nil {
			return err
		}
		builder.SetID(uid)
	}
	created, err := builder.Save(ctx)
	if err != nil {
		return mapError(err)
	}
	rec.ID = created.ID.String()
	rec.CreatedAt = created.Created
	rec.UpdatedAt = created.Updated
	return nil
}

// UpdateDelegationAdoption writes the mutable fields of rec by ID.
func (s *DelegationAdoptionStore) UpdateDelegationAdoption(ctx context.Context, rec *store.DelegationAdoption) error {
	if err := validateDelegationAdoption(rec); err != nil {
		return err
	}
	uid, err := parseGetID(rec.ID)
	if err != nil {
		return err
	}
	upd := s.client.DelegationAdoption.UpdateOneID(uid).
		SetStatus(string(rec.Status)).
		SetReason(rec.Reason).
		SetAfterSummary(rec.AfterSummary).
		SetActorKind(rec.ActorKind).
		SetActorID(rec.ActorID).
		SetRevertedByKind(rec.RevertedByKind).
		SetRevertedByID(rec.RevertedByID).
		SetRevertSummary(rec.RevertSummary)
	if rec.RevertedAt == nil {
		upd.ClearRevertedAt()
	} else {
		upd.SetRevertedAt(*rec.RevertedAt)
	}
	if rec.OriginalEdgeID == "" {
		upd.ClearOriginalEdgeID()
	} else {
		upd.SetOriginalEdgeID(rec.OriginalEdgeID)
	}
	if rec.AdoptedEdgeID == "" {
		upd.ClearAdoptedEdgeID()
	} else {
		upd.SetAdoptedEdgeID(rec.AdoptedEdgeID)
	}
	updated, err := upd.Save(ctx)
	if err != nil {
		return mapError(err)
	}
	rec.UpdatedAt = updated.Updated
	return nil
}

// GetDelegationAdoption returns a record by ID.
func (s *DelegationAdoptionStore) GetDelegationAdoption(ctx context.Context, id string) (*store.DelegationAdoption, error) {
	uid, err := parseGetID(id)
	if err != nil {
		return nil, err
	}
	e, err := s.client.DelegationAdoption.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}
	return entDelegationAdoptionToStore(e), nil
}

// ListDelegationAdoptions returns matching records ordered by depth, then
// creation, and the total match count.
func (s *DelegationAdoptionStore) ListDelegationAdoptions(ctx context.Context, filter store.DelegationAdoptionFilter) ([]*store.DelegationAdoption, int, error) {
	q := s.client.DelegationAdoption.Query()
	if filter.CohortID != "" {
		q = q.Where(delegationadoption.CohortIDEQ(filter.CohortID))
	}
	if filter.Origin != "" {
		q = q.Where(delegationadoption.OriginEQ(filter.Origin))
	}
	if filter.Status != "" {
		q = q.Where(delegationadoption.StatusEQ(string(filter.Status)))
	}
	if filter.Reason != "" {
		q = q.Where(delegationadoption.ReasonEQ(filter.Reason))
	}
	if filter.ScopeID != "" {
		q = q.Where(delegationadoption.ScopeIDEQ(filter.ScopeID))
	}
	if filter.DelegateID != "" {
		q = q.Where(delegationadoption.DelegateIDEQ(filter.DelegateID))
	}
	if filter.AdoptedEdgeID != "" {
		q = q.Where(delegationadoption.AdoptedEdgeIDEQ(filter.AdoptedEdgeID))
	}
	total, err := q.Clone().Count(ctx)
	if err != nil {
		return nil, 0, mapError(err)
	}
	q = q.Order(
		ent.Asc(delegationadoption.FieldDepth),
		ent.Asc(delegationadoption.FieldCreated),
		ent.Asc(delegationadoption.FieldID),
	)
	if filter.Offset > 0 {
		q = q.Offset(filter.Offset)
	}
	if filter.Limit > 0 {
		q = q.Limit(filter.Limit)
	}
	rows, err := q.All(ctx)
	if err != nil {
		return nil, 0, mapError(err)
	}
	out := make([]*store.DelegationAdoption, len(rows))
	for i, r := range rows {
		out[i] = entDelegationAdoptionToStore(r)
	}
	return out, total, nil
}
