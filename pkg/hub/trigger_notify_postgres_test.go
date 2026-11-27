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

//go:build integration

package hub

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

// R1 real-Postgres race test: two goroutines on two Server instances sharing
// one Postgres store race to notify for different failed fires of the same
// trigger within the same hour bucket. Exactly 1 user notice and 1 agent
// notification must be emitted.
func TestTriggerNotify_TwoServersRace_Postgres(t *testing.T) {
	if !enttest.Active() {
		t.Skip("requires -tags integration and SCION_TEST_POSTGRES_URL")
	}
	client := enttest.NewClient(t)
	t.Logf("dialect=%s", client.Driver().Dialect())
	cs := entadapter.NewCompositeStore(client)

	ctx := context.Background()
	srvA, _ := testServerWithStore(t, cs)
	srvB, _ := testServerOnMigratedStore(t, cs)
	enableTriggersExperimentForTest(t, srvA, true)
	enableTriggersExperimentForTest(t, srvB, true)

	fixedNow := time.Date(2026, 11, 27, 15, 10, 0, 0, time.UTC)
	srvA.setFireNotifyClockForTest(func() time.Time { return fixedNow })
	srvB.setFireNotifyClockForTest(func() time.Time { return fixedNow })

	disp := &notifyTestDispatcher{}
	srvA.SetDispatcher(disp)
	srvB.SetDispatcher(disp)

	owner := &store.User{
		ID:          api.NewUUID(),
		Email:       "notify-pg-owner@example.com",
		DisplayName: "PG Owner",
		Role:        store.UserRoleAdmin,
		Status:      store.UserStatusActive,
	}
	require.NoError(t, cs.CreateUser(ctx, owner))

	projID := api.NewUUID()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{
		ID:   projID,
		Name: "p7a-pg-race",
		Slug: "p7a-pg-race",
	}))
	agentID := api.NewUUID()
	require.NoError(t, cs.CreateAgent(ctx, &store.Agent{
		ID:        agentID,
		Slug:      "pg-notify-agent",
		Name:      "pg-notify-agent",
		ProjectID: projID,
		Phase:     "running",
		OwnerID:   owner.ID,
	}))

	tr := &store.Trigger{
		ID:         api.NewUUID(),
		ProjectID:  projID,
		Name:       "pg-race-trig",
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   api.NewUUID(),
		MatchTypes: []string{"webhook.received"},
		Actions:    []store.TriggerAction{{Type: store.TriggerActionMessage, Payload: `{"agentId":"` + agentID + `","message":"hi"}`}},
		Notify: &store.TriggerNotifyPolicy{
			UserNotice: boolPtr(true),
			AgentID:    agentID,
			AgentName:  "pg-notify-agent",
		},
		Status:    store.TriggerStatusActive,
		CreatedBy: owner.ID,
		InitiatorAttribution: store.InitiatorAttribution{
			InitiatorPrincipalKind:  store.DelegationPrincipalUser,
			InitiatorPrincipalID:    owner.ID,
			InitiatorCredentialKind: store.InitiatorCredentialKindSession,
			AttributionVersion:      1,
			AuthorizationRevision:   1,
		},
		AuthorityCeiling: store.EffectCeiling{
			Kind:    store.EffectCeilingPrincipal,
			Version: 1,
		},
	}
	require.NoError(t, cs.CreateTrigger(ctx, tr))

	out := hubfire.FireOutcome{Status: hubfire.FireStatusFailed, Error: "message failed"}
	fireA := &store.TriggerFire{
		ID: api.NewUUID(), ProjectID: projID, TriggerID: tr.ID, EventID: api.NewUUID(),
		ActionType: store.TriggerActionMessage, ActionIndex: 0,
	}
	fireB := &store.TriggerFire{
		ID: api.NewUUID(), ProjectID: projID, TriggerID: tr.ID, EventID: api.NewUUID(),
		ActionType: store.TriggerActionMessage, ActionIndex: 0,
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		srvA.notifyTriggerFire(ctx, tr, fireA, out)
	}()
	go func() {
		defer wg.Done()
		<-start
		srvB.notifyTriggerFire(ctx, tr, fireB, out)
	}()
	close(start)
	wg.Wait()

	notifs, err := cs.GetNotifications(ctx, store.SubscriberTypeUser, owner.ID, false)
	require.NoError(t, err)
	t.Logf("dialect=%s user_notices=%d agent_messages=%d", client.Driver().Dialect(), len(notifs), len(disp.getCalls()))
	assert.Len(t, notifs, 1, "two racing replicas on Postgres must emit exactly 1 user notice")
	assert.Len(t, disp.getCalls(), 1, "two racing replicas on Postgres must emit exactly 1 agent message")
}
