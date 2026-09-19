package telemetry

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	mexporter "github.com/GoogleCloudPlatform/opentelemetry-operations-go/exporter/metric"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/api/option"
	labelpb "google.golang.org/genproto/googleapis/api/label"
	googlemetricpb "google.golang.org/genproto/googleapis/api/metric"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

func TestGCPMetricIdentityPrivacyAndCanonicalLabels(t *testing.T) {
	makeInput := func(value *commonpb.AnyValue) *metricpb.ResourceMetrics {
		input := testMetricResource("native", "scope", "1", "a", testNumber("calls", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, 1, 2, 1, &commonpb.KeyValue{Key: "operation", Value: value}))
		return input
	}
	stringValue := &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "1"}}
	intValue := &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 1}}
	stringOut, err := gcpIdentityMetrics([]*metricpb.ResourceMetrics{makeInput(stringValue)})
	if err != nil {
		t.Fatal(err)
	}
	intOut, err := gcpIdentityMetrics([]*metricpb.ResourceMetrics{makeInput(intValue)})
	if err != nil {
		t.Fatal(err)
	}
	stringAttrs := stringOut[0].ScopeMetrics[0].Metrics[0].GetSum().DataPoints[0].Attributes
	intAttrs := intOut[0].ScopeMetrics[0].Metrics[0].GetSum().DataPoints[0].Attributes
	if attrValue(stringAttrs, gcpPointIDLabel) == attrValue(intAttrs, gcpPointIDLabel) {
		t.Fatal("typed point values share Cloud digest")
	}
	if attrValue(stringAttrs, gcpAgentLabel) != "agent" || attrValue(stringAttrs, gcpProjectLabel) != "project" {
		t.Fatal("missing authoritative readable labels")
	}
	unmapped := makeInput(stringValue)
	unmapped.ScopeMetrics[0].Metrics[0].GetSum().DataPoints[0].Attributes = append(unmapped.ScopeMetrics[0].Metrics[0].GetSum().DataPoints[0].Attributes, metricStringLabel("unmapped", "PRIVATE-MARKER"))
	if _, err := gcpIdentityMetrics([]*metricpb.ResourceMetrics{unmapped}); err == nil || strings.Contains(err.Error(), "PRIVATE-MARKER") {
		t.Fatalf("unmapped point handling = %v", err)
	}
	nested := func(reverse bool) *commonpb.AnyValue {
		values := []*commonpb.KeyValue{metricStringLabel("a", "1"), metricStringLabel("b", "2")}
		if reverse {
			values[0], values[1] = values[1], values[0]
		}
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{Values: values}}}
	}
	a, err := gcpIdentityMetrics([]*metricpb.ResourceMetrics{makeInput(nested(false))})
	if err != nil {
		t.Fatal(err)
	}
	b, err := gcpIdentityMetrics([]*metricpb.ResourceMetrics{makeInput(nested(true))})
	if err != nil {
		t.Fatal(err)
	}
	if attrValue(a[0].ScopeMetrics[0].Metrics[0].GetSum().DataPoints[0].Attributes, gcpPointIDLabel) != attrValue(b[0].ScopeMetrics[0].Metrics[0].GetSum().DataPoints[0].Attributes, gcpPointIDLabel) {
		t.Fatal("nested map order changed Cloud digest")
	}

	private := makeInput(stringValue)
	private.ScopeMetrics[0].Metrics[0].GetSum().DataPoints[0].Attributes = append(private.ScopeMetrics[0].Metrics[0].GetSum().DataPoints[0].Attributes, metricStringLabel("conversation.id", "PRIVATE-MARKER"))
	if _, err := gcpIdentityMetrics([]*metricpb.ResourceMetrics{private}); err == nil || strings.Contains(err.Error(), "PRIVATE-MARKER") {
		t.Fatalf("private point handling = %v", err)
	}
	if err := newMetricStreams().add([]*metricpb.ResourceMetrics{private}); err != nil {
		t.Fatalf("generic processed stream rejected: %v", err)
	}
	missing := makeInput(stringValue)
	missing.Resource.Attributes = []*commonpb.KeyValue{metricStringLabel("service.name", "native")}
	without, err := gcpIdentityMetrics([]*metricpb.ResourceMetrics{missing})
	if err != nil {
		t.Fatal(err)
	}
	attrs := without[0].ScopeMetrics[0].Metrics[0].GetSum().DataPoints[0].Attributes
	if attrValue(attrs, gcpAgentLabel) != "" || attrValue(attrs, gcpProjectLabel) != "" {
		t.Fatal("invented authority when resource identity absent")
	}
}

func TestCloudRejectsUnknownAndNestedForbiddenDimensionsOnFirstArrival(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*metricpb.ResourceMetrics)
	}{
		{"unknown resource", func(rm *metricpb.ResourceMetrics) {
			rm.Resource.Attributes = append(rm.Resource.Attributes, metricStringLabel("tenant.region", "west"))
		}},
		{"unknown scope", func(rm *metricpb.ResourceMetrics) {
			rm.ScopeMetrics[0].Scope.Attributes = []*commonpb.KeyValue{metricStringLabel("tenant.region", "west")}
		}},
		{"unknown point", func(rm *metricpb.ResourceMetrics) {
			rm.ScopeMetrics[0].Metrics[0].GetSum().DataPoints[0].Attributes = []*commonpb.KeyValue{metricStringLabel("tenant.region", "west")}
		}},
		{"nested resource", func(rm *metricpb.ResourceMetrics) {
			rm.Resource.Attributes = append(rm.Resource.Attributes, &commonpb.KeyValue{Key: "service.namespace", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{Values: []*commonpb.KeyValue{metricStringLabel("prompt", "redacted")}}}}})
		}},
		{"nested point", func(rm *metricpb.ResourceMetrics) {
			rm.ScopeMetrics[0].Metrics[0].GetSum().DataPoints[0].Attributes = []*commonpb.KeyValue{{Key: "operation", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{Values: []*commonpb.KeyValue{metricStringLabel("conversation.id", "already-policy-hashed-session")}}}}}}
		}},
		{"array nested scope", func(rm *metricpb.ResourceMetrics) {
			rm.ScopeMetrics[0].Scope.Attributes = []*commonpb.KeyValue{{Key: "component", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{Values: []*commonpb.AnyValue{{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{Values: []*commonpb.KeyValue{metricStringLabel("payload", "redacted")}}}}}}}}}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := testMetricResource("native", "scope", "", "", testNumber("agent.tool.calls", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, 1, 2, 1))
			tc.change(input)
			s := newMetricStreams()
			s.gcp = true
			if err := s.add([]*metricpb.ResourceMetrics{input}); err == nil {
				t.Fatal("first Cloud admission accepted unsupported dimension")
			}
			if len(s.streams) != 0 {
				t.Fatal("unsupported dimension reached stream state")
			}
			if _, err := gcpIdentityMetrics([]*metricpb.ResourceMetrics{input}); err == nil {
				t.Fatal("Cloud adapter discarded unsupported dimension")
			}
			generic := newMetricStreams()
			if err := generic.add([]*metricpb.ResourceMetrics{input}); err != nil {
				t.Fatalf("generic OTLP lost processed dimension: %v", err)
			}
		})
	}
}

func TestCloudGCPProjectResourceIdentity(t *testing.T) {
	makeInput := func() *metricpb.ResourceMetrics {
		return testMetricResource("native", "scope", "", "", testNumber("agent.tool.calls", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, 1, 2, 1))
	}
	base := makeInput()
	first := makeInput()
	first.Resource.Attributes = append(first.Resource.Attributes, metricStringLabel("gcp.project_id", "cloud-one"))
	second := makeInput()
	second.Resource.Attributes = append(second.Resource.Attributes, metricStringLabel("gcp.project_id", "cloud-two"))
	ids := map[string]bool{}
	streams := newMetricStreams()
	streams.gcp = true
	for _, input := range []*metricpb.ResourceMetrics{base, first, second} {
		if err := streams.add([]*metricpb.ResourceMetrics{input}); err != nil {
			t.Fatalf("Cloud project stream admission: %v", err)
		}
		out, err := gcpIdentityMetrics([]*metricpb.ResourceMetrics{input})
		if err != nil {
			t.Fatal(err)
		}
		attrs := out[0].ScopeMetrics[0].Metrics[0].GetSum().DataPoints[0].Attributes
		ids[attrValue(attrs, gcpResourceIDLabel)] = true
		if attrValue(attrs, "gcp.project_id") != "" {
			t.Fatal("Cloud destination project became a readable label")
		}
	}
	if len(ids) != 3 || len(streams.streams) != 3 {
		t.Fatalf("absent and distinct Cloud project identities collapsed: digests=%d streams=%d", len(ids), len(streams.streams))
	}
	malformed := makeInput()
	malformed.Resource.Attributes = append(malformed.Resource.Attributes, &commonpb.KeyValue{Key: "gcp.project_id", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{}}}})
	if _, err := gcpIdentityMetrics([]*metricpb.ResourceMetrics{malformed}); err == nil {
		t.Fatal("nested Cloud project accepted")
	}
	malformed.Resource.Attributes[len(malformed.Resource.Attributes)-1].Value = &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 1}}
	if _, err := gcpIdentityMetrics([]*metricpb.ResourceMetrics{malformed}); err == nil {
		t.Fatal("typed Cloud project accepted")
	}
	malformed.Resource.Attributes[len(malformed.Resource.Attributes)-1].Value = &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: strings.Repeat("x", 257)}}
	if _, err := gcpIdentityMetrics([]*metricpb.ResourceMetrics{malformed}); err == nil {
		t.Fatal("overlong Cloud project accepted")
	}
}

func TestCloudSelfMetricPointFieldsAreNameScopedAndBounded(t *testing.T) {
	for _, tc := range []struct {
		name   string
		metric *metricpb.Metric
	}{
		{"status", &metricpb.Metric{Name: "scion.telemetry.pipeline.status", Data: &metricpb.Metric_Gauge{Gauge: &metricpb.Gauge{DataPoints: []*metricpb.NumberDataPoint{{TimeUnixNano: 2, Value: &metricpb.NumberDataPoint_AsInt{AsInt: 1}, Attributes: []*commonpb.KeyValue{metricStringLabel("scion.telemetry.provider", "gcp"), metricStringLabel("scion.telemetry.project_id", "cloud-project")}}}}}}},
		{"export error", testNumber("scion.telemetry.export.errors", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, 1, 2, 1, metricStringLabel("signal", "metrics"), metricStringLabel("error_type", "auth"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := testMetricResource("sciontool", pipelineMetricScope, "", "", tc.metric)
			s := newMetricStreams()
			s.gcp = true
			if err := s.add([]*metricpb.ResourceMetrics{input}); err != nil {
				t.Fatal(err)
			}
			if _, err := gcpIdentityMetrics([]*metricpb.ResourceMetrics{input}); err != nil {
				t.Fatal(err)
			}
			wrongScope := proto.Clone(input).(*metricpb.ResourceMetrics)
			wrongScope.ScopeMetrics[0].Scope.Name = "native.scope"
			if err := s.add([]*metricpb.ResourceMetrics{wrongScope}); err == nil {
				t.Fatal("self metric labels accepted outside internal scope")
			}
			malformed := proto.Clone(input).(*metricpb.ResourceMetrics)
			var attrs []*commonpb.KeyValue
			if tc.metric.GetGauge() != nil {
				attrs = malformed.ScopeMetrics[0].Metrics[0].GetGauge().DataPoints[0].Attributes
			} else {
				attrs = malformed.ScopeMetrics[0].Metrics[0].GetSum().DataPoints[0].Attributes
			}
			attrs[0].Value = &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 1}}
			if err := s.add([]*metricpb.ResourceMetrics{malformed}); err == nil {
				t.Fatal("typed self metric dimension admitted")
			}
		})
	}
	badSignal := testMetricResource("sciontool", pipelineMetricScope, "", "", testNumber("scion.telemetry.export.errors", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, 1, 2, 1, metricStringLabel("signal", "raw-error"), metricStringLabel("error_type", "other")))
	if _, err := gcpIdentityMetrics([]*metricpb.ResourceMetrics{badSignal}); err == nil {
		t.Fatal("unbounded diagnostic signal admitted")
	}
}

type monitoringCapture struct {
	monitoringpb.UnimplementedMetricServiceServer
	mu                 sync.Mutex
	descriptors        []*googlemetricpb.MetricDescriptor
	series             []*monitoringpb.CreateTimeSeriesRequest
	existingDescriptor *googlemetricpb.MetricDescriptor
	rejectUnknown      bool
}

func (c *monitoringCapture) GetMetricDescriptor(context.Context, *monitoringpb.GetMetricDescriptorRequest) (*googlemetricpb.MetricDescriptor, error) {
	if c.existingDescriptor != nil {
		return c.existingDescriptor, nil
	}
	return nil, status.Error(codes.NotFound, "test descriptor")
}

func (c *monitoringCapture) CreateMetricDescriptor(_ context.Context, req *monitoringpb.CreateMetricDescriptorRequest) (*googlemetricpb.MetricDescriptor, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.descriptors = append(c.descriptors, proto.Clone(req.MetricDescriptor).(*googlemetricpb.MetricDescriptor))
	return req.MetricDescriptor, nil
}

func (c *monitoringCapture) CreateTimeSeries(_ context.Context, req *monitoringpb.CreateTimeSeriesRequest) (*emptypb.Empty, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rejectUnknown && c.existingDescriptor != nil {
		allowed := map[string]bool{}
		for _, label := range c.existingDescriptor.Labels {
			allowed[label.Key] = true
		}
		for _, ts := range req.TimeSeries {
			for key := range ts.Metric.Labels {
				if !allowed[key] {
					return nil, status.Error(codes.InvalidArgument, "unrecognized metric label")
				}
			}
		}
	}
	c.series = append(c.series, proto.Clone(req).(*monitoringpb.CreateTimeSeriesRequest))
	return &emptypb.Empty{}, nil
}

func TestGCPMonitoringPropagatesExistingDescriptorMismatch(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	capture := &monitoringCapture{rejectUnknown: true, existingDescriptor: &googlemetricpb.MetricDescriptor{Type: "workload.googleapis.com/agent.tool.calls", Labels: []*labelpb.LabelDescriptor{{Key: "service_name"}, {Key: "agent_id"}, {Key: "project_id"}, {Key: "harness"}, {Key: "tool_name"}, {Key: "status"}}}}
	server := grpc.NewServer()
	monitoringpb.RegisterMetricServiceServer(server, capture)
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	sdkExporter, err := mexporter.New(mexporter.WithProjectID("test-project"), mexporter.WithMonitoringClientOptions(option.WithGRPCConn(conn)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sdkExporter.Shutdown(context.Background()) }()
	exporter := &GCPExporter{metricExporter: sdkExporter}
	input := testMetricResource("native", hookMetricScope, "", "", testNumber("agent.tool.calls", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, 1000000000, 2000000000, 1, metricStringLabel("tool_name", "Bash")))
	if err := exporter.ExportProtoMetrics(context.Background(), []*metricpb.ResourceMetrics{input}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("mismatch export error = %v", err)
	}
}

func TestGCPMonitoringDescriptorAndTimeSeriesIdentity(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	capture := &monitoringCapture{}
	monitoringpb.RegisterMetricServiceServer(server, capture)
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	sdkExporter, err := mexporter.New(mexporter.WithProjectID("test-project"), mexporter.WithMonitoringClientOptions(option.WithGRPCConn(conn)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sdkExporter.Shutdown(context.Background()) }()
	exporter := &GCPExporter{metricExporter: sdkExporter}
	hookLabels := func() []*commonpb.KeyValue {
		return []*commonpb.KeyValue{metricStringLabel("agent_id", "agent"), metricStringLabel("project_id", "project"), metricStringLabel("harness", "claude"), metricStringLabel("status", "success"), metricStringLabel("tool_name", "Bash")}
	}
	input := []*metricpb.ResourceMetrics{
		testMetricResource("native-a", "scope-one", "1", "a", testNumber("agent.tool.calls", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, 1000000000, 2000000000, 7, hookLabels()...)),
		testMetricResource("native-b", "scope-one", "1", "a", testNumber("agent.tool.calls", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, 1000000000, 3000000000, 8, hookLabels()...)),
		testMetricResource("native-a", "scope-two", "2", "b", testNumber("agent.tool.calls", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, 1000000000, 4000000000, 9, hookLabels()...)),
	}
	if err := exporter.ExportProtoMetrics(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	capture.mu.Lock()
	defer capture.mu.Unlock()
	if len(capture.series) != 3 || len(capture.descriptors) != 2 {
		t.Fatalf("descriptors=%d writes=%d", len(capture.descriptors), len(capture.series))
	}
	resourceIDs := map[string]bool{}
	scopeIDs := map[string]bool{}
	for _, write := range capture.series {
		if len(write.TimeSeries) != 1 {
			t.Fatalf("time series = %d", len(write.TimeSeries))
		}
		ts := write.TimeSeries[0]
		if ts.Metric.Type != "workload.googleapis.com/agent.tool.calls" {
			t.Fatalf("type = %q", ts.Metric.Type)
		}
		labels := ts.Metric.Labels
		for _, key := range []string{"service_name", "service_instance_id", "agent_id", "project_id", "harness", "status", "tool_name", gcpResourceIDLabel, gcpScopeIDLabel, gcpPointIDLabel, gcpAgentLabel, gcpProjectLabel} {
			if labels[key] == "" {
				t.Fatalf("missing %s in %v", key, labels)
			}
		}
		if len(labels) != 12 {
			t.Fatalf("wire label count = %d: %v", len(labels), labels)
		}
		resourceIDs[labels[gcpResourceIDLabel]] = true
		scopeIDs[labels[gcpScopeIDLabel]] = true
		if ts.Points[0].Interval.StartTime.Seconds != 1 {
			t.Fatalf("start = %v", ts.Points[0].Interval)
		}
	}
	if len(resourceIDs) != 3 || len(scopeIDs) != 2 {
		t.Fatalf("resource IDs=%d scope IDs=%d", len(resourceIDs), len(scopeIDs))
	}
	for _, descriptor := range capture.descriptors {
		if descriptor.MetricKind != googlemetricpb.MetricDescriptor_CUMULATIVE || descriptor.ValueType != googlemetricpb.MetricDescriptor_INT64 {
			t.Fatalf("descriptor kind/type = %v/%v", descriptor.MetricKind, descriptor.ValueType)
		}
		found := map[string]bool{}
		for _, label := range descriptor.Labels {
			found[label.Key] = true
		}
		if len(descriptor.Labels) != 12 {
			t.Fatalf("proposed descriptor labels = %d", len(descriptor.Labels))
		}
		for _, key := range []string{gcpResourceIDLabel, gcpScopeIDLabel, gcpPointIDLabel, gcpAgentLabel, gcpProjectLabel} {
			if !found[key] {
				t.Fatalf("descriptor missing %s", key)
			}
		}
	}
}
