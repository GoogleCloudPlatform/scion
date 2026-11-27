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
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/runattribution"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

// EnsureRunAttributionForAgent implements store.UsageLedgerStore.EnsureRunAttributionForAgent.
// Used as the lazy attribution fallback at ingest time (P1.1 review O2) when
// an event names the agent's current run_id and no run_attribution row exists
// yet (narrow OFF->ON race). Always builds attribution from the Hub's agents
// row inside the transaction, re-checking that the agent is not soft-deleted
// and its current run_id still equals runID (N1); returns store.ErrNotFound
// if the agent moved to a different run or was deleted concurrently.
func (s *UsageLedgerStore) EnsureRunAttributionForAgent(ctx context.Context, agentID, runID, hubID string) (*store.RunAttribution, error) {
	if agentID == "" || runID == "" {
		return nil, fmt.Errorf("%w: agent_id and run_id are required", store.ErrInvalidInput)
	}
	uid, err := uuid.Parse(agentID)
	if err != nil {
		return nil, store.ErrNotFound
	}
	run := func(client *ent.Client) error {
		// If another concurrent writer already created run_attribution for
		// runID, return it without touching the agent row.
		exists, err := client.RunAttribution.Query().
			Where(runattribution.IDEQ(runID)).
			Exist(ctx)
		if err != nil {
			return mapError(err)
		}
		if exists {
			return nil
		}
		// Re-check inside the transaction that the agent is live and its
		// current run_id still matches runID before building attribution.
		q := client.Agent.Query().
			Where(
				agent.IDEQ(uid),
				agent.DeletedAtIsNil(),
				agent.RunIDEQ(runID),
			)
		if s.usesRowLocks() {
			q = q.ForUpdate()
		}
		row, err := q.Select(runAttributionAgentColumns...).Only(ctx)
		if err != nil {
			return mapError(err)
		}
		attr, err := buildRunAttributionFromAgentRow(ctx, client, row, runID, "", hubID, "")
		if err != nil {
			return err
		}
		_, err = insertRunAttributionOnConflictDoNothing(ctx, client, attr)
		return err
	}
	if s.inTx {
		if err := run(s.client); err != nil {
			return nil, err
		}
		return s.GetRunAttribution(ctx, runID)
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
	return s.GetRunAttribution(ctx, runID)
}

// IngestUsageBatch implements store.UsageLedgerStore.IngestUsageBatch.
//
// In a single transaction:
//  1. On Postgres, acquires a shared transaction-scoped advisory lock
//     hashtext('usage_rollup:' || agent_id) per §4.3 so concurrent ingest runs
//     commutatively while excluding admin re-rollup repair. (SQLite uses a
//     single-writer pool.)
//  2. Inserts each unique (hub_id, event_id) into usage_events using
//     INSERT ... ON CONFLICT (hub_id, event_id) DO NOTHING RETURNING event_id.
//  3. Aggregates ONLY newly inserted rows and upserts usage_rollups_daily and
//     usage_scope_counters_daily in canonical key-sorted order (preventing
//     deadlocks across concurrent replicas). Anomalous events (>50M summable
//     tokens) are stored and rolled up in usage_rollups_daily for visibility,
//     but their tokens_summable are excluded from usage_scope_counters_daily so
//     P3 automatic hard quota enforcement is never tripped by a corrupt spike.
//  4. Upserts meter_health (events and/or heartbeats) with monotonic sequence
//     progression and shrinking-only missing_ranges.
func (s *UsageLedgerStore) IngestUsageBatch(ctx context.Context, in *store.UsageIngestBatchInput) (*store.UsageIngestBatchResult, error) {
	if in == nil {
		return nil, fmt.Errorf("%w: ingest input is required", store.ErrInvalidInput)
	}
	if len(in.Events) == 0 && in.MeterHealth == nil {
		return &store.UsageIngestBatchResult{}, nil
	}

	var result *store.UsageIngestBatchResult
	run := func(client *ent.Client) error {
		var err error
		result, err = s.ingestUsageBatchTx(ctx, client, in)
		return err
	}

	if s.inTx {
		if err := run(s.client); err != nil {
			return nil, err
		}
		return result, nil
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
	return result, nil
}

func (s *UsageLedgerStore) ingestUsageBatchTx(ctx context.Context, client *ent.Client, in *store.UsageIngestBatchInput) (*store.UsageIngestBatchResult, error) {
	drv := client.Driver()
	dialectName := drv.Dialect()

	// §4.3: Acquire Postgres shared advisory lock per agent_id so ingest runs
	// concurrently across replicas while serializing against admin repair.
	if dialectName == dialect.Postgres && in.AgentID != "" {
		var res sql.Result
		if err := drv.Exec(ctx, "SELECT pg_advisory_xact_lock_shared(hashtext('usage_rollup:' || $1))", []any{in.AgentID}, &res); err != nil {
			return nil, mapError(err)
		}
	}

	// Deduplicate within the batch by (hub_id, event_id) before hitting SQL so
	// intra-batch duplicates are cleanly counted and never double-aggregated.
	type eventKey struct {
		hubID   string
		eventID string
	}
	seenInBatch := make(map[eventKey]struct{}, len(in.Events))
	uniqueEvents := make([]*store.UsageEvent, 0, len(in.Events))
	intraDuplicates := 0
	for _, ev := range in.Events {
		if ev == nil {
			continue
		}
		k := eventKey{hubID: ev.HubID, eventID: ev.EventID}
		if _, dup := seenInBatch[k]; dup {
			intraDuplicates++
			continue
		}
		seenInBatch[k] = struct{}{}
		uniqueEvents = append(uniqueEvents, ev)
	}

	// O3: Sort uniqueEvents by (hub_id, event_id) before inserting so
	// concurrent overlapping batches acquire unique-index locks in a
	// canonical order and cannot deadlock in Postgres.
	sort.Slice(uniqueEvents, func(i, j int) bool {
		if uniqueEvents[i].HubID != uniqueEvents[j].HubID {
			return uniqueEvents[i].HubID < uniqueEvents[j].HubID
		}
		return uniqueEvents[i].EventID < uniqueEvents[j].EventID
	})

	insertedEvents := make([]*store.UsageEvent, 0, len(uniqueEvents))
	var rejectedRows []store.UsageIngestRowRejection
	var rejectedEvents []*store.UsageEvent
	dbDuplicates := 0

	if len(uniqueEvents) > 0 {
		jsonbParam := "?"
		if dialectName == dialect.Postgres {
			jsonbParam = "CAST(? AS jsonb)"
		}
		insertSQL := usageRebindSQL(dialectName, fmt.Sprintf(`
INSERT INTO usage_events (
	id, hub_id, event_id, run_id, project_id, agent_id, billing_user_id, broker_id,
	harness, model, model_source, usage_source, source_kind, source_identity,
	native_request_id, session_id_hash, status, calls,
	input, output, cache_read, cache_write, cache_write_1h, reasoning,
	token_status, malformed_fields, reported_cost_micros, reported_cost_currency,
	reported_cost_source, event_ts, received_at, producer_epoch, producer_seq,
	anomalous, charged_group_ids, relocated_from_hub_id
) VALUES (
	?, ?, ?, ?, ?, ?, ?, ?,
	?, ?, ?, ?, ?, ?,
	?, ?, ?, ?,
	?, ?, ?, ?, ?, ?,
	?, %s, ?, ?,
	?, ?, ?, ?, ?,
	?, %s, ?
)
ON CONFLICT (hub_id, event_id) DO NOTHING
RETURNING event_id`, jsonbParam, jsonbParam))

		for _, ev := range uniqueEvents {
			// Wrap each row insert in a SAVEPOINT so a per-row DB/data error
			// (e.g., Postgres invalid byte sequence 22021 or constraint error
			// 23xxx) rolls back only that row and becomes a per-event rejection
			// instead of aborting the whole batch transaction.
			var spRes sql.Result
			if err := drv.Exec(ctx, "SAVEPOINT sp_usage_event", []any{}, &spRes); err != nil {
				return nil, mapError(err)
			}
			inserted, err := insertUsageEventReturning(ctx, drv, insertSQL, ev)
			if err != nil {
				if isUsageEventRowDataError(err) {
					if rbErr := drv.Exec(ctx, "ROLLBACK TO SAVEPOINT sp_usage_event", []any{}, &spRes); rbErr != nil {
						return nil, mapError(rbErr)
					}
					if relErr := drv.Exec(ctx, "RELEASE SAVEPOINT sp_usage_event", []any{}, &spRes); relErr != nil {
						return nil, mapError(relErr)
					}
					rejectedRows = append(rejectedRows, store.UsageIngestRowRejection{
						EventID: ev.EventID,
						Reason:  err.Error(),
					})
					rejectedEvents = append(rejectedEvents, ev)
					continue
				}
				return nil, mapError(err)
			}
			if err := drv.Exec(ctx, "RELEASE SAVEPOINT sp_usage_event", []any{}, &spRes); err != nil {
				return nil, mapError(err)
			}
			if inserted {
				insertedEvents = append(insertedEvents, ev)
			} else {
				dbDuplicates++
			}
		}
	}

	// Aggregate ONLY newly inserted rows into daily rollups and scope counters.
	if len(insertedEvents) > 0 {
		if err := upsertDailyRollupsTx(ctx, drv, dialectName, insertedEvents); err != nil {
			return nil, err
		}
		if err := upsertScopeCountersDailyTx(ctx, drv, dialectName, insertedEvents); err != nil {
			return nil, err
		}
	}

	// Update meter_health (for events and/or heartbeats).
	if in.MeterHealth != nil && in.MeterHealth.AgentID != "" && in.MeterHealth.RunID != "" && in.MeterHealth.ProducerEpoch != "" {
		mhUpd := in.MeterHealth
		var anomalousDelta int64
		for _, ev := range insertedEvents {
			if ev.Anomalous && ev.RunID == mhUpd.RunID && ev.ProducerEpoch == mhUpd.ProducerEpoch {
				anomalousDelta++
			}
		}
		receivedSeqs := mhUpd.ReceivedSeqs
		if len(rejectedEvents) > 0 && len(receivedSeqs) > 0 {
			dropSeqCounts := make(map[int64]int, len(rejectedEvents))
			for _, ev := range rejectedEvents {
				if ev.RunID == mhUpd.RunID && ev.ProducerEpoch == mhUpd.ProducerEpoch && ev.ProducerSeq > 0 {
					dropSeqCounts[ev.ProducerSeq]++
				}
			}
			if len(dropSeqCounts) > 0 {
				filtered := make([]int64, 0, len(receivedSeqs))
				for _, seq := range receivedSeqs {
					if dropSeqCounts[seq] > 0 {
						dropSeqCounts[seq]--
						continue
					}
					filtered = append(filtered, seq)
				}
				receivedSeqs = filtered
			}
		}
		mh := &store.MeterHealth{
			AgentID:              mhUpd.AgentID,
			RunID:                mhUpd.RunID,
			ProducerEpoch:        mhUpd.ProducerEpoch,
			LastReceiptAt:        mhUpd.LastReceiptAt,
			MaxSeqSeen:           mhUpd.ProducerSeq,
			LastSourceActivityAt: mhUpd.LastSourceActivityAt,
			SupportedSignals:     mhUpd.SupportedSignals,
			SchemaDriftDetected:  mhUpd.SchemaDriftDetected,
			FinalState:           mhUpd.FinalState,
		}
		if err := s.upsertMeterHealthTx(ctx, client, mh, anomalousDelta, receivedSeqs); err != nil {
			return nil, err
		}
	}

	return &store.UsageIngestBatchResult{
		Accepted:       len(insertedEvents),
		Duplicates:     intraDuplicates + dbDuplicates,
		Rejected:       rejectedRows,
		InsertedEvents: insertedEvents,
	}, nil
}

// isUsageEventRowDataError reports whether err is a per-row data or integrity
// constraint error (Postgres SQLSTATE class 22 data exception or class 23
// integrity constraint violation; SQLite SQLITE_CONSTRAINT or SQLITE_MISMATCH)
// that should reject only the offending event rather than failing the batch.
// Transient/infrastructure errors (deadlocks 40P01, serialization 40001,
// connection errors 08xxx, SQLITE_BUSY/LOCKED) return false so the batch fails
// and the client retries.
func isUsageEventRowDataError(err error) bool {
	if err == nil {
		return false
	}
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) {
		state := pgErr.SQLState()
		return strings.HasPrefix(state, "22") || strings.HasPrefix(state, "23")
	}
	var sqliteErr interface{ Code() int }
	if errors.As(err, &sqliteErr) {
		primary := sqliteErr.Code() & 0xff
		return primary == 19 || primary == 20 // SQLITE_CONSTRAINT, SQLITE_MISMATCH
	}
	return false
}

func insertUsageEventReturning(ctx context.Context, drv dialect.Driver, insertSQL string, ev *store.UsageEvent) (bool, error) {
	id := uuid.New()
	if ev.ID != "" {
		if parsed, err := uuid.Parse(ev.ID); err == nil {
			id = parsed
		}
	}
	malformedJSON, err := nullableStringSliceJSON(ev.MalformedFields)
	if err != nil {
		return false, err
	}
	chargedGroupsJSON, err := nullableStringSliceJSON(ev.ChargedGroupIDs)
	if err != nil {
		return false, err
	}
	eventTS := ev.EventTS.UTC().Truncate(time.Microsecond)
	receivedAt := ev.ReceivedAt
	if receivedAt.IsZero() {
		receivedAt = time.Now()
	}
	receivedAt = receivedAt.UTC().Truncate(time.Microsecond)

	args := []any{
		id,
		ev.HubID,
		ev.EventID,
		ev.RunID,
		ev.ProjectID,
		ev.AgentID,
		nullableStringArg(ev.BillingUserID),
		ev.BrokerID,
		ev.Harness,
		ev.Model,
		ev.ModelSource,
		ev.UsageSource,
		ev.SourceKind,
		ev.SourceIdentity,
		nullableStringArg(ev.NativeRequestID),
		nullableStringArg(ev.SessionIDHash),
		ev.Status,
		ev.Calls,
		nullableInt64Arg(ev.Input),
		nullableInt64Arg(ev.Output),
		nullableInt64Arg(ev.CacheRead),
		nullableInt64Arg(ev.CacheWrite),
		nullableInt64Arg(ev.CacheWrite1h),
		nullableInt64Arg(ev.Reasoning),
		ev.TokenStatus,
		malformedJSON,
		nullableInt64Arg(ev.ReportedCostMicros),
		nullableStringArg(ev.ReportedCostCurrency),
		nullableStringArg(ev.ReportedCostSource),
		eventTS,
		receivedAt,
		ev.ProducerEpoch,
		ev.ProducerSeq,
		ev.Anomalous,
		chargedGroupsJSON,
		nullableStringArg(ev.RelocatedFromHubID),
	}

	rows := &entsql.Rows{}
	if err := drv.Query(ctx, insertSQL, args, rows); err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()

	inserted := false
	if rows.Next() {
		var returnedEventID string
		if err := rows.Scan(&returnedEventID); err != nil {
			return false, err
		}
		inserted = returnedEventID != ""
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return inserted, nil
}

type dailyRollupKey struct {
	bucketStart   time.Time
	projectID     string
	billingUserID string
	agentID       string
	harness       string
	model         string
	usageSource   string
}

type dailyRollupAgg struct {
	key                dailyRollupKey
	calls              int64
	errorCalls         int64
	input              int64
	output             int64
	cacheRead          int64
	cacheWrite         int64
	cacheWrite1h       int64
	reasoning          int64
	reportedCostMicros int64
	events             int64
	nullTokenEvents    int64
	firstEventTS       time.Time
	lastEventTS        time.Time
}

func upsertDailyRollupsTx(ctx context.Context, drv dialect.Driver, dialectName string, events []*store.UsageEvent) error {
	aggs := make(map[dailyRollupKey]*dailyRollupAgg, len(events))
	for _, ev := range events {
		eventTS := ev.EventTS.UTC().Truncate(time.Microsecond)
		bucketStart := utcDayStart(eventTS)
		billingUserID := ""
		if ev.BillingUserID != nil {
			billingUserID = *ev.BillingUserID
		}
		k := dailyRollupKey{
			bucketStart:   bucketStart,
			projectID:     ev.ProjectID,
			billingUserID: billingUserID,
			agentID:       ev.AgentID,
			harness:       ev.Harness,
			model:         ev.Model,
			usageSource:   ev.UsageSource,
		}
		agg, ok := aggs[k]
		if !ok {
			agg = &dailyRollupAgg{
				key:          k,
				firstEventTS: eventTS,
				lastEventTS:  eventTS,
			}
			aggs[k] = agg
		}
		agg.calls = saturatingAddInt64(agg.calls, ev.Calls)
		if ev.Status == telemetrycontract.StatusError {
			agg.errorCalls = saturatingAddInt64(agg.errorCalls, ev.Calls)
		}
		if ev.Input != nil {
			agg.input = saturatingAddInt64(agg.input, *ev.Input)
		}
		if ev.Output != nil {
			agg.output = saturatingAddInt64(agg.output, *ev.Output)
		}
		if ev.CacheRead != nil {
			agg.cacheRead = saturatingAddInt64(agg.cacheRead, *ev.CacheRead)
		}
		if ev.CacheWrite != nil {
			agg.cacheWrite = saturatingAddInt64(agg.cacheWrite, *ev.CacheWrite)
		}
		if ev.CacheWrite1h != nil {
			agg.cacheWrite1h = saturatingAddInt64(agg.cacheWrite1h, *ev.CacheWrite1h)
		}
		if ev.Reasoning != nil {
			agg.reasoning = saturatingAddInt64(agg.reasoning, *ev.Reasoning)
		}
		if ev.ReportedCostMicros != nil {
			agg.reportedCostMicros = saturatingAddInt64(agg.reportedCostMicros, *ev.ReportedCostMicros)
		}
		agg.events = saturatingAddInt64(agg.events, 1)
		if ev.Input == nil && ev.Output == nil && ev.CacheRead == nil && ev.CacheWrite == nil && ev.CacheWrite1h == nil && ev.Reasoning == nil {
			agg.nullTokenEvents = saturatingAddInt64(agg.nullTokenEvents, 1)
		}
		if eventTS.Before(agg.firstEventTS) {
			agg.firstEventTS = eventTS
		}
		if eventTS.After(agg.lastEventTS) {
			agg.lastEventTS = eventTS
		}
	}

	// Canonical key-sorted order prevents deadlocks across concurrent replicas (§4.3).
	ordered := make([]*dailyRollupAgg, 0, len(aggs))
	for _, agg := range aggs {
		ordered = append(ordered, agg)
	}
	sort.Slice(ordered, func(i, j int) bool {
		a, b := ordered[i].key, ordered[j].key
		if !a.bucketStart.Equal(b.bucketStart) {
			return a.bucketStart.Before(b.bucketStart)
		}
		if a.projectID != b.projectID {
			return a.projectID < b.projectID
		}
		if a.billingUserID != b.billingUserID {
			return a.billingUserID < b.billingUserID
		}
		if a.agentID != b.agentID {
			return a.agentID < b.agentID
		}
		if a.harness != b.harness {
			return a.harness < b.harness
		}
		if a.model != b.model {
			return a.model < b.model
		}
		return a.usageSource < b.usageSource
	})

	upsertSQL := usageRebindSQL(dialectName, `
INSERT INTO usage_rollups_daily (
	id, bucket_start, project_id, billing_user_id, agent_id, harness, model, usage_source,
	calls, error_calls, input, output, cache_read, cache_write, cache_write_1h, reasoning,
	reported_cost_micros, events, null_token_events, first_event_ts, last_event_ts
) VALUES (
	?, ?, ?, ?, ?, ?, ?, ?,
	?, ?, ?, ?, ?, ?, ?, ?,
	?, ?, ?, ?, ?
)
ON CONFLICT (bucket_start, project_id, billing_user_id, agent_id, harness, model, usage_source)
DO UPDATE SET
	calls = CASE WHEN EXCLUDED.calls > 9223372036854775807 - usage_rollups_daily.calls THEN 9223372036854775807 ELSE usage_rollups_daily.calls + EXCLUDED.calls END,
	error_calls = CASE WHEN EXCLUDED.error_calls > 9223372036854775807 - usage_rollups_daily.error_calls THEN 9223372036854775807 ELSE usage_rollups_daily.error_calls + EXCLUDED.error_calls END,
	input = CASE WHEN EXCLUDED.input > 9223372036854775807 - usage_rollups_daily.input THEN 9223372036854775807 ELSE usage_rollups_daily.input + EXCLUDED.input END,
	output = CASE WHEN EXCLUDED.output > 9223372036854775807 - usage_rollups_daily.output THEN 9223372036854775807 ELSE usage_rollups_daily.output + EXCLUDED.output END,
	cache_read = CASE WHEN EXCLUDED.cache_read > 9223372036854775807 - usage_rollups_daily.cache_read THEN 9223372036854775807 ELSE usage_rollups_daily.cache_read + EXCLUDED.cache_read END,
	cache_write = CASE WHEN EXCLUDED.cache_write > 9223372036854775807 - usage_rollups_daily.cache_write THEN 9223372036854775807 ELSE usage_rollups_daily.cache_write + EXCLUDED.cache_write END,
	cache_write_1h = CASE WHEN EXCLUDED.cache_write_1h > 9223372036854775807 - usage_rollups_daily.cache_write_1h THEN 9223372036854775807 ELSE usage_rollups_daily.cache_write_1h + EXCLUDED.cache_write_1h END,
	reasoning = CASE WHEN EXCLUDED.reasoning > 9223372036854775807 - usage_rollups_daily.reasoning THEN 9223372036854775807 ELSE usage_rollups_daily.reasoning + EXCLUDED.reasoning END,
	reported_cost_micros = CASE WHEN EXCLUDED.reported_cost_micros > 9223372036854775807 - usage_rollups_daily.reported_cost_micros THEN 9223372036854775807 ELSE usage_rollups_daily.reported_cost_micros + EXCLUDED.reported_cost_micros END,
	events = CASE WHEN EXCLUDED.events > 9223372036854775807 - usage_rollups_daily.events THEN 9223372036854775807 ELSE usage_rollups_daily.events + EXCLUDED.events END,
	null_token_events = CASE WHEN EXCLUDED.null_token_events > 9223372036854775807 - usage_rollups_daily.null_token_events THEN 9223372036854775807 ELSE usage_rollups_daily.null_token_events + EXCLUDED.null_token_events END,
	first_event_ts = CASE WHEN EXCLUDED.first_event_ts < usage_rollups_daily.first_event_ts THEN EXCLUDED.first_event_ts ELSE usage_rollups_daily.first_event_ts END,
	last_event_ts = CASE WHEN EXCLUDED.last_event_ts > usage_rollups_daily.last_event_ts THEN EXCLUDED.last_event_ts ELSE usage_rollups_daily.last_event_ts END`)

	for _, agg := range ordered {
		var res sql.Result
		args := []any{
			uuid.New(),
			agg.key.bucketStart,
			agg.key.projectID,
			agg.key.billingUserID,
			agg.key.agentID,
			agg.key.harness,
			agg.key.model,
			agg.key.usageSource,
			agg.calls,
			agg.errorCalls,
			agg.input,
			agg.output,
			agg.cacheRead,
			agg.cacheWrite,
			agg.cacheWrite1h,
			agg.reasoning,
			agg.reportedCostMicros,
			agg.events,
			agg.nullTokenEvents,
			agg.firstEventTS,
			agg.lastEventTS,
		}
		if err := drv.Exec(ctx, upsertSQL, args, &res); err != nil {
			return mapError(err)
		}
	}
	return nil
}

type scopeCounterKey struct {
	scopeKind string
	scopeID   string
	dayUTC    time.Time
}

type scopeCounterAgg struct {
	key             scopeCounterKey
	tokensSummable  int64
	calls           int64
	unmeasuredCalls int64
}

func upsertScopeCountersDailyTx(ctx context.Context, drv dialect.Driver, dialectName string, events []*store.UsageEvent) error {
	aggs := make(map[scopeCounterKey]*scopeCounterAgg, len(events)*3)
	addScope := func(kind, id string, dayUTC time.Time, tokens, calls, unmeasured int64) {
		k := scopeCounterKey{scopeKind: kind, scopeID: id, dayUTC: dayUTC}
		agg, ok := aggs[k]
		if !ok {
			agg = &scopeCounterAgg{key: k}
			aggs[k] = agg
		}
		agg.tokensSummable = saturatingAddInt64(agg.tokensSummable, tokens)
		agg.calls = saturatingAddInt64(agg.calls, calls)
		agg.unmeasuredCalls = saturatingAddInt64(agg.unmeasuredCalls, unmeasured)
	}

	for _, ev := range events {
		dayUTC := utcDayStart(ev.EventTS.UTC())
		// Anomalous events (>50M summable tokens) are stored and rolled up in
		// usage_rollups_daily for visibility, but their tokens_summable are
		// excluded from usage_scope_counters_daily so P3 automatic hard quota
		// enforcement is never tripped by a single corrupt spike (§4.1 item 3).
		var tokens int64
		if !ev.Anomalous {
			if ev.Input != nil {
				tokens = saturatingAddInt64(tokens, *ev.Input)
			}
			if ev.Output != nil {
				tokens = saturatingAddInt64(tokens, *ev.Output)
			}
			if ev.CacheRead != nil {
				tokens = saturatingAddInt64(tokens, *ev.CacheRead)
			}
			if ev.CacheWrite != nil {
				tokens = saturatingAddInt64(tokens, *ev.CacheWrite)
			}
		}
		var unmeasured int64
		if ev.Calls > 0 && (ev.TokenStatus == telemetrycontract.TokenStatusNotReported ||
			(ev.Input == nil && ev.Output == nil && ev.CacheRead == nil && ev.CacheWrite == nil)) {
			unmeasured = ev.Calls
		}

		// Project scope is always updated.
		addScope(store.UsageScopeKindProject, ev.ProjectID, dayUTC, tokens, ev.Calls, unmeasured)

		// User and user_project scopes are updated only when billing_user_id is non-NULL (§3.2).
		if ev.BillingUserID != nil && *ev.BillingUserID != "" {
			uid := *ev.BillingUserID
			addScope(store.UsageScopeKindUser, uid, dayUTC, tokens, ev.Calls, unmeasured)
			addScope(store.UsageScopeKindUserProject, uid+":"+ev.ProjectID, dayUTC, tokens, ev.Calls, unmeasured)
		}
	}

	// Canonical key-sorted order prevents deadlocks across concurrent replicas (§4.3).
	ordered := make([]*scopeCounterAgg, 0, len(aggs))
	for _, agg := range aggs {
		ordered = append(ordered, agg)
	}
	sort.Slice(ordered, func(i, j int) bool {
		a, b := ordered[i].key, ordered[j].key
		if a.scopeKind != b.scopeKind {
			return a.scopeKind < b.scopeKind
		}
		if a.scopeID != b.scopeID {
			return a.scopeID < b.scopeID
		}
		return a.dayUTC.Before(b.dayUTC)
	})

	upsertSQL := usageRebindSQL(dialectName, `
INSERT INTO usage_scope_counters_daily (
	id, scope_kind, scope_id, day_utc, tokens_summable, calls, unmeasured_calls
) VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (scope_kind, scope_id, day_utc)
DO UPDATE SET
	tokens_summable = CASE WHEN EXCLUDED.tokens_summable > 9223372036854775807 - usage_scope_counters_daily.tokens_summable THEN 9223372036854775807 ELSE usage_scope_counters_daily.tokens_summable + EXCLUDED.tokens_summable END,
	calls = CASE WHEN EXCLUDED.calls > 9223372036854775807 - usage_scope_counters_daily.calls THEN 9223372036854775807 ELSE usage_scope_counters_daily.calls + EXCLUDED.calls END,
	unmeasured_calls = CASE WHEN EXCLUDED.unmeasured_calls > 9223372036854775807 - usage_scope_counters_daily.unmeasured_calls THEN 9223372036854775807 ELSE usage_scope_counters_daily.unmeasured_calls + EXCLUDED.unmeasured_calls END`)

	for _, agg := range ordered {
		var res sql.Result
		args := []any{
			uuid.New(),
			agg.key.scopeKind,
			agg.key.scopeID,
			agg.key.dayUTC,
			agg.tokensSummable,
			agg.calls,
			agg.unmeasuredCalls,
		}
		if err := drv.Exec(ctx, upsertSQL, args, &res); err != nil {
			return mapError(err)
		}
	}
	return nil
}

// maxMeterHealthMissingRanges bounds the number of disjoint missing_ranges
// entries persisted in meter_health per (agent_id, run_id, producer_epoch).
// When exceeded (e.g., an adversarial every-other-seq producer), adjacent
// ranges separated by the smallest gaps are conservatively coalesced into
// larger ranges. Coalescing may conservatively include already-received seqs
// between adjacent gaps, but NEVER drops an unreceived seq or advances
// last_contiguous_seq past an unreceived seq.
const maxMeterHealthMissingRanges = 1024

// advanceIngestSeqState computes (last_contiguous_seq, max_seq_seen,
// missing_ranges, coalesced) for IngestUsageBatch where the Hub is the sole
// authority on sequence gaps and contiguity (RV1, R1):
//   - Always adds (existingMax, nextMax] minus receivedSeqs to missing_ranges,
//     regardless of any producer-supplied missing_ranges.
//   - Producer-supplied missing_ranges and last_contiguous_seq are ignored so
//     they can never shrink or close Hub-recorded gaps for unreceived seqs.
//   - last_contiguous_seq advances strictly through received sequences.
//   - Range subtraction is a single linear merge over sorted ranges.
//   - Stored missing_ranges are capped at maxMeterHealthMissingRanges via
//     conservative coalescing of the closest adjacent ranges.
func advanceIngestSeqState(
	existingContig, existingMax int64,
	existingMissing []store.SeqRange,
	producerSeq int64,
	receivedSeqs []int64,
) (nextContig, nextMax int64, nextMissing []store.SeqRange, coalesced bool) {
	nextContig = existingContig
	nextMax = existingMax
	if producerSeq > nextMax {
		nextMax = producerSeq
	}
	for _, seq := range receivedSeqs {
		if seq > nextMax {
			nextMax = seq
		}
	}

	cur := normalizeSeqRanges(existingMissing)
	if nextMax > existingMax {
		newRange := store.SeqRange{Start: existingMax + 1, End: nextMax}
		if len(cur) > 0 && newRange.Start <= cur[len(cur)-1].End+1 {
			if newRange.End > cur[len(cur)-1].End {
				cur[len(cur)-1].End = newRange.End
			}
		} else {
			cur = append(cur, newRange)
		}
	}
	if len(receivedSeqs) > 0 {
		cur = subtractSeqsFromRanges(cur, receivedSeqs)
	}
	cur = pruneRangesAtOrBelow(cur, nextContig)

	if len(receivedSeqs) > 0 && nextMax > nextContig {
		if len(cur) == 0 {
			nextContig = nextMax
		} else if cur[0].Start-1 > nextContig {
			nextContig = cur[0].Start - 1
		}
		cur = pruneRangesAtOrBelow(cur, nextContig)
	}

	cur, coalesced = capSeqRanges(cur, maxMeterHealthMissingRanges)
	if len(cur) == 0 {
		return nextContig, nextMax, nil, coalesced
	}
	return nextContig, nextMax, cur, coalesced
}

func normalizeSeqRanges(in []store.SeqRange) []store.SeqRange {
	if len(in) == 0 {
		return nil
	}
	valid := make([]store.SeqRange, 0, len(in))
	for _, r := range in {
		if r.Start <= 0 {
			r.Start = 1
		}
		if r.End < r.Start {
			continue
		}
		valid = append(valid, r)
	}
	if len(valid) == 0 {
		return nil
	}
	sort.Slice(valid, func(i, j int) bool {
		if valid[i].Start != valid[j].Start {
			return valid[i].Start < valid[j].Start
		}
		return valid[i].End < valid[j].End
	})
	out := []store.SeqRange{valid[0]}
	for _, r := range valid[1:] {
		last := &out[len(out)-1]
		if r.Start <= last.End+1 {
			if r.End > last.End {
				last.End = r.End
			}
		} else {
			out = append(out, r)
		}
	}
	return out
}

// subtractSeqsFromRanges removes all positive seqs from sorted, disjoint
// ranges in a single linear merge pass after sorting seqs (O(R + S log S)).
func subtractSeqsFromRanges(ranges []store.SeqRange, seqs []int64) []store.SeqRange {
	if len(ranges) == 0 || len(seqs) == 0 {
		return ranges
	}
	sorted := make([]int64, 0, len(seqs))
	for _, s := range seqs {
		if s > 0 {
			sorted = append(sorted, s)
		}
	}
	if len(sorted) == 0 {
		return ranges
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	out := make([]store.SeqRange, 0, len(ranges))
	si := 0
	for _, r := range ranges {
		for si < len(sorted) && sorted[si] < r.Start {
			si++
		}
		curStart := r.Start
		for si < len(sorted) && sorted[si] <= r.End {
			s := sorted[si]
			if s > curStart {
				out = append(out, store.SeqRange{Start: curStart, End: s - 1})
			}
			curStart = s + 1
			for si < len(sorted) && sorted[si] == s {
				si++
			}
		}
		if curStart <= r.End {
			out = append(out, store.SeqRange{Start: curStart, End: r.End})
		}
	}
	return out
}

// capSeqRanges enforces len(out) <= maxRanges by conservatively coalescing the
// (len(ranges) - maxRanges) adjacent pairs separated by the smallest gaps
// (breaking ties toward older/lower ranges so recent gaps remain granular).
// Coalescing only expands ranges across intermediate received seqs and never
// drops an unreceived seq or increases ranges[0].Start.
func capSeqRanges(ranges []store.SeqRange, maxRanges int) ([]store.SeqRange, bool) {
	if maxRanges <= 0 || len(ranges) <= maxRanges {
		return ranges, false
	}
	type gapPair struct {
		idx int
		gap int64
	}
	pairs := make([]gapPair, len(ranges)-1)
	for i := 0; i < len(ranges)-1; i++ {
		pairs[i] = gapPair{
			idx: i,
			gap: ranges[i+1].Start - ranges[i].End - 1,
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].gap != pairs[j].gap {
			return pairs[i].gap < pairs[j].gap
		}
		return pairs[i].idx < pairs[j].idx
	})
	excess := len(ranges) - maxRanges
	mergeAfter := make([]bool, len(ranges)-1)
	for i := 0; i < excess; i++ {
		mergeAfter[pairs[i].idx] = true
	}
	out := make([]store.SeqRange, 0, maxRanges)
	cur := ranges[0]
	for i := 0; i < len(ranges)-1; i++ {
		if mergeAfter[i] {
			cur.End = ranges[i+1].End
		} else {
			out = append(out, cur)
			cur = ranges[i+1]
		}
	}
	out = append(out, cur)
	return out, true
}

func pruneRangesAtOrBelow(ranges []store.SeqRange, cutoff int64) []store.SeqRange {
	if len(ranges) == 0 {
		return nil
	}
	var out []store.SeqRange
	for _, r := range ranges {
		if r.End <= cutoff {
			continue
		}
		if r.Start <= cutoff {
			r.Start = cutoff + 1
		}
		out = append(out, r)
	}
	return out
}

func saturatingAddInt64(a, b int64) int64 {
	if b <= 0 {
		return a
	}
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

func utcDayStart(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

func nullableStringArg(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

func nullableInt64Arg(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

func nullableStringSliceJSON(s []string) (any, error) {
	if len(s) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

func usageRebindSQL(dialectName, query string) string {
	if dialectName != dialect.Postgres {
		return query
	}
	var b strings.Builder
	b.Grow(len(query) + 32)
	argNum := 1
	for i := 0; i < len(query); i++ {
		if query[i] == '?' {
			fmt.Fprintf(&b, "$%d", argNum)
			argNum++
		} else {
			b.WriteByte(query[i])
		}
	}
	return b.String()
}
