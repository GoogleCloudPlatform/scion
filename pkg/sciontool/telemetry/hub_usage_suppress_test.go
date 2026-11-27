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
	"errors"
	"net"
	"sync"
	"sync/atomic"
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
						return &telemetrycontract.UsageIngestResponse{SuppressDirectOTel: tc.suppressDirectOTel}, nil
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

func TestHubUsageSink_DirectOTelSuppressedFromPerEventRecord(t *testing.T) {
	dir := t.TempDir()
	o, err := OpenAnalyticsOutbox(dir)
	if err != nil {
		t.Fatalf("OpenAnalyticsOutbox: %v", err)
	}

	var gotReqs []telemetrycontract.UsageIngestRequest
	sink := NewHubUsageSink(o, HubUsageSinkConfig{
		Post: func(_ context.Context, req telemetrycontract.UsageIngestRequest) (*telemetrycontract.UsageIngestResponse, error) {
			gotReqs = append(gotReqs, req)
			return &telemetrycontract.UsageIngestResponse{
				Accepted:           len(req.Events),
				SuppressDirectOTel: true,
			}, nil
		},
		RunID:              "run-current",
		HubID:              "hub-1",
		SuppressDirectOTel: false, // sender's current state must NOT override per-event stamps
	})

	now := time.Now().UTC()
	makeLine := func(runID, evID string, seq int64, suppressed bool) []byte {
		inTokens := int64(10)
		var supPtr *bool
		if suppressed {
			v := true
			supPtr = &v
		}
		ev := telemetrycontract.UsageEvent{
			Envelope:             telemetrycontract.NewEnvelope(telemetrycontract.EventTypeUsage, evID, "sciontool/agent/ag1", now, now),
			UsageSource:          telemetrycontract.UsageSourceNative,
			SourceKind:           "claude.api_request",
			SourceSchemaVersion:  "1",
			SourceEventID:        "rid-" + evID,
			SourceIdentity:       telemetrycontract.SourceIdentityRequestID,
			SourceOccurrence:     1,
			ProducerEpoch:        "ep-1",
			ProducerSeq:          seq,
			RunID:                runID,
			Harness:              "claude",
			Model:                "claude-sonnet-4-6",
			ModelSource:          telemetrycontract.ModelSourceEvent,
			Status:               telemetrycontract.StatusSuccess,
			Calls:                1,
			Input:                &inTokens,
			InputDerivation:      telemetrycontract.InputDerivationReported,
			TokenStatus:          telemetrycontract.TokenStatusReported,
			MalformedFields:      []string{},
			DirectOTelSuppressed: supPtr,
		}
		b, err := telemetrycontract.Marshal(&ev)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return b
	}

	// Case 1: prior-run residual written while suppressed -> expect true
	// Case 2: prior-run residual written unsuppressed / old producer -> expect false
	// Case 3: current-run event written while suppressed -> expect true
	_, err = sink.SendBatch(context.Background(), [][]byte{
		makeLine("run-prior-sup", "ev-prior-sup", 1, true),
		makeLine("run-prior-old", "ev-prior-old", 2, false),
		makeLine("run-current", "ev-curr-sup", 3, true),
	})
	if err != nil {
		t.Fatalf("SendBatch: %v", err)
	}
	if len(gotReqs) != 3 {
		t.Fatalf("expected 3 grouped requests, got %d", len(gotReqs))
	}
	if gotReqs[0].RunID != "run-prior-sup" || !gotReqs[0].DirectOTelSuppressed {
		t.Errorf("case 1 (prior-run suppressed residual): got run=%q suppressed=%v, want true",
			gotReqs[0].RunID, gotReqs[0].DirectOTelSuppressed)
	}
	if gotReqs[1].RunID != "run-prior-old" || gotReqs[1].DirectOTelSuppressed {
		t.Errorf("case 2 (prior-run unsuppressed/old residual): got run=%q suppressed=%v, want false",
			gotReqs[1].RunID, gotReqs[1].DirectOTelSuppressed)
	}
	if gotReqs[2].RunID != "run-current" || !gotReqs[2].DirectOTelSuppressed {
		t.Errorf("case 3 (current-run suppressed): got run=%q suppressed=%v, want true",
			gotReqs[2].RunID, gotReqs[2].DirectOTelSuppressed)
	}

	// Also verify UsageAnalytics.Record stamps DirectOTelSuppressed from the
	// suppression state at Record time.
	t.Setenv("SCION_AGENT_ID", "ag-stamp")
	t.Setenv("SCION_HARNESS", "claude")
	t.Setenv("SCION_LAUNCH_ID", "run-stamp")
	stampOutbox, err := OpenAnalyticsOutbox(t.TempDir())
	if err != nil {
		t.Fatalf("OpenAnalyticsOutbox: %v", err)
	}
	if _, err := stampOutbox.PinHubID("hub-1"); err != nil {
		t.Fatalf("PinHubID: %v", err)
	}
	supState := true
	ua := NewUsageAnalytics(stampOutbox, telemetrycontract.UsageSourceNative)
	ua.setDirectOTelSuppressedFn(func() bool { return supState })
	inTok := int64(5)
	if _, err := ua.Record(UsageObservation{
		SourceKind:      "claude.api_request",
		Identity:        telemetrycontract.SourceIdentityRequestID,
		SourceEventID:   "req_aaaaaaaaaaaaaaaa",
		Model:           "claude-sonnet-4-6",
		Status:          telemetrycontract.StatusSuccess,
		Calls:           1,
		Input:           &inTok,
		InputDerivation: telemetrycontract.InputDerivationReported,
		TokenStatus:     telemetrycontract.TokenStatusReported,
	}); err != nil {
		t.Fatalf("Record suppressed=true: %v", err)
	}
	supState = false
	if _, err := ua.Record(UsageObservation{
		SourceKind:      "claude.api_request",
		Identity:        telemetrycontract.SourceIdentityRequestID,
		SourceEventID:   "req_bbbbbbbbbbbbbbbb",
		Model:           "claude-sonnet-4-6",
		Status:          telemetrycontract.StatusSuccess,
		Calls:           1,
		Input:           &inTok,
		InputDerivation: telemetrycontract.InputDerivationReported,
		TokenStatus:     telemetrycontract.TokenStatusReported,
	}); err != nil {
		t.Fatalf("Record suppressed=false: %v", err)
	}
	if _, err := stampOutbox.finalize(context.Background(), "hub-1"); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	var recorded []telemetrycontract.UsageEvent
	_, err = stampOutbox.DrainBatch(context.Background(), 10, 64*1024, func(_ context.Context, lines [][]byte) (BatchSendOutcome, error) {
		for _, l := range lines {
			var ev telemetrycontract.UsageEvent
			if uerr := telemetrycontract.Unmarshal(l, &ev); uerr != nil {
				return BatchSendOutcome{}, uerr
			}
			recorded = append(recorded, ev)
		}
		return BatchSendOutcome{}, nil
	})
	if err != nil {
		t.Fatalf("DrainBatch: %v", err)
	}
	if len(recorded) != 2 || !eventDirectOTelSuppressed(&recorded[0]) || eventDirectOTelSuppressed(&recorded[1]) {
		t.Fatalf("expected [true, false] per-event suppression stamps, got %+v", recorded)
	}
}

func TestPipeline_SuppressRevalidatedPerResponse(t *testing.T) {
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

	var mu sync.Mutex
	replicaSuppress := true

	p := NewWithConfig(cfg)
	p.SetAnalyticsOutbox(t.TempDir(), -1, -1)
	p.SetHubUsageSink(HubUsageSinkConfig{
		Post: func(_ context.Context, req telemetrycontract.UsageIngestRequest) (*telemetrycontract.UsageIngestResponse, error) {
			mu.Lock()
			sup := replicaSuppress
			mu.Unlock()
			return &telemetrycontract.UsageIngestResponse{
				Accepted:           len(req.Events),
				SuppressDirectOTel: sup,
			}, nil
		},
		RunID:              "run-reval",
		HubID:              "hub-reval",
		SuppressDirectOTel: true,
		DrainInterval:      time.Hour,
		HeartbeatInterval:  time.Hour,
	})

	ctx := context.Background()
	if err := p.Start(ctx); err != nil {
		t.Fatalf("Pipeline.Start: %v", err)
	}

	// Stop the background drainer goroutine before driving hubSink directly so
	// its startup/shutdown heartbeats cannot race with SendBatch/SendHeartbeat.
	p.analytics.drainer.Stop(ctx)

	// Initially suppressed (replica A says true).
	if !p.SuppressDirectOTel() {
		t.Fatal("expected SuppressDirectOTel=true initially")
	}
	if err := p.analytics.drainer.hubSink.SendHeartbeat(ctx, ""); err != nil {
		t.Fatalf("heartbeat replica A: %v", err)
	}
	if !p.SuppressDirectOTel() {
		t.Fatal("expected SuppressDirectOTel=true after replica A response")
	}

	// Replica B answers suppress=false (or omits the field) on an event batch
	// via sendGroup (mutant s8): suppression MUST flip to false.
	mu.Lock()
	replicaSuppress = false
	mu.Unlock()
	now := time.Now().UTC()
	inTok := int64(10)
	supTrue := true
	ev := telemetrycontract.UsageEvent{
		Envelope:             telemetrycontract.NewEnvelope(telemetrycontract.EventTypeUsage, "ev-reval-batch", "sciontool/agent/ag1", now, now),
		UsageSource:          telemetrycontract.UsageSourceNative,
		SourceKind:           "claude.api_request",
		SourceSchemaVersion:  "1",
		SourceEventID:        "rid-reval-batch",
		SourceIdentity:       telemetrycontract.SourceIdentityRequestID,
		SourceOccurrence:     1,
		ProducerEpoch:        "ep-1",
		ProducerSeq:          1,
		RunID:                "run-reval",
		Harness:              "claude",
		Model:                "claude-sonnet-4-6",
		ModelSource:          telemetrycontract.ModelSourceEvent,
		Status:               telemetrycontract.StatusSuccess,
		Calls:                1,
		Input:                &inTok,
		InputDerivation:      telemetrycontract.InputDerivationReported,
		TokenStatus:          telemetrycontract.TokenStatusReported,
		MalformedFields:      []string{},
		DirectOTelSuppressed: &supTrue,
	}
	line, err := telemetrycontract.Marshal(&ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := p.analytics.drainer.hubSink.SendBatch(ctx, [][]byte{line}); err != nil {
		t.Fatalf("SendBatch replica B: %v", err)
	}
	if p.SuppressDirectOTel() {
		t.Fatal("expected SuppressDirectOTel=false after replica B batch response answered false")
	}

	// Flip back to true via batch response, then flip back to false via heartbeat
	// to pin both SendBatch (sendGroup) and SendHeartbeat revalidation paths.
	mu.Lock()
	replicaSuppress = true
	mu.Unlock()
	if _, err := p.analytics.drainer.hubSink.SendBatch(ctx, [][]byte{line}); err != nil {
		t.Fatalf("SendBatch replica A: %v", err)
	}
	if !p.SuppressDirectOTel() {
		t.Fatal("expected SuppressDirectOTel=true after replica A batch response")
	}
	mu.Lock()
	replicaSuppress = false
	mu.Unlock()
	if err := p.analytics.drainer.hubSink.SendHeartbeat(ctx, ""); err != nil {
		t.Fatalf("heartbeat replica B: %v", err)
	}
	if p.SuppressDirectOTel() {
		t.Fatal("expected SuppressDirectOTel=false after replica B heartbeat answered false")
	}

	// Canonical counters sent now must be exported directly again.
	if err := p.handleMetrics(ctx, testMixedMetricsBatch()); err != nil {
		t.Fatalf("handleMetrics after un-suppress: %v", err)
	}

	stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := p.Stop(stopCtx); err != nil {
		t.Fatalf("Pipeline.Stop: %v", err)
	}

	backend.mu.Lock()
	defer backend.mu.Unlock()
	names := exportedMetricNames(backend.metrics)
	if names[telemetrycontract.MetricAPICalls] == 0 || names[telemetrycontract.MetricUsageTokens] == 0 {
		t.Fatalf("expected canonical counters to resume direct export after replica B returned suppress=false, got %v", names)
	}
}

func TestPipeline_SuppressClearedOnRevocationAndSetOnLateActivation(t *testing.T) {
	// 1. Revocation (O4): onHubRevoked flips suppression from true -> false.
	t.Run("revocation clears suppression", func(t *testing.T) {
		var revoke atomic.Bool
		p := NewWithConfig(&Config{Enabled: true, CloudEnabled: false, GRPCPort: 0, HTTPPort: 0})
		p.SetAnalyticsOutbox(t.TempDir(), -1, -1)
		p.SetHubUsageSink(HubUsageSinkConfig{
			Post: func(context.Context, telemetrycontract.UsageIngestRequest) (*telemetrycontract.UsageIngestResponse, error) {
				if revoke.Load() {
					return nil, ErrUsageIngestRevoked
				}
				return &telemetrycontract.UsageIngestResponse{SuppressDirectOTel: true}, nil
			},
			RunID:              "run-revoke",
			HubID:              "hub-revoke",
			SuppressDirectOTel: true,
			DrainInterval:      time.Hour,
			HeartbeatInterval:  time.Hour,
		})
		ctx := context.Background()
		if err := p.Start(ctx); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if !p.SuppressDirectOTel() {
			t.Fatal("expected SuppressDirectOTel=true before revocation")
		}
		if p.analytics.drainer.onHubRevoked == nil {
			t.Fatal("expected onHubRevoked callback to be wired")
		}
		// Stop the background drainer before enabling revocation so an in-flight
		// startup heartbeat cannot race with onHubRevoked and re-enable suppression.
		p.analytics.drainer.Stop(ctx)
		revoke.Store(true)
		p.analytics.drainer.onHubRevoked()
		if p.SuppressDirectOTel() {
			t.Fatal("expected SuppressDirectOTel=false after onHubRevoked")
		}
		stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		_ = p.Stop(stopCtx)
	})

	// 2. Late activation (O4): probe callback returning SuppressDirectOTel=true flips false -> true.
	t.Run("late activation sets suppression", func(t *testing.T) {
		p := NewWithConfig(&Config{Enabled: true, CloudEnabled: false, GRPCPort: 0, HTTPPort: 0})
		hubDir := HubAnalyticsOutboxDirFor(t.TempDir())
		hubOutbox, err := OpenAnalyticsOutbox(hubDir)
		if err != nil {
			t.Fatalf("OpenAnalyticsOutbox: %v", err)
		}
		if p.SuppressDirectOTel() {
			t.Fatal("expected SuppressDirectOTel=false initially")
		}
		p.analytics.onSuppressChanged = p.setSuppressDirectOTel
		p.analytics.isSuppressed = p.SuppressDirectOTel

		drainer := NewAnalyticsDrainer(hubOutbox, func(context.Context, []byte) error { return nil }, func(context.Context) (string, error) { return "hub-1", nil })
		drainer.hubOutboxDir = hubDir
		drainer.SetHubProbe(
			func(context.Context) (*HubUsageSinkConfig, error) {
				return &HubUsageSinkConfig{
					Post: func(context.Context, telemetrycontract.UsageIngestRequest) (*telemetrycontract.UsageIngestResponse, error) {
						return &telemetrycontract.UsageIngestResponse{SuppressDirectOTel: true}, nil
					},
					RunID:              "run-late",
					HubID:              "hub-1",
					SuppressDirectOTel: true,
				}, nil
			},
			time.Millisecond,
			nil,
			nil,
			func(cfg HubUsageSinkConfig, ho *AnalyticsOutbox, sink *HubUsageSink) {
				applySuppress := func(v bool) {
					ho.setHubSuppress(v)
					p.setSuppressDirectOTel(v)
				}
				sink.cfg.OnSuppressChanged = applySuppress
				applySuppress(cfg.SuppressDirectOTel)
			},
		)
		drainer.RunOnce(context.Background())
		if !p.SuppressDirectOTel() {
			t.Fatal("expected SuppressDirectOTel=true after late probe activation")
		}
		if !isHubOutboxSuppressDir(hubDir) {
			t.Fatal("expected suppress.hub marker on disk after late probe activation")
		}
	})

	// 3. Revocation on the late-activation (GCP-default) path (mutant s6b):
	// exercises analyticsState.startDrainer's default branch end-to-end.
	t.Run("late activation followed by revocation clears suppression", func(t *testing.T) {
		addr, backend := startCaptureOTLPBackend(t)
		p := NewWithConfig(&Config{
			Enabled:      true,
			CloudEnabled: true,
			Endpoint:     addr,
			Protocol:     "grpc",
			Insecure:     true,
			GRPCPort:     0,
			HTTPPort:     0,
		})
		home := t.TempDir()
		p.SetAnalyticsOutbox(home, -1, -1)
		p.SetHubIDResolver(func(context.Context) (string, error) { return "hub-late-rev", nil })
		p.SetHubUsageProbeInterval(time.Millisecond)
		var revoke atomic.Bool
		p.SetHubUsageProbe(func(context.Context) (*HubUsageSinkConfig, error) {
			return &HubUsageSinkConfig{
				Post: func(context.Context, telemetrycontract.UsageIngestRequest) (*telemetrycontract.UsageIngestResponse, error) {
					if revoke.Load() {
						return nil, ErrUsageIngestRevoked
					}
					return &telemetrycontract.UsageIngestResponse{SuppressDirectOTel: true}, nil
				},
				RunID:              "run-late-rev",
				HubID:              "hub-late-rev",
				SuppressDirectOTel: true,
				DrainInterval:      time.Hour,
				HeartbeatInterval:  time.Millisecond,
			}, nil
		})

		ctx := context.Background()
		if err := p.Start(ctx); err != nil {
			t.Fatalf("Pipeline.Start: %v", err)
		}
		// Start analyticsState in the default (GCP-only + hubProbe) branch directly
		// with a non-nil *GCPExporter stub so startDrainer takes default:.
		p.analytics.startDrainer(ctx, &GCPExporter{})
		// Wait for the background drainer loop's initial probe to activate the Hub
		// sink and set suppress=true (never call RunOnce concurrently with Start).
		deadline := time.Now().Add(2 * time.Second)
		for !p.SuppressDirectOTel() && time.Now().Before(deadline) {
			time.Sleep(2 * time.Millisecond)
		}
		if !p.SuppressDirectOTel() {
			t.Fatal("expected SuppressDirectOTel=true after late activation")
		}
		// Stop the drainer goroutine, then run one deterministic RunOnce with 404
		// revocation enabled -> exercises the late-activation branch's wired
		// onHubRevoked callback end-to-end without concurrent RunOnce races.
		p.analytics.drainer.Stop(ctx)
		revoke.Store(true)
		time.Sleep(2 * time.Millisecond)
		p.analytics.drainer.RunOnce(ctx)
		if p.SuppressDirectOTel() {
			t.Fatal("expected SuppressDirectOTel=false after revocation on late-activation path")
		}
		// Canonical counters must now pass through handleMetrics and export directly.
		if err := p.handleMetrics(ctx, testMixedMetricsBatch()); err != nil {
			t.Fatalf("handleMetrics after late-activation revocation: %v", err)
		}
		stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		_ = p.Stop(stopCtx)

		backend.mu.Lock()
		defer backend.mu.Unlock()
		names := exportedMetricNames(backend.metrics)
		if names[telemetrycontract.MetricAPICalls] == 0 || names[telemetrycontract.MetricUsageTokens] == 0 {
			t.Fatalf("expected canonical counters exported after late-activation revocation, got %v", names)
		}
	})
}

func TestHookPath_DirectOTelSuppressedFromMarker(t *testing.T) {
	for _, tc := range []struct {
		name       string
		gcpEnabled bool
	}{
		{name: "gcp_enabled_tee", gcpEnabled: true},
		{name: "hub_only", gcpEnabled: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SCION_AGENT_ID", "agent-hook-1")
			t.Setenv("SCION_PROJECT_ID", "proj-hook-1")
			t.Setenv("SCION_RUN_ID", "run-hook-1")
			t.Setenv("SCION_HARNESS", "claude")

			home := t.TempDir()
			hubOutbox, err := OpenAnalyticsOutbox(HubAnalyticsOutboxDirFor(home))
			if err != nil {
				t.Fatalf("OpenAnalyticsOutbox(hub): %v", err)
			}
			var reqs []telemetrycontract.UsageIngestRequest
			sink := NewHubUsageSink(hubOutbox, HubUsageSinkConfig{
				RunID: "run-hook-1",
				HubID: "hub-hook-1",
				Post: func(_ context.Context, req telemetrycontract.UsageIngestRequest) (*telemetrycontract.UsageIngestResponse, error) {
					reqs = append(reqs, req)
					return &telemetrycontract.UsageIngestResponse{Accepted: len(req.Events)}, nil
				},
			})
			d := NewDualAnalyticsDrainer(nil, nil, sink, func(context.Context) (string, error) { return "hub-hook-1", nil })
			hubOutbox.setHubSuppress(true)

			inTok := int64(100)
			outTok := int64(25)
			now := time.Now().UTC()
			makeHookObs := func(id string) UsageObservation {
				return UsageObservation{
					SourceKind:      "claude.api_request",
					Identity:        telemetrycontract.SourceIdentityComposite,
					SourceEventID:   id,
					SourceTime:      now,
					EventTime:       now,
					Model:           "claude-sonnet-4-6",
					Status:          telemetrycontract.StatusSuccess,
					Calls:           1,
					Input:           &inTok,
					Output:          &outTok,
					InputDerivation: telemetrycontract.InputDerivationReported,
					TokenStatus:     telemetrycontract.TokenStatusReported,
					MalformedFields: []string{},
				}
			}

			// 1. With suppress.hub marker present, hook subprocess stamps direct_otel_suppressed=true.
			hookSuppressed := NewLazyHookUsageAnalytics(home, tc.gcpEnabled, nil)
			if out, err := hookSuppressed.Record(makeHookObs("hook-ev-suppressed")); err != nil || out != AppendWritten {
				t.Fatalf("hookSuppressed.Record = (%v, %v), want (AppendWritten, nil)", out, err)
			}

			// 2. Remove suppress.hub marker; next hook subprocess stamps false/absent.
			hubOutbox.setHubSuppress(false)
			hookUnsuppressed := NewLazyHookUsageAnalytics(home, tc.gcpEnabled, nil)
			if out, err := hookUnsuppressed.Record(makeHookObs("hook-ev-unsuppressed")); err != nil || out != AppendWritten {
				t.Fatalf("hookUnsuppressed.Record = (%v, %v), want (AppendWritten, nil)", out, err)
			}

			// Drain hubOutbox via HubUsageSink + drainer and verify per-event stamps
			// and per-group request flags.
			d.RunOnce(context.Background())

			var seenSuppressed, seenUnsuppressed bool
			for _, req := range reqs {
				for _, ev := range req.Events {
					suppressed := ev.DirectOTelSuppressed != nil && *ev.DirectOTelSuppressed
					if req.DirectOTelSuppressed != suppressed {
						t.Fatalf("event %s: req.DirectOTelSuppressed=%v != event stamp=%v", ev.EventID, req.DirectOTelSuppressed, suppressed)
					}
					switch ev.SourceEventID {
					case "hook-ev-suppressed":
						if !suppressed {
							t.Fatalf("hook-ev-suppressed: expected direct_otel_suppressed=true, got %v", ev.DirectOTelSuppressed)
						}
						seenSuppressed = true
					case "hook-ev-unsuppressed":
						if suppressed {
							t.Fatalf("hook-ev-unsuppressed: expected direct_otel_suppressed absent/false, got true")
						}
						seenUnsuppressed = true
					}
				}
			}
			if !seenSuppressed || !seenUnsuppressed {
				t.Fatalf("did not observe both hook events in drained batches (seenSuppressed=%v, seenUnsuppressed=%v, reqs=%d)", seenSuppressed, seenUnsuppressed, len(reqs))
			}
		})
	}
}

func TestPipeline_NativeMeteringOnlyCombinedWithSuppressDirectOTel(t *testing.T) {
	mkBatch := func() []*metricpb.ResourceMetrics {
		now := uint64(time.Now().UnixNano())
		mkCounter := func(name string, val int64) *metricpb.Metric {
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
						}},
					},
				},
			}
		}
		mkRM := func(serviceName string, metrics ...*metricpb.Metric) *metricpb.ResourceMetrics {
			return &metricpb.ResourceMetrics{
				Resource: &resourcepb.Resource{
					Attributes: []*commonpb.KeyValue{{
						Key:   "service.name",
						Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: serviceName}},
					}},
				},
				ScopeMetrics: []*metricpb.ScopeMetrics{{Metrics: metrics}},
			}
		}
		return []*metricpb.ResourceMetrics{
			// Native harness metric (non-sciontool resource): always dropped by metering-only.
			mkRM("claude-code", mkCounter("native.harness.metric", 99)),
			// sciontool-derived canonical usage counters: exported when suppress=false, stripped when suppress=true.
			mkRM("sciontool",
				mkCounter(telemetrycontract.MetricAPICalls, 1),
				mkCounter(telemetrycontract.MetricUsageTokens, 500),
				// sciontool non-canonical metric: always passes both filters.
				mkCounter("agent.tool.calls", 3),
			),
		}
	}

	for _, tc := range []struct {
		name           string
		suppressDirect bool
		wantCanonical  bool
	}{
		{
			name:           "metering_only_on_suppress_off",
			suppressDirect: false,
			wantCanonical:  true,
		},
		{
			name:           "metering_only_on_suppress_on",
			suppressDirect: true,
			wantCanonical:  false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr, backend := startCaptureOTLPBackend(t)
			p := NewWithConfig(&Config{
				Enabled:      true,
				CloudEnabled: true,
				Endpoint:     addr,
				Protocol:     "grpc",
				Insecure:     true,
				GRPCPort:     0,
				HTTPPort:     0,
			})
			p.SetNativeMeteringOnly(true)
			p.SetAnalyticsOutbox(t.TempDir(), -1, -1)
			p.SetHubUsageSink(HubUsageSinkConfig{
				Post: func(context.Context, telemetrycontract.UsageIngestRequest) (*telemetrycontract.UsageIngestResponse, error) {
					return &telemetrycontract.UsageIngestResponse{SuppressDirectOTel: tc.suppressDirect}, nil
				},
				RunID:              "run-metering-suppress",
				HubID:              "hub-metering-suppress",
				SuppressDirectOTel: tc.suppressDirect,
				DrainInterval:      time.Hour,
				HeartbeatInterval:  time.Hour,
			})

			ctx := context.Background()
			if err := p.Start(ctx); err != nil {
				t.Fatalf("Pipeline.Start: %v", err)
			}
			if p.SuppressDirectOTel() != tc.suppressDirect {
				t.Fatalf("SuppressDirectOTel() = %v, want %v", p.SuppressDirectOTel(), tc.suppressDirect)
			}
			if !p.NativeMeteringOnly() {
				t.Fatal("expected NativeMeteringOnly() = true")
			}

			if err := p.handleMetrics(ctx, mkBatch()); err != nil {
				t.Fatalf("handleMetrics: %v", err)
			}

			stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			_ = p.Stop(stopCtx)

			backend.mu.Lock()
			defer backend.mu.Unlock()
			names := exportedMetricNames(backend.metrics)

			if names["native.harness.metric"] != 0 {
				t.Fatalf("expected native.harness.metric dropped by metering-only, got %v", names)
			}
			if names["agent.tool.calls"] == 0 {
				t.Fatalf("expected sciontool non-canonical agent.tool.calls to pass both filters, got %v", names)
			}
			if tc.wantCanonical {
				if names[telemetrycontract.MetricAPICalls] == 0 || names[telemetrycontract.MetricUsageTokens] == 0 {
					t.Fatalf("expected sciontool canonical counters exported when suppress=false, got %v", names)
				}
			} else {
				if names[telemetrycontract.MetricAPICalls] != 0 || names[telemetrycontract.MetricUsageTokens] != 0 {
					t.Fatalf("expected sciontool canonical counters stripped when suppress=true, got %v", names)
				}
			}
		})
	}
}

// TestPipeline_LateActivationInitialSuppressWithoutHeartbeatEcho verifies that
// analyticsState.startDrainer's late-activation callback applies
// cfg.SuppressDirectOTel immediately upon probe activation without relying on a
// subsequent heartbeat response echoing SuppressDirectOTel=true. If the initial
// applySuppress(cfg.SuppressDirectOTel) call in analytics_drainer.go is removed,
// this test fails because the first heartbeat returns a transient error and
// never invokes OnSuppressChanged.
func TestPipeline_LateActivationInitialSuppressWithoutHeartbeatEcho(t *testing.T) {
	p := NewWithConfig(&Config{
		Enabled:      true,
		CloudEnabled: false,
		GRPCPort:     0,
		HTTPPort:     0,
	})
	home := t.TempDir()
	p.SetAnalyticsOutbox(home, -1, -1)
	p.SetHubIDResolver(func(context.Context) (string, error) { return "hub-late-init", nil })
	p.SetHubUsageProbeInterval(time.Millisecond)

	var postCalls atomic.Int32
	p.SetHubUsageProbe(func(context.Context) (*HubUsageSinkConfig, error) {
		return &HubUsageSinkConfig{
			Post: func(context.Context, telemetrycontract.UsageIngestRequest) (*telemetrycontract.UsageIngestResponse, error) {
				postCalls.Add(1)
				// Transient error: SendHeartbeat must NOT invoke OnSuppressChanged,
				// so only the initial applySuppress(cfg.SuppressDirectOTel) in
				// onHubActivated can flip suppression to true.
				return nil, errors.New("transient 503 during initial heartbeat")
			},
			RunID:              "run-late-init",
			HubID:              "hub-late-init",
			SuppressDirectOTel: true,
			DrainInterval:      time.Hour,
			HeartbeatInterval:  time.Millisecond,
		}, nil
	})

	ctx := context.Background()
	if err := p.Start(ctx); err != nil {
		t.Fatalf("Pipeline.Start: %v", err)
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		_ = p.Stop(stopCtx)
	}()

	if p.SuppressDirectOTel() {
		t.Fatal("expected SuppressDirectOTel=false before late activation")
	}

	// Start analyticsState in the default (GCP-only + hubProbe) branch directly
	// with a non-nil *GCPExporter stub so startDrainer takes default:.
	p.analytics.startDrainer(ctx, &GCPExporter{})

	deadline := time.Now().Add(2 * time.Second)
	for postCalls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if postCalls.Load() == 0 {
		t.Fatal("expected late-activated Hub sink to attempt initial heartbeat")
	}
	if !p.SuppressDirectOTel() {
		t.Fatal("expected SuppressDirectOTel=true from initial late-activation apply even when first heartbeat failed")
	}
	if !isHubOutboxSuppressDir(HubAnalyticsOutboxDirFor(home)) {
		t.Fatal("expected suppress.hub marker on disk from initial late-activation apply")
	}
}
