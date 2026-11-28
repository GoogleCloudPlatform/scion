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
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"entgo.io/ent/dialect"
	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
	entasm "github.com/GoogleCloudPlatform/scion/pkg/ent/agentsessionmetrics"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/runattribution"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/usageevent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

const (
	defaultLegacySessionMetricsBackfillPageSize = 100
	defaultLegacyBackfillBatchYield             = 25 * time.Millisecond
	legacySessionMetricsSourceKind              = "agent_session_metrics"
	legacySessionMetricsProducerEpoch           = "legacy-session-metrics"
)

// runAttributionAgentColumnsWithRunIDUpdatedAt extends runAttributionAgentColumns
// with agents.run_id_updated_at so catch-up attribution can determine when the
// agent's current run_id was assigned without an extra query.
var runAttributionAgentColumnsWithRunIDUpdatedAt = append(
	append([]string(nil), runAttributionAgentColumns...),
	agent.FieldRunIDUpdatedAt,
)

// ConvertLegacySessionMetricsRow implements store.UsageLedgerStore.ConvertLegacySessionMetricsRow.
// Converts a single newly posted agent_session_metrics row into a
// legacy_session_metrics usage_events row through IngestUsageBatch and stamps
// converted_at on the source row in the same transaction (D9a).
func (s *UsageLedgerStore) ConvertLegacySessionMetricsRow(
	ctx context.Context,
	hubID string,
	row *store.AgentSessionMetrics,
	preferredRunID string,
) (bool, error) {
	if row == nil || row.ID == "" || row.AgentID == "" || row.ProjectID == "" || row.SessionID == "" {
		return false, fmt.Errorf("%w: agent_session_metrics row with id, agent_id, project_id, session_id is required", store.ErrInvalidInput)
	}
	if hubID == "" {
		hubID = "hub-local"
	}

	var converted bool
	var wroteHourly bool
	run := func(client *ent.Client) error {
		receivedAt := time.Now().UTC().Truncate(time.Microsecond)
		attrCache := make(map[string]*store.RunAttribution, 1)
		ev, err := s.prepareLegacyUsageEventTx(ctx, client, hubID, row, preferredRunID, receivedAt, true, attrCache)
		if err != nil {
			return err
		}
		mark := store.SessionMetricsConversionMark{ID: row.ID}
		in := &store.UsageIngestBatchInput{
			AgentID: row.AgentID,
		}
		if ev != nil {
			mark.EventID = ev.EventID
			in.Events = []*store.UsageEvent{ev}
		}
		in.MarkSessionMetricsConverted = []store.SessionMetricsConversionMark{mark}
		res, wh, err := s.ingestUsageBatchTx(ctx, client, in)
		if err != nil {
			return err
		}
		wroteHourly = wh
		converted = res.Accepted > 0
		return nil
	}

	if s.inTx {
		if err := run(s.client); err != nil {
			return false, err
		}
		return converted, nil
	}

	tx, err := s.client.Tx(ctx)
	if err != nil {
		return false, mapError(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := run(tx.Client()); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, mapError(err)
	}
	if wroteHourly && s.hourlyCoverageRecorded != nil {
		s.hourlyCoverageRecorded.Store(true)
	}
	return converted, nil
}

// BackfillLegacySessionMetrics implements store.UsageLedgerStore.BackfillLegacySessionMetrics.
func (s *UsageLedgerStore) BackfillLegacySessionMetrics(ctx context.Context, hubID string) (store.LegacySessionMetricsBackfillResult, error) {
	return s.backfillLegacySessionMetricsPaged(ctx, hubID, defaultLegacySessionMetricsBackfillPageSize, defaultLegacyBackfillBatchYield, nil, nil)
}

// backfillLegacySessionMetricsPaged processes unconverted agent_session_metrics
// rows (WHERE converted_at IS NULL) in bounded keyset-paginated batches
// (id > lastID) with a short inter-batch yield. Keyset pagination within a
// pass guarantees that permanently rejected rows cannot starve later rows (O-a).
// Each batch acquires a Postgres transaction-scoped advisory lock (and an
// in-process mutex for SQLite), converts non-overlapping rows through
// ingestUsageBatchTx with IsBackfill=true, and stamps converted_at on every
// non-rejected row in the batch inside the same transaction.
func (s *UsageLedgerStore) backfillLegacySessionMetricsPaged(
	ctx context.Context,
	hubID string,
	pageSize int,
	batchYield time.Duration,
	afterBatchHook func(batch int, res store.LegacySessionMetricsBackfillResult) error,
	mutateBatchInput func(in *store.UsageIngestBatchInput),
) (store.LegacySessionMetricsBackfillResult, error) {
	if pageSize <= 0 {
		pageSize = defaultLegacySessionMetricsBackfillPageSize
	}
	if hubID == "" {
		hubID = "hub-local"
	}

	if s.legacyBackfillMu != nil {
		s.legacyBackfillMu.Lock()
		defer s.legacyBackfillMu.Unlock()
	}

	var total store.LegacySessionMetricsBackfillResult
	var lastID uuid.UUID
	hasCursor := false
	totalRejected := 0
	batchNum := 0

	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		batchRes, fetched, nextLastID, rejectedCount, err := s.backfillLegacySessionMetricsOneBatch(
			ctx, hubID, pageSize, lastID, hasCursor, mutateBatchInput,
		)
		if err != nil {
			return total, err
		}
		total.Processed += batchRes.Processed
		total.Converted += batchRes.Converted
		total.Skipped += batchRes.Skipped
		totalRejected += rejectedCount
		batchNum++

		if fetched > 0 {
			lastID = nextLastID
			hasCursor = true
		}

		if afterBatchHook != nil && fetched > 0 {
			if err := afterBatchHook(batchNum, total); err != nil {
				return total, err
			}
		}
		if fetched < pageSize {
			break
		}
		if batchYield > 0 {
			timer := time.NewTimer(batchYield)
			select {
			case <-ctx.Done():
				timer.Stop()
				return total, ctx.Err()
			case <-timer.C:
			}
		}
	}
	if totalRejected > 0 {
		return total, fmt.Errorf("legacy session_metrics backfill completed pass with %d rejected row(s)", totalRejected)
	}
	return total, nil
}

func (s *UsageLedgerStore) backfillLegacySessionMetricsOneBatch(
	ctx context.Context,
	hubID string,
	pageSize int,
	afterID uuid.UUID,
	hasCursor bool,
	mutateBatchInput func(in *store.UsageIngestBatchInput),
) (store.LegacySessionMetricsBackfillResult, int, uuid.UUID, int, error) {
	var batchRes store.LegacySessionMetricsBackfillResult
	var fetched, rejectedCount int
	var lastID uuid.UUID

	run := func(client *ent.Client) error {
		drv := client.Driver()
		if drv.Dialect() == dialect.Postgres {
			var res sql.Result
			if err := drv.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext('legacy_session_metrics_backfill'))", []any{}, &res); err != nil {
				return mapError(err)
			}
		}

		q := client.AgentSessionMetrics.Query().
			Where(entasm.ConvertedAtIsNil()).
			Order(ent.Asc(entasm.FieldID)).
			Limit(pageSize)
		if hasCursor {
			q = q.Where(entasm.IDGT(afterID))
		}
		entities, err := q.All(ctx)
		if err != nil {
			return mapError(err)
		}
		fetched = len(entities)
		if fetched == 0 {
			return nil
		}
		lastID = entities[fetched-1].ID

		receivedAt := time.Now().UTC().Truncate(time.Microsecond)
		attrCache := make(map[string]*store.RunAttribution, len(entities))
		events := make([]*store.UsageEvent, 0, len(entities))
		marks := make([]store.SessionMetricsConversionMark, 0, len(entities))

		for _, e := range entities {
			row := entAgentSessionMetricsToStore(e)
			ev, err := s.prepareLegacyUsageEventTx(ctx, client, hubID, row, "", receivedAt, false, attrCache)
			if err != nil {
				return err
			}
			mark := store.SessionMetricsConversionMark{ID: row.ID}
			if ev != nil {
				mark.EventID = ev.EventID
				events = append(events, ev)
			} else {
				batchRes.Skipped++
			}
			marks = append(marks, mark)
		}

		in := &store.UsageIngestBatchInput{
			Events:                      events,
			IsBackfill:                  true,
			MarkSessionMetricsConverted: marks,
		}
		if mutateBatchInput != nil {
			mutateBatchInput(in)
		}
		ingestRes, _, err := s.ingestUsageBatchTx(ctx, client, in)
		if err != nil {
			return err
		}
		rejectedCount = len(ingestRes.Rejected)
		stamped := len(marks) - rejectedCount
		if stamped < 0 {
			stamped = 0
		}
		batchRes.Processed = stamped
		batchRes.Converted = ingestRes.Accepted
		batchRes.Skipped += ingestRes.Duplicates
		return nil
	}

	if s.inTx {
		if err := run(s.client); err != nil {
			return batchRes, 0, uuid.Nil, 0, err
		}
		return batchRes, fetched, lastID, rejectedCount, nil
	}

	tx, err := s.client.Tx(ctx)
	if err != nil {
		return batchRes, 0, uuid.Nil, 0, mapError(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := run(tx.Client()); err != nil {
		return batchRes, 0, uuid.Nil, 0, err
	}
	if err := tx.Commit(); err != nil {
		return batchRes, 0, uuid.Nil, 0, mapError(err)
	}
	return batchRes, fetched, lastID, rejectedCount, nil
}

// prepareLegacyUsageEventTx checks for Track 1 overlap, resolves or ensures a
// run_attribution row, and constructs the canonical store.UsageEvent for one
// agent_session_metrics segment. Returns (nil, nil) when the row overlaps
// existing non-legacy Track 1 events and must be skipped without double counting.
func (s *UsageLedgerStore) prepareLegacyUsageEventTx(
	ctx context.Context,
	client *ent.Client,
	hubID string,
	row *store.AgentSessionMetrics,
	preferredRunID string,
	receivedAt time.Time,
	clampFuture bool,
	attrCache map[string]*store.RunAttribution,
) (*store.UsageEvent, error) {
	overlap, err := hasTrack1OverlapTx(ctx, client, row)
	if err != nil {
		return nil, err
	}
	if overlap {
		return nil, nil
	}

	attr, err := s.ensureLegacyRunAttributionTx(ctx, client, hubID, row, preferredRunID, attrCache)
	if err != nil {
		return nil, err
	}

	return buildLegacyUsageEvent(hubID, row, attr, receivedAt, clampFuture), nil
}

// hasTrack1OverlapTx reports whether non-legacy Track 1 usage_events already
// exist for this agent matching either:
//  1. the segment's session_id_hash, or
//  2. an event_ts falling inside the segment's [started_at, effectiveEnd] window.
//
// A run-level check is intentionally NOT used: if a sink is revoked mid-run
// (e.g. experiment toggled OFF then ON), subsequent unmarked sessions in the
// same run carry a new session_id and later window and must be converted (R1).
func hasTrack1OverlapTx(ctx context.Context, client *ent.Client, row *store.AgentSessionMetrics) (bool, error) {
	// 1. Exact session_id_hash match against any non-legacy Track 1 event for
	// this agent.
	if row.SessionID != "" {
		sum := sha256.Sum256([]byte(row.SessionID))
		sessHash := hex.EncodeToString(sum[:])
		exists, err := client.UsageEvent.Query().
			Where(
				usageevent.AgentIDEQ(row.AgentID),
				usageevent.SessionIDHashEQ(sessHash),
				usageevent.UsageSourceNEQ(store.UsageSourceLegacySessionMetrics),
			).
			Exist(ctx)
		if err != nil {
			return false, mapError(err)
		}
		if exists {
			return true, nil
		}
	}

	// 2. Time-window overlap: if non-legacy Track 1 events for this agent fall
	// within [started_at, effectiveEnd], skip to avoid double counting
	// harnesses whose Track 1 events omit session_id.
	startTS := row.StartedAt.UTC().Truncate(time.Microsecond)
	endTS := startTS
	if row.EndedAt != nil && !row.EndedAt.IsZero() {
		endTS = row.EndedAt.UTC().Truncate(time.Microsecond)
	} else if !row.CreatedAt.IsZero() {
		endTS = row.CreatedAt.UTC().Truncate(time.Microsecond)
	}
	if endTS.Before(startTS) {
		endTS = startTS
	}
	exists, err := client.UsageEvent.Query().
		Where(
			usageevent.AgentIDEQ(row.AgentID),
			usageevent.UsageSourceNEQ(store.UsageSourceLegacySessionMetrics),
			usageevent.EventTsGTE(startTS),
			usageevent.EventTsLTE(endTS),
		).
		Exist(ctx)
	if err != nil {
		return false, mapError(err)
	}
	return exists, nil
}

// ensureLegacyRunAttributionTx resolves or lazily creates the run_attribution
// row required by the usage_events.run_id FK.
//
// Live path (preferredRunID != ""): uses existing run_attribution for
// preferredRunID if owned by row.AgentID, or lazily ensures it from the live
// agents row if agent.RunID == preferredRunID; falls back to synthetic
// "legacy-agent:<agent_id>" if no run_id exists.
//
// Catch-up/backfill path (preferredRunID == ""): when the agent is live and
// has a current run_id, uses agents.run_id_updated_at as the run start
// boundary (falling back to existing run_attribution.created_at only when
// run_id_updated_at is NULL). If row.StartedAt >= boundary, lazy-ensures the
// current run's run_attribution (via INSERT ... ON CONFLICT DO NOTHING) and
// attributes the segment to that real run (R1/O4). Otherwise uses synthetic
// "legacy-backfill:<agent_id>" built from the agents row as it exists at
// conversion time (or minimal project_id/agent_id when the agent was
// hard-deleted).
func (s *UsageLedgerStore) ensureLegacyRunAttributionTx(
	ctx context.Context,
	client *ent.Client,
	hubID string,
	row *store.AgentSessionMetrics,
	preferredRunID string,
	attrCache map[string]*store.RunAttribution,
) (*store.RunAttribution, error) {
	if preferredRunID != "" {
		if cached, ok := attrCache[preferredRunID]; ok && cached.AgentID == row.AgentID {
			return cached, nil
		}
		existing, err := client.RunAttribution.Query().
			Where(runattribution.IDEQ(preferredRunID)).
			Only(ctx)
		if err == nil && existing.AgentID == row.AgentID {
			attr := entRunAttributionToStore(existing)
			attrCache[preferredRunID] = attr
			return attr, nil
		}
		if err != nil && !ent.IsNotFound(err) {
			return nil, mapError(err)
		}
		// Lazy ensure for live agent's current run_id (same rule as P1 ingest).
		if uid, parseErr := uuid.Parse(row.AgentID); parseErr == nil {
			agRow, qerr := client.Agent.Query().
				Where(
					agent.IDEQ(uid),
					agent.DeletedAtIsNil(),
					agent.RunIDEQ(preferredRunID),
				).
				Select(runAttributionAgentColumns...).
				Only(ctx)
			if qerr == nil {
				attr, berr := buildRunAttributionFromAgentRow(ctx, client, agRow, preferredRunID, "", hubID, "", nil)
				if berr != nil {
					return nil, berr
				}
				if _, ierr := insertRunAttributionOnConflictDoNothing(ctx, client, attr); ierr != nil {
					return nil, ierr
				}
				stored, rerr := client.RunAttribution.Query().
					Where(runattribution.IDEQ(preferredRunID)).
					Only(ctx)
				if rerr != nil {
					return nil, mapError(rerr)
				}
				if stored.AgentID == row.AgentID {
					res := entRunAttributionToStore(stored)
					attrCache[preferredRunID] = res
					return res, nil
				}
			} else if !ent.IsNotFound(qerr) {
				return nil, mapError(qerr)
			}
		}
	} else if uid, parseErr := uuid.Parse(row.AgentID); parseErr == nil {
		// Catch-up path (R1): use agents.run_id_updated_at as the current-run
		// start boundary (falling back to existing run_attribution.created_at
		// if run_id_updated_at is NULL), and lazy-ensure run_attribution for
		// the current run so segments posted while the experiment was OFF land
		// on the current run regardless of backfill ordering.
		agRow, qerr := client.Agent.Query().
			Where(
				agent.IDEQ(uid),
				agent.DeletedAtIsNil(),
				agent.RunIDNEQ(""),
			).
			Select(runAttributionAgentColumnsWithRunIDUpdatedAt...).
			Only(ctx)
		if qerr == nil && agRow.RunID != "" {
			var existingAttr *store.RunAttribution
			if cached, ok := attrCache[agRow.RunID]; ok {
				existingAttr = cached
			} else {
				ra, rerr := client.RunAttribution.Query().
					Where(runattribution.IDEQ(agRow.RunID)).
					Only(ctx)
				if rerr == nil {
					existingAttr = entRunAttributionToStore(ra)
					attrCache[agRow.RunID] = existingAttr
				} else if !ent.IsNotFound(rerr) {
					return nil, mapError(rerr)
				}
			}

			var boundary time.Time
			hasBoundary := false
			if agRow.RunIDUpdatedAt != nil && !agRow.RunIDUpdatedAt.IsZero() {
				boundary = agRow.RunIDUpdatedAt.UTC().Truncate(time.Microsecond)
				hasBoundary = true
			} else if existingAttr != nil && existingAttr.AgentID == row.AgentID {
				boundary = existingAttr.CreatedAt.UTC().Truncate(time.Microsecond)
				hasBoundary = true
			}

			rowStart := row.StartedAt.UTC().Truncate(time.Microsecond)
			if hasBoundary && !rowStart.Before(boundary) {
				if existingAttr != nil {
					if existingAttr.AgentID == row.AgentID {
						return existingAttr, nil
					}
				} else {
					built, berr := buildRunAttributionFromAgentRow(ctx, client, agRow, agRow.RunID, "", hubID, "", nil)
					if berr != nil {
						return nil, berr
					}
					if _, ierr := insertRunAttributionOnConflictDoNothing(ctx, client, built); ierr != nil {
						return nil, ierr
					}
					stored, rerr := client.RunAttribution.Query().
						Where(runattribution.IDEQ(agRow.RunID)).
						Only(ctx)
					if rerr != nil {
						return nil, mapError(rerr)
					}
					if stored.AgentID == row.AgentID {
						res := entRunAttributionToStore(stored)
						attrCache[agRow.RunID] = res
						return res, nil
					}
				}
			}
		} else if qerr != nil && !ent.IsNotFound(qerr) {
			return nil, mapError(qerr)
		}
	}

	syntheticRunID := "legacy-backfill:" + row.AgentID
	if preferredRunID != "" {
		syntheticRunID = "legacy-agent:" + row.AgentID
	}
	if cached, ok := attrCache[syntheticRunID]; ok {
		return cached, nil
	}

	existing, err := client.RunAttribution.Query().
		Where(runattribution.IDEQ(syntheticRunID)).
		Only(ctx)
	if err == nil {
		attr := entRunAttributionToStore(existing)
		attrCache[syntheticRunID] = attr
		return attr, nil
	}
	if !ent.IsNotFound(err) {
		return nil, mapError(err)
	}

	var attr *store.RunAttribution
	if uid, parseErr := uuid.Parse(row.AgentID); parseErr == nil {
		agRow, qerr := client.Agent.Query().
			Where(agent.IDEQ(uid)).
			Select(runAttributionAgentColumns...).
			Only(ctx)
		if qerr == nil {
			attr, err = buildRunAttributionFromAgentRow(ctx, client, agRow, syntheticRunID, "", hubID, "", nil)
			if err != nil {
				return nil, err
			}
		} else if !ent.IsNotFound(qerr) {
			return nil, mapError(qerr)
		}
	}
	if attr == nil {
		// Agent was hard-deleted: preserve minimal project/agent attribution.
		attr = &store.RunAttribution{
			RunID:      syntheticRunID,
			Generation: 1,
			HubID:      hubID,
			ProjectID:  row.ProjectID,
			AgentID:    row.AgentID,
			CreatedAt:  row.StartedAt.UTC().Truncate(time.Microsecond),
		}
	}
	if _, err := insertRunAttributionOnConflictDoNothing(ctx, client, attr); err != nil {
		return nil, err
	}
	stored, err := client.RunAttribution.Query().
		Where(runattribution.IDEQ(syntheticRunID)).
		Only(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || ent.IsNotFound(err) {
			return attr, nil
		}
		return nil, mapError(err)
	}
	res := entRunAttributionToStore(stored)
	attrCache[syntheticRunID] = res
	return res, nil
}

// buildLegacyUsageEvent maps one agent_session_metrics segment to a canonical
// Track 1 store.UsageEvent following §3.1 (unknown = NULL, never 0):
//   - cache_write and cache_write_1h are always nil;
//   - input, output, cache_read (tokens_cached), reasoning are non-nil only
//     when in (0, MaxSafeInteger]; values > MaxSafeInteger become NULL and are
//     recorded in malformed_fields with token_status = malformed (O-b);
//   - calls is 0 (usage_events.calls is NOT NULL; agent_session_metrics does
//     not record api_calls).
func buildLegacyUsageEvent(
	hubID string,
	row *store.AgentSessionMetrics,
	attr *store.RunAttribution,
	receivedAt time.Time,
	clampFuture bool,
) *store.UsageEvent {
	eventTS := row.StartedAt
	if row.EndedAt != nil && !row.EndedAt.IsZero() {
		eventTS = *row.EndedAt
	}
	eventTS = eventTS.UTC().Truncate(time.Microsecond)
	if clampFuture && eventTS.After(receivedAt.Add(telemetrycontract.UsageIngestMaxFutureSkew)) {
		eventTS = receivedAt
	}

	var malformedFields []string
	boundToken := func(fieldName string, raw int64) *int64 {
		if raw <= 0 {
			return nil
		}
		if raw > telemetrycontract.MaxSafeInteger {
			malformedFields = append(malformedFields, fieldName)
			return nil
		}
		v := raw
		return &v
	}

	input := boundToken("input", row.TokensInput)
	output := boundToken("output", row.TokensOutput)
	cacheRead := boundToken("cache_read", row.TokensCached)
	reasoning := boundToken("reasoning", row.TokensReasoning)

	tokenStatus := telemetrycontract.TokenStatusNotReported
	if len(malformedFields) > 0 {
		tokenStatus = telemetrycontract.TokenStatusMalformed
	} else if input != nil || output != nil || cacheRead != nil || reasoning != nil {
		// cache_write is never measured by legacy agent_session_metrics.
		tokenStatus = telemetrycontract.TokenStatusPartial
	}

	// Anomalous spike guard (§4.1 item 3): > 50,000,000 summable tokens.
	var summable int64
	overflow := false
	for _, p := range []*int64{input, output, cacheRead} {
		if p == nil || *p <= 0 {
			continue
		}
		if summable > (1<<63-1)-*p {
			overflow = true
			break
		}
		summable += *p
	}
	anomalous := overflow || summable > telemetrycontract.UsageIngestAnomalousTokenThreshold

	model := telemetrycontract.ResolveModelLabel(row.Model, "")
	modelSource := telemetrycontract.ModelSourceUnknown
	if strings.TrimSpace(row.Model) != "" {
		modelSource = telemetrycontract.ModelSourceEvent
	}

	status := telemetrycontract.StatusSuccess
	if strings.EqualFold(strings.TrimSpace(row.Status), "error") {
		status = telemetrycontract.StatusError
	}

	var sessionIDHash *string
	if row.SessionID != "" {
		sum := sha256.Sum256([]byte(row.SessionID))
		h := hex.EncodeToString(sum[:])
		sessionIDHash = &h
	}

	var billingUserID *string
	if attr.BillingUserID != nil {
		cp := *attr.BillingUserID
		billingUserID = &cp
	}

	return &store.UsageEvent{
		HubID:           hubID,
		EventID:         store.LegacySessionMetricsEventID(row.AgentID, row.SessionID, row.StartedAt),
		RunID:           attr.RunID,
		ProjectID:       attr.ProjectID,
		AgentID:         attr.AgentID,
		BillingUserID:   billingUserID,
		BrokerID:        attr.BrokerID,
		Harness:         attr.Harness,
		Model:           model,
		ModelSource:     modelSource,
		UsageSource:     store.UsageSourceLegacySessionMetrics,
		SourceKind:      legacySessionMetricsSourceKind,
		SourceIdentity:  telemetrycontract.SourceIdentityComposite,
		SessionIDHash:   sessionIDHash,
		Status:          status,
		Calls:           0,
		Input:           input,
		Output:          output,
		CacheRead:       cacheRead,
		CacheWrite:      nil,
		CacheWrite1h:    nil,
		Reasoning:       reasoning,
		TokenStatus:     tokenStatus,
		MalformedFields: malformedFields,
		EventTS:         eventTS,
		ReceivedAt:      receivedAt,
		ProducerEpoch:   legacySessionMetricsProducerEpoch,
		ProducerSeq:     0,
		Anomalous:       anomalous,
	}
}
