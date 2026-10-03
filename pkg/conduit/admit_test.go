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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// slowAdmitter blocks Admit until released. If honourCtx is set it returns
// when its context ends; otherwise it ignores the context and returns its
// Welcome once released. It records every AbandonAdmission call.
type slowAdmitter struct {
	testAdmitter
	honourCtx bool
	entered   chan struct{}
	release   chan struct{}
	ctxErr    chan error
	welcome   *conduitv1.Welcome
	abandoned chan *conduitv1.Welcome
}

func newSlowAdmitter(honourCtx bool) *slowAdmitter {
	return &slowAdmitter{
		honourCtx: honourCtx,
		entered:   make(chan struct{}, 4),
		release:   make(chan struct{}),
		ctxErr:    make(chan error, 4),
		welcome:   &conduitv1.Welcome{SessionId: "late"},
		abandoned: make(chan *conduitv1.Welcome, 4),
	}
}

func (a *slowAdmitter) Admit(ctx context.Context, _ *conduitv1.Hello) (*conduitv1.Welcome, error) {
	a.entered <- struct{}{}
	if a.honourCtx {
		<-ctx.Done()
		a.ctxErr <- context.Cause(ctx)
		return nil, ctx.Err()
	}
	<-a.release
	return a.welcome, nil
}

func (a *slowAdmitter) AbandonAdmission(ctx context.Context, h *conduitv1.Hello, w *conduitv1.Welcome) {
	if ctx.Err() != nil || h.GetPrincipalId() != testHello().GetPrincipalId() {
		w = nil // reported as a wrong call
	}
	a.abandoned <- w
}

// expectAbandoned waits for exactly one AbandonAdmission call with want.
func expectAbandoned(t *testing.T, ch <-chan *conduitv1.Welcome, want *conduitv1.Welcome) {
	t.Helper()
	select {
	case w := <-ch:
		if w != want {
			t.Fatalf("AbandonAdmission(%v), want the admitter's Welcome %v with a live ctx and the hello", w, want)
		}
	case <-time.After(waitTimeout):
		t.Fatal("AbandonAdmission not called")
	}
	expectNotAbandoned(t, ch)
}

// expectNotAbandoned checks that no (further) AbandonAdmission call comes.
// A negative check needs a grace period; the call runs in a goroutine
// started before Accept returns, so a short one suffices.
func expectNotAbandoned(t *testing.T, ch <-chan *conduitv1.Welcome) {
	t.Helper()
	select {
	case w := <-ch:
		t.Fatalf("unexpected AbandonAdmission(%v)", w)
	case <-time.After(50 * time.Millisecond):
	}
}

// startAccept runs Accept against a raw dialer on a fake clock.
func startAccept(t *testing.T, ctx context.Context, adm Admitter) (*clock.Fake, transport.Conn, *rawPeer, <-chan error) {
	t.Helper()
	clk := clock.NewFake(t0)
	a, b := transport.Pipe(transport.MemoryOptions{Buffer: 8})
	errc := make(chan error, 1)
	go func() {
		_, err := Accept(ctx, a, Config{Clock: clk}, adm)
		errc <- err
	}()
	return clk, b, &rawPeer{t: t, conn: b}, errc
}

// TestAcceptAdmitBoundedByHandshakeTimeout: Admit is bounded by the time
// left of HandshakeTimeout, measured from the start of Accept. At the
// deadline the dialer gets GoAway{4504} without a drain deadline and the
// transport closes; a Welcome Admit returns later is never sent
// (1a-r2-F1).
func TestAcceptAdmitBoundedByHandshakeTimeout(t *testing.T) {
	for _, tc := range []struct {
		name      string
		honourCtx bool
	}{{"admitter honours ctx", true}, {"admitter ignores ctx", false}} {
		t.Run(tc.name, func(t *testing.T) {
			clk := clock.NewFake(t0)
			adm := newSlowAdmitter(tc.honourCtx)
			a, b := transport.Pipe(transport.MemoryOptions{Buffer: 8})
			raw := &rawPeer{t: t, conn: b}
			errc := make(chan error, 1)
			go func() {
				_, err := Accept(context.Background(), a, Config{Clock: clk}, adm)
				errc <- err
			}()
			// The Hello arrives 4s into the handshake: Admit gets the
			// remaining 6s.
			if !clk.WaitFor(waitTimeout, func(n int) bool { return n == 1 }) {
				t.Fatal("handshake timer not armed")
			}
			clk.Advance(4 * time.Second)
			raw.send(&conduitv1.Frame{Body: &conduitv1.Frame_Hello{Hello: testHello()}})
			select {
			case <-adm.entered:
			case <-time.After(waitTimeout):
				t.Fatal("Admit not called")
			}
			if !clk.WaitFor(waitTimeout, func(n int) bool { return n == 1 }) {
				t.Fatal("admission timer not armed")
			}
			clk.Advance(DefaultHandshakeTimeout - 4*time.Second - time.Millisecond)
			select {
			case err := <-errc:
				t.Fatalf("Accept returned before the deadline: %v", err)
			default:
			}
			clk.Advance(time.Millisecond)

			ga := raw.recv().GetGoAway()
			if ga.GetCode() != CloseRelayTimeout || ga.GetDrainDeadlineMs() != 0 {
				t.Fatalf("rejection = %v, want GoAway 4504 without a drain deadline", ga)
			}
			if plannedDrain(ga) {
				t.Fatal("4504 rejection classified as a planned drain")
			}
			if code, _ := closeCode(<-errc); code != CloseRelayTimeout {
				t.Fatal("Accept did not fail with 4504")
			}
			if tc.honourCtx {
				if code, _ := closeCode(<-adm.ctxErr); code != CloseRelayTimeout {
					t.Fatal("Admit ctx not cancelled with the 4504 cause")
				}
			} else {
				close(adm.release) // the late Welcome is discarded
			}
			if _, err := b.ReadFrame(); err == nil {
				t.Fatal("frame after the rejection; want the transport closed")
			}
			if tc.honourCtx {
				expectNotAbandoned(t, adm.abandoned) // Admit failed
			} else {
				expectAbandoned(t, adm.abandoned, adm.welcome)
			}
		})
	}
}

// TestReconnectorBacksOffOnAdmitTimeout: a dial rejected with 4504 because
// admission timed out is a failure on the dialer side and backs off.
func TestReconnectorBacksOffOnAdmitTimeout(t *testing.T) {
	clk := clock.NewFake(t0)
	adm := newSlowAdmitter(true)
	dialer := transport.DialerFunc(func(context.Context) (transport.Conn, error) {
		a, b := transport.Pipe(transport.MemoryOptions{Buffer: 8})
		go func() { _, _ = Accept(context.Background(), b, Config{Clock: clk}, adm) }()
		return a, nil
	})
	type dialErr struct {
		err   error
		delay time.Duration
	}
	errs := make(chan dialErr, 4)
	ceilings := make(chan time.Duration, 4)
	r := &Reconnector{
		Dialer: dialer,
		// The dialer's own handshake wait outlasts the relay's.
		Config: Config{Clock: clk, HandshakeTimeout: time.Hour},
		Hello:  testHello,
		OnDialError: func(err error, d time.Duration) {
			errs <- dialErr{err, d}
		},
		Backoff: &Backoff{Rand: func(n int64) int64 {
			ceilings <- time.Duration(n - 1)
			return n - 1
		}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	select {
	case <-adm.entered:
	case <-time.After(waitTimeout):
		t.Fatal("Admit not called")
	}
	// The dialer's 1h handshake timer and the relay's admission timer.
	if !clk.WaitFor(waitTimeout, func(n int) bool { return n == 2 }) {
		t.Fatal("timers not armed")
	}
	clk.Advance(DefaultHandshakeTimeout)
	select {
	case e := <-errs:
		if code, _ := closeCode(e.err); code != CloseRelayTimeout {
			t.Fatalf("dial err = %v, want 4504", e.err)
		}
		if e.delay != time.Second {
			t.Fatalf("redial delay %v, want the 1s backoff", e.delay)
		}
	case <-time.After(waitTimeout):
		t.Fatal("no dial error")
	}
	if c := <-ceilings; c != time.Second {
		t.Fatalf("backoff ceiling %v, want 1s", c)
	}
}

// tieAdmitter succeeds exactly as the admission deadline fires: it moves
// the fake clock to the deadline itself before returning its Welcome.
type tieAdmitter struct {
	*slowAdmitter
	clk *clock.Fake
}

func (a *tieAdmitter) Admit(context.Context, *conduitv1.Hello) (*conduitv1.Welcome, error) {
	// Wait for the admission timer: Admit starts before it is armed.
	a.clk.WaitFor(waitTimeout, func(n int) bool { return n == 1 })
	a.clk.Advance(DefaultHandshakeTimeout)
	return a.welcome, nil
}

// TestAcceptAdmitTieAbandoned: a Welcome that arrives as the deadline
// fires loses to it; the dialer gets 4504 and the admitter is told once
// (1a-r3-F1). Either select branch may run; both must behave the same.
func TestAcceptAdmitTieAbandoned(t *testing.T) {
	for i := 0; i < 20; i++ {
		adm := &tieAdmitter{slowAdmitter: newSlowAdmitter(false)}
		clk, _, raw, errc := startAccept(t, context.Background(), adm)
		adm.clk = clk
		raw.send(&conduitv1.Frame{Body: &conduitv1.Frame_Hello{Hello: testHello()}})
		if ga := raw.recv().GetGoAway(); ga.GetCode() != CloseRelayTimeout {
			t.Fatalf("iteration %d: rejection %v, want GoAway 4504", i, ga)
		}
		if code, _ := closeCode(<-errc); code != CloseRelayTimeout {
			t.Fatalf("iteration %d: Accept did not fail with 4504", i)
		}
		expectAbandoned(t, adm.abandoned, adm.welcome)
	}
}

// abandonCounter is a testAdmitter that records AbandonAdmission calls.
type abandonCounter struct {
	testAdmitter
	abandoned chan *conduitv1.Welcome
}

func (a *abandonCounter) AbandonAdmission(_ context.Context, _ *conduitv1.Hello, w *conduitv1.Welcome) {
	a.abandoned <- w
}

// TestAdmitNotAbandonedOnSuccessOrError: AbandonAdmission is not called
// for a session that started or for an Admit error.
func TestAdmitNotAbandonedOnSuccessOrError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{{"success", nil}, {"admit error", Reject(CloseForbidden, "no")}} {
		t.Run(tc.name, func(t *testing.T) {
			adm := &abandonCounter{testAdmitter: testAdmitter{admitErr: tc.err}, abandoned: make(chan *conduitv1.Welcome, 4)}
			_, _, raw, errc := startAccept(t, context.Background(), adm)
			raw.send(&conduitv1.Frame{Body: &conduitv1.Frame_Hello{Hello: testHello()}})
			f := raw.recv()
			err := <-errc
			if tc.err == nil && (f.GetWelcome() == nil || err != nil) {
				t.Fatalf("got %v, %v; want a Welcome", f, err)
			}
			if tc.err != nil && f.GetGoAway().GetCode() != CloseForbidden {
				t.Fatalf("got %v; want GoAway 4403", f)
			}
			expectNotAbandoned(t, adm.abandoned)
		})
	}
}

// cancelAdmitter returns when its ctx ends: with an error (as an admitter
// whose backend call was cancelled would) or, if succeed is set, with a
// Welcome anyway.
type cancelAdmitter struct {
	*slowAdmitter
	succeed bool
}

func (a *cancelAdmitter) Admit(ctx context.Context, _ *conduitv1.Hello) (*conduitv1.Welcome, error) {
	a.entered <- struct{}{}
	<-ctx.Done()
	if a.succeed {
		return a.welcome, nil
	}
	return nil, errors.New("backend: context canceled")
}

// TestAcceptCancelledDuringAdmitIsQuiet: when Accept's ctx ends during
// Admit, the transport closes without a GoAway, even if the admitter's
// result (caused by the cancellation) arrives first; a successful result
// is abandoned (1a-r3-F2).
func TestAcceptCancelledDuringAdmitIsQuiet(t *testing.T) {
	for _, succeed := range []bool{false, true} {
		t.Run(fmt.Sprintf("succeed=%v", succeed), func(t *testing.T) {
			for i := 0; i < 20; i++ {
				adm := &cancelAdmitter{slowAdmitter: newSlowAdmitter(true), succeed: succeed}
				ctx, cancel := context.WithCancel(context.Background())
				_, b, raw, errc := startAccept(t, ctx, adm)
				raw.send(&conduitv1.Frame{Body: &conduitv1.Frame_Hello{Hello: testHello()}})
				<-adm.entered
				cancel()
				if err := <-errc; !errors.Is(err, context.Canceled) {
					t.Fatalf("Accept err = %v, want context.Canceled", err)
				}
				if bs, err := b.ReadFrame(); err == nil {
					t.Fatalf("frame %x after cancellation; want the transport closed quietly", bs)
				}
				if succeed {
					expectAbandoned(t, adm.abandoned, adm.welcome)
				}
			}
		})
	}
}
