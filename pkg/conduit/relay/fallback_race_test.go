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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay/relaytest"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport/ws"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
	"google.golang.org/protobuf/proto"
)

// TestFallbackPostInsertRecheck drives the check-then-insert race of the
// fallback fence deterministically: the launch-id Hello (L) is admitted
// after the fallback Hello's (F) pre-check and before F's insert. F's
// post-insert re-check refuses it with 4409 legacy_hello_superseded and
// deletes F's row. F's insert made L epoch-obsolete, so L's row is evicted
// as well; L's relay notices at its next touch and sends GoAway 4503 so
// the launch container reconnects and becomes routable again.
func TestFallbackPostInsertRecheck(t *testing.T) {
	w := relaytest.NewWorld(t)
	a := w.StartNode("relay-a", nil)
	b := w.StartNode("relay-b", nil)
	w.SetPrincipal("a", agentPrincipal("L2", 3))

	var (
		once    atomic.Bool
		l       conduit.LocalSession
		lWel    *conduitv1.Welcome
		lErr    error
		lDialed = make(chan struct{})
	)
	w.SetFault(func(op string) error {
		if op == registry.OpInsertSessionWithNextEpoch && once.CompareAndSwap(false, true) {
			// F has passed its pre-check and is about to insert: admit L
			// now (its own insert does not re-enter this branch).
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			l, lWel, lErr = a.Dial(ctx, "a", relaytest.AgentHello(agentID, "L2", "", "pty"), conduit.Config{})
			close(lDialed)
		}
		return nil
	})
	_, _, err := b.Dial(context.Background(), "a", relaytest.AgentHello(agentID, "", "", "pty"), conduit.Config{})
	w.SetFault(nil)
	relaytest.WaitClosed(t, lDialed, "L dial inside F's insert")
	if lErr != nil {
		t.Fatalf("L dial: %v", lErr)
	}
	assertClose(t, err, relay.CloseSupersededIncarnation, relay.ReasonLegacyHelloSuperseded)
	if !relay.IsSupersededIncarnation(err) {
		t.Fatalf("IsSupersededIncarnation(%v) = false", err)
	}
	if ps := w.Sessions(registry.PrincipalAgent, agentID); len(ps.Sessions) != 0 {
		t.Fatalf("rows after the re-check = %+v; want F's row deleted and L's obsolete row evicted", ps.Sessions)
	}

	// L's relay touches on the next pong, finds the row gone and asks the
	// launch container to reconnect.
	a.Relay.TouchInterceptorForTest(t, lWel.GetSessionId(), nil)(conduit.Inbound, relay.PongFrame)
	a.Relay.WaitTouchesForTest()
	relaytest.WaitClosed(t, l.GoAwayReceived(), "GoAway to the evicted launch session")
	relaytest.WaitClosed(t, l.Done(), "evicted launch session close")
	if code := conduit.CodeOf(l.Err(), 0); code != conduit.CloseRelayRestart {
		t.Fatalf("evicted launch session ended with %v, want 4503", l.Err())
	}

	// The reconnect is admitted and routable.
	_, back := a.MustDial("a", relaytest.AgentHello(agentID, "L2", "", "pty"), conduit.Config{})
	res, err := resolveAgent(t, w, b, relay.AgentIncarnationFacts{LaunchID: "L2", Generation: 3})
	if err != nil || res.Record.SessionID != back.GetSessionId() {
		t.Fatalf("after reconnect: resolved %+v, %v; want %s", res.Record, err, back.GetSessionId())
	}
}

// TestFallbackConcurrentWithLaunchID races a fallback Hello against a
// launch-id Hello of the current launch on different relays. Whatever the
// interleaving, the launch-id Hello is admitted, the fallback is either
// admitted or refused with 4409, no generation-source row ever holds the
// current epoch while a launch row exists, and every remaining launch row
// is epoch-current (routable).
func TestFallbackConcurrentWithLaunchID(t *testing.T) {
	for i := range 10 {
		w := relaytest.NewWorld(t)
		a := w.StartNode("relay-a", nil)
		b := w.StartNode("relay-b", nil)
		w.SetPrincipal("a", agentPrincipal("L2", 3))
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		var wg sync.WaitGroup
		var lErr, fErr error
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, _, lErr = a.Dial(ctx, "a", relaytest.AgentHello(agentID, "L2", "", "pty"), conduit.Config{})
		}()
		go func() {
			defer wg.Done()
			<-start
			_, _, fErr = b.Dial(ctx, "a", relaytest.AgentHello(agentID, "", "", "pty"), conduit.Config{})
		}()
		close(start)
		wg.Wait()
		cancel()
		if lErr != nil {
			t.Fatalf("iteration %d: launch-id dial: %v", i, lErr)
		}
		if fErr != nil {
			assertClose(t, fErr, relay.CloseSupersededIncarnation, relay.ReasonLegacyHelloSuperseded)
		}
		ps := w.Sessions(registry.PrincipalAgent, agentID)
		for _, v := range ps.Sessions {
			s := v.Session
			current := s.ConnectionEpoch == ps.CurrentEpoch
			switch s.Capabilities.IncarnationSource {
			case relay.IncarnationSourceGeneration:
				if current {
					t.Fatalf("iteration %d (fallback err %v): generation row %s holds the current epoch %d", i, fErr, s.SessionID, ps.CurrentEpoch)
				}
			case relay.IncarnationSourceLaunchID:
				if !current {
					t.Fatalf("iteration %d (fallback err %v): launch row %s at epoch %d is not current (%d)", i, fErr, s.SessionID, s.ConnectionEpoch, ps.CurrentEpoch)
				}
			}
		}
	}
}

// TestUserPipelinedStreamOpenClosed4403 (F1): a user that writes Hello and
// StreamOpen back to back has the session closed with 4403 even when the
// StreamOpen is refused before Serve has marked the session ready. The
// seam holds Serve until the client has seen the stream refused, so the
// refusal deterministically lands in the window.
func TestUserPipelinedStreamOpenClosed4403(t *testing.T) {
	w := relaytest.NewWorld(t)
	n := w.StartNode("relay-a", nil)
	w.SetPrincipal("u", relay.Principal{Kind: registry.PrincipalUser, ID: "user-1"})
	release := make(chan struct{})
	n.Relay.SetBeforeReadyHookForTest(func() { <-release })
	conn := rawDial(t, n, "u")
	if err := conn.WriteFrame(frameBytes(t, &conduitv1.Frame{Body: &conduitv1.Frame_Hello{Hello: relaytest.UserHello("user-1")}})); err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteFrame(frameBytes(t, &conduitv1.Frame{Body: &conduitv1.Frame_StreamOpen{StreamOpen: &conduitv1.StreamOpen{
		StreamId: 1, Kind: conduitv1.StreamKind_STREAM_KIND_PTY}}})); err != nil {
		t.Fatal(err)
	}
	for {
		b, err := conn.ReadFrame()
		if err != nil {
			close(release)
			t.Fatalf("read before the stream refusal: %v", err)
		}
		f := &conduitv1.Frame{}
		if err := proto.Unmarshal(b, f); err != nil {
			t.Fatal(err)
		}
		if sc := f.GetStreamClose(); sc != nil && sc.GetStreamId() == 1 {
			if sc.GetCode() != conduit.CloseForbidden {
				t.Fatalf("stream refused with %d, want 4403", sc.GetCode())
			}
			break
		}
	}
	close(release)
	served := relaytest.Wait(t, n.Served, "Serve to return")
	if code := conduit.CodeOf(served, 0); code != conduit.CloseForbidden {
		t.Fatalf("session ended with %v, want 4403", served)
	}
}

func frameBytes(t *testing.T, f *conduitv1.Frame) []byte {
	t.Helper()
	b, err := proto.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// rawDial opens a bare WebSocket to n's public endpoint as the principal
// registered under key, without the conduit handshake.
func rawDial(t *testing.T, n *relaytest.Node, key string) transport.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	d := &ws.Dialer{URL: "ws" + strings.TrimPrefix(n.Public.URL, "http") + "/?principal=" + key}
	conn, err := d.Dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}
