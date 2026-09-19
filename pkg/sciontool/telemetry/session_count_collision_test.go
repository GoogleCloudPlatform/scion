package telemetry

import (
	"context"
	"net"
	"reflect"
	"testing"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	mexporter "github.com/GoogleCloudPlatform/opentelemetry-operations-go/exporter/metric"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/api/option"
	googlemetricpb "google.golang.org/genproto/googleapis/api/metric"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestLifecycleAndHookSessionCountsUseDistinctCloudSeries(t *testing.T) {
	for k, v := range map[string]string{"SCION_AGENT_ID": "probe-agent", "SCION_PROJECT_ID": "probe-project", "SCION_HARNESS": "synthetic"} {
		t.Setenv(k, v)
	}
	cfg := &Config{Enabled: true, CloudProvider: "gcp", GRPCPort: availableTCPPort(t)}
	raw := make(chan *metricpb.ResourceMetrics, 4)
	receiver := NewReceiver(cfg, nil, WithMetricHandler(func(_ context.Context, rms []*metricpb.ResourceMetrics) error {
		for _, rm := range rms {
			raw <- rm
		}
		return nil
	}))
	if err := receiver.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer receiver.Stop(context.Background())
	initP, err := NewProviders(context.Background(), cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	initC, err := initP.MeterProvider.Meter(LifecycleMetricScope).Int64Counter("agent.session.count", metric.WithUnit("{session}"))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	hookP, err := NewProviders(context.Background(), cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	hookC, err := hookP.MeterProvider.Meter(hookMetricScope).Int64Counter("agent.session.count", metric.WithUnit("{session}"))
	if err != nil {
		t.Fatal(err)
	}
	attrs := metric.WithAttributes(attribute.String("agent_id", "probe-agent"), attribute.String("project_id", "probe-project"), attribute.String("harness", "synthetic"), attribute.String("status", "completed"))
	hookC.Add(context.Background(), 1, attrs)
	if err := hookP.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	initC.Add(context.Background(), 1, attrs)
	if err := initP.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	input := make([]*metricpb.ResourceMetrics, 0, 2)
	for len(input) < 2 {
		select {
		case rm := <-raw:
			input = append(input, rm)
		case <-time.After(5 * time.Second):
			t.Fatal("missing OTLP")
		}
	}
	var hook, init *metricpb.ResourceMetrics
	for _, rm := range input {
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if m.Name != "agent.session.count" {
					continue
				}
				if m.GetSum().AggregationTemporality == metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA {
					hook = rm
				} else {
					init = rm
				}
			}
		}
	}
	if hook == nil || init == nil {
		t.Fatalf("missing hook or init: %v", input)
	}
	if hook.ScopeMetrics[0].Scope.Name != hookMetricScope || init.ScopeMetrics[0].Scope.Name != LifecycleMetricScope {
		t.Fatalf("provider metric scopes: hook=%q init=%q", hook.ScopeMetrics[0].Scope.Name, init.ScopeMetrics[0].Scope.Name)
	}
	h := hook.ScopeMetrics[0].Metrics[0].GetSum().DataPoints[0]
	i := init.ScopeMetrics[0].Metrics[0].GetSum().DataPoints[0]
	t.Logf("actual provider OTLP: hook temporal=%v start=%d end=%d value=%d; init temporal=%v start=%d end=%d value=%d", hook.ScopeMetrics[0].Metrics[0].GetSum().AggregationTemporality, h.StartTimeUnixNano, h.TimeUnixNano, h.GetAsInt(), init.ScopeMetrics[0].Metrics[0].GetSum().AggregationTemporality, i.StartTimeUnixNano, i.TimeUnixNano, i.GetAsInt())
	if i.StartTimeUnixNano >= h.StartTimeUnixNano {
		t.Fatalf("expected init earlier start: init=%d hook=%d", i.StartTimeUnixNano, h.StartTimeUnixNano)
	}
	policy := newReceiverPolicy(cfg)
	hD := policy.processMetrics([]*metricpb.ResourceMetrics{hook})
	iD := policy.processMetrics([]*metricpb.ResourceMetrics{init})
	if hD.Reason != "" || iD.Reason != "" {
		t.Fatalf("policy: hook=%q init=%q", hD.Reason, iD.Reason)
	}
	state := newMetricStreams()
	state.gcp = true
	if err := state.add(hD.Data); err != nil {
		t.Fatal(err)
	}
	first := state.snapshot()
	state.clearPendingMarker()
	if err := state.add(iD.Data); err != nil {
		t.Fatalf("second admitted? %v", err)
	}
	second := state.snapshot()
	t.Logf("state after both: internal streams=%d Cloud identities=%d", len(state.streams), len(state.cloudIdentities))
	if len(state.streams) != 2 || len(state.cloudIdentities) != 2 {
		t.Fatal("distinct source identities collapsed")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	capture := &monitoringCapture{}
	server := grpc.NewServer()
	monitoringpb.RegisterMetricServiceServer(server, capture)
	go server.Serve(listener)
	defer server.Stop()
	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	sdk, err := mexporter.New(mexporter.WithProjectID("test-project"), mexporter.WithMonitoringClientOptions(option.WithGRPCConn(conn)))
	if err != nil {
		t.Fatal(err)
	}
	defer sdk.Shutdown(context.Background())
	exp := &GCPExporter{metricExporter: sdk}
	if err := exp.ExportProtoMetrics(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if err := exp.ExportProtoMetrics(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	capture.mu.Lock()
	defer capture.mu.Unlock()
	if len(capture.series) != 2 {
		t.Fatalf("writes=%d", len(capture.series))
	}
	if len(capture.descriptors) == 0 {
		t.Fatal("missing session descriptor request")
	}
	for _, descriptor := range capture.descriptors {
		if descriptor.Type != "workload.googleapis.com/agent.session.count" || descriptor.MetricKind != googlemetricpb.MetricDescriptor_CUMULATIVE || descriptor.ValueType != googlemetricpb.MetricDescriptor_INT64 || descriptor.Unit != "{session}" {
			t.Fatalf("session descriptor changed: %+v", descriptor)
		}
	}
	a, b := capture.series[0].TimeSeries[0], capture.series[1].TimeSeries[0]
	t.Logf("Monitoring writes: first type=%q labels=%v start=%v end=%v value=%v; second type=%q labels=%v start=%v end=%v value=%v", a.Metric.Type, a.Metric.Labels, a.Points[0].Interval.StartTime, a.Points[0].Interval.EndTime, a.Points[0].Value, b.Metric.Type, b.Metric.Labels, b.Points[0].Interval.StartTime, b.Points[0].Interval.EndTime, b.Points[0].Value)
	if a.Metric.Type != b.Metric.Type || a.Metric.Type != "workload.googleapis.com/agent.session.count" || !reflect.DeepEqual(a.Resource, b.Resource) {
		t.Fatal("metric name or monitored resource changed")
	}
	if reflect.DeepEqual(a.Metric.Labels, b.Metric.Labels) || a.Metric.Labels[gcpScopeIDLabel] == b.Metric.Labels[gcpScopeIDLabel] {
		t.Fatal("scope digest did not separate Cloud series")
	}
	for key, value := range a.Metric.Labels {
		if key != gcpScopeIDLabel && b.Metric.Labels[key] != value {
			t.Fatalf("non-scope label %s changed: %q vs %q", key, value, b.Metric.Labels[key])
		}
	}
	if a.Points[0].Interval.StartTime.AsTime().UnixNano() != int64(h.StartTimeUnixNano) || b.Points[0].Interval.StartTime.AsTime().UnixNano() != int64(i.StartTimeUnixNano) {
		t.Fatal("provider start timestamps changed")
	}
	if a.Points[0].Value.GetInt64Value() != 1 || b.Points[0].Value.GetInt64Value() != 1 {
		t.Fatal("provider values changed")
	}
}
