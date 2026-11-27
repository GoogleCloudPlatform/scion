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

//go:build !no_sqlite

package hub

import (
	"context"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func tbStrPtr(s string) *string { return &s }
func tbI64Ptr(v int64) *int64   { return &v }

func TestExceedsPercent_NoOverflow(t *testing.T) {
	maxCap := int64(math.MaxInt64)
	halfFloor := maxCap / 2   // 4611686018427387903 (< 50% of MaxInt64 because MaxInt64 is odd)
	halfCeil := halfFloor + 1 // 4611686018427387904 (> 50% of MaxInt64)

	assert.False(t, exceedsPercent(halfFloor, maxCap, 50), "floor(MaxInt64/2) is strictly below 50%")
	assert.True(t, exceedsPercent(halfCeil, maxCap, 50), "ceil(MaxInt64/2) is >= 50%")
	assert.False(t, exceedsPercent(maxCap-1, maxCap, 100), "MaxInt64-1 is below 100%")
	assert.True(t, exceedsPercent(maxCap, maxCap, 100), "MaxInt64 is 100%")

	// Even near-MaxInt64 cap where 50% is exact.
	evenCap := int64(math.MaxInt64 - 1)
	assert.False(t, exceedsPercent(evenCap/2-1, evenCap, 50))
	assert.True(t, exceedsPercent(evenCap/2, evenCap, 50))
	assert.Equal(t, store.UsageBudgetStateOK, computeBudgetState(evenCap/2-1, evenCap))
	assert.Equal(t, store.UsageBudgetStateWarn, computeBudgetState(evenCap/2, evenCap))
	assert.Equal(t, store.UsageBudgetStateExhausted, computeBudgetState(evenCap, evenCap))
}

func setupBudgetEvalTest(t *testing.T, now time.Time) (store.Store, *QuotaService, *TokenBudgetEvaluator) {
	t.Helper()
	s, err := newTestStore(t, ":memory:")
	require.NoError(t, err)
	ctx := context.Background()
	seedTokenBudgetLimitDefinitions(ctx, s)
	qs := &QuotaService{store: s}
	eval := NewTokenBudgetEvaluator(s, qs, nil, func() bool { return true })
	eval.nowFn = func() time.Time { return now }
	return s, qs, eval
}

func ingestTestTokens(t *testing.T, s store.Store, projectID string, billingUserID *string, ts time.Time, tokens int64, anomalous bool) *store.UsageEvent {
	t.Helper()
	ctx := context.Background()
	runID := "run-" + uuid.NewString()
	agentID := uuid.NewString()
	_, err := s.EnsureRunAttribution(ctx, &store.RunAttribution{
		RunID:         runID,
		HubID:         "hub-test",
		ProjectID:     projectID,
		AgentID:       agentID,
		BillingUserID: billingUserID,
		Harness:       "claude",
	})
	require.NoError(t, err)

	ev := &store.UsageEvent{
		HubID:         "hub-test",
		EventID:       "ev-" + uuid.NewString(),
		RunID:         runID,
		ProjectID:     projectID,
		AgentID:       agentID,
		BillingUserID: billingUserID,
		Harness:       "claude",
		Model:         "claude-sonnet-4-6",
		ModelSource:   "event",
		UsageSource:   store.UsageSourceNative,
		Status:        "success",
		Calls:         1,
		Input:         tbI64Ptr(tokens),
		TokenStatus:   "reported",
		EventTS:       ts,
		ReceivedAt:    ts,
		Anomalous:     anomalous,
	}
	res, err := s.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agentID,
		Events:  []*store.UsageEvent{ev},
	})
	require.NoError(t, err)
	require.Len(t, res.InsertedEvents, 1)
	return res.InsertedEvents[0]
}

func TestTokenBudgetEvaluator_DayVsMonthWindowsAndClosedWindowSkip(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 27, 15, 0, 0, 0, time.UTC)
	s, _, eval := setupBudgetEvalTest(t, now)

	projID := "proj-win"
	dayDef, err := s.GetLimitDefinitionByName(ctx, store.LimitLLMTokensPerProjectDay)
	require.NoError(t, err)
	monthDef, err := s.GetLimitDefinitionByName(ctx, store.LimitLLMTokensPerProjectMonth)
	require.NoError(t, err)

	// Day cap = 1000, Month cap = 2000.
	_, err = s.CreateEntitlementBinding(ctx, &store.EntitlementBinding{
		LimitDefinitionID: dayDef.ID,
		SubjectType:       store.EntitlementSubjectSystemDefault,
		ScopeType:         store.QuotaScopeProject,
		ScopeID:           projID,
		Value:             1000,
	})
	require.NoError(t, err)
	_, err = s.CreateEntitlementBinding(ctx, &store.EntitlementBinding{
		LimitDefinitionID: monthDef.ID,
		SubjectType:       store.EntitlementSubjectSystemDefault,
		ScopeType:         store.QuotaScopeProject,
		ScopeID:           projID,
		Value:             2000,
	})
	require.NoError(t, err)

	// 1. Late event from previous month (closed day AND closed month) -> must NOT create any state rows.
	prevMonthTS := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	evPrevMonth := ingestTestTokens(t, s, projID, nil, prevMonthTS, 5000, false)
	eval.EvaluateIngestedEvents(ctx, []*store.UsageEvent{evPrevMonth})

	_, err = s.GetUsageBudgetState(ctx, store.UsageBudgetStateKey{
		LimitDefinitionID: dayDef.ID, ScopeKind: store.UsageScopeKindProject,
		ScopeID: projID, WindowStartUTC: time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC),
	})
	require.ErrorIs(t, err, store.ErrNotFound, "closed day window must not create state row")
	_, err = s.GetUsageBudgetState(ctx, store.UsageBudgetStateKey{
		LimitDefinitionID: monthDef.ID, ScopeKind: store.UsageScopeKindProject,
		ScopeID: projID, WindowStartUTC: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
	})
	require.ErrorIs(t, err, store.ErrNotFound, "closed month window must not create state row")

	// 2. Late event from yesterday (closed day, OPEN current month) -> month row created, closed day skipped!
	yesterdayTS := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	evYesterday := ingestTestTokens(t, s, projID, nil, yesterdayTS, 1100, false)
	eval.EvaluateIngestedEvents(ctx, []*store.UsageEvent{evYesterday})

	_, err = s.GetUsageBudgetState(ctx, store.UsageBudgetStateKey{
		LimitDefinitionID: dayDef.ID, ScopeKind: store.UsageScopeKindProject,
		ScopeID: projID, WindowStartUTC: time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC),
	})
	require.ErrorIs(t, err, store.ErrNotFound, "closed yesterday day window must not create state row")

	monthState, err := s.GetUsageBudgetState(ctx, store.UsageBudgetStateKey{
		LimitDefinitionID: monthDef.ID, ScopeKind: store.UsageScopeKindProject,
		ScopeID: projID, WindowStartUTC: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	assert.Equal(t, store.UsageBudgetStateWarn, monthState.State) // 1100 / 2000 = 55%
	assert.Equal(t, int64(1100), monthState.ConsumedAtEval)

	// 3. Today's event of 950 -> day = 950/1000 (warn), month = 1100+950 = 2050/2000 (exhausted).
	evToday := ingestTestTokens(t, s, projID, nil, now, 950, false)
	eval.EvaluateIngestedEvents(ctx, []*store.UsageEvent{evToday})

	dayState, err := s.GetUsageBudgetState(ctx, store.UsageBudgetStateKey{
		LimitDefinitionID: dayDef.ID, ScopeKind: store.UsageScopeKindProject,
		ScopeID: projID, WindowStartUTC: time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	assert.Equal(t, store.UsageBudgetStateWarn, dayState.State)
	assert.Equal(t, int64(950), dayState.ConsumedAtEval)

	monthState, err = s.GetUsageBudgetState(ctx, store.UsageBudgetStateKey{
		LimitDefinitionID: monthDef.ID, ScopeKind: store.UsageScopeKindProject,
		ScopeID: projID, WindowStartUTC: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	assert.Equal(t, store.UsageBudgetStateExhausted, monthState.State)
	assert.Equal(t, int64(2050), monthState.ConsumedAtEval)
}

func TestTokenBudgetEvaluator_UnlimitedZeroAndNullBillingUser(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 27, 15, 0, 0, 0, time.UTC)
	s, _, eval := setupBudgetEvalTest(t, now)

	projID := "proj-unlim"
	userID := "user-1"

	// All default_value = 0 (unlimited) -> no state rows created.
	ev := ingestTestTokens(t, s, projID, &userID, now, 50000, false)
	eval.EvaluateIngestedEvents(ctx, []*store.UsageEvent{ev})

	projStates, err := s.ListUsageBudgetStatesForScope(ctx, store.UsageScopeKindProject, projID, "", now)
	require.NoError(t, err)
	assert.Empty(t, projStates)

	userStates, err := s.ListUsageBudgetStatesForScope(ctx, store.UsageScopeKindUser, userID, "", now)
	require.NoError(t, err)
	assert.Empty(t, userStates)

	// Configure project and user caps, then ingest an autonomous event (billing_user_id == nil).
	projDayDef, err := s.GetLimitDefinitionByName(ctx, store.LimitLLMTokensPerProjectDay)
	require.NoError(t, err)
	userDayDef, err := s.GetLimitDefinitionByName(ctx, store.LimitLLMTokensPerUserDay)
	require.NoError(t, err)
	userDayDef.DefaultValue = 1000
	_, err = s.UpdateLimitDefinition(ctx, userDayDef)
	require.NoError(t, err)
	projDayDef.DefaultValue = 1000
	_, err = s.UpdateLimitDefinition(ctx, projDayDef)
	require.NoError(t, err)

	evAnon := ingestTestTokens(t, s, "proj-anon", nil, now, 1200, false)
	eval.EvaluateIngestedEvents(ctx, []*store.UsageEvent{evAnon})

	anonProjStates, err := s.ListUsageBudgetStatesForScope(ctx, store.UsageScopeKindProject, "proj-anon", "", now)
	require.NoError(t, err)
	require.Len(t, anonProjStates, 1)
	assert.Equal(t, store.UsageBudgetStateExhausted, anonProjStates[0].State)

	// No user or user_project rows created for nil billing_user_id.
	userStatesAfter, err := s.ListUsageBudgetStatesForScope(ctx, store.UsageScopeKindUser, "", "", now)
	require.NoError(t, err)
	assert.Empty(t, userStatesAfter)
}

func TestTokenBudgetEvaluator_MostGenerousBindingAndActionTieBreak(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 27, 15, 0, 0, 0, time.UTC)
	s, qs, _ := setupBudgetEvalTest(t, now)

	userDayDef, err := s.GetLimitDefinitionByName(ctx, store.LimitLLMTokensPerUserDay)
	require.NoError(t, err)

	u := &store.User{ID: uuid.NewString(), Email: "u@example.com", DisplayName: "U", Role: "member", Status: "active"}
	require.NoError(t, s.CreateUser(ctx, u))
	g1 := &store.Group{ID: uuid.NewString(), Name: "G1", Slug: "g1", GroupType: store.GroupTypeExplicit}
	g2 := &store.Group{ID: uuid.NewString(), Name: "G2", Slug: "g2", GroupType: store.GroupTypeExplicit}
	require.NoError(t, s.CreateGroup(ctx, g1))
	require.NoError(t, s.CreateGroup(ctx, g2))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{GroupID: g1.ID, MemberType: store.GroupMemberTypeUser, MemberID: u.ID, Role: store.GroupMemberRoleMember}))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{GroupID: g2.ID, MemberType: store.GroupMemberTypeUser, MemberID: u.ID, Role: store.GroupMemberRoleMember}))

	// System default: 1000, suspend_and_pause.
	_, err = s.CreateEntitlementBinding(ctx, &store.EntitlementBinding{
		LimitDefinitionID: userDayDef.ID,
		SubjectType:       store.EntitlementSubjectSystemDefault,
		ScopeType:         store.QuotaScopeSystem,
		Value:             1000,
		TokenAction:       tbStrPtr(store.TokenActionSuspendAndPause),
	})
	require.NoError(t, err)

	// Group 1: 5000, suspend_and_pause (unique winner -> suspend_and_pause wins).
	_, err = s.CreateEntitlementBinding(ctx, &store.EntitlementBinding{
		LimitDefinitionID: userDayDef.ID,
		SubjectType:       store.EntitlementSubjectGroup,
		SubjectID:         g1.ID,
		ScopeType:         store.QuotaScopeSystem,
		Value:             5000,
		TokenAction:       tbStrPtr(store.TokenActionSuspendAndPause),
	})
	require.NoError(t, err)

	capVal, act, err := qs.ResolveEffectiveTokenLimit(ctx, userDayDef.ID, u.ID, store.QuotaScopeSystem, "")
	require.NoError(t, err)
	assert.Equal(t, int64(5000), capVal)
	assert.Equal(t, store.TokenActionSuspendAndPause, act)

	// Group 2 ties at 5000 with NULL (= pause_new) -> tie-break most permissive: pause_new > suspend_and_pause.
	_, err = s.CreateEntitlementBinding(ctx, &store.EntitlementBinding{
		LimitDefinitionID: userDayDef.ID,
		SubjectType:       store.EntitlementSubjectGroup,
		SubjectID:         g2.ID,
		ScopeType:         store.QuotaScopeSystem,
		Value:             5000,
		TokenAction:       nil,
	})
	require.NoError(t, err)

	capVal, act, err = qs.ResolveEffectiveTokenLimit(ctx, userDayDef.ID, u.ID, store.QuotaScopeSystem, "")
	require.NoError(t, err)
	assert.Equal(t, int64(5000), capVal)
	assert.Equal(t, store.TokenActionPauseNew, act)

	// User binding ties at 5000 with notify_only -> tie-break most permissive: notify_only > pause_new.
	ub, err := s.CreateEntitlementBinding(ctx, &store.EntitlementBinding{
		LimitDefinitionID: userDayDef.ID,
		SubjectType:       store.EntitlementSubjectUser,
		SubjectID:         u.ID,
		ScopeType:         store.QuotaScopeSystem,
		Value:             5000,
		TokenAction:       tbStrPtr(store.TokenActionNotifyOnly),
	})
	require.NoError(t, err)

	capVal, act, err = qs.ResolveEffectiveTokenLimit(ctx, userDayDef.ID, u.ID, store.QuotaScopeSystem, "")
	require.NoError(t, err)
	assert.Equal(t, int64(5000), capVal)
	assert.Equal(t, store.TokenActionNotifyOnly, act)

	// Unlimited (0) binding overrides everything to 0.
	ub.Value = 0
	_, err = s.UpdateEntitlementBinding(ctx, ub)
	require.NoError(t, err)
	capVal, _, err = qs.ResolveEffectiveTokenLimit(ctx, userDayDef.ID, u.ID, store.QuotaScopeSystem, "")
	require.NoError(t, err)
	assert.Equal(t, int64(0), capVal)
}

func TestTokenBudgetEvaluator_AnomalousExcluded(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 27, 15, 0, 0, 0, time.UTC)
	s, _, eval := setupBudgetEvalTest(t, now)

	projID := "proj-anom"
	projDayDef, err := s.GetLimitDefinitionByName(ctx, store.LimitLLMTokensPerProjectDay)
	require.NoError(t, err)
	_, err = s.CreateEntitlementBinding(ctx, &store.EntitlementBinding{
		LimitDefinitionID: projDayDef.ID,
		SubjectType:       store.EntitlementSubjectSystemDefault,
		ScopeType:         store.QuotaScopeProject,
		ScopeID:           projID,
		Value:             1000,
	})
	require.NoError(t, err)

	// Anomalous spike (>50M tokens) is excluded from usage_scope_counters_daily,
	// so consumption stays 0 and state remains ok.
	evSpike := ingestTestTokens(t, s, projID, nil, now, 60_000_000, true)
	eval.EvaluateIngestedEvents(ctx, []*store.UsageEvent{evSpike})

	st, err := s.GetUsageBudgetState(ctx, store.UsageBudgetStateKey{
		LimitDefinitionID: projDayDef.ID,
		ScopeKind:         store.UsageScopeKindProject,
		ScopeID:           projID,
		WindowStartUTC:    time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	assert.Equal(t, store.UsageBudgetStateOK, st.State)
	assert.Equal(t, int64(0), st.ConsumedAtEval)
}

func TestTokenBudgetEvaluator_MonotonicityAndConcurrentConvergence(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 27, 15, 0, 0, 0, time.UTC)
	s, _, eval := setupBudgetEvalTest(t, now)

	projID := "proj-conc"
	projDayDef, err := s.GetLimitDefinitionByName(ctx, store.LimitLLMTokensPerProjectDay)
	require.NoError(t, err)
	b, err := s.CreateEntitlementBinding(ctx, &store.EntitlementBinding{
		LimitDefinitionID: projDayDef.ID,
		SubjectType:       store.EntitlementSubjectSystemDefault,
		ScopeType:         store.QuotaScopeProject,
		ScopeID:           projID,
		Value:             1000,
	})
	require.NoError(t, err)

	var hookMu sync.Mutex
	var crossings []BudgetThresholdCrossing
	eval.SetThresholdHook(func(_ context.Context, c BudgetThresholdCrossing) {
		hookMu.Lock()
		crossings = append(crossings, c)
		hookMu.Unlock()
	})

	// Ingest 1200 tokens (exhausts 1000 cap).
	ev := ingestTestTokens(t, s, projID, nil, now, 1200, false)

	// Launch 10 concurrent evaluators on the same scope.
	const workers = 10
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			eval.EvaluateIngestedEvents(ctx, []*store.UsageEvent{ev})
		}()
	}
	close(start)
	wg.Wait()

	assert.Equal(t, int64(0), eval.EvalErrors(), "concurrent evaluators should converge without errors")

	st, err := s.GetUsageBudgetState(ctx, store.UsageBudgetStateKey{
		LimitDefinitionID: projDayDef.ID,
		ScopeKind:         store.UsageScopeKindProject,
		ScopeID:           projID,
		WindowStartUTC:    time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	assert.Equal(t, store.UsageBudgetStateExhausted, st.State)
	assert.Equal(t, int64(1200), st.ConsumedAtEval)

	// Hook must have fired EXACTLY ONCE for the "" -> exhausted crossing.
	hookMu.Lock()
	require.Len(t, crossings, 1, "exactly one threshold crossing hook call even under concurrency")
	assert.Equal(t, "", crossings[0].PrevState)
	assert.Equal(t, store.UsageBudgetStateExhausted, crossings[0].NewState)
	assert.Equal(t, store.TokenActionPauseNew, crossings[0].TokenAction)
	hookMu.Unlock()

	// Monotonicity: even if the cap is raised in the DB, the inline evaluator
	// NEVER lowers state from exhausted back to ok/warn (only A2's limit-change
	// handler may lower state).
	b.Value = 10000
	_, err = s.UpdateEntitlementBinding(ctx, b)
	require.NoError(t, err)

	ev2 := ingestTestTokens(t, s, projID, nil, now, 100, false)
	eval.EvaluateIngestedEvents(ctx, []*store.UsageEvent{ev2})

	stAfter, err := s.GetUsageBudgetState(ctx, store.UsageBudgetStateKey{
		LimitDefinitionID: projDayDef.ID,
		ScopeKind:         store.UsageScopeKindProject,
		ScopeID:           projID,
		WindowStartUTC:    time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	assert.Equal(t, store.UsageBudgetStateExhausted, stAfter.State, "evaluator must never lower state within a window")
	assert.Equal(t, int64(1300), stAfter.ConsumedAtEval)

	// O1: consumed-only CAS while already exhausted must NOT fire another crossing.
	hookMu.Lock()
	assert.Len(t, crossings, 1, "consumed-only CAS on exhausted row must not fire another threshold crossing")
	hookMu.Unlock()
}

func TestTokenBudgetEvaluator_ConsumedOnlyCASFiresNoHook(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 27, 15, 0, 0, 0, time.UTC)
	s, _, eval := setupBudgetEvalTest(t, now)

	projID := "proj-warn-stay"
	projDayDef, err := s.GetLimitDefinitionByName(ctx, store.LimitLLMTokensPerProjectDay)
	require.NoError(t, err)
	_, err = s.CreateEntitlementBinding(ctx, &store.EntitlementBinding{
		LimitDefinitionID: projDayDef.ID,
		SubjectType:       store.EntitlementSubjectSystemDefault,
		ScopeType:         store.QuotaScopeProject,
		ScopeID:           projID,
		Value:             1000,
	})
	require.NoError(t, err)

	var crossings []BudgetThresholdCrossing
	eval.SetThresholdHook(func(_ context.Context, c BudgetThresholdCrossing) {
		crossings = append(crossings, c)
	})

	// First ingest: 600/1000 -> warn (1 crossing: "" -> warn).
	ev1 := ingestTestTokens(t, s, projID, nil, now, 600, false)
	eval.EvaluateIngestedEvents(ctx, []*store.UsageEvent{ev1})
	require.Len(t, crossings, 1)
	assert.Equal(t, store.UsageBudgetStateWarn, crossings[0].NewState)

	// Second ingest: +200 -> 800/1000 (warn stays warn; CAS bumps consumed_at_eval & state_version).
	ev2 := ingestTestTokens(t, s, projID, nil, now, 200, false)
	eval.EvaluateIngestedEvents(ctx, []*store.UsageEvent{ev2})

	st, err := s.GetUsageBudgetState(ctx, store.UsageBudgetStateKey{
		LimitDefinitionID: projDayDef.ID,
		ScopeKind:         store.UsageScopeKindProject,
		ScopeID:           projID,
		WindowStartUTC:    time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	assert.Equal(t, store.UsageBudgetStateWarn, st.State)
	assert.Equal(t, int64(800), st.ConsumedAtEval)
	assert.Equal(t, int64(2), st.StateVersion)
	assert.Len(t, crossings, 1, "warn-stays-warn consumed-only CAS must not fire a new crossing")
}

type countingBudgetStore struct {
	store.Store
	listDefs       int
	getDefByName   int
	getDefByID     int
	listForSubject int
	getUserGroups  int
}

func (c *countingBudgetStore) ListLimitDefinitions(ctx context.Context) ([]*store.LimitDefinition, error) {
	c.listDefs++
	return c.Store.ListLimitDefinitions(ctx)
}

func (c *countingBudgetStore) GetLimitDefinitionByName(ctx context.Context, name string) (*store.LimitDefinition, error) {
	c.getDefByName++
	return c.Store.GetLimitDefinitionByName(ctx, name)
}

func (c *countingBudgetStore) GetLimitDefinition(ctx context.Context, id string) (*store.LimitDefinition, error) {
	c.getDefByID++
	return c.Store.GetLimitDefinition(ctx, id)
}

func (c *countingBudgetStore) ListEntitlementBindingsForSubject(ctx context.Context, subjectType, subjectID string) ([]*store.EntitlementBinding, error) {
	c.listForSubject++
	return c.Store.ListEntitlementBindingsForSubject(ctx, subjectType, subjectID)
}

func (c *countingBudgetStore) GetUserGroups(ctx context.Context, userID string) ([]store.GroupMember, error) {
	c.getUserGroups++
	return c.Store.GetUserGroups(ctx, userID)
}

func TestTokenBudgetEvaluator_QueryCountBatchLoaded(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 27, 15, 0, 0, 0, time.UTC)
	baseStore, err := newTestStore(t, ":memory:")
	require.NoError(t, err)
	seedTokenBudgetLimitDefinitions(ctx, baseStore)

	u := &store.User{ID: uuid.NewString(), Email: "qc@example.com", DisplayName: "QC", Role: "member", Status: "active"}
	require.NoError(t, baseStore.CreateUser(ctx, u))
	for _, slug := range []string{"qc-g1", "qc-g2"} {
		g := &store.Group{ID: uuid.NewString(), Name: slug, Slug: slug, GroupType: store.GroupTypeExplicit}
		require.NoError(t, baseStore.CreateGroup(ctx, g))
		require.NoError(t, baseStore.AddGroupMember(ctx, &store.GroupMember{
			GroupID: g.ID, MemberType: store.GroupMemberTypeUser, MemberID: u.ID, Role: store.GroupMemberRoleMember,
		}))
	}

	evWithUser := ingestTestTokens(t, baseStore, "proj-qc", &u.ID, now, 100, false)
	evAnon := ingestTestTokens(t, baseStore, "proj-qc", nil, now, 100, false)

	cs := &countingBudgetStore{Store: baseStore}
	qs := &QuotaService{store: cs}
	eval := NewTokenBudgetEvaluator(cs, qs, nil, func() bool { return true })
	eval.nowFn = func() time.Time { return now }

	// All caps = 0, billing user in G=2 groups:
	// 1 ListLimitDefinitions + 1 system_default + 1 user + 1 GetUserGroups + 2 group = 6 queries (was 26 + 4*2 = 34).
	eval.EvaluateIngestedEvents(ctx, []*store.UsageEvent{evWithUser})
	assert.Equal(t, 1, cs.listDefs)
	assert.Equal(t, 0, cs.getDefByName)
	assert.Equal(t, 0, cs.getDefByID)
	assert.Equal(t, 4, cs.listForSubject, "1 system_default + 1 user + 2 groups")
	assert.Equal(t, 1, cs.getUserGroups)

	// All caps = 0, NULL billing user:
	// 1 ListLimitDefinitions + 1 system_default = 2 queries.
	cs.listDefs, cs.getDefByName, cs.getDefByID, cs.listForSubject, cs.getUserGroups = 0, 0, 0, 0, 0
	eval.EvaluateIngestedEvents(ctx, []*store.UsageEvent{evAnon})
	assert.Equal(t, 1, cs.listDefs)
	assert.Equal(t, 0, cs.getDefByName)
	assert.Equal(t, 0, cs.getDefByID)
	assert.Equal(t, 1, cs.listForSubject, "only system_default")
	assert.Equal(t, 0, cs.getUserGroups)
}

func TestTokenBudgetEvaluator_IsScopeExhaustedForAction(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 27, 15, 0, 0, 0, time.UTC)
	s, _, eval := setupBudgetEvalTest(t, now)

	projID := "proj-adm"
	userID := "user-adm"

	projDayDef, err := s.GetLimitDefinitionByName(ctx, store.LimitLLMTokensPerProjectDay)
	require.NoError(t, err)
	upDayDef, err := s.GetLimitDefinitionByName(ctx, store.LimitLLMTokensPerUserProjectDay)
	require.NoError(t, err)

	// Project day cap = 1000 with notify_only.
	_, err = s.CreateEntitlementBinding(ctx, &store.EntitlementBinding{
		LimitDefinitionID: projDayDef.ID,
		SubjectType:       store.EntitlementSubjectSystemDefault,
		ScopeType:         store.QuotaScopeProject,
		ScopeID:           projID,
		Value:             1000,
		TokenAction:       tbStrPtr(store.TokenActionNotifyOnly),
	})
	require.NoError(t, err)

	// User-project day cap = 2000 with suspend_and_pause.
	_, err = s.CreateEntitlementBinding(ctx, &store.EntitlementBinding{
		LimitDefinitionID: upDayDef.ID,
		SubjectType:       store.EntitlementSubjectUser,
		SubjectID:         userID,
		ScopeType:         store.QuotaScopeProject,
		ScopeID:           projID,
		Value:             2000,
		TokenAction:       tbStrPtr(store.TokenActionSuspendAndPause),
	})
	require.NoError(t, err)

	// Ingest 1500 tokens WITHOUT calling EvaluateIngestedEvents first —
	// tests on-demand self-heal (D4)!
	ingestTestTokens(t, s, projID, &userID, now, 1500, false)

	// Project cap (1000, notify_only) is exhausted, but querying blocking actions
	// (pause_new, suspend_and_pause) returns Exhausted=false.
	stBlock, err := eval.IsScopeExhaustedForAction(ctx, projID, userID, now, store.TokenActionPauseNew, store.TokenActionSuspendAndPause)
	require.NoError(t, err)
	assert.False(t, stBlock.Exhausted)

	// Querying notify_only returns Exhausted=true for project day limit.
	stNotify, err := eval.IsScopeExhaustedForAction(ctx, projID, userID, now, store.TokenActionNotifyOnly)
	require.NoError(t, err)
	assert.True(t, stNotify.Exhausted)
	assert.Equal(t, store.LimitLLMTokensPerProjectDay, stNotify.LimitName)
	assert.Equal(t, store.TokenActionNotifyOnly, stNotify.TokenAction)

	// Ingest another 600 tokens -> total 2100 exhausts user_project (2000, suspend_and_pause).
	ingestTestTokens(t, s, projID, &userID, now, 600, false)

	stBlock, err = eval.IsScopeExhaustedForAction(ctx, projID, userID, now, store.TokenActionPauseNew, store.TokenActionSuspendAndPause)
	require.NoError(t, err)
	assert.True(t, stBlock.Exhausted)
	assert.Equal(t, store.LimitLLMTokensPerUserProjectDay, stBlock.LimitName)
	assert.Equal(t, store.UsageScopeKindUserProject, stBlock.ScopeKind)
	assert.Equal(t, userID, stBlock.ScopeID)
	assert.Equal(t, projID, stBlock.ProjectScopeID)
	assert.Equal(t, store.TokenActionSuspendAndPause, stBlock.TokenAction)

	// Setting override_until in the future clears exhaustion for admission.
	upState, err := s.GetUsageBudgetState(ctx, store.UsageBudgetStateKey{
		LimitDefinitionID: upDayDef.ID,
		ScopeKind:         store.UsageScopeKindUserProject,
		ScopeID:           userID,
		ProjectScopeID:    projID,
		WindowStartUTC:    time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	until := now.Add(time.Hour)
	upState.OverrideUntil = &until
	_, err = s.CompareAndSwapUsageBudgetState(ctx, upState.StateVersion, upState)
	require.NoError(t, err)

	stOverridden, err := eval.IsScopeExhaustedForAction(ctx, projID, userID, now, store.TokenActionPauseNew, store.TokenActionSuspendAndPause)
	require.NoError(t, err)
	assert.False(t, stOverridden.Exhausted, "active override_until must suppress exhaustion")

	// N7: passing a past timestamp (closed month/day) is clamped to evaluator
	// clock and must never create closed-window state rows.
	pastTime := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	_, err = eval.IsScopeExhaustedForAction(ctx, projID, userID, pastTime, store.TokenActionPauseNew, store.TokenActionSuspendAndPause)
	require.NoError(t, err)
	_, err = s.GetUsageBudgetState(ctx, store.UsageBudgetStateKey{
		LimitDefinitionID: projDayDef.ID,
		ScopeKind:         store.UsageScopeKindProject,
		ScopeID:           projID,
		WindowStartUTC:    time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC),
	})
	require.ErrorIs(t, err, store.ErrNotFound, "past now passed to IsScopeExhaustedForAction must not create closed-window rows")

	// When disabled, returns Exhausted=false.
	eval.enabledFn = func() bool { return false }
	stDisabled, err := eval.IsScopeExhaustedForAction(ctx, projID, userID, now, store.TokenActionNotifyOnly)
	require.NoError(t, err)
	assert.False(t, stDisabled.Exhausted)
}
