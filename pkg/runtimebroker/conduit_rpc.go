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

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/envelope"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// conduitRPCHandler serves the Hub's RPCs on the broker's conduit session
// (conduit.Config.RPCHandler of the dialer). Each RpcRequest maps one to
// one onto the control channel's request envelope and runs through the
// same HTTP handlers as a control channel request (serveTunneledRequest),
// so both connections reach one handler tree with the same headers,
// panic recovery and response shape.
//
// As on the control channel, at most a bounded number of requests run at
// once and the rest wait for a slot. The session cancels a request's
// context when the Hub sends RpcCancel or the session ends; a request
// cancelled while it waits never runs its handler, and the session sends
// no response for a cancelled request.
type conduitRPCHandler struct {
	handlers       http.Handler
	connectionName string
	log            *slog.Logger
	dispatchSem    chan struct{}
	// wg tracks running HandleRPC calls.
	wg sync.WaitGroup
	// onQueued, if set (tests), runs when a request starts waiting for a
	// dispatch slot.
	onQueued func(requestID string)
}

var _ conduit.RPCHandler = (*conduitRPCHandler)(nil)

// newConduitRPCHandler returns the RPC handler for a conduit session of
// the hub connection connectionName, dispatching to handlers with at most
// slots requests at once (defaultMaxConcurrentDispatches when slots <= 0).
func newConduitRPCHandler(handlers http.Handler, connectionName string, slots int, log *slog.Logger) *conduitRPCHandler {
	if slots <= 0 {
		slots = defaultMaxConcurrentDispatches
	}
	if log == nil {
		log = slog.Default()
	}
	return &conduitRPCHandler{
		handlers:       handlers,
		connectionName: connectionName,
		log:            log,
		dispatchSem:    make(chan struct{}, slots),
	}
}

// HandleRPC implements conduit.RPCHandler.
func (h *conduitRPCHandler) HandleRPC(ctx context.Context, req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
	h.wg.Add(1)
	defer h.wg.Done()

	if h.onQueued != nil {
		h.onQueued(req.GetRequestId())
	}
	select {
	case h.dispatchSem <- struct{}{}:
		defer func() { <-h.dispatchSem }()
	case <-ctx.Done():
		h.logQueuedCancel(req)
		return nil
	}
	// select picks at random when a slot and the cancel are both ready:
	// recheck so a request cancelled while queued never runs its handler.
	if ctx.Err() != nil {
		h.logQueuedCancel(req)
		return nil
	}

	var resp *wsprotocol.ResponseEnvelope
	serveTunneledRequest(ctx, h.handlers, h.connectionName, "broker.conduit.dispatch", h.log, *envelope.RPCToRequest(req),
		func(r *wsprotocol.ResponseEnvelope) error {
			resp = r
			return nil
		})
	if resp == nil {
		return nil // the session answers 500
	}
	return envelope.ResponseToRPC(resp)
}

// logQueuedCancel records, at debug level, that req was cancelled before
// it obtained a dispatch slot and so never ran.
func (h *conduitRPCHandler) logQueuedCancel(req *conduitv1.RpcRequest) {
	h.log.Debug("Conduit request cancelled while queued; not dispatched",
		"requestID", req.GetRequestId(), "method", req.GetMethod(), "path", req.GetPath())
}
