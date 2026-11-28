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

// Structural invariant for all usage_budget_state writers (evaluateSpec,
// reevaluateSpec, OverrideBudgetState):
//
//   Every budget-state writer MUST resolve the effective (cap, token_action)
//   FRESH (uncached, via a fresh tokenLimitEvalCache) AFTER reading the
//   usage_budget_state row, on EVERY CAS attempt that writes.
//
//   A per-call tokenLimitEvalCache may ONLY be used to decide to skip (when
//   the cached effective cap <= 0 -> no write), never as the cap persisted
//   into usage_budget_state. This guarantees that neither the create path nor
//   the existing-row update path nor a CAS retry can write a stale cap after a
//   concurrent entitlement/limit change commits.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/bits"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const (
	// tokenBudgetWarnThresholdPct is the lowest default soft threshold (50%;
	// default soft thresholds are 50% and 80%, hard threshold 100%, per D4).
	tokenBudgetWarnThresholdPct = 50

	// maxBudgetEvalCASRetries bounds optimistic CAS retries when concurrent
	// evaluators contend on the same (limit, scope, window) row.
	maxBudgetEvalCASRetries = 16
)

// BudgetThresholdCrossing describes a monotonic state transition persisted via
// CAS in usage_budget_state (e.g. "" -> warn, ok -> warn, warn -> exhausted,
// "" -> exhausted). Consumed by P3.2 breach side effects (notifications,
// suspend sweep, analytics, metrics) via BudgetThresholdHook.
type BudgetThresholdCrossing struct {
	LimitName      string
	LimitDefID     string
	ScopeKind      string // "project" | "user" | "user_project"
	ScopeID        string
	ProjectScopeID string
	WindowStartUTC time.Time
	WindowEndUTC   time.Time
	PrevState      string // "" when newly created, else "ok" | "warn" | "exhausted"
	NewState       string // "warn" | "exhausted"
	Consumed       int64
	Cap            int64
	TokenAction    string // resolved effective action: "pause_new" | "suspend_and_pause" | "notify_only"
	StateVersion   int64
}

// BudgetThresholdHook is invoked outside any DB transaction strictly after a
// usage_budget_state create or CAS succeeds and moves the state upward into
// warn or exhausted. Because CAS succeeds for at most one writer per state
// transition, each crossing fires at most once even under concurrent evaluators.
type BudgetThresholdHook func(ctx context.Context, crossing BudgetThresholdCrossing)

// BudgetExhaustionStatus is returned by IsScopeExhaustedForAction (Seam 2 for
// P3.2 admission gate).
type BudgetExhaustionStatus struct {
	Exhausted      bool
	HasActiveCap   bool
	LimitName      string
	ScopeKind      string
	ScopeID        string
	ProjectScopeID string
	Consumed       int64
	Cap            int64
	WindowStartUTC time.Time
	WindowEndUTC   time.Time
	TokenAction    string
}

// BudgetNoticeStatus describes the highest-severity active warn/exhausted
// budget state applicable to an agent, surfaced on usage ingest responses
// (Tokenomics P3.2 B1).
type BudgetNoticeStatus struct {
	State       string
	LimitName   string
	ScopeKind   string
	Consumed    int64
	Cap         int64
	WindowEnd   time.Time
	TokenAction string
}

// TokenBudgetEvaluator evaluates raw-token consumption from
// usage_scope_counters_daily against configured llm_tokens LimitDefinitions
// and persists monotonic state transitions in usage_budget_state using
// optimistic CAS on state_version (Tokenomics P3.1, D3/D4).
type TokenBudgetEvaluator struct {
	store        store.Store
	quota        *QuotaService
	pooledGroups *PooledGroupResolver
	logger       *slog.Logger
	enabledFn    func() bool
	nowFn        func() time.Time

	hookMu        sync.RWMutex
	thresholdHook BudgetThresholdHook

	evalErrors        atomic.Int64
	hookPanics        atomic.Int64
	reevalUnprocessed atomic.Int64
	reevalPageSize    int
	reevalTimeout     time.Duration
	reevalWG          sync.WaitGroup
	metrics           atomic.Pointer[OTelTokenBudgetMetrics]
}

// NewTokenBudgetEvaluator constructs a TokenBudgetEvaluator.
func NewTokenBudgetEvaluator(st store.Store, qs *QuotaService, logger *slog.Logger, enabledFn func() bool) *TokenBudgetEvaluator {
	if logger == nil {
		logger = slog.Default()
	}
	return &TokenBudgetEvaluator{
		store:        st,
		quota:        qs,
		pooledGroups: NewPooledGroupResolver(st),
		logger:       logger,
		enabledFn:    enabledFn,
		nowFn:        func() time.Time { return time.Now().UTC() },
	}
}

// SetPooledGroupResolver wires a shared PooledGroupResolver into the evaluator
// so ingest, admission, and cache invalidation share the same replica cache.
func (e *TokenBudgetEvaluator) SetPooledGroupResolver(r *PooledGroupResolver) {
	if e != nil && r != nil {
		e.pooledGroups = r
	}
}

// SetThresholdHook installs (or clears when nil) the post-CAS threshold
// crossing callback (Seam 1 for P3.2).
func (e *TokenBudgetEvaluator) SetThresholdHook(hook BudgetThresholdHook) {
	e.hookMu.Lock()
	defer e.hookMu.Unlock()
	e.thresholdHook = hook
}

// EvalErrors returns the cumulative count of evaluation errors encountered by
// EvaluateIngestedEvents (useful for diagnostics and tests).
func (e *TokenBudgetEvaluator) EvalErrors() int64 {
	return e.evalErrors.Load()
}

// HookPanics returns the cumulative count of recovered panics from
// BudgetThresholdHook invocations.
func (e *TokenBudgetEvaluator) HookPanics() int64 {
	return e.hookPanics.Load()
}

func (e *TokenBudgetEvaluator) enabled() bool {
	if e == nil || e.store == nil || e.quota == nil {
		return false
	}
	if e.enabledFn == nil {
		return true
	}
	return e.enabledFn()
}

func (e *TokenBudgetEvaluator) now() time.Time {
	if e != nil && e.nowFn != nil {
		return e.nowFn().UTC()
	}
	return time.Now().UTC()
}

func (e *TokenBudgetEvaluator) fireThresholdHook(ctx context.Context, crossing BudgetThresholdCrossing) {
	e.hookMu.RLock()
	hook := e.thresholdHook
	e.hookMu.RUnlock()
	if hook == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			e.hookPanics.Add(1)
			e.logger.Error("token budget threshold hook panicked; recovered",
				"limit", crossing.LimitName,
				"scope_kind", crossing.ScopeKind,
				"scope_id", crossing.ScopeID,
				"project_scope_id", crossing.ProjectScopeID,
				"new_state", crossing.NewState,
				"panic", r)
		}
	}()
	hook(ctx, crossing)
}

// utcDayWindow returns the half-open UTC day window [00:00, next 00:00) containing ts.
func utcDayWindow(ts time.Time) (start, end time.Time) {
	u := ts.UTC()
	start = time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
	end = start.AddDate(0, 0, 1)
	return start, end
}

// utcMonthWindow returns the half-open UTC calendar month window [1st 00:00, next 1st 00:00) containing ts.
func utcMonthWindow(ts time.Time) (start, end time.Time) {
	u := ts.UTC()
	start = time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
	end = start.AddDate(0, 1, 0)
	return start, end
}

// exceedsPercent reports whether consumed/cap >= pct/100 without int64 overflow
// (exact for all int64 values up to math.MaxInt64 via 128-bit unsigned multiply).
func exceedsPercent(consumed, capVal, pct int64) bool {
	if capVal <= 0 {
		return false
	}
	if pct <= 0 {
		return true
	}
	if consumed <= 0 {
		return false
	}
	if pct >= 100 {
		return consumed >= capVal
	}
	hiC, loC := bits.Mul64(uint64(consumed), 100)
	hiT, loT := bits.Mul64(uint64(capVal), uint64(pct))
	return hiC > hiT || (hiC == hiT && loC >= loT)
}

func computeBudgetState(consumed, capVal int64) string {
	if capVal <= 0 {
		return ""
	}
	if consumed >= capVal {
		return store.UsageBudgetStateExhausted
	}
	if exceedsPercent(consumed, capVal, tokenBudgetWarnThresholdPct) {
		return store.UsageBudgetStateWarn
	}
	return store.UsageBudgetStateOK
}

func budgetStateRank(state string) int {
	switch state {
	case store.UsageBudgetStateOK:
		return 0
	case store.UsageBudgetStateWarn:
		return 1
	case store.UsageBudgetStateExhausted:
		return 2
	default:
		return -1
	}
}

func maxBudgetState(a, b string) string {
	if budgetStateRank(b) > budgetStateRank(a) {
		return b
	}
	return a
}

type budgetEvalSpec struct {
	limitName      string
	scopeKind      string
	scopeID        string
	projectScopeID string
	counterScopeID string
	bindingSubject string
	bindingScope   string
	bindingScopeID string
	windowStartUTC time.Time
	windowEndUTC   time.Time
}

// EvaluateIngestedEvents evaluates every touched project, user, and
// user_project scope for newly committed usage events against all applicable
// non-zero raw-token caps in open UTC day and month windows (window_end_utc > now).
//
// Errors are logged and counted via EvalErrors; evaluation never fails or
// alters the ingest response (D4).
func (e *TokenBudgetEvaluator) EvaluateIngestedEvents(ctx context.Context, inserted []*store.UsageEvent) {
	_ = e.EvaluateIngestedEventsWithNotice(ctx, inserted)
}

// EvaluateIngestedEventsWithNotice runs EvaluateIngestedEvents and derives the
// highest-severity active non-overridden warn/exhausted notice directly from
// the states evaluated for this batch (zero extra DB queries).
func (e *TokenBudgetEvaluator) EvaluateIngestedEventsWithNotice(ctx context.Context, inserted []*store.UsageEvent) *BudgetNoticeStatus {
	if !e.enabled() || len(inserted) == 0 {
		return nil
	}
	now := e.now()

	type evalDedupeKey struct {
		limitName      string
		scopeKind      string
		scopeID        string
		projectScopeID string
		windowStartUTC time.Time
	}
	seen := make(map[evalDedupeKey]struct{}, len(inserted)*6)
	specs := make([]budgetEvalSpec, 0, len(inserted)*6)

	addIfOpen := func(spec budgetEvalSpec) {
		// Only evaluate windows still open (window_end_utc > now); late events
		// for a closed day or month must not create or mutate state rows.
		if !spec.windowEndUTC.After(now) {
			return
		}
		dk := evalDedupeKey{
			limitName:      spec.limitName,
			scopeKind:      spec.scopeKind,
			scopeID:        spec.scopeID,
			projectScopeID: spec.projectScopeID,
			windowStartUTC: spec.windowStartUTC,
		}
		if _, dup := seen[dk]; dup {
			return
		}
		seen[dk] = struct{}{}
		specs = append(specs, spec)
	}

	for _, ev := range inserted {
		if ev == nil || ev.ProjectID == "" {
			continue
		}
		dayStart, dayEnd := utcDayWindow(ev.EventTS)
		monthStart, monthEnd := utcMonthWindow(ev.EventTS)
		billingUserID := ""
		if ev.BillingUserID != nil {
			billingUserID = *ev.BillingUserID
		}
		for _, spec := range applicableTokenBudgetSpecs(ev.ProjectID, billingUserID, ev.ChargedGroupIDs, dayStart, dayEnd, monthStart, monthEnd) {
			addIfOpen(spec)
		}
	}

	evalCache := newTokenLimitEvalCache(e.quota)
	var bestNotice *BudgetNoticeStatus
	for _, spec := range specs {
		st, action, err := e.evaluateSpec(ctx, spec, evalCache)
		if err != nil {
			e.recordEvalError()
			e.logger.Warn("token budget evaluation failed",
				"limit", spec.limitName,
				"scope_kind", spec.scopeKind,
				"scope_id", spec.scopeID,
				"project_scope_id", spec.projectScopeID,
				"window_start_utc", spec.windowStartUTC,
				"error", err)
			continue
		}
		if st == nil || (st.State != store.UsageBudgetStateWarn && st.State != store.UsageBudgetStateExhausted) {
			continue
		}
		if st.OverrideUntil != nil && st.OverrideUntil.After(now) {
			continue
		}
		bestNotice = pickHigherBudgetNotice(bestNotice, &BudgetNoticeStatus{
			State:       st.State,
			LimitName:   spec.limitName,
			ScopeKind:   spec.scopeKind,
			Consumed:    st.ConsumedAtEval,
			Cap:         st.CapAtEval,
			WindowEnd:   st.WindowEndUTC,
			TokenAction: action,
		})
	}
	return bestNotice
}

func pickHigherBudgetNotice(best, cand *BudgetNoticeStatus) *BudgetNoticeStatus {
	if cand == nil {
		return best
	}
	if best == nil {
		return cand
	}
	rBest, rCand := budgetStateRank(best.State), budgetStateRank(cand.State)
	if rCand != rBest {
		if rCand > rBest {
			return cand
		}
		return best
	}
	// Tie-break by higher utilization ratio (cand.Consumed/cand.Cap > best.Consumed/best.Cap).
	if cand.Cap > 0 && best.Cap > 0 && cand.Consumed >= 0 && best.Consumed >= 0 {
		hiC, loC := bits.Mul64(uint64(cand.Consumed), uint64(best.Cap))
		hiB, loB := bits.Mul64(uint64(best.Consumed), uint64(cand.Cap))
		if hiC > hiB || (hiC == hiB && loC > loB) {
			return cand
		}
	}
	return best
}

// HighestActiveBudgetNotice reads active usage_budget_state rows for
// (projectID, billingUserID) at the evaluator's clock and returns the
// highest-severity non-overridden warn/exhausted notice, or nil.
// Used only as a fallback on ingest batches that did not run
// EvaluateIngestedEventsWithNotice (e.g. empty heartbeats or all-duplicate batches).
func (e *TokenBudgetEvaluator) HighestActiveBudgetNotice(ctx context.Context, projectID, billingUserID string) *BudgetNoticeStatus {
	if !e.enabled() || projectID == "" {
		return nil
	}
	now := e.now()
	dayStart, dayEnd := utcDayWindow(now)
	monthStart, monthEnd := utcMonthWindow(now)
	var groupIDs []string
	if billingUserID != "" && e.pooledGroups != nil {
		var err error
		groupIDs, err = e.pooledGroups.ResolveForUser(ctx, billingUserID)
		if err != nil {
			e.logger.Warn("token budget notice: failed to resolve pooled groups",
				"billing_user_id", billingUserID, "error", err)
		}
	}
	specs := applicableTokenBudgetSpecs(projectID, billingUserID, groupIDs, dayStart, dayEnd, monthStart, monthEnd)

	type scopeQuery struct {
		scopeKind      string
		scopeID        string
		projectScopeID string
		bindingSubject string
		bindingScope   string
		bindingScopeID string
	}
	type scopeDedupeKey struct {
		scopeKind      string
		scopeID        string
		projectScopeID string
	}
	seenScopes := make(map[scopeDedupeKey]struct{}, len(specs))
	queries := make([]scopeQuery, 0, len(specs))
	for _, spec := range specs {
		dk := scopeDedupeKey{
			scopeKind:      spec.scopeKind,
			scopeID:        spec.scopeID,
			projectScopeID: spec.projectScopeID,
		}
		if _, dup := seenScopes[dk]; dup {
			continue
		}
		seenScopes[dk] = struct{}{}
		queries = append(queries, scopeQuery{
			scopeKind:      spec.scopeKind,
			scopeID:        spec.scopeID,
			projectScopeID: spec.projectScopeID,
			bindingSubject: spec.bindingSubject,
			bindingScope:   spec.bindingScope,
			bindingScopeID: spec.bindingScopeID,
		})
	}

	var bestRow *store.UsageBudgetState
	var bestQuery scopeQuery
	var bestNotice *BudgetNoticeStatus
	for _, q := range queries {
		rows, err := e.store.ListUsageBudgetStatesForScope(ctx, q.scopeKind, q.scopeID, q.projectScopeID, now)
		if err != nil {
			e.logger.Warn("token budget notice lookup failed",
				"scope_kind", q.scopeKind, "scope_id", q.scopeID, "error", err)
			continue
		}
		for _, row := range rows {
			if row == nil || (row.State != store.UsageBudgetStateWarn && row.State != store.UsageBudgetStateExhausted) {
				continue
			}
			if row.OverrideUntil != nil && row.OverrideUntil.After(now) {
				continue
			}
			cand := &BudgetNoticeStatus{
				State:     row.State,
				ScopeKind: row.ScopeKind,
				Consumed:  row.ConsumedAtEval,
				Cap:       row.CapAtEval,
				WindowEnd: row.WindowEndUTC,
			}
			if pickHigherBudgetNotice(bestNotice, cand) == cand {
				bestNotice = cand
				bestRow = row
				bestQuery = q
			}
		}
	}
	if bestNotice == nil || bestRow == nil {
		return nil
	}
	if def, err := e.store.GetLimitDefinition(ctx, bestRow.LimitDefinitionID); err == nil && def != nil {
		bestNotice.LimitName = def.Name
		var action string
		var rerr error
		if bestQuery.scopeKind == store.UsageScopeKindGroup {
			_, action, rerr = e.quota.ResolveEffectiveGroupTokenLimit(
				ctx, def.ID, bestQuery.scopeID, bestQuery.bindingScope, bestQuery.bindingScopeID,
			)
		} else {
			_, action, rerr = e.quota.ResolveEffectiveTokenLimit(
				ctx, def.ID, bestQuery.bindingSubject, bestQuery.bindingScope, bestQuery.bindingScopeID,
			)
		}
		if rerr == nil && action != "" {
			bestNotice.TokenAction = action
		}
	}
	if bestNotice.TokenAction == "" {
		bestNotice.TokenAction = store.TokenActionPauseNew
	}
	return bestNotice
}

func applicableTokenBudgetSpecs(projectID, billingUserID string, groupIDs []string, dayStart, dayEnd, monthStart, monthEnd time.Time) []budgetEvalSpec {
	specs := []budgetEvalSpec{
		{
			limitName:      store.LimitLLMTokensPerProjectDay,
			scopeKind:      store.UsageScopeKindProject,
			scopeID:        projectID,
			counterScopeID: projectID,
			bindingScope:   store.QuotaScopeProject,
			bindingScopeID: projectID,
			windowStartUTC: dayStart,
			windowEndUTC:   dayEnd,
		},
		{
			limitName:      store.LimitLLMTokensPerProjectMonth,
			scopeKind:      store.UsageScopeKindProject,
			scopeID:        projectID,
			counterScopeID: projectID,
			bindingScope:   store.QuotaScopeProject,
			bindingScopeID: projectID,
			windowStartUTC: monthStart,
			windowEndUTC:   monthEnd,
		},
	}
	if billingUserID != "" {
		upCounterID := billingUserID + ":" + projectID
		specs = append(specs,
			budgetEvalSpec{
				limitName:      store.LimitLLMTokensPerUserDay,
				scopeKind:      store.UsageScopeKindUser,
				scopeID:        billingUserID,
				counterScopeID: billingUserID,
				bindingSubject: billingUserID,
				bindingScope:   store.QuotaScopeSystem,
				bindingScopeID: "",
				windowStartUTC: dayStart,
				windowEndUTC:   dayEnd,
			},
			budgetEvalSpec{
				limitName:      store.LimitLLMTokensPerUserMonth,
				scopeKind:      store.UsageScopeKindUser,
				scopeID:        billingUserID,
				counterScopeID: billingUserID,
				bindingSubject: billingUserID,
				bindingScope:   store.QuotaScopeSystem,
				bindingScopeID: "",
				windowStartUTC: monthStart,
				windowEndUTC:   monthEnd,
			},
			budgetEvalSpec{
				limitName:      store.LimitLLMTokensPerUserProjectDay,
				scopeKind:      store.UsageScopeKindUserProject,
				scopeID:        billingUserID,
				projectScopeID: projectID,
				counterScopeID: upCounterID,
				bindingSubject: billingUserID,
				bindingScope:   store.QuotaScopeProject,
				bindingScopeID: projectID,
				windowStartUTC: dayStart,
				windowEndUTC:   dayEnd,
			},
			budgetEvalSpec{
				limitName:      store.LimitLLMTokensPerUserProjectMonth,
				scopeKind:      store.UsageScopeKindUserProject,
				scopeID:        billingUserID,
				projectScopeID: projectID,
				counterScopeID: upCounterID,
				bindingSubject: billingUserID,
				bindingScope:   store.QuotaScopeProject,
				bindingScopeID: projectID,
				windowStartUTC: monthStart,
				windowEndUTC:   monthEnd,
			},
		)
	}
	// Pooled user-group scopes (Tokenomics P3.3, §4.4): system-wide and
	// project-scoped day/month pools for each charged/effective group.
	for _, gid := range groupIDs {
		if gid == "" {
			continue
		}
		specs = append(specs, groupTokenBudgetSpecs(gid, "", dayStart, dayEnd, monthStart, monthEnd)...)
		specs = append(specs, groupTokenBudgetSpecs(gid, projectID, dayStart, dayEnd, monthStart, monthEnd)...)
	}
	return specs
}

func groupTokenBudgetSpecs(groupID, projectScopeID string, dayStart, dayEnd, monthStart, monthEnd time.Time) []budgetEvalSpec {
	bindingScope := store.QuotaScopeSystem
	bindingScopeID := ""
	if projectScopeID != "" {
		bindingScope = store.QuotaScopeProject
		bindingScopeID = projectScopeID
	}
	return []budgetEvalSpec{
		{
			limitName:      store.LimitLLMTokensPerGroupDay,
			scopeKind:      store.UsageScopeKindGroup,
			scopeID:        groupID,
			projectScopeID: projectScopeID,
			bindingScope:   bindingScope,
			bindingScopeID: bindingScopeID,
			windowStartUTC: dayStart,
			windowEndUTC:   dayEnd,
		},
		{
			limitName:      store.LimitLLMTokensPerGroupMonth,
			scopeKind:      store.UsageScopeKindGroup,
			scopeID:        groupID,
			projectScopeID: projectScopeID,
			bindingScope:   bindingScope,
			bindingScopeID: bindingScopeID,
			windowStartUTC: monthStart,
			windowEndUTC:   monthEnd,
		},
	}
}

// EvaluateGroupScope evaluates the active UTC day and month pooled-group
// budgets for (groupID, projectScopeID) at `now`. Called by the pooled-group
// seeder right after writing origin="seed" rows so usage_budget_state reflects
// newly seeded spend immediately.
func (e *TokenBudgetEvaluator) EvaluateGroupScope(ctx context.Context, groupID, projectScopeID string, now time.Time) error {
	if !e.enabled() || groupID == "" {
		return nil
	}
	if now.IsZero() {
		now = e.now()
	} else {
		now = now.UTC()
	}
	dayStart, dayEnd := utcDayWindow(now)
	monthStart, monthEnd := utcMonthWindow(now)
	specs := groupTokenBudgetSpecs(groupID, projectScopeID, dayStart, dayEnd, monthStart, monthEnd)
	evalCache := newTokenLimitEvalCache(e.quota)
	for _, spec := range specs {
		if _, _, err := e.evaluateSpec(ctx, spec, evalCache); err != nil {
			return err
		}
	}
	return nil
}

// IsScopeExhaustedForAction checks whether any applicable non-zero raw-token
// limit for (projectID, billingUserID) in the active UTC day or month window
// at `now` is exhausted (respecting override_until > now) with an effective
// token_action matching `actions` (if actions is empty, any action matches).
//
// `now` is clamped to at least the evaluator's clock so closed windows are
// never evaluated or created. Re-evaluates on demand from
// usage_scope_counters_daily (≤31 rows per window) so a missed post-commit
// evaluation self-heals (D4). Returns Exhausted=false when either
// tokenomics.token_budgets or tokenomics.usage_ledger is disabled.
func (e *TokenBudgetEvaluator) IsScopeExhaustedForAction(
	ctx context.Context,
	projectID string,
	billingUserID string,
	now time.Time,
	actions ...string,
) (*BudgetExhaustionStatus, error) {
	if !e.enabled() || projectID == "" {
		return &BudgetExhaustionStatus{Exhausted: false}, nil
	}
	evalNow := e.now()
	if now.IsZero() || now.Before(evalNow) {
		now = evalNow
	} else {
		now = now.UTC()
	}

	actionSet := make(map[string]struct{}, len(actions))
	for _, a := range actions {
		if a != "" {
			actionSet[a] = struct{}{}
		}
	}
	matchesAction := func(act string) bool {
		if len(actionSet) == 0 {
			return true
		}
		_, ok := actionSet[act]
		return ok
	}

	dayStart, dayEnd := utcDayWindow(now)
	monthStart, monthEnd := utcMonthWindow(now)
	var groupIDs []string
	if billingUserID != "" && e.pooledGroups != nil {
		var err error
		groupIDs, err = e.pooledGroups.ResolveForUser(ctx, billingUserID)
		if err != nil {
			return nil, fmt.Errorf("resolve pooled groups for admission check: %w", err)
		}
	}
	specs := applicableTokenBudgetSpecs(projectID, billingUserID, groupIDs, dayStart, dayEnd, monthStart, monthEnd)

	evalCache := newTokenLimitEvalCache(e.quota)
	hasActiveCap := false
	for _, spec := range specs {
		st, action, err := e.evaluateSpec(ctx, spec, evalCache)
		if err != nil {
			return nil, err
		}
		if st == nil {
			continue
		}
		if !matchesAction(action) {
			continue
		}
		if st.OverrideUntil != nil && st.OverrideUntil.After(now) {
			continue
		}
		// Only blocking (action-matching), non-overridden, >0 caps count as an
		// active cap for usage.unmeasured_policy=deny (D9).
		hasActiveCap = true
		if st.State != store.UsageBudgetStateExhausted {
			continue
		}
		return &BudgetExhaustionStatus{
			Exhausted:      true,
			HasActiveCap:   true,
			LimitName:      spec.limitName,
			ScopeKind:      spec.scopeKind,
			ScopeID:        spec.scopeID,
			ProjectScopeID: spec.projectScopeID,
			Consumed:       st.ConsumedAtEval,
			Cap:            st.CapAtEval,
			WindowStartUTC: st.WindowStartUTC,
			WindowEndUTC:   st.WindowEndUTC,
			TokenAction:    action,
		}, nil
	}

	return &BudgetExhaustionStatus{Exhausted: false, HasActiveCap: hasActiveCap}, nil
}

// evaluateSpec evaluates a single (limit, scope, window) tuple and persists
// any monotonic state transition via optimistic CAS. Returns (nil, action, nil)
// when the limit is not defined or is unlimited (cap <= 0).
func (e *TokenBudgetEvaluator) evaluateSpec(
	ctx context.Context,
	spec budgetEvalSpec,
	evalCache *tokenLimitEvalCache,
) (*store.UsageBudgetState, string, error) {
	def, err := evalCache.lookupLimitDef(ctx, spec.limitName)
	if err != nil {
		return nil, "", err
	}
	if def == nil {
		return nil, "", nil
	}

	// Per the structural fresh-cap writer rule, evalCache may ONLY be used to
	// decide to skip (cached cap <= 0 -> no write). Every CAS attempt that
	// actually writes resolves the cap fresh after reading the row (for both
	// project/user/user_project and pooled group scopes).
	if evalCache != nil {
		var cachedCap int64
		var cachedAction string
		if spec.scopeKind == store.UsageScopeKindGroup {
			cachedCap, cachedAction, err = evalCache.resolveGroupForDef(
				ctx, def, spec.scopeID, spec.bindingScope, spec.bindingScopeID,
			)
		} else {
			cachedCap, cachedAction, err = evalCache.resolveForDef(
				ctx, def, spec.bindingSubject, spec.bindingScope, spec.bindingScopeID,
			)
		}
		if err != nil {
			return nil, "", fmt.Errorf("resolve effective token limit %q: %w", spec.limitName, err)
		}
		if cachedCap <= 0 {
			return nil, cachedAction, nil
		}
	}

	key := store.UsageBudgetStateKey{
		LimitDefinitionID: def.ID,
		ScopeKind:         spec.scopeKind,
		ScopeID:           spec.scopeID,
		ProjectScopeID:    spec.projectScopeID,
		WindowStartUTC:    spec.windowStartUTC,
	}

	for attempt := 0; attempt < maxBudgetEvalCASRetries; attempt++ {
		existing, err := e.store.GetUsageBudgetState(ctx, key)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, "", fmt.Errorf("get usage budget state: %w", err)
		}

		// Always resolve the effective cap FRESH (uncached, strict) AFTER reading the
		// row on every CAS attempt that may create or update usage_budget_state
		// (applies uniformly to project, user, user_project, and group scopes).
		freshCache := newStrictTokenLimitEvalCache(e.quota)
		var capVal int64
		var action string
		if spec.scopeKind == store.UsageScopeKindGroup {
			capVal, action, err = freshCache.resolveGroupByID(
				ctx, def.ID, spec.scopeID, spec.bindingScope, spec.bindingScopeID,
			)
		} else {
			capVal, action, err = freshCache.resolveByID(
				ctx, def.ID, spec.bindingSubject, spec.bindingScope, spec.bindingScopeID,
			)
		}
		if err != nil {
			return nil, "", fmt.Errorf("resolve effective token limit %q: %w", spec.limitName, err)
		}
		// Cap <= 0 means unlimited for llm_tokens limits; no state row is created (D3).
		if capVal <= 0 {
			return nil, action, nil
		}

		var consumed int64
		if spec.scopeKind == store.UsageScopeKindGroup {
			consumed, err = e.store.SumUsageGroupRollupTokens(
				ctx, spec.scopeID, spec.projectScopeID, spec.windowStartUTC, spec.windowEndUTC,
			)
		} else {
			consumed, err = e.store.SumUsageScopeCounterTokens(
				ctx, spec.scopeKind, spec.counterScopeID, spec.windowStartUTC, spec.windowEndUTC,
			)
		}
		if err != nil {
			return nil, "", fmt.Errorf("sum scope counter tokens: %w", err)
		}
		computedState := computeBudgetState(consumed, capVal)

		if existing == nil {
			created, createErr := e.store.CreateUsageBudgetState(ctx, &store.UsageBudgetState{
				LimitDefinitionID: def.ID,
				ScopeKind:         spec.scopeKind,
				ScopeID:           spec.scopeID,
				ProjectScopeID:    spec.projectScopeID,
				WindowStartUTC:    spec.windowStartUTC,
				WindowEndUTC:      spec.windowEndUTC,
				State:             computedState,
				ConsumedAtEval:    consumed,
				CapAtEval:         capVal,
				StateVersion:      1,
			})
			if createErr != nil {
				if errors.Is(createErr, store.ErrAlreadyExists) {
					// Another concurrent evaluator inserted first; retry via CAS.
					continue
				}
				return nil, "", fmt.Errorf("create usage budget state: %w", createErr)
			}
			// Fire threshold hook strictly after successful create (outside any tx).
			if computedState == store.UsageBudgetStateWarn || computedState == store.UsageBudgetStateExhausted {
				e.fireThresholdHook(ctx, BudgetThresholdCrossing{
					LimitName:      spec.limitName,
					LimitDefID:     def.ID,
					ScopeKind:      spec.scopeKind,
					ScopeID:        spec.scopeID,
					ProjectScopeID: spec.projectScopeID,
					WindowStartUTC: created.WindowStartUTC,
					WindowEndUTC:   created.WindowEndUTC,
					PrevState:      "",
					NewState:       computedState,
					Consumed:       created.ConsumedAtEval,
					Cap:            created.CapAtEval,
					TokenAction:    action,
					StateVersion:   created.StateVersion,
				})
			}
			return created, action, nil
		}

		// Monotonic within window: evaluator never moves state downward (D4).
		// While an admin override is active (override_until > evaluator clock),
		// do not re-raise state above existing.State (ok); state resumes rising
		// once override_until elapses.
		var nextState string
		if existing.OverrideUntil != nil && existing.OverrideUntil.After(e.now()) {
			nextState = existing.State
		} else {
			nextState = maxBudgetState(existing.State, computedState)
		}
		nextConsumed := consumed
		if existing.ConsumedAtEval > nextConsumed {
			nextConsumed = existing.ConsumedAtEval
		}

		if nextState == existing.State && nextConsumed == existing.ConsumedAtEval && capVal == existing.CapAtEval {
			return existing, action, nil
		}

		candidate := *existing
		candidate.State = nextState
		candidate.ConsumedAtEval = nextConsumed
		candidate.CapAtEval = capVal

		swapped, swapErr := e.store.CompareAndSwapUsageBudgetState(ctx, existing.StateVersion, &candidate)
		if swapErr != nil {
			if errors.Is(swapErr, store.ErrBudgetStateConflict) {
				continue
			}
			return nil, "", fmt.Errorf("cas usage budget state: %w", swapErr)
		}

		// Fire threshold hook strictly after successful CAS and only on an
		// upward state crossing (ok->warn, ok->exhausted, warn->exhausted).
		if budgetStateRank(nextState) > budgetStateRank(existing.State) &&
			(nextState == store.UsageBudgetStateWarn || nextState == store.UsageBudgetStateExhausted) {
			e.fireThresholdHook(ctx, BudgetThresholdCrossing{
				LimitName:      spec.limitName,
				LimitDefID:     def.ID,
				ScopeKind:      spec.scopeKind,
				ScopeID:        spec.scopeID,
				ProjectScopeID: spec.projectScopeID,
				WindowStartUTC: swapped.WindowStartUTC,
				WindowEndUTC:   swapped.WindowEndUTC,
				PrevState:      existing.State,
				NewState:       nextState,
				Consumed:       swapped.ConsumedAtEval,
				Cap:            swapped.CapAtEval,
				TokenAction:    action,
				StateVersion:   swapped.StateVersion,
			})
		}
		return swapped, action, nil
	}

	return nil, "", fmt.Errorf("usage budget state CAS retries exhausted for %s (%s/%s)",
		spec.limitName, spec.scopeKind, spec.scopeID)
}

// TokenBudgetEvaluator returns (lazily constructing on first call) the Hub's
// raw-token budget evaluator. Uses an RLock fast path so hot-path ingest
// requests never contend on the global write lock after initialization.
func (s *Server) TokenBudgetEvaluator() *TokenBudgetEvaluator {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	eval := s.tokenBudgetEvaluator
	s.mu.RUnlock()
	if eval != nil {
		return eval
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tokenBudgetEvaluator == nil && s.store != nil && s.quotaService != nil {
		if s.pooledGroupResolver == nil {
			s.pooledGroupResolver = NewPooledGroupResolver(s.store)
		}
		eval := NewTokenBudgetEvaluator(
			s.store,
			s.quotaService,
			slog.Default(),
			func() bool {
				return s.experimentEnabled(experiments.TokenomicsUsageLedger) &&
					s.experimentEnabled(experiments.TokenomicsTokenBudgets)
			},
		)
		eval.SetPooledGroupResolver(s.pooledGroupResolver)
		s.tokenBudgetEvaluator = eval
	}
	return s.tokenBudgetEvaluator
}

// SetBudgetThresholdHook installs the threshold-crossing hook on the server's
// TokenBudgetEvaluator (Seam 1 for P3.2).
func (s *Server) SetBudgetThresholdHook(hook BudgetThresholdHook) {
	if eval := s.TokenBudgetEvaluator(); eval != nil {
		eval.SetThresholdHook(hook)
	}
}

// reconcileTokenBudgets idempotently seeds the six raw-token LimitDefinitions
// when tokenomics.token_budgets is enabled. Called at Hub startup and on live
// settings ApplySnapshot (D5).
func (s *Server) reconcileTokenBudgets(ctx context.Context) {
	if s == nil || s.store == nil {
		return
	}
	if !s.experimentEnabled(experiments.TokenomicsTokenBudgets) {
		return
	}
	seedTokenBudgetLimitDefinitions(ctx, s.store)
	s.startGroupBudgetSeedDrainer(ctx)
}
