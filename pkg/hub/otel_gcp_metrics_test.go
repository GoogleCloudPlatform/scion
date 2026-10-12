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

var _ GCPTokenMetricsRecorder = (*OTelGCPTokenMetrics)(nil)

func newTestGCPRecorder(t *testing.T) (*OTelGCPTokenMetrics, *metric.ManualReader) {
	t.Helper()
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	rec, err := NewOTelGCPTokenMetrics(mp)
	if err != nil {
		t.Fatalf("NewOTelGCPTokenMetrics: %v", err)
	}
	return rec, reader
}

func collectGCPMetrics(t *testing.T, reader *metric.ManualReader) map[string]metricdata.Metrics {
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

func gcpSumCounter(m metricdata.Metrics) int64 {
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

func TestOTelGCPRecordAccessTokenRequest(t *testing.T) {
	rec, reader := newTestGCPRecorder(t)

	rec.RecordAccessTokenRequest(true, 30*time.Millisecond)
	rec.RecordAccessTokenRequest(false, 50*time.Millisecond)

	metrics := collectGCPMetrics(t, reader)

	if got := gcpSumCounter(metrics["scion.hub.gcp.token.access.requests"]); got != 2 {
		t.Errorf("access.requests = %d, want 2", got)
	}
	if got := gcpSumCounter(metrics["scion.hub.gcp.token.access.successes"]); got != 1 {
		t.Errorf("access.successes = %d, want 1", got)
	}
	if got := gcpSumCounter(metrics["scion.hub.gcp.token.access.failures"]); got != 1 {
		t.Errorf("access.failures = %d, want 1", got)
	}

	snap := rec.GetSnapshot()
	if snap.AccessTokenRequests != 2 {
		t.Errorf("snapshot AccessTokenRequests = %d, want 2", snap.AccessTokenRequests)
	}
	if snap.AccessTokenSuccesses != 1 {
		t.Errorf("snapshot AccessTokenSuccesses = %d, want 1", snap.AccessTokenSuccesses)
	}
	if snap.AccessTokenFailures != 1 {
		t.Errorf("snapshot AccessTokenFailures = %d, want 1", snap.AccessTokenFailures)
	}
}

func TestOTelGCPRecordIDTokenRequest(t *testing.T) {
	rec, reader := newTestGCPRecorder(t)

	rec.RecordIDTokenRequest(true, 20*time.Millisecond)
	rec.RecordIDTokenRequest(false, 40*time.Millisecond)

	metrics := collectGCPMetrics(t, reader)

	if got := gcpSumCounter(metrics["scion.hub.gcp.token.identity.requests"]); got != 2 {
		t.Errorf("identity.requests = %d, want 2", got)
	}
	if got := gcpSumCounter(metrics["scion.hub.gcp.token.identity.successes"]); got != 1 {
		t.Errorf("identity.successes = %d, want 1", got)
	}
	if got := gcpSumCounter(metrics["scion.hub.gcp.token.identity.failures"]); got != 1 {
		t.Errorf("identity.failures = %d, want 1", got)
	}

	snap := rec.GetSnapshot()
	if snap.IDTokenRequests != 2 {
		t.Errorf("snapshot IDTokenRequests = %d, want 2", snap.IDTokenRequests)
	}
	if snap.IDTokenSuccesses != 1 {
		t.Errorf("snapshot IDTokenSuccesses = %d, want 1", snap.IDTokenSuccesses)
	}
	if snap.IDTokenFailures != 1 {
		t.Errorf("snapshot IDTokenFailures = %d, want 1", snap.IDTokenFailures)
	}
}

func TestOTelGCPRecordRateLimitRejection(t *testing.T) {
	rec, reader := newTestGCPRecorder(t)

	rec.RecordRateLimitRejection()
	rec.RecordRateLimitRejection()

	metrics := collectGCPMetrics(t, reader)

	if got := gcpSumCounter(metrics["scion.hub.gcp.token.ratelimit.rejections"]); got != 2 {
		t.Errorf("ratelimit.rejections = %d, want 2", got)
	}

	snap := rec.GetSnapshot()
	if snap.RateLimitRejections != 2 {
		t.Errorf("snapshot RateLimitRejections = %d, want 2", snap.RateLimitRejections)
	}
}

func TestOTelGCPIAMDurationHistogram(t *testing.T) {
	rec, reader := newTestGCPRecorder(t)

	rec.RecordAccessTokenRequest(true, 42*time.Millisecond)

	metrics := collectGCPMetrics(t, reader)
	m, ok := metrics["scion.hub.gcp.iam.duration"]
	if !ok {
		t.Fatal("scion.hub.gcp.iam.duration not found")
	}
	hist, ok := m.Data.(metricdata.Histogram[float64])
	if !ok {
		t.Fatal("iam.duration is not a histogram")
	}
	if len(hist.DataPoints) == 0 {
		t.Fatal("histogram has no data points")
	}
	if hist.DataPoints[0].Sum <= 0 {
		t.Errorf("histogram sum = %f, want > 0", hist.DataPoints[0].Sum)
	}
}

func TestOTelGCPGetSnapshot(t *testing.T) {
	rec, _ := newTestGCPRecorder(t)

	rec.RecordAccessTokenRequest(true, 10*time.Millisecond)
	rec.RecordIDTokenRequest(false, 20*time.Millisecond)
	rec.RecordRateLimitRejection()

	snap := rec.GetSnapshot()
	if snap.AccessTokenRequests != 1 {
		t.Errorf("AccessTokenRequests = %d, want 1", snap.AccessTokenRequests)
	}
	if snap.IDTokenRequests != 1 {
		t.Errorf("IDTokenRequests = %d, want 1", snap.IDTokenRequests)
	}
	if snap.RateLimitRejections != 1 {
		t.Errorf("RateLimitRejections = %d, want 1", snap.RateLimitRejections)
	}
}
