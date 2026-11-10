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

package commands

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	core "github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport/ws"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/control"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

const pfWait = 10 * time.Second

// pfHub serves the conduit endpoint (or 404s it, like an older hub) and
// the legacy tunnel endpoint, recording which ones sciontool calls.
type pfHub struct {
	srv          *httptest.Server
	conduitHits  atomic.Int64
	tunnelHits   atomic.Int64
	tunnel       chan struct{}
	conduitAdmit chan struct{}
	hellos       chan *conduitv1.Hello
	sessions     chan core.Session
	refreshes    chan struct{}
	// authRefreshes carries the credential of every AuthRefresh the agent
	// sends; the hub accepts each.
	authRefreshes chan string
	// failRefresh answers the token refresh endpoint 503 while set.
	failRefresh atomic.Bool
	// dialTokens carries the agent token of every conduit dial.
	dialTokens chan string
	// refreshHits counts token refresh requests.
	refreshHits atomic.Int64

	statusMu     sync.Mutex
	statusBodies []map[string]any // bodies of status POSTs, in order
}

// statusPosts returns a copy of the recorded status POST bodies.
func (h *pfHub) statusPosts() []map[string]any {
	h.statusMu.Lock()
	defer h.statusMu.Unlock()
	return append([]map[string]any(nil), h.statusBodies...)
}

func newPFHub(t *testing.T, servesConduit bool) *pfHub {
	h := &pfHub{tunnel: make(chan struct{}, 16), conduitAdmit: make(chan struct{}, 16),
		hellos: make(chan *conduitv1.Hello, 16), sessions: make(chan core.Session, 16), refreshes: make(chan struct{}, 16),
		authRefreshes: make(chan string, 16), dialTokens: make(chan string, 16)}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/conduit", func(w http.ResponseWriter, r *http.Request) {
		h.conduitHits.Add(1)
		h.dialTokens <- r.Header.Get("X-Scion-Agent-Token")
		if !servesConduit {
			http.NotFound(w, r)
			return
		}
		conn, err := ws.Upgrade(w, r, nil, ws.Options{})
		if err != nil {
			return
		}
		s, err := core.Accept(r.Context(), conn, core.Config{}, pfAdmitter{admit: func(hello *conduitv1.Hello) *conduitv1.Welcome {
			h.hellos <- hello
			h.conduitAdmit <- struct{}{}
			return &conduitv1.Welcome{SessionId: "s1", RelayInstanceId: "r1", ConnectionEpoch: 1,
				EndpointIncarnation: hello.GetCapabilities().GetEndpointIncarnation()}
		}, refreshes: h.authRefreshes})
		if err != nil {
			return
		}
		h.sessions <- s
		<-s.(core.LocalSession).Done()
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/ports/tunnel") {
			h.tunnelHits.Add(1)
			h.tunnel <- struct{}{}
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/status") {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			h.statusMu.Lock()
			h.statusBodies = append(h.statusBodies, body)
			h.statusMu.Unlock()
			w.WriteHeader(http.StatusOK)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/token/refresh") {
			h.refreshHits.Add(1)
			h.refreshes <- struct{}{}
			if h.failRefresh.Load() {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
			exp := time.Now().Add(10 * time.Hour).UTC().Format(time.RFC3339)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":"token-2","expires_at":"` + exp + `"}`))
			return
		}
		http.Error(w, "no", http.StatusServiceUnavailable)
	})
	h.srv = httptest.NewServer(mux)
	t.Cleanup(h.srv.Close)
	return h
}

// pfAdmitter admits every Hello and accepts every AuthRefresh,
// reporting its credential.
type pfAdmitter struct {
	admit     func(*conduitv1.Hello) *conduitv1.Welcome
	refreshes chan<- string
}

func (a pfAdmitter) Admit(_ context.Context, h *conduitv1.Hello) (*conduitv1.Welcome, error) {
	return a.admit(h), nil
}

func (a pfAdmitter) Refresh(_ context.Context, ar *conduitv1.AuthRefresh) error {
	a.refreshes <- string(ar.GetCredential())
	return nil
}

func waitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(pfWait):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// TestPortForwardingConduitGate is the sciontool half of the old/new
// matrix. Without hub.conduit in SCION_HUB_EXPERIMENTS (an older hub, or
// hub.conduit off) sciontool runs only the legacy tunnel and never calls
// /api/v1/conduit; with it, it dials conduit and falls back to the tunnel
// on a 404. An older hub's SCION_HUB_CONDUIT is not read.
func TestPortForwardingConduitGate(t *testing.T) {
	tests := []struct {
		name           string
		experiments    string // SCION_HUB_EXPERIMENTS
		legacy         string // SCION_HUB_CONDUIT, set by older hubs
		disableConduit bool
		servesConduit  bool
		wantConduit    int64 // conduit attempts
		wantTunnel     bool
	}{
		{name: "old hub, no capability", servesConduit: false, wantTunnel: true},
		{name: "old hub, SCION_HUB_CONDUIT ignored", legacy: "true", servesConduit: true, wantTunnel: true},
		{name: "new hub, conduit off", servesConduit: true, wantTunnel: true},
		{name: "conduit not listed", experiments: "hub.k8s_nfs_home", servesConduit: true, wantTunnel: true},
		{name: "conduit name prefix only", experiments: "hub.conduit_x", servesConduit: true, wantTunnel: true},
		{name: "DisableConduit", experiments: conduit.ExperimentConduit, disableConduit: true, servesConduit: true, wantTunnel: true},
		{name: "capability but endpoint 404", experiments: conduit.ExperimentConduit, servesConduit: false, wantConduit: 1, wantTunnel: true},
		{name: "new hub, conduit on", experiments: conduit.ExperimentConduit, servesConduit: true, wantConduit: 1},
		{name: "conduit listed with others", experiments: "hub.other, hub.conduit", servesConduit: true, wantConduit: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newPFHub(t, tt.servesConduit)
			client := hub.NewClientWithConfig(h.srv.URL, "token-1", "agent-1")
			env := map[string]string{
				conduit.EnvHubExperiments: tt.experiments,
				"SCION_HUB_CONDUIT":       tt.legacy,
				projectkeys.EnvProjectID:  "project-1",
				conduit.EnvLaunchID:       "launch-1",
			}
			p := newPortForwarding(client, tt.disableConduit, conduit.PTYUser{}, nil, func(k string) string { return env[k] })
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { p.run(ctx); close(done) }()

			if tt.wantTunnel {
				waitSignal(t, h.tunnel, "the legacy tunnel")
			} else {
				waitSignal(t, h.conduitAdmit, "a conduit session")
			}
			cancel()
			select {
			case <-done:
			case <-time.After(pfWait):
				t.Fatal("run did not return after cancel")
			}
			if n := h.conduitHits.Load(); n != tt.wantConduit {
				t.Fatalf("/api/v1/conduit called %d times, want %d", n, tt.wantConduit)
			}
			if !tt.wantTunnel && h.tunnelHits.Load() != 0 {
				t.Fatalf("legacy tunnel dialed %d times while conduit was up", h.tunnelHits.Load())
			}
		})
	}
}

// TestPortForwardingConduitControl: with conduit on, the agent session
// advertises exactly the control routes, a hub-side rotate-token call is
// answered 202 and wakes the token refresh loop, and paths outside
// /v1/control/ are 404.
func TestPortForwardingConduitControl(t *testing.T) {
	t.Cleanup(hub.SetTokenHome(t.TempDir()))
	h := newPFHub(t, true)
	client := hub.NewClientWithConfig(h.srv.URL, "token-1", "agent-1")
	env := map[string]string{
		conduit.EnvHubExperiments: conduit.ExperimentConduit,
		projectkeys.EnvProjectID:  "project-1",
		conduit.EnvLaunchID:       "launch-1",
	}
	ctx, cancel := context.WithCancel(context.Background())
	refreshDone := client.StartTokenRefresh(ctx, &hub.TokenRefreshConfig{RefreshAt: time.Now().Add(5 * time.Hour)})
	p := newPortForwarding(client, false, conduit.PTYUser{}, nil, func(k string) string { return env[k] })
	done := make(chan struct{})
	go func() { p.run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		<-done
		<-refreshDone
	})

	var hello *conduitv1.Hello
	select {
	case hello = <-h.hellos:
	case <-time.After(pfWait):
		t.Fatal("no conduit Hello")
	}
	if got := hello.GetCapabilities().GetRpc(); len(got) != 1 || got[0] != control.RouteRotateToken {
		t.Fatalf("Hello rpc = %v, want [%s]", got, control.RouteRotateToken)
	}
	var s core.Session
	select {
	case s = <-h.sessions:
	case <-time.After(pfWait):
		t.Fatal("no conduit session")
	}

	for _, tt := range []struct {
		method, path string
		want         int32
	}{
		{"GET", "/v1/control/rotate-token", 405},
		{"POST", "/v1/control/wake", 404},
		{"POST", "/scion/v1/exec", 404},
		{"POST", "/v1/control/rotate-token", 202},
	} {
		resp, err := s.Call(ctx, &conduitv1.RpcRequest{Method: tt.method, Path: tt.path})
		if err != nil {
			t.Fatalf("%s %s: %v", tt.method, tt.path, err)
		}
		if resp.GetStatus() != tt.want || (tt.want == 202 && len(resp.GetBody()) != 0) {
			t.Fatalf("%s %s: status %d body %q, want %d", tt.method, tt.path, resp.GetStatus(), resp.GetBody(), tt.want)
		}
	}
	waitSignal(t, h.refreshes, "the token refresh woken by rotate-token")
}

// TestPortForwardingConduitAuthRefresh: each successful refresh of the
// loop is sent on the live session as AuthRefresh with the new token,
// and the session stays up; a failed refresh sends nothing.
func TestPortForwardingConduitAuthRefresh(t *testing.T) {
	t.Cleanup(hub.SetTokenHome(t.TempDir()))
	h := newPFHub(t, true)
	client := hub.NewClientWithConfig(h.srv.URL, "token-1", "agent-1")
	env := map[string]string{
		conduit.EnvHubExperiments: conduit.ExperimentConduit,
		projectkeys.EnvProjectID:  "project-1",
		conduit.EnvLaunchID:       "launch-1",
	}
	ctx, cancel := context.WithCancel(context.Background())
	results := make(chan hub.TokenRefreshResult, 8)
	unsubscribe := client.SubscribeTokenRefresh(func(r hub.TokenRefreshResult) { results <- r })
	t.Cleanup(unsubscribe)
	refreshDone := client.StartTokenRefresh(ctx, &hub.TokenRefreshConfig{
		RefreshAt:      time.Now().Add(5 * time.Hour),
		RetryBaseDelay: time.Hour, // a failure backs off past the test
	})
	p := newPortForwarding(client, false, conduit.PTYUser{}, nil, func(k string) string { return env[k] })
	done := make(chan struct{})
	go func() { p.run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		<-done
		<-refreshDone
	})
	var s core.Session
	select {
	case s = <-h.sessions:
	case <-time.After(pfWait):
		t.Fatal("no conduit session")
	}
	nextResult := func() hub.TokenRefreshResult {
		t.Helper()
		select {
		case r := <-results:
			return r
		case <-time.After(pfWait):
			t.Fatal("no refresh result")
			return hub.TokenRefreshResult{}
		}
	}

	// Kicks until the agent has seen its session: until then a refreshed
	// token has no session to go to (the next dial presents it).
	var got string
	deadline := time.After(pfWait)
	for got == "" {
		client.KickTokenRefresh()
		if r := nextResult(); r.Err != nil {
			t.Fatalf("refresh failed: %v", r.Err)
		}
		select {
		case got = <-h.authRefreshes:
		case <-time.After(100 * time.Millisecond):
		case <-deadline:
			t.Fatal("no AuthRefresh after a successful refresh")
		}
	}
	if got != "token-2" {
		t.Fatalf("AuthRefresh credential = %q, want token-2", got)
	}

	// A failed refresh: the loop reports the error and nothing is sent.
	h.failRefresh.Store(true)
	client.KickTokenRefresh()
	if r := nextResult(); r.Err == nil {
		t.Fatal("refresh succeeded, want 503")
	}
	select {
	case cred := <-h.authRefreshes:
		t.Fatalf("AuthRefresh %q sent after a failed refresh", cred)
	case <-time.After(200 * time.Millisecond):
	}
	if n := h.conduitHits.Load(); n != 1 {
		t.Fatalf("conduit dialed %d times, want 1 (the session stays up)", n)
	}
	select {
	case <-s.(core.LocalSession).Done():
		t.Fatal("hub-side session ended")
	default:
	}
}

// TestPortForwardingConduit4401WaitsForRefreshLoop: after the hub closes
// the session with 4401 the reconnect does not refresh the token itself.
// With no refresh loop running it makes no refresh request; once a loop
// starts, the reconnect's pending kick runs exactly one refresh, and the
// redial presents the token that refresh produced.
func TestPortForwardingConduit4401WaitsForRefreshLoop(t *testing.T) {
	t.Cleanup(hub.SetTokenHome(t.TempDir()))
	h := newPFHub(t, true)
	client := hub.NewClientWithConfig(h.srv.URL, "token-1", "agent-1")
	env := map[string]string{
		conduit.EnvHubExperiments: conduit.ExperimentConduit,
		projectkeys.EnvProjectID:  "project-1",
		conduit.EnvLaunchID:       "launch-1",
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := newPortForwarding(client, false, conduit.PTYUser{}, nil, func(k string) string { return env[k] })
	done := make(chan struct{})
	go func() { p.run(ctx); close(done) }()
	refreshDone := make(chan (<-chan struct{}), 1)
	t.Cleanup(func() {
		cancel()
		<-done
		select {
		case d := <-refreshDone:
			<-d
		default:
		}
	})
	nextDialToken := func() string {
		t.Helper()
		select {
		case tok := <-h.dialTokens:
			return tok
		case <-time.After(pfWait):
			t.Fatal("no conduit dial")
			return ""
		}
	}
	if tok := nextDialToken(); tok != "token-1" {
		t.Fatalf("first dial token = %q, want token-1", tok)
	}
	var s core.Session
	select {
	case s = <-h.sessions:
	case <-time.After(pfWait):
		t.Fatal("no conduit session")
	}
	if err := s.(core.LocalSession).CloseWithCode(core.CloseUnauthenticated, "unauthenticated"); err != nil {
		t.Fatal(err)
	}

	// No loop runs: a reconnect that refreshed by itself would call the
	// refresh endpoint now.
	select {
	case <-h.refreshes:
		t.Fatal("the reconnect called the refresh endpoint itself")
	case tok := <-h.dialTokens:
		t.Fatalf("redialed with %q before any refresh completed", tok)
	case <-time.After(500 * time.Millisecond):
	}

	// The loop starts and serves the pending kick.
	refreshDone <- client.StartTokenRefresh(ctx, &hub.TokenRefreshConfig{RefreshAt: time.Now().Add(5 * time.Hour)})
	waitSignal(t, h.refreshes, "the refresh run by the reconnect's kick")
	if tok := nextDialToken(); tok != "token-2" {
		t.Fatalf("redial token = %q, want token-2 (the loop's refresh)", tok)
	}
	if n := h.refreshHits.Load(); n != 1 {
		t.Fatalf("refresh requests = %d, want 1", n)
	}
}

// TestConduitPTYUser: PTY tmux clients run as the harness identity.
func TestConduitPTYUser(t *testing.T) {
	tests := []struct {
		name               string
		uid, gid           int
		rootless, required bool
		want               conduit.PTYUser
	}{
		{"privilege drop", 1000, 1000, false, true, conduit.PTYUser{UID: 1000, GID: 1000, Username: "scion", RequirePrivilegeDrop: true}},
		{"rootless", 0, 0, true, false, conduit.PTYUser{Username: "scion"}},
		{"plain root", 0, 0, false, false, conduit.PTYUser{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := conduitPTYUser(tt.uid, tt.gid, tt.rootless, tt.required); got != tt.want {
				t.Fatalf("conduitPTYUser = %+v, want %+v", got, tt.want)
			}
		})
	}
}
