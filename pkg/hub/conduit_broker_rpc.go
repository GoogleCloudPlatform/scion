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
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"google.golang.org/protobuf/proto"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/envelope"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// conduitBrokerTunnel carries the hub's tunnelled broker requests over a
// broker's conduit session instead of its control channel. It satisfies
// controlChannelTunnel, the interface ControlChannelBrokerClient tunnels
// through, so the broker client works unchanged over either connection.
//
// Each request envelope maps one to one onto an RpcRequest and each
// RpcResponse back onto a response envelope (pkg/conduit/envelope). The
// broker serves it with the same HTTP handlers as a control channel
// request.
//
// Errors keep the control channel's meaning for callers:
//   - no session for the broker: errStartBrokerNotConnected;
//   - a request too large for a session (which a session would answer
//     with RpcResponse{status:413} without sending it): checked before the
//     call and reported as *ErrPayloadTooLarge wrapped with
//     errStartRequestNotSent, so HybridBrokerClient falls back to direct
//     HTTP as it does for the control channel. Every status the call
//     returns, 413 included, is passed through as the broker's response;
//   - a draining session (it refuses new requests before sending):
//     errStartRequestNotSent;
//   - any failure once the request may have been sent (session lost,
//     timeout, caller cancelled) is returned without either marker, so
//     callers treat the outcome as unknown and do not retry.
//
// The caller's context and the request timeout propagate to the broker as
// RpcCancel.
//
// sessions resolves a broker to its conduit session; it does not route
// (routing belongs to the conduit router).
type conduitBrokerTunnel struct {
	sessions func(brokerID string) conduit.Session
	// timeout bounds each request, as ControlChannelConfig.RequestTimeout
	// does on the control channel (default 120s).
	timeout time.Duration
}

var _ controlChannelTunnel = (*conduitBrokerTunnel)(nil)

// newConduitBrokerTunnel returns a tunnel over the sessions sessions
// returns (nil when the broker has none). A zero timeout is 120s.
func newConduitBrokerTunnel(sessions func(brokerID string) conduit.Session, timeout time.Duration) *conduitBrokerTunnel {
	if timeout <= 0 {
		timeout = DefaultControlChannelConfig().RequestTimeout
	}
	return &conduitBrokerTunnel{sessions: sessions, timeout: timeout}
}

// session returns the broker's live session, or nil.
func (t *conduitBrokerTunnel) session(brokerID string) conduit.Session {
	if t.sessions == nil {
		return nil
	}
	s := t.sessions(brokerID)
	if s == nil {
		return nil
	}
	if ls, ok := s.(conduit.LocalSession); ok {
		select {
		case <-ls.Done():
			return nil
		default:
		}
	}
	return s
}

// IsConnected implements controlChannelTunnel.
func (t *conduitBrokerTunnel) IsConnected(brokerID string) bool {
	return t.session(brokerID) != nil
}

// TunnelRequest implements controlChannelTunnel.
func (t *conduitBrokerTunnel) TunnelRequest(ctx context.Context, brokerID string, req *wsprotocol.RequestEnvelope) (*wsprotocol.ResponseEnvelope, error) {
	ctx, span := tracer.Start(ctx, "hub.conduit.tunnel")
	defer span.End()
	span.SetAttributes(
		attribute.String("scion.broker.id", brokerID),
		attribute.String("scion.request.method", req.Method),
	)
	resp, err := t.tunnel(ctx, brokerID, req)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
	}
	return resp, err
}

func (t *conduitBrokerTunnel) tunnel(ctx context.Context, brokerID string, req *wsprotocol.RequestEnvelope) (*wsprotocol.ResponseEnvelope, error) {
	// Inject trace context into the request envelope headers for
	// cross-component propagation, as on the control channel.
	if req.Headers == nil {
		req.Headers = make(map[string]string)
	}
	otel.GetTextMapPropagator().Inject(ctx, propagation.MapCarrier(req.Headers))

	s := t.session(brokerID)
	if s == nil {
		return nil, fmt.Errorf("broker %s has no conduit session: %w", brokerID, errStartBrokerNotConnected)
	}
	rpc := envelope.RequestToRPC(req)
	if err := rpcTooLarge(req, rpc); err != nil {
		// A session answers such a request with 413 without sending it:
		// fall back to direct HTTP, as for a body too large for the
		// control channel.
		return nil, fmt.Errorf("%w (%w)", err, errStartRequestNotSent)
	}

	callCtx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	resp, err := s.Call(callCtx, rpc)
	switch {
	case err == nil:
		// Every status, 413 included, is the broker's response.
		return envelope.RPCToResponse(resp), nil
	case errors.Is(err, conduit.ErrDraining):
		return nil, fmt.Errorf("broker %s conduit session draining: %w (%w)", brokerID, err, errStartRequestNotSent)
	case ctx.Err() != nil:
		// The caller gave up; Call sent RpcCancel.
		return nil, ctx.Err()
	case callCtx.Err() != nil:
		// Past our own request timeout; Call sent RpcCancel.
		return nil, fmt.Errorf("request timeout after %v", t.timeout)
	default:
		return nil, fmt.Errorf("conduit request failed: %w", err)
	}
}

// rpcTooLarge returns *ErrPayloadTooLarge for a request a session would
// refuse with 413 before sending it: a body over conduit.MaxRPCBody, or an
// encoded frame (body, headers, path and query) over the default RPC frame
// limit.
func rpcTooLarge(req *wsprotocol.RequestEnvelope, rpc *conduitv1.RpcRequest) error {
	if n := len(rpc.GetBody()); n > conduit.MaxRPCBody {
		return &ErrPayloadTooLarge{Method: req.Method, Path: req.Path, Size: n, Limit: conduit.MaxRPCBody}
	}
	// Broker sessions use the default session config (nothing sets
	// MaxRPCFrame), so the default frame limit is the session's limit.
	if n := proto.Size(&conduitv1.Frame{Body: &conduitv1.Frame_RpcRequest{RpcRequest: rpc}}); n > conduit.DefaultMaxRPCFrame {
		return &ErrPayloadTooLarge{Method: req.Method, Path: req.Path, Size: n, Limit: conduit.DefaultMaxRPCFrame}
	}
	return nil
}
