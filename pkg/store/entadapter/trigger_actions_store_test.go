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
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestTriggerActionsStore_CRUDAndFilters(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)
	t.Logf("dialect=%s", cs.client.Driver().Dialect())

	projID := uuid.NewString()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{
		ID:   projID,
		Name: "p9-actions-crud",
		Slug: "p9-actions-crud",
	}))

	agentA := uuid.NewString()
	agentB := uuid.NewString()

	// Create a multi-action trigger with empty legacy scalar columns.
	tr := &store.Trigger{
		ID:         uuid.NewString(),
		ProjectID:  projID,
		Name:       "multi-action-trig",
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   uuid.NewString(),
		MatchTypes: []string{"webhook.received"},
		Actions: []store.TriggerAction{
			{Type: store.TriggerActionDispatchAgent, Payload: `{"template":"dev","task":"do work"}`},
			{Type: store.TriggerActionMessage, Payload: `{"agentId":"` + agentA + `","message":"dispatched"}`},
		},
		Status: store.TriggerStatusActive,
	}
	require.NoError(t, cs.CreateTrigger(ctx, tr))

	got, err := cs.GetTrigger(ctx, tr.ID)
	require.NoError(t, err)
	assert.Empty(t, got.ActionType, "legacy action_type must stay empty when written via actions[]")
	assert.Empty(t, got.ActionPayload, "legacy action_payload must stay empty when written via actions[]")
	assert.False(t, got.ActionsCorrupt)
	require.Len(t, got.Actions, 2)
	assert.Equal(t, store.TriggerActionDispatchAgent, got.Actions[0].Type)
	assert.Equal(t, store.TriggerActionMessage, got.Actions[1].Type)

	// ActionTypes and TargetAgentID filters must match triggers stored in actions[].
	byType, err := cs.ListTriggers(ctx, store.TriggerFilter{
		ProjectID:   projID,
		ActionTypes: []string{store.TriggerActionMessage},
	}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, byType.Items, 1)
	assert.Equal(t, tr.ID, byType.Items[0].ID)

	byTarget, err := cs.ListTriggers(ctx, store.TriggerFilter{
		ProjectID:     projID,
		TargetAgentID: agentA,
	}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, byTarget.Items, 1)
	assert.Equal(t, tr.ID, byTarget.Items[0].ID)

	byOtherTarget, err := cs.ListTriggers(ctx, store.TriggerFilter{
		ProjectID:     projID,
		TargetAgentID: agentB,
	}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, byOtherTarget.Items)

	// Update actions[] with revision bump.
	tr.Actions = []store.TriggerAction{
		{Type: store.TriggerActionMessage, Payload: `{"agentId":"` + agentB + `","message":"updated"}`},
	}
	newAttr := store.InitiatorAttribution{
		InitiatorPrincipalKind:  store.DelegationPrincipalUser,
		InitiatorPrincipalID:    uuid.NewString(),
		InitiatorCredentialKind: store.InitiatorCredentialKindSession,
		AttributionVersion:      1,
		AuthorizationRevision:   2,
	}
	require.NoError(t, cs.UpdateTrigger(ctx, tr, store.TriggerFieldMask{Actions: true}, 0, false, &newAttr))
	updated, err := cs.GetTrigger(ctx, tr.ID)
	require.NoError(t, err)
	require.Len(t, updated.Actions, 1)
	assert.Equal(t, 2, updated.AuthorizationRevision)
	assert.Contains(t, updated.Actions[0].Payload, agentB)
}

func TestTriggerActionsStore_FireUniquenessClaimsAndDistinctEventRateCount(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)
	t.Logf("dialect=%s", cs.client.Driver().Dialect())

	projID := uuid.NewString()
	trigID := uuid.NewString()
	ev1ID := uuid.NewString()
	ev2ID := uuid.NewString()
	now := time.Now().UTC().Truncate(time.Microsecond)

	// 1. Fire uniqueness is (event_id, trigger_id, action_index):
	//    action_index=0 and action_index=1 for the same (event_id, trigger_id) both succeed,
	//    while a duplicate claim for action_index=0 returns claimed=false and the existing row.
	f0 := &store.TriggerFire{
		ID:          uuid.NewString(),
		ProjectID:   projID,
		TriggerID:   trigID,
		EventID:     ev1ID,
		Subject:     "pr-99",
		ActionType:  store.TriggerActionDispatchAgent,
		ActionIndex: 0,
		Status:      store.TriggerFireStatusFiring,
		LeaseUntil:  now.Add(10 * time.Minute),
		StartedAt:   now,
		CreatedAt:   now,
	}
	claimed, existing, err := cs.ClaimTriggerFire(ctx, f0)
	require.NoError(t, err)
	require.True(t, claimed)
	assert.Nil(t, existing)

	f1 := &store.TriggerFire{
		ID:          uuid.NewString(),
		ProjectID:   projID,
		TriggerID:   trigID,
		EventID:     ev1ID,
		Subject:     "pr-99",
		ActionType:  store.TriggerActionMessage,
		ActionIndex: 1,
		Status:      store.TriggerFireStatusFiring,
		LeaseUntil:  now.Add(10 * time.Minute),
		StartedAt:   now.Add(time.Millisecond),
		CreatedAt:   now.Add(time.Millisecond),
	}
	claimed, existing, err = cs.ClaimTriggerFire(ctx, f1)
	require.NoError(t, err)
	require.True(t, claimed)
	assert.Nil(t, existing)

	dup0 := *f0
	dup0.ID = uuid.NewString()
	claimed, existing, err = cs.ClaimTriggerFire(ctx, &dup0)
	require.NoError(t, err)
	assert.False(t, claimed)
	require.NotNil(t, existing)
	assert.Equal(t, f0.ID, existing.ID)
	assert.Equal(t, 0, existing.ActionIndex)

	// 2. CountRecentTriggerFires counts DISTINCT matched events, not actions,
	//    and excludes the caller's own event_id so sibling actions never rate-limit each other.
	finished, err := cs.FinishTriggerFire(ctx, f0.ID, store.TriggerFireStatusFired, "agent-1", "", "", now.Add(2*time.Millisecond))
	require.NoError(t, err)
	require.True(t, finished)

	// While f1 (same event ev1) is checking rate limit, ev1 must NOT count against itself.
	recentForF1, err := cs.CountRecentTriggerFires(ctx, trigID, now.Add(-time.Minute), f1.ID)
	require.NoError(t, err)
	assert.Equal(t, 0, recentForF1, "sibling action of the same event must not count against its own event's rate limit")

	finished, err = cs.FinishTriggerFire(ctx, f1.ID, store.TriggerFireStatusFired, "msg-1", "", "", now.Add(3*time.Millisecond))
	require.NoError(t, err)
	require.True(t, finished)

	// For a new event ev2, ev1 has TWO fired rows (action 0 and action 1) but must count as ONE matched event.
	f2 := &store.TriggerFire{
		ID:          uuid.NewString(),
		ProjectID:   projID,
		TriggerID:   trigID,
		EventID:     ev2ID,
		Subject:     "pr-100",
		ActionType:  store.TriggerActionDispatchAgent,
		ActionIndex: 0,
		Status:      store.TriggerFireStatusFiring,
		LeaseUntil:  now.Add(10 * time.Minute),
		StartedAt:   now.Add(10 * time.Millisecond),
		CreatedAt:   now.Add(10 * time.Millisecond),
	}
	claimed, _, err = cs.ClaimTriggerFire(ctx, f2)
	require.NoError(t, err)
	require.True(t, claimed)

	recentForF2, err := cs.CountRecentTriggerFires(ctx, trigID, now.Add(-time.Minute), f2.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, recentForF2, "two actions from one event must count as one matched event")

	// 3. Subject claims are keyed by (trigger_id, action_index, subject):
	//    action_index=0 and action_index=1 hold independent claims on the same subject.
	c0, ex0, err := cs.ClaimTriggerSubject(ctx, trigID, 0, "pr-99", ev1ID, f0.ID, now, now.Add(-5*time.Minute))
	require.NoError(t, err)
	require.NotNil(t, c0)
	assert.Nil(t, ex0)
	assert.Equal(t, 0, c0.ActionIndex)

	c1, ex1, err := cs.ClaimTriggerSubject(ctx, trigID, 1, "pr-99", ev1ID, f1.ID, now, now.Add(-5*time.Minute))
	require.NoError(t, err)
	require.NotNil(t, c1)
	assert.Nil(t, ex1)
	assert.Equal(t, 1, c1.ActionIndex)
	assert.NotEqual(t, c0.ID, c1.ID)

	got0, err := cs.GetActiveTriggerSubjectClaim(ctx, trigID, 0, "pr-99")
	require.NoError(t, err)
	assert.Equal(t, c0.ID, got0.ID)

	got1, err := cs.GetActiveTriggerSubjectClaim(ctx, trigID, 1, "pr-99")
	require.NoError(t, err)
	assert.Equal(t, c1.ID, got1.ID)

	// Re-claiming action_index=0 conflicts with c0, not c1.
	c0Dup, ex0Dup, err := cs.ClaimTriggerSubject(ctx, trigID, 0, "pr-99", ev2ID, f2.ID, now.Add(time.Second), now.Add(-4*time.Minute))
	require.NoError(t, err)
	assert.Nil(t, c0Dup)
	require.NotNil(t, ex0Dup)
	assert.Equal(t, c0.ID, ex0Dup.ID)
}

func TestTriggerActionsStore_RateLimitDoesNotSplitMatchedEventUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)
	t.Logf("dialect=%s", cs.client.Driver().Dialect())

	projID := uuid.NewString()
	trigID := uuid.NewString()
	e1 := uuid.NewString()
	e2 := uuid.NewString()
	now := time.Now().UTC().Truncate(time.Microsecond)
	since := now.Add(-time.Minute)

	mk := func(ev string, idx int, at time.Time) *store.TriggerFire {
		return &store.TriggerFire{
			ID:          uuid.NewString(),
			ProjectID:   projID,
			TriggerID:   trigID,
			EventID:     ev,
			Subject:     "pr-1",
			ActionType:  store.TriggerActionMessage,
			ActionIndex: idx,
			Status:      store.TriggerFireStatusFiring,
			LeaseUntil:  at.Add(10 * time.Minute),
			StartedAt:   at,
			CreatedAt:   at,
		}
	}

	// E1 action 0 is claimed and admitted (sees 0 prior events).
	e1a0 := mk(e1, 0, now)
	ok, _, err := cs.ClaimTriggerFire(ctx, e1a0)
	require.NoError(t, err)
	require.True(t, ok)
	n0, err := cs.CountRecentTriggerFires(ctx, trigID, since, e1a0.ID)
	require.NoError(t, err)
	require.Equal(t, 0, n0, "E1 action 0 must see 0 prior events")
	_, err = cs.FinishTriggerFire(ctx, e1a0.ID, store.TriggerFireStatusFired, "", "", "", now.Add(time.Millisecond))
	require.NoError(t, err)

	// Concurrent drainer inserts E2 action 0 (in-flight firing) before E1 action 1 is claimed.
	e2a0 := mk(e2, 0, now.Add(2*time.Millisecond))
	ok, _, err = cs.ClaimTriggerFire(ctx, e2a0)
	require.NoError(t, err)
	require.True(t, ok)
	nE2, err := cs.CountRecentTriggerFires(ctx, trigID, since, e2a0.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, nE2, "E2 action 0 must count E1 and be rate-limited when max=1")

	// E1 action 1 is claimed after E2's in-flight row, but must still use E1's
	// earliest non-skipped ordering key so E1 is never split across siblings.
	e1a1 := mk(e1, 1, now.Add(3*time.Millisecond))
	ok, _, err = cs.ClaimTriggerFire(ctx, e1a1)
	require.NoError(t, err)
	require.True(t, ok)
	n1, err := cs.CountRecentTriggerFires(ctx, trigID, since, e1a1.ID)
	require.NoError(t, err)
	assert.Equal(t, 0, n1, "E1 action 1 must get the same rate decision as E1 action 0")
}

func TestTriggerActionsStore_SiblingExecOverlapKeysAdmitted(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)
	t.Logf("dialect=%s", cs.client.Driver().Dialect())

	projID := uuid.NewString()
	agentA := uuid.NewString()
	agentB := uuid.NewString()
	now := time.Now().UTC().Truncate(time.Microsecond)

	tr := &store.Trigger{
		ID:         uuid.NewString(),
		ProjectID:  projID,
		Name:       "two-exec-overlap",
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   uuid.NewString(),
		MatchTypes: []string{"webhook.received"},
		Actions: []store.TriggerAction{
			{Type: store.TriggerActionExec, Payload: `{"agentId":"` + agentA + `","command":["true"]}`},
			{Type: store.TriggerActionExec, Payload: `{"agentId":"` + agentB + `","command":["true"]}`},
		},
		Status: store.TriggerStatusActive,
	}
	require.NoError(t, cs.CreateTrigger(ctx, tr))
	ev := &store.TriggerEvent{
		ID:         uuid.NewString(),
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   tr.SourceID,
		Type:       "webhook.received",
		Subject:    "s",
		DedupKey:   uuid.NewString(),
		OccurredAt: now,
		ReceivedAt: now,
		Status:     store.TriggerEventStatusPending,
	}
	require.NoError(t, cs.CreateTriggerEvent(ctx, ev))

	admit := func(evID string, idx int, target string) error {
		tf := &store.TriggerFire{
			ID:          uuid.NewString(),
			ProjectID:   projID,
			TriggerID:   tr.ID,
			EventID:     evID,
			Subject:     "s",
			ActionType:  store.TriggerActionExec,
			ActionIndex: idx,
			Status:      store.TriggerFireStatusFiring,
			LeaseUntil:  now.Add(10 * time.Minute),
			StartedAt:   now,
			CreatedAt:   now,
		}
		ok, _, err := cs.ClaimTriggerFire(ctx, tf)
		require.NoError(t, err)
		require.True(t, ok)
		return cs.CreateScheduledExecRun(ctx, &store.ScheduledExecRun{
			ProjectID:      projID,
			FireID:         tf.ID,
			OverlapKey:     "trigger:" + tr.ID + ":" + string(rune('0'+idx)),
			TargetAgentID:  target,
			CommandSHA256:  "abc",
			Argc:           1,
			TimeoutSeconds: 60,
			StartedAt:      now,
			NotAfter:       now.Add(time.Minute),
			LeaseUntil:     now.Add(5 * time.Minute),
			Outcome:        store.ScheduledExecOutcomeStarting,
		})
	}

	require.NoError(t, admit(ev.ID, 0, agentA), "exec action 0 must be admitted")
	require.NoError(t, admit(ev.ID, 1, agentB), "sibling exec action 1 must be admitted while action 0 is running")

	// A second event firing action 0 while action 0 is still running MUST still be refused with overlap.
	ev2 := &store.TriggerEvent{
		ID:         uuid.NewString(),
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   tr.SourceID,
		Type:       "webhook.received",
		Subject:    "s2",
		DedupKey:   uuid.NewString(),
		OccurredAt: now.Add(time.Second),
		ReceivedAt: now.Add(time.Second),
		Status:     store.TriggerEventStatusPending,
	}
	require.NoError(t, cs.CreateTriggerEvent(ctx, ev2))
	err := admit(ev2.ID, 0, agentA)
	require.ErrorIs(t, err, store.ErrScheduledExecOverlap, "repeated fire of the same exec action index must still refuse overlap")
}

// TestTriggerActionsStore_RateDecisionPersistsAcrossPasses lifts review 2's
// R2-bis store probe (both skip-then-window-slide and admit-then-later-event)
// on SQLite and real Postgres 17.
func TestTriggerActionsStore_RateDecisionPersistsAcrossPasses(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)
	t.Logf("dialect=%s", cs.client.Driver().Dialect())

	projID := uuid.NewString()
	trigID := uuid.NewString()
	evF := uuid.NewString()
	evE := uuid.NewString()
	start := time.Now().UTC().Truncate(time.Microsecond)

	claim := func(ev string, idx int, at time.Time) *store.TriggerFire {
		tf := &store.TriggerFire{
			ID:          uuid.NewString(),
			ProjectID:   projID,
			TriggerID:   trigID,
			EventID:     ev,
			Subject:     "pr-1",
			ActionType:  store.TriggerActionMessage,
			ActionIndex: idx,
			Status:      store.TriggerFireStatusFiring,
			LeaseUntil:  at.Add(10 * time.Minute),
			StartedAt:   at,
			CreatedAt:   at,
		}
		ok, _, err := cs.ClaimTriggerFire(ctx, tf)
		require.NoError(t, err)
		require.True(t, ok)
		return tf
	}

	// Case 1: Skip-then-admit across window slide (review 2 probe).
	f0 := claim(evF, 0, start.Add(-59*time.Second))
	_, err := cs.FinishTriggerFire(ctx, f0.ID, store.TriggerFireStatusFired, "", "", "", start.Add(-59*time.Second))
	require.NoError(t, err)

	ea0 := claim(evE, 0, start)
	n0, err := cs.CountRecentTriggerFires(ctx, trigID, time.Now().UTC().Add(-time.Minute), ea0.ID)
	require.NoError(t, err)
	require.Equal(t, 1, n0, "pass 1: E a0 counts F -> rate limited at max=1")
	_, err = cs.FinishTriggerFire(ctx, ea0.ID, store.TriggerFireStatusSkipped, "", "", "rate limit", start)
	require.NoError(t, err)

	ea1 := claim(evE, 1, start.Add(time.Millisecond))
	// Pass 2: window slides past F (using since after F.Created).
	n1, err := cs.CountRecentTriggerFires(ctx, trigID, start.Add(-30*time.Second), ea1.ID)
	require.NoError(t, err)
	require.GreaterOrEqual(t, n1, 1, "pass 2: E a1 must reuse E a0's durable rate-limit skip decision")

	// Case 2: Admit-then-skip across deferral > 60s.
	trigID2 := uuid.NewString()
	evA := uuid.NewString()
	evG := uuid.NewString()
	claimOn := func(tid, ev string, idx int, at time.Time) *store.TriggerFire {
		tf := &store.TriggerFire{
			ID:          uuid.NewString(),
			ProjectID:   projID,
			TriggerID:   tid,
			EventID:     ev,
			Subject:     "pr-2",
			ActionType:  store.TriggerActionMessage,
			ActionIndex: idx,
			Status:      store.TriggerFireStatusFiring,
			LeaseUntil:  at.Add(10 * time.Minute),
			StartedAt:   at,
			CreatedAt:   at,
		}
		ok, _, cErr := cs.ClaimTriggerFire(ctx, tf)
		require.NoError(t, cErr)
		require.True(t, ok)
		return tf
	}
	a0 := claimOn(trigID2, evA, 0, start.Add(-70*time.Second))
	_, err = cs.FinishTriggerFire(ctx, a0.ID, store.TriggerFireStatusFired, "", "", "", start.Add(-70*time.Second))
	require.NoError(t, err)
	// Event G arrives >60s later and is admitted.
	g0 := claimOn(trigID2, evG, 0, start.Add(-5*time.Second))
	_, err = cs.FinishTriggerFire(ctx, g0.ID, store.TriggerFireStatusFired, "", "", "", start.Add(-5*time.Second))
	require.NoError(t, err)
	// Event A's deferred action 1 is re-checked in pass 2: must reuse A a0's durable admit decision (0).
	a1 := claimOn(trigID2, evA, 1, start)
	nA1, err := cs.CountRecentTriggerFires(ctx, trigID2, start.Add(-time.Minute), a1.ID)
	require.NoError(t, err)
	assert.Equal(t, 0, nA1, "pass 2: deferred E a1 must reuse E a0's durable admit decision")
}

// TestTriggerActionsStore_CountTriggerActionFiresSince_MixedExecTrigger verifies O-c
// on SQLite and real Postgres 17: only exec rows from other events are counted.
func TestTriggerActionsStore_CountTriggerActionFiresSince_MixedExecTrigger(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)
	t.Logf("dialect=%s", cs.client.Driver().Dialect())

	projID := uuid.NewString()
	trigID := uuid.NewString()
	now := time.Now().UTC().Truncate(time.Microsecond)

	// 5 events x (7 message + 1 exec) = 35 message rows + 5 exec rows.
	var lastEv string
	for e := 0; e < 5; e++ {
		evID := uuid.NewString()
		lastEv = evID
		for idx := 0; idx < 8; idx++ {
			actType := store.TriggerActionMessage
			if idx == 7 {
				actType = store.TriggerActionExec
			}
			tf := &store.TriggerFire{
				ID:          uuid.NewString(),
				ProjectID:   projID,
				TriggerID:   trigID,
				EventID:     evID,
				Subject:     "s",
				ActionType:  actType,
				ActionIndex: idx,
				Status:      store.TriggerFireStatusFired,
				LeaseUntil:  now.Add(10 * time.Minute),
				StartedAt:   now,
				CreatedAt:   now,
			}
			ok, _, err := cs.ClaimTriggerFire(ctx, tf)
			require.NoError(t, err)
			require.True(t, ok)
		}
	}

	// Excluding lastEv, there are 4 exec rows (not 35+4).
	n, err := cs.CountTriggerActionFiresSince(ctx, trigID, store.TriggerActionExec, lastEv, now.Add(-time.Minute), store.TriggerFireStatusSkipped)
	require.NoError(t, err)
	assert.Equal(t, 4, n, "exec rate cap must count only exec rows from other events")
}
