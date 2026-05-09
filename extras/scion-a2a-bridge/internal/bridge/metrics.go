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
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds Prometheus metrics for the A2A bridge.
type Metrics struct {
	RequestsTotal   *prometheus.CounterVec
	RequestDuration *prometheus.HistogramVec
	ActiveSSE       prometheus.Gauge
	TasksCreated    *prometheus.CounterVec
	TasksCompleted  *prometheus.CounterVec
}

// NewMetrics creates and registers Prometheus metrics.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		RequestsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "a2a_bridge_requests_total",
				Help: "Total number of A2A bridge requests.",
			},
			[]string{"method", "status"},
		),
		RequestDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "a2a_bridge_request_duration_seconds",
				Help:    "Request latency in seconds.",
				Buckets: prometheus.DefBuckets,
			},
			[]string{"method"},
		),
		ActiveSSE: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "a2a_bridge_active_sse_connections",
				Help: "Number of active SSE streaming connections.",
			},
		),
		TasksCreated: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "a2a_bridge_tasks_created_total",
				Help: "Total tasks created.",
			},
			[]string{"grove"},
		),
		TasksCompleted: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "a2a_bridge_tasks_completed_total",
				Help: "Total tasks completed by final state.",
			},
			[]string{"state"},
		),
	}

	reg.MustRegister(m.RequestsTotal, m.RequestDuration, m.ActiveSSE, m.TasksCreated, m.TasksCompleted)
	return m
}

// MetricsHandler returns an http.Handler for the /metrics endpoint.
func MetricsHandler() http.Handler {
	return promhttp.Handler()
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.status = code
	sr.ResponseWriter.WriteHeader(code)
}

func (sr *statusRecorder) Flush() {
	if f, ok := sr.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (sr *statusRecorder) Unwrap() http.ResponseWriter { return sr.ResponseWriter }

func normalizeRoute(path string) string {
	switch {
	case path == "/healthz":
		return "/healthz"
	case path == "/readyz":
		return "/readyz"
	case path == "/metrics":
		return "/metrics"
	case path == "/.well-known/agent-card.json":
		return "/agent-card"
	case strings.HasSuffix(path, "/.well-known/agent-card.json"):
		return "/agent-card"
	case strings.HasSuffix(path, "/jsonrpc"):
		return "/jsonrpc"
	default:
		return "/other"
	}
}

// InstrumentHandler wraps an http.Handler to record request metrics.
// NOTE: SSE connections inflate the duration histogram since ServeHTTP blocks
// for the connection lifetime. Acceptable for MVP; a separate SSE-duration
// metric can be added if P99 alerts become unreliable.
func InstrumentHandler(next http.Handler, metrics *Metrics) http.Handler {
	if metrics == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(rec, r)

		duration := time.Since(start).Seconds()
		route := r.Method + " " + normalizeRoute(r.URL.Path)
		metrics.RequestsTotal.WithLabelValues(route, strconv.Itoa(rec.status)).Inc()
		metrics.RequestDuration.WithLabelValues(route).Observe(duration)
	})
}
