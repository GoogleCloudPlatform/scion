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
	"strings"
	"testing"
	"time"

	scionhub "github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	scionportforward "github.com/GoogleCloudPlatform/scion/pkg/sciontool/portforward"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// configuredHeaderStubAuthenticator is a ProxyAuthenticator whose assertion header is a
// non-default configured name, used to prove the strip threads that name.
type configuredHeaderStubAuthenticator struct{ header string }

func (configuredHeaderStubAuthenticator) Authenticate(*http.Request) (*ProxyUserInfo, error) {
	return nil, nil
}
func (configuredHeaderStubAuthenticator) Name() string                 { return "configured-header-stub" }
func (a configuredHeaderStubAuthenticator) AssertionHeaders() []string { return []string{a.header} }

func TestStripFrontProxyIdentityHeaders(t *testing.T) {
	const custom = "X-Custom-Assn"
	build := func() http.Header {
		h := http.Header{}
		h.Set("X-Auth-Proxy-JWT", "a")
		h.Set("X-Goog-IAP-JWT-Assertion", "b")
		h.Set("X-Goog-Authenticated-User-Email", "c")
		h.Set("X-Goog-Authenticated-User-Id", "d")
		h.Set(custom, "e")
		h.Set("X-Keep", "keep")
		h.Set("Content-Type", "application/json")
		return h
	}

	// No configured provider: the built-in identity headers are removed; the
	// configured-name header is left (no provider declares it) and ordinary
	// headers stay.
	t.Run("defaults", func(t *testing.T) {
		s := &Server{}
		h := build()
		s.stripFrontProxyIdentityHeaders(h)
		for _, k := range []string{"X-Auth-Proxy-JWT", "X-Goog-IAP-JWT-Assertion", "X-Goog-Authenticated-User-Email", "X-Goog-Authenticated-User-Id"} {
			assert.Empty(t, h.Values(k), "%s must be stripped", k)
		}
		assert.Equal(t, "keep", h.Get("X-Keep"))
		assert.Equal(t, "application/json", h.Get("Content-Type"))
	})

	// A configured provider with a non-default header: that header is removed
	// too (in addition to the built-in set).
	t.Run("configured-name", func(t *testing.T) {
		s := &Server{authConfig: AuthConfig{ProxyAuthenticator: configuredHeaderStubAuthenticator{header: custom}}}
		h := build()
		s.stripFrontProxyIdentityHeaders(h)
		assert.Empty(t, h.Values(custom), "the configured assertion header must be stripped")
		assert.Empty(t, h.Values("X-Auth-Proxy-JWT"), "the default header must be stripped too")
		assert.Equal(t, "keep", h.Get("X-Keep"))
	})

	// Case-insensitive: a lower-cased configured name still matches.
	t.Run("case-insensitive", func(t *testing.T) {
		s := &Server{authConfig: AuthConfig{ProxyAuthenticator: configuredHeaderStubAuthenticator{header: "x-custom-assn"}}}
		h := build()
		s.stripFrontProxyIdentityHeaders(h)
		assert.Empty(t, h.Values(custom))
	})
}

// TestConduitProxyStripsFrontProxyAssertion proves that on the conduit path
// proxy the front-proxy identity headers (default and IAP) never reach the
// agent app, while an ordinary header does.
func TestConduitProxyStripsFrontProxyAssertion(t *testing.T) {
	f := newConduitProxyFixture(t, nil)
	f.startAgent(t)

	resp := f.get(t, "/hello", http.Header{
		"X-Auth-Proxy-Jwt":                {"assertion"},
		"X-Goog-Iap-Jwt-Assertion":        {"iap"},
		"X-Goog-Authenticated-User-Email": {"viewer@x"},
		"X-Keep":                          {"kept"},
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)

	got := <-f.app.got
	for _, h := range []string{"X-Auth-Proxy-Jwt", "X-Goog-Iap-Jwt-Assertion", "X-Goog-Authenticated-User-Email"} {
		assert.Empty(t, got.Header.Get(h), "%s reached the agent app", h)
	}
	assert.Equal(t, "kept", got.Header.Get("X-Keep"), "an ordinary header must still be forwarded")
}

// hubIdentityRequestHeaders are request headers the hub reads for its own
// authentication (the federation token and the trusted-proxy identity
// headers). A request proxied to an agent's port must not carry them to the app.
var hubIdentityRequestHeaders = map[string]string{
	FederationTokenHeader:    "federation-token",
	"X-Forwarded-User-Id":    "user-id",
	"X-Forwarded-User-Email": "viewer@x",
	"X-Forwarded-User-Name":  "Viewer",
	"X-Forwarded-User-Role":  "admin",
}

// TestSensitiveHeaderCoversHubIdentityHeaders: the shared request strip of
// both port proxy transports removes the federation token and the
// trusted-proxy identity headers, in any letter case, and keeps an
// ordinary header.
func TestSensitiveHeaderCoversHubIdentityHeaders(t *testing.T) {
	for h := range hubIdentityRequestHeaders {
		assert.True(t, sensitiveHeader(h), h)
		assert.True(t, sensitiveHeader(strings.ToLower(h)), h)
		assert.True(t, sensitiveHeader(strings.ToUpper(h)), h)
	}
	assert.False(t, sensitiveHeader("X-Keep"))
	assert.False(t, sensitiveHeader("X-Forwarded-For"), "the client address header is not an identity header")
}

// TestConduitProxyDropsHubIdentityHeaders: on the conduit path, a request
// to an exposed port that carries the federation token and the
// trusted-proxy identity headers reaches the app without them, while an
// ordinary header is forwarded. The request authenticates with an agent
// token, which the hub accepts before it reads those headers.
func TestConduitProxyDropsHubIdentityHeaders(t *testing.T) {
	f := newConduitProxyFixture(t, nil)
	f.startAgent(t)

	req, err := http.NewRequest(http.MethodGet,
		f.base+"/api/v1/agents/"+f.launched.ID+"/ports/"+strconv.Itoa(f.app.port)+"/proxy/hello", nil)
	require.NoError(t, err)
	for k, v := range hubIdentityRequestHeaders {
		req.Header.Set(k, v)
	}
	req.Header.Set("X-Keep", "kept")
	req.Header.Set("X-Scion-Agent-Token", f.agentToken(t, f.launched))
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))

	got := <-f.app.got
	for h := range hubIdentityRequestHeaders {
		assert.Empty(t, got.Header.Values(h), "%s reached the agent app", h)
	}
	assert.Equal(t, "kept", got.Header.Get("X-Keep"), "an ordinary header must still be forwarded")
}

// TestConduitProxyStripsConfiguredAssertionHeader proves the configured
// (non-default) jwt assertion header is stripped on the conduit path.
func TestConduitProxyStripsConfiguredAssertionHeader(t *testing.T) {
	const custom = "X-Custom-Assn"
	f := newConduitProxyFixtureServer(t, nil, func(s *Server) {
		s.authConfig.ProxyAuthenticator = configuredHeaderStubAuthenticator{header: custom}
	})
	f.startAgent(t)

	resp := f.get(t, "/hello", http.Header{custom: {"assertion"}, "X-Keep": {"kept"}})
	require.Equal(t, http.StatusOK, resp.StatusCode)

	got := <-f.app.got
	assert.Empty(t, got.Header.Get(custom), "the configured assertion header reached the agent app")
	assert.Equal(t, "kept", got.Header.Get("X-Keep"))
}

// legacyPortApp is an app served to the hub over the legacy port tunnel:
// an agent with an exposed port, its tunnel session and the app that
// records the headers of each request it receives.
type legacyPortApp struct {
	srv      *Server
	agentID  string
	token    string
	portText string
	got      chan http.Header
}

func startLegacyPortApp(t *testing.T) *legacyPortApp {
	t.Helper()
	srv, s := testServer(t)
	hubHTTP := httptest.NewServer(srv.Handler())
	t.Cleanup(hubHTTP.Close)

	gotHeader := make(chan http.Header, 1)
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader <- r.Header.Clone()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(app.Close)

	appURL, err := url.Parse(app.URL)
	require.NoError(t, err)
	host, portText, err := net.SplitHostPort(appURL.Host)
	require.NoError(t, err)
	port, err := net.LookupPort("tcp", portText)
	require.NoError(t, err)
	if strings.TrimSpace(host) == "" {
		host = "127.0.0.1"
	}

	agent, token := createPortForwardAgent(t, srv, s)
	rec := doAgentTokenRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/ports", map[string]any{
		"port": port, "host": host,
	}, token)
	require.Equal(t, http.StatusCreated, rec.Code)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	manager := scionportforward.NewManager(scionhub.NewClientWithConfig(hubHTTP.URL, token, agent.ID))
	go manager.Run(ctx)
	require.Eventually(t, func() bool {
		srv.portTunnels.mu.RLock()
		defer srv.portTunnels.mu.RUnlock()
		return srv.portTunnels.sessions[agent.ID] != nil
	}, 5*time.Second, 50*time.Millisecond)
	return &legacyPortApp{srv: srv, agentID: agent.ID, token: token, portText: portText, got: gotHeader}
}

// appHeaders sends a GET with headers (authenticated with the agent token)
// through the legacy port proxy and returns the headers the app received.
func (a *legacyPortApp) appHeaders(t *testing.T, headers http.Header) http.Header {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/"+a.agentID+"/ports/"+a.portText+"/proxy/", nil)
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("X-Scion-Agent-Token", a.token)
	rec := httptest.NewRecorder()
	a.srv.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	select {
	case h := <-a.got:
		return h
	case <-time.After(5 * time.Second):
		t.Fatal("the app never received the proxied request")
		return nil
	}
}

// conduitAppHeaders sends a GET with headers (authenticated with the agent
// token) through f's conduit port proxy and returns the headers the app
// received. f's agent is started.
func conduitAppHeaders(t *testing.T, f *conduitProxyFixture, headers http.Header) http.Header {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet,
		f.base+"/api/v1/agents/"+f.launched.ID+"/ports/"+strconv.Itoa(f.app.port)+"/proxy/hello", nil)
	require.NoError(t, err)
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("X-Scion-Agent-Token", f.agentToken(t, f.launched))
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	select {
	case got := <-f.app.got:
		return got.Header
	case <-time.After(5 * time.Second):
		t.Fatal("the app never received the proxied request")
		return nil
	}
}

// TestLegacyPortProxyStripsFrontProxyAssertion proves that on the legacy tunnel
// path proxy the front-proxy identity headers, the federation token and the
// trusted-proxy identity headers never reach the agent app.
func TestLegacyPortProxyStripsFrontProxyAssertion(t *testing.T) {
	sent := http.Header{
		"X-Auth-Proxy-Jwt":         {"assertion"},
		"X-Goog-Iap-Jwt-Assertion": {"iap"},
		"X-Keep":                   {"kept"},
	}
	for k, v := range hubIdentityRequestHeaders {
		sent.Set(k, v)
	}
	h := startLegacyPortApp(t).appHeaders(t, sent)
	assert.Empty(t, h.Values("X-Auth-Proxy-Jwt"), "X-Auth-Proxy-JWT reached the agent app")
	assert.Empty(t, h.Values("X-Goog-Iap-Jwt-Assertion"), "X-Goog-IAP-JWT-Assertion reached the agent app")
	for k := range hubIdentityRequestHeaders {
		assert.Empty(t, h.Values(k), "%s reached the agent app", k)
	}
	assert.Equal(t, "kept", h.Get("X-Keep"), "an ordinary header must still be forwarded")
}

// TestPortProxyRequestHeaderParity: the conduit and the legacy tunnel
// transports forward the same request headers to the agent's app. Each
// header is sent alone (with the agent token) over both transports; the
// app receives it on both or on neither, and the expected outcome holds.
func TestPortProxyRequestHeaderParity(t *testing.T) {
	cases := []struct {
		header  string
		forward bool
	}{
		{FederationTokenHeader, false},
		{"X-Forwarded-User-Id", false},
		{"X-Forwarded-User-Email", false},
		{"X-Forwarded-User-Name", false},
		{"X-Forwarded-User-Role", false},
		{"x-forwarded-user-email", false},
		{"X-Auth-Proxy-Jwt", false},
		{"X-Goog-Iap-Jwt-Assertion", false},
		{"X-Goog-Authenticated-User-Email", false},
		{"X-Keep", true},
		{"X-Requested-With", true},
	}
	conduit := newConduitProxyFixture(t, nil)
	conduit.startAgent(t)
	legacy := startLegacyPortApp(t)
	for _, tc := range cases {
		t.Run(tc.header, func(t *testing.T) {
			sent := http.Header{}
			sent.Set(tc.header, "parity-value")
			viaConduit := conduitAppHeaders(t, conduit, sent).Get(tc.header)
			viaLegacy := legacy.appHeaders(t, sent).Get(tc.header)
			assert.Equal(t, viaConduit, viaLegacy, "the transports disagree on %s", tc.header)
			if tc.forward {
				assert.Equal(t, "parity-value", viaConduit, "%s must reach the app", tc.header)
			} else {
				assert.Empty(t, viaConduit, "%s reached the app", tc.header)
			}
		})
	}
}
