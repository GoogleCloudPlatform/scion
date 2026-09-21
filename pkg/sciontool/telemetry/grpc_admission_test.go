package telemetry

import (
	"context"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/encoding"
	_ "google.golang.org/grpc/encoding/gzip"
	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"
)

func TestBoundedGRPCAdaptersAcceptGeneratedUnaryClients(t *testing.T) {
	var traces, metrics, logs atomic.Int32
	r := NewReceiver(&Config{GRPCPort: 0, HTTPPort: 0}, func(_ context.Context, got []*tracepb.ResourceSpans) error {
		if len(got) != 1 {
			t.Errorf("trace resources = %d", len(got))
		}
		traces.Add(1)
		return nil
	}, WithMetricHandler(func(_ context.Context, got []*metricpb.ResourceMetrics) error {
		if len(got) != 1 {
			t.Errorf("metric resources = %d", len(got))
		}
		metrics.Add(1)
		return nil
	}), WithLogHandler(func(_ context.Context, got []*logspb.ResourceLogs) error {
		if len(got) != 1 {
			t.Errorf("log resources = %d", len(got))
		}
		logs.Add(1)
		return nil
	}))
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Stop(context.Background()) }()
	conn, err := grpc.NewClient(r.grpcListenAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := coltracepb.NewTraceServiceClient(conn).Export(ctx, &coltracepb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := colmetricpb.NewMetricsServiceClient(conn).Export(ctx, &colmetricpb.ExportMetricsServiceRequest{ResourceMetrics: []*metricpb.ResourceMetrics{{}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := collogspb.NewLogsServiceClient(conn).Export(ctx, &collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{}}}); err != nil {
		t.Fatal(err)
	}
	if traces.Load() != 1 || metrics.Load() != 1 || logs.Load() != 1 || len(r.decodeSlots) != 0 {
		t.Fatalf("handler calls traces=%d metrics=%d logs=%d slots=%d", traces.Load(), metrics.Load(), logs.Load(), len(r.decodeSlots))
	}
	for _, serviceName := range []string{"opentelemetry.proto.collector.trace.v1.TraceService", "opentelemetry.proto.collector.metrics.v1.MetricsService", "opentelemetry.proto.collector.logs.v1.LogsService"} {
		info := r.grpcServer.GetServiceInfo()[serviceName]
		if len(info.Methods) != 1 || !info.Methods[0].IsServerStream || info.Methods[0].IsClientStream {
			t.Fatalf("service %s classification = %+v", serviceName, info.Methods)
		}
	}
}

type countedCodec struct {
	base    encoding.CodecV2
	calls   atomic.Int32
	entered chan struct{}
	unblock chan struct{}
}

func (c *countedCodec) Name() string                           { return c.base.Name() }
func (c *countedCodec) Marshal(v any) (mem.BufferSlice, error) { return c.base.Marshal(v) }
func (c *countedCodec) Unmarshal(b mem.BufferSlice, v any) error {
	c.calls.Add(1)
	if c.entered != nil {
		select {
		case c.entered <- struct{}{}:
		default:
		}
		<-c.unblock
	}
	return c.base.Unmarshal(b, v)
}

type countedCompressor struct {
	base  encoding.Compressor
	calls atomic.Int32
}

func (c *countedCompressor) Name() string { return "phase3gzip" }
func (c *countedCompressor) Compress(w io.Writer) (io.WriteCloser, error) {
	return c.base.Compress(w)
}
func (c *countedCompressor) Decompress(r io.Reader) (io.Reader, error) {
	c.calls.Add(1)
	return c.base.Decompress(r)
}

var compressedIntake = &countedCompressor{base: encoding.GetCompressor("gzip")}

func init() { encoding.RegisterCompressor(compressedIntake) }

type dispatchBarrierStats struct {
	grpcDeadlineStats
	entered chan struct{}
	resume  chan struct{}
	once    sync.Once
}

type classificationStats struct {
	grpcDeadlineStats
	begins atomic.Int32
	wrong  atomic.Bool
}

func (s *classificationStats) HandleRPC(ctx context.Context, event stats.RPCStats) {
	if begin, ok := event.(*stats.Begin); ok {
		s.begins.Add(1)
		if !begin.IsServerStream || begin.IsClientStream {
			s.wrong.Store(true)
		}
	}
	s.grpcDeadlineStats.HandleRPC(ctx, event)
}

func (b *dispatchBarrierStats) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context {
	b.once.Do(func() { close(b.entered) })
	<-b.resume
	return ctx
}

func testBoundedGRPCServer(t *testing.T, extra ...grpc.ServerOption) (*grpc.ClientConn, chan struct{}, *countedCodec) {
	t.Helper()
	slots := make(chan struct{}, maxConcurrentIntake)
	codec := &countedCodec{base: encoding.GetCodecV2("proto")}
	options := []grpc.ServerOption{grpc.ForceServerCodecV2(codec), grpc.MaxRecvMsgSize(maxDecodedBytes), grpc.InTapHandle(grpcDeadlineTap)}
	options = append(options, extra...)
	server := grpc.NewServer(options...)
	registerBoundedOTLPServices(server, slots, &traceServiceServer{}, &metricsServiceServer{}, &logsServiceServer{})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve(listener) }()
	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(); server.Stop(); _ = listener.Close() })
	return conn, slots, codec
}

func TestBoundedGRPCAdapterRejectsBeforeDecompressionAndDecode(t *testing.T) {
	conn, slots, codec := testBoundedGRPCServer(t, grpc.StatsHandler(grpcDeadlineStats{}))
	compressedIntake.calls.Store(0)
	for range maxConcurrentIntake {
		slots <- struct{}{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := collogspb.NewLogsServiceClient(conn).Export(ctx,
		&collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{}}}, grpc.UseCompressor(compressedIntake.Name()))
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("saturation = %v", err)
	}
	if codec.calls.Load() != 0 || compressedIntake.calls.Load() != 0 {
		t.Fatalf("overflow decoded=%d decompressed=%d", codec.calls.Load(), compressedIntake.calls.Load())
	}
	for range maxConcurrentIntake {
		<-slots
	}
	if _, err := collogspb.NewLogsServiceClient(conn).Export(ctx,
		&collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{}}}, grpc.UseCompressor(compressedIntake.Name())); err != nil {
		t.Fatalf("recovery = %v", err)
	}
	if codec.calls.Load() != 1 || compressedIntake.calls.Load() != 1 {
		t.Fatalf("recovered decoded=%d decompressed=%d", codec.calls.Load(), compressedIntake.calls.Load())
	}
}

func TestBoundedGRPCAdapterStatsClassification(t *testing.T) {
	collector := &classificationStats{}
	conn, _, _ := testBoundedGRPCServer(t, grpc.StatsHandler(collector))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := collogspb.NewLogsServiceClient(conn).Export(ctx, &collogspb.ExportLogsServiceRequest{}); err != nil {
		t.Fatal(err)
	}
	if collector.begins.Load() != 1 || collector.wrong.Load() {
		t.Fatalf("streaming stats classification begins=%d wrong=%v", collector.begins.Load(), collector.wrong.Load())
	}
}

func TestBoundedGRPCAdapterLateCanceledDispatchDoesNotTakeSlot(t *testing.T) {
	barrier := &dispatchBarrierStats{entered: make(chan struct{}), resume: make(chan struct{})}
	conn, slots, codec := testBoundedGRPCServer(t, grpc.StatsHandler(barrier))
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() {
		_, err := collogspb.NewLogsServiceClient(conn).Export(ctx, &collogspb.ExportLogsServiceRequest{})
		finished <- err
	}()
	select {
	case <-barrier.entered:
	case <-time.After(time.Second):
		t.Fatal("request did not reach dispatch barrier")
	}
	cancel()
	for range maxConcurrentIntake {
		slots <- struct{}{}
	}
	close(barrier.resume)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("canceled client did not return")
	}
	if len(slots) != maxConcurrentIntake || codec.calls.Load() != 0 {
		t.Fatalf("late canceled dispatch slots=%d decoded=%d", len(slots), codec.calls.Load())
	}
	for range maxConcurrentIntake {
		<-slots
	}
}

func TestBoundedGRPCAdapterCancellationDuringDecodeRetainsSlot(t *testing.T) {
	conn, slots, codec := testBoundedGRPCServer(t, grpc.StatsHandler(grpcDeadlineStats{}))
	codec.entered = make(chan struct{}, 1)
	codec.unblock = make(chan struct{})
	var unblockOnce sync.Once
	unblock := func() { unblockOnce.Do(func() { close(codec.unblock) }) }
	defer unblock()
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() {
		_, err := collogspb.NewLogsServiceClient(conn).Export(ctx, &collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{}}})
		finished <- err
	}()
	select {
	case <-codec.entered:
	case <-time.After(time.Second):
		t.Fatal("decode did not start")
	}
	cancel()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("canceled client did not return")
	}
	if len(slots) != 1 {
		t.Fatalf("cancel released live decoder: %d", len(slots))
	}
	for range maxConcurrentIntake - 1 {
		slots <- struct{}{}
	}
	probe, probeCancel := context.WithTimeout(context.Background(), time.Second)
	defer probeCancel()
	_, err := collogspb.NewLogsServiceClient(conn).Export(probe, &collogspb.ExportLogsServiceRequest{})
	if status.Code(err) != codes.ResourceExhausted || codec.calls.Load() != 1 {
		t.Fatalf("overflow while decode held: error=%v decodes=%d", err, codec.calls.Load())
	}
	unblock()
	for range maxConcurrentIntake - 1 {
		<-slots
	}
	deadline := time.After(time.Second)
	for len(slots) != 0 {
		select {
		case <-deadline:
			t.Fatal("completed decoder retained slot")
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

func TestBoundedGRPCAdapterRejectsExtraMessageBeforeExport(t *testing.T) {
	var exports atomic.Int32
	r := NewReceiver(&Config{GRPCPort: 0, HTTPPort: 0}, nil, WithLogHandler(func(context.Context, []*logspb.ResourceLogs) error {
		exports.Add(1)
		return nil
	}))
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Stop(context.Background()) }()
	conn, err := grpc.NewClient(r.grpcListenAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	stream, err := conn.NewStream(ctx, &grpc.StreamDesc{ClientStreams: true, ServerStreams: true}, logExportMethod)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := stream.SendMsg(&collogspb.ExportLogsServiceRequest{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	err = stream.RecvMsg(&collogspb.ExportLogsServiceResponse{})
	if status.Code(err) != codes.Internal || exports.Load() != 0 {
		t.Fatalf("extra message error=%v exports=%d", err, exports.Load())
	}
	if len(r.decodeSlots) != 0 {
		t.Fatalf("extra message retained %d slots", len(r.decodeSlots))
	}
}

func TestBoundedGRPCAdapterCompressedExpansionLimit(t *testing.T) {
	conn, slots, codec := testBoundedGRPCServer(t, grpc.StatsHandler(grpcDeadlineStats{}))
	compressedIntake.calls.Store(0)
	request := &collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: strings.Repeat("x", maxDecodedBytes)}}}}}}}}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := collogspb.NewLogsServiceClient(conn).Export(ctx, request, grpc.UseCompressor(compressedIntake.Name()))
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("compressed expansion = %v", err)
	}
	if codec.calls.Load() != 0 || compressedIntake.calls.Load() != 1 || len(slots) != 0 {
		t.Fatalf("oversize decoded=%d decompressed=%d slots=%d", codec.calls.Load(), compressedIntake.calls.Load(), len(slots))
	}
}

func TestBoundedGRPCAdapterRetainsCanceledWorkAndRejectsSeventeenth(t *testing.T) {
	started := make(chan struct{}, maxConcurrentIntake)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseWork := func() { releaseOnce.Do(func() { close(release) }) }
	var active, highWater atomic.Int32
	r := NewReceiver(&Config{GRPCPort: 0, HTTPPort: 0}, nil, WithLogHandler(func(context.Context, []*logspb.ResourceLogs) error {
		now := active.Add(1)
		for old := highWater.Load(); now > old; old = highWater.Load() {
			if highWater.CompareAndSwap(old, now) {
				break
			}
		}
		started <- struct{}{}
		<-release
		active.Add(-1)
		return nil
	}))
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Stop(context.Background()) }()
	defer releaseWork()
	conn, err := grpc.NewClient(r.grpcListenAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	var wg sync.WaitGroup
	cancels := make([]context.CancelFunc, maxConcurrentIntake)
	for i := 0; i < maxConcurrentIntake; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancels[i] = cancel
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = collogspb.NewLogsServiceClient(conn).Export(ctx, &collogspb.ExportLogsServiceRequest{})
		}()
	}
	for range maxConcurrentIntake {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("16 handlers did not start")
		}
	}
	// The advertised per-connection stream cap queues this call in the
	// generated client before its HEADERS reach the server. Its caller deadline
	// still bounds that wait, while server work and slots stay unchanged.
	queuedCtx, queuedCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	_, queuedErr := collogspb.NewLogsServiceClient(conn).Export(queuedCtx, &collogspb.ExportLogsServiceRequest{})
	queuedCancel()
	if status.Code(queuedErr) != codes.DeadlineExceeded || len(r.decodeSlots) != maxConcurrentIntake || highWater.Load() != maxConcurrentIntake {
		t.Fatalf("same-connection queued call=%v slots=%d highWater=%d", queuedErr, len(r.decodeSlots), highWater.Load())
	}
	cancels[0]()
	if len(r.decodeSlots) != maxConcurrentIntake {
		t.Fatalf("canceled live handler released capacity: %d", len(r.decodeSlots))
	}
	overflowConn, err := grpc.NewClient(r.grpcListenAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = overflowConn.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	_, err = collogspb.NewLogsServiceClient(overflowConn).Export(ctx, &collogspb.ExportLogsServiceRequest{})
	if status.Code(err) != codes.ResourceExhausted || time.Since(start) > time.Second {
		t.Fatalf("seventeenth result=%v elapsed=%v", err, time.Since(start))
	}
	st, _ := status.FromError(err)
	if len(st.Details()) != 1 {
		t.Fatalf("retry details = %v", st.Details())
	}
	if _, ok := st.Details()[0].(*errdetails.RetryInfo); !ok {
		t.Fatalf("retry detail = %T", st.Details()[0])
	}
	releaseWork()
	for _, cancel := range cancels {
		cancel()
	}
	wg.Wait()
	deadline := time.After(time.Second)
	for len(r.decodeSlots) != 0 {
		select {
		case <-deadline:
			t.Fatal("completed handlers retained capacity")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if highWater.Load() > maxConcurrentIntake {
		t.Fatalf("high-water processing stacks = %d", highWater.Load())
	}
	if _, err := collogspb.NewLogsServiceClient(overflowConn).Export(ctx, &collogspb.ExportLogsServiceRequest{}); err != nil {
		t.Fatalf("capacity did not recover: %v", err)
	}
}

func TestBoundedGRPCAdapterStopKeepsLiveHandlerOwnership(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	r := NewReceiver(&Config{GRPCPort: 0, HTTPPort: 0}, nil, WithLogHandler(func(context.Context, []*logspb.ResourceLogs) error {
		close(entered)
		<-release
		return nil
	}))
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	conn, err := grpc.NewClient(r.grpcListenAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	callDone := make(chan struct{})
	go func() {
		defer close(callDone)
		_, _ = collogspb.NewLogsServiceClient(conn).Export(context.Background(), &collogspb.ExportLogsServiceRequest{})
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("handler did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	stopDone := make(chan error, 1)
	go func() { stopDone <- r.Stop(ctx) }()
	select {
	case err := <-stopDone:
		if err == nil {
			t.Fatal("forced stop did not report deadline")
		}
	case <-time.After(time.Second):
		t.Fatal("forced stop did not return within caller budget")
	}
	if len(r.decodeSlots) != 1 {
		t.Fatalf("Stop released live handler capacity: %d", len(r.decodeSlots))
	}
	gracefulDone, forceDone := r.grpcGracefulDone, r.grpcForceDone
	if gracefulDone == nil || forceDone == nil {
		t.Fatal("forced shutdown lifecycle was not recorded")
	}
	var repeated sync.WaitGroup
	for range 8 {
		repeated.Add(1)
		go func() {
			defer repeated.Done()
			shortCtx, shortCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer shortCancel()
			if err := r.Stop(shortCtx); err == nil {
				t.Error("repeated Stop claimed completion while handler was live")
			}
		}()
	}
	repeated.Wait()
	if r.grpcGracefulDone != gracefulDone || r.grpcForceDone != forceDone {
		t.Fatal("repeated Stop launched another shutdown lifecycle")
	}
	close(release)
	select {
	case <-callDone:
	case <-time.After(time.Second):
		t.Fatal("handler did not complete")
	}
	deadline := time.After(time.Second)
	for len(r.decodeSlots) != 0 {
		select {
		case <-deadline:
			t.Fatal("handler return leaked slot")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	for _, done := range []<-chan struct{}{gracefulDone, forceDone} {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("shutdown goroutine remained after handler returned")
		}
	}
	finishCtx, finishCancel := context.WithTimeout(context.Background(), time.Second)
	defer finishCancel()
	if err := r.Stop(finishCtx); err != nil {
		t.Fatalf("completed receiver Stop: %v", err)
	}
}

func TestPipelineStopLeavesActiveReceiverResourcesUntilHandlerReturns(t *testing.T) {
	p := NewWithConfig(&Config{Enabled: true, GRPCPort: 0, HTTPPort: 0})
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseWork := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseWork()
	p.exporter = &CloudExporter{logClient: &mockLogClient{exportFunc: func(context.Context, *collogspb.ExportLogsServiceRequest, ...grpc.CallOption) (*collogspb.ExportLogsServiceResponse, error) {
		close(entered)
		<-release
		return &collogspb.ExportLogsServiceResponse{}, nil
	}}}
	conn, err := grpc.NewClient(p.receiver.grpcListenAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	callDone := make(chan struct{})
	go func() {
		defer close(callDone)
		_, _ = collogspb.NewLogsServiceClient(conn).Export(context.Background(), &collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{EventName: "safe"}}}}}}})
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("pipeline receiver never reached exporter")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	err = p.Stop(ctx)
	cancel()
	if err == nil || p.exporter == nil || !p.running || p.activeIntake() != 1 || p.QueueDepth().Entries != 1 {
		t.Fatalf("incomplete stop error=%v running=%v active=%d depth=%+v", err, p.running, p.activeIntake(), p.QueueDepth())
	}
	if status.Code(p.acceptLogs(context.Background(), []*logspb.ResourceLogs{{}})) != codes.Unavailable {
		t.Fatal("pipeline accepted intake after shutdown closure")
	}
	releaseWork()
	select {
	case <-callDone:
	case <-time.After(time.Second):
		t.Fatal("in-flight handler did not return")
	}
	// Client cancellation can finish before the server handler unwinds. Wait
	// for the actual owner to release its intake and queue reservations.
	ownerDone := time.After(time.Second)
	for p.activeIntake() != 0 || p.QueueDepth().Entries != 0 {
		select {
		case <-ownerDone:
			t.Fatalf("residual after handler: active=%d depth=%+v", p.activeIntake(), p.QueueDepth())
		default:
			time.Sleep(time.Millisecond)
		}
	}
	// Model a pre-handler RecvMsg that still owns a processing slot after a
	// forced transport stop. A repeated Stop cannot claim clean drain yet.
	p.receiver.decodeSlots <- struct{}{}
	shortCtx, shortCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	if err := p.Stop(shortCtx); err == nil {
		t.Fatal("repeated Stop claimed clean drain with live processing slot")
	}
	shortCancel()
	<-p.receiver.decodeSlots
	finishCtx, finishCancel := context.WithTimeout(context.Background(), time.Second)
	defer finishCancel()
	if err := p.Stop(finishCtx); err != nil {
		t.Fatalf("final cleanup: %v", err)
	}
}
