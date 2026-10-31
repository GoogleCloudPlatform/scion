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
	"crypto/tls"
	"crypto/x509"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

const testDevToken = "placeholder-dev-token"

// recordedRequest is the auth-relevant part of a request a fake hub saw.
type recordedRequest struct {
	authorization string
	brokerID      string
}

// devAuthTestHub starts a fake hub on 127.0.0.1 that records the auth
// headers of every request it receives.
func devAuthTestHub(t *testing.T) (*httptest.Server, func() []recordedRequest) {
	t.Helper()
	return devAuthTestHubWith(t, httptest.NewServer)
}

// remoteHubHost is the non-loopback name the tests give a remote hub. It is
// one of the names in httptest's TLS certificate, so a TLS fake hub serves it
// with a certificate the client accepts (hubURLAs trusts it).
const remoteHubHost = "example.com"

// devAuthTestHubFor is devAuthTestHub for a hub addressed as host: a
// loopback host gets a plain http hub, any other host a TLS hub, since the
// broker refuses a plain http hub on a network host
// (config.CheckHubTransport) and these tests are about what reaches a hub
// the broker does connect to.
func devAuthTestHubFor(t *testing.T, host string) (*httptest.Server, func() []recordedRequest) {
	t.Helper()
	if config.IsLoopbackHost(host) {
		return devAuthTestHub(t)
	}
	return devAuthTestHubWith(t, httptest.NewTLSServer)
}

func devAuthTestHubWith(t *testing.T, newServer func(http.Handler) *httptest.Server) (*httptest.Server, func() []recordedRequest) {
	t.Helper()
	var mu sync.Mutex
	var seen []recordedRequest
	srv := newServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, recordedRequest{
			authorization: r.Header.Get("Authorization"),
			brokerID:      r.Header.Get(apiclient.HeaderBrokerID),
		})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []recordedRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]recordedRequest(nil), seen...)
	}
}

// hubURLAs returns srv's URL under host, and makes http.DefaultTransport
// (which the hub client uses) dial that host and port to srv's own address
// for the rest of the test, with no proxy. A hub at localhost, [::1] or a
// non-loopback name is then real to the code under test without binding any
// address besides 127.0.0.1.
func hubURLAs(t *testing.T, srv *httptest.Server, host string) string {
	t.Helper()
	addr := srv.Listener.Addr().String()
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	hostPort := net.JoinHostPort(host, port)
	orig := http.DefaultTransport
	base, ok := orig.(*http.Transport)
	if !ok {
		t.Fatal("http.DefaultTransport is not an *http.Transport")
	}
	tr := base.Clone()
	tr.Proxy = nil
	scheme := "http"
	if cert := srv.Certificate(); cert != nil {
		// A TLS fake hub: trust its certificate (it names example.com,
		// 127.0.0.1 and ::1).
		pool := x509.NewCertPool()
		pool.AddCert(cert)
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
		scheme = "https"
	}
	prevDial := base.DialContext
	if prevDial == nil {
		var d net.Dialer
		prevDial = d.DialContext
	}
	tr.DialContext = func(ctx context.Context, network, a string) (net.Conn, error) {
		if a == hostPort {
			a = addr
		}
		return prevDial(ctx, network, a)
	}
	http.DefaultTransport = tr
	t.Cleanup(func() { http.DefaultTransport = orig })
	return scheme + "://" + hostPort
}

// devTokenEnv sets a made-up dev token and clears every other source of a
// hub credential or transport token the client could pick up.
func devTokenEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	for _, name := range []string{
		"SCION_DEV_TOKEN_FILE", "SCION_AUTH_TOKEN", "SCION_HUB_TOKEN",
		"SCION_TRANSPORT_TOKEN", "SCION_TRANSPORT_TOKEN_FILE", "SCION_TRANSPORT_AUDIENCE",
		"SCION_TRANSPORT_MODE", "SCION_TRANSPORT_TOKEN_EXPIRY", "SCION_HUB_OIDC_AUDIENCE",
		"SCION_METADATA_MODE", "SCION_DEBUG",
	} {
		t.Setenv(name, "")
	}
	t.Setenv("SCION_DEV_TOKEN", testDevToken)
}

// captureLogs sends slog output to a buffer for the rest of the test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(orig) })
	return &buf
}

// firstRequest makes one call with client and returns what the hub saw.
func firstRequest(t *testing.T, client hubclient.Client, seen func() []recordedRequest) recordedRequest {
	t.Helper()
	_, _ = client.Auth().Me(context.Background())
	got := seen()
	if len(got) == 0 {
		t.Fatal("the hub received no request")
	}
	return got[0]
}

// isLocalhostEndpoint is the rule for the broker's dev token. These are the
// cases pkg/hubsync's IsLocalhostEndpoint (the CLI's rule) is held to, so
// the two copies cannot drift apart unnoticed.
func TestIsLocalhostEndpoint_DevAuthLoopbackRule(t *testing.T) {
	for ep, want := range map[string]bool{
		"http://localhost":                    true,
		"http://localhost:8080":               true,
		"https://localhost:9810/api":          true,
		"http://127.0.0.1":                    true,
		"http://127.0.0.1:8080":               true,
		"http://[::1]":                        true,
		"http://[::1]:8080":                   true,
		"http://LOCALHOST:8080":               false,
		"http://Localhost:8080":               false,
		"http://127.0.0.2:8080":               false,
		"http://0.0.0.0:8080":                 false,
		"http://10.0.0.5:8080":                false,
		"http://[::2]:8080":                   false,
		"http://::1:8080":                     false,
		"https://hub.example.com":             false,
		"http://localhost.example:8080":       false,
		"http://localhost.example.com:8080":   false,
		"http://localhost.:8080":              false,
		"http://localhost@hub.example.com":    false,
		"http://hub.example.com/localhost":    false,
		"http://hub.example.com?h=127.0.0.1":  false,
		"http://127.0.0.1.hub.example.com:80": false,
		"localhost:8080":                      false,
		"":                                    false,
	} {
		if got := isLocalhostEndpoint(ep); got != want {
			t.Errorf("isLocalhostEndpoint(%q) = %v, want %v", ep, got, want)
		}
	}
}

// A dev-auth connection, or a connection without a secret key, sends the dev
// token to a loopback hub and no credential to any other hub.
func TestBuildHubClientOpts_DevTokenOnlyForLoopbackHub(t *testing.T) {
	devTokenEnv(t)
	logs := captureLogs(t)

	for _, mode := range []brokercredentials.AuthMode{brokercredentials.AuthModeDevAuth, ""} {
		for _, host := range []string{"127.0.0.1", "localhost", "::1"} {
			srv, seen := devAuthTestHub(t)
			endpoint := hubURLAs(t, srv, host)
			creds := &brokercredentials.BrokerCredentials{Name: "local", HubEndpoint: endpoint, AuthMode: mode}
			client, err := hubclient.New(endpoint, buildHubClientOpts(creds, nil, endpoint)...)
			if err != nil {
				t.Fatal(err)
			}
			if got := firstRequest(t, client, seen).authorization; got != "Bearer "+testDevToken {
				t.Errorf("mode %q, hub %s: Authorization %q, want the dev token", mode, endpoint, got)
			}
		}

		srv, seen := devAuthTestHub(t)
		endpoint := hubURLAs(t, srv, "hub.example.test")
		creds := &brokercredentials.BrokerCredentials{Name: "remote", HubEndpoint: endpoint, AuthMode: mode}
		client, err := hubclient.New(endpoint, buildHubClientOpts(creds, nil, endpoint)...)
		if err != nil {
			t.Fatal(err)
		}
		if got := firstRequest(t, client, seen); got.authorization != "" || got.brokerID != "" {
			t.Errorf("mode %q: sent %+v, want no credential (hub %s)", mode, got, endpoint)
		}
	}

	out := logs.String()
	if strings.Contains(out, testDevToken) {
		t.Errorf("logs contain the dev token:\n%s", out)
	}
	if n := strings.Count(out, "the dev token is used only for a local hub; it is not sent"); n != 2 {
		t.Errorf("not-sent log line appeared %d times, want 2 (one per remote connection):\n%s", n, out)
	}
}

// With a secret key the connection authenticates with HMAC, whatever the
// hub's address, and never sends the dev token.
func TestBuildHubClientOpts_HMACUnchanged(t *testing.T) {
	devTokenEnv(t)
	for _, host := range []string{"127.0.0.1", "hub.example.test"} {
		srv, seen := devAuthTestHub(t)
		endpoint := hubURLAs(t, srv, host)
		creds := &brokercredentials.BrokerCredentials{Name: "hmac", BrokerID: "rb-test", HubEndpoint: endpoint}
		client, err := hubclient.New(endpoint, buildHubClientOpts(creds, []byte("made-up-secret"), endpoint)...)
		if err != nil {
			t.Fatal(err)
		}
		got := firstRequest(t, client, seen)
		if got.brokerID != "rb-test" {
			t.Errorf("hub %s: broker ID header %q, want rb-test", endpoint, got.brokerID)
		}
		if strings.Contains(got.authorization, testDevToken) {
			t.Errorf("hub %s: Authorization %q carries the dev token", endpoint, got.authorization)
		}
	}
}

// A connection built from server config sends the configured bearer token to
// any hub; without one it sends the dev token only to a loopback hub.
func TestCreateHubConnectionFromConfig_DevTokenOnlyForLoopbackHub(t *testing.T) {
	devTokenEnv(t)

	cases := []struct {
		host     string
		hubToken string
		want     string
	}{
		{"127.0.0.1", "", "Bearer " + testDevToken},
		{"localhost", "", "Bearer " + testDevToken},
		{"::1", "", "Bearer " + testDevToken},
		{remoteHubHost, "", ""},
		{remoteHubHost, "made-up-hub-token", "Bearer made-up-hub-token"},
		{"127.0.0.1", "made-up-hub-token", "Bearer made-up-hub-token"},
	}
	for _, tc := range cases {
		srv, seen := devAuthTestHubFor(t, tc.host)
		endpoint := hubURLAs(t, srv, tc.host)
		s := &Server{config: ServerConfig{HubEndpoint: endpoint, HubToken: tc.hubToken}}
		conn, err := s.createHubConnectionFromConfig()
		if err != nil {
			t.Fatal(err)
		}
		if got := firstRequest(t, conn.HubClient, seen).authorization; got != tc.want {
			t.Errorf("hub %s, hub token %q: Authorization %q, want %q", endpoint, tc.hubToken, got, tc.want)
		}
	}
}

// A connection from stored credentials decides on the hub it actually
// connects to: the credentials' endpoint, or the server's hub endpoint when
// the credentials name none. When both are set, the credentials' endpoint is
// the one dialed, so a loopback server endpoint does not let the dev token
// reach a remote hub named by the credentials.
func TestCreateHubConnection_DevTokenOnlyForLoopbackHub(t *testing.T) {
	devTokenEnv(t)

	cases := []struct {
		name       string
		credsHost  string // "" = the credentials name no endpoint
		configHost string // "" = the server has no hub endpoint
		mode       brokercredentials.AuthMode
		want       string
	}{
		{"creds-local", "127.0.0.1", "localhost", brokercredentials.AuthModeDevAuth, "Bearer " + testDevToken},
		{"creds-remote", remoteHubHost, "", brokercredentials.AuthModeDevAuth, ""},
		{"creds-remote-config-local", remoteHubHost, "localhost", brokercredentials.AuthModeDevAuth, ""},
		{"config-local", "", "localhost", brokercredentials.AuthModeDevAuth, "Bearer " + testDevToken},
		{"config-remote", "", remoteHubHost, brokercredentials.AuthModeDevAuth, ""},
		{"creds-local-nosecret", "::1", "localhost", "", "Bearer " + testDevToken},
		{"config-remote-nosecret", "", remoteHubHost, "", ""},
	}
	for _, tc := range cases {
		dialed := tc.credsHost
		if dialed == "" {
			dialed = tc.configHost
		}
		srv, seen := devAuthTestHubFor(t, dialed)
		creds := &brokercredentials.BrokerCredentials{Name: tc.name, BrokerID: "rb-test", AuthMode: tc.mode}
		s := &Server{}
		if tc.credsHost != "" {
			creds.HubEndpoint = hubURLAs(t, srv, tc.credsHost)
		}
		if tc.configHost != "" {
			s.config.HubEndpoint = hubURLAs(t, srv, tc.configHost)
		}
		conn, err := s.createHubConnection(tc.name, creds)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := firstRequest(t, conn.HubClient, seen).authorization; got != tc.want {
			t.Errorf("%s: Authorization %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A hub token that is the dev token (here: equal to SCION_DEV_TOKEN, the
// form a dev token from the settings or the token file takes) is only sent
// to a loopback hub; for any other hub nothing is sent and the not-sent line
// is logged. A hub token an operator configured is sent as before.
func TestCreateHubConnectionFromConfig_DevHubTokenOnlyForLoopbackHub(t *testing.T) {
	devTokenEnv(t)
	logs := captureLogs(t)

	cases := []struct {
		name     string
		host     string
		hubToken string
		want     string
	}{
		{"dev-remote", remoteHubHost, testDevToken, ""},
		{"dev-ipv4", "127.0.0.1", testDevToken, "Bearer " + testDevToken},
		{"dev-localhost", "localhost", testDevToken, "Bearer " + testDevToken},
		{"operator-remote", remoteHubHost, "made-up-hub-token", "Bearer made-up-hub-token"},
	}
	for _, tc := range cases {
		srv, seen := devAuthTestHubFor(t, tc.host)
		s := &Server{config: ServerConfig{HubEndpoint: hubURLAs(t, srv, tc.host), HubToken: tc.hubToken}}
		conn, err := s.createHubConnectionFromConfig()
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := firstRequest(t, conn.HubClient, seen).authorization; got != tc.want {
			t.Errorf("%s (dev hub token): Authorization %q, want %q", tc.name, got, tc.want)
		}
	}

	out := logs.String()
	if strings.Contains(out, testDevToken) {
		t.Errorf("logs contain the dev token:\n%s", out)
	}
	if n := strings.Count(out, "the dev token is used only for a local hub; it is not sent"); n != 1 {
		t.Errorf("not-sent log line appeared %d times, want 1 (the remote dev case):\n%s", n, out)
	}
}

// A hub token with the dev token prefix is a dev token even when it differs
// from the resolved one: it is only sent to a loopback hub.
func TestCreateHubConnectionFromConfig_PrefixedHubTokenOnlyForLoopbackHub(t *testing.T) {
	devTokenEnv(t)
	tok := apiclient.DevTokenPrefix + "made-up-0123"

	cases := []struct {
		name string
		host string
		want string
	}{
		{"prefixed-remote", remoteHubHost, ""},
		{"prefixed-localhost", "localhost", "Bearer " + tok},
	}
	for _, tc := range cases {
		srv, seen := devAuthTestHubFor(t, tc.host)
		s := &Server{config: ServerConfig{HubEndpoint: hubURLAs(t, srv, tc.host), HubToken: tok}}
		conn, err := s.createHubConnectionFromConfig()
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := firstRequest(t, conn.HubClient, seen).authorization; got != tc.want {
			t.Errorf("%s (prefixed hub token): Authorization %q, want %q", tc.name, got, tc.want)
		}
	}
}

// isDevAuthToken tells the dev token (prefix, or equal to the resolved dev
// token) from an operator-configured hub token.
func TestIsDevAuthToken(t *testing.T) {
	devTokenEnv(t)
	for tok, want := range map[string]bool{
		testDevToken:                           true,
		apiclient.DevTokenPrefix + "made-up-1": true,
		"made-up-hub-token":                    false,
		testDevToken + "x":                     false,
		"":                                     false,
	} {
		if got := isDevAuthToken(tok); got != want {
			t.Errorf("isDevAuthToken(%q) = %v, want %v", tok, got, want)
		}
	}
	// With no dev token resolvable, only the prefix marks a dev token.
	t.Setenv("SCION_DEV_TOKEN", "")
	if isDevAuthToken(testDevToken) {
		t.Errorf("isDevAuthToken(%q) = true with no dev token configured, want false", testDevToken)
	}
	if !isDevAuthToken(apiclient.DevTokenPrefix + "made-up-2") {
		t.Error("a prefixed token is a dev token even with no dev token configured")
	}
}
