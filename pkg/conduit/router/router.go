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

// Package router resolves a target principal (agent or broker) to a live
// conduit session: local first, then the owning relay over the internal
// API (design §3.5). Resolution fails closed: a registry read error is a
// refusal, never a guess. The Want's endpoint incarnation is derived only
// through the relay package's incarnation policy, the same functions
// admission uses, so routing and admission cannot drift.
package router

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
)

// MaxReResolves bounds re-resolution after a stale route (design §3.5).
const MaxReResolves = 2

// Errors.
var (
	// ErrNoSession: no eligible session for the target (after at most
	// MaxReResolves re-resolutions).
	ErrNoSession = errors.New("conduit router: no eligible session")
	// ErrRegistryUnavailable: the registry could not be read; the
	// operation is refused (fail closed).
	ErrRegistryUnavailable = errors.New("conduit router: registry unavailable")
	// ErrInvalidRequest: the request is malformed (for example
	// AnyExecScope on a stateful operation, or a caller-supplied
	// incarnation).
	ErrInvalidRequest = errors.New("conduit router: invalid request")
)

// Op classifies the operation; it decides whether AnyExecScope is allowed.
type Op int

const (
	// OpStream opens a stream (stateful).
	OpStream Op = iota
	// OpStatefulRPC is an RPC whose effect depends on the runtime scope.
	OpStatefulRPC
	// OpStatelessRPC is an RPC any exec scope may serve; only this class
	// may set Want.AnyExecScope.
	OpStatelessRPC
)

func (o Op) String() string {
	switch o {
	case OpStream:
		return "stream"
	case OpStatefulRPC:
		return "stateful_rpc"
	case OpStatelessRPC:
		return "stateless_rpc"
	}
	return fmt.Sprintf("op(%d)", int(o))
}

// Request names the target and what the caller needs.
type Request struct {
	Op   Op
	Kind string // registry.PrincipalAgent | registry.PrincipalBroker
	ID   string
	// Want is the complete expectation except Incarnation, which must be
	// empty: the router derives it from Agent / BrokerIncarnation.
	Want registry.Want
	// Agent: the agent row's launch_id and generation (agents).
	Agent relay.AgentIncarnationFacts
	// BrokerIncarnation: the broker incarnation the caller knows (brokers).
	BrokerIncarnation string
}

// Config configures a Router.
type Config struct {
	// Relay is the local relay (local sessions are used directly).
	Relay *relay.Relay
	// Registry answers Eligible.
	Registry *registry.Registry
	// Store reads the owning relay's endpoint
	// (ListPrincipalSessionsBySession). Follow-up: a registry.Relay(ctx,
	// id) accessor would remove this dependency.
	Store registry.Store
	// Peers calls other relays (injectable for tests).
	Peers *relay.PeerClient
	// Now defaults to time.Now.
	Now func() time.Time
}

// Router resolves targets to sessions.
type Router struct{ cfg Config }

// New returns a Router.
func New(cfg Config) (*Router, error) {
	if cfg.Relay == nil || cfg.Registry == nil || cfg.Store == nil || cfg.Peers == nil {
		return nil, errors.New("conduit router: Relay, Registry, Store and Peers are required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Router{cfg: cfg}, nil
}

// Resolved is a resolution result.
type Resolved struct {
	Session conduit.Session
	Record  registry.SessionRecord
	// Want is the exact expectation the session was resolved with
	// (including the derived incarnation); grants are minted against it.
	Want registry.Want
	// Local reports whether the session is held by this relay.
	Local bool
}

// wants returns the request's Wants in policy order.
func (r *Router) wants(req Request) ([]registry.Want, error) {
	if req.Want.Incarnation != "" {
		return nil, fmt.Errorf("%w: Want.Incarnation is derived by the incarnation policy; pass the agent/broker facts instead", ErrInvalidRequest)
	}
	if req.Want.AnyExecScope && req.Op != OpStatelessRPC {
		return nil, fmt.Errorf("%w: AnyExecScope is only allowed for stateless RPCs (op %s)", ErrInvalidRequest, req.Op)
	}
	var incs []relay.Incarnation
	switch req.Kind {
	case registry.PrincipalAgent:
		incs = relay.RouteAgentIncarnations(req.Agent)
	case registry.PrincipalBroker:
		incs = relay.RouteBrokerIncarnations(req.BrokerIncarnation)
	default:
		return nil, fmt.Errorf("%w: principal kind %q is not routable", ErrInvalidRequest, req.Kind)
	}
	if len(incs) == 0 {
		return nil, fmt.Errorf("%w: no authoritative incarnation for %s %s", ErrInvalidRequest, req.Kind, req.ID)
	}
	out := make([]registry.Want, len(incs))
	for i, inc := range incs {
		w := req.Want
		w.Incarnation = inc.Value
		out[i] = w
	}
	return out, nil
}

// Resolve returns a session for req, skipping session ids in exclude. It
// tries the policy's incarnations in order (for agents: the current
// launch id, then the interim gen-N value) and, within one, the
// registry's ranking. A candidate this relay owns is returned as the live
// local session; any other is reached through its owner's registered
// internal endpoint, provided the owner row is of the session's
// generation and addressable.
func (r *Router) Resolve(ctx context.Context, req Request, exclude map[string]bool) (Resolved, error) {
	wants, err := r.wants(req)
	if err != nil {
		return Resolved{}, err
	}
	self := r.cfg.Relay.InstanceID()
	for _, w := range wants {
		recs, err := r.cfg.Registry.Eligible(ctx, req.Kind, req.ID, w, r.cfg.Now())
		if err != nil {
			if errors.Is(err, registry.ErrIncompleteWant) || errors.Is(err, registry.ErrInvalidInput) || errors.Is(err, registry.ErrNotRoutable) {
				return Resolved{}, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
			}
			return Resolved{}, fmt.Errorf("%w: %w", ErrRegistryUnavailable, err)
		}
		for _, rec := range recs {
			if exclude[rec.SessionID] {
				continue
			}
			if rec.RelayInstanceID == self {
				ls, lrec, ok := r.cfg.Relay.Local(rec.SessionID)
				if !ok || lrec.RelayGeneration != rec.RelayGeneration {
					continue // ending, or a previous generation's row
				}
				return Resolved{Session: ls, Record: rec, Want: w, Local: true}, nil
			}
			endpoint, err := r.ownerEndpoint(ctx, rec)
			if err != nil {
				return Resolved{}, err
			}
			if endpoint == "" {
				continue
			}
			return Resolved{Session: relay.NewRemoteSession(r.cfg.Peers, endpoint, rec, w), Record: rec, Want: w}, nil
		}
	}
	return Resolved{}, ErrNoSession
}

// ownerEndpoint returns the owning relay's internal endpoint, or "" when
// the owner moved on (different generation, gone) or is unaddressable.
func (r *Router) ownerEndpoint(ctx context.Context, rec registry.SessionRecord) (string, error) {
	ps, found, err := r.cfg.Store.ListPrincipalSessionsBySession(ctx, rec.SessionID)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrRegistryUnavailable, err)
	}
	if !found {
		return "", nil
	}
	for _, v := range ps.Sessions {
		if v.Session.SessionID != rec.SessionID {
			continue
		}
		if v.Relay == nil || v.Relay.InstanceID != rec.RelayInstanceID || v.Relay.Generation != rec.RelayGeneration {
			return "", nil
		}
		return v.Relay.InternalEndpoint, nil
	}
	return "", nil
}

// Do resolves req and runs fn on the session. If fn reports a stale route
// (relay.ErrStaleRoute: the owner no longer holds the session or it is no
// longer admissible), the session is excluded and req re-resolved, at most
// MaxReResolves times; then ErrNoSession. fn receives the resolution so it
// can mint a grant for exactly that session (session id, epoch,
// incarnation) on every attempt.
func (r *Router) Do(ctx context.Context, req Request, fn func(context.Context, Resolved) error) error {
	exclude := map[string]bool{}
	for attempt := 0; ; attempt++ {
		res, err := r.Resolve(ctx, req, exclude)
		if err != nil {
			return err
		}
		err = fn(ctx, res)
		if !errors.Is(err, relay.ErrStaleRoute) {
			return err
		}
		if attempt >= MaxReResolves {
			return fmt.Errorf("%w (after %d re-resolutions): %w", ErrNoSession, attempt, err)
		}
		exclude[res.Record.SessionID] = true
	}
}
