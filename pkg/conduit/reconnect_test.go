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

package conduit

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

func TestBackoffCeilings(t *testing.T) {
	b := &Backoff{Rand: func(n int64) int64 { return n - 1 }} // always the ceiling
	want := []time.Duration{1, 2, 4, 8, 16, 32, 60, 60, 60}
	for i, w := range want {
		if got := b.Next(); got != w*time.Second {
			t.Fatalf("attempt %d: delay %v, want %v", i, got, w*time.Second)
		}
	}
	b.Reset()
	if got := b.Next(); got != time.Second {
		t.Fatalf("after Reset: %v, want 1s", got)
	}
	// A long-running reconnect loop never overflows.
	for i := 0; i < 200; i++ {
		b.Next()
	}
	if c := b.Ceiling(); c != BackoffMax {
		t.Fatalf("ceiling after 200 attempts = %v", c)
	}
}

// TestBackoffFullJitter checks the real random source: every delay lies in
// [0, ceiling], the samples cover the whole range and their mean is about
// ceiling/2 (full jitter, not equal or decorrelated jitter).
func TestBackoffFullJitter(t *testing.T) {
	const samples = 4000
	for attempt := 0; attempt < 8; attempt++ {
		var sum float64
		lo, hi := time.Duration(math.MaxInt64), time.Duration(0)
		var ceil time.Duration
		for i := 0; i < samples; i++ {
			b := &Backoff{attempt: attempt}
			ceil = b.Ceiling()
			d := b.Next()
			if d < 0 || d > ceil {
				t.Fatalf("attempt %d: delay %v outside [0, %v]", attempt, d, ceil)
			}
			sum += float64(d)
			lo, hi = min(lo, d), max(hi, d)
		}
		mean := sum / samples / float64(ceil)
		// Standard error of the mean of U(0,1) over 4000 samples ≈ 0.0046;
		// ±0.05 is > 10σ.
		if mean < 0.45 || mean > 0.55 {
			t.Errorf("attempt %d: mean %.3f·ceiling, want ≈0.5", attempt, mean)
		}
		if float64(lo) > 0.02*float64(ceil) || float64(hi) < 0.98*float64(ceil) {
			t.Errorf("attempt %d: samples span [%v, %v] of [0, %v]", attempt, lo, hi, ceil)
		}
	}
}

// reconnectHarness runs a Reconnector against scripted dial outcomes on a
// fake clock. Sessions use long keepalives so they end only when the test
// ends them.
type reconnectHarness struct {
	t        *testing.T
	clk      *clock.Fake
	plan     chan error // nil = succeed
	relays   chan *session
	ceilings chan time.Duration
	sessions chan Session
	cancel   context.CancelFunc
	done     chan error
	// fraction, when set, makes the RNG draw that fraction of its range
	// instead of the top.
	fraction atomic.Pointer[float64]
}

// drawFraction makes later draws return f of their range.
func (h *reconnectHarness) drawFraction(f float64) { h.fraction.Store(&f) }

func newReconnectHarness(t *testing.T) *reconnectHarness {
	return newReconnectHarnessWith(t, nil)
}

// newReconnectHarnessWith lets configure adjust the Reconnector before it
// runs.
func newReconnectHarnessWith(t *testing.T, configure func(*Reconnector)) *reconnectHarness {
	h := &reconnectHarness{
		t:        t,
		clk:      clock.NewFake(t0),
		plan:     make(chan error),
		relays:   make(chan *session, 8),
		ceilings: make(chan time.Duration, 64),
		sessions: make(chan Session, 8),
		done:     make(chan error, 1),
	}
	cfg := Config{Clock: h.clk, PingInterval: time.Hour, PongWait: 2 * time.Hour, WriteWait: testWriteWait}
	dialer := transport.DialerFunc(func(ctx context.Context) (transport.Conn, error) {
		var err error
		select {
		case err = <-h.plan:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if err != nil {
			return nil, err
		}
		a, b := transport.Pipe(transport.MemoryOptions{Buffer: 64})
		rcfg := cfg
		rcfg.StreamHandler = acceptAll(make(chan Stream, 16))
		go func() {
			s, err := Accept(context.Background(), b, rcfg, &testAdmitter{})
			if err == nil {
				h.relays <- s.(*session)
			}
		}()
		return a, nil
	})
	r := &Reconnector{
		Dialer: dialer,
		Config: cfg,
		Hello:  testHello,
		OnSession: func(_ context.Context, s Session, _ *conduitv1.Welcome) {
			h.sessions <- s
		},
		Backoff: &Backoff{Rand: func(n int64) int64 {
			h.ceilings <- time.Duration(n - 1)
			if f := h.fraction.Load(); f != nil {
				return int64(float64(n-1) * *f)
			}
			return n - 1
		}},
	}
	if configure != nil {
		configure(r)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() { h.done <- r.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-h.done
	})
	return h
}

func (h *reconnectHarness) expectCeiling(want time.Duration) {
	h.t.Helper()
	select {
	case got := <-h.ceilings:
		if got != want {
			h.t.Fatalf("backoff ceiling %v, want %v", got, want)
		}
	case <-time.After(waitTimeout):
		h.t.Fatalf("no backoff (want ceiling %v)", want)
	}
}

func (h *reconnectHarness) noCeiling() {
	h.t.Helper()
	select {
	case got := <-h.ceilings:
		h.t.Fatalf("unexpected backoff with ceiling %v", got)
	default:
	}
}

// waitTimer waits until a timer with deadline now+d is armed on h.clk.
// Unlike settle(t, h.clk, 1), it cannot return early while an ended
// session's keepalive timers are still being stopped (4->3->2->1->0)
// before the Reconnector arms its backoff or reconnect-window timer.
func (h *reconnectHarness) waitTimer(d time.Duration) {
	h.t.Helper()
	if !h.tryWaitTimer(d) {
		h.t.Fatalf("redial timer for %v was not armed (pending %d)", d, h.clk.Pending())
	}
}

func (h *reconnectHarness) tryWaitTimer(d time.Duration) bool {
	return h.clk.WaitForTimer(waitTimeout, h.clk.Now().Add(d))
}

// connect lets the next dial succeed and returns both ends.
func (h *reconnectHarness) connect() (Session, *session) {
	h.t.Helper()
	select {
	case h.plan <- nil:
	case <-time.After(waitTimeout):
		h.t.Fatal("reconnector never dialled")
	}
	var relay *session
	select {
	case relay = <-h.relays:
	case <-time.After(waitTimeout):
		h.t.Fatal("relay never accepted")
	}
	select {
	case s := <-h.sessions:
		return s, relay
	case <-time.After(waitTimeout):
		h.t.Fatal("OnSession not called")
		return nil, nil
	}
}

// TestReconnectorBackoffAndReset: failed dials back off 1s, 2s; a session
// that lived ≥60s resets the backoff; a short one does not.
func TestReconnectorBackoffAndReset(t *testing.T) {
	h := newReconnectHarness(t)
	for _, ceil := range []time.Duration{time.Second, 2 * time.Second} {
		h.plan <- errors.New("refused")
		h.expectCeiling(ceil)
		h.waitTimer(ceil)
		h.clk.Advance(ceil)
	}
	// Long session: 60s, then the relay drops it.
	_, relay := h.connect()
	settle(t, h.clk, 4) // ping + watchdog on each side
	h.clk.Advance(BackoffResetLive)
	_ = relay.Close()
	h.expectCeiling(time.Second) // reset
	h.waitTimer(time.Second)
	h.clk.Advance(time.Second)

	// Short session: no reset, the backoff keeps growing.
	_, relay = h.connect()
	_ = relay.Close()
	h.expectCeiling(2 * time.Second)
}

// age lets the current session live for d on the fake clock.
func (h *reconnectHarness) age(d time.Duration) {
	h.t.Helper()
	settle(h.t, h.clk, 4) // ping + watchdog on each side
	h.clk.Advance(d)
}

// TestReconnectorReplacesOnGoAway: a planned GoAway triggers an immediate
// redial, with no backoff, while the old session keeps draining.
func TestReconnectorReplacesOnGoAway(t *testing.T) {
	h := newReconnectHarness(t)
	old, relay := h.connect()
	h.age(MinPlannedDrainLife)
	// Keep a stream open so the old session drains instead of closing.
	if _, err := old.OpenStream(context.Background(), tcpOpen()); err != nil {
		t.Fatal(err)
	}
	if err := relay.GoAway(GoAwayOptions{}); err != nil {
		t.Fatal(err)
	}
	replacement, _ := h.connect()
	h.noCeiling()
	if replacement == old {
		t.Fatal("no new session")
	}
	if old.(LocalSession).Err() != nil {
		t.Fatalf("old session ended before draining: %v", old.(LocalSession).Err())
	}
	if !old.Info().Draining {
		t.Fatal("old session not draining")
	}
}

// TestReconnectorHonoursReconnectAfter: the replacement dial waits the
// delay drawn from the relay's reconnect window. The harness RNG draws
// the top of the window, so the dial must not happen before it. A window
// wider than the backoff maximum is honoured too.
func TestReconnectorHonoursReconnectAfter(t *testing.T) {
	for _, window := range []time.Duration{5 * time.Second, 90 * time.Second} {
		t.Run(window.String(), func(t *testing.T) {
			h := newReconnectHarness(t)
			_, relay := h.connect()
			h.age(MinPlannedDrainLife)
			if err := relay.GoAway(GoAwayOptions{ReconnectAfter: window}); err != nil {
				t.Fatal(err)
			}
			// The GoAway with no streams closes both sessions; wait for the
			// window timer itself rather than a pending count, since the ended
			// sessions' keepalive timers may still be stopping. Whether the
			// Reconnector saw Done or GoAwayReceived first, it classifies the
			// end from the session state.
			h.expectCeiling(window) // the window draw, not a backoff
			h.waitTimer(window)
			h.clk.Advance(window - time.Millisecond)
			select {
			case h.plan <- nil:
				t.Fatal("redialled before the drawn delay")
			default:
			}
			h.clk.Advance(time.Millisecond)
			h.connect()
			h.noCeiling()
		})
	}
}

// TestReconnectorGoAwayDrawsWithinWindow: the Reconnector waits exactly
// the value drawn from [0, reconnect_after_ms], here a point inside the
// window.
func TestReconnectorGoAwayDrawsWithinWindow(t *testing.T) {
	h := newReconnectHarness(t)
	h.drawFraction(0.3)
	_, relay := h.connect()
	h.age(MinPlannedDrainLife)
	if err := relay.GoAway(GoAwayOptions{ReconnectAfter: 10 * time.Second}); err != nil {
		t.Fatal(err)
	}
	h.expectCeiling(10 * time.Second)
	h.waitTimer(3 * time.Second)
	h.clk.Advance(3*time.Second - time.Millisecond)
	select {
	case h.plan <- nil:
		t.Fatal("redialled before the drawn delay")
	default:
	}
	h.clk.Advance(time.Millisecond)
	h.connect()
}

// TestReconnectorBacksOffOnFailureGoAway: a GoAway that is not a planned
// drain (4400 protocol error, 4401 auth) backs off like a failure, even on
// a long-lived session (1a-r1-F2).
func TestReconnectorBacksOffOnFailureGoAway(t *testing.T) {
	for _, code := range []uint32{CloseProtocolError, CloseUnauthenticated} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			h := newReconnectHarness(t)
			_, relay := h.connect()
			h.age(MinPlannedDrainLife)
			if err := relay.CloseWithCode(code, "failure"); err != nil {
				t.Fatal(err)
			}
			h.expectCeiling(time.Second)
			h.waitTimer(time.Second)
			h.clk.Advance(time.Second)
			// The next failure keeps growing the backoff.
			_, relay = h.connect()
			if err := relay.CloseWithCode(code, "failure"); err != nil {
				t.Fatal(err)
			}
			h.expectCeiling(2 * time.Second)
		})
	}
}

// TestReconnectorBacksOffOnEarlyPlannedDrain: a relay that drains fresh
// sessions straight away cannot cause a tight reconnect loop.
func TestReconnectorBacksOffOnEarlyPlannedDrain(t *testing.T) {
	h := newReconnectHarness(t)
	_, relay := h.connect()
	if err := relay.GoAway(GoAwayOptions{}); err != nil {
		t.Fatal(err)
	}
	h.expectCeiling(time.Second)
}

// TestRedialDelayOrderings: the end of a session is classified the same
// whether its GoAway is seen while the session lingers or after it has
// already closed, so the Reconnector's select order cannot change it.
func TestRedialDelayOrderings(t *testing.T) {
	// windowTop draws the top of the range and fails on a backoff draw
	// (any range other than the 3s window).
	noRand := func(t *testing.T) *Backoff {
		return &Backoff{Rand: func(n int64) int64 {
			if time.Duration(n-1) != 3*time.Second {
				t.Fatalf("unexpected draw over %v", time.Duration(n-1))
			}
			return n - 1
		}}
	}
	t.Run("GoAway then Done", func(t *testing.T) {
		accepted := make(chan Stream, 1)
		p := newPair(t, Config{}, Config{StreamHandler: acceptAll(accepted)})
		if _, err := p.dialer.OpenStream(context.Background(), tcpOpen()); err != nil {
			t.Fatal(err)
		}
		if err := p.relay.GoAway(GoAwayOptions{ReconnectAfter: 3 * time.Second}); err != nil {
			t.Fatal(err)
		}
		<-p.dialer.GoAwayReceived()
		if p.dialer.isDone() {
			t.Fatal("dialer ended; want it still draining")
		}
		if d := redialDelay(receivedGoAway(p.dialer), MinPlannedDrainLife, noRand(t)); d != 3*time.Second {
			t.Fatalf("delay %v, want the top of the 3s window", d)
		}
	})
	t.Run("Done already closed", func(t *testing.T) {
		p := newPair(t, Config{}, Config{})
		if err := p.relay.GoAway(GoAwayOptions{ReconnectAfter: 3 * time.Second}); err != nil {
			t.Fatal(err)
		}
		_ = waitDone(t, p.dialer)
		if d := redialDelay(receivedGoAway(p.dialer), MinPlannedDrainLife, noRand(t)); d != 3*time.Second {
			t.Fatalf("delay %v, want the top of the 3s window", d)
		}
	})
	t.Run("Done without GoAway", func(t *testing.T) {
		p := newPair(t, Config{}, Config{})
		_ = p.relay.Close()
		_ = waitDone(t, p.dialer)
		if ga := receivedGoAway(p.dialer); ga != nil {
			t.Fatalf("GoAway %v on a dropped session", ga)
		}
		bo := &Backoff{Rand: func(n int64) int64 { return n - 1 }}
		if d := redialDelay(nil, MinPlannedDrainLife, bo); d != time.Second {
			t.Fatalf("delay %v, want the 1s backoff ceiling", d)
		}
	})
	t.Run("classification", func(t *testing.T) {
		ceil := func(n int64) int64 { return n - 1 }
		for _, tc := range []struct {
			name  string
			ga    *conduitv1.GoAway
			lived time.Duration
			want  time.Duration
		}{
			{"planned 4503", &conduitv1.GoAway{Code: CloseRelayRestart, ReconnectAfterMs: 500}, MinPlannedDrainLife, 500 * time.Millisecond},
			{"planned by deadline", &conduitv1.GoAway{Code: CloseForbidden, DrainDeadlineMs: 1000}, MinPlannedDrainLife, 0},
			{"window capped", &conduitv1.GoAway{Code: CloseRelayRestart, ReconnectAfterMs: 3600_000}, MinPlannedDrainLife, MaxReconnectWindow},
			{"window wider than the backoff maximum", &conduitv1.GoAway{Code: CloseRelayRestart, ReconnectAfterMs: 90_000}, MinPlannedDrainLife, 90 * time.Second},
			{"protocol error", &conduitv1.GoAway{Code: CloseProtocolError}, time.Hour, time.Second},
			{"unauthenticated", &conduitv1.GoAway{Code: CloseUnauthenticated}, time.Hour, time.Second},
			{"relay timeout", &conduitv1.GoAway{Code: CloseRelayTimeout}, time.Hour, time.Second},
			{"early planned", &conduitv1.GoAway{Code: CloseRelayRestart}, time.Second, time.Second},
			{"early planned, longer window", &conduitv1.GoAway{Code: CloseRelayRestart, ReconnectAfterMs: 5000}, time.Second, 5 * time.Second},
		} {
			if d := redialDelay(tc.ga, tc.lived, &Backoff{Rand: ceil}); d != tc.want {
				t.Errorf("%s: delay %v, want %v", tc.name, d, tc.want)
			}
		}
	})
}

// TestReconnectHarnessWaitTimerIgnoresLingeringTimers guards against the
// race where an ended session's keepalive timers are still being stopped
// (4->3->2->1->0) while Reconnector.Run has not yet armed its backoff
// timer: waiting on a pending timer count of 1 would return early on a
// lingering timer, causing Advance to miss the redial timer and connect
// to deadlock.
func TestReconnectHarnessWaitTimerIgnoresLingeringTimers(t *testing.T) {
	inDecide := make(chan struct{})
	releaseDecide := make(chan struct{})
	h := newReconnectHarnessWith(t, func(r *Reconnector) {
		r.Decide = func(ctx context.Context, _ End, d time.Duration) (time.Duration, error) {
			close(inDecide)
			select {
			case <-releaseDecide:
				return d, nil
			case <-ctx.Done():
				return 0, ctx.Err()
			}
		}
	})
	// Arm a lingering timer (like an ended session's keepalive) so
	// h.clk.Pending() == 1 before Reconnector.Run reaches wait(d).
	stale := h.clk.AfterFunc(time.Hour, func() {})
	defer stale.Stop()

	h.plan <- errors.New("refused")
	h.expectCeiling(time.Second)
	select {
	case <-inDecide:
	case <-time.After(waitTimeout):
		t.Fatal("Decide not called")
	}

	waited := make(chan bool, 1)
	go func() {
		waited <- h.tryWaitTimer(time.Second)
	}()

	// With only the lingering 1h timer armed (Pending() == 1), tryWaitTimer
	// must not return before Decide releases and Reconnector arms its 1s
	// backoff timer.
	select {
	case <-waited:
		t.Fatal("waitTimer returned before redial timer was armed (fooled by lingering timer)")
	case <-time.After(20 * time.Millisecond):
	}

	close(releaseDecide)
	select {
	case ok := <-waited:
		if !ok {
			t.Fatalf("redial timer for %v was not armed (pending %d)", time.Second, h.clk.Pending())
		}
	case <-time.After(2 * waitTimeout):
		t.Fatal("waitTimer did not return after redial timer was armed")
	}
	stale.Stop()
	h.clk.Advance(time.Second)
	h.connect()
}

// TestRemoveSessionClearsBackingArray: removing an ended session clears
// its slot in the backing array, so the Reconnector does not keep it
// reachable.
func TestRemoveSessionClearsBackingArray(t *testing.T) {
	a, b, c := &session{}, &session{}, &session{}
	for _, tc := range []struct {
		name string
		rm   Session
		want []Session
	}{
		{"first", a, []Session{b, c}},
		{"middle", b, []Session{a, c}},
		{"last", c, []Session{a, b}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			live := []Session{a, b, c}
			got := removeSession(live, tc.rm)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("removeSession = %v, want %v", got, tc.want)
			}
			if tail := got[:cap(got)][len(got)]; tail != nil {
				t.Fatalf("vacated slot still holds %p", tail)
			}
		})
	}
	live := []Session{a}
	if got := removeSession(live, b); len(got) != 1 || got[0] != a {
		t.Fatalf("removing an absent session changed live: %v", got)
	}
}
