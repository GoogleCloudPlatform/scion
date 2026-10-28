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
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relayrole/relayserver"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport/ws"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// startExternalForDispatch starts external relay mode on a fresh hub with
// the ingress kept off for offCause ("" serves), after registering one live
// relay when withRelay is set, and refreshes the eligible-relay count.
func startExternalForDispatch(t *testing.T, offCause string, withRelay bool) *externalFixture {
	t.Helper()
	f := &externalFixture{conduitFixture: newConduitFixture(t)}
	f.regStore = entadapter.NewConduitRegistryStore(enttest.NewClient(t))
	f.reg = registry.New(f.regStore, registry.Config{})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if withRelay {
		// Relay staleness is judged against the hub's registry clock (the
		// wall clock here), not the fixture clock.
		now := time.Now()
		_, err := f.regStore.RegisterRelay(ctx, registry.RelayInstance{InstanceID: "relay-0", InternalEndpoint: "http://127.0.0.1:1", StartedAt: now, LastSeen: now})
		require.NoError(t, err)
	}
	opts := externalOptions(t, f)
	opts.IngressOffCause = offCause
	f.srv.config.SharedSigningSecret = testRelaySessionSecret
	require.NoError(t, f.srv.StartConduitExternal(ctx, opts))
	f.srv.refreshConduitEligibleRelays(ctx, f.srv.conduitExternal.Load())
	return f
}

// TestDispatchExperiments_ExternalRelayMode: in external relay mode the hub
// tells agents to dial conduit (hub.conduit in the dispatch experiments)
// only while the ingress proxies dials and a relay is eligible.
func TestDispatchExperiments_ExternalRelayMode(t *testing.T) {
	advertised := func(f *externalFixture) bool {
		return slices.Contains(f.srv.dispatchExperiments(), conduitExperiment)
	}

	t.Run("ingress serving with an eligible relay", func(t *testing.T) {
		f := startExternalForDispatch(t, "", true)
		require.Equal(t, int64(1), f.srv.conduitEligibleRelays.Load())
		assert.True(t, advertised(f))
		assert.True(t, f.srv.conduitExternal.Load().ingressServing(), "the dial is proxied under the same condition")
	})
	t.Run("ingress off", func(t *testing.T) {
		f := startExternalForDispatch(t, ConduitIngressCauseRequiresHMAC, true)
		require.Equal(t, int64(1), f.srv.conduitEligibleRelays.Load())
		assert.False(t, advertised(f), "a relay is eligible, but the hub does not proxy dials")
		assert.False(t, f.srv.conduitExternal.Load().ingressServing())
	})
	t.Run("no eligible relay", func(t *testing.T) {
		f := startExternalForDispatch(t, "", false)
		require.Equal(t, int64(0), f.srv.conduitEligibleRelays.Load())
		assert.False(t, advertised(f))
	})
	t.Run("relay count unknown", func(t *testing.T) {
		f := startExternalForDispatch(t, "", true)
		f.srv.conduitEligibleRelays.Store(-1) // the registry could not be read
		assert.False(t, advertised(f))
	})
	t.Run("terminating", func(t *testing.T) {
		f := startExternalForDispatch(t, "", true)
		f.srv.conduitExternal.Load().terminating.Store(true)
		assert.False(t, advertised(f))
		assert.False(t, f.srv.conduitExternal.Load().ingressServing())
	})
	t.Run("experiment off", func(t *testing.T) {
		f := startExternalForDispatch(t, "", true)
		setConduitExperiment(t, f.srv, false)
		assert.False(t, advertised(f))
	})
}

// TestDispatchExperiments_InProcessUnchanged: with the in-process relay the
// dispatch advertises hub.conduit exactly when it did before (the relay runs
// and the experiment is on); external relay mode plays no part.
func TestDispatchExperiments_InProcessUnchanged(t *testing.T) {
	f := newRelayFixture(t, nil)
	require.Nil(t, f.srv.conduitExternal.Load())
	assert.True(t, slices.Contains(f.srv.dispatchExperiments(), conduitExperiment))
	snap := f.srv.experimentsSnapshot()
	assert.False(t, f.srv.conduitIngressServingIn(snap), "no external relay mode")
	setConduitExperiment(t, f.srv, false)
	assert.False(t, slices.Contains(f.srv.dispatchExperiments(), conduitExperiment))
}

// TestConduitIngress_AgentDialsAfterNoRelayPeriod: both hubs tell agents to
// dial once they count an eligible relay. When the only relay goes away
// after that (the eligible-relay count lags by up to its refresh period), an
// agent's dial through the hub ingress is refused with 503 and redialed with
// backoff; once another relay registers, the agent is admitted there on a
// later redial and its port is reached through the other hub.
func TestConduitIngress_AgentDialsAfterNoRelayPeriod(t *testing.T) {
	w := newSeparateRelayWorldWith(t, separateRelayOptions{noConduitAgent: true, worldMod: func(w *separateRelayWorld) {
		w.srv.config.HubEndpoints = &HubEndpoints{Public: w.base, Operator: w.base}
	}})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	for _, srv := range []*Server{w.srv, w.hubB} {
		require.Equal(t, 1, srv.refreshConduitEligibleRelays(ctx, srv.conduitExternal.Load()))
		assert.True(t, slices.Contains(srv.dispatchExperiments(), conduitExperiment), "agents are told to dial")
	}

	// The only relay goes away.
	sctx, scancel := context.WithTimeout(ctx, 10*time.Second)
	defer scancel()
	require.NoError(t, w.relay.Shutdown(sctx, true, time.Second))
	reg := registry.New(entadapter.NewConduitRegistryStore(w.regClient), registry.Config{})
	require.Eventually(t, func() bool {
		rs, err := reg.EligibleRelays(ctx, registry.TransportWS, time.Now())
		return err == nil && len(rs) == 0
	}, 10*time.Second, 50*time.Millisecond, "the stopped relay is no longer eligible")

	ends := make(chan conduit.End, 64)
	w.onEnd = func(end conduit.End, _ time.Duration, _ error) {
		select {
		case ends <- end:
		default:
		}
	}
	admitted := make(chan *conduitv1.Welcome, 4)
	_, result := w.runAgent(t, w.launched.RunID, admitted)
	// Drive the agent's backoff timers.
	stopTicks := make(chan struct{})
	ticked := make(chan struct{})
	go func() {
		defer close(ticked)
		tk := time.NewTicker(20 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-stopTicks:
				return
			case <-tk.C:
				w.agentClock.Advance(conduit.BackoffMax)
			}
		}
	}()
	t.Cleanup(func() {
		select {
		case <-stopTicks:
		default:
			close(stopTicks)
		}
		<-ticked
	})

	// Refused, and redialed, while no relay is eligible.
	for refused := 0; refused < 2; {
		select {
		case end := <-ends:
			var de *ws.DialError
			require.ErrorAs(t, end.DialErr, &de, "a refused dial")
			require.Equal(t, http.StatusServiceUnavailable, de.StatusCode)
			refused++
		case <-admitted:
			t.Fatal("admitted with no eligible relay")
		case err := <-result:
			t.Fatalf("the agent stopped dialing: %v", err)
		case <-time.After(10 * time.Second):
			t.Fatal("no refused dial")
		}
	}

	// Another relay registers; a later redial is admitted there.
	internalA := httptest.NewServer(w.srv.ConduitInternalHandler())
	t.Cleanup(internalA.Close)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	r1, err := relayserver.Start(ctx, relayserver.Config{
		InstanceID: "relay-1", PublicURL: "http://" + ln.Addr().String(), HubInternalURL: internalA.URL,
		PublicListener: ln, InternalListen: "127.0.0.1:0",
		PeerAuth: externalRelayAuth(t, "relay-1"), Registry: entadapter.NewConduitRegistryStore(w.regClient), Experiment: conduitAlwaysOn{},
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer scancel()
		_ = r1.Shutdown(sctx, true, time.Second)
	})
	select {
	case wel := <-admitted:
		assert.Equal(t, "relay-1", wel.GetRelayInstanceId())
	case err := <-result:
		t.Fatalf("the agent stopped dialing: %v", err)
	case <-time.After(20 * time.Second):
		t.Fatal("not admitted after a relay registered")
	}
	close(stopTicks)
	<-ticked
	sessions := w.agentSessions(t)
	require.Len(t, sessions, 1)
	assert.Equal(t, "relay-1", sessions[0].RelayInstanceID)

	resp := w.getB(t, "/after-relay-returns")
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	assert.Equal(t, "hello /after-relay-returns", string(body))
}
