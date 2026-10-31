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
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	goast "go/ast"
	goparser "go/parser"
	gotoken "go/token"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// brokerConnTestHub starts a fake hub on 127.0.0.1 that records the
// Authorization and broker ID headers of every request, as
// "<authorization>|<broker id>".
func brokerConnTestHub(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	return brokerConnTestHubWith(t, httptest.NewServer)
}

// brokerConnRemoteHost is the non-loopback name a remote hub gets in these
// tests: one of the names in httptest's TLS certificate.
const brokerConnRemoteHost = "example.com"

// brokerConnTestHubFor is brokerConnTestHub for a hub addressed as host: a
// plain http hub for a loopback host, a TLS hub otherwise (a plain http hub
// on a network host is refused by config.CheckHubTransport).
func brokerConnTestHubFor(t *testing.T, host string) (*httptest.Server, func() []string) {
	t.Helper()
	if config.IsLoopbackHost(host) {
		return brokerConnTestHub(t)
	}
	return brokerConnTestHubWith(t, httptest.NewTLSServer)
}

func brokerConnTestHubWith(t *testing.T, newServer func(http.Handler) *httptest.Server) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	srv := newServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization")+"|"+r.Header.Get(apiclient.HeaderBrokerID))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

// brokerConnHubURLAs returns srv's URL under host, and makes
// http.DefaultTransport dial that host and port to srv's own address for the
// rest of the test, with no proxy.
func brokerConnHubURLAs(t *testing.T, srv *httptest.Server, host string) string {
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

// writeBrokerConnection stores creds as a named hub connection under home.
func writeBrokerConnection(t *testing.T, home string, creds brokercredentials.BrokerCredentials) {
	t.Helper()
	dir := filepath.Join(home, ".scion", brokercredentials.DefaultMultiDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(creds)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, creds.Name+".json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// The client 'scion broker' builds for a named hub connection sends the dev
// token (dev-auth mode, or no secret key) only to a loopback hub, and keeps
// HMAC authentication when the connection has a secret key.
func TestGetHubClientForConnection_DevTokenOnlyForLoopbackHub(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, name := range []string{
		"SCION_DEV_TOKEN_FILE", "SCION_AUTH_TOKEN", "SCION_HUB_TOKEN",
		"SCION_TRANSPORT_MODE", "SCION_TRANSPORT_AUDIENCE", "SCION_DEBUG",
	} {
		t.Setenv(name, "")
	}
	t.Setenv("SCION_DEV_TOKEN", "placeholder-dev-token")
	secret := base64.StdEncoding.EncodeToString([]byte("made-up-secret"))

	cases := []struct {
		name   string
		host   string
		mode   brokercredentials.AuthMode
		secret string
		want   string // "<authorization>|<broker id>" the hub must see
	}{
		{"devauth-ipv4", "127.0.0.1", brokercredentials.AuthModeDevAuth, secret, "Bearer placeholder-dev-token|"},
		{"devauth-localhost", "localhost", brokercredentials.AuthModeDevAuth, secret, "Bearer placeholder-dev-token|"},
		{"devauth-ipv6", "::1", brokercredentials.AuthModeDevAuth, secret, "Bearer placeholder-dev-token|"},
		{"devauth-remote", brokerConnRemoteHost, brokercredentials.AuthModeDevAuth, secret, "|"},
		{"nosecret-local", "127.0.0.1", "", "", "Bearer placeholder-dev-token|"},
		{"nosecret-remote", brokerConnRemoteHost, "", "", "|"},
		{"hmac-remote", brokerConnRemoteHost, "", secret, "|rb-test"},
	}
	for _, tc := range cases {
		srv, seen := brokerConnTestHubFor(t, tc.host)
		endpoint := brokerConnHubURLAs(t, srv, tc.host)
		writeBrokerConnection(t, home, brokercredentials.BrokerCredentials{
			Name: tc.name, BrokerID: "rb-test", SecretKey: tc.secret, HubEndpoint: endpoint, AuthMode: tc.mode,
		})
		client, err := getHubClientForConnection(tc.name)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		_, _ = client.Auth().Me(context.Background())
		got := seen()
		if len(got) == 0 {
			t.Fatalf("%s: the hub received no request", tc.name)
		}
		if got[0] != tc.want {
			t.Errorf("%s: hub saw %q, want %q", tc.name, got[0], tc.want)
		}
	}
}

// A stored connection naming a plain http hub on a network host (credentials
// written before the transport rule) gets no client: nothing is sent.
func TestGetHubClientForConnection_RefusesPlaintextNonLoopbackHub(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	srv, seen := brokerConnTestHub(t)
	endpoint := brokerConnHubURLAs(t, srv, "hub.example.test")
	writeBrokerConnection(t, home, brokercredentials.BrokerCredentials{
		Name: "old-plaintext", BrokerID: "rb-test", SecretKey: base64.StdEncoding.EncodeToString([]byte("made-up-secret")), HubEndpoint: endpoint,
	})
	client, err := getHubClientForConnection("old-plaintext")
	if !errors.Is(err, config.ErrPlaintextNonLoopback) {
		t.Fatalf("getHubClientForConnection error = %v, want config.ErrPlaintextNonLoopback", err)
	}
	if client != nil {
		t.Error("a client was returned for a refused hub")
	}
	if got := seen(); len(got) != 0 {
		t.Errorf("the hub received %q, want nothing", got)
	}
}

// brokerConfigSources are the files that build the broker's
// runtimebroker.ServerConfig, embedded so the check below reads exactly the
// sources the test binary was built from.
//
//go:embed server_foreground.go server_flat_host.go
var brokerConfigSources embed.FS

// The broker server configurations the CLI builds never set HubToken. That
// field is an operator-configured bearer; in dev-auth mode the broker's hub
// connection gets the dev token only through runtimebroker.DevAuthHubOptions,
// which sends it only to a loopback hub. The check parses the files that
// build those configurations and inspects each runtimebroker.ServerConfig
// literal.
func TestBrokerServerConfigsSetNoHubToken(t *testing.T) {
	fset := gotoken.NewFileSet()
	for _, f := range []string{"server_foreground.go", "server_flat_host.go"} {
		src, err := brokerConfigSources.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		af, err := goparser.ParseFile(fset, f, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		literals := 0
		goast.Inspect(af, func(n goast.Node) bool {
			cl, ok := n.(*goast.CompositeLit)
			if !ok {
				return true
			}
			sel, ok := cl.Type.(*goast.SelectorExpr)
			if !ok || sel.Sel.Name != "ServerConfig" {
				return true
			}
			if pkg, ok := sel.X.(*goast.Ident); !ok || pkg.Name != "runtimebroker" {
				return true
			}
			literals++
			for _, e := range cl.Elts {
				kv, ok := e.(*goast.KeyValueExpr)
				if !ok {
					continue
				}
				if key, ok := kv.Key.(*goast.Ident); ok && key.Name == "HubToken" {
					t.Errorf("%s: runtimebroker.ServerConfig sets HubToken; the dev token reaches the hub only through runtimebroker.DevAuthHubOptions", fset.Position(kv.Pos()))
				}
			}
			return true
		})
		if literals == 0 {
			t.Errorf("no runtimebroker.ServerConfig literal found in %s: the check would pass vacuously", f)
		}
	}
}
