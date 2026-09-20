package telemetry

import (
	"bytes"
	"context"
	"fmt"
	"io"
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

	scionlog "github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
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
	mu                sync.Mutex
	logs              []*colLogs.ExportLogsServiceRequest
	metrics           []*colMetrics.ExportMetricsServiceRequest
	traces            []*colTrace.ExportTraceServiceRequest
	failLogs          bool
	transientFailures int
	logCalls          int
}

func (s *phase5Sink) Export(ctx context.Context, request *colLogs.ExportLogsServiceRequest) (*colLogs.ExportLogsServiceResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logCalls++
	if s.failLogs {
		return nil, status.Error(codes.PermissionDenied, "synthetic sink rejection")
	}
	if s.transientFailures > 0 {
		s.transientFailures--
		return nil, status.Error(codes.Unavailable, "synthetic transient rejection")
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
			// Pinned Claude Code 2.1.273 leaves EventName empty and emits event.name.
			LogRecords: []*logspb.LogRecord{{Body: phase5String("synthetic-private-body"),
				Attributes: []*commonpb.KeyValue{phase5Attr("event.name", event), phase5Attr("response", "synthetic-private-response"), phase5Attr("request_id", "synthetic-private-id"), phase5Attr("safe.marker", "visible")},
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

func phase5IsPromptMarker(value string) bool {
	value = strings.ToLower(strings.NewReplacer(".", "_", "-", "_", " ", "_").Replace(value))
	return strings.Contains(value, "user_prompt")
}

func phase5NativeInventoryError(records []*logspb.LogRecord, expected string) error {
	for _, record := range records {
		if phase5IsPromptMarker(record.EventName) {
			return fmt.Errorf("filtered prompt marker reached native sink")
		}
		for _, attr := range record.Attributes {
			if phase5IsPromptMarker(attr.Key) || phase5IsPromptMarker(attr.Value.GetStringValue()) {
				return fmt.Errorf("filtered prompt marker reached native sink")
			}
		}
		if record.EventName != expected {
			return fmt.Errorf("unexpected native event reached sink")
		}
	}
	if len(records) != 1 {
		return fmt.Errorf("native sink record count mismatch: %d", len(records))
	}
	return nil
}

func TestPhase5NativeSinkRejectsExtraPrompt(t *testing.T) {
	allowed := phase5NativeLog("assistant_response").ResourceLogs[0].ScopeLogs[0].LogRecords[0]
	allowed.EventName = "assistant_response"
	prompt := phase5NativeLog("user_prompt").ResourceLogs[0].ScopeLogs[0].LogRecords[0]
	if err := phase5NativeInventoryError([]*logspb.LogRecord{allowed, prompt}, "assistant_response"); err == nil || !strings.Contains(err.Error(), "prompt marker") {
		t.Fatal("extra prompt-shaped native sink record was not rejected as a prompt")
	}
	unknown := phase5NativeLog("unknown_event").ResourceLogs[0].ScopeLogs[0].LogRecords[0]
	unknown.EventName = "assistant_response"
	if err := phase5NativeInventoryError([]*logspb.LogRecord{allowed, unknown}, "assistant_response"); err == nil || !strings.Contains(err.Error(), "count mismatch") {
		t.Fatal("extra unknown native sink record was not rejected by exact count")
	}
}

func phase5RequireNativeRecord(t *testing.T, record *logspb.LogRecord, event string) {
	t.Helper()
	if record.EventName != event || record.Body.GetStringValue() != "[REDACTED]" {
		t.Fatal("native event name or body policy mismatch")
	}
	counts := map[string]int{}
	for _, attr := range record.Attributes {
		switch attr.Key {
		case "event.name":
			if attr.Value.GetStringValue() != event {
				t.Fatal("normalized event attribute mismatch")
			}
		case "response", "request_id":
			if attr.Value.GetStringValue() != "[REDACTED]" {
				t.Fatal("mandatory native attribute redaction mismatch")
			}
		case "safe.marker":
			if attr.Value.GetStringValue() != "visible" {
				t.Fatal("safe positive control missing")
			}
		}
		counts[attr.Key]++
	}
	for _, key := range []string{"event.name", "response", "request_id", "safe.marker"} {
		if counts[key] != 1 {
			t.Fatal("required native attribute missing or duplicated: " + key)
		}
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
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		if err := cmd.Run(); err != nil {
			t.Fatalf("independent hook failed: %v", err)
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
	var hookPoints, hookLogs, hookSpans int
	var nativeRecords []*logspb.LogRecord
	spanIDs := map[string]bool{}
	for _, request := range sink.metrics {
		for _, rm := range request.ResourceMetrics {
			resourceAttrs := map[string]string{}
			for _, attr := range rm.GetResource().GetAttributes() {
				resourceAttrs[attr.Key] = attr.Value.GetStringValue()
			}
			for _, sm := range rm.ScopeMetrics {
				for _, metric := range sm.Metrics {
					if metric.Name == "agent.tool.calls" {
						if resourceAttrs["scion.agent.id"] != "authoritative-agent" || resourceAttrs["scion.project.id"] != "authoritative-project" || resourceAttrs["service.name"] != "sciontool" || sm.Scope.GetName() != "github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/handlers" || metric.Unit != "{call}" || metric.GetSum() == nil || !metric.GetSum().IsMonotonic {
							t.Fatal("hook counter resource, scope, type or unit mismatch")
						}
						for _, point := range metric.GetSum().DataPoints {
							hookPoints++
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
				t.Fatal("receiver Scion identity was not authoritative")
			}
			for _, sl := range rl.ScopeLogs {
				for _, record := range sl.LogRecords {
					if sl.Scope.GetName() == "com.anthropic.claude_code.events" {
						nativeRecords = append(nativeRecords, record)
						if resourceAttrs["service.name"] != "claude-code" {
							t.Fatal("native service identity was not preserved")
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
	if err := phase5NativeInventoryError(nativeRecords, "assistant_response"); err != nil {
		t.Fatal(err)
	}
	phase5RequireNativeRecord(t, nativeRecords[0], "assistant_response")
	for _, request := range sink.traces {
		for _, rs := range request.ResourceSpans {
			resourceAttrs := map[string]string{}
			for _, attr := range rs.GetResource().GetAttributes() {
				resourceAttrs[attr.Key] = attr.Value.GetStringValue()
			}
			if resourceAttrs["scion.agent.id"] != "authoritative-agent" || resourceAttrs["scion.project.id"] != "authoritative-project" || resourceAttrs["service.name"] != "sciontool" {
				t.Fatal("hook span identity mismatch")
			}
			for _, ss := range rs.ScopeSpans {
				if ss.Scope.GetName() != "github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/handlers" {
					t.Fatal("hook span scope mismatch")
				}
				for _, span := range ss.Spans {
					if span.Name != "agent.tool.result" || len(span.SpanId) != 8 {
						t.Fatal("hook span shape mismatch")
					}
					id := string(span.SpanId)
					if spanIDs[id] {
						t.Fatal("duplicate hook span ID")
					}
					spanIDs[id] = true
					hookSpans++
				}
			}
		}
	}
	if hookCount != 2 || hookPoints != 1 || hookLogs != 2 || hookSpans != 2 {
		t.Fatalf("hook count=%d, points=%d, native=%d, hook logs=%d, spans=%d", hookCount, hookPoints, len(nativeRecords), hookLogs, hookSpans)
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
	phase5RequireNativeRecord(t, record, "agent.user.prompt")
}

func TestPhase5BoundedFailureDiagnostics(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "collector.log")
	scionlog.SetLogPath(logPath)
	t.Cleanup(func() { scionlog.SetLogPath("/tmp/agent.log") })
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
	stopErr := p.Stop(ctx)
	if stopErr == nil {
		t.Fatal("failure Stop reported success")
	}
	d := p.Diagnostics()["logs"]
	if d.Accepted != 1 || d.Delivered != 0 || d.Unconfirmed != 1 || d.Permanent != 1 || d.Failed != 1 || d.Attempts != 1 || p.QueueDepth() != (QueueDepth{}) {
		t.Fatalf("failure accounting: %+v depth=%+v", d, p.QueueDepth())
	}
	output, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(output), "state=degraded") || !strings.Contains(string(output), "Permanent:1") || !strings.Contains(string(output), "Failed:1") || strings.Contains(string(output), "synthetic-private") || strings.Contains(stopErr.Error(), "synthetic-private") {
		t.Fatal("fixed failure diagnostics absent or raw payload leaked")
	}
}

func TestPhase5TransientRecoveryDoesNotDoubleCount(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sink := &phase5Sink{transientFailures: 1}
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
	phase5PostLog(t, p.receiver.httpListenAddr, phase5NativeLog("assistant_response"), http.StatusOK)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := p.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	got := p.Diagnostics()["logs"]
	if got.Accepted != 1 || got.Delivered != 1 || got.Unconfirmed != 0 || got.Attempts != 2 || got.Failed != 0 || p.QueueDepth() != (QueueDepth{}) {
		t.Fatalf("transient recovery accounting: %+v depth=%+v", got, p.QueueDepth())
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.logCalls != 2 || len(sink.logs) != 1 {
		t.Fatal("transient retry duplicated destination record")
	}
	phase5RequireNativeRecord(t, sink.logs[0].ResourceLogs[0].ScopeLogs[0].LogRecords[0], "assistant_response")
}
