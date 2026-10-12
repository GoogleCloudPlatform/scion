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

package runtimebroker

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/envelope"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// The request/response tests of the broker's tunnelled requests run over
// both connections the Hub can use: the control channel and the broker's
// conduit session (conduitRPCHandler). A tunnelHarness plays the Hub on
// one of them.

// tunnelHarness drives tunnelled requests into the broker as the Hub would.
type tunnelHarness interface {
	// send delivers req to the broker.
	send(t *testing.T, req wsprotocol.RequestEnvelope)
	// cancel sends the Hub's cancel for requestID.
	cancel(t *testing.T, requestID string)
	// response returns the response the Hub receives for requestID.
	response(t *testing.T, requestID string) wsprotocol.ResponseEnvelope
	// noResponse checks that the Hub receives no response for requestID.
	noResponse(t *testing.T, requestID string)
	// awaitQueued returns once n sent requests reached the broker's
	// dispatch queue.
	awaitQueued(t *testing.T, n int)
	// occupy takes one dispatch slot; release returns it.
	occupy()
	release()
	// shutdown ends the broker side of the connection.
	shutdown()
	// wait returns once every dispatched request has finished.
	wait(t *testing.T)
}

type tunnelTransport struct {
	name string
	new  func(t *testing.T, handler http.Handler, slots int) tunnelHarness
}

var tunnelTransports = []tunnelTransport{
	{name: "control-channel", new: newControlChannelHarness},
	{name: "conduit", new: newConduitHarness},
}

// ---------------------------------------------------------------- control channel

type controlChannelHarness struct {
	client  *ControlChannelClient
	hubConn *wsprotocol.Connection
}

func newControlChannelHarness(t *testing.T, handler http.Handler, slots int) tunnelHarness {
	client, hubConn := newCancelTestClient(t, handler, slots)
	return &controlChannelHarness{client: client, hubConn: hubConn}
}

func (h *controlChannelHarness) send(t *testing.T, req wsprotocol.RequestEnvelope) {
	req.Type = wsprotocol.TypeRequest
	feed(t, h.client, req)
}

func (h *controlChannelHarness) cancel(t *testing.T, requestID string) {
	feed(t, h.client, wsprotocol.NewCancelMessage(requestID))
}

func (h *controlChannelHarness) response(t *testing.T, requestID string) wsprotocol.ResponseEnvelope {
	t.Helper()
	_ = h.hubConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var resp wsprotocol.ResponseEnvelope
	if err := h.hubConn.ReadJSON(&resp); err != nil {
		t.Fatalf("read response for %s: %v", requestID, err)
	}
	return resp
}

func (h *controlChannelHarness) noResponse(t *testing.T, requestID string) {
	t.Helper()
	_ = h.hubConn.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	var resp wsprotocol.ResponseEnvelope
	if err := h.hubConn.ReadJSON(&resp); err == nil {
		t.Errorf("unexpected response for %s: %+v", requestID, resp)
	}
}

// awaitQueued: the control channel registers a request on the read loop
// but has no hook for its dispatch goroutine reaching the slot wait, so it
// keeps the settle delay these tests have always used.
func (h *controlChannelHarness) awaitQueued(*testing.T, int) { time.Sleep(100 * time.Millisecond) }

func (h *controlChannelHarness) occupy()  { h.client.dispatchSem <- struct{}{} }
func (h *controlChannelHarness) release() { <-h.client.dispatchSem }
func (h *controlChannelHarness) shutdown() {
	h.client.cancel()
}
func (h *controlChannelHarness) wait(t *testing.T) { waitWG(t, h.client) }

// ---------------------------------------------------------------- conduit

// conduitHarness is the broker's conduit session, kept by the broker's
// conduit dialer with a conduitRPCHandler as its RPC handler, and the hub's
// end of it.
type conduitHarness struct {
	hub     conduit.Session
	pair    *brokerSessionPair
	handler *conduitRPCHandler

	queued chan string

	// responses counts, per request id, the RpcResponse frames the
	// broker's session sent.
	respMu    sync.Mutex
	responses map[string]int

	mu      sync.Mutex
	calls   map[string]*conduitCall
	callsWG sync.WaitGroup
}

type conduitCall struct {
	cancel context.CancelFunc
	done   chan struct{}
	resp   *conduitv1.RpcResponse
	err    error
}

// brokerSessionPair is a broker's conduit session as the broker's
// conduit dialer (ConduitDialer) keeps it, dialed over WebSocket to a
// fake hub, and the hub's end of that session.
type brokerSessionPair struct {
	// hub is the hub's end of the session.
	hub conduit.LocalSession
	// ends receives each end of the dialer's session (none while it is up).
	ends <-chan dialerEnd
	// stop stops the dialer, which closes its session.
	stop func() error
}

// newConduitSessionPair connects a broker dialer session serving rpc to a
// hub session. Both are closed when the test ends.
func newConduitSessionPair(t *testing.T, rpc conduit.RPCHandler) *brokerSessionPair {
	t.Helper()
	return newConduitSessionPairConfig(t, conduit.Config{RPCHandler: rpc})
}

// newConduitSessionPairConfig starts the broker's conduit dialer with
// brokerCfg as its session config (ConduitDialConfig.Session, which
// carries the RPC handler) against a fake hub, and returns the session
// pair once the hub has admitted it.
func newConduitSessionPairConfig(t *testing.T, brokerCfg conduit.Config) *brokerSessionPair {
	t.Helper()
	admitted := make(chan conduit.LocalSession, 4)
	hub := newFakeConduitHub(t, hubStep{after: func(ls conduit.LocalSession) { admitted <- ls }})
	ends, _, stop := startTestDialer(t, hub.srv.URL, clock.Real(), maxRand, func(c *ConduitDialConfig) {
		c.Session = brokerCfg
	})
	p := &brokerSessionPair{ends: ends, stop: stop}
	select {
	case p.hub = <-admitted:
	case <-time.After(10 * time.Second):
		t.Fatal("the broker's conduit session was never admitted")
	}
	return p
}

func newConduitHarness(t *testing.T, handler http.Handler, slots int) tunnelHarness {
	h := &conduitHarness{
		handler:   newConduitRPCHandler(handler, "", slots, slog.Default()),
		queued:    make(chan string, 64),
		calls:     map[string]*conduitCall{},
		responses: map[string]int{},
	}
	h.handler.onQueued = func(id string) { h.queued <- id }
	countResponses := func(dir conduit.Direction, f *conduitv1.Frame) []*conduitv1.Frame {
		if r := f.GetRpcResponse(); dir == conduit.Outbound && r != nil {
			h.respMu.Lock()
			h.responses[r.GetRequestId()]++
			h.respMu.Unlock()
		}
		return []*conduitv1.Frame{f}
	}
	h.pair = newConduitSessionPairConfig(t, conduit.Config{RPCHandler: h.handler, Interceptor: countResponses})
	h.hub = h.pair.hub
	t.Cleanup(func() {
		h.mu.Lock()
		for _, c := range h.calls {
			c.cancel()
		}
		h.mu.Unlock()
		h.callsWG.Wait()
	})
	return h
}

func (h *conduitHarness) send(t *testing.T, req wsprotocol.RequestEnvelope) {
	ctx, cancel := context.WithCancel(context.Background())
	c := &conduitCall{cancel: cancel, done: make(chan struct{})}
	h.mu.Lock()
	h.calls[req.RequestID] = c
	h.mu.Unlock()
	h.callsWG.Add(1)
	go func() {
		defer h.callsWG.Done()
		defer close(c.done)
		c.resp, c.err = h.hub.Call(ctx, envelope.RequestToRPC(&req))
	}()
}

func (h *conduitHarness) call(t *testing.T, requestID string) *conduitCall {
	t.Helper()
	h.mu.Lock()
	c := h.calls[requestID]
	h.mu.Unlock()
	if c == nil {
		t.Fatalf("no call sent with request id %s", requestID)
	}
	return c
}

func (h *conduitHarness) cancel(t *testing.T, requestID string) {
	h.call(t, requestID).cancel()
}

func (h *conduitHarness) finished(t *testing.T, requestID string) *conduitCall {
	t.Helper()
	c := h.call(t, requestID)
	select {
	case <-c.done:
	case <-time.After(5 * time.Second):
		t.Fatalf("call %s did not finish", requestID)
	}
	return c
}

func (h *conduitHarness) response(t *testing.T, requestID string) wsprotocol.ResponseEnvelope {
	t.Helper()
	c := h.finished(t, requestID)
	if c.err != nil {
		t.Fatalf("call %s: %v", requestID, c.err)
	}
	return *envelope.RPCToResponse(c.resp)
}

// noResponse checks that the broker's session sends no RpcResponse for
// requestID within the same window the control channel leg waits, and
// that the Call returned without a response.
func (h *conduitHarness) noResponse(t *testing.T, requestID string) {
	t.Helper()
	if c := h.finished(t, requestID); c.err == nil {
		t.Errorf("unexpected response for %s: %v", requestID, c.resp)
	}
	time.Sleep(150 * time.Millisecond)
	h.respMu.Lock()
	n := h.responses[requestID]
	h.respMu.Unlock()
	if n != 0 {
		t.Errorf("broker session sent %d responses for %s, want 0", n, requestID)
	}
}

func (h *conduitHarness) awaitQueued(t *testing.T, n int) {
	t.Helper()
	for range n {
		select {
		case <-h.queued:
		case <-time.After(5 * time.Second):
			t.Fatal("request never reached the dispatch queue")
		}
	}
}

func (h *conduitHarness) occupy()   { h.handler.dispatchSem <- struct{}{} }
func (h *conduitHarness) release()  { <-h.handler.dispatchSem }
func (h *conduitHarness) shutdown() { _ = h.pair.stop() }
func (h *conduitHarness) wait(t *testing.T) {
	t.Helper()
	done := make(chan struct{})
	go func() { h.handler.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("conduit dispatches did not finish")
	}
}

// runOverTunnels runs test once per transport, as a subtest named after it.
func runOverTunnels(t *testing.T, test func(t *testing.T, tr tunnelTransport)) {
	for _, tr := range tunnelTransports {
		t.Run(tr.name, func(t *testing.T) { test(t, tr) })
	}
}
