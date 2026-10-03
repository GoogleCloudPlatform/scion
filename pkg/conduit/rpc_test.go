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
