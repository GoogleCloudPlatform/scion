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
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// OTelDecisionAuditMetrics implements DecisionAuditMetricsRecorder with OTel
// instruments under the shared hub instrumentationScope, following
// OTelMetricsRecorder's registration convention.
type OTelDecisionAuditMetrics struct {
	dropped      metric.Int64Counter
	queueDepth   metric.Int64Gauge
	writeLatency metric.Float64Histogram
}

var _ DecisionAuditMetricsRecorder = (*OTelDecisionAuditMetrics)(nil)

// NewOTelDecisionAuditMetrics registers the decision audit writer
// instruments: scion.hub.decision_audit.dropped (counter; reason, decision),
// scion.hub.decision_audit.queue_depth (gauge) and
// scion.hub.decision_audit.write_latency (histogram, ms; outcome).
func NewOTelDecisionAuditMetrics(mp metric.MeterProvider) (*OTelDecisionAuditMetrics, error) {
	if mp == nil {
		return nil, fmt.Errorf("otel decision audit metrics: nil MeterProvider")
	}
	m := mp.Meter(instrumentationScope)
	r := &OTelDecisionAuditMetrics{}
	var err error
	if r.dropped, err = m.Int64Counter("scion.hub.decision_audit.dropped",
		metric.WithUnit("{record}"),
	); err != nil {
		return nil, fmt.Errorf("creating decision_audit.dropped counter: %w", err)
	}
	if r.queueDepth, err = m.Int64Gauge("scion.hub.decision_audit.queue_depth",
		metric.WithUnit("{record}"),
	); err != nil {
		return nil, fmt.Errorf("creating decision_audit.queue_depth gauge: %w", err)
	}
	if r.writeLatency, err = m.Float64Histogram("scion.hub.decision_audit.write_latency",
		metric.WithUnit("ms"),
	); err != nil {
		return nil, fmt.Errorf("creating decision_audit.write_latency histogram: %w", err)
	}
	return r, nil
}

// RecordDecisionAuditDrop implements DecisionAuditMetricsRecorder.
func (r *OTelDecisionAuditMetrics) RecordDecisionAuditDrop(reason DecisionAuditDropReason, decision string) {
	r.dropped.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("reason", string(reason)),
		attribute.String("decision", decision),
	))
}

// SetDecisionAuditQueueDepth implements DecisionAuditMetricsRecorder.
func (r *OTelDecisionAuditMetrics) SetDecisionAuditQueueDepth(depth int64) {
	r.queueDepth.Record(context.Background(), depth)
}

// RecordDecisionAuditWrite implements DecisionAuditMetricsRecorder.
func (r *OTelDecisionAuditMetrics) RecordDecisionAuditWrite(latency time.Duration, success bool) {
	outcome := "ok"
	if !success {
		outcome = "error"
	}
	r.writeLatency.Record(context.Background(), float64(latency)/float64(time.Millisecond),
		metric.WithAttributes(attribute.String("outcome", outcome)))
}
