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

package conduit_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport/ws"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

type admitAll struct{}

func (admitAll) Admit(_ context.Context, h *conduitv1.Hello) (*conduitv1.Welcome, error) {
	return &conduitv1.Welcome{SessionId: "s-" + h.GetPrincipalId(), RelayInstanceId: "relay-1", ConnectionEpoch: 1}, nil
}
func (admitAll) Refresh(context.Context, *conduitv1.AuthRefresh) error { return nil }

// TestSessionOverWS runs a full session over the ws transport, using only
// the exported API (as the relay and target packages will): handshake,
// RPC, a stream round trip, and the frame interceptor seam.
func TestSessionOverWS(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]int{}
	count := func(d conduit.Direction, f *conduitv1.Frame) []*conduitv1.Frame {
		mu.Lock()
		seen[conduit.FrameType(f)]++
		mu.Unlock()
		return []*conduitv1.Frame{f}
	}
	relayCfg := conduit.Config{
		Interceptor: count,
		RPCHandler: conduit.RPCHandlerFunc(func(_ context.Context, r *conduitv1.RpcRequest) *conduitv1.RpcResponse {
			return &conduitv1.RpcResponse{Status: 200, Body: []byte("pong:" + r.GetPath())}
		}),
	}
	relaySessions := make(chan conduit.Session, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := ws.Upgrade(w, r, nil, ws.Options{})
		if err != nil {
			return
		}
		s, err := conduit.Accept(r.Context(), c, relayCfg, admitAll{})
		if err != nil {
			return
		}
		relaySessions <- s
		<-s.(conduit.LocalSession).Done()
	}))
	defer srv.Close()

	accepted := make(chan conduit.Stream, 1)
	dialCfg := conduit.Config{StreamHandler: conduit.StreamHandlerFunc(func(_ context.Context, _ *conduitv1.StreamOpen, ps conduit.PendingStream) error {
		st, err := ps.Accept()
		if err == nil {
			accepted <- st
		}
		return nil
	})}
	hello := &conduitv1.Hello{
		PrincipalKind: conduitv1.PrincipalKind_PRINCIPAL_KIND_BROKER,
		PrincipalId:   "b1",
		Capabilities:  &conduitv1.Capabilities{StreamKinds: []string{"tcp"}, ExecScope: "scope"},
	}
	d := &ws.Dialer{URL: "ws" + strings.TrimPrefix(srv.URL, "http")}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, welcome, err := conduit.Dial(ctx, d, dialCfg, hello)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	info := s.Info()
	if welcome.GetSessionId() != "s-b1" || info.Transport != "ws" || info.PrincipalKind != "broker" || info.ExecScope != "scope" {
		t.Fatalf("welcome %v, info %+v", welcome, info)
	}

	resp, err := s.Call(ctx, &conduitv1.RpcRequest{Method: "GET", Path: "/x"})
	if err != nil || string(resp.GetBody()) != "pong:/x" {
		t.Fatalf("Call: %v %v", resp, err)
	}

	// The relay opens a stream to the dialer (relay → target direction).
	relay := <-relaySessions
	rs, err := relay.OpenStream(ctx, &conduitv1.StreamOpen{Kind: conduitv1.StreamKind_STREAM_KIND_TCP})
	if err != nil {
		t.Fatal(err)
	}
	if rs.ID()%2 != 0 {
		t.Fatalf("relay stream id %d is odd", rs.ID())
	}
	ts := <-accepted
	payload := strings.Repeat("z", 300*1024) // > one stream window
	go func() {
		_, _ = io.WriteString(rs, payload)
		_ = rs.Close()
	}()
	got, err := io.ReadAll(ts)
	if err != nil || string(got) != payload {
		t.Fatalf("read %d bytes, err %v", len(got), err)
	}
	mu.Lock()
	defer mu.Unlock()
	if seen["stream_data"] == 0 || seen["rpc_request"] != 1 || seen["stream_window"] == 0 {
		t.Fatalf("interceptor saw %v", seen)
	}
}
