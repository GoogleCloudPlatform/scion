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
	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
