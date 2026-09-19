package telemetry

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func TestGenericHTTPDestinationRejectedAtStartup(t *testing.T) {
	config := &Config{Enabled: true, CloudEnabled: true, Endpoint: "127.0.0.1:1234", Protocol: "http"}
	if _, err := NewCloudExporter(context.Background(), config); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("NewCloudExporter() error = %v, want unsupported protocol", err)
	}
	if err := NewWithConfig(config).Start(context.Background()); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("Pipeline.Start() error = %v, want unsupported protocol", err)
	}
}

type mixedResultMetricExporter struct {
	captureMetricExporter
	calls int
}

func (e *mixedResultMetricExporter) Export(_ context.Context, _ *metricdata.ResourceMetrics) error {
	e.calls++
	if e.calls == 2 {
		return errors.New("descriptor mismatch")
	}
	return nil
}

func TestGCPMixedMetricResultIsTerminal(t *testing.T) {
	sink := &mixedResultMetricExporter{}
	e := &GCPExporter{metricExporter: sink}
	inputs := []*metricpb.ResourceMetrics{
		testMetricResource("native-a", "scope", "", "", testNumber("first", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, 1, 2, 1)),
		testMetricResource("native-b", "scope", "", "", testNumber("second", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, 1, 2, 1)),
	}
	err := e.ExportProtoMetrics(context.Background(), inputs)
	if err == nil || isRetryable(err) {
		t.Fatalf("mixed GCP result = %v, should be terminal", err)
	}
	if sink.calls != 2 {
		t.Fatalf("SDK calls = %d", sink.calls)
	}
}

func TestCloudLoggingAsyncErrorReachesLocalDiagnostics(t *testing.T) {
	p := NewWithConfig(&Config{Enabled: true, CloudEnabled: true})
	e := &GCPExporter{onAsyncLogError: func(error) { p.logDiagnostics.failed.Add(1) }}
	e.reportAsyncLogError(errors.New("async write failed"))
	if e.asyncLogErrors.Load() != 1 || p.Diagnostics()["logs"].Failed != 1 {
		t.Fatalf("async errors = %d, diagnostics = %+v", e.asyncLogErrors.Load(), p.Diagnostics()["logs"])
	}
}

func TestCloudBoundIntakeRejectsMissingExporter(t *testing.T) {
	p := NewWithConfig(&Config{Enabled: true, CloudEnabled: true})
	for _, test := range []struct {
		name string
		call func() error
	}{
		{"traces", func() error {
			return p.handleSpans(context.Background(), []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{Name: "safe"}}}}}})
		}},
		{"metrics", func() error {
			return p.handleMetrics(context.Background(), []*metricpb.ResourceMetrics{testMetricResource("native", "scope", "", "", testNumber("safe", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, 1, 2, 1))})
		}},
		{"logs", func() error {
			return p.handleLogs(context.Background(), []*logspb.ResourceLogs{{ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{EventName: "safe"}}}}}})
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if code := status.Code(test.call()); code != codes.Unavailable {
				t.Fatalf("admission status = %v, want Unavailable", code)
			}
		})
	}
}

func TestNoDestinationDiagnosticsCountAcceptedImmediateDrops(t *testing.T) {
	p := NewWithConfig(&Config{Enabled: true})
	if err := p.handleSpans(context.Background(), []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{Name: "safe"}}}}}}); err != nil {
		t.Fatal(err)
	}
	if err := p.handleLogs(context.Background(), []*logspb.ResourceLogs{{ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{EventName: "safe"}}}}}}); err != nil {
		t.Fatal(err)
	}
	if err := p.handleMetrics(context.Background(), []*metricpb.ResourceMetrics{testMetricResource("native", "scope", "", "", testNumber("safe", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, 1, 2, 1))}); err != nil {
		t.Fatal(err)
	}
	for signal, snapshot := range p.Diagnostics() {
		if snapshot.Accepted != 1 || snapshot.Queued != 0 || snapshot.Delivered != 0 || snapshot.Dropped != 1 {
			t.Fatalf("%s diagnostics = %+v", signal, snapshot)
		}
	}
	if depth := p.QueueDepth(); depth != (QueueDepth{}) {
		t.Fatalf("immediate drops retained budget: %+v", depth)
	}
}

func TestGenericExporterNeverClaimsMissingClientDelivery(t *testing.T) {
	e := &CloudExporter{}
	if err := e.ExportProtoSpans(context.Background(), nil); err == nil {
		t.Fatal("trace client absence reported as delivery")
	}
	if err := e.ExportProtoMetrics(context.Background(), nil); err == nil {
		t.Fatal("metric client absence reported as delivery")
	}
	if err := e.ExportProtoLogs(context.Background(), nil); err == nil {
		t.Fatal("log client absence reported as delivery")
	}
}

func TestHTTPIntakeLimitsAndTypes(t *testing.T) {
	for _, tc := range []struct {
		name, contentType, encoding string
		body                        []byte
		want                        int
	}{
		{"protobuf", "application/x-protobuf", "", nil, http.StatusOK},
		{"protobuf parameters", "application/x-protobuf; charset=binary", "identity", nil, http.StatusOK},
		{"missing type", "", "", nil, http.StatusUnsupportedMediaType},
		{"JSON", "application/json", "", nil, http.StatusUnsupportedMediaType},
		{"gzip", "application/x-protobuf", "gzip", nil, http.StatusUnsupportedMediaType},
		{"wire limit minus one", "application/x-protobuf", "", make([]byte, maxHTTPWireBytes-1), http.StatusBadRequest},
		{"wire limit", "application/x-protobuf", "", make([]byte, maxHTTPWireBytes), http.StatusBadRequest},
		{"wire limit plus one", "application/x-protobuf", "", make([]byte, maxHTTPWireBytes+1), http.StatusRequestEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/logs", bytes.NewReader(tc.body))
			if tc.contentType != "" {
				req.Header.Set("Content-Type", tc.contentType)
			}
			if tc.encoding != "" {
				req.Header.Set("Content-Encoding", tc.encoding)
			}
			response := httptest.NewRecorder()
			(&Receiver{}).handleHTTPLogs(response, req)
			if response.Code != tc.want {
				t.Fatalf("status = %d, want %d", response.Code, tc.want)
			}
		})
	}
}

func TestHTTPIntakeKeepsShorterCallerDeadline(t *testing.T) {
	caller, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req := otlpHTTPRequest("/v1/logs", bytes.NewReader(nil)).WithContext(caller)
	response := httptest.NewRecorder()
	(&Receiver{logHandler: func(ctx context.Context, _ []*logspb.ResourceLogs) error {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > time.Second {
			t.Fatalf("intake extended caller deadline: %v", deadline)
		}
		return nil
	}}).handleHTTPLogs(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
}

func TestReceiverBindsBothListenersToLoopback(t *testing.T) {
	r := NewReceiver(&Config{GRPCPort: 0, HTTPPort: 0}, nil)
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := r.Stop(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	if !strings.HasPrefix(r.grpcListenAddr, "127.0.0.1:") || !strings.HasPrefix(r.httpListenAddr, "127.0.0.1:") {
		t.Fatalf("listeners: grpc=%q http=%q", r.grpcListenAddr, r.httpListenAddr)
	}
}

func TestGRPCPredecodeOverloadRespondsAndRecovers(t *testing.T) {
	r := NewReceiver(&Config{GRPCPort: 0, HTTPPort: 0}, nil)
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.Stop(context.Background())
	conn, err := grpc.NewClient(r.grpcListenAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for i := 0; i < maxConcurrentIntake; i++ {
		r.decodeSlots <- struct{}{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err = collogspb.NewLogsServiceClient(conn).Export(ctx, &collogspb.ExportLogsServiceRequest{})
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("predecode overload = %v", err)
	}
	st, _ := status.FromError(err)
	if len(st.Details()) != 1 {
		t.Fatalf("retry details = %v", st.Details())
	}
	if _, ok := st.Details()[0].(*errdetails.RetryInfo); !ok {
		t.Fatalf("retry detail = %T", st.Details()[0])
	}
	for i := 0; i < maxConcurrentIntake; i++ {
		<-r.decodeSlots
	}
	if _, err := collogspb.NewLogsServiceClient(conn).Export(ctx, &collogspb.ExportLogsServiceRequest{}); err != nil {
		t.Fatalf("intake did not recover: %v", err)
	}
}

func TestIdleGRPCConnectionsDoNotConsumeDecodeSlots(t *testing.T) {
	r := NewReceiver(&Config{GRPCPort: 0, HTTPPort: 0}, nil)
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.Stop(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var connections []*grpc.ClientConn
	defer func() {
		for _, conn := range connections {
			_ = conn.Close()
		}
	}()
	for i := 0; i < maxConcurrentIntake+1; i++ {
		conn, err := grpc.NewClient(r.grpcListenAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			t.Fatal(err)
		}
		connections = append(connections, conn)
		if _, err := collogspb.NewLogsServiceClient(conn).Export(ctx, &collogspb.ExportLogsServiceRequest{}); err != nil {
			t.Fatalf("idle clients plus client %d: %v", i, err)
		}
	}
	if got := len(r.decodeSlots); got != 0 {
		t.Fatalf("idle clients retained %d decode slots", got)
	}
}

func TestGRPCCallerDeadlineReleasesAfterHandlerEnds(t *testing.T) {
	started := make(chan struct{})
	finished := make(chan struct{})
	r := NewReceiver(&Config{GRPCPort: 0, HTTPPort: 0}, nil, WithLogHandler(func(ctx context.Context, _ []*logspb.ResourceLogs) error {
		close(started)
		<-ctx.Done()
		close(finished)
		return ctx.Err()
	}))
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.Stop(context.Background())
	conn, err := grpc.NewClient(r.grpcListenAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = collogspb.NewLogsServiceClient(conn).Export(ctx, &collogspb.ExportLogsServiceRequest{})
	if err == nil || time.Since(start) > time.Second {
		t.Fatalf("deadline response=%v elapsed=%v", err, time.Since(start))
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("handler never started")
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("handler did not finish")
	}
	deadline := time.After(time.Second)
	for len(r.decodeSlots) != 0 {
		select {
		case <-deadline:
			t.Fatal("permit leaked after deadline")
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

type malformedCodec struct{ encoding.Codec }

func (malformedCodec) Name() string                { return "proto" }
func (malformedCodec) Marshal(any) ([]byte, error) { return []byte{0x80}, nil }
func (malformedCodec) Unmarshal([]byte, any) error { return nil }

type sizedMalformedCodec struct{ size int }

func (sizedMalformedCodec) Name() string { return "proto" }
func (c sizedMalformedCodec) Marshal(any) ([]byte, error) {
	return bytes.Repeat([]byte{0x80}, c.size), nil
}
func (sizedMalformedCodec) Unmarshal([]byte, any) error { return nil }

func TestGRPCDecodedMessageSizeBoundary(t *testing.T) {
	r := NewReceiver(&Config{GRPCPort: 0, HTTPPort: 0}, nil)
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.Stop(context.Background())
	conn, err := grpc.NewClient(r.grpcListenAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, size := range []int{maxDecodedBytes - 1, maxDecodedBytes, maxDecodedBytes + 1} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := conn.Invoke(ctx, "/opentelemetry.proto.collector.logs.v1.LogsService/Export", &collogspb.ExportLogsServiceRequest{}, &collogspb.ExportLogsServiceResponse{}, grpc.ForceCodec(sizedMalformedCodec{size: size}))
		cancel()
		want := codes.Internal // malformed protobuf within the size boundary
		if size > maxDecodedBytes {
			want = codes.ResourceExhausted
		}
		if status.Code(err) != want {
			t.Fatalf("size %d: status %v, want %v", size, err, want)
		}
	}
	if len(r.decodeSlots) != 0 {
		t.Fatalf("size boundary leaked %d slots", len(r.decodeSlots))
	}
}

func TestGRPCMalformedOversizeAndUnknownMethodReclaimPermits(t *testing.T) {
	r := NewReceiver(&Config{GRPCPort: 0, HTTPPort: 0}, nil)
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.Stop(context.Background())
	conn, err := grpc.NewClient(r.grpcListenAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	malformed := conn.Invoke(ctx, "/opentelemetry.proto.collector.logs.v1.LogsService/Export", &collogspb.ExportLogsServiceRequest{}, &collogspb.ExportLogsServiceResponse{}, grpc.ForceCodec(malformedCodec{}))
	if status.Code(malformed) != codes.Internal {
		t.Fatalf("malformed status = %v", malformed)
	}
	large := &collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: strings.Repeat("x", maxDecodedBytes)}}}}}}}}}
	_, err = collogspb.NewLogsServiceClient(conn).Export(ctx, large)
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("oversize status = %v", err)
	}
	err = conn.Invoke(ctx, "/unknown.Service/Export", &collogspb.ExportLogsServiceRequest{}, &collogspb.ExportLogsServiceResponse{})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("unknown method status = %v", err)
	}
	deadline := time.After(time.Second)
	for len(r.decodeSlots) != 0 {
		select {
		case <-deadline:
			t.Fatal("permit leaked after rejected requests")
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

func TestGRPCSlowRawBodyHonorsPredecodeCallerDeadline(t *testing.T) {
	testGRPCSlowRawBodyDeadline(t, "100m", time.Second)
}

func TestGRPCSlowRawBodyHonorsDefaultIntakeDeadline(t *testing.T) {
	testGRPCSlowRawBodyDeadline(t, "", intakeDeadline+2*time.Second)
}

func testGRPCSlowRawBodyDeadline(t *testing.T, clientTimeout string, maxElapsed time.Duration) {
	t.Helper()
	r := NewReceiver(&Config{GRPCPort: 0, HTTPPort: 0}, nil)
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.Stop(context.Background())
	conn, err := net.DialTimeout("tcp", r.grpcListenAddr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(maxElapsed + time.Second))
	if _, err := conn.Write([]byte(http2.ClientPreface)); err != nil {
		t.Fatal(err)
	}
	frames := http2.NewFramer(conn, conn)
	if err := frames.WriteSettings(); err != nil {
		t.Fatal(err)
	}
	var headers bytes.Buffer
	encoder := hpack.NewEncoder(&headers)
	fields := [][2]string{{":method", "POST"}, {":scheme", "http"}, {":path", "/opentelemetry.proto.collector.logs.v1.LogsService/Export"}, {":authority", r.grpcListenAddr}, {"content-type", "application/grpc"}, {"te", "trailers"}}
	if clientTimeout != "" {
		fields = append(fields, [2]string{"grpc-timeout", clientTimeout})
	}
	for _, field := range fields {
		if err := encoder.WriteField(hpack.HeaderField{Name: field[0], Value: field[1]}); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	if err := frames.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: headers.Bytes(), EndHeaders: true}); err != nil {
		t.Fatal(err)
	}
	// Advertise 100 bytes and send only one; decoding cannot complete yet.
	if err := frames.WriteData(1, false, []byte{0, 0, 0, 0, 100, 0}); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(maxElapsed)
	for len(r.decodeSlots) == 0 {
		select {
		case <-deadline:
			t.Fatal("slow stream never acquired predecode slot")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	for len(r.decodeSlots) != 0 {
		select {
		case <-deadline:
			t.Fatal("caller deadline did not end slow decode")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if elapsed := time.Since(start); elapsed > maxElapsed || (clientTimeout == "" && elapsed < intakeDeadline-time.Second) {
		t.Fatalf("slow body held slot for %v", elapsed)
	}
}

func TestGRPCConnectionOverflowClosesPromptly(t *testing.T) {
	r := NewReceiver(&Config{GRPCPort: 0, HTTPPort: 0}, nil)
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.Stop(context.Background())
	var clients []net.Conn
	defer func() {
		for _, c := range clients {
			_ = c.Close()
		}
	}()
	for i := 0; i < maxGRPCConnections; i++ {
		conn, err := net.DialTimeout("tcp", r.grpcListenAddr, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, conn)
	}
	deadline := time.After(time.Second)
	for len(r.grpcConnections.slots) < maxGRPCConnections {
		select {
		case <-deadline:
			t.Fatal("connections did not fill cap")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	overflow, err := net.DialTimeout("tcp", r.grpcListenAddr, time.Second)
	if err != nil {
		return
	} // An immediate transport refusal is also valid.
	defer overflow.Close()
	_ = overflow.SetReadDeadline(time.Now().Add(time.Second))
	start := time.Now()
	var one [1]byte
	if _, err := overflow.Read(one[:]); err == nil {
		t.Fatal("overflow connection remained accepted")
	} else if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		t.Fatalf("overflow connection was not closed: %v", err)
	} else if !errors.Is(err, io.EOF) && !strings.Contains(err.Error(), "reset") {
		t.Fatalf("unexpected overflow transport error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("overflow close took %v", elapsed)
	}
}

func TestGRPCFailedHandshakeReclaimsConnection(t *testing.T) {
	r := NewReceiver(&Config{GRPCPort: 0, HTTPPort: 0}, nil)
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.Stop(context.Background())
	conn, err := net.DialTimeout("tcp", r.grpcListenAddr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	start := time.Now()
	_ = conn.SetReadDeadline(time.Now().Add(grpcHandshakeTimeout + 2*time.Second))
	var one [1]byte
	for {
		if _, err := conn.Read(one[:]); err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				t.Fatalf("handshake connection timed out without close: %v", err)
			}
			break
		}
	}
	if elapsed := time.Since(start); elapsed < grpcHandshakeTimeout-time.Second || elapsed > grpcHandshakeTimeout+2*time.Second {
		t.Fatalf("handshake close elapsed %v", elapsed)
	}
	deadline := time.After(time.Second)
	for len(r.grpcConnections.slots) != 0 {
		select {
		case <-deadline:
			t.Fatal("failed handshake retained connection")
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

func TestGRPCOversizedHeadersDoNotRetainConnection(t *testing.T) {
	r := NewReceiver(&Config{GRPCPort: 0, HTTPPort: 0}, nil)
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.Stop(context.Background())
	conn, err := grpc.NewClient(r.grpcListenAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(metadata.AppendToOutgoingContext(context.Background(), "oversized", strings.Repeat("x", 20<<10)), 2*time.Second)
	defer cancel()
	_, err = collogspb.NewLogsServiceClient(conn).Export(ctx, &collogspb.ExportLogsServiceRequest{})
	if err == nil {
		t.Fatal("header above 16 KiB was accepted")
	}
	if got := len(r.decodeSlots); got != 0 {
		t.Fatalf("header rejection held %d decode slots", got)
	}
	_ = conn.Close()
	deadline := time.After(time.Second)
	for len(r.grpcConnections.slots) != 0 {
		select {
		case <-deadline:
			t.Fatal("closed header-overflow connection retained transport slot")
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

func TestHTTPAndGRPCShareIntakeCapacity(t *testing.T) {
	r := NewReceiver(&Config{GRPCPort: 0, HTTPPort: 0}, nil)
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.Stop(context.Background())
	for i := 0; i < maxConcurrentIntake-1; i++ {
		r.decodeSlots <- struct{}{}
	}
	conn, err := grpc.NewClient(r.grpcListenAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// Simulate active work on the shared slots, then verify both transports
	// reject further work and recover after capacity is released.
	r.decodeSlots <- struct{}{}
	response := httptest.NewRecorder()
	r.httpServer.Handler.ServeHTTP(response, otlpHTTPRequest("/v1/logs", bytes.NewReader(nil)))
	if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") == "" {
		t.Fatalf("HTTP saturation = %d, headers %v", response.Code, response.Header())
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err = collogspb.NewLogsServiceClient(conn).Export(ctx, &collogspb.ExportLogsServiceRequest{})
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("gRPC saturation = %v", err)
	}
	<-r.decodeSlots
	for i := 0; i < maxConcurrentIntake-1; i++ {
		<-r.decodeSlots
	}
	if _, err := collogspb.NewLogsServiceClient(conn).Export(ctx, &collogspb.ExportLogsServiceRequest{}); err != nil {
		t.Fatalf("gRPC did not recover: %v", err)
	}
}

func TestAdmissionBudgetRejectsAndRecovers(t *testing.T) {
	var budget admissionBudget
	if err := budget.reserve(maxRetainedBytes, maxRetainedRecords); err != nil {
		t.Fatal(err)
	}
	if status.Code(budget.reserve(1, 1)) != codes.ResourceExhausted {
		t.Fatal("overload was accepted")
	}
	budget.release(maxRetainedBytes, maxRetainedRecords, 1)
	if err := budget.reserve(1, 1); err != nil {
		t.Fatalf("admission did not recover: %v", err)
	}
	budget.release(1, 1, 1)
	if budget.bytes != 0 || budget.records != 0 || budget.entries != 0 {
		t.Fatalf("residual budget: bytes=%d records=%d entries=%d", budget.bytes, budget.records, budget.entries)
	}
}

func TestAdmissionBudgetConcurrentSaturation(t *testing.T) {
	var budget admissionBudget
	var accepted atomic.Int32
	var rejected atomic.Int32
	start := make(chan struct{})
	release := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < maxRetainedEntries+8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := budget.reserve(1, 1); err != nil {
				if status.Code(err) != codes.ResourceExhausted {
					t.Errorf("unexpected admission error: %v", err)
				}
				rejected.Add(1)
				return
			}
			accepted.Add(1)
			<-release
			budget.release(1, 1, 1)
		}()
	}
	close(start)
	deadline := time.After(5 * time.Second)
	for accepted.Load()+rejected.Load() < maxRetainedEntries+8 {
		select {
		case <-deadline:
			t.Fatal("admission workers stalled")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if accepted.Load() != maxRetainedEntries || rejected.Load() != 8 {
		t.Fatalf("accepted=%d rejected=%d", accepted.Load(), rejected.Load())
	}
	close(release)
	wg.Wait()
	if budget.bytes != 0 || budget.records != 0 || budget.entries != 0 {
		t.Fatalf("budget leaked: bytes=%d records=%d entries=%d", budget.bytes, budget.records, budget.entries)
	}
}

func TestMetricIntakeRetains512TinyAdmissionsThenReclaims(t *testing.T) {
	p := NewWithConfig(&Config{Enabled: true, CloudEnabled: true})
	p.exporter = &CloudExporter{metricClient: &mockMetricClient{exportFunc: func(context.Context, *colmetricpb.ExportMetricsServiceRequest, ...grpc.CallOption) (*colmetricpb.ExportMetricsServiceResponse, error) {
		return &colmetricpb.ExportMetricsServiceResponse{}, nil
	}}}
	metric := func(i int) []*metricpb.ResourceMetrics {
		return []*metricpb.ResourceMetrics{testMetricResource("sciontool", hookMetricScope, "", "", testNumber("agent.tool.calls", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA, uint64(i+1), uint64(i+2), 1))}
	}
	for i := 0; i < maxRetainedEntries; i++ {
		if err := p.handleMetrics(context.Background(), metric(i)); err != nil {
			t.Fatalf("admission %d: %v", i, err)
		}
	}
	if got := status.Code(p.handleMetrics(context.Background(), metric(maxRetainedEntries))); got != codes.ResourceExhausted {
		t.Fatalf("513th admission = %v", got)
	}
	if p.budget.entries != maxRetainedEntries {
		t.Fatalf("entries = %d", p.budget.entries)
	}
	if !p.flushMetricBuffer(context.Background(), true) {
		t.Fatal("flush failed")
	}
	if p.budget.entries != 0 || p.budget.records != 0 {
		t.Fatalf("residual budget: entries=%d records=%d", p.budget.entries, p.budget.records)
	}
	if err := p.handleMetrics(context.Background(), metric(maxRetainedEntries)); err != nil {
		t.Fatalf("admission after flush: %v", err)
	}
}

func TestPermanentMetricFailureDropsPendingAndReclaimsBudget(t *testing.T) {
	p := NewWithConfig(&Config{Enabled: true, CloudEnabled: true})
	p.retryConfig.MaxRetries = 0
	p.exporter = &CloudExporter{metricClient: &mockMetricClient{exportFunc: func(context.Context, *colmetricpb.ExportMetricsServiceRequest, ...grpc.CallOption) (*colmetricpb.ExportMetricsServiceResponse, error) {
		return nil, status.Error(codes.PermissionDenied, "descriptor denied")
	}}}
	input := []*metricpb.ResourceMetrics{testMetricResource("native", "scope", "", "", testNumber("safe", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, 1, 2, 1))}
	if err := p.handleMetrics(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if p.flushMetricBuffer(context.Background(), true) {
		t.Fatal("permanent failure reported as delivery")
	}
	if depth := p.QueueDepth(); depth != (QueueDepth{}) {
		t.Fatalf("budget after permanent disposition: %+v", depth)
	}
	if diagnostics := p.Diagnostics()["metrics"]; diagnostics.Accepted != 1 || diagnostics.Unconfirmed != 1 || diagnostics.Permanent != 1 || diagnostics.Dropped != 0 || diagnostics.Delivered != 0 || diagnostics.Failed != 1 {
		t.Fatalf("diagnostics: %+v", diagnostics)
	}
}

func TestPartialSuccessIsTerminal(t *testing.T) {
	called := 0
	e := &CloudExporter{logClient: &mockLogClient{exportFunc: func(context.Context, *collogspb.ExportLogsServiceRequest, ...grpc.CallOption) (*collogspb.ExportLogsServiceResponse, error) {
		called++
		return &collogspb.ExportLogsServiceResponse{PartialSuccess: &collogspb.ExportLogsPartialSuccess{RejectedLogRecords: 1}}, nil
	}}}
	err := retryExport(context.Background(), DefaultRetryConfig(), "logs", func() error { return e.ExportProtoLogs(context.Background(), []*logspb.ResourceLogs{{}}) })
	if err == nil || called != 1 {
		t.Fatalf("partial success error=%v attempts=%d", err, called)
	}
}

func TestTLSSkipVerifyIsDistinctFromPlaintext(t *testing.T) {
	t.Setenv(EnvInsecure, "false")
	t.Setenv(EnvSkipTLSVerify, "true")
	config := LoadConfig()
	if config.Insecure || !config.SkipTLSVerify {
		t.Fatalf("transport config: plaintext=%t skip_verify=%t", config.Insecure, config.SkipTLSVerify)
	}
	tlsConfig, err := loadSecureOTLPTLSConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	if !tlsConfig.InsecureSkipVerify {
		t.Fatal("TLS verification option was lost")
	}
}

func TestCloudDestinationRejectsContradictoryOrIgnoredTLSOptions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config Config
	}{
		{"plaintext plus skip", Config{CloudEnabled: true, Endpoint: "127.0.0.1:1234", Protocol: "grpc", Insecure: true, SkipTLSVerify: true}},
		{"plaintext plus CA", Config{CloudEnabled: true, Endpoint: "127.0.0.1:1234", Protocol: "grpc", Insecure: true, CAFile: "ca.pem"}},
		{"GCP plaintext override", Config{CloudEnabled: true, CloudProvider: "gcp", ProjectID: "test", Insecure: true}},
		{"GCP skip verify override", Config{CloudEnabled: true, CloudProvider: "gcp", ProjectID: "test", SkipTLSVerify: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewCloudExporter(context.Background(), &tc.config); err == nil {
				t.Fatal("invalid TLS configuration was accepted")
			}
		})
	}
}

func TestGenericGRPCTransportModesAgainstRealServers(t *testing.T) {
	fixture := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	certificate := fixture.TLS.Certificates[0]
	root := fixture.Certificate()
	fixture.Close()
	caFile := t.TempDir() + "/root.pem"
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name        string
		tls         bool
		insecure    bool
		skip        bool
		ca          string
		wantSuccess bool
	}{
		{"verified TLS rejects unknown root", true, false, false, "", false},
		{"custom CA", true, false, false, caFile, true},
		{"skip verification still TLS", true, false, true, "", true},
		{"explicit plaintext", false, true, false, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			var options []grpc.ServerOption
			if tc.tls {
				options = append(options, grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{certificate}})))
			}
			server := grpc.NewServer(options...)
			collogspb.RegisterLogsServiceServer(server, &logsServiceServer{})
			go server.Serve(listener)
			defer server.Stop()
			config := &Config{CloudEnabled: true, Endpoint: listener.Addr().String(), Protocol: "grpc", Insecure: tc.insecure, SkipTLSVerify: tc.skip, CAFile: tc.ca}
			exporter, err := NewCloudExporter(context.Background(), config)
			if err != nil {
				t.Fatal(err)
			}
			defer exporter.Shutdown(context.Background())
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			err = exporter.ExportProtoLogs(ctx, []*logspb.ResourceLogs{{}})
			if (err == nil) != tc.wantSuccess {
				t.Fatalf("ExportProtoLogs error = %v, want success=%t", err, tc.wantSuccess)
			}
		})
	}
}
