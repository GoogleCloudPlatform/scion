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

func testFireVisTrigger(t *testing.T, cs *CompositeStore, projID, name, kind, principal string, attrVer int, credKind string) *store.Trigger {
	t.Helper()
	tr := &store.Trigger{
		ID:            uuid.NewString(),
		ProjectID:     projID,
		Name:          name,
		SourceKind:    store.TriggerSourceKindWebhook,
		SourceID:      uuid.NewString(),
		MatchTypes:    []string{"webhook.received"},
		ActionType:    store.TriggerActionMessage,
		ActionPayload: `{"agentId":"t","message":"m"}`,
		CreatedBy:     principal,
		InitiatorAttribution: store.InitiatorAttribution{
			InitiatorPrincipalKind:  kind,
			InitiatorPrincipalID:    principal,
			InitiatorCredentialKind: credKind,
			AttributionVersion:      attrVer,
			AuthorizationRevision:   1,
		},
		AuthorityCeiling: store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
	}
	require.NoError(t, cs.CreateTrigger(context.Background(), tr))
	return tr
}

func testFireVisRow(t *testing.T, cs *CompositeStore, projID, trID string, created time.Time) string {
	t.Helper()
	evID := uuid.NewString()
	f := &store.TriggerFire{
		ID:         uuid.NewString(),
		ProjectID:  projID,
		TriggerID:  trID,
		EventID:    evID,
		Subject:    "webhook:x/" + evID,
		ActionType: store.TriggerActionMessage,
		Status:     store.TriggerFireStatusFired,
		LeaseUntil: created.Add(time.Minute),
		StartedAt:  created,
		CreatedAt:  created,
	}
	claimed, _, err := cs.ClaimTriggerFire(context.Background(), f)
	require.NoError(t, err)
	require.True(t, claimed)
	return f.ID
}

func TestListTriggerFires_VisibilityAndPagination(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)
	t.Logf("dialect=%s", cs.client.Driver().Dialect())

	projP := uuid.NewString()
	projQ := uuid.NewString()
	agentA := uuid.NewString()
	agentB := uuid.NewString()
	user := uuid.NewString()

	trA := testFireVisTrigger(t, cs, projP, "a-held", store.DelegationPrincipalAgent, agentA, 1, store.InitiatorCredentialKindAgent)
	trALegacy := testFireVisTrigger(t, cs, projP, "a-legacy", store.DelegationPrincipalAgent, agentA, 0, store.InitiatorCredentialKindLegacyUnknown)
	trB := testFireVisTrigger(t, cs, projP, "b-held", store.DelegationPrincipalAgent, agentB, 1, store.InitiatorCredentialKindAgent)
	trUser := testFireVisTrigger(t, cs, projP, "user-held", store.DelegationPrincipalUser, user, 1, store.InitiatorCredentialKindSession)
	trAQ := testFireVisTrigger(t, cs, projQ, "a-held-q", store.DelegationPrincipalAgent, agentA, 1, store.InitiatorCredentialKindAgent)
	trCollide := testFireVisTrigger(t, cs, projP, "user-collide", store.DelegationPrincipalUser, agentA, 1, store.InitiatorCredentialKindSession)

	base := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	visible := map[string]bool{}
	for _, off := range []time.Duration{0, time.Minute, time.Minute, time.Minute, 3 * time.Minute} {
		visible[testFireVisRow(t, cs, projP, trA.ID, base.Add(off))] = true
	}
	for _, trID := range []string{trALegacy.ID, trB.ID, trUser.ID, trCollide.ID} {
		testFireVisRow(t, cs, projP, trID, base.Add(time.Minute))
		testFireVisRow(t, cs, projP, trID, base.Add(2*time.Minute))
	}
	testFireVisRow(t, cs, projQ, trAQ.ID, base.Add(time.Minute))
	testFireVisRow(t, cs, projP, trAQ.ID, base.Add(time.Minute))

	seen := map[string]bool{}
	var order []time.Time
	cursor := ""
	pages := 0
	for {
		res, err := cs.ListTriggerFires(ctx, store.TriggerFireFilter{ProjectID: projP, VisibleToAgentID: agentA}, store.ListOptions{Limit: 2, Cursor: cursor})
		require.NoError(t, err)
		assert.Equal(t, 5, res.TotalCount)
		for _, f := range res.Items {
			require.True(t, visible[f.ID], "invisible fire leaked: %s (trigger %s)", f.ID, f.TriggerID)
			require.False(t, seen[f.ID], "duplicate across pages: %s", f.ID)
			seen[f.ID] = true
			order = append(order, f.CreatedAt)
		}
		pages++
		require.Less(t, pages, 10)
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}
	assert.Len(t, seen, 5)
	for i := 1; i < len(order); i++ {
		assert.False(t, order[i].After(order[i-1]), "not newest-first")
	}

	resB, err := cs.ListTriggerFires(ctx, store.TriggerFireFilter{ProjectID: projP, VisibleToAgentID: agentB}, store.ListOptions{Limit: 50})
	require.NoError(t, err)
	assert.Equal(t, 2, resB.TotalCount)

	resB2, err := cs.ListTriggerFires(ctx, store.TriggerFireFilter{ProjectID: projP, TriggerID: trA.ID, VisibleToAgentID: agentB}, store.ListOptions{Limit: 50})
	require.NoError(t, err)
	assert.Equal(t, 0, resB2.TotalCount)
	assert.Empty(t, resB2.Items)

	future := encodeCursor(base.Add(1000*time.Hour), "ffffffff-ffff-ffff-ffff-ffffffffffff")
	resT, err := cs.ListTriggerFires(ctx, store.TriggerFireFilter{ProjectID: projP, VisibleToAgentID: agentA}, store.ListOptions{Limit: 50, Cursor: future})
	require.NoError(t, err)
	for _, f := range resT.Items {
		require.True(t, visible[f.ID])
	}
	assert.Len(t, resT.Items, 5)

	_, err = cs.ListTriggerFires(ctx, store.TriggerFireFilter{ProjectID: projP, VisibleToAgentID: agentA}, store.ListOptions{Limit: 50, Cursor: "not-a-cursor"})
	require.Error(t, err)

	since := base.Add(time.Minute)
	resS, err := cs.ListTriggerFires(ctx, store.TriggerFireFilter{ProjectID: projP, VisibleToAgentID: agentA, Since: &since}, store.ListOptions{Limit: 50})
	require.NoError(t, err)
	assert.Equal(t, 4, resS.TotalCount)

	resAll, err := cs.ListTriggerFires(ctx, store.TriggerFireFilter{ProjectID: projP}, store.ListOptions{Limit: 50})
	require.NoError(t, err)
	assert.Equal(t, 14, resAll.TotalCount)
}
