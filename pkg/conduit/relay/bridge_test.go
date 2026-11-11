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

package relay_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/proto"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay/relaytest"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// ownerOf names the relay holding p's target session, at endpoint.
func ownerOf(p *pair, endpoint string) relay.Owner {
	return relay.Owner{InstanceID: p.rec.RelayInstanceID, Generation: p.rec.RelayGeneration, Endpoint: endpoint}
}

// noAccept fails if the target accepted a stream.
func noAccept(t *testing.T, ids <-chan uint32) {
	t.Helper()
	select {
	case id := <-ids:
		t.Fatalf("the target accepted stream %d, want none", id)
	default:
	}
}

// TestOpenBridgeReturnsOwnerAssertedTarget: a bridge exposes the stream on
// the target's own session as the owner relay asserted it, and the relay
// it was opened to.
func TestOpenBridgeReturnsOwnerAssertedTarget(t *testing.T) {
	ids := make(chan uint32, 4)
	p := newPair(t, acceptingConfig(t, ids))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	owner := ownerOf(p, p.a.Internal.URL)
	b, err := relay.OpenBridge(ctx, p.b.Peers, owner, p.rec, p.want, &conduitv1.StreamOpen{Kind: conduitv1.StreamKind_STREAM_KIND_PTY})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	targetID := relaytest.Wait(t, ids, "target-side accept")
	if got, want := b.Target(), (relay.TargetStream{SessionID: p.rec.SessionID, StreamID: targetID}); got != want {
		t.Fatalf("Target() = %+v, want %+v", got, want)
	}
	if got := b.Owner(); got != owner {
		t.Fatalf("Owner() = %+v, want %+v", got, owner)
	}
	if got, ok := relay.TargetOf(nil, b); !ok || got != b.Target() {
		t.Fatalf("TargetOf(bridge) = %+v, %v; want %+v, true", got, ok, b.Target())
	}
	if n := p.a.Relay.ActiveBridges(); n != 1 {
		t.Fatalf("owner holds %d bridges, want 1", n)
	}
}

// TestOpenBridgeRefusesAcceptWithoutTargetIDs: an owner accept that names
// no target stream, or a stream on another session, is refused with 4400
// bad_frame and the hop is closed. OpenStream, the in-process
// compatibility path, still opens the stream from an accept without ids.
func TestOpenBridgeRefusesAcceptWithoutTargetIDs(t *testing.T) {
	p := newPair(t, echoConfig())
	for _, tc := range []struct {
		name    string
		session string
		stream  uint32
	}{
		{name: "no ids"},
		{name: "session id only", session: "SELF"},
		{name: "stream id only", stream: 7},
		{name: "another session", session: "other-session", stream: 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session := tc.session
			if session == "SELF" {
				session = p.rec.SessionID
			}
			fake := relaytest.StartFakeOwner(t, session, tc.stream)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			b, err := relay.OpenBridge(ctx, p.b.Peers, ownerOf(p, fake.URL), p.rec, p.want, &conduitv1.StreamOpen{Kind: conduitv1.StreamKind_STREAM_KIND_PTY})
			if err == nil {
				_ = b.Close()
				t.Fatal("OpenBridge succeeded, want a refusal")
			}
			var ce *conduit.CloseError
			if !errors.Is(err, relay.ErrBridgeNoTarget) || !errors.As(err, &ce) || ce.Code != conduit.CloseProtocolError || !strings.HasPrefix(ce.Reason, relay.ReasonBadFrame) {
				t.Fatalf("OpenBridge = %v, want ErrBridgeNoTarget with 4400 bad_frame", err)
			}
			relaytest.Wait(t, fake.Opens, "the hop's StreamOpen")
			c := relaytest.Wait(t, fake.Closes, "the caller's StreamClose")
			if c == nil || c.GetCode() != conduit.CloseProtocolError {
				t.Fatalf("caller sent %v on the hop, want StreamClose 4400", c)
			}
		})
	}
	t.Run("OpenStream stays lenient", func(t *testing.T) {
		fake := relaytest.StartFakeOwner(t, "", 0)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		rs := relay.NewRemoteSession(p.b.Peers, fake.URL, p.rec, p.want)
		st, err := rs.OpenStream(ctx, &conduitv1.StreamOpen{Kind: conduitv1.StreamKind_STREAM_KIND_PTY})
		if err != nil {
			t.Fatalf("OpenStream = %v, want the stream", err)
		}
		defer func() { _ = st.Close() }()
		if got, ok := relay.TargetOf(rs, st); ok {
			t.Fatalf("TargetOf = %+v, true; want unknown", got)
		}
	})
}

// TestOpenBridgeRequiresRelayPeerAuth: a bridge is never opened without a
// relay-peer signer: nothing reaches the owner. (The owner side refuses an
// unsigned or wrongly signed hop: TestInternalAPIRejectsUnauthenticatedPeer.)
func TestOpenBridgeRequiresRelayPeerAuth(t *testing.T) {
	p := newPair(t, echoConfig())
	fake := relaytest.StartFakeOwner(t, p.rec.SessionID, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	open := &conduitv1.StreamOpen{Kind: conduitv1.StreamKind_STREAM_KIND_PTY}
	for name, pc := range map[string]*relay.PeerClient{
		"no client": nil,
		"no signer": {HTTP: http.DefaultClient},
	} {
		if b, err := relay.OpenBridge(ctx, pc, ownerOf(p, fake.URL), p.rec, p.want, open); err == nil {
			_ = b.Close()
			t.Fatalf("%s: OpenBridge succeeded, want a refusal", name)
		}
	}
	unsigned := relay.NewRemoteSession(&relay.PeerClient{HTTP: http.DefaultClient}, fake.URL, p.rec, p.want)
	if b, err := unsigned.OpenBridge(ctx, open); err == nil {
		_ = b.Close()
		t.Fatal("RemoteSession.OpenBridge without a signer succeeded, want a refusal")
	}
	select {
	case o := <-fake.Opens:
		t.Fatalf("the owner received %v, want nothing", o)
	default:
	}
}

// TestOpenBridgeOwnerMismatchIsStale: an owner that is not the relay
// instance and generation the session was resolved at is a stale route,
// refused before anything is dialed.
func TestOpenBridgeOwnerMismatchIsStale(t *testing.T) {
	p := newPair(t, echoConfig())
	fake := relaytest.StartFakeOwner(t, p.rec.SessionID, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for name, owner := range map[string]relay.Owner{
		"another instance":   {InstanceID: "relay-x", Generation: p.rec.RelayGeneration, Endpoint: fake.URL},
		"another generation": {InstanceID: p.rec.RelayInstanceID, Generation: p.rec.RelayGeneration + 1, Endpoint: fake.URL},
		"no endpoint":        {InstanceID: p.rec.RelayInstanceID, Generation: p.rec.RelayGeneration},
	} {
		_, err := relay.OpenBridge(ctx, p.b.Peers, owner, p.rec, p.want, &conduitv1.StreamOpen{Kind: conduitv1.StreamKind_STREAM_KIND_PTY})
		if !errors.Is(err, relay.ErrStaleRoute) {
			t.Fatalf("%s: OpenBridge = %v, want ErrStaleRoute", name, err)
		}
	}
	select {
	case o := <-fake.Opens:
		t.Fatalf("the owner received %v, want nothing", o)
	default:
	}
}

// TestOpenBridgeStaleEpochRefusedByOwner: a bridge to a session record the
// target has since replaced (an older connection epoch) is refused by the
// owner's admission as a stale route (the caller re-resolves), and the
// target never sees a stream.
func TestOpenBridgeStaleEpochRefusedByOwner(t *testing.T) {
	ids := make(chan uint32, 4)
	p := newPair(t, acceptingConfig(t, ids))
	p.w.SetPrincipal("a2", agentPrincipal("L1", 1))
	_, _ = p.a.MustDial("a2", relaytest.AgentHello(agentID, "L1", "", "pty"), acceptingConfig(t, ids))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := relay.OpenBridge(ctx, p.b.Peers, ownerOf(p, p.a.Internal.URL), p.rec, p.want, &conduitv1.StreamOpen{Kind: conduitv1.StreamKind_STREAM_KIND_PTY})
	if !errors.Is(err, relay.ErrStaleRoute) {
		t.Fatalf("OpenBridge = %v, want ErrStaleRoute", err)
	}
	noAccept(t, ids)
}

// TestBridgeOwnerRefusesPeerFrameWithTargetIDs: target ids are never taken
// from the requesting peer. A hop whose first frame is a StreamAccept
// naming a target stream, instead of StreamOpen, is refused: the owner
// closes the link, opens nothing on the target and holds no bridge.
func TestBridgeOwnerRefusesPeerFrameWithTargetIDs(t *testing.T) {
	ids := make(chan uint32, 4)
	p := newPair(t, acceptingConfig(t, ids))
	url := p.a.Internal.URL + relay.InternalPathPrefix + "sessions/" + p.rec.SessionID + "/stream"
	want := `{"project_id":"` + project + `","incarnation":"L1","capability":"pty"}`
	req := signed(t, p.a.Relay, p.w.PeerAuth("relay-b"), http.MethodGet, url, nil, want)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.DefaultDialer.DialContext(ctx, "ws"+strings.TrimPrefix(url, "http"), req.Header)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	forged, _ := proto.Marshal(&conduitv1.Frame{Body: &conduitv1.Frame_StreamAccept{StreamAccept: &conduitv1.StreamAccept{
		StreamId: 1, InitialWindow: 1 << 20, TargetSessionId: p.rec.SessionID, TargetStreamId: 5,
	}}})
	if err := c.WriteMessage(websocket.BinaryMessage, forged); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, _, err := c.ReadMessage(); err == nil {
		t.Fatal("the owner answered a hop without StreamOpen, want the link closed")
	} else if ne, ok := err.(interface{ Timeout() bool }); ok && ne.Timeout() {
		t.Fatal("the owner did not close the link")
	}
	p.a.Relay.WaitBridgesForTest()
	if n := p.a.Relay.ActiveBridges(); n != 0 {
		t.Fatalf("%d active bridges, want 0", n)
	}
	noAccept(t, ids)
}
