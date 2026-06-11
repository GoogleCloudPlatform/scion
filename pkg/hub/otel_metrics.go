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

const instrumentationScope = "github.com/GoogleCloudPlatform/scion/pkg/hub"

// OTelMetricsRecorder implements MetricsRecorder using OTel instruments for
// Cloud Monitoring export. It embeds a BrokerAuthMetrics for the /api/metrics
// JSON snapshot endpoint (dual-write).
type OTelMetricsRecorder struct {
	authAttempts     metric.Int64Counter
	authSuccesses    metric.Int64Counter
	authFailures     metric.Int64Counter
	authDuration     metric.Float64Histogram
	registrations    metric.Int64Counter
	joins            metric.Int64Counter
	joinFailures     metric.Int64Counter
	rotations        metric.Int64Counter
	dispatchAttempts metric.Int64Counter
	dispatchFailures metric.Int64Counter
	connectedBrokers metric.Int64Gauge

	snap *BrokerAuthMetrics
}

var _ MetricsRecorder = (*OTelMetricsRecorder)(nil)

// NewOTelMetricsRecorder creates an OTel-backed MetricsRecorder. All
// instruments are registered under the hub instrumentation scope.
func NewOTelMetricsRecorder(mp metric.MeterProvider) (*OTelMetricsRecorder, error) {
	m := mp.Meter(instrumentationScope)
	r := &OTelMetricsRecorder{snap: NewBrokerAuthMetrics()}

	var err error

	if r.authAttempts, err = m.Int64Counter("scion.hub.auth.attempts",
		metric.WithUnit("{attempt}"),
	); err != nil {
		return nil, fmt.Errorf("creating auth.attempts counter: %w", err)
	}
	if r.authSuccesses, err = m.Int64Counter("scion.hub.auth.successes",
		metric.WithUnit("{attempt}"),
	); err != nil {
		return nil, fmt.Errorf("creating auth.successes counter: %w", err)
	}
	if r.authFailures, err = m.Int64Counter("scion.hub.auth.failures",
		metric.WithUnit("{attempt}"),
	); err != nil {
		return nil, fmt.Errorf("creating auth.failures counter: %w", err)
	}
	if r.authDuration, err = m.Float64Histogram("scion.hub.auth.duration",
		metric.WithUnit("ms"),
	); err != nil {
		return nil, fmt.Errorf("creating auth.duration histogram: %w", err)
	}
	if r.registrations, err = m.Int64Counter("scion.hub.registration.count",
		metric.WithUnit("{registration}"),
	); err != nil {
		return nil, fmt.Errorf("creating registration.count counter: %w", err)
	}
	if r.joins, err = m.Int64Counter("scion.hub.join.attempts",
		metric.WithUnit("{attempt}"),
	); err != nil {
		return nil, fmt.Errorf("creating join.attempts counter: %w", err)
	}
	if r.joinFailures, err = m.Int64Counter("scion.hub.join.failures",
		metric.WithUnit("{attempt}"),
	); err != nil {
		return nil, fmt.Errorf("creating join.failures counter: %w", err)
	}
	if r.rotations, err = m.Int64Counter("scion.hub.rotation.count",
		metric.WithUnit("{rotation}"),
	); err != nil {
		return nil, fmt.Errorf("creating rotation.count counter: %w", err)
	}
	if r.dispatchAttempts, err = m.Int64Counter("scion.hub.dispatch.attempts",
		metric.WithUnit("{attempt}"),
	); err != nil {
		return nil, fmt.Errorf("creating dispatch.attempts counter: %w", err)
	}
	if r.dispatchFailures, err = m.Int64Counter("scion.hub.dispatch.failures",
		metric.WithUnit("{attempt}"),
	); err != nil {
		return nil, fmt.Errorf("creating dispatch.failures counter: %w", err)
	}
	if r.connectedBrokers, err = m.Int64Gauge("scion.hub.brokers.connected",
		metric.WithUnit("{broker}"),
	); err != nil {
		return nil, fmt.Errorf("creating brokers.connected gauge: %w", err)
	}

	return r, nil
}

func (r *OTelMetricsRecorder) RecordAuthAttempt(brokerID string, success bool, latency time.Duration) {
	ctx := context.Background()
	attrs := metric.WithAttributes(attribute.String("broker_id", brokerID))
	r.authAttempts.Add(ctx, 1, attrs)
	if success {
		r.authSuccesses.Add(ctx, 1, attrs)
	} else {
		r.authFailures.Add(ctx, 1, attrs)
	}
	r.authDuration.Record(ctx, float64(latency.Milliseconds()), attrs)
	r.snap.RecordAuthAttempt(brokerID, success, latency)
}

func (r *OTelMetricsRecorder) RecordRegistration(brokerID string) {
	ctx := context.Background()
	attrs := metric.WithAttributes(attribute.String("broker_id", brokerID))
	r.registrations.Add(ctx, 1, attrs)
	r.snap.RecordRegistration(brokerID)
}

func (r *OTelMetricsRecorder) RecordJoin(brokerID string, success bool) {
	ctx := context.Background()
	attrs := metric.WithAttributes(attribute.String("broker_id", brokerID))
	r.joins.Add(ctx, 1, attrs)
	if !success {
		r.joinFailures.Add(ctx, 1, attrs)
	}
	r.snap.RecordJoin(brokerID, success)
}

func (r *OTelMetricsRecorder) RecordRotation(brokerID string) {
	ctx := context.Background()
	attrs := metric.WithAttributes(attribute.String("broker_id", brokerID))
	r.rotations.Add(ctx, 1, attrs)
	r.snap.RecordRotation(brokerID)
}

func (r *OTelMetricsRecorder) RecordDispatch(brokerID string, operation string, success bool, latency time.Duration) {
	ctx := context.Background()
	attrs := metric.WithAttributes(
		attribute.String("broker_id", brokerID),
		attribute.String("operation", operation),
	)
	r.dispatchAttempts.Add(ctx, 1, attrs)
	if !success {
		r.dispatchFailures.Add(ctx, 1, attrs)
	}
	r.snap.RecordDispatch(brokerID, operation, success, latency)
}

func (r *OTelMetricsRecorder) SetConnectedBrokers(count int64) {
	ctx := context.Background()
	r.connectedBrokers.Record(ctx, count)
	r.snap.SetConnectedBrokers(count)
}

func (r *OTelMetricsRecorder) GetSnapshot() *MetricsSnapshot {
	return r.snap.GetSnapshot()
}
