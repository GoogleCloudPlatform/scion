package commands

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/telemetry"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

func TestInitLifecycleRegistrationEmitsOnlyLifecycleSessionMetric(t *testing.T) {
	t.Setenv("SCION_HOOKS_DIR", t.TempDir())
	t.Setenv("SCION_AGENT_ID", "init-test-agent")
	t.Setenv("SCION_PROJECT_ID", "init-test-project")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	cfg := &telemetry.Config{Enabled: true, GRPCPort: port}
	metrics := make(chan *metricpb.ResourceMetrics, 4)
	spans := make(chan *tracepb.ResourceSpans, 8)
	logs := make(chan *logspb.ResourceLogs, 8)
	receiver := telemetry.NewReceiver(cfg, func(_ context.Context, batches []*tracepb.ResourceSpans) error {
		for _, rs := range batches {
			spans <- rs
		}
		return nil
	}, telemetry.WithMetricHandler(func(_ context.Context, batches []*metricpb.ResourceMetrics) error {
		for _, rm := range batches {
			metrics <- rm
		}
		return nil
	}), telemetry.WithLogHandler(func(_ context.Context, batches []*logspb.ResourceLogs) error {
		for _, rl := range batches {
			logs <- rl
		}
		return nil
	}))
	if err := receiver.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer receiver.Stop(context.Background())
	providers, err := telemetry.NewProviders(context.Background(), cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	manager := hooks.NewLifecycleManager()
	registerLifecycleTelemetryHandler(manager, providers, nil)
	for _, name := range []string{hooks.EventPreStart, hooks.EventPostStart, hooks.EventPreStop, hooks.EventSessionEnd} {
		if len(manager.Handlers[name]) != 1 {
			t.Fatalf("%s registrations=%d", name, len(manager.Handlers[name]))
		}
	}
	for _, run := range []func() error{manager.RunPreStart, manager.RunPostStart, manager.RunPreStop, manager.RunSessionEnd} {
		if err := run(); err != nil {
			t.Fatal(err)
		}
	}
	if err := providers.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case rm := <-metrics:
		if len(rm.ScopeMetrics) != 1 || rm.ScopeMetrics[0].Scope.Name != telemetry.LifecycleMetricScope || len(rm.ScopeMetrics[0].Metrics) != 1 || rm.ScopeMetrics[0].Metrics[0].Name != "agent.session.count" {
			t.Fatalf("init lifecycle metrics=%v", rm.ScopeMetrics)
		}
		sum := rm.ScopeMetrics[0].Metrics[0].GetSum()
		if sum == nil || sum.AggregationTemporality != metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE || len(sum.DataPoints) != 1 || sum.DataPoints[0].GetAsInt() != 1 {
			t.Fatalf("init session counter=%v", sum)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no init lifecycle metric")
	}
	select {
	case rs := <-spans:
		if len(rs.ScopeSpans) == 0 || rs.ScopeSpans[0].Scope.Name != "github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/handlers" {
			t.Fatalf("trace scope changed: %v", rs.ScopeSpans)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no lifecycle spans")
	}
	select {
	case rl := <-logs:
		if len(rl.ScopeLogs) == 0 || rl.ScopeLogs[0].Scope.Name != "sciontool.hooks" {
			t.Fatalf("log scope changed: %v", rl.ScopeLogs)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no lifecycle logs")
	}
}
