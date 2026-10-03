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
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay/relaytest"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// opLog records store operations through the FaultStore.
type opLog struct {
	mu  sync.Mutex
	ops []string
}

func (l *opLog) record(op string) {
	l.mu.Lock()
	l.ops = append(l.ops, op)
	l.mu.Unlock()
}

func (l *opLog) index(op string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Index(l.ops, op)
}

func TestStartRegistersAndSweeps(t *testing.T) {
	w := relaytest.NewWorld(t)
	var log opLog
	w.SetFault(func(op string) error { log.record(op); return nil })
	n := w.StartNode("relay-a", nil)
	if n.Relay.Generation() == 0 || n.Relay.InternalEndpoint() != n.Internal.URL {
		t.Fatalf("gen %d endpoint %q", n.Relay.Generation(), n.Relay.InternalEndpoint())
	}
	if r, s := log.index(registry.OpRegisterRelay), log.index(registry.OpDeleteSessionsOfOlderGenerations); r < 0 || s < r {
		t.Fatalf("ops %v: want RegisterRelay then the older-generation sweep", log.ops)
	}
	// Heartbeat every 15s on the relay clock.
	log.ops = nil
	n.Clock.Advance(relay.DefaultHeartbeatInterval)
	if log.index(registry.OpHeartbeatRelay) < 0 {
		t.Fatalf("no heartbeat after 15s: %v", log.ops)
	}
}

// TestStartSelfCheck (C8 at library level): hosted-HA (RequireSelfCheck)
// refuses an unaddressable relay or a failing self-check without
// registering; other profiles register as unaddressable.
func TestStartSelfCheck(t *testing.T) {
	for _, tc := range []struct {
		name     string
		endpoint func(n *relaytest.Node) string
		require  bool
		wantErr  error
	}{
		{name: "HA unaddressable", endpoint: func(*relaytest.Node) string { return "" }, require: true, wantErr: registry.ErrUnaddressable},
		{name: "HA endpoint answered by another instance", endpoint: func(*relaytest.Node) string { return "" }, require: true},
		{name: "solo unaddressable registers without endpoint", endpoint: func(*relaytest.Node) string { return "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := relaytest.NewWorld(t)
			other := w.StartNode("relay-other", nil)
			var log opLog
			w.SetFault(func(op string) error { log.record(op); return nil })
			n, err := w.NewNode("relay-a", func(c *relay.Config) {
				c.RequireSelfCheck = tc.require
				c.InternalEndpoint = ""
				if tc.name == "HA endpoint answered by another instance" {
					c.InternalEndpoint = other.Internal.URL
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			err = n.Relay.Start(context.Background())
			switch {
			case tc.require && err == nil:
				t.Fatal("hosted-HA start succeeded on an unaddressable relay")
			case tc.require:
				if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				if tc.wantErr == nil && !errors.Is(err, registry.ErrSelfCheckMismatch) {
					t.Fatalf("err = %v, want ErrSelfCheckMismatch", err)
				}
				if log.index(registry.OpRegisterRelay) >= 0 {
					t.Fatal("a relay that failed the self-check registered")
				}
			default:
				if err != nil || n.Relay.InternalEndpoint() != "" {
					t.Fatalf("solo start = %v, endpoint %q", err, n.Relay.InternalEndpoint())
				}
			}
		})
	}
}

// TestHeartbeatSupersededStopsServing: on ErrRelaySuperseded the relay
// stops serving, sends GoAway to its sessions and reports Fatal.
func TestHeartbeatSupersededStopsServing(t *testing.T) {
	w := relaytest.NewWorld(t)
	n := w.StartNode("relay-a", nil)
	w.SetPrincipal("a", agentPrincipal("L1", 1))
	sess, _ := n.MustDial("a", relaytest.AgentHello(agentID, "L1", "", "pty"), conduit.Config{})

	// A second process registers the same instance id.
	if _, err := w.Registry.RegisterRelay(context.Background(), registry.RelayInstance{InstanceID: "relay-a"}); err != nil {
		t.Fatal(err)
	}
	n.Clock.Advance(relay.DefaultHeartbeatInterval)
	if err := relaytest.Wait(t, n.Relay.Fatal(), "fatal"); !errors.Is(err, relay.ErrSuperseded) {
		t.Fatalf("fatal = %v", err)
	}
	relaytest.WaitClosed(t, sess.GoAwayReceived(), "GoAway after supersede")
	_, _, err := n.Dial(context.Background(), "a", relaytest.AgentHello(agentID, "L1", "", "pty"), conduit.Config{})
	if conduit.CodeOf(err, 0) != conduit.CloseRelayRestart {
		t.Fatalf("dial after supersede = %v, want 4503", err)
	}
}

// TestGoAwayMarksSessionDraining: SetSessionDraining runs before GoAway is
// sent. With no open streams the session ends right after GoAway, so the
// row is observed at its delete: it must already be draining.
func TestGoAwayMarksSessionDraining(t *testing.T) {
	w := relaytest.NewWorld(t)
	n := w.StartNode("relay-a", nil)
	w.SetPrincipal("a", agentPrincipal("L1", 1))
	sess, wel := n.MustDial("a", relaytest.AgentHello(agentID, "L1", "", "pty"), conduit.Config{})
	var log opLog
	drainingAtDelete := make(chan bool, 1)
	w.SetFault(func(op string) error {
		log.record(op)
		if op == registry.OpDeleteSessionCAS {
			rows := w.Sessions(registry.PrincipalAgent, agentID).Sessions
			select {
			case drainingAtDelete <- len(rows) == 1 && rows[0].Session.Draining:
			default:
			}
		}
		return nil
	})
	if err := n.Relay.GoAway(wel.GetSessionId(), conduit.GoAwayOptions{Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	relaytest.WaitClosed(t, sess.GoAwayReceived(), "GoAway")
	if !relaytest.Wait(t, drainingAtDelete, "row delete") {
		t.Fatal("session row was not draining when the session ended")
	}
	if sd, del := log.index(registry.OpSetSessionDraining), log.index(registry.OpDeleteSessionCAS); sd < 0 || del < sd {
		t.Fatalf("ops %v: want SetSessionDraining before DeleteSessionCAS", log.ops)
	}
	if err := n.Relay.GoAway("no-such-session", conduit.GoAwayOptions{}); !errors.Is(err, registry.ErrSessionNotFound) {
		t.Fatalf("GoAway(unknown) = %v", err)
	}
}

func TestShutdownDrains(t *testing.T) {
	w := relaytest.NewWorld(t)
	n := w.StartNode("relay-a", nil)
	w.SetPrincipal("a", agentPrincipal("L1", 1))
	sess, _ := n.MustDial("a", relaytest.AgentHello(agentID, "L1", "", "pty"), conduit.Config{})
	go func() { <-sess.GoAwayReceived(); _ = sess.Close() }()
	var log opLog
	w.SetFault(func(op string) error { log.record(op); return nil })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := n.Relay.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	_ = relaytest.Wait(t, n.Served, "serve to return")
	rd, sd, del := log.index(registry.OpSetRelayDraining), log.index(registry.OpSetSessionDraining), log.index(registry.OpDeleteSessionCAS)
	// The two draining writes run concurrently; both precede the GoAway
	// and so the row delete.
	if rd < 0 || sd < 0 || del < rd || del < sd {
		t.Fatalf("ops %v: want SetRelayDraining and SetSessionDraining, then DeleteSessionCAS", log.ops)
	}
	if n := len(w.Sessions(registry.PrincipalAgent, agentID).Sessions); n != 0 {
		t.Fatalf("%d rows after shutdown", n)
	}
}

// TestShutdownWaitsAndIsBounded (F5): Shutdown returns only after Serve's
// row delete and in-flight touches have finished. The delete case blocks
// the delete, cancels ctx while it is blocked and expects context.Canceled
// (Shutdown would return nil if it did not wait); the touch case releases
// the touch and expects it to have finished when Shutdown returns.
func TestShutdownWaitsAndIsBounded(t *testing.T) {
	for _, tc := range []struct {
		name string
		// block is the store op held until the test ends ("" = none).
		block string
		touch bool // start a touch before Shutdown
	}{
		{name: "row delete in flight", block: registry.OpDeleteSessionCAS},
		{name: "touch in flight", block: registry.OpTouchSession, touch: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := relaytest.NewWorld(t)
			n := w.StartNode("relay-a", nil)
			w.SetPrincipal("a", agentPrincipal("L1", 1))
			sess, wel := n.MustDial("a", relaytest.AgentHello(agentID, "L1", "", "pty"), conduit.Config{})
			release := make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(unblock) // runs before the world's cleanups
			entered := make(chan struct{}, 1)
			var finished atomic.Bool // the blocked op has returned
			if tc.block != "" {
				w.SetFault(func(op string) error {
					if op == tc.block {
						select {
						case entered <- struct{}{}:
						default:
						}
						<-release
						finished.Store(true)
					}
					return nil
				})
			}
			if tc.touch {
				n.Relay.TouchInterceptorForTest(wel.GetSessionId(), nil)(conduit.Inbound, relay.PongFrame)
				relaytest.Wait(t, entered, "touch to reach the store")
			}
			go func() { <-sess.GoAwayReceived(); _ = sess.Close() }()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- n.Relay.Shutdown(ctx) }()
			switch {
			case tc.touch:
				// Serve has returned (row deleted); only the touch is
				// left. Shutdown returns once the touch has finished.
				_ = relaytest.Wait(t, n.Served, "Serve to return")
				unblock()
				if err := relaytest.Wait(t, result, "Shutdown to return"); err != nil {
					t.Fatalf("Shutdown = %v", err)
				}
				if !finished.Load() {
					t.Fatal("Shutdown returned while a touch was in flight")
				}
				return
			default:
				relaytest.Wait(t, entered, tc.block+" to reach the store")
			}
			cancel()
			if err := relaytest.Wait(t, result, "Shutdown to return"); !errors.Is(err, context.Canceled) {
				t.Fatalf("Shutdown = %v, want context.Canceled (it did not wait)", err)
			}
		})
	}
}

// TestShutdownDeadlineClosesLateSessions (F5): a session that outlives
// GoAway (it has an open stream) is closed with 4503 at the Shutdown
// deadline, and Shutdown returns ctx.Err().
func TestShutdownDeadlineClosesLateSessions(t *testing.T) {
	p := newPair(t, echoConfig())
	octx, ocancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer ocancel()
	st, err := p.remote().OpenStream(octx, &conduitv1.StreamOpen{Kind: conduitv1.StreamKind_STREAM_KIND_PTY})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- p.a.Relay.Shutdown(ctx) }()
	relaytest.WaitClosed(t, p.target.GoAwayReceived(), "GoAway")
	cancel()
	if err := relaytest.Wait(t, result, "Shutdown to return"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Shutdown = %v, want context.Canceled", err)
	}
	relaytest.WaitClosed(t, p.target.Done(), "late session close")
	if code := conduit.CodeOf(p.target.Err(), 0); code != conduit.CloseRelayRestart {
		t.Fatalf("late session ended with %v, want 4503", p.target.Err())
	}
}

// TestTouchAfterShutdownIsNoop (F5): a pong delivered after Shutdown
// schedules no touch.
func TestTouchAfterShutdownIsNoop(t *testing.T) {
	w := relaytest.NewWorld(t)
	n := w.StartNode("relay-a", nil)
	w.SetPrincipal("a", agentPrincipal("L1", 1))
	sess, wel := n.MustDial("a", relaytest.AgentHello(agentID, "L1", "", "pty"), conduit.Config{})
	ic := n.Relay.TouchInterceptorForTest(wel.GetSessionId(), nil)
	go func() { <-sess.GoAwayReceived(); _ = sess.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := n.Relay.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	var log opLog
	w.SetFault(func(op string) error { log.record(op); return nil })
	ic(conduit.Inbound, relay.PongFrame)
	n.Relay.WaitTouchesForTest()
	if i := log.index(registry.OpTouchSession); i >= 0 {
		t.Fatalf("ops %v: a touch ran after Shutdown", log.ops)
	}
}

func TestAbandonAdmissionDeletesRow(t *testing.T) {
	w := relaytest.NewWorld(t)
	n := w.StartNode("relay-a", nil)
	p := agentPrincipal("L1", 1)
	adm, admitted := n.Relay.NewAdmitterForTest(p, registry.TransportWS)
	hello := relaytest.AgentHello(agentID, "L1", "")
	wel, err := adm.Admit(context.Background(), hello)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := admitted(); !ok || len(w.Sessions(registry.PrincipalAgent, agentID).Sessions) != 1 {
		t.Fatal("admission wrote no row")
	}
	adm.(conduit.AdmitAbandoner).AbandonAdmission(context.Background(), hello, wel)
	if n := len(w.Sessions(registry.PrincipalAgent, agentID).Sessions); n != 0 {
		t.Fatalf("%d rows after AbandonAdmission", n)
	}
}

// TestDeleteSessionCASRetried: the row delete after a session ends is
// retried with backoff; after the attempt budget the reaper is the
// backstop and the row stays.
func TestDeleteSessionCASRetried(t *testing.T) {
	for _, tc := range []struct {
		name     string
		failures int
		wantRows int
		wantTry  int
	}{
		{name: "succeeds on third attempt", failures: 2, wantRows: 0, wantTry: 3},
		{name: "gives up after the budget", failures: 100, wantRows: 1, wantTry: relay.DefaultDeleteAttempts},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := relaytest.NewWorld(t)
			n := w.StartNode("relay-a", nil)
			w.SetPrincipal("a", agentPrincipal("L1", 1))
			sess, _ := n.MustDial("a", relaytest.AgentHello(agentID, "L1", "", "pty"), conduit.Config{})
			var tries atomic.Int32
			attempted := make(chan struct{}, relay.DefaultDeleteAttempts+1)
			w.SetFault(func(op string) error {
				if op != registry.OpDeleteSessionCAS {
					return nil
				}
				attempted <- struct{}{}
				if int(tries.Add(1)) <= tc.failures {
					return errors.New("injected")
				}
				return nil
			})
			_ = sess.Close()
			for i := 1; i <= tc.wantTry; i++ {
				relaytest.Wait(t, attempted, "delete attempt")
				if i == tc.wantTry {
					break
				}
				// The heartbeat timer plus the backoff timer.
				if !n.Clock.WaitFor(10*time.Second, func(p int) bool { return p >= 2 }) {
					t.Fatalf("attempt %d failed but no backoff timer was armed", i)
				}
				n.Clock.Advance(5 * time.Second)
			}
			_ = relaytest.Wait(t, n.Served, "serve to return")
			if got := int(tries.Load()); got != tc.wantTry {
				t.Fatalf("delete attempts = %d, want %d", got, tc.wantTry)
			}
			w.SetFault(nil)
			if got := len(w.Sessions(registry.PrincipalAgent, agentID).Sessions); got != tc.wantRows {
				t.Fatalf("rows = %d, want %d", got, tc.wantRows)
			}
		})
	}
}

// --- Touch on pong (conduit-em Q3 conditions) ---

// TestTouchInterceptorChains: the touch interceptor chains with a
// configured interceptor instead of replacing it, in Serve and in the
// helper.
func TestTouchInterceptorChains(t *testing.T) {
	w := relaytest.NewWorld(t)
	var hellos atomic.Int32
	n := w.StartNode("relay-a", func(c *relay.Config) {
		c.Session.Interceptor = func(dir conduit.Direction, f *conduitv1.Frame) []*conduitv1.Frame {
			if dir == conduit.Inbound && f.GetHello() != nil {
				hellos.Add(1)
			}
			return []*conduitv1.Frame{f}
		}
	})
	w.SetPrincipal("a", agentPrincipal("L1", 1))
	_, wel := n.MustDial("a", relaytest.AgentHello(agentID, "L1", "", "pty"), conduit.Config{})
	if hellos.Load() != 1 {
		t.Fatalf("configured interceptor saw %d hellos; Serve must chain it", hellos.Load())
	}
	var seen []*conduitv1.Frame
	next := func(_ conduit.Direction, f *conduitv1.Frame) []*conduitv1.Frame {
		seen = append(seen, f)
		return []*conduitv1.Frame{f, f} // next may rewrite: its output wins
	}
	before := w.Sessions(registry.PrincipalAgent, agentID).Sessions[0].Session.LastSeen
	w.Advance(5 * time.Second)
	out := n.Relay.TouchInterceptorForTest(wel.GetSessionId(), next)(conduit.Inbound, relay.PongFrame)
	n.Relay.WaitTouchesForTest()
	if len(seen) != 1 || len(out) != 2 {
		t.Fatalf("next saw %d frames, chain returned %d; want 1 and 2", len(seen), len(out))
	}
	after := w.Sessions(registry.PrincipalAgent, agentID).Sessions[0].Session.LastSeen
	if !after.After(before) {
		t.Fatalf("last_seen not bumped on pong: %v -> %v", before, after)
	}
}

// TestTouchNeverBlocksReadLoop: while a touch is stuck in the store, pongs
// return immediately; at most one touch is in flight and the extra pongs
// coalesce into a single follow-up touch.
func TestTouchNeverBlocksReadLoop(t *testing.T) {
	w := relaytest.NewWorld(t)
	n := w.StartNode("relay-a", nil)
	w.SetPrincipal("a", agentPrincipal("L1", 1))
	_, wel := n.MustDial("a", relaytest.AgentHello(agentID, "L1", "", "pty"), conduit.Config{})
	gate := make(chan struct{})
	entered := make(chan struct{}, 16)
	var touches, inflight, maxInflight atomic.Int32
	w.SetFault(func(op string) error {
		if op != registry.OpTouchSession {
			return nil
		}
		touches.Add(1)
		cur := inflight.Add(1)
		for {
			m := maxInflight.Load()
			if cur <= m || maxInflight.CompareAndSwap(m, cur) {
				break
			}
		}
		entered <- struct{}{}
		<-gate
		inflight.Add(-1)
		return nil
	})
	ic := n.Relay.TouchInterceptorForTest(wel.GetSessionId(), nil)
	ic(conduit.Inbound, relay.PongFrame)
	relaytest.Wait(t, entered, "first touch")
	returned := make(chan struct{})
	go func() {
		for range 100 {
			ic(conduit.Inbound, relay.PongFrame)
		}
		close(returned)
	}()
	relaytest.WaitClosed(t, returned, "pongs to return while a touch is blocked")
	close(gate)
	n.Relay.WaitTouchesForTest()
	if got := touches.Load(); got != 2 {
		t.Fatalf("touches = %d, want 2 (one in flight + one coalesced follow-up)", got)
	}
	if maxInflight.Load() != 1 {
		t.Fatalf("max in-flight touches = %d, want 1", maxInflight.Load())
	}
	// Outbound pongs and other frames never touch.
	ic(conduit.Outbound, relay.PongFrame)
	ic(conduit.Inbound, &conduitv1.Frame{Body: &conduitv1.Frame_Ping{Ping: &conduitv1.Ping{}}})
	n.Relay.WaitTouchesForTest()
	if got := touches.Load(); got != 2 {
		t.Fatalf("touches = %d after non-pong frames, want 2", got)
	}
}

// TestTouchSessionNotFoundCloses: a touch that finds the row gone (reaped
// or replaced) sends GoAway 4503 relay_restart (with a jittered reconnect
// hint) so the target reconnects.
func TestTouchSessionNotFoundCloses(t *testing.T) {
	w := relaytest.NewWorld(t)
	n := w.StartNode("relay-a", nil)
	w.SetPrincipal("a", agentPrincipal("L1", 1))
	sess, wel := n.MustDial("a", relaytest.AgentHello(agentID, "L1", "", "pty"), conduit.Config{})
	if _, err := w.Inner.DeleteSessionCAS(context.Background(), wel.GetSessionId(), "relay-a", n.Relay.Generation()); err != nil {
		t.Fatal(err)
	}
	n.Relay.TouchInterceptorForTest(wel.GetSessionId(), nil)(conduit.Inbound, relay.PongFrame)
	relaytest.WaitClosed(t, sess.GoAwayReceived(), "GoAway")
	relaytest.WaitClosed(t, sess.Done(), "session close")
	if code := conduit.CodeOf(sess.Err(), 0); code != conduit.CloseRelayRestart {
		t.Fatalf("session ended with %v, want 4503", sess.Err())
	}
}
