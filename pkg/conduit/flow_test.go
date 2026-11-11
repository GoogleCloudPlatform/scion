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
	"bytes"
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// creditAuditor checks, at the receiver, that the sender never exceeded
// the credit the receiver granted: per stream (initial window + increments)
// and per session (4 MiB + session increments). Grants are counted when the
// receiver writes them, which is no later than the sender can use them, so
// the check is sound.
type creditAuditor struct {
	mu          sync.Mutex
	window      int64
	outstanding map[uint32]int64
	session     int64
	maxStream   int64
	maxSession  int64
	violations  []string
}

func newCreditAuditor(window int64) *creditAuditor {
	return &creditAuditor{window: window, outstanding: map[uint32]int64{}}
}

func (a *creditAuditor) intercept(d Direction, f *conduitv1.Frame) []*conduitv1.Frame {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case d == Inbound && f.GetStreamData() != nil:
		sd := f.GetStreamData()
		n := int64(len(sd.GetData()))
		a.outstanding[sd.GetStreamId()] += n
		a.session += n
		if o := a.outstanding[sd.GetStreamId()]; o > a.maxStream {
			a.maxStream = o
		}
		a.maxSession = max(a.maxSession, a.session)
		if a.outstanding[sd.GetStreamId()] > a.window {
			a.violations = append(a.violations, "stream credit exceeded")
		}
		if a.session > SessionWindow {
			a.violations = append(a.violations, "session credit exceeded")
		}
	case d == Outbound && f.GetStreamWindow() != nil:
		w := f.GetStreamWindow()
		if w.GetStreamId() == 0 {
			a.session -= int64(w.GetIncrement())
		} else {
			a.outstanding[w.GetStreamId()] -= int64(w.GetIncrement())
		}
	}
	return []*conduitv1.Frame{f}
}

func TestFlowControlNeverExceedsCredit(t *testing.T) {
	const window = 64 * 1024
	audit := newCreditAuditor(window)
	in := make(chan Stream, 8)
	p := newPair(t, Config{}, Config{StreamWindow: window, StreamHandler: acceptAll(in), Interceptor: audit.intercept})

	const streams = 6
	const size = 2 << 20 // 6 × 2 MiB = 12 MiB > session window
	var wg sync.WaitGroup
	for i := 0; i < streams; i++ {
		st, err := p.dialer.OpenStream(context.Background(), tcpOpen())
		if err != nil {
			t.Fatal(err)
		}
		rs := recvStream(t, in)
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = st.Write(bytes.Repeat([]byte{byte(i)}, size))
			_ = st.(*stream).CloseWrite()
		}()
		go func() {
			defer wg.Done()
			// Slow-ish reader: small reads.
			buf := make([]byte, 3000)
			total := 0
			for {
				n, err := rs.Read(buf)
				total += n
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Errorf("read: %v", err)
					return
				}
			}
			if total != size {
				t.Errorf("stream got %d bytes, want %d", total, size)
			}
		}()
	}
	wg.Wait()
	audit.mu.Lock()
	defer audit.mu.Unlock()
	if len(audit.violations) > 0 {
		t.Fatalf("credit violations: %v", audit.violations[:min(5, len(audit.violations))])
	}
	if audit.maxStream == 0 || audit.maxStream > window {
		t.Fatalf("max outstanding per stream %d (window %d)", audit.maxStream, window)
	}
	t.Logf("max outstanding: stream %d / %d, session %d / %d", audit.maxStream, window, audit.maxSession, SessionWindow)
}

func TestFlowControlViolationIsRefused(t *testing.T) {
	t.Run("stream credit", func(t *testing.T) {
		in := make(chan Stream, 1)
		s, raw := acceptAgainstRaw(t, Config{StreamWindow: 1024, StreamHandler: acceptAll(in)}, transport.MemoryOptions{Buffer: 64})
		raw.send(&conduitv1.Frame{Body: &conduitv1.Frame_StreamOpen{StreamOpen: &conduitv1.StreamOpen{StreamId: 1, Kind: conduitv1.StreamKind_STREAM_KIND_TCP, InitialWindow: 1024}}})
		if a := raw.recvType("stream_accept").GetStreamAccept(); a.GetInitialWindow() != 1024 {
			t.Fatalf("accept window %d", a.GetInitialWindow())
		}
		raw.send(&conduitv1.Frame{Body: &conduitv1.Frame_StreamData{StreamData: &conduitv1.StreamData{StreamId: 1, Data: make([]byte, 1025)}}})
		if c := raw.recvType("stream_close").GetStreamClose(); c.GetCode() != CloseProtocolError {
			t.Fatalf("got %v, want 4400", c)
		}
		rs := recvStream(t, in)
		if _, err := rs.Read(make([]byte, 10)); err == nil {
			t.Fatal("stream still readable after violation")
		}
		if s.isDone() {
			t.Fatal("a stream violation must not end the session")
		}
	})
	t.Run("session credit", func(t *testing.T) {
		in := make(chan Stream, 64)
		// A tiny RecvBufferLimit: session credit returns only on read,
		// and nothing is read.
		s, raw := acceptAgainstRaw(t, Config{StreamWindow: MaxWindow / 2, RecvBufferLimit: 1, StreamHandler: acceptAll(in)}, transport.MemoryOptions{Buffer: 1024})
		raw.send(&conduitv1.Frame{Body: &conduitv1.Frame_StreamOpen{StreamOpen: &conduitv1.StreamOpen{StreamId: 1, Kind: conduitv1.StreamKind_STREAM_KIND_TCP, InitialWindow: 1024}}})
		raw.recvType("stream_accept")
		// 4 MiB + one frame without any session window update.
		for i := 0; i <= SessionWindow/MaxDataFrame; i++ {
			raw.send(&conduitv1.Frame{Body: &conduitv1.Frame_StreamData{StreamData: &conduitv1.StreamData{StreamId: 1, Data: make([]byte, MaxDataFrame)}}})
		}
		if code, _ := closeCode(waitDone(t, s)); code != CloseProtocolError {
			t.Fatalf("session err %v, want 4400", s.Err())
		}
	})
}

func TestSlowReaderStallsOnlyItsStream(t *testing.T) {
	in := make(chan Stream, 2)
	p := newPair(t, Config{}, Config{StreamHandler: acceptAll(in)})
	stalled, err := p.dialer.OpenStream(context.Background(), tcpOpen())
	if err != nil {
		t.Fatal(err)
	}
	_ = recvStream(t, in) // never read
	busy, err := p.dialer.OpenStream(context.Background(), tcpOpen())
	if err != nil {
		t.Fatal(err)
	}
	busyR := recvStream(t, in)

	var stalledWritten atomic.Int64
	go func() {
		chunk := make([]byte, 1024)
		for {
			n, err := stalled.Write(chunk)
			stalledWritten.Add(int64(n))
			if err != nil {
				return
			}
		}
	}()
	eventually(t, "stalled stream to exhaust its credit", func() bool {
		return stalledWritten.Load() >= DefaultStreamWindow
	})

	const size = 16 << 20 // 16 MiB through the busy stream: 4× the session window
	go func() {
		_, _ = busy.Write(make([]byte, size))
		_ = busy.(*stream).CloseWrite()
	}()
	n, err := io.Copy(io.Discard, busyR)
	if err != nil || n != size {
		t.Fatalf("busy stream moved %d bytes, %v", n, err)
	}
	if got := stalledWritten.Load(); got > DefaultStreamWindow {
		t.Fatalf("stalled stream wrote %d bytes, more than its %d credit", got, DefaultStreamWindow)
	}
	if rb := p.relay.Stats().RecvBufferedBytes; rb > DefaultStreamWindow {
		t.Fatalf("relay buffers %d bytes for the stalled stream", rb)
	}
}

// TestControlFramesBypassBulkData blocks the dialer's transport while a
// stream's data is queued, then issues an RPC: the RPC request must be the
// next frame written, ahead of every queued data frame.
func TestControlFramesBypassBulkData(t *testing.T) {
	s, raw := dialAgainstRaw(t, Config{}, transport.MemoryOptions{Buffer: 0})
	openRes := make(chan Stream, 1)
	go func() {
		st, err := s.OpenStream(context.Background(), tcpOpen())
		if err != nil {
			t.Error(err)
		}
		openRes <- st
	}()
	o := raw.recvType("stream_open").GetStreamOpen()
	raw.send(&conduitv1.Frame{Body: &conduitv1.Frame_StreamAccept{StreamAccept: &conduitv1.StreamAccept{StreamId: o.GetStreamId(), InitialWindow: 1 << 20}}})
	st := <-openRes

	// Queue 1 MiB of data; the unbuffered pipe holds the writer on the first
	// frame because the raw peer is not reading.
	go func() { _, _ = st.Write(make([]byte, 1<<20)) }()
	// Wait until all 16 frames are queued and the writer holds the first.
	const frames = (1 << 20) / MaxDataFrame
	eventually(t, "writer blocked on a data frame", func() bool {
		return s.Stats().QueuedDataBytes == (frames-1)*(MaxDataFrame+dataFrameOverhead)
	})

	go func() { _, _ = s.Call(context.Background(), &conduitv1.RpcRequest{Method: "POST", Path: "/lifecycle"}) }()
	eventually(t, "rpc queued", func() bool { return s.Stats().QueuedControlBytes > 0 })

	// The frame already in the writer's hands is data; the very next one must
	// be the RPC.
	if typ := FrameType(raw.recv()); typ != "stream_data" {
		t.Fatalf("first frame %s, want the in-flight stream_data", typ)
	}
	if typ := FrameType(raw.recv()); typ != "rpc_request" {
		t.Fatalf("second frame %s, want rpc_request ahead of queued data", typ)
	}
}

func TestSchedulerControlFirstAndRoundRobin(t *testing.T) {
	sc := newScheduler(1 << 20)
	data := func(id uint32) *outFrame {
		return &outFrame{f: &conduitv1.Frame{Body: &conduitv1.Frame_StreamData{StreamData: &conduitv1.StreamData{StreamId: id, Data: []byte{1}}}}, class: classData, streamID: id, size: 1}
	}
	ctl := &outFrame{f: &conduitv1.Frame{Body: &conduitv1.Frame_Ping{Ping: &conduitv1.Ping{}}}, class: classControl, size: 1}
	never := make(chan struct{})
	for _, of := range []*outFrame{data(1), data(1), data(1), data(3), data(3), data(5)} {
		if err := sc.enqueue(of, never, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := sc.enqueue(ctl, never, nil); err != nil {
		t.Fatal(err)
	}
	var order []string
	for i := 0; i < 7; i++ {
		of, _ := sc.next()
		if of.class == classControl {
			order = append(order, "ctl")
		} else {
			order = append(order, string(rune('0'+of.streamID)))
		}
	}
	want := []string{"ctl", "1", "3", "5", "1", "3", "1"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order %v, want %v", order, want)
		}
	}
}

func TestAggregateBufferBudgetEnforced(t *testing.T) {
	const budget = 200 * 1024
	// The writer calls the interceptor as it dequeues a frame to write.
	var rpcDequeued atomic.Bool
	icpt := func(dir Direction, f *conduitv1.Frame) []*conduitv1.Frame {
		if dir == Outbound && f.GetRpcRequest() != nil {
			rpcDequeued.Store(true)
		}
		return []*conduitv1.Frame{f}
	}
	s, raw := dialAgainstRaw(t, Config{BufferBudget: budget, Interceptor: icpt}, transport.MemoryOptions{Buffer: 0})
	_ = raw

	// Two streams with plenty of credit (1 MiB each); together they would
	// queue 2 MiB, ten times the budget.
	var streams []Stream
	for i := 0; i < 2; i++ {
		res := make(chan Stream, 1)
		go func() {
			st, err := s.OpenStream(context.Background(), tcpOpen())
			if err != nil {
				t.Error(err)
			}
			res <- st
		}()
		o := raw.recvType("stream_open").GetStreamOpen()
		raw.send(&conduitv1.Frame{Body: &conduitv1.Frame_StreamAccept{StreamAccept: &conduitv1.StreamAccept{StreamId: o.GetStreamId(), InitialWindow: 1 << 20}}})
		streams = append(streams, <-res)
	}
	var written atomic.Int64
	for _, st := range streams {
		go func() {
			n, _ := st.Write(make([]byte, 1<<20))
			written.Add(int64(n))
		}()
	}
	// Data fills the budget up to the control share; with the session
	// credit (4 MiB) far above the budget, data alone would fill it all.
	dataLimit := int64(budget) - controlShareOf(budget)
	eventually(t, "budget filled with data", func() bool {
		return s.Stats().QueuedDataBytes >= dataLimit-MaxDataFrame-dataFrameOverhead
	})
	// A caller-originated control frame is still queued at once: it
	// does not wait for the data backlog to drain (1a-r1-F3).
	callCtx, cancelCall := context.WithCancel(context.Background())
	callDone := make(chan struct{})
	go func() {
		defer close(callDone)
		_, _ = s.Call(callCtx, &conduitv1.RpcRequest{Body: make([]byte, 64*1024)})
	}()
	// It is either still queued (the writer is stuck on a data frame) or
	// was already dequeued ahead of the queued data (the writer had not
	// yet taken the first data frame when the Call arrived).
	eventually(t, "call queued behind no data", func() bool {
		return s.Stats().QueuedControlBytes >= 64*1024 || rpcDequeued.Load()
	})
	if q := s.Stats().QueuedDataBytes; q > dataLimit {
		t.Fatalf("data queued %d beyond the data share %d", q, dataLimit)
	}

	st := s.Stats()
	if st.PeakQueuedBytes > budget {
		t.Fatalf("peak queued %d exceeds budget %d", st.PeakQueuedBytes, budget)
	}
	if st.Budget != budget {
		t.Fatalf("stats budget %d", st.Budget)
	}

	// Drain everything: the writers finish and the budget was never exceeded.
	go func() {
		for {
			if _, err := raw.conn.ReadFrame(); err != nil {
				return
			}
		}
	}()
	eventually(t, "writers finish", func() bool { return written.Load() == 2<<20 })
	eventually(t, "rpc written", func() bool { return s.Stats().ControlFramesSent >= 3 }) // 2 opens + rpc
	cancelCall()                                                                          // the raw peer never answers
	<-callDone
	if peak := s.Stats().PeakQueuedBytes; peak > budget {
		t.Fatalf("peak queued %d exceeds budget %d", peak, budget)
	}
}

func TestInternalFramesBeyondReserveEndSession(t *testing.T) {
	sc := newScheduler(1000)
	of := func() *outFrame { return &outFrame{class: classControl, size: 600} }
	never := make(chan struct{})
	if err := sc.enqueue(of(), never, nil); err != nil {
		t.Fatal(err)
	}
	var err error
	for i := 0; i < 1000 && err == nil; i++ {
		err = sc.enqueueInternal(of())
	}
	if err != ErrBufferBudget {
		t.Fatalf("err = %v, want ErrBufferBudget", err)
	}
	var st Stats
	sc.stats(&st)
	if st.QueuedBytes > 1000+controlReserve {
		t.Fatalf("queued %d beyond budget+reserve", st.QueuedBytes)
	}
}

func TestPurgeRestoresSessionCredit(t *testing.T) {
	s, raw := dialAgainstRaw(t, Config{}, transport.MemoryOptions{Buffer: 0})
	res := make(chan Stream, 1)
	go func() {
		st, _ := s.OpenStream(context.Background(), tcpOpen())
		res <- st
	}()
	o := raw.recvType("stream_open").GetStreamOpen()
	raw.send(&conduitv1.Frame{Body: &conduitv1.Frame_StreamAccept{StreamAccept: &conduitv1.StreamAccept{StreamId: o.GetStreamId(), InitialWindow: 1 << 20}}})
	st := <-res
	go func() { _, _ = st.Write(make([]byte, 512*1024)) }()
	eventually(t, "data queued", func() bool { return s.Stats().QueuedDataBytes >= 256*1024 })
	// The peer aborts the stream: queued data is purged and its session credit
	// returns (the peer never saw those bytes).
	raw.send(&conduitv1.Frame{Body: &conduitv1.Frame_StreamClose{StreamClose: &conduitv1.StreamClose{StreamId: o.GetStreamId(), Code: CloseForbidden}}})
	eventually(t, "purge", func() bool { return s.Stats().QueuedDataBytes == 0 })
	go func() {
		for {
			if _, err := raw.conn.ReadFrame(); err != nil {
				return
			}
		}
	}()
	eventually(t, "credit restored", func() bool {
		s.fcMu.Lock()
		defer s.fcMu.Unlock()
		// Only frames that actually reached the wire keep their credit
		// consumed; at most a couple of frames were in flight.
		return s.sendSession >= SessionWindow-3*MaxDataFrame
	})
}

// TestManyStalledReadersDoNotStallSession: more stalled streams than the
// session window can cover (20 × 256 KiB > 4 MiB) do not stall the other
// streams: within RecvBufferLimit session credit returns on receipt, and
// each stalled stream holds only its own window (1a-r1-F4).
func TestManyStalledReadersDoNotStallSession(t *testing.T) {
	accepted := make(chan Stream, 64)
	p := newPair(t, Config{}, Config{StreamHandler: acceptAll(accepted)})
	const stalled = SessionWindow/DefaultStreamWindow + 4
	for i := 0; i < stalled; i++ {
		st, err := p.dialer.OpenStream(context.Background(), tcpOpen())
		if err != nil {
			t.Fatal(err)
		}
		recvStream(t, accepted) // never read
		if _, err := st.Write(make([]byte, DefaultStreamWindow)); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "stalled windows delivered", func() bool {
		return p.relay.Stats().RecvBufferedBytes == stalled*DefaultStreamWindow
	})

	st, err := p.dialer.OpenStream(context.Background(), tcpOpen())
	if err != nil {
		t.Fatal(err)
	}
	target := recvStream(t, accepted)
	const n = 2 << 20
	go func() {
		if _, err := st.Write(bytes.Repeat([]byte{'x'}, n)); err != nil {
			t.Error(err)
		}
	}()
	got, err := io.ReadFull(target, make([]byte, n))
	if err != nil || got != n {
		t.Fatalf("live stream read %d, %v", got, err)
	}
	if peak := p.relay.Stats().PeakRecvBufferedBytes; peak > (stalled+1)*DefaultStreamWindow {
		t.Fatalf("receive buffering %d beyond the per-stream windows", peak)
	}
}

// TestRecvBufferLimitBoundsSessionMemory: beyond RecvBufferLimit session
// credit returns only as the application reads, so a session's unread
// data stays within RecvBufferLimit + SessionWindow.
func TestRecvBufferLimitBoundsSessionMemory(t *testing.T) {
	accepted := make(chan Stream, 64)
	const limit = 1 << 20
	p := newPair(t, Config{}, Config{StreamHandler: acceptAll(accepted), RecvBufferLimit: limit})
	const streams = (limit+SessionWindow)/DefaultStreamWindow + 4
	var targets []Stream
	for i := 0; i < streams; i++ {
		st, err := p.dialer.OpenStream(context.Background(), tcpOpen())
		if err != nil {
			t.Fatal(err)
		}
		targets = append(targets, recvStream(t, accepted))
		go func() { _, _ = st.Write(make([]byte, DefaultStreamWindow)) }()
	}
	bound := int64(limit + SessionWindow)
	eventually(t, "session window exhausted", func() bool {
		return p.relay.Stats().RecvBufferedBytes >= bound-int64(SessionWindow)/4
	})
	if peak := p.relay.Stats().PeakRecvBufferedBytes; peak > bound {
		t.Fatalf("buffered %d beyond RecvBufferLimit+SessionWindow %d", peak, bound)
	}
	// Reading releases credit and the stalled writers complete (read
	// concurrently: some streams have nothing delivered yet).
	var wg sync.WaitGroup
	for _, tg := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := io.ReadFull(tg, make([]byte, DefaultStreamWindow)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if peak := p.relay.Stats().PeakRecvBufferedBytes; peak > bound {
		t.Fatalf("buffered %d beyond RecvBufferLimit+SessionWindow %d", peak, bound)
	}
}
