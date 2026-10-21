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
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/enttest"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
)

// Conduit ingress (design §3.9 "Ingress" and "Relay selection"; contracts
// "Addition (6.2 (a3))"): the hub proxies a conduit dial to a relay it
// picks, over a signed relay-peer leg, and re-picks only on failures
// before admission.

// fakeIngressRelay stands in for a relay's internal listener: it records
// each proxied dial and answers with answer.
type fakeIngressRelay struct {
	id     string
	srv    *httptest.Server
	verify relay.PeerAuth
	answer func(w http.ResponseWriter, r *http.Request)

	hits atomic.Int32
	mu   sync.Mutex
	got  []*http.Request
	peer []string
}

func (f *fakeIngressRelay) last(t *testing.T) (*http.Request, string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	require.NotEmpty(t, f.got, "relay %s received no dial", f.id)
	return f.got[len(f.got)-1], f.peer[len(f.peer)-1]
}

// ingressFixture is a hub in external relay mode with proxy ingress over
// fake relays registered in a real registry.
type ingressFixture struct {
	s      *Server
	rt     *conduitExternalRuntime
	relays map[string]*fakeIngressRelay
	st     registry.Store
}

func newIngressFixture(t *testing.T, ids ...string) *ingressFixture {
	t.Helper()
	ctx := context.Background()
	st := entadapter.NewConduitRegistryStore(enttest.NewClient(t))
	f := &ingressFixture{s: &Server{}, relays: map[string]*fakeIngressRelay{}, st: st}
	f.rt = &conduitExternalRuntime{
		registry: registry.New(st, registry.Config{}), store: st, now: time.Now,
		peerAuth: externalRelayAuth(t, "hub-a"), instanceID: "hub-a",
		ingress: "proxy", lifetimeCap: 3500 * time.Second,
		ingressTransport: newConduitIngressTransport(nil, conduitIngressResponseHeaderTimeout),
	}
	for _, id := range ids {
		fr := &fakeIngressRelay{id: id, verify: externalRelayAuth(t, id)}
		fr.answer = func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, "reached "+fr.id)
		}
		fr.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fr.hits.Add(1)
			peer, err := fr.verify.Verify(r)
			if err != nil {
				peer = "unverified: " + err.Error()
			}
			fr.mu.Lock()
			fr.got = append(fr.got, r.Clone(context.Background()))
			fr.peer = append(fr.peer, peer)
			fr.mu.Unlock()
			fr.answer(w, r)
		}))
		t.Cleanup(fr.srv.Close)
		now := time.Now()
		_, err := st.RegisterRelay(ctx, registry.RelayInstance{InstanceID: id, InternalEndpoint: fr.srv.URL, StartedAt: now, LastSeen: now})
		require.NoError(t, err)
		f.relays[id] = fr
	}
	return f
}

// preferRelays makes selection try the relays in order: each sample is the
// first of order that is still a candidate.
func (f *ingressFixture) preferRelays(order ...string) {
	f.s.conduitIngressSampleTestHook = func(candidates []registry.RelayInstance) []registry.RelayInstance {
		for _, id := range order {
			for _, ri := range candidates {
				if ri.InstanceID == id {
					return []registry.RelayInstance{ri}
				}
			}
		}
		return candidates[:1]
	}
}

// dial sends a client conduit upgrade through the ingress and returns the
// recorded answer.
func (f *ingressFixture) dial(t *testing.T, extra http.Header) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://hub.example.com/api/v1/conduit?v=1", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	req.Header.Set("Authorization", "Bearer client-credential")
	for k, v := range extra {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	f.s.serveConduitIngress(rec, req, f.rt)
	return rec
}

func writeRelayUnavailable(w http.ResponseWriter, reason string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
		"code": "unavailable", "message": "unavailable", "details": map[string]string{"reason": reason},
	}})
}

// TestConduitIngress_ProxyLeg: the proxied dial keeps the client's path,
// query, Host and credential, drops every relay-peer, ingress and
// forwarding header the client sent, carries the hub's ingress instance
// and lifetime cap, and is signed as relay-peer for the chosen relay.
func TestConduitIngress_ProxyLeg(t *testing.T) {
	f := newIngressFixture(t, "relay-0")
	junk := http.Header{}
	for _, k := range []string{
		relay.HeaderPeerID, relay.HeaderPeerTimestamp, relay.HeaderPeerNonce, relay.HeaderPeerSignature,
		relay.HeaderPeerTarget, relay.HeaderPeerTargetGeneration, relay.HeaderBodySHA256, relay.HeaderWant,
		relay.HeaderIngressInstance, relay.HeaderIngressLifetimeCap,
		"X-Conduit-Future-Peer-Header", "X-Scion-Relay-Peer-Authorization", "X-Scion-Peer-Token",
		"X-Forwarded-For", "X-Forwarded-Host", "Forwarded",
	} {
		junk.Set(k, "junk")
	}
	rec := f.dial(t, junk)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	assert.Equal(t, "reached relay-0", rec.Body.String())

	got, peer := f.relays["relay-0"].last(t)
	assert.Equal(t, "hub-a", peer, "the leg is signed as relay-peer by the hub")
	assert.Equal(t, "hub.example.com", got.Host, "Host passes unchanged")
	assert.Equal(t, "/api/v1/conduit", got.URL.Path)
	assert.Equal(t, "v=1", got.URL.RawQuery)
	assert.Equal(t, "Bearer client-credential", got.Header.Get("Authorization"), "the client's credential passes unchanged")
	assert.Equal(t, "websocket", got.Header.Get("Upgrade"))
	assert.Equal(t, "hub-a", got.Header.Get(relay.HeaderIngressInstance))
	assert.Equal(t, "3500", got.Header.Get(relay.HeaderIngressLifetimeCap))
	assert.Equal(t, "relay-0", got.Header.Get(relay.HeaderPeerTarget))
	for _, k := range []string{relay.HeaderBodySHA256, relay.HeaderWant, "X-Conduit-Future-Peer-Header",
		"X-Scion-Relay-Peer-Authorization", "X-Scion-Peer-Token", "X-Forwarded-For", "X-Forwarded-Host", "Forwarded"} {
		assert.Empty(t, got.Header.Values(k), "%s from the client must not reach the relay", k)
	}
}

// TestConduitIngress_RepicksOnlyBeforeAdmission: another relay is picked
// only after a transport failure before the request was written, 409
// relay_target_mismatch, or 503 relay_not_admitting; any other answer,
// including a 503 given after admission, a 503 not_serving or a 401,
// reaches the client unchanged, and a connection lost after the request
// was written is 502, with no second relay contacted.
func TestConduitIngress_RepicksOnlyBeforeAdmission(t *testing.T) {
	for _, tc := range []struct {
		name    string
		first   func(w http.ResponseWriter, r *http.Request)
		down    bool
		repick  bool
		status  int
		contain string
	}{
		{name: "relay not admitting", first: func(w http.ResponseWriter, _ *http.Request) {
			writeRelayUnavailable(w, relay.ReasonRelayNotAdmitting)
		}, repick: true},
		{name: "relay target mismatch", first: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set(relay.HeaderStaleReason, "relay_target_mismatch")
			http.Error(w, "stale", http.StatusConflict)
		}, repick: true},
		{name: "stopped relay on the ingress route", first: func(w http.ResponseWriter, _ *http.Request) {
			relay.WriteNotAdmitting(w) // what Relay.AuthenticateIngressPeer answers for a stopped relay
		}, repick: true},
		{name: "transport failure before any answer", down: true, repick: true},
		{name: "connection lost after the request was written", first: func(w http.ResponseWriter, _ *http.Request) {
			conn, _, err := http.NewResponseController(w).Hijack()
			if err == nil {
				_ = conn.Close()
			}
		}, status: http.StatusBadGateway, contain: "Conduit relay connection failed"},
		{name: "503 after admission", first: func(w http.ResponseWriter, _ *http.Request) {
			writeRelayUnavailable(w, relay.ReasonUpstreamUnreachable)
		}, status: http.StatusServiceUnavailable, contain: relay.ReasonUpstreamUnreachable},
		{name: "503 not_serving is not in the re-pick set", first: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set(relay.HeaderRefusalReason, relay.ReasonNotServing)
			http.Error(w, "relay unavailable", http.StatusServiceUnavailable)
		}, status: http.StatusServiceUnavailable, contain: "relay unavailable"},
		{name: "409 without the stale reason", first: func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "session id already in use", http.StatusConflict)
		}, status: http.StatusConflict, contain: "session id already in use"},
		{name: "401 from admission", first: func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "agent no longer exists", http.StatusUnauthorized)
		}, status: http.StatusUnauthorized, contain: "agent no longer exists"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newIngressFixture(t, "relay-a", "relay-b")
			f.preferRelays("relay-a", "relay-b")
			if tc.first != nil {
				f.relays["relay-a"].answer = tc.first
			}
			if tc.down {
				f.relays["relay-a"].srv.Close()
			}
			rec := f.dial(t, http.Header{"X-Scion-Broker-Id": {"broker-1"}, "X-Scion-Nonce": {"once"}})
			if tc.repick {
				require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
				assert.Equal(t, "reached relay-b", rec.Body.String())
				assert.EqualValues(t, 1, f.relays["relay-b"].hits.Load())
				return
			}
			require.Equal(t, tc.status, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), tc.contain, "the relay's answer reaches the client unchanged")
			assert.EqualValues(t, 1, f.relays["relay-a"].hits.Load())
			assert.Zero(t, f.relays["relay-b"].hits.Load(), "no second relay after admission may have run")
		})
	}
}

// TestConduitIngress_UnansweringRelayTimesOut: a relay that takes the dial
// and never answers ends in 502 once the response header bound passes, with
// no second relay contacted, since the relay may have run Admit.
func TestConduitIngress_UnansweringRelayTimesOut(t *testing.T) {
	f := newIngressFixture(t, "relay-a", "relay-b")
	f.preferRelays("relay-a", "relay-b")
	const bound = 300 * time.Millisecond
	f.rt.ingressTransport = newConduitIngressTransport(nil, bound)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	f.relays["relay-a"].answer = func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}
	start := time.Now()
	rec := f.dial(t, nil)
	elapsed := time.Since(start)
	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
	assert.GreaterOrEqual(t, elapsed, bound)
	assert.Less(t, elapsed, 10*time.Second, "the dial ends at the response header bound")
	assert.EqualValues(t, 1, f.relays["relay-a"].hits.Load())
	assert.Zero(t, f.relays["relay-b"].hits.Load(), "no second relay after the request was written")
}

// TestConduitIngressTransportBounds: the production transport has a
// response header bound above the relay's admission call bound, and keeps
// no connection for reuse.
func TestConduitIngressTransportBounds(t *testing.T) {
	tr, ok := newConduitIngressTransport(nil, conduitIngressResponseHeaderTimeout).(*http.Transport)
	require.True(t, ok)
	assert.Greater(t, tr.ResponseHeaderTimeout, relay.DefaultHubCallTimeout)
	assert.True(t, tr.DisableKeepAlives)
	assert.NotNil(t, tr.DialContext)
	assert.Positive(t, tr.TLSHandshakeTimeout)
	assert.Zero(t, http.DefaultTransport.(*http.Transport).ResponseHeaderTimeout, "the default transport is not changed")
}

// TestConduitIngress_NoRelayLeft: with no eligible relay, or every relay
// refusing before admission, the dial gets 503 conduit_not_serving.
func TestConduitIngress_NoRelayLeft(t *testing.T) {
	t.Run("no relay", func(t *testing.T) {
		rec := newIngressFixture(t).dial(t, nil)
		require.Equal(t, http.StatusServiceUnavailable, rec.Code)
		assert.Contains(t, rec.Body.String(), ptyReasonConduitNotServing)
	})
	t.Run("every relay draining", func(t *testing.T) {
		f := newIngressFixture(t, "relay-a", "relay-b")
		for _, fr := range f.relays {
			fr.answer = func(w http.ResponseWriter, _ *http.Request) { writeRelayUnavailable(w, relay.ReasonRelayNotAdmitting) }
		}
		rec := f.dial(t, nil)
		require.Equal(t, http.StatusServiceUnavailable, rec.Code)
		assert.Contains(t, rec.Body.String(), ptyReasonConduitNotServing)
		assert.EqualValues(t, 1, f.relays["relay-a"].hits.Load())
		assert.EqualValues(t, 1, f.relays["relay-b"].hits.Load())
	})
	t.Run("not an upgrade", func(t *testing.T) {
		f := newIngressFixture(t, "relay-a")
		req := httptest.NewRequest(http.MethodGet, "http://hub.example.com/api/v1/conduit", nil)
		rec := httptest.NewRecorder()
		f.s.serveConduitIngress(rec, req, f.rt)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Zero(t, f.relays["relay-a"].hits.Load())
	})
}

// ingressCountStore answers session counts from counts, or fails them.
type ingressCountStore struct {
	registry.Store
	counts map[string]int
	fail   bool
}

func (s *ingressCountStore) CountRelaySessions(_ context.Context, id string, _ int64) (int, error) {
	if s.fail {
		return 0, errors.New("injected count failure")
	}
	return s.counts[id], nil
}

// TestConduitIngress_SelectionPrefersFewerSessions: of the two sampled
// relays the one holding fewer sessions is picked; when a count fails, one
// of the two is picked and the failure is counted.
func TestConduitIngress_SelectionPrefersFewerSessions(t *testing.T) {
	both := func(f *ingressFixture) {
		f.s.conduitIngressSampleTestHook = func(c []registry.RelayInstance) []registry.RelayInstance {
			require.Len(t, c, 2)
			return c
		}
	}
	t.Run("fewer sessions wins", func(t *testing.T) {
		for _, busy := range []string{"relay-a", "relay-b"} {
			f := newIngressFixture(t, "relay-a", "relay-b")
			both(f)
			f.rt.store = &ingressCountStore{Store: f.st, counts: map[string]int{busy: 5}}
			rec := f.dial(t, nil)
			require.Equal(t, http.StatusAccepted, rec.Code)
			assert.Zero(t, f.relays[busy].hits.Load(), "the busier relay %s was picked", busy)
		}
	})
	t.Run("a failed count picks one of the two", func(t *testing.T) {
		f := newIngressFixture(t, "relay-a", "relay-b")
		both(f)
		f.rt.store = &ingressCountStore{Store: f.st, fail: true}
		rec := f.dial(t, nil)
		require.Equal(t, http.StatusAccepted, rec.Code, "the dial does not fail")
		assert.EqualValues(t, 1, f.relays["relay-a"].hits.Load()+f.relays["relay-b"].hits.Load())
		assert.EqualValues(t, 1, f.rt.ingressCountFailures.Load())
	})
}

// TestConduitAdmitArrivalRefusal: Admit binds Host to the arrival. Direct
// dials must name the relay's public host and carry no ingress instance;
// ingress dials must carry an instance and name one of the hub's public
// hosts; an unknown arrival is refused, and an absent one is direct.
func TestConduitAdmitArrivalRefusal(t *testing.T) {
	s := &Server{config: ServerConfig{HubEndpoints: &HubEndpoints{Public: "https://hub.example.com", Operator: "https://hub-ops.example.com:8443"}}}
	const relayEndpoint = "https://relay-0.example.com"
	for _, tc := range []struct {
		name     string
		arrival  string
		instance string
		host     string
		want     string
	}{
		{name: "ingress with a hub host", arrival: relay.ArrivalIngress, instance: "hub-a", host: "hub.example.com"},
		{name: "ingress with the operator host", arrival: relay.ArrivalIngress, instance: "hub-a", host: "hub-ops.example.com:8443"},
		{name: "ingress with a relay host", arrival: relay.ArrivalIngress, instance: "hub-a", host: "relay-0.example.com", want: "host_not_hub"},
		{name: "ingress without an instance", arrival: relay.ArrivalIngress, host: "hub.example.com", want: "ingress_instance_invalid"},
		{name: "direct with the relay host", arrival: relay.ArrivalDirect, host: "relay-0.example.com"},
		{name: "absent arrival is direct", host: "relay-0.example.com:443"},
		{name: "direct with a hub host", arrival: relay.ArrivalDirect, host: "hub.example.com", want: "host_not_relay"},
		{name: "direct with an instance", arrival: relay.ArrivalDirect, instance: "hub-a", host: "relay-0.example.com", want: "ingress_instance_on_direct"},
		{name: "unknown arrival", arrival: "sideways", host: "relay-0.example.com", want: "arrival_unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := s.conduitAdmitArrivalRefusal(relayEndpoint, relay.HubAdmitRequest{Host: tc.host, Arrival: tc.arrival, IngressInstance: tc.instance})
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestConduitIngress_AgentThroughHubIngress: an agent that dials the hub's
// own conduit endpoint in external relay mode is proxied to the separate
// relay and admitted there as an ingress arrival, and its port is then
// reached through another hub.
func TestConduitIngress_AgentThroughHubIngress(t *testing.T) {
	w := newSeparateRelayWorldWith(t, separateRelayOptions{noConduitAgent: true, worldMod: func(w *separateRelayWorld) {
		w.srv.config.HubEndpoints = &HubEndpoints{Public: w.base, Operator: w.base}
	}})
	w.startAgent(t)
	sessions := w.agentSessions(t)
	require.Len(t, sessions, 1)
	assert.Equal(t, "relay-0", sessions[0].RelayInstanceID, "the session is held by the separate relay")

	resp := w.getB(t, "/through-ingress")
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	assert.Equal(t, "hello /through-ingress", string(body))
}
