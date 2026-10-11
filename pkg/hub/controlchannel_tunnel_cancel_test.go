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

package hub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	"github.com/gorilla/websocket"
)

// newHubWSPair creates a connected pair of wsprotocol.Connection for testing
// BrokerConnection in isolation, mirroring the "hub" end (returned first)
// and the simulated "broker" end (returned second) of a real control
// channel, without needing a full ControlChannelManager/broker process.
func newHubWSPair(t *testing.T) (hubSide, brokerSide *wsprotocol.Connection, cleanup func()) {
	t.Helper()
	ready := make(chan *wsprotocol.Connection, 1)
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		cfg := wsprotocol.ConnectionConfig{WriteWait: 5 * time.Second}
		ready <- wsprotocol.NewConnection(ws, cfg)
	}))

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	rawConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	cfg := wsprotocol.ConnectionConfig{WriteWait: 5 * time.Second}
	brokerSide = wsprotocol.NewConnection(rawConn, cfg)
	hubSide = <-ready

	return hubSide, brokerSide, func() {
		_ = hubSide.Close()
		_ = brokerSide.Close()
		srv.Close()
	}
}

// TestTunnelRequest_TimeoutSendsCancelToBroker covers ptone/scion#1886: when
// the Hub's own dispatch timeout elapses before the broker answers a
// tunneled request (e.g. a slow container/sandbox create), TunnelRequest
// must tell the broker to abort instead of silently giving up and letting
// the broker's create run to completion unwatched, which is exactly how the
// leak in the issue occurred.
func TestTunnelRequest_TimeoutSendsCancelToBroker(t *testing.T) {
	runOverSilentBrokers(t, testTunnelRequest_TimeoutSendsCancelToBroker)
}

func testTunnelRequest_TimeoutSendsCancelToBroker(t *testing.T, tr silentBrokerTransport) {
	b := tr.new(t, 100*time.Millisecond)

	req := &wsprotocol.RequestEnvelope{
		Type:      wsprotocol.TypeRequest,
		RequestID: "create-req-1",
		Method:    "POST",
		Path:      "/api/v1/agents",
	}

	// The broker receives the request, simulating a broker that is still
	// busy with a slow create and never responds.
	resp, err := b.tunnel().TunnelRequest(context.Background(), "broker-1", req)
	if err == nil {
		t.Fatalf("expected TunnelRequest to time out, got response: %+v", resp)
	}
	b.received(t)

	// The broker side must receive a cancel for the same RequestID.
	if got := b.cancelled(t); got != "create-req-1" {
		t.Errorf("expected cancel for requestID 'create-req-1', got %q", got)
	}
}

// TestTunnelRequest_CallerCtxCancelledSendsCancelToBroker covers the other
// half of ptone/scion#1886: if the original caller's context is cancelled
// (e.g. the inbound HTTP request was itself cancelled) before the broker
// responds, TunnelRequest must also notify the broker.
func TestTunnelRequest_CallerCtxCancelledSendsCancelToBroker(t *testing.T) {
	runOverSilentBrokers(t, testTunnelRequest_CallerCtxCancelledSendsCancelToBroker)
}

func testTunnelRequest_CallerCtxCancelledSendsCancelToBroker(t *testing.T, tr silentBrokerTransport) {
	b := tr.new(t, 30*time.Second)

	req := &wsprotocol.RequestEnvelope{
		Type:      wsprotocol.TypeRequest,
		RequestID: "create-req-2",
		Method:    "POST",
		Path:      "/api/v1/agents",
	}

	ctx, cancel := context.WithCancel(context.Background())
	resultCh := make(chan error, 1)
	go func() {
		_, err := b.tunnel().TunnelRequest(ctx, "broker-1", req)
		resultCh <- err
	}()

	b.received(t)

	// Give TunnelRequest a moment to reach its select before cancelling.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-resultCh:
		if err == nil {
			t.Fatal("expected TunnelRequest to return an error after ctx cancellation")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("TunnelRequest did not return after ctx cancellation")
	}

	if got := b.cancelled(t); got != "create-req-2" {
		t.Errorf("expected cancel for requestID 'create-req-2', got %q", got)
	}
}
