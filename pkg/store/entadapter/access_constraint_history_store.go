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

	"entgo.io/ent/dialect/sql"
	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/accessconstraint"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/accessconstrainthistory"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const accessConstraintHistoryLimit = 1000

var errConstraintHistoryTransactionRequired = errors.New("access constraint history append requires Store.WithTx")

// AppendConstraintHistoryTx inserts one live-boundary history row and enforces
// the retention cap as one transactionally inseparable operation. Locking the
// live constraint serializes concurrent PostgreSQL writers for that constraint.
func (s *AccessConstraintStore) AppendConstraintHistoryTx(ctx context.Context, entry *store.AccessConstraintHistory) error {
	if !s.inTx {
		return errConstraintHistoryTransactionRequired
	}
	constraintID, err := uuid.Parse(entry.ConstraintID)
	if err != nil {
		return fmt.Errorf("invalid constraint ID: %w", store.ErrInvalidInput)
	}
	constraintQuery := s.client.AccessConstraint.Query().Where(accessconstraint.IDEQ(constraintID))
	if s.usesRowLocks(ctx) {
		constraintQuery = constraintQuery.ForUpdate()
	}
	if _, err := constraintQuery.Only(ctx); err != nil {
		return fmt.Errorf("lock access constraint for history append: %w", mapError(err))
	}
	builder := s.client.AccessConstraintHistory.Create().
		SetID(entry.EventID).
		SetConstraintID(constraintID).
		SetOccurredAt(entry.OccurredAt).
		SetOperation(entry.Operation)
	setOptionalHistoryFields(builder, entry)
	if _, err := builder.Save(ctx); err != nil {
		return mapError(err)
	}
	return s.pruneConstraintHistory(ctx, constraintID)
}

func setOptionalHistoryFields(builder *ent.AccessConstraintHistoryCreate, entry *store.AccessConstraintHistory) {
	if entry.ActorKind != "" {
		builder.SetActorKind(entry.ActorKind)
	}
	if entry.ActorID != "" {
		builder.SetActorID(entry.ActorID)
	}
	if entry.CorrelationID != "" {
		builder.SetCorrelationID(entry.CorrelationID)
	}
	if entry.BatchOperationID != "" {
		builder.SetBatchOperationID(entry.BatchOperationID)
	}
	builder.SetNillableBeforeRevision(entry.BeforeRevision).
		SetNillableAfterRevision(entry.AfterRevision)
	if entry.Classification != "" {
		builder.SetClassification(entry.Classification)
	}
	if entry.PreviewID != "" {
		builder.SetPreviewID(entry.PreviewID)
	}
	if entry.DraftHash != "" {
		builder.SetDraftHash(entry.DraftHash)
	}
	if entry.ImpactCountsJSON != "" {
		builder.SetImpactCountsJSON(entry.ImpactCountsJSON)
	}
	if entry.ChangedFieldsJSON != "" {
		builder.SetChangedFieldsJSON(entry.ChangedFieldsJSON)
	}
}

func (s *AccessConstraintStore) pruneConstraintHistory(ctx context.Context, constraintID uuid.UUID) error {
	ids, err := s.client.AccessConstraintHistory.Query().
		Where(accessconstrainthistory.ConstraintIDEQ(constraintID)).
		Order(
			accessconstrainthistory.ByOccurredAt(sql.OrderDesc()),
			accessconstrainthistory.ByID(sql.OrderDesc()),
		).
		Offset(accessConstraintHistoryLimit).
		IDs(ctx)
	if err != nil {
		return fmt.Errorf("select access constraint history for pruning: %w", err)
	}
	if len(ids) == 0 {
		return nil
	}
	if _, err := s.client.AccessConstraintHistory.Delete().
		Where(accessconstrainthistory.IDIn(ids...)).
		Exec(ctx); err != nil {
		return fmt.Errorf("prune access constraint history: %w", err)
	}
	return nil
}

// ListConstraintHistory returns the retained live timeline newest-first.
func (s *AccessConstraintStore) ListConstraintHistory(ctx context.Context, constraintID string) ([]*store.AccessConstraintHistory, error) {
	uid, err := uuid.Parse(constraintID)
	if err != nil {
		return nil, fmt.Errorf("invalid constraint ID: %w", store.ErrInvalidInput)
	}
	rows, err := s.client.AccessConstraintHistory.Query().
		Where(accessconstrainthistory.ConstraintIDEQ(uid)).
		Order(
			accessconstrainthistory.ByOccurredAt(sql.OrderDesc()),
			accessconstrainthistory.ByID(sql.OrderDesc()),
		).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("list access constraint history: %w", err)
	}
	result := make([]*store.AccessConstraintHistory, len(rows))
	for i, row := range rows {
		result[i] = entConstraintHistoryToStore(row)
	}
	return result, nil
}

func entConstraintHistoryToStore(row *ent.AccessConstraintHistory) *store.AccessConstraintHistory {
	result := &store.AccessConstraintHistory{
		EventID:        row.ID,
		ConstraintID:   row.ConstraintID.String(),
		OccurredAt:     row.OccurredAt,
		Operation:      row.Operation,
		BeforeRevision: row.BeforeRevision,
		AfterRevision:  row.AfterRevision,
	}
	copyOptionalString(row.ActorKind, &result.ActorKind)
	copyOptionalString(row.ActorID, &result.ActorID)
	copyOptionalString(row.CorrelationID, &result.CorrelationID)
	copyOptionalString(row.BatchOperationID, &result.BatchOperationID)
	copyOptionalString(row.Classification, &result.Classification)
	copyOptionalString(row.PreviewID, &result.PreviewID)
	copyOptionalString(row.DraftHash, &result.DraftHash)
	copyOptionalString(row.ImpactCountsJSON, &result.ImpactCountsJSON)
	copyOptionalString(row.ChangedFieldsJSON, &result.ChangedFieldsJSON)
	return result
}

func copyOptionalString(source *string, destination *string) {
	if source != nil {
		*destination = *source
	}
}
