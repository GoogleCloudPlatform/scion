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
	"log/slog"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/router"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// Target-side deadlines at stream open (design §3.5 "Target-side
// deadlines", Open grant and Gating; contracts "Renewal path
// unavailable"). A user stream to a target that advertises
// stream_renewal.v2 gets an open grant carrying its origin (via), its
// scoped-token context (scope) and its authorization deadline
// (authz_until). With server.hub.conduit.target_stream_deadlines =
// enforce, such a target closes the stream at that deadline unless a
// renewal grant moves it, so the hub opens the stream only on a route it
// can deliver renewals on. Every open path (tunnel, relay-held tunnel,
// port proxy, PTY preflight and open) decides that through
// conduitRenewalPathUnavailable.

// errConduitStreamRenewalUnavailable refuses a user stream open, before
// any grant is minted, when target-side deadlines are enforced, the target
// would get authz_until, and this node cannot deliver renewals on the
// route (503 unavailable, reason stream_renewal_unavailable).
var errConduitStreamRenewalUnavailable = errors.New("conduit: the hub cannot keep this stream authorized yet")

// errConduitGrantInternal marks an open grant the hub does not mint for an
// internal reason: the stream's scope cannot be determined, or the grant
// would exceed the grant size limit. The open fails with 500
// internal_error; nothing is minted and the cause is logged.
var errConduitGrantInternal = errors.New("conduit: open grant not minted")

// conduitReasonStreamRenewalUnavailable is the error.details.reason (and
// PTY close reason) of errConduitStreamRenewalUnavailable.
const conduitReasonStreamRenewalUnavailable = "stream_renewal_unavailable"

// conduitStreamRenewalUnavailableMessage is the client message of the
// refusal. It names no internal topology.
const conduitStreamRenewalUnavailableMessage = "The hub cannot keep this stream authorized yet; try again later"

// Open routes, the route attribute of the refusal log and metric.
const (
	conduitRouteTunnel      = "tunnel"
	conduitRouteRelayTunnel = "relay_tunnel"
	conduitRouteProxy       = "proxy"
	conduitRoutePTY         = "pty"
)

// conduitRouteOf names the open route of an origin, for logs.
func conduitRouteOf(via string) string {
	switch via {
	case grant.ViaTunnel:
		return conduitRouteTunnel
	case grant.ViaProxy:
		return conduitRouteProxy
	case grant.ViaPTY:
		return conduitRoutePTY
	default:
		return "none"
	}
}

// conduitStampsDeadline reports whether an open grant carries via, scope
// and authz_until: a user stream to a stream_renewal.v2 target (design
// §3.5, Open grant). Agent-originated streams and other targets get the
// payload unchanged.
func conduitStampsDeadline(identity Identity, targetRenews bool) bool {
	if !targetRenews || isNilIdentity(identity) {
		return false
	}
	_, user := identity.(UserIdentity)
	return user
}

// conduitTargetRenews reports whether the session res resolved advertises
// stream renewal (conduit.FeatureStreamRenewal), read from the registry
// row the grant binds to, so the capability check and the mint read the
// same row (design §3.5, Check point). A legacy broker session never
// does.
func conduitTargetRenews(res router.Resolved) bool {
	if res.Legacy != nil {
		return false
	}
	return res.Record.Capabilities.HasFeature(conduit.FeatureStreamRenewal)
}

// conduitRouteCanRenew reports whether this node can deliver renewal grants
// for a user stream opened on res. The hub's renewal driver for hub-held
// streams lands in a follow-up change, and relay-held streams are renewed
// through the relay's re-check, which is not wired yet; until then no
// route can, so with enforce every user stream to a renewal-capable target
// is refused rather than left to end at its first deadline.
func conduitRouteCanRenew(res router.Resolved) bool {
	_ = res
	return false
}

// conduitTargetStreamDeadlines returns the configured target-side stream
// deadline mode, "off" or "enforce".
func (s *Server) conduitTargetStreamDeadlines() string {
	if s.config.ConduitTargetStreamDeadlines == config.ConduitTargetStreamDeadlinesEnforce {
		return config.ConduitTargetStreamDeadlinesEnforce
	}
	return config.ConduitTargetStreamDeadlinesOff
}

// conduitRenewalPathUnavailable is the one decision behind
// stream_renewal_unavailable (contracts, Renewal path unavailable):
// target-side deadlines are enforced, the stream's open grant would carry
// authz_until (conduitStampsDeadline), and canRenew is false. Tunnel,
// relay-held tunnel, port proxy and PTY path selection (preflight and open,
// before any upgrade) call it at resolution, and mintConduitGrant calls it
// again as the backstop for a re-resolution onto another row. A refusal is
// logged with the route and counted by reason.
func (s *Server) conduitRenewalPathUnavailable(ctx context.Context, identity Identity, targetRenews, canRenew bool, route, agentID string) bool {
	if canRenew || !conduitStampsDeadline(identity, targetRenews) ||
		s.conduitTargetStreamDeadlines() != config.ConduitTargetStreamDeadlinesEnforce {
		return false
	}
	slog.WarnContext(ctx, "Conduit stream open refused: renewals cannot be delivered on this route",
		"route", route, "agent_id", agentID, "reason", conduitReasonStreamRenewalUnavailable)
	if r := s.conduitOpenRefusedMetrics.Load(); r != nil {
		(*r).RecordConduitOpenRefused(route, conduitReasonStreamRenewalUnavailable)
	}
	return true
}

// conduitOpenRefusedMetrics counts user stream opens refused before any
// grant is minted.
type conduitOpenRefusedMetrics interface {
	RecordConduitOpenRefused(route, reason string)
}

// OTelConduitOpenRefusedMetrics counts user stream opens refused before
// any grant is minted as scion.hub.conduit.stream_open_refused (attributes
// route, reason).
type OTelConduitOpenRefusedMetrics struct {
	total metric.Int64Counter
}

// NewOTelConduitOpenRefusedMetrics creates the counter.
func NewOTelConduitOpenRefusedMetrics(mp metric.MeterProvider) (*OTelConduitOpenRefusedMetrics, error) {
	if mp == nil {
		return nil, errors.New("otel conduit open refused metrics: nil MeterProvider")
	}
	c, err := mp.Meter(instrumentationScope).Int64Counter("scion.hub.conduit.stream_open_refused",
		metric.WithUnit("{open}"))
	if err != nil {
		return nil, err
	}
	return &OTelConduitOpenRefusedMetrics{total: c}, nil
}

// RecordConduitOpenRefused implements conduitOpenRefusedMetrics.
func (m *OTelConduitOpenRefusedMetrics) RecordConduitOpenRefused(route, reason string) {
	m.total.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("route", route),
		attribute.String("reason", reason),
	))
}

// SetConduitOpenRefusedMetrics wires the
// scion.hub.conduit.stream_open_refused counter.
func (s *Server) SetConduitOpenRefusedMetrics(m *OTelConduitOpenRefusedMetrics) {
	if m == nil {
		return
	}
	var r conduitOpenRefusedMetrics = m
	s.conduitOpenRefusedMetrics.Store(&r)
}

// conduitScopeOf returns the scope claim of identity (conduitScopeFor).
// conduitScopeOfTest, set only by tests, replaces it.
func (s *Server) conduitScopeOf(identity Identity) (*grant.Scope, error) {
	if f := s.conduitScopeOfTest; f != nil {
		return f(identity)
	}
	return conduitScopeFor(identity)
}

// conduitScopeFunc computes an identity's scope claim.
type conduitScopeFunc func(Identity) (*grant.Scope, error)
