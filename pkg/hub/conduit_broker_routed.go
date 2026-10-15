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
	"log/slog"
	"net/http"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/envelope"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/router"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// Transient broker requests through the conduit Router (design §3.5
// "Where route() ends up", §7 Phase 3 "3.3b items").
//
// With hub.conduit on, HybridBrokerClient sends logs, exec, image and
// project cleanup requests through Router.Resolve(broker) to a broker
// conduit session that lists the operation in Hello.capabilities.rpc, on
// this node or through its owner relay. The control channel and direct
// HTTP are the fallback only: they run when no request was sent over
// conduit (no eligible session, a session-layer refusal, the registry
// unreadable). Durable operations and keys keep route().
//
// Outcomes (contracts.md §2 "Ambiguous broker results"):
//   - a request never handed to a session's transport (no session, the
//     session ended first, a draining session) is re-resolved within the
//     router's shared budget, and then falls back;
//   - a session-layer refusal (X-Conduit-Refusal): unsupported falls back;
//     draining is retried at most once on a replacement session, and then
//     falls back;
//   - a request written to a session that then closed or was lost is
//     retried on another session when it is idempotent, and is
//     errBrokerRoutedUnavailable (503) when the budget is exhausted; a
//     non-idempotent request gets *brokerOutcomeUnknownError and is never
//     retried;
//   - a request that cannot be encoded fails alone; the session stays up.

// brokerRPC names the operation of a routed broker request (a broker RPC
// capability name, contracts.md §2) and whether it is idempotent.
type brokerRPC struct {
	op         string
	idempotent bool
}

type brokerRPCKey struct{}

// withBrokerRPC marks ctx with the operation a routed broker request
// performs.
func withBrokerRPC(ctx context.Context, op string, idempotent bool) context.Context {
	return context.WithValue(ctx, brokerRPCKey{}, brokerRPC{op: op, idempotent: idempotent})
}

func brokerRPCFrom(ctx context.Context) (brokerRPC, bool) {
	r, ok := ctx.Value(brokerRPCKey{}).(brokerRPC)
	return r, ok && r.op != ""
}

var (
	// errBrokerNotRouted: nothing was sent over conduit; the caller uses
	// the fallback (control channel, then direct HTTP).
	errBrokerNotRouted = errors.New("broker request not sent over conduit")
	// errBrokerRoutedUnavailable: an idempotent request was written to a
	// broker session that was lost, and no other session served it within
	// the request budget (503 unavailable).
	errBrokerRoutedUnavailable = errors.New("runtime broker unavailable over conduit: the request budget is exhausted")
)

// brokerOutcomeUnknownError is ErrOutcomeUnknown for a non-idempotent
// broker request (design §3.5 "Ambiguous results and session end"): it
// was written to a broker session whose result was not received, so the
// broker may have executed it. It is never retried.
type brokerOutcomeUnknownError struct {
	Operation string
	BrokerID  string
	RequestID string
	cause     error
}

func (e *brokerOutcomeUnknownError) Error() string {
	return fmt.Sprintf("broker %s: %s request %s may have executed; its result was not received: %v", e.BrokerID, e.Operation, e.RequestID, e.cause)
}

func (e *brokerOutcomeUnknownError) Unwrap() error { return e.cause }

// brokerRPCRouter is the part of *router.Router a routed tunnel uses.
type brokerRPCRouter interface {
	Do(ctx context.Context, req router.Request, fn func(context.Context, router.Resolved) error) error
}

// Fallback reasons recorded by routedBrokerTunnel.onFallback.
const (
	brokerRPCFallbackRegistryUnavailable = "registry_unavailable"
	brokerRPCFallbackBrokerUnreadable    = "broker_unreadable"
)

// routedBrokerTunnel is the controlChannelTunnel over the conduit Router.
// A ControlChannelBrokerClient over it builds, signs and decodes broker
// requests exactly as over the control channel. The operation comes from
// the request context (withBrokerRPC).
type routedBrokerTunnel struct {
	// router returns the node's router, or nil when conduit does not
	// serve on this node.
	router func() brokerRPCRouter
	// enabled reports whether hub.conduit is on (evaluated per request).
	enabled func() bool
	// execScope returns the broker's authoritative exec_scope.
	execScope func(ctx context.Context, brokerID string) (string, error)
	// timeout bounds each attempt (default 120s, as on the control
	// channel).
	timeout time.Duration
	// onFallback, when set, sees a fallback forced by a hub-side fault
	// (reason: brokerRPCFallback*).
	onFallback func(brokerID, op, reason string, err error)
}

var _ controlChannelTunnel = (*routedBrokerTunnel)(nil)

// IsConnected implements controlChannelTunnel: whether this node may route
// broker requests at all. Whether a session serves the request is decided
// per request; when none does, TunnelRequest returns errBrokerNotRouted.
func (t *routedBrokerTunnel) IsConnected(string) bool {
	return t.routable() != nil
}

func (t *routedBrokerTunnel) routable() brokerRPCRouter {
	if t == nil || t.router == nil || t.enabled == nil || !t.enabled() {
		return nil
	}
	return t.router()
}

func (t *routedBrokerTunnel) attemptTimeout() time.Duration {
	if t.timeout > 0 {
		return t.timeout
	}
	return DefaultControlChannelConfig().RequestTimeout
}

func (t *routedBrokerTunnel) fallback(brokerID, op, reason string, err error) {
	if t.onFallback != nil {
		t.onFallback(brokerID, op, reason, err)
	}
}

// TunnelRequest implements controlChannelTunnel.
func (t *routedBrokerTunnel) TunnelRequest(ctx context.Context, brokerID string, req *wsprotocol.RequestEnvelope) (*wsprotocol.ResponseEnvelope, error) {
	ctx, span := tracer.Start(ctx, "hub.conduit.broker_rpc")
	defer span.End()
	span.SetAttributes(
		attribute.String("scion.broker.id", brokerID),
		attribute.String("scion.request.method", req.Method),
	)
	resp, err := t.tunnel(ctx, brokerID, req)
	if err != nil && !errors.Is(err, errBrokerNotRouted) {
		span.SetStatus(codes.Error, err.Error())
	}
	return resp, err
}

// routedAttempt records what the attempts of one request did.
type routedAttempt struct {
	// written: some attempt's request was handed to a session's
	// transport, so a broker may have received it.
	written bool
	// drainRetried: the one retry after a draining refusal was used.
	drainRetried bool
}

// errRoutedFallback ends Router.Do when nothing was sent and the
// remaining attempts must use the fallback.
var errRoutedFallback = errors.New("conduit broker request: use the fallback")

// errRoutedLost ends Router.Do for a non-idempotent request written to a
// session whose result was not received.
var errRoutedLost = errors.New("conduit broker request: result not received")

// routedPermanentError ends Router.Do with a failure of this request
// alone (it cannot be encoded, or the caller gave up): no retry, no
// fallback.
type routedPermanentError struct{ err error }

func (e *routedPermanentError) Error() string { return e.err.Error() }
func (e *routedPermanentError) Unwrap() error { return e.err }

func (t *routedBrokerTunnel) tunnel(ctx context.Context, brokerID string, req *wsprotocol.RequestEnvelope) (*wsprotocol.ResponseEnvelope, error) {
	spec, ok := brokerRPCFrom(ctx)
	if !ok {
		return nil, fmt.Errorf("%w: the request names no broker operation", errBrokerNotRouted)
	}
	rt := t.routable()
	if rt == nil {
		return nil, errBrokerNotRouted
	}
	scope, err := t.execScope(ctx, brokerID)
	if err != nil {
		// A broker that is no longer registered is not a hub-side fault.
		if !errors.Is(err, store.ErrNotFound) {
			t.fallback(brokerID, spec.op, brokerRPCFallbackBrokerUnreadable, err)
		}
		return nil, fmt.Errorf("%w: reading broker %s: %w", errBrokerNotRouted, brokerID, err)
	}
	if req.Headers == nil {
		req.Headers = make(map[string]string)
	}
	addTraceHeaders(ctx, req.Headers)
	rpc := envelope.RequestToRPC(req)
	if err := rpcTooLarge(req, rpc); err != nil {
		return nil, fmt.Errorf("%w: %w", errBrokerNotRouted, err)
	}

	rreq := router.Request{
		Op:   router.OpStatefulRPC,
		Kind: registry.PrincipalBroker,
		ID:   brokerID,
		Want: registry.Want{Capability: spec.op, ExecScope: scope},
	}
	var st routedAttempt
	var answer *conduitv1.RpcResponse
	err = rt.Do(ctx, rreq, func(ctx context.Context, res router.Resolved) error {
		r, err := t.attempt(ctx, res, rpc, spec, &st)
		if err == nil {
			answer = r
		}
		return err
	})
	switch {
	case err == nil:
		// Never forward the session-layer marker to API callers.
		conduit.StripRefusal(answer.Headers)
		return envelope.RPCToResponse(answer), nil
	case errors.Is(err, errRoutedLost):
		return nil, &brokerOutcomeUnknownError{Operation: spec.op, BrokerID: brokerID, RequestID: req.RequestID, cause: err}
	case ctx.Err() != nil:
		return nil, ctx.Err()
	}
	var perm *routedPermanentError
	if errors.As(err, &perm) {
		return nil, perm.err
	}
	if errors.Is(err, router.ErrRegistryUnavailable) {
		t.fallback(brokerID, spec.op, brokerRPCFallbackRegistryUnavailable, err)
	}
	if st.written {
		// An idempotent request reached a session that was lost, and
		// nothing else served it: the budget is exhausted.
		return nil, fmt.Errorf("%w: %w", errBrokerRoutedUnavailable, err)
	}
	return nil, fmt.Errorf("%w: %w", errBrokerNotRouted, err)
}

// attempt runs one call on a resolved session and classifies its result
// for Router.Do: a nil error with the broker's answer, a stale route to
// exclude the session and re-resolve, or an error that ends the request.
func (t *routedBrokerTunnel) attempt(ctx context.Context, res router.Resolved, rpc *conduitv1.RpcRequest, spec brokerRPC, st *routedAttempt) (*conduitv1.RpcResponse, error) {
	if res.Session == nil {
		// A legacy resolution: not a conduit session. Brokers resolve
		// legacy sessions for streams only, so this does not happen.
		return nil, errRoutedFallback
	}
	callCtx, cancel := context.WithTimeout(ctx, t.attemptTimeout())
	defer cancel()
	r, err := res.Session.Call(callCtx, rpc)
	if err == nil {
		switch conduit.RefusalOf(r) {
		case conduit.RefusalUnsupported:
			// The session serves no RPCs although its Hello listed the
			// operation: nothing ran.
			return nil, errRoutedFallback
		case conduit.RefusalDraining:
			return nil, st.drained()
		}
		return r, nil
	}
	var stale *relay.StaleRouteError
	switch {
	case errors.Is(err, conduit.ErrDraining):
		return nil, st.drained()
	case errors.As(err, &stale) && stale.Reason == string(registry.ReasonDraining):
		return nil, st.drained()
	case errors.Is(err, conduit.ErrRequestNotSent):
		return nil, &relay.StaleRouteError{Reason: "not_sent"}
	case errors.As(err, &stale) && ownerStaleReason(stale.Reason), errors.Is(err, relay.ErrOwnerUnreachable):
		// Refused before the target saw it: the router re-resolves.
		return nil, err
	case errors.Is(err, conduit.ErrRPCEncode):
		return nil, &routedPermanentError{err: err}
	case ctx.Err() != nil:
		// The caller gave up; Call sent RpcCancel.
		return nil, &routedPermanentError{err: ctx.Err()}
	}
	// The request was written and its result was not received: the
	// session closed or was lost, the owner relay lost it, or the attempt
	// timed out.
	st.written = true
	if errors.Is(err, relay.ErrStaleRoute) {
		// A stale route outside the owner's closed set: keep only its
		// text, so the router does not re-resolve on it.
		err = errors.New("owner relay refused with an unrecognised stale reason: " + err.Error())
	}
	if callCtx.Err() != nil {
		err = &brokerTransportError{msg: fmt.Sprintf("request timeout after %v", t.attemptTimeout()), sentinel: errBrokerResponseTimeout}
	}
	if !spec.idempotent {
		return nil, fmt.Errorf("%w: %w", errRoutedLost, err)
	}
	if errors.Is(err, errBrokerResponseTimeout) {
		// A slow broker: another session would wait as long.
		return nil, &routedPermanentError{err: err}
	}
	slog.Debug("Conduit broker request lost with its session; retrying on another session",
		"broker_id", res.Record.PrincipalID, "session_id", res.Record.SessionID, "operation", spec.op, "error", err)
	return nil, &relay.StaleRouteError{Reason: "session_lost"}
}

// ownerStaleReasons is the closed set of X-Conduit-Stale-Reason values an
// owner relay sends with the 404 or 409 that refuses a route before the
// target saw the request: not_local, the registry admission reasons,
// relay_target_mismatch, relay_not_serving, draining and not_sent. On the
// routed broker path only a stale route with one of them is re-resolved;
// one without the header, or with any other value, is handled under the
// operation class like any other failure (the request may have reached
// the broker).
var ownerStaleReasons = map[string]bool{
	"not_local":                                true,
	"relay_target_mismatch":                    true,
	"relay_not_serving":                        true,
	"not_sent":                                 true,
	string(registry.ReasonNotFound):            true,
	string(registry.ReasonNotRoutable):         true,
	string(registry.ReasonRelayMissing):        true,
	string(registry.ReasonRelaySuperseded):     true,
	string(registry.ReasonRelayStale):          true,
	string(registry.ReasonSessionStale):        true,
	string(registry.ReasonDraining):            true,
	string(registry.ReasonProjectMismatch):     true,
	string(registry.ReasonExecScopeMismatch):   true,
	string(registry.ReasonIncarnationMismatch): true,
	string(registry.ReasonCapabilityMissing):   true,
	string(registry.ReasonEpochObsolete):       true,
}

// ownerStaleReason reports whether reason is in ownerStaleReasons.
func ownerStaleReason(reason string) bool { return ownerStaleReasons[reason] }

// drained handles a draining session's refusal: the first is retried on a
// replacement session (the router excludes this one), a second falls back.
func (st *routedAttempt) drained() error {
	if st.drainRetried {
		return errRoutedFallback
	}
	st.drainRetried = true
	return &relay.StaleRouteError{Reason: string(registry.ReasonDraining)}
}

// routedBrokerFallback reports whether err from a routed broker request
// means the fallback must run: nothing was sent over conduit.
func routedBrokerFallback(err error) bool {
	return errors.Is(err, errBrokerNotRouted) || errors.Is(err, errStartRequestNotSent) || errors.Is(err, errStartBrokerNotConnected)
}

// newRoutedBrokerTunnel returns the routed tunnel of this hub node: the
// node's conduit router while hub.conduit is on, each broker's exec_scope
// from its broker row, the control channel's request timeout, and the
// fallback log and counter.
func (s *Server) newRoutedBrokerTunnel() *routedBrokerTunnel {
	t := &routedBrokerTunnel{
		router: func() brokerRPCRouter {
			if r := s.conduitRouter(); r != nil {
				return r
			}
			return nil
		},
		enabled: s.conduitRoutable,
		execScope: func(ctx context.Context, brokerID string) (string, error) {
			b, err := s.store.GetRuntimeBroker(ctx, brokerID)
			if err != nil {
				return "", err
			}
			return brokerExecScope(b), nil
		},
		onFallback: s.recordBrokerRPCFallback,
	}
	if s.controlChannel != nil {
		t.timeout = s.controlChannel.config.RequestTimeout
	}
	return t
}

// brokerOutcomeUnknownMessage is the fixed message of 502 outcome_unknown.
const brokerOutcomeUnknownMessage = "The runtime broker may have executed the request, and its result was not received; it was not retried"

// writeBrokerOutcomeUnknown writes 502 outcome_unknown for an ambiguous
// broker result (contracts.md §2 "Ambiguous broker results") and reports
// whether it did. The body carries the fixed message and details
// {operation, broker_id, request_id}, never broker or command output.
func writeBrokerOutcomeUnknown(w http.ResponseWriter, err error) bool {
	var ou *brokerOutcomeUnknownError
	if !errors.As(err, &ou) {
		return false
	}
	writeError(w, http.StatusBadGateway, ErrCodeOutcomeUnknown, brokerOutcomeUnknownMessage, map[string]interface{}{
		"operation":  ou.Operation,
		"broker_id":  ou.BrokerID,
		"request_id": ou.RequestID,
	})
	return true
}

// writeBrokerRoutedUnavailable writes 503 unavailable for an idempotent
// broker request whose request budget was exhausted, and reports whether
// it did.
func writeBrokerRoutedUnavailable(w http.ResponseWriter, err error) bool {
	if !errors.Is(err, errBrokerRoutedUnavailable) {
		return false
	}
	writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable,
		"The runtime broker could not be reached over any of its sessions; retry later", nil)
	return true
}
