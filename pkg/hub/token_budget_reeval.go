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
	"runtime/debug"
	"time"

	"go.opentelemetry.io/otel/metric"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const (
	// defaultReevalPageSize is the keyset pagination page size used when
	// re-evaluating existing open-window usage_budget_state rows after a
	// system-wide, group, or project-default limit change.
	defaultReevalPageSize = 500

	// defaultReevalTimeout bounds asynchronous detached limit-change
	// re-evaluation so it survives client HTTP disconnects without running
	// unboundedly (O1).
	defaultReevalTimeout = 5 * time.Minute
)

// OTelTokenBudgetMetrics exports operational counters for raw-token budget
// evaluation and limit-change re-evaluation:
//   - scion.hub.budget.eval_errors: total evaluation/re-evaluation errors.
//   - scion.hub.budget.reeval_unprocessed: lower-bound count of open-window
//     rows left unprocessed by limit-change re-evaluation due to timeout or
//     store errors (an interruption at a page boundary or list error adds 1 as
//     a lower bound because the exact remaining count is unknown).
type OTelTokenBudgetMetrics struct {
	evalErrors        metric.Int64Counter
	reevalUnprocessed metric.Int64Counter
}

// NewOTelTokenBudgetMetrics constructs OTelTokenBudgetMetrics on mp.
func NewOTelTokenBudgetMetrics(mp metric.MeterProvider) (*OTelTokenBudgetMetrics, error) {
	if mp == nil {
		return nil, errors.New("otel token budget metrics: nil MeterProvider")
	}
	meter := mp.Meter(instrumentationScope)
	evalErrs, err := meter.Int64Counter(
		"scion.hub.budget.eval_errors",
		metric.WithUnit("{error}"),
		metric.WithDescription("Total raw-token budget evaluation and re-evaluation errors"),
	)
	if err != nil {
		return nil, err
	}
	unproc, err := meter.Int64Counter(
		"scion.hub.budget.reeval_unprocessed",
		metric.WithUnit("{row}"),
		metric.WithDescription("Lower-bound count of open-window budget state rows left unprocessed by limit-change re-evaluation"),
	)
	if err != nil {
		return nil, err
	}
	return &OTelTokenBudgetMetrics{
		evalErrors:        evalErrs,
		reevalUnprocessed: unproc,
	}, nil
}

// SetTokenBudgetMetrics wires OTelTokenBudgetMetrics onto the server's
// TokenBudgetEvaluator.
func (s *Server) SetTokenBudgetMetrics(m *OTelTokenBudgetMetrics) {
	if eval := s.TokenBudgetEvaluator(); eval != nil {
		eval.metrics.Store(m)
	}
}

func (e *TokenBudgetEvaluator) recordEvalError() {
	if e == nil {
		return
	}
	e.evalErrors.Add(1)
	if m := e.metrics.Load(); m != nil && m.evalErrors != nil {
		m.evalErrors.Add(context.Background(), 1)
	}
}

func (e *TokenBudgetEvaluator) recordReevalUnprocessed(n int64) {
	if e == nil || n <= 0 {
		return
	}
	e.reevalUnprocessed.Add(n)
	if m := e.metrics.Load(); m != nil && m.reevalUnprocessed != nil {
		m.reevalUnprocessed.Add(context.Background(), n)
	}
}

// ReevalUnprocessed returns the cumulative lower-bound count of open-window
// rows that could not be processed during limit-change re-evaluation (e.g. due
// to timeout or store errors). Note: a page-boundary timeout or list failure
// adds 1 as a lower bound because the exact remaining count is unknown.
func (e *TokenBudgetEvaluator) ReevalUnprocessed() int64 {
	if e == nil {
		return 0
	}
	return e.reevalUnprocessed.Load()
}

// WaitReeval blocks until all asynchronously scheduled limit-change
// re-evaluation goroutines have finished (used by tests and graceful shutdown).
func (e *TokenBudgetEvaluator) WaitReeval() {
	if e == nil {
		return
	}
	e.reevalWG.Wait()
}

// ScheduleReevaluateAfterEntitlementChange dispatches
// ReevaluateAfterEntitlementChange asynchronously on a detached context so
// admin HTTP writes return immediately without blocking on large re-evaluations.
func (e *TokenBudgetEvaluator) ScheduleReevaluateAfterEntitlementChange(
	ctx context.Context,
	def *store.LimitDefinition,
	binding *store.EntitlementBinding,
) {
	if !e.enabled() || def == nil || binding == nil || def.ResourceType != store.LimitResourceTypeLLMTokens {
		return
	}
	defCopy := *def
	bindingCopy := *binding
	detached := context.WithoutCancel(ctx)
	e.reevalWG.Add(1)
	go func() {
		defer e.reevalWG.Done()
		defer func() {
			if r := recover(); r != nil {
				e.recordEvalError()
				e.recordReevalUnprocessed(1)
				e.logger.Error("panic in async token budget entitlement re-evaluation",
					"limit", defCopy.Name, "panic", r, "stack", string(debug.Stack()))
			}
		}()
		e.ReevaluateAfterEntitlementChange(detached, &defCopy, &bindingCopy)
	}()
}

// ScheduleReevaluateAfterLimitDefaultChange dispatches
// ReevaluateAfterLimitDefaultChange asynchronously on a detached context so
// admin HTTP writes return immediately without blocking on large re-evaluations.
func (e *TokenBudgetEvaluator) ScheduleReevaluateAfterLimitDefaultChange(
	ctx context.Context,
	def *store.LimitDefinition,
) {
	if !e.enabled() || def == nil || def.ResourceType != store.LimitResourceTypeLLMTokens {
		return
	}
	defCopy := *def
	detached := context.WithoutCancel(ctx)
	e.reevalWG.Add(1)
	go func() {
		defer e.reevalWG.Done()
		defer func() {
			if r := recover(); r != nil {
				e.recordEvalError()
				e.recordReevalUnprocessed(1)
				e.logger.Error("panic in async token budget limit-default re-evaluation",
					"limit", defCopy.Name, "panic", r, "stack", string(debug.Stack()))
			}
		}()
		e.ReevaluateAfterLimitDefaultChange(detached, &defCopy)
	}()
}

func (e *TokenBudgetEvaluator) getReevalPageSize() int {
	if e != nil && e.reevalPageSize > 0 {
		return e.reevalPageSize
	}
	return defaultReevalPageSize
}

func (e *TokenBudgetEvaluator) getReevalTimeout() time.Duration {
	if e != nil && e.reevalTimeout > 0 {
		return e.reevalTimeout
	}
	return defaultReevalTimeout
}

var (
	// ErrBudgetWindowClosed is returned when an override targets a closed window.
	ErrBudgetWindowClosed = errors.New("budget window is already closed")
	// ErrBudgetOverrideInvalidRange is returned when override_until is outside
	// (now, window_end_utc].
	ErrBudgetOverrideInvalidRange = errors.New("override_until must be after now and at or before window_end_utc")
)

// clearUnmetThresholdBits clears threshold notification bits whose thresholds
// are no longer met after a cap is raised or removed, so a subsequent
// re-crossing notifies again.
//
// IMPORTANT:
//   - Limit-change re-evaluation calls this when a cap is raised or removed
//     (lowering consumption below previously-notified thresholds).
//   - Admin overrides do NOT clear thresholds_notified bits.
func clearUnmetThresholdBits(notified, consumed, capVal int64) int64 {
	if capVal <= 0 {
		// Unlimited: no threshold is met.
		return notified &^ (store.UsageBudgetThresholdBit50 | store.UsageBudgetThresholdBit80 | store.UsageBudgetThresholdBit100)
	}
	out := notified
	if !exceedsPercent(consumed, capVal, 50) {
		out &^= store.UsageBudgetThresholdBit50
	}
	if !exceedsPercent(consumed, capVal, 80) {
		out &^= store.UsageBudgetThresholdBit80
	}
	if consumed < capVal {
		out &^= store.UsageBudgetThresholdBit100
	}
	return out
}

// budgetWindowForLimit returns the current open UTC window [start, end) at now
// for a raw-token LimitDefinition name, or ok=false if the limit name is not a
// recognized day/month raw-token limit.
func budgetWindowForLimit(limitName string, now time.Time) (start, end time.Time, ok bool) {
	switch limitName {
	case store.LimitLLMTokensPerProjectDay,
		store.LimitLLMTokensPerUserDay,
		store.LimitLLMTokensPerUserProjectDay:
		s, e := utcDayWindow(now)
		return s, e, true
	case store.LimitLLMTokensPerProjectMonth,
		store.LimitLLMTokensPerUserMonth,
		store.LimitLLMTokensPerUserProjectMonth:
		s, e := utcMonthWindow(now)
		return s, e, true
	default:
		return time.Time{}, time.Time{}, false
	}
}

// budgetEvalSpecFromRow reconstructs a budgetEvalSpec from an existing
// usage_budget_state row and its LimitDefinition.
func budgetEvalSpecFromRow(def *store.LimitDefinition, row *store.UsageBudgetState) (budgetEvalSpec, bool) {
	if def == nil || row == nil {
		return budgetEvalSpec{}, false
	}
	spec := budgetEvalSpec{
		limitName:      def.Name,
		scopeKind:      row.ScopeKind,
		scopeID:        row.ScopeID,
		projectScopeID: row.ProjectScopeID,
		windowStartUTC: row.WindowStartUTC,
		windowEndUTC:   row.WindowEndUTC,
	}
	switch row.ScopeKind {
	case store.UsageScopeKindProject:
		spec.counterScopeID = row.ScopeID
		spec.bindingScope = store.QuotaScopeProject
		spec.bindingScopeID = row.ScopeID
	case store.UsageScopeKindUser:
		spec.counterScopeID = row.ScopeID
		spec.bindingSubject = row.ScopeID
		spec.bindingScope = store.QuotaScopeSystem
		spec.bindingScopeID = ""
	case store.UsageScopeKindUserProject:
		spec.counterScopeID = row.ScopeID + ":" + row.ProjectScopeID
		spec.bindingSubject = row.ScopeID
		spec.bindingScope = store.QuotaScopeProject
		spec.bindingScopeID = row.ProjectScopeID
	default:
		return budgetEvalSpec{}, false
	}
	return spec, true
}

// OverrideBudgetResult captures the before/after state and live view returned
// by OverrideBudgetState.
type OverrideBudgetResult struct {
	Before    *store.UsageBudgetState
	After     *store.UsageBudgetState
	LimitName string
	Consumed  int64
	Cap       int64
	Action    string
}

// OverrideBudgetState sets or clears override_until on an open usage_budget_state
// row using optimistic CAS on state_version.
//
// Semantics:
//   - Setting override_until (non-nil): requires now < override_until <= window_end_utc;
//     moves exhausted|warn -> ok, preserves thresholds_notified (override does NOT
//     clear notification bits), and bumps state_version.
//   - Clearing override_until (nil): clears override_until, recomputes state from
//     live consumption vs effective cap (so consumed >= cap returns to exhausted,
//     >= 50% to warn, else ok), preserves thresholds_notified, and bumps state_version.
func (e *TokenBudgetEvaluator) OverrideBudgetState(
	ctx context.Context,
	row *store.UsageBudgetState,
	overrideUntil *time.Time,
	expectedVersion int64,
) (*OverrideBudgetResult, error) {
	if row == nil {
		return nil, store.ErrNotFound
	}
	now := e.now()
	if !row.WindowEndUTC.After(now) {
		return nil, ErrBudgetWindowClosed
	}

	var nextOverride *time.Time
	if overrideUntil != nil {
		t := overrideUntil.UTC().Truncate(time.Microsecond)
		if !t.After(now) || t.After(row.WindowEndUTC) {
			return nil, ErrBudgetOverrideInvalidRange
		}
		nextOverride = &t
	}

	def, err := e.store.GetLimitDefinition(ctx, row.LimitDefinitionID)
	if err != nil {
		return nil, fmt.Errorf("get limit definition: %w", err)
	}
	spec, ok := budgetEvalSpecFromRow(def, row)
	if !ok {
		return nil, fmt.Errorf("%w: unsupported budget scope kind %q", store.ErrInvalidInput, row.ScopeKind)
	}

	// Structural fresh-cap rule: ResolveEffectiveTokenLimit constructs a fresh
	// uncached tokenLimitEvalCache after row has already been read.
	capVal, action, err := e.quota.ResolveEffectiveTokenLimit(
		ctx, def.ID, spec.bindingSubject, spec.bindingScope, spec.bindingScopeID,
	)
	if err != nil {
		return nil, fmt.Errorf("resolve effective token limit: %w", err)
	}

	consumed, err := e.store.SumUsageScopeCounterTokens(
		ctx, spec.scopeKind, spec.counterScopeID, spec.windowStartUTC, spec.windowEndUTC,
	)
	if err != nil {
		return nil, fmt.Errorf("sum scope counter tokens: %w", err)
	}

	var nextState string
	if nextOverride != nil {
		// Active override lowers exhausted|warn -> ok for the override period.
		// Override intentionally does NOT clear thresholds_notified bits.
		nextState = store.UsageBudgetStateOK
	} else {
		// Clearing override re-evaluates against live consumption and effective cap.
		if capVal <= 0 {
			nextState = store.UsageBudgetStateOK
		} else {
			nextState = computeBudgetState(consumed, capVal)
		}
	}

	beforeCopy := *row
	candidate := *row
	candidate.State = nextState
	candidate.ConsumedAtEval = consumed
	candidate.CapAtEval = capVal
	candidate.OverrideUntil = nextOverride
	// Override does NOT clear ThresholdsNotified.

	swapped, err := e.store.CompareAndSwapUsageBudgetState(ctx, expectedVersion, &candidate)
	if err != nil {
		return nil, err
	}

	// Fire threshold hook strictly after successful CAS on any upward transition
	// (e.g., clearing an override early when consumption crossed warn/exhausted
	// while the override was active). Downward transitions (setting an override)
	// never fire the hook.
	if budgetStateRank(swapped.State) > budgetStateRank(beforeCopy.State) &&
		(swapped.State == store.UsageBudgetStateWarn || swapped.State == store.UsageBudgetStateExhausted) {
		e.fireThresholdHook(ctx, BudgetThresholdCrossing{
			LimitName:      def.Name,
			LimitDefID:     def.ID,
			ScopeKind:      swapped.ScopeKind,
			ScopeID:        swapped.ScopeID,
			ProjectScopeID: swapped.ProjectScopeID,
			WindowStartUTC: swapped.WindowStartUTC,
			WindowEndUTC:   swapped.WindowEndUTC,
			PrevState:      beforeCopy.State,
			NewState:       swapped.State,
			Consumed:       swapped.ConsumedAtEval,
			Cap:            swapped.CapAtEval,
			TokenAction:    action,
			StateVersion:   swapped.StateVersion,
		})
	}

	return &OverrideBudgetResult{
		Before:    &beforeCopy,
		After:     swapped,
		LimitName: def.Name,
		Consumed:  consumed,
		Cap:       capVal,
		Action:    action,
	}, nil
}

// ReevaluateAfterEntitlementChange re-evaluates affected open-window budget
// states after an llm_tokens EntitlementBinding is created, updated, or deleted.
// Runs on a context detached from client HTTP cancellation with a bounded
// timeout so a client disconnect does not abandon remaining open rows. Errors
// are logged and counted via evalErrors / reevalUnprocessed; they never fail
// the already-committed admin write.
func (e *TokenBudgetEvaluator) ReevaluateAfterEntitlementChange(
	ctx context.Context,
	def *store.LimitDefinition,
	binding *store.EntitlementBinding,
) {
	if !e.enabled() || def == nil || binding == nil || def.ResourceType != store.LimitResourceTypeLLMTokens {
		return
	}
	reevalCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), e.getReevalTimeout())
	defer cancel()

	now := e.now()
	winStart, winEnd, ok := budgetWindowForLimit(def.Name, now)
	if !ok {
		return
	}

	// Targeted single-scope re-evaluation for direct user or project bindings
	// (including creating a state row when a newly added/lowered cap is already
	// exceeded by current-window consumption).
	switch {
	case binding.SubjectType == store.EntitlementSubjectSystemDefault &&
		binding.ScopeType == store.QuotaScopeProject &&
		binding.ScopeID != "" &&
		(def.Name == store.LimitLLMTokensPerProjectDay || def.Name == store.LimitLLMTokensPerProjectMonth):
		spec := budgetEvalSpec{
			limitName:      def.Name,
			scopeKind:      store.UsageScopeKindProject,
			scopeID:        binding.ScopeID,
			counterScopeID: binding.ScopeID,
			bindingScope:   store.QuotaScopeProject,
			bindingScopeID: binding.ScopeID,
			windowStartUTC: winStart,
			windowEndUTC:   winEnd,
		}
		if err := e.reevaluateSpec(reevalCtx, spec, def, now); err != nil {
			e.recordReevalUnprocessed(1)
			e.recordReevalError(spec, err)
		}
		return

	case binding.SubjectType == store.EntitlementSubjectUser &&
		binding.SubjectID != "" &&
		(def.Name == store.LimitLLMTokensPerUserDay || def.Name == store.LimitLLMTokensPerUserMonth):
		spec := budgetEvalSpec{
			limitName:      def.Name,
			scopeKind:      store.UsageScopeKindUser,
			scopeID:        binding.SubjectID,
			counterScopeID: binding.SubjectID,
			bindingSubject: binding.SubjectID,
			bindingScope:   store.QuotaScopeSystem,
			bindingScopeID: "",
			windowStartUTC: winStart,
			windowEndUTC:   winEnd,
		}
		if err := e.reevaluateSpec(reevalCtx, spec, def, now); err != nil {
			e.recordReevalUnprocessed(1)
			e.recordReevalError(spec, err)
		}
		return

	case binding.SubjectType == store.EntitlementSubjectUser &&
		binding.SubjectID != "" &&
		binding.ScopeType == store.QuotaScopeProject &&
		binding.ScopeID != "" &&
		(def.Name == store.LimitLLMTokensPerUserProjectDay || def.Name == store.LimitLLMTokensPerUserProjectMonth):
		spec := budgetEvalSpec{
			limitName:      def.Name,
			scopeKind:      store.UsageScopeKindUserProject,
			scopeID:        binding.SubjectID,
			projectScopeID: binding.ScopeID,
			counterScopeID: binding.SubjectID + ":" + binding.ScopeID,
			bindingSubject: binding.SubjectID,
			bindingScope:   store.QuotaScopeProject,
			bindingScopeID: binding.ScopeID,
			windowStartUTC: winStart,
			windowEndUTC:   winEnd,
		}
		if err := e.reevaluateSpec(reevalCtx, spec, def, now); err != nil {
			e.recordReevalUnprocessed(1)
			e.recordReevalError(spec, err)
		}
		return
	}

	// System-wide, project-default-for-user_project, or group bindings:
	// keyset-paginate through all existing open-window state rows for this
	// limit (pushing any project scope filter into the DB query).
	projectFilter := ""
	if binding.ScopeType == store.QuotaScopeProject && binding.ScopeID != "" {
		projectFilter = binding.ScopeID
	}
	e.reevaluateOpenRowsForLimit(reevalCtx, def, projectFilter, now)
}

// ReevaluateAfterLimitDefaultChange re-evaluates all existing open-window
// budget states for an llm_tokens LimitDefinition after its default_value
// changes. Runs on a context detached from client HTTP cancellation with a
// bounded timeout and keyset-paginates over every open row for the limit.
// Scopes with no state row yet are evaluated on their next ingest or admission.
func (e *TokenBudgetEvaluator) ReevaluateAfterLimitDefaultChange(ctx context.Context, def *store.LimitDefinition) {
	if !e.enabled() || def == nil || def.ResourceType != store.LimitResourceTypeLLMTokens {
		return
	}
	reevalCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), e.getReevalTimeout())
	defer cancel()

	e.reevaluateOpenRowsForLimit(reevalCtx, def, "", e.now())
}

func (e *TokenBudgetEvaluator) reevaluateOpenRowsForLimit(
	ctx context.Context,
	def *store.LimitDefinition,
	projectFilter string,
	now time.Time,
) {
	pageSize := e.getReevalPageSize()
	afterID := ""
	var processed int

	for {
		if err := ctx.Err(); err != nil {
			e.recordEvalError()
			e.recordReevalUnprocessed(1)
			e.logger.Warn("token budget re-evaluation interrupted before completing all open rows",
				"limit", def.Name,
				"limit_id", def.ID,
				"project_filter", projectFilter,
				"processed", processed,
				"after_id", afterID,
				"error", err)
			return
		}

		rows, err := e.store.ListOpenUsageBudgetStatesByLimit(ctx, def.ID, now, projectFilter, afterID, pageSize)
		if err != nil {
			e.recordEvalError()
			e.recordReevalUnprocessed(1)
			e.logger.Warn("token budget re-evaluation failed to list open rows",
				"limit", def.Name,
				"limit_id", def.ID,
				"project_filter", projectFilter,
				"processed", processed,
				"after_id", afterID,
				"error", err)
			return
		}
		if len(rows) == 0 {
			return
		}

		for i, row := range rows {
			if err := ctx.Err(); err != nil {
				remaining := int64(len(rows) - i)
				e.recordEvalError()
				e.recordReevalUnprocessed(remaining)
				e.logger.Warn("token budget re-evaluation timed out with unprocessed rows in page",
					"limit", def.Name,
					"limit_id", def.ID,
					"project_filter", projectFilter,
					"processed", processed,
					"unprocessed_in_page", remaining,
					"error", err)
				return
			}
			spec, ok := budgetEvalSpecFromRow(def, row)
			if !ok {
				continue
			}
			if err := e.reevaluateSpec(ctx, spec, def, now); err != nil {
				e.recordReevalUnprocessed(1)
				e.recordReevalError(spec, err)
			}
			processed++
		}

		if len(rows) < pageSize {
			return
		}
		afterID = rows[len(rows)-1].ID
	}
}

func (e *TokenBudgetEvaluator) recordReevalError(spec budgetEvalSpec, err error) {
	e.recordEvalError()
	e.logger.Warn("token budget re-evaluation failed",
		"limit", spec.limitName,
		"scope_kind", spec.scopeKind,
		"scope_id", spec.scopeID,
		"project_scope_id", spec.projectScopeID,
		"window_start_utc", spec.windowStartUTC,
		"error", err)
}

// reevaluateSpec recomputes the effective cap and live consumption for spec and
// updates usage_budget_state via CAS. Unlike evaluateSpec (which is strictly
// monotonic upward), reevaluateSpec is permitted to lower state (exhausted ->
// warn|ok, warn -> ok) when a cap is raised or removed, and clears
// thresholds_notified bits for thresholds no longer met.
//
// Obeys the structural fresh-cap writer rule: reads existing first and resolves
// the effective (cap, action) FRESH (uncached) on every CAS attempt.
func (e *TokenBudgetEvaluator) reevaluateSpec(
	ctx context.Context,
	spec budgetEvalSpec,
	def *store.LimitDefinition,
	now time.Time,
) error {
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
			return fmt.Errorf("get usage budget state: %w", err)
		}

		// Structural fresh-cap writer rule: resolve cap FRESH (uncached) AFTER
		// reading existing on every CAS attempt so concurrent binding edits
		// cannot write an outdated cap on retry (applies uniformly to project,
		// user, user_project, and group scopes).
		freshCache := newTokenLimitEvalCache(e.quota)
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
			return fmt.Errorf("resolve effective token limit %q: %w", spec.limitName, err)
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
			return fmt.Errorf("sum scope counter tokens: %w", err)
		}

		if existing == nil {
			// No row exists yet. Only create one if a finite cap applies and
			// consumption is already non-zero (so lowering/adding a cap below
			// existing consumption immediately records warn/exhausted).
			if capVal <= 0 || consumed <= 0 {
				return nil
			}
			computedState := computeBudgetState(consumed, capVal)
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
					continue
				}
				return fmt.Errorf("create usage budget state: %w", createErr)
			}
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
			return nil
		}

		var nextState string
		if existing.OverrideUntil != nil && existing.OverrideUntil.After(now) {
			// Active override keeps state at ok until override_until elapses.
			nextState = existing.State
		} else if capVal <= 0 {
			// Cap removed (unlimited): lower state to ok.
			nextState = store.UsageBudgetStateOK
		} else {
			nextState = computeBudgetState(consumed, capVal)
		}

		// Clear thresholds_notified bits for any threshold no longer met under
		// the current effective cap (including when an ingest between the
		// binding commit and re-evaluation already updated CapAtEval). Because
		// consumption never decreases within a window, an unmet previously-set
		// bit can only result from a cap raise or removal.
		// Admin overrides do NOT call reevaluateSpec and preserve bits.
		nextNotified := clearUnmetThresholdBits(existing.ThresholdsNotified, consumed, capVal)

		if nextState == existing.State &&
			consumed == existing.ConsumedAtEval &&
			capVal == existing.CapAtEval &&
			nextNotified == existing.ThresholdsNotified {
			return nil
		}

		candidate := *existing
		candidate.State = nextState
		candidate.ConsumedAtEval = consumed
		candidate.CapAtEval = capVal
		candidate.ThresholdsNotified = nextNotified

		swapped, swapErr := e.store.CompareAndSwapUsageBudgetState(ctx, existing.StateVersion, &candidate)
		if swapErr != nil {
			if errors.Is(swapErr, store.ErrBudgetStateConflict) {
				continue
			}
			return fmt.Errorf("cas usage budget state: %w", swapErr)
		}

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
		return nil
	}

	return fmt.Errorf("usage budget state CAS retries exhausted during re-evaluation for %s (%s/%s)",
		spec.limitName, spec.scopeKind, spec.scopeID)
}
