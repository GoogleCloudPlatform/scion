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
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	sconduit "github.com/GoogleCloudPlatform/scion/pkg/sciontool/conduit"
	sciontoollog "github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConduitProxyTransportNeverDials (TCP rule): the proxy transport's
// only connection is the conduit stream it was given. A second dial and
// any TLS dial fail, and there is no HTTP proxy.
func TestConduitProxyTransportNeverDials(t *testing.T) {
	stream, peer := net.Pipe()
	t.Cleanup(func() { _ = stream.Close(); _ = peer.Close() })
	tr := conduitProxyTransport(stream)

	assert.Nil(t, tr.Proxy, "no HTTP proxy")
	assert.True(t, tr.DisableKeepAlives, "the stream is not reused")
	c, err := tr.DialContext(context.Background(), "tcp", "127.0.0.1:80")
	require.NoError(t, err)
	assert.Same(t, stream, c, "the first dial returns the conduit stream")
	for _, addr := range []string{"127.0.0.1:80", "10.0.0.1:9810", "broker:9800"} {
		_, err := tr.DialContext(context.Background(), "tcp", addr)
		assert.Error(t, err, "second dial to %s", addr)
		_, err = tr.DialTLSContext(context.Background(), "tcp", addr)
		assert.Error(t, err, "TLS dial to %s", addr)
	}
}

// TestConduitProxySourceHasNoNetworkDial (TCP rule): the conduit proxy
// code never dials or listens on the network itself.
func TestConduitProxySourceHasNoNetworkDial(t *testing.T) {
	forbidden := map[string]map[string]bool{
		"net": {"Dial": true, "DialTimeout": true, "Dialer": true, "DialTCP": true, "DialUDP": true,
			"DialIP": true, "DialUnix": true, "Listen": true, "ListenTCP": true, "ListenPacket": true},
		"tls":  {"Dial": true, "DialWithDialer": true, "Dialer": true},
		"http": {"DefaultTransport": true, "DefaultClient": true, "Get": true, "Post": true},
	}
	file, err := parser.ParseFile(token.NewFileSet(), "conduit_proxy.go", nil, 0)
	require.NoError(t, err)
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && forbidden[pkg.Name][sel.Sel.Name] {
			t.Errorf("conduit_proxy.go uses %s.%s", pkg.Name, sel.Sel.Name)
		}
		return true
	})
}

// proxyApp is the agent's in-container HTTP server, recording what it
// received.
type proxyApp struct {
	srv  *httptest.Server
	port int
	got  chan *http.Request
}

// newProxyApp starts the app; a nil handler answers "hello <path>?<query>".
func newProxyApp(t *testing.T, handler http.Handler) *proxyApp {
	t.Helper()
	a := &proxyApp{got: make(chan *http.Request, 16)}
	if handler == nil {
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-App", "ok")
			w.Header().Set("Set-Cookie", "app=1")
			_, _ = io.WriteString(w, "hello "+r.URL.Path+"?"+r.URL.RawQuery)
		})
	}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case a.got <- r.Clone(context.Background()):
		default:
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(a.srv.Close)
	u, err := url.Parse(a.srv.URL)
	require.NoError(t, err)
	a.port, err = strconv.Atoi(u.Port())
	require.NoError(t, err)
	return a
}

// conduitProxyFixture is a relay fixture with hub.conduit on, an app
// listening inside the "agent" and that port exposed on the launched
// agent.
type conduitProxyFixture struct {
	*relayFixture
	app       *proxyApp
	userToken string
	// base is the hub URL agents and callers use (default f.public).
	base string
	// sciontool is the agent started last; sessions counts its admitted
	// sessions.
	sciontool *sconduit.Agent
	sessions  atomic.Int64
	// agentClock fires the sciontool agent's timers; onEnd, when set,
	// receives its reconnect decisions.
	agentClock *clock.Fake
	onEnd      func(end conduit.End, delay time.Duration, err error)
}

func newConduitProxyFixture(t *testing.T, app http.Handler) *conduitProxyFixture {
	t.Helper()
	f := &conduitProxyFixture{relayFixture: newRelayFixture(t, nil), app: newProxyApp(t, app)}
	f.base = f.public.URL
	setConduitExperiment(t, f.srv, true)
	require.True(t, f.srv.conduitServing())
	ctx := context.Background()
	require.NoError(t, f.store.UpdateAgentExposedPorts(ctx, f.launched.ID,
		[]store.ExposedPort{{Port: f.app.port, Host: conduitProxyHost, Mode: "rw"}}))
	got, err := f.store.GetAgent(ctx, f.launched.ID)
	require.NoError(t, err)
	f.launched = got
	f.userToken, _, _, err = f.srv.userTokenService.GenerateTokenPair(
		f.launched.OwnerID, "owner@conduit.test", "Owner", store.UserRoleMember, ClientTypeWeb)
	require.NoError(t, err)
	return f
}

// withWriteTimeout serves the hub on a server with the given write
// timeout (as production does) and makes it f.base.
func (f *conduitProxyFixture) withWriteTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	srv := httptest.NewUnstartedServer(f.srv.Handler())
	srv.Config.WriteTimeout = d
	srv.Start()
	t.Cleanup(srv.Close)
	f.base = srv.URL
}

// fixtureClock is the sciontool agent's clock: it reads the fixture's
// clock (grants are minted on it); its timers fire only when the test
// advances f.agentClock.
type fixtureClock struct {
	*clock.Fake
	now func() time.Time
}

func (c fixtureClock) Now() time.Time { return c.now() }

// startAgent runs a sciontool conduit agent for the launched agent and
// returns once its session is admitted. stop ends it and waits for Run.
func (f *conduitProxyFixture) startAgent(t *testing.T) (stop func()) {
	t.Helper()
	admitted := make(chan *conduitv1.Welcome, 4)
	stop, _ = f.runAgent(t, f.launched.LaunchID, admitted)
	select {
	case w := <-admitted:
		require.Equal(t, f.launched.LaunchID, w.GetEndpointIncarnation())
	case <-time.After(10 * time.Second):
		t.Fatal("conduit session not admitted")
	}
	return stop
}

// runAgent runs a sciontool conduit agent presenting launchID, reporting
// admitted sessions on admitted, and returns a stop function and Run's
// result.
func (f *conduitProxyFixture) runAgent(t *testing.T, launchID string, admitted chan<- *conduitv1.Welcome) (stop func(), result <-chan error) {
	t.Helper()
	guardSciontoolLog()
	tok := f.agentToken(t, f.launched)
	f.agentClock = clock.NewFake(f.clock.Now())
	a, err := sconduit.New(sconduit.Options{
		HubURL:    f.base,
		AgentID:   f.launched.ID,
		ProjectID: f.launched.ProjectID,
		LaunchID:  launchID,
		Token:     func() string { return tok },
		OnSession: func(w *conduitv1.Welcome) {
			f.sessions.Add(1)
			admitted <- w
		},
		OnEnd:   f.onEnd,
		Backoff: &conduit.Backoff{Rand: func(int64) int64 { return 0 }},
		Clock:   fixtureClock{Fake: f.agentClock, now: f.clock.Now},
	})
	require.NoError(t, err)
	f.sciontool = a
	ctx, cancel := context.WithCancel(context.Background())
	exited := make(chan struct{})
	res := make(chan error, 1)
	go func() {
		defer close(exited)
		res <- a.Run(ctx)
	}()
	var once bool
	stop = func() {
		if once {
			return
		}
		once = true
		cancel()
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			t.Error("conduit agent did not stop")
		}
	}
	t.Cleanup(stop)
	return stop, res
}

// get sends a request through the hub's port proxy route.
func (f *conduitProxyFixture) get(t *testing.T, path string, header http.Header) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet,
		f.base+"/api/v1/agents/"+f.launched.ID+"/ports/"+strconv.Itoa(f.app.port)+"/proxy"+path, nil)
	require.NoError(t, err)
	for k, v := range header {
		req.Header[k] = v
	}
	req.Header.Set("Authorization", "Bearer "+f.userToken)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// guardSciontoolLog trips sciontool/log's lazy Init (which replaces the
// process-wide slog default, ptone/scion#2337) and restores the globals.
func guardSciontoolLog() {
	prevDefault, prevWriter, prevFlags := slog.Default(), log.Writer(), log.Flags()
	sciontoollog.Init()
	slog.SetDefault(prevDefault)
	log.SetOutput(prevWriter)
	log.SetFlags(prevFlags)
}

// TestConduitProxyThroughSession: with hub.conduit on, a request to an
// exposed port goes over the agent's conduit session (no port-forward
// tunnel exists), reaches 127.0.0.1:port inside the agent without the
// caller's credentials, and the response is sandboxed.
func TestConduitProxyThroughSession(t *testing.T) {
	f := newConduitProxyFixture(t, nil)
	f.startAgent(t)
	require.False(t, f.srv.portTunnels.has(f.launched.ID), "no legacy tunnel")

	resp := f.get(t, "/hello?x=1", http.Header{"Cookie": {"scion_sess=secret"}})
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	assert.Equal(t, "hello /hello?x=1", string(body))
	assert.Equal(t, "ok", resp.Header.Get("X-App"))
	assert.Empty(t, resp.Header.Get("Set-Cookie"), "the app cannot set hub-origin cookies")
	assert.Equal(t, untrustedContentSandboxCSP, resp.Header.Get("Content-Security-Policy"))
	assert.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"))

	got := <-f.app.got
	assert.Equal(t, "127.0.0.1:"+strconv.Itoa(f.app.port), got.Host)
	for _, h := range []string{"Authorization", "Cookie"} {
		assert.Empty(t, got.Header.Get(h), "%s reached the agent", h)
	}
}

// TestConduitProxyAgentOffline: with hub.conduit on and neither a conduit
// session nor a tunnel, the proxy answers 503 agent_offline (an HTML page
// for browsers), for plain requests and WebSocket upgrades alike.
func TestConduitProxyAgentOffline(t *testing.T) {
	f := newConduitProxyFixture(t, nil)
	for _, tc := range []struct {
		name   string
		header http.Header
		html   bool
	}{
		{name: "api client", header: http.Header{"Accept": {"application/json"}}},
		{name: "browser", header: http.Header{"Accept": {"text/html"}}, html: true},
		{name: "websocket", header: http.Header{"Connection": {"Upgrade"}, "Upgrade": {"websocket"},
			"Sec-Websocket-Version": {"13"}, "Sec-Websocket-Key": {"dGhlIHNhbXBsZSBub25jZQ=="}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := f.get(t, "/", tc.header)
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, string(body))
			if tc.html {
				assert.Contains(t, string(body), "Agent Offline")
				return
			}
			var e struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(body, &e), string(body))
			assert.Equal(t, ErrCodeAgentOffline, e.Error.Code)
		})
	}
}

// TestConduitProxyKeepsPortsWhenSessionEnds: when the agent's conduit
// session ends its exposed ports are kept; the proxy answers 503
// agent_offline until the agent reconnects, then serves again.
func TestConduitProxyKeepsPortsWhenSessionEnds(t *testing.T) {
	f := newConduitProxyFixture(t, nil)
	stop := f.startAgent(t)
	require.Equal(t, http.StatusOK, f.get(t, "/", nil).StatusCode)

	stop()
	require.Eventually(t, func() bool {
		return f.get(t, "/", nil).StatusCode == http.StatusServiceUnavailable
	}, 10*time.Second, 20*time.Millisecond, "proxy still served after the session ended")
	got, err := f.store.GetAgent(context.Background(), f.launched.ID)
	require.NoError(t, err)
	require.Len(t, got.ExposedPorts, 1, "exposed ports were cleared")
	assert.Equal(t, f.app.port, got.ExposedPorts[0].Port)

	f.startAgent(t)
	resp := f.get(t, "/again", nil)
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	assert.True(t, strings.HasPrefix(string(body), "hello /again"))
}
