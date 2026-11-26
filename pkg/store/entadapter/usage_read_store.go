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

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/meterhealth"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/usageevent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/usagerollupdaily"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ListMeterHealthByAgent implements store.UsageLedgerStore.ListMeterHealthByAgent.
func (s *UsageLedgerStore) ListMeterHealthByAgent(ctx context.Context, agentID string) ([]*store.MeterHealth, error) {
	if agentID == "" {
		return nil, fmt.Errorf("%w: agent_id is required", store.ErrInvalidInput)
	}
	rows, err := s.client.MeterHealth.Query().
		Where(meterhealth.AgentIDEQ(agentID)).
		Order(ent.Desc(meterhealth.FieldLastReceiptAt), ent.Asc(meterhealth.FieldProducerEpoch)).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]*store.MeterHealth, len(rows))
	for i, r := range rows {
		out[i] = entMeterHealthToStore(r)
	}
	return out, nil
}

// ListUsageRollupsDailyByAgent implements store.UsageLedgerStore.ListUsageRollupsDailyByAgent.
func (s *UsageLedgerStore) ListUsageRollupsDailyByAgent(ctx context.Context, agentID string, sinceDay, untilDay time.Time) ([]*store.UsageRollupDaily, error) {
	if agentID == "" {
		return nil, fmt.Errorf("%w: agent_id is required", store.ErrInvalidInput)
	}
	q := s.client.UsageRollupDaily.Query().
		Where(usagerollupdaily.AgentIDEQ(agentID))
	if !sinceDay.IsZero() {
		q = q.Where(usagerollupdaily.BucketStartGTE(utcDayStart(sinceDay)))
	}
	if !untilDay.IsZero() {
		q = q.Where(usagerollupdaily.BucketStartLTE(utcDayStart(untilDay)))
	}
	rows, err := q.
		Order(
			ent.Asc(usagerollupdaily.FieldBucketStart),
			ent.Asc(usagerollupdaily.FieldHarness),
			ent.Asc(usagerollupdaily.FieldModel),
			ent.Asc(usagerollupdaily.FieldUsageSource),
		).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]*store.UsageRollupDaily, len(rows))
	for i, r := range rows {
		out[i] = entUsageRollupDailyToStore(r)
	}
	return out, nil
}

// GetUsageTokenPresenceByAgent implements store.UsageLedgerStore.GetUsageTokenPresenceByAgent.
func (s *UsageLedgerStore) GetUsageTokenPresenceByAgent(ctx context.Context, agentID string, since, until time.Time) ([]*store.UsageTokenPresence, error) {
	if agentID == "" {
		return nil, fmt.Errorf("%w: agent_id is required", store.ErrInvalidInput)
	}
	drv := s.client.Driver()
	dialectName := drv.Dialect()

	query := `
SELECT
	model,
	harness,
	usage_source,
	COUNT(*) AS event_count,
	COUNT(input) AS input_count,
	COUNT(output) AS output_count,
	COUNT(cache_read) AS cache_read_count,
	COUNT(cache_write) AS cache_write_count,
	COUNT(cache_write_1h) AS cache_write_1h_count,
	COUNT(reasoning) AS reasoning_count
FROM usage_events
WHERE agent_id = ?`
	args := []any{agentID}
	if !since.IsZero() {
		query += ` AND event_ts >= ?`
		args = append(args, since.UTC().Truncate(time.Microsecond))
	}
	if !until.IsZero() {
		query += ` AND event_ts <= ?`
		args = append(args, until.UTC().Truncate(time.Microsecond))
	}
	query += ` GROUP BY model, harness, usage_source ORDER BY model ASC, harness ASC, usage_source ASC`

	rows := &entsql.Rows{}
	if err := drv.Query(ctx, usageRebindSQL(dialectName, query), args, rows); err != nil {
		return nil, mapError(err)
	}
	defer func() { _ = rows.Close() }()

	var out []*store.UsageTokenPresence
	for rows.Next() {
		var p store.UsageTokenPresence
		if err := rows.Scan(
			&p.Model,
			&p.Harness,
			&p.UsageSource,
			&p.EventCount,
			&p.InputCount,
			&p.OutputCount,
			&p.CacheReadCount,
			&p.CacheWriteCount,
			&p.CacheWrite1hCount,
			&p.ReasoningCount,
		); err != nil {
			return nil, mapError(err)
		}
		out = append(out, &p)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err)
	}
	return out, nil
}

// HasUsageEventsForRun implements store.UsageLedgerStore.HasUsageEventsForRun.
func (s *UsageLedgerStore) HasUsageEventsForRun(ctx context.Context, agentID, runID string) (bool, error) {
	if agentID == "" || runID == "" {
		return false, nil
	}
	exists, err := s.client.UsageEvent.Query().
		Where(usageevent.AgentIDEQ(agentID), usageevent.RunIDEQ(runID)).
		Exist(ctx)
	if err != nil {
		return false, mapError(err)
	}
	return exists, nil
}

// DBNow implements store.UsageLedgerStore.DBNow, reading the current UTC
// timestamp directly from the database engine clock (§4.2 item 7).
func (s *UsageLedgerStore) DBNow(ctx context.Context) (time.Time, error) {
	drv := s.client.Driver()
	dialectName := drv.Dialect()

	rows := &entsql.Rows{}
	if dialectName == dialect.Postgres {
		if err := drv.Query(ctx, "SELECT CURRENT_TIMESTAMP", []any{}, rows); err != nil {
			return time.Time{}, mapError(err)
		}
		defer func() { _ = rows.Close() }()
		if !rows.Next() {
			if err := rows.Err(); err != nil {
				return time.Time{}, mapError(err)
			}
			return time.Time{}, fmt.Errorf("DBNow: empty result from Postgres CURRENT_TIMESTAMP")
		}
		var ts time.Time
		if err := rows.Scan(&ts); err != nil {
			return time.Time{}, mapError(err)
		}
		return ts.UTC().Truncate(time.Microsecond), nil
	}

	// SQLite: strftime('%Y-%m-%dT%H:%M:%fZ', 'now') gives millisecond-precision UTC.
	if err := drv.Query(ctx, "SELECT strftime('%Y-%m-%dT%H:%M:%fZ', 'now')", []any{}, rows); err != nil {
		return time.Time{}, mapError(err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return time.Time{}, mapError(err)
		}
		return time.Time{}, fmt.Errorf("DBNow: empty result from SQLite strftime")
	}
	var raw string
	if err := rows.Scan(&raw); err != nil {
		return time.Time{}, mapError(err)
	}
	ts, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("DBNow: parse SQLite timestamp %q: %w", raw, err)
	}
	return ts.UTC().Truncate(time.Microsecond), nil
}

// CountStaleMetersByProject implements store.UsageLedgerStore.CountStaleMetersByProject.
// Matches the read-time staleness rule in pkg/hub/handlers_usage_read.go: a run
// is stale when its latest meter_health row is still "open" and has
// last_receipt_at strictly before staleBefore.
func (s *UsageLedgerStore) CountStaleMetersByProject(ctx context.Context, staleBefore time.Time) (map[string]int64, error) {
	drv := s.client.Driver()
	dialectName := drv.Dialect()

	query := `
SELECT project_id, COUNT(*) AS stale_count
FROM (
	SELECT ra.project_id, mh.agent_id, mh.run_id
	FROM meter_health mh
	INNER JOIN run_attribution ra ON ra.run_id = mh.run_id
	WHERE mh.final_state = 'open'
	  AND NOT EXISTS (
		SELECT 1
		FROM meter_health mh2
		WHERE mh2.agent_id = mh.agent_id
		  AND mh2.run_id = mh.run_id
		  AND mh2.final_state <> 'open'
		  AND mh2.last_receipt_at >= mh.last_receipt_at
	  )
	GROUP BY ra.project_id, mh.agent_id, mh.run_id
	HAVING MAX(mh.last_receipt_at) < ?
) AS stale_runs
GROUP BY project_id
ORDER BY project_id ASC`

	cutoff := staleBefore.UTC().Truncate(time.Microsecond)
	rows := &entsql.Rows{}
	if err := drv.Query(ctx, usageRebindSQL(dialectName, query), []any{cutoff}, rows); err != nil {
		return nil, mapError(err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]int64)
	for rows.Next() {
		var projectID string
		var count int64
		if err := rows.Scan(&projectID, &count); err != nil {
			return nil, mapError(err)
		}
		if projectID != "" && count > 0 {
			out[projectID] = count
		}
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err)
	}
	return out, nil
}

func entUsageRollupDailyToStore(r *ent.UsageRollupDaily) *store.UsageRollupDaily {
	return &store.UsageRollupDaily{
		ID:                 r.ID.String(),
		BucketStart:        r.BucketStart.UTC(),
		ProjectID:          r.ProjectID,
		BillingUserID:      r.BillingUserID,
		AgentID:            r.AgentID,
		Harness:            r.Harness,
		Model:              r.Model,
		UsageSource:        r.UsageSource,
		Calls:              r.Calls,
		ErrorCalls:         r.ErrorCalls,
		Input:              r.Input,
		Output:             r.Output,
		CacheRead:          r.CacheRead,
		CacheWrite:         r.CacheWrite,
		CacheWrite1h:       r.CacheWrite1h,
		Reasoning:          r.Reasoning,
		ReportedCostMicros: r.ReportedCostMicros,
		Events:             r.Events,
		NullTokenEvents:    r.NullTokenEvents,
		FirstEventTS:       r.FirstEventTs.UTC(),
		LastEventTS:        r.LastEventTs.UTC(),
	}
}
