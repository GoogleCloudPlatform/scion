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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newProviderSelfHealFixture creates a broker and a project linked to it as
// the default provider -- the same minimal wiring getAvailableBrokersForProject
// (agent-create's broker-selection path) reads.
func newProviderSelfHealFixture(t *testing.T, s store.Store, suffix string) (*store.RuntimeBroker, *store.Project) {
	t.Helper()
	ctx := context.Background()

	broker := &store.RuntimeBroker{
		ID:     tid("broker-selfheal-" + suffix),
		Name:   "Self-Heal Broker " + suffix,
		Slug:   "selfheal-broker-" + suffix,
		Status: store.BrokerStatusOffline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	project := &store.Project{
		ID:   tid("proj-selfheal-" + suffix),
		Name: "Self-Heal Project " + suffix,
		Slug: "selfheal-project-" + suffix,
	}
	require.NoError(t, s.CreateProject(ctx, project))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOffline,
	}))
	project.DefaultRuntimeBrokerID = broker.ID
	require.NoError(t, s.UpdateProject(ctx, project))

	return broker, project
}

// TestBrokerProviderSelfHeal_RestoresAvailabilityAfterAffinityOwnerDisconnects
// reproduces issue #2090: a broker_id served by two live sessions (e.g. a
// co-located broker embedded in every replica of a multi-instance Hub
// deployment). When the instance holding affinity disconnects (scale-in), its
// disconnect callback (handleBrokerDisconnect) stamps every project-provider
// row for the broker offline. The surviving instance's own control-channel
// session never drops, so it never re-runs the connect path (markBrokerOnline)
// that would otherwise restore the provider rows, and agent-create is stuck
// reporting "Default runtime broker is unavailable".
//
// brokerProviderSelfHealHandler must restore the provider rows from the
// surviving instance's live local socket, without needing affinity to move.
func TestBrokerProviderSelfHeal_RestoresAvailabilityAfterAffinityOwnerDisconnects(t *testing.T) {
	ctx := context.Background()
	srv1, s := testServer(t)

	// A second Hub replica sharing the same store: a distinct instanceID and a
	// distinct, in-process control-channel connections map, modelling a
	// second instance of the same multi-instance deployment.
	srv2, err := New(srv1.config, s)
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv2.Shutdown(ctx) })

	broker, project := newProviderSelfHealFixture(t, s, "2inst")

	const session1 = "sess-1"
	const session2 = "sess-2"

	// Both instances' co-located brokers dial in under the same broker_id.
	// Instance 1 connected most recently, so it holds affinity.
	srv1.controlChannel.mu.Lock()
	srv1.controlChannel.connections[broker.ID] = &BrokerConnection{brokerID: broker.ID, sessionID: session1}
	srv1.controlChannel.mu.Unlock()
	srv2.controlChannel.mu.Lock()
	srv2.controlChannel.connections[broker.ID] = &BrokerConnection{brokerID: broker.ID, sessionID: session2}
	srv2.controlChannel.mu.Unlock()

	srv1.markBrokerOnline(broker.ID, session1)

	// Sanity: the fixture is genuinely available before the disconnect.
	available, err := srv1.getAvailableBrokersForProject(ctx, project.ID)
	require.NoError(t, err)
	require.Len(t, available, 1)

	// Scale-in: instance 1 (the affinity owner) disconnects. Instance 2's
	// socket is untouched -- it never reconnects, so nothing about its local
	// state changes and it never re-triggers markBrokerOnline.
	srv1.handleBrokerDisconnect(ctx, broker.ID, session1)

	// The bug: the disconnect callback stamped every provider offline, and
	// agent-create's broker-selection now sees nothing available.
	provider, err := s.GetProjectProvider(ctx, project.ID, broker.ID)
	require.NoError(t, err)
	require.Equal(t, store.BrokerStatusOffline, provider.Status, "precondition: disconnect must stamp the provider offline")

	available, err = srv1.getAvailableBrokersForProject(ctx, project.ID)
	require.NoError(t, err)
	require.Empty(t, available, "precondition: broker must look unavailable immediately after disconnect")

	// The fix: instance 2 still holds a live local control-channel socket for
	// this broker_id, so its self-heal tick restores the provider row.
	srv2.brokerProviderSelfHealHandler()(ctx)

	provider, err = s.GetProjectProvider(ctx, project.ID, broker.ID)
	require.NoError(t, err)
	assert.Equal(t, store.BrokerStatusOnline, provider.Status, "surviving instance must restore the provider online")

	// The broker-status half of this gap (runtime_brokers.status, as opposed
	// to the project-provider rows) is already self-healed by the pre-existing
	// heartbeat mechanisms (the co-located internal heartbeat loop and the
	// generic broker heartbeat handler both unconditionally report "online").
	// Simulate that here so the end-to-end availability check reflects what a
	// real surviving instance would produce.
	require.NoError(t, s.UpdateRuntimeBrokerHeartbeat(ctx, broker.ID, store.BrokerStatusOnline))

	available, err = srv1.getAvailableBrokersForProject(ctx, project.ID)
	require.NoError(t, err)
	require.Len(t, available, 1, "agent-create broker selection must see the broker available again")
	assert.Equal(t, broker.ID, available[0].ID)
}

// TestBrokerProviderSelfHeal_GenuineDisconnectStaysOffline is the safety
// counterpart: when no instance holds a live local socket for the broker_id
// (a genuine full disconnect, not scale-in of one of several redundant
// sessions), the self-heal handler must not resurrect the provider row --
// there is no live socket to base that on, and doing so would mask an
// actually-dead broker until the heartbeat-timeout reaper caught up.
func TestBrokerProviderSelfHeal_GenuineDisconnectStaysOffline(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)

	broker, project := newProviderSelfHealFixture(t, s, "full")

	const session1 = "sess-only"
	srv.controlChannel.mu.Lock()
	srv.controlChannel.connections[broker.ID] = &BrokerConnection{brokerID: broker.ID, sessionID: session1}
	srv.controlChannel.mu.Unlock()

	srv.markBrokerOnline(broker.ID, session1)

	// Genuine disconnect: the only live socket drops and is actually removed
	// from the local connections map first, matching what
	// ControlChannelManager.removeConnection does before firing onDisconnect.
	srv.controlChannel.mu.Lock()
	delete(srv.controlChannel.connections, broker.ID)
	srv.controlChannel.mu.Unlock()
	srv.handleBrokerDisconnect(ctx, broker.ID, session1)

	provider, err := s.GetProjectProvider(ctx, project.ID, broker.ID)
	require.NoError(t, err)
	require.Equal(t, store.BrokerStatusOffline, provider.Status)

	// No instance anywhere holds a live socket for this broker_id, so
	// self-heal must leave it alone.
	srv.brokerProviderSelfHealHandler()(ctx)

	provider, err = s.GetProjectProvider(ctx, project.ID, broker.ID)
	require.NoError(t, err)
	assert.Equal(t, store.BrokerStatusOffline, provider.Status, "self-heal must not restore a genuinely disconnected broker's provider")
}

// TestBrokerProviderSelfHeal_SkipsAlreadyOnlineRows covers review item 3: the
// self-heal path (unlike stampProvidersOnline, which markBrokerOnline uses on
// every connect and must keep refreshing last_seen) must skip provider rows
// that GetBrokerProjects already reports as online, rather than
// unconditionally re-stamping them on every tick. UpdateProviderStatus always
// sets last_seen to time.Now(), so an unchanged last_seen after the tick is
// proof no UPDATE was issued for this row.
func TestBrokerProviderSelfHeal_SkipsAlreadyOnlineRows(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)

	broker, project := newProviderSelfHealFixture(t, s, "skip")

	const session1 = "sess-skip"
	srv.controlChannel.mu.Lock()
	srv.controlChannel.connections[broker.ID] = &BrokerConnection{brokerID: broker.ID, sessionID: session1}
	srv.controlChannel.mu.Unlock()

	// The connect path stamps the provider online and sets last_seen.
	srv.markBrokerOnline(broker.ID, session1)

	before, err := s.GetProjectProvider(ctx, project.ID, broker.ID)
	require.NoError(t, err)
	require.Equal(t, store.BrokerStatusOnline, before.Status, "precondition: connect must stamp the provider online")

	// The broker is still connected, so a self-heal tick reaches this row and
	// finds it already online.
	srv.brokerProviderSelfHealHandler()(ctx)

	after, err := s.GetProjectProvider(ctx, project.ID, broker.ID)
	require.NoError(t, err)
	assert.Equal(t, store.BrokerStatusOnline, after.Status)
	assert.True(t, before.LastSeen.Equal(after.LastSeen),
		"self-heal must not UPDATE an already-online provider row: last_seen must stay unchanged")
}

// TestBrokerProviderSelfHeal_SkipsBrokerNoLongerConnected covers review item
// 2: the self-heal loop re-checks IsConnected immediately before stamping
// each broker, rather than trusting its snapshot argument for the whole tick.
// It drives selfHealBrokerProviders directly with a stale snapshot -- a
// broker ID that is not in srv.controlChannel.connections -- so the test
// actually exercises the re-check rather than merely repeating
// GenuineDisconnectStaysOffline in a weaker form.
func TestBrokerProviderSelfHeal_SkipsBrokerNoLongerConnected(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)

	broker, project := newProviderSelfHealFixture(t, s, "recheck")

	// No entry in srv.controlChannel.connections for this broker, but the
	// snapshot passed in claims it was connected -- modelling a disconnect
	// that landed between ListConnectedBrokers() and this call.
	srv.selfHealBrokerProviders(ctx, []string{broker.ID})

	provider, err := s.GetProjectProvider(ctx, project.ID, broker.ID)
	require.NoError(t, err)
	assert.Equal(t, store.BrokerStatusOffline, provider.Status,
		"self-heal must not stamp a broker that is no longer connected, even if the snapshot says it was")
}

// TestBrokerProviderSelfHeal_RegisteredNonSingleton pins review item 4: the
// scheduler must register "broker-provider-selfheal" via RegisterRecurring,
// not RegisterRecurringSingleton. This handler decides health from
// per-process, in-memory socket state (ControlChannelManager.connections), so
// it must run on every replica; wrapping it in RegisterRecurringSingleton
// would let only one replica's local-socket view drive every replica's
// provider rows, defeating the fix for issue #2090.
//
// The registration mode is read from RecurringHandler.Singleton, a field set
// explicitly by RegisterRecurringSingleton, rather than inferred from
// function identity: closures produced by RegisterRecurringSingleton at
// different call sites are not guaranteed to share a function pointer once
// inlined, so a pointer-identity check can pass even when the handler is
// wrongly registered as a singleton.
//
// broker-heartbeat-timeout is a positive control: it must have Singleton ==
// true, so this test would fail if the field were never set at all rather
// than only failing to distinguish self-heal's registration.
//
// registerSchedulerHandlers is exercised directly (not via
// StartBackgroundServices) so this test only builds the scheduler's
// registration table, without starting the ticker or any other background
// service.
func TestBrokerProviderSelfHeal_RegisteredNonSingleton(t *testing.T) {
	srv, _ := testServer(t)
	srv.scheduler = NewScheduler(srv.store, logging.Subsystem("hub.scheduler"))
	srv.registerSchedulerHandlers()

	var selfHeal, heartbeatTimeout *RecurringHandler
	for i := range srv.scheduler.recurring {
		switch srv.scheduler.recurring[i].Name {
		case "broker-provider-selfheal":
			selfHeal = &srv.scheduler.recurring[i]
		case "broker-heartbeat-timeout":
			heartbeatTimeout = &srv.scheduler.recurring[i]
		}
	}
	require.NotNil(t, selfHeal, "broker-provider-selfheal must be registered as a recurring handler")
	assert.Equal(t, 1, selfHeal.Interval, "must run every tick so surviving instances self-heal promptly")
	assert.False(t, selfHeal.Singleton,
		"broker-provider-selfheal must be registered via RegisterRecurring, not RegisterRecurringSingleton")

	require.NotNil(t, heartbeatTimeout, "positive control: broker-heartbeat-timeout must be registered")
	assert.True(t, heartbeatTimeout.Singleton,
		"positive control: broker-heartbeat-timeout must be registered via RegisterRecurringSingleton")
}
