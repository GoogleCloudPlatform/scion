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
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

var _ MetricsRecorder = (*OTelMetricsRecorder)(nil)

func newTestRecorder(t *testing.T) (*OTelMetricsRecorder, *metric.ManualReader) {
	t.Helper()
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	rec, err := NewOTelMetricsRecorder(mp)
	if err != nil {
		t.Fatalf("NewOTelMetricsRecorder: %v", err)
	}
	return rec, reader
}

func collectMetrics(t *testing.T, reader *metric.ManualReader) map[string]metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collecting metrics: %v", err)
	}
	result := make(map[string]metricdata.Metrics)
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			result[m.Name] = m
		}
	}
	return result
}

func sumCounter(m metricdata.Metrics) int64 {
	sum, ok := m.Data.(metricdata.Sum[int64])
	if !ok {
		return 0
	}
	var total int64
	for _, dp := range sum.DataPoints {
		total += dp.Value
	}
	return total
}

func TestOTelRecordAuthAttempt(t *testing.T) {
	rec, reader := newTestRecorder(t)

	rec.RecordAuthAttempt("broker-1", true, 50*time.Millisecond)
	rec.RecordAuthAttempt("broker-1", false, 100*time.Millisecond)

	metrics := collectMetrics(t, reader)

	if got := sumCounter(metrics["scion.hub.auth.attempts"]); got != 2 {
		t.Errorf("auth.attempts = %d, want 2", got)
	}
	if got := sumCounter(metrics["scion.hub.auth.successes"]); got != 1 {
		t.Errorf("auth.successes = %d, want 1", got)
	}
	if got := sumCounter(metrics["scion.hub.auth.failures"]); got != 1 {
		t.Errorf("auth.failures = %d, want 1", got)
	}

	snap := rec.GetSnapshot()
	if snap.AuthAttempts != 2 {
		t.Errorf("snapshot AuthAttempts = %d, want 2", snap.AuthAttempts)
	}
	if snap.AuthSuccesses != 1 {
		t.Errorf("snapshot AuthSuccesses = %d, want 1", snap.AuthSuccesses)
	}
	if snap.AuthFailures != 1 {
		t.Errorf("snapshot AuthFailures = %d, want 1", snap.AuthFailures)
	}
}

func TestOTelAuthDurationHistogram(t *testing.T) {
	rec, reader := newTestRecorder(t)

	rec.RecordAuthAttempt("broker-1", true, 42*time.Millisecond)

	metrics := collectMetrics(t, reader)
	m, ok := metrics["scion.hub.auth.duration"]
	if !ok {
		t.Fatal("scion.hub.auth.duration not found")
	}
	hist, ok := m.Data.(metricdata.Histogram[float64])
	if !ok {
		t.Fatal("auth.duration is not a histogram")
	}
	if len(hist.DataPoints) == 0 {
		t.Fatal("histogram has no data points")
	}
	if hist.DataPoints[0].Sum <= 0 {
		t.Errorf("histogram sum = %f, want > 0", hist.DataPoints[0].Sum)
	}
}

func TestOTelRecordRegistration(t *testing.T) {
	rec, reader := newTestRecorder(t)

	rec.RecordRegistration("broker-1")
	rec.RecordRegistration("broker-2")

	metrics := collectMetrics(t, reader)
	if got := sumCounter(metrics["scion.hub.registration.count"]); got != 2 {
		t.Errorf("registration.count = %d, want 2", got)
	}

	snap := rec.GetSnapshot()
	if snap.Registrations != 2 {
		t.Errorf("snapshot Registrations = %d, want 2", snap.Registrations)
	}
}

func TestOTelRecordJoin(t *testing.T) {
	rec, reader := newTestRecorder(t)

	rec.RecordJoin("broker-1", true)
	rec.RecordJoin("broker-2", false)

	metrics := collectMetrics(t, reader)
	if got := sumCounter(metrics["scion.hub.join.attempts"]); got != 2 {
		t.Errorf("join.attempts = %d, want 2", got)
	}
	if got := sumCounter(metrics["scion.hub.join.failures"]); got != 1 {
		t.Errorf("join.failures = %d, want 1", got)
	}

	snap := rec.GetSnapshot()
	if snap.Joins != 2 {
		t.Errorf("snapshot Joins = %d, want 2", snap.Joins)
	}
	if snap.JoinFailures != 1 {
		t.Errorf("snapshot JoinFailures = %d, want 1", snap.JoinFailures)
	}
}

func TestOTelRecordRotation(t *testing.T) {
	rec, reader := newTestRecorder(t)

	rec.RecordRotation("broker-1")

	metrics := collectMetrics(t, reader)
	if got := sumCounter(metrics["scion.hub.rotation.count"]); got != 1 {
		t.Errorf("rotation.count = %d, want 1", got)
	}

	snap := rec.GetSnapshot()
	if snap.Rotations != 1 {
		t.Errorf("snapshot Rotations = %d, want 1", snap.Rotations)
	}
}

func TestOTelRecordDispatch(t *testing.T) {
	rec, reader := newTestRecorder(t)

	rec.RecordDispatch("broker-1", "create", true, 10*time.Millisecond)
	rec.RecordDispatch("broker-1", "create", false, 20*time.Millisecond)

	metrics := collectMetrics(t, reader)
	if got := sumCounter(metrics["scion.hub.dispatch.attempts"]); got != 2 {
		t.Errorf("dispatch.attempts = %d, want 2", got)
	}
	if got := sumCounter(metrics["scion.hub.dispatch.failures"]); got != 1 {
		t.Errorf("dispatch.failures = %d, want 1", got)
	}

	snap := rec.GetSnapshot()
	if snap.DispatchAttempts != 2 {
		t.Errorf("snapshot DispatchAttempts = %d, want 2", snap.DispatchAttempts)
	}
	if snap.DispatchFailures != 1 {
		t.Errorf("snapshot DispatchFailures = %d, want 1", snap.DispatchFailures)
	}
}

func TestOTelSetConnectedBrokers(t *testing.T) {
	rec, reader := newTestRecorder(t)

	rec.SetConnectedBrokers(5)

	metrics := collectMetrics(t, reader)
	m, ok := metrics["scion.hub.brokers.connected"]
	if !ok {
		t.Fatal("scion.hub.brokers.connected not found")
	}
	gauge, ok := m.Data.(metricdata.Gauge[int64])
	if !ok {
		t.Fatal("brokers.connected is not a gauge")
	}
	if len(gauge.DataPoints) == 0 {
		t.Fatal("gauge has no data points")
	}
	if gauge.DataPoints[0].Value != 5 {
		t.Errorf("gauge value = %d, want 5", gauge.DataPoints[0].Value)
	}

	snap := rec.GetSnapshot()
	if snap.ConnectedBrokers != 5 {
		t.Errorf("snapshot ConnectedBrokers = %d, want 5", snap.ConnectedBrokers)
	}
}
