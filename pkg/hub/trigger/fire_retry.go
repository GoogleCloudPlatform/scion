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
	"maps"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/durablequeue"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Offline message retry queue constants and bounds (Stage 2 §6.1, §6.2).
const (
	FireRetryQueueKind           = "trigger.fire_retry"
	FireRetryQueuePayloadVersion = 1
	FireRetryDedupPrefix         = "fire:"

	FireRetryInitialBackoff = 30 * time.Second
	FireRetryMaxBackoff     = 5 * time.Minute
	FireRetryMaxAge         = 72 * time.Hour
	FireRetryMaxRows        = 50_000

	MaxWaitingFiresPerTarget  = 100
	MaxWaitingFiresPerProject = 1000

	ExecutorKindTriggerRetryDrainer = "trigger_retry_drainer"
	ExecutorIDPrefixTriggerFire     = "trigger_fire:"

	// Content-free generic error/reason texts for offline queue outcomes (§6.2).
	ErrFireOfflineQueueFull     = "offline queue full"
	ErrFireOfflineQueueExpired  = "offline queue expired"
	ErrFireDroppedByQueueLimits = "dropped by queue limits"
	ErrFireTriggerChanged       = "trigger changed"
	ErrFireEventDeleted         = "event deleted"
	ErrFireTargetDeleted        = "target deleted"
	ErrFireTargetNotRunning     = "target not running"
)

// FireRetryQueuePayload is the JSON payload stored in durable_queue_items for
// FireRetryQueueKind. Consumers reload the authoritative trigger_fires,
// triggers, and trigger_events rows by ID and trust no other field from the
// queue payload (Stage 2 §6.2).
type FireRetryQueuePayload struct {
	Version int    `json:"v"`
	FireID  string `json:"fire_id"`
}

// RetryTargetState summarizes a target agent's live deliverability state for
// offline message retries and the nudge-while-leased re-check (Stage 2 §6.2).
type RetryTargetState int

const (
	// RetryTargetOffline indicates a transiently undeliverable phase
	// (suspended, stopped, provisioning, starting, created, stopping).
	RetryTargetOffline RetryTargetState = iota
	// RetryTargetDeliverable indicates the target agent is running and can
	// accept messages immediately.
	RetryTargetDeliverable
	// RetryTargetTerminal indicates an error or halted phase that will not
	// recover on its own.
	RetryTargetTerminal
	// RetryTargetDeleted indicates the target agent no longer exists or has
	// been soft-deleted.
	RetryTargetDeleted
)

// RetryTargetChecker is an optional interface Core implementations may satisfy
// so the trigger.fire_retry handler can detect deleted targets before firing
// and re-check live deliverability right before returning RetryAt (closing the
// nudge-while-leased window where a running transition arrived while the retry
// attempt held the queue lease).
type RetryTargetChecker interface {
	CheckRetryTarget(ctx context.Context, agentID string) (RetryTargetState, error)
}

// FireRetryBackoff computes exponential retry backoff for offline message
// fires: 30 s -> 60 s -> 120 s -> 240 s -> 5 min cap (Stage 2 §6.2 item 3).
func FireRetryBackoff(attempts int) time.Duration {
	if attempts <= 1 {
		return FireRetryInitialBackoff
	}
	shift := min(attempts-1, 4)
	d := FireRetryInitialBackoff << shift
	if d > FireRetryMaxBackoff {
		return FireRetryMaxBackoff
	}
	return d
}

// NewFireRetryDrainer constructs the trigger.fire_retry durablequeue.Drainer
// with Stage 2 §6.2 item 6 bounds: MaxAge 72h (>= 48h TTL ceiling), MaxRows
// 50,000, FairByProject, Enabled gated on hub.triggers, and OnDrop marking
// dropped waiting fires expired "dropped by queue limits".
func (d *Dispatcher) NewFireRetryDrainer(opts DrainerOptions) (*durablequeue.Drainer, error) {
	if d == nil {
		return nil, errors.New("trigger: dispatcher is nil")
	}
	if opts.QueueStore == nil {
		return nil, errors.New("trigger: fire retry drainer requires QueueStore")
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
		maxRows = FireRetryMaxRows
	}
	maxPerm := opts.MaxPermanent
	if maxPerm <= 0 {
		maxPerm = DefaultMaxPermanent
	}
	maxAge := opts.MaxAge
	if maxAge <= 0 {
		maxAge = FireRetryMaxAge
	}

	return durablequeue.NewDrainer(durablequeue.DrainerConfig{
		Store:   opts.QueueStore,
		Queue:   opts.Queue,
		Kind:    FireRetryQueueKind,
		Handler: d.HandleFireRetryItem,
		Owner:   opts.Owner,
		Enabled: func() bool {
			return d.core.Experiment(experiments.Triggers)
		},
		PollInterval:        poll,
		LeaseDuration:       leaseDur,
		HandlerTimeout:      handlerTimeout,
		BatchSize:           batchSize,
		FairByProject:       true,
		MaxRows:             maxRows,
		MaxPermanent:        maxPerm,
		MaxAge:              maxAge,
		MaintenanceInterval: opts.MaintenanceInterval,
		ShutdownBudget:      opts.ShutdownBudget,
		OnDrop:              d.onFireRetryDrop,
		Metrics:             opts.Metrics,
		Logger:              d.logger,
		Now:                 d.now,
	})
}

func (d *Dispatcher) onFireRetryDrop(ctx context.Context, dropped []durablequeue.Item, reason string) {
	now := d.now().UTC()
	for _, it := range dropped {
		fireID, ok := strings.CutPrefix(it.DedupKey, FireRetryDedupPrefix)
		if !ok || fireID == "" {
			continue
		}
		finished, err := d.store.FinishWaitingTriggerFire(
			ctx,
			fireID,
			store.TriggerFireStatusExpired,
			"",
			"",
			ErrFireDroppedByQueueLimits,
			now,
			0,
		)
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) && !errors.Is(err, store.ErrInvalidInput) {
				d.logger.Warn("trigger: failed to mark dropped waiting fire expired",
					"fireID", fireID, "reason", reason, "error", err)
			}
			continue
		}
		if !finished {
			continue
		}
		fire, gErr := d.store.GetTriggerFire(ctx, fireID)
		if gErr != nil {
			d.logger.Warn("trigger: failed to load dropped waiting fire for accounting",
				"fireID", fireID, "error", gErr)
			continue
		}
		d.recordWaitingFireTerminal(ctx, fire, store.TriggerFireStatusExpired, ErrFireDroppedByQueueLimits, now)
	}
}

// HandleFireRetryItem processes one leased trigger.fire_retry queue item
// (Stage 2 §6.2 item 3, §18 S17):
//   - terminal fire -> Done
//   - past wait_until -> CAS waiting -> expired "offline queue expired", Done
//   - trigger deleted, paused, or edited (authorization_revision differs) ->
//     CAS waiting -> skipped "trigger changed", Done
//   - target agent deleted -> CAS waiting -> skipped "target deleted", Done
//   - otherwise rebuild FireRequest from current trigger + event and invoke
//     Core.Fire (full recorded authority, standing, live authz, and wake)
//   - terminal outcome -> CAS waiting -> outcome, Done
//   - waiting again -> re-check live target deliverability before returning
//     RetryAt (closing the nudge-while-leased gap); if deliverable now, return
//     RetryAt = now, otherwise exponential backoff 30s -> 5m bounded by
//     wait_until.
func (d *Dispatcher) HandleFireRetryItem(ctx context.Context, item durablequeue.Item, lease durablequeue.Lease) durablequeue.Result {
	var qp FireRetryQueuePayload
	if err := json.Unmarshal(item.Payload, &qp); err != nil || qp.Version != FireRetryQueuePayloadVersion || qp.FireID == "" {
		return durablequeue.Result{
			Permanent: true,
			Err:       errors.New("trigger: invalid fire retry queue payload"),
		}
	}

	fire, err := d.store.GetTriggerFire(ctx, qp.FireID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalidInput) {
			return durablequeue.Result{Done: true}
		}
		d.logger.Warn("trigger: failed to load waiting fire", "fireID", qp.FireID, "error", err)
		return durablequeue.Result{Err: errors.New("trigger: load waiting fire failed")}
	}
	if fire.Status != store.TriggerFireStatusWaiting {
		return durablequeue.Result{Done: true}
	}

	now := d.now().UTC()
	if fire.WaitUntil != nil && !now.Before(*fire.WaitUntil) {
		if err := d.finishWaitingFire(ctx, fire, store.TriggerFireStatusExpired, "", fire.TargetAgentID, ErrFireOfflineQueueExpired, now, fire.Attempts, nil); err != nil {
			return durablequeue.Result{Err: errors.New("trigger: expire waiting fire failed")}
		}
		return durablequeue.Result{Done: true}
	}

	tr, err := d.store.GetTrigger(ctx, fire.TriggerID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalidInput) {
			if fErr := d.finishWaitingFire(ctx, fire, store.TriggerFireStatusSkipped, "", fire.TargetAgentID, ErrFireTriggerChanged, now, fire.Attempts, nil); fErr != nil {
				return durablequeue.Result{Err: errors.New("trigger: skip waiting fire failed")}
			}
			return durablequeue.Result{Done: true}
		}
		d.logger.Warn("trigger: failed to load trigger for waiting fire", "fireID", fire.ID, "triggerID", fire.TriggerID, "error", err)
		return durablequeue.Result{Err: errors.New("trigger: load trigger failed")}
	}

	if tr.Status != store.TriggerStatusActive || tr.AuthorizationRevision != fire.AuthorizationRevision {
		if fErr := d.finishWaitingFire(ctx, fire, store.TriggerFireStatusSkipped, "", fire.TargetAgentID, ErrFireTriggerChanged, now, fire.Attempts, nil); fErr != nil {
			return durablequeue.Result{Err: errors.New("trigger: skip waiting fire failed")}
		}
		return durablequeue.Result{Done: true}
	}

	if checker, ok := d.core.(RetryTargetChecker); ok && fire.TargetAgentID != "" {
		targetState, tErr := checker.CheckRetryTarget(ctx, fire.TargetAgentID)
		if tErr != nil {
			if ctx.Err() != nil {
				return durablequeue.Result{Err: errors.New("trigger: fire retry canceled")}
			}
			d.logger.Warn("trigger: failed to check retry target", "fireID", fire.ID, "targetAgentID", fire.TargetAgentID, "error", tErr)
			return durablequeue.Result{Err: errors.New("trigger: check retry target failed")}
		}
		if targetState == RetryTargetDeleted {
			if fErr := d.finishWaitingFire(ctx, fire, store.TriggerFireStatusSkipped, "", fire.TargetAgentID, ErrFireTargetDeleted, now, fire.Attempts, nil); fErr != nil {
				return durablequeue.Result{Err: errors.New("trigger: skip deleted target fire failed")}
			}
			return durablequeue.Result{Done: true}
		}
	}

	ev, err := d.store.GetTriggerEvent(ctx, fire.EventID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalidInput) {
			if fErr := d.finishWaitingFire(ctx, fire, store.TriggerFireStatusSkipped, "", fire.TargetAgentID, ErrFireEventDeleted, now, fire.Attempts, nil); fErr != nil {
				return durablequeue.Result{Err: errors.New("trigger: skip waiting fire failed")}
			}
			return durablequeue.Result{Done: true}
		}
		d.logger.Warn("trigger: failed to load event for waiting fire", "fireID", fire.ID, "eventID", fire.EventID, "error", err)
		return durablequeue.Result{Err: errors.New("trigger: load event failed")}
	}

	if lease != nil {
		held, lErr := lease.Renew(ctx)
		if lErr != nil {
			return durablequeue.Result{Err: errors.New("trigger: renew retry queue lease failed")}
		}
		if !held {
			return durablequeue.Result{Err: errors.New("trigger: retry queue lease lost")}
		}
	}

	// Atomically claim the waiting fire (waiting -> firing with lease) BEFORE
	// invoking Core.Fire so a replica crash, lost lease, or transient terminal
	// write failure after delivery can never cause a second delivery (Stage 2
	// §6.2 at-most-once delivery invariant). Any stuck firing row is reaped to
	// failed "hub restarted during fire" once lease_until expires.
	nextAttempts := fire.Attempts + 1
	claimNow := d.now().UTC()
	claimed, cErr := d.store.ClaimWaitingTriggerFire(ctx, fire.ID, nextAttempts, claimNow.Add(FireLeaseHorizon))
	if cErr != nil {
		d.logger.Warn("trigger: failed to claim waiting fire for retry", "fireID", fire.ID, "error", cErr)
		return durablequeue.Result{Err: errors.New("trigger: claim waiting fire failed")}
	}
	if !claimed {
		return durablequeue.Result{Done: true}
	}

	var rawPayload []byte
	if ev.Payload != "" {
		rawPayload = []byte(ev.Payload)
	}
	tracker := newTriggerFireTracker(ctx, d.core)
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
			Lists:      cloneStringListMap(ev.Lists),
			Payload:    rawPayload,
			Trigger: &hubfire.FireTriggerRef{
				ID:          tr.ID,
				Name:        tr.Name,
				SubjectMode: tr.SubjectMode,
				FireSubject: fire.Subject,
			},
		},
		Executor: hubfire.ExecutorContext{
			Kind: ExecutorKindTriggerRetryDrainer,
			ID:   ExecutorIDPrefixTriggerFire + fire.ID,
		},
		FireAt:   ev.ReceivedAt,
		Attempts: nextAttempts,
		Tracked:  tracker,
	}

	fireCtx, cancelFire := context.WithTimeout(ctx, d.perFireTimeout)
	out := d.core.Fire(fireCtx, req)
	cancelFire()

	finishedAt := d.now().UTC()
	targetID := out.TargetAgentID
	if targetID == "" {
		targetID = fire.TargetAgentID
	}

	if out.Status == hubfire.FireStatusWaiting {
		// If the deadline passed during the attempt (e.g. a 90s wake), expire
		// immediately instead of scheduling another retry.
		if fire.WaitUntil != nil && !finishedAt.Before(*fire.WaitUntil) {
			if fErr := d.finishFiringRetryFire(ctx, fire, store.TriggerFireStatusExpired, "", targetID, ErrFireOfflineQueueExpired, finishedAt, nil); fErr != nil {
				return durablequeue.Result{Err: errors.New("trigger: expire waiting fire failed")}
			}
			return durablequeue.Result{Done: true}
		}

		writeCtx, cancelWrite := storeWriteContext(ctx)
		returned, rErr := d.store.ReturnFiringTriggerFireToWaiting(writeCtx, fire.ID, out.Error)
		cancelWrite()
		if rErr != nil {
			d.logger.Warn("trigger: failed to return firing retry to waiting",
				"fireID", fire.ID, "attempts", nextAttempts, "error", rErr)
			return durablequeue.Result{Err: errors.New("trigger: return waiting fire failed")}
		}
		if !returned {
			return durablequeue.Result{Done: true}
		}

		retryAt := finishedAt.Add(FireRetryBackoff(nextAttempts))

		// Close the P2a nudge-while-leased gap: a running status transition
		// that arrived while this attempt held the queue lease could not
		// update the leased queue item. Re-check live deliverability right
		// before returning RetryAt; if the target is deliverable now, schedule
		// an immediate retry instead of sleeping for the backoff.
		if checker, ok := d.core.(RetryTargetChecker); ok && targetID != "" {
			if targetState, tErr := checker.CheckRetryTarget(ctx, targetID); tErr == nil {
				switch targetState {
				case RetryTargetDeliverable:
					retryAt = finishedAt
				case RetryTargetDeleted:
					if fErr := d.finishWaitingFire(ctx, fire, store.TriggerFireStatusSkipped, "", targetID, ErrFireTargetDeleted, finishedAt, nextAttempts, nil); fErr != nil {
						return durablequeue.Result{Err: errors.New("trigger: skip deleted target fire failed")}
					}
					return durablequeue.Result{Done: true}
				case RetryTargetTerminal:
					if fErr := d.finishWaitingFire(ctx, fire, store.TriggerFireStatusSkipped, "", targetID, ErrFireTargetNotRunning, finishedAt, nextAttempts, nil); fErr != nil {
						return durablequeue.Result{Err: errors.New("trigger: skip terminal target fire failed")}
					}
					return durablequeue.Result{Done: true}
				}
			}
		}

		if fire.WaitUntil != nil && retryAt.After(*fire.WaitUntil) {
			retryAt = *fire.WaitUntil
		}
		return durablequeue.Result{
			RetryAt: retryAt,
			Err:     errors.New(ErrFireTargetNotRunning),
		}
	}

	statusStr := normalizeFireStatus(out.Status, out.Error)
	if fErr := d.finishFiringRetryFire(ctx, fire, statusStr, out.ResultRef, targetID, out.Error, finishedAt, out.AfterRecord); fErr != nil {
		return durablequeue.Result{Err: errors.New("trigger: finish waiting fire failed")}
	}
	return durablequeue.Result{Done: true}
}

func (d *Dispatcher) finishFiringRetryFire(
	ctx context.Context,
	fire *store.TriggerFire,
	status, resultRef, targetAgentID, errMsg string,
	finishedAt time.Time,
	afterRecord func(context.Context),
) error {
	writeCtx, cancelWrite := storeWriteContext(ctx)
	defer cancelWrite()

	finished, err := d.store.FinishTriggerFire(writeCtx, fire.ID, status, resultRef, targetAgentID, errMsg, finishedAt)
	if err != nil {
		d.logger.Warn("trigger: failed to finish claimed retry fire",
			"fireID", fire.ID, "triggerID", fire.TriggerID, "status", status, "error", err)
		return err
	}
	if !finished {
		return nil
	}
	if afterRecord != nil {
		afterRecord(writeCtx)
	}
	d.recordWaitingFireTerminal(writeCtx, fire, status, errMsg, finishedAt)
	return nil
}

func (d *Dispatcher) finishWaitingFire(
	ctx context.Context,
	fire *store.TriggerFire,
	status, resultRef, targetAgentID, errMsg string,
	finishedAt time.Time,
	attempts int,
	afterRecord func(context.Context),
) error {
	writeCtx, cancelWrite := storeWriteContext(ctx)
	defer cancelWrite()

	finished, err := d.store.FinishWaitingTriggerFire(writeCtx, fire.ID, status, resultRef, targetAgentID, errMsg, finishedAt, attempts)
	if err != nil {
		d.logger.Warn("trigger: failed to finish waiting fire",
			"fireID", fire.ID, "triggerID", fire.TriggerID, "status", status, "error", err)
		return err
	}
	if !finished {
		return nil
	}
	if afterRecord != nil {
		afterRecord(writeCtx)
	}
	d.recordWaitingFireTerminal(writeCtx, fire, status, errMsg, finishedAt)
	return nil
}

func (d *Dispatcher) recordWaitingFireTerminal(
	ctx context.Context,
	fire *store.TriggerFire,
	status, errMsg string,
	finishedAt time.Time,
) {
	d.recordFireMetric(fire.ActionType, status, errMsg)
	if rErr := d.store.RecordTriggerFire(ctx, fire.TriggerID, finishedAt, status, errMsg); rErr != nil {
		if !errors.Is(rErr, store.ErrNotFound) {
			d.logger.Warn("trigger: failed to update trigger counters for waiting fire",
				"triggerID", fire.TriggerID, "fireID", fire.ID, "error", rErr)
		}
	}
	if d.fireAudit != nil {
		d.fireAudit.RecordFire(ctx, FireAuditRecord{
			ProjectID:      fire.ProjectID,
			TriggerID:      fire.TriggerID,
			EventID:        fire.EventID,
			FireID:         fire.ID,
			ActionType:     fire.ActionType,
			Status:         status,
			MatchedThrough: fire.MatchedThrough,
			DecidedDialect: fire.DecidedDialect,
		})
	}
}

func (d *Dispatcher) parkWaitingFire(
	ctx context.Context,
	tracker *triggerFireTracker,
	tr *store.Trigger,
	ev *store.TriggerEvent,
	fire *store.TriggerFire,
	out hubfire.FireOutcome,
	finishedAt time.Time,
) error {
	if d.queue == nil || out.TargetAgentID == "" {
		// Defensive fallback when no queue or target agent ID is available.
		var (
			finished  bool
			finishErr error
		)
		tracker.record(func() {
			finished, finishErr = d.store.FinishTriggerFire(ctx, fire.ID, store.TriggerFireStatusSkipped, "", out.TargetAgentID, ErrFireTargetNotRunning, finishedAt)
		})
		if finishErr != nil {
			return finishErr
		}
		if finished {
			d.recordFireMetric(tr.ActionType, store.TriggerFireStatusSkipped, ErrFireTargetNotRunning)
			_ = d.store.RecordTriggerFire(ctx, tr.ID, finishedAt, store.TriggerFireStatusSkipped, ErrFireTargetNotRunning)
			if d.fireAudit != nil {
				d.fireAudit.RecordFire(ctx, FireAuditRecord{
					ProjectID:      tr.ProjectID,
					TriggerID:      tr.ID,
					EventID:        ev.ID,
					FireID:         fire.ID,
					ActionType:     tr.ActionType,
					Status:         store.TriggerFireStatusSkipped,
					MatchedThrough: fire.MatchedThrough,
					DecidedDialect: fire.DecidedDialect,
					Reason:         FireSkipReasonTargetNotRunning,
				})
			}
		}
		return nil
	}

	ttl := extractMessageQueueTTL(tr.ActionPayload)
	waitUntil := ev.ReceivedAt.UTC().Add(time.Duration(ttl) * time.Second)
	notBefore := finishedAt.Add(FireRetryInitialBackoff)
	if notBefore.After(waitUntil) {
		notBefore = waitUntil
	}
	payloadBytes, err := json.Marshal(FireRetryQueuePayload{
		Version: FireRetryQueuePayloadVersion,
		FireID:  fire.ID,
	})
	if err != nil {
		return err
	}

	var (
		parked      bool
		capExceeded bool
		parkErr     error
	)
	runPark := func(runCtx context.Context, tx store.Tx, ts store.TriggerStore) error {
		p, capHit, pErr := ts.ParkTriggerFireWaiting(
			runCtx,
			fire.ID,
			tr.ProjectID,
			out.TargetAgentID,
			out.Error,
			waitUntil,
			MaxWaitingFiresPerTarget,
			MaxWaitingFiresPerProject,
		)
		if pErr != nil {
			return pErr
		}
		capExceeded = capHit
		if capHit || !p {
			return nil
		}
		_, qErr := d.queue.Enqueue(runCtx, tx, durablequeue.Item{
			Kind:      FireRetryQueueKind,
			DedupKey:  FireRetryDedupPrefix + fire.ID,
			ProjectID: tr.ProjectID,
			Payload:   payloadBytes,
			NotBefore: notBefore,
		})
		if qErr != nil && !errors.Is(qErr, durablequeue.ErrDuplicate) {
			return qErr
		}
		parked = true
		return nil
	}

	tracker.record(func() {
		if d.txRunner != nil {
			parkErr = d.txRunner(ctx, runPark)
		} else {
			parkErr = runPark(ctx, nil, d.store)
		}
	})
	if parkErr != nil {
		return parkErr
	}

	if capExceeded {
		finished, fErr := d.store.FinishTriggerFire(ctx, fire.ID, store.TriggerFireStatusSkipped, "", out.TargetAgentID, ErrFireOfflineQueueFull, finishedAt)
		if fErr != nil {
			return fErr
		}
		if finished {
			d.recordFireMetric(tr.ActionType, store.TriggerFireStatusSkipped, ErrFireOfflineQueueFull)
			if rErr := d.store.RecordTriggerFire(ctx, tr.ID, finishedAt, store.TriggerFireStatusSkipped, ErrFireOfflineQueueFull); rErr != nil {
				d.logger.Warn("trigger: failed to update trigger fire counters on offline queue cap",
					"triggerID", tr.ID, "fireID", fire.ID, "error", rErr)
			}
			if d.fireAudit != nil {
				d.fireAudit.RecordFire(ctx, FireAuditRecord{
					ProjectID:      tr.ProjectID,
					TriggerID:      tr.ID,
					EventID:        ev.ID,
					FireID:         fire.ID,
					ActionType:     tr.ActionType,
					Status:         store.TriggerFireStatusSkipped,
					MatchedThrough: fire.MatchedThrough,
					DecidedDialect: fire.DecidedDialect,
					Reason:         FireSkipReasonOfflineQueueFull,
				})
			}
		}
		return nil
	}

	if parked {
		if !notBefore.After(finishedAt) {
			d.queue.Wake(FireRetryQueueKind)
		}
		if d.fireAudit != nil {
			d.fireAudit.RecordFire(ctx, FireAuditRecord{
				ProjectID:      tr.ProjectID,
				TriggerID:      tr.ID,
				EventID:        ev.ID,
				FireID:         fire.ID,
				ActionType:     tr.ActionType,
				Status:         store.TriggerFireStatusWaiting,
				MatchedThrough: fire.MatchedThrough,
				DecidedDialect: fire.DecidedDialect,
			})
		}
	}
	return nil
}

func extractMessageQueueTTL(rawPayload string) int {
	var partial struct {
		QueueTTLSeconds int `json:"queueTtlSeconds"`
	}
	if rawPayload != "" {
		_ = json.Unmarshal([]byte(rawPayload), &partial)
	}
	if partial.QueueTTLSeconds <= 0 {
		return store.DefaultMessageQueueTTLSeconds
	}
	if partial.QueueTTLSeconds < store.MinMessageQueueTTLSeconds {
		return store.MinMessageQueueTTLSeconds
	}
	if partial.QueueTTLSeconds > store.MaxMessageQueueTTLSeconds {
		return store.MaxMessageQueueTTLSeconds
	}
	return partial.QueueTTLSeconds
}
