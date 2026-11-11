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
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConduitProxyTransportNeverDials (TCP rule): the proxy transport's
// only connection is the conduit stream it was given. A second dial and
// any TLS dial fail, and there is no HTTP proxy.
func TestConduitProxyTransportNeverDials(t *testing.T) {
	stream, peer := net.Pipe()
	t.Cleanup(func() { _ = stream.Close(); _ = peer.Close() })
	tr := conduitProxyTransport(stream, 0)

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
	assert.Equal(t, agentServedPageSandboxCSP, resp.Header.Get("Content-Security-Policy"))
	assert.Equal(t, untrustedContentPermissionsPolicy, resp.Header.Get("Permissions-Policy"))
	assert.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"))

	got := <-f.app.got
	assert.Equal(t, "127.0.0.1:"+strconv.Itoa(f.app.port), got.Host)
	for _, h := range []string{"Authorization", "Cookie"} {
		assert.Empty(t, got.Header.Get(h), "%s reached the agent", h)
	}
}

// TestConduitProxyReplacesThePagePolicy: behind the web server's security
// headers, which set the web pages' policy on every response, a page
// served from an agent's port carries exactly one policy, the sandbox
// one, one Permissions-Policy, the untrusted-content one, and one nosniff
// header, as on the tunnel path.
func TestConduitProxyReplacesThePagePolicy(t *testing.T) {
	f := newConduitProxyFixture(t, nil)
	f.startAgent(t)
	web := httptest.NewServer((&WebServer{}).securityHeadersMiddleware(f.srv.Handler()))
	t.Cleanup(web.Close)
	f.base = web.URL

	resp := f.get(t, "/hello", nil)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	assert.Equal(t, []string{agentServedPageSandboxCSP}, resp.Header.Values("Content-Security-Policy"), "conduit response policies")
	assert.Equal(t, []string{untrustedContentPermissionsPolicy}, resp.Header.Values("Permissions-Policy"), "conduit response Permissions-Policy headers")
	assert.Equal(t, []string{"nosniff"}, resp.Header.Values("X-Content-Type-Options"), "conduit response nosniff headers")

	// Positive control: the same wrapper does put the page policy on a hub
	// response that is not proxied, so the check above is not vacuous.
	health, err := http.Get(web.URL + "/healthz")
	require.NoError(t, err)
	t.Cleanup(func() { _ = health.Body.Close() })
	policies := health.Header.Values("Content-Security-Policy")
	require.Len(t, policies, 1)
	assert.Contains(t, policies[0], "script-src 'self' 'nonce-")
}

// TestConduitProxyKeepsHubPageHeaders: behind the web server's security
// headers, the hub's own page headers are not combined with the proxied
// app's: each has exactly the hub's single value. A header the hub does
// not set still passes through from the app (positive control).
func TestConduitProxyKeepsHubPageHeaders(t *testing.T) {
	f := newConduitProxyFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for name, v := range agentPageHeaders {
			w.Header().Set(name, v)
		}
		w.Header().Set("X-Agent-Kept", "kept")
		_, _ = io.WriteString(w, "page")
	}))
	f.startAgent(t)
	web := httptest.NewServer((&WebServer{}).securityHeadersMiddleware(f.srv.Handler()))
	t.Cleanup(web.Close)
	f.base = web.URL

	resp := f.get(t, "/", nil)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	hub := proxiedPageHeaderValues(t)
	for _, name := range hubPageHeaderNames {
		assert.Equal(t, []string{hub[name]}, resp.Header.Values(name), "conduit page header "+name)
	}
	assert.Equal(t, "kept", resp.Header.Get("X-Agent-Kept"))
}

// TestConduitProxyLoopbackHosts: an exposed port registered with any
// loopback host goes over the agent's conduit session to 127.0.0.1:port;
// a non-loopback host never takes the conduit route.
func TestConduitProxyLoopbackHosts(t *testing.T) {
	for _, tc := range []struct {
		host    string
		conduit bool
	}{
		{host: "127.0.0.1", conduit: true},
		{host: "127.0.0.2", conduit: true},
		{host: "localhost", conduit: true},
		{host: "LocalHost", conduit: true},
		{host: "::1", conduit: true},
		{host: "10.0.0.1", conduit: false},
	} {
		t.Run(tc.host, func(t *testing.T) {
			f := newConduitProxyFixture(t, nil)
			f.setExposedHost(t, tc.host)
			if !tc.conduit {
				require.Error(t, validateExposedPort(f.app.port, tc.host), "registration accepts a non-loopback host")
			}
			f.startAgent(t)
			require.False(t, f.srv.portTunnels.has(f.launched.ID), "no legacy tunnel")

			resp := f.get(t, "/hello", nil)
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			if !tc.conduit {
				// Not proxied over conduit: with no tunnel either, the
				// agent is offline, and nothing reached the app.
				require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, string(body))
				select {
				case got := <-f.app.got:
					t.Fatalf("request reached the app at %s", got.Host)
				default:
				}
				return
			}
			require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
			assert.Equal(t, "hello /hello?", string(body))
			got := <-f.app.got
			assert.Equal(t, "127.0.0.1:"+strconv.Itoa(f.app.port), got.Host)
		})
	}
}

// conduitProxyTestTimeout bounds every request a test makes through the
// proxy, so a hang fails the test within seconds instead of at the
// package timeout.
const conduitProxyTestTimeout = 15 * time.Second

// requireIPv6Loopback skips unless [::1] can be bound and dialled, and
// returns the bound listener.
func requireIPv6Loopback(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback: %v", err)
	}
	accepted := make(chan struct{})
	go func() {
		defer close(accepted)
		if c, err := ln.Accept(); err == nil {
			_ = c.Close()
		}
	}()
	c, err := net.DialTimeout("tcp", ln.Addr().String(), 2*time.Second)
	if err != nil {
		_ = ln.Close()
		<-accepted
		t.Skipf("IPv6 loopback not dialable: %v", err)
	}
	_ = c.Close()
	<-accepted
	return ln
}

// TestConduitProxyIPv6OnlyListener: a ::1 registration targets 127.0.0.1
// inside the agent, and the agent's TCP target retries on [::1] when
// 127.0.0.1 refuses, so a service listening only on ::1 answers over
// conduit (ptone/scion#3489). Once nothing listens on either family the
// proxy answers 502, for plain requests and WebSocket upgrades alike.
func TestConduitProxyIPv6OnlyListener(t *testing.T) {
	ln := requireIPv6Loopback(t)
	t.Cleanup(func() { _ = ln.Close() })
	port := ln.Addr().(*net.TCPAddr).Port
	if v4, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port))); err != nil {
		t.Skipf("127.0.0.1:%d is in use: %v", port, err)
	} else {
		_ = v4.Close()
	}
	got := make(chan string, 4)
	v6 := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case got <- r.Host:
			default:
			}
			_, _ = io.WriteString(w, "v6 "+r.URL.Path)
		}),
		ReadHeaderTimeout: conduitProxyTestTimeout,
	}
	go func() { _ = v6.Serve(ln) }()
	t.Cleanup(func() { _ = v6.Close() })

	f := newConduitProxyFixture(t, nil)
	f.app.port = port
	f.setExposedHost(t, "::1")
	f.startAgent(t)

	resp := f.getWithin(t, "/hello", nil, conduitProxyTestTimeout)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	assert.Equal(t, "v6 /hello", string(body))
	select {
	case host := <-got:
		assert.Equal(t, "127.0.0.1:"+strconv.Itoa(port), host)
	default:
		t.Fatal("request did not reach the ::1 service")
	}

	// Nothing listens on either family now: the agent's dial is refused
	// on both, and the proxy answers 502.
	require.NoError(t, v6.Close())
	for _, tc := range []struct {
		name   string
		header http.Header
	}{
		{name: "http"},
		{name: "websocket", header: http.Header{"Connection": {"Upgrade"}, "Upgrade": {"websocket"},
			"Sec-Websocket-Version": {"13"}, "Sec-Websocket-Key": {"dGhlIHNhbXBsZSBub25jZQ=="}}},
	} {
		t.Run("refused on both/"+tc.name, func(t *testing.T) {
			resp := f.getWithin(t, "/", tc.header, conduitProxyTestTimeout)
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.Equal(t, http.StatusBadGateway, resp.StatusCode, string(body))
			var e struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(body, &e), string(body))
			assert.Equal(t, ErrCodeRuntimeError, e.Error.Code)
			assert.Equal(t, "Nothing is listening on the agent port", e.Error.Message)
		})
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

// TestConduitProxyBareURLRedirect (ptone/scion#5566): on the conduit path,
// GET and HEAD of the bare .../proxy URL are redirected to .../proxy/ with
// the raw query kept, and a client that follows the redirect gets the
// app's root.
func TestConduitProxyBareURLRedirect(t *testing.T) {
	f := newConduitProxyFixture(t, nil)
	f.startAgent(t)
	bare := f.base + "/api/v1/agents/" + f.launched.ID + "/ports/" + strconv.Itoa(f.app.port) + "/proxy"
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			req, err := http.NewRequest(method, bare+"?x=1&y=a%2Fb", nil)
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer "+f.userToken)
			resp, err := noFollow.Do(req)
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()
			require.Equal(t, http.StatusPermanentRedirect, resp.StatusCode)
			assert.Equal(t, "proxy/?x=1&y=a%2Fb", resp.Header.Get("Location"))
		})
	}

	t.Run("followed", func(t *testing.T) {
		resp := f.get(t, "?x=1", nil)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
		assert.Equal(t, "hello /?x=1", string(body))
		assert.Equal(t, bare+"/?x=1", resp.Request.URL.String())
		assert.Equal(t, agentServedPageSandboxCSP, resp.Header.Get("Content-Security-Policy"))
	})

	t.Run("caller_without_access_unchanged", func(t *testing.T) {
		// The fixture's stranger: a signed-in project member without
		// port access to the owner's agents.
		stranger, _, _, err := f.srv.userTokenService.GenerateTokenPair(
			f.stranger.ID(), "stranger@conduit.test", "Stranger", store.UserRoleMember, ClientTypeWeb)
		require.NoError(t, err)
		answer := func(suffix string) (int, http.Header, string) {
			req, err := http.NewRequest(http.MethodGet, bare+suffix+"?x=1", nil)
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer "+stranger)
			resp, err := noFollow.Do(req)
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			return resp.StatusCode, resp.Header, string(body)
		}
		bareCode, bareHeader, bareBody := answer("")
		slashCode, _, slashBody := answer("/")
		assert.Equal(t, http.StatusForbidden, bareCode, bareBody)
		assert.Equal(t, slashCode, bareCode)
		assert.Equal(t, slashBody, bareBody)
		assert.Empty(t, bareHeader.Get("Location"))
	})
}

// TestConduitProxyNoCredentialsPlainText (ptone/scion#5566): on the
// conduit path, a page sub-request with no credential gets the plain-text
// 401 and is counted, and never reaches the app.
func TestConduitProxyNoCredentialsPlainText(t *testing.T) {
	f := newConduitProxyFixture(t, nil)
	count := portProxyCounterNamed(t, f.srv, "scion.hub.port_proxy.sandboxed_subresource_unauth")
	f.startAgent(t)
	base := f.base + "/api/v1/agents/" + f.launched.ID + "/ports/" + strconv.Itoa(f.app.port) + "/proxy"

	for i, path := range []string{"/app.js", "", "/api/ping?x=1"} {
		req, err := http.NewRequest(http.MethodGet, base+path, nil)
		require.NoError(t, err)
		req.Header.Set("Origin", "null")
		req.Header.Set("Accept", "*/*")
		req.Header.Set("Cookie", "scion_sess=not-a-session")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		require.NoError(t, err)
		requirePortProxyNoCredentials(t, resp.StatusCode, resp.Header, string(body), true)
		assert.Equal(t, int64(i+1), count(), "path %q", path)
	}
	select {
	case got := <-f.app.got:
		t.Fatalf("a request without credentials reached the app: %s", got.URL)
	default:
	}
}

// TestConduitProxyBareURLWebSocketNotRedirected (ptone/scion#5566): an
// admitted WebSocket upgrade to the bare .../proxy URL is not redirected
// (WebSocket clients do not follow redirects); it is proxied to the app's
// root as before.
func TestConduitProxyBareURLWebSocketNotRedirected(t *testing.T) {
	f := newConduitProxyFixture(t, echoApp())
	f.startAgent(t)
	wsURL := "ws" + strings.TrimPrefix(f.base, "http") +
		"/api/v1/agents/" + f.launched.ID + "/ports/" + strconv.Itoa(f.app.port) + "/proxy"
	c, resp, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Authorization": {"Bearer " + f.userToken}})
	if resp != nil {
		assert.NotEqual(t, http.StatusPermanentRedirect, resp.StatusCode)
		assert.Empty(t, resp.Header.Get("Location"))
	}
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	require.NoError(t, c.SetReadDeadline(time.Now().Add(10*time.Second)))
	require.NoError(t, c.WriteMessage(websocket.TextMessage, []byte("ping")))
	_, got, err := c.ReadMessage()
	require.NoError(t, err)
	assert.Equal(t, "ping", string(got))
	select {
	case req := <-f.app.got:
		assert.Equal(t, "/", req.URL.Path)
	case <-time.After(5 * time.Second):
		t.Fatal("the upgrade did not reach the app")
	}
}
