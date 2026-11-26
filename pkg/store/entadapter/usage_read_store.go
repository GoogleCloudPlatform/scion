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
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/hubsetting"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/meterhealth"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/usageevent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/usagerollupdaily"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/usagerolluphourly"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const (
	defaultUsageRollupQueryLimit = 100
	maxUsageRollupQueryLimit     = 500
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

// ListUsageRollupsHourlyByAgent implements store.UsageLedgerStore.ListUsageRollupsHourlyByAgent.
func (s *UsageLedgerStore) ListUsageRollupsHourlyByAgent(ctx context.Context, agentID string, sinceHour, untilHour time.Time) ([]*store.UsageRollupHourly, error) {
	if agentID == "" {
		return nil, fmt.Errorf("%w: agent_id is required", store.ErrInvalidInput)
	}
	q := s.client.UsageRollupHourly.Query().
		Where(usagerolluphourly.AgentIDEQ(agentID))
	if !sinceHour.IsZero() {
		q = q.Where(usagerolluphourly.BucketStartGTE(utcHourStart(sinceHour)))
	}
	if !untilHour.IsZero() {
		q = q.Where(usagerolluphourly.BucketStartLTE(utcHourStart(untilHour)))
	}
	rows, err := q.
		Order(
			ent.Asc(usagerolluphourly.FieldBucketStart),
			ent.Asc(usagerolluphourly.FieldHarness),
			ent.Asc(usagerolluphourly.FieldModel),
			ent.Asc(usagerolluphourly.FieldUsageSource),
		).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]*store.UsageRollupHourly, len(rows))
	for i, r := range rows {
		out[i] = entUsageRollupHourlyToStore(r)
	}
	return out, nil
}

// GetUsageHourlyCoverageStart implements store.UsageLedgerStore.GetUsageHourlyCoverageStart.
func (s *UsageLedgerStore) GetUsageHourlyCoverageStart(ctx context.Context) (time.Time, bool, error) {
	row, err := s.client.HubSetting.Query().
		Where(hubsetting.SectionEQ(store.UsageRollupHourlyCoverageSection)).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, mapError(err)
	}
	var marker hourlyCoverageMarker
	if err := json.Unmarshal(row.Value, &marker); err != nil {
		return time.Time{}, false, fmt.Errorf("parse hourly coverage marker: %w", err)
	}
	if marker.StartedAt == "" {
		return time.Time{}, false, nil
	}
	ts, err := time.Parse(time.RFC3339, marker.StartedAt)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("parse hourly coverage timestamp %q: %w", marker.StartedAt, err)
	}
	return utcHourStart(ts), true, nil
}

// QueryUsageRollups implements store.UsageLedgerStore.QueryUsageRollups.
// It pushes aggregation into SQL against usage_rollups_hourly or
// usage_rollups_daily using the composite indexes and bounds grouped results
// with LIMIT limit+1 so unbounded rollup rows are never loaded into Go memory.
func (s *UsageLedgerStore) QueryUsageRollups(ctx context.Context, q store.UsageRollupQuery) (*store.UsageRollupQueryResult, error) {
	var (
		tableName string
		bucketFn  func(time.Time) time.Time
	)
	switch q.Granularity {
	case store.UsageGranularityHour:
		tableName = "usage_rollups_hourly"
		bucketFn = utcHourStart
	case store.UsageGranularityDay, "":
		tableName = "usage_rollups_daily"
		bucketFn = utcDayStart
	default:
		return nil, fmt.Errorf("%w: unsupported granularity %q", store.ErrInvalidInput, q.Granularity)
	}

	limit := q.Limit
	if limit <= 0 {
		limit = defaultUsageRollupQueryLimit
	}
	if limit > maxUsageRollupQueryLimit {
		limit = maxUsageRollupQueryLimit
	}

	drv := s.client.Driver()
	dialectName := drv.Dialect()

	// Build WHERE clause leading with indexed scope columns.
	var whereParts []string
	var whereArgs []any
	if q.ProjectID != "" {
		whereParts = append(whereParts, "project_id = ?")
		whereArgs = append(whereArgs, q.ProjectID)
	}
	if q.BillingUserID != "" {
		whereParts = append(whereParts, "billing_user_id = ?")
		whereArgs = append(whereArgs, q.BillingUserID)
	}
	if q.AgentID != "" {
		whereParts = append(whereParts, "agent_id = ?")
		whereArgs = append(whereArgs, q.AgentID)
	}
	if !q.Since.IsZero() {
		whereParts = append(whereParts, "bucket_start >= ?")
		whereArgs = append(whereArgs, bucketFn(q.Since))
	}
	if !q.Until.IsZero() {
		whereParts = append(whereParts, "bucket_start <= ?")
		whereArgs = append(whereArgs, bucketFn(q.Until))
	}
	if q.Harness != "" {
		whereParts = append(whereParts, "harness = ?")
		whereArgs = append(whereArgs, q.Harness)
	}
	if q.Model != "" {
		whereParts = append(whereParts, "model = ?")
		whereArgs = append(whereArgs, q.Model)
	}
	if q.UsageSource != "" {
		whereParts = append(whereParts, "usage_source = ?")
		whereArgs = append(whereArgs, q.UsageSource)
	}

	whereSQL := ""
	if len(whereParts) > 0 {
		whereSQL = " WHERE " + strings.Join(whereParts, " AND ")
	}

	sumCols := `
	CAST(COALESCE(SUM(calls), 0) AS BIGINT),
	CAST(COALESCE(SUM(error_calls), 0) AS BIGINT),
	CAST(COALESCE(SUM(input), 0) AS BIGINT),
	CAST(COALESCE(SUM(output), 0) AS BIGINT),
	CAST(COALESCE(SUM(cache_read), 0) AS BIGINT),
	CAST(COALESCE(SUM(cache_write), 0) AS BIGINT),
	CAST(COALESCE(SUM(cache_write_1h), 0) AS BIGINT),
	CAST(COALESCE(SUM(reasoning), 0) AS BIGINT),
	CAST(COALESCE(SUM(reported_cost_micros), 0) AS BIGINT),
	CAST(COALESCE(SUM(events), 0) AS BIGINT),
	CAST(COALESCE(SUM(null_token_events), 0) AS BIGINT),
	CAST(COALESCE(SUM(input_known_events), 0) AS BIGINT),
	CAST(COALESCE(SUM(output_known_events), 0) AS BIGINT),
	CAST(COALESCE(SUM(cache_read_known_events), 0) AS BIGINT),
	CAST(COALESCE(SUM(cache_write_known_events), 0) AS BIGINT),
	CAST(COALESCE(SUM(cache_write_1h_known_events), 0) AS BIGINT),
	CAST(COALESCE(SUM(reasoning_known_events), 0) AS BIGINT),
	CAST(COALESCE(SUM(presence_tracked_events), 0) AS BIGINT)`

	// 1. Overall window totals (exact even when groups are truncated).
	totalsQuery := usageRebindSQL(dialectName, "SELECT "+sumCols+" FROM "+tableName+whereSQL)
	totalsRows := &entsql.Rows{}
	if err := drv.Query(ctx, totalsQuery, whereArgs, totalsRows); err != nil {
		return nil, mapError(err)
	}
	var totals store.UsageRollupTotals
	if totalsRows.Next() {
		if err := scanUsageRollupTotals(totalsRows, &totals); err != nil {
			_ = totalsRows.Close()
			return nil, mapError(err)
		}
	}
	if err := totalsRows.Err(); err != nil {
		_ = totalsRows.Close()
		return nil, mapError(err)
	}
	_ = totalsRows.Close()

	res := &store.UsageRollupQueryResult{
		Totals: totals,
	}
	if len(q.GroupBy) == 0 {
		return res, nil
	}

	// 2. Bounded grouped query.
	type dimSpec struct {
		name     string
		selectEx string
		groupEx  string
		isTime   bool
	}
	seenDims := make(map[string]bool, len(q.GroupBy))
	dims := make([]dimSpec, 0, len(q.GroupBy))
	hasDay, hasHour := false, false
	for _, raw := range q.GroupBy {
		d := strings.TrimSpace(raw)
		if d == "" || seenDims[d] {
			continue
		}
		seenDims[d] = true
		switch d {
		case store.UsageGroupByHour:
			if tableName != "usage_rollups_hourly" {
				return nil, fmt.Errorf("%w: groupBy=hour requires hourly granularity", store.ErrInvalidInput)
			}
			hasHour = true
			dims = append(dims, dimSpec{name: d, selectEx: "bucket_start", groupEx: "bucket_start", isTime: true})
		case store.UsageGroupByDay:
			hasDay = true
			if tableName == "usage_rollups_daily" {
				dims = append(dims, dimSpec{name: d, selectEx: "bucket_start", groupEx: "bucket_start", isTime: true})
			} else if dialectName == dialect.Postgres {
				expr := "date_trunc('day', bucket_start AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'"
				dims = append(dims, dimSpec{name: d, selectEx: expr, groupEx: expr, isTime: true})
			} else {
				expr := "substr(bucket_start, 1, 10)"
				dims = append(dims, dimSpec{name: d, selectEx: expr, groupEx: expr, isTime: true})
			}
		case store.UsageGroupByProject:
			dims = append(dims, dimSpec{name: d, selectEx: "project_id", groupEx: "project_id"})
		case store.UsageGroupByUser:
			dims = append(dims, dimSpec{name: d, selectEx: "billing_user_id", groupEx: "billing_user_id"})
		case store.UsageGroupByAgent:
			dims = append(dims, dimSpec{name: d, selectEx: "agent_id", groupEx: "agent_id"})
		case store.UsageGroupByHarness:
			dims = append(dims, dimSpec{name: d, selectEx: "harness", groupEx: "harness"})
		case store.UsageGroupByModel:
			dims = append(dims, dimSpec{name: d, selectEx: "model", groupEx: "model"})
		default:
			return nil, fmt.Errorf("%w: unsupported groupBy dimension %q", store.ErrInvalidInput, d)
		}
	}
	if hasDay && hasHour {
		return nil, fmt.Errorf("%w: groupBy cannot include both day and hour", store.ErrInvalidInput)
	}
	if len(dims) == 0 {
		return res, nil
	}

	selectParts := make([]string, 0, len(dims))
	groupParts := make([]string, 0, len(dims))
	var timeOrderExpr string
	nonTimeOrders := make([]string, 0, len(dims))
	for _, d := range dims {
		selectParts = append(selectParts, d.selectEx)
		groupParts = append(groupParts, d.groupEx)
		if d.isTime {
			timeOrderExpr = d.groupEx + " ASC"
		} else {
			nonTimeOrders = append(nonTimeOrders, d.groupEx+" ASC")
		}
	}

	orderParts := make([]string, 0, len(dims)+2)
	if timeOrderExpr != "" {
		orderParts = append(orderParts, timeOrderExpr)
	}
	orderParts = append(orderParts,
		"CAST(COALESCE(SUM(input), 0) + COALESCE(SUM(output), 0) + COALESCE(SUM(cache_read), 0) + COALESCE(SUM(cache_write), 0) AS BIGINT) DESC",
		"CAST(COALESCE(SUM(calls), 0) AS BIGINT) DESC",
	)
	orderParts = append(orderParts, nonTimeOrders...)

	groupQuery := fmt.Sprintf(
		"SELECT %s, %s FROM %s%s GROUP BY %s ORDER BY %s LIMIT ?",
		strings.Join(selectParts, ", "),
		sumCols,
		tableName,
		whereSQL,
		strings.Join(groupParts, ", "),
		strings.Join(orderParts, ", "),
	)
	groupArgs := append(append([]any(nil), whereArgs...), limit+1)

	gRows := &entsql.Rows{}
	if err := drv.Query(ctx, usageRebindSQL(dialectName, groupQuery), groupArgs, gRows); err != nil {
		return nil, mapError(err)
	}
	defer func() { _ = gRows.Close() }()

	groups := make([]*store.UsageRollupGroup, 0, limit)
	for gRows.Next() {
		var grp store.UsageRollupGroup
		var rawTime any
		dests := make([]any, 0, len(dims)+18)
		for _, d := range dims {
			switch d.name {
			case store.UsageGroupByHour, store.UsageGroupByDay:
				dests = append(dests, &rawTime)
			case store.UsageGroupByProject:
				dests = append(dests, &grp.ProjectID)
			case store.UsageGroupByUser:
				dests = append(dests, &grp.BillingUserID)
			case store.UsageGroupByAgent:
				dests = append(dests, &grp.AgentID)
			case store.UsageGroupByHarness:
				dests = append(dests, &grp.Harness)
			case store.UsageGroupByModel:
				dests = append(dests, &grp.Model)
			}
		}
		dests = append(dests,
			&grp.Calls,
			&grp.ErrorCalls,
			&grp.Input,
			&grp.Output,
			&grp.CacheRead,
			&grp.CacheWrite,
			&grp.CacheWrite1h,
			&grp.Reasoning,
			&grp.ReportedCostMicros,
			&grp.Events,
			&grp.NullTokenEvents,
			&grp.InputKnownEvents,
			&grp.OutputKnownEvents,
			&grp.CacheReadKnownEvents,
			&grp.CacheWriteKnownEvents,
			&grp.CacheWrite1hKnownEvents,
			&grp.ReasoningKnownEvents,
			&grp.PresenceTrackedEvents,
		)
		if err := gRows.Scan(dests...); err != nil {
			return nil, mapError(err)
		}
		if hasDay || hasHour {
			ts, err := parseSQLTimestamp(rawTime)
			if err != nil {
				return nil, fmt.Errorf("parse group bucket_start: %w", err)
			}
			if ts != nil {
				if hasDay {
					d := utcDayStart(*ts)
					grp.BucketStart = &d
				} else {
					h := utcHourStart(*ts)
					grp.BucketStart = &h
				}
			}
		}
		if len(groups) == limit {
			res.Truncated = true
			break
		}
		groups = append(groups, &grp)
	}
	if err := gRows.Err(); err != nil {
		return nil, mapError(err)
	}
	res.Groups = groups
	return res, nil
}

// GetUsageScopeCoverage implements store.UsageLedgerStore.GetUsageScopeCoverage.
// Executes a single aggregate SQL query over non-deleted agents in scope,
// joining meter_health and probing usage_events via EXISTS so global and
// project views never perform per-agent round trips.
func (s *UsageLedgerStore) GetUsageScopeCoverage(ctx context.Context, filter store.UsageCoverageFilter) (*store.UsageScopeCoverage, error) {
	drv := s.client.Driver()
	dialectName := drv.Dialect()

	dbNow := filter.DBNow
	if dbNow.IsZero() {
		var err error
		dbNow, err = s.DBNow(ctx)
		if err != nil {
			return nil, err
		}
	}
	staleCutoff := dbNow.UTC().Add(-filter.StaleAfter).Truncate(time.Microsecond)

	var scopeWhere []string
	var args []any
	scopeWhere = append(scopeWhere, "a.deleted_at IS NULL")

	if filter.ProjectID != "" {
		projUUID, err := uuid.Parse(filter.ProjectID)
		if err != nil {
			return &store.UsageScopeCoverage{}, nil
		}
		scopeWhere = append(scopeWhere, "a.project_id = ?")
		args = append(args, projUUID)
	}
	if filter.BillingUserID != "" {
		ownerUUID, parseErr := uuid.Parse(filter.BillingUserID)
		if parseErr == nil {
			scopeWhere = append(scopeWhere,
				"(EXISTS (SELECT 1 FROM run_attribution ra WHERE ra.run_id = a.run_id AND ra.billing_user_id = ?) OR (a.run_id = '' AND a.owner_id = ?))")
			args = append(args, filter.BillingUserID, ownerUUID)
		} else {
			scopeWhere = append(scopeWhere,
				"EXISTS (SELECT 1 FROM run_attribution ra WHERE ra.run_id = a.run_id AND ra.billing_user_id = ?)")
			args = append(args, filter.BillingUserID)
		}
	}

	// Bind staleCutoff twice for the two CASE branches.
	args = append(args, staleCutoff, staleCutoff)

	query := fmt.Sprintf(`
WITH scoped_agents AS (
	SELECT
		CAST(a.id AS TEXT) AS agent_id_text,
		COALESCE(a.run_id, '') AS run_id
	FROM agents a
	WHERE %s
),
agent_mh AS (
	SELECT
		sa.agent_id_text,
		sa.run_id,
		MAX(mh.last_receipt_at) AS latest_receipt_at,
		MAX(CASE
			WHEN mh.final_state = 'open' AND mh.last_receipt_at = (
				SELECT MAX(mh2.last_receipt_at)
				FROM meter_health mh2
				WHERE mh2.agent_id = sa.agent_id_text AND mh2.run_id = sa.run_id
			) THEN 1
			ELSE 0
		END) AS latest_is_open
	FROM scoped_agents sa
	LEFT JOIN meter_health mh
		ON mh.agent_id = sa.agent_id_text AND mh.run_id = sa.run_id AND sa.run_id <> ''
	GROUP BY sa.agent_id_text, sa.run_id
)
SELECT
	CAST(COUNT(*) AS BIGINT) AS total_agents,
	CAST(COALESCE(SUM(CASE WHEN am.run_id <> '' AND am.latest_is_open = 1 AND am.latest_receipt_at < ? THEN 1 ELSE 0 END), 0) AS BIGINT) AS stale_agents,
	CAST(COALESCE(SUM(CASE
		WHEN am.run_id <> '' AND am.latest_is_open = 1 AND am.latest_receipt_at < ? THEN 0
		WHEN am.run_id <> '' AND EXISTS (
			SELECT 1 FROM usage_events ue WHERE ue.agent_id = am.agent_id_text AND ue.run_id = am.run_id
		) THEN 1
		ELSE 0
	END), 0) AS BIGINT) AS measured_agents,
	MAX(am.latest_receipt_at) AS max_last_receipt_at
FROM agent_mh am`, strings.Join(scopeWhere, " AND "))

	rows := &entsql.Rows{}
	if err := drv.Query(ctx, usageRebindSQL(dialectName, query), args, rows); err != nil {
		return nil, mapError(err)
	}
	defer func() { _ = rows.Close() }()

	out := &store.UsageScopeCoverage{}
	if rows.Next() {
		var (
			totalAgents    int64
			staleAgents    int64
			measuredAgents int64
			rawLastReceipt any
		)
		if err := rows.Scan(&totalAgents, &staleAgents, &measuredAgents, &rawLastReceipt); err != nil {
			return nil, mapError(err)
		}
		out.TotalAgents = int(totalAgents)
		out.StaleAgents = int(staleAgents)
		out.MeasuredAgents = int(measuredAgents)
		unmeasured := totalAgents - staleAgents - measuredAgents
		if unmeasured < 0 {
			unmeasured = 0
		}
		out.UnmeasuredAgents = int(unmeasured)
		if rawLastReceipt != nil {
			ts, err := parseSQLTimestamp(rawLastReceipt)
			if err != nil {
				return nil, fmt.Errorf("parse max_last_receipt_at: %w", err)
			}
			out.LastReceiptAt = ts
		}
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
// Matches the read-time staleness rule in pkg/hub/handlers_usage_read.go: scoped
// to each live (non-deleted, running) agent's current run_id, a run is stale
// when its latest meter_health row is still "open" and has last_receipt_at
// strictly before staleBefore.
func (s *UsageLedgerStore) CountStaleMetersByProject(ctx context.Context, staleBefore time.Time) (map[string]int64, error) {
	drv := s.client.Driver()
	dialectName := drv.Dialect()

	query := `
SELECT project_id, COUNT(*) AS stale_count
FROM (
	SELECT ra.project_id, mh.agent_id, mh.run_id
	FROM meter_health mh
	INNER JOIN agents a ON CAST(a.id AS TEXT) = mh.agent_id AND a.run_id = mh.run_id AND a.deleted_at IS NULL AND a.phase = 'running'
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

func scanUsageRollupTotals(rows *entsql.Rows, dst *store.UsageRollupTotals) error {
	return rows.Scan(
		&dst.Calls,
		&dst.ErrorCalls,
		&dst.Input,
		&dst.Output,
		&dst.CacheRead,
		&dst.CacheWrite,
		&dst.CacheWrite1h,
		&dst.Reasoning,
		&dst.ReportedCostMicros,
		&dst.Events,
		&dst.NullTokenEvents,
		&dst.InputKnownEvents,
		&dst.OutputKnownEvents,
		&dst.CacheReadKnownEvents,
		&dst.CacheWriteKnownEvents,
		&dst.CacheWrite1hKnownEvents,
		&dst.ReasoningKnownEvents,
		&dst.PresenceTrackedEvents,
	)
}

func parseSQLTimestamp(v any) (*time.Time, error) {
	if v == nil {
		return nil, nil
	}
	switch t := v.(type) {
	case time.Time:
		u := t.UTC().Truncate(time.Microsecond)
		return &u, nil
	case []byte:
		return parseSQLTimestampString(string(t))
	case string:
		return parseSQLTimestampString(t)
	default:
		return nil, fmt.Errorf("unexpected timestamp type %T", v)
	}
}

func parseSQLTimestampString(raw string) (*time.Time, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, nil
	}
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999 -0700 MST",
		"2006-01-02 15:04:05 -0700 MST",
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05.999999999+00:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05-07:00",
		"2006-01-02 15:04:05",
		"2006-01-02",
	}
	for _, layout := range layouts {
		if parsed, err := time.Parse(layout, s); err == nil {
			u := parsed.UTC().Truncate(time.Microsecond)
			return &u, nil
		}
	}
	return nil, errors.New("unrecognized SQL timestamp: " + s)
}

func entUsageRollupDailyToStore(r *ent.UsageRollupDaily) *store.UsageRollupDaily {
	return &store.UsageRollupDaily{
		ID:                      r.ID.String(),
		BucketStart:             r.BucketStart.UTC(),
		ProjectID:               r.ProjectID,
		BillingUserID:           r.BillingUserID,
		AgentID:                 r.AgentID,
		Harness:                 r.Harness,
		Model:                   r.Model,
		UsageSource:             r.UsageSource,
		Calls:                   r.Calls,
		ErrorCalls:              r.ErrorCalls,
		Input:                   r.Input,
		Output:                  r.Output,
		CacheRead:               r.CacheRead,
		CacheWrite:              r.CacheWrite,
		CacheWrite1h:            r.CacheWrite1h,
		Reasoning:               r.Reasoning,
		ReportedCostMicros:      r.ReportedCostMicros,
		Events:                  r.Events,
		NullTokenEvents:         r.NullTokenEvents,
		InputKnownEvents:        r.InputKnownEvents,
		OutputKnownEvents:       r.OutputKnownEvents,
		CacheReadKnownEvents:    r.CacheReadKnownEvents,
		CacheWriteKnownEvents:   r.CacheWriteKnownEvents,
		CacheWrite1hKnownEvents: r.CacheWrite1hKnownEvents,
		ReasoningKnownEvents:    r.ReasoningKnownEvents,
		PresenceTrackedEvents:   r.PresenceTrackedEvents,
		FirstEventTS:            r.FirstEventTs.UTC(),
		LastEventTS:             r.LastEventTs.UTC(),
	}
}

func entUsageRollupHourlyToStore(r *ent.UsageRollupHourly) *store.UsageRollupHourly {
	return &store.UsageRollupHourly{
		ID:                      r.ID.String(),
		BucketStart:             r.BucketStart.UTC(),
		ProjectID:               r.ProjectID,
		BillingUserID:           r.BillingUserID,
		AgentID:                 r.AgentID,
		Harness:                 r.Harness,
		Model:                   r.Model,
		UsageSource:             r.UsageSource,
		Calls:                   r.Calls,
		ErrorCalls:              r.ErrorCalls,
		Input:                   r.Input,
		Output:                  r.Output,
		CacheRead:               r.CacheRead,
		CacheWrite:              r.CacheWrite,
		CacheWrite1h:            r.CacheWrite1h,
		Reasoning:               r.Reasoning,
		ReportedCostMicros:      r.ReportedCostMicros,
		Events:                  r.Events,
		NullTokenEvents:         r.NullTokenEvents,
		InputKnownEvents:        r.InputKnownEvents,
		OutputKnownEvents:       r.OutputKnownEvents,
		CacheReadKnownEvents:    r.CacheReadKnownEvents,
		CacheWriteKnownEvents:   r.CacheWriteKnownEvents,
		CacheWrite1hKnownEvents: r.CacheWrite1hKnownEvents,
		ReasoningKnownEvents:    r.ReasoningKnownEvents,
		PresenceTrackedEvents:   r.PresenceTrackedEvents,
		FirstEventTS:            r.FirstEventTs.UTC(),
		LastEventTS:             r.LastEventTs.UTC(),
	}
}
