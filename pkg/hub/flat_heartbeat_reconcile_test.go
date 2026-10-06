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
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// P1.3 part 2 (ptone/scion#3269): heartbeats and inventory reconcile keep a
// flat agent on its saved Runtime Broker instance and target. Heartbeat
// data is observed state only: it never re-points runtime_broker_id or the
// pin, and never moves an agent between a flat and a legacy Runtime Broker.

func sendFlatHeartbeat(t *testing.T, f *flatHubFixture, brokerID string, hb brokerHeartbeatRequest) {
	t.Helper()
	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/runtime-brokers/"+brokerID+"/heartbeat", hb)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func flatAgentHeartbeat(slug string) brokerAgentHeartbeat {
	return brokerAgentHeartbeat{Slug: slug, Phase: "running", Activity: "working", RuntimeTarget: "docker"}
}

// placementOf is the stored placement: runtime_broker_id plus the pin.
func placementOf(t *testing.T, s store.Store, agentID string) (string, store.PinnedPlacement) {
	t.Helper()
	a, err := s.GetAgent(context.Background(), agentID)
	require.NoError(t, err)
	return a.RuntimeBrokerID, store.PinnedPlacement{
		RuntimeBrokerID: a.PinnedRuntimeBrokerID, RuntimeTargetID: a.PinnedRuntimeTargetID, RuntimeTargetType: a.PinnedRuntimeTargetType,
	}
}

func requireFlatRowUnchanged(t *testing.T, s store.Store, want *store.RuntimeBroker) {
	t.Helper()
	got, err := s.GetRuntimeBroker(context.Background(), want.ID)
	require.NoError(t, err)
	require.NotNil(t, got.RuntimeTarget, "the flat row keeps its runtime target")
	assert.Equal(t, *want.RuntimeTarget, *got.RuntimeTarget, "runtime target unchanged")
	assert.Equal(t, want.Name, got.Name)
	assert.Empty(t, got.Profiles, "no profiles on a flat row")
	assert.Empty(t, got.DefaultProfile, "no default profile on a flat row")
}

// TestFlatHeartbeat_AfterRestartKeepsIdentityTargetAndPlacement: after a Hub
// restart, a heartbeat from the flat instance carrying every profile-scoped
// field updates the agent's observed state only. The Runtime Broker row
// keeps its identity and target with no profiles, and the agent keeps its
// runtime_broker_id and pin, with no profile backfilled.
func TestFlatHeartbeat_AfterRestartKeepsIdentityTargetAndPlacement(t *testing.T) {
	ctx := context.Background()
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	grantDevUserRuntimeBrokerAccess(t, f.s)
	a := f.pinnedAgent(t, "hb-restart", string(state.PhaseRunning))
	wantBroker, wantPin := placementOf(t, f.s, a.ID)
	require.Equal(t, f.flat.ID, wantBroker)

	// Heartbeats do not depend on the experiment.
	restartFlatHub(t, f, false)

	batch := "batch"
	hb := brokerHeartbeatRequest{
		Status:         store.BrokerStatusOnline,
		Capabilities:   &store.BrokerCapabilities{Reprovision: true, Sync: true, StartsInFlight: true},
		Inventory:      completeInventory(),
		DefaultProfile: &batch,
		ProfileAttach:  []brokerProfileAttach{{Name: "batch", Attach: true}},
		ProfileSAMappings: []brokerProfileSAMappings{{Name: "batch", ServiceAccountMappings: []store.BrokerProfileSAMapping{
			{GSA: "gsa@example.iam"},
		}}},
	}
	agentHB := flatAgentHeartbeat(a.Slug)
	agentHB.Profile = "batch"
	hb.Projects = []brokerProjectHeartbeat{{ProjectID: f.project.ID, AgentCount: 1, Agents: []brokerAgentHeartbeat{agentHB}}}
	sendFlatHeartbeat(t, f, f.flat.ID, hb)
	sendFlatHeartbeat(t, f, f.flat.ID, hb)

	requireFlatRowUnchanged(t, f.s, f.flat)
	row, err := f.s.GetRuntimeBroker(ctx, f.flat.ID)
	require.NoError(t, err)
	assert.Equal(t, store.BrokerStatusOnline, row.Status)
	assert.Equal(t, len(f.flat.Labels), len(row.Labels), "labels unchanged")

	gotBroker, gotPin := placementOf(t, f.s, a.ID)
	assert.Equal(t, wantBroker, gotBroker, "runtime_broker_id unchanged")
	assert.Equal(t, wantPin, gotPin, "pin unchanged")
	got, err := f.s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, "working", got.Activity, "the observed state is applied")
	assert.WithinDuration(t, time.Now(), got.LastSeen, time.Minute)
	require.NotNil(t, got.AppliedConfig)
	assert.Empty(t, got.AppliedConfig.Profile, "no Runtime Broker-reported profile on a flat agent")
	assert.Equal(t, "docker", got.AppliedConfig.RuntimeTarget, "the inventory target key is recorded")
}

// TestFlatHeartbeat_MissingAgentFollowsObservedStateRules: a running flat
// agent absent from complete inventories past the grace period is marked
// container-missing exactly as a legacy agent is, and its placement stays.
func TestFlatHeartbeat_MissingAgentFollowsObservedStateRules(t *testing.T) {
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	grantDevUserRuntimeBrokerAccess(t, f.s)
	a := f.pinnedAgentWith(t, "hb-missing", string(state.PhaseRunning), func(a *store.Agent) {
		a.Activity = "working"
		a.LastSeen = time.Now().Add(-time.Hour)
		a.AppliedConfig.RuntimeTarget = "docker"
	})
	wantBroker, wantPin := placementOf(t, f.s, a.ID)
	empty := brokerHeartbeatRequest{Status: store.BrokerStatusOnline, Inventory: completeInventory()}

	// The first heartbeat makes the broker online and fresh; the second
	// starts the agent's missing clock.
	sendFlatHeartbeat(t, f, f.flat.ID, empty)
	sendFlatHeartbeat(t, f, f.flat.ID, empty)
	tr := &f.srv.missingAgents
	tr.mu.Lock()
	require.Contains(t, tr.since[f.flat.ID], a.ID, "the flat agent's missing clock started under its own Runtime Broker")
	tr.since[f.flat.ID][a.ID] = time.Now().Add(-2 * f.srv.missingAgentGrace())
	tr.mu.Unlock()
	sendFlatHeartbeat(t, f, f.flat.ID, empty)

	got, err := f.s.GetAgent(context.Background(), a.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseError), got.Phase)
	assert.Equal(t, string(state.ExitReasonContainerMissing), got.ExitReason)
	gotBroker, gotPin := placementOf(t, f.s, a.ID)
	assert.Equal(t, wantBroker, gotBroker, "runtime_broker_id unchanged")
	assert.Equal(t, wantPin, gotPin, "not re-pinned")
	requireFlatRowUnchanged(t, f.s, f.flat)
}

// TestFlatHeartbeat_NoAdoptionAcrossFlatAndLegacy: a legacy Runtime
// Broker's heartbeat that names a flat agent, and a flat instance's
// heartbeat that names a legacy agent, change neither agent; and a legacy
// broker's complete inventory never reconciles a flat agent.
func TestFlatHeartbeat_NoAdoptionAcrossFlatAndLegacy(t *testing.T) {
	ctx := context.Background()
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	grantDevUserRuntimeBrokerAccess(t, f.s)
	old := time.Now().Add(-time.Hour)
	flatAgent := f.pinnedAgentWith(t, "hb-flat", string(state.PhaseRunning), func(a *store.Agent) {
		a.Activity = "idle"
		a.LastSeen = old
		a.AppliedConfig.RuntimeTarget = "docker"
	})
	legacyAgent := f.unpinnedAgentOnWith(t, "hb-legacy", f.legacy.ID, string(state.PhaseRunning), func(a *store.Agent) {
		a.Activity = "idle"
		a.LastSeen = old
	})

	report := func(slug string) brokerHeartbeatRequest {
		return brokerHeartbeatRequest{Status: store.BrokerStatusOnline, Inventory: completeInventory(), Projects: []brokerProjectHeartbeat{{
			ProjectID: f.project.ID, AgentCount: 1, Agents: []brokerAgentHeartbeat{flatAgentHeartbeat(slug)},
		}}}
	}
	for i := 0; i < 2; i++ {
		sendFlatHeartbeat(t, f, f.legacy.ID, report(flatAgent.Slug))
		sendFlatHeartbeat(t, f, f.flat.ID, report(legacyAgent.Slug))
	}

	for _, a := range []*store.Agent{flatAgent, legacyAgent} {
		got, err := f.s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, "idle", got.Activity, "%s: another Runtime Broker's report is not applied", a.Slug)
		assert.Equal(t, a.RuntimeBrokerID, got.RuntimeBrokerID, "%s: runtime_broker_id unchanged", a.Slug)
		assert.Equal(t, a.PinnedRuntimeBrokerID, got.PinnedRuntimeBrokerID, "%s: pin unchanged", a.Slug)
		assert.Equal(t, a.PinnedRuntimeTargetID, got.PinnedRuntimeTargetID, "%s: pin unchanged", a.Slug)
	}
	tr := &f.srv.missingAgents
	tr.mu.Lock()
	defer tr.mu.Unlock()
	assert.NotContains(t, tr.since[f.legacy.ID], flatAgent.ID, "a legacy inventory never reconciles a flat agent")
}

// TestFlatHeartbeat_RunIDAndRecoverySemanticsPreserved: for a flat agent,
// a heartbeat leaves the run ID alone, the target inventory is recorded
// under the flat Runtime Broker, and recovery observations follow the
// existing rules: an absent agent meant to run is observed absent, and a
// start the instance reports in flight is recorded in flight.
func TestFlatHeartbeat_RunIDAndRecoverySemanticsPreserved(t *testing.T) {
	ctx := context.Background()
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	grantDevUserRuntimeBrokerAccess(t, f.s)
	lost := f.pinnedAgent(t, "hb-lost", string(state.PhaseError))
	inflight := f.pinnedAgent(t, "hb-inflight", string(state.PhaseError))
	for _, a := range []*store.Agent{lost, inflight} {
		_, err := f.s.SetRunIntent(ctx, a.ID, store.RunIntentRunning)
		require.NoError(t, err)
	}
	_, err := f.s.SetAgentRunID(ctx, lost.ID, "run-flat-1")
	require.NoError(t, err)

	hb := brokerHeartbeatRequest{
		Status:         store.BrokerStatusOnline,
		Inventory:      completeInventory(),
		Capabilities:   &store.BrokerCapabilities{StartsInFlight: true},
		StartsInFlight: []brokerStartInFlight{{ProjectID: f.project.ID, Slug: inflight.Slug}},
	}
	sendFlatHeartbeat(t, f, f.flat.ID, hb)
	sendFlatHeartbeat(t, f, f.flat.ID, hb)

	obs, err := f.s.GetRecoveryObservations(ctx, []string{lost.ID, inflight.ID})
	require.NoError(t, err)
	require.Contains(t, obs, lost.ID)
	assert.Equal(t, store.ObservedAbsent, obs[lost.ID].State)
	assert.False(t, obs[lost.ID].InFlight)
	require.Contains(t, obs, inflight.ID)
	assert.True(t, obs[inflight.ID].InFlight, "a start the flat instance reports in flight is recorded")

	inv, err := f.s.ListBrokerTargetInventory(ctx, f.flat.ID)
	require.NoError(t, err)
	require.Len(t, inv, 1)
	assert.Equal(t, "docker", inv[0].Target, "the inventory target key, not the runtime target ID")

	got, err := f.s.GetAgent(ctx, lost.ID)
	require.NoError(t, err)
	assert.Equal(t, "run-flat-1", got.RunID, "a heartbeat never changes the run ID")
	for _, a := range []*store.Agent{lost, inflight} {
		gotBroker, gotPin := placementOf(t, f.s, a.ID)
		assert.Equal(t, f.flat.ID, gotBroker)
		assert.Equal(t, f.flat.RuntimeTarget.ID, gotPin.RuntimeTargetID)
	}
}

// TestFlatHeartbeat_NoProfileBackfillOnFlatRuntimeBroker: an agent of a
// flat Runtime Broker without a pin (a stale state no current writer
// produces) gets no Runtime Broker-reported profile from a heartbeat either.
func TestFlatHeartbeat_NoProfileBackfillOnFlatRuntimeBroker(t *testing.T) {
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	grantDevUserRuntimeBrokerAccess(t, f.s)
	a := f.unpinnedAgentOnWith(t, "hb-null-pin", f.flat.ID, string(state.PhaseRunning), nil)
	agentHB := flatAgentHeartbeat(a.Slug)
	agentHB.Profile = "batch"
	sendFlatHeartbeat(t, f, f.flat.ID, brokerHeartbeatRequest{Status: store.BrokerStatusOnline, Projects: []brokerProjectHeartbeat{{
		ProjectID: f.project.ID, AgentCount: 1, Agents: []brokerAgentHeartbeat{agentHB},
	}}})
	got, err := f.s.GetAgent(context.Background(), a.ID)
	require.NoError(t, err)
	assert.Equal(t, "working", got.Activity, "the observed state is applied")
	if got.AppliedConfig != nil {
		assert.Empty(t, got.AppliedConfig.Profile, "no profile backfilled onto an agent of a flat Runtime Broker")
	}
	assert.Equal(t, f.flat.ID, got.RuntimeBrokerID)
	assert.False(t, got.IsPinned(), "a heartbeat never pins")
}
