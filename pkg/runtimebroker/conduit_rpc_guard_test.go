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
	"bytes"
	"context"
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport/ws"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

const (
	guardBrokerID = "broker-g"
	guardKey      = "0123456789abcdef0123456789abcdef"
)

// guardFixture is a broker conduit session served by the production
// (guarded) RPC handler in front of a recording handler tree, behind a
// permissive broker auth middleware.
type guardFixture struct {
	hub       conduit.LocalSession
	sessionID string

	mu   sync.Mutex
	hits []string
}

func (g *guardFixture) served() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.hits...)
}

// newGuardFixture dials a broker session whose Hello lists rpcs and
// carries incarnation (the fake hub derives the session id from it), and
// returns once the hub admitted it.
func newGuardFixture(t *testing.T, incarnation string, rpcs []string, mods ...func(*conduitRPCGuard)) *guardFixture {
	t.Helper()
	// Permissive: unsigned requests would pass this middleware.
	permissive := NewMultiKeyBrokerAuthMiddleware(true, 5*time.Minute, true)
	permissive.UpdateKeys([]secretKeyEntry{{hubName: "hub-a", secretKey: []byte(guardKey)}})
	return newGuardFixtureBehind(t, incarnation, rpcs, permissive.Middleware, mods...)
}

// newGuardFixtureBehind is newGuardFixture with inner wrapping the
// recording handler tree behind the guard in place of the permissive
// broker auth middleware.
func newGuardFixtureBehind(t *testing.T, incarnation string, rpcs []string, inner func(http.Handler) http.Handler, mods ...func(*conduitRPCGuard)) *guardFixture {
	t.Helper()
	g := &guardFixture{}
	tree := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.hits = append(g.hits, r.Method+" "+r.URL.Path)
		g.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	guard := newConduitRPCGuard(guardBrokerID, func() []byte { return []byte(guardKey) })
	for _, mod := range mods {
		mod(guard)
	}
	h := newGuardedConduitRPCHandler(inner(tree), "hub-a", 0, nil, guard)

	admitted := make(chan conduit.LocalSession, 1)
	hub := newFakeConduitHub(t, hubStep{after: func(ls conduit.LocalSession) { admitted <- ls }})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	d := &ws.Dialer{URL: "ws" + strings.TrimPrefix(hub.srv.URL, "http") + conduitEndpointPath}
	hello := &conduitv1.Hello{
		PrincipalKind: conduitv1.PrincipalKind_PRINCIPAL_KIND_BROKER,
		PrincipalId:   guardBrokerID,
		Capabilities:  &conduitv1.Capabilities{Rpc: rpcs, EndpointIncarnation: incarnation},
	}
	s, w, err := conduit.Dial(ctx, d, conduit.Config{Clock: clock.Real(), RPCHandler: h}, hello)
	require.NoError(t, err)
	ls := s.(conduit.LocalSession)
	t.Cleanup(func() { _ = ls.Close() })
	select {
	case g.hub = <-admitted:
	case <-time.After(10 * time.Second):
		t.Fatal("the broker session was never admitted")
	}
	g.sessionID = w.GetSessionId()
	require.NotEmpty(t, g.sessionID)
	return g
}

// signOpts shapes a signed request.
type signOpts struct {
	noSign          bool   // send no authentication headers at all
	noSessionHeader bool   // omit the session header (and its signed listing)
	unlisted        bool   // set the session header but do not list it as signed
	sessionID       string // the session id to sign for ("" = the fixture's)
	key             string // signing key ("" = guardKey)
}

// signedHeaders returns the headers of a request to method path?query with
// body, signed as the hub signs a routed broker request.
func (g *guardFixture) signedHeaders(t *testing.T, method, path, query string, body []byte, o signOpts) map[string]string {
	t.Helper()
	target := "http://runtime-broker" + path
	if query != "" {
		target += "?" + query
	}
	req, err := http.NewRequest(method, target, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	if o.noSign {
		return map[string]string{"Content-Type": "application/json"}
	}
	sid := o.sessionID
	if sid == "" {
		sid = g.sessionID
	}
	if !o.noSessionHeader {
		req.Header.Set(conduit.BrokerRPCSessionHeader, sid)
		if !o.unlisted {
			req.Header.Set(apiclient.HeaderSignedHeaders, conduit.BrokerRPCSessionHeader)
		}
	}
	key := o.key
	if key == "" {
		key = guardKey
	}
	require.NoError(t, (&apiclient.HMACAuth{BrokerID: guardBrokerID, SecretKey: []byte(key)}).ApplyAuth(req))
	out := map[string]string{}
	for k := range req.Header {
		out[k] = req.Header.Get(k)
	}
	return out
}

// call sends one RpcRequest on the hub's end of the session.
func (g *guardFixture) call(t *testing.T, method, path, query string, headers map[string]string, body []byte) *conduitv1.RpcResponse {
	t.Helper()
	resp, err := g.hub.Call(context.Background(), &conduitv1.RpcRequest{Method: method, Path: path, Query: query, Headers: headers, Body: body})
	require.NoError(t, err)
	return resp
}

// send signs (per o) and sends one request.
func (g *guardFixture) send(t *testing.T, method, path, query string, o signOpts) int32 {
	t.Helper()
	body := []byte(nil)
	if method == http.MethodPost || method == http.MethodDelete {
		body = []byte(`{"x":1}`)
	}
	return g.call(t, method, path, query, g.signedHeaders(t, method, path, query, body, o), body).GetStatus()
}

// TestConduitRPCGuard_ServesOnlyListedOperations: the six broker RPCs are
// served; lifecycle, keys and any other route get 404 before any handler
// runs, even when correctly signed.
func TestConduitRPCGuard_ServesOnlyListedOperations(t *testing.T) {
	g := newGuardFixture(t, "inc-1", conduit.BrokerRPCs())
	for _, r := range []struct{ method, path, query string }{
		{http.MethodGet, "/api/v1/agents/a1/logs", "tail=10"},
		{http.MethodPost, "/api/v1/agents/a1/exec", ""},
		{http.MethodGet, "/api/v1/images/status", "short=img"},
		{http.MethodPost, "/api/v1/images/pull", ""},
		{http.MethodDelete, "/api/v1/images/local", ""},
		{http.MethodDelete, "/api/v1/projects/proj-a", "project_id=p1"},
	} {
		assert.Equal(t, int32(http.StatusOK), g.send(t, r.method, r.path, r.query, signOpts{}), "%s %s", r.method, r.path)
	}
	before := len(g.served())
	for _, r := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/agents/a1/start"},
		{http.MethodPost, "/api/v1/agents/a1/stop"},
		{http.MethodDelete, "/api/v1/agents/a1"},
		{http.MethodPost, "/api/v1/agents/a1/keys"},
		{http.MethodPost, "/api/v1/agents"},
		{http.MethodGet, "/api/v1/agents"},
		{http.MethodGet, "/healthz"},
	} {
		assert.Equal(t, int32(http.StatusNotFound), g.send(t, r.method, r.path, "", signOpts{}), "%s %s", r.method, r.path)
	}
	assert.Len(t, g.served(), before, "no refused request reached a handler")
}

// TestConduitRPCGuard_CanonicalFormOnly: a path or method that is not in
// the exact canonical form of a listed operation is refused.
func TestConduitRPCGuard_CanonicalFormOnly(t *testing.T) {
	g := newGuardFixture(t, "inc-1", conduit.BrokerRPCs())
	for _, r := range []struct{ name, method, path string }{
		{"double slash", http.MethodGet, "/api/v1//agents/a1/logs"},
		{"leading double slash", http.MethodGet, "//api/v1/agents/a1/logs"},
		{"trailing slash", http.MethodGet, "/api/v1/images/status/"},
		{"dot-dot segment", http.MethodGet, "/api/v1/agents/../images/status"},
		{"dot-dot wildcard", http.MethodGet, "/api/v1/agents/../logs"},
		{"dot wildcard", http.MethodGet, "/api/v1/agents/./logs"},
		{"dot-dot into another route", http.MethodPost, "/api/v1/agents/a1/exec/../keys"},
		{"percent-encoded segment", http.MethodGet, "/api/v1/%61gents/a1/logs"},
		{"percent-encoded slash", http.MethodGet, "/api/v1/agents/a1%2F..%2Fx/logs"},
		{"percent-encoded wildcard", http.MethodDelete, "/api/v1/projects/proj%2Da"},
		{"lower-case method", "get", "/api/v1/agents/a1/logs"},
		{"mixed-case method", "Post", "/api/v1/agents/a1/exec"},
		{"wrong method", http.MethodPost, "/api/v1/agents/a1/logs"},
	} {
		assert.Equal(t, int32(http.StatusNotFound), g.send(t, r.method, r.path, "", signOpts{}), r.name)
	}
	assert.Empty(t, g.served(), "no variant reached a handler")
}

// TestConduitRPCGuard_HelloDoesNotWidenAllowlist: the served set is fixed
// on the broker; a session whose Hello lists more RPC names serves no
// more routes.
func TestConduitRPCGuard_HelloDoesNotWidenAllowlist(t *testing.T) {
	g := newGuardFixture(t, "inc-1", append(conduit.BrokerRPCs(), "agent_keys", "agent_start", "agent_delete"))
	for _, r := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/agents/a1/keys"},
		{http.MethodPost, "/api/v1/agents/a1/start"},
		{http.MethodDelete, "/api/v1/agents/a1"},
	} {
		assert.Equal(t, int32(http.StatusNotFound), g.send(t, r.method, r.path, "", signOpts{}), "%s %s", r.method, r.path)
	}
	assert.Empty(t, g.served())
	assert.Len(t, conduitRPCRoutes, len(conduit.BrokerRPCs()), "one route per broker RPC")
}

// TestConduitRPCRoutesMatchBrokerRPCs: the served routes are exactly the
// broker RPCs, pairwise: every name in conduit.BrokerRPCs has one route,
// every route serves one listed name, and each name is on the method and
// path pinned here. An added, removed or changed route, a duplicate, or a
// name on another RPC's route fails.
func TestConduitRPCRoutesMatchBrokerRPCs(t *testing.T) {
	type route struct{ method, path string }
	pinned := map[string]route{
		conduit.RPCAgentLogs:      {http.MethodGet, "/api/v1/agents/*/logs"},
		conduit.RPCAgentExec:      {http.MethodPost, "/api/v1/agents/*/exec"},
		conduit.RPCImageStatus:    {http.MethodGet, "/api/v1/images/status"},
		conduit.RPCImagePull:      {http.MethodPost, "/api/v1/images/pull"},
		conduit.RPCImageDelete:    {http.MethodDelete, "/api/v1/images/local"},
		conduit.RPCProjectCleanup: {http.MethodDelete, "/api/v1/projects/*"},
	}

	names := conduit.BrokerRPCs()
	listed := map[string]bool{}
	for _, n := range names {
		assert.False(t, listed[n], "broker RPC %q listed twice", n)
		listed[n] = true
		_, ok := pinned[n]
		assert.True(t, ok, "broker RPC %q has no pinned route", n)
	}
	for n := range pinned {
		assert.True(t, listed[n], "pinned route %q is not a broker RPC", n)
	}

	served := map[string]route{}
	byRoute := map[route]string{}
	for _, r := range conduitRPCRoutes {
		got := route{r.method, "/" + strings.Join(r.segments, "/")}
		if prev, dup := served[r.name]; dup {
			t.Errorf("name %q served twice: %v and %v", r.name, prev, got)
		}
		if prev, dup := byRoute[got]; dup {
			t.Errorf("route %v served twice: %q and %q", got, prev, r.name)
		}
		served[r.name], byRoute[got] = got, r.name
		assert.True(t, listed[r.name], "route %v serves %q, which is not a broker RPC", got, r.name)
	}
	assert.Equal(t, pinned, served, "served routes by name")
	assert.Len(t, conduitRPCRoutes, len(names), "one route per broker RPC")
	assert.Len(t, conduitRPCRoutes, 6)
}

// TestConduitRPCGuard_SignatureRequired: whatever the broker auth
// middleware allows, a request on the conduit session must carry a valid
// hub signature over the receiving session's id.
func TestConduitRPCGuard_SignatureRequired(t *testing.T) {
	g := newGuardFixture(t, "inc-1", conduit.BrokerRPCs())
	const path = "/api/v1/images/status"
	for _, tc := range []struct {
		name string
		o    signOpts
	}{
		{"unsigned", signOpts{noSign: true}},
		{"bad signature", signOpts{key: "another-key-another-key-another-k"}},
		{"session header missing", signOpts{noSessionHeader: true}},
		{"session header not signed", signOpts{unlisted: true}},
		{"signed for another session", signOpts{sessionID: "s-other"}},
	} {
		assert.Equal(t, int32(http.StatusUnauthorized), g.send(t, http.MethodGet, path, "", tc.o), tc.name)
	}
	assert.Empty(t, g.served(), "no refused request reached a handler")
	assert.Equal(t, int32(http.StatusOK), g.send(t, http.MethodGet, path, "", signOpts{}), "valid")
	assert.Len(t, g.served(), 1)
}

// TestConduitRPCGuard_RepeatRefused: a signed request is served once: a
// repeat on the same session, or onto another session of the broker, is
// refused; a fresh request is served.
func TestConduitRPCGuard_RepeatRefused(t *testing.T) {
	g1 := newGuardFixture(t, "inc-1", conduit.BrokerRPCs())
	g2 := newGuardFixture(t, "inc-2", conduit.BrokerRPCs())
	require.NotEqual(t, g1.sessionID, g2.sessionID)
	const path = "/api/v1/agents/a1/exec"
	body := []byte(`{"command":["true"]}`)
	headers := g1.signedHeaders(t, http.MethodPost, path, "", body, signOpts{})

	assert.Equal(t, int32(http.StatusOK), g1.call(t, http.MethodPost, path, "", headers, body).GetStatus(), "first use")
	assert.Equal(t, int32(http.StatusUnauthorized), g1.call(t, http.MethodPost, path, "", headers, body).GetStatus(), "repeat on the same session")
	assert.Equal(t, int32(http.StatusUnauthorized), g2.call(t, http.MethodPost, path, "", headers, body).GetStatus(), "repeat onto another session")

	fresh := g2.signedHeaders(t, http.MethodPost, path, "", body, signOpts{})
	assert.Equal(t, int32(http.StatusOK), g2.call(t, http.MethodPost, path, "", fresh, body).GetStatus(), "fresh request")
	assert.Len(t, g1.served(), 1)
	assert.Len(t, g2.served(), 1)
}

// TestConduitRPCGuard_RespelledSignatureRefused: the same signed request
// with its signature header written in another base64 spelling of the same
// bytes (other unused low bits, or CR/LF inserted, which the decoder skips)
// is refused by the guard itself, with nothing else behind it.
func TestConduitRPCGuard_RespelledSignatureRefused(t *testing.T) {
	g := newGuardFixtureBehind(t, "inc-1", conduit.BrokerRPCs(), func(h http.Handler) http.Handler { return h })
	const path = "/api/v1/agents/a1/exec"
	body := []byte(`{"command":["true"]}`)
	headers := g.signedHeaders(t, http.MethodPost, path, "", body, signOpts{})
	sig := headers[apiclient.HeaderSignature]
	want, err := base64.StdEncoding.DecodeString(sig)
	require.NoError(t, err)

	spellings := respelledSignatures(t, sig)
	for _, alt := range []string{sig[:10] + "\r\n" + sig[10:], sig[:20] + "\n" + sig[20:], "\r" + sig, sig + "\n"} {
		got, err := base64.StdEncoding.DecodeString(alt)
		require.NoError(t, err, "spelling %q", alt)
		require.Equal(t, want, got, "spelling %q decodes to the same bytes", alt)
		spellings = append(spellings, alt)
	}

	assert.Equal(t, int32(http.StatusOK), g.call(t, http.MethodPost, path, "", headers, body).GetStatus(), "first use")
	for _, alt := range spellings {
		h := make(map[string]string, len(headers))
		for k, v := range headers {
			h[k] = v
		}
		h[apiclient.HeaderSignature] = alt
		resp := g.call(t, http.MethodPost, path, "", h, body)
		assert.Equal(t, int32(http.StatusUnauthorized), resp.GetStatus(), "signature spelled %q", alt)
		assert.Contains(t, string(resp.GetBody()), "request already served", "signature spelled %q", alt)
	}
	assert.Len(t, g.served(), 1, "one signed request is served once")
}

// TestConduitRPCGuard_ReframedNonceRefused: the same signed request with a
// signed header line moved into its nonce (same canonical input, same
// signature, different nonce text; the session header stays listed) is
// refused by the guard itself, with nothing else behind it.
func TestConduitRPCGuard_ReframedNonceRefused(t *testing.T) {
	g := newGuardFixtureBehind(t, "inc-1", conduit.BrokerRPCs(), func(h http.Handler) http.Handler { return h })
	const path = "/api/v1/agents/a1/exec"
	body := []byte(`{"command":["true"]}`)
	signed, reframed := reframedTestHeaders(t, func() *http.Request {
		req, err := http.NewRequest(http.MethodPost, "http://runtime-broker"+path, bytes.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(conduit.BrokerRPCSessionHeader, g.sessionID)
		return req
	}, guardBrokerID, []byte(guardKey), conduit.BrokerRPCSessionHeader)
	flat := func(h http.Header) map[string]string {
		out := map[string]string{}
		for k := range h {
			out[k] = h.Get(k)
		}
		return out
	}

	assert.Equal(t, int32(http.StatusOK), g.call(t, http.MethodPost, path, "", flat(signed), body).GetStatus(), "first use")
	resp := g.call(t, http.MethodPost, path, "", flat(reframed), body)
	assert.Equal(t, int32(http.StatusUnauthorized), resp.GetStatus(), "re-framed request")
	assert.Contains(t, string(resp.GetBody()), "request already served", "re-framed request")
	assert.Len(t, g.served(), 1, "one signed request is served once")
}

// TestSeenOnce_EvictsAfterWindow: a key is refused until its expiry,
// inclusive, and dropped once now is after it, so the store holds one
// window at most.
func TestSeenOnce_EvictsAfterWindow(t *testing.T) {
	s := newSeenOnce()
	t0 := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	window := 5 * time.Minute
	assert.True(t, s.add("k1", t0.Add(window), t0))
	assert.False(t, s.add("k1", t0.Add(window), t0.Add(time.Minute)), "repeat within the window")
	assert.False(t, s.add("k1", t0.Add(window), t0.Add(window)), "repeat exactly at the expiry")
	assert.True(t, s.add("k2", t0.Add(2*window), t0.Add(time.Minute)))
	assert.True(t, s.add("k3", t0.Add(3*window), t0.Add(window).Add(time.Nanosecond)), "k1 expired")
	s.mu.Lock()
	_, k1 := s.keys["k1"]
	n := len(s.keys)
	s.mu.Unlock()
	assert.False(t, k1, "the expired key was dropped")
	assert.Equal(t, 2, n)
}

// TestSeenOnce_RefusesExpiryBeforeSweep: a key whose expiry is before a
// time another add already swept with is refused, even if that add dropped
// it: the caller's own now can be older than the sweep.
func TestSeenOnce_RefusesExpiryBeforeSweep(t *testing.T) {
	s := newSeenOnce()
	t0 := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	assert.True(t, s.add("a", t0, t0.Add(-time.Second)), "first use, inside the window")
	assert.True(t, s.add("b", t0.Add(10*time.Second), t0.Add(time.Nanosecond)), "another key, after a's expiry")
	assert.False(t, s.add("a", t0, t0), "a again, checked at its expiry but after the sweep that dropped it")
	assert.False(t, s.add("c", t0, t0), "a new key whose expiry is before the sweep")
	assert.True(t, s.add("d", t0.Add(time.Nanosecond), t0), "a key expiring exactly at the sweep time")
}

// passAllChecker serves every request (handler tests that are not about
// the guard).
type passAllChecker struct{}

func (passAllChecker) check(context.Context, *wsprotocol.RequestEnvelope) *conduitRPCRefusal {
	return nil
}

// TestConduitRPCHandler_NoGuardRefuses: a handler without a guard serves
// nothing.
func TestConduitRPCHandler_NoGuardRefuses(t *testing.T) {
	served := false
	tree := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served = true
		w.WriteHeader(http.StatusOK)
	})
	for name, h := range map[string]*conduitRPCHandler{
		"unguarded constructor": newConduitRPCHandler(tree, "hub-a", 0, nil),
		"nil guard":             newGuardedConduitRPCHandler(tree, "hub-a", 0, nil, nil),
		"typed nil guard":       newGuardedConduitRPCHandler(tree, "hub-a", 0, nil, (*conduitRPCGuard)(nil)),
	} {
		resp := h.HandleRPC(context.Background(), &conduitv1.RpcRequest{RequestId: "r1", Method: http.MethodGet, Path: "/api/v1/images/status"})
		require.NotNil(t, resp, name)
		assert.Equal(t, int32(http.StatusServiceUnavailable), resp.GetStatus(), name)
	}
	assert.False(t, served, "no request reached a handler")
}

// TestConduitRPCGuard_RawPathCanonical: the raw request path itself must
// be canonical: a fragment, a question mark inside the path, no leading
// slash (host and port form), percent-encoding, a control character or
// user info are refused, even where the parsed path would be a listed
// operation.
func TestConduitRPCGuard_RawPathCanonical(t *testing.T) {
	g := newGuardFixture(t, "inc-1", conduit.BrokerRPCs())
	for _, r := range []struct{ name, method, path string }{
		{"fragment", http.MethodGet, "/api/v1/images/status#frag"},
		{"question mark in the path", http.MethodGet, "/api/v1/images/status?short=img"},
		{"no leading slash (port form)", http.MethodGet, "runtime-broker:80/api/v1/images/status"},
		{"no leading slash", http.MethodGet, "api/v1/images/status"},
		{"percent-encoding", http.MethodGet, "/api/v1/images/st%61tus"},
		{"control character", http.MethodGet, "/api/v1/images/status\t"},
		{"newline", http.MethodGet, "/api/v1/agents/a1\n/logs"},
		{"user info", http.MethodGet, "@evil.example/api/v1/images/status"},
		{"user info after the host", http.MethodGet, "user:pw@runtime-broker/api/v1/images/status"},
	} {
		// Signed for the canonical form; the raw path sent differs from it.
		headers := g.signedHeaders(t, r.method, "/api/v1/images/status", "", nil, signOpts{})
		resp := g.call(t, r.method, r.path, "", headers, nil)
		assert.Equal(t, int32(http.StatusNotFound), resp.GetStatus(), r.name)
	}
	assert.Empty(t, g.served(), "no variant reached a handler")
}

// TestConduitRPCGuard_RepeatAtWindowEdgeRefused: a request repeated at
// exactly its timestamp plus the 5-minute window is still refused.
func TestConduitRPCGuard_RepeatAtWindowEdgeRefused(t *testing.T) {
	var nowNanos atomic.Int64
	g := newGuardFixture(t, "inc-1", conduit.BrokerRPCs(), func(guard *conduitRPCGuard) {
		guard.now = func() time.Time { return time.Unix(0, nowNanos.Load()) }
		// Its own set: the injected clock must not move the process-wide
		// set's sweep time for other tests.
		guard.seen = newSeenOnce()
	})
	const path = "/api/v1/images/status"
	headers := g.signedHeaders(t, http.MethodGet, path, "", nil, signOpts{})
	ts, err := strconv.ParseInt(headers[apiclient.HeaderTimestamp], 10, 64)
	require.NoError(t, err)
	edge := time.Unix(ts, 0).Add(5 * time.Minute)
	nowNanos.Store(edge.UnixNano())

	assert.Equal(t, int32(http.StatusOK), g.call(t, http.MethodGet, path, "", headers, nil).GetStatus(), "first use, at the edge of the window")
	assert.Equal(t, int32(http.StatusUnauthorized), g.call(t, http.MethodGet, path, "", headers, nil).GetStatus(), "repeat at the edge")
	assert.Len(t, g.served(), 1)
}

// TestConduitRPCGuard_NoWelcomeRefused: a request checked on a context
// that carries no session Welcome is refused exactly as a request signed
// for another session, even when it is correctly signed and names an
// empty session id (which no session has).
func TestConduitRPCGuard_NoWelcomeRefused(t *testing.T) {
	guard := newConduitRPCGuard(guardBrokerID, func() []byte { return []byte(guardKey) })
	sign := func(sessionID string) map[string]string {
		req, err := http.NewRequest(http.MethodGet, "http://runtime-broker/api/v1/images/status", nil)
		require.NoError(t, err)
		req.Header.Set(conduit.BrokerRPCSessionHeader, sessionID)
		req.Header.Set(apiclient.HeaderSignedHeaders, conduit.BrokerRPCSessionHeader)
		require.NoError(t, (&apiclient.HMACAuth{BrokerID: guardBrokerID, SecretKey: []byte(guardKey)}).ApplyAuth(req))
		out := map[string]string{}
		for k := range req.Header {
			out[k] = req.Header.Get(k)
		}
		return out
	}
	for _, sid := range []string{"s-1", ""} {
		ref := guard.check(context.Background(), &wsprotocol.RequestEnvelope{
			RequestID: "r-" + sid, Method: http.MethodGet, Path: "/api/v1/images/status", Headers: sign(sid),
		})
		require.NotNil(t, ref, "session id %q: a request without a Welcome must be refused", sid)
		assert.Equal(t, http.StatusUnauthorized, ref.status, "session id %q", sid)
		assert.Equal(t, "request not issued for this session", ref.message, "session id %q: the session-mismatch refusal", sid)
	}
}
