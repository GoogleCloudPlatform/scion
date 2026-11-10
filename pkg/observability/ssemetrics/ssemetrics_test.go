/*
Copyright 2026 The Scion Authors.
*/

package ssemetrics

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestNewDisabledIsNoop(t *testing.T) {
	r := NewDisabled()
	if r == nil {
		t.Fatal("NewDisabled returned nil")
	}
	if r.Enabled() {
		t.Error("a recorder on the no-op provider must report Enabled()==false")
	}
	ctx := context.Background()
	r.RecordStreams(ctx, "view", 1)
	r.RecordUsers(ctx, 1)
	r.IncConnects(ctx, "view")
	r.RecordClose(ctx, "view", "client", time.Second)
	r.AddEventsWritten(ctx, "view", 1)
	r.AddBytesWritten(ctx, "view", 10)
	r.IncCapReached(ctx, "view")
	r.IncPublished(ctx, "agent.status", TriggerHeartbeat)
}

func collect(t *testing.T, reader *sdkmetric.ManualReader) map[string]metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	out := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name != instrumentationName {
			t.Errorf("instrument scope %q, want %q", sm.Scope.Name, instrumentationName)
		}
		for _, m := range sm.Metrics {
			out[m.Name] = m
		}
	}
	return out
}

func attrsOf(set attribute.Set) map[string]string {
	out := map[string]string{}
	for _, kv := range set.ToSlice() {
		out[string(kv.Key)] = kv.Value.AsString()
	}
	return out
}

func sameAttrs(got map[string]string, want map[string]string) bool {
	if len(got) != len(want) {
		return false
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

// int64Point returns the value of the single point of an Int64 sum or gauge
// with exactly the given attributes.
func int64Point(t *testing.T, m metricdata.Metrics, want map[string]string) int64 {
	t.Helper()
	var points []metricdata.DataPoint[int64]
	switch d := m.Data.(type) {
	case metricdata.Sum[int64]:
		points = d.DataPoints
	case metricdata.Gauge[int64]:
		points = d.DataPoints
	default:
		t.Fatalf("%s: unexpected data type %T", m.Name, m.Data)
	}
	for _, p := range points {
		if sameAttrs(attrsOf(p.Attributes), want) {
			return p.Value
		}
	}
	t.Fatalf("%s: no point with attributes %v", m.Name, want)
	return 0
}

func TestRecordedMetricsCarryDocumentedAttributes(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	r, err := New(mp)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !r.Enabled() {
		t.Error("a recorder on a real provider must report Enabled()==true")
	}
	ctx := context.Background()
	r.RecordStreams(ctx, "view", 3)
	r.RecordStreams(ctx, "view", 2) // a gauge keeps the last value
	r.RecordUsers(ctx, 2)
	r.IncConnects(ctx, "view")
	r.IncConnects(ctx, "view")
	r.RecordClose(ctx, "view", "max_age", 90*time.Second)
	r.AddEventsWritten(ctx, "view", 4)
	r.AddBytesWritten(ctx, "view", 100)
	r.IncCapReached(ctx, "agent-feed")
	r.IncPublished(ctx, "agent.status", TriggerHeartbeat)
	r.IncPublished(ctx, "agent.status", TriggerHeartbeat)
	r.IncPublished(ctx, "project.created", TriggerChange)

	got := collect(t, reader)
	view := map[string]string{AttrKind: "view"}
	cases := []struct {
		name  string
		attrs map[string]string
		want  int64
	}{
		{MetricStreams, view, 2},
		{MetricUsers, map[string]string{}, 2},
		{MetricConnects, view, 2},
		{MetricCloses, map[string]string{AttrKind: "view", AttrReason: "max_age"}, 1},
		{MetricEventsWritten, view, 4},
		{MetricBytesWritten, view, 100},
		{MetricCapReached, map[string]string{AttrKind: "agent-feed"}, 1},
		{MetricEventsPublished, map[string]string{AttrSubjectClass: "agent.status", AttrTrigger: TriggerHeartbeat}, 2},
		{MetricEventsPublished, map[string]string{AttrSubjectClass: "project.created", AttrTrigger: TriggerChange}, 1},
	}
	for _, c := range cases {
		m, ok := got[c.name]
		if !ok {
			t.Errorf("%s not exported", c.name)
			continue
		}
		if v := int64Point(t, m, c.attrs); v != c.want {
			t.Errorf("%s%v = %d, want %d", c.name, c.attrs, v, c.want)
		}
	}

	h, ok := got[MetricStreamDuration]
	if !ok {
		t.Fatalf("%s not exported", MetricStreamDuration)
	}
	hist, ok := h.Data.(metricdata.Histogram[float64])
	if !ok || len(hist.DataPoints) != 1 {
		t.Fatalf("%s: want one float64 histogram point, got %#v", MetricStreamDuration, h.Data)
	}
	p := hist.DataPoints[0]
	if !sameAttrs(attrsOf(p.Attributes), map[string]string{AttrKind: "view", AttrReason: "max_age"}) {
		t.Errorf("%s attributes = %v", MetricStreamDuration, attrsOf(p.Attributes))
	}
	if p.Count != 1 || p.Sum != 90 {
		t.Errorf("%s count=%d sum=%v, want 1 and 90 (seconds)", MetricStreamDuration, p.Count, p.Sum)
	}
	if len(p.Bounds) != len(durationBuckets) {
		t.Errorf("%s bounds = %v, want %v", MetricStreamDuration, p.Bounds, durationBuckets)
	}
	if h.Unit != "s" {
		t.Errorf("%s unit = %q, want s", MetricStreamDuration, h.Unit)
	}
}
