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
	"math"
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
}

func newReconnectHarness(t *testing.T) *reconnectHarness {
	h := &reconnectHarness{
		t:        t,
		clk:      clock.NewFake(t0),
		plan:     make(chan error),
		relays:   make(chan *session, 8),
		ceilings: make(chan time.Duration, 64),
		sessions: make(chan Session, 8),
		done:     make(chan error, 1),
	}
	cfg := Config{Clock: h.clk, PingInterval: time.Hour, PongWait: 2 * time.Hour}
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
			return n - 1
		}},
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

// connect lets the next dial succeed and returns both ends.
func (h *reconnectHarness) connect() (Session, *session) {
	h.t.Helper()
	h.plan <- nil
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
		settle(t, h.clk, 1)
		h.clk.Advance(ceil)
	}
	// Long session: 60s, then the relay drops it.
	_, relay := h.connect()
	settle(t, h.clk, 4) // ping + watchdog on each side
	h.clk.Advance(BackoffResetLive)
	_ = relay.Close()
	h.expectCeiling(time.Second) // reset
	settle(t, h.clk, 1)
	h.clk.Advance(time.Second)

	// Short session: no reset, the backoff keeps growing.
	_, relay = h.connect()
	_ = relay.Close()
	h.expectCeiling(2 * time.Second)
}

// TestReconnectorReplacesOnGoAway: a GoAway triggers an immediate redial,
// with no backoff, while the old session keeps draining.
func TestReconnectorReplacesOnGoAway(t *testing.T) {
	h := newReconnectHarness(t)
	old, relay := h.connect()
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

// TestReconnectorHonoursReconnectAfter: the relay's reconnect hint delays
// the replacement dial.
func TestReconnectorHonoursReconnectAfter(t *testing.T) {
	h := newReconnectHarness(t)
	_, relay := h.connect()
	if err := relay.GoAway(GoAwayOptions{ReconnectAfter: 5 * time.Second}); err != nil {
		t.Fatal(err)
	}
	// The GoAway with no streams closes both sessions; only the hint
	// timer remains.
	settle(t, h.clk, 1)
	select {
	case h.plan <- nil:
		t.Fatal("redialled before reconnect_after_ms")
	default:
	}
	h.clk.Advance(5 * time.Second)
	h.connect()
	h.noCeiling()
}
