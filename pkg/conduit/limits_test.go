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
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// TestMaxConcurrentStreams: peer-opened streams beyond the limit are
// refused with 4400; a finished stream frees its slot (1a-r1-F5).
func TestMaxConcurrentStreams(t *testing.T) {
	// The handler never decides: streams stay opening, holding slots.
	hold := StreamHandlerFunc(func(context.Context, *conduitv1.StreamOpen, PendingStream) error { return nil })
	p := newPair(t, Config{}, Config{StreamHandler: hold, MaxConcurrentStreams: 2})
	var cancels []context.CancelFunc
	defer func() {
		for _, c := range cancels {
			c()
		}
	}()
	open := func() {
		ctx, cancel := context.WithCancel(context.Background())
		cancels = append(cancels, cancel)
		go func() { _, _ = p.dialer.OpenStream(ctx, tcpOpen()) }()
	}
	open()
	open()
	eventually(t, "two pending streams", func() bool { return p.relay.Stats().OpenStreams == 2 })
	if _, err := p.dialer.OpenStream(context.Background(), tcpOpen()); CodeOf(err, 0) != CloseProtocolError {
		t.Fatalf("third stream err = %v, want 4400", err)
	}
	cancels[0]() // 4499: the relay forgets the stream
	eventually(t, "slot freed", func() bool { return p.relay.Stats().OpenStreams == 1 })
	open()
	eventually(t, "slot reused", func() bool { return p.relay.Stats().OpenStreams == 2 })
}

// TestMaxConcurrentRPCs: inbound RPCs beyond the limit are answered 429.
func TestMaxConcurrentRPCs(t *testing.T) {
	h := newBlockingRPC()
	p := newPair(t, Config{}, Config{RPCHandler: h, MaxConcurrentRPCs: 1})
	first := goCall(p.dialer)
	h.waitEntered(t)
	resp, err := p.dialer.Call(context.Background(), &conduitv1.RpcRequest{})
	if err != nil || resp.GetStatus() != 429 {
		t.Fatalf("second call = %v, %v; want 429", resp, err)
	}
	close(h.release)
	if r := waitCall(t, first); r.err != nil || r.resp.GetStatus() != 200 {
		t.Fatalf("first call = %v, %v", r.resp, r.err)
	}
}

// TestAuthRefreshSerialised: refreshes are validated one at a time, and
// those arriving during a validation collapse to the latest.
func TestAuthRefreshSerialised(t *testing.T) {
	p := newPair(t, Config{}, Config{})
	release := make(chan struct{})
	entered := make(chan struct{}, 4)
	var inFlight, maxInFlight atomic.Int32
	p.adm.mu.Lock()
	p.adm.refreshFn = func(ar *conduitv1.AuthRefresh) error {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		if n > maxInFlight.Load() {
			maxInFlight.Store(n)
		}
		entered <- struct{}{}
		if string(ar.GetCredential()) == "1" {
			<-release
		}
		return nil
	}
	p.adm.mu.Unlock()
	for _, c := range []string{"1", "2", "3"} {
		if err := p.dialer.RefreshAuth([]byte(c), 0); err != nil {
			t.Fatal(err)
		}
	}
	<-entered
	eventually(t, "later refreshes queued", func() bool {
		p.relay.mu.Lock()
		defer p.relay.mu.Unlock()
		return p.relay.refreshNext != nil && string(p.relay.refreshNext.GetCredential()) == "3"
	})
	close(release)
	eventually(t, "refreshes validated", func() bool {
		p.relay.mu.Lock()
		defer p.relay.mu.Unlock()
		return !p.relay.refreshing
	})
	p.adm.mu.Lock()
	var got []string
	for _, ar := range p.adm.refreshes {
		got = append(got, string(ar.GetCredential()))
	}
	p.adm.mu.Unlock()
	if strings.Join(got, ",") != "1,3" {
		t.Fatalf("validated %v, want [1 3] (2 superseded)", got)
	}
	if maxInFlight.Load() != 1 {
		t.Fatalf("%d refreshes validated concurrently", maxInFlight.Load())
	}
}

// TestSessionCloseWithCode: CloseWithCode ends the session at once with
// the code on both sides and on every stream (1a-r1-F6).
func TestSessionCloseWithCode(t *testing.T) {
	accepted := make(chan Stream, 1)
	p := newPair(t, Config{}, Config{StreamHandler: acceptAll(accepted)})
	opener, err := p.dialer.OpenStream(context.Background(), tcpOpen())
	if err != nil {
		t.Fatal(err)
	}
	recvStream(t, accepted)
	var ls LocalSession = p.relay
	if err := ls.CloseWithCode(CloseForbidden, "revoked"); err != nil {
		t.Fatal(err)
	}
	if err := waitDone(t, p.relay); CodeOf(err, 0) != CloseForbidden {
		t.Fatalf("relay err = %v, want 4403", err)
	}
	err = waitDone(t, p.dialer)
	var ce *CloseError
	if !errors.As(err, &ce) || ce.Code != CloseForbidden || ce.Reason != "revoked" {
		t.Fatalf("dialer err = %v, want 4403 revoked", err)
	}
	if _, err := opener.Read(make([]byte, 1)); CodeOf(err, 0) != CloseForbidden {
		t.Fatalf("stream read err = %v, want 4403", err)
	}
	if err := ls.CloseWithCode(CloseForbidden, ""); err != ErrSessionClosed {
		t.Fatalf("second CloseWithCode = %v, want ErrSessionClosed", err)
	}
}

// TestStreamResizeDelivered: inbound StreamResize reaches the stream's
// Resizes channel, coalesced to the latest size, and the channel closes
// with the stream (1a-r1-F7).
func TestStreamResizeDelivered(t *testing.T) {
	accepted := make(chan Stream, 1)
	p := newPair(t, Config{}, Config{StreamHandler: acceptAll(accepted)})
	opener, err := p.dialer.OpenStream(context.Background(), &conduitv1.StreamOpen{Kind: conduitv1.StreamKind_STREAM_KIND_PTY})
	if err != nil {
		t.Fatal(err)
	}
	target := recvStream(t, accepted).(Resizable)
	if err := opener.Resize(80, 24); err != nil {
		t.Fatal(err)
	}
	if err := opener.Resize(120, 40); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(waitTimeout)
	for got := (WindowSize{}); got != (WindowSize{Cols: 120, Rows: 40}); {
		select {
		case got = <-target.Resizes():
		case <-deadline:
			t.Fatalf("latest size never delivered (last %v)", got)
		}
	}

	// Coalescing and clamping, without a consumer.
	st := target.(*stream)
	st.deliverResize(1, 2)
	st.deliverResize(70000, 5)
	if got := <-target.Resizes(); got != (WindowSize{Cols: 0xFFFF, Rows: 5}) {
		t.Fatalf("coalesced size %v", got)
	}

	_ = opener.Close()
	select {
	case _, ok := <-target.Resizes():
		if ok {
			t.Fatal("unexpected size after close")
		}
	case <-time.After(waitTimeout):
		t.Fatal("Resizes not closed when the stream ended")
	}
}

// TestRPCFrameLimit413: an RPC whose encoded frame exceeds MaxRPCFrame
// (here through its headers, with a legal body) is answered 413 locally,
// on the request and the response side, and the session survives
// (1a-r1-F8).
func TestRPCFrameLimit413(t *testing.T) {
	var calls atomic.Int32
	bigHeaders := map[string]string{"x-big": strings.Repeat("h", 300*1024)}
	h := RPCHandlerFunc(func(_ context.Context, req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
		calls.Add(1)
		if req.GetPath() == "/big-response" {
			return &conduitv1.RpcResponse{Status: 200, Headers: bigHeaders, Body: bytes.Repeat([]byte{'b'}, MaxRPCBody)}
		}
		return &conduitv1.RpcResponse{Status: 200}
	})
	p := newPair(t, Config{}, Config{RPCHandler: h})

	resp, err := p.dialer.Call(context.Background(), &conduitv1.RpcRequest{Path: "/x", Headers: bigHeaders, Body: bytes.Repeat([]byte{'a'}, MaxRPCBody)})
	if err != nil || resp.GetStatus() != 413 {
		t.Fatalf("oversized request = %v, %v; want 413", resp, err)
	}
	if calls.Load() != 0 {
		t.Fatal("oversized request reached the handler")
	}
	resp, err = p.dialer.Call(context.Background(), &conduitv1.RpcRequest{Path: "/big-response"})
	if err != nil || resp.GetStatus() != 413 {
		t.Fatalf("oversized response = %v, %v; want 413", resp, err)
	}
	resp, err = p.dialer.Call(context.Background(), &conduitv1.RpcRequest{Path: "/x"})
	if err != nil || resp.GetStatus() != 200 {
		t.Fatalf("session did not survive: %v, %v", resp, err)
	}
}

// blockingCloseConn is a transport whose Close blocks until released, as
// a ws Close does while a writer is stuck.
type blockingCloseConn struct {
	transport.Conn
	release chan struct{}
	once    sync.Once
}

func (c *blockingCloseConn) Close() error {
	<-c.release
	var err error
	c.once.Do(func() { err = c.Conn.Close() })
	return err
}

// TestFailDoesNotWaitForTransportClose: streams fail as soon as the
// session ends, even if closing the transport blocks (1a-r1-F10).
func TestFailDoesNotWaitForTransportClose(t *testing.T) {
	a, b := transport.Pipe(transport.MemoryOptions{Buffer: 64})
	conn := &blockingCloseConn{Conn: a, release: make(chan struct{})}
	defer close(conn.release)
	raw := &rawPeer{t: t, conn: b}
	go func() {
		raw.recv() // hello
		raw.send(&conduitv1.Frame{Body: &conduitv1.Frame_Welcome{Welcome: &conduitv1.Welcome{SessionId: "raw"}}})
	}()
	s, _, err := Dial(context.Background(), transport.DialerFunc(func(context.Context) (transport.Conn, error) { return conn, nil }), Config{}, testHello())
	if err != nil {
		t.Fatal(err)
	}
	res := make(chan Stream, 1)
	go func() {
		st, err := s.OpenStream(context.Background(), tcpOpen())
		if err != nil {
			t.Error(err)
		}
		res <- st
	}()
	o := raw.recvType("stream_open").GetStreamOpen()
	raw.send(&conduitv1.Frame{Body: &conduitv1.Frame_StreamAccept{StreamAccept: &conduitv1.StreamAccept{StreamId: o.GetStreamId(), InitialWindow: DefaultStreamWindow}}})
	st := <-res

	go func() { _ = s.Close() }()
	readErr := make(chan error, 1)
	go func() {
		_, err := st.Read(make([]byte, 1))
		readErr <- err
	}()
	select {
	case err := <-readErr:
		if !errors.Is(err, ErrSessionClosed) {
			t.Fatalf("read err = %v, want ErrSessionClosed", err)
		}
	case <-time.After(waitTimeout):
		t.Fatal("stream read blocked behind the transport close")
	}
}

// TestAbortReplacesPurgedGracefulClose: a graceful StreamClose still
// queued behind data when the stream is aborted is replaced by an
// abortive one, so the peer always learns the stream ended (1a-r1-F11).
func TestAbortReplacesPurgedGracefulClose(t *testing.T) {
	s, raw := dialAgainstRaw(t, Config{}, transport.MemoryOptions{Buffer: 0})
	res := make(chan Stream, 1)
	go func() {
		st, err := s.OpenStream(context.Background(), tcpOpen())
		if err != nil {
			t.Error(err)
		}
		res <- st
	}()
	o := raw.recvType("stream_open").GetStreamOpen()
	id := o.GetStreamId()
	raw.send(&conduitv1.Frame{Body: &conduitv1.Frame_StreamAccept{StreamAccept: &conduitv1.StreamAccept{StreamId: id, InitialWindow: 3 * MaxDataFrame}}})
	st := <-res
	// The writer blocks on the first data frame (the raw peer is not
	// reading); the rest and the graceful close stay queued.
	if _, err := st.Write(make([]byte, 3*MaxDataFrame)); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	eventually(t, "close queued", func() bool { return s.Stats().QueuedDataBytes > 2*MaxDataFrame })

	// The peer overruns the stream window: the stream is aborted with 4400.
	for i := 0; i <= DefaultStreamWindow/MaxDataFrame; i++ {
		raw.send(&conduitv1.Frame{Body: &conduitv1.Frame_StreamData{StreamData: &conduitv1.StreamData{StreamId: id, Data: make([]byte, MaxDataFrame)}}})
	}
	eventually(t, "stream removed", func() bool { return s.Stats().OpenStreams == 0 })
	for {
		f := raw.recv()
		switch FrameType(f) {
		case "stream_data", "stream_window", "ping", "pong":
			continue
		case "stream_close":
			if c := f.GetStreamClose(); c.GetStreamId() != id || c.GetCode() != CloseProtocolError {
				t.Fatalf("stream close = %v, want %d/4400", c, id)
			}
			return
		default:
			t.Fatalf("unexpected %s", FrameType(f))
		}
	}
}
