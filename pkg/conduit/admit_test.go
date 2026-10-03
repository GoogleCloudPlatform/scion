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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// abandonRecorder implements AdmitAbandoner. A call with an expired ctx
// or the wrong hello is recorded as nil, so it fails the comparison.
type abandonRecorder struct {
	mu        sync.Mutex
	abandoned []*conduitv1.Welcome
}

func (a *abandonRecorder) AbandonAdmission(ctx context.Context, h *conduitv1.Hello, w *conduitv1.Welcome) {
	if ctx.Err() != nil || h.GetPrincipalId() != testHello().GetPrincipalId() {
		w = nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.abandoned = append(a.abandoned, w)
}

func (a *abandonRecorder) calls() []*conduitv1.Welcome {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]*conduitv1.Welcome(nil), a.abandoned...)
}

// slowAdmitter blocks Admit until released. If honourCtx is set it returns
// when its context ends; otherwise it ignores the context and returns its
// Welcome once released.
type slowAdmitter struct {
	testAdmitter
	abandonRecorder
	honourCtx bool
	entered   chan struct{}
	release   chan struct{}
	ctxErr    chan error
	welcome   *conduitv1.Welcome
}

func newSlowAdmitter(honourCtx bool) *slowAdmitter {
	return &slowAdmitter{
		honourCtx: honourCtx,
		entered:   make(chan struct{}, 4),
		release:   make(chan struct{}),
		ctxErr:    make(chan error, 4),
		welcome:   &conduitv1.Welcome{SessionId: "late"},
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

// fixedAdmitter answers Admit at once with its Welcome or error, after
// running before (if set).
type fixedAdmitter struct {
	testAdmitter
	abandonRecorder
	welcome *conduitv1.Welcome
	err     error
	before  func()
}

func newFixedAdmitter(err error) *fixedAdmitter {
	return &fixedAdmitter{welcome: &conduitv1.Welcome{SessionId: "fixed"}, err: err}
}

func (a *fixedAdmitter) Admit(context.Context, *conduitv1.Hello) (*conduitv1.Welcome, error) {
	if a.before != nil {
		a.before()
	}
	if a.err != nil {
		return nil, a.err
	}
	return a.welcome, nil
}

// acceptRun is one Accept against a raw dialer. Its admitWG tracks the
// goroutines Accept leaves behind for discarded admissions.
type acceptRun struct {
	t    *testing.T
	wg   sync.WaitGroup
	conn transport.Conn // the dialer's end
	raw  *rawPeer
	errc chan error
}

// startAccept runs Accept(ctx, conn, cfg, adm); wrap, if set, wraps the
// relay's end of the pipe.
func startAccept(t *testing.T, ctx context.Context, cfg Config, adm Admitter, wrap func(transport.Conn) transport.Conn) *acceptRun {
	t.Helper()
	a, b := transport.Pipe(transport.MemoryOptions{Buffer: 8})
	relayEnd := transport.Conn(a)
	if wrap != nil {
		relayEnd = wrap(a)
	}
	r := &acceptRun{t: t, conn: b, raw: &rawPeer{t: t, conn: b}, errc: make(chan error, 1)}
	cfg.admitWG = &r.wg
	go func() {
		_, err := Accept(ctx, relayEnd, cfg, adm)
		r.errc <- err
	}()
	return r
}

func (r *acceptRun) hello() {
	r.raw.send(&conduitv1.Frame{Body: &conduitv1.Frame_Hello{Hello: testHello()}})
}

func (r *acceptRun) err() error {
	r.t.Helper()
	select {
	case err := <-r.errc:
		return err
	case <-time.After(waitTimeout):
		r.t.Fatal("Accept did not return")
		return nil
	}
}

// expectClosedQuietly checks that the transport closed with no further
// frame.
func (r *acceptRun) expectClosedQuietly() {
	r.t.Helper()
	if b, err := r.conn.ReadFrame(); err == nil {
		f := &conduitv1.Frame{}
		_ = proto.Unmarshal(b, f)
		r.t.Fatalf("got %s %v; want the transport closed without a frame", FrameType(f), f)
	}
}

// expectAbandons waits for Accept's leftover goroutines, then checks that
// AbandonAdmission was called exactly with want (none if want is empty).
func (r *acceptRun) expectAbandons(rec *abandonRecorder, want ...*conduitv1.Welcome) {
	r.t.Helper()
	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(waitTimeout):
		r.t.Fatal("Accept's admission goroutines did not finish")
	}
	got := rec.calls()
	if len(got) != len(want) {
		r.t.Fatalf("AbandonAdmission called %d times (%v), want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			r.t.Fatalf("AbandonAdmission(%v), want the admitter's own Welcome %v with a live ctx and the hello", got[i], want[i])
		}
	}
}

// TestAcceptAdmitBoundedByHandshakeTimeout: Admit is bounded by the time
// left of HandshakeTimeout, measured from the start of Accept. At the
// deadline the dialer gets GoAway{4504} without a drain deadline and the
// transport closes; a Welcome Admit returns later is never sent but is
// abandoned once (1a-r2-F1, 1a-r3-F1).
func TestAcceptAdmitBoundedByHandshakeTimeout(t *testing.T) {
	for _, tc := range []struct {
		name      string
		honourCtx bool
	}{{"admitter honours ctx", true}, {"admitter ignores ctx", false}} {
		t.Run(tc.name, func(t *testing.T) {
			clk := clock.NewFake(t0)
			adm := newSlowAdmitter(tc.honourCtx)
			r := startAccept(t, context.Background(), Config{Clock: clk}, adm, nil)
			// The Hello arrives 4s into the handshake: Admit gets the
			// remaining 6s.
			if !clk.WaitFor(waitTimeout, func(n int) bool { return n == 1 }) {
				t.Fatal("handshake timer not armed")
			}
			clk.Advance(4 * time.Second)
			r.hello()
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
			case err := <-r.errc:
				t.Fatalf("Accept returned before the deadline: %v", err)
			default:
			}
			clk.Advance(time.Millisecond)

			ga := r.raw.recv().GetGoAway()
			if ga.GetCode() != CloseRelayTimeout || ga.GetDrainDeadlineMs() != 0 {
				t.Fatalf("rejection = %v, want GoAway 4504 without a drain deadline", ga)
			}
			if plannedDrain(ga) {
				t.Fatal("4504 rejection classified as a planned drain")
			}
			if code, _ := closeCode(r.err()); code != CloseRelayTimeout {
				t.Fatal("Accept did not fail with 4504")
			}
			r.expectClosedQuietly()
			if tc.honourCtx {
				if code, _ := closeCode(<-adm.ctxErr); code != CloseRelayTimeout {
					t.Fatal("Admit ctx not cancelled with the 4504 cause")
				}
				r.expectAbandons(&adm.abandonRecorder) // Admit failed
			} else {
				close(adm.release) // the late Welcome is discarded
				r.expectAbandons(&adm.abandonRecorder, adm.welcome)
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

// tieClock is a Clock whose timers never fire and whose Stop reports
// that they already did: every timer looks as if it fired the instant a
// result arrived, without its callback running.
type tieClock struct{}

func (tieClock) Now() time.Time { return t0 }

func (tieClock) AfterFunc(time.Duration, func()) clock.Timer { return tieTimer{} }

type tieTimer struct{}

func (tieTimer) Stop() bool { return false }

// TestAcceptAdmitTieDropsResult: a Welcome that arrives in a tie with the
// admission deadline (the result is received but the timer can no longer
// be stopped) loses: the dialer gets 4504 and the Welcome is abandoned
// once (1a-r3-F1, 1a-r4-F1).
func TestAcceptAdmitTieDropsResult(t *testing.T) {
	adm := newFixedAdmitter(nil)
	r := startAccept(t, context.Background(), Config{Clock: tieClock{}}, adm, nil)
	r.hello()
	if ga := r.raw.recv().GetGoAway(); ga.GetCode() != CloseRelayTimeout || ga.GetDrainDeadlineMs() != 0 {
		t.Fatalf("rejection %v, want GoAway 4504 without a drain deadline", ga)
	}
	if code, _ := closeCode(r.err()); code != CloseRelayTimeout {
		t.Fatal("Accept did not fail with 4504")
	}
	r.expectClosedQuietly()
	r.expectAbandons(&adm.abandonRecorder, adm.welcome)
}

// TestAcceptAdmitDeadlineFiresAsAdmitReturns is a smoke test: Admit
// returns its Welcome just after moving the fake clock to the deadline.
// The fake clock fires the timer synchronously first, so this takes the
// timeout branch and the late-result path; TestAcceptAdmitTieDropsResult
// covers the tie branch.
func TestAcceptAdmitDeadlineFiresAsAdmitReturns(t *testing.T) {
	for i := 0; i < 20; i++ {
		clk := clock.NewFake(t0)
		adm := newFixedAdmitter(nil)
		adm.before = func() {
			// Admit starts before the admission timer is armed.
			clk.WaitFor(waitTimeout, func(n int) bool { return n == 1 })
			clk.Advance(DefaultHandshakeTimeout)
		}
		r := startAccept(t, context.Background(), Config{Clock: clk}, adm, nil)
		r.hello()
		if ga := r.raw.recv().GetGoAway(); ga.GetCode() != CloseRelayTimeout {
			t.Fatalf("iteration %d: rejection %v, want GoAway 4504", i, ga)
		}
		if code, _ := closeCode(r.err()); code != CloseRelayTimeout {
			t.Fatalf("iteration %d: Accept did not fail with 4504", i)
		}
		r.expectAbandons(&adm.abandonRecorder, adm.welcome)
	}
}

// TestAdmitNotAbandonedOnSuccessOrError: AbandonAdmission is not called
// for a session that started or for an Admit error.
func TestAdmitNotAbandonedOnSuccessOrError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{{"success", nil}, {"admit error", Reject(CloseForbidden, "no")}} {
		t.Run(tc.name, func(t *testing.T) {
			adm := newFixedAdmitter(tc.err)
			r := startAccept(t, context.Background(), Config{Clock: clock.NewFake(t0)}, adm, nil)
			r.hello()
			f := r.raw.recv()
			err := r.err()
			if tc.err == nil && (f.GetWelcome().GetSessionId() != "fixed" || err != nil) {
				t.Fatalf("got %v, %v; want the Welcome", f, err)
			}
			if tc.err != nil && f.GetGoAway().GetCode() != CloseForbidden {
				t.Fatalf("got %v; want GoAway 4403", f)
			}
			r.expectAbandons(&adm.abandonRecorder)
		})
	}
}

// flagCtx is a context whose Done channel never closes but whose Err
// reports cancellation once flagged: the cancellation is observed only
// when Accept checks ctx.Err() after receiving the Admit result.
type flagCtx struct {
	context.Context
	cancelled atomic.Bool
}

func (c *flagCtx) Err() error {
	if c.cancelled.Load() {
		return context.Canceled
	}
	return nil
}

// TestAcceptResultAfterCancelIsQuiet: an Admit result that arrives after
// Accept's ctx ended (seen by the result branch, not via Done) closes the
// transport without a GoAway: an error is not sent as 4403, and a Welcome
// is abandoned once (1a-r3-F2, 1a-r4-F1).
func TestAcceptResultAfterCancelIsQuiet(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{{"success", nil}, {"admit error", errors.New("backend: context canceled")}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := &flagCtx{Context: context.Background()}
			adm := newFixedAdmitter(tc.err)
			adm.before = func() { ctx.cancelled.Store(true) }
			r := startAccept(t, ctx, Config{Clock: clock.NewFake(t0)}, adm, nil)
			r.hello()
			if err := r.err(); !errors.Is(err, context.Canceled) {
				t.Fatalf("Accept err = %v, want context.Canceled", err)
			}
			r.expectClosedQuietly()
			if tc.err == nil {
				r.expectAbandons(&adm.abandonRecorder, adm.welcome)
			} else {
				r.expectAbandons(&adm.abandonRecorder)
			}
		})
	}
}

// failWelcomeConn fails the write of a Welcome frame.
type failWelcomeConn struct {
	transport.Conn
}

var errWelcomeWrite = errors.New("test: welcome write failed")

func (c failWelcomeConn) WriteFrame(b []byte) error {
	f := &conduitv1.Frame{}
	if err := proto.Unmarshal(b, f); err == nil && f.GetWelcome() != nil {
		return errWelcomeWrite
	}
	return c.Conn.WriteFrame(b)
}

// TestAcceptWelcomeWriteFailureAbandons: if the Welcome cannot be written
// the session never starts: Accept fails, the transport closes and the
// admitter's own (unmodified) Welcome is abandoned once (1a-r4-F1).
func TestAcceptWelcomeWriteFailureAbandons(t *testing.T) {
	adm := newFixedAdmitter(nil)
	r := startAccept(t, context.Background(), Config{Clock: clock.NewFake(t0)}, adm,
		func(c transport.Conn) transport.Conn { return failWelcomeConn{c} })
	r.hello()
	if err := r.err(); !errors.Is(err, errWelcomeWrite) {
		t.Fatalf("Accept err = %v, want the write error", err)
	}
	r.expectClosedQuietly()
	r.expectAbandons(&adm.abandonRecorder, adm.welcome)
	if adm.welcome.GetPingIntervalMs() != 0 {
		t.Fatal("Accept modified the admitter's Welcome")
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
// Admit, the transport closes without a GoAway and a Welcome Admit then
// returns is abandoned once. Admit sees the cancellation only after
// Accept's ctx is done, so this exercises the ctx.Done branch and the
// late-result path; TestAcceptResultAfterCancelIsQuiet covers a result
// that wins the race (1a-r3-F2).
func TestAcceptCancelledDuringAdmitIsQuiet(t *testing.T) {
	for _, succeed := range []bool{false, true} {
		t.Run(fmt.Sprintf("succeed=%v", succeed), func(t *testing.T) {
			adm := &cancelAdmitter{slowAdmitter: newSlowAdmitter(true), succeed: succeed}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := startAccept(t, ctx, Config{Clock: clock.NewFake(t0)}, adm, nil)
			r.hello()
			<-adm.entered
			cancel()
			if err := r.err(); !errors.Is(err, context.Canceled) {
				t.Fatalf("Accept err = %v, want context.Canceled", err)
			}
			r.expectClosedQuietly()
			if succeed {
				r.expectAbandons(&adm.abandonRecorder, adm.welcome)
			} else {
				r.expectAbandons(&adm.abandonRecorder)
			}
		})
	}
}
