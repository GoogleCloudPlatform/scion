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
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// invalidUTF8 cannot be encoded in a proto3 string field, so a frame
// carrying it fails proto.Marshal.
const invalidUTF8 = "\xff"

// badHeaderName and badHeaderValue make up the unencodable response
// header; neither may appear in the session log.
const (
	badHeaderName  = "x-bad"
	badHeaderValue = "hdrmarker" + invalidUTF8
)

func requireSessionOpen(t *testing.T, name string, s LocalSession) {
	t.Helper()
	select {
	case <-s.Done():
		t.Fatalf("%s session ended: %v", name, s.Err())
	default:
	}
}

// TestRPCResponseEncodeFailureFailsOnlyThatRPC: a handler response that
// cannot be encoded is answered with a 500 for that request id; the
// session stays open and a concurrent RPC on it completes.
func TestRPCResponseEncodeFailureFailsOnlyThatRPC(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	h := RPCHandlerFunc(func(_ context.Context, req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
		switch req.GetPath() {
		case "/bad":
			return &conduitv1.RpcResponse{Status: 200, Headers: map[string]string{badHeaderName: badHeaderValue}}
		case "/slow":
			close(entered)
			<-release
		}
		return &conduitv1.RpcResponse{Status: 200, Body: []byte(req.GetPath())}
	})
	var logBuf syncBuffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	p := newPair(t, Config{}, Config{RPCHandler: h, Logger: logger})

	type result struct {
		resp *conduitv1.RpcResponse
		err  error
	}
	slow := make(chan result, 1)
	go func() {
		resp, err := p.dialer.Call(context.Background(), &conduitv1.RpcRequest{Path: "/slow"})
		slow <- result{resp, err}
	}()
	select {
	case <-entered:
	case <-time.After(waitTimeout):
		t.Fatal("slow handler not entered")
	}

	resp, err := p.dialer.Call(context.Background(), &conduitv1.RpcRequest{RequestId: "bad-1", Path: "/bad"})
	if err != nil {
		t.Fatalf("bad call: %v", err)
	}
	if resp.GetStatus() != 500 || resp.GetRequestId() != "bad-1" || string(resp.GetBody()) != rpcResponseEncodeFailedBody {
		t.Fatalf("bad call resp = %v, want 500 for bad-1", resp)
	}
	requireSessionOpen(t, "dialer", p.dialer)
	requireSessionOpen(t, "relay", p.relay)

	close(release)
	select {
	case r := <-slow:
		if r.err != nil || r.resp.GetStatus() != 200 || string(r.resp.GetBody()) != "/slow" {
			t.Fatalf("concurrent call: resp = %v, err = %v", r.resp, r.err)
		}
	case <-time.After(waitTimeout):
		t.Fatal("concurrent call did not complete")
	}
	requireSessionOpen(t, "dialer", p.dialer)
	requireSessionOpen(t, "relay", p.relay)

	logged := logBuf.String()
	for _, want := range []string{"failed to encode", "request_id=bad-1"} {
		if !strings.Contains(logged, want) {
			t.Errorf("log %q does not contain %q", logged, want)
		}
	}
	// The log names the RPC only: no header name, header value or invalid
	// byte (raw, in backslash form or replaced) from the unencodable response.
	for _, s := range []string{badHeaderName, "hdrmarker", invalidUTF8, `\xff`, "\ufffd"} {
		if strings.Contains(logged, s) {
			t.Errorf("log %q contains response content %q", logged, s)
		}
	}
}

// TestRPCRequestEncodeFailureFailsOnlyThatCall: a request that cannot be
// encoded fails the Call with ErrRPCEncode, no frame is sent, and the
// session stays open for later RPCs.
func TestRPCRequestEncodeFailureFailsOnlyThatCall(t *testing.T) {
	var rpcRequestsSent atomic.Int64
	count := func(dir Direction, f *conduitv1.Frame) []*conduitv1.Frame {
		if dir == Outbound && f.GetRpcRequest() != nil {
			rpcRequestsSent.Add(1)
		}
		return []*conduitv1.Frame{f}
	}
	p := newPair(t, Config{Interceptor: count}, Config{RPCHandler: echoRPC()})

	before := p.dialer.Stats().ControlFramesSent
	resp, err := p.dialer.Call(context.Background(), &conduitv1.RpcRequest{Path: "/x", Headers: map[string]string{"x-bad": invalidUTF8}})
	if !errors.Is(err, ErrRPCEncode) || resp != nil {
		t.Fatalf("resp = %v, err = %v; want ErrRPCEncode", resp, err)
	}
	if n := rpcRequestsSent.Load(); n != 0 {
		t.Fatalf("%d rpc request frames sent for an unencodable request", n)
	}
	if got := p.dialer.Stats().ControlFramesSent; got != before {
		t.Fatalf("control frames sent %d -> %d for an unencodable request", before, got)
	}
	requireSessionOpen(t, "dialer", p.dialer)
	requireSessionOpen(t, "relay", p.relay)
	p.dialer.mu.Lock()
	pending := len(p.dialer.pendingRPC)
	p.dialer.mu.Unlock()
	if pending != 0 {
		t.Fatalf("%d pending rpcs left after an encode failure", pending)
	}

	resp, err = p.dialer.Call(context.Background(), &conduitv1.RpcRequest{Path: "/later", Body: []byte("ok")})
	if err != nil || resp.GetStatus() != 200 || string(resp.GetBody()) != "ok" {
		t.Fatalf("later call: resp = %v, err = %v", resp, err)
	}
	if n := rpcRequestsSent.Load(); n != 1 {
		t.Fatalf("rpc request frames sent = %d, want 1", n)
	}
	requireSessionOpen(t, "dialer", p.dialer)
}

// TestRPCTransportWriteFailureEndsSession: a frame that encodes but that
// the transport refuses to write is a transport failure and still ends
// the session (unchanged behaviour).
func TestRPCTransportWriteFailureEndsSession(t *testing.T) {
	s, _ := dialAgainstRaw(t, Config{}, transport.MemoryOptions{Buffer: 64, MaxFrame: 1024})

	resp, err := s.Call(context.Background(), &conduitv1.RpcRequest{Path: "/x", Body: bytes.Repeat([]byte{'a'}, 2048)})
	if err == nil || errors.Is(err, ErrRPCEncode) {
		t.Fatalf("resp = %v, err = %v; want a session-ending transport error", resp, err)
	}
	end := waitDone(t, s)
	if !errors.Is(end, transport.ErrFrameTooLarge) || !strings.Contains(end.Error(), "transport write") {
		t.Fatalf("session err = %v, want transport write failure", end)
	}
	if !errors.Is(err, transport.ErrFrameTooLarge) {
		t.Fatalf("call err = %v, want it to carry the transport write failure", err)
	}
}
