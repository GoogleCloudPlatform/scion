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

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Outcomes of scion.hub.port_view.redirect: what a page navigation to a
// port link got with hub.port_view on.
const (
	// portViewRedirectMinted: answered with a new page link.
	portViewRedirectMinted = "minted"
	// portViewRedirectNotEligible: the request or its caller does not
	// qualify (not a user, or a sub-path the page view route would not take).
	portViewRedirectNotEligible = "not_eligible"
	// portViewRedirectIssuanceClosed: no key may sign a page link now.
	portViewRedirectIssuanceClosed = "issuance_closed"
	// portViewRedirectNoTransport: no transport would carry the page view
	// request now.
	portViewRedirectNoTransport = "no_transport"
	// portViewRedirectUserRefused: the user may not open the agent's ports.
	portViewRedirectUserRefused = "user_refused"
)

// Transports and outcomes of scion.hub.port_view.request: how the page
// view route answered. The transport attribute is set only once a
// transport is chosen: a request answered before that (outcome invalid)
// carries the outcome attribute alone, never a third transport value.
const (
	portViewTransportConduit = "conduit"
	portViewTransportLegacy  = "legacy"

	// portViewRequestServed: the agent's app answered, with any status.
	portViewRequestServed = "served"
	// portViewRequestInvalid: answered before any transport is chosen: the
	// page link failed or cannot be used, or names a port that is not
	// exposed.
	portViewRequestInvalid = "invalid"
	// portViewRequestOffline: the chosen transport could not get an answer
	// from the agent's app: no conduit session or tunnel, routing
	// unavailable, a busy tunnel, an internal error, or a round trip that
	// failed or timed out (502, 504).
	portViewRequestOffline = "offline"
	// portViewRequestRefused: the hub did not carry the request on the
	// chosen transport: an access or policy refusal (the open refused for
	// access, placement, an agent that must upgrade or a stream that
	// cannot stay authorized; the tunnel route refused for user requests;
	// a re-check that denied it), or a request the transport does not
	// carry (a WebSocket on the tunnel, 501; a body over the tunnel's
	// limit, 413; a request the conduit open rejects as invalid, 400).
	portViewRequestRefused = "refused"
)

// portViewConduitOpenOutcome is the request outcome of a conduit open
// that failed with err, other than for a missing session (which goes on
// to the tunnel), as writeConduitProxyError answers it.
func portViewConduitOpenOutcome(err error) string {
	var placement *PlacementConflictError
	switch {
	case errors.As(err, &placement),
		errors.Is(err, errConduitForbidden),
		errors.Is(err, errConduitStreamRenewalUnavailable),
		errors.Is(err, errConduitAgentUpgradeRequired),
		errors.Is(err, errConduitInvalid):
		return portViewRequestRefused
	default:
		return portViewRequestOffline
	}
}

// OTelPortViewMetrics counts page view outcomes as
// scion.hub.port_view.redirect (attribute outcome) and
// scion.hub.port_view.request (attributes transport and outcome). Every
// attribute value is one of the enumerated constants above: no link,
// user, agent, path or query is ever recorded.
type OTelPortViewMetrics struct {
	redirect metric.Int64Counter
	request  metric.Int64Counter
}

// NewOTelPortViewMetrics creates the counters.
func NewOTelPortViewMetrics(mp metric.MeterProvider) (*OTelPortViewMetrics, error) {
	if mp == nil {
		return nil, errors.New("otel port view metrics: nil MeterProvider")
	}
	meter := mp.Meter(instrumentationScope)
	redirect, err := meter.Int64Counter("scion.hub.port_view.redirect", metric.WithUnit("{navigation}"))
	if err != nil {
		return nil, err
	}
	request, err := meter.Int64Counter("scion.hub.port_view.request", metric.WithUnit("{request}"))
	if err != nil {
		return nil, err
	}
	return &OTelPortViewMetrics{redirect: redirect, request: request}, nil
}

// RecordPortViewRedirect counts one port link navigation with outcome.
func (m *OTelPortViewMetrics) RecordPortViewRedirect(outcome string) {
	m.redirect.Add(context.Background(), 1, metric.WithAttributes(attribute.String("outcome", outcome)))
}

// RecordPortViewRequest counts one page view request with outcome, over
// transport when one was chosen ("" records no transport attribute).
func (m *OTelPortViewMetrics) RecordPortViewRequest(transport, outcome string) {
	attrs := []attribute.KeyValue{attribute.String("outcome", outcome)}
	if transport != "" {
		attrs = append(attrs, attribute.String("transport", transport))
	}
	m.request.Add(context.Background(), 1, metric.WithAttributes(attrs...))
}

// SetPortViewMetrics wires the page view counters. Until it is set,
// outcomes are not counted.
func (s *Server) SetPortViewMetrics(m *OTelPortViewMetrics) {
	s.portViewMetrics.Store(m)
}

func (s *Server) recordPortViewRedirect(outcome string) {
	if m := s.portViewMetrics.Load(); m != nil {
		m.RecordPortViewRedirect(outcome)
	}
}

func (s *Server) recordPortViewRequest(transport, outcome string) {
	if outcome == "" {
		return
	}
	if m := s.portViewMetrics.Load(); m != nil {
		m.RecordPortViewRequest(transport, outcome)
	}
}
