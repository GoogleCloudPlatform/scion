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
	dropped       metric.Int64Counter
	writeDuration metric.Float64Histogram
	queueDepth    metric.Int64ObservableGauge
}

var _ DecisionAuditMetricsRecorder = (*OTelDecisionAuditMetrics)(nil)

// NewOTelDecisionAuditMetrics registers the decision audit writer
// instruments:
//   - scion.hub.decision_audit.dropped (counter; reason, decision). The
//     decision label is allow or deny; "unknown" is defensive only and is
//     not produced by the current record builder.
//   - scion.hub.decision_audit.write.duration (histogram, ms; outcome =
//     ok, duplicate or error), one sample per write attempt.
//   - scion.hub.decision_audit.queue_depth (observable gauge). queueDepth
//     is called at each collection, so the value is current even when the
//     hub is idle. Pass Server.DecisionAuditQueueDepth.
func NewOTelDecisionAuditMetrics(mp metric.MeterProvider, queueDepth func() int64) (*OTelDecisionAuditMetrics, error) {
	if mp == nil {
		return nil, fmt.Errorf("otel decision audit metrics: nil MeterProvider")
	}
	if queueDepth == nil {
		return nil, fmt.Errorf("otel decision audit metrics: nil queue depth source")
	}
	m := mp.Meter(instrumentationScope)
	r := &OTelDecisionAuditMetrics{}
	var err error
	if r.dropped, err = m.Int64Counter("scion.hub.decision_audit.dropped",
		metric.WithUnit("{record}"),
	); err != nil {
		return nil, fmt.Errorf("creating decision_audit.dropped counter: %w", err)
	}
	if r.writeDuration, err = m.Float64Histogram("scion.hub.decision_audit.write.duration",
		metric.WithUnit("ms"),
	); err != nil {
		return nil, fmt.Errorf("creating decision_audit.write.duration histogram: %w", err)
	}
	if r.queueDepth, err = m.Int64ObservableGauge("scion.hub.decision_audit.queue_depth",
		metric.WithUnit("{record}"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(queueDepth())
			return nil
		}),
	); err != nil {
		return nil, fmt.Errorf("creating decision_audit.queue_depth gauge: %w", err)
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

// RecordDecisionAuditWrite implements DecisionAuditMetricsRecorder.
func (r *OTelDecisionAuditMetrics) RecordDecisionAuditWrite(latency time.Duration, outcome DecisionAuditWriteOutcome) {
	r.writeDuration.Record(context.Background(), float64(latency)/float64(time.Millisecond),
		metric.WithAttributes(attribute.String("outcome", string(outcome))))
}
