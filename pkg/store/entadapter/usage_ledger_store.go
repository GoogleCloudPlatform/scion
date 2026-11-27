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
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/meterhealth"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/runattribution"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/user"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// defaultRunAttributionBackfillPageSize bounds each paged batch in
// BackfillActiveRunAttributions so large fleets do not load all agents into
// memory at once.
const defaultRunAttributionBackfillPageSize = 200

// runAttributionConfigSlot holds the active *store.RunAttributionConfig
// atomically so the Hub can toggle tokenomics.usage_ledger at runtime without
// restarting or locking the run-ID hot path. Shared with transactional copies
// of AgentStore via CompositeStore.newTxStore.
type runAttributionConfigSlot struct {
	v atomic.Pointer[store.RunAttributionConfig]
}

func newRunAttributionConfigSlot() *runAttributionConfigSlot {
	return &runAttributionConfigSlot{}
}

func (slot *runAttributionConfigSlot) load() *store.RunAttributionConfig {
	if slot == nil {
		return nil
	}
	return slot.v.Load()
}

func (slot *runAttributionConfigSlot) store(cfg *store.RunAttributionConfig) {
	if slot == nil {
		return
	}
	if cfg == nil {
		slot.v.Store(nil)
		return
	}
	cp := *cfg
	slot.v.Store(&cp)
}

// SetRunAttributionConfig installs (non-nil) or clears (nil) the atomic
// run_attribution writer configuration on AgentStore.
func (s *AgentStore) SetRunAttributionConfig(cfg *store.RunAttributionConfig) {
	s.runAttribution.store(cfg)
}

// RunAttributionConfigInstalled reports whether atomic run_attribution writes
// are currently active on AgentStore.
func (s *AgentStore) RunAttributionConfigInstalled() bool {
	return s.runAttribution.load() != nil
}

// UsageLedgerStore implements store.UsageLedgerStore using the Ent ORM.
type UsageLedgerStore struct {
	client                 *ent.Client
	inTx                   bool
	hourlyCoverageRecorded *atomic.Bool
	legacyBackfillMu       *sync.Mutex
}

// NewUsageLedgerStore creates a new Ent-backed UsageLedgerStore.
func NewUsageLedgerStore(client *ent.Client) *UsageLedgerStore {
	return &UsageLedgerStore{
		client:                 client,
		hourlyCoverageRecorded: &atomic.Bool{},
		legacyBackfillMu:       &sync.Mutex{},
	}
}

var _ store.UsageLedgerStore = (*UsageLedgerStore)(nil)
var _ store.UsageLedgerStore = (*CompositeStore)(nil)

func (s *UsageLedgerStore) usesRowLocks() bool {
	return s.client.Driver().Dialect() == dialect.Postgres
}

// EnsureRunAttribution implements store.UsageLedgerStore.EnsureRunAttribution.
// Returns created=true iff the INSERT ... ON CONFLICT (run_id) DO NOTHING
// statement actually inserted a new row (based on RowsAffected).
func (s *UsageLedgerStore) EnsureRunAttribution(ctx context.Context, attr *store.RunAttribution) (bool, error) {
	if attr == nil || attr.RunID == "" || attr.ProjectID == "" || attr.AgentID == "" {
		return false, fmt.Errorf("%w: run_id, project_id, and agent_id are required", store.ErrInvalidInput)
	}
	return insertRunAttributionOnConflictDoNothing(ctx, s.client, attr)
}

func buildInsertRunAttributionQuery(dialectName string, attr *store.RunAttribution) (string, []any, error) {
	createdAt := attr.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	createdAt = createdAt.UTC().Truncate(time.Microsecond)

	var labelsVal any
	if len(attr.LabelsAtLaunch) > 0 {
		raw, err := json.Marshal(attr.LabelsAtLaunch)
		if err != nil {
			return "", nil, fmt.Errorf("marshal labels_at_launch: %w", err)
		}
		if dialectName == dialect.Postgres {
			labelsVal = entsql.ExprFunc(func(b *entsql.Builder) {
				b.Arg(string(raw)).WriteString("::jsonb")
			})
		} else {
			labelsVal = string(raw)
		}
	}

	query, args := entsql.Dialect(dialectName).
		Insert(runattribution.Table).
		Columns(
			runattribution.FieldID,
			runattribution.FieldLaunchID,
			runattribution.FieldGeneration,
			runattribution.FieldPriorRunID,
			runattribution.FieldHubID,
			runattribution.FieldProjectID,
			runattribution.FieldAgentID,
			runattribution.FieldAgentSlug,
			runattribution.FieldBrokerID,
			runattribution.FieldBillingUserID,
			runattribution.FieldOwnerID,
			runattribution.FieldCreatedBy,
			runattribution.FieldHarness,
			runattribution.FieldHarnessConfig,
			runattribution.FieldTemplate,
			runattribution.FieldHarnessConfigDigest,
			runattribution.FieldTemplateDigest,
			runattribution.FieldHarnessBuild,
			runattribution.FieldLabelsAtLaunch,
			runattribution.FieldMeteringWriterMode,
			runattribution.FieldUsageLiveSource,
			runattribution.FieldCapturePolicy,
			runattribution.FieldNoticeVersion,
			runattribution.FieldBackfillExisting,
			runattribution.FieldLegalHold,
			runattribution.FieldCreatedAt,
		).
		Values(
			attr.RunID,
			attr.LaunchID,
			attr.Generation,
			attr.PriorRunID,
			attr.HubID,
			attr.ProjectID,
			attr.AgentID,
			attr.AgentSlug,
			attr.BrokerID,
			attr.BillingUserID,
			attr.OwnerID,
			attr.CreatedBy,
			attr.Harness,
			attr.HarnessConfig,
			attr.Template,
			attr.HarnessConfigDigest,
			attr.TemplateDigest,
			attr.HarnessBuild,
			labelsVal,
			attr.MeteringWriterMode,
			attr.UsageLiveSource,
			attr.CapturePolicy,
			attr.NoticeVersion,
			attr.BackfillExisting,
			attr.LegalHold,
			createdAt,
		).
		OnConflict(
			entsql.ConflictColumns(runattribution.FieldID),
			entsql.DoNothing(),
		).
		Query()
	return query, args, nil
}

func insertRunAttributionOnConflictDoNothing(ctx context.Context, client *ent.Client, attr *store.RunAttribution) (bool, error) {
	drv := client.Driver()
	query, args, err := buildInsertRunAttributionQuery(drv.Dialect(), attr)
	if err != nil {
		return false, err
	}
	var res sql.Result
	if err := drv.Exec(ctx, query, args, &res); err != nil {
		return false, mapError(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, mapError(err)
	}
	return n > 0, nil
}

// GetRunAttribution implements store.UsageLedgerStore.GetRunAttribution.
func (s *UsageLedgerStore) GetRunAttribution(ctx context.Context, runID string) (*store.RunAttribution, error) {
	if runID == "" {
		return nil, store.ErrNotFound
	}
	row, err := s.client.RunAttribution.Query().
		Where(runattribution.IDEQ(runID)).
		Only(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return entRunAttributionToStore(row), nil
}

// ListRunAttributionsByAgent implements store.UsageLedgerStore.ListRunAttributionsByAgent.
func (s *UsageLedgerStore) ListRunAttributionsByAgent(ctx context.Context, agentID string, limit int) ([]*store.RunAttribution, error) {
	if agentID == "" {
		return nil, fmt.Errorf("%w: agent_id is required", store.ErrInvalidInput)
	}
	q := s.client.RunAttribution.Query().
		Where(runattribution.AgentIDEQ(agentID)).
		Order(ent.Desc(runattribution.FieldCreatedAt), ent.Desc(runattribution.FieldID))
	if limit > 0 {
		q = q.Limit(limit)
	}
	rows, err := q.All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]*store.RunAttribution, len(rows))
	for i, r := range rows {
		out[i] = entRunAttributionToStore(r)
	}
	return out, nil
}

// FillRunAttributionDigests implements store.UsageLedgerStore.FillRunAttributionDigests.
// Each non-empty field in upd is filled at most once (WHERE col IS NULL).
// An identical retry is a no-op; a conflicting value returns
// store.ErrRunAttributionDigestConflict and logs a warning without overwriting.
func (s *UsageLedgerStore) FillRunAttributionDigests(ctx context.Context, runID string, upd store.RunAttributionDigestUpdate) error {
	if runID == "" {
		return store.ErrNotFound
	}
	run := func(client *ent.Client) error {
		q := client.RunAttribution.Query().Where(runattribution.IDEQ(runID))
		if s.usesRowLocks() {
			q = q.ForUpdate()
		}
		row, err := q.Only(ctx)
		if err != nil {
			return mapError(err)
		}

		var conflictFields []string
		checkField := func(name string, existing, incoming *string) (shouldSet bool) {
			if incoming == nil || *incoming == "" {
				return false
			}
			if existing == nil || *existing == "" {
				return true
			}
			if *existing != *incoming {
				conflictFields = append(conflictFields, name)
			}
			return false
		}

		setHCD := checkField("harness_config_digest", row.HarnessConfigDigest, upd.HarnessConfigDigest)
		setTD := checkField("template_digest", row.TemplateDigest, upd.TemplateDigest)
		setHB := checkField("harness_build", row.HarnessBuild, upd.HarnessBuild)

		if len(conflictFields) > 0 {
			slog.Warn("run_attribution: quarantined conflicting resolved-config digest update",
				"run_id", runID, "fields", conflictFields)
			return fmt.Errorf("%w: run %s conflicting fields %v", store.ErrRunAttributionDigestConflict, runID, conflictFields)
		}
		if !setHCD && !setTD && !setHB {
			return nil
		}

		u := client.RunAttribution.Update().Where(runattribution.IDEQ(runID))
		if setHCD {
			u = u.Where(runattribution.HarnessConfigDigestIsNil()).SetHarnessConfigDigest(*upd.HarnessConfigDigest)
		}
		if setTD {
			u = u.Where(runattribution.TemplateDigestIsNil()).SetTemplateDigest(*upd.TemplateDigest)
		}
		if setHB {
			u = u.Where(runattribution.HarnessBuildIsNil()).SetHarnessBuild(*upd.HarnessBuild)
		}
		if _, err := u.Save(ctx); err != nil {
			return mapError(err)
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

// ResolveBillingUserID implements store.UsageLedgerStore.ResolveBillingUserID.
func (s *UsageLedgerStore) ResolveBillingUserID(ctx context.Context, ownerID, createdBy string, ancestry []string) (*string, error) {
	return resolveBillingUserIDTx(ctx, s.client, ownerID, createdBy, ancestry)
}

// resolveBillingUserIDTx resolves billing_user_id in priority order
// (Tokenomics design §3.2):
//  1. ownerID if it resolves to a User row;
//  2. first element of ancestry (from index 0) that resolves to a User row;
//  3. createdBy if it resolves to a User row;
//  4. nil (SQL NULL) for principal-less agents.
func resolveBillingUserIDTx(ctx context.Context, client *ent.Client, ownerID, createdBy string, ancestry []string) (*string, error) {
	// Collect ordered unique candidate UUIDs so we can check all of them in a
	// single SELECT id FROM users WHERE id IN (...).
	ordered := make([]uuid.UUID, 0, 2+len(ancestry))
	seen := make(map[uuid.UUID]struct{}, 2+len(ancestry))
	addCandidate := func(raw string) {
		if raw == "" {
			return
		}
		uid, err := uuid.Parse(raw)
		if err != nil {
			return
		}
		if _, dup := seen[uid]; dup {
			return
		}
		seen[uid] = struct{}{}
		ordered = append(ordered, uid)
	}

	addCandidate(ownerID)
	for _, a := range ancestry {
		addCandidate(a)
	}
	addCandidate(createdBy)

	if len(ordered) == 0 {
		return nil, nil
	}

	foundIDs, err := client.User.Query().
		Where(user.IDIn(ordered...)).
		IDs(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	if len(foundIDs) == 0 {
		return nil, nil
	}
	foundSet := make(map[uuid.UUID]struct{}, len(foundIDs))
	for _, id := range foundIDs {
		foundSet[id] = struct{}{}
	}
	for _, cand := range ordered {
		if _, ok := foundSet[cand]; ok {
			res := cand.String()
			return &res, nil
		}
	}
	return nil, nil
}

// buildRunAttributionFromAgentRow constructs the immutable run_attribution
// snapshot for runID from an Ent Agent row inside the caller's transaction.
// overrideLaunchID, when non-empty (e.g. from SettleLaunchedRun's fence),
// takes precedence over the row's launch_id match.
func buildRunAttributionFromAgentRow(ctx context.Context, client *ent.Client, row *ent.Agent, runID, priorRunID, hubID, overrideLaunchID string) (*store.RunAttribution, error) {
	var ownerIDStr, createdByStr string
	var ownerIDPtr, createdByPtr *string
	if row.OwnerID != nil {
		ownerIDStr = row.OwnerID.String()
		ownerIDPtr = &ownerIDStr
	}
	if row.CreatedBy != nil {
		createdByStr = row.CreatedBy.String()
		createdByPtr = &createdByStr
	}

	billingUserID, err := resolveBillingUserIDTx(ctx, client, ownerIDStr, createdByStr, row.Ancestry)
	if err != nil {
		return nil, err
	}

	harnessConfig := row.HarnessConfig
	var appliedCfg *store.AgentAppliedConfig
	if row.AppliedConfig != "" {
		if cfg, parseErr := parseAppliedConfig(row.AppliedConfig); parseErr == nil && cfg != nil {
			appliedCfg = cfg
			if harnessConfig == "" {
				harnessConfig = cfg.HarnessConfig
			}
		}
	}
	var effectiveCfg *store.AgentEffectiveConfig
	if row.EffectiveConfig != nil && *row.EffectiveConfig != "" {
		effectiveCfg = parseEffectiveConfig(row.ID.String(), row.EffectiveConfig)
	}
	harness := store.ResolveRunAttributionHarness(appliedCfg, effectiveCfg)

	launchID := overrideLaunchID
	if launchID == "" && row.LaunchRunID == runID && row.LaunchID != "" {
		launchID = row.LaunchID
	}

	gen := row.Generation
	if gen < 1 {
		gen = 1
	}

	return &store.RunAttribution{
		RunID:         runID,
		LaunchID:      launchID,
		Generation:    gen,
		PriorRunID:    priorRunID,
		HubID:         hubID,
		ProjectID:     row.ProjectID.String(),
		AgentID:       row.ID.String(),
		AgentSlug:     row.Slug,
		BrokerID:      row.RuntimeBrokerID,
		BillingUserID: billingUserID,
		OwnerID:       ownerIDPtr,
		CreatedBy:     createdByPtr,
		Harness:       harness,
		HarnessConfig: harnessConfig,
		Template:      row.Template,
		LabelsAtLaunch: func() map[string]string {
			if len(row.Labels) == 0 {
				return nil
			}
			cp := make(map[string]string, len(row.Labels))
			for k, v := range row.Labels {
				cp[k] = v
			}
			return cp
		}(),
		CreatedAt: time.Now().UTC().Truncate(time.Microsecond),
	}, nil
}

// ensureRunAttributionForAgentTx reads the agent row in client's transaction
// and inserts an immutable run_attribution row for runID via ON CONFLICT DO NOTHING.
func ensureRunAttributionForAgentTx(ctx context.Context, client *ent.Client, uid uuid.UUID, runID, priorRunID, hubID, overrideLaunchID string, beforeInsert func(ctx context.Context, runID string) error) error {
	if runID == "" {
		return nil
	}
	row, err := client.Agent.Query().
		Where(agent.IDEQ(uid)).
		Select(runAttributionAgentColumns...).
		Only(ctx)
	if err != nil {
		return mapError(err)
	}
	attr, err := buildRunAttributionFromAgentRow(ctx, client, row, runID, priorRunID, hubID, overrideLaunchID)
	if err != nil {
		return err
	}
	if beforeInsert != nil {
		if err := beforeInsert(ctx, runID); err != nil {
			return err
		}
	}
	_, err = insertRunAttributionOnConflictDoNothing(ctx, client, attr)
	return err
}

// runAttributionAgentColumns is the narrow column projection needed to build a
// run_attribution snapshot from an agents row.
var runAttributionAgentColumns = []string{
	agent.FieldID,
	agent.FieldProjectID,
	agent.FieldSlug,
	agent.FieldRunID,
	agent.FieldLaunchID,
	agent.FieldLaunchRunID,
	agent.FieldGeneration,
	agent.FieldRuntimeBrokerID,
	agent.FieldOwnerID,
	agent.FieldCreatedBy,
	agent.FieldAncestry,
	agent.FieldHarnessConfig,
	agent.FieldTemplate,
	agent.FieldLabels,
	agent.FieldAppliedConfig,
	agent.FieldEffectiveConfig,
}

// fillRunAttributionLaunchIDTx fills launch_id once on run_attribution when
// BeginLaunch / BeginStartLaunch binds launch_run_id to a minted run.
func fillRunAttributionLaunchIDTx(ctx context.Context, client *ent.Client, runID, launchID string) error {
	if runID == "" || launchID == "" {
		return nil
	}
	_, err := client.RunAttribution.Update().
		Where(runattribution.IDEQ(runID), runattribution.LaunchIDEQ("")).
		SetLaunchID(launchID).
		Save(ctx)
	return mapError(err)
}

// runAttributionNotExists filters agents to those whose current run_id does
// not yet have a row in run_attribution (dialect-uniform SQLite & Postgres).
func runAttributionNotExists(sel *entsql.Selector) {
	ra := entsql.Table(runattribution.Table)
	sub := entsql.Dialect(sel.Dialect()).
		Select(ra.C(runattribution.FieldID)).
		From(ra).
		Where(entsql.ColumnsEQ(ra.C(runattribution.FieldID), sel.C(agent.FieldRunID)))
	sel.Where(entsql.NotExists(sub))
}

// BackfillActiveRunAttributions implements store.UsageLedgerStore.BackfillActiveRunAttributions.
// Pages through non-deleted agents with a non-empty run_id that have no
// run_attribution row yet (NOT EXISTS filter, narrow column projection),
// inserting missing rows via INSERT ... ON CONFLICT (run_id) DO NOTHING.
// Continues past per-agent errors (logging each and returning an aggregated
// error at the end so the caller retries on the next reconcile).
func (s *UsageLedgerStore) BackfillActiveRunAttributions(ctx context.Context, hubID string) (int, error) {
	return s.backfillActiveRunAttributionsPaged(ctx, hubID, defaultRunAttributionBackfillPageSize)
}

func (s *UsageLedgerStore) backfillActiveRunAttributionsPaged(ctx context.Context, hubID string, pageSize int) (int, error) {
	if pageSize <= 0 {
		pageSize = defaultRunAttributionBackfillPageSize
	}
	inserted := 0
	var lastID uuid.UUID
	hasCursor := false
	var rowErrs []error

	for {
		if err := ctx.Err(); err != nil {
			return inserted, err
		}
		q := s.client.Agent.Query().
			Where(
				agent.DeletedAtIsNil(),
				agent.RunIDNEQ(""),
				runAttributionNotExists,
			).
			Select(runAttributionAgentColumns...).
			Order(ent.Asc(agent.FieldID)).
			Limit(pageSize)
		if hasCursor {
			q = q.Where(agent.IDGT(lastID))
		}
		rows, err := q.All(ctx)
		if err != nil {
			return inserted, mapError(err)
		}
		if len(rows) == 0 {
			break
		}
		for _, row := range rows {
			lastID = row.ID
			hasCursor = true
			if err := ctx.Err(); err != nil {
				return inserted, err
			}
			attr, err := buildRunAttributionFromAgentRow(ctx, s.client, row, row.RunID, "", hubID, "")
			if err != nil {
				slog.Warn("run_attribution backfill: failed to build attribution for agent",
					"agent_id", row.ID.String(), "run_id", row.RunID, "error", err)
				rowErrs = append(rowErrs, fmt.Errorf("agent %s run %s: %w", row.ID, row.RunID, err))
				continue
			}
			created, err := insertRunAttributionOnConflictDoNothing(ctx, s.client, attr)
			if err != nil {
				slog.Warn("run_attribution backfill: failed to insert attribution for agent",
					"agent_id", row.ID.String(), "run_id", row.RunID, "error", err)
				rowErrs = append(rowErrs, fmt.Errorf("agent %s run %s: %w", row.ID, row.RunID, err))
				continue
			}
			if created {
				inserted++
			}
		}
		if len(rows) < pageSize {
			break
		}
	}
	return inserted, errors.Join(rowErrs...)
}

// upsertMeterHealthTx performs a race-safe monotonic upsert of meter_health
// inside IngestUsageBatch's transaction. The Hub is the sole authority on
// sequence gaps and contiguity: (existingMax, maxSeq] minus
// (receivedSeqs ∪ rejectedSeqs) is added to missing_ranges, rejectedSeqs are
// tracked separately in rejected_ranges/rejected_count so Hub-rejected events
// are never misclassified as lost data gaps, and last_contiguous_seq advances
// strictly through accounted (received or rejected) sequences.
//
// Returns the merged (supportedSignals, schemaDriftDetected) state for this
// (agent_id, run_id, producer_epoch) so scope-counter accounting in the same
// transaction can evaluate companion token-metering state without an extra query.
func (s *UsageLedgerStore) upsertMeterHealthTx(
	ctx context.Context,
	client *ent.Client,
	mh *store.MeterHealth,
	anomalousDelta int64,
	receivedSeqs []int64,
	rejectedSeqs []int64,
) ([]string, bool, error) {
	lastReceipt := mh.LastReceiptAt
	if lastReceipt.IsZero() {
		lastReceipt = time.Now()
	}
	lastReceipt = lastReceipt.UTC().Truncate(time.Microsecond)
	var lastSourceActivity *time.Time
	if mh.LastSourceActivityAt != nil {
		t := mh.LastSourceActivityAt.UTC().Truncate(time.Microsecond)
		lastSourceActivity = &t
	}
	finalState := mh.FinalState
	if finalState == "" {
		finalState = store.MeterHealthFinalStateOpen
	}
	now := time.Now().UTC().Truncate(time.Microsecond)

	queryExisting := func() (*ent.MeterHealth, error) {
		q := client.MeterHealth.Query().Where(
			meterhealth.AgentIDEQ(mh.AgentID),
			meterhealth.RunIDEQ(mh.RunID),
			meterhealth.ProducerEpochEQ(mh.ProducerEpoch),
		)
		if s.usesRowLocks() {
			q = q.ForUpdate()
		}
		row, err := q.Only(ctx)
		if err != nil && !ent.IsNotFound(err) {
			return nil, mapError(err)
		}
		return row, nil
	}

	existing, err := queryExisting()
	if err != nil {
		return nil, false, err
	}

	if existing == nil {
		initContig, initMax, initMissing, initRejCount, initRejRanges, missingCoalesced, rejectedCoalesced := advanceIngestSeqState(
			0, 0, nil, 0, nil, mh.MaxSeqSeen, receivedSeqs, rejectedSeqs,
		)
		if missingCoalesced {
			slog.Warn("meter_health: missing_ranges exceeded cap; conservatively coalesced adjacent gaps",
				"agent_id", mh.AgentID, "run_id", mh.RunID, "producer_epoch", mh.ProducerEpoch,
				"cap", maxMeterHealthMissingRanges, "max_seq_seen", initMax)
		}
		if rejectedCoalesced {
			slog.Warn("meter_health: rejected_ranges exceeded cap; conservatively coalesced adjacent ranges",
				"agent_id", mh.AgentID, "run_id", mh.RunID, "producer_epoch", mh.ProducerEpoch,
				"cap", maxMeterHealthMissingRanges, "max_seq_seen", initMax)
		}
		newID := uuid.New()
		b := client.MeterHealth.Create().
			SetID(newID).
			SetAgentID(mh.AgentID).
			SetRunID(mh.RunID).
			SetProducerEpoch(mh.ProducerEpoch).
			SetLastReceiptAt(lastReceipt).
			SetLastContiguousSeq(initContig).
			SetMaxSeqSeen(initMax).
			SetRejectedCount(initRejCount).
			SetNillableLastSourceActivityAt(lastSourceActivity).
			SetAnomalousCount(anomalousDelta).
			SetSchemaDriftDetected(mh.SchemaDriftDetected).
			SetFinalState(finalState).
			SetUpdatedAt(now)
		if len(initMissing) > 0 {
			b = b.SetMissingRanges(initMissing)
		}
		if len(initRejRanges) > 0 {
			b = b.SetRejectedRanges(initRejRanges)
		}
		if len(mh.SupportedSignals) > 0 {
			b = b.SetSupportedSignals(mh.SupportedSignals)
		}
		if err := client.MeterHealth.CreateBulk(b).
			OnConflictColumns(meterhealth.FieldAgentID, meterhealth.FieldRunID, meterhealth.FieldProducerEpoch).
			DoNothing().
			Exec(ctx); err != nil {
			return nil, false, mapError(err)
		}

		// Check whether our insert won the first-insert race. If another
		// concurrent writer inserted first, row.ID != newID and we fall
		// through to the read-modify-write merge path so no sequence advance
		// or anomaly count is silently lost (P1.1 review O6).
		row, err := queryExisting()
		if err != nil {
			return nil, false, err
		}
		if row == nil || row.ID == newID {
			return mh.SupportedSignals, mh.SchemaDriftDetected, nil
		}
		existing = row
	}

	nextReceipt := existing.LastReceiptAt.UTC()
	if lastReceipt.After(nextReceipt) {
		nextReceipt = lastReceipt
	}
	nextContig, nextMax, nextMissing, nextRejCount, nextRejRanges, missingCoalesced, rejectedCoalesced := advanceIngestSeqState(
		existing.LastContiguousSeq, existing.MaxSeqSeen, existing.MissingRanges,
		existing.RejectedCount, existing.RejectedRanges,
		mh.MaxSeqSeen, receivedSeqs, rejectedSeqs,
	)
	if missingCoalesced {
		slog.Warn("meter_health: missing_ranges exceeded cap; conservatively coalesced adjacent gaps",
			"agent_id", mh.AgentID, "run_id", mh.RunID, "producer_epoch", mh.ProducerEpoch,
			"cap", maxMeterHealthMissingRanges, "max_seq_seen", nextMax)
	}
	if rejectedCoalesced {
		slog.Warn("meter_health: rejected_ranges exceeded cap; conservatively coalesced adjacent ranges",
			"agent_id", mh.AgentID, "run_id", mh.RunID, "producer_epoch", mh.ProducerEpoch,
			"cap", maxMeterHealthMissingRanges, "max_seq_seen", nextMax)
	}

	nextAnomalous := existing.AnomalousCount + anomalousDelta
	nextDrift := existing.SchemaDriftDetected || mh.SchemaDriftDetected
	nextFinal := existing.FinalState
	if nextFinal == store.MeterHealthFinalStateOpen &&
		(finalState == store.MeterHealthFinalStateFinal || finalState == store.MeterHealthFinalStateIncomplete) {
		nextFinal = finalState
	}

	effectiveSignals := existing.SupportedSignals
	if len(mh.SupportedSignals) > 0 {
		effectiveSignals = mh.SupportedSignals
	}

	u := client.MeterHealth.UpdateOneID(existing.ID).
		SetLastReceiptAt(nextReceipt).
		SetLastContiguousSeq(nextContig).
		SetMaxSeqSeen(nextMax).
		SetRejectedCount(nextRejCount).
		SetAnomalousCount(nextAnomalous).
		SetSchemaDriftDetected(nextDrift).
		SetFinalState(nextFinal).
		SetUpdatedAt(now)

	if lastSourceActivity != nil {
		if existing.LastSourceActivityAt == nil || lastSourceActivity.After(existing.LastSourceActivityAt.UTC()) {
			u = u.SetLastSourceActivityAt(*lastSourceActivity)
		}
	}
	if len(mh.SupportedSignals) > 0 {
		u = u.SetSupportedSignals(mh.SupportedSignals)
	}
	if nextMissing != nil {
		u = u.SetMissingRanges(nextMissing)
	} else if len(existing.MissingRanges) > 0 {
		u = u.SetMissingRanges([]store.SeqRange{})
	}
	if nextRejRanges != nil {
		u = u.SetRejectedRanges(nextRejRanges)
	}

	_, err = u.Save(ctx)
	if err != nil {
		return nil, false, mapError(err)
	}
	return effectiveSignals, nextDrift, nil
}

// GetMeterHealth implements store.UsageLedgerStore.GetMeterHealth.
func (s *UsageLedgerStore) GetMeterHealth(ctx context.Context, agentID, runID string) ([]*store.MeterHealth, error) {
	rows, err := s.client.MeterHealth.Query().
		Where(meterhealth.AgentIDEQ(agentID), meterhealth.RunIDEQ(runID)).
		Order(ent.Asc(meterhealth.FieldProducerEpoch)).
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

func entRunAttributionToStore(r *ent.RunAttribution) *store.RunAttribution {
	out := &store.RunAttribution{
		RunID:               r.ID,
		LaunchID:            r.LaunchID,
		Generation:          r.Generation,
		PriorRunID:          r.PriorRunID,
		HubID:               r.HubID,
		ProjectID:           r.ProjectID,
		AgentID:             r.AgentID,
		AgentSlug:           r.AgentSlug,
		BrokerID:            r.BrokerID,
		BillingUserID:       copyStringPtr(r.BillingUserID),
		OwnerID:             copyStringPtr(r.OwnerID),
		CreatedBy:           copyStringPtr(r.CreatedBy),
		Harness:             r.Harness,
		HarnessConfig:       r.HarnessConfig,
		Template:            r.Template,
		HarnessConfigDigest: copyStringPtr(r.HarnessConfigDigest),
		TemplateDigest:      copyStringPtr(r.TemplateDigest),
		HarnessBuild:        copyStringPtr(r.HarnessBuild),
		MeteringWriterMode:  r.MeteringWriterMode,
		UsageLiveSource:     r.UsageLiveSource,
		CapturePolicy:       r.CapturePolicy,
		NoticeVersion:       r.NoticeVersion,
		BackfillExisting:    r.BackfillExisting,
		LegalHold:           r.LegalHold,
		CreatedAt:           r.CreatedAt.UTC(),
	}
	if len(r.LabelsAtLaunch) > 0 {
		out.LabelsAtLaunch = make(map[string]string, len(r.LabelsAtLaunch))
		for k, v := range r.LabelsAtLaunch {
			out.LabelsAtLaunch[k] = v
		}
	}
	return out
}

func entMeterHealthToStore(r *ent.MeterHealth) *store.MeterHealth {
	out := &store.MeterHealth{
		ID:                   r.ID.String(),
		AgentID:              r.AgentID,
		RunID:                r.RunID,
		ProducerEpoch:        r.ProducerEpoch,
		LastReceiptAt:        r.LastReceiptAt.UTC(),
		LastContiguousSeq:    r.LastContiguousSeq,
		MaxSeqSeen:           r.MaxSeqSeen,
		MissingRanges:        append([]store.SeqRange(nil), r.MissingRanges...),
		RejectedCount:        r.RejectedCount,
		RejectedRanges:       append([]store.SeqRange(nil), r.RejectedRanges...),
		LastSourceActivityAt: copyTimePtr(r.LastSourceActivityAt),
		SupportedSignals:     append([]string(nil), r.SupportedSignals...),
		AnomalousCount:       r.AnomalousCount,
		SchemaDriftDetected:  r.SchemaDriftDetected,
		FinalState:           r.FinalState,
		UpdatedAt:            r.UpdatedAt.UTC(),
	}
	return out
}

func copyStringPtr(s *string) *string {
	if s == nil {
		return nil
	}
	cp := *s
	return &cp
}
