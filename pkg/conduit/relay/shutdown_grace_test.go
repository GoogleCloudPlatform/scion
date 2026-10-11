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
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay/relaytest"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport/ws"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// graceAgent registers agent id under key and dials it with a GoAway
// recorder (drop hides the GoAway from the target, so it keeps opening
// streams on the old session).
func graceAgent(t *testing.T, w *relaytest.World, n *relaytest.Node, key, id string, drop bool) (conduit.LocalSession, conduit.LocalSession, <-chan *conduitv1.GoAway) {
	t.Helper()
	w.SetPrincipal(key, relay.Principal{Kind: registry.PrincipalAgent, ID: id, ProjectID: project,
		Agent: relay.AgentIncarnationFacts{LaunchID: "L1", Generation: 1}})
	cfg := echoConfig()
	var goAways <-chan *conduitv1.GoAway
	cfg.Interceptor, goAways = goAwayRecorder(drop)
	target, wel := n.MustDial(key, relaytest.AgentHello(id, "L1", "", "pty"), cfg)
	ls, _ := openEcho(t, n, wel.GetSessionId())
	return target, ls, goAways
}

// TestShutdownDrainCap: shutdown_grace - 2s, clamped at 0.
func TestShutdownDrainCap(t *testing.T) {
	for grace, want := range map[time.Duration]time.Duration{
		10 * time.Second: 8 * time.Second,
		30 * time.Second: 28 * time.Second,
		2 * time.Second:  0,
		time.Second:      0,
	} {
		if got := relay.ShutdownDrainCap(grace); got != want {
			t.Errorf("ShutdownDrainCap(%s) = %s, want %s", grace, got, want)
		}
	}
}

// TestShutdownGrace_GoAwayBeforeSentHook (criterion 26): a capped
// Shutdown with shutdown_grace = 10s sends GoAway to every session with
// drain_deadline_ms <= 8000, and reports GoAwaysSent only once every
// session has had its GoAway (the hub closes the listeners after that
// report). A StreamOpen the target sends on the old session after the
// GoAway is refused with 4503 relay_restart.
func TestShutdownGrace_GoAwayBeforeSentHook(t *testing.T) {
	w := relaytest.NewWorld(t)
	n := lifetimeNode(t, w, 0, 0)
	_, lsA, goAwaysA := graceAgent(t, w, n, "a", "agent-a", false)
	targetB, lsB, goAwaysB := graceAgent(t, w, n, "b", "agent-b", true)

	sentDraining := make(chan [2]bool, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- n.Relay.ShutdownWith(ctx, relay.ShutdownOptions{
			CapDrain: true,
			DrainCap: relay.ShutdownDrainCap(10 * time.Second),
			GoAwaysSent: func() {
				sentDraining <- [2]bool{lsA.Info().Draining, lsB.Info().Draining}
			},
		})
	}()
	if got := relaytest.Wait(t, sentDraining, "GoAwaysSent"); got != [2]bool{true, true} {
		t.Fatalf("at GoAwaysSent, sessions draining = %v, want both", got)
	}
	for name, ch := range map[string]<-chan *conduitv1.GoAway{"a": goAwaysA, "b": goAwaysB} {
		g := relaytest.Wait(t, ch, "GoAway at target "+name)
		if g.GetReason() != relay.ReasonDraining || g.GetDrainDeadlineMs() == 0 || g.GetDrainDeadlineMs() > 8000 {
			t.Fatalf("target %s GoAway = {%q, %dms}, want {draining, 1..8000ms}", name, g.GetReason(), g.GetDrainDeadlineMs())
		}
		if g.GetDrainDeadlineMs() != 8000 {
			t.Fatalf("target %s drain_deadline_ms = %d, want 8000 (10s grace - 2s)", name, g.GetDrainDeadlineMs())
		}
	}

	// B never saw its GoAway: a StreamOpen on the old session gets 4503.
	octx, ocancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer ocancel()
	_, err := targetB.OpenStream(octx, &conduitv1.StreamOpen{Kind: conduitv1.StreamKind_STREAM_KIND_PTY})
	var ce *conduit.CloseError
	if !errors.As(err, &ce) || ce.Code != conduit.CloseRelayRestart || !strings.HasPrefix(ce.Reason, relay.ReasonRelayRestart) {
		t.Fatalf("StreamOpen after GoAway = %v, want StreamClose{4503 relay_restart}", err)
	}

	// The drain ends at the 8s deadline.
	t0 := n.Clock.Now()
	waitTimer(t, n, t0.Add(8*time.Second), "capped drain deadline")
	n.Clock.Advance(8*time.Second - time.Millisecond)
	if isClosed(lsA.Done()) || isClosed(lsB.Done()) {
		t.Fatal("session closed before the 8s drain deadline")
	}
	n.Clock.Advance(time.Millisecond)
	relaytest.WaitClosed(t, lsA.Done(), "session a to end at 8s")
	relaytest.WaitClosed(t, lsB.Done(), "session b to end at 8s")
	if err := relaytest.Wait(t, result, "Shutdown to return"); err != nil {
		t.Fatalf("ShutdownWith = %v", err)
	}
}

// TestShutdownGrace_DrainDeadline pins the drain deadline a SIGTERM drain
// sends: min(drain_deadline_ms, shutdown_grace - 2s) when shutdown_grace
// is set, the 1ms floor when that is 0 or less (never 0, which is the 30s
// session default), and the unchanged 30s when it is unset.
func TestShutdownGrace_DrainDeadline(t *testing.T) {
	for _, tc := range []struct {
		name   string
		opts   relay.ShutdownOptions
		wantMs uint32
	}{
		{name: "unset (plain Shutdown)", opts: relay.ShutdownOptions{}, wantMs: 30000},
		{name: "grace 10s", opts: relay.ShutdownOptions{CapDrain: true, DrainCap: relay.ShutdownDrainCap(10 * time.Second)}, wantMs: 8000},
		{name: "grace 40s keeps 30s", opts: relay.ShutdownOptions{CapDrain: true, DrainCap: relay.ShutdownDrainCap(40 * time.Second)}, wantMs: 30000},
		{name: "grace 2s floors at 1ms", opts: relay.ShutdownOptions{CapDrain: true, DrainCap: relay.ShutdownDrainCap(2 * time.Second)}, wantMs: 1},
		{name: "grace 1s floors at 1ms", opts: relay.ShutdownOptions{CapDrain: true, DrainCap: relay.ShutdownDrainCap(time.Second)}, wantMs: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := relaytest.NewWorld(t)
			n := lifetimeNode(t, w, 0, 0)
			_, ls, goAways := graceAgent(t, w, n, "a", "agent-a", false)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- n.Relay.ShutdownWith(ctx, tc.opts) }()
			g := relaytest.Wait(t, goAways, "drain GoAway")
			if g.GetDrainDeadlineMs() != tc.wantMs {
				t.Fatalf("drain_deadline_ms = %d, want %d", g.GetDrainDeadlineMs(), tc.wantMs)
			}
			// End the drain: advance past the deadline once it is armed.
			waitTimer(t, n, n.Clock.Now().Add(time.Duration(tc.wantMs)*time.Millisecond), "drain deadline")
			n.Clock.Advance(time.Duration(tc.wantMs) * time.Millisecond)
			relaytest.WaitClosed(t, ls.Done(), "session to end at the drain deadline")
			if err := relaytest.Wait(t, result, "Shutdown to return"); err != nil {
				t.Fatalf("ShutdownWith = %v", err)
			}
		})
	}
}

// TestShutdownGrace_LifetimeGoAwayUnchanged: the SIGTERM cap applies only
// to the shutdown drain; a lifetime GoAway still carries the usual 30s
// drain deadline (the full lifetime behaviour is pinned by the
// TestLifetimeCap tests, which this change leaves untouched).
func TestShutdownGrace_LifetimeGoAwayUnchanged(t *testing.T) {
	w := relaytest.NewWorld(t)
	n := lifetimeNode(t, w, 120*time.Second, 0)
	t0 := n.Clock.Now()
	_, ls, goAways := graceAgent(t, w, n, "a", "agent-a", false)
	waitTimer(t, n, t0.Add(60*time.Second), "lifetime GoAway")
	n.Clock.Advance(60 * time.Second)
	g := relaytest.Wait(t, goAways, "lifetime GoAway")
	if g.GetReason() != relay.ReasonRelayRestart || g.GetDrainDeadlineMs() != 30000 {
		t.Fatalf("lifetime GoAway = {%q, %dms}, want {relay_restart, 30000ms}", g.GetReason(), g.GetDrainDeadlineMs())
	}
	if !ls.Info().Draining {
		t.Fatal("session not draining after the lifetime GoAway")
	}
}

// switchDialer dials first, then every later attempt dials next (a load
// balancer sending the replacement session to a surviving relay).
type switchDialer struct {
	first, next transport.Dialer
	dials       int
}

func (d *switchDialer) Dial(ctx context.Context) (transport.Conn, error) {
	d.dials++
	if d.dials == 1 {
		return d.first.Dial(ctx)
	}
	return d.next.Dial(ctx)
}

func nodeDialer(n *relaytest.Node, key string) transport.Dialer {
	return &ws.Dialer{URL: "ws" + strings.TrimPrefix(n.Public.URL, "http") + "/?principal=" + key}
}

// TestShutdownGrace_ReplacementBeforeOldEnds (criterion 26): after the
// SIGTERM GoAway the dialer opens a replacement session on another relay
// while the old session is still draining.
func TestShutdownGrace_ReplacementBeforeOldEnds(t *testing.T) {
	w := relaytest.NewWorld(t)
	a := lifetimeNode(t, w, 0, 0)
	b := w.StartNode("relay-b", nil)
	w.SetPrincipal("a", agentPrincipal("L1", 1))

	type welcome struct {
		s conduit.Session
		w *conduitv1.Welcome
	}
	sessions := make(chan welcome, 4)
	rc := &conduit.Reconnector{
		Dialer:    &switchDialer{first: nodeDialer(a, "a"), next: nodeDialer(b, "a")},
		Config:    echoConfig(),
		Hello:     func() *conduitv1.Hello { return relaytest.AgentHello(agentID, "L1", "", "pty") },
		Backoff:   &conduit.Backoff{Rand: func(int64) int64 { return 0 }},
		OnSession: func(_ context.Context, s conduit.Session, wel *conduitv1.Welcome) { sessions <- welcome{s, wel} },
	}
	rctx, rcancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- rc.Run(rctx) }()
	t.Cleanup(func() { rcancel(); <-runDone })

	first := relaytest.Wait(t, sessions, "first session")
	if got := first.w.GetRelayInstanceId(); got != "relay-a" {
		t.Fatalf("first session on %q, want relay-a", got)
	}
	ls, _ := openEcho(t, a, first.w.GetSessionId())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go func() {
		_ = a.Relay.ShutdownWith(ctx, relay.ShutdownOptions{CapDrain: true, DrainCap: relay.ShutdownDrainCap(10 * time.Second)})
	}()

	second := relaytest.Wait(t, sessions, "replacement session")
	if got := second.w.GetRelayInstanceId(); got != "relay-b" {
		t.Fatalf("replacement session on %q, want relay-b", got)
	}
	// relay-a's sessions run on its fake clock, which has not moved: the
	// old session is still draining when the replacement is up.
	if isClosed(ls.Done()) || !ls.Info().Draining {
		t.Fatalf("old session ended (%v) or not draining before the replacement came up", ls.Err())
	}
}
