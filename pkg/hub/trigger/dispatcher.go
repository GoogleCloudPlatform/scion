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

package trigger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/durablequeue"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/triggerrule"
)

// Trigger drainer, fire budget, and maintenance defaults (Stage 1 §4.3, §4.4,
// §6.1, §6.3, §6.4, rev 7.2, rev 7.4).
const (
	DefaultPollInterval   = 2 * time.Second
	DefaultHandlerTimeout = 150 * time.Second
	DefaultLeaseDuration  = 180 * time.Second
	DefaultMaxRows        = 500_000
	DefaultBatchSize      = 4
	DefaultMaxAge         = 72 * time.Hour
	DefaultMaxPermanent   = 5

	DefaultPerFireTimeout  = 30 * time.Second
	DefaultEventRuleBudget = time.Second
	FireLeaseHorizon       = 10 * time.Minute
	HistoryRetention       = 7 * 24 * time.Hour
	MaxFireSubjectBytes    = 256

	DefaultReapBatchLimit      = 500
	DefaultReconcileBatchLimit = 500

	ExecutorKindTriggerDrainer   = "trigger_drainer"
	ExecutorIDPrefixTriggerEvent = "trigger_event:"

	// Content-free generic error texts persisted on trigger_events / trigger_fires.
	ErrEventDroppedByQueueLimits = "dropped by queue limits"
	ErrEventProcessingFailed     = "processing failed"
	ErrFireRuleBudgetExhausted   = "rule budget exhausted"
	ErrFireRuleEvaluationFailed  = "rule evaluation failed"
)

// Core is the narrow capability interface package hub/trigger consumes from
// package hub without importing package hub (Stage 1 §11 D-S1-1(c)).
type Core interface {
	Fire(ctx context.Context, req hubfire.FireRequest) hubfire.FireOutcome
	Experiment(name string) bool
}

// FireAdmitter is an optional interface Core implementations may satisfy so
// long-running trigger fires (such as message --wake) register with hub
// shutdown admission and receive hub shutdown-abort signals while remaining
// bounded by itemCtx (the queue item's lifetime, Stage 1 ledger L3).
//
// admittedCtx preserves the caller's per-fire deadline alongside any abort
// values; abortCtx carries ONLY the real abort signal (hub shutdown + queue
// item lifetime, never the per-fire deadline) so Detach can outlive a wake
// resume longer than PerFireTimeout without outliving shutdown or itemCtx.
type FireAdmitter interface {
	AdmitFire(ctx, itemCtx context.Context) (admittedCtx, abortCtx context.Context, cleanup func(), ok bool)
}

// DispatcherConfig configures a trigger event Dispatcher.
type DispatcherConfig struct {
	Core           Core
	Store          store.TriggerStore
	PerFireTimeout time.Duration
	RuleBudget     time.Duration
	Logger         *slog.Logger
	Now            func() time.Time
}

// DrainerOptions configures a durablequeue.Drainer driving a Dispatcher.
type DrainerOptions struct {
	QueueStore          store.DurableQueueStore
	Queue               durablequeue.Queue
	Owner               string
	PollInterval        time.Duration
	LeaseDuration       time.Duration
	HandlerTimeout      time.Duration
	BatchSize           int
	MaxRows             int
	MaxPermanent        int
	MaxAge              time.Duration
	MaintenanceInterval time.Duration
	ShutdownBudget      time.Duration
	Metrics             durablequeue.Metrics
}

// Dispatcher consumes trigger.event items from durablequeue, matches active
// triggers, claims idempotent trigger_fires rows, invokes Core.Fire, and marks
// trigger_events rows processed (§6.3).
type Dispatcher struct {
	core           Core
	store          store.TriggerStore
	perFireTimeout time.Duration
	ruleBudget     time.Duration
	logger         *slog.Logger
	now            func() time.Time
}

// NewDispatcher validates cfg and constructs a Dispatcher.
func NewDispatcher(cfg DispatcherConfig) (*Dispatcher, error) {
	if cfg.Core == nil {
		return nil, errors.New("trigger: dispatcher requires Core")
	}
	if cfg.Store == nil {
		return nil, errors.New("trigger: dispatcher requires Store")
	}
	perFire := cfg.PerFireTimeout
	if perFire <= 0 {
		perFire = DefaultPerFireTimeout
	}
	ruleBudget := cfg.RuleBudget
	if ruleBudget <= 0 {
		ruleBudget = DefaultEventRuleBudget
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	return &Dispatcher{
		core:           cfg.Core,
		store:          cfg.Store,
		perFireTimeout: perFire,
		ruleBudget:     ruleBudget,
		logger:         logger,
		now:            nowFn,
	}, nil
}

// NewDrainer constructs the trigger.event durablequeue.Drainer with Stage 1
// §6.1 / §6.3 / rev 7.2 / rev 7.4 bounds: HandlerTimeout 150s, LeaseDuration
// 180s, MaxRows 500_000 (explicit), BatchSize 4, MaxAge 72h, MaxPermanent 5,
// Enabled gated on Core.Experiment(experiments.Triggers), and OnDrop marking
// dropped trigger_events rows failed.
func (d *Dispatcher) NewDrainer(opts DrainerOptions) (*durablequeue.Drainer, error) {
	if d == nil {
		return nil, errors.New("trigger: dispatcher is nil")
	}
	if opts.QueueStore == nil {
		return nil, errors.New("trigger: drainer requires QueueStore")
	}
	poll := opts.PollInterval
	if poll <= 0 {
		poll = DefaultPollInterval
	}
	handlerTimeout := opts.HandlerTimeout
	if handlerTimeout <= 0 {
		handlerTimeout = DefaultHandlerTimeout
	}
	leaseDur := opts.LeaseDuration
	if leaseDur <= 0 {
		leaseDur = DefaultLeaseDuration
	}
	batchSize := opts.BatchSize
	if batchSize <= 0 {
		batchSize = DefaultBatchSize
	}
	maxRows := opts.MaxRows
	if maxRows <= 0 {
		maxRows = DefaultMaxRows
	}
	maxPerm := opts.MaxPermanent
	if maxPerm <= 0 {
		maxPerm = DefaultMaxPermanent
	}
	maxAge := opts.MaxAge
	if maxAge <= 0 {
		maxAge = DefaultMaxAge
	}

	return durablequeue.NewDrainer(durablequeue.DrainerConfig{
		Store:   opts.QueueStore,
		Queue:   opts.Queue,
		Kind:    EventQueueKind,
		Handler: d.HandleItem,
		Owner:   opts.Owner,
		Enabled: func() bool {
			return d.core.Experiment(experiments.Triggers)
		},
		PollInterval:        poll,
		LeaseDuration:       leaseDur,
		HandlerTimeout:      handlerTimeout,
		BatchSize:           batchSize,
		MaxRows:             maxRows,
		MaxPermanent:        maxPerm,
		MaxAge:              maxAge,
		MaintenanceInterval: opts.MaintenanceInterval,
		ShutdownBudget:      opts.ShutdownBudget,
		OnDrop:              d.onQueueDrop,
		Metrics:             opts.Metrics,
		Logger:              d.logger,
		Now:                 d.now,
	})
}

func (d *Dispatcher) onQueueDrop(ctx context.Context, dropped []durablequeue.Item, reason string) {
	errMsg := ErrEventDroppedByQueueLimits
	if reason == durablequeue.DropReasonPoison {
		errMsg = ErrEventProcessingFailed
	}
	now := d.now().UTC()
	for _, it := range dropped {
		if it.DedupKey == "" {
			continue
		}
		if err := d.store.MarkTriggerEventFailed(ctx, it.DedupKey, now, errMsg); err != nil &&
			!errors.Is(err, store.ErrNotFound) && !errors.Is(err, store.ErrInvalidInput) {
			d.logger.Warn("trigger: failed to mark dropped event failed",
				"eventID", it.DedupKey, "reason", reason, "error", err)
		}
	}
}

// HandleItem processes one leased trigger.event queue item (§6.3).
func (d *Dispatcher) HandleItem(ctx context.Context, item durablequeue.Item, lease durablequeue.Lease) durablequeue.Result {
	var qp EventQueuePayload
	if err := json.Unmarshal(item.Payload, &qp); err != nil || qp.Version != EventQueuePayloadVersion || qp.EventID == "" {
		return durablequeue.Result{
			Permanent: true,
			Err:       errors.New("trigger: invalid event queue payload"),
		}
	}

	ev, err := d.store.GetTriggerEvent(ctx, qp.EventID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalidInput) {
			return durablequeue.Result{Done: true}
		}
		d.logger.Warn("trigger: failed to load trigger event", "eventID", qp.EventID, "error", err)
		return durablequeue.Result{Err: errors.New("trigger: load event failed")}
	}
	if ev.Status != store.TriggerEventStatusPending {
		return durablequeue.Result{Done: true}
	}

	candidates, err := d.loadActiveTriggers(ctx, ev.ProjectID, ev.SourceKind, ev.SourceID)
	if err != nil {
		d.logger.Warn("trigger: failed to list active triggers",
			"eventID", ev.ID, "projectID", ev.ProjectID, "sourceKind", ev.SourceKind, "sourceID", ev.SourceID, "error", err)
		return durablequeue.Result{Err: errors.New("trigger: list candidates failed")}
	}

	// Parse payload JSON once per event so rule evaluation shares one value (§6.3, F5).
	// Phase 3b-2 (ptone/scion#8030) consumes parsedPayload when building the CEL
	// rule activation; Stage 1b-3 performs Types-only matching and still charges
	// the once-per-event JSON parse against ruleElapsed.
	parseStart := d.now()
	var parsedPayload any
	if ev.Payload != "" {
		_ = json.Unmarshal([]byte(ev.Payload), &parsedPayload)
	}
	_ = parsedPayload
	ruleElapsed := max(d.now().Sub(parseStart), time.Duration(0))

	var (
		matchCount int
		fireCount  int
	)

	for i := range candidates {
		if ctx.Err() != nil {
			return durablequeue.Result{Err: errors.New("trigger: event handler canceled")}
		}
		tr := &candidates[i]

		typeStart := d.now()
		matched := triggerrule.MatchTypes(tr.MatchTypes, ev.Type)
		ruleElapsed += max(d.now().Sub(typeStart), time.Duration(0))
		if !matched {
			continue
		}

		if ruleElapsed >= d.ruleBudget {
			if rErr := d.recordDirectFire(ctx, tr, ev, deriveFireSubject(ev.Subject), store.TriggerFireStatusSkipped, ErrFireRuleBudgetExhausted); rErr != nil {
				return durablequeue.Result{Err: errors.New("trigger: record skipped fire failed")}
			}
			continue
		}

		// Fail closed if a stored row carries a filter expression before 3b-2 wires evaluation.
		if strings.TrimSpace(tr.MatchRule) != "" {
			if rErr := d.recordDirectFire(ctx, tr, ev, deriveFireSubject(ev.Subject), store.TriggerFireStatusSkipped, ErrFireRuleEvaluationFailed); rErr != nil {
				return durablequeue.Result{Err: errors.New("trigger: record skipped fire failed")}
			}
			continue
		}

		matchCount++

		if lease != nil {
			held, lErr := lease.Renew(ctx)
			if lErr != nil {
				return durablequeue.Result{Err: errors.New("trigger: renew queue lease failed")}
			}
			if !held {
				return durablequeue.Result{Err: errors.New("trigger: queue lease lost")}
			}
		}

		fireSubject := deriveFireSubject(ev.Subject)
		claimNow := d.now().UTC()
		fire := store.TriggerFire{
			ID:                   uuid.NewString(),
			ProjectID:            tr.ProjectID,
			TriggerID:            tr.ID,
			EventID:              ev.ID,
			Subject:              fireSubject,
			ActionType:           tr.ActionType,
			Status:               store.TriggerFireStatusFiring,
			ExecutorKind:         ExecutorKindTriggerDrainer,
			ExecutorID:           ExecutorIDPrefixTriggerEvent + ev.ID,
			LeaseUntil:           claimNow.Add(FireLeaseHorizon),
			StartedAt:            claimNow,
			CreatedAt:            claimNow,
			InitiatorAttribution: tr.InitiatorAttribution,
		}

		claimed, existing, cErr := d.store.ClaimTriggerFire(ctx, &fire)
		if cErr != nil {
			d.logger.Warn("trigger: failed to claim trigger fire",
				"eventID", ev.ID, "triggerID", tr.ID, "error", cErr)
			return durablequeue.Result{Err: errors.New("trigger: claim fire failed")}
		}
		if !claimed {
			// Display-only counter: on a lost-lease re-handle, if an earlier
			// attempt's claimed row is still in status "firing" (or transitions to
			// "fired" after this check), fire_count on trigger_events can
			// undercount; trigger_fires rows remain the authoritative fire log.
			if existing != nil && existing.Status == store.TriggerFireStatusFired {
				fireCount++
			}
			continue
		}

		tracker := newTriggerFireTracker(ctx, d.core)
		var rawPayload []byte
		if ev.Payload != "" {
			rawPayload = []byte(ev.Payload)
		}
		req := hubfire.FireRequest{
			ProjectID: tr.ProjectID,
			Authority: hubfire.RecordedAuthority{
				Attribution:    tr.InitiatorAttribution,
				Ceiling:        tr.AuthorityCeiling,
				DefinitionKind: hubfire.DefinitionKindTrigger,
				DefinitionID:   tr.ID,
				FireID:         fire.ID,
			},
			ActionType: tr.ActionType,
			Payload:    tr.ActionPayload,
			Event: &hubfire.FireEvent{
				ID:         ev.ID,
				ProjectID:  ev.ProjectID,
				SourceKind: ev.SourceKind,
				SourceID:   ev.SourceID,
				Type:       ev.Type,
				Subject:    ev.Subject,
				OccurredAt: ev.OccurredAt,
				ReceivedAt: ev.ReceivedAt,
				Attributes: maps.Clone(ev.Attributes),
				Payload:    rawPayload,
				Trigger: &hubfire.FireTriggerRef{
					ID:   tr.ID,
					Name: tr.Name,
				},
			},
			Executor: hubfire.ExecutorContext{
				Kind: ExecutorKindTriggerDrainer,
				ID:   ExecutorIDPrefixTriggerEvent + ev.ID,
			},
			FireAt:  ev.ReceivedAt,
			Tracked: tracker,
		}

		fireCtx, cancelFire := context.WithTimeout(ctx, d.perFireTimeout)
		out := d.core.Fire(fireCtx, req)
		cancelFire()

		statusStr := normalizeFireStatus(out.Status, out.Error)
		finishedAt := d.now().UTC()

		writeCtx, cancelWrite := storeWriteContext(ctx)
		var (
			finished  bool
			finishErr error
		)
		// NOTE (PR 2): triggerFireTracker.Record runs at most one write per fire.
		// Stage 1's message action never calls req.Tracked.Record itself; if a PR 2
		// action records via req.Tracked.Record directly, this outer tracker.Record
		// call becomes a no-op and—unless that action also updates trigger_fires—the
		// firing row would be left to the hourly reaper.
		tracker.Record(func() {
			finished, finishErr = d.store.FinishTriggerFire(writeCtx, fire.ID, statusStr, out.ResultRef, out.Error, finishedAt)
		})
		if finishErr != nil {
			cancelWrite()
			d.logger.Warn("trigger: failed to finish trigger fire",
				"fireID", fire.ID, "eventID", ev.ID, "triggerID", tr.ID, "error", finishErr)
			return durablequeue.Result{Err: errors.New("trigger: finish fire failed")}
		}
		if finished {
			if statusStr == store.TriggerFireStatusFired {
				fireCount++
			}
			if out.AfterRecord != nil {
				out.AfterRecord(writeCtx)
			}
			if rErr := d.store.RecordTriggerFire(writeCtx, tr.ID, finishedAt, statusStr, out.Error); rErr != nil {
				d.logger.Warn("trigger: failed to update trigger fire counters",
					"triggerID", tr.ID, "fireID", fire.ID, "error", rErr)
			}
		}
		cancelWrite()
	}

	if ctx.Err() != nil {
		return durablequeue.Result{Err: errors.New("trigger: event handler canceled")}
	}
	if err := d.store.MarkTriggerEventProcessed(ctx, ev.ID, d.now().UTC(), matchCount, fireCount); err != nil &&
		!errors.Is(err, store.ErrNotFound) {
		d.logger.Warn("trigger: failed to mark event processed", "eventID", ev.ID, "error", err)
		return durablequeue.Result{Err: errors.New("trigger: mark event processed failed")}
	}
	return durablequeue.Result{Done: true}
}

func (d *Dispatcher) loadActiveTriggers(ctx context.Context, projectID, sourceKind, sourceID string) ([]store.Trigger, error) {
	var (
		all    []store.Trigger
		cursor string
	)
	for {
		res, err := d.store.ListTriggers(ctx, store.TriggerFilter{
			ProjectID:  projectID,
			SourceKind: sourceKind,
			SourceID:   sourceID,
			Status:     store.TriggerStatusActive,
		}, store.ListOptions{
			Limit:  store.MaxTriggersPerProject,
			Cursor: cursor,
		})
		if err != nil {
			return nil, err
		}
		all = append(all, res.Items...)
		if res.NextCursor == "" || len(res.Items) == 0 {
			return all, nil
		}
		cursor = res.NextCursor
	}
}

func (d *Dispatcher) recordDirectFire(ctx context.Context, tr *store.Trigger, ev *store.TriggerEvent, subject, status, errMsg string) error {
	now := d.now().UTC()
	fire := store.TriggerFire{
		ID:                   uuid.NewString(),
		ProjectID:            tr.ProjectID,
		TriggerID:            tr.ID,
		EventID:              ev.ID,
		Subject:              subject,
		ActionType:           tr.ActionType,
		Status:               status,
		Error:                errMsg,
		ExecutorKind:         ExecutorKindTriggerDrainer,
		ExecutorID:           ExecutorIDPrefixTriggerEvent + ev.ID,
		LeaseUntil:           now.Add(FireLeaseHorizon),
		StartedAt:            now,
		FinishedAt:           &now,
		CreatedAt:            now,
		InitiatorAttribution: tr.InitiatorAttribution,
	}
	claimed, _, err := d.store.ClaimTriggerFire(ctx, &fire)
	if err != nil {
		return err
	}
	if claimed {
		if rErr := d.store.RecordTriggerFire(ctx, tr.ID, now, status, errMsg); rErr != nil {
			d.logger.Warn("trigger: failed to record direct fire counter",
				"triggerID", tr.ID, "fireID", fire.ID, "error", rErr)
		}
	}
	return nil
}

func normalizeFireStatus(st hubfire.FireStatus, errMsg string) string {
	switch st {
	case hubfire.FireStatusFired:
		return store.TriggerFireStatusFired
	case hubfire.FireStatusFailed:
		return store.TriggerFireStatusFailed
	case hubfire.FireStatusRefused:
		return store.TriggerFireStatusRefused
	case hubfire.FireStatusSkipped:
		return store.TriggerFireStatusSkipped
	default:
		if errMsg != "" {
			return store.TriggerFireStatusFailed
		}
		return store.TriggerFireStatusFired
	}
}

func deriveFireSubject(raw string) string {
	if len(raw) <= MaxFireSubjectBytes {
		return raw
	}
	cut := MaxFireSubjectBytes
	for cut > 0 && !utf8.RuneStart(raw[cut]) {
		cut--
	}
	return raw[:cut]
}

func storeWriteContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx.Err() == nil {
		return ctx, func() {}
	}
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
}

// triggerFireTracker implements hubfire.FireTracker for a single trigger fire
// inside an active trigger.event queue item (§5.4, §6.3, rev 7.2).
//
// Detach strips the per-fire 30s deadline while keeping the queue item's 150s
// deadline, lease-loss cancellation, and any hub shutdown-abort cancellation so
// a wake fire never outlives its queue item.
type triggerFireTracker struct {
	itemCtx context.Context
	core    Core

	mu       sync.Mutex
	abortCtx context.Context

	once sync.Once
}

func newTriggerFireTracker(itemCtx context.Context, core Core) *triggerFireTracker {
	return &triggerFireTracker{
		itemCtx: itemCtx,
		core:    core,
	}
}

func (t *triggerFireTracker) Detach(ctx context.Context) (context.Context, context.CancelFunc) {
	dctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stopItem := context.AfterFunc(t.itemCtx, cancel)

	t.mu.Lock()
	abortCtx := t.abortCtx
	t.mu.Unlock()
	var stopAbort func() bool
	if abortCtx != nil {
		stopAbort = context.AfterFunc(abortCtx, cancel)
	}

	cleanup := func() {
		stopItem()
		if stopAbort != nil {
			stopAbort()
		}
		cancel()
	}
	if deadline, ok := t.itemCtx.Deadline(); ok {
		tctx, tcancel := context.WithDeadline(dctx, deadline)
		return tctx, func() {
			tcancel()
			cleanup()
		}
	}
	return dctx, cleanup
}

func (t *triggerFireTracker) Admit(ctx context.Context) (context.Context, func(), bool) {
	if t.itemCtx.Err() != nil {
		return ctx, func() {}, false
	}
	if adm, ok := t.core.(FireAdmitter); ok {
		admittedCtx, abortCtx, done, admitted := adm.AdmitFire(ctx, t.itemCtx)
		if !admitted {
			return ctx, func() {}, false
		}
		t.mu.Lock()
		t.abortCtx = abortCtx
		t.mu.Unlock()
		return admittedCtx, done, true
	}
	return ctx, func() {}, true
}

func (t *triggerFireTracker) Record(write func()) bool {
	ran := false
	t.once.Do(func() {
		ran = true
		if write != nil {
			write()
		}
	})
	return ran
}

// MaintenanceResult reports counts from one hourly trigger maintenance pass.
type MaintenanceResult struct {
	FiresReaped      int
	EventsReconciled int
	EventsPurged     int
	FiresPurged      int
}

// RunMaintenance executes the hourly trigger maintenance pass (§4.3, §6.4,
// rev 7.4):
//  1. Reaps expired firing rows (lease_until <= now) to failed "hub restarted during fire".
//  2. Reconciles pending trigger_events older than DefaultMaxAge (72h) with no
//     trigger.event queue item to failed "dropped by queue limits".
//  3. Purges terminal trigger_fires and trigger_events older than
//     HistoryRetention (7 days).
func RunMaintenance(ctx context.Context, ts store.TriggerStore, now time.Time) (MaintenanceResult, error) {
	var out MaintenanceResult
	if ts == nil {
		return out, fmt.Errorf("%w: trigger store is nil", store.ErrInvalidInput)
	}
	now = now.UTC()

	var errs []error
	reaped, err := ts.ReapExpiredTriggerFires(ctx, now, DefaultReapBatchLimit)
	out.FiresReaped = reaped
	if err != nil {
		errs = append(errs, fmt.Errorf("reap expired trigger fires: %w", err))
	}

	reconciled, err := ts.ReconcileOrphanedPendingTriggerEvents(
		ctx,
		EventQueueKind,
		now.Add(-DefaultMaxAge),
		now,
		ErrEventDroppedByQueueLimits,
		DefaultReconcileBatchLimit,
	)
	out.EventsReconciled = reconciled
	if err != nil {
		errs = append(errs, fmt.Errorf("reconcile orphaned pending trigger events: %w", err))
	}

	eventsPurged, firesPurged, err := ts.PurgeOldTriggerHistory(ctx, now.Add(-HistoryRetention))
	out.EventsPurged = eventsPurged
	out.FiresPurged = firesPurged
	if err != nil {
		errs = append(errs, fmt.Errorf("purge old trigger history: %w", err))
	}

	return out, errors.Join(errs...)
}
