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

func TestTriggerNotifyStore_CRUDRoundTrip(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)
	t.Logf("dialect=%s", cs.client.Driver().Dialect())

	projID := uuid.NewString()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{
		ID:   projID,
		Name: "p7a-notify-crud",
		Slug: "p7a-notify-crud",
	}))

	agentID := uuid.NewString()
	userNoticeFalse := false
	tr := &store.Trigger{
		ID:         uuid.NewString(),
		ProjectID:  projID,
		Name:       "notify-crud-trig",
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   uuid.NewString(),
		MatchTypes: []string{"webhook.received"},
		Actions: []store.TriggerAction{
			{Type: store.TriggerActionMessage, Payload: `{"agentId":"` + agentID + `","message":"hi"}`},
		},
		Notify: &store.TriggerNotifyPolicy{
			OnOutcomes: []string{store.TriggerFireStatusFailed, store.TriggerFireStatusExpired, store.TriggerFireStatusSkipped},
			UserNotice: &userNoticeFalse,
			AgentID:    agentID,
			AgentName:  "ops-agent",
		},
		Status: store.TriggerStatusActive,
		InitiatorAttribution: store.InitiatorAttribution{
			InitiatorPrincipalKind: "user",
			InitiatorPrincipalID:   uuid.NewString(),
			AuthorizationRevision:  1,
		},
	}
	require.NoError(t, cs.CreateTrigger(ctx, tr))

	got, err := cs.GetTrigger(ctx, tr.ID)
	require.NoError(t, err)
	require.NotNil(t, got.Notify)
	assert.Equal(t, []string{store.TriggerFireStatusFailed, store.TriggerFireStatusExpired, store.TriggerFireStatusSkipped}, got.Notify.OnOutcomes)
	require.NotNil(t, got.Notify.UserNotice)
	assert.False(t, *got.Notify.UserNotice)
	assert.Equal(t, agentID, got.Notify.AgentID)
	assert.Equal(t, "ops-agent", got.Notify.AgentName)

	// Update notify policy and verify revision + persistence.
	userNoticeTrue := true
	got.Notify = &store.TriggerNotifyPolicy{
		OnOutcomes: []string{store.TriggerFireStatusFailed, store.TriggerFireStatusRefused, store.TriggerFireStatusExpired},
		UserNotice: &userNoticeTrue,
	}
	nextAttr := got.InitiatorAttribution
	nextAttr.AuthorizationRevision = 2
	require.NoError(t, cs.UpdateTrigger(ctx, got, store.TriggerFieldMask{Notify: true}, 1, true, &nextAttr))

	got2, err := cs.GetTrigger(ctx, tr.ID)
	require.NoError(t, err)
	require.NotNil(t, got2.Notify)
	require.NotNil(t, got2.Notify.UserNotice)
	assert.True(t, *got2.Notify.UserNotice)
	assert.Empty(t, got2.Notify.AgentID)
	assert.Equal(t, 2, got2.AuthorizationRevision)

	// Clearing Notify stores empty column and reads back nil.
	got2.Notify = nil
	nextAttr.AuthorizationRevision = 3
	require.NoError(t, cs.UpdateTrigger(ctx, got2, store.TriggerFieldMask{Notify: true}, 2, true, &nextAttr))

	got3, err := cs.GetTrigger(ctx, tr.ID)
	require.NoError(t, err)
	assert.Nil(t, got3.Notify)
}

func TestTriggerNotifyStore_FireRowIdempotencyAndRateExclusion(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)
	t.Logf("dialect=%s", cs.client.Driver().Dialect())

	projID := uuid.NewString()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{
		ID:   projID,
		Name: "p7a-notify-idem",
		Slug: "p7a-notify-idem",
	}))

	trID := uuid.NewString()
	require.NoError(t, cs.CreateTrigger(ctx, &store.Trigger{
		ID:         trID,
		ProjectID:  projID,
		Name:       "notify-idem-trig",
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   uuid.NewString(),
		MatchTypes: []string{"webhook.received"},
		Actions: []store.TriggerAction{
			{Type: store.TriggerActionMessage, Payload: `{"agentId":"` + uuid.NewString() + `","message":"hi"}`},
		},
		Status: store.TriggerStatusActive,
	}))

	now := time.Now().UTC().Truncate(time.Microsecond)
	realEventID := uuid.NewString()
	origFireID := uuid.NewString()

	// Real action fire row at action_index = 0.
	realFire := &store.TriggerFire{
		ID:          origFireID,
		ProjectID:   projID,
		TriggerID:   trID,
		EventID:     realEventID,
		Subject:     "subj-1",
		ActionType:  store.TriggerActionMessage,
		ActionIndex: 0,
		Status:      store.TriggerFireStatusFailed,
		Error:       "message failed",
		LeaseUntil:  now.Add(10 * time.Minute),
		StartedAt:   now,
		CreatedAt:   now,
	}
	claimed, _, err := cs.ClaimTriggerFire(ctx, realFire)
	require.NoError(t, err)
	require.True(t, claimed)

	// Notify fire row uses action_index = TriggerFireActionIndexNotify (-1) and
	// a deterministic UUIDv5 derived from (fire_id, "notify").
	ns := uuid.MustParse("c3b2d9a4-7e1f-4c8b-9a5d-2e6f8a1b4c90")
	derivedEventID := uuid.NewSHA1(ns, []byte(origFireID+"\x00notify")).String()

	notifyFire1 := &store.TriggerFire{
		ID:          uuid.NewString(),
		ProjectID:   projID,
		TriggerID:   trID,
		EventID:     derivedEventID,
		Subject:     "notify:" + origFireID,
		ActionType:  store.TriggerActionNotify,
		ActionIndex: store.TriggerFireActionIndexNotify,
		Status:      store.TriggerFireStatusFiring,
		LeaseUntil:  now.Add(10 * time.Minute),
		StartedAt:   now,
		CreatedAt:   now,
	}
	claimed1, existing1, err := cs.ClaimTriggerFire(ctx, notifyFire1)
	require.NoError(t, err)
	assert.True(t, claimed1)
	assert.Nil(t, existing1)

	// Second claim for the same (derivedEventID, triggerID, -1) must be idempotent.
	notifyFire2 := &store.TriggerFire{
		ID:          uuid.NewString(),
		ProjectID:   projID,
		TriggerID:   trID,
		EventID:     derivedEventID,
		Subject:     "notify:" + origFireID,
		ActionType:  store.TriggerActionNotify,
		ActionIndex: store.TriggerFireActionIndexNotify,
		Status:      store.TriggerFireStatusFiring,
		LeaseUntil:  now.Add(10 * time.Minute),
		StartedAt:   now,
		CreatedAt:   now,
	}
	claimed2, existing2, err := cs.ClaimTriggerFire(ctx, notifyFire2)
	require.NoError(t, err)
	assert.False(t, claimed2, "duplicate notify fire claim must be rejected by unique (event_id, trigger_id, action_index)")
	require.NotNil(t, existing2)
	assert.Equal(t, notifyFire1.ID, existing2.ID)
	assert.Equal(t, store.TriggerActionNotify, existing2.ActionType)
	assert.Equal(t, store.TriggerFireActionIndexNotify, existing2.ActionIndex)

	// Even if a notify row ever shared realEventID, action_index = -1 never collides with action_index = 0.
	sameEvtNotify := &store.TriggerFire{
		ID:          uuid.NewString(),
		ProjectID:   projID,
		TriggerID:   trID,
		EventID:     realEventID,
		Subject:     "notify-same-evt",
		ActionType:  store.TriggerActionNotify,
		ActionIndex: store.TriggerFireActionIndexNotify,
		Status:      store.TriggerFireStatusFired,
		LeaseUntil:  now.Add(10 * time.Minute),
		StartedAt:   now,
		CreatedAt:   now,
	}
	claimedSameEvt, _, err := cs.ClaimTriggerFire(ctx, sameEvtNotify)
	require.NoError(t, err)
	assert.True(t, claimedSameEvt, "action_index=-1 must never collide with real action_index=0")

	// CountRecentTriggerFires must exclude action_type = "notify" rows so notify
	// fires never consume the trigger's max_fires_per_minute budget.
	recent, err := cs.CountRecentTriggerFires(ctx, trID, now.Add(-time.Minute), "")
	require.NoError(t, err)
	assert.Equal(t, 1, recent, "only the 1 real matched event should count, not notify rows")
}
