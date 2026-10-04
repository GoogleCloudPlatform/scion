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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecoveryObservations_HeartbeatObservedState(t *testing.T) {
	code := 1
	cases := []struct {
		hb   brokerAgentHeartbeat
		want store.RecoveryObservedState
	}{
		{brokerAgentHeartbeat{Phase: "running"}, store.ObservedPresentRunning},
		{brokerAgentHeartbeat{Phase: "starting"}, store.ObservedPresentRunning},
		{brokerAgentHeartbeat{ContainerStatus: "Pending"}, store.ObservedPresentRunning},
		{brokerAgentHeartbeat{Phase: "stopped"}, store.ObservedPresentTerminal},
		{brokerAgentHeartbeat{Phase: "error"}, store.ObservedPresentTerminal},
		{brokerAgentHeartbeat{ContainerStatus: "Exited (255) 2 hours ago"}, store.ObservedPresentTerminal},
		{brokerAgentHeartbeat{ContainerStatus: "Succeeded"}, store.ObservedPresentTerminal},
		{brokerAgentHeartbeat{Phase: "running", ExitCode: &code}, store.ObservedPresentTerminal},
		{brokerAgentHeartbeat{Phase: "running", ExitReason: "evicted"}, store.ObservedPresentTerminal},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, heartbeatObservedState(tc.hb), "%+v", tc.hb)
	}
}

func TestRecoveryObservations_NeedsObservation(t *testing.T) {
	cases := []struct {
		name string
		a    store.Agent
		want bool
	}{
		{"intent running, not running", store.Agent{RunIntent: store.RunIntentRunning, Phase: "error"}, true},
		{"intent running, running", store.Agent{RunIntent: store.RunIntentRunning, Phase: "running"}, false},
		{"unconfirmed claim", store.Agent{StartClaimID: "c", StartClaimState: store.StartClaimUnconfirmed, Phase: "created"}, true},
		{"live claim", store.Agent{StartClaimID: "c", StartClaimState: store.StartClaimLive, RunIntent: store.RunIntentRunning, Phase: "running"}, false},
		{"intent stopped, running, no claim", store.Agent{RunIntent: store.RunIntentStopped, Phase: "running"}, true},
		{"intent stopped, running, claim", store.Agent{RunIntent: store.RunIntentStopped, Phase: "running", StartClaimID: "c", StartClaimState: store.StartClaimLive}, false},
		{"intent stopped, stopped", store.Agent{RunIntent: store.RunIntentStopped, Phase: "stopped"}, false},
		{"provisioned only", store.Agent{RunIntent: store.RunIntentStopped, Phase: "created"}, false},
		{"deleted", store.Agent{RunIntent: store.RunIntentRunning, Phase: "error", DeletedAt: time.Now()}, false},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, needsRecoveryObservation(&tc.a), tc.name)
	}
}

func TestRecoveryObservations_Freshness(t *testing.T) {
	now := time.Now()
	obs := store.RecoveryObservationRecord{Target: "A", ObservedAt: now.Add(-10 * time.Second)}
	inv := map[string]time.Time{"A": now.Add(-10 * time.Second), "B": now.Add(-time.Hour)}

	assert.True(t, observationFresh(obs, inv, &store.RuntimeBroker{}, now, true))

	// Per target: B has not been listed complete for an hour, so its
	// observations are stale even though A is current.
	b := obs
	b.Target = "B"
	b.ObservedAt = now.Add(-time.Hour)
	assert.False(t, observationFresh(b, inv, &store.RuntimeBroker{}, now, true), "an incomplete target's observation is stale")

	unknown := obs
	unknown.Target = "C"
	assert.False(t, observationFresh(unknown, inv, &store.RuntimeBroker{}, now, true))

	// Current session: the inventory must postdate the control channel
	// session start.
	connected := now.Add(-5 * time.Second)
	assert.False(t, observationFresh(obs, inv, &store.RuntimeBroker{ConnectedAt: &connected}, now, true), "an inventory from before this session is stale")
	assert.True(t, observationFresh(obs, inv, &store.RuntimeBroker{ConnectedAt: &connected}, now, false), "the session rule is skipped where connected_at is on another clock")
	earlier := now.Add(-time.Minute)
	assert.True(t, observationFresh(obs, inv, &store.RuntimeBroker{ConnectedAt: &earlier}, now, true))

	newer := obs
	newer.ObservedAt = now
	assert.False(t, observationFresh(newer, inv, &store.RuntimeBroker{}, now, true), "an observation newer than its inventory is not covered by it")

	assert.False(t, observationFresh(obs, inv, &store.RuntimeBroker{}, now.Add(2*observationFreshness), true), "older than two heartbeat intervals is stale")
}

func TestRecoveryObservations_RecordedFromHeartbeat(t *testing.T) {
	f := newReconcileFixture(t)
	ctx := context.Background()
	lost := f.addAgent("lost", "error", "")
	inflight := f.addAgent("inflight", "error", "")
	running := f.addAgent("running", "running", "working")
	stoppedRunning := f.addAgent("stopped-running", "running", "working")
	provisioned := f.addAgent("provisioned", "created", "")
	for _, a := range []*store.Agent{lost, inflight, running} {
		_, err := f.s.SetRunIntent(ctx, a.ID, store.RunIntentRunning)
		require.NoError(t, err)
	}
	for _, a := range []*store.Agent{stoppedRunning, provisioned} {
		_, err := f.s.SetRunIntent(ctx, a.ID, store.RunIntentStopped)
		require.NoError(t, err)
	}

	f.send(brokerHeartbeatRequest{
		Status:         store.BrokerStatusOnline,
		Inventory:      completeInventory(),
		Capabilities:   &store.BrokerCapabilities{StartsInFlight: true},
		StartsInFlight: []brokerStartInFlight{{ProjectID: f.projectID, Slug: "inflight"}},
		Projects: []brokerProjectHeartbeat{{ProjectID: f.projectID, Agents: []brokerAgentHeartbeat{
			{Slug: "running", Phase: "running", RuntimeTarget: "docker"},
			{Slug: "stopped-running", Phase: "running", RuntimeTarget: "docker"},
		}}},
	})

	got, err := f.s.GetRecoveryObservations(ctx, []string{lost.ID, inflight.ID, running.ID, stoppedRunning.ID, provisioned.ID})
	require.NoError(t, err)
	require.Contains(t, got, lost.ID)
	assert.Equal(t, store.ObservedAbsent, got[lost.ID].State)
	assert.False(t, got[lost.ID].InFlight)
	require.Contains(t, got, inflight.ID)
	assert.Equal(t, store.ObservedAbsent, got[inflight.ID].State)
	assert.True(t, got[inflight.ID].InFlight, "a start the broker reports in flight is recorded")
	require.Contains(t, got, stoppedRunning.ID)
	assert.Equal(t, store.ObservedPresentRunning, got[stoppedRunning.ID].State)
	assert.NotContains(t, got, running.ID, "an agent running as intended is not observed")
	assert.NotContains(t, got, provisioned.ID, "a provisioned agent at rest is not observed")

	inv, err := f.s.ListBrokerTargetInventory(ctx, f.brokerID)
	require.NoError(t, err)
	require.Len(t, inv, 1)
	assert.Equal(t, "docker", inv[0].Target)
}

func TestRecoveryObservations_UnconfirmedClaimWithoutTargetUsesClaimTarget(t *testing.T) {
	f := newReconcileFixture(t)
	ctx := context.Background()
	a := f.addAgent("new", "created", "", func(a *store.Agent) { a.AppliedConfig = &store.AgentAppliedConfig{} })
	c, err := f.s.ClaimAgentStart(ctx, a.ID, "hub-1", store.StartClaimCreate, "docker", time.Minute)
	require.NoError(t, err)
	_, err = f.s.MarkStartUnconfirmed(ctx, a.ID, c.ID, "hub-1", time.Minute)
	require.NoError(t, err)

	f.heartbeat(completeInventory())
	got, err := f.s.GetRecoveryObservations(ctx, []string{a.ID})
	require.NoError(t, err)
	require.Contains(t, got, a.ID, "an unconfirmed claim with no recorded target is observed on its claim target")
	assert.Equal(t, "docker", got[a.ID].Target)
	assert.Equal(t, store.ObservedAbsent, got[a.ID].State)
}

// An HTTP-only broker (no control channel session) returning after a long
// gap: its first heartbeat is not used (the previous heartbeat is stale),
// so a pre-gap observation does not become fresh until a complete
// inventory in the new period has rewritten it.
func TestRecoveryObservations_HTTPBrokerReconnectFreshness(t *testing.T) {
	f := newReconcileFixture(t)
	ctx := context.Background()
	a := f.addAgent("gone", "error", "")
	_, err := f.s.SetRunIntent(ctx, a.ID, store.RunIntentRunning)
	require.NoError(t, err)

	f.heartbeat(completeInventory())
	before, err := f.s.GetRecoveryObservations(ctx, []string{a.ID})
	require.NoError(t, err)
	require.Contains(t, before, a.ID)

	// Ten minutes pass with no heartbeat.
	broker, err := f.s.GetRuntimeBroker(ctx, f.brokerID)
	require.NoError(t, err)
	require.Nil(t, broker.ConnectedAt, "fixture broker is HTTP-only")
	later := time.Now().Add(10 * time.Minute)
	f.srv.missingAgents.nowFor = func() time.Time { return later }

	f.heartbeat(completeInventory())
	invRows, err := f.s.ListBrokerTargetInventory(ctx, f.brokerID)
	require.NoError(t, err)
	inv := targetInventoryTimes(invRows)
	obs, err := f.s.GetRecoveryObservations(ctx, []string{a.ID})
	require.NoError(t, err)
	assert.True(t, obs[a.ID].ObservedAt.Equal(before[a.ID].ObservedAt), "the first heartbeat after the gap writes nothing")
	assert.False(t, observationFresh(obs[a.ID], inv, broker, later, true), "the pre-gap observation is not fresh after the first heartbeat")
}
