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

package runtimebroker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newCommandPathTestClient returns a control channel client that never
// dials, on a clock the test moves.
func newCommandPathTestClient(now *time.Time) *ControlChannelClient {
	cc := newLoggedCloseTestClient("http://127.0.0.1:1", newLevelRecorder())
	cc.now = func() time.Time { return *now }
	cc.disconnectedSince = *now
	return cc
}

// markCommandPathConnected records a completed handshake, as doConnect does.
func markCommandPathConnected(cc *ControlChannelClient) {
	cc.mu.Lock()
	cc.connected = true
	cc.disconnectedSince = time.Time{}
	cc.transportRefusal = nil
	cc.mu.Unlock()
}

// addTestHubConnection registers hc on srv for the test.
func addTestHubConnection(t *testing.T, srv *Server, hc *HubConnection) {
	t.Helper()
	srv.hubMu.Lock()
	if srv.hubConnections == nil {
		srv.hubConnections = map[string]*HubConnection{}
	}
	srv.hubConnections[hc.Name] = hc
	srv.hubMu.Unlock()
	t.Cleanup(func() {
		srv.hubMu.Lock()
		delete(srv.hubConnections, hc.Name)
		srv.hubMu.Unlock()
	})
}

// The control channel's command path is healthy while connected, unknown
// while starting or reconnecting within the grace, and unhealthy past the
// grace or while the hub's transport policy refuses it.
func TestCommandPathState(t *testing.T) {
	now := time.Now()
	cc := newCommandPathTestClient(&now)
	assert.Equal(t, api.BrokerHealthUnknown, cc.CommandPathState(), "new, within the grace")

	now = now.Add(commandPathGrace + time.Second)
	assert.Equal(t, api.BrokerHealthUnhealthy, cc.CommandPathState(), "never connected, past the grace")

	markCommandPathConnected(cc)
	assert.Equal(t, api.BrokerHealthHealthy, cc.CommandPathState(), "connected")

	cc.markDisconnected()
	assert.Equal(t, api.BrokerHealthUnknown, cc.CommandPathState(), "dropped just now: reconnecting")
	now = now.Add(commandPathGrace - time.Second)
	assert.Equal(t, api.BrokerHealthUnknown, cc.CommandPathState(), "still within the grace")
	now = now.Add(2 * time.Second)
	assert.Equal(t, api.BrokerHealthUnhealthy, cc.CommandPathState(), "dropped and not back past the grace")

	// A second drop report while already disconnected keeps the first time.
	cc.markDisconnected()
	assert.Equal(t, api.BrokerHealthUnhealthy, cc.CommandPathState(), "a repeated drop does not restart the grace")

	fresh := newCommandPathTestClient(&now)
	fresh.setTransportRefusal(parseHubRefusal(http.StatusForbidden, []byte(newHubTransportRefusal)))
	assert.Equal(t, api.BrokerHealthUnhealthy, fresh.CommandPathState(), "a transport refusal is unhealthy at once")
	markCommandPathConnected(fresh)
	assert.Equal(t, api.BrokerHealthHealthy, fresh.CommandPathState(), "a handshake clears the refusal")
}

// The conduit reports unhealthy while refused, healthy with a session, and
// nothing otherwise (it is optional).
func TestConduitCommandPathState(t *testing.T) {
	d := newRefusalTestDialer(t, &syncBuffer{})
	assert.Equal(t, "", d.CommandPathState(), "no session, not refused")
	d.sessionUp.Store(true)
	assert.Equal(t, api.BrokerHealthHealthy, d.CommandPathState())
	d.sessionUp.Store(false)
	d.fallback(preUpgrade(http.StatusForbidden, newHubTransportRefusal))
	assert.Equal(t, api.BrokerHealthUnhealthy, d.CommandPathState())
	d.fallback(preUpgrade(http.StatusNotFound, `{}`))
	assert.Equal(t, "", d.CommandPathState(), "a hub without conduit is not a failure")
}

// GetHealthInfo reports the command path: nothing without a control
// channel, nothing inside the grace (starting or reconnecting is not a
// failure), unhealthy and degraded past it, and the worst value across
// connections.
func TestHealthInfo_CommandPath(t *testing.T) {
	ctx := context.Background()
	srv := newTestServer(t)

	info := srv.GetHealthInfo(ctx)
	assert.NotContains(t, info.Checks, healthCheckControlChannel, "no hub connection")
	addTestHubConnection(t, srv, &HubConnection{Name: "no-channel"})
	info = srv.GetHealthInfo(ctx)
	assert.NotContains(t, info.Checks, healthCheckControlChannel, "a connection without a control channel")
	assert.Equal(t, "healthy", info.Status)

	now := time.Now()
	a := newCommandPathTestClient(&now)
	markCommandPathConnected(a)
	addTestHubConnection(t, srv, &HubConnection{Name: "hub-a", ControlChannel: a})
	info = srv.GetHealthInfo(ctx)
	assert.Equal(t, api.BrokerHealthHealthy, info.Checks[healthCheckControlChannel])
	assert.Equal(t, "healthy", info.Status)

	b := newCommandPathTestClient(&now)
	addTestHubConnection(t, srv, &HubConnection{Name: "hub-b", ControlChannel: b})
	info = srv.GetHealthInfo(ctx)
	assert.Equal(t, api.BrokerHealthHealthy, info.Checks[healthCheckControlChannel], "a channel within its grace is not reported")
	assert.Equal(t, "healthy", info.Status, "a channel within its grace does not degrade")

	lone := newTestServer(t)
	addTestHubConnection(t, lone, &HubConnection{Name: "hub-starting", ControlChannel: newCommandPathTestClient(&now)})
	info = lone.GetHealthInfo(ctx)
	assert.NotContains(t, info.Checks, healthCheckControlChannel, "only a channel within its grace: nothing reported")
	assert.Equal(t, "healthy", info.Status)

	now = now.Add(commandPathGrace + time.Second)
	info = srv.GetHealthInfo(ctx)
	assert.Equal(t, api.BrokerHealthUnhealthy, info.Checks[healthCheckControlChannel])
	assert.Equal(t, "degraded", info.Status)
}

// Each hub connection's heartbeat reports only its own control channel and
// conduit: a hub whose channel is connected is never told that the broker
// cannot receive commands because another hub's channel is down. The
// broker's own /healthz keeps the worst value.
func TestHeartbeatHealth_CommandPathPerConnection(t *testing.T) {
	ctx := context.Background()
	srv := newTestServer(t)
	now := time.Now()

	good := newCommandPathTestClient(&now)
	markCommandPathConnected(good)
	goodConduit := newRefusalTestDialer(t, &syncBuffer{})
	goodConduit.sessionUp.Store(true)
	hubA := &HubConnection{Name: "hub-a", ControlChannel: good, conduitDialer: goodConduit}

	bad := newCommandPathTestClient(&now)
	bad.setTransportRefusal(parseHubRefusal(http.StatusForbidden, []byte(newHubTransportRefusal)))
	badConduit := newRefusalTestDialer(t, &syncBuffer{})
	badConduit.transportRefused.Store(true)
	hubB := &HubConnection{Name: "hub-b", ControlChannel: bad, conduitDialer: badConduit}

	addTestHubConnection(t, srv, hubA)
	addTestHubConnection(t, srv, hubB)

	a := sendConnectionHealthHeartbeat(t, srv, hubA)
	require.NotNil(t, a.Health)
	assert.Equal(t, api.BrokerHealthHealthy, a.Health.Checks[healthCheckControlChannel], "hub A gets its own channel")
	assert.Equal(t, api.BrokerHealthHealthy, a.Health.Checks[healthCheckConduit])
	assert.Equal(t, api.BrokerHealthHealthy, a.Health.Status, "hub B's refusal does not degrade hub A's report")

	b := sendConnectionHealthHeartbeat(t, srv, hubB)
	require.NotNil(t, b.Health)
	assert.Equal(t, api.BrokerHealthUnhealthy, b.Health.Checks[healthCheckControlChannel], "hub B gets its own channel")
	assert.Equal(t, api.BrokerHealthUnhealthy, b.Health.Checks[healthCheckConduit])
	assert.Equal(t, api.BrokerHealthDegraded, b.Health.Status)

	none := srv.heartbeatHealthReport(ctx)
	require.NotNil(t, none)
	assert.NotContains(t, none.Checks, healthCheckControlChannel, "no connection: no command path")

	local := srv.GetHealthInfo(ctx)
	assert.Equal(t, api.BrokerHealthUnhealthy, local.Checks[healthCheckControlChannel], "/healthz keeps the worst value")
	assert.Equal(t, "degraded", local.Status)
}

// The broker's hub-connections API reports each connection's command path
// with fixed words and booleans.
func TestHandleHubConnections_CommandPath(t *testing.T) {
	creds := makeTestCreds("local", "broker-1", "http://localhost:8080")
	srv := newTestServerWithInMemoryCreds(creds)

	get := func() HubConnectionInfo {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/hub-connections", nil)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var resp HubConnectionStatusResponse
		require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
		require.Len(t, resp.Connections, 1)
		return resp.Connections[0]
	}

	got := get()
	assert.Equal(t, "", got.CommandPath, "no control channel: nothing reported")
	assert.False(t, got.TransportRefused)

	srv.hubMu.RLock()
	var hc *HubConnection
	for _, c := range srv.hubConnections {
		hc = c
	}
	srv.hubMu.RUnlock()
	require.NotNil(t, hc)

	now := time.Now()
	cc := newCommandPathTestClient(&now)
	cc.setTransportRefusal(parseHubRefusal(http.StatusForbidden, []byte(newHubTransportRefusal)))
	hc.mu.Lock()
	hc.ControlChannel = cc
	hc.mu.Unlock()
	got = get()
	assert.True(t, got.HasControlChannel)
	assert.Equal(t, api.BrokerHealthUnhealthy, got.CommandPath)
	assert.True(t, got.TransportRefused)

	markCommandPathConnected(cc)
	got = get()
	assert.Equal(t, api.BrokerHealthHealthy, got.CommandPath)
	assert.False(t, got.TransportRefused)

	hc.mu.Lock()
	hc.ControlChannel = nil
	hc.mu.Unlock()
}

// A hub connection with a control channel is not reported connected until
// the channel's handshake completes: against a hub that does not serve the
// control channel it stays connecting. Without a control channel, the
// started connection is connected.
func TestHubConnectionStart_StatusWaitsForHandshake(t *testing.T) {
	t.Setenv(transportauth.EnvTransportMode, "")
	t.Setenv(transportauth.EnvTransportAudience, "")

	for _, tc := range []struct {
		name    string
		channel bool
		want    ConnectionStatus
	}{
		{name: "control channel", channel: true, want: ConnectionStatusConnecting},
		{name: "no control channel", channel: false, want: ConnectionStatusConnected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub, _ := newLifecycleTestHub(t)
			cfg := DefaultServerConfig()
			cfg.BrokerID = "broker-1"
			cfg.BrokerName = "test-host"
			cfg.HeartbeatEnabled = false
			cfg.ControlChannelEnabled = tc.channel
			cfg.BrokerAuthEnabled = false
			srv := New(cfg, &mockManager{}, &runtime.MockRuntime{NameFunc: func() string { return "docker" }}, config.RuntimeBuildRecord{})

			conn, err := srv.createHubConnection("hub-status", makeTestCreds("hub-status", "broker-1", hub.URL))
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			require.NoError(t, conn.Start(ctx, srv))
			defer conn.Stop()

			assert.Equal(t, tc.want, conn.GetStatus())
			// The refused dials keep it connecting, never connected.
			time.Sleep(200 * time.Millisecond)
			assert.Equal(t, tc.want, conn.GetStatus())
		})
	}
}

// The command path values are a closed set of fixed words in every state,
// never error text: what the hub-connections API and the heartbeat carry.
func TestCommandPathState_ClosedSet(t *testing.T) {
	closed := map[string]bool{api.BrokerHealthHealthy: true, api.BrokerHealthUnhealthy: true, api.BrokerHealthUnknown: true}
	now := time.Now()
	cc := newCommandPathTestClient(&now)
	seen := map[string]bool{}
	check := func() {
		t.Helper()
		v := cc.CommandPathState()
		assert.True(t, closed[v], "control channel value %q", v)
		seen[v] = true
	}
	check()
	cc.setTransportRefusal(parseHubRefusal(http.StatusForbidden, []byte(newHubTransportRefusal)))
	check()
	cc.setTransportRefusal(parseHubRefusal(http.StatusBadGateway, []byte(`<html>proxy error at /var/run/x</html>`)))
	check()
	now = now.Add(commandPathGrace + time.Second)
	check()
	markCommandPathConnected(cc)
	check()
	assert.Len(t, seen, 3, "every value was produced")

	d := newRefusalTestDialer(t, &syncBuffer{})
	for _, pre := range []*conduitPreUpgradeError{
		preUpgrade(http.StatusForbidden, newHubTransportRefusal),
		preUpgrade(http.StatusBadGateway, `<html>proxy error at /var/run/x</html>`),
		preUpgrade(http.StatusNotFound, `{}`),
	} {
		d.fallback(pre)
		v := d.CommandPathState()
		assert.True(t, v == "" || closed[v], "conduit value %q", v)
	}
}

// A hub connection becomes connected when its control channel's handshake
// completes, and connected again after the channel drops and re-handshakes.
// A control channel whose Connect gives up (a hub URL the transport rule
// refuses) leaves the connection in error.
func TestHubConnectionStart_StatusTransitions(t *testing.T) {
	t.Setenv(transportauth.EnvTransportMode, "")
	t.Setenv(transportauth.EnvTransportAudience, "")

	newServer := func() *Server {
		cfg := DefaultServerConfig()
		cfg.BrokerID = "broker-1"
		cfg.BrokerName = "test-host"
		cfg.HeartbeatEnabled = false
		cfg.ControlChannelEnabled = true
		cfg.BrokerAuthEnabled = false
		return New(cfg, &mockManager{}, &runtime.MockRuntime{NameFunc: func() string { return "docker" }}, config.RuntimeBuildRecord{})
	}

	t.Run("handshake", func(t *testing.T) {
		hub := newFakeControlHub(t)
		// Serve only the control channel, so the conduit dialer gets a
		// plain 404 (the hub serves no conduit).
		front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/runtime-brokers/connect" {
				hub.srv.Config.Handler.ServeHTTP(w, r)
				return
			}
			http.NotFound(w, r)
		}))
		t.Cleanup(front.Close)

		srv := newServer()
		conn, err := srv.createHubConnection("hub-ok", makeTestCreds("hub-ok", "broker-1", front.URL))
		require.NoError(t, err)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		require.NoError(t, conn.Start(ctx, srv))
		defer conn.Stop()

		waitFor(t, 5*time.Second, "connected after the handshake", func() bool { return conn.GetStatus() == ConnectionStatusConnected })
		assert.Equal(t, api.BrokerHealthHealthy, conn.snapshot().ControlChannel.CommandPathState())

		hub.closeAll()
		waitFor(t, 10*time.Second, "a second handshake", func() bool { return hub.connects.Load() >= 2 })
		waitFor(t, 5*time.Second, "connected again", func() bool { return conn.GetStatus() == ConnectionStatusConnected })
	})

	t.Run("connect gives up", func(t *testing.T) {
		srv := newServer()
		conn, err := srv.createHubConnection("hub-refused", makeTestCreds("hub-refused", "broker-1", "http://127.0.0.1:1"))
		require.NoError(t, err)
		// Plain http to a host that is not loopback: the transport rule
		// refuses it, so Connect returns at once with an error.
		// createHubConnection refuses such a URL itself, so the test sets
		// it afterwards to reach Connect's own check.
		conn.mu.Lock()
		conn.HubEndpoint = "http://hub.example.com"
		conn.mu.Unlock()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		require.NoError(t, conn.Start(ctx, srv))
		defer conn.Stop()

		waitFor(t, 5*time.Second, "error after Connect gives up", func() bool { return conn.GetStatus() == ConnectionStatusError })
	})
}

// The heartbeats a started hub connection sends carry that connection's own
// control_channel check: HubConnection.Start wires the per-connection
// health into its heartbeat service. Without that wiring the heartbeat
// falls back to heartbeatHealthReport, which reports no command path, and
// this test fails.
func TestHubConnectionStart_HeartbeatCarriesOwnCommandPath(t *testing.T) {
	t.Setenv(transportauth.EnvTransportMode, "")
	t.Setenv(transportauth.EnvTransportAudience, "")

	control := newFakeControlHub(t)
	var mu sync.Mutex
	// sawHealthy: some heartbeat the hub received carried control_channel
	// healthy. (An initial heartbeat built before the handshake may land
	// late without it, so the last one is not the test.)
	sawHealthy := false
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/runtime-brokers/connect":
			control.srv.Config.Handler.ServeHTTP(w, r)
		case strings.HasSuffix(r.URL.Path, "/heartbeat"):
			var hb hubclient.BrokerHeartbeat
			if err := json.NewDecoder(r.Body).Decode(&hb); err == nil {
				mu.Lock()
				if hb.Health != nil && hb.Health.Checks[healthCheckControlChannel] == api.BrokerHealthHealthy {
					sawHealthy = true
				}
				mu.Unlock()
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("{}"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(front.Close)

	cfg := DefaultServerConfig()
	cfg.BrokerID = "broker-1"
	cfg.BrokerName = "test-host"
	cfg.HeartbeatEnabled = true
	// Below MinHeartbeatInterval, so it is raised to 5s; the test forces a
	// heartbeat instead of waiting for a tick.
	cfg.HeartbeatInterval = 20 * time.Millisecond
	cfg.ControlChannelEnabled = true
	cfg.BrokerAuthEnabled = false
	srv := New(cfg, &mockManager{}, &runtime.MockRuntime{NameFunc: func() string { return "docker" }}, config.RuntimeBuildRecord{})

	conn, err := srv.createHubConnection("hub-hb", makeTestCreds("hub-hb", "broker-1", front.URL))
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, conn.Start(ctx, srv))
	defer conn.Stop()

	waitFor(t, 5*time.Second, "connected after the handshake", func() bool { return conn.GetStatus() == ConnectionStatusConnected })

	// The initial heartbeat may have gone out before the handshake, and the
	// next tick is MinHeartbeatInterval (5s) away, so send one now through
	// the service Start built: it uses the health Start wired in.
	hb := conn.snapshot().Heartbeat
	require.NotNil(t, hb)
	require.NoError(t, hb.ForceHeartbeat(ctx))
	mu.Lock()
	saw := sawHealthy
	mu.Unlock()
	assert.True(t, saw, "no heartbeat the hub received carried control_channel healthy")

	// The service's health, read directly, reports the same.
	require.NotNil(t, hb.health)
	report := hb.health(ctx)
	require.NotNil(t, report)
	assert.Equal(t, api.BrokerHealthHealthy, report.Checks[healthCheckControlChannel])
}
