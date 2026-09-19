package telemetry

// Actual pinned Cloud Logging SDK against a loopback fake gRPC service. No ADC,
// metadata detection, credentials, or Google endpoint is used.
import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/logging"
	lp "cloud.google.com/go/logging/apiv2/loggingpb"
	mexporter "github.com/GoogleCloudPlatform/opentelemetry-operations-go/exporter/metric"
	slog "github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	logs "go.opentelemetry.io/proto/otlp/logs/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
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

func TestPipelineStopRetriesExpiredLoggingClientClose(t *testing.T) {
	conn, err := grpc.NewClient("127.0.0.1:1", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client, err := logging.NewClient(context.Background(), "phase3-local-only", option.WithGRPCConn(conn))
	if err != nil {
		t.Fatal(err)
	}
	g := &GCPExporter{logClient: client}
	p := NewWithConfig(&Config{Enabled: true, CloudEnabled: true})
	p.running = true
	p.exporter = &CloudExporter{gcpExporter: g}
	short, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-short.Done()
	if err := p.Stop(short); err == nil {
		t.Fatal("expired Stop claimed complete shutdown")
	}
	if !p.running || p.DeliveryState() != "degraded" || g.logClient == nil {
		t.Fatalf("incomplete Stop lost client ownership: running=%v state=%s client=%v", p.running, p.DeliveryState(), g.logClient)
	}
	if err := p.Stop(context.Background()); err != nil {
		t.Fatalf("later Stop failed to close retained Logging client: %v", err)
	}
	if p.running || g.logClient != nil || !errors.Is(p.shutdownErr, context.DeadlineExceeded) || p.DeliveryState() != "degraded" {
		t.Fatalf("later Stop left client owned: running=%v client=%v", p.running, g.logClient)
	}
	// Hold the same slot after Close so this export cannot check availability
	// until it actually becomes its owner. A pre-slot check could silently
	// enqueue against a client that another owner just closed.
	if err := g.acquireLogSlot(context.Background()); err != nil {
		t.Fatal(err)
	}
	late := make(chan error, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		late <- g.ExportProtoLogs(context.Background(), nil)
	}()
	<-started
	select {
	case err := <-late:
		t.Fatalf("late export bypassed owned slot: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	<-g.logSlot
	select {
	case err := <-late:
		if err == nil || !strings.Contains(err.Error(), "unavailable") {
			t.Fatalf("closed Logging exporter accepted delayed export: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("delayed export did not return after slot release")
	}
	if g.asyncLogErrors.Load() != 0 {
		t.Fatal("delayed export entered the closed Logging SDK")
	}
	if err := p.Stop(context.Background()); err != nil {
		t.Fatalf("completed Stop not idempotent: %v", err)
	}
}

func TestPipelineStopMetricBudgetExpiresBeforeLoggingClose(t *testing.T) {
	conn, err := grpc.NewClient("127.0.0.1:1", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client, err := logging.NewClient(context.Background(), "phase3-local-only", option.WithGRPCConn(conn))
	if err != nil {
		t.Fatal(err)
	}
	g := &GCPExporter{logClient: client}
	p := NewWithConfig(&Config{Enabled: true, CloudEnabled: true})
	p.running = true
	p.exporter = &CloudExporter{gcpExporter: g}
	if err := p.handleMetrics(context.Background(), []*metricpb.ResourceMetrics{stopTestMetric(7, 2)}); err != nil {
		t.Fatal(err)
	}
	// A recent write holds this same-series metric until its 15-second slot.
	// The caller budget expires during the drain, before Logging SDK shutdown.
	p.metricLastFlush = time.Now()
	short, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := p.Stop(short); err == nil {
		t.Fatal("metric drain deadline claimed clean shutdown")
	}
	if !p.running || g.logClient == nil || p.DeliveryState() != "degraded" {
		t.Fatalf("expired metric drain lost SDK ownership: running=%v client=%v state=%s", p.running, g.logClient, p.DeliveryState())
	}
	if err := p.beginIntake(); err == nil {
		p.endIntake()
		t.Fatal("intake reopened after incomplete Stop")
	}
	if d := p.Diagnostics()["metrics"]; d.Accepted != 1 || d.Unconfirmed != 1 || p.QueueDepth() != (QueueDepth{}) {
		t.Fatalf("metric residual disposition=%+v depth=%+v", d, p.QueueDepth())
	}
	// Concurrent retries share p.mu. Exactly one can close the retained client;
	// the rest observe a completed Stop without repeating Close.
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- p.Stop(context.Background())
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil && !strings.Contains(err.Error(), "unconfirmed") {
			t.Fatalf("later cleanup error: %v", err)
		}
	}
	if p.running || g.logClient != nil || p.DeliveryState() != "degraded" {
		t.Fatalf("later cleanup incomplete: running=%v client=%v state=%s", p.running, g.logClient, p.DeliveryState())
	}
	if d := p.Diagnostics()["metrics"]; d.Accepted != 1 || d.Unconfirmed != 1 || d.Delivered != 0 {
		t.Fatalf("duplicate metric disposition after repeat Stop: %+v", d)
	}
}

func TestPipelineStopCompositeGCPOneShotMonitoringAndLogging(t *testing.T) {
	for _, debug := range []bool{false, true} {
		for _, withMetric := range []bool{false, true} {
			name := "normal"
			if debug {
				name = "debug_wrapper"
			}
			if withMetric {
				name += "_metric_budget"
			} else {
				name += "_expired"
			}
			t.Run(name, func(t *testing.T) {
				logConn, err := grpc.NewClient("127.0.0.1:1", grpc.WithTransportCredentials(insecure.NewCredentials()))
				if err != nil {
					t.Fatal(err)
				}
				defer logConn.Close()
				metricConn, err := grpc.NewClient("127.0.0.1:1", grpc.WithTransportCredentials(insecure.NewCredentials()))
				if err != nil {
					t.Fatal(err)
				}
				defer metricConn.Close()
				client, err := logging.NewClient(context.Background(), "phase3-local-only", option.WithGRPCConn(logConn))
				if err != nil {
					t.Fatal(err)
				}
				metric, err := mexporter.New(mexporter.WithProjectID("phase3-local-only"), mexporter.WithMonitoringClientOptions(option.WithGRPCConn(metricConn)))
				if err != nil {
					t.Fatal(err)
				}
				if debug {
					metric = newDebugMetricExporter(metric)
				}
				g := &GCPExporter{metricExporter: metric, logClient: client}
				p := NewWithConfig(&Config{Enabled: true, CloudEnabled: true})
				p.running = true
				p.exporter = &CloudExporter{gcpExporter: g}
				var stopCtx context.Context
				var cancel context.CancelFunc
				wantContextErr := context.Canceled
				if withMetric {
					if err := p.handleMetrics(context.Background(), []*metricpb.ResourceMetrics{stopTestMetric(7, 2)}); err != nil {
						t.Fatal(err)
					}
					p.metricLastFlush = time.Now()
					stopCtx, cancel = context.WithTimeout(context.Background(), 50*time.Millisecond)
					wantContextErr = context.DeadlineExceeded
				} else {
					stopCtx, cancel = context.WithCancel(context.Background())
					cancel()
				}
				defer cancel()
				first := p.Stop(stopCtx)
				if !errors.Is(first, wantContextErr) || !strings.Contains(first.Error(), "metric exporter shutdown") || !p.running || g.metricExporter != nil || g.logClient == nil {
					t.Fatalf("first composite Stop error=%v running=%v metric=%v log=%v", first, p.running, g.metricExporter, g.logClient)
				}
				if err := metric.Export(context.Background(), &metricdata.ResourceMetrics{}); err == nil || !strings.Contains(err.Error(), "exporter is shutdown") {
					t.Fatalf("pinned Monitoring SDK remained usable after one-shot Shutdown: %v", err)
				}
				if withMetric {
					if d := p.Diagnostics()["metrics"]; d.Accepted != 1 || d.Unconfirmed != 1 || d.Delivered != 0 || p.QueueDepth() != (QueueDepth{}) {
						t.Fatalf("first metric disposition=%+v depth=%+v", d, p.QueueDepth())
					}
				}
				if err := p.beginIntake(); err == nil {
					p.endIntake()
					t.Fatal("incomplete Stop reopened intake")
				}
				firstShutdownErr := p.shutdownErr
				for i := 0; i < 3; i++ {
					if err := p.Stop(stopCtx); !errors.Is(err, wantContextErr) || g.logClient == nil || p.shutdownErr != firstShutdownErr {
						t.Fatalf("repeated expired Stop changed ownership/error: err=%v client=%v", err, g.logClient)
					}
				}
				var wg sync.WaitGroup
				results := make(chan error, 8)
				for i := 0; i < 8; i++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						results <- p.Stop(context.Background())
					}()
				}
				wg.Wait()
				close(results)
				preserved := 0
				for err := range results {
					if err != nil {
						if strings.Contains(err.Error(), "exporter is shutdown") {
							t.Fatalf("later Stop lost first error or repeated Monitoring shutdown: %v", err)
						}
						preserved++
					}
				}
				wantPreserved := 0
				if withMetric {
					wantPreserved = 1 // the admitted metric was terminal Unconfirmed
				}
				if preserved != wantPreserved || !errors.Is(p.shutdownErr, wantContextErr) || p.running || g.metricExporter != nil || g.logClient != nil || p.DeliveryState() != "degraded" || p.QueueDepth() != (QueueDepth{}) {
					t.Fatalf("composite cleanup preserved=%d running=%v metric=%v log=%v state=%s depth=%+v", preserved, p.running, g.metricExporter, g.logClient, p.DeliveryState(), p.QueueDepth())
				}
				if withMetric {
					if d := p.Diagnostics()["metrics"]; d.Accepted != 1 || d.Unconfirmed != 1 || d.Delivered != 0 {
						t.Fatalf("duplicate metric disposition after cleanup: %+v", d)
					}
				}
			})
		}
	}
}
