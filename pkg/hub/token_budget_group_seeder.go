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
)

// GroupBudgetSeedPayload is the JSON payload persisted in durable_queue_items
// for usage.group_budget_seed.
type GroupBudgetSeedPayload struct {
	GroupID        string    `json:"group_id"`
	ProjectScopeID string    `json:"project_scope_id"`
	WindowKind     string    `json:"window_kind"` // "day" | "month"
	WindowStartUTC time.Time `json:"window_start_utc"`
	WindowEndUTC   time.Time `json:"window_end_utc"`
	ActivatedAt    time.Time `json:"activated_at"`
	IsFinalPass    bool      `json:"is_final_pass"`
}

func groupWindowForLimit(limitName string, ts time.Time) (windowKind string, start, end time.Time, ok bool) {
	switch limitName {
	case store.LimitLLMTokensPerGroupDay:
		s, e := utcDayWindow(ts)
		return store.UsageBudgetWindowDay, s, e, true
	case store.LimitLLMTokensPerGroupMonth:
		s, e := utcMonthWindow(ts)
		return store.UsageBudgetWindowMonth, s, e, true
	default:
		return "", time.Time{}, time.Time{}, false
	}
}

// GroupBudgetSeeder orchestrates D6 mid-window seeding for pooled user-group
// token budgets via durablequeue and per-key usage_group_budget_seeds records.
//
// D6 invariants:
//   - Activation is recorded explicitly on binding create (value > 0), update
//     (0 -> >0), or scope/subject change — NEVER inferred from binding.UpdatedAt.
//   - Each (group, project_scope, window_kind, window_start_utc) has its own
//     usage_group_budget_seeds row, updated atomically with compute + replace
//     under a per-key advisory lock/tx so multi-replica seeders never drop or
//     overwrite each other's records.
//   - Effective members are snapshotted once on the first seed run; once the
//     +2m pass finalizes (guarded by activatedAt matching the record), the seed
//     is never recomputed from later membership ("leaving never refunds; joining
//     charges only future spend").
//   - Nightly reconcile only enqueues incomplete seed records and bindings
//     whose CreatedAt fell inside the current window with no seed record at all.
type GroupBudgetSeeder struct {
	store           store.Store
	queue           durablequeue.Queue
	evaluator       *TokenBudgetEvaluator
	logger          *slog.Logger
	enabledFn       func() bool
	retentionDaysFn func() int
	nowFn           func() time.Time
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

// EnqueueForBindingActivation records an explicit binding activation in
// usage_group_budget_seeds and enqueues both the immediate seed pass and the
// +2m TTL-cover seed pass. Called on pooled binding create (value > 0), update
// (0 -> >0), or scope/subject change.
func (gs *GroupBudgetSeeder) EnqueueForBindingActivation(
	ctx context.Context,
	limitName, groupID, scopeType, scopeID string,
	activatedAt time.Time,
) error {
	if !gs.enabled() || !pooledGroupLimit(limitName) || groupID == "" {
		return nil
	}
	if activatedAt.IsZero() {
		activatedAt = gs.now()
	}
	activatedAt = activatedAt.UTC().Truncate(time.Microsecond)

	winKind, winStart, winEnd, ok := groupWindowForLimit(limitName, activatedAt)
	if !ok {
		return nil
	}

	projectScopeID := ""
	if scopeType == store.QuotaScopeProject {
		projectScopeID = scopeID
	}

	// Persist the explicit activation record before enqueueing so nightly
	// reconcile can recover even if enqueue fails or the queue item is lost,
	// without ever inferring activation from binding.UpdatedAt (R1).
	rec, err := gs.store.RecordUsageGroupSeedActivation(ctx, &store.UsageGroupSeedRecord{
		GroupID:        groupID,
		ProjectScopeID: projectScopeID,
		WindowKind:     winKind,
		WindowStartUTC: winStart,
		WindowEndUTC:   winEnd,
		ActivatedAt:    activatedAt,
		CutoffAt:       activatedAt.Add(groupBudgetSeedTTLCoverDelay),
	})
	if err != nil {
		return fmt.Errorf("record group budget seed activation: %w", err)
	}
	if gs.queue == nil {
		return nil
	}

	effActivatedAt := rec.ActivatedAt.UTC()
	passes := []struct {
		tag       string
		notBefore time.Time
		finalPass bool
	}{
		{tag: "initial", notBefore: effActivatedAt, finalPass: false},
		{tag: "ttl_cover", notBefore: effActivatedAt.Add(groupBudgetSeedTTLCoverDelay), finalPass: true},
	}

	for _, p := range passes {
		payload := GroupBudgetSeedPayload{
			GroupID:        groupID,
			ProjectScopeID: projectScopeID,
			WindowKind:     winKind,
			WindowStartUTC: winStart,
			WindowEndUTC:   winEnd,
			ActivatedAt:    effActivatedAt,
			IsFinalPass:    p.finalPass,
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("marshal group budget seed payload: %w", err)
		}
		dedupKey := fmt.Sprintf("%s:%s:%s:%s:act-%d:%s",
			groupID, projectScopeID, winKind, winStart.Format("2006-01-02"), effActivatedAt.UnixMicro(), p.tag)
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

// RunSeed executes one idempotent, atomically-locked D6 seed pass for payload.
func (gs *GroupBudgetSeeder) RunSeed(ctx context.Context, payload GroupBudgetSeedPayload) error {
	if !gs.enabled() {
		return nil
	}
	if payload.GroupID == "" || !payload.WindowEndUTC.After(payload.WindowStartUTC) {
		return fmt.Errorf("%w: invalid group budget seed payload", store.ErrInvalidInput)
	}
	winKind := payload.WindowKind
	if winKind == "" {
		if payload.WindowEndUTC.Sub(payload.WindowStartUTC) <= 25*time.Hour {
			winKind = store.UsageBudgetWindowDay
		} else {
			winKind = store.UsageBudgetWindowMonth
		}
	}
	now := gs.now().Truncate(time.Microsecond)
	activatedAt := payload.ActivatedAt.UTC().Truncate(time.Microsecond)
	if activatedAt.IsZero() {
		activatedAt = now
	}

	_, didMutate, err := gs.store.ExecuteUsageGroupBudgetSeedPass(ctx, store.UsageGroupSeedPassInput{
		GroupID:               payload.GroupID,
		ProjectScopeID:        payload.ProjectScopeID,
		WindowKind:            winKind,
		WindowStartUTC:        payload.WindowStartUTC.UTC(),
		WindowEndUTC:          payload.WindowEndUTC.UTC(),
		ActivatedAt:           activatedAt,
		CutoffAt:              activatedAt.Add(groupBudgetSeedTTLCoverDelay),
		LedgerRetentionCutoff: gs.ledgerRetentionCutoff(now),
		Now:                   now,
		IsFinalPass:           payload.IsFinalPass,
	})
	if err != nil {
		return err
	}

	// Re-evaluate group budget state immediately when the seed mutated rollups
	// so newly seeded spend is reflected in usage_budget_state without waiting
	// for the next ingest.
	if didMutate && gs.evaluator != nil {
		if evalErr := gs.evaluator.EvaluateGroupScope(ctx, payload.GroupID, payload.ProjectScopeID, now); evalErr != nil {
			gs.logger.Warn("post-seed group budget evaluation failed",
				"group_id", payload.GroupID, "project_scope_id", payload.ProjectScopeID, "error", evalErr)
		}
	}
	return nil
}

// ReconcileMissingSeeds enqueues seed items ONLY for:
//  1. Existing non-finalized usage_group_budget_seeds records whose window is
//     still open (recovering lost queue items).
//  2. Active pooled bindings whose CreatedAt fell inside the current window and
//     have NO usage_group_budget_seeds record at all.
//
// It NEVER inspects binding.UpdatedAt and NEVER re-enqueues a finalized seed
// (R1, D6).
func (gs *GroupBudgetSeeder) ReconcileMissingSeeds(ctx context.Context) (int, error) {
	if !gs.enabled() || gs.queue == nil {
		return 0, nil
	}
	now := gs.now().Truncate(time.Microsecond)
	enqueued := 0

	enqueueRec := func(rec *store.UsageGroupSeedRecord) error {
		notBefore := now
		isFinal := !now.Before(rec.CutoffAt)
		if !isFinal {
			notBefore = rec.CutoffAt
			isFinal = true
		}
		payload := GroupBudgetSeedPayload{
			GroupID:        rec.GroupID,
			ProjectScopeID: rec.ProjectScopeID,
			WindowKind:     rec.WindowKind,
			WindowStartUTC: rec.WindowStartUTC,
			WindowEndUTC:   rec.WindowEndUTC,
			ActivatedAt:    rec.ActivatedAt,
			IsFinalPass:    isFinal,
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		dedupKey := fmt.Sprintf("%s:%s:%s:%s:act-%d:reconcile-%s",
			rec.GroupID, rec.ProjectScopeID, rec.WindowKind,
			rec.WindowStartUTC.Format("2006-01-02"), rec.ActivatedAt.UnixMicro(), now.Format("2006-01-02"))
		_, err = gs.queue.Enqueue(ctx, nil, durablequeue.Item{
			Kind:      usageGroupBudgetSeedQueueKind,
			DedupKey:  dedupKey,
			ProjectID: rec.ProjectScopeID,
			Payload:   raw,
			NotBefore: notBefore,
		})
		if err != nil {
			if errors.Is(err, durablequeue.ErrDuplicate) {
				return nil
			}
			return err
		}
		enqueued++
		return nil
	}

	// 1. Recover any existing incomplete seed records for open windows.
	incomplete, err := gs.store.ListIncompleteUsageGroupSeeds(ctx, now)
	if err != nil {
		return 0, fmt.Errorf("list incomplete group seeds: %w", err)
	}
	for _, rec := range incomplete {
		if err := enqueueRec(rec); err != nil {
			return enqueued, err
		}
	}

	// 2. Recover any active pooled bindings created in the current window that
	//    have no seed record at all (e.g. direct store creation or failed
	//    activation recording). Never compare against binding.UpdatedAt.
	limitWindows := []struct {
		name  string
		kind  string
		start time.Time
		end   time.Time
	}{
		{name: store.LimitLLMTokensPerGroupDay, kind: store.UsageBudgetWindowDay},
		{name: store.LimitLLMTokensPerGroupMonth, kind: store.UsageBudgetWindowMonth},
	}
	limitWindows[0].start, limitWindows[0].end = utcDayWindow(now)
	limitWindows[1].start, limitWindows[1].end = utcMonthWindow(now)

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
			createdAt := b.CreatedAt.UTC().Truncate(time.Microsecond)
			// New windows after binding creation need no seed (D6).
			if createdAt.Before(lw.start) || !createdAt.Before(lw.end) {
				continue
			}
			projectScopeID := ""
			if b.ScopeType == store.QuotaScopeProject {
				projectScopeID = b.ScopeID
			}
			_, err := gs.store.GetUsageGroupSeedRecord(ctx, b.SubjectID, projectScopeID, lw.kind, lw.start)
			if err == nil {
				// Record exists (either already finalized, or already handled in step 1).
				continue
			}
			if !errors.Is(err, store.ErrNotFound) {
				return enqueued, err
			}
			rec, err := gs.store.RecordUsageGroupSeedActivation(ctx, &store.UsageGroupSeedRecord{
				GroupID:        b.SubjectID,
				ProjectScopeID: projectScopeID,
				WindowKind:     lw.kind,
				WindowStartUTC: lw.start,
				WindowEndUTC:   lw.end,
				ActivatedAt:    createdAt,
				CutoffAt:       createdAt.Add(groupBudgetSeedTTLCoverDelay),
			})
			if err != nil {
				return enqueued, err
			}
			if !rec.Finalized {
				if err := enqueueRec(rec); err != nil {
					return enqueued, err
				}
			}
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
