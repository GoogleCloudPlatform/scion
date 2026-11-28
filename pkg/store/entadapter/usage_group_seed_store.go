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
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/usagegroupbudgetseed"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const groupSeedUserChunkSize = 200

func entUsageGroupBudgetSeedToStore(row *ent.UsageGroupBudgetSeed) *store.UsageGroupSeedRecord {
	if row == nil {
		return nil
	}
	var members []string
	if len(row.FrozenMemberIds) > 0 {
		members = make([]string, len(row.FrozenMemberIds))
		copy(members, row.FrozenMemberIds)
	}
	return &store.UsageGroupSeedRecord{
		ID:                row.ID.String(),
		GroupID:           row.GroupID,
		ProjectScopeID:    row.ProjectScopeID,
		WindowKind:        row.WindowKind,
		WindowStartUTC:    row.WindowStartUtc.UTC(),
		WindowEndUTC:      row.WindowEndUtc.UTC(),
		ActivatedAt:       row.ActivatedAt.UTC(),
		CutoffAt:          row.CutoffAt.UTC(),
		FrozenMemberIDs:   members,
		MemberSnapshotSet: row.MemberSnapshotSet,
		Finalized:         row.Finalized,
		StateVersion:      row.StateVersion,
		CreatedAt:         row.CreatedAt.UTC(),
		UpdatedAt:         row.UpdatedAt.UTC(),
	}
}

func sqlBigIntSumExpr(dialectName, expr string) string {
	if dialectName == dialect.Postgres {
		return fmt.Sprintf("CAST(COALESCE(SUM(%s), 0) AS BIGINT)", expr)
	}
	return fmt.Sprintf("COALESCE(SUM(%s), 0)", expr)
}

// ComputeUnchargedGroupRollupSeed implements store.UsageBudgetStore.ComputeUnchargedGroupRollupSeed.
// Aggregates usage_events in SQL (GROUP BY UTC day) for memberUserIDs in
// [fromTS, toTS) (optionally restricted to projectScopeID) where groupID is NOT
// in charged_group_ids using dialect-aware JSON containment on Postgres
// (jsonb @>) and SQLite (json_each). Anomalous events are excluded from token
// counters identically to ingest.
func (s *UsageLedgerStore) ComputeUnchargedGroupRollupSeed(
	ctx context.Context,
	groupID, projectScopeID string,
	memberUserIDs []string,
	fromTS, toTS time.Time,
) ([]store.UsageGroupRollupDay, error) {
	return computeUnchargedGroupRollupSeedWithDriver(ctx, s.client.Driver(), groupID, projectScopeID, memberUserIDs, fromTS, toTS)
}

func computeUnchargedGroupRollupSeedWithDriver(
	ctx context.Context,
	drv dialect.Driver,
	groupID, projectScopeID string,
	memberUserIDs []string,
	fromTS, toTS time.Time,
) ([]store.UsageGroupRollupDay, error) {
	if groupID == "" {
		return nil, fmt.Errorf("%w: group_id is required", store.ErrInvalidInput)
	}
	if len(memberUserIDs) == 0 || !toTS.After(fromTS) {
		return nil, nil
	}

	dialectName := drv.Dialect()
	fromUTC := fromTS.UTC().Truncate(time.Microsecond)
	toUTC := toTS.UTC().Truncate(time.Microsecond)

	dayExpr := "substr(event_ts, 1, 10)"
	var groupParam any = groupID
	notChargedClause := "(charged_group_ids IS NULL OR NOT EXISTS (SELECT 1 FROM json_each(usage_events.charged_group_ids) WHERE json_each.value = ?))"
	if dialectName == dialect.Postgres {
		dayExpr = "to_char(event_ts AT TIME ZONE 'UTC', 'YYYY-MM-DD')"
		b, err := json.Marshal([]string{groupID})
		if err != nil {
			return nil, fmt.Errorf("marshal group id json: %w", err)
		}
		groupParam = string(b)
		notChargedClause = "(charged_group_ids IS NULL OR NOT (charged_group_ids @> CAST(? AS jsonb)))"
	}

	sumTok := sqlBigIntSumExpr(dialectName, "CASE WHEN anomalous THEN 0 ELSE COALESCE(input, 0) + COALESCE(output, 0) + COALESCE(cache_read, 0) + COALESCE(cache_write, 0) END")
	sumIn := sqlBigIntSumExpr(dialectName, "CASE WHEN anomalous THEN 0 ELSE COALESCE(input, 0) END")
	sumOut := sqlBigIntSumExpr(dialectName, "CASE WHEN anomalous THEN 0 ELSE COALESCE(output, 0) END")
	sumCR := sqlBigIntSumExpr(dialectName, "CASE WHEN anomalous THEN 0 ELSE COALESCE(cache_read, 0) END")
	sumCW := sqlBigIntSumExpr(dialectName, "CASE WHEN anomalous THEN 0 ELSE COALESCE(cache_write, 0) END")
	sumCalls := sqlBigIntSumExpr(dialectName, "calls")

	byDay := make(map[time.Time]*store.UsageGroupRollupDay)

	for start := 0; start < len(memberUserIDs); start += groupSeedUserChunkSize {
		end := start + groupSeedUserChunkSize
		if end > len(memberUserIDs) {
			end = len(memberUserIDs)
		}
		chunk := memberUserIDs[start:end]

		var b strings.Builder
		args := make([]any, 0, len(chunk)+5)
		fmt.Fprintf(&b, `SELECT %s AS day_str, %s, %s, %s, %s, %s, %s, COUNT(*) FROM usage_events WHERE billing_user_id IN (`,
			dayExpr, sumTok, sumIn, sumOut, sumCR, sumCW, sumCalls)
		for i, uid := range chunk {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteByte('?')
			args = append(args, uid)
		}
		b.WriteString(`) AND event_ts >= ? AND event_ts < ?`)
		args = append(args, fromUTC, toUTC)
		if projectScopeID != "" {
			b.WriteString(` AND project_id = ?`)
			args = append(args, projectScopeID)
		}
		b.WriteString(` AND `)
		b.WriteString(notChargedClause)
		args = append(args, groupParam)
		fmt.Fprintf(&b, ` GROUP BY %s`, dayExpr)

		q := usageRebindSQL(dialectName, b.String())
		rows := &entsql.Rows{}
		if err := drv.Query(ctx, q, args, rows); err != nil {
			return nil, mapError(err)
		}
		for rows.Next() {
			var dayStr string
			var tokens, inTok, outTok, crTok, cwTok, calls, events int64
			if err := rows.Scan(&dayStr, &tokens, &inTok, &outTok, &crTok, &cwTok, &calls, &events); err != nil {
				_ = rows.Close()
				return nil, mapError(err)
			}
			dayUTC, err := time.Parse("2006-01-02", dayStr)
			if err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("parse day_utc %q: %w", dayStr, err)
			}
			dayUTC = dayUTC.UTC()
			agg, ok := byDay[dayUTC]
			if !ok {
				agg = &store.UsageGroupRollupDay{DayUTC: dayUTC}
				byDay[dayUTC] = agg
			}
			agg.TokensSummable = saturatingAddInt64(agg.TokensSummable, tokens)
			agg.Input = saturatingAddInt64(agg.Input, inTok)
			agg.Output = saturatingAddInt64(agg.Output, outTok)
			agg.CacheRead = saturatingAddInt64(agg.CacheRead, crTok)
			agg.CacheWrite = saturatingAddInt64(agg.CacheWrite, cwTok)
			agg.Calls = saturatingAddInt64(agg.Calls, calls)
			agg.Events = saturatingAddInt64(agg.Events, events)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, mapError(err)
		}
		_ = rows.Close()
	}

	out := make([]store.UsageGroupRollupDay, 0, len(byDay))
	for _, agg := range byDay {
		out = append(out, *agg)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].DayUTC.Before(out[j].DayUTC)
	})
	return out, nil
}

// ReplaceUsageGroupRollupSeed implements store.UsageBudgetStore.ReplaceUsageGroupRollupSeed.
// Atomically replaces (SET, never adds) all origin="seed" rows for
// (groupID, projectScopeID) within [windowStartUTC, windowEndUTC), never
// touching origin="ingest" rows.
func (s *UsageLedgerStore) ReplaceUsageGroupRollupSeed(
	ctx context.Context,
	groupID, projectScopeID string,
	windowStartUTC, windowEndUTC time.Time,
	rows []store.UsageGroupRollupDay,
) error {
	if groupID == "" {
		return fmt.Errorf("%w: group_id is required", store.ErrInvalidInput)
	}
	winStart := utcDayStart(windowStartUTC)
	winEnd := utcDayStart(windowEndUTC)
	if !winEnd.After(winStart) {
		return nil
	}

	run := func(client *ent.Client) error {
		drv := client.Driver()
		if drv.Dialect() == dialect.Postgres {
			var lockRes sql.Result
			if err := drv.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext('usage_group_seed:' || $1 || ':' || $2))", []any{groupID, projectScopeID}, &lockRes); err != nil {
				return mapError(err)
			}
		}
		return replaceUsageGroupRollupSeedWithDriver(ctx, drv, groupID, projectScopeID, winStart, winEnd, rows)
	}

	if s.inTx {
		return run(s.client)
	}
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return mapError(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := run(tx.Client()); err != nil {
		return err
	}
	return mapError(tx.Commit())
}

func replaceUsageGroupRollupSeedWithDriver(
	ctx context.Context,
	drv dialect.Driver,
	groupID, projectScopeID string,
	winStart, winEnd time.Time,
	rows []store.UsageGroupRollupDay,
) error {
	dialectName := drv.Dialect()
	sorted := make([]store.UsageGroupRollupDay, len(rows))
	copy(sorted, rows)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].DayUTC.Before(sorted[j].DayUTC)
	})

	delSQL := usageRebindSQL(dialectName, `
DELETE FROM usage_group_rollups_daily
WHERE group_id = ?
  AND project_scope_id = ?
  AND origin = ?
  AND day_utc >= ?
  AND day_utc < ?`)
	var delRes sql.Result
	if err := drv.Exec(ctx, delSQL, []any{groupID, projectScopeID, store.UsageGroupRollupOriginSeed, winStart, winEnd}, &delRes); err != nil {
		return mapError(err)
	}

	if len(sorted) == 0 {
		return nil
	}

	setSQL := usageRebindSQL(dialectName, `
INSERT INTO usage_group_rollups_daily (
	id, day_utc, group_id, project_scope_id, origin,
	tokens_summable, input, output, cache_read, cache_write, calls, events
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (day_utc, group_id, project_scope_id, origin)
DO UPDATE SET
	tokens_summable = EXCLUDED.tokens_summable,
	input = EXCLUDED.input,
	output = EXCLUDED.output,
	cache_read = EXCLUDED.cache_read,
	cache_write = EXCLUDED.cache_write,
	calls = EXCLUDED.calls,
	events = EXCLUDED.events`)

	for _, r := range sorted {
		day := utcDayStart(r.DayUTC)
		if day.Before(winStart) || !day.Before(winEnd) {
			continue
		}
		var res sql.Result
		args := []any{
			uuid.New(),
			day,
			groupID,
			projectScopeID,
			store.UsageGroupRollupOriginSeed,
			r.TokensSummable,
			r.Input,
			r.Output,
			r.CacheRead,
			r.CacheWrite,
			r.Calls,
			r.Events,
		}
		if err := drv.Exec(ctx, setSQL, args, &res); err != nil {
			return mapError(err)
		}
	}
	return nil
}

func acquireGroupSeedAdvisoryLock(ctx context.Context, drv dialect.Driver, groupID, projectScopeID, windowKind string, winStart time.Time) error {
	if drv.Dialect() != dialect.Postgres {
		return nil
	}
	var lockRes sql.Result
	lockKey := fmt.Sprintf("usage_group_seed:%s:%s:%s:%s", groupID, projectScopeID, windowKind, winStart.UTC().Format("2006-01-02"))
	if err := drv.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", []any{lockKey}, &lockRes); err != nil {
		return mapError(err)
	}
	return nil
}

// RecordUsageGroupSeedActivation implements store.UsageBudgetStore.RecordUsageGroupSeedActivation.
// Records an explicit binding activation in usage_group_budget_seeds under the
// per-key advisory lock. Never resets an existing record unless the new
// ActivatedAt is strictly newer than the stored ActivatedAt.
func (s *UsageLedgerStore) RecordUsageGroupSeedActivation(ctx context.Context, rec *store.UsageGroupSeedRecord) (*store.UsageGroupSeedRecord, error) {
	if rec == nil || rec.GroupID == "" || rec.WindowKind == "" || !rec.WindowEndUTC.After(rec.WindowStartUTC) {
		return nil, fmt.Errorf("%w: invalid group seed activation record", store.ErrInvalidInput)
	}
	winStart := utcDayStart(rec.WindowStartUTC)
	winEnd := utcDayStart(rec.WindowEndUTC)
	activatedAt := rec.ActivatedAt.UTC().Truncate(time.Microsecond)
	cutoffAt := rec.CutoffAt.UTC().Truncate(time.Microsecond)
	if cutoffAt.IsZero() {
		cutoffAt = activatedAt.Add(2 * time.Minute)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)

	var out *store.UsageGroupSeedRecord
	run := func(client *ent.Client) error {
		if err := acquireGroupSeedAdvisoryLock(ctx, client.Driver(), rec.GroupID, rec.ProjectScopeID, rec.WindowKind, winStart); err != nil {
			return err
		}
		existing, err := client.UsageGroupBudgetSeed.Query().
			Where(
				usagegroupbudgetseed.GroupIDEQ(rec.GroupID),
				usagegroupbudgetseed.ProjectScopeIDEQ(rec.ProjectScopeID),
				usagegroupbudgetseed.WindowKindEQ(rec.WindowKind),
				usagegroupbudgetseed.WindowStartUtcEQ(winStart),
			).
			Only(ctx)
		if err != nil && !ent.IsNotFound(err) {
			return mapError(err)
		}
		if existing == nil {
			created, err := client.UsageGroupBudgetSeed.Create().
				SetID(uuid.New()).
				SetGroupID(rec.GroupID).
				SetProjectScopeID(rec.ProjectScopeID).
				SetWindowKind(rec.WindowKind).
				SetWindowStartUtc(winStart).
				SetWindowEndUtc(winEnd).
				SetActivatedAt(activatedAt).
				SetCutoffAt(cutoffAt).
				SetMemberSnapshotSet(false).
				SetFinalized(false).
				SetStateVersion(1).
				SetCreatedAt(now).
				SetUpdatedAt(now).
				Save(ctx)
			if err != nil {
				return mapError(err)
			}
			out = entUsageGroupBudgetSeedToStore(created)
			return nil
		}

		// Older or identical activation does not reset existing seed state.
		if !activatedAt.After(existing.ActivatedAt.UTC()) {
			out = entUsageGroupBudgetSeedToStore(existing)
			return nil
		}

		// Genuine newer re-activation resets snapshot and finality via CAS.
		updated, err := client.UsageGroupBudgetSeed.UpdateOneID(existing.ID).
			Where(usagegroupbudgetseed.StateVersionEQ(existing.StateVersion)).
			SetActivatedAt(activatedAt).
			SetCutoffAt(cutoffAt).
			ClearFrozenMemberIds().
			SetMemberSnapshotSet(false).
			SetFinalized(false).
			SetStateVersion(existing.StateVersion + 1).
			SetUpdatedAt(now).
			Save(ctx)
		if err != nil {
			return mapError(err)
		}
		out = entUsageGroupBudgetSeedToStore(updated)
		return nil
	}

	if s.inTx {
		if err := run(s.client); err != nil {
			return nil, err
		}
		return out, nil
	}
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := run(tx.Client()); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, mapError(err)
	}
	return out, nil
}

// ExecuteUsageGroupBudgetSeedPass implements store.UsageBudgetStore.ExecuteUsageGroupBudgetSeedPass.
// Runs the full D6 seed pass (load -> snapshot effective members if unset ->
// compute uncharged events -> SET origin="seed" rollups -> finalize if
// activatedAt matches -> save record) atomically inside one per-key locked tx.
func (s *UsageLedgerStore) ExecuteUsageGroupBudgetSeedPass(ctx context.Context, in store.UsageGroupSeedPassInput) (*store.UsageGroupSeedRecord, bool, error) {
	if in.GroupID == "" || in.WindowKind == "" || !in.WindowEndUTC.After(in.WindowStartUTC) {
		return nil, false, fmt.Errorf("%w: invalid group budget seed pass input", store.ErrInvalidInput)
	}
	winStart := utcDayStart(in.WindowStartUTC)
	winEnd := utcDayStart(in.WindowEndUTC)
	now := in.Now.UTC().Truncate(time.Microsecond)
	if now.IsZero() {
		now = time.Now().UTC().Truncate(time.Microsecond)
	}
	activatedAt := in.ActivatedAt.UTC().Truncate(time.Microsecond)
	if activatedAt.IsZero() {
		activatedAt = now
	}
	cutoffAt := in.CutoffAt.UTC().Truncate(time.Microsecond)
	if cutoffAt.IsZero() {
		cutoffAt = activatedAt.Add(2 * time.Minute)
	}

	var out *store.UsageGroupSeedRecord
	var didMutate bool

	run := func(client *ent.Client) error {
		drv := client.Driver()
		if err := acquireGroupSeedAdvisoryLock(ctx, drv, in.GroupID, in.ProjectScopeID, in.WindowKind, winStart); err != nil {
			return err
		}

		existing, err := client.UsageGroupBudgetSeed.Query().
			Where(
				usagegroupbudgetseed.GroupIDEQ(in.GroupID),
				usagegroupbudgetseed.ProjectScopeIDEQ(in.ProjectScopeID),
				usagegroupbudgetseed.WindowKindEQ(in.WindowKind),
				usagegroupbudgetseed.WindowStartUtcEQ(winStart),
			).
			Only(ctx)
		if err != nil && !ent.IsNotFound(err) {
			return mapError(err)
		}

		// D6 finality: once finalized for this activation (or a newer one),
		// never recompute from later membership.
		if existing != nil && existing.Finalized && !activatedAt.After(existing.ActivatedAt.UTC()) {
			out = entUsageGroupBudgetSeedToStore(existing)
			didMutate = false
			return nil
		}

		// O2 guard: if an older activation item arrives after a newer
		// activation was recorded, ignore it rather than overwriting or
		// prematurely finalizing the newer activation.
		if existing != nil && activatedAt.Before(existing.ActivatedAt.UTC()) {
			out = entUsageGroupBudgetSeedToStore(existing)
			didMutate = false
			return nil
		}

		var frozenMembers []string
		memberSnapshotSet := false
		effectiveActivatedAt := activatedAt
		effectiveCutoffAt := cutoffAt

		if existing != nil && !activatedAt.After(existing.ActivatedAt.UTC()) {
			effectiveActivatedAt = existing.ActivatedAt.UTC()
			effectiveCutoffAt = existing.CutoffAt.UTC()
			memberSnapshotSet = existing.MemberSnapshotSet
			if len(existing.FrozenMemberIds) > 0 {
				frozenMembers = make([]string, len(existing.FrozenMemberIds))
				copy(frozenMembers, existing.FrozenMemberIds)
			}
		}

		// Snapshot effective members on the first seed run only (D6).
		if !memberSnapshotSet {
			gs := &GroupStore{client: client}
			members, err := gs.GetEffectiveGroupMemberUserIDs(ctx, in.GroupID)
			if err != nil {
				return fmt.Errorf("resolve effective members for group %s: %w", in.GroupID, err)
			}
			frozenMembers = members
			memberSnapshotSet = true
		}

		// Bound seed scan to [max(winStart, ledgerRetentionCutoff), min(winEnd, effectiveCutoffAt)).
		fromTS := winStart
		if !in.LedgerRetentionCutoff.IsZero() && in.LedgerRetentionCutoff.UTC().After(fromTS) {
			fromTS = in.LedgerRetentionCutoff.UTC()
		}
		toTS := effectiveCutoffAt
		if winEnd.Before(toTS) {
			toTS = winEnd
		}

		var days []store.UsageGroupRollupDay
		if len(frozenMembers) > 0 && toTS.After(fromTS) {
			days, err = computeUnchargedGroupRollupSeedWithDriver(
				ctx, drv, in.GroupID, in.ProjectScopeID, frozenMembers, fromTS, toTS,
			)
			if err != nil {
				return fmt.Errorf("compute uncharged group seed: %w", err)
			}
		}

		if err := replaceUsageGroupRollupSeedWithDriver(ctx, drv, in.GroupID, in.ProjectScopeID, winStart, winEnd, days); err != nil {
			return fmt.Errorf("replace group rollup seed: %w", err)
		}

		// O2: finalize ONLY when the item's activation matches the record's
		// activation and either this is the +2m final pass or now >= cutoff.
		finalized := activatedAt.Equal(effectiveActivatedAt) && (in.IsFinalPass || !now.Before(effectiveCutoffAt))

		if existing == nil {
			created, err := client.UsageGroupBudgetSeed.Create().
				SetID(uuid.New()).
				SetGroupID(in.GroupID).
				SetProjectScopeID(in.ProjectScopeID).
				SetWindowKind(in.WindowKind).
				SetWindowStartUtc(winStart).
				SetWindowEndUtc(winEnd).
				SetActivatedAt(effectiveActivatedAt).
				SetCutoffAt(effectiveCutoffAt).
				SetFrozenMemberIds(frozenMembers).
				SetMemberSnapshotSet(memberSnapshotSet).
				SetFinalized(finalized).
				SetStateVersion(1).
				SetCreatedAt(now).
				SetUpdatedAt(now).
				Save(ctx)
			if err != nil {
				return mapError(err)
			}
			out = entUsageGroupBudgetSeedToStore(created)
			didMutate = true
			return nil
		}

		updated, err := client.UsageGroupBudgetSeed.UpdateOneID(existing.ID).
			Where(usagegroupbudgetseed.StateVersionEQ(existing.StateVersion)).
			SetActivatedAt(effectiveActivatedAt).
			SetCutoffAt(effectiveCutoffAt).
			SetFrozenMemberIds(frozenMembers).
			SetMemberSnapshotSet(memberSnapshotSet).
			SetFinalized(finalized).
			SetStateVersion(existing.StateVersion + 1).
			SetUpdatedAt(now).
			Save(ctx)
		if err != nil {
			return mapError(err)
		}
		out = entUsageGroupBudgetSeedToStore(updated)
		didMutate = true
		return nil
	}

	if s.inTx {
		if err := run(s.client); err != nil {
			return nil, false, err
		}
		return out, didMutate, nil
	}
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return nil, false, mapError(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := run(tx.Client()); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, mapError(err)
	}
	return out, didMutate, nil
}

// GetUsageGroupSeedRecord implements store.UsageBudgetStore.GetUsageGroupSeedRecord.
func (s *UsageLedgerStore) GetUsageGroupSeedRecord(ctx context.Context, groupID, projectScopeID, windowKind string, windowStartUTC time.Time) (*store.UsageGroupSeedRecord, error) {
	if groupID == "" || windowKind == "" {
		return nil, fmt.Errorf("%w: group_id and window_kind are required", store.ErrInvalidInput)
	}
	winStart := utcDayStart(windowStartUTC)
	row, err := s.client.UsageGroupBudgetSeed.Query().
		Where(
			usagegroupbudgetseed.GroupIDEQ(groupID),
			usagegroupbudgetseed.ProjectScopeIDEQ(projectScopeID),
			usagegroupbudgetseed.WindowKindEQ(windowKind),
			usagegroupbudgetseed.WindowStartUtcEQ(winStart),
		).
		Only(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return entUsageGroupBudgetSeedToStore(row), nil
}

// ListIncompleteUsageGroupSeeds implements store.UsageBudgetStore.ListIncompleteUsageGroupSeeds.
func (s *UsageLedgerStore) ListIncompleteUsageGroupSeeds(ctx context.Context, activeAt time.Time) ([]*store.UsageGroupSeedRecord, error) {
	atUTC := activeAt.UTC().Truncate(time.Microsecond)
	rows, err := s.client.UsageGroupBudgetSeed.Query().
		Where(
			usagegroupbudgetseed.FinalizedEQ(false),
			usagegroupbudgetseed.WindowEndUtcGT(atUTC),
		).
		Order(ent.Asc(usagegroupbudgetseed.FieldWindowStartUtc)).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]*store.UsageGroupSeedRecord, len(rows))
	for i, r := range rows {
		out[i] = entUsageGroupBudgetSeedToStore(r)
	}
	return out, nil
}

// QueryGroupMemberUsageBreakdown implements store.UsageBudgetStore.QueryGroupMemberUsageBreakdown.
// Aggregates per-member consumption in SQL (GROUP BY billing_user_id) from
// usage_events over [windowStartUTC, windowEndUTC) for (groupID, projectScopeID),
// returning O(members) rows over the wire rather than streaming individual events.
//
// Combines:
//  1. Ingest-charged events: groupID in charged_group_ids (any billing_user_id,
//     preserving former members' past spend).
//  2. Seeded pre-activation events: uncharged events (groupID NOT in
//     charged_group_ids) in [seedFromTS, seedToTS) by seededMemberUserIDs.
//
// Note: for system-wide pools without a project_id filter, Pass 1 range-scans
// usage_events over the window via the event_ts index and evaluates JSON
// containment in SQL before grouping. Anomalous events are excluded from token
// sums identically to rollups.
func (s *UsageLedgerStore) QueryGroupMemberUsageBreakdown(
	ctx context.Context,
	groupID, projectScopeID string,
	seededMemberUserIDs []string,
	seedFromTS, seedToTS, windowStartUTC, windowEndUTC time.Time,
) ([]store.UsageGroupMemberBreakdown, error) {
	if groupID == "" {
		return nil, fmt.Errorf("%w: group_id is required", store.ErrInvalidInput)
	}
	winStart := windowStartUTC.UTC().Truncate(time.Microsecond)
	winEnd := windowEndUTC.UTC().Truncate(time.Microsecond)
	if !winEnd.After(winStart) {
		return []store.UsageGroupMemberBreakdown{}, nil
	}

	drv := s.client.Driver()
	dialectName := drv.Dialect()

	var groupParam any = groupID
	chargedClause := "(charged_group_ids IS NOT NULL AND EXISTS (SELECT 1 FROM json_each(usage_events.charged_group_ids) WHERE json_each.value = ?))"
	notChargedClause := "(charged_group_ids IS NULL OR NOT EXISTS (SELECT 1 FROM json_each(usage_events.charged_group_ids) WHERE json_each.value = ?))"
	if dialectName == dialect.Postgres {
		b, err := json.Marshal([]string{groupID})
		if err != nil {
			return nil, fmt.Errorf("marshal group id json: %w", err)
		}
		groupParam = string(b)
		chargedClause = "(charged_group_ids IS NOT NULL AND (charged_group_ids @> CAST(? AS jsonb)))"
		notChargedClause = "(charged_group_ids IS NULL OR NOT (charged_group_ids @> CAST(? AS jsonb)))"
	}

	sumTok := sqlBigIntSumExpr(dialectName, "CASE WHEN anomalous THEN 0 ELSE COALESCE(input, 0) + COALESCE(output, 0) + COALESCE(cache_read, 0) + COALESCE(cache_write, 0) END")
	sumCalls := sqlBigIntSumExpr(dialectName, "calls")

	byUser := make(map[string]*store.UsageGroupMemberBreakdown)
	scanGrouped := func(rows *entsql.Rows) error {
		for rows.Next() {
			var uid string
			var tokens, calls, events int64
			if err := rows.Scan(&uid, &tokens, &calls, &events); err != nil {
				return mapError(err)
			}
			agg, ok := byUser[uid]
			if !ok {
				agg = &store.UsageGroupMemberBreakdown{UserID: uid}
				byUser[uid] = agg
			}
			agg.TokensSummable = saturatingAddInt64(agg.TokensSummable, tokens)
			agg.Calls = saturatingAddInt64(agg.Calls, calls)
			agg.Events = saturatingAddInt64(agg.Events, events)
		}
		return mapError(rows.Err())
	}

	// Pass 1: ingest-charged events (groupID in charged_group_ids), grouped in SQL.
	{
		var b strings.Builder
		args := []any{winStart, winEnd}
		fmt.Fprintf(&b, `SELECT COALESCE(billing_user_id, ''), %s, %s, COUNT(*) FROM usage_events WHERE event_ts >= ? AND event_ts < ?`, sumTok, sumCalls)
		if projectScopeID != "" {
			b.WriteString(` AND project_id = ?`)
			args = append(args, projectScopeID)
		}
		b.WriteString(` AND `)
		b.WriteString(chargedClause)
		args = append(args, groupParam)
		b.WriteString(` GROUP BY COALESCE(billing_user_id, '')`)

		rows := &entsql.Rows{}
		if err := drv.Query(ctx, usageRebindSQL(dialectName, b.String()), args, rows); err != nil {
			return nil, mapError(err)
		}
		err := scanGrouped(rows)
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
	}

	// Pass 2: pre-activation seeded events (groupID NOT in charged_group_ids), grouped in SQL.
	if len(seededMemberUserIDs) > 0 && seedToTS.After(seedFromTS) {
		effFrom := seedFromTS.UTC().Truncate(time.Microsecond)
		if effFrom.Before(winStart) {
			effFrom = winStart
		}
		effTo := seedToTS.UTC().Truncate(time.Microsecond)
		if effTo.After(winEnd) {
			effTo = winEnd
		}
		if effTo.After(effFrom) {
			for start := 0; start < len(seededMemberUserIDs); start += groupSeedUserChunkSize {
				end := start + groupSeedUserChunkSize
				if end > len(seededMemberUserIDs) {
					end = len(seededMemberUserIDs)
				}
				chunk := seededMemberUserIDs[start:end]

				var b strings.Builder
				args := make([]any, 0, len(chunk)+5)
				fmt.Fprintf(&b, `SELECT COALESCE(billing_user_id, ''), %s, %s, COUNT(*) FROM usage_events WHERE billing_user_id IN (`, sumTok, sumCalls)
				for i, uid := range chunk {
					if i > 0 {
						b.WriteByte(',')
					}
					b.WriteByte('?')
					args = append(args, uid)
				}
				b.WriteString(`) AND event_ts >= ? AND event_ts < ?`)
				args = append(args, effFrom, effTo)
				if projectScopeID != "" {
					b.WriteString(` AND project_id = ?`)
					args = append(args, projectScopeID)
				}
				b.WriteString(` AND `)
				b.WriteString(notChargedClause)
				args = append(args, groupParam)
				b.WriteString(` GROUP BY COALESCE(billing_user_id, '')`)

				rows := &entsql.Rows{}
				if err := drv.Query(ctx, usageRebindSQL(dialectName, b.String()), args, rows); err != nil {
					return nil, mapError(err)
				}
				err := scanGrouped(rows)
				_ = rows.Close()
				if err != nil {
					return nil, err
				}
			}
		}
	}

	out := make([]store.UsageGroupMemberBreakdown, 0, len(byUser))
	for _, agg := range byUser {
		out = append(out, *agg)
	}
	// Deterministic order: highest token consumption first, then user ID.
	sort.Slice(out, func(i, j int) bool {
		if out[i].TokensSummable != out[j].TokensSummable {
			return out[i].TokensSummable > out[j].TokensSummable
		}
		return out[i].UserID < out[j].UserID
	})
	return out, nil
}
