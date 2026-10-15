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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Broker incarnation rules on the hub (design §3.4, broker incarnation
// reference; §3.9, session re-check). A broker session whose incarnation
// is not its broker's reference is closed with 4409 superseded_incarnation:
// on the admitting relay at admission (pkg/conduit/relay), and everywhere
// else by the session re-check, within one conduit.authz_recheck_interval.
// The in-process re-check closes this node's older-incarnation sessions of
// the broker; the re-check of a relay-held session answers close 4409.
// Every such close is counted with the broker's id, which is the
// cluster-wide signal; the flap report is per node.

// Where a broker session was closed with 4409 superseded_incarnation: the
// path attribute of scion.hub.conduit.broker_incarnation_superseded.
const (
	brokerSupersededAtAdmission = "admission"
	brokerSupersededAtRecheck   = "recheck"
)

// OTelConduitBrokerIncarnationMetrics counts broker sessions closed with
// 4409 superseded_incarnation, as
// scion.hub.conduit.broker_incarnation_superseded (attributes broker_id and
// path: admission or recheck), and broker incarnation flaps, as
// scion.hub.conduit.broker_incarnation_flap (attribute broker_id).
type OTelConduitBrokerIncarnationMetrics struct {
	superseded metric.Int64Counter
	flap       metric.Int64Counter
}

// NewOTelConduitBrokerIncarnationMetrics creates the counters.
func NewOTelConduitBrokerIncarnationMetrics(mp metric.MeterProvider) (*OTelConduitBrokerIncarnationMetrics, error) {
	if mp == nil {
		return nil, errors.New("otel conduit broker incarnation metrics: nil MeterProvider")
	}
	m := mp.Meter(instrumentationScope)
	superseded, err := m.Int64Counter("scion.hub.conduit.broker_incarnation_superseded",
		metric.WithUnit("{session}"))
	if err != nil {
		return nil, err
	}
	flap, err := m.Int64Counter("scion.hub.conduit.broker_incarnation_flap",
		metric.WithUnit("{event}"))
	if err != nil {
		return nil, err
	}
	return &OTelConduitBrokerIncarnationMetrics{superseded: superseded, flap: flap}, nil
}

// RecordBrokerSessionsSuperseded counts closed sessions of brokerID.
func (m *OTelConduitBrokerIncarnationMetrics) RecordBrokerSessionsSuperseded(brokerID, path string, closed int) {
	m.superseded.Add(context.Background(), int64(closed), metric.WithAttributes(
		attribute.String("broker_id", brokerID), attribute.String("path", path)))
}

// RecordBrokerIncarnationFlap counts one flap of brokerID.
func (m *OTelConduitBrokerIncarnationMetrics) RecordBrokerIncarnationFlap(brokerID string) {
	m.flap.Add(context.Background(), 1, metric.WithAttributes(attribute.String("broker_id", brokerID)))
}

// SetConduitBrokerIncarnationMetrics wires the broker incarnation counters.
// Until it is set, closes and flaps are logged but not counted.
func (s *Server) SetConduitBrokerIncarnationMetrics(m *OTelConduitBrokerIncarnationMetrics) {
	s.conduitBrokerIncarnationMetrics.Store(m)
}

// recordBrokerSessionsSuperseded counts closed sessions of brokerID closed
// with 4409 superseded_incarnation at path.
func (s *Server) recordBrokerSessionsSuperseded(brokerID, path string, closed int) {
	if closed <= 0 {
		return
	}
	if m := s.conduitBrokerIncarnationMetrics.Load(); m != nil {
		m.RecordBrokerSessionsSuperseded(brokerID, path, closed)
	}
}

// recordBrokerIncarnationFlap counts a broker incarnation flap the relay
// reported (the relay logs it).
func (s *Server) recordBrokerIncarnationFlap(brokerID, _ string) {
	if m := s.conduitBrokerIncarnationMetrics.Load(); m != nil {
		m.RecordBrokerIncarnationFlap(brokerID)
	}
}

// closeSupersededBrokerSessions is the in-process session re-check's
// incarnation step for brokerID: it reads the broker's reference session
// and closes this node's sessions of the broker with another incarnation
// and a lower epoch with 4409. A read error closes nothing; the next
// re-check tries again.
func (s *Server) closeSupersededBrokerSessions(ctx context.Context, brokerID string) {
	rt := s.conduit.Load()
	if rt == nil {
		return
	}
	ref, ok, err := rt.registry.BrokerReference(ctx, brokerID, rt.now())
	if err != nil {
		slog.Warn("Conduit: reading the broker incarnation reference failed; closing nothing", "broker_id", brokerID, "error", err)
		return
	}
	if !ok {
		return
	}
	n := rt.relay.CloseOlderIncarnationSessions(registry.PrincipalBroker, brokerID, ref.Incarnation, ref.ConnectionEpoch,
		relay.CloseSupersededIncarnation, relay.SupersededIncarnationReason())
	if n == 0 {
		return
	}
	slog.Info("Conduit: session re-check closed broker sessions of an older incarnation (4409 superseded_incarnation)",
		"broker_id", brokerID, "incarnation", ref.Incarnation, "closed", n)
	s.recordBrokerSessionsSuperseded(brokerID, brokerSupersededAtRecheck, n)
}

// relayBrokerSessionSuperseded reports whether the relay-held broker
// session c was admitted for belongs to an older incarnation of its broker
// (registry.BrokerSessionSuperseded), and counts it when it does. A read
// error reports false: the session stays open and the next re-check tries
// again.
func (s *Server) relayBrokerSessionSuperseded(ctx context.Context, reg *registry.Registry, now time.Time, c conduitAdmissionClaims) bool {
	superseded, err := reg.BrokerSessionSuperseded(ctx, c.ID, c.SessionID, now)
	if err != nil {
		slog.Warn("Conduit: reading the broker incarnation reference for a relay-held session failed; keeping it",
			"broker_id", c.ID, "session_id", c.SessionID, "error", err)
		return false
	}
	if superseded {
		s.recordBrokerSessionsSuperseded(c.ID, brokerSupersededAtRecheck, 1)
	}
	return superseded
}
