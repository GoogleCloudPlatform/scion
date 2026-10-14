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
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// TestRefusalsCarryMarker: the session layer's two "not handled here"
// refusals carry the refusal marker, and the other refusals do not.
func TestRefusalsCarryMarker(t *testing.T) {
	t.Run("no handler is 501 unsupported", func(t *testing.T) {
		p := newPair(t, Config{}, Config{})
		resp, err := p.dialer.Call(context.Background(), &conduitv1.RpcRequest{Path: "/x"})
		if err != nil || resp.GetStatus() != 501 {
			t.Fatalf("resp = %v, err = %v; want 501", resp, err)
		}
		if got := RefusalOf(resp); got != RefusalUnsupported {
			t.Fatalf("RefusalOf = %q, want %q (headers %v)", got, RefusalUnsupported, resp.GetHeaders())
		}
	})
	t.Run("draining is 503 draining", func(t *testing.T) {
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
		if r.GetRequestId() != "b" || RefusalOf(r) != RefusalDraining || r.GetHeaders()[RetryAfterHeader] != "0" {
			t.Fatalf("draining response = %v, want 503 marked %q with %s", r, RefusalDraining, RetryAfterHeader)
		}
		close(h.release)
		r = raw.recvType("rpc_response").GetRpcResponse()
		if r.GetRequestId() != "a" || r.GetStatus() != 200 || RefusalOf(r) != "" {
			t.Fatalf("in-flight response = %v, want a/200 without a marker", r)
		}
	})
	t.Run("duplicate 409 is unmarked", func(t *testing.T) {
		h := newBlockingRPC()
		_, raw := acceptAgainstRaw(t, Config{RPCHandler: h}, transport.MemoryOptions{Buffer: 64})
		f := &conduitv1.Frame{Body: &conduitv1.Frame_RpcRequest{RpcRequest: &conduitv1.RpcRequest{RequestId: "d", Path: "/x"}}}
		raw.send(f)
		h.waitEntered(t)
		raw.send(f)
		r := raw.recvType("rpc_response").GetRpcResponse()
		if r.GetStatus() != 409 {
			t.Fatalf("duplicate response = %v, want 409", r)
		}
		if _, ok := headerValueFold(r.GetHeaders(), RefusalHeader); ok {
			t.Fatalf("409 carries %s: %v", RefusalHeader, r.GetHeaders())
		}
		close(h.release)
	})
}

// TestHandlerReplyRefusalHeaderStripped: a handler that sets the refusal
// marker on its own reply, in any letter case, has it removed, so the
// caller sees the handler's own answer.
func TestHandlerReplyRefusalHeaderStripped(t *testing.T) {
	cases := []struct {
		status int32
		key    string
		value  string
	}{
		{503, RefusalHeader, RefusalDraining},
		{501, "x-conduit-refusal", RefusalUnsupported},
		{503, "X-CONDUIT-REFUSAL", RefusalDraining},
	}
	for _, tc := range cases {
		h := RPCHandlerFunc(func(context.Context, *conduitv1.RpcRequest) *conduitv1.RpcResponse {
			return &conduitv1.RpcResponse{Status: tc.status, Headers: map[string]string{tc.key: tc.value, "X-Other": "kept"}}
		})
		p := newPair(t, Config{}, Config{RPCHandler: h})
		resp, err := p.dialer.Call(context.Background(), &conduitv1.RpcRequest{Path: "/x"})
		if err != nil || resp.GetStatus() != tc.status {
			t.Fatalf("%s: resp = %v, err = %v; want %d", tc.key, resp, err, tc.status)
		}
		if _, ok := headerValueFold(resp.GetHeaders(), RefusalHeader); ok {
			t.Fatalf("%s: handler marker reached the caller: %v", tc.key, resp.GetHeaders())
		}
		if RefusalOf(resp) != "" {
			t.Fatalf("%s: handler reply classified as a session refusal", tc.key)
		}
		if resp.GetHeaders()["X-Other"] != "kept" {
			t.Fatalf("%s: other headers lost: %v", tc.key, resp.GetHeaders())
		}
	}
}

// TestRefusalOfClosedSet: only 501/unsupported and 503/draining are
// refusals; any other value or status counts as no marker.
func TestRefusalOfClosedSet(t *testing.T) {
	resp := func(status int32, v string) *conduitv1.RpcResponse {
		return &conduitv1.RpcResponse{Status: status, Headers: map[string]string{RefusalHeader: v}}
	}
	cases := []struct {
		name string
		r    *conduitv1.RpcResponse
		want string
	}{
		{"nil", nil, ""},
		{"no header", &conduitv1.RpcResponse{Status: 503}, ""},
		{"501 unsupported", resp(501, RefusalUnsupported), RefusalUnsupported},
		{"503 draining", resp(503, RefusalDraining), RefusalDraining},
		{"lower-case name", &conduitv1.RpcResponse{Status: 503, Headers: map[string]string{"x-conduit-refusal": RefusalDraining}}, RefusalDraining},
		{"503 unsupported", resp(503, RefusalUnsupported), ""},
		{"501 draining", resp(501, RefusalDraining), ""},
		{"unknown value", resp(503, "busy"), ""},
		{"value case differs", resp(503, "Draining"), ""},
		{"200 draining", resp(200, RefusalDraining), ""},
	}
	for _, tc := range cases {
		if got := RefusalOf(tc.r); got != tc.want {
			t.Errorf("%s: RefusalOf = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestCallNotWrittenIsNotSent: a request still queued when the session
// ends is reported as not sent, and is never written afterwards.
func TestCallNotWrittenIsNotSent(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	var written []string
	icpt := func(dir Direction, f *conduitv1.Frame) []*conduitv1.Frame {
		if dir == Outbound && f.GetRpcRequest() != nil {
			mu.Lock()
			written = append(written, f.GetRpcRequest().GetPath())
			mu.Unlock()
			if f.GetRpcRequest().GetPath() == "/a" {
				once.Do(func() { close(entered) })
				<-release // hold the writer on the first request
			}
		}
		return []*conduitv1.Frame{f}
	}
	p := newPair(t, Config{Interceptor: icpt}, Config{RPCHandler: echoRPC()})
	defer close(release)

	type result struct{ err error }
	callA := make(chan result, 1)
	go func() {
		_, err := p.dialer.Call(context.Background(), &conduitv1.RpcRequest{Path: "/a"})
		callA <- result{err}
	}()
	select {
	case <-entered:
	case <-time.After(waitTimeout):
		t.Fatal("first request not taken by the writer")
	}
	callB := make(chan result, 1)
	go func() {
		_, err := p.dialer.Call(context.Background(), &conduitv1.RpcRequest{Path: "/b"})
		callB <- result{err}
	}()
	eventually(t, "second request queued", func() bool {
		p.dialer.sched.mu.Lock()
		defer p.dialer.sched.mu.Unlock()
		return len(p.dialer.sched.control) > 0
	})
	p.dialer.fail(errors.New("conduit test: transport lost"))

	rb := <-callB
	if !errors.Is(rb.err, ErrRequestNotSent) || !errors.Is(rb.err, ErrSessionClosed) {
		t.Fatalf("queued call err = %v, want ErrRequestNotSent and ErrSessionClosed", rb.err)
	}
	ra := <-callA
	if !errors.Is(ra.err, ErrSessionClosed) || errors.Is(ra.err, ErrRequestNotSent) {
		t.Fatalf("taken call err = %v, want ErrSessionClosed without ErrRequestNotSent", ra.err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, path := range written {
		if path == "/b" {
			t.Fatal("abandoned request reached the writer")
		}
	}
}

// TestCallWrittenIsSessionClosed: a request the peer already received
// gets ErrSessionClosed alone when the session ends, because its outcome
// is unknown.
func TestCallWrittenIsSessionClosed(t *testing.T) {
	h := newBlockingRPC()
	p := newPair(t, Config{}, Config{RPCHandler: h})
	errc := make(chan error, 1)
	go func() {
		_, err := p.dialer.Call(context.Background(), &conduitv1.RpcRequest{Path: "/slow"})
		errc <- err
	}()
	h.waitEntered(t)
	p.dialer.fail(errors.New("conduit test: transport lost"))
	err := <-errc
	if !errors.Is(err, ErrSessionClosed) || errors.Is(err, ErrRequestNotSent) {
		t.Fatalf("Call err = %v, want ErrSessionClosed without ErrRequestNotSent", err)
	}
}

// TestCallOnEndedSessionIsNotSent: a Call on a session that has already
// ended is reported as not sent.
func TestCallOnEndedSessionIsNotSent(t *testing.T) {
	p := newPair(t, Config{}, Config{RPCHandler: echoRPC()})
	p.dialer.fail(errors.New("conduit test: transport lost"))
	_, err := p.dialer.Call(context.Background(), &conduitv1.RpcRequest{Path: "/x"})
	if !errors.Is(err, ErrRequestNotSent) || !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("Call err = %v, want ErrRequestNotSent and ErrSessionClosed", err)
	}
}
