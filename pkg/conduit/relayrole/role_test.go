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

package relayrole

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay/relaytest"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport/ws"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

const (
	testAgentID = "agent-1"
	testProject = "proj-1"
)

// roleFixture is a relay fronted by a Role, with a fake hub behind it.
type roleFixture struct {
	w     *relaytest.World
	node  *relaytest.Node
	role  *Role
	hub   *fakeHub
	clk   *clock.Fake
	front *httptest.Server
}

func newRoleFixture(t *testing.T) *roleFixture {
	t.Helper()
	return newRoleFixtureWith(t, nil)
}

// newRoleFixtureWith is newRoleFixture with mod applied to the relay's
// configuration (nil: none).
func newRoleFixtureWith(t *testing.T, mod func(*relay.Config)) *roleFixture {
	t.Helper()
	f := &roleFixture{w: relaytest.NewWorld(t), hub: newFakeHub(t), clk: clock.NewFake(time.Now())}
	ro, err := New(Config{Hub: f.hub.client(), Clock: f.clk, RecheckInterval: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	f.role = ro
	f.node = f.w.StartNode("relay-0", func(c *relay.Config) {
		c.Refresh = ro.Refresh
		if mod != nil {
			mod(c)
		}
	})
	ro.Bind(f.node.Relay)
	f.front = httptest.NewServer(ro)
	t.Cleanup(f.front.Close)
	return f
}

// admitAs makes the fake hub admit every dialer as an agent (or user)
// with token, and answer everything else with answer (nil: 503).
func (f *roleFixture) admitAs(kind, token string, answer func(path string, body []byte) hubAnswer) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	f.hub.setAnswer(func(path string, body []byte) hubAnswer {
		if path == "admit" {
			resp := relay.HubAdmitResponse{AdmissionToken: token, PrincipalKind: kind, PrincipalID: testAgentID,
				GrantKeys: []relay.HubGrantKey{{KID: "k-hub", PublicKey: pub}}}
			if kind == registry.PrincipalAgent {
				resp.ProjectID, resp.AgentLaunchID, resp.AgentGeneration = testProject, "L1", 1
			} else {
				resp.PrincipalID = "user-1"
			}
			return hubAnswer{status: http.StatusOK, body: resp}
		}
		if answer != nil {
			return answer(path, body)
		}
		return hubAnswer{status: http.StatusServiceUnavailable}
	})
}

// dial opens a conduit session through the front.
func (f *roleFixture) dial(t *testing.T, hello *conduitv1.Hello, cfg conduit.Config) (conduit.LocalSession, *conduitv1.Welcome, error) {
	t.Helper()
	if cfg.Clock == nil {
		cfg.Clock = clock.Real()
	}
	d := &ws.Dialer{
		URL: "ws" + strings.TrimPrefix(f.front.URL, "http") + ConduitPath,
		Header: func(context.Context) (http.Header, error) {
			return http.Header{"Authorization": {"Bearer dialer-credential"}, "X-Forwarded-User-Id": {"spoofed"}}, nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, wel, err := conduit.Dial(ctx, d, cfg, hello)
	if err != nil {
		return nil, nil, err
	}
	ls := s.(conduit.LocalSession)
	t.Cleanup(func() { _ = ls.Close() })
	return ls, wel, nil
}

func (f *roleFixture) mustDialAgent(t *testing.T) (conduit.LocalSession, *conduitv1.Welcome) {
	t.Helper()
	ls, wel, err := f.dial(t, relaytest.AgentHello(testAgentID, "L1", "", "pty"), conduit.Config{})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return ls, wel
}

func waitEnded(t *testing.T, ls conduit.LocalSession) error {
	t.Helper()
	relaytest.WaitClosed(t, ls.Done(), "session end")
	return ls.Err()
}

func closeCode(err error) uint32 {
	var ce *conduit.CloseError
	if errors.As(err, &ce) {
		return ce.Code
	}
	return 0
}

// TestFrontAdmitsThroughHub (design §3.9 "Admission"): the relay asks the
// hub to admit the dialer with its credential headers (allowlisted only),
// the host it received and the session id it serves the session under,
// and the Welcome carries the hub's grant keys.
func TestFrontAdmitsThroughHub(t *testing.T) {
	f := newRoleFixture(t)
	f.admitAs(registry.PrincipalAgent, "tok-1", nil)
	_, wel := f.mustDialAgent(t)

	var req relay.HubAdmitRequest
	f.hub.last("admit", &req)
	if req.SessionID != wel.GetSessionId() || req.RelayInstanceID != "relay-0" || req.RelayGeneration != f.node.Relay.Generation() {
		t.Fatalf("admit request %+v for session %s", req, wel.GetSessionId())
	}
	if req.Host != strings.TrimPrefix(f.front.URL, "http://") {
		t.Fatalf("admit host %q", req.Host)
	}
	if got := req.Headers["Authorization"]; len(got) != 1 || got[0] != "Bearer dialer-credential" {
		t.Fatalf("credential header not forwarded: %v", req.Headers)
	}
	if _, ok := req.Headers["X-Forwarded-User-Id"]; ok {
		t.Fatal("a proxy identity header was forwarded")
	}
	if keys := wel.GetGrantKeys(); len(keys) != 1 || keys[0].GetKid() != "k-hub" {
		t.Fatalf("Welcome grant keys %v, want the hub's", keys)
	}
	ps := f.w.Sessions(registry.PrincipalAgent, testAgentID)
	if len(ps.Sessions) != 1 || ps.Sessions[0].Session.SessionID != wel.GetSessionId() {
		t.Fatalf("registry rows %+v", ps.Sessions)
	}
}

// rawDial sends a WebSocket upgrade request to the front and returns the
// HTTP answer (a refused upgrade).
func (f *roleFixture) rawDial(t *testing.T) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, f.front.URL+ConduitPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestFrontHubRefusalPassedOn: a hub refusal reaches the dialer unchanged.
func TestFrontHubRefusalPassedOn(t *testing.T) {
	f := newRoleFixture(t)
	body := map[string]any{"error": map[string]string{"code": "unauthorized", "message": "Agent no longer exists"}}
	f.hub.setAnswer(func(string, []byte) hubAnswer { return hubAnswer{status: http.StatusUnauthorized, body: body} })
	status, got := f.rawDial(t)
	if status != http.StatusUnauthorized || !strings.Contains(got, "Agent no longer exists") {
		t.Fatalf("answer %d %s, want the hub's 401", status, got)
	}
}

// TestFrontHubUnreachable503 (design §3.9): with the hub unreachable the
// relay answers 503 unavailable, never 401 or 403.
func TestFrontHubUnreachable503(t *testing.T) {
	f := newRoleFixture(t)
	f.hub.srv.Close()
	status, got := f.rawDial(t)
	if status != http.StatusServiceUnavailable || !strings.Contains(got, relay.ReasonUpstreamUnreachable) {
		t.Fatalf("answer %d %s, want 503 %s", status, got, relay.ReasonUpstreamUnreachable)
	}
}

// TestFrontNotAdmittingRefuses: with admission stopped the relay refuses
// dials without asking the hub.
func TestFrontNotAdmittingRefuses(t *testing.T) {
	f := newRoleFixture(t)
	f.admitAs(registry.PrincipalAgent, "tok-1", nil)
	f.node.Relay.SetAdmitting(false)
	status, got := f.rawDial(t)
	if status != http.StatusServiceUnavailable || !strings.Contains(got, relay.ReasonRelayNotAdmitting) {
		t.Fatalf("answer %d %s", status, got)
	}
	if n := f.hub.count("admit"); n != 0 {
		t.Fatalf("the hub was asked to admit %d times", n)
	}
}

func sessionRecheckAnswer(close bool, code uint32, token string) func(string, []byte) hubAnswer {
	return func(path string, body []byte) hubAnswer {
		if path != "sessions/recheck" {
			return hubAnswer{status: http.StatusServiceUnavailable}
		}
		var req relay.HubSessionRecheckRequest
		_ = json.Unmarshal(body, &req)
		res := make([]relay.HubSessionRecheckResult, len(req.Items))
		for i, it := range req.Items {
			res[i] = relay.HubSessionRecheckResult{SessionID: it.SessionID, OK: !close, Close: close, CloseCode: code, AdmissionToken: token}
		}
		return hubAnswer{status: http.StatusOK, body: relay.HubSessionRecheckResponse{Results: res}}
	}
}

// TestSessionRecheckClosesWithHubCode (criteria 16b, 16c): a session the
// hub's re-check closes ends with the hub's code (4404 for a deleted
// agent), with the admission token presented unchanged.
func TestSessionRecheckClosesWithHubCode(t *testing.T) {
	f := newRoleFixture(t)
	f.admitAs(registry.PrincipalAgent, "tok-1", sessionRecheckAnswer(true, relay.CloseTargetNotFound, ""))
	ls, _ := f.mustDialAgent(t)
	f.role.RecheckSessions(context.Background(), TriggerNotify)
	var req relay.HubSessionRecheckRequest
	f.hub.last("sessions/recheck", &req)
	if len(req.Items) != 1 || req.Items[0].AdmissionToken != "tok-1" {
		t.Fatalf("re-check items %+v", req.Items)
	}
	if code := closeCode(waitEnded(t, ls)); code != relay.CloseTargetNotFound {
		t.Fatalf("session closed with %d, want 4404", code)
	}
}

// TestSessionRecheckMissedNotifySweepCloses (criterion 16b): with no event
// delivered, the periodic sweep alone closes a revoked session.
func TestSessionRecheckMissedNotifySweepCloses(t *testing.T) {
	f := newRoleFixture(t)
	f.admitAs(registry.PrincipalAgent, "tok-1", sessionRecheckAnswer(true, conduit.CloseUnauthenticated, ""))
	ls, _ := f.mustDialAgent(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.role.Start(ctx)
	f.clk.Advance(time.Minute)
	if code := closeCode(waitEnded(t, ls)); code != conduit.CloseUnauthenticated {
		t.Fatalf("session closed with %d, want 4401", code)
	}
}

// TestSessionRecheckUnavailableKeeps: when the hub cannot be reached, a
// re-check closes nothing.
func TestSessionRecheckUnavailableKeeps(t *testing.T) {
	f := newRoleFixture(t)
	f.admitAs(registry.PrincipalAgent, "tok-1", nil) // re-checks get 503
	ls, _ := f.mustDialAgent(t)
	f.role.RecheckSessions(context.Background(), TriggerSweep)
	if n := f.hub.count("sessions/recheck"); n != 1 {
		t.Fatalf("%d re-check calls, want 1", n)
	}
	assertOpen(t, ls)
}

// assertOpen checks that ls is still open: a round trip through the relay
// after the action under test (a close the relay sent before it arrives
// first), then a non-blocking check of Done.
func assertOpen(t *testing.T, ls conduit.LocalSession) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := ls.Call(ctx, &conduitv1.RpcRequest{RequestId: "round-trip", Method: http.MethodGet, Path: "/v1/round-trip"}); err != nil {
		t.Fatalf("round trip: %v (session err %v)", err, ls.Err())
	}
	select {
	case <-ls.Done():
		t.Fatalf("session closed: %v", ls.Err())
	default:
	}
}

// TestRefreshForwarded (criterion 30): an in-band credential refresh on a
// relay-held session is checked by the hub with the session's admission
// token; an accepted refresh replaces the token, a refused one closes the
// session with 4401, and a hub outage closes nothing.
func TestRefreshForwarded(t *testing.T) {
	refreshWith := func(status int, token string) func(string, []byte) hubAnswer {
		return func(path string, body []byte) hubAnswer {
			switch path {
			case "refresh":
				return hubAnswer{status: status, body: relay.HubRefreshResponse{AdmissionToken: token}}
			case "sessions/recheck":
				return sessionRecheckAnswer(false, 0, "")(path, body)
			}
			return hubAnswer{status: http.StatusServiceUnavailable}
		}
	}

	t.Run("accepted", func(t *testing.T) {
		f := newRoleFixture(t)
		f.admitAs(registry.PrincipalAgent, "tok-1", refreshWith(http.StatusOK, "tok-2"))
		ls, wel := f.mustDialAgent(t)
		if err := ls.RefreshAuth([]byte("new-credential"), 0); err != nil {
			t.Fatal(err)
		}
		f.hub.waitCalls("refresh", 1)
		var req relay.HubRefreshRequest
		f.hub.last("refresh", &req)
		if req.AdmissionToken != "tok-1" || string(req.Credential) != "new-credential" {
			t.Fatalf("refresh request %+v", req)
		}
		// Refreshes are checked one at a time: the second reaches the hub
		// only after the first was handled, and presents its new token.
		if err := ls.RefreshAuth([]byte("newer-credential"), 0); err != nil {
			t.Fatal(err)
		}
		f.hub.waitCalls("refresh", 2)
		f.hub.last("refresh", &req)
		if req.AdmissionToken != "tok-2" {
			t.Fatalf("second refresh presented %q, want the replaced token", req.AdmissionToken)
		}
		if got := f.role.token(wel.GetSessionId()); got != "tok-2" {
			t.Fatalf("held token %q", got)
		}
	})

	t.Run("refused", func(t *testing.T) {
		f := newRoleFixture(t)
		f.admitAs(registry.PrincipalAgent, "tok-1", refreshWith(http.StatusUnauthorized, ""))
		ls, _ := f.mustDialAgent(t)
		_ = ls.RefreshAuth([]byte("bad-credential"), 0)
		if code := closeCode(waitEnded(t, ls)); code != conduit.CloseUnauthenticated {
			t.Fatalf("session closed with %d, want 4401", code)
		}
	})

	t.Run("hub unavailable", func(t *testing.T) {
		f := newRoleFixture(t)
		f.admitAs(registry.PrincipalAgent, "tok-1", refreshWith(http.StatusServiceUnavailable, ""))
		ls, _ := f.mustDialAgent(t)
		_ = ls.RefreshAuth([]byte("new-credential"), 0)
		f.hub.waitCalls("refresh", 1)
		// A refusal would close the session and stop refreshing; a second
		// refresh reaching the hub shows the first was handled and kept it.
		_ = ls.RefreshAuth([]byte("newer-credential"), 0)
		f.hub.waitCalls("refresh", 2)
		assertOpen(t, ls)
	})
}

// TestRPCForwardedForUserSession (design §3.9 "RPCs"): an RPC a user sends
// on a relay-held session goes to the hub with the session's admission
// token, and the hub's answer comes back unchanged.
func TestRPCForwardedForUserSession(t *testing.T) {
	f := newRoleFixture(t)
	f.admitAs(registry.PrincipalUser, "tok-user", func(path string, body []byte) hubAnswer {
		if path != "rpc" {
			return hubAnswer{status: http.StatusServiceUnavailable}
		}
		return hubAnswer{status: http.StatusOK, body: relay.HubRPCResponse{RPC: mustMarshalRPC(t, &conduitv1.RpcResponse{RequestId: "r1", Status: 200, Body: []byte("from hub")})}}
	})
	ls, _, err := f.dial(t, relaytest.UserHello("user-1"), conduit.Config{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := ls.Call(ctx, &conduitv1.RpcRequest{RequestId: "r1", Method: http.MethodPost, Path: "/v1/tunnels"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetStatus() != 200 || string(resp.GetBody()) != "from hub" {
		t.Fatalf("RPC answer %v", resp)
	}
	var req relay.HubRPCRequest
	f.hub.last("rpc", &req)
	if req.AdmissionToken != "tok-user" {
		t.Fatalf("forwarded with token %q", req.AdmissionToken)
	}
}
