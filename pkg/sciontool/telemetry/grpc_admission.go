package telemetry

import (
	"context"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/tap"
	"google.golang.org/protobuf/proto"
)

const (
	traceExportMethod  = "/opentelemetry.proto.collector.trace.v1.TraceService/Export"
	metricExportMethod = "/opentelemetry.proto.collector.metrics.v1.MetricsService/Export"
	logExportMethod    = "/opentelemetry.proto.collector.logs.v1.LogsService/Export"
)

type grpcDeadlineCancelKey struct{}

// The tap installs the deadline before grpc-go captures the stream reader's
// context. It owns no admission permit: a canceled stream may still dispatch
// a worker after this callback returns.
func grpcDeadlineTap(ctx context.Context, info *tap.Info) (context.Context, error) {
	switch info.FullMethodName {
	case traceExportMethod, metricExportMethod, logExportMethod:
	default:
		return ctx, status.Error(codes.Unimplemented, "unsupported OTLP method")
	}
	bounded, cancel := context.WithTimeout(ctx, intakeDeadline)
	return context.WithValue(bounded, grpcDeadlineCancelKey{}, cancel), nil
}

// End releases only the deadline timer. The service handler itself owns and
// releases the processing permit, so cancellation never frees live work.
type grpcDeadlineStats struct{}

func (grpcDeadlineStats) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context {
	return ctx
}
func (grpcDeadlineStats) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context {
	return ctx
}
func (grpcDeadlineStats) HandleConn(context.Context, stats.ConnStats) {}
func (grpcDeadlineStats) HandleRPC(ctx context.Context, event stats.RPCStats) {
	if _, ok := event.(*stats.End); ok {
		if cancel, ok := ctx.Value(grpcDeadlineCancelKey{}).(context.CancelFunc); ok {
			cancel()
		}
	}
}

// streamExport acquires the shared capacity before RecvMsg enters framed
// read, decompression, and protobuf decode. The handler stack retains it
// through typed Export and the single SendMsg, even if context is canceled.
func streamExport[Req proto.Message, Resp proto.Message](ss grpc.ServerStream, slots chan struct{}, request Req, export func(context.Context, Req) (Resp, error)) error {
	if err := ss.Context().Err(); err != nil {
		return status.FromContextError(err).Err()
	}
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	default:
		return transientOverload("telemetry intake concurrency exhausted")
	}
	if err := ss.Context().Err(); err != nil {
		return status.FromContextError(err).Err()
	}
	if err := ss.RecvMsg(request); err != nil {
		return err
	}
	response, err := export(ss.Context(), request)
	if err != nil {
		return err
	}
	return ss.SendMsg(response)
}

// grpc-go requires a streaming flag to dispatch a public StreamDesc before
// receiving the message. These fresh descriptors preserve the unary OTLP wire
// contract and generated client compatibility, while server introspection and
// stats intentionally classify Export as server-streaming.
func registerBoundedOTLPServices(server *grpc.Server, slots chan struct{}, traces coltracepb.TraceServiceServer, metrics colmetricpb.MetricsServiceServer, logs collogspb.LogsServiceServer) {
	server.RegisterService(&grpc.ServiceDesc{
		ServiceName: "opentelemetry.proto.collector.trace.v1.TraceService",
		HandlerType: (*coltracepb.TraceServiceServer)(nil),
		Streams: []grpc.StreamDesc{{StreamName: "Export", ServerStreams: true, Handler: func(srv any, ss grpc.ServerStream) error {
			return streamExport(ss, slots, new(coltracepb.ExportTraceServiceRequest), srv.(coltracepb.TraceServiceServer).Export)
		}}},
		Metadata: "opentelemetry/proto/collector/trace/v1/trace_service.proto",
	}, traces)
	server.RegisterService(&grpc.ServiceDesc{
		ServiceName: "opentelemetry.proto.collector.metrics.v1.MetricsService",
		HandlerType: (*colmetricpb.MetricsServiceServer)(nil),
		Streams: []grpc.StreamDesc{{StreamName: "Export", ServerStreams: true, Handler: func(srv any, ss grpc.ServerStream) error {
			return streamExport(ss, slots, new(colmetricpb.ExportMetricsServiceRequest), srv.(colmetricpb.MetricsServiceServer).Export)
		}}},
		Metadata: "opentelemetry/proto/collector/metrics/v1/metrics_service.proto",
	}, metrics)
	server.RegisterService(&grpc.ServiceDesc{
		ServiceName: "opentelemetry.proto.collector.logs.v1.LogsService",
		HandlerType: (*collogspb.LogsServiceServer)(nil),
		Streams: []grpc.StreamDesc{{StreamName: "Export", ServerStreams: true, Handler: func(srv any, ss grpc.ServerStream) error {
			return streamExport(ss, slots, new(collogspb.ExportLogsServiceRequest), srv.(collogspb.LogsServiceServer).Export)
		}}},
		Metadata: "opentelemetry/proto/collector/logs/v1/logs_service.proto",
	}, logs)
}
