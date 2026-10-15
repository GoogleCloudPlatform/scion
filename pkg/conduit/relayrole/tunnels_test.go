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
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay/relaytest"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// echoTarget is an agent session config that accepts every stream, records
// its StreamOpen and id, and echoes the stream's bytes.
type echoTarget struct {
	mu     sync.Mutex
	opens  []*conduitv1.StreamOpen
	ids    chan uint32
	config conduit.Config
}

func newEchoTarget() *echoTarget {
	e := &echoTarget{ids: make(chan uint32, 8)}
	e.config = conduit.Config{StreamHandler: conduit.StreamHandlerFunc(func(_ context.Context, open *conduitv1.StreamOpen, ps conduit.PendingStream) error {
		st, err := ps.Accept()
		if err != nil {
			return err
		}
		e.mu.Lock()
		e.opens = append(e.opens, open)
		e.mu.Unlock()
		e.ids <- st.ID()
		go func() {
			_, _ = io.Copy(st, st)
			_ = st.Close()
		}()
		return nil
	})}
	return e
}

func (e *echoTarget) lastGrant() []byte {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.opens) == 0 {
		return nil
	}
	return e.opens[len(e.opens)-1].GetGrant()
}

// tunnelUser is a user session config that accepts the stream the relay
// opens toward it.
func tunnelUser() (conduit.Config, chan conduit.Stream) {
	streams := make(chan conduit.Stream, 4)
	return conduit.Config{StreamHandler: conduit.StreamHandlerFunc(func(_ context.Context, _ *conduitv1.StreamOpen, ps conduit.PendingStream) error {
		st, err := ps.Accept()
		if err != nil {
			return err
		}
		streams <- st
		return nil
	})}, streams
}

// dialTarget connects the agent target directly to node (not through the
// role's front) and returns its registry row.
func dialTarget(t *testing.T, w *relaytest.World, node *relaytest.Node, cfg conduit.Config) registry.SessionRecord {
	t.Helper()
	w.SetPrincipal("agent", relay.Principal{Kind: registry.PrincipalAgent, ID: testAgentID, ProjectID: testProject,
		Agent: relay.AgentIncarnationFacts{LaunchID: "L1", Generation: 1}})
	_, wel := node.MustDial("agent", relaytest.AgentHello(testAgentID, "L1", "", "pty"), cfg)
	for _, s := range w.Sessions(registry.PrincipalAgent, testAgentID).Sessions {
		if s.Session.SessionID == wel.GetSessionId() {
			return s.Session
		}
	}
	t.Fatalf("no registry row for session %s", wel.GetSessionId())
	return registry.SessionRecord{}
}

// tunnelInstruction is the hub's accepted answer naming target on owner.
func tunnelInstruction(t *testing.T, target registry.SessionRecord, endpoint string) relay.HubRPCResponse {
	t.Helper()
	body, err := json.Marshal(map[string]any{relay.TunnelResponseStreamIDKey: 0, "sessionId": "user-session", "agentId": testAgentID, "kind": "pty"})
	if err != nil {
		t.Fatal(err)
	}
	unavailable := mustMarshalRPC(t, &conduitv1.RpcResponse{Status: http.StatusServiceUnavailable, Body: []byte(`{"error":{"code":"agent_session_unavailable"}}`)})
	return relay.HubRPCResponse{
		RPC: mustMarshalRPC(t, &conduitv1.RpcResponse{Status: http.StatusOK, Body: body}),
		Tunnel: &relay.HubTunnelOpen{
			TargetSessionID: target.SessionID,
			OwnerInstanceID: target.RelayInstanceID,
			OwnerGeneration: target.RelayGeneration,
			OwnerEndpoint:   endpoint,
			WantProjectID:   testProject,
			WantIncarnation: "L1",
			Kind:            "pty",
			Params:          map[string]string{"cols": "80", "rows": "24"},
			Grant:           []byte("hub-signed-open-grant"),
			OpenJTI:         "jti-1",
			Unavailable:     unavailable,
		},
	}
}

// requestTunnel sends POST /v1/tunnels on the user session.
func requestTunnel(t *testing.T, ls conduit.LocalSession) *conduitv1.RpcResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := ls.Call(ctx, &conduitv1.RpcRequest{RequestId: "t1", Method: http.MethodPost, Path: "/v1/tunnels", Body: []byte(`{"agent":"agent-1","kind":"pty"}`)})
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// streamIDOf reads the user stream id from an accepted answer.
func streamIDOf(t *testing.T, resp *conduitv1.RpcResponse) uint32 {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(resp.GetBody(), &body); err != nil {
		t.Fatalf("answer body %q: %v", resp.GetBody(), err)
	}
	id, ok := body[relay.TunnelResponseStreamIDKey].(float64)
	if !ok || id == 0 {
		t.Fatalf("answer body %s names no user stream", resp.GetBody())
	}
	return uint32(id)
}

// assertEcho writes through the user stream and reads the target's echo.
func assertEcho(t *testing.T, st conduit.Stream) {
	t.Helper()
	msg := []byte("through the tunnel")
	if _, err := st.Write(msg); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(msg))
	done := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(st, got)
		done <- err
	}()
	if err := relaytest.Wait(t, done, "the echo"); err != nil || !bytes.Equal(got, msg) {
		t.Fatalf("echo %q, %v; want %q", got, err, msg)
	}
}

// userSessionFor dials the user through the front, with the hub answering
// tunnel requests with answer.
func userSessionFor(t *testing.T, f *roleFixture, answer func(n int, req relay.HubRPCRequest) hubAnswer) (conduit.LocalSession, chan conduit.Stream) {
	t.Helper()
	var mu sync.Mutex
	n := 0
	f.admitAs(registry.PrincipalUser, "tok-user", func(path string, body []byte) hubAnswer {
		if path != "rpc" {
			return hubAnswer{status: http.StatusServiceUnavailable}
		}
		var req relay.HubRPCRequest
		_ = json.Unmarshal(body, &req)
		mu.Lock()
		n++
		i := n
		mu.Unlock()
		return answer(i, req)
	})
	cfg, streams := tunnelUser()
	ls, _, err := f.dial(t, relaytest.UserHello("user-1"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return ls, streams
}

// TestRoleTunnel_SameRelayOpensLocally (design §3.9 Delivery): when the
// hub names a target session this relay holds, the relay opens the target
// stream itself, with no bridge, passes the hub's grant unchanged, splices
// it to a user stream it opens, and records the tunnel with the target's
// own ids.
func TestRoleTunnel_SameRelayOpensLocally(t *testing.T) {
	f := newRoleFixture(t)
	target := newEchoTarget()
	rec := dialTarget(t, f.w, f.node, target.config)
	ls, streams := userSessionFor(t, f, func(int, relay.HubRPCRequest) hubAnswer {
		return hubAnswer{status: http.StatusOK, body: tunnelInstruction(t, rec, f.node.Internal.URL)}
	})

	resp := requestTunnel(t, ls)
	if resp.GetStatus() != http.StatusOK || resp.GetRequestId() != "t1" {
		t.Fatalf("answer %v, want 200 for t1", resp)
	}
	userStreamID := streamIDOf(t, resp)
	targetStreamID := relaytest.Wait(t, target.ids, "the target stream")
	if got := string(target.lastGrant()); got != "hub-signed-open-grant" {
		t.Fatalf("target got grant %q, want the hub's unchanged", got)
	}
	tn, ok := f.role.Tunnel(ls.Info().SessionID, userStreamID)
	if !ok {
		t.Fatalf("no tunnel recorded for user stream %d (tunnels %+v)", userStreamID, f.role.Tunnels())
	}
	if tn.Bridge != nil {
		t.Fatal("a same-relay tunnel has a bridge")
	}
	if want := (relay.TargetStream{SessionID: rec.SessionID, StreamID: targetStreamID}); tn.Target != want || tn.OpenJTI != "jti-1" {
		t.Fatalf("tunnel %+v, want target %+v and jti-1", tn, want)
	}
	if n := f.node.Relay.ActiveBridges(); n != 0 {
		t.Fatalf("%d bridges, want none", n)
	}
	assertEcho(t, relaytest.Wait(t, streams, "the user stream"))
}

// TestRoleTunnel_OtherRelayOpensBridge (design §3.9 Delivery): when the
// target session is held by another relay, the user's relay opens exactly
// one bridge to it, and the tunnel records the bridge and the owner's
// target-side ids.
func TestRoleTunnel_OtherRelayOpensBridge(t *testing.T) {
	f := newRoleFixture(t)
	owner := f.w.StartNode("relay-1", nil)
	target := newEchoTarget()
	rec := dialTarget(t, f.w, owner, target.config)
	ls, streams := userSessionFor(t, f, func(int, relay.HubRPCRequest) hubAnswer {
		return hubAnswer{status: http.StatusOK, body: tunnelInstruction(t, rec, owner.Internal.URL)}
	})

	resp := requestTunnel(t, ls)
	if resp.GetStatus() != http.StatusOK {
		t.Fatalf("answer %v, want 200", resp)
	}
	userStreamID := streamIDOf(t, resp)
	targetStreamID := relaytest.Wait(t, target.ids, "the target stream")
	tn, ok := f.role.Tunnel(ls.Info().SessionID, userStreamID)
	if !ok || tn.Bridge == nil {
		t.Fatalf("tunnel %+v, %v; want one with a bridge", tn, ok)
	}
	want := relay.TargetStream{SessionID: rec.SessionID, StreamID: targetStreamID}
	if tn.Target != want || tn.Bridge.Target() != want {
		t.Fatalf("tunnel target %+v, bridge target %+v; want %+v", tn.Target, tn.Bridge.Target(), want)
	}
	if got := tn.Bridge.Owner(); got.InstanceID != "relay-1" || got.Endpoint != owner.Internal.URL {
		t.Fatalf("bridge owner %+v, want relay-1 at %s", got, owner.Internal.URL)
	}
	if n := owner.Relay.ActiveBridges(); n != 1 {
		t.Fatalf("owner holds %d bridges, want exactly 1", n)
	}
	if got := string(target.lastGrant()); got != "hub-signed-open-grant" {
		t.Fatalf("target got grant %q, want the hub's unchanged", got)
	}
	assertEcho(t, relaytest.Wait(t, streams, "the user stream"))
}

// TestRoleTunnel_StaleRouteRetriesWithExclusion: a target the owner no
// longer holds sends the request back to the hub with that target
// excluded, and the tunnel opens on the target the hub names next.
func TestRoleTunnel_StaleRouteRetriesWithExclusion(t *testing.T) {
	f := newRoleFixture(t)
	owner := f.w.StartNode("relay-1", nil)
	target := newEchoTarget()
	rec := dialTarget(t, f.w, owner, target.config)
	gone := rec
	gone.SessionID = "session-gone"
	var (
		mu       sync.Mutex
		excluded [][]string
	)
	ls, _ := userSessionFor(t, f, func(n int, req relay.HubRPCRequest) hubAnswer {
		mu.Lock()
		excluded = append(excluded, req.ExcludeTargets)
		mu.Unlock()
		if n == 1 {
			return hubAnswer{status: http.StatusOK, body: tunnelInstruction(t, gone, owner.Internal.URL)}
		}
		return hubAnswer{status: http.StatusOK, body: tunnelInstruction(t, rec, owner.Internal.URL)}
	})

	resp := requestTunnel(t, ls)
	if resp.GetStatus() != http.StatusOK {
		t.Fatalf("answer %v, want 200 after re-resolution", resp)
	}
	_ = streamIDOf(t, resp)
	_ = relaytest.Wait(t, target.ids, "the target stream")
	mu.Lock()
	defer mu.Unlock()
	if len(excluded) != 2 || len(excluded[0]) != 0 || len(excluded[1]) != 1 || excluded[1][0] != "session-gone" {
		t.Fatalf("hub requests excluded %v, want [] then [session-gone]", excluded)
	}
}

// TestRoleTunnel_StaleRouteBudget: when every target the hub names is
// stale, the relay stops after the re-resolution budget and answers with
// the hub's unavailable answer; no tunnel is recorded.
func TestRoleTunnel_StaleRouteBudget(t *testing.T) {
	f := newRoleFixture(t)
	owner := f.w.StartNode("relay-1", nil)
	rec := dialTarget(t, f.w, owner, newEchoTarget().config)
	gone := rec
	gone.SessionID = "session-gone"
	ls, _ := userSessionFor(t, f, func(int, relay.HubRPCRequest) hubAnswer {
		return hubAnswer{status: http.StatusOK, body: tunnelInstruction(t, gone, owner.Internal.URL)}
	})
	resp := requestTunnel(t, ls)
	if resp.GetStatus() != http.StatusServiceUnavailable || !bytes.Contains(resp.GetBody(), []byte("agent_session_unavailable")) {
		t.Fatalf("answer %v, want the hub's 503 agent_session_unavailable", resp)
	}
	if n := f.hub.count("rpc"); n != MaxTunnelReResolves+1 {
		t.Fatalf("%d hub calls, want %d", n, MaxTunnelReResolves+1)
	}
	if tns := f.role.Tunnels(); len(tns) != 0 {
		t.Fatalf("tunnels %+v, want none", tns)
	}
}

// TestRoleTunnel_BridgeRefusesAcceptWithoutTargetIDs: the relay role's
// bridges are strict. An owner whose accept names no target stream gets
// the hop closed with 4400, the user gets the unavailable answer, and no
// tunnel is recorded.
func TestRoleTunnel_BridgeRefusesAcceptWithoutTargetIDs(t *testing.T) {
	f := newRoleFixture(t)
	fake := relaytest.StartFakeOwner(t, "", 0)
	rec := registry.SessionRecord{SessionID: "remote-session", RelayInstanceID: "relay-x", RelayGeneration: 3}
	ls, _ := userSessionFor(t, f, func(int, relay.HubRPCRequest) hubAnswer {
		return hubAnswer{status: http.StatusOK, body: tunnelInstruction(t, rec, fake.URL)}
	})
	resp := requestTunnel(t, ls)
	if resp.GetStatus() != http.StatusServiceUnavailable {
		t.Fatalf("answer %v, want 503", resp)
	}
	relaytest.Wait(t, fake.Opens, "the bridge's StreamOpen")
	if c := relaytest.Wait(t, fake.Closes, "the relay's StreamClose"); c == nil || c.GetCode() != conduit.CloseProtocolError {
		t.Fatalf("relay sent %v on the hop, want StreamClose 4400", c)
	}
	if tns := f.role.Tunnels(); len(tns) != 0 {
		t.Fatalf("tunnels %+v, want none", tns)
	}
}

// TestRoleTunnel_ReportsOpenTunnelsForCap: each tunnel request tells the
// hub how many tunnels the session already has open or opening, so the hub
// can apply its per-session cap.
func TestRoleTunnel_ReportsOpenTunnelsForCap(t *testing.T) {
	f := newRoleFixture(t)
	target := newEchoTarget()
	rec := dialTarget(t, f.w, f.node, target.config)
	var (
		mu     sync.Mutex
		counts []int
	)
	ls, _ := userSessionFor(t, f, func(_ int, req relay.HubRPCRequest) hubAnswer {
		mu.Lock()
		counts = append(counts, req.OpenTunnels)
		mu.Unlock()
		return hubAnswer{status: http.StatusOK, body: tunnelInstruction(t, rec, f.node.Internal.URL)}
	})
	_ = streamIDOf(t, requestTunnel(t, ls))
	_ = streamIDOf(t, requestTunnel(t, ls))
	mu.Lock()
	defer mu.Unlock()
	if len(counts) != 2 || counts[0] != 0 || counts[1] != 1 {
		t.Fatalf("open tunnel counts sent to the hub %v, want [0 1]", counts)
	}
}
