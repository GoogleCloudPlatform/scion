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
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
)

// Drain through the proxy, hub side (design §3.9, v2.21.57): the shutdown's
// mark phase stops proxying; the drain step asks every live relay to drain
// the sessions this hub proxied, holds the proxied legs and closes the ones
// left at the step deadline. Every wait ends by the step deadline.

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

// TestConduitIngressDrainDeadline: the drain deadline sent is max(1ms,
// min(min(30s, grace - 2s) from the signal, H - now - 1s)), H being the
// hold deadline; with no grace it is still sent, at most H - now - 1s, and
// with 1s or less left it is the 1ms floor.
func TestConduitIngressDrainDeadline(t *testing.T) {
	t0 := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		sinceSig time.Duration // now - signal
		holdLeft time.Duration // H - now
		grace    time.Duration
		set      bool
		want     time.Duration
	}{
		{name: "grace unset: H less 1s", holdLeft: 12 * time.Second, want: 11 * time.Second},
		{name: "grace unset, far H: the 30s session drain deadline", holdLeft: 40 * time.Second, want: 30 * time.Second},
		{name: "grace 30s: H less 1s", holdLeft: 12 * time.Second, grace: 30 * time.Second, set: true, want: 11 * time.Second},
		{name: "grace 10s, H 9s away: grace - 2s", holdLeft: 9 * time.Second, grace: 10 * time.Second, set: true, want: 8 * time.Second},
		{name: "measured from the signal", sinceSig: 5 * time.Second, holdLeft: 12 * time.Second, grace: 10 * time.Second, set: true, want: 3 * time.Second},
		{name: "grace 2s: the 1ms floor", holdLeft: 2 * time.Second, grace: 2 * time.Second, set: true, want: time.Millisecond},
		{name: "G = 10s at T0: H = T0+5s gives about 4s", holdLeft: 5 * time.Second, grace: 10 * time.Second, set: true, want: 4 * time.Second},
		{name: "1s or less left: the 1ms floor", holdLeft: time.Second, grace: 10 * time.Second, set: true, want: time.Millisecond},
		{name: "H passed: the 1ms floor", holdLeft: -time.Second, want: time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := t0.Add(tc.sinceSig)
			got := conduitIngressDrainDeadline(now, t0, now.Add(tc.holdLeft), tc.grace, tc.set)
			assert.Equal(t, tc.want, got)
			if tc.holdLeft > 2*time.Second {
				assert.LessOrEqual(t, got, tc.holdLeft-time.Second, "never more than H - now - 1s")
			}
		})
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

// TestDrainConduitIngressForShutdown_FanOut: the drain step calls drain on
// every live relay (draining included, stale excluded) as this hub's
// instance, with the clamped deadline; each failure is counted by cause
// (refused, bad_response, unreachable, timeout), and a slow relay holds
// the step only until the step deadline (fake clock). A second call does
// nothing.
func TestDrainConduitIngressForShutdown_FanOut(t *testing.T) {
	f := newIngressFixture(t, "relay-ok", "relay-refused", "relay-bad", "relay-slow", "relay-stale")
	f.s.conduitExternal.Store(f.rt)
	okURL, err := url.Parse(f.relays["relay-ok"].srv.URL)
	require.NoError(t, err)
	okRead := &bufferingTransport{base: http.DefaultTransport, host: okURL.Host, done: make(chan struct{})}
	f.rt.peers = &relay.PeerClient{HTTP: &http.Client{Transport: okRead}, Auth: f.rt.peerAuth}
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
	slowEntered := make(chan struct{}, 1)
	f.relays["relay-slow"].answer = func(_ http.ResponseWriter, r *http.Request) {
		slowEntered <- struct{}{}
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

	t0 := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	clk := clock.NewFake(t0)
	opts := ConduitIngressDrainOptions{Clock: clk, SignalAt: t0, HoldDeadline: t0.Add(time.Second), Grace: 2 * time.Second, GraceSet: true}
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		f.s.DrainConduitIngressForShutdown(opts) // deadline sent: 1ms
	}()
	require.True(t, clk.WaitForTimer(10*time.Second, t0.Add(time.Second)), "the step deadline is armed")
	select {
	case <-slowEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("the slow relay was not called")
	}
	require.Eventually(t, func() bool {
		select {
		case <-okRead.done:
		default:
			return false
		}
		c := drainFailedCounts(t, reader)
		return c[relay.DrainFailedRefused] == 1 && c[relay.DrainFailedBadResponse] == 1 && c[relay.DrainFailedUnreachable] == 1
	}, 10*time.Second, 10*time.Millisecond, "the other calls answered")
	select {
	case <-returned:
		t.Fatal("a slow relay must hold the fan-out until the step deadline")
	default:
	}
	clk.Advance(time.Second)
	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("the fan-out did not end at the step deadline")
	}
	assert.True(t, f.rt.terminating.Load(), "proxying has stopped")

	mu.Lock()
	require.Len(t, bodies, 1)
	assert.Equal(t, "hub-a", bodies[0].IngressInstance, "the hub drains as its own instance")
	assert.EqualValues(t, 1, bodies[0].DrainDeadlineMs, "grace 2s asks for 1ms, never the relay default")
	mu.Unlock()
	_, peer := f.relays["relay-ok"].last(t)
	assert.Equal(t, "hub-a", peer, "the call is signed as relay-peer by the hub")
	assert.Zero(t, f.relays["relay-stale"].hits.Load(), "a stale relay is not asked")
	want := map[string]int64{
		relay.DrainFailedRefused:     1,
		relay.DrainFailedBadResponse: 1,
		relay.DrainFailedTimeout:     1,
		relay.DrainFailedUnreachable: 1,
	}
	// The slow call records its timeout just after the step deadline ends
	// the wait for it.
	require.Eventually(t, func() bool { return assert.ObjectsAreEqual(want, drainFailedCounts(t, reader)) }, 10*time.Second, 10*time.Millisecond)

	hits := f.relays["relay-ok"].hits.Load()
	f.s.DrainConduitIngressForShutdown(opts)
	assert.Equal(t, hits, f.relays["relay-ok"].hits.Load(), "a second drain does nothing")
}

// bufferingTransport reads the whole answer of host into memory before
// returning it and then closes done, so the caller's decode no longer
// depends on its context.
type bufferingTransport struct {
	base http.RoundTripper
	host string
	done chan struct{}
}

func (b *bufferingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := b.base.RoundTrip(req)
	if err != nil || req.URL.Host != b.host {
		return resp, err
	}
	data, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(data))
	close(b.done)
	return resp, nil
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

// TestDrainConduitIngressForShutdown_ProxyOffAsksNoRelay: with the ingress
// proxy off (the hub never proxied a session) the proxy is not active, so
// cmd registers no step; a drain call still stops proxying but asks no
// relay to drain.
func TestDrainConduitIngressForShutdown_ProxyOffAsksNoRelay(t *testing.T) {
	f := newIngressFixture(t, "relay-a")
	f.s.conduitExternal.Store(f.rt)
	f.rt.peers = &relay.PeerClient{HTTP: http.DefaultClient, Auth: f.rt.peerAuth}
	assert.True(t, f.s.ConduitIngressProxyActive(), "proxy ingress serving")
	f.rt.ingressOffCause = ConduitIngressCauseRequiresHMAC
	assert.False(t, f.s.ConduitIngressProxyActive(), "the proxy is off")
	assert.False(t, (&Server{}).ConduitIngressProxyActive(), "not in external relay mode")
	now := time.Now()
	f.s.DrainConduitIngressForShutdown(ConduitIngressDrainOptions{SignalAt: now, HoldDeadline: now.Add(30 * time.Second), Grace: 30 * time.Second, GraceSet: true})
	assert.True(t, f.rt.terminating.Load())
	assert.Zero(t, f.relays["relay-a"].hits.Load(), "no relay is asked to drain")
}

// TestConduitIngress_DialBetweenMarkAndDrain: the shutdown stops proxying
// (BeginConduitIngressShutdown, the step before the marks), then marks the
// hub (BeginShutdown); the drain step has not run yet. In that window,
// behind the hub's shutdown refusal, a broker-signed dial reaches the
// conduit handler and gets 503 conduit_not_serving with the terminating
// message (cause ingress_terminating) and is not proxied; a user dial, a
// broker dial on behalf of a user and a dial with no broker identity (an
// agent) get the generic shutdown 503. No relay is contacted.
func TestConduitIngress_DialBetweenMarkAndDrain(t *testing.T) {
	f := newIngressFixture(t, "relay-a")
	srv, _ := testServer(t)
	srv.conduitExternal.Store(f.rt)
	setConduitExperiment(t, srv, true)
	srv.BeginConduitIngressShutdown()
	srv.BeginShutdown()
	assert.True(t, f.rt.terminating.Load(), "proxying stopped before the mark")
	assert.False(t, f.rt.drainStarted.Load(), "the drain step has not run")
	h := srv.shutdownMark.refuseDuringShutdown(http.HandlerFunc(srv.handleConduit), isBrokerRequest)
	upgrade := func(ctx context.Context) *http.Request {
		req := httptest.NewRequest(http.MethodGet, "http://hub.example.com/api/v1/conduit", nil)
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "websocket")
		return req.WithContext(ctx)
	}
	user := NewAuthenticatedUser("u1", "user@example.com", "User", "member", "cli")
	withUser := func(ctx context.Context) context.Context {
		return context.WithValue(contextWithIdentity(ctx, user), userContextKey{}, user)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, upgrade(contextWithBrokerIdentity(context.Background(), NewBrokerIdentity("b1"))))
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ptyReasonConduitNotServing, "a broker dial reaches the conduit handler")
	assert.Contains(t, rec.Body.String(), conduitExternalTerminating)

	for name, ctx := range map[string]context.Context{
		"user":                     withUser(context.Background()),
		"broker on behalf of user": withUser(contextWithBrokerIdentity(context.Background(), NewBrokerIdentity("b1"))),
		"no broker identity":       context.Background(),
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, upgrade(ctx))
		require.Equal(t, http.StatusServiceUnavailable, rec.Code, "%s: %s", name, rec.Body.String())
		assert.Equal(t, "10", rec.Header().Get("Retry-After"), "%s gets the generic shutdown 503", name)
		assert.NotContains(t, rec.Body.String(), ptyReasonConduitNotServing, name)
	}
	assert.Zero(t, f.relays["relay-a"].hits.Load(), "no relay is contacted after the mark")
	assert.Zero(t, f.rt.legs.count(), "no proxied leg is open")
}

// TestConduitIngress_NoLegAfterTerminatingAtSend: a dial whose relay was
// picked before the mark phase and is about to be sent after it never
// reaches the relay: it gets 503 conduit_not_serving.
func TestConduitIngress_NoLegAfterTerminatingAtSend(t *testing.T) {
	f := newIngressFixture(t, "relay-a")
	f.s.conduitIngressSampleTestHook = func(candidates []registry.RelayInstance) []registry.RelayInstance {
		f.rt.terminating.Store(true) // the mark phase runs while the relay is picked
		return candidates[:1]
	}
	rec := f.dial(t, nil)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ptyReasonConduitNotServing)
	assert.Zero(t, f.relays["relay-a"].hits.Load(), "no byte reaches the relay")
	assert.Zero(t, f.rt.legs.count(), "the dial's leg count ended")
}

// upgradedLeg is a conduit dial proxied through a hub ingress server to a
// relay that switched protocols and holds the connection.
type upgradedLeg struct {
	client      net.Conn
	relayClosed chan struct{} // closed when the relay side sees the leg end
}

// openUpgradedLeg makes relay id of f answer 101 and hold the connection,
// then dials the hub ingress over a real listener and reads the 101.
func openUpgradedLeg(t *testing.T, f *ingressFixture, hubURL, id string) *upgradedLeg {
	t.Helper()
	leg := &upgradedLeg{relayClosed: make(chan struct{})}
	f.relays[id].answer = func(w http.ResponseWriter, _ *http.Request) {
		conn, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		_ = brw.Flush()
		_, _ = io.Copy(io.Discard, brw) // until the leg ends
		close(leg.relayClosed)
	}
	u, err := url.Parse(hubURL)
	require.NoError(t, err)
	conn, err := net.Dial("tcp", u.Host)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	_, err = io.WriteString(conn, "GET /api/v1/conduit?v=1 HTTP/1.1\r\nHost: hub.example.com\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n")
	require.NoError(t, err)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
	leg.client = conn
	return leg
}

// TestDrainConduitIngressForShutdown_ReturnsEarlyWhenLastLegCloses: after
// the fan-out the step holds while a proxied leg is open and returns as
// soon as the last one closes, well before the step deadline (the fake
// clock never moves): the step deadline is only the backstop.
func TestDrainConduitIngressForShutdown_ReturnsEarlyWhenLastLegCloses(t *testing.T) {
	f := newIngressFixture(t, "relay-a")
	f.s.conduitExternal.Store(f.rt)
	f.rt.peers = &relay.PeerClient{HTTP: http.DefaultClient, Auth: f.rt.peerAuth}
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.s.serveConduitIngress(w, r, f.rt)
	}))
	t.Cleanup(hub.Close)
	leg := openUpgradedLeg(t, f, hub.URL, "relay-a")
	require.Equal(t, 1, f.rt.legs.count(), "one open proxied leg")
	f.relays["relay-a"].answer = func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(relay.DrainResponse{GoAwaySent: 1})
	}

	t0 := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	clk := clock.NewFake(t0)
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		f.s.DrainConduitIngressForShutdown(ConduitIngressDrainOptions{Clock: clk, SignalAt: t0, HoldDeadline: t0.Add(12 * time.Second)})
	}()
	require.True(t, clk.WaitForTimer(10*time.Second, t0.Add(12*time.Second)), "the step deadline is armed")
	require.Eventually(t, func() bool { return f.relays["relay-a"].hits.Load() == 2 }, 10*time.Second, 10*time.Millisecond, "the relay was asked to drain")
	select {
	case <-returned:
		t.Fatal("the step returned while a proxied leg was open")
	default:
	}
	select {
	case <-leg.relayClosed:
		t.Fatal("the leg was closed before it ended on its own")
	default:
	}
	assert.Equal(t, 1, f.rt.legs.count(), "the leg is still open during the hold")
	_ = leg.client.Close() // the client moved after its GoAway
	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("the step did not return when the last leg closed")
	}
	assert.Equal(t, t0, clk.Now(), "returned before the step deadline, with the clock not moved")
	select {
	case <-leg.relayClosed:
	case <-time.After(10 * time.Second):
		t.Fatal("the relay side of the leg did not end")
	}
	assert.Zero(t, f.rt.legs.count())
}

// TestDrainConduitIngressForShutdown_ClosesLegsAtDeadline: a proxied leg
// still open at the step deadline is closed by the hub (both sides see it
// end), and the step returns; the live-relay read and the fan-out ended by
// the same deadline. With no grace the deadline sent is step deadline - 1s.
func TestDrainConduitIngressForShutdown_ClosesLegsAtDeadline(t *testing.T) {
	f := newIngressFixture(t, "relay-a")
	f.s.conduitExternal.Store(f.rt)
	f.rt.peers = &relay.PeerClient{HTTP: http.DefaultClient, Auth: f.rt.peerAuth}
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.s.serveConduitIngress(w, r, f.rt)
	}))
	t.Cleanup(hub.Close)
	leg := openUpgradedLeg(t, f, hub.URL, "relay-a")
	sent := make(chan relay.DrainRequest, 1)
	f.relays["relay-a"].answer = func(w http.ResponseWriter, r *http.Request) {
		var dr relay.DrainRequest
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &dr)
		sent <- dr
		_ = json.NewEncoder(w).Encode(relay.DrainResponse{GoAwaySent: 0}) // a relay that did not drain it
	}

	t0 := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	clk := clock.NewFake(t0)
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		f.s.DrainConduitIngressForShutdown(ConduitIngressDrainOptions{Clock: clk, SignalAt: t0, HoldDeadline: t0.Add(12 * time.Second)})
	}()
	select {
	case dr := <-sent:
		assert.EqualValues(t, 11000, dr.DrainDeadlineMs, "grace unset: sent, step deadline - now - 1s")
	case <-time.After(10 * time.Second):
		t.Fatal("the relay was not asked to drain")
	}
	require.True(t, clk.WaitForTimer(10*time.Second, t0.Add(12*time.Second)), "the step deadline is armed")
	require.Eventually(t, func() bool { return f.relays["relay-a"].hits.Load() == 2 }, 10*time.Second, 10*time.Millisecond, "the drain call was answered")
	select {
	case <-returned:
		t.Fatal("the step returned before its deadline with a leg open")
	default:
	}
	select {
	case <-leg.relayClosed:
		t.Fatal("the leg was closed before the step deadline")
	default:
	}
	assert.Equal(t, 1, f.rt.legs.count(), "the leg is still open until the step deadline")
	clk.Advance(12 * time.Second)
	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("the step did not return at its deadline")
	}
	select {
	case <-leg.relayClosed:
	case <-time.After(10 * time.Second):
		t.Fatal("the relay side of the leg was not closed")
	}
	require.NoError(t, leg.client.SetReadDeadline(time.Now().Add(10*time.Second)))
	_, err := leg.client.Read(make([]byte, 1))
	assert.ErrorIs(t, err, io.EOF, "the client side of the leg was closed")
	require.Eventually(t, func() bool { return f.rt.legs.count() == 0 }, 10*time.Second, 10*time.Millisecond)
}

// TestDrainConduitIngressForShutdown_HoldDeadlinePassed: with the hold
// deadline already past at step start there is no hold and no fan-out: the
// open legs are closed and the step returns at once.
func TestDrainConduitIngressForShutdown_HoldDeadlinePassed(t *testing.T) {
	f := newIngressFixture(t, "relay-a")
	f.s.conduitExternal.Store(f.rt)
	f.rt.peers = &relay.PeerClient{HTTP: http.DefaultClient, Auth: f.rt.peerAuth}
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.s.serveConduitIngress(w, r, f.rt)
	}))
	t.Cleanup(hub.Close)
	leg := openUpgradedLeg(t, f, hub.URL, "relay-a")
	require.EqualValues(t, 1, f.relays["relay-a"].hits.Load())

	t0 := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	clk := clock.NewFake(t0.Add(6 * time.Second))
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		f.s.DrainConduitIngressForShutdown(ConduitIngressDrainOptions{Clock: clk, SignalAt: t0, HoldDeadline: t0.Add(5 * time.Second), Grace: 10 * time.Second, GraceSet: true})
	}()
	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("the step did not return at once with the hold deadline past")
	}
	select {
	case <-leg.relayClosed:
	case <-time.After(10 * time.Second):
		t.Fatal("the open leg was not closed")
	}
	assert.EqualValues(t, 1, f.relays["relay-a"].hits.Load(), "no drain call with the hold deadline past")
	assert.Zero(t, clk.Pending(), "no timer was armed")
}

// TestConduitIngressLegs: the leg set counts, reports idle when empty and
// closes every open leg.
func TestConduitIngressLegs(t *testing.T) {
	var l conduitIngressLegs
	select {
	case <-l.idle():
	default:
		t.Fatal("an empty set is idle")
	}
	closed := 0
	a := l.add(func() { closed++ })
	b := l.add(func() { closed++ })
	assert.Equal(t, 2, l.count())
	idle := l.idle()
	l.done(a)
	select {
	case <-idle:
		t.Fatal("idle with a leg open")
	default:
	}
	assert.Equal(t, 1, l.closeAll())
	assert.Equal(t, 1, closed)
	l.done(b)
	select {
	case <-idle:
	default:
		t.Fatal("not idle after the last leg ended")
	}
	assert.Zero(t, l.count())
}
