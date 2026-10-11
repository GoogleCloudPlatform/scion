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
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport/ws"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// brokerConduitFixture is a hub with broker HMAC authentication, hub.conduit
// on and a running in-process relay, served on an httptest server, plus one
// registered broker with its secret.
type brokerConduitFixture struct {
	srv      *Server
	store    store.Store
	public   *httptest.Server
	regStore registry.Store
	brokerID string
	secret   []byte
}

func newBrokerConduitFixture(t *testing.T, target *api.RuntimeTargetDescriptor, mod func(*ConduitRelayOptions)) *brokerConduitFixture {
	t.Helper()
	srv, s := testServerWithBrokerAuth(t)
	ctx := context.Background()
	f := &brokerConduitFixture{srv: srv, store: s, brokerID: tid("conduit-broker")}
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: f.brokerID, Name: "conduit-broker", Slug: "conduit-broker", Status: store.BrokerStatusOnline,
		RuntimeTarget: target, Created: time.Now(), Updated: time.Now(),
	}))
	key, err := srv.brokerAuthService.GenerateAndStoreSecret(ctx, f.brokerID)
	require.NoError(t, err)
	f.secret, err = base64.StdEncoding.DecodeString(key)
	require.NoError(t, err)

	srv.conduitGrants = newConduitGrantKeys(&memoryConduitGrantKeyStore{}, time.Now)
	setConduitExperiment(t, srv, true)
	f.regStore = entadapter.NewConduitRegistryStore(enttest.NewClient(t))
	peerSecret := make([]byte, 32)
	_, _ = rand.Read(peerSecret)
	auth, err := relay.NewHMACPeerAuthFromSecret(relay.HMACPeerAuthConfig{Secret: peerSecret, SelfID: "hub-a"})
	require.NoError(t, err)
	opts := ConduitRelayOptions{InstanceID: "hub-a", PeerAuth: auth, Store: f.regStore, Registry: registry.New(f.regStore, registry.Config{}), Clock: clock.NewFake(time.Now())}
	if mod != nil {
		mod(&opts)
	}
	require.NoError(t, srv.StartConduitRelay(ctx, opts))
	t.Cleanup(func() {
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.shutdownConduitRelay(sctx)
	})
	f.public = httptest.NewServer(srv.Handler())
	t.Cleanup(f.public.Close)
	return f
}

// signedHeader returns the broker upgrade headers for path, signed with
// secret (the broker's own when nil).
func (f *brokerConduitFixture) signedHeader(t *testing.T, path string, secret []byte) http.Header {
	t.Helper()
	if secret == nil {
		secret = f.secret
	}
	req, err := http.NewRequest(http.MethodGet, f.public.URL+path, nil)
	require.NoError(t, err)
	require.NoError(t, (&apiclient.HMACAuth{BrokerID: f.brokerID, SecretKey: secret}).ApplyAuth(req))
	return req.Header
}

func (f *brokerConduitFixture) wsURL(path string) string {
	return "ws" + strings.TrimPrefix(f.public.URL, "http") + path
}

// dial opens the broker's conduit session with hello's exec scope and
// incarnation.
func (f *brokerConduitFixture) dial(t *testing.T, execScope, incarnation string) (conduit.LocalSession, *conduitv1.Welcome, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	d := &ws.Dialer{
		URL:    f.wsURL("/api/v1/conduit"),
		Header: func(context.Context) (http.Header, error) { return f.signedHeader(t, "/api/v1/conduit", nil), nil },
	}
	hello := &conduitv1.Hello{
		PrincipalKind: conduitv1.PrincipalKind_PRINCIPAL_KIND_BROKER,
		PrincipalId:   f.brokerID,
		Capabilities:  &conduitv1.Capabilities{EndpointIncarnation: incarnation, ExecScope: execScope},
	}
	s, w, err := conduit.Dial(ctx, d, conduit.Config{Clock: clock.Real()}, hello)
	if err != nil {
		return nil, nil, err
	}
	ls := s.(conduit.LocalSession)
	t.Cleanup(func() { _ = ls.Close() })
	return ls, w, nil
}

func (f *brokerConduitFixture) sessions(t *testing.T) []registry.SessionView {
	t.Helper()
	ps, err := f.regStore.ListPrincipalSessions(context.Background(), registry.PrincipalBroker, f.brokerID)
	require.NoError(t, err)
	return ps.Sessions
}

// TestConduitBroker_SharedConnectVerifier: the control channel and the
// conduit endpoint accept and refuse exactly the same broker credentials.
// Each case dials both routes and requires the same handshake status, once
// through the hub's full handler and once on the bare route handlers.
// The bare run matters: in the full handler the global broker middleware
// verifies the signature before either handler runs, so only the bare run
// shows that each handler applies the shared verifier itself.
func TestConduitBroker_SharedConnectVerifier(t *testing.T) {
	f := newBrokerConduitFixture(t, nil, nil)
	wrong := make([]byte, 32)
	_, _ = rand.Read(wrong)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/runtime-brokers/connect", f.srv.handleRuntimeBrokerConnect)
	mux.HandleFunc("/api/v1/conduit", f.srv.handleConduit)
	bare := httptest.NewServer(mux)
	t.Cleanup(bare.Close)

	cases := []struct {
		name   string
		header func(path string) http.Header
		want   int
	}{
		{name: "valid signature", header: func(p string) http.Header { return f.signedHeader(t, p, nil) }, want: http.StatusSwitchingProtocols},
		{name: "wrong secret", header: func(p string) http.Header { return f.signedHeader(t, p, wrong) }, want: http.StatusUnauthorized},
		{name: "broker id without signature", header: func(string) http.Header {
			return http.Header{HeaderBrokerID: {f.brokerID}}
		}, want: http.StatusUnauthorized},
		{name: "signed for the other route", header: func(p string) http.Header {
			other := "/api/v1/conduit"
			if p == other {
				other = "/api/v1/runtime-brokers/connect"
			}
			return f.signedHeader(t, other, nil)
		}, want: http.StatusUnauthorized},
		{name: "unknown broker", header: func(p string) http.Header {
			h := f.signedHeader(t, p, nil)
			h.Set(HeaderBrokerID, tid("conduit-unknown-broker"))
			return h
		}, want: http.StatusUnauthorized},
	}
	for _, srv := range []struct {
		name string
		url  string
	}{
		{name: "full handler", url: f.public.URL},
		{name: "bare handlers", url: bare.URL},
	} {
		for _, tc := range cases {
			t.Run(srv.name+"/"+tc.name, func(t *testing.T) {
				got := map[string]int{}
				for _, path := range []string{"/api/v1/runtime-brokers/connect", "/api/v1/conduit"} {
					u := "ws" + strings.TrimPrefix(srv.url, "http") + path
					c, resp, err := websocket.DefaultDialer.Dial(u, tc.header(path))
					if c != nil {
						_ = c.Close()
					}
					require.NotNil(t, resp, "%s: %v", path, err)
					_ = resp.Body.Close()
					got[path] = resp.StatusCode
				}
				assert.Equal(t, tc.want, got["/api/v1/runtime-brokers/connect"], "control channel")
				assert.Equal(t, tc.want, got["/api/v1/conduit"], "conduit")
			})
		}
	}
}

// TestAuthenticateBrokerUpgrade_WithoutMiddlewareIdentity: the shared step
// verifies the signature itself when no broker identity is in the request
// context.
func TestAuthenticateBrokerUpgrade_WithoutMiddlewareIdentity(t *testing.T) {
	f := newBrokerConduitFixture(t, nil, nil)
	for _, tc := range []struct {
		name   string
		header http.Header
		wantOK bool
	}{
		{name: "valid", header: f.signedHeader(t, "/api/v1/conduit", nil), wantOK: true},
		{name: "no broker id", header: http.Header{}},
		{name: "bad signature", header: func() http.Header {
			h := f.signedHeader(t, "/api/v1/conduit", nil)
			h.Set("X-Scion-Signature", base64.StdEncoding.EncodeToString(make([]byte, 32)))
			return h
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/conduit", nil)
			req.Header = tc.header
			rec := httptest.NewRecorder()
			id, ok := f.srv.authenticateBrokerUpgrade(rec, req)
			assert.Equal(t, tc.wantOK, ok)
			if tc.wantOK {
				assert.Equal(t, f.brokerID, id)
			} else {
				assert.Equal(t, http.StatusUnauthorized, rec.Code)
			}
		})
	}
}

// TestConduitBroker_ExperimentOff: with hub.conduit off a signed broker
// gets the same 404 as any other caller.
func TestConduitBroker_ExperimentOff(t *testing.T) {
	f := newBrokerConduitFixture(t, nil, nil)
	setConduitExperiment(t, f.srv, false)
	_, resp, err := websocket.DefaultDialer.Dial(f.wsURL("/api/v1/conduit"), f.signedHeader(t, "/api/v1/conduit", nil))
	require.Error(t, err)
	require.NotNil(t, resp)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// TestConduitBroker_AdmittedAlongsideControlChannel: a signed broker is
// admitted and registered with its process incarnation, no stream kinds
// and no RPCs, while the router keeps resolving the broker to its
// control channel.
func TestConduitBroker_AdmittedAlongsideControlChannel(t *testing.T) {
	f := newBrokerConduitFixture(t, nil, nil)
	b := connectFakeBroker(t, f.srv, f.brokerID)

	_, wel, err := f.dial(t, "", "proc-1")
	require.NoError(t, err)
	assert.Equal(t, "proc-1", wel.GetEndpointIncarnation())
	assert.Equal(t, "hub-a", wel.GetRelayInstanceId())

	rows := f.sessions(t)
	require.Len(t, rows, 1)
	rec := rows[0].Session
	assert.Equal(t, wel.GetSessionId(), rec.SessionID)
	assert.Equal(t, "proc-1", rec.EndpointIncarnation)
	assert.Equal(t, "", rec.ExecScope)
	assert.Empty(t, rec.ProjectID)
	assert.Empty(t, rec.Capabilities.StreamKinds)
	assert.Empty(t, rec.Capabilities.RPC)

	res, err := f.srv.conduit.Load().router.Resolve(context.Background(), brokerRequest(f.brokerID), nil)
	require.NoError(t, err)
	assert.Nil(t, res.Session, "broker requests must not resolve to the conduit session")
	require.NotNil(t, res.Legacy)
	assert.Equal(t, b.sessionID, res.Record.SessionID)
	assert.Equal(t, legacyBrokerTransport, res.Record.Transport)
	assert.True(t, f.srv.controlChannel.IsConnected(f.brokerID), "the control channel is unaffected")
}

// TestConduitBroker_ExecScope: a flat Runtime Broker's sessions carry its
// runtime target as exec_scope; a Hello with another scope is refused
// 4403 and writes no row.
func TestConduitBroker_ExecScope(t *testing.T) {
	f := newBrokerConduitFixture(t, &api.RuntimeTargetDescriptor{ID: "target-1", Type: "docker"}, nil)

	_, _, err := f.dial(t, "target-2", "proc-1")
	require.Error(t, err)
	assert.Equal(t, conduit.CloseForbidden, conduit.CodeOf(err, 0), "%v", err)
	assert.Empty(t, f.sessions(t))

	_, _, err = f.dial(t, "target-1", "proc-1")
	require.NoError(t, err)
	rows := f.sessions(t)
	require.Len(t, rows, 1)
	assert.Equal(t, "target-1", rows[0].Session.ExecScope)
}

// TestConduitBroker_MissingIncarnation: a broker Hello without an endpoint
// incarnation is a protocol error (4400).
func TestConduitBroker_MissingIncarnation(t *testing.T) {
	f := newBrokerConduitFixture(t, nil, nil)
	_, _, err := f.dial(t, "", "")
	require.Error(t, err)
	assert.Equal(t, conduit.CloseProtocolError, conduit.CodeOf(err, 0), "%v", err)
}

// TestConduitBroker_RemovedDuringAdmission: a broker removed after the
// upgrade was authenticated but before its session was registered is
// closed with 4401 by the post-registration re-read.
func TestConduitBroker_RemovedDuringAdmission(t *testing.T) {
	var f *brokerConduitFixture
	f = newBrokerConduitFixture(t, nil, func(o *ConduitRelayOptions) {
		o.testHookAdmission = func() {
			assert.NoError(t, f.store.DeleteRuntimeBroker(context.Background(), f.brokerID))
		}
	})
	ls, _, err := f.dial(t, "", "proc-1")
	if err != nil {
		// The close can reach the dialer before it has read the Welcome.
		assert.Equal(t, conduit.CloseUnauthenticated, conduit.CodeOf(err, 0), "%v", err)
		return
	}
	select {
	case <-ls.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("session was not closed after the broker was removed")
	}
	assert.Equal(t, conduit.CloseUnauthenticated, conduit.CodeOf(ls.Err(), 0), "%v", ls.Err())
}

// TestConduitBroker_SessionsIndependent: closing the conduit session
// leaves the control channel up, and dropping the control channel leaves
// the conduit session up.
func TestConduitBroker_SessionsIndependent(t *testing.T) {
	f := newBrokerConduitFixture(t, nil, nil)
	b := connectFakeBroker(t, f.srv, f.brokerID)
	ls, _, err := f.dial(t, "", "proc-1")
	require.NoError(t, err)

	require.NoError(t, ls.Close())
	<-ls.Done()
	assert.True(t, f.srv.controlChannel.IsConnected(f.brokerID), "control channel survives a conduit close")

	ls2, wel2, err := f.dial(t, "", "proc-1")
	require.NoError(t, err)
	_ = b.ws.Close()
	require.Eventually(t, func() bool { return !f.srv.controlChannel.IsConnected(f.brokerID) }, 10*time.Second, 10*time.Millisecond)
	select {
	case <-ls2.Done():
		t.Fatalf("conduit session closed with the control channel: %v", ls2.Err())
	default:
	}
	_, _, ok := f.srv.conduit.Load().relay.Local(context.Background(), wel2.GetSessionId())
	assert.True(t, ok, "conduit session still registered on the relay")
}
