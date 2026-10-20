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
)

// OTelConduitBrokerRPCMetrics counts routed broker requests that used the
// fallback because of a hub-side fault, as
// scion.hub.conduit.broker_rpc_fallback (attributes reason:
// registry_unavailable or broker_unreadable, and operation).
type OTelConduitBrokerRPCMetrics struct {
	total metric.Int64Counter
}

// NewOTelConduitBrokerRPCMetrics creates the counter.
func NewOTelConduitBrokerRPCMetrics(mp metric.MeterProvider) (*OTelConduitBrokerRPCMetrics, error) {
	if mp == nil {
		return nil, errors.New("otel conduit broker rpc metrics: nil MeterProvider")
	}
	c, err := mp.Meter(instrumentationScope).Int64Counter("scion.hub.conduit.broker_rpc_fallback",
		metric.WithUnit("{request}"))
	if err != nil {
		return nil, err
	}
	return &OTelConduitBrokerRPCMetrics{total: c}, nil
}

// RecordBrokerRPCFallback counts one fallback.
func (m *OTelConduitBrokerRPCMetrics) RecordBrokerRPCFallback(reason, operation string) {
	m.total.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("reason", reason), attribute.String("operation", operation)))
}

// SetConduitBrokerRPCMetrics wires the scion.hub.conduit.broker_rpc_fallback
// counter. Until it is set, fallbacks are logged but not counted.
func (s *Server) SetConduitBrokerRPCMetrics(m *OTelConduitBrokerRPCMetrics) {
	s.conduitBrokerRPCMetrics.Store(m)
}

// recordBrokerRPCFallback logs and counts a routed broker request that used
// the fallback because of a hub-side fault.
func (s *Server) recordBrokerRPCFallback(brokerID, op, reason string, err error) {
	slog.Warn("Conduit broker request used the fallback path: hub-side fault",
		"broker_id", brokerID, "operation", op, "reason", reason, "error", err)
	if m := s.conduitBrokerRPCMetrics.Load(); m != nil {
		m.RecordBrokerRPCFallback(reason, op)
	}
}
