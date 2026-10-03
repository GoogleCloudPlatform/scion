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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// slowAdmitter blocks Admit until released. If honourCtx is set it returns
// when its context ends; otherwise it ignores the context and returns a
// Welcome once released.
type slowAdmitter struct {
	testAdmitter
	honourCtx bool
	entered   chan struct{}
	release   chan struct{}
	ctxErr    chan error
}

func newSlowAdmitter(honourCtx bool) *slowAdmitter {
	return &slowAdmitter{honourCtx: honourCtx, entered: make(chan struct{}, 4), release: make(chan struct{}), ctxErr: make(chan error, 4)}
}

func (a *slowAdmitter) Admit(ctx context.Context, _ *conduitv1.Hello) (*conduitv1.Welcome, error) {
	a.entered <- struct{}{}
	if a.honourCtx {
		<-ctx.Done()
		a.ctxErr <- context.Cause(ctx)
		return nil, ctx.Err()
	}
	<-a.release
	return &conduitv1.Welcome{SessionId: "late"}, nil
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
