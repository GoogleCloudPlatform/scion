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
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Broker presence (design v2.21.50 §3.4). The registry runs on the broker
// conduit fixture's store (enttest: SQLite by default, PostgreSQL under the
// integration tag with SCION_TEST_POSTGRES_URL set).

// brokerPresenceFixture is the broker conduit fixture (an in-process relay
// with hub.conduit on) and its conduit runtime. The registry runs on a
// FaultStore, whose principal session reads fail while failReads is set,
// and on its own fake clock (regClock) for the timestamps and generations
// it writes; liveness is judged at the runtime's now, the real clock.
type brokerPresenceFixture struct {
	*brokerConduitFixture
	rt        *conduitRuntime
	regClock  *clock.Fake
	failReads atomic.Bool
}

// errBrokerPresenceRegistry is the registry read error the FaultStore returns.
var errBrokerPresenceRegistry = errors.New("forced registry read error")

func newBrokerPresenceFixture(t *testing.T) *brokerPresenceFixture {
	t.Helper()
	pf := &brokerPresenceFixture{regClock: clock.NewFake(time.Now())}
	f := newBrokerConduitFixture(t, nil, func(o *ConduitRelayOptions) {
		fs := &registry.FaultStore{Inner: o.Store, Fault: func(_ context.Context, op string) error {
			if op == registry.OpListPrincipalSessions && pf.failReads.Load() {
				return errBrokerPresenceRegistry
			}
			return nil
		}}
		o.Store = fs
		o.Registry = registry.New(fs, registry.Config{Clock: pf.regClock})
	})
	pf.brokerConduitFixture = f
	pf.rt = f.srv.conduit.Load()
	require.NotNil(t, pf.rt, "the in-process conduit runtime runs")
	return pf
}

// broker creates a broker row with status and returns it as read back.
func (f *brokerPresenceFixture) broker(t *testing.T, name string, status string, mod func(*store.RuntimeBroker)) *store.RuntimeBroker {
	t.Helper()
	ctx := context.Background()
	b := &store.RuntimeBroker{ID: tid(name), Name: name, Slug: name, Status: status, Created: time.Now(), Updated: time.Now()}
	if mod != nil {
		mod(b)
	}
	require.NoError(t, f.store.CreateRuntimeBroker(ctx, b))
	got, err := f.store.GetRuntimeBroker(ctx, b.ID)
	require.NoError(t, err)
	return got
}

// registerRelay registers relay id in the registry with lastSeen (zero:
// the registry clock) and returns its generation.
func (f *brokerPresenceFixture) registerRelay(t *testing.T, id string, lastSeen time.Time) int64 {
	t.Helper()
	gen, err := f.rt.registry.RegisterRelay(context.Background(), registry.RelayInstance{
		InstanceID: id, InternalEndpoint: "http://" + id + ".invalid:9811", LastSeen: lastSeen,
	})
	require.NoError(t, err)
	return gen
}

// brokerSession inserts a session of broker brokerID on relay relayID at
// generation gen, last seen at lastSeen (zero: the registry clock).
func (f *brokerPresenceFixture) brokerSession(t *testing.T, brokerID, relayID string, gen int64, lastSeen time.Time) {
	t.Helper()
	_, err := f.rt.registry.InsertSessionWithNextEpoch(context.Background(), registry.SessionRecord{
		SessionID: "s-" + brokerID + "-" + relayID, PrincipalKind: registry.PrincipalBroker, PrincipalID: brokerID,
		RelayInstanceID: relayID, RelayGeneration: gen, Transport: registry.TransportWS, EndpointIncarnation: "proc-1",
		LastSeen: lastSeen,
	})
	require.NoError(t, err)
}

// liveBrokerSession gives broker brokerID a live, current registry session.
func (f *brokerPresenceFixture) liveBrokerSession(t *testing.T, brokerID string) {
	t.Helper()
	f.brokerSession(t, brokerID, "hub-p", f.registerRelay(t, "hub-p", time.Time{}), time.Time{})
}

func hubIDPtr(s string) *string { return &s }

// TestBrokerPresence_LegacySignals: with hub.conduit off, and with it on
// but no registry session, the legacy signals decide: the control channel
// held here, the affinity columns with a fresh heartbeat, or the stored
// status online. Stale columns and an offline status are not present.
func TestBrokerPresence_LegacySignals(t *testing.T) {
	for _, on := range []bool{false, true} {
		t.Run(experimentName(on), func(t *testing.T) { testBrokerPresenceLegacySignals(t, on) })
	}
}

// experimentName names a subtest by the hub.conduit state, so each subtest
// gets its own stores (the test stores are named after the test).
func experimentName(on bool) string {
	if on {
		return "conduit on"
	}
	return "conduit off"
}

func testBrokerPresenceLegacySignals(t *testing.T, on bool) {
	f := newBrokerPresenceFixture(t)
	setConduitExperiment(t, f.srv, on)
	local := f.broker(t, "presence-local", store.BrokerStatusOffline, nil)
	connectFakeBroker(t, f.srv, local.ID)
	fresh := f.broker(t, "presence-fresh", store.BrokerStatusOffline, func(b *store.RuntimeBroker) {
		b.ConnectedHubID, b.LastHeartbeat = hubIDPtr("hub-x"), time.Now()
	})
	stale := f.broker(t, "presence-stale", store.BrokerStatusOffline, func(b *store.RuntimeBroker) {
		b.ConnectedHubID, b.LastHeartbeat = hubIDPtr("hub-x"), time.Now().Add(-2*defaultAffinityFreshness)
	})
	online := f.broker(t, "presence-online", store.BrokerStatusOnline, nil)
	offline := f.broker(t, "presence-offline", store.BrokerStatusOffline, nil)
	for name, tc := range map[string]struct {
		b    *store.RuntimeBroker
		want brokerPresenceResult
	}{
		"control channel held here": {local, brokerPresenceResult{Connected: true, Source: brokerPresenceLegacy}},
		"fresh affinity columns":    {fresh, brokerPresenceResult{Connected: true, Source: brokerPresenceLegacy}},
		"stale affinity columns":    {stale, brokerPresenceResult{Source: brokerPresenceNone}},
		"status online":             {online, brokerPresenceResult{Connected: true, Source: brokerPresenceLegacy}},
		"status offline":            {offline, brokerPresenceResult{Source: brokerPresenceNone}},
	} {
		assert.Equal(t, tc.want, f.srv.brokerPresence(context.Background(), tc.b), "hub.conduit %v: %s", on, name)
	}
	assert.Zero(t, f.srv.brokerPresenceRegistryFallbacks.Load())
}

// TestBrokerPresence_ColocatedPresentOnAndOff: a co-located broker (status
// online from its own loops, no affinity columns, no control channel here)
// is present with hub.conduit off and on.
func TestBrokerPresence_ColocatedPresentOnAndOff(t *testing.T) {
	for _, on := range []bool{false, true} {
		t.Run(experimentName(on), func(t *testing.T) {
			f := newBrokerPresenceFixture(t)
			setConduitExperiment(t, f.srv, on)
			b := f.broker(t, "presence-colocated", store.BrokerStatusOnline, nil)
			require.Nil(t, b.ConnectedHubID, "no affinity columns")
			got := f.srv.brokerPresence(context.Background(), b)
			assert.True(t, got.Connected, "hub.conduit %v", on)
			assert.True(t, f.srv.brokerRecordReachable(context.Background(), b), "hub.conduit %v", on)
		})
	}
}

// TestBrokerPresence_LiveRegistrySession: with hub.conduit on, a live,
// current registry session makes a broker present even when every legacy
// signal says offline; with hub.conduit off the session is not read.
func TestBrokerPresence_LiveRegistrySession(t *testing.T) {
	f := newBrokerPresenceFixture(t)
	b := f.broker(t, "presence-registry", store.BrokerStatusOffline, nil)
	f.liveBrokerSession(t, b.ID)

	setConduitExperiment(t, f.srv, true)
	assert.Equal(t, brokerPresenceResult{Connected: true, Source: brokerPresenceRegistry}, f.srv.brokerPresence(context.Background(), b))

	setConduitExperiment(t, f.srv, false)
	assert.Equal(t, brokerPresenceResult{Source: brokerPresenceNone}, f.srv.brokerPresence(context.Background(), b))
}

// TestBrokerPresence_RegistrySessionNotLive: a registry session that is
// stale, whose relay is stale, or whose relay generation was superseded
// does not make a broker present; the legacy signals decide (offline
// here).
func TestBrokerPresence_RegistrySessionNotLive(t *testing.T) {
	old := func(f *brokerPresenceFixture) time.Time {
		return f.rt.now().Add(-2 * registry.DefaultSessionStaleAfter)
	}
	for name, setup := range map[string]func(t *testing.T, f *brokerPresenceFixture, brokerID string){
		"session stale": func(t *testing.T, f *brokerPresenceFixture, brokerID string) {
			f.brokerSession(t, brokerID, "hub-p", f.registerRelay(t, "hub-p", time.Time{}), old(f))
		},
		"relay stale": func(t *testing.T, f *brokerPresenceFixture, brokerID string) {
			f.brokerSession(t, brokerID, "hub-p", f.registerRelay(t, "hub-p", old(f)), time.Time{})
		},
		"relay generation superseded": func(t *testing.T, f *brokerPresenceFixture, brokerID string) {
			gen := f.registerRelay(t, "hub-p", time.Time{})
			f.brokerSession(t, brokerID, "hub-p", gen, time.Time{})
			// A generation is the registry clock's Unix milliseconds: move
			// it before hub-p registers again.
			f.regClock.Advance(time.Millisecond)
			require.Greater(t, f.registerRelay(t, "hub-p", time.Time{}), gen, "hub-p restarted with a newer generation")
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newBrokerPresenceFixture(t)
			b := f.broker(t, "presence-not-live", store.BrokerStatusOffline, nil)
			setup(t, f, b.ID)
			assert.Equal(t, brokerPresenceResult{Source: brokerPresenceNone}, f.srv.brokerPresence(context.Background(), b))
			assert.Zero(t, f.srv.brokerPresenceRegistryFallbacks.Load())
		})
	}
}

// TestBrokerPresence_RegistryErrorFallsBack: a registry read that fails
// (a forced store error) falls back to the legacy signals, is counted
// once per read, and the count is exported as
// scion.hub.broker.presence_registry_fallbacks. With hub.conduit off the
// registry is not read and nothing is counted.
func TestBrokerPresence_RegistryErrorFallsBack(t *testing.T) {
	f := newBrokerPresenceFixture(t)
	reader := sdkmetric.NewManualReader()
	require.NoError(t, f.srv.RegisterBrokerPresenceMetrics(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))))
	online := f.broker(t, "presence-fallback-online", store.BrokerStatusOnline, nil)
	offline := f.broker(t, "presence-fallback-offline", store.BrokerStatusOffline, nil)
	f.liveBrokerSession(t, offline.ID)
	f.failReads.Store(true)
	ctx := context.Background()

	setConduitExperiment(t, f.srv, true)
	assert.Equal(t, brokerPresenceResult{Connected: true, Source: brokerPresenceLegacy}, f.srv.brokerPresence(ctx, online))
	assert.Equal(t, brokerPresenceResult{Source: brokerPresenceNone}, f.srv.brokerPresence(ctx, offline),
		"the registry session is not read, so the legacy signals decide")
	assert.Equal(t, int64(2), f.srv.brokerPresenceRegistryFallbacks.Load())
	assert.Equal(t, int64(2), brokerPresenceFallbacksExported(t, reader, brokerPresenceFallbackCauseRegistry))
	assert.Zero(t, brokerPresenceFallbacksExported(t, reader, brokerPresenceFallbackCauseRecord), "a registry fallback is not a record read error")

	setConduitExperiment(t, f.srv, false)
	f.srv.brokerPresence(ctx, online)
	assert.Equal(t, int64(2), f.srv.brokerPresenceRegistryFallbacks.Load(), "off: no registry read, nothing counted")
}

// TestBrokerPresence_CallerContextEndedNotCounted: a registry read cut short
// by the caller's own context (the registry is healthy) is not a registry
// failure: the legacy signals decide and nothing is counted.
func TestBrokerPresence_CallerContextEndedNotCounted(t *testing.T) {
	f := newBrokerPresenceFixture(t)
	setConduitExperiment(t, f.srv, true)
	online := f.broker(t, "presence-ctx-online", store.BrokerStatusOnline, nil)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	assert.Equal(t, brokerPresenceResult{Connected: true, Source: brokerPresenceLegacy}, f.srv.brokerPresence(cancelled, online))
	assert.Zero(t, f.srv.brokerPresenceRegistryFallbacks.Load())
}

// brokerPresenceFallbacksExported collects
// scion.hub.broker.presence_registry_fallbacks and returns the data point
// whose cause attribute is cause. Every cause is observed, so each export
// has exactly one data point per cause (ptone/scion#5479).
func brokerPresenceFallbacksExported(t *testing.T, reader *sdkmetric.ManualReader, cause string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != brokerPresenceFallbacksMetric {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok, "counter type %T", m.Data)
			require.Len(t, sum.DataPoints, 2, "one data point per cause")
			for _, dp := range sum.DataPoints {
				v, ok := dp.Attributes.Value(attribute.Key("cause"))
				require.True(t, ok, "data point without a cause attribute: %v", dp.Attributes)
				if v.AsString() == cause {
					return dp.Value
				}
			}
			t.Fatalf("%s: no data point with cause=%s", brokerPresenceFallbacksMetric, cause)
		}
	}
	t.Fatalf("%s not exported", brokerPresenceFallbacksMetric)
	return 0
}

// TestBrokerReachable_UsesPresence: the lifecycle reachability checks
// follow brokerPresence: a broker whose stored status is offline but which
// has a live registry session is reachable with hub.conduit on, not with it
// off.
func TestBrokerReachable_UsesPresence(t *testing.T) {
	f := newBrokerPresenceFixture(t)
	b := f.broker(t, "presence-reachable", store.BrokerStatusOffline, nil)
	f.liveBrokerSession(t, b.ID)
	agent := &store.Agent{ID: tid("presence-agent"), RuntimeBrokerID: b.ID}
	ctx := context.Background()

	setConduitExperiment(t, f.srv, true)
	assert.True(t, f.srv.brokerRecordReachable(ctx, b))
	assert.True(t, f.srv.brokerReachable(ctx, agent))

	setConduitExperiment(t, f.srv, false)
	assert.False(t, f.srv.brokerRecordReachable(ctx, b))
	assert.False(t, f.srv.brokerReachable(ctx, agent))
}

// TestConduitTunnelBrokerOffline_Presence: the tunnel's broker check refuses
// only a broker whose stored status is offline and which is not present.
// An offline status with a live registry session passes, an offline status
// with no presence is refused (503 runtime_broker_unavailable, as
// TestConduitTunnel covers end to end), and a degraded status with no
// presence passes, as before.
func TestConduitTunnelBrokerOffline_Presence(t *testing.T) {
	f := newBrokerPresenceFixture(t)
	setConduitExperiment(t, f.srv, true)
	ctx := context.Background()
	offlineLive := f.broker(t, "tunnel-offline-live", store.BrokerStatusOffline, nil)
	f.liveBrokerSession(t, offlineLive.ID)
	offline := f.broker(t, "tunnel-offline", store.BrokerStatusOffline, nil)
	degraded := f.broker(t, "tunnel-degraded", store.BrokerStatusDegraded, nil)
	for name, tc := range map[string]struct {
		brokerID string
		want     bool
	}{
		"offline status, live registry session": {offlineLive.ID, false},
		"offline status, no presence":           {offline.ID, true},
		"degraded status, no presence":          {degraded.ID, false},
	} {
		got, err := f.srv.conduitTunnelBrokerOffline(ctx, &store.Agent{RuntimeBrokerID: tc.brokerID})
		require.NoError(t, err, name)
		assert.Equal(t, tc.want, got, name)
	}
}

// TestBrokerPresent_Wrapper: the exported wrapper reports brokerPresence
// for a broker ID; a broker with no record is not present and not an error.
func TestBrokerPresent_Wrapper(t *testing.T) {
	f := newBrokerPresenceFixture(t)
	setConduitExperiment(t, f.srv, true)
	ctx := context.Background()
	offlineLive := f.broker(t, "present-offline-live", store.BrokerStatusOffline, nil)
	f.liveBrokerSession(t, offlineLive.ID)
	online := f.broker(t, "present-online", store.BrokerStatusOnline, nil)
	offline := f.broker(t, "present-offline", store.BrokerStatusOffline, nil)
	for name, tc := range map[string]struct {
		id   string
		want bool
	}{
		"offline status, live registry session": {offlineLive.ID, true},
		"online status":                         {online.ID, true},
		"offline status, no presence":           {offline.ID, false},
		"no record":                             {tid("present-missing"), false},
	} {
		got, err := f.srv.BrokerPresent(ctx, tc.id)
		require.NoError(t, err, name)
		assert.Equal(t, tc.want, got, name)
	}
}

// TestBrokerPresent_NoRegistryLegacySignalsDecide: with hub.conduit on but
// no conduit registry open, as at startup before the relay runs, the legacy
// signals decide: the stored status online is present, offline is not.
func TestBrokerPresent_NoRegistryLegacySignalsDecide(t *testing.T) {
	srv, s := testServer(t)
	setConduitExperiment(t, srv, true)
	ctx := context.Background()
	reg, _, _ := srv.conduitRegistry()
	require.Nil(t, reg, "no conduit registry is open")
	for _, tc := range []struct {
		name, status string
		want         bool
	}{
		{"startup-online", store.BrokerStatusOnline, true},
		{"startup-offline", store.BrokerStatusOffline, false},
	} {
		require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
			ID: tid(tc.name), Name: tc.name, Slug: tc.name, Status: tc.status, Created: time.Now(), Updated: time.Now(),
		}))
		got, err := srv.BrokerPresent(ctx, tid(tc.name))
		require.NoError(t, err, tc.name)
		assert.Equal(t, tc.want, got, tc.name)
	}
	assert.Zero(t, srv.brokerPresenceRegistryFallbacks.Load(), "no registry is not a registry failure")
}

// placementPresenceProject creates a project whose providers are the given
// brokers, each provider row online.
func (f *brokerPresenceFixture) placementPresenceProject(t *testing.T, name string, brokers ...*store.RuntimeBroker) *store.Project {
	t.Helper()
	ctx := context.Background()
	p := &store.Project{ID: tid(name), Slug: name, Name: name, Created: time.Now(), Updated: time.Now()}
	require.NoError(t, f.store.CreateProject(ctx, p))
	for _, b := range brokers {
		require.NoError(t, f.store.AddProjectProvider(ctx, &store.ProjectProvider{
			ProjectID: p.ID, BrokerID: b.ID, BrokerName: b.Name, Status: store.BrokerStatusOnline,
		}))
	}
	return p
}

func brokerIDsOf(brokers []store.RuntimeBroker) []string {
	ids := make([]string, 0, len(brokers))
	for _, b := range brokers {
		ids = append(ids, b.ID)
	}
	return ids
}

// TestPlacement_AvailableBrokersUsePresence: the placement set holds the
// providers whose broker is present: with hub.conduit on, a broker stored
// offline with a live registry session is in it; a broker stored offline
// with no presence is not. With hub.conduit off only the legacy signals
// count.
func TestPlacement_AvailableBrokersUsePresence(t *testing.T) {
	f := newBrokerPresenceFixture(t)
	ctx := context.Background()
	offlineLive := f.broker(t, "placement-offline-live", store.BrokerStatusOffline, nil)
	f.liveBrokerSession(t, offlineLive.ID)
	online := f.broker(t, "placement-online", store.BrokerStatusOnline, nil)
	offline := f.broker(t, "placement-offline", store.BrokerStatusOffline, nil)
	p := f.placementPresenceProject(t, "placement-presence", offlineLive, online, offline)

	setConduitExperiment(t, f.srv, true)
	got, err := f.srv.getAvailableBrokersForProject(ctx, p.ID)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{offlineLive.ID, online.ID}, brokerIDsOf(got))

	setConduitExperiment(t, f.srv, false)
	got, err = f.srv.getAvailableBrokersForProject(ctx, p.ID)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{online.ID}, brokerIDsOf(got))
}

// TestPlacement_SingleProviderAutoSelectUsesPresence: the only provider of
// a project with no default is auto-selected when its broker is present: a
// broker stored offline with a live registry session is selected with
// hub.conduit on, and refused as offline with it off.
func TestPlacement_SingleProviderAutoSelectUsesPresence(t *testing.T) {
	f := newBrokerPresenceFixture(t)
	b := f.broker(t, "placement-single", store.BrokerStatusOffline, func(b *store.RuntimeBroker) { b.AutoProvide = true })
	f.liveBrokerSession(t, b.ID)
	p := f.placementPresenceProject(t, "placement-single-project", b)
	ctx := contextWithIdentity(context.Background(), NewDevUser(DevUserConfig{
		Username: "dev", DisplayName: "Development User", Email: "dev@localhost",
	}))

	setConduitExperiment(t, f.srv, true)
	w := httptest.NewRecorder()
	got, err := f.srv.resolveRuntimeBroker(ctx, w, "", p)
	require.NoError(t, err, w.Body.String())
	assert.Equal(t, b.ID, got)

	setConduitExperiment(t, f.srv, false)
	w = httptest.NewRecorder()
	got, err = f.srv.resolveRuntimeBroker(ctx, w, "", p)
	require.Error(t, err)
	assert.Empty(t, got)
	assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
	assert.Contains(t, w.Body.String(), "only runtime broker is offline")
}

// errBrokerRecordRead is the broker record read error brokerReadFailStore
// returns.
var errBrokerRecordRead = errors.New("forced broker record read error")

// brokerReadFailStore fails every broker record read while its fault switch
// is armed: with the caller's context error once that context has ended,
// otherwise with errBrokerRecordRead. Disarmed, it delegates.
type brokerReadFailStore struct {
	store.Store
	fault *storeFaultSwitch
}

func (w *brokerReadFailStore) GetRuntimeBroker(ctx context.Context, id string) (*store.RuntimeBroker, error) {
	if !w.fault.Active() {
		return w.Store.GetRuntimeBroker(ctx, id)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, errBrokerRecordRead
}

// TestBrokerPresent_ReadErrorLoggedAndCounted: a broker record read that
// fails is returned (the caller counts the broker as present), logged at
// Warn with the broker ID, and counted once in
// scion.hub.broker.presence_registry_fallbacks. A read cut short by the
// caller's own context is returned with no log and no count.
func TestBrokerPresent_ReadErrorLoggedAndCounted(t *testing.T) {
	srv, _, _, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *brokerReadFailStore {
		return &brokerReadFailStore{Store: inner, fault: f}
	})
	reader := sdkmetric.NewManualReader()
	require.NoError(t, srv.RegisterBrokerPresenceMetrics(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))))
	fault.Arm()
	logs := captureSlog(t)
	id := tid("present-read-error")
	const line = "the broker record could not be read"

	got, err := srv.BrokerPresent(context.Background(), id)
	require.ErrorIs(t, err, errBrokerRecordRead)
	assert.False(t, got)
	assert.Contains(t, logs.String(), "level=WARN")
	assert.Contains(t, logs.String(), line)
	assert.Contains(t, logs.String(), id)
	assert.Equal(t, int64(1), srv.brokerPresenceRecordReadFallbacks.Load())
	assert.Zero(t, srv.brokerPresenceRegistryFallbacks.Load(), "a record read error is not a registry fallback")
	assert.Equal(t, int64(1), brokerPresenceFallbacksExported(t, reader, brokerPresenceFallbackCauseRecord))
	assert.Zero(t, brokerPresenceFallbacksExported(t, reader, brokerPresenceFallbackCauseRegistry))

	logs.Reset()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = srv.BrokerPresent(cancelled, id)
	require.ErrorIs(t, err, context.Canceled)
	assert.NotContains(t, logs.String(), line, "a cancelled caller context is not logged")
	assert.Equal(t, int64(1), srv.brokerPresenceRecordReadFallbacks.Load(), "a cancelled caller context is not counted")
}

// TestPlacement_OfflineProviderRowStaysOut: presence decides only the
// broker row; the provider row must still be online. A broker that is
// present, by a live registry session or by its stored status, but whose
// provider row for the project is offline is not in the placement set.
func TestPlacement_OfflineProviderRowStaysOut(t *testing.T) {
	f := newBrokerPresenceFixture(t)
	setConduitExperiment(t, f.srv, true)
	ctx := context.Background()
	liveOfflineRow := f.broker(t, "placement-row-live", store.BrokerStatusOffline, nil)
	f.liveBrokerSession(t, liveOfflineRow.ID)
	onlineOfflineRow := f.broker(t, "placement-row-online", store.BrokerStatusOnline, nil)
	onlineOnlineRow := f.broker(t, "placement-row-both", store.BrokerStatusOnline, nil)
	p := f.placementPresenceProject(t, "placement-row-project", onlineOnlineRow)
	for _, b := range []*store.RuntimeBroker{liveOfflineRow, onlineOfflineRow} {
		require.NoError(t, f.store.AddProjectProvider(ctx, &store.ProjectProvider{
			ProjectID: p.ID, BrokerID: b.ID, BrokerName: b.Name, Status: store.BrokerStatusOffline,
		}))
		require.True(t, f.srv.brokerPresence(ctx, b).Connected, "%s is present", b.Name)
	}

	got, err := f.srv.getAvailableBrokersForProject(ctx, p.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{onlineOnlineRow.ID}, brokerIDsOf(got))
}
