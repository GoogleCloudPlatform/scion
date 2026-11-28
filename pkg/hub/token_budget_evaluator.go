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

	evalErrors atomic.Int64
	hookPanics atomic.Int64
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
	if !e.enabled() || len(inserted) == 0 {
		return
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

		// Project scope always applies (including when billing_user_id is NULL).
		addIfOpen(budgetEvalSpec{
			limitName:      store.LimitLLMTokensPerProjectDay,
			scopeKind:      store.UsageScopeKindProject,
			scopeID:        ev.ProjectID,
			counterScopeID: ev.ProjectID,
			bindingScope:   store.QuotaScopeProject,
			bindingScopeID: ev.ProjectID,
			windowStartUTC: dayStart,
			windowEndUTC:   dayEnd,
		})
		addIfOpen(budgetEvalSpec{
			limitName:      store.LimitLLMTokensPerProjectMonth,
			scopeKind:      store.UsageScopeKindProject,
			scopeID:        ev.ProjectID,
			counterScopeID: ev.ProjectID,
			bindingScope:   store.QuotaScopeProject,
			bindingScopeID: ev.ProjectID,
			windowStartUTC: monthStart,
			windowEndUTC:   monthEnd,
		})

		// User and user_project scopes apply only when billing_user_id is non-NULL (D3).
		if ev.BillingUserID != nil && *ev.BillingUserID != "" {
			uid := *ev.BillingUserID
			upCounterID := uid + ":" + ev.ProjectID

			addIfOpen(budgetEvalSpec{
				limitName:      store.LimitLLMTokensPerUserDay,
				scopeKind:      store.UsageScopeKindUser,
				scopeID:        uid,
				counterScopeID: uid,
				bindingSubject: uid,
				bindingScope:   store.QuotaScopeSystem,
				bindingScopeID: "",
				windowStartUTC: dayStart,
				windowEndUTC:   dayEnd,
			})
			addIfOpen(budgetEvalSpec{
				limitName:      store.LimitLLMTokensPerUserMonth,
				scopeKind:      store.UsageScopeKindUser,
				scopeID:        uid,
				counterScopeID: uid,
				bindingSubject: uid,
				bindingScope:   store.QuotaScopeSystem,
				bindingScopeID: "",
				windowStartUTC: monthStart,
				windowEndUTC:   monthEnd,
			})
			addIfOpen(budgetEvalSpec{
				limitName:      store.LimitLLMTokensPerUserProjectDay,
				scopeKind:      store.UsageScopeKindUserProject,
				scopeID:        uid,
				projectScopeID: ev.ProjectID,
				counterScopeID: upCounterID,
				bindingSubject: uid,
				bindingScope:   store.QuotaScopeProject,
				bindingScopeID: ev.ProjectID,
				windowStartUTC: dayStart,
				windowEndUTC:   dayEnd,
			})
			addIfOpen(budgetEvalSpec{
				limitName:      store.LimitLLMTokensPerUserProjectMonth,
				scopeKind:      store.UsageScopeKindUserProject,
				scopeID:        uid,
				projectScopeID: ev.ProjectID,
				counterScopeID: upCounterID,
				bindingSubject: uid,
				bindingScope:   store.QuotaScopeProject,
				bindingScopeID: ev.ProjectID,
				windowStartUTC: monthStart,
				windowEndUTC:   monthEnd,
			})
		}

		// Pooled user-group scopes: evaluate every charged group for both its
		// system-wide pool (project_scope_id = "") and project-scoped pool
		// (project_scope_id = ev.ProjectID). Overlapping pools are each
		// evaluated independently (non-additive, §4.4).
		for _, gid := range ev.ChargedGroupIDs {
			if gid == "" {
				continue
			}
			addIfOpen(budgetEvalSpec{
				limitName:      store.LimitLLMTokensPerGroupDay,
				scopeKind:      store.UsageScopeKindGroup,
				scopeID:        gid,
				projectScopeID: "",
				bindingScope:   store.QuotaScopeSystem,
				bindingScopeID: "",
				windowStartUTC: dayStart,
				windowEndUTC:   dayEnd,
			})
			addIfOpen(budgetEvalSpec{
				limitName:      store.LimitLLMTokensPerGroupMonth,
				scopeKind:      store.UsageScopeKindGroup,
				scopeID:        gid,
				projectScopeID: "",
				bindingScope:   store.QuotaScopeSystem,
				bindingScopeID: "",
				windowStartUTC: monthStart,
				windowEndUTC:   monthEnd,
			})
			addIfOpen(budgetEvalSpec{
				limitName:      store.LimitLLMTokensPerGroupDay,
				scopeKind:      store.UsageScopeKindGroup,
				scopeID:        gid,
				projectScopeID: ev.ProjectID,
				bindingScope:   store.QuotaScopeProject,
				bindingScopeID: ev.ProjectID,
				windowStartUTC: dayStart,
				windowEndUTC:   dayEnd,
			})
			addIfOpen(budgetEvalSpec{
				limitName:      store.LimitLLMTokensPerGroupMonth,
				scopeKind:      store.UsageScopeKindGroup,
				scopeID:        gid,
				projectScopeID: ev.ProjectID,
				bindingScope:   store.QuotaScopeProject,
				bindingScopeID: ev.ProjectID,
				windowStartUTC: monthStart,
				windowEndUTC:   monthEnd,
			})
		}
	}

	evalCache := newTokenLimitEvalCache(e.quota)
	for _, spec := range specs {
		if _, _, err := e.evaluateSpec(ctx, spec, evalCache); err != nil {
			e.evalErrors.Add(1)
			e.logger.Warn("token budget evaluation failed",
				"limit", spec.limitName,
				"scope_kind", spec.scopeKind,
				"scope_id", spec.scopeID,
				"project_scope_id", spec.projectScopeID,
				"window_start_utc", spec.windowStartUTC,
				"error", err)
		}
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
	bindingScope := store.QuotaScopeSystem
	bindingScopeID := ""
	if projectScopeID != "" {
		bindingScope = store.QuotaScopeProject
		bindingScopeID = projectScopeID
	}
	specs := []budgetEvalSpec{
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

		// Include every active pooled group that billingUserID effectively
		// belongs to so admission (B1) blocks on exhausted group pools without
		// further wiring (§4.4).
		if e.pooledGroups != nil {
			groupIDs, err := e.pooledGroups.ResolveForUser(ctx, billingUserID)
			if err != nil {
				return nil, fmt.Errorf("resolve pooled groups for admission check: %w", err)
			}
			for _, gid := range groupIDs {
				specs = append(specs,
					budgetEvalSpec{
						limitName:      store.LimitLLMTokensPerGroupDay,
						scopeKind:      store.UsageScopeKindGroup,
						scopeID:        gid,
						projectScopeID: "",
						bindingScope:   store.QuotaScopeSystem,
						bindingScopeID: "",
						windowStartUTC: dayStart,
						windowEndUTC:   dayEnd,
					},
					budgetEvalSpec{
						limitName:      store.LimitLLMTokensPerGroupMonth,
						scopeKind:      store.UsageScopeKindGroup,
						scopeID:        gid,
						projectScopeID: "",
						bindingScope:   store.QuotaScopeSystem,
						bindingScopeID: "",
						windowStartUTC: monthStart,
						windowEndUTC:   monthEnd,
					},
					budgetEvalSpec{
						limitName:      store.LimitLLMTokensPerGroupDay,
						scopeKind:      store.UsageScopeKindGroup,
						scopeID:        gid,
						projectScopeID: projectID,
						bindingScope:   store.QuotaScopeProject,
						bindingScopeID: projectID,
						windowStartUTC: dayStart,
						windowEndUTC:   dayEnd,
					},
					budgetEvalSpec{
						limitName:      store.LimitLLMTokensPerGroupMonth,
						scopeKind:      store.UsageScopeKindGroup,
						scopeID:        gid,
						projectScopeID: projectID,
						bindingScope:   store.QuotaScopeProject,
						bindingScopeID: projectID,
						windowStartUTC: monthStart,
						windowEndUTC:   monthEnd,
					},
				)
			}
		}
	}

	evalCache := newTokenLimitEvalCache(e.quota)
	for _, spec := range specs {
		st, action, err := e.evaluateSpec(ctx, spec, evalCache)
		if err != nil {
			return nil, err
		}
		if st == nil || st.State != store.UsageBudgetStateExhausted {
			continue
		}
		if !matchesAction(action) {
			continue
		}
		if st.OverrideUntil != nil && st.OverrideUntil.After(now) {
			continue
		}
		return &BudgetExhaustionStatus{
			Exhausted:      true,
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

	return &BudgetExhaustionStatus{Exhausted: false}, nil
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

	var capVal int64
	var action string
	if spec.scopeKind == store.UsageScopeKindGroup {
		capVal, action, err = evalCache.resolveGroupForDef(
			ctx, def, spec.scopeID, spec.bindingScope, spec.bindingScopeID,
		)
	} else {
		capVal, action, err = evalCache.resolveForDef(
			ctx, def, spec.bindingSubject, spec.bindingScope, spec.bindingScopeID,
		)
	}
	if err != nil {
		return nil, "", fmt.Errorf("resolve effective token limit %q: %w", spec.limitName, err)
	}
	// Cap <= 0 means unlimited for llm_tokens limits; no state row is created (D3).
	if capVal <= 0 {
		return nil, action, nil
	}

	key := store.UsageBudgetStateKey{
		LimitDefinitionID: def.ID,
		ScopeKind:         spec.scopeKind,
		ScopeID:           spec.scopeID,
		ProjectScopeID:    spec.projectScopeID,
		WindowStartUTC:    spec.windowStartUTC,
	}

	for attempt := 0; attempt < maxBudgetEvalCASRetries; attempt++ {
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

		existing, err := e.store.GetUsageBudgetState(ctx, key)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, "", fmt.Errorf("get usage budget state: %w", err)
		}

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
		nextState := maxBudgetState(existing.State, computedState)
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
