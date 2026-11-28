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
	// charging-episode seeding (Tokenomics P3.3, §4.3, D6a).
	usageGroupBudgetSeedQueueKind = "usage.group_budget_seed"

	// usageGroupSeedReconcileJobName is the recurring singleton job name for
	// completing missing/incomplete episode seeds.
	usageGroupSeedReconcileJobName = "usage-group-seed-reconcile"
)

// GroupBudgetSeedPayload is the JSON payload persisted in durable_queue_items
// for usage.group_budget_seed.
type GroupBudgetSeedPayload struct {
	EpisodeID   string `json:"episode_id"`
	GroupID     string `json:"group_id"`
	IsFinalPass bool   `json:"is_final_pass"`
}

// GroupBudgetSeeder orchestrates D6a charging-episode seeding for pooled
// user-group token budgets via durablequeue and usage_group_budget_seeds.
//
// D6a invariants:
//   - A charging episode for group G starts when G's first active pooled
//     binding (any scope, any window kind) activates, and ends when its last
//     active pooled binding deactivates.
//   - At episode start, exactly one seed is computed over
//     [max(current UTC month start, previous episode's ended_at), activated_at+2m)
//     for members as of the first seed run, written to usage_group_rollups_daily
//     under origin="seed:<episode_id>" for both (day, G, "") and (day, G, <project>).
//   - Bindings activating while an episode is already open (other scope, other
//     window kind, cap raises) trigger NO seed. Day and month limits both read
//     the same rollup rows.
//   - Once the +2m pass finalizes, the episode seed is final forever.
//   - Nightly reconcile only completes missing/incomplete episode seeds.
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

func (gs *GroupBudgetSeeder) enqueueEpisodePasses(ctx context.Context, ep *store.UsageGroupSeedEpisode) error {
	if gs.queue == nil || ep == nil {
		return nil
	}
	passes := []struct {
		tag       string
		notBefore time.Time
		finalPass bool
	}{
		{tag: "initial", notBefore: ep.ActivatedAt.UTC(), finalPass: false},
		{tag: "ttl_cover", notBefore: ep.CutoffAt.UTC(), finalPass: true},
	}
	for _, p := range passes {
		payload := GroupBudgetSeedPayload{
			EpisodeID:   ep.ID,
			GroupID:     ep.GroupID,
			IsFinalPass: p.finalPass,
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("marshal group budget seed payload: %w", err)
		}
		dedupKey := fmt.Sprintf("ep:%s:%s", ep.ID, p.tag)
		_, err = gs.queue.Enqueue(ctx, nil, durablequeue.Item{
			Kind:      usageGroupBudgetSeedQueueKind,
			DedupKey:  dedupKey,
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

// OnGroupBindingActivated opens a charging episode for groupID if none is
// currently open, and enqueues the episode's immediate and +2m seed passes.
// If an episode is already open for groupID, this is a no-op (D6a: bindings
// activating inside an open episode trigger no seed).
func (gs *GroupBudgetSeeder) OnGroupBindingActivated(ctx context.Context, groupID string, activatedAt time.Time) error {
	if !gs.enabled() || groupID == "" {
		return nil
	}
	if activatedAt.IsZero() {
		activatedAt = gs.now()
	}
	activatedAt = activatedAt.UTC().Truncate(time.Microsecond)

	ep, opened, err := gs.store.OpenUsageGroupChargingEpisode(ctx, groupID, activatedAt)
	if err != nil {
		return fmt.Errorf("open group charging episode: %w", err)
	}
	if !opened {
		return nil
	}
	return gs.enqueueEpisodePasses(ctx, ep)
}

// OnGroupBindingDeactivated closes groupID's open charging episode if and only
// if groupID now has zero active pooled bindings across all scopes and window
// kinds (D6a).
func (gs *GroupBudgetSeeder) OnGroupBindingDeactivated(ctx context.Context, groupID string, endedAt time.Time) error {
	if !gs.enabled() || groupID == "" {
		return nil
	}
	if endedAt.IsZero() {
		endedAt = gs.now()
	}
	endedAt = endedAt.UTC().Truncate(time.Microsecond)

	_, _, err := gs.store.CloseUsageGroupChargingEpisodeIfInactive(ctx, groupID, endedAt)
	if err != nil {
		return fmt.Errorf("close group charging episode: %w", err)
	}
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

// RunSeed executes one idempotent, per-group-locked D6a episode seed pass.
func (gs *GroupBudgetSeeder) RunSeed(ctx context.Context, payload GroupBudgetSeedPayload) error {
	if !gs.enabled() {
		return nil
	}
	if payload.EpisodeID == "" || payload.GroupID == "" {
		return fmt.Errorf("%w: invalid group budget seed payload", store.ErrInvalidInput)
	}
	now := gs.now().Truncate(time.Microsecond)

	res, err := gs.store.ExecuteUsageGroupEpisodeSeedPass(ctx, store.UsageGroupEpisodeSeedPassInput{
		EpisodeID:             payload.EpisodeID,
		GroupID:               payload.GroupID,
		LedgerRetentionCutoff: gs.ledgerRetentionCutoff(now),
		Now:                   now,
		IsFinalPass:           payload.IsFinalPass,
	})
	if err != nil {
		return err
	}

	// Re-evaluate group budget state immediately for both system scope and any
	// touched project scopes when the seed mutated rollups.
	if res.DidMutate && gs.evaluator != nil {
		if evalErr := gs.evaluator.EvaluateGroupScope(ctx, payload.GroupID, "", now); evalErr != nil {
			gs.logger.Warn("post-seed group budget evaluation failed",
				"group_id", payload.GroupID, "project_scope_id", "", "error", evalErr)
		}
		for _, projID := range res.TouchedProjectScope {
			if evalErr := gs.evaluator.EvaluateGroupScope(ctx, payload.GroupID, projID, now); evalErr != nil {
				gs.logger.Warn("post-seed group budget evaluation failed",
					"group_id", payload.GroupID, "project_scope_id", projID, "error", evalErr)
			}
		}
	}
	return nil
}

// ReconcileMissingSeeds enqueues seed passes ONLY for:
//  1. Existing non-finalized usage_group_budget_seeds episodes (recovering lost
//     queue items).
//  2. Groups that currently hold at least one active pooled binding created in
//     the current UTC month but have NO open charging episode (recovering a
//     direct store write or failed activation write).
//
// It NEVER inspects binding.UpdatedAt and NEVER re-enqueues a finalized
// episode (D6a, R1).
func (gs *GroupBudgetSeeder) ReconcileMissingSeeds(ctx context.Context) (int, error) {
	if !gs.enabled() || gs.queue == nil {
		return 0, nil
	}
	now := gs.now().Truncate(time.Microsecond)
	enqueued := 0

	enqueueEp := func(ep *store.UsageGroupSeedEpisode) error {
		notBefore := now
		if now.Before(ep.CutoffAt) {
			notBefore = ep.CutoffAt.UTC()
		}
		payload := GroupBudgetSeedPayload{
			EpisodeID:   ep.ID,
			GroupID:     ep.GroupID,
			IsFinalPass: true,
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		dedupKey := fmt.Sprintf("ep:%s:reconcile-%s", ep.ID, now.UTC().Format("2006-01-02"))
		_, err = gs.queue.Enqueue(ctx, nil, durablequeue.Item{
			Kind:      usageGroupBudgetSeedQueueKind,
			DedupKey:  dedupKey,
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

	// 1. Recover any existing incomplete charging episodes.
	incomplete, err := gs.store.ListIncompleteUsageGroupSeedEpisodes(ctx)
	if err != nil {
		return 0, fmt.Errorf("list incomplete group seed episodes: %w", err)
	}
	for _, ep := range incomplete {
		if err := enqueueEp(ep); err != nil {
			return enqueued, err
		}
	}

	// 2. Recover any group with an active pooled binding created in the current
	//    UTC month that has no open charging episode (e.g. direct store write
	//    or lost activation write).
	//
	//    Note (O6): Using binding.CreatedAt as the fallback activation timestamp
	//    avoids ever inferring activation from binding.UpdatedAt (which would
	//    re-seed on cap/action edits). If a binding was originally created with
	//    value=0 and later raised to >0 while its admin-API activation write
	//    failed, CreatedAt is a conservative lower bound; in normal operation
	//    OnGroupBindingActivated records the exact activation timestamp at the
	//    admin API.
	monthStart, monthEnd := utcMonthWindow(now)
	earliestCreatedByGroup := make(map[string]time.Time)
	for _, limitName := range []string{store.LimitLLMTokensPerGroupDay, store.LimitLLMTokensPerGroupMonth} {
		def, err := gs.store.GetLimitDefinitionByName(ctx, limitName)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			return enqueued, fmt.Errorf("lookup limit definition %q: %w", limitName, err)
		}
		if def == nil {
			continue
		}
		bindings, err := gs.store.ListEntitlementBindings(ctx, def.ID)
		if err != nil {
			return enqueued, fmt.Errorf("list bindings for %q: %w", limitName, err)
		}
		for _, b := range bindings {
			if b == nil || b.SubjectType != store.EntitlementSubjectGroup || b.SubjectID == "" || b.Value <= 0 {
				continue
			}
			createdAt := b.CreatedAt.UTC().Truncate(time.Microsecond)
			if createdAt.Before(monthStart) || !createdAt.Before(monthEnd) {
				continue
			}
			if prev, ok := earliestCreatedByGroup[b.SubjectID]; !ok || createdAt.Before(prev) {
				earliestCreatedByGroup[b.SubjectID] = createdAt
			}
		}
	}

	for groupID, actAt := range earliestCreatedByGroup {
		_, err := gs.store.GetOpenUsageGroupChargingEpisode(ctx, groupID)
		if err == nil {
			// Open episode exists (either already finalized, or handled in step 1).
			continue
		}
		if !errors.Is(err, store.ErrNotFound) {
			return enqueued, err
		}
		ep, opened, err := gs.store.OpenUsageGroupChargingEpisode(ctx, groupID, actAt)
		if err != nil {
			return enqueued, err
		}
		if opened && !ep.Finalized {
			if err := enqueueEp(ep); err != nil {
				return enqueued, err
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
// enqueues any missing/incomplete pooled-group episode seeds (D6a).
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
