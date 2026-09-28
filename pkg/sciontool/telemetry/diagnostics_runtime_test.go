package telemetry

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	scionlog "github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	colLog "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	logpb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type diagnosticLogBackend struct {
	colLog.UnimplementedLogsServiceServer
	fail atomic.Bool
}

func (b *diagnosticLogBackend) Export(context.Context, *colLog.ExportLogsServiceRequest) (*colLog.ExportLogsServiceResponse, error) {
	if b.fail.Load() {
		return nil, status.Error(codes.PermissionDenied, "local fake rejection")
	}
	return &colLog.ExportLogsServiceResponse{}, nil
}

func TestRuntimeDeliverySnapshotsSurviveBrokenDestination(t *testing.T) {
	path := filepath.Join(t.TempDir(), "collector.log")
	scionlog.SetLogPath(path)
	t.Cleanup(func() { scionlog.SetLogPath("/tmp/agent.log") })
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	backend := &diagnosticLogBackend{}
	backend.fail.Store(true)
	server := grpc.NewServer()
	colLog.RegisterLogsServiceServer(server, backend)
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	defer func() { _ = listener.Close() }()

	p := NewWithConfig(&Config{Enabled: true, CloudEnabled: true, Endpoint: listener.Addr().String(), Protocol: "grpc", Insecure: true})
	if p.DeliveryState() != "configured" {
		t.Fatalf("pre-start state=%s", p.DeliveryState())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.Start(ctx); err != nil {
		t.Fatal(err)
	}
	request := []*logpb.ResourceLogs{{ScopeLogs: []*logpb.ScopeLogs{{LogRecords: []*logpb.LogRecord{{EventName: "safe"}}}}}}
	if err := p.handleLogs(ctx, request); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("fake failure=%v", err)
	}
	if p.DeliveryState() != "degraded" {
		t.Fatalf("failure state=%s", p.DeliveryState())
	}
	backend.fail.Store(false)
	if err := p.handleLogs(ctx, request); err != nil {
		t.Fatalf("recovery=%v", err)
	}
	for i := 0; i < 20; i++ {
		p.logDeliverySnapshot(false)
	}
	p.diagnosticMu.Lock()
	p.diagnosticLast = time.Now().Add(-diagnosticSnapshotInterval)
	p.diagnosticMu.Unlock()
	p.logDeliverySnapshot(false)
	for i := 0; i < 20; i++ {
		p.logDeliverySnapshot(false)
	}
	if err := p.Stop(ctx); err == nil || !strings.Contains(err.Error(), "telemetry") {
		t.Fatalf("terminal uncertainty missing from Stop=%v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := string(content)
	if strings.Count(lines, "Telemetry delivery snapshot") != 4 { // startup, first error, one interval, final
		t.Fatalf("snapshot frequency=%d\n%s", strings.Count(lines, "Telemetry delivery snapshot"), lines)
	}
	for _, want := range []string{"state=running", "state=degraded", "queue_records=0", "Accepted:2", "Delivered:1", "Unconfirmed:1", "Failed:1"} {
		if !strings.Contains(lines, want) {
			t.Fatalf("missing %q in local diagnostics:\n%s", want, lines)
		}
	}
	if strings.Contains(lines, "local fake rejection") && strings.Contains(lines, "Telemetry delivery snapshot state=") {
		// The existing export-error log may contain a sanitized backend error;
		// snapshot fields themselves must never include its text.
		for _, line := range strings.Split(lines, "\n") {
			if strings.Contains(line, "Telemetry delivery snapshot") && strings.Contains(line, "local fake rejection") {
				t.Fatalf("raw backend error in fixed snapshot: %s", line)
			}
		}
	}
}

func TestDeliverySnapshotsCoverDisabledAndFailedStartup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "collector.log")
	scionlog.SetLogPath(path)
	t.Cleanup(func() { scionlog.SetLogPath("/tmp/agent.log") })
	disabled := NewWithConfig(&Config{Enabled: true})
	if disabled.DeliveryState() != "disabled" {
		t.Fatalf("disabled state=%s", disabled.DeliveryState())
	}
	if err := disabled.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := disabled.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	bad := NewWithConfig(&Config{Enabled: true, CloudEnabled: true, Endpoint: "127.0.0.1:1234", Protocol: "http"})
	if err := bad.Start(context.Background()); err == nil || bad.DeliveryState() != "failed" {
		t.Fatalf("invalid startup=%v state=%s", err, bad.DeliveryState())
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"state=running", "state=disabled", "state=failed"} {
		if !strings.Contains(string(content), "Telemetry delivery snapshot "+want) && !strings.Contains(string(content), "Telemetry delivery snapshot state="+strings.TrimPrefix(want, "state=")) {
			t.Fatalf("missing %s in diagnostics: %s", want, content)
		}
	}
}
