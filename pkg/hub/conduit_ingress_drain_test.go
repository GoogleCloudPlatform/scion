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
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
)

// Drain through the proxy, hub side (design §3.9, v2.21.53): on SIGTERM the
// hub stops proxying, then asks every live relay to drain the sessions it
// proxied, bounded and best effort.

// TestConduitIngress_TerminatingRefusesBeforeAnyRelay: once the hub is
// terminating, a conduit dial gets 503 conduit_not_serving and no relay is
// contacted.
func TestConduitIngress_TerminatingRefusesBeforeAnyRelay(t *testing.T) {
	f := newIngressFixture(t, "relay-a")
	f.s.conduitExternal.Store(f.rt)
	f.rt.terminating.Store(true)
	setConduitExperiment(t, f.s, true)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://hub.example.com/api/v1/conduit", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	f.s.handleConduit(rec, req)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ptyReasonConduitNotServing)
	assert.Contains(t, rec.Body.String(), "shutting down")
	assert.Zero(t, f.relays["relay-a"].hits.Load(), "no relay is contacted after SIGTERM")
}

// TestConduitIngress_NoRepickAfterTerminating: a dial whose first relay
// refused before admission is not re-picked once the hub is terminating.
func TestConduitIngress_NoRepickAfterTerminating(t *testing.T) {
	f := newIngressFixture(t, "relay-a", "relay-b")
	f.preferRelays("relay-a", "relay-b")
	f.relays["relay-a"].answer = func(w http.ResponseWriter, _ *http.Request) {
		f.rt.terminating.Store(true)
		writeRelayUnavailable(w, relay.ReasonRelayNotAdmitting)
	}
	rec := f.dial(t, nil)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ptyReasonConduitNotServing)
	assert.Zero(t, f.relays["relay-b"].hits.Load(), "no second relay after SIGTERM")
}

// TestConduitIngressDrainBoundAndDeadline: the fan-out bound is
// max(grace - 1s, grace/2), or 10s with no grace; the drain deadline asked
// for is max(grace - 2s, 1ms), or none (the relay's own) with no grace.
func TestConduitIngressDrainBoundAndDeadline(t *testing.T) {
	for _, tc := range []struct {
		grace        time.Duration
		set          bool
		wantBound    time.Duration
		wantDeadline time.Duration
	}{
		{set: false, wantBound: 10 * time.Second, wantDeadline: 0},
		{grace: 30 * time.Second, set: true, wantBound: 29 * time.Second, wantDeadline: 28 * time.Second},
		{grace: 2 * time.Second, set: true, wantBound: time.Second, wantDeadline: time.Millisecond},
		{grace: time.Second, set: true, wantBound: 500 * time.Millisecond, wantDeadline: time.Millisecond},
		{grace: 500 * time.Millisecond, set: true, wantBound: 250 * time.Millisecond, wantDeadline: time.Millisecond},
	} {
		assert.Equal(t, tc.wantBound, conduitIngressDrainBound(tc.grace, tc.set), "bound for grace %s set=%v", tc.grace, tc.set)
		assert.Equal(t, tc.wantDeadline, conduitIngressDrainDeadline(tc.grace, tc.set), "deadline for grace %s set=%v", tc.grace, tc.set)
	}
}

// drainFailedCounts collects scion.hub.conduit.ingress_drain_failed by cause.
func drainFailedCounts(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "scion.hub.conduit.ingress_drain_failed" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok, "counter type %T", m.Data)
			for _, dp := range sum.DataPoints {
				cause, _ := dp.Attributes.Value("cause")
				out[cause.AsString()] += dp.Value
			}
		}
	}
	return out
}

// TestDrainConduitIngressForShutdown_FanOut: SIGTERM stops proxying, then
// calls drain on every live relay (draining included, stale excluded) as
// this hub's instance, with the clamped deadline; each failure is counted
// by cause (refused, bad_response, unreachable, timeout) and none holds
// shutdown past the bound. A second call does nothing.
func TestDrainConduitIngressForShutdown_FanOut(t *testing.T) {
	f := newIngressFixture(t, "relay-ok", "relay-refused", "relay-bad", "relay-slow", "relay-stale")
	f.s.conduitExternal.Store(f.rt)
	f.rt.peers = &relay.PeerClient{HTTP: http.DefaultClient, Auth: f.rt.peerAuth}
	reader := sdkmetric.NewManualReader()
	m, err := NewOTelConduitIngressDrainMetrics(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	require.NoError(t, err)
	f.s.SetConduitIngressDrainMetrics(m)
	ctx := context.Background()

	var mu sync.Mutex
	var bodies []relay.DrainRequest
	f.relays["relay-ok"].answer = func(w http.ResponseWriter, r *http.Request) {
		var dr relay.DrainRequest
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &dr)
		mu.Lock()
		bodies = append(bodies, dr)
		mu.Unlock()
		assert.Equal(t, relay.InternalPathPrefix+"drain", r.URL.Path)
		assert.Equal(t, http.MethodPost, r.Method)
		assert.NotEmpty(t, r.Header.Get(relay.HeaderBodySHA256), "the body is bound to the signature")
		_ = json.NewEncoder(w).Encode(relay.DrainResponse{GoAwaySent: 2})
	}
	f.relays["relay-refused"].answer = func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "relay unavailable", http.StatusServiceUnavailable)
	}
	f.relays["relay-bad"].answer = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "not json")
	}
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	f.relays["relay-slow"].answer = func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}
	// relay-ok is draining (still asked); relay-stale was last seen long ago
	// (never asked); relay-gone is registered at a closed endpoint.
	okRow := relayRow(t, f, "relay-ok")
	require.NoError(t, f.rt.registry.SetRelayDraining(ctx, "relay-ok", okRow.Generation, true))
	staleRow := relayRow(t, f, "relay-stale")
	staleRow.LastSeen = time.Now().Add(-time.Hour)
	_, err = f.st.RegisterRelay(ctx, staleRow)
	require.NoError(t, err)
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	_, err = f.st.RegisterRelay(ctx, registry.RelayInstance{InstanceID: "relay-gone", InternalEndpoint: gone.URL, StartedAt: time.Now(), LastSeen: time.Now()})
	require.NoError(t, err)

	start := time.Now()
	f.s.DrainConduitIngressForShutdown(2*time.Second, true) // bound 1s, deadline 1ms
	elapsed := time.Since(start)
	assert.True(t, f.rt.terminating.Load(), "proxying stops first")
	assert.Less(t, elapsed, 5*time.Second, "the fan-out is bounded (1s)")

	mu.Lock()
	require.Len(t, bodies, 1)
	assert.Equal(t, "hub-a", bodies[0].IngressInstance, "the hub drains as its own instance")
	assert.EqualValues(t, 1, bodies[0].DrainDeadlineMs, "grace 2s asks for 1ms, never the relay default")
	mu.Unlock()
	_, peer := f.relays["relay-ok"].last(t)
	assert.Equal(t, "hub-a", peer, "the call is signed as relay-peer by the hub")
	assert.Zero(t, f.relays["relay-stale"].hits.Load(), "a stale relay is not asked")
	assert.Equal(t, map[string]int64{
		relay.DrainFailedRefused:     1,
		relay.DrainFailedBadResponse: 1,
		relay.DrainFailedTimeout:     1,
		relay.DrainFailedUnreachable: 1,
	}, drainFailedCounts(t, reader))

	hits := f.relays["relay-ok"].hits.Load()
	f.s.DrainConduitIngressForShutdown(2*time.Second, true)
	assert.Equal(t, hits, f.relays["relay-ok"].hits.Load(), "a second SIGTERM drain does nothing")
}

// relayRow reads the registered row of a fixture relay.
func relayRow(t *testing.T, f *ingressFixture, id string) registry.RelayInstance {
	t.Helper()
	all, err := f.st.ListRelays(context.Background())
	require.NoError(t, err)
	for _, ri := range all {
		if ri.InstanceID == id {
			return ri
		}
	}
	t.Fatalf("relay %s is not registered", id)
	return registry.RelayInstance{}
}
