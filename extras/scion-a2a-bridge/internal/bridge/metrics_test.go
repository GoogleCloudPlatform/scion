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

package bridge

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func TestNewMetricsRegistration(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	if m.RequestsTotal == nil {
		t.Error("RequestsTotal should not be nil")
	}
	if m.RequestDuration == nil {
		t.Error("RequestDuration should not be nil")
	}
	if m.ActiveSSE == nil {
		t.Error("ActiveSSE should not be nil")
	}
	if m.TasksCreated == nil {
		t.Error("TasksCreated should not be nil")
	}
	if m.TasksCompleted == nil {
		t.Error("TasksCompleted should not be nil")
	}

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	names := make(map[string]bool)
	for _, f := range families {
		names[f.GetName()] = true
	}

	for _, want := range []string{
		"a2a_bridge_active_sse_connections",
	} {
		if !names[want] {
			t.Errorf("expected metric %q to be registered", want)
		}
	}
}

func TestInstrumentHandlerRecordsMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := InstrumentHandler(inner, m)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var metric dto.Metric
	counter, err := m.RequestsTotal.GetMetricWithLabelValues("GET /healthz", "200")
	if err != nil {
		t.Fatalf("get metric: %v", err)
	}
	counter.Write(&metric)
	if got := metric.GetCounter().GetValue(); got != 1 {
		t.Errorf("RequestsTotal = %v, want 1", got)
	}
}

func TestNormalizeRoute(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/healthz", "/healthz"},
		{"/readyz", "/readyz"},
		{"/metrics", "/metrics"},
		{"/.well-known/agent-card.json", "/agent-card"},
		{"/groves/foo/agents/bar/.well-known/agent-card.json", "/agent-card"},
		{"/groves/foo/agents/bar/jsonrpc", "/jsonrpc"},
		{"/some/random/path", "/other"},
	}
	for _, tt := range tests {
		if got := normalizeRoute(tt.path); got != tt.want {
			t.Errorf("normalizeRoute(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

func TestInstrumentHandlerRecordsDuration(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := InstrumentHandler(inner, m)

	req := httptest.NewRequest(http.MethodPost, "/jsonrpc", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var metric dto.Metric
	observer, err := m.RequestDuration.GetMetricWithLabelValues("POST /jsonrpc")
	if err != nil {
		t.Fatalf("get metric: %v", err)
	}
	observer.(prometheus.Metric).Write(&metric)
	if got := metric.GetHistogram().GetSampleCount(); got != 1 {
		t.Errorf("RequestDuration sample count = %d, want 1", got)
	}
}

func TestInstrumentHandlerNilMetrics(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := InstrumentHandler(inner, nil)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("nil metrics should pass through, got status %d", rec.Code)
	}
}

func TestInstrumentHandlerCapturesNonOKStatus(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	handler := InstrumentHandler(inner, m)

	req := httptest.NewRequest(http.MethodGet, "/missing", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var metric dto.Metric
	counter, err := m.RequestsTotal.GetMetricWithLabelValues("GET /other", "404")
	if err != nil {
		t.Fatalf("get metric: %v", err)
	}
	counter.Write(&metric)
	if got := metric.GetCounter().GetValue(); got != 1 {
		t.Errorf("RequestsTotal for 404 = %v, want 1", got)
	}
}

func TestStatusRecorderForwardsFlusher(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("expected http.Flusher to be available through statusRecorder")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		w.Write([]byte("data: hello\n\n"))
		flusher.Flush()
	})

	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	handler := InstrumentHandler(inner, m)

	req := httptest.NewRequest(http.MethodPost, "/jsonrpc", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "data: hello\n\n" {
		t.Errorf("body = %q, want SSE data", got)
	}
}

func TestFlusherThroughFullMiddlewareChain(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("http.Flusher lost through middleware chain")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		w.Write([]byte("data: streamed\n\n"))
		flusher.Flush()
	})

	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	handler := RateLimitMiddleware(inner, RateLimitConfig{Enabled: true, RequestsPerSec: 100, Burst: 100})
	handler = InstrumentHandler(handler, m)

	req := httptest.NewRequest(http.MethodPost, "/jsonrpc", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "data: streamed\n\n" {
		t.Errorf("body = %q, want SSE data through full chain", got)
	}
}

func TestStatusRecorderUnwrapEnablesSetWriteDeadline(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		if err := rc.SetWriteDeadline(time.Time{}); err != nil {
			t.Fatalf("SetWriteDeadline through statusRecorder should succeed, got: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	})

	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	handler := InstrumentHandler(inner, m)

	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/test")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestMetricsHandler(t *testing.T) {
	handler := MetricsHandler()
	if handler == nil {
		t.Fatal("MetricsHandler returned nil")
	}

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("metrics endpoint status = %d, want 200", rec.Code)
	}
}
