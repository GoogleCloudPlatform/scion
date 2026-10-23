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

package eventstreams

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/GoogleCloudPlatform/scion/pkg/observability/ssemetrics"
)

// logCapture is a JSON slog logger whose records a test can read back.
type logCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *logCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *logCapture) logger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(c, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

func (c *logCapture) records(t *testing.T) []map[string]any {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(c.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

func newMeteredRegistry(t *testing.T) (*Registry, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	rec, err := ssemetrics.New(mp)
	if err != nil {
		t.Fatalf("ssemetrics.New: %v", err)
	}
	r := &Registry{}
	r.SetMetrics(rec)
	return r, reader
}

// int64Points returns every point of the named Int64 sum or gauge, keyed by
// its attributes rendered as k=v pairs joined with ",", sorted by key.
func int64Points(t *testing.T, reader *sdkmetric.ManualReader, name string) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			var pts []metricdata.DataPoint[int64]
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				pts = d.DataPoints
			case metricdata.Gauge[int64]:
				pts = d.DataPoints
			default:
				t.Fatalf("%s: unexpected data %T", name, m.Data)
			}
			for _, p := range pts {
				out[attrKey(p.Attributes)] = p.Value
			}
		}
	}
	return out
}

func attrKey(set attribute.Set) string {
	var parts []string
	for _, kv := range set.ToSlice() {
		parts = append(parts, string(kv.Key)+"="+kv.Value.AsString())
	}
	return strings.Join(parts, ",")
}

func TestParseKind(t *testing.T) {
	for in, want := range map[string]Kind{
		"view":              KindView,
		"agent-feed":        KindAgentFeed,
		"terminal-metadata": KindTerminalMetadata,
		"":                  KindOther,
		"other":             KindOther,
		"VIEW":              KindOther,
		"pty":               KindOther,
	} {
		if got := ParseKind(in); got != want {
			t.Errorf("ParseKind(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeClientID(t *testing.T) {
	long := strings.Repeat("a", MaxClientIDLen)
	for in, want := range map[string]string{
		"tab-1_A9":  "tab-1_A9",
		long:        long,
		long + "a":  "",
		"":          "",
		"a b":       "",
		"a\nb":      "",
		"a.b":       "",
		"ü":         "",
		"x=1&y=2":   "",
		"<script>":  "",
		"0123-abcd": "0123-abcd",
	} {
		if got := SanitizeClientID(in); got != want {
			t.Errorf("SanitizeClientID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNewConnIDIsUniqueHex(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := NewConnID()
		if len(id) != 16 || SanitizeClientID(id) != id {
			t.Fatalf("NewConnID() = %q, want 16 hex digits", id)
		}
		if seen[id] {
			t.Fatalf("NewConnID() repeated %q", id)
		}
		seen[id] = true
	}
}

func TestKeyHasClientIDs(t *testing.T) {
	if (Key{ConnID: "c"}).HasClientIDs() {
		t.Error("a key without client ids reports HasClientIDs")
	}
	if (Key{BrowserID: "b", ConnID: "c"}).HasClientIDs() {
		t.Error("a key with only a browser id reports HasClientIDs")
	}
	if !(Key{BrowserID: "b", TabID: "t", ConnID: "c"}).HasClientIDs() {
		t.Error("a key with browser and tab ids does not report HasClientIDs")
	}
}

func TestRegistryZeroValueCountsWithoutMetrics(t *testing.T) {
	var r Registry
	s := r.Register(Key{UserID: "u1", ConnID: "c1"}, 2)
	if s.Key().Kind != KindOther {
		t.Errorf("empty kind registered as %q, want %q", s.Key().Kind, KindOther)
	}
	if r.Count() != 1 || r.Users() != 1 {
		t.Fatalf("Count=%d Users=%d, want 1 and 1", r.Count(), r.Users())
	}
	s.RecordEvent(10)
	s.RecordBytes(5)
	s.Close(CloseClient, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	if r.Count() != 0 || r.Users() != 0 || len(r.CountByKind()) != 0 {
		t.Fatalf("after close Count=%d Users=%d kinds=%v, want all zero", r.Count(), r.Users(), r.CountByKind())
	}
}

func TestRegistryTracksStreamsAndLogsOneLinePerClose(t *testing.T) {
	r, reader := newMeteredRegistry(t)
	base := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	now := base
	r.clock = func() time.Time { return now }
	logs := &logCapture{}
	lg := logs.logger()

	a1 := r.Register(Key{UserID: "alice", BrowserID: "b1", TabID: "t1", Kind: KindView, ConnID: "c1"}, 3)
	now = now.Add(time.Second)
	a2 := r.Register(Key{UserID: "alice", Kind: KindAgentFeed, ConnID: "c2"}, 1)
	now = now.Add(time.Second)
	b1 := r.Register(Key{UserID: "bob", Kind: KindView, ConnID: "c3"}, 2)

	if got := r.Count(); got != 3 {
		t.Fatalf("Count = %d, want 3", got)
	}
	if got := r.Users(); got != 2 {
		t.Fatalf("Users = %d, want 2", got)
	}
	if got := r.CountByKind(); got[KindView] != 2 || got[KindAgentFeed] != 1 || len(got) != 2 {
		t.Fatalf("CountByKind = %v", got)
	}
	if got := int64Points(t, reader, ssemetrics.MetricStreams); got["kind=view"] != 2 || got["kind=agent-feed"] != 1 {
		t.Errorf("streams gauge = %v", got)
	}
	if got := int64Points(t, reader, ssemetrics.MetricUsers); got[""] != 2 {
		t.Errorf("users gauge = %v", got)
	}
	if got := int64Points(t, reader, ssemetrics.MetricConnects); got["kind=view"] != 2 || got["kind=agent-feed"] != 1 {
		t.Errorf("connects = %v", got)
	}

	snap := r.Snapshot()
	if len(snap) != 3 || snap[0].Key.ConnID != "c1" || snap[1].Key.ConnID != "c2" || snap[2].Key.ConnID != "c3" {
		t.Fatalf("Snapshot not oldest first: %+v", snap)
	}
	if us := r.UserStreams("alice"); len(us) != 2 || us[0].Key.ConnID != "c1" || us[1].Key.ConnID != "c2" {
		t.Fatalf("UserStreams(alice) = %+v", us)
	}

	a1.RecordEvent(100)
	a1.RecordEvent(50)
	a1.RecordBytes(20)
	if info := a1.Info(); info.EventsWritten != 2 || info.BytesWritten != 170 || info.Subjects != 3 {
		t.Errorf("a1 info = %+v", info)
	}

	now = base.Add(10 * time.Second)
	a1.Close(CloseMaxAge, lg)
	a1.Close(CloseClient, lg) // a second close does nothing
	a2.Close(CloseClient, lg)
	b1.Close(ClosePublisher, lg)

	if r.Count() != 0 || r.Users() != 0 || len(r.CountByKind()) != 0 || len(r.Snapshot()) != 0 {
		t.Fatalf("registry not empty after closes: count=%d users=%d", r.Count(), r.Users())
	}
	if got := int64Points(t, reader, ssemetrics.MetricStreams); got["kind=view"] != 0 || got["kind=agent-feed"] != 0 {
		t.Errorf("streams gauge after closes = %v", got)
	}
	if got := int64Points(t, reader, ssemetrics.MetricUsers); got[""] != 0 {
		t.Errorf("users gauge after closes = %v", got)
	}
	closes := int64Points(t, reader, ssemetrics.MetricCloses)
	want := map[string]int64{
		"kind=view,reason=max_age":      1,
		"kind=agent-feed,reason=client": 1,
		"kind=view,reason=publisher":    1,
	}
	if len(closes) != len(want) {
		t.Errorf("closes = %v, want %v", closes, want)
	}
	for k, v := range want {
		if closes[k] != v {
			t.Errorf("closes[%s] = %d, want %d (all: %v)", k, closes[k], v, closes)
		}
	}
	if got := int64Points(t, reader, ssemetrics.MetricEventsWritten); got["kind=view"] != 2 {
		t.Errorf("events_written = %v", got)
	}
	if got := int64Points(t, reader, ssemetrics.MetricBytesWritten); got["kind=view"] != 170 {
		t.Errorf("bytes_written = %v", got)
	}

	recs := logs.records(t)
	if len(recs) != 3 {
		t.Fatalf("got %d close log lines, want 3: %v", len(recs), recs)
	}
	first := recs[0]
	wantFirst := map[string]any{
		"msg":            "events stream closed",
		"level":          "INFO",
		"conn_id":        "c1",
		"user_id":        "alice",
		"browser_id":     "b1",
		"tab_id":         "t1",
		"kind":           "view",
		"subjects":       float64(3),
		"duration_ms":    float64(10000),
		"events_written": float64(2),
		"bytes_written":  float64(170),
		"reason":         "max_age",
	}
	for k, v := range wantFirst {
		if first[k] != v {
			t.Errorf("close log %s = %v, want %v", k, first[k], v)
		}
	}
	if recs[1]["reason"] != "client" || recs[1]["conn_id"] != "c2" || recs[1]["duration_ms"] != float64(9000) {
		t.Errorf("second close log = %v", recs[1])
	}
	if recs[2]["reason"] != "publisher" || recs[2]["conn_id"] != "c3" {
		t.Errorf("third close log = %v", recs[2])
	}
}

func TestRegistryStreamDurationHistogram(t *testing.T) {
	r, reader := newMeteredRegistry(t)
	now := time.Unix(1000, 0)
	r.clock = func() time.Time { return now }
	s := r.Register(Key{UserID: "u", Kind: KindTerminalMetadata, ConnID: "c"}, 1)
	now = now.Add(90 * time.Second)
	s.Close(CloseShutdown, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != ssemetrics.MetricStreamDuration {
				continue
			}
			h := m.Data.(metricdata.Histogram[float64])
			if len(h.DataPoints) != 1 {
				t.Fatalf("duration points = %d, want 1", len(h.DataPoints))
			}
			p := h.DataPoints[0]
			if attrKey(p.Attributes) != "kind=terminal-metadata,reason=shutdown" || p.Count != 1 || p.Sum != 90 {
				t.Fatalf("duration point attrs=%s count=%d sum=%v", attrKey(p.Attributes), p.Count, p.Sum)
			}
			return
		}
	}
	t.Fatalf("%s not exported", ssemetrics.MetricStreamDuration)
}

func TestRegistryConcurrentRegisterAndClose(t *testing.T) {
	r, reader := newMeteredRegistry(t)
	lg := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	const n = 200
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			kind := KindView
			if i%2 == 1 {
				kind = KindAgentFeed
			}
			s := r.Register(Key{UserID: []string{"a", "b", "c"}[i%3], Kind: kind, ConnID: NewConnID()}, 1)
			s.RecordEvent(1)
			s.Close(CloseClient, lg)
		}(i)
	}
	wg.Wait()
	if r.Count() != 0 || r.Users() != 0 {
		t.Fatalf("Count=%d Users=%d after concurrent closes", r.Count(), r.Users())
	}
	streams := int64Points(t, reader, ssemetrics.MetricStreams)
	if streams["kind=view"] != 0 || streams["kind=agent-feed"] != 0 {
		t.Errorf("streams gauge = %v, want 0 for both kinds", streams)
	}
	if got := int64Points(t, reader, ssemetrics.MetricCloses); got["kind=view,reason=client"]+got["kind=agent-feed,reason=client"] != n {
		t.Errorf("closes = %v, want %d in total", got, n)
	}
}
