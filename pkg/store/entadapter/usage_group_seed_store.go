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
	"github.com/GoogleCloudPlatform/scion/pkg/ent/hubsetting"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const groupSeedUserChunkSize = 200

// ComputeUnchargedGroupRollupSeed implements store.UsageBudgetStore.ComputeUnchargedGroupRollupSeed.
// Scans usage_events for memberUserIDs in [fromTS, toTS) (optionally restricted
// to projectScopeID) where groupID is NOT in charged_group_ids using
// dialect-aware JSON containment on Postgres (jsonb @>) and SQLite (json_each).
// Anomalous events are excluded from token counters identically to ingest.
func (s *UsageLedgerStore) ComputeUnchargedGroupRollupSeed(
	ctx context.Context,
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

	drv := s.client.Driver()
	dialectName := drv.Dialect()
	fromUTC := fromTS.UTC().Truncate(time.Microsecond)
	toUTC := toTS.UTC().Truncate(time.Microsecond)

	var groupParam any = groupID
	notChargedClause := "(charged_group_ids IS NULL OR NOT EXISTS (SELECT 1 FROM json_each(usage_events.charged_group_ids) WHERE json_each.value = ?))"
	if dialectName == dialect.Postgres {
		b, err := json.Marshal([]string{groupID})
		if err != nil {
			return nil, fmt.Errorf("marshal group id json: %w", err)
		}
		groupParam = string(b)
		notChargedClause = "(charged_group_ids IS NULL OR NOT (charged_group_ids @> CAST(? AS jsonb)))"
	}

	byDay := make(map[time.Time]*store.UsageGroupRollupDay)

	for start := 0; start < len(memberUserIDs); start += groupSeedUserChunkSize {
		end := start + groupSeedUserChunkSize
		if end > len(memberUserIDs) {
			end = len(memberUserIDs)
		}
		chunk := memberUserIDs[start:end]

		var b strings.Builder
		args := make([]any, 0, len(chunk)+5)
		b.WriteString(`SELECT event_ts, anomalous, input, output, cache_read, cache_write, calls FROM usage_events WHERE billing_user_id IN (`)
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

		q := usageRebindSQL(dialectName, b.String())
		rows := &entsql.Rows{}
		if err := drv.Query(ctx, q, args, rows); err != nil {
			return nil, mapError(err)
		}
		for rows.Next() {
			var evTS time.Time
			var anomalous bool
			var inTok, outTok, crTok, cwTok sql.NullInt64
			var calls int64
			if err := rows.Scan(&evTS, &anomalous, &inTok, &outTok, &crTok, &cwTok, &calls); err != nil {
				_ = rows.Close()
				return nil, mapError(err)
			}
			dayUTC := utcDayStart(evTS.UTC())
			agg, ok := byDay[dayUTC]
			if !ok {
				agg = &store.UsageGroupRollupDay{DayUTC: dayUTC}
				byDay[dayUTC] = agg
			}
			if !anomalous {
				var tokens int64
				if inTok.Valid {
					agg.Input = saturatingAddInt64(agg.Input, inTok.Int64)
					tokens = saturatingAddInt64(tokens, inTok.Int64)
				}
				if outTok.Valid {
					agg.Output = saturatingAddInt64(agg.Output, outTok.Int64)
					tokens = saturatingAddInt64(tokens, outTok.Int64)
				}
				if crTok.Valid {
					agg.CacheRead = saturatingAddInt64(agg.CacheRead, crTok.Int64)
					tokens = saturatingAddInt64(tokens, crTok.Int64)
				}
				if cwTok.Valid {
					agg.CacheWrite = saturatingAddInt64(agg.CacheWrite, cwTok.Int64)
					tokens = saturatingAddInt64(tokens, cwTok.Int64)
				}
				agg.TokensSummable = saturatingAddInt64(agg.TokensSummable, tokens)
			}
			agg.Calls = saturatingAddInt64(agg.Calls, calls)
			agg.Events = saturatingAddInt64(agg.Events, 1)
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

	sorted := make([]store.UsageGroupRollupDay, len(rows))
	copy(sorted, rows)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].DayUTC.Before(sorted[j].DayUTC)
	})

	run := func(client *ent.Client) error {
		drv := client.Driver()
		dialectName := drv.Dialect()
		if dialectName == dialect.Postgres {
			var lockRes sql.Result
			if err := drv.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext('usage_group_seed:' || $1 || ':' || $2))", []any{groupID, projectScopeID}, &lockRes); err != nil {
				return mapError(err)
			}
		}

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

// GetUsageGroupSeedState implements store.UsageBudgetStore.GetUsageGroupSeedState.
func (s *UsageLedgerStore) GetUsageGroupSeedState(ctx context.Context) (*store.UsageGroupSeedState, error) {
	row, err := s.client.HubSetting.Query().
		Where(hubsetting.SectionEQ(store.UsageGroupSeedStateSection)).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return &store.UsageGroupSeedState{Records: make(map[string]*store.UsageGroupSeedRecord)}, nil
		}
		return nil, mapError(err)
	}
	var st store.UsageGroupSeedState
	if err := json.Unmarshal(row.Value, &st); err != nil {
		return nil, fmt.Errorf("parse usage group seed state: %w", err)
	}
	if st.Records == nil {
		st.Records = make(map[string]*store.UsageGroupSeedRecord)
	}
	for _, rec := range st.Records {
		if rec == nil {
			continue
		}
		rec.WindowStartUTC = rec.WindowStartUTC.UTC()
		rec.WindowEndUTC = rec.WindowEndUTC.UTC()
		rec.ActivatedAt = rec.ActivatedAt.UTC()
		rec.CutoffAt = rec.CutoffAt.UTC()
		rec.UpdatedAt = rec.UpdatedAt.UTC()
	}
	return &st, nil
}

// SaveUsageGroupSeedState implements store.UsageBudgetStore.SaveUsageGroupSeedState.
func (s *UsageLedgerStore) SaveUsageGroupSeedState(ctx context.Context, state *store.UsageGroupSeedState) error {
	if state == nil {
		return fmt.Errorf("%w: usage group seed state is required", store.ErrInvalidInput)
	}
	payload, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("marshal usage group seed state: %w", err)
	}
	drv := s.client.Driver()
	dialectName := drv.Dialect()
	valParam := "?"
	if dialectName == dialect.Postgres {
		valParam = "CAST(? AS jsonb)"
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	q := usageRebindSQL(dialectName, fmt.Sprintf(`
INSERT INTO hub_settings (id, section, value, revision, updated_by, origin, create_time, update_time)
VALUES (?, ?, %s, 1, 'seed', 'seeded', ?, ?)
ON CONFLICT (section) DO UPDATE SET
	value = EXCLUDED.value,
	revision = hub_settings.revision + 1,
	update_time = EXCLUDED.update_time`, valParam))
	var res sql.Result
	if err := drv.Exec(ctx, q, []any{uuid.New(), store.UsageGroupSeedStateSection, string(payload), now, now}, &res); err != nil {
		return mapError(err)
	}
	return nil
}

// QueryGroupMemberUsageBreakdown implements store.UsageBudgetStore.QueryGroupMemberUsageBreakdown.
// Aggregates per-member consumption from usage_events over [windowStartUTC,
// windowEndUTC) for (groupID, projectScopeID), combining:
//  1. Ingest-charged events: groupID in charged_group_ids (any billing_user_id,
//     preserving former members' past spend).
//  2. Seeded pre-activation events: uncharged events (groupID NOT in
//     charged_group_ids) in [seedFromTS, seedToTS) by seededMemberUserIDs.
//
// Anomalous events are excluded from token sums identically to rollups.
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

	byUser := make(map[string]*store.UsageGroupMemberBreakdown)
	scanRows := func(rows *entsql.Rows) error {
		for rows.Next() {
			var uid sql.NullString
			var anomalous bool
			var inTok, outTok, crTok, cwTok sql.NullInt64
			var calls int64
			if err := rows.Scan(&uid, &anomalous, &inTok, &outTok, &crTok, &cwTok, &calls); err != nil {
				return mapError(err)
			}
			userKey := ""
			if uid.Valid {
				userKey = uid.String
			}
			agg, ok := byUser[userKey]
			if !ok {
				agg = &store.UsageGroupMemberBreakdown{UserID: userKey}
				byUser[userKey] = agg
			}
			if !anomalous {
				var tokens int64
				if inTok.Valid {
					tokens = saturatingAddInt64(tokens, inTok.Int64)
				}
				if outTok.Valid {
					tokens = saturatingAddInt64(tokens, outTok.Int64)
				}
				if crTok.Valid {
					tokens = saturatingAddInt64(tokens, crTok.Int64)
				}
				if cwTok.Valid {
					tokens = saturatingAddInt64(tokens, cwTok.Int64)
				}
				agg.TokensSummable = saturatingAddInt64(agg.TokensSummable, tokens)
			}
			agg.Calls = saturatingAddInt64(agg.Calls, calls)
			agg.Events = saturatingAddInt64(agg.Events, 1)
		}
		return mapError(rows.Err())
	}

	// Pass 1: ingest-charged events (groupID in charged_group_ids).
	{
		var b strings.Builder
		args := []any{winStart, winEnd}
		b.WriteString(`SELECT billing_user_id, anomalous, input, output, cache_read, cache_write, calls FROM usage_events WHERE event_ts >= ? AND event_ts < ?`)
		if projectScopeID != "" {
			b.WriteString(` AND project_id = ?`)
			args = append(args, projectScopeID)
		}
		b.WriteString(` AND `)
		b.WriteString(chargedClause)
		args = append(args, groupParam)

		rows := &entsql.Rows{}
		if err := drv.Query(ctx, usageRebindSQL(dialectName, b.String()), args, rows); err != nil {
			return nil, mapError(err)
		}
		err := scanRows(rows)
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
	}

	// Pass 2: pre-activation seeded events (groupID NOT in charged_group_ids).
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
				b.WriteString(`SELECT billing_user_id, anomalous, input, output, cache_read, cache_write, calls FROM usage_events WHERE billing_user_id IN (`)
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

				rows := &entsql.Rows{}
				if err := drv.Query(ctx, usageRebindSQL(dialectName, b.String()), args, rows); err != nil {
					return nil, mapError(err)
				}
				err := scanRows(rows)
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
