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

package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/durablequeue"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const (
	// usageGroupBudgetSeedQueueKind is the durablequeue kind for pooled-group
	// mid-window seeding (Tokenomics P3.3, §4.3, D6).
	usageGroupBudgetSeedQueueKind = "usage.group_budget_seed"

	// groupBudgetSeedTTLCoverDelay is the +2m delay for the second seed pass
	// covering peer Hub replicas whose 60s pooled-group cache may still be
	// stale immediately after binding activation (D6).
	groupBudgetSeedTTLCoverDelay = 2 * time.Minute

	// usageGroupSeedReconcileJobName is the recurring singleton job name for
	// filling missing/incomplete seeds.
	usageGroupSeedReconcileJobName = "usage-group-seed-reconcile"

	// usageGroupSeedRecordRetainDays bounds how long expired seed records stay
	// in hub_settings before pruning.
	usageGroupSeedRecordRetainDays = 60
)

// GroupBudgetSeedPayload is the JSON payload persisted in durable_queue_items
// for usage.group_budget_seed.
type GroupBudgetSeedPayload struct {
	GroupID        string    `json:"group_id"`
	ProjectScopeID string    `json:"project_scope_id"`
	WindowStartUTC time.Time `json:"window_start_utc"`
	WindowEndUTC   time.Time `json:"window_end_utc"`
	ActivatedAt    time.Time `json:"activated_at"`
	IsFinalPass    bool      `json:"is_final_pass"`
}

func groupSeedRecordKey(groupID, projectScopeID string, windowStartUTC time.Time) string {
	return groupID + "|" + projectScopeID + "|" + windowStartUTC.UTC().Format("2006-01-02")
}

// GroupBudgetSeeder orchestrates D6 mid-window seeding for pooled user-group
// token budgets via durablequeue.
//
// D6 invariants:
//   - A seed covers ONLY spend that predates activation: uncharged events with
//     event_ts in [max(window_start, ledger_retention_cutoff), min(window_end, activated_at + 2m))
//     by users who were effective members at the first seed run.
//   - The immediate pass and +2m pass compute the exact same bounded set via
//     idempotent SET (ReplaceUsageGroupRollupSeed), never touching origin='ingest'.
//   - Once the +2m pass completes (or a pass runs at/after activated_at+2m), the
//     seed record is marked Finalized=true and is NEVER recomputed from later
//     membership ("leaving never refunds; joining charges only future spend").
//   - Nightly reconcile only enqueues missing/incomplete seeds for active
//     bindings whose activation fell inside the current window; new windows
//     after activation need no seed.
type GroupBudgetSeeder struct {
	store           store.Store
	queue           durablequeue.Queue
	evaluator       *TokenBudgetEvaluator
	logger          *slog.Logger
	enabledFn       func() bool
	retentionDaysFn func() int
	nowFn           func() time.Time
	stateMu         sync.Mutex // serializes in-process seed state updates
}

// NewGroupBudgetSeeder constructs a GroupBudgetSeeder.
func NewGroupBudgetSeeder(
	st store.Store,
	q durablequeue.Queue,
	eval *TokenBudgetEvaluator,
	logger *slog.Logger,
	enabledFn func() bool,
	retentionDaysFn func() int,
) *GroupBudgetSeeder {
	if logger == nil {
		logger = slog.Default()
	}
	return &GroupBudgetSeeder{
		store:           st,
		queue:           q,
		evaluator:       eval,
		logger:          logger,
		enabledFn:       enabledFn,
		retentionDaysFn: retentionDaysFn,
		nowFn:           func() time.Time { return time.Now().UTC() },
	}
}

func (gs *GroupBudgetSeeder) enabled() bool {
	if gs == nil || gs.store == nil {
		return false
	}
	if gs.enabledFn == nil {
		return true
	}
	return gs.enabledFn()
}

func (gs *GroupBudgetSeeder) now() time.Time {
	if gs != nil && gs.nowFn != nil {
		return gs.nowFn().UTC()
	}
	return time.Now().UTC()
}

func (gs *GroupBudgetSeeder) ledgerRetentionCutoff(now time.Time) time.Time {
	if gs == nil || gs.retentionDaysFn == nil {
		return time.Time{}
	}
	days := gs.retentionDaysFn()
	if days <= 0 {
		return time.Time{}
	}
	return now.UTC().Add(-time.Duration(days) * 24 * time.Hour)
}

// EnqueueForBindingActivation enqueues both the immediate seed pass and the +2m
// TTL-cover seed pass for a newly activated pooled group binding (created with
// value > 0, or updated 0 -> >0, or scope changed). Safe to call when the
// durable queue is nil (logs and returns).
func (gs *GroupBudgetSeeder) EnqueueForBindingActivation(
	ctx context.Context,
	limitName, groupID, scopeType, scopeID string,
	activatedAt time.Time,
) error {
	if !gs.enabled() || gs.queue == nil || !pooledGroupLimit(limitName) || groupID == "" {
		return nil
	}
	if activatedAt.IsZero() {
		activatedAt = gs.now()
	} else {
		activatedAt = activatedAt.UTC().Truncate(time.Microsecond)
	}

	var winStart, winEnd time.Time
	switch limitName {
	case store.LimitLLMTokensPerGroupDay:
		winStart, winEnd = utcDayWindow(activatedAt)
	case store.LimitLLMTokensPerGroupMonth:
		winStart, winEnd = utcMonthWindow(activatedAt)
	default:
		return nil
	}

	projectScopeID := ""
	if scopeType == store.QuotaScopeProject {
		projectScopeID = scopeID
	}

	passes := []struct {
		tag       string
		notBefore time.Time
		finalPass bool
	}{
		{tag: "initial", notBefore: activatedAt, finalPass: false},
		{tag: "ttl_cover", notBefore: activatedAt.Add(groupBudgetSeedTTLCoverDelay), finalPass: true},
	}

	for _, p := range passes {
		payload := GroupBudgetSeedPayload{
			GroupID:        groupID,
			ProjectScopeID: projectScopeID,
			WindowStartUTC: winStart,
			WindowEndUTC:   winEnd,
			ActivatedAt:    activatedAt,
			IsFinalPass:    p.finalPass,
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("marshal group budget seed payload: %w", err)
		}
		dedupKey := fmt.Sprintf("%s:%s:%s:act-%d:%s",
			groupID, projectScopeID, winStart.Format("2006-01-02"), activatedAt.Unix(), p.tag)
		_, err = gs.queue.Enqueue(ctx, nil, durablequeue.Item{
			Kind:      usageGroupBudgetSeedQueueKind,
			DedupKey:  dedupKey,
			ProjectID: projectScopeID,
			Payload:   raw,
			NotBefore: p.notBefore,
		})
		if err != nil && !errors.Is(err, durablequeue.ErrDuplicate) {
			return fmt.Errorf("enqueue group budget seed (%s): %w", p.tag, err)
		}
	}
	gs.queue.Wake(usageGroupBudgetSeedQueueKind)
	return nil
}

// HandleQueueItem implements durablequeue.Handler for usage.group_budget_seed.
func (gs *GroupBudgetSeeder) HandleQueueItem(ctx context.Context, item durablequeue.Item, _ durablequeue.Lease) durablequeue.Result {
	if !gs.enabled() {
		return durablequeue.Result{Done: true}
	}
	var payload GroupBudgetSeedPayload
	if err := json.Unmarshal(item.Payload, &payload); err != nil {
		return durablequeue.Result{Permanent: true, Err: fmt.Errorf("unmarshal seed payload: %w", err)}
	}
	if err := gs.RunSeed(ctx, payload); err != nil {
		return durablequeue.Result{Err: err}
	}
	return durablequeue.Result{Done: true}
}

// RunSeed executes one idempotent D6 seed pass for payload.
func (gs *GroupBudgetSeeder) RunSeed(ctx context.Context, payload GroupBudgetSeedPayload) error {
	if !gs.enabled() {
		return nil
	}
	if payload.GroupID == "" || !payload.WindowEndUTC.After(payload.WindowStartUTC) {
		return fmt.Errorf("%w: invalid group budget seed payload", store.ErrInvalidInput)
	}
	now := gs.now()
	winStart := payload.WindowStartUTC.UTC()
	winEnd := payload.WindowEndUTC.UTC()
	activatedAt := payload.ActivatedAt.UTC().Truncate(time.Microsecond)
	if activatedAt.IsZero() {
		activatedAt = now.Truncate(time.Microsecond)
	}
	cutoffAt := activatedAt.Add(groupBudgetSeedTTLCoverDelay)
	key := groupSeedRecordKey(payload.GroupID, payload.ProjectScopeID, winStart)

	gs.stateMu.Lock()
	defer gs.stateMu.Unlock()

	state, err := gs.store.GetUsageGroupSeedState(ctx)
	if err != nil {
		return fmt.Errorf("load group seed state: %w", err)
	}
	if state.Records == nil {
		state.Records = make(map[string]*store.UsageGroupSeedRecord)
	}

	rec := state.Records[key]
	// D6 finality: once finalized for this activation (or a newer one), never
	// recompute from later membership.
	if rec != nil && rec.Finalized && !activatedAt.After(rec.ActivatedAt) {
		return nil
	}

	if rec == nil || activatedAt.After(rec.ActivatedAt) {
		rec = &store.UsageGroupSeedRecord{
			GroupID:        payload.GroupID,
			ProjectScopeID: payload.ProjectScopeID,
			WindowStartUTC: winStart,
			WindowEndUTC:   winEnd,
			ActivatedAt:    activatedAt,
			CutoffAt:       cutoffAt,
		}
		state.Records[key] = rec
	}

	// Snapshot effective members on the first seed run only (D6).
	if !rec.MemberSnapshotSet {
		members, err := gs.store.GetEffectiveGroupMemberUserIDs(ctx, payload.GroupID)
		if err != nil {
			return fmt.Errorf("resolve effective members for group %s: %w", payload.GroupID, err)
		}
		rec.FrozenMemberIDs = members
		rec.MemberSnapshotSet = true
	}

	// Bound seed scan to [max(winStart, ledgerRetentionCutoff), min(winEnd, cutoffAt)).
	fromTS := winStart
	if retCutoff := gs.ledgerRetentionCutoff(now); !retCutoff.IsZero() && retCutoff.After(fromTS) {
		fromTS = retCutoff
	}
	toTS := rec.CutoffAt
	if winEnd.Before(toTS) {
		toTS = winEnd
	}

	var days []store.UsageGroupRollupDay
	if len(rec.FrozenMemberIDs) > 0 && toTS.After(fromTS) {
		days, err = gs.store.ComputeUnchargedGroupRollupSeed(
			ctx, payload.GroupID, payload.ProjectScopeID, rec.FrozenMemberIDs, fromTS, toTS,
		)
		if err != nil {
			return fmt.Errorf("compute uncharged group seed: %w", err)
		}
	}

	if err := gs.store.ReplaceUsageGroupRollupSeed(
		ctx, payload.GroupID, payload.ProjectScopeID, winStart, winEnd, days,
	); err != nil {
		return fmt.Errorf("replace group rollup seed: %w", err)
	}

	if payload.IsFinalPass || !now.Before(rec.CutoffAt) {
		rec.Finalized = true
	}
	rec.UpdatedAt = now

	// Prune expired seed records well past window end.
	pruneBefore := now.AddDate(0, 0, -usageGroupSeedRecordRetainDays)
	for k, r := range state.Records {
		if r == nil || r.WindowEndUTC.Before(pruneBefore) {
			delete(state.Records, k)
		}
	}

	if err := gs.store.SaveUsageGroupSeedState(ctx, state); err != nil {
		return fmt.Errorf("save group seed state: %w", err)
	}

	// Re-evaluate group budget state immediately so newly seeded spend is
	// reflected in usage_budget_state without waiting for the next ingest.
	if gs.evaluator != nil {
		if evalErr := gs.evaluator.EvaluateGroupScope(ctx, payload.GroupID, payload.ProjectScopeID, now); evalErr != nil {
			gs.logger.Warn("post-seed group budget evaluation failed",
				"group_id", payload.GroupID, "project_scope_id", payload.ProjectScopeID, "error", evalErr)
		}
	}
	return nil
}

// ReconcileMissingSeeds scans active pooled group bindings and enqueues seed
// items ONLY for bindings whose activation fell inside the current window and
// whose D6 seed record is missing or not yet finalized (D6).
func (gs *GroupBudgetSeeder) ReconcileMissingSeeds(ctx context.Context) (int, error) {
	if !gs.enabled() || gs.queue == nil {
		return 0, nil
	}
	now := gs.now()
	state, err := gs.store.GetUsageGroupSeedState(ctx)
	if err != nil {
		return 0, fmt.Errorf("load group seed state: %w", err)
	}

	limitWindows := []struct {
		name  string
		start time.Time
		end   time.Time
	}{
		{name: store.LimitLLMTokensPerGroupDay},
		{name: store.LimitLLMTokensPerGroupMonth},
	}
	limitWindows[0].start, limitWindows[0].end = utcDayWindow(now)
	limitWindows[1].start, limitWindows[1].end = utcMonthWindow(now)

	enqueued := 0
	for _, lw := range limitWindows {
		def, err := gs.store.GetLimitDefinitionByName(ctx, lw.name)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			return enqueued, fmt.Errorf("lookup limit definition %q: %w", lw.name, err)
		}
		if def == nil {
			continue
		}
		bindings, err := gs.store.ListEntitlementBindings(ctx, def.ID)
		if err != nil {
			return enqueued, fmt.Errorf("list bindings for %q: %w", lw.name, err)
		}
		for _, b := range bindings {
			if b == nil || b.SubjectType != store.EntitlementSubjectGroup || b.SubjectID == "" || b.Value <= 0 {
				continue
			}
			activatedAt := b.UpdatedAt.UTC().Truncate(time.Microsecond)
			if activatedAt.IsZero() {
				activatedAt = b.CreatedAt.UTC().Truncate(time.Microsecond)
			}
			// New windows after activation need no seed (D6).
			if activatedAt.Before(lw.start) || !activatedAt.Before(lw.end) {
				continue
			}
			projectScopeID := ""
			if b.ScopeType == store.QuotaScopeProject {
				projectScopeID = b.ScopeID
			}
			key := groupSeedRecordKey(b.SubjectID, projectScopeID, lw.start)
			if rec := state.Records[key]; rec != nil && rec.Finalized && !activatedAt.After(rec.ActivatedAt) {
				continue
			}

			cutoffAt := activatedAt.Add(groupBudgetSeedTTLCoverDelay)
			notBefore := now
			isFinal := !now.Before(cutoffAt)
			if !isFinal {
				notBefore = cutoffAt
				isFinal = true
			}
			payload := GroupBudgetSeedPayload{
				GroupID:        b.SubjectID,
				ProjectScopeID: projectScopeID,
				WindowStartUTC: lw.start,
				WindowEndUTC:   lw.end,
				ActivatedAt:    activatedAt,
				IsFinalPass:    isFinal,
			}
			raw, err := json.Marshal(payload)
			if err != nil {
				return enqueued, err
			}
			dedupKey := fmt.Sprintf("%s:%s:%s:reconcile-%s",
				b.SubjectID, projectScopeID, lw.start.Format("2006-01-02"), now.Format("2006-01-02"))
			_, err = gs.queue.Enqueue(ctx, nil, durablequeue.Item{
				Kind:      usageGroupBudgetSeedQueueKind,
				DedupKey:  dedupKey,
				ProjectID: projectScopeID,
				Payload:   raw,
				NotBefore: notBefore,
			})
			if err != nil {
				if errors.Is(err, durablequeue.ErrDuplicate) {
					continue
				}
				return enqueued, err
			}
			enqueued++
		}
	}
	if enqueued > 0 {
		gs.queue.Wake(usageGroupBudgetSeedQueueKind)
	}
	return enqueued, nil
}

// GroupBudgetSeeder returns (lazily constructing on first call) the Hub's
// pooled-group budget seeder.
func (s *Server) GroupBudgetSeeder() *GroupBudgetSeeder {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	seeder := s.groupBudgetSeeder
	s.mu.RUnlock()
	if seeder != nil {
		return seeder
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.groupBudgetSeeder == nil && s.store != nil {
		var q durablequeue.Queue
		if s.durableQueue != nil {
			q = s.durableQueue
		}
		s.groupBudgetSeeder = NewGroupBudgetSeeder(
			s.store,
			q,
			s.tokenBudgetEvaluator,
			slog.Default(),
			func() bool {
				return s.experimentEnabled(experiments.TokenomicsUsageLedger) &&
					s.experimentEnabled(experiments.TokenomicsTokenBudgets)
			},
			func() int {
				return s.usageRetentionConfig().UsageEventsDays
			},
		)
	}
	// Ensure evaluator is attached if initialized lazily afterwards.
	if s.groupBudgetSeeder != nil && s.groupBudgetSeeder.evaluator == nil && s.tokenBudgetEvaluator != nil {
		s.groupBudgetSeeder.evaluator = s.tokenBudgetEvaluator
	}
	return s.groupBudgetSeeder
}

// NewGroupBudgetSeedDrainer constructs the durablequeue.Drainer for
// usage.group_budget_seed backed by Server's shared StoreQueue.
func (s *Server) NewGroupBudgetSeedDrainer() (*durablequeue.Drainer, error) {
	if s == nil || s.store == nil || s.durableQueue == nil {
		return nil, nil
	}
	dqs, ok := s.store.(store.DurableQueueStore)
	if !ok {
		return nil, nil
	}
	// Ensure evaluator is constructed before seeder so post-seed evaluation runs.
	_ = s.TokenBudgetEvaluator()
	seeder := s.GroupBudgetSeeder()
	if seeder == nil {
		return nil, nil
	}
	return durablequeue.NewDrainer(durablequeue.DrainerConfig{
		Store:          dqs,
		Queue:          s.durableQueue,
		Kind:           usageGroupBudgetSeedQueueKind,
		Handler:        seeder.HandleQueueItem,
		Owner:          s.config.HubID,
		Enabled:        seeder.enabled,
		PollInterval:   5 * time.Second,
		LeaseDuration:  60 * time.Second,
		HandlerTimeout: 45 * time.Second,
		BatchSize:      10,
	})
}

// usageGroupSeedReconcileHandler returns the recurring singleton handler that
// enqueues any missing/incomplete pooled-group seeds (D6).
func (s *Server) usageGroupSeedReconcileHandler() func(ctx context.Context) {
	return func(ctx context.Context) {
		if !s.experimentEnabled(experiments.TokenomicsUsageLedger) ||
			!s.experimentEnabled(experiments.TokenomicsTokenBudgets) {
			return
		}
		_ = s.TokenBudgetEvaluator()
		seeder := s.GroupBudgetSeeder()
		if seeder == nil {
			return
		}
		n, err := seeder.ReconcileMissingSeeds(ctx)
		if err != nil {
			slog.Warn("pooled group budget seed reconcile failed", "error", err)
			return
		}
		if n > 0 {
			slog.Info("pooled group budget seed reconcile enqueued missing seeds", "count", n)
		}
	}
}

// startGroupBudgetSeedDrainer lazily constructs and starts the background
// durablequeue.Drainer for usage.group_budget_seed when both
// tokenomics.usage_ledger and tokenomics.token_budgets are enabled.
func (s *Server) startGroupBudgetSeedDrainer(ctx context.Context) {
	if s == nil || s.durableQueue == nil {
		return
	}
	if !s.experimentEnabled(experiments.TokenomicsUsageLedger) ||
		!s.experimentEnabled(experiments.TokenomicsTokenBudgets) {
		return
	}
	s.mu.Lock()
	if s.groupBudgetSeedDrainer != nil {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()

	drainer, err := s.NewGroupBudgetSeedDrainer()
	if err != nil {
		slog.Warn("failed to construct pooled group budget seed drainer", "error", err)
		return
	}
	if drainer == nil {
		return
	}

	s.mu.Lock()
	if s.groupBudgetSeedDrainer != nil {
		s.mu.Unlock()
		return
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	s.groupBudgetSeedDrainer = drainer
	s.groupBudgetSeedCancel = cancel
	s.groupBudgetSeedDone = done
	s.mu.Unlock()

	go func() {
		defer close(done)
		drainer.Run(runCtx)
	}()
}

// stopGroupBudgetSeedDrainer stops the background usage.group_budget_seed
// drainer if it was started and waits for it to exit within ctx.
func (s *Server) stopGroupBudgetSeedDrainer(ctx context.Context) {
	if s == nil {
		return
	}
	s.mu.Lock()
	cancel := s.groupBudgetSeedCancel
	done := s.groupBudgetSeedDone
	s.groupBudgetSeedDrainer = nil
	s.groupBudgetSeedCancel = nil
	s.groupBudgetSeedDone = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
		}
	}
}
