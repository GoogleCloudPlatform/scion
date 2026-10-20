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
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relayrole/relayserver"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	sconduit "github.com/GoogleCloudPlatform/scion/pkg/sciontool/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// conduitAlwaysOn is a relay's hub.conduit reader that reads on.
type conduitAlwaysOn struct{}

func (conduitAlwaysOn) ConduitEnabled(context.Context) (bool, error) { return true, nil }

// separateRelayWorld is two hubs in external relay mode over one store and
// one conduit registry, and a separate relay process (relayserver)
// admitting through hub A. The launched agent's sciontool dials the relay
// directly: that direct dial is test-only (how agents are pointed at a
// relay is outside this test).
type separateRelayWorld struct {
	*conduitProxyFixture // hub A, the launched agent and its app
	hubB                 *Server
	publicB              *httptest.Server
	userTokenB           string
	relay                *relayserver.Server
	release              chan struct{} // feeds the app's event stream
	// regClient is the conduit registry database (each relay opens its own
	// store on it).
	regClient *ent.Client
	// spawnPTY, when set, makes the agent serve pty streams with it.
	spawnPTY func(context.Context, sconduit.PTYRequest) (sconduit.PTYProcess, error)

	mu          sync.Mutex
	agentOpened []uint32 // ids of the streams opened on the agent's session
}

// lastAgentStream returns the id of the last stream opened on the agent's
// session, as the agent saw its StreamOpen.
func (w *separateRelayWorld) lastAgentStream(t *testing.T) uint32 {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	require.NotEmpty(t, w.agentOpened, "no stream reached the agent")
	return w.agentOpened[len(w.agentOpened)-1]
}

func newSeparateRelayWorld(t *testing.T, mods ...func(*separateRelayWorld)) *separateRelayWorld {
	t.Helper()
	return newSeparateRelayWorldWith(t, separateRelayOptions{}, mods...)
}

// separateRelayOptions adjusts the separate relay world before it serves.
type separateRelayOptions struct {
	// enforce sets target_stream_deadlines to enforce on both hubs before
	// they start.
	enforce bool
	// agentWithoutRenewal makes the agent's Hello advertise no protocol
	// features, as a sciontool without stream renewal does.
	agentWithoutRenewal bool
}

// newSeparateRelayWorldWith is newSeparateRelayWorld with opts.
func newSeparateRelayWorldWith(t *testing.T, opts separateRelayOptions, mods ...func(*separateRelayWorld)) *separateRelayWorld {
	t.Helper()
	cf := newConduitFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	w := &separateRelayWorld{release: make(chan struct{})}

	// The app inside the agent: /events holds an event stream open until
	// the test feeds it; everything else answers hello.
	events := sseApp(w.release, 0, 1)
	app := newProxyApp(t, http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/events" {
			events.ServeHTTP(rw, r)
			return
		}
		_, _ = io.WriteString(rw, "hello "+r.URL.Path)
	}))
	launched := &store.Agent{
		ID: tid("relay-launched"), Slug: "relay-launched", Name: "Launched",
		ProjectID: cf.agent.ProjectID, OwnerID: cf.agent.OwnerID, Ancestry: cf.agent.Ancestry,
		RuntimeBrokerID: "broker-1", Phase: string(state.PhaseCreated),
	}
	require.NoError(t, cf.store.CreateAgent(ctx, launched))
	_, err := cf.store.SetAgentRunID(ctx, launched.ID, uuid.NewString(), nil)
	require.NoError(t, err)
	require.NoError(t, cf.store.UpdateAgentExposedPorts(ctx, launched.ID, []store.ExposedPort{{Port: app.port, Host: conduitProxyHost, Mode: "rw"}}))
	launched, err = cf.store.GetAgent(ctx, launched.ID)
	require.NoError(t, err)

	// Hub B shares the store, the grant key ring and the session secret.
	w.hubB, err = newTestHubServer(t, testServerConfig(), cf.store)
	require.NoError(t, err)
	w.hubB.SetHubID("test-hub-id")
	setConduitExperiment(t, w.hubB, true)
	keys := &memoryConduitGrantKeyStore{}
	cf.srv.conduitGrants = newConduitGrantKeys(keys, cf.clock.Now)
	w.hubB.conduitGrants = newConduitGrantKeys(keys, cf.clock.Now)

	// One database for the conduit registry. The hubs share one store on it;
	// each separate relay gets its own store on the same database, as a relay
	// process has its own registry access.
	w.regClient = enttest.NewClient(t)
	regStore := entadapter.NewConduitRegistryStore(w.regClient)
	replay := testReplayStore(t) // shared by both hubs, as the database is
	for id, srv := range map[string]*Server{"hub-a": cf.srv, "hub-b": w.hubB} {
		srv.config.SharedSigningSecret = testRelaySessionSecret
		if opts.enforce {
			enforceTargetDeadlines(srv)
		}
		require.NoError(t, srv.StartConduitExternal(ctx, ConduitExternalOptions{
			InstanceID: id, PeerAuth: externalRelayAuth(t, id), Store: regStore,
			// The clock also drives the stream re-check and its renewal
			// driver, so it starts at the clock grants are minted on.
			Registry: registry.New(regStore, registry.Config{}), Clock: clock.NewFake(cf.clock.Now()),
			replayStore: replay,
		}))
	}
	internalA := httptest.NewServer(cf.srv.ConduitInternalHandler())
	t.Cleanup(internalA.Close)

	publicLn, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := publicLn.Addr().String()
	w.relay, err = relayserver.Start(ctx, relayserver.Config{
		InstanceID: "relay-0", PublicURL: "http://" + addr, HubInternalURL: internalA.URL,
		PublicListener: publicLn, InternalListen: "127.0.0.1:0",
		PeerAuth: externalRelayAuth(t, "relay-0"), Registry: entadapter.NewConduitRegistryStore(w.regClient), Experiment: conduitAlwaysOn{},
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer scancel()
		_ = w.relay.Shutdown(sctx, true, time.Second)
	})

	// The target's hub URL is hub A: grant keys always come from the hub.
	publicA := httptest.NewServer(cf.srv.Handler())
	t.Cleanup(publicA.Close)

	w.conduitProxyFixture = &conduitProxyFixture{
		relayFixture: &relayFixture{conduitFixture: cf, regStore: regStore, launched: launched},
		app:          app,
		base:         publicA.URL,
		agentSession: conduit.Config{Interceptor: func(dir conduit.Direction, f *conduitv1.Frame) []*conduitv1.Frame {
			if open := f.GetStreamOpen(); dir == conduit.Inbound && open != nil {
				w.mu.Lock()
				w.agentOpened = append(w.agentOpened, open.GetStreamId())
				w.mu.Unlock()
			}
			return []*conduitv1.Frame{f}
		}},
	}
	if opts.agentWithoutRenewal {
		stripHelloFeatures(&w.agentSession)
	}
	for _, m := range mods {
		m(w)
	}
	w.startAgentDialingRelay(t, addr)

	w.publicB = httptest.NewServer(w.hubB.Handler())
	t.Cleanup(w.publicB.Close)
	w.userTokenB, _, _, err = w.hubB.userTokenService.GenerateTokenPair(
		launched.OwnerID, "owner@conduit.test", "Owner", store.UserRoleMember, ClientTypeWeb)
	require.NoError(t, err)
	return w
}

// startAgentDialingRelay starts the sciontool agent with hub A (w.base) as
// its hub URL and waits until the separate relay admits its session.
// Targets reach a separate relay through the hub conduit ingress (proxy or
// redirect). Until that ingress exists, this test dials the relay directly
// for the conduit endpoint only, while grant keys always come from the hub.
func (w *separateRelayWorld) startAgentDialingRelay(t *testing.T, relayAddr string) {
	t.Helper()
	guardSciontoolLog()
	tok := w.agentToken(t, w.launched)
	w.agentClock = clock.NewFake(w.clock.Now())
	admitted := make(chan *conduitv1.Welcome, 4)
	// The conduit dial goes to the relay; every other request (grant keys
	// among them) goes to the hub URL. Relay admission binds the request
	// Host to the relay's registered public endpoint (relayServesHost; a
	// dial naming another host is refused with 403, which
	// TestRelayAdmit_BindsRelayAndHost in conduit_relay_ops_test.go covers),
	// so a direct dial must present the relay's own host (design §3.9; a
	// dial through the hub conduit ingress presents the hub's host
	// instead). The Proxy hook sets that Host on the built upgrade request
	// and uses no proxy.
	toRelay := &websocket.Dialer{
		HandshakeTimeout: 45 * time.Second,
		NetDialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, relayAddr)
		},
		Proxy: func(req *http.Request) (*url.URL, error) {
			req.Host = relayAddr
			return nil, nil
		},
	}
	a, err := sconduit.New(sconduit.Options{
		HubURL:    w.base,
		AgentID:   w.launched.ID,
		ProjectID: w.launched.ProjectID,
		LaunchID:  w.launched.RunID,
		Token:     func() string { return tok },
		OnSession: func(wel *conduitv1.Welcome) {
			w.sessions.Add(1)
			admitted <- wel
		},
		OnEnd:    w.onEnd,
		Backoff:  &conduit.Backoff{Rand: func(int64) int64 { return 0 }},
		Clock:    fixtureClock{Fake: w.agentClock, now: w.clock.Now},
		Session:  w.agentSession,
		WS:       toRelay,
		SpawnPTY: w.spawnPTY,
	})
	require.NoError(t, err)
	w.sciontool = a
	ctx, cancel := context.WithCancel(context.Background())
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		_ = a.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			t.Error("conduit agent did not stop")
		}
	})
	select {
	case wel := <-admitted:
		require.Equal(t, w.launched.RunID, wel.GetEndpointIncarnation())
	case <-time.After(10 * time.Second):
		t.Fatal("conduit session not admitted")
	}
}

// getB sends a request through hub B's port proxy route.
func (w *separateRelayWorld) getB(t *testing.T, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet,
		w.publicB.URL+"/api/v1/agents/"+w.launched.ID+"/ports/"+strconv.Itoa(w.app.port)+"/proxy"+path, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+w.userTokenB)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// trackedOnB returns the user streams hub B re-checks.
func (w *separateRelayWorld) trackedOnB(t *testing.T) []conduitUserStream {
	t.Helper()
	a := w.hubB.conduitAuthz.Load()
	require.NotNil(t, a, "hub B runs no stream re-check")
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]conduitUserStream, 0, len(a.streams))
	for st := range a.streams {
		out = append(out, conduitUserStream{AgentID: st.AgentID, SessionID: st.SessionID, StreamID: st.StreamID})
	}
	return out
}

// TestSeparateRelay_AgentReachedThroughAnotherHub (design §3.9): an agent
// whose session a separate relay holds is reached through hub B, which
// neither admitted it nor shares a process with the relay. Hub B records
// the stream's ids on the agent's own session (from the hop's accept, not
// the hop's own stream id) and closes its leaf when the agent is deleted.
func TestSeparateRelay_AgentReachedThroughAnotherHub(t *testing.T) {
	w := newSeparateRelayWorld(t)
	sessions := w.agentSessions(t)
	require.Len(t, sessions, 1)
	require.Equal(t, "relay-0", sessions[0].RelayInstanceID, "the agent's session is on the separate relay")

	resp := w.getB(t, "/hello")
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	assert.Equal(t, "hello /hello", string(body))

	// An open stream: hub B tracks its leaf with the target-side ids.
	events := w.getB(t, "/events")
	require.Equal(t, http.StatusOK, events.StatusCode)
	tracked := w.trackedOnB(t)
	require.Len(t, tracked, 1)
	assert.Equal(t, w.launched.ID, tracked[0].AgentID)
	assert.Equal(t, sessions[0].SessionID, tracked[0].SessionID, "the agent's own session id")
	assert.Equal(t, w.lastAgentStream(t), tracked[0].StreamID, "the stream id on the agent's session, not the hop's")

	// The agent is deleted: hub B's re-check closes the leaf it holds.
	ended := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(events.Body)
		ended <- err
	}()
	require.NoError(t, w.store.DeleteAgent(context.Background(), w.launched.ID))
	w.hubB.conduitAuthz.Load().Recheck(context.Background(), "notify", conduitAuthzMatch{})
	select {
	case <-ended:
	case <-time.After(10 * time.Second):
		t.Fatal("hub B's leaf stayed open after the agent was deleted")
	}
	assert.Empty(t, w.trackedOnB(t))
}

// stripHelloFeatures makes the session's outbound Hello advertise no
// protocol features (stream renewal among them), as an older sciontool's
// does, keeping any interceptor already set.
func stripHelloFeatures(cfg *conduit.Config) {
	prev := cfg.Interceptor
	cfg.Interceptor = func(dir conduit.Direction, f *conduitv1.Frame) []*conduitv1.Frame {
		if h := f.GetHello(); dir == conduit.Outbound && h != nil && h.GetCapabilities() != nil {
			h.Capabilities.Features = nil
		}
		if prev != nil {
			return prev(dir, f)
		}
		return []*conduitv1.Frame{f}
	}
}
