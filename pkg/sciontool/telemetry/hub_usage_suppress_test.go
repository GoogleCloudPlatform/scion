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

package telemetry

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"
)

type captureOTLPBackend struct {
	colmetricpb.UnimplementedMetricsServiceServer
	coltracepb.UnimplementedTraceServiceServer
	collogspb.UnimplementedLogsServiceServer

	mu      sync.Mutex
	metrics []*metricpb.ResourceMetrics
	traces  []*tracepb.ResourceSpans
	logs    []*logspb.ResourceLogs
}

func (b *captureOTLPBackend) Export(_ context.Context, req *colmetricpb.ExportMetricsServiceRequest) (*colmetricpb.ExportMetricsServiceResponse, error) {
	b.mu.Lock()
	b.metrics = append(b.metrics, req.GetResourceMetrics()...)
	b.mu.Unlock()
	return &colmetricpb.ExportMetricsServiceResponse{}, nil
}

func (b *captureOTLPBackend) ExportTraces(_ context.Context, req *coltracepb.ExportTraceServiceRequest) (*coltracepb.ExportTraceServiceResponse, error) {
	b.mu.Lock()
	b.traces = append(b.traces, req.GetResourceSpans()...)
	b.mu.Unlock()
	return &coltracepb.ExportTraceServiceResponse{}, nil
}

type captureTraceSvc struct {
	coltracepb.UnimplementedTraceServiceServer
	parent *captureOTLPBackend
}

func (c *captureTraceSvc) Export(ctx context.Context, req *coltracepb.ExportTraceServiceRequest) (*coltracepb.ExportTraceServiceResponse, error) {
	return c.parent.ExportTraces(ctx, req)
}

type captureLogSvc struct {
	collogspb.UnimplementedLogsServiceServer
	parent *captureOTLPBackend
}

func (c *captureLogSvc) Export(_ context.Context, req *collogspb.ExportLogsServiceRequest) (*collogspb.ExportLogsServiceResponse, error) {
	c.parent.mu.Lock()
	c.parent.logs = append(c.parent.logs, req.GetResourceLogs()...)
	c.parent.mu.Unlock()
	return &collogspb.ExportLogsServiceResponse{}, nil
}

func startCaptureOTLPBackend(t *testing.T) (string, *captureOTLPBackend) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	b := &captureOTLPBackend{}
	srv := grpc.NewServer()
	colmetricpb.RegisterMetricsServiceServer(srv, b)
	coltracepb.RegisterTraceServiceServer(srv, &captureTraceSvc{parent: b})
	collogspb.RegisterLogsServiceServer(srv, &captureLogSvc{parent: b})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() {
		srv.Stop()
		_ = lis.Close()
	})
	return lis.Addr().String(), b
}

func testMixedMetricsBatch() []*metricpb.ResourceMetrics {
	now := uint64(time.Now().UnixNano())
	mkCounter := func(name string, val int64, attrs ...*commonpb.KeyValue) *metricpb.Metric {
		return &metricpb.Metric{
			Name: name,
			Data: &metricpb.Metric_Sum{
				Sum: &metricpb.Sum{
					AggregationTemporality: metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
					IsMonotonic:            true,
					DataPoints: []*metricpb.NumberDataPoint{{
						StartTimeUnixNano: now - 1000,
						TimeUnixNano:      now,
						Value:             &metricpb.NumberDataPoint_AsInt{AsInt: val},
						Attributes:        attrs,
					}},
				},
			},
		}
	}
	return []*metricpb.ResourceMetrics{{
		Resource: &resourcepb.Resource{
			Attributes: []*commonpb.KeyValue{
				metricStringLabel("service.name", "scion-agent"),
				metricStringLabel("scion.agent.id", "ag-sup"),
				metricStringLabel("scion.project.id", "proj-sup"),
			},
		},
		ScopeMetrics: []*metricpb.ScopeMetrics{{
			Scope: &commonpb.InstrumentationScope{Name: hookMetricScope},
			Metrics: []*metricpb.Metric{
				mkCounter(telemetrycontract.MetricAPICalls, 1,
					metricStringLabel("harness", "claude"),
					metricStringLabel("model", "claude-sonnet-4-6"),
					metricStringLabel("status", "success"),
				),
				mkCounter(telemetrycontract.MetricUsageTokens, 100,
					metricStringLabel("harness", "claude"),
					metricStringLabel("model", "claude-sonnet-4-6"),
					metricStringLabel("token_type", "input"),
				),
				mkCounter("agent.tool.calls", 3,
					metricStringLabel("tool_name", "bash"),
				),
				mkCounter("agent.session.count", 1),
			},
		}},
	}}
}

func exportedMetricNames(rms []*metricpb.ResourceMetrics) map[string]int {
	out := map[string]int{}
	for _, rm := range rms {
		for _, sm := range rm.GetScopeMetrics() {
			for _, m := range sm.GetMetrics() {
				out[m.GetName()]++
			}
		}
	}
	return out
}

func TestPipeline_SuppressDirectOTelMatrix(t *testing.T) {
	cases := []struct {
		name               string
		wireHubSink        bool
		suppressDirectOTel bool
		wantCanonical      bool
	}{
		{
			name:               "old hub (no hub sink) keeps canonical counters",
			wireHubSink:        false,
			suppressDirectOTel: false,
			wantCanonical:      true,
		},
		{
			name:               "hub sink active with suppress=false keeps canonical counters",
			wireHubSink:        true,
			suppressDirectOTel: false,
			wantCanonical:      true,
		},
		{
			name:               "hub sink active with suppress=true strips canonical counters only",
			wireHubSink:        true,
			suppressDirectOTel: true,
			wantCanonical:      false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			addr, backend := startCaptureOTLPBackend(t)
			cfg := &Config{
				Enabled:      true,
				CloudEnabled: true,
				Endpoint:     addr,
				Protocol:     "grpc",
				Insecure:     true,
				GRPCPort:     0,
				HTTPPort:     0,
			}

			p := NewWithConfig(cfg)
			p.SetAnalyticsOutbox(t.TempDir(), -1, -1)
			if tc.wireHubSink {
				p.SetHubUsageSink(HubUsageSinkConfig{
					Post: func(context.Context, telemetrycontract.UsageIngestRequest) (*telemetrycontract.UsageIngestResponse, error) {
						return &telemetrycontract.UsageIngestResponse{}, nil
					},
					RunID:              "run-sup-test",
					HubID:              "hub-sup-test",
					SuppressDirectOTel: tc.suppressDirectOTel,
					DrainInterval:      time.Hour,
					HeartbeatInterval:  time.Hour,
				})
			}

			ctx := context.Background()
			if err := p.Start(ctx); err != nil {
				t.Fatalf("Pipeline.Start: %v", err)
			}

			if got := p.SuppressDirectOTel(); got != (tc.wireHubSink && tc.suppressDirectOTel) {
				t.Fatalf("SuppressDirectOTel() = %v, want %v", got, tc.wireHubSink && tc.suppressDirectOTel)
			}

			// Send mixed metrics (canonical + non-canonical), a trace span, and a log record.
			if err := p.handleMetrics(ctx, testMixedMetricsBatch()); err != nil {
				t.Fatalf("handleMetrics: %v", err)
			}
			if err := p.handleSpans(ctx, []*tracepb.ResourceSpans{{
				ScopeSpans: []*tracepb.ScopeSpans{{
					Spans: []*tracepb.Span{{Name: "test-span", TraceId: []byte("1234567890123456"), SpanId: []byte("12345678")}},
				}},
			}}); err != nil {
				t.Fatalf("handleSpans: %v", err)
			}
			if err := p.handleLogs(ctx, []*logspb.ResourceLogs{{
				ScopeLogs: []*logspb.ScopeLogs{{
					LogRecords: []*logspb.LogRecord{{Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "hello"}}}},
				}},
			}}); err != nil {
				t.Fatalf("handleLogs: %v", err)
			}

			stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			if err := p.Stop(stopCtx); err != nil {
				t.Fatalf("Pipeline.Stop: %v", err)
			}

			backend.mu.Lock()
			defer backend.mu.Unlock()
			names := exportedMetricNames(backend.metrics)

			// Non-canonical metrics, traces, and logs must ALWAYS be exported regardless of suppression.
			if names["agent.tool.calls"] == 0 {
				t.Errorf("expected agent.tool.calls to be exported, got %v", names)
			}
			if names["agent.session.count"] == 0 {
				t.Errorf("expected agent.session.count to be exported, got %v", names)
			}
			if len(backend.traces) == 0 {
				t.Errorf("expected traces to remain exported when suppress=%v", tc.suppressDirectOTel)
			}
			if len(backend.logs) == 0 {
				t.Errorf("expected logs to remain exported when suppress=%v", tc.suppressDirectOTel)
			}

			if tc.wantCanonical {
				if names[telemetrycontract.MetricAPICalls] == 0 {
					t.Errorf("expected %s to be exported, got %v", telemetrycontract.MetricAPICalls, names)
				}
				if names[telemetrycontract.MetricUsageTokens] == 0 {
					t.Errorf("expected %s to be exported, got %v", telemetrycontract.MetricUsageTokens, names)
				}
			} else {
				if names[telemetrycontract.MetricAPICalls] != 0 {
					t.Errorf("expected %s to be suppressed, got %v", telemetrycontract.MetricAPICalls, names)
				}
				if names[telemetrycontract.MetricUsageTokens] != 0 {
					t.Errorf("expected %s to be suppressed, got %v", telemetrycontract.MetricUsageTokens, names)
				}
			}
		})
	}
}

func TestHubUsageSink_DirectOTelSuppressedFlagAcrossRuns(t *testing.T) {
	dir := t.TempDir()
	o, err := OpenAnalyticsOutbox(dir)
	if err != nil {
		t.Fatalf("OpenAnalyticsOutbox: %v", err)
	}

	var gotReqs []telemetrycontract.UsageIngestRequest
	sink := NewHubUsageSink(o, HubUsageSinkConfig{
		Post: func(_ context.Context, req telemetrycontract.UsageIngestRequest) (*telemetrycontract.UsageIngestResponse, error) {
			gotReqs = append(gotReqs, req)
			return &telemetrycontract.UsageIngestResponse{Accepted: len(req.Events)}, nil
		},
		RunID:              "run-current",
		HubID:              "hub-1",
		SuppressDirectOTel: true,
	})

	now := time.Now().UTC()
	makeLine := func(runID, evID string, seq int64) []byte {
		inTokens := int64(10)
		ev := telemetrycontract.UsageEvent{
			Envelope:            telemetrycontract.NewEnvelope(telemetrycontract.EventTypeUsage, evID, "sciontool/agent/ag1", now, now),
			UsageSource:         telemetrycontract.UsageSourceNative,
			SourceKind:          "claude.api_request",
			SourceSchemaVersion: "1",
			SourceEventID:       "rid-" + evID,
			SourceIdentity:      telemetrycontract.SourceIdentityRequestID,
			SourceOccurrence:    1,
			ProducerEpoch:       "ep-1",
			ProducerSeq:         seq,
			RunID:               runID,
			Harness:             "claude",
			Model:               "claude-sonnet-4-6",
			ModelSource:         telemetrycontract.ModelSourceEvent,
			Status:              telemetrycontract.StatusSuccess,
			Calls:               1,
			Input:               &inTokens,
			InputDerivation:     telemetrycontract.InputDerivationReported,
			TokenStatus:         telemetrycontract.TokenStatusReported,
			MalformedFields:     []string{},
		}
		b, err := telemetrycontract.Marshal(&ev)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return b
	}

	// Batch contains one residual event from prior run and one from current run.
	_, err = sink.SendBatch(context.Background(), [][]byte{
		makeLine("run-prior", "ev-prior", 1),
		makeLine("run-current", "ev-curr", 2),
	})
	if err != nil {
		t.Fatalf("SendBatch: %v", err)
	}
	if len(gotReqs) != 2 {
		t.Fatalf("expected 2 grouped requests, got %d", len(gotReqs))
	}
	if gotReqs[0].RunID != "run-prior" || gotReqs[0].DirectOTelSuppressed {
		t.Errorf("prior-run residual batch must have DirectOTelSuppressed=false, got run=%q suppressed=%v",
			gotReqs[0].RunID, gotReqs[0].DirectOTelSuppressed)
	}
	if gotReqs[1].RunID != "run-current" || !gotReqs[1].DirectOTelSuppressed {
		t.Errorf("current-run batch must have DirectOTelSuppressed=true, got run=%q suppressed=%v",
			gotReqs[1].RunID, gotReqs[1].DirectOTelSuppressed)
	}
}
