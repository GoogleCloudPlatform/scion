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

// Runtime observation store tests. Named TestRecoveryObs_* so the Postgres
// CI job runs them against Postgres as well as SQLite.
package entadapter

import (
	"context"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecoveryObs_RecordKeepsFirstAbsentAndIgnoresIncompleteTargets(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := newClaimAgent(t, ctx, s, projectID, "obs-a")
	b := newClaimAgent(t, ctx, s, projectID, "obs-b")
	const broker = "broker-1"

	t1, err := s.RecordRecoveryObservations(ctx, broker, []string{"A"}, []store.RecoveryObservation{
		{AgentID: a.ID, Target: "A", State: store.ObservedAbsent},
		{AgentID: b.ID, Target: "B", State: store.ObservedAbsent}, // B is not complete: ignored
	})
	require.NoError(t, err)
	require.False(t, t1.IsZero())

	got, err := s.GetRecoveryObservations(ctx, []string{a.ID, b.ID})
	require.NoError(t, err)
	require.Contains(t, got, a.ID)
	assert.NotContains(t, got, b.ID, "an observation of an incomplete target is not recorded")
	require.NotNil(t, got[a.ID].FirstAbsentAt)
	firstAbsent := *got[a.ID].FirstAbsentAt
	assert.True(t, firstAbsent.Equal(t1))
	assert.Equal(t, broker, got[a.ID].BrokerID)

	time.Sleep(2 * time.Millisecond)
	t2, err := s.RecordRecoveryObservations(ctx, broker, []string{"A"}, []store.RecoveryObservation{
		{AgentID: a.ID, Target: "A", State: store.ObservedAbsent, InFlight: true},
	})
	require.NoError(t, err)
	got, err = s.GetRecoveryObservations(ctx, []string{a.ID})
	require.NoError(t, err)
	assert.True(t, got[a.ID].ObservedAt.Equal(t2))
	assert.True(t, got[a.ID].FirstAbsentAt.Equal(firstAbsent), "consecutive absent observations keep first_absent_at")
	assert.True(t, got[a.ID].InFlight)

	_, err = s.RecordRecoveryObservations(ctx, broker, []string{"A"}, []store.RecoveryObservation{
		{AgentID: a.ID, Target: "A", State: store.ObservedPresentRunning},
	})
	require.NoError(t, err)
	got, err = s.GetRecoveryObservations(ctx, []string{a.ID})
	require.NoError(t, err)
	assert.Equal(t, store.ObservedPresentRunning, got[a.ID].State)
	assert.Nil(t, got[a.ID].FirstAbsentAt, "a present observation clears first_absent_at")
	assert.False(t, got[a.ID].InFlight)

	inv, err := s.ListBrokerTargetInventory(ctx, broker)
	require.NoError(t, err)
	require.Len(t, inv, 1, "only complete targets get an inventory time")
	assert.Equal(t, "A", inv[0].Target)

	_, err = s.RecordRecoveryObservations(ctx, broker, []string{"A"}, []store.RecoveryObservation{{AgentID: a.ID, Target: "A", State: "bogus"}})
	require.ErrorIs(t, err, store.ErrInvalidInput)
}

func TestRecoveryObs_PerTargetInventoryTime(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestAgentStore(t)
	const broker = "broker-1"

	tA1, err := s.RecordRecoveryObservations(ctx, broker, []string{"A", "B"}, nil)
	require.NoError(t, err)
	time.Sleep(2 * time.Millisecond)
	tA2, err := s.RecordRecoveryObservations(ctx, broker, []string{"A"}, nil)
	require.NoError(t, err)

	inv, err := s.ListBrokerTargetInventory(ctx, broker)
	require.NoError(t, err)
	require.Len(t, inv, 2)
	byTarget := map[string]time.Time{}
	for _, r := range inv {
		byTarget[r.Target] = r.LastCompleteInventoryAt
	}
	assert.True(t, byTarget["A"].Equal(tA2), "A advances with each complete inventory")
	assert.True(t, byTarget["B"].Equal(tA1), "B keeps its last complete time while it is incomplete")

	other, err := s.ListBrokerTargetInventory(ctx, "broker-2")
	require.NoError(t, err)
	assert.Empty(t, other)

	zero, err := s.RecordRecoveryObservations(ctx, broker, nil, nil)
	require.NoError(t, err)
	assert.True(t, zero.IsZero(), "no complete target writes nothing")
}

func TestRecoveryObs_UpdateRuntimeBrokerLeavesInventoryAndDeletesCascade(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)

	b := newBroker()
	require.NoError(t, cs.CreateRuntimeBroker(ctx, b))
	p := &store.Project{ID: "40000000-0000-0000-0000-0000000000b1", Name: "obs", Slug: "obs"}
	require.NoError(t, cs.CreateProject(ctx, p))
	a := makeAgent(p.ID, "obs-cascade")
	require.NoError(t, cs.CreateAgent(ctx, a))

	at, err := cs.RecordRecoveryObservations(ctx, b.ID, []string{"A"}, []store.RecoveryObservation{
		{AgentID: a.ID, Target: "A", State: store.ObservedAbsent},
	})
	require.NoError(t, err)

	// A broker record written back from memory cannot roll the inventory
	// time back: it is not a runtime_brokers column.
	stale, err := cs.GetRuntimeBroker(ctx, b.ID)
	require.NoError(t, err)
	stale.Version = "changed"
	require.NoError(t, cs.UpdateRuntimeBroker(ctx, stale))
	inv, err := cs.ListBrokerTargetInventory(ctx, b.ID)
	require.NoError(t, err)
	require.Len(t, inv, 1)
	assert.True(t, inv[0].LastCompleteInventoryAt.Equal(at))

	require.NoError(t, cs.DeleteAgent(ctx, a.ID))
	obs, err := cs.GetRecoveryObservations(ctx, []string{a.ID})
	require.NoError(t, err)
	assert.Empty(t, obs, "deleting the agent deletes its observation")

	require.NoError(t, cs.DeleteRuntimeBroker(ctx, b.ID))
	inv, err = cs.ListBrokerTargetInventory(ctx, b.ID)
	require.NoError(t, err)
	assert.Empty(t, inv, "deleting the broker deletes its inventory times")
}

func TestRecoveryObs_UpsertsOverConcurrentInsert(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := newClaimAgent(t, ctx, s, projectID, "obs-upsert")
	// A row inserted by another writer before this heartbeat's write.
	_, err := s.client.AgentRecovery.Create().SetID(a.ID).SetBrokerID("other").SetObservedState(string(store.ObservedPresentRunning)).Save(ctx)
	require.NoError(t, err)
	_, err = s.RecordRecoveryObservations(ctx, "broker-1", []string{"A"}, []store.RecoveryObservation{
		{AgentID: a.ID, Target: "A", State: store.ObservedAbsent},
	})
	require.NoError(t, err)
	got, err := s.GetRecoveryObservations(ctx, []string{a.ID})
	require.NoError(t, err)
	assert.Equal(t, store.ObservedAbsent, got[a.ID].State)
	assert.Equal(t, "broker-1", got[a.ID].BrokerID)
	require.NotNil(t, got[a.ID].FirstAbsentAt)
}
