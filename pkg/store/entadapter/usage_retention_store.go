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
	"time"

	"entgo.io/ent/dialect"
	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/hubsetting"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// GetUsageRetentionState implements store.UsageRetentionStore.GetUsageRetentionState.
func (s *UsageLedgerStore) GetUsageRetentionState(ctx context.Context) (*store.UsageRetentionState, error) {
	row, err := s.client.HubSetting.Query().
		Where(hubsetting.SectionEQ(store.UsageRetentionStateSection)).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return &store.UsageRetentionState{}, nil
		}
		return nil, mapError(err)
	}
	var st store.UsageRetentionState
	if err := json.Unmarshal(row.Value, &st); err != nil {
		return nil, fmt.Errorf("parse usage retention state: %w", err)
	}
	if st.LastCompletedAt != nil {
		t := st.LastCompletedAt.UTC()
		st.LastCompletedAt = &t
	}
	if st.HourlySweptThrough != nil {
		t := utcHourStart(*st.HourlySweptThrough)
		st.HourlySweptThrough = &t
	}
	return &st, nil
}

// SaveUsageRetentionState implements store.UsageRetentionStore.SaveUsageRetentionState.
func (s *UsageLedgerStore) SaveUsageRetentionState(ctx context.Context, state *store.UsageRetentionState) error {
	if state == nil {
		return fmt.Errorf("%w: usage retention state is required", store.ErrInvalidInput)
	}
	normalized := *state
	if normalized.LastCompletedAt != nil {
		t := normalized.LastCompletedAt.UTC().Truncate(time.Microsecond)
		normalized.LastCompletedAt = &t
	}
	if normalized.HourlySweptThrough != nil {
		t := utcHourStart(*normalized.HourlySweptThrough)
		normalized.HourlySweptThrough = &t
	}
	payload, err := json.Marshal(&normalized)
	if err != nil {
		return fmt.Errorf("marshal usage retention state: %w", err)
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
VALUES (?, ?, %s, 1, 'system', 'seeded', ?, ?)
ON CONFLICT (section) DO UPDATE SET
	value = EXCLUDED.value,
	revision = hub_settings.revision + 1,
	update_time = EXCLUDED.update_time`, valParam))
	var res sql.Result
	if err := drv.Exec(ctx, q, []any{uuid.New(), store.UsageRetentionStateSection, string(payload), now, now}, &res); err != nil {
		return mapError(err)
	}
	return nil
}

// execUsageRetentionDeleteBatch executes a bounded DELETE statement inside its
// own transaction (unless the store is already bound to a tx) and returns the
// number of deleted rows. Portable across SQLite and PostgreSQL via
// usageRebindSQL.
func (s *UsageLedgerStore) execUsageRetentionDeleteBatch(ctx context.Context, query string, args []any, batchSize int) (int64, error) {
	if batchSize <= 0 {
		return 0, fmt.Errorf("%w: batch_size must be positive, got %d", store.ErrInvalidInput, batchSize)
	}
	run := func(client *ent.Client) (int64, error) {
		drv := client.Driver()
		rebound := usageRebindSQL(drv.Dialect(), query)
		var res sql.Result
		if err := drv.Exec(ctx, rebound, args, &res); err != nil {
			return 0, mapError(err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, mapError(err)
		}
		return n, nil
	}
	if s.inTx {
		return run(s.client)
	}
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return 0, mapError(err)
	}
	defer func() { _ = tx.Rollback() }()
	n, err := run(tx.Client())
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, mapError(err)
	}
	return n, nil
}

// SweepUsageEventsBatch implements store.UsageRetentionStore.SweepUsageEventsBatch.
// Deletes up to batchSize usage_events rows strictly older than cutoff
// (event_ts < cutoff). When exportCursor is non-nil (P8 analytics export),
// additionally restricts deletion to rows at or before the cursor so unexported
// rows are never dropped.
func (s *UsageLedgerStore) SweepUsageEventsBatch(ctx context.Context, cutoff time.Time, exportCursor *store.UsageExportCursor, batchSize int) (int64, error) {
	cutoffUTC := cutoff.UTC()
	var (
		query string
		args  []any
	)
	switch {
	case exportCursor != nil && exportCursor.EventID != "":
		cursorTS := exportCursor.ReceivedAt.UTC()
		query = `
DELETE FROM usage_events
WHERE id IN (
	SELECT id FROM usage_events
	WHERE event_ts < ?
	  AND (received_at < ? OR (received_at = ? AND event_id <= ?))
	ORDER BY event_ts ASC
	LIMIT ?
)`
		args = []any{cutoffUTC, cursorTS, cursorTS, exportCursor.EventID, batchSize}
	case exportCursor != nil:
		cursorTS := exportCursor.ReceivedAt.UTC()
		query = `
DELETE FROM usage_events
WHERE id IN (
	SELECT id FROM usage_events
	WHERE event_ts < ?
	  AND received_at <= ?
	ORDER BY event_ts ASC
	LIMIT ?
)`
		args = []any{cutoffUTC, cursorTS, batchSize}
	default:
		query = `
DELETE FROM usage_events
WHERE id IN (
	SELECT id FROM usage_events
	WHERE event_ts < ?
	ORDER BY event_ts ASC
	LIMIT ?
)`
		args = []any{cutoffUTC, batchSize}
	}
	return s.execUsageRetentionDeleteBatch(ctx, query, args, batchSize)
}

// SweepUsageRollupsHourlyBatch implements store.UsageRetentionStore.SweepUsageRollupsHourlyBatch.
func (s *UsageLedgerStore) SweepUsageRollupsHourlyBatch(ctx context.Context, cutoff time.Time, batchSize int) (int64, error) {
	const query = `
DELETE FROM usage_rollups_hourly
WHERE id IN (
	SELECT id FROM usage_rollups_hourly
	WHERE bucket_start < ?
	ORDER BY bucket_start ASC
	LIMIT ?
)`
	return s.execUsageRetentionDeleteBatch(ctx, query, []any{cutoff.UTC(), batchSize}, batchSize)
}

// SweepUsageRollupsDailyBatch implements store.UsageRetentionStore.SweepUsageRollupsDailyBatch.
func (s *UsageLedgerStore) SweepUsageRollupsDailyBatch(ctx context.Context, cutoff time.Time, batchSize int) (int64, error) {
	const query = `
DELETE FROM usage_rollups_daily
WHERE id IN (
	SELECT id FROM usage_rollups_daily
	WHERE bucket_start < ?
	ORDER BY bucket_start ASC
	LIMIT ?
)`
	return s.execUsageRetentionDeleteBatch(ctx, query, []any{cutoff.UTC(), batchSize}, batchSize)
}

// SweepUsageScopeCountersDailyBatch implements store.UsageRetentionStore.SweepUsageScopeCountersDailyBatch.
func (s *UsageLedgerStore) SweepUsageScopeCountersDailyBatch(ctx context.Context, cutoff time.Time, batchSize int) (int64, error) {
	const query = `
DELETE FROM usage_scope_counters_daily
WHERE id IN (
	SELECT id FROM usage_scope_counters_daily
	WHERE day_utc < ?
	ORDER BY day_utc ASC
	LIMIT ?
)`
	return s.execUsageRetentionDeleteBatch(ctx, query, []any{cutoff.UTC(), batchSize}, batchSize)
}

// SweepMeterHealthBatch implements store.UsageRetentionStore.SweepMeterHealthBatch.
// Per D8, sweeps closed (final_state != 'open') and abandoned (superseded run
// or deleted agent) meter_health rows with updated_at < cutoff while never
// touching the current run of a live (non-deleted) agent.
func (s *UsageLedgerStore) SweepMeterHealthBatch(ctx context.Context, cutoff time.Time, batchSize int) (int64, error) {
	const query = `
DELETE FROM meter_health
WHERE id IN (
	SELECT mh.id FROM meter_health mh
	WHERE mh.updated_at < ?
	  AND NOT EXISTS (
		SELECT 1 FROM agents a
		WHERE CAST(a.id AS TEXT) = mh.agent_id
		  AND a.deleted_at IS NULL
		  AND a.run_id = mh.run_id
	  )
	ORDER BY mh.updated_at ASC
	LIMIT ?
)`
	return s.execUsageRetentionDeleteBatch(ctx, query, []any{cutoff.UTC(), batchSize}, batchSize)
}

// SweepRunAttributionBatch implements store.UsageRetentionStore.SweepRunAttributionBatch.
// Per D8a, sweeps run_attribution rows with created_at < cutoff and
// legal_hold = false that are not the current run of a live (non-deleted) agent
// and have no remaining usage_events or meter_health rows referencing the run.
// P5 will add an additional NOT EXISTS guard for trajectory_segments.
func (s *UsageLedgerStore) SweepRunAttributionBatch(ctx context.Context, cutoff time.Time, batchSize int) (int64, error) {
	const query = `
DELETE FROM run_attribution
WHERE run_id IN (
	SELECT ra.run_id FROM run_attribution ra
	WHERE ra.created_at < ?
	  AND ra.legal_hold = ?
	  AND NOT EXISTS (
		SELECT 1 FROM agents a
		WHERE CAST(a.id AS TEXT) = ra.agent_id
		  AND a.deleted_at IS NULL
		  AND a.run_id = ra.run_id
	  )
	  AND NOT EXISTS (
		SELECT 1 FROM usage_events ue
		WHERE ue.run_id = ra.run_id
	  )
	  AND NOT EXISTS (
		SELECT 1 FROM meter_health mh
		WHERE mh.run_id = ra.run_id
	  )
	ORDER BY ra.created_at ASC
	LIMIT ?
)`
	return s.execUsageRetentionDeleteBatch(ctx, query, []any{cutoff.UTC(), false, batchSize}, batchSize)
}
