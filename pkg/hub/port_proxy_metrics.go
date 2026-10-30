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

	"go.opentelemetry.io/otel/metric"
)

// portProxyMetrics records agent port proxy events.
type portProxyMetrics interface {
	RecordPortProxyUpstreamTimeout()
	RecordPortProxyNoCredentials()
}

// OTelPortProxyMetrics counts agent port proxy events:
//   - scion.hub.port_proxy.upstream_timeout: requests whose upstream did
//     not send its response headers within the bound;
//   - scion.hub.port_proxy.sandboxed_subresource_unauth: page sub-requests
//     to a proxy route that arrived with no credential and got the
//     plain-text 401 (writePortProxyNoCredentials). It has no attributes,
//     so its cardinality stays fixed; the agent is in the debug log.
type OTelPortProxyMetrics struct {
	upstreamTimeout metric.Int64Counter
	noCredentials   metric.Int64Counter
}

// NewOTelPortProxyMetrics creates the counters.
func NewOTelPortProxyMetrics(mp metric.MeterProvider) (*OTelPortProxyMetrics, error) {
	if mp == nil {
		return nil, errors.New("otel port proxy metrics: nil MeterProvider")
	}
	meter := mp.Meter(instrumentationScope)
	timeout, err := meter.Int64Counter("scion.hub.port_proxy.upstream_timeout",
		metric.WithUnit("{request}"))
	if err != nil {
		return nil, err
	}
	noCreds, err := meter.Int64Counter("scion.hub.port_proxy.sandboxed_subresource_unauth",
		metric.WithUnit("{request}"))
	if err != nil {
		return nil, err
	}
	return &OTelPortProxyMetrics{upstreamTimeout: timeout, noCredentials: noCreds}, nil
}

// RecordPortProxyUpstreamTimeout implements portProxyMetrics.
func (m *OTelPortProxyMetrics) RecordPortProxyUpstreamTimeout() {
	m.upstreamTimeout.Add(context.Background(), 1)
}

// RecordPortProxyNoCredentials implements portProxyMetrics.
func (m *OTelPortProxyMetrics) RecordPortProxyNoCredentials() {
	m.noCredentials.Add(context.Background(), 1)
}

// SetPortProxyMetrics wires the port proxy counters.
func (s *Server) SetPortProxyMetrics(m *OTelPortProxyMetrics) {
	if m == nil {
		return
	}
	var r portProxyMetrics = m
	s.portProxyMetrics.Store(&r)
}

// recordPortProxyUpstreamTimeout counts one upstream header timeout on
// the counter set by SetPortProxyMetrics, if any.
func (s *Server) recordPortProxyUpstreamTimeout() {
	if r := s.portProxyMetrics.Load(); r != nil {
		(*r).RecordPortProxyUpstreamTimeout()
	}
}

// recordPortProxyNoCredentials counts one plain-text no-credentials 401
// on the counter set by SetPortProxyMetrics, if any.
func (s *Server) recordPortProxyNoCredentials() {
	if r := s.portProxyMetrics.Load(); r != nil {
		(*r).RecordPortProxyNoCredentials()
	}
}
