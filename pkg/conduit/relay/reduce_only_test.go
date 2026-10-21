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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay/relaytest"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// The reduce-only invariant (plan.md standing rule; design §3.9 "Drain
// through the proxy" and "Source of the cap"): no path moves a session's
// lifetime cap or drain deadline later than it already was. A session gets
// at most one GoAway, its drain deadline is never longer than the bounds
// in force, and the session ends no later than its first planned end.

// roSession is one agent session proxied by hub-a, with a PTY stream open
// so that it lives until its drain deadline, and the GoAways it receives.
type roSession struct {
	ls      conduit.LocalSession
	goAways <-chan *conduitv1.GoAway
	t0      time.Time
	hint    uint32
}

// roNode starts a fake-clock relay with lifetime cap relayCap and session
// drain deadline own (0: the defaults).
func roNode(t *testing.T, relayCap, own time.Duration) (*relaytest.World, *relaytest.Node) {
	t.Helper()
	w := relaytest.NewWorld(t)
	return w, lifetimeNode(t, w, relayCap, own)
}

// roDial dials the session with principal cap pcap.
func roDial(t *testing.T, w *relaytest.World, n *relaytest.Node, pcap time.Duration) roSession {
	t.Helper()
	p := agentPrincipal("L1", 1)
	p.IngressInstance = "hub-a"
	p.LifetimeCap = pcap
	w.SetPrincipal("a", p)
	t0 := n.Clock.Now()
	cfg := echoConfig()
	var goAways <-chan *conduitv1.GoAway
	cfg.Interceptor, goAways = goAwayRecorder(false)
	_, wel := n.MustDial("a", relaytest.AgentHello(agentID, "L1", "", "pty"), cfg)
	ls, _ := openEcho(t, n, wel.GetSessionId())
	return roSession{ls: ls, goAways: goAways, t0: t0, hint: wel.GetLifetimeHintS()}
}

// firstGoAway waits for the session's GoAway and checks its drain deadline.
func firstGoAway(t *testing.T, s roSession, wantMs uint32) {
	t.Helper()
	if g := relaytest.Wait(t, s.goAways, "GoAway"); g.GetDrainDeadlineMs() != wantMs {
		t.Fatalf("drain_deadline_ms = %d, want %d", g.GetDrainDeadlineMs(), wantMs)
	}
}

// endsBy advances the clock to at and checks the session has ended by
// then, having received no second GoAway.
func endsBy(t *testing.T, n *relaytest.Node, s roSession, at time.Time) {
	t.Helper()
	if now := n.Clock.Now(); now.Before(at) && !isClosed(s.ls.Done()) {
		waitTimer(t, n, at, "drain deadline")
		n.Clock.Advance(at.Sub(now))
	}
	relaytest.WaitClosed(t, s.ls.Done(), "session to end by its planned end")
	if now := n.Clock.Now(); now.After(at) {
		t.Fatalf("session ended at +%s, after its planned end +%s", now.Sub(s.t0), at.Sub(s.t0))
	}
	select {
	case g := <-s.goAways:
		t.Fatalf("a second GoAway was sent: %+v", g)
	default:
	}
}

func drainHubA(t *testing.T, n *relaytest.Node, deadline time.Duration, want int) {
	t.Helper()
	if got := n.Relay.DrainIngress("hub-a", deadline); got != want {
		t.Fatalf("DrainIngress = %d, want %d", got, want)
	}
}

// TestReduceOnly_NoPathLengthensASession runs each path that can set a
// session's lifetime cap or drain deadline, alone and after another, and
// checks that none lengthens the session.
func TestReduceOnly_NoPathLengthensASession(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(t *testing.T)
	}{
		{name: "drain asking more than the relay's own deadline", run: func(t *testing.T) {
			w, n := roNode(t, 0, 20*time.Second)
			s := roDial(t, w, n, 0)
			drainHubA(t, n, time.Hour, 1)
			firstGoAway(t, s, 20000)
			endsBy(t, n, s, s.t0.Add(20*time.Second))
		}},
		{name: "drain asking more than the cap's remainder", run: func(t *testing.T) {
			w, n := roNode(t, 0, 120*time.Second)
			s := roDial(t, w, n, 100*time.Second)
			drainHubA(t, n, 0, 1)
			firstGoAway(t, s, 100000)
			endsBy(t, n, s, s.t0.Add(100*time.Second))
		}},
		{name: "re-drain is a no-op", run: func(t *testing.T) {
			w, n := roNode(t, 0, 0)
			s := roDial(t, w, n, 0)
			drainHubA(t, n, 5*time.Second, 1)
			drainHubA(t, n, time.Hour, 0)
			firstGoAway(t, s, 5000)
			endsBy(t, n, s, s.t0.Add(5*time.Second))
		}},
		{name: "over-24h request saturates and is clamped", run: func(t *testing.T) {
			w, n := roNode(t, 0, 0)
			s := roDial(t, w, n, 0)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			sent, err := hubDrainClient(w, n, "hub-a").Drain(ctx, n.Internal.URL, "relay-a", n.Relay.Generation(), "hub-a", 1000*24*time.Hour)
			if err != nil || sent != 1 {
				t.Fatalf("drain = %d, %v; want 1", sent, err)
			}
			firstGoAway(t, s, uint32(conduit.DefaultDrainDeadline/time.Millisecond))
			endsBy(t, n, s, s.t0.Add(conduit.DefaultDrainDeadline))
		}},
		{name: "principal cap above the relay cap", run: func(t *testing.T) {
			w, n := roNode(t, 90*time.Second, 0)
			s := roDial(t, w, n, time.Hour)
			if s.hint != 30 {
				t.Fatalf("lifetime_hint_s = %d, want 30 (the relay's 90s cap)", s.hint)
			}
			waitTimer(t, n, s.t0.Add(30*time.Second), "lifetime GoAway")
			n.Clock.Advance(30 * time.Second)
			firstGoAway(t, s, 30000)
			endsBy(t, n, s, s.t0.Add(60*time.Second))
		}},
		{name: "lifetime GoAway during a drain", run: func(t *testing.T) {
			w, n := roNode(t, 90*time.Second, 120*time.Second)
			s := roDial(t, w, n, 0)
			drainHubA(t, n, 50*time.Second, 1)
			firstGoAway(t, s, 50000)
			endsBy(t, n, s, s.t0.Add(50*time.Second)) // the lifetime timer at +30s finds the session draining
		}},
		{name: "Shutdown during a drain", run: func(t *testing.T) {
			w, n := roNode(t, 0, 0)
			s := roDial(t, w, n, 0)
			drainHubA(t, n, 5*time.Second, 1)
			firstGoAway(t, s, 5000)
			done := make(chan error, 1)
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				done <- n.Relay.Shutdown(ctx)
			}()
			endsBy(t, n, s, s.t0.Add(5*time.Second))
			relaytest.Wait(t, done, "Shutdown")
		}},
		{name: "admission stopped during a drain", run: func(t *testing.T) {
			w, n := roNode(t, 0, 0)
			s := roDial(t, w, n, 0)
			drainHubA(t, n, 5*time.Second, 1)
			n.Relay.SetAdmitting(false)
			firstGoAway(t, s, 5000)
			endsBy(t, n, s, s.t0.Add(5*time.Second))
		}},
		{name: "drain after a lifetime GoAway", run: func(t *testing.T) {
			w, n := roNode(t, 90*time.Second, 0)
			s := roDial(t, w, n, 0)
			waitTimer(t, n, s.t0.Add(30*time.Second), "lifetime GoAway")
			n.Clock.Advance(30 * time.Second)
			firstGoAway(t, s, 30000)
			drainHubA(t, n, time.Millisecond, 0)
			endsBy(t, n, s, s.t0.Add(60*time.Second))
		}},
	} {
		t.Run(tc.name, tc.run)
	}
}
