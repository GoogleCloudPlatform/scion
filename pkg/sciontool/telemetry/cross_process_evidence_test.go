package telemetry

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	colLogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colMetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	colTrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// This sink is deliberately local: native-shaped input here is synthetic and
// does not establish installed-vendor emission or Cloud delivery.
type phase5Sink struct {
	colLogs.UnimplementedLogsServiceServer
	colMetrics.UnimplementedMetricsServiceServer
	colTrace.UnimplementedTraceServiceServer
	mu       sync.Mutex
	logs     []*colLogs.ExportLogsServiceRequest
	metrics  []*colMetrics.ExportMetricsServiceRequest
	traces   []*colTrace.ExportTraceServiceRequest
	failLogs bool
}

func (s *phase5Sink) Export(ctx context.Context, request *colLogs.ExportLogsServiceRequest) (*colLogs.ExportLogsServiceResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failLogs {
		return nil, status.Error(codes.PermissionDenied, "synthetic sink rejection")
	}
	s.logs = append(s.logs, proto.Clone(request).(*colLogs.ExportLogsServiceRequest))
	return &colLogs.ExportLogsServiceResponse{}, nil
}

// The three generated services share Export, so separate adapters keep the
// capture implementation explicit.
type phase5Metrics struct {
	colMetrics.UnimplementedMetricsServiceServer
	sink *phase5Sink
}

func (m *phase5Metrics) Export(_ context.Context, request *colMetrics.ExportMetricsServiceRequest) (*colMetrics.ExportMetricsServiceResponse, error) {
	m.sink.mu.Lock()
	defer m.sink.mu.Unlock()
	m.sink.metrics = append(m.sink.metrics, proto.Clone(request).(*colMetrics.ExportMetricsServiceRequest))
	return &colMetrics.ExportMetricsServiceResponse{}, nil
}

type phase5Traces struct {
	colTrace.UnimplementedTraceServiceServer
	sink *phase5Sink
}

func (m *phase5Traces) Export(_ context.Context, request *colTrace.ExportTraceServiceRequest) (*colTrace.ExportTraceServiceResponse, error) {
	m.sink.mu.Lock()
	defer m.sink.mu.Unlock()
	m.sink.traces = append(m.sink.traces, proto.Clone(request).(*colTrace.ExportTraceServiceRequest))
	return &colTrace.ExportTraceServiceResponse{}, nil
}

func phase5String(value string) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: value}}
}
func phase5Attr(key, value string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: phase5String(value)}
}

func phase5CleanEnv() []string {
	var env []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "SCION_") || strings.HasPrefix(key, "OTEL_") || strings.HasPrefix(key, "GOOGLE_") || key == "CLOUDSDK_CORE_PROJECT" {
			continue
		}
		env = append(env, entry)
	}
	return env
}

func phase5BuildTool(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../../.."))
	path := filepath.Join(t.TempDir(), "sciontool")
	cmd := exec.Command("go", "build", "-buildvcs=false", "-o", path, "./cmd/sciontool")
	cmd.Dir = root
	cmd.Env = phase5CleanEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build sciontool: %v\n%s", err, out)
	}
	return path
}

func phase5NativeLog(event string) *colLogs.ExportLogsServiceRequest {
	return &colLogs.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
			phase5Attr("service.name", "claude-code"), phase5Attr("scion.agent.id", "spoofed"),
		}},
		ScopeLogs: []*logspb.ScopeLogs{{Scope: &commonpb.InstrumentationScope{Name: "com.anthropic.claude_code.events"},
			LogRecords: []*logspb.LogRecord{{EventName: event, Body: phase5String("safe-positive-marker"),
				Attributes: []*commonpb.KeyValue{phase5Attr("response", "synthetic-private-response"), phase5Attr("request_id", "synthetic-private-id"), phase5Attr("safe.marker", "visible")},
			}},
		}},
	}}}
}

func phase5PostLog(t *testing.T, addr string, request *colLogs.ExportLogsServiceRequest, wantStatus int) {
	t.Helper()
	body, err := proto.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.Post("http://"+addr+"/v1/logs", "application/x-protobuf", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != wantStatus {
		t.Fatalf("native-shaped OTLP status %d", response.StatusCode)
	}
}

func TestPhase5CrossProcessReceiverEvidence(t *testing.T) {
	t.Setenv("SCION_AGENT_ID", "authoritative-agent")
	t.Setenv("SCION_PROJECT_ID", "authoritative-project")
	t.Setenv("SCION_HARNESS", "claude")
	tool := phase5BuildTool(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sink := &phase5Sink{}
	server := grpc.NewServer()
	colLogs.RegisterLogsServiceServer(server, sink)
	colMetrics.RegisterMetricsServiceServer(server, &phase5Metrics{sink: sink})
	colTrace.RegisterTraceServiceServer(server, &phase5Traces{sink: sink})
	go server.Serve(listener)
	defer server.Stop()
	defer listener.Close()

	cfg := &Config{Enabled: true, CloudEnabled: true, Endpoint: listener.Addr().String(), Protocol: "grpc", Insecure: true,
		Filter: FilterConfig{Exclude: DefaultFilterExclude}, Redaction: RedactionConfig{Redact: DefaultRedactFields, Hash: DefaultHashFields}}
	p := NewWithConfig(cfg)
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	stop := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		return p.Stop(ctx)
	}
	defer func() {
		if p.IsRunning() {
			_ = stop()
		}
	}()

	home := t.TempDir()
	baseEnv := append(phase5CleanEnv(), "HOME="+home, "SCION_AGENT_ID=authoritative-agent", "SCION_PROJECT_ID=authoritative-project",
		"SCION_HARNESS=claude", "SCION_TELEMETRY_ENABLED=true", "SCION_TELEMETRY_CLOUD_ENABLED=false",
		fmt.Sprintf("SCION_OTEL_GRPC_PORT=%d", p.receiver.config.GRPCPort))
	// The receiver bound port is authoritative when the config requested port 0.
	_, port, err := net.SplitHostPort(p.receiver.grpcListenAddr)
	if err != nil {
		t.Fatal(err)
	}
	baseEnv = append(baseEnv, "SCION_OTEL_GRPC_PORT="+port)
	for _, payload := range []string{
		`{"hook_event_name":"PostToolUse","tool_name":"Bash"}`,
		`{"hook_event_name":"PostToolUse","tool_name":"Bash"}`,
	} {
		cmd := exec.Command(tool, "hook", "--dialect=claude")
		cmd.Env = baseEnv
		cmd.Stdin = strings.NewReader(payload)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("independent hook: %v\n%s", err, out)
		}
	}
	phase5PostLog(t, p.receiver.httpListenAddr, phase5NativeLog("user_prompt"), http.StatusOK)
	phase5PostLog(t, p.receiver.httpListenAddr, phase5NativeLog("assistant_response"), http.StatusOK)
	if err := stop(); err != nil {
		t.Fatalf("clean Stop: %v; diagnostics=%+v", err, p.Diagnostics())
	}
	if depth := p.QueueDepth(); depth != (QueueDepth{}) {
		t.Fatalf("residual queue: %+v", depth)
	}
	d := p.Diagnostics()
	if d["logs"].Filtered != 1 || d["logs"].Unconfirmed != 0 || d["logs"].Failed != 0 {
		t.Fatalf("log diagnostics: %+v", d["logs"])
	}
	for _, signal := range []string{"spans", "metrics", "logs"} {
		got := d[signal]
		if got.Accepted == 0 || got.Accepted != got.Delivered || got.Unconfirmed != 0 || got.Failed != 0 || got.SDKErrors != 0 || got.Dropped != 0 || got.Rejected != 0 {
			t.Fatalf("%s delivery diagnostics: %+v", signal, got)
		}
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	var hookCount int64
	var nativeCount, hookLogs int
	var safeMarker bool
	for _, request := range sink.metrics {
		for _, rm := range request.ResourceMetrics {
			for _, sm := range rm.ScopeMetrics {
				for _, metric := range sm.Metrics {
					if metric.Name == "agent.tool.calls" {
						for _, point := range metric.GetSum().DataPoints {
							hookCount += point.GetAsInt()
						}
					}
				}
			}
		}
	}
	for _, request := range sink.logs {
		for _, rl := range request.ResourceLogs {
			resourceAttrs := map[string]string{}
			for _, attr := range rl.GetResource().GetAttributes() {
				resourceAttrs[attr.Key] = attr.Value.GetStringValue()
			}
			if resourceAttrs["scion.agent.id"] != "authoritative-agent" || resourceAttrs["scion.project.id"] != "authoritative-project" || resourceAttrs["scion.harness"] != "claude" {
				t.Fatalf("receiver identity not authoritative: %v", resourceAttrs)
			}
			for _, sl := range rl.ScopeLogs {
				for _, record := range sl.LogRecords {
					if sl.Scope.GetName() == "com.anthropic.claude_code.events" {
						if resourceAttrs["service.name"] != "claude-code" {
							t.Fatalf("native service identity lost: %v", resourceAttrs)
						}
						if record.EventName == "user_prompt" {
							t.Fatal("default-filtered prompt reached sink")
						}
						if record.EventName == "assistant_response" {
							nativeCount++
							for _, attr := range record.Attributes {
								if attr.Key == "safe.marker" && attr.Value.GetStringValue() == "visible" {
									safeMarker = true
								}
							}
						}
						if strings.Contains(record.String(), "synthetic-private") {
							t.Fatal("mandatory redaction failed")
						}
					} else if sl.Scope.GetName() == "sciontool.hooks" {
						hookLogs++
					}
					if strings.Contains(record.String(), "spoofed") {
						t.Fatal("spoofed native identity reached sink")
					}
				}
			}
		}
	}
	if hookCount != 2 || nativeCount != 1 || hookLogs != 2 || !safeMarker {
		t.Fatalf("hook count=%d, native=%d, hook logs=%d, safe marker=%t", hookCount, nativeCount, hookLogs, safeMarker)
	}
	if len(sink.traces) == 0 {
		t.Fatal("no hook traces delivered")
	}
}

func TestPhase5ExplicitAllowKeepsMandatoryRedaction(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sink := &phase5Sink{}
	server := grpc.NewServer()
	colLogs.RegisterLogsServiceServer(server, sink)
	go server.Serve(listener)
	defer server.Stop()
	defer listener.Close()
	p := NewWithConfig(&Config{Enabled: true, CloudEnabled: true, Endpoint: listener.Addr().String(), Protocol: "grpc", Insecure: true,
		Filter: FilterConfig{Include: []string{"agent.user.prompt"}}, Redaction: RedactionConfig{}})
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	phase5PostLog(t, p.receiver.httpListenAddr, phase5NativeLog("user_prompt"), http.StatusOK)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if got := p.Diagnostics()["logs"]; got.Accepted != 1 || got.Delivered != 1 || got.Filtered != 0 {
		t.Fatalf("explicit allow: %+v", got)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.logs) != 1 {
		t.Fatalf("captured exports=%d", len(sink.logs))
	}
	record := sink.logs[0].ResourceLogs[0].ScopeLogs[0].LogRecords[0]
	if record.EventName != "agent.user.prompt" {
		t.Fatalf("event=%q", record.EventName)
	}
	for _, attr := range record.Attributes {
		if attr.Key == "response" || attr.Key == "request_id" {
			if attr.Value.GetStringValue() != "[REDACTED]" {
				t.Fatalf("%s escaped mandatory policy", attr.Key)
			}
		}
	}
}

func TestPhase5BoundedFailureDiagnostics(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sink := &phase5Sink{failLogs: true}
	server := grpc.NewServer()
	colLogs.RegisterLogsServiceServer(server, sink)
	go server.Serve(listener)
	defer server.Stop()
	defer listener.Close()
	p := NewWithConfig(&Config{Enabled: true, CloudEnabled: true, Endpoint: listener.Addr().String(), Protocol: "grpc", Insecure: true,
		Filter: FilterConfig{Exclude: DefaultFilterExclude}, Redaction: RedactionConfig{Redact: DefaultRedactFields}})
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	phase5PostLog(t, p.receiver.httpListenAddr, phase5NativeLog("assistant_response"), http.StatusInternalServerError)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := p.Stop(ctx); err == nil {
		t.Fatal("failure Stop reported success")
	}
	d := p.Diagnostics()["logs"]
	if d.Accepted != 1 || d.Delivered != 0 || d.Unconfirmed != 1 || d.Failed == 0 || p.QueueDepth() != (QueueDepth{}) {
		t.Fatalf("failure accounting: %+v depth=%+v", d, p.QueueDepth())
	}
}
