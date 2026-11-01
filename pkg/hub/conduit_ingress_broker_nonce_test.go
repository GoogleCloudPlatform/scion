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
	"encoding/base64"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport/ws"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// brokerNonceReasonDeferred is the audit detail of a broker conduit dial
// whose nonce is left to Admit.
const brokerNonceReasonDeferred = "broker_nonce_deferred_to_admit"

// brokerNonceAuditRecorder records broker auth events; it is safe for the
// concurrent requests of a test hub.
type brokerNonceAuditRecorder struct {
	mockAuditLogger
	mu     sync.Mutex
	events []BrokerAuthEvent
}

func (r *brokerNonceAuditRecorder) LogBrokerAuthEvent(_ context.Context, e *BrokerAuthEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, *e)
	return nil
}

// forBroker returns the recorded events of brokerID, in order.
func (r *brokerNonceAuditRecorder) forBroker(brokerID string) []BrokerAuthEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []BrokerAuthEvent
	for _, e := range r.events {
		if e.BrokerID == brokerID {
			out = append(out, e)
		}
	}
	return out
}

// addNonceTestBroker registers an online broker with a fresh secret.
func addNonceTestBroker(t *testing.T, s store.Store, svc *BrokerAuthService, name string) (string, []byte) {
	t.Helper()
	ctx := context.Background()
	id := tid(name)
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: id, Name: name, Slug: name, Status: store.BrokerStatusOnline, Created: time.Now(), Updated: time.Now(),
	}))
	key, err := svc.GenerateAndStoreSecret(ctx, id)
	require.NoError(t, err)
	secret, err := base64.StdEncoding.DecodeString(key)
	require.NoError(t, err)
	return id, secret
}

// brokerSignedHeader returns the headers of a broker request for method and
// rawURL, signed the way a Runtime Broker signs (apiclient.HMACAuth).
func brokerSignedHeader(t *testing.T, method, rawURL, brokerID string, secret []byte) http.Header {
	t.Helper()
	req, err := http.NewRequest(method, rawURL, nil)
	require.NoError(t, err)
	require.NoError(t, (&apiclient.HMACAuth{BrokerID: brokerID, SecretKey: secret}).ApplyAuth(req))
	return req.Header
}

// nonceRecorded reports whether nonce is already in the shared nonce store
// (it records the nonce when it was not).
func nonceRecorded(t *testing.T, nonces *NonceCacheStore, nonce string) bool {
	t.Helper()
	require.NotEmpty(t, nonce)
	fresh, err := nonces.CheckAndStore(context.Background(), nonce, time.Hour)
	require.NoError(t, err)
	return !fresh
}

// dialBrokerConduit opens a broker conduit session at u with the handshake
// headers h (used as they are on every attempt).
func dialBrokerConduit(t *testing.T, u string, wd *websocket.Dialer, h http.Header, brokerID string) (*conduitv1.Welcome, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	d := &ws.Dialer{URL: u, WS: wd, Header: func(context.Context) (http.Header, error) { return h.Clone(), nil }}
	hello := &conduitv1.Hello{
		PrincipalKind: conduitv1.PrincipalKind_PRINCIPAL_KIND_BROKER,
		PrincipalId:   brokerID,
		Capabilities:  &conduitv1.Capabilities{EndpointIncarnation: "proc-1"},
	}
	s, wel, err := conduit.Dial(ctx, d, conduit.Config{Clock: clock.Real()}, hello)
	if err != nil {
		return nil, err
	}
	ls := s.(conduit.LocalSession)
	t.Cleanup(func() { _ = ls.Close() })
	return wel, nil
}

// brokerIngressWorld is the separate relay world with broker HMAC auth on
// both hubs, one shared database nonce store, and a registered broker. Both
// hubs serve one public host (hub A's), so a dial that lands on hub B is
// proxied by hub B's ingress and admitted by hub A.
type brokerIngressWorld struct {
	*separateRelayWorld
	nonces   *NonceCacheStore
	audit    map[*Server]*brokerNonceAuditRecorder
	brokerID string
	secret   []byte
}

func newBrokerIngressWorld(t *testing.T, withAudit bool) *brokerIngressWorld {
	t.Helper()
	bw := &brokerIngressWorld{nonces: NewNonceCacheStore(enttest.NewClient(t)), audit: map[*Server]*brokerNonceAuditRecorder{}}
	bw.separateRelayWorld = newSeparateRelayWorldWith(t, separateRelayOptions{
		noConduitAgent: true,
		srvMod: func(srv *Server) {
			// The broker auth service New built (broker auth is on in the
			// default config), with the deferral it wires in; only its
			// nonce store is replaced by the database store both hubs
			// share. Shutdown closes the service.
			require.NotNil(t, srv.brokerAuthService, "test hubs run broker auth")
			require.NotNil(t, srv.brokerAuthService.deferNonce, "New wires the conduit ingress deferral")
			srv.brokerAuthService.SetNonceCacheStore(bw.nonces)
			if withAudit {
				rec := &brokerNonceAuditRecorder{}
				bw.audit[srv] = rec
				srv.SetAuditLogger(rec)
			} else {
				srv.SetAuditLogger(nil)
			}
		},
		worldMod: func(w *separateRelayWorld) {
			w.srv.config.HubEndpoints = &HubEndpoints{Public: w.base, Operator: w.base}
		},
	})
	bw.brokerID, bw.secret = addNonceTestBroker(t, bw.store, bw.hubB.brokerAuthService, "ingress-nonce-broker")
	return bw
}

// signedDial returns the headers of a broker conduit dial to hub A's host.
func (bw *brokerIngressWorld) signedDial(t *testing.T) http.Header {
	t.Helper()
	return brokerSignedHeader(t, http.MethodGet, bw.base+"/api/v1/conduit", bw.brokerID, bw.secret)
}

// dialThroughHubB dials hub A's host on hub B's listener: hub B's conduit
// ingress proxies the dial to the separate relay, which asks hub A to Admit.
func (bw *brokerIngressWorld) dialThroughHubB(t *testing.T, h http.Header) (*conduitv1.Welcome, error) {
	t.Helper()
	hostB := strings.TrimPrefix(bw.publicB.URL, "http://")
	wd := &websocket.Dialer{
		HandshakeTimeout: 30 * time.Second,
		ReadBufferSize:   wsprotocol.DefaultReadBufferSize,
		WriteBufferSize:  wsprotocol.DefaultWriteBufferSize,
		NetDialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, hostB)
		},
	}
	return dialBrokerConduit(t, "ws"+strings.TrimPrefix(bw.base, "http")+"/api/v1/conduit", wd, h, bw.brokerID)
}

func (bw *brokerIngressWorld) brokerSessions(t *testing.T) []registry.SessionView {
	t.Helper()
	ps, err := bw.regStore.ListPrincipalSessions(context.Background(), registry.PrincipalBroker, bw.brokerID)
	require.NoError(t, err)
	return ps.Sessions
}

// TestConduitIngress_BrokerDialThroughHubIngress: a broker's conduit dial
// through a hub's conduit ingress, in a deployment of two hubs sharing the
// database nonce store, is admitted once by the separate relay. The ingress
// hop verifies the broker's signature and leaves the nonce to Admit, which
// records it. Run with the audited broker middleware (an audit logger is
// configured, as in production) and with the plain one.
func TestConduitIngress_BrokerDialThroughHubIngress(t *testing.T) {
	for _, withAudit := range []bool{true, false} {
		t.Run("audit="+strconv.FormatBool(withAudit), func(t *testing.T) {
			bw := newBrokerIngressWorld(t, withAudit)
			h := bw.signedDial(t)
			wel, err := bw.dialThroughHubB(t, h)
			require.NoError(t, err, "the broker dial through the ingress is admitted")
			assert.Equal(t, "relay-0", wel.GetRelayInstanceId(), "the session is held by the separate relay")
			require.Len(t, bw.brokerSessions(t), 1)
			assert.True(t, nonceRecorded(t, bw.nonces, h.Get(HeaderNonce)), "Admit records the nonce")

			if !withAudit {
				return
			}
			ingress := bw.audit[bw.hubB].forBroker(bw.brokerID)
			require.Len(t, ingress, 1, "the ingress hop authenticates the broker once")
			assert.Equal(t, BrokerAuthEventAuthSuccess, ingress[0].EventType)
			assert.Equal(t, brokerNonceReasonDeferred, ingress[0].Details["nonce_reason"])
			assert.Empty(t, ingress[0].FailureCode)
			admit := bw.audit[bw.srv].forBroker(bw.brokerID)
			require.Len(t, admit, 1, "Admit authenticates the broker once")
			assert.Equal(t, BrokerAuthEventAuthSuccess, admit[0].EventType)
			assert.NotContains(t, admit[0].Details, "nonce_reason", "Admit records the nonce")
		})
	}
}

// TestConduitIngress_BrokerReplayRefusedAtAdmit: replaying a broker's
// conduit dial through the ingress is refused at Admit, where the nonce is
// recorded; the ingress hop passes it on unrecorded both times.
func TestConduitIngress_BrokerReplayRefusedAtAdmit(t *testing.T) {
	bw := newBrokerIngressWorld(t, true)
	h := bw.signedDial(t)
	_, err := bw.dialThroughHubB(t, h)
	require.NoError(t, err)
	_, err = bw.dialThroughHubB(t, h)
	require.Error(t, err, "the replayed dial is refused")
	assert.Len(t, bw.brokerSessions(t), 1, "the replay admits no session")

	ingress := bw.audit[bw.hubB].forBroker(bw.brokerID)
	require.Len(t, ingress, 2)
	for _, e := range ingress {
		assert.Equal(t, BrokerAuthEventAuthSuccess, e.EventType)
		assert.Equal(t, brokerNonceReasonDeferred, e.Details["nonce_reason"])
	}
	admit := bw.audit[bw.srv].forBroker(bw.brokerID)
	require.Len(t, admit, 2)
	assert.Equal(t, BrokerAuthEventAuthSuccess, admit[0].EventType)
	assert.Equal(t, BrokerAuthEventAuthFailure, admit[1].EventType)
	assert.Equal(t, brokerAuthFailureCode(errBrokerAuthReplay), admit[1].FailureCode, "Admit refuses the replay")
}

// TestValidateBrokerSignature_ForgedSignatureKeepsNonce: a request with a
// legitimate nonce and a bad signature is refused without recording the
// nonce, so the legitimate request still succeeds (once), with the
// in-memory and the database nonce store.
func TestValidateBrokerSignature_ForgedSignatureKeepsNonce(t *testing.T) {
	for _, db := range []bool{false, true} {
		t.Run("db="+strconv.FormatBool(db), func(t *testing.T) {
			svc, s := setupTestBrokerAuthService(t)
			if db {
				svc.SetNonceCacheStore(NewNonceCacheStore(enttest.NewClient(t)))
			}
			_, signReq := setupSignedBrokerRequest(t, svc, s)
			ctx := context.Background()
			legit := signReq(http.MethodGet, "/api/v1/conduit", nil)
			forged := legit.Clone(ctx)
			forged.Header.Set(HeaderSignature, base64.StdEncoding.EncodeToString(make([]byte, 32)))

			_, err := svc.ValidateBrokerSignature(ctx, forged)
			require.ErrorIs(t, err, errBrokerAuthInvalidSignature)
			_, err = svc.ValidateBrokerSignature(ctx, legit)
			require.NoError(t, err, "the forged request left the nonce unused")
			_, err = svc.ValidateBrokerSignature(ctx, legit)
			require.ErrorIs(t, err, errBrokerAuthReplay)
		})
	}
}

// TestValidateBrokerSignature_DeferredNonceVerifiesEverything: a request
// whose nonce is deferred passes every check except recording: missing
// headers, clock skew, unknown brokers and bad signatures are refused as
// before, and only a verified request passes, its nonce unrecorded.
func TestValidateBrokerSignature_DeferredNonceVerifiesEverything(t *testing.T) {
	svc, s := setupTestBrokerAuthService(t)
	nonces := NewNonceCacheStore(enttest.NewClient(t))
	svc.SetNonceCacheStore(nonces)
	svc.deferNonce = func(*http.Request) bool { return true }
	_, signReq := setupSignedBrokerRequest(t, svc, s)
	ctx := context.Background()

	refusals := []struct {
		name string
		mod  func(h http.Header)
		want error
	}{
		{name: "no nonce", mod: func(h http.Header) { h.Del(HeaderNonce) }, want: errBrokerAuthMissingNonce},
		{name: "no timestamp", mod: func(h http.Header) { h.Del(HeaderTimestamp) }, want: errBrokerAuthMissingTimestamp},
		{name: "no signature", mod: func(h http.Header) { h.Del(HeaderSignature) }, want: errBrokerAuthMissingSignature},
		{name: "clock skew", mod: func(h http.Header) {
			h.Set(HeaderTimestamp, strconv.FormatInt(time.Now().Add(-time.Hour).Unix(), 10))
		}, want: errBrokerAuthClockSkew},
		{name: "unknown broker", mod: func(h http.Header) { h.Set(HeaderBrokerID, tid("deferred-unknown-broker")) }, want: errBrokerAuthUnknownBroker},
		{name: "bad signature", mod: func(h http.Header) {
			h.Set(HeaderSignature, base64.StdEncoding.EncodeToString(make([]byte, 32)))
		}, want: errBrokerAuthInvalidSignature},
	}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			req := signReq(http.MethodGet, "/api/v1/conduit", nil)
			tc.mod(req.Header)
			_, err := svc.ValidateBrokerSignature(ctx, req)
			require.Error(t, err)
			if tc.want == errBrokerAuthUnknownBroker {
				// admitBroker may refuse an unknown broker before the
				// secret lookup does.
				return
			}
			assert.ErrorIs(t, err, tc.want)
		})
	}

	legit := signReq(http.MethodGet, "/api/v1/conduit", nil)
	for i := 0; i < 2; i++ {
		_, err := svc.ValidateBrokerSignature(ctx, legit)
		require.NoError(t, err, "a deferred request is verified, not recorded")
	}
	assert.False(t, nonceRecorded(t, nonces, legit.Header.Get(HeaderNonce)), "the deferred nonce is not recorded")

	svc.deferNonce = func(*http.Request) bool { return false }
	legit = signReq(http.MethodGet, "/api/v1/conduit", nil)
	_, err := svc.ValidateBrokerSignature(ctx, legit)
	require.NoError(t, err)
	_, err = svc.ValidateBrokerSignature(ctx, legit)
	require.ErrorIs(t, err, errBrokerAuthReplay, "without deferral the nonce is recorded")
}

// startBrokerAuthExternal starts external relay mode on a hub New built with
// broker auth (testServerWithBrokerAuth), over its own registry, with the
// shared database nonce store nonces. The hub has no separate relay.
func startBrokerAuthExternal(t *testing.T, ingressOff bool, nonces *NonceCacheStore) (*Server, store.Store, *httptest.Server) {
	t.Helper()
	srv, s := testServerWithBrokerAuth(t)
	require.NotNil(t, srv.brokerAuthService)
	srv.brokerAuthService.SetNonceCacheStore(nonces)
	srv.conduitGrants = newConduitGrantKeys(&memoryConduitGrantKeyStore{}, time.Now)
	setConduitExperiment(t, srv, true)
	f := &externalFixture{conduitFixture: &conduitFixture{srv: srv, store: s, clock: &conduitTestClock{now: time.Now()}}}
	f.regStore = entadapter.NewConduitRegistryStore(enttest.NewClient(t))
	f.reg = registry.New(f.regStore, registry.Config{})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv.config.SharedSigningSecret = testRelaySessionSecret
	opts := externalOptions(t, f)
	if ingressOff {
		opts.IngressOffCause = ConduitIngressCauseRequiresHMAC
	}
	require.NoError(t, srv.StartConduitExternal(ctx, opts))
	public := httptest.NewServer(srv.Handler())
	t.Cleanup(public.Close)
	return srv, s, public
}

// TestConduitIngress_BrokerDialIngressOffConsumesNothing: an external-mode
// hub whose ingress proxy is off refuses a broker's conduit dial (503) and
// neither admits it nor records its nonce; so does a hub with the proxy on
// and no relay to proxy to.
func TestConduitIngress_BrokerDialIngressOffConsumesNothing(t *testing.T) {
	for _, ingressOff := range []bool{true, false} {
		t.Run("ingressOff="+strconv.FormatBool(ingressOff), func(t *testing.T) {
			nonces := NewNonceCacheStore(enttest.NewClient(t))
			srv, s, public := startBrokerAuthExternal(t, ingressOff, nonces)
			brokerID, secret := addNonceTestBroker(t, s, srv.brokerAuthService, "ingress-off-broker")
			h := brokerSignedHeader(t, http.MethodGet, public.URL+"/api/v1/conduit", brokerID, secret)
			c, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(public.URL, "http")+"/api/v1/conduit", h)
			if c != nil {
				_ = c.Close()
			}
			require.Error(t, err, "no session is admitted")
			require.NotNil(t, resp)
			_ = resp.Body.Close()
			assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
			assert.False(t, nonceRecorded(t, nonces, h.Get(HeaderNonce)), "the refused dial consumes no nonce")
		})
	}
}

// TestConduitIngress_BrokerNonceStillRecorded: every broker request other
// than a conduit dial on an external-mode hub records its nonce: other
// routes (the conduit grant keys among them), other methods on the conduit
// route, and the control-channel upgrade, through the middleware and
// through authenticateBrokerUpgrade alone. A conduit dial to a hub that is
// not in external relay mode records it too.
func TestConduitIngress_BrokerNonceStillRecorded(t *testing.T) {
	nonces := NewNonceCacheStore(enttest.NewClient(t))
	srv, s, public := startBrokerAuthExternal(t, false, nonces)
	brokerID, secret := addNonceTestBroker(t, s, srv.brokerAuthService, "nonce-recorded-broker")

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/conduit/grant-keys"},
		{http.MethodPost, "/api/v1/conduit"},
		{http.MethodGet, "/api/v1/conduit/"},
		{http.MethodGet, "/api/v1/projects"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			h := brokerSignedHeader(t, tc.method, public.URL+tc.path, brokerID, secret)
			req, err := http.NewRequest(tc.method, public.URL+tc.path, nil)
			require.NoError(t, err)
			req.Header = h.Clone()
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			_ = resp.Body.Close()
			// A nonce is recorded only once its signature verified.
			assert.True(t, nonceRecorded(t, nonces, h.Get(HeaderNonce)), "the nonce is recorded")
		})
	}

	t.Run("control channel upgrade", func(t *testing.T) {
		const path = "/api/v1/runtime-brokers/connect"
		h := brokerSignedHeader(t, http.MethodGet, public.URL+path, brokerID, secret)
		u := "ws" + strings.TrimPrefix(public.URL, "http") + path
		c, resp, err := websocket.DefaultDialer.Dial(u, h)
		require.NoError(t, err)
		_ = resp.Body.Close()
		_ = c.Close()
		c, resp, err = websocket.DefaultDialer.Dial(u, h)
		if c != nil {
			_ = c.Close()
		}
		require.Error(t, err)
		require.NotNil(t, resp)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "the replayed upgrade is refused")
	})

	t.Run("authenticateBrokerUpgrade alone", func(t *testing.T) {
		const path = "/api/v1/runtime-brokers/connect"
		h := brokerSignedHeader(t, http.MethodGet, public.URL+path, brokerID, secret)
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header = h.Clone()
		id, ok := srv.authenticateBrokerUpgrade(httptest.NewRecorder(), req)
		require.True(t, ok)
		assert.Equal(t, brokerID, id)
		assert.True(t, nonceRecorded(t, nonces, h.Get(HeaderNonce)), "the upgrade records the nonce")
	})

	t.Run("direct conduit dial", func(t *testing.T) {
		f := newBrokerConduitFixture(t, nil, nil)
		h := f.signedHeader(t, "/api/v1/conduit", nil)
		for i, want := range []int{http.StatusSwitchingProtocols, http.StatusUnauthorized} {
			c, resp, err := websocket.DefaultDialer.Dial(f.wsURL("/api/v1/conduit"), h)
			if c != nil {
				_ = c.Close()
			}
			require.NotNil(t, resp, "dial %d: %v", i, err)
			_ = resp.Body.Close()
			assert.Equal(t, want, resp.StatusCode, "dial %d", i)
		}
	})
}

// TestConduitAdmitNonce_NotSettableFromHeaders: the predicate defers a
// conduit dial's nonce only in external relay mode, only for GET
// /api/v1/conduit, and only without the Admit marker, which no request
// header sets. Through the hub's handler a conduit dial carrying any
// marker-like or relay header still has its nonce deferred.
func TestConduitAdmitNonce_NotSettableFromHeaders(t *testing.T) {
	ext := newExternalFixture(t)
	plain, _ := testServer(t)

	spoof := http.Header{
		"X-Scion-Conduit-Admit":       {"true"},
		"X-Scion-Conduit-Admit-Nonce": {"true"},
		"X-Scion-Admit":               {"1"},
		"Conduit-Admit-Nonce":         {"1"},
		relay.HeaderIngressInstance:   {"hub-a"},
		"X-Forwarded-For":             {conduitRelayAdmitRemoteAddr},
	}
	req := func(method, path string, h http.Header, marked bool) *http.Request {
		r := httptest.NewRequest(method, path, nil)
		for k, v := range h {
			r.Header[k] = v
		}
		if marked {
			r = r.WithContext(withConduitAdmitNonce(r.Context()))
		}
		return r
	}
	for _, tc := range []struct {
		name   string
		srv    *Server
		r      *http.Request
		defers bool
	}{
		{name: "external conduit dial", srv: ext.srv, r: req(http.MethodGet, "/api/v1/conduit", nil, false), defers: true},
		{name: "external conduit dial with query", srv: ext.srv, r: req(http.MethodGet, "/api/v1/conduit?x=1", nil, false), defers: true},
		{name: "external conduit dial with spoofed headers", srv: ext.srv, r: req(http.MethodGet, "/api/v1/conduit", spoof, false), defers: true},
		{name: "Admit's replayed handshake", srv: ext.srv, r: req(http.MethodGet, "/api/v1/conduit", nil, true)},
		{name: "external grant keys", srv: ext.srv, r: req(http.MethodGet, "/api/v1/conduit/grant-keys", nil, false)},
		{name: "external trailing slash", srv: ext.srv, r: req(http.MethodGet, "/api/v1/conduit/", nil, false)},
		{name: "external POST", srv: ext.srv, r: req(http.MethodPost, "/api/v1/conduit", nil, false)},
		{name: "external HEAD", srv: ext.srv, r: req(http.MethodHead, "/api/v1/conduit", nil, false)},
		{name: "external other route", srv: ext.srv, r: req(http.MethodGet, "/api/v1/runtime-brokers/connect", nil, false)},
		{name: "in-process conduit dial", srv: plain, r: req(http.MethodGet, "/api/v1/conduit", spoof, false)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.defers, tc.srv.conduitIngressDefersBrokerNonce(tc.r))
		})
	}
	assert.False(t, conduitAdmitNonce(context.Background()))
	assert.False(t, conduitAdmitNonce(context.WithValue(context.Background(), struct{}{}, true)), "only the unexported key marks")

	t.Run("through the handler", func(t *testing.T) {
		nonces := NewNonceCacheStore(enttest.NewClient(t))
		srv, s, public := startBrokerAuthExternal(t, true, nonces)
		brokerID, secret := addNonceTestBroker(t, s, srv.brokerAuthService, "spoof-marker-broker")
		h := brokerSignedHeader(t, http.MethodGet, public.URL+"/api/v1/conduit", brokerID, secret)
		for k, v := range spoof {
			h[k] = v
		}
		c, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(public.URL, "http")+"/api/v1/conduit", h)
		if c != nil {
			_ = c.Close()
		}
		require.NotNil(t, resp, "%v", err)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
		assert.False(t, nonceRecorded(t, nonces, h.Get(HeaderNonce)), "headers do not mark the dial as Admit's")
	})
}

// TestConduitAdmitNonce_SetOnlyByHubAdmit scans the package's non-test
// sources: the Admit marker is set in exactly one place, hubAdmit's own
// rebuilt request, after the relay row, session id and arrival checks;
// hubAdmit is reached only through the hub internal API's operation table,
// which serveHubInternal dispatches only after the relay-peer
// authentication; and the marker's key is used only by its two accessors.
func TestConduitAdmitNonce_SetOnlyByHubAdmit(t *testing.T) {
	fset := token.NewFileSet()
	// pkgmove:scan-covers guardScanSubdirs
	// scanned through guardScanSubdirs: pkg/hub and every area moved out of it.
	entries, err := readGuardScanDirs(".")
	require.NoError(t, err)
	type use struct {
		file, fn string
		pos      token.Pos
	}
	var setters, keyUses, adminRefs, opsReads []use
	funcs := map[string]*ast.FuncDecl{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		require.NoError(t, err, name)
		for _, d := range file.Decls {
			fd, ok := d.(*ast.FuncDecl)
			fn := ""
			if ok {
				fn = fd.Name.Name
				funcs[name+":"+fn] = fd
			}
			ast.Inspect(d, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "withConduitAdmitNonce" {
						setters = append(setters, use{name, fn, x.Pos()})
					}
				case *ast.Ident:
					if x.Name == "conduitAdmitNonceKey" {
						keyUses = append(keyUses, use{name, fn, x.Pos()})
					}
				case *ast.SelectorExpr:
					if x.Sel.Name == "hubAdmit" {
						adminRefs = append(adminRefs, use{name, fn, x.Pos()})
					}
				case *ast.IndexExpr:
					if id, ok := x.X.(*ast.Ident); ok && id.Name == "hubInternalOps" {
						opsReads = append(opsReads, use{name, fn, x.Pos()})
					}
				}
				return true
			})
		}
	}

	// The marker is set once, in hubAdmit, after its checks.
	require.Len(t, setters, 1, "withConduitAdmitNonce has one call site: %v", setters)
	assert.Equal(t, "conduit_relay_admit.go", setters[0].file)
	require.Equal(t, "hubAdmit", setters[0].fn)
	firstCall := func(fd *ast.FuncDecl, sel string) token.Pos {
		var at token.Pos
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok && at == token.NoPos {
				if s, ok := c.Fun.(*ast.SelectorExpr); ok && s.Sel.Name == sel {
					at = c.Pos()
				}
			}
			return true
		})
		return at
	}
	hubAdmit := funcs["conduit_relay_admit.go:hubAdmit"]
	require.NotNil(t, hubAdmit)
	for _, check := range []string{"relayRow", "ListPrincipalSessionsBySession", "conduitAdmitArrivalRefusal"} {
		at := firstCall(hubAdmit, check)
		require.NotEqual(t, token.NoPos, at, "hubAdmit calls %s", check)
		assert.Less(t, int(at), int(setters[0].pos), "the marker is set after %s", check)
	}
	// The marked context goes only to hubAdmit's own rebuilt request.
	var marksFwd bool
	ast.Inspect(hubAdmit.Body, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			if s, ok := c.Fun.(*ast.SelectorExpr); ok && s.Sel.Name == "NewRequestWithContext" && len(c.Args) > 0 {
				if inner, ok := c.Args[0].(*ast.CallExpr); ok && inner.Pos() == setters[0].pos {
					marksFwd = true
				}
			}
		}
		return true
	})
	assert.True(t, marksFwd, "the marker is the context of hubAdmit's http.NewRequestWithContext")

	// hubAdmit is reached only through the operation table, read only by
	// serveHubInternal after the relay-peer authentication.
	require.Len(t, adminRefs, 1, "hubAdmit is referenced once: %v", adminRefs)
	assert.Equal(t, "conduit_relay_admit.go", adminRefs[0].file)
	assert.Equal(t, "init", adminRefs[0].fn, "the reference registers hubAdmit in hubInternalOps")
	var dispatch []use
	for _, u := range opsReads {
		if u.fn != "init" {
			dispatch = append(dispatch, u)
		}
	}
	require.Len(t, dispatch, 1, "hubInternalOps is read once outside registration: %v", dispatch)
	require.Equal(t, "serveHubInternal", dispatch[0].fn)
	serve := funcs[dispatch[0].file+":serveHubInternal"]
	require.NotNil(t, serve)
	verify := firstCall(serve, "Verify")
	require.NotEqual(t, token.NoPos, verify, "serveHubInternal authenticates the relay peer")
	assert.Less(t, int(verify), int(dispatch[0].pos), "the relay peer is authenticated before dispatch")

	// The key is used only by the type declaration and its two accessors.
	for _, u := range keyUses {
		assert.Equal(t, "conduit_ingress_broker_nonce.go", u.file, "conduitAdmitNonceKey used outside its file")
		assert.Contains(t, []string{"", "withConduitAdmitNonce", "conduitAdmitNonce"}, u.fn, "conduitAdmitNonceKey used by %s", u.fn)
	}
}

// TestNew_BrokerAuthDefersConduitIngressNonce: the broker auth service New
// builds defers a conduit dial's nonce exactly when its own hub is in
// external relay mode.
func TestNew_BrokerAuthDefersConduitIngressNonce(t *testing.T) {
	srv, _ := testServerWithBrokerAuth(t)
	dial := httptest.NewRequest(http.MethodGet, "/api/v1/conduit", nil)
	assert.False(t, srv.brokerAuthService.defersNonce(dial), "not in external relay mode")

	ext, _, _ := startBrokerAuthExternal(t, true, NewNonceCacheStore(enttest.NewClient(t)))
	assert.True(t, ext.brokerAuthService.defersNonce(dial), "in external relay mode")
	assert.False(t, ext.brokerAuthService.defersNonce(dial.WithContext(withConduitAdmitNonce(dial.Context()))))
	assert.False(t, srv.brokerAuthService.defersNonce(dial), "each hub's service reads its own mode")
}

// overCapBrokerRequest returns a copy of the signed request r whose declared
// body is one byte over apiclient.MaxSignedRequestBodyBytes, and the body,
// which counts the bytes read from it. The signature does not cover the body.
func overCapBrokerRequest(r *http.Request) (*http.Request, *bodyLimitCountingBody) {
	over := r.Clone(r.Context())
	body := &bodyLimitCountingBody{size: apiclient.MaxSignedRequestBodyBytes + 1}
	over.Body = body
	over.ContentLength = body.size
	return over, body
}

// TestValidateBrokerSignature_OverCapBodyKeepsNonce: a signed request whose
// declared body is over the signed body limit is refused (413 through the
// broker middleware) before any of its body is read, so before its
// signature is checked, and records no nonce: the same nonce on a request
// within the limit is still accepted once. With the in-memory and the
// database nonce store.
func TestValidateBrokerSignature_OverCapBodyKeepsNonce(t *testing.T) {
	for _, db := range []bool{false, true} {
		t.Run("db="+strconv.FormatBool(db), func(t *testing.T) {
			svc, s := setupTestBrokerAuthService(t)
			if db {
				svc.SetNonceCacheStore(NewNonceCacheStore(enttest.NewClient(t)))
			}
			_, signReq := setupSignedBrokerRequest(t, svc, s)
			ctx := context.Background()
			legit := signReq(http.MethodGet, "/api/v1/conduit", nil)

			nextCalled := false
			mw := BrokerAuthMiddleware(svc)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { nextCalled = true }))
			req, body := overCapBrokerRequest(legit)
			rec := httptest.NewRecorder()
			mw.ServeHTTP(rec, req)
			assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, "body %s", rec.Body.String())
			assert.False(t, nextCalled, "the over-cap request reaches no handler")
			assert.Zero(t, body.read, "no body byte read: the canonical string and its HMAC were not computed")

			req, body = overCapBrokerRequest(legit)
			_, err := svc.ValidateBrokerSignature(ctx, req)
			require.ErrorIs(t, err, errBrokerAuthBodyTooLarge)
			assert.Zero(t, body.read)

			_, err = svc.ValidateBrokerSignature(ctx, legit)
			require.NoError(t, err, "the over-cap requests left the nonce unused")
			_, err = svc.ValidateBrokerSignature(ctx, legit)
			require.ErrorIs(t, err, errBrokerAuthReplay, "the nonce cache is on")
		})
	}
}

// TestConduitIngress_AdmitReplayPassesBodyLimit: Admit's replayed handshake
// (hubAdmit) is a GET without a body, so the signed body limit admits it:
// the broker dial through the ingress is admitted, Admit authenticates the
// broker once and records the nonce.
func TestConduitIngress_AdmitReplayPassesBodyLimit(t *testing.T) {
	var mu sync.Mutex
	var replays []*http.Request
	prev := conduitRelayAdmitObserve
	conduitRelayAdmitObserve = func(r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		replays = append(replays, r)
	}
	t.Cleanup(func() { conduitRelayAdmitObserve = prev })

	bw := newBrokerIngressWorld(t, true)
	h := bw.signedDial(t)
	_, err := bw.dialThroughHubB(t, h)
	require.NoError(t, err, "the dial is admitted through Admit's replayed handshake")
	assert.True(t, nonceRecorded(t, bw.nonces, h.Get(HeaderNonce)), "Admit records the nonce")

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, replays, 1, "Admit replays the handshake once")
	fwd := replays[0]
	assert.Equal(t, http.MethodGet, fwd.Method)
	assert.Zero(t, fwd.ContentLength, "the replayed handshake declares no body")
	assert.True(t, fwd.Body == nil || fwd.Body == http.NoBody, "the replayed handshake carries no body")
	admit := bw.audit[bw.srv].forBroker(bw.brokerID)
	require.Len(t, admit, 1, "Admit authenticates the broker once")
	assert.Equal(t, BrokerAuthEventAuthSuccess, admit[0].EventType)
	assert.Empty(t, admit[0].FailureCode)
}

// TestConduitIngress_OverCapDeferredDialKeepsNonce: on an external-mode hub,
// a broker conduit dial, whose nonce the ingress leaves to Admit, with a
// declared body over the signed body limit is refused 413 before any of its
// body is read, so before its signature is checked, and its nonce stays
// unrecorded.
func TestConduitIngress_OverCapDeferredDialKeepsNonce(t *testing.T) {
	nonces := NewNonceCacheStore(enttest.NewClient(t))
	srv, s, public := startBrokerAuthExternal(t, false, nonces)
	brokerID, secret := addNonceTestBroker(t, s, srv.brokerAuthService, "over-cap-dial-broker")
	h := brokerSignedHeader(t, http.MethodGet, public.URL+"/api/v1/conduit", brokerID, secret)
	dial := httptest.NewRequest(http.MethodGet, "/api/v1/conduit", nil)
	dial.Header = h.Clone()
	dial.Header.Set("Connection", "Upgrade")
	dial.Header.Set("Upgrade", "websocket")
	require.True(t, srv.brokerAuthService.defersNonce(dial), "the dial is on the deferred verify-only path")

	req, body := overCapBrokerRequest(dial)
	_, err := srv.brokerAuthService.ValidateBrokerSignature(context.Background(), req)
	require.ErrorIs(t, err, errBrokerAuthBodyTooLarge)
	assert.Zero(t, body.read, "no body byte read: the signature was not checked")

	req, body = overCapBrokerRequest(dial)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, "body %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"payload_too_large"`)
	assert.Zero(t, body.read)
	assert.False(t, nonceRecorded(t, nonces, h.Get(HeaderNonce)), "the refused dial records no nonce")
}
