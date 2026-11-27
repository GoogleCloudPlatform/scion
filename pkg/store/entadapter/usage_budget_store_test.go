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

package entadapter

import (
	"context"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

func TestUsageBudgetStore_CRUDAndUniqueKey(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	limitDefID := uuid.NewString()
	dayStart := time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC)
	dayEnd := dayStart.AddDate(0, 0, 1)
	monthStart := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	monthEnd := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)

	created, err := cs.CreateUsageBudgetState(ctx, &store.UsageBudgetState{
		LimitDefinitionID: limitDefID,
		ScopeKind:         store.UsageScopeKindUserProject,
		ScopeID:           "user-1",
		ProjectScopeID:    "proj-1",
		WindowStartUTC:    dayStart,
		WindowEndUTC:      dayEnd,
		State:             store.UsageBudgetStateOK,
		ConsumedAtEval:    1000,
		CapAtEval:         10000,
	})
	require.NoError(t, err)
	assert.NotEmpty(t, created.ID)
	assert.Equal(t, int64(1), created.StateVersion)
	assert.Equal(t, store.UsageBudgetStateOK, created.State)
	assert.Equal(t, int64(1000), created.ConsumedAtEval)
	assert.Equal(t, int64(10000), created.CapAtEval)

	// Duplicate composite key must return ErrAlreadyExists.
	_, err = cs.CreateUsageBudgetState(ctx, &store.UsageBudgetState{
		LimitDefinitionID: limitDefID,
		ScopeKind:         store.UsageScopeKindUserProject,
		ScopeID:           "user-1",
		ProjectScopeID:    "proj-1",
		WindowStartUTC:    dayStart,
		WindowEndUTC:      dayEnd,
		State:             store.UsageBudgetStateWarn,
		ConsumedAtEval:    5000,
		CapAtEval:         10000,
	})
	require.ErrorIs(t, err, store.ErrAlreadyExists)

	// Different project_scope_id for same user_project is distinct.
	otherProj, err := cs.CreateUsageBudgetState(ctx, &store.UsageBudgetState{
		LimitDefinitionID: limitDefID,
		ScopeKind:         store.UsageScopeKindUserProject,
		ScopeID:           "user-1",
		ProjectScopeID:    "proj-2",
		WindowStartUTC:    dayStart,
		WindowEndUTC:      dayEnd,
		State:             store.UsageBudgetStateOK,
		ConsumedAtEval:    200,
		CapAtEval:         10000,
	})
	require.NoError(t, err)
	assert.NotEqual(t, created.ID, otherProj.ID)

	// Different window (month) for same (limit, user, proj-1) is distinct.
	monthRow, err := cs.CreateUsageBudgetState(ctx, &store.UsageBudgetState{
		LimitDefinitionID: limitDefID,
		ScopeKind:         store.UsageScopeKindUserProject,
		ScopeID:           "user-1",
		ProjectScopeID:    "proj-1",
		WindowStartUTC:    monthStart,
		WindowEndUTC:      monthEnd,
		State:             store.UsageBudgetStateWarn,
		ConsumedAtEval:    6000,
		CapAtEval:         10000,
	})
	require.NoError(t, err)

	got, err := cs.GetUsageBudgetState(ctx, store.UsageBudgetStateKey{
		LimitDefinitionID: limitDefID,
		ScopeKind:         store.UsageScopeKindUserProject,
		ScopeID:           "user-1",
		ProjectScopeID:    "proj-1",
		WindowStartUTC:    dayStart,
	})
	require.NoError(t, err)
	assert.Equal(t, created.ID, got.ID)

	// ListUsageBudgetStatesForScope returns both active day & month rows for (user-1, proj-1).
	activeAt := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	listed, err := cs.ListUsageBudgetStatesForScope(ctx, store.UsageScopeKindUserProject, "user-1", "proj-1", activeAt)
	require.NoError(t, err)
	require.Len(t, listed, 2)
	assert.Equal(t, monthRow.ID, listed[0].ID)
	assert.Equal(t, created.ID, listed[1].ID)

	// After dayEnd, only monthRow is active.
	listedAfterDay, err := cs.ListUsageBudgetStatesForScope(ctx, store.UsageScopeKindUserProject, "user-1", "proj-1", dayEnd)
	require.NoError(t, err)
	require.Len(t, listedAfterDay, 1)
	assert.Equal(t, monthRow.ID, listedAfterDay[0].ID)
}

func TestUsageBudgetStore_CompareAndSwapAndConcurrency(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	limitDefID := uuid.NewString()
	dayStart := time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC)
	dayEnd := dayStart.AddDate(0, 0, 1)

	row, err := cs.CreateUsageBudgetState(ctx, &store.UsageBudgetState{
		LimitDefinitionID: limitDefID,
		ScopeKind:         store.UsageScopeKindProject,
		ScopeID:           "proj-cas",
		WindowStartUTC:    dayStart,
		WindowEndUTC:      dayEnd,
		State:             store.UsageBudgetStateOK,
		ConsumedAtEval:    100,
		CapAtEval:         1000,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(1), row.StateVersion)

	// Valid CAS from version 1 -> 2.
	ackAt := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	overrideAt := time.Date(2026, 11, 27, 20, 0, 0, 0, time.UTC)
	next := *row
	next.State = store.UsageBudgetStateWarn
	next.ConsumedAtEval = 600
	next.ThresholdsNotified = 1
	next.AckAnomalousThrough = &ackAt
	next.OverrideUntil = &overrideAt

	swapped, err := cs.CompareAndSwapUsageBudgetState(ctx, 1, &next)
	require.NoError(t, err)
	assert.Equal(t, int64(2), swapped.StateVersion)
	assert.Equal(t, store.UsageBudgetStateWarn, swapped.State)
	assert.Equal(t, int64(600), swapped.ConsumedAtEval)
	assert.Equal(t, int64(1), swapped.ThresholdsNotified)
	require.NotNil(t, swapped.AckAnomalousThrough)
	assert.True(t, ackAt.Equal(*swapped.AckAnomalousThrough))
	require.NotNil(t, swapped.OverrideUntil)
	assert.True(t, overrideAt.Equal(*swapped.OverrideUntil))

	// Stale version 1 must fail with ErrBudgetStateConflict.
	_, err = cs.CompareAndSwapUsageBudgetState(ctx, 1, &next)
	require.ErrorIs(t, err, store.ErrBudgetStateConflict)

	// Concurrent CAS workers all racing to move warn -> exhausted:
	// exactly one worker wins the version-2 -> version-3 transition.
	const workers = 8
	var wins atomic.Int64
	var conflicts atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			cand := *swapped
			cand.State = store.UsageBudgetStateExhausted
			cand.ConsumedAtEval = 1000 + int64(idx)
			_, casErr := cs.CompareAndSwapUsageBudgetState(ctx, 2, &cand)
			if casErr == nil {
				wins.Add(1)
			} else if errors.Is(casErr, store.ErrBudgetStateConflict) {
				conflicts.Add(1)
			} else {
				t.Errorf("unexpected CAS error: %v", casErr)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	assert.Equal(t, int64(1), wins.Load(), "exactly one concurrent CAS on version 2 must win")
	assert.Equal(t, int64(workers-1), conflicts.Load(), "all other concurrent CAS calls must see conflict")

	final, err := cs.GetUsageBudgetState(ctx, store.UsageBudgetStateKey{
		LimitDefinitionID: limitDefID,
		ScopeKind:         store.UsageScopeKindProject,
		ScopeID:           "proj-cas",
		WindowStartUTC:    dayStart,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(3), final.StateVersion)
	assert.Equal(t, store.UsageBudgetStateExhausted, final.State)
}

func TestUsageBudgetStore_SumUsageScopeCounterTokens(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	seedCounter := func(kind, scopeID string, day time.Time, tokens int64) {
		t.Helper()
		_, err := client.UsageScopeCounterDaily.Create().
			SetScopeKind(kind).
			SetScopeID(scopeID).
			SetDayUtc(day).
			SetTokensSummable(tokens).
			SetCalls(1).
			Save(ctx)
		require.NoError(t, err)
	}

	oct31 := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	nov01 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	nov15 := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	nov30 := time.Date(2026, 11, 30, 0, 0, 0, 0, time.UTC)
	dec01 := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)

	seedCounter(store.UsageScopeKindProject, "p1", oct31, 999)
	seedCounter(store.UsageScopeKindProject, "p1", nov01, 100)
	seedCounter(store.UsageScopeKindProject, "p1", nov15, 250)
	seedCounter(store.UsageScopeKindProject, "p1", nov30, 50)
	seedCounter(store.UsageScopeKindProject, "p1", dec01, 777)

	// Half-open [Nov 1, Dec 1) sums Nov 1 + Nov 15 + Nov 30 = 400 (excludes Oct 31 & Dec 1).
	monthSum, err := cs.SumUsageScopeCounterTokens(ctx, store.UsageScopeKindProject, "p1", nov01, dec01)
	require.NoError(t, err)
	assert.Equal(t, int64(400), monthSum)

	// Single day [Nov 15, Nov 16) sums only Nov 15 = 250.
	daySum, err := cs.SumUsageScopeCounterTokens(ctx, store.UsageScopeKindProject, "p1", nov15, nov15.AddDate(0, 0, 1))
	require.NoError(t, err)
	assert.Equal(t, int64(250), daySum)

	// Saturation on near-MaxInt64 multi-day totals.
	seedCounter(store.UsageScopeKindUser, "u-sat", nov01, math.MaxInt64-10)
	seedCounter(store.UsageScopeKindUser, "u-sat", nov15, 500)
	satSum, err := cs.SumUsageScopeCounterTokens(ctx, store.UsageScopeKindUser, "u-sat", nov01, dec01)
	require.NoError(t, err)
	assert.Equal(t, int64(math.MaxInt64), satSum)
}

func TestUsageBudgetStore_SweepUsageBudgetStateBatch(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	limitDefID := uuid.NewString()
	cutoff := time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC)

	seedState := func(scopeID string, winStart, winEnd time.Time) *store.UsageBudgetState {
		t.Helper()
		st, err := cs.CreateUsageBudgetState(ctx, &store.UsageBudgetState{
			LimitDefinitionID: limitDefID,
			ScopeKind:         store.UsageScopeKindProject,
			ScopeID:           scopeID,
			WindowStartUTC:    winStart,
			WindowEndUTC:      winEnd,
			State:             store.UsageBudgetStateOK,
			ConsumedAtEval:    100,
			CapAtEval:         1000,
		})
		require.NoError(t, err)
		return st
	}

	// Two closed windows strictly before cutoff.
	old1 := seedState("p-old-1", cutoff.AddDate(0, 0, -10), cutoff.AddDate(0, 0, -9))
	old2 := seedState("p-old-2", cutoff.AddDate(0, 0, -5), cutoff.AddDate(0, 0, -4))
	// Closed window ending exactly at cutoff -> preserved (< cutoff is strict).
	atCutoff := seedState("p-at-cutoff", cutoff.AddDate(0, 0, -1), cutoff)
	// Active window ending after cutoff -> preserved.
	active := seedState("p-active", cutoff.AddDate(0, 0, 89), cutoff.AddDate(0, 0, 90))

	// Batch size 1 deletes oldest closed row first.
	n, err := cs.SweepUsageBudgetStateBatch(ctx, cutoff, 1)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)

	_, err = cs.GetUsageBudgetState(ctx, store.UsageBudgetStateKey{
		LimitDefinitionID: limitDefID, ScopeKind: store.UsageScopeKindProject,
		ScopeID: old1.ScopeID, WindowStartUTC: old1.WindowStartUTC,
	})
	require.ErrorIs(t, err, store.ErrNotFound)

	// Next batch deletes the second old row, leaving atCutoff and active intact.
	n, err = cs.SweepUsageBudgetStateBatch(ctx, cutoff, 10)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)

	_, err = cs.GetUsageBudgetState(ctx, store.UsageBudgetStateKey{
		LimitDefinitionID: limitDefID, ScopeKind: store.UsageScopeKindProject,
		ScopeID: old2.ScopeID, WindowStartUTC: old2.WindowStartUTC,
	})
	require.ErrorIs(t, err, store.ErrNotFound)

	for _, kept := range []*store.UsageBudgetState{atCutoff, active} {
		got, err := cs.GetUsageBudgetState(ctx, store.UsageBudgetStateKey{
			LimitDefinitionID: limitDefID, ScopeKind: store.UsageScopeKindProject,
			ScopeID: kept.ScopeID, WindowStartUTC: kept.WindowStartUTC,
		})
		require.NoError(t, err)
		assert.Equal(t, kept.ID, got.ID)
	}
}

func TestQuotaStore_EntitlementBindingTokenAction(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	def, err := cs.CreateLimitDefinition(ctx, &store.LimitDefinition{
		Name:         store.LimitLLMTokensPerProjectDay,
		ResourceType: store.LimitResourceTypeLLMTokens,
		Unit:         store.LimitUnitTokens,
		DefaultValue: 0,
		System:       true,
	})
	require.NoError(t, err)

	// 1. Default NULL token_action.
	b1, err := cs.CreateEntitlementBinding(ctx, &store.EntitlementBinding{
		LimitDefinitionID: def.ID,
		SubjectType:       store.EntitlementSubjectSystemDefault,
		SubjectID:         "",
		ScopeType:         store.QuotaScopeProject,
		ScopeID:           "proj-a",
		Value:             100000,
	})
	require.NoError(t, err)
	assert.Nil(t, b1.TokenAction)
	assert.Equal(t, store.TokenActionPauseNew, store.EffectiveTokenAction(b1.TokenAction))

	// 2. Explicit suspend_and_pause on create.
	suspendAct := store.TokenActionSuspendAndPause
	b2, err := cs.CreateEntitlementBinding(ctx, &store.EntitlementBinding{
		LimitDefinitionID: def.ID,
		SubjectType:       store.EntitlementSubjectSystemDefault,
		SubjectID:         "",
		ScopeType:         store.QuotaScopeProject,
		ScopeID:           "proj-b",
		Value:             200000,
		TokenAction:       &suspendAct,
	})
	require.NoError(t, err)
	require.NotNil(t, b2.TokenAction)
	assert.Equal(t, store.TokenActionSuspendAndPause, *b2.TokenAction)

	// 3. Update to notify_only, then clear back to NULL.
	notifyAct := store.TokenActionNotifyOnly
	b2.TokenAction = &notifyAct
	b2.Value = 250000
	updated, err := cs.UpdateEntitlementBinding(ctx, b2)
	require.NoError(t, err)
	require.NotNil(t, updated.TokenAction)
	assert.Equal(t, store.TokenActionNotifyOnly, *updated.TokenAction)
	assert.Equal(t, int64(250000), updated.Value)

	updated.TokenAction = nil
	cleared, err := cs.UpdateEntitlementBinding(ctx, updated)
	require.NoError(t, err)
	assert.Nil(t, cleared.TokenAction)
}

func TestUsageBudgetStore_GetByIDAndListOpenByLimit(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	def1, err := cs.CreateLimitDefinition(ctx, &store.LimitDefinition{
		Name:         store.LimitLLMTokensPerProjectDay,
		ResourceType: store.LimitResourceTypeLLMTokens,
		Unit:         store.LimitUnitTokens,
		DefaultValue: 0,
		System:       true,
	})
	require.NoError(t, err)
	def2, err := cs.CreateLimitDefinition(ctx, &store.LimitDefinition{
		Name:         store.LimitLLMTokensPerUserDay,
		ResourceType: store.LimitResourceTypeLLMTokens,
		Unit:         store.LimitUnitTokens,
		DefaultValue: 0,
		System:       true,
	})
	require.NoError(t, err)

	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	dayStart := time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC)
	dayEnd := dayStart.AddDate(0, 0, 1)
	prevStart := dayStart.AddDate(0, 0, -1)

	open1, err := cs.CreateUsageBudgetState(ctx, &store.UsageBudgetState{
		LimitDefinitionID: def1.ID,
		ScopeKind:         store.UsageScopeKindProject,
		ScopeID:           "proj-1",
		WindowStartUTC:    dayStart,
		WindowEndUTC:      dayEnd,
		State:             store.UsageBudgetStateExhausted,
		ConsumedAtEval:    1000,
		CapAtEval:         1000,
	})
	require.NoError(t, err)

	_, err = cs.CreateUsageBudgetState(ctx, &store.UsageBudgetState{
		LimitDefinitionID: def1.ID,
		ScopeKind:         store.UsageScopeKindProject,
		ScopeID:           "proj-2",
		WindowStartUTC:    dayStart,
		WindowEndUTC:      dayEnd,
		State:             store.UsageBudgetStateWarn,
		ConsumedAtEval:    600,
		CapAtEval:         1000,
	})
	require.NoError(t, err)

	// Closed window for def1 should be excluded.
	_, err = cs.CreateUsageBudgetState(ctx, &store.UsageBudgetState{
		LimitDefinitionID: def1.ID,
		ScopeKind:         store.UsageScopeKindProject,
		ScopeID:           "proj-closed",
		WindowStartUTC:    prevStart,
		WindowEndUTC:      dayStart,
		State:             store.UsageBudgetStateExhausted,
		ConsumedAtEval:    1000,
		CapAtEval:         1000,
	})
	require.NoError(t, err)

	// Open window for def2 should be excluded when listing def1.
	_, err = cs.CreateUsageBudgetState(ctx, &store.UsageBudgetState{
		LimitDefinitionID: def2.ID,
		ScopeKind:         store.UsageScopeKindUser,
		ScopeID:           "user-1",
		WindowStartUTC:    dayStart,
		WindowEndUTC:      dayEnd,
		State:             store.UsageBudgetStateWarn,
		ConsumedAtEval:    500,
		CapAtEval:         1000,
	})
	require.NoError(t, err)

	// GetUsageBudgetStateByID
	got, err := cs.GetUsageBudgetStateByID(ctx, open1.ID)
	require.NoError(t, err)
	assert.Equal(t, open1.ID, got.ID)
	assert.Equal(t, "proj-1", got.ScopeID)

	_, err = cs.GetUsageBudgetStateByID(ctx, "00000000-0000-0000-0000-000000000001")
	require.ErrorIs(t, err, store.ErrNotFound)

	// ListOpenUsageBudgetStatesByLimit
	openRows, err := cs.ListOpenUsageBudgetStatesByLimit(ctx, def1.ID, now, 10)
	require.NoError(t, err)
	require.Len(t, openRows, 2)

	// Respects limit cap.
	limited, err := cs.ListOpenUsageBudgetStatesByLimit(ctx, def1.ID, now, 1)
	require.NoError(t, err)
	require.Len(t, limited, 1)
}

func TestQuotaStore_UpdateEntitlementBindingIfMatch(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	def, err := cs.CreateLimitDefinition(ctx, &store.LimitDefinition{
		Name:         store.LimitLLMTokensPerProjectDay,
		ResourceType: store.LimitResourceTypeLLMTokens,
		Unit:         store.LimitUnitTokens,
		DefaultValue: 0,
		System:       true,
	})
	require.NoError(t, err)

	b, err := cs.CreateEntitlementBinding(ctx, &store.EntitlementBinding{
		LimitDefinitionID: def.ID,
		SubjectType:       store.EntitlementSubjectSystemDefault,
		SubjectID:         "",
		ScopeType:         store.QuotaScopeProject,
		ScopeID:           "proj-cas",
		Value:             100000,
	})
	require.NoError(t, err)
	origUpdated := b.UpdatedAt

	// Matching expectedUpdatedAt succeeds and strictly advances UpdatedAt.
	b.Value = 200000
	updated, err := cs.UpdateEntitlementBindingIfMatch(ctx, b, origUpdated)
	require.NoError(t, err)
	assert.Equal(t, int64(200000), updated.Value)
	assert.True(t, updated.UpdatedAt.After(origUpdated), "updated_at must advance monotonically")

	// Reusing stale origUpdated fails with ErrRevisionConflict.
	updated.Value = 300000
	_, err = cs.UpdateEntitlementBindingIfMatch(ctx, updated, origUpdated)
	require.ErrorIs(t, err, store.ErrRevisionConflict)

	// Using current UpdatedAt succeeds.
	updated2, err := cs.UpdateEntitlementBindingIfMatch(ctx, updated, updated.UpdatedAt)
	require.NoError(t, err)
	assert.Equal(t, int64(300000), updated2.Value)

	// Non-existent binding returns ErrNotFound.
	missing := *updated2
	missing.ID = "00000000-0000-0000-0000-000000000002"
	_, err = cs.UpdateEntitlementBindingIfMatch(ctx, &missing, origUpdated)
	require.ErrorIs(t, err, store.ErrNotFound)
}
