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
// deadlines", Open grant, Gating and Principals without a renewal path,
// v2.21.36; contracts "Renewal path
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

// conduitRouteCanRenew reports whether this node delivers renewal grants to
// the target of a hub-held user stream opened on res (the renewal driver,
// conduit_stream_renewal.go). In an in-process profile it does: on a
// session this node holds, AuthRefresh goes on that session; on a session
// another hub node's relay holds, the stream's renewer sends it across the
// internal hop, and the owner relay forwards it to the target stream. In
// external relay mode a hub-held leaf reaches the target through its bridge
// to the separate relay that holds the target's session
// (openConduitTargetStream), and the renewer sends AuthRefresh on that
// bridge (relay.Bridge is a conduit.StreamRenewer), which the owner relay
// forwards unchanged (design §3.9, Hub-held leaves). Only a resolution
// with no session, or a legacy broker session, has no renewal path; with
// enforce a renewal-capable stream on it is refused (503
// stream_renewal_unavailable) rather than left to end at its first
// deadline. Relay-held streams (openForRelay) never call it; their
// renewals are the relay's.
func (s *Server) conduitRouteCanRenew(res router.Resolved) bool {
	if f := s.conduitRouteCanRenewTest; f != nil {
		return f(res)
	}
	return res.Session != nil && res.Legacy == nil
}

// conduitTargetStreamDeadlines returns the configured target-side stream
// deadline mode, "off" or "enforce".
func (s *Server) conduitTargetStreamDeadlines() string {
	if s.config.ConduitTargetStreamDeadlines == config.ConduitTargetStreamDeadlinesEnforce {
		return config.ConduitTargetStreamDeadlinesEnforce
	}
	return config.ConduitTargetStreamDeadlinesOff
}

// Causes of a stream_renewal_unavailable refusal: log and metric labels
// only, never sent to the caller (contracts v2.21.36).
const (
	// conduitCauseRenewalRouteUnavailable: this node cannot deliver
	// renewals on the stream's route.
	conduitCauseRenewalRouteUnavailable = "renewal_route_unavailable"
	// conduitCauseRenewalPrincipalUnsupported: the user principal has no
	// renewal path (conduitRenewalUnsupportedPrincipal).
	conduitCauseRenewalPrincipalUnsupported = "renewal_principal_unsupported"
)

// conduitRenewalPathRefusal is the condition of
// conduitRenewalPathUnavailable: whether the open is refused, and the
// cause. It logs and records nothing, so a caller can ask before any
// stream is opened.
func (s *Server) conduitRenewalPathRefusal(identity Identity, targetRenews, canRenew bool) (string, bool) {
	if !conduitStampsDeadline(identity, targetRenews) ||
		s.conduitTargetStreamDeadlines() != config.ConduitTargetStreamDeadlinesEnforce {
		return "", false
	}
	if _, unsupported := conduitRenewalUnsupportedPrincipal(identity); unsupported {
		return conduitCauseRenewalPrincipalUnsupported, true
	}
	if !canRenew {
		return conduitCauseRenewalRouteUnavailable, true
	}
	return "", false
}

// conduitRenewalPathUnavailable is the one decision behind
// stream_renewal_unavailable (contracts v2.21.36, Renewal path
// unavailable): target-side deadlines are enforced, the stream's open grant
// would carry authz_until (conduitStampsDeadline), and either the user
// principal is renewal-unsupported (design v2.21.36 §3.5, Principals
// without a renewal path) or canRenew is false. Tunnel, relay-held tunnel,
// port proxy and PTY path selection (preflight and open, before any
// upgrade) call it at resolution, and mintConduitGrant calls it again as
// the backstop for a re-resolution onto another row. A refusal is logged
// with the route and cause and counted by route, reason and cause.
func (s *Server) conduitRenewalPathUnavailable(ctx context.Context, identity Identity, targetRenews, canRenew bool, route, agentID string) bool {
	cause, refused := s.conduitRenewalPathRefusal(identity, targetRenews, canRenew)
	if !refused {
		return false
	}
	attrs := []any{"route", route, "agent_id", agentID, "reason", conduitReasonStreamRenewalUnavailable}
	if kind, unsupported := conduitRenewalUnsupportedPrincipal(identity); unsupported {
		attrs = append(attrs, "principal_kind", kind)
	}
	attrs = append(attrs, "cause", cause)
	slog.WarnContext(ctx, "Conduit stream open refused: the hub cannot keep this stream authorized", attrs...)
	if r := s.conduitOpenRefusedMetrics.Load(); r != nil {
		(*r).RecordConduitOpenRefused(route, conduitReasonStreamRenewalUnavailable, cause)
	}
	return true
}

// recordConduitOpenUnstamped counts an open grant minted unstamped for a
// renewal-unsupported principal under off (cause
// renewal_principal_unsupported).
func (s *Server) recordConduitOpenUnstamped(route, principalKind string) {
	s.recordConduitOpenUnstampedCause(route, principalKind, conduitCauseRenewalPrincipalUnsupported)
}

// recordConduitOpenUnstampedCause counts an open grant minted unstamped
// under off, by route, principal kind and cause: one of
// renewal_principal_unsupported (recordConduitOpenUnstamped) and
// renewal_route_unavailable (a relay-held tunnel forwarded by a relay that
// does not list tunnel_recheck; contracts dc2f4f22 §3).
func (s *Server) recordConduitOpenUnstampedCause(route, principalKind, cause string) {
	if r := s.conduitOpenRefusedMetrics.Load(); r != nil {
		(*r).RecordConduitOpenUnstamped(route, principalKind, cause)
	}
}

// conduitPrincipalKind is identity's principal kind for logs and metrics,
// "scoped_" prefixed for a scoped identity, as
// conduitRenewalUnsupportedPrincipal names it.
func conduitPrincipalKind(identity Identity) string {
	if isNilIdentity(identity) {
		return "unknown"
	}
	inner, prefix := identity, ""
	if s, ok := identity.(*ScopedUserIdentity); ok {
		if s == nil || isNilIdentity(s.UserIdentity) {
			return "unknown"
		}
		inner, prefix = s.UserIdentity, "scoped_"
	}
	k := string(principalContextForIdentity(inner).Kind)
	if k == "" {
		k = "unknown"
	}
	return prefix + k
}

// conduitOpenRefusedMetrics counts user stream opens refused before any
// grant is minted, and open grants minted unstamped under off (by cause).
type conduitOpenRefusedMetrics interface {
	RecordConduitOpenRefused(route, reason, cause string)
	RecordConduitOpenUnstamped(route, principalKind, cause string)
}

// OTelConduitOpenRefusedMetrics counts user stream opens refused before
// any grant is minted as scion.hub.conduit.stream_open_refused (attributes
// route, reason, cause), and open grants minted unstamped under off as
// scion.hub.conduit.stream_open_unstamped (attributes route,
// principal_kind, cause).
type OTelConduitOpenRefusedMetrics struct {
	refused   metric.Int64Counter
	unstamped metric.Int64Counter
}

// NewOTelConduitOpenRefusedMetrics creates the counters.
func NewOTelConduitOpenRefusedMetrics(mp metric.MeterProvider) (*OTelConduitOpenRefusedMetrics, error) {
	if mp == nil {
		return nil, errors.New("otel conduit open refused metrics: nil MeterProvider")
	}
	meter := mp.Meter(instrumentationScope)
	refused, err := meter.Int64Counter("scion.hub.conduit.stream_open_refused", metric.WithUnit("{open}"))
	if err != nil {
		return nil, err
	}
	unstamped, err := meter.Int64Counter("scion.hub.conduit.stream_open_unstamped", metric.WithUnit("{open}"))
	if err != nil {
		return nil, err
	}
	return &OTelConduitOpenRefusedMetrics{refused: refused, unstamped: unstamped}, nil
}

// RecordConduitOpenRefused implements conduitOpenRefusedMetrics.
func (m *OTelConduitOpenRefusedMetrics) RecordConduitOpenRefused(route, reason, cause string) {
	m.refused.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("route", route),
		attribute.String("reason", reason),
		attribute.String("cause", cause),
	))
}

// RecordConduitOpenUnstamped implements conduitOpenRefusedMetrics.
func (m *OTelConduitOpenRefusedMetrics) RecordConduitOpenUnstamped(route, principalKind, cause string) {
	m.unstamped.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("route", route),
		attribute.String("principal_kind", principalKind),
		attribute.String("cause", cause),
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

// conduitRouteRenewFunc decides whether a route delivers renewals.
type conduitRouteRenewFunc func(router.Resolved) bool
