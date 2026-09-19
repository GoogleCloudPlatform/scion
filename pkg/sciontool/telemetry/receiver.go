/*
Copyright 2025 The Scion Authors.
*/

package telemetry

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/tap"
	"google.golang.org/protobuf/proto"
)

const (
	maxHTTPWireBytes    = 4 << 20
	maxDecodedBytes     = 8 << 20
	intakeDeadline      = 15 * time.Second
	maxConcurrentIntake = 16
)

// SpanHandler is called when spans are received.
type SpanHandler func(ctx context.Context, spans []*tracepb.ResourceSpans) error

// MetricHandler is called when metrics are received.
type MetricHandler func(ctx context.Context, metrics []*metricpb.ResourceMetrics) error

// LogHandler is called when logs are received.
type LogHandler func(ctx context.Context, logs []*logspb.ResourceLogs) error

// Receiver accepts OTLP trace and metric data via gRPC and HTTP.
type Receiver struct {
	config                         *Config
	grpcServer                     *grpc.Server
	httpServer                     *http.Server
	handler                        SpanHandler
	metricHandler                  MetricHandler
	logHandler                     LogHandler
	mu                             sync.Mutex
	running                        bool
	decodeSlots                    chan struct{}
	grpcListenAddr, httpListenAddr string
	grpcConnections                *connectionLimit
	httpConnections                *connectionLimit
}

// NewReceiver creates a new OTLP receiver.
func NewReceiver(config *Config, handler SpanHandler, opts ...ReceiverOption) *Receiver {
	r := &Receiver{
		config:      config,
		handler:     handler,
		decodeSlots: make(chan struct{}, maxConcurrentIntake),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// ReceiverOption configures optional receiver behavior.
type ReceiverOption func(*Receiver)

// WithMetricHandler sets the handler for received metrics.
func WithMetricHandler(h MetricHandler) ReceiverOption {
	return func(r *Receiver) {
		r.metricHandler = h
	}
}

// WithLogHandler sets the handler for received logs.
func WithLogHandler(h LogHandler) ReceiverOption {
	return func(r *Receiver) {
		r.logHandler = h
	}
}

// Start starts the OTLP gRPC and HTTP receivers.
func (r *Receiver) Start(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.running {
		return fmt.Errorf("receiver already running")
	}

	// Start gRPC server
	grpcAddr := fmt.Sprintf("127.0.0.1:%d", r.config.GRPCPort)
	grpcLis, err := net.Listen("tcp", grpcAddr)
	if err != nil {
		return fmt.Errorf("failed to listen on gRPC port %d: %w", r.config.GRPCPort, err)
	}
	r.grpcListenAddr = grpcLis.Addr().String()
	r.grpcConnections = newConnectionLimit(grpcLis, maxGRPCConnections)

	r.grpcServer = grpc.NewServer(grpc.MaxRecvMsgSize(maxDecodedBytes), grpc.MaxConcurrentStreams(maxConcurrentIntake), grpc.MaxHeaderListSize(16<<10), grpc.ConnectionTimeout(grpcHandshakeTimeout), grpc.KeepaliveParams(keepalive.ServerParameters{MaxConnectionIdle: 30 * time.Second}), grpc.StatsHandler(grpcAdmissionStats{}), grpc.InTapHandle(func(ctx context.Context, info *tap.Info) (context.Context, error) {
		if info.FullMethodName != "/opentelemetry.proto.collector.trace.v1.TraceService/Export" && info.FullMethodName != "/opentelemetry.proto.collector.metrics.v1.MetricsService/Export" && info.FullMethodName != "/opentelemetry.proto.collector.logs.v1.LogsService/Export" {
			return ctx, status.Error(codes.Unimplemented, "unsupported OTLP method")
		}
		select {
		case r.decodeSlots <- struct{}{}:
			return newGRPCPermitContext(ctx, r.decodeSlots), nil
		default:
			return ctx, transientOverload("telemetry intake concurrency exhausted")
		}
	}))
	coltracepb.RegisterTraceServiceServer(r.grpcServer, &traceServiceServer{handler: r.handler})
	colmetricpb.RegisterMetricsServiceServer(r.grpcServer, &metricsServiceServer{handler: r.metricHandler})
	collogspb.RegisterLogsServiceServer(r.grpcServer, &logsServiceServer{handler: r.logHandler})

	go func() {
		if err := r.grpcServer.Serve(r.grpcConnections); err != nil && err != grpc.ErrServerStopped {
			_ = err // receiver may be stopping; ignore serve errors
		}
	}()

	// Start HTTP server
	httpAddr := fmt.Sprintf("127.0.0.1:%d", r.config.HTTPPort)
	httpLis, err := net.Listen("tcp", httpAddr)
	if err != nil {
		r.grpcServer.Stop()
		return fmt.Errorf("failed to listen on HTTP port %d: %w", r.config.HTTPPort, err)
	}
	r.httpListenAddr = httpLis.Addr().String()
	r.httpConnections = newConnectionLimit(httpLis, maxGRPCConnections)

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/traces", r.handleHTTPTraces)
	mux.HandleFunc("/v1/metrics", r.handleHTTPMetrics)
	mux.HandleFunc("/v1/logs", r.handleHTTPLogs)

	r.httpServer = &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			select {
			case r.decodeSlots <- struct{}{}:
				defer func() { <-r.decodeSlots }()
			default:
				w.Header().Set("Retry-After", "1")
				http.Error(w, "Telemetry intake concurrency exhausted", http.StatusTooManyRequests)
				return
			}
			mux.ServeHTTP(w, req)
		}),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       intakeDeadline,
		WriteTimeout:      intakeDeadline,
		MaxHeaderBytes:    16 << 10,
	}

	go func() {
		// Log error but don't fail - error is intentionally ignored
		// because this is a best-effort telemetry receiver.
		_ = r.httpServer.Serve(r.httpConnections)
	}()

	r.running = true
	return nil
}

// Stop stops the OTLP receivers.
func (r *Receiver) Stop(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.running {
		return nil
	}

	var errs []error

	// Stop gRPC server
	if r.grpcServer != nil {
		done := make(chan struct{})
		go func() {
			r.grpcServer.GracefulStop()
			close(done)
		}()
		select {
		case <-done:
		case <-ctx.Done():
			r.grpcServer.Stop()
			<-done
			errs = append(errs, fmt.Errorf("gRPC shutdown: %w", ctx.Err()))
		}
	}

	// Stop HTTP server
	if r.httpServer != nil {
		if err := r.httpServer.Shutdown(ctx); err != nil {
			_ = r.httpServer.Close()
			errs = append(errs, fmt.Errorf("HTTP shutdown error: %w", err))
		}
	}

	r.running = false

	if len(errs) > 0 {
		return errs[0]
	}
	return nil
}

// IsRunning returns true if the receiver is running.
func (r *Receiver) IsRunning() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running
}

// handleHTTPTraces handles OTLP HTTP trace requests.
func (r *Receiver) handleHTTPTraces(w http.ResponseWriter, req *http.Request) {
	var exportReq coltracepb.ExportTraceServiceRequest
	handleHTTPExport(w, req, &exportReq, &coltracepb.ExportTraceServiceResponse{}, "Failed to process spans", func(ctx context.Context) error {
		if r.handler == nil {
			return nil
		}
		return r.handler(ctx, exportReq.ResourceSpans)
	})
}

// traceServiceServer implements the OTLP gRPC trace service.
type traceServiceServer struct {
	coltracepb.UnimplementedTraceServiceServer
	handler SpanHandler
}

// Export implements the OTLP trace export RPC.
func (s *traceServiceServer) Export(ctx context.Context, req *coltracepb.ExportTraceServiceRequest) (*coltracepb.ExportTraceServiceResponse, error) {
	if s.handler != nil {
		if err := s.handler(ctx, req.ResourceSpans); err != nil {
			return nil, err
		}
	}
	return &coltracepb.ExportTraceServiceResponse{}, nil
}

// handleHTTPMetrics handles OTLP HTTP metric requests.
func (r *Receiver) handleHTTPMetrics(w http.ResponseWriter, req *http.Request) {
	var exportReq colmetricpb.ExportMetricsServiceRequest
	handleHTTPExport(w, req, &exportReq, &colmetricpb.ExportMetricsServiceResponse{}, "Failed to process metrics", func(ctx context.Context) error {
		if r.metricHandler == nil {
			return nil
		}
		return r.metricHandler(ctx, exportReq.ResourceMetrics)
	})
}

// metricsServiceServer implements the OTLP gRPC metrics service.
type metricsServiceServer struct {
	colmetricpb.UnimplementedMetricsServiceServer
	handler MetricHandler
}

// Export implements the OTLP metric export RPC.
func (s *metricsServiceServer) Export(ctx context.Context, req *colmetricpb.ExportMetricsServiceRequest) (*colmetricpb.ExportMetricsServiceResponse, error) {
	if s.handler != nil {
		if err := s.handler(ctx, req.ResourceMetrics); err != nil {
			return nil, err
		}
	}
	return &colmetricpb.ExportMetricsServiceResponse{}, nil
}

// handleHTTPLogs handles OTLP HTTP log requests.
func (r *Receiver) handleHTTPLogs(w http.ResponseWriter, req *http.Request) {
	var exportReq collogspb.ExportLogsServiceRequest
	handleHTTPExport(w, req, &exportReq, &collogspb.ExportLogsServiceResponse{}, "Failed to process logs", func(ctx context.Context) error {
		if r.logHandler == nil {
			return nil
		}
		return r.logHandler(ctx, exportReq.ResourceLogs)
	})
}

func handleHTTPExport(w http.ResponseWriter, req *http.Request, exportReq, exportResp proto.Message, processError string, process func(context.Context) error) {
	if req.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if ct := strings.TrimSpace(strings.Split(req.Header.Get("Content-Type"), ";")[0]); ct != "application/x-protobuf" {
		http.Error(w, "Unsupported OTLP content type", http.StatusUnsupportedMediaType)
		return
	}
	if encoding := strings.TrimSpace(req.Header.Get("Content-Encoding")); encoding != "" && encoding != "identity" {
		http.Error(w, "Unsupported OTLP content encoding", http.StatusUnsupportedMediaType)
		return
	}
	if req.ContentLength > maxHTTPWireBytes {
		http.Error(w, "OTLP request too large", http.StatusRequestEntityTooLarge)
		return
	}
	ctx, cancel := context.WithTimeout(req.Context(), intakeDeadline)
	defer cancel()
	body, err := io.ReadAll(io.LimitReader(req.Body, maxHTTPWireBytes+1))
	if err != nil {
		http.Error(w, "Failed to read body", http.StatusBadRequest)
		return
	}
	if len(body) > maxHTTPWireBytes {
		http.Error(w, "OTLP request too large", http.StatusRequestEntityTooLarge)
		return
	}
	if err := proto.Unmarshal(body, exportReq); err != nil {
		http.Error(w, "Failed to parse OTLP request", http.StatusBadRequest)
		return
	}
	if err := process(ctx); err != nil {
		if status.Code(err) == codes.InvalidArgument {
			http.Error(w, policyAdmissionReason, http.StatusBadRequest)
			return
		}
		if status.Code(err) == codes.ResourceExhausted {
			code := http.StatusRequestEntityTooLarge
			if st, ok := status.FromError(err); ok {
				for _, detail := range st.Details() {
					if _, ok := detail.(*errdetails.RetryInfo); ok {
						w.Header().Set("Retry-After", "1")
						code = http.StatusTooManyRequests
						break
					}
				}
			}
			http.Error(w, "Telemetry intake limit exceeded", code)
			return
		}
		if status.Code(err) == codes.Unavailable {
			http.Error(w, "Telemetry destination unavailable", http.StatusServiceUnavailable)
			return
		}
		http.Error(w, processError, http.StatusInternalServerError)
		return
	}

	respBytes, _ := proto.Marshal(exportResp)
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(respBytes)
}

// logsServiceServer implements the OTLP gRPC logs service.
type logsServiceServer struct {
	collogspb.UnimplementedLogsServiceServer
	handler LogHandler
}

// Export implements the OTLP log export RPC.
func (s *logsServiceServer) Export(ctx context.Context, req *collogspb.ExportLogsServiceRequest) (*collogspb.ExportLogsServiceResponse, error) {
	if s.handler != nil {
		if err := s.handler(ctx, req.ResourceLogs); err != nil {
			return nil, err
		}
	}
	return &collogspb.ExportLogsServiceResponse{}, nil
}
