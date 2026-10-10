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
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

func echoRPC() RPCHandler {
	return RPCHandlerFunc(func(_ context.Context, req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
		return &conduitv1.RpcResponse{Status: 200, Headers: map[string]string{"x-path": req.GetPath()}, Body: req.GetBody()}
	})
}

func TestRPCRoundTripBothDirections(t *testing.T) {
	p := newPair(t, Config{RPCHandler: echoRPC()}, Config{RPCHandler: echoRPC()})
	for name, s := range map[string]Session{"dialer→relay": p.dialer, "relay→dialer": p.relay} {
		resp, err := s.Call(context.Background(), &conduitv1.RpcRequest{Method: "POST", Path: "/v1/x", Body: []byte(name)})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if resp.GetStatus() != 200 || string(resp.GetBody()) != name || resp.GetHeaders()["x-path"] != "/v1/x" {
			t.Fatalf("%s: resp = %v", name, resp)
		}
	}
}

func TestRPCBodyLimit413(t *testing.T) {
	called := make(chan struct{}, 4)
	big := bytes.Repeat([]byte{'a'}, MaxRPCBody+1)
	h := RPCHandlerFunc(func(_ context.Context, req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
		called <- struct{}{}
		if req.GetPath() == "/big-response" {
			return &conduitv1.RpcResponse{Status: 200, Body: big}
		}
		return &conduitv1.RpcResponse{Status: 200}
	})
	p := newPair(t, Config{}, Config{RPCHandler: h})

	t.Run("request answered locally", func(t *testing.T) {
		before := p.dialer.Stats().ControlFramesSent
		resp, err := p.dialer.Call(context.Background(), &conduitv1.RpcRequest{Path: "/x", Body: big})
		if err != nil || resp.GetStatus() != 413 {
			t.Fatalf("resp = %v, err = %v; want 413", resp, err)
		}
		if p.dialer.Stats().ControlFramesSent != before {
			t.Fatal("oversized request was sent")
		}
	})
	t.Run("exactly at the limit passes", func(t *testing.T) {
		resp, err := p.dialer.Call(context.Background(), &conduitv1.RpcRequest{Path: "/x", Body: big[:MaxRPCBody]})
		if err != nil || resp.GetStatus() != 200 {
			t.Fatalf("resp = %v, err = %v; want 200", resp, err)
		}
		<-called
	})
	t.Run("oversized handler response", func(t *testing.T) {
		resp, err := p.dialer.Call(context.Background(), &conduitv1.RpcRequest{Path: "/big-response"})
		if err != nil || resp.GetStatus() != 413 {
			t.Fatalf("resp = %v, err = %v; want 413", resp, err)
		}
		<-called
	})
	t.Run("oversized inbound request", func(t *testing.T) {
		_, raw := acceptAgainstRaw(t, Config{RPCHandler: h}, transport.MemoryOptions{Buffer: 64})
		raw.send(&conduitv1.Frame{Body: &conduitv1.Frame_RpcRequest{RpcRequest: &conduitv1.RpcRequest{RequestId: "r1", Body: big}}})
		r := raw.recvType("rpc_response").GetRpcResponse()
		if r.GetRequestId() != "r1" || r.GetStatus() != 413 {
			t.Fatalf("got %v, want 413 for r1", r)
		}
	})
	select {
	case <-called:
		t.Fatal("handler saw an oversized request")
	default:
	}
}

func TestRPCWithoutHandler501(t *testing.T) {
	p := newPair(t, Config{}, Config{})
	resp, err := p.dialer.Call(context.Background(), &conduitv1.RpcRequest{Path: "/x"})
	if err != nil || resp.GetStatus() != 501 {
		t.Fatalf("resp = %v, err = %v; want 501", resp, err)
	}
}

func TestRPCNilResponse500AndDuplicate409(t *testing.T) {
	release := make(chan struct{})
	h := RPCHandlerFunc(func(_ context.Context, req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
		if req.GetPath() == "/block" {
			<-release
			return &conduitv1.RpcResponse{Status: 200}
		}
		return nil
	})
	_, raw := acceptAgainstRaw(t, Config{RPCHandler: h}, transport.MemoryOptions{Buffer: 64})
	raw.send(&conduitv1.Frame{Body: &conduitv1.Frame_RpcRequest{RpcRequest: &conduitv1.RpcRequest{RequestId: "n", Path: "/nil"}}})
	if r := raw.recvType("rpc_response").GetRpcResponse(); r.GetStatus() != 500 {
		t.Fatalf("nil response mapped to %v, want 500", r)
	}
	raw.send(&conduitv1.Frame{Body: &conduitv1.Frame_RpcRequest{RpcRequest: &conduitv1.RpcRequest{RequestId: "d", Path: "/block"}}})
	raw.send(&conduitv1.Frame{Body: &conduitv1.Frame_RpcRequest{RpcRequest: &conduitv1.RpcRequest{RequestId: "d", Path: "/block"}}})
	if r := raw.recvType("rpc_response").GetRpcResponse(); r.GetStatus() != 409 {
		t.Fatalf("duplicate id answered %v, want 409", r)
	}
	close(release)
	if r := raw.recvType("rpc_response").GetRpcResponse(); r.GetRequestId() != "d" || r.GetStatus() != 200 {
		t.Fatalf("got %v", r)
	}
}

// TestRPCCancelPropagates: cancelling Call sends RpcCancel and cancels the
// handler's ctx; no response is sent for a cancelled request.
func TestRPCCancelPropagates(t *testing.T) {
	entered := make(chan struct{})
	handlerCancelled := make(chan struct{})
	h := RPCHandlerFunc(func(ctx context.Context, _ *conduitv1.RpcRequest) *conduitv1.RpcResponse {
		close(entered)
		<-ctx.Done()
		close(handlerCancelled)
		return &conduitv1.RpcResponse{Status: 200}
	})
	p := newPair(t, Config{}, Config{RPCHandler: h})
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := p.dialer.Call(ctx, &conduitv1.RpcRequest{Path: "/slow"})
		errc <- err
	}()
	<-entered
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("Call err = %v", err)
	}
	select {
	case <-handlerCancelled:
	case <-time.After(waitTimeout):
		t.Fatal("handler ctx not cancelled by RpcCancel")
	}
	eventually(t, "inbound rpc released", func() bool {
		p.relay.mu.Lock()
		defer p.relay.mu.Unlock()
		return len(p.relay.inboundRPC) == 0
	})
}

func TestRPCSessionLossCancelsHandler(t *testing.T) {
	entered := make(chan struct{})
	handlerCancelled := make(chan struct{})
	h := RPCHandlerFunc(func(ctx context.Context, _ *conduitv1.RpcRequest) *conduitv1.RpcResponse {
		close(entered)
		<-ctx.Done()
		close(handlerCancelled)
		return nil
	})
	p := newPair(t, Config{}, Config{RPCHandler: h})
	errc := make(chan error, 1)
	go func() {
		_, err := p.dialer.Call(context.Background(), &conduitv1.RpcRequest{Path: "/slow"})
		errc <- err
	}()
	<-entered
	_ = p.dialer.Close()
	if err := <-errc; !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("Call err = %v, want ErrSessionClosed", err)
	}
	select {
	case <-handlerCancelled:
	case <-time.After(waitTimeout):
		t.Fatal("handler ctx not cancelled on session loss")
	}
}

func TestAuthRefresh(t *testing.T) {
	t.Run("accepted", func(t *testing.T) {
		p := newPair(t, Config{}, Config{})
		if err := p.dialer.RefreshAuth([]byte("new-credential"), 0); err != nil {
			t.Fatal(err)
		}
		eventually(t, "Refresh called", func() bool {
			p.adm.mu.Lock()
			defer p.adm.mu.Unlock()
			return len(p.adm.refreshes) == 1 && string(p.adm.refreshes[0].GetCredential()) == "new-credential"
		})
		if p.dialer.isDone() {
			t.Fatal("session ended after accepted refresh")
		}
	})
	t.Run("rejected ends the session with 4401", func(t *testing.T) {
		p := newPair(t, Config{}, Config{})
		p.adm.mu.Lock()
		p.adm.refreshFn = func(*conduitv1.AuthRefresh) error { return Reject(CloseUnauthenticated, "expired") }
		p.adm.mu.Unlock()
		if err := p.dialer.RefreshAuth([]byte("stale"), 0); err != nil {
			t.Fatal(err)
		}
		if err := waitDone(t, p.dialer); CodeOf(err, 0) != CloseUnauthenticated {
			t.Fatalf("dialer err = %v, want 4401", err)
		}
		if err := waitDone(t, p.relay); CodeOf(err, 0) != CloseUnauthenticated {
			t.Fatalf("relay err = %v, want 4401", err)
		}
	})
}

// TestAuthRefreshStreamIDFromDialerIsBadFrame: stream renewal
// (AuthRefresh{stream_id}) only travels relay → target. One sent by a
// dialer ends the session with 4400 bad_frame, is logged, and never
// reaches the Admitter.
func TestAuthRefreshStreamIDFromDialerIsBadFrame(t *testing.T) {
	var logBuf syncBuffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	p := newPair(t, Config{}, Config{Logger: logger})
	if err := p.dialer.RefreshAuth([]byte("credential"), 5); err != nil {
		t.Fatal(err)
	}
	err := waitDone(t, p.relay)
	var ce *CloseError
	if !errors.As(err, &ce) || ce.Code != CloseProtocolError || !strings.HasPrefix(ce.Reason, "bad_frame") {
		t.Fatalf("relay err = %v, want 4400 bad_frame", err)
	}
	if err := waitDone(t, p.dialer); CodeOf(err, 0) != CloseProtocolError {
		t.Fatalf("dialer err = %v, want 4400", err)
	}
	p.adm.mu.Lock()
	n := len(p.adm.refreshes)
	p.adm.mu.Unlock()
	if n != 0 {
		t.Fatalf("Admitter.Refresh called %d times for a dialer stream refresh", n)
	}
	logged := logBuf.String()
	for _, want := range []string{"AuthRefresh with a stream id", "stream_id=5", "principal_id=agent-1"} {
		if !strings.Contains(logged, want) {
			t.Errorf("log %q does not contain %q", logged, want)
		}
	}
}

// syncBuffer is a bytes.Buffer safe for concurrent writes and reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestAuthRefreshStreamIDFromRelayIsAccepted: the hub-originated renewal
// notice AuthRefresh{stream_id} (relay → target) is accepted on the
// target side. Neither the session nor the stream it names is closed, the
// stream carries data in both directions afterwards, and a notice naming
// a stream that already ended is ignored. (The dialer-sent case is
// TestAuthRefreshStreamIDFromDialerIsBadFrame.)
func TestAuthRefreshStreamIDFromRelayIsAccepted(t *testing.T) {
	in := make(chan Stream, 1)
	var mu sync.Mutex
	var notices []uint32
	p := newPair(t, Config{
		StreamHandler: acceptAll(in),
		Interceptor: func(dir Direction, f *conduitv1.Frame) []*conduitv1.Frame {
			if ar := f.GetAuthRefresh(); dir == Inbound && ar != nil {
				mu.Lock()
				notices = append(notices, ar.GetStreamId())
				mu.Unlock()
			}
			return []*conduitv1.Frame{f}
		},
	}, Config{})
	rs, err := p.relay.OpenStream(context.Background(), tcpOpen())
	if err != nil {
		t.Fatal(err)
	}
	ds := recvStream(t, in)

	if err := p.relay.RefreshAuth(nil, rs.ID()); err != nil {
		t.Fatal(err)
	}
	const ended = 1000 // an even (relay-owned) id with no stream
	if err := p.relay.RefreshAuth(nil, ended); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the target received both notices", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(notices) == 2
	})
	mu.Lock()
	got := append([]uint32(nil), notices...)
	mu.Unlock()
	if got[0] != rs.ID() || got[1] != ended {
		t.Fatalf("notices %v, want [%d %d]", got, rs.ID(), ended)
	}

	// The stream is still open both ways after the notices.
	go func() { _, _ = rs.Write([]byte("ping")) }()
	buf := make([]byte, 4)
	if _, err := io.ReadFull(ds, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("target read %q, %v", buf, err)
	}
	go func() { _, _ = ds.Write([]byte("pong")) }()
	if _, err := io.ReadFull(rs, buf); err != nil || string(buf) != "pong" {
		t.Fatalf("relay read %q, %v", buf, err)
	}
	if p.dialer.isDone() || p.relay.isDone() {
		t.Fatalf("a session ended after a relay-sent renewal notice: dialer %v, relay %v", p.dialer.Err(), p.relay.Err())
	}
	if st := ds.(*stream).State(); st != StateActive {
		t.Fatalf("target stream state %v, want active", st)
	}
	p.adm.mu.Lock()
	n := len(p.adm.refreshes)
	p.adm.mu.Unlock()
	if n != 0 {
		t.Fatalf("Admitter.Refresh called %d times for a relay-sent notice", n)
	}
}

// TestAuthRefreshStreamIDAfterStreamClosed: a hub renewal notice that
// reaches the target after the stream it names has closed is ignored;
// both sessions stay up and keep serving new streams.
func TestAuthRefreshStreamIDAfterStreamClosed(t *testing.T) {
	in := make(chan Stream, 2)
	var mu sync.Mutex
	var notices int
	p := newPair(t, Config{
		StreamHandler: acceptAll(in),
		Interceptor: func(dir Direction, f *conduitv1.Frame) []*conduitv1.Frame {
			if dir == Inbound && f.GetAuthRefresh() != nil {
				mu.Lock()
				notices++
				mu.Unlock()
			}
			return []*conduitv1.Frame{f}
		},
	}, Config{})
	rs, err := p.relay.OpenStream(context.Background(), tcpOpen())
	if err != nil {
		t.Fatal(err)
	}
	ds := recvStream(t, in)
	id := rs.ID()
	if err := rs.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(ds); err != nil {
		t.Fatalf("target read after close: %v", err)
	}
	_ = ds.Close()
	eventually(t, "stream removed on both sides", func() bool {
		return p.dialer.Stats().OpenStreams == 0 && p.relay.Stats().OpenStreams == 0
	})

	if err := p.relay.RefreshAuth(nil, id); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the target received the notice", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return notices == 1
	})
	// The sessions still serve a new stream.
	rs2, err := p.relay.OpenStream(context.Background(), tcpOpen())
	if err != nil {
		t.Fatalf("open after a late notice: %v", err)
	}
	ds2 := recvStream(t, in)
	go func() { _, _ = rs2.Write([]byte("ok")) }()
	buf := make([]byte, 2)
	if _, err := io.ReadFull(ds2, buf); err != nil || string(buf) != "ok" {
		t.Fatalf("target read %q, %v", buf, err)
	}
	if p.dialer.isDone() || p.relay.isDone() {
		t.Fatalf("a session ended after a late renewal notice: dialer %v, relay %v", p.dialer.Err(), p.relay.Err())
	}
}
