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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func sendClaudeUsageLogGRPC(t *testing.T, grpcPort int, model string, input, output, cacheRead int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := grpc.NewClient(
		"127.0.0.1:"+strconv.Itoa(grpcPort),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	defer func() { _ = conn.Close() }()

	client := collogspb.NewLogsServiceClient(conn)
	_, err = client.Export(ctx, &collogspb.ExportLogsServiceRequest{
		ResourceLogs: []*logspb.ResourceLogs{{
			ScopeLogs: []*logspb.ScopeLogs{{
				Scope: &commonpb.InstrumentationScope{Name: claudeUsageScope},
				LogRecords: []*logspb.LogRecord{{
					TimeUnixNano: uint64(time.Now().UTC().UnixNano()),
					Attributes: []*commonpb.KeyValue{
						{Key: "event.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "api_request"}}},
						{Key: "model", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: model}}},
						{Key: "input_tokens", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: input}}},
						{Key: "output_tokens", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: output}}},
						{Key: "cache_read_tokens", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: cacheRead}}},
					},
				}},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("OTLP gRPC Export logs: %v", err)
	}
}

// TestPipeline_HubUsageSinkStartsWhenExplicitCloudMissingDestination verifies
// that when Hub usage metering is active and cloud export was explicitly enabled
// without a destination, the pipeline still starts the Hub usage sink, outbox,
// and loopback receiver, surfaces the cloud misconfiguration via
// DeliveryState()=="degraded", and delivers native usage events to the Hub.
func TestPipeline_HubUsageSinkStartsWhenExplicitCloudMissingDestination(t *testing.T) {
	cloudDefaultEnv(t, "true") // explicit SCION_TELEMETRY_CLOUD_ENABLED=true, no destination
	t.Setenv("SCION_AGENT_ID", "agent-degraded-1")
	t.Setenv("SCION_HARNESS", "claude")
	t.Setenv("SCION_USAGE_SOURCE", "native")

	var mu sync.Mutex
	var gotEvents []telemetrycontract.UsageEvent
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req telemetrycontract.UsageIngestRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		gotEvents = append(gotEvents, req.Events...)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(telemetrycontract.UsageIngestResponse{Accepted: len(req.Events)})
	}))
	defer srv.Close()

	p := New()
	if p == nil {
		t.Fatal("New() = nil, want pipeline")
	}
	agentHome := t.TempDir()
	p.SetAnalyticsOutbox(agentHome, 0, 0)
	p.SetHubUsageSink(HubUsageSinkConfig{
		Post:          httptestPostFunc(srv.URL, "agent-degraded-1", "tok-1"),
		RunID:         "run-degraded-1",
		HubID:         "hub-1",
		DrainInterval: 50 * time.Millisecond,
	})

	ctx := context.Background()
	if err := p.Start(ctx); err != nil {
		t.Fatalf("Start() = %v, want nil when Hub usage sink is active", err)
	}
	if !p.IsRunning() {
		t.Fatal("pipeline not running after Start")
	}
	if p.exporter != nil {
		t.Error("expected nil cloud exporter when destination is missing")
	}
	if p.Config().CloudEnabled {
		t.Error("CloudEnabled must be cleared so loopback intake does not reject batches")
	}
	if got := p.DeliveryState(); got != "degraded" {
		t.Errorf("DeliveryState() = %q, want %q to surface cloud misconfiguration", got, "degraded")
	}
	if _, ok, reason := p.AnalyticsDiagnostics(); !ok || reason != "" {
		t.Fatalf("AnalyticsDiagnostics() = ok:%v reason:%q, want ok:true reason:\"\"", ok, reason)
	}

	sendClaudeUsageLogGRPC(t, p.Config().GRPCPort, "claude-haiku-4-5-20251001", 100, 25, 400)

	stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := p.Stop(stopCtx); err != nil {
		t.Fatalf("Stop() = %v", err)
	}
	if got := p.DeliveryState(); got != "degraded" {
		t.Errorf("DeliveryState() after Stop = %q, want %q", got, "degraded")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(gotEvents) != 1 {
		t.Fatalf("received %d usage events at Hub, want 1", len(gotEvents))
	}
	ev := gotEvents[0]
	if ev.Input == nil || *ev.Input != 100 || ev.Output == nil || *ev.Output != 25 || ev.CacheRead == nil || *ev.CacheRead != 400 {
		t.Errorf("unexpected token counts: input=%v output=%v cache_read=%v", ev.Input, ev.Output, ev.CacheRead)
	}
}

// TestPipeline_HubUsageSinkStartsWhenCloudExporterFails verifies that when
// NewCloudExporter fails (e.g. unreadable CA file), a pipeline with an active
// Hub usage sink degrades cloud export cleanly, reports DeliveryState()=="degraded",
// and continues metering native usage events to the Hub.
func TestPipeline_HubUsageSinkStartsWhenCloudExporterFails(t *testing.T) {
	cloudDefaultEnv(t, "true")
	t.Setenv(EnvEndpoint, "127.0.0.1:4317")
	t.Setenv(EnvCAFile, "/nonexistent/scion-test-ca.pem")
	t.Setenv("SCION_AGENT_ID", "agent-degraded-2")
	t.Setenv("SCION_HARNESS", "claude")
	t.Setenv("SCION_USAGE_SOURCE", "native")

	var mu sync.Mutex
	var gotEvents []telemetrycontract.UsageEvent
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req telemetrycontract.UsageIngestRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		gotEvents = append(gotEvents, req.Events...)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(telemetrycontract.UsageIngestResponse{Accepted: len(req.Events)})
	}))
	defer srv.Close()

	p := New()
	if p == nil {
		t.Fatal("New() = nil, want pipeline")
	}
	if !p.Config().IsCloudConfigured() {
		t.Fatal("precondition: expected IsCloudConfigured() == true so NewCloudExporter is attempted")
	}
	agentHome := t.TempDir()
	p.SetAnalyticsOutbox(agentHome, 0, 0)
	p.SetHubUsageSink(HubUsageSinkConfig{
		Post:          httptestPostFunc(srv.URL, "agent-degraded-2", "tok-1"),
		RunID:         "run-degraded-2",
		HubID:         "hub-1",
		DrainInterval: 50 * time.Millisecond,
	})

	ctx := context.Background()
	if err := p.Start(ctx); err != nil {
		t.Fatalf("Start() = %v, want nil when Hub usage sink is active despite cloud exporter failure", err)
	}
	if !p.IsRunning() {
		t.Fatal("pipeline not running after Start")
	}
	if p.exporter != nil {
		t.Error("expected nil cloud exporter after NewCloudExporter failure")
	}
	if got := p.DeliveryState(); got != "degraded" {
		t.Errorf("DeliveryState() = %q, want %q", got, "degraded")
	}

	sendClaudeUsageLogGRPC(t, p.Config().GRPCPort, "claude-haiku-4-5-20251001", 42, 13, 0)

	stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := p.Stop(stopCtx); err != nil {
		t.Fatalf("Stop() = %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(gotEvents) != 1 {
		t.Fatalf("received %d usage events at Hub, want 1", len(gotEvents))
	}
	if gotEvents[0].Input == nil || *gotEvents[0].Input != 42 || gotEvents[0].Output == nil || *gotEvents[0].Output != 13 {
		t.Errorf("unexpected token counts: input=%v output=%v", gotEvents[0].Input, gotEvents[0].Output)
	}
}

// TestNew_ExplicitDisabledVsDefaultUnset distinguishes an explicit
// SCION_TELEMETRY_ENABLED=false (pipeline nil) from unset default (pipeline
// created with Enabled=true, CloudEnabledExplicit=false).
func TestNew_ExplicitDisabledVsDefaultUnset(t *testing.T) {
	t.Run("unset default creates pipeline", func(t *testing.T) {
		clearTelemetryEnv()
		t.Cleanup(SetTelemetryTestSandboxed())
		t.Setenv("HOME", t.TempDir())
		t.Setenv(EnvEnabled, "")
		if err := os.Unsetenv(EnvEnabled); err != nil {
			t.Fatal(err)
		}
		p := New()
		if p == nil {
			t.Fatal("New() = nil on unset default, want non-nil pipeline")
		}
		if !p.Config().Enabled || p.Config().CloudEnabledExplicit {
			t.Errorf("unexpected default config: Enabled=%v CloudEnabledExplicit=%v", p.Config().Enabled, p.Config().CloudEnabledExplicit)
		}
	})

	t.Run("explicit false returns nil", func(t *testing.T) {
		clearTelemetryEnv()
		t.Cleanup(SetTelemetryTestSandboxed())
		t.Setenv("HOME", t.TempDir())
		t.Setenv(EnvEnabled, "false")
		if p := New(); p != nil {
			t.Fatal("New() != nil on explicit SCION_TELEMETRY_ENABLED=false, want nil")
		}
	})
}
