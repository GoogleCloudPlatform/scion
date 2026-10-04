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
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// blockingRPC is an RPC handler that signals entry and answers 200 once
// released, or reports its cancellation.
type blockingRPC struct {
	entered   chan struct{}
	release   chan struct{}
	cancelled chan struct{}
}

func newBlockingRPC() *blockingRPC {
	return &blockingRPC{entered: make(chan struct{}, 16), release: make(chan struct{}), cancelled: make(chan struct{}, 16)}
}

func (b *blockingRPC) HandleRPC(ctx context.Context, _ *conduitv1.RpcRequest) *conduitv1.RpcResponse {
	b.entered <- struct{}{}
	select {
	case <-b.release:
		return &conduitv1.RpcResponse{Status: 200, Body: []byte("done")}
	case <-ctx.Done():
		b.cancelled <- struct{}{}
		return nil
	}
}

func (b *blockingRPC) waitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-b.entered:
	case <-time.After(waitTimeout):
		t.Fatal("rpc handler not entered")
	}
}

type callResult struct {
	resp *conduitv1.RpcResponse
	err  error
}

func goCall(s Session) <-chan callResult {
	ch := make(chan callResult, 1)
	go func() {
		resp, err := s.Call(context.Background(), &conduitv1.RpcRequest{Method: "POST", Path: "/v1/op"})
		ch <- callResult{resp, err}
	}()
	return ch
}

func waitCall(t *testing.T, ch <-chan callResult) callResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(waitTimeout):
		t.Fatal("call did not return")
		return callResult{}
	}
}

// TestGoAwayWaitsForInFlightRPCs: a planned drain on a session with no
// streams waits for the RPCs in flight, in either direction, and closes
// with 4503 once they completed (1a-r1-F1).
func TestGoAwayWaitsForInFlightRPCs(t *testing.T) {
	for _, dir := range []string{"handler on draining side", "caller on draining side"} {
		t.Run(dir, func(t *testing.T) {
			h := newBlockingRPC()
			var p *pair
			var caller Session
			if dir == "handler on draining side" {
				p = newPair(t, Config{}, Config{RPCHandler: h})
				caller = p.dialer
			} else {
				p = newPair(t, Config{RPCHandler: h}, Config{})
				caller = p.relay
			}
			res := goCall(caller)
			h.waitEntered(t)

			if err := p.relay.GoAway(GoAwayOptions{}); err != nil {
				t.Fatal(err)
			}
			<-p.dialer.GoAwayReceived()
			if p.relay.isDone() || p.dialer.isDone() {
				t.Fatalf("session closed with an RPC in flight: relay %v, dialer %v", p.relay.Err(), p.dialer.Err())
			}
			// New calls on a draining session go to the replacement.
			for name, s := range map[string]*session{"dialer": p.dialer, "relay": p.relay} {
				if _, err := s.Call(context.Background(), &conduitv1.RpcRequest{}); !errors.Is(err, ErrDraining) {
					t.Fatalf("%s Call while draining: %v, want ErrDraining", name, err)
				}
			}

			close(h.release)
			r := waitCall(t, res)
			if r.err != nil || r.resp.GetStatus() != 200 || string(r.resp.GetBody()) != "done" {
				t.Fatalf("in-flight call = %v, %v; want 200 done", r.resp, r.err)
			}
			if err := waitDone(t, p.relay); CodeOf(err, 0) != CloseRelayRestart {
				t.Fatalf("relay err = %v, want 4503", err)
			}
			if err := waitDone(t, p.dialer); CodeOf(err, 0) != CloseRelayRestart {
				t.Fatalf("dialer err = %v, want 4503", err)
			}
		})
	}
}

// TestGoAwayDeadlineCancelsRPCs: an RPC still running at the drain
// deadline is cancelled and its caller fails with 4503.
func TestGoAwayDeadlineCancelsRPCs(t *testing.T) {
	h := newBlockingRPC()
	p := newPair(t, Config{}, Config{RPCHandler: h})
	res := goCall(p.dialer)
	h.waitEntered(t)
	if err := p.relay.GoAway(GoAwayOptions{DrainDeadline: 5 * time.Second}); err != nil {
		t.Fatal(err)
	}
	<-p.dialer.GoAwayReceived()
	settle(t, p.clk, 6) // ping, watchdog, drain timer (or backstop) per side
	p.clk.Advance(5*time.Second - time.Millisecond)
	settle(t, p.clk, 6)
	if p.relay.isDone() {
		t.Fatalf("relay ended before the deadline: %v", p.relay.Err())
	}
	p.clk.Advance(time.Millisecond)
	select {
	case <-h.cancelled:
	case <-time.After(waitTimeout):
		t.Fatal("handler not cancelled at the drain deadline")
	}
	r := waitCall(t, res)
	if !errors.Is(r.err, ErrSessionClosed) || CodeOf(r.err, 0) != CloseRelayRestart {
		t.Fatalf("call err = %v, want ErrSessionClosed with 4503", r.err)
	}
	if err := waitDone(t, p.relay); CodeOf(err, 0) != CloseRelayRestart {
		t.Fatalf("relay err = %v, want 4503", err)
	}
}

// TestRPCOnDrainingSessionRefused503: a request that reaches a draining
// session is answered 503 with Retry-After, while the RPC already in
// flight completes before the session closes.
func TestRPCOnDrainingSessionRefused503(t *testing.T) {
	h := newBlockingRPC()
	relay, raw := acceptAgainstRaw(t, Config{RPCHandler: h}, transport.MemoryOptions{Buffer: 64})
	req := func(id string) *conduitv1.Frame {
		return &conduitv1.Frame{Body: &conduitv1.Frame_RpcRequest{RpcRequest: &conduitv1.RpcRequest{RequestId: id, Method: "GET", Path: "/x"}}}
	}
	raw.send(req("a"))
	h.waitEntered(t)
	if err := relay.GoAway(GoAwayOptions{}); err != nil {
		t.Fatal(err)
	}
	raw.recvType("go_away")
	raw.send(req("b"))
	r := raw.recvType("rpc_response").GetRpcResponse()
	if r.GetRequestId() != "b" || r.GetStatus() != 503 || r.GetHeaders()[RetryAfterHeader] != "0" {
		t.Fatalf("draining response = %v, want 503 with %s", r, RetryAfterHeader)
	}
	if relay.isDone() {
		t.Fatalf("relay closed with an RPC in flight: %v", relay.Err())
	}
	close(h.release)
	r = raw.recvType("rpc_response").GetRpcResponse()
	if r.GetRequestId() != "a" || r.GetStatus() != 200 {
		t.Fatalf("in-flight response = %v, want a/200", r)
	}
	if err := waitDone(t, relay); CodeOf(err, 0) != CloseRelayRestart {
		t.Fatalf("relay err = %v, want 4503", err)
	}
}

// TestGoAwayRacingInboundOpen: a GoAway racing a StreamOpen either
// refuses the stream (last_stream_id below it) or keeps the session open
// for it; it never advertises the stream and then closes (1a-r1-F9).
func TestGoAwayRacingInboundOpen(t *testing.T) {
	for i := 0; i < 200; i++ {
		accepted := make(chan Stream, 1)
		relay, raw := acceptAgainstRaw(t, Config{StreamHandler: acceptAll(accepted)}, transport.MemoryOptions{Buffer: 64})
		go func() { _ = relay.GoAway(GoAwayOptions{DrainDeadline: time.Hour}) }()
		// The write fails if the GoAway won and the drained session
		// already closed; the GoAway then refused the stream.
		if b, err := proto.Marshal(rawOpen(1)); err != nil {
			t.Fatal(err)
		} else {
			_ = raw.conn.WriteFrame(b)
		}

		// Read until both the GoAway and the stream's fate are known. A
		// closed connection is a valid fate only for a stream the GoAway
		// refused (last_stream_id 0): the drained session closed at once.
		var ga *conduitv1.GoAway
		var verdict *conduitv1.Frame
		closed := false
		for !closed && (ga == nil || verdict == nil) {
			b, err := raw.conn.ReadFrame()
			if err != nil {
				closed = true
				break
			}
			f := &conduitv1.Frame{}
			if err := proto.Unmarshal(b, f); err != nil {
				t.Fatal(err)
			}
			switch FrameType(f) {
			case "go_away":
				ga = f.GetGoAway()
			case "stream_accept", "stream_close":
				verdict = f
			}
		}
		switch {
		case ga == nil:
			t.Fatalf("iteration %d: connection closed before GoAway", i)
		case ga.GetLastStreamId() >= 1:
			if FrameType(verdict) != "stream_accept" {
				t.Fatalf("iteration %d: GoAway covers stream 1 but it got %v (closed %v)", i, verdict, closed)
			}
			if relay.isDone() {
				t.Fatalf("iteration %d: session closed under an accepted stream: %v", i, relay.Err())
			}
		case verdict != nil && verdict.GetStreamClose().GetCode() != CloseRelayRestart:
			t.Fatalf("iteration %d: GoAway refuses stream 1 but it got %v", i, verdict)
		}
		_ = relay.Close()
	}
}
