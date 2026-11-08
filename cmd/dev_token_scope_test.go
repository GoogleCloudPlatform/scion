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

package cmd

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// authHeaderHub starts a fake hub on 127.0.0.1 that records the
// Authorization header of every request.
func authHeaderHub(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	return newAuthHeaderHub(t, false)
}

// authHeaderHubTLS is authHeaderHub served over TLS (httptest's certificate).
func authHeaderHubTLS(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	return newAuthHeaderHub(t, true)
}

// newAuthHeaderHub starts the recording hub, over TLS when withTLS is set.
func newAuthHeaderHub(t *testing.T, withTLS bool) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	if withTLS {
		srv.StartTLS()
	} else {
		srv.Start()
	}
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

// nonLoopbackTestHost is a host name that is not a loopback hub for the dev
// credential rule; tests reach a local fake hub under it (hostnameURL).
const nonLoopbackTestHost = "hub.example.test"

// hostnameURL returns the URL of srv under nonLoopbackTestHost. For the test,
// http.DefaultTransport (which the hub clients use) dials that host and port
// to srv's own address, with no proxy, so a non-loopback hub is real to the
// code under test without binding any address besides 127.0.0.1.
func hostnameURL(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	addr := srv.Listener.Addr().String()
	_, port, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	hostPort := net.JoinHostPort(nonLoopbackTestHost, port)
	orig := http.DefaultTransport
	base, ok := orig.(*http.Transport)
	require.True(t, ok, "http.DefaultTransport is not an *http.Transport")
	tr := base.Clone()
	tr.Proxy = nil
	var d net.Dialer
	prev := base.DialContext // an earlier hostnameURL's routing, if any
	tr.DialContext = func(ctx context.Context, network, a string) (net.Conn, error) {
		if a == hostPort {
			a = addr
		}
		if prev != nil {
			return prev(ctx, network, a)
		}
		return d.DialContext(ctx, network, a)
	}
	scheme := "http"
	if srv.TLS != nil {
		// Trust the test server's certificate, which names example.com.
		pool := x509.NewCertPool()
		pool.AddCert(srv.Certificate())
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, ServerName: "example.com"}
		scheme = "https"
	}
	http.DefaultTransport = tr
	t.Cleanup(func() { http.DefaultTransport = orig })
	return scheme + "://" + hostPort
}

// With only a dev token, the REST hub client sends it to a loopback hub and
// sends no credential to an https hub under a non-loopback host name (the
// dev-token loopback gate), while a plain http hub under that name is refused
// by the transport rule before any request.
func TestHubClientForEndpoint_DevTokenOnlyForLoopback(t *testing.T) {
	setupContextTest(t)
	t.Setenv("SCION_DEV_TOKEN", "placeholder-dev-token")
	other, otherSeen := authHeaderHub(t)
	secure, secureSeen := authHeaderHubTLS(t)
	local, localSeen := authHeaderHub(t)

	_, err := hubClientForEndpoint(&config.Settings{}, hostnameURL(t, other))
	require.ErrorIs(t, err, config.ErrPlaintextNonLoopback)
	assert.Empty(t, otherSeen(), "the plain http non-loopback hub received a request")

	client, err := hubClientForEndpoint(&config.Settings{}, hostnameURL(t, secure))
	require.NoError(t, err)
	_, _ = client.Auth().Me(context.Background())
	got := secureSeen()
	require.NotEmpty(t, got, "the https non-loopback hub received no request")
	assert.Empty(t, got[0], "the https non-loopback hub got a credential from the REST client")

	client, err = hubClientForEndpoint(&config.Settings{}, local.URL)
	require.NoError(t, err)
	_, _ = client.Auth().Me(context.Background())
	got = localSeen()
	require.NotEmpty(t, got)
	assert.Equal(t, "Bearer placeholder-dev-token", got[0])
}

// With only a dev token, the completion client sends it to a loopback hub and
// sends no credential to an https hub under a non-loopback host name (its own
// dev-token loopback gate), and a plain http hub under that name gets no
// request.
func TestFetchHubAgents_DevTokenOnlyForLoopback(t *testing.T) {
	setupContextTest(t)
	t.Setenv("SCION_DEV_TOKEN", "placeholder-dev-token")
	other, otherSeen := authHeaderHub(t)
	secure, secureSeen := authHeaderHubTLS(t)
	local, localSeen := authHeaderHub(t)

	_, err := fetchHubAgents(context.Background(), hostnameURL(t, other), &config.Settings{})
	require.ErrorIs(t, err, config.ErrPlaintextNonLoopback)
	assert.Empty(t, otherSeen(), "the plain http non-loopback hub received a request")

	_, _ = fetchHubAgents(context.Background(), hostnameURL(t, secure), &config.Settings{})
	got := secureSeen()
	require.NotEmpty(t, got, "the https non-loopback hub received no request")
	assert.Empty(t, got[0], "the https non-loopback hub got a credential from the completion client")

	_, _ = fetchHubAgents(context.Background(), local.URL, &config.Settings{})
	got = localSeen()
	require.NotEmpty(t, got)
	assert.Equal(t, "Bearer placeholder-dev-token", got[0])
}
