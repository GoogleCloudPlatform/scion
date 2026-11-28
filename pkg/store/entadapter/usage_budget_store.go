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
	"time"

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/usagebudgetstate"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/usagegrouprollupdaily"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/usagescopecounterdaily"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func entUsageBudgetStateToStore(row *ent.UsageBudgetState) *store.UsageBudgetState {
	if row == nil {
		return nil
	}
	var ackAnomalous *time.Time
	if row.AckAnomalousThrough != nil {
		t := row.AckAnomalousThrough.UTC()
		ackAnomalous = &t
	}
	var overrideUntil *time.Time
	if row.OverrideUntil != nil {
		t := row.OverrideUntil.UTC()
		overrideUntil = &t
	}
	return &store.UsageBudgetState{
		ID:                  row.ID.String(),
		LimitDefinitionID:   row.LimitDefinitionID.String(),
		ScopeKind:           row.ScopeKind,
		ScopeID:             row.ScopeID,
		ProjectScopeID:      row.ProjectScopeID,
		WindowStartUTC:      row.WindowStartUtc.UTC(),
		WindowEndUTC:        row.WindowEndUtc.UTC(),
		State:               row.State,
		ConsumedAtEval:      row.ConsumedAtEval,
		CapAtEval:           row.CapAtEval,
		ThresholdsNotified:  row.ThresholdsNotified,
		AckAnomalousThrough: ackAnomalous,
		OverrideUntil:       overrideUntil,
		StateVersion:        row.StateVersion,
		CreatedAt:           row.CreatedAt.UTC(),
		UpdatedAt:           row.UpdatedAt.UTC(),
	}
}

// SumUsageScopeCounterTokens implements store.UsageBudgetStore.SumUsageScopeCounterTokens.
// Scans at most 31 daily counter rows for a calendar month window and sums
// tokens_summable with int64 saturation so SQLite and Postgres behave identically.
func (s *UsageLedgerStore) SumUsageScopeCounterTokens(ctx context.Context, scopeKind, counterScopeID string, fromDayUTC, toDayUTC time.Time) (int64, error) {
	if scopeKind == "" || counterScopeID == "" {
		return 0, fmt.Errorf("%w: scope_kind and scope_id are required", store.ErrInvalidInput)
	}
	fromUTC := utcDayStart(fromDayUTC)
	toUTC := utcDayStart(toDayUTC)
	if !toUTC.After(fromUTC) {
		return 0, nil
	}

	rows, err := s.client.UsageScopeCounterDaily.Query().
		Where(
			usagescopecounterdaily.ScopeKindEQ(scopeKind),
			usagescopecounterdaily.ScopeIDEQ(counterScopeID),
			usagescopecounterdaily.DayUtcGTE(fromUTC),
			usagescopecounterdaily.DayUtcLT(toUTC),
		).
		All(ctx)
	if err != nil {
		return 0, mapError(err)
	}

	var total int64
	for _, r := range rows {
		if r.TokensSummable > 0 {
			total = saturatingAddInt64(total, r.TokensSummable)
		}
	}
	return total, nil
}

// SumUsageGroupRollupTokens implements store.UsageBudgetStore.SumUsageGroupRollupTokens.
// Scans at most 62 daily rows (31 days x 2 origins: "ingest" and "seed") for a
// calendar month window and sums tokens_summable with int64 saturation so
// SQLite and Postgres behave identically.
func (s *UsageLedgerStore) SumUsageGroupRollupTokens(ctx context.Context, groupID, projectScopeID string, fromDayUTC, toDayUTC time.Time) (int64, error) {
	if groupID == "" {
		return 0, fmt.Errorf("%w: group_id is required", store.ErrInvalidInput)
	}
	fromUTC := utcDayStart(fromDayUTC)
	toUTC := utcDayStart(toDayUTC)
	if !toUTC.After(fromUTC) {
		return 0, nil
	}

	rows, err := s.client.UsageGroupRollupDaily.Query().
		Where(
			usagegrouprollupdaily.GroupIDEQ(groupID),
			usagegrouprollupdaily.ProjectScopeIDEQ(projectScopeID),
			usagegrouprollupdaily.DayUtcGTE(fromUTC),
			usagegrouprollupdaily.DayUtcLT(toUTC),
		).
		All(ctx)
	if err != nil {
		return 0, mapError(err)
	}

	var total int64
	for _, r := range rows {
		if r.TokensSummable > 0 {
			total = saturatingAddInt64(total, r.TokensSummable)
		}
	}
	return total, nil
}

// SumUsageScopeCounterTokensByPrefix implements store.UsageBudgetStore.SumUsageScopeCounterTokensByPrefix.
// Queries all usage_scope_counters_daily rows matching (scopeKind, scopeIDPrefix%)
// over [fromDayUTC, toDayUTC) in one query and returns per-scope saturating sums.
func (s *UsageLedgerStore) SumUsageScopeCounterTokensByPrefix(ctx context.Context, scopeKind, scopeIDPrefix string, fromDayUTC, toDayUTC time.Time) (map[string]int64, error) {
	if scopeKind == "" || scopeIDPrefix == "" {
		return nil, fmt.Errorf("%w: scope_kind and scope_id prefix are required", store.ErrInvalidInput)
	}
	fromUTC := utcDayStart(fromDayUTC)
	toUTC := utcDayStart(toDayUTC)
	if !toUTC.After(fromUTC) {
		return map[string]int64{}, nil
	}

	rows, err := s.client.UsageScopeCounterDaily.Query().
		Where(
			usagescopecounterdaily.ScopeKindEQ(scopeKind),
			usagescopecounterdaily.ScopeIDHasPrefix(scopeIDPrefix),
			usagescopecounterdaily.DayUtcGTE(fromUTC),
			usagescopecounterdaily.DayUtcLT(toUTC),
		).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}

	out := make(map[string]int64)
	for _, r := range rows {
		if r.TokensSummable > 0 {
			out[r.ScopeID] = saturatingAddInt64(out[r.ScopeID], r.TokensSummable)
		} else if _, exists := out[r.ScopeID]; !exists {
			out[r.ScopeID] = 0
		}
	}
	return out, nil
}

// GetUsageBudgetState implements store.UsageBudgetStore.GetUsageBudgetState.
func (s *UsageLedgerStore) GetUsageBudgetState(ctx context.Context, key store.UsageBudgetStateKey) (*store.UsageBudgetState, error) {
	ldUID, err := parseGetID(key.LimitDefinitionID)
	if err != nil {
		return nil, err
	}
	winStart := key.WindowStartUTC.UTC().Truncate(time.Microsecond)
	row, err := s.client.UsageBudgetState.Query().
		Where(
			usagebudgetstate.LimitDefinitionIDEQ(ldUID),
			usagebudgetstate.ScopeKindEQ(key.ScopeKind),
			usagebudgetstate.ScopeIDEQ(key.ScopeID),
			usagebudgetstate.ProjectScopeIDEQ(key.ProjectScopeID),
			usagebudgetstate.WindowStartUtcEQ(winStart),
		).
		Only(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return entUsageBudgetStateToStore(row), nil
}

// GetUsageBudgetStateByID implements store.UsageBudgetStore.GetUsageBudgetStateByID.
func (s *UsageLedgerStore) GetUsageBudgetStateByID(ctx context.Context, id string) (*store.UsageBudgetState, error) {
	uid, err := parseGetID(id)
	if err != nil {
		return nil, err
	}
	row, err := s.client.UsageBudgetState.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}
	return entUsageBudgetStateToStore(row), nil
}

// ListUsageBudgetStatesForScope implements store.UsageBudgetStore.ListUsageBudgetStatesForScope.
func (s *UsageLedgerStore) ListUsageBudgetStatesForScope(ctx context.Context, scopeKind, scopeID, projectScopeID string, activeAt time.Time) ([]*store.UsageBudgetState, error) {
	atUTC := activeAt.UTC().Truncate(time.Microsecond)
	rows, err := s.client.UsageBudgetState.Query().
		Where(
			usagebudgetstate.ScopeKindEQ(scopeKind),
			usagebudgetstate.ScopeIDEQ(scopeID),
			usagebudgetstate.ProjectScopeIDEQ(projectScopeID),
			usagebudgetstate.WindowStartUtcLTE(atUTC),
			usagebudgetstate.WindowEndUtcGT(atUTC),
		).
		Order(ent.Asc(usagebudgetstate.FieldWindowStartUtc)).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]*store.UsageBudgetState, len(rows))
	for i, r := range rows {
		out[i] = entUsageBudgetStateToStore(r)
	}
	return out, nil
}

// ListOpenUsageBudgetStatesByLimit implements store.UsageBudgetStore.ListOpenUsageBudgetStatesByLimit.
func (s *UsageLedgerStore) ListOpenUsageBudgetStatesByLimit(ctx context.Context, limitDefinitionID string, activeAt time.Time, projectID, afterID string, limit int) ([]*store.UsageBudgetState, error) {
	ldUID, err := parseGetID(limitDefinitionID)
	if err != nil {
		return nil, err
	}
	atUTC := activeAt.UTC().Truncate(time.Microsecond)
	q := s.client.UsageBudgetState.Query().
		Where(
			usagebudgetstate.LimitDefinitionIDEQ(ldUID),
			usagebudgetstate.WindowEndUtcGT(atUTC),
		)
	if projectID != "" {
		q = q.Where(usagebudgetstate.Or(
			usagebudgetstate.And(
				usagebudgetstate.ScopeKindEQ(store.UsageScopeKindProject),
				usagebudgetstate.ScopeIDEQ(projectID),
			),
			usagebudgetstate.And(
				usagebudgetstate.ScopeKindEQ(store.UsageScopeKindUserProject),
				usagebudgetstate.ProjectScopeIDEQ(projectID),
			),
		))
	}
	if afterID != "" {
		afterUID, err := parseGetID(afterID)
		if err != nil {
			return nil, err
		}
		q = q.Where(usagebudgetstate.IDGT(afterUID))
	}
	q = q.Order(ent.Asc(usagebudgetstate.FieldID))
	if limit > 0 {
		q = q.Limit(limit)
	}
	rows, err := q.All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]*store.UsageBudgetState, len(rows))
	for i, r := range rows {
		out[i] = entUsageBudgetStateToStore(r)
	}
	return out, nil
}

// ListOpenUsageBudgetStatesForUser implements store.UsageBudgetStore.ListOpenUsageBudgetStatesForUser.
func (s *UsageLedgerStore) ListOpenUsageBudgetStatesForUser(ctx context.Context, userID string, activeAt time.Time) ([]*store.UsageBudgetState, error) {
	if userID == "" {
		return nil, fmt.Errorf("%w: user_id is required", store.ErrInvalidInput)
	}
	atUTC := activeAt.UTC().Truncate(time.Microsecond)
	rows, err := s.client.UsageBudgetState.Query().
		Where(
			usagebudgetstate.ScopeKindIn(store.UsageScopeKindUser, store.UsageScopeKindUserProject),
			usagebudgetstate.ScopeIDEQ(userID),
			usagebudgetstate.WindowEndUtcGT(atUTC),
		).
		Order(ent.Asc(usagebudgetstate.FieldWindowStartUtc), ent.Asc(usagebudgetstate.FieldID)).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]*store.UsageBudgetState, len(rows))
	for i, r := range rows {
		out[i] = entUsageBudgetStateToStore(r)
	}
	return out, nil
}

// CreateUsageBudgetState implements store.UsageBudgetStore.CreateUsageBudgetState.
func (s *UsageLedgerStore) CreateUsageBudgetState(ctx context.Context, st *store.UsageBudgetState) (*store.UsageBudgetState, error) {
	if st == nil {
		return nil, fmt.Errorf("%w: usage budget state is required", store.ErrInvalidInput)
	}
	ldUID, err := parseUUID(st.LimitDefinitionID)
	if err != nil {
		return nil, err
	}
	if st.ScopeKind == "" || st.ScopeID == "" {
		return nil, fmt.Errorf("%w: scope_kind and scope_id are required", store.ErrInvalidInput)
	}
	winStart := st.WindowStartUTC.UTC().Truncate(time.Microsecond)
	winEnd := st.WindowEndUTC.UTC().Truncate(time.Microsecond)
	if !winEnd.After(winStart) {
		return nil, fmt.Errorf("%w: window_end_utc must be after window_start_utc", store.ErrInvalidInput)
	}
	state := st.State
	if state == "" {
		state = store.UsageBudgetStateOK
	}
	version := st.StateVersion
	if version < 1 {
		version = 1
	}
	now := time.Now().UTC().Truncate(time.Microsecond)

	builder := s.client.UsageBudgetState.Create().
		SetLimitDefinitionID(ldUID).
		SetScopeKind(st.ScopeKind).
		SetScopeID(st.ScopeID).
		SetProjectScopeID(st.ProjectScopeID).
		SetWindowStartUtc(winStart).
		SetWindowEndUtc(winEnd).
		SetState(state).
		SetConsumedAtEval(st.ConsumedAtEval).
		SetCapAtEval(st.CapAtEval).
		SetThresholdsNotified(st.ThresholdsNotified).
		SetStateVersion(version).
		SetCreatedAt(now).
		SetUpdatedAt(now)

	if st.ID != "" {
		uid, err := parseUUID(st.ID)
		if err != nil {
			return nil, err
		}
		builder.SetID(uid)
	} else {
		builder.SetID(uuid.New())
	}
	if st.AckAnomalousThrough != nil {
		t := st.AckAnomalousThrough.UTC().Truncate(time.Microsecond)
		builder.SetAckAnomalousThrough(t)
	}
	if st.OverrideUntil != nil {
		t := st.OverrideUntil.UTC().Truncate(time.Microsecond)
		builder.SetOverrideUntil(t)
	}

	created, err := builder.Save(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return entUsageBudgetStateToStore(created), nil
}

// CompareAndSwapUsageBudgetState implements store.UsageBudgetStore.CompareAndSwapUsageBudgetState.
func (s *UsageLedgerStore) CompareAndSwapUsageBudgetState(ctx context.Context, expectedVersion int64, next *store.UsageBudgetState) (*store.UsageBudgetState, error) {
	if next == nil {
		return nil, fmt.Errorf("%w: usage budget state is required", store.ErrInvalidInput)
	}
	uid, err := parseGetID(next.ID)
	if err != nil {
		return nil, store.ErrBudgetStateConflict
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	nextVersion := expectedVersion + 1

	upd := s.client.UsageBudgetState.Update().
		Where(
			usagebudgetstate.IDEQ(uid),
			usagebudgetstate.StateVersionEQ(expectedVersion),
		).
		SetState(next.State).
		SetConsumedAtEval(next.ConsumedAtEval).
		SetCapAtEval(next.CapAtEval).
		SetThresholdsNotified(next.ThresholdsNotified).
		SetStateVersion(nextVersion).
		SetUpdatedAt(now)

	var ackCopy *time.Time
	if next.AckAnomalousThrough != nil {
		t := next.AckAnomalousThrough.UTC().Truncate(time.Microsecond)
		ackCopy = &t
		upd.SetAckAnomalousThrough(t)
	} else {
		upd.ClearAckAnomalousThrough()
	}

	var overrideCopy *time.Time
	if next.OverrideUntil != nil {
		t := next.OverrideUntil.UTC().Truncate(time.Microsecond)
		overrideCopy = &t
		upd.SetOverrideUntil(t)
	} else {
		upd.ClearOverrideUntil()
	}

	n, err := upd.Save(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	if n == 0 {
		return nil, store.ErrBudgetStateConflict
	}

	out := *next
	out.AckAnomalousThrough = ackCopy
	out.OverrideUntil = overrideCopy
	out.StateVersion = nextVersion
	out.UpdatedAt = now
	return &out, nil
}

// SweepUsageBudgetStateBatch implements store.UsageRetentionStore.SweepUsageBudgetStateBatch.
// Deletes up to batchSize closed usage_budget_state rows with window_end_utc < cutoff.
func (s *UsageLedgerStore) SweepUsageBudgetStateBatch(ctx context.Context, cutoff time.Time, batchSize int) (int64, error) {
	const query = `
DELETE FROM usage_budget_state
WHERE id IN (
	SELECT id FROM usage_budget_state
	WHERE window_end_utc < ?
	ORDER BY window_end_utc ASC
	LIMIT ?
)`
	return s.execUsageRetentionDeleteBatch(ctx, query, []any{cutoff.UTC(), batchSize}, batchSize)
}

// SweepUsageGroupRollupsDailyBatch implements store.UsageRetentionStore.SweepUsageGroupRollupsDailyBatch.
// Deletes up to batchSize usage_group_rollups_daily rows with day_utc < cutoff.
func (s *UsageLedgerStore) SweepUsageGroupRollupsDailyBatch(ctx context.Context, cutoff time.Time, batchSize int) (int64, error) {
	const query = `
DELETE FROM usage_group_rollups_daily
WHERE id IN (
	SELECT id FROM usage_group_rollups_daily
	WHERE day_utc < ?
	ORDER BY day_utc ASC
	LIMIT ?
)`
	return s.execUsageRetentionDeleteBatch(ctx, query, []any{cutoff.UTC(), batchSize}, batchSize)
}
