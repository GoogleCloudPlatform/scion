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

package logging

import (
	"context"
	"sort"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/GoogleCloudPlatform/scion/pkg/util/asyncwrite"
)

type fakeWriterSource struct {
	name    string
	depth   int64
	stalled bool
}

func (f *fakeWriterSource) Name() string      { return f.name }
func (f *fakeWriterSource) QueueDepth() int64 { return f.depth }
func (f *fakeWriterSource) Stalled() bool     { return f.stalled }

type point struct {
	attrs map[string]string
	value int64
}

func collectPoints(t *testing.T, reader *sdkmetric.ManualReader) map[string][]point {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string][]point{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			var dps []metricdata.DataPoint[int64]
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				dps = d.DataPoints
			case metricdata.Gauge[int64]:
				dps = d.DataPoints
			default:
				t.Fatalf("%s: unexpected data type %T", m.Name, m.Data)
			}
			for _, dp := range dps {
				attrs := map[string]string{}
				for _, kv := range dp.Attributes.ToSlice() {
					attrs[string(kv.Key)] = kv.Value.Emit()
				}
				out[m.Name] = append(out[m.Name], point{attrs: attrs, value: dp.Value})
			}
		}
	}
	return out
}

func attrKeys(p point) []string {
	keys := make([]string, 0, len(p.attrs))
	for k := range p.attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestWriteMetrics_ExportsDeclaredSeriesOnly(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	wm, err := NewWriteMetrics(mp)
	if err != nil {
		t.Fatal(err)
	}
	src := &fakeWriterSource{name: "audit", depth: 3, stalled: true}
	wm.Observe(src)
	for _, r := range []asyncwrite.Result{
		asyncwrite.ResultWritten, asyncwrite.ResultWritten, asyncwrite.ResultLateReturn,
		asyncwrite.ResultError, asyncwrite.ResultTimeout, asyncwrite.ResultQueueFull,
		asyncwrite.ResultOversize, asyncwrite.ResultUnsupported, asyncwrite.ResultClosed, asyncwrite.ResultShutdown,
	} {
		wm.Record("audit", r)
	}
	pts := collectPoints(t, reader)

	want := map[string]bool{MetricWriteFailures: true, MetricWriteRecords: true, MetricWriteLateReturns: true,
		MetricQueueDepth: true, MetricWriterStalled: true}
	for name := range pts {
		if !want[name] {
			t.Fatalf("unexpected metric %s", name)
		}
	}
	if got := pts[MetricWriteRecords]; len(got) != 1 || got[0].value != 2 || got[0].attrs["writer"] != "audit" || len(got[0].attrs) != 1 {
		t.Fatalf("records = %+v", got)
	}
	if got := pts[MetricWriteLateReturns]; len(got) != 1 || got[0].value != 1 || len(got[0].attrs) != 1 {
		t.Fatalf("late returns = %+v", got)
	}
	reasons := map[string]int64{}
	for _, p := range pts[MetricWriteFailures] {
		if k := attrKeys(p); len(k) != 2 || k[0] != "reason" || k[1] != "writer" {
			t.Fatalf("failure attrs = %v", k)
		}
		reasons[p.attrs["reason"]] = p.value
	}
	for _, r := range []string{"error", "timeout", "queue_full", "oversize", "unsupported", "closed", "shutdown"} {
		if reasons[r] != 1 {
			t.Fatalf("reason %s = %d (all: %v)", r, reasons[r], reasons)
		}
	}
	if len(reasons) != 7 {
		t.Fatalf("reasons = %v", reasons)
	}
	if got := pts[MetricQueueDepth]; len(got) != 1 || got[0].value != 3 || len(got[0].attrs) != 1 {
		t.Fatalf("depth = %+v", got)
	}
	if got := pts[MetricWriterStalled]; len(got) != 1 || got[0].value != 1 {
		t.Fatalf("stalled = %+v", got)
	}
	// The gauge is read at collection time: recovery shows without new records.
	src.stalled = false
	if got := collectPoints(t, reader)[MetricWriterStalled]; len(got) != 1 || got[0].value != 0 {
		t.Fatalf("stalled after recovery = %+v", got)
	}
}

func TestWriteMetrics_NilProviderAndNilReceiver(t *testing.T) {
	if _, err := NewWriteMetrics(nil); err == nil {
		t.Fatal("nil MeterProvider accepted")
	}
	var wm *WriteMetrics
	wm.Record("audit", asyncwrite.ResultError) // must not panic
	wm.Observe(&fakeWriterSource{})
}

// A real writer drives the recorder end to end.
func TestWriteMetrics_WithRealWriter(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	wm, err := NewWriteMetrics(mp)
	if err != nil {
		t.Fatal(err)
	}
	w, err := NewAsyncWriter(asyncwrite.Config{Name: "audit", AfterFunc: neverAfterFunc})
	if err != nil {
		t.Fatal(err)
	}
	w.SetRecorder(wm)
	wm.Observe(w)
	c := newCaptureInner(nil)
	h := NewAsyncHandler(c.handler(), w)
	if err := h.Handle(context.Background(), rec("scion.audit")); err != nil {
		t.Fatal(err)
	}
	<-c.out
	if err := w.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = h.Handle(context.Background(), rec("scion.audit")) // closed
	pts := collectPoints(t, reader)
	if got := pts[MetricWriteRecords]; len(got) != 1 || got[0].value != 1 {
		t.Fatalf("records = %+v", got)
	}
	if got := pts[MetricWriteFailures]; len(got) != 1 || got[0].attrs["reason"] != "closed" {
		t.Fatalf("failures = %+v", got)
	}
}
