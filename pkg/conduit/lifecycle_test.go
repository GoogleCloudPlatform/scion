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
	"io"
	"math/rand/v2"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// preparation stands for a target-side resource (spawned pty, dialed
// socket) that must never outlive a cancelled opening (T8).
type preparation struct{ live atomic.Int64 }

// handler prepares, waits until told to accept, then accepts; on any
// cancellation it tears the preparation down.
func (p *preparation) handler(acceptGate func(ctx context.Context) bool) StreamHandler {
	return StreamHandlerFunc(func(ctx context.Context, o *conduitv1.StreamOpen, ps PendingStream) error {
		p.live.Add(1)
		go func() {
			release := func() { p.live.Add(-1) }
			if !acceptGate(ctx) {
				release()
				return
			}
			st, err := ps.Accept()
			if err != nil {
				release() // late accept: tear down
				return
			}
			// Accepted: the preparation lives as long as the stream.
			go func() {
				_, _ = io.Copy(io.Discard, st)
				<-ctx.Done()
				release()
			}()
		}()
		return nil
	})
}

func TestCancelDuringOpening(t *testing.T) {
	prep := &preparation{}
	entered := make(chan struct{})
	accept := make(chan struct{})
	var acceptErr error
	acceptDone := make(chan struct{})
	h := StreamHandlerFunc(func(ctx context.Context, o *conduitv1.StreamOpen, ps PendingStream) error {
		prep.live.Add(1)
		close(entered)
		go func() {
			defer close(acceptDone)
			<-ctx.Done() // the opener's cancel reaches the target as ctx cancellation
			prep.live.Add(-1)
			<-accept
			_, acceptErr = ps.Accept()
		}()
		return nil
	})
	var closes []*conduitv1.StreamClose
	var mu sync.Mutex
	rec := func(d Direction, f *conduitv1.Frame) []*conduitv1.Frame {
		if d == Inbound && f.GetStreamClose() != nil {
			mu.Lock()
			closes = append(closes, f.GetStreamClose())
			mu.Unlock()
		}
		return []*conduitv1.Frame{f}
	}
	p := newPair(t, Config{Interceptor: rec}, Config{StreamHandler: h})
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := p.dialer.OpenStream(ctx, tcpOpen())
		errc <- err
	}()
	<-entered
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("OpenStream err = %v", err)
	}
	eventually(t, "preparation torn down", func() bool { return prep.live.Load() == 0 })
	close(accept)
	<-acceptDone
	if !errors.Is(acceptErr, ErrStreamCancelled) {
		t.Fatalf("late Accept err = %v, want ErrStreamCancelled", acceptErr)
	}
	// The late Accept sends StreamClose, never StreamAccept.
	eventually(t, "late-accept StreamClose", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(closes) == 1 && closes[0].GetCode() == CloseCancelled
	})
	eventually(t, "no streams left", func() bool {
		return p.dialer.Stats().OpenStreams == 0 && p.relay.Stats().OpenStreams == 0
	})
}

func TestCancelSends4499OnTheWire(t *testing.T) {
	s, raw := dialAgainstRaw(t, Config{}, transport.MemoryOptions{Buffer: 64})
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := s.OpenStream(ctx, tcpOpen())
		errc <- err
	}()
	o := raw.recvType("stream_open").GetStreamOpen()
	if o.GetOpenTimeoutMs() != uint32(DefaultOpenTimeout/time.Millisecond) || o.GetInitialWindow() != DefaultStreamWindow {
		t.Fatalf("defaults not applied: %v", o)
	}
	cancel()
	<-errc
	c := raw.recvType("stream_close").GetStreamClose()
	if c.GetStreamId() != o.GetStreamId() || c.GetCode() != CloseCancelled {
		t.Fatalf("got %v, want StreamClose{%d, 4499}", c, o.GetStreamId())
	}
}

// TestLateAcceptRejectedAndCleanedUp plays the target by hand: it accepts
// after the opener cancelled. The opener must answer the late StreamAccept
// with StreamClose{4499} and keep no stream.
func TestLateAcceptRejectedAndCleanedUp(t *testing.T) {
	s, raw := dialAgainstRaw(t, Config{}, transport.MemoryOptions{Buffer: 64})
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := s.OpenStream(ctx, tcpOpen())
		errc <- err
	}()
	o := raw.recvType("stream_open").GetStreamOpen()
	cancel()
	<-errc
	raw.recvType("stream_close")
	raw.send(&conduitv1.Frame{Body: &conduitv1.Frame_StreamAccept{StreamAccept: &conduitv1.StreamAccept{StreamId: o.GetStreamId(), InitialWindow: 1024}}})
	c := raw.recvType("stream_close").GetStreamClose()
	if c.GetStreamId() != o.GetStreamId() || c.GetCode() != CloseCancelled {
		t.Fatalf("late accept answered with %v", c)
	}
	if n := s.Stats().OpenStreams; n != 0 {
		t.Fatalf("opener keeps %d streams", n)
	}
	// Data for the dead stream is discarded without breaking the session.
	raw.send(&conduitv1.Frame{Body: &conduitv1.Frame_StreamData{StreamData: &conduitv1.StreamData{StreamId: o.GetStreamId(), Data: []byte("x")}}})
	raw.send(&conduitv1.Frame{Body: &conduitv1.Frame_Ping{Ping: &conduitv1.Ping{Nonce: 9}}})
	if pong := raw.recvType("pong").GetPong(); pong.GetNonce() != 9 {
		t.Fatalf("pong %v", pong)
	}
}

// TestCancelRacingAccept is the T8 mechanics test: many iterations of an
// opener cancel racing the target's Accept. Whatever wins, no preparation
// survives, neither side keeps a stream, and a stream the opener never
// received is never left open on the target.
func TestCancelRacingAccept(t *testing.T) {
	iterations := 500
	if testing.Short() {
		iterations = 100
	}
	prep := &preparation{}
	gate := func(ctx context.Context) bool {
		// Race the cancel: sometimes accept at once, sometimes after a
		// scheduler yield, sometimes only after the cancel was seen.
		switch rand.IntN(3) {
		case 0:
			return true
		case 1:
			for i := 0; i < rand.IntN(50); i++ {
				runtimeGosched()
			}
			return true
		default:
			select {
			case <-ctx.Done():
				return rand.IntN(2) == 0 // late accept half of the time
			case <-time.After(time.Millisecond):
				return true
			}
		}
	}
	p := newPair(t, Config{}, Config{StreamHandler: prep.handler(gate)})
	var won, cancelled int
	for i := 0; i < iterations; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		res := make(chan Stream, 1)
		go func() {
			st, _ := p.dialer.OpenStream(ctx, tcpOpen())
			res <- st
		}()
		for j := 0; j < rand.IntN(100); j++ {
			runtimeGosched()
		}
		cancel()
		if st := <-res; st != nil {
			won++
			_ = st.Close()
		} else {
			cancelled++
		}
	}
	eventually(t, "every preparation torn down", func() bool { return prep.live.Load() == 0 })
	eventually(t, "no stream left on either side", func() bool {
		return p.dialer.Stats().OpenStreams == 0 && p.relay.Stats().OpenStreams == 0
	})
	if p.dialer.isDone() || p.relay.isDone() {
		t.Fatalf("session ended: %v / %v", p.dialer.Err(), p.relay.Err())
	}
	t.Logf("%d iterations: accept won %d, cancel won %d", iterations, won, cancelled)
}

func TestOpenTimeout(t *testing.T) {
	prep := &preparation{}
	p := newPair(t, Config{WriteWait: testWriteWait}, Config{WriteWait: testWriteWait, StreamHandler: prep.handler(func(ctx context.Context) bool {
		<-ctx.Done()
		return true // accept after the timeout: must be rejected
	})})
	errc := make(chan error, 1)
	go func() {
		_, err := p.dialer.OpenStream(context.Background(), tcpOpen())
		errc <- err
	}()
	eventually(t, "target preparing", func() bool { return prep.live.Load() == 1 })
	settle(t, p.clk, 6) // ping+watchdog per side, opener and target open timers
	p.clk.Advance(DefaultOpenTimeout)
	if err := <-errc; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("OpenStream err = %v, want deadline exceeded", err)
	}
	eventually(t, "preparation torn down", func() bool { return prep.live.Load() == 0 })
	eventually(t, "no streams", func() bool {
		return p.dialer.Stats().OpenStreams == 0 && p.relay.Stats().OpenStreams == 0
	})
}

func TestOpenTimeoutBounded(t *testing.T) {
	tests := []struct {
		ms   uint32
		want time.Duration
	}{
		{0, DefaultOpenTimeout},
		{500, 500 * time.Millisecond},
		{3_600_000, MaxOpenTimeout},
	}
	for _, tc := range tests {
		if got := openTimeout(tc.ms); got != tc.want {
			t.Errorf("openTimeout(%d) = %v, want %v", tc.ms, got, tc.want)
		}
	}
	// The target enforces the bound on its own, even against an opener
	// that asks for an hour and never cancels.
	clk := clock.NewFake(t0)
	entered := make(chan context.Context, 1)
	s, raw := acceptAgainstRaw(t, Config{Clock: clk, PingInterval: time.Hour, PongWait: 2 * time.Hour, WriteWait: testWriteWait, StreamHandler: StreamHandlerFunc(func(ctx context.Context, _ *conduitv1.StreamOpen, _ PendingStream) error {
		entered <- ctx
		return nil
	})}, transport.MemoryOptions{Buffer: 64})
	raw.send(&conduitv1.Frame{Body: &conduitv1.Frame_StreamOpen{StreamOpen: &conduitv1.StreamOpen{StreamId: 1, Kind: conduitv1.StreamKind_STREAM_KIND_PTY, InitialWindow: 1024, OpenTimeoutMs: 3_600_000}}})
	hctx := <-entered
	settle(t, clk, 3) // ping, watchdog, open timer
	clk.Advance(MaxOpenTimeout - time.Millisecond)
	if hctx.Err() != nil {
		t.Fatal("handler ctx cancelled before the 60s bound")
	}
	clk.Advance(time.Millisecond)
	<-hctx.Done()
	c := raw.recvType("stream_close").GetStreamClose()
	if c.GetStreamId() != 1 || c.GetCode() != CloseCancelled {
		t.Fatalf("got %v, want StreamClose{1, 4499}", c)
	}
	if n := s.Stats().OpenStreams; n != 0 {
		t.Fatalf("target keeps %d streams", n)
	}
}

// TestStreamStateMachine walks opening → active → draining → closed, and
// active → failed for a nonzero close code.
func TestStreamStateMachine(t *testing.T) {
	pendings := make(chan PendingStream, 1)
	// Delay the opener's first StreamClose so the draining state is
	// observable while the close is still queued.
	holdClose := make(chan struct{})
	var held atomic.Bool
	delayClose := func(d Direction, f *conduitv1.Frame) []*conduitv1.Frame {
		if d == Outbound && f.GetStreamClose() != nil && held.CompareAndSwap(false, true) {
			<-holdClose
		}
		return []*conduitv1.Frame{f}
	}
	p := newPair(t, Config{Interceptor: delayClose}, Config{StreamHandler: StreamHandlerFunc(func(_ context.Context, _ *conduitv1.StreamOpen, ps PendingStream) error {
		pendings <- ps
		return nil
	})})
	open := func() (opener, target *stream) {
		res := make(chan Stream, 1)
		go func() {
			st, _ := p.dialer.OpenStream(context.Background(), tcpOpen())
			res <- st
		}()
		ps := <-pendings
		if got := ps.(pendingStream).st.State(); got != StateOpening {
			t.Fatalf("target before accept: %v", got)
		}
		ts, err := ps.Accept()
		if err != nil {
			t.Fatalf("Accept: %v", err)
		}
		os := <-res
		if os == nil {
			t.Fatal("OpenStream failed")
		}
		return os.(*stream), ts.(*stream)
	}

	opener, target := open()
	if opener.State() != StateActive || target.State() != StateActive {
		t.Fatalf("after accept: %v / %v", opener.State(), target.State())
	}
	// The stream is draining until its StreamClose reaches the wire.
	_ = opener.CloseWithCode(CloseNormal, "")
	eventually(t, "close held", held.Load)
	if got := opener.State(); got != StateDraining {
		t.Fatalf("after CloseWithCode: %v, want draining", got)
	}
	close(holdClose)
	eventually(t, "opener closed", func() bool { return opener.State() == StateClosed })
	eventually(t, "target closed", func() bool { return target.State() == StateClosed })
	if _, err := target.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("target read after normal close = %v, want EOF", err)
	}

	opener, target = open()
	_ = opener.CloseWithCode(CloseForbidden, "nope")
	eventually(t, "opener failed", func() bool { return opener.State() == StateFailed })
	eventually(t, "target failed", func() bool { return target.State() == StateFailed })
	var ce *CloseError
	if _, err := target.Read(make([]byte, 1)); !errors.As(err, &ce) || ce.Code != CloseForbidden {
		t.Fatalf("target read err = %v, want CloseError{4403}", err)
	}
	eventually(t, "no streams", func() bool {
		return p.dialer.Stats().OpenStreams == 0 && p.relay.Stats().OpenStreams == 0
	})
}

// runtimeGosched yields; kept in one place so the race loop reads clearly.
func runtimeGosched() { runtime.Gosched() }
