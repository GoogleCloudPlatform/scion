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
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dualDialBroker runs a broker's control channel and conduit dialer
// against f's hub, as the broker's HubConnection does.
type dualDialBroker struct {
	cc     *runtimebroker.ControlChannelClient
	cancel context.CancelFunc
	done   chan struct{}
}

func startDualDialBroker(t *testing.T, f *brokerConduitFixture, incarnation, execScope string) *dualDialBroker {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cc := runtimebroker.NewControlChannelClient(runtimebroker.ControlChannelConfig{
		HubEndpoint:         f.public.URL,
		BrokerID:            f.brokerID,
		SecretKey:           f.secret,
		ReconnectInitial:    10 * time.Millisecond,
		ReconnectMax:        50 * time.Millisecond,
		ReconnectMultiplier: 2,
		PingInterval:        30 * time.Second,
		PongWait:            60 * time.Second,
		WriteWait:           10 * time.Second,
	}, http.NotFoundHandler(), nil, "test", slog.Default())
	d, err := runtimebroker.NewConduitDialer(runtimebroker.ConduitDialConfig{
		HubEndpoint: f.public.URL,
		BrokerID:    f.brokerID,
		SecretKey:   f.secret,
		Incarnation: incarnation,
		ExecScope:   execScope,
		Rand:        func(int64) int64 { return 0 },
	})
	require.NoError(t, err)
	b := &dualDialBroker{cc: cc, cancel: cancel, done: make(chan struct{})}
	go func() { _ = cc.Connect(ctx) }()
	go func() {
		defer close(b.done)
		_ = d.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		_ = cc.Close()
		<-b.done
	})
	return b
}

// conduitSessionID returns the broker's only conduit session id, or "".
func (f *brokerConduitFixture) conduitSessionID(t *testing.T) string {
	rows := f.sessions(t)
	if len(rows) != 1 {
		return ""
	}
	return rows[0].Session.SessionID
}

// TestBrokerDualDial_NewHubNewBroker: the broker's dialer is admitted with
// its HMAC credentials and registered with its incarnation and exec scope
// while the control channel stays connected and keeps serving routing.
func TestBrokerDualDial_NewHubNewBroker(t *testing.T) {
	f := newBrokerConduitFixture(t, nil, nil)
	b := startDualDialBroker(t, f, "proc-e2e", "")

	require.Eventually(t, func() bool { return f.conduitSessionID(t) != "" && b.cc.IsConnected() }, 10*time.Second, 10*time.Millisecond)
	rec := f.sessions(t)[0].Session
	assert.Equal(t, "proc-e2e", rec.EndpointIncarnation)
	assert.Empty(t, rec.Capabilities.StreamKinds)
	assert.Empty(t, rec.Capabilities.RPC)
	require.Eventually(t, func() bool { return f.srv.controlChannel.IsConnected(f.brokerID) }, 10*time.Second, 10*time.Millisecond)

	res, err := f.srv.conduit.Load().router.Resolve(context.Background(), brokerRequest(f.brokerID), nil)
	require.NoError(t, err)
	assert.Nil(t, res.Session)
	require.NotNil(t, res.Legacy, "broker requests resolve to the control channel")
	assert.Equal(t, b.cc.SessionID(), res.Record.SessionID)
}

// TestBrokerDualDial_IndependentLifecycles: ending either connection
// leaves the other up, and the ended one reconnects.
func TestBrokerDualDial_IndependentLifecycles(t *testing.T) {
	f := newBrokerConduitFixture(t, nil, nil)
	b := startDualDialBroker(t, f, "proc-e2e", "")
	require.Eventually(t, func() bool {
		return f.conduitSessionID(t) != "" && b.cc.IsConnected() && f.srv.controlChannel.IsConnected(f.brokerID)
	}, 10*time.Second, 10*time.Millisecond)

	// End the conduit session (a planned relay close).
	first := f.conduitSessionID(t)
	ccSession := b.cc.SessionID()
	n := f.srv.conduit.Load().relay.CloseSessionsOf(registry.PrincipalBroker, f.brokerID, conduit.CloseRelayRestart, "relay_restart")
	require.Equal(t, 1, n)
	require.Eventually(t, func() bool {
		id := f.conduitSessionID(t)
		return id != "" && id != first
	}, 10*time.Second, 10*time.Millisecond, "conduit session reconnects")
	assert.True(t, b.cc.IsConnected(), "control channel stays up")
	assert.Equal(t, ccSession, b.cc.SessionID(), "control channel was not replaced")

	// End the control channel.
	second := f.conduitSessionID(t)
	hc := f.srv.controlChannel.GetConnection(f.brokerID)
	require.NotNil(t, hc)
	hc.Close()
	require.Eventually(t, func() bool {
		return b.cc.IsConnected() && b.cc.SessionID() != ccSession
	}, 10*time.Second, 10*time.Millisecond, "control channel reconnects")
	assert.Equal(t, second, f.conduitSessionID(t), "conduit session was not replaced")
	_, _, ok := f.srv.conduit.Load().relay.Local(context.Background(), second)
	assert.True(t, ok, "conduit session stays up")
}

// TestBrokerDualDial_NewHubOldBroker: a broker that only opens the control
// channel gets today's behaviour, and no conduit row exists for it.
func TestBrokerDualDial_NewHubOldBroker(t *testing.T) {
	f := newBrokerConduitFixture(t, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cc := runtimebroker.NewControlChannelClient(runtimebroker.ControlChannelConfig{
		HubEndpoint: f.public.URL, BrokerID: f.brokerID, SecretKey: f.secret,
		ReconnectInitial: 10 * time.Millisecond, ReconnectMax: 50 * time.Millisecond, ReconnectMultiplier: 2,
		PingInterval: 30 * time.Second, PongWait: 60 * time.Second, WriteWait: 10 * time.Second,
	}, http.NotFoundHandler(), nil, "test", slog.Default())
	go func() { _ = cc.Connect(ctx) }()
	t.Cleanup(func() { _ = cc.Close() })

	require.Eventually(t, func() bool { return cc.IsConnected() && f.srv.controlChannel.IsConnected(f.brokerID) }, 10*time.Second, 10*time.Millisecond)
	assert.Empty(t, f.sessions(t))
	res, err := f.srv.conduit.Load().router.Resolve(context.Background(), brokerRequest(f.brokerID), nil)
	require.NoError(t, err)
	require.NotNil(t, res.Legacy)
}

// TestBrokerDualDial_ExperimentOff: with hub.conduit off the broker's
// conduit probe gets 404 and waits the full re-probe cap, and the control
// channel is unaffected.
func TestBrokerDualDial_ExperimentOff(t *testing.T) {
	f := newBrokerConduitFixture(t, nil, nil)
	setConduitExperiment(t, f.srv, false)
	ends := make(chan time.Duration, 4)
	ctx, cancel := context.WithCancel(context.Background())
	d, err := runtimebroker.NewConduitDialer(runtimebroker.ConduitDialConfig{
		HubEndpoint: f.public.URL,
		BrokerID:    f.brokerID,
		SecretKey:   f.secret,
		OnEnd: func(end conduit.End, delay time.Duration, err error) {
			ends <- delay
		},
	})
	require.NoError(t, err)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = d.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	cc := runtimebroker.NewControlChannelClient(runtimebroker.ControlChannelConfig{
		HubEndpoint: f.public.URL, BrokerID: f.brokerID, SecretKey: f.secret,
		ReconnectInitial: 10 * time.Millisecond, ReconnectMax: 50 * time.Millisecond, ReconnectMultiplier: 2,
		PingInterval: 30 * time.Second, PongWait: 60 * time.Second, WriteWait: 10 * time.Second,
	}, http.NotFoundHandler(), nil, "test", slog.Default())
	go func() { _ = cc.Connect(ctx) }()
	t.Cleanup(func() { _ = cc.Close() })

	select {
	case delay := <-ends:
		assert.Equal(t, 5*time.Minute, delay, "a 404 waits the full re-probe cap")
	case <-time.After(10 * time.Second):
		t.Fatal("no probe outcome")
	}
	require.Eventually(t, func() bool { return cc.IsConnected() && f.srv.controlChannel.IsConnected(f.brokerID) }, 10*time.Second, 10*time.Millisecond)
	assert.Empty(t, f.sessions(t))
}
