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
