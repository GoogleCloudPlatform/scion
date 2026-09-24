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

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func newTestExternalBearerRecorder(t *testing.T) (*OTelExternalBearerMetrics, *ExternalBearerSnapshotMetrics, *metric.ManualReader) {
	t.Helper()
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	snap := NewExternalBearerSnapshotMetrics()
	rec, err := NewOTelExternalBearerMetrics(mp, snap)
	if err != nil {
		t.Fatalf("NewOTelExternalBearerMetrics: %v", err)
	}
	return rec, snap, reader
}

func collectExternalBearerMetrics(t *testing.T, reader *metric.ManualReader) map[string]metricdata.Metrics {
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

func externalBearerSumDataPoints(t *testing.T, m metricdata.Metrics) []metricdata.DataPoint[int64] {
	t.Helper()
	sum, ok := m.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("%s: data is %T, want metricdata.Sum[int64]", m.Name, m.Data)
	}
	return sum.DataPoints
}

// wantAttrs asserts that attrs contains exactly the given key/value pairs —
// no more, no fewer — so a mutation that renames, drops or adds a label key
// fails this check.
func wantAttrs(t *testing.T, attrs attribute.Set, want map[string]string) {
	t.Helper()
	if got := attrs.Len(); got != len(want) {
		t.Errorf("attribute count = %d, want %d (set: %s)", got, len(want), attrs.Encoded(attribute.DefaultEncoder()))
	}
	for k, wantV := range want {
		gotV, ok := attrs.Value(attribute.Key(k))
		if !ok {
			t.Errorf("attribute %q missing from set %s", k, attrs.Encoded(attribute.DefaultEncoder()))
			continue
		}
		if gotV.AsString() != wantV {
			t.Errorf("attribute %q = %q, want %q", k, gotV.AsString(), wantV)
		}
	}
}

// TestOTelExternalBearer_RecordExternalBearer pins the instrument name, unit,
// attribute key set/values and sum for scion.hub.external_bearer, and proves
// the dual-write into the shared in-process snapshot.
func TestOTelExternalBearer_RecordExternalBearer(t *testing.T) {
	rec, snap, reader := newTestExternalBearerRecorder(t)

	rec.RecordExternalBearer(ExternalBearerKindIDToken, ExternalBearerPrincipalUser, ExternalBearerOutcomeOK)

	metrics := collectExternalBearerMetrics(t, reader)
	m, ok := metrics["scion.hub.external_bearer"]
	if !ok {
		t.Fatal("instrument scion.hub.external_bearer not found")
	}
	if m.Unit != "{request}" {
		t.Errorf("unit = %q, want {request}", m.Unit)
	}
	dps := externalBearerSumDataPoints(t, m)
	if len(dps) != 1 {
		t.Fatalf("data points = %d, want 1", len(dps))
	}
	if dps[0].Value != 1 {
		t.Errorf("value = %d, want 1", dps[0].Value)
	}
	wantAttrs(t, dps[0].Attributes, map[string]string{
		"kind": "id_token", "principal": "user", "outcome": "ok",
	})

	snapshot := snap.GetSnapshot()
	if got := snapshot.ExternalBearerTotal[string(ExternalBearerOutcomeOK)]; got != 1 {
		t.Errorf("dual-written snapshot ok count = %d, want 1", got)
	}
}

// TestOTelExternalBearer_RecordGoogleValidatorCache pins the instrument name,
// unit, attribute key/value and sum for scion.hub.google_validator_cache.
func TestOTelExternalBearer_RecordGoogleValidatorCache(t *testing.T) {
	rec, snap, reader := newTestExternalBearerRecorder(t)

	rec.RecordGoogleValidatorCache(GoogleValidatorCacheHit)
	rec.RecordGoogleValidatorCache(GoogleValidatorCacheHit)

	metrics := collectExternalBearerMetrics(t, reader)
	m, ok := metrics["scion.hub.google_validator_cache"]
	if !ok {
		t.Fatal("instrument scion.hub.google_validator_cache not found")
	}
	if m.Unit != "{lookup}" {
		t.Errorf("unit = %q, want {lookup}", m.Unit)
	}
	dps := externalBearerSumDataPoints(t, m)
	if len(dps) != 1 {
		t.Fatalf("data points = %d, want 1", len(dps))
	}
	if dps[0].Value != 2 {
		t.Errorf("value = %d, want 2", dps[0].Value)
	}
	wantAttrs(t, dps[0].Attributes, map[string]string{"result": "hit"})

	snapshot := snap.GetSnapshot()
	if got := snapshot.GoogleValidatorCacheTotal[string(GoogleValidatorCacheHit)]; got != 2 {
		t.Errorf("dual-written snapshot hit count = %d, want 2", got)
	}
}

// TestOTelExternalBearer_RecordGEExchangeRequest pins the instrument name,
// unit, attribute key/value and sum for scion.hub.ge_exchange.requests.
func TestOTelExternalBearer_RecordGEExchangeRequest(t *testing.T) {
	rec, snap, reader := newTestExternalBearerRecorder(t)

	rec.RecordGEExchangeRequest(GEExchangeOutcomeForbidden)

	metrics := collectExternalBearerMetrics(t, reader)
	m, ok := metrics["scion.hub.ge_exchange.requests"]
	if !ok {
		t.Fatal("instrument scion.hub.ge_exchange.requests not found")
	}
	if m.Unit != "{request}" {
		t.Errorf("unit = %q, want {request}", m.Unit)
	}
	dps := externalBearerSumDataPoints(t, m)
	if len(dps) != 1 {
		t.Fatalf("data points = %d, want 1", len(dps))
	}
	if dps[0].Value != 1 {
		t.Errorf("value = %d, want 1", dps[0].Value)
	}
	wantAttrs(t, dps[0].Attributes, map[string]string{"outcome": "forbidden"})

	snapshot := snap.GetSnapshot()
	if got := snapshot.GEExchangeRequestsTotal[string(GEExchangeOutcomeForbidden)]; got != 1 {
		t.Errorf("dual-written snapshot forbidden count = %d, want 1", got)
	}
}

// TestOTelExternalBearer_NilSnapshotDoesNotPanic proves the dual-write is
// nil-safe: NewOTelExternalBearerMetrics(mp, nil) is a valid call (no shared
// snapshot to dual-write into).
func TestOTelExternalBearer_NilSnapshotDoesNotPanic(t *testing.T) {
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	rec, err := NewOTelExternalBearerMetrics(mp, nil)
	if err != nil {
		t.Fatalf("NewOTelExternalBearerMetrics: %v", err)
	}
	rec.RecordExternalBearer(ExternalBearerKindAccessToken, ExternalBearerPrincipalServiceAccount, ExternalBearerOutcomeRejected)
	rec.RecordGoogleValidatorCache(GoogleValidatorCacheNegativeHit)
	rec.RecordGEExchangeRequest(GEExchangeOutcomeBadRequest)
}
