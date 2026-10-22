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

package hubsync

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// authRecordingHub starts a hub on 127.0.0.1 that records the Authorization
// header of every request.
func authRecordingHub(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
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

// nonLoopbackTestHost is a host name that is not a loopback hub for the dev
// credential rule; tests reach a local fake hub under it (hostnameURL).
const nonLoopbackTestHost = "hub.example.test"

// hostnameURL returns the URL of srv under nonLoopbackTestHost. For the test,
// http.DefaultTransport (which the hub client uses) dials that host and port
// to srv's own address, with no proxy, so a non-loopback hub is real to the
// code under test without binding any address besides 127.0.0.1.
func hostnameURL(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	addr := srv.Listener.Addr().String()
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	hostPort := net.JoinHostPort(nonLoopbackTestHost, port)
	orig := http.DefaultTransport
	base, ok := orig.(*http.Transport)
	if !ok {
		t.Fatal("http.DefaultTransport is not an *http.Transport")
	}
	tr := base.Clone()
	tr.Proxy = nil
	var d net.Dialer
	tr.DialContext = func(ctx context.Context, network, a string) (net.Conn, error) {
		if a == hostPort {
			a = addr
		}
		return d.DialContext(ctx, network, a)
	}
	http.DefaultTransport = tr
	t.Cleanup(func() { http.DefaultTransport = orig })
	return "http://" + hostPort
}

// devTokenOnlyEnv leaves a dev token as the only credential.
func devTokenOnlyEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	for _, name := range []string{"SCION_AUTH_TOKEN", "SCION_HUB_TOKEN", "SCION_DEV_TOKEN_FILE", "SCION_HOST_UID", "SCION_AGENT_ID"} {
		t.Setenv(name, "")
	}
	t.Setenv("SCION_DEV_TOKEN", "placeholder-dev-token")
}

// With only a dev token, a hub that is not on a loopback address gets no
// credential; a loopback hub gets the dev token.
func TestSelectHubCredential_DevTokenOnlyForLoopback(t *testing.T) {
	devTokenOnlyEnv(t)
	for _, ep := range []string{"https://hub.example.com", "http://127.0.0.2:8080", "http://10.0.0.5:8080"} {
		opt, kind := selectHubCredential(ep)
		if opt != nil || kind != CredentialKindNone {
			t.Errorf("selectHubCredential(%q) = %v, %q; want no credential", ep, opt != nil, kind)
		}
	}
	for _, ep := range []string{"http://127.0.0.1:8080", "http://localhost:8080", "http://[::1]:8080"} {
		opt, kind := selectHubCredential(ep)
		if opt == nil || kind != CredentialKindDevAuto {
			t.Errorf("selectHubCredential(%q) = %v, %q; want the dev token", ep, opt != nil, kind)
		}
	}
}

// End to end: the client createHubClient builds sends the dev token to a
// loopback hub and nothing to a hub under a non-loopback host name.
func TestCreateHubClient_DevAuthOnlyForLoopbackHub(t *testing.T) {
	devTokenOnlyEnv(t)
	other, otherSeen := authRecordingHub(t)
	local, localSeen := authRecordingHub(t)

	for _, endpoint := range []string{hostnameURL(t, other), local.URL} {
		client, _, err := createHubClient(nil, endpoint)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = client.Auth().Me(context.Background())
	}
	got := otherSeen()
	if len(got) == 0 {
		t.Fatal("the non-loopback hub received no request")
	}
	if got[0] != "" {
		t.Errorf("non-loopback hub got Authorization %q, want none", got[0])
	}
	got = localSeen()
	if len(got) == 0 || got[0] != "Bearer placeholder-dev-token" {
		t.Errorf("loopback hub got Authorization %q, want the dev token", got)
	}
}

// IsLocalhostEndpoint is the loopback rule for dev credentials.
func TestIsLocalhostEndpoint_LoopbackRule(t *testing.T) {
	for ep, want := range map[string]bool{
		"http://localhost:8080":   true,
		"http://127.0.0.1:8080":   true,
		"http://[::1]:8080":       true,
		"http://127.0.0.2:8080":   false,
		"http://0.0.0.0:8080":     false,
		"https://hub.example.com": false,
		"http://localhost.:8080":  false,
		"":                        false,
	} {
		if got := IsLocalhostEndpoint(ep); got != want {
			t.Errorf("IsLocalhostEndpoint(%q) = %v, want %v", ep, got, want)
		}
	}
}
