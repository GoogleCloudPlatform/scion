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
	"log/slog"
	"sync/atomic"
	"time"

	"entgo.io/ent/dialect"
	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/meterhealth"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/runattribution"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/user"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

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
	client *ent.Client
	inTx   bool
}

// NewUsageLedgerStore creates a new Ent-backed UsageLedgerStore.
func NewUsageLedgerStore(client *ent.Client) *UsageLedgerStore {
	return &UsageLedgerStore{client: client}
}

var _ store.UsageLedgerStore = (*UsageLedgerStore)(nil)
var _ store.UsageLedgerStore = (*CompositeStore)(nil)

func (s *UsageLedgerStore) usesRowLocks() bool {
	return s.client.Driver().Dialect() == dialect.Postgres
}

// EnsureRunAttribution implements store.UsageLedgerStore.EnsureRunAttribution.
func (s *UsageLedgerStore) EnsureRunAttribution(ctx context.Context, attr *store.RunAttribution) (bool, error) {
	if attr == nil || attr.RunID == "" || attr.ProjectID == "" || attr.AgentID == "" {
		return false, fmt.Errorf("%w: run_id, project_id, and agent_id are required", store.ErrInvalidInput)
	}
	existed, err := s.client.RunAttribution.Query().
		Where(runattribution.IDEQ(attr.RunID)).
		Exist(ctx)
	if err != nil {
		return false, mapError(err)
	}
	if existed {
		return false, nil
	}
	if err := insertRunAttributionOnConflictDoNothing(ctx, s.client, attr); err != nil {
		return false, err
	}
	return true, nil
}

func insertRunAttributionOnConflictDoNothing(ctx context.Context, client *ent.Client, attr *store.RunAttribution) error {
	createdAt := attr.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	createdAt = createdAt.UTC().Truncate(time.Microsecond)

	b := client.RunAttribution.Create().
		SetID(attr.RunID).
		SetLaunchID(attr.LaunchID).
		SetGeneration(attr.Generation).
		SetPriorRunID(attr.PriorRunID).
		SetHubID(attr.HubID).
		SetProjectID(attr.ProjectID).
		SetAgentID(attr.AgentID).
		SetAgentSlug(attr.AgentSlug).
		SetBrokerID(attr.BrokerID).
		SetNillableBillingUserID(attr.BillingUserID).
		SetNillableOwnerID(attr.OwnerID).
		SetNillableCreatedBy(attr.CreatedBy).
		SetHarness(attr.Harness).
		SetHarnessConfig(attr.HarnessConfig).
		SetTemplate(attr.Template).
		SetNillableHarnessConfigDigest(attr.HarnessConfigDigest).
		SetNillableTemplateDigest(attr.TemplateDigest).
		SetNillableHarnessBuild(attr.HarnessBuild).
		SetMeteringWriterMode(attr.MeteringWriterMode).
		SetUsageLiveSource(attr.UsageLiveSource).
		SetCapturePolicy(attr.CapturePolicy).
		SetNoticeVersion(attr.NoticeVersion).
		SetBackfillExisting(attr.BackfillExisting).
		SetLegalHold(attr.LegalHold).
		SetCreatedAt(createdAt)
	if len(attr.LabelsAtLaunch) > 0 {
		labels := make(map[string]string, len(attr.LabelsAtLaunch))
		for k, v := range attr.LabelsAtLaunch {
			labels[k] = v
		}
		b = b.SetLabelsAtLaunch(labels)
	}

	if err := client.RunAttribution.CreateBulk(b).
		OnConflictColumns(runattribution.FieldID).
		DoNothing().
		Exec(ctx); err != nil {
		return mapError(err)
	}
	return nil
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
func buildRunAttributionFromAgentRow(ctx context.Context, client *ent.Client, row *ent.Agent, runID, priorRunID, hubID string) (*store.RunAttribution, error) {
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
	var harness string
	if row.AppliedConfig != "" {
		if cfg, parseErr := parseAppliedConfig(row.AppliedConfig); parseErr == nil && cfg != nil {
			if harnessConfig == "" {
				harnessConfig = cfg.HarnessConfig
			}
			if cfg.Env != nil {
				if h := cfg.Env["SCION_HARNESS"]; h != "" {
					harness = h
				}
			}
		}
	}
	if harness == "" && row.EffectiveConfig != nil && *row.EffectiveConfig != "" {
		if eff := parseEffectiveConfig(row.ID.String(), row.EffectiveConfig); eff != nil && eff.Harness != "" {
			harness = eff.Harness
		}
	}
	if harness == "" {
		harness = harnessConfig
	}

	var launchID string
	if row.LaunchRunID == runID && row.LaunchID != "" {
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
func ensureRunAttributionForAgentTx(ctx context.Context, client *ent.Client, uid uuid.UUID, runID, priorRunID, hubID string) error {
	if runID == "" {
		return nil
	}
	row, err := client.Agent.Query().
		Where(agent.IDEQ(uid)).
		Only(ctx)
	if err != nil {
		return mapError(err)
	}
	attr, err := buildRunAttributionFromAgentRow(ctx, client, row, runID, priorRunID, hubID)
	if err != nil {
		return err
	}
	return insertRunAttributionOnConflictDoNothing(ctx, client, attr)
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

// BackfillActiveRunAttributions implements store.UsageLedgerStore.BackfillActiveRunAttributions.
// Queries all non-deleted agents with a non-empty run_id and inserts missing
// run_attribution rows via INSERT ... ON CONFLICT (run_id) DO NOTHING.
// Completely idempotent and safe to run concurrently across multiple replicas
// at startup and on every OFF -> ON transition of tokenomics.usage_ledger.
func (s *UsageLedgerStore) BackfillActiveRunAttributions(ctx context.Context, hubID string) (int, error) {
	rows, err := s.client.Agent.Query().
		Where(agent.DeletedAtIsNil(), agent.RunIDNEQ("")).
		All(ctx)
	if err != nil {
		return 0, mapError(err)
	}

	inserted := 0
	for _, row := range rows {
		existed, err := s.client.RunAttribution.Query().
			Where(runattribution.IDEQ(row.RunID)).
			Exist(ctx)
		if err != nil {
			return inserted, mapError(err)
		}
		if existed {
			continue
		}
		attr, err := buildRunAttributionFromAgentRow(ctx, s.client, row, row.RunID, "", hubID)
		if err != nil {
			return inserted, err
		}
		if err := insertRunAttributionOnConflictDoNothing(ctx, s.client, attr); err != nil {
			return inserted, err
		}
		inserted++
	}
	return inserted, nil
}

// UpsertMeterHealth implements store.UsageLedgerStore.UpsertMeterHealth.
// Enforces monotonic sequence and final_state progression per §4.3.
func (s *UsageLedgerStore) UpsertMeterHealth(ctx context.Context, mh *store.MeterHealth) error {
	if mh == nil || mh.AgentID == "" || mh.RunID == "" || mh.ProducerEpoch == "" {
		return fmt.Errorf("%w: agent_id, run_id, and producer_epoch are required", store.ErrInvalidInput)
	}
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

	run := func(client *ent.Client) error {
		q := client.MeterHealth.Query().Where(
			meterhealth.AgentIDEQ(mh.AgentID),
			meterhealth.RunIDEQ(mh.RunID),
			meterhealth.ProducerEpochEQ(mh.ProducerEpoch),
		)
		if s.usesRowLocks() {
			q = q.ForUpdate()
		}
		existing, err := q.Only(ctx)
		if err != nil && !ent.IsNotFound(err) {
			return mapError(err)
		}
		if existing == nil {
			b := client.MeterHealth.Create().
				SetID(uuid.New()).
				SetAgentID(mh.AgentID).
				SetRunID(mh.RunID).
				SetProducerEpoch(mh.ProducerEpoch).
				SetLastReceiptAt(lastReceipt).
				SetLastContiguousSeq(mh.LastContiguousSeq).
				SetMaxSeqSeen(mh.MaxSeqSeen).
				SetNillableLastSourceActivityAt(lastSourceActivity).
				SetAnomalousCount(mh.AnomalousCount).
				SetSchemaDriftDetected(mh.SchemaDriftDetected).
				SetFinalState(finalState).
				SetUpdatedAt(now)
			if len(mh.MissingRanges) > 0 {
				b = b.SetMissingRanges(mh.MissingRanges)
			}
			if len(mh.SupportedSignals) > 0 {
				b = b.SetSupportedSignals(mh.SupportedSignals)
			}
			if err := client.MeterHealth.CreateBulk(b).
				OnConflictColumns(meterhealth.FieldAgentID, meterhealth.FieldRunID, meterhealth.FieldProducerEpoch).
				DoNothing().
				Exec(ctx); err != nil {
				return mapError(err)
			}
			return nil
		}

		nextReceipt := existing.LastReceiptAt.UTC()
		if lastReceipt.After(nextReceipt) {
			nextReceipt = lastReceipt
		}
		nextContig := existing.LastContiguousSeq
		if mh.LastContiguousSeq > nextContig {
			nextContig = mh.LastContiguousSeq
		}
		nextMax := existing.MaxSeqSeen
		if mh.MaxSeqSeen > nextMax {
			nextMax = mh.MaxSeqSeen
		}
		nextAnomalous := existing.AnomalousCount
		if mh.AnomalousCount > nextAnomalous {
			nextAnomalous = mh.AnomalousCount
		}
		nextDrift := existing.SchemaDriftDetected || mh.SchemaDriftDetected
		nextFinal := existing.FinalState
		if nextFinal == store.MeterHealthFinalStateOpen &&
			(finalState == store.MeterHealthFinalStateFinal || finalState == store.MeterHealthFinalStateIncomplete) {
			nextFinal = finalState
		}

		u := client.MeterHealth.UpdateOneID(existing.ID).
			SetLastReceiptAt(nextReceipt).
			SetLastContiguousSeq(nextContig).
			SetMaxSeqSeen(nextMax).
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
		if mh.MissingRanges != nil {
			// Prune any ranges at or below nextContig so missing_ranges stay
			// consistent with the monotonic contiguous sequence.
			pruned := make([]store.SeqRange, 0, len(mh.MissingRanges))
			for _, r := range mh.MissingRanges {
				if r.End <= nextContig {
					continue
				}
				if r.Start <= nextContig {
					r.Start = nextContig + 1
				}
				pruned = append(pruned, r)
			}
			u = u.SetMissingRanges(pruned)
		}

		_, err = u.Save(ctx)
		return mapError(err)
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
