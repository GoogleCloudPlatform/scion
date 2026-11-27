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
	"math"
	"sort"
	"strings"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/hubsetting"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/meterhealth"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/usageevent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/usagerollupdaily"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const (
	defaultUsageRollupQueryLimit = 100
	maxUsageRollupQueryLimit     = 500
	sqliteSplitRadix             = int64(1_000_000_000)
)

var rollupSumColumns = []string{
	"calls",
	"error_calls",
	"input",
	"output",
	"cache_read",
	"cache_write",
	"cache_write_1h",
	"reasoning",
	"reported_cost_micros",
	"events",
	"null_token_events",
	"input_known_events",
	"output_known_events",
	"cache_read_known_events",
	"cache_write_known_events",
	"cache_write_1h_known_events",
	"reasoning_known_events",
	"presence_tracked_events",
}

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

// CheckUsageHourlyConsistency implements store.UsageLedgerStore.CheckUsageHourlyConsistency.
// It compares SUM(events) between usage_rollups_hourly and usage_rollups_daily
// over the inclusive whole UTC days [utcDayStart(q.Since), utcDayStart(q.Until)]
// using the same scope/dimension filters, in a single snapshot-consistent SQL
// statement (two indexed aggregate subqueries).
func (s *UsageLedgerStore) CheckUsageHourlyConsistency(ctx context.Context, q store.UsageRollupQuery) (bool, error) {
	if q.Since.IsZero() || q.Until.IsZero() {
		return true, nil
	}
	sinceDay := utcDayStart(q.Since)
	untilDay := utcDayStart(q.Until)
	if sinceDay.After(untilDay) {
		return true, nil
	}

	drv := s.client.Driver()
	dialectName := drv.Dialect()

	buildWhere := func(sinceBound, untilBound time.Time) (string, []any) {
		var parts []string
		var args []any
		if q.ProjectID != "" {
			parts = append(parts, "project_id = ?")
			args = append(args, q.ProjectID)
		}
		if q.BillingUserID != "" {
			parts = append(parts, "billing_user_id = ?")
			args = append(args, q.BillingUserID)
		}
		if q.AgentID != "" {
			parts = append(parts, "agent_id = ?")
			args = append(args, q.AgentID)
		}
		parts = append(parts, "bucket_start >= ?", "bucket_start <= ?")
		args = append(args, sinceBound, untilBound)
		if q.Harness != "" {
			parts = append(parts, "harness = ?")
			args = append(args, q.Harness)
		}
		if q.Model != "" {
			parts = append(parts, "model = ?")
			args = append(args, q.Model)
		}
		if q.UsageSource != "" {
			parts = append(parts, "usage_source = ?")
			args = append(args, q.UsageSource)
		}
		return strings.Join(parts, " AND "), args
	}

	hourlyWhere, hourlyArgs := buildWhere(sinceDay, untilDay.Add(23*time.Hour))
	dailyWhere, dailyArgs := buildWhere(sinceDay, untilDay)

	query := fmt.Sprintf(
		"SELECT CASE WHEN (SELECT COALESCE(SUM(events), 0) FROM usage_rollups_hourly WHERE %s) = (SELECT COALESCE(SUM(events), 0) FROM usage_rollups_daily WHERE %s) THEN 1 ELSE 0 END",
		hourlyWhere,
		dailyWhere,
	)
	args := append(hourlyArgs, dailyArgs...)

	rows := &entsql.Rows{}
	if err := drv.Query(ctx, usageRebindSQL(dialectName, query), args, rows); err != nil {
		return false, mapError(err)
	}
	defer func() { _ = rows.Close() }()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return false, mapError(err)
		}
		return false, nil
	}
	var match int
	if err := rows.Scan(&match); err != nil {
		return false, mapError(err)
	}
	return match == 1, nil
}

// QueryUsageRollups implements store.UsageLedgerStore.QueryUsageRollups.
// It pushes overflow-safe saturating aggregation into SQL against
// usage_rollups_hourly or usage_rollups_daily using the composite indexes and
// bounds grouped results with LIMIT limit+1 so unbounded rollup rows are never
// loaded into Go memory. Time-series queries order newest-first in SQL so
// truncation preserves the most recent buckets, then return ascending order.
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

	sumCols := buildRollupSumSelectSQL(dialectName)

	// 1. Overall window totals (exact even when groups are truncated; saturated on int64 overflow).
	totalsQuery := usageRebindSQL(dialectName, "SELECT "+sumCols+" FROM "+tableName+whereSQL)
	totalsRows := &entsql.Rows{}
	if err := drv.Query(ctx, totalsQuery, whereArgs, totalsRows); err != nil {
		return nil, mapError(err)
	}
	var totals store.UsageRollupTotals
	if totalsRows.Next() {
		if err := scanUsageRollupTotalsRow(dialectName, totalsRows, nil, &totals); err != nil {
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
			// O3: Order newest-first in SQL so LIMIT keeps the most recent buckets
			// on truncation; re-sorted ascending in Go below.
			timeOrderExpr = d.groupEx + " DESC"
		} else {
			nonTimeOrders = append(nonTimeOrders, d.groupEx+" ASC")
		}
	}

	orderParts := make([]string, 0, len(dims)+2)
	if timeOrderExpr != "" {
		orderParts = append(orderParts, timeOrderExpr)
	}
	if dialectName == dialect.Postgres {
		// Postgres SUM(bigint) produces arbitrary-precision numeric (never overflows).
		orderParts = append(orderParts,
			"(COALESCE(SUM(input), 0) + COALESCE(SUM(output), 0) + COALESCE(SUM(cache_read), 0) + COALESCE(SUM(cache_write), 0)) DESC",
			"COALESCE(SUM(calls), 0) DESC",
		)
	} else {
		// SQLite TOTAL() produces float64 (never raises integer overflow).
		orderParts = append(orderParts,
			"(TOTAL(input) + TOTAL(output) + TOTAL(cache_read) + TOTAL(cache_write)) DESC",
			"TOTAL(calls) DESC",
		)
	}
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
		prefixDests := make([]any, 0, len(dims))
		for _, d := range dims {
			switch d.name {
			case store.UsageGroupByHour, store.UsageGroupByDay:
				prefixDests = append(prefixDests, &rawTime)
			case store.UsageGroupByProject:
				prefixDests = append(prefixDests, &grp.ProjectID)
			case store.UsageGroupByUser:
				prefixDests = append(prefixDests, &grp.BillingUserID)
			case store.UsageGroupByAgent:
				prefixDests = append(prefixDests, &grp.AgentID)
			case store.UsageGroupByHarness:
				prefixDests = append(prefixDests, &grp.Harness)
			case store.UsageGroupByModel:
				prefixDests = append(prefixDests, &grp.Model)
			}
		}
		if err := scanUsageRollupTotalsRow(dialectName, gRows, prefixDests, &grp.UsageRollupTotals); err != nil {
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

	// Present time-series groups in ascending chronological order while
	// preserving the most recent buckets retained by LIMIT.
	if hasDay || hasHour {
		sort.SliceStable(groups, func(i, j int) bool {
			ti, tj := groups[i].BucketStart, groups[j].BucketStart
			if ti == nil && tj == nil {
				return false
			}
			if ti == nil {
				return true
			}
			if tj == nil {
				return false
			}
			if !ti.Equal(*tj) {
				return ti.Before(*tj)
			}
			return false
		})
	}

	res.Groups = groups
	return res, nil
}

// GetUsageScopeCoverage implements store.UsageLedgerStore.GetUsageScopeCoverage.
// Executes a single bounded aggregate SQL query over non-deleted agents
// overlapping [Since, Until] (with at least one run_attribution created at or
// before Until), returning counts grouped by the derivation's input flags
// (at most ~12 rows regardless of fleet size) so the Hub derives status
// through its single deriveMeasurementStatus function in Go.
func (s *UsageLedgerStore) GetUsageScopeCoverage(ctx context.Context, filter store.UsageCoverageFilter) (*store.UsageScopeCoverage, error) {
	drv := s.client.Driver()
	dialectName := drv.Dialect()

	dbNow := filter.DBNow
	if dbNow.IsZero() {
		var err error
		dbNow, err = dbNowFromDriver(ctx, drv, dialectName)
		if err != nil {
			return nil, err
		}
	}
	until := filter.Until
	if until.IsZero() {
		until = dbNow
	}
	until = until.UTC().Truncate(time.Microsecond)
	since := filter.Since
	if since.IsZero() {
		since = utcDayStart(until).AddDate(0, 0, -29)
	}
	since = since.UTC().Truncate(time.Microsecond)
	sinceDay := utcDayStart(since)
	untilDay := utcDayStart(until)
	staleCutoff := dbNow.UTC().Add(-filter.StaleAfter).Truncate(time.Microsecond)
	windowIncludesNow := !until.Before(staleCutoff)
	runningPhase := string(state.PhaseRunning)

	var scopeWhere []string
	var args []any
	scopeWhere = append(scopeWhere, "a.deleted_at IS NULL")

	// When scoped to a billing user, drive scoped_agents from
	// run_attribution(billing_user_id, created_at) and join agents by PK so
	// user-scoped coverage avoids scanning the whole agents table.
	var scopedAgentsCTE string
	if filter.BillingUserID != "" {
		if dialectName == dialect.Postgres {
			scopedAgentsCTE = `user_agents AS (
	SELECT DISTINCT ra.agent_id
	FROM run_attribution ra
	WHERE ra.billing_user_id = ? AND ra.created_at <= ?
		AND ra.agent_id ~ '^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$'
),
scoped_agents AS (
	SELECT
		CAST(a.id AS TEXT) AS agent_id_text,
		COALESCE(a.run_id, '') AS run_id,
		COALESCE(a.phase, '') AS phase
	FROM user_agents ua
	JOIN agents a ON a.id = CAST(ua.agent_id AS UUID)
	WHERE %s
)`
		} else {
			scopedAgentsCTE = `user_agents AS (
	SELECT DISTINCT ra.agent_id
	FROM run_attribution ra
	WHERE ra.billing_user_id = ? AND ra.created_at <= ?
),
scoped_agents AS (
	SELECT
		CAST(a.id AS TEXT) AS agent_id_text,
		COALESCE(a.run_id, '') AS run_id,
		COALESCE(a.phase, '') AS phase
	FROM user_agents ua
	JOIN agents a ON a.id = ua.agent_id
	WHERE %s
)`
		}
		args = append(args, filter.BillingUserID, until)
	} else {
		scopedAgentsCTE = `scoped_agents AS (
	SELECT
		CAST(a.id AS TEXT) AS agent_id_text,
		COALESCE(a.run_id, '') AS run_id,
		COALESCE(a.phase, '') AS phase
	FROM agents a
	WHERE %s
)`
	}

	if filter.ProjectID != "" {
		projUUID, err := uuid.Parse(filter.ProjectID)
		if err != nil {
			return &store.UsageScopeCoverage{}, nil
		}
		scopeWhere = append(scopeWhere, "a.project_id = ?")
		args = append(args, projUUID)
	}
	if filter.AgentID != "" {
		agentUUID, err := uuid.Parse(filter.AgentID)
		if err != nil {
			return &store.UsageScopeCoverage{}, nil
		}
		scopeWhere = append(scopeWhere, "a.id = ?")
		args = append(args, agentUUID)
	}

	// Window relevance: agent must have a run_attribution created <= until
	// (matching billing_user_id when scoped to a user) AND overlap [since, until]
	// via run creation, receipt timestamp, daily rollup, or (only when the window
	// includes now, i.e. until >= dbNow - staleAfter) active running phase.
	if filter.BillingUserID == "" {
		scopeWhere = append(scopeWhere, "EXISTS (SELECT 1 FROM run_attribution ra WHERE ra.agent_id = CAST(a.id AS TEXT) AND ra.created_at <= ?)")
		args = append(args, until)
	}

	ra2Cond := "ra2.agent_id = CAST(a.id AS TEXT) AND ra2.created_at >= ? AND ra2.created_at <= ?"
	rdwCond := "rd_w.agent_id = CAST(a.id AS TEXT) AND rd_w.bucket_start >= ? AND rd_w.bucket_start <= ?"
	var ra2Args []any
	ra2Args = append(ra2Args, since, until)
	var rdwArgs []any
	rdwArgs = append(rdwArgs, sinceDay, untilDay)
	if filter.BillingUserID != "" {
		ra2Cond += " AND ra2.billing_user_id = ?"
		ra2Args = append(ra2Args, filter.BillingUserID)
		rdwCond += " AND rd_w.billing_user_id = ?"
		rdwArgs = append(rdwArgs, filter.BillingUserID)
	}

	if windowIncludesNow {
		overlapCond := fmt.Sprintf(`(
			EXISTS (SELECT 1 FROM run_attribution ra2 WHERE %s)
			OR a.phase = ?
			OR EXISTS (SELECT 1 FROM meter_health mh_w WHERE mh_w.agent_id = CAST(a.id AS TEXT) AND mh_w.last_receipt_at >= ? AND mh_w.last_receipt_at <= ?)
			OR EXISTS (SELECT 1 FROM usage_rollups_daily rd_w WHERE %s)
		)`, ra2Cond, rdwCond)
		scopeWhere = append(scopeWhere, overlapCond)
		args = append(args, ra2Args...)
		args = append(args, runningPhase, since, until)
		args = append(args, rdwArgs...)
	} else {
		overlapCond := fmt.Sprintf(`(
			EXISTS (SELECT 1 FROM run_attribution ra2 WHERE %s)
			OR EXISTS (SELECT 1 FROM meter_health mh_w WHERE mh_w.agent_id = CAST(a.id AS TEXT) AND mh_w.last_receipt_at >= ? AND mh_w.last_receipt_at <= ?)
			OR EXISTS (SELECT 1 FROM usage_rollups_daily rd_w WHERE %s)
		)`, ra2Cond, rdwCond)
		scopeWhere = append(scopeWhere, overlapCond)
		args = append(args, ra2Args...)
		args = append(args, since, until)
		args = append(args, rdwArgs...)
	}

	// Parameters for agent_flags projection:
	// Running-phase and stale classification apply only when the window includes
	// now (until >= dbNow - staleAfter), so historical windows are not marked
	// stale by a currently running agent's open meter.
	isRunningExpr := "0"
	isStaleExpr := "0"
	if windowIncludesNow {
		isRunningExpr = "CASE WHEN am.phase = ? THEN 1 ELSE 0 END"
		isStaleExpr = "CASE WHEN am.latest_receipt_at IS NOT NULL AND am.latest_receipt_at < ? THEN 1 ELSE 0 END"
		args = append(args, runningPhase, staleCutoff)
	}
	rdFlagCond := "rd.agent_id = am.agent_id_text AND rd.bucket_start >= ? AND rd.bucket_start <= ?"
	args = append(args, sinceDay, untilDay)
	if filter.BillingUserID != "" {
		rdFlagCond += " AND rd.billing_user_id = ?"
		args = append(args, filter.BillingUserID)
	}

	query := fmt.Sprintf(`
WITH %s,
agent_mh AS (
	SELECT
		sa.agent_id_text,
		sa.run_id,
		sa.phase,
		MAX(mh.last_receipt_at) AS latest_receipt_at,
		COALESCE(MAX(CASE
			WHEN mh.last_receipt_at = (
				SELECT MAX(mh2.last_receipt_at)
				FROM meter_health mh2
				WHERE mh2.agent_id = sa.agent_id_text AND mh2.run_id = sa.run_id
			) THEN mh.final_state
			ELSE ''
		END), '') AS latest_final_state
	FROM scoped_agents sa
	LEFT JOIN meter_health mh
		ON mh.agent_id = sa.agent_id_text AND mh.run_id = sa.run_id AND sa.run_id <> ''
	GROUP BY sa.agent_id_text, sa.run_id, sa.phase
),
agent_flags AS (
	SELECT
		%s AS is_running,
		CASE WHEN am.latest_receipt_at IS NOT NULL THEN 1 ELSE 0 END AS has_meter_health,
		am.latest_final_state AS latest_final_state,
		%s AS is_receipt_stale,
		CASE
			WHEN am.run_id <> '' AND EXISTS (
				SELECT 1 FROM usage_events ue WHERE ue.agent_id = am.agent_id_text AND ue.run_id = am.run_id
			) THEN 1
			WHEN EXISTS (
				SELECT 1 FROM usage_rollups_daily rd WHERE %s
			) THEN 1
			ELSE 0
		END AS has_usage_data,
		am.latest_receipt_at AS latest_receipt_at
	FROM agent_mh am
)
SELECT
	is_running,
	has_meter_health,
	latest_final_state,
	is_receipt_stale,
	has_usage_data,
	CAST(COUNT(*) AS BIGINT) AS agent_count,
	MAX(latest_receipt_at) AS max_last_receipt_at
FROM agent_flags
GROUP BY is_running, has_meter_health, latest_final_state, is_receipt_stale, has_usage_data`,
		fmt.Sprintf(scopedAgentsCTE, strings.Join(scopeWhere, " AND ")),
		isRunningExpr,
		isStaleExpr,
		rdFlagCond,
	)

	rows := &entsql.Rows{}
	if err := drv.Query(ctx, usageRebindSQL(dialectName, query), args, rows); err != nil {
		return nil, mapError(err)
	}
	defer func() { _ = rows.Close() }()

	out := &store.UsageScopeCoverage{}
	for rows.Next() {
		var (
			isRunning        int
			hasMeterHealth   int
			latestFinalState string
			isReceiptStale   int
			hasUsageData     int
			agentCount       int64
			rawLastReceipt   any
		)
		if err := rows.Scan(
			&isRunning,
			&hasMeterHealth,
			&latestFinalState,
			&isReceiptStale,
			&hasUsageData,
			&agentCount,
			&rawLastReceipt,
		); err != nil {
			return nil, mapError(err)
		}
		out.Buckets = append(out.Buckets, store.UsageCoverageBucket{
			IsRunning:        isRunning != 0,
			HasMeterHealth:   hasMeterHealth != 0,
			LatestFinalState: latestFinalState,
			IsReceiptStale:   isReceiptStale != 0,
			HasUsageData:     hasUsageData != 0,
			AgentCount:       int(agentCount),
		})
		if rawLastReceipt != nil {
			ts, err := parseSQLTimestamp(rawLastReceipt)
			if err != nil {
				return nil, fmt.Errorf("parse max_last_receipt_at: %w", err)
			}
			if ts != nil && (out.LastReceiptAt == nil || ts.After(*out.LastReceiptAt)) {
				out.LastReceiptAt = ts
			}
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
	return dbNowFromDriver(ctx, drv, drv.Dialect())
}

func dbNowFromDriver(ctx context.Context, drv dialect.Driver, dialectName string) (time.Time, error) {
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

// buildRollupSumSelectSQL builds an overflow-safe SELECT list for all 18
// rollup counters:
//   - Postgres: SUM(bigint) returns arbitrary-precision numeric; clamped with
//     LEAST(..., 9223372036854775807::numeric) before BIGINT cast, plus a
//     boolean saturated flag.
//   - SQLite: splits each int64 into exact (col / 1e9, col % 1e9) integer
//     halves so SUM never raises "integer overflow" up to 100M saturated rows,
//     then recombines with saturation in Go.
func buildRollupSumSelectSQL(dialectName string) string {
	if dialectName == dialect.Postgres {
		parts := make([]string, 0, len(rollupSumColumns)+1)
		for _, col := range rollupSumColumns {
			parts = append(parts, fmt.Sprintf("CAST(LEAST(COALESCE(SUM(%s), 0), 9223372036854775807::numeric) AS BIGINT)", col))
		}
		satChecks := make([]string, 0, len(rollupSumColumns))
		for _, col := range rollupSumColumns {
			satChecks = append(satChecks, fmt.Sprintf("COALESCE(SUM(%s), 0) >= 9223372036854775807::numeric", col))
		}
		parts = append(parts, "("+strings.Join(satChecks, " OR ")+")")
		return strings.Join(parts, ",\n\t")
	}

	parts := make([]string, 0, len(rollupSumColumns)*2)
	for _, col := range rollupSumColumns {
		parts = append(parts,
			fmt.Sprintf("COALESCE(SUM(%s / %d), 0)", col, sqliteSplitRadix),
			fmt.Sprintf("COALESCE(SUM(%s %% %d), 0)", col, sqliteSplitRadix),
		)
	}
	return strings.Join(parts, ",\n\t")
}

func combineSplitInt64(hi, lo int64) (int64, bool) {
	if hi < 0 {
		hi = 0
	}
	if lo < 0 {
		lo = 0
	}
	carry := lo / sqliteSplitRadix
	rem := lo % sqliteSplitRadix
	if hi > math.MaxInt64-carry {
		return math.MaxInt64, true
	}
	hi += carry
	maxHi := math.MaxInt64 / sqliteSplitRadix
	if hi > maxHi {
		return math.MaxInt64, true
	}
	val := hi * sqliteSplitRadix
	if rem > math.MaxInt64-val {
		return math.MaxInt64, true
	}
	val += rem
	return val, val == math.MaxInt64
}

func scanUsageRollupTotalsRow(dialectName string, rows *entsql.Rows, prefixDests []any, dst *store.UsageRollupTotals) error {
	if dialectName == dialect.Postgres {
		var sat bool
		dests := make([]any, 0, len(prefixDests)+len(rollupSumColumns)+1)
		dests = append(dests, prefixDests...)
		dests = append(dests,
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
			&sat,
		)
		if err := rows.Scan(dests...); err != nil {
			return err
		}
		dst.Saturated = sat
		return nil
	}

	var hi, lo [18]int64
	dests := make([]any, 0, len(prefixDests)+36)
	dests = append(dests, prefixDests...)
	for i := 0; i < 18; i++ {
		dests = append(dests, &hi[i], &lo[i])
	}
	if err := rows.Scan(dests...); err != nil {
		return err
	}

	targets := []*int64{
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
	}
	saturated := false
	for i, ptr := range targets {
		v, sat := combineSplitInt64(hi[i], lo[i])
		*ptr = v
		if sat {
			saturated = true
		}
	}
	dst.Saturated = saturated
	return nil
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
