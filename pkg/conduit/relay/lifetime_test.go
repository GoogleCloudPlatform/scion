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
	"io"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay/relaytest"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// lifetimeNode starts a relay whose sessions run on the node's fake clock
// (so the lifetime timer and the drain deadline share one time base and
// nothing waits in real time), with keepalive pushed out of the way and
// the given lifetime cap and session drain deadline (0 = default).
func lifetimeNode(t *testing.T, w *relaytest.World, lifetimeCap, drain time.Duration) *relaytest.Node {
	t.Helper()
	return w.StartNode("relay-a", func(c *relay.Config) {
		c.LifetimeCap = lifetimeCap
		c.Session.Clock = c.Clock
		c.Session.PingInterval = time.Hour
		c.Session.PongWait = 2 * time.Hour
		c.Session.DrainDeadline = drain
	})
}

// goAwayRecorder is a target interceptor that records the GoAway frames
// the target receives and, with drop, hides them from the target (so it
// keeps opening streams on the old session).
func goAwayRecorder(drop bool) (conduit.Interceptor, <-chan *conduitv1.GoAway) {
	ch := make(chan *conduitv1.GoAway, 4)
	return func(dir conduit.Direction, f *conduitv1.Frame) []*conduitv1.Frame {
		if g := f.GetGoAway(); dir == conduit.Inbound && g != nil {
			select {
			case ch <- g:
			default:
			}
			if drop {
				return nil
			}
		}
		return []*conduitv1.Frame{f}
	}, ch
}

// openEcho opens a PTY stream from the relay to the target (echo
// handler) and checks one round trip.
func openEcho(t *testing.T, n *relaytest.Node, sessionID string) (conduit.LocalSession, conduit.Stream) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ls, _, ok := n.Relay.Local(ctx, sessionID)
	if !ok {
		t.Fatalf("session %s not held by the relay", sessionID)
	}
	st, err := ls.OpenStream(ctx, &conduitv1.StreamOpen{Kind: conduitv1.StreamKind_STREAM_KIND_PTY})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	echo(t, st, "hello")
	return ls, st
}

func echo(t *testing.T, st conduit.Stream, s string) {
	t.Helper()
	if _, err := st.Write([]byte(s)); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, len(s))
	if _, err := io.ReadFull(st, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != s {
		t.Fatalf("echo = %q, want %q", buf, s)
	}
}

// streamEnd reads st until it fails and returns the close error.
func streamEnd(t *testing.T, st conduit.Stream) *conduit.CloseError {
	t.Helper()
	res := make(chan error, 1)
	go func() {
		buf := make([]byte, 64)
		for {
			if _, err := st.Read(buf); err != nil {
				res <- err
				return
			}
		}
	}()
	err := relaytest.Wait(t, res, "stream to end")
	var ce *conduit.CloseError
	if !errors.As(err, &ce) {
		t.Fatalf("stream ended with %v, want a close code", err)
	}
	return ce
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// TestLifetimeCap_GoAwayLeadAndDrainDeadline: with a 90s cap the relay
// sends GoAway{4503 relay_restart} 60s before the cap (with the 5s
// reconnect window and a 30s drain deadline), marks the row draining and
// refuses new streams on the old session; the accepted stream keeps
// working until the drain deadline and is then closed with 4503
// relay_restart, 30s before the cap.
func TestLifetimeCap_GoAwayLeadAndDrainDeadline(t *testing.T) {
	w := relaytest.NewWorld(t)
	n := lifetimeNode(t, w, 90*time.Second, 0)
	w.SetPrincipal("a", agentPrincipal("L1", 1))
	t0 := n.Clock.Now()
	cfg := echoConfig()
	var goAways <-chan *conduitv1.GoAway
	cfg.Interceptor, goAways = goAwayRecorder(false)
	target, wel := n.MustDial("a", relaytest.AgentHello(agentID, "L1", "", "pty"), cfg)
	if got := wel.GetLifetimeHintS(); got != 30 {
		t.Fatalf("Welcome.lifetime_hint_s = %d, want 30 (seconds until GoAway)", got)
	}
	ls, st := openEcho(t, n, wel.GetSessionId())

	n.Clock.Advance(30*time.Second - time.Millisecond)
	if ls.Info().Draining {
		t.Fatal("GoAway sent more than 60s before the cap")
	}
	n.Clock.Advance(time.Millisecond)
	if !ls.Info().Draining {
		t.Fatal("no GoAway 60s before the cap")
	}
	if lead := t0.Add(90 * time.Second).Sub(n.Clock.Now()); lead < relay.LifetimeGoAwayLead {
		t.Fatalf("GoAway sent %s before the cap, want at least %s", lead, relay.LifetimeGoAwayLead)
	}
	g := relaytest.Wait(t, goAways, "GoAway at the target")
	if g.GetCode() != conduit.CloseRelayRestart || g.GetReason() != relay.ReasonRelayRestart {
		t.Fatalf("GoAway = {%d %q}, want {4503 relay_restart}", g.GetCode(), g.GetReason())
	}
	if g.GetReconnectAfterMs() != 5000 || g.GetDrainDeadlineMs() != 30000 {
		t.Fatalf("GoAway reconnect_after_ms=%d drain_deadline_ms=%d, want 5000 and 30000", g.GetReconnectAfterMs(), g.GetDrainDeadlineMs())
	}
	relaytest.WaitClosed(t, target.GoAwayReceived(), "GoAway")
	if rows := w.Sessions(registry.PrincipalAgent, agentID).Sessions; len(rows) != 1 || !rows[0].Session.Draining {
		t.Fatalf("session row not marked draining: %+v", rows)
	}

	// New streams on the old session are refused; the accepted one works.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := ls.OpenStream(ctx, &conduitv1.StreamOpen{Kind: conduitv1.StreamKind_STREAM_KIND_PTY}); !errors.Is(err, conduit.ErrDraining) {
		t.Fatalf("OpenStream after GoAway = %v, want ErrDraining", err)
	}
	echo(t, st, "during drain")

	n.Clock.Advance(30*time.Second - time.Millisecond)
	if isClosed(ls.Done()) {
		t.Fatal("session closed before the drain deadline")
	}
	n.Clock.Advance(time.Millisecond)
	ce := streamEnd(t, st)
	if ce.Code != conduit.CloseRelayRestart || ce.Reason != relay.ReasonRelayRestart {
		t.Fatalf("stream closed with {%d %q}, want {4503 relay_restart}", ce.Code, ce.Reason)
	}
	relaytest.WaitClosed(t, ls.Done(), "session to end at the drain deadline")
	if left := t0.Add(90 * time.Second).Sub(n.Clock.Now()); left < 0 {
		t.Fatalf("drain ended %s after the cap", -left)
	}
	relaytest.WaitClosed(t, target.Done(), "target session to end")
	if code := conduit.CodeOf(target.Err(), 0); code != conduit.CloseRelayRestart {
		t.Fatalf("target session ended with %v, want 4503", target.Err())
	}
}

// TestLifetimeCap_StreamOpenAfterGoAwayRefused: a StreamOpen the target
// sends on the old session after the lifetime GoAway (here the target
// never saw the GoAway) is answered with StreamClose{4503 relay_restart}.
func TestLifetimeCap_StreamOpenAfterGoAwayRefused(t *testing.T) {
	w := relaytest.NewWorld(t)
	n := lifetimeNode(t, w, 90*time.Second, 0)
	w.SetPrincipal("a", agentPrincipal("L1", 1))
	cfg := echoConfig()
	var goAways <-chan *conduitv1.GoAway
	cfg.Interceptor, goAways = goAwayRecorder(true)
	target, wel := n.MustDial("a", relaytest.AgentHello(agentID, "L1", "", "pty"), cfg)
	ls, _ := openEcho(t, n, wel.GetSessionId())

	n.Clock.Advance(30 * time.Second)
	if !ls.Info().Draining {
		t.Fatal("no lifetime GoAway")
	}
	_ = relaytest.Wait(t, goAways, "GoAway sent to the target")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := target.OpenStream(ctx, &conduitv1.StreamOpen{Kind: conduitv1.StreamKind_STREAM_KIND_PTY})
	var ce *conduit.CloseError
	if !errors.As(err, &ce) {
		t.Fatalf("StreamOpen after GoAway = %v, want a StreamClose", err)
	}
	if ce.Code != conduit.CloseRelayRestart || !strings.HasPrefix(ce.Reason, relay.ReasonRelayRestart) {
		t.Fatalf("StreamOpen after GoAway closed with {%d %q}, want {4503 relay_restart}", ce.Code, ce.Reason)
	}
}

// TestLifetimeCap_DrainDeadlineBoundedByCap: a drain deadline longer than
// the time left before the cap is cut to end exactly at the cap.
func TestLifetimeCap_DrainDeadlineBoundedByCap(t *testing.T) {
	w := relaytest.NewWorld(t)
	// Lifetime GoAway at t=60s with 60s left; the 90s session default
	// would end the drain 30s after the cap.
	n := lifetimeNode(t, w, 120*time.Second, 90*time.Second)
	w.SetPrincipal("a", agentPrincipal("L1", 1))
	t0 := n.Clock.Now()
	cfg := echoConfig()
	var goAways <-chan *conduitv1.GoAway
	cfg.Interceptor, goAways = goAwayRecorder(false)
	_, wel := n.MustDial("a", relaytest.AgentHello(agentID, "L1", "", "pty"), cfg)
	ls, st := openEcho(t, n, wel.GetSessionId())

	n.Clock.Advance(60 * time.Second)
	g := relaytest.Wait(t, goAways, "GoAway")
	if g.GetDrainDeadlineMs() != 60000 {
		t.Fatalf("drain_deadline_ms = %d, want 60000 (ends at the cap)", g.GetDrainDeadlineMs())
	}
	n.Clock.Advance(60*time.Second - time.Millisecond)
	if isClosed(ls.Done()) {
		t.Fatal("session closed before the drain deadline")
	}
	n.Clock.Advance(time.Millisecond)
	if ce := streamEnd(t, st); ce.Code != conduit.CloseRelayRestart {
		t.Fatalf("stream closed with %d, want 4503", ce.Code)
	}
	relaytest.WaitClosed(t, ls.Done(), "session to end by the cap")
	if !n.Clock.Now().Equal(t0.Add(120 * time.Second)) {
		t.Fatalf("drain ended at +%s, want the cap (+2m0s)", n.Clock.Now().Sub(t0))
	}
}

// TestLifetimeCap_ShutdownDrainNotExtended: a Shutdown drain that began
// before the lifetime GoAway keeps its deadline: the lifetime timer firing
// during the drain neither extends it nor sends a second GoAway, and the
// stream still open at the deadline is closed with 4503 relay_restart.
func TestLifetimeCap_ShutdownDrainNotExtended(t *testing.T) {
	w := relaytest.NewWorld(t)
	n := lifetimeNode(t, w, 120*time.Second, 0)
	w.SetPrincipal("a", agentPrincipal("L1", 1))
	t0 := n.Clock.Now()
	cfg := echoConfig()
	var goAways <-chan *conduitv1.GoAway
	cfg.Interceptor, goAways = goAwayRecorder(false)
	_, wel := n.MustDial("a", relaytest.AgentHello(agentID, "L1", "", "pty"), cfg)
	ls, st := openEcho(t, n, wel.GetSessionId())

	n.Clock.Advance(50 * time.Second) // lifetime GoAway would be at +60s
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- n.Relay.Shutdown(ctx) }()
	g := relaytest.Wait(t, goAways, "drain GoAway")
	if g.GetReason() != relay.ReasonDraining || g.GetDrainDeadlineMs() != 30000 {
		t.Fatalf("drain GoAway = {%q, %dms}, want {draining, 30000ms}", g.GetReason(), g.GetDrainDeadlineMs())
	}

	n.Clock.Advance(10 * time.Second) // +60s: the lifetime timer fires
	n.Clock.Advance(20*time.Second - time.Millisecond)
	if isClosed(ls.Done()) {
		t.Fatal("session closed before the drain deadline")
	}
	select {
	case g := <-goAways:
		t.Fatalf("second GoAway during the drain: %v", g)
	default:
	}
	n.Clock.Advance(time.Millisecond) // +80s: the Shutdown drain deadline
	ce := streamEnd(t, st)
	if ce.Code != conduit.CloseRelayRestart || ce.Reason != relay.ReasonRelayRestart {
		t.Fatalf("stream closed with {%d %q}, want {4503 relay_restart}", ce.Code, ce.Reason)
	}
	relaytest.WaitClosed(t, ls.Done(), "session to end at the drain deadline")
	if got := n.Clock.Now().Sub(t0); got != 80*time.Second {
		t.Fatalf("drain ended at +%s, want +1m20s", got)
	}
	if err := relaytest.Wait(t, result, "Shutdown to return"); err != nil {
		t.Fatalf("Shutdown = %v", err)
	}
}

// TestLifetimeCap_NoCapNoGoAway: without a cap no GoAway is ever sent.
func TestLifetimeCap_NoCapNoGoAway(t *testing.T) {
	w := relaytest.NewWorld(t)
	n := lifetimeNode(t, w, 0, 0)
	w.SetPrincipal("a", agentPrincipal("L1", 1))
	_, wel := n.MustDial("a", relaytest.AgentHello(agentID, "L1", "", "pty"), echoConfig())
	if wel.GetLifetimeHintS() != 0 {
		t.Fatalf("lifetime_hint_s = %d without a cap", wel.GetLifetimeHintS())
	}
	ls, _ := openEcho(t, n, wel.GetSessionId())
	n.Clock.Advance(24 * time.Hour)
	if ls.Info().Draining || isClosed(ls.Done()) {
		t.Fatal("session drained without a lifetime cap")
	}
}

// TestLifetimeCap_ConfigRejected: a cap that leaves no room for the 60s
// GoAway lead, or a negative one, is refused by New.
func TestLifetimeCap_ConfigRejected(t *testing.T) {
	w := relaytest.NewWorld(t)
	for _, c := range []time.Duration{-time.Second, time.Second, relay.LifetimeGoAwayLead} {
		if _, err := w.NewNode("relay-x", func(cfg *relay.Config) { cfg.LifetimeCap = c }); err == nil {
			t.Errorf("LifetimeCap %s accepted", c)
		}
	}
	if _, err := w.NewNode("relay-y", func(cfg *relay.Config) { cfg.LifetimeCap = relay.LifetimeGoAwayLead + time.Second }); err != nil {
		t.Errorf("LifetimeCap 61s refused: %v", err)
	}
}
