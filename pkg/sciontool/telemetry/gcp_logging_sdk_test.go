package telemetry

// Actual pinned Cloud Logging SDK against a loopback fake gRPC service. No ADC,
// metadata detection, credentials, or Google endpoint is used.
import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/logging"
	lp "cloud.google.com/go/logging/apiv2/loggingpb"
	slog "github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	logs "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/api/option"
	mr "google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

type p3LoggingServer struct {
	lp.UnimplementedLoggingServiceV2Server
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int64
}

func (s *p3LoggingServer) WriteLogEntries(ctx context.Context, r *lp.WriteLogEntriesRequest) (*lp.WriteLogEntriesResponse, error) {
	if s.calls.Add(1) == 1 {
		close(s.entered)
		select {
		case <-s.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return nil, status.Error(codes.PermissionDenied, "synthetic local backend rejection")
	}
	return &lp.WriteLogEntriesResponse{}, nil
}
func TestActualLoggingSDKFailureAndRecovery(t *testing.T) {
	for _, overlap := range []bool{false, true} {
		name := "single_batch"
		if overlap {
			name = "canceled_waiter"
		}
		t.Run(name, func(t *testing.T) { testActualLoggingSDKFailureAndRecovery(t, overlap) })
	}
}

func testActualLoggingSDKFailureAndRecovery(t *testing.T, overlap bool) {
	slog.Init() // real sciontool initializes logging before concurrent callbacks
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	server := grpc.NewServer()
	fake := &p3LoggingServer{entered: make(chan struct{}), release: make(chan struct{})}
	lp.RegisterLoggingServiceV2Server(server, fake)
	go server.Serve(listener)
	defer server.Stop()
	conn, e := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	client, e := logging.NewClient(ctx, "phase3-local-only", option.WithGRPCConn(conn))
	if e != nil {
		t.Fatal(e)
	}
	logger := client.Logger("phase3-local-sdk", logging.CommonResource(&mr.MonitoredResource{Type: "global", Labels: map[string]string{"project_id": "phase3-local-only"}}), logging.ContextFunc(func() (context.Context, func()) { c, k := context.WithTimeout(ctx, 3*time.Second); return c, k }))
	p := NewWithConfig(&Config{Enabled: true, CloudEnabled: true})
	g := &GCPExporter{logClient: client, logger: logger, projectID: "phase3-local-only", onAsyncLogError: func(error) { p.logDiagnostics.sdkErrors.Add(1) }}
	callback := make(chan struct{}, 4)
	client.OnError = func(e error) { g.reportAsyncLogError(e); callback <- struct{}{} }
	p.exporter = &CloudExporter{gcpExporter: g}
	req := []*logs.ResourceLogs{{ScopeLogs: []*logs.ScopeLogs{{LogRecords: []*logs.LogRecord{{EventName: "phase3.allowed"}}}}}}
	done := make(chan error, 1)
	go func() { done <- p.handleLogs(ctx, req) }()
	select {
	case <-fake.entered:
	case <-ctx.Done():
		t.Fatal("SDK did not reach fake transport")
	}
	if d := p.Diagnostics()["logs"]; d.Accepted != 1 || d.Delivered != 0 || p.QueueDepth().Records != 1 {
		t.Fatalf("enqueue prematurely confirmed %+v depth%+v", d, p.QueueDepth())
	}
	// The second batch is admitted but must not enter Logger.Log or reach the
	// SDK after its caller deadline while the first Flush owns the slot.
	extra := int64(0)
	if overlap {
		extra = 1
		shortCtx, shortCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer shortCancel()
		secondErr := p.handleLogs(shortCtx, req)
		if status.Code(secondErr) != codes.DeadlineExceeded && !errors.Is(secondErr, context.DeadlineExceeded) {
			t.Fatalf("waiting batch result = %v", secondErr)
		}
		if fake.calls.Load() != 1 || p.QueueDepth().Records != 1 {
			t.Fatalf("waiting batch sent late or first ownership lost: calls=%d depth=%+v", fake.calls.Load(), p.QueueDepth())
		}
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer stopCancel()
		if err := g.Shutdown(stopCtx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("client closed while Flush was live: %v", err)
		}
		if fake.calls.Load() != 1 {
			t.Fatalf("shutdown changed live transport calls=%d", fake.calls.Load())
		}
	}
	close(fake.release)
	select {
	case e = <-done:
	case <-ctx.Done():
		t.Fatal("Flush did not finish")
	}
	if e == nil {
		t.Fatal("actual SDK flush failure returned success")
	}
	select {
	case <-callback:
	case <-ctx.Done():
		t.Fatal("actual SDK async callback missing")
	}
	d := p.Diagnostics()["logs"]
	if d.Accepted != 1+extra || d.Delivered != 0 || d.Unconfirmed != 1+extra || d.Partial != 1 || d.Canceled != extra || d.Attempts != 1+extra || d.Failed != 1+extra || d.SDKErrors != 1 || p.QueueDepth() != (QueueDepth{}) || fake.calls.Load() != 1 {
		t.Fatalf("failed SDK disposition %+v depth%+v calls%d", d, p.QueueDepth(), fake.calls.Load())
	}
	t.Logf("actual SDK failed enqueue/Flush/OnError: error=%v diagnostics=%+v calls=%d", e, d, fake.calls.Load())
	if e = p.handleLogs(ctx, req); e != nil {
		t.Fatalf("healthy positive control failed: %v", e)
	}
	d = p.Diagnostics()["logs"]
	if d.Accepted != 2+extra || d.Delivered != 1 || d.Unconfirmed != 1+extra || d.Attempts != 2+extra || d.Failed != 1+extra || d.SDKErrors != 1 || fake.calls.Load() != 2 {
		t.Fatalf("healthy recovery %+v calls%d", d, fake.calls.Load())
	}
	if e = g.Shutdown(ctx); e != nil {
		t.Fatalf("SDK cleanup: %v", e)
	}
	t.Logf("recovered actual SDK diagnostics=%+v; two local transport calls; no Google backend contacted", d)
}
